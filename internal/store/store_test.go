package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zawnk/later/internal/reminder"
)

func TestSaveAndListPending(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	r := reminder.Reminder{ID: "abc123", Text: "buy milk", DueAt: time.Now()}
	if err := s.SaveReminder(r); err != nil {
		t.Fatalf("SaveReminder() error = %v", err)
	}

	pending := s.ListPendingReminders()
	if len(pending) != 1 {
		t.Fatalf("ListPendingReminders() returned %d reminders, want 1", len(pending))
	}

	if pending[0].ID != r.ID {
		t.Errorf("got ID %q, want %q", pending[0].ID, r.ID)
	}
}

func TestPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	s1, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	r := reminder.Reminder{ID: "abc123", Text: "buy milk", DueAt: time.Now()}
	if err := s1.SaveReminder(r); err != nil {
		t.Fatalf("SaveReminder() error = %v", err)
	}

	s2, err := New(dir)
	if err != nil {
		t.Fatalf("New() (second open) error = %v", err)
	}

	pending := s2.ListPendingReminders()
	if len(pending) != 1 {
		t.Fatalf("after restart: got %d pending reminders, want 1", len(pending))
	}

	if pending[0].ID != r.ID {
		t.Errorf("after restart: got ID %q, want %q", pending[0].ID, r.ID)
	}
}

func TestCancelReminder(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	r := reminder.Reminder{ID: "abc123", Text: "buy milk", DueAt: time.Now()}
	if err := s.SaveReminder(r); err != nil {
		t.Fatalf("SaveReminder() error = %v", err)
	}

	found, err := s.CancelReminder("abc123")
	if err != nil {
		t.Fatalf("CancelReminder() error = %v", err)
	}

	if !found {
		t.Errorf("CancelReminder() found = false, want true")
	}

	if len(s.ListPendingReminders()) != 0 {
		t.Errorf("expected no pending reminders after cancel, got %d", len(s.ListPendingReminders()))
	}
}

func TestCancelReminder_NotFound(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	found, err := s.CancelReminder("does-not-exist")
	if err != nil {
		t.Fatalf("CancelReminder() error = %v, want nil", err)
	}

	if found {
		t.Errorf("CancelReminder() found = true, want false for an unknown ID")
	}
}

func TestArchiveReminder(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	r := reminder.Reminder{ID: "abc123", Text: "buy milk", DueAt: time.Now()}
	if err := s.SaveReminder(r); err != nil {
		t.Fatalf("SaveReminder() error = %v", err)
	}

	firedAt := time.Now()
	if err := s.ArchiveReminder(r, firedAt); err != nil {
		t.Fatalf("ArchiveReminder() error = %v", err)
	}

	if pending := s.ListPendingReminders(); len(pending) != 0 {
		t.Errorf("expected 0 pending reminders after archiving, got %d", len(pending))
	}

	archive, err := s.ListArchive()
	if err != nil {
		t.Fatalf("ListArchive() error = %v", err)
	}

	if len(archive) != 1 {
		t.Fatalf("expected 1 archived reminder, got %d", len(archive))
	}

	if archive[0].ID != r.ID {
		t.Errorf("archived ID = %q, want %q", archive[0].ID, r.ID)
	}

	if !archive[0].FiredAt.Equal(firedAt) {
		t.Errorf("archived FiredAt = %v, want %v", archive[0].FiredAt, firedAt)
	}
}

func TestNewCreatesDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("test setup broken: %q already exists", dir)
	}

	s, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v, want it to create the missing directory", err)
	}

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("New() did not create %q", dir)
	}

	if err := s.SaveReminder(reminder.Reminder{ID: "abc123"}); err != nil {
		t.Fatalf("SaveReminder() error = %v after New() created the dir", err)
	}
}

func TestLoadArchive_CorruptedFile(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := os.WriteFile(s.archivePath, []byte("{not valid json"), 0600); err != nil {
		t.Fatalf("failed to corrupt archive file: %v", err)
	}

	archive, err := s.ListArchive()
	if err == nil {
		t.Fatal("ListArchive() error = nil, want an error for corrupted JSON")
	}

	if archive != nil {
		t.Errorf("ListArchive() = %v, want nil slice alongside the error", archive)
	}
}
