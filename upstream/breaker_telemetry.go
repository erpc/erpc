package upstream

import (
	"strings"
	"sync"

	"github.com/erpc/erpc/failsafe"
	"github.com/erpc/erpc/telemetry"
)

// circuitBreakerStateValue maps a breaker state onto the numeric
// encoding published by `erpc_upstream_circuit_breaker_state`.
//
// The switch is explicit on purpose: those numbers are a public
// contract (dashboards and alerts compare against them), so they must
// not move if the `failsafe.State` iota order is ever reshuffled. The
// default arm is unreachable for the current three-state machine; it
// returns a value that cannot be mistaken for a real state so a future
// unmapped state shows up on the dashboard instead of silently reading
// as "closed".
func circuitBreakerStateValue(s failsafe.State) float64 {
	switch s {
	case failsafe.StateClosed:
		return 0
	case failsafe.StateOpen:
		return 1
	case failsafe.StateHalfOpen:
		return 2
	}
	return -1
}

// breakerScopeLabels renders the failsafe entry that owns a breaker as
// the `category` / `finality` label pair. An upstream may configure
// several entries, each with its own independent breaker, so the pair
// is what keeps their gauges from overwriting one another. "*" means
// "matches anything" — the same wildcard the executor itself uses for
// an unset matchMethod.
func breakerScopeLabels(e *upstreamExecutor) (category string, finality string) {
	category, finality = "*", "*"
	if e == nil {
		return
	}
	if m := e.MatchMethod(); m != "" {
		category = m
	}
	if fs := e.MatchFinality(); len(fs) > 0 {
		parts := make([]string, 0, len(fs))
		for _, f := range fs {
			parts = append(parts, f.String())
		}
		finality = strings.Join(parts, "|")
	}
	return
}

// breakerStateReporter is the telemetry adapter for exactly one
// breaker instance. It owns both circuit-breaker metric emissions so
// the counter and the gauge can never disagree about which upstream
// they describe: identity labels are read from the live *Upstream at
// emission time, because vendor name and network label are only
// resolved after the upstream is constructed and bootstrapped.
type breakerStateReporter struct {
	upstream *Upstream
	breaker  *failsafe.Breaker
	category string
	finality string

	// mu serialises publish so two transition callbacks cannot write the
	// gauge out of order — see publish.
	mu sync.Mutex
}

// newBreakerStateReporter returns nil when the executor has no breaker.
func newBreakerStateReporter(u *Upstream, e *upstreamExecutor) *breakerStateReporter {
	if u == nil || e == nil {
		return nil
	}
	b := e.Breaker()
	if b == nil {
		return nil
	}
	category, finality := breakerScopeLabels(e)
	return &breakerStateReporter{
		upstream: u,
		breaker:  b,
		category: category,
		finality: finality,
	}
}

// publish writes the breaker's CURRENT state to the gauge.
//
// It deliberately reads `breaker.State()` rather than trusting the `to`
// of the transition that triggered it: `OnTransition` callbacks are
// fired on their own goroutine, so the callback for an older transition
// can reach us after a newer one and would pin the gauge to a stale
// state. Taking the reading under the same mutex that guards the write
// means whichever call publishes last also holds the freshest reading,
// so the gauge converges on the true state.
func (r *breakerStateReporter) publish() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	telemetry.MetricUpstreamCircuitBreakerState.WithLabelValues(
		r.upstream.ProjectId,
		r.upstream.VendorName(),
		r.upstream.NetworkLabel(),
		r.upstream.Id(),
		r.category,
		r.finality,
	).Set(circuitBreakerStateValue(r.breaker.State()))
}

// onTransition is wired to failsafe.Breaker.OnTransition. It emits the
// `upstream_breaker_state_change_total` counter (unchanged behaviour)
// and refreshes the state gauge.
func (r *breakerStateReporter) onTransition(from failsafe.State, to failsafe.State, _ string) {
	if r == nil {
		return
	}
	telemetry.MetricUpstreamBreakerStateChange.WithLabelValues(
		r.upstream.ProjectId,
		r.upstream.Id(),
		from.String()+"_to_"+to.String(),
	).Inc()
	r.publish()
}

// wireBreakerTelemetry attaches a reporter to every configured breaker
// of this upstream. Called once at construction time, before the
// upstream serves traffic.
func (u *Upstream) wireBreakerTelemetry() {
	if u == nil {
		return
	}
	for _, fe := range u.failsafeExecutors {
		r := newBreakerStateReporter(u, fe)
		if r == nil {
			continue
		}
		u.breakerReporters = append(u.breakerReporters, r)
		fe.Breaker().OnTransition = r.onTransition
	}
}

// publishBreakerStates seeds the state gauge for every configured
// breaker so a breaker that has never transitioned still reports its
// (closed) state instead of being absent from /metrics.
//
// It is called once the upstream's network is known, since `network` is
// one of the gauge's labels — seeding earlier would strand a permanent
// `network="n/a"` series next to the real one. Emitting the gauge
// directly (rather than faking a transition) keeps the transition
// counter untouched.
func (u *Upstream) publishBreakerStates() {
	if u == nil {
		return
	}
	for _, r := range u.breakerReporters {
		r.publish()
	}
}
