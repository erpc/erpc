package common

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/erpc/erpc/util"
)

func (c *Config) Validate() error {
	if c.Server != nil {
		if err := c.Server.Validate(); err != nil {
			return err
		}
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
	}
	if c.RateLimiters != nil {
		if err := c.RateLimiters.Validate(); err != nil {
			return err
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
	if s.MaxTimeout == nil {
		return fmt.Errorf("server.maxTimeout is required")
	}
	_, err := time.ParseDuration(*s.MaxTimeout)
	if err != nil {
		return fmt.Errorf("server.maxTimeout is invalid (must be like 500ms, 2s, etc): %w", err)
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
	if r.WaitTime == "" {
		return fmt.Errorf("rateLimiter.*.budget.rules.*.waitTime is required")
	} else {
		_, err := time.ParseDuration(r.WaitTime)
		if err != nil {
			return fmt.Errorf("rateLimiter.*.budget.rules.*.waitTime is invalid: %w", err)
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
	return nil
}

func (c *ConnectorConfig) Validate() error {
	if c.Driver == "" {
		return fmt.Errorf("database.*.connector.driver is required")
	}
	drivers := []ConnectorDriverType{DriverMemory, DriverRedis, DriverPostgres, DriverDynamoDB}
	if !slices.Contains(drivers, c.Driver) {
		return fmt.Errorf("database.*.connector.driver is invalid must be one of: %v", drivers)
	}
	if c.Driver == DriverMemory && c.Memory == nil {
		return fmt.Errorf("database.*.connector.memory is required when driver is memory")
	}
	if c.Driver == DriverRedis && c.Redis == nil {
		return fmt.Errorf("database.*.connector.redis is required when driver is redis")
	}
	if c.Driver == DriverPostgres && c.PostgreSQL == nil {
		return fmt.Errorf("database.*.connector.postgres is required when driver is postgres")
	}
	if c.Driver == DriverDynamoDB && c.DynamoDB == nil {
		return fmt.Errorf("database.*.connector.dynamodb is required when driver is dynamodb")
	}

	// TODO switch to go-validator library :D
	if c.Memory != nil && (c.Redis != nil || c.PostgreSQL != nil || c.DynamoDB != nil) {
		return fmt.Errorf("database.*.connector.memory is mutually exclusive with database.*.connector.redis, database.*.connector.postgres, and database.*.connector.dynamodb")
	}
	if c.Redis != nil && (c.Memory != nil || c.PostgreSQL != nil || c.DynamoDB != nil) {
		return fmt.Errorf("database.*.connector.redis is mutually exclusive with database.*.connector.memory, database.*.connector.postgres, and database.*.connector.dynamodb")
	}
	if c.PostgreSQL != nil && (c.Memory != nil || c.Redis != nil || c.DynamoDB != nil) {
		return fmt.Errorf("database.*.connector.postgres is mutually exclusive with database.*.connector.memory, database.*.connector.redis, and database.*.connector.dynamodb")
	}
	if c.DynamoDB != nil && (c.Memory != nil || c.Redis != nil || c.PostgreSQL != nil) {
		return fmt.Errorf("database.*.connector.dynamodb is mutually exclusive with database.*.connector.memory, database.*.connector.redis, and database.*.connector.postgres")
	}

	return nil
}

func (p *ProjectConfig) Validate(c *Config) error {
	if p.Id == "" {
		return fmt.Errorf("project id is required")
	}
	if p.Upstreams != nil && len(p.Upstreams) > 0 {
		for _, upstream := range p.Upstreams {
			if err := upstream.Validate(c); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("project.*.upstreams is required, add at least one upstream")
	}
	if p.Networks != nil {
		for _, network := range p.Networks {
			if err := network.Validate(c); err != nil {
				return err
			}
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
	if p.HealthCheck != nil {
		if err := p.HealthCheck.Validate(); err != nil {
			return err
		}
	}
	if p.RateLimitBudget != "" {
		if !c.HasRateLimiterBudget(p.RateLimitBudget) {
			return fmt.Errorf("project.*.rateLimitBudget '%s' does not exist in config.rateLimiters", p.RateLimitBudget)
		}
	}
	return nil
}

func (a *AuthConfig) Validate() error {
	if a.Strategies == nil || len(a.Strategies) == 0 {
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
		return fmt.Errorf("auth.*.type is invalid must be one of: %v", []AuthType{
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
	if j.VerificationKeys == nil || len(j.VerificationKeys) == 0 {
		return fmt.Errorf("auth.*.jwt.verificationKeys is required, add at least one verification key")
	}
	return nil
}

func (s *SiweStrategyConfig) Validate() error {
	return nil
}

func (c *CORSConfig) Validate() error {
	if c.AllowedOrigins == nil || len(c.AllowedOrigins) == 0 {
		return fmt.Errorf("*.cors.allowedOrigins is required, add at least one allowed origin")
	}
	return nil
}

func (h *HealthCheckConfig) Validate() error {
	if h.ScoreMetricsWindowSize == "" {
		return fmt.Errorf("project.*.healthCheck.scoreMetricsWindowSize is required")
	}
	return nil
}

func (u *UpstreamConfig) Validate(c *Config) error {
	if u.Endpoint == "" {
		return fmt.Errorf("upstream.*.endpoint is required")
	}
	if u.Evm != nil {
		if err := u.Evm.Validate(u); err != nil {
			return err
		}
	}
	if u.Failsafe != nil {
		if err := u.Failsafe.Validate(); err != nil {
			return err
		}
	}
	if u.JsonRpc != nil {
		if err := u.JsonRpc.Validate(); err != nil {
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
	if !strings.HasPrefix(u.Endpoint, "http") && !strings.HasPrefix(u.Endpoint, "ws") {
		if e.ChainId > 0 {
			return fmt.Errorf("upstream.*.evm.chainId must be 0 for non-http endpoints, but '%d' is provided for %s", e.ChainId, util.RedactEndpoint(u.Endpoint))
		}
	}

	if e.StatePollerInterval == "" {
		return fmt.Errorf("upstream.*.evm.statePollerInterval is required")
	}
	_, err := time.ParseDuration(e.StatePollerInterval)
	if err != nil {
		return fmt.Errorf("upstream.*.evm.statePollerInterval is invalid (must be like 10s, 5m, etc): %w", err)
	}
	if e.NodeType != "" {
		allowed := []EvmNodeType{
			EvmNodeTypeArchive,
			EvmNodeTypeFull,
		}
		if !slices.Contains(allowed, e.NodeType) {
			return fmt.Errorf("upstream.*.evm.nodeType is invalid must be one of: %v", allowed)
		}
	}
	return nil
}

func (f *FailsafeConfig) Validate() error {
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
	if t.Duration == "" {
		return fmt.Errorf("upstream.*.failsafe.timeout.duration is required")
	}
	_, err := time.ParseDuration(t.Duration)
	if err != nil {
		return fmt.Errorf("upstream.*.failsafe.timeout.duration is invalid (must be like 500ms, 2s, etc): %w", err)
	}
	return nil
}

func (r *RetryPolicyConfig) Validate() error {
	if r.BackoffFactor <= 0 {
		return fmt.Errorf("upstream.*.failsafe.retry.backoffFactor must be greater than 0")
	}
	if r.BackoffMaxDelay == "" {
		return fmt.Errorf("upstream.*.failsafe.retry.backoffMaxDelay is required")
	}
	_, err := time.ParseDuration(r.BackoffMaxDelay)
	if err != nil {
		return fmt.Errorf("upstream.*.failsafe.retry.backoffMaxDelay is invalid (must be like 500ms, 2s, etc): %w", err)
	}
	if r.Jitter == "" {
		return fmt.Errorf("upstream.*.failsafe.retry.jitter is required")
	}
	_, err = time.ParseDuration(r.Jitter)
	if err != nil {
		return fmt.Errorf("upstream.*.failsafe.retry.jitter is invalid (must be like 500ms, 2s, etc): %w", err)
	}
	if r.Delay == "" {
		return fmt.Errorf("upstream.*.failsafe.retry.delay is required")
	}
	_, err = time.ParseDuration(r.Delay)
	if err != nil {
		return fmt.Errorf("upstream.*.failsafe.retry.delay is invalid (must be like 500ms, 2s, etc): %w", err)
	}
	return nil
}

func (h *HedgePolicyConfig) Validate() error {
	if h.Delay == "" {
		return fmt.Errorf("upstream.*.failsafe.hedge.delay is required")
	}
	_, err := time.ParseDuration(h.Delay)
	if err != nil {
		return fmt.Errorf("upstream.*.failsafe.hedge.delay is invalid (must be like 500ms, 2s, etc): %w", err)
	}
	return nil
}

func (c *CircuitBreakerPolicyConfig) Validate() error {
	if c.HalfOpenAfter == "" {
		return fmt.Errorf("upstream.*.failsafe.circuitBreaker.halfOpenAfter is required")
	}
	_, err := time.ParseDuration(c.HalfOpenAfter)
	if err != nil {
		return fmt.Errorf("upstream.*.failsafe.circuitBreaker.halfOpenAfter is invalid (must be like 30s, 5m, etc): %w", err)
	}
	if c.FailureThresholdCapacity <= 0 {
		return fmt.Errorf("upstream.*.failsafe.circuitBreaker.failureThresholdCapacity must be greater than 0")
	}
	if c.FailureThresholdCount <= 0 {
		return fmt.Errorf("upstream.*.failsafe.circuitBreaker.failureThresholdCount must be greater than 0")
	}
	if c.FailureThresholdCount > c.FailureThresholdCapacity {
		return fmt.Errorf("upstream.*.failsafe.circuitBreaker.failureThresholdCount must be less than or equal to failureThresholdCapacity")
	}
	if c.SuccessThresholdCount <= 0 {
		return fmt.Errorf("upstream.*.failsafe.circuitBreaker.successThresholdCount must be greater than 0")
	}
	if c.SuccessThresholdCapacity <= 0 {
		return fmt.Errorf("upstream.*.failsafe.circuitBreaker.successThresholdCapacity must be greater than 0")
	}
	if c.SuccessThresholdCount > c.SuccessThresholdCapacity {
		return fmt.Errorf("upstream.*.failsafe.circuitBreaker.successThresholdCount must be less than or equal to failureThresholdCapacity")
	}
	return nil
}

func (j *JsonRpcUpstreamConfig) Validate() error {
	if j.SupportsBatch == nil || !*j.SupportsBatch {
		return nil
	}
	if j.BatchMaxWait == "" {
		return fmt.Errorf("upstream.*.jsonRpc.batchMaxWait is required")
	}
	_, err := time.ParseDuration(j.BatchMaxWait)
	if err != nil {
		return fmt.Errorf("upstream.*.jsonRpc.batchMaxWait is invalid (must be like 500ms, 2s, etc): %w", err)
	}
	if j.BatchMaxSize <= 0 {
		return fmt.Errorf("upstream.*.jsonRpc.batchMaxSize must be greater than 0")
	}
	return nil
}

func (r *RateLimitAutoTuneConfig) Validate() error {
	if r.Enabled == nil || !*r.Enabled {
		return nil
	}
	if r.AdjustmentPeriod == "" {
		return fmt.Errorf("upstream.*.rateLimitAutoTune.adjustmentPeriod is required")
	}
	_, err := time.ParseDuration(r.AdjustmentPeriod)
	if err != nil {
		return fmt.Errorf("upstream.*.rateLimitAutoTune.adjustmentPeriod is invalid (must be like 30s, 5m, etc): %w", err)
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
		if err := n.Failsafe.Validate(); err != nil {
			return err
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
	return nil
}

func (e *EvmNetworkConfig) Validate() error {
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
	if p.Overall <= 0 {
		return fmt.Errorf("priorityMultipliers.*.overall multiplier must be greater than 0")
	}
	if p.ErrorRate < 0 {
		return fmt.Errorf("priorityMultipliers.*.errorRate multiplier must be greater than or equal to 0")
	}
	if p.P90Latency < 0 {
		return fmt.Errorf("priorityMultipliers.*.p90latency multiplier must be greater than or equal to 0")
	}
	if p.TotalRequests < 0 {
		return fmt.Errorf("priorityMultipliers.*.totalRequests multiplier must be greater than or equal to 0")
	}
	if p.ThrottledRate < 0 {
		return fmt.Errorf("priorityMultipliers.*.throttledRate multiplier must be greater than or equal to 0")
	}
	if p.BlockHeadLag < 0 {
		return fmt.Errorf("priorityMultipliers.*.blockHeadLag multiplier must be greater than or equal to 0")
	}
	if p.FinalizationLag < 0 {
		return fmt.Errorf("priorityMultipliers.*.finalizationLag multiplier must be greater than or equal to 0")
	}
	return nil
}
