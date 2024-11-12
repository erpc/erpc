package common

import (
	"fmt"
	"os"
	"time"

	"strings"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common/script"
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
	LogLevel     string             `yaml:"logLevel" json:"logLevel" tstype:"types.LogLevel"`
	Server       *ServerConfig      `yaml:"server" json:"server"`
	Admin        *AdminConfig       `yaml:"admin" json:"admin"`
	Database     *DatabaseConfig    `yaml:"database" json:"database"`
	Projects     []*ProjectConfig   `yaml:"projects" json:"projects"`
	RateLimiters *RateLimiterConfig `yaml:"rateLimiters" json:"rateLimiters"`
	Metrics      *MetricsConfig     `yaml:"metrics" json:"metrics"`
}

func (c *Config) HasRateLimiterBudget(id string) bool {
	for _, budget := range c.RateLimiters.Budgets {
		if budget.Id == id {
			return true
		}
	}
	return false
}

type ServerConfig struct {
	ListenV4   *bool   `yaml:"listenV4,omitempty" json:"listenV4,omitempty"`
	HttpHostV4 *string `yaml:"httpHostV4,omitempty" json:"httpHostV4,omitempty"`
	ListenV6   *bool   `yaml:"listenV6,omitempty" json:"listenV6,omitempty"`
	HttpHostV6 *string `yaml:"httpHostV6,omitempty" json:"httpHostV6,omitempty"`
	HttpPort   *int    `yaml:"httpPort,omitempty" json:"httpPort,omitempty"`
	MaxTimeout *string `yaml:"maxTimeout,omitempty" json:"maxTimeout,omitempty"`
}

type AdminConfig struct {
	Auth *AuthConfig `yaml:"auth" json:"auth"`
	CORS *CORSConfig `yaml:"cors" json:"cors"`
}

type DatabaseConfig struct {
	EvmJsonRpcCache *ConnectorConfig `yaml:"evmJsonRpcCache" json:"evmJsonRpcCache"`
}

type ConnectorDriverType string

const (
	DriverMemory   ConnectorDriverType = "memory"
	DriverRedis    ConnectorDriverType = "redis"
	DriverPostgres ConnectorDriverType = "postgres"
	DriverDynamoDB ConnectorDriverType = "dynamodb"
)

type ConnectorConfig struct {
	Driver     ConnectorDriverType        `yaml:"driver" json:"driver" tstype:"types.ConnectorDriverType"`
	Memory     *MemoryConnectorConfig     `yaml:"memory" json:"memory"`
	Redis      *RedisConnectorConfig      `yaml:"redis" json:"redis"`
	DynamoDB   *DynamoDBConnectorConfig   `yaml:"dynamodb" json:"dynamodb"`
	PostgreSQL *PostgreSQLConnectorConfig `yaml:"postgresql" json:"postgresql"`
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
	Password string     `yaml:"password" json:"-"`
	DB       int        `yaml:"db" json:"db"`
	TLS      *TLSConfig `yaml:"tls" json:"tls"`
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
	Auth            *AuthConfig        `yaml:"auth,omitempty" json:"auth,omitempty"`
	CORS            *CORSConfig        `yaml:"cors,omitempty" json:"cors,omitempty"`
	Upstreams       []*UpstreamConfig  `yaml:"upstreams" json:"upstreams"`
	Networks        []*NetworkConfig   `yaml:"networks,omitempty" json:"networks,omitempty"`
	RateLimitBudget string             `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget,omitempty"`
	HealthCheck     *HealthCheckConfig `yaml:"healthCheck,omitempty" json:"healthCheck,omitempty"`
}

type CORSConfig struct {
	AllowedOrigins   []string `yaml:"allowedOrigins" json:"allowedOrigins"`
	AllowedMethods   []string `yaml:"allowedMethods" json:"allowedMethods"`
	AllowedHeaders   []string `yaml:"allowedHeaders" json:"allowedHeaders"`
	ExposedHeaders   []string `yaml:"exposedHeaders" json:"exposedHeaders"`
	AllowCredentials *bool    `yaml:"allowCredentials" json:"allowCredentials"`
	MaxAge           int      `yaml:"maxAge" json:"maxAge"`
}

type UpstreamConfig struct {
	Id                           string                   `yaml:"id,omitempty" json:"id"`
	Type                         UpstreamType             `yaml:"type,omitempty" json:"type"`
	Group                        string                   `yaml:"group,omitempty" json:"group"`
	VendorName                   string                   `yaml:"vendorName,omitempty" json:"vendorName"`
	Endpoint                     string                   `yaml:"endpoint" json:"endpoint"`
	Evm                          *EvmUpstreamConfig       `yaml:"evm,omitempty" json:"evm"`
	JsonRpc                      *JsonRpcUpstreamConfig   `yaml:"jsonRpc,omitempty" json:"jsonRpc"`
	IgnoreMethods                []string                 `yaml:"ignoreMethods,omitempty" json:"ignoreMethods"`
	AllowMethods                 []string                 `yaml:"allowMethods,omitempty" json:"allowMethods"`
	AutoIgnoreUnsupportedMethods *bool                    `yaml:"autoIgnoreUnsupportedMethods,omitempty" json:"autoIgnoreUnsupportedMethods"`
	Failsafe                     *FailsafeConfig          `yaml:"failsafe,omitempty" json:"failsafe"`
	RateLimitBudget              string                   `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget"`
	RateLimitAutoTune            *RateLimitAutoTuneConfig `yaml:"rateLimitAutoTune,omitempty" json:"rateLimitAutoTune"`
	Routing                      *RoutingConfig           `yaml:"routing,omitempty" json:"routing"`
}

type RoutingConfig struct {
	ScoreMultipliers []*ScoreMultiplierConfig `yaml:"scoreMultipliers" json:"scoreMultipliers"`
}

type ScoreMultiplierConfig struct {
	Network         string  `yaml:"network" json:"network"`
	Method          string  `yaml:"method" json:"method"`
	Overall         float64 `yaml:"overall" json:"overall"`
	ErrorRate       float64 `yaml:"errorRate" json:"errorRate"`
	P90Latency      float64 `yaml:"p90latency" json:"p90latency"`
	TotalRequests   float64 `yaml:"totalRequests" json:"totalRequests"`
	ThrottledRate   float64 `yaml:"throttledRate" json:"throttledRate"`
	BlockHeadLag    float64 `yaml:"blockHeadLag" json:"blockHeadLag"`
	FinalizationLag float64 `yaml:"finalizationLag" json:"finalizationLag"`
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
	Enabled            *bool   `yaml:"enabled" json:"enabled"`
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
	ChainId             int         `yaml:"chainId" json:"chainId"`
	NodeType            EvmNodeType `yaml:"nodeType,omitempty" json:"nodeType"`
	StatePollerInterval string      `yaml:"statePollerInterval,omitempty" json:"statePollerInterval"`
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
	Architecture    NetworkArchitecture    `yaml:"architecture" json:"architecture" tstype:"'evm'"`
	RateLimitBudget string                 `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget,omitempty"`
	Failsafe        *FailsafeConfig        `yaml:"failsafe,omitempty" json:"failsafe,omitempty"`
	Evm             *EvmNetworkConfig      `yaml:"evm,omitempty" json:"evm,omitempty"`
	SelectionPolicy *SelectionPolicyConfig `yaml:"selectionPolicy,omitempty" json:"selectionPolicy,omitempty"`
}

type EvmNetworkConfig struct {
	ChainId       int64 `yaml:"chainId" json:"chainId"`
	FinalityDepth int64 `yaml:"finalityDepth,omitempty" json:"finalityDepth,omitempty"`
}

type SelectionPolicyConfig struct {
	EvalInterval  time.Duration  `yaml:"evalInterval,omitempty" json:"evalInterval,omitempty"`
	EvalFunction  sobek.Callable `yaml:"evalFunction,omitempty" json:"evalFunction,omitempty" tstype:"types.SelectionPolicyEvalFunction | undefined"`
	EvalPerMethod bool           `yaml:"evalPerMethod,omitempty" json:"evalPerMethod,omitempty"`
	SampleAfter   time.Duration  `yaml:"sampleAfter,omitempty" json:"sampleAfter,omitempty"`
	SampleCount   int            `yaml:"sampleCount,omitempty" json:"sampleCount,omitempty"`

	evalFunctionOriginal string `yaml:"-" json:"-"`
}

func (c *SelectionPolicyConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawSelectionPolicyConfig struct {
		EvalInterval  string `yaml:"evalInterval"`
		EvalPerMethod bool   `yaml:"evalPerMethod"`
		EvalFunction  string `yaml:"evalFunction"`
		SampleAfter   string `yaml:"sampleAfter"`
		SampleCount   int    `yaml:"sampleCount"`
	}
	raw := rawSelectionPolicyConfig{}

	if err := unmarshal(&raw); err != nil {
		return err
	}

	if raw.SampleAfter != "" {
		sampleAfter, err := time.ParseDuration(raw.SampleAfter)
		if err != nil {
			return fmt.Errorf("failed to parse sampleAfter: %v", err)
		}
		c.SampleAfter = sampleAfter
	}

	if raw.EvalInterval != "" {
		evalInterval, err := time.ParseDuration(raw.EvalInterval)
		if err != nil {
			return fmt.Errorf("failed to parse evalInterval: %v", err)
		}
		c.EvalInterval = evalInterval
		c.evalFunctionOriginal = raw.EvalFunction
	}

	if raw.EvalFunction != "" {
		evalFunction, err := script.CompileFunction(raw.EvalFunction)
		c.EvalFunction = evalFunction
		if err != nil {
			return fmt.Errorf("failed to compile selectionPolicy.evalFunction: %v", err)
		}
	}

	c.EvalPerMethod = raw.EvalPerMethod
	c.SampleCount = raw.SampleCount

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
		"evalInterval":  c.EvalInterval,
		"evalPerMethod": c.EvalPerMethod,
		"evalFunction":  evf,
		"sampleAfter":   c.SampleAfter,
		"sampleCount":   c.SampleCount,
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
	Strategies []*AuthStrategyConfig `yaml:"strategies" json:"strategies"`
}

type AuthStrategyConfig struct {
	IgnoreMethods   []string `yaml:"ignoreMethods,omitempty" json:"ignoreMethods,omitempty"`
	AllowMethods    []string `yaml:"allowMethods,omitempty" json:"allowMethods,omitempty"`
	RateLimitBudget string   `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget,omitempty"`

	Type    AuthType               `yaml:"type" json:"type"`
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

type MetricsConfig struct {
	Enabled  *bool   `yaml:"enabled" json:"enabled"`
	ListenV4 *bool   `yaml:"listenV4" json:"listenV4"`
	HostV4   *string `yaml:"hostV4" json:"hostV4"`
	ListenV6 *bool   `yaml:"listenV6" json:"listenV6"`
	HostV6   *string `yaml:"hostV6" json:"hostV6"`
	Port     *int    `yaml:"port" json:"port"`
}

var cfgInstance *Config

// LoadConfig loads the configuration from the specified file.
// It supports both YAML and TypeScript (.ts) files.
func LoadConfig(fs afero.Fs, filename string) (*Config, error) {
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
		err = yaml.Unmarshal(expandedData, &cfg)
		if err != nil {
			return nil, err
		}
	}

	cfg.SetDefaults()

	err = cfg.Validate()
	if err != nil {
		return nil, err
	}

	cfgInstance = &cfg

	return &cfg, nil
}

func loadConfigFromTypescript(filename string) (*Config, error) {
	contents, err := script.CompileTypeScript(filename)
	if err != nil {
		return nil, err
	}

	runtime, err := script.NewRuntime()
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
	err = script.MapJavascriptObjectToGo(v, &cfg)
	if err != nil {
		return nil, err
	}

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
