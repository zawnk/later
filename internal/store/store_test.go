package store

import (
	"os"
	"path/filepath"
	"strings"
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
	ntfyIDs := map[string]string{"topic-a": "ntfy-id-1"}
	if err := s.ArchiveReminder(r, firedAt, ntfyIDs); err != nil {
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

	if got := archive[0].NtfyMessageIDs["topic-a"]; got != "ntfy-id-1" {
		t.Errorf("archived NtfyMessageIDs[topic-a] = %q, want %q", got, "ntfy-id-1")
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

func TestNew_CorruptedPendingFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(dir); err != nil {
		t.Fatalf("New() error = %v", err)
	}

	pendingPath := filepath.Join(dir, "pending.json")
	if err := os.WriteFile(pendingPath, []byte("{not valid json"), 0600); err != nil {
		t.Fatalf("failed to corrupt pending file: %v", err)
	}

	_, err := New(dir)
	if err == nil {
		t.Fatal("New() error = nil, want an error for corrupted pending.json")
	}
	if !strings.Contains(err.Error(), pendingPath) {
		t.Errorf("New() error = %q, want it to mention the file path %q", err.Error(), pendingPath)
	}
}

func TestLoadStubs_ReadsDefinitions(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	contents := `{
  "hockey": {"text": "in 15m back to the game", "tags": ["hockey"], "priority": "high", "click": "https://example.com/game"},
  "laundry": {"text": "in 45m move the laundry"}
}`
	if err := os.WriteFile(filepath.Join(dir, "stubs.json"), []byte(contents), 0600); err != nil {
		t.Fatalf("failed to write stubs file: %v", err)
	}

	stubs, err := s.LoadStubs()
	if err != nil {
		t.Fatalf("LoadStubs() error = %v", err)
	}

	if len(stubs) != 2 {
		t.Fatalf("LoadStubs() returned %d stubs, want 2", len(stubs))
	}

	hockey := stubs["hockey"]
	if hockey.Text != "in 15m back to the game" {
		t.Errorf("hockey text = %q, want %q", hockey.Text, "in 15m back to the game")
	}
	if len(hockey.Tags) != 1 || hockey.Tags[0] != "hockey" {
		t.Errorf("hockey tags = %v, want [hockey]", hockey.Tags)
	}
	if hockey.Priority != "high" {
		t.Errorf("hockey priority = %q, want %q", hockey.Priority, "high")
	}
	if hockey.Click != "https://example.com/game" {
		t.Errorf("hockey click = %q, want %q", hockey.Click, "https://example.com/game")
	}

	if stubs["laundry"].Text != "in 45m move the laundry" {
		t.Errorf("laundry text = %q, want %q", stubs["laundry"].Text, "in 45m move the laundry")
	}
}

func TestLoadStubs_MissingFile(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stubs, err := s.LoadStubs()
	if err != nil {
		t.Fatalf("LoadStubs() error = %v, want no error for a missing file", err)
	}
	if len(stubs) != 0 {
		t.Errorf("LoadStubs() returned %d stubs, want 0", len(stubs))
	}

	stubsPath := filepath.Join(dir, "stubs.json")
	if _, err := os.Stat(stubsPath); !os.IsNotExist(err) {
		t.Errorf("reading created %q; the file must not be created merely by reading", stubsPath)
	}
}

func TestLoadStubs_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "stubs.json"), nil, 0600); err != nil {
		t.Fatalf("failed to write empty stubs file: %v", err)
	}

	stubs, err := s.LoadStubs()
	if err != nil {
		t.Fatalf("LoadStubs() error = %v, want no error for an empty file", err)
	}
	if len(stubs) != 0 {
		t.Errorf("LoadStubs() returned %d stubs, want 0", len(stubs))
	}
}

func TestLoadStubs_MalformedFile(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stubsPath := filepath.Join(dir, "stubs.json")
	if err := os.WriteFile(stubsPath, []byte("{not valid json"), 0600); err != nil {
		t.Fatalf("failed to corrupt stubs file: %v", err)
	}

	stubs, err := s.LoadStubs()
	if err == nil {
		t.Fatal("LoadStubs() error = nil, want an error for malformed JSON")
	}
	if !strings.Contains(err.Error(), stubsPath) {
		t.Errorf("LoadStubs() error = %q, want it to name the file path %q", err.Error(), stubsPath)
	}
	if stubs != nil {
		t.Errorf("LoadStubs() = %v, want nil map alongside the error", stubs)
	}
}

func TestLoadStubs_RereadsOnEveryCall(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stubsPath := filepath.Join(dir, "stubs.json")
	if err := os.WriteFile(stubsPath, []byte(`{"hockey": {"text": "in 15m back to the game"}}`), 0600); err != nil {
		t.Fatalf("failed to write stubs file: %v", err)
	}
	if _, err := s.LoadStubs(); err != nil {
		t.Fatalf("LoadStubs() error = %v", err)
	}

	if err := os.WriteFile(stubsPath, []byte(`{"laundry": {"text": "in 45m move the laundry"}}`), 0600); err != nil {
		t.Fatalf("failed to rewrite stubs file: %v", err)
	}

	stubs, err := s.LoadStubs()
	if err != nil {
		t.Fatalf("LoadStubs() (second call) error = %v", err)
	}
	if _, ok := stubs["hockey"]; ok {
		t.Error("LoadStubs() still returned the edited-away stub; it must re-read the file on every call")
	}
	if stubs["laundry"].Text != "in 45m move the laundry" {
		t.Errorf("laundry text = %q, want the hand-edited value", stubs["laundry"].Text)
	}
}
