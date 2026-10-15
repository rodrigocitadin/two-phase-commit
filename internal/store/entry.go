package store

import (
	"encoding/binary"
	"hash/crc32"

	"github.com/google/uuid"
)

type TransactionState uint8

const (
	TRANSACTION_PREPARED  TransactionState = 1
	TRANSACTION_COMMITTED TransactionState = 2
	TRANSACTION_ABORTED   TransactionState = 3
	// TRANSACTION_ENDED is only logged by the coordinator, once every
	// participant acknowledged the decision.
	TRANSACTION_ENDED TransactionState = 4
)

func (s TransactionState) String() string {
	switch s {
	case TRANSACTION_PREPARED:
		return "PREPARED"
	case TRANSACTION_COMMITTED:
		return "COMMITTED"
	case TRANSACTION_ABORTED:
		return "ABORTED"
	case TRANSACTION_ENDED:
		return "ENDED"
	default:
		return "UNKNOWN"
	}
}

type Entry struct {
	TxID     uuid.UUID
	State    TransactionState
	SenderID int
	Value    int
}

// TxRecord is what a node remembers about a transaction.
type TxRecord struct {
	State    TransactionState // PREPARED, COMMITTED or ABORTED
	SenderID int              // coordinator of the transaction
	Value    int
	Ended    bool // coordinator only: every participant acknowledged the decision
}

// On disk every entry is a fixed size record: crc32 | txID | state | senderID | value.
const (
	payloadSize = 16 + 1 + 8 + 8
	recordSize  = 4 + payloadSize
)

func (e Entry) encode() []byte {
	buf := make([]byte, recordSize)
	p := buf[4:]
	copy(p[0:16], e.TxID[:])
	p[16] = byte(e.State)
	binary.LittleEndian.PutUint64(p[17:25], uint64(int64(e.SenderID)))
	binary.LittleEndian.PutUint64(p[25:33], uint64(int64(e.Value)))
	binary.LittleEndian.PutUint32(buf[0:4], crc32.ChecksumIEEE(p))
	return buf
}

func decodeEntry(buf []byte) (Entry, bool) {
	p := buf[4:recordSize]
	if crc32.ChecksumIEEE(p) != binary.LittleEndian.Uint32(buf[0:4]) {
		return Entry{}, false
	}

	var e Entry
	copy(e.TxID[:], p[0:16])
	e.State = TransactionState(p[16])
	e.SenderID = int(int64(binary.LittleEndian.Uint64(p[17:25])))
	e.Value = int(int64(binary.LittleEndian.Uint64(p[25:33])))
	return e, true
}
