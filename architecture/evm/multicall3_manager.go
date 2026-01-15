package evm

import (
	"sync"

	"github.com/erpc/erpc/common"
)

// BatcherManager manages per-network Multicall3 batchers.
type BatcherManager struct {
	batchers map[string]*Batcher
	mu       sync.RWMutex
}

// NewBatcherManager creates a new batcher manager.
func NewBatcherManager() *BatcherManager {
	return &BatcherManager{
		batchers: make(map[string]*Batcher),
	}
}

// GetOrCreate returns the batcher for a network, creating one if needed.
func (m *BatcherManager) GetOrCreate(networkId string, cfg *common.Multicall3AggregationConfig, forwarder Forwarder) *Batcher {
	m.mu.RLock()
	if b, ok := m.batchers[networkId]; ok {
		m.mu.RUnlock()
		return b
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if b, ok := m.batchers[networkId]; ok {
		return b
	}

	batcher := NewBatcher(cfg, forwarder)
	m.batchers[networkId] = batcher
	return batcher
}

// Get returns the batcher for a network, or nil if not exists.
func (m *BatcherManager) Get(networkId string) *Batcher {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.batchers[networkId]
}

// Shutdown stops all batchers.
func (m *BatcherManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, b := range m.batchers {
		b.Shutdown()
	}
	m.batchers = make(map[string]*Batcher)
}
