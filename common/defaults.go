package common

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/erpc/erpc/common/script"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
)

func (c *Config) SetDefaults() error {
	if c.LogLevel == "" {
		c.LogLevel = "INFO"
	}
	if c.Server == nil {
		c.Server = &ServerConfig{}
	}
	if err := c.Server.SetDefaults(); err != nil {
		return err
	}

	if c.Database != nil {
		if err := c.Database.SetDefaults(); err != nil {
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
			if err := project.SetDefaults(); err != nil {
				return err
			}
		}
	}
	if len(c.Projects) == 0 {
		log.Warn().Msg("no projects found in config; will add a default 'main' project")
		c.Projects = []*ProjectConfig{
			{
				Id: "main",
				NetworkDefaults: &NetworkDefaults{
					Failsafe: &FailsafeConfig{
						Retry: &RetryPolicyConfig{
							MaxAttempts: 3,
							Delay:       "0",
							Jitter:      "0",
						},
						Timeout: &TimeoutPolicyConfig{
							Duration: "60s",
						},
						Hedge: &HedgePolicyConfig{
							Quantile: 0.9,
							MaxCount: 3,
							MinDelay: "500ms",
						},
					},
				},
				UpstreamDefaults: &UpstreamConfig{
					Evm: &EvmUpstreamConfig{
						GetLogsMaxBlockRange: 500,
					},
					Failsafe: &FailsafeConfig{
						Retry: &RetryPolicyConfig{
							MaxAttempts: 2,
							Delay:       "500ms",
						},
						Timeout: &TimeoutPolicyConfig{
							Duration: "20s",
						},
						CircuitBreaker: &CircuitBreakerPolicyConfig{
							FailureThresholdCount:    8,
							FailureThresholdCapacity: 10,
							HalfOpenAfter:            "5m",
							SuccessThresholdCount:    5,
							SuccessThresholdCapacity: 5,
						},
					},
				},
			},
		}
		err := c.Projects[0].SetDefaults()
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
	},
	"eth_getBlockByHash": {
		ReqRefs:  FirstParam,
		RespRefs: NumberOrHashParam,
	},
	"eth_getBlockByNumber": {
		ReqRefs:  FirstParam,
		RespRefs: NumberOrHashParam,
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
	"eth_estimateGas": {
		ReqRefs: SecondParam,
	},
	"debug_traceCall": {
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
		ReqRefs:  FirstParam,
		RespRefs: BlockNumberOrBlockHashParam,
	},
	"trace_block": {
		ReqRefs: FirstParam,
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
			if err := connector.SetDefaults(); err != nil {
				return fmt.Errorf("failed to set defaults for cache connector: %w", err)
			}
		}
	}

	if c.Methods == nil {
		// Merge all default methods into a single map
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
		c.Methods = mergedMethods
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

	return nil
}

func (s *ServerConfig) SetDefaults() error {
	if s.ListenV4 == nil {
		if !util.IsTest() {
			s.ListenV4 = util.BoolPtr(true)
		}
	}
	if s.HttpHostV4 == nil {
		s.HttpHostV4 = util.StringPtr("0.0.0.0")
	}
	if s.HttpHostV6 == nil {
		s.HttpHostV6 = util.StringPtr("[::]")
	}
	if s.HttpPort == nil {
		s.HttpPort = util.IntPtr(4000)
	}
	if s.MaxTimeout == nil {
		s.MaxTimeout = util.StringPtr("150s")
	}
	if s.ReadTimeout == nil {
		s.ReadTimeout = util.StringPtr("30s")
	}
	if s.WriteTimeout == nil {
		s.WriteTimeout = util.StringPtr("120s")
	}
	if s.EnableGzip == nil {
		s.EnableGzip = util.BoolPtr(true)
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

func (d *DatabaseConfig) SetDefaults() error {
	if d.EvmJsonRpcCache != nil {
		if err := d.EvmJsonRpcCache.SetDefaults(); err != nil {
			return err
		}
	}

	return nil
}

func (c *ConnectorConfig) SetDefaults() error {
	if c.Driver == "" {
		return nil
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
		if err := c.PostgreSQL.SetDefaults(); err != nil {
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
		if err := c.DynamoDB.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for dynamo db connector: %w", err)
		}
	}

	return nil
}

func (m *MemoryConnectorConfig) SetDefaults() error {
	if m.MaxItems == 0 {
		m.MaxItems = 100000
	}

	return nil
}

func (r *RedisConnectorConfig) SetDefaults() error {
	if r.Addr == "" {
		r.Addr = "localhost:6379"
	}
	if strings.HasPrefix(r.Addr, "rediss://") {
		r.TLS = &TLSConfig{
			Enabled: true,
		}
	}
	r.Addr = strings.TrimPrefix(r.Addr, "rediss://")
	r.Addr = strings.TrimPrefix(r.Addr, "redis://")
	if r.ConnPoolSize == 0 {
		r.ConnPoolSize = 128
	}
	if r.InitTimeout == 0 {
		r.InitTimeout = 5 * time.Second
	}
	if r.GetTimeout == 0 {
		r.GetTimeout = 1 * time.Second
	}
	if r.SetTimeout == 0 {
		r.SetTimeout = 2 * time.Second
	}

	return nil
}

func (p *PostgreSQLConnectorConfig) SetDefaults() error {
	if p.Table == "" {
		p.Table = "erpc_json_rpc_cache"
	}
	if p.MinConns == 0 {
		p.MinConns = 4
	}
	if p.MaxConns == 0 {
		p.MaxConns = 32
	}
	if p.InitTimeout == 0 {
		p.InitTimeout = 5 * time.Second
	}
	if p.GetTimeout == 0 {
		p.GetTimeout = 1 * time.Second
	}
	if p.SetTimeout == 0 {
		p.SetTimeout = 2 * time.Second
	}

	return nil
}

func (d *DynamoDBConnectorConfig) SetDefaults() error {
	if d.Table == "" {
		d.Table = "erpc_json_rpc_cache"
	}
	if d.PartitionKeyName == "" {
		d.PartitionKeyName = "groupKey"
	}
	if d.RangeKeyName == "" {
		d.RangeKeyName = "requestKey"
	}
	if d.ReverseIndexName == "" {
		d.ReverseIndexName = "idx_groupKey_requestKey"
	}
	if d.TTLAttributeName == "" {
		d.TTLAttributeName = "ttl"
	}
	if d.InitTimeout == 0 {
		d.InitTimeout = 5 * time.Second
	}
	if d.GetTimeout == 0 {
		d.GetTimeout = 1 * time.Second
	}
	if d.SetTimeout == 0 {
		d.SetTimeout = 2 * time.Second
	}

	return nil
}

func (p *ProjectConfig) SetDefaults() error {
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
				return fmt.Errorf("failed to convert upstream to provider: %w", err)
			} else if provider != nil {
				p.Providers = append(p.Providers, provider)
				p.Upstreams = append(p.Upstreams[:i], p.Upstreams[i+1:]...)
				i--
			}
		}
	}
	if len(p.Providers) == 0 && len(p.Upstreams) == 0 {
		log.Warn().Msg("no providers or upstreams found in project; will use default 'public' endpoints repository")
		repositoryCfg := &ProviderConfig{
			Id:     "public",
			Vendor: "repository",
			// Let the vendor use the default repository URL
		}
		if err := repositoryCfg.SetDefaults(nil); err != nil {
			return fmt.Errorf("failed to set defaults for repository provider: %w", err)
		}
		p.Providers = append(p.Providers, repositoryCfg)
	}
	if p.Networks != nil {
		for _, network := range p.Networks {
			if err := network.SetDefaults(p.Upstreams, p.NetworkDefaults); err != nil {
				return fmt.Errorf("failed to set defaults for network: %w", err)
			}
		}
	}
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
	if p.HealthCheck == nil {
		p.HealthCheck = &HealthCheckConfig{}
	}
	if err := p.HealthCheck.SetDefaults(); err != nil {
		return fmt.Errorf("failed to set defaults for health check: %w", err)
	}

	return nil
}

func convertUpstreamToProvider(upstream *UpstreamConfig) (*ProviderConfig, error) {
	if strings.HasPrefix(upstream.Endpoint, "http://") || strings.HasPrefix(upstream.Endpoint, "https://") {
		return nil, nil
	}

	endpoint, err := url.Parse(upstream.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream endpoint while converting to provider: %w", err)
	}

	vendorName := strings.Replace(endpoint.Scheme, "evm+", "", 1)
	settings, err := buildProviderSettings(vendorName, endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to build provider settings: %w", err)
	}

	cfg := &ProviderConfig{
		Id:                 util.RedactEndpoint(upstream.Endpoint),
		Vendor:             vendorName,
		Settings:           settings,
		UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
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
	case "repository", "evm+repository":
		return VendorSettings{
			"repositoryUrl": "https://" + endpoint.Host,
		}, nil
	}

	return nil, fmt.Errorf("unsupported vendor name in vendor.settings: %s", vendorName)
}

func (n *NetworkDefaults) SetDefaults() error {
	if n.Failsafe != nil {
		if err := n.Failsafe.SetDefaults(nil); err != nil {
			return fmt.Errorf("failed to set defaults for failsafe: %w", err)
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
		if err := u.Evm.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for evm upstream: %w", err)
		}
	} else if u.Evm != nil && defaults.Evm != nil {
		if u.Evm.StatePollerInterval == "" && defaults.Evm.StatePollerInterval != "" {
			u.Evm.StatePollerInterval = defaults.Evm.StatePollerInterval
		}
		if u.Evm.StatePollerDebounce == "" && defaults.Evm.StatePollerDebounce != "" {
			u.Evm.StatePollerDebounce = defaults.Evm.StatePollerDebounce
		}
		if u.Evm.MaxAvailableRecentBlocks == 0 && defaults.Evm.MaxAvailableRecentBlocks != 0 {
			u.Evm.MaxAvailableRecentBlocks = defaults.Evm.MaxAvailableRecentBlocks
		}
		if u.Evm.GetLogsMaxBlockRange == 0 && defaults.Evm.GetLogsMaxBlockRange != 0 {
			u.Evm.GetLogsMaxBlockRange = defaults.Evm.GetLogsMaxBlockRange
		}
	}
	if u.JsonRpc == nil && defaults.JsonRpc != nil {
		u.JsonRpc = &JsonRpcUpstreamConfig{
			SupportsBatch: defaults.JsonRpc.SupportsBatch,
			BatchMaxSize:  defaults.JsonRpc.BatchMaxSize,
			BatchMaxWait:  defaults.JsonRpc.BatchMaxWait,
			EnableGzip:    defaults.JsonRpc.EnableGzip,
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
		u.Id = util.RedactEndpoint(u.Endpoint)
	}
	if u.Type == "" {
		// TODO make actual calls to detect other types (solana, btc, etc)?
		u.Type = UpstreamTypeEvm
	}

	if u.Failsafe != nil {
		if defaults != nil && defaults.Failsafe != nil {
			if err := u.Failsafe.SetDefaults(defaults.Failsafe); err != nil {
				return fmt.Errorf("failed to set defaults for failsafe: %w", err)
			}
		} else {
			if err := u.Failsafe.SetDefaults(nil); err != nil {
				return fmt.Errorf("failed to set defaults for failsafe: %w", err)
			}
		}
	} else if defaults != nil && defaults.Failsafe != nil {
		u.Failsafe = &FailsafeConfig{}
		if err := u.Failsafe.SetDefaults(defaults.Failsafe); err != nil {
			return fmt.Errorf("failed to set defaults for failsafe: %w", err)
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
		if err := u.Evm.SetDefaults(); err != nil {
			return fmt.Errorf("failed to set defaults for evm: %w", err)
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

func (e *EvmUpstreamConfig) SetDefaults() error {
	if e.StatePollerInterval == "" {
		e.StatePollerInterval = "30s"
	}

	if e.NodeType == "" {
		e.NodeType = EvmNodeTypeArchive
	}

	if e.MaxAvailableRecentBlocks == 0 {
		switch e.NodeType {
		case EvmNodeTypeFull:
			e.MaxAvailableRecentBlocks = 128
		}
	}

	// TODO Enable this after more production testing
	// if e.GetLogsMaxBlockRange == 0 {
	// 	e.GetLogsMaxBlockRange = 500
	// }

	return nil
}

func (j *JsonRpcUpstreamConfig) SetDefaults() error {
	return nil
}

func (n *NetworkConfig) SetDefaults(upstreams []*UpstreamConfig, defaults *NetworkDefaults) error {
	sysDefCfg := NewDefaultNetworkConfig(upstreams)
	if defaults != nil {
		if n.RateLimitBudget == "" {
			n.RateLimitBudget = defaults.RateLimitBudget
		}
		if defaults.Failsafe != nil {
			if n.Failsafe == nil {
				n.Failsafe = &FailsafeConfig{}
				*n.Failsafe = *defaults.Failsafe
			} else {
				if err := n.Failsafe.SetDefaults(defaults.Failsafe); err != nil {
					return fmt.Errorf("failed to set defaults for failsafe: %w", err)
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
			if n.Evm.FallbackStatePollerDebounce == "" && defaults.Evm.FallbackStatePollerDebounce != "" {
				n.Evm.FallbackStatePollerDebounce = defaults.Evm.FallbackStatePollerDebounce
			}
			if n.Evm.FallbackFinalityDepth == 0 && defaults.Evm.FallbackFinalityDepth != 0 {
				n.Evm.FallbackFinalityDepth = defaults.Evm.FallbackFinalityDepth
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
	} else if n.Failsafe != nil {
		if err := n.Failsafe.SetDefaults(sysDefCfg.Failsafe); err != nil {
			return fmt.Errorf("failed to set defaults for failsafe: %w", err)
		}
	} else {
		n.Failsafe = sysDefCfg.Failsafe
	}

	if n.Architecture == "" {
		if n.Evm != nil {
			n.Architecture = "evm"
		}
	}

	if n.Architecture == "evm" && n.Evm == nil {
		n.Evm = &EvmNetworkConfig{}
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

	return nil
}

const DefaultEvmFinalityDepth = 1024
const DefaultEvmStatePollerDebounce = "5s"

func (e *EvmNetworkConfig) SetDefaults() error {
	if e.FallbackFinalityDepth == 0 {
		e.FallbackFinalityDepth = DefaultEvmFinalityDepth
	}
	if e.FallbackStatePollerDebounce == "" {
		e.FallbackStatePollerDebounce = DefaultEvmStatePollerDebounce
	}
	if e.Integrity == nil {
		e.Integrity = &EvmIntegrityConfig{}
	}
	if err := e.Integrity.SetDefaults(); err != nil {
		return err
	}

	return nil
}

func (i *EvmIntegrityConfig) SetDefaults() error {
	// TODO After testing for a while, we can set these to true by default
	if i.EnforceHighestBlock == nil {
		i.EnforceHighestBlock = util.BoolPtr(false)
	}
	if i.EnforceGetLogsBlockRange == nil {
		i.EnforceGetLogsBlockRange = util.BoolPtr(false)
	}
	return nil
}

func (f *FailsafeConfig) SetDefaults(defaults *FailsafeConfig) error {
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

	return nil
}

func (t *TimeoutPolicyConfig) SetDefaults(defaults *TimeoutPolicyConfig) error {
	if defaults != nil && t.Duration == "" {
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
	if r.BackoffMaxDelay == "" {
		if defaults != nil && defaults.BackoffMaxDelay != "" {
			r.BackoffMaxDelay = defaults.BackoffMaxDelay
		} else {
			r.BackoffMaxDelay = "3s"
		}
	}
	if r.Delay == "" {
		if defaults != nil && defaults.Delay != "" {
			r.Delay = defaults.Delay
		} else {
			r.Delay = "100ms"
		}
	}
	if r.Jitter == "" {
		if defaults != nil && defaults.Jitter != "" {
			r.Jitter = defaults.Jitter
		} else {
			r.Jitter = "0ms"
		}
	}

	return nil
}

func (h *HedgePolicyConfig) SetDefaults(defaults *HedgePolicyConfig) error {
	if h.Delay == "" {
		if defaults != nil && defaults.Delay != "" {
			h.Delay = defaults.Delay
		} else {
			h.Delay = "0ms"
		}
	}
	if h.Quantile == 0 {
		if defaults != nil && defaults.Quantile != 0 {
			h.Quantile = defaults.Quantile
		}
	}
	if h.MinDelay == "" {
		if defaults != nil && defaults.MinDelay != "" {
			h.MinDelay = defaults.MinDelay
		} else {
			h.MinDelay = "100ms"
		}
	}
	if h.MaxDelay == "" {
		if defaults != nil && defaults.MaxDelay != "" {
			h.MaxDelay = defaults.MaxDelay
		} else {
			// Intentionally high, so it never hits in practical scenarios
			h.MaxDelay = "999s"
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
	if c.HalfOpenAfter == "" {
		if defaults != nil && defaults.HalfOpenAfter != "" {
			c.HalfOpenAfter = defaults.HalfOpenAfter
		} else {
			c.HalfOpenAfter = "5m"
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

func (r *RateLimitAutoTuneConfig) SetDefaults() error {
	if r.Enabled == nil {
		r.Enabled = util.BoolPtr(true)
	}
	if r.AdjustmentPeriod == "" {
		r.AdjustmentPeriod = "1m"
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
	if r.ScoreMultipliers != nil {
		for _, multiplier := range r.ScoreMultipliers {
			if err := multiplier.SetDefaults(); err != nil {
				return fmt.Errorf("failed to set defaults for score multiplier: %w", err)
			}
		}
	}

	return nil
}

var DefaultScoreMultiplier = &ScoreMultiplierConfig{
	Network: "*",
	Method:  "*",

	ErrorRate:       8.0,
	P90Latency:      4.0,
	TotalRequests:   1.0,
	ThrottledRate:   3.0,
	BlockHeadLag:    2.0,
	FinalizationLag: 1.0,

	Overall: 1.0,
}

func (s *ScoreMultiplierConfig) SetDefaults() error {
	if s.Network == "" {
		s.Network = DefaultScoreMultiplier.Network
	}
	if s.Method == "" {
		s.Method = DefaultScoreMultiplier.Method
	}
	if s.ErrorRate == 0 {
		s.ErrorRate = DefaultScoreMultiplier.ErrorRate
	}
	if s.P90Latency == 0 {
		s.P90Latency = DefaultScoreMultiplier.P90Latency
	}
	if s.TotalRequests == 0 {
		s.TotalRequests = DefaultScoreMultiplier.TotalRequests
	}
	if s.ThrottledRate == 0 {
		s.ThrottledRate = DefaultScoreMultiplier.ThrottledRate
	}
	if s.BlockHeadLag == 0 {
		s.BlockHeadLag = DefaultScoreMultiplier.BlockHeadLag
	}
	if s.FinalizationLag == 0 {
		s.FinalizationLag = DefaultScoreMultiplier.FinalizationLag
	}
	if s.Overall == 0 {
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
		c.EvalInterval = 1 * time.Minute
	}
	if c.EvalFunction == nil {
		evalFunction, err := script.CompileFunction(DefaultPolicyFunction)
		if err != nil {
			log.Error().Err(err).Msg("failed to compile default selection policy function")
		} else {
			c.EvalFunction = evalFunction
		}
	}
	if c.ResampleExcluded {
		if c.ResampleInterval == 0 {
			c.ResampleInterval = 5 * time.Minute
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

func (s *SecretStrategyConfig) SetDefaults() error {
	return nil
}

func (j *JwtStrategyConfig) SetDefaults() error {
	return nil
}

func (s *SiweStrategyConfig) SetDefaults() error {
	return nil
}

func (n *NetworkStrategyConfig) SetDefaults() error {
	return nil
}

func (r *RateLimiterConfig) SetDefaults() error {
	if len(r.Budgets) > 0 {
		for _, budget := range r.Budgets {
			if err := budget.SetDefaults(); err != nil {
				return fmt.Errorf("failed to set defaults for rate limit budget: %w", err)
			}
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
	if r.WaitTime == "" {
		r.WaitTime = "1s"
	}
	if r.Period == "" {
		r.Period = "1s"
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

func (h *HealthCheckConfig) SetDefaults() error {
	if h.ScoreMetricsWindowSize == "" {
		h.ScoreMetricsWindowSize = "30m"
	}

	return nil
}

func NewDefaultNetworkConfig(upstreams []*UpstreamConfig) *NetworkConfig {
	hasAnyFallbackUpstream := slices.ContainsFunc(upstreams, func(u *UpstreamConfig) bool {
		return u.Group == "fallback"
	})
	n := &NetworkConfig{}
	if hasAnyFallbackUpstream {
		evalFunction, err := script.CompileFunction(DefaultPolicyFunction)
		if err != nil {
			log.Error().Err(err).Msg("failed to compile default selection policy function")
			return nil
		}

		selectionPolicy := &SelectionPolicyConfig{
			EvalInterval:     1 * time.Minute,
			EvalFunction:     evalFunction,
			EvalPerMethod:    false,
			ResampleInterval: 5 * time.Minute,
			ResampleCount:    10,

			evalFunctionOriginal: DefaultPolicyFunction,
		}

		n.SelectionPolicy = selectionPolicy
	}
	return n
}
