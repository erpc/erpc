package common

import (
	"os"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/util"
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
	LogLevel     string             `yaml:"logLevel" json:"logLevel"`
	Server       *ServerConfig      `yaml:"server" json:"server"`
	Database     *DatabaseConfig    `yaml:"database" json:"database"`
	Projects     []*ProjectConfig   `yaml:"projects" json:"projects"`
	RateLimiters *RateLimiterConfig `yaml:"rateLimiters" json:"rateLimiters"`
	Metrics      *MetricsConfig     `yaml:"metrics" json:"metrics"`
	Admin        *AdminConfig       `yaml:"admin" json:"admin"`
}

type ServerConfig struct {
	ListenV4   bool   `yaml:"listenV4" json:"listenV4"`
	HttpHostV4 string `yaml:"httpHostV4" json:"httpHostV4"`
	ListenV6   bool   `yaml:"listenV6" json:"listenV6"`
	HttpHostV6 string `yaml:"httpHostV6" json:"httpHostV6"`
	HttpPort   int    `yaml:"httpPort" json:"httpPort"`
	MaxTimeout string `yaml:"maxTimeout" json:"maxTimeout"`
}

type AdminConfig struct {
	Auth *AuthConfig `yaml:"auth" json:"auth"`
}

type DatabaseConfig struct {
	EvmJsonRpcCache *ConnectorConfig `yaml:"evmJsonRpcCache" json:"evmJsonRpcCache"`
}

type ConnectorConfig struct {
	Driver     string                     `yaml:"driver" json:"driver"`
	Memory     *MemoryConnectorConfig     `yaml:"memory" json:"memory"`
	Redis      *RedisConnectorConfig      `yaml:"redis" json:"redis"`
	DynamoDB   *DynamoDBConnectorConfig   `yaml:"dynamodb" json:"dynamodb"`
	PostgreSQL *PostgreSQLConnectorConfig `yaml:"postgresql" json:"postgresql"`
	Methods    []*MethodCacheConfig       `yaml:"methods" json:"methods"`
}

type MemoryConnectorConfig struct {
	MaxItems int `yaml:"maxItems" json:"maxItems"`
}

type TLSConfig struct {
	Enabled            bool   `yaml:"enabled" json:"enabled"`
	CertFile           string `yaml:"certFile" json:"certFile"`
	KeyFile            string `yaml:"keyFile" json:"keyFile"`
	CAFile             string `yaml:"caFile" json:"caFile"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify" json:"insecureSkipVerify"`
}

type RedisConnectorConfig struct {
	Addr     string     `yaml:"addr" json:"addr"`
	Password string     `yaml:"password" json:"password"`
	DB       int        `yaml:"db" json:"db"`
	TLS      *TLSConfig `yaml:"tls" json:"tls"`
}

func (r *RedisConnectorConfig) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]interface{}{
		"addr":     r.Addr,
		"password": "REDACTED",
		"db":       r.DB,
	})
}

type MethodCacheConfig struct {
	Method string `yaml:"method" json:"method"`
	TTL    string `yamle:"ttl" json:"ttl"`
}

type DynamoDBConnectorConfig struct {
	Table            string         `yaml:"table" json:"table"`
	Region           string         `yaml:"region" json:"region"`
	Endpoint         string         `yaml:"endpoint" json:"endpoint"`
	Auth             *AwsAuthConfig `yaml:"auth" json:"auth"`
	PartitionKeyName string         `yaml:"partitionKeyName" json:"partitionKeyName"`
	RangeKeyName     string         `yaml:"rangeKeyName" json:"rangeKeyName"`
	ReverseIndexName string         `yaml:"reverseIndexName" json:"reverseIndexName"`
}

type PostgreSQLConnectorConfig struct {
	ConnectionUri string `yaml:"connectionUri" json:"connectionUri"`
	Table         string `yaml:"table" json:"table"`
}

func (p *PostgreSQLConnectorConfig) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]string{
		"connectionUri": util.RedactEndpoint(p.ConnectionUri),
		"table":         p.Table,
	})
}

type AwsAuthConfig struct {
	Mode            string `yaml:"mode" json:"mode"` // "file", "env", "secret"
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
	Id              string             `yaml:"id" json:"id"`
	Admin           *AdminConfig       `yaml:"admin" json:"admin"`
	Auth            *AuthConfig        `yaml:"auth" json:"auth"`
	CORS            *CORSConfig        `yaml:"cors" json:"cors"`
	Upstreams       []*UpstreamConfig  `yaml:"upstreams" json:"upstreams"`
	Networks        []*NetworkConfig   `yaml:"networks" json:"networks"`
	RateLimitBudget string             `yaml:"rateLimitBudget" json:"rateLimitBudget"`
	HealthCheck     *HealthCheckConfig `yaml:"healthCheck" json:"healthCheck"`
}

type CORSConfig struct {
	AllowedOrigins   []string `yaml:"allowedOrigins" json:"allowedOrigins"`
	AllowedMethods   []string `yaml:"allowedMethods" json:"allowedMethods"`
	AllowedHeaders   []string `yaml:"allowedHeaders" json:"allowedHeaders"`
	ExposedHeaders   []string `yaml:"exposedHeaders" json:"exposedHeaders"`
	AllowCredentials bool     `yaml:"allowCredentials" json:"allowCredentials"`
	MaxAge           int      `yaml:"maxAge" json:"maxAge"`
}

type UpstreamConfig struct {
	Id                           string                   `yaml:"id" json:"id"`
	Type                         UpstreamType             `yaml:"type" json:"type"` // evm, evm+alchemy, solana
	VendorName                   string                   `yaml:"vendorName" json:"vendorName"`
	Endpoint                     string                   `yaml:"endpoint" json:"endpoint"`
	Evm                          *EvmUpstreamConfig       `yaml:"evm" json:"evm"`
	JsonRpc                      *JsonRpcUpstreamConfig   `yaml:"jsonRpc" json:"jsonRpc"`
	IgnoreMethods                []string                 `yaml:"ignoreMethods" json:"ignoreMethods"`
	AllowMethods                 []string                 `yaml:"allowMethods" json:"allowMethods"`
	AutoIgnoreUnsupportedMethods *bool                    `yaml:"autoIgnoreUnsupportedMethods" json:"autoIgnoreUnsupportedMethods"`
	Failsafe                     *FailsafeConfig          `yaml:"failsafe" json:"failsafe"`
	RateLimitBudget              string                   `yaml:"rateLimitBudget" json:"rateLimitBudget"`
	RateLimitAutoTune            *RateLimitAutoTuneConfig `yaml:"rateLimitAutoTune" json:"rateLimitAutoTune"`
}

// redact Endpoint
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
	Enabled            bool    `yaml:"enabled" json:"enabled"`
	AdjustmentPeriod   string  `yaml:"adjustmentPeriod" json:"adjustmentPeriod"`
	ErrorRateThreshold float64 `yaml:"errorRateThreshold" json:"errorRateThreshold"`
	IncreaseFactor     float64 `yaml:"increaseFactor" json:"increaseFactor"`
	DecreaseFactor     float64 `yaml:"decreaseFactor" json:"decreaseFactor"`
	MinBudget          int     `yaml:"minBudget" json:"minBudget"`
	MaxBudget          int     `yaml:"maxBudget" json:"maxBudget"`
}

type JsonRpcUpstreamConfig struct {
	SupportsBatch *bool  `yaml:"supportsBatch" json:"supportsBatch"`
	BatchMaxSize  int    `yaml:"batchMaxSize" json:"batchMaxSize"`
	BatchMaxWait  string `yaml:"batchMaxWait" json:"batchMaxWait"`
}

type EvmUpstreamConfig struct {
	ChainId              int         `yaml:"chainId" json:"chainId"`
	NodeType             EvmNodeType `yaml:"nodeType" json:"nodeType"`
	Engine               string      `yaml:"engine" json:"engine"`
	GetLogsMaxBlockRange int         `yaml:"getLogsMaxBlockRange" json:"getLogsMaxBlockRange"`
	StatePollerInterval  string      `yaml:"statePollerInterval" json:"statePollerInterval"`

	// By default "Syncing" is marked as unknown (nil) and that means we will be retrying empty responses
	// from such upstream, unless we explicitly know that the upstream is fully synced (false).
	Syncing *bool `yaml:"syncing" json:"syncing"`
}

type FailsafeConfig struct {
	Retry          *RetryPolicyConfig          `yaml:"retry" json:"retry"`
	CircuitBreaker *CircuitBreakerPolicyConfig `yaml:"circuitBreaker" json:"circuitBreaker"`
	Timeout        *TimeoutPolicyConfig        `yaml:"timeout" json:"timeout"`
	Hedge          *HedgePolicyConfig          `yaml:"hedge" json:"hedge"`
}

type RetryPolicyConfig struct {
	MaxAttempts     int     `yaml:"maxAttempts" json:"maxAttempts"`
	Delay           string  `yaml:"delay" json:"delay"`
	BackoffMaxDelay string  `yaml:"backoffMaxDelay" json:"backoffMaxDelay"`
	BackoffFactor   float32 `yaml:"backoffFactor" json:"backoffFactor"`
	Jitter          string  `yaml:"jitter" json:"jitter"`
}

type CircuitBreakerPolicyConfig struct {
	FailureThresholdCount    uint   `yaml:"failureThresholdCount" json:"failureThresholdCount"`
	FailureThresholdCapacity uint   `yaml:"failureThresholdCapacity" json:"failureThresholdCapacity"`
	HalfOpenAfter            string `yaml:"halfOpenAfter" json:"halfOpenAfter"`
	SuccessThresholdCount    uint   `yaml:"successThresholdCount" json:"successThresholdCount"`
	SuccessThresholdCapacity uint   `yaml:"successThresholdCapacity" json:"successThresholdCapacity"`
}

type TimeoutPolicyConfig struct {
	Duration string `yaml:"duration" json:"duration"`
}

type HedgePolicyConfig struct {
	Delay    string `yaml:"delay" json:"delay"`
	MaxCount int    `yaml:"maxCount" json:"maxCount"`
}

type RateLimiterConfig struct {
	Budgets []*RateLimitBudgetConfig `yaml:"budgets" json:"budgets"`
}

type RateLimitBudgetConfig struct {
	Id    string                 `yaml:"id" json:"id"`
	Rules []*RateLimitRuleConfig `yaml:"rules" json:"rules"`
}

type RateLimitRuleConfig struct {
	Method   string `yaml:"method" json:"method"`
	MaxCount uint   `yaml:"maxCount" json:"maxCount"`
	Period   string `yaml:"period" json:"period"`
	WaitTime string `yaml:"waitTime" json:"waitTime"`
}

type HealthCheckConfig struct {
	ScoreMetricsWindowSize string `yaml:"scoreMetricsWindowSize" json:"scoreMetricsWindowSize"`
}

type NetworkConfig struct {
	Architecture    NetworkArchitecture `yaml:"architecture" json:"architecture"`
	RateLimitBudget string              `yaml:"rateLimitBudget" json:"rateLimitBudget"`
	Failsafe        *FailsafeConfig     `yaml:"failsafe" json:"failsafe"`
	Evm             *EvmNetworkConfig   `yaml:"evm" json:"evm"`
}

type EvmNetworkConfig struct {
	ChainId              int64  `yaml:"chainId" json:"chainId"`
	FinalityDepth        int64  `yaml:"finalityDepth" json:"finalityDepth"`
	BlockTrackerInterval string `yaml:"blockTrackerInterval" json:"blockTrackerInterval"`
}

type AuthType string

const (
	AuthTypeSecret  AuthType = "secret"
	AuthTypeJwt     AuthType = "jwt"
	AuthTypeSiwe    AuthType = "siwe"
	AuthTypeNetwork AuthType = "network"
)

type AuthConfig struct {
	Strategies []*AuthStrategyConfig `yaml:"strategies" json:"strategies"`
}

type AuthStrategyConfig struct {
	IgnoreMethods   []string `yaml:"ignoreMethods" json:"ignoreMethods"`
	AllowMethods    []string `yaml:"allowMethods" json:"allowMethods"`
	RateLimitBudget string   `yaml:"rateLimitBudget" json:"rateLimitBudget"`

	Type    AuthType               `yaml:"type" json:"type"`
	Network *NetworkStrategyConfig `yaml:"network" json:"network"`
	Secret  *SecretStrategyConfig  `yaml:"secret" json:"secret"`
	Jwt     *JwtStrategyConfig     `yaml:"jwt" json:"jwt"`
	Siwe    *SiweStrategyConfig    `yaml:"siwe" json:"siwe"`
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

type MetricsConfig struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	ListenV4 bool   `yaml:"listenV4" json:"listenV4"`
	HostV4   string `yaml:"hostV4" json:"hostV4"`
	ListenV6 bool   `yaml:"listenV6" json:"listenV6"`
	HostV6   string `yaml:"hostV6" json:"hostV6"`
	Port     int    `yaml:"port" json:"port"`
}

var cfgInstance *Config

// LoadConfig loads the configuration from the specified file.
func LoadConfig(fs afero.Fs, filename string) (*Config, error) {
	data, err := afero.ReadFile(fs, filename)

	if err != nil {
		return nil, err
	}

	// Expand environment variables
	expandedData := []byte(os.ExpandEnv(string(data)))

	var cfg Config
	err = yaml.Unmarshal(expandedData, &cfg)
	if err != nil {
		return nil, err
	}

	cfgInstance = &cfg

	return &cfg, nil
}

func GetConfig() *Config {
	return cfgInstance
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
		Str("period", c.Period).
		Str("waitTime", c.WaitTime)
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

func (c *ServerConfig) MarshalZerologObject(e *zerolog.Event) {
	e.Str("hostV4", c.HttpHostV4).
		Str("hostV6", c.HttpHostV6).
		Int("port", c.HttpPort).
		Str("maxTimeout", c.MaxTimeout)
}

func (s *Config) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawConfig Config
	var raw rawConfig
	if !util.IsTest() {
		raw = rawConfig{
			LogLevel: "INFO",
			Server: &ServerConfig{
				HttpHostV4: "0.0.0.0",
				ListenV4:   true,
				HttpHostV6: "[::]",
				ListenV6:   false,
				HttpPort:   4000,
			},
			Database: &DatabaseConfig{
				EvmJsonRpcCache: nil,
			},
			Metrics: &MetricsConfig{
				Enabled:  true,
				HostV4:   "0.0.0.0",
				ListenV4: true,
				HostV6:   "[::]",
				ListenV6: false,
				Port:     4001,
			},
		}
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}

	*s = Config(raw)
	return nil
}

func (s *UpstreamConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawUpstreamConfig UpstreamConfig
	raw := rawUpstreamConfig{
		Failsafe: &FailsafeConfig{
			Timeout: &TimeoutPolicyConfig{
				Duration: "15s",
			},
			Retry: &RetryPolicyConfig{
				MaxAttempts:     3,
				Delay:           "500ms",
				Jitter:          "500ms",
				BackoffMaxDelay: "5s",
				BackoffFactor:   1.5,
			},
			CircuitBreaker: &CircuitBreakerPolicyConfig{
				FailureThresholdCount:    800,
				FailureThresholdCapacity: 1000,
				HalfOpenAfter:            "5m",
				SuccessThresholdCount:    3,
				SuccessThresholdCapacity: 3,
			},
		},
		RateLimitAutoTune: &RateLimitAutoTuneConfig{
			Enabled:            true,
			AdjustmentPeriod:   "1m",
			ErrorRateThreshold: 0.1,
			IncreaseFactor:     1.05,
			DecreaseFactor:     0.9,
			MinBudget:          0,
			MaxBudget:          10_000,
		},
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}

	if raw.Endpoint == "" {
		return NewErrInvalidConfig("upstream.*.endpoint is required")
	}
	if raw.Id == "" {
		raw.Id = util.RedactEndpoint(raw.Endpoint)
	}

	*s = UpstreamConfig(raw)
	return nil
}

var NetworkDefaultFailsafeConfig = &FailsafeConfig{
	Hedge: &HedgePolicyConfig{
		Delay:    "200ms",
		MaxCount: 3,
	},
	Retry: &RetryPolicyConfig{
		MaxAttempts:     3,
		Delay:           "1s",
		Jitter:          "500ms",
		BackoffMaxDelay: "10s",
		BackoffFactor:   2,
	},
	Timeout: &TimeoutPolicyConfig{
		Duration: "30s",
	},
}

func (c *NetworkConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawNetworkConfig NetworkConfig
	raw := rawNetworkConfig{
		Failsafe: NetworkDefaultFailsafeConfig,
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}

	if raw.Architecture == "" {
		return NewErrInvalidConfig("network.*.architecture is required")
	}

	if raw.Architecture == "evm" && raw.Evm == nil {
		return NewErrInvalidConfig("network.*.evm is required for evm networks")
	}

	*c = NetworkConfig(raw)
	return nil
}
