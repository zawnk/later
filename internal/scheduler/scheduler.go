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
	notify    func(ctx context.Context, r reminder.Reminder, late bool) (map[string]string, error)
	startedAt time.Time
}

func New(s *store.Store, notify func(ctx context.Context, r reminder.Reminder, late bool) (map[string]string, error)) *Scheduler {
	return &Scheduler{
		store:     s,
		notify:    notify,
		startedAt: time.Now(),
	}
}

func (s *Scheduler) Run(ctx context.Context) {
	// fire any missed reminders on startup
	s.tick(ctx)

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutdown signal received - scheduler stopped")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now()
	for _, r := range s.store.ListPendingReminders() {
		if !r.DueAt.After(now) {
			late := r.DueAt.Before(s.startedAt)
			ntfyIDs, err := s.notify(ctx, r, late)
			if err != nil {
				slog.Error("failed to send notification", "id", r.ID, "err", err)
				continue
			}
			if err := s.store.ArchiveReminder(r, now, ntfyIDs); err != nil {
				slog.Error("failed to archive reminder", "id", r.ID, "err", err)
			}
		}
	}
}
