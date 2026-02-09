package store

import (
	"encoding/gob"
	"fmt"
	"os"

	"github.com/google/uuid"
)

type StableStore interface {
	WritePrepared(txID uuid.UUID, value int) error
	WriteCommited(txID uuid.UUID, value int) error
	WriteAborted(txID uuid.UUID, value int) error
}

type stableStore struct {
	nodeID  int
	file    *os.File
	encoder *gob.Encoder
}

func (s *stableStore) WriteAborted(txID uuid.UUID, value int) error {
	panic("unimplemented")
}

func (s *stableStore) WriteCommited(txID uuid.UUID, value int) error {
	panic("unimplemented")
}

func (s *stableStore) WritePrepared(txID uuid.UUID, value int) error {
	panic("unimplemented")
}

func NewStableStore(nodeID int) (StableStore, error) {
	dir := "./logs/"
	filename := fmt.Sprintf("node_%d.log", nodeID)
	fullPath := dir + filename

	file, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return nil, err
	}

	return &stableStore{
		file:   file,
		nodeID: nodeID,
	}, nil
}
