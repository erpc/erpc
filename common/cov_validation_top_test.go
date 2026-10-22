package common

import (
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cvCase is one row of a "valid baseline, mutate one field" table: mutate
// breaks exactly one rule and wantErr is the message fragment that rule emits.
type cvCase[T any] struct {
	name    string
	mutate  func(*T)
	wantErr string
}

// cvRunCases builds a fresh baseline per row, asserts the untouched baseline
// passes, and that each mutation is rejected with its specific message.
func cvRunCases[T any](t *testing.T, baseline func() *T, validate func(*T) error, cases []cvCase[T]) {
	t.Helper()
	t.Run("valid baseline accepted", func(t *testing.T) {
		require.NoError(t, validate(baseline()))
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := baseline()
			tc.mutate(v)
			err := validate(v)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func cvDur(d time.Duration) *Duration { v := Duration(d); return &v }

func cvValidConfig() *Config {
	return &Config{
		Server:      &ServerConfig{MaxTimeout: cvDur(30 * time.Second)},
		HealthCheck: &HealthCheckConfig{},
		Projects: []*ProjectConfig{{
			Id:        "main",
			Upstreams: []*UpstreamConfig{{Id: "u1", Endpoint: "http://rpc.example"}},
		}},
	}
}

func TestCovValidation_Config(t *testing.T) {
	cvRunCases(t, cvValidConfig, (*Config).Validate, []cvCase[Config]{
		{"server missing", func(c *Config) { c.Server = nil }, "server config is required"},
		{"server invalid", func(c *Config) { c.Server.MaxTimeout = nil }, "server.maxTimeout is required"},
		{"healthCheck missing", func(c *Config) { c.HealthCheck = nil }, "healthCheck config is required"},
		{"healthCheck auth invalid", func(c *Config) { c.HealthCheck.Auth = &AuthConfig{} }, "project.*.auth.strategies is required"},
		{"metrics invalid", func(c *Config) { c.Metrics = &MetricsConfig{ErrorLabelMode: "loud"} }, "metrics.errorLabelMode must be either"},
		{"metrics valid", func(c *Config) { c.Metrics = &MetricsConfig{} }, ""},
		{"admin invalid", func(c *Config) { c.Admin = &AdminConfig{CORS: &CORSConfig{}} }, "*.cors.allowedOrigins is required"},
		{"admin valid", func(c *Config) { c.Admin = &AdminConfig{CORS: &CORSConfig{AllowedOrigins: []string{"*"}}} }, ""},
		{"database invalid", func(c *Config) {
			c.Database = &DatabaseConfig{SharedState: &SharedStateConfig{}}
		}, "sharedState.connector is required"},
		{"projects missing", func(c *Config) { c.Projects = nil }, "projects config is required"},
		{"project invalid", func(c *Config) { c.Projects[0].Id = "" }, "project id is required"},
		{"rateLimiters invalid", func(c *Config) { c.RateLimiters = &RateLimiterConfig{} }, "rateLimiters.store is required"},
		{"rateLimiters valid", func(c *Config) {
			c.RateLimiters = &RateLimiterConfig{Store: &RateLimitStoreConfig{Driver: "memory"}}
		}, ""},
		{"proxyPool invalid", func(c *Config) { c.ProxyPools = []*ProxyPoolConfig{{}} }, "proxyPool.*.id is required"},
		{"proxyPool valid", func(c *Config) {
			c.ProxyPools = []*ProxyPoolConfig{{ID: "p", Urls: []string{"http://proxy"}}}
		}, ""},
		{"indexer invalid", func(c *Config) { c.Indexer = &IndexerConfig{DedupWindowSize: -1} }, "indexer.dedupWindowSize must be >= 0"},
		{"indexer zero uses default", func(c *Config) { c.Indexer = &IndexerConfig{} }, ""},
	})
}

func TestCovValidation_Server(t *testing.T) {
	baseline := func() *ServerConfig {
		return &ServerConfig{
			ListenV4:   util.BoolPtr(true),
			HttpHostV4: util.StringPtr("0.0.0.0"),
			HttpPortV4: util.IntPtr(4000),
			ListenV6:   util.BoolPtr(true),
			HttpHostV6: util.StringPtr("[::]"),
			HttpPortV6: util.IntPtr(4000),
			MaxTimeout: cvDur(time.Second),
			TrustedIPForwarders: []string{
				"10.0.0.0/8", " 192.168.1.1 ", "::1",
			},
		}
	}
	cvRunCases(t, baseline, (*ServerConfig).Validate, []cvCase[ServerConfig]{
		{"listenV4 without host", func(s *ServerConfig) { s.HttpHostV4 = nil }, "server.listenV4 is true but server.httpHostV4 is not set"},
		{"listenV4 without port", func(s *ServerConfig) { s.HttpPortV4 = nil }, "server.listenV4 is true but server.httpPortV4 is not set"},
		{"listenV6 without host", func(s *ServerConfig) { s.HttpHostV6 = nil }, "server.listenV6 is true but server.httpHostV6 is not set"},
		{"listenV6 without port", func(s *ServerConfig) { s.HttpPortV6 = nil }, "server.listenV6 is true but server.httpPortV6 is not set"},
		{"listen disabled needs no host", func(s *ServerConfig) {
			s.ListenV4, s.ListenV6 = util.BoolPtr(false), util.BoolPtr(false)
			s.HttpHostV4, s.HttpHostV6 = nil, nil
		}, ""},
		{"maxTimeout nil", func(s *ServerConfig) { s.MaxTimeout = nil }, "server.maxTimeout is required"},
		{"maxTimeout zero", func(s *ServerConfig) { s.MaxTimeout = cvDur(0) }, "server.maxTimeout is required"},
		{"empty forwarder", func(s *ServerConfig) { s.TrustedIPForwarders = []string{"  "} }, "server.trustedForwarders contains empty entry"},
		{"bad CIDR", func(s *ServerConfig) { s.TrustedIPForwarders = []string{"10.0.0.0/99"} }, "server.trustedForwarders entry '10.0.0.0/99' is not a valid CIDR"},
		{"bad IP", func(s *ServerConfig) { s.TrustedIPForwarders = []string{"not-an-ip"} }, "server.trustedForwarders entry 'not-an-ip' is not a valid IP address"},
		{"webSocket negative limits", func(s *ServerConfig) {
			s.WebSocket = &WebSocketServerConfig{
				PingInterval:                       cvDur(time.Second),
				MaxSubscriptionsPerConnection:      1,
				MaxConcurrentRequestsPerConnection: 1,
				SubscriptionBufferSize:             1,
				MaxConnectionsPerIP:                -1,
			}
		}, "maxConnections, maxConnectionsPerIP and maxSubscriptions must not be negative"},
		{"webSocket nil pingInterval", func(s *ServerConfig) {
			s.WebSocket = &WebSocketServerConfig{}
		}, "server.webSocket.pingInterval must be greater than 0"},
	})
}

func TestCovValidation_Metrics(t *testing.T) {
	baseline := func() *MetricsConfig {
		return &MetricsConfig{
			Enabled:          util.BoolPtr(true),
			HostV4:           util.StringPtr("0.0.0.0"),
			Port:             util.IntPtr(4001),
			ErrorLabelMode:   ErrorLabelModeCompact,
			HistogramBuckets: "0.05, 0.5,5",
			Customizations: []*MetricsCustomizationConfig{
				{Subject: "erpc_upstream_*", Action: MetricActionDrop},
			},
		}
	}
	cvRunCases(t, baseline, (*MetricsConfig).Validate, []cvCase[MetricsConfig]{
		{"enabled without any host", func(m *MetricsConfig) { m.HostV4 = nil }, "metrics.hostV4 or metrics.hostV6 is required when metrics.enabled is true"},
		{"enabled with only v6 host", func(m *MetricsConfig) { m.HostV4, m.HostV6 = nil, util.StringPtr("::") }, ""},
		{"enabled without port", func(m *MetricsConfig) { m.Port = nil }, "metrics.port is required when metrics.enabled is true"},
		{"disabled needs no host or port", func(m *MetricsConfig) {
			m.Enabled, m.HostV4, m.Port = util.BoolPtr(false), nil, nil
		}, ""},
		{"verbose label mode", func(m *MetricsConfig) { m.ErrorLabelMode = ErrorLabelModeVerbose }, ""},
		{"unknown label mode", func(m *MetricsConfig) { m.ErrorLabelMode = "terse" }, "metrics.errorLabelMode must be either 'verbose' or 'compact'"},
		{"non-float bucket", func(m *MetricsConfig) { m.HistogramBuckets = "0.1,abc" }, "metrics.histogramBuckets contains invalid float value: abc"},
		{"malformed customization subject", func(m *MetricsConfig) {
			m.Customizations = []*MetricsCustomizationConfig{{Subject: "erpc_*_total", Action: MetricActionDrop}}
		}, "may only use '*' as its final character"},
		{"customization that does nothing", func(m *MetricsConfig) {
			m.Customizations = []*MetricsCustomizationConfig{{Subject: "erpc_x"}}
		}, "has no action, labels or buckets"},
	})
}

func TestCovValidation_AdminAndHealthCheck(t *testing.T) {
	validAuth := func() *AuthConfig {
		return &AuthConfig{Strategies: []*AuthStrategyConfig{{Type: AuthTypeSecret, Secret: &SecretStrategyConfig{Value: "s"}}}}
	}
	cvRunCases(t, func() *AdminConfig {
		return &AdminConfig{Auth: validAuth(), CORS: &CORSConfig{AllowedOrigins: []string{"https://app"}}}
	}, (*AdminConfig).Validate, []cvCase[AdminConfig]{
		{"auth invalid", func(a *AdminConfig) { a.Auth.Strategies[0].Secret.Value = "" }, "auth.*.secret.value is required"},
		{"cors without origins", func(a *AdminConfig) { a.CORS.AllowedOrigins = nil }, "*.cors.allowedOrigins is required"},
	})

	cvRunCases(t, func() *HealthCheckConfig {
		return &HealthCheckConfig{Auth: validAuth()}
	}, (*HealthCheckConfig).Validate, []cvCase[HealthCheckConfig]{
		{"auth strategy without type", func(h *HealthCheckConfig) { h.Auth.Strategies[0].Type = "" }, "auth.*.type is required"},
	})
}

func TestCovValidation_RateLimiters(t *testing.T) {
	baseline := func() *RateLimiterConfig {
		return &RateLimiterConfig{
			Store: &RateLimitStoreConfig{
				Driver:         " Redis ",
				Redis:          &RedisConnectorConfig{URI: "redis://localhost:6379"},
				NearLimitRatio: 0.8,
			},
			Budgets: []*RateLimitBudgetConfig{{
				Id:          "b1",
				Rules:       []*RateLimitRuleConfig{{Method: "*", MaxCount: 10, Period: RateLimitPeriodMinute}},
				CreditUnits: map[string]int64{"eth_call": 2},
			}},
		}
	}
	cvRunCases(t, baseline, (*RateLimiterConfig).Validate, []cvCase[RateLimiterConfig]{
		{"store missing", func(r *RateLimiterConfig) { r.Store = nil }, "rateLimiters.store is required"},
		{"redis store without redis", func(r *RateLimiterConfig) { r.Store.Redis = nil }, "rateLimiters.store.redis is required when store.type is 'redis'"},
		{"nearLimitRatio too high", func(r *RateLimiterConfig) { r.Store.NearLimitRatio = 1 }, "rateLimiters.store.nearLimitRatio must be > 0 and < 1"},
		{"nearLimitRatio negative", func(r *RateLimiterConfig) { r.Store.NearLimitRatio = -0.1 }, "rateLimiters.store.nearLimitRatio must be > 0 and < 1"},
		{"nearLimitRatio zero means default", func(r *RateLimiterConfig) { r.Store.NearLimitRatio = 0 }, ""},
		{"redis connector invalid", func(r *RateLimiterConfig) { r.Store.Redis.URI = "" }, "rateLimiters.store.redis is invalid: database.*.connector.redis.uri is required"},
		{"memory store", func(r *RateLimiterConfig) { r.Store = &RateLimitStoreConfig{Driver: "memory"} }, ""},
		{"empty driver", func(r *RateLimiterConfig) { r.Store.Driver = "" }, "rateLimiters.store.type '' is invalid must be one of: redis, memory"},
		{"unknown driver", func(r *RateLimiterConfig) { r.Store.Driver = "etcd" }, "rateLimiters.store.type 'etcd' is invalid"},
		{"budget without rules", func(r *RateLimiterConfig) { r.Budgets[0].Rules = nil }, "rateLimiter.*.budget.rules is required"},
		{"rule without method", func(r *RateLimiterConfig) { r.Budgets[0].Rules[0].Method = "" }, "rateLimiter.*.budget.rules.*.method is required"},
		{"negative creditUnits", func(r *RateLimiterConfig) { r.Budgets[0].CreditUnits["eth_call"] = -1 }, "rateLimiter.*.budget.creditUnits.eth_call must not be negative"},
	})
}

func TestCovValidation_RateLimitRule(t *testing.T) {
	baseline := func() *RateLimitRuleConfig {
		return &RateLimitRuleConfig{Method: "eth_*", MaxCount: 5, Period: RateLimitPeriodSecond}
	}
	cvRunCases(t, baseline, (*RateLimitRuleConfig).Validate, []cvCase[RateLimitRuleConfig]{
		{"deprecated waitTime is ignored", func(r *RateLimitRuleConfig) { r.WaitTime = Duration(time.Second) }, ""},
		{"unknown period", func(r *RateLimitRuleConfig) { r.Period = RateLimitPeriod(99) }, "rateLimiter.*.budget.rules.*.period must be one of"},
		{"year period", func(r *RateLimitRuleConfig) { r.Period = RateLimitPeriodYear }, ""},
		{"request count mode", func(r *RateLimitRuleConfig) { r.CountMode = RateLimitCountModeRequest }, ""},
		{"unknown count mode", func(r *RateLimitRuleConfig) { r.CountMode = "bytes" }, "countMode 'bytes' is invalid, must be one of: request, credit"},
		{"credit mode with zero ceiling", func(r *RateLimitRuleConfig) {
			r.CountMode, r.MaxCount = RateLimitCountModeCredit, 0
		}, "maxCount must be greater than 0 when countMode is credit"},
		{"credit mode with ceiling", func(r *RateLimitRuleConfig) { r.CountMode = RateLimitCountModeCredit }, ""},
	})
}

func TestCovValidation_ProxyPool(t *testing.T) {
	baseline := func() *ProxyPoolConfig {
		return &ProxyPoolConfig{ID: "pool", Urls: []string{"http://a", "HTTPS://b", "socks5://c"}}
	}
	cvRunCases(t, baseline, (*ProxyPoolConfig).Validate, []cvCase[ProxyPoolConfig]{
		{"id missing", func(p *ProxyPoolConfig) { p.ID = "" }, "proxyPool.*.id is required under proxyPools"},
		{"urls missing", func(p *ProxyPoolConfig) { p.Urls = nil }, "proxyPool.*.urls is required under proxyPool.*.id 'pool'"},
		{"unsupported scheme", func(p *ProxyPoolConfig) { p.Urls = []string{"ftp://x"} }, "must be valid HTTP, HTTPS, or SOCKS5 URLs, got: ftp://x"},
	})
}

func TestCovValidation_RateLimitBudgetHasCreditRule(t *testing.T) {
	b := &RateLimitBudgetConfig{Rules: []*RateLimitRuleConfig{nil, {CountMode: RateLimitCountModeRequest}}}
	assert.False(t, b.HasCreditRule())
	b.Rules = append(b.Rules, &RateLimitRuleConfig{CountMode: RateLimitCountModeCredit})
	assert.True(t, b.HasCreditRule())
}
