package integrity

import (
	"sync"
	"time"
)

// Exhaustion tracking: the all-upstream false-positive detector.
//
// When a check rejects a response AND the request still fails, that rejection
// produced no correction — by definition there was no better response to fail
// over to. One such event is unremarkable (a chain-wide vendor hiccup). But
// repeated exhaustion on the same (network, check) is the **all-upstream
// signature**: the check is rejecting data that every vendor of that chain
// produces, which means it is protocol-invalid for the chain rather than
// catching corruption. Five independent vendors do not corrupt identically.
//
// Every false-positive class this module has shipped showed exactly this
// signature and was diagnosed by a human reading per-upstream metrics hours
// later — during which it cost real client-facing errors (95 in one week from
// transactionsRootConsistency on HyperEVM alone, because a check that rejects
// on ALL upstreams defeats failover entirely).
//
// This tracker only DETECTS and reports. It deliberately does not downgrade or
// disable the check: when every vendor genuinely serves corrupt data, rejecting
// is the correct behaviour and erroring beats serving known-bad data. Choosing
// between "protocol-invalid check" and "everyone is genuinely wrong" is a
// correctness-policy decision for an operator, so the tracker's job is to put
// that decision in front of them in minutes instead of hours.
const (
	// exhaustionWindow is the sliding window over which exhaustions are counted.
	exhaustionWindow = 15 * time.Minute
	// exhaustionThreshold is how many exhaustions inside the window constitute
	// the all-upstream signature rather than isolated bad luck.
	exhaustionThreshold = 10
	// exhaustionRealertAfter throttles repeat reports for a (network, check)
	// that stays broken, so a persistent fault reports periodically instead of
	// on every request.
	exhaustionRealertAfter = time.Hour
)

type exhaustionKey struct {
	network string
	check   string
}

type exhaustionState struct {
	// events holds the timestamps inside the current window (bounded by the
	// threshold — older entries are dropped as they age out).
	events    []time.Time
	lastAlert time.Time
}

var (
	exhaustionMu    sync.Mutex
	exhaustionSeen  = map[exhaustionKey]*exhaustionState{}
	exhaustionClock = time.Now // swapped in tests
)

// RecordExhaustion notes that `check` rejected a response on `network` and the
// request nevertheless failed. It reports whether this event completes the
// all-upstream signature and should be surfaced to an operator, along with the
// number of exhaustions currently inside the window.
//
// Reporting is throttled per (network, check) by exhaustionRealertAfter so a
// persistently protocol-invalid check does not log on every request.
func RecordExhaustion(network, check string) (report bool, count int) {
	if network == "" || check == "" {
		return false, 0
	}
	now := exhaustionClock()
	cutoff := now.Add(-exhaustionWindow)

	exhaustionMu.Lock()
	defer exhaustionMu.Unlock()

	key := exhaustionKey{network: network, check: check}
	st := exhaustionSeen[key]
	if st == nil {
		st = &exhaustionState{}
		exhaustionSeen[key] = st
	}

	// Drop events that have aged out, then record this one.
	kept := st.events[:0]
	for _, t := range st.events {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	st.events = append(kept, now)
	count = len(st.events)

	if count < exhaustionThreshold {
		return false, count
	}
	if !st.lastAlert.IsZero() && now.Sub(st.lastAlert) < exhaustionRealertAfter {
		return false, count
	}
	st.lastAlert = now
	return true, count
}

// ExhaustionWindow exposes the window for callers that report it alongside the
// count, so the log line is self-describing.
func ExhaustionWindow() time.Duration { return exhaustionWindow }

// resetExhaustionForTest clears tracker state between tests.
func resetExhaustionForTest() {
	exhaustionMu.Lock()
	defer exhaustionMu.Unlock()
	exhaustionSeen = map[exhaustionKey]*exhaustionState{}
	exhaustionClock = time.Now
}
