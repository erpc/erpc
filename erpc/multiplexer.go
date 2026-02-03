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

	// Ensure we only close once using once.Do for thread safety
	m.once.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		// Process the response if provided
		if resp != nil {
			if jrr, parseErr := resp.JsonRpcResponse(ctx); parseErr != nil {
				resp.Request().Network().Logger().Warn().Err(parseErr).Str("multiplexerHash", m.hash).Object("response", resp).Msg("failed to parse response before storing in multiplexer")
				// If parsing fails, propagate this error instead of storing a response that can't be copied
				if err == nil {
					err = parseErr
				}
				resp = nil // Don't store a response that can't be parsed
			} else {
				// Create a deep clone of the JsonRpcResponse so that upstream buffers can be released
				// on the original without affecting the multiplexer copy.
				cloned, cerr := jrr.Clone()
				if cerr != nil {
					resp = nil
					err = cerr
				} else {
					multiplexerResp := common.NewNormalizedResponse()
					multiplexerResp.SetUpstream(resp.Upstream())
					multiplexerResp.SetFromCache(resp.FromCache())
					multiplexerResp.SetCacheStoredAtUnix(resp.CacheStoredAtUnix())
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

		// Store the final result
		m.resp = resp
		m.err = err

		// Signal completion
		close(m.done)
	})
}
