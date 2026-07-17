package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/zawnk/later/internal/reminder"
	"github.com/zawnk/later/internal/store"
)

type Scheduler struct {
	store     *store.Store
	notify    func(r reminder.Reminder, late bool) error
	startedAt time.Time
}

func New(s *store.Store, notify func(r reminder.Reminder, late bool) error) *Scheduler {
	return &Scheduler{
		store:     s,
		notify:    notify,
		startedAt: time.Now(),
	}
}

func (s *Scheduler) Run(ctx context.Context) {
	// fire any missed reminders on startup
	s.tick()

	// TODO: is one minute fine?
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

func (s *Scheduler) tick() {
	now := time.Now()
	for _, r := range s.store.ListPendingReminders() {
		if r.DueAt.Before(now) || r.DueAt.Equal(now) {
			late := r.DueAt.Before(s.startedAt)
			if err := s.notify(r, late); err != nil {
				slog.Error("failed to send notification", "id", r.ID, "err", err)
				continue
			}
			if err := s.store.ArchiveReminder(r, now); err != nil {
				// TODO: if archiving fails, reminder will fire again on next tick
				slog.Error("failed to archive reminder", "id", r.ID, "err", err)
			}
		}
	}
}
