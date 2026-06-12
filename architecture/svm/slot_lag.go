package svm

import "github.com/erpc/erpc/common"

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
