package svm

import (
	"context"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// upstreamWithFloor is a per-id SVM upstream whose ledger floor is settable.
func upstreamWithFloor(id string, latest, floor int64, floorKnown bool) common.Upstream {
	return &slotLagUpstream{
		id:     id,
		poller: &pollerAtSlot{slot: latest, floor: floor, floorKnown: floorKnown},
	}
}

func reqOf(t *testing.T, method string, params string) *common.NormalizedRequest {
	t.Helper()
	return common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)))
}

func TestRequestLowerBoundSlot_InScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method string
		params string
		want   int64
	}{
		{"getBlock", `[100]`, 100},
		{"getBlock", `[100,{"encoding":"jsonParsed"}]`, 100},
		{"getConfirmedBlock", `[100]`, 100},
		{"getBlockTime", `[100]`, 100},
		{"getBlocks", `[100]`, 100},
		{"getBlocks", `[100,200]`, 100},
		// Post-injection shape: commitment injection appends a trailing object
		// and never displaces param 0.
		{"getBlocks", `[100,{"commitment":"finalized"}]`, 100},
		{"getBlocks", `[100,200,{"commitment":"finalized"}]`, 100},
		{"getBlocksWithLimit", `[100,10]`, 100},
		{"getBlocksWithLimit", `[100,10,{"commitment":"finalized"}]`, 100},
		// Slot 0 is in scope. getBlocks(0, 100) against a validator whose
		// ledger starts at 200000 is exactly the silently-short-list case.
		{"getBlocks", `[0,100]`, 0},
		{"getBlock", `[0]`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.params, func(t *testing.T) {
			slot, ok := RequestLowerBoundSlot(context.Background(), reqOf(t, tc.method, tc.params))
			require.True(t, ok, "method must be in scope")
			require.Equal(t, tc.want, slot)
		})
	}
}

// Every method here either names no slot at all, or names one the ledger floor
// is not the predicate for. Each must fall through untouched — the unmatched
// path is the primary path.
func TestRequestLowerBoundSlot_OutOfScope(t *testing.T) {
	t.Parallel()
	cases := []struct{ method, params string }{
		// Keyed by signature, not slot.
		{"getTransaction", `["5V3s...sig"]`},
		{"getSignaturesForAddress", `["Vote111111111111111111111111111111111111111"]`},
		// Answered from the leader schedule or restricted to the current epoch.
		{"getSlotLeaders", `[100,10]`},
		{"getLeaderSchedule", `[100]`},
		{"getBlockProduction", `[{"range":{"firstSlot":100}}]`},
		// Valid non-error answer on every node regardless of floor.
		{"getBlockCommitment", `[100]`},
		// Keyed by epoch.
		{"getInflationReward", `[["addr"],{"epoch":100}]`},
		{"getStakeActivation", `["addr",{"epoch":100}]`},
		// Param 0 is a base58 pubkey. These prove that even if the method set
		// were widened by accident, toInt64 refuses a base58 string.
		{"getAccountInfo", `["Vote111111111111111111111111111111111111111"]`},
		{"getBalance", `["Vote111111111111111111111111111111111111111"]`},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			slot, ok := RequestLowerBoundSlot(context.Background(), reqOf(t, tc.method, tc.params))
			require.False(t, ok, "method must be out of scope")
			require.Zero(t, slot)
		})
	}
}

func TestRequestLowerBoundSlot_Malformed(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, params string }{
		{"empty params", `[]`},
		{"null slot", `[null]`},
		// A string slot falls through so the upstream answers -32602 honestly,
		// rather than eRPC guessing what the caller meant.
		{"string slot", `["100"]`},
		{"negative slot", `[-1]`},
		// 199999.9 is not slot 199999: truncating it would reject (or admit) a
		// slot the client never named, pre-empting the node's own -32602.
		{"fractional slot", `[199999.9]`},
		// JSON numbers decode as float64; 1e30 is integral but far beyond
		// int64, so it cannot be a slot either.
		{"slot beyond int64", `[1e30]`},
		{"options object first", `[{"commitment":"finalized"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slot, ok := RequestLowerBoundSlot(context.Background(), reqOf(t, "getBlock", tc.params))
			require.False(t, ok)
			require.Zero(t, slot)
		})
	}
	slot, ok := RequestLowerBoundSlot(context.Background(), nil)
	require.False(t, ok)
	require.Zero(t, slot)
}

func TestGetConfirmedBlockAlias_LowerBoundSlotParity(t *testing.T) {
	t.Parallel()
	for _, params := range []string{`[100]`, `[100,{"encoding":"base64"}]`, `[0]`, `[]`, `["100"]`} {
		t.Run(params, func(t *testing.T) {
			gotSlot, gotOk := RequestLowerBoundSlot(context.Background(), reqOf(t, "getBlock", params))
			aliasSlot, aliasOk := RequestLowerBoundSlot(context.Background(), reqOf(t, "getConfirmedBlock", params))
			require.Equal(t, gotOk, aliasOk, "alias must be in scope exactly when getBlock is")
			require.Equal(t, gotSlot, aliasSlot)
		})
	}
}

func TestRequestRangeEndSlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("getBlocks named end", func(t *testing.T) {
		end, fromTip, ok := RequestRangeEndSlot(ctx, reqOf(t, "getBlocks", `[50, 1000]`), 50, 80)
		require.True(t, ok)
		require.EqualValues(t, 1000, end)
		require.False(t, fromTip, "a named end does not move with the tip")
	})

	t.Run("getBlocks omitted end uses tip", func(t *testing.T) {
		end, fromTip, ok := RequestRangeEndSlot(ctx, reqOf(t, "getBlocks", `[50]`), 50, 80)
		require.True(t, ok)
		require.EqualValues(t, 80, end)
		require.True(t, fromTip)
	})

	t.Run("getBlocks omitted end inside agave span is not the tip", func(t *testing.T) {
		end, fromTip, ok := RequestRangeEndSlot(ctx, reqOf(t, "getBlocks", `[50]`), 50, 50+maxGetBlocksRange+10)
		require.True(t, ok)
		require.EqualValues(t, 50+maxGetBlocksRange, end)
		require.False(t, fromTip, "start+span is fixed by the request")
	})

	t.Run("getBlocks null end uses tip like omitted", func(t *testing.T) {
		end, fromTip, ok := RequestRangeEndSlot(ctx, reqOf(t, "getBlocks", `[50, null]`), 50, 80)
		require.True(t, ok)
		require.EqualValues(t, 80, end)
		require.True(t, fromTip)

		end, fromTip, ok = RequestRangeEndSlot(ctx, reqOf(t, "getBlocks", `[50, null, {"commitment":"finalized"}]`), 50, 80)
		require.True(t, ok)
		require.EqualValues(t, 80, end)
		require.True(t, fromTip)
	})

	t.Run("getBlocksWithLimit uses tip, not start+limit", func(t *testing.T) {
		// limit is a confirmed-block COUNT. start+limit would both reject
		// limit=1 against an upper equal to start, and under-bound ranges
		// that skip slots.
		end, fromTip, ok := RequestRangeEndSlot(ctx, reqOf(t, "getBlocksWithLimit", `[50, 1]`), 50, 80)
		require.True(t, ok)
		require.EqualValues(t, 80, end)
		require.True(t, fromTip)

		end, _, ok = RequestRangeEndSlot(ctx, reqOf(t, "getBlocksWithLimit", `[50, 2000]`), 50, 80)
		require.True(t, ok)
		require.EqualValues(t, 80, end)
	})

	t.Run("getBlocksWithLimit unknown tip fails open", func(t *testing.T) {
		end, _, ok := RequestRangeEndSlot(ctx, reqOf(t, "getBlocksWithLimit", `[50, 10]`), 50, 0)
		require.False(t, ok)
		require.Zero(t, end)
	})
}
