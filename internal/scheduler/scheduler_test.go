package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zawnk/later/internal/reminder"
	"github.com/zawnk/later/internal/store"
)

type notifyRecorder struct {
	mu    sync.Mutex
	calls []notifyCall
	err   error
}

type notifyCall struct {
	r    reminder.Reminder
	late bool
}

func (n *notifyRecorder) notify(ctx context.Context, r reminder.Reminder, late bool) (map[string]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil {
		return nil, n.err
	}
	n.calls = append(n.calls, notifyCall{r: r, late: late})
	return map[string]string{"topic-a": "ntfy-id-" + r.ID}, nil
}

func (n *notifyRecorder) recorded() []notifyCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notifyCall(nil), n.calls...)
}

func newTestScheduler(t *testing.T) (*Scheduler, *store.Store, *notifyRecorder) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	rec := &notifyRecorder{}
	return New(st, rec.notify), st, rec
}

func TestTick_FiresOnlyDueReminders(t *testing.T) {
	s, st, rec := newTestScheduler(t)

	due := reminder.Reminder{ID: "due-1", Text: "due", DueAt: time.Now().Add(-time.Minute)}
	future := reminder.Reminder{ID: "future-1", Text: "not yet", DueAt: time.Now().Add(time.Hour)}
	for _, r := range []reminder.Reminder{due, future} {
		if err := st.SaveReminder(r); err != nil {
			t.Fatalf("SaveReminder() error = %v", err)
		}
	}

	s.tick(context.Background())

	calls := rec.recorded()
	if len(calls) != 1 {
		t.Fatalf("notify called %d times, want 1; calls: %+v", len(calls), calls)
	}

	if calls[0].r.ID != "due-1" {
		t.Errorf("notified reminder ID = %q, want %q", calls[0].r.ID, "due-1")
	}

	pending := st.ListPendingReminders()
	if len(pending) != 1 || pending[0].ID != "future-1" {
		t.Errorf("pending after tick = %+v, want only future-1", pending)
	}

	archive, err := st.ListArchive()
	if err != nil {
		t.Fatalf("ListArchive() error = %v", err)
	}

	if len(archive) != 1 || archive[0].ID != "due-1" {
		t.Fatalf("archive after tick = %+v, want only due-1", archive)
	}

	if archive[0].FiredAt.IsZero() {
		t.Error("archived reminder has zero FiredAt, want the tick's timestamp")
	}

	if got := archive[0].NtfyMessageIDs["topic-a"]; got != "ntfy-id-due-1" {
		t.Errorf("archived NtfyMessageIDs[topic-a] = %q, want %q (ids returned by notify must reach the archived reminder)", got, "ntfy-id-due-1")
	}
}

func TestTick_LateFlag(t *testing.T) {
	s, st, rec := newTestScheduler(t)

	s.startedAt = time.Now().Add(-time.Hour)

	missed := reminder.Reminder{ID: "missed", DueAt: time.Now().Add(-2 * time.Hour)}
	onTime := reminder.Reminder{ID: "on-time", DueAt: time.Now().Add(-30 * time.Minute)}
	for _, r := range []reminder.Reminder{missed, onTime} {
		if err := st.SaveReminder(r); err != nil {
			t.Fatalf("SaveReminder() error = %v", err)
		}
	}

	s.tick(context.Background())

	calls := rec.recorded()
	if len(calls) != 2 {
		t.Fatalf("notify called %d times, want 2", len(calls))
	}

	lateByID := map[string]bool{}
	for _, c := range calls {
		lateByID[c.r.ID] = c.late
	}

	if !lateByID["missed"] {
		t.Error("reminder due before startedAt was notified with late=false, want true")
	}

	if lateByID["on-time"] {
		t.Error("reminder due after startedAt was notified with late=true, want false")
	}
}

func TestTick_NotifyFailureKeepsReminderPending(t *testing.T) {
	s, st, rec := newTestScheduler(t)

	r := reminder.Reminder{ID: "due-1", DueAt: time.Now().Add(-time.Minute)}
	if err := st.SaveReminder(r); err != nil {
		t.Fatalf("SaveReminder() error = %v", err)
	}

	rec.err = errors.New("ntfy unreachable")
	s.tick(context.Background())

	if pending := st.ListPendingReminders(); len(pending) != 1 {
		t.Fatalf("pending after failed tick = %d reminders, want 1 (must not be dropped or archived)", len(pending))
	}

	if archive, _ := st.ListArchive(); len(archive) != 0 {
		t.Fatalf("archive after failed tick = %d entries, want 0", len(archive))
	}

	rec.err = nil
	s.tick(context.Background())

	calls := rec.recorded()
	if len(calls) != 1 || calls[0].r.ID != "due-1" {
		t.Fatalf("after recovery, notify calls = %+v, want exactly one for due-1", calls)
	}

	if pending := st.ListPendingReminders(); len(pending) != 0 {
		t.Errorf("pending after recovery tick = %d reminders, want 0", len(pending))
	}
}

func TestRun_FiresMissedRemindersOnStartup(t *testing.T) {
	s, st, rec := newTestScheduler(t)

	r := reminder.Reminder{ID: "missed", DueAt: time.Now().Add(-time.Hour)}
	if err := st.SaveReminder(r); err != nil {
		t.Fatalf("SaveReminder() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// No synchronous call for s.Run(ctx) because:
	// Safeguard against future refactorings: if Run ever stops returning on a
	// cancelled context (e.g. the ctx.Done() case gets dropped), this fails in
	// 5s instead of hanging the whole suite.
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on a cancelled context")
	}

	calls := rec.recorded()
	if len(calls) != 1 || calls[0].r.ID != "missed" {
		t.Fatalf("startup tick notify calls = %+v, want exactly one for the missed reminder", calls)
	}
	if !calls[0].late {
		t.Error("reminder missed before startup was notified with late=false, want true")
	}
}
