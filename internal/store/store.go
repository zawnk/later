package store

import (
	"bytes"
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
	mu sync.Mutex
	// stubsMu serializes *writers* to stubs.json against each other, and
	// does nothing else. Reads deliberately never take it: writeAtomic
	// renames the finished file into place and rename is atomic, so a
	// reader always sees either the whole old file or the whole new one,
	// and loadStubs builds a fresh map per call, so two readers share no
	// memory either. Contrast mu, which guards the long-lived pending
	// slice and therefore has to be held by readers as well.
	stubsMu     sync.Mutex
	pending     []reminder.Reminder
	pendingPath string
	archivePath string
	stubsPath   string
}

func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	s := &Store{
		pendingPath: filepath.Join(dataDir, "pending.json"),
		archivePath: filepath.Join(dataDir, "archive.json"),
		stubsPath:   filepath.Join(dataDir, "stubs.json"),
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

// LoadStubs reads the stub definitions from stubs.json. It reads from
// disk on every call and keeps no cached copy, so hand-editing the file
// takes effect on the next lookup with no restart, signal or watcher.
// The file is never created merely by reading it.
//
// This is the entry point for callers that do not hold stubsMu, which
// is everyone outside this file. See loadStubs for the other half.
func (s *Store) LoadStubs() (map[string]reminder.Stub, error) {
	return s.loadStubs()
}

// loadStubs is LoadStubs' body without the lock, for callers that are
// already inside stubsMu.
//
// The rule, in one line: holding stubsMu, call loadStubs; not holding
// it, call LoadStubs. Today that is SetStub and DeleteStub for the
// former and everything else for the latter, and a future writer
// (RenameStub, say) belongs in the former.
//
// Why bother, when the two are identical today: Because LoadStubs
// currently does not lock, so the split buys nothing at runtime - which
// makes it tempting to collapse, or to "tidy up" by having LoadStubs
// take stubsMu for consistency with its siblings. sync.Mutex is not
// reentrant: a write that already holds the lock and then calls the
// exported method would block on itself forever, with no error, no
// stack trace and no failing test - just a request that never returns.
// Two entry points is what keeps that edit safe to make. Same split as
// loadPending/loadArchive.
func (s *Store) loadStubs() (map[string]reminder.Stub, error) {
	data, err := os.ReadFile(s.stubsPath)
	if os.IsNotExist(err) {
		return map[string]reminder.Stub{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read stubs %s: %w", s.stubsPath, err)
	}
	stubs := map[string]reminder.Stub{}

	if len(bytes.TrimSpace(data)) == 0 {
		return stubs, nil
	}
	if err := json.Unmarshal(data, &stubs); err != nil {
		return nil, fmt.Errorf("parse stubs %s: %w", s.stubsPath, err)
	}
	return stubs, nil
}

// SetStub stores stub under name in stubs.json, creating the file if it
// is absent and replacing any definition already held under that name.
//
// The lock spans the whole read-modify-write, not just the write: each
// write rewrites the entire map, so a lock held only around writeAtomic
// would let two concurrent writes to different names read the same
// map and the later one drop the earlier one's stub. It is a lock of
// its own rather than the pending mutex because the two guard
// unrelated files.
func (s *Store) SetStub(name string, stub reminder.Stub) error {
	s.stubsMu.Lock()
	defer s.stubsMu.Unlock()

	stubs, err := s.loadStubs()
	if err != nil {
		return err
	}
	stubs[name] = stub
	return s.saveStubs(stubs)
}

// DeleteStub removes name from stubs.json, reporting whether it was
// there to begin with. Like SetStub it holds the lock across the whole
// read-modify-write.
func (s *Store) DeleteStub(name string) (bool, error) {
	s.stubsMu.Lock()
	defer s.stubsMu.Unlock()

	stubs, err := s.loadStubs()
	if err != nil {
		return false, err
	}
	if _, ok := stubs[name]; !ok {
		return false, nil
	}
	delete(stubs, name)
	return true, s.saveStubs(stubs)
}

func (s *Store) saveStubs(stubs map[string]reminder.Stub) error {
	data, err := json.MarshalIndent(stubs, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.stubsPath, data)
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
