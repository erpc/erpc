package policy

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// TestResolveScoreMultipliers pins the per-(network, method, finality)
// resolution that attaches `u.scoreMultipliers` at runtime: matcher
// semantics (glob network/method, finality membership, empty == any),
// first-match-wins, and the JS-side key mapping.
//
// `upstreamDefaults.routing` inheritance is handled at config-load time
// (ApplyDefaults), so the resolver itself is single-source — see
// TestApplyDefaults_InheritsRouting for the load-time wiring.
func TestResolveScoreMultipliers(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	routing := func(ms ...*common.ScoreMultiplierConfig) *common.UpstreamRoutingConfig {
		return &common.UpstreamRoutingConfig{ScoreMultipliers: ms}
	}

	t.Run("nil routing", func(t *testing.T) {
		require.Nil(t, resolveScoreMultipliers(nil, "evm:1", "eth_call", "realtime"))
	})

	t.Run("no entries", func(t *testing.T) {
		require.Nil(t, resolveScoreMultipliers(&common.UpstreamRoutingConfig{}, "evm:1", "eth_call", "realtime"))
	})

	t.Run("match-all maps weights + overall", func(t *testing.T) {
		got := resolveScoreMultipliers(routing(&common.ScoreMultiplierConfig{
			Overall: f(2), ErrorRate: f(8), RespLatency: f(15),
		}), "evm:1", "eth_call", "realtime")
		require.Equal(t, map[string]float64{"overall": 2, "errorRate": 8, "respLatency": 15}, got)
	})

	t.Run("network glob", func(t *testing.T) {
		r := routing(&common.ScoreMultiplierConfig{Network: "evm:1", Overall: f(2)})
		require.NotNil(t, resolveScoreMultipliers(r, "evm:1", "*", "unknown"))
		require.Nil(t, resolveScoreMultipliers(r, "evm:10", "*", "unknown"))
	})

	t.Run("method match needs a per-method slot", func(t *testing.T) {
		r := routing(&common.ScoreMultiplierConfig{Method: "eth_getLogs", Overall: f(2)})
		require.NotNil(t, resolveScoreMultipliers(r, "evm:1", "eth_getLogs", "unknown"))
		require.Nil(t, resolveScoreMultipliers(r, "evm:1", "eth_call", "unknown"))
		require.Nil(t, resolveScoreMultipliers(r, "evm:1", "*", "unknown"),
			"wildcard slot (evalPerMethod=false): a specific-method entry must not match")
	})

	t.Run("finality membership", func(t *testing.T) {
		r := routing(&common.ScoreMultiplierConfig{
			Finality: []common.DataFinalityState{common.DataFinalityStateRealtime},
			Overall:  f(2),
		})
		require.NotNil(t, resolveScoreMultipliers(r, "evm:1", "*", "realtime"))
		require.Nil(t, resolveScoreMultipliers(r, "evm:1", "*", "finalized"))
	})

	t.Run("first match wins", func(t *testing.T) {
		got := resolveScoreMultipliers(routing(
			&common.ScoreMultiplierConfig{Method: "eth_getLogs", RespLatency: f(25)},
			&common.ScoreMultiplierConfig{Method: "*", Overall: f(9)},
		), "evm:1", "eth_call", "unknown")
		require.Equal(t, map[string]float64{"overall": 9}, got,
			"specific-method entry skipped; the catch-all wins")
	})

	t.Run("matching but weightless entry yields nil", func(t *testing.T) {
		require.Nil(t, resolveScoreMultipliers(
			routing(&common.ScoreMultiplierConfig{Network: "*"}), "evm:1", "*", "unknown"))
	})

	t.Run("explicit zero weight is kept (distinct from unset)", func(t *testing.T) {
		got := resolveScoreMultipliers(routing(&common.ScoreMultiplierConfig{
			RespLatency: f(0), Overall: f(1),
		}), "evm:1", "*", "unknown")
		require.Equal(t, map[string]float64{"respLatency": 0, "overall": 1}, got)
	})
}

// TestApplyDefaults_InheritsRouting pins the all-or-nothing inheritance of
// `upstreamDefaults.routing` at config-load time. An upstream that omits its
// own `routing` block inherits the project's defaults fully (cloned, not
// pointer-shared); one with its own keeps it unchanged.
func TestApplyDefaults_InheritsRouting(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	defaultsRouting := &common.UpstreamRoutingConfig{
		ScoreMultipliers: []*common.ScoreMultiplierConfig{
			{Finality: []common.DataFinalityState{common.DataFinalityStateRealtime}, RespLatency: f(10)},
		},
	}
	defaults := &common.UpstreamConfig{Routing: defaultsRouting}

	t.Run("upstream without own routing inherits a clone", func(t *testing.T) {
		u := &common.UpstreamConfig{Id: "u1"}
		require.NoError(t, u.ApplyDefaults(defaults))
		require.NotNil(t, u.Routing, "u.Routing must be populated from defaults")
		require.Len(t, u.Routing.ScoreMultipliers, 1)
		require.NotSame(t, defaultsRouting, u.Routing,
			"ApplyDefaults must clone the routing struct, not share the pointer")
		require.Equal(t, defaultsRouting.ScoreMultipliers[0], u.Routing.ScoreMultipliers[0],
			"entry pointers may be reused (they're immutable post-load)")
	})

	t.Run("upstream with own routing keeps it unchanged", func(t *testing.T) {
		ownRouting := &common.UpstreamRoutingConfig{
			ScoreMultipliers: []*common.ScoreMultiplierConfig{{Overall: f(3.5)}},
		}
		u := &common.UpstreamConfig{Id: "u2", Routing: ownRouting}
		require.NoError(t, u.ApplyDefaults(defaults))
		require.Same(t, ownRouting, u.Routing, "all-or-nothing: own routing wins entirely")
	})

	t.Run("no defaults.Routing: u.Routing stays nil", func(t *testing.T) {
		u := &common.UpstreamConfig{Id: "u3"}
		require.NoError(t, u.ApplyDefaults(&common.UpstreamConfig{}))
		require.Nil(t, u.Routing)
	})

	t.Run("defaults applied to themselves is a no-op for Routing", func(t *testing.T) {
		// ApplyDefaults is also called on `prj.UpstreamDefaults` itself
		// at config-load time; the self-reference guard prevents the
		// routing block from being cloned onto itself.
		before := defaults.Routing
		require.NoError(t, defaults.ApplyDefaults(defaults))
		require.Same(t, before, defaults.Routing)
	})
}
