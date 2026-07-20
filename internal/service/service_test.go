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
		{"single day", "3d", "3 days"},
		{"with a leading word", "in 3d", "in 3 days"},
		{"back-to-back units", "2h30m", "2 hours 30 minutes"},
		{"matches inside a word", "server1h down", "server 1 hours down"},
		{"weeks", "in 4w test", "in 4 weeks test"},
		{"months", "3mo time", "3 months time"},
		{"three letter mon", "pok3 mon", "pok3 mon"},
		{"years", "10y is a decade", "10 years is a decade"},
		{"seconds", "60s make a minute", "60 seconds make a minute"},
		{"no time included", "no time included", "no time included"},
		{"all at once", "1y2mo3w4d5h6m7s", "1 years 2 months 3 weeks 4 days 5 hours 6 minutes 7 seconds"},
		{"order check", "3m1mo", "3 minutes 1 months"},
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

func TestApplyDuration(t *testing.T) {
	startDate := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("failed to load Europe/Berlin location: %v", err)
	}

	tests := []struct {
		name     string
		duration string
		from     time.Time
		want     time.Time
		wantErr  bool
	}{
		{"one day", "1d", startDate, startDate.AddDate(0, 0, 1), false},
		{"one week", "1w", startDate, startDate.AddDate(0, 0, 7), false},
		{"one year", "1y", startDate, startDate.AddDate(1, 0, 0), false},
		{"five minutes", "5m", startDate, startDate.Add(5 * time.Minute), false},
		{"120 seconds", "120s", startDate, startDate.Add(2 * time.Minute), false},
		{"combined units: month then day", "1mo1d", startDate, startDate.AddDate(0, 1, 1), false},
		{"combined units: day then month (order shouldn't matter)", "1d1mo", startDate, startDate.AddDate(0, 1, 1), false},
		{"invalid input has no error return, but check the error path", "not-a-duration", startDate, time.Time{}, true},
		{"combining calendar and clock unit", "1d2h", startDate, startDate.AddDate(0, 0, 1).Add(2 * time.Hour), false},
		{
			name:     "DST spring-forward: 1 day preserves wall-clock time",
			duration: "1d",
			from:     time.Date(2026, 3, 28, 12, 0, 0, 0, berlin),
			want:     time.Date(2026, 3, 29, 12, 0, 0, 0, berlin),
			wantErr:  false,
		}, {
			name:     "DST spring-forward: 12 hours preserves clock time",
			duration: "12h",
			from:     time.Date(2026, 3, 28, 19, 0, 0, 0, berlin),
			want:     time.Date(2026, 3, 29, 8, 0, 0, 0, berlin),
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyDuration(tt.duration, tt.from)
			if (err != nil) != tt.wantErr {
				t.Fatalf("applyDuration(%q) error = %v, wantErr %v", tt.duration, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !got.Equal(tt.want) {
				t.Errorf("applyDuration(%q) = %v, want %v", tt.duration, got, tt.want)
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
		{"valid reminder with calendar date", "marathon is on 10/10/2026", []string{"topic-a"}, "marathon is on", time.Date(2026, 10, 10, 9, 0, 0, 0, time.Local), false},
		{"weekday expression", "let's meet next tuesday", []string{"topic-a"}, "let's meet", time.Date(2026, 06, 16, 9, 0, 0, 0, time.Local), false},
		{"just a time string", "in 2 hours", []string{"topic-a"}, "", time.Time{}, true},
		{"due date in the past", "buy milk yesterday", []string{"topic-a"}, "", time.Time{}, true},
		{"check the at trim", "attend standup in 3 days", []string{"topic-a"}, "attend standup", fixedNow.AddDate(0, 0, 3), false},
		{"dangling at trim", "call mom at 5pm", []string{"topic-a"}, "call mom", time.Date(2026, 6, 15, 17, 0, 0, 0, time.Local), false},
		{"leading at trim", "at 5pm call mom", []string{"topic-a"}, "call mom", time.Date(2026, 6, 15, 17, 0, 0, 0, time.Local), false},
		{"mid-string double space collapse, no at involved", "buy milk tomorrow from the store", []string{"topic-a"}, "buy milk from the store", fixedNow.AddDate(0, 0, 1), false},
		{"multiple outbound topics are passed through", "buy milk in 3 days", []string{"topic-a", "topic-b"}, "buy milk", fixedNow.AddDate(0, 0, 3), false},
		{"nil outbound topics are passed through", "buy milk in 3 days", nil, "buy milk", fixedNow.AddDate(0, 0, 3), false},
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
			name:     "valid postpone",
			archive:  []reminder.ArchivedReminder{archived},
			id:       "abc123",
			duration: "1d",
			wantErr:  "",
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
			wantDue := fixedNow.AddDate(0, 0, 1)
			if !rem.DueAt.Equal(wantDue) {
				t.Errorf("Postpone() DueAt = %v, want %v", rem.DueAt, wantDue)
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
