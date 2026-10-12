package svm

import (
	"context"
	"fmt"
	"math"

	"github.com/erpc/erpc/common"
)

// FloorConfidenceLabel is the `confidence` label used on
// MetricUpstreamStaleLowerBound for an SVM floor skip. It answers "how do we
// know?" — the upstream's own getFirstAvailableBlock answer, not a
// head-derived heuristic. Kept a literal rather than a member of the EVM-owned
// AvailbilityConfidence enum: an SVM value there would be a cross-architecture
// commitment for the sake of one label.
const FloorConfidenceLabel = "firstAvailableBlock"

// lowerBoundSlotMethods are the methods whose FIRST positional param is a slot
// the caller is asking us to read at or above. That is the only request shape
// the ledger floor can be evaluated against, and it is why this is a set
// rather than a method→index map: every member's slot is param 0. A future
// method whose slot sits elsewhere must NOT be added here — turn this into an
// index map at that point rather than guessing a position now.
//
// This is deliberately NOT derived from commitmentOptionsIndex. That table
// records where the OPTIONS object goes: param 0 there is a base58 pubkey for
// most entries (getAccountInfo, getBalance, getTokenAccountsByOwner),
// getBlocks is encoded as a sentinel arity rather than a slot position, and
// getBlockTime is excluded from it on purpose because it takes no options
// object at all. Reusing it would be a coincidence, not a rule.
//
// The unmatched path is the primary path: any method not listed falls through
// with the request untouched, which is exactly the behaviour without a floor.
//
// Deliberate exclusions, each because the floor is not the predicate:
//   - getTransaction, getSignaturesForAddress: keyed by signature, so no slot
//     is named. The existing empty-result sweep handles their null.
//   - getBlockCommitment: agave answers old slots with a valid non-error body
//     on every node, floor or no floor.
//   - getSlotLeaders, getLeaderSchedule, getBlockProduction: answered from the
//     leader schedule or restricted to the current epoch, not from retained
//     blocks.
//   - getInflationReward, getStakeActivation: keyed by epoch. Converting epoch
//     to slot needs the epoch schedule — a derived claim about mutable state.
var lowerBoundSlotMethods = map[string]struct{}{
	"getBlock": {},
	// Deprecated alias of getBlock with the same signature, routed to the same
	// handler. Omitting it would reproduce exactly the divergence that
	// getconfirmedblock_alias_test.go exists to prevent.
	"getConfirmedBlock":  {},
	"getBlockTime":       {},
	"getBlocks":          {},
	"getBlocksWithLimit": {},
}

// poolFloorShortCircuitMethods are the methods for which "below every
// upstream's floor" means the answer does not exist anywhere in the pool, so
// the request can be refused outright with no upstream call.
//
// It is a strict subset of lowerBoundSlotMethods, and the two exclusions are
// the point:
//   - getBlocks, getBlocksWithLimit: RANGE queries. agave answers a start
//     below its floor by clamping to the lowest slot it retains and returning
//     that list, so refusing the request would be a verdict agave never gives.
//     The per-upstream gate treats them as a routing PREFERENCE for the same
//     reason: a below-floor upstream is skipped only while some upstream in
//     the pool covers the start slot (see checkSvmUpstreamSlotAvailability);
//     when none does, the request forwards and every upstream clamps — the
//     behaviour as if the floor were unknown.
//   - getBlockTime: agave answers -32001 for a slot below its floor, and the
//     per-upstream gate already surfaces exactly that code once every upstream
//     has been skipped. Restating it as a -32009 pool verdict would change the
//     code a client sees for no gain.
//
// Anything absent from this set forwards, which is the safe default: a new
// slot-named method gets the existing gate, not a refusal chosen for it.
var poolFloorShortCircuitMethods = map[string]struct{}{
	"getBlock":          {},
	"getConfirmedBlock": {},
}

// rangeSlotMethods are the RANGE members of lowerBoundSlotMethods: the caller
// names a start slot and the node answers with the retained slots at or above
// it, clamping a start below its floor instead of erroring. The floor is a
// routing preference for them, never a refusal — see
// checkSvmUpstreamSlotAvailability.
var rangeSlotMethods = map[string]struct{}{
	"getBlocks":          {},
	"getBlocksWithLimit": {},
}

// IsRangeSlotMethod reports whether method answers with a clamped list rather
// than an error when the named start slot is below the node's floor.
func IsRangeSlotMethod(method string) bool {
	_, ok := rangeSlotMethods[method]
	return ok
}

// RequestRangeEndSlot extracts the highest slot a range request can touch, as
// opposed to RequestLowerBoundSlot's start. It exists for the declared UPPER
// bound: an operator's window cap is violated by a range whose END runs past
// it even when the start is inside, and a truncated answer from this upstream
// would hide a fuller one from another.
//
//   - getBlocks(start, end?): end is param 1. When omitted the range extends
//     to the tip, so latest (the upstream's own) stands in for it; unknown
//     latest fails open.
//   - getBlocksWithLimit(start, limit): limit is a COUNT of confirmed blocks,
//     not a slot span. Skipped slots mean the Nth returned block can sit well
//     past start+limit, and limit=1 returns only the block at start (so
//     start+1 would falsely reject a request whose upper equals start). No
//     exact end is derivable from the args; the conservative upper of what
//     could be returned is the upstream's own tip. A missing/invalid limit
//     or unknown tip fails open.
//
// Anything not a range method resolves to start itself.
func RequestRangeEndSlot(ctx context.Context, r *common.NormalizedRequest, start, latest int64) (int64, bool) {
	method, err := r.Method()
	if err != nil {
		return start, true
	}
	if _, isRange := rangeSlotMethods[method]; !isRange {
		return start, true
	}
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return 0, false
	}
	rpcReq.RLock()
	defer rpcReq.RUnlock()

	switch method {
	case "getBlocks":
		if len(rpcReq.Params) >= 2 {
			// Commitment injection appends the options object where an omitted
			// end would sit ([start] becomes [start, {options}]), so a trailing
			// object means "no end named" and the range extends to the tip.
			if _, isOptions := rpcReq.Params[1].(map[string]interface{}); !isOptions {
				// A present-but-invalid end (fractional, string, negative) is
				// not ours to interpret — fail open and let the node answer
				// -32602.
				end, ok := toInt64Exact(rpcReq.Params[1])
				if !ok || end < 0 {
					return 0, false
				}
				return end, true
			}
		}
		if latest <= 0 {
			return 0, false
		}
		return latest, true
	case "getBlocksWithLimit":
		if len(rpcReq.Params) < 2 {
			return 0, false
		}
		// Validate the limit is a non-negative integer so a malformed call
		// fails open to the node's -32602, but do not treat it as a span:
		// the end is the tip (see comment above).
		if limit, ok := toInt64Exact(rpcReq.Params[1]); !ok || limit < 0 {
			return 0, false
		}
		if latest <= 0 {
			return 0, false
		}
		return latest, true
	}
	return start, true
}

// PoolCoversSlotStart reports whether at least one upstream has a KNOWN floor
// at or below slot — i.e. whether the selected pool contains an upstream that
// can serve a range read starting there without clamping. Only a known floor
// authorizes the routing preference: an unknown/null poller does NOT count as
// cover. Otherwise a single cold upstream would keep every below-floor
// validator skipped, and a timeout on that cold upstream would exhaust the
// request instead of falling back to their valid clamped answers. When no
// known upstream covers the start, the gate forwards everywhere so agave
// clamps — the documented fail-open for range methods.
func PoolCoversSlotStart(upstreams []common.Upstream, slot int64) bool {
	for _, u := range upstreams {
		sup, ok := u.(common.SvmUpstream)
		if !ok {
			continue
		}
		poller := sup.SvmStatePoller()
		if poller == nil || poller.IsObjectNull() {
			continue
		}
		floor, known := poller.FirstAvailableSlot()
		if known && floor <= slot {
			return true
		}
	}
	return false
}

// RequestLowerBoundSlot extracts the slot a request asks us to read at or
// above, when the method names one positionally.
//
// Two invariants this relies on:
//   - Commitment injection never moves param 0. It runs earlier, at the
//     project layer, and only appends a trailing object or mutates an existing
//     one; for getBlocks with empty params it explicitly refuses to displace
//     the required start slot.
//   - No string coercion. toInt64 handles numeric JSON only, so a client
//     sending "12345" falls through and the node answers -32602 honestly.
//     MinContextSlotOf accepts strings because that field lives inside an
//     options object where clients are sloppier; a positional slot is spec'd
//     as u64.
func RequestLowerBoundSlot(ctx context.Context, r *common.NormalizedRequest) (int64, bool) {
	if r == nil {
		return 0, false
	}
	method, err := r.Method()
	if err != nil {
		return 0, false
	}
	if _, ok := lowerBoundSlotMethods[method]; !ok {
		return 0, false
	}
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return 0, false
	}
	rpcReq.RLock()
	defer rpcReq.RUnlock()

	if len(rpcReq.Params) == 0 {
		return 0, false
	}
	// Slot 0 is IN SCOPE and must be accepted: getBlocks(0, 100) against a
	// validator whose floor is 200000 is precisely the silently-short-list
	// case. networkPreForward_getBlock's `slot <= 0` guard is safe there only
	// because slot 0 can never be ABOVE the tip.
	slot, ok := toInt64Exact(rpcReq.Params[0])
	if !ok || slot < 0 {
		return 0, false
	}
	return slot, true
}

// toInt64Exact is toInt64 restricted to values that ARE int64s. Sonic decodes
// every JSON number as float64, so integral slots arrive here too — but so
// does 199999.9, which is NOT slot 199999. Truncating it would let the floor
// and declared-bounds gates reject (or admit) a slot the client never named,
// pre-empting the node's own -32602. NaN/Inf and magnitudes at or beyond 2^63
// cannot convert to int64 at all. Anything not exactly an int64 is not a slot
// for these gates: fail open and let the upstream validate it.
//
// Scoped to the floor code on purpose: the shared toInt64 also feeds the
// pre-existing upper-bound guard and minContextSlot, whose numeric handling
// is not this change's business.
func toInt64Exact(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) ||
			n >= 9.223372036854776e18 || n <= -9.223372036854776e18 {
			return 0, false
		}
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

// CheckUpstreamLedgerFloor reports an error when the upstream's ledger floor is
// above the slot the request names, meaning this upstream provably cannot serve
// it. It returns nil on every unknown: no floor observed, no slot named, or a
// non-SVM upstream. Absence of a floor observation is not evidence of absent
// history.
//
// The error is ErrEndpointMissingData carrying agave's own -32001
// (BlockCleanedUp), marked permanently missing. That combination is
// load-bearing:
//
//   - -32001 is agave's code for exactly this condition, and its message shape
//     ("...First available block: N") is what Solana clients parse. -32009
//     would over-claim, since it asserts long-term storage was checked.
//   - Wrapping in ErrJsonRpcExceptionInternal is what preserves the numeric
//     code to the client. Without it the client sees -32603.
//   - Marking it permanent suppresses the network executor's time-delayed
//     re-sweep. Waiting cannot lower a floor.
func CheckUpstreamLedgerFloor(ctx context.Context, u common.Upstream, r *common.NormalizedRequest) error {
	slot, named := RequestLowerBoundSlot(ctx, r)
	if !named {
		return nil
	}
	sup, ok := u.(common.SvmUpstream)
	if !ok {
		return nil
	}
	poller := sup.SvmStatePoller()
	if poller == nil || poller.IsObjectNull() {
		return nil
	}
	floor, known := poller.FirstAvailableSlot()
	if !known || slot >= floor {
		return nil
	}
	err := common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(
			svmCodeBlockCleanedUp,
			common.JsonRpcErrorNumber(svmCodeBlockCleanedUp),
			fmt.Sprintf("Slot %d cleaned up, does not exist on upstream %s. First available block: %d",
				slot, u.Id(), floor),
			nil, nil,
		),
		u,
	)
	if me, ok := err.(*common.ErrEndpointMissingData); ok {
		me.WithPermanentMissingData(true)
	}
	return err
}

// PoolFirstAvailableSlot returns the pool's ledger floor: the MIN across
// upstreams, because a request below one upstream's floor fails over to
// another. That is the same "never deny data some upstream has" rule that makes
// the upper guard use MAX.
//
// known=false unless EVERY upstream's floor is observed. A MIN over a partial
// set is not a conservative estimate, it is an over-report in the forbidden
// direction: with a known validator at 200000 and an unknown archive,
// MIN-over-known says "history starts at 200000" — unknown reading as
// unavailable. An upstream we cannot inspect counts as unknown for the same
// reason.
func PoolFirstAvailableSlot(upstreams []common.Upstream) (int64, bool) {
	if len(upstreams) == 0 {
		return 0, false
	}
	minSlot := int64(math.MaxInt64)
	for _, u := range upstreams {
		sup, ok := u.(common.SvmUpstream)
		if !ok {
			return 0, false
		}
		poller := sup.SvmStatePoller()
		if poller == nil || poller.IsObjectNull() {
			return 0, false
		}
		slot, known := poller.FirstAvailableSlot()
		if !known {
			return 0, false
		}
		if slot < minSlot {
			minSlot = slot
		}
	}
	return minSlot, true
}

// networkPreForward_belowPoolFloor short-circuits a point lookup for a slot
// that sits below EVERY upstream's ledger floor, with no upstream call at all.
//
// The per-upstream gate already avoids the wasted round trips — it skips each
// upstream before forwarding — but it reaches that verdict one upstream at a
// time and reports it as an exhausted sweep. This states it once, at the pool
// level, which is where the claim actually belongs: no upstream retains the
// slot, so nothing in this pool can ever answer it.
//
// The code is -32009 (LongTermStorageSlotSkipped), not the -32001 the
// per-upstream skip carries. The distinction is scope. -32001 is "THIS node
// pruned it", which is what a single skipped upstream knows. -32009 is
// "skipped, or missing in long-term storage", which asserts that long-term
// storage was consulted — true here precisely because the floor of every
// upstream, archive included, is above the slot. It is also what a
// BigTable-backed validator answers for the same request, so a client
// migrating onto eRPC sees the code it already handles.
//
// Permanent via newSweptSkipMissingData: a pruned slot never comes back, so
// the network executor must not enter its wait-and-retry loop.
//
// Scoped by poolFloorShortCircuitMethods rather than by the caller's switch,
// so the default for any method not named there is to forward — the safe
// direction.
//
// Fails open when the pool floor is unknown. PoolFirstAvailableSlot requires
// every upstream's floor to be observed, so a single cold poller leaves the
// request untouched — no upstream reporting a floor is not evidence that the
// slot is pruned.
func networkPreForward_belowPoolFloor(ctx context.Context, n common.Network, upstreams []common.Upstream, r *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	svmNet, ok := n.(common.SvmNetwork)
	if !ok || !svmNet.SvmEnforceBlockAvailability() {
		return false, nil, nil
	}
	method, err := r.Method()
	if err != nil {
		return false, nil, nil
	}
	if _, eligible := poolFloorShortCircuitMethods[method]; !eligible {
		return false, nil, nil
	}
	slot, named := RequestLowerBoundSlot(ctx, r)
	if !named {
		return false, nil, nil
	}
	floor, known := PoolFirstAvailableSlot(upstreams)
	if !known || slot >= floor {
		return false, nil, nil
	}
	return true, nil, newSweptSkipMissingData(
		svmCodeLongTermStorageSlotSkipped,
		fmt.Sprintf("slot %d is below the provider pool history floor (earliest available: %d)", slot, floor),
		nil, nil,
	)
}
