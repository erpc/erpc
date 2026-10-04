package common

import (
	"fmt"
	"time"
)

// EvmBlockStoreConfig configures the head-driven block/log cache. The live
// window verifies only headers in the background; block bodies and logs are
// fetched on demand.
type EvmBlockStoreConfig struct {
	// Enabled turns the cache on. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled"`
	// ConnectorId names a redis-driver connector declared under
	// database.evmJsonRpcCache.connectors. Fleet mode elects one lease holder to
	// verify and publish a canonical snapshot; followers consume that snapshot.
	// Required.
	ConnectorId string `yaml:"connectorId,omitempty" json:"connectorId,omitempty"`
	// Depth is how many recent canonical blocks the window holds. Default 128.
	Depth int64 `yaml:"depth,omitempty" json:"depth,omitempty"`
	// MaxBytes bounds the process-local cache of on-demand block bodies and
	// log lists (headers are not counted). When exceeded, payloads of the
	// lowest heights are evicted and reloaded from Redis on demand. Default 256MB.
	MaxBytes int64 `yaml:"maxBytes,omitempty" json:"maxBytes,omitempty"`
	// MaxPerTick bounds how many older headers a single refresh backfills
	// while the window is shorter than Depth (cold start, recovery). New heads
	// above the window are always fetched in full. Default min(16, depth).
	MaxPerTick int64 `yaml:"maxPerTick,omitempty" json:"maxPerTick,omitempty"`
	// Concurrency bounds simultaneous header fetches and per-height log
	// loads of one request per network. Default 4.
	Concurrency int `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
	// PollInterval is how often the lease holder checks the in-memory latest
	// block and fetches headers for new heights (a state-poller advance also
	// triggers an early check). An unchanged tip costs no upstream call; a
	// replaced tip is detected when the next block does not link to it, or
	// when an on-demand body fetch disagrees with the window. Default 2s.
	PollInterval Duration `yaml:"pollInterval,omitempty" json:"pollInterval,omitempty" tstype:"Duration"`
	// FetchTimeout bounds each header, block or logs fetch. Default 10s.
	FetchTimeout Duration `yaml:"fetchTimeout,omitempty" json:"fetchTimeout,omitempty" tstype:"Duration"`
	// MaxLogsRange caps the block span an eth_getLogs range may have to be
	// served from the cache. Wider ranges go upstream. Default = Depth.
	MaxLogsRange int64 `yaml:"maxLogsRange,omitempty" json:"maxLogsRange,omitempty"`
	// MaxBlockBytes rejects (never caches) any single block body or log list
	// larger than it. Default min(16MB, maxBytes).
	MaxBlockBytes int64 `yaml:"maxBlockBytes,omitempty" json:"maxBlockBytes,omitempty"`
	// MaxStaleness disables serving (normal upstream path) when this replica's
	// view has not been verified for this long. Default 5 * pollInterval.
	MaxStaleness Duration `yaml:"maxStaleness,omitempty" json:"maxStaleness,omitempty" tstype:"Duration"`
	// Namespace isolates shared payloads between deployments. Defaults to
	// "default". A fingerprint of the network's upstream set is always appended.
	Namespace string `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	// Historical configures the independent cache for finalized blocks and complete logs.
	Historical EvmBlockStoreHistoricalConfig `yaml:"historical,omitempty" json:"historical,omitempty"`
	// LogsFill configures the standalone small-range eth_getLogs fill. It works
	// without the live window (enabled) or historical cache.
	LogsFill EvmBlockStoreLogsFillConfig `yaml:"logsFill,omitempty" json:"logsFill,omitempty"`
}

// EvmBlockStoreLogsFillConfig configures the small-range eth_getLogs fill: an
// explicit-range request of at most MaxRange blocks is answered by locally
// filtering per-block log lists that one unfiltered upstream eth_getLogs
// filled. Stored in the blockStore connectorId (redis) when set, otherwise in a
// bounded per-network in-memory cache.
type EvmBlockStoreLogsFillConfig struct {
	// Enabled opts into the fill. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled"`
	// MaxRange is the widest (toBlock-fromBlock+1) range handled. Wider
	// ranges take the normal path. Default 10.
	MaxRange int64 `yaml:"maxRange,omitempty" json:"maxRange,omitempty"`
	// FinalizedTTL is the lifetime of per-block entries at or below the
	// network's finalized height. Default 1h.
	FinalizedTTL Duration `yaml:"finalizedTtl,omitempty" json:"finalizedTtl,omitempty" tstype:"Duration"`
	// UnfinalizedTTL is the lifetime of per-block entries above the finalized
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
}

func (c *EvmBlockStoreLogsFillConfig) SetDefaults() {
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
}

func (c *EvmBlockStoreLogsFillConfig) Validate() error {
	if c == nil || !c.Enabled {
		return nil
	}
	if c.MaxRange < 1 || c.MaxRange > 1000 {
		return fmt.Errorf("evm.blockStore.logsFill.maxRange must be within 1..1000")
	}
	if c.FinalizedTTL <= 0 {
		return fmt.Errorf("evm.blockStore.logsFill.finalizedTtl must be positive")
	}
	if c.UnfinalizedTTL < 0 {
		return fmt.Errorf("evm.blockStore.logsFill.unfinalizedTtl must not be negative")
	}
	if c.EmptyTipGuard < 1 {
		return fmt.Errorf("evm.blockStore.logsFill.emptyTipGuard must be at least 1")
	}
	if c.MemoryMaxBytes < 1<<20 {
		return fmt.Errorf("evm.blockStore.logsFill.memoryMaxBytes must be at least 1MB")
	}
	if c.PeerWait != nil && (*c.PeerWait < 0 || *c.PeerWait > Duration(time.Minute)) {
		return fmt.Errorf("evm.blockStore.logsFill.peerWait must be within 0..1m")
	}
	return nil
}

// EvmBlockStoreHistoricalConfig configures the independent finalized-block and complete-log cache.
type EvmBlockStoreHistoricalConfig struct {
	// Enabled opts into storing finalized full blocks independently of the live window. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled"`
	// TTL is how long historical records remain eligible for reuse. Default 1h.
	TTL Duration `yaml:"ttl,omitempty" json:"ttl,omitempty" tstype:"Duration"`
}

func (c *EvmBlockStoreConfig) SetDefaults() {
	if c == nil {
		return
	}
	c.Historical.SetDefaults()
	c.LogsFill.SetDefaults()
	if c.Depth == 0 {
		c.Depth = 128
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 256 << 20
	}
	if c.MaxPerTick == 0 {
		// Validate bounds maxPerTick by depth, so a small explicit depth must not
		// inherit a default above it (the runtime clamps the same way).
		c.MaxPerTick = min(16, c.Depth)
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
}

func (c *EvmBlockStoreHistoricalConfig) SetDefaults() {
	if c == nil {
		return
	}
	if c.TTL == 0 {
		c.TTL = Duration(time.Hour)
	}
}

func (c *EvmBlockStoreConfig) Validate() error {
	if c == nil {
		return nil
	}
	if err := c.Historical.Validate(); err != nil {
		return err
	}
	if err := c.LogsFill.Validate(); err != nil {
		return err
	}
	if c.LogsFill.Enabled && c.FetchTimeout <= 0 {
		return fmt.Errorf("evm.blockStore.fetchTimeout must be positive")
	}
	if !c.Enabled && !c.Historical.Enabled {
		return nil
	}
	if c.ConnectorId == "" {
		return fmt.Errorf("evm.blockStore.connectorId is required (a redis connector under database.evmJsonRpcCache.connectors)")
	}
	if c.Depth < 1 || c.Depth > 4096 {
		return fmt.Errorf("evm.blockStore.depth must be within 1..4096")
	}
	if c.MaxBytes < 1<<20 {
		return fmt.Errorf("evm.blockStore.maxBytes must be at least 1MB")
	}
	if c.Concurrency < 1 || c.Concurrency > 64 {
		return fmt.Errorf("evm.blockStore.concurrency must be within 1..64")
	}
	if c.FetchTimeout <= 0 {
		return fmt.Errorf("evm.blockStore.fetchTimeout must be positive")
	}
	if c.MaxLogsRange < 1 || c.MaxLogsRange > c.Depth {
		return fmt.Errorf("evm.blockStore.maxLogsRange must be within 1..depth")
	}
	if c.MaxBlockBytes < 1024 || c.MaxBlockBytes > c.MaxBytes {
		return fmt.Errorf("evm.blockStore.maxBlockBytes must be within 1KB..maxBytes")
	}
	if !c.Enabled {
		return nil
	}
	if c.MaxPerTick < 1 || c.MaxPerTick > c.Depth {
		return fmt.Errorf("evm.blockStore.maxPerTick must be within 1..depth")
	}
	if c.PollInterval <= 0 {
		return fmt.Errorf("evm.blockStore.pollInterval must be positive")
	}
	if c.MaxStaleness < c.PollInterval {
		return fmt.Errorf("evm.blockStore.maxStaleness must be >= pollInterval")
	}
	return nil
}

func (c *EvmBlockStoreHistoricalConfig) Validate() error {
	if c == nil || !c.Enabled {
		return nil
	}
	if c.TTL <= 0 {
		return fmt.Errorf("evm.blockStore.historical.ttl must be positive")
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

// NeedsConnector reports whether ConnectorId must resolve to a redis
// connector: the live window and historical cache require one; the logs fill
// uses it only when set (otherwise it falls back to process memory).
func (c *EvmBlockStoreConfig) NeedsConnector() bool {
	if c == nil {
		return false
	}
	return c.Enabled || c.Historical.Enabled || (c.LogsFill.Enabled && c.ConnectorId != "")
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
