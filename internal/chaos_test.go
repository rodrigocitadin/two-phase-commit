package internal

import (
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rodrigocitadin/two-phase-commit/internal/store"
)

// lockedRand is shared by the test and the network rule, which runs in many
// goroutines.
type lockedRand struct {
	mu sync.Mutex
	r  *rand.Rand
}

func (r *lockedRand) IntN(n int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.r.IntN(n)
}

func (r *lockedRand) Shuffle(ids []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.r.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
}

// TestChaos runs concurrent transactions while nodes crash at random points
// and the network drops, duplicates and delays messages. Then it heals
// everything and checks that the cluster converged to a consistent state.
//
// The schedule depends on goroutine timing too, so a seed reproduces it only
// approximately: rerun with CHAOS_SEED=<seed> -count=N.
func TestChaos(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	if s := os.Getenv("CHAOS_SEED"); s != "" {
		seed, _ = strconv.ParseUint(s, 10, 64)
	}
	t.Logf("CHAOS_SEED=%d", seed)
	rng := &lockedRand{r: rand.New(rand.NewPCG(seed, seed))}

	c := newCluster(t, 210, 4)
	c.net.setRule(func(link) fault {
		switch rng.IntN(60) {
		case 0:
			return dropRequest
		case 1:
			return dropReply
		case 2:
			return duplicate
		case 3:
			return delayRequest
		default:
			return deliver
		}
	})

	crashPoints := []crashPoint{crashAfterVotes, crashAfterDecision}
	for range 60 {
		live, down := c.live(), c.down()
		switch r := rng.IntN(10); {
		case r < 2 && len(live) > 1:
			c.crash(live[rng.IntN(len(live))])

		case (r < 5 || len(live) == 0) && len(down) > 0:
			c.restart(down[rng.IntN(len(down))])

		default:
			// Up to 2 concurrent transactions, each coordinated by a different
			// node. They compete for the locks, so often both abort.
			rng.Shuffle(live)
			coordinators := live[:1+rng.IntN(min(2, len(live)))]
			crashed := make([]bool, len(coordinators))

			var wg sync.WaitGroup
			for i, id := range coordinators {
				n := c.node(id)
				n.crashHook = nil
				if rng.IntN(8) == 0 {
					point := crashPoints[rng.IntN(len(crashPoints))]
					n.crashHook = func(p crashPoint, _ uuid.UUID) bool { return p == point }
				}
				wg.Go(func() { crashed[i] = n.Transaction(1) == errCrashed })
			}
			wg.Wait()

			for i, id := range coordinators {
				if crashed[i] {
					c.crash(id)
				}
			}
		}
	}

	c.net.heal()
	for _, id := range c.down() {
		c.restart(id)
	}
	for _, n := range c.nodes {
		n.crashHook = nil
	}

	waitForQuiescence(t, c)
	checkInvariants(t, c)

	// Nobody is left locked: the cluster still accepts transactions.
	for _, id := range c.ids {
		if err := c.node(id).Transaction(1); err != nil {
			t.Fatalf("Transaction on node %d after healing failed: %v", id, err)
		}
	}
	waitForQuiescence(t, c)
	checkInvariants(t, c)
}

// waitForQuiescence waits until no transaction is in doubt and every
// coordinator got all acknowledgements.
func waitForQuiescence(t *testing.T, c *cluster) {
	t.Helper()
	waitForWithin(t, 20*time.Second, "cluster to settle", func() bool {
		for id, n := range c.nodes {
			for _, rec := range n.records() {
				if rec.State == store.TRANSACTION_PREPARED || (rec.SenderID == id && !rec.Ended) {
					return false
				}
			}
		}
		return true
	})
}

func checkInvariants(t *testing.T, c *cluster) {
	t.Helper()

	// Atomicity: a transaction has the same outcome on every node that knows it.
	outcome := map[uuid.UUID]store.TransactionState{}
	committed := 0
	for id, n := range c.nodes {
		for txID, rec := range n.records() {
			prev, seen := outcome[txID]
			if !seen {
				outcome[txID] = rec.State
				if rec.State == store.TRANSACTION_COMMITTED {
					committed++
				}
			} else if prev != rec.State {
				t.Errorf("Transaction %s is %s on node %d but %s elsewhere", txID, rec.State, id, prev)
			}
		}
	}

	// Every node takes part in every commit, so every node applied each
	// committed +1 exactly once: no lost or duplicated update.
	for id, n := range c.nodes {
		if n.State() != committed {
			t.Errorf("Node %d state = %d, want %d (committed transactions)", id, n.State(), committed)
		}
	}
	t.Logf("%d transactions known, %d committed", len(outcome), committed)
}
