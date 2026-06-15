package evm

import "sort"

// ServedTipInput is a single observation: an upstream's last-known tip block.
// Callers are responsible for excluding syncing or cordoned upstreams BEFORE
// passing observations here; the picker treats every input as a candidate.
type ServedTipInput struct {
	// UpstreamID is preserved only for telemetry attribution. It is not used
	// in the pick.
	UpstreamID string

	// BlockNumber is the upstream's reported tip. Zero or negative values are
	// treated as "no data yet" and filtered before picking.
	BlockNumber int64
}

// ServedTipPick is the picker's output.
type ServedTipPick struct {
	// Tip is the value to advertise as latest/finalized: the highest block
	// number that a strict MAJORITY of the inputs have already reached, or 0
	// when there are no valid inputs.
	Tip int64

	// Freshest is the freshest CORROBORATED view: the 2nd-highest valid input
	// (or the only input when N=1) — the reference for the deliberate-lag
	// gauge (Freshest - Tip). Using the 2nd-highest instead of the raw max
	// means a single rogue far-future upstream cannot inflate the lag gauge
	// (the problem the old velocity gate solved via MaxEligible: one
	// wrong-chain endpoint used to make the gauge read hundreds of thousands
	// of blocks). The absolute per-upstream maxima remain observable via
	// erpc_upstream_latest_block_number.
	Freshest int64

	// Inputs is the number of valid (BlockNumber > 0) observations.
	Inputs int
}

// PickServedTip returns the freshest block number that a strict majority of
// the eligible upstreams have already reached: the floor(N/2)-th highest head
// (0-indexed, descending). This is the entire served-tip algorithm.
//
// One order statistic over the live heads provides every protection the
// previous cluster + velocity-gate + persistent-counter pipeline engineered
// separately — with zero state and zero configuration:
//
//   - GARBAGE-RESISTANT: a far-future tip from a rogue/wrong-chain upstream
//     cannot move the pick unless a strict majority agrees with it.
//   - STUCK-RESISTANT: a frozen or lagging upstream cannot hold the pick
//     back unless it IS the majority (a halted chain — where holding back is
//     the correct answer).
//   - SERVABLE: by construction at least floor(N/2)+1 upstreams already have
//     the advertised block, so interpolated "latest" requests land on
//     upstreams that can actually serve it.
//   - MONOTONIC IN PRACTICE: each input is itself a monotonic, rollback-
//     tolerant poller counter, and an order statistic over monotonic inputs
//     only regresses when the ELIGIBLE SET changes — bounded by the live
//     head spread (a couple of blocks), the same wobble any load-balanced
//     provider exhibits.
//   - WEDGE-IMMUNE: nothing is persisted and nothing is predicted — no
//     inherited counter, no anchor clock, no block-time estimate, no
//     absorbing state. The 2026-06 production incident (served tips silently
//     frozen hours in the past, fleet-wide) is structurally impossible here;
//     networks_served_tip_invariants_test.go (package erpc) pins that class
//     of outcome forever.
//
// Examples (heads descending): N=1 → that head; N=2 → the LOWER (never
// advertise a block only one upstream claims); N=3 → 2nd; N=4 → 3rd; N=5 → 3rd.
func PickServedTip(tips []ServedTipInput) ServedTipPick {
	heads := make([]int64, 0, len(tips))
	for _, t := range tips {
		if t.BlockNumber > 0 {
			heads = append(heads, t.BlockNumber)
		}
	}
	if len(heads) == 0 {
		return ServedTipPick{}
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i] > heads[j] })
	freshest := heads[0]
	if len(heads) > 1 {
		// Corroborated freshest: a single rogue far-future tip must not be
		// able to inflate the lag reference (see ServedTipPick.Freshest).
		freshest = heads[1]
	}
	return ServedTipPick{
		Tip:      heads[len(heads)/2],
		Freshest: freshest,
		Inputs:   len(heads),
	}
}
