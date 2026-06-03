package erpc

import (
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func emptyArrayResponseForDelay(t *testing.T) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponse(1, []interface{}{}, nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrr)
}

// Empty-result retries reuse the SAME dynamic block-time delay the block-unavailable
// path uses (EMA block time × BlockUnavailableDelayMultiplier) — there is no separate
// per-policy multiplier. A not-yet-visible block/tx typically appears within ~one
// block, so this waits about that long instead of a hand-tuned constant.
func TestNetworkExecutor_ComputeDelay_EmptyResultUsesDynamicBlockTimeDelay(t *testing.T) {
	cfg := &common.NetworkFailsafeConfig{
		Retry: &common.RetryPolicyConfig{
			MaxAttempts: 2,
			// EmptyResultDelay deliberately unset: the dynamic block-time delay must drive it.
		},
	}
	e := &networkExecutor{
		cfg:                          cfg,
		method:                       "*",
		dynamicBlockUnavailableDelay: func() time.Duration { return 1600 * time.Millisecond }, // e.g. 2s block × 0.8
	}
	got := e.computeDelay(nil, emptyArrayResponseForDelay(t), nil, 0)
	assert.Equal(t, 1600*time.Millisecond, got,
		"empty-result delay must reuse the dynamic block-time delay (block-unavailable mechanism)")
}

// Before the block-time estimate warms up (dynamic delay returns 0), the fixed
// per-policy EmptyResultDelay is used as the fallback.
func TestNetworkExecutor_ComputeDelay_EmptyResultFallsBackToFixed(t *testing.T) {
	cfg := &common.NetworkFailsafeConfig{
		Retry: &common.RetryPolicyConfig{
			MaxAttempts:      2,
			EmptyResultDelay: common.Duration(500 * time.Millisecond),
		},
	}
	e := &networkExecutor{
		cfg:                          cfg,
		method:                       "*",
		dynamicBlockUnavailableDelay: func() time.Duration { return 0 }, // not warmed up
	}
	got := e.computeDelay(nil, emptyArrayResponseForDelay(t), nil, 0)
	assert.Equal(t, 500*time.Millisecond, got,
		"with block time unknown, fall back to fixed EmptyResultDelay")
}

// The single "data not available yet" cap (EmptyResultMaxAttempts) bounds
// block-unavailable and missing-data retries too — not just plain empty results —
// so every not-ready path gets the same one-retry treatment, decoupled from
// MaxAttempts (which governs genuine-error failover).
func TestNetworkExecutor_ShouldRetry_DataUnavailableSharesOneCap(t *testing.T) {
	cfg := &common.NetworkFailsafeConfig{
		Retry: &common.RetryPolicyConfig{
			MaxAttempts:            6, // error-failover ceiling, intentionally higher
			EmptyResultMaxAttempts: 2, // one original + one retry for "not available yet"
		},
	}
	e := &networkExecutor{cfg: cfg, method: "*"}

	bu := common.NewErrUpstreamBlockUnavailable("up1", 100, 99, 50)
	md := common.NewErrEndpointMissingData(errors.New("empty"), nil)

	// First attempt (0): retry fires for both.
	assert.Equal(t, "block_unavailable", e.shouldRetryWithReason(nil, nil, bu, 0))
	assert.Equal(t, "missing_data", e.shouldRetryWithReason(nil, nil, md, 0))

	// Second attempt (1): the shared cap stops both, even though MaxAttempts=6.
	assert.Equal(t, "", e.shouldRetryWithReason(nil, nil, bu, 1),
		"block_unavailable must respect EmptyResultMaxAttempts, not MaxAttempts")
	assert.Equal(t, "", e.shouldRetryWithReason(nil, nil, md, 1),
		"missing_data must respect EmptyResultMaxAttempts, not MaxAttempts")
}

// The former separate BlockUnavailableDelay was merged into EmptyResultDelay, so a
// block-unavailable retry falls back to EmptyResultDelay before block-time warms up.
func TestNetworkExecutor_ComputeDelay_BlockUnavailableUsesEmptyResultDelay(t *testing.T) {
	cfg := &common.NetworkFailsafeConfig{
		Retry: &common.RetryPolicyConfig{
			MaxAttempts:      2,
			EmptyResultDelay: common.Duration(700 * time.Millisecond),
		},
	}
	e := &networkExecutor{
		cfg:                          cfg,
		method:                       "*",
		dynamicBlockUnavailableDelay: func() time.Duration { return 0 }, // not warmed up
	}
	bu := common.NewErrUpstreamBlockUnavailable("up1", 100, 99, 50)
	got := e.computeDelay(nil, nil, bu, 0)
	assert.Equal(t, 700*time.Millisecond, got,
		"block-unavailable fallback now uses the unified EmptyResultDelay")
}
