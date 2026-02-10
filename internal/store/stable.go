package store

import (
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/google/uuid"
)

type StableStore interface {
	WritePrepared(txID uuid.UUID, value, senderID int) error
	WriteCommited(txID uuid.UUID, value, senderID int) error
	WriteAborted(txID uuid.UUID, senderID int) error
	SaveSnapshot(state int, history map[uuid.UUID]bool) error
	LoadSnapshot() (*SnapshotData, error)
	RecoverLastState() (*Entry, error)
	Truncate() error
	GetTransactionState(txID uuid.UUID) (TransactionState, error)
	ReplayLog(callback func(Entry) error) error
	Close() error
}

type SnapshotData struct {
	State        int
	CommittedLog map[uuid.UUID]bool
}

type stableStore struct {
	mu      sync.Mutex
	nodeID  int
	file    *os.File
	encoder *gob.Encoder
}

func (s *stableStore) ReplayLog(callback func(Entry) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.file.Seek(0, 0)
	if err != nil {
		return err
	}

	decoder := gob.NewDecoder(s.file)
	for {
		var e Entry
		if err := decoder.Decode(&e); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if err := callback(e); err != nil {
			return err
		}
	}
	return nil
}

func (s *stableStore) GetTransactionState(txID uuid.UUID) (TransactionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	filename := fmt.Sprintf("./logs/node_%d.wal", s.nodeID)
	f, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return TRANSACTION_ABORTED, nil
		}
		return 0, err
	}
	defer f.Close()

	decoder := gob.NewDecoder(f)
	// Default to Aborted (Presumed Abort) if not found
	finalState := TRANSACTION_ABORTED

	for {
		var e Entry
		if err := decoder.Decode(&e); err != nil {
			if err == io.EOF {
				break
			}
			return 0, err
		}

		if e.TxID == txID {
			if e.State == TRANSACTION_COMMITTED {
				finalState = TRANSACTION_COMMITTED
			}
		}
	}

	return finalState, nil
}

func (s *stableStore) SaveSnapshot(state int, history map[uuid.UUID]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	filename := fmt.Sprintf("logs/snaps/node_%d.snap", s.nodeID)
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	data := SnapshotData{
		State:        state,
		CommittedLog: history,
	}

	return gob.NewEncoder(f).Encode(data)
}

func (s *stableStore) LoadSnapshot() (*SnapshotData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	filename := fmt.Sprintf("logs/snaps/node_%d.snap", s.nodeID)
	f, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var data SnapshotData
	if err := gob.NewDecoder(f).Decode(&data); err != nil {
		return nil, err
	}

	return &data, nil
}

func (s *stableStore) RecoverLastState() (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var lastState Entry
	decoder := gob.NewDecoder(s.file)

	for {
		var current Entry
		if err := decoder.Decode(&current); err != nil {
			if err == io.EOF {
				break
			}
			return &lastState, fmt.Errorf("potential corruption at end of log: %w", err)
		}
		lastState = current
	}

	return &lastState, nil
}

func (s *stableStore) Truncate() error {
	return s.file.Truncate(0)
}

func (s *stableStore) WriteAborted(txID uuid.UUID, senderID int) error {
	return s.writeLog(Entry{
		TxID:  txID,
		State: TRANSACTION_ABORTED,
	})
}

func (s *stableStore) WriteCommited(txID uuid.UUID, value, senderID int) error {
	return s.writeLog(Entry{
		TxID:  txID,
		Value: value,
		State: TRANSACTION_COMMITTED,
	})
}

func (s *stableStore) WritePrepared(txID uuid.UUID, value, senderID int) error {
	return s.writeLog(Entry{
		TxID:  txID,
		Value: value,
		State: TRANSACTION_PREPARED,
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
	if err := os.MkdirAll("./logs", 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	if err := os.MkdirAll("./logs/snaps", 0755); err != nil {
		return nil, fmt.Errorf("failed to create snaps directory: %w", err)
	}

	filename := fmt.Sprintf("./logs/node_%d.wal", nodeID)

	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}

	return &stableStore{
		file:    f,
		encoder: gob.NewEncoder(f),
		nodeID:  nodeID,
	}, nil
}
