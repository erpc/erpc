package erpc

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
)

// Multiplexer lifecycle tracking
var (
	multiplexersCreated    atomic.Int64
	multiplexersClosed     atomic.Int64
	multiplexersActive     atomic.Int64
	multiplexerTrackerOnce sync.Once
	activeMuxMap           sync.Map // hash -> *multiplexerMeta
)

type multiplexerMeta struct {
	hash      string
	createdAt time.Time
}

func initMultiplexerTracker() {
	multiplexerTrackerOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				created := multiplexersCreated.Load()
				closed := multiplexersClosed.Load()
				active := multiplexersActive.Load()

				// Find oldest active multiplexers
				var oldestAge time.Duration
				var oldestHash string
				var stuckCount int
				now := time.Now()
				threshold := 30 * time.Second // Consider stuck if older than 30s

				activeMuxMap.Range(func(key, value interface{}) bool {
					meta := value.(*multiplexerMeta)
					age := now.Sub(meta.createdAt)
					if age > oldestAge {
						oldestAge = age
						oldestHash = meta.hash
					}
					if age > threshold {
						stuckCount++
					}
					return true
				})

				logger := log.Info().
					Int64("created_last_minute", created).
					Int64("closed_last_minute", closed).
					Int64("currently_active", active).
					Int64("delta_last_minute", created-closed)

				if stuckCount > 0 {
					logger = logger.
						Int("stuck_count", stuckCount).
						Str("oldest_hash", oldestHash).
						Dur("oldest_age", oldestAge)
				}

				logger.Msg("multiplexer lifecycle stats")

				// Reset minute counters
				multiplexersCreated.Store(0)
				multiplexersClosed.Store(0)
			}
		}()
	})
}

type Multiplexer struct {
	hash   string
	resp   *common.NormalizedResponse
	err    error
	done   chan struct{}
	mu     *sync.RWMutex
	once   sync.Once
	closed atomic.Bool
}

func NewMultiplexer(hash string) *Multiplexer {
	initMultiplexerTracker()
	multiplexersCreated.Add(1)
	multiplexersActive.Add(1)

	// Track this multiplexer
	activeMuxMap.Store(hash, &multiplexerMeta{
		hash:      hash,
		createdAt: time.Now(),
	})

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

// Release frees the stored response if any
func (m *Multiplexer) Release() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Track that this multiplexer is being cleaned up
	if !m.closed.Load() {
		m.closed.Store(true)
		multiplexersClosed.Add(1)
		multiplexersActive.Add(-1)
		// Remove from active tracking map
		activeMuxMap.Delete(m.hash)
	}

	if m.resp != nil {
		m.resp.Release()
		m.resp = nil
	}
}
