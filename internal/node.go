package internal

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/rpc"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rodrigocitadin/two-phase-commit/internal/store"
)

type Node interface {
	Transaction(delta int) error
	State() int
	Close() error
}

type Config struct {
	ID int
	// Nodes maps every node of the cluster (including this one) to its address.
	Nodes   map[int]string
	DataDir string
	// RPCTimeout bounds dialing and each RPC call.
	RPCTimeout time.Duration
	// RetryInterval is how often the coordinator retransmits an unacknowledged
	// decision and how often an in-doubt participant asks for it.
	RetryInterval time.Duration
	Logger        *slog.Logger

	// Test hooks to inject network and disk faults.
	wrapPeer  func(p Peer) Peer
	wrapStore func(s store.StableStore) store.StableStore
}

// crashPoint names a step of Transaction where tests can simulate a crash.
type crashPoint string

const (
	crashAfterVotes    crashPoint = "after-votes"    // votes collected, decision not logged
	crashAfterDecision crashPoint = "after-decision" // decision logged, not delivered
)

var errCrashed = errors.New("simulated crash")

type node struct {
	id            int
	address       string
	peers         []Peer
	stableStore   store.StableStore
	volatileStore store.VolatileStore
	listener      net.Listener
	logger        *slog.Logger
	retryInterval time.Duration

	// mu serializes every transaction state change (WAL write + volatile
	// update) and guards txs.
	mu  sync.Mutex
	txs map[uuid.UUID]store.TxRecord

	// bgMu guards closed, conns and adding to wg.
	bgMu   sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
	done   chan struct{}

	// crashHook, when set by tests, is called at each crashPoint of
	// Transaction; returning true stops it there as if the process died.
	crashHook func(point crashPoint, txID uuid.UUID) bool
}

func (n *node) Close() error {
	n.bgMu.Lock()
	if n.closed {
		n.bgMu.Unlock()
		return nil
	}
	n.closed = true
	close(n.done)
	conns := slices.Collect(maps.Keys(n.conns))
	n.bgMu.Unlock()

	n.logger.Info("Shutting down node")
	var errs []error

	n.logger.Info("Closing TCP listener", "address", n.address)
	if err := n.listener.Close(); err != nil {
		errs = append(errs, err)
	}
	for _, c := range conns {
		c.Close()
	}

	n.logger.Info("Closing connections to peers", "count", len(n.peers))
	for _, p := range n.peers {
		if err := p.Close(); err != nil {
			n.logger.Warn("Error closing peer", "peer_id", p.ID(), "error", err)
			errs = append(errs, err)
		}
	}

	n.wg.Wait()

	n.logger.Info("Closing stable store")
	if err := n.stableStore.Close(); err != nil {
		errs = append(errs, err)
	}

	n.logger.Info("Node shutdown complete")
	return errors.Join(errs...)
}

// goBackground runs f in a goroutine that Close waits for.
func (n *node) goBackground(f func()) {
	n.bgMu.Lock()
	defer n.bgMu.Unlock()

	if n.closed {
		return
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		f()
	}()
}

// sleep waits for d and reports false if the node is shutting down.
func (n *node) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-n.done:
		return false
	case <-t.C:
		return true
	}
}

func (n *node) serve(server *rpc.Server) {
	for {
		conn, err := n.listener.Accept()
		if err != nil {
			return
		}

		n.bgMu.Lock()
		if n.closed {
			n.bgMu.Unlock()
			conn.Close()
			return
		}
		n.conns[conn] = struct{}{}
		n.bgMu.Unlock()

		go func() {
			server.ServeConn(conn)
			n.bgMu.Lock()
			delete(n.conns, conn)
			n.bgMu.Unlock()
		}()
	}
}

func (n *node) peerByID(id int) Peer {
	for _, p := range n.peers {
		if p.ID() == id {
			return p
		}
	}
	return nil
}

// recover rebuilds the state from the snapshot and the WAL, then compacts
// them into a new snapshot. Unfinished work is picked up later by resume.
func (n *node) recover() error {
	n.logger.Info("Starting recovery")
	snapshot, err := n.stableStore.LoadSnapshot()
	if err != nil {
		return err
	}

	state := 0
	txs := make(map[uuid.UUID]store.TxRecord)
	if snapshot != nil {
		state = snapshot.State
		maps.Copy(txs, snapshot.Transactions)
		n.logger.Info("Loaded snapshot", "state", state, "transactions", len(txs))
	}

	// The WAL may repeat entries already in the snapshot (crash between
	// snapshot and truncate), so every transition below is idempotent.
	err = n.stableStore.ReplayLog(func(e store.Entry) error {
		rec, known := txs[e.TxID]
		switch e.State {
		case store.TRANSACTION_PREPARED:
			if !known {
				txs[e.TxID] = store.TxRecord{State: e.State, SenderID: e.SenderID, Value: e.Value}
			}
		case store.TRANSACTION_COMMITTED:
			if known && rec.State == store.TRANSACTION_ABORTED {
				return fmt.Errorf("tx %s is both aborted and committed in the log", e.TxID)
			}
			if !known || rec.State != store.TRANSACTION_COMMITTED {
				rec.State, rec.SenderID, rec.Value = e.State, e.SenderID, e.Value
				txs[e.TxID] = rec
				state = e.Value
			}
		case store.TRANSACTION_ABORTED:
			if known && rec.State == store.TRANSACTION_COMMITTED {
				return fmt.Errorf("tx %s is both committed and aborted in the log", e.TxID)
			}
			rec.State, rec.SenderID = e.State, e.SenderID
			txs[e.TxID] = rec
		case store.TRANSACTION_ENDED:
			if known {
				rec.Ended = true
				txs[e.TxID] = rec
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// The prepare lock allows a single PREPARED transaction at a time.
	var inDoubt []uuid.UUID
	for txID, rec := range txs {
		if rec.State == store.TRANSACTION_PREPARED {
			inDoubt = append(inDoubt, txID)
		}
	}
	if len(inDoubt) > 1 {
		return fmt.Errorf("found %d prepared transactions, expected at most one", len(inDoubt))
	}

	n.volatileStore.Recover(state)
	for _, txID := range inDoubt {
		n.volatileStore.Prepare(txID, func(int) int { return txs[txID].Value })
	}
	n.txs = txs

	// The snapshot keeps in-doubt transactions and undelivered decisions too,
	// so nothing the WAL knew is lost when it is truncated.
	err = n.stableStore.SaveSnapshot(store.Snapshot{State: state, Transactions: maps.Clone(txs)})
	if err != nil {
		return err
	}
	return n.stableStore.Truncate()
}

// resume restarts the work recovery found unfinished.
func (n *node) resume() {
	n.mu.Lock()
	defer n.mu.Unlock()

	for txID, rec := range n.txs {
		logger := n.logger.With("txID", txID, "process", "resume")
		switch {
		case rec.State == store.TRANSACTION_PREPARED && rec.SenderID == n.id:
			// We crashed before logging a decision, so nobody can have learned one.
			logger.Warn("Recovered own undecided transaction, aborting it")
			n.goBackground(func() {
				if err := n.decide(txID, n.id, store.TRANSACTION_ABORTED); err != nil {
					logger.Error("Failed to abort recovered transaction", "error", err)
					return
				}
				n.retryDecision(txID, store.TRANSACTION_ABORTED, rec.Value, n.peers)
			})

		case rec.State == store.TRANSACTION_PREPARED:
			logger.Warn("Recovered in-doubt transaction, asking the coordinator", "coordinator_id", rec.SenderID)
			n.goBackground(func() { n.awaitDecision(txID, rec.SenderID) })

		case rec.SenderID == n.id && !rec.Ended:
			logger.Info("Re-delivering decision not acknowledged by every participant", "decision", rec.State)
			n.goBackground(func() { n.retryDecision(txID, rec.State, rec.Value, n.peers) })
		}
	}
}

// awaitDecision is the participant side of the termination protocol: while
// txID is PREPARED it keeps asking the coordinator for the outcome. It never
// aborts on its own, since the coordinator may have decided COMMIT.
func (n *node) awaitDecision(txID uuid.UUID, coordinatorID int) {
	logger := n.logger.With("txID", txID, "process", "await_decision")

	coordinator := n.peerByID(coordinatorID)
	if coordinator == nil {
		logger.Error("Coordinator not in cluster config, transaction stays in doubt", "coordinator_id", coordinatorID)
		return
	}

	for n.sleep(n.retryInterval) {
		if !n.isPrepared(txID) {
			return
		}

		var status store.TransactionState
		if err := coordinator.Call("Node.GetStatus", txID, &status); err != nil {
			logger.Warn("Failed to contact coordinator, retrying...", "error", err)
			continue
		}

		if status == store.TRANSACTION_PREPARED {
			logger.Debug("Coordinator has not decided yet")
			continue
		}

		logger.Info("Fetched decision from coordinator", "status", status)
		if err := n.decide(txID, coordinatorID, status); err != nil {
			logger.Error("Failed to apply decision, retrying...", "error", err)
			continue
		}
		return
	}
}

func (n *node) getStatus(txID uuid.UUID) store.TransactionState {
	n.mu.Lock()
	defer n.mu.Unlock()

	rec, ok := n.txs[txID]
	if !ok {
		// Presumed abort: a coordinator logs PREPARED before asking anyone to
		// prepare, so a transaction it never heard of cannot have committed.
		return store.TRANSACTION_ABORTED
	}
	return rec.State
}

func (n *node) isPrepared(txID uuid.UUID) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.txs[txID].State == store.TRANSACTION_PREPARED
}

// prepare locks the store and durably logs the YES vote.
func (n *node) prepare(txID uuid.UUID, senderID int, propose func(current int) int) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	logger := n.logger.With("txID", txID, "process", "prepare")

	if rec, ok := n.txs[txID]; ok {
		if rec.State == store.TRANSACTION_PREPARED {
			return rec.Value, nil // retransmitted prepare
		}
		// e.g. a delayed prepare arriving after the abort
		return 0, fmt.Errorf("transaction already %s", rec.State)
	}

	value, err := n.volatileStore.Prepare(txID, propose)
	if err != nil {
		logger.Warn("Prepare failed in volatile store", "error", err)
		return 0, err
	}

	logger.Debug("Preparing transaction", "value", value)
	entry := store.Entry{TxID: txID, State: store.TRANSACTION_PREPARED, SenderID: senderID, Value: value}
	if err := n.stableStore.Append(entry); err != nil {
		logger.Error("WAL write failed during prepare", "error", err)
		n.volatileStore.Abort(txID)
		return 0, err
	}

	n.txs[txID] = store.TxRecord{State: store.TRANSACTION_PREPARED, SenderID: senderID, Value: value}
	return value, nil
}

// decide durably logs the outcome of txID and applies it. The coordinator
// uses it for its own decision and participants when they learn it. It is
// idempotent. If the WAL write fails nothing changes and the caller retries:
// once a transaction is decided it must never be undone locally.
func (n *node) decide(txID uuid.UUID, senderID int, decision store.TransactionState) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	logger := n.logger.With("txID", txID, "process", "decide", "decision", decision)

	rec, known := n.txs[txID]
	if known && rec.State == decision {
		return nil
	}
	if known && rec.State != store.TRANSACTION_PREPARED {
		logger.Error("Conflicting decision", "current", rec.State)
		return fmt.Errorf("cannot %s a transaction already %s", decision, rec.State)
	}
	if known {
		senderID = rec.SenderID
	}

	switch decision {
	case store.TRANSACTION_COMMITTED:
		if !known {
			return errors.New("cannot commit a transaction that was never prepared")
		}
		logger.Info("Committing transaction", "final_value", rec.Value)
		entry := store.Entry{TxID: txID, State: decision, SenderID: senderID, Value: rec.Value}
		if err := n.stableStore.Append(entry); err != nil {
			return err
		}
		if err := n.volatileStore.Commit(txID); err != nil {
			return err
		}

	case store.TRANSACTION_ABORTED:
		// Also logged for unknown transactions, so a late prepare is rejected.
		logger.Info("Aborting transaction")
		entry := store.Entry{TxID: txID, State: decision, SenderID: senderID}
		if err := n.stableStore.Append(entry); err != nil {
			return err
		}
		n.volatileStore.Abort(txID)

	default:
		return fmt.Errorf("invalid decision %s", decision)
	}

	rec.State, rec.SenderID = decision, senderID
	n.txs[txID] = rec
	return nil
}

func (n *node) markEnded(txID uuid.UUID) {
	n.mu.Lock()
	defer n.mu.Unlock()

	rec := n.txs[txID]
	if rec.Ended {
		return
	}

	entry := store.Entry{TxID: txID, State: store.TRANSACTION_ENDED, SenderID: n.id}
	if err := n.stableStore.Append(entry); err != nil {
		// Not fatal: after a restart the decision is just delivered again.
		n.logger.Warn("Failed to log END", "txID", txID, "error", err)
		return
	}
	rec.Ended = true
	n.txs[txID] = rec
}

// sendDecision sends the decision to peers and returns those that did not
// acknowledge it.
func (n *node) sendDecision(txID uuid.UUID, decision store.TransactionState, value int, peers []Peer) []Peer {
	method := "Node.Abort"
	if decision == store.TRANSACTION_COMMITTED {
		method = "Node.Commit"
	}

	args := RequestArgs{TxID: txID, Value: value, SenderID: n.id}
	var pending []Peer
	for _, r := range Broadcast[bool](peers, method, args) {
		if r.Err != nil || !r.Value {
			n.logger.Warn("Participant did not acknowledge decision", "txID", txID, "peer_id", r.PeerID, "decision", decision, "error", r.Err)
			pending = append(pending, n.peerByID(r.PeerID))
		}
	}
	return pending
}

// retryDecision keeps sending the decision to the participants that have not
// acknowledged it. Once all of them did, it logs END: from then on no
// participant can be in doubt about txID.
func (n *node) retryDecision(txID uuid.UUID, decision store.TransactionState, value int, pending []Peer) {
	for {
		pending = n.sendDecision(txID, decision, value, pending)
		if len(pending) == 0 {
			n.markEnded(txID)
			return
		}
		if !n.sleep(n.retryInterval) {
			return
		}
	}
}

func (n *node) State() int {
	return n.volatileStore.State()
}

func (n *node) checkVotes(votes []Result[bool]) bool {
	success := true
	for _, v := range votes {
		if v.Err != nil {
			n.logger.Warn("Peer returned error", "peer_id", v.PeerID, "error", v.Err)
			success = false
		} else if !v.Value {
			n.logger.Warn("Peer rejected transaction", "peer_id", v.PeerID)
			success = false
		}
	}

	return success
}

func (n *node) crashed(point crashPoint, txID uuid.UUID) bool {
	return n.crashHook != nil && n.crashHook(point, txID)
}

func (n *node) Transaction(delta int) error {
	txID, err := uuid.NewUUID()
	if err != nil {
		return err
	}

	logger := n.logger.With("txID", txID, "coordinator", n.id)
	logger.Info("Initiating transaction", "delta", delta)

	// --- PHASE 1: PREPARE ---
	// The new value is computed while holding the lock, so a commit that
	// lands concurrently cannot be overwritten.
	value, err := n.prepare(txID, n.id, func(current int) int { return current + delta })
	if err != nil {
		return fmt.Errorf("coordinator is busy/locked: %w", err)
	}

	args := RequestArgs{TxID: txID, Value: value, SenderID: n.id}
	decision := store.TRANSACTION_COMMITTED
	if !n.checkVotes(Broadcast[bool](n.peers, "Node.Prepare", args)) {
		logger.Warn("Consensus failed in Phase 1 (Prepare). Aborting.")
		decision = store.TRANSACTION_ABORTED
	}

	if n.crashed(crashAfterVotes, txID) {
		return errCrashed
	}

	// --- PHASE 2: COMMIT / ABORT ---
	// Logging the decision is the commit point of the whole transaction.
	if err := n.decide(txID, n.id, decision); err != nil {
		// The decision is not durable, so nobody learned it. Participants
		// that ask are told to wait, and recovery aborts it after a restart.
		logger.Error("Critical: failed to log decision", "error", err)
		return err
	}

	if n.crashed(crashAfterDecision, txID) {
		return errCrashed
	}

	if pending := n.sendDecision(txID, decision, value, n.peers); len(pending) > 0 {
		logger.Warn("Decision not acknowledged by every participant, retrying in background", "pending", len(pending))
		n.goBackground(func() {
			if n.sleep(n.retryInterval) {
				n.retryDecision(txID, decision, value, pending)
			}
		})
	} else {
		n.markEnded(txID)
	}

	if decision == store.TRANSACTION_ABORTED {
		return errors.New("consensus failed: a peer rejected or failed")
	}
	logger.Info("Transaction successfully committed")
	return nil
}

func NewNode(cfg Config) (Node, error) {
	n, err := newNode(cfg)
	if err != nil {
		return nil, err
	}
	return n, nil
}

func newNode(cfg Config) (*node, error) {
	address, ok := cfg.Nodes[cfg.ID]
	if !ok {
		return nil, fmt.Errorf("node %d is not in the cluster config", cfg.ID)
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "./logs"
	}
	if cfg.RPCTimeout == 0 {
		cfg.RPCTimeout = 2 * time.Second
	}
	if cfg.RetryInterval == 0 {
		cfg.RetryInterval = time.Second
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	logger = logger.With("node_id", cfg.ID)

	peers := make([]Peer, 0, len(cfg.Nodes))
	for peerID, peerAddress := range cfg.Nodes {
		if peerID == cfg.ID {
			continue
		}
		p := NewPeer(peerID, peerAddress, cfg.RPCTimeout, logger)
		if cfg.wrapPeer != nil {
			p = cfg.wrapPeer(p)
		}
		peers = append(peers, p)
	}

	stableStore, err := store.NewStableStore(cfg.DataDir, cfg.ID)
	if err != nil {
		return nil, err
	}
	if cfg.wrapStore != nil {
		stableStore = cfg.wrapStore(stableStore)
	}

	n := &node{
		id:            cfg.ID,
		address:       address,
		peers:         peers,
		stableStore:   stableStore,
		volatileStore: store.NewVolatileStore(0),
		logger:        logger,
		retryInterval: cfg.RetryInterval,
		conns:         make(map[net.Conn]struct{}),
		done:          make(chan struct{}),
	}

	// Recover before listening, so peers never see a half recovered node.
	if err := n.recover(); err != nil {
		stableStore.Close()
		return nil, err
	}

	l, err := net.Listen("tcp", address)
	if err != nil {
		stableStore.Close()
		return nil, err
	}
	n.listener = l

	server := rpc.NewServer()
	if err := server.RegisterName("Node", &nodeRPC{node: n}); err != nil {
		l.Close()
		stableStore.Close()
		return nil, err
	}
	go n.serve(server)

	n.resume()
	return n, nil
}
