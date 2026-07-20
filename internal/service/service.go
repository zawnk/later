package service

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/olebedev/when"
	"github.com/olebedev/when/rules/common"
	"github.com/olebedev/when/rules/en"
	"github.com/zawnk/later/internal/reminder"
)

const maxReminderTextLength = 4096

var (
	ErrInvalidInput = errors.New("invalid input")
	ErrNotFound     = errors.New("not found")
	ErrStillPending = errors.New("still a pending reminder")
)

type Store interface {
	SaveReminder(r reminder.Reminder) error
	ListPendingReminders() []reminder.Reminder
	ListArchive() ([]reminder.ArchivedReminder, error)
	CancelReminder(id string) (bool, error)
}

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
	store  Store
	parser *when.Parser
	now    func() time.Time
}

type CreateInput struct {
	Text           string
	OutboundTopics []string
	Tags           []string
	Priority       string
	Click          string
}

var validPriorities = map[string]struct{}{
	"min": {}, "low": {}, "default": {}, "high": {}, "urgent": {}, "max": {},
	"1": {}, "2": {}, "3": {}, "4": {}, "5": {},
}

func validateNotificationOptions(in CreateInput) error {
	if in.Priority != "" {
		if _, ok := validPriorities[in.Priority]; !ok {
			return fmt.Errorf("%w: invalid priority %q (want min/low/default/high/urgent or 1-5)", ErrInvalidInput, in.Priority)
		}
	}
	if in.Click != "" {
		u, err := url.Parse(in.Click)
		if err != nil || u.Scheme == "" {
			return fmt.Errorf("%w: click must be an absolute URL, got %q", ErrInvalidInput, in.Click)
		}
	}
	return nil
}

func New(s Store) *Service {
	w := when.New(nil)
	w.Add(en.All...)
	w.Add(common.All...)

	return &Service{
		store:  s,
		parser: w,
		now:    time.Now,
	}
}

func (s *Service) CreateReminder(in CreateInput) (*reminder.Reminder, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil, fmt.Errorf("%w: empty reminder text", ErrInvalidInput)
	}

	if err := validateNotificationOptions(in); err != nil {
		return nil, err
	}

	if utf8.RuneCountInString(text) > maxReminderTextLength {
		return nil, fmt.Errorf("%w: reminder text too long (max %d chars)", ErrInvalidInput, maxReminderTextLength)
	}

	text = preprocessDuration(text)

	result, err := s.parser.Parse(text, s.now())
	if err != nil {
		return nil, fmt.Errorf("%w: failed to parse time: %w", ErrInvalidInput, err)
	}
	if result == nil {
		return nil, fmt.Errorf("%w: no time information found in: %q", ErrInvalidInput, text)
	}

	task := strings.Join(strings.Fields(strings.Replace(text, result.Text, "", 1)), " ")
	task = strings.TrimSuffix(task, " at")
	task = strings.TrimPrefix(task, "at ")
	if task == "" {
		return nil, fmt.Errorf("%w: no task text found", ErrInvalidInput)
	}

	dueAt := result.Time.Local().Round(time.Minute)
	if dueAt.Before(s.now().Round(time.Minute)) {
		return nil, fmt.Errorf("%w: due time %s is in the past", ErrInvalidInput, dueAt.Format(time.RFC3339))
	}

	rem := &reminder.Reminder{
		ID:             reminder.GenerateID(),
		Text:           task,
		DueAt:          dueAt,
		CreatedAt:      s.now(),
		OutboundTopics: in.OutboundTopics,
		Tags:           in.Tags,
		Priority:       in.Priority,
		Click:          in.Click,
	}

	if err := s.store.SaveReminder(*rem); err != nil {
		return nil, fmt.Errorf("failed to store reminder: %w", err)
	}

	return rem, nil
}

func preprocessDuration(s string) string {
	res := durationRegex.ReplaceAllStringFunc(s, func(match string) string {
		parts := durationRegex.FindStringSubmatch(match)

		word := durationWords[parts[2]]

		return fmt.Sprintf(" %s %s ", parts[1], word)
	})

	return strings.ReplaceAll(strings.TrimSpace(res), "  ", " ")
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
	pending := s.store.ListPendingReminders()
	for _, r := range pending {
		if r.ID == id {
			return nil, fmt.Errorf("reminder %s is %w, cannot postpone", id, ErrStillPending)
		}
	}

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
		return nil, fmt.Errorf("reminder %s %w in archive", id, ErrNotFound)
	}

	due, err := applyDuration(duration, s.now())
	if err != nil {
		return nil, fmt.Errorf("%w: invalid duration: %w", ErrInvalidInput, err)
	}

	rem := &reminder.Reminder{
		ID:             reminder.GenerateID(),
		Text:           found.Text,
		DueAt:          due.Round(time.Minute),
		CreatedAt:      s.now(),
		OutboundTopics: found.OutboundTopics,
		Tags:           found.Tags,
		Priority:       found.Priority,
		Click:          found.Click,
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
