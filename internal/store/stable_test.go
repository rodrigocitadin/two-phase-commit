package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func replayAll(t *testing.T, s StableStore) []Entry {
	t.Helper()
	var entries []Entry
	if err := s.ReplayLog(func(e Entry) error {
		entries = append(entries, e)
		return nil
	}); err != nil {
		t.Fatalf("ReplayLog: %v", err)
	}
	return entries
}

func writeEntries(t *testing.T, dir string, n int) []Entry {
	t.Helper()
	s, err := NewStableStore(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var entries []Entry
	for i := range n {
		e := Entry{TxID: uuid.New(), State: TRANSACTION_PREPARED, SenderID: i, Value: -i}
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	return entries
}

func TestWAL_ReplayAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	first := writeEntries(t, dir, 2)
	second := writeEntries(t, dir, 2)

	s, _ := NewStableStore(dir, 1)
	defer s.Close()
	got := replayAll(t, s)
	want := append(first, second...)
	if len(got) != len(want) {
		t.Fatalf("Replayed %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestWAL_TornTailIsDropped(t *testing.T) {
	dir := t.TempDir()
	writeEntries(t, dir, 3)

	// Simulate a crash in the middle of a write.
	f, _ := os.OpenFile(filepath.Join(dir, "node_1.wal"), os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{1, 2, 3, 4, 5})
	f.Close()

	s, _ := NewStableStore(dir, 1)
	defer s.Close()
	if got := len(replayAll(t, s)); got != 3 {
		t.Fatalf("Replayed %d entries, want 3", got)
	}

	// New entries are appended after the last valid record.
	if err := s.Append(Entry{TxID: uuid.New(), State: TRANSACTION_ABORTED}); err != nil {
		t.Fatal(err)
	}
	if got := len(replayAll(t, s)); got != 4 {
		t.Fatalf("Replayed %d entries after append, want 4", got)
	}
}

func TestWAL_CorruptionInTheMiddleIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeEntries(t, dir, 3)

	path := filepath.Join(dir, "node_1.wal")
	data, _ := os.ReadFile(path)
	data[recordSize+10] ^= 0xff
	os.WriteFile(path, data, 0644)

	s, _ := NewStableStore(dir, 1)
	defer s.Close()
	if err := s.ReplayLog(func(Entry) error { return nil }); err == nil {
		t.Fatal("Expected corruption error")
	}
}

func TestSnapshot_RoundTrip(t *testing.T) {
	s, _ := NewStableStore(t.TempDir(), 1)
	defer s.Close()

	if snap, err := s.LoadSnapshot(); err != nil || snap != nil {
		t.Fatalf("Expected no snapshot, got %v, %v", snap, err)
	}

	txID := uuid.New()
	want := Snapshot{State: 7, Transactions: map[uuid.UUID]TxRecord{
		txID: {State: TRANSACTION_PREPARED, SenderID: 3, Value: 7},
	}}
	if err := s.SaveSnapshot(want); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got.State != want.State || got.Transactions[txID] != want.Transactions[txID] {
		t.Fatalf("Snapshot = %+v, want %+v", got, want)
	}
}
