package common

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/erpc/erpc/util"
)

func (c *Config) Validate() error {
	if c.Server != nil {
		if err := c.Server.Validate(); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("server config is required")
	}
	if c.HealthCheck != nil {
		if err := c.HealthCheck.Validate(); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("healthCheck config is required")
	}
	if c.Metrics != nil {
		if err := c.Metrics.Validate(); err != nil {
			return err
		}
	}
	if c.Admin != nil {
		if err := c.Admin.Validate(); err != nil {
			return err
		}
	}
	if c.Database != nil {
		if err := c.Database.Validate(); err != nil {
			return err
		}
	}
	if c.Projects != nil {
		for _, project := range c.Projects {
			if err := project.Validate(c); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("projects config is required")
	}
	if c.RateLimiters != nil {
		if err := c.RateLimiters.Validate(); err != nil {
			return err
		}
	}
	if c.ProxyPools != nil {
		for _, pool := range c.ProxyPools {
			if err := pool.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *ServerConfig) Validate() error {
	if s.ListenV4 != nil {
		if *s.ListenV4 {
			if s.HttpHostV4 == nil {
				return fmt.Errorf("server.listenV4 is true but server.httpHostV4 is not set")
			}
			if s.HttpPort == nil {
				return fmt.Errorf("server.listenV4 is true but server.httpPort is not set")
			}
		}
	}
	if s.ListenV6 != nil {
		if *s.ListenV6 {
			if s.HttpHostV6 == nil {
				return fmt.Errorf("server.listenV6 is true but server.httpHostV6 is not set")
			}
			if s.HttpPort == nil {
				return fmt.Errorf("server.listenV6 is true but server.httpPort is not set")
			}
		}
	}
	if s.MaxTimeout == nil || *s.MaxTimeout == 0 {
		return fmt.Errorf("server.maxTimeout is required")
	}
	return nil
}

func (h *HealthCheckConfig) Validate() error {
	if h.Auth != nil {
		if err := h.Auth.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (a *AdminConfig) Validate() error {
	if a.Auth != nil {
		if err := a.Auth.Validate(); err != nil {
			return err
		}
	}
	if a.CORS != nil {
		if err := a.CORS.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (m *MetricsConfig) Validate() error {
	if m.Enabled != nil && *m.Enabled {
		if m.HostV4 == nil && m.HostV6 == nil {
			return fmt.Errorf("metrics.hostV4 or metrics.hostV6 is required when metrics.enabled is true")
		}
		if m.Port == nil {
			return fmt.Errorf("metrics.port is required when metrics.enabled is true")
		}
	}

	if m.ErrorLabelMode != "" && m.ErrorLabelMode != ErrorLabelModeVerbose && m.ErrorLabelMode != ErrorLabelModeCompact {
		return fmt.Errorf("metrics.errorLabelMode must be either 'verbose' or 'compact'")
	}

	if m.HistogramBuckets != "" {
		parts := strings.Split(m.HistogramBuckets, ",")
		for _, part := range parts {
			if _, err := strconv.ParseFloat(strings.TrimSpace(part), 64); err != nil {
				return fmt.Errorf("metrics.histogramBuckets contains invalid float value: %s", part)
			}
		}
	}

	return nil
}

func (r *RateLimiterConfig) Validate() error {
	if len(r.Budgets) > 0 {
		for _, budget := range r.Budgets {
			if err := budget.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *RateLimitBudgetConfig) Validate() error {
	if len(b.Rules) == 0 {
		return fmt.Errorf("rateLimiter.*.budget.rules is required, add at least one rule")
	}
	for _, rule := range b.Rules {
		if err := rule.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (r *RateLimitRuleConfig) Validate() error {
	if r.Method == "" {
		return fmt.Errorf("rateLimiter.*.budget.rules.*.method is required")
	}
	if r.WaitTime == 0 {
		return fmt.Errorf("rateLimiter.*.budget.rules.*.waitTime is required")
	}
	return nil
}

func (p *ProxyPoolConfig) Validate() error {
	if p.ID == "" {
		return fmt.Errorf("proxyPool.*.id is required under proxyPools")
	}
	if len(p.Urls) == 0 {
		return fmt.Errorf("proxyPool.*.urls is required under proxyPool.*.id '%s', add at least one URL", p.ID)
	}

	for _, url := range p.Urls {
		urlLower := strings.ToLower(url)
		if !strings.HasPrefix(urlLower, "http://") &&
			!strings.HasPrefix(urlLower, "https://") &&
			!strings.HasPrefix(urlLower, "socks5://") {
			return fmt.Errorf("proxyPool.*.urls under proxyPool.*.id '%s' must be valid HTTP, HTTPS, or SOCKS5 URLs, got: %s", p.ID, url)
		}
	}
	return nil
}

func (d *DatabaseConfig) Validate() error {
	if d.EvmJsonRpcCache != nil {
		if err := d.EvmJsonRpcCache.Validate(); err != nil {
			return err
		}
	}
	if d.SharedState != nil {
		if err := d.SharedState.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (s *SharedStateConfig) Validate() error {
	if s.Connector != nil {
		if err := s.Connector.Validate(); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("sharedState.connector is required")
	}
	if s.FallbackTimeout == 0 {
		return fmt.Errorf("sharedState.fallbackTimeout is required")
	}
	return nil
}

func (c *CacheConfig) Validate() error {
	if len(c.Connectors) == 0 {
		return fmt.Errorf("cache.*.connectors is required, add at least one connector")
	}
	existingIds := make(map[string]bool)
	for _, connector := range c.Connectors {
		if err := connector.Validate(); err != nil {
			return err
		}
		if existingIds[connector.Id] {
			return fmt.Errorf("cache.*.connectors.*.id must be unique, '%s' is duplicated", connector.Id)
		}
		existingIds[connector.Id] = true
	}
	if len(c.Policies) == 0 {
		return fmt.Errorf("cache.*.policies is required, add at least one policy")
	}
	for _, policy := range c.Policies {
		if err := policy.Validate(c); err != nil {
			return err
		}
	}
	if c.Compression != nil {
		if err := c.Compression.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c *CompressionConfig) Validate() error {
	if c.Algorithm != "" && c.Algorithm != "zstd" {
		return fmt.Errorf("cache.*.compression.algorithm must be 'zstd' (currently the only supported algorithm)")
	}

	if c.ZstdLevel != "" {
		validLevels := []string{"fastest", "default", "better", "best"}
		found := false
		for _, level := range validLevels {
			if c.ZstdLevel == level {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("cache.*.compression.zstdLevel must be one of: fastest, default, better, best")
		}
	}

	if c.Threshold < 0 {
		return fmt.Errorf("cache.*.compression.threshold must be greater than or equal to 0")
	}

	return nil
}

func (p *CachePolicyConfig) Validate(c *CacheConfig) error {
	if p.Network == "" {
		return fmt.Errorf("cache.*.policies.*.network is required")
	}
	if p.Method == "" {
		return fmt.Errorf("cache.*.policies.*.method is required")
	}
	if p.Connector == "" {
		return fmt.Errorf("cache.*.policies.*.connector is required")
	}

	found := false
	for _, connector := range c.Connectors {
		if connector.Id == p.Connector {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("cache.*.policies.*.connector '%s' does not exist in cache.connectors", p.Connector)
	}

	if p.MinItemSize != nil {
		if _, err := util.ParseByteSize(*p.MinItemSize); err != nil {
			return fmt.Errorf("cache.*.policies.*.minItemSize is invalid: %w", err)
		}
	}

	if p.MaxItemSize != nil {
		if _, err := util.ParseByteSize(*p.MaxItemSize); err != nil {
			return fmt.Errorf("cache.*.policies.*.maxItemSize is invalid: %w", err)
		}
	}

	if p.MinItemSize != nil && p.MaxItemSize != nil {
		minSize, _ := util.ParseByteSize(*p.MinItemSize)
		maxSize, _ := util.ParseByteSize(*p.MaxItemSize)
		if minSize > maxSize {
			return fmt.Errorf("cache.*.policies.*.minItemSize must be less than or equal to maxItemSize")
		}
	}

	return nil
}

func (c *ConnectorConfig) Validate() error {
	if c.Driver == "" {
		return fmt.Errorf("database.*.connector.driver is required")
	}
	drivers := []ConnectorDriverType{DriverMemory, DriverRedis, DriverPostgreSQL, DriverDynamoDB}
	if !slices.Contains(drivers, c.Driver) {
		return fmt.Errorf("database.*.connector.driver '%s' is invalid must be one of: %v", c.Driver, drivers)
	}
	if c.Driver == DriverMemory && c.Memory == nil {
		return fmt.Errorf("database.*.connector.memory is required when driver is memory")
	}
	if c.Driver == DriverRedis && c.Redis == nil {
		return fmt.Errorf("database.*.connector.redis is required when driver is redis")
	}
	if c.Driver == DriverPostgreSQL && c.PostgreSQL == nil {
		return fmt.Errorf("database.*.connector.postgresql is required when driver is postgresql")
	}
	if c.Driver == DriverDynamoDB && c.DynamoDB == nil {
		return fmt.Errorf("database.*.connector.dynamodb is required when driver is dynamodb")
	}

	// TODO switch to go-validator library :D
	if c.Memory != nil && (c.Redis != nil || c.PostgreSQL != nil || c.DynamoDB != nil) {
		return fmt.Errorf("database.*.connector.memory is mutually exclusive with database.*.connector.redis, database.*.connector.postgresql, and database.*.connector.dynamodb")
	}
	if c.Redis != nil && (c.Memory != nil || c.PostgreSQL != nil || c.DynamoDB != nil) {
		return fmt.Errorf("database.*.connector.redis is mutually exclusive with database.*.connector.memory, database.*.connector.postgresql, and database.*.connector.dynamodb")
	}
	if c.PostgreSQL != nil && (c.Memory != nil || c.Redis != nil || c.DynamoDB != nil) {
		return fmt.Errorf("database.*.connector.postgresql is mutually exclusive with database.*.connector.memory, database.*.connector.redis, and database.*.connector.dynamodb")
	}
	if c.DynamoDB != nil && (c.Memory != nil || c.Redis != nil || c.PostgreSQL != nil) {
		return fmt.Errorf("database.*.connector.dynamodb is mutually exclusive with database.*.connector.memory, database.*.connector.redis, and database.*.connector.postgresql")
	}

	if c.DynamoDB != nil {
		if err := c.DynamoDB.Validate(); err != nil {
			return err
		}
	}
	if c.PostgreSQL != nil {
		if err := c.PostgreSQL.Validate(); err != nil {
			return err
		}
	}
	if c.Redis != nil {
		if err := c.Redis.Validate(); err != nil {
			return err
		}
	}
	if c.Memory != nil {
		if err := c.Memory.Validate(); err != nil {
			return err
		}
	}

	return nil
}

func (p *DynamoDBConnectorConfig) Validate() error {
	if p.Table == "" {
		return fmt.Errorf("database.*.connector.dynamodb.table is required")
	}
	if p.PartitionKeyName == "" {
		return fmt.Errorf("database.*.connector.dynamodb.partitionKeyName is required")
	}
	if p.RangeKeyName == "" {
		return fmt.Errorf("database.*.connector.dynamodb.rangeKeyName is required")
	}
	if p.ReverseIndexName == "" {
		return fmt.Errorf("database.*.connector.dynamodb.reverseIndexName is required")
	}
	if p.TTLAttributeName == "" {
		return fmt.Errorf("database.*.connector.dynamodb.ttlAttributeName is required")
	}
	if p.InitTimeout == 0 {
		return fmt.Errorf("database.*.connector.dynamodb.initTimeout is required")
	}
	if p.GetTimeout == 0 {
		return fmt.Errorf("database.*.connector.dynamodb.getTimeout is required")
	}
	if p.SetTimeout == 0 {
		return fmt.Errorf("database.*.connector.dynamodb.setTimeout is required")
	}
	if p.StatePollInterval == 0 {
		return fmt.Errorf("database.*.connector.dynamodb.statePollInterval is required")
	}
	return nil
}

func (p *PostgreSQLConnectorConfig) Validate() error {
	if p.ConnectionUri == "" {
		return fmt.Errorf("database.*.connector.postgresql.connectionUri is required")
	}
	if p.Table == "" {
		return fmt.Errorf("database.*.connector.postgresql.table is required")
	}
	if p.MinConns == 0 {
		return fmt.Errorf("database.*.connector.postgresql.minConns is required")
	}
	if p.MaxConns == 0 {
		return fmt.Errorf("database.*.connector.postgresql.maxConns is required")
	}
	if p.InitTimeout == 0 {
		return fmt.Errorf("database.*.connector.postgresql.initTimeout is required")
	}
	if p.GetTimeout == 0 {
		return fmt.Errorf("database.*.connector.postgresql.getTimeout is required")
	}
	if p.SetTimeout == 0 {
		return fmt.Errorf("database.*.connector.postgresql.setTimeout is required")
	}
	return nil
}

func (c *RedisConnectorConfig) Validate() error {
	uri := strings.TrimSpace(c.URI)
	if uri == "" {
		return fmt.Errorf("database.*.connector.redis.uri is required")
	}

	// Enforce supported schemes.
	if !strings.HasPrefix(uri, "rediss://") && !strings.HasPrefix(uri, "redis://") {
		return fmt.Errorf("redis connector: invalid URI scheme, must be 'rediss://' or 'redis://'")
	}

	return nil
}

func (p *MemoryConnectorConfig) Validate() error {
	return nil
}

func (p *ProjectConfig) Validate(c *Config) error {
	if p.Id == "" {
		return fmt.Errorf("project id is required")
	}
	if len(p.Providers) > 0 {
		existingIds := make(map[string]bool)
		for _, provider := range p.Providers {
			if err := provider.Validate(c); err != nil {
				return err
			}
			if existingIds[provider.Id] {
				return fmt.Errorf("project.*.providers.*.id must be unique, '%s' is duplicated", provider.Id)
			}
			existingIds[provider.Id] = true
		}
	}
	if len(p.Upstreams) > 0 {
		existingIds := make(map[string]bool)
		for _, upstream := range p.Upstreams {
			if err := upstream.Validate(c, false); err != nil {
				return err
			}
			if existingIds[upstream.Id] {
				return fmt.Errorf("project.*.upstreams.*.id must be unique, '%s' is duplicated", upstream.Id)
			}
			existingIds[upstream.Id] = true
		}
	} else if len(p.Providers) == 0 {
		return fmt.Errorf("project.*.upstreams or project.*.providers is required, add at least one of them")
	}
	if p.Networks != nil {
		existingIds := make(map[string]bool)
		for _, network := range p.Networks {
			if err := network.Validate(c); err != nil {
				return err
			}
			ntwId := network.NetworkId()
			if existingIds[ntwId] {
				return fmt.Errorf("project.*.networks.*.id must be unique, '%s' is duplicated", ntwId)
			}
			existingIds[ntwId] = true
		}
	}
	if p.Auth != nil {
		if err := p.Auth.Validate(); err != nil {
			return err
		}
	}
	if p.CORS != nil {
		if err := p.CORS.Validate(); err != nil {
			return err
		}
	}
	if p.RateLimitBudget != "" {
		if !c.HasRateLimiterBudget(p.RateLimitBudget) {
			return fmt.Errorf("project.*.rateLimitBudget '%s' does not exist in config.rateLimiters", p.RateLimitBudget)
		}
	}
	if p.ScoreMetricsWindowSize == 0 {
		return fmt.Errorf("project.*.scoreMetricsWindowSize is required")
	}
	return nil
}

func (a *AuthConfig) Validate() error {
	if len(a.Strategies) == 0 {
		return fmt.Errorf("project.*.auth.strategies is required, add at least one strategy")
	}
	for _, strategy := range a.Strategies {
		if err := strategy.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (s *AuthStrategyConfig) Validate() error {
	if s.Type == "" {
		return fmt.Errorf("auth.*.type is required")
	}
	switch s.Type {
	case AuthTypeNetwork:
		if s.Network == nil {
			return fmt.Errorf("auth.*.network is required for network strategy")
		}
		if err := s.Network.Validate(); err != nil {
			return err
		}
	case AuthTypeSecret:
		if s.Secret == nil {
			return fmt.Errorf("auth.*.secret is required for secret strategy")
		}
		if err := s.Secret.Validate(); err != nil {
			return err
		}
	case AuthTypeJwt:
		if s.Jwt == nil {
			return fmt.Errorf("auth.*.jwt is required for jwt strategy")
		}
		if err := s.Jwt.Validate(); err != nil {
			return err
		}
	case AuthTypeSiwe:
		if s.Siwe == nil {
			return fmt.Errorf("auth.*.siwe is required for siwe strategy")
		}
		if err := s.Siwe.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("auth.*.type '%s' is invalid must be one of: %v", s.Type, []AuthType{
			AuthTypeNetwork,
			AuthTypeSecret,
			AuthTypeJwt,
			AuthTypeSiwe,
		})
	}
	return nil
}

func (s *NetworkStrategyConfig) Validate() error {
	return nil
}

func (s *SecretStrategyConfig) Validate() error {
	if s.Value == "" {
		return fmt.Errorf("auth.*.secret.value is required")
	}
	return nil
}

func (j *JwtStrategyConfig) Validate() error {
	if len(j.VerificationKeys) == 0 {
		return fmt.Errorf("auth.*.jwt.verificationKeys is required, add at least one verification key")
	}
	return nil
}

func (s *SiweStrategyConfig) Validate() error {
	return nil
}

func (c *CORSConfig) Validate() error {
	if len(c.AllowedOrigins) == 0 {
		return fmt.Errorf("*.cors.allowedOrigins is required, add at least one allowed origin")
	}
	return nil
}

func (h *DeprecatedProjectHealthCheckConfig) Validate() error {
	if h.ScoreMetricsWindowSize == 0 {
		return fmt.Errorf("project.*.healthCheck.scoreMetricsWindowSize is required")
	}
	return nil
}

func (u *ProviderConfig) Validate(c *Config) error {
	if u.Id == "" {
		return fmt.Errorf("project.*.providers.*.id is required")
	}
	if u.Vendor == "" {
		return fmt.Errorf("project.*.providers.*.vendor is required")
	}
	if u.UpstreamIdTemplate == "" {
		return fmt.Errorf("project.*.providers.*.upstreamIdTemplate is required")
	}
	if u.OnlyNetworks != nil && u.IgnoreNetworks != nil {
		return fmt.Errorf("project.*.providers.*.onlyNetworks and project.*.providers.*.ignoreNetworks are mutually exclusive")
	}
	if u.Overrides != nil {
		for _, override := range u.Overrides {
			if err := override.Validate(c, true); err != nil {
				return err
			}
		}
	}
	if u.OnlyNetworks != nil {
		for _, network := range u.OnlyNetworks {
			if !IsValidNetwork(network) {
				return fmt.Errorf("project.*.providers.*.onlyNetworks.* '%s' is invalid must be like evm:1", network)
			}
		}
	}
	if u.IgnoreNetworks != nil {
		for _, network := range u.IgnoreNetworks {
			if !IsValidNetwork(network) {
				return fmt.Errorf("project.*.providers.*.ignoreNetworks.* '%s' is invalid must be like evm:1", network)
			}
		}
	}
	return nil
}

func (u *UpstreamConfig) Validate(c *Config, skipEndpointCheck bool) error {
	if !skipEndpointCheck && u.Endpoint == "" {
		return fmt.Errorf("upstream.*.endpoint is required")
	}
	if u.Evm != nil {
		if err := u.Evm.Validate(u); err != nil {
			return err
		}
	}
	if u.Failsafe != nil {
		for _, fs := range u.Failsafe {
			if err := fs.Validate(); err != nil {
				return err
			}
		}
	}
	if u.JsonRpc != nil {
		if err := u.JsonRpc.Validate(c); err != nil {
			return err
		}
	}
	if u.RateLimitAutoTune != nil {
		if err := u.RateLimitAutoTune.Validate(); err != nil {
			return err
		}
	}
	if u.Routing != nil {
		if err := u.Routing.Validate(); err != nil {
			return err
		}
	}
	if u.RateLimitBudget != "" {
		if !c.HasRateLimiterBudget(u.RateLimitBudget) {
			return fmt.Errorf("upstream.*.rateLimitBudget '%s' does not exist in config.rateLimiters", u.RateLimitBudget)
		}
	}
	return nil
}

func (e *EvmUpstreamConfig) Validate(u *UpstreamConfig) error {
	if !util.IsNativeProtocol(u.Endpoint) {
		if e.ChainId > 0 {
			return fmt.Errorf("upstream.*.evm.chainId must be 0 for non-http endpoints, but '%d' is provided for %s", e.ChainId, util.RedactEndpoint(u.Endpoint))
		}
	}

	if e.StatePollerInterval == 0 {
		return fmt.Errorf("upstream.*.evm.statePollerInterval is required")
	}
	if e.NodeType != "" {
		allowed := []EvmNodeType{
			EvmNodeTypeUnknown,
			EvmNodeTypeArchive,
			EvmNodeTypeFull,
		}
		if !slices.Contains(allowed, e.NodeType) {
			return fmt.Errorf("upstream.*.evm.nodeType '%s' is invalid must be one of: %v", e.NodeType, allowed)
		}
	}

	return nil
}

func (f *FailsafeConfig) Validate() error {
	// Validate MatchMethod - empty string is not allowed
	if f.MatchMethod == "" {
		return fmt.Errorf("failsafe.matchMethod cannot be empty, use '*' to match any method")
	}

	if f.Timeout != nil {
		if err := f.Timeout.Validate(); err != nil {
			return err
		}
	}
	if f.Retry != nil {
		if err := f.Retry.Validate(); err != nil {
			return err
		}
	}
	if f.Hedge != nil {
		if err := f.Hedge.Validate(); err != nil {
			return err
		}
	}
	if f.CircuitBreaker != nil {
		if err := f.CircuitBreaker.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (t *TimeoutPolicyConfig) Validate() error {
	if t.Duration == 0 {
		return fmt.Errorf("upstream.*.failsafe.timeout.duration is required")
	}
	return nil
}

func (r *RetryPolicyConfig) Validate() error {
	if r.BackoffFactor <= 0 {
		return fmt.Errorf("upstream.*.failsafe.retry.backoffFactor must be greater than 0")
	}
	if r.BackoffMaxDelay == 0 {
		return fmt.Errorf("upstream.*.failsafe.retry.backoffMaxDelay is required")
	}
	return nil
}

func (h *HedgePolicyConfig) Validate() error {
	if h.Quantile <= 0 && h.Delay <= 0 {
		return fmt.Errorf("failsafe.hedge.delay or failsafe.hedge.quantile is required")
	}
	return nil
}

func (c *CircuitBreakerPolicyConfig) Validate() error {
	if c.HalfOpenAfter == 0 {
		return fmt.Errorf("failsafe.circuitBreaker.halfOpenAfter is required")
	}
	if c.FailureThresholdCapacity <= 0 {
		return fmt.Errorf("failsafe.circuitBreaker.failureThresholdCapacity must be greater than 0")
	}
	if c.FailureThresholdCount <= 0 {
		return fmt.Errorf("failsafe.circuitBreaker.failureThresholdCount must be greater than 0")
	}
	if c.FailureThresholdCount > c.FailureThresholdCapacity {
		return fmt.Errorf("failsafe.circuitBreaker.failureThresholdCount must be less than or equal to failureThresholdCapacity")
	}
	if c.SuccessThresholdCount <= 0 {
		return fmt.Errorf("failsafe.circuitBreaker.successThresholdCount must be greater than 0")
	}
	if c.SuccessThresholdCapacity <= 0 {
		return fmt.Errorf("failsafe.circuitBreaker.successThresholdCapacity must be greater than 0")
	}
	if c.SuccessThresholdCount > c.SuccessThresholdCapacity {
		return fmt.Errorf("failsafe.circuitBreaker.successThresholdCount must be less than or equal to failureThresholdCapacity")
	}
	return nil
}

func (j *JsonRpcUpstreamConfig) Validate(c *Config) error {
	if j.SupportsBatch != nil && *j.SupportsBatch {
		if j.BatchMaxWait == 0 {
			return fmt.Errorf("jsonRpc.batchMaxWait is required and must be greater than 0")
		}
		if j.BatchMaxSize <= 0 {
			return fmt.Errorf("jsonRpc.batchMaxSize must be greater than 0")
		}
	}
	if j.ProxyPool != "" {
		found := false
		allIds := []string{}
		for _, pool := range c.ProxyPools {
			allIds = append(allIds, pool.ID)
			if pool.ID == j.ProxyPool {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("jsonRpc.proxyPool '%s' does not exist in configured proxyPools, must be one of: %v", j.ProxyPool, allIds)
		}
	}
	return nil
}

func (r *RateLimitAutoTuneConfig) Validate() error {
	if r.Enabled == nil || !*r.Enabled {
		return nil
	}
	if r.AdjustmentPeriod == 0 {
		return fmt.Errorf("upstream.*.rateLimitAutoTune.adjustmentPeriod is required")
	}
	if r.ErrorRateThreshold <= 0 || r.ErrorRateThreshold > 1 {
		return fmt.Errorf("upstream.*.rateLimitAutoTune.errorRateThreshold must be greater than 0 and less than 1")
	}
	if r.IncreaseFactor <= 1 {
		return fmt.Errorf("upstream.*.rateLimitAutoTune.increaseFactor must be greater than 1")
	}
	if r.DecreaseFactor <= 0 || r.DecreaseFactor >= 1 {
		return fmt.Errorf("upstream.*.rateLimitAutoTune.decreaseFactor must be greater than 0 and less than 1")
	}
	if r.MinBudget < 0 {
		return fmt.Errorf("upstream.*.rateLimitAutoTune.minBudget must be greater than or equal to 0")
	}
	if r.MaxBudget < 0 {
		return fmt.Errorf("upstream.*.rateLimitAutoTune.maxBudget must be greater than or equal to 0")
	}
	return nil
}

func (r *RoutingConfig) Validate() error {
	if len(r.ScoreMultipliers) > 0 {
		for _, multiplier := range r.ScoreMultipliers {
			if err := multiplier.Validate(); err != nil {
				return err
			}
		}
	}
	if r.ScoreLatencyQuantile < 0 || r.ScoreLatencyQuantile > 1 {
		return fmt.Errorf("upstream.*.routing.scoreLatencyQuantile must be between 0 and 1")
	}
	return nil
}

func (n *NetworkConfig) Validate(c *Config) error {
	if n.Architecture == "" {
		return fmt.Errorf("network.*.architecture is required")
	}
	if n.Architecture == "evm" && n.Evm == nil {
		return fmt.Errorf("network.*.evm is required for evm networks")
	}
	if n.Evm != nil {
		if err := n.Evm.Validate(); err != nil {
			return err
		}
	}
	if n.Failsafe != nil {
		for _, fs := range n.Failsafe {
			if err := fs.Validate(); err != nil {
				return err
			}
		}
	}
	if n.SelectionPolicy != nil {
		if err := n.SelectionPolicy.Validate(); err != nil {
			return err
		}
	}
	if n.RateLimitBudget != "" {
		if !c.HasRateLimiterBudget(n.RateLimitBudget) {
			return fmt.Errorf("network.*.rateLimitBudget '%s' does not exist in config.rateLimiters", n.RateLimitBudget)
		}
	}
	if n.Alias != "" {
		// Check if alias contains only valid characters (alphanumeric, dash, underscore)
		if !util.IsValidIdentifier(n.Alias) {
			return fmt.Errorf("network.*.alias '%s' must contain only alphanumeric characters, dash, or underscore", n.Alias)
		}
	}
	return nil
}

func (e *EvmNetworkConfig) Validate() error {
	if e.FallbackFinalityDepth == 0 {
		return fmt.Errorf("network.*.evm.fallbackFinalityDepth must be greater than 0")
	}
	if e.FallbackStatePollerDebounce == 0 {
		return fmt.Errorf("network.*.evm.fallbackStatePollerDebounce is required")
	}
	return nil
}

func (c *SelectionPolicyConfig) Validate() error {
	if c.EvalInterval <= 0 {
		return fmt.Errorf("selectionPolicy.evalInterval must be greater than 0")
	}
	if c.EvalFunction == nil {
		return fmt.Errorf("selectionPolicy.evalFunction is required")
	}
	if c.ResampleInterval <= 0 {
		return fmt.Errorf("selectionPolicy.resampleInterval must be greater than 0")
	}
	if c.ResampleCount <= 0 {
		return fmt.Errorf("selectionPolicy.resampleCount must be greater than 0")
	}
	return nil
}

func (p *ScoreMultiplierConfig) Validate() error {
	if p.Overall == nil || *p.Overall <= 0 {
		return fmt.Errorf("priorityMultipliers.*.overall multiplier must be greater than 0")
	}
	if p.ErrorRate == nil || *p.ErrorRate < 0 {
		return fmt.Errorf("priorityMultipliers.*.errorRate multiplier must be greater than or equal to 0")
	}
	if p.RespLatency == nil || *p.RespLatency < 0 {
		return fmt.Errorf("priorityMultipliers.*.respLatency multiplier must be greater than or equal to 0")
	}
	if p.TotalRequests == nil || *p.TotalRequests < 0 {
		return fmt.Errorf("priorityMultipliers.*.totalRequests multiplier must be greater than or equal to 0")
	}
	if p.ThrottledRate == nil || *p.ThrottledRate < 0 {
		return fmt.Errorf("priorityMultipliers.*.throttledRate multiplier must be greater than or equal to 0")
	}
	if p.BlockHeadLag == nil || *p.BlockHeadLag < 0 {
		return fmt.Errorf("priorityMultipliers.*.blockHeadLag multiplier must be greater than or equal to 0")
	}
	if p.FinalizationLag == nil || *p.FinalizationLag < 0 {
		return fmt.Errorf("priorityMultipliers.*.finalizationLag multiplier must be greater than or equal to 0")
	}
	return nil
}
