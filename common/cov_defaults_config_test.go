package common

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"gopkg.in/yaml.v3"
)

func covDefaultsWriteCfg(t *testing.T, fs afero.Fs, name, body string) string {
	t.Helper()
	require.NoError(t, afero.WriteFile(fs, name, []byte(body), 0o644))
	return name
}

func covDefaultsSetLegacyHooks(t *testing.T, fn func(*Config) ([]string, error), logger func(string)) {
	t.Helper()
	prevFn, prevLog := LegacyTranslateFn, LegacyTranslateLogger
	LegacyTranslateFn, LegacyTranslateLogger = fn, logger
	t.Cleanup(func() { LegacyTranslateFn, LegacyTranslateLogger = prevFn, prevLog })
}

func TestCovDefaults_LoadConfig_LegacyHookAndEnvExpansion(t *testing.T) {
	t.Setenv("COVDEFAULTS_ENDPOINT", "https://env.example")
	fs := afero.NewMemMapFs()
	path := covDefaultsWriteCfg(t, fs, "erpc.yaml", `
logLevel: warn
projects:
  - id: main
    upstreams:
      - id: u1
        endpoint: ${COVDEFAULTS_ENDPOINT}
        evm:
          chainId: 1
`)
	var warnings []string
	covDefaultsSetLegacyHooks(t, func(c *Config) ([]string, error) {
		c.Projects[0].Upstreams[0].RateLimitBudget = ""
		return []string{"w1", "w2"}, nil
	}, func(w string) { warnings = append(warnings, w) })

	cfg, err := LoadConfig(fs, path, &DefaultOptions{})
	require.NoError(t, err)
	assert.Equal(t, "https://env.example", cfg.Projects[0].Upstreams[0].Endpoint)
	assert.Equal(t, []string{"w1", "w2"}, warnings, "every translator warning reaches the logger")
	assert.Same(t, cfg.Projects[0], cfg.GetProjectConfig("main"))
	assert.Nil(t, cfg.GetProjectConfig("other"))
}

func TestCovDefaults_LoadConfig_FailurePaths(t *testing.T) {
	fs := afero.NewMemMapFs()
	ok := covDefaultsWriteCfg(t, fs, "ok.yaml", "logLevel: warn\nprojects:\n  - id: main\n    upstreams:\n      - id: u\n        endpoint: https://a\n        evm:\n          chainId: 1\n")

	t.Run("translator error", func(t *testing.T) {
		covDefaultsSetLegacyHooks(t, func(*Config) ([]string, error) { return nil, errors.New("boom") }, nil)
		_, err := LoadConfig(fs, ok, &DefaultOptions{})
		assert.EqualError(t, err, "legacy config migration: boom")
	})
	t.Run("set defaults error", func(t *testing.T) {
		covDefaultsSetLegacyHooks(t, nil, nil)
		bad := covDefaultsWriteCfg(t, fs, "bad-defaults.yaml", "logLevel: warn\nprojects:\n  - id: main\n    upstreams:\n      - id: u\n        endpoint: nosuchvendor://k\n")
		_, err := LoadConfig(fs, bad, &DefaultOptions{})
		assert.ErrorContains(t, err, "unsupported vendor name in vendor.settings: nosuchvendor")
	})
	t.Run("validate error", func(t *testing.T) {
		covDefaultsSetLegacyHooks(t, nil, nil)
		bad := covDefaultsWriteCfg(t, fs, "bad-validate.yaml", "logLevel: warn\nprojects:\n  - id: main\n    upstreams:\n      - id: u\n        endpoint: https://a\n        evm:\n          chainId: 1\n      - id: u\n        endpoint: https://b\n        evm:\n          chainId: 1\n")
		_, err := LoadConfig(fs, bad, &DefaultOptions{})
		require.Error(t, err)
	})
	t.Run("ts file that does not exist on disk", func(t *testing.T) {
		covDefaultsSetLegacyHooks(t, nil, nil)
		p := covDefaultsWriteCfg(t, fs, "/covdefaults-nowhere/erpc.ts", "export default {}")
		_, err := LoadConfig(fs, p, &DefaultOptions{})
		require.Error(t, err, "the TS compiler reads the real filesystem, not the afero one")
	})
}

func TestCovDefaults_LoadConfig_TypeScriptErrors(t *testing.T) {
	covDefaultsSetLegacyHooks(t, nil, nil)
	dir := t.TempDir()
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"no default export", "export const x = 1;\n", "config object must be default exported"},
		{"unknown field", "export default { logLevel: 'warn', notAField: 1 };\n", "ts config decode"},
		{"runtime throw", "throw new Error('kaboom');\nexport default {};\n", "kaboom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".ts")
			require.NoError(t, os.WriteFile(p, []byte(tc.src), 0o644))
			_, err := LoadConfig(afero.NewOsFs(), p, &DefaultOptions{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestCovDefaults_LegacyYAMLShapes(t *testing.T) {
	t.Run("network single failsafe object", func(t *testing.T) {
		var n NetworkConfig
		require.NoError(t, yaml.Unmarshal([]byte(`
architecture: evm
rateLimitBudget: rb
alias: eth
cacheKeySuffix: s
evm:
  chainId: 1
failover:
  onDefaultsExhausted: true
failsafe:
  retry:
    maxAttempts: 4
`), &n))
		assert.Equal(t, NetworkArchitecture("evm"), n.Architecture)
		assert.Equal(t, "rb", n.RateLimitBudget)
		assert.Equal(t, "eth", n.Alias)
		assert.Equal(t, "s", n.CacheKeySuffix)
		assert.True(t, n.Failover.Enabled())
		require.Len(t, n.Failsafe, 1)
		assert.Equal(t, "*", n.Failsafe[0].MatchMethod)
		assert.Equal(t, 4, n.Failsafe[0].Retry.MaxAttempts)
		assert.Equal(t, "evm:1", n.NetworkId())
	})
	t.Run("network single failsafe with explicit method", func(t *testing.T) {
		var n NetworkConfig
		require.NoError(t, yaml.Unmarshal([]byte("architecture: evm\nfailsafe:\n  matchMethod: eth_call\n"), &n))
		assert.Equal(t, "eth_call", n.Failsafe[0].MatchMethod)
	})
	t.Run("network unknown field surfaces as-is", func(t *testing.T) {
		var n NetworkConfig
		dec := yaml.NewDecoder(strings.NewReader("architecture: evm\nbogusField: 1\n"))
		dec.KnownFields(true)
		err := dec.Decode(&n)
		assert.ErrorContains(t, err, "not found in type")
	})
	t.Run("network type error in both shapes keeps original", func(t *testing.T) {
		var n NetworkConfig
		err := yaml.Unmarshal([]byte("architecture: [1]\n"), &n)
		require.Error(t, err)
	})
	t.Run("networkDefaults single failsafe", func(t *testing.T) {
		var nd NetworkDefaults
		require.NoError(t, yaml.Unmarshal([]byte(`
rateLimitBudget: rb
cacheKeySuffix: s
evm:
  getLogsMaxAllowedRange: 9
svm:
  chain: fogo
failsafe:
  hedge:
    maxCount: 2
`), &nd))
		assert.Equal(t, "rb", nd.RateLimitBudget)
		assert.Equal(t, "s", nd.CacheKeySuffix)
		assert.Equal(t, int64(9), nd.Evm.GetLogsMaxAllowedRange)
		assert.Equal(t, "fogo", nd.Svm.Chain)
		require.Len(t, nd.Failsafe, 1)
		assert.Equal(t, "*", nd.Failsafe[0].MatchMethod)
		assert.Equal(t, 2, nd.Failsafe[0].Hedge.MaxCount)
	})
	t.Run("networkDefaults unknown field", func(t *testing.T) {
		var nd NetworkDefaults
		dec := yaml.NewDecoder(strings.NewReader("bogus: 1\n"))
		dec.KnownFields(true)
		assert.ErrorContains(t, dec.Decode(&nd), "not found in type")
	})
	t.Run("networkDefaults bad type", func(t *testing.T) {
		var nd NetworkDefaults
		assert.Error(t, yaml.Unmarshal([]byte("rateLimitBudget: [1]\n"), &nd))
	})
	t.Run("upstream single failsafe object with legacy group", func(t *testing.T) {
		var u UpstreamConfig
		require.NoError(t, yaml.Unmarshal([]byte(`
id: u1
type: evm
endpoint: https://a
group: fallback
cohort: c1
rateLimitBudget: rb
routing:
  scoreMultipliers:
    - errorRate: 3
failsafe:
  timeout:
    duration: 2s
`), &u))
		assert.Equal(t, "u1", u.Id)
		assert.Equal(t, "rb", u.RateLimitBudget)
		require.Len(t, u.Failsafe, 1)
		assert.Equal(t, "*", u.Failsafe[0].MatchMethod)
		assert.Equal(t, covDefaultsDur(2*time.Second), u.Failsafe[0].Timeout.Duration.Base)
		assert.True(t, u.HasTag("tier:fallback"))
		assert.True(t, u.HasTag("cohort:c1"))
		assert.False(t, u.HasTag("tier:default"))
		require.NotNil(t, u.Routing)
		assert.Equal(t, 3.0, *u.Routing.ScoreMultipliers[0].ErrorRate)
	})
	t.Run("upstream legacy group not duplicated", func(t *testing.T) {
		var u UpstreamConfig
		require.NoError(t, yaml.Unmarshal([]byte("id: u\ntags: [\"tier:fallback\"]\ngroup: fallback\n"), &u))
		assert.Equal(t, []string{"tier:fallback"}, u.Tags)
	})
	t.Run("upstream unknown field", func(t *testing.T) {
		var u UpstreamConfig
		dec := yaml.NewDecoder(strings.NewReader("id: u\nbogus: 1\n"))
		dec.KnownFields(true)
		assert.ErrorContains(t, dec.Decode(&u), "not found in type")
	})
	t.Run("upstream bad type in both shapes", func(t *testing.T) {
		var u UpstreamConfig
		assert.Error(t, yaml.Unmarshal([]byte("id: [1]\n"), &u))
	})
	t.Run("project legacy routing fields stashed", func(t *testing.T) {
		var p ProjectConfig
		require.NoError(t, yaml.Unmarshal([]byte("id: p\nroutingStrategy: score-based\nscoreGranularity: method\n"), &p))
		require.NotNil(t, p.LegacyProject)
		assert.Equal(t, "score-based", p.LegacyProject.RoutingStrategy)
		assert.Equal(t, "method", p.LegacyProject.ScoreGranularity)
		var plain ProjectConfig
		require.NoError(t, yaml.Unmarshal([]byte("id: p\n"), &plain))
		assert.Nil(t, plain.LegacyProject)
		assert.Error(t, yaml.Unmarshal([]byte("id: [1]\n"), &plain))
	})
	t.Run("selection policy legacy fields stashed", func(t *testing.T) {
		var s SelectionPolicyConfig
		require.NoError(t, yaml.Unmarshal([]byte("evalFunction: \"(u) => u\"\nresampleCount: 3\n"), &s))
		require.NotNil(t, s.LegacySelectionPolicy)
		assert.Equal(t, "(u) => u", s.LegacySelectionPolicy.EvalFunction)
		assert.Equal(t, 3, s.LegacySelectionPolicy.ResampleCount)
		assert.Error(t, yaml.Unmarshal([]byte("evalInterval: [1]\n"), &s))
	})
	t.Run("directive skipCacheRead normalized to string", func(t *testing.T) {
		var d DirectiveDefaultsConfig
		require.NoError(t, yaml.Unmarshal([]byte("skipCacheRead: true\n"), &d))
		assert.Equal(t, "true", d.SkipCacheRead)
		var dj DirectiveDefaultsConfig
		require.NoError(t, json.Unmarshal([]byte(`{"skipCacheRead":"cache-a*"}`), &dj))
		assert.Equal(t, "cache-a*", dj.SkipCacheRead)
		var dn DirectiveDefaultsConfig
		require.NoError(t, json.Unmarshal([]byte(`{"retryEmpty":true}`), &dn))
		assert.Nil(t, dn.SkipCacheRead)
		assert.Error(t, yaml.Unmarshal([]byte("retryEmpty: [1]\n"), &dn))
		assert.Error(t, json.Unmarshal([]byte(`{"retryEmpty":[1]}`), &dn))
	})
}

func TestCovDefaults_TimeoutAndHedgeLegacySiblings(t *testing.T) {
	t.Run("timeout yaml legacy siblings fold in", func(t *testing.T) {
		var c TimeoutPolicyConfig
		require.NoError(t, yaml.Unmarshal([]byte("quantile: 0.9\nminDuration: 1s\nmaxDuration: 5s\n"), &c))
		assert.Equal(t, &AdaptiveDuration{Quantile: 0.9, Min: covDefaultsDur(time.Second), Max: covDefaultsDur(5 * time.Second)}, c.Duration)
		var keep TimeoutPolicyConfig
		require.NoError(t, yaml.Unmarshal([]byte("duration: {base: 2s, quantile: 0.5, min: 100ms, max: 3s}\nquantile: 0.9\nminDuration: 1s\nmaxDuration: 5s\n"), &keep))
		assert.Equal(t, 0.5, keep.Duration.Quantile, "unified form wins over legacy siblings")
		assert.Equal(t, covDefaultsDur(100*time.Millisecond), keep.Duration.Min)
		assert.Error(t, yaml.Unmarshal([]byte("quantile: [1]\n"), &c))
	})
	t.Run("timeout json forms", func(t *testing.T) {
		var c TimeoutPolicyConfig
		require.NoError(t, json.Unmarshal([]byte(`{"quantile":0.95,"minDuration":"200ms","maxDuration":1500}`), &c))
		assert.Equal(t, &AdaptiveDuration{Quantile: 0.95, Min: covDefaultsDur(200 * time.Millisecond), Max: covDefaultsDur(1500 * time.Millisecond)}, c.Duration)
		var n TimeoutPolicyConfig
		require.NoError(t, n.UnmarshalJSON([]byte("null")))
		assert.Nil(t, n.Duration)
		assert.ErrorContains(t, json.Unmarshal([]byte(`{"minDuration":"bogus"}`), &c), "timeout.minDuration")
		assert.ErrorContains(t, json.Unmarshal([]byte(`{"maxDuration":"bogus"}`), &c), "timeout.maxDuration")
		assert.Error(t, json.Unmarshal([]byte(`{"quantile":"x"}`), &c))
	})
	t.Run("hedge yaml and json legacy siblings", func(t *testing.T) {
		var h HedgePolicyConfig
		require.NoError(t, yaml.Unmarshal([]byte("maxCount: 2\nquantile: 0.8\nminDelay: 50ms\nmaxDelay: 2s\n"), &h))
		assert.Equal(t, 2, h.MaxCount)
		assert.Equal(t, &AdaptiveDuration{Quantile: 0.8, Min: covDefaultsDur(50 * time.Millisecond), Max: covDefaultsDur(2 * time.Second)}, h.Delay)
		assert.Error(t, yaml.Unmarshal([]byte("maxCount: [1]\n"), &h))

		var hj HedgePolicyConfig
		require.NoError(t, json.Unmarshal([]byte(`{"delay":"100ms","maxCount":3,"quantile":0.7,"minDelay":10,"maxDelay":"1s"}`), &hj))
		assert.Equal(t, 3, hj.MaxCount)
		assert.Equal(t, &AdaptiveDuration{Base: covDefaultsDur(100 * time.Millisecond), Quantile: 0.7, Min: covDefaultsDur(10 * time.Millisecond), Max: covDefaultsDur(time.Second)}, hj.Delay)
		var hn HedgePolicyConfig
		require.NoError(t, hn.UnmarshalJSON(nil))
		assert.Nil(t, hn.Delay)
		assert.ErrorContains(t, json.Unmarshal([]byte(`{"minDelay":"x"}`), &hj), "hedge.minDelay")
		assert.ErrorContains(t, json.Unmarshal([]byte(`{"maxDelay":"x"}`), &hj), "hedge.maxDelay")
		assert.Error(t, json.Unmarshal([]byte(`{"maxCount":"x"}`), &hj))
	})
}

func TestCovDefaults_RateLimitPeriodCodec(t *testing.T) {
	cases := []struct {
		in   string
		want RateLimitPeriod
	}{
		{"1", RateLimitPeriodMinute}, {"second", RateLimitPeriodSecond}, {"1s", RateLimitPeriodSecond},
		{"Minute", RateLimitPeriodMinute}, {"60s", RateLimitPeriodMinute}, {"hour", RateLimitPeriodHour},
		{"3600s", RateLimitPeriodHour}, {"1d", RateLimitPeriodDay}, {"86400s", RateLimitPeriodDay},
		{"7d", RateLimitPeriodWeek}, {"604800s", RateLimitPeriodWeek}, {"30d", RateLimitPeriodMonth},
		{"720h", RateLimitPeriodMonth}, {"365d", RateLimitPeriodYear}, {"8760h", RateLimitPeriodYear},
		{"1000ms", RateLimitPeriodSecond}, {"1m0s", RateLimitPeriodMinute}, {"60m", RateLimitPeriodHour},
		{"1440m", RateLimitPeriodDay}, {"10080m", RateLimitPeriodWeek}, {"43200m", RateLimitPeriodMonth},
		{"525600m", RateLimitPeriodYear},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			var p RateLimitPeriod
			require.NoError(t, yaml.Unmarshal([]byte(tc.in), &p))
			assert.Equal(t, tc.want, p)
		})
	}
	for _, bad := range []string{"99", "2s", "fortnight", "[1]"} {
		var p RateLimitPeriod
		assert.Error(t, yaml.Unmarshal([]byte(bad), &p), bad)
	}
	var p RateLimitPeriod
	assert.EqualError(t, yaml.Unmarshal([]byte("2s"), &p), "rate limiter period must be one of: second, minute, hour, day, week, month, year (got 2s)")
	assert.EqualError(t, yaml.Unmarshal([]byte("{a: 1}"), &p), "invalid period type; expected string enum, integer enum, or duration like '1s'")

	names := map[RateLimitPeriod]string{
		RateLimitPeriodSecond: "second", RateLimitPeriodMinute: "minute", RateLimitPeriodHour: "hour",
		RateLimitPeriodDay: "day", RateLimitPeriodWeek: "week", RateLimitPeriodMonth: "month",
		RateLimitPeriodYear: "year", RateLimitPeriod(42): "unknown",
	}
	units := map[RateLimitPeriod]pb.RateLimitResponse_RateLimit_Unit{
		RateLimitPeriodSecond: pb.RateLimitResponse_RateLimit_SECOND, RateLimitPeriodMinute: pb.RateLimitResponse_RateLimit_MINUTE,
		RateLimitPeriodHour: pb.RateLimitResponse_RateLimit_HOUR, RateLimitPeriodDay: pb.RateLimitResponse_RateLimit_DAY,
		RateLimitPeriodWeek: pb.RateLimitResponse_RateLimit_WEEK, RateLimitPeriodMonth: pb.RateLimitResponse_RateLimit_MONTH,
		RateLimitPeriodYear: pb.RateLimitResponse_RateLimit_YEAR, RateLimitPeriod(42): pb.RateLimitResponse_RateLimit_UNKNOWN,
	}
	for period, name := range names {
		assert.Equal(t, name, period.String())
		y, err := period.MarshalYAML()
		require.NoError(t, err)
		assert.Equal(t, name, y)
		j, err := period.MarshalJSON()
		require.NoError(t, err)
		assert.Equal(t, `"`+name+`"`, string(j))
		assert.Equal(t, units[period], period.Unit())
	}
}

func TestCovDefaults_RateLimitRuleHelpers(t *testing.T) {
	cases := []struct {
		rule      RateLimitRuleConfig
		scope     string
		countMode string
	}{
		{RateLimitRuleConfig{}, "", "request"},
		{RateLimitRuleConfig{PerUser: true, PerNetwork: true, PerIP: true, CountMode: "credit"}, "user,network,ip", "credit"},
		{RateLimitRuleConfig{PerIP: true}, "ip", "request"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.scope, tc.rule.ScopeString())
		assert.Equal(t, tc.countMode, tc.rule.CountModeString())
	}

}

func TestCovDefaults_SecretsAreRedactedWhenMarshalled(t *testing.T) {
	redis := &RedisConnectorConfig{Addr: "r:6379", Username: "u", Password: "hunter2", URI: "redis://u:hunter2@r:6379/0", InitTimeout: covDefaultsDur(time.Second)}
	j, err := json.Marshal(redis)
	require.NoError(t, err)
	assert.NotContains(t, string(j), "hunter2")
	var rm map[string]interface{}
	require.NoError(t, json.Unmarshal(j, &rm))
	assert.Equal(t, "REDACTED", rm["password"])
	assert.Equal(t, "1s", rm["initTimeout"])
	y, err := yaml.Marshal(redis)
	require.NoError(t, err)
	assert.NotContains(t, string(y), "hunter2")
	assert.Contains(t, string(y), "lockRetryInterval:")

	pg := &PostgreSQLConnectorConfig{ConnectionUri: "postgres://u:pw-secret@db:5432/x", Table: "t", MinConns: 1, MaxConns: 2}
	j, err = json.Marshal(pg)
	require.NoError(t, err)
	assert.NotContains(t, string(j), "pw-secret")
	assert.Contains(t, string(j), `"minConns":"1"`)
	y, err = yaml.Marshal(pg)
	require.NoError(t, err)
	assert.NotContains(t, string(y), "pw-secret")
	assert.Contains(t, string(y), "table: t")

	aws := &AwsAuthConfig{Mode: "secret", AccessKeyID: "AKIA", SecretAccessKey: "very-secret"}
	j, err = json.Marshal(aws)
	require.NoError(t, err)
	assert.JSONEq(t, `{"mode":"secret","credentialsFile":"","profile":"","accessKeyID":"AKIA","secretAccessKey":"REDACTED"}`, string(j))
	y, err = yaml.Marshal(aws)
	require.NoError(t, err)
	assert.NotContains(t, string(y), "very-secret")

	prov := &ProviderConfig{Id: "p", Vendor: "alchemy", Settings: VendorSettings{"apiKey": "ak-secret"}, IgnoreNetworks: []string{"evm:5"}}
	j, err = json.Marshal(prov)
	require.NoError(t, err)
	assert.NotContains(t, string(j), "ak-secret")
	assert.Contains(t, string(j), `"settings":"REDACTED"`)
	y, err = yaml.Marshal(prov)
	require.NoError(t, err)
	assert.NotContains(t, string(y), "ak-secret")
	assert.Contains(t, string(y), "evm:5")

	ups := &UpstreamConfig{Id: "u", Endpoint: "https://node.example/v2/topsecretkey"}
	j, err = json.Marshal(ups)
	require.NoError(t, err)
	assert.NotContains(t, string(j), "topsecretkey")
	assert.Contains(t, string(j), "https://node.example#redacted=")
	y, err = yaml.Marshal(ups)
	require.NoError(t, err)
	assert.NotContains(t, string(y), "topsecretkey")
	assert.Equal(t, "https://node.example/v2/topsecretkey", ups.Endpoint, "marshalling never mutates the source")

	sec := &SecretStrategyConfig{Id: "s", Value: "tok-secret", RateLimitBudget: "rb"}
	j, err = json.Marshal(sec)
	require.NoError(t, err)
	assert.JSONEq(t, `{"value":"REDACTED"}`, string(j))
	y, err = yaml.Marshal(sec)
	require.NoError(t, err)
	assert.NotContains(t, string(y), "tok-secret")
	assert.Contains(t, string(y), "rateLimitBudget: rb")
}

func TestCovDefaults_UpstreamCopyIsDeep(t *testing.T) {
	lbm := int64(5)
	slot := int64(9)
	src := &UpstreamConfig{
		Id:                "u",
		Evm:               &EvmUpstreamConfig{ChainId: 1, BlockAvailability: &EvmBlockAvailabilityConfig{Lower: &EvmAvailabilityBoundConfig{LatestBlockMinus: &lbm}}, SkipWhenSyncing: util.BoolPtr(true), SkipSyncingCheck: util.BoolPtr(true), PinLatestTag: util.BoolPtr(true), DeprecatedGetLogsSplitOnError: util.BoolPtr(true), QueryShim: &EvmQueryShimConfig{Enabled: util.BoolPtr(true), AllowedMethods: []string{"eth_queryBlocks"}}, DeprecatedIntegrity: &UpstreamIntegrityConfig{EthGetBlockReceipts: &UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true, CheckLogIndexStrictIncrements: util.BoolPtr(true), CheckLogsBloom: util.BoolPtr(true)}}},
		Svm:               &SvmUpstreamConfig{Chain: "solana", BlockAvailability: &SvmBlockAvailabilityConfig{Lower: &SvmAvailabilityBoundConfig{LatestSlotMinus: &slot, ExactSlot: &slot, FirstAvailableSlotPlus: &slot}, Upper: &SvmAvailabilityBoundConfig{}}},
		Failsafe:          []*FailsafeConfig{{MatchFinality: []DataFinalityState{DataFinalityStateFinalized}, MatchCommitment: []string{"confirmed"}, Retry: &RetryPolicyConfig{MaxAttempts: 2}, CircuitBreaker: &CircuitBreakerPolicyConfig{FailureThresholdCount: 3}, Timeout: &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Base: 1}}, Hedge: &HedgePolicyConfig{MaxCount: 2, Delay: &AdaptiveDuration{Base: 2}}, Consensus: &ConsensusPolicyConfig{
			MaxParticipants:         3,
			PunishMisbehavior:       &PunishMisbehaviorConfig{DisputeThreshold: 4},
			MisbehaviorsDestination: &MisbehaviorsDestinationConfig{Type: MisbehaviorsDestinationTypeS3, S3: &S3FlushConfig{MaxRecords: 1, Credentials: &AwsAuthConfig{Mode: "env"}}},
			IgnoreFields:            map[string][]string{"eth_getLogs": {"a"}},
			PreferHighestValueFor:   map[string][]string{"eth_blockNumber": {"result"}},
			MaxWaitOnResult:         &AdaptiveDuration{Base: 3},
			MaxWaitOnEmpty:          &AdaptiveDuration{Base: 4},
			RequiredParticipants:    []*ConsensusRequiredParticipant{nil, {Tag: "t", MinParticipants: 1}},
		}}},
		JsonRpc:           &JsonRpcUpstreamConfig{SupportsBatch: util.BoolPtr(true), EnableGzip: util.BoolPtr(true), Headers: map[string]string{"h": "v"}},
		Grpc:              &GrpcUpstreamConfig{Headers: map[string]string{"g": "v"}},
		RateLimitAutoTune: &RateLimitAutoTuneConfig{Enabled: util.BoolPtr(true)},
		IgnoreMethods:     []string{"a"},
		AllowMethods:      []string{"b"},
	}
	cp := src.Copy()
	require.Equal(t, src, cp, "copy is value-equal")

	// Mutating every nested piece of the copy must leave the source untouched.
	*cp.Evm.BlockAvailability.Lower.LatestBlockMinus = 99
	*cp.Evm.SkipWhenSyncing = false
	*cp.Evm.PinLatestTag = false
	*cp.Evm.DeprecatedGetLogsSplitOnError = false
	*cp.Evm.QueryShim.Enabled = false
	cp.Evm.QueryShim.AllowedMethods[0] = "x"
	*cp.Evm.DeprecatedIntegrity.EthGetBlockReceipts.CheckLogsBloom = false
	*cp.Svm.BlockAvailability.Lower.LatestSlotMinus = 0
	cp.Failsafe[0].MatchFinality[0] = DataFinalityStateUnfinalized
	cp.Failsafe[0].MatchCommitment[0] = "x"
	cp.Failsafe[0].Retry.MaxAttempts = 9
	cp.Failsafe[0].CircuitBreaker.FailureThresholdCount = 9
	cp.Failsafe[0].Timeout.Duration.Base = 9
	cp.Failsafe[0].Hedge.Delay.Base = 9
	cs := cp.Failsafe[0].Consensus
	cs.PunishMisbehavior.DisputeThreshold = 9
	cs.MisbehaviorsDestination.S3.Credentials.Mode = "x"
	cs.IgnoreFields["eth_getLogs"][0] = "x"
	cs.PreferHighestValueFor["eth_blockNumber"][0] = "x"
	cs.MaxWaitOnResult.Base = 9
	cs.RequiredParticipants[1].Tag = "x"
	cp.Grpc.Headers["g"] = "x"
	cp.IgnoreMethods[0] = "x"
	cp.AllowMethods[0] = "x"

	assert.Equal(t, int64(5), *src.Evm.BlockAvailability.Lower.LatestBlockMinus)
	assert.True(t, *src.Evm.SkipWhenSyncing)
	assert.True(t, *src.Evm.PinLatestTag)
	assert.True(t, *src.Evm.DeprecatedGetLogsSplitOnError)
	assert.True(t, *src.Evm.QueryShim.Enabled)
	assert.Equal(t, "eth_queryBlocks", src.Evm.QueryShim.AllowedMethods[0])
	assert.True(t, *src.Evm.DeprecatedIntegrity.EthGetBlockReceipts.CheckLogsBloom)
	assert.Equal(t, int64(9), *src.Svm.BlockAvailability.Lower.LatestSlotMinus)
	sf := src.Failsafe[0]
	assert.Equal(t, DataFinalityStateFinalized, sf.MatchFinality[0])
	assert.Equal(t, "confirmed", sf.MatchCommitment[0])
	assert.Equal(t, 2, sf.Retry.MaxAttempts)
	assert.Equal(t, uint(3), sf.CircuitBreaker.FailureThresholdCount)
	assert.Equal(t, Duration(1), sf.Timeout.Duration.Base)
	assert.Equal(t, Duration(2), sf.Hedge.Delay.Base)
	assert.Equal(t, uint(4), sf.Consensus.PunishMisbehavior.DisputeThreshold)
	assert.Equal(t, "env", sf.Consensus.MisbehaviorsDestination.S3.Credentials.Mode)
	assert.Equal(t, "a", sf.Consensus.IgnoreFields["eth_getLogs"][0])
	assert.Equal(t, "result", sf.Consensus.PreferHighestValueFor["eth_blockNumber"][0])
	assert.Equal(t, Duration(3), sf.Consensus.MaxWaitOnResult.Base)
	assert.Equal(t, "t", sf.Consensus.RequiredParticipants[1].Tag)
	assert.Nil(t, cp.Failsafe[0].Consensus.RequiredParticipants[0])
	assert.Equal(t, "v", src.Grpc.Headers["g"])
	assert.Equal(t, "a", src.IgnoreMethods[0])
	assert.Equal(t, "b", src.AllowMethods[0])

	// Nil receivers copy to nil.
	assert.Nil(t, (*UpstreamConfig)(nil).Copy())
	assert.Nil(t, (*EvmUpstreamConfig)(nil).Copy())
	assert.Nil(t, (*FailsafeConfig)(nil).Copy())
	assert.Nil(t, (*RetryPolicyConfig)(nil).Copy())
	assert.Nil(t, (*CircuitBreakerPolicyConfig)(nil).Copy())
	assert.Nil(t, (*TimeoutPolicyConfig)(nil).Copy())
	assert.Nil(t, (*HedgePolicyConfig)(nil).Copy())
	assert.Nil(t, (*ConsensusPolicyConfig)(nil).Copy())
	assert.Nil(t, (*MisbehaviorsDestinationConfig)(nil).Copy())
	assert.Nil(t, (*PunishMisbehaviorConfig)(nil).Copy())
	assert.Nil(t, (*UpstreamIntegrityConfig)(nil).Copy())
	assert.Nil(t, (*UpstreamIntegrityEthGetBlockReceiptsConfig)(nil).Copy())
	assert.Nil(t, (*EvmQueryShimConfig)(nil).Copy())
	assert.Nil(t, (*EvmBlockAvailabilityConfig)(nil).Copy())
	assert.Nil(t, (*EvmAvailabilityBoundConfig)(nil).Copy())
	assert.Nil(t, (*SvmUpstreamConfig)(nil).Copy())
	assert.Nil(t, (*SvmBlockAvailabilityConfig)(nil).Copy())
	assert.Nil(t, (*SvmAvailabilityBoundConfig)(nil).Copy())
	assert.Nil(t, (*JsonRpcUpstreamConfig)(nil).Copy())
	assert.Nil(t, (*GrpcUpstreamConfig)(nil).Copy())
	assert.Nil(t, (*RateLimitAutoTuneConfig)(nil).Copy())
}

func TestCovDefaults_QueryShimAllowsMethod(t *testing.T) {
	cases := []struct {
		shim   *EvmQueryShimConfig
		method string
		want   bool
	}{
		{nil, "eth_queryBlocks", false},
		{&EvmQueryShimConfig{}, "eth_queryBlocks", false},
		{&EvmQueryShimConfig{Enabled: util.BoolPtr(false)}, "eth_queryBlocks", false},
		{&EvmQueryShimConfig{Enabled: util.BoolPtr(true)}, "eth_queryBlocks", true},
		{&EvmQueryShimConfig{Enabled: util.BoolPtr(true), AllowedMethods: []string{"eth_queryL*"}}, "eth_queryLogs", true},
		{&EvmQueryShimConfig{Enabled: util.BoolPtr(true), AllowedMethods: []string{"eth_queryL*"}}, "eth_queryBlocks", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tc.shim.AllowsMethod(tc.method), "%+v %s", tc.shim, tc.method)
	}
}

func TestCovDefaults_NetworkIdAndHelpers(t *testing.T) {
	cases := []struct {
		n    NetworkConfig
		want string
	}{
		{NetworkConfig{}, ""},
		{NetworkConfig{Architecture: ArchitectureEvm}, ""},
		{NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{ChainId: 137}}, "evm:137"},
		{NetworkConfig{Architecture: ArchitectureSvm}, ""},
		{NetworkConfig{Architecture: ArchitectureSvm, Svm: &SvmNetworkConfig{}}, ""},
		{NetworkConfig{Architecture: ArchitectureSvm, Svm: &SvmNetworkConfig{Cluster: "mainnet-beta"}}, "svm:mainnet-beta"},
		{NetworkConfig{Architecture: ArchitectureSvm, Svm: &SvmNetworkConfig{Chain: "fogo", Cluster: "testnet"}}, "svm:fogo:testnet"},
		{NetworkConfig{Architecture: "btc"}, ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tc.n.NetworkId())
	}

	var nilNet *NetworkConfig
	assert.True(t, nilNet.MultiplexingEnabled(), "multiplexing defaults on")
	assert.False(t, (*FailoverConfig)(nil).Enabled())
	assert.False(t, (&FailoverConfig{}).Enabled())
	var nilEvm *EvmNetworkConfig
	assert.False(t, nilEvm.ShortCircuitFutureBlocksEnabled())
	assert.Equal(t, ServedTipQuorum(0), nilEvm.ServedTipQuorum())

	assert.Equal(t, "evm:1:42", CachePartitionKey("evm:1", "", "42"))
	assert.Equal(t, "evm:1:sfx:42", CachePartitionKey("evm:1", "sfx", "42"))
	assert.Equal(t, `evm:1:systx\:foo`, CachePartitionKey("evm:1", "", "systx:foo"))
	assert.Equal(t, `evm:1:a\\b:*`, CachePartitionKey("evm:1", `a\b`, "*"))
	assert.Equal(t, `svm\:mainnet:bar:1`, CachePartitionKey("svm:mainnet", "bar", "1"), "suffix that would form another network id escapes the network id")
}

func TestCovDefaults_MetricsTelemetryOptions(t *testing.T) {
	assert.Nil(t, (*MetricsConfig)(nil).TelemetryOptions())

	evict := covDefaultsDur(2 * time.Hour)
	m := &MetricsConfig{
		HistogramBuckets:        "0.1,1",
		HistogramDropLabels:     []string{"a"},
		HistogramLabelOverrides: map[string][]string{"h": {"b"}},
		CounterDropLabels:       []string{"c"},
		CounterLabelOverrides:   map[string][]string{"k": {"d"}},
		Customizations: []*MetricsCustomizationConfig{
			nil,
			{Subject: "consensus_*", Action: "drop"},
			{Subject: "x", Buckets: []float64{1, 2}, Labels: []*MetricLabelCustomizationConfig{nil, {Subject: "network", Action: "keep"}}},
		},
		CounterIdleEvictionAfter: &evict,
	}
	o := m.TelemetryOptions()
	assert.Equal(t, "0.1,1", o.HistogramBuckets)
	assert.Equal(t, []string{"a"}, o.LegacyLabels.HistogramDropLabels)
	assert.Equal(t, map[string][]string{"k": {"d"}}, o.LegacyLabels.CounterLabelOverrides)
	require.Len(t, o.Customizations, 2, "nil customizations are skipped")
	assert.Equal(t, "consensus_*", o.Customizations[0].Subject)
	assert.Equal(t, "drop", o.Customizations[0].Action)
	assert.Equal(t, []float64{1, 2}, o.Customizations[1].Buckets)
	require.Len(t, o.Customizations[1].Labels, 1)
	assert.Equal(t, "network", o.Customizations[1].Labels[0].Subject)
	assert.Equal(t, "keep", o.Customizations[1].Labels[0].Action)
	require.NotNil(t, o.CounterIdleEvictionAfter)
	assert.Equal(t, 2*time.Hour, *o.CounterIdleEvictionAfter)

	assert.Nil(t, (&MetricsConfig{}).TelemetryOptions().CounterIdleEvictionAfter)
}
