package internal

import (
	"errors"
	"log/slog"
	"net"
	"net/rpc"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/rodrigocitadin/two-phase-commit/internal/store"
)

type Node interface {
	Transaction(value int) error
	State() int
	Close() error

	prepare(txID uuid.UUID, value, senderID int) error
	commit(txID uuid.UUID, value, senderID int) error
	abort(txID uuid.UUID, senderID int) error
	checkResult(result []Result[bool]) bool
	recover() error
	getStatus(txID uuid.UUID) (store.TransactionState, error)
}

type node struct {
	id            int
	address       string
	peers         []Peer
	stableStore   store.StableStore
	volatileStore store.VolatileStore
	listener      net.Listener
	logger        *slog.Logger
}

func (n *node) Close() error {
	n.logger.Info("Shutting down node")
	var errs []error

	if n.listener != nil {
		n.logger.Info("Closing TCP listener", "address", n.address)
		if err := n.listener.Close(); err != nil {
			n.logger.Error("Error closing listener", "error", err)
			errs = append(errs, err)
		}
	}

	n.logger.Info("Closing connections to peers", "count", len(n.peers))
	for _, p := range n.peers {
		if err := p.Close(); err != nil {
			n.logger.Warn("Error closing peer", "peer_id", p.ID(), "error", err)
			errs = append(errs, err)
		}
	}

	n.logger.Info("Closing stable store")
	if err := n.stableStore.Close(); err != nil {
		n.logger.Error("Error closing stable store", "error", err)
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return errs[0]
	}

	n.logger.Info("Node shutdown complete")
	return nil
}

func (n *node) getStatus(txID uuid.UUID) (store.TransactionState, error) {
	return n.stableStore.GetTransactionState(txID)
}

func (n *node) recover() error {
	n.logger.Info("Starting recovery", "node_id", n.id)
	snapshot, err := n.stableStore.LoadSnapshot()
	if err != nil {
		return err
	}

	rebuiltState := 0
	rebuiltHistory := make(map[uuid.UUID]bool)

	if snapshot != nil {
		rebuiltState = snapshot.State
		rebuiltHistory = snapshot.CommittedLog
		n.logger.Info("Loaded snapshot", "state", rebuiltState)
	}

	var lastTx *store.Entry

	err = n.stableStore.ReplayLog(func(e store.Entry) error {
		lastTx = &e
		if e.State == store.TRANSACTION_COMMITTED {
			rebuiltState = e.Value
			rebuiltHistory[e.TxID] = true
		}
		return nil
	})
	if err != nil {
		return err
	}

	n.volatileStore.Recover(rebuiltState, rebuiltHistory)

	if lastTx != nil && lastTx.State == store.TRANSACTION_PREPARED {
		n.logger.Warn("Found transaction in PREPARED state during recovery. Attempting resolution.", "txID", lastTx.TxID)
		n.volatileStore.Prepare(lastTx.TxID, lastTx.Value)
		go n.resolveAnomaly(lastTx.TxID, lastTx.SenderID, lastTx.Value)
	}

	if err := n.stableStore.SaveSnapshot(n.volatileStore.State(), n.volatileStore.GetCommittedHistory()); err != nil {
		return err
	}
	return n.stableStore.Truncate()
}

func (n *node) resolveAnomaly(txID uuid.UUID, senderID int, value int) {
	logger := n.logger.With("txID", txID, "process", "anomaly_resolution")

	if senderID == n.id {
		logger.Info("Coordinator recovered, aborting own incomplete transaction")
		n.abort(txID, senderID)
		return
	}

	var coordinator Peer
	for _, p := range n.peers {
		if p.ID() == senderID {
			coordinator = p
			break
		}
	}

	if coordinator == nil {
		logger.Error("Coordinator not found in peer list, aborting", "coordinator_id", senderID)
		n.abort(txID, senderID)
		return
	}

	for {
		var status store.TransactionState
		err := coordinator.Call("Node.GetStatus", txID, &status)

		if err == nil {
			logger.Info("Fetched status from coordinator", "status", status)
			if status == store.TRANSACTION_COMMITTED {
				n.commit(txID, value, senderID)
			} else {
				n.abort(txID, senderID)
			}
			return
		}

		logger.Warn("Failed to contact coordinator, retrying...", "error", err)
		time.Sleep(2 * time.Second)
	}
}

func (n *node) abort(txID uuid.UUID, senderID int) error {
	logger := n.logger.With("txID", txID, "process", "abort")

	logger.Info("Aborting transaction")
	if err := n.stableStore.WriteAborted(txID, senderID); err != nil {
		return err
	}

	if err := n.volatileStore.Abort(txID); err != nil {
		return err
	}

	return nil
}

func (n *node) prepare(txID uuid.UUID, value, senderID int) error {
	logger := n.logger.With("txID", txID, "process", "prepare")

	logger.Debug("Preparing transaction", "value", value)
	if err := n.volatileStore.Prepare(txID, value); err != nil {
		logger.Warn("Prepare failed in volatile store", "error", err)
		return err
	}

	if err := n.stableStore.WritePrepared(txID, value, senderID); err != nil {
		logger.Error("WAL write failed during prepare", "error", err)
		if err := n.abort(txID, senderID); err != nil {
			return err
		}
		return err
	}

	return nil
}

func (n *node) commit(txID uuid.UUID, value, senderID int) error {
	logger := n.logger.With("txID", txID, "process", "commit")

	logger.Info("Committing transaction", "final_value", value)
	if err := n.stableStore.WriteCommited(txID, value, senderID); err != nil {
		n.abort(txID, senderID)
		return err
	}

	if err := n.volatileStore.Commit(txID); err != nil {
		return err
	}

	return nil
}

func (n *node) State() int {
	return n.volatileStore.State()
}

func (n *node) checkResult(result []Result[bool]) bool {
	success := true
	for _, v := range result {
		if v.Err != nil {
			n.logger.Warn("Peer returned error", "peer_id", v.PeerID, "error", v.Err)
			success = false
		} else if v.Value == false {
			n.logger.Warn("Peer rejected transaction", "peer_id", v.PeerID)
			success = false
		}
	}

	return success
}

func (n *node) Transaction(value int) error {
	txID, err := uuid.NewUUID()
	if err != nil {
		return err
	}

	logger := n.logger.With("txID", txID, "coordinator", n.id)
	logger.Info("Initiating transaction", "delta", value)

	computedValue := n.volatileStore.State() + value

	// --- PHASE 1: PREPARE ---
	if err := n.prepare(txID, computedValue, n.id); err != nil {
		n.abort(txID, n.id)
		return errors.New("coordinator is busy/locked")
	}

	transactionArgs := RequestArgs{
		TxID:     txID,
		Value:    computedValue,
		SenderID: n.id,
	}

	prepareResults := Broadcast[bool](n.peers, "Node.Prepare", transactionArgs)
	if !n.checkResult(prepareResults) {
		logger.Warn("Consensus failed in Phase 1 (Prepare). Broadcasting Abort.")
		Broadcast[bool](n.peers, "Node.Abort", RequestArgs{TxID: txID})
		n.abort(txID, n.id)
		return errors.New("consensus failed: a peer rejected or failed")
	}

	// --- PHASE 2: COMMIT ---
	if err := n.commit(txID, computedValue, n.id); err != nil {
		logger.Error("Critical: Failed to commit on coordinator")
		return err // rare critical failure and unsolved in this project/protocol
	}

	Broadcast[bool](n.peers, "Node.Commit", transactionArgs)
	logger.Info("Transaction successfully committed")
	return nil
}

func NewNode(id int, nodes map[int]string) (Node, error) {
	port := 3000 + id
	address := "localhost:" + strconv.Itoa(port)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logger = logger.With("node_id", id)

	peers := make([]Peer, 0, len(nodes))
	for peerId, peerAddress := range nodes {
		if id != peerId && address != peerAddress {
			peer := NewPeer(peerId, peerAddress, logger)
			peers = append(peers, peer)
		}
	}

	stableStore, err := store.NewStableStore(id)
	if err != nil {
		return nil, err
	}

	volatileStore := store.NewVolatileStore(0)

	l, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}

	n := &node{
		id:            id,
		address:       address,
		peers:         peers,
		stableStore:   stableStore,
		volatileStore: volatileStore,
		listener:      l,
		logger:        logger,
	}

	if err := n.recover(); err != nil {
		return nil, err
	}

	nodeRPC := newNodeRPC(n)

	server := rpc.NewServer()
	server.RegisterName("Node", nodeRPC)

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go server.ServeConn(conn)
		}
	}()

	return n, nil
}
