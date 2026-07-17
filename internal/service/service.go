package service

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/olebedev/when"
	"github.com/olebedev/when/rules/common"
	"github.com/olebedev/when/rules/en"
	"github.com/zawnk/later/internal/reminder"
	"github.com/zawnk/later/internal/store"
)

const maxReminderTextLength = 4096

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
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("empty reminder text")
	}

	if utf8.RuneCountInString(text) > maxReminderTextLength {
		return nil, fmt.Errorf("reminder text too long (max %d chars)", maxReminderTextLength)
	}

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

	// TODO: does the Round cause any issues?
	rem := &reminder.Reminder{
		ID:             reminder.GenerateID(),
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

	due, err := applyDuration(duration, time.Now())
	if err != nil {
		return nil, fmt.Errorf("invalid duration: %w", err)
	}

	rem := &reminder.Reminder{
		ID:             reminder.GenerateID(),
		Text:           found.Text,
		DueAt:          due.Round(time.Minute),
		CreatedAt:      time.Now(),
		OutboundTopics: found.OutboundTopics,
	}

	if err := s.store.SaveReminder(*rem); err != nil {
		return nil, fmt.Errorf("failed to store postponed reminder: %w", err)
	}

	return rem, nil
}

func applyDuration(duration string, from time.Time) (time.Time, error) {
	matches := durationRegex.FindAllStringSubmatch(duration, -1)
	if len(matches) == 0 {
		return time.Time{}, fmt.Errorf("invalid duration: %s", duration)
	}

	var years, months, days int
	var clock time.Duration

	for _, match := range matches {
		n, _ := strconv.Atoi(match[1])
		switch match[2] {
		case "y":
			years += n
		case "mo":
			months += n
		case "w":
			days += n * 7
		case "d":
			days += n
		case "h":
			clock += time.Duration(n) * time.Hour
		case "m":
			clock += time.Duration(n) * time.Minute
		case "s":
			clock += time.Duration(n) * time.Second

		default:
			return time.Time{}, fmt.Errorf("unknown unit: %s", match[2])
		}
	}
	return from.AddDate(years, months, days).Add(clock), nil
}
