package policy

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

func boolPtr(b bool) *bool { return &b }

// TestBoundaryKey_CanonicalAndStable — the lane key must depend only on
// WHICH upstreams are eligible, independent of their order, and must not
// mutate the caller's slice. Empty/nil collapses to the full-pool wildcard.
func TestBoundaryKey_CanonicalAndStable(t *testing.T) {
	require.Equal(t, "*", boundaryKey(nil))
	require.Equal(t, "*", boundaryKey([]string{}))
	require.Equal(t, "a", boundaryKey([]string{"a"}))
	require.Equal(t, "a|b|c", boundaryKey([]string{"c", "b", "a"}))

	// Order-independent: same membership → same key (so a drifting numeric
	// bound that re-derives the same set reuses the same slot).
	require.Equal(t, boundaryKey([]string{"a", "b", "c"}), boundaryKey([]string{"c", "a", "b"}))
	// Different membership → different key.
	require.NotEqual(t, boundaryKey([]string{"a", "b"}), boundaryKey([]string{"a", "c"}))

	// Must not mutate the caller's slice (the request path may reuse it).
	in := []string{"c", "a", "b"}
	_ = boundaryKey(in)
	require.Equal(t, []string{"c", "a", "b"}, in)
}

// TestPerBoundaryEnabled — nil cfg / absent / explicit-false are all off;
// only an explicit true turns the axis on.
func TestPerBoundaryEnabled(t *testing.T) {
	require.False(t, perBoundaryEnabled(nil))
	require.False(t, perBoundaryEnabled(&common.SelectionPolicyConfig{}))
	require.False(t, perBoundaryEnabled(&common.SelectionPolicyConfig{EvalPerBoundary: boolPtr(false)}))
	require.True(t, perBoundaryEnabled(&common.SelectionPolicyConfig{EvalPerBoundary: boolPtr(true)}))
}

// TestEffectiveKey_BoundaryGating — the boundary dimension is honored only
// when the axis is on, is orthogonal to the EvalScope method/finality axes,
// and "*"/empty always collapses to the wildcard.
func TestEffectiveKey_BoundaryGating(t *testing.T) {
	// Axis OFF: a passed lane key is ignored → wildcard boundary.
	cfgOff := &common.SelectionPolicyConfig{EvalScope: common.EvalScopeNetwork}
	require.Equal(t,
		slotKey{"evm:1", "*", "*", "*"},
		effectiveKey(cfgOff, "evm:1", "eth_call", "finalized", "a|b"))

	// Axis ON, scope=network: only the boundary narrows; method/finality stay "*".
	cfgOn := &common.SelectionPolicyConfig{EvalScope: common.EvalScopeNetwork, EvalPerBoundary: boolPtr(true)}
	require.Equal(t,
		slotKey{"evm:1", "*", "*", "a|b"},
		effectiveKey(cfgOn, "evm:1", "eth_call", "finalized", "a|b"))
	// "*" boundary stays wildcard even with the axis on.
	require.Equal(t,
		slotKey{"evm:1", "*", "*", "*"},
		effectiveKey(cfgOn, "evm:1", "eth_call", "finalized", "*"))

	// Axis ON + scope=network-method: boundary AND method narrow; finality "*".
	cfgMB := &common.SelectionPolicyConfig{EvalScope: common.EvalScopeNetworkMethod, EvalPerBoundary: boolPtr(true)}
	require.Equal(t,
		slotKey{"evm:1", "eth_call", "*", "a|b"},
		effectiveKey(cfgMB, "evm:1", "eth_call", "finalized", "a|b"))
}
