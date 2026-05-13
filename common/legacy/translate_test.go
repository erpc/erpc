package legacy_test

import (
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/common/legacy"
	"github.com/stretchr/testify/require"
)

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
	require.Contains(t, nwCfgs[0].SelectionPolicy.Eval, "rotateBy(ctx.tickCount)")
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
	require.Contains(t, nwCfgs[0].SelectionPolicy.Eval, "sortByScore(BALANCED)")
	require.Contains(t, nwCfgs[0].SelectionPolicy.Eval, "stickyPrimary")
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
	user := "(upstreams, ctx) => upstreams.byGroup('hot')"
	nwCfgs := []*common.NetworkConfig{{
		Architecture:    common.ArchitectureEvm,
		Evm:             &common.EvmNetworkConfig{ChainId: 123},
		SelectionPolicy: &common.SelectionPolicyConfig{Eval: user},
	}}
	_, err := legacy.Translate(prj, nil, nil, []legacy.WidenedNetwork{{}}, nwCfgs)
	require.NoError(t, err)
	require.Equal(t, user, nwCfgs[0].SelectionPolicy.Eval,
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
		strings.Contains(nwCfgs[0].SelectionPolicy.Eval, "const __legacyFn ="),
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
	require.Contains(t, nwCfgs[0].SelectionPolicy.Eval, "hysteresis: 0.25")
	require.Contains(t, nwCfgs[0].SelectionPolicy.Eval, "minSwitchInterval: '2m0s'")
}
