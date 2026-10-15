package internal

type Result[T any] struct {
	PeerID int
	Value  T
	Err    error
}

// Broadcast calls method on every peer in parallel and waits for all of them.
// Each call is bounded by the peer's own RPC timeout.
func Broadcast[T any](peers []Peer, method string, args any) []Result[T] {
	resultsChan := make(chan Result[T], len(peers))

	for _, p := range peers {
		go func() {
			var reply T
			err := p.Call(method, args, &reply)

			result := Result[T]{PeerID: p.ID(), Err: err}
			if err == nil {
				// reply is only safe to read once the call completed.
				result.Value = reply
			}
			resultsChan <- result
		}()
	}

	results := make([]Result[T], 0, len(peers))
	for range peers {
		results = append(results, <-resultsChan)
	}

	return results
}
