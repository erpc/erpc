package erpc

import (
	"context"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
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
				log.Warn().Err(parseErr).Str("multiplexer_hash", m.hash).Object("response", resp).Msg("failed to parse response before storing in multiplexer")
				// If parsing fails, propagate this error instead of storing a response that can't be copied
				if err == nil {
					err = parseErr
				}
				resp = nil // Don't store a response that can't be parsed
			} else {
				// Create a copy of the response for the multiplexer to avoid race conditions
				// with the HTTP server's Release() call. We create a new response with the
				// already-parsed JsonRpcResponse so we don't need to re-parse the stream.
				multiplexerResp := common.NewNormalizedResponse()
				multiplexerResp.SetUpstream(resp.Upstream())
				multiplexerResp.SetFromCache(resp.FromCache())
				multiplexerResp.SetAttempts(resp.Attempts())
				multiplexerResp.SetRetries(resp.Retries())
				multiplexerResp.SetHedges(resp.Hedges())
				multiplexerResp.SetEvmBlockRef(resp.EvmBlockRef())
				multiplexerResp.SetEvmBlockNumber(resp.EvmBlockNumber())
				multiplexerResp.WithJsonRpcResponse(jrr)

				resp = multiplexerResp
			}
		}

		// Store the final result
		m.resp = resp
		m.err = err

		// Signal completion
		close(m.done)
	})
}
