package svm

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

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
	errJr  error  // classified error, returned as (nil, err) like Forward
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
	// cfg overrides the embedded fake's config when set, so a test can declare
	// svm.blockAvailability on the upstream the poller reads it from.
	cfg *common.UpstreamConfig
}

func (s *scriptedUpstream) Config() *common.UpstreamConfig {
	if s.cfg != nil {
		return s.cfg
	}
	return s.fakeUpstreamForPoller.Config()
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

// scriptError registers a JSON-RPC error for the given key. It comes back the
// way upstream.Forward really returns errors: (nil, classified err), not a
// response object carrying the error inside.
func (s *scriptedUpstream) scriptError(key string, code int, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[key] = scriptedResponse{errJr: common.NewErrJsonRpcExceptionExternal(code, msg, "")}
}

// scriptClassifiedError registers an arbitrary classified error (e.g. circuit
// breaker open, upstream rate-limit budget) for the given key, returned the
// way Forward returns it: (nil, err).
func (s *scriptedUpstream) scriptClassifiedError(key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[key] = scriptedResponse{errJr: err}
}

// requestKey maps a state-poller request payload to one of the known kinds.
// getSlot is split by commitment so processed and finalized route to separate
// canned responses.
func requestKey(body string) string {
	switch {
	case strings.Contains(body, `"method":"getMaxShredInsertSlot"`):
		return "getMaxShredInsertSlot"
	case strings.Contains(body, `"method":"getBlocksWithLimit","params":[0,1]`):
		return "oldestBlock"
	case strings.Contains(body, `"method":"getSlot"`) && strings.Contains(body, `"processed"`):
		return "getSlot:processed"
	case strings.Contains(body, `"method":"getSlot"`) && strings.Contains(body, `"finalized"`):
		return "getSlot:finalized"
	}
	return "unknown"
}

// Forward mimics upstream.Upstream.Forward faithfully on the shapes liveness
// cares about: an unscripted method is TRANSPORT SILENCE (a classified
// transport failure, not a bare error — a bare error would classify as
// "answered"), and a scripted RPC error comes back as (nil, err), because
// the real Forward drops the response on any error (upstream.go's errCall
// branch). Tests that need an endpoint answering with an RPC error must use
// scriptError; tests that need a dead endpoint simply leave a method
// unscripted.
func (s *scriptedUpstream) Forward(ctx context.Context, req *common.NormalizedRequest, _, _ bool) (*common.NormalizedResponse, error) {
	body := string(req.Body())
	key := requestKey(body)

	s.mu.Lock()
	s.calls[key]++
	scripted, ok := s.responses[key]
	s.mu.Unlock()

	if !ok {
		return nil, common.NewErrEndpointTransportFailure(&url.URL{Host: "scriptedUpstream"},
			fmt.Errorf("no scripted response for %q (body=%s)", key, body))
	}

	if scripted.errJr != nil {
		// The real Forward returns (nil, wrapped err) on JSON-RPC errors;
		// the raw classified error classifies identically under HasErrorCode.
		return nil, scripted.errJr
	}

	jrr, err := common.NewJsonRpcResponseFromBytes(nil, scripted.result, nil)
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
	return newPollerOnCtx(t, ctx, up)
}

// newPollerOnCtx is newPollerWithUpstream with a caller-supplied appCtx, so a
// test can cancel the context SEVERAL pollers share and observe every loop tear
// down. Each poller gets its own shared-state registry: the counter key is
// derived from the upstream identity, and two pollers sharing one registry
// would land on the same counter instance and cross-fire each other's OnValue
// callbacks.
func newPollerOnCtx(t *testing.T, appCtx context.Context, up common.Upstream) *SvmStatePoller {
	t.Helper()
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
	ssr, err := data.NewSharedStateRegistry(appCtx, &log.Logger, cfg)
	require.NoError(t, err)

	return NewSvmStatePoller(
		"test", appCtx, &log.Logger, up,
		health.NewTracker(&log.Logger, "test", time.Minute),
		ssr,
	)
}

func TestSvmStatePoller_Poll_FansOutAllCalls(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()

	up.script("getSlot:processed", []byte(`1000`))
	up.script("getSlot:finalized", []byte(`990`))
	// The shred watermark is structurally AHEAD of the replayed slot, so
	// ingestion lag = 1002 - 1000 = 2 (healthy).
	up.script("getMaxShredInsertSlot", []byte(`1002`))
	up.script("oldestBlock", []byte(`[200]`))

	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Poll(context.Background()))

	if up.callCount("getSlot:processed") != 1 {
		t.Errorf("getSlot(processed) called %d times, want 1", up.callCount("getSlot:processed"))
	}
	if up.callCount("getSlot:finalized") != 1 {
		t.Errorf("getSlot(finalized) called %d times, want 1", up.callCount("getSlot:finalized"))
	}
	if up.callCount("getMaxShredInsertSlot") != 1 {
		t.Errorf("getMaxShredInsertSlot called %d times, want 1", up.callCount("getMaxShredInsertSlot"))
	}
	if up.callCount("oldestBlock") != 1 {
		t.Errorf("floor probe called %d times, want 1", up.callCount("oldestBlock"))
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
		t.Error("IsHealthy should be true: probes answering + lag within threshold")
	}
	// The floor fetch may finish after Poll's bounded foreground wait under
	// -race; it still lands via the async closure.
	require.Eventually(t, func() bool {
		floor, known := p.FirstAvailableSlot()
		return known && floor == 200
	}, 5*time.Second, 10*time.Millisecond, "FirstAvailableSlot must become (200, true)")
}

// The ledger floor is the one poll whose absence must never cost us an
// upstream. Each of these leaves known=false, which every consumer reads as
// "no constraint".
func TestSvmStatePoller_FirstAvailableSlot_UnknownUntilObserved(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()

	up.script("getSlot:processed", []byte(`1000`))
	up.script("getSlot:finalized", []byte(`990`))
	up.script("getMaxShredInsertSlot", []byte(`1002`))

	p := newPollerWithUpstream(t, up)
	// Cold: never polled.
	floor, known := p.FirstAvailableSlot()
	require.False(t, known, "a cold poller must report the floor as unknown")
	require.Zero(t, floor)

	// An upstream that does not implement the method stays unknown.
	up.scriptError("oldestBlock", -32601, "Method not found")
	require.NoError(t, p.Poll(context.Background()))
	floor, known = p.FirstAvailableSlot()
	require.False(t, known, "-32601 must leave the floor unknown, not record 0")
	require.Zero(t, floor)

	// The failed fetch marks the counter fresh. The next tick takes the
	// not-stale fast path and must not promote that stale 0 to an observation.
	require.NoError(t, p.Poll(context.Background()))
	floor, known = p.FirstAvailableSlot()
	require.False(t, known, "a failed probe must stay unknown on the next tick, not become a known 0")
	require.Zero(t, floor)
}

// Every upstream observes its floor, declared bound or not, and the probe is
// debounced by the staleness window rather than issued on every tick.
func TestSvmStatePoller_FirstAvailableSlot_PolledWithoutDeclaredBound(t *testing.T) {
	t.Parallel()
	exact := int64(0)
	cases := map[string]*common.UpstreamConfig{
		"no svm block availability": nil,
		"exactSlot lower only": {
			Id: "test-poller", Type: common.UpstreamTypeSvm, Endpoint: "http://x",
			Svm: &common.SvmUpstreamConfig{BlockAvailability: &common.SvmBlockAvailabilityConfig{
				Lower: &common.SvmAvailabilityBoundConfig{ExactSlot: &exact},
			}},
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			up := newScriptedUpstream()
			up.cfg = cfg

			up.script("getSlot:processed", []byte(`1000`))
			up.script("getSlot:finalized", []byte(`990`))
			up.script("getMaxShredInsertSlot", []byte(`1002`))
			up.script("oldestBlock", []byte(`[509]`))

			p := newPollerWithUpstream(t, up)
			for range 3 {
				require.NoError(t, p.Poll(context.Background()))
			}
			require.Equal(t, 1, up.callCount("oldestBlock"), "one probe per staleness window")
			floor, known := p.FirstAvailableSlot()
			require.True(t, known)
			require.EqualValues(t, 509, floor)
		})
	}
}

// Zero is a real floor — a genesis-backed archive reports it — so it must be
// distinguishable from "never observed". This is the whole reason
// FirstAvailableSlot returns a second value instead of just the counter.
func TestSvmStatePoller_FirstAvailableSlot_ZeroIsKnown(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()

	up.script("getSlot:processed", []byte(`1000`))
	up.script("getSlot:finalized", []byte(`990`))
	up.script("getMaxShredInsertSlot", []byte(`1002`))
	up.script("oldestBlock", []byte(`[0]`))

	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Poll(context.Background()))

	// Under -race the foreground wait can time out while the fetch completes
	// asynchronously; the closure then marks the floor known.
	require.Eventually(t, func() bool {
		_, known := p.FirstAvailableSlot()
		return known
	}, 5*time.Second, 10*time.Millisecond, "an observed floor of 0 must read as KNOWN")
	floor, _ := p.FirstAvailableSlot()
	require.Zero(t, floor)
}

// An empty getBlocksWithLimit(0, 1) is no observation, not a genesis floor:
// agave swallows a long-term-storage error into an empty list. Recording it as
// known 0 would make firstAvailableSlotPlus enforce a window from slot 0 and
// violate the unknown/fail-open contract.
func TestSvmStatePoller_FirstAvailableSlot_EmptyLeavesUnknown(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()

	up.script("getSlot:processed", []byte(`1000`))
	up.script("getSlot:finalized", []byte(`990`))
	up.script("getMaxShredInsertSlot", []byte(`1002`))
	up.script("oldestBlock", []byte(`[]`))

	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Poll(context.Background()))

	floor, known := p.FirstAvailableSlot()
	require.False(t, known, "an empty list must leave the floor unknown, not record 0")
	require.Zero(t, floor)
}

// A value that arrives through the COUNTER rather than through this instance's
// own fetch must still mark the floor known. On a multi-instance deployment the
// counter is seeded from the shared connector and refreshed by peer pushes, so
// a replica may legitimately never perform the fetch itself.
func TestSvmStatePoller_FirstAvailableSlot_KnownFromCounterPropagation(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	p := newPollerWithUpstream(t, up)

	_, known := p.FirstAvailableSlot()
	require.False(t, known, "nothing observed yet")

	// Stands in for a peer's value arriving via cross-instance propagation:
	// both paths land on the counter's accepted-value callback.
	p.firstAvailableSlotShared.TryUpdate(context.Background(), 200000)

	floor, known := p.FirstAvailableSlot()
	require.True(t, known, "an accepted floor value must mark the floor known, whatever produced it")
	require.EqualValues(t, 200000, floor)
}

// The replica case, end to end: when the counter is already fresh,
// TryUpdateIfStale takes its not-stale fast path and never runs the fetch
// closure. The floor must still read as known, or it reads as "no constraint"
// and the silent-truncation case reopens on every instance that is not the one
// doing the fetching.
func TestSvmStatePoller_FirstAvailableSlot_KnownWithoutLocalFetch(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()

	up.script("getSlot:processed", []byte(`300000`))
	up.script("getSlot:finalized", []byte(`299900`))
	up.script("getMaxShredInsertSlot", []byte(`300001`))
	up.script("oldestBlock", []byte(`[200000]`))

	p := newPollerWithUpstream(t, up)
	// Seed the counter the way the shared-state registry does before this
	// instance's first tick, which also marks it fresh.
	p.firstAvailableSlotShared.TryUpdate(context.Background(), 200000)

	require.NoError(t, p.Poll(context.Background()))

	require.Zero(t, up.callCount("oldestBlock"),
		"a fresh counter must suppress the local fetch — that is the condition this guards")
	floor, known := p.FirstAvailableSlot()
	require.True(t, known, "the floor must be known even though this instance never fetched it")
	require.EqualValues(t, 200000, floor)
}

// The propagated-zero case: a peer's floor of 0 lands on a counter whose value
// is already 0, so no value-change callback fires — yet the counter is fresh,
// so the first tick's TryUpdateIfStale takes its fast path and never fetches.
// The floor must still become known on that tick, and the gauge must be
// maintained, or firstAvailableSlotPlus silently disables
// on every instance except the one doing the fetching.
func TestSvmStatePoller_FirstAvailableSlot_PropagatedZeroBecomesKnownOnTick(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()

	up.script("getSlot:processed", []byte(`300000`))
	up.script("getSlot:finalized", []byte(`299900`))
	up.script("getMaxShredInsertSlot", []byte(`300001`))
	up.script("oldestBlock", []byte(`[0]`))

	p := newPollerWithUpstream(t, up)
	// Stands in for a peer's 0 arriving via propagation: value unchanged
	// (0 -> 0), no callback, but the counter is now fresh.
	p.firstAvailableSlotShared.TryUpdate(context.Background(), 0)

	_, known := p.FirstAvailableSlot()
	require.False(t, known, "a no-change propagation fires no callback, so nothing is known yet")

	require.NoError(t, p.Poll(context.Background()))
	require.Zero(t, up.callCount("oldestBlock"),
		"a fresh counter must suppress the local fetch — that is the condition this guards")

	floor, known := p.FirstAvailableSlot()
	require.True(t, known, "a valid shared observation, even of 0, must mark the floor known on the tick")
	require.Zero(t, floor)
}

// The gauge twin of the replica case: when the value arrives through the
// counter rather than this instance's fetch, the gauge must still be exported
// — otherwise the series exists only on the pod that happens to fetch.
//
// Not parallel: the assertion reads a process-global gauge series whose labels
// (test / empty vendor/network / test-poller) are shared by every poller test
// in this file. Another parallel test writing a different floor between Poll
// and ToFloat64 would flake the equality check.
func TestSvmStatePoller_FirstAvailableSlot_GaugeSetWithoutLocalFetch(t *testing.T) {
	up := newScriptedUpstream()

	up.script("getSlot:processed", []byte(`300000`))
	up.script("getSlot:finalized", []byte(`299900`))
	up.script("getMaxShredInsertSlot", []byte(`300001`))
	up.script("oldestBlock", []byte(`[200000]`))

	p := newPollerWithUpstream(t, up)
	p.firstAvailableSlotShared.TryUpdate(context.Background(), 200000)

	require.NoError(t, p.Poll(context.Background()))
	require.Zero(t, up.callCount("oldestBlock"),
		"a fresh counter must suppress the local fetch — that is the condition this guards")
	require.Equal(t, float64(200000),
		testutil.ToFloat64(p.gauge(telemetry.MetricUpstreamSvmFirstAvailableSlot)),
		"the gauge must be exported even when this instance never fetched")
}

// A floor above the node's own head is structurally impossible, so the sample
// is rejected at the edge. Without this one buggy or hostile upstream could
// advertise a floor near MaxInt64 and evict itself from every slot-named read.
func TestSvmStatePoller_FirstAvailableSlot_AboveOwnLatestRejected(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()

	up.script("getSlot:processed", []byte(`1000`))
	up.script("getSlot:finalized", []byte(`990`))
	up.script("getMaxShredInsertSlot", []byte(`1002`))
	up.script("oldestBlock", []byte(`[5000]`))

	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Poll(context.Background()))

	floor, known := p.FirstAvailableSlot()
	require.False(t, known, "a floor above the upstream's own latest slot must be rejected")
	require.Zero(t, floor)
}

// The floor moves O(1) slot per slot while sitting O(100k) slots below the
// head, so by default it is probed once and then frozen, as EVM's
// earliestBlockPlus is at updateRate 0. A firstAvailableSlotPlus bound's
// updateRate re-probes it on that cadence; nothing else does.
func TestSvmStatePoller_FirstAvailableSlot_PolledLessOftenThanHealth(t *testing.T) {
	t.Parallel()
	plus := int64(0)
	exact := int64(0)
	withRate := func(b *common.SvmAvailabilityBoundConfig) *common.UpstreamConfig {
		return &common.UpstreamConfig{
			Id: "test-poller", Type: common.UpstreamTypeSvm, Endpoint: "http://x",
			Svm: &common.SvmUpstreamConfig{BlockAvailability: &common.SvmBlockAvailabilityConfig{Lower: b}},
		}
	}
	cases := map[string]struct {
		cfg       *common.UpstreamConfig
		wantFloor int
	}{
		"default: frozen after first observation": {nil, 1},
		"firstAvailableSlotPlus updateRate 1ns": {
			withRate(&common.SvmAvailabilityBoundConfig{FirstAvailableSlotPlus: &plus, UpdateRate: common.Duration(time.Nanosecond)}),
			10,
		},
		"updateRate on a bound without the floor is ignored": {
			withRate(&common.SvmAvailabilityBoundConfig{ExactSlot: &exact, UpdateRate: common.Duration(time.Nanosecond)}),
			1,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			up := newScriptedUpstream()
			up.cfg = tc.cfg

			up.script("getSlot:processed", []byte(`1000`))
			up.script("getSlot:finalized", []byte(`990`))
			up.script("getMaxShredInsertSlot", []byte(`1002`))
			up.script("oldestBlock", []byte(`[200]`))

			p := newPollerWithUpstream(t, up)
			const polls = 10
			for range polls {
				require.NoError(t, p.Poll(context.Background()))
				time.Sleep(time.Millisecond)
			}

			require.Equal(t, tc.wantFloor, up.callCount("oldestBlock"),
				"the floor is debounced by its own staleness window, not by the poll gate")
		})
	}
}

// A single failing probe is not a verdict: while any other probe answers, the
// upstream is alive. getMaxShredInsertSlot erroring -32601 here stands in for
// a vendor that does not implement it — an RPC error is an answer, so that
// must never cordon.
func TestSvmStatePoller_Poll_PartialProbeFailureStaysHealthy(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	up.scriptError("getMaxShredInsertSlot", -32601, "Method not found")
	up.script("getSlot:processed", []byte(`2000`))
	up.script("getSlot:finalized", []byte(`1990`))

	p := newPollerWithUpstream(t, up)
	for range deadPollsBeforeUnhealthy + 1 {
		require.NoError(t, p.Poll(context.Background()))
	}

	require.True(t, p.IsHealthy(), "a single dead probe must never flip the verdict while others answer")
	require.Empty(t, up.cordons(), "a vendor missing one method must not be cordoned for it")
	// Slot reads are unaffected by the failing probe — the poller reports each
	// signal independently, not bail on first error.
	require.EqualValues(t, 2000, p.LatestSlot())
}

// The liveness threshold: one all-failed poll (a vendor 429 against the poller
// is the common case) must NOT flip the verdict; two consecutive ones must
// cordon the upstream out of rotation.
func TestSvmStatePoller_Liveness_DeadAfterTwoAllFailedPolls(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	// Nothing scripted: every probe errors with no response object.
	p := newPollerWithUpstream(t, up)

	require.NoError(t, p.Poll(context.Background()))
	require.True(t, p.IsHealthy(), "one all-failed poll is not a verdict")
	require.Empty(t, up.cordons())

	require.NoError(t, p.Poll(context.Background()))
	require.False(t, p.IsHealthy(), "two consecutive all-failed polls must flip the verdict")
	cordons := up.cordons()
	require.Len(t, cordons, 1, "a dead upstream must be cordoned out of rotation")
	require.Equal(t, "*", cordons[0].method)
	require.Contains(t, cordons[0].reason, "not answering state probes")
}

// Recovery is detected on the first answering probe, and the uncordon lifts
// only the poller's own cordon (edge-triggered).
func TestSvmStatePoller_Liveness_RecoversOnFirstAnswer(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	p := newPollerWithUpstream(t, up)

	require.NoError(t, p.Poll(context.Background()))
	require.NoError(t, p.Poll(context.Background()))
	require.False(t, p.IsHealthy())
	require.Len(t, up.cordons(), 1)

	scriptAllProbes(up)
	require.NoError(t, p.Poll(context.Background()))
	require.True(t, p.IsHealthy(), "the first answering poll must clear the verdict")
	require.Len(t, up.uncordons(), 1, "recovery must lift the poller's own cordon")
}

// An RPC error is still an ANSWER — the endpoint is alive, whatever the node
// thinks of the method. Only transport silence reads as dead.
func TestSvmStatePoller_Liveness_RpcErrorAnswersCountAsAlive(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	up.scriptError("getSlot:processed", -32005, "Node is behind by 42 slots")
	up.scriptError("getSlot:finalized", -32005, "Node is behind by 42 slots")
	up.scriptError("getMaxShredInsertSlot", -32601, "Method not found")
	p := newPollerWithUpstream(t, up)

	for range deadPollsBeforeUnhealthy + 1 {
		require.NoError(t, p.Poll(context.Background()))
	}
	require.True(t, p.IsHealthy(), "RPC errors are answers; only silence is dead")
	require.Empty(t, up.cordons())
}

// Errors raised LOCALLY before dispatch (circuit breaker open, the upstream's
// own rate-limit budget) never reached the endpoint: they are evidence of
// nothing, so they must move the counter neither way — neither proving life
// nor counting as death.
func TestSvmStatePoller_Liveness_LocalRejectionsAreNoSignal(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	cbErr := common.NewErrFailsafeCircuitBreakerOpen(common.ScopeUpstream, nil, nil)
	rlErr := common.NewErrUpstreamRateLimitRuleExceeded("up", "budget", "rule")

	// Healthy upstream with all probes rejected locally: the healthy verdict
	// must NOT flip (no evidence of death), but the counter must not reset
	// either — verified by driving it to the edge first.
	up.scriptClassifiedError("getSlot:processed", cbErr)
	up.scriptClassifiedError("getSlot:finalized", rlErr)
	up.scriptClassifiedError("getMaxShredInsertSlot", cbErr)
	p := newPollerWithUpstream(t, up)

	for range deadPollsBeforeUnhealthy + 2 {
		require.NoError(t, p.Poll(context.Background()))
	}
	require.True(t, p.IsHealthy(), "local rejections are not evidence of death")
	require.Zero(t, p.consecutiveDeadPolls, "local rejections must not advance the dead counter")
	require.Empty(t, up.cordons())

	// From the unhealthy side: drive to a dead verdict, then switch to local
	// rejections — the counter must HOLD (not reset), so the next answering
	// poll is what recovers, not the no-signal stretch.
	up2 := newScriptedUpstream() // unscripted = transport silence
	p2 := newPollerWithUpstream(t, up2)
	require.NoError(t, p2.Poll(context.Background()))
	require.NoError(t, p2.Poll(context.Background()))
	require.False(t, p2.IsHealthy())
	dead := p2.consecutiveDeadPolls
	up2.scriptClassifiedError("getSlot:processed", cbErr)
	up2.scriptClassifiedError("getSlot:finalized", cbErr)
	up2.scriptClassifiedError("getMaxShredInsertSlot", rlErr)
	require.NoError(t, p2.Poll(context.Background()))
	require.Equal(t, dead, p2.consecutiveDeadPolls, "no-signal must not reset the counter")
	require.False(t, p2.IsHealthy(), "the last verdict stands through a no-signal stretch")
}

// Alpenglow mode: the processed probe is skipped entirely, and the finalized
// sample double-feeds the latest view — correct within ~1 slot on a
// fast-finality chain, and one fewer upstream call per poll.
func TestSvmStatePoller_SkipProcessedSlotProbe_SkipsAndDoubleFeeds(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	up.script("getSlot:processed", []byte(`1024`)) // must NOT be called
	up.script("getSlot:finalized", []byte(`990`))
	up.script("getMaxShredInsertSlot", []byte(`998`))
	p := newPollerWithUpstream(t, up)
	p.SetSkipProcessedSlotProbe(true)

	require.NoError(t, p.Poll(context.Background()))

	require.Zero(t, up.callCount("getSlot:processed"), "skipProcessedSlotProbe=true must skip the processed probe")
	require.Equal(t, 1, up.callCount("getSlot:finalized"))
	require.Equal(t, 1, up.callCount("getMaxShredInsertSlot"))
	require.EqualValues(t, 990, p.LatestSlot(), "finalized sample must double-feed the latest view")
	require.EqualValues(t, 990, p.FinalizedSlot())
	require.EqualValues(t, 8, p.MaxShredInsertSlotLag(), "lag = shred 998 - latest 990")
	require.True(t, p.IsHealthy())
}

// Default remains full polling: with no setter call the processed probe runs
// (the atomic zero value must never read as "skip").
func TestSvmStatePoller_SkipProcessedSlotProbe_DefaultOffPollsProcessed(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllProbes(up)
	p := newPollerWithUpstream(t, up)

	require.NoError(t, p.Poll(context.Background()))
	require.Equal(t, 1, up.callCount("getSlot:processed"), "default must poll the processed view")
	require.EqualValues(t, 1000, p.LatestSlot())
}

// Liveness derivation is unaffected by the skipped probe: the remaining two
// probes still carry the verdict, and silence on both cordons after
// deadPollsBeforeUnhealthy polls.
func TestSvmStatePoller_SkipProcessedSlotProbe_LivenessFromRemainingProbes(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	p := newPollerWithUpstream(t, up)
	p.SetSkipProcessedSlotProbe(true)

	require.NoError(t, p.Poll(context.Background()))
	require.True(t, p.IsHealthy(), "one all-failed poll is not a verdict")
	require.NoError(t, p.Poll(context.Background()))
	require.False(t, p.IsHealthy(), "two consecutive all-failed polls must cordon")
	require.Len(t, up.cordons(), 1)
}

// A skipped processed probe must not invent a dead signal: when the remaining
// probes are local rejections (no-signal), the counter and verdict hold —
// the zero-value probeDead must not poison anyDead.
func TestSvmStatePoller_SkipProcessedSlotProbe_NoSignalDoesNotCordon(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	cbErr := common.NewErrFailsafeCircuitBreakerOpen(common.ScopeUpstream, nil, nil)
	up.scriptClassifiedError("getSlot:finalized", cbErr)
	up.scriptClassifiedError("getMaxShredInsertSlot", cbErr)
	p := newPollerWithUpstream(t, up)
	p.SetSkipProcessedSlotProbe(true)

	for range deadPollsBeforeUnhealthy + 2 {
		require.NoError(t, p.Poll(context.Background()))
	}
	require.True(t, p.IsHealthy(), "skipped probe + no-signal remaining must not cordon")
	require.Zero(t, p.consecutiveDeadPolls)
	require.Empty(t, up.cordons())
	require.Zero(t, up.callCount("getSlot:processed"))
}

// Bare context.DeadlineExceeded (retry backoff slept past the poll ctx) is
// transport silence, not an answer — otherwise a dead upstream with retries
// never cordons.
func TestSvmStatePoller_Liveness_BareDeadlineIsDead(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	up.scriptClassifiedError("getSlot:processed", context.DeadlineExceeded)
	up.scriptClassifiedError("getSlot:finalized", context.DeadlineExceeded)
	up.scriptClassifiedError("getMaxShredInsertSlot", context.DeadlineExceeded)
	p := newPollerWithUpstream(t, up)

	require.NoError(t, p.Poll(context.Background()))
	require.True(t, p.IsHealthy(), "one all-deadline poll is not a verdict")
	require.NoError(t, p.Poll(context.Background()))
	require.False(t, p.IsHealthy(), "two consecutive bare-deadline polls must cordon")
	require.Len(t, up.cordons(), 1)
}

func TestSvmStatePoller_Poll_ExcessiveShredLagFlipsHealthy(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()

	// Processed = 10000 while the shred watermark has reached 10500 → ingestion
	// lag 500 > threshold 100. This is the silent-stale signature: shreds keep
	// arriving while replay is stalled, and the state probes still answer "ok".
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

// scriptAllProbes registers healthy canned responses for every poller request
// so the traffic-gate tests can focus on WHICH calls fire, not what they return.
func scriptAllProbes(up *scriptedUpstream) {
	up.script("getSlot:processed", []byte(`1000`))
	up.script("getSlot:finalized", []byte(`990`))
	up.script("getMaxShredInsertSlot", []byte(`998`))
}

// TestSvmStatePoller_Poll_TrafficGate_ClosedWithoutSuggestions: enabling the
// debounce gate alone must not suppress anything — without external freshness
// evidence Poll still fans out all three calls.
func TestSvmStatePoller_Poll_TrafficGate_ClosedWithoutSuggestions(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllProbes(up)
	p := newPollerWithUpstream(t, up)
	p.SetDebounceInterval(200 * time.Millisecond)

	// First Poll is never debounce-blocked (no prior poll recorded).
	require.NoError(t, p.Poll(context.Background()))

	for _, key := range []string{"getSlot:processed", "getSlot:finalized", "getMaxShredInsertSlot"} {
		if got := up.callCount(key); got != 1 {
			t.Errorf("%s called %d times, want 1 (gate must stay closed without suggestions)", key, got)
		}
	}
}

// TestSvmStatePoller_Poll_TrafficGate_SkipsGetSlotWhenBothViewsFresh: when live
// traffic refreshed BOTH slot views within the debounce window, Poll skips the
// two getSlot calls but still issues getMaxShredInsertSlot (traffic carries no
// such signal), and the slot surface keeps serving the traffic-fed values.
func TestSvmStatePoller_Poll_TrafficGate_SkipsGetSlotWhenBothViewsFresh(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllProbes(up)
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
// observations within the window, else Poll fans out all three calls.
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
			scriptAllProbes(up)
			p := newPollerWithUpstream(t, up)
			p.SetDebounceInterval(200 * time.Millisecond)

			tc.suggest(p)
			require.NoError(t, p.Poll(context.Background()))

			for _, key := range []string{"getSlot:processed", "getSlot:finalized", "getMaxShredInsertSlot"} {
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
	scriptAllProbes(up)
	p := newPollerWithUpstream(t, up)

	// Wide enough that a loaded -race run cannot age the stamps made right
	// before each Poll out of the gate window.
	const debounce = 400 * time.Millisecond
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

		// Guard against a vacuous pass: getMaxShredInsertSlot runs on every poll
		// that runs, so a debounce-dropped iteration is caught here, not
		// mistaken for a gated one.
		if got := up.callCount("getMaxShredInsertSlot"); got != i+1 {
			t.Fatalf("poll %d did not run: getMaxShredInsertSlot=%d, want %d", i+1, got, i+1)
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
	scriptAllProbes(up)
	p := newPollerWithUpstream(t, up)

	const debounce = 100 * time.Millisecond
	p.SetDebounceInterval(debounce)

	require.NoError(t, p.Poll(context.Background())) // full poll: updates slots internally
	time.Sleep(debounce + 40*time.Millisecond)       // clear the whole-poll debounce
	require.NoError(t, p.Poll(context.Background()))

	if got := up.callCount("getMaxShredInsertSlot"); got != 2 {
		t.Fatalf("second poll did not run: getMaxShredInsertSlot=%d, want 2", got)
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
// TestSvmStatePoller_Poll_AllProbesRunEveryPoll pins the hot cadence: every
// state probe runs on EVERY poll because its value moves at chain rate and
// each is a live bound — getSlot(finalized) for the getBlock guard,
// getSlot(processed) for non-finalized routing, and getMaxShredInsertSlot for
// every commitment level, whose tipStalenessMargin assumes the snapshot is at
// most one debounce old.
func TestSvmStatePoller_Poll_AllProbesRunEveryPoll(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	scriptAllProbes(up)
	p := newPollerWithUpstream(t, up)
	// Debounce left at 0: no whole-poll gate and no traffic gate, so every Poll
	// runs its full body.

	const polls = 10
	for range polls {
		require.NoError(t, p.Poll(context.Background()))
	}

	require.Equal(t, polls, up.callCount("getSlot:processed"), "getSlot(processed) must run every poll")
	require.Equal(t, polls, up.callCount("getSlot:finalized"), "getSlot(finalized) must run every poll")
	require.Equal(t, polls, up.callCount("getMaxShredInsertSlot"),
		"getMaxShredInsertSlot moves at chain rate and bounds non-finalized requests — it must NOT be throttled")
}

func TestSvmStatePoller_HealthToRouting_EdgeTriggered(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()

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

// blockingUpstream parks inside Forward until the CALLER's context is done, so
// a test can hold a poller mid-poll and prove cancellation interrupts work in
// flight rather than only an idle ticker.
type blockingUpstream struct {
	fakeUpstreamForPoller
	entered     chan struct{} // closed once Forward has been entered at least once
	enteredOnce sync.Once
}

func newBlockingUpstream() *blockingUpstream {
	return &blockingUpstream{entered: make(chan struct{})}
}

func (b *blockingUpstream) Forward(ctx context.Context, _ *common.NormalizedRequest, _, _ bool) (*common.NormalizedResponse, error) {
	b.enteredOnce.Do(func() { close(b.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestSvmStatePoller_AppCtxCancellation_DrainsEveryLoop pins process shutdown.
// Every SVM upstream owns a poller and every poller owns a ticker goroutine; on
// appCtx cancellation ALL of them must exit, including one parked mid-poll on an
// upstream that has not answered yet. A loop that only checks appCtx between
// ticks (or a Poll that ignores the derived context) would leave the blocked
// poller running until process exit.
func TestSvmStatePoller_AppCtxCancellation_DrainsEveryLoop(t *testing.T) {
	// No t.Parallel: asserts a goroutine-lifecycle invariant with real tickers.
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	responsive := newScriptedUpstream()
	scriptAllProbes(responsive)
	stuck := newBlockingUpstream()

	pollers := []*SvmStatePoller{
		newPollerOnCtx(t, appCtx, responsive),
		newPollerOnCtx(t, appCtx, stuck),
	}
	for _, p := range pollers {
		require.NoError(t, p.Bootstrap(context.Background()))
	}

	liveLoops := func() int32 {
		var n int32
		for _, p := range pollers {
			n += p.loopsRunning.Load()
		}
		return n
	}
	require.Eventually(t, func() bool { return liveLoops() == int32(len(pollers)) },
		2*time.Second, 5*time.Millisecond, "every bootstrapped poller must bring up its loop")

	// Do not cancel until the stuck poller is genuinely inside Poll — otherwise
	// the test would only re-prove the idle-ticker teardown.
	select {
	case <-stuck.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking upstream was never called; the poller never entered a poll")
	}

	cancel()
	require.Eventually(t, func() bool { return liveLoops() == 0 },
		5*time.Second, 5*time.Millisecond,
		"appCtx cancellation must drain every poll loop, including one blocked mid-poll")
}

// TestSvmStatePoller_ConcurrentSuggestionsDuringPoll_StaySlotMonotonic runs the
// slot surface the way production does — many concurrent traffic-fed
// suggestions (upstreamPostForward_trackContextSlot fires on every response)
// racing the poller's own fan-out — and asserts the invariant survives, not
// merely that nothing crashed:
//
//   - no reader ever observes a slot moving BACKWARDS, and
//   - the final value is exactly the highest slot anyone suggested, so no
//     concurrent update is lost to a failed compare-and-swap.
//
// Every value stays inside a DefaultToleratedSlotRollback-wide window on
// purpose: a drop LARGER than that is a deliberate reorg signal the counter
// accepts, so a wider spread would make backwards movement correct behavior and
// the invariant meaningless.
//
// Run under -race; the shared-counter CAS loops and the poller's atomics are
// what this is aimed at.
func TestSvmStatePoller_ConcurrentSuggestionsDuringPoll_StaySlotMonotonic(t *testing.T) {
	// No t.Parallel: the point is to saturate cores and widen interleavings.
	const (
		seed      = uint64(0x51075EED) // fixed so a red run reproduces exactly
		writers   = 8
		perWriter = 250
		spread    = 400 // < DefaultToleratedSlotRollback (1024)

		latestBase    = int64(10_000)
		finalizedBase = int64(9_900)
		// The poller's own observations land mid-window, so the maximum can only
		// come from a writer — that is what makes the final equality check
		// sensitive to a lost update.
		polledProcessed = latestBase + spread/2
		polledFinalized = finalizedBase + spread/2
	)
	t.Logf("deterministic seed: %#x (rerun reproduces the exact suggestion order)", seed)

	up := newScriptedUpstream()

	up.script("getSlot:processed", fmt.Appendf(nil, `%d`, polledProcessed))
	up.script("getSlot:finalized", fmt.Appendf(nil, `%d`, polledFinalized))
	up.script("getMaxShredInsertSlot", fmt.Appendf(nil, `%d`, polledProcessed))

	p := newPollerWithUpstream(t, up)

	wantLatest := latestBase + spread - 1
	wantFinalized := finalizedBase + spread - 1

	var (
		wg         sync.WaitGroup
		violations = make(chan string, 64)
		stop       = make(chan struct{})
	)

	// Reader: the monotonicity contract must hold at EVERY observation, not just
	// after the dust settles.
	wg.Add(1)
	go func() {
		defer wg.Done()
		var prevLatest, prevFinalized int64
		for {
			select {
			case <-stop:
				return
			default:
			}
			if got := p.LatestSlot(); got < prevLatest {
				select {
				case violations <- fmt.Sprintf("latest slot went backwards: %d → %d", prevLatest, got):
				default:
				}
			} else {
				prevLatest = got
			}
			if got := p.FinalizedSlot(); got < prevFinalized {
				select {
				case violations <- fmt.Sprintf("finalized slot went backwards: %d → %d", prevFinalized, got):
				default:
				}
			} else {
				prevFinalized = got
			}
		}
	}()

	// Poller: keeps fanning out real polls (which write both slot views through
	// the private suggest path) while the writers hammer the public one.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := p.Poll(context.Background()); err != nil {
				select {
				case violations <- fmt.Sprintf("poll failed: %v", err):
				default:
				}
				return
			}
		}
	}()

	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Per-writer deterministic stream: each goroutine walks its own
			// shuffled slice of the window, so out-of-order suggestions (the
			// case the counter must reject) are guaranteed, reproducibly.
			rng := rand.New(rand.NewPCG(seed, uint64(w)))
			for range perWriter {
				off := int64(rng.IntN(spread))
				p.SuggestLatestSlot(latestBase + off)
				p.SuggestFinalizedSlot(finalizedBase + off)
			}
			// Guarantee the window maximum is offered by every writer, so the
			// final-value assertion does not depend on the RNG covering it.
			p.SuggestLatestSlot(wantLatest)
			p.SuggestFinalizedSlot(wantFinalized)
		}(w)
	}

	// Writers finish on their own; the reader and poller run until told to stop.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	// Let the writers drain, then release the two open-ended goroutines.
	require.Eventually(t, func() bool {
		return p.LatestSlot() == wantLatest && p.FinalizedSlot() == wantFinalized
	}, 30*time.Second, time.Millisecond,
		"slot surface never reached the highest suggested value; a concurrent update was lost")
	close(stop)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("workers did not finish")
	}
	close(violations)

	for v := range violations {
		t.Errorf("slot monotonicity violated: %s", v)
	}
	require.Equal(t, wantLatest, p.LatestSlot())
	require.Equal(t, wantFinalized, p.FinalizedSlot())
}

// TestSvmStatePoller_ConcurrentBootstrap_StartsExactlyOneLoop is the racing twin
// of TestSvmStatePoller_Bootstrap_StartsAtMostOneLoop. The upstream initializer
// can re-run bootstrap from more than one goroutine (retry after a failed
// genesis validation, plus PrepareUpstreamsForNetwork on another network), so
// the guard has to be a compare-and-swap, not a load-then-store — which only
// the race detector and real concurrency can tell apart.
func TestSvmStatePoller_ConcurrentBootstrap_StartsExactlyOneLoop(t *testing.T) {
	// No t.Parallel: goroutine-lifecycle invariant.
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	up := newScriptedUpstream()
	scriptAllProbes(up)
	p := newPollerOnCtx(t, appCtx, up)

	const racers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			require.NoError(t, p.Bootstrap(context.Background()))
		}()
	}
	close(start)
	wg.Wait()

	require.Eventually(t, func() bool { return p.loopsRunning.Load() == 1 },
		2*time.Second, 5*time.Millisecond, "exactly one poll loop must come up")
	// Give any loser goroutine time to register itself before re-asserting.
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(1), p.loopsRunning.Load(),
		"concurrent Bootstrap calls each leaked a ticker goroutine")

	cancel()
	require.Eventually(t, func() bool { return p.loopsRunning.Load() == 0 },
		2*time.Second, 5*time.Millisecond, "appCtx cancellation must stop the surviving loop")
}
