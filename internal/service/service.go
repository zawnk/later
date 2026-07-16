package service

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/olebedev/when"
	"github.com/olebedev/when/rules/common"
	"github.com/olebedev/when/rules/en"
	"github.com/zawnk/later/internal/reminder"
	"github.com/zawnk/later/internal/store"
)

var durationRegex = regexp.MustCompile(`(\d+)(y|mo|w|d|h|m|s)`)

var durationWords = map[string]string{
	"y":  "years",
	"mo": "months",
	"w":  "weeks",
	"d":  "days",
	"h":  "hours",
	"m":  "minutes",
	"s":  "seconds",
}

type Service struct {
	store  *store.Store
	parser *when.Parser
}

func New(s *store.Store) *Service {
	w := when.New(nil)
	w.Add(en.All...)
	w.Add(common.All...)

	return &Service{
		store:  s,
		parser: w,
	}
}

func (s *Service) CreateReminder(text string, outboundTopics []string) (*reminder.Reminder, error) {
	text = preprocessDuration(text)

	result, err := s.parser.Parse(text, time.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to parse time: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("no time information found in: %q", text)
	}

	task := strings.TrimSpace(strings.Replace(text, result.Text, "", 1))
	task = strings.TrimPrefix(task, "at")
	task = strings.TrimSpace(task)
	if task == "" {
		return nil, fmt.Errorf("no task text found")
	}

	id, err := reminder.GenerateID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate id: %w", err)
	}

	// TODO: does the Round cause any issues?
	rem := &reminder.Reminder{
		ID:             id,
		Text:           task,
		DueAt:          result.Time.Local().Round(time.Minute),
		CreatedAt:      time.Now(),
		OutboundTopics: outboundTopics,
	}

	if err := s.store.SaveReminder(*rem); err != nil {
		return nil, fmt.Errorf("failed to store reminder: %w", err)
	}

	return rem, nil
}

// preprocessDuration converts compact duration strings to forms when can parse
// e.g. "3d" -> "3 days", "2h30m" -> "2 hours 30 minutes"
func preprocessDuration(s string) string {
	return durationRegex.ReplaceAllStringFunc(s, func(match string) string {
		parts := durationRegex.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		word, ok := durationWords[parts[2]]
		if !ok {
			return match
		}
		return parts[1] + " " + word
	})
}

func (s *Service) ListPending() []reminder.Reminder {
	return s.store.ListPendingReminders()
}

func (s *Service) ListArchive() ([]reminder.ArchivedReminder, error) {
	return s.store.ListArchive()
}

func (s *Service) Cancel(id string) (bool, error) {
	return s.store.CancelReminder(id)
}

func (s *Service) Next() *reminder.Reminder {
	pending := s.store.ListPendingReminders()
	if len(pending) == 0 {
		return nil
	}
	next := pending[0]
	for _, r := range pending[1:] {
		if r.DueAt.Before(next.DueAt) {
			next = r
		}
	}
	return &next
}

func (s *Service) Last() (*reminder.ArchivedReminder, error) {
	archive, err := s.store.ListArchive()
	if err != nil {
		return nil, err
	}
	if len(archive) == 0 {
		return nil, nil
	}
	last := archive[len(archive)-1]
	return &last, nil
}

func (s *Service) Postpone(id string, duration string) (*reminder.Reminder, error) {
	// verify it's in archive, not pending
	pending := s.store.ListPendingReminders()
	for _, r := range pending {
		if r.ID == id {
			return nil, fmt.Errorf("reminder %s is still pending, cannot postpone", id)
		}
	}

	// find in archive
	archive, err := s.store.ListArchive()
	if err != nil {
		return nil, err
	}
	var found *reminder.ArchivedReminder
	for _, r := range archive {
		if r.ID == id {
			found = &r
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("reminder %s not found in archive", id)
	}

	d, err := parseDuration(duration)
	if err != nil {
		return nil, fmt.Errorf("invalid duration: %w", err)
	}

	newID, err := reminder.GenerateID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate id: %w", err)
	}

	rem := &reminder.Reminder{
		ID:             newID,
		Text:           found.Text,
		DueAt:          time.Now().Add(d).Round(time.Minute),
		CreatedAt:      time.Now(),
		OutboundTopics: found.OutboundTopics,
	}

	if err := s.store.SaveReminder(*rem); err != nil {
		return nil, fmt.Errorf("failed to store postponed reminder: %w", err)
	}

	return rem, nil
}

func parseDuration(s string) (time.Duration, error) {
	matches := durationRegex.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return 0, fmt.Errorf("invalid duration: %s", s)
	}

	var total time.Duration
	for _, match := range matches {
		n, _ := strconv.Atoi(match[1])
		switch match[2] {
		case "s":
			total += time.Duration(n) * time.Second
		case "m":
			total += time.Duration(n) * time.Minute
		case "h":
			total += time.Duration(n) * time.Hour
		case "d":
			total += time.Duration(n) * 24 * time.Hour
		case "w":
			total += time.Duration(n) * 7 * 24 * time.Hour
		case "mo":
			total += time.Duration(n) * 30 * 24 * time.Hour
		case "y":
			total += time.Duration(n) * 365 * 24 * time.Hour
		default:
			return 0, fmt.Errorf("unknown unit: %s", match[2])
		}
	}
	return total, nil
}
