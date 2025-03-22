package common

import (
	"bytes"
	"fmt"
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
	Admin        *AdminConfig       `yaml:"admin,omitempty" json:"admin"`
	Database     *DatabaseConfig    `yaml:"database,omitempty" json:"database"`
	Projects     []*ProjectConfig   `yaml:"projects,omitempty" json:"projects"`
	RateLimiters *RateLimiterConfig `yaml:"rateLimiters,omitempty" json:"rateLimiters"`
	Metrics      *MetricsConfig     `yaml:"metrics,omitempty" json:"metrics"`
	ProxyPools   []*ProxyPoolConfig `yaml:"proxyPools,omitempty" json:"proxyPools"`
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

type ServerConfig struct {
	ListenV4     *bool           `yaml:"listenV4,omitempty" json:"listenV4"`
	HttpHostV4   *string         `yaml:"httpHostV4,omitempty" json:"httpHostV4"`
	ListenV6     *bool           `yaml:"listenV6,omitempty" json:"listenV6"`
	HttpHostV6   *string         `yaml:"httpHostV6,omitempty" json:"httpHostV6"`
	HttpPort     *int            `yaml:"httpPort,omitempty" json:"httpPort"`
	MaxTimeout   *Duration       `yaml:"maxTimeout,omitempty" json:"maxTimeout" tstype:"Duration"`
	ReadTimeout  *Duration       `yaml:"readTimeout,omitempty" json:"readTimeout" tstype:"Duration"`
	WriteTimeout *Duration       `yaml:"writeTimeout,omitempty" json:"writeTimeout" tstype:"Duration"`
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
	MaxItems int `yaml:"maxItems" json:"maxItems"`
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
	Addr         string     `yaml:"addr" json:"addr"`
	Password     string     `yaml:"password" json:"-"`
	DB           int        `yaml:"db" json:"db"`
	TLS          *TLSConfig `yaml:"tls" json:"tls"`
	ConnPoolSize int        `yaml:"connPoolSize" json:"connPoolSize"`
	InitTimeout  Duration   `yaml:"initTimeout,omitempty" json:"initTimeout" tstype:"Duration"`
	GetTimeout   Duration   `yaml:"getTimeout,omitempty" json:"getTimeout" tstype:"Duration"`
	SetTimeout   Duration   `yaml:"setTimeout,omitempty" json:"setTimeout" tstype:"Duration"`
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
	StatePollInterval Duration       `yaml:"statePollInterval,omitempty" json:"statePollInterval" tstype:"Duration"`
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
	Enabled            *bool    `yaml:"enabled" json:"enabled"`
	AdjustmentPeriod   Duration `yaml:"adjustmentPeriod" json:"adjustmentPeriod" tstype:"Duration"`
	ErrorRateThreshold float64  `yaml:"errorRateThreshold" json:"errorRateThreshold"`
	IncreaseFactor     float64  `yaml:"increaseFactor" json:"increaseFactor"`
	DecreaseFactor     float64  `yaml:"decreaseFactor" json:"decreaseFactor"`
	MinBudget          int      `yaml:"minBudget" json:"minBudget"`
	MaxBudget          int      `yaml:"maxBudget" json:"maxBudget"`
}

type JsonRpcUpstreamConfig struct {
	SupportsBatch *bool             `yaml:"supportsBatch,omitempty" json:"supportsBatch"`
	BatchMaxSize  int               `yaml:"batchMaxSize,omitempty" json:"batchMaxSize"`
	BatchMaxWait  Duration          `yaml:"batchMaxWait,omitempty" json:"batchMaxWait" tstype:"Duration"`
	EnableGzip    *bool             `yaml:"enableGzip,omitempty" json:"enableGzip"`
	Headers       map[string]string `yaml:"headers,omitempty" json:"headers"`
	ProxyPool     string            `yaml:"proxyPool,omitempty" json:"proxyPool"`
}

type EvmUpstreamConfig struct {
	ChainId                  int64       `yaml:"chainId" json:"chainId"`
	NodeType                 EvmNodeType `yaml:"nodeType,omitempty" json:"nodeType"`
	StatePollerInterval      Duration    `yaml:"statePollerInterval,omitempty" json:"statePollerInterval" tstype:"Duration"`
	StatePollerDebounce      Duration    `yaml:"statePollerDebounce,omitempty" json:"statePollerDebounce" tstype:"Duration"`
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
	MaxAttempts     int      `yaml:"maxAttempts" json:"maxAttempts"`
	Delay           Duration `yaml:"delay,omitempty" json:"delay" tstype:"Duration"`
	BackoffMaxDelay Duration `yaml:"backoffMaxDelay,omitempty" json:"backoffMaxDelay" tstype:"Duration"`
	BackoffFactor   float32  `yaml:"backoffFactor,omitempty" json:"backoffFactor"`
	Jitter          Duration `yaml:"jitter,omitempty" json:"jitter" tstype:"Duration"`
}

type CircuitBreakerPolicyConfig struct {
	FailureThresholdCount    uint     `yaml:"failureThresholdCount" json:"failureThresholdCount"`
	FailureThresholdCapacity uint     `yaml:"failureThresholdCapacity" json:"failureThresholdCapacity"`
	HalfOpenAfter            Duration `yaml:"halfOpenAfter,omitempty" json:"halfOpenAfter" tstype:"Duration"`
	SuccessThresholdCount    uint     `yaml:"successThresholdCount" json:"successThresholdCount"`
	SuccessThresholdCapacity uint     `yaml:"successThresholdCapacity" json:"successThresholdCapacity"`
}

type TimeoutPolicyConfig struct {
	Duration Duration `yaml:"duration,omitempty" json:"duration" tstype:"Duration"`
}

type HedgePolicyConfig struct {
	Delay    Duration `yaml:"delay,omitempty" json:"delay" tstype:"Duration"`
	MaxCount int      `yaml:"maxCount" json:"maxCount"`
	Quantile float64  `yaml:"quantile,omitempty" json:"quantile"`
	MinDelay Duration `yaml:"minDelay,omitempty" json:"minDelay" tstype:"Duration"`
	MaxDelay Duration `yaml:"maxDelay,omitempty" json:"maxDelay" tstype:"Duration"`
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

type HealthCheckConfig struct {
	ScoreMetricsWindowSize Duration `yaml:"scoreMetricsWindowSize" json:"scoreMetricsWindowSize" tstype:"Duration"`
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

type MetricsConfig struct {
	Enabled  *bool   `yaml:"enabled" json:"enabled"`
	ListenV4 *bool   `yaml:"listenV4" json:"listenV4"`
	HostV4   *string `yaml:"hostV4" json:"hostV4"`
	ListenV6 *bool   `yaml:"listenV6" json:"listenV6"`
	HostV6   *string `yaml:"hostV6" json:"hostV6"`
	Port     *int    `yaml:"port" json:"port"`
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
