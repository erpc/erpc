package evm

import (
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// BatcherManager manages per-project+network Multicall3 batchers.
// It provides thread-safe access to batchers keyed by "projectId|networkId".
// Each batcher handles batching for a specific project and network combination
// to ensure proper isolation between projects.
type BatcherManager struct {
	batchers map[string]*Batcher // Key: "projectId|networkId"
	mu       sync.RWMutex
}

// NewBatcherManager creates a new batcher manager.
func NewBatcherManager() *BatcherManager {
	return &BatcherManager{
		batchers: make(map[string]*Batcher),
	}
}

// GetOrCreate returns the batcher for a project+network, creating one if needed.
// The key combines projectId and networkId to ensure project isolation.
// Returns nil if batching is disabled (cfg is nil or cfg.Enabled is false).
// The logger parameter is optional (can be nil) - if nil, debug logging is disabled.
func (m *BatcherManager) GetOrCreate(projectId, networkId string, cfg *common.Multicall3AggregationConfig, forwarder Forwarder, logger *zerolog.Logger) *Batcher {
	// Use null byte separator to prevent key collisions from field values containing common separators
	key := projectId + "\x00" + networkId

	m.mu.RLock()
	if b, ok := m.batchers[key]; ok {
		m.mu.RUnlock()
		return b
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if b, ok := m.batchers[key]; ok {
		return b
	}

	batcher := NewBatcher(cfg, forwarder, logger)
	if batcher == nil {
		// Don't store nil batchers - batching is disabled for this config
		return nil
	}
	m.batchers[key] = batcher
	return batcher
}

// Get returns the batcher for a project+network, or nil if not exists.
func (m *BatcherManager) Get(projectId, networkId string) *Batcher {
	key := projectId + "\x00" + networkId
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.batchers[key]
}

// Shutdown stops all batchers.
func (m *BatcherManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, b := range m.batchers {
		if b != nil {
			b.Shutdown()
		}
	}
	m.batchers = make(map[string]*Batcher)
}
