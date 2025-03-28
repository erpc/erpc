package erpc

import (
	"context"
	"sync"

	"github.com/erpc/erpc/common"
)

type Multiplexer struct {
	hash string
	resp *common.NormalizedResponse
	err  error
	done chan struct{}
	mu   *sync.RWMutex
	once sync.Once
}

func NewMultiplexer(hash string) *Multiplexer {
	return &Multiplexer{
		hash: hash,
		done: make(chan struct{}),
		mu:   &sync.RWMutex{},
	}
}

func (inf *Multiplexer) Close(ctx context.Context, resp *common.NormalizedResponse, err error) {
	_, span := common.StartDetailSpan(ctx, "Multiplexer.Close")
	defer span.End()

	inf.once.Do(func() {
		inf.mu.Lock()
		defer inf.mu.Unlock()
		inf.resp = resp
		inf.err = err
		close(inf.done)
	})
}
