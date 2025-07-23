package common

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"time"

	"strings"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/util"
	"github.com/grafana/sobek"
	"github.com/rs/zerolog"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v3"
)

var (
	ErpcVersion   = "dev"
	ErpcCommitSha = "none"
	TRUE          = true
	FALSE         = false
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
	ListenV4            *bool           `yaml:"listenV4,omitempty" json:"listenV4"`
	HttpHostV4          *string         `yaml:"httpHostV4,omitempty" json:"httpHostV4"`
	ListenV6            *bool           `yaml:"listenV6,omitempty" json:"listenV6"`
	HttpHostV6          *string         `yaml:"httpHostV6,omitempty" json:"httpHostV6"`
	HttpPort            *int            `yaml:"httpPort,omitempty" json:"httpPort"`
	MaxTimeout          *Duration       `yaml:"maxTimeout,omitempty" json:"maxTimeout" tstype:"Duration"`
	ReadTimeout         *Duration       `yaml:"readTimeout,omitempty" json:"readTimeout" tstype:"Duration"`
	WriteTimeout        *Duration       `yaml:"writeTimeout,omitempty" json:"writeTimeout" tstype:"Duration"`
	EnableGzip          *bool           `yaml:"enableGzip,omitempty" json:"enableGzip"`
	TLS                 *TLSConfig      `yaml:"tls,omitempty" json:"tls"`
	Aliasing            *AliasingConfig `yaml:"aliasing" json:"aliasing"`
	WaitBeforeShutdown  *Duration       `yaml:"waitBeforeShutdown,omitempty" json:"waitBeforeShutdown" tstype:"Duration"`
	WaitAfterShutdown   *Duration       `yaml:"waitAfterShutdown,omitempty" json:"waitAfterShutdown" tstype:"Duration"`
	IncludeErrorDetails *bool           `yaml:"includeErrorDetails,omitempty" json:"includeErrorDetails"`
}

type HealthCheckConfig struct {
	Mode        HealthCheckMode `yaml:"mode,omitempty" json:"mode"`
	Auth        *AuthConfig     `yaml:"auth,omitempty" json:"auth"`
	DefaultEval string          `yaml:"defaultEval,omitempty" json:"defaultEval"`
}

type HealthCheckMode string

const (
	HealthCheckModeSimple  HealthCheckMode = "simple"
	HealthCheckModeVerbose HealthCheckMode = "verbose"
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
	Enabled    bool            `yaml:"enabled,omitempty" json:"enabled"`
	Endpoint   string          `yaml:"endpoint,omitempty" json:"endpoint"`
	Protocol   TracingProtocol `yaml:"protocol,omitempty" json:"protocol"`
	SampleRate float64         `yaml:"sampleRate,omitempty" json:"sampleRate"`
	Detailed   bool            `yaml:"detailed,omitempty" json:"detailed"`
	TLS        *TLSConfig      `yaml:"tls,omitempty" json:"tls"`
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
	ClusterKey      string           `yaml:"clusterKey,omitempty" json:"clusterKey"`
	Connector       *ConnectorConfig `yaml:"connector,omitempty" json:"connector"`
	FallbackTimeout Duration         `yaml:"fallbackTimeout,omitempty" json:"fallbackTimeout" tstype:"Duration"`
	LockTtl         Duration         `yaml:"lockTtl,omitempty" json:"lockTtl" tstype:"Duration"`
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
}

type CachePolicyConfig struct {
	Connector   string             `yaml:"connector" json:"connector"`
	Network     string             `yaml:"network,omitempty" json:"network"`
	Method      string             `yaml:"method,omitempty" json:"method"`
	Params      []interface{}      `yaml:"params,omitempty" json:"params"`
	Finality    DataFinalityState  `yaml:"finality,omitempty" json:"finality" tstype:"DataFinalityState"`
	Empty       CacheEmptyBehavior `yaml:"empty,omitempty" json:"empty" tstype:"CacheEmptyBehavior"`
	MinItemSize *string            `yaml:"minItemSize,omitempty" json:"minItemSize" tstype:"ByteSize"`
	MaxItemSize *string            `yaml:"maxItemSize,omitempty" json:"maxItemSize" tstype:"ByteSize"`
	TTL         Duration           `yaml:"ttl,omitempty" json:"ttl" tstype:"Duration"`
}

type ConnectorDriverType string

const (
	DriverMemory     ConnectorDriverType = "memory"
	DriverRedis      ConnectorDriverType = "redis"
	DriverPostgreSQL ConnectorDriverType = "postgresql"
	DriverDynamoDB   ConnectorDriverType = "dynamodb"
)

type ConnectorConfig struct {
	Id         string                     `yaml:"id,omitempty" json:"id"`
	Driver     ConnectorDriverType        `yaml:"driver" json:"driver" tstype:"TsConnectorDriverType"`
	Memory     *MemoryConnectorConfig     `yaml:"memory,omitempty" json:"memory"`
	Redis      *RedisConnectorConfig      `yaml:"redis,omitempty" json:"redis"`
	DynamoDB   *DynamoDBConnectorConfig   `yaml:"dynamodb,omitempty" json:"dynamodb"`
	PostgreSQL *PostgreSQLConnectorConfig `yaml:"postgresql,omitempty" json:"postgresql"`
	Mock       *MockConnectorConfig       `yaml:"-" json:"-"`
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
	Id                     string                              `yaml:"id" json:"id"`
	Auth                   *AuthConfig                         `yaml:"auth,omitempty" json:"auth"`
	CORS                   *CORSConfig                         `yaml:"cors,omitempty" json:"cors"`
	Providers              []*ProviderConfig                   `yaml:"providers,omitempty" json:"providers"`
	UpstreamDefaults       *UpstreamConfig                     `yaml:"upstreamDefaults,omitempty" json:"upstreamDefaults"`
	Upstreams              []*UpstreamConfig                   `yaml:"upstreams,omitempty" json:"upstreams"`
	NetworkDefaults        *NetworkDefaults                    `yaml:"networkDefaults,omitempty" json:"networkDefaults"`
	Networks               []*NetworkConfig                    `yaml:"networks,omitempty" json:"networks"`
	RateLimitBudget        string                              `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget"`
	ScoreMetricsWindowSize Duration                            `yaml:"scoreMetricsWindowSize,omitempty" json:"scoreMetricsWindowSize" tstype:"Duration"`
	DeprecatedHealthCheck  *DeprecatedProjectHealthCheckConfig `yaml:"healthCheck,omitempty" json:"healthCheck"`
}

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
	LoadBalancer                 *LoadBalancerConfig      `yaml:"loadBalancer,omitempty" json:"loadBalancer"`
	Shadow                       *ShadowUpstreamConfig    `yaml:"shadow,omitempty" json:"shadow"`
	ProjectConfig                *ProjectConfig           `yaml:"-" json:"-"` // Reference to parent project config
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
		LoadBalancer                 *LoadBalancerConfig      `yaml:"loadBalancer,omitempty"`
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
	u.LoadBalancer = old.LoadBalancer
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
	Network         string   `yaml:"network" json:"network"`
	Method          string   `yaml:"method" json:"method"`
	Overall         *float64 `yaml:"overall" json:"overall"`
	ErrorRate       *float64 `yaml:"errorRate" json:"errorRate"`
	RespLatency     *float64 `yaml:"respLatency" json:"respLatency"`
	TotalRequests   *float64 `yaml:"totalRequests" json:"totalRequests"`
	ThrottledRate   *float64 `yaml:"throttledRate" json:"throttledRate"`
	BlockHeadLag    *float64 `yaml:"blockHeadLag" json:"blockHeadLag"`
	FinalizationLag *float64 `yaml:"finalizationLag" json:"finalizationLag"`
}

func (c *ScoreMultiplierConfig) Copy() *ScoreMultiplierConfig {
	if c == nil {
		return nil
	}
	copied := &ScoreMultiplierConfig{}
	*copied = *c
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
	ChainId                            int64       `yaml:"chainId" json:"chainId"`
	NodeType                           EvmNodeType `yaml:"nodeType,omitempty" json:"nodeType"`
	StatePollerInterval                Duration    `yaml:"statePollerInterval,omitempty" json:"statePollerInterval" tstype:"Duration"`
	StatePollerDebounce                Duration    `yaml:"statePollerDebounce,omitempty" json:"statePollerDebounce" tstype:"Duration"`
	MaxAvailableRecentBlocks           int64       `yaml:"maxAvailableRecentBlocks,omitempty" json:"maxAvailableRecentBlocks"`
	GetLogsAutoSplittingRangeThreshold int64       `yaml:"getLogsAutoSplittingRangeThreshold,omitempty" json:"getLogsAutoSplittingRangeThreshold"`
	GetLogsMaxAllowedRange             int64       `yaml:"getLogsMaxAllowedRange,omitempty" json:"getLogsMaxAllowedRange"`
	GetLogsMaxAllowedAddresses         int64       `yaml:"getLogsMaxAllowedAddresses,omitempty" json:"getLogsMaxAllowedAddresses"`
	GetLogsMaxAllowedTopics            int64       `yaml:"getLogsMaxAllowedTopics,omitempty" json:"getLogsMaxAllowedTopics"`
	GetLogsSplitOnError                *bool       `yaml:"getLogsSplitOnError,omitempty" json:"getLogsSplitOnError"`
	SkipWhenSyncing                    *bool       `yaml:"skipWhenSyncing,omitempty" json:"skipWhenSyncing"`
	// TODO: remove deprecated alias (backward compat): maps to GetLogsAutoSplittingRangeThreshold
	GetLogsMaxBlockRange int64 `yaml:"getLogsMaxBlockRange,omitempty" json:"-"`
}

func (c *EvmUpstreamConfig) Copy() *EvmUpstreamConfig {
	if c == nil {
		return nil
	}

	copied := &EvmUpstreamConfig{}
	*copied = *c

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
	RequiredParticipants    int                              `yaml:"requiredParticipants" json:"requiredParticipants"`
	AgreementThreshold      int                              `yaml:"agreementThreshold,omitempty" json:"agreementThreshold"`
	DisputeBehavior         ConsensusDisputeBehavior         `yaml:"disputeBehavior,omitempty" json:"disputeBehavior"`
	LowParticipantsBehavior ConsensusLowParticipantsBehavior `yaml:"lowParticipantsBehavior,omitempty" json:"lowParticipantsBehavior"`
	PunishMisbehavior       *PunishMisbehaviorConfig         `yaml:"punishMisbehavior,omitempty" json:"punishMisbehavior"`
	DisputeLogLevel         string                           `yaml:"disputeLogLevel,omitempty" json:"disputeLogLevel"` // "trace", "debug", "info", "warn", "error"
	IgnoreFields            map[string][]string              `yaml:"ignoreFields,omitempty" json:"ignoreFields"`
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

	if c.IgnoreFields != nil {
		copied.IgnoreFields = make(map[string][]string, len(c.IgnoreFields))
		for method, fields := range c.IgnoreFields {
			copied.IgnoreFields[method] = make([]string, len(fields))
			copy(copied.IgnoreFields[method], fields)
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
	Budgets []*RateLimitBudgetConfig `yaml:"budgets" json:"budgets" tstype:"RateLimitBudgetConfig[]"`
}

type RateLimitBudgetConfig struct {
	Id    string                 `yaml:"id" json:"id"`
	Rules []*RateLimitRuleConfig `yaml:"rules" json:"rules" tstype:"RateLimitRuleConfig[]"`
}

type RateLimitRuleConfig struct {
	Method   string   `yaml:"method" json:"method"`
	MaxCount uint     `yaml:"maxCount" json:"maxCount"`
	Period   Duration `yaml:"period" json:"period" tstype:"Duration"`
	WaitTime Duration `yaml:"waitTime" json:"waitTime" tstype:"Duration"`
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
	RetryEmpty    *bool   `yaml:"retryEmpty,omitempty" json:"retryEmpty"`
	RetryPending  *bool   `yaml:"retryPending,omitempty" json:"retryPending"`
	SkipCacheRead *bool   `yaml:"skipCacheRead,omitempty" json:"skipCacheRead"`
	UseUpstream   *string `yaml:"useUpstream,omitempty" json:"useUpstream"`
}

type EvmNetworkConfig struct {
	ChainId                     int64               `yaml:"chainId" json:"chainId"`
	FallbackFinalityDepth       int64               `yaml:"fallbackFinalityDepth,omitempty" json:"fallbackFinalityDepth"`
	FallbackStatePollerDebounce Duration            `yaml:"fallbackStatePollerDebounce,omitempty" json:"fallbackStatePollerDebounce" tstype:"Duration"`
	Integrity                   *EvmIntegrityConfig `yaml:"integrity,omitempty" json:"integrity"`
}

type EvmIntegrityConfig struct {
	EnforceHighestBlock      *bool `yaml:"enforceHighestBlock,omitempty" json:"enforceHighestBlock"`
	EnforceGetLogsBlockRange *bool `yaml:"enforceGetLogsBlockRange,omitempty" json:"enforceGetLogsBlockRange"`
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
	AuthTypeSecret  AuthType = "secret"
	AuthTypeJwt     AuthType = "jwt"
	AuthTypeSiwe    AuthType = "siwe"
	AuthTypeNetwork AuthType = "network"
)

type AuthConfig struct {
	Strategies []*AuthStrategyConfig `yaml:"strategies" json:"strategies" tstype:"TsAuthStrategyConfig[]"`
}

type AuthStrategyConfig struct {
	IgnoreMethods   []string `yaml:"ignoreMethods,omitempty" json:"ignoreMethods,omitempty"`
	AllowMethods    []string `yaml:"allowMethods,omitempty" json:"allowMethods,omitempty"`
	RateLimitBudget string   `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget,omitempty"`

	Type    AuthType               `yaml:"type" json:"type" tstype:"TsAuthType"`
	Network *NetworkStrategyConfig `yaml:"network,omitempty" json:"network,omitempty"`
	Secret  *SecretStrategyConfig  `yaml:"secret,omitempty" json:"secret,omitempty"`
	Jwt     *JwtStrategyConfig     `yaml:"jwt,omitempty" json:"jwt,omitempty"`
	Siwe    *SiweStrategyConfig    `yaml:"siwe,omitempty" json:"siwe,omitempty"`
}

type SecretStrategyConfig struct {
	Value string `yaml:"value" json:"value"`
}

// custom json marshaller to redact the secret value
func (s *SecretStrategyConfig) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]string{
		"value": "REDACTED",
	})
}

type JwtStrategyConfig struct {
	AllowedIssuers    []string          `yaml:"allowedIssuers" json:"allowedIssuers"`
	AllowedAudiences  []string          `yaml:"allowedAudiences" json:"allowedAudiences"`
	AllowedAlgorithms []string          `yaml:"allowedAlgorithms" json:"allowedAlgorithms"`
	RequiredClaims    []string          `yaml:"requiredClaims" json:"requiredClaims"`
	VerificationKeys  map[string]string `yaml:"verificationKeys" json:"verificationKeys"`
}

type SiweStrategyConfig struct {
	AllowedDomains []string `yaml:"allowedDomains" json:"allowedDomains"`
}

type NetworkStrategyConfig struct {
	AllowedIPs     []string `yaml:"allowedIPs" json:"allowedIPs"`
	AllowedCIDRs   []string `yaml:"allowedCIDRs" json:"allowedCIDRs"`
	AllowLocalhost bool     `yaml:"allowLocalhost" json:"allowLocalhost"`
	TrustedProxies []string `yaml:"trustedProxies" json:"trustedProxies"`
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
		Uint("maxCount", c.MaxCount).
		Str("periodMs", fmt.Sprintf("%d", c.Period)).
		Str("waitTimeMs", fmt.Sprintf("%d", c.WaitTime))
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

type LoadBalancerConfig struct {
	Type LoadBalancerType `json:"type"`
}
