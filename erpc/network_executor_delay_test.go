package erpc

import (
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

// When EmptyResultDelayMultiplier is set, empty-result retries wait
// (EMA block time × multiplier) — reusing the same block-time estimate the
// block-unavailable delay already uses — instead of a hand-tuned constant.
func TestNetworkExecutor_ComputeDelay_EmptyResultBlockTimeRelative(t *testing.T) {
	mult := 1.0
	cfg := &common.NetworkFailsafeConfig{
		Retry: &common.RetryPolicyConfig{
			MaxAttempts:                2,
			EmptyResultDelayMultiplier: &mult,
			// EmptyResultDelay deliberately unset: the multiplier must drive it.
		},
	}
	e := &networkExecutor{
		cfg:              cfg,
		method:           "*",
		networkBlockTime: func() time.Duration { return 2 * time.Second },
	}
	got := e.computeDelay(nil, emptyArrayResponseForDelay(t), nil)
	assert.Equal(t, 2*time.Second, got,
		"empty-result delay must be blockTime × multiplier (2s × 1.0)")
}

// Before the block-time estimate warms up (returns 0), the fixed EmptyResultDelay
// is used as the fallback.
func TestNetworkExecutor_ComputeDelay_EmptyResultFallsBackToFixed(t *testing.T) {
	mult := 1.0
	cfg := &common.NetworkFailsafeConfig{
		Retry: &common.RetryPolicyConfig{
			MaxAttempts:                2,
			EmptyResultDelayMultiplier: &mult,
			EmptyResultDelay:           common.Duration(500 * time.Millisecond),
		},
	}
	e := &networkExecutor{
		cfg:              cfg,
		method:           "*",
		networkBlockTime: func() time.Duration { return 0 }, // not warmed up
	}
	got := e.computeDelay(nil, emptyArrayResponseForDelay(t), nil)
	assert.Equal(t, 500*time.Millisecond, got,
		"with block time unknown, fall back to fixed EmptyResultDelay")
}
