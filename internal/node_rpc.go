package internal

import (
	"github.com/google/uuid"
	"github.com/rodrigocitadin/two-phase-commit/internal/store"
)

type RequestArgs struct {
	TxID     uuid.UUID
	Value    int
	SenderID int
}

type NodeRPC interface {
	Abort(args RequestArgs, reply *bool) error
	Prepare(args RequestArgs, reply *bool) error
	Commit(args RequestArgs, reply *bool) error
	GetStatus(txID uuid.UUID, reply *store.TransactionState) error
}

type nodeRPC struct {
	node *node
}

// GetStatus answers PREPARED while the coordinator has not decided yet, so
// the caller must keep waiting.
func (r *nodeRPC) GetStatus(txID uuid.UUID, reply *store.TransactionState) error {
	*reply = r.node.getStatus(txID)
	return nil
}

func (r *nodeRPC) Abort(args RequestArgs, reply *bool) error {
	err := r.node.decide(args.TxID, args.SenderID, store.TRANSACTION_ABORTED)
	*reply = err == nil
	return err
}

func (r *nodeRPC) Prepare(args RequestArgs, reply *bool) error {
	_, err := r.node.prepare(args.TxID, args.SenderID, func(int) int { return args.Value })
	if err != nil {
		*reply = false
		return err
	}

	// We voted YES: from now on only the coordinator can decide, so keep
	// asking it in case the decision never arrives.
	r.node.goBackground(func() { r.node.awaitDecision(args.TxID, args.SenderID) })
	*reply = true
	return nil
}

func (r *nodeRPC) Commit(args RequestArgs, reply *bool) error {
	err := r.node.decide(args.TxID, args.SenderID, store.TRANSACTION_COMMITTED)
	*reply = err == nil
	return err
}
