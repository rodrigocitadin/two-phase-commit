package internal

type Result[T any] struct {
	PeerID int
	Value  T
	Err    error
}

func Broadcast[T any](peers []Peer, method string, args any) []Result[T] {
	resultsChan := make(chan Result[T], len(peers))

	for _, p := range peers {
		go func(peer Peer) {
			var reply T
			err := peer.Call(method, args, &reply)

			resultsChan <- Result[T]{
				PeerID: peer.ID(),
				Value:  reply,
				Err:    err,
			}
		}(p)
	}

	results := make([]Result[T], 0, len(peers))
	for range peers {
		results = append(results, <-resultsChan)
	}

	return results
}
