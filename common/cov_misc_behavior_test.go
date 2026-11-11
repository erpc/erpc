package common

import (
	"bytes"
	"context"
	"crypto/x509"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/grafana/sobek"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"gopkg.in/yaml.v3"
)

// ---- timeout_func.go ----

func TestCovMiscTimeoutFunc_DisabledAndStatic(t *testing.T) {
	logger := zerolog.Nop()
	assert.Nil(t, NewTimeoutFunc(&logger, nil))
	assert.Nil(t, NewTimeoutFunc(&logger, &TimeoutPolicyConfig{}))
	assert.Nil(t, NewTimeoutFunc(&logger, &TimeoutPolicyConfig{Duration: &AdaptiveDuration{}}))
	// Only Min set, no quantile: static Resolve returns Base=0 → no timeout.
	assert.Nil(t, NewTimeoutFunc(&logger, &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Min: Duration(time.Second)}}))

	fn := NewTimeoutFunc(&logger, &TimeoutPolicyConfig{Duration: NewStaticDuration(3 * time.Second)})
	require.NotNil(t, fn)
	got := fn(context.Background(), nil)
	require.NotNil(t, got)
	assert.Equal(t, 3*time.Second, *got)
}

func TestCovMiscTimeoutFunc_QuantilePaths(t *testing.T) {
	logger := zerolog.Nop()
	ntw := &covMiscNetwork{id: "evm:1", label: "mainnet", finality: DataFinalityStateUnfinalized, metrics: map[string]TrackedMetrics{
		"eth_call":    &covMiscMetrics{q: &fakeQuantile{val: 400 * time.Millisecond}},
		"eth_getLogs": &covMiscMetrics{q: &fakeQuantile{val: 0}},
	}}
	mkReq := func(method string, withNet bool) *NormalizedRequest {
		r := NewNormalizedRequestFromJsonRpcRequest(NewJsonRpcRequest(method, nil))
		if withNet {
			r.SetNetwork(ntw)
		}
		return r
	}
	ctx := context.Background()

	t.Run("base set: min auto-floors to base/2", func(t *testing.T) {
		fn := NewTimeoutFunc(&logger, &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Base: Duration(2 * time.Second), Quantile: 0.9, Max: Duration(10 * time.Second)}})
		require.NotNil(t, fn)
		assert.Equal(t, 2400*time.Millisecond, *fn(ctx, mkReq("eth_call", true)))
		// no latency data: Resolve → base + min(=1s) = 3s
		assert.Equal(t, 3*time.Second, *fn(ctx, mkReq("eth_getLogs", true)))
		// cold-start fallbacks use Base
		assert.Equal(t, 2*time.Second, *fn(ctx, mkReq("eth_call", false)))
		assert.Equal(t, 2*time.Second, *fn(ctx, mkReq("", true)))
		assert.Equal(t, 2*time.Second, *fn(ctx, mkReq("eth_unknownMethod", true)))
	})

	t.Run("no base: min auto-floors to 500ms; fallback is max", func(t *testing.T) {
		fn := NewTimeoutFunc(&logger, &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Quantile: 0.5, Max: Duration(5 * time.Second)}})
		require.NotNil(t, fn)
		assert.Equal(t, 500*time.Millisecond, *fn(ctx, mkReq("eth_call", true)), "400ms raised to 500ms floor")
		assert.Equal(t, 5*time.Second, *fn(ctx, mkReq("eth_call", false)))
	})

	t.Run("explicit min is kept", func(t *testing.T) {
		fn := NewTimeoutFunc(&logger, &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Quantile: 0.5, Min: Duration(10 * time.Millisecond), Max: Duration(5 * time.Second)}})
		assert.Equal(t, 400*time.Millisecond, *fn(ctx, mkReq("eth_call", true)))
	})

	t.Run("fallback nil when neither base nor max", func(t *testing.T) {
		assert.Nil(t, coldStartFallback(&AdaptiveDuration{Quantile: 0.5}))
	})

	t.Run("non-positive resolve falls back", func(t *testing.T) {
		// A negative base makes Resolve non-positive even with tracker data.
		fn := NewTimeoutFunc(&logger, &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Base: Duration(-time.Second), Quantile: 0.5, Min: Duration(time.Millisecond), Max: Duration(time.Hour)}})
		require.NotNil(t, fn)
		assert.Equal(t, time.Millisecond, *fn(ctx, mkReq("eth_call", true)), "min clamp keeps it positive")
	})
}

// ---- match.go ----

type covMiscExec struct {
	id       string
	method   string
	finality []DataFinalityState
}

func TestCovMiscSelectExecutor_Priority(t *testing.T) {
	getM := func(e covMiscExec) string { return e.method }
	getF := func(e covMiscExec) []DataFinalityState { return e.finality }
	execs := []covMiscExec{
		{"catchall", "*", nil},
		{"catchall2", "", nil},
		{"finality-only", "", []DataFinalityState{DataFinalityStateRealtime}},
		{"finality-only-2", "*", []DataFinalityState{DataFinalityStateRealtime}},
		{"method", "eth_call", nil},
		{"method-2", "eth_*", nil},
		{"method+finality", "eth_call", []DataFinalityState{DataFinalityStateUnfinalized}},
		{"method+finality-2", "eth_call|eth_x", []DataFinalityState{DataFinalityStateUnfinalized}},
		{"other", "trace_*", []DataFinalityState{DataFinalityStateFinalized}},
	}
	cases := []struct {
		method   string
		finality DataFinalityState
		want     string
	}{
		{"eth_call", DataFinalityStateUnfinalized, "method+finality"},
		{"eth_call", DataFinalityStateFinalized, "method"},
		{"eth_getBalance", DataFinalityStateFinalized, "method-2"},
		{"debug_x", DataFinalityStateRealtime, "finality-only"},
		{"debug_x", DataFinalityStateUnknown, "catchall"},
		{"trace_block", DataFinalityStateFinalized, "other"},
	}
	for _, tc := range cases {
		got, ok := SelectExecutor(execs, tc.method, tc.finality, getM, getF)
		require.True(t, ok, tc.method)
		assert.Equal(t, tc.want, got.id, "%s/%s", tc.method, tc.finality)
	}

	none, ok := SelectExecutor(execs[4:6], "trace_x", DataFinalityStateFinalized, getM, getF)
	assert.False(t, ok)
	assert.Equal(t, covMiscExec{}, none)
	_, ok = SelectExecutor([]covMiscExec(nil), "eth_call", DataFinalityStateFinalized, getM, getF)
	assert.False(t, ok)

	assert.True(t, matchMethodPattern("", "x"))
	assert.True(t, matchMethodPattern("eth_call", "eth_call"))
	assert.False(t, matchMethodPattern("eth_call", "eth_callx"))
}

// ---- matcher.go ----

func TestCovMiscMatcher_NumericComparisons(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{">=0x10", "0x10", true},
		{">=0x10", "0xf", false},
		{"<=0x10", "0x10", true},
		{"<=0x10", "0x11", false},
		{">0x10", "0x11", true},
		{">0x10", "0x10", false},
		{"<0x10", "0xf", true},
		{"<16", "0xf", true},
		{"=0x10", "0x10", true},
		{"=0x10", " 0x10 ", true},
		{"=0x10", "0x11", false},
		{">=zz", "0x10", false},
		{">=0x10", "0xzz", false},
		{">0x10", "17", false}, // non-hex values fall back to glob
		{"<empty>", "", true},
		{"<empty>", "x", false},
		{"(eth_* & !eth_call) | debug_*", "eth_getLogs", true},
		{"(eth_* & !eth_call) | debug_*", "eth_call", false},
		{"(eth_* & !eth_call) | debug_*", "debug_trace", true},
		{"!!eth_call", "eth_call", true},
	}
	for _, tc := range cases {
		got, err := WildcardMatch(tc.pattern, tc.value)
		require.NoError(t, err, tc.pattern)
		assert.Equal(t, tc.want, got, "%q vs %q", tc.pattern, tc.value)
	}

	assert.False(t, compareNumbers("1", "1", "~"), "unknown operator never matches")
	n, err := parseNumber("0X1f")
	require.NoError(t, err)
	assert.Equal(t, int64(31), n)
	n, err = parseNumber("0xzz1")
	require.Error(t, err)
	assert.Zero(t, n)

	// unbalanced parens parse but evaluate per grammar
	m, err := NewWildcardMatcher("(a")
	require.Error(t, err)
	assert.Nil(t, m)
	ok, err := WildcardMatch("(a", "a")
	require.ErrorContains(t, err, "expected closing parenthesis")
	assert.False(t, ok)
	ok, err = WildcardMatch("(a)|", "x")
	require.NoError(t, err)
	assert.False(t, ok, "dangling operator matches nothing")
}

func TestCovMiscMatcher_ValidatePatternErrors(t *testing.T) {
	cases := []struct {
		pattern string
		wantErr string
	}{
		{"   ", "empty pattern"},
		{"a)", "unmatched closing parenthesis at position 1"},
		{"| a", "operator '|' missing operand at position 0"},
		{"a &", "operator '&' missing operand at position 1"},
		{"(| a)", "invalid left operand for '|' at position 1"},
		{"a | )", "invalid right operand for '|' at position 1"},
		{"a & |b", "invalid right operand for '&' at position 1"},
		{"a !", "NOT operator missing operand at position 1"},
		{"!)", "invalid operand for NOT at position 0"},
		{">=0xzz", "invalid hex number in comparison: >=0xzz"},
		{"<foo", "invalid hex number in comparison: <foo"},
		{"(a", "unclosed parenthesis"},
		{"a b", "unexpected token at position 1: b"},
		{"(a)", ""},
		{"<=0x1f", ""},
	}
	for _, tc := range cases {
		err := ValidatePattern(tc.pattern)
		if tc.wantErr == "" {
			assert.NoError(t, err, tc.pattern)
		} else {
			assert.ErrorContains(t, err, tc.wantErr, tc.pattern)
		}
	}
}

func TestCovMiscMatcher_SelectorsAndUpstreams(t *testing.T) {
	up := NewFakeUpstream("alchemy-1", WithTags("family:systx", "region:eu"))
	cases := []struct {
		pattern string
		want    bool
	}{
		{"alchemy-*", true},
		{"family:systx", true},
		{"region:*", true},
		{"!alchemy-1", false},
		{"infura-*", false},
	}
	for _, tc := range cases {
		got, err := UpstreamMatchesSelector(tc.pattern, up)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, tc.pattern)
	}
	got, err := UpstreamMatchesSelector("x", &covMiscNilCfgUpstream{FakeUpstream: NewFakeUpstream("x").(*FakeUpstream)})
	require.NoError(t, err)
	assert.True(t, got)

	// error propagation from malformed patterns
	_, err = MatchesSelector("(a", "id", nil)
	require.Error(t, err)
	_, err = SelectorAdmits("!(a", "id", nil)
	require.Error(t, err)
	ok, err := SelectorAdmits("!systx*", "prism", []string{"ok", "systx-1"})
	require.NoError(t, err)
	assert.False(t, ok, "one excluded tag rejects")
	ok, err = SelectorAdmits("!systx*", "prism", []string{"ok"})
	require.NoError(t, err)
	assert.True(t, ok)
}

type covMiscNilCfgUpstream struct{ *FakeUpstream }

func (u *covMiscNilCfgUpstream) Config() *UpstreamConfig { return nil }

// ---- static_response.go ----

func TestCovMiscStaticResponse_ValueEquality(t *testing.T) {
	cases := []struct {
		name string
		a, b interface{}
		want bool
	}{
		{"nil vs nil", nil, nil, true},
		{"nil vs value", nil, "x", false},
		{"int vs float64", 5, 5.0, true},
		{"int8 vs uint64", int8(3), uint64(3), true},
		{"int16 vs int32", int16(-2), int32(-2), true},
		{"int64 vs uint", int64(9), uint(9), true},
		{"uint8 vs uint16", uint8(1), uint16(1), true},
		{"uint32 vs float32", uint32(4), float32(4), true},
		{"number vs string", 1, "1", false},
		{"string mismatch type", "true", true, false},
		{"bool equal", true, true, true},
		{"bool vs string", false, "false", false},
		{"slice equal", []interface{}{1, "a"}, []interface{}{1.0, "a"}, true},
		{"slice length", []interface{}{1}, []interface{}{1, 2}, false},
		{"slice element", []interface{}{1}, []interface{}{2}, false},
		{"map equal", map[string]interface{}{"a": 1}, map[string]interface{}{"a": 1.0}, true},
		{"map size", map[string]interface{}{"a": 1}, map[string]interface{}{}, false},
		{"map missing key", map[string]interface{}{"a": 1}, map[string]interface{}{"b": 1}, false},
		{"map value", map[string]interface{}{"a": 1}, map[string]interface{}{"a": 2}, false},
		{"map vs slice", map[string]interface{}{}, []interface{}{}, false},
		{"fallback deep equal", []string{"x"}, []string{"x"}, true},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, valueEqual(tc.a, tc.b), tc.name)
	}

	entries := []*StaticResponseConfig{nil, {Method: "eth_chainId", Params: []interface{}{}}, {Method: "net_version", Params: []interface{}{1}}, {Method: "net_version", Params: []interface{}{2}}}
	assert.Same(t, entries[3], FindStaticResponseMatch(entries, "net_version", []interface{}{2.0}))
	assert.Nil(t, FindStaticResponseMatch(entries, "net_version", []interface{}{1, 2}))
	assert.Nil(t, FindStaticResponseMatch(entries, "eth_blockNumber", nil))
}

// ---- network.go / architecture_evm.go ----

func TestCovMiscNetwork_EvmHelpersDegradeForNonEvm(t *testing.T) {
	ctx := context.Background()
	plain := &covMiscNetwork{id: "svm:mainnet-beta"}
	assert.Zero(t, EvmHeadOf(plain, ctx, nil))

	leader := NewFakeUpstream("leader")
	evm := &covMiscEvmNetwork{covMiscNetwork: &covMiscNetwork{id: "evm:1"}, latest: 100, finalized: 90, leader: leader}
	head := EvmHeadOf(evm, ctx, nil)
	assert.Equal(t, int64(100), head.Latest)
	assert.Equal(t, int64(90), head.Finalized)
	assert.Same(t, leader, head.Leader)
}

func TestCovMiscNetwork_Validity(t *testing.T) {
	assert.True(t, IsValidArchitecture("evm"))
	assert.True(t, IsValidArchitecture("svm"))
	assert.False(t, IsValidArchitecture("btc"))

	cases := []struct {
		network string
		want    bool
	}{
		{"evm:1", true},
		{"evm:0", false},
		{"evm:-5", false},
		{"evm:abc", false},
		{"svm:", false},
		{"svm:mainnet-beta", true},
		{"svm:solana:devnet", true},
		{"svm:eclipse:my_cluster.v2", true},
		{"svm:localnet", true},
		{"svm:a:b:c", false},
		{"svm:chain:", false},
		{"svm:ch@in:x", false},
		{"svm:chain:cl ster", false},
		{"cosmos:1", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, IsValidNetwork(tc.network), tc.network)
	}
}

func TestCovMiscArchitectureEvm_Enums(t *testing.T) {
	assert.Equal(t, "blockHead", AvailbilityConfidenceBlockHead.String())
	assert.Equal(t, "finalizedBlock", AvailbilityConfidenceFinalized.String())
	assert.Equal(t, "unknown(0)", AvailbilityConfidence(0).String())
	y, err := yaml.Marshal(AvailbilityConfidenceFinalized)
	require.NoError(t, err)
	assert.Equal(t, "finalizedBlock\n", string(y))
	j, err := AvailbilityConfidenceBlockHead.MarshalJSON()
	require.NoError(t, err)
	assert.Equal(t, `"blockHead"`, string(j))

	cases := []struct {
		in      string
		want    AvailbilityConfidence
		wantErr string
	}{
		{"blockHead", AvailbilityConfidenceBlockHead, ""},
		{"1", AvailbilityConfidenceBlockHead, ""},
		{"FinalizedBlock", AvailbilityConfidenceFinalized, ""},
		{"2", AvailbilityConfidenceFinalized, ""},
		{"safe", 0, "invalid availability confidence: safe"},
		{"[1]", 0, "cannot unmarshal"},
	}
	for _, tc := range cases {
		var got AvailbilityConfidence
		err := yaml.Unmarshal([]byte(tc.in), &got)
		if tc.wantErr != "" {
			require.ErrorContains(t, err, tc.wantErr, tc.in)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}

	assert.Equal(t, "syncing", EvmSyncingStateSyncing.String())
	assert.Equal(t, "not_syncing", EvmSyncingStateNotSyncing.String())
	assert.Equal(t, "unknown(0)", EvmSyncingStateUnknown.String())
}

// ---- evm_local_answers.go ----

func TestCovMiscLocalAnswers(t *testing.T) {
	ctx := context.Background()
	synced := covMiscNewSyncedUpstream("synced", EvmSyncingStateNotSyncing)
	syncing := covMiscNewSyncedUpstream("syncing", EvmSyncingStateSyncing)
	cfg := &NetworkConfig{Evm: &EvmNetworkConfig{ChainId: 137}}
	evm := &covMiscEvmNetwork{covMiscNetwork: &covMiscNetwork{id: "evm:137", cfg: cfg}, routable: []Upstream{syncing, synced, NewFakeUpstream("plain")}}
	plain := &covMiscNetwork{id: "evm:0", cfg: &NetworkConfig{}}

	assert.Equal(t, []string{"eth_chainId", "eth_syncing", "net_listening", "net_peerCount", "net_version", "web3_clientVersion"}, LocalAnswerMethods())

	cases := []struct {
		name   string
		n      Network
		method string
		want   any
		ok     bool
	}{
		{"chainId hex", evm, "eth_chainId", "0x89", true},
		{"chainId case-insensitive", evm, "ETH_CHAINID", "0x89", true},
		{"chainId unknown", plain, "eth_chainId", nil, false},
		{"net_version", evm, "net_version", "137", true},
		{"net_version unknown", plain, "net_version", nil, false},
		{"listening", plain, "net_listening", true, true},
		{"peerCount", evm, "net_peerCount", "0x3", true},
		{"peerCount without provider", plain, "net_peerCount", nil, false},
		{"syncing false when one synced", evm, "eth_syncing", false, true},
		{"syncing without provider", plain, "eth_syncing", nil, false},
		{"not a local method", evm, "eth_call", nil, false},
	}
	for _, tc := range cases {
		got, ok := LocalAnswer(ctx, tc.n, tc.method)
		assert.Equal(t, tc.ok, ok, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}

	v, ok := LocalAnswer(ctx, plain, "web3_clientVersion")
	require.True(t, ok)
	assert.Equal(t, "erpc/"+ErpcVersion+"/"+runtime.GOOS+"-"+runtime.GOARCH+"/"+runtime.Version(), v)

	onlySyncing := &covMiscEvmNetwork{covMiscNetwork: &covMiscNetwork{id: "evm:137", cfg: cfg}, routable: []Upstream{syncing}}
	got, ok := LocalAnswer(ctx, onlySyncing, "eth_syncing")
	assert.False(t, ok, "no synced upstream: defer to upstreams")
	assert.Nil(t, got)

	disabled := &covMiscEvmNetwork{covMiscNetwork: &covMiscNetwork{id: "evm:137", cfg: &NetworkConfig{Evm: &EvmNetworkConfig{ChainId: 137, LocalAnswers: map[string]bool{"eth_chainId": false, "net_version": true}}}}}
	_, ok = LocalAnswer(ctx, disabled, "eth_chainId")
	assert.False(t, ok, "turned off per network")
	got, ok = LocalAnswer(ctx, disabled, "net_version")
	assert.True(t, ok)
	assert.Equal(t, "137", got)

	nilCfg := &covMiscNetwork{id: "evm:1"}
	_, ok = LocalAnswer(ctx, nilCfg, "eth_chainId")
	assert.False(t, ok)
}

// ---- config_integrity.go ----

func TestCovMiscIntegrity_MergeOverridesEveryField(t *testing.T) {
	tru, fls := true, false
	dest := &MisbehaviorsDestinationConfig{}
	base := &IntegrityConfig{
		IntegritySettings: IntegritySettings{
			Level:  "intrinsic",
			Checks: map[string]*IntegrityCheckConfig{"keep": {OnFailure: "recordOnly"}},
		},
	}
	over := &IntegrityConfig{
		IntegritySettings: IntegritySettings{
			Budget:                  &IntegrityBudgetConfig{MaxConcurrent: 4},
			InvalidBehavior:         &IntegrityInvalidBehaviorConfig{Finalized: "hardReject"},
			Follow:                  &IntegrityFollowConfig{Enabled: &tru},
			StateProbe:              &IntegrityStateProbeConfig{Enabled: &fls},
			ObserveOnly:             &tru,
			AutoCorrectWhenPossible: &fls,
			MisbehaviorsDestination: dest,
			Checks:                  map[string]*IntegrityCheckConfig{"new": {Enabled: &tru, Params: map[string]string{"m": "superset"}}},
		},
		HeaderMode: "profiles",
		Profiles:   map[string]*IntegritySettings{"strict": {Level: "authoritative"}},
	}
	m := MergeIntegrityConfig(base, over)
	assert.Equal(t, "intrinsic", m.Level)
	assert.Equal(t, 4, m.Budget.MaxConcurrent)
	assert.Equal(t, "hardReject", m.InvalidBehavior.Finalized)
	assert.True(t, m.Follow.IsEnabled())
	assert.False(t, m.StateProbe.IsEnabled())
	assert.True(t, *m.ObserveOnly)
	assert.False(t, *m.AutoCorrectWhenPossible)
	assert.Same(t, dest, m.MisbehaviorsDestination)
	assert.Contains(t, m.Checks, "keep")
	assert.Equal(t, "superset", m.Checks["new"].Params["m"])
	assert.Equal(t, "profiles", m.HeaderMode)
	assert.Equal(t, "authoritative", m.Profiles["strict"].Level)

	// over.Checks copied, not aliased
	over.Checks["new"].Params["m"] = "eq"
	assert.Equal(t, "superset", m.Checks["new"].Params["m"])

	// base with nil maps gets them created
	m2 := MergeIntegrityConfig(&IntegrityConfig{}, over)
	assert.Len(t, m2.Checks, 1)
	assert.Len(t, m2.Profiles, 1)
	assert.Nil(t, MergeIntegrityConfig(nil, nil))
}

func TestCovMiscIntegrity_CopiesAreDeep(t *testing.T) {
	tru := true
	follow := &IntegrityFollowConfig{Enabled: &tru}
	fc := follow.Copy()
	*follow.Enabled = false
	assert.True(t, fc.IsEnabled())
	assert.Nil(t, (*IntegrityFollowConfig)(nil).Copy())
	assert.False(t, (*IntegrityFollowConfig)(nil).IsEnabled())
	assert.False(t, (&IntegrityFollowConfig{}).Copy().IsEnabled())

	tru2 := true
	probe := &IntegrityStateProbeConfig{Enabled: &tru2}
	pc := probe.Copy()
	*probe.Enabled = false
	assert.True(t, pc.IsEnabled())
	assert.Nil(t, (*IntegrityStateProbeConfig)(nil).Copy())
	assert.False(t, (&IntegrityStateProbeConfig{}).Copy().IsEnabled())

	assert.Nil(t, (*IntegritySettings)(nil).Copy())
	assert.Nil(t, (*IntegrityConfig)(nil).Copy())
	assert.Nil(t, (*IntegrityCheckConfig)(nil).Copy())
	assert.Nil(t, (*IntegrityBudgetConfig)(nil).Copy())
	assert.Nil(t, (*IntegrityInvalidBehaviorConfig)(nil).Copy())
}

// ---- exec_state.go ----

func TestCovMiscExecState_SnapshotLogAndApply(t *testing.T) {
	var nilState *ExecState
	nilState.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "x"})
	nilState.MarkUpstreamAttemptWon("x")
	nilState.Apply(nil)
	assert.Nil(t, nilState.UpstreamAttemptLog())
	assert.Equal(t, ExecStateSnapshot{}, nilState.Snapshot())
	assert.Zero(t, nilState.CreditUnitsTotal())
	assert.Nil(t, nilState.CreditUnitsByVendor())

	started := time.Unix(1700000000, 0)
	s := &ExecState{StartedAt: started}
	s.UpstreamAttempts.Store(3)
	s.UpstreamRetries.Store(1)
	s.UpstreamHedges.Store(1)
	s.NetworkAttempts.Store(2)
	s.NetworkRetries.Store(1)
	s.CacheAttempts.Store(1)
	s.CacheHedges.Store(2)
	s.ConsensusSlots.Store(3)
	s.ConsensusDisputes.Store(1)
	s.ConsensusLowParticipants.Store(1)

	snap := s.Snapshot()
	assert.Equal(t, 4, snap.Attempts, "network attempts are rotations, not summed")
	assert.Equal(t, 2, snap.Retries)
	assert.Equal(t, 3, snap.Hedges)
	assert.Equal(t, 3, snap.ConsensusSlots)
	assert.Equal(t, 1, snap.ConsensusDisputes)
	assert.Equal(t, 1, snap.ConsensusLowParticipants)
	assert.Equal(t, started, snap.StartedAt)

	// Apply without attempts: counters only
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	_, span := tp.Tracer("t").Start(context.Background(), "a")
	s.Apply(span)
	span.End()
	attrs := covMiscAttrs(sr.Ended()[0].Attributes())
	assert.Equal(t, int64(4), attrs["execution.attempts"].AsInt64())
	assert.Equal(t, int64(2), attrs["execution.network_attempts"].AsInt64())
	assert.Equal(t, int64(2), attrs["execution.cache_hedges"].AsInt64())
	_, hasTried := attrs["upstreams.tried"]
	assert.False(t, hasTried)

	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "a", Outcome: UpstreamOutcomeTimeout, Reason: SelectionReasonPrimary, Duration: 1500 * time.Millisecond})
	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "b", Outcome: UpstreamOutcomeSuccess, Reason: SelectionReasonHedge, Duration: 20 * time.Millisecond})
	s.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "a", Outcome: UpstreamOutcomeSuccess, Reason: SelectionReasonRetry, Duration: 5 * time.Millisecond})
	s.MarkUpstreamAttemptWon("a")
	s.MarkUpstreamAttemptWon("")
	s.MarkUpstreamAttemptWon("missing")

	log := s.UpstreamAttemptLog()
	require.Len(t, log, 3)
	assert.False(t, log[0].Won, "only the most recent attempt on the upstream wins")
	assert.True(t, log[2].Won)
	log[0].UpstreamId = "mutated"
	assert.Equal(t, "a", s.UpstreamAttemptLog()[0].UpstreamId, "log is a copy")

	_, span = tp.Tracer("t").Start(context.Background(), "b")
	s.Apply(span)
	span.End()
	attrs = covMiscAttrs(sr.Ended()[1].Attributes())
	assert.Equal(t, int64(3), attrs["upstreams.attempts"].AsInt64())
	assert.Equal(t, []string{"a", "b", "a"}, attrs["upstreams.tried"].AsStringSlice())
	assert.Equal(t, []string{"timeout", "success", "success"}, attrs["upstreams.outcomes"].AsStringSlice())
	assert.Equal(t, []string{"primary", "hedge", "retry"}, attrs["upstreams.reasons"].AsStringSlice())
	assert.Equal(t, []int64{1500, 20, 5}, attrs["upstreams.durations_ms"].AsInt64Slice())
	assert.Equal(t, []bool{false, false, true}, attrs["upstreams.won"].AsBoolSlice())
	var _ attribute.Value = attrs["upstreams.won"]
}

func TestCovMiscExecState_HolderLazyInitOnce(t *testing.T) {
	var h execStateHolder
	var wg sync.WaitGroup
	got := make([]*ExecState, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = h.get()
		}(i)
	}
	wg.Wait()
	for _, g := range got {
		assert.Same(t, got[0], g)
	}
	assert.False(t, got[0].StartedAt.IsZero())
}

// ---- compiler.go / console.go / utils.go / tls.go ----

func TestCovMiscCompiler(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper.ts")
	entry := filepath.Join(dir, "entry.ts")
	require.NoError(t, os.WriteFile(helper, []byte(`export const double = (n: number): number => n * 2;`), 0o600))
	require.NoError(t, os.WriteFile(entry, []byte(`import { double } from "./helper"; export const answer: number = double(21);`), 0o600))

	js, err := CompileTypeScript(entry)
	require.NoError(t, err)
	rt, err := NewRuntime()
	require.NoError(t, err)
	_, err = rt.Evaluate(js)
	require.NoError(t, err)
	assert.Equal(t, int64(42), rt.Exports().Get("answer").ToInteger(), "bundled TS evaluates with imports resolved")

	_, err = CompileTypeScript(filepath.Join(dir, "missing.ts"))
	require.ErrorContains(t, err, "build failed")
	bad := filepath.Join(dir, "bad.ts")
	require.NoError(t, os.WriteFile(bad, []byte(`export const x = ;`), 0o600))
	_, err = CompileTypeScript(bad)
	require.ErrorContains(t, err, "build failed")

	fn, err := CompileFunction(`(function(a, b) { return a + b })`)
	require.NoError(t, err)
	res, err := fn(nil, rt.ToValue(2), rt.ToValue(3))
	require.NoError(t, err)
	assert.Equal(t, int64(5), res.ToInteger())
	_, err = CompileFunction(`42`)
	require.EqualError(t, err, "result is not a function")
	_, err = CompileFunction(`function (`)
	require.Error(t, err)

	prog, err := CompileProgram(`(x) => x + 1`)
	require.NoError(t, err)
	v, err := rt.VM().RunProgram(prog)
	require.NoError(t, err)
	f, ok := sobek.AssertFunction(v)
	require.True(t, ok)
	out, err := f(nil, rt.ToValue(1))
	require.NoError(t, err)
	assert.Equal(t, int64(2), out.ToInteger())
	_, err = CompileProgram(`) =>`)
	require.Error(t, err)
}

func TestCovMiscConsole_LogsByLevel(t *testing.T) {
	var buf bytes.Buffer
	prevLogger, prevLevel := log.Logger, zerolog.GlobalLevel()
	log.Logger = zerolog.New(&buf).Level(zerolog.InfoLevel)
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	t.Cleanup(func() {
		log.Logger = prevLogger
		zerolog.SetGlobalLevel(prevLevel)
	})

	rt, err := NewRuntime()
	require.NoError(t, err)
	_, err = rt.Evaluate(`console.log("hello", 1, true); console.warn("careful"); console.debug("hidden"); console.trace("hidden2"); console.info("fyi")`)
	require.NoError(t, err)
	out := buf.String()
	assert.Contains(t, out, `"message":"hello 1 true"`)
	assert.Contains(t, out, `"level":"warn","message":"careful"`)
	assert.Contains(t, out, `"message":"fyi"`)
	assert.NotContains(t, out, "hidden", "below logger level is dropped")
}

func TestCovMiscUtils(t *testing.T) {
	assert.Equal(t, []string{"a", "b", "c"}, RemoveDuplicates([]string{"a", "b", "a", "c", "b"}))
	assert.Equal(t, []string{}, RemoveDuplicates(nil))

	for _, in := range []string{`{}`, `[]`, `"s"`, `null`, `true`, `false`} {
		assert.True(t, IsSemiValidJson([]byte(in)), in)
	}
	for _, in := range []string{``, `<html>`, `1`} {
		assert.False(t, IsSemiValidJson([]byte(in)), in)
	}

	u, err := HexToUint64("0x10")
	require.NoError(t, err)
	assert.Equal(t, uint64(16), u)
	i, err := HexToInt64("0xff")
	require.NoError(t, err)
	assert.Equal(t, int64(255), i)
	b, err := HexToBytes("0x0102")
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 2}, b)
	h, err := NormalizeHex("0x00ff")
	require.NoError(t, err)
	assert.Equal(t, "0xff", h)
}

func TestCovMiscTLS_CreateConfig(t *testing.T) {
	certFile, keyFile := covMiscWriteCert(t)

	cfg, err := CreateTLSConfig(&TLSConfig{CertFile: certFile, KeyFile: keyFile, CAFile: certFile, InsecureSkipVerify: true})
	require.NoError(t, err)
	assert.True(t, cfg.InsecureSkipVerify)
	require.Len(t, cfg.Certificates, 1)
	require.NotNil(t, cfg.RootCAs)
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	require.NoError(t, err)
	assert.Equal(t, "cov-misc.local", leaf.Subject.CommonName)
	// RootCAs is built from CAFile: the self-signed cert verifies against it.
	_, err = leaf.Verify(x509.VerifyOptions{Roots: cfg.RootCAs, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	require.NoError(t, err)

	cfg, err = CreateTLSConfig(&TLSConfig{})
	require.NoError(t, err)
	assert.Empty(t, cfg.Certificates)
	assert.Nil(t, cfg.RootCAs)

	// cert without key: ignored (both required)
	cfg, err = CreateTLSConfig(&TLSConfig{CertFile: certFile})
	require.NoError(t, err)
	assert.Empty(t, cfg.Certificates)

	_, err = CreateTLSConfig(&TLSConfig{CertFile: certFile, KeyFile: certFile})
	require.ErrorContains(t, err, "failed to load client cert/key pair")
	_, err = CreateTLSConfig(&TLSConfig{CAFile: filepath.Join(t.TempDir(), "nope.pem")})
	require.ErrorContains(t, err, "failed to read CA file")
}
