package config

import (
	"github.com/rs/zerolog"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v2"
)

// Config represents the configuration of the application.
type Config struct {
	LogLevel     string             `yaml:"logLevel"`
	Server       *ServerConfig      `yaml:"server"`
	Store        *StoreConfig       `yaml:"store"`
	Projects     []*ProjectConfig   `yaml:"projects"`
	RateLimiters *RateLimiterConfig `yaml:"rateLimiters"`
	HealthChecks *HealthCheckConfig `yaml:"healthChecks"`
	Metrics      *MetricsConfig     `yaml:"metrics"`
}

type ServerConfig struct {
	HttpHost     string `yaml:"httpHost"`
	HttpPort     string `yaml:"httpPort"`
	MaxTimeoutMs int    `yaml:"maxTimeoutMs"`
}

type StoreConfig struct {
	Driver string       `yaml:"driver"`
	Memory *MemoryStore `yaml:"memory"`
	Redis  *RedisStore  `yaml:"redis"`
}

type MemoryStore struct {
	MaxSize string `yaml:"maxSize"`
}

type RedisStore struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type ProjectConfig struct {
	Id              string            `yaml:"id"`
	Upstreams       []*UpstreamConfig `yaml:"upstreams"`
	Networks        []*NetworkConfig  `yaml:"networks"`
	RateLimitBucket string            `yaml:"rateLimitBucket"`
}

type UpstreamConfig struct {
	Id                 string            `yaml:"id"`
	Architecture       string            `yaml:"architecture,omitempty"`
	Endpoint           string            `yaml:"endpoint"`
	Metadata           map[string]string `yaml:"metadata"`
	Failsafe           *FailsafeConfig   `yaml:"failsafe"`
	RateLimitBucket    string            `yaml:"rateLimitBucket"`
	SupportedMethods   []string          `yaml:"supportedMethods"`
	UnsupportedMethods []string          `yaml:"unsupportedMethods"`
	CreditUnitMapping  string            `yaml:"creditUnitMapping"`
	HealthCheckGroup   string            `yaml:"healthCheckGroup"`
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
	Buckets []*RateLimitBucketConfig `yaml:"buckets"`
}

type RateLimitBucketConfig struct {
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
	Groups []*HealthCheckGroupConfig `yaml:"groups"`
}

func (c *HealthCheckConfig) GetGroupConfig(groupId string) *HealthCheckGroupConfig {
	for _, group := range c.Groups {
		if group.Id == groupId {
			return group
		}
	}

	return nil
}

type HealthCheckGroupConfig struct {
	Id                  string `yaml:"id"`
	CheckInterval       string `yaml:"checkInterval"`
	MaxErrorRatePercent int    `yaml:"maxErrorRatePercent"`
	MaxP90Latency       string `yaml:"maxP90Latency"`
	MaxBlocksLag        int    `yaml:"maxBlocksLag"`
}

type NetworkConfig struct {
	Architecture    string          `yaml:"architecture"`
	NetworkId       string          `yaml:"networkId"`
	RateLimitBucket string          `yaml:"rateLimitBucket"`
	Failsafe        *FailsafeConfig `yaml:"failsafe"`
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
