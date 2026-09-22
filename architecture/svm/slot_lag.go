package svm

import (
	"context"
	"strconv"

	"github.com/erpc/erpc/common"
)

// FilterByFinalizedSlotLag returns the subset of upstreams whose finalized slot
// is within maxLag of the given reference slot. Intended for consensus-eligible
// requests (finalized commitment + consensus policy active) where including an
// upstream that trails the cluster could poison the vote — the same query sent
// to a stale upstream and a current one returns different answers for data the
// caller has promised is immutable.
//
// Upstream eligibility rules:
//   - Upstreams without an SvmStatePoller are INCLUDED. Bootstrap hasn't finished
//     yet; excluding them would break forwarding for newly-registered upstreams.
//   - Upstreams whose state poller reports 0 finalized slot are INCLUDED. The
//     poller hasn't seen a successful tick yet and we can't tell whether it's
//     trailing or simply hasn't woken up.
//   - Upstreams whose finalized slot is more than maxLag behind referenceSlot
//     are EXCLUDED.
//
// When maxLag <= 0, filtering is disabled and all upstreams pass through.
// When the input list is empty, the output is also empty.
//
// The function is defensive: a nil poller, wrong type, or unwritten slot never
// causes exclusion — we'd rather include a fresh upstream than drop everyone
// and deadlock the request.
func FilterByFinalizedSlotLag(upstreams []common.Upstream, maxLag, referenceSlot int64) []common.Upstream {
	if maxLag <= 0 || referenceSlot <= 0 || len(upstreams) == 0 {
		// No meaningful threshold or reference → pass through.
		return upstreams
	}

	filtered := make([]common.Upstream, 0, len(upstreams))
	for _, u := range upstreams {
		if !isSlotWithinLag(u, maxLag, referenceSlot) {
			continue
		}
		filtered = append(filtered, u)
	}

	// Defensive fallback: if the filter would eliminate every upstream,
	// return the original list. Better to serve potentially-stale data than
	// fail consensus with an empty candidate set — the failsafe consensus
	// policy is the correct layer to detect divergence.
	if len(filtered) == 0 {
		return upstreams
	}
	return filtered
}

// isSlotWithinLag returns true when the upstream should pass the filter.
// Split out so FilterByFinalizedSlotLag reads as a pure filter expression and
// the defensive cases have one obvious place to live.
func isSlotWithinLag(u common.Upstream, maxLag, referenceSlot int64) bool {
	sup, ok := u.(common.SvmUpstream)
	if !ok {
		return true // non-SVM upstream shouldn't be filtered by this rule
	}
	poller := sup.SvmStatePoller()
	if poller == nil || poller.IsObjectNull() {
		return true // no poller yet → too early to judge
	}
	finalized := poller.FinalizedSlot()
	if finalized <= 0 {
		return true // poller hasn't observed a slot yet
	}
	return (referenceSlot - finalized) <= maxLag
}

// HighestFinalizedSlot returns the maximum FinalizedSlot across the upstreams,
// or 0 if none report a positive value. Convenience accessor for callers that
// want to derive referenceSlot from the current pool.
func HighestFinalizedSlot(upstreams []common.Upstream) int64 {
	var max int64
	for _, u := range upstreams {
		sup, ok := u.(common.SvmUpstream)
		if !ok {
			continue
		}
		poller := sup.SvmStatePoller()
		if poller == nil || poller.IsObjectNull() {
			continue
		}
		if s := poller.FinalizedSlot(); s > max {
			max = s
		}
	}
	return max
}

// ReferenceFinalizedSlot derives the reference slot the consensus slot-lag
// prefilter measures against. Plain pool-max is poisonable: a single upstream
// reporting a wildly inflated finalized slot becomes the reference, every
// honest upstream trails it by more than maxLag, and the filter (before its
// all-filtered fallback) can shrink the pool to just the liar. Clamp: when the
// leader outruns the runner-up by more than maxLag, use the runner-up as the
// reference — the leader still passes (the filter only drops trailers), but it
// can no longer drag the bar above the honest pack.
//
// ponytail: second-highest clamp defends a single liar; colluding upstreams
// need a majority/vote-based baseline.
func ReferenceFinalizedSlot(upstreams []common.Upstream, maxLag int64) int64 {
	var first, second int64 // first >= second, both 0 when unreported
	for _, u := range upstreams {
		sup, ok := u.(common.SvmUpstream)
		if !ok {
			continue
		}
		poller := sup.SvmStatePoller()
		if poller == nil || poller.IsObjectNull() {
			continue
		}
		s := poller.FinalizedSlot()
		if s <= 0 {
			continue
		}
		if s > first {
			second = first
			first = s
		} else if s > second {
			second = s
		}
	}
	if maxLag > 0 && second > 0 && first-second > maxLag {
		return second
	}
	return first
}

// FilterByMinContextSlot excludes upstreams that are known to be behind the
// request's minContextSlot at the relevant commitment — forwarding to them is
// a guaranteed -32016 round-trip. Same defensive posture as
// FilterByFinalizedSlotLag: unknown state (no poller, zero slot, non-SVM)
// never excludes, and if every upstream would be excluded the original list is
// returned so the -32016 failover path (rather than an empty pool) reports the
// truth.
func FilterByMinContextSlot(upstreams []common.Upstream, minContextSlot int64, finalized bool) []common.Upstream {
	if minContextSlot <= 0 || len(upstreams) == 0 {
		return upstreams
	}
	filtered := make([]common.Upstream, 0, len(upstreams))
	for _, u := range upstreams {
		if !isAtOrAheadOfSlot(u, minContextSlot, finalized) {
			continue
		}
		filtered = append(filtered, u)
	}
	if len(filtered) == 0 {
		return upstreams
	}
	return filtered
}

// isAtOrAheadOfSlot returns true when the upstream should pass the
// minContextSlot filter. The comparison slot matches the request's
// commitment: a finalized-commitment request needs the node's *finalized*
// slot at minContextSlot, anything weaker needs only the processed tip.
func isAtOrAheadOfSlot(u common.Upstream, minContextSlot int64, finalized bool) bool {
	sup, ok := u.(common.SvmUpstream)
	if !ok {
		return true
	}
	poller := sup.SvmStatePoller()
	if poller == nil || poller.IsObjectNull() {
		return true
	}
	var slot int64
	if finalized {
		slot = poller.FinalizedSlot()
	} else {
		slot = poller.LatestSlot()
	}
	if slot <= 0 {
		return true // no observation yet → too early to judge
	}
	return slot >= minContextSlot
}

// MinContextSlotOf extracts the caller-supplied minContextSlot from the
// request params, or 0 when absent/unparseable. Mirrors the scan the SVM
// cache uses for slot partitioning (any object param carrying the field).
func MinContextSlotOf(ctx context.Context, r *common.NormalizedRequest) int64 {
	if r == nil {
		return 0
	}
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return 0
	}
	rpcReq.RLock()
	defer rpcReq.RUnlock()
	for _, p := range rpcReq.Params {
		m, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		v, ok := m["minContextSlot"]
		if !ok {
			continue
		}
		if n, ok := toInt64(v); ok && n > 0 {
			return n
		}
		if s, ok := v.(string); ok {
			if n, perr := strconv.ParseInt(s, 10, 64); perr == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}
