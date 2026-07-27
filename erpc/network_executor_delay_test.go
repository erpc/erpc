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
// ErrUpstreamsExhausted where every cause is -32004 (all providers returned
// "block not available") must retry as missing_data. Before the fix, HasErrorCode
// walking the cause chain matched ErrCodeEndpointMissingData first, hit the
// RetryEmpty=false gate, and returned "" — blocking the retry entirely.
func TestNetworkExecutor_ShouldRetry_ExhaustedAllMissingDataRetries(t *testing.T) {
	cfg := &common.NetworkFailsafeConfig{
		Retry: &common.RetryPolicyConfig{MaxAttempts: 3},
	}
	e := &networkExecutor{cfg: cfg, method: "*"}

	md1 := common.NewErrEndpointMissingData(errors.New("-32004 upstream 1"), nil)
	md2 := common.NewErrEndpointMissingData(errors.New("-32004 upstream 2"), nil)
	exhausted := common.NewErrUpstreamsExhaustedWithCause(errors.Join(md1, md2))

	assert.Equal(t, "missing_data", e.shouldRetryWithReason(nil, nil, exhausted, 0),
		"all-missing ErrUpstreamsExhausted must retry as missing_data")

	// Production: directiveDefaults is nil for Solana, so directives are never set.
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[]}`))
	assert.Equal(t, "missing_data", e.shouldRetryWithReason(req, nil, exhausted, 0),
		"nil directives must not block all-missing ErrUpstreamsExhausted retry")
}

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

// permMissingData builds a MissingData error flagged permanent (SVM
// -32007/-32009: skipped/authoritatively-absent slot).
func permMissingData(msg string) error {
	md := common.NewErrEndpointMissingData(errors.New(msg), nil)
	md.(*common.ErrEndpointMissingData).WithPermanentMissingData(true)
	return md
}

// When every exhausted cause is a PERMANENT skipped/absent slot, the
// cross-provider sweep already ran and no wait-and-retry can change the verdict,
// so shouldRetryWithReason must surface it now (""). EmptyResultMaxAttempts is
// set HIGH (5) at attempt 0 so a "" proves the permanent short-circuit — not the
// cap. Pins the `if permanent { return "" }` branch in network_executor.go.
func TestNetworkExecutor_ShouldRetry_ExhaustedAllPermanentSkipsIsTerminal(t *testing.T) {
	cfg := &common.NetworkFailsafeConfig{
		Retry: &common.RetryPolicyConfig{MaxAttempts: 6, EmptyResultMaxAttempts: 5},
	}
	e := &networkExecutor{cfg: cfg, method: "*"}

	exhausted := common.NewErrUpstreamsExhaustedWithCause(
		errors.Join(permMissingData("-32007 up1"), permMissingData("-32007 up2")))

	assert.Equal(t, "", e.shouldRetryWithReason(nil, nil, exhausted, 0),
		"all-permanent skipped slots must short-circuit despite high EmptyResultMaxAttempts")

	// Production shape: Solana leaves directives nil (directiveDefaults nil), so
	// the nil-directive path must not resurrect the killed retry either.
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[]}`))
	assert.Equal(t, "", e.shouldRetryWithReason(req, nil, exhausted, 0),
		"nil directives must not revive a retry the permanent short-circuit killed")
}

// One TRANSIENT sibling among the exhausted causes still warrants the wait: a
// not-yet-indexed block may materialize, so the whole set keeps retrying. Guards
// the ALL-permanent loop against collapsing to ANY-permanent.
func TestNetworkExecutor_ShouldRetry_ExhaustedMixedStillRetries(t *testing.T) {
	cfg := &common.NetworkFailsafeConfig{
		Retry: &common.RetryPolicyConfig{MaxAttempts: 6, EmptyResultMaxAttempts: 5},
	}
	e := &networkExecutor{cfg: cfg, method: "*"}

	transient := common.NewErrEndpointMissingData(errors.New("-32004 not indexed yet"), nil)
	exhausted := common.NewErrUpstreamsExhaustedWithCause(
		errors.Join(permMissingData("-32007 skipped"), transient))

	assert.Equal(t, "missing_data", e.shouldRetryWithReason(nil, nil, exhausted, 0),
		"a transient sibling means a wait-retry can still surface the block")
}

// A SINGLE permanent MissingData (non-exhausted path) short-circuits to "" too,
// while a single TRANSIENT one still retries. Pins the
// `if IsPermanentlyMissingData(err) { return "" }` branch and guards it against
// swallowing transient missing-data.
func TestNetworkExecutor_ShouldRetry_SinglePermanentIsTerminal(t *testing.T) {
	cfg := &common.NetworkFailsafeConfig{
		Retry: &common.RetryPolicyConfig{MaxAttempts: 6, EmptyResultMaxAttempts: 5},
	}
	e := &networkExecutor{cfg: cfg, method: "*"}

	assert.Equal(t, "", e.shouldRetryWithReason(nil, nil, permMissingData("-32009 authoritative skip"), 0),
		"a single permanent skip surfaces now; no wait-retry can change it")

	transient := common.NewErrEndpointMissingData(errors.New("-32004 not indexed yet"), nil)
	assert.Equal(t, "missing_data", e.shouldRetryWithReason(nil, nil, transient, 0),
		"a single transient MissingData must still retry (wait-and-retry may surface it)")
}
