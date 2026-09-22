package integrity

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flappingResolver reports the block as finalized on the first read and
// unfinalized afterwards — what a real upstream does when its effective
// finalized head ROLLS BACK during a reorg (erpc logs "applied large finalized
// block rollback for upstream in tracker" for exactly this).
type flappingResolver struct{ calls *int }

func (f flappingResolver) IsFinalized(ctx context.Context, bn int64) (bool, bool) {
	*f.calls++
	return *f.calls == 1, true
}
func (f flappingResolver) CanonicalReceipts(ctx context.Context, ref string) ([]Receipt, bool) {
	return nil, false
}
func (f flappingResolver) CanonicalHeader(ctx context.Context, ref string) (*Header, bool) {
	return nil, false
}

func validateWithResolver(t *testing.T, result []byte, cs CheckSet, hist History, r Resolver, policy ReorgPolicy) Result {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x11",false]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), result, nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	return Validate(context.Background(), Input{
		Method:   "eth_getBlockByNumber",
		Upstream: common.NewFakeUpstream("u"),
		Response: rs,
		Checks:   cs,
		Params:   []any{"0x11", false},
		History:  hist,
		Resolver: r,
		Reorg:    policy,
	})
}

// A violation's verdict and its reported finality must describe the SAME
// observation. Finality comes from the serving upstream's effective finalized
// head, which moves — and rolls back mid-reorg. Reading it once for the verdict
// and again for the metric produced a reject (judged finalized) reported as
// unfinalized: observed live on mainnet, where a lone parentHashLinkage reject
// appeared with finality="unfinalized" beside five soft-flags of the same check
// at the same height, making the metric misattribute which policy fired.
func TestFinalityIsObservedOncePerResponse(t *testing.T) {
	// Policy that maps the two finalities to DIFFERENT verdicts, so a
	// disagreement is observable.
	policy := ReorgPolicy{Finalized: BehaviorError, Unfinalized: BehaviorRecord}
	hist := mockHistory{0x11: "0xbbb"} // pin disagrees with the response below
	body := blockResult("0x11", "0xddd", "0xaaa")

	t.Run("verdict and label agree when finality flaps mid-validation", func(t *testing.T) {
		calls := 0
		res := validateWithResolver(t, body, only("hashStability", nil), hist, flappingResolver{calls: &calls}, policy)

		// First read said finalized → the strict verdict must apply...
		require.Error(t, res.Err, "finalized verdict should reject")
		assert.Equal(t, "reject", outcomeOf(res, "hashStability"))
		// ...and the label must say finalized too, not report the later value.
		assert.Equal(t, "finalized", res.Finality,
			"the reported finality must match the observation the verdict used")
		assert.Equal(t, 1, calls, "finality must be resolved once per response")
	})

	t.Run("a soft-flagged mismatch reports the same finality it was judged on", func(t *testing.T) {
		res := validateWithResolver(t, body, only("hashStability", nil), hist,
			mockResolver{finalized: false, known: true}, ReorgPolicy{Finalized: BehaviorError, Unfinalized: BehaviorRecord})
		require.NoError(t, res.Err)
		require.Len(t, res.Recorded, 1)
		assert.Equal(t, "unfinalized", res.Recorded[0].Finality)
	})

	t.Run("deterministic rejects are still labelled (verdict never consults finality)", func(t *testing.T) {
		// blockByNumberIdentity is deterministic: it rejects regardless of
		// finality, but the label must still be resolved for observability.
		res := validateWithResolver(t, blockResult("0x99", "0xddd", "0xaaa"),
			only("blockByNumberIdentity", nil), nil, mockResolver{finalized: true, known: true}, policy)
		require.Error(t, res.Err)
		assert.Equal(t, "finalized", res.Finality)
	})

	t.Run("no resolver → unknown, and nothing panics", func(t *testing.T) {
		res := validateWithResolver(t, body, only("hashStability", nil), hist, nil, policy)
		// Unknown finality is treated as unfinalized → recorded, labelled unknown.
		require.NoError(t, res.Err)
		require.Len(t, res.Recorded, 1)
		assert.Equal(t, "unknown", res.Recorded[0].Finality)
	})
}
