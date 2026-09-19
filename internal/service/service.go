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
	"slices"
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
	ErrPastDue      = errors.New("due time is in the past")
)

type Store interface {
	SaveReminder(r reminder.Reminder) error
	ListPendingReminders() []reminder.Reminder
	ListArchive() ([]reminder.ArchivedReminder, error)
	CancelReminder(id string) (bool, error)
	LoadStubs() (map[string]reminder.Stub, error)
	SetStub(name string, stub reminder.Stub) (created bool, err error)
	DeleteStub(name string) (bool, error)
}

var stubInvocationRegex = regexp.MustCompile(`^:([A-Za-z][A-Za-z0-9_-]*)(?:\s|$)`)
var stubNameRegex = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

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
// CreateReminder uses, without generating an ID or saving anything - the
// parsing half of both creation and the "show what would be scheduled"
// preview (which reaches it through PreviewReminderText, one stub
// resolution earlier). Returns the same
// ErrInvalidInput-wrapped errors CreateReminder would, since it's the same
// code, so a preview failure is always a real reason a real create would
// also fail - though not necessarily the first one reported: CreateReminder
// validates notification options ahead of parsing, so input wrong in both
// ways at once previews as a parse failure and creates as an option
// failure.
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
		return "", time.Time{}, fmt.Errorf("%w: %w (%s)", ErrInvalidInput, ErrPastDue, dueAt.Format(time.RFC3339))
	}

	return task, dueAt, nil
}

// PreviewReminderText backs the /test/parse "show what would be
// scheduled" preview on all three surfaces (CLI, API, and the ntfy
// inbound "/test <text>" trigger): it runs CreateReminder's stub
// resolution and then its text parsing, so previewing ":hockey" reports
// the task text and due time that stub would actually produce rather
// than echoing the unexpanded invocation, and an unknown stub-shaped
// name fails with the same unknown-stub error a create would report.
//
// It runs CreateReminder's first three steps in that pipeline's own
// order, option validation included: preview is handed text and nothing
// else, so the only options it can ever validate are the ones the
// resolved stub carries - and a hand-edited stub is exactly what
// someone reaches for a preview to check. A stub carrying a bad
// priority therefore fails the same way here as it would at create
// time, rather than previewing clean and failing on the real thing.
func (s *Service) PreviewReminderText(text string) (task string, due time.Time, err error) {
	in, err := s.resolveStub(CreateInput{Text: text})
	if err != nil {
		return "", time.Time{}, err
	}
	if err := validateNotificationOptions(in); err != nil {
		return "", time.Time{}, err
	}
	return s.ParseReminderText(in.Text)
}

// CreateReminder runs creation as an explicit, ordered sequence of named
// steps, each of which may reject the input before the next one runs:
//
//  1. resolveStub - expand a leading ":name" into the fields its
//     definition carries.
//  2. validateNotificationOptions - the notification options ntfy itself
//     would reject (priority, click).
//  3. ParseReminderText - task text and due time out of in.Text.
//  4. generateUniqueID - an id that collides with nothing pending or
//     archived.
//  5. newReminder - assemble the record.
//  6. store.SaveReminder - persist it.
//
// The order is load-bearing. Option validation runs
// first because everything ntfy would reject must be rejected at create
// time, while there is still a caller to report it to: a reminder that
// stores cleanly and then fails forever at fire time is the one outcome
// this pipeline exists to prevent. That is also why stub resolution -
// the one step that can *supply* those options - runs ahead of
// validation rather than after it: a hand-edited stub carrying a bad
// priority would otherwise slip past create-time validation entirely.
func (s *Service) CreateReminder(in CreateInput) (*reminder.Reminder, error) {
	in, err := s.resolveStub(in)
	if err != nil {
		return nil, err
	}

	if err := validateNotificationOptions(in); err != nil {
		return nil, err
	}

	task, dueAt, err := s.ParseReminderText(in.Text)
	if err != nil {
		return nil, err
	}

	id, err := s.generateUniqueID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate reminder id: %w", err)
	}

	rem := s.newReminder(id, task, dueAt, in)

	if err := s.store.SaveReminder(*rem); err != nil {
		return nil, fmt.Errorf("failed to store reminder: %w", err)
	}

	return rem, nil
}

// resolveStub expands a leading ":name" into the reminder its stub
// defines. Anything not stub-shaped is returned untouched, and the
// stubs file is read only once the text is stub-shaped, so a missing or
// malformed stubs.json can never affect a plain reminder.
//
// The merge:
//
//   - text: the stub's, with any trailing text appended raw. ":hockey in
//     30m" becomes "in 15m back to the game in 30m", so the stub's own
//     time wins by being leftmost and "in 30m" stays in the task text.
//     No time-phrase detection - a plain reminder carrying two durations
//     already resolves this way.
//   - priority and click: the request's if it supplied them, else the
//     stub's.
//   - tags: both, deduped.
//   - outbound topics: never the stub's - routing is already concrete
//     by the time creation runs.
//
// Expansion happens once. A stub whose own text is stub-shaped is
// rejected rather than expanded again, and rejecting it here rather
// than leaving it to the parser matters because only some such bodies
// fail to parse: ":laundry in 45m" parses fine and would store a
// reminder whose task text is a literal sigil.
func (s *Service) resolveStub(in CreateInput) (CreateInput, error) {
	text := strings.TrimSpace(in.Text)

	match := stubInvocationRegex.FindStringSubmatch(text)
	if match == nil {
		return in, nil
	}
	name := strings.ToLower(match[1])

	stubs, err := s.store.LoadStubs()
	if err != nil {
		return CreateInput{}, fmt.Errorf("failed to load stubs: %w", err)
	}
	stub, ok := stubs[name]
	if !ok {
		return CreateInput{}, fmt.Errorf("%w: unknown stub %q", ErrInvalidInput, ":"+match[1])
	}
	if stubInvocationRegex.MatchString(strings.TrimSpace(stub.Text)) {
		return CreateInput{}, fmt.Errorf("%w: stub %q expands to another stub invocation; stubs expand once", ErrInvalidInput, ":"+name)
	}

	in.Text = stub.Text
	if trailing := strings.TrimSpace(text[len(match[0]):]); trailing != "" {
		in.Text = stub.Text + " " + trailing
	}
	if len(stub.Tags)+len(in.Tags) > 0 {
		in.Tags = reminder.DedupeStrings(append(append([]string{}, stub.Tags...), in.Tags...))
	}
	if in.Priority == "" {
		in.Priority = stub.Priority
	}
	if in.Click == "" {
		in.Click = stub.Click
	}

	return in, nil
}

// ListStubs returns every defined stub, each carrying its own name,
// sorted alphabetically. The map the file stores has no order at all,
// so the sort is what makes the listing deterministic.
func (s *Service) ListStubs() ([]reminder.NamedStub, error) {
	stubs, err := s.store.LoadStubs()
	if err != nil {
		return nil, fmt.Errorf("failed to load stubs: %w", err)
	}

	named := make([]reminder.NamedStub, 0, len(stubs))
	for name, stub := range stubs {
		named = append(named, reminder.NamedStub{Name: name, Stub: stub})
	}
	slices.SortFunc(named, func(x, y reminder.NamedStub) int { return strings.Compare(x.Name, y.Name) })
	return named, nil
}

// SetStub validates a definition and stores it under name, replacing
// any stub already held there. Addressed by a caller-chosen name and
// replacing wholesale, it is idempotent: the same write twice leaves
// the same single entry, so there is no create-versus-conflict case.
//
// It reports whether the name was new. That is not a conflict signal -
// both outcomes are a successful write - it is what lets a surface
// confirming the write say "created" or "updated", so a mistyped name
// does not read like an edit of the stub that was meant.
//
// Validation is on the way in, and it is the only validation there is.
func (s *Service) SetStub(name string, stub reminder.Stub) (bool, error) {
	if err := s.validateStub(name, stub); err != nil {
		return false, err
	}
	created, err := s.store.SetStub(name, stub)
	if err != nil {
		return false, fmt.Errorf("failed to store stub: %w", err)
	}
	return created, nil
}

// validateStub rejects anything that could never produce a reminder,
// running the same checks CreateReminder's pipeline runs and in its
// order, so a stub that stores cleanly is one an invocation can
// actually use.
//
// Two checks are the stub surface's own. A stub-shaped body is rejected
// because expansion happens once, so it could never resolve. And the
// name must conform exactly.
func (s *Service) validateStub(name string, stub reminder.Stub) error {
	if !stubNameRegex.MatchString(name) {
		return fmt.Errorf("%w: stub name %q must be lowercase, start with a letter, and hold only letters, digits, '-' and '_'", ErrInvalidInput, name)
	}
	if stubInvocationRegex.MatchString(strings.TrimSpace(stub.Text)) {
		return fmt.Errorf("%w: stub text %q is itself a stub invocation; stubs expand once, so it could never resolve", ErrInvalidInput, stub.Text)
	}
	if err := validateNotificationOptions(CreateInput{Priority: stub.Priority, Click: stub.Click}); err != nil {
		return err
	}
	// Past-due is the one parse failure tolerated here. It says the text
	// cannot be scheduled *right now*, not that it never could, and a
	// stub is re-parsed on every invocation anyway - so rejecting it
	// would only make whether "standup at 9am" can be defined depend on
	// what time of day you happen to define it, returning 200 in the
	// morning and 400 in the afternoon for byte-identical input.
	//
	// The cost is that a stub whose text can never be future ("buy milk
	// yesterday") is now accepted. That is bounded: it fails at create
	// time on every invocation, before anything is stored, so it can
	// never put a broken record into pending.json.
	if _, _, err := s.ParseReminderText(stub.Text); err != nil && !errors.Is(err, ErrPastDue) {
		return err
	}
	return nil
}

// DeleteStub removes name, reporting ErrNotFound when there was nothing
// to remove - consistent with cancelling a reminder that does not
// exist.
func (s *Service) DeleteStub(name string) error {
	found, err := s.store.DeleteStub(name)
	if err != nil {
		return fmt.Errorf("failed to delete stub: %w", err)
	}
	if !found {
		return fmt.Errorf("stub %q %w", name, ErrNotFound)
	}
	return nil
}

// newReminder assembles the stored record out of an already-validated
// input, an already-parsed task and due time, and an already-unique id.
// It is the last step of CreateReminder's pipeline that can still shape
// what gets stored, so anything a later step in that sequence needs to
// see must be folded in here rather than after the save.
func (s *Service) newReminder(id, task string, dueAt time.Time, in CreateInput) *reminder.Reminder {
	return &reminder.Reminder{
		ID:             id,
		Text:           task,
		DueAt:          dueAt,
		CreatedAt:      s.now(),
		OutboundTopics: in.OutboundTopics,
		Tags:           reminder.DedupeStrings(in.Tags),
		Priority:       in.Priority,
		Click:          in.Click,
	}
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

// GetArchived looks up id in the archive only - unlike Get, a pending
// reminder is treated as not found. Used by the clear endpoint,
// which only ever makes sense for a reminder that already fired (and so
// already has NtfyMessageIDs to clear).
func (s *Service) GetArchived(id string) (*reminder.ArchivedReminder, error) {
	archive, err := s.store.ListArchive()
	if err != nil {
		return nil, err
	}
	for _, r := range archive {
		if r.ID == id {
			return &r, nil
		}
	}
	return nil, fmt.Errorf("reminder %s %w", id, ErrNotFound)
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
