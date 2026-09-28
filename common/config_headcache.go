package common

import (
	"fmt"
	"time"
)

// EvmHeadCacheConfig configures the head-driven full-block/log cache.
type EvmHeadCacheConfig struct {
	// Enabled turns the cache on. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled"`
	// ConnectorId names a redis-driver connector declared under
	// database.evmJsonRpcCache.connectors. Its Redis client holds the shared
	// canonical window: one lease holder hydrates and publishes epoch-fenced
	// snapshots, every replica serves them. Required.
	ConnectorId string `yaml:"connectorId,omitempty" json:"connectorId,omitempty"`
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
	// payload exceeds it. Default min(16MB, maxBytes).
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
	// LeaseTTL is the hydration leadership lease. It is renewed every
	// tick. Coordination and all batch work share a deadline of 80% of this
	// TTL so publication cannot outlive the lease. Must be > 2*pollInterval.
	// Default 3*pollInterval + 1s.
	LeaseTTL Duration `yaml:"leaseTtl,omitempty" json:"leaseTtl,omitempty" tstype:"Duration"`
}

func (c *EvmHeadCacheConfig) SetDefaults() {
	if c == nil {
		return
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
		c.MaxBlockBytes = min(int64(16<<20), c.MaxBytes)
	}
	if c.MaxStaleness == 0 {
		c.MaxStaleness = Duration(5 * c.PollInterval.Duration())
	}
	if c.LeaseTTL == 0 {
		c.LeaseTTL = Duration(3*c.PollInterval.Duration() + time.Second)
	}
}

func (c *EvmHeadCacheConfig) Validate() error {
	if c == nil || !c.Enabled {
		return nil
	}
	if c.ConnectorId == "" {
		return fmt.Errorf("evm.headCache.connectorId is required (a redis connector under database.evmJsonRpcCache.connectors)")
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
	// PingInterval is the keepalive ping period. Default 30s.
	PingInterval Duration `yaml:"pingInterval,omitempty" json:"pingInterval,omitempty" tstype:"Duration"`
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
	if c.PingInterval == 0 {
		c.PingInterval = Duration(30 * time.Second)
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
	if c.PingInterval <= 0 {
		return fmt.Errorf("server.webSocket.pingInterval must be positive")
	}
	return nil
}

// ValidateConnector checks that ConnectorId references a redis connector in
// database.evmJsonRpcCache.connectors.
func (c *EvmHeadCacheConfig) ValidateConnector(cfg *Config) error {
	if cfg != nil && cfg.Database != nil && cfg.Database.EvmJsonRpcCache != nil {
		for _, conn := range cfg.Database.EvmJsonRpcCache.Connectors {
			if conn == nil || conn.Id != c.ConnectorId {
				continue
			}
			if conn.Driver != DriverRedis || conn.Redis == nil {
				return fmt.Errorf("evm.headCache.connectorId %q must reference a redis connector, got driver %q", c.ConnectorId, conn.Driver)
			}
			return nil
		}
	}
	return fmt.Errorf("evm.headCache.connectorId %q not found in database.evmJsonRpcCache.connectors", c.ConnectorId)
}
