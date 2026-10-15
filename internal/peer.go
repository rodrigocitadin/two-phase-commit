package internal

import (
	"errors"
	"log/slog"
	"net"
	"net/rpc"
	"sync"
	"time"
)

type Peer interface {
	ID() int
	Call(method string, args, reply any) error
	Close() error
}

type peer struct {
	mu      sync.Mutex
	id      int
	address string
	timeout time.Duration
	client  *rpc.Client
	closed  bool
	logger  *slog.Logger
}

func (p *peer) ID() int {
	return p.id
}

func (p *peer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true
	if p.client != nil {
		p.logger.Info("Closing peer connection")
		err := p.client.Close()
		p.client = nil
		return err
	}
	return nil
}

func (p *peer) getClient() (*rpc.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, errors.New("peer is closed")
	}
	if p.client != nil {
		return p.client, nil
	}

	p.logger.Debug("Dialing peer")
	conn, err := net.DialTimeout("tcp", p.address, p.timeout)
	if err != nil {
		p.logger.Warn("Failed to dial peer", "error", err)
		return nil, err
	}
	p.client = rpc.NewClient(conn)
	return p.client, nil
}

// reset drops client so the next call redials, unless it was already replaced.
func (p *peer) reset(client *rpc.Client) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.client == client {
		p.client = nil
	}
	client.Close()
}

// Call retries once on a broken connection, typically left by a peer that
// restarted since the last call. That is safe because every RPC of the
// protocol is idempotent.
func (p *peer) Call(method string, args any, reply any) error {
	broken, err := p.call(method, args, reply)
	if broken {
		_, err = p.call(method, args, reply)
	}
	return err
}

// call does not hold the lock while waiting, so calls to the same peer can
// run concurrently (rpc.Client is safe for that) and Close never blocks.
func (p *peer) call(method string, args any, reply any) (broken bool, err error) {
	client, err := p.getClient()
	if err != nil {
		return false, err
	}

	call := client.Go(method, args, reply, make(chan *rpc.Call, 1))
	timer := time.NewTimer(p.timeout)
	defer timer.Stop()

	select {
	case <-call.Done:
		var serverErr rpc.ServerError
		if call.Error != nil && !errors.As(call.Error, &serverErr) {
			// Connection level failure (peer restarted, EOF, shutdown...).
			p.logger.Warn("RPC connection failed, resetting client", "error", call.Error)
			p.reset(client)
			return true, call.Error
		}
		return false, call.Error

	case <-timer.C:
		p.logger.Warn("RPC call timed out, closing connection")
		p.reset(client)
		return false, errors.New("rpc call timed out")
	}
}

func NewPeer(id int, address string, timeout time.Duration, logger *slog.Logger) Peer {
	return &peer{
		id:      id,
		address: address,
		timeout: timeout,
		logger:  logger.With("peer_scope", "client", "target_peer_id", id),
	}
}
