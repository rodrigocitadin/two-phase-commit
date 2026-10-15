package store

import (
	"errors"
	"sync"

	"github.com/google/uuid"
)

// VolatileStore holds the in-memory value and the lock taken during the
// prepare phase. It knows nothing about durability: callers log to the
// StableStore before changing it.
type VolatileStore interface {
	// Prepare locks the store for txID and stages the value returned by
	// propose, which receives the committed state while the lock is held.
	Prepare(txID uuid.UUID, propose func(current int) int) (int, error)
	Commit(txID uuid.UUID) error
	Abort(txID uuid.UUID)
	Recover(state int)
	State() int
}

type volatileStore struct {
	mu            sync.RWMutex
	locked        bool
	lockedByTx    uuid.UUID
	state         int
	proposedValue int
}

func (vs *volatileStore) Recover(state int) {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	vs.state = state
	vs.locked = false
	vs.lockedByTx = uuid.Nil
}

func (vs *volatileStore) State() int {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.state
}

func (vs *volatileStore) Prepare(txID uuid.UUID, propose func(current int) int) (int, error) {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if vs.locked {
		if vs.lockedByTx == txID {
			return vs.proposedValue, nil
		}
		return 0, errors.New("node is locked by another transaction")
	}

	vs.locked = true
	vs.lockedByTx = txID
	vs.proposedValue = propose(vs.state)

	return vs.proposedValue, nil
}

func (vs *volatileStore) Commit(txID uuid.UUID) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if !vs.locked || vs.lockedByTx != txID {
		return errors.New("invalid transaction commit")
	}

	vs.state = vs.proposedValue
	vs.locked = false
	vs.lockedByTx = uuid.Nil

	return nil
}

func (vs *volatileStore) Abort(txID uuid.UUID) {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if !vs.locked || vs.lockedByTx != txID {
		return
	}

	vs.locked = false
	vs.lockedByTx = uuid.Nil
	vs.proposedValue = vs.state
}

func NewVolatileStore(state int) VolatileStore {
	return &volatileStore{state: state}
}
