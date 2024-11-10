package common

import (
	"fmt"
	"os"
	"reflect"
	"time"

	"strings"

	"github.com/bytedance/sonic"
	"github.com/clarkmcc/go-typescript"
	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/buffer"
	"github.com/dop251/goja_nodejs/console"
	"github.com/dop251/goja_nodejs/process"
	"github.com/dop251/goja_nodejs/require"
	"github.com/dop251/goja_nodejs/url"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
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
	Admin        *AdminConfig       `yaml:"admin" json:"admin"`
	Database     *DatabaseConfig    `yaml:"database" json:"database"`
	Projects     []*ProjectConfig   `yaml:"projects" json:"projects"`
	RateLimiters *RateLimiterConfig `yaml:"rateLimiters" json:"rateLimiters"`
	Metrics      *MetricsConfig     `yaml:"metrics" json:"metrics"`
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
	CORS *CORSConfig `yaml:"cors" json:"cors"`
}

func (a *AdminConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawAdminConfig AdminConfig
	raw := rawAdminConfig{
		// In context of eRPC admin endpoint, enforcing CORS origin is not really necessary
		// because we do not use cookies or other credentials that are exploitable in a
		// cross-origin attack context. Therefore a default value of "*" for allowed origins
		// is acceptable as the attacker will not have access to admin-token when attempting
		// from a different origin than the trusted one.
		CORS: &CORSConfig{
			AllowedOrigins: []string{"*"},
			AllowedMethods: []string{"GET", "POST", "OPTIONS"},
			AllowedHeaders: []string{
				"content-type",
				"authorization",
				"x-erpc-secret-token",
			},
			AllowCredentials: false,
			MaxAge:           3600,
		},
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}

	*a = AdminConfig(raw)
	return nil
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
	Type                         UpstreamType             `yaml:"type" json:"type"`
	Group                        string                   `yaml:"group" json:"group"`
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
	Routing                      *RoutingConfig           `yaml:"routing" json:"routing"`
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

var DefaultScoreMultiplier = &ScoreMultiplierConfig{
	Network: "*",
	Method:  "*",

	ErrorRate:       8.0,
	P90Latency:      4.0,
	TotalRequests:   1.0,
	ThrottledRate:   3.0,
	BlockHeadLag:    2.0,
	FinalizationLag: 1.0,

	Overall: 1.0,
}

func (p *ScoreMultiplierConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawScoreMultiplierConfig ScoreMultiplierConfig
	raw := rawScoreMultiplierConfig{
		Network: DefaultScoreMultiplier.Network,
		Method:  DefaultScoreMultiplier.Method,

		ErrorRate:       DefaultScoreMultiplier.ErrorRate,
		P90Latency:      DefaultScoreMultiplier.P90Latency,
		TotalRequests:   DefaultScoreMultiplier.TotalRequests,
		ThrottledRate:   DefaultScoreMultiplier.ThrottledRate,
		BlockHeadLag:    DefaultScoreMultiplier.BlockHeadLag,
		FinalizationLag: DefaultScoreMultiplier.FinalizationLag,

		Overall: DefaultScoreMultiplier.Overall,
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}

	if raw.Overall <= 0 {
		return fmt.Errorf("priorityMultipliers.*.overall multiplier must be greater than 0")
	} else if raw.ErrorRate < 0 {
		return fmt.Errorf("priorityMultipliers.*.errorRate multiplier must be greater than or equal to 0")
	} else if raw.P90Latency < 0 {
		return fmt.Errorf("priorityMultipliers.*.p90latency multiplier must be greater than or equal to 0")
	} else if raw.TotalRequests < 0 {
		return fmt.Errorf("priorityMultipliers.*.totalRequests multiplier must be greater than or equal to 0")
	} else if raw.ThrottledRate < 0 {
		return fmt.Errorf("priorityMultipliers.*.throttledRate multiplier must be greater than or equal to 0")
	} else if raw.BlockHeadLag < 0 {
		return fmt.Errorf("priorityMultipliers.*.blockHeadLag multiplier must be greater than or equal to 0")
	} else if raw.FinalizationLag < 0 {
		return fmt.Errorf("priorityMultipliers.*.finalizationLag multiplier must be greater than or equal to 0")
	}

	*p = ScoreMultiplierConfig(raw)
	return nil
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
	Architecture    NetworkArchitecture    `yaml:"architecture" json:"architecture"`
	RateLimitBudget string                 `yaml:"rateLimitBudget" json:"rateLimitBudget"`
	Failsafe        *FailsafeConfig        `yaml:"failsafe" json:"failsafe"`
	Evm             *EvmNetworkConfig      `yaml:"evm" json:"evm"`
	SelectionPolicy *SelectionPolicyConfig `yaml:"selectionPolicy" json:"selectionPolicy"`
}

type EvmNetworkConfig struct {
	ChainId              int64  `yaml:"chainId" json:"chainId"`
	FinalityDepth        int64  `yaml:"finalityDepth" json:"finalityDepth"`
	BlockTrackerInterval string `yaml:"blockTrackerInterval" json:"blockTrackerInterval"`
}

type SelectionPolicyConfig struct {
	EvalInterval  time.Duration `yaml:"evalInterval" json:"evalInterval"`
	EvalFunction  goja.Callable `yaml:"evalFunction" json:"evalFunction"`
	EvalPerMethod bool          `yaml:"evalPerMethod" json:"evalPerMethod"`
	SampleAfter   time.Duration `yaml:"sampleAfter" json:"sampleAfter"`
	SampleCount   int           `yaml:"sampleCount" json:"sampleCount"`
}

func (c *SelectionPolicyConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Use raw config to parse strings before converting
	type rawSelectionPolicyConfig struct {
		EvalInterval  string `yaml:"evalInterval"`
		EvalPerMethod bool   `yaml:"evalPerMethod"`
		EvalFunction  string `yaml:"evalFunction"`
		SampleAfter   string `yaml:"sampleAfter"`
		SampleCount   int    `yaml:"sampleCount"`
	}
	raw := rawSelectionPolicyConfig{
		EvalInterval:  "1m",
		SampleAfter:   "5m",
		EvalPerMethod: false,
		SampleCount:   10,
	}

	if err := unmarshal(&raw); err != nil {
		return err
	}

	sampleAfter, err := time.ParseDuration(raw.SampleAfter)
	if err != nil {
		return fmt.Errorf("failed to parse sampleAfter: %v", err)
	}

	evalInterval, err := time.ParseDuration(raw.EvalInterval)
	if err != nil {
		return fmt.Errorf("failed to parse evalInterval: %v", err)
	}

	evalFunction, err := stringToGojaCallable(raw.EvalFunction)
	if err != nil {
		return fmt.Errorf("failed to parse evalFunction: %v", err)
	}

	c.EvalInterval = evalInterval
	c.EvalFunction = evalFunction
	c.EvalPerMethod = raw.EvalPerMethod
	c.SampleAfter = sampleAfter
	c.SampleCount = raw.SampleCount

	return nil
}

func (c *SelectionPolicyConfig) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]interface{}{
		"evalInterval":  c.EvalInterval,
		"evalPerMethod": c.EvalPerMethod,
		"sampleAfter":   c.SampleAfter,
		"sampleCount":   c.SampleCount,
	})
}

func stringToGojaCallable(funcStr string) (goja.Callable, error) {
	if funcStr == "" {
		return nil, nil
	}

	vm := goja.New()

	req := new(require.Registry)
	req.Enable(vm)

	// Enable Node.js modules support
	process.Enable(vm)
	console.Enable(vm)
	url.Enable(vm)
	buffer.Enable(vm)

	// Attempt as a plain JavaScript code
	wrappedFunc := fmt.Sprintf("(%s)", funcStr)
	program, errJs := goja.Compile("", wrappedFunc, false)
	if errJs != nil {
		// Handle TypeScript code
		result, err := typescript.Evaluate(
			strings.NewReader(funcStr),
			typescript.WithTranspile(),
			typescript.WithEvaluationRuntime(vm),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate TypeScript: '%v' nor JavaScript: %v", err, errJs)
		}

		// Get the function from the evaluated result
		if obj, ok := result.(*goja.Object); ok {
			if fn, ok := goja.AssertFunction(obj); ok {
				return fn, nil
			}
		}
		return nil, fmt.Errorf("TypeScript code must export a function")
	}

	result, err := vm.RunProgram(program)
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate function: %v", err)
	}

	evalFunction, ok := goja.AssertFunction(result)
	if !ok {
		return nil, fmt.Errorf("code must evaluate to a function")
	}

	return evalFunction, nil
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
// It supports both YAML and TypeScript (.ts) files.
func LoadConfig(fs afero.Fs, filename string) (*Config, error) {
	data, err := afero.ReadFile(fs, filename)
	if err != nil {
		return nil, err
	}

	var cfg Config

	if strings.HasSuffix(filename, ".ts") {
		cfgPtr, err := loadConfigFromTypescript(data)
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

	cfgInstance = &cfg

	return &cfg, nil
}

func loadConfigFromTypescript(data []byte) (*Config, error) {
	vm := goja.New()

	process.Enable(vm)
	console.Enable(vm)
	url.Enable(vm)
	buffer.Enable(vm)

	req := new(require.Registry)
	req.Enable(vm)

	result, err := typescript.Evaluate(
		strings.NewReader(string(data)),
		typescript.WithTranspile(),
		typescript.WithEvaluationRuntime(vm),
	)
	if err != nil {
		return nil, err
	}

	// Get the config object default-exported from the TS code
	v := result.(*goja.Object)
	if v == nil {
		return nil, fmt.Errorf("config object must be default exported from TypeScript code AND must be the last statement in the file")
	}

	var cfg Config
	err = mapTsToGo(v, &cfg)
	if err != nil {
		return nil, err
	}

	return &cfg, nil
}

func mapTsToGo(v goja.Value, dest interface{}) error {
	destVal := reflect.ValueOf(dest)
	if destVal.Kind() != reflect.Ptr || destVal.IsNil() {
		return fmt.Errorf("dest must be a non-nil pointer")
	}

	destElem := destVal.Elem()
	destType := destElem.Type()

	if destElem.Kind() != reflect.Struct {
		return fmt.Errorf("dest must point to a struct")
	}

	if v == nil || v == goja.Undefined() || v == goja.Null() {
		return nil
	}

	obj := v.(*goja.Object)
	if obj == nil {
		return fmt.Errorf("value is not an object")
	}

	for i := 0; i < destType.NumField(); i++ {
		field := destType.Field(i)
		fieldName := field.Name

		jsonTag := field.Tag.Get("json")
		if jsonTag != "" {
			fieldName = strings.Split(jsonTag, ",")[0]
		}

		jsValue := obj.Get(fieldName)
		if jsValue == nil || jsValue == goja.Undefined() {
			continue
		}

		fieldValue := destElem.Field(i)
		if !fieldValue.CanSet() {
			continue
		}

		err := setJsValueToGoField(jsValue, fieldValue)
		if err != nil {
			return fmt.Errorf("failed to set field %s: %v", fieldName, err)
		}
	}

	return nil
}

func setJsValueToGoField(jsValue goja.Value, fieldValue reflect.Value) error {
	if !fieldValue.CanSet() {
		return nil
	}

	switch fieldValue.Kind() {
	case reflect.Bool:
		fieldValue.SetBool(jsValue.ToBoolean())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		intVal := jsValue.ToInteger()
		fieldValue.SetInt(intVal)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		intVal := jsValue.ToInteger()
		fieldValue.SetInt(intVal)
	case reflect.Float32, reflect.Float64:
		floatVal := jsValue.ToFloat()
		fieldValue.SetFloat(floatVal)
	case reflect.String:
		strVal := jsValue.String()
		fieldValue.SetString(strVal)
	case reflect.Struct:
		// If the field is of type goja.Value
		if fieldValue.Type() == reflect.TypeOf(goja.Value(nil)) {
			fieldValue.Set(reflect.ValueOf(jsValue))
		} else {
			return mapTsToGo(jsValue, fieldValue.Addr().Interface())
		}
	case reflect.Ptr:
		if jsValue == goja.Undefined() || jsValue == goja.Null() {
			return nil
		}
		ptrValue := reflect.New(fieldValue.Type().Elem())
		err := setJsValueToGoField(jsValue, ptrValue.Elem())
		if err != nil {
			return err
		}
		fieldValue.Set(ptrValue)
	case reflect.Slice:
		if jsValue == goja.Undefined() || jsValue == goja.Null() {
			return nil
		}
		if jsValue.ExportType().Kind() != reflect.Slice {
			return fmt.Errorf("expected array but got %v", jsValue)
		}
		jsArray := jsValue.(*goja.Object)
		length := jsArray.Get("length").ToInteger()
		slice := reflect.MakeSlice(fieldValue.Type(), int(length), int(length))
		for i := 0; i < int(length); i++ {
			elemValue := jsArray.Get(fmt.Sprintf("%d", i))
			if elemValue == goja.Undefined() {
				return fmt.Errorf("expected array element %d to be a goja.Value", i)
			}
			err := setJsValueToGoField(elemValue, slice.Index(i))
			if err != nil {
				return err
			}
		}
		fieldValue.Set(slice)
	case reflect.Interface:
		fieldValue.Set(reflect.ValueOf(jsValue.Export()))
	default:
		return fmt.Errorf("unsupported kind: %v", fieldValue.Kind())
	}
	return nil
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

func NewDefaultNetworkConfig() *NetworkConfig {
	evalFunction, err := stringToGojaCallable(`
		(upstreams, method) => {
			const defaults = upstreams.filter(u => u.group !== 'fallback')
			const fallbacks = upstreams.filter(u => u.group === 'fallback')
			const maxErrorRate = parseFloat(process.env.ROUTING_POLICY_ERROR_RATE || '0.7')
			const maxBlockHeadLag = parseFloat(process.env.ROUTING_POLICY_BLOCK_LAG || '10')
			const healthyOnes = defaults.filter(u => u.errorRate < maxErrorRate || u.blockHeadLag < maxBlockHeadLag)
			if (healthyOnes.length > 0) {
			return healthyOnes
			}
			return fallbacks
		}
	`)
	if err != nil {
		log.Error().Err(err).Msg("failed to create eval function for NewDefaultNetworkConfig selection policy")
		return nil
	}
	selectionPolicy := &SelectionPolicyConfig{
		EvalInterval:  1 * time.Minute,
		EvalFunction:  evalFunction,
		EvalPerMethod: false,
		SampleAfter:   5 * time.Minute,
		SampleCount:   10,
	}
	return &NetworkConfig{
		Failsafe: &FailsafeConfig{
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
		},
		SelectionPolicy: selectionPolicy,
	}
}

func (c *NetworkConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawNetworkConfig NetworkConfig

	defCfg := NewDefaultNetworkConfig()
	raw := rawNetworkConfig{
		Architecture:    defCfg.Architecture,
		Evm:             defCfg.Evm,
		Failsafe:        defCfg.Failsafe,
		SelectionPolicy: defCfg.SelectionPolicy,
		RateLimitBudget: defCfg.RateLimitBudget,
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
