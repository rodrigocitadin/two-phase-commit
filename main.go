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
		node, err := internal.NewNode(i, nodeAddresses)
		if err != nil {
			panic(err)
		}

		nodes = append(nodes, node)
	}

	nodes[0].Transaction(1)
	fmt.Println("\n--- end of transaction ---\n")

	nodes[1].Transaction(1)
	fmt.Println("\n--- end of transaction ---\n")

	nodes[3].Transaction(1)
	fmt.Println("\n--- end of transaction ---\n")

	nodes[2].Transaction(1)
	fmt.Println("\n--- end of transaction ---\n")
}
