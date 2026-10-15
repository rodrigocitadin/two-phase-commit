package internal

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rodrigocitadin/two-phase-commit/internal/store"
)

func TestRPC_Idempotency(t *testing.T) {
	c := newCluster(t, 120, 1)
	n := c.node(120)
	r := &nodeRPC{node: n}

	call := func(method string, args RequestArgs) error {
		var ok bool
		switch method {
		case "prepare":
			return r.Prepare(args, &ok)
		case "commit":
			return r.Commit(args, &ok)
		default:
			return r.Abort(args, &ok)
		}
	}

	tests := []struct {
		name    string
		calls   []string
		wantErr bool // of the last call; all the previous ones must succeed
		want    store.TransactionState
	}{
		{"duplicate prepare", []string{"prepare", "prepare", "commit"}, false, store.TRANSACTION_COMMITTED},
		{"duplicate commit", []string{"prepare", "commit", "commit"}, false, store.TRANSACTION_COMMITTED},
		{"duplicate abort", []string{"prepare", "abort", "abort"}, false, store.TRANSACTION_ABORTED},
		{"commit after abort", []string{"prepare", "abort", "commit"}, true, store.TRANSACTION_ABORTED},
		{"abort after commit", []string{"prepare", "commit", "abort"}, true, store.TRANSACTION_COMMITTED},
		{"commit without prepare", []string{"commit"}, true, 0},
		{"abort without prepare", []string{"abort"}, false, store.TRANSACTION_ABORTED},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// SenderID is not in the cluster, so no background query decides for us.
			args := RequestArgs{TxID: uuid.New(), Value: i + 1, SenderID: 999}
			before := n.State()

			for j, method := range tt.calls {
				err := call(method, args)
				if last := j == len(tt.calls)-1; last && tt.wantErr {
					if err == nil {
						t.Fatalf("%s should fail", method)
					}
				} else if err != nil {
					t.Fatalf("%s #%d failed: %v", method, j, err)
				}
			}

			if got := n.record(args.TxID).State; got != tt.want {
				t.Errorf("State = %s, want %s", got, tt.want)
			}
			wantValue := before
			if tt.want == store.TRANSACTION_COMMITTED {
				wantValue = args.Value
			}
			if n.State() != wantValue {
				t.Errorf("Value = %d, want %d", n.State(), wantValue)
			}
		})
	}
}

func TestSingleNodeCluster(t *testing.T) {
	c := newCluster(t, 220, 1)
	n := c.node(220)

	for i := range 3 {
		if err := n.Transaction(5); err != nil {
			t.Fatal(err)
		}
		if n.State() != 5*(i+1) {
			t.Fatalf("State = %d, want %d", n.State(), 5*(i+1))
		}
	}

	c.crash(220)
	if got := c.restart(220).State(); got != 15 {
		t.Fatalf("State after restart = %d, want 15", got)
	}
}

// Concurrent transactions on the same coordinator: the lock lets them run
// one at a time and none of the successful ones is lost.
func TestConcurrentTransactionsOnSameNode(t *testing.T) {
	c := newCluster(t, 230, 3)
	n := c.node(230)

	var wg sync.WaitGroup
	var committed atomic.Int64
	for range 10 {
		wg.Go(func() {
			if n.Transaction(1) == nil {
				committed.Add(1)
			}
		})
	}
	wg.Wait()

	if committed.Load() == 0 {
		t.Fatal("No transaction committed")
	}
	c.waitForState(int(committed.Load()))
}

// The prepare lock allows a single in-doubt transaction; a log with two means
// it is corrupt and the node must refuse to start rather than guess.
func TestRecovery_RejectsTwoPreparedTransactions(t *testing.T) {
	dir := t.TempDir()
	s, err := store.NewStableStore(dir, 240)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s.Append(store.Entry{TxID: uuid.New(), State: store.TRANSACTION_PREPARED, SenderID: 1, Value: 1})
	}
	s.Close()

	n, err := NewNode(Config{ID: 240, Nodes: map[int]string{240: "localhost:3240"}, DataDir: dir, Logger: testLogger})
	if err == nil {
		n.Close()
		t.Fatal("Node started with two prepared transactions")
	}
}

func TestRecovery_NoRetransmissionAfterEnd(t *testing.T) {
	c := newCluster(t, 250, 2)
	coordinator := c.node(250)

	var txID uuid.UUID
	coordinator.crashHook = func(_ crashPoint, id uuid.UUID) bool { txID = id; return false }
	if err := coordinator.Transaction(1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "coordinator to log END", func() bool { return coordinator.record(txID).Ended })

	c.crash(250)
	sent := c.net.count(250, "Node.Commit")
	if !c.restart(250).record(txID).Ended {
		t.Fatal("END lost across restart")
	}

	time.Sleep(5 * retryInterval)
	if got := c.net.count(250, "Node.Commit"); got != sent {
		t.Errorf("Coordinator re-sent %d commits for an ended transaction", got-sent)
	}
}

func TestClose_StopsBackgroundWork(t *testing.T) {
	c := newCluster(t, 260, 2)
	c.crash(261)

	// The abort cannot be delivered, so the coordinator keeps retrying it.
	if err := c.node(260).Transaction(1); err == nil {
		t.Fatal("Expected transaction to fail")
	}
	waitFor(t, "abort retransmissions", func() bool { return c.net.count(260, "Node.Abort") >= 3 })

	start := time.Now()
	c.crash(260)
	if took := time.Since(start); took > time.Second {
		t.Errorf("Close took %s", took)
	}

	sent := c.net.count(260, "Node.Abort")
	time.Sleep(5 * retryInterval)
	if got := c.net.count(260, "Node.Abort"); got != sent {
		t.Errorf("Closed node sent %d more aborts", got-sent)
	}
}
