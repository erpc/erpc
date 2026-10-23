package common

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ServedTipPick is the picker's output.
type ServedTipPick struct {
	// Tip is the value to advertise as latest/finalized: the highest block
	// number that at least quorum of the inputs have already reached (all of
	// them when there are fewer inputs than the quorum), or 0 when there are
	// no valid inputs.
	Tip int64

	// Freshest is the freshest CORROBORATED view: the 2nd-highest valid input
	// (or the only input when N=1) — the reference for the deliberate-lag
	// gauge (Freshest - Tip). Using the 2nd-highest instead of the raw max
	// means a single rogue far-future upstream cannot inflate the lag gauge.
	// The absolute per-upstream maxima remain observable via
	// erpc_upstream_latest_block_number.
	Freshest int64

	// Max is the highest valid input: the raw ceiling no pick may exceed. It is
	// deliberately not corroborated — a ceiling a single rogue can only RAISE
	// cannot be used to make a caller serve anything the rogue chose — and it
	// is the one place a lone far-ahead head is still visible to callers.
	Max int64

	// Inputs is the number of valid (> 0) heads.
	Inputs int
}

// ServedTipQuorum is how many voters must already have a block before it is
// served: a fixed count (>= 1), or ServedTipQuorumMajority for floor(N/2)+1 of
// the N voters. The zero value is DefaultServedTipQuorum. In YAML/JSON it is a
// positive integer or the string "majority".
type ServedTipQuorum int

// ServedTipQuorumMajority requires a strict majority of the voters.
const ServedTipQuorumMajority ServedTipQuorum = -1

// Of returns the quorum for a ballot of n voters, capped at n so a ballot with
// fewer voters than the quorum serves the block all of them have.
func (q ServedTipQuorum) Of(n int) int {
	need := DefaultServedTipQuorum
	switch {
	case q == ServedTipQuorumMajority:
		need = n/2 + 1
	case q > 0:
		need = int(q)
	}
	return max(1, min(need, n))
}

func parseServedTipQuorum(s string) (ServedTipQuorum, error) {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "majority") {
		return ServedTipQuorumMajority, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("servedTip.quorum must be an integer >= 1 or \"majority\", got %q", s)
	}
	return ServedTipQuorum(n), nil
}

func (q ServedTipQuorum) MarshalYAML() (interface{}, error) {
	if q == ServedTipQuorumMajority {
		return "majority", nil
	}
	return int(q), nil
}

func (q *ServedTipQuorum) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var n int
	if err := unmarshal(&n); err == nil {
		return q.set(strconv.Itoa(n))
	}
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	return q.set(s)
}

func (q ServedTipQuorum) MarshalJSON() ([]byte, error) {
	if q == ServedTipQuorumMajority {
		return []byte(`"majority"`), nil
	}
	return []byte(strconv.Itoa(int(q))), nil
}

func (q *ServedTipQuorum) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	return q.set(strings.Trim(string(data), `"`))
}

func (q *ServedTipQuorum) set(s string) error {
	parsed, err := parseServedTipQuorum(s)
	if err != nil {
		return err
	}
	*q = parsed
	return nil
}

// PickServedTip returns the freshest block number that at least q of the
// eligible upstreams have already reached: the q-th highest head (all of them
// when fewer than q voted). This is the entire served-tip algorithm.
//
// heads are the voters' heads; callers exclude syncing, cordoned or benched
// upstreams BEFORE passing them, and heads <= 0 ("no data yet") are ignored.
// It sorts heads in place, so a caller's ballot pays no copy.
//
// One order statistic over the live heads provides every protection a
// stateful pipeline would have to engineer separately — with zero state:
//
//   - GARBAGE-RESISTANT: a far-future tip from a rogue/wrong-chain upstream
//     cannot move the pick unless q-1 others agree with it.
//   - SERVABLE: at least q upstreams already have the advertised block, so
//     interpolated "latest" requests land on upstreams that can serve it.
//   - MONOTONIC IN PRACTICE: each input is itself a monotonic, rollback-
//     tolerant poller counter, and an order statistic over monotonic inputs
//     only regresses when the ELIGIBLE SET changes — bounded by the live
//     head spread.
//   - WEDGE-IMMUNE: nothing is persisted and nothing is predicted. A
//     frozen-tip regression (served tips silently stuck hours in the past)
//     is structurally impossible here;
//     networks_served_tip_invariants_test.go (package erpc) pins that class
//     of outcome forever.
//
// PAIR RULE: two voters cannot corroborate anything — the lower head is one
// upstream's claim exactly as the higher one is — so a pick that needs both
// (q >= 2) would only ever pin "latest" to whichever of the two trails, and a
// node a few blocks behind would hold every reader of "latest" (eth_blockNumber,
// eth_call interpolation) at its height for as long as it is not benched
// (erpc#1253). With exactly two valid heads at most pairLead apart, the higher
// head is served and is the Freshest view; a wider gap is another chain or a
// garbage head, not a lead, and the lower head still wins. pairLead <= 0
// disables the rule.
//
// A voter that stopped moving holds the pick back only while it is needed for
// the quorum; the health tracker benches such a voter (see
// health.Tracker.IsHeadBenched) so it leaves the ballot instead.
//
// Examples (heads descending), default q=2: N=1 → that head; N=2 → the higher
// when within pairLead, else the lower; N=3..∞ → 2nd. Majority: N=3 → 2nd;
// N=4 → 3rd; N=5 → 3rd.
func PickServedTip(heads []int64, q ServedTipQuorum, pairLead int64) ServedTipPick {
	slices.Sort(heads)
	first, _ := slices.BinarySearch(heads, 1)
	valid := heads[first:]
	n := len(valid)
	if n == 0 {
		return ServedTipPick{}
	}
	tip := valid[n-q.Of(n)]
	freshest := valid[max(n-2, 0)]
	if n == 2 && tip < valid[1] && valid[1]-valid[0] <= pairLead {
		tip, freshest = valid[1], valid[1]
	}
	return ServedTipPick{
		Tip:      tip,
		Freshest: freshest,
		Max:      valid[n-1],
		Inputs:   n,
	}
}
