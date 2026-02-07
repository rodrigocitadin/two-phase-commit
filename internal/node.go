package internal

import (
	"net"
	"net/rpc"
	"strconv"
)

type Node interface {
	Transaction(value int) error
	State() int
}

type node struct {
	id      int
	address string
	peers   []Peer
	state   int
}

func (n *node) State() int {
	return n.state
}

func (n *node) Transaction(value int) error {
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

	n := &node{
		id:      id,
		address: address,
		peers:   peers,
	}

	server := rpc.NewServer()
	server.RegisterName("Node", n)

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
