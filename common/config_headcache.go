package common

import (
	"fmt"
	"time"
)

// HeadCacheModeLocal uses per-instance coordination:
// every eRPC instance follows the chain and hydrates its own window. No
// cross-instance leadership is claimed; instances may duplicate upstream
// fetches (bounded by depth and concurrency).
const HeadCacheModeLocal = "local"

// EvmHeadCacheConfig configures the head-driven full-block/log cache.
type EvmHeadCacheConfig struct {
	// Enabled turns the cache on. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled"`
	// Mode selects "local" (default) or Redis-coordinated "shared".
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Depth is how many recent canonical blocks the window holds. Default 128.
	Depth int64 `yaml:"depth,omitempty" json:"depth,omitempty"`
	// MaxBytes bounds the total serialized size (blocks + logs) held. When the
	// window would exceed it, the oldest blocks are evicted. Default 256MB.
	MaxBytes int64 `yaml:"maxBytes,omitempty" json:"maxBytes,omitempty"`
	// MaxPerTick bounds how many blocks a single follow step hydrates, so a
	// cold start or a long outage converges steadily. Default 16.
	MaxPerTick int64 `yaml:"maxPerTick,omitempty" json:"maxPerTick,omitempty"`
	// Concurrency bounds in-flight upstream fetches per network. Default 4.
	Concurrency int `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
	// PollInterval is the fallback tick that re-verifies the tip hash even
	// when the height has not changed (same-height reorgs). Default 2s.
	PollInterval Duration `yaml:"pollInterval,omitempty" json:"pollInterval,omitempty" tstype:"Duration"`
	// FetchTimeout bounds each hydration fetch, additionally capped by the
	// whole-tick lease deadline. Default 10s.
	FetchTimeout Duration `yaml:"fetchTimeout,omitempty" json:"fetchTimeout,omitempty" tstype:"Duration"`
	// MaxLogsRange caps the block span an eth_getLogs range may have to be
	// served from the cache. Wider ranges go upstream. Default = Depth.
	MaxLogsRange int64 `yaml:"maxLogsRange,omitempty" json:"maxLogsRange,omitempty"`
	// MaxBlockBytes rejects (never caches) any single block whose block+logs
	// payload exceeds it. Default 16MB.
	MaxBlockBytes int64 `yaml:"maxBlockBytes,omitempty" json:"maxBlockBytes,omitempty"`
	// MaxStaleness disables serving (normal upstream path) when the local
	// view has not been verified for this long. Followers anchor freshness to
	// the snapshot's writer timestamp, clamped to local receipt time.
	// Default 5 * pollInterval.
	MaxStaleness Duration `yaml:"maxStaleness,omitempty" json:"maxStaleness,omitempty" tstype:"Duration"`
	// Namespace isolates shared state between deployments. Defaults to
	// "default". A fingerprint of the
	// network's upstream set is always appended, so replicas only share data
	// when they run the same upstream configuration.
	Namespace string `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	// Redis is required when mode is "shared". Only the URI (and optional
	// TLS) of the connector config are used.
	Redis *RedisConnectorConfig `yaml:"redis,omitempty" json:"redis,omitempty"`
	// LeaseTTL is the shared-mode leadership lease. It is renewed every
	// tick. Coordination and all batch work share a deadline of 80% of this
	// TTL so publication cannot outlive the lease. Must be > 2*pollInterval.
	// Default 3*pollInterval + 1s.
	LeaseTTL Duration `yaml:"leaseTtl,omitempty" json:"leaseTtl,omitempty" tstype:"Duration"`
	// HeadSource selects which tip the window publishes up to:
	//   "served" (default): cap at the network's served latest tip when it is
	//     known and fresh, so subscribers never see blocks that HTTP `latest`
	//     would not yet return. When the served tip is unknown (cold pollers)
	//     or stale (dormant pollers), live eth_blockNumber discovery is used so
	//     the window cannot freeze.
	//   "max": publish up to the live-discovered head (pre-existing behavior).
	HeadSource string `yaml:"headSource,omitempty" json:"headSource,omitempty"`
	// ServedTipMaxAge is how long the served tip may lag live discovery
	// (measured from lag onset, reset when served advances or catches up)
	// before live is used uncapped. Default 3*pollInterval.
	ServedTipMaxAge Duration `yaml:"servedTipMaxAge,omitempty" json:"servedTipMaxAge,omitempty" tstype:"Duration"`
}

const (
	HeadCacheHeadSourceServed = "served"
	HeadCacheHeadSourceMax    = "max"
)

// HeadCacheModeShared coordinates replicas through Redis: one lease holder
// hydrates and publishes epoch-fenced snapshots, every replica serves them.
const HeadCacheModeShared = "shared"

func (c *EvmHeadCacheConfig) SetDefaults() {
	if c == nil {
		return
	}
	if c.Mode == "" {
		c.Mode = HeadCacheModeLocal
	}
	if c.Depth == 0 {
		c.Depth = 128
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 256 << 20
	}
	if c.MaxPerTick == 0 {
		c.MaxPerTick = 16
	}
	if c.Concurrency == 0 {
		c.Concurrency = 4
	}
	if c.PollInterval == 0 {
		c.PollInterval = Duration(2 * time.Second)
	}
	if c.FetchTimeout == 0 {
		c.FetchTimeout = Duration(10 * time.Second)
	}
	if c.MaxLogsRange == 0 {
		c.MaxLogsRange = c.Depth
	}
	if c.MaxBlockBytes == 0 {
		c.MaxBlockBytes = 16 << 20
	}
	if c.MaxStaleness == 0 {
		c.MaxStaleness = Duration(5 * c.PollInterval.Duration())
	}
	if c.LeaseTTL == 0 {
		c.LeaseTTL = Duration(3*c.PollInterval.Duration() + time.Second)
	}
	if c.HeadSource == "" {
		c.HeadSource = HeadCacheHeadSourceServed
	}
	if c.ServedTipMaxAge == 0 {
		c.ServedTipMaxAge = Duration(3 * c.PollInterval.Duration())
	}
}

func (c *EvmHeadCacheConfig) Validate() error {
	if c == nil || !c.Enabled {
		return nil
	}
	switch c.Mode {
	case HeadCacheModeLocal:
	case HeadCacheModeShared:
		// Shared mode never silently degrades to local hydration.
		if c.Redis == nil || c.Redis.URI == "" {
			return fmt.Errorf("evm.headCache.redis.uri is required when mode is %q", HeadCacheModeShared)
		}
	default:
		return fmt.Errorf("evm.headCache.mode %q is not supported (use %q or %q)", c.Mode, HeadCacheModeLocal, HeadCacheModeShared)
	}
	if c.Depth < 1 || c.Depth > 4096 {
		return fmt.Errorf("evm.headCache.depth must be within 1..4096")
	}
	if c.MaxBytes < 1<<20 {
		return fmt.Errorf("evm.headCache.maxBytes must be at least 1MB")
	}
	if c.MaxPerTick < 1 || c.MaxPerTick > c.Depth {
		return fmt.Errorf("evm.headCache.maxPerTick must be within 1..depth")
	}
	if c.Concurrency < 1 || c.Concurrency > 64 {
		return fmt.Errorf("evm.headCache.concurrency must be within 1..64")
	}
	if c.PollInterval <= 0 || c.FetchTimeout <= 0 {
		return fmt.Errorf("evm.headCache.pollInterval and fetchTimeout must be positive")
	}
	if c.MaxLogsRange < 1 || c.MaxLogsRange > c.Depth {
		return fmt.Errorf("evm.headCache.maxLogsRange must be within 1..depth")
	}
	if c.MaxBlockBytes < 1024 || c.MaxBlockBytes > c.MaxBytes {
		return fmt.Errorf("evm.headCache.maxBlockBytes must be within 1KB..maxBytes")
	}
	if c.MaxStaleness < c.PollInterval {
		return fmt.Errorf("evm.headCache.maxStaleness must be >= pollInterval")
	}
	if c.PollInterval.Duration() >= c.LeaseTTL.Duration()/2 {
		return fmt.Errorf("evm.headCache.pollInterval must be < leaseTtl/2")
	}
	if c.HeadSource != HeadCacheHeadSourceServed && c.HeadSource != HeadCacheHeadSourceMax {
		return fmt.Errorf("evm.headCache.headSource %q is not supported (use %q or %q)", c.HeadSource, HeadCacheHeadSourceServed, HeadCacheHeadSourceMax)
	}
	if c.ServedTipMaxAge < 0 {
		return fmt.Errorf("evm.headCache.servedTipMaxAge must be >= 0")
	}
	return nil
}

// WebSocketServerConfig configures the JSON-RPC WebSocket endpoint. It is
// served on the same port/paths as HTTP (/<project>/evm/<chainId>) when a
// client sends an Upgrade request.
type WebSocketServerConfig struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled"`
	// MaxConnections bounds concurrent WS connections per server. Default 1024.
	MaxConnections int `yaml:"maxConnections,omitempty" json:"maxConnections,omitempty"`
	// MaxConnectionsPerProject bounds concurrent WS connections per project so
	// one tenant cannot exhaust MaxConnections. 0 = only the global cap.
	MaxConnectionsPerProject int `yaml:"maxConnectionsPerProject,omitempty" json:"maxConnectionsPerProject,omitempty"`
	// MaxSubscriptionsPerConnection. Default 32.
	MaxSubscriptionsPerConnection int `yaml:"maxSubscriptionsPerConnection,omitempty" json:"maxSubscriptionsPerConnection,omitempty"`
	// SendQueueSize bounds queued outbound messages per connection. A client
	// that falls this far behind is disconnected (policy violation) rather
	// than buffered without bound. Default 256.
	SendQueueSize int `yaml:"sendQueueSize,omitempty" json:"sendQueueSize,omitempty"`
	// MaxMessageBytes caps inbound frame size. Default 1MB.
	MaxMessageBytes int64 `yaml:"maxMessageBytes,omitempty" json:"maxMessageBytes,omitempty"`
	// WriteTimeout bounds each outbound write. Default 10s.
	WriteTimeout Duration `yaml:"writeTimeout,omitempty" json:"writeTimeout,omitempty" tstype:"Duration"`
	// MaxInflightPerConnection bounds concurrently handled requests per
	// connection. Default 16.
	MaxInflightPerConnection int `yaml:"maxInflightPerConnection,omitempty" json:"maxInflightPerConnection,omitempty"`
	// PingInterval is the keepalive ping period. Default 30s.
	PingInterval Duration `yaml:"pingInterval,omitempty" json:"pingInterval,omitempty" tstype:"Duration"`
	// MaxBatchSize caps JSON-RPC batch length over WS. Default 100.
	MaxBatchSize int `yaml:"maxBatchSize,omitempty" json:"maxBatchSize,omitempty"`
}

func (c *WebSocketServerConfig) SetDefaults() {
	if c == nil {
		return
	}
	if c.MaxConnections == 0 {
		c.MaxConnections = 1024
	}
	if c.MaxSubscriptionsPerConnection == 0 {
		c.MaxSubscriptionsPerConnection = 32
	}
	if c.SendQueueSize == 0 {
		c.SendQueueSize = 256
	}
	if c.MaxMessageBytes == 0 {
		c.MaxMessageBytes = 1 << 20
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = Duration(10 * time.Second)
	}
	if c.MaxInflightPerConnection == 0 {
		c.MaxInflightPerConnection = 16
	}
	if c.PingInterval == 0 {
		c.PingInterval = Duration(30 * time.Second)
	}
	if c.MaxBatchSize == 0 {
		c.MaxBatchSize = 100
	}
}

func (c *WebSocketServerConfig) Validate() error {
	if c == nil || !c.Enabled {
		return nil
	}
	if c.MaxConnections < 1 || c.MaxSubscriptionsPerConnection < 1 || c.SendQueueSize < 1 || c.MaxMessageBytes < 1024 || c.WriteTimeout <= 0 {
		return fmt.Errorf("server.webSocket limits must be positive (maxMessageBytes >= 1024)")
	}
	if c.MaxConnectionsPerProject < 0 || c.MaxConnectionsPerProject > c.MaxConnections {
		return fmt.Errorf("server.webSocket.maxConnectionsPerProject must be within 0..maxConnections")
	}
	if c.MaxInflightPerConnection < 1 || c.PingInterval <= 0 || c.MaxBatchSize < 1 {
		return fmt.Errorf("server.webSocket maxInflightPerConnection, pingInterval and maxBatchSize must be positive")
	}
	return nil
}
