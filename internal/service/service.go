// Package service implements reminder creation, postponing, and retrieval.
//
// Time parsing (parseDueTime) has four layers, tried in order:
//
//  1. combinedDurationRegex - 2+ chained compact units ("1w2d", "in
//     2h30m"). Only trusted when isUnambiguousDurationRun says so -
//     either explicitly flagged with "in "/"within ", or bare and
//     anchoring the true start or end of the text.
//  2. singleUnitRegex - a single "in|within N<unit>" ("in 3d"). Always
//     flagged - there's no bare-unit case here at all, the regex itself
//     requires "in"/"within".
//     Both (1) and (2) resolve the exact same way: our own AddDate+Add
//     arithmetic (DST-safe), never handed to when.Parse. Single units went
//     through when.Parse's own
//     "Deadline" rule instead - until a confirmed, unfixed bug was found
//     there (rules/en/deadline.go@v1.1.0: its "in N months" case computes
//     `(ref.Month()+num) % 12` with no year carry, so "in 1 month" from
//     December silently lands in January of the *current* year instead of
//     next year). Since (2) already requires the same "in"/"within" flag
//     (1) already trusts regardless of position, routing it through our own
//     arithmetic instead needed no new ambiguity rule - it closes the bug
//     and simplifies the pipeline at the same time (preprocessDuration, the
//     function that used to rewrite (2) into English for when.Parse, no
//     longer has a reason to exist).
//  3. slashDateRegex - a bare "DD/MM" with no year, naming a month later
//     in the current year than now (e.g. "25/12" typed in July).
//     Resolved locally, working around a second, separate confirmed bug
//     in olebedev/when's SlashDMY rule (rules/common/slash_dmy.go@v1.1.0)
//     that silently resolves this exact shape to "now" instead of the
//     intended date - see resolveFutureMonthSlashDate's doc comment.
//     Every other slash-date shape (an explicit year; a month already
//     passed this year; the same month as now) is already correct in
//     that rule and is left to when.Parse untouched.
//  4. Everything else - "tomorrow", "next monday", calendar dates, times
//     of day - goes straight to when.Parse. If the match came from
//     ExactMonthDate (a spelled-out month name, e.g. "march 3rd") and
//     landed in the past only because that rule never considers a year at
//     all, rollFutureMonthNameDate rolls it to next year - the one thing
//     SlashDMY already gets right on its own that ExactMonthDate doesn't.
//     See rollFutureMonthNameDate's doc comment for the narrow conditions
//     this applies under.
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
var slashDateRegex = regexp.MustCompile(`(?:^|\W)(0?[1-9]|[12][0-9]|3[01])[/\\](0?[1-9]|1[0-2])(?:[/\\]((?:1|2)[0-9]{3}))?(?:\W|$)`)
var monthNamePattern = regexp.MustCompile(`(?i)\b` + en.MONTH_OFFSET_PATTERN)
var digitRunPattern = regexp.MustCompile(`\d+`)
var trailingYearLikePattern = regexp.MustCompile(`^[\s,]*\d{1,4}`)

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
// was consumed, trying these in order:
//
//  1. A combined (2+ chained) compact-unit run, e.g. "1w2d". Only trusted
//     when isUnambiguousDurationRun says so — otherwise it falls through,
//     since a duration-shaped fragment can just as easily be part of the
//     task itself (e.g. "1y2mo" in "buy 1y2mo of insurance").
//  2. A single compact unit prefixed with "in"/"within", e.g. "in 3d" —
//     always flagged, there's no bare case for this one at all.
//
// Both (1) and (2) resolve via our own AddDate/Add arithmetic (DST-safe),
// never touching when.Parse.
//
//  3. A bare "DD/MM" naming a month later in the current year than now —
//     resolveFutureMonthSlashDate, working around a confirmed olebedev/when
//     bug (see its own doc comment).
//  4. Whatever when.Parse understands on its own — casual dates
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

	if loc := singleUnitRegex.FindStringSubmatchIndex(text); loc != nil {
		run := text[loc[4]:loc[9]]
		years, months, days, clock := sumDurationMatches(durationRegex.FindAllStringSubmatch(run, -1))
		return text, run, s.now().AddDate(years, months, days).Add(clock), nil
	}

	if run, due, ok := resolveFutureMonthSlashDate(text, s.now()); ok {
		return text, run, due, nil
	}

	result, perr := s.parser.Parse(text, s.now())
	if perr != nil {
		return "", "", time.Time{}, fmt.Errorf("%w: failed to parse time: %w", ErrInvalidInput, perr)
	}
	if result == nil {
		return "", "", time.Time{}, fmt.Errorf("%w: no time information found in: %q", ErrInvalidInput, text)
	}
	if due, ok := rollFutureMonthNameDate(text, result, s.now()); ok {
		return text, result.Text, due, nil
	}
	return text, result.Text, result.Time, nil
}

// rollFutureMonthNameDate works around a missing feature (not a bug, just
// an asymmetry) in olebedev/when's ExactMonthDate rule
// (rules/en/exact_month_date.go@v1.1.0, verified against the dependency
// source): unlike SlashDMY (which already rolls a passed month to next
// year on its own, see resolveFutureMonthSlashDate), ExactMonthDate never
// parses or considers a year at all - a spelled-out month name ("march
// 3rd", "3 march", "twentieth of december") always resolves in ref's
// current year, even when that date has already passed this year, leaving
// it to later's own past-due guard to reject outright rather than roll
// forward the way a future-only reminder app should.
//
// Deliberately narrow, mirroring resolveFutureMonthSlashDate's caution:
//   - only acts when when.Parse's own matched text contains a recognized
//     month name at all - never touches a weekday/relative match that
//     happens to also land in the past ("yesterday", "last monday"),
//     which should stay rejected, not get silently reinterpreted a year
//     later
//   - backs off if the matched text has more than one bare digit run
//     (e.g. "17 april 85") - that's the one confirmed shape where
//     ExactMonthDate misreads a trailing number as a second day value
//     instead of a year, silently overwriting the real day (see
//     docs/maintainers-manual.md) - the already-resolved date can't be
//     trusted enough to roll forward on top of
//   - backs off if anything year-shaped immediately follows the match in
//     the original text (e.g. "february 14, 2004") - the user typed an
//     explicit (if unusable-by-when) year, so silently substituting a
//     different one would be worse than leaving it to fail as past-due
func rollFutureMonthNameDate(text string, result *when.Result, ref time.Time) (time.Time, bool) {
	if !result.Time.Before(ref) {
		return time.Time{}, false
	}
	if !monthNamePattern.MatchString(result.Text) {
		return time.Time{}, false
	}
	if len(digitRunPattern.FindAllString(result.Text, -1)) > 1 {
		return time.Time{}, false
	}
	if end := result.Index + len(result.Text); end <= len(text) && trailingYearLikePattern.MatchString(text[end:]) {
		return time.Time{}, false
	}
	return result.Time.AddDate(1, 0, 0), true
}

// resolveFutureMonthSlashDate works around a confirmed, unfixed bug in
// olebedev/when's SlashDMY rule (rules/common/slash_dmy.go@v1.1.0,
// verified against the actual dependency source): when a bare "DD/MM"
// (no year) names a month later in the current year than "now", the
// rule's Applier function returns true (claims a match) without ever
// setting the match's day/month/year, so when.Parse silently resolves
// the whole input to "now" instead of erroring or landing on the
// intended date. Reported upstream as
// https://github.com/olebedev/when/pull/34 (filed 2023) - the maintainer
// rejected the proposed fix and there's no timeline for one.
//
// Every other slash-date shape that same rule handles is already
// correct and is deliberately left to when.Parse untouched: an explicit
// year; a month that's already passed this year (rolls to next year); a
// day within the same month as now (before/after/on today). This only
// intercepts the one specific broken case.
func resolveFutureMonthSlashDate(text string, ref time.Time) (matchedText string, due time.Time, ok bool) {
	loc := slashDateRegex.FindStringSubmatchIndex(text)
	if loc == nil || loc[6] != -1 { // loc[6] != -1 means a year was captured - when.Parse already handles that correctly
		return "", time.Time{}, false
	}

	day, _ := strconv.Atoi(text[loc[2]:loc[3]])
	month, _ := strconv.Atoi(text[loc[4]:loc[5]])
	if month <= int(ref.Month()) {
		return "", time.Time{}, false // not the buggy case - when.Parse's own same-month/rollover logic already works
	}

	due = time.Date(ref.Year(), time.Month(month), day, ref.Hour(), ref.Minute(), ref.Second(), 0, ref.Location())
	if int(due.Month()) != month {
		return "", time.Time{}, false // invalid day for that month (e.g. "31/4") - Go's time.Date silently normalized it
	}

	return text[loc[2]:loc[5]], due, true
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
