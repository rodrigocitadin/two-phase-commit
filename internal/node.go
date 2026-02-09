package internal

import (
	"errors"
	"net"
	"net/rpc"
	"strconv"

	"github.com/google/uuid"
	"github.com/rodrigocitadin/two-phase-commit/internal/store"
)

type Node interface {
	Transaction(value int) error
	State() int

	prepare(transactionID uuid.UUID, value, senderID int) error
	commit(transactionID uuid.UUID, value, senderID int) error
	checkResult(result []Result[bool]) bool
}

type node struct {
	id            int
	address       string
	peers         []Peer
	stableStore   store.StableStore
	volatileStore store.VolatileStore
}

func (n *node) commit(transactionID uuid.UUID, value, senderID int) error {

	// write to stable
	return nil
}

func (n *node) prepare(transactionID uuid.UUID, value, senderID int) error {
	// write to stable
	return nil
}

func (n *node) State() int {
	return n.volatileStore.State()
}

func (n *node) checkResult(result []Result[bool]) bool {
	for _, v := range result {
		if v.Value == false {
			return false
		}
	}
	return true
}

func (n *node) Transaction(value int) error {
	transactionID, err := uuid.NewUUID()
	if err != nil {
		return err
	}

	prepareRes := Broadcast[bool](
		n.peers,
		"Node.Prepare",
		RequestArgs{
			TransactionID: transactionID,
			Value:         value,
			SenderID:      n.id,
		})

	if !n.checkResult(prepareRes) {
		return errors.New("Someone rejected the transaction")
	}

	// newState := n.state + value

	// write decision log

	commitRes := Broadcast[bool](
		n.peers,
		"Node.Commit",
		RequestArgs{
			TransactionID: transactionID,
			Value:         value,
			SenderID:      n.id,
		})

	if !n.checkResult(commitRes) {
		return errors.New("Someone rejected the commit")
	}

	// n.state = newState

	return nil
}

func NewNode(id int, nodes map[int]string) (Node, error) {
	port := 3000 + id
	address := "localhost:" + strconv.Itoa(port)

	peers := make([]Peer, 0, len(nodes))
	for peerId, peerAddress := range nodes {
		if id != peerId && address != peerAddress {
			peer := NewPeer(peerId, peerAddress)
			peers = append(peers, peer)
		}
	}

	stableStore, err := store.NewStableStore(id)
	if err != nil {
		return nil, err
	}

	volatileStore := store.NewVolatileStore(0)

	n := &node{
		id:            id,
		address:       address,
		peers:         peers,
		stableStore:   stableStore,
		volatileStore: volatileStore,
	}

	nodeRPC := newNodeRPC(n)

	server := rpc.NewServer()
	server.RegisterName("Node", nodeRPC)

	l, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}

	go func() {
		for {
			conn, _ := l.Accept()
			go server.ServeConn(conn)
		}
	}()

	return n, nil
}
