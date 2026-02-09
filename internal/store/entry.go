package store

import "github.com/google/uuid"

type TransactionState uint8

const (
	STARTED   TransactionState = 1
	PREPARED  TransactionState = 2
	COMMITTED TransactionState = 3
	ABORTED   TransactionState = 4
)

type entry struct {
	TxID  uuid.UUID
	State TransactionState
	Value int
}
