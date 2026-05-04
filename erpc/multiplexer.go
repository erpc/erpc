package erpc

import (
	"context"
	"fmt"
	"sync"

	"github.com/erpc/erpc/common"
	"go.opentelemetry.io/otel/attribute"
)

type Multiplexer struct {
	hash string
	resp *common.NormalizedResponse
	err  error
	done chan struct{}
	once sync.Once

	// mu protects all mutable state: resp, err, closed, and copyWg coordination.
	// RWMutex allows multiple followers to read resp concurrently.
	mu sync.RWMutex
	// copyWg tracks followers that are actively copying the response.
	// Cleanup waits for all followers to finish copying before releasing resources.
	copyWg sync.WaitGroup
	// closed is set when cleanup starts; prevents new followers from registering
	closed bool
}

func NewMultiplexer(hash string) *Multiplexer {
	return &Multiplexer{
		hash: hash,
		done: make(chan struct{}),
	}
}

func (m *Multiplexer) Close(ctx context.Context, resp *common.NormalizedResponse, err error) {
	_, span := common.StartDetailSpan(ctx, "Multiplexer.Close")
	defer span.End()
	span.SetAttributes(attribute.String("multiplexer.hash", m.hash))

	// Ensure we only close once using once.Do for thread safety
	m.once.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		// Guarantee the done channel is closed even if cloning panics,
		// so followers never hang indefinitely.
		defer close(m.done)
		defer func() {
			if rec := recover(); rec != nil {
				m.resp = nil
				m.err = fmt.Errorf("panic in multiplexer close: %v", rec)
			}
		}()

		if resp != nil {
			jrr, parseErr := resp.JsonRpcResponse(ctx)
			if parseErr != nil {
				if err == nil {
					err = parseErr
				}
				resp = nil
			} else if jrr == nil {
				resp = nil
			} else {
				cloned, cerr := jrr.Clone()
				if cerr != nil {
					resp = nil
					err = cerr
				} else {
					multiplexerResp := common.NewNormalizedResponse()
					multiplexerResp.SetUpstream(resp.Upstream())
					multiplexerResp.SetFromCache(resp.FromCache())
					multiplexerResp.SetAttempts(resp.Attempts())
					multiplexerResp.SetRetries(resp.Retries())
					multiplexerResp.SetHedges(resp.Hedges())
					multiplexerResp.SetEvmBlockRef(resp.EvmBlockRef())
					multiplexerResp.SetEvmBlockNumber(resp.EvmBlockNumber())
					multiplexerResp.WithJsonRpcResponse(cloned)
					resp = multiplexerResp
				}
			}
		}

		m.resp = resp
		m.err = err
	})
}
