package internal

import (
	"net/rpc"
)

type Peer interface {
	ID() int
	Call(method string, args, reply any) error
}

type peer struct {
	id      int
	address string
	client  *rpc.Client
}

func (p *peer) ID() int {
	return p.id
}

func (p *peer) Call(method string, args any, reply any) error {
	if p.client == nil {
		c, err := rpc.Dial("tcp", p.address)
		if err != nil {
			return err
		}
		p.client = c
	}

	err := p.client.Call(method, args, reply)

	if err == rpc.ErrShutdown {
		p.client.Close()
		p.client = nil
	}

	return err
}

func NewPeer(id int, address string) Peer {
	return &peer{
		id:      id,
		address: address,
	}
}

