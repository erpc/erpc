package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func cvMemoryConnector(id string) *ConnectorConfig {
	return &ConnectorConfig{Id: id, Driver: DriverMemory, Memory: &MemoryConnectorConfig{MaxItems: 10}}
}

func cvValidSharedState() *SharedStateConfig {
	return &SharedStateConfig{
		Connector:       cvMemoryConnector("ss"),
		FallbackTimeout: Duration(time.Second),
		LockTtl:         Duration(4 * time.Second),
		LockMaxWait:     Duration(100 * time.Millisecond),
		UpdateMaxWait:   Duration(50 * time.Millisecond),
	}
}

func TestCovValidation_Database(t *testing.T) {
	baseline := func() *DatabaseConfig {
		return &DatabaseConfig{
			EvmJsonRpcCache: &CacheConfig{Connectors: []*ConnectorConfig{cvMemoryConnector("m")}},
			SvmJsonRpcCache: &CacheConfig{Connectors: []*ConnectorConfig{cvMemoryConnector("m")}},
			SharedState:     cvValidSharedState(),
		}
	}
	cvRunCases(t, baseline, (*DatabaseConfig).Validate, []cvCase[DatabaseConfig]{
		{"evm cache invalid", func(d *DatabaseConfig) { d.EvmJsonRpcCache.Connectors[0].Id = "" }, "*.connector.id is required"},
		{"svm cache invalid", func(d *DatabaseConfig) { d.SvmJsonRpcCache.Compression = &CompressionConfig{Algorithm: "gzip"} }, "cache.*.compression.algorithm must be 'zstd'"},
		{"shared state invalid", func(d *DatabaseConfig) { d.SharedState.LockTtl = 0 }, "sharedState.lockTtl is required"},
	})
}

func TestCovValidation_SharedState(t *testing.T) {
	cvRunCases(t, cvValidSharedState, (*SharedStateConfig).Validate, []cvCase[SharedStateConfig]{
		{"connector missing", func(s *SharedStateConfig) { s.Connector = nil }, "sharedState.connector is required"},
		{"connector invalid", func(s *SharedStateConfig) { s.Connector.Driver = "" }, "database.*.connector.driver is required"},
		{"fallbackTimeout missing", func(s *SharedStateConfig) { s.FallbackTimeout = 0 }, "sharedState.fallbackTimeout is required"},
		{"fallbackTimeout too small", func(s *SharedStateConfig) { s.FallbackTimeout = Duration(50 * time.Millisecond) }, "sharedState.fallbackTimeout should be at least 100ms"},
		{"lockTtl missing", func(s *SharedStateConfig) { s.LockTtl = 0 }, "sharedState.lockTtl is required"},
		{"lockTtl too small", func(s *SharedStateConfig) { s.LockTtl = Duration(500 * time.Millisecond) }, "sharedState.lockTtl should be at least 1s"},
		{"lockTtl shorter than fallbackTimeout", func(s *SharedStateConfig) {
			s.FallbackTimeout = Duration(3 * time.Second)
			s.LockTtl = Duration(2 * time.Second)
		}, "sharedState.lockTtl (2s) should be at least as long as fallbackTimeout (3s)"},
		{"lockMaxWait not below fallbackTimeout", func(s *SharedStateConfig) { s.LockMaxWait = Duration(time.Second) }, "sharedState.lockMaxWait (1s) should be less than fallbackTimeout (1s)"},
		{"updateMaxWait not below fallbackTimeout", func(s *SharedStateConfig) { s.UpdateMaxWait = Duration(2 * time.Second) }, "sharedState.updateMaxWait (2s) should be less than fallbackTimeout (1s)"},
	})
}

func TestCovValidation_CacheConfig(t *testing.T) {
	baseline := func() *CacheConfig {
		return &CacheConfig{
			Connectors: []*ConnectorConfig{cvMemoryConnector("a"), cvMemoryConnector("b")},
			Policies: []*CachePolicyConfig{{
				Network: "*", Method: "*", Connector: "b",
				MinItemSize: strPtrCv("1KB"), MaxItemSize: strPtrCv("1MB"),
				AppliesTo: CachePolicyAppliesToGet,
				Finality:  DataFinalityStateRealtime,
				TTL:       &BlockTimeAdaptiveDuration{BlockTimeMultiplier: 2},
			}},
			Compression: &CompressionConfig{Algorithm: "zstd", ZstdLevel: "better", Threshold: 1024},
		}
	}
	cvRunCases(t, baseline, (*CacheConfig).Validate, []cvCase[CacheConfig]{
		{"duplicate connector id", func(c *CacheConfig) { c.Connectors[1].Id = "a"; c.Policies[0].Connector = "a" }, "cache.*.connectors.*.id must be unique, 'a' is duplicated"},
		{"invalid connector", func(c *CacheConfig) { c.Connectors[0].Memory = nil }, "database.*.connector.memory is required when driver is memory"},
		{"policy without network", func(c *CacheConfig) { c.Policies[0].Network = "" }, "cache.*.policies.*.network is required"},
		{"policy without method", func(c *CacheConfig) { c.Policies[0].Method = "" }, "cache.*.policies.*.method is required"},
		{"policy without connector", func(c *CacheConfig) { c.Policies[0].Connector = "" }, "cache.*.policies.*.connector is required"},
		{"policy unknown connector", func(c *CacheConfig) { c.Policies[0].Connector = "zz" }, "cache.*.policies.*.connector 'zz' does not exist in cache.connectors"},
		{"bad minItemSize", func(c *CacheConfig) { c.Policies[0].MinItemSize = strPtrCv("lots") }, "cache.*.policies.*.minItemSize is invalid"},
		{"bad maxItemSize", func(c *CacheConfig) { c.Policies[0].MaxItemSize = strPtrCv("-5") }, "cache.*.policies.*.maxItemSize is invalid"},
		{"min above max", func(c *CacheConfig) { c.Policies[0].MinItemSize = strPtrCv("2MB") }, "cache.*.policies.*.minItemSize must be less than or equal to maxItemSize"},
		{"unknown appliesTo", func(c *CacheConfig) { c.Policies[0].AppliesTo = "delete" }, "cache.*.policies.*.appliesTo must be one of: get, set, both"},
		{"negative blockTimeMultiplier", func(c *CacheConfig) { c.Policies[0].TTL.BlockTimeMultiplier = -1 }, "cache.*.policies.*.ttl.blockTimeMultiplier must be >= 0"},
		{"blockTimeMultiplier on finalized", func(c *CacheConfig) { c.Policies[0].Finality = DataFinalityStateFinalized }, "ttl.blockTimeMultiplier is only supported when finality is 'realtime'"},
		{"fixed ttl on finalized", func(c *CacheConfig) {
			c.Policies[0].Finality = DataFinalityStateFinalized
			c.Policies[0].TTL = &BlockTimeAdaptiveDuration{Fallback: Duration(time.Minute)}
		}, ""},
		{"unknown compression algorithm", func(c *CacheConfig) { c.Compression.Algorithm = "lz4" }, "cache.*.compression.algorithm must be 'zstd'"},
		{"unknown zstd level", func(c *CacheConfig) { c.Compression.ZstdLevel = "max" }, "cache.*.compression.zstdLevel must be one of: fastest, default, better, best"},
		{"negative threshold", func(c *CacheConfig) { c.Compression.Threshold = -1 }, "cache.*.compression.threshold must be greater than or equal to 0"},
	})
}

func strPtrCv(s string) *string { return &s }

func cvValidConnector(driver ConnectorDriverType) *ConnectorConfig {
	c := &ConnectorConfig{Id: "c1", Driver: driver}
	switch driver {
	case DriverMemory:
		c.Memory = &MemoryConnectorConfig{MaxItems: 100}
	case DriverRedis:
		c.Redis = &RedisConnectorConfig{URI: "redis://localhost:6379", LockRetryInterval: Duration(200 * time.Millisecond)}
	case DriverPostgreSQL:
		c.PostgreSQL = &PostgreSQLConnectorConfig{
			ConnectionUri: "postgres://user:pw@localhost:5432/db",
			Table:         "rpc_cache",
			MinConns:      1, MaxConns: 4,
			InitTimeout: Duration(time.Second), GetTimeout: Duration(time.Second), SetTimeout: Duration(time.Second),
		}
	case DriverDynamoDB:
		c.DynamoDB = &DynamoDBConnectorConfig{
			Table: "t", PartitionKeyName: "pk", RangeKeyName: "rk", ReverseIndexName: "idx", TTLAttributeName: "ttl",
			InitTimeout: Duration(time.Second), GetTimeout: Duration(time.Second), SetTimeout: Duration(time.Second),
			StatePollInterval: Duration(time.Second),
		}
	case DriverGrpc:
		c.Grpc = &GrpcConnectorConfig{Servers: []string{"grpc://cache:50051"}, PoolSize: 4, HealthCheckService: "erpc"}
	}
	return c
}

func TestCovValidation_ConnectorCommon(t *testing.T) {
	cvRunCases(t, func() *ConnectorConfig { return cvValidConnector(DriverMemory) }, (*ConnectorConfig).Validate, []cvCase[ConnectorConfig]{
		{"id missing", func(c *ConnectorConfig) { c.Id = "" }, "*.connector.id is required"},
		{"driver missing", func(c *ConnectorConfig) { c.Driver = "" }, "database.*.connector.driver is required"},
		{"unknown driver", func(c *ConnectorConfig) { c.Driver = "mongo" }, "database.*.connector.driver 'mongo' is invalid must be one of"},
		{"memory with redis", func(c *ConnectorConfig) { c.Redis = &RedisConnectorConfig{URI: "redis://x"} }, "database.*.connector.memory is mutually exclusive"},
		{"redis with postgres", func(c *ConnectorConfig) {
			c.Driver, c.Memory = DriverRedis, nil
			c.Redis = &RedisConnectorConfig{URI: "redis://x"}
			c.PostgreSQL = &PostgreSQLConnectorConfig{}
		}, "database.*.connector.redis is mutually exclusive"},
		{"postgres with dynamo", func(c *ConnectorConfig) {
			c.Driver, c.Memory = DriverPostgreSQL, nil
			c.PostgreSQL = &PostgreSQLConnectorConfig{}
			c.DynamoDB = &DynamoDBConnectorConfig{}
		}, "database.*.connector.postgresql is mutually exclusive"},
		{"dynamo driver with empty dynamo block", func(c *ConnectorConfig) {
			c.Driver, c.Memory = DriverDynamoDB, nil
			c.DynamoDB = &DynamoDBConnectorConfig{}
		}, "database.*.connector.dynamodb.table is required"},
		{"failsafeForGets consensus", func(c *ConnectorConfig) {
			c.FailsafeForGets = []*FailsafeConfig{nil, {Consensus: &ConsensusPolicyConfig{}}}
		}, "connector 'c1'.failsafeForGets[1]: consensus is not supported for connector-level failsafe"},
		{"failsafeForSets matchCommitment", func(c *ConnectorConfig) {
			c.FailsafeForSets = []*FailsafeConfig{{MatchCommitment: []string{"finalized"}}}
		}, "connector 'c1'.failsafeForSets[0]: matchCommitment is not supported for connector-level failsafe"},
		{"plain failsafe accepted", func(c *ConnectorConfig) {
			c.FailsafeForGets = []*FailsafeConfig{{MatchMethod: "*"}}
		}, ""},
	})

	// Every driver's own block must be present when that driver is chosen.
	for _, tc := range []struct {
		driver  ConnectorDriverType
		wantErr string
	}{
		{DriverMemory, "database.*.connector.memory is required when driver is memory"},
		{DriverRedis, "database.*.connector.redis is required when driver is redis"},
		{DriverPostgreSQL, "database.*.connector.postgresql is required when driver is postgresql"},
		{DriverDynamoDB, "database.*.connector.dynamodb is required when driver is dynamodb"},
	} {
		t.Run(string(tc.driver)+" block required", func(t *testing.T) {
			require.ErrorContains(t, (&ConnectorConfig{Id: "x", Driver: tc.driver}).Validate(), tc.wantErr)
		})
	}
	t.Run("dynamo with memory block reports the memory exclusivity", func(t *testing.T) {
		c := cvValidConnector(DriverDynamoDB)
		c.Memory = &MemoryConnectorConfig{}
		// memory is checked first, so the memory-side message wins.
		require.ErrorContains(t, c.Validate(), "database.*.connector.memory is mutually exclusive")
	})
}

func TestCovValidation_ConnectorDynamoDB(t *testing.T) {
	cvRunCases(t, func() *ConnectorConfig { return cvValidConnector(DriverDynamoDB) }, (*ConnectorConfig).Validate, []cvCase[ConnectorConfig]{
		{"table", func(c *ConnectorConfig) { c.DynamoDB.Table = "" }, "database.*.connector.dynamodb.table is required"},
		{"partitionKeyName", func(c *ConnectorConfig) { c.DynamoDB.PartitionKeyName = "" }, "database.*.connector.dynamodb.partitionKeyName is required"},
		{"rangeKeyName", func(c *ConnectorConfig) { c.DynamoDB.RangeKeyName = "" }, "database.*.connector.dynamodb.rangeKeyName is required"},
		{"reverseIndexName", func(c *ConnectorConfig) { c.DynamoDB.ReverseIndexName = "" }, "database.*.connector.dynamodb.reverseIndexName is required"},
		{"ttlAttributeName", func(c *ConnectorConfig) { c.DynamoDB.TTLAttributeName = "" }, "database.*.connector.dynamodb.ttlAttributeName is required"},
		{"initTimeout", func(c *ConnectorConfig) { c.DynamoDB.InitTimeout = 0 }, "database.*.connector.dynamodb.initTimeout is required"},
		{"getTimeout", func(c *ConnectorConfig) { c.DynamoDB.GetTimeout = 0 }, "database.*.connector.dynamodb.getTimeout is required"},
		{"setTimeout", func(c *ConnectorConfig) { c.DynamoDB.SetTimeout = 0 }, "database.*.connector.dynamodb.setTimeout is required"},
		{"statePollInterval", func(c *ConnectorConfig) { c.DynamoDB.StatePollInterval = 0 }, "database.*.connector.dynamodb.statePollInterval is required"},
		{"with redis block", func(c *ConnectorConfig) { c.Redis = &RedisConnectorConfig{} }, "database.*.connector.redis is mutually exclusive"},
	})
}

func TestCovValidation_ConnectorPostgreSQL(t *testing.T) {
	cvRunCases(t, func() *ConnectorConfig { return cvValidConnector(DriverPostgreSQL) }, (*ConnectorConfig).Validate, []cvCase[ConnectorConfig]{
		{"connectionUri", func(c *ConnectorConfig) { c.PostgreSQL.ConnectionUri = "" }, "database.*.connector.postgresql.connectionUri is required"},
		{"table", func(c *ConnectorConfig) { c.PostgreSQL.Table = "" }, "database.*.connector.postgresql.table is required"},
		{"minConns", func(c *ConnectorConfig) { c.PostgreSQL.MinConns = 0 }, "database.*.connector.postgresql.minConns is required"},
		{"maxConns", func(c *ConnectorConfig) { c.PostgreSQL.MaxConns = 0 }, "database.*.connector.postgresql.maxConns is required"},
		{"initTimeout", func(c *ConnectorConfig) { c.PostgreSQL.InitTimeout = 0 }, "database.*.connector.postgresql.initTimeout is required"},
		{"getTimeout", func(c *ConnectorConfig) { c.PostgreSQL.GetTimeout = 0 }, "database.*.connector.postgresql.getTimeout is required"},
		{"setTimeout", func(c *ConnectorConfig) { c.PostgreSQL.SetTimeout = 0 }, "database.*.connector.postgresql.setTimeout is required"},
	})
}

func TestCovValidation_PostgreSQLIAMAuth(t *testing.T) {
	baseline := func() *PostgreSQLConnectorConfig {
		c := cvValidConnector(DriverPostgreSQL).PostgreSQL
		c.ConnectionUri = "postgres://app@db.example:5432/cache?sslmode=verify-full"
		c.IAMAuth = &PostgreSQLIAMAuthConfig{
			Enabled: true, Endpoint: "db.example:5432", DBUser: "app",
			Auth: &AwsAuthConfig{Mode: "env"},
		}
		return c
	}
	cvRunCases(t, baseline, (*PostgreSQLConnectorConfig).Validate, []cvCase[PostgreSQLConnectorConfig]{
		{"disabled IAM ignores fields", func(p *PostgreSQLConnectorConfig) { p.IAMAuth = &PostgreSQLIAMAuthConfig{} }, ""},
		{"endpoint missing", func(p *PostgreSQLConnectorConfig) { p.IAMAuth.Endpoint = "" }, "postgresql.iamAuth.endpoint could not be derived from connectionUri"},
		{"endpoint without port", func(p *PostgreSQLConnectorConfig) { p.IAMAuth.Endpoint = "db.example" }, "postgresql.iamAuth.endpoint must include a port (host:port)"},
		{"dbUser missing", func(p *PostgreSQLConnectorConfig) { p.IAMAuth.DBUser = "" }, "postgresql.iamAuth.dbUser could not be derived from connectionUri"},
		{"static password", func(p *PostgreSQLConnectorConfig) {
			p.ConnectionUri = "postgres://app:secret@db.example:5432/cache?sslmode=require"
		}, `cannot combine IAM auth with a static password in connectionUri (got password for user "app")`},
		{"dbUser mismatch", func(p *PostgreSQLConnectorConfig) { p.IAMAuth.DBUser = "other" }, `postgresql.iamAuth.dbUser "other" does not match the user in connectionUri "app"`},
		{"uri without user uses dbUser", func(p *PostgreSQLConnectorConfig) {
			p.ConnectionUri = "postgres://db.example:5432/cache?sslmode=require"
		}, ""},
		{"sslmode disable", func(p *PostgreSQLConnectorConfig) {
			p.ConnectionUri = "postgres://app@db.example:5432/cache?sslmode=disable"
		}, `sslmode must be require, verify-ca, or verify-full (got "disable"`},
		{"sslmode missing", func(p *PostgreSQLConnectorConfig) {
			p.ConnectionUri = "postgres://app@db.example:5432/cache"
		}, `(got ""`},
		{"bad auth mode", func(p *PostgreSQLConnectorConfig) { p.IAMAuth.Auth.Mode = "imds" }, `postgresql.iamAuth.auth.mode "imds" is invalid; must be file, env, or secret`},
	})
}

func TestCovValidation_ConnectorRedis(t *testing.T) {
	cvRunCases(t, func() *ConnectorConfig { return cvValidConnector(DriverRedis) }, (*ConnectorConfig).Validate, []cvCase[ConnectorConfig]{
		{"uri missing", func(c *ConnectorConfig) { c.Redis.URI = "  " }, "database.*.connector.redis.uri is required"},
		{"bad scheme", func(c *ConnectorConfig) { c.Redis.URI = "tcp://localhost:6379" }, "redis connector: invalid URI scheme, must be 'rediss://' or 'redis://'"},
		{"lockRetryInterval too small", func(c *ConnectorConfig) { c.Redis.LockRetryInterval = Duration(10 * time.Millisecond) }, "redis.lockRetryInterval should be at least 100ms"},
		{"lockRetryInterval unset", func(c *ConnectorConfig) { c.Redis.LockRetryInterval = 0 }, ""},
		{"with memory block", func(c *ConnectorConfig) { c.Memory = &MemoryConnectorConfig{} }, "database.*.connector.memory is mutually exclusive"},
	})
}

func TestCovValidation_RedisIAMAuth(t *testing.T) {
	baseline := func() *RedisConnectorConfig {
		return &RedisConnectorConfig{
			URI: "rediss://app@cache.example:6379",
			IAMAuth: &RedisIAMAuthConfig{
				Enabled: true, CacheName: "c", UserID: "app",
				Auth: &AwsAuthConfig{Mode: "secret"},
			},
		}
	}
	cvRunCases(t, baseline, (*RedisConnectorConfig).Validate, []cvCase[RedisConnectorConfig]{
		{"cacheName missing", func(r *RedisConnectorConfig) { r.IAMAuth.CacheName = "" }, "redis.iamAuth.cacheName is required when iamAuth.enabled is true"},
		{"userID missing", func(r *RedisConnectorConfig) { r.IAMAuth.UserID = "" }, "redis.iamAuth.userID is required when iamAuth.enabled is true"},
		{"plaintext scheme", func(r *RedisConnectorConfig) { r.URI = "redis://cache.example:6379" }, "redis.iamAuth requires in-transit TLS: URI must use 'rediss://' scheme"},
		{"static password", func(r *RedisConnectorConfig) { r.URI = "rediss://app:pw@cache.example:6379" }, `redis.iamAuth: cannot combine IAM auth with a static password (got password for user "app")`},
		{"bad auth mode", func(r *RedisConnectorConfig) { r.IAMAuth.Auth.Mode = "web" }, `redis.iamAuth.auth.mode "web" is invalid; must be file, env, or secret`},
		{"disabled IAM over plaintext", func(r *RedisConnectorConfig) {
			r.URI = "redis://cache.example:6379"
			r.IAMAuth.Enabled = false
		}, ""},
	})
}

func TestCovValidation_ConnectorGrpc(t *testing.T) {
	cvRunCases(t, func() *ConnectorConfig { return cvValidConnector(DriverGrpc) }, (*ConnectorConfig).Validate, []cvCase[ConnectorConfig]{
		{"negative pool", func(c *ConnectorConfig) { c.Grpc.PoolSize = -1 }, "database.*.connector.grpc.poolSize must not be negative"},
		{"pool too large", func(c *ConnectorConfig) { c.Grpc.PoolSize = MaxGrpcConnPoolSize + 1 }, "database.*.connector.grpc.poolSize must be <= 256"},
		{"health check on mounted path", func(c *ConnectorConfig) { c.Grpc.Servers = []string{"grpc://cache:50051/tenant"} }, "database.*.connector.grpc.healthCheckService cannot be used with"},
		{"unparsable server", func(c *ConnectorConfig) { c.Grpc.Servers = []string{"grpc://cache:50051/%zz"} }, "database.*.connector.grpc: invalid gRPC endpoint"},
		{"mounted path without health check", func(c *ConnectorConfig) {
			c.Grpc.Servers = []string{"grpc://cache:50051/tenant"}
			c.Grpc.HealthCheckService = ""
		}, ""},
		{"trailing slash is root", func(c *ConnectorConfig) { c.Grpc.Servers = []string{"grpc://cache:50051/"} }, ""},
	})
}
