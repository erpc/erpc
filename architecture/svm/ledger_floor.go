package svm

import (
	"context"
	"math"

	"github.com/erpc/erpc/common"
)

// lowerBoundSlotMethods are the methods whose FIRST positional param is a slot
// the caller is asking us to read at or above. That is the only request shape
// a declared slot window (svm.blockAvailability) can be evaluated against, and
// it is why this is a set
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
// with the request untouched, which is exactly the behaviour without a window.
//
// Deliberate exclusions, each because a slot window is not the predicate:
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

// rangeSlotMethods are the RANGE members of lowerBoundSlotMethods: the caller
// names a start slot and the node answers with the retained slots at or above
// it, clamping a start below its floor instead of erroring. Used to evaluate a
// declared upper bound against the END of the range, not just its start.
var rangeSlotMethods = map[string]struct{}{
	"getBlocks":          {},
	"getBlocksWithLimit": {},
}

// maxGetBlocksRange is agave's MAX_GET_CONFIRMED_BLOCKS_RANGE: the span an
// open-ended getBlocks covers.
const maxGetBlocksRange = 500_000

// RequestRangeEndSlot extracts the highest slot a range request can touch, as
// opposed to RequestLowerBoundSlot's start. It exists for the declared UPPER
// bound: an operator's window cap is violated by a range whose END runs past
// it even when the start is inside, and a truncated answer from this upstream
// would hide a fuller one from another.
//
//   - getBlocks(start, end?): end is param 1. When omitted, agave ends the
//     range at start+maxGetBlocksRange, capped at its root (rpc.rs get_blocks),
//     so that is the end here, capped by latest when known.
//   - getBlocksWithLimit(start, limit): limit is a COUNT of confirmed blocks,
//     not a slot span. Skipped slots mean the Nth returned block can sit well
//     past start+limit, and limit=1 returns only the block at start (so
//     start+1 would falsely reject a request whose upper equals start). No
//     exact end is derivable from the args; the conservative upper of what
//     could be returned is the upstream's own tip. A missing/invalid limit
//     or unknown tip fails open.
//
// fromTip reports that the end is the tip itself rather than a slot the
// request names: such an end advances with the tip, so a tip-relative bound
// never catches up to it.
//
// Anything not a range method resolves to start itself.
func RequestRangeEndSlot(ctx context.Context, r *common.NormalizedRequest, start, latest int64) (end int64, fromTip bool, ok bool) {
	method, err := r.Method()
	if err != nil {
		return start, false, true
	}
	if _, isRange := rangeSlotMethods[method]; !isRange {
		return start, false, true
	}
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return 0, false, false
	}
	rpcReq.RLock()
	defer rpcReq.RUnlock()

	switch method {
	case "getBlocks":
		if len(rpcReq.Params) >= 2 {
			// Commitment injection appends the options object where an omitted
			// end would sit ([start] becomes [start, {options}]), so a trailing
			// object means "no end named" and the range extends to the tip.
			// Explicit JSON null is the same omitted end ([start, null] or
			// [start, null, {options}]) — do not fail-open past the upper gate.
			if rpcReq.Params[1] != nil {
				if _, isOptions := rpcReq.Params[1].(map[string]interface{}); !isOptions {
					// A present-but-invalid end (fractional, string, negative) is
					// not ours to interpret — fail open and let the node answer
					// -32602.
					named, ok := toInt64Exact(rpcReq.Params[1])
					if !ok || named < 0 {
						return 0, false, false
					}
					return named, false, true
				}
			}
		}
		if limit := start + maxGetBlocksRange; latest <= 0 || limit <= latest {
			return limit, false, true
		}
		return latest, true, true
	case "getBlocksWithLimit":
		if len(rpcReq.Params) < 2 {
			return 0, false, false
		}
		// Validate the limit is a non-negative integer so a malformed call
		// fails open to the node's -32602, but do not treat it as a span:
		// the end is the tip (see comment above).
		if limit, ok := toInt64Exact(rpcReq.Params[1]); !ok || limit < 0 {
			return 0, false, false
		}
		if latest <= 0 {
			return 0, false, false
		}
		return latest, true, true
	}
	return start, false, true
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
