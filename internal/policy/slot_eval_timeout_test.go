package policy_test

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// newTimeoutEngine registers evm:1 with rpc1 and rpc2 and the given eval.
// The ticker never starts, so the only ticks are RegisterNetwork's first
// eval and the ones a test drives with TickForTest.
func newTimeoutEngine(t *testing.T, evalFunc string, timeout time.Duration) *policy.Engine {
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
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))
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

// fastThenSlow returns rpc1 alone on its first call, and busy-waits on
// every later call before returning both upstreams.
const fastThenSlow = `(ups, _ctx) => {
	globalThis.__tick = (globalThis.__tick || 0) + 1;
	if (globalThis.__tick === 1) { return ups.filter((u) => u.id === 'rpc1'); }
	const t = Date.now();
	while (Date.now() - t < 300) {}
	return ups;
}`

// Once the slot holds an ordering, a timed-out tick keeps it, and the
// timeout reaches the decision. Before, the late result overwrote the
// timeout error, so the slow result was published and no timeout was ever
// recorded or counted.
func TestSlot_EvalTimeout_KeepsThePreviousOrderAndRecordsTheTimeout(t *testing.T) {
	e := newTimeoutEngine(t, fastThenSlow, 100*time.Millisecond)
	require.Equal(t, []string{"rpc1"}, orderedIDs(e))

	policy.TickForTest(e, "evm:1", "*")

	require.Contains(t, lastDecisionError(t, e), "selection policy eval timed out")
	require.Equal(t, []string{"rpc1"}, orderedIDs(e),
		"a timed-out tick must not publish its late result over an existing ordering")
}

// The first eval runs inside RegisterNetwork, where a busy host can miss
// the deadline. With no ordering yet, discarding the late result would
// leave the network on raw registration order until the next tick. The
// slot publishes it, and the decision still records the timeout.
func TestSlot_EvalTimeout_FirstTickPublishesItsLateResult(t *testing.T) {
	e := newTimeoutEngine(t, `(ups, _ctx) => {
		const t = Date.now();
		while (Date.now() - t < 300) {}
		return ups.filter((u) => u.id === 'rpc2');
	}`, 20*time.Millisecond)

	require.Equal(t, []string{"rpc2"}, orderedIDs(e))
	require.Contains(t, lastDecisionError(t, e), "selection policy eval timed out")
}

// Only a valid late result is published: a first eval that overruns and
// then throws leaves the slot empty.
func TestSlot_EvalTimeout_FirstTickLateThrowPublishesNothing(t *testing.T) {
	e := newTimeoutEngine(t, `(ups, _ctx) => {
		const t = Date.now();
		while (Date.now() - t < 300) {}
		throw new Error('boom');
	}`, 20*time.Millisecond)

	require.Empty(t, orderedIDs(e))
	require.Contains(t, lastDecisionError(t, e), "selection policy eval timed out")
}

// Many timed-out ticks give the race detector a wide window on the eval
// goroutine and the timeout branch, which used to write the same variable.
func TestSlot_EvalTimeout_HasNoRaceOnTheOutcome(t *testing.T) {
	e := newTimeoutEngine(t, `(ups, _ctx) => {
		const t = Date.now();
		while (Date.now() - t < 30) {}
		return ups;
	}`, time.Millisecond)
	for i := 0; i < 10; i++ {
		policy.TickForTest(e, "evm:1", "*")
	}
	require.Contains(t, lastDecisionError(t, e), "selection policy eval timed out")
}
