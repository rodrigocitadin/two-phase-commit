package main

import (
	"fmt"
	"strconv"

	"github.com/rodrigocitadin/two-phase-commit/internal"
)

const (
	numberOfNodes = 4
)

func main() {
	nodeAddresses := make(map[int]string, numberOfNodes)
	for i := range numberOfNodes {
		port := 3000 + i
		nodeAddresses[i] = "localhost:" + strconv.Itoa(port)
	}

	nodes := make([]internal.Node, 0, numberOfNodes)
	for i := range numberOfNodes {
		node, err := internal.NewNode(internal.Config{ID: i, Nodes: nodeAddresses, DataDir: "./logs"})
		if err != nil {
			panic(err)
		}

		nodes = append(nodes, node)
	}

	for _, coordinator := range []int{0, 1, 3, 2} {
		nodes[coordinator].Transaction(1)
		fmt.Print("\n--- end of transaction ---\n\n")
	}

	for _, n := range nodes {
		n.Close()
	}
}
