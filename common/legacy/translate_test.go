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
				LegacyRouting: &common.LegacyUpstreamRouting{
					ScoreMultipliers: []*common.LegacyScoreMultiplier{
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
	require.NotEmpty(t, warns, "scoreMultipliers + routingStrategy should each emit one warning")

	// Synthesized eval landed on the network's selectionPolicy.
	require.NotNil(t, prj.Networks[0].SelectionPolicy,
		"translator must synthesize a selectionPolicy for the legacy-config network")
	eval := prj.Networks[0].SelectionPolicy.EvalFunc
	require.NotEmpty(t, eval)
	require.Contains(t, eval, "sortByScore",
		"score-based legacy → sortByScore in eval")
	require.Contains(t, eval, "hysteresis: 0.25",
		"project-level scoreSwitchHysteresis must flow into stickyPrimary")
	require.Contains(t, eval, "minSwitchInterval: '2m0s'",
		"project-level scoreMinSwitchInterval must flow into stickyPrimary")
	// Per-upstream scoreMultiplier shows up in the per-id weights map.
	// `overall` is intentionally not emitted (it's a uniform scale; sort
	// is invariant under monotonic scaling — see weightsFromMul).
	require.Contains(t, eval, `"primary":{"errorRate":4}`,
		"per-upstream scoreMultiplier must show up in the per-id weights map")

	// Stashes must be cleared so subsequent SetDefaults doesn't re-run them.
	require.Nil(t, prj.LegacyProject, "project legacy stash cleared post-translate")
	require.Nil(t, prj.Upstreams[0].LegacyRouting, "upstream legacy stash cleared")
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
	userEval := "(upstreams, ctx) => upstreams.sortByScore(PREFER_FASTER)"
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
	require.Contains(t, nwCfgs[0].SelectionPolicy.EvalFunc, "sortByScore(BALANCED)")
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
// 2x2 matrix tests: (legacy selectionPolicy.evalFunction yes/no) ×
// (upstream.routing.scoreMultipliers yes/no)
//
// Cell (NO eval × NO multipliers) is covered by
// TestTranslate_ScoreBased_DefaultEmitsBalanced above. The other three
// cells live here.
// ----------------------------------------------------------------------

// Cell (NO eval × YES multipliers): only score-multipliers, no legacy
// project-level routing strategy. The translator should still trigger
// (multipliers ARE a legacy field) and emit a per-id weights function
// passed to sortByScore.
func TestTranslate_ScoreMultipliers_OnlyMultipliers_EmitsWeightsFn(t *testing.T) {
	upCfgs := []*common.UpstreamConfig{
		{Id: "hot"},
		{Id: "cold"},
	}
	legacyUps := []legacy.WidenedUpstream{
		legacy.WidenedUpstreamForTest([]legacy.ScoreMultiplierSnapshot{{
			Network: "*", Method: "*",
			ErrorRate: 8, RespLatency: 12, Overall: 1,
		}}, 0),
		legacy.WidenedUpstreamForTest([]legacy.ScoreMultiplierSnapshot{{
			Network: "*", Method: "*",
			ErrorRate: 8, RespLatency: 4, Overall: 1,
		}}, 0),
	}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}}

	warns, err := legacy.Translate(legacy.WidenedProject{}, legacyUps, upCfgs, []legacy.WidenedNetwork{{}}, nwCfgs)
	require.NoError(t, err)

	// Should emit the multipliers warning.
	require.Truef(t, containsWarn(warns, "scoreMultipliers"),
		"expected multipliers deprecation warning; got %v", warns)

	require.NotNil(t, nwCfgs[0].SelectionPolicy)
	eval := nwCfgs[0].SelectionPolicy.EvalFunc
	require.Contains(t, eval, "__mulById", "weights map should be emitted")
	require.Contains(t, eval, `"hot"`, "hot upstream's weights must be in map")
	require.Contains(t, eval, `"cold"`, "cold upstream's weights must be in map")
	require.Contains(t, eval, "sortByScore(__weightsFn)", "function-form sortByScore")
	require.Contains(t, eval, "stickyPrimary", "sticky-primary should still be appended")
	// Per-upstream weight values land in the emitted JS literal.
	require.Contains(t, eval, `"respLatency":12`, "hot upstream gets respLatency=12")
	require.Contains(t, eval, `"respLatency":4`, "cold upstream gets respLatency=4")
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

// Cell (YES eval × YES multipliers): BOTH legacy fields set on the same
// project. The wrapper must pre-sort upstreams by the per-id weights
// (the legacy semantics passed the SCORED list to the eval fn) before
// invoking the legacy fn — otherwise the multiplier configuration is
// silently dropped at migration time.
func TestTranslate_ScoreMultipliers_PlusEvalFunction_PreSortsByWeights(t *testing.T) {
	legacyFn := "(upstreams, method) => upstreams.filter(u => u.metrics.errorRate < 0.5)"
	upCfgs := []*common.UpstreamConfig{
		{Id: "hot"},
		{Id: "cold"},
	}
	legacyUps := []legacy.WidenedUpstream{
		legacy.WidenedUpstreamForTest([]legacy.ScoreMultiplierSnapshot{{
			Network: "*", Method: "*",
			ErrorRate: 8, RespLatency: 12, Overall: 1,
		}}, 0),
		legacy.WidenedUpstreamForTest([]legacy.ScoreMultiplierSnapshot{{
			Network: "*", Method: "*",
			ErrorRate: 8, RespLatency: 4, Overall: 1,
		}}, 0),
	}
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
	require.Contains(t, eval, "__mulById",
		"multipliers MUST flow through into the wrapped eval — otherwise translation is lossy")
	require.Contains(t, eval, "__weightsFn", "weights fn must be emitted")
	require.Contains(t, eval, ".sortByScore(__weightsFn)",
		"pre-sort by weights so the legacy fn sees the same ordered list it saw in the old system")
	// And the legacy fn must be called against the pre-sorted list, not
	// the raw input. Match a tolerant pattern that survives whitespace.
	require.Regexp(t, `__legacyFn\(\s*__sorted\s*,\s*ctx\.method\s*\)`, eval,
		"legacy fn must receive the pre-sorted list, not raw upstreams")
}

// Cell (YES eval × YES multipliers + score-based project): same as above
// PLUS project-level routing strategy. The wrapper should still pre-sort
// and the legacy eval-function still wins (it's an explicit user filter).
func TestTranslate_ScoreMultipliers_PlusEvalFunctionAndScoreBased(t *testing.T) {
	upCfgs := []*common.UpstreamConfig{{Id: "hot"}, {Id: "cold"}}
	legacyUps := []legacy.WidenedUpstream{
		legacy.WidenedUpstreamForTest([]legacy.ScoreMultiplierSnapshot{
			{Network: "*", Method: "*", ErrorRate: 10, RespLatency: 4},
		}, 0),
		legacy.WidenedUpstreamForTest([]legacy.ScoreMultiplierSnapshot{
			{Network: "*", Method: "*", ErrorRate: 2, RespLatency: 8},
		}, 0),
	}
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
	require.GreaterOrEqual(t, len(warns), 2,
		"both routingStrategy and scoreMultipliers should warn; got %v", warns)

	eval := nwCfgs[0].SelectionPolicy.EvalFunc
	require.Contains(t, eval, "const __legacyFn =", "evalFunction takes precedence")
	require.Contains(t, eval, "__mulById", "multipliers must NOT be silently dropped")
}

// Per-upstream-multipliers indexing test: 3 upstreams, only the middle
// one has overrides. Verify the emitted map only includes that one id
// and the default weights apply to the other two via fallback.
func TestTranslate_ScoreMultipliers_PerUpstreamIndexing(t *testing.T) {
	upCfgs := []*common.UpstreamConfig{
		{Id: "rpc1"},
		{Id: "rpc2"},
		{Id: "rpc3"},
	}
	legacyUps := []legacy.WidenedUpstream{
		{},
		legacy.WidenedUpstreamForTest([]legacy.ScoreMultiplierSnapshot{
			{Network: "*", Method: "*", ErrorRate: 13, RespLatency: 7},
		}, 0),
		{},
	}
	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 1},
	}}
	_, err := legacy.Translate(legacy.WidenedProject{}, legacyUps, upCfgs, []legacy.WidenedNetwork{{}}, nwCfgs)
	require.NoError(t, err)

	eval := nwCfgs[0].SelectionPolicy.EvalFunc
	require.Contains(t, eval, `"rpc2"`, "rpc2 (only one with overrides) MUST be in the map")
	require.NotContains(t, eval, `"rpc1":{`, "rpc1 has no overrides; must NOT be a map key")
	require.NotContains(t, eval, `"rpc3":{`, "rpc3 has no overrides; must NOT be a map key")
	require.Contains(t, eval, `"errorRate":13`, "rpc2's errorRate weight is emitted")
	require.Contains(t, eval, `"respLatency":7`, "rpc2's respLatency weight is emitted")
	require.Contains(t, eval, "__mulDefault", "fallback default weights map must be defined")
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
	require.Contains(t, eval, "reAdmitAfter: '7m0s'", "reAdmitAfter must use the legacy interval")
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
	require.Contains(t, eval, "reAdmitAfter: '3m0s'", "reAdmitAfter from legacy interval")
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
	require.True(t, nwCfgs[0].SelectionPolicy.EvalPerMethod,
		"legacy evalPerMethod must carry over to new-shape EvalPerMethod")
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
