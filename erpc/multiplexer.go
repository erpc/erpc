package erpc

import (
	"context"
	"fmt"
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

		// Process the response if provided.
		//
		// IMPORTANT: do not deep-clone the JsonRpcResponse here.
		// Large responses (notably eth_getLogs) can be hundreds of MBs and cloning
		// the result bytes will OOM the pod.
		if resp != nil {
			jrr, parseErr := resp.JsonRpcResponse(ctx)
			if parseErr != nil {
				if req := resp.Request(); req != nil {
					if nw := req.Network(); nw != nil {
						nw.Logger().Warn().Err(parseErr).Str("multiplexerHash", m.hash).Object("response", resp).Msg("failed to parse response before storing in multiplexer")
					}
				}
				// If parsing fails, propagate this error instead of storing a response that can't be copied
				if err == nil {
					err = parseErr
				}
				resp = nil // Don't store a response that can't be parsed
			} else if jrr != nil {
				// Ensure the stored response is shallow-cloneable for followers. Some resultWriters are
				// one-shot streamers and cannot be safely cloned/shared; in those cases, disable
				// multiplex fan-out by not storing the response.
				clone, cerr := jrr.CloneShallow()
				if cerr != nil {
					resp = nil
					if err == nil {
						err = fmt.Errorf("response not shareable across multiplexer: %w", cerr)
					}
				} else if clone != nil {
					clone.Free() // drop our temporary ref; cloneability check only
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
