package common

import (
	"bytes"
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
}

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

	err = cfg.SetDefaults(opts)
	if err != nil {
		return nil, err
	}

	err = cfg.Validate()
	if err != nil {
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
}

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
	TTL         Duration             `yaml:"ttl,omitempty" json:"ttl" tstype:"Duration"`
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
	Id         string                     `yaml:"id,omitempty" json:"id"`
	Driver     ConnectorDriverType        `yaml:"driver" json:"driver" tstype:"TsConnectorDriverType"`
	Memory     *MemoryConnectorConfig     `yaml:"memory,omitempty" json:"memory"`
	Redis      *RedisConnectorConfig      `yaml:"redis,omitempty" json:"redis"`
	DynamoDB   *DynamoDBConnectorConfig   `yaml:"dynamodb,omitempty" json:"dynamodb"`
	PostgreSQL *PostgreSQLConnectorConfig `yaml:"postgresql,omitempty" json:"postgresql"`
	Grpc       *GrpcConnectorConfig       `yaml:"grpc,omitempty" json:"grpc"`
	Mock       *MockConnectorConfig       `yaml:"-" json:"-"`
}

type GrpcConnectorConfig struct {
	Bootstrap  string            `yaml:"bootstrap,omitempty" json:"bootstrap"`
	Servers    []string          `yaml:"servers,omitempty" json:"servers"`
	Headers    map[string]string `yaml:"headers,omitempty" json:"headers"`
	GetTimeout Duration          `yaml:"getTimeout,omitempty" json:"getTimeout" tstype:"Duration"`
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
	Addr              string     `yaml:"addr,omitempty" json:"addr"`
	Username          string     `yaml:"username,omitempty" json:"username"`
	Password          string     `yaml:"password,omitempty" json:"-"`
	DB                int        `yaml:"db,omitempty" json:"db"`
	TLS               *TLSConfig `yaml:"tls,omitempty" json:"tls"`
	ConnPoolSize      int        `yaml:"connPoolSize,omitempty" json:"connPoolSize"`
	URI               string     `yaml:"uri" json:"uri"`
	InitTimeout       Duration   `yaml:"initTimeout,omitempty" json:"initTimeout" tstype:"Duration"`
	GetTimeout        Duration   `yaml:"getTimeout,omitempty" json:"getTimeout" tstype:"Duration"`
	SetTimeout        Duration   `yaml:"setTimeout,omitempty" json:"setTimeout" tstype:"Duration"`
	LockRetryInterval Duration   `yaml:"lockRetryInterval,omitempty" json:"lockRetryInterval" tstype:"Duration"`
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
	})
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
	ConnectionUri string   `yaml:"connectionUri" json:"connectionUri"`
	Table         string   `yaml:"table" json:"table"`
	MinConns      int32    `yaml:"minConns,omitempty" json:"minConns"`
	MaxConns      int32    `yaml:"maxConns,omitempty" json:"maxConns"`
	InitTimeout   Duration `yaml:"initTimeout,omitempty" json:"initTimeout" tstype:"Duration"`
	GetTimeout    Duration `yaml:"getTimeout,omitempty" json:"getTimeout" tstype:"Duration"`
	SetTimeout    Duration `yaml:"setTimeout,omitempty" json:"setTimeout" tstype:"Duration"`
}

func (p *PostgreSQLConnectorConfig) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]string{
		"connectionUri": util.RedactEndpoint(p.ConnectionUri),
		"table":         p.Table,
		"minConns":      fmt.Sprintf("%d", p.MinConns),
		"maxConns":      fmt.Sprintf("%d", p.MaxConns),
		"initTimeout":   p.InitTimeout.String(),
		"getTimeout":    p.GetTimeout.String(),
		"setTimeout":    p.SetTimeout.String(),
	})
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

type ProjectConfig struct {
	Id                     string            `yaml:"id" json:"id"`
	Auth                   *AuthConfig       `yaml:"auth,omitempty" json:"auth"`
	CORS                   *CORSConfig       `yaml:"cors,omitempty" json:"cors"`
	Providers              []*ProviderConfig `yaml:"providers,omitempty" json:"providers"`
	UpstreamDefaults       *UpstreamConfig   `yaml:"upstreamDefaults,omitempty" json:"upstreamDefaults"`
	Upstreams              []*UpstreamConfig `yaml:"upstreams,omitempty" json:"upstreams"`
	NetworkDefaults        *NetworkDefaults  `yaml:"networkDefaults,omitempty" json:"networkDefaults"`
	Networks               []*NetworkConfig  `yaml:"networks,omitempty" json:"networks"`
	RateLimitBudget        string            `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget"`
	ScoreMetricsWindowSize Duration          `yaml:"scoreMetricsWindowSize,omitempty" json:"scoreMetricsWindowSize" tstype:"Duration"`
	ScoreRefreshInterval   Duration          `yaml:"scoreRefreshInterval,omitempty" json:"scoreRefreshInterval" tstype:"Duration"`
	// ScoreMetricsMode controls label cardinality for upstream score metrics for this project.
	// Allowed values:
	// - "compact": emit compact series by setting upstream and category labels to 'n/a'
	// - "detailed": emit full project/vendor/network/upstream/category series
	ScoreMetricsMode      string                              `yaml:"scoreMetricsMode,omitempty" json:"scoreMetricsMode"`
	DeprecatedHealthCheck *DeprecatedProjectHealthCheckConfig `yaml:"healthCheck,omitempty" json:"healthCheck"`
	// Configure user agent tracking at the project level
	UserAgentMode UserAgentTrackingMode `yaml:"userAgentMode,omitempty" json:"userAgentMode"`
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

type UpstreamConfig struct {
	Id                           string                   `yaml:"id,omitempty" json:"id"`
	Type                         UpstreamType             `yaml:"type,omitempty" json:"type" tstype:"TsUpstreamType"`
	Group                        string                   `yaml:"group,omitempty" json:"group"`
	VendorName                   string                   `yaml:"vendorName,omitempty" json:"vendorName"`
	Endpoint                     string                   `yaml:"endpoint,omitempty" json:"endpoint"`
	Evm                          *EvmUpstreamConfig       `yaml:"evm,omitempty" json:"evm"`
	JsonRpc                      *JsonRpcUpstreamConfig   `yaml:"jsonRpc,omitempty" json:"jsonRpc"`
	IgnoreMethods                []string                 `yaml:"ignoreMethods,omitempty" json:"ignoreMethods"`
	AllowMethods                 []string                 `yaml:"allowMethods,omitempty" json:"allowMethods"`
	AutoIgnoreUnsupportedMethods *bool                    `yaml:"autoIgnoreUnsupportedMethods,omitempty" json:"autoIgnoreUnsupportedMethods"`
	Failsafe                     []*FailsafeConfig        `yaml:"failsafe,omitempty" json:"failsafe"`
	RateLimitBudget              string                   `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget"`
	RateLimitAutoTune            *RateLimitAutoTuneConfig `yaml:"rateLimitAutoTune,omitempty" json:"rateLimitAutoTune"`
	Routing                      *RoutingConfig           `yaml:"routing,omitempty" json:"routing"`
	Shadow                       *ShadowUpstreamConfig    `yaml:"shadow,omitempty" json:"shadow"`
}

// UnmarshalYAML provides backward compatibility for old single failsafe object format
func (u *UpstreamConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Define a type alias to avoid recursion
	type rawUpstreamConfig UpstreamConfig
	raw := (*rawUpstreamConfig)(u)

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
	type oldUpstreamConfig struct {
		Id                           string                   `yaml:"id,omitempty"`
		Type                         UpstreamType             `yaml:"type,omitempty"`
		Group                        string                   `yaml:"group,omitempty"`
		VendorName                   string                   `yaml:"vendorName,omitempty"`
		Endpoint                     string                   `yaml:"endpoint,omitempty"`
		Evm                          *EvmUpstreamConfig       `yaml:"evm,omitempty"`
		JsonRpc                      *JsonRpcUpstreamConfig   `yaml:"jsonRpc,omitempty"`
		IgnoreMethods                []string                 `yaml:"ignoreMethods,omitempty"`
		AllowMethods                 []string                 `yaml:"allowMethods,omitempty"`
		AutoIgnoreUnsupportedMethods *bool                    `yaml:"autoIgnoreUnsupportedMethods,omitempty"`
		Failsafe                     *FailsafeConfig          `yaml:"failsafe,omitempty"`
		RateLimitBudget              string                   `yaml:"rateLimitBudget,omitempty"`
		RateLimitAutoTune            *RateLimitAutoTuneConfig `yaml:"rateLimitAutoTune,omitempty"`
		Routing                      *RoutingConfig           `yaml:"routing,omitempty"`
		Shadow                       *ShadowUpstreamConfig    `yaml:"shadow,omitempty"`
	}

	var old oldUpstreamConfig
	if err := unmarshal(&old); err != nil {
		// If both formats fail, return the original error as it's likely more informative
		// about the actual problem (like invalid field names)
		return originalErr
	}

	// Convert old format to new format
	u.Id = old.Id
	u.Type = old.Type
	u.Group = old.Group
	u.VendorName = old.VendorName
	u.Endpoint = old.Endpoint
	u.Evm = old.Evm
	u.JsonRpc = old.JsonRpc
	u.IgnoreMethods = old.IgnoreMethods
	u.AllowMethods = old.AllowMethods
	u.AutoIgnoreUnsupportedMethods = old.AutoIgnoreUnsupportedMethods
	u.RateLimitBudget = old.RateLimitBudget
	u.RateLimitAutoTune = old.RateLimitAutoTune
	u.Routing = old.Routing
	u.Shadow = old.Shadow

	if old.Failsafe != nil {
		// Ensure MatchMethod has a default value for backward compatibility
		if old.Failsafe.MatchMethod == "" {
			old.Failsafe.MatchMethod = "*"
		}
		u.Failsafe = []*FailsafeConfig{old.Failsafe}
	}

	return nil
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
	if c.Routing != nil {
		copied.Routing = c.Routing.Copy()
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

type RoutingConfig struct {
	ScoreMultipliers     []*ScoreMultiplierConfig `yaml:"scoreMultipliers" json:"scoreMultipliers"`
	ScoreLatencyQuantile float64                  `yaml:"scoreLatencyQuantile,omitempty" json:"scoreLatencyQuantile"`
}

func (c *RoutingConfig) Copy() *RoutingConfig {
	if c == nil {
		return nil
	}

	copied := &RoutingConfig{}

	if c.ScoreMultipliers != nil {
		copied.ScoreMultipliers = make([]*ScoreMultiplierConfig, len(c.ScoreMultipliers))
		for i, multiplier := range c.ScoreMultipliers {
			copied.ScoreMultipliers[i] = multiplier.Copy()
		}
	}

	return copied
}

type ScoreMultiplierConfig struct {
	Network         string              `yaml:"network" json:"network"`
	Method          string              `yaml:"method" json:"method"`
	Finality        []DataFinalityState `yaml:"finality,omitempty" json:"finality,omitempty" tstype:"DataFinalityState[]"`
	Overall         *float64            `yaml:"overall" json:"overall"`
	ErrorRate       *float64            `yaml:"errorRate" json:"errorRate"`
	RespLatency     *float64            `yaml:"respLatency" json:"respLatency"`
	TotalRequests   *float64            `yaml:"totalRequests" json:"totalRequests"`
	ThrottledRate   *float64            `yaml:"throttledRate" json:"throttledRate"`
	BlockHeadLag    *float64            `yaml:"blockHeadLag" json:"blockHeadLag"`
	FinalizationLag *float64            `yaml:"finalizationLag" json:"finalizationLag"`
	Misbehaviors    *float64            `yaml:"misbehaviors" json:"misbehaviors"`
}

func (c *ScoreMultiplierConfig) Copy() *ScoreMultiplierConfig {
	if c == nil {
		return nil
	}
	copied := &ScoreMultiplierConfig{}
	*copied = *c
	// Deep copy the Finality array
	if c.Finality != nil {
		copied.Finality = make([]DataFinalityState, len(c.Finality))
		copy(copied.Finality, c.Finality)
	}
	return copied
}

func (u *UpstreamConfig) MarshalJSON() ([]byte, error) {
	type Alias UpstreamConfig
	return sonic.Marshal(&struct {
		Endpoint string `json:"endpoint"`
		*Alias
	}{
		Endpoint: util.RedactEndpoint(u.Endpoint),
		Alias:    (*Alias)(u),
	})
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

type EvmUpstreamConfig struct {
	ChainId                            int64                       `yaml:"chainId" json:"chainId"`
	StatePollerInterval                Duration                    `yaml:"statePollerInterval,omitempty" json:"statePollerInterval" tstype:"Duration"`
	StatePollerDebounce                Duration                    `yaml:"statePollerDebounce,omitempty" json:"statePollerDebounce" tstype:"Duration"`
	BlockAvailability                  *EvmBlockAvailabilityConfig `yaml:"blockAvailability,omitempty" json:"blockAvailability"`
	GetLogsAutoSplittingRangeThreshold int64                       `yaml:"getLogsAutoSplittingRangeThreshold,omitempty" json:"getLogsAutoSplittingRangeThreshold"`
	SkipWhenSyncing                    *bool                       `yaml:"skipWhenSyncing,omitempty" json:"skipWhenSyncing"`
	Integrity                          *UpstreamIntegrityConfig    `yaml:"integrity,omitempty" json:"integrity"`

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
	MaxAttempts           int                   `yaml:"maxAttempts" json:"maxAttempts"`
	Delay                 Duration              `yaml:"delay,omitempty" json:"delay" tstype:"Duration"`
	BackoffMaxDelay       Duration              `yaml:"backoffMaxDelay,omitempty" json:"backoffMaxDelay" tstype:"Duration"`
	BackoffFactor         float32               `yaml:"backoffFactor,omitempty" json:"backoffFactor"`
	Jitter                Duration              `yaml:"jitter,omitempty" json:"jitter" tstype:"Duration"`
	EmptyResultConfidence AvailbilityConfidence `yaml:"emptyResultConfidence,omitempty" json:"emptyResultConfidence"`
	EmptyResultIgnore     []string              `yaml:"emptyResultIgnore,omitempty" json:"emptyResultIgnore"`
	// EmptyResultMaxAttempts limits total attempts when retries are triggered due to empty responses.
	EmptyResultMaxAttempts int `yaml:"emptyResultMaxAttempts,omitempty" json:"emptyResultMaxAttempts"`
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

type TimeoutPolicyConfig struct {
	Duration Duration `yaml:"duration,omitempty" json:"duration" tstype:"Duration"`
}

func (c *TimeoutPolicyConfig) Copy() *TimeoutPolicyConfig {
	if c == nil {
		return nil
	}
	copied := &TimeoutPolicyConfig{}
	*copied = *c
	return copied
}

type HedgePolicyConfig struct {
	Delay    Duration `yaml:"delay,omitempty" json:"delay" tstype:"Duration"`
	MaxCount int      `yaml:"maxCount" json:"maxCount"`
	Quantile float64  `yaml:"quantile,omitempty" json:"quantile"`
	MinDelay Duration `yaml:"minDelay,omitempty" json:"minDelay" tstype:"Duration"`
	MaxDelay Duration `yaml:"maxDelay,omitempty" json:"maxDelay" tstype:"Duration"`
}

func (c *HedgePolicyConfig) Copy() *HedgePolicyConfig {
	if c == nil {
		return nil
	}
	copied := &HedgePolicyConfig{}
	*copied = *c
	return copied
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

func (p RateLimitPeriod) MarshalJSON() ([]byte, error) {
	return SonicCfg.Marshal(p.String())
}

// Backward-compat: accept Go duration strings (e.g., 1s, 1m, 1h, 24h, 7d, 30d, 365d) and map to enum.
func (p *RateLimitPeriod) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Try as string (enum name)
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
	// Try as integer enum
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
	// Not a string → invalid for our schema
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

type DeprecatedProjectHealthCheckConfig struct {
	ScoreMetricsWindowSize Duration `yaml:"scoreMetricsWindowSize" json:"scoreMetricsWindowSize" tstype:"Duration"`
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
	RetryEmpty        *bool   `yaml:"retryEmpty,omitempty" json:"retryEmpty"`
	RetryPending      *bool   `yaml:"retryPending,omitempty" json:"retryPending"`
	SkipCacheRead     *bool   `yaml:"skipCacheRead,omitempty" json:"skipCacheRead"`
	UseUpstream       *string `yaml:"useUpstream,omitempty" json:"useUpstream"`
	SkipInterpolation *bool   `yaml:"skipInterpolation,omitempty" json:"skipInterpolation"`

	// Validation: Block Integrity
	EnforceHighestBlock        *bool `yaml:"enforceHighestBlock,omitempty" json:"enforceHighestBlock"`
	EnforceGetLogsBlockRange   *bool `yaml:"enforceGetLogsBlockRange,omitempty" json:"enforceGetLogsBlockRange"`
	EnforceNonNullTaggedBlocks *bool `yaml:"enforceNonNullTaggedBlocks,omitempty" json:"enforceNonNullTaggedBlocks"`

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

type EvmNetworkConfig struct {
	ChainId                     int64               `yaml:"chainId" json:"chainId"`
	FallbackFinalityDepth       int64               `yaml:"fallbackFinalityDepth,omitempty" json:"fallbackFinalityDepth"`
	FallbackStatePollerDebounce Duration            `yaml:"fallbackStatePollerDebounce,omitempty" json:"fallbackStatePollerDebounce" tstype:"Duration"`
	Integrity                   *EvmIntegrityConfig `yaml:"integrity,omitempty" json:"integrity"`
	GetLogsMaxAllowedRange      int64               `yaml:"getLogsMaxAllowedRange,omitempty" json:"getLogsMaxAllowedRange"`
	GetLogsMaxAllowedAddresses  int64               `yaml:"getLogsMaxAllowedAddresses,omitempty" json:"getLogsMaxAllowedAddresses"`
	GetLogsMaxAllowedTopics     int64               `yaml:"getLogsMaxAllowedTopics,omitempty" json:"getLogsMaxAllowedTopics"`
	GetLogsSplitOnError         *bool               `yaml:"getLogsSplitOnError,omitempty" json:"getLogsSplitOnError"`
	GetLogsSplitConcurrency     int                 `yaml:"getLogsSplitConcurrency,omitempty" json:"getLogsSplitConcurrency"`
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

	// IdempotentTransactionBroadcast enables idempotency handling for eth_sendRawTransaction.
	// When enabled (default), "already known" and verified "nonce too low" errors are converted
	// to success responses with the transaction hash. This allows failsafe policies (retry/hedge)
	// to work safely with transaction broadcasting.
	// Set to false to disable this behavior and return raw upstream errors.
	IdempotentTransactionBroadcast *bool `yaml:"idempotentTransactionBroadcast,omitempty" json:"idempotentTransactionBroadcast,omitempty"`
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

type SelectionPolicyConfig struct {
	EvalInterval     Duration       `yaml:"evalInterval,omitempty" json:"evalInterval" tstype:"Duration"`
	EvalFunction     sobek.Callable `yaml:"evalFunction,omitempty" json:"evalFunction" tstype:"SelectionPolicyEvalFunction | undefined"`
	EvalPerMethod    bool           `yaml:"evalPerMethod,omitempty" json:"evalPerMethod"`
	ResampleExcluded bool           `yaml:"resampleExcluded,omitempty" json:"resampleExcluded"`
	ResampleInterval Duration       `yaml:"resampleInterval,omitempty" json:"resampleInterval" tstype:"Duration"`
	ResampleCount    int            `yaml:"resampleCount,omitempty" json:"resampleCount"`

	evalFunctionOriginal string `yaml:"-" json:"-"`
}

func (c *SelectionPolicyConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawSelectionPolicyConfig struct {
		EvalInterval     Duration `yaml:"evalInterval"`
		EvalPerMethod    bool     `yaml:"evalPerMethod"`
		EvalFunction     string   `yaml:"evalFunction"`
		ResampleInterval Duration `yaml:"resampleInterval"`
		ResampleCount    int      `yaml:"resampleCount"`
		ResampleExcluded bool     `yaml:"resampleExcluded"`
	}
	raw := rawSelectionPolicyConfig{}

	if err := unmarshal(&raw); err != nil {
		return err
	}
	*c = SelectionPolicyConfig{
		EvalInterval:     raw.EvalInterval,
		EvalFunction:     nil,
		EvalPerMethod:    raw.EvalPerMethod,
		ResampleInterval: raw.ResampleInterval,
		ResampleCount:    raw.ResampleCount,
		ResampleExcluded: raw.ResampleExcluded,
	}

	if raw.EvalFunction != "" {
		evalFunction, err := CompileFunction(raw.EvalFunction)
		c.EvalFunction = evalFunction
		c.evalFunctionOriginal = raw.EvalFunction
		if err != nil {
			return fmt.Errorf("failed to compile selectionPolicy.evalFunction: %v", err)
		}
	}

	return nil
}

func (c *SelectionPolicyConfig) MarshalJSON() ([]byte, error) {
	evf := "<undefined>"
	if c.evalFunctionOriginal != "" {
		evf = c.evalFunctionOriginal
	}
	if c.EvalFunction != nil {
		evf = "<function>"
	}
	return sonic.Marshal(map[string]interface{}{
		"evalInterval":     c.EvalInterval,
		"evalPerMethod":    c.EvalPerMethod,
		"evalFunction":     evf,
		"resampleInterval": c.ResampleInterval,
		"resampleCount":    c.ResampleCount,
		"resampleExcluded": c.ResampleExcluded,
	})
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
	AllowedIssuers    []string          `yaml:"allowedIssuers" json:"allowedIssuers"`
	AllowedAudiences  []string          `yaml:"allowedAudiences" json:"allowedAudiences"`
	AllowedAlgorithms []string          `yaml:"allowedAlgorithms" json:"allowedAlgorithms"`
	RequiredClaims    []string          `yaml:"requiredClaims" json:"requiredClaims"`
	VerificationKeys  map[string]string `yaml:"verificationKeys" json:"verificationKeys"`
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

func loadConfigFromTypescript(filename string) (*Config, error) {
	contents, err := CompileTypeScript(filename)
	if err != nil {
		return nil, err
	}

	runtime, err := NewRuntime()
	if err != nil {
		return nil, err
	}
	_, err = runtime.Evaluate(contents)
	if err != nil {
		return nil, err
	}

	defaultExport := runtime.Exports().Get("default")

	// Get the config object default-exported from the TS code
	v := defaultExport.(*sobek.Object)
	if v == nil {
		return nil, fmt.Errorf("config object must be default exported from TypeScript code AND must be the last statement in the file")
	}

	var cfg Config
	err = MapJavascriptObjectToGo(v, &cfg)
	if err != nil {
		return nil, err
	}

	return &cfg, nil
}
