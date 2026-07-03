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
	pins      map[int64]string // the (possibly stale) cached pins the checks read
	canonical map[int64]string // what a fresh fetch would return
	calls     int
	fail      bool // reconfirm fetch unavailable
}

func (h *reconfirmingHistory) HashAt(n int64) (string, bool) { v, ok := h.pins[n]; return v, ok }

func (h *reconfirmingHistory) ReconfirmPin(ctx context.Context, n int64) (string, bool) {
	h.calls++
	if h.fail {
		return "", false
	}
	c, ok := h.canonical[n]
	if !ok {
		return "", false
	}
	h.pins[n] = c // adopt
	return c, true
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
