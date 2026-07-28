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
	p, _ := newTestPollerWithCancel(t)
	return p
}

// newTestPollerWithCancel additionally hands back the appCtx cancel so a test
// can assert goroutine teardown, not just startup.
func newTestPollerWithCancel(t *testing.T) (*SvmStatePoller, context.CancelFunc) {
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
	), cancel
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
	// cordonCalls / uncordonCalls record the routing side of the poller's health
	// verdict: cordon is what actually removes an upstream from selection, so
	// these counters are how a test proves the verdict is edge-triggered rather
	// than re-issued on every tick.
	cordonCalls   []cordonEvent
	uncordonCalls []cordonEvent
	mu            sync.Mutex
}

type cordonEvent struct {
	method string
	reason string
}

func newScriptedUpstream() *scriptedUpstream {
	return &scriptedUpstream{
		responses: map[string]scriptedResponse{},
		calls:     map[string]int{},
	}
}

// script registers a result-bearing response for the given key.
func (s *scriptedUpstream) script(key string, resultBody []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[key] = scriptedResponse{result: resultBody}
}

// scriptError registers a JSON-RPC error response for the given key.
func (s *scriptedUpstream) scriptError(key string, code int, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *scriptedUpstream) Cordon(method, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cordonCalls = append(s.cordonCalls, cordonEvent{method, reason})
}

func (s *scriptedUpstream) Uncordon(method, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uncordonCalls = append(s.uncordonCalls, cordonEvent{method, reason})
}

func (s *scriptedUpstream) cordons() []cordonEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]cordonEvent(nil), s.cordonCalls...)
}

func (s *scriptedUpstream) uncordons() []cordonEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]cordonEvent(nil), s.uncordonCalls...)
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
	// The shred watermark is structurally AHEAD of the replayed slot, so
	// ingestion lag = 1002 - 1000 = 2 (healthy).
	up.script("getMaxShredInsertSlot", []byte(`1002`))

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
		t.Errorf("MaxShredInsertSlotLag = %d, want 2 (maxShredInsertSlot 1002 - processedSlot 1000)", p.MaxShredInsertSlotLag())
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
	up.script("getMaxShredInsertSlot", []byte(`2001`))

	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Poll(context.Background()))

	if p.IsHealthy() {
		t.Error("IsHealthy should flip to false when getHealth returns a JSON-RPC error")
	}
	// The verdict must reach routing, else it changes nothing.
	cordons := up.cordons()
	require.Len(t, cordons, 1, "a failing getHealth must cordon the upstream out of rotation")
	require.Equal(t, "*", cordons[0].method)
	require.Contains(t, cordons[0].reason, "getHealth")
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
	// Processed = 10000 while the shred watermark has reached 10500 → ingestion
	// lag 500 > threshold 100. This is the silent-stale signature: shreds keep
	// arriving while replay is stalled, and getHealth still answers "ok".
	up.script("getSlot:processed", []byte(`10000`))
	up.script("getSlot:finalized", []byte(`9990`))
	up.script("getMaxShredInsertSlot", []byte(`10500`))

	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Poll(context.Background()))

	if p.MaxShredInsertSlotLag() != 500 {
		t.Errorf("MaxShredInsertSlotLag = %d, want 500 (maxShredInsertSlot 10500 - processedSlot 10000)", p.MaxShredInsertSlotLag())
	}
	if p.IsHealthy() {
		t.Errorf("IsHealthy should be false when shred lag (%d) exceeds threshold (%d)",
			p.MaxShredInsertSlotLag(), common.MaxShredInsertSlotLagThreshold)
	}
	cordons := up.cordons()
	require.Len(t, cordons, 1, "excessive shred-insert lag must cordon the upstream out of rotation")
	require.Equal(t, "*", cordons[0].method)
	require.Contains(t, cordons[0].reason, "shred-insert lag")
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

// TestSvmStatePoller_SetDebounceInterval_UpdatesCadence guards the fix for the
// dead statePollerDebounce config: SetDebounceInterval must update the poll
// cadence to ANY positive value (the prior bug ignored configured values), and
// ignore non-positive input.
func TestSvmStatePoller_SetDebounceInterval_UpdatesCadence(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)
	p.SetDebounceInterval(2 * time.Second)
	if got := time.Duration(p.debounceInterval.Load()); got != 2*time.Second {
		t.Fatalf("debounceInterval = %v, want 2s", got)
	}
	// A sub-default value (< DefaultPollInterval) must also take effect.
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

// TestSvmStatePoller_Bootstrap_HonorsPresetDebounce verifies Bootstrap preserves
// a debounce gate set before it runs (config-before-Bootstrap ordering) instead
// of clobbering it with the default. (The ticker always runs at the fixed
// DefaultPollInterval; the gate is what the config controls.)
func TestSvmStatePoller_Bootstrap_HonorsPresetDebounce(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)
	p.SetDebounceInterval(30 * time.Second)
	require.NoError(t, p.Bootstrap(context.Background()))
	if got := time.Duration(p.debounceInterval.Load()); got != 30*time.Second {
		t.Fatalf("Bootstrap overwrote preset debounce: got %v, want 30s", got)
	}
	// Updating the gate after Bootstrap is safe and takes effect.
	p.SetDebounceInterval(25 * time.Second)
	if got := time.Duration(p.debounceInterval.Load()); got != 25*time.Second {
		t.Fatalf("post-Bootstrap update ignored: got %v, want 25s", got)
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

// TestSvmStatePoller_Poll_ShredWatermarkBehindProcessedClampsToZero pins the
// clamp DIRECTION. maxShredInsertSlot below the processed slot is structurally
// impossible on a single node, so it means the two samples are skewed (a later
// traffic-fed processed slot, or a shared-state peer writing a higher slot for
// this upstream) — not ingestion lag. It must report no lag, and must not
// condemn a node on sampling noise.
func TestSvmStatePoller_Poll_ShredWatermarkBehindProcessedClampsToZero(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	up.script("getHealth", []byte(`"ok"`))
	up.script("getSlot:processed", []byte(`10000`))
	up.script("getSlot:finalized", []byte(`9990`))
	up.script("getMaxShredInsertSlot", []byte(`9000`)) // 1000 slots BEHIND processed

	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Poll(context.Background()))

	require.Equal(t, int64(9000), p.ShredInsertSlot(), "the raw watermark is still reported as observed")
	require.Zero(t, p.MaxShredInsertSlotLag(), "a watermark behind the processed slot is skew, not lag")
	require.True(t, p.IsHealthy(), "skewed samples must not mark an upstream unhealthy")
	require.Empty(t, up.cordons(), "skewed samples must not cordon")
}

// TestSvmStatePoller_HealthToRouting_EdgeTriggered: the health verdict must
// reach upstream selection exactly on TRANSITIONS. A level-triggered cordon
// would re-stamp the reason every 400ms tick (spawning a fresh
// erpc_upstream_cordoned gauge series and resetting the cordon-duration
// observation each time), and a level-triggered uncordon would keep clearing an
// operator's manual cordon.
func TestSvmStatePoller_HealthToRouting_EdgeTriggered(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	up.script("getHealth", []byte(`"ok"`))
	up.script("getSlot:processed", []byte(`10000`))
	up.script("getSlot:finalized", []byte(`9990`))
	up.script("getMaxShredInsertSlot", []byte(`10500`)) // lag 500 > threshold

	p := newPollerWithUpstream(t, up)

	// Degraded across several ticks → exactly ONE cordon.
	for range 3 {
		require.NoError(t, p.Poll(context.Background()))
	}
	require.False(t, p.IsHealthy())
	require.Len(t, up.cordons(), 1, "cordon must fire on the healthy→unhealthy edge only, not every tick")
	require.Empty(t, up.uncordons())

	// Replay catches up: the watermark is now only 5 slots ahead of processed.
	up.script("getMaxShredInsertSlot", []byte(`10005`))
	for range 3 {
		require.NoError(t, p.Poll(context.Background()))
	}
	require.True(t, p.IsHealthy())
	require.Len(t, up.cordons(), 1, "recovery must not add cordons")
	require.Len(t, up.uncordons(), 1, "uncordon must fire on the unhealthy→healthy edge only")
	require.Equal(t, "*", up.uncordons()[0].method)

	// And a second degradation cordons again — the edge detector re-arms.
	up.script("getMaxShredInsertSlot", []byte(`11000`))
	require.NoError(t, p.Poll(context.Background()))
	require.Len(t, up.cordons(), 2, "the edge detector must re-arm after recovery")
}

// TestSvmStatePoller_ColdPoller_DoesNotCordon: unknown != unhealthy. A poller
// that has not yet landed a getMaxShredInsertSlot sample has no lag evidence
// and must leave the upstream in rotation.
func TestSvmStatePoller_ColdPoller_DoesNotCordon(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	up.script("getHealth", []byte(`"ok"`))
	up.script("getSlot:processed", []byte(`5000`))
	up.script("getSlot:finalized", []byte(`4990`))
	// getMaxShredInsertSlot deliberately unscripted: the upstream errors on it,
	// so the poller never observes a shred watermark.

	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Poll(context.Background()))

	require.Zero(t, p.ShredInsertSlot(), "no shred observation should have landed")
	require.Zero(t, p.MaxShredInsertSlotLag())
	require.True(t, p.IsHealthy(), "a missing shred signal must not read as unhealthy")
	require.Empty(t, up.cordons(), "a poller with no shred observation must never cordon")
}

// TestSvmStatePoller_Bootstrap_StartsAtMostOneLoop guards the ticker-goroutine
// leak: the upstream initializer retries the whole bootstrap task on the SAME
// poller instance after a failed genesis validation, so every extra Bootstrap
// used to leave behind a ticker goroutine that only process exit could stop.
func TestSvmStatePoller_Bootstrap_StartsAtMostOneLoop(t *testing.T) {
	// No t.Parallel: this asserts a goroutine-lifecycle invariant with sleeps.
	p, cancel := newTestPollerWithCancel(t)

	for range 5 {
		require.NoError(t, p.Bootstrap(context.Background()))
	}
	require.Eventually(t, func() bool { return p.loopsRunning.Load() == 1 },
		2*time.Second, 5*time.Millisecond, "exactly one poll loop must come up")

	// Give any leaked sibling time to register itself, then re-assert.
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(1), p.loopsRunning.Load(), "Bootstrap leaked one ticker goroutine per retry")

	// appCtx cancellation must still tear the single loop down.
	cancel()
	require.Eventually(t, func() bool { return p.loopsRunning.Load() == 0 },
		2*time.Second, 5*time.Millisecond, "appCtx cancellation must stop the poll loop")
}
