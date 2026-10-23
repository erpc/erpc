package policy_test

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// newTimeoutEngine registers evm:1 with rpc1 and rpc2 and the given eval.
// The ticker never starts, so the only ticks are RegisterNetwork's first
// eval and the ones a test drives with TickForTest.
func newTimeoutEngine(t *testing.T, label, evalFunc string, timeout time.Duration) *policy.Engine {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", time.Minute)
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, nil, nil)
	t.Cleanup(engine.Stop)

	cfg := &common.SelectionPolicyConfig{
		DisableTickerForTest: true,
		EvalTimeout:          common.Duration(timeout),
		EvalFunc:             evalFunc,
	}
	require.NoError(t, cfg.SetDefaults())
	ups := []common.Upstream{&fakeUpstream{id: "rpc1"}, &fakeUpstream{id: "rpc2"}}
	require.NoError(t, engine.RegisterNetwork("evm:1", label, func() []common.Upstream { return ups }, cfg))
	return engine
}

func orderedIDs(e *policy.Engine) []string {
	out := []string{}
	for _, u := range e.GetOrdered("evm:1", "*", "*") {
		out = append(out, u.Id())
	}
	return out
}

func lastDecisionError(t *testing.T, e *policy.Engine) string {
	t.Helper()
	ds := e.RecentDecisions("evm:1", "*", "*", 1)
	require.NotEmpty(t, ds)
	return ds[len(ds)-1].Error
}

func evalTimeouts(label string) float64 {
	return testutil.ToFloat64(telemetry.MetricSelectionEvalErrorsTotal.WithLabelValues("p1", label, "*", "timeout"))
}

// fastThenSlow returns rpc1 alone on its first call, and busy-waits on
// every later call before returning both upstreams.
const fastThenSlow = `(ups, _ctx) => {
	globalThis.__tick = (globalThis.__tick || 0) + 1;
	if (globalThis.__tick === 1) { return ups.filter((u) => u.id === 'rpc1'); }
	const t = Date.now();
	while (Date.now() - t < 300) {}
	return ups;
}`

// sobek cannot interrupt a running eval, so a tick past evalTimeout is
// counted as a timeout and its result still replaces the older ordering.
// Before #1284 the timeout branch raced the eval goroutine for the outcome
// and the late write always won, so no timeout was ever counted.
func TestSlot_EvalTimeout_AppliesTheLateResultAndCountsTheTimeout(t *testing.T) {
	const label = "slot-timeout-applies"
	e := newTimeoutEngine(t, label, fastThenSlow, 100*time.Millisecond)
	require.Equal(t, []string{"rpc1"}, orderedIDs(e))
	before := evalTimeouts(label)

	policy.TickForTest(e, "evm:1", "*")

	require.Equal(t, before+1, evalTimeouts(label))
	require.Empty(t, lastDecisionError(t, e), "a slow eval is not an eval failure")
	require.Equal(t, []string{"rpc1", "rpc2"}, orderedIDs(e), "the late result replaces the older ordering")
}

// The first eval runs inside RegisterNetwork, where a busy host can miss
// the deadline. The slot must not leave the network on raw registration
// order until the next tick.
func TestSlot_EvalTimeout_FirstTickPublishesItsLateResult(t *testing.T) {
	const label = "slot-timeout-first"
	before := evalTimeouts(label)
	e := newTimeoutEngine(t, label, `(ups, _ctx) => {
		const t = Date.now();
		while (Date.now() - t < 300) {}
		return ups.filter((u) => u.id === 'rpc2');
	}`, 20*time.Millisecond)

	require.Equal(t, []string{"rpc2"}, orderedIDs(e))
	require.Equal(t, before+1, evalTimeouts(label))
}

// A first eval that overruns and then throws leaves the slot empty and
// reports the throw, not the timeout.
func TestSlot_EvalTimeout_FirstTickLateThrowPublishesNothing(t *testing.T) {
	const label = "slot-timeout-throw"
	before := evalTimeouts(label)
	e := newTimeoutEngine(t, label, `(ups, _ctx) => {
		const t = Date.now();
		while (Date.now() - t < 300) {}
		throw new Error('boom');
	}`, 20*time.Millisecond)

	require.Empty(t, orderedIDs(e))
	require.Contains(t, lastDecisionError(t, e), "boom")
	require.Equal(t, before+1, evalTimeouts(label))
}

// Many timed-out ticks give the race detector a wide window on the eval
// and the timeout accounting, which used to write the same variable from
// two goroutines.
func TestSlot_EvalTimeout_HasNoRaceOnTheOutcome(t *testing.T) {
	const label = "slot-timeout-race"
	before := evalTimeouts(label)
	e := newTimeoutEngine(t, label, `(ups, _ctx) => {
		const t = Date.now();
		while (Date.now() - t < 30) {}
		return ups;
	}`, time.Millisecond)
	for range 10 {
		policy.TickForTest(e, "evm:1", "*")
	}
	require.Equal(t, before+11, evalTimeouts(label))
	require.Equal(t, []string{"rpc1", "rpc2"}, orderedIDs(e))
}
