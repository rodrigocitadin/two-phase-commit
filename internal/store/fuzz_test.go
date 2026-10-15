package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func validLog(n int) []byte {
	var data []byte
	for i := range n {
		data = append(data, Entry{TxID: uuid.New(), State: TRANSACTION_COMMITTED, SenderID: i, Value: i}.encode()...)
	}
	return data
}

func FuzzEntryRoundTrip(f *testing.F) {
	f.Add([]byte("0123456789abcdef"), uint8(TRANSACTION_PREPARED), int64(3), int64(-7))
	f.Add([]byte{}, uint8(0), int64(-1<<63), int64(1<<63-1))

	f.Fuzz(func(t *testing.T, id []byte, state uint8, sender, value int64) {
		var txID uuid.UUID
		copy(txID[:], id)
		e := Entry{TxID: txID, State: TransactionState(state), SenderID: int(sender), Value: int(value)}

		got, ok := decodeEntry(e.encode())
		if !ok || got != e {
			t.Fatalf("decode(encode(%+v)) = %+v, %v", e, got, ok)
		}
	})
}

// Whatever is on disk, ReplayLog never panics, and when it succeeds the log
// is left aligned so the next append is replayed after the others.
func FuzzReplayLog(f *testing.F) {
	f.Add([]byte{})
	f.Add(validLog(3))
	f.Add(append(validLog(2), 1, 2, 3))
	f.Add(make([]byte, recordSize*2))

	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "node_1.wal"), data, 0644); err != nil {
			t.Fatal(err)
		}
		s, err := NewStableStore(dir, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		count := func() (int, error) {
			n := 0
			err := s.ReplayLog(func(Entry) error { n++; return nil })
			return n, err
		}

		before, err := count()
		if err != nil {
			return // corruption reported, that is fine
		}
		if err := s.Append(Entry{TxID: uuid.New(), State: TRANSACTION_ABORTED}); err != nil {
			t.Fatal(err)
		}
		after, err := count()
		if err != nil || after != before+1 {
			t.Fatalf("Replayed %d entries after append (err %v), want %d", after, err, before+1)
		}
	})
}
