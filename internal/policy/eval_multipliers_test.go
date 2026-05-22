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
