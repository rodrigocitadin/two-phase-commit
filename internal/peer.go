package internal

import (
	"errors"
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
	client  *rpc.Client
}

func (p *peer) ID() int {
	return p.id
}

func (p *peer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client.Close()
	}
	return nil
}

func (p *peer) Call(method string, args any, reply any) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.client == nil {
		conn, err := net.DialTimeout("tcp", p.address, 2*time.Second)
		if err != nil {
			return err
		}
		p.client = rpc.NewClient(conn)
	}

	call := p.client.Go(method, args, reply, nil)

	select {
	case <-call.Done:
		if call.Error == rpc.ErrShutdown {
			p.client.Close()
			p.client = nil
		}
		return call.Error

	case <-time.After(5 * time.Second):
		p.client.Close()
		p.client = nil
		return errors.New("rpc call timed out")
	}
}

func NewPeer(id int, address string) Peer {
	return &peer{
		id:      id,
		address: address,
	}
}
