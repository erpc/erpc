package integrity

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reconfirmingHistory is a History + PinReconfirmer whose pins refresh from a
// fixed "canonical" chain on ReconfirmPin — simulating the ChainView adopting
// whatever the network currently serves.
type reconfirmingHistory struct {
	pins        map[int64]string // the (possibly stale) cached pins the checks read
	canonical   map[int64]string // what a fresh fetch would return
	calls       int
	fail        bool // reconfirm fetch unavailable
	rateLimited bool // re-confirmation suppressed: pin returned, but unverified
}

func (h *reconfirmingHistory) HashAt(n int64) (string, bool) { v, ok := h.pins[n]; return v, ok }

func (h *reconfirmingHistory) ReconfirmPin(ctx context.Context, n int64) (string, PinConfirmation) {
	h.calls++
	if h.rateLimited {
		// Exactly what the ChainView does inside reconfirmCooldown: hand back the
		// cached pin without re-resolving it.
		return h.pins[n], PinRateLimited
	}
	if h.fail {
		return "", PinUnverifiable
	}
	c, ok := h.canonical[n]
	if !ok {
		return "", PinUnverifiable
	}
	h.pins[n] = c // adopt
	return c, PinFresh
}

// rejectAll mirrors the strict shadow config: reorg-sensitive mismatches reject
// at every finality — the exact policy that made a stale pin self-blocking.
var rejectAll = ReorgPolicy{Finalized: BehaviorError, Unfinalized: BehaviorError}

func validateBlockPolicy(t *testing.T, result []byte, cs CheckSet, hist History, policy ReorgPolicy) Result {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), result, nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	return Validate(context.Background(), Input{
		Method:   "eth_getBlockByNumber",
		Upstream: common.NewFakeUpstream("u"),
		Response: rs,
		Checks:   cs,
		History:  hist,
		Reorg:    policy,
	})
}

func outcomeOf(res Result, id string) string {
	for _, oc := range res.Outcomes {
		if oc.CheckID == id {
			return oc.Outcome
		}
	}
	return ""
}

// The reorg self-block scenario: block N reorged, the pin still holds the old
// fork's hash, and every upstream now serves the new fork. Without reconfirm,
// hashStability rejects every honest response and the pin never adopts.
func TestReconfirm_ReorgAdoptsAndPasses(t *testing.T) {
	cs := only("hashStability", nil)

	t.Run("stale pin after a reorg → reconfirmed pass, pin adopted", func(t *testing.T) {
		hist := &reconfirmingHistory{
			pins:      map[int64]string{0x10: "0xold"},
			canonical: map[int64]string{0x10: "0xnew"},
		}
		res := validateBlockPolicy(t, blockResult("0x10", "0xnew", "0xparent"), cs, hist, rejectAll)
		assert.NoError(t, res.Err, "an honest new-fork response must not be rejected")
		assert.Equal(t, "reconfirmed", outcomeOf(res, "hashStability"))
		assert.Equal(t, 1, hist.calls)
		assert.Equal(t, "0xnew", hist.pins[0x10], "the pin must adopt the new fork")
	})

	t.Run("mismatch that survives the fresh pin is genuine → reject stands", func(t *testing.T) {
		hist := &reconfirmingHistory{
			pins:      map[int64]string{0x10: "0xpin"},
			canonical: map[int64]string{0x10: "0xpin"}, // fresh fetch confirms the pin
		}
		res := validateBlockPolicy(t, blockResult("0x10", "0xbogus", "0xparent"), cs, hist, rejectAll)
		require.Error(t, res.Err)
		assert.True(t, common.HasErrorCode(res.Err, common.ErrCodeEndpointContentValidation))
		assert.Equal(t, "reject", outcomeOf(res, "hashStability"))
		assert.Equal(t, 1, hist.calls)
	})

	t.Run("reconfirm unavailable → verdict unchanged (strict reject)", func(t *testing.T) {
		hist := &reconfirmingHistory{
			pins: map[int64]string{0x10: "0xold"},
			fail: true,
		}
		res := validateBlockPolicy(t, blockResult("0x10", "0xnew", "0xparent"), cs, hist, rejectAll)
		require.Error(t, res.Err)
		assert.Equal(t, 1, hist.calls)
	})

	t.Run("plain History without PinReconfirmer → today's behavior", func(t *testing.T) {
		hist := mockHistory{0x10: "0xold"}
		res := validateBlockPolicy(t, blockResult("0x10", "0xnew", "0xparent"), cs, hist, rejectAll)
		require.Error(t, res.Err)
	})
}

// Regression — mainnet 25589196, 2026-07-22. A stale pin was re-confirmed once;
// for the next second every further dispute at that height hit the cooldown,
// which handed the SAME unverified pin back as if it were a confirmation. The
// engine then hard-rejected 24 honest responses from three independent upstreams
// in ~700ms, erroring 8 client requests with nothing left to fail over to (all
// upstreams rejected → 0 saves). Canonical later proved the upstreams right and
// the pin non-canonical. A rate-limited answer carries no evidence, so it must
// never license a rejection.
func TestReconfirm_RateLimitedPinNeverRejects(t *testing.T) {
	cs := only("hashStability", nil)

	newHist := func() *reconfirmingHistory {
		return &reconfirmingHistory{
			pins:        map[int64]string{0x10: "0xstalepin"},
			rateLimited: true,
		}
	}

	t.Run("rate-limited reconfirm degrades the reject to a soft-flag", func(t *testing.T) {
		hist := newHist()
		res := validateBlockPolicy(t, blockResult("0x10", "0xhonest", "0xparent"), cs, hist, rejectAll)

		assert.NoError(t, res.Err, "an unverified pin must not reject an honest response")
		assert.Equal(t, "soft_flag", outcomeOf(res, "hashStability"))
		require.Len(t, res.Recorded, 1, "the mismatch must still be recorded, not swallowed")
		assert.Equal(t, "hashStability", res.Recorded[0].CheckID)
		assert.Equal(t, 1, hist.calls)
		assert.Equal(t, "0xstalepin", hist.pins[0x10], "a rate-limited call must not adopt anything")
	})

	t.Run("the whole burst is served — no client failures", func(t *testing.T) {
		// The incident shape: many requests for the same height while the pin is
		// rate-limited. Every one of them must survive.
		hist := newHist()
		for i := 0; i < 24; i++ {
			res := validateBlockPolicy(t, blockResult("0x10", "0xhonest", "0xparent"), cs, hist, rejectAll)
			require.NoErrorf(t, res.Err, "request %d was rejected on an unverified pin", i)
			require.Equal(t, "soft_flag", outcomeOf(res, "hashStability"))
		}
		assert.Equal(t, 24, hist.calls)
	})

	t.Run("soft-flag policy is unaffected (already non-rejecting)", func(t *testing.T) {
		hist := newHist()
		policy := ReorgPolicy{Finalized: BehaviorRecord, Unfinalized: BehaviorRecord}
		res := validateBlockPolicy(t, blockResult("0x10", "0xhonest", "0xparent"), cs, hist, policy)
		assert.NoError(t, res.Err)
		assert.Equal(t, "soft_flag", outcomeOf(res, "hashStability"))
	})

	t.Run("a fresh reconfirm still rejects a genuine mismatch", func(t *testing.T) {
		// The degrade must not blunt real detection: once the pin IS re-resolved
		// and the mismatch survives it, the strict verdict stands.
		hist := &reconfirmingHistory{
			pins:      map[int64]string{0x10: "0xpin"},
			canonical: map[int64]string{0x10: "0xpin"},
		}
		res := validateBlockPolicy(t, blockResult("0x10", "0xbogus", "0xparent"), cs, hist, rejectAll)
		require.Error(t, res.Err)
		assert.Equal(t, "reject", outcomeOf(res, "hashStability"))
	})

	t.Run("parentHashLinkage is covered too (the check that fired in the incident)", func(t *testing.T) {
		hist := &reconfirmingHistory{
			pins:        map[int64]string{0x10: "0xstaleparent"},
			rateLimited: true,
		}
		res := validateBlockPolicy(t, blockResult("0x11", "0xchild", "0xrealparent"), only("parentHashLinkage", nil), hist, rejectAll)
		assert.NoError(t, res.Err)
		assert.Equal(t, "soft_flag", outcomeOf(res, "parentHashLinkage"))
	})
}

func TestReconfirm_ParentHashLinkage_DisputesParent(t *testing.T) {
	cs := only("parentHashLinkage", nil)
	// Reorg at N-1: the child links to the new parent, the pin still holds the old.
	hist := &reconfirmingHistory{
		pins:      map[int64]string{0x10: "0xoldparent"},
		canonical: map[int64]string{0x10: "0xnewparent"},
	}
	res := validateBlockPolicy(t, blockResult("0x11", "0xchild", "0xnewparent"), cs, hist, rejectAll)
	assert.NoError(t, res.Err)
	assert.Equal(t, "reconfirmed", outcomeOf(res, "parentHashLinkage"))
	assert.Equal(t, "0xnewparent", hist.pins[0x10], "the PARENT pin (N-1) is the one reconfirmed")
}

// soft-flag mode also self-heals: the mismatch clears instead of emitting noise.
func TestReconfirm_SoftFlagModeHealsInsteadOfRecording(t *testing.T) {
	cs := only("hashStability", nil)
	hist := &reconfirmingHistory{
		pins:      map[int64]string{0x10: "0xold"},
		canonical: map[int64]string{0x10: "0xnew"},
	}
	res := validateBlockPolicy(t, blockResult("0x10", "0xnew", "0xparent"), cs, hist, DefaultReorgPolicy())
	assert.NoError(t, res.Err)
	assert.Empty(t, res.Recorded, "a reorg must not be recorded as a soft-flag violation")
	assert.Equal(t, "reconfirmed", outcomeOf(res, "hashStability"))
}

// receiptVsBlock's pin-consistency branch is anchored to the same pin: a receipt
// from the new fork must not be rejected against a stale pin.
func TestReconfirm_ReceiptVsBlockPinBranch(t *testing.T) {
	cs := only("receiptVsBlock", nil)
	receipt := []byte(`{"transactionHash":"0xaa","blockHash":"0xnew","blockNumber":"0x10","logs":[]}`)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xaa"]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), receipt, nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	hist := &reconfirmingHistory{
		pins:      map[int64]string{0x10: "0xold"},
		canonical: map[int64]string{0x10: "0xnew"},
	}
	// No resolver: the log-corroboration half no-ops; only the pin branch runs.
	res := Validate(context.Background(), Input{
		Method:   "eth_gettransactionreceipt",
		Upstream: common.NewFakeUpstream("u"),
		Response: rs,
		Checks:   cs,
		History:  hist,
		Reorg:    rejectAll,
	})
	assert.NoError(t, res.Err)
	assert.Equal(t, "reconfirmed", outcomeOf(res, "receiptVsBlock"))
	assert.Equal(t, "0xnew", hist.pins[0x10])
}
