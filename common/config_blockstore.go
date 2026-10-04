package common

import (
	"fmt"
	"time"
)

// EvmBlockStoreConfig configures the block store: an on-demand, per-height
// eth_getLogs cache. An explicit-range eth_getLogs of at most MaxRange blocks
// is answered by locally filtering per-height log lists that one unfiltered
// upstream eth_getLogs filled. Entries live in the connectorId (redis) when
// set, otherwise in a bounded per-replica in-memory store.
type EvmBlockStoreConfig struct {
	// Enabled opts into the block store. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled"`
	// ConnectorId optionally names a redis-driver connector declared under
	// database.evmJsonRpcCache.connectors. Replicas then share entries and
	// coalesce fills of the same range through a short Redis lock. Empty
	// keeps a bounded per-replica in-memory store.
	ConnectorId string `yaml:"connectorId,omitempty" json:"connectorId,omitempty"`
	// Namespace isolates shared entries between deployments. Defaults to
	// "default". A fingerprint of the network's upstream set is always appended.
	Namespace string `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	// MaxRange is the widest (toBlock-fromBlock+1) range handled. Wider
	// ranges take the normal path. Default 10.
	MaxRange int64 `yaml:"maxRange,omitempty" json:"maxRange,omitempty"`
	// FinalizedTTL is the lifetime of per-height entries at or below the
	// network's finalized height. Default 1h.
	FinalizedTTL Duration `yaml:"finalizedTtl,omitempty" json:"finalizedTtl,omitempty" tstype:"Duration"`
	// UnfinalizedTTL is the lifetime of per-height entries above the finalized
	// height. 0 (default) = one network block time clamped to 2s..12s, or 2s
	// while the block time is unknown.
	UnfinalizedTTL Duration `yaml:"unfinalizedTtl,omitempty" json:"unfinalizedTtl,omitempty" tstype:"Duration"`
	// EmptyTipGuard: an unfinalized height with no logs is stored only when it
	// is at least this many blocks below the network's latest head. Default 2.
	EmptyTipGuard int64 `yaml:"emptyTipGuard,omitempty" json:"emptyTipGuard,omitempty"`
	// MemoryMaxBytes bounds the in-memory store used when no connectorId is
	// configured. Default 64MB.
	MemoryMaxBytes int64 `yaml:"memoryMaxBytes,omitempty" json:"memoryMaxBytes,omitempty"`
	// PeerWait (connectorId only) is the longest a miss waits for another
	// replica already filling the same range (a short Redis lock per range)
	// before fetching itself. Waiters stop as soon as the peer finishes.
	// 0 disables cross-replica coalescing. Default 1.5s.
	PeerWait *Duration `yaml:"peerWait,omitempty" json:"peerWait,omitempty" tstype:"Duration"`
	// Concurrency bounds simultaneous per-height store reads and writes for
	// one request. Default 4.
	Concurrency int `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
}

func (c *EvmBlockStoreConfig) SetDefaults() {
	if c == nil {
		return
	}
	if c.MaxRange == 0 {
		c.MaxRange = 10
	}
	if c.FinalizedTTL == 0 {
		c.FinalizedTTL = Duration(time.Hour)
	}
	if c.EmptyTipGuard == 0 {
		c.EmptyTipGuard = 2
	}
	if c.MemoryMaxBytes == 0 {
		c.MemoryMaxBytes = 64 << 20
	}
	if c.PeerWait == nil {
		// Caps a waiter's added latency when a peer replica's fill is slow
		// or stuck. A small unfiltered eth_getLogs normally completes well
		// inside it and waiters return as soon as the peer releases its
		// lock, so it is paid in full only when the peer misbehaves. It
		// stays far below typical client timeouts.
		c.PeerWait = Duration(1500 * time.Millisecond).Ptr()
	}
	if c.Concurrency == 0 {
		c.Concurrency = 4
	}
}

func (c *EvmBlockStoreConfig) Validate() error {
	if c == nil || !c.Enabled {
		return nil
	}
	if c.MaxRange < 1 || c.MaxRange > 1000 {
		return fmt.Errorf("evm.blockStore.maxRange must be within 1..1000")
	}
	if c.FinalizedTTL <= 0 {
		return fmt.Errorf("evm.blockStore.finalizedTtl must be positive")
	}
	if c.UnfinalizedTTL < 0 {
		return fmt.Errorf("evm.blockStore.unfinalizedTtl must not be negative")
	}
	if c.EmptyTipGuard < 1 {
		return fmt.Errorf("evm.blockStore.emptyTipGuard must be at least 1")
	}
	if c.MemoryMaxBytes < 1<<20 {
		return fmt.Errorf("evm.blockStore.memoryMaxBytes must be at least 1MB")
	}
	if c.PeerWait != nil && (*c.PeerWait < 0 || *c.PeerWait > Duration(time.Minute)) {
		return fmt.Errorf("evm.blockStore.peerWait must be within 0..1m")
	}
	if c.Concurrency < 1 || c.Concurrency > 64 {
		return fmt.Errorf("evm.blockStore.concurrency must be within 1..64")
	}
	return nil
}

// NeedsConnector reports whether ConnectorId must resolve to a redis
// connector: only when the block store is enabled and a connectorId is set
// (otherwise it falls back to process memory).
func (c *EvmBlockStoreConfig) NeedsConnector() bool {
	if c == nil {
		return false
	}
	return c.Enabled && c.ConnectorId != ""
}

// ValidateConnector checks that ConnectorId references a redis connector in
// database.evmJsonRpcCache.connectors.
func (c *EvmBlockStoreConfig) ValidateConnector(cfg *Config) error {
	if cfg != nil && cfg.Database != nil && cfg.Database.EvmJsonRpcCache != nil {
		for _, conn := range cfg.Database.EvmJsonRpcCache.Connectors {
			if conn == nil || conn.Id != c.ConnectorId {
				continue
			}
			if conn.Driver != DriverRedis || conn.Redis == nil {
				return fmt.Errorf("evm.blockStore.connectorId %q must reference a redis connector, got driver %q", c.ConnectorId, conn.Driver)
			}
			return nil
		}
	}
	return fmt.Errorf("evm.blockStore.connectorId %q not found in database.evmJsonRpcCache.connectors", c.ConnectorId)
}
