package store

import (
	"errors"
	"sync"

	"github.com/google/uuid"
)

type VolatileStore interface {
	Prepare(txID uuid.UUID, newState int) error
	Commit(txID uuid.UUID) error
	Abort(txID uuid.UUID) error
	State() int
}

type volatileStore struct {
	mu            sync.RWMutex
	locked        bool
	lockedByTx    *uuid.UUID
	state         int
	proposedValue int
}

func (vs *volatileStore) State() int {
	return vs.state
}

func (vs *volatileStore) Prepare(txID uuid.UUID, newState int) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if vs.locked {
		if vs.lockedByTx == &txID {
			return nil
		}
		return errors.New("node is locked by another transaction")
	}

	vs.locked = true
	vs.lockedByTx = &txID
	vs.proposedValue = newState

	return nil
}

func (vs *volatileStore) Commit(txID uuid.UUID) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if !vs.locked || vs.lockedByTx != &txID {
		return errors.New("invalid transaction commit")
	}

	vs.state = vs.proposedValue
	vs.locked = false
	vs.lockedByTx = nil

	return nil
}

func (vs *volatileStore) Abort(txID uuid.UUID) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if !vs.locked || vs.lockedByTx != &txID {
		return nil
	}

	vs.locked = false
	vs.lockedByTx = nil

	return nil
}

func NewVolatileStore(state int) VolatileStore {
	return &volatileStore{state: state}
}
