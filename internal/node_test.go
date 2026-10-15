package internal

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rodrigocitadin/two-phase-commit/internal/store"
)

const retryInterval = 50 * time.Millisecond

var testLogger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

// cluster runs nodes firstID..firstID+size-1 on ports 3000+id, sharing a
// temporary data directory that survives node restarts within the test.
// Every link goes through net and every WAL through disks[id], so tests can
// inject faults.
type cluster struct {
	t     *testing.T
	dir   string
	addrs map[int]string
	ids   []int
	nodes map[int]*node
	net   *faultyNet
	disks map[int]*faultyStore
}

func newCluster(t *testing.T, firstID, size int) *cluster {
	c := &cluster{
		t:     t,
		dir:   t.TempDir(),
		addrs: map[int]string{},
		nodes: map[int]*node{},
		net:   newFaultyNet(),
		disks: map[int]*faultyStore{},
	}
	for i := range size {
		id := firstID + i
		c.ids = append(c.ids, id)
		c.addrs[id] = fmt.Sprintf("localhost:%d", 3000+id)
	}
	for _, id := range c.ids {
		c.start(id)
	}
	t.Cleanup(func() {
		for _, n := range c.nodes {
			n.Close()
		}
	})
	return c
}

func (c *cluster) node(id int) *node {
	return c.nodes[id]
}

func (c *cluster) start(id int) *node {
	c.t.Helper()
	n, err := newNode(Config{
		ID:            id,
		Nodes:         c.addrs,
		DataDir:       c.dir,
		RPCTimeout:    500 * time.Millisecond,
		RetryInterval: retryInterval,
		Logger:        testLogger,
		wrapPeer:      func(p Peer) Peer { return &faultyPeer{Peer: p, from: id, net: c.net} },
		wrapStore: func(s store.StableStore) store.StableStore {
			c.disks[id] = &faultyStore{StableStore: s}
			return c.disks[id]
		},
	})
	if err != nil {
		c.t.Fatalf("Failed to start node %d: %v", id, err)
	}
	c.nodes[id] = n
	return n
}

// crash stops the node. Everything it acknowledged is in its WAL, so a
// Close is indistinguishable from a crash for the protocol.
func (c *cluster) crash(id int) {
	c.t.Helper()
	c.t.Logf("Crashing node %d", id)
	c.nodes[id].Close()
	delete(c.nodes, id)
}

func (c *cluster) restart(id int) *node {
	c.t.Helper()
	c.t.Logf("Restarting node %d", id)
	return c.start(id)
}

// live returns the ids of the running nodes, in order.
func (c *cluster) live() []int {
	var ids []int
	for _, id := range c.ids {
		if c.nodes[id] != nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// down returns the ids of the crashed nodes, in order.
func (c *cluster) down() []int {
	var ids []int
	for _, id := range c.ids {
		if c.nodes[id] == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func (c *cluster) walPath(id int) string {
	return filepath.Join(c.dir, fmt.Sprintf("node_%d.wal", id))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForWithin(t, 5*time.Second, what, cond)
}

func waitForWithin(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("Timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (n *node) record(txID uuid.UUID) store.TxRecord {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.txs[txID]
}

func (n *node) records() map[uuid.UUID]store.TxRecord {
	n.mu.Lock()
	defer n.mu.Unlock()
	return maps.Clone(n.txs)
}

func (c *cluster) waitForState(want int) {
	c.t.Helper()
	for id, n := range c.nodes {
		waitFor(c.t, fmt.Sprintf("node %d to reach state %d", id, want), func() bool { return n.State() == want })
	}
}

// crashAt makes the next transaction coordinated by n stop at point, and
// stores its id in txID.
func crashAt(n *node, point crashPoint, txID *uuid.UUID) {
	n.crashHook = func(p crashPoint, id uuid.UUID) bool {
		*txID = id
		return p == point
	}
}

func TestTwoPhaseCommit_HappyPath(t *testing.T) {
	c := newCluster(t, 10, 3)

	if err := c.node(10).Transaction(10); err != nil {
		t.Fatalf("Transaction failed: %v", err)
	}

	c.waitForState(10)
}

func TestTwoPhaseCommit_AbortOnPrepareFailure(t *testing.T) {
	c := newCluster(t, 20, 2)
	coordinator, participant := c.node(20), c.node(21)

	// Lock the participant with another transaction so it votes NO.
	fakeTxID := uuid.New()
	if _, err := participant.prepare(fakeTxID, 99, func(int) int { return 999 }); err != nil {
		t.Fatal(err)
	}

	var txID uuid.UUID
	coordinator.crashHook = func(_ crashPoint, id uuid.UUID) bool { txID = id; return false }
	if err := coordinator.Transaction(50); err == nil {
		t.Fatal("Expected transaction to fail, but it succeeded")
	}

	if coordinator.State() != 0 || participant.State() != 0 {
		t.Errorf("State altered on abort: coordinator=%d participant=%d", coordinator.State(), participant.State())
	}
	if coordinator.isPrepared(txID) {
		t.Error("Coordinator still locked after abort")
	}
	if !participant.isPrepared(fakeTxID) {
		t.Error("Participant lost the lock of its other transaction")
	}
}

func TestCoordinator_Timeout_HandlesPeerFailure(t *testing.T) {
	c := newCluster(t, 40, 2)
	coordinator := c.node(40)
	c.crash(41)

	var txID uuid.UUID
	coordinator.crashHook = func(_ crashPoint, id uuid.UUID) bool { txID = id; return false }
	if err := coordinator.Transaction(10); err == nil {
		t.Fatal("Transaction succeeded despite dead peer!")
	}

	if coordinator.State() != 0 || coordinator.isPrepared(txID) {
		t.Errorf("Coordinator inconsistent: state=%d locked=%v", coordinator.State(), coordinator.isPrepared(txID))
	}
}

func TestSequentialTransactions(t *testing.T) {
	c := newCluster(t, 50, 3)

	// Every node takes a turn as coordinator.
	for i := range 6 {
		coordinator := c.node(50 + i%3)
		if err := coordinator.Transaction(1); err != nil {
			t.Fatalf("Transaction %d failed: %v", i, err)
		}
		c.waitForState(i + 1)
	}
}

func TestNodeRecovery_Persistence(t *testing.T) {
	c := newCluster(t, 30, 2)

	if err := c.node(30).Transaction(100); err != nil {
		t.Fatalf("Setup transaction failed: %v", err)
	}
	c.waitForState(100)

	c.crash(31)
	if got := c.restart(31).State(); got != 100 {
		t.Errorf("Recovery failed. Expected State 100, Got %d", got)
	}

	// Restart again: the state must also survive the snapshot + truncate.
	c.crash(31)
	if got := c.restart(31).State(); got != 100 {
		t.Errorf("Second recovery failed. Expected State 100, Got %d", got)
	}
}

// The participant votes YES and crashes before the COMMIT arrives. After the
// restart it must commit, never abort on its own.
func TestParticipantCrashAfterVoting(t *testing.T) {
	c := newCluster(t, 60, 2)
	coordinator := c.node(60)

	var txID uuid.UUID
	coordinator.crashHook = func(p crashPoint, id uuid.UUID) bool {
		txID = id
		if p == crashAfterVotes {
			c.crash(61)
		}
		return false
	}
	if err := coordinator.Transaction(100); err != nil {
		t.Fatalf("Transaction failed: %v", err)
	}

	participant := c.restart(61)
	waitFor(t, "participant to commit", func() bool { return participant.State() == 100 })
	waitFor(t, "coordinator to log END", func() bool { return coordinator.record(txID).Ended })
}

// The coordinator crashes after collecting the votes but before logging a
// decision. On restart it must abort and release the participants.
func TestCoordinatorCrashBeforeDecision(t *testing.T) {
	c := newCluster(t, 70, 3)

	var txID uuid.UUID
	crashAt(c.node(70), crashAfterVotes, &txID)
	if err := c.node(70).Transaction(10); err != errCrashed {
		t.Fatalf("Expected simulated crash, got %v", err)
	}
	c.crash(70)

	for _, id := range []int{71, 72} {
		if !c.node(id).isPrepared(txID) {
			t.Fatalf("Participant %d should be prepared", id)
		}
	}

	coordinator := c.restart(70)
	for _, id := range []int{71, 72} {
		participant := c.node(id)
		waitFor(t, fmt.Sprintf("participant %d to abort", id), func() bool { return !participant.isPrepared(txID) })
	}
	if got := coordinator.getStatus(txID); got != store.TRANSACTION_ABORTED {
		t.Errorf("Coordinator status = %s, want ABORTED", got)
	}
	c.waitForState(0)

	// The cluster is usable again.
	if err := c.node(71).Transaction(1); err != nil {
		t.Fatalf("Transaction after recovery failed: %v", err)
	}
	c.waitForState(1)
}

// The coordinator crashes after logging COMMIT but before telling anyone.
// Participants must stay blocked while it is down and commit once it is back.
func TestCoordinatorCrashAfterDecision(t *testing.T) {
	c := newCluster(t, 80, 3)

	var txID uuid.UUID
	crashAt(c.node(80), crashAfterDecision, &txID)
	if err := c.node(80).Transaction(10); err != errCrashed {
		t.Fatalf("Expected simulated crash, got %v", err)
	}
	c.crash(80)

	// Blocking is the price of 2PC: they cannot abort, the decision was COMMIT.
	time.Sleep(10 * retryInterval)
	for _, id := range []int{81, 82} {
		if p := c.node(id); !p.isPrepared(txID) || p.State() != 0 {
			t.Fatalf("Participant %d decided without the coordinator", id)
		}
	}

	coordinator := c.restart(80)
	if got := coordinator.getStatus(txID); got != store.TRANSACTION_COMMITTED {
		t.Errorf("Coordinator status after restart = %s, want COMMITTED", got)
	}
	c.waitForState(10)
	waitFor(t, "coordinator to log END", func() bool { return coordinator.record(txID).Ended })
}

// An ABORT of an unrelated transaction logged after an in-doubt PREPARED must
// not hide it during recovery.
func TestRecoveryKeepsInDoubtTransaction(t *testing.T) {
	c := newCluster(t, 90, 3)
	coordinator := c.node(90)

	var txID uuid.UUID
	coordinator.crashHook = func(p crashPoint, id uuid.UUID) bool {
		txID = id
		if p != crashAfterVotes {
			return false
		}
		// Node 91 is prepared for txID. Another coordinator now tries a
		// transaction: 91 votes NO and logs its ABORT after the PREPARED.
		if err := c.node(92).Transaction(1); err == nil {
			t.Error("Concurrent transaction should have failed")
		}

		c.crash(91)
		if !c.restart(91).isPrepared(txID) {
			t.Error("Recovered participant forgot the in-doubt transaction")
		}
		return false
	}

	if err := coordinator.Transaction(10); err != nil {
		t.Fatalf("Transaction failed: %v", err)
	}
	c.waitForState(10)
}

func TestGetStatus_PendingWhileCollectingVotes(t *testing.T) {
	c := newCluster(t, 100, 2)
	coordinator := c.node(100)

	coordinator.crashHook = func(p crashPoint, id uuid.UUID) bool {
		if p == crashAfterVotes {
			if got := coordinator.getStatus(id); got != store.TRANSACTION_PREPARED {
				t.Errorf("Undecided transaction reported as %s", got)
			}
		}
		return false
	}
	if err := coordinator.Transaction(1); err != nil {
		t.Fatal(err)
	}
}

func TestLatePrepareAfterAbortIsRejected(t *testing.T) {
	c := newCluster(t, 110, 2)
	rpc := &nodeRPC{node: c.node(111)}
	args := RequestArgs{TxID: uuid.New(), Value: 5, SenderID: 110}

	var ok bool
	if err := rpc.Abort(args, &ok); err != nil {
		t.Fatal(err)
	}
	if err := rpc.Prepare(args, &ok); err == nil || ok {
		t.Fatal("Prepare after abort should be rejected")
	}
	if c.node(111).isPrepared(args.TxID) {
		t.Fatal("Participant locked by an aborted transaction")
	}
}
