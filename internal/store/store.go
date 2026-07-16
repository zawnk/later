package store

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/zawnk/later/internal/reminder"
)

type Store struct {
	mu          sync.Mutex
	pending     []reminder.Reminder
	pendingPath string
	archivePath string
}

func New(dataDir string) (*Store, error) {
	s := &Store{
		pendingPath: dataDir + "/pending.json",
		archivePath: dataDir + "/archive.json",
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
		return os.WriteFile(s.archivePath, []byte("[]"), 0644)
	}
	return nil
}

func (s *Store) loadPending() error {
	data, err := os.ReadFile(s.pendingPath)
	if os.IsNotExist(err) {
		s.pending = []reminder.Reminder{}
		return os.WriteFile(s.pendingPath, []byte("[]"), 0644)
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &s.pending)
}

func (s *Store) savePending() error {
	data, err := json.MarshalIndent(s.pending, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.pendingPath, data, 0644)
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

func (s *Store) ArchiveReminder(r reminder.Reminder, firedAt time.Time) error {
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
		Reminder: r,
		FiredAt:  firedAt,
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
	return os.WriteFile(s.archivePath, data, 0644)
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
	return archive, json.Unmarshal(data, &archive)
}

func (s *Store) ListArchive() ([]reminder.ArchivedReminder, error) {
	return s.loadArchive()
}
