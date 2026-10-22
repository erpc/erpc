package common

import (
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func covDefaultsDur(d time.Duration) Duration { return Duration(d) }

func TestCovDefaults_ConfigSetDefaults_EmptyConfigGetsMainProject(t *testing.T) {
	cfg := &Config{}
	require.NoError(t, cfg.SetDefaults(&DefaultOptions{}))

	assert.Equal(t, "INFO", cfg.LogLevel)
	assert.Equal(t, "erpc-default", cfg.ClusterKey)
	require.NotNil(t, cfg.HealthCheck)
	assert.Equal(t, HealthCheckModeNetworks, cfg.HealthCheck.Mode)
	assert.Equal(t, EvalAnyInitializedUpstreams, cfg.HealthCheck.DefaultEval)

	// A lone synthetic project gets an alias rule so /evm/1 works without /main.
	require.NotNil(t, cfg.Server.Aliasing)
	require.Len(t, cfg.Server.Aliasing.Rules, 1)
	assert.Equal(t, "*", cfg.Server.Aliasing.Rules[0].MatchDomain)
	assert.Equal(t, "main", cfg.Server.Aliasing.Rules[0].ServeProject)

	require.Len(t, cfg.Projects, 1)
	p := cfg.Projects[0]
	assert.Equal(t, "main", p.Id)
	require.Len(t, p.Providers, 1)
	assert.Equal(t, "public", p.Providers[0].Id)
	assert.Equal(t, "repository", p.Providers[0].Vendor)

	// The synthetic project's upstream defaults were defaulted in turn.
	require.Len(t, p.UpstreamDefaults.Failsafe, 1)
	assert.Equal(t, 1, p.UpstreamDefaults.Failsafe[0].Retry.MaxAttempts)
	assert.Equal(t, covDefaultsDur(500*time.Millisecond), p.UpstreamDefaults.Failsafe[0].Retry.Delay)
	assert.Equal(t, float32(1.2), p.UpstreamDefaults.Failsafe[0].Retry.BackoffFactor)
	require.Len(t, p.NetworkDefaults.Failsafe, 1)
	assert.Equal(t, 2, p.NetworkDefaults.Failsafe[0].Hedge.MaxCount)
	assert.Equal(t, covDefaultsDur(100*time.Millisecond), p.NetworkDefaults.Failsafe[0].Hedge.Delay.Min)
}

func TestCovDefaults_ConfigSetDefaults_EndpointsOptionBecomeUpstreams(t *testing.T) {
	cfg := &Config{}
	require.NoError(t, cfg.SetDefaults(&DefaultOptions{Endpoints: []string{"https://rpc.covdefaults-a.example", "https://rpc.covdefaults-a.example"}}))
	p := cfg.Projects[0]
	assert.Empty(t, p.Providers)
	require.Len(t, p.Upstreams, 2)
	for _, u := range p.Upstreams {
		assert.Equal(t, "https://rpc.covdefaults-a.example", u.Endpoint)
		assert.Equal(t, UpstreamTypeEvm, u.Type)
		// The synthetic project's upstreamDefaults failsafe is copied onto each.
		require.Len(t, u.Failsafe, 1)
		assert.Equal(t, 1, u.Failsafe[0].Retry.MaxAttempts)
	}
	// Ids are derived from the host and kept unique by a counter.
	assert.NotEqual(t, p.Upstreams[0].Id, p.Upstreams[1].Id)
	assert.Contains(t, p.Upstreams[0].Id, "rpc.covdefaults-a.example-")
}

func TestCovDefaults_ConfigSetDefaults_PropagatesComponents(t *testing.T) {
	cfg := &Config{
		ClusterKey: "my-cluster",
		Tracing:    &TracingConfig{Enabled: true},
		Database: &DatabaseConfig{
			SharedState: &SharedStateConfig{},
		},
		Admin: &AdminConfig{},
		RateLimiters: &RateLimiterConfig{
			Budgets: []*RateLimitBudgetConfig{{Id: "b", Rules: []*RateLimitRuleConfig{{MaxCount: 5, Period: RateLimitPeriod(99)}}}},
		},
		Projects: []*ProjectConfig{{Id: "p1", Upstreams: []*UpstreamConfig{{Endpoint: "https://x.example"}}}},
	}
	require.NoError(t, cfg.SetDefaults(nil))

	assert.Equal(t, "my-cluster", cfg.Database.SharedState.ClusterKey, "shared state inherits the top-level cluster key")
	assert.Equal(t, "grpc", string(cfg.Tracing.Protocol))
	require.NotNil(t, cfg.Admin.CORS)
	assert.Equal(t, []string{"*"}, cfg.Admin.CORS.AllowedOrigins)
	assert.Equal(t, "memory", cfg.RateLimiters.Store.Driver)
	rule := cfg.RateLimiters.Budgets[0].Rules[0]
	assert.Equal(t, RateLimitPeriodSecond, rule.Period, "invalid period falls back to second")
	assert.Equal(t, "*", rule.Method)
	assert.Nil(t, cfg.Server.Aliasing, "explicit projects get no implicit aliasing rule")
	assert.True(t, cfg.HasRateLimiterBudget("b"))
	assert.False(t, cfg.HasRateLimiterBudget("missing"))
}

func TestCovDefaults_ConfigSetDefaults_ErrorsPropagate(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"database", &Config{Database: &DatabaseConfig{SharedState: &SharedStateConfig{Connector: &ConnectorConfig{Redis: &RedisConnectorConfig{URI: "redis://a", Addr: "b"}}}}}, "not both"},
		{"admin", &Config{Admin: &AdminConfig{Auth: &AuthConfig{Strategies: []*AuthStrategyConfig{{Type: AuthTypeDatabase, Database: &DatabaseStrategyConfig{Connector: &ConnectorConfig{Redis: &RedisConnectorConfig{URI: "redis://a", Addr: "b"}}}}}}}}, "failed to set defaults for auth"},
		{"project", &Config{Projects: []*ProjectConfig{{Id: "p", Networks: []*NetworkConfig{{SelectionPolicy: &SelectionPolicyConfig{EvalScope: "bogus"}}}}}}, "evalScope"},
		{"rateLimiters", &Config{Projects: []*ProjectConfig{{Id: "p", Upstreams: []*UpstreamConfig{{Endpoint: "https://x"}}}}, RateLimiters: &RateLimiterConfig{Store: &RateLimitStoreConfig{Driver: "redis", Redis: &RedisConnectorConfig{URI: "redis://a", Addr: "b"}}}}, "rate limit store"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.SetDefaults(nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestCovDefaults_ServerConfig_DeprecatedHttpPortMigrates(t *testing.T) {
	s := &ServerConfig{HttpPort: util.IntPtr(8080)}
	require.NoError(t, s.SetDefaults())
	assert.Equal(t, 8080, *s.HttpPortV4)
	assert.Equal(t, 9080, *s.HttpPortV6, "v6 port derives as http port + 1000")
	assert.Equal(t, 8080, *s.GrpcPortV4)
	assert.Equal(t, 9080, *s.GrpcPortV6)

	s2 := &ServerConfig{HttpPortV4: util.IntPtr(7000), HttpPortV6: util.IntPtr(7001), HttpHostV4: util.StringPtr("10.0.0.1")}
	require.NoError(t, s2.SetDefaults())
	assert.Equal(t, 7000, *s2.HttpPort, "deprecated field mirrors the v4 port")
	assert.Equal(t, 7001, *s2.HttpPortV6)
	assert.Equal(t, "10.0.0.1", *s2.GrpcHostV4)
	assert.Equal(t, "[::]", *s2.GrpcHostV6)
}

func TestCovDefaults_ServerConfig_OmittedFieldsAndExplicitWins(t *testing.T) {
	s := &ServerConfig{}
	require.NoError(t, s.SetDefaults())
	assert.Nil(t, s.ListenV4, "tests do not listen unless FORCE_TEST_LISTEN_V4")
	assert.Equal(t, covDefaultsDur(150*time.Second), *s.MaxTimeout)
	assert.Equal(t, covDefaultsDur(30*time.Second), *s.ReadTimeout)
	assert.Equal(t, covDefaultsDur(120*time.Second), *s.WriteTimeout)
	assert.Equal(t, covDefaultsDur(10*time.Second), *s.WaitBeforeShutdown)
	assert.Equal(t, covDefaultsDur(10*time.Second), *s.WaitAfterShutdown)
	assert.Equal(t, ExecutionHeadersAll, *s.ExecutionHeaders)
	assert.False(t, *s.CostHeaders)
	assert.True(t, *s.EnableGzip)
	assert.True(t, *s.IncludeErrorDetails)
	assert.False(t, *s.GrpcEnabled)
	assert.True(t, *s.GrpcReflection)
	assert.Equal(t, 100*1024*1024, *s.GrpcMaxRecvMsgSize)
	assert.Equal(t, []string{"127.0.0.1/8", "::1/128"}, s.TrustedIPForwarders)
	assert.Equal(t, []string{}, s.TrustedIPHeaders)
	ws := s.WebSocket
	assert.False(t, *ws.Enabled)
	assert.Equal(t, 4096, ws.ReadBufferSize)
	assert.Equal(t, int64(1024*1024), ws.MaxMessageSize)
	assert.Equal(t, covDefaultsDur(30*time.Second), *ws.PingInterval)
	assert.Equal(t, 100, ws.MaxSubscriptionsPerConnection)
	assert.Equal(t, 250, ws.MaxConcurrentRequestsPerConnection)
	assert.Equal(t, 256, ws.SubscriptionBufferSize)

	t.Setenv("FORCE_TEST_LISTEN_V4", "true")
	forced := &ServerConfig{
		TrustedIPForwarders: []string{"10.0.0.0/8"},
		WebSocket:           &WebSocketServerConfig{Enabled: util.BoolPtr(true), ReadBufferSize: 1},
	}
	require.NoError(t, forced.SetDefaults())
	assert.True(t, *forced.ListenV4)
	assert.Equal(t, []string{"10.0.0.0/8"}, forced.TrustedIPForwarders)
	assert.True(t, *forced.WebSocket.Enabled)
	assert.Equal(t, 1, forced.WebSocket.ReadBufferSize)
}

func TestCovDefaults_MetricsAndTracingDefaults(t *testing.T) {
	m := &MetricsConfig{}
	require.NoError(t, m.SetDefaults())
	assert.Nil(t, m.Enabled, "metrics stay disabled under go test")
	assert.Equal(t, "0.0.0.0", *m.HostV4)
	assert.Equal(t, "[::]", *m.HostV6)
	assert.Equal(t, 4001, *m.Port)
	assert.Equal(t, ErrorLabelModeCompact, m.ErrorLabelMode)
	assert.Equal(t, covDefaultsDur(24*time.Hour), *m.CounterIdleEvictionAfter)

	explicit := &MetricsConfig{Port: util.IntPtr(9100), ErrorLabelMode: ErrorLabelModeVerbose}
	require.NoError(t, explicit.SetDefaults())
	assert.Equal(t, 9100, *explicit.Port)
	assert.Equal(t, ErrorLabelModeVerbose, explicit.ErrorLabelMode)

	cases := []struct {
		in           TracingConfig
		wantEndpoint string
		wantProtocol TracingProtocol
	}{
		{TracingConfig{}, "localhost:4317", TracingProtocolGrpc},
		{TracingConfig{Protocol: TracingProtocolHttp}, "http://localhost:4318", TracingProtocolHttp},
		{TracingConfig{Endpoint: "otel:4317"}, "otel:4317", TracingProtocolGrpc},
	}
	for _, tc := range cases {
		tr := tc.in
		require.NoError(t, tr.SetDefaults())
		assert.Equal(t, tc.wantEndpoint, tr.Endpoint)
		assert.Equal(t, tc.wantProtocol, tr.Protocol)
		assert.Equal(t, 1.0, tr.SampleRate)
		assert.Equal(t, "erpc", tr.ServiceName)
	}
	tr := &TracingConfig{SampleRate: 0.25, ServiceName: "svc"}
	require.NoError(t, tr.SetDefaults())
	assert.Equal(t, 0.25, tr.SampleRate)
	assert.Equal(t, "svc", tr.ServiceName)
}

func TestCovDefaults_AdminAndCORS(t *testing.T) {
	a := &AdminConfig{Auth: &AuthConfig{Strategies: []*AuthStrategyConfig{{Secret: &SecretStrategyConfig{Value: "s"}}}}}
	require.NoError(t, a.SetDefaults())
	assert.Equal(t, AuthTypeSecret, a.Auth.Strategies[0].Type, "a secret block implies the secret type")
	assert.Equal(t, []string{"*"}, a.CORS.AllowedOrigins)
	assert.Equal(t, []string{"GET", "POST", "OPTIONS"}, a.CORS.AllowedMethods)
	assert.Equal(t, []string{"content-type", "authorization", "x-erpc-secret-token"}, a.CORS.AllowedHeaders)
	assert.False(t, *a.CORS.AllowCredentials)
	assert.Equal(t, 3600, a.CORS.MaxAge)

	c := &CORSConfig{AllowedOrigins: []string{"https://app"}, AllowedMethods: []string{"POST"}, AllowedHeaders: []string{"x"}, AllowCredentials: util.BoolPtr(true), MaxAge: 5}
	require.NoError(t, c.SetDefaults())
	assert.Equal(t, CORSConfig{AllowedOrigins: []string{"https://app"}, AllowedMethods: []string{"POST"}, AllowedHeaders: []string{"x"}, AllowCredentials: util.BoolPtr(true), MaxAge: 5}, *c)

	bad := &AdminConfig{Auth: &AuthConfig{Strategies: []*AuthStrategyConfig{{Type: AuthTypeDatabase, Database: &DatabaseStrategyConfig{Connector: &ConnectorConfig{Redis: &RedisConnectorConfig{URI: "redis://a", Addr: "b"}}}}}}}
	assert.ErrorContains(t, bad.SetDefaults(), "failed to set defaults for auth")
}

func TestCovDefaults_SharedState(t *testing.T) {
	s := &SharedStateConfig{}
	require.NoError(t, s.SetDefaults("ck"))
	assert.Equal(t, "ck", s.ClusterKey)
	assert.Equal(t, "memory", s.Connector.Id)
	assert.Equal(t, DriverMemory, s.Connector.Driver)
	assert.Equal(t, 100_000, s.Connector.Memory.MaxItems)
	assert.Equal(t, covDefaultsDur(3*time.Second), s.FallbackTimeout)
	assert.Equal(t, covDefaultsDur(4*time.Second), s.LockTtl)
	assert.Equal(t, covDefaultsDur(100*time.Millisecond), s.LockMaxWait)
	assert.Equal(t, covDefaultsDur(50*time.Millisecond), s.UpdateMaxWait)

	r := &SharedStateConfig{ClusterKey: "own", LockTtl: covDefaultsDur(time.Minute), Connector: &ConnectorConfig{Driver: DriverRedis, Redis: &RedisConnectorConfig{Addr: "r:6380"}}}
	require.NoError(t, r.SetDefaults("ck"))
	assert.Equal(t, "own", r.ClusterKey)
	assert.Equal(t, "redis", r.Connector.Id, "an id-less connector is named after its driver")
	assert.Equal(t, "redis://r:6380/0", r.Connector.Redis.URI)
	assert.Equal(t, covDefaultsDur(time.Minute), r.LockTtl)

	bad := &SharedStateConfig{Connector: &ConnectorConfig{Redis: &RedisConnectorConfig{URI: "redis://a", Addr: "b"}}}
	assert.ErrorContains(t, bad.SetDefaults("ck"), "not both")
}

func TestCovDefaults_DatabaseCaches(t *testing.T) {
	d := &DatabaseConfig{
		EvmJsonRpcCache: &CacheConfig{
			Connectors: []*ConnectorConfig{{Driver: DriverMemory}},
			Policies:   []*CachePolicyConfig{{Connector: "cache-memory"}},
		},
		SvmJsonRpcCache: &CacheConfig{Compression: &CompressionConfig{Enabled: util.BoolPtr(false), Algorithm: "zstd", ZstdLevel: "best", Threshold: 10}},
	}
	require.NoError(t, d.SetDefaults("ck"))
	evm := d.EvmJsonRpcCache
	assert.Equal(t, "cache-memory", evm.Connectors[0].Id)
	assert.Equal(t, &MemoryConnectorConfig{MaxItems: 100000, MaxTotalSize: "1GB"}, evm.Connectors[0].Memory)
	assert.Equal(t, "*", evm.Policies[0].Method)
	assert.Equal(t, "*", evm.Policies[0].Network)
	assert.Equal(t, CachePolicyAppliesToBoth, evm.Policies[0].AppliesTo)
	assert.Equal(t, &CompressionConfig{Enabled: util.BoolPtr(true), Algorithm: "zstd", ZstdLevel: "fastest", Threshold: 1024}, evm.Compression)
	assert.Equal(t, &CompressionConfig{Enabled: util.BoolPtr(false), Algorithm: "zstd", ZstdLevel: "best", Threshold: 10}, d.SvmJsonRpcCache.Compression)

	pol := &CachePolicyConfig{Method: "eth_call", Network: "evm:1", AppliesTo: CachePolicyAppliesToGet}
	require.NoError(t, pol.SetDefaults())
	assert.Equal(t, "eth_call", pol.Method)
	assert.Equal(t, CachePolicyAppliesToGet, pol.AppliesTo)

	for name, c := range map[string]*DatabaseConfig{
		"evm":    {EvmJsonRpcCache: &CacheConfig{Connectors: []*ConnectorConfig{{Redis: &RedisConnectorConfig{URI: "redis://a", Addr: "b"}}}}},
		"svm":    {SvmJsonRpcCache: &CacheConfig{Connectors: []*ConnectorConfig{{Redis: &RedisConnectorConfig{URI: "redis://a", Addr: "b"}}}}},
		"shared": {SharedState: &SharedStateConfig{Connector: &ConnectorConfig{Redis: &RedisConnectorConfig{URI: "redis://a", Addr: "b"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			assert.ErrorContains(t, c.SetDefaults("ck"), "not both")
		})
	}
}

func TestCovDefaults_ConnectorDriverInference(t *testing.T) {
	cases := []struct {
		name  string
		scope connectorScope
		in    ConnectorConfig
		check func(t *testing.T, c *ConnectorConfig)
	}{
		{"memory by driver", connectorScopeCache, ConnectorConfig{Driver: DriverMemory}, func(t *testing.T, c *ConnectorConfig) {
			assert.Equal(t, "cache-memory", c.Id)
			assert.Equal(t, 100000, c.Memory.MaxItems)
		}},
		{"redis block implies driver", connectorScopeCache, ConnectorConfig{Redis: &RedisConnectorConfig{Addr: "h"}}, func(t *testing.T, c *ConnectorConfig) {
			assert.Equal(t, DriverRedis, c.Driver)
			assert.Equal(t, "redis://h:6379/0", c.Redis.URI)
		}},
		{"redis driver without block", connectorScopeSharedState, ConnectorConfig{Driver: DriverRedis}, func(t *testing.T, c *ConnectorConfig) {
			require.NotNil(t, c.Redis)
			assert.Equal(t, 8, c.Redis.ConnPoolSize)
			assert.Equal(t, "", c.Redis.URI)
		}},
		{"postgres driver without block", connectorScopeCache, ConnectorConfig{Driver: DriverPostgreSQL}, func(t *testing.T, c *ConnectorConfig) {
			assert.Equal(t, "erpc_json_rpc_cache", c.PostgreSQL.Table)
		}},
		{"postgres block implies driver", connectorScopeAuth, ConnectorConfig{Id: "mine", PostgreSQL: &PostgreSQLConnectorConfig{}}, func(t *testing.T, c *ConnectorConfig) {
			assert.Equal(t, DriverPostgreSQL, c.Driver)
			assert.Equal(t, "mine", c.Id)
			assert.Equal(t, "erpc_auth", c.PostgreSQL.Table)
		}},
		{"dynamodb driver without block", connectorScopeSharedState, ConnectorConfig{Driver: DriverDynamoDB}, func(t *testing.T, c *ConnectorConfig) {
			assert.Equal(t, "erpc_shared_state", c.DynamoDB.Table)
		}},
		{"dynamodb block implies driver", connectorScopeCache, ConnectorConfig{DynamoDB: &DynamoDBConnectorConfig{Table: "t"}}, func(t *testing.T, c *ConnectorConfig) {
			assert.Equal(t, DriverDynamoDB, c.Driver)
			assert.Equal(t, "t", c.DynamoDB.Table)
		}},
		{"grpc driver without block", connectorScopeCache, ConnectorConfig{Driver: DriverGrpc}, func(t *testing.T, c *ConnectorConfig) {
			assert.Equal(t, covDefaultsDur(100*time.Millisecond), c.Grpc.GetTimeout)
		}},
		{"grpc block keeps explicit timeout", connectorScopeCache, ConnectorConfig{Grpc: &GrpcConnectorConfig{GetTimeout: covDefaultsDur(time.Second)}}, func(t *testing.T, c *ConnectorConfig) {
			assert.Equal(t, DriverGrpc, c.Driver)
			assert.Equal(t, covDefaultsDur(time.Second), c.Grpc.GetTimeout)
		}},
		{"failsafe lists default match method and skip nils", connectorScopeCache, ConnectorConfig{
			Driver:          DriverMemory,
			FailsafeForGets: []*FailsafeConfig{nil, {Retry: &RetryPolicyConfig{}}},
			FailsafeForSets: []*FailsafeConfig{nil, {MatchMethod: "eth_*"}},
		}, func(t *testing.T, c *ConnectorConfig) {
			assert.Nil(t, c.FailsafeForGets[0])
			assert.Equal(t, "*", c.FailsafeForGets[1].MatchMethod)
			assert.Equal(t, 3, c.FailsafeForGets[1].Retry.MaxAttempts)
			assert.Equal(t, "eth_*", c.FailsafeForSets[1].MatchMethod)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.in
			require.NoError(t, c.SetDefaults(tc.scope))
			tc.check(t, &c)
		})
	}
}

func TestCovDefaults_ConnectorErrorsAreWrapped(t *testing.T) {
	badHedge := []*FailsafeConfig{{Consensus: &ConsensusPolicyConfig{MisbehaviorsDestination: &MisbehaviorsDestinationConfig{}}}}
	_ = badHedge
	cases := []struct {
		name  string
		scope connectorScope
		in    ConnectorConfig
		want  string
	}{
		{"redis", connectorScopeCache, ConnectorConfig{Redis: &RedisConnectorConfig{URI: "redis://a", Addr: "b"}}, "failed to set defaults for redis connector"},
		{"postgres scope", connectorScope("bogus"), ConnectorConfig{Driver: DriverPostgreSQL}, "failed to set defaults for postgres connector: invalid connector scope: bogus"},
		{"dynamo scope", connectorScope("bogus"), ConnectorConfig{Driver: DriverDynamoDB}, "failed to set defaults for dynamo db connector: invalid connector scope: bogus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.in
			assert.ErrorContains(t, c.SetDefaults(tc.scope), tc.want)
		})
	}
}

func TestCovDefaults_RedisConnector(t *testing.T) {
	cases := []struct {
		name    string
		in      RedisConnectorConfig
		wantURI string
	}{
		{"addr without port", RedisConnectorConfig{Addr: "cache.local"}, "redis://cache.local:6379/0"},
		{"addr with scheme prefix and creds", RedisConnectorConfig{Addr: "redis://cache.local:6380", Username: "u", Password: "p@ss", DB: 2}, "redis://u:p%40ss@cache.local:6380/2"},
		{"rediss prefix stripped, tls picks scheme", RedisConnectorConfig{Addr: "rediss://c:1", TLS: &TLSConfig{Enabled: true}}, "rediss://c:1/0"},
		{"uri kept verbatim", RedisConnectorConfig{URI: "redis://x:1/3"}, "redis://x:1/3"},
		{"nothing set leaves uri empty", RedisConnectorConfig{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.in
			require.NoError(t, r.SetDefaults())
			assert.Equal(t, tc.wantURI, r.URI)
			if tc.in.Addr != "" {
				assert.Empty(t, r.Addr, "discrete fields are folded into the uri")
				assert.Empty(t, r.Username)
				assert.Empty(t, r.Password)
				assert.Zero(t, r.DB)
			}
			assert.Equal(t, 8, r.ConnPoolSize)
			assert.Equal(t, covDefaultsDur(5*time.Second), r.InitTimeout)
			assert.Equal(t, covDefaultsDur(time.Second), r.GetTimeout)
			assert.Equal(t, covDefaultsDur(3*time.Second), r.SetTimeout)
			assert.Equal(t, covDefaultsDur(500*time.Millisecond), r.LockRetryInterval)
		})
	}

	explicit := &RedisConnectorConfig{URI: "redis://x", ConnPoolSize: 2, InitTimeout: 1, GetTimeout: 1, SetTimeout: 1, LockRetryInterval: 1}
	require.NoError(t, explicit.SetDefaults())
	assert.Equal(t, 2, explicit.ConnPoolSize)
	assert.Equal(t, Duration(1), explicit.LockRetryInterval)

	err := (&RedisConnectorConfig{URI: "redis://a", Addr: "b"}).SetDefaults()
	assert.EqualError(t, err, "redis connector: provide either 'uri' or 'addr/username/password/db', not both")

	iam := &RedisConnectorConfig{Addr: "c:6379", IAMAuth: &RedisIAMAuthConfig{Enabled: true, CacheName: "  MyCache "}}
	require.NoError(t, iam.SetDefaults())
	assert.Equal(t, "mycache", iam.IAMAuth.CacheName)
	assert.True(t, iam.TLS.Enabled)
	assert.Equal(t, "rediss://c:6379/0", iam.URI)
}

func TestCovDefaults_PostgresConnector(t *testing.T) {
	cases := []struct {
		scope     connectorScope
		wantTable string
		wantMin   int32
		wantMax   int32
	}{
		{connectorScopeSharedState, "erpc_shared_state", 4, 32},
		{connectorScopeCache, "erpc_json_rpc_cache", 4, 32},
		{connectorScopeAuth, "erpc_auth", 1, 4},
	}
	for _, tc := range cases {
		t.Run(string(tc.scope), func(t *testing.T) {
			p := &PostgreSQLConnectorConfig{}
			require.NoError(t, p.SetDefaults(tc.scope))
			assert.Equal(t, tc.wantTable, p.Table)
			assert.Equal(t, tc.wantMin, p.MinConns)
			assert.Equal(t, tc.wantMax, p.MaxConns)
			assert.Equal(t, covDefaultsDur(5*time.Second), p.InitTimeout)
			assert.Equal(t, covDefaultsDur(time.Second), p.GetTimeout)
			assert.Equal(t, covDefaultsDur(2*time.Second), p.SetTimeout)
		})
	}
	explicit := &PostgreSQLConnectorConfig{Table: "t", MinConns: 7, MaxConns: 9}
	require.NoError(t, explicit.SetDefaults(connectorScope("anything")), "explicit table skips scope validation")
	assert.Equal(t, int32(7), explicit.MinConns)
	assert.Equal(t, int32(9), explicit.MaxConns)

	assert.EqualError(t, (&PostgreSQLConnectorConfig{}).SetDefaults("weird"), "invalid connector scope: weird")

	withQuery := &PostgreSQLConnectorConfig{ConnectionUri: "postgres://u@db:5432/x?connect_timeout=5", IAMAuth: &PostgreSQLIAMAuthConfig{Enabled: true}}
	require.NoError(t, withQuery.SetDefaults(connectorScopeCache))
	assert.Equal(t, "postgres://u@db:5432/x?connect_timeout=5&sslmode=require", withQuery.ConnectionUri)

	alreadySSL := &PostgreSQLConnectorConfig{ConnectionUri: "postgres://u@db:5432/x?sslmode=verify-full", IAMAuth: &PostgreSQLIAMAuthConfig{Enabled: true}}
	require.NoError(t, alreadySSL.SetDefaults(connectorScopeCache))
	assert.Equal(t, "postgres://u@db:5432/x?sslmode=verify-full", alreadySSL.ConnectionUri)

	disabledIAM := &PostgreSQLConnectorConfig{ConnectionUri: "postgres://db/x", IAMAuth: &PostgreSQLIAMAuthConfig{Enabled: false}}
	require.NoError(t, disabledIAM.SetDefaults(connectorScopeCache))
	assert.Equal(t, "postgres://db/x", disabledIAM.ConnectionUri)
	assert.Empty(t, disabledIAM.IAMAuth.Endpoint)
}

func TestCovDefaults_DynamoDBConnector(t *testing.T) {
	for scope, table := range map[connectorScope]string{
		connectorScopeSharedState: "erpc_shared_state",
		connectorScopeCache:       "erpc_json_rpc_cache",
		connectorScopeAuth:        "erpc_auth",
	} {
		d := &DynamoDBConnectorConfig{}
		require.NoError(t, d.SetDefaults(scope))
		assert.Equal(t, &DynamoDBConnectorConfig{
			Table:             table,
			PartitionKeyName:  "groupKey",
			RangeKeyName:      "requestKey",
			ReverseIndexName:  "idx_requestKey_groupKey",
			TTLAttributeName:  "ttl",
			InitTimeout:       covDefaultsDur(5 * time.Second),
			GetTimeout:        covDefaultsDur(time.Second),
			SetTimeout:        covDefaultsDur(2 * time.Second),
			StatePollInterval: covDefaultsDur(5 * time.Second),
		}, d)
	}
	explicit := &DynamoDBConnectorConfig{Table: "x", PartitionKeyName: "pk", RangeKeyName: "rk", ReverseIndexName: "ri", TTLAttributeName: "exp", InitTimeout: 1, GetTimeout: 2, SetTimeout: 3, StatePollInterval: 4}
	want := *explicit
	require.NoError(t, explicit.SetDefaults("bogus"))
	assert.Equal(t, want, *explicit)
	assert.EqualError(t, (&DynamoDBConnectorConfig{}).SetDefaults("bogus"), "invalid connector scope: bogus")
}

func TestCovDefaults_AuthStrategies(t *testing.T) {
	t.Run("type implies empty sub-config", func(t *testing.T) {
		for _, typ := range []AuthType{AuthTypeNetwork, AuthTypeSecret, AuthTypeJwt, AuthTypeSiwe, AuthTypeDatabase} {
			s := &AuthStrategyConfig{Type: typ}
			require.NoError(t, s.SetDefaults())
			assert.Equal(t, typ, s.Type)
			switch typ {
			case AuthTypeNetwork:
				assert.NotNil(t, s.Network)
			case AuthTypeSecret:
				assert.NotNil(t, s.Secret)
			case AuthTypeJwt:
				assert.Equal(t, "rlm", s.Jwt.RateLimitBudgetClaimName)
				assert.Zero(t, s.Jwt.VerificationJwksRefreshInterval, "no jwks url, no refresh interval")
			case AuthTypeSiwe:
				assert.NotNil(t, s.Siwe)
			case AuthTypeDatabase:
				assert.Equal(t, int64(10000), *s.Database.Cache.MaxSize)
			}
		}
	})
	t.Run("sub-config block sets the type", func(t *testing.T) {
		cases := []struct {
			in   AuthStrategyConfig
			want AuthType
		}{
			{AuthStrategyConfig{Jwt: &JwtStrategyConfig{}}, AuthTypeJwt},
			{AuthStrategyConfig{Siwe: &SiweStrategyConfig{}}, AuthTypeSiwe},
			{AuthStrategyConfig{Database: &DatabaseStrategyConfig{}}, AuthTypeDatabase},
			{AuthStrategyConfig{Secret: &SecretStrategyConfig{}}, AuthTypeSecret},
		}
		for _, tc := range cases {
			s := tc.in
			require.NoError(t, s.SetDefaults())
			assert.Equal(t, tc.want, s.Type)
		}
	})
	t.Run("jwt keeps explicit values and defaults jwks refresh", func(t *testing.T) {
		j := &JwtStrategyConfig{RateLimitBudgetClaimName: "tier", VerificationJwksUrl: "https://idp/jwks"}
		require.NoError(t, j.SetDefaults())
		assert.Equal(t, "tier", j.RateLimitBudgetClaimName)
		assert.Equal(t, covDefaultsDur(time.Hour), j.VerificationJwksRefreshInterval)
		j2 := &JwtStrategyConfig{VerificationJwksUrl: "https://idp/jwks", VerificationJwksRefreshInterval: covDefaultsDur(time.Minute)}
		require.NoError(t, j2.SetDefaults())
		assert.Equal(t, covDefaultsDur(time.Minute), j2.VerificationJwksRefreshInterval)
	})
	t.Run("database strategy defaults", func(t *testing.T) {
		d := &DatabaseStrategyConfig{Connector: &ConnectorConfig{Driver: DriverPostgreSQL, PostgreSQL: &PostgreSQLConnectorConfig{ConnectionUri: "postgres://db"}}}
		require.NoError(t, d.SetDefaults())
		assert.Equal(t, time.Hour, *d.Cache.TTL)
		assert.Equal(t, int64(10000), *d.Cache.MaxSize)
		assert.Equal(t, int64(1<<30), *d.Cache.MaxCost)
		assert.Equal(t, int64(100000), *d.Cache.NumCounters)
		assert.Equal(t, 3, d.Retry.MaxAttempts)
		assert.Equal(t, covDefaultsDur(100*time.Millisecond), d.Retry.BaseBackoff)
		assert.False(t, d.FailOpen.Enabled)
		assert.Equal(t, "emergency-failopen", d.FailOpen.UserId)
		assert.Equal(t, covDefaultsDur(time.Second), d.MaxWait)
		assert.Equal(t, "auth-postgresql", d.Connector.Id)
		assert.Equal(t, "erpc_auth", d.Connector.PostgreSQL.Table)
		assert.Equal(t, int32(1), d.Connector.PostgreSQL.MinConns)
		assert.Equal(t, int32(4), d.Connector.PostgreSQL.MaxConns)
	})
	t.Run("database strategy explicit values survive", func(t *testing.T) {
		ttl := time.Minute
		size, cost, counters := int64(1), int64(2), int64(3)
		d := &DatabaseStrategyConfig{
			Connector: &ConnectorConfig{Driver: DriverMemory},
			Cache:     &DatabaseStrategyCacheConfig{TTL: &ttl, MaxSize: &size, MaxCost: &cost, NumCounters: &counters},
			Retry:     &DatabaseRetryConfig{MaxAttempts: -1, BaseBackoff: covDefaultsDur(time.Second)},
			FailOpen:  &DatabaseFailOpenConfig{Enabled: true, UserId: "ops"},
			MaxWait:   covDefaultsDur(5 * time.Second),
		}
		require.NoError(t, d.SetDefaults())
		assert.Equal(t, time.Minute, *d.Cache.TTL)
		assert.Equal(t, int64(1), *d.Cache.MaxSize)
		assert.Equal(t, 3, d.Retry.MaxAttempts, "non-positive attempts fall back to 3")
		assert.Equal(t, covDefaultsDur(time.Second), d.Retry.BaseBackoff)
		assert.Equal(t, "ops", d.FailOpen.UserId)
		assert.Equal(t, covDefaultsDur(5*time.Second), d.MaxWait)
	})
	t.Run("database strategy connector error wrapped", func(t *testing.T) {
		s := &AuthStrategyConfig{Database: &DatabaseStrategyConfig{Connector: &ConnectorConfig{Driver: DriverPostgreSQL, PostgreSQL: &PostgreSQLConnectorConfig{}, Redis: &RedisConnectorConfig{URI: "redis://a", Addr: "b"}}}}
		assert.ErrorContains(t, s.SetDefaults(), "failed to set defaults for database strategy")
		a := &AuthConfig{Strategies: []*AuthStrategyConfig{s}}
		assert.ErrorContains(t, a.SetDefaults(), "failed to set defaults for auth strategy")
	})
}

func TestCovDefaults_RateLimiters(t *testing.T) {
	r := &RateLimiterConfig{Store: &RateLimitStoreConfig{Driver: "redis"}, Budgets: []*RateLimitBudgetConfig{
		{Id: "b1", Rules: []*RateLimitRuleConfig{{Method: "eth_call", Period: RateLimitPeriodHour}, {Period: RateLimitPeriod(-3)}}},
		{Id: "b2"},
	}}
	require.NoError(t, r.SetDefaults())
	assert.Equal(t, 8, r.Store.Redis.ConnPoolSize, "redis store gets connector defaults")
	assert.Equal(t, "eth_call", r.Budgets[0].Rules[0].Method)
	assert.Equal(t, RateLimitPeriodHour, r.Budgets[0].Rules[0].Period)
	assert.Equal(t, "*", r.Budgets[0].Rules[1].Method)
	assert.Equal(t, RateLimitPeriodSecond, r.Budgets[0].Rules[1].Period)
	assert.Same(t, r.Budgets[1], (&Config{RateLimiters: r}).RateLimiterBudget("b2"))
	assert.Nil(t, (&Config{}).RateLimiterBudget("b2"))

	mem := &RateLimiterConfig{}
	require.NoError(t, mem.SetDefaults())
	assert.Equal(t, "memory", mem.Store.Driver)
	assert.Nil(t, mem.Store.Redis)
}

func TestCovDefaults_MisbehaviorsDestinationAndAutoTune(t *testing.T) {
	f := &MisbehaviorsDestinationConfig{}
	require.NoError(t, f.SetDefaults())
	assert.Equal(t, MisbehaviorsDestinationTypeFile, f.Type)
	assert.Equal(t, "{timestampMs}-{method}-{networkId}", f.FilePattern)
	assert.Nil(t, f.S3, "file destinations get no s3 block")

	s3 := &MisbehaviorsDestinationConfig{Type: MisbehaviorsDestinationTypeS3, FilePattern: "x"}
	require.NoError(t, s3.SetDefaults())
	assert.Equal(t, "x", s3.FilePattern)
	assert.Equal(t, &S3FlushConfig{MaxRecords: 100, MaxSize: 1024 * 1024, FlushInterval: covDefaultsDur(60 * time.Second), ContentType: "application/jsonl"}, s3.S3)

	s3e := &MisbehaviorsDestinationConfig{Type: MisbehaviorsDestinationTypeS3, S3: &S3FlushConfig{MaxRecords: 1, MaxSize: 2, FlushInterval: 3, ContentType: "text/plain", Region: "eu"}}
	require.NoError(t, s3e.SetDefaults())
	assert.Equal(t, &S3FlushConfig{MaxRecords: 1, MaxSize: 2, FlushInterval: 3, ContentType: "text/plain", Region: "eu"}, s3e.S3)

	// Consensus applies the destination defaults to its own destination.
	c := &ConsensusPolicyConfig{MisbehaviorsDestination: &MisbehaviorsDestinationConfig{Type: MisbehaviorsDestinationTypeS3}}
	require.NoError(t, c.SetDefaults())
	assert.Equal(t, 100, c.MisbehaviorsDestination.S3.MaxRecords)

	at := &RateLimitAutoTuneConfig{}
	require.NoError(t, at.SetDefaults())
	assert.True(t, *at.Enabled)
	assert.Equal(t, covDefaultsDur(time.Minute), at.AdjustmentPeriod)
	assert.Equal(t, 0.1, at.ErrorRateThreshold)
	assert.Equal(t, 1.05, at.IncreaseFactor)
	assert.Equal(t, 0.95, at.DecreaseFactor)
	assert.Equal(t, 100000, at.MaxBudget)

	at2 := &RateLimitAutoTuneConfig{Enabled: util.BoolPtr(false), IncreaseFactor: 2, DecreaseFactor: 0.5, MaxBudget: 10}
	require.NoError(t, at2.SetDefaults())
	assert.False(t, *at2.Enabled)
	assert.Equal(t, 2.0, at2.IncreaseFactor)
	assert.Equal(t, 10, at2.MaxBudget)
}
