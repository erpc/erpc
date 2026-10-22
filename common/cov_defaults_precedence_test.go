package common

import (
	"net/url"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCovDefaults_BuildProviderSettings_Vendors(t *testing.T) {
	cases := []struct {
		endpoint string
		want     VendorSettings
	}{
		{"alchemy://k1", VendorSettings{"apiKey": "k1"}},
		{"blastapi://k1", VendorSettings{"apiKey": "k1"}},
		{"drpc://k1", VendorSettings{"apiKey": "k1"}},
		{"envio://rpc.hypersync.example", VendorSettings{"rootDomain": "rpc.hypersync.example"}},
		{"etherspot://k1", VendorSettings{"apiKey": "k1"}},
		{"infura://k1", VendorSettings{"apiKey": "k1"}},
		{"llama://k1", VendorSettings{"apiKey": "k1"}},
		{"pimlico://k1", VendorSettings{"apiKey": "k1"}},
		{"thirdweb://cid", VendorSettings{"clientId": "cid"}},
		{"dwellir://k1", VendorSettings{"apiKey": "k1"}},
		{"conduit://k1", VendorSettings{"apiKey": "k1"}},
		{"superchain://github.com", VendorSettings{"registryUrl": "github.com"}},
		{"superchain://github.com/ethereum-optimism/registry", VendorSettings{"registryUrl": "github.com/ethereum-optimism/registry"}},
		{"tenderly://k1", VendorSettings{"apiKey": "k1"}},
		{"onfinality://k1", VendorSettings{"apiKey": "k1"}},
		{"blockpi://k1", VendorSettings{"apiKey": "k1"}},
		{"ankr://k1", VendorSettings{"apiKey": "k1"}},
		{"satelink://k1", VendorSettings{"apiKey": "k1"}},
		{"blockdaemon://k1", VendorSettings{"apiKey": "k1"}},
		{"erpc://erpc.example/main/evm", VendorSettings{"endpoint": "https://erpc.example/main/evm"}},
		{"erpc://erpc.example/main?secret=s3", VendorSettings{"endpoint": "https://erpc.example/main", "secret": "s3"}},
		{"erpc://erpc.example/main?other=1", VendorSettings{"endpoint": "https://erpc.example/main"}},
		{"repository://evm-public-endpoints.erpc.cloud/list?x=1", VendorSettings{"repositoryUrl": "https://evm-public-endpoints.erpc.cloud/list?x=1"}},
		{"routemesh://lb.example/rpc/1/secret-key", VendorSettings{"baseURL": "lb.example", "apiKey": "secret-key"}},
		{"quicknode://k1?tagIds=42", VendorSettings{"apiKey": "k1", "tagIds": 42}},
		{"quicknode://k1?tagIds=abc", VendorSettings{"apiKey": "k1"}},
		{"quicknode://k1?tagIds=a,b", VendorSettings{"apiKey": "k1"}},
		{"quicknode://k1?tagLabels=+prod+", VendorSettings{"apiKey": "k1", "tagLabels": "prod"}},
		{"chainstack://k1?region=a&region=b", VendorSettings{"apiKey": "k1", "region": []string{"a", "b"}}},
		{"spectrum://team/key/?plan=pro&nodeType=archive", VendorSettings{"apiKey": "team/key", "plan": "pro", "nodeType": "archive"}},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			u, err := url.Parse(tc.endpoint)
			require.NoError(t, err)
			got, err := buildProviderSettings(u.Scheme, u)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCovDefaults_BuildProviderSettings_Errors(t *testing.T) {
	cases := []struct {
		endpoint string
		want     string
	}{
		{"routemesh://lb.example/v1/1/k", "routemesh endpoint path must be in format /rpc/<chainId>/<apiKey>, got: /v1/1/k"},
		{"nosuchvendor://k", "unsupported vendor name in vendor.settings: nosuchvendor"},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.endpoint)
		require.NoError(t, err)
		_, err = buildProviderSettings(u.Scheme, u)
		assert.EqualError(t, err, tc.want)
	}

	bad := &url.URL{Scheme: "goldsky", Host: "s", RawQuery: "a=%zz"}
	_, err := buildProviderSettings("goldsky", bad)
	assert.ErrorContains(t, err, "failed to parse goldsky query parameters")
	_, err = buildProviderSettings("quicknode", &url.URL{Host: "k", RawQuery: "a=%zz"})
	assert.ErrorContains(t, err, "failed to parse quicknode query parameters")
	_, err = buildProviderSettings("chainstack", &url.URL{Host: "k", RawQuery: "a=%zz"})
	assert.ErrorContains(t, err, "failed to parse chainstack query parameters")
	_, err = buildProviderSettings("spectrum", &url.URL{Host: "k", RawQuery: "a=%zz"})
	assert.ErrorContains(t, err, "failed to parse spectrum query parameters")
	_, err = buildProviderSettings("erpc", &url.URL{Host: "k", RawQuery: "a=%zz"})
	assert.ErrorContains(t, err, "failed to parse erpc query parameters")
}

func TestCovDefaults_ConvertUpstreamToProvider(t *testing.T) {
	for _, ep := range []string{"http://a", "https://a", "ws://a", "wss://a", "grpc://a", "grpc+bds://a", "grpcs://a"} {
		p, err := convertUpstreamToProvider(&UpstreamConfig{Endpoint: ep})
		require.NoError(t, err)
		assert.Nil(t, p, "%s is a native endpoint, not a vendor shorthand", ep)
	}

	u := &UpstreamConfig{Id: "my-alchemy", Endpoint: "evm+alchemy://KEY", RateLimitBudget: "rl", Evm: &EvmUpstreamConfig{ChainId: 1}}
	p, err := convertUpstreamToProvider(u)
	require.NoError(t, err)
	assert.Equal(t, "my-alchemy", p.Id)
	assert.Equal(t, "alchemy", p.Vendor, "evm+ prefix is stripped from the vendor")
	assert.Equal(t, VendorSettings{"apiKey": "KEY"}, p.Settings)
	assert.Equal(t, "<PROVIDER>-<NETWORK>", p.UpstreamIdTemplate)
	ov := p.Overrides["*"]
	require.NotNil(t, ov)
	assert.Empty(t, ov.Endpoint, "endpoint/id are left for the provider to fill")
	assert.NotEmpty(t, ov.Id, "the override is defaulted, which assigns a generated id")
	assert.NotEqual(t, "my-alchemy", ov.Id)
	assert.Equal(t, "rl", ov.RateLimitBudget)
	assert.Equal(t, int64(1), ov.Evm.ChainId)

	anon, err := convertUpstreamToProvider(&UpstreamConfig{Endpoint: "infura://K"})
	require.NoError(t, err)
	assert.Contains(t, anon.Id, "infura-", "id-less shorthand gets a vendor-prefixed id")

	_, err = convertUpstreamToProvider(&UpstreamConfig{Id: "x", Endpoint: "nosuch://K"})
	assert.EqualError(t, err, "failed to convert upstream (id: x) to provider: unsupported vendor name in vendor.settings: nosuch")
	_, err = convertUpstreamToProvider(&UpstreamConfig{Endpoint: "bad scheme://%%"})
	assert.ErrorContains(t, err, "invalid upstream endpoint while converting to provider")
}

func TestCovDefaults_ProjectSetDefaults_ShorthandUpstreamBecomesProvider(t *testing.T) {
	p := &ProjectConfig{
		Id: "p",
		UpstreamDefaults: &UpstreamConfig{
			RateLimitBudget: "global",
			Tags:            []string{"pool:a"},
		},
		NetworkDefaults: &NetworkDefaults{
			RateLimitBudget: "net-budget",
			Failsafe:        []*FailsafeConfig{{Retry: &RetryPolicyConfig{MaxAttempts: 7}}},
		},
		Upstreams: []*UpstreamConfig{
			{Id: "plain", Endpoint: "https://node.example", RateLimitBudget: "own"},
			{Id: "drpc-up", Endpoint: "drpc://KEY"},
		},
		Networks: []*NetworkConfig{{Architecture: "evm", Evm: &EvmNetworkConfig{ChainId: 1}}},
		Auth:     &AuthConfig{Strategies: []*AuthStrategyConfig{{Secret: &SecretStrategyConfig{Value: "x"}}}},
		CORS:     &CORSConfig{},
	}
	require.NoError(t, p.SetDefaults(nil))

	require.Len(t, p.Upstreams, 1, "the vendor shorthand is moved out of upstreams")
	plain := p.Upstreams[0]
	assert.Equal(t, "own", plain.RateLimitBudget, "explicit upstream value wins over upstreamDefaults")
	assert.Equal(t, []string{"pool:a"}, plain.Tags)
	require.NotNil(t, plain.RateLimitAutoTune, "a budget implies auto-tune defaults")
	assert.True(t, *plain.RateLimitAutoTune.Enabled)

	require.Len(t, p.Providers, 1)
	assert.Equal(t, "drpc-up", p.Providers[0].Id)
	assert.Equal(t, "drpc", p.Providers[0].Vendor)
	assert.Equal(t, "global", p.Providers[0].Overrides["*"].RateLimitBudget)

	n := p.Networks[0]
	assert.Equal(t, "net-budget", n.RateLimitBudget)
	require.Len(t, n.Failsafe, 1)
	assert.Equal(t, 7, n.Failsafe[0].Retry.MaxAttempts)
	assert.Equal(t, AuthTypeSecret, p.Auth.Strategies[0].Type)
	assert.Equal(t, 3600, p.CORS.MaxAge)
}

func TestCovDefaults_ProjectSetDefaults_Errors(t *testing.T) {
	redisBoth := &ConnectorConfig{Redis: &RedisConnectorConfig{URI: "redis://a", Addr: "b"}}
	_ = redisBoth
	badPolicy := &SelectionPolicyConfig{EvalScope: "nope"}
	cases := []struct {
		name string
		p    *ProjectConfig
		want string
	}{
		{"networkDefaults", &ProjectConfig{NetworkDefaults: &NetworkDefaults{SelectionPolicy: badPolicy}}, "failed to set defaults for network defaults"},
		{"upstream parse", &ProjectConfig{Upstreams: []*UpstreamConfig{{Endpoint: "://bad"}}}, "failed to set defaults for upstream: failed to parse endpoint"},
		{"upstream convert", &ProjectConfig{Upstreams: []*UpstreamConfig{{Id: "u", Endpoint: "nosuch://k"}}}, "failed to convert upstream (id: u) to provider"},
		{"endpoints option", &ProjectConfig{}, ""},
		{"network", &ProjectConfig{Upstreams: []*UpstreamConfig{{Id: "u", Endpoint: "https://a"}}, Networks: []*NetworkConfig{{SelectionPolicy: &SelectionPolicyConfig{EvalScope: "nope"}}}}, "failed to set defaults for network"},
		{"auth", &ProjectConfig{Upstreams: []*UpstreamConfig{{Id: "u", Endpoint: "https://a"}}, Auth: &AuthConfig{Strategies: []*AuthStrategyConfig{{Database: &DatabaseStrategyConfig{Connector: &ConnectorConfig{Redis: &RedisConnectorConfig{URI: "redis://a", Addr: "b"}}}}}}}, "failed to set defaults for auth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == "" {
				err := tc.p.SetDefaults(&DefaultOptions{Endpoints: []string{"://bad"}})
				assert.ErrorContains(t, err, "failed to set defaults for upstream")
				return
			}
			assert.ErrorContains(t, tc.p.SetDefaults(nil), tc.want)
		})
	}
}

func TestCovDefaults_UpstreamSetDefaults_IdsAndTypes(t *testing.T) {
	u := &UpstreamConfig{Endpoint: "https://Rpc.CovPrec.example:8545/path"}
	require.NoError(t, u.SetDefaults(nil))
	assert.Contains(t, u.Id, "Rpc.CovPrec.example-", "native endpoints derive the id from the host, port dropped")
	assert.NotContains(t, u.Id, "8545")
	assert.Equal(t, UpstreamTypeEvm, u.Type)
	require.NotNil(t, u.Evm, "evm type gets an evm block")
	assert.Equal(t, covDefaultsDur(30*time.Second), u.Evm.StatePollerInterval)
	assert.Equal(t, EvmNodeTypeUnknown, u.Evm.NodeType)
	assert.False(t, *u.Evm.SkipWhenSyncing)
	assert.False(t, *u.Evm.SkipSyncingCheck)
	assert.Nil(t, u.Evm.BlockAvailability)
	assert.NotNil(t, u.JsonRpc)
	assert.Nil(t, u.RateLimitAutoTune, "no budget, no auto-tune")
	assert.Nil(t, u.IgnoreMethods)

	vendor := &UpstreamConfig{VendorName: "acme", Endpoint: "https://x"}
	require.NoError(t, vendor.SetDefaults(nil))
	assert.Contains(t, vendor.Id, "acme-")

	scheme := &UpstreamConfig{Endpoint: "custom://x"}
	require.NoError(t, scheme.SetDefaults(nil))
	assert.Contains(t, scheme.Id, "custom-", "non-native endpoints derive the id from the scheme")

	svm := &UpstreamConfig{Id: "s", Type: UpstreamTypeSvm, AllowMethods: []string{"getSlot"}}
	require.NoError(t, svm.SetDefaults(nil))
	assert.Nil(t, svm.Evm, "non-evm types get no evm block")
	assert.Equal(t, []string{"*"}, svm.IgnoreMethods, "allowMethods implies ignoring everything else")

	both := &UpstreamConfig{Id: "b", AllowMethods: []string{"a"}, IgnoreMethods: []string{"b"}}
	require.NoError(t, both.SetDefaults(nil))
	assert.Equal(t, []string{"b"}, both.IgnoreMethods)
}

func TestCovDefaults_UpstreamSetDefaults_FailsafeMatching(t *testing.T) {
	defaults := &UpstreamConfig{
		Failsafe: []*FailsafeConfig{
			{MatchMethod: "eth_get*", Timeout: &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Base: covDefaultsDur(2 * time.Second)}}},
			{MatchMethod: "*", Retry: &RetryPolicyConfig{MaxAttempts: 9}},
		},
	}
	u := &UpstreamConfig{
		Id: "u",
		Failsafe: []*FailsafeConfig{
			{MatchMethod: "eth_getLogs", Timeout: &TimeoutPolicyConfig{}},
			{MatchMethod: "debug_trace", Retry: &RetryPolicyConfig{}},
		},
	}
	require.NoError(t, u.SetDefaults(defaults))
	require.NotNil(t, u.Failsafe[0].Timeout.Duration)
	assert.Equal(t, covDefaultsDur(2*time.Second), u.Failsafe[0].Timeout.Duration.Base, "matched default fills omitted timeout")
	assert.Equal(t, 9, u.Failsafe[1].Retry.MaxAttempts, "wildcard default matches any method")

	// Upstream with no failsafe of its own copies every default.
	empty := &UpstreamConfig{Id: "e"}
	require.NoError(t, empty.SetDefaults(defaults))
	require.Len(t, empty.Failsafe, 2)
	assert.Equal(t, "eth_get*", empty.Failsafe[0].MatchMethod)
	assert.NotSame(t, defaults.Failsafe[0], empty.Failsafe[0], "defaults are copied, not shared")
	assert.Equal(t, 9, empty.Failsafe[1].Retry.MaxAttempts)
}

func TestCovDefaults_UpstreamApplyDefaults_ExplicitWins(t *testing.T) {
	ba := int64(500)
	defaults := &UpstreamConfig{
		Endpoint:                     "https://default.example",
		Type:                         UpstreamTypeEvm,
		VendorName:                   "v",
		Tags:                         []string{"pool:x"},
		RateLimitBudget:              "d",
		RateLimitAutoTune:            &RateLimitAutoTuneConfig{MaxBudget: 5},
		AllowMethods:                 []string{"eth_call"},
		IgnoreMethods:                []string{"debug_*"},
		AutoIgnoreUnsupportedMethods: util.BoolPtr(true),
		JsonRpc:                      &JsonRpcUpstreamConfig{BatchMaxSize: 50, SupportsBatch: util.BoolPtr(true)},
		Grpc:                         &GrpcUpstreamConfig{Headers: map[string]string{"h": "v"}},
		Routing:                      &UpstreamRoutingConfig{ScoreMultipliers: []*ScoreMultiplierConfig{{Method: "*"}}},
		Svm:                          &SvmUpstreamConfig{Chain: "fogo", Cluster: "mainnet", CheckGenesisHash: true},
		Evm: &EvmUpstreamConfig{
			ChainId:                                1,
			NodeType:                               EvmNodeTypeArchive,
			StatePollerInterval:                    covDefaultsDur(5 * time.Second),
			StatePollerDebounce:                    covDefaultsDur(time.Second),
			MaxAvailableRecentBlocks:               ba,
			GetLogsAutoSplittingRangeThreshold:     1000,
			TraceFilterAutoSplittingRangeThreshold: 200,
			DeprecatedIntegrity:                    &UpstreamIntegrityConfig{},
		},
	}

	t.Run("empty upstream inherits everything", func(t *testing.T) {
		u := &UpstreamConfig{}
		require.NoError(t, u.ApplyDefaults(defaults))
		assert.Equal(t, "https://default.example", u.Endpoint)
		assert.Equal(t, "v", u.VendorName)
		assert.Equal(t, []string{"pool:x"}, u.Tags)
		assert.Equal(t, "d", u.RateLimitBudget)
		assert.Same(t, defaults.RateLimitAutoTune, u.RateLimitAutoTune)
		assert.Equal(t, []string{"eth_call"}, u.AllowMethods)
		assert.Equal(t, []string{"debug_*"}, u.IgnoreMethods)
		assert.True(t, *u.AutoIgnoreUnsupportedMethods)
		assert.Equal(t, 50, u.JsonRpc.BatchMaxSize)
		assert.Equal(t, "v", u.Grpc.Headers["h"])
		u.Grpc.Headers["h"] = "mutated"
		assert.Equal(t, "v", defaults.Grpc.Headers["h"], "grpc block is deep-copied")
		require.NotNil(t, u.Routing)
		assert.NotSame(t, defaults.Routing, u.Routing)
		assert.Len(t, u.Routing.ScoreMultipliers, 1)
		assert.Equal(t, "fogo", u.Svm.Chain)
		assert.NotSame(t, defaults.Svm, u.Svm)
		assert.Equal(t, int64(1), u.Evm.ChainId)
		assert.Equal(t, EvmNodeTypeArchive, u.Evm.NodeType)
		assert.Equal(t, int64(500), u.Evm.MaxAvailableRecentBlocks)
		require.NotNil(t, u.Evm.BlockAvailability, "legacy window maps onto blockAvailability")
		assert.Equal(t, int64(500), *u.Evm.BlockAvailability.Lower.LatestBlockMinus)
		assert.NotNil(t, u.Evm.DeprecatedIntegrity)
	})

	t.Run("partial upstream keeps its own values", func(t *testing.T) {
		u := &UpstreamConfig{
			Endpoint:        "https://mine",
			Tags:            []string{"own"},
			RateLimitBudget: "mine",
			AllowMethods:    []string{},
			JsonRpc:         &JsonRpcUpstreamConfig{BatchMaxSize: 1},
			Svm:             &SvmUpstreamConfig{},
			Evm:             &EvmUpstreamConfig{ChainId: 10},
		}
		require.NoError(t, u.ApplyDefaults(defaults))
		assert.Equal(t, "https://mine", u.Endpoint)
		assert.Equal(t, []string{"own"}, u.Tags)
		assert.Equal(t, "mine", u.RateLimitBudget)
		assert.Equal(t, []string{}, u.AllowMethods)
		assert.Equal(t, 1, u.JsonRpc.BatchMaxSize)
		assert.Equal(t, "fogo", u.Svm.Chain, "empty svm fields fill from defaults")
		assert.Equal(t, "mainnet", u.Svm.Cluster)
		assert.True(t, u.Svm.CheckGenesisHash)
		assert.Equal(t, int64(10), u.Evm.ChainId)
		assert.Equal(t, covDefaultsDur(5*time.Second), u.Evm.StatePollerInterval)
		assert.Equal(t, covDefaultsDur(time.Second), u.Evm.StatePollerDebounce)
		assert.Equal(t, int64(500), u.Evm.MaxAvailableRecentBlocks)
		assert.Equal(t, int64(1000), u.Evm.GetLogsAutoSplittingRangeThreshold)
		assert.Equal(t, int64(200), u.Evm.TraceFilterAutoSplittingRangeThreshold)
		assert.NotNil(t, u.Evm.DeprecatedIntegrity)
	})

	t.Run("explicit blockAvailability in defaults suppresses legacy window", func(t *testing.T) {
		lbm := int64(7)
		d := &UpstreamConfig{Evm: &EvmUpstreamConfig{MaxAvailableRecentBlocks: 99, BlockAvailability: &EvmBlockAvailabilityConfig{Lower: &EvmAvailabilityBoundConfig{LatestBlockMinus: &lbm}}}}
		u := &UpstreamConfig{}
		require.NoError(t, u.ApplyDefaults(d))
		assert.Zero(t, u.Evm.MaxAvailableRecentBlocks)
		assert.Equal(t, int64(7), *u.Evm.BlockAvailability.Lower.LatestBlockMinus)
		assert.NotSame(t, d.Evm.BlockAvailability, u.Evm.BlockAvailability)

		partial := &UpstreamConfig{Evm: &EvmUpstreamConfig{}}
		require.NoError(t, partial.ApplyDefaults(d))
		assert.Equal(t, int64(7), *partial.Evm.BlockAvailability.Lower.LatestBlockMinus)
		assert.Zero(t, partial.Evm.MaxAvailableRecentBlocks)
	})

	t.Run("nil defaults is a no-op", func(t *testing.T) {
		u := &UpstreamConfig{Endpoint: "x"}
		require.NoError(t, u.ApplyDefaults(nil))
		assert.Equal(t, &UpstreamConfig{Endpoint: "x"}, u)
	})
}

func TestCovDefaults_EvmUpstreamSetDefaults(t *testing.T) {
	full := &EvmUpstreamConfig{NodeType: EvmNodeTypeFull}
	require.NoError(t, full.SetDefaults(nil))
	assert.Equal(t, int64(128), full.MaxAvailableRecentBlocks, "full nodes default to a 128-block window")
	assert.Equal(t, int64(128), *full.BlockAvailability.Lower.LatestBlockMinus)

	lbm := int64(3)
	defaults := &EvmUpstreamConfig{
		StatePollerDebounce: covDefaultsDur(2 * time.Second),
		NodeType:            EvmNodeTypeFull,
		BlockAvailability:   &EvmBlockAvailabilityConfig{Lower: &EvmAvailabilityBoundConfig{LatestBlockMinus: &lbm}},
		SkipWhenSyncing:     util.BoolPtr(true),
		SkipSyncingCheck:    util.BoolPtr(true),
		PinLatestTag:        util.BoolPtr(true),
		Multicall3:          &EvmMulticall3Config{MaxCalls: 5},
	}
	e := &EvmUpstreamConfig{}
	require.NoError(t, e.SetDefaults(defaults))
	assert.Equal(t, covDefaultsDur(2*time.Second), e.StatePollerDebounce)
	assert.Equal(t, EvmNodeTypeFull, e.NodeType)
	assert.Equal(t, int64(3), *e.BlockAvailability.Lower.LatestBlockMinus, "inherited window beats the full-node 128")
	assert.Zero(t, e.MaxAvailableRecentBlocks)
	assert.True(t, *e.SkipWhenSyncing)
	assert.True(t, *e.SkipSyncingCheck)
	assert.True(t, *e.PinLatestTag)
	assert.Equal(t, 5, e.Multicall3.MaxCalls)
	assert.Equal(t, covDefaultsDur(10*time.Millisecond), e.Multicall3.MaxWait)
	assert.NotSame(t, defaults.Multicall3, e.Multicall3)

	legacy := &EvmUpstreamConfig{}
	require.NoError(t, legacy.SetDefaults(&EvmUpstreamConfig{MaxAvailableRecentBlocks: 64}))
	assert.Equal(t, int64(64), legacy.MaxAvailableRecentBlocks)
	assert.Equal(t, int64(64), *legacy.BlockAvailability.Lower.LatestBlockMinus)

	own := &EvmUpstreamConfig{Multicall3: &EvmMulticall3Config{MaxWait: 1, MaxCalls: 2}}
	require.NoError(t, own.SetDefaults(nil))
	assert.Equal(t, Duration(1), own.Multicall3.MaxWait)
	assert.Equal(t, 2, own.Multicall3.MaxCalls)
}

func TestCovDefaults_NetworkSetDefaults_NetworkDefaultsPrecedence(t *testing.T) {
	dbt, fdm, bud := 2.0, 3.0, 4.0
	defaults := &NetworkDefaults{
		RateLimitBudget:   "nd",
		CacheKeySuffix:    "sfx",
		SelectionPolicy:   &SelectionPolicyConfig{EvalInterval: covDefaultsDur(time.Minute)},
		DirectiveDefaults: &DirectiveDefaultsConfig{EnforceHighestBlock: util.BoolPtr(false)},
		Multiplexing:      util.BoolPtr(false),
		Failover:          &FailoverConfig{OnDefaultsExhausted: util.BoolPtr(true)},
		Evm: &EvmNetworkConfig{
			Integrity:                          &EvmIntegrityConfig{EnforceGetLogsBlockRange: util.BoolPtr(false)},
			FallbackStatePollerDebounce:        covDefaultsDur(3 * time.Second),
			DynamicBlockTimeDebounceMultiplier: &dbt,
			FinalizedDebounceDepthMultiplier:   &fdm,
			BlockUnavailableDelayMultiplier:    &bud,
			FallbackFinalityDepth:              77,
			GetLogsMaxAllowedAddresses:         11,
			GetLogsMaxAllowedTopics:            12,
			GetLogsSplitOnError:                util.BoolPtr(false),
			GetLogsSplitConcurrency:            3,
			GetLogsMaxResponseBytes:            1024,
			TraceFilterSplitOnError:            util.BoolPtr(true),
			TraceFilterSplitConcurrency:        4,
			ServedTip:                          &EvmServedTipConfig{Quorum: 3},
			EmptyResultConfidence:              AvailbilityConfidenceFinalized,
			ShortCircuitFutureBlocks:           util.BoolPtr(true),
			StripSubscribeFromBlockZero:        util.BoolPtr(true),
			LocalAnswers:                       map[string]bool{"eth_chainId": true, "net_version": false},
		},
	}

	n := &NetworkConfig{Evm: &EvmNetworkConfig{ChainId: 1, LocalAnswers: map[string]bool{"eth_chainId": false}}}
	require.NoError(t, n.SetDefaults(nil, defaults))
	assert.Equal(t, NetworkArchitecture("evm"), n.Architecture)
	assert.Equal(t, "nd", n.RateLimitBudget)
	assert.Equal(t, "sfx", n.CacheKeySuffix)
	assert.Equal(t, covDefaultsDur(time.Minute), n.SelectionPolicy.EvalInterval)
	assert.NotSame(t, defaults.SelectionPolicy, n.SelectionPolicy)
	assert.False(t, *n.DirectiveDefaults.EnforceHighestBlock, "networkDefaults directive wins over the built-in true")
	assert.False(t, *n.DirectiveDefaults.EnforceGetLogsBlockRange, "deprecated evm.integrity migrates into directives")
	assert.True(t, *n.DirectiveDefaults.EnforceNonNullTaggedBlocks)
	assert.False(t, n.MultiplexingEnabled())
	assert.True(t, n.Failover.Enabled())
	*defaults.Failover.OnDefaultsExhausted = false
	assert.True(t, n.Failover.Enabled(), "failover is cloned, not aliased")
	*defaults.Failover.OnDefaultsExhausted = true

	e := n.Evm
	assert.Equal(t, covDefaultsDur(3*time.Second), e.FallbackStatePollerDebounce)
	assert.Equal(t, 2.0, *e.DynamicBlockTimeDebounceMultiplier)
	assert.Equal(t, 3.0, *e.FinalizedDebounceDepthMultiplier)
	assert.Equal(t, 4.0, *e.BlockUnavailableDelayMultiplier)
	assert.Equal(t, int64(77), e.FallbackFinalityDepth)
	assert.Equal(t, int64(11), e.GetLogsMaxAllowedAddresses)
	assert.Equal(t, int64(12), e.GetLogsMaxAllowedTopics)
	assert.False(t, *e.GetLogsSplitOnError)
	assert.Equal(t, 3, e.GetLogsSplitConcurrency)
	assert.Equal(t, int64(1024), e.GetLogsMaxResponseBytes)
	assert.True(t, *e.TraceFilterSplitOnError)
	assert.Equal(t, 4, e.TraceFilterSplitConcurrency)
	assert.Equal(t, ServedTipQuorum(3), e.ServedTipQuorum())
	assert.NotSame(t, defaults.Evm.ServedTip, e.ServedTip)
	assert.Equal(t, AvailbilityConfidenceFinalized, e.EmptyResultConfidence)
	assert.True(t, e.ShortCircuitFutureBlocksEnabled())
	assert.True(t, *e.StripSubscribeFromBlockZero)
	assert.Equal(t, map[string]bool{"eth_chainId": false, "net_version": false}, e.LocalAnswers, "network's own localAnswers entry wins")
	assert.Equal(t, int64(30_000), e.GetLogsMaxAllowedRange, "fields absent from both fall to built-ins")

	// Explicit network values are never overwritten.
	own := &NetworkConfig{
		RateLimitBudget: "own",
		CacheKeySuffix:  "mine",
		Multiplexing:    util.BoolPtr(true),
		Evm:             &EvmNetworkConfig{ChainId: 2, GetLogsSplitConcurrency: 99, FallbackFinalityDepth: 5},
	}
	require.NoError(t, own.SetDefaults(nil, defaults))
	assert.Equal(t, "own", own.RateLimitBudget)
	assert.Equal(t, "mine", own.CacheKeySuffix)
	assert.True(t, own.MultiplexingEnabled())
	assert.Equal(t, 99, own.Evm.GetLogsSplitConcurrency)
	assert.Equal(t, int64(5), own.Evm.FallbackFinalityDepth)
	assert.Equal(t, map[string]bool{"eth_chainId": true, "net_version": false}, own.Evm.LocalAnswers)
}

func TestCovDefaults_NetworkSetDefaults_ArchitectureAndSvm(t *testing.T) {
	lag := int64(0)
	defaults := &NetworkDefaults{
		Evm: &EvmNetworkConfig{GetLogsMaxAllowedRange: 5},
		Svm: &SvmNetworkConfig{Chain: "fogo", Commitment: "confirmed", StatePollerDebounce: covDefaultsDur(time.Second), MaxFinalizedSlotLag: &lag, EnforceBlockAvailability: util.BoolPtr(false), Cluster: "ignored"},
	}

	evm := &NetworkConfig{}
	require.NoError(t, evm.SetDefaults(nil, defaults))
	assert.Equal(t, NetworkArchitecture("evm"), evm.Architecture, "an empty network adopts the evm defaults block")
	assert.Equal(t, int64(5), evm.Evm.GetLogsMaxAllowedRange)

	svm := &NetworkConfig{Architecture: ArchitectureSvm}
	require.NoError(t, svm.SetDefaults(nil, defaults))
	assert.Nil(t, svm.Evm, "svm networks never get the evm defaults block")
	require.NotNil(t, svm.Svm)
	assert.Equal(t, covDefaultsDur(400*time.Millisecond), svm.Svm.StatePollerDebounce, "an svm block created after the merge only gets built-ins")

	svm2 := &NetworkConfig{Svm: &SvmNetworkConfig{Cluster: "devnet"}}
	require.NoError(t, svm2.SetDefaults(nil, defaults))
	assert.Equal(t, ArchitectureSvm, svm2.Architecture)
	assert.Equal(t, "devnet", svm2.Svm.Cluster, "cluster is identity and never inherited")
	assert.Equal(t, "fogo", svm2.Svm.Chain)
	assert.Equal(t, "confirmed", svm2.Svm.Commitment)
	assert.Equal(t, covDefaultsDur(time.Second), svm2.Svm.StatePollerDebounce)
	assert.Equal(t, int64(0), *svm2.Svm.MaxFinalizedSlotLag, "explicit zero lag in defaults is kept")
	assert.NotSame(t, defaults.Svm.MaxFinalizedSlotLag, svm2.Svm.MaxFinalizedSlotLag)
	assert.False(t, *svm2.Svm.EnforceBlockAvailability)

	bare := &NetworkConfig{Architecture: ArchitectureSvm}
	require.NoError(t, bare.SetDefaults(nil, nil))
	assert.Equal(t, MaxShredInsertSlotLagThreshold, *bare.Svm.MaxFinalizedSlotLag)

	bareEvm := &NetworkConfig{Architecture: "evm"}
	require.NoError(t, bareEvm.SetDefaults(nil, nil))
	require.NotNil(t, bareEvm.Evm)
	assert.Equal(t, int64(DefaultEvmFinalityDepth), bareEvm.Evm.FallbackFinalityDepth)
	assert.True(t, *bareEvm.Evm.Integrity.EnforceHighestBlock)
	assert.NotNil(t, bareEvm.Methods)
}

func TestCovDefaults_NetworkSetDefaults_FailsafeMergeAndErrors(t *testing.T) {
	defaults := &NetworkDefaults{Failsafe: []*FailsafeConfig{
		{MatchMethod: "eth_*", MatchCommitment: []string{"confirmed", "finalized"}, Retry: &RetryPolicyConfig{MaxAttempts: 4}},
	}}
	n := &NetworkConfig{Failsafe: []*FailsafeConfig{
		{MatchMethod: "eth_call", MatchCommitment: []string{"finalized"}, Retry: &RetryPolicyConfig{}},
		{MatchMethod: "eth_call", MatchCommitment: []string{"processed"}, Retry: &RetryPolicyConfig{}},
	}}
	require.NoError(t, n.SetDefaults(nil, defaults))
	assert.Equal(t, 4, n.Failsafe[0].Retry.MaxAttempts, "default covering the rule's commitments applies")
	assert.Equal(t, 3, n.Failsafe[1].Retry.MaxAttempts, "uncovered commitment falls back to built-ins")

	noDefaults := &NetworkConfig{Failsafe: []*FailsafeConfig{{Retry: &RetryPolicyConfig{}}}}
	require.NoError(t, noDefaults.SetDefaults(nil, &NetworkDefaults{}))
	assert.Equal(t, 3, noDefaults.Failsafe[0].Retry.MaxAttempts)
	nd2 := &NetworkDefaults{SelectionPolicy: &SelectionPolicyConfig{EvalScope: "x"}}
	assert.ErrorContains(t, nd2.SetDefaults(), "failed to set defaults for selection policy")
	badNet := &NetworkConfig{SelectionPolicy: &SelectionPolicyConfig{EvalScope: "x"}}
	assert.ErrorContains(t, badNet.SetDefaults(nil, nil), "failed to set defaults for selection policy")
}

func TestCovDefaults_NetworkDefaultsSetDefaults_FillsNested(t *testing.T) {
	nd := &NetworkDefaults{
		Failsafe:          []*FailsafeConfig{{Hedge: &HedgePolicyConfig{}}},
		SelectionPolicy:   &SelectionPolicyConfig{},
		Evm:               &EvmNetworkConfig{},
		Svm:               &SvmNetworkConfig{},
		DirectiveDefaults: &DirectiveDefaultsConfig{EnforceHighestBlock: util.BoolPtr(false)},
	}
	require.NoError(t, nd.SetDefaults())
	assert.Equal(t, "*", nd.Failsafe[0].MatchMethod)
	assert.Equal(t, 1, nd.Failsafe[0].Hedge.MaxCount)
	assert.Equal(t, DefaultSelectionPolicySource, nd.SelectionPolicy.EvalFunc)
	assert.NotNil(t, nd.SelectionPolicy.CompiledProgram)
	assert.Equal(t, int64(30_000), nd.Evm.GetLogsMaxAllowedRange)
	assert.Equal(t, covDefaultsDur(400*time.Millisecond), nd.Svm.StatePollerDebounce)
	assert.False(t, *nd.DirectiveDefaults.EnforceHighestBlock)
	assert.True(t, *nd.DirectiveDefaults.EnforceGetLogsBlockRange)

	var nilDD *DirectiveDefaultsConfig
	assert.NoError(t, nilDD.SetDefaults())
	var nilSvm *SvmNetworkConfig
	assert.NoError(t, nilSvm.SetDefaults())
}

func TestCovDefaults_FailsafeSetDefaults_PolicyPrecedence(t *testing.T) {
	defaults := &FailsafeConfig{
		MatchMethod:    "eth_*",
		Timeout:        &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Base: covDefaultsDur(time.Second)}},
		Retry:          &RetryPolicyConfig{MaxAttempts: 5, BackoffFactor: 2, BackoffMaxDelay: 7, Delay: 8, Jitter: 9, EmptyResultMaxAttempts: 4, EmptyResultDelay: 6, EmptyResultIgnore: []string{"eth_getLogs"}},
		Hedge:          &HedgePolicyConfig{MaxCount: 3, Delay: &AdaptiveDuration{Base: covDefaultsDur(50 * time.Millisecond)}},
		CircuitBreaker: &CircuitBreakerPolicyConfig{FailureThresholdCount: 1, FailureThresholdCapacity: 2, SuccessThresholdCount: 3, SuccessThresholdCapacity: 4, HalfOpenAfter: 5},
	}

	t.Run("absent policies are created from defaults", func(t *testing.T) {
		f := &FailsafeConfig{}
		require.NoError(t, f.SetDefaults(defaults))
		assert.Equal(t, "eth_*", f.MatchMethod)
		assert.Equal(t, covDefaultsDur(time.Second), f.Timeout.Duration.Base)
		assert.NotSame(t, defaults.Timeout.Duration, f.Timeout.Duration)
		assert.Equal(t, RetryPolicyConfig{MaxAttempts: 5, BackoffFactor: 2, BackoffMaxDelay: 7, Delay: 8, Jitter: 9, EmptyResultMaxAttempts: 4, EmptyResultDelay: 6, EmptyResultAccept: []string{"eth_getLogs"}}, *f.Retry)
		assert.Equal(t, 3, f.Hedge.MaxCount)
		assert.Equal(t, covDefaultsDur(50*time.Millisecond), f.Hedge.Delay.Base)
		assert.Equal(t, covDefaultsDur(100*time.Millisecond), f.Hedge.Delay.Min)
		assert.Equal(t, CircuitBreakerPolicyConfig{FailureThresholdCount: 1, FailureThresholdCapacity: 2, SuccessThresholdCount: 3, SuccessThresholdCapacity: 4, HalfOpenAfter: 5}, *f.CircuitBreaker)
	})

	t.Run("present policies keep explicit fields", func(t *testing.T) {
		f := &FailsafeConfig{
			MatchMethod:    "eth_call",
			Timeout:        &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Base: covDefaultsDur(3 * time.Second)}},
			Retry:          &RetryPolicyConfig{MaxAttempts: 1, EmptyResultAccept: []string{"x"}},
			Hedge:          &HedgePolicyConfig{MaxCount: 9},
			CircuitBreaker: &CircuitBreakerPolicyConfig{FailureThresholdCount: 50},
		}
		require.NoError(t, f.SetDefaults(defaults))
		assert.Equal(t, "eth_call", f.MatchMethod)
		assert.Equal(t, covDefaultsDur(3*time.Second), f.Timeout.Duration.Base)
		assert.Equal(t, 1, f.Retry.MaxAttempts)
		assert.Equal(t, float32(2), f.Retry.BackoffFactor)
		assert.Equal(t, []string{"x"}, f.Retry.EmptyResultAccept)
		assert.Equal(t, 9, f.Hedge.MaxCount)
		assert.Equal(t, uint(50), f.CircuitBreaker.FailureThresholdCount)
		assert.Equal(t, uint(2), f.CircuitBreaker.FailureThresholdCapacity)
	})

	t.Run("built-ins without defaults", func(t *testing.T) {
		f := &FailsafeConfig{Timeout: &TimeoutPolicyConfig{}, Hedge: &HedgePolicyConfig{}, CircuitBreaker: &CircuitBreakerPolicyConfig{}, Consensus: &ConsensusPolicyConfig{}}
		require.NoError(t, f.SetDefaults(nil))
		assert.Equal(t, "*", f.MatchMethod)
		assert.Nil(t, f.Timeout.Duration, "timeout has no built-in duration")
		assert.Equal(t, 1, f.Hedge.MaxCount)
		assert.Equal(t, covDefaultsDur(999*time.Second), f.Hedge.Delay.Max)
		assert.Equal(t, CircuitBreakerPolicyConfig{FailureThresholdCount: 20, FailureThresholdCapacity: 80, SuccessThresholdCount: 8, SuccessThresholdCapacity: 200, HalfOpenAfter: covDefaultsDur(5 * time.Minute)}, *f.CircuitBreaker)
		assert.Equal(t, 5, f.Consensus.MaxParticipants)
	})

	t.Run("retry ignore alias and deprecated delay", func(t *testing.T) {
		r := &RetryPolicyConfig{EmptyResultIgnore: []string{"m"}}
		require.NoError(t, r.SetDefaults(nil))
		assert.Equal(t, []string{"m"}, r.EmptyResultAccept)
		r2 := &RetryPolicyConfig{}
		require.NoError(t, r2.SetDefaults(&RetryPolicyConfig{EmptyResultAccept: []string{"a"}}))
		assert.Equal(t, []string{"a"}, r2.EmptyResultAccept)
	})
}

func TestCovDefaults_SelectionPolicyAndEvmNetworkDefaults(t *testing.T) {
	s := &SelectionPolicyConfig{EvalFunc: "this is ( not js"}
	assert.ErrorContains(t, s.SetDefaults(), "failed to compile selectionPolicy.evalFunc")

	ts := &SelectionPolicyConfig{EvalFunc: tsFunctionSentinelPrefix + "7"}
	require.NoError(t, ts.SetDefaults())
	assert.Nil(t, ts.CompiledProgram, "TS sentinels resolve at runtime, never compiled")
	assert.Equal(t, tsFunctionSentinelPrefix+"7", ts.EvalFuncOriginal)
	assert.True(t, IsTSFunctionSentinel(ts.EvalFunc))
	assert.Equal(t, "7", TSFunctionSentinelID(ts.EvalFunc))

	dist := int64(5)
	e := &EvmNetworkConfig{MaxFutureBlockRetryDistance: &dist, MarkEmptyAsErrorMethods: []string{}}
	require.NoError(t, e.SetDefaults())
	assert.Nil(t, e.MaxFutureBlockRetryDistance, "deprecated field is cleared")
	assert.Equal(t, []string{}, e.MarkEmptyAsErrorMethods, "explicit empty list is kept")
	assert.Equal(t, AvailbilityConfidenceBlockHead, e.EmptyResultConfidence)
	assert.Equal(t, DefaultEvmStatePollerDebounce, e.FallbackStatePollerDebounce)
	assert.Equal(t, 10, e.TraceFilterSplitConcurrency)
	assert.Nil(t, e.TraceFilterSplitOnError)
}
