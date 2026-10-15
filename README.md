# Two-Phase Commit (2PC) with WAL & Recovery

[Original 2PC Paper](https://www.researchgate.net/publication/275155037_Two-Phase_Commit)

This project is a functional implementation of the Two-Phase Commit (2PC) distributed consensus protocol, written entirely in Go. It demonstrates how distributed nodes can achieve atomic transaction commitment even in the presence of failures, using persistent storage and Write-Ahead Logging (WAL).

## What is Two-Phase Commit?

Two-Phase Commit is a standardized protocol for ensuring that a distributed transaction is either committed on all involved nodes or aborted on all of them. It consists of two phases:

1. **Prepare Phase**: The Coordinator asks all Participants if they are ready to commit. Participants lock resources and vote "Yes" or "No".
2. **Commit/Abort Phase**:
* If all Participants vote "Yes", the Coordinator sends a **Commit** message.
* If any Participant votes "No" (or times out), the Coordinator sends an **Abort** message.

This implementation also includes **Recovery** mechanisms. If a node crashes, it can rebuild its state and resolve pending transactions by reading its stable log upon restart.

The protocol follows the **presumed abort** variant: a coordinator logs `PREPARED` before asking anyone to prepare, so a transaction it has no record of can only have aborted. Logging the decision (`COMMITTED`/`ABORTED`) is the commit point of the whole transaction.

## How This Project Implements 2PC

The implementation is divided into core library components (`internal/`) and storage engines (`internal/store/`):

### Node (`internal/node.go`): 

The core entity that acts as both a Coordinator and a Participant. It handles:

* Initiating transactions.
* Broadcasting `Prepare`, `Commit`, and `Abort` RPCs to peers, and retransmitting the decision until every participant acknowledges it (then it logs `END`).
* Answering `GetStatus` for the transactions it coordinates (`PREPARED` means "not decided yet, keep waiting").
* As a participant, asking the coordinator for the outcome while it is in doubt.
* Handling incoming RPC requests via `NodeRPC`.
* Recovering state from disk on startup.


### Stable Store (`internal/store/stable.go`):

A persistent storage engine using Write-Ahead Logging (WAL).

* Records transaction states (`PREPARED`, `COMMITTED`, `ABORTED`, `END`) to disk, with `fsync`, before modifying volatile state.
* Fixed size binary records with a CRC32 in `<data dir>/node_ID.wal`. A torn record at the end of the log (crash in the middle of a write) is dropped; corruption anywhere else is an error.
* Snapshots (`<data dir>/node_ID.snap`) are written atomically (temp file + `fsync` + rename) and compact the log on every startup. They keep the in-doubt transactions and the undelivered decisions too.


### Volatile Store (`internal/store/volatile.go`):

Manages the in-memory state machine.

* Handles locking mechanisms to ensure isolation during the Prepare phase.
* Maintains the current value and the value staged by the prepared transaction.


### Cluster Simulation (`main.go`):

* A simulation entry point that spins up 4 networked nodes (ports 3000-3003) within a single process.
* Demonstrates sequential transactions where different nodes take turns acting as the Coordinator.

## Key Features

* **Crash Recovery**: Nodes replay their snapshot and WAL on startup to restore the last known consistent state, before accepting any RPC. Then:
  * a participant that was `PREPARED` keeps the lock and asks the coordinator until it gets a decision. It never aborts on its own: the coordinator may have decided `COMMIT`, so it blocks while the coordinator is down (the known cost of 2PC);
  * a coordinator that crashed before logging a decision aborts and tells the participants;
  * a coordinator re-delivers every decision not yet acknowledged by all participants.
* **RPC Communication**: Uses Go's standard `net/rpc` for type-safe peer-to-peer communication.
* **Timeout Handling**: The Coordinator aborts if peers fail to respond within the RPC timeout. Lost decision messages are retransmitted.
* **Concurrency Control**: Uses `sync.RWMutex` and distinct locking states to prevent race conditions during transaction processing.

## Project Structure

```
/
├── internal/
│   ├── node.go          # Core 2PC logic (Coordinator & Participant)
│   ├── node_rpc.go      # RPC handlers for network requests
│   ├── broadcast.go     # Helper for broadcasting messages to peers
│   ├── peer.go          # Client wrapper for dialing other nodes
│   ├── node_test.go     # Integration tests (Happy path, Abort, crash points)
│   └── store/
│       ├── stable.go    # Disk persistence (WAL & Snapshots)
│       ├── stable_test.go # WAL & snapshot tests
│       ├── volatile.go  # In-memory state & Locking
│       └── entry.go     # Log entry definitions
├── logs/                # Generated runtime logs (gitignored)
├── main.go              # Simulation entry point
├── Makefile             # Commands to run and test
└── go.mod

```

## How to Run

### Prerequisites

* Go (version 1.25 or later)
* `make`

### Running the Simulation

To see the protocol in action, run the main simulation. This starts a cluster of 4 nodes and executes a series of transactions.

```sh
make run

```

*Note: the nodes recover from `./logs` on every run, so a second run continues from the previous state. Run `make clean` to start from zero.*

### Running Tests

The project includes comprehensive integration tests that verify success, failure, and recovery scenarios.

```sh
make test

```

Every test runs its own cluster with a temporary data directory. Crashes at precise points of the protocol are simulated with a test hook in `Transaction` (`crashAfterVotes`, `crashAfterDecision`).

Key tests included in `internal/node_test.go`:

* `TestTwoPhaseCommit_HappyPath`: Verifies a standard successful transaction.
* `TestTwoPhaseCommit_AbortOnPrepareFailure`: Simulates a node rejecting a proposal, causing a cluster-wide abort.
* `TestCoordinator_Timeout_HandlesPeerFailure`: Verifies that the coordinator aborts if a participant is unresponsive.
* `TestNodeRecovery_Persistence`: Writes data, crashes a node, restarts it (twice), and verifies it recovers the correct state from disk.
* `TestParticipantCrashAfterVoting`: A participant votes YES and crashes; after restarting it commits.
* `TestCoordinatorCrashBeforeDecision`: The coordinator crashes before deciding; on restart it aborts and releases the participants.
* `TestCoordinatorCrashAfterDecision`: The coordinator crashes after logging COMMIT; participants stay blocked while it is down and commit once it is back.
* `TestRecoveryKeepsInDoubtTransaction`: An unrelated ABORT logged after an in-doubt PREPARED does not hide it during recovery.
* `TestGetStatus_PendingWhileCollectingVotes` and `TestLatePrepareAfterAbortIsRejected`.

Fault injection (`internal/faults_test.go`): every link goes through a `faultyNet` that can drop requests, drop replies, duplicate or delay calls, and every WAL through a `faultyStore` that can fail appends. Tests cover a participant or coordinator that cannot log, lost commit acks, lost commits (learned via `GetStatus`), duplicated messages, a PREPARE arriving after the abort and a crash between snapshot and truncate. `internal/edge_test.go` covers RPC idempotency, single node clusters, concurrent transactions on the same node and shutdown.

`TestChaos` (`internal/chaos_test.go`) runs concurrent transactions while nodes crash at random points and the network misbehaves, then heals everything and checks that every transaction has the same outcome on every node and that every node applied each commit exactly once. It logs its seed; rerun with `CHAOS_SEED=<seed>`.

`make fuzz` fuzzes the WAL decoder and replay (torn or corrupted logs must never panic or misalign the log).
