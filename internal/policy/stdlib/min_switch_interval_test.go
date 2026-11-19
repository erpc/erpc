package stdlib_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// `minSwitchInterval` is the only duration an operator hands the std-lib, and
// the Go side parses it. Every spelling ends up as one number of milliseconds,
// and that number decides one thing: whether a degraded primary keeps the
// traffic for another cooldown window, or the challenger takes it over on the
// very next tick. These tests drive the whole engine and read the routing
// outcome, because the parse result is only interesting through the switch.

// stickyCooldownFixture registers two upstreams under an eval that sorts by
// score and then applies stickyPrimary with the caller-supplied
// `minSwitchInterval` literal. It returns the engine after the first tick has
// elected `aaa`, plus the tracker so the caller can degrade `aaa`.
func stickyCooldownFixture(t *testing.T, minSwitchIntervalJS string) (*policy.Engine, *health.Tracker, []common.Upstream, func()) {
	t.Helper()
	eval := fmt.Sprintf(`(upstreams, ctx) => upstreams
		.sortByScore(PREFER_FASTEST)
		.stickyPrimary({ hysteresis: 0.10, minSwitchInterval: %s })`, minSwitchIntervalJS)

	engine, _, tracker, cancel := newTestEngine(t, eval)
	ups := mkUps("aaa", "bbb")
	cfg := &common.SelectionPolicyConfig{
		EvalInterval: common.Duration(0),
		EvalTimeout:  common.Duration(50 * time.Millisecond),
		EvalFunc:     eval,
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// `aaa` is clean and fast, `bbb` is clean but slow. The first tick elects
	// `aaa` and seeds the sticky register with that moment.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		tracker.RecordUpstreamRequest(ups[1], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[1], "*", 200*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, "aaa", engine.GetOrdered("evm:1", "*", "*")[0].Id(),
		"the fast upstream must win the first tick, or the cooldown test has no incumbent")

	return engine, tracker, ups, func() {
		engine.Stop()
		cancel()
	}
}

// degradeIncumbent flips the health picture: `aaa` slows to two seconds while
// `bbb` speeds up to five milliseconds. That leaves `bbb` roughly 28 times the
// score of `aaa` — far past the 10% hysteresis margin, so only the cooldown can
// still hold the traffic on `aaa`.
func degradeIncumbent(tracker *health.Tracker, ups []common.Upstream) {
	for i := 0; i < 300; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 2000*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		tracker.RecordUpstreamRequest(ups[1], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[1], "*", 5*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
}

// TestStickyPrimary_MinSwitchInterval_DecidesTheHandover walks every spelling an
// operator can write. A spelling the Go parser can read buys the incumbent the
// cooldown it names; a spelling it cannot read buys the same cooldown that
// omitting the key buys, which is 30 seconds.
//
// An unreadable spelling must not become 0 ms. A 0 ms cooldown makes the
// guard `(ctx.now - lastSwitchAt) < minSwitchMs` always false, so every tick
// re-decides the primary on the score gap alone. The operator who wrote the
// knob wrongly would then get less protection than one who left it out.
func TestStickyPrimary_MinSwitchInterval_DecidesTheHandover(t *testing.T) {
	for _, tc := range []struct {
		name    string
		js      string
		primary string
		why     string
	}{
		{
			name: "DurationString", js: `'30s'`, primary: "aaa",
			why: "30s parses to 30000 ms, so the cooldown holds the degraded incumbent",
		},
		{
			name: "MillisecondString", js: `'30000ms'`, primary: "aaa",
			why: "the ms suffix parses the same as the seconds form",
		},
		{
			name: "WholeNumber", js: `30000`, primary: "aaa",
			why: "a bare number is already milliseconds and must hold just like '30s'",
		},
		{
			name: "FractionalNumber", js: `30000.5`, primary: "aaa",
			why: "a fractional millisecond count truncates, it does not degrade to no cooldown",
		},
		{
			name: "ZeroMilliseconds", js: `0`, primary: "bbb",
			why: "an explicit zero is readable, and it means what it says: reconsider every tick",
		},
		{
			name: "SpaceInTheUnit", js: `'30 s'`, primary: "aaa",
			why: "an unreadable spelling must cost the default cooldown, not all of it",
		},
		{
			name: "UnparseableString", js: `'banana'`, primary: "aaa",
			why: "a typo is not an instruction to switch stickiness off",
		},
		{
			name: "EmptyString", js: `''`, primary: "aaa",
			why: "an empty string names no duration, so it lands where an absent key lands",
		},
		{
			name: "WrongType", js: `true`, primary: "aaa",
			why: "a value that is neither a string nor a number names no duration either",
		},
		{
			name: "NotANumber", js: `0 / 0`, primary: "aaa",
			why: "NaN is an ordinary JS number and converting it to int64 is undefined in Go",
		},
		{
			name: "Infinity", js: `1 / 0`, primary: "aaa",
			why: "an infinite cooldown is unreadable, not a cooldown of whatever int64 produces",
		},
		{
			name: "AbsentKnobReadThroughDurationMs", js: `durationMs(ctx.noSuchKnob)`, primary: "aaa",
			why: "reading a knob the context does not carry is absence, and absence is 30 seconds",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, tracker, ups, stop := stickyCooldownFixture(t, tc.js)
			defer stop()

			degradeIncumbent(tracker, ups)
			policy.TickForTest(engine, "evm:1", "*")

			got := engine.GetOrdered("evm:1", "*", "*")
			require.NotEmpty(t, got)
			require.Equal(t, tc.primary, got[0].Id(), tc.why)
		})
	}
}

// A knob the parser cannot read and a knob the operator did not write route
// the same traffic. The warning keeps them distinguishable: without it the
// operator cannot learn that their spelling did nothing.
func TestStickyPrimary_MinSwitchInterval_ReportsASpellingItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		js    string
		warns bool
	}{
		{name: "Unreadable", js: `'30 s'`, warns: true},
		{name: "Absent", js: `undefined`, warns: false},
		{name: "Readable", js: `'30s'`, warns: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			restoreLogger, restoreLevel := log.Logger, zerolog.GlobalLevel()
			log.Logger = zerolog.New(&logs).Level(zerolog.TraceLevel)
			zerolog.SetGlobalLevel(zerolog.TraceLevel)
			defer func() {
				log.Logger = restoreLogger
				zerolog.SetGlobalLevel(restoreLevel)
			}()

			engine, tracker, ups, stop := stickyCooldownFixture(t, tc.js)
			defer stop()
			degradeIncumbent(tracker, ups)
			policy.TickForTest(engine, "evm:1", "*")

			if !tc.warns {
				require.NotContains(t, logs.String(), "minSwitchInterval",
					"a knob the parser can read, or one nobody wrote, must stay silent")
				return
			}
			require.Contains(t, logs.String(), "minSwitchInterval",
				"the warning must name the knob the operator got wrong")
			require.Contains(t, logs.String(), "30 s",
				"the warning must quote the spelling the operator actually typed")

			// The eval runs on every tick. A line per tick would bury the one
			// line that carries the message.
			before := strings.Count(logs.String(), "minSwitchInterval")
			for i := 0; i < 5; i++ {
				policy.TickForTest(engine, "evm:1", "*")
			}
			require.Equal(t, before, strings.Count(logs.String(), "minSwitchInterval"),
				"the warning must be written once per spelling, not once per tick")
		})
	}
}

// TestStickyPrimary_MinSwitchInterval_HoldIsADelayNotALock pins the other half
// of the contract: a parsed cooldown delays the handover, it does not cancel it.
// Once the eval clock passes the window the challenger takes the traffic.
func TestStickyPrimary_MinSwitchInterval_HoldIsADelayNotALock(t *testing.T) {
	engine, tracker, ups, stop := stickyCooldownFixture(t, `'30s'`)
	defer stop()

	degradeIncumbent(tracker, ups)
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, "aaa", engine.GetOrdered("evm:1", "*", "*")[0].Id(),
		"inside the window the degraded incumbent keeps the traffic")

	policy.AdvanceEvalNowForTest(engine, "evm:1", "*", 60*time.Second)
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, "bbb", engine.GetOrdered("evm:1", "*", "*")[0].Id(),
		"past the window the healthy challenger must take over")
}
