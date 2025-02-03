package common

import (
	"bytes"
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
	LogLevel     string             `yaml:"logLevel" json:"logLevel" tstype:"LogLevel"`
	Server       *ServerConfig      `yaml:"server" json:"server"`
	Admin        *AdminConfig       `yaml:"admin" json:"admin"`
	Database     *DatabaseConfig    `yaml:"database" json:"database"`
	Projects     []*ProjectConfig   `yaml:"projects" json:"projects"`
	RateLimiters *RateLimiterConfig `yaml:"rateLimiters" json:"rateLimiters"`
	Metrics      *MetricsConfig     `yaml:"metrics" json:"metrics"`
	ProxyPools   []*ProxyPoolConfig `yaml:"proxyPools,omitempty" json:"proxyPools"`
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
	ListenV4     *bool           `yaml:"listenV4,omitempty" json:"listenV4"`
	HttpHostV4   *string         `yaml:"httpHostV4,omitempty" json:"httpHostV4"`
	ListenV6     *bool           `yaml:"listenV6,omitempty" json:"listenV6"`
	HttpHostV6   *string         `yaml:"httpHostV6,omitempty" json:"httpHostV6"`
	HttpPort     *int            `yaml:"httpPort,omitempty" json:"httpPort"`
	MaxTimeout   *string         `yaml:"maxTimeout,omitempty" json:"maxTimeout"`
	ReadTimeout  *string         `yaml:"readTimeout,omitempty" json:"readTimeout"`
	WriteTimeout *string         `yaml:"writeTimeout,omitempty" json:"writeTimeout"`
	EnableGzip   *bool           `yaml:"enableGzip,omitempty" json:"enableGzip"`
	TLS          *TLSConfig      `yaml:"tls,omitempty" json:"tls"`
	Aliasing     *AliasingConfig `yaml:"aliasing" json:"aliasing"`
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
	EvmJsonRpcCache *CacheConfig `yaml:"evmJsonRpcCache" json:"evmJsonRpcCache"`
}

type CacheConfig struct {
	Connectors []*ConnectorConfig            `yaml:"connectors,omitempty" json:"connectors" tstype:"TsConnectorConfig[]"`
	Policies   []*CachePolicyConfig          `yaml:"policies,omitempty" json:"policies"`
	Methods    map[string]*CacheMethodConfig `yaml:"methods,omitempty" json:"methods"`
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
	Finality    DataFinalityState  `yaml:"finality,omitempty" json:"finality"`
	Empty       CacheEmptyBehavior `yaml:"empty,omitempty" json:"empty"`
	MinItemSize *string            `yaml:"minItemSize,omitempty" json:"minItemSize" tstype:"ByteSize"`
	MaxItemSize *string            `yaml:"maxItemSize,omitempty" json:"maxItemSize" tstype:"ByteSize"`
	TTL         time.Duration      `yaml:"ttl,omitempty" json:"ttl"`
}

func (c *CachePolicyConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawCachePolicyConfig struct {
		Connector   string             `yaml:"connector"`
		Network     string             `yaml:"network,omitempty"`
		Method      string             `yaml:"method,omitempty"`
		Params      []interface{}      `yaml:"params,omitempty"`
		Finality    DataFinalityState  `yaml:"finality,omitempty"`
		Empty       CacheEmptyBehavior `yaml:"empty,omitempty"`
		MinItemSize *string            `yaml:"minItemSize,omitempty"`
		MaxItemSize *string            `yaml:"maxItemSize,omitempty"`
		TTL         interface{}        `yaml:"ttl"`
	}
	raw := rawCachePolicyConfig{}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	*c = CachePolicyConfig{
		Connector:   raw.Connector,
		Network:     raw.Network,
		Method:      raw.Method,
		Params:      raw.Params,
		Finality:    raw.Finality,
		Empty:       raw.Empty,
		MinItemSize: raw.MinItemSize,
		MaxItemSize: raw.MaxItemSize,
	}
	if raw.TTL != nil {
		switch v := raw.TTL.(type) {
		case string:
			ttl, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("failed to parse ttl: %v", err)
			}
			c.TTL = ttl
		case int:
			c.TTL = time.Duration(v) * time.Second
		default:
			return fmt.Errorf("invalid ttl type: %T", v)
		}
	} else {
		// Set default value of 0 when TTL is not specified, which means no ttl
		c.TTL = time.Duration(0)
	}
	return nil
}

func (c *CachePolicyConfig) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]interface{}{
		"network":   c.Network,
		"method":    c.Method,
		"params":    c.Params,
		"finality":  c.Finality,
		"ttl":       c.TTL,
		"connector": c.Connector,
	})
}

type ConnectorDriverType string

const (
	DriverMemory     ConnectorDriverType = "memory"
	DriverRedis      ConnectorDriverType = "redis"
	DriverPostgreSQL ConnectorDriverType = "postgresql"
	DriverDynamoDB   ConnectorDriverType = "dynamodb"
)

type ConnectorConfig struct {
	Id         string                     `yaml:"id" json:"id"`
	Driver     ConnectorDriverType        `yaml:"driver" json:"driver" tstype:"TsConnectorDriverType"`
	Memory     *MemoryConnectorConfig     `yaml:"memory,omitempty" json:"memory,omitempty"`
	Redis      *RedisConnectorConfig      `yaml:"redis,omitempty" json:"redis,omitempty"`
	DynamoDB   *DynamoDBConnectorConfig   `yaml:"dynamodb,omitempty" json:"dynamodb,omitempty"`
	PostgreSQL *PostgreSQLConnectorConfig `yaml:"postgresql,omitempty" json:"postgresql,omitempty"`
}

type MemoryConnectorConfig struct {
	MaxItems int `yaml:"maxItems" json:"maxItems"`
}

type TLSConfig struct {
	Enabled            bool   `yaml:"enabled" json:"enabled"`
	CertFile           string `yaml:"certFile" json:"certFile"`
	KeyFile            string `yaml:"keyFile" json:"keyFile"`
	CAFile             string `yaml:"caFile,omitempty" json:"caFile"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify,omitempty" json:"insecureSkipVerify"`
}

type RedisConnectorConfig struct {
	Addr         string        `yaml:"addr" json:"addr"`
	Password     string        `yaml:"password" json:"-"`
	DB           int           `yaml:"db" json:"db"`
	TLS          *TLSConfig    `yaml:"tls" json:"tls"`
	ConnPoolSize int           `yaml:"connPoolSize" json:"connPoolSize"`
	InitTimeout  time.Duration `yaml:"initTimeout,omitempty" json:"initTimeout"`
	GetTimeout   time.Duration `yaml:"getTimeout,omitempty" json:"getTimeout"`
	SetTimeout   time.Duration `yaml:"setTimeout,omitempty" json:"setTimeout"`
}

func (r *RedisConnectorConfig) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]interface{}{
		"addr":         r.Addr,
		"password":     "REDACTED",
		"db":           r.DB,
		"connPoolSize": r.ConnPoolSize,
		"tls":          r.TLS,
		"initTimeout":  r.InitTimeout.String(),
		"getTimeout":   r.GetTimeout.String(),
		"setTimeout":   r.SetTimeout.String(),
	})
}

type DynamoDBConnectorConfig struct {
	Table            string         `yaml:"table" json:"table"`
	Region           string         `yaml:"region" json:"region"`
	Endpoint         string         `yaml:"endpoint" json:"endpoint"`
	Auth             *AwsAuthConfig `yaml:"auth" json:"auth"`
	PartitionKeyName string         `yaml:"partitionKeyName" json:"partitionKeyName"`
	RangeKeyName     string         `yaml:"rangeKeyName" json:"rangeKeyName"`
	ReverseIndexName string         `yaml:"reverseIndexName" json:"reverseIndexName"`
	TTLAttributeName string         `yaml:"ttlAttributeName" json:"ttlAttributeName"`
	InitTimeout      time.Duration  `yaml:"initTimeout,omitempty" json:"initTimeout"`
	GetTimeout       time.Duration  `yaml:"getTimeout,omitempty" json:"getTimeout"`
	SetTimeout       time.Duration  `yaml:"setTimeout,omitempty" json:"setTimeout"`
}

type PostgreSQLConnectorConfig struct {
	ConnectionUri string        `yaml:"connectionUri" json:"connectionUri"`
	Table         string        `yaml:"table" json:"table"`
	MinConns      int32         `yaml:"minConns,omitempty" json:"minConns"`
	MaxConns      int32         `yaml:"maxConns,omitempty" json:"maxConns"`
	InitTimeout   time.Duration `yaml:"initTimeout,omitempty" json:"initTimeout"`
	GetTimeout    time.Duration `yaml:"getTimeout,omitempty" json:"getTimeout"`
	SetTimeout    time.Duration `yaml:"setTimeout,omitempty" json:"setTimeout"`
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
	Id               string             `yaml:"id" json:"id"`
	Auth             *AuthConfig        `yaml:"auth,omitempty" json:"auth"`
	CORS             *CORSConfig        `yaml:"cors,omitempty" json:"cors"`
	Providers        []*ProviderConfig  `yaml:"providers,omitempty" json:"providers"`
	UpstreamDefaults *UpstreamConfig    `yaml:"upstreamDefaults,omitempty" json:"upstreamDefaults"`
	Upstreams        []*UpstreamConfig  `yaml:"upstreams,omitempty" json:"upstreams"`
	NetworkDefaults  *NetworkDefaults   `yaml:"networkDefaults,omitempty" json:"networkDefaults"`
	Networks         []*NetworkConfig   `yaml:"networks,omitempty" json:"networks"`
	RateLimitBudget  string             `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget"`
	HealthCheck      *HealthCheckConfig `yaml:"healthCheck,omitempty" json:"healthCheck"`
}

type NetworkDefaults struct {
	RateLimitBudget   string                   `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget"`
	Failsafe          *FailsafeConfig          `yaml:"failsafe,omitempty" json:"failsafe"`
	SelectionPolicy   *SelectionPolicyConfig   `yaml:"selectionPolicy,omitempty" json:"selectionPolicy"`
	DirectiveDefaults *DirectiveDefaultsConfig `yaml:"directiveDefaults,omitempty" json:"directiveDefaults"`
	Evm               *EvmNetworkConfig        `yaml:"evm,omitempty" json:"evm"`
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
	AdjustmentPeriod   string  `yaml:"adjustmentPeriod" json:"adjustmentPeriod" tstype:"Duration"`
	ErrorRateThreshold float64 `yaml:"errorRateThreshold" json:"errorRateThreshold"`
	IncreaseFactor     float64 `yaml:"increaseFactor" json:"increaseFactor"`
	DecreaseFactor     float64 `yaml:"decreaseFactor" json:"decreaseFactor"`
	MinBudget          int     `yaml:"minBudget" json:"minBudget"`
	MaxBudget          int     `yaml:"maxBudget" json:"maxBudget"`
}

type JsonRpcUpstreamConfig struct {
	SupportsBatch *bool             `yaml:"supportsBatch,omitempty" json:"supportsBatch"`
	BatchMaxSize  int               `yaml:"batchMaxSize,omitempty" json:"batchMaxSize"`
	BatchMaxWait  string            `yaml:"batchMaxWait,omitempty" json:"batchMaxWait"`
	EnableGzip    *bool             `yaml:"enableGzip,omitempty" json:"enableGzip"`
	Headers       map[string]string `yaml:"headers,omitempty" json:"headers"`
	ProxyPool     string            `yaml:"proxyPool,omitempty" json:"proxyPool"`
}

type EvmUpstreamConfig struct {
	ChainId                  int64       `yaml:"chainId" json:"chainId"`
	NodeType                 EvmNodeType `yaml:"nodeType,omitempty" json:"nodeType"`
	StatePollerInterval      string      `yaml:"statePollerInterval,omitempty" json:"statePollerInterval"`
	StatePollerDebounce      string      `yaml:"statePollerDebounce,omitempty" json:"statePollerDebounce"`
	MaxAvailableRecentBlocks int64       `yaml:"maxAvailableRecentBlocks,omitempty" json:"maxAvailableRecentBlocks"`
	GetLogsMaxBlockRange     int64       `yaml:"getLogsMaxBlockRange,omitempty" json:"getLogsMaxBlockRange"`
}

type FailsafeConfig struct {
	Retry          *RetryPolicyConfig          `yaml:"retry" json:"retry"`
	CircuitBreaker *CircuitBreakerPolicyConfig `yaml:"circuitBreaker" json:"circuitBreaker"`
	Timeout        *TimeoutPolicyConfig        `yaml:"timeout" json:"timeout"`
	Hedge          *HedgePolicyConfig          `yaml:"hedge" json:"hedge"`
	Consensus      *ConsensusPolicyConfig      `yaml:"consensus" json:"consensus"`
}

type RetryPolicyConfig struct {
	MaxAttempts        int     `yaml:"maxAttempts" json:"maxAttempts"`
	Delay              string  `yaml:"delay,omitempty" json:"delay"`
	BackoffMaxDelay    string  `yaml:"backoffMaxDelay,omitempty" json:"backoffMaxDelay"`
	BackoffFactor      float32 `yaml:"backoffFactor,omitempty" json:"backoffFactor"`
	Jitter             string  `yaml:"jitter,omitempty" json:"jitter"`
	IgnoreClientErrors bool    `yaml:"ignoreClientErrors,omitempty" json:"ignoreClientErrors"`
}

type CircuitBreakerPolicyConfig struct {
	FailureThresholdCount    uint   `yaml:"failureThresholdCount" json:"failureThresholdCount"`
	FailureThresholdCapacity uint   `yaml:"failureThresholdCapacity" json:"failureThresholdCapacity"`
	HalfOpenAfter            string `yaml:"halfOpenAfter" json:"halfOpenAfter"`
	SuccessThresholdCount    uint   `yaml:"successThresholdCount" json:"successThresholdCount"`
	SuccessThresholdCapacity uint   `yaml:"successThresholdCapacity" json:"successThresholdCapacity"`
}

type TimeoutPolicyConfig struct {
	Duration string `yaml:"duration" json:"duration" tstype:"Duration"`
}

type HedgePolicyConfig struct {
	Delay    string  `yaml:"delay" json:"delay"`
	MaxCount int     `yaml:"maxCount" json:"maxCount"`
	Quantile float64 `yaml:"quantile" json:"quantile"`
	MinDelay string  `yaml:"minDelay" json:"minDelay" tstype:"Duration"`
	MaxDelay string  `yaml:"maxDelay" json:"maxDelay" tstype:"Duration"`
}

type ConsensusFailureBehavior string

const (
	ConsensusFailureBehaviorReturnError           ConsensusFailureBehavior = "returnError"
	ConsensusFailureBehaviorAcceptAnyValidResult  ConsensusFailureBehavior = "acceptAnyValidResult"
	ConsensusFailureBehaviorPreferBlockHeadLeader ConsensusFailureBehavior = "preferBlockHeadLeader"
	ConsensusFailureBehaviorOnlyBlockHeadLeader   ConsensusFailureBehavior = "onlyBlockHeadLeader"
)

type ConsensusLowParticipantsBehavior string

const (
	ConsensusLowParticipantsBehaviorReturnError           ConsensusLowParticipantsBehavior = "returnError"
	ConsensusLowParticipantsBehaviorAcceptAnyValidResult  ConsensusLowParticipantsBehavior = "acceptAnyValidResult"
	ConsensusLowParticipantsBehaviorPreferBlockHeadLeader ConsensusLowParticipantsBehavior = "preferBlockHeadLeader"
	ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader   ConsensusLowParticipantsBehavior = "onlyBlockHeadLeader"
)

type ConsensusDisputeBehavior string

const (
	ConsensusDisputeBehaviorReturnError           ConsensusDisputeBehavior = "returnError"
	ConsensusDisputeBehaviorAcceptAnyValidResult  ConsensusDisputeBehavior = "acceptAnyValidResult"
	ConsensusDisputeBehaviorPreferBlockHeadLeader ConsensusDisputeBehavior = "preferBlockHeadLeader"
	ConsensusDisputeBehaviorOnlyBlockHeadLeader   ConsensusDisputeBehavior = "onlyBlockHeadLeader"
)

type ConsensusPolicyConfig struct {
	RequiredParticipants    int                              `yaml:"requiredParticipants" json:"requiredParticipants"`
	AgreementThreshold      int                              `yaml:"agreementThreshold,omitempty" json:"agreementThreshold"`
	FailureBehavior         ConsensusFailureBehavior         `yaml:"failureBehavior,omitempty" json:"failureBehavior"`
	DisputeBehavior         ConsensusDisputeBehavior         `yaml:"disputeBehavior,omitempty" json:"disputeBehavior"`
	LowParticipantsBehavior ConsensusLowParticipantsBehavior `yaml:"lowParticipantsBehavior,omitempty" json:"lowParticipantsBehavior"`
	PunishMisbehavior       *PunishMisbehaviorConfig         `yaml:"punishMisbehavior,omitempty" json:"punishMisbehavior"`
}

type PunishMisbehaviorConfig struct {
	DisputeThreshold int    `yaml:"disputeThreshold" json:"disputeThreshold"`
	SitOutPenalty    string `yaml:"sitOutPenalty,omitempty" json:"sitOutPenalty"`
}

type RateLimiterConfig struct {
	Budgets []*RateLimitBudgetConfig `yaml:"budgets" json:"budgets" tstype:"RateLimitBudgetConfig[]"`
}

type RateLimitBudgetConfig struct {
	Id    string                 `yaml:"id" json:"id"`
	Rules []*RateLimitRuleConfig `yaml:"rules" json:"rules" tstype:"RateLimitRuleConfig[]"`
}

type RateLimitRuleConfig struct {
	Method   string `yaml:"method" json:"method"`
	MaxCount uint   `yaml:"maxCount" json:"maxCount"`
	Period   string `yaml:"period" json:"period" tstype:"Duration"`
	WaitTime string `yaml:"waitTime" json:"waitTime" tstype:"Duration"`
}

type ProxyPoolConfig struct {
	ID   string   `yaml:"id" json:"id"`
	Urls []string `yaml:"urls" json:"urls"`
}

type HealthCheckConfig struct {
	ScoreMetricsWindowSize string `yaml:"scoreMetricsWindowSize" json:"scoreMetricsWindowSize"`
}

type NetworkConfig struct {
	Architecture      NetworkArchitecture      `yaml:"architecture" json:"architecture" tstype:"TsNetworkArchitecture"`
	RateLimitBudget   string                   `yaml:"rateLimitBudget,omitempty" json:"rateLimitBudget"`
	Failsafe          *FailsafeConfig          `yaml:"failsafe,omitempty" json:"failsafe"`
	Evm               *EvmNetworkConfig        `yaml:"evm,omitempty" json:"evm"`
	SelectionPolicy   *SelectionPolicyConfig   `yaml:"selectionPolicy,omitempty" json:"selectionPolicy"`
	DirectiveDefaults *DirectiveDefaultsConfig `yaml:"directiveDefaults,omitempty" json:"directiveDefaults"`
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
	FallbackStatePollerDebounce string              `yaml:"fallbackStatePollerDebounce,omitempty" json:"fallbackStatePollerDebounce"`
	Integrity                   *EvmIntegrityConfig `yaml:"integrity,omitempty" json:"integrity"`
}

type EvmIntegrityConfig struct {
	EnforceHighestBlock      *bool `yaml:"enforceHighestBlock,omitempty" json:"enforceHighestBlock"`
	EnforceGetLogsBlockRange *bool `yaml:"enforceGetLogsBlockRange,omitempty" json:"enforceGetLogsBlockRange"`
}

type SelectionPolicyConfig struct {
	EvalInterval     time.Duration  `yaml:"evalInterval,omitempty" json:"evalInterval"`
	EvalFunction     sobek.Callable `yaml:"evalFunction,omitempty" json:"evalFunction" tstype:"SelectionPolicyEvalFunction | undefined"`
	EvalPerMethod    bool           `yaml:"evalPerMethod,omitempty" json:"evalPerMethod"`
	ResampleExcluded bool           `yaml:"resampleExcluded,omitempty" json:"resampleExcluded"`
	ResampleInterval time.Duration  `yaml:"resampleInterval,omitempty" json:"resampleInterval"`
	ResampleCount    int            `yaml:"resampleCount,omitempty" json:"resampleCount"`

	evalFunctionOriginal string `yaml:"-" json:"-"`
}

func (c *SelectionPolicyConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawSelectionPolicyConfig struct {
		EvalInterval     string `yaml:"evalInterval"`
		EvalPerMethod    bool   `yaml:"evalPerMethod"`
		EvalFunction     string `yaml:"evalFunction"`
		ResampleInterval string `yaml:"resampleInterval"`
		ResampleCount    int    `yaml:"resampleCount"`
		ResampleExcluded bool   `yaml:"resampleExcluded"`
	}
	raw := rawSelectionPolicyConfig{}

	if err := unmarshal(&raw); err != nil {
		return err
	}

	if raw.ResampleInterval != "" {
		resampleInterval, err := time.ParseDuration(raw.ResampleInterval)
		if err != nil {
			return fmt.Errorf("failed to parse resampleInterval: %v", err)
		}
		c.ResampleInterval = resampleInterval
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
	c.ResampleCount = raw.ResampleCount

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

type MetricsConfig struct {
	Enabled  *bool   `yaml:"enabled" json:"enabled"`
	ListenV4 *bool   `yaml:"listenV4" json:"listenV4"`
	HostV4   *string `yaml:"hostV4" json:"hostV4"`
	ListenV6 *bool   `yaml:"listenV6" json:"listenV6"`
	HostV6   *string `yaml:"hostV6" json:"hostV6"`
	Port     *int    `yaml:"port" json:"port"`
}

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
		decoder := yaml.NewDecoder(bytes.NewReader(expandedData))
		decoder.KnownFields(true)
		err = decoder.Decode(&cfg)
		if err != nil {
			return nil, err
		}
	}

	err = cfg.SetDefaults()
	if err != nil {
		return nil, err
	}

	err = cfg.Validate()
	if err != nil {
		return nil, err
	}

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
