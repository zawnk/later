// Package service implements reminder creation, postponing, and retrieval.
//
// Time parsing (parseDueTime) has three layers, tried in order:
//
//  1. combinedDurationRegex - 2+ chained compact units ("1w2d", "in
//     2h30m"). Resolved locally via AddDate+Add so DST is preserved. Only
//     trusted when isUnambiguousDurationRun says so - either explicitly
//     flagged with "in "/"within ", or bare and anchoring the true start
//     or end of the text.
//  2. singleUnitRegex - a single "in|within N<unit>" ("in 3d"). Rewritten
//     to English ("in 3 days") by preprocessDuration and handed to
//     when.Parse, which has no native understanding of the abbreviated
//     form and, unlike (1), never recognizes a bare single unit no matter
//     how it's rewritten - so a single unit is never trusted unflagged.
//  3. Everything else - "tomorrow", "next monday", calendar dates, times
//     of day - goes straight to when.Parse.
//
// Postpone uses the same pipeline via resolvePostponeTime, which
// additionally requires the match to consume the whole input (no task
// text left over).
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

var singleUnitRegex = regexp.MustCompile(`(^|\s)(in|within) (\d+)(y|mo|w|d|h|m|s)\b`)
var combinedDurationRegex = regexp.MustCompile(`(^|\s)((?:in |within )?(?:\d+(?:y|mo|w|d|h|m|s)){2,})\b`)
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

func sumDurationMatches(matches [][]string) (years, months, days int, clock time.Duration) {
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
		}
	}
	return
}

type Service struct {
	store      Store
	parser     *when.Parser
	now        func() time.Time
	generateID func() string
}

type CreateInput struct {
	Text           string
	OutboundTopics []string
	Tags           []string
	Priority       string
	Click          string
}

func validateNotificationOptions(in CreateInput) error {
	if in.Priority != "" {
		if !reminder.IsValidPriority(in.Priority) {
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
		store:      s,
		parser:     w,
		now:        time.Now,
		generateID: reminder.GenerateID,
	}
}

// ParseReminderText runs text through the exact same task/due-time extraction
// CreateReminder uses, without generating an ID or saving anything -
// backs the /test/parse "show what would be scheduled" preview (CLI, API,
// and the ntfy inbound "/test <text>" trigger). Returns the same
// ErrInvalidInput-wrapped errors CreateReminder would, since it's the same
// code, so a preview failure is always the real reason a real create would
// also fail.
func (s *Service) ParseReminderText(text string) (task string, due time.Time, err error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", time.Time{}, fmt.Errorf("%w: empty reminder text", ErrInvalidInput)
	}

	if utf8.RuneCountInString(text) > maxReminderTextLength {
		return "", time.Time{}, fmt.Errorf("%w: reminder text too long (max %d chars)", ErrInvalidInput, maxReminderTextLength)
	}

	textForTask, matchedText, parsedTime, err := s.parseDueTime(text)
	if err != nil {
		return "", time.Time{}, err
	}

	task = collapseWhitespace(strings.Replace(textForTask, matchedText, "", 1))
	task = strings.TrimSuffix(task, " at")
	task = strings.TrimPrefix(task, "at ")
	if task == "" {
		return "", time.Time{}, fmt.Errorf("%w: no task text found", ErrInvalidInput)
	}

	dueAt := parsedTime.Local().Round(time.Minute)
	if dueAt.Before(s.now().Round(time.Minute)) {
		return "", time.Time{}, fmt.Errorf("%w: due time %s is in the past", ErrInvalidInput, dueAt.Format(time.RFC3339))
	}

	return task, dueAt, nil
}

func (s *Service) CreateReminder(in CreateInput) (*reminder.Reminder, error) {
	task, dueAt, err := s.ParseReminderText(in.Text)
	if err != nil {
		return nil, err
	}

	if err := validateNotificationOptions(in); err != nil {
		return nil, err
	}

	id, err := s.generateUniqueID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate reminder id: %w", err)
	}

	rem := &reminder.Reminder{
		ID:             id,
		Text:           task,
		DueAt:          dueAt,
		CreatedAt:      s.now(),
		OutboundTopics: in.OutboundTopics,
		Tags:           reminder.DedupeStrings(in.Tags),
		Priority:       in.Priority,
		Click:          in.Click,
	}

	if err := s.store.SaveReminder(*rem); err != nil {
		return nil, fmt.Errorf("failed to store reminder: %w", err)
	}

	return rem, nil
}

// parseDueTime finds the due time in text and reports which part of it
// was consumed, trying three things in order:
//
//  1. A combined (2+ chained) compact-unit run, e.g. "1w2d" — resolved
//     directly via our own AddDate/Add arithmetic (DST-safe), never
//     touching when.Parse. Only trusted when isUnambiguousDurationRun
//     says so — otherwise it falls through to (2)/(3), since a
//     duration-shaped fragment can just as easily be part of the task
//     itself (e.g. "1y2mo" in "buy 1y2mo of insurance").
//  2. A single compact unit prefixed with "in"/"within", e.g. "in 3d" —
//     expanded to words ("in 3 days") and handed to when.Parse, since
//     when has no native understanding of the abbreviated form. Never
//     bare: when.Parse itself requires "in"/"within" for a single unit
//     no matter what we do on our end, so there's nothing to gain (and a
//     real risk of expanding the wrong occurrence) by expanding one that
//     isn't marked.
//  3. Whatever when.Parse understands on its own — casual dates
//     ("tomorrow"), weekdays ("next monday"), calendar dates, specific
//     times, and so on.
func (s *Service) parseDueTime(text string) (textForTask, matchedText string, parsedTime time.Time, err error) {
	text = strings.TrimSpace(text)

	if loc := combinedDurationRegex.FindStringSubmatchIndex(text); loc != nil {
		run := text[loc[4]:loc[5]]
		if isUnambiguousDurationRun(run, loc[0] == 0, loc[1] == len(text)) {
			years, months, days, clock := sumDurationMatches(durationRegex.FindAllStringSubmatch(run, -1))
			return text, run, s.now().AddDate(years, months, days).Add(clock), nil
		}
	}

	preprocessed := preprocessDuration(text)
	result, perr := s.parser.Parse(preprocessed, s.now())
	if perr != nil {
		return "", "", time.Time{}, fmt.Errorf("%w: failed to parse time: %w", ErrInvalidInput, perr)
	}
	if result == nil {
		return "", "", time.Time{}, fmt.Errorf("%w: no time information found in: %q", ErrInvalidInput, preprocessed)
	}
	return preprocessed, result.Text, result.Time, nil
}

// isUnambiguousDurationRun reports whether a combined-unit-shaped run
// (e.g. "1w2d") should be trusted as the actual time reference, rather
// than a coincidental fragment of unrelated task text. Trusted either
// when it's explicitly flagged with "in "/"within " - unambiguous
// regardless of where it sits ("call the plumber in 1w2d please" is fine
// even mid-sentence) - or, for a bare unflagged run, only when it
// anchors the true start or end of the text, the two positions an
// unmarked duration is unambiguous in every example this app supports
// ("1w2d call plumber" / "call plumber 1w2d").
func isUnambiguousDurationRun(run string, atStart, atEnd bool) bool {
	flagged := strings.HasPrefix(run, "in ") || strings.HasPrefix(run, "within ")
	return flagged || atStart || atEnd
}

// preprocessDuration expands only "in "/"within "-prefixed compact units
// into words, since that's the only shape when.Parse ever recognizes as
// a deadline anyway - a coincidental duration-shaped substring elsewhere
// in the text (e.g. "2h" in "buy 2h of parking in 3d") is task content,
// not a second time reference, and this way it's never touched at all
// rather than needing to guess which occurrence was "the" real one.
func preprocessDuration(s string) string {
	res := singleUnitRegex.ReplaceAllStringFunc(s, func(match string) string {
		parts := singleUnitRegex.FindStringSubmatch(match)

		word := durationWords[parts[4]]

		return fmt.Sprintf("%s%s %s %s ", parts[1], parts[2], parts[3], word)
	})

	return collapseWhitespace(res)
}

// generateUniqueID retries reminder.GenerateID until it produces an ID
// that doesn't collide with any pending or archived reminder. Collisions
// are astronomically unlikely at this app's scale, but action-button
// tokens now reference an archived ID for up to 72h, and an
// archive-to-archive collision would silently resolve to the wrong
// reminder's fields rather than fail loud.
func (s *Service) generateUniqueID() (string, error) {
	archive, err := s.store.ListArchive()
	if err != nil {
		return "", err
	}

	existing := make(map[string]struct{}, len(archive))
	for _, r := range s.store.ListPendingReminders() {
		existing[r.ID] = struct{}{}
	}
	for _, r := range archive {
		existing[r.ID] = struct{}{}
	}

	for {
		id := s.generateID()
		if _, collision := existing[id]; !collision {
			return id, nil
		}
	}
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
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

func (s *Service) Get(id string) (*reminder.Reminder, *reminder.ArchivedReminder, error) {
	for _, r := range s.store.ListPendingReminders() {
		if r.ID == id {
			return &r, nil, nil
		}
	}

	archive, err := s.store.ListArchive()

	if err != nil {
		return nil, nil, err
	}
	for _, r := range archive {
		if r.ID == id {
			return nil, &r, nil
		}
	}

	return nil, nil, fmt.Errorf("reminder %s %w", id, ErrNotFound)
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

// Postpone accepts anything parseDueTime can resolve: a bare compact
// duration ("1d", "2h30m", combined units never needed "in" to begin
// with), or full natural language ("tomorrow morning", "next monday").
// A leading "in " is always prepended before parsing — verified
// empirically that when's parser simply ignores it when it doesn't apply
// (e.g. "in tomorrow morning" still resolves correctly) — so a lone bare
// unit like "1d" keeps working without requiring the caller to spell out
// "in 1d" themselves, avoiding an inconsistency with how "in" works
// everywhere else in this app.
func (s *Service) Postpone(id string, timeExpr string) (*reminder.Reminder, error) {
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

	due, err := s.resolvePostponeTime(timeExpr)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid duration: %w", ErrInvalidInput, err)
	}

	newID, err := s.generateUniqueID()
	if err != nil {
		return nil, err
	}

	rem := &reminder.Reminder{
		ID:             newID,
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

// resolvePostponeTime resolves timeExpr via parseDueTime (prefixed with
// "in " unless the caller already led with "in"/"within" themselves, so a
// lone bare unit like "1d" still works without spelling out "in 1d") and
// requires the match to consume the entire input. Unlike CreateReminder -
// where leftover text after the match is expected and wanted (it's the
// task) - here any leftover means timeExpr merely contains a
// duration-shaped fragment inside otherwise unrelated text (e.g. "3d
// rotate the tires" or "garbage 1y2mo garbage"), which must be rejected
// rather than silently accepted with the rest of the string discarded.
func (s *Service) resolvePostponeTime(timeExpr string) (time.Time, error) {
	prefixed := ensureInPrefix(strings.TrimSpace(timeExpr))

	textForTask, matchedText, due, err := s.parseDueTime(prefixed)
	if err != nil {
		return time.Time{}, err
	}

	leftover := strings.Fields(strings.Replace(textForTask, matchedText, "", 1))
	if len(leftover) > 0 && (leftover[0] == "in" || leftover[0] == "within") {
		leftover = leftover[1:]
	}
	if len(leftover) > 0 {
		return time.Time{}, fmt.Errorf("%q isn't a recognizable time expression on its own", timeExpr)
	}

	return due, nil
}

// ensureInPrefix leads s with "in " unless it already starts with "in "/
// "within " - so a bare unit like "1d" still reaches parseDueTime as "in
// 1d" (required for when.Parse to recognize a single unit at all), without
// doubling up on a caller-supplied "in tomorrow" into "in in tomorrow".
func ensureInPrefix(s string) string {
	if leadWord, _, _ := strings.Cut(s, " "); leadWord == "in" || leadWord == "within" {
		return s
	}
	return "in " + s
}
