package erpc

import (
	"sync"

	"github.com/flair-sdk/erpc/common"
)

type Multiplexer struct {
	resp common.NormalizedResponse
	err  error
	done chan struct{}
	mu   *sync.RWMutex
}

func NewMultiplexer() *Multiplexer {
	return &Multiplexer{
		done: make(chan struct{}),
		mu:   &sync.RWMutex{},
	}
}

func (inf *Multiplexer) Close(resp common.NormalizedResponse, err error) {
	inf.mu.Lock()
	defer inf.mu.Unlock()
	inf.resp = resp
	inf.err = err
	close(inf.done)
}
