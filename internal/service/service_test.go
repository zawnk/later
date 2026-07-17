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
	saved   []reminder.Reminder
	saveErr error
}

func (m *mockStore) SaveReminder(r reminder.Reminder) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.saved = append(m.saved, r)
	return nil
}

func (m *mockStore) ListPendingReminders() []reminder.Reminder         { return m.saved }
func (m *mockStore) ListArchive() ([]reminder.ArchivedReminder, error) { return nil, nil }
func (m *mockStore) CancelReminder(id string) (bool, error)            { return false, nil }

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
		{"check the at trim", "attend standup in 3 days", []string{"topic-a"}, "attend standup", fixedNow.AddDate(0, 0, 3), false},
		{"multiple outbound topics are passed through", "buy milk in 3 days", []string{"topic-a", "topic-b"}, "buy milk", fixedNow.AddDate(0, 0, 3), false},
		{"nil outbound topics are passed through", "buy milk in 3 days", nil, "buy milk", fixedNow.AddDate(0, 0, 3), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &mockStore{}
			svc := New(store)
			svc.now = func() time.Time { return fixedNow }

			rem, err := svc.CreateReminder(tt.inputText, tt.outboundTopics)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CreateReminder(%q) error = %v, wantErr %v", tt.inputText, err, tt.wantErr)
			}
			if tt.wantErr {
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

	rem, err := svc.CreateReminder("buy milk in 3 days", []string{"topic-a"})
	if err == nil {
		t.Fatal("CreateReminder() error = nil, want an error from the store")
	}

	if !errors.Is(err, proxyErr) {
		t.Errorf("CreateReminder() error = %v, want it to wrap %v", err, proxyErr)
	}
	if rem != nil {
		t.Errorf("CreateReminder() returned non-nil reminder %v alongside an error", rem)
	}
}
