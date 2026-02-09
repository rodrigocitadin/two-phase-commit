package store

import (
	"encoding/gob"
	"fmt"
	"github.com/google/uuid"
	"os"
	"sync"
)

type StableStore interface {
	WriteStarted(txID uuid.UUID, value int) error
	WritePrepared(txID uuid.UUID, value int) error
	WriteCommited(txID uuid.UUID, value int) error
	WriteAborted(txID uuid.UUID, value int) error
}

type stableStore struct {
	mu      sync.Mutex
	nodeID  int
	file    *os.File
	encoder *gob.Encoder
}

func (s *stableStore) WriteStarted(txID uuid.UUID, value int) error {
	return s.writeLog(Entry{
		TxID:  txID,
		Value: value,
		State: STARTED,
	})
}

func (s *stableStore) WriteAborted(txID uuid.UUID, value int) error {
	return s.writeLog(Entry{
		TxID:  txID,
		Value: value,
		State: ABORTED,
	})
}

func (s *stableStore) WriteCommited(txID uuid.UUID, value int) error {
	return s.writeLog(Entry{
		TxID:  txID,
		Value: value,
		State: COMMITED,
	})
}

func (s *stableStore) WritePrepared(txID uuid.UUID, value int) error {
	return s.writeLog(Entry{
		TxID:  txID,
		Value: value,
		State: PREPARED,
	})
}

func (s *stableStore) writeLog(entry Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.encoder.Encode(entry); err != nil {
		return err
	}

	return s.file.Sync()
}

func (s *stableStore) Close() error {
	return s.file.Close()
}

func NewStableStore(nodeID int) (StableStore, error) {
	dir := "./logs"

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	filename := fmt.Sprintf("%s/node_%d.wal", dir, nodeID)

	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	return &stableStore{
		file:    f,
		encoder: gob.NewEncoder(f),
		nodeID:  nodeID,
	}, nil
}
