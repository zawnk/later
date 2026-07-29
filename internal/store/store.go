package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zawnk/later/internal/reminder"
)

const stateFileMode os.FileMode = 0600

type Store struct {
	mu          sync.Mutex
	pending     []reminder.Reminder
	pendingPath string
	archivePath string
}

func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	s := &Store{
		pendingPath: filepath.Join(dataDir, "pending.json"),
		archivePath: filepath.Join(dataDir, "archive.json"),
	}
	if err := s.loadPending(); err != nil {
		return nil, err
	}

	if err := s.ensureArchive(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureArchive() error {
	if _, err := os.Stat(s.archivePath); os.IsNotExist(err) {
		return writeAtomic(s.archivePath, []byte("[]"))
	}
	return nil
}

func (s *Store) loadPending() error {
	data, err := os.ReadFile(s.pendingPath)
	if os.IsNotExist(err) {
		s.pending = []reminder.Reminder{}
		return writeAtomic(s.pendingPath, []byte("[]"))
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &s.pending); err != nil {
		return fmt.Errorf("parse pending %s: %w", s.pendingPath, err)
	}
	return nil
}

func (s *Store) savePending() error {
	data, err := json.MarshalIndent(s.pending, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.pendingPath, data)
}

func (s *Store) SaveReminder(r reminder.Reminder) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, r)
	return s.savePending()
}

func (s *Store) CancelReminder(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.pending {
		if r.ID == id {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return true, s.savePending()
		}
	}
	return false, nil
}

func (s *Store) ListPendingReminders() []reminder.Reminder {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]reminder.Reminder, len(s.pending))
	copy(result, s.pending)
	return result
}

func (s *Store) ArchiveReminder(r reminder.Reminder, firedAt time.Time, ntfyMessageIDs map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// remove from pending
	for i, p := range s.pending {
		if p.ID == r.ID {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			break
		}
	}
	if err := s.savePending(); err != nil {
		return err
	}

	// append to archive
	archived := reminder.ArchivedReminder{
		Reminder:       r,
		FiredAt:        firedAt,
		NtfyMessageIDs: ntfyMessageIDs,
	}
	return s.appendToArchive(archived)
}

func (s *Store) appendToArchive(r reminder.ArchivedReminder) error {
	existing, err := s.loadArchive()
	if err != nil {
		return err
	}
	existing = append(existing, r)
	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.archivePath, data)
}

func (s *Store) loadArchive() ([]reminder.ArchivedReminder, error) {
	data, err := os.ReadFile(s.archivePath)
	if os.IsNotExist(err) {
		return []reminder.ArchivedReminder{}, nil
	}
	if err != nil {
		return nil, err
	}
	var archive []reminder.ArchivedReminder
	if err := json.Unmarshal(data, &archive); err != nil {
		return nil, fmt.Errorf("parse archive %s: %w", s.archivePath, err)
	}
	return archive, nil
}

func (s *Store) ListArchive() ([]reminder.ArchivedReminder, error) {
	return s.loadArchive()
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("error when creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(stateFileMode); err != nil {
		tmp.Close()
		return fmt.Errorf("error when chmod on temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("error when writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("error when syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("error when closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("error when renaming temp file into place: %w", err)
	}
	return nil
}
