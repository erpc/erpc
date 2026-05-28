package legacy_test

import (
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/common/legacy"
	"github.com/stretchr/testify/require"
)

// TestTranslateFromConfig_EndToEnd exercises the LoadConfig hook path:
// a Config decoded from YAML — with `routing.scoreMultipliers` blocks
// on the upstream + project-level `scoreSwitchHysteresis` /
// `scoreMinSwitchInterval` — is migrated into a synthesized
// `selectionPolicy.eval` on each network without the caller doing
// any manual setup. After the hook runs, the legacy stashes are
// cleared so SetDefaults / Validate see a clean canonical config.
func TestTranslateFromConfig_EndToEnd(t *testing.T) {
	multiplier := func(v float64) *float64 { return &v }

	prj := &common.ProjectConfig{
		Id: "p1",
		LegacyProject: &common.LegacyProjectFields{
			RoutingStrategy:        "score-based",
			ScoreSwitchHysteresis:  0.25,
			ScoreMinSwitchInterval: common.Duration(2 * time.Minute),
		},
		Upstreams: []*common.UpstreamConfig{
			{
				Id:       "primary",
				Type:     common.UpstreamTypeEvm,
				Endpoint: "https://primary.example/",
				Evm:      &common.EvmUpstreamConfig{ChainId: 1},
				Routing: &common.UpstreamRoutingConfig{
					ScoreMultipliers: []*common.ScoreMultiplierConfig{
						{Overall: multiplier(0.5), ErrorRate: multiplier(4)},
					},
				},
			},
			{
				Id:       "fallback",
				Type:     common.UpstreamTypeEvm,
				Endpoint: "https://fallback.example/",
				Evm:      &common.EvmUpstreamConfig{ChainId: 1},
			},
		},
		Networks: []*common.NetworkConfig{
			{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}},
		},
	}
	cfg := &common.Config{Projects: []*common.ProjectConfig{prj}}

	warns, err := legacy.TranslateFromConfig(cfg)
	require.NoError(t, err)
	require.NotEmpty(t, warns, "routingStrategy should emit a deprecation warning")

	// Synthesized eval landed on the network's selectionPolicy (because
	// routingStrategy / scoreSwitchHysteresis are still legacy triggers).
	require.NotNil(t, prj.Networks[0].SelectionPolicy,
		"translator must synthesize a selectionPolicy for the legacy-config network")
	eval := prj.Networks[0].SelectionPolicy.EvalFunc
	require.NotEmpty(t, eval)
	require.Contains(t, eval, "sortByScore(PREFER_FASTEST)",
		"score-based legacy → plain sortByScore(PREFER_FASTEST) in eval")
	require.Contains(t, eval, "hysteresis: 0.25",
		"project-level scoreSwitchHysteresis must flow into stickyPrimary")
	require.Contains(t, eval, "minSwitchInterval: '2m0s'",
		"project-level scoreMinSwitchInterval must flow into stickyPrimary")
	// scoreMultipliers are first-class now: they flow into the eval at
	// runtime via `u.scoreMultipliers`, NOT as a baked-in per-id weights
	// map. The synthesized source must therefore carry no such map.
	require.NotContains(t, eval, "__mulById",
		"per-upstream weights map must NOT be emitted (multipliers flow at runtime)")

	// Project-level legacy stashes are cleared; the first-class
	// routing.scoreMultipliers must SURVIVE (it's not a stash).
	require.Nil(t, prj.LegacyProject, "project legacy stash cleared post-translate")
	require.NotNil(t, prj.Upstreams[0].Routing, "first-class routing must survive translate")
	require.Len(t, prj.Upstreams[0].Routing.ScoreMultipliers, 1,
		"routing.scoreMultipliers preserved for runtime")
	require.Nil(t, prj.Networks[0].SelectionPolicy.LegacySelectionPolicy,
		"selectionPolicy legacy stash cleared")
}

// TestTranslateFromConfig_NoLegacy is the fast-path: a config with
// zero legacy fields must produce zero warnings, leave SelectionPolicy
// nil (so SetDefaults installs the canonical default policy), and
// otherwise be a no-op.
func TestTranslateFromConfig_NoLegacy(t *testing.T) {
	prj := &common.ProjectConfig{
		Id: "p1",
		Upstreams: []*common.UpstreamConfig{
			{Id: "u1", Type: common.UpstreamTypeEvm, Endpoint: "https://u1.example/",
				Evm: &common.EvmUpstreamConfig{ChainId: 1}},
		},
		Networks: []*common.NetworkConfig{
			{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}},
		},
	}
	cfg := &common.Config{Projects: []*common.ProjectConfig{prj}}
	warns, err := legacy.TranslateFromConfig(cfg)
	require.NoError(t, err)
	require.Empty(t, warns)
	require.Nil(t, prj.Networks[0].SelectionPolicy,
		"no legacy fields → translator must not synthesize a SelectionPolicy")
}

// TestTranslateFromConfig_PreservesUserEval: if the user has both
// modern `eval` AND legacy `evalFunction`, the modern one wins
// untouched. Critical: prevents the translator from clobbering a
// hand-written policy when someone forgets to delete the legacy block.
func TestTranslateFromConfig_PreservesUserEval(t *testing.T) {
	userEval := "(upstreams, ctx) => upstreams.sortByScore(PREFER_FASTEST)"
	prj := &common.ProjectConfig{
		Id: "p1",
		Networks: []*common.NetworkConfig{
			{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 1},
				SelectionPolicy: &common.SelectionPolicyConfig{
					EvalFunc: userEval,
					LegacySelectionPolicy: &common.LegacySelectionPolicyFields{
						EvalFunction: "(upstreams, method) => upstreams",
					},
				},
			},
		},
	}
	cfg := &common.Config{Projects: []*common.ProjectConfig{prj}}
	_, err := legacy.TranslateFromConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, userEval, prj.Networks[0].SelectionPolicy.EvalFunc,
		"existing modern eval must not be overwritten by legacy evalFunction")
}

func TestTranslate_RoundRobin_EmitsRotateBy(t *testing.T) {
	prj := legacy.WidenedProject{RoutingStrategy: "round-robin"}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}}
	warns, err := legacy.Translate(prj, nil, nil, []legacy.WidenedNetwork{{}}, nwCfgs)
	require.NoError(t, err)
	require.Len(t, warns, 1)
	require.Contains(t, warns[0], "routingStrategy")
	require.NotNil(t, nwCfgs[0].SelectionPolicy)
	require.Contains(t, nwCfgs[0].SelectionPolicy.EvalFunc, "rotateBy(ctx.tickCount)")
}

func TestTranslate_ScoreBased_DefaultEmitsBalanced(t *testing.T) {
	prj := legacy.WidenedProject{RoutingStrategy: "score-based"}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}}
	warns, err := legacy.Translate(prj, nil, nil, []legacy.WidenedNetwork{{}}, nwCfgs)
	require.NoError(t, err)
	require.Len(t, warns, 1, "routingStrategy warning")
	require.Contains(t, nwCfgs[0].SelectionPolicy.EvalFunc, "sortByScore(PREFER_FASTEST)")
	require.Contains(t, nwCfgs[0].SelectionPolicy.EvalFunc, "stickyPrimary")
}

func TestTranslate_NoLegacy_NoOp(t *testing.T) {
	prj := legacy.WidenedProject{}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}}
	warns, err := legacy.Translate(prj, nil, nil, []legacy.WidenedNetwork{{}}, nwCfgs)
	require.NoError(t, err)
	require.Empty(t, warns)
	require.Nil(t, nwCfgs[0].SelectionPolicy, "no legacy → no synthesized selectionPolicy")
}

// TestTranslate_InertLegacyOnly: a config that only carries deprecated
// fields with NO behavioral mapping (scoreMetricsMode,
// scoreGranularity, scorePenaltyDecayRate, scoreRefreshInterval) must
// emit warnings and leave SelectionPolicy alone — so SetDefaults
// installs the canonical default policy.
//
// Regression for: legacy translator firing on any inert field caused
// stray deprecated knobs to override the rich default policy with a
// minimal sortByScore-only synth.
func TestTranslate_InertLegacyOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prj     legacy.WidenedProject
		expects string // substring that must appear in warnings
	}{
		{
			"scoreMetricsMode alone",
			legacy.WidenedProject{ScoreMetricsMode: "compact"},
			"scoreMetricsMode",
		},
		{
			"scoreGranularity alone",
			legacy.WidenedProject{ScoreGranularity: "method"},
			"scoreGranularity",
		},
		{
			"scorePenaltyDecayRate alone",
			legacy.WidenedProject{ScorePenaltyDecayRate: 0.95},
			"scorePenaltyDecayRate",
		},
		{
			"scoreRefreshInterval alone",
			legacy.WidenedProject{ScoreRefreshInterval: common.Duration(30 * time.Second)},
			"scoreRefreshInterval",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nwCfgs := []*common.NetworkConfig{{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123},
			}}
			warns, err := legacy.Translate(tc.prj, nil, nil, []legacy.WidenedNetwork{{}}, nwCfgs)
			require.NoError(t, err)
			require.NotEmpty(t, warns, "inert legacy field must emit a warning")
			require.True(t, strings.Contains(strings.Join(warns, "|"), tc.expects),
				"warning must mention %q; got %v", tc.expects, warns)
			require.Nil(t, nwCfgs[0].SelectionPolicy,
				"inert legacy must NOT synthesize a selectionPolicy; the default policy must install via SetDefaults")
		})
	}
}

// TestTranslate_SemanticPlusInertLegacy: when BOTH inert + semantic
// legacy fields are present, semantic wins — synthesizer fires, AND
// inert warnings still flow through. This is the typical "operator
// removed scoreMultipliers but left a stray inert knob" case.
func TestTranslate_SemanticPlusInertLegacy(t *testing.T) {
	prj := legacy.WidenedProject{
		RoutingStrategy:  "round-robin", // semantic
		ScoreMetricsMode: "compact",     // inert
	}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}}
	warns, err := legacy.Translate(prj, nil, nil, []legacy.WidenedNetwork{{}}, nwCfgs)
	require.NoError(t, err)
	joined := strings.Join(warns, "|")
	require.Contains(t, joined, "routingStrategy", "semantic warning must fire")
	require.Contains(t, joined, "scoreMetricsMode", "inert warning must fire too")
	require.NotNil(t, nwCfgs[0].SelectionPolicy,
		"semantic field present → synth runs")
	require.Contains(t, nwCfgs[0].SelectionPolicy.EvalFunc, "rotateBy",
		"routing-strategy round-robin synthesizes rotateBy")
}

func TestTranslate_PreservesExistingEval(t *testing.T) {
	prj := legacy.WidenedProject{RoutingStrategy: "score-based"}
	user := "(upstreams, ctx) => upstreams.byTag('tier:hot')"
	nwCfgs := []*common.NetworkConfig{{
		Architecture:    common.ArchitectureEvm,
		Evm:             &common.EvmNetworkConfig{ChainId: 123},
		SelectionPolicy: &common.SelectionPolicyConfig{EvalFunc: user},
	}}
	_, err := legacy.Translate(prj, nil, nil, []legacy.WidenedNetwork{{}}, nwCfgs)
	require.NoError(t, err)
	require.Equal(t, user, nwCfgs[0].SelectionPolicy.EvalFunc,
		"existing new-shape eval must not be overwritten")
}

func TestTranslate_LegacyEvalFunction_Wrapped(t *testing.T) {
	legacyFn := "(upstreams, method) => upstreams.filter(u => u.metrics.errorRate < 0.5)"
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}}
	nws := []legacy.WidenedNetwork{{
		SelectionPolicy: legacy.WidenedSelectionPolicyForTest(legacy.LegacyPolicySnapshot{
			EvalFunction: legacyFn,
		}),
	}}
	_, err := legacy.Translate(legacy.WidenedProject{}, nil, nil, nws, nwCfgs)
	require.NoError(t, err)
	require.NotNil(t, nwCfgs[0].SelectionPolicy)
	require.True(t,
		strings.Contains(nwCfgs[0].SelectionPolicy.EvalFunc, "const __legacyFn ="),
		"synthesized eval should bind the legacy fn under __legacyFn",
	)
}

func TestTranslate_StickyPrimaryHonorsLegacyHysteresis(t *testing.T) {
	prj := legacy.WidenedProject{
		RoutingStrategy:        "score-based",
		ScoreSwitchHysteresis:  0.25,
		ScoreMinSwitchInterval: common.Duration(2 * time.Minute),
	}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}}
	_, err := legacy.Translate(prj, nil, nil, []legacy.WidenedNetwork{{}}, nwCfgs)
	require.NoError(t, err)
	require.Contains(t, nwCfgs[0].SelectionPolicy.EvalFunc, "hysteresis: 0.25")
	require.Contains(t, nwCfgs[0].SelectionPolicy.EvalFunc, "minSwitchInterval: '2m0s'")
}

// ----------------------------------------------------------------------
// Matrix tests for the remaining LEGACY triggers (legacy
// selectionPolicy.evalFunction yes/no × project routingStrategy). Note
// `routing.scoreMultipliers` is NO LONGER a translator concern — it's
// first-class config attached to upstreams at runtime, so it never
// triggers synthesis and never appears as a baked-in weights map.
// ----------------------------------------------------------------------

// scoreMultipliers-only (no legacy evalFunction / routingStrategy /
// scoreLatencyQuantile) must NOT synthesize an eval: the network keeps a
// nil selectionPolicy so SetDefaults installs the canonical default, and
// the per-upstream multipliers flow through it at runtime. No deprecation
// warning either — scoreMultipliers is a supported, current feature.
func TestTranslate_ScoreMultipliers_OnlyMultipliers_NoSynthesis(t *testing.T) {
	mul := func(v float64) *float64 { return &v }
	upCfgs := []*common.UpstreamConfig{
		{Id: "hot", Routing: &common.UpstreamRoutingConfig{
			ScoreMultipliers: []*common.ScoreMultiplierConfig{
				{Network: "*", Method: "*", ErrorRate: mul(8), RespLatency: mul(12), Overall: mul(2)},
			},
		}},
		{Id: "cold"},
	}
	// The widened view carries nothing the translator triggers on.
	legacyUps := []legacy.WidenedUpstream{{}, {}}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}}

	warns, err := legacy.Translate(legacy.WidenedProject{}, legacyUps, upCfgs, []legacy.WidenedNetwork{{}}, nwCfgs)
	require.NoError(t, err)

	require.Nil(t, nwCfgs[0].SelectionPolicy,
		"scoreMultipliers-only must NOT synthesize an eval (rich default applies)")
	require.False(t, containsWarn(warns, "scoreMultipliers"),
		"scoreMultipliers is first-class — it must not emit a deprecation warning")
}

// Cell (YES eval × NO multipliers): the legacy evalFunction is wrapped
// untouched. No __mulById map should be emitted because no upstream had
// multipliers.
func TestTranslate_ScoreMultipliers_EvalFunctionOnly_NoWeightsMap(t *testing.T) {
	legacyFn := "(upstreams, method) => upstreams.filter(u => u.metrics.errorRate < 0.5)"
	upCfgs := []*common.UpstreamConfig{{Id: "a"}, {Id: "b"}}
	legacyUps := []legacy.WidenedUpstream{{}, {}} // no multipliers
	nws := []legacy.WidenedNetwork{{
		SelectionPolicy: legacy.WidenedSelectionPolicyForTest(legacy.LegacyPolicySnapshot{
			EvalFunction: legacyFn,
		}),
	}}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 1},
	}}
	_, err := legacy.Translate(legacy.WidenedProject{}, legacyUps, upCfgs, nws, nwCfgs)
	require.NoError(t, err)

	eval := nwCfgs[0].SelectionPolicy.EvalFunc
	require.Contains(t, eval, "const __legacyFn =", "legacy fn must be wrapped")
	require.NotContains(t, eval, "__mulById", "no multipliers → no weights map")
	require.NotContains(t, eval, "__weightsFn", "no multipliers → no weights fn")
}

// YES eval × YES multipliers: a legacy evalFunction AND per-upstream
// scoreMultipliers. The wrapper pre-sorts with sortByScore(PREFER_FASTEST)
// so the legacy fn sees a SCORED list (matching old semantics); the
// per-upstream multipliers merge into that sort at runtime via
// `u.scoreMultipliers`, so NO static weights map is baked into the source.
func TestTranslate_ScoreMultipliers_PlusEvalFunction_PreSorts(t *testing.T) {
	mul := func(v float64) *float64 { return &v }
	legacyFn := "(upstreams, method) => upstreams.filter(u => u.metrics.errorRate < 0.5)"
	upCfgs := []*common.UpstreamConfig{
		{Id: "hot", Routing: &common.UpstreamRoutingConfig{
			ScoreMultipliers: []*common.ScoreMultiplierConfig{{ErrorRate: mul(8), RespLatency: mul(12)}},
		}},
		{Id: "cold"},
	}
	legacyUps := []legacy.WidenedUpstream{{}, {}}
	nws := []legacy.WidenedNetwork{{
		SelectionPolicy: legacy.WidenedSelectionPolicyForTest(legacy.LegacyPolicySnapshot{
			EvalFunction: legacyFn,
		}),
	}}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 1},
	}}

	_, err := legacy.Translate(legacy.WidenedProject{}, legacyUps, upCfgs, nws, nwCfgs)
	require.NoError(t, err)

	eval := nwCfgs[0].SelectionPolicy.EvalFunc
	require.Contains(t, eval, "const __legacyFn =", "legacy fn must still be wrapped")
	require.Contains(t, eval, ".sortByScore(PREFER_FASTEST)",
		"pre-sort so the legacy fn sees a scored list; per-upstream multipliers merge in at runtime")
	require.NotContains(t, eval, "__mulById",
		"multipliers flow at runtime via u.scoreMultipliers — no baked-in weights map")
	// The legacy fn must be called against the pre-sorted list, not the
	// raw input. Match a tolerant pattern that survives whitespace.
	require.Regexp(t, `__legacyFn\(\s*__sorted\s*,\s*ctx\.method\s*\)`, eval,
		"legacy fn must receive the pre-sorted list, not raw upstreams")
}

// YES eval × score-based project: legacy evalFunction PLUS project-level
// routingStrategy. The legacy eval-function still wins (explicit user
// filter); routingStrategy warns but scoreMultipliers (first-class) does not.
func TestTranslate_ScoreMultipliers_PlusEvalFunctionAndScoreBased(t *testing.T) {
	mul := func(v float64) *float64 { return &v }
	upCfgs := []*common.UpstreamConfig{
		{Id: "hot", Routing: &common.UpstreamRoutingConfig{
			ScoreMultipliers: []*common.ScoreMultiplierConfig{{ErrorRate: mul(10), RespLatency: mul(4)}},
		}},
		{Id: "cold"},
	}
	legacyUps := []legacy.WidenedUpstream{{}, {}}
	nws := []legacy.WidenedNetwork{{
		SelectionPolicy: legacy.WidenedSelectionPolicyForTest(legacy.LegacyPolicySnapshot{
			EvalFunction: "(upstreams, method) => upstreams",
		}),
	}}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 1},
	}}
	prj := legacy.WidenedProject{
		RoutingStrategy:        "score-based",
		ScoreSwitchHysteresis:  0.2,
		ScoreMinSwitchInterval: common.Duration(time.Minute),
	}

	warns, err := legacy.Translate(prj, legacyUps, upCfgs, nws, nwCfgs)
	require.NoError(t, err)
	require.True(t, containsWarn(warns, "routingStrategy"),
		"routingStrategy should warn; got %v", warns)
	require.False(t, containsWarn(warns, "scoreMultipliers"),
		"scoreMultipliers is first-class — no deprecation warning")

	eval := nwCfgs[0].SelectionPolicy.EvalFunc
	require.Contains(t, eval, "const __legacyFn =", "evalFunction takes precedence")
	require.NotContains(t, eval, "__mulById", "no baked-in weights map")
}

// scoreLatencyQuantile is still a translator trigger and folds into the
// synthesized sortByScore as a `latencyQuantile` option (0.95 → 'p95').
func TestTranslate_ScoreLatencyQuantile_EmitsOpt(t *testing.T) {
	upCfgs := []*common.UpstreamConfig{{Id: "a"}, {Id: "b"}}
	legacyUps := []legacy.WidenedUpstream{
		legacy.WidenedUpstreamForTest(nil, 0.95), // scoreLatencyQuantile=0.95
		{},
	}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 1},
	}}
	_, err := legacy.Translate(legacy.WidenedProject{}, legacyUps, upCfgs, []legacy.WidenedNetwork{{}}, nwCfgs)
	require.NoError(t, err)
	require.NotNil(t, nwCfgs[0].SelectionPolicy, "scoreLatencyQuantile must trigger synthesis")
	require.Contains(t, nwCfgs[0].SelectionPolicy.EvalFunc,
		"sortByScore(PREFER_FASTEST, { latencyQuantile: 'p95' })",
		"0.95 quantile must fold into the sortByScore latencyQuantile opt")
}

// resampleExcluded + score-based: probeExcluded must be appended to the
// chain with the user's interval honored.
func TestTranslate_ScoreBased_WithResampleExcluded_AppendsProbe(t *testing.T) {
	prj := legacy.WidenedProject{RoutingStrategy: "score-based"}
	nws := []legacy.WidenedNetwork{{
		SelectionPolicy: legacy.WidenedSelectionPolicyForTest(legacy.LegacyPolicySnapshot{
			ResampleExcluded: true,
			ResampleInterval: common.Duration(7 * time.Minute),
		}),
	}}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 1},
	}}
	_, err := legacy.Translate(prj, nil, nil, nws, nwCfgs)
	require.NoError(t, err)
	eval := nwCfgs[0].SelectionPolicy.EvalFunc
	require.Contains(t, eval, ".probeExcluded(", "probeExcluded must be appended")
	require.Contains(t, eval, "sampleRate: 1.0", "translator emits the new sample-driven probeExcluded shape")
}

// resampleExcluded + evalFunction: probeExcluded is appended after the
// wrapped legacy fn so its re-admission semantics still apply.
func TestTranslate_EvalFunction_WithResampleExcluded_AppendsProbe(t *testing.T) {
	legacyFn := "(upstreams, method) => upstreams"
	nws := []legacy.WidenedNetwork{{
		SelectionPolicy: legacy.WidenedSelectionPolicyForTest(legacy.LegacyPolicySnapshot{
			EvalFunction:     legacyFn,
			ResampleExcluded: true,
			ResampleInterval: common.Duration(3 * time.Minute),
		}),
	}}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 1},
	}}
	_, err := legacy.Translate(legacy.WidenedProject{}, nil, nil, nws, nwCfgs)
	require.NoError(t, err)
	eval := nwCfgs[0].SelectionPolicy.EvalFunc
	require.Contains(t, eval, "const __legacyFn =", "legacy fn wrapped")
	require.Contains(t, eval, ".probeExcluded(", "probeExcluded must be appended")
	require.Contains(t, eval, "sampleRate: 1.0", "translator emits the new sample-driven probeExcluded shape")
}

// Legacy selectionPolicy.evalInterval must flow into the new shape.
func TestTranslate_LegacyEvalInterval_Preserved(t *testing.T) {
	nws := []legacy.WidenedNetwork{{
		SelectionPolicy: legacy.WidenedSelectionPolicyForTest(legacy.LegacyPolicySnapshot{
			EvalFunction: "(upstreams, method) => upstreams",
			EvalInterval: common.Duration(45 * time.Second),
		}),
	}}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 1},
	}}
	_, err := legacy.Translate(legacy.WidenedProject{}, nil, nil, nws, nwCfgs)
	require.NoError(t, err)
	require.Equal(t, common.Duration(45*time.Second), nwCfgs[0].SelectionPolicy.EvalInterval,
		"legacy evalInterval must carry over to new-shape EvalInterval")
}

// Legacy selectionPolicy.evalPerMethod must flow into the new shape.
func TestTranslate_LegacyEvalPerMethod_Preserved(t *testing.T) {
	nws := []legacy.WidenedNetwork{{
		SelectionPolicy: legacy.WidenedSelectionPolicyForTest(legacy.LegacyPolicySnapshot{
			EvalFunction:  "(upstreams, method) => upstreams",
			EvalPerMethod: true,
		}),
	}}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 1},
	}}
	_, err := legacy.Translate(legacy.WidenedProject{}, nil, nil, nws, nwCfgs)
	require.NoError(t, err)
	// The legacy `evalPerMethod: true` bool now promotes to the
	// modern `EvalScope: network-method` enum. SetDefaults will later
	// nil-out the deprecated pointer; here we're observing the
	// translator's output BEFORE SetDefaults runs, so the enum is the
	// source of truth.
	require.Equal(t, common.EvalScopeNetworkMethod, nwCfgs[0].SelectionPolicy.EvalScope,
		"legacy evalPerMethod must translate into EvalScope=network-method")
}

// scoreMetricsMode is removed; the translator must surface a warning.
func TestTranslate_ScoreMetricsMode_EmitsWarning(t *testing.T) {
	prj := legacy.WidenedProject{
		RoutingStrategy:  "score-based",
		ScoreMetricsMode: "detailed",
	}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 1},
	}}
	warns, err := legacy.Translate(prj, nil, nil, []legacy.WidenedNetwork{{}}, nwCfgs)
	require.NoError(t, err)
	require.Truef(t, containsWarn(warns, "scoreMetricsMode"),
		"expected scoreMetricsMode deprecation warning; got %v", warns)
}

// Translator must run per-network for a multi-network project, applying
// the same synthesized eval to each.
func TestTranslate_MultiNetwork_AppliesPerNetwork(t *testing.T) {
	prj := legacy.WidenedProject{RoutingStrategy: "round-robin"}
	nwCfgs := []*common.NetworkConfig{
		{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}},
		{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 10}},
		{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 137}},
	}
	nws := []legacy.WidenedNetwork{{}, {}, {}}
	_, err := legacy.Translate(prj, nil, nil, nws, nwCfgs)
	require.NoError(t, err)
	for i, cfg := range nwCfgs {
		require.NotNilf(t, cfg.SelectionPolicy, "network[%d] should have a synthesized selection policy", i)
		require.Containsf(t, cfg.SelectionPolicy.EvalFunc, "rotateBy(ctx.tickCount)",
			"network[%d] should have round-robin eval", i)
	}
}

// helpers

func containsWarn(warnings []string, needle string) bool {
	for _, w := range warnings {
		if strings.Contains(w, needle) {
			return true
		}
	}
	return false
}
