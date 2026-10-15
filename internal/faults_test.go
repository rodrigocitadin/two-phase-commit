package internal

import (
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rodrigocitadin/two-phase-commit/internal/store"
)

var (
	errInjectedNet  = errors.New("injected network fault")
	errInjectedDisk = errors.New("injected disk fault")
)

type fault int

const (
	deliver      fault = iota
	dropRequest        // the call never reaches the peer
	dropReply          // the peer handles the call, but the caller sees an error
	duplicate          // the peer handles the call twice
	delayRequest       // the caller gives up, the peer handles the call later
)

type link struct {
	from, to int
	method   string
}

// faultyNet decides the fate of every RPC between the nodes of a cluster.
type faultyNet struct {
	mu      sync.Mutex
	rule    func(l link) fault
	calls   map[link]int
	delay   time.Duration
	delayed sync.WaitGroup
}

func newFaultyNet() *faultyNet {
	return &faultyNet{calls: map[link]int{}, delay: 200 * time.Millisecond}
}

func (fn *faultyNet) setRule(rule func(l link) fault) {
	fn.mu.Lock()
	defer fn.mu.Unlock()
	fn.rule = rule
}

// heal stops injecting faults and waits for delayed requests to be delivered.
func (fn *faultyNet) heal() {
	fn.setRule(nil)
	fn.delayed.Wait()
}

// count returns how many calls of method were sent by from.
func (fn *faultyNet) count(from int, method string) int {
	fn.mu.Lock()
	defer fn.mu.Unlock()
	total := 0
	for l, n := range fn.calls {
		if l.from == from && l.method == method {
			total += n
		}
	}
	return total
}

func (fn *faultyNet) decide(l link) fault {
	fn.mu.Lock()
	defer fn.mu.Unlock()

	fn.calls[l]++
	if fn.rule == nil {
		return deliver
	}
	f := fn.rule(l)
	if f == delayRequest {
		fn.delayed.Add(1)
	}
	return f
}

type faultyPeer struct {
	Peer
	from int
	net  *faultyNet
}

func (p *faultyPeer) Call(method string, args, reply any) error {
	switch p.net.decide(link{from: p.from, to: p.ID(), method: method}) {
	case dropRequest:
		return errInjectedNet
	case dropReply:
		if err := p.Peer.Call(method, args, reply); err != nil {
			return err
		}
		return errInjectedNet
	case duplicate:
		p.Peer.Call(method, args, reply)
		return p.Peer.Call(method, args, reply)
	case delayRequest:
		go func() {
			defer p.net.delayed.Done()
			time.Sleep(p.net.delay)
			p.Peer.Call(method, args, reflect.New(reflect.TypeOf(reply).Elem()).Interface())
		}()
		return errInjectedNet
	default:
		return p.Peer.Call(method, args, reply)
	}
}

// faultyStore fails the appends matched by fail, without writing anything.
type faultyStore struct {
	store.StableStore
	mu   sync.Mutex
	fail func(e store.Entry) bool
}

func (s *faultyStore) setFail(fail func(e store.Entry) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

func (s *faultyStore) Append(e store.Entry) error {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()

	if fail != nil && fail(e) {
		return errInjectedDisk
	}
	return s.StableStore.Append(e)
}

func failState(state store.TransactionState) func(store.Entry) bool {
	return func(e store.Entry) bool { return e.State == state }
}

func onMethod(method string, f fault) func(link) fault {
	return func(l link) fault {
		if l.method == method {
			return f
		}
		return deliver
	}
}

// --- Disk faults ---

// A participant that cannot log the COMMIT must stay PREPARED (never abort)
// until the coordinator's retransmission succeeds.
func TestDiskFault_ParticipantCommitIsRetried(t *testing.T) {
	c := newCluster(t, 130, 3)
	coordinator := c.node(130)
	c.disks[131].setFail(failState(store.TRANSACTION_COMMITTED))

	var txID uuid.UUID
	coordinator.crashHook = func(_ crashPoint, id uuid.UUID) bool { txID = id; return false }
	if err := coordinator.Transaction(10); err != nil {
		t.Fatalf("Transaction failed: %v", err)
	}

	time.Sleep(5 * retryInterval)
	if p := c.node(131); !p.isPrepared(txID) || p.State() != 0 {
		t.Fatalf("Participant left PREPARED without logging the commit")
	}
	if coordinator.record(txID).Ended {
		t.Fatal("Coordinator logged END without every acknowledgement")
	}

	c.disks[131].setFail(nil)
	c.waitForState(10)
	waitFor(t, "coordinator to log END", func() bool { return coordinator.record(txID).Ended })
}

// A coordinator that cannot log its decision must not tell anyone. After a
// restart the transaction is aborted.
func TestDiskFault_CoordinatorCannotLogDecision(t *testing.T) {
	c := newCluster(t, 140, 3)
	coordinator := c.node(140)
	c.disks[140].setFail(failState(store.TRANSACTION_COMMITTED))

	var txID uuid.UUID
	coordinator.crashHook = func(_ crashPoint, id uuid.UUID) bool { txID = id; return false }
	if err := coordinator.Transaction(10); err == nil {
		t.Fatal("Transaction succeeded without a durable decision")
	}

	if got := coordinator.getStatus(txID); got != store.TRANSACTION_PREPARED {
		t.Errorf("Coordinator status = %s, want PREPARED (undecided)", got)
	}
	if c.net.count(140, "Node.Commit") != 0 {
		t.Error("Coordinator sent a decision it did not log")
	}

	c.crash(140)
	c.restart(140)
	for _, id := range c.ids {
		n := c.node(id)
		waitFor(t, "transaction to abort everywhere", func() bool {
			return n.record(txID).State == store.TRANSACTION_ABORTED
		})
	}
	c.waitForState(0)
}

// A participant that cannot log its PREPARED votes NO and releases the lock.
func TestDiskFault_PrepareVotesNo(t *testing.T) {
	c := newCluster(t, 150, 3)
	c.disks[151].setFail(failState(store.TRANSACTION_PREPARED))

	if err := c.node(150).Transaction(10); err == nil {
		t.Fatal("Transaction succeeded without a durable vote")
	}

	c.disks[151].setFail(nil)
	if err := c.node(150).Transaction(10); err != nil {
		t.Fatalf("Transaction after disk recovered failed: %v", err)
	}
	c.waitForState(10)
}

// --- Network faults ---

// The participant commits but the ack is lost: the coordinator retransmits,
// the commit is idempotent and END is only logged once the ack arrives.
func TestNetFault_LostCommitAck(t *testing.T) {
	c := newCluster(t, 160, 2)
	coordinator := c.node(160)
	c.net.setRule(onMethod("Node.Commit", dropReply))

	var txID uuid.UUID
	coordinator.crashHook = func(_ crashPoint, id uuid.UUID) bool { txID = id; return false }
	if err := coordinator.Transaction(10); err != nil {
		t.Fatalf("Transaction failed: %v", err)
	}
	c.waitForState(10)

	waitFor(t, "commit retransmissions", func() bool { return c.net.count(160, "Node.Commit") >= 3 })
	if coordinator.record(txID).Ended {
		t.Fatal("Coordinator logged END without the acknowledgement")
	}

	c.net.heal()
	waitFor(t, "coordinator to log END", func() bool { return coordinator.record(txID).Ended })
	if c.node(161).State() != 10 {
		t.Error("Duplicated commit changed the state")
	}
}

// Every COMMIT is lost: the in-doubt participant learns the outcome by
// asking the coordinator.
func TestNetFault_LostCommitIsLearnedFromCoordinator(t *testing.T) {
	c := newCluster(t, 170, 2)
	coordinator := c.node(170)
	c.net.setRule(onMethod("Node.Commit", dropRequest))

	var txID uuid.UUID
	coordinator.crashHook = func(_ crashPoint, id uuid.UUID) bool { txID = id; return false }
	if err := coordinator.Transaction(10); err != nil {
		t.Fatalf("Transaction failed: %v", err)
	}

	c.waitForState(10)
	if c.net.count(171, "Node.GetStatus") == 0 {
		t.Error("Participant committed without asking the coordinator")
	}
	if coordinator.record(txID).Ended {
		t.Error("Coordinator logged END without any acknowledgement")
	}

	c.net.heal()
	waitFor(t, "coordinator to log END", func() bool { return coordinator.record(txID).Ended })
}

func TestNetFault_DuplicatedMessages(t *testing.T) {
	c := newCluster(t, 180, 3)
	c.net.setRule(func(link) fault { return duplicate })

	for i := range 3 {
		if err := c.node(180 + i).Transaction(1); err != nil {
			t.Fatalf("Transaction %d failed: %v", i, err)
		}
		c.waitForState(i + 1)
	}

	// Aborts are duplicated too.
	fakeTxID := uuid.New()
	c.node(182).prepare(fakeTxID, 999, func(int) int { return 999 })
	if err := c.node(180).Transaction(1); err == nil {
		t.Fatal("Expected transaction to fail")
	}
	c.node(182).decide(fakeTxID, 999, store.TRANSACTION_ABORTED)

	if err := c.node(181).Transaction(1); err != nil {
		t.Fatalf("Transaction after abort failed: %v", err)
	}
	c.waitForState(4)
}

// The coordinator gives up on a slow PREPARE and aborts; when the PREPARE
// finally arrives the participant must reject it instead of locking.
func TestNetFault_DelayedPrepareAfterAbort(t *testing.T) {
	c := newCluster(t, 190, 2)
	coordinator := c.node(190)
	c.net.setRule(func(l link) fault {
		if l.method == "Node.Prepare" && l.to == 191 {
			return delayRequest
		}
		return deliver
	})

	var txID uuid.UUID
	coordinator.crashHook = func(_ crashPoint, id uuid.UUID) bool { txID = id; return false }
	if err := coordinator.Transaction(10); err == nil {
		t.Fatal("Expected transaction to abort")
	}

	c.net.heal() // waits for the delayed PREPARE to be delivered
	if c.node(191).isPrepared(txID) {
		t.Fatal("Participant locked by a transaction that was already aborted")
	}
	if err := coordinator.Transaction(10); err != nil {
		t.Fatalf("Next transaction failed: %v", err)
	}
	c.waitForState(10)
}

// --- Snapshot ---

// A crash between SaveSnapshot and Truncate leaves the old WAL next to a
// snapshot that already contains it. Replaying it must be harmless.
func TestRecovery_CrashBetweenSnapshotAndTruncate(t *testing.T) {
	c := newCluster(t, 200, 2)
	for i := range 3 {
		if err := c.node(200).Transaction(1); err != nil {
			t.Fatal(err)
		}
		c.waitForState(i + 1)
	}

	c.crash(201)
	wal, err := os.ReadFile(c.walPath(201))
	if err != nil || len(wal) == 0 {
		t.Fatalf("Expected a non empty WAL: %v", err)
	}

	c.restart(201) // snapshot + truncate
	c.crash(201)
	if err := os.WriteFile(c.walPath(201), wal, 0644); err != nil {
		t.Fatal(err)
	}

	if got := c.restart(201).State(); got != 3 {
		t.Fatalf("State after replaying old WAL = %d, want 3", got)
	}
	if err := c.node(201).Transaction(1); err != nil {
		t.Fatal(err)
	}
	c.waitForState(4)
}
