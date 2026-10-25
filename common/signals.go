package common

import (
	"sync"
	"time"

	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// Invariant is one chain-tracking invariant (spec §15.3) counted by
// erpc_chain_tracker_invariant_violations_total (§17).
type Invariant string

const (
	InvariantSentAboveLatest  Invariant = "sent_above_latest"
	InvariantSentUnconfirmed  Invariant = "sent_unconfirmed"
	InvariantTapGap           Invariant = "tap_gap"
	InvariantRemovedUnsent    Invariant = "removed_unsent"
	InvariantPinsUnlinked     Invariant = "pins_unlinked"
	InvariantFinalHashChanged Invariant = "final_hash_changed"
	InvariantLatestRegressed  Invariant = "latest_regressed"
)

var invariants = []Invariant{
	InvariantSentAboveLatest, InvariantSentUnconfirmed, InvariantTapGap, InvariantRemovedUnsent,
	InvariantPinsUnlinked, InvariantFinalHashChanged, InvariantLatestRegressed,
}

// ExportInvariants creates every invariant series of a network at 0, so a
// network that never violated one still shows it holds.
func ExportInvariants(projectId, network string) {
	for _, inv := range invariants {
		telemetry.CounterHandle(telemetry.MetricChainTrackerInvariantViolationsTotal, projectId, network, string(inv))
	}
}

// ViolateInvariant counts one violation of inv and returns its log line,
// rate limited like every transition; the caller adds the numbers and
// hashes, then sends it.
func ViolateInvariant(l *zerolog.Logger, projectId, network string, inv Invariant) *zerolog.Event {
	telemetry.CounterHandle(telemetry.MetricChainTrackerInvariantViolationsTotal, projectId, network, string(inv)).Inc()
	return TransitionLog(l, zerolog.ErrorLevel, network, "invariant_"+string(inv)).Str("invariant", string(inv))
}

// TransitionLogPeriod is how often one (network, kind) of transition logs
// a line: the first transition of each period does (spec §17); metrics
// count every one.
var TransitionLogPeriod = 10 * time.Second

type transitionKey struct{ network, kind string }

var transitionSamplers sync.Map // transitionKey -> *zerolog.BurstSampler

// TransitionLog is l's event at level for one state transition of kind on
// network, nil (a no-op event) when (network, kind) already logged within
// TransitionLogPeriod.
func TransitionLog(l *zerolog.Logger, level zerolog.Level, network, kind string) *zerolog.Event {
	k := transitionKey{network, kind}
	v, ok := transitionSamplers.Load(k)
	if !ok {
		v, _ = transitionSamplers.LoadOrStore(k, &zerolog.BurstSampler{Burst: 1, Period: TransitionLogPeriod})
	}
	sl := l.Sample(v.(*zerolog.BurstSampler))
	return sl.WithLevel(level).Str("network", network).Str("transition", kind)
}
package common

import (
	"sync"
	"time"

	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// Invariant is one chain-tracking invariant (spec §15.3) counted by
// erpc_chain_tracker_invariant_violations_total (§17).
type Invariant string

const (
	InvariantSentAboveLatest  Invariant = "sent_above_latest"
	InvariantSentUnconfirmed  Invariant = "sent_unconfirmed"
	InvariantTapGap           Invariant = "tap_gap"
	InvariantRemovedUnsent    Invariant = "removed_unsent"
	InvariantPinsUnlinked     Invariant = "pins_unlinked"
	InvariantFinalHashChanged Invariant = "final_hash_changed"
	InvariantLatestRegressed  Invariant = "latest_regressed"
)

var invariants = []Invariant{
	InvariantSentAboveLatest, InvariantSentUnconfirmed, InvariantTapGap, InvariantRemovedUnsent,
	InvariantPinsUnlinked, InvariantFinalHashChanged, InvariantLatestRegressed,
}

// ExportInvariants creates every invariant series of a network at 0, so a
// network that never violated one still shows it holds.
func ExportInvariants(projectId, network string) {
	for _, inv := range invariants {
		telemetry.CounterHandle(telemetry.MetricChainTrackerInvariantViolationsTotal, projectId, network, string(inv))
	}
}

// ViolateInvariant counts one violation of inv and returns its log line,
// rate limited like every transition; the caller adds the numbers and
// hashes, then sends it.
func ViolateInvariant(l *zerolog.Logger, projectId, network string, inv Invariant) *zerolog.Event {
	telemetry.CounterHandle(telemetry.MetricChainTrackerInvariantViolationsTotal, projectId, network, string(inv)).Inc()
	return TransitionLog(l, zerolog.ErrorLevel, network, "invariant_"+string(inv)).Str("invariant", string(inv))
}

// TransitionLogPeriod is how often one (network, kind) of transition logs
// a line: the first transition of each period does (spec §17); metrics
// count every one.
var TransitionLogPeriod = 10 * time.Second

type transitionKey struct{ network, kind string }

var transitionSamplers sync.Map // transitionKey -> *zerolog.BurstSampler

// TransitionLog is l's event at level for one state transition of kind on
// network, nil (a no-op event) when (network, kind) already logged within
// TransitionLogPeriod.
func TransitionLog(l *zerolog.Logger, level zerolog.Level, network, kind string) *zerolog.Event {
	k := transitionKey{network, kind}
	v, ok := transitionSamplers.Load(k)
	if !ok {
		v, _ = transitionSamplers.LoadOrStore(k, &zerolog.BurstSampler{Burst: 1, Period: TransitionLogPeriod})
	}
	sl := l.Sample(v.(*zerolog.BurstSampler))
	return sl.WithLevel(level).Str("network", network).Str("transition", kind)
}
package common

import (
	"sync"
	"time"

	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// Invariant is one chain-tracking invariant (spec §15.3) counted by
// erpc_chain_tracker_invariant_violations_total (§17).
type Invariant string

const (
	InvariantSentAboveLatest  Invariant = "sent_above_latest"
	InvariantSentUnconfirmed  Invariant = "sent_unconfirmed"
	InvariantTapGap           Invariant = "tap_gap"
	InvariantRemovedUnsent    Invariant = "removed_unsent"
	InvariantPinsUnlinked     Invariant = "pins_unlinked"
	InvariantFinalHashChanged Invariant = "final_hash_changed"
	InvariantLatestRegressed  Invariant = "latest_regressed"
)

var invariants = []Invariant{
	InvariantSentAboveLatest, InvariantSentUnconfirmed, InvariantTapGap, InvariantRemovedUnsent,
	InvariantPinsUnlinked, InvariantFinalHashChanged, InvariantLatestRegressed,
}

// ExportInvariants creates every invariant series of a network at 0, so a
// network that never violated one still shows it holds.
func ExportInvariants(projectId, network string) {
	for _, inv := range invariants {
		telemetry.CounterHandle(telemetry.MetricChainTrackerInvariantViolationsTotal, projectId, network, string(inv))
	}
}

// ViolateInvariant counts one violation of inv and returns its log line,
// rate limited like every transition; the caller adds the numbers and
// hashes, then sends it.
func ViolateInvariant(l *zerolog.Logger, projectId, network string, inv Invariant) *zerolog.Event {
	telemetry.CounterHandle(telemetry.MetricChainTrackerInvariantViolationsTotal, projectId, network, string(inv)).Inc()
	return TransitionLog(l, zerolog.ErrorLevel, network, "invariant_"+string(inv)).Str("invariant", string(inv))
}

// TransitionLogPeriod is how often one (network, kind) of transition logs
// a line: the first transition of each period does (spec §17); metrics
// count every one.
var TransitionLogPeriod = 10 * time.Second

type transitionKey struct{ network, kind string }

var transitionSamplers sync.Map // transitionKey -> *zerolog.BurstSampler

// TransitionLog is l's event at level for one state transition of kind on
// network, nil (a no-op event) when (network, kind) already logged within
// TransitionLogPeriod.
func TransitionLog(l *zerolog.Logger, level zerolog.Level, network, kind string) *zerolog.Event {
	k := transitionKey{network, kind}
	v, ok := transitionSamplers.Load(k)
	if !ok {
		v, _ = transitionSamplers.LoadOrStore(k, &zerolog.BurstSampler{Burst: 1, Period: TransitionLogPeriod})
	}
	sl := l.Sample(v.(*zerolog.BurstSampler))
	return sl.WithLevel(level).Str("network", network).Str("transition", kind)
}
