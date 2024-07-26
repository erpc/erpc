package common

import (
	"github.com/flair-sdk/erpc/util"
	"github.com/rs/zerolog"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v2"
)

// Config represents the configuration of the application.
type Config struct {
	LogLevel     string             `yaml:"logLevel"`
	Server       *ServerConfig      `yaml:"server"`
	Database     *DatabaseConfig    `yaml:"database"`
	Projects     []*ProjectConfig   `yaml:"projects"`
	RateLimiters *RateLimiterConfig `yaml:"rateLimiters"`
	Metrics      *MetricsConfig     `yaml:"metrics"`
}

type ServerConfig struct {
	HttpHost     string `yaml:"httpHost"`
	HttpPort     int    `yaml:"httpPort"`
	MaxTimeoutMs int    `yaml:"maxTimeoutMs"`
}

type DatabaseConfig struct {
	EvmJsonRpcCache    *ConnectorConfig `yaml:"evmJsonRpcCache"`
	EvmBlockIngestions *ConnectorConfig `yaml:"evmBlockIngestions"`
	RateLimitSnapshots *ConnectorConfig `yaml:"rateLimitSnapshots"`
}

type ConnectorConfig struct {
	Driver     string                     `yaml:"driver"`
	Memory     *MemoryConnectorConfig     `yaml:"memory"`
	Redis      *RedisConnectorConfig      `yaml:"redis"`
	DynamoDB   *DynamoDBConnectorConfig   `yaml:"dynamodb"`
	PostgreSQL *PostgreSQLConnectorConfig `yaml:"postgresql"`
}

type MemoryConnectorConfig struct {
	MaxItems int `yaml:"maxItems"`
}

type RedisConnectorConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type DynamoDBConnectorConfig struct {
	Table            string         `yaml:"table"`
	Region           string         `yaml:"region"`
	Endpoint         string         `yaml:"endpoint"`
	Auth             *AwsAuthConfig `yaml:"auth"`
	PartitionKeyName string         `yaml:"partitionKeyName"`
	RangeKeyName     string         `yaml:"rangeKeyName"`
	ReverseIndexName string         `yaml:"reverseIndexName"`
}

type PostgreSQLConnectorConfig struct {
	ConnectionUri string `yaml:"connectionUri"`
	Table         string `yaml:"table"`
}

type AwsAuthConfig struct {
	Mode            string `yaml:"mode"` // "file", "env", "secret"
	CredentialsFile string `yaml:"credentialsFile"`
	Profile         string `yaml:"profile"`
	AccessKeyID     string `yaml:"accessKeyID"`
	SecretAccessKey string `yaml:"secretAccessKey"`
}

type ProjectConfig struct {
	Id              string             `yaml:"id"`
	Upstreams       []*UpstreamConfig  `yaml:"upstreams"`
	Networks        []*NetworkConfig   `yaml:"networks"`
	RateLimitBudget string             `yaml:"rateLimitBudget"`
	HealthCheck     *HealthCheckConfig `yaml:"healthCheck"`
}

type UpstreamConfig struct {
	Id                string             `yaml:"id"`
	Type              UpstreamType       `yaml:"type"` // evm, evm-alchemy, solana
	VendorName        string             `yaml:"vendorName"`
	Endpoint          string             `yaml:"endpoint"`
	Evm               *EvmUpstreamConfig `yaml:"evm"`
	AllowMethods      []string           `yaml:"allowMethods"`
	IgnoreMethods     []string           `yaml:"ignoreMethods"`
	Failsafe          *FailsafeConfig    `yaml:"failsafe"`
	RateLimitBudget   string             `yaml:"rateLimitBudget"`
	CreditUnitMapping string             `yaml:"creditUnitMapping"`
}

type EvmUpstreamConfig struct {
	ChainId              int         `yaml:"chainId"`
	NodeType             EvmNodeType `yaml:"nodeType"`
	Engine               string      `yaml:"engine"`
	GetLogsMaxBlockRange int         `yaml:"getLogsMaxBlockRange"`
	Syncing              bool        `yaml:"syncing"`
}

type FailsafeConfig struct {
	Retry          *RetryPolicyConfig          `yaml:"retry"`
	CircuitBreaker *CircuitBreakerPolicyConfig `yaml:"circuitBreaker"`
	Timeout        *TimeoutPolicyConfig        `yaml:"timeout"`
	Hedge          *HedgePolicyConfig          `yaml:"hedge"`
}

type RetryPolicyConfig struct {
	MaxAttempts     int     `yaml:"maxAttempts"`
	Delay           string  `yaml:"delay"`
	BackoffMaxDelay string  `yaml:"backoffMaxDelay"`
	BackoffFactor   float32 `yaml:"backoffFactor"`
	Jitter          string  `yaml:"jitter"`
}

type CircuitBreakerPolicyConfig struct {
	FailureThresholdCount    int    `yaml:"failureThresholdCount"`
	FailureThresholdCapacity int    `yaml:"failureThresholdCapacity"`
	HalfOpenAfter            string `yaml:"halfOpenAfter"`
	SuccessThresholdCount    int    `yaml:"successThresholdCount"`
	SuccessThresholdCapacity int    `yaml:"successThresholdCapacity"`
}

type TimeoutPolicyConfig struct {
	Duration string `yaml:"duration"`
}

type HedgePolicyConfig struct {
	Delay    string `yaml:"delay"`
	MaxCount int    `yaml:"maxCount"`
}

type RateLimiterConfig struct {
	Budgets []*RateLimitBudgetConfig `yaml:"budgets"`
}

type RateLimitBudgetConfig struct {
	Id    string                 `yaml:"id"`
	Rules []*RateLimitRuleConfig `yaml:"rules"`
}

type RateLimitRuleConfig struct {
	Scope    string `yaml:"scope"`
	Method   string `yaml:"method"`
	MaxCount int    `yaml:"maxCount"`
	Period   string `yaml:"period"`
	WaitTime string `yaml:"waitTime"`
}

type HealthCheckConfig struct {
	ScoreMetricsWindowSize string `yaml:"scoreMetricsWindowSize"`
}

type NetworkConfig struct {
	Architecture    NetworkArchitecture `yaml:"architecture"`
	RateLimitBudget string              `yaml:"rateLimitBudget"`
	Failsafe        *FailsafeConfig     `yaml:"failsafe"`
	Evm             *EvmNetworkConfig   `yaml:"evm"`
}

type EvmNetworkConfig struct {
	ChainId              int    `yaml:"chainId"`
	FinalityDepth        uint64 `yaml:"finalityDepth"`
	BlockTrackerInterval string `yaml:"blockTrackerInterval"`
}

type MetricsConfig struct {
	Enabled bool   `toml:"enabled"`
	Host    string `toml:"host"`
	Port    int    `toml:"port"`
}

var cfgInstance *Config

// LoadConfig loads the configuration from the specified file.
func LoadConfig(fs afero.Fs, filename string) (*Config, error) {
	data, err := afero.ReadFile(fs, filename)

	if err != nil {
		return nil, err
	}

	var cfg Config
	err = yaml.Unmarshal(data, &cfg)
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
	e.Str("scope", c.Scope).
		Str("method", c.Method).
		Int("maxCount", c.MaxCount).
		Str("period", c.Period).
		Str("waitTime", c.WaitTime)
}

func (c *NetworkConfig) NetworkId() string {
	switch c.Architecture {
	case "evm":
		return util.EvmNetworkId(c.Evm.ChainId)
	default:
		return ""
	}
}

func (c *ServerConfig) MarshalZerologObject(e *zerolog.Event) {
	e.Str("host", c.HttpHost).
		Int("port", c.HttpPort).
		Int("maxTimeoutMs", c.MaxTimeoutMs)
}
