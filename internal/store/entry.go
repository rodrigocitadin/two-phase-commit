package store

import "github.com/google/uuid"

type TransactionState string

const (
	STARTED  TransactionState = "STARTED"
	PREPARED TransactionState = "PREPARED"
	COMMITED TransactionState = "COMMITED"
	ABORTED  TransactionState = "ABORTED"
)

type Entry struct {
	TxID  uuid.UUID
	State TransactionState
	Value int
}
