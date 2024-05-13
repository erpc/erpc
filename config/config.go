package config

import (
	"github.com/spf13/afero"
	"gopkg.in/yaml.v2"
)

// Config represents the configuration of the application.
type Config struct {
	LogLevel     string             `yaml:"logLevel"`
	Server       ServerConfig       `yaml:"server"`
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
	Retry          RetryPolicyConfig          `yaml:"retry"`
	CircuitBreaker CircuitBreakerPolicyConfig `yaml:"circuitBreaker"`
	Timeout        TimeoutPolicyConfig        `yaml:"timeout"`
	Hedge          HedgePolicyConfig          `yaml:"hedge"`
}

type RetryPolicyConfig struct {
	MaxCount        int     `yaml:"maxCount"`
	Delay           string  `yaml:"delay"`
	BackoffMaxDelay string  `yaml:"backoffMaxDelay"`
	BackoffFactor   float64 `yaml:"backoffFactor"`
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
	Mode     string `yaml:"mode"`
	Method   string `yaml:"method"`
	MaxCount int    `yaml:"maxCount"`
	Period   string `yaml:"period"`
	WaitTime string `yaml:"waitTime"`
}

type HealthCheckConfig struct {
	Groups []*HealthCheckGroupConfig `yaml:"groups"`
}

type HealthCheckGroupConfig struct {
	Id                  string `yaml:"id"`
	CheckIntervalMs     int    `yaml:"checkIntervalMs"`
	MaxErrorRatePercent int    `yaml:"maxErrorRatePercent"`
	MaxP90LatencyMs     int    `yaml:"maxP90LatencyMs"`
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

	return &cfg, nil
}
