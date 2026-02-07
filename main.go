package main

type Node interface {
	Transaction() // public API
	State()       // public API

	prepare()
	vote()
	commit()
	ack()
	writePrepareLog()  // not implemented yet
	readPrepareLog()   // not implemented yet
	writeDecisionLog() // not implemented yet
	readDecisionLog()  // not implemented yet
}

type node struct {
	id    int
	port  int
	nodes []int
	log   any // not implemented yet
}

func (n *node) State() {
	panic("unimplemented")
}

func (n *node) Transaction() {
	panic("unimplemented")
}

func (n *node) ack() {
	panic("unimplemented")
}

func (n *node) commit() {
	panic("unimplemented")
}

func (n *node) prepare() {
	panic("unimplemented")
}

func (n *node) readDecisionLog() {
	panic("unimplemented")
}

func (n *node) readPrepareLog() {
	panic("unimplemented")
}

func (n *node) vote() {
	panic("unimplemented")
}

func (n *node) writeDecisionLog() {
	panic("unimplemented")
}

func (n *node) writePrepareLog() {
	panic("unimplemented")
}

func NewNode(nodeID int, nodeIDs []int, defaultPort int) Node {
	port := defaultPort + nodeID
	nodes := make([]int, 0, len(nodeIDs))

	for _, id := range nodeIDs {
		nodes = append(nodes, id+defaultPort)
	}

	return &node{
		id:    nodeID,
		port:  port,
		nodes: nodes,
	}
}
