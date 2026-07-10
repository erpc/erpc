package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"time"

	"strings"

	"github.com/bytedance/sonic"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/erpc/erpc/util"
	"github.com/grafana/sobek"
	"github.com/rs/zerolog"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v3"
)

var (
	// ErpcVersion is the version of eRPC, overridden at build time via ldflags.
	// Default: "dev" for development builds.
	// Build example: -ldflags="-X github.com/erpc/erpc/common.ErpcVersion=v1.0.0"
	ErpcVersion = "dev"

	// ErpcCommitSha is the git commit SHA, overridden at build time via ldflags.
	// Default: "none" for development builds.
	// Build example: -ldflags="-X github.com/erpc/erpc/common.ErpcCommitSha=abc123def"
	ErpcCommitSha = "none"

	TRUE  = true
	FALSE = false
)

// Config represents the configuration of the application.
type Config struct {
	LogLevel     string             `yaml:"logLevel,omitempty" json:"logLevel" tstype:"LogLevel"`
	ClusterKey   string             `yaml:"clusterKey,omitempty" json:"clusterKey"`
	Server       *ServerConfig      `yaml:"server,omitempty" json:"server"`
	HealthCheck  *HealthCheckConfig `yaml:"healthCheck,omitempty" json:"healthCheck"`
	Admin        *AdminConfig       `yaml:"admin,omitempty" json:"admin"`
	Database     *DatabaseConfig    `yaml:"database,omitempty" json:"database"`
	Projects     []*ProjectConfig   `yaml:"projects,omitempty" json:"projects"`
	RateLimiters *RateLimiterConfig `yaml:"rateLimiters,omitempty" json:"rateLimiters"`
	Metrics      *MetricsConfig     `yaml:"metrics,omitempty" json:"metrics"`
	ProxyPools   []*ProxyPoolConfig `yaml:"proxyPools,omitempty" json:"proxyPools"`
	Tracing      *TracingConfig     `yaml:"tracing,omitempty" json:"tracing"`

	// UserScript is the compiled program of the user's TS/JS config file
	// (the WHOLE thing — imports, helpers, the createConfig call). Set
	// by `loadConfigFromTypescript`; nil for YAML configs.
	//
	// Each policy-engine pool runtime runs this program once on primer
	// (via the pool's primer hook), which:
	//   * Evaluates the user's helpers and imports natively in the
	//     runtime so closures referenced by `evalFunc` actually exist.
	//   * Populates `globalThis.__erpcFns` with the runtime-native
	//     function values discovered in the default export. The
	//     SelectionPolicy's `EvalFunc` carries the lookup ID
	//     (`__ts_fn__:fn_<n>`) rather than a stringified function
	//     source — so the function never round-trips through
	//     `.toString()` + recompile.
	//
	// Never serialized; opaque to YAML/JSON.
	UserScript *sobek.Program `yaml:"-" json:"-"`
}

// LegacyTranslateFn is the post-decode migration hook invoked by
// LoadConfig before SetDefaults. nil = no migration (canonical configs
// only). cmd/erpc/main.go wires this to legacy.TranslateFromConfig
// during init; tests that exercise legacy YAML must set it explicitly.
//
// Decoupled via a package var to avoid a circular import: this package
// owns the runtime types, and common/legacy depends on those types to
// describe the legacy shape — so legacy imports common, not the other
// way around. The hook lets common stay free of the legacy package.
var LegacyTranslateFn func(*Config) ([]string, error)

// LegacyTranslateLogger lets the caller observe deprecation warnings
// emitted by LegacyTranslateFn. If nil, warnings are dropped silently.
var LegacyTranslateLogger func(warning string)

// LoadConfig loads the configuration from the specified file.
// It supports both YAML and TypeScript (.ts) files.
func LoadConfig(fs afero.Fs, filename string, opts *DefaultOptions) (*Config, error) {
	data, err := afero.ReadFile(fs, filename)
	if err != nil {
		return nil, err
	}

	var cfg Config

	if strings.HasSuffix(filename, ".ts") || strings.HasSuffix(filename, ".js") {
		cfgPtr, err := loadConfigFromTypescript(filename)
		if err != nil {
			return nil, err
		}
		cfg = *cfgPtr
	} else {
		expandedData := []byte(os.ExpandEnv(string(data)))
		decoder := yaml.NewDecoder(bytes.NewReader(expandedData))
		decoder.KnownFields(true)
		err = decoder.Decode(&cfg)
		if err != nil {
			return nil, err
		}
	}

	if LegacyTranslateFn != nil {
		warnings, err := LegacyTranslateFn(&cfg)
		if err != nil {
			return nil, fmt.Errorf("legacy config migration: %w", err)
		}
		if LegacyTranslateLogger != nil {
			for _, w := range warnings {
				LegacyTranslateLogger(w)
			}
		}
	}

	if err := cfg.SetDefaults(opts); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

type ServerConfig struct {
	ListenV4            *bool             `yaml:"listenV4,omitempty" json:"listenV4"`
	HttpHostV4          *string           `yaml:"httpHostV4,omitempty" json:"httpHostV4"`
	ListenV6            *bool             `yaml:"listenV6,omitempty" json:"listenV6"`
	HttpHostV6          *string           `yaml:"httpHostV6,omitempty" json:"httpHostV6"`
	HttpPort            *int              `yaml:"httpPort,omitempty" json:"httpPort"` // Deprecated: use HttpPortV4
	HttpPortV4          *int              `yaml:"httpPortV4,omitempty" json:"httpPortV4"`
	HttpPortV6          *int              `yaml:"httpPortV6,omitempty" json:"httpPortV6"`
	GrpcEnabled         *bool             `yaml:"grpcEnabled,omitempty" json:"grpcEnabled"`
	GrpcHostV4          *string           `yaml:"grpcHostV4,omitempty" json:"grpcHostV4"`
	GrpcPortV4          *int              `yaml:"grpcPortV4,omitempty" json:"grpcPortV4"`
	GrpcHostV6          *string           `yaml:"grpcHostV6,omitempty" json:"grpcHostV6"`
	GrpcPortV6          *int              `yaml:"grpcPortV6,omitempty" json:"grpcPortV6"`
	GrpcMaxRecvMsgSize  *int              `yaml:"grpcMaxRecvMsgSize,omitempty" json:"grpcMaxRecvMsgSize"`
	GrpcMaxSendMsgSize  *int              `yaml:"grpcMaxSendMsgSize,omitempty" json:"grpcMaxSendMsgSize"`
	GrpcReflection      *bool             `yaml:"grpcReflection,omitempty" json:"grpcReflection"`
	MaxTimeout          *Duration         `yaml:"maxTimeout,omitempty" json:"maxTimeout" tstype:"Duration"`
	ReadTimeout         *Duration         `yaml:"readTimeout,omitempty" json:"readTimeout" tstype:"Duration"`
	WriteTimeout        *Duration         `yaml:"writeTimeout,omitempty" json:"writeTimeout" tstype:"Duration"`
	EnableGzip          *bool             `yaml:"enableGzip,omitempty" json:"enableGzip"`
	TLS                 *TLSConfig        `yaml:"tls,omitempty" json:"tls"`
	Aliasing            *AliasingConfig   `yaml:"aliasing" json:"aliasing"`
	WaitBeforeShutdown  *Duration         `yaml:"waitBeforeShutdown,omitempty" json:"waitBeforeShutdown" tstype:"Duration"`
	WaitAfterShutdown   *Duration         `yaml:"waitAfterShutdown,omitempty" json:"waitAfterShutdown" tstype:"Duration"`
	IncludeErrorDetails *bool             `yaml:"includeErrorDetails,omitempty" json:"includeErrorDetails"`
	TrustedIPForwarders []string          `yaml:"trustedIPForwarders,omitempty" json:"trustedIPForwarders"`
	TrustedIPHeaders    []string          `yaml:"trustedIPHeaders,omitempty" json:"trustedIPHeaders"`
	ResponseHeaders     map[string]string `yaml:"responseHeaders,omitempty" json:"responseHeaders"`

	// ExecutionHeaders controls the per-request diagnostic headers
	// (X-ERPC-Attempts, X-ERPC-Upstreams-Tried, etc.) that expose how
	// eRPC routed and resolved each request. Defaults to "all" — set
	// "summary" to keep only counters, or "off" to disable entirely
	// (useful for low-latency / bandwidth-constrained clients).
	ExecutionHeaders *ExecutionHeadersMode `yaml:"executionHeaders,omitempty" json:"executionHeaders" tstype:"ExecutionHeadersMode"`

	// CostHeaders opts into the cost/billing response headers
	// (X-ERPC-Calls, X-ERPC-Billable, X-ERPC-Methods, X-ERPC-Credits,
	// X-ERPC-Credits-Version) on single and batch responses. Off by
	// default. Credit-unit pricing is vendor-level configuration — see
	// CreditUnitsProvider and UpstreamConfig.CreditUnits.
	CostHeaders *bool `yaml:"costHeaders,omitempty" json:"costHeaders"`
}

// ExecutionHeadersMode controls how much per-request execution detail is
// exposed in HTTP response headers.
type ExecutionHeadersMode string

const (
	// ExecutionHeadersAll emits the full set: counters + per-upstream
	// trace (upstream IDs, outcomes, reasons, durations). Default.
	ExecutionHeadersAll ExecutionHeadersMode = "all"
	// ExecutionHeadersSummary emits only the counter triplet
	// (X-ERPC-Attempts/Retries/Hedges) + the cache-hit / final-upstream
	// markers. Skips the (potentially large) per-attempt slice headers.
	ExecutionHeadersSummary ExecutionHeadersMode = "summary"
	// ExecutionHeadersOff disables all X-ERPC-* diagnostic headers.
	ExecutionHeadersOff ExecutionHeadersMode = "off"
)

type HealthCheckConfig struct {
	Mode        HealthCheckMode `yaml:"mode,omitempty" json:"mode"`
	Auth        *AuthConfig     `yaml:"auth,omitempty" json:"auth"`
	DefaultEval string          `yaml:"defaultEval,omitempty" json:"defaultEval"`
}

type HealthCheckMode string

const (
	HealthCheckModeSimple   HealthCheckMode = "simple"
	HealthCheckModeNetworks HealthCheckMode = "networks"
	HealthCheckModeVerbose  HealthCheckMode = "verbose"
)

const (
	EvalAnyInitializedUpstreams = "any:initializedUpstreams"
	EvalAnyErrorRateBelow90     = "any:errorRateBelow90"
	EvalAllErrorRateBelow90     = "all:errorRateBelow90"
	EvalAnyErrorRateBelow100    = "any:errorRateBelow100"
	EvalAllErrorRateBelow100    = "all:errorRateBelow100"
	EvalEvmAnyChainId           = "any:evm:eth_chainId"
	EvalEvmAllChainId           = "all:evm:eth_chainId"
	EvalAllActiveUpstreams      = "all:activeUpstreams"
)

type TracingProtocol string

const (
	TracingProtocolHttp TracingProtocol = "http"
	TracingProtocolGrpc TracingProtocol = "grpc"
)

type TracingConfig struct {
	Enabled            bool              `yaml:"enabled,omitempty" json:"enabled"`
	Endpoint           string            `yaml:"endpoint,omitempty" json:"endpoint"`
	Protocol           TracingProtocol   `yaml:"protocol,omitempty" json:"protocol"`
	SampleRate         float64           `yaml:"sampleRate,omitempty" json:"sampleRate"`
	Detailed           bool              `yaml:"detailed,omitempty" json:"detailed"`
	ServiceName        string            `yaml:"serviceName,omitempty" json:"serviceName"`
	Headers            map[string]string `yaml:"headers,omitempty" json:"headers"`
	TLS                *TLSConfig        `yaml:"tls,omitempty" json:"tls"`
	ResourceAttributes map[string]string `yaml:"resourceAttributes,omitempty" json:"resourceAttributes"`

	// ForceTraceMatchers defines conditions for force-tracing requests.
	// Each matcher can specify network and/or method patterns.
	// Multiple patterns can be separated by "|" (OR within field).
	// Both network and method must match if both are specified (AND between fields).
	// If only one field is specified, only that field is checked.
	ForceTraceMatchers []*ForceTraceMatcher `yaml:"forceTraceMatchers,omitempty" json:"forceTraceMatchers"`
}

// ForceTraceMatcher defines a condition for force-tracing requests.
type ForceTraceMatcher struct {
	// Network patterns to match (e.g., "evm:1", "evm:1|evm:42161", "evm:*")
	// Multiple patterns separated by "|" act as OR conditions.
	Network string `yaml:"network,omitempty" json:"network"`

	// Method patterns to match (e.g., "eth_call", "debug_*|trace_*")
	// Multiple patterns separated by "|" act as OR conditions.
	Method string `yaml:"method,omitempty" json:"method"`
}

type AdminConfig struct {
	Auth *AuthConfig `yaml:"auth" json:"auth"`
	CORS *CORSConfig `yaml:"cors" json:"cors"`
}

type AliasingConfig struct {
	Rules []*AliasingRuleConfig `yaml:"rules" json:"rules"`
}

type AliasingRuleConfig struct {
	MatchDomain       string `yaml:"matchDomain" json:"matchDomain"`
	ServeProject      string `yaml:"serveProject" json:"serveProject"`
	ServeArchitecture string `yaml:"serveArchitecture" json:"serveArchitecture"`
	ServeChain        string `yaml:"serveChain" json:"serveChain"`
}

type DatabaseConfig struct {
	EvmJsonRpcCache *CacheConfig       `yaml:"evmJsonRpcCache,omitempty" json:"evmJsonRpcCache"`
	SharedState     *SharedStateConfig `yaml:"sharedState,omitempty" json:"sharedState"`
}

type SharedStateConfig struct {
	// ClusterKey identifies the logical group for shared counters across replicas (multi-tenant friendly)
	ClusterKey string `yaml:"clusterKey,omitempty" json:"clusterKey"`
	// Connector contains the storage driver configuration (redis, postgresql, dynamodb, memory)
	Connector *ConnectorConfig `yaml:"connector,omitempty" json:"connector"`
	// FallbackTimeout is the timeout for remote storage operations (get/set/publish).
	// It is a seconds-scale network timeout and NOT a foreground latency budget.
	FallbackTimeout Duration `yaml:"fallbackTimeout,omitempty" json:"fallbackTimeout" tstype:"Duration"`
	// LockTtl is the expiration for the distributed lock key in the backing store.
	// Should comfortably exceed the expected duration of remote writes.
	LockTtl Duration `yaml:"lockTtl,omitempty" json:"lockTtl" tstype:"Duration"`
	// LockMaxWait caps how long the foreground path will wait to acquire the lock
	// before proceeding locally and deferring the remote write to background.
	LockMaxWait Duration `yaml:"lockMaxWait,omitempty" json:"lockMaxWait" tstype:"Duration"`
	// UpdateMaxWait caps how long the foreground path will spend computing a new value
	// (e.g., polling latest block) before returning the current local value.
	UpdateMaxWait Duration `yaml:"updateMaxWait,omitempty" json:"updateMaxWait" tstype:"Duration"`
}

type CacheConfig struct {
	Connectors  []*ConnectorConfig   `yaml:"connectors,omitempty" json:"connectors" tstype:"TsConnectorConfig[]"`
	Policies    []*CachePolicyConfig `yaml:"policies,omitempty" json:"policies"`
	Compression *CompressionConfig   `yaml:"compression,omitempty" json:"compression"`
}

type CompressionConfig struct {
	Enabled   *bool  `yaml:"enabled,omitempty" json:"enabled"`
	Algorithm string `yaml:"algorithm,omitempty" json:"algorithm"` // "zstd" for now, can be extended
	ZstdLevel string `yaml:"zstdLevel,omitempty" json:"zstdLevel"` // "fastest", "default", "better", "best"
	Threshold int    `yaml:"threshold,omitempty" json:"threshold"` // Minimum size in bytes to compress
}

type CacheMethodConfig struct {
	ReqRefs   [][]interface{} `yaml:"reqRefs" json:"reqRefs"`
	RespRefs  [][]interface{} `yaml:"respRefs" json:"respRefs"`
	Finalized bool            `yaml:"finalized" json:"finalized"`
	Realtime  bool            `yaml:"realtime" json:"realtime"`
	Stateful  bool            `yaml:"stateful,omitempty" json:"stateful"`
	// TranslateLatestTag controls whether the method-level tag translation should convert "latest" to a concrete hex block number.
	// When nil or true, translation is enabled by default.
	TranslateLatestTag *bool `yaml:"translateLatestTag,omitempty" json:"translateLatestTag,omitempty"`
	// TranslateFinalizedTag controls whether the method-level tag translation should convert "finalized" to a concrete hex block number.
	// When nil or true, translation is enabled by default.
	TranslateFinalizedTag *bool `yaml:"translateFinalizedTag,omitempty" json:"translateFinalizedTag,omitempty"`
	// EnforceBlockAvailability controls whether per-upstream block availability bounds (upper/lower)
	// are enforced for this method at the network level. When nil or true, enforcement is enabled.
	EnforceBlockAvailability *bool `yaml:"enforceBlockAvailability,omitempty" json:"enforceBlockAvailability,omitempty"`
}

type CachePolicyConfig struct {
	Connector   string               `yaml:"connector" json:"connector"`
	Network     string               `yaml:"network,omitempty" json:"network"`
	Method      string               `yaml:"method,omitempty" json:"method"`
	Params      []interface{}        `yaml:"params,omitempty" json:"params"`
	Finality    DataFinalityState    `yaml:"finality,omitempty" json:"finality" tstype:"DataFinalityState"`
	Empty       CacheEmptyBehavior   `yaml:"empty,omitempty" json:"empty" tstype:"CacheEmptyBehavior"`
	AppliesTo   CachePolicyAppliesTo `yaml:"appliesTo,omitempty" json:"appliesTo" tstype:"'get' | 'set' | 'both'"`
	MinItemSize *string              `yaml:"minItemSize,omitempty" json:"minItemSize" tstype:"ByteSize"`
	MaxItemSize *string              `yaml:"maxItemSize,omitempty" json:"maxItemSize" tstype:"ByteSize"`

	// TTL is either a fixed duration ("2s") or, in object form, derived from the
	// network's estimated block time ({ blockTimeMultiplier: 1, fallback: 2s }).
	// For realtime finality the resolved value is the age limit; the fixed/
	// fallback component is also used as the cache storage expiry. See
	// BlockTimeAdaptiveDuration.
	TTL *BlockTimeAdaptiveDuration `yaml:"ttl,omitempty" json:"ttl,omitempty" tstype:"Duration | BlockTimeAdaptiveDuration"`
}

type ConnectorDriverType string

const (
	DriverMemory     ConnectorDriverType = "memory"
	DriverRedis      ConnectorDriverType = "redis"
	DriverPostgreSQL ConnectorDriverType = "postgresql"
	DriverDynamoDB   ConnectorDriverType = "dynamodb"
	DriverGrpc       ConnectorDriverType = "grpc"
)

type ConnectorConfig struct {
	Id     string              `yaml:"id,omitempty" json:"id"`
	Driver ConnectorDriverType `yaml:"driver" json:"driver" tstype:"TsConnectorDriverType"`
	// Tags label the data source a connector caches, so the `use-upstream`
	// directive can gate which connector is read/written — e.g. a connector
	// fed by a system-transaction node tagged `systx` is only used when the
	// request's selector admits it. Same glob/`!negation` grammar as upstream
	// tags. Untagged connectors are always eligible (a use-upstream pin meant
	// for upstreams must not disable a normal cache).
	Tags            []string                   `yaml:"tags,omitempty" json:"tags,omitempty"`
	Memory          *MemoryConnectorConfig     `yaml:"memory,omitempty" json:"memory"`
	Redis           *RedisConnectorConfig      `yaml:"redis,omitempty" json:"redis"`
	DynamoDB        *DynamoDBConnectorConfig   `yaml:"dynamodb,omitempty" json:"dynamodb"`
	PostgreSQL      *PostgreSQLConnectorConfig `yaml:"postgresql,omitempty" json:"postgresql"`
	Grpc            *GrpcConnectorConfig       `yaml:"grpc,omitempty" json:"grpc"`
	FailsafeForGets []*FailsafeConfig          `yaml:"failsafeForGets,omitempty" json:"failsafeForGets"`
	FailsafeForSets []*FailsafeConfig          `yaml:"failsafeForSets,omitempty" json:"failsafeForSets"`
	Mock            *MockConnectorConfig       `yaml:"-" json:"-"`
}

type GrpcConnectorConfig struct {
	Bootstrap  string            `yaml:"bootstrap,omitempty" json:"bootstrap"`
	Servers    []string          `yaml:"servers,omitempty" json:"servers"`
	Headers    map[string]string `yaml:"headers,omitempty" json:"headers"`
	GetTimeout Duration          `yaml:"getTimeout,omitempty" json:"getTimeout" tstype:"Duration"`

	// PoolSize is the number of independent gRPC connections opened to each
	// backing server, selected round-robin per request. Larger values raise the
	// concurrent-stream ceiling and shrink the blast radius of a single wedged
	// connection, at the cost of more open connections per server. When unset
	// (0) a built-in default is used.
	PoolSize int `yaml:"poolSize,omitempty" json:"poolSize"`
}

type MemoryConnectorConfig struct {
	MaxItems     int    `yaml:"maxItems" json:"maxItems"`
	MaxTotalSize string `yaml:"maxTotalSize" json:"maxTotalSize"`
	EmitMetrics  *bool  `yaml:"emitMetrics,omitempty" json:"emitMetrics,omitempty"`
}

type MockConnectorConfig struct {
	MemoryConnectorConfig
	GetDelay     time.Duration
	SetDelay     time.Duration
	GetErrorRate float64
	SetErrorRate float64
}

type TLSConfig struct {
	Enabled            bool   `yaml:"enabled" json:"enabled"`
	CertFile           string `yaml:"certFile" json:"certFile"`
	KeyFile            string `yaml:"keyFile" json:"keyFile"`
	CAFile             string `yaml:"caFile,omitempty" json:"caFile"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify,omitempty" json:"insecureSkipVerify"`
}

type RedisConnectorConfig struct {
	Addr              string              `yaml:"addr,omitempty" json:"addr"`
	Username          string              `yaml:"username,omitempty" json:"username"`
	Password          string              `yaml:"password,omitempty" json:"-"`
	DB                int                 `yaml:"db,omitempty" json:"db"`
	TLS               *TLSConfig          `yaml:"tls,omitempty" json:"tls"`
	ConnPoolSize      int                 `yaml:"connPoolSize,omitempty" json:"connPoolSize"`
	URI               string              `yaml:"uri" json:"uri"`
	InitTimeout       Duration            `yaml:"initTimeout,omitempty" json:"initTimeout" tstype:"Duration"`
	GetTimeout        Duration            `yaml:"getTimeout,omitempty" json:"getTimeout" tstype:"Duration"`
	SetTimeout        Duration            `yaml:"setTimeout,omitempty" json:"setTimeout" tstype:"Duration"`
	LockRetryInterval Duration            `yaml:"lockRetryInterval,omitempty" json:"lockRetryInterval" tstype:"Duration"`
	IAMAuth           *RedisIAMAuthConfig `yaml:"iamAuth,omitempty" json:"iamAuth,omitempty"`
}

func (r *RedisConnectorConfig) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]interface{}{
		"addr":         r.Addr,
		"username":     r.Username,
		"password":     "REDACTED",
		"db":           r.DB,
		"connPoolSize": r.ConnPoolSize,
		"uri":          util.RedactEndpoint(r.URI),
		"tls":          r.TLS,
		"initTimeout":  r.InitTimeout.String(),
		"getTimeout":   r.GetTimeout.String(),
		"setTimeout":   r.SetTimeout.String(),
		"iamAuth":      r.IAMAuth,
	})
}

func (r *RedisConnectorConfig) MarshalYAML() (interface{}, error) {
	return map[string]interface{}{
		"addr":              r.Addr,
		"username":          r.Username,
		"password":          "REDACTED",
		"db":                r.DB,
		"connPoolSize":      r.ConnPoolSize,
		"uri":               util.RedactEndpoint(r.URI),
		"tls":               r.TLS,
		"initTimeout":       r.InitTimeout.String(),
		"getTimeout":        r.GetTimeout.String(),
		"setTimeout":        r.SetTimeout.String(),
		"lockRetryInterval": r.LockRetryInterval.String(),
		"iamAuth":           r.IAMAuth,
	}, nil
}

type DynamoDBConnectorConfig struct {
	Table             string         `yaml:"table,omitempty" json:"table"`
	Region            string         `yaml:"region,omitempty" json:"region"`
	Endpoint          string         `yaml:"endpoint,omitempty" json:"endpoint"`
	Auth              *AwsAuthConfig `yaml:"auth,omitempty" json:"auth"`
	PartitionKeyName  string         `yaml:"partitionKeyName,omitempty" json:"partitionKeyName"`
	RangeKeyName      string         `yaml:"rangeKeyName,omitempty" json:"rangeKeyName"`
	ReverseIndexName  string         `yaml:"reverseIndexName,omitempty" json:"reverseIndexName"`
	TTLAttributeName  string         `yaml:"ttlAttributeName,omitempty" json:"ttlAttributeName"`
	InitTimeout       Duration       `yaml:"initTimeout,omitempty" json:"initTimeout" tstype:"Duration"`
	GetTimeout        Duration       `yaml:"getTimeout,omitempty" json:"getTimeout" tstype:"Duration"`
	SetTimeout        Duration       `yaml:"setTimeout,omitempty" json:"setTimeout" tstype:"Duration"`
	MaxRetries        int            `yaml:"maxRetries,omitempty" json:"maxRetries"`
	StatePollInterval Duration       `yaml:"statePollInterval,omitempty" json:"statePollInterval" tstype:"Duration"`
	LockRetryInterval Duration       `yaml:"lockRetryInterval,omitempty" json:"lockRetryInterval" tstype:"Duration"`
}

type PostgreSQLConnectorConfig struct {
	ConnectionUri string                   `yaml:"connectionUri" json:"connectionUri"`
	Table         string                   `yaml:"table" json:"table"`
	MinConns      int32                    `yaml:"minConns,omitempty" json:"minConns"`
	MaxConns      int32                    `yaml:"maxConns,omitempty" json:"maxConns"`
	InitTimeout   Duration                 `yaml:"initTimeout,omitempty" json:"initTimeout" tstype:"Duration"`
	GetTimeout    Duration                 `yaml:"getTimeout,omitempty" json:"getTimeout" tstype:"Duration"`
	SetTimeout    Duration                 `yaml:"setTimeout,omitempty" json:"setTimeout" tstype:"Duration"`
	IAMAuth       *PostgreSQLIAMAuthConfig `yaml:"iamAuth,omitempty" json:"iamAuth,omitempty"`
	// SkipSchemaSetup skips all startup DDL (CREATE TABLE/INDEX, column
	// migrations, pg_cron) and the local expired-row cleanup DELETE loop. Set
	// it for connectors whose ConnectionUri targets a read-only replica (e.g.
	// an Aurora global-database secondary): DDL cannot execute there (SQLSTATE
	// 25006) and is not write-forwarded, so the writer-region connector owns
	// the schema and the replica receives it via storage replication.
	SkipSchemaSetup bool `yaml:"skipSchemaSetup,omitempty" json:"skipSchemaSetup"`
}

func (p *PostgreSQLConnectorConfig) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]interface{}{
		"connectionUri":   util.RedactEndpoint(p.ConnectionUri),
		"table":           p.Table,
		"minConns":        fmt.Sprintf("%d", p.MinConns),
		"maxConns":        fmt.Sprintf("%d", p.MaxConns),
		"initTimeout":     p.InitTimeout.String(),
		"getTimeout":      p.GetTimeout.String(),
		"setTimeout":      p.SetTimeout.String(),
		"iamAuth":         p.IAMAuth,
		"skipSchemaSetup": p.SkipSchemaSetup,
	})
}

func (p *PostgreSQLConnectorConfig) MarshalYAML() (interface{}, error) {
	return map[string]interface{}{
		"connectionUri":   util.RedactEndpoint(p.ConnectionUri),
		"table":           p.Table,
		"minConns":        p.MinConns,
		"maxConns":        p.MaxConns,
		"initTimeout":     p.InitTimeout.String(),
		"getTimeout":      p.GetTimeout.String(),
		"setTimeout":      p.SetTimeout.String(),
		"iamAuth":         p.IAMAuth,
		"skipSchemaSetup": p.SkipSchemaSetup,
	}, nil
}

type AwsAuthConfig struct {
	Mode            string `yaml:"mode" json:"mode" tstype:"'file' | 'env' | 'secret'"` // "file", "env", "secret"
	CredentialsFile string `yaml:"credentialsFile" json:"credentialsFile"`
	Profile         string `yaml:"profile" json:"profile"`
	AccessKeyID     string `yaml:"accessKeyID" json:"accessKeyID"`
	SecretAccessKey string `yaml:"secretAccessKey" json:"secretAccessKey"`
}

func (a *AwsAuthConfig) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]interface{}{
		"mode":            a.Mode,
		"credentialsFile": a.CredentialsFile,
		"profile":         a.Profile,
		"accessKeyID":     a.AccessKeyID,
		"secretAccessKey": "REDACTED",
	})
}

func (a *AwsAuthConfig) MarshalYAML() (interface{}, error) {
	return map[string]interface{}{
		"mode":            a.Mode,
		"credentialsFile": a.CredentialsFile,
		"profile":         a.Profile,
		"accessKeyID":     a.AccessKeyID,
		"secretAccessKey": "REDACTED",
	}, nil
}

// RedisIAMAuthConfig enables AWS IAM authentication for ElastiCache (Valkey ≥7.2
// or Redis OSS ≥7.0). When enabled, eRPC mints SigV4-presigned auth tokens via
// go-redis's CredentialsProviderContext on every new connection. TLS is required
// (auto-enabled by SetDefaults). For IAM-enabled ElastiCache users, the user
// name and user ID must be identical — supply that single value as UserID.
type RedisIAMAuthConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// CacheName is the ElastiCache replication-group ID.
	// Will be lowercased automatically (AWS lowercases cache names at creation time).
	CacheName string `yaml:"cacheName" json:"cacheName"`
	// Region is optional — derived from AWS_REGION / instance metadata when omitted.
	Region string `yaml:"region,omitempty" json:"region,omitempty"`
	UserID string `yaml:"userID" json:"userID"`
	// Auth selects the AWS credential source (same shape as DynamoDB's auth).
	// Omit to use the default credential chain (instance role, env vars, …).
	Auth *AwsAuthConfig `yaml:"auth,omitempty" json:"auth,omitempty"`
}

// PostgreSQLIAMAuthConfig enables AWS IAM authentication for RDS PostgreSQL.
// eRPC mints SigV4-presigned tokens via pgxpool.BeforeConnect on each new pool
// connection. SSL is required (auto-enforced via sslmode=require by SetDefaults).
type PostgreSQLIAMAuthConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Endpoint is host:port of the RDS instance. If empty, derived from
	// ConnectionUri at SetDefaults time.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	// Region is optional — derived from AWS_REGION / instance metadata when omitted.
	Region string `yaml:"region,omitempty" json:"region,omitempty"`
	// DBUser is the database user mapped to the IAM role (must be granted rds_iam
	// in PostgreSQL). If empty, derived from the user in ConnectionUri.
	DBUser string         `yaml:"dbUser,omitempty" json:"dbUser,omitempty"`
	Auth   *AwsAuthConfig `yaml:"auth,omitempty" json:"auth,omitempty"`
}

type ProjectConfig struct {
	Id               string            `yaml:"id" json:"id"`
	Auth             *AuthConfig       `yaml:"auth,omitempty" json:"auth"`
	CORS             *CORSConfig       `yaml:"cors,omitempty" json:"cors"`
	Providers        []*ProviderConfig `yaml:"providers,omitempty" json:"providers"`
	UpstreamDefaults *UpstreamConfig   `yaml:"upstreamDefaults,omitempty" json:"upstreamDefaults"`
	Upstreams        []*UpstreamConfig `yaml:"upstreams,omitempty" json:"upstreams"`
	NetworkDefaults  *NetworkDefaults  `yaml:"networkDefaults,omitempty" json:"networkDefaults"`
	Networks         []*NetworkConfig  `yaml:"networks,omitempty" json:"networks"`
	RateLimitBudget  string            `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget"`
	// Configure user agent tracking at the project level
	UserAgentMode         UserAgentTrackingMode `yaml:"userAgentMode,omitempty" json:"userAgentMode"`
	ForwardHeaders        []string              `yaml:"forwardHeaders,omitempty" json:"forwardHeaders"`
	AllowClientDirectives *string               `yaml:"allowClientDirectives,omitempty" json:"allowClientDirectives"`
	IgnoreMethods         []string              `yaml:"ignoreMethods,omitempty" json:"ignoreMethods"`
	AllowMethods          []string              `yaml:"allowMethods,omitempty" json:"allowMethods"`

	// ScoreMetricsWindowSize is the tumbling window the per-upstream
	// health tracker uses for its rolling counters (errorRate, p50/p70/
	// p95 latency, throttledRate, misbehaviorRate). At each tick the
	// counters reset and start re-accumulating, so this knob effectively
	// controls how fast a degraded upstream's metrics start reflecting
	// the new reality. Short windows (e.g. 30s) react quickly but give
	// noisier ranking; long windows (5–10m) give stable averages but
	// hide spikes for longer. Defaults to 10m when zero — production
	// systems typically leave it default; the eRPC simulator overrides
	// to 30s so knob changes show up in the UI within seconds.
	ScoreMetricsWindowSize Duration `yaml:"scoreMetricsWindowSize,omitempty" json:"scoreMetricsWindowSize"`

	// LegacyProject captures deprecated project-level scoring / routing
	// keys that used to live directly on ProjectConfig. Consumed by the
	// legacy translator hook in LoadConfig, then cleared. Never serialized.
	LegacyProject *LegacyProjectFields `yaml:"-" json:"-"`
}

// LegacyProjectFields collects the deprecated project-level scoring +
// routing keys. The translator inspects these to synthesize a
// `selectionPolicy.eval` for each network and to emit deprecation
// warnings. All fields are zero-valued for a clean modern config.
//
// `scoreMetricsWindowSize` was previously listed here as inert; it is
// now a first-class field on ProjectConfig (above) — operators
// control the health tracker window directly.
type LegacyProjectFields struct {
	RoutingStrategy        string   `yaml:"routingStrategy,omitempty"`
	ScoreGranularity       string   `yaml:"scoreGranularity,omitempty"`
	ScorePenaltyDecayRate  float64  `yaml:"scorePenaltyDecayRate,omitempty"`
	ScoreSwitchHysteresis  float64  `yaml:"scoreSwitchHysteresis,omitempty"`
	ScoreMinSwitchInterval Duration `yaml:"scoreMinSwitchInterval,omitempty"`
	ScoreMetricsMode       string   `yaml:"scoreMetricsMode,omitempty"`
	ScoreRefreshInterval   Duration `yaml:"scoreRefreshInterval,omitempty"`
}

// UnmarshalYAML captures legacy project-level scoring / routing keys
// alongside the canonical schema. The legacy fields are stashed in
// LegacyProject; the load-time translator hook reads them and
// synthesizes the equivalent selectionPolicy.eval per network.
func (p *ProjectConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type canonicalProjectConfig ProjectConfig
	type shadow struct {
		canonicalProjectConfig `yaml:",inline"`
		LegacyProjectFields    `yaml:",inline"`
	}
	var s shadow
	if err := unmarshal(&s); err != nil {
		return err
	}
	*p = ProjectConfig(s.canonicalProjectConfig)
	if s.LegacyProjectFields != (LegacyProjectFields{}) {
		lf := s.LegacyProjectFields
		p.LegacyProject = &lf
	}
	return nil
}

// UserAgentTrackingMode controls how user agents are recorded for metrics/labels
type UserAgentTrackingMode string

const (
	// UserAgentTrackingModeSimplified lowers cardinality by bucketing common user agents
	UserAgentTrackingModeSimplified UserAgentTrackingMode = "simplified"
	// UserAgentTrackingModeRaw records the user agent string as-is (high cardinality)
	UserAgentTrackingModeRaw UserAgentTrackingMode = "raw"
)

// Removed legacy nested UserAgentConfig; use ProjectConfig.UserAgentMode

type NetworkDefaults struct {
	RateLimitBudget   string                   `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget"`
	Failsafe          []*FailsafeConfig        `yaml:"failsafe,omitempty" json:"failsafe"`
	SelectionPolicy   *SelectionPolicyConfig   `yaml:"selectionPolicy,omitempty" json:"selectionPolicy"`
	DirectiveDefaults *DirectiveDefaultsConfig `yaml:"directiveDefaults,omitempty" json:"directiveDefaults"`
	Evm               *EvmNetworkConfig        `yaml:"evm,omitempty" json:"evm" tstype:"TsEvmNetworkConfigForDefaults"`
	Multiplexing      *bool                    `yaml:"multiplexing,omitempty" json:"multiplexing"`
}

// UnmarshalYAML provides backward compatibility for old single failsafe object format
func (n *NetworkDefaults) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Define a type alias to avoid recursion
	type rawNetworkDefaults NetworkDefaults
	raw := (*rawNetworkDefaults)(n)

	// Try unmarshaling normally first
	err := unmarshal(raw)
	if err == nil {
		return nil
	}

	// Save the original error - it might be more informative
	originalErr := err

	// Check if the error is about unknown fields - if so, return it as is
	// This preserves errors like "field maxCount not found in type"
	errStr := err.Error()
	if strings.Contains(errStr, "not found in type") ||
		strings.Contains(errStr, "unknown field") {
		return originalErr
	}

	// If that fails, try the old format with single failsafe object
	type oldNetworkDefaults struct {
		RateLimitBudget   string                   `yaml:"rateLimitBudget,omitempty"`
		Failsafe          *FailsafeConfig          `yaml:"failsafe,omitempty"`
		SelectionPolicy   *SelectionPolicyConfig   `yaml:"selectionPolicy,omitempty"`
		DirectiveDefaults *DirectiveDefaultsConfig `yaml:"directiveDefaults,omitempty"`
		Evm               *EvmNetworkConfig        `yaml:"evm,omitempty"`
	}

	var old oldNetworkDefaults
	if err := unmarshal(&old); err != nil {
		// If both formats fail, return the original error as it's likely more informative
		// about the actual problem (like invalid field names)
		return originalErr
	}

	// Convert old format to new format
	n.RateLimitBudget = old.RateLimitBudget
	n.SelectionPolicy = old.SelectionPolicy
	n.DirectiveDefaults = old.DirectiveDefaults
	n.Evm = old.Evm

	if old.Failsafe != nil {
		// Ensure MatchMethod has a default value for backward compatibility
		if old.Failsafe.MatchMethod == "" {
			old.Failsafe.MatchMethod = "*"
		}
		n.Failsafe = []*FailsafeConfig{old.Failsafe}
	}

	return nil
}

type CORSConfig struct {
	AllowedOrigins   []string `yaml:"allowedOrigins" json:"allowedOrigins"`
	AllowedMethods   []string `yaml:"allowedMethods" json:"allowedMethods"`
	AllowedHeaders   []string `yaml:"allowedHeaders" json:"allowedHeaders"`
	ExposedHeaders   []string `yaml:"exposedHeaders" json:"exposedHeaders"`
	AllowCredentials *bool    `yaml:"allowCredentials" json:"allowCredentials"`
	MaxAge           int      `yaml:"maxAge" json:"maxAge"`
}

type VendorSettings map[string]interface{}

// CreditUnits extracts the `creditUnits` override dictionary from vendor
// settings (`providers[].settings.creditUnits`): JSON-RPC method → credit
// units, with "*" as the vendor's fallback for unlisted methods. YAML
// decodes numbers as int or float64 — both normalize to int64. Nil when
// absent or empty.
func (s VendorSettings) CreditUnits() map[string]int64 {
	raw, ok := s["creditUnits"].(map[string]interface{})
	if !ok {
		return nil
	}
	out := make(map[string]int64, len(raw))
	for method, v := range raw {
		switch n := v.(type) {
		case int:
			out[method] = int64(n)
		case int64:
			out[method] = n
		case float64:
			out[method] = int64(n)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type ProviderConfig struct {
	Id                 string                     `yaml:"id,omitempty" json:"id"`
	Vendor             string                     `yaml:"vendor" json:"vendor"`
	Settings           VendorSettings             `yaml:"settings,omitempty" json:"settings"`
	OnlyNetworks       []string                   `yaml:"onlyNetworks,omitempty" json:"onlyNetworks"`
	IgnoreNetworks     []string                   `yaml:"ignoreNetworks,omitempty" json:"ignoreNetworks"`
	UpstreamIdTemplate string                     `yaml:"upstreamIdTemplate,omitempty" json:"upstreamIdTemplate"`
	Overrides          map[string]*UpstreamConfig `yaml:"overrides,omitempty" json:"overrides"`
}

func (p *ProviderConfig) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]interface{}{
		"id":                 p.Id,
		"vendor":             p.Vendor,
		"settings":           "REDACTED",
		"onlyNetworks":       p.OnlyNetworks,
		"upstreamIdTemplate": p.UpstreamIdTemplate,
		"overrides":          p.Overrides,
	})
}

func (p *ProviderConfig) MarshalYAML() (interface{}, error) {
	return map[string]interface{}{
		"id":                 p.Id,
		"vendor":             p.Vendor,
		"settings":           "REDACTED",
		"onlyNetworks":       p.OnlyNetworks,
		"ignoreNetworks":     p.IgnoreNetworks,
		"upstreamIdTemplate": p.UpstreamIdTemplate,
		"overrides":          p.Overrides,
	}, nil
}

type UpstreamConfig struct {
	Id   string       `yaml:"id,omitempty" json:"id"`
	Type UpstreamType `yaml:"type,omitempty" json:"type" tstype:"TsUpstreamType"`
	// Tags is the single canonical user-applied label set. Convention is
	// `<dimension>:<value>` so a single upstream can carry orthogonal labels:
	//
	//   tags:
	//     - tier:main               # used by .preferTag for tiering
	//     - region:us-east          # used by .spreadAcrossTags('region:')
	//     - sequencer:op-base       # shared-fate label
	//
	// Bare strings (no prefix) work too. Patterns supported by every
	// stdlib tag method: glob (`*`, `?`) and `!negation`.
	//
	// Tier convention: `tier:fallback` declares a fallback-tier upstream
	// (matches the long-standing eRPC convention; the default policy
	// looks for `!tier:fallback` as the primary tier).
	//
	// The legacy `group: X` / `cohort: Y` YAML keys are accepted at
	// load time only — UnmarshalYAML rewrites them as `tier:X` /
	// `cohort:Y` tags and forgets them. There is no Go-level Group or
	// Cohort field; programmatic code uses Tags directly.
	Tags []string `yaml:"tags,omitempty" json:"tags,omitempty"`

	VendorName                   string                   `yaml:"vendorName,omitempty" json:"vendorName"`
	Endpoint                     string                   `yaml:"endpoint,omitempty" json:"endpoint"`
	Evm                          *EvmUpstreamConfig       `yaml:"evm,omitempty" json:"evm"`
	JsonRpc                      *JsonRpcUpstreamConfig   `yaml:"jsonRpc,omitempty" json:"jsonRpc"`
	Grpc                         *GrpcUpstreamConfig      `yaml:"grpc,omitempty" json:"grpc"`
	IgnoreMethods                []string                 `yaml:"ignoreMethods,omitempty" json:"ignoreMethods"`
	AllowMethods                 []string                 `yaml:"allowMethods,omitempty" json:"allowMethods"`
	AutoIgnoreUnsupportedMethods *bool                    `yaml:"autoIgnoreUnsupportedMethods,omitempty" json:"autoIgnoreUnsupportedMethods"`
	Failsafe                     []*FailsafeConfig        `yaml:"failsafe,omitempty" json:"failsafe"`
	RateLimitBudget              string                   `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget"`
	RateLimitAutoTune            *RateLimitAutoTuneConfig `yaml:"rateLimitAutoTune,omitempty" json:"rateLimitAutoTune"`
	// CreditUnits overrides the vendor's built-in per-method credit table
	// (CreditUnitsProvider) for this upstream, merged per method over the
	// vendor defaults ("*" = fallback for unlisted methods). Normally set
	// once per provider via `providers[].settings.creditUnits`, which is
	// copied onto every upstream the provider generates.
	CreditUnits map[string]int64      `yaml:"creditUnits,omitempty" json:"creditUnits,omitempty"`
	Shadow      *ShadowUpstreamConfig `yaml:"shadow,omitempty" json:"shadow"`

	// Routing holds per-upstream routing hints consumed by the selection
	// policy. `scoreMultipliers` bias this upstream's rank inside
	// `sortByScore` (see SelectionPolicyConfig): the engine resolves the
	// matching entry for each (network, method, finality) tick and exposes
	// the resulting weight map to the eval function as `u.scoreMultipliers`.
	// When the upstream omits its own `routing` block, `ApplyDefaults`
	// inherits the project-level `upstreamDefaults.routing` (all-or-nothing,
	// matching the Tags inheritance pattern).
	Routing *UpstreamRoutingConfig `yaml:"routing,omitempty" json:"routing,omitempty"`
}

// UpstreamRoutingConfig holds per-upstream routing hints. Today this is
// the home of `scoreMultipliers` (per-upstream weight overrides folded
// into `sortByScore`) and `probe` (per-upstream opt-out for the
// selection policy's `probeExcluded` shadow-mirror traffic).
type UpstreamRoutingConfig struct {
	// ScoreMultipliers biases this upstream's rank. Each entry is a
	// matcher (network/method/finality) plus weight overrides; the engine
	// resolves the first matching entry per tick and hands it to the eval
	// function as `u.scoreMultipliers`. `sortByScore` merges it over the
	// base weights by default (see its `multipliers` option).
	ScoreMultipliers []*ScoreMultiplierConfig `yaml:"scoreMultipliers,omitempty" json:"scoreMultipliers,omitempty"`
	// ScoreLatencyQuantile selects which response-time quantile feeds the
	// score (e.g. 0.9 → p90). When unset, the policy's own
	// `sortByScore({ latencyQuantile })` (default p70) applies.
	ScoreLatencyQuantile float64 `yaml:"scoreLatencyQuantile,omitempty" json:"scoreLatencyQuantile,omitempty"`
	// Probe gates whether the selection policy's `probeExcluded` step
	// may shadow-mirror real requests to THIS upstream when it's
	// currently in the excluded set. `"on"` (default) opts in; `"off"`
	// disables probing entirely — the upstream stays excluded
	// permanently once predicates trip until an operator intervenes
	// (manual cordon/uncordon, or until the predicate stops matching
	// via state-poller-driven structural metrics like head lag). Use
	// `"off"` for pay-per-call vendors where shadow traffic eats quota.
	Probe ProbeMode `yaml:"probe,omitempty" json:"probe,omitempty" tstype:"ProbeMode | \"on\" | \"off\""`
}

// ProbeMode is the per-upstream `routing.probe` enum.
type ProbeMode string

const (
	// ProbeModeOn — default. The selection policy may mirror sampled
	// real requests to this upstream while it's excluded so it
	// accumulates fresh tracker samples for natural re-admission.
	ProbeModeOn ProbeMode = "on"
	// ProbeModeOff — never mirror. Upstream stays excluded after
	// predicates trip; only structural signals (head lag, etc.) can
	// drive re-admission.
	ProbeModeOff ProbeMode = "off"
)

// ScoreMultiplierConfig is one per-upstream weight override. The matcher
// fields (network/method/finality) scope the entry — leave them empty (or
// `"*"`) for an entry that applies to every request. Weight fields are
// pointers so "unset" is distinct from "zero": an unset weight inherits
// from the policy's base weights (merge mode), a zero weight removes that
// metric's contribution. `overall` scales the upstream's FINAL score — a
// preference dial where >1 prefers this upstream and <1 avoids it.
type ScoreMultiplierConfig struct {
	Network         string              `yaml:"network,omitempty" json:"network,omitempty"`
	Method          string              `yaml:"method,omitempty" json:"method,omitempty"`
	Finality        []DataFinalityState `yaml:"finality,omitempty" json:"finality,omitempty" tstype:"DataFinalityState[]"`
	Overall         *float64            `yaml:"overall,omitempty" json:"overall,omitempty"`
	ErrorRate       *float64            `yaml:"errorRate,omitempty" json:"errorRate,omitempty"`
	RespLatency     *float64            `yaml:"respLatency,omitempty" json:"respLatency,omitempty"`
	ThrottledRate   *float64            `yaml:"throttledRate,omitempty" json:"throttledRate,omitempty"`
	BlockHeadLag    *float64            `yaml:"blockHeadLag,omitempty" json:"blockHeadLag,omitempty"`
	FinalizationLag *float64            `yaml:"finalizationLag,omitempty" json:"finalizationLag,omitempty"`
	Misbehaviors    *float64            `yaml:"misbehaviors,omitempty" json:"misbehaviors,omitempty"`
	// TotalRequests is accepted for backward compatibility but no longer
	// influences scoring (the score is computed from rolling-window rates,
	// not absolute request counts).
	TotalRequests *float64 `yaml:"totalRequests,omitempty" json:"totalRequests,omitempty"`
}

// UnmarshalYAML accepts the current canonical schema, plus three legacy
// shapes for backward compatibility at load time only:
//
//  1. `failsafe:` as a single object (pre-list shape).
//  2. `group:` / `cohort:` keys at the top level — rewritten as
//     `tier:<value>` / `cohort:<value>` entries in `tags`.
//
// The `routing:` block (`scoreMultipliers`, `scoreLatencyQuantile`) is a
// first-class field (`u.Routing`) — it parses straight through the
// canonical schema and survives to runtime; the `group`/`cohort` keys are
// the only load-time-only rewrites left here.
func (u *UpstreamConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Strict-schema shadow: inlines the canonical config and adds the
	// deprecated yaml-only keys. yaml v3 ignores `json:"-"` so this is safe.
	type canonicalUpstreamConfig UpstreamConfig
	type shadow struct {
		canonicalUpstreamConfig `yaml:",inline"`
		Group                   string `yaml:"group,omitempty"`
		Cohort                  string `yaml:"cohort,omitempty"`
	}

	var s shadow
	err := unmarshal(&s)
	if err == nil {
		*u = UpstreamConfig(s.canonicalUpstreamConfig)
		mergeLegacyLabelKeysIntoTags(u, s.Group, s.Cohort)
		return nil
	}

	originalErr := err
	errStr := err.Error()
	// Unknown-field errors are real schema bugs; surface them as-is.
	if strings.Contains(errStr, "not found in type") ||
		strings.Contains(errStr, "unknown field") {
		return originalErr
	}

	// Legacy shape: `failsafe:` as a single object instead of a list.
	type oldShadow struct {
		Id                           string                   `yaml:"id,omitempty"`
		Type                         UpstreamType             `yaml:"type,omitempty"`
		Tags                         []string                 `yaml:"tags,omitempty"`
		Group                        string                   `yaml:"group,omitempty"`
		Cohort                       string                   `yaml:"cohort,omitempty"`
		Routing                      *UpstreamRoutingConfig   `yaml:"routing,omitempty"`
		VendorName                   string                   `yaml:"vendorName,omitempty"`
		Endpoint                     string                   `yaml:"endpoint,omitempty"`
		Evm                          *EvmUpstreamConfig       `yaml:"evm,omitempty"`
		JsonRpc                      *JsonRpcUpstreamConfig   `yaml:"jsonRpc,omitempty"`
		Grpc                         *GrpcUpstreamConfig      `yaml:"grpc,omitempty"`
		IgnoreMethods                []string                 `yaml:"ignoreMethods,omitempty"`
		AllowMethods                 []string                 `yaml:"allowMethods,omitempty"`
		AutoIgnoreUnsupportedMethods *bool                    `yaml:"autoIgnoreUnsupportedMethods,omitempty"`
		Failsafe                     *FailsafeConfig          `yaml:"failsafe,omitempty"`
		RateLimitBudget              string                   `yaml:"rateLimitBudget,omitempty"`
		RateLimitAutoTune            *RateLimitAutoTuneConfig `yaml:"rateLimitAutoTune,omitempty"`
		Shadow                       *ShadowUpstreamConfig    `yaml:"shadow,omitempty"`
	}

	var old oldShadow
	if err := unmarshal(&old); err != nil {
		return originalErr
	}

	u.Id = old.Id
	u.Type = old.Type
	u.Tags = old.Tags
	u.VendorName = old.VendorName
	u.Endpoint = old.Endpoint
	u.Evm = old.Evm
	u.JsonRpc = old.JsonRpc
	u.Grpc = old.Grpc
	u.IgnoreMethods = old.IgnoreMethods
	u.AllowMethods = old.AllowMethods
	u.AutoIgnoreUnsupportedMethods = old.AutoIgnoreUnsupportedMethods
	u.RateLimitBudget = old.RateLimitBudget
	u.RateLimitAutoTune = old.RateLimitAutoTune
	u.Shadow = old.Shadow

	if old.Failsafe != nil {
		if old.Failsafe.MatchMethod == "" {
			old.Failsafe.MatchMethod = "*"
		}
		u.Failsafe = []*FailsafeConfig{old.Failsafe}
	}

	mergeLegacyLabelKeysIntoTags(u, old.Group, old.Cohort)
	if old.Routing != nil {
		u.Routing = old.Routing
	}
	return nil
}

// HasTag returns true if `u.Tags` contains the given exact tag. For
// glob / prefix matching, do it at the call site or in JS via `byTag`.
func (u *UpstreamConfig) HasTag(tag string) bool {
	for _, t := range u.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// mergeLegacyLabelKeysIntoTags appends `tier:<group>` / `cohort:<cohort>`
// to u.Tags for the deprecated yaml-only keys, deduplicating against
// what's already there. Called once from UnmarshalYAML; not exported.
func mergeLegacyLabelKeysIntoTags(u *UpstreamConfig, group, cohort string) {
	has := func(tag string) bool {
		for _, t := range u.Tags {
			if t == tag {
				return true
			}
		}
		return false
	}
	if group != "" {
		if t := "tier:" + group; !has(t) {
			u.Tags = append(u.Tags, t)
		}
	}
	if cohort != "" {
		if t := "cohort:" + cohort; !has(t) {
			u.Tags = append(u.Tags, t)
		}
	}
}

func (c *UpstreamConfig) Copy() *UpstreamConfig {
	if c == nil {
		return nil
	}

	copied := &UpstreamConfig{}
	*copied = *c

	if c.Evm != nil {
		copied.Evm = c.Evm.Copy()
	}
	if c.Failsafe != nil {
		copied.Failsafe = make([]*FailsafeConfig, len(c.Failsafe))
		for i, failsafe := range c.Failsafe {
			copied.Failsafe[i] = failsafe.Copy()
		}
	}
	if c.JsonRpc != nil {
		copied.JsonRpc = c.JsonRpc.Copy()
	}
	if c.Grpc != nil {
		copied.Grpc = c.Grpc.Copy()
	}
	if c.RateLimitAutoTune != nil {
		copied.RateLimitAutoTune = c.RateLimitAutoTune.Copy()
	}

	if c.IgnoreMethods != nil {
		copied.IgnoreMethods = make([]string, len(c.IgnoreMethods))
		copy(copied.IgnoreMethods, c.IgnoreMethods)
	}

	if c.AllowMethods != nil {
		copied.AllowMethods = make([]string, len(c.AllowMethods))
		copy(copied.AllowMethods, c.AllowMethods)
	}

	return copied
}

type ShadowUpstreamConfig struct {
	Enabled      bool                `yaml:"enabled" json:"enabled"`
	SampleRate   *float64            `yaml:"sampleRate,omitempty" json:"sampleRate,omitempty"`
	IgnoreFields map[string][]string `yaml:"ignoreFields,omitempty" json:"ignoreFields"`
}

type UpstreamIntegrityConfig struct {
	EthGetBlockReceipts *UpstreamIntegrityEthGetBlockReceiptsConfig `yaml:"eth_getBlockReceipts,omitempty" json:"eth_getBlockReceipts"`
}

func (c *UpstreamIntegrityConfig) Copy() *UpstreamIntegrityConfig {
	if c == nil {
		return nil
	}

	copied := &UpstreamIntegrityConfig{}

	if c.EthGetBlockReceipts != nil {
		copied.EthGetBlockReceipts = c.EthGetBlockReceipts.Copy()
	}

	return copied
}

type UpstreamIntegrityEthGetBlockReceiptsConfig struct {
	Enabled                       bool  `yaml:"enabled,omitempty" json:"enabled"`
	CheckLogIndexStrictIncrements *bool `yaml:"checkLogIndexStrictIncrements,omitempty" json:"checkLogIndexStrictIncrements"`
	CheckLogsBloom                *bool `yaml:"checkLogsBloom,omitempty" json:"checkLogsBloom"`
}

func (c *UpstreamIntegrityEthGetBlockReceiptsConfig) Copy() *UpstreamIntegrityEthGetBlockReceiptsConfig {
	if c == nil {
		return nil
	}

	copyCfg := &UpstreamIntegrityEthGetBlockReceiptsConfig{
		Enabled: c.Enabled,
	}

	if c.CheckLogIndexStrictIncrements != nil {
		val := *c.CheckLogIndexStrictIncrements
		copyCfg.CheckLogIndexStrictIncrements = &val
	}
	if c.CheckLogsBloom != nil {
		val := *c.CheckLogsBloom
		copyCfg.CheckLogsBloom = &val
	}

	return copyCfg
}

func (u *UpstreamConfig) MarshalJSON() ([]byte, error) {
	type UJAlias UpstreamConfig
	return sonic.Marshal(&struct {
		Endpoint string `json:"endpoint"`
		*UJAlias
	}{
		Endpoint: util.RedactEndpoint(u.Endpoint),
		UJAlias:  (*UJAlias)(u),
	})
}

func (u *UpstreamConfig) MarshalYAML() (interface{}, error) {
	type UYAlias UpstreamConfig
	cp := *u
	cp.Endpoint = util.RedactEndpoint(u.Endpoint)
	return (*UYAlias)(&cp), nil
}

type RateLimitAutoTuneConfig struct {
	Enabled            *bool    `yaml:"enabled" json:"enabled"`
	AdjustmentPeriod   Duration `yaml:"adjustmentPeriod" json:"adjustmentPeriod" tstype:"Duration"`
	ErrorRateThreshold float64  `yaml:"errorRateThreshold" json:"errorRateThreshold"`
	IncreaseFactor     float64  `yaml:"increaseFactor" json:"increaseFactor"`
	DecreaseFactor     float64  `yaml:"decreaseFactor" json:"decreaseFactor"`
	MinBudget          int      `yaml:"minBudget" json:"minBudget"`
	MaxBudget          int      `yaml:"maxBudget" json:"maxBudget"`
}

func (c *RateLimitAutoTuneConfig) Copy() *RateLimitAutoTuneConfig {
	if c == nil {
		return nil
	}

	copied := &RateLimitAutoTuneConfig{}
	*copied = *c

	return copied
}

type JsonRpcUpstreamConfig struct {
	SupportsBatch *bool             `yaml:"supportsBatch,omitempty" json:"supportsBatch"`
	BatchMaxSize  int               `yaml:"batchMaxSize,omitempty" json:"batchMaxSize"`
	BatchMaxWait  Duration          `yaml:"batchMaxWait,omitempty" json:"batchMaxWait" tstype:"Duration"`
	EnableGzip    *bool             `yaml:"enableGzip,omitempty" json:"enableGzip"`
	Headers       map[string]string `yaml:"headers,omitempty" json:"headers"`
	ProxyPool     string            `yaml:"proxyPool,omitempty" json:"proxyPool"`
}

func (c *JsonRpcUpstreamConfig) Copy() *JsonRpcUpstreamConfig {
	if c == nil {
		return nil
	}

	copied := &JsonRpcUpstreamConfig{}
	*copied = *c

	if c.Headers != nil {
		maps.Copy(copied.Headers, c.Headers)
	}

	return copied
}

// GrpcUpstreamConfig tunes a gRPC (grpc:// / grpc+bds://) upstream. It is the
// gRPC analogue of JsonRpcUpstreamConfig: JsonRpc holds JSON-RPC/HTTP-specific
// knobs, this holds gRPC-specific ones. Headers are applied as gRPC metadata on
// every outbound request (e.g. an edge-api auth key: authorization: Bearer ...).
type GrpcUpstreamConfig struct {
	Headers map[string]string `yaml:"headers,omitempty" json:"headers"`

	// PoolSize is the number of independent gRPC connections opened to this
	// upstream, selected round-robin per request. See GrpcConnectorConfig.PoolSize.
	// When unset (0) a built-in default is used.
	PoolSize int `yaml:"poolSize,omitempty" json:"poolSize"`
}

func (c *GrpcUpstreamConfig) Copy() *GrpcUpstreamConfig {
	if c == nil {
		return nil
	}

	copied := &GrpcUpstreamConfig{}
	*copied = *c

	if c.Headers != nil {
		copied.Headers = make(map[string]string, len(c.Headers))
		maps.Copy(copied.Headers, c.Headers)
	}

	return copied
}

type EvmUpstreamConfig struct {
	ChainId             int64    `yaml:"chainId" json:"chainId"`
	StatePollerInterval Duration `yaml:"statePollerInterval,omitempty" json:"statePollerInterval" tstype:"Duration"`
	// StatePollerDebounce overrides the debounce interval for the state poller.
	// When 0 (default), the interval is dynamically inferred from the chain's
	// observed block time, falling back to the network-level
	// FallbackStatePollerDebounce, then to a 1s floor.
	StatePollerDebounce                Duration                    `yaml:"statePollerDebounce,omitempty" json:"statePollerDebounce" tstype:"Duration"`
	BlockAvailability                  *EvmBlockAvailabilityConfig `yaml:"blockAvailability,omitempty" json:"blockAvailability"`
	GetLogsAutoSplittingRangeThreshold int64                       `yaml:"getLogsAutoSplittingRangeThreshold,omitempty" json:"getLogsAutoSplittingRangeThreshold"`
	// TraceFilterAutoSplittingRangeThreshold proactively splits trace_filter and
	// arbtrace_filter requests whose block range exceeds this value into contiguous
	// sub-requests executed concurrently and merged before returning. Zero disables
	// the feature.
	TraceFilterAutoSplittingRangeThreshold int64                    `yaml:"traceFilterAutoSplittingRangeThreshold,omitempty" json:"traceFilterAutoSplittingRangeThreshold"`
	SkipWhenSyncing                        *bool                    `yaml:"skipWhenSyncing,omitempty" json:"skipWhenSyncing"`
	Integrity                              *UpstreamIntegrityConfig `yaml:"integrity,omitempty" json:"integrity"`

	// @deprecated: use blockAvailability bounds instead; kept for config back-compat only
	NodeType EvmNodeType `yaml:"nodeType,omitempty" json:"nodeType"`
	// @deprecated: should be removed in a future release
	MaxAvailableRecentBlocks int64 `yaml:"maxAvailableRecentBlocks,omitempty" json:"maxAvailableRecentBlocks"`
	// @deprecated: should be removed in a future release
	DeprecatedGetLogsMaxAllowedRange int64 `yaml:"getLogsMaxAllowedRange,omitempty" json:"-"`
	// @deprecated: should be removed in a future release
	DeprecatedGetLogsMaxAllowedAddresses int64 `yaml:"getLogsMaxAllowedAddresses,omitempty" json:"-"`
	// @deprecated: should be removed in a future release
	DeprecatedGetLogsMaxAllowedTopics int64 `yaml:"getLogsMaxAllowedTopics,omitempty" json:"-"`
	// @deprecated: should be removed in a future release
	DeprecatedGetLogsSplitOnError *bool `yaml:"getLogsSplitOnError,omitempty" json:"-"`
	// @deprecated: should be removed in a future release
	DeprecatedGetLogsMaxBlockRange int64 `yaml:"getLogsMaxBlockRange,omitempty" json:"-"`

	QueryShim *EvmQueryShimConfig `yaml:"queryShim,omitempty" json:"queryShim"`
}

type EvmQueryShimConfig struct {
	Enabled        *bool    `yaml:"enabled,omitempty" json:"enabled"`
	AllowedMethods []string `yaml:"allowedMethods,omitempty" json:"allowedMethods"`
	Concurrency    int      `yaml:"concurrency,omitempty" json:"concurrency"`
	MaxBlockRange  int64    `yaml:"maxBlockRange,omitempty" json:"maxBlockRange"`
	MaxLimit       int      `yaml:"maxLimit,omitempty" json:"maxLimit"`
	DefaultLimit   int      `yaml:"defaultLimit,omitempty" json:"defaultLimit"`
}

func (c *EvmQueryShimConfig) Copy() *EvmQueryShimConfig {
	if c == nil {
		return nil
	}
	copied := &EvmQueryShimConfig{
		Concurrency:   c.Concurrency,
		MaxBlockRange: c.MaxBlockRange,
		MaxLimit:      c.MaxLimit,
		DefaultLimit:  c.DefaultLimit,
	}
	if c.Enabled != nil {
		v := *c.Enabled
		copied.Enabled = &v
	}
	if c.AllowedMethods != nil {
		copied.AllowedMethods = make([]string, len(c.AllowedMethods))
		copy(copied.AllowedMethods, c.AllowedMethods)
	}
	return copied
}

// EvmBlockAvailability defines optional lower/upper block availability expressions for an upstream.
// Presence of lower/upper implies the feature is active. When both are nil, it's effectively off
type EvmBlockAvailabilityConfig struct {
	Lower *EvmAvailabilityBoundConfig `yaml:"lower,omitempty" json:"lower,omitempty"`
	Upper *EvmAvailabilityBoundConfig `yaml:"upper,omitempty" json:"upper,omitempty"`
}

func (c *EvmBlockAvailabilityConfig) Copy() *EvmBlockAvailabilityConfig {
	if c == nil {
		return nil
	}
	out := &EvmBlockAvailabilityConfig{}
	if c.Lower != nil {
		out.Lower = c.Lower.Copy()
	}
	if c.Upper != nil {
		out.Upper = c.Upper.Copy()
	}
	return out
}

// EvmBound represents a single bound definition.
// Exactly one of ExactBlock, LatestMinus, EarliestPlus should be set.
// UpdateRate only applies to earliestBlockPlus bounds: 0 means freeze at first evaluation; >0 means recompute on that cadence.
// For latestBlockMinus, updateRate is ignored: bounds are computed on-demand using the continuously-updated latest block from evmStatePoller.
type EvmAvailabilityProbeType string

const (
	EvmProbeBlockHeader EvmAvailabilityProbeType = "blockHeader"
	EvmProbeEventLogs   EvmAvailabilityProbeType = "eventLogs"
	EvmProbeCallState   EvmAvailabilityProbeType = "callState"
	EvmProbeTraceData   EvmAvailabilityProbeType = "traceData"
)

type EvmAvailabilityBoundConfig struct {
	ExactBlock        *int64                   `yaml:"exactBlock,omitempty" json:"exactBlock,omitempty"`
	LatestBlockMinus  *int64                   `yaml:"latestBlockMinus,omitempty" json:"latestBlockMinus,omitempty"`
	EarliestBlockPlus *int64                   `yaml:"earliestBlockPlus,omitempty" json:"earliestBlockPlus,omitempty"`
	Probe             EvmAvailabilityProbeType `yaml:"probe,omitempty" json:"probe,omitempty"`
	UpdateRate        Duration                 `yaml:"updateRate,omitempty" json:"updateRate,omitempty" tstype:"Duration"`
}

func (c *EvmAvailabilityBoundConfig) Copy() *EvmAvailabilityBoundConfig {
	if c == nil {
		return nil
	}
	out := &EvmAvailabilityBoundConfig{UpdateRate: c.UpdateRate}
	if c.ExactBlock != nil {
		v := *c.ExactBlock
		out.ExactBlock = &v
	}
	if c.LatestBlockMinus != nil {
		v := *c.LatestBlockMinus
		out.LatestBlockMinus = &v
	}
	if c.EarliestBlockPlus != nil {
		v := *c.EarliestBlockPlus
		out.EarliestBlockPlus = &v
	}
	out.Probe = c.Probe
	return out
}

func (c *EvmUpstreamConfig) Copy() *EvmUpstreamConfig {
	if c == nil {
		return nil
	}

	copied := &EvmUpstreamConfig{}
	*copied = *c

	// Deep copy pointer fields to avoid shared state
	if c.BlockAvailability != nil {
		copied.BlockAvailability = c.BlockAvailability.Copy()
	}
	if c.SkipWhenSyncing != nil {
		v := *c.SkipWhenSyncing
		copied.SkipWhenSyncing = &v
	}
	if c.Integrity != nil {
		copied.Integrity = c.Integrity.Copy()
	}
	if c.DeprecatedGetLogsSplitOnError != nil {
		v := *c.DeprecatedGetLogsSplitOnError
		copied.DeprecatedGetLogsSplitOnError = &v
	}
	if c.QueryShim != nil {
		copied.QueryShim = c.QueryShim.Copy()
	}

	return copied
}

type FailsafeConfig struct {
	MatchMethod    string                      `yaml:"matchMethod,omitempty" json:"matchMethod"`
	MatchFinality  []DataFinalityState         `yaml:"matchFinality,omitempty" json:"matchFinality"`
	Retry          *RetryPolicyConfig          `yaml:"retry" json:"retry"`
	CircuitBreaker *CircuitBreakerPolicyConfig `yaml:"circuitBreaker" json:"circuitBreaker"`
	Timeout        *TimeoutPolicyConfig        `yaml:"timeout" json:"timeout"`
	Hedge          *HedgePolicyConfig          `yaml:"hedge" json:"hedge"`
	Consensus      *ConsensusPolicyConfig      `yaml:"consensus" json:"consensus"`
}

// NetworkFailsafeConfig is the scope-specific alias for network-level
// failsafe policies. By convention, CircuitBreaker is not used at this
// scope (use upstream-scope breakers instead); validation enforces this.
type NetworkFailsafeConfig = FailsafeConfig

// UpstreamFailsafeConfig is the scope-specific alias for per-upstream
// failsafe policies. By convention, Consensus is not used at this
// scope (consensus is a network-scope concern only); validation
// enforces this.
type UpstreamFailsafeConfig = FailsafeConfig

// CacheFailsafeConfig is the scope-specific alias for cache-connector
// failsafe policies. Hedge.Quantile is not allowed here (no per-method
// quantile data on cache reads); validation enforces this.
type CacheFailsafeConfig = FailsafeConfig

func (c *FailsafeConfig) Copy() *FailsafeConfig {
	if c == nil {
		return nil
	}

	copied := &FailsafeConfig{}
	*copied = *c

	// Deep copy the MatchFinality array
	if c.MatchFinality != nil {
		copied.MatchFinality = make([]DataFinalityState, len(c.MatchFinality))
		copy(copied.MatchFinality, c.MatchFinality)
	}

	if c.Retry != nil {
		copied.Retry = c.Retry.Copy()
	}

	if c.CircuitBreaker != nil {
		copied.CircuitBreaker = c.CircuitBreaker.Copy()
	}

	if c.Timeout != nil {
		copied.Timeout = c.Timeout.Copy()
	}

	if c.Hedge != nil {
		copied.Hedge = c.Hedge.Copy()
	}

	if c.Consensus != nil {
		copied.Consensus = c.Consensus.Copy()
	}

	return copied
}

type RetryPolicyConfig struct {
	MaxAttempts     int      `yaml:"maxAttempts" json:"maxAttempts"`
	Delay           Duration `yaml:"delay,omitempty" json:"delay" tstype:"Duration"`
	BackoffMaxDelay Duration `yaml:"backoffMaxDelay,omitempty" json:"backoffMaxDelay" tstype:"Duration"`
	BackoffFactor   float32  `yaml:"backoffFactor,omitempty" json:"backoffFactor"`
	Jitter          Duration `yaml:"jitter,omitempty" json:"jitter" tstype:"Duration"`
	// EmptyResultAccept lists methods for which an empty/null result is considered valid
	// and should NOT be retried (e.g. eth_getLogs, eth_call where empty is a legitimate response).
	EmptyResultAccept []string `yaml:"emptyResultAccept,omitempty" json:"emptyResultAccept"`
	// @deprecated: use EmptyResultAccept instead.
	EmptyResultIgnore []string `yaml:"emptyResultIgnore,omitempty" json:"emptyResultIgnore"`
	// EmptyResultMaxAttempts limits total attempts when retries are triggered due to empty responses.
	EmptyResultMaxAttempts int `yaml:"emptyResultMaxAttempts,omitempty" json:"emptyResultMaxAttempts"`
	// EmptyResultDelay is the fixed fallback delay before retrying when the requested
	// data isn't on the upstream yet — an empty/missing-data point-lookup OR an
	// ErrUpstreamBlockUnavailable (same root cause: the block/tx isn't produced or
	// indexed yet). Retries prefer the dynamic block-time delay (EMA block time ×
	// Evm.BlockUnavailableDelayMultiplier); this fixed value is used only before that
	// estimate warms up. (Supersedes the now-deprecated BlockUnavailableDelay.)
	EmptyResultDelay Duration `yaml:"emptyResultDelay,omitempty" json:"emptyResultDelay" tstype:"Duration"`

	// Deprecated: merged into EmptyResultDelay (same purpose — fixed fallback delay for
	// data-not-available retries). Retained as a yaml-only key so existing configs that
	// still set `blockUnavailableDelay` keep loading; RetryPolicyConfig.SetDefaults
	// migrates the value into EmptyResultDelay and clears this. Not read anywhere at
	// runtime, and hidden from the generated TS types (json:"-") so new configs use
	// emptyResultDelay.
	BlockUnavailableDelay Duration `yaml:"blockUnavailableDelay,omitempty" json:"-"`
}

func (c *RetryPolicyConfig) Copy() *RetryPolicyConfig {
	if c == nil {
		return nil
	}
	copied := &RetryPolicyConfig{}
	*copied = *c
	return copied
}

type CircuitBreakerPolicyConfig struct {
	FailureThresholdCount    uint     `yaml:"failureThresholdCount" json:"failureThresholdCount"`
	FailureThresholdCapacity uint     `yaml:"failureThresholdCapacity" json:"failureThresholdCapacity"`
	HalfOpenAfter            Duration `yaml:"halfOpenAfter,omitempty" json:"halfOpenAfter" tstype:"Duration"`
	SuccessThresholdCount    uint     `yaml:"successThresholdCount" json:"successThresholdCount"`
	SuccessThresholdCapacity uint     `yaml:"successThresholdCapacity" json:"successThresholdCapacity"`
}

func (c *CircuitBreakerPolicyConfig) Copy() *CircuitBreakerPolicyConfig {
	if c == nil {
		return nil
	}
	copied := &CircuitBreakerPolicyConfig{}
	*copied = *c
	return copied
}

// TimeoutPolicyConfig is the timeout policy. Duration is the unified
// AdaptiveDuration — a scalar shorthand ("5s") or an object form
// ({base, quantile, min, max}) for adaptive caps driven by per-method
// latency quantiles.
//
// Wire format also accepts the legacy flat form
// (`duration: 5s, quantile: 0.99, minDuration: 200ms, maxDuration: 10s`)
// — siblings get folded into Duration at YAML/JSON unmarshal time.
type TimeoutPolicyConfig struct {
	Duration *AdaptiveDuration `yaml:"duration,omitempty" json:"duration,omitempty" tstype:"Duration | AdaptiveDuration"`
}

func (c *TimeoutPolicyConfig) Copy() *TimeoutPolicyConfig {
	if c == nil {
		return nil
	}
	return &TimeoutPolicyConfig{Duration: c.Duration.Copy()}
}

// UnmarshalYAML accepts the new unified form (Duration as scalar or
// AdaptiveDuration object) and the legacy flat form with sibling
// quantile/minDuration/maxDuration fields — siblings fold into Duration.
func (c *TimeoutPolicyConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type legacy struct {
		Duration    *AdaptiveDuration `yaml:"duration,omitempty"`
		Quantile    float64           `yaml:"quantile,omitempty"`
		MinDuration Duration          `yaml:"minDuration,omitempty"`
		MaxDuration Duration          `yaml:"maxDuration,omitempty"`
	}
	var raw legacy
	if err := unmarshal(&raw); err != nil {
		return err
	}
	c.Duration = raw.Duration
	c.applyLegacySiblings(raw.Quantile, raw.MinDuration, raw.MaxDuration)
	return nil
}

// UnmarshalJSON mirrors the YAML behaviour for admin/RPC entry points.
func (c *TimeoutPolicyConfig) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	type legacy struct {
		Duration    *AdaptiveDuration `json:"duration,omitempty"`
		Quantile    float64           `json:"quantile,omitempty"`
		MinDuration json.RawMessage   `json:"minDuration,omitempty"`
		MaxDuration json.RawMessage   `json:"maxDuration,omitempty"`
	}
	var raw legacy
	if err := SonicCfg.Unmarshal(data, &raw); err != nil {
		return err
	}
	minD, err := parseJSONDuration(raw.MinDuration)
	if err != nil {
		return fmt.Errorf("timeout.minDuration: %w", err)
	}
	maxD, err := parseJSONDuration(raw.MaxDuration)
	if err != nil {
		return fmt.Errorf("timeout.maxDuration: %w", err)
	}
	c.Duration = raw.Duration
	c.applyLegacySiblings(raw.Quantile, minD, maxD)
	return nil
}

func (c *TimeoutPolicyConfig) applyLegacySiblings(quantile float64, minD, maxD Duration) {
	if quantile == 0 && minD == 0 && maxD == 0 {
		return
	}
	if c.Duration == nil {
		c.Duration = &AdaptiveDuration{}
	}
	if c.Duration.Quantile == 0 {
		c.Duration.Quantile = quantile
	}
	if c.Duration.Min == 0 {
		c.Duration.Min = minD
	}
	if c.Duration.Max == 0 {
		c.Duration.Max = maxD
	}
}

// HedgePolicyConfig is the hedge policy. Delay is the unified
// AdaptiveDuration — scalar shorthand ("100ms") or object form
// ({base, quantile, min, max}) for quantile-driven hedge timing.
//
// Wire format also accepts the legacy flat form
// (`delay: 100ms, quantile: 0.95, minDelay: 50ms, maxDelay: 2s`) —
// siblings get folded into Delay at YAML/JSON unmarshal time.
type HedgePolicyConfig struct {
	Delay    *AdaptiveDuration `yaml:"delay,omitempty" json:"delay,omitempty" tstype:"Duration | AdaptiveDuration"`
	MaxCount int               `yaml:"maxCount" json:"maxCount"`
}

func (c *HedgePolicyConfig) Copy() *HedgePolicyConfig {
	if c == nil {
		return nil
	}
	return &HedgePolicyConfig{
		Delay:    c.Delay.Copy(),
		MaxCount: c.MaxCount,
	}
}

// UnmarshalYAML accepts the new unified form (Delay as scalar or
// AdaptiveDuration object) and the legacy flat form with sibling
// quantile/minDelay/maxDelay fields — siblings fold into Delay.
func (c *HedgePolicyConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type legacy struct {
		Delay    *AdaptiveDuration `yaml:"delay,omitempty"`
		MaxCount int               `yaml:"maxCount,omitempty"`
		Quantile float64           `yaml:"quantile,omitempty"`
		MinDelay Duration          `yaml:"minDelay,omitempty"`
		MaxDelay Duration          `yaml:"maxDelay,omitempty"`
	}
	var raw legacy
	if err := unmarshal(&raw); err != nil {
		return err
	}
	c.Delay = raw.Delay
	c.MaxCount = raw.MaxCount
	c.applyLegacySiblings(raw.Quantile, raw.MinDelay, raw.MaxDelay)
	return nil
}

// UnmarshalJSON mirrors the YAML behaviour.
func (c *HedgePolicyConfig) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	type legacy struct {
		Delay    *AdaptiveDuration `json:"delay,omitempty"`
		MaxCount int               `json:"maxCount,omitempty"`
		Quantile float64           `json:"quantile,omitempty"`
		MinDelay json.RawMessage   `json:"minDelay,omitempty"`
		MaxDelay json.RawMessage   `json:"maxDelay,omitempty"`
	}
	var raw legacy
	if err := SonicCfg.Unmarshal(data, &raw); err != nil {
		return err
	}
	minD, err := parseJSONDuration(raw.MinDelay)
	if err != nil {
		return fmt.Errorf("hedge.minDelay: %w", err)
	}
	maxD, err := parseJSONDuration(raw.MaxDelay)
	if err != nil {
		return fmt.Errorf("hedge.maxDelay: %w", err)
	}
	c.Delay = raw.Delay
	c.MaxCount = raw.MaxCount
	c.applyLegacySiblings(raw.Quantile, minD, maxD)
	return nil
}

func (c *HedgePolicyConfig) applyLegacySiblings(quantile float64, minD, maxD Duration) {
	if quantile == 0 && minD == 0 && maxD == 0 {
		return
	}
	if c.Delay == nil {
		c.Delay = &AdaptiveDuration{}
	}
	if c.Delay.Quantile == 0 {
		c.Delay.Quantile = quantile
	}
	if c.Delay.Min == 0 {
		c.Delay.Min = minD
	}
	if c.Delay.Max == 0 {
		c.Delay.Max = maxD
	}
}

type ConsensusLowParticipantsBehavior string

const (
	ConsensusLowParticipantsBehaviorReturnError                 ConsensusLowParticipantsBehavior = "returnError"
	ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult ConsensusLowParticipantsBehavior = "acceptMostCommonValidResult"
	ConsensusLowParticipantsBehaviorPreferBlockHeadLeader       ConsensusLowParticipantsBehavior = "preferBlockHeadLeader"
	ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader         ConsensusLowParticipantsBehavior = "onlyBlockHeadLeader"
)

type ConsensusDisputeBehavior string

const (
	ConsensusDisputeBehaviorReturnError                 ConsensusDisputeBehavior = "returnError"
	ConsensusDisputeBehaviorAcceptMostCommonValidResult ConsensusDisputeBehavior = "acceptMostCommonValidResult"
	ConsensusDisputeBehaviorPreferBlockHeadLeader       ConsensusDisputeBehavior = "preferBlockHeadLeader"
	ConsensusDisputeBehaviorOnlyBlockHeadLeader         ConsensusDisputeBehavior = "onlyBlockHeadLeader"
)

type ConsensusPolicyConfig struct {
	MaxParticipants         int                              `yaml:"maxParticipants" json:"maxParticipants"`
	AgreementThreshold      int                              `yaml:"agreementThreshold,omitempty" json:"agreementThreshold"`
	DisputeBehavior         ConsensusDisputeBehavior         `yaml:"disputeBehavior,omitempty" json:"disputeBehavior"`
	LowParticipantsBehavior ConsensusLowParticipantsBehavior `yaml:"lowParticipantsBehavior,omitempty" json:"lowParticipantsBehavior"`
	PunishMisbehavior       *PunishMisbehaviorConfig         `yaml:"punishMisbehavior,omitempty" json:"punishMisbehavior"`
	DisputeLogLevel         string                           `yaml:"disputeLogLevel,omitempty" json:"disputeLogLevel"` // "trace", "debug", "info", "warn", "error"
	IgnoreFields            map[string][]string              `yaml:"ignoreFields,omitempty" json:"ignoreFields"`
	PreferNonEmpty          *bool                            `yaml:"preferNonEmpty,omitempty" json:"preferNonEmpty"`
	PreferLargerResponses   *bool                            `yaml:"preferLargerResponses,omitempty" json:"preferLargerResponses"`
	MisbehaviorsDestination *MisbehaviorsDestinationConfig   `yaml:"misbehaviorsDestination,omitempty" json:"misbehaviorsDestination"`
	// PreferHighestValueFor specifies methods that should use highest-value comparison
	// instead of hash-based consensus. Map key is method name, value is array of field paths.
	// Field paths: "result" for direct result value (e.g., eth_getTransactionCount returns hex),
	// or field name for nested result objects (e.g., "nonce" for result.nonce).
	// When multiple fields are specified, they act as tie-breakers in order.
	PreferHighestValueFor map[string][]string `yaml:"preferHighestValueFor,omitempty" json:"preferHighestValueFor"`
	// FireAndForget when true, allows consensus to return a response to the client immediately
	// upon short-circuit, but does NOT cancel in-flight requests to other upstreams.
	// This is useful for write operations like eth_sendRawTransaction where you want to
	// broadcast the transaction to as many nodes as possible while still returning quickly.
	// Default is false (normal behavior - cancel remaining requests on short-circuit).
	FireAndForget bool `yaml:"fireAndForget,omitempty" json:"fireAndForget"`

	// MaxWaitOnResult caps how long consensus waits for additional participants
	// AFTER at least one non-empty response has arrived. Use this to bound
	// p99 latency when most upstreams are fast but one is a slow straggler:
	// once a real answer is in hand, give the rest at most this long to
	// confirm or dispute, then resolve with what we have.
	//
	// Accepts a duration scalar ("200ms") or an AdaptiveDuration object
	// ({base, quantile, min, max}) for adaptive caps driven by per-method
	// latency quantiles. Defaults are applied when consensus is configured
	// but this field is omitted — see common/defaults.go.
	MaxWaitOnResult *AdaptiveDuration `yaml:"maxWaitOnResult,omitempty" json:"maxWaitOnResult,omitempty" tstype:"Duration | AdaptiveDuration"`

	// MaxWaitOnEmpty caps how long consensus waits for additional participants
	// AFTER the first response (of any kind — empty, error, or non-empty)
	// has arrived. Typically set larger than MaxWaitOnResult because an
	// operator is more patient when no useful data is in hand yet.
	//
	// Same shape as MaxWaitOnResult; defaults applied when consensus is set.
	MaxWaitOnEmpty *AdaptiveDuration `yaml:"maxWaitOnEmpty,omitempty" json:"maxWaitOnEmpty,omitempty" tstype:"Duration | AdaptiveDuration"`

	// RequiredParticipants enforces a minimum number of consensus
	// participants carrying a given tag. Each entry says "at least
	// `minParticipants` of the upstreams that participate must match
	// `tag`". The engine front-loads enough tag-matching upstreams into
	// the participant set so the first `maxParticipants` drawn satisfy
	// every entry — without changing `maxParticipants` itself.
	//
	// Best-effort and governed by the EXISTING consensus behaviors: if a
	// required group has fewer healthy upstreams than requested (or the
	// quotas can't all fit within `maxParticipants`), consensus simply
	// runs with what it can promote and the resulting participation is
	// handled by `lowParticipantsBehavior` / `agreementThreshold` exactly
	// like any other low-participation tick. Empty (default) = disabled.
	RequiredParticipants []*ConsensusRequiredParticipant `yaml:"requiredParticipants,omitempty" json:"requiredParticipants,omitempty"`
}

// ConsensusRequiredParticipant is one tag-quota entry for
// `consensus.requiredParticipants`. `Tag` is a glob pattern (`*`, `?`)
// matched against each upstream's `tags`; `MinParticipants` is the minimum
// number of matching upstreams that must be in the consensus participant
// set. A single upstream can satisfy multiple entries it matches.
type ConsensusRequiredParticipant struct {
	Tag             string `yaml:"tag" json:"tag"`
	MinParticipants int    `yaml:"minParticipants" json:"minParticipants"`
}

func (c *ConsensusPolicyConfig) Copy() *ConsensusPolicyConfig {
	if c == nil {
		return nil
	}
	copied := &ConsensusPolicyConfig{}
	*copied = *c

	if c.PunishMisbehavior != nil {
		copied.PunishMisbehavior = c.PunishMisbehavior.Copy()
	}

	if c.MisbehaviorsDestination != nil {
		copied.MisbehaviorsDestination = c.MisbehaviorsDestination.Copy()
	}

	if c.IgnoreFields != nil {
		copied.IgnoreFields = make(map[string][]string, len(c.IgnoreFields))
		for method, fields := range c.IgnoreFields {
			copied.IgnoreFields[method] = make([]string, len(fields))
			copy(copied.IgnoreFields[method], fields)
		}
	}

	if c.PreferHighestValueFor != nil {
		copied.PreferHighestValueFor = make(map[string][]string, len(c.PreferHighestValueFor))
		for method, fields := range c.PreferHighestValueFor {
			copied.PreferHighestValueFor[method] = make([]string, len(fields))
			copy(copied.PreferHighestValueFor[method], fields)
		}
	}

	copied.MaxWaitOnResult = c.MaxWaitOnResult.Copy()
	copied.MaxWaitOnEmpty = c.MaxWaitOnEmpty.Copy()

	if c.RequiredParticipants != nil {
		copied.RequiredParticipants = make([]*ConsensusRequiredParticipant, len(c.RequiredParticipants))
		for i, rp := range c.RequiredParticipants {
			if rp == nil {
				continue
			}
			rpCopy := *rp
			copied.RequiredParticipants[i] = &rpCopy
		}
	}

	return copied
}

type MisbehaviorsDestinationType string

const (
	MisbehaviorsDestinationTypeFile MisbehaviorsDestinationType = "file"
	MisbehaviorsDestinationTypeS3   MisbehaviorsDestinationType = "s3"
)

type MisbehaviorsDestinationConfig struct {
	// Type of destination: "file" or "s3"
	Type MisbehaviorsDestinationType `yaml:"type" json:"type" tstype:"'file' | 's3'"`

	// Path for file destination, or S3 URI (s3://bucket/prefix/) for S3 destination
	Path string `yaml:"path" json:"path"`

	// Pattern for generating file names. Supports placeholders:
	// {dateByHour} - formatted as 2006-01-02-15
	// {dateByDay} - formatted as 2006-01-02
	// {method} - the RPC method name
	// {networkId} - the network ID with : replaced by _
	// {instanceId} - unique instance identifier
	FilePattern string `yaml:"filePattern,omitempty" json:"filePattern"`

	// S3-specific settings for bulk flushing
	S3 *S3FlushConfig `yaml:"s3,omitempty" json:"s3,omitempty"`
}

type S3FlushConfig struct {
	// Maximum number of records to buffer before flushing (default: 100)
	MaxRecords int `yaml:"maxRecords,omitempty" json:"maxRecords"`

	// Maximum size in bytes to buffer before flushing (default: 1MB)
	MaxSize int64 `yaml:"maxSize,omitempty" json:"maxSize"`

	// Maximum time to wait before flushing buffered records (default: 60s)
	FlushInterval Duration `yaml:"flushInterval,omitempty" json:"flushInterval" tstype:"Duration"`

	// AWS region for S3 bucket (defaults to AWS_REGION env var)
	Region string `yaml:"region,omitempty" json:"region"`

	// AWS credentials config (optional). If not specified, uses standard AWS credential chain:
	// 1. Environment variables (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY)
	// 2. IAM role (for EC2/ECS/EKS)
	// 3. Shared credentials file (~/.aws/credentials)
	// Supported modes: "env", "file", "secret"
	Credentials *AwsAuthConfig `yaml:"credentials,omitempty" json:"credentials,omitempty"`

	// Content type for uploaded files (default: "application/jsonl")
	ContentType string `yaml:"contentType,omitempty" json:"contentType"`
}

func (c *MisbehaviorsDestinationConfig) Copy() *MisbehaviorsDestinationConfig {
	if c == nil {
		return nil
	}
	copied := &MisbehaviorsDestinationConfig{
		Type:        c.Type,
		Path:        c.Path,
		FilePattern: c.FilePattern,
	}
	if c.S3 != nil {
		copied.S3 = &S3FlushConfig{
			MaxRecords:    c.S3.MaxRecords,
			MaxSize:       c.S3.MaxSize,
			FlushInterval: c.S3.FlushInterval,
			Region:        c.S3.Region,
			ContentType:   c.S3.ContentType,
		}
		if c.S3.Credentials != nil {
			// AwsAuthConfig already exists in the codebase
			creds := *c.S3.Credentials
			copied.S3.Credentials = &creds
		}
	}
	return copied
}

type PunishMisbehaviorConfig struct {
	DisputeThreshold uint     `yaml:"disputeThreshold" json:"disputeThreshold"`
	DisputeWindow    Duration `yaml:"disputeWindow,omitempty" json:"disputeWindow" tstype:"Duration"`
	SitOutPenalty    Duration `yaml:"sitOutPenalty,omitempty" json:"sitOutPenalty" tstype:"Duration"`
}

func (c *PunishMisbehaviorConfig) Copy() *PunishMisbehaviorConfig {
	if c == nil {
		return nil
	}
	copied := &PunishMisbehaviorConfig{}
	*copied = *c
	return copied
}

type RateLimiterConfig struct {
	Store   *RateLimitStoreConfig    `yaml:"store,omitempty" json:"store"`
	Budgets []*RateLimitBudgetConfig `yaml:"budgets" json:"budgets" tstype:"RateLimitBudgetConfig[]"`
}

type RateLimitBudgetConfig struct {
	Id    string                 `yaml:"id" json:"id"`
	Rules []*RateLimitRuleConfig `yaml:"rules" json:"rules" tstype:"RateLimitRuleConfig[]"`
}

type RateLimitRuleConfig struct {
	Method   string `yaml:"method" json:"method"`
	MaxCount uint32 `yaml:"maxCount" json:"maxCount"`
	// Period is the canonical period selector. Supported: second, minute, hour, day, week, month, year
	Period     RateLimitPeriod `yaml:"period" json:"period" tstype:"RateLimitPeriod"`
	WaitTime   Duration        `yaml:"waitTime,omitempty" json:"waitTime,omitempty" tstype:"Duration"`
	PerIP      bool            `yaml:"perIP,omitempty" json:"perIP,omitempty"`
	PerUser    bool            `yaml:"perUser,omitempty" json:"perUser,omitempty"`
	PerNetwork bool            `yaml:"perNetwork,omitempty" json:"perNetwork,omitempty"`
}

// ScopeString returns a comma-separated list of enabled scopes in deterministic order.
// Possible values: "user", "network", "ip". Empty string if no scope-specific flags are enabled.
func (c *RateLimitRuleConfig) ScopeString() string {
	scopes := make([]string, 0, 3)
	if c.PerUser {
		scopes = append(scopes, "user")
	}
	if c.PerNetwork {
		scopes = append(scopes, "network")
	}
	if c.PerIP {
		scopes = append(scopes, "ip")
	}
	return strings.Join(scopes, ",")
}

// RateLimitPeriod enumerates supported periods for rate limiting.
// It is an int enum to enable strong typing in TypeScript generation, while
// marshaling to JSON/YAML as human-readable strings like "second", "minute", etc.
type RateLimitPeriod int

const (
	RateLimitPeriodSecond RateLimitPeriod = iota
	RateLimitPeriodMinute
	RateLimitPeriodHour
	RateLimitPeriodDay
	RateLimitPeriodWeek
	RateLimitPeriodMonth
	RateLimitPeriodYear
)

func (p RateLimitPeriod) String() string {
	switch p {
	case RateLimitPeriodSecond:
		return "second"
	case RateLimitPeriodMinute:
		return "minute"
	case RateLimitPeriodHour:
		return "hour"
	case RateLimitPeriodDay:
		return "day"
	case RateLimitPeriodWeek:
		return "week"
	case RateLimitPeriodMonth:
		return "month"
	case RateLimitPeriodYear:
		return "year"
	default:
		return "unknown"
	}
}

func (p RateLimitPeriod) MarshalYAML() (interface{}, error) {
	return p.String(), nil
}

func (p RateLimitPeriod) MarshalJSON() ([]byte, error) {
	return SonicCfg.Marshal(p.String())
}

// Backward-compat: accept Go duration strings (e.g., 1s, 1m, 1h, 24h, 7d, 30d, 365d) and map to enum.
func (p *RateLimitPeriod) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Try as integer enum first (YAML integer values like period: 1)
	var i int
	if err := unmarshal(&i); err == nil {
		switch RateLimitPeriod(i) {
		case RateLimitPeriodSecond, RateLimitPeriodMinute, RateLimitPeriodHour, RateLimitPeriodDay,
			RateLimitPeriodWeek, RateLimitPeriodMonth, RateLimitPeriodYear:
			*p = RateLimitPeriod(i)
			return nil
		default:
			return fmt.Errorf("rate limiter period must be one of: second, minute, hour, day, week, month, year (got %d)", i)
		}
	}

	// Try as string (enum name or duration expression)
	var s string
	if err := unmarshal(&s); err == nil {
		ls := strings.ToLower(strings.TrimSpace(s))
		switch ls {
		case "second", "1s":
			*p = RateLimitPeriodSecond
			return nil
		case "minute", "1m", "60s":
			*p = RateLimitPeriodMinute
			return nil
		case "hour", "1h", "3600s":
			*p = RateLimitPeriodHour
			return nil
		case "day", "24h", "1d", "86400s":
			*p = RateLimitPeriodDay
			return nil
		case "week", "7d", "168h", "604800s":
			*p = RateLimitPeriodWeek
			return nil
		case "month", "30d", "720h", "2592000s":
			*p = RateLimitPeriodMonth
			return nil
		case "year", "365d", "8760h", "31536000s":
			*p = RateLimitPeriodYear
			return nil
		default:
			// Try as duration expression (e.g., 1s)
			if d, err := time.ParseDuration(s); err == nil {
				switch d {
				case time.Second:
					*p = RateLimitPeriodSecond
				case time.Minute:
					*p = RateLimitPeriodMinute
				case time.Hour:
					*p = RateLimitPeriodHour
				case 24 * time.Hour:
					*p = RateLimitPeriodDay
				case 7 * 24 * time.Hour:
					*p = RateLimitPeriodWeek
				case 30 * 24 * time.Hour:
					*p = RateLimitPeriodMonth
				case 365 * 24 * time.Hour:
					*p = RateLimitPeriodYear
				default:
					return fmt.Errorf("rate limiter period must be one of: second, minute, hour, day, week, month, year (got %s)", s)
				}
				return nil
			}
			return fmt.Errorf("rate limiter period must be one of: second, minute, hour, day, week, month, year (got %s)", s)
		}
	}
	// Neither integer nor string matched
	return fmt.Errorf("invalid period type; expected string enum, integer enum, or duration like '1s'")
}

func (p RateLimitPeriod) Unit() pb.RateLimitResponse_RateLimit_Unit {
	switch p {
	case RateLimitPeriodSecond:
		return pb.RateLimitResponse_RateLimit_SECOND
	case RateLimitPeriodMinute:
		return pb.RateLimitResponse_RateLimit_MINUTE
	case RateLimitPeriodHour:
		return pb.RateLimitResponse_RateLimit_HOUR
	case RateLimitPeriodDay:
		return pb.RateLimitResponse_RateLimit_DAY
	case RateLimitPeriodWeek:
		return pb.RateLimitResponse_RateLimit_WEEK
	case RateLimitPeriodMonth:
		return pb.RateLimitResponse_RateLimit_MONTH
	case RateLimitPeriodYear:
		return pb.RateLimitResponse_RateLimit_YEAR
	default:
		return pb.RateLimitResponse_RateLimit_UNKNOWN
	}
}

func (c *Config) HasRateLimiterBudget(id string) bool {
	if c.RateLimiters == nil || len(c.RateLimiters.Budgets) == 0 {
		return false
	}
	for _, budget := range c.RateLimiters.Budgets {
		if budget.Id == id {
			return true
		}
	}
	return false
}

type ProxyPoolConfig struct {
	ID   string   `yaml:"id" json:"id"`
	Urls []string `yaml:"urls" json:"urls"`
}

type MethodsConfig struct {
	PreserveDefaultMethods bool                          `yaml:"preserveDefaultMethods,omitempty" json:"preserveDefaultMethods"`
	Definitions            map[string]*CacheMethodConfig `yaml:"definitions,omitempty" json:"definitions"`
}

type NetworkConfig struct {
	Architecture      NetworkArchitecture      `yaml:"architecture" json:"architecture" tstype:"TsNetworkArchitecture"`
	RateLimitBudget   string                   `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget"`
	Failsafe          []*FailsafeConfig        `yaml:"failsafe,omitempty" json:"failsafe"`
	Evm               *EvmNetworkConfig        `yaml:"evm,omitempty" json:"evm"`
	SelectionPolicy   *SelectionPolicyConfig   `yaml:"selectionPolicy,omitempty" json:"selectionPolicy"`
	DirectiveDefaults *DirectiveDefaultsConfig `yaml:"directiveDefaults,omitempty" json:"directiveDefaults"`
	Alias             string                   `yaml:"alias,omitempty" json:"alias"`
	Methods           *MethodsConfig           `yaml:"methods,omitempty" json:"methods"`
	Multiplexing      *bool                    `yaml:"multiplexing,omitempty" json:"multiplexing"`
	StaticResponses   []*StaticResponseConfig  `yaml:"staticResponses,omitempty" json:"staticResponses,omitempty"`
}

// StaticResponseConfig declares a canned JSON-RPC response for a specific
// (method, params) pair on a network. When an inbound request matches, the
// configured response is returned immediately and no upstream is contacted.
// Useful for chains that deviate from client assumptions (for example, chains
// whose genesis block is not 0) where probing upstreams would yield errors
// or inconsistent data.
type StaticResponseConfig struct {
	Method   string                    `yaml:"method" json:"method"`
	Params   []interface{}             `yaml:"params,omitempty" json:"params,omitempty"`
	Response *StaticResponseBodyConfig `yaml:"response" json:"response"`
}

// StaticResponseBodyConfig holds the JSON-RPC payload to serve. Exactly one
// of Result or Error must be set.
type StaticResponseBodyConfig struct {
	Result interface{}                `yaml:"result,omitempty" json:"result"`
	Error  *StaticResponseErrorConfig `yaml:"error,omitempty" json:"error"`
}

// StaticResponseErrorConfig mirrors a JSON-RPC error object.
type StaticResponseErrorConfig struct {
	Code    int         `yaml:"code" json:"code"`
	Message string      `yaml:"message" json:"message"`
	Data    interface{} `yaml:"data,omitempty" json:"data"`
}

func (n *NetworkConfig) MultiplexingEnabled() bool {
	if n == nil || n.Multiplexing == nil {
		return true
	}
	return *n.Multiplexing
}

// UnmarshalYAML provides backward compatibility for old single failsafe object format
func (n *NetworkConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Define a type alias to avoid recursion
	type rawNetworkConfig NetworkConfig
	raw := (*rawNetworkConfig)(n)

	// Try unmarshaling normally first
	err := unmarshal(raw)
	if err == nil {
		return nil
	}

	// Save the original error - it might be more informative
	originalErr := err

	// Check if the error is about unknown fields - if so, return it as is
	// This preserves errors like "field maxCount not found in type"
	errStr := err.Error()
	if strings.Contains(errStr, "not found in type") ||
		strings.Contains(errStr, "unknown field") {
		return originalErr
	}

	// If that fails, try the old format with single failsafe object
	type oldNetworkConfig struct {
		Architecture      NetworkArchitecture      `yaml:"architecture"`
		RateLimitBudget   string                   `yaml:"rateLimitBudget,omitempty"`
		Failsafe          *FailsafeConfig          `yaml:"failsafe,omitempty"`
		Evm               *EvmNetworkConfig        `yaml:"evm,omitempty"`
		SelectionPolicy   *SelectionPolicyConfig   `yaml:"selectionPolicy,omitempty"`
		DirectiveDefaults *DirectiveDefaultsConfig `yaml:"directiveDefaults,omitempty"`
		Alias             string                   `yaml:"alias,omitempty"`
		Methods           *MethodsConfig           `yaml:"methods,omitempty"`
		StaticResponses   []*StaticResponseConfig  `yaml:"staticResponses,omitempty"`
	}

	var old oldNetworkConfig
	if err := unmarshal(&old); err != nil {
		// If both formats fail, return the original error as it's likely more informative
		// about the actual problem (like invalid field names)
		return originalErr
	}

	// Convert old format to new format
	n.Architecture = old.Architecture
	n.RateLimitBudget = old.RateLimitBudget
	n.Evm = old.Evm
	n.SelectionPolicy = old.SelectionPolicy
	n.DirectiveDefaults = old.DirectiveDefaults
	n.Alias = old.Alias
	n.Methods = old.Methods
	n.StaticResponses = old.StaticResponses

	if old.Failsafe != nil {
		// Ensure MatchMethod has a default value for backward compatibility
		if old.Failsafe.MatchMethod == "" {
			old.Failsafe.MatchMethod = "*"
		}
		n.Failsafe = []*FailsafeConfig{old.Failsafe}
	}

	return nil
}

type DirectiveDefaultsConfig struct {
	RetryEmpty        *bool       `yaml:"retryEmpty,omitempty" json:"retryEmpty"`
	RetryPending      *bool       `yaml:"retryPending,omitempty" json:"retryPending"`
	SkipCacheRead     interface{} `yaml:"skipCacheRead,omitempty" json:"skipCacheRead"`
	UseUpstream       *string     `yaml:"useUpstream,omitempty" json:"useUpstream"`
	SkipInterpolation *bool       `yaml:"skipInterpolation,omitempty" json:"skipInterpolation"`
	SkipConsensus     *bool       `yaml:"skipConsensus,omitempty" json:"skipConsensus"`

	// Validation: Block Integrity
	EnforceHighestBlock        *bool `yaml:"enforceHighestBlock,omitempty" json:"enforceHighestBlock"`
	EnforceGetLogsBlockRange   *bool `yaml:"enforceGetLogsBlockRange,omitempty" json:"enforceGetLogsBlockRange"`
	EnforceNonNullTaggedBlocks *bool `yaml:"enforceNonNullTaggedBlocks,omitempty" json:"enforceNonNullTaggedBlocks"`

	// ValidateTransactionsRoot: checks transactionsRoot vs transaction count consistency.
	// Defaults to true. Disable for non-standard chains that use unusual trie roots.
	ValidateTransactionsRoot *bool `yaml:"validateTransactionsRoot,omitempty" json:"validateTransactionsRoot"`

	// Validation: Header Field Lengths
	ValidateHeaderFieldLengths *bool `yaml:"validateHeaderFieldLengths,omitempty" json:"validateHeaderFieldLengths"`

	// Validation: Transactions (for eth_getBlockByNumber/Hash with full txs)
	ValidateTransactionFields    *bool `yaml:"validateTransactionFields,omitempty" json:"validateTransactionFields"`
	ValidateTransactionBlockInfo *bool `yaml:"validateTransactionBlockInfo,omitempty" json:"validateTransactionBlockInfo"`

	// Validation: Receipts & Logs
	EnforceLogIndexStrictIncrements *bool `yaml:"enforceLogIndexStrictIncrements,omitempty" json:"enforceLogIndexStrictIncrements"`
	ValidateTxHashUniqueness        *bool `yaml:"validateTxHashUniqueness,omitempty" json:"validateTxHashUniqueness"`
	ValidateTransactionIndex        *bool `yaml:"validateTransactionIndex,omitempty" json:"validateTransactionIndex"`
	ValidateLogFields               *bool `yaml:"validateLogFields,omitempty" json:"validateLogFields"`

	// Validation: Bloom Filter (simplified to 2 checks)
	// ValidateLogsBloomEmptiness: if logs exist, bloom must not be zero; if bloom is non-zero, logs must exist
	ValidateLogsBloomEmptiness *bool `yaml:"validateLogsBloomEmptiness,omitempty" json:"validateLogsBloomEmptiness"`
	// ValidateLogsBloomMatch: recalculate bloom from logs and verify it matches the provided bloom
	ValidateLogsBloomMatch *bool `yaml:"validateLogsBloomMatch,omitempty" json:"validateLogsBloomMatch"`

	// Validation: Receipt-to-Transaction Cross-Validation (requires GroundTruthTransactions in library-mode)
	ValidateReceiptTransactionMatch *bool `yaml:"validateReceiptTransactionMatch,omitempty" json:"validateReceiptTransactionMatch"`
	ValidateContractCreation        *bool `yaml:"validateContractCreation,omitempty" json:"validateContractCreation"`

	// Validation: numeric checks
	ReceiptsCountExact   *int64 `yaml:"receiptsCountExact,omitempty" json:"receiptsCountExact"`
	ReceiptsCountAtLeast *int64 `yaml:"receiptsCountAtLeast,omitempty" json:"receiptsCountAtLeast"`

	// Validation: Expected Ground Truths
	ValidationExpectedBlockHash   *string `yaml:"validationExpectedBlockHash,omitempty" json:"validationExpectedBlockHash"`
	ValidationExpectedBlockNumber *int64  `yaml:"validationExpectedBlockNumber,omitempty" json:"validationExpectedBlockNumber"`
}

func (d *DirectiveDefaultsConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type raw DirectiveDefaultsConfig
	if err := unmarshal((*raw)(d)); err != nil {
		return err
	}
	// Normalize SkipCacheRead: accept both bool and string from YAML, store as string.
	if d.SkipCacheRead != nil {
		d.SkipCacheRead = fmt.Sprintf("%v", d.SkipCacheRead)
	}
	return nil
}

func (d *DirectiveDefaultsConfig) UnmarshalJSON(data []byte) error {
	type raw DirectiveDefaultsConfig
	if err := sonic.Unmarshal(data, (*raw)(d)); err != nil {
		return err
	}
	if d.SkipCacheRead != nil {
		d.SkipCacheRead = fmt.Sprintf("%v", d.SkipCacheRead)
	}
	return nil
}

type EvmNetworkConfig struct {
	ChainId                     int64               `yaml:"chainId" json:"chainId"`
	FallbackFinalityDepth       int64               `yaml:"fallbackFinalityDepth,omitempty" json:"fallbackFinalityDepth"`
	FallbackStatePollerDebounce Duration            `yaml:"fallbackStatePollerDebounce,omitempty" json:"fallbackStatePollerDebounce" tstype:"Duration"`
	Integrity                   *EvmIntegrityConfig `yaml:"integrity,omitempty" json:"integrity"`

	// ServedTip configures how the network derives the "latest"/"finalized"
	// block it advertises to clients (and enforces via block-availability).
	// Nil or disabled selects the default max mode (MAX latest across eligible
	// upstreams); set Enabled to opt into the cluster-min tip. See
	// EvmServedTipConfig.
	ServedTip                  *EvmServedTipConfig `yaml:"servedTip,omitempty" json:"servedTip,omitempty"`
	GetLogsMaxAllowedRange     int64               `yaml:"getLogsMaxAllowedRange,omitempty" json:"getLogsMaxAllowedRange"`
	GetLogsMaxAllowedAddresses int64               `yaml:"getLogsMaxAllowedAddresses,omitempty" json:"getLogsMaxAllowedAddresses"`
	GetLogsMaxAllowedTopics    int64               `yaml:"getLogsMaxAllowedTopics,omitempty" json:"getLogsMaxAllowedTopics"`
	GetLogsSplitOnError        *bool               `yaml:"getLogsSplitOnError,omitempty" json:"getLogsSplitOnError"`
	GetLogsSplitConcurrency    int                 `yaml:"getLogsSplitConcurrency,omitempty" json:"getLogsSplitConcurrency"`
	// TraceFilterSplitOnError controls reactive splitting for trace_filter and
	// arbtrace_filter requests when the upstream returns a range-too-large error.
	// Nil disables the feature.
	TraceFilterSplitOnError *bool `yaml:"traceFilterSplitOnError,omitempty" json:"traceFilterSplitOnError"`
	// TraceFilterSplitConcurrency caps in-flight sub-requests when a trace_filter
	// or arbtrace_filter request is split. Zero falls back to 10.
	TraceFilterSplitConcurrency int `yaml:"traceFilterSplitConcurrency,omitempty" json:"traceFilterSplitConcurrency"`
	// EnforceBlockAvailability controls whether the network should enforce per-upstream
	// block availability bounds (upper/lower) for methods by default. Method-level config may override.
	// When nil or true, enforcement is enabled.
	EnforceBlockAvailability *bool `yaml:"enforceBlockAvailability,omitempty" json:"enforceBlockAvailability,omitempty"`

	// MaxRetryableBlockDistance controls the maximum block distance for which an upstream
	// block unavailability error is considered retryable. If the requested block is within
	// this distance from the upstream's latest block, the error is retryable (upstream may catch up).
	// If the distance is larger, the error is not retryable (upstream is too far behind).
	// Default: 128 blocks.
	MaxRetryableBlockDistance *int64 `yaml:"maxRetryableBlockDistance,omitempty" json:"maxRetryableBlockDistance,omitempty"`

	// MarkEmptyAsErrorMethods lists methods for which an empty/null result from an upstream
	// should be treated as a "missing data" error, triggering retry on other upstreams.
	// This is useful for point-lookups (blocks, transactions, receipts, traces) where an
	// empty result likely means the upstream hasn't indexed that data yet.
	// Default includes common point-lookup methods like eth_getBlockByNumber, eth_getTransactionByHash, etc.
	MarkEmptyAsErrorMethods []string `yaml:"markEmptyAsErrorMethods,omitempty" json:"markEmptyAsErrorMethods,omitempty"`

	// DynamicBlockTimeDebounceMultiplier scales the EMA-estimated block time to derive
	// the debounce interval for block polling. A value of 0.7 means debounce = 70% of
	// the estimated block time, preferring fresher data at the cost of slightly more
	// polling. Lower values reduce staleness risk; higher values reduce RPC calls.
	// Default: 0.7 (30% under the estimated block time).
	DynamicBlockTimeDebounceMultiplier *float64 `yaml:"dynamicBlockTimeDebounceMultiplier,omitempty" json:"dynamicBlockTimeDebounceMultiplier,omitempty"`

	// BlockUnavailableDelayMultiplier scales the EMA-estimated block time to derive the
	// retry delay when the requested data isn't available yet (ErrUpstreamBlockUnavailable
	// or an empty/missing-data point-lookup). When the dynamic block time is known, the
	// delay is blockTime * this multiplier. Falls back to the static
	// RetryPolicyConfig.EmptyResultDelay when block time is not yet available. Default: 1.0.
	BlockUnavailableDelayMultiplier *float64 `yaml:"blockUnavailableDelayMultiplier,omitempty" json:"blockUnavailableDelayMultiplier,omitempty"`

	// IdempotentTransactionBroadcast enables idempotency handling for eth_sendRawTransaction.
	// When enabled (default), "already known" and verified "nonce too low" errors are converted
	// to success responses with the transaction hash. This allows failsafe policies (retry/hedge)
	// to work safely with transaction broadcasting.
	// Set to false to disable this behavior and return raw upstream errors.
	IdempotentTransactionBroadcast *bool `yaml:"idempotentTransactionBroadcast,omitempty" json:"idempotentTransactionBroadcast,omitempty"`

	// EmptyResultConfidence sets how confirmed a concrete numeric block must be for an
	// empty/null point-lookup result to be treated as retryable missing-data, versus a
	// truthful "not yet produced/confirmed" empty returned without retrying. Applies to
	// MarkEmptyAsErrorMethods when the RetryEmpty directive is on; tags and block-hash
	// lookups never qualify, and it fails open when the head is unknown.
	//   - blockHead (default): retry empties for blocks at/below the latest head; a
	//     block above the head isn't produced yet → return the empty truthfully.
	//   - finalizedBlock: stricter — only retry empties for blocks at/below the
	//     finalized head; an unfinalized block's empty is treated as not-yet-confirmed.
	EmptyResultConfidence AvailbilityConfidence `yaml:"emptyResultConfidence,omitempty" json:"emptyResultConfidence,omitempty"`

	// Deprecated: replaced by EmptyResultConfidence (blockHead). Retained as a yaml-only
	// key so existing configs keep loading; SetDefaults warns and ignores it. The old
	// numeric distance band is gone — use emptyResultConfidence instead.
	MaxFutureBlockRetryDistance *int64 `yaml:"maxFutureBlockRetryDistance,omitempty" json:"-"`
}

// EvmServedTipConfig controls how the network derives the "latest"/"finalized"
// block it advertises (and enforces) from its upstreams.
//
// In the default max mode the served tip is the MAX latest block across eligible
// non-syncing upstreams — which can advertise a block only the single most-ahead
// upstream has, causing "block not found" churn when requests route to a
// slightly-behind upstream. When a tag is listed in EnabledFor, that tag's
// served value is instead the freshest block a strict MAJORITY of the eligible
// upstreams already have, so interpolated requests land on upstreams that can
// serve the advertised block.
type EvmServedTipConfig struct {
	// EnabledFor lists the block tags whose served value uses the cluster-min tip
	// instead of the default max. Valid entries: "latest" and "finalized" (the
	// "safe" tag follows "finalized"). Empty selects the max mode for all tags.
	EnabledFor []string `yaml:"enabledFor,omitempty" json:"enabledFor,omitempty"`

	// Deprecated: ClusterDelta configured the former cluster-based picker and is
	// ignored — the majority order statistic needs no tuning. Kept only so
	// existing configs keep parsing.
	ClusterDelta int64 `yaml:"clusterDelta,omitempty" json:"clusterDelta,omitempty"`

	// GuaranteedMethods lists method name patterns (glob; e.g. "trace_*",
	// "debug_traceBlockByNumber") whose supporting-upstream subset must be able to
	// serve the advertised latest. For a request on a matching method, "latest"
	// resolves against the majority of only the upstreams that support it
	// (membership auto-detected via ShouldHandleMethod — no per-upstream config).
	// Empty means only the global (all-eligible) majority is computed.
	GuaranteedMethods []string `yaml:"guaranteedMethods,omitempty" json:"guaranteedMethods,omitempty"`
}

// ServedTipEnabledFor reports whether the majority served tip is enabled for
// the given block axis ("latest" or "finalized"). The "safe" tag resolves to the
// finalized axis, so listing "safe" in EnabledFor enables it for "finalized".
// Anything not listed uses the default max mode. Nil-receiver safe.
func (c *EvmNetworkConfig) ServedTipEnabledFor(tag string) bool {
	if c == nil || c.ServedTip == nil {
		return false
	}
	for _, t := range c.ServedTip.EnabledFor {
		if strings.EqualFold(t, tag) {
			return true
		}
		// "safe" resolves to the finalized axis.
		if strings.EqualFold(tag, "finalized") && strings.EqualFold(t, "safe") {
			return true
		}
	}
	return false
}

// EvmIntegrityConfig is deprecated. Use DirectiveDefaultsConfig for validation settings.
type EvmIntegrityConfig struct {
	// @deprecated: use DirectiveDefaults.EnforceHighestBlock
	EnforceHighestBlock *bool `yaml:"enforceHighestBlock,omitempty" json:"enforceHighestBlock"`
	// @deprecated: use DirectiveDefaults.EnforceGetLogsBlockRange
	EnforceGetLogsBlockRange *bool `yaml:"enforceGetLogsBlockRange,omitempty" json:"enforceGetLogsBlockRange"`
	// @deprecated: use DirectiveDefaults.EnforceNonNullTaggedBlocks
	EnforceNonNullTaggedBlocks *bool `yaml:"enforceNonNullTaggedBlocks,omitempty" json:"enforceNonNullTaggedBlocks"`
}

// EvalScope picks the grain at which the selection policy evaluates AND
// at which the health tracker stores per-upstream metrics. One knob
// covers what `evalPerMethod` + `evalPerFinality` used to cover as two
// bools — and pulls the tracker's metric grain in lockstep so a
// predicate like `errorRateAbove(0.5)` in a (method, finality)-grained
// slot sees genuinely (method, finality)-specific error rate, not the
// per-method aggregate.
//
// Values are kebab-case strings so they round-trip through YAML / JSON
// / Go enum literals cleanly. The TS SDK exports the same names in
// CAPITAL_SNAKE_CASE with these same string values, so `evalScope:
// NETWORK` in a TS config and `evalScope: network` in a YAML config
// produce identical Go state.
type EvalScope string

const (
	// EvalScopeNetwork — single slot per network. Methods + finalities
	// share. Default. Lowest cardinality + lowest tracker memory.
	EvalScopeNetwork EvalScope = "network"
	// EvalScopeNetworkMethod — slot per (network, method). Finalities
	// share. Useful when one upstream is fast on `eth_call` but slow on
	// `trace_filter`.
	EvalScopeNetworkMethod EvalScope = "network-method"
	// EvalScopeNetworkFinality — slot per (network, finality). Methods
	// share. Useful when realtime reads weight freshness differently
	// from finalized reads.
	EvalScopeNetworkFinality EvalScope = "network-finality"
	// EvalScopeNetworkMethodFinality — slot per (network, method,
	// finality). Most granular routing. Cardinality scales linearly;
	// each slot's ticker only spins up after the first request for
	// that bucket lands.
	EvalScopeNetworkMethodFinality EvalScope = "network-method-finality"
)

// SelectionPolicyConfig declares the per-network upstream selection policy.
//
// The eval function is JavaScript that receives `upstreams` and `ctx` and
// returns the ordered list of upstreams that should serve traffic for the
// network/method scope. The chainable std-lib (see internal/policy/stdlib)
// provides the building blocks (sortByScore, removeByLag, stickyPrimary,
// probeExcluded, etc.) — see specs/selection-policy/feature.md.
type SelectionPolicyConfig struct {
	EvalInterval Duration `yaml:"evalInterval,omitempty" json:"evalInterval" tstype:"Duration"`
	// EvalScope picks the slot grain — and the matching tracker grain
	// (see `EvalScope` doc). Default `network` (one slot per network).
	// Tighter scopes let predicates like `errorRateAbove(0.5)` see
	// genuinely (method, finality)-specific health.
	EvalScope EvalScope `yaml:"evalScope,omitempty" json:"evalScope" tstype:"EvalScope | \"network\" | \"network-method\" | \"network-finality\" | \"network-method-finality\""`
	// EvalPerMethod is a config-load-time alias that translates to
	// EvalScope. Pointer-typed so SetDefaults can distinguish "key
	// absent" (nil) from "key explicitly false". Niled out after
	// translation — the engine + downstream code only ever consult
	// EvalScope. Kept out of the public TS surface (`tstype:"-"`); the
	// canonical knob is `evalScope`.
	EvalPerMethod *bool `yaml:"evalPerMethod,omitempty" json:"evalPerMethod,omitempty" tstype:"-"`
	// EvalPerFinality — same shape and translation behavior as
	// EvalPerMethod, for the finality axis.
	EvalPerFinality *bool    `yaml:"evalPerFinality,omitempty" json:"evalPerFinality,omitempty" tstype:"-"`
	EvalTimeout     Duration `yaml:"evalTimeout,omitempty" json:"evalTimeout" tstype:"Duration"`
	// EvalFunc is the per-tick evaluation function. In YAML it's a JS
	// source string; in TS configs it's a real arrow function compiled
	// into `CompiledProgram` at config load.
	// Signature: `(upstreams, ctx) => Upstream[]`.
	// See specs/selection-policy/feature.md for the stdlib reference.
	EvalFunc string `yaml:"evalFunc,omitempty" json:"evalFunc" tstype:"SelectionPolicyEvalFunction | string"`

	// DisableTickerForTest skips spawning the per-slot ticker goroutine.
	// Tests that don't need background re-eval set this to avoid
	// accumulating goroutines across hundreds of sub-tests + the per-tick
	// JS overhead that pushes the race-detector CI past budget. Tests
	// that need to step the engine call `policy.TickForTest` instead.
	DisableTickerForTest bool `yaml:"-" json:"-"`

	// CompiledProgram is set by SetDefaults/Validate; never marshalled.
	CompiledProgram *sobek.Program `yaml:"-" json:"-"`
	// EvalFuncOriginal preserves the source for diagnostics + tooling.
	EvalFuncOriginal string `yaml:"-" json:"-"`

	// LegacySelectionPolicy stashes deprecated `evalFunction` / resample*
	// keys captured at config-load time. The legacy translator wraps
	// the legacy eval function into a modern `Eval` and emits warnings.
	// Never serialized.
	LegacySelectionPolicy *LegacySelectionPolicyFields `yaml:"-" json:"-"`
}

// LegacySelectionPolicyFields mirrors the deprecated keys that used to
// live on SelectionPolicyConfig. None survive translation.
type LegacySelectionPolicyFields struct {
	EvalFunction     string   `yaml:"evalFunction,omitempty"`
	ResampleExcluded bool     `yaml:"resampleExcluded,omitempty"`
	ResampleInterval Duration `yaml:"resampleInterval,omitempty"`
	ResampleCount    int      `yaml:"resampleCount,omitempty"`
}

// UnmarshalYAML captures legacy selectionPolicy keys (`evalFunction`,
// resampleExcluded, resampleInterval, resampleCount) into
// LegacySelectionPolicy for the post-decode translator hook.
func (s *SelectionPolicyConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type canonicalSelectionPolicyConfig SelectionPolicyConfig
	type shadow struct {
		canonicalSelectionPolicyConfig `yaml:",inline"`
		LegacySelectionPolicyFields    `yaml:",inline"`
	}
	var sh shadow
	if err := unmarshal(&sh); err != nil {
		return err
	}
	*s = SelectionPolicyConfig(sh.canonicalSelectionPolicyConfig)
	if sh.LegacySelectionPolicyFields != (LegacySelectionPolicyFields{}) {
		lf := sh.LegacySelectionPolicyFields
		s.LegacySelectionPolicy = &lf
	}
	return nil
}

type AuthType string

const (
	AuthTypeSecret   AuthType = "secret"
	AuthTypeDatabase AuthType = "database"
	AuthTypeJwt      AuthType = "jwt"
	AuthTypeSiwe     AuthType = "siwe"
	AuthTypeNetwork  AuthType = "network"
)

type AuthConfig struct {
	Strategies []*AuthStrategyConfig `yaml:"strategies" json:"strategies" tstype:"TsAuthStrategyConfig[]"`
}

type AuthStrategyConfig struct {
	IgnoreMethods   []string `yaml:"ignoreMethods,omitempty" json:"ignoreMethods,omitempty"`
	AllowMethods    []string `yaml:"allowMethods,omitempty" json:"allowMethods,omitempty"`
	RateLimitBudget string   `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget,omitempty"`

	Type     AuthType                `yaml:"type" json:"type" tstype:"TsAuthType"`
	Network  *NetworkStrategyConfig  `yaml:"network,omitempty" json:"network,omitempty"`
	Secret   *SecretStrategyConfig   `yaml:"secret,omitempty" json:"secret,omitempty"`
	Database *DatabaseStrategyConfig `yaml:"database,omitempty" json:"database,omitempty"`
	Jwt      *JwtStrategyConfig      `yaml:"jwt,omitempty" json:"jwt,omitempty"`
	Siwe     *SiweStrategyConfig     `yaml:"siwe,omitempty" json:"siwe,omitempty"`
}

type SecretStrategyConfig struct {
	Id    string `yaml:"id" json:"id"`
	Value string `yaml:"value" json:"value"`
	// RateLimitBudget, if set, is applied to the authenticated user from this strategy
	RateLimitBudget string `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget,omitempty"`
}

// custom json marshaller to redact the secret value
func (s *SecretStrategyConfig) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]string{
		"value": "REDACTED",
	})
}

func (s *SecretStrategyConfig) MarshalYAML() (interface{}, error) {
	return map[string]string{
		"id":              s.Id,
		"value":           "REDACTED",
		"rateLimitBudget": s.RateLimitBudget,
	}, nil
}

type DatabaseStrategyConfig struct {
	Connector *ConnectorConfig             `yaml:"connector" json:"connector"`
	Cache     *DatabaseStrategyCacheConfig `yaml:"cache,omitempty" json:"cache,omitempty"`
	Retry     *DatabaseRetryConfig         `yaml:"retry,omitempty" json:"retry,omitempty"`
	FailOpen  *DatabaseFailOpenConfig      `yaml:"failOpen,omitempty" json:"failOpen,omitempty"`
	MaxWait   Duration                     `yaml:"maxWait,omitempty" json:"maxWait" tstype:"Duration"`
}

type DatabaseStrategyCacheConfig struct {
	TTL         *time.Duration `yaml:"ttl,omitempty" json:"ttl,omitempty"`
	MaxSize     *int64         `yaml:"maxSize,omitempty" json:"maxSize,omitempty"`
	MaxCost     *int64         `yaml:"maxCost,omitempty" json:"maxCost,omitempty"`
	NumCounters *int64         `yaml:"numCounters,omitempty" json:"numCounters,omitempty"`
}

type DatabaseRetryConfig struct {
	MaxAttempts int      `yaml:"maxAttempts,omitempty" json:"maxAttempts"`
	BaseBackoff Duration `yaml:"baseBackoff,omitempty" json:"baseBackoff" tstype:"Duration"`
}

type DatabaseFailOpenConfig struct {
	Enabled         bool   `yaml:"enabled" json:"enabled"`
	UserId          string `yaml:"userId,omitempty" json:"userId"`
	RateLimitBudget string `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget,omitempty"`
}

type JwtStrategyConfig struct {
	AllowedIssuers                  []string            `yaml:"allowedIssuers" json:"allowedIssuers"`
	AllowedAudiences                []string            `yaml:"allowedAudiences" json:"allowedAudiences"`
	AllowedAlgorithms               []string            `yaml:"allowedAlgorithms" json:"allowedAlgorithms"`
	RequiredClaims                  []string            `yaml:"requiredClaims" json:"requiredClaims"`
	ClaimMatchers                   map[string][]string `yaml:"claimMatchers,omitempty" json:"claimMatchers,omitempty"`
	VerificationKeys                map[string]string   `yaml:"verificationKeys,omitempty" json:"verificationKeys,omitempty"`
	VerificationJwksUrl             string              `yaml:"verificationJwksUrl,omitempty" json:"verificationJwksUrl,omitempty"`
	VerificationJwksRefreshInterval Duration            `yaml:"verificationJwksRefreshInterval,omitempty" json:"verificationJwksRefreshInterval" tstype:"Duration"`
	// Skipping TLS verification is an explicit operator opt-in for the JWKS
	// fetch, not a hardcoded bypass.
	//nolint:gosec
	VerificationJwksTlsInsecureSkipVerify bool `yaml:"verificationJwksTlsInsecureSkipVerify,omitempty" json:"verificationJwksTlsInsecureSkipVerify,omitempty"`
	// RateLimitBudgetClaimName is the JWT claim name that, if present,
	// will be used to set the per-user RateLimitBudget override.
	// Defaults to "rlm".
	RateLimitBudgetClaimName string `yaml:"rateLimitBudgetClaimName,omitempty" json:"rateLimitBudgetClaimName,omitempty"`
}

type SiweStrategyConfig struct {
	AllowedDomains []string `yaml:"allowedDomains" json:"allowedDomains"`
	// RateLimitBudget, if set, is applied to the authenticated user
	RateLimitBudget string `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget,omitempty"`
}

type NetworkStrategyConfig struct {
	AllowedIPs     []string `yaml:"allowedIPs" json:"allowedIPs"`
	AllowedCIDRs   []string `yaml:"allowedCIDRs" json:"allowedCIDRs"`
	AllowLocalhost bool     `yaml:"allowLocalhost" json:"allowLocalhost"`
	TrustedProxies []string `yaml:"trustedProxies" json:"trustedProxies"`
	// RateLimitBudget, if set, is applied to the authenticated user (client IP)
	RateLimitBudget string `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget,omitempty"`
	IPAsUser        bool   `yaml:"ipAsUser,omitempty" json:"ipAsUser,omitempty"`
}

type LabelMode string

const (
	ErrorLabelModeVerbose LabelMode = "verbose"
	ErrorLabelModeCompact LabelMode = "compact"
)

type MetricsConfig struct {
	Enabled          *bool     `yaml:"enabled" json:"enabled"`
	ListenV4         *bool     `yaml:"listenV4" json:"listenV4"`
	HostV4           *string   `yaml:"hostV4" json:"hostV4"`
	ListenV6         *bool     `yaml:"listenV6" json:"listenV6"`
	HostV6           *string   `yaml:"hostV6" json:"hostV6"`
	Port             *int      `yaml:"port" json:"port"`
	ErrorLabelMode   LabelMode `yaml:"errorLabelMode,omitempty" json:"errorLabelMode"`
	HistogramBuckets string    `yaml:"histogramBuckets,omitempty" json:"histogramBuckets"`

	// HistogramDropLabels removes these labels from every histogram. Counters
	// and gauges are unaffected. Useful to cap per-instance /metrics response
	// size when high-cardinality labels (e.g. "user") push a scrape past the
	// managed scraper's sample/body limits.
	HistogramDropLabels []string `yaml:"histogramDropLabels,omitempty" json:"histogramDropLabels,omitempty"`

	// HistogramLabelOverrides re-adds labels for specific histograms even if
	// they appear in HistogramDropLabels. Key is the metric Name (without the
	// "erpc_" namespace prefix), e.g. "network_request_duration_seconds".
	// Value is the list of label names to keep for that metric.
	HistogramLabelOverrides map[string][]string `yaml:"histogramLabelOverrides,omitempty" json:"histogramLabelOverrides,omitempty"`
}

// GetProjectConfig returns the project configuration by the specified project ID.
func (c *Config) GetProjectConfig(projectId string) *ProjectConfig {
	for _, project := range c.Projects {
		if project.Id == projectId {
			return project
		}
	}

	return nil
}

func (c *RateLimitRuleConfig) MarshalZerologObject(e *zerolog.Event) {
	e.Str("method", c.Method).
		Uint("maxCount", uint(c.MaxCount)).
		Str("period", c.Period.String()).
		Str("waitTimeMs", fmt.Sprintf("%d", c.WaitTime))
}

// RateLimitStoreConfig defines where rate limit counters are stored
type RateLimitStoreConfig struct {
	Driver         string                `yaml:"driver" json:"driver"` // "redis" | "memory"
	Redis          *RedisConnectorConfig `yaml:"redis,omitempty" json:"redis,omitempty"`
	CacheKeyPrefix string                `yaml:"cacheKeyPrefix,omitempty" json:"cacheKeyPrefix"`
	NearLimitRatio float32               `yaml:"nearLimitRatio,omitempty" json:"nearLimitRatio"`
}

func (c *NetworkConfig) NetworkId() string {
	if c.Architecture == "" || c.Evm == nil {
		return ""
	}

	switch c.Architecture {
	case "evm":
		return util.EvmNetworkId(c.Evm.ChainId)
	default:
		return ""
	}
}

// tsFunctionSentinelPrefix marks a SelectionPolicy.EvalFunc whose actual
// implementation lives as a real sobek function in `globalThis.__erpcFns`
// (populated by running `Config.UserScript` inside the policy-engine pool
// runtime). The suffix is the function's lookup id. Sentinels NEVER hit
// `sobek.Compile` and the function is never stringified — when the engine
// needs to evaluate, it pulls the runtime-native function out of the
// registry and calls it directly. This preserves closures + module-level
// helpers that the user wrote in the TS config alongside the function.
const tsFunctionSentinelPrefix = "__ts_fn__:"

// tsLoaderWalker is JS that runs INSIDE the user script. After the user's
// `createConfig(...)` builds the default export, the walker:
//
//   - assigns a stable sequential id to each function-typed leaf
//     in the default export's object tree;
//   - registers that function on `globalThis.__erpcFns[id]` so the
//     engine can look it up natively in this runtime;
//   - stamps `fn.__erpcFnId = id` on the function value so the
//     LOAD-time JSON.stringify replacer can substitute the sentinel
//     string `"__ts_fn__:<id>"` into the serialized form (which then
//     becomes the value of `SelectionPolicyConfig.EvalFunc` in Go).
//
// The walk is order-deterministic, so the same user script produces the
// same ids in every pool runtime that subsequently runs it. That's what
// keeps the load-time sentinel ids and the pool-runtime registry ids in
// lockstep without sharing state across runtimes.
const tsLoaderWalker = `
;(function () {
  if (!globalThis.__erpcFns) globalThis.__erpcFns = {};
  var __counter = 0;
  function walk(node) {
    if (!node) return;
    if (Array.isArray(node)) {
      for (var i = 0; i < node.length; i++) walk(node[i]);
      return;
    }
    if (typeof node !== 'object') return;
    for (var k in node) {
      if (!Object.prototype.hasOwnProperty.call(node, k)) continue;
      var v = node[k];
      if (typeof v === 'function') {
        var id = 'fn_' + (__counter++);
        globalThis.__erpcFns[id] = v;
        try { Object.defineProperty(v, '__erpcFnId', { value: id, enumerable: false, configurable: true, writable: false }); } catch (_) {}
      } else if (v && typeof v === 'object') {
        walk(v);
      }
    }
  }
  if (typeof exports !== 'undefined' && exports && exports.default) {
    walk(exports.default);
  }
})();
`

// loadConfigFromTypescript compiles the user's TS/JS config and produces
// (a) the decoded Config struct, with each function-typed leaf replaced
// by a `__ts_fn__:<id>` sentinel string in the corresponding string field
// (e.g. `SelectionPolicyConfig.EvalFunc`); and (b) a compiled *sobek.Program
// of the user's whole script, attached to `cfg.UserScript`.
//
// The policy-engine runtime pool runs `cfg.UserScript` once per acquired
// runtime (via the primer). That evaluates the user's helpers + imports
// natively in the runtime AND populates `globalThis.__erpcFns` with the
// real function values. At per-tick eval time, the engine looks up the
// function in that registry and invokes it directly — no `.toString()`,
// no `sobek.Compile`, no JSON-string round-trip.
//
// This means closures, imports, and module-level helpers in the user's
// TS file flow naturally into the evalFunc:
//
//	const weights = { hot: { errorRate: 8 }, cold: { errorRate: 4 } };
//	selectionPolicy: { evalFunc: (u, ctx) => u.sortByScore((u) => weights[u.id] || PREFER_FASTEST) }
//
// works as written, because `weights` exists in the same module scope
// as the function in every pool runtime.
//
// Non-function fields still flow through JSON+YAML so the existing
// `UnmarshalYAML` hooks (strict-schema validation, back-compat shadows
// for `routing:` / `group:` / `cohort:`) keep firing identically to the
// .yaml path.
func loadConfigFromTypescript(filename string) (*Config, error) {
	jsSource, err := CompileTypeScript(filename)
	if err != nil {
		return nil, err
	}

	// Append the walker so it runs in EVERY runtime that evaluates this
	// program — including the temp runtime used here AND each policy-
	// engine pool runtime later (via primer.RunProgram). Same source =
	// same deterministic id assignment, so the sentinel ids encoded into
	// the Config struct line up with the registry the pool builds.
	wrapped := jsSource + "\n" + tsLoaderWalker
	userScript, err := sobek.Compile(filename, wrapped, false)
	if err != nil {
		return nil, fmt.Errorf("compile ts config: %w", err)
	}

	// Run once in a temp runtime to (a) walk the default export and (b)
	// pull out the non-function fields. The temp runtime is discarded
	// after this; the pool will recreate the same state via the primer.
	runtime, err := NewRuntime()
	if err != nil {
		return nil, err
	}
	if _, err := runtime.VM().RunProgram(userScript); err != nil {
		return nil, err
	}

	defaultExport := runtime.Exports().Get("default")
	if defaultExport == nil || sobek.IsUndefined(defaultExport) || sobek.IsNull(defaultExport) {
		return nil, fmt.Errorf("config object must be default exported from TypeScript code AND must be the last statement in the file")
	}

	// JSON.stringify with a replacer that converts each function value
	// to its `__ts_fn__:<id>` sentinel string (the walker stamped
	// `__erpcFnId` on the function). NO `.toString()` is involved —
	// the function lives on as a real sobek value in `__erpcFns`, and
	// the sentinel is just a lookup key.
	if err := runtime.VM().GlobalObject().Set("__erpcConfigToMarshal", defaultExport); err != nil {
		return nil, fmt.Errorf("ts config marshal setup: %w", err)
	}
	defer func() {
		_ = runtime.VM().GlobalObject().Delete("__erpcConfigToMarshal")
	}()
	jsonValue, err := runtime.VM().RunString(`
		JSON.stringify(__erpcConfigToMarshal, (k, v) => {
			if (typeof v === 'function') {
				if (v.__erpcFnId) return ` + "`" + tsFunctionSentinelPrefix + `${v.__erpcFnId}` + "`" + `;
				// Function the walker didn't catch (orphaned reference,
				// inside an Array.prototype method, etc.). Fall through
				// to undefined so JSON.stringify omits it rather than
				// emitting a function-as-string blob.
				return undefined;
			}
			return v;
		})
	`)
	if err != nil {
		return nil, fmt.Errorf("ts config json-stringify: %w", err)
	}
	if jsonValue == nil || sobek.IsUndefined(jsonValue) || sobek.IsNull(jsonValue) {
		return nil, fmt.Errorf("config default export serialized to null/undefined")
	}
	jsonBytes := []byte(jsonValue.String())

	// yaml.v3 accepts JSON (it's a subset of YAML) AND runs all
	// UnmarshalYAML hooks during decode. Strict-decode so typos in TS
	// configs surface the same way they do in YAML configs.
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(jsonBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("ts config decode: %w", err)
	}

	// Attach the compiled program — the policy engine's pool primer will
	// run it once per acquired runtime to rebuild `__erpcFns` natively.
	cfg.UserScript = userScript

	return &cfg, nil
}

// IsTSFunctionSentinel reports whether a SelectionPolicyConfig.EvalFunc
// string carries a TS-loader sentinel pointing into `globalThis.__erpcFns`
// (as opposed to a YAML-style JS source string the engine should compile).
func IsTSFunctionSentinel(s string) bool {
	return strings.HasPrefix(s, tsFunctionSentinelPrefix)
}

// TSFunctionSentinelID extracts the `__erpcFns` lookup id from a sentinel
// EvalFunc value. Caller-checks `IsTSFunctionSentinel` first.
func TSFunctionSentinelID(s string) string {
	return strings.TrimPrefix(s, tsFunctionSentinelPrefix)
}
