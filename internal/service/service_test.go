package service

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zawnk/later/internal/reminder"
)

type mockStore struct {
	saved      []reminder.Reminder
	saveErr    error
	archive    []reminder.ArchivedReminder
	archiveErr error
}

func (m *mockStore) SaveReminder(r reminder.Reminder) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.saved = append(m.saved, r)
	return nil
}

func (m *mockStore) ListPendingReminders() []reminder.Reminder { return m.saved }
func (m *mockStore) ListArchive() ([]reminder.ArchivedReminder, error) {
	return m.archive, m.archiveErr
}
func (m *mockStore) CancelReminder(id string) (bool, error) { return false, nil }

func TestPreprocessDuration(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"in-prefixed day", "in 3d", "in 3 days"},
		{"within-prefixed day", "within 3d", "within 3 days"},
		{"weeks", "in 4w test", "in 4 weeks test"},
		{"months", "in 3mo time", "in 3 months time"},
		{"three letter mon", "pok3 mon", "pok3 mon"},
		{"years", "in 10y is a decade", "in 10 years is a decade"},
		{"seconds", "in 60s make a minute", "in 60 seconds make a minute"},
		{"no time included", "no time included", "no time included"},
		{"bare unit with no in/within is left untouched", "3d", "3d"},
		{"bare months left untouched", "3mo time", "3mo time"},
		{"no longer matches inside a word)", "in server1h down", "in server1h down"},
		{"no longer matches inside a word)", "in remind me to check server1h status", "in remind me to check server1h status"},
		{"combined units are no longer this function's job", "in 2h30m", "in 2h30m"},
		{"combined units are no longer this function's job, full combo", "in 1y2mo3w4d5h6m7s", "in 1y2mo3w4d5h6m7s"},
		{"extra whitespace around the unit doesn't leave a double space", "in 3d  call the plumber", "in 3 days call the plumber"},
		{"a non-in-prefixed unit earlier in the text is left alone", "buy 2h of parking in 3d", "buy 2h of parking in 3 days"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := preprocessDuration(tt.in)
			if got != tt.want {
				t.Errorf("preprocessDuration(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCollapseWhitespace(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"already single-spaced", "a b c", "a b c"},
		{"double space", "a  b", "a b"},
		{"triple space", "a   b", "a b"},
		{"leading and trailing whitespace", "  a b  ", "a b"},
		{"tabs and newlines", "a\tb\nc", "a b c"},
		{"empty string", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collapseWhitespace(tt.in)
			if got != tt.want {
				t.Errorf("collapseWhitespace(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSumDurationMatches covers the pure arithmetic Postpone no longer
// reaches directly (applyDuration was removed once Postpone unified onto
// parseDueTime, see the "in "-prefixing comment on Postpone) but which
// parseDueTime's own combined-duration branch still relies on for 2+
// chained compact units.
func TestSumDurationMatches(t *testing.T) {
	tests := []struct {
		name        string
		matches     [][]string
		wantYears   int
		wantMonths  int
		wantDays    int
		wantSeconds float64
	}{
		{"single day", [][]string{{"1d", "1", "d"}}, 0, 0, 1, 0},
		{"week converts to days", [][]string{{"1w", "1", "w"}}, 0, 0, 7, 0},
		{"month then day", [][]string{{"1mo", "1", "mo"}, {"1d", "1", "d"}}, 0, 1, 1, 0},
		{"day then month (order shouldn't matter)", [][]string{{"1d", "1", "d"}, {"1mo", "1", "mo"}}, 0, 1, 1, 0},
		{"clock units accumulate as a duration", [][]string{{"120s", "120", "s"}}, 0, 0, 0, 120},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			years, months, days, clock := sumDurationMatches(tt.matches)
			if years != tt.wantYears || months != tt.wantMonths || days != tt.wantDays {
				t.Errorf("sumDurationMatches() = (y=%d,mo=%d,d=%d), want (y=%d,mo=%d,d=%d)", years, months, days, tt.wantYears, tt.wantMonths, tt.wantDays)
			}
			if clock.Seconds() != tt.wantSeconds {
				t.Errorf("sumDurationMatches() clock = %v, want %v seconds", clock, tt.wantSeconds)
			}
		})
	}
}

func TestIsUnambiguousDurationRun(t *testing.T) {
	tests := []struct {
		name           string
		run            string
		atStart, atEnd bool
		want           bool
	}{
		{"in-flagged run trusted regardless of position", "in 1w2d", false, false, true},
		{"within-flagged run trusted regardless of position", "within 1w2d", false, false, true},
		{"bare run at the start is trusted", "1w2d", true, false, true},
		{"bare run at the end is trusted", "1w2d", false, true, true},
		{"bare run sandwiched in the middle is not trusted", "1w2d", false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isUnambiguousDurationRun(tt.run, tt.atStart, tt.atEnd)
			if got != tt.want {
				t.Errorf("isUnambiguousDurationRun(%q, atStart=%v, atEnd=%v) = %v, want %v", tt.run, tt.atStart, tt.atEnd, got, tt.want)
			}
		})
	}
}

// TestParseDueTime_CombinedDurationDST proves parseDueTime's combined-unit
// branch (2+ chained compact units, e.g. "1d2h") still preserves
// wall-clock time across a DST transition - the same guarantee
// applyDuration used to provide directly before Postpone unified onto
// parseDueTime. Single bare units ("1d" alone) go through when.Parse
// instead and don't carry this same guarantee (when adds a flat 24h
// rather than preserving wall-clock time) - a pre-existing when.Parse
// characteristic, not something introduced here, and not covered by this
// test since it's not this codebase's arithmetic to promise.
func TestParseDueTime_CombinedDurationDST(t *testing.T) {
	svc := New(&mockStore{})
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("failed to load Europe/Berlin location: %v", err)
	}

	from := time.Date(2026, 3, 28, 12, 0, 0, 0, berlin)
	svc.now = func() time.Time { return from }

	_, _, due, err := svc.parseDueTime("1d2h")
	if err != nil {
		t.Fatalf("parseDueTime() error = %v", err)
	}

	want := from.AddDate(0, 0, 1).Add(2 * time.Hour)
	if !due.Equal(want) {
		t.Errorf("parseDueTime(\"1d2h\") across DST = %v, want %v (wall-clock-preserving day, then a real 2h)", due, want)
	}
}

// TestParseDueTime_TrimsSurroundingWhitespace guards a latent edge case in
// the atStart/atEnd anchoring combinedDurationRegex.FindStringSubmatchIndex
// relies on: without trimming first, a trailing space (or more than one
// leading space) shifts loc[1]/loc[0] away from len(text)/0, so an
// otherwise-anchored bare run like "1w2d" would be wrongly treated as
// sandwiched mid-text and rejected. Not reachable today through
// CreateReminder or resolvePostponeTime - both already trim their whole
// input before calling parseDueTime - but parseDueTime trims defensively
// too so this stays true for any future caller that doesn't.
func TestParseDueTime_TrimsSurroundingWhitespace(t *testing.T) {
	fixedNow := time.Date(2026, 6, 15, 9, 0, 0, 0, time.Local)
	svc := New(&mockStore{})
	svc.now = func() time.Time { return fixedNow }

	tests := []struct {
		name string
		text string
	}{
		{"trailing whitespace after a bare run at the end", "clean the gutters 1w2d "},
		{"multiple leading spaces before a bare run at the start", "  1w2d clean the gutters"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, due, err := svc.parseDueTime(tt.text)
			if err != nil {
				t.Fatalf("parseDueTime(%q) error = %v", tt.text, err)
			}
			want := fixedNow.AddDate(0, 0, 9)
			if !due.Equal(want) {
				t.Errorf("parseDueTime(%q) = %v, want %v", tt.text, due, want)
			}
		})
	}
}

func TestCreateReminder(t *testing.T) {
	fixedNow := time.Date(2026, 6, 15, 9, 0, 0, 0, time.Local)

	tests := []struct {
		name           string
		inputText      string
		outboundTopics []string
		expectedText   string
		expectedDue    time.Time
		wantErr        bool
	}{
		{"valid reminder with relative time", "buy milk in 3 days", []string{"topic-a"}, "buy milk", fixedNow.AddDate(0, 0, 3), false},
		{"empty text", "", []string{"topic-a"}, "", time.Time{}, true},
		{"whitespace-only text", "   ", []string{"topic-a"}, "", time.Time{}, true},
		{"string longer than maxReminderTextLength", strings.Repeat("a", 4097), []string{"topic-a"}, "", time.Time{}, true},
		{"string exactly maxReminderTextLength", strings.Repeat("a", 4086) + " in 9 days", []string{"topic-a"}, strings.Repeat("a", 4086), fixedNow.AddDate(0, 0, 9), false},
		{"text with no time", "buy eggs", []string{"topic-a"}, "", time.Time{}, true},
		{"embedded multi-unit sequence inside a word is not a duration, combined-run path)", "check server1h2m status", []string{"topic-a"}, "", time.Time{}, true},
		{"valid reminder with calendar date", "marathon is on 10/10/2026", []string{"topic-a"}, "marathon is on", time.Date(2026, 10, 10, 9, 0, 0, 0, time.Local), false},
		{"slash date is day/month, not month/day", "book the campsite 28/07/2026", []string{"topic-a"}, "book the campsite", time.Date(2026, 7, 28, 9, 0, 0, 0, time.Local), false},
		{"weekday expression", "let's meet next tuesday", []string{"topic-a"}, "let's meet", time.Date(2026, 06, 16, 9, 0, 0, 0, time.Local), false},
		{"just a time string", "in 2 hours", []string{"topic-a"}, "", time.Time{}, true},
		{"due date in the past", "buy milk yesterday", []string{"topic-a"}, "", time.Time{}, true},
		{"check the at trim", "attend standup in 3 days", []string{"topic-a"}, "attend standup", fixedNow.AddDate(0, 0, 3), false},
		{"dangling at trim", "call mom at 5pm", []string{"topic-a"}, "call mom", time.Date(2026, 6, 15, 17, 0, 0, 0, time.Local), false},
		{"leading at trim", "at 5pm call mom", []string{"topic-a"}, "call mom", time.Date(2026, 6, 15, 17, 0, 0, 0, time.Local), false},
		{"mid-string double space collapse, no at involved", "buy milk tomorrow from the store", []string{"topic-a"}, "buy milk from the store", fixedNow.AddDate(0, 0, 1), false},
		{"multiple outbound topics are passed through", "buy milk in 3 days", []string{"topic-a", "topic-b"}, "buy milk", fixedNow.AddDate(0, 0, 3), false},
		{"nil outbound topics are passed through", "buy milk in 3 days", nil, "buy milk", fixedNow.AddDate(0, 0, 3), false},
		{"compact duration: single unit", "buy milk in 3d", []string{"topic-a"}, "buy milk", fixedNow.AddDate(0, 0, 3), false},
		{"compact duration: combined weeks+days", "clean the gutters in 1w2d", []string{"topic-a"}, "clean the gutters", fixedNow.AddDate(0, 0, 9), false},
		{"compact duration: combined hours+minutes", "check the oven in 2h30m", []string{"topic-a"}, "check the oven", fixedNow.Add(2*time.Hour + 30*time.Minute), false},
		{"compact duration: every unit combined", "renew everything in 1y2mo3w4d5h6m", []string{"topic-a"}, "renew everything", fixedNow.AddDate(1, 2, 25).Add(5*time.Hour + 6*time.Minute), false},
		{"exact calendar date", "defrost the freezer october 21st", []string{"topic-a"}, "defrost the freezer", time.Date(2026, 10, 21, 9, 0, 0, 0, time.Local), false},
		{"combined weekday and time", "pick up the dry cleaning next tuesday at 2pm", []string{"topic-a"}, "pick up the dry cleaning", time.Date(2026, 6, 16, 14, 0, 0, 0, time.Local), false},
		{"a duration-shaped substring inside the task text is not rewritten", "in 3d buy 5m of rope", []string{"topic-a"}, "buy 5m of rope", fixedNow.AddDate(0, 0, 3), false},
		{"a second duration-shaped substring later in the task is left alone too", "remind me in 2h to check the 5h parking meter", []string{"topic-a"}, "remind me to check the 5h parking meter", fixedNow.Add(2 * time.Hour), false},
		{"an earlier single-unit duration is not overridden by a later combined-unit-shaped task fragment", "in 3d buy 1y2mo of insurance", []string{"topic-a"}, "buy 1y2mo of insurance", fixedNow.AddDate(0, 0, 3), false},
		{"a non-in-prefixed duration-shaped fragment before the real duration doesn't break recognition", "buy 2h of parking in 3d", []string{"topic-a"}, "buy 2h of parking", fixedNow.AddDate(0, 0, 3), false},
		{"same case with a number and unrelated bare number mixed in", "reserve court 3 for 1h in 2d", []string{"topic-a"}, "reserve court 3 for 1h", fixedNow.AddDate(0, 0, 2), false},
		{"an in-prefixed combined duration works with trailing words after it", "call the plumber in 1w2d please", []string{"topic-a"}, "call the plumber please", fixedNow.AddDate(0, 0, 9), false},
		{"a bare combined duration at the very start still works", "1w2d clean the gutters", []string{"topic-a"}, "clean the gutters", fixedNow.AddDate(0, 0, 9), false},
		{"a bare combined duration at the very end still works", "clean the gutters 1w2d", []string{"topic-a"}, "clean the gutters", fixedNow.AddDate(0, 0, 9), false},
		{"two in-prefixed combined durations: the leftmost wins", "call in 1d2h then in 3d4h again", []string{"topic-a"}, "call then in 3d4h again", fixedNow.Add(26 * time.Hour), false},
		{"within works as a trigger for a single unit, same as in", "within 3d call mom", []string{"topic-a"}, "call mom", fixedNow.AddDate(0, 0, 3), false},
		{"a bare combined duration with no task at all still requires one", "1w2d", []string{"topic-a"}, "", time.Time{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &mockStore{}
			svc := New(store)
			svc.now = func() time.Time { return fixedNow }

			rem, err := svc.CreateReminder(CreateInput{Text: tt.inputText, OutboundTopics: tt.outboundTopics})
			if (err != nil) != tt.wantErr {
				t.Fatalf("CreateReminder(%q) error = %v, wantErr %v", tt.inputText, err, tt.wantErr)
			}

			if tt.wantErr {
				if !errors.Is(err, ErrInvalidInput) {
					t.Errorf("CreateReminder(%q) error = %v, want it to wrap ErrInvalidInput", tt.inputText, err)
				}
				return
			}

			if rem == nil {
				t.Fatal("CreateReminder() returned nil reminder with no error")
			}

			if len(store.saved) != 1 {
				t.Errorf("expected 1 reminder saved to the store, got %d", len(store.saved))
			}

			if rem.Text != tt.expectedText {
				t.Errorf("CreateReminder(%q) text = %q, want %q", tt.inputText, rem.Text, tt.expectedText)
			}

			if !rem.DueAt.Equal(tt.expectedDue) {
				t.Errorf("CreateReminder(%q) DueAt = %v, want %v", tt.inputText, rem.DueAt, tt.expectedDue)
			}

			if !rem.CreatedAt.Equal(fixedNow) {
				t.Errorf("CreateReminder(%q) CreatedAt = %v, want %v (the injected clock)", tt.inputText, rem.CreatedAt, fixedNow)
			}

			if !slices.Equal(rem.OutboundTopics, tt.outboundTopics) {
				t.Errorf("CreateReminder(%q) OutboundTopics = %v, want %v", tt.inputText, rem.OutboundTopics, tt.outboundTopics)
			}
		})
	}
}

func TestCreateReminder_StoreError(t *testing.T) {
	proxyErr := errors.New("disk full")
	store := &mockStore{saveErr: proxyErr}
	svc := New(store)
	svc.now = func() time.Time { return time.Date(2026, 6, 15, 9, 0, 0, 0, time.Local) }

	rem, err := svc.CreateReminder(CreateInput{Text: "buy milk in 3 days", OutboundTopics: []string{"topic-a"}})
	if err == nil {
		t.Fatal("CreateReminder() error = nil, want an error from the store")
	}

	if !errors.Is(err, proxyErr) {
		t.Errorf("CreateReminder() error = %v, want it to wrap %v", err, proxyErr)
	}

	if errors.Is(err, ErrInvalidInput) {
		t.Errorf("CreateReminder() store error wraps ErrInvalidInput, want it treated as internal")
	}

	if rem != nil {
		t.Errorf("CreateReminder() returned non-nil reminder %v alongside an error", rem)
	}
}

func TestCreateReminder_NotificationOptionsPassthrough(t *testing.T) {
	store := &mockStore{}
	svc := New(store)
	svc.now = func() time.Time { return time.Date(2026, 6, 15, 9, 0, 0, 0, time.Local) }

	in := CreateInput{
		Text:           "buy cake in 3 days",
		OutboundTopics: []string{"topic-a"},
		Tags:           []string{"partying_face", "birthday"},
		Priority:       "high",
		Click:          "https://example.com/cake-recipe",
	}
	rem, err := svc.CreateReminder(in)
	if err != nil {
		t.Fatalf("CreateReminder() error = %v", err)
	}

	if !slices.Equal(rem.Tags, in.Tags) {
		t.Errorf("CreateReminder() Tags = %v, want %v", rem.Tags, in.Tags)
	}

	if rem.Priority != in.Priority {
		t.Errorf("CreateReminder() Priority = %q, want %q", rem.Priority, in.Priority)
	}

	if rem.Click != in.Click {
		t.Errorf("CreateReminder() Click = %q, want %q", rem.Click, in.Click)
	}

	if len(store.saved) != 1 || !slices.Equal(store.saved[0].Tags, in.Tags) || store.saved[0].Priority != in.Priority || store.saved[0].Click != in.Click {
		t.Errorf("stored reminder = %+v, want tags/priority/click persisted, not just returned", store.saved)
	}
}

func TestCreateReminder_DedupesTags(t *testing.T) {
	store := &mockStore{}
	svc := New(store)
	svc.now = func() time.Time { return time.Date(2026, 6, 15, 9, 0, 0, 0, time.Local) }

	in := CreateInput{
		Text:           "buy cake in 3 days",
		OutboundTopics: []string{"topic-a"},
		Tags:           []string{"birthday", "partying_face", "birthday"},
	}
	rem, err := svc.CreateReminder(in)
	if err != nil {
		t.Fatalf("CreateReminder() error = %v", err)
	}

	want := []string{"birthday", "partying_face"}
	if !slices.Equal(rem.Tags, want) {
		t.Errorf("CreateReminder() Tags = %v, want %v (deduplicated, matching ntfy's inbound directive path)", rem.Tags, want)
	}

	if len(store.saved) != 1 || !slices.Equal(store.saved[0].Tags, want) {
		t.Errorf("stored reminder tags = %v, want %v persisted deduplicated, not just returned", store.saved[0].Tags, want)
	}
}

func TestCreateReminder_NotificationOptionsValidation(t *testing.T) {
	tests := []struct {
		name     string
		priority string
		click    string
		wantErr  bool
	}{
		{"priority name is valid", "urgent", "", false},
		{"priority digit is valid", "5", "", false},
		{"unknown priority is rejected", "sometime", "", true},
		{"priority typo is rejected", "hgih", "", true},
		{"absolute click URL is valid", "", "https://example.com/x", false},
		{"app-scheme click URL is valid", "", "geo:52.5,13.4", false},
		{"scheme-less click is rejected", "", "example.com/x", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := New(&mockStore{})
			svc.now = func() time.Time { return time.Date(2026, 6, 15, 9, 0, 0, 0, time.Local) }

			_, err := svc.CreateReminder(CreateInput{Text: "buy milk in 3 days", Priority: tt.priority, Click: tt.click})
			if (err != nil) != tt.wantErr {
				t.Fatalf("CreateReminder(priority=%q, click=%q) error = %v, wantErr %v", tt.priority, tt.click, err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrInvalidInput) {
				t.Errorf("CreateReminder() error = %v, want it to wrap ErrInvalidInput", err)
			}
		})
	}
}

func TestNext(t *testing.T) {
	tests := []struct {
		name    string
		pending []reminder.Reminder
		want    *reminder.Reminder
	}{
		{
			name:    "no pending reminders",
			pending: nil,
			want:    nil,
		},
		{
			name:    "single pending reminder",
			pending: []reminder.Reminder{{ID: "a", DueAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}},
			want:    &reminder.Reminder{ID: "a", DueAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
		},
		{
			name: "earliest DueAt wins, regardless of slice order",
			pending: []reminder.Reminder{
				{ID: "later", DueAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
				{ID: "earliest", DueAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
				{ID: "middle", DueAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
			},
			want: &reminder.Reminder{ID: "earliest", DueAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
		{
			name: "tie on DueAt keeps the first one seen",
			pending: []reminder.Reminder{
				{ID: "first", DueAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
				{ID: "second", DueAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			},
			want: &reminder.Reminder{ID: "first", DueAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &mockStore{saved: tt.pending}
			svc := New(store)

			got := svc.Next()
			if tt.want == nil {
				if got != nil {
					t.Fatalf("Next() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("Next() = nil, want a reminder")
			}
			if got.ID != tt.want.ID {
				t.Errorf("Next().ID = %q, want %q", got.ID, tt.want.ID)
			}
			if !got.DueAt.Equal(tt.want.DueAt) {
				t.Errorf("Next().DueAt = %v, want %v", got.DueAt, tt.want.DueAt)
			}
		})
	}
}

func TestLast(t *testing.T) {
	tests := []struct {
		name       string
		archive    []reminder.ArchivedReminder
		archiveErr error
		wantErr    bool
		want       *reminder.ArchivedReminder
	}{
		{
			name:    "empty archive returns nil, nil",
			archive: nil,
			want:    nil,
		},
		{
			name: "returns the last element",
			archive: []reminder.ArchivedReminder{
				{Reminder: reminder.Reminder{ID: "first"}},
				{Reminder: reminder.Reminder{ID: "second"}},
				{Reminder: reminder.Reminder{ID: "last"}},
			},
			want: &reminder.ArchivedReminder{Reminder: reminder.Reminder{ID: "last"}},
		},
		{
			name:       "store error is forwarded, not swallowed",
			archiveErr: errors.New("disk error"),
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &mockStore{archive: tt.archive, archiveErr: tt.archiveErr}
			svc := New(store)

			got, err := svc.Last()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Last() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("Last() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("Last() = nil, want a reminder")
			}
			if got.ID != tt.want.ID {
				t.Errorf("Last().ID = %q, want %q", got.ID, tt.want.ID)
			}
		})
	}
}

func TestPostpone(t *testing.T) {
	fixedNow := time.Date(2026, 6, 15, 9, 0, 0, 0, time.Local)

	archived := reminder.ArchivedReminder{
		Reminder: reminder.Reminder{
			ID:             "abc123",
			Text:           "buy milk",
			OutboundTopics: []string{"topic-a"},
			Tags:           []string{"shopping_cart"},
			Priority:       "low",
			Click:          "https://example.com/list",
		},
		FiredAt: fixedNow.AddDate(0, 0, -1),
	}

	tests := []struct {
		name              string
		pending           []reminder.Reminder
		archive           []reminder.ArchivedReminder
		archiveErr        error
		id                string
		duration          string
		wantErr           string
		wantSentinelErrIs error
		wantDue           time.Time
	}{
		{
			name:              "still pending, refuses to postpone",
			pending:           []reminder.Reminder{{ID: "abc123"}},
			id:                "abc123",
			duration:          "1d",
			wantErr:           "still a pending reminder",
			wantSentinelErrIs: ErrStillPending,
		},
		{
			name:       "archive lookup fails",
			archiveErr: errors.New("disk error"),
			id:         "abc123",
			duration:   "1d",
			wantErr:    "disk error",
		},
		{
			name:              "not found in archive",
			archive:           []reminder.ArchivedReminder{archived},
			id:                "does-not-exist",
			duration:          "1d",
			wantErr:           "not found in archive",
			wantSentinelErrIs: ErrNotFound,
		},
		{
			name:              "invalid duration",
			archive:           []reminder.ArchivedReminder{archived},
			id:                "abc123",
			duration:          "not-a-duration",
			wantErr:           "invalid duration",
			wantSentinelErrIs: ErrInvalidInput,
		},
		{
			name:              "text merely containing a duration-shaped substring is not a duration",
			archive:           []reminder.ArchivedReminder{archived},
			id:                "abc123",
			duration:          "gate 3d assembly",
			wantErr:           "invalid duration",
			wantSentinelErrIs: ErrInvalidInput,
		},
		{
			name:     "valid postpone with a bare compact duration (existing CLI behavior, unaffected)",
			archive:  []reminder.ArchivedReminder{archived},
			id:       "abc123",
			duration: "1d",
			wantDue:  fixedNow.AddDate(0, 0, 1),
		},
		{
			name:     "compact duration with an 'in' prefix",
			archive:  []reminder.ArchivedReminder{archived},
			id:       "abc123",
			duration: "in 1h",
			wantDue:  fixedNow.Add(time.Hour),
		},
		{
			name:     "full natural-language phrase falls through to parseDueTime",
			archive:  []reminder.ArchivedReminder{archived},
			id:       "abc123",
			duration: "tomorrow morning",
			wantDue:  time.Date(2026, 6, 16, 8, 0, 0, 0, time.Local),
		},
		{
			name:     "caller-supplied leading 'in' is not doubled",
			archive:  []reminder.ArchivedReminder{archived},
			id:       "abc123",
			duration: "in tomorrow",
			wantDue:  fixedNow.AddDate(0, 0, 1),
		},
		{
			name:              "combined duration-shaped substring embedded in unrelated text is not a duration",
			archive:           []reminder.ArchivedReminder{archived},
			id:                "abc123",
			duration:          "garbage 1y2mo garbage",
			wantErr:           "invalid duration",
			wantSentinelErrIs: ErrInvalidInput,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &mockStore{saved: tt.pending, archive: tt.archive, archiveErr: tt.archiveErr}
			svc := New(store)
			svc.now = func() time.Time { return fixedNow }

			rem, err := svc.Postpone(tt.id, tt.duration)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Postpone() error = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Postpone() error = %q, want it to contain %q", err.Error(), tt.wantErr)
				}
				if tt.wantSentinelErrIs != nil && !errors.Is(err, tt.wantSentinelErrIs) {
					t.Errorf("Postpone() error = %v, want it to wrap %v", err, tt.wantSentinelErrIs)
				}
				if rem != nil {
					t.Errorf("Postpone() returned non-nil reminder %v alongside an error", rem)
				}
				return
			}

			if err != nil {
				t.Fatalf("Postpone() unexpected error = %v", err)
			}
			if rem == nil {
				t.Fatal("Postpone() returned nil reminder with no error")
			}
			if rem.ID == tt.id {
				t.Errorf("Postpone() reused the old ID %q, want a freshly generated one", rem.ID)
			}
			if rem.Text != archived.Text {
				t.Errorf("Postpone() Text = %q, want %q", rem.Text, archived.Text)
			}
			if !slices.Equal(rem.OutboundTopics, archived.OutboundTopics) {
				t.Errorf("Postpone() OutboundTopics = %v, want %v", rem.OutboundTopics, archived.OutboundTopics)
			}
			if !slices.Equal(rem.Tags, archived.Tags) {
				t.Errorf("Postpone() Tags = %v, want %v (carried over like the topics)", rem.Tags, archived.Tags)
			}
			if rem.Priority != archived.Priority || rem.Click != archived.Click {
				t.Errorf("Postpone() Priority/Click = %q/%q, want %q/%q (carried over like the topics)", rem.Priority, rem.Click, archived.Priority, archived.Click)
			}
			if !rem.DueAt.Equal(tt.wantDue) {
				t.Errorf("Postpone() DueAt = %v, want %v", rem.DueAt, tt.wantDue)
			}
			if !rem.CreatedAt.Equal(fixedNow) {
				t.Errorf("Postpone() CreatedAt = %v, want %v", rem.CreatedAt, fixedNow)
			}
			if len(store.saved) != len(tt.pending)+1 {
				t.Errorf("expected the postponed reminder to be saved to the store, saved count = %d", len(store.saved))
			}
		})
	}
}

// TestResolvePostponeTime drives the pure resolution function directly
// with a broader battery of edge cases than TestPostpone's table (which
// only needs enough to prove Postpone wires it up correctly).
func TestResolvePostponeTime(t *testing.T) {
	fixedNow := time.Date(2026, 6, 15, 9, 0, 0, 0, time.Local)

	tests := []struct {
		name    string
		expr    string
		wantErr bool
		wantDue time.Time
	}{
		{"empty string", "", true, time.Time{}},
		{"whitespace only", "   ", true, time.Time{}},
		{"pure garbage", "asdkfj", true, time.Time{}},
		{"bare single unit", "1d", false, fixedNow.AddDate(0, 0, 1)},
		{"single unit with in", "in 1d", false, fixedNow.AddDate(0, 0, 1)},
		{"single unit with double in", "in in 1h", false, fixedNow.Add(time.Hour)},
		{"bare combined units", "3h20m", false, fixedNow.Add(3*time.Hour + 20*time.Minute)},
		{"combined units with in", "in 3h20m", false, fixedNow.Add(3*time.Hour + 20*time.Minute)},
		{"combined units with within", "within 1d2h", false, fixedNow.AddDate(0, 0, 1).Add(2 * time.Hour)},
		{"casual phrase", "tomorrow", false, fixedNow.AddDate(0, 0, 1)},
		{"casual phrase already prefixed with in (not doubled)", "in tomorrow", false, fixedNow.AddDate(0, 0, 1)},
		{"casual phrase with time-of-day word", "tomorrow morning", false, time.Date(2026, 6, 16, 8, 0, 0, 0, time.Local)},
		{"weekday", "next tuesday", false, time.Date(2026, 6, 16, 9, 0, 0, 0, time.Local)},
		{"single unit embedded in unrelated text", "3d rotate the tires", true, time.Time{}},
		{"combined units embedded in unrelated text", "garbage 1y2mo garbage", true, time.Time{}},
		{"space-separated units are not combined", "1d 2h", true, time.Time{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := New(&mockStore{})
			svc.now = func() time.Time { return fixedNow }

			due, err := svc.resolvePostponeTime(tt.expr)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolvePostponeTime(%q) error = nil, want an error", tt.expr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePostponeTime(%q) unexpected error = %v", tt.expr, err)
			}
			if !due.Equal(tt.wantDue) {
				t.Errorf("resolvePostponeTime(%q) = %v, want %v", tt.expr, due, tt.wantDue)
			}
		})
	}
}

func TestGenerateUniqueID(t *testing.T) {
	t.Run("archive load error propagates", func(t *testing.T) {
		store := &mockStore{archiveErr: errors.New("disk read failed")}
		svc := New(store)

		if _, err := svc.generateUniqueID(); err == nil {
			t.Fatal("generateUniqueID() error = nil, want the archive load error surfaced")
		}
	})

	t.Run("retries on collision with a pending or archived id", func(t *testing.T) {
		store := &mockStore{
			saved:   []reminder.Reminder{{ID: "pending-id"}},
			archive: []reminder.ArchivedReminder{{Reminder: reminder.Reminder{ID: "archived-id"}}},
		}
		svc := New(store)

		draws := []string{"pending-id", "archived-id", "fresh-id"}
		call := 0
		svc.generateID = func() string {
			id := draws[call]
			call++
			return id
		}

		id, err := svc.generateUniqueID()
		if err != nil {
			t.Fatalf("generateUniqueID() error = %v", err)
		}
		if id != "fresh-id" {
			t.Errorf("generateUniqueID() = %q, want %q after rejecting two colliding draws", id, "fresh-id")
		}
		if call != 3 {
			t.Errorf("generator called %d times, want exactly 3 (two collisions then the accepted draw)", call)
		}
	})
}

func TestGet(t *testing.T) {
	pending := reminder.Reminder{ID: "pending-1", Text: "buy milk", DueAt: time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)}
	archived := reminder.ArchivedReminder{
		Reminder: reminder.Reminder{ID: "archived-1", Text: "call mom", DueAt: time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)},
		FiredAt:  time.Date(2026, 6, 15, 9, 1, 0, 0, time.UTC),
	}
	store := &mockStore{saved: []reminder.Reminder{pending}, archive: []reminder.ArchivedReminder{archived}}
	svc := New(store)

	t.Run("found pending", func(t *testing.T) {
		gotPending, gotArchived, err := svc.Get("pending-1")

		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}

		if gotArchived != nil {
			t.Errorf("Get() archived = %+v, want nil", gotArchived)
		}

		if gotPending == nil || gotPending.ID != "pending-1" {
			t.Errorf("Get() pending = %+v, want ID %q", gotPending, "pending-1")
		}
	})

	t.Run("found archived", func(t *testing.T) {
		gotPending, gotArchived, err := svc.Get("archived-1")

		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}

		if gotPending != nil {
			t.Errorf("Get() pending = %+v, want nil", gotPending)
		}

		if gotArchived == nil || gotArchived.ID != "archived-1" {
			t.Errorf("Get() archived = %+v, want ID %q", gotArchived, "archived-1")
		}
	})

	t.Run("not found returns ErrNotFound", func(t *testing.T) {
		gotPending, gotArchived, err := svc.Get("does-not-exist")

		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get() error = %v, want ErrNotFound", err)
		}

		if gotPending != nil || gotArchived != nil {
			t.Errorf("Get() = (%+v, %+v), want (nil, nil)", gotPending, gotArchived)
		}
	})

	t.Run("archive load error propagates", func(t *testing.T) {
		failingStore := &mockStore{archiveErr: errors.New("disk read failed")}
		failingSvc := New(failingStore)
		_, _, err := failingSvc.Get("anything")

		if err == nil {
			t.Fatal("Get() error = nil, want the archive load error surfaced")
		}
	})
}
