package common

import (
	"slices"
	"time"

	"github.com/erpc/erpc/common/script"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
)

func (c *Config) SetDefaults() {
	if c.LogLevel == "" {
		c.LogLevel = "INFO"
	}
	if c.Server == nil {
		c.Server = &ServerConfig{}
	}
	c.Server.SetDefaults()

	if c.Database == nil {
		c.Database = &DatabaseConfig{
			EvmJsonRpcCache: &CacheConfig{
				Connectors: []*ConnectorConfig{
					{
						Id:     "memory-cache",
						Driver: DriverMemory,
					},
				},
				Policies: []*CachePolicyConfig{
					{
						Network:   "*",
						Method:    "*",
						Finality:  DataFinalityStateFinalized,
						TTL:       0, // Forever
						Connector: "memory-cache",
					},
					{
						Network:   "*",
						Method:    "*",
						Finality:  DataFinalityStateUnknown,
						TTL:       30 * time.Second,
						Connector: "memory-cache",
					},
					{
						Network:   "*",
						Method:    "*",
						Finality:  DataFinalityStateUnfinalized,
						TTL:       30 * time.Second,
						Connector: "memory-cache",
					},
				},
			},
		}
	}
	c.Database.SetDefaults()

	if c.Metrics == nil {
		c.Metrics = &MetricsConfig{}
	}
	c.Metrics.SetDefaults()

	if c.Admin != nil {
		c.Admin.SetDefaults()
	}

	if c.Projects != nil {
		for _, project := range c.Projects {
			project.SetDefaults()
		}
	}

	if c.RateLimiters != nil {
		c.RateLimiters.SetDefaults()
	}
}

func (c *CacheConfig) SetDefaults() {
	if len(c.Policies) > 0 {
		for _, policy := range c.Policies {
			policy.SetDefaults()
		}
	}
	if len(c.Connectors) > 0 {
		for _, connector := range c.Connectors {
			connector.SetDefaults()
		}
	}
}

func (c *CachePolicyConfig) SetDefaults() {
	if c.Method == "" {
		c.Method = "*"
	}
	if c.Network == "" {
		c.Network = "*"
	}
}

func (s *ServerConfig) SetDefaults() {
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
		s.MaxTimeout = util.StringPtr("30s")
	}
	if s.EnableGzip == nil {
		s.EnableGzip = util.BoolPtr(true)
	}
}

func (m *MetricsConfig) SetDefaults() {
	if m.HostV4 == nil {
		m.HostV4 = util.StringPtr("0.0.0.0")
	}
	if m.HostV6 == nil {
		m.HostV6 = util.StringPtr("[::]")
	}
	if m.Port == nil {
		m.Port = util.IntPtr(4001)
	}
}

func (a *AdminConfig) SetDefaults() {
	if a.Auth != nil {
		a.Auth.SetDefaults()
	}
	if a.CORS == nil {
		// It is safe to enable CORS of * for admin endpoint since requests are protected by Secret Tokens
		a.CORS = &CORSConfig{
			AllowedOrigins:   []string{"*"},
			AllowCredentials: util.BoolPtr(false),
		}
	}
	a.CORS.SetDefaults()
}

func (d *DatabaseConfig) SetDefaults() {
	if d.EvmJsonRpcCache != nil {
		d.EvmJsonRpcCache.SetDefaults()
	}
}

func (c *ConnectorConfig) SetDefaults() {
	if c.Driver == "" {
		return
	}

	if c.Memory != nil {
		c.Driver = DriverMemory
	}
	if c.Driver == DriverMemory {
		if c.Memory == nil {
			c.Memory = &MemoryConnectorConfig{}
		}
		c.Memory.SetDefaults()
	}
	if c.Redis != nil {
		c.Driver = DriverRedis
	}
	if c.Driver == DriverRedis {
		if c.Redis == nil {
			c.Redis = &RedisConnectorConfig{}
		}
		c.Redis.SetDefaults()
	}
	if c.PostgreSQL != nil {
		c.Driver = DriverPostgreSQL
	}
	if c.Driver == DriverPostgreSQL {
		if c.PostgreSQL == nil {
			c.PostgreSQL = &PostgreSQLConnectorConfig{}
		}
		c.PostgreSQL.SetDefaults()
	}
	if c.DynamoDB != nil {
		c.Driver = DriverDynamoDB
	}
	if c.Driver == DriverDynamoDB {
		if c.DynamoDB == nil {
			c.DynamoDB = &DynamoDBConnectorConfig{}
		}
		c.DynamoDB.SetDefaults()
	}
}

func (m *MemoryConnectorConfig) SetDefaults() {
	if m.MaxItems == 0 {
		m.MaxItems = 100000
	}
}

func (r *RedisConnectorConfig) SetDefaults() {
	if r.Addr == "" {
		r.Addr = "localhost:6379"
	}
	if r.ConnPoolSize == 0 {
		r.ConnPoolSize = 128
	}
}

func (p *PostgreSQLConnectorConfig) SetDefaults() {
	if p.Table == "" {
		p.Table = "erpc_json_rpc_cache"
	}
}

func (d *DynamoDBConnectorConfig) SetDefaults() {
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
}

func (p *ProjectConfig) SetDefaults() {
	if p.Upstreams != nil {
		for _, upstream := range p.Upstreams {
			upstream.SetDefaults()
		}
	}
	if p.Networks != nil {
		for _, network := range p.Networks {
			network.SetDefaults(p.Upstreams)
		}
	}
	if p.Auth != nil {
		p.Auth.SetDefaults()
	}
	if p.CORS != nil {
		p.CORS.SetDefaults()
	}
	if p.HealthCheck == nil {
		p.HealthCheck = &HealthCheckConfig{}
	}
	p.HealthCheck.SetDefaults()
}

func (u *UpstreamConfig) SetDefaults() {
	if u.Id == "" {
		u.Id = util.RedactEndpoint(u.Endpoint)
	}

	if u.Failsafe == nil {
		u.Failsafe = &FailsafeConfig{
			Timeout: &TimeoutPolicyConfig{
				Duration: "15s",
			},
			Retry: &RetryPolicyConfig{
				MaxAttempts:     3,
				Delay:           "300ms",
				Jitter:          "100ms",
				BackoffMaxDelay: "5s",
				BackoffFactor:   1.5,
			},
			CircuitBreaker: &CircuitBreakerPolicyConfig{
				FailureThresholdCount:    160, // 80% error rate
				FailureThresholdCapacity: 200,
				HalfOpenAfter:            "5m",
				SuccessThresholdCount:    3,
				SuccessThresholdCapacity: 3,
			},
		}
	}
	u.Failsafe.SetDefaults()

	if u.RateLimitAutoTune == nil && u.RateLimitBudget != "" {
		u.RateLimitAutoTune = &RateLimitAutoTuneConfig{}
	}
	if u.RateLimitAutoTune != nil {
		u.RateLimitAutoTune.SetDefaults()
	}

	if u.Evm != nil {
		u.Evm.SetDefaults()
	}
	if u.JsonRpc != nil {
		u.JsonRpc.SetDefaults()
	}

	if u.Routing != nil {
		u.Routing.SetDefaults()
	}
}

func (e *EvmUpstreamConfig) SetDefaults() {
	if e.StatePollerInterval == "" {
		e.StatePollerInterval = "30s"
	}

	if e.NodeType == "" {
		e.NodeType = "archive"
	}

	if e.MaxAvailableRecentBlocks == 0 {
		switch e.NodeType {
		case "full":
			e.MaxAvailableRecentBlocks = 128
		}
	}
}

func (j *JsonRpcUpstreamConfig) SetDefaults() {}

func (n *NetworkConfig) SetDefaults(upstreams []*UpstreamConfig) {
	if n.Architecture == "" {
		if n.Evm != nil {
			n.Architecture = "evm"
		}
	}

	if n.Failsafe == nil {
		nwCfg := NewDefaultNetworkConfig(upstreams)
		n.Failsafe = nwCfg.Failsafe
	}
	n.Failsafe.SetDefaults()

	if n.Architecture == "evm" && n.Evm == nil {
		n.Evm = &EvmNetworkConfig{}
	}
	if n.Evm != nil {
		n.Evm.SetDefaults()
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
		n.SelectionPolicy.SetDefaults()
	}
}

const DefaultEvmFinalityDepth = 1024

func (e *EvmNetworkConfig) SetDefaults() {
	if e.FallbackFinalityDepth == 0 {
		e.FallbackFinalityDepth = DefaultEvmFinalityDepth
	}
}

func (f *FailsafeConfig) SetDefaults() {
	if f.Timeout != nil {
		f.Timeout.SetDefaults()
	}
	if f.Retry != nil {
		f.Retry.SetDefaults()
	}
	if f.CircuitBreaker != nil {
		f.CircuitBreaker.SetDefaults()
	}
}

func (t *TimeoutPolicyConfig) SetDefaults() {}

func (r *RetryPolicyConfig) SetDefaults() {
	if r.BackoffFactor == 0 {
		r.BackoffFactor = 1.5
	}
	if r.BackoffMaxDelay == "" {
		r.BackoffMaxDelay = "5s"
	}
	if r.Delay == "" {
		r.Delay = "300ms"
	}
	if r.Jitter == "" {
		r.Jitter = "100ms"
	}
}

func (h *HedgePolicyConfig) SetDefaults() {
	if h.Delay == "" {
		h.Delay = "100ms"
	}
}

func (c *CircuitBreakerPolicyConfig) SetDefaults() {
	if c.HalfOpenAfter == "" {
		c.HalfOpenAfter = "5m"
	}
}

func (r *RateLimitAutoTuneConfig) SetDefaults() {
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
		r.DecreaseFactor = 0.9
	}
	if r.MaxBudget == 0 {
		r.MaxBudget = 10000
	}
}

func (r *RoutingConfig) SetDefaults() {
	if r.ScoreMultipliers != nil {
		for _, multiplier := range r.ScoreMultipliers {
			multiplier.SetDefaults()
		}
	}
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

func (s *ScoreMultiplierConfig) SetDefaults() {
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

		return [...fallbacks, ...healthyOnes]
	}
`

func (c *SelectionPolicyConfig) SetDefaults() {
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
}

func (a *AuthConfig) SetDefaults() {
	if a.Strategies != nil {
		for _, strategy := range a.Strategies {
			strategy.SetDefaults()
		}
	}
}

func (s *AuthStrategyConfig) SetDefaults() {
	if s.Type == AuthTypeNetwork && s.Network == nil {
		s.Network = &NetworkStrategyConfig{}
	}
	if s.Network != nil {
		s.Network.SetDefaults()
	}

	if s.Type == AuthTypeSecret && s.Secret == nil {
		s.Secret = &SecretStrategyConfig{}
	}
	if s.Secret != nil {
		s.Type = AuthTypeSecret
		s.Secret.SetDefaults()
	}

	if s.Type == AuthTypeJwt && s.Jwt == nil {
		s.Jwt = &JwtStrategyConfig{}
	}
	if s.Jwt != nil {
		s.Type = AuthTypeJwt
		s.Jwt.SetDefaults()
	}

	if s.Type == AuthTypeSiwe && s.Siwe == nil {
		s.Siwe = &SiweStrategyConfig{}
	}
	if s.Siwe != nil {
		s.Type = AuthTypeSiwe
		s.Siwe.SetDefaults()
	}
}

func (s *SecretStrategyConfig) SetDefaults() {}

func (j *JwtStrategyConfig) SetDefaults() {}

func (s *SiweStrategyConfig) SetDefaults() {}

func (n *NetworkStrategyConfig) SetDefaults() {}

func (r *RateLimiterConfig) SetDefaults() {
	if len(r.Budgets) > 0 {
		for _, budget := range r.Budgets {
			budget.SetDefaults()
		}
	}
}

func (b *RateLimitBudgetConfig) SetDefaults() {
	if len(b.Rules) > 0 {
		for _, rule := range b.Rules {
			rule.SetDefaults()
		}
	}
}

func (r *RateLimitRuleConfig) SetDefaults() {
	if r.WaitTime == "" {
		r.WaitTime = "1s"
	}
	if r.Period == "" {
		r.Period = "1s"
	}
	if r.Method == "" {
		r.Method = "*"
	}
}

func (c *CORSConfig) SetDefaults() {
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
}

func (h *HealthCheckConfig) SetDefaults() {
	if h.ScoreMetricsWindowSize == "" {
		h.ScoreMetricsWindowSize = "30m"
	}
}

func NewDefaultNetworkConfig(upstreams []*UpstreamConfig) *NetworkConfig {
	hasAnyFallbackUpstream := slices.ContainsFunc(upstreams, func(u *UpstreamConfig) bool {
		return u.Group == "fallback"
	})
	n := &NetworkConfig{
		Failsafe: &FailsafeConfig{
			Hedge: &HedgePolicyConfig{
				Delay:    "200ms",
				MaxCount: 3,
			},
			Retry: &RetryPolicyConfig{
				MaxAttempts:     3,
				Delay:           "100ms",
				Jitter:          "0ms",
				BackoffMaxDelay: "1s",
				BackoffFactor:   1.5,
			},
			Timeout: &TimeoutPolicyConfig{
				Duration: "30s",
			},
		},
	}
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
