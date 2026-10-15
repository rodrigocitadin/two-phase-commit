package store

import (
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
)

type StableStore interface {
	// Append durably writes e to the WAL (fsync before returning).
	Append(e Entry) error
	// ReplayLog calls callback for every entry in the WAL, in order. A torn
	// record at the end of the log (crash in the middle of a write) is dropped.
	ReplayLog(callback func(Entry) error) error
	// SaveSnapshot atomically replaces the snapshot.
	SaveSnapshot(s Snapshot) error
	LoadSnapshot() (*Snapshot, error)
	// Truncate empties the WAL. Only call it after a snapshot that covers it.
	Truncate() error
	Close() error
}

type Snapshot struct {
	State        int
	Transactions map[uuid.UUID]TxRecord
}

type stableStore struct {
	mu       sync.Mutex
	file     *os.File
	size     int64
	walPath  string
	snapPath string
}

func (s *stableStore) Append(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.file.Write(e.encode()); err != nil {
		// Drop a partially written record so the log stays aligned.
		_ = s.file.Truncate(s.size)
		return err
	}
	if err := s.file.Sync(); err != nil {
		_ = s.file.Truncate(s.size)
		return err
	}

	s.size += recordSize
	return nil
}

func (s *stableStore) ReplayLog(callback func(Entry) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := io.ReadAll(io.NewSectionReader(s.file, 0, s.size))
	if err != nil {
		return err
	}

	records := len(data) / recordSize
	valid := 0
	for i := range records {
		e, ok := decodeEntry(data[i*recordSize:])
		if !ok {
			if i == records-1 {
				break // torn last record
			}
			return fmt.Errorf("wal corrupted at offset %d", i*recordSize)
		}
		if err := callback(e); err != nil {
			return err
		}
		valid = (i + 1) * recordSize
	}

	if valid < len(data) {
		if err := s.file.Truncate(int64(valid)); err != nil {
			return err
		}
		s.size = int64(valid)
	}
	return nil
}

func (s *stableStore) SaveSnapshot(snap Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Write to a temporary file and rename it over the old snapshot, so a
	// crash leaves either the old or the new snapshot, never a partial one.
	tmp := s.snapPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(snap); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.snapPath); err != nil {
		return err
	}
	return syncDir(filepath.Dir(s.snapPath))
}

func (s *stableStore) LoadSnapshot() (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open(s.snapPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var snap Snapshot
	if err := gob.NewDecoder(f).Decode(&snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *stableStore) Truncate() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.file.Truncate(0); err != nil {
		return err
	}
	s.size = 0
	return s.file.Sync()
}

func (s *stableStore) Close() error {
	return s.file.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func NewStableStore(dir string, nodeID int) (StableStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	walPath := filepath.Join(dir, fmt.Sprintf("node_%d.wal", nodeID))
	f, err := os.OpenFile(walPath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	return &stableStore{
		file:     f,
		size:     info.Size(),
		walPath:  walPath,
		snapPath: filepath.Join(dir, fmt.Sprintf("node_%d.snap", nodeID)),
	}, nil
}
