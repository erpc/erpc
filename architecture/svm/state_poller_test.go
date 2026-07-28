package svm

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// newTestPoller builds a SvmStatePoller backed by an in-memory shared-state
// registry. It does NOT start the background loop — tests drive state through
// the public Suggest* methods directly. This lets us verify the slot/health
// surface without spinning up an HTTP mock or waiting on ticks.
func newTestPoller(t *testing.T) *SvmStatePoller {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "1MB"},
		},
		LockMaxWait:     common.Duration(50 * time.Millisecond),
		UpdateMaxWait:   common.Duration(50 * time.Millisecond),
		FallbackTimeout: common.Duration(1 * time.Second),
		LockTtl:         common.Duration(2 * time.Second),
	}
	cfg.SetDefaults("test")
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, cfg)
	require.NoError(t, err)

	return NewSvmStatePoller(
		"test", ctx, &log.Logger,
		&fakeUpstreamForPoller{},
		health.NewTracker(&log.Logger, "test", time.Minute),
		ssr,
	)
}

// fakeUpstreamForPoller satisfies common.Upstream for NewSvmStatePoller —
// only Id/Config/NetworkId are read during construction and UniqueUpstreamKey.
type fakeUpstreamForPoller struct{}

func (*fakeUpstreamForPoller) Id() string           { return "test-poller" }
func (*fakeUpstreamForPoller) VendorName() string   { return "" }
func (*fakeUpstreamForPoller) NetworkId() string    { return "svm:mainnet-beta" }
func (*fakeUpstreamForPoller) NetworkLabel() string { return "" }
func (*fakeUpstreamForPoller) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: "test-poller", Type: common.UpstreamTypeSvm, Endpoint: "http://x"}
}
func (*fakeUpstreamForPoller) Logger() *zerolog.Logger       { l := zerolog.Nop(); return &l }
func (*fakeUpstreamForPoller) Vendor() common.Vendor         { return nil }
func (*fakeUpstreamForPoller) Tracker() common.HealthTracker { return nil }
func (*fakeUpstreamForPoller) Forward(context.Context, *common.NormalizedRequest, bool, bool) (*common.NormalizedResponse, error) {
	// Return an error (never a nil/nil pair): a stray background Poll tick must
	// fail gracefully rather than nil-deref in fetchHealth/fetchSlot.
	return nil, fmt.Errorf("fakeUpstreamForPoller: no transport")
}
func (*fakeUpstreamForPoller) ShouldHandleMethod(string) (bool, error) { return true, nil }
func (*fakeUpstreamForPoller) Cordon(string, string)                   {}
func (*fakeUpstreamForPoller) Uncordon(string, string)                 {}
func (*fakeUpstreamForPoller) IgnoreMethod(string)                     {}

// scriptedResponse holds either a result payload or a JSON-RPC error. Exactly
// one of the two fields is populated per canned response.
type scriptedResponse struct {
	result []byte // raw result bytes, e.g. `"ok"` or `1234`
	errJr  *common.ErrJsonRpcExceptionExternal
}

// scriptedUpstream returns canned JSON-RPC responses keyed by the request
// body's method name (and for getSlot, its commitment argument too). Lets
// Poll() exercise the full fan-out without any HTTP transport.
type scriptedUpstream struct {
	fakeUpstreamForPoller
	responses map[string]scriptedResponse // request key → scripted answer
	calls     map[string]int              // request key → invocation count
	mu        sync.Mutex
}

func newScriptedUpstream() *scriptedUpstream {
	return &scriptedUpstream{
		responses: map[string]scriptedResponse{},
		calls:     map[string]int{},
	}
}

// script registers a result-bearing response for the given key.
func (s *scriptedUpstream) script(key string, resultBody []byte) {
	s.responses[key] = scriptedResponse{result: resultBody}
}

// scriptError registers a JSON-RPC error response for the given key.
func (s *scriptedUpstream) scriptError(key string, code int, msg string) {
	s.responses[key] = scriptedResponse{errJr: common.NewErrJsonRpcExceptionExternal(code, msg, "")}
}

// requestKey maps a state-poller request payload to one of the four known
// kinds. getSlot is split by commitment so processed and finalized route to
// separate canned responses.
func requestKey(body string) string {
	switch {
	case strings.Contains(body, `"method":"getHealth"`):
		return "getHealth"
	case strings.Contains(body, `"method":"getMaxShredInsertSlot"`):
		return "getMaxShredInsertSlot"
	case strings.Contains(body, `"method":"getSlot"`) && strings.Contains(body, `"processed"`):
		return "getSlot:processed"
	case strings.Contains(body, `"method":"getSlot"`) && strings.Contains(body, `"finalized"`):
		return "getSlot:finalized"
	}
	return "unknown"
}

func (s *scriptedUpstream) Forward(ctx context.Context, req *common.NormalizedRequest, _, _ bool) (*common.NormalizedResponse, error) {
	body := string(req.Body())
	key := requestKey(body)

	s.mu.Lock()
	s.calls[key]++
	scripted, ok := s.responses[key]
	s.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("scriptedUpstream: no scripted response for %q (body=%s)", key, body)
	}

	var jrr *common.JsonRpcResponse
	var err error
	if scripted.errJr != nil {
		jrr, err = common.NewJsonRpcResponse(1, nil, scripted.errJr)
	} else {
		jrr, err = common.NewJsonRpcResponseFromBytes(nil, scripted.result, nil)
	}
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}

func (s *scriptedUpstream) callCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[key]
}

// newPollerWithUpstream wires a state poller around a caller-provided Upstream.
// Mirrors newTestPoller but lets tests observe the scripted upstream's counters.
func newPollerWithUpstream(t *testing.T, up common.Upstream) *SvmStatePoller {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "1MB"},
		},
		LockMaxWait:     common.Duration(50 * time.Millisecond),
		UpdateMaxWait:   common.Duration(50 * time.Millisecond),
		FallbackTimeout: common.Duration(1 * time.Second),
		LockTtl:         common.Duration(2 * time.Second),
	}
	cfg.SetDefaults("test")
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, cfg)
	require.NoError(t, err)

	return NewSvmStatePoller(
		"test", ctx, &log.Logger, up,
		health.NewTracker(&log.Logger, "test", time.Minute),
		ssr,
	)
}

func TestSvmStatePoller_Poll_FansOutAllFourCalls(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	up.script("getHealth", []byte(`"ok"`))
	up.script("getSlot:processed", []byte(`1000`))
	up.script("getSlot:finalized", []byte(`990`))
	up.script("getMaxShredInsertSlot", []byte(`998`)) // lag = 1000 - 998 = 2 (healthy)

	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Poll(context.Background()))

	if up.callCount("getHealth") != 1 {
		t.Errorf("getHealth called %d times, want 1", up.callCount("getHealth"))
	}
	if up.callCount("getSlot:processed") != 1 {
		t.Errorf("getSlot(processed) called %d times, want 1", up.callCount("getSlot:processed"))
	}
	if up.callCount("getSlot:finalized") != 1 {
		t.Errorf("getSlot(finalized) called %d times, want 1", up.callCount("getSlot:finalized"))
	}
	if up.callCount("getMaxShredInsertSlot") != 1 {
		t.Errorf("getMaxShredInsertSlot called %d times, want 1", up.callCount("getMaxShredInsertSlot"))
	}

	if p.LatestSlot() != 1000 {
		t.Errorf("LatestSlot = %d, want 1000", p.LatestSlot())
	}
	if p.FinalizedSlot() != 990 {
		t.Errorf("FinalizedSlot = %d, want 990", p.FinalizedSlot())
	}
	if p.MaxShredInsertSlotLag() != 2 {
		t.Errorf("MaxShredInsertSlotLag = %d, want 2 (1000 - 998)", p.MaxShredInsertSlotLag())
	}
	if !p.IsHealthy() {
		t.Error("IsHealthy should be true: getHealth ok + lag within threshold")
	}
}

func TestSvmStatePoller_Poll_HealthFailureFlipsHealthy(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	// getHealth returns a JSON-RPC error — the poller should flip healthy=false
	// without poisoning the slot readings.
	up.scriptError("getHealth", -32000, "degraded")
	up.script("getSlot:processed", []byte(`2000`))
	up.script("getSlot:finalized", []byte(`1990`))
	up.script("getMaxShredInsertSlot", []byte(`1999`))

	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Poll(context.Background()))

	if p.IsHealthy() {
		t.Error("IsHealthy should flip to false when getHealth returns a JSON-RPC error")
	}
	// Slot reads should still succeed even when health fails — the poller must
	// report each signal independently, not bail on first error.
	if p.LatestSlot() != 2000 {
		t.Errorf("slot tracking should be unaffected by health failure; got %d", p.LatestSlot())
	}
}

func TestSvmStatePoller_Poll_ExcessiveShredLagFlipsHealthy(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	up.script("getHealth", []byte(`"ok"`))
	// Latest = 10000, shred = 9500 → lag 500 > threshold 100.
	up.script("getSlot:processed", []byte(`10000`))
	up.script("getSlot:finalized", []byte(`9990`))
	up.script("getMaxShredInsertSlot", []byte(`9500`))

	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Poll(context.Background()))

	if p.IsHealthy() {
		t.Errorf("IsHealthy should be false when shred lag (%d) exceeds threshold (%d)",
			p.MaxShredInsertSlotLag(), common.MaxShredInsertSlotLagThreshold)
	}
}

func TestSvmStatePoller_Poll_DebouncesWithinInterval(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	up.script("getHealth", []byte(`"ok"`))
	up.script("getSlot:processed", []byte(`100`))
	up.script("getSlot:finalized", []byte(`99`))
	up.script("getMaxShredInsertSlot", []byte(`100`))

	p := newPollerWithUpstream(t, up)
	p.SetDebounceInterval(5 * time.Second) // force a skip on the second call
	require.NoError(t, p.Poll(context.Background()))
	// Second call within the debounce window should be a no-op.
	require.NoError(t, p.Poll(context.Background()))

	if up.callCount("getSlot:processed") != 1 {
		t.Errorf("debounce should suppress second Poll; got %d getSlot calls", up.callCount("getSlot:processed"))
	}
}

// scriptAllFour registers healthy canned responses for every poller request so
// the traffic-gate tests can focus on WHICH calls fire, not what they return.
func scriptAllFour(up *scriptedUpstream) {
	up.script("getHealth", []byte(`"ok"`))
	up.script("getSlot:processed", []byte(`1000`))
	up.script("getSlot:finalized", []byte(`990`))
	up.script("getMaxShredInsertSlot", []byte(`998`))
}

// TestSvmStatePoller_Poll_TrafficGate_ClosedWithoutSuggestions: enabling the
// debounce gate alone must not suppress anything — without external freshness
// evidence Poll still fans out all four calls.
func TestSvmStatePoller_Poll_TrafficGate_ClosedWithoutSuggestions(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllFour(up)
	p := newPollerWithUpstream(t, up)
	p.SetDebounceInterval(200 * time.Millisecond)

	// First Poll is never debounce-blocked (no prior poll recorded).
	require.NoError(t, p.Poll(context.Background()))

	for _, key := range []string{"getHealth", "getSlot:processed", "getSlot:finalized", "getMaxShredInsertSlot"} {
		if got := up.callCount(key); got != 1 {
			t.Errorf("%s called %d times, want 1 (gate must stay closed without suggestions)", key, got)
		}
	}
}

// TestSvmStatePoller_Poll_TrafficGate_SkipsGetSlotWhenBothViewsFresh: when live
// traffic refreshed BOTH slot views within the debounce window, Poll skips the
// two getSlot calls but still issues getHealth and getMaxShredInsertSlot
// (traffic carries neither signal), and the slot surface keeps serving the
// traffic-fed values.
func TestSvmStatePoller_Poll_TrafficGate_SkipsGetSlotWhenBothViewsFresh(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllFour(up)
	p := newPollerWithUpstream(t, up)
	p.SetDebounceInterval(200 * time.Millisecond)

	// External values differ from the scripted getSlot answers (1000/990) so a
	// sneaky fetch would show up in the slot surface too, not just the counters.
	p.SuggestLatestSlot(1500)
	p.SuggestFinalizedSlot(1490)
	require.NoError(t, p.Poll(context.Background()))

	if got := up.callCount("getSlot:processed"); got != 0 {
		t.Errorf("getSlot(processed) called %d times, want 0 (gated by fresh traffic)", got)
	}
	if got := up.callCount("getSlot:finalized"); got != 0 {
		t.Errorf("getSlot(finalized) called %d times, want 0 (gated by fresh traffic)", got)
	}
	if got := up.callCount("getHealth"); got != 1 {
		t.Errorf("getHealth called %d times, want 1 (must run on gated ticks)", got)
	}
	if got := up.callCount("getMaxShredInsertSlot"); got != 1 {
		t.Errorf("getMaxShredInsertSlot called %d times, want 1 (must run on gated ticks)", got)
	}
	if p.LatestSlot() != 1500 || p.FinalizedSlot() != 1490 {
		t.Errorf("slot surface must stay traffic-fed on a gated tick: latest=%d finalized=%d, want 1500/1490",
			p.LatestSlot(), p.FinalizedSlot())
	}
}

// TestSvmStatePoller_Poll_TrafficGate_PartialFreshnessStillPollsSlots: one
// fresh view is not freshness — the gate requires BOTH latest and finalized
// observations within the window, else Poll fans out all four calls.
func TestSvmStatePoller_Poll_TrafficGate_PartialFreshnessStillPollsSlots(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		suggest func(*SvmStatePoller)
	}{
		{"only latest fresh", func(p *SvmStatePoller) { p.SuggestLatestSlot(1500) }},
		{"only finalized fresh", func(p *SvmStatePoller) { p.SuggestFinalizedSlot(1490) }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			up := newScriptedUpstream()
			scriptAllFour(up)
			p := newPollerWithUpstream(t, up)
			p.SetDebounceInterval(200 * time.Millisecond)

			tc.suggest(p)
			require.NoError(t, p.Poll(context.Background()))

			for _, key := range []string{"getHealth", "getSlot:processed", "getSlot:finalized", "getMaxShredInsertSlot"} {
				if got := up.callCount(key); got != 1 {
					t.Errorf("%s called %d times, want 1 (one fresh view must not gate)", key, got)
				}
			}
		})
	}
}

// TestSvmStatePoller_Poll_TrafficGate_SkipCapForcesFullPoll: continuously fresh
// traffic may gate at most maxConsecutiveSlotPollSkips consecutive polls; the
// next poll forces the poller's own getSlot observation, and that forced poll
// resets the counter so gating resumes afterwards.
func TestSvmStatePoller_Poll_TrafficGate_SkipCapForcesFullPoll(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllFour(up)
	p := newPollerWithUpstream(t, up)

	const debounce = 100 * time.Millisecond
	p.SetDebounceInterval(debounce)

	// Expected cumulative getSlot fetches after the i-th poll: polls 1-4 are
	// gated, poll 5 hits the skip cap (maxConsecutiveSlotPollSkips=4) and
	// forces a full poll, poll 6 may gate again (counter reset by poll 5).
	wantSlotCalls := []int{0, 0, 0, 0, 1, 1}

	for i, want := range wantSlotCalls {
		if i > 0 {
			// Clear the whole-poll debounce so every iteration actually polls.
			time.Sleep(debounce + 40*time.Millisecond)
		}
		// Re-stamp external freshness right before each poll so both views are
		// always well inside the debounce window.
		p.SuggestLatestSlot(int64(2000 + i))
		p.SuggestFinalizedSlot(int64(1990 + i))
		require.NoError(t, p.Poll(context.Background()))

		// Guard against a vacuous pass: getHealth counts every poll that ran,
		// so a debounce-dropped iteration is caught here, not mistaken for a
		// gated one.
		if got := up.callCount("getHealth"); got != i+1 {
			t.Fatalf("poll %d did not run: getHealth=%d, want %d", i+1, got, i+1)
		}
		if got := up.callCount("getSlot:processed"); got != want {
			t.Fatalf("after poll %d: getSlot(processed)=%d, want %d", i+1, got, want)
		}
		if got := up.callCount("getSlot:finalized"); got != want {
			t.Fatalf("after poll %d: getSlot(finalized)=%d, want %d", i+1, got, want)
		}
	}
}

// TestSvmStatePoller_Poll_TrafficGate_SelfSuggestionsDoNotOpenGate: a full poll
// updates the slot views internally, but the poller's own observations are not
// traffic — the next eligible poll must still fetch both slots.
func TestSvmStatePoller_Poll_TrafficGate_SelfSuggestionsDoNotOpenGate(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllFour(up)
	p := newPollerWithUpstream(t, up)

	const debounce = 100 * time.Millisecond
	p.SetDebounceInterval(debounce)

	require.NoError(t, p.Poll(context.Background())) // full poll: updates slots internally
	time.Sleep(debounce + 40*time.Millisecond)       // clear the whole-poll debounce
	require.NoError(t, p.Poll(context.Background()))

	if got := up.callCount("getHealth"); got != 2 {
		t.Fatalf("second poll did not run: getHealth=%d, want 2", got)
	}
	if got := up.callCount("getSlot:processed"); got != 2 {
		t.Errorf("getSlot(processed)=%d, want 2 (self-observed slots must not gate)", got)
	}
	if got := up.callCount("getSlot:finalized"); got != 2 {
		t.Errorf("getSlot(finalized)=%d, want 2 (self-observed slots must not gate)", got)
	}
}

// TestSvmStatePoller_SetDebounceInterval_UpdatesGate guards the fix for the
// dead statePollerDebounce config: SetDebounceInterval must update the
// coalesce / traffic-gate window to ANY positive value, and ignore non-positive.
func TestSvmStatePoller_SetDebounceInterval_UpdatesGate(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)
	p.SetDebounceInterval(2 * time.Second)
	if got := time.Duration(p.debounceInterval.Load()); got != 2*time.Second {
		t.Fatalf("debounceInterval = %v, want 2s", got)
	}
	// A sub-slot value must also take effect.
	p.SetDebounceInterval(150 * time.Millisecond)
	if got := time.Duration(p.debounceInterval.Load()); got != 150*time.Millisecond {
		t.Fatalf("debounceInterval = %v, want 150ms", got)
	}
	// Non-positive is ignored.
	p.SetDebounceInterval(0)
	if got := time.Duration(p.debounceInterval.Load()); got != 150*time.Millisecond {
		t.Fatalf("zero must be ignored, got %v", got)
	}
}

// TestSvmStatePoller_SetPollInterval_UpdatesCadence verifies SetPollInterval
// stores any positive ticker cadence and ignores non-positive input.
func TestSvmStatePoller_SetPollInterval_UpdatesCadence(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)
	p.SetPollInterval(5 * time.Second)
	if got := time.Duration(p.pollInterval.Load()); got != 5*time.Second {
		t.Fatalf("pollInterval = %v, want 5s", got)
	}
	p.SetPollInterval(30 * time.Second)
	if got := time.Duration(p.pollInterval.Load()); got != 30*time.Second {
		t.Fatalf("pollInterval = %v, want 30s", got)
	}
	p.SetPollInterval(0)
	if got := time.Duration(p.pollInterval.Load()); got != 30*time.Second {
		t.Fatalf("zero must be ignored, got %v", got)
	}
}

// TestSvmStatePoller_Bootstrap_HonorsPresetCadence verifies Bootstrap preserves
// interval/debounce set before it runs (config-before-Bootstrap ordering)
// instead of clobbering them with defaults.
func TestSvmStatePoller_Bootstrap_HonorsPresetCadence(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)
	p.SetPollInterval(30 * time.Second)
	p.SetDebounceInterval(1 * time.Second)
	require.NoError(t, p.Bootstrap(context.Background()))
	if got := time.Duration(p.pollInterval.Load()); got != 30*time.Second {
		t.Fatalf("Bootstrap overwrote preset interval: got %v, want 30s", got)
	}
	if got := time.Duration(p.debounceInterval.Load()); got != 1*time.Second {
		t.Fatalf("Bootstrap overwrote preset debounce: got %v, want 1s", got)
	}
	// Updating after Bootstrap is safe and takes effect on the next loop sleep / Poll.
	p.SetPollInterval(10 * time.Second)
	p.SetDebounceInterval(400 * time.Millisecond)
	if got := time.Duration(p.pollInterval.Load()); got != 10*time.Second {
		t.Fatalf("post-Bootstrap interval update ignored: got %v, want 10s", got)
	}
	if got := time.Duration(p.debounceInterval.Load()); got != 400*time.Millisecond {
		t.Fatalf("post-Bootstrap debounce update ignored: got %v, want 400ms", got)
	}
}

// TestSvmStatePoller_Bootstrap_DefaultsCadence verifies unset interval/debounce
// fall back to DefaultStatePollerInterval / DefaultStatePollerDebounce.
func TestSvmStatePoller_Bootstrap_DefaultsCadence(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)
	require.NoError(t, p.Bootstrap(context.Background()))
	if got := time.Duration(p.pollInterval.Load()); got != DefaultStatePollerInterval {
		t.Fatalf("pollInterval = %v, want %v", got, DefaultStatePollerInterval)
	}
	if got := time.Duration(p.debounceInterval.Load()); got != DefaultStatePollerDebounce {
		t.Fatalf("debounceInterval = %v, want %v", got, DefaultStatePollerDebounce)
	}
}

func TestSvmStatePoller_SuggestLatestSlot_Monotonic(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)

	p.SuggestLatestSlot(100)
	if p.LatestSlot() != 100 {
		t.Fatalf("expected 100, got %d", p.LatestSlot())
	}
	p.SuggestLatestSlot(200)
	if p.LatestSlot() != 200 {
		t.Fatalf("expected 200 after advance, got %d", p.LatestSlot())
	}
	// Suggesting a lower slot must not roll the value back within the tolerance.
	p.SuggestLatestSlot(150)
	if p.LatestSlot() != 200 {
		t.Fatalf("expected 200 (no rollback), got %d", p.LatestSlot())
	}
}

func TestSvmStatePoller_SuggestFinalizedSlot_Monotonic(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)

	p.SuggestFinalizedSlot(300)
	p.SuggestFinalizedSlot(400)
	if p.FinalizedSlot() != 400 {
		t.Fatalf("expected 400 after advance, got %d", p.FinalizedSlot())
	}
	// A lower suggestion must not roll the finalized view back.
	p.SuggestFinalizedSlot(350)
	if p.FinalizedSlot() != 400 {
		t.Fatalf("expected 400 (no rollback), got %d", p.FinalizedSlot())
	}
}

func TestSvmStatePoller_SuggestLatestSlot_IgnoresNonPositive(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)

	p.SuggestLatestSlot(0)
	p.SuggestLatestSlot(-1)
	if p.LatestSlot() != 0 {
		t.Fatalf("0 and -1 must be ignored, got %d", p.LatestSlot())
	}
}

func TestSvmStatePoller_LatestAndFinalized_AreIndependent(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)

	p.SuggestLatestSlot(500)
	p.SuggestFinalizedSlot(480)
	if p.LatestSlot() != 500 || p.FinalizedSlot() != 480 {
		t.Fatalf("expected latest=500, finalized=480; got latest=%d, finalized=%d",
			p.LatestSlot(), p.FinalizedSlot())
	}

	// Advancing finalized alone must not move latest.
	p.SuggestFinalizedSlot(490)
	if p.LatestSlot() != 500 || p.FinalizedSlot() != 490 {
		t.Fatalf("finalized advance leaked to latest: latest=%d, finalized=%d",
			p.LatestSlot(), p.FinalizedSlot())
	}
}

func TestSvmStatePoller_IsHealthy_DefaultTrue(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)
	if !p.IsHealthy() {
		t.Fatal("new poller must report healthy until the first failing tick")
	}
}

func TestSvmStatePoller_IsHealthy_FalseWhenLagExceedsThreshold(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)

	// Below threshold → still healthy.
	p.maxShredInsertSlotLag.Store(common.MaxShredInsertSlotLagThreshold)
	if !p.IsHealthy() {
		t.Fatal("lag exactly at threshold must stay healthy")
	}

	// One above → degraded.
	p.maxShredInsertSlotLag.Store(common.MaxShredInsertSlotLagThreshold + 1)
	if p.IsHealthy() {
		t.Fatal("lag above threshold must mark the upstream unhealthy")
	}
}

func TestSvmStatePoller_IsHealthy_FalseWhenHealthFlagIsFalse(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)

	p.healthy.Store(false)
	// Zero lag but explicit unhealthy signal — IsHealthy must respect the flag.
	if p.IsHealthy() {
		t.Fatal("healthy=false must be honored regardless of lag")
	}
}

func TestSvmStatePoller_IsObjectNull_NilReceiver(t *testing.T) {
	t.Parallel()
	var p *SvmStatePoller
	if !p.IsObjectNull() {
		t.Fatal("nil receiver must report IsObjectNull()=true")
	}

	p2 := newTestPoller(t)
	if p2.IsObjectNull() {
		t.Fatal("non-nil poller must report IsObjectNull()=false")
	}
}
