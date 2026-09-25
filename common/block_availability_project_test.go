package common

import (
	"time"

	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project-level block availability is configured once under
// `projects[].upstreamDefaults.evm.blockAvailability` and inherited by every
// upstream that does not declare its own window. Enforcement itself is the
// existing per-upstream path — these tests only pin the inheritance rules and
// their precedence, because getting the precedence wrong silently widens or
// narrows the served range on every upstream at once.
func TestEvmUpstreamConfig_SetDefaults_InheritsBlockAvailability(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	projectDefaults := func() *EvmUpstreamConfig {
		return &EvmUpstreamConfig{
			BlockAvailability: &EvmBlockAvailabilityConfig{
				Lower: &EvmAvailabilityBoundConfig{LatestBlockMinus: i64(604800)},
			},
		}
	}
	lowerMinus := func(t *testing.T, e *EvmUpstreamConfig) int64 {
		t.Helper()
		require.NotNil(t, e.BlockAvailability, "expected a resolved blockAvailability")
		require.NotNil(t, e.BlockAvailability.Lower, "expected a resolved lower bound")
		require.NotNil(t, e.BlockAvailability.Lower.LatestBlockMinus)
		return *e.BlockAvailability.Lower.LatestBlockMinus
	}

	t.Run("inherited when the upstream declares no window", func(t *testing.T) {
		e := &EvmUpstreamConfig{ChainId: 1}
		require.NoError(t, e.SetDefaults(projectDefaults()))
		assert.EqualValues(t, 604800, lowerMinus(t, e))
	})

	// Bounds are recomputed per upstream at runtime against that upstream's own
	// state poller, so a shared pointer would let one upstream's mutation leak
	// into every sibling.
	t.Run("copied, never shared with the defaults object", func(t *testing.T) {
		defs := projectDefaults()
		e := &EvmUpstreamConfig{ChainId: 1}
		require.NoError(t, e.SetDefaults(defs))
		assert.NotSame(t, defs.BlockAvailability, e.BlockAvailability)
		assert.NotSame(t, defs.BlockAvailability.Lower, e.BlockAvailability.Lower)
	})

	t.Run("upstream's own blockAvailability wins", func(t *testing.T) {
		e := &EvmUpstreamConfig{
			ChainId: 1,
			BlockAvailability: &EvmBlockAvailabilityConfig{
				Lower: &EvmAvailabilityBoundConfig{LatestBlockMinus: i64(128)},
			},
		}
		require.NoError(t, e.SetDefaults(projectDefaults()))
		assert.EqualValues(t, 128, lowerMinus(t, e))
	})

	// The deprecated per-upstream knob is still an explicit statement about that
	// upstream, so it outranks the project-wide default.
	t.Run("upstream's own maxAvailableRecentBlocks wins", func(t *testing.T) {
		e := &EvmUpstreamConfig{ChainId: 1, MaxAvailableRecentBlocks: 256}
		require.NoError(t, e.SetDefaults(projectDefaults()))
		assert.EqualValues(t, 256, lowerMinus(t, e))
	})

	// nodeType:full otherwise derives a 128-block window. An explicitly
	// configured project default is not a guess and must outrank it.
	t.Run("inherited window outranks the nodeType full default", func(t *testing.T) {
		e := &EvmUpstreamConfig{ChainId: 1, NodeType: EvmNodeTypeFull}
		require.NoError(t, e.SetDefaults(projectDefaults()))
		assert.EqualValues(t, 604800, lowerMinus(t, e))
		assert.Zero(t, e.MaxAvailableRecentBlocks,
			"nodeType must not also derive a legacy 128 window that clamps this one")
	})

	t.Run("no defaults leaves existing behaviour untouched", func(t *testing.T) {
		e := &EvmUpstreamConfig{ChainId: 1, NodeType: EvmNodeTypeFull}
		require.NoError(t, e.SetDefaults(nil))
		assert.EqualValues(t, 128, lowerMinus(t, e))
	})
}

// A block below every upstream's availability window must reach the client as
// missing data. Before this mapping it fell through to the generic -32603
// "server side exception", which is indistinguishable from eRPC itself failing.
func TestTranslateToJsonRpcException_BlockUnavailableIsMissingData(t *testing.T) {
	blockUnavailable := func(id string) error {
		return NewErrUpstreamBlockUnavailable(id, 100, 1_000_000, 999_000)
	}

	t.Run("bare cause", func(t *testing.T) {
		got := clientWireCode(t, TranslateToJsonRpcException(blockUnavailable("up-a")))
		assert.EqualValues(t, JsonRpcErrorMissingData, got)
	})

	// The production path: every upstream skipped the block, so the network hands
	// translation an exhausted bundle — and the retry loop wraps that again.
	t.Run("exhausted bundle and its retry-exceeded wrapper", func(t *testing.T) {
		order := []string{"up-a", "up-b", "up-c"}
		causes := map[string]error{}
		for _, id := range order {
			causes[id] = blockUnavailable(id)
		}
		exhausted := newExhausted(t, order, causes)

		assert.EqualValues(t, JsonRpcErrorMissingData,
			clientWireCode(t, TranslateToJsonRpcException(exhausted)))
		assert.EqualValues(t, JsonRpcErrorMissingData,
			clientWireCode(t, TranslateToJsonRpcException(
				NewErrFailsafeRetryExceeded(ScopeNetwork, exhausted, nil))))
	})

	// All upstreams pre-check skipped due to block-unavailable (e.g. every upstream
	// exceeded its lower/upper availability bound). The skip carries a verdict —
	// the upstream was consulted, just not over the wire — so all-skipped is
	// still unanimity and must return -32014.
	t.Run("all pre-check skipped due to block unavailable", func(t *testing.T) {
		skip := func(id string) error {
			return NewErrUpstreamRequestSkipped(blockUnavailable(id), id)
		}
		order := []string{"up-a", "up-b", "up-c"}
		causes := map[string]error{
			"up-a": skip("up-a"),
			"up-b": skip("up-b"),
			"up-c": skip("up-c"),
		}
		exhausted := newExhausted(t, order, causes)
		assert.EqualValues(t, JsonRpcErrorMissingData,
			clientWireCode(t, TranslateToJsonRpcException(exhausted)),
			"all pre-check skips due to block-unavailable must be -32014")
		assert.EqualValues(t, JsonRpcErrorMissingData,
			clientWireCode(t, TranslateToJsonRpcException(
				NewErrFailsafeRetryExceeded(ScopeNetwork, exhausted, nil))),
			"retry-exceeded wrapper must also be -32014")
	})

	// A skip due to a reason other than block-unavailable (method filter, cordon,
	// etc.) is neutral — it carries no verdict and must not trigger -32014.
	t.Run("all skipped for non-block-unavailable reason", func(t *testing.T) {
		neutralSkip := func(id string) error {
			return NewErrUpstreamRequestSkipped(nil, id)
		}
		order := []string{"up-a", "up-b"}
		causes := map[string]error{"up-a": neutralSkip("up-a"), "up-b": neutralSkip("up-b")}
		exhausted := newExhausted(t, order, causes)
		assert.NotEqualValues(t, JsonRpcErrorMissingData,
			clientWireCode(t, TranslateToJsonRpcException(exhausted)),
			"neutral skips must not claim block is definitively absent")
	})

	// Membership in a multi-error bundle means ONE upstream hit the condition, not
	// that it is the verdict. Reporting -32014 for these would hand the client a
	// definitive "this block does not exist" while hiding a real consensus or
	// infrastructure failure — the worst possible way to be wrong here, because a
	// consumer can act on it by permanently skipping the block.
	t.Run("mixed bundles are not reported as missing data", func(t *testing.T) {
		unavailable := NewErrUpstreamBlockUnavailable("up-a", 100, 1_000_000, 999_000)
		timeout := NewErrEndpointRequestTimeout(5*time.Second, nil)

		for _, tc := range []struct {
			name string
			err  error
		}{
			{"consensus dispute", NewErrConsensusDispute("dispute", nil, []error{unavailable, timeout})},
			{"low participants", NewErrConsensusLowParticipants("low", nil, []error{unavailable, timeout})},
			{"exhausted dominated by timeouts", newExhausted(t,
				[]string{"up-a", "up-b", "up-c"},
				map[string]error{
					"up-a": unavailable,
					"up-b": timeout,
					"up-c": NewErrEndpointRequestTimeout(6*time.Second, nil),
				})},
			// A plurality is not unanimity: up-c never answered, so eRPC cannot
			// claim the block is definitively absent. The dominance scan picks
			// block-unavailable here, which is exactly why the verdict has to be
			// decided before that scan runs.
			{"exhausted with an unavailable plurality", newExhausted(t,
				[]string{"up-a", "up-b", "up-c"},
				map[string]error{
					"up-a": unavailable,
					"up-b": NewErrUpstreamBlockUnavailable("up-b", 100, 1_000_000, 999_000),
					"up-c": timeout,
				})},
			{"exhausted with a tie", newExhausted(t,
				[]string{"up-a", "up-b"},
				map[string]error{"up-a": unavailable, "up-b": timeout})},
		} {
			t.Run(tc.name, func(t *testing.T) {
				assert.NotEqualValues(t, JsonRpcErrorMissingData,
					clientWireCode(t, TranslateToJsonRpcException(tc.err)),
					"a single unavailable upstream must not speak for the whole request")
			})
		}
	})
}

// The inheritance must be asserted through the real project-initialisation path,
// not by calling EvmUpstreamConfig.SetDefaults directly: ProjectConfig.SetDefaults
// normalises upstreamDefaults FIRST, which materialises MaxAvailableRecentBlocks
// from `nodeType: full`, and ApplyDefaults then copies that value onto each
// upstream. A unit-level test never sees that sequence, and the legacy back-compat
// mapping silently replaced the configured window with the 128-block default.
func TestProjectConfig_SetDefaults_BlockAvailabilityReachesUpstreams(t *testing.T) {
	v := int64(604800)
	window := func() *EvmBlockAvailabilityConfig {
		return &EvmBlockAvailabilityConfig{
			Lower: &EvmAvailabilityBoundConfig{LatestBlockMinus: &v},
		}
	}

	for _, tc := range []struct {
		name     string
		defEvm   *EvmUpstreamConfig
		upstream *UpstreamConfig
	}{
		{
			name:     "upstream declares no evm block",
			defEvm:   &EvmUpstreamConfig{NodeType: EvmNodeTypeFull, BlockAvailability: window()},
			upstream: &UpstreamConfig{Id: "u1", Endpoint: "http://rpc1.localhost"},
		},
		{
			name:     "upstream declares a partial evm block",
			defEvm:   &EvmUpstreamConfig{NodeType: EvmNodeTypeFull, BlockAvailability: window()},
			upstream: &UpstreamConfig{Id: "u1", Endpoint: "http://rpc1.localhost", Evm: &EvmUpstreamConfig{ChainId: 1}},
		},
		{
			name:     "defaults carry no nodeType",
			defEvm:   &EvmUpstreamConfig{BlockAvailability: window()},
			upstream: &UpstreamConfig{Id: "u1", Endpoint: "http://rpc1.localhost"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &ProjectConfig{
				Id:               "main",
				UpstreamDefaults: &UpstreamConfig{Evm: tc.defEvm},
				Upstreams:        []*UpstreamConfig{tc.upstream},
			}
			require.NoError(t, p.SetDefaults(nil))

			e := p.Upstreams[0].Evm
			got := e.BlockAvailability
			require.NotNil(t, got)
			require.NotNil(t, got.Lower)
			require.NotNil(t, got.Lower.LatestBlockMinus)
			assert.EqualValues(t, 604800, *got.Lower.LatestBlockMinus,
				"configured project window must survive to the upstream")
			// The field's presence is not the guarantee. EvmAssertBlockAvailability
			// applies MaxAvailableRecentBlocks as a SECOND independent lower bound on
			// top of the resolved ones, so a leftover 128 here would clamp the
			// effective window back to 128 while this config still reads as 604800.
			assert.Zero(t, e.MaxAvailableRecentBlocks,
				"legacy field must not independently narrow the configured window")
		})
	}
}
