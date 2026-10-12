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

func TestCheckUpstreamLedgerFloor_SkipsBelowFloor(t *testing.T) {
	t.Parallel()
	up := upstreamWithFloor("validator", 300000, 200000, true)
	err := CheckUpstreamLedgerFloor(context.Background(), up, reqOf(t, "getBlock", `[1000]`))
	require.Error(t, err)

	// The verdict class drives routing: MissingData sweeps to the next
	// upstream, and permanent suppresses the delayed re-sweep.
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData),
		"must be MissingData so the sweep reaches an upstream that has the slot")
	require.True(t, common.IsPermanentlyMissingData(err),
		"waiting cannot lower a floor, so no time-delayed re-sweep")

	// The wire code drives the CLIENT: Solana clients dispatch on the number.
	// -32001 is agave's own code for a purged slot; -32009 would over-claim
	// that long-term storage was checked.
	require.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcExceptionInternal),
		"the JSON-RPC exception wrap is what preserves the numeric code")
	require.Contains(t, err.Error(), "-32001")
	require.Contains(t, err.Error(), "First available block: 200000",
		"the message must name the real boundary, matching agave's own wording")
}

func TestCheckUpstreamLedgerFloor_FailsOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name string
		up   common.Upstream
		req  *common.NormalizedRequest
	}{
		{
			"slot at the floor is servable",
			upstreamWithFloor("v", 300000, 200000, true),
			reqOf(t, "getBlock", `[200000]`),
		},
		{
			"slot above the floor is servable",
			upstreamWithFloor("v", 300000, 200000, true),
			reqOf(t, "getBlock", `[250000]`),
		},
		{
			"floor unknown is no constraint",
			upstreamWithFloor("v", 300000, 0, false),
			reqOf(t, "getBlock", `[1000]`),
		},
		{
			"observed floor of zero never excludes",
			upstreamWithFloor("archive", 300000, 0, true),
			reqOf(t, "getBlock", `[1000]`),
		},
		{
			"method out of scope",
			upstreamWithFloor("v", 300000, 200000, true),
			reqOf(t, "getTransaction", `["sig"]`),
		},
		{
			"nil poller",
			&slotLagUpstream{id: "v", poller: nil},
			reqOf(t, "getBlock", `[1000]`),
		},
		{
			"poller reports itself null",
			&slotLagUpstream{id: "v", poller: &pollerAtSlot{null: true, floor: 200000, floorKnown: true}},
			reqOf(t, "getBlock", `[1000]`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, CheckUpstreamLedgerFloor(ctx, tc.up, tc.req))
		})
	}
}

func TestPoolCoversSlotStart(t *testing.T) {
	t.Parallel()

	t.Run("covered by the lowest-floor upstream", func(t *testing.T) {
		t.Parallel()
		ups := []common.Upstream{
			upstreamWithFloor("validator", 300000, 200000, true),
			upstreamWithFloor("archive", 300000, 0, true),
		}
		require.True(t, PoolCoversSlotStart(ups, 1000))
	})

	t.Run("unknown floor does not authorize cover", func(t *testing.T) {
		t.Parallel()
		// Only a KNOWN floor at or below the start authorizes the routing
		// preference. A cold upstream must not keep every below-floor
		// validator skipped — if it times out, the request would exhaust
		// instead of falling back to their valid clamped answers.
		ups := []common.Upstream{
			upstreamWithFloor("validator", 300000, 200000, true),
			upstreamWithFloor("cold", 0, 0, false),
		}
		require.False(t, PoolCoversSlotStart(ups, 1000))
	})

	t.Run("null poller does not authorize cover", func(t *testing.T) {
		t.Parallel()
		ups := []common.Upstream{
			upstreamWithFloor("validator", 300000, 200000, true),
			&slotLagUpstream{id: "null", poller: &pollerAtSlot{null: true, floor: 0, floorKnown: true}},
		}
		require.False(t, PoolCoversSlotStart(ups, 1000))
	})

	t.Run("not covered when every known floor is above", func(t *testing.T) {
		t.Parallel()
		ups := []common.Upstream{
			upstreamWithFloor("v1", 300000, 200000, true),
			upstreamWithFloor("v2", 300000, 250000, true),
		}
		require.False(t, PoolCoversSlotStart(ups, 1000))
	})

	t.Run("empty pool is not covered", func(t *testing.T) {
		t.Parallel()
		require.False(t, PoolCoversSlotStart(nil, 1000))
	})
}

func TestPoolFirstAvailableSlot(t *testing.T) {
	t.Parallel()

	t.Run("min across a fully observed pool", func(t *testing.T) {
		floor, known := PoolFirstAvailableSlot([]common.Upstream{
			upstreamWithFloor("validator", 300000, 200000, true),
			upstreamWithFloor("archive", 300000, 0, true),
		})
		require.True(t, known)
		require.Zero(t, floor, "a request below one floor fails over to the other, so the pool floor is the MIN")
	})

	// MIN over a partial set is not a conservative estimate — it is an
	// over-report in the forbidden direction. With a known validator at 200000
	// and an unknown archive, MIN-over-known would claim history starts at
	// 200000: unknown reading as unavailable.
	t.Run("one unknown floor makes the pool unknown", func(t *testing.T) {
		floor, known := PoolFirstAvailableSlot([]common.Upstream{
			upstreamWithFloor("validator", 300000, 200000, true),
			upstreamWithFloor("archive", 300000, 0, false),
		})
		require.False(t, known)
		require.Zero(t, floor)
	})

	t.Run("empty pool is unknown", func(t *testing.T) {
		floor, known := PoolFirstAvailableSlot(nil)
		require.False(t, known)
		require.Zero(t, floor)
	})

	t.Run("uninspectable upstream makes the pool unknown", func(t *testing.T) {
		floor, known := PoolFirstAvailableSlot([]common.Upstream{
			upstreamWithFloor("validator", 300000, 200000, true),
			&slotLagUpstream{id: "no-poller", poller: nil},
		})
		require.False(t, known)
		require.Zero(t, floor)
	})
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

// The pool-level fast path: a slot below EVERY upstream's floor is refused
// outright, with no upstream call. The per-upstream gate already avoids the
// round trips, but it reaches the verdict one upstream at a time and reports
// an exhausted sweep; this states it once, where the claim belongs.
func TestNetworkPreForward_BelowPoolFloor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	net := &fakeNetwork{}
	pool := []common.Upstream{
		upstreamWithFloor("validator", 300000, 200000, true),
		upstreamWithFloor("archive", 300000, 150000, true),
	}

	t.Run("below every floor is refused with -32009", func(t *testing.T) {
		handled, resp, err := networkPreForward_belowPoolFloor(
			ctx, net, pool, reqOf(t, "getBlock", `[1000]`))
		require.True(t, handled, "must short-circuit before any upstream call")
		require.Nil(t, resp)
		require.Error(t, err)

		// -32009 rather than the -32001 a single skipped upstream carries: the
		// pool verdict asserts long-term storage WAS consulted, which is true
		// precisely because every floor, archive included, is above the slot.
		// It is also what a BigTable-backed validator answers.
		require.Contains(t, err.Error(), "-32009")
		require.Contains(t, err.Error(), "150000", "the message must name the pool floor")
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
		require.True(t, common.IsPermanentlyMissingData(err),
			"a pruned slot never comes back, so no wait-and-retry")
	})

	t.Run("at or above the pool floor falls through to the per-upstream gate", func(t *testing.T) {
		for _, params := range []string{`[150000]`, `[175000]`, `[250000]`} {
			handled, _, err := networkPreForward_belowPoolFloor(
				ctx, net, pool, reqOf(t, "getBlock", params))
			require.NoError(t, err)
			require.Falsef(t, handled, "slot %s is servable by at least one upstream", params)
		}
	})

	t.Run("one unobserved floor fails open", func(t *testing.T) {
		coldPool := []common.Upstream{
			upstreamWithFloor("validator", 300000, 200000, true),
			upstreamWithFloor("archive", 300000, 0, false),
		}
		handled, _, err := networkPreForward_belowPoolFloor(
			ctx, net, coldPool, reqOf(t, "getBlock", `[1000]`))
		require.NoError(t, err)
		require.False(t, handled,
			"no upstream reporting a floor is not evidence that the slot is pruned")
	})

	t.Run("honours the enforcement switch", func(t *testing.T) {
		off := false
		disabled := &fakeNetwork{enforceBlockAvailability: &off}
		handled, _, err := networkPreForward_belowPoolFloor(
			ctx, disabled, pool, reqOf(t, "getBlock", `[1000]`))
		require.NoError(t, err)
		require.False(t, handled)
	})

	// Range queries must NOT be refused: agave answers a start below its floor
	// by clamping to the lowest slot it retains and returning that list, so an
	// error here would be a verdict agave never gives. getBlockTime is excluded
	// too — the per-upstream gate already surfaces agave's own -32001 for it.
	t.Run("only point lookups are short-circuited", func(t *testing.T) {
		cases := []struct{ method, params string }{
			{"getBlocks", `[1000,1010]`},
			{"getBlocksWithLimit", `[1000,10]`},
			{"getBlockTime", `[1000]`},
			{"getTransaction", `["sig"]`},
		}
		for _, tc := range cases {
			t.Run(tc.method, func(t *testing.T) {
				handled, _, err := networkPreForward_belowPoolFloor(
					ctx, net, pool, reqOf(t, tc.method, tc.params))
				require.NoError(t, err)
				require.False(t, handled)
			})
		}
		// The alias must behave exactly like getBlock.
		handled, _, err := networkPreForward_belowPoolFloor(
			ctx, net, pool, reqOf(t, "getConfirmedBlock", `[1000]`))
		require.Error(t, err)
		require.True(t, handled)
	})
}

func TestRequestRangeEndSlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("getBlocks named end", func(t *testing.T) {
		end, ok := RequestRangeEndSlot(ctx, reqOf(t, "getBlocks", `[50, 1000]`), 50, 80)
		require.True(t, ok)
		require.EqualValues(t, 1000, end)
	})

	t.Run("getBlocks omitted end uses tip", func(t *testing.T) {
		end, ok := RequestRangeEndSlot(ctx, reqOf(t, "getBlocks", `[50]`), 50, 80)
		require.True(t, ok)
		require.EqualValues(t, 80, end)
	})

	t.Run("getBlocksWithLimit uses tip, not start+limit", func(t *testing.T) {
		// limit is a confirmed-block COUNT. start+limit would both reject
		// limit=1 against an upper equal to start, and under-bound ranges
		// that skip slots.
		end, ok := RequestRangeEndSlot(ctx, reqOf(t, "getBlocksWithLimit", `[50, 1]`), 50, 80)
		require.True(t, ok)
		require.EqualValues(t, 80, end)

		end, ok = RequestRangeEndSlot(ctx, reqOf(t, "getBlocksWithLimit", `[50, 2000]`), 50, 80)
		require.True(t, ok)
		require.EqualValues(t, 80, end)
	})

	t.Run("getBlocksWithLimit unknown tip fails open", func(t *testing.T) {
		end, ok := RequestRangeEndSlot(ctx, reqOf(t, "getBlocksWithLimit", `[50, 10]`), 50, 0)
		require.False(t, ok)
		require.Zero(t, end)
	})
}
