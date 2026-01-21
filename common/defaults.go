package common

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
)

// MatchFinalities checks if two finality arrays match.
// Empty arrays (nil or len=0) match any finality.
func MatchFinalities(finalities1, finalities2 []DataFinalityState) bool {
	// If either is empty, they match any finality
	if len(finalities1) == 0 || len(finalities2) == 0 {
		return true
	}

	// Check if there's any overlap between the two arrays
	for _, f1 := range finalities1 {
		for _, f2 := range finalities2 {
			if f1 == f2 {
				return true
			}
		}
	}
	return false
}

type connectorScope string

const (
	connectorScopeSharedState connectorScope = "shared-state"
	connectorScopeCache       connectorScope = "cache"
	connectorScopeAuth        connectorScope = "auth"
)

// DefaultOptions is used to pass env-provided or args-provided options to the config defaults initializer
type DefaultOptions struct {
	Endpoints []string
}

func (c *Config) SetDefaults(opts *DefaultOptions) error {
	if c.LogLevel == "" {
		c.LogLevel = "INFO"
	}
	if c.ClusterKey == "" {
		c.ClusterKey = "erpc-default"
	}
	if c.Server == nil {
		c.Server = &ServerConfig{}
	}
	if err := c.Server.SetDefaults(); err != nil {
		return err
	}
	if c.HealthCheck == nil {
		c.HealthCheck = &HealthCheckConfig{}
	}
	if err := c.HealthCheck.SetDefaults(); err != nil {
		return err
	}
	if c.Tracing != nil {
		if err := c.Tracing.SetDefaults(); err != nil {
			return err
		}
	}

	if c.Database != nil {
		if err := c.Database.SetDefaults(c.ClusterKey); err != nil {
			return err
		}
	}

	if c.Metrics == nil {
		c.Metrics = &MetricsConfig{}
	}
	if err := c.Metrics.SetDefaults(); err != nil {
		return err
	}

	if c.Admin != nil {
		if err := c.Admin.SetDefaults(); err != nil {
			return err
		}
	}

	if c.Projects != nil {
		for _, project := range c.Projects {
			if err := project.SetDefaults(opts); err != nil {
				return err
			}
		}
	}
	if len(c.Projects) == 0 {
		log.Warn().Msg("no projects found in config; will add a default 'main' project")
		c.Server.Aliasing = &AliasingConfig{
			// Since we're adding only 1 project let's add an aliasing rule so users can send requests to /evm/123 without specifying the project id (/main/evm/123)
			Rules: []*AliasingRuleConfig{
				{
					MatchDomain:  "*",
					ServeProject: "main",
				},
			},
		}
		c.Projects = []*ProjectConfig{
			{
				Id: "main",
				NetworkDefaults: &NetworkDefaults{
					Evm: &EvmNetworkConfig{
						Integrity: &EvmIntegrityConfig{
							EnforceHighestBlock:        util.BoolPtr(true),
							EnforceGetLogsBlockRange:   util.BoolPtr(true),
							EnforceNonNullTaggedBlocks: util.BoolPtr(true),
						},
						GetLogsMaxAllowedRange: 30_000,
						GetLogsSplitOnError:    util.BoolPtr(true),
					},
					Failsafe: []*FailsafeConfig{
						{
							MatchMethod: "*",
							Retry: &RetryPolicyConfig{
								MaxAttempts:     5,
								Delay:           Duration(0),
								Jitter:          Duration(0),
								BackoffMaxDelay: Duration(0),
								BackoffFactor:   1.0,
							},
							Timeout: &TimeoutPolicyConfig{
								Duration: Duration(120 * time.Second),
							},
							Hedge: &HedgePolicyConfig{
								Quantile: 0.7,
								MaxCount: 2,
							},
						},
					},
				},
				UpstreamDefaults: &UpstreamConfig{
					Evm: &EvmUpstreamConfig{
						GetLogsAutoSplittingRangeThreshold: 5000,
					},
					Failsafe: []*FailsafeConfig{
						{
							MatchMethod: "*",
							Retry: &RetryPolicyConfig{
								MaxAttempts: 1,
								Delay:       Duration(500 * time.Millisecond),
							},
							Timeout: &TimeoutPolicyConfig{
								Duration: Duration(60 * time.Second),
							},
						},
					},
				},
			},
		}
		err := c.Projects[0].SetDefaults(opts)
		if err != nil {
			return err
		}
	}

	if c.RateLimiters != nil {
		if err := c.RateLimiters.SetDefaults(); err != nil {
			return err
		}
	}

	return nil
}

var DefaultStatefulMethodNames = []string{
	"eth_newFilter",
	"eth_newBlockFilter",
	"eth_newPendingTransactionFilter",
	"eth_getFilterChanges",
	"eth_getFilterLogs",
	"eth_uninstallFilter",
}

// These methods return a fixed value that does not change over time
var DefaultStaticCacheMethods = map[string]*CacheMethodConfig{
	"eth_chainId": {
		Finalized: true,
	},
	"net_version": {
		Finalized: true,
	},
}

// These methods return a value that changes in realtime (e.g. per block)
var DefaultRealtimeCacheMethods = map[string]*CacheMethodConfig{
	"eth_hashrate": {
		Realtime: true,
	},
	"eth_mining": {
		Realtime: true,
	},
	"eth_syncing": {
		Realtime: true,
	},
	"net_peerCount": {
		Realtime: true,
	},
	"eth_gasPrice": {
		Realtime: true,
	},
	"eth_maxPriorityFeePerGas": {
		Realtime: true,
	},
	"eth_blobBaseFee": {
		Realtime: true,
	},
	"eth_blockNumber": {
		Realtime: true,
		RespRefs: [][]interface{}{
			{}, // Means response is a direct hex string for block number
		},
	},
	"erigon_blockNumber": {
		Realtime: true,
		RespRefs: [][]interface{}{
			{}, // Means response is a direct hex string for block number
		},
	},
}

// Common path references to where to find the block number, tag or hash in the request
var FirstParam = [][]interface{}{
	{0},
}
var SecondParam = [][]interface{}{
	{1},
}
var ThirdParam = [][]interface{}{
	{2},
}
var NumberOrHashParam = [][]interface{}{
	{"number"},
	{"hash"},
}
var BlockNumberOrBlockHashParam = [][]interface{}{
	{"blockNumber"},
	{"blockHash"},
}

// This special case of "*" is used for methods that can be cached regardless of their block number or hash
var ArbitraryBlock = [][]interface{}{
	{"*"},
}

// These methods always reference block number, tag or hash in their request (and sometimes in response)
var DefaultWithBlockCacheMethods = map[string]*CacheMethodConfig{
	"eth_getLogs": {
		ReqRefs: [][]interface{}{
			{0, "fromBlock"},
			{0, "toBlock"},
			{0, "blockHash"},
		},
		// evm/eth_getLogs.go hook already enforces lower/upper-bound against per-upstream latest/finality, so we don't need to enforce it here.
		EnforceBlockAvailability: util.BoolPtr(false),
	},
	"eth_getBlockByHash": {
		ReqRefs:  FirstParam,
		RespRefs: NumberOrHashParam,
	},
	"eth_getBlockByNumber": {
		ReqRefs:  FirstParam,
		RespRefs: NumberOrHashParam,
		// evm/eth_getBlockByNumber.go hook already enforces lower/upper-bound against per-upstream latest/finality, so we don't need to enforce it here.
		EnforceBlockAvailability: util.BoolPtr(false),
		// Don't interpolate "latest"/"finalized" tags for this method - it should fetch actual
		// current state from upstream. This method is the source of truth for block tags,
		// not a consumer of interpolated values. Other methods (eth_getLogs, eth_call, etc.)
		// will still interpolate tags using the state poller's highest known block.
		TranslateLatestTag:    util.BoolPtr(false),
		TranslateFinalizedTag: util.BoolPtr(false),
	},
	"eth_getTransactionByBlockHashAndIndex": {
		ReqRefs:  FirstParam,
		RespRefs: BlockNumberOrBlockHashParam,
	},
	"eth_getTransactionByBlockNumberAndIndex": {
		ReqRefs:  FirstParam,
		RespRefs: BlockNumberOrBlockHashParam,
	},
	"eth_getUncleByBlockHashAndIndex": {
		ReqRefs:  FirstParam,
		RespRefs: NumberOrHashParam,
	},
	"eth_getUncleByBlockNumberAndIndex": {
		ReqRefs:  FirstParam,
		RespRefs: NumberOrHashParam,
	},
	"eth_getBlockTransactionCountByHash": {
		ReqRefs: FirstParam,
	},
	"eth_getBlockTransactionCountByNumber": {
		ReqRefs: FirstParam,
	},
	"eth_getUncleCountByBlockHash": {
		ReqRefs: FirstParam,
	},
	"eth_getUncleCountByBlockNumber": {
		ReqRefs: FirstParam,
	},
	"eth_getStorageAt": {
		ReqRefs: ThirdParam,
	},
	"eth_getBalance": {
		ReqRefs: SecondParam,
	},
	"eth_getTransactionCount": {
		ReqRefs: SecondParam,
	},
	"eth_getCode": {
		ReqRefs: SecondParam,
	},
	"eth_call": {
		ReqRefs: SecondParam,
	},
	"eth_estimateGas": {
		ReqRefs: SecondParam,
	},
	"trace_call": {
		// Support both param orderings used in the wild:
		// - second param as block tag/number (e.g., ["latest"])
		// - third param as block tag/number when second param is trace types/config
		ReqRefs: [][]interface{}{
			{1}, // second param
			{2}, // third param
		},
	},
	"debug_traceCall": {
		ReqRefs: SecondParam,
	},
	"eth_getProof": {
		ReqRefs: ThirdParam,
	},
	"arbtrace_call": {
		ReqRefs: ThirdParam,
	},
	"eth_feeHistory": {
		ReqRefs: SecondParam,
	},
	"eth_getAccount": {
		ReqRefs: SecondParam,
	},
	"eth_simulateV1": {
		ReqRefs: SecondParam,
	},
	"erigon_getBlockByTimestamp": {
		ReqRefs: SecondParam,
	},
	"arbtrace_callMany": {
		ReqRefs: SecondParam,
	},
	"eth_getBlockReceipts": {
		ReqRefs: [][]interface{}{
			{0},
			{0, "blockHash"},
			{0, "blockNumber"},
		},
		RespRefs: [][]interface{}{
			{0, "blockHash"},
			{0, "blockNumber"},
		},
	},
	"trace_block": {
		ReqRefs: FirstParam,
	},
	"trace_filter": {
		ReqRefs: [][]interface{}{
			{0, "fromBlock"},
			{0, "toBlock"},
		},
		// architecture/evm/trace_filter.go hook enforces lower/upper-bound against per-upstream latest/finality.
		EnforceBlockAvailability: util.BoolPtr(false),
	},
	"arbtrace_filter": {
		ReqRefs: [][]interface{}{
			{0, "fromBlock"},
			{0, "toBlock"},
		},
		// architecture/evm/trace_filter.go hook enforces lower/upper-bound against per-upstream latest/finality.
		EnforceBlockAvailability: util.BoolPtr(false),
	},
	"debug_traceBlockByNumber": {
		ReqRefs: FirstParam,
	},
	"trace_replayBlockTransactions": {
		ReqRefs: FirstParam,
	},
	"debug_storageRangeAt": {
		ReqRefs: FirstParam,
	},
	"debug_traceBlockByHash": {
		ReqRefs: FirstParam,
	},
	"debug_getRawBlock": {
		ReqRefs: FirstParam,
	},
	"debug_getRawHeader": {
		ReqRefs: FirstParam,
	},
	"debug_getRawReceipts": {
		ReqRefs: FirstParam,
	},
	"erigon_getHeaderByNumber": {
		ReqRefs: FirstParam,
	},
	"arbtrace_block": {
		ReqRefs: FirstParam,
	},
	"arbtrace_replayBlockTransactions": {
		ReqRefs: FirstParam,
	},
}

// Special methods that can be cached regardless of block.
// Most often finality of these responses is 'unknown'.
// For these data it is safe to keep the data in cache even after reorg,
// because if client explcitly querying such data (e.g. a specific tx hash receipt)
// they know it might be reorged from a separate process.
// For example this is not safe to do for eth_getBlockByNumber because users
// require this method always give them current accurate data (even if it's reorged).
// Returning "*" as blockRef means that these data are safe be cached irrevelant of their block.
var DefaultSpecialCacheMethods = map[string]*CacheMethodConfig{
	"eth_getTransactionReceipt": {
		ReqRefs:  ArbitraryBlock,
		RespRefs: BlockNumberOrBlockHashParam,
	},
	"eth_getTransactionByHash": {
		ReqRefs:  ArbitraryBlock,
		RespRefs: BlockNumberOrBlockHashParam,
	},
	"arbtrace_replayTransaction": {
		ReqRefs: ArbitraryBlock,
	},
	"trace_replayTransaction": {
		ReqRefs: ArbitraryBlock,
	},
	"debug_traceTransaction": {
		ReqRefs: ArbitraryBlock,
	},
	"trace_rawTransaction": {
		ReqRefs: ArbitraryBlock,
	},
	"trace_transaction": {
		ReqRefs: ArbitraryBlock,
	},
	"debug_traceBlock": {
		ReqRefs: ArbitraryBlock,
	},
}

func (c *CacheConfig) SetDefaults() error {
	if len(c.Policies) > 0 {
		for _, policy := range c.Policies {
			if err := policy.SetDefaults(); err != nil {
				return fmt.Errorf("failed to set defaults for cache policy: %w", err)
			}
		}
	}
	if len(c.Connectors) > 0 {
		for _, connector := range c.Connectors {
			if err := connector.SetDefaults(connectorScopeCache); err != nil {
				return fmt.Errorf("failed to set defaults for cache connector: %w", err)
			}
		}
	}

	// Set compression defaults
	if c.Compression == nil {
		c.Compression = &CompressionConfig{}
	}
	if err := c.Compression.SetDefaults(); err != nil {
		return fmt.Errorf("failed to set defaults for compression: %w", err)
	}

	return nil
}

func (m *MethodsConfig) SetDefaults() error {
	if m.Definitions == nil || (len(m.Definitions) == 0 && !m.PreserveDefaultMethods) {
		// If no definitions provided or PreserveDefaultMethods is false, use all defaults
		mergedMethods := map[string]*CacheMethodConfig{}

		// Merge all default methods into a single map
		for name, method := range DefaultStaticCacheMethods {
			mergedMethods[name] = method
		}
		for name, method := range DefaultRealtimeCacheMethods {
			mergedMethods[name] = method
		}
		for name, method := range DefaultWithBlockCacheMethods {
			mergedMethods[name] = method
		}
		for name, method := range DefaultSpecialCacheMethods {
			mergedMethods[name] = method
		}

		// Mark default stateful methods
		for _, mn := range DefaultStatefulMethodNames {
			if cm, ok := mergedMethods[mn]; ok {
				cm.Stateful = true
			} else {
				mergedMethods[mn] = &CacheMethodConfig{Stateful: true}
			}
		}

		if m.PreserveDefaultMethods && m.Definitions != nil {
			// Merge user definitions on top of defaults
			for name, method := range m.Definitions {
				mergedMethods[name] = method
			}
		}

		m.Definitions = mergedMethods
	} else if m.PreserveDefaultMethods {
		// User provided some definitions and wants to preserve defaults
		// First copy all defaults
		mergedMethods := map[string]*CacheMethodConfig{}
		for name, method := range DefaultStaticCacheMethods {
			mergedMethods[name] = method
		}
		for name, method := range DefaultRealtimeCacheMethods {
			mergedMethods[name] = method
		}
		for name, method := range DefaultWithBlockCacheMethods {
			mergedMethods[name] = method
		}
		for name, method := range DefaultSpecialCacheMethods {
			mergedMethods[name] = method
		}

		// Mark default stateful methods
		for _, mn := range DefaultStatefulMethodNames {
			if cm, ok := mergedMethods[mn]; ok {
				cm.Stateful = true
			} else {
				mergedMethods[mn] = &CacheMethodConfig{Stateful: true}
			}
		}

		// Then override with user definitions
		for name, method := range m.Definitions {
			mergedMethods[name] = method
		}

		m.Definitions = mergedMethods
	} else {
		// User provided definitions and doesn't want defaults
		// Still need to ensure stateful methods are marked correctly
		for _, mn := range DefaultStatefulMethodNames {
			if cm, ok := m.Definitions[mn]; ok {
				// Method exists in user definitions, ensure it's marked as stateful
				cm.Stateful = true
			} else {
				// Method not in user definitions, add it as stateful
				m.Definitions[mn] = &CacheMethodConfig{Stateful: true}
			}
		}
	}

	return nil
}

func (c *CompressionConfig) SetDefaults() error {
	// Enable compression by default
	if c.Enabled == nil {
		c.Enabled = util.BoolPtr(true)
	}

	// Default to zstd algorithm
	if c.Algorithm == "" {
		c.Algorithm = "zstd"
	}

	// Default to fastest compression for optimal performance
	if c.ZstdLevel == "" {
		c.ZstdLevel = "fastest"
	}

	// Default threshold of 1KB based on real-world experience
	// JSON-RPC responses smaller than 1KB typically don't benefit much from compression
	// due to the overhead, while larger responses (blocks, logs, traces) see significant savings
	if c.Threshold == 0 {
		c.Threshold = 1024
	}

	return nil
}

func (c *CachePolicyConfig) SetDefaults() error {
	if c.Method == "" {
		c.Method = "*"
	}
	if c.Network == "" {
		c.Network = "*"
	}
	if c.AppliesTo == "" {
		c.AppliesTo = CachePolicyAppliesToBoth
	}

	return nil
}

func (c *TracingConfig) SetDefaults() error {
	if c.Protocol == "" {
		c.Protocol = "grpc"
	}
	if c.Endpoint == "" {
		if c.Protocol == TracingProtocolGrpc {
			c.Endpoint = "localhost:4317"
		} else {
			c.Endpoint = "http://localhost:4318"
		}
	}
	if c.SampleRate == 0 {
		c.SampleRate = 1.0
	}
	if c.ServiceName == "" {
		c.ServiceName = "erpc"
	}

	return nil
}

func (s *ServerConfig) SetDefaults() error {
	if s.ListenV4 == nil {
		if !util.IsTest() || os.Getenv("FORCE_TEST_LISTEN_V4") == "true" {
			s.ListenV4 = util.BoolPtr(true)
		}
	}
	if s.HttpHostV4 == nil {
		s.HttpHostV4 = util.StringPtr("0.0.0.0")
	}
	if s.HttpHostV6 == nil {
		s.HttpHostV6 = util.StringPtr("[::]")
	}
	// Handle deprecated HttpPort -> HttpPortV4 migration
	if s.HttpPort != nil && s.HttpPortV4 == nil {
		s.HttpPortV4 = s.HttpPort // Migrate deprecated field
	}
	if s.HttpPortV4 == nil {
		s.HttpPortV4 = util.IntPtr(4000)
	}
	// Keep HttpPort for backward compatibility (will be same as HttpPortV4)
	if s.HttpPort == nil {
		s.HttpPort = s.HttpPortV4
	}
	// Handle deprecated HttpPort -> HttpPortV6 migration for backward compatibility
	if s.HttpPort != nil && s.HttpPortV6 == nil {
		s.HttpPortV6 = util.IntPtr(*s.HttpPort + 1000)
	}
	if s.HttpPortV6 == nil {
		s.HttpPortV6 = util.IntPtr(5000) // Default: avoid 4001 (metrics)
	}
	if s.MaxTimeout == nil {
		d := Duration(150 * time.Second)
		s.MaxTimeout = &d
	}
	if s.ReadTimeout == nil {
		d := Duration(30 * time.Second)
		s.ReadTimeout = &d
	}
	if s.WriteTimeout == nil {
		d := Duration(120 * time.Second)
		s.WriteTimeout = &d
	}
	if s.EnableGzip == nil {
		s.EnableGzip = util.BoolPtr(true)
	}
	if s.WaitBeforeShutdown == nil {
		d := Duration(10 * time.Second)
		s.WaitBeforeShutdown = &d
	}
	if s.WaitAfterShutdown == nil {
		d := Duration(10 * time.Second)
		s.WaitAfterShutdown = &d
	}
	if s.IncludeErrorDetails == nil {
		s.IncludeErrorDetails = util.BoolPtr(true)
	}

	// Safe defaults for client IP resolution
	if len(s.TrustedIPForwarders) == 0 {
		// Only loopback by default; do not trust private subnets unless explicitly configured
		s.TrustedIPForwarders = []string{"127.0.0.1/8", "::1/128"}
	}
	if s.TrustedIPHeaders == nil {
		// Empty by default to avoid trusting headers unless explicitly configured
		s.TrustedIPHeaders = []string{}
	}

	return nil
}

func (h *HealthCheckConfig) SetDefaults() error {
	if h.Mode == "" {
		h.Mode = HealthCheckModeNetworks
	}
	if h.DefaultEval == "" {
		h.DefaultEval = EvalAnyInitializedUpstreams
	}

	return nil
}

func (m *MetricsConfig) SetDefaults() error {
	if m.Enabled == nil && !util.IsTest() {
		m.Enabled = util.BoolPtr(true)
	}
	if m.HostV4 == nil {
		m.HostV4 = util.StringPtr("0.0.0.0")
	}
	if m.HostV6 == nil {
		m.HostV6 = util.StringPtr("[::]")
	}
	if m.Port == nil {
		m.Port = util.IntPtr(4001)
	}
	if m.ErrorLabelMode == "" {
		m.ErrorLabelMode = ErrorLabelModeCompact
	}

	return nil
}

func (a *AdminConfig) SetDefaults() error {
	if a.Auth != nil {
		if err := a.Auth.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for auth: %w", err)
		}
	}
	if a.CORS == nil {
		// It is safe to enable CORS of * for admin endpoint since requests are protected by Secret Tokens
		a.CORS = &CORSConfig{
			AllowedOrigins:   []string{"*"},
			AllowCredentials: util.BoolPtr(false),
		}
	}
	if err := a.CORS.SetDefaults(); err != nil {
		return err
	}

	return nil
}

func (c *SharedStateConfig) SetDefaults(defClusterKey string) error {
	if c.Connector == nil {
		c.Connector = &ConnectorConfig{
			Id:     "memory",
			Driver: DriverMemory,
			Memory: &MemoryConnectorConfig{
				MaxItems: 100_000, MaxTotalSize: "1GB",
			},
		}
	} else {
		if c.Connector.Id == "" {
			c.Connector.Id = string(c.Connector.Driver)
		}
	}
	if c.ClusterKey == "" {
		c.ClusterKey = defClusterKey
	}
	if err := c.Connector.SetDefaults(connectorScopeSharedState); err != nil {
		return err
	}
	if c.FallbackTimeout == 0 {
		// Default timeout for Redis operations (get/set/publish)
		// Allow sufficient time for network latency to cloud Redis while still failing fast
		c.FallbackTimeout = Duration(3 * time.Second)
	}
	if c.LockTtl == 0 {
		// Lock TTL should be long enough to complete remote operations
		// Provides 1s buffer beyond FallbackTimeout for get+set operations
		c.LockTtl = Duration(4 * time.Second)
	}
	// Foreground best-effort budgets
	if c.LockMaxWait == 0 {
		// How long the foreground waits to acquire the lock before proceeding locally
		c.LockMaxWait = Duration(100 * time.Millisecond)
	}
	if c.UpdateMaxWait == 0 {
		// How long the foreground waits for the update function before returning stale
		c.UpdateMaxWait = Duration(50 * time.Millisecond)
	}
	return nil
}

func (d *DatabaseConfig) SetDefaults(defClusterKey string) error {
	if d.EvmJsonRpcCache != nil {
		if err := d.EvmJsonRpcCache.SetDefaults(); err != nil {
			return err
		}
	}
	if d.SharedState != nil {
		if err := d.SharedState.SetDefaults(defClusterKey); err != nil {
			return err
		}
	}

	return nil
}

func (c *ConnectorConfig) SetDefaults(scope connectorScope) error {
	if c.Id == "" {
		c.Id = string(scope) + "-" + string(c.Driver)
	}
	if c.Memory != nil {
		c.Driver = DriverMemory
	}
	if c.Driver == DriverMemory {
		if c.Memory == nil {
			c.Memory = &MemoryConnectorConfig{}
		}
		if err := c.Memory.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for memory connector: %w", err)
		}
	}
	if c.Redis != nil {
		c.Driver = DriverRedis
	}
	if c.Driver == DriverRedis {
		if c.Redis == nil {
			c.Redis = &RedisConnectorConfig{}
		}
		if err := c.Redis.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for redis connector: %w", err)
		}
	}
	if c.PostgreSQL != nil {
		c.Driver = DriverPostgreSQL
	}
	if c.Driver == DriverPostgreSQL {
		if c.PostgreSQL == nil {
			c.PostgreSQL = &PostgreSQLConnectorConfig{}
		}
		if err := c.PostgreSQL.SetDefaults(scope); err != nil {
			return fmt.Errorf("failed to set defaults for postgres connector: %w", err)
		}
	}
	if c.DynamoDB != nil {
		c.Driver = DriverDynamoDB
	}
	if c.Driver == DriverDynamoDB {
		if c.DynamoDB == nil {
			c.DynamoDB = &DynamoDBConnectorConfig{}
		}
		if err := c.DynamoDB.SetDefaults(scope); err != nil {
			return fmt.Errorf("failed to set defaults for dynamo db connector: %w", err)
		}
	}
	if c.Grpc != nil {
		c.Driver = DriverGrpc
	}
	if c.Driver == DriverGrpc {
		if c.Grpc == nil {
			c.Grpc = &GrpcConnectorConfig{}
		}
		if c.Grpc.GetTimeout == 0 {
			c.Grpc.GetTimeout = Duration(200 * time.Millisecond)
		}
	}

	return nil
}

func (m *MemoryConnectorConfig) SetDefaults() error {
	if m.MaxItems == 0 {
		m.MaxItems = 100000
	}
	if m.MaxTotalSize == "" {
		m.MaxTotalSize = "1GB"
	}

	return nil
}

func (r *RedisConnectorConfig) SetDefaults() error {
	if r.URI != "" && r.Addr != "" {
		return fmt.Errorf(
			"redis connector: provide either 'uri' or 'addr/username/password/db', not both",
		)
	}

	// Set default values for timeouts and connection pool size
	// This must happen BEFORE the URI check to ensure timeouts are always set
	if r.ConnPoolSize == 0 {
		r.ConnPoolSize = 8
	}
	if r.InitTimeout == 0 {
		r.InitTimeout = Duration(5 * time.Second)
	}
	if r.GetTimeout == 0 {
		r.GetTimeout = Duration(1 * time.Second)
	}
	if r.SetTimeout == 0 {
		r.SetTimeout = Duration(3 * time.Second)
	}
	if r.LockRetryInterval == 0 {
		r.LockRetryInterval = Duration(500 * time.Millisecond)
	}

	// URI is provided directly
	if r.URI != "" {
		return nil
	}

	// URI needs to be constructed from individual fields

	// Clean up Addr if it has scheme prefixes
	r.Addr = strings.TrimPrefix(r.Addr, "rediss://")
	r.Addr = strings.TrimPrefix(r.Addr, "redis://")

	// Construct URI from discrete fields using net/url to ensure
	// credentials and host parts are safely escaped.
	if r.Addr != "" {
		// Split host and port; if port is missing default to 6379.
		host, port, err := net.SplitHostPort(r.Addr)
		if err != nil {
			host = r.Addr
			port = "6379"
		}

		scheme := "redis"
		if r.TLS != nil && r.TLS.Enabled {
			scheme = "rediss"
		}

		u := &url.URL{
			Scheme: scheme,
			Host:   net.JoinHostPort(host, port),
		}

		// Add credentials, automatically URL‑encoded by url.URL.
		if r.Username != "" || r.Password != "" {
			u.User = url.UserPassword(r.Username, r.Password)
		}

		// Always include the database index (mirrors previous behaviour).
		u.Path = "/" + strconv.Itoa(r.DB)

		r.URI = u.String()

		r.Addr = ""
		r.Username = ""
		r.Password = ""
		r.DB = 0
	}

	return nil
}

func (p *PostgreSQLConnectorConfig) SetDefaults(scope connectorScope) error {
	if p.Table == "" {
		switch scope {
		case connectorScopeSharedState:
			p.Table = "erpc_shared_state"
		case connectorScopeCache:
			p.Table = "erpc_json_rpc_cache"
		case connectorScopeAuth:
			p.Table = "erpc_auth"
		default:
			return fmt.Errorf("invalid connector scope: %s", scope)
		}
	}
	if p.MinConns == 0 {
		if scope == connectorScopeAuth {
			p.MinConns = 1
		} else {
			p.MinConns = 4
		}
	}
	if p.MaxConns == 0 {
		if scope == connectorScopeAuth {
			p.MaxConns = 4
		} else {
			p.MaxConns = 32
		}
	}
	if p.InitTimeout == 0 {
		p.InitTimeout = Duration(5 * time.Second)
	}
	if p.GetTimeout == 0 {
		p.GetTimeout = Duration(1 * time.Second)
	}
	if p.SetTimeout == 0 {
		p.SetTimeout = Duration(2 * time.Second)
	}

	return nil
}

func (d *DynamoDBConnectorConfig) SetDefaults(scope connectorScope) error {
	if d.Table == "" {
		switch scope {
		case connectorScopeSharedState:
			d.Table = "erpc_shared_state"
		case connectorScopeCache:
			d.Table = "erpc_json_rpc_cache"
		case connectorScopeAuth:
			d.Table = "erpc_auth"
		default:
			return fmt.Errorf("invalid connector scope: %s", scope)
		}
	}
	if d.PartitionKeyName == "" {
		d.PartitionKeyName = "groupKey"
	}
	if d.RangeKeyName == "" {
		d.RangeKeyName = "requestKey"
	}
	if d.ReverseIndexName == "" {
		d.ReverseIndexName = "idx_requestKey_groupKey"
	}
	if d.TTLAttributeName == "" {
		d.TTLAttributeName = "ttl"
	}
	if d.InitTimeout == 0 {
		d.InitTimeout = Duration(5 * time.Second)
	}
	if d.GetTimeout == 0 {
		d.GetTimeout = Duration(1 * time.Second)
	}
	if d.SetTimeout == 0 {
		d.SetTimeout = Duration(2 * time.Second)
	}
	if d.StatePollInterval == 0 {
		d.StatePollInterval = Duration(5 * time.Second)
	}

	return nil
}

func (p *ProjectConfig) SetDefaults(opts *DefaultOptions) error {
	if p.NetworkDefaults != nil {
		if err := p.NetworkDefaults.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for network defaults: %w", err)
		}
	}
	if p.UpstreamDefaults != nil {
		if err := p.UpstreamDefaults.SetDefaults(nil); err != nil {
			return fmt.Errorf("failed to set defaults for upstream defaults: %w", err)
		}
	}
	if len(p.Providers) == 0 && len(p.Upstreams) == 0 {
		if opts != nil && len(opts.Endpoints) > 0 {
			for _, endpoint := range opts.Endpoints {
				upstream := &UpstreamConfig{
					Endpoint: endpoint,
				}
				if err := upstream.SetDefaults(p.UpstreamDefaults); err != nil {
					return fmt.Errorf("failed to set defaults for upstream: %w", err)
				}
				p.Upstreams = append(p.Upstreams, upstream)
			}
		} else {
			log.Warn().Msg("no providers or upstreams found in project; will use default 'public' endpoints repository")
			repositoryCfg := &ProviderConfig{
				Id:     "public",
				Vendor: "repository",
				// Let the vendor use the default repository URL
			}
			if err := repositoryCfg.SetDefaults(p.UpstreamDefaults); err != nil {
				return fmt.Errorf("failed to set defaults for repository provider: %w", err)
			}
			envioCfg := &ProviderConfig{
				Id:     "envio",
				Vendor: "envio",
			}
			if err := envioCfg.SetDefaults(p.UpstreamDefaults); err != nil {
				return fmt.Errorf("failed to set defaults for envio provider: %w", err)
			}
			p.Providers = append(p.Providers, repositoryCfg, envioCfg)
		}
	}
	if p.Providers == nil {
		p.Providers = []*ProviderConfig{}
	}
	for _, provider := range p.Providers {
		if err := provider.SetDefaults(p.UpstreamDefaults); err != nil {
			return fmt.Errorf("failed to set defaults for provider: %w", err)
		}
	}
	if p.Upstreams != nil {
		for i := 0; i < len(p.Upstreams); i++ {
			upstream := p.Upstreams[i]
			if p.UpstreamDefaults != nil {
				err := upstream.ApplyDefaults(p.UpstreamDefaults)
				if err != nil {
					return fmt.Errorf("failed to apply defaults for upstream: %w", err)
				}
			}
			if err := upstream.SetDefaults(p.UpstreamDefaults); err != nil {
				return fmt.Errorf("failed to set defaults for upstream: %w", err)
			}
			if provider, err := convertUpstreamToProvider(upstream); err != nil {
				return fmt.Errorf("failed to convert upstream (id: %s) to provider: %w", upstream.Id, err)
			} else if provider != nil {
				p.Providers = append(p.Providers, provider)
				p.Upstreams = slices.Delete(p.Upstreams, i, i+1)
				i--
			}
		}
	}
	if p.Networks != nil {
		for _, network := range p.Networks {
			if err := network.SetDefaults(p.Upstreams, p.NetworkDefaults); err != nil {
				return fmt.Errorf("failed to set defaults for network: %w", err)
			}
		}
	}
	if p.Auth != nil {
		if err := p.Auth.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for auth: %w", err)
		}
	}
	if p.CORS != nil {
		if err := p.CORS.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for cors: %w", err)
		}
	}
	if p.ScoreMetricsWindowSize == 0 {
		if p.DeprecatedHealthCheck != nil && p.DeprecatedHealthCheck.ScoreMetricsWindowSize != 0 {
			log.Warn().Msg("projects.*.healthCheck.scoreMetricsWindowSize is deprecated; use projects.*.scoreMetricsWindowSize instead")
			p.ScoreMetricsWindowSize = p.DeprecatedHealthCheck.ScoreMetricsWindowSize
		} else {
			p.ScoreMetricsWindowSize = Duration(10 * time.Minute)
		}
	}
	// Default score metrics mode to compact when not provided
	if strings.TrimSpace(p.ScoreMetricsMode) == "" {
		p.ScoreMetricsMode = "compact"
	}

	return nil
}

func convertUpstreamToProvider(upstream *UpstreamConfig) (*ProviderConfig, error) {
	if strings.HasPrefix(upstream.Endpoint, "http://") ||
		strings.HasPrefix(upstream.Endpoint, "https://") ||
		strings.HasPrefix(upstream.Endpoint, "grpc://") ||
		strings.HasPrefix(upstream.Endpoint, "grpc+bds://") {
		return nil, nil
	}

	endpoint, err := url.Parse(upstream.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream endpoint while converting to provider: %w", err)
	}

	vendorName := strings.Replace(endpoint.Scheme, "evm+", "", 1)
	settings, err := buildProviderSettings(vendorName, endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to convert upstream (id: %s) to provider: %w", upstream.Id, err)
	}

	// Create a copy of upstream config to apply to provider-created upstreams,
	// except Id and Endpoint, because they will be set by the provider.
	upsCfg := &UpstreamConfig{}
	*upsCfg = *upstream
	upsCfg.Endpoint = ""
	upsCfg.Id = ""

	id := upstream.Id
	// Generate a unique provider ID that includes the vendor name and a hash of the endpoint
	// This ensures that multiple instances of the same provider with different credentials
	// will have unique IDs
	if id == "" {
		id = vendorName + "-" + util.IncrementAndGetIndex("shorthand-provider-id", vendorName)
	}

	cfg := &ProviderConfig{
		Id:                 id,
		Vendor:             vendorName,
		Settings:           settings,
		UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
		Overrides: map[string]*UpstreamConfig{
			"*": upsCfg,
		},
	}
	if err := cfg.SetDefaults(upstream); err != nil {
		return nil, fmt.Errorf("failed to set defaults for provider: %w", err)
	}

	return cfg, nil
}

func buildProviderSettings(vendorName string, endpoint *url.URL) (VendorSettings, error) {
	switch vendorName {
	case "alchemy", "evm+alchemy":
		return VendorSettings{
			"apiKey": endpoint.Host,
		}, nil
	case "blastapi", "evm+blastapi":
		return VendorSettings{
			"apiKey": endpoint.Host,
		}, nil
	case "drpc", "evm+drpc":
		return VendorSettings{
			"apiKey": endpoint.Host,
		}, nil
	case "envio", "evm+envio":
		return VendorSettings{
			"rootDomain": endpoint.Host,
		}, nil
	case "etherspot", "evm+etherspot":
		return VendorSettings{
			"apiKey": endpoint.Host,
		}, nil
	case "infura", "evm+infura":
		return VendorSettings{
			"apiKey": endpoint.Host,
		}, nil
	case "llama", "evm+llama":
		return VendorSettings{
			"apiKey": endpoint.Host,
		}, nil
	case "pimlico", "evm+pimlico":
		return VendorSettings{
			"apiKey": endpoint.Host,
		}, nil
	case "thirdweb", "evm+thirdweb":
		return VendorSettings{
			"clientId": endpoint.Host,
		}, nil
	case "dwellir", "evm+dwellir":
		return VendorSettings{
			"apiKey": endpoint.Host,
		}, nil
	case "conduit", "evm+conduit":
		return VendorSettings{
			"apiKey": endpoint.Host,
		}, nil
	case "superchain", "evm+superchain":
		spec := endpoint.Host
		if endpoint.Path != "" && endpoint.Path != "/" {
			spec += endpoint.Path
		}
		return VendorSettings{
			"registryUrl": spec,
		}, nil
	case "tenderly", "evm+tenderly":
		return VendorSettings{
			"apiKey": endpoint.Host,
		}, nil
	case "quicknode", "evm+quicknode":
		settings := VendorSettings{
			"apiKey": endpoint.Host,
		}

		// Parse query parameters for tag filters
		if endpoint.RawQuery != "" {
			params, err := url.ParseQuery(endpoint.RawQuery)
			if err != nil {
				return nil, fmt.Errorf("failed to parse quicknode query parameters: %w", err)
			}

			// Parse tagIds - can be comma-separated or multiple values
			if tagIds := params.Get("tagIds"); tagIds != "" {
				// Split comma-separated values
				ids := strings.Split(tagIds, ",")
				if len(ids) == 1 {
					// Single value - try to parse as int
					if id, err := strconv.Atoi(strings.TrimSpace(ids[0])); err == nil {
						settings["tagIds"] = id
					}
				} else {
					// Multiple values - parse as int array
					intIds := make([]int, 0, len(ids))
					for _, idStr := range ids {
						if id, err := strconv.Atoi(strings.TrimSpace(idStr)); err == nil {
							intIds = append(intIds, id)
						}
					}
					if len(intIds) > 0 {
						settings["tagIds"] = intIds
					}
				}
			}

			// Parse tagLabels - can be comma-separated or multiple values
			if tagLabels := params.Get("tagLabels"); tagLabels != "" {
				// Split comma-separated values
				labels := strings.Split(tagLabels, ",")
				for i := range labels {
					labels[i] = strings.TrimSpace(labels[i])
				}
				if len(labels) == 1 {
					settings["tagLabels"] = labels[0]
				} else {
					settings["tagLabels"] = labels
				}
			}
		}

		return settings, nil
	case "chainstack", "evm+chainstack":
		settings := VendorSettings{
			"apiKey": endpoint.Host,
		}

		// Parse query parameters for additional filters
		if endpoint.RawQuery != "" {
			params, err := url.ParseQuery(endpoint.RawQuery)
			if err != nil {
				return nil, fmt.Errorf("failed to parse chainstack query parameters: %w", err)
			}
			for key, values := range params {
				if len(values) == 1 {
					settings[key] = values[0]
				} else {
					settings[key] = values
				}
			}
		}

		return settings, nil
	case "onfinality", "evm+onfinality":
		return VendorSettings{
			"apiKey": endpoint.Host,
		}, nil
	case "blockpi", "evm+blockpi":
		return VendorSettings{
			"apiKey": endpoint.Host,
		}, nil
	case "ankr", "evm+ankr":
		return VendorSettings{
			"apiKey": endpoint.Host,
		}, nil
	case "erpc", "evm+erpc":
		settings := VendorSettings{
			"endpoint": "https://" + endpoint.Host + "/" + strings.TrimPrefix(endpoint.Path, "/"),
		}

		if endpoint.RawQuery != "" {
			params, err := url.ParseQuery(endpoint.RawQuery)
			if err != nil {
				return nil, fmt.Errorf("failed to parse erpc query parameters: %w", err)
			}

			if secret := params.Get("secret"); secret != "" {
				settings["secret"] = secret
			}
		}
		return settings, nil
	case "repository", "evm+repository":
		return VendorSettings{
			"repositoryUrl": "https://" + endpoint.Host + "/" + strings.TrimPrefix(endpoint.Path, "/") + "?" + endpoint.RawQuery,
		}, nil
	case "routemesh", "evm+routemesh":
		settings := VendorSettings{
			"baseURL": endpoint.Host,
		}
		// Extract apiKey from path: /rpc/<chainId>/<apiKey>
		// Path segments: ["", "rpc", "<chainId>", "<apiKey>"]
		pathParts := strings.Split(strings.TrimPrefix(endpoint.Path, "/"), "/")
		if len(pathParts) >= 3 && pathParts[0] == "rpc" {
			settings["apiKey"] = pathParts[2] // apiKey is the 3rd segment (index 2)
		} else {
			return nil, fmt.Errorf("routemesh endpoint path must be in format /rpc/<chainId>/<apiKey>, got: %s", endpoint.Path)
		}
		return settings, nil
	}

	return nil, fmt.Errorf("unsupported vendor name in vendor.settings: %s", vendorName)
}

func (n *NetworkDefaults) SetDefaults() error {
	if len(n.Failsafe) > 0 {
		for i, fs := range n.Failsafe {
			if err := fs.SetDefaults(nil); err != nil {
				return fmt.Errorf("failed to set defaults for failsafe[%d]: %w", i, err)
			}
		}
	}
	if n.SelectionPolicy != nil {
		if err := n.SelectionPolicy.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for selection policy: %w", err)
		}
	}
	if n.Evm != nil {
		if err := n.Evm.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for evm: %w", err)
		}
	}
	if n.DirectiveDefaults != nil {
		if err := n.DirectiveDefaults.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for directive defaults: %w", err)
		}
	}

	return nil
}

func (d *DirectiveDefaultsConfig) SetDefaults() error {
	if d == nil {
		return nil
	}
	if d.EnforceHighestBlock == nil {
		d.EnforceHighestBlock = util.BoolPtr(true)
	}
	if d.EnforceGetLogsBlockRange == nil {
		d.EnforceGetLogsBlockRange = util.BoolPtr(true)
	}
	if d.EnforceNonNullTaggedBlocks == nil {
		d.EnforceNonNullTaggedBlocks = util.BoolPtr(true)
	}
	return nil
}

func (p *ProviderConfig) SetDefaults(upsDefaults *UpstreamConfig) error {
	if p.Id == "" {
		p.Id = p.Vendor
	}
	if p.UpstreamIdTemplate == "" {
		p.UpstreamIdTemplate = "<PROVIDER>-<NETWORK>"
	}
	if p.Overrides != nil {
		for _, override := range p.Overrides {
			if err := override.SetDefaults(upsDefaults); err != nil {
				return fmt.Errorf("failed to set defaults for override: %w", err)
			}
		}
	}

	return nil
}

func (u *UpstreamConfig) ApplyDefaults(defaults *UpstreamConfig) error {
	if defaults == nil {
		return nil
	}

	if u.Endpoint == "" {
		u.Endpoint = defaults.Endpoint
	}
	if u.Type == "" {
		u.Type = defaults.Type
	}
	if u.VendorName == "" {
		u.VendorName = defaults.VendorName
	}
	if u.Group == "" {
		u.Group = defaults.Group
	}
	if u.Failsafe == nil && defaults.Failsafe != nil {
		u.Failsafe = defaults.Failsafe
	}
	if u.RateLimitBudget == "" {
		u.RateLimitBudget = defaults.RateLimitBudget
	}
	if u.RateLimitAutoTune == nil {
		u.RateLimitAutoTune = defaults.RateLimitAutoTune
	}
	// IMPORTANT: Some of the configs must be copied vs referenced, because the object might be updated in runtime only for this specific upstream
	// TODO Should we refactor so this won't happen?
	if u.Evm == nil && defaults.Evm != nil {
		u.Evm = &EvmUpstreamConfig{
			ChainId:                  defaults.Evm.ChainId,
			NodeType:                 defaults.Evm.NodeType,
			StatePollerInterval:      defaults.Evm.StatePollerInterval,
			StatePollerDebounce:      defaults.Evm.StatePollerDebounce,
			MaxAvailableRecentBlocks: defaults.Evm.MaxAvailableRecentBlocks,
		}
		if err := u.Evm.SetDefaults(defaults.Evm); err != nil {
			return fmt.Errorf("failed to set defaults for evm upstream: %w", err)
		}
	} else if u.Evm != nil && defaults.Evm != nil {
		if u.Evm.StatePollerInterval == 0 && defaults.Evm.StatePollerInterval != 0 {
			u.Evm.StatePollerInterval = defaults.Evm.StatePollerInterval
		}
		if u.Evm.StatePollerDebounce == 0 && defaults.Evm.StatePollerDebounce != 0 {
			u.Evm.StatePollerDebounce = defaults.Evm.StatePollerDebounce
		}
		if u.Evm.MaxAvailableRecentBlocks == 0 && defaults.Evm.MaxAvailableRecentBlocks != 0 {
			u.Evm.MaxAvailableRecentBlocks = defaults.Evm.MaxAvailableRecentBlocks
		}
		if u.Evm.GetLogsAutoSplittingRangeThreshold == 0 && defaults.Evm.GetLogsAutoSplittingRangeThreshold != 0 {
			u.Evm.GetLogsAutoSplittingRangeThreshold = defaults.Evm.GetLogsAutoSplittingRangeThreshold
		}
	}
	if u.JsonRpc == nil && defaults.JsonRpc != nil {
		u.JsonRpc = &JsonRpcUpstreamConfig{
			SupportsBatch:      defaults.JsonRpc.SupportsBatch,
			BatchMaxSize:       defaults.JsonRpc.BatchMaxSize,
			BatchMaxWait:       defaults.JsonRpc.BatchMaxWait,
			EnableGzip:         defaults.JsonRpc.EnableGzip,
			ProxyPool:          defaults.JsonRpc.ProxyPool,
			Headers:            defaults.JsonRpc.Headers,
			HTTPClientTimeouts: defaults.JsonRpc.HTTPClientTimeouts,
		}
	} else if u.JsonRpc != nil && defaults.JsonRpc != nil {
		// Merge individual fields from defaults if not set on upstream
		if u.JsonRpc.SupportsBatch == nil && defaults.JsonRpc.SupportsBatch != nil {
			u.JsonRpc.SupportsBatch = defaults.JsonRpc.SupportsBatch
		}
		if u.JsonRpc.BatchMaxSize == 0 && defaults.JsonRpc.BatchMaxSize != 0 {
			u.JsonRpc.BatchMaxSize = defaults.JsonRpc.BatchMaxSize
		}
		if u.JsonRpc.BatchMaxWait == 0 && defaults.JsonRpc.BatchMaxWait != 0 {
			u.JsonRpc.BatchMaxWait = defaults.JsonRpc.BatchMaxWait
		}
		if u.JsonRpc.EnableGzip == nil && defaults.JsonRpc.EnableGzip != nil {
			u.JsonRpc.EnableGzip = defaults.JsonRpc.EnableGzip
		}
		if u.JsonRpc.ProxyPool == "" && defaults.JsonRpc.ProxyPool != "" {
			u.JsonRpc.ProxyPool = defaults.JsonRpc.ProxyPool
		}
		if u.JsonRpc.Headers == nil && defaults.JsonRpc.Headers != nil {
			u.JsonRpc.Headers = defaults.JsonRpc.Headers
		}
		u.JsonRpc.HTTPClientTimeouts.MergeFrom(&defaults.JsonRpc.HTTPClientTimeouts)
	}
	// Integrity moved under Evm.Integrity
	if u.Evm != nil && defaults.Evm != nil {
		if u.Evm.Integrity == nil && defaults.Evm.Integrity != nil {
			u.Evm.Integrity = defaults.Evm.Integrity.Copy()
		}
	}
	if u.Routing == nil {
		u.Routing = defaults.Routing
	}
	if u.AllowMethods == nil && defaults.AllowMethods != nil {
		u.AllowMethods = append([]string{}, defaults.AllowMethods...)
	}
	if u.IgnoreMethods == nil && defaults.IgnoreMethods != nil {
		u.IgnoreMethods = append([]string{}, defaults.IgnoreMethods...)
	}
	if u.AutoIgnoreUnsupportedMethods == nil && defaults.AutoIgnoreUnsupportedMethods != nil {
		u.AutoIgnoreUnsupportedMethods = defaults.AutoIgnoreUnsupportedMethods
	}

	return nil
}

func (u *UpstreamConfig) SetDefaults(defaults *UpstreamConfig) error {
	if u.Id == "" {
		if u.VendorName == "" {
			epUrl, err := url.Parse(u.Endpoint)
			if err != nil {
				return fmt.Errorf("failed to parse endpoint: %w", err)
			}
			if util.IsNativeProtocol(u.Endpoint) {
				host := epUrl.Hostname()
				if host == "" {
					host = epUrl.Host
				}
				u.Id = host + "-" + util.IncrementAndGetIndex("shorthand-upstream-default-id", host)
			} else {
				u.Id = epUrl.Scheme + "-" + util.IncrementAndGetIndex("shorthand-upstream-default-id", epUrl.Scheme)
			}
		} else {
			u.Id = u.VendorName + "-" + util.IncrementAndGetIndex("shorthand-upstream-default-id", u.VendorName)
		}
	}
	if u.Type == "" {
		// TODO make actual calls to detect other types (solana, btc, etc)?
		u.Type = UpstreamTypeEvm
	}

	if len(u.Failsafe) > 0 {
		if defaults != nil && defaults.Failsafe != nil && len(defaults.Failsafe) > 0 {
			// Apply defaults to each failsafe config
			for i, fs := range u.Failsafe {
				// Find matching default by method/finality
				var defaultFs *FailsafeConfig
				for _, dfs := range defaults.Failsafe {
					// Match method using wildcard (if both are specified)
					methodMatch := true
					if dfs.MatchMethod != "" && fs.MatchMethod != "" {
						methodMatch, _ = WildcardMatch(dfs.MatchMethod, fs.MatchMethod)
					} else if dfs.MatchMethod != "" || fs.MatchMethod != "" {
						// If only one has a method specified, they don't match
						methodMatch = false
					}

					// Match finality (empty array means any finality)
					finalityMatch := MatchFinalities(dfs.MatchFinality, fs.MatchFinality)

					if methodMatch && finalityMatch {
						defaultFs = dfs
						break
					}
				}
				if defaultFs != nil {
					if err := fs.SetDefaults(defaultFs); err != nil {
						return fmt.Errorf("failed to set defaults for failsafe[%d]: %w", i, err)
					}
				} else {
					if err := fs.SetDefaults(nil); err != nil {
						return fmt.Errorf("failed to set defaults for failsafe[%d]: %w", i, err)
					}
				}
			}
		} else {
			// Apply nil defaults to each failsafe config
			for i, fs := range u.Failsafe {
				if err := fs.SetDefaults(nil); err != nil {
					return fmt.Errorf("failed to set defaults for failsafe[%d]: %w", i, err)
				}
			}
		}
	} else if defaults != nil && defaults.Failsafe != nil && len(defaults.Failsafe) > 0 {
		u.Failsafe = make([]*FailsafeConfig, len(defaults.Failsafe))
		for i, dfs := range defaults.Failsafe {
			u.Failsafe[i] = &FailsafeConfig{}
			*u.Failsafe[i] = *dfs
			if err := u.Failsafe[i].SetDefaults(dfs); err != nil {
				return fmt.Errorf("failed to set defaults for failsafe[%d]: %w", i, err)
			}
		}
	}

	if u.RateLimitAutoTune == nil && u.RateLimitBudget != "" {
		u.RateLimitAutoTune = &RateLimitAutoTuneConfig{}
	}
	if u.RateLimitAutoTune != nil {
		if err := u.RateLimitAutoTune.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for rate limit auto tune: %w", err)
		}
	}

	if u.Evm == nil {
		if strings.HasPrefix(string(u.Type), "evm") {
			u.Evm = &EvmUpstreamConfig{}
		}
	}
	if u.Evm != nil {
		if defaults != nil && defaults.Evm != nil {
			if err := u.Evm.SetDefaults(defaults.Evm); err != nil {
				return fmt.Errorf("failed to set defaults for evm: %w", err)
			}
		} else {
			if err := u.Evm.SetDefaults(nil); err != nil {
				return fmt.Errorf("failed to set defaults for evm: %w", err)
			}
		}
	}

	if u.JsonRpc == nil {
		u.JsonRpc = &JsonRpcUpstreamConfig{}
	}
	if err := u.JsonRpc.SetDefaults(); err != nil {
		return fmt.Errorf("failed to set defaults for json rpc: %w", err)
	}
	if u.Routing == nil {
		u.Routing = &RoutingConfig{}
	}
	if err := u.Routing.SetDefaults(); err != nil {
		return fmt.Errorf("failed to set defaults for routing: %w", err)
	}

	// By default if any allowed methods are specified, all other methods are ignored (unless ignoreMethods is explicitly defined by user)
	// Similar to how common network security policies work.
	if u.AllowMethods != nil {
		if u.IgnoreMethods == nil {
			u.IgnoreMethods = []string{"*"}
		}
	}

	return nil
}

func (e *EvmUpstreamConfig) SetDefaults(defaults *EvmUpstreamConfig) error {
	if e.StatePollerInterval == 0 {
		if defaults != nil && defaults.StatePollerInterval != 0 {
			e.StatePollerInterval = defaults.StatePollerInterval
		} else {
			e.StatePollerInterval = Duration(30 * time.Second)
		}
	}
	if e.StatePollerDebounce == 0 {
		if defaults != nil && defaults.StatePollerDebounce != 0 {
			e.StatePollerDebounce = defaults.StatePollerDebounce
		} else {
			e.StatePollerDebounce = Duration(5 * time.Second)
		}
	}
	if e.NodeType == "" {
		if defaults != nil && defaults.NodeType != "" {
			e.NodeType = defaults.NodeType
		} else {
			e.NodeType = EvmNodeTypeUnknown
		}
	}

	if e.MaxAvailableRecentBlocks == 0 {
		if defaults != nil && defaults.MaxAvailableRecentBlocks != 0 {
			e.MaxAvailableRecentBlocks = defaults.MaxAvailableRecentBlocks
		} else {
			// @deprecated: NodeType-based defaults moved under blockAvailability bounds
			// Kept for back-compat in config only; runtime logic does not use NodeType.
			switch e.NodeType {
			case EvmNodeTypeFull:
				e.MaxAvailableRecentBlocks = 128
			}
		}
	}

	// Back-compat mapping: if blockAvailability not provided but legacy MaxAvailableRecentBlocks is set,
	// map it to lower.latestBlockMinus with updateRate=0 (freeze).
	if e.BlockAvailability == nil && e.MaxAvailableRecentBlocks > 0 {
		v := e.MaxAvailableRecentBlocks
		e.BlockAvailability = &EvmBlockAvailabilityConfig{
			Lower: &EvmAvailabilityBoundConfig{LatestBlockMinus: &v},
		}
	}

	// If NodeType==full and no lower bound specified anywhere, set a default window of 128 blocks.
	if e.BlockAvailability == nil && e.MaxAvailableRecentBlocks == 0 && e.NodeType == EvmNodeTypeFull {
		v := int64(128)
		e.BlockAvailability = &EvmBlockAvailabilityConfig{
			Lower: &EvmAvailabilityBoundConfig{LatestBlockMinus: &v},
		}
	}

	if e.SkipWhenSyncing == nil {
		if defaults != nil && defaults.SkipWhenSyncing != nil {
			e.SkipWhenSyncing = defaults.SkipWhenSyncing
		} else {
			e.SkipWhenSyncing = util.BoolPtr(false)
		}
	}

	return nil
}

func (j *JsonRpcUpstreamConfig) SetDefaults() error {
	return nil
}

func (n *NetworkConfig) SetDefaults(upstreams []*UpstreamConfig, defaults *NetworkDefaults) error {
	if defaults != nil {
		if n.RateLimitBudget == "" {
			n.RateLimitBudget = defaults.RateLimitBudget
		}
		if len(defaults.Failsafe) > 0 {
			if len(n.Failsafe) == 0 {
				n.Failsafe = make([]*FailsafeConfig, len(defaults.Failsafe))
				for i, fs := range defaults.Failsafe {
					n.Failsafe[i] = &FailsafeConfig{}
					*n.Failsafe[i] = *fs
				}
			} else {
				// Apply defaults to each failsafe config
				for i, fs := range n.Failsafe {
					// Find matching default by method/finality
					defaultFs := &FailsafeConfig{
						MatchMethod: "*",
					}
					for _, dfs := range defaults.Failsafe {
						// Match method using wildcard (if both are specified)
						methodMatch := true
						if dfs.MatchMethod != "" && fs.MatchMethod != "" {
							methodMatch, _ = WildcardMatch(dfs.MatchMethod, fs.MatchMethod)
						} else if dfs.MatchMethod != "" || fs.MatchMethod != "" {
							// If only one has a method specified, they don't match
							methodMatch = false
						}

						// Match finality (empty array means any finality)
						finalityMatch := MatchFinalities(dfs.MatchFinality, fs.MatchFinality)

						if methodMatch && finalityMatch {
							defaultFs = dfs
							break
						}
					}
					if defaultFs != nil {
						if err := fs.SetDefaults(defaultFs); err != nil {
							return fmt.Errorf("failed to set defaults for failsafe[%d]: %w", i, err)
						}
					}
				}
			}
		} else if len(n.Failsafe) > 0 {
			// Apply system default to each failsafe config
			for i, fs := range n.Failsafe {
				if err := fs.SetDefaults(nil); err != nil {
					return fmt.Errorf("failed to set defaults for failsafe[%d]: %w", i, err)
				}
			}
		}
		if n.SelectionPolicy == nil && defaults.SelectionPolicy != nil {
			n.SelectionPolicy = &SelectionPolicyConfig{}
			*n.SelectionPolicy = *defaults.SelectionPolicy
		}
		if n.DirectiveDefaults == nil && defaults.DirectiveDefaults != nil {
			n.DirectiveDefaults = &DirectiveDefaultsConfig{}
			*n.DirectiveDefaults = *defaults.DirectiveDefaults
		}
		if n.Evm != nil && defaults.Evm != nil {
			if n.Evm.Integrity == nil && defaults.Evm.Integrity != nil {
				n.Evm.Integrity = &EvmIntegrityConfig{}
				*n.Evm.Integrity = *defaults.Evm.Integrity
			}
			if n.Evm.FallbackStatePollerDebounce == 0 && defaults.Evm.FallbackStatePollerDebounce != 0 {
				n.Evm.FallbackStatePollerDebounce = defaults.Evm.FallbackStatePollerDebounce
			}
			if n.Evm.FallbackFinalityDepth == 0 && defaults.Evm.FallbackFinalityDepth != 0 {
				n.Evm.FallbackFinalityDepth = defaults.Evm.FallbackFinalityDepth
			}
			if n.Evm.GetLogsMaxAllowedAddresses == 0 && defaults.Evm.GetLogsMaxAllowedAddresses != 0 {
				n.Evm.GetLogsMaxAllowedAddresses = defaults.Evm.GetLogsMaxAllowedAddresses
			}
			if n.Evm.GetLogsMaxAllowedTopics == 0 && defaults.Evm.GetLogsMaxAllowedTopics != 0 {
				n.Evm.GetLogsMaxAllowedTopics = defaults.Evm.GetLogsMaxAllowedTopics
			}
			if n.Evm.GetLogsSplitOnError == nil && defaults.Evm.GetLogsSplitOnError != nil {
				n.Evm.GetLogsSplitOnError = defaults.Evm.GetLogsSplitOnError
			}
			if n.Evm.GetLogsMaxAllowedRange == 0 && defaults.Evm.GetLogsMaxAllowedRange != 0 {
				n.Evm.GetLogsMaxAllowedRange = defaults.Evm.GetLogsMaxAllowedRange
			}
			if n.Evm.GetLogsSplitConcurrency == 0 && defaults.Evm.GetLogsSplitConcurrency != 0 {
				n.Evm.GetLogsSplitConcurrency = defaults.Evm.GetLogsSplitConcurrency
			}
		} else if n.Evm == nil && defaults.Evm != nil {
			n.Evm = &EvmNetworkConfig{}
			*n.Evm = *defaults.Evm
		}
		if n.Evm != nil {
			if err := n.Evm.SetDefaults(); err != nil {
				return fmt.Errorf("failed to set defaults for evm network config: %w", err)
			}
		}
	} else if len(n.Failsafe) > 0 {
		// Apply system default to each failsafe config
		for i, fs := range n.Failsafe {
			if err := fs.SetDefaults(nil); err != nil {
				return fmt.Errorf("failed to set defaults for failsafe[%d]: %w", i, err)
			}
		}
	}

	if n.Architecture == "" {
		if n.Evm != nil {
			n.Architecture = "evm"
		}
	}

	if n.Architecture == "evm" && n.Evm == nil {
		n.Evm = &EvmNetworkConfig{}
	}

	// Apply methods defaults
	if n.Methods == nil {
		n.Methods = &MethodsConfig{}
	}
	if err := n.Methods.SetDefaults(); err != nil {
		return fmt.Errorf("failed to set defaults for methods: %w", err)
	}

	if n.Evm != nil {
		if err := n.Evm.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for network evm config: %w", err)
		}
	}

	if len(upstreams) > 0 {
		anyUpstreamInFallbackGroup := slices.ContainsFunc(upstreams, func(u *UpstreamConfig) bool {
			return u.Group == "fallback"
		})
		if anyUpstreamInFallbackGroup && n.SelectionPolicy == nil {
			defCfg := NewDefaultNetworkConfig(upstreams)
			n.SelectionPolicy = defCfg.SelectionPolicy
		}
	}
	if n.SelectionPolicy != nil {
		if err := n.SelectionPolicy.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for selection policy: %w", err)
		}
	}

	// Always ensure DirectiveDefaults exists and has proper defaults
	if n.DirectiveDefaults == nil {
		n.DirectiveDefaults = &DirectiveDefaultsConfig{}
	}

	// Backward compatibility: migrate old EvmIntegrityConfig to DirectiveDefaultsConfig
	// User's explicit old config values take precedence over defaults
	if n.Evm != nil && n.Evm.Integrity != nil {
		integrity := n.Evm.Integrity
		// Only migrate if the user hasn't explicitly set the new directive
		if n.DirectiveDefaults.EnforceHighestBlock == nil && integrity.EnforceHighestBlock != nil {
			n.DirectiveDefaults.EnforceHighestBlock = integrity.EnforceHighestBlock
		}
		if n.DirectiveDefaults.EnforceGetLogsBlockRange == nil && integrity.EnforceGetLogsBlockRange != nil {
			n.DirectiveDefaults.EnforceGetLogsBlockRange = integrity.EnforceGetLogsBlockRange
		}
		if n.DirectiveDefaults.EnforceNonNullTaggedBlocks == nil && integrity.EnforceNonNullTaggedBlocks != nil {
			n.DirectiveDefaults.EnforceNonNullTaggedBlocks = integrity.EnforceNonNullTaggedBlocks
		}
	}

	if err := n.DirectiveDefaults.SetDefaults(); err != nil {
		return err
	}

	return nil
}

const DefaultEvmFinalityDepth = 1024
const DefaultEvmStatePollerDebounce = Duration(5 * time.Second)

// DefaultMarkEmptyAsErrorMethods lists the default methods for which empty/null results
// should be treated as "missing data" errors, triggering retry on other upstreams.
// Note: eth_getBlockByHash is intentionally excluded because subgraph-based upstreams
// commonly return empty for this method, which is expected behavior.
var DefaultMarkEmptyAsErrorMethods = []string{
	// Block lookups (eth_getBlockByHash excluded - subgraphs return empty for it)
	"eth_getBlockByNumber",
	"eth_getBlockReceipts",
	// Transaction lookups
	"eth_getTransactionByHash",
	"eth_getTransactionReceipt",
	"eth_getTransactionByBlockHashAndIndex",
	"eth_getTransactionByBlockNumberAndIndex",
	// Uncle/ommers (legacy API)
	"eth_getUncleByBlockHashAndIndex",
	"eth_getUncleByBlockNumberAndIndex",
	// Traces (debug/trace/parity modules)
	"debug_traceTransaction",
	"trace_transaction",
	"trace_block",
	"trace_get",
}

func (e *EvmNetworkConfig) SetDefaults() error {
	if e.FallbackFinalityDepth == 0 {
		e.FallbackFinalityDepth = DefaultEvmFinalityDepth
	}
	if e.FallbackStatePollerDebounce == 0 {
		e.FallbackStatePollerDebounce = DefaultEvmStatePollerDebounce
	}
	if e.Integrity == nil {
		e.Integrity = &EvmIntegrityConfig{}
	}
	if err := e.Integrity.SetDefaults(); err != nil {
		return err
	}

	// Defaults for network-level getLogs controls
	if e.GetLogsMaxAllowedRange == 0 {
		e.GetLogsMaxAllowedRange = 30_000
	}
	if e.GetLogsSplitOnError == nil {
		e.GetLogsSplitOnError = util.BoolPtr(true)
	}
	if e.GetLogsSplitConcurrency == 0 {
		e.GetLogsSplitConcurrency = 10
	}

	// Default methods for marking empty results as errors
	if e.MarkEmptyAsErrorMethods == nil {
		e.MarkEmptyAsErrorMethods = DefaultMarkEmptyAsErrorMethods
	}

	return nil
}

func (i *EvmIntegrityConfig) SetDefaults() error {
	if i.EnforceHighestBlock == nil {
		i.EnforceHighestBlock = util.BoolPtr(true)
	}
	if i.EnforceGetLogsBlockRange == nil {
		i.EnforceGetLogsBlockRange = util.BoolPtr(true)
	}
	if i.EnforceNonNullTaggedBlocks == nil {
		i.EnforceNonNullTaggedBlocks = util.BoolPtr(true)
	}
	return nil
}

func (f *FailsafeConfig) SetDefaults(defaults *FailsafeConfig) error {
	// Set default for MatchMethod if empty
	if f.MatchMethod == "" {
		if defaults != nil && defaults.MatchMethod != "" {
			f.MatchMethod = defaults.MatchMethod
		} else {
			f.MatchMethod = "*" // Default to match any method
		}
	}

	if f.Timeout != nil {
		if defaults != nil && defaults.Timeout != nil {
			if err := f.Timeout.SetDefaults(defaults.Timeout); err != nil {
				return err
			}
		} else {
			if err := f.Timeout.SetDefaults(nil); err != nil {
				return err
			}
		}
	} else if defaults != nil && defaults.Timeout != nil {
		f.Timeout = &TimeoutPolicyConfig{}
		if err := f.Timeout.SetDefaults(defaults.Timeout); err != nil {
			return fmt.Errorf("failed to set defaults for timeout: %w", err)
		}
	}
	if f.Retry != nil {
		if defaults != nil && defaults.Retry != nil {
			if err := f.Retry.SetDefaults(defaults.Retry); err != nil {
				return fmt.Errorf("failed to set defaults for retry: %w", err)
			}
		} else {
			if err := f.Retry.SetDefaults(nil); err != nil {
				return fmt.Errorf("failed to set defaults for retry: %w", err)
			}
		}
	} else if defaults != nil && defaults.Retry != nil {
		f.Retry = &RetryPolicyConfig{}
		if err := f.Retry.SetDefaults(defaults.Retry); err != nil {
			return fmt.Errorf("failed to set defaults for retry: %w", err)
		}
	}
	if f.Hedge != nil {
		if defaults != nil && defaults.Hedge != nil {
			if err := f.Hedge.SetDefaults(defaults.Hedge); err != nil {
				return fmt.Errorf("failed to set defaults for hedge: %w", err)
			}
		} else {
			if err := f.Hedge.SetDefaults(nil); err != nil {
				return fmt.Errorf("failed to set defaults for hedge: %w", err)
			}
		}
	} else if defaults != nil && defaults.Hedge != nil {
		f.Hedge = &HedgePolicyConfig{}
		if err := f.Hedge.SetDefaults(defaults.Hedge); err != nil {
			return fmt.Errorf("failed to set defaults for hedge: %w", err)
		}
	}
	if f.CircuitBreaker != nil {
		if defaults != nil && defaults.CircuitBreaker != nil {
			if err := f.CircuitBreaker.SetDefaults(defaults.CircuitBreaker); err != nil {
				return fmt.Errorf("failed to set defaults for circuit breaker: %w", err)
			}
		} else {
			if err := f.CircuitBreaker.SetDefaults(nil); err != nil {
				return fmt.Errorf("failed to set defaults for circuit breaker: %w", err)
			}
		}
	} else if defaults != nil && defaults.CircuitBreaker != nil {
		f.CircuitBreaker = &CircuitBreakerPolicyConfig{}
		if err := f.CircuitBreaker.SetDefaults(defaults.CircuitBreaker); err != nil {
			return fmt.Errorf("failed to set defaults for circuit breaker: %w", err)
		}
	}
	if f.Consensus != nil {
		if err := f.Consensus.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for consensus: %w", err)
		}
	}

	return nil
}

func (t *TimeoutPolicyConfig) SetDefaults(defaults *TimeoutPolicyConfig) error {
	if defaults != nil && t.Duration == 0 {
		t.Duration = defaults.Duration
	}

	return nil
}

func (r *RetryPolicyConfig) SetDefaults(defaults *RetryPolicyConfig) error {
	if r.MaxAttempts == 0 {
		if defaults != nil && defaults.MaxAttempts != 0 {
			r.MaxAttempts = defaults.MaxAttempts
		} else {
			r.MaxAttempts = 3
		}
	}
	if r.BackoffFactor == 0 {
		if defaults != nil && defaults.BackoffFactor != 0 {
			r.BackoffFactor = defaults.BackoffFactor
		} else {
			r.BackoffFactor = 1.2
		}
	}
	if r.BackoffMaxDelay == 0 {
		if defaults != nil && defaults.BackoffMaxDelay != 0 {
			r.BackoffMaxDelay = defaults.BackoffMaxDelay
		} else {
			r.BackoffMaxDelay = Duration(3 * time.Second)
		}
	}
	if r.Delay == 0 {
		if defaults != nil && defaults.Delay != 0 {
			r.Delay = defaults.Delay
		} else {
			r.Delay = Duration(0 * time.Millisecond)
		}
	}
	if r.Jitter == 0 {
		if defaults != nil && defaults.Jitter != 0 {
			r.Jitter = defaults.Jitter
		} else {
			r.Jitter = Duration(0 * time.Millisecond)
		}
	}
	// Only set EmptyResultConfidence if provided through defaults
	if r.EmptyResultConfidence == 0 && defaults != nil && defaults.EmptyResultConfidence != 0 {
		r.EmptyResultConfidence = defaults.EmptyResultConfidence
	}
	// Only set EmptyResultIgnore if provided through defaults
	if r.EmptyResultIgnore == nil && defaults != nil && defaults.EmptyResultIgnore != nil {
		r.EmptyResultIgnore = defaults.EmptyResultIgnore
	}

	// Default EmptyResultMaxAttempts to MaxAttempts if not set
	if r.EmptyResultMaxAttempts == 0 {
		if defaults != nil && defaults.EmptyResultMaxAttempts != 0 {
			r.EmptyResultMaxAttempts = defaults.EmptyResultMaxAttempts
		}
	}

	return nil
}

func (h *HedgePolicyConfig) SetDefaults(defaults *HedgePolicyConfig) error {
	if h.Delay == 0 {
		if defaults != nil && defaults.Delay != 0 {
			h.Delay = defaults.Delay
		} else {
			h.Delay = Duration(0)
		}
	}
	if h.Quantile == 0 {
		if defaults != nil && defaults.Quantile != 0 {
			h.Quantile = defaults.Quantile
		}
	}
	if h.MinDelay == 0 {
		if defaults != nil && defaults.MinDelay != 0 {
			h.MinDelay = defaults.MinDelay
		} else {
			h.MinDelay = Duration(100 * time.Millisecond)
		}
	}
	if h.MaxDelay == 0 {
		if defaults != nil && defaults.MaxDelay != 0 {
			h.MaxDelay = defaults.MaxDelay
		} else {
			// Intentionally high, so it never hits in practical scenarios
			h.MaxDelay = Duration(999 * time.Second)
		}
	}
	if h.MaxCount == 0 {
		if defaults != nil && defaults.MaxCount != 0 {
			h.MaxCount = defaults.MaxCount
		} else {
			h.MaxCount = 1
		}
	}

	return nil
}

func (c *CircuitBreakerPolicyConfig) SetDefaults(defaults *CircuitBreakerPolicyConfig) error {
	if c.FailureThresholdCount == 0 {
		if defaults != nil && defaults.FailureThresholdCount != 0 {
			c.FailureThresholdCount = defaults.FailureThresholdCount
		} else {
			c.FailureThresholdCount = 20
		}
	}
	if c.FailureThresholdCapacity == 0 {
		if defaults != nil && defaults.FailureThresholdCapacity != 0 {
			c.FailureThresholdCapacity = defaults.FailureThresholdCapacity
		} else {
			c.FailureThresholdCapacity = 80
		}
	}
	if c.SuccessThresholdCapacity == 0 {
		if defaults != nil && defaults.SuccessThresholdCapacity != 0 {
			c.SuccessThresholdCapacity = defaults.SuccessThresholdCapacity
		} else {
			c.SuccessThresholdCapacity = 200
		}
	}
	if c.HalfOpenAfter == 0 {
		if defaults != nil && defaults.HalfOpenAfter != 0 {
			c.HalfOpenAfter = defaults.HalfOpenAfter
		} else {
			c.HalfOpenAfter = Duration(5 * time.Minute)
		}
	}
	if c.SuccessThresholdCount == 0 {
		if defaults != nil && defaults.SuccessThresholdCount != 0 {
			c.SuccessThresholdCount = defaults.SuccessThresholdCount
		} else {
			c.SuccessThresholdCount = 8
		}
	}
	if c.SuccessThresholdCapacity == 0 {
		if defaults != nil && defaults.SuccessThresholdCapacity != 0 {
			c.SuccessThresholdCapacity = defaults.SuccessThresholdCapacity
		} else {
			c.SuccessThresholdCapacity = 10
		}
	}

	return nil
}

func (c *ConsensusPolicyConfig) SetDefaults() error {
	if c.MaxParticipants == 0 {
		c.MaxParticipants = 5
	}
	if c.AgreementThreshold == 0 {
		c.AgreementThreshold = 2
	}
	if c.DisputeBehavior == "" {
		c.DisputeBehavior = ConsensusDisputeBehaviorReturnError
	}
	if c.LowParticipantsBehavior == "" {
		c.LowParticipantsBehavior = ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult
	}
	if c.DisputeLogLevel == "" {
		c.DisputeLogLevel = "warn"
	}
	if c.IgnoreFields == nil {
		c.IgnoreFields = map[string][]string{
			"eth_getLogs": {
				"*.blockTimestamp",
			},
			"eth_getTransactionReceipt": {
				"blockTimestamp",
				"logs.*.blockTimestamp",
			},
			"eth_getBlockReceipts": {
				"*.blockTimestamp",
				"*.logs.*.blockTimestamp",
			},
		}
	}
	if c.PreferNonEmpty == nil {
		c.PreferNonEmpty = util.BoolPtr(true)
	}
	if c.PreferLargerResponses == nil {
		c.PreferLargerResponses = util.BoolPtr(true)
	}

	// Destination defaults
	if c.MisbehaviorsDestination != nil {
		if err := c.MisbehaviorsDestination.SetDefaults(); err != nil {
			return fmt.Errorf("consensus.misbehaviorsDestination defaults: %w", err)
		}
	}

	return nil
}

// SetDefaults applies defaults for misbehavior export destination
func (c *MisbehaviorsDestinationConfig) SetDefaults() error {
	// Default type: file
	if c.Type == "" {
		c.Type = MisbehaviorsDestinationTypeFile
	}
	// Default file pattern handled in resolver at use-site; keep here for explicitness
	if c.FilePattern == "" {
		c.FilePattern = "{timestampMs}-{method}-{networkId}"
	}
	// S3 defaults
	if c.Type == MisbehaviorsDestinationTypeS3 {
		if c.S3 == nil {
			c.S3 = &S3FlushConfig{}
		}
		if c.S3.MaxRecords == 0 {
			c.S3.MaxRecords = 100
		}
		if c.S3.MaxSize == 0 {
			c.S3.MaxSize = 1 * 1024 * 1024 // 1MB
		}
		if c.S3.FlushInterval == 0 {
			c.S3.FlushInterval = Duration(60 * time.Second)
		}
		if c.S3.ContentType == "" {
			c.S3.ContentType = "application/jsonl"
		}
		// Region left empty → picked from env/IMDS by SDK; keep as-is
	}
	return nil
}

func (r *RateLimitAutoTuneConfig) SetDefaults() error {
	if r.Enabled == nil {
		r.Enabled = util.BoolPtr(true)
	}
	if r.AdjustmentPeriod == 0 {
		r.AdjustmentPeriod = Duration(1 * time.Minute)
	}
	if r.ErrorRateThreshold == 0 {
		r.ErrorRateThreshold = 0.1
	}
	if r.IncreaseFactor == 0 {
		r.IncreaseFactor = 1.05
	}
	if r.DecreaseFactor == 0 {
		r.DecreaseFactor = 0.95
	}
	if r.MaxBudget == 0 {
		r.MaxBudget = 100000
	}

	return nil
}

func (r *RoutingConfig) SetDefaults() error {
	if len(r.ScoreMultipliers) == 0 {
		r.ScoreMultipliers = []*ScoreMultiplierConfig{
			// For realtime/unfinalized: prioritize block lag (need fresh data)
			{
				Network:  "*",
				Method:   "*",
				Finality: []DataFinalityState{DataFinalityStateRealtime, DataFinalityStateUnfinalized},

				ErrorRate:       util.Float64Ptr(4.0),
				RespLatency:     util.Float64Ptr(6.0),
				TotalRequests:   util.Float64Ptr(1.0),
				ThrottledRate:   util.Float64Ptr(3.0),
				BlockHeadLag:    util.Float64Ptr(8.0),
				FinalizationLag: util.Float64Ptr(2.0),
				Misbehaviors:    util.Float64Ptr(5.0),
				Overall:         util.Float64Ptr(1.0),
			},
			// For finalized/unknown: prioritize latency (block lag doesn't matter)
			// Even though "unknown" might include requests for tx hashes or block hashes
			// of tip-of-chain range, they'll be retried due to retryEmpty logic if
			//  an upstream doesn't have the data. This is not ideal for numeric getBlockByNumber calls.
			{
				Network:  "*",
				Method:   "*",
				Finality: []DataFinalityState{DataFinalityStateFinalized, DataFinalityStateUnknown},

				ErrorRate:       util.Float64Ptr(4.0),
				RespLatency:     util.Float64Ptr(8.0),
				TotalRequests:   util.Float64Ptr(1.0),
				ThrottledRate:   util.Float64Ptr(3.0),
				BlockHeadLag:    util.Float64Ptr(2.0),
				FinalizationLag: util.Float64Ptr(1.0),
				Misbehaviors:    util.Float64Ptr(5.0),
				Overall:         util.Float64Ptr(1.0),
			},
		}
	} else {
		for _, multiplier := range r.ScoreMultipliers {
			if err := multiplier.SetDefaults(); err != nil {
				return fmt.Errorf("failed to set defaults for score multiplier: %w", err)
			}
		}
	}
	if r.ScoreLatencyQuantile == 0 {
		r.ScoreLatencyQuantile = 0.70
	}

	return nil
}

var DefaultScoreMultiplier = &ScoreMultiplierConfig{
	Network: "*",
	Method:  "*",
	// Finality: nil means match all finality states

	ErrorRate:       util.Float64Ptr(4.0),
	RespLatency:     util.Float64Ptr(8.0),
	TotalRequests:   util.Float64Ptr(1.0),
	ThrottledRate:   util.Float64Ptr(3.0),
	BlockHeadLag:    util.Float64Ptr(2.0),
	FinalizationLag: util.Float64Ptr(1.0),
	Misbehaviors:    util.Float64Ptr(5.0),

	Overall: util.Float64Ptr(1.0),
}

func (s *ScoreMultiplierConfig) SetDefaults() error {
	if s.Network == "" {
		s.Network = DefaultScoreMultiplier.Network
	}
	if s.Method == "" {
		s.Method = DefaultScoreMultiplier.Method
	}
	if s.ErrorRate == nil {
		s.ErrorRate = DefaultScoreMultiplier.ErrorRate
	}
	if s.RespLatency == nil {
		s.RespLatency = DefaultScoreMultiplier.RespLatency
	}
	if s.TotalRequests == nil {
		s.TotalRequests = DefaultScoreMultiplier.TotalRequests
	}
	if s.ThrottledRate == nil {
		s.ThrottledRate = DefaultScoreMultiplier.ThrottledRate
	}
	if s.BlockHeadLag == nil {
		s.BlockHeadLag = DefaultScoreMultiplier.BlockHeadLag
	}
	if s.FinalizationLag == nil {
		s.FinalizationLag = DefaultScoreMultiplier.FinalizationLag
	}
	if s.Misbehaviors == nil {
		s.Misbehaviors = DefaultScoreMultiplier.Misbehaviors
	}
	if s.Overall == nil {
		s.Overall = DefaultScoreMultiplier.Overall
	}

	return nil
}

const DefaultPolicyFunction = `
	(upstreams, method) => {
		const defaults = upstreams.filter(u => u.config.group !== 'fallback')
		const fallbacks = upstreams.filter(u => u.config.group === 'fallback')

		const maxErrorRate = parseFloat(process.env.ROUTING_POLICY_MAX_ERROR_RATE || '0.7')
		const maxBlockHeadLag = parseFloat(process.env.ROUTING_POLICY_MAX_BLOCK_HEAD_LAG || '10')
		const minHealthyThreshold = parseInt(process.env.ROUTING_POLICY_MIN_HEALTHY_THRESHOLD || '1')

		const healthyOnes = defaults.filter(
			u => u.metrics.errorRate < maxErrorRate && u.metrics.blockHeadLag < maxBlockHeadLag
		)

		if (healthyOnes.length >= minHealthyThreshold) {
			return healthyOnes
		}

		if (fallbacks.length > 0) {
			let healthyFallbacks = fallbacks.filter(
				u => u.metrics.errorRate < maxErrorRate && u.metrics.blockHeadLag < maxBlockHeadLag
			)

			if (healthyFallbacks.length > 0) {
				return healthyFallbacks
			}
		}

		// The reason all upstreams are returned is to be less harsh and still consider default nodes (in case they have intermittent issues)
		// Order of upstreams does not matter as that will be decided by the upstream scoring mechanism
		return upstreams
	}
`

func (c *SelectionPolicyConfig) SetDefaults() error {
	if c.EvalInterval == 0 {
		c.EvalInterval = Duration(1 * time.Minute)
	}
	if c.EvalFunction == nil {
		evalFunction, err := CompileFunction(DefaultPolicyFunction)
		if err != nil {
			// This should never happen with the default function - it's a programming error
			return fmt.Errorf("failed to compile default selection policy function: %w", err)
		}
		c.EvalFunction = evalFunction
	}
	if c.ResampleExcluded {
		if c.ResampleInterval == 0 {
			c.ResampleInterval = Duration(5 * time.Minute)
		}
		if c.ResampleCount == 0 {
			c.ResampleCount = 10
		}
	}

	return nil
}

func (a *AuthConfig) SetDefaults() error {
	if a.Strategies != nil {
		for _, strategy := range a.Strategies {
			if err := strategy.SetDefaults(); err != nil {
				return fmt.Errorf("failed to set defaults for auth strategy: %w", err)
			}
		}
	}

	return nil
}

func (s *AuthStrategyConfig) SetDefaults() error {
	if s.Type == AuthTypeNetwork && s.Network == nil {
		s.Network = &NetworkStrategyConfig{}
	}
	if s.Network != nil {
		if err := s.Network.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for network strategy: %w", err)
		}
	}

	if s.Type == AuthTypeSecret && s.Secret == nil {
		s.Secret = &SecretStrategyConfig{}
	}
	if s.Secret != nil {
		s.Type = AuthTypeSecret
		if err := s.Secret.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for secret strategy: %w", err)
		}
	}
	if s.Type == AuthTypeDatabase && s.Database == nil {
		s.Database = &DatabaseStrategyConfig{}
	}
	if s.Database != nil {
		s.Type = AuthTypeDatabase
		if err := s.Database.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for database strategy: %w", err)
		}
	}

	if s.Type == AuthTypeJwt && s.Jwt == nil {
		s.Jwt = &JwtStrategyConfig{}
	}
	if s.Jwt != nil {
		s.Type = AuthTypeJwt
		if err := s.Jwt.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for jwt strategy: %w", err)
		}
	}

	if s.Type == AuthTypeSiwe && s.Siwe == nil {
		s.Siwe = &SiweStrategyConfig{}
	}
	if s.Siwe != nil {
		s.Type = AuthTypeSiwe
		if err := s.Siwe.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for siwe strategy: %w", err)
		}
	}

	return nil
}

func (s *DatabaseStrategyConfig) SetDefaults() error {
	if s.Connector == nil {
		s.Connector = &ConnectorConfig{}
	}

	if s.Cache == nil {
		s.Cache = &DatabaseStrategyCacheConfig{}
	}

	if s.Retry == nil {
		s.Retry = &DatabaseRetryConfig{}
	}
	if s.FailOpen == nil {
		s.FailOpen = &DatabaseFailOpenConfig{}
	}

	if s.Cache.TTL == nil {
		defaultTTL := time.Hour
		s.Cache.TTL = &defaultTTL
	}

	if s.Cache.MaxSize == nil {
		defaultMaxSize := int64(10000)
		s.Cache.MaxSize = &defaultMaxSize
	}

	if s.Cache.MaxCost == nil {
		defaultMaxCost := int64(1 << 30) // 1GB
		s.Cache.MaxCost = &defaultMaxCost
	}

	if s.Cache.NumCounters == nil {
		defaultNumCounters := int64(100000)
		s.Cache.NumCounters = &defaultNumCounters
	}

	if err := s.Retry.SetDefaults(); err != nil {
		return fmt.Errorf("failed to set defaults for database retry: %w", err)
	}
	if err := s.FailOpen.SetDefaults(); err != nil {
		return fmt.Errorf("failed to set defaults for database failOpen: %w", err)
	}

	if s.MaxWait == 0 {
		s.MaxWait = Duration(1 * time.Second)
	}

	return s.Connector.SetDefaults(connectorScopeAuth)
}

func (r *DatabaseRetryConfig) SetDefaults() error {
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = 3
	}
	if r.BaseBackoff.Duration() == 0 {
		r.BaseBackoff = Duration(100 * time.Millisecond)
	}
	return nil
}

func (f *DatabaseFailOpenConfig) SetDefaults() error {
	// Disabled by default
	// Only set userId default; keep Enabled as-is
	if f.UserId == "" {
		f.UserId = "emergency-failopen"
	}
	return nil
}

func (s *SecretStrategyConfig) SetDefaults() error {
	return nil
}

func (j *JwtStrategyConfig) SetDefaults() error {
	if j.RateLimitBudgetClaimName == "" {
		j.RateLimitBudgetClaimName = "rlm"
	}
	return nil
}

func (s *SiweStrategyConfig) SetDefaults() error {
	return nil
}

func (n *NetworkStrategyConfig) SetDefaults() error {
	return nil
}

func (r *RateLimiterConfig) SetDefaults() error {
	if r.Store == nil {
		r.Store = &RateLimitStoreConfig{}
	}
	if err := r.Store.SetDefaults(); err != nil {
		return fmt.Errorf("failed to set defaults for rate limit store: %w", err)
	}
	if len(r.Budgets) > 0 {
		for _, budget := range r.Budgets {
			if err := budget.SetDefaults(); err != nil {
				return fmt.Errorf("failed to set defaults for rate limit budget: %w", err)
			}
		}
	}

	return nil
}

func (r *RateLimitStoreConfig) SetDefaults() error {
	if r.Driver == "" {
		r.Driver = "memory"
	}
	if r.Driver == "redis" {
		if r.Redis == nil {
			r.Redis = &RedisConnectorConfig{}
		}
		if err := r.Redis.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for redis connector: %w", err)
		}
	}
	return nil
}

func (b *RateLimitBudgetConfig) SetDefaults() error {
	if len(b.Rules) > 0 {
		for _, rule := range b.Rules {
			if err := rule.SetDefaults(); err != nil {
				return fmt.Errorf("failed to set defaults for rate limit rule: %w", err)
			}
		}
	}

	return nil
}

func (r *RateLimitRuleConfig) SetDefaults() error {
	// Default to 'second' when period is unset or invalid
	switch r.Period {
	case RateLimitPeriodSecond, RateLimitPeriodMinute, RateLimitPeriodHour, RateLimitPeriodDay,
		RateLimitPeriodWeek, RateLimitPeriodMonth, RateLimitPeriodYear:
		// ok
	default:
		r.Period = RateLimitPeriodSecond
	}
	if r.Method == "" {
		r.Method = "*"
	}

	return nil
}

func (c *CORSConfig) SetDefaults() error {
	if c.AllowedOrigins == nil {
		c.AllowedOrigins = []string{"*"}
	}
	if c.AllowedMethods == nil {
		c.AllowedMethods = []string{"GET", "POST", "OPTIONS"}
	}
	if c.AllowedHeaders == nil {
		c.AllowedHeaders = []string{
			"content-type",
			"authorization",
			"x-erpc-secret-token",
		}
	}
	if c.AllowCredentials == nil {
		c.AllowCredentials = util.BoolPtr(false)
	}
	if c.MaxAge == 0 {
		c.MaxAge = 3600
	}

	return nil
}

func NewDefaultNetworkConfig(upstreams []*UpstreamConfig) *NetworkConfig {
	hasAnyFallbackUpstream := slices.ContainsFunc(upstreams, func(u *UpstreamConfig) bool {
		return u.Group == "fallback"
	})
	n := &NetworkConfig{}
	if hasAnyFallbackUpstream {
		evalFunction, err := CompileFunction(DefaultPolicyFunction)
		if err != nil {
			// This should never happen with the default function - it's a programming error
			panic(fmt.Sprintf("failed to compile default selection policy function: %v", err))
		}

		selectionPolicy := &SelectionPolicyConfig{
			EvalInterval:     Duration(1 * time.Minute),
			EvalFunction:     evalFunction,
			EvalPerMethod:    false,
			ResampleInterval: Duration(5 * time.Minute),
			ResampleCount:    10,

			evalFunctionOriginal: DefaultPolicyFunction,
		}

		n.SelectionPolicy = selectionPolicy
	}
	return n
}
