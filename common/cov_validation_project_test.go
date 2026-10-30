package common

import (
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// cvConfigWithBudgets is the root config Project/Upstream/Network validation
// resolves rate-limit budgets and proxy pools against.
func cvConfigWithBudgets() *Config {
	return &Config{
		RateLimiters: &RateLimiterConfig{Budgets: []*RateLimitBudgetConfig{
			{Id: "plain", Rules: []*RateLimitRuleConfig{{Method: "*", MaxCount: 1}}},
			{Id: "priced", Rules: []*RateLimitRuleConfig{{Method: "*", MaxCount: 1}}, CreditUnits: map[string]int64{"eth_call": 2}},
			{Id: "credit-rule", Rules: []*RateLimitRuleConfig{{Method: "*", MaxCount: 1, CountMode: RateLimitCountModeCredit}}},
		}},
		ProxyPools: []*ProxyPoolConfig{{ID: "pool-a", Urls: []string{"http://p"}}, {ID: "pool-b", Urls: []string{"http://q"}}},
	}
}

func cvValidEvmNetwork(chainId int64) *NetworkConfig {
	return &NetworkConfig{
		Architecture: ArchitectureEvm,
		Evm: &EvmNetworkConfig{
			ChainId:                     chainId,
			FallbackFinalityDepth:       1024,
			FallbackStatePollerDebounce: Duration(time.Second),
			GetLogsMaxAllowedRange:      10_000,
		},
	}
}

func TestCovValidation_Project(t *testing.T) {
	cfg := cvConfigWithBudgets()
	baseline := func() *ProjectConfig {
		return &ProjectConfig{
			Id: "main",
			Providers: []*ProviderConfig{
				{Id: "alchemy", Vendor: "alchemy", UpstreamIdTemplate: "<PROVIDER>-<NETWORK>"},
			},
			Upstreams: []*UpstreamConfig{
				{Id: "u1", Endpoint: "http://a"},
				{Id: "u2", Endpoint: "http://b"},
			},
			UpstreamDefaults: &UpstreamConfig{Failsafe: []*FailsafeConfig{nil, {MatchMethod: "*"}}},
			NetworkDefaults:  &NetworkDefaults{CacheKeySuffix: "v2"},
			Networks: []*NetworkConfig{
				func() *NetworkConfig { n := cvValidEvmNetwork(1); n.Alias = "eth"; return n }(),
				cvValidEvmNetwork(10),
			},
			Auth:                  &AuthConfig{Strategies: []*AuthStrategyConfig{{Type: AuthTypeNetwork, Network: &NetworkStrategyConfig{}}}},
			CORS:                  &CORSConfig{AllowedOrigins: []string{"*"}},
			AllowClientDirectives: util.StringPtr("use-upstream | retry-*"),
			RateLimitBudget:       "plain",
			Integrity:             &IntegrityConfig{IntegritySettings: IntegritySettings{Level: "intrinsic"}},
		}
	}
	validate := func(p *ProjectConfig) error { return p.Validate(cfg) }
	cvRunCases(t, baseline, validate, []cvCase[ProjectConfig]{
		{"id missing", func(p *ProjectConfig) { p.Id = "" }, "project id is required"},
		{"invalid provider", func(p *ProjectConfig) { p.Providers[0].Vendor = "" }, "project.*.providers.*.vendor is required"},
		{"duplicate provider", func(p *ProjectConfig) { p.Providers = append(p.Providers, p.Providers[0]) }, "project.*.providers.*.id must be unique, 'alchemy' is duplicated"},
		{"invalid upstream", func(p *ProjectConfig) { p.Upstreams[0].Endpoint = "" }, "upstream.*.endpoint is required"},
		{"duplicate upstream", func(p *ProjectConfig) { p.Upstreams[1].Id = "u1" }, "project.*.upstreams.*.id must be unique, 'u1' is duplicated"},
		{"no upstreams nor providers", func(p *ProjectConfig) { p.Upstreams, p.Providers = nil, nil }, "project.*.upstreams or project.*.providers is required"},
		{"providers only", func(p *ProjectConfig) { p.Upstreams = nil }, ""},
		{"upstreamDefaults consensus", func(p *ProjectConfig) {
			p.UpstreamDefaults.Failsafe[1].Consensus = &ConsensusPolicyConfig{}
		}, "project.*.upstreamDefaults: failsafe.consensus is only supported for network-level failsafe"},
		{"upstreamDefaults matchCommitment", func(p *ProjectConfig) {
			p.UpstreamDefaults.Failsafe[1].MatchCommitment = []string{"confirmed"}
		}, "project.*.upstreamDefaults: failsafe.matchCommitment is only supported for network-level failsafe"},
		{"networkDefaults bad suffix", func(p *ProjectConfig) { p.NetworkDefaults.CacheKeySuffix = "v2!" }, "networkDefaults.cacheKeySuffix 'v2!' must contain only alphanumeric characters"},
		{"invalid network", func(p *ProjectConfig) { p.Networks[1].Architecture = "" }, "network.*.architecture is required"},
		{"duplicate network id", func(p *ProjectConfig) { p.Networks[1].Evm.ChainId = 1 }, "project.*.networks.*.id must be unique, 'evm:1' is duplicated"},
		{"duplicate alias", func(p *ProjectConfig) { p.Networks[1].Alias = "eth" }, "project.*.networks.*.alias must be unique, 'eth' is duplicated"},
		{"svm upstream chain mismatch", func(p *ProjectConfig) {
			p.Upstreams = append(p.Upstreams, &UpstreamConfig{
				Id: "sol", Type: UpstreamTypeSvm, Endpoint: "http://sol",
				Svm: &SvmUpstreamConfig{Chain: "eclipse", Cluster: "mainnet-beta"},
			})
			p.Networks = append(p.Networks, &NetworkConfig{Architecture: ArchitectureSvm, Svm: &SvmNetworkConfig{Cluster: "mainnet-beta"}})
		}, "upstream.*.svm.chain 'eclipse' (upstream 'sol') matches no network.*.svm.chain for cluster 'mainnet-beta'"},
		{"jsonrpc upstream slug mismatch", func(p *ProjectConfig) {
			p.Upstreams = append(p.Upstreams, &UpstreamConfig{
				Id: "stark", Type: UpstreamTypeJsonRpc, Endpoint: "http://stark",
				Generic: &GenericUpstreamConfig{Slug: "starknet-sepolia"},
			})
			p.Networks = append(p.Networks, &NetworkConfig{Architecture: ArchitectureJsonRpc, Generic: &GenericNetworkConfig{Slug: "starknet"}})
		}, "upstream.*.generic.slug 'starknet-sepolia' (upstream 'stark') matches no network.*.generic.slug"},
		{"jsonrpc upstream without network is lazy", func(p *ProjectConfig) {
			p.Upstreams = append(p.Upstreams, &UpstreamConfig{
				Id: "stark", Type: UpstreamTypeJsonRpc, Endpoint: "http://stark",
				Generic: &GenericUpstreamConfig{Slug: "starknet-sepolia"},
			})
		}, ""},
		{"invalid auth", func(p *ProjectConfig) { p.Auth.Strategies = nil }, "project.*.auth.strategies is required"},
		{"invalid cors", func(p *ProjectConfig) { p.CORS.AllowedOrigins = nil }, "*.cors.allowedOrigins is required"},
		{"bad allowClientDirectives", func(p *ProjectConfig) { p.AllowClientDirectives = util.StringPtr("(use-upstream") }, "project.*.allowClientDirectives pattern is invalid"},
		{"empty allowClientDirectives", func(p *ProjectConfig) { p.AllowClientDirectives = util.StringPtr("") }, ""},
		{"unknown budget", func(p *ProjectConfig) { p.RateLimitBudget = "nope" }, "project.*.rateLimitBudget 'nope' does not exist in config.rateLimiters"},
		{"invalid integrity", func(p *ProjectConfig) { p.Integrity.Level = "paranoid" }, "project.*: integrity.level 'paranoid' is invalid"},
	})
}

func TestCovValidation_Provider(t *testing.T) {
	cfg := cvConfigWithBudgets()
	baseline := func() *ProviderConfig {
		return &ProviderConfig{
			Id: "p", Vendor: "drpc", UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
			OnlyNetworks: []string{"evm:1", "svm:mainnet-beta"},
			Overrides:    map[string]*UpstreamConfig{"evm:1": {RateLimitBudget: "plain"}},
		}
	}
	validate := func(p *ProviderConfig) error { return p.Validate(cfg) }
	cvRunCases(t, baseline, validate, []cvCase[ProviderConfig]{
		{"id missing", func(p *ProviderConfig) { p.Id = "" }, "project.*.providers.*.id is required"},
		{"vendor missing", func(p *ProviderConfig) { p.Vendor = "" }, "project.*.providers.*.vendor is required"},
		{"template missing", func(p *ProviderConfig) { p.UpstreamIdTemplate = "" }, "project.*.providers.*.upstreamIdTemplate is required"},
		{"only and ignore together", func(p *ProviderConfig) { p.IgnoreNetworks = []string{} }, "onlyNetworks and project.*.providers.*.ignoreNetworks are mutually exclusive"},
		{"override skips endpoint but checks budget", func(p *ProviderConfig) {
			p.Overrides["evm:1"].RateLimitBudget = "nope"
		}, "upstream.*.rateLimitBudget 'nope' does not exist in config.rateLimiters"},
		{"bad onlyNetworks", func(p *ProviderConfig) { p.OnlyNetworks = []string{"evm:0"} }, "project.*.providers.*.onlyNetworks.* 'evm:0' is invalid must be like evm:1"},
		{"bad ignoreNetworks", func(p *ProviderConfig) {
			p.OnlyNetworks, p.IgnoreNetworks = nil, []string{"ethereum"}
		}, "project.*.providers.*.ignoreNetworks.* 'ethereum' is invalid must be like evm:1"},
		{"valid ignoreNetworks", func(p *ProviderConfig) { p.OnlyNetworks, p.IgnoreNetworks = nil, []string{"evm:137"} }, ""},
	})
}

func TestCovValidation_AuthConfig(t *testing.T) {
	dbStrategy := func(connId string) *AuthStrategyConfig {
		return &AuthStrategyConfig{Type: AuthTypeDatabase, Database: &DatabaseStrategyConfig{Connector: cvMemoryConnector(connId)}}
	}
	baseline := func() *AuthConfig {
		return &AuthConfig{Strategies: []*AuthStrategyConfig{dbStrategy("db1"), dbStrategy("db2")}}
	}
	cvRunCases(t, baseline, (*AuthConfig).Validate, []cvCase[AuthConfig]{
		{"no strategies", func(a *AuthConfig) { a.Strategies = nil }, "project.*.auth.strategies is required, add at least one strategy"},
		{"invalid strategy", func(a *AuthConfig) { a.Strategies[0].Type = "oauth" }, "auth.*.type 'oauth' is invalid must be one of"},
		{"duplicate database connector", func(a *AuthConfig) { a.Strategies[1].Database.Connector.Id = "db1" }, "database auth strategy connector ID 'db1' is not unique"},
	})
}

func TestCovValidation_AuthStrategy(t *testing.T) {
	baseline := func() *AuthStrategyConfig {
		return &AuthStrategyConfig{Type: AuthTypeSecret, Secret: &SecretStrategyConfig{Value: "s3cret"}}
	}
	cvRunCases(t, baseline, (*AuthStrategyConfig).Validate, []cvCase[AuthStrategyConfig]{
		{"type missing", func(s *AuthStrategyConfig) { s.Type = "" }, "auth.*.type is required"},
		{"network strategy without block", func(s *AuthStrategyConfig) { s.Type = AuthTypeNetwork }, "auth.*.network is required for network strategy"},
		{"network strategy", func(s *AuthStrategyConfig) { s.Type, s.Network = AuthTypeNetwork, &NetworkStrategyConfig{} }, ""},
		{"secret strategy without block", func(s *AuthStrategyConfig) { s.Secret = nil }, "auth.*.secret is required for secret strategy"},
		{"secret value missing", func(s *AuthStrategyConfig) { s.Secret.Value = "" }, "auth.*.secret.value is required"},
		{"jwt strategy without block", func(s *AuthStrategyConfig) { s.Type = AuthTypeJwt }, "auth.*.jwt is required for jwt strategy"},
		{"jwt strategy invalid", func(s *AuthStrategyConfig) { s.Type, s.Jwt = AuthTypeJwt, &JwtStrategyConfig{} }, "auth.*.jwt.verificationKeys or auth.*.jwt.verificationJwksUrl is required"},
		{"jwt strategy", func(s *AuthStrategyConfig) {
			s.Type, s.Jwt = AuthTypeJwt, &JwtStrategyConfig{VerificationKeys: map[string]string{"k": "v"}}
		}, ""},
		{"siwe strategy without block", func(s *AuthStrategyConfig) { s.Type = AuthTypeSiwe }, "auth.*.siwe is required for siwe strategy"},
		{"siwe strategy", func(s *AuthStrategyConfig) { s.Type, s.Siwe = AuthTypeSiwe, &SiweStrategyConfig{} }, ""},
		{"database strategy without block", func(s *AuthStrategyConfig) { s.Type = AuthTypeDatabase }, "auth.*.database is required for database strategy"},
		{"database strategy invalid", func(s *AuthStrategyConfig) {
			s.Type, s.Database = AuthTypeDatabase, &DatabaseStrategyConfig{}
		}, "auth.*.database.connector is required"},
	})
}

func TestCovValidation_DatabaseStrategy(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	baseline := func() *DatabaseStrategyConfig {
		ttl := time.Minute
		return &DatabaseStrategyConfig{
			Connector: cvMemoryConnector("auth"),
			Cache:     &DatabaseStrategyCacheConfig{TTL: &ttl, MaxSize: i64(10), MaxCost: i64(10), NumCounters: i64(100)},
		}
	}
	cvRunCases(t, baseline, (*DatabaseStrategyConfig).Validate, []cvCase[DatabaseStrategyConfig]{
		{"connector missing", func(s *DatabaseStrategyConfig) { s.Connector = nil }, "auth.*.database.connector is required"},
		{"negative ttl", func(s *DatabaseStrategyConfig) { d := -time.Second; s.Cache.TTL = &d }, "auth.*.database.cache.ttl must be non-negative"},
		{"zero maxSize", func(s *DatabaseStrategyConfig) { s.Cache.MaxSize = i64(0) }, "auth.*.database.cache.maxSize must be positive"},
		{"zero maxCost", func(s *DatabaseStrategyConfig) { s.Cache.MaxCost = i64(0) }, "auth.*.database.cache.maxCost must be positive"},
		{"negative numCounters", func(s *DatabaseStrategyConfig) { s.Cache.NumCounters = i64(-1) }, "auth.*.database.cache.numCounters must be positive"},
		{"invalid connector", func(s *DatabaseStrategyConfig) { s.Connector.Driver = "x" }, "database.*.connector.driver 'x' is invalid"},
		{"no cache block", func(s *DatabaseStrategyConfig) { s.Cache = nil }, ""},
	})
}

func TestCovValidation_Upstream(t *testing.T) {
	cfg := cvConfigWithBudgets()
	baseline := func() *UpstreamConfig {
		return &UpstreamConfig{
			Id:       "u1",
			Endpoint: "https://rpc.example",
			Evm:      &EvmUpstreamConfig{ChainId: 1, StatePollerInterval: Duration(time.Second)},
			Failsafe: []*FailsafeConfig{{MatchMethod: "*", Retry: &RetryPolicyConfig{BackoffFactor: 2, BackoffMaxDelay: Duration(time.Second)}}},
			JsonRpc:  &JsonRpcUpstreamConfig{SupportsBatch: util.BoolPtr(true), BatchMaxSize: 10, BatchMaxWait: Duration(time.Millisecond), ProxyPool: "pool-b"},
			Grpc:     &GrpcUpstreamConfig{PoolSize: 2},
			RateLimitAutoTune: &RateLimitAutoTuneConfig{
				Enabled: util.BoolPtr(true), AdjustmentPeriod: Duration(time.Minute),
				ErrorRateThreshold: 0.1, IncreaseFactor: 1.05, DecreaseFactor: 0.9,
			},
			RateLimitCountMode: RateLimitCountModeCredit,
			RateLimitBudget:    "plain",
		}
	}
	validate := func(u *UpstreamConfig) error { return u.Validate(cfg, false) }
	cvRunCases(t, baseline, validate, []cvCase[UpstreamConfig]{
		{"endpoint missing", func(u *UpstreamConfig) { u.Endpoint = "" }, "upstream.*.endpoint is required"},
		{"evm invalid", func(u *UpstreamConfig) { u.Evm.StatePollerInterval = 0 }, "upstream.*.evm.statePollerInterval is required"},
		{"svm invalid", func(u *UpstreamConfig) {
			u.Type, u.Evm, u.Svm = UpstreamTypeSvm, nil, &SvmUpstreamConfig{}
		}, "upstream.*.svm.cluster is required for svm upstreams"},
		{"jsonrpc without slug", func(u *UpstreamConfig) {
			u.Type, u.Evm = UpstreamTypeJsonRpc, nil
		}, "upstream.*.generic.slug is required for jsonrpc upstreams"},
		{"jsonrpc bad slug", func(u *UpstreamConfig) {
			u.Type, u.Evm, u.Generic = UpstreamTypeJsonRpc, nil, &GenericUpstreamConfig{Slug: "star:net"}
		}, "upstream.*.generic.slug 'star:net' is invalid"},
		{"jsonrpc rejects evm block", func(u *UpstreamConfig) {
			u.Type, u.Generic = UpstreamTypeJsonRpc, &GenericUpstreamConfig{Slug: "starknet"}
		}, "upstream.*.evm is not supported on jsonrpc upstreams"},
		{"jsonrpc rejects svm block", func(u *UpstreamConfig) {
			u.Type, u.Evm, u.Svm, u.Generic = UpstreamTypeJsonRpc, nil, &SvmUpstreamConfig{}, &GenericUpstreamConfig{Slug: "starknet"}
		}, "upstream.*.svm is not supported on jsonrpc upstreams"},
		{"generic block on evm upstream", func(u *UpstreamConfig) {
			u.Generic = &GenericUpstreamConfig{Slug: "starknet"}
		}, "upstream.*.generic is only valid on upstreams with type: jsonrpc"},
		{"jsonrpc valid", func(u *UpstreamConfig) {
			u.Type, u.Evm, u.Generic = UpstreamTypeJsonRpc, nil, &GenericUpstreamConfig{Slug: "starknet"}
		}, ""},
		{"failsafe invalid", func(u *UpstreamConfig) { u.Failsafe[0].MatchMethod = "" }, "failsafe.matchMethod cannot be empty"},
		{"failsafe consensus", func(u *UpstreamConfig) {
			u.Failsafe[0].Consensus = &ConsensusPolicyConfig{MaxParticipants: 3, AgreementThreshold: 2}
		}, "upstream 'u1': failsafe.consensus is only supported for network-level failsafe"},
		{"jsonRpc batch without wait", func(u *UpstreamConfig) { u.JsonRpc.BatchMaxWait = 0 }, "jsonRpc.batchMaxWait is required and must be greater than 0"},
		{"jsonRpc batch without size", func(u *UpstreamConfig) { u.JsonRpc.BatchMaxSize = 0 }, "jsonRpc.batchMaxSize must be greater than 0"},
		{"jsonRpc unknown proxy pool", func(u *UpstreamConfig) { u.JsonRpc.ProxyPool = "pool-z" }, "jsonRpc.proxyPool 'pool-z' does not exist in configured proxyPools, must be one of: [pool-a pool-b]"},
		{"jsonRpc batch disabled ignores sizes", func(u *UpstreamConfig) {
			u.JsonRpc = &JsonRpcUpstreamConfig{SupportsBatch: util.BoolPtr(false)}
		}, ""},
		{"grpc pool negative", func(u *UpstreamConfig) { u.Grpc.PoolSize = -2 }, "upstream.*.grpc.poolSize must not be negative"},
		{"grpc health check on mounted path", func(u *UpstreamConfig) {
			u.Endpoint = "grpc://node:443/v1"
			u.Evm.ChainId = 0
			u.Grpc.HealthCheckService = "svc"
		}, "upstream.*.grpc.healthCheckService cannot be used with"},
		{"autoTune invalid", func(u *UpstreamConfig) { u.RateLimitAutoTune.AdjustmentPeriod = 0 }, "upstream.*.rateLimitAutoTune.adjustmentPeriod is required"},
		{"unknown count mode", func(u *UpstreamConfig) { u.RateLimitCountMode = "tokens" }, "upstream.*.rateLimitCountMode 'tokens' is invalid"},
		{"unknown budget", func(u *UpstreamConfig) { u.RateLimitBudget = "nope" }, "upstream.*.rateLimitBudget 'nope' does not exist in config.rateLimiters"},
		{"credit mode with creditUnits budget", func(u *UpstreamConfig) { u.RateLimitBudget = "priced" }, "but its budget 'priced' also defines creditUnits"},
		{"credit mode with credit-rule budget", func(u *UpstreamConfig) { u.RateLimitBudget = "credit-rule" }, "but its budget 'credit-rule' also sets rules.*.countMode"},
		{"request mode with priced budget", func(u *UpstreamConfig) {
			u.RateLimitCountMode, u.RateLimitBudget = RateLimitCountModeRequest, "priced"
		}, ""},
	})

	t.Run("skipEndpointCheck allows empty endpoint", func(t *testing.T) {
		u := baseline()
		u.Endpoint = ""
		u.Evm.ChainId = 0
		require.NoError(t, u.Validate(cfg, true))
	})
}

func TestCovValidation_RateLimitAutoTune(t *testing.T) {
	baseline := func() *RateLimitAutoTuneConfig {
		return &RateLimitAutoTuneConfig{
			Enabled: util.BoolPtr(true), AdjustmentPeriod: Duration(time.Minute),
			ErrorRateThreshold: 1, IncreaseFactor: 1.5, DecreaseFactor: 0.5, MinBudget: 0, MaxBudget: 100,
		}
	}
	cvRunCases(t, baseline, (*RateLimitAutoTuneConfig).Validate, []cvCase[RateLimitAutoTuneConfig]{
		{"disabled skips checks", func(r *RateLimitAutoTuneConfig) { r.Enabled = util.BoolPtr(false); r.IncreaseFactor = 0 }, ""},
		{"nil enabled skips checks", func(r *RateLimitAutoTuneConfig) { r.Enabled = nil; r.AdjustmentPeriod = 0 }, ""},
		{"adjustmentPeriod", func(r *RateLimitAutoTuneConfig) { r.AdjustmentPeriod = 0 }, "upstream.*.rateLimitAutoTune.adjustmentPeriod is required"},
		{"errorRateThreshold zero", func(r *RateLimitAutoTuneConfig) { r.ErrorRateThreshold = 0 }, "upstream.*.rateLimitAutoTune.errorRateThreshold must be greater than 0 and less than 1"},
		{"errorRateThreshold above one", func(r *RateLimitAutoTuneConfig) { r.ErrorRateThreshold = 1.01 }, "errorRateThreshold must be greater than 0"},
		{"increaseFactor", func(r *RateLimitAutoTuneConfig) { r.IncreaseFactor = 1 }, "upstream.*.rateLimitAutoTune.increaseFactor must be greater than 1"},
		{"decreaseFactor zero", func(r *RateLimitAutoTuneConfig) { r.DecreaseFactor = 0 }, "upstream.*.rateLimitAutoTune.decreaseFactor must be greater than 0 and less than 1"},
		{"decreaseFactor one", func(r *RateLimitAutoTuneConfig) { r.DecreaseFactor = 1 }, "decreaseFactor must be greater than 0 and less than 1"},
		{"minBudget negative", func(r *RateLimitAutoTuneConfig) { r.MinBudget = -1 }, "upstream.*.rateLimitAutoTune.minBudget must be greater than or equal to 0"},
		{"maxBudget negative", func(r *RateLimitAutoTuneConfig) { r.MaxBudget = -1 }, "upstream.*.rateLimitAutoTune.maxBudget must be greater than or equal to 0"},
	})
}

func TestCovValidation_EvmUpstream(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	u := &UpstreamConfig{Endpoint: "https://rpc.example"}
	baseline := func() *EvmUpstreamConfig {
		return &EvmUpstreamConfig{
			ChainId:             1,
			StatePollerInterval: Duration(time.Second),
			NodeType:            EvmNodeTypeArchive,
			BlockAvailability: &EvmBlockAvailabilityConfig{
				Lower: &EvmAvailabilityBoundConfig{LatestBlockMinus: i64(1000), Probe: EvmProbeEventLogs},
				Upper: &EvmAvailabilityBoundConfig{LatestBlockMinus: i64(0)},
			},
			Multicall3: &EvmMulticall3Config{
				MaxWait: Duration(time.Millisecond), MaxCalls: 2,
				BypassContracts: []string{"0x" + "11223344556677889900aabbccddeeff00112233"},
			},
		}
	}
	validate := func(e *EvmUpstreamConfig) error { return e.Validate(u) }
	cvRunCases(t, baseline, validate, []cvCase[EvmUpstreamConfig]{
		{"statePollerInterval", func(e *EvmUpstreamConfig) { e.StatePollerInterval = 0 }, "upstream.*.evm.statePollerInterval is required"},
		{"unknown nodeType", func(e *EvmUpstreamConfig) { e.NodeType = "light" }, "upstream.*.evm.nodeType 'light' is invalid must be one of"},
		{"invalid lower bound", func(e *EvmUpstreamConfig) { e.BlockAvailability.Lower = &EvmAvailabilityBoundConfig{} }, "upstream.*.evm.blockAvailability.lower is invalid: bound must set exactly one of"},
		{"invalid upper bound", func(e *EvmUpstreamConfig) { e.BlockAvailability.Upper.Probe = "receipts" }, "upstream.*.evm.blockAvailability.upper is invalid: bound.probe 'receipts' is invalid"},
		{"latestBlockMinus inverted", func(e *EvmUpstreamConfig) { e.BlockAvailability.Lower.LatestBlockMinus = i64(0); e.BlockAvailability.Upper.LatestBlockMinus = i64(5) }, "lower.latestBlockMinus must be >= upper.latestBlockMinus"},
		{"exactBlock inverted", func(e *EvmUpstreamConfig) {
			e.BlockAvailability.Lower = &EvmAvailabilityBoundConfig{ExactBlock: i64(10)}
			e.BlockAvailability.Upper = &EvmAvailabilityBoundConfig{ExactBlock: i64(5)}
		}, "upstream.*.evm.blockAvailability.lower.exactBlock must be <= upper.exactBlock"},
		{"earliestBlockPlus inverted", func(e *EvmUpstreamConfig) {
			e.BlockAvailability.Lower = &EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(10)}
			e.BlockAvailability.Upper = &EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(5)}
		}, "lower.earliestBlockPlus must be <= upper.earliestBlockPlus"},
		{"multicall maxWait", func(e *EvmUpstreamConfig) { e.Multicall3.MaxWait = 0 }, "upstream.*.evm.multicall3.maxWait must be greater than 0"},
		{"multicall maxCalls", func(e *EvmUpstreamConfig) { e.Multicall3.MaxCalls = 1 }, "upstream.*.evm.multicall3.maxCalls must be at least 2"},
		{"multicall short bypass address", func(e *EvmUpstreamConfig) { e.Multicall3.BypassContracts = []string{"0x1234"} }, "upstream.*.evm.multicall3.bypassContracts entry '0x1234' is not a 20-byte hex address"},
		{"multicall non-hex bypass address", func(e *EvmUpstreamConfig) { e.Multicall3.BypassContracts = []string{"0xzz"} }, "bypassContracts entry '0xzz' is not a 20-byte hex address"},
	})

	t.Run("chainId rejected on non-native endpoint", func(t *testing.T) {
		e := baseline()
		err := e.Validate(&UpstreamConfig{Endpoint: "alchemy://key"})
		require.ErrorContains(t, err, "upstream.*.evm.chainId must be 0 for non-http endpoints, but '1' is provided")
	})
	t.Run("zero chainId accepted on non-native endpoint", func(t *testing.T) {
		e := baseline()
		e.ChainId = 0
		require.NoError(t, e.Validate(&UpstreamConfig{Endpoint: "alchemy://key"}))
	})
}

func TestCovValidation_EvmAvailabilityBound(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	baseline := func() *EvmAvailabilityBoundConfig {
		return &EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(5), Probe: EvmProbeTraceData, UpdateRate: Duration(time.Minute)}
	}
	cvRunCases(t, baseline, (*EvmAvailabilityBoundConfig).Validate, []cvCase[EvmAvailabilityBoundConfig]{
		{"none set", func(b *EvmAvailabilityBoundConfig) { b.EarliestBlockPlus = nil }, "bound must set exactly one of: exactBlock, latestBlockMinus, earliestBlockPlus"},
		{"two set", func(b *EvmAvailabilityBoundConfig) { b.ExactBlock = i64(1) }, "bound fields exactBlock, latestBlockMinus, earliestBlockPlus are mutually exclusive"},
		{"exact with probe", func(b *EvmAvailabilityBoundConfig) {
			b.EarliestBlockPlus, b.ExactBlock, b.UpdateRate = nil, i64(1), 0
		}, "bound.probe must be empty when exactBlock is set"},
		{"exact with updateRate", func(b *EvmAvailabilityBoundConfig) {
			b.EarliestBlockPlus, b.ExactBlock, b.Probe = nil, i64(1), ""
		}, "bound.updateRate must be 0 when exactBlock is set"},
		{"exact negative", func(b *EvmAvailabilityBoundConfig) {
			*b = EvmAvailabilityBoundConfig{ExactBlock: i64(-1)}
		}, "bound.exactBlock must be >= 0"},
		{"exact valid", func(b *EvmAvailabilityBoundConfig) { *b = EvmAvailabilityBoundConfig{ExactBlock: i64(0)} }, ""},
		{"latest negative", func(b *EvmAvailabilityBoundConfig) { b.EarliestBlockPlus, b.LatestBlockMinus = nil, i64(-1) }, "bound.latestBlockMinus must be >= 0"},
		{"earliest negative", func(b *EvmAvailabilityBoundConfig) { b.EarliestBlockPlus = i64(-1) }, "bound.earliestBlockPlus must be >= 0"},
		{"negative updateRate", func(b *EvmAvailabilityBoundConfig) { b.UpdateRate = Duration(-time.Second) }, "bound.updateRate must be >= 0"},
		{"latest with updateRate only warns", func(b *EvmAvailabilityBoundConfig) { b.EarliestBlockPlus, b.LatestBlockMinus = nil, i64(3) }, ""},
		{"empty probe defaults", func(b *EvmAvailabilityBoundConfig) { b.Probe = "" }, ""},
	})
}
