package policy

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubProbeUpstream is a minimal common.Upstream impl that counts
// Forward() calls. Used to assert the prober actually mirrors.
type stubProbeUpstream struct {
	id      string
	routing *common.UpstreamRoutingConfig
	calls   atomic.Int64
	// blockUntil is fired before each Forward returns, so a test can
	// hold a probe in-flight to assert the inflight counter is
	// enforced.
	blockUntil chan struct{}
}

func (s *stubProbeUpstream) Id() string           { return s.id }
func (s *stubProbeUpstream) VendorName() string   { return "v" + s.id }
func (s *stubProbeUpstream) NetworkId() string    { return "evm:1" }
func (s *stubProbeUpstream) NetworkLabel() string { return "evm:1" }
func (s *stubProbeUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: s.id, Routing: s.routing}
}
func (s *stubProbeUpstream) Logger() *zerolog.Logger { l := zerolog.Nop(); return &l }
func (s *stubProbeUpstream) Vendor() common.Vendor   { return nil }
func (s *stubProbeUpstream) Tracker() common.HealthTracker {
	return nil
}
func (s *stubProbeUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPass, isHedge bool) (*common.NormalizedResponse, error) {
	s.calls.Add(1)
	if s.blockUntil != nil {
		select {
		case <-s.blockUntil:
		case <-ctx.Done():
		}
	}
	return nil, nil
}
func (s *stubProbeUpstream) Cordon(method, reason string)   {}
func (s *stubProbeUpstream) Uncordon(method, reason string) {}
func (s *stubProbeUpstream) IgnoreMethod(method string)     {}

// stubEngine implements the narrow proberDeps interface so we can
// drive the prober without standing up a full Engine.
type stubEngine struct {
	mu       sync.Mutex
	excluded map[string][]common.Upstream
}

func (s *stubEngine) GetExcluded(networkID, method, finality string) []common.Upstream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.excluded[networkID]
}

func (s *stubEngine) setExcluded(networkID string, ups []common.Upstream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.excluded == nil {
		s.excluded = make(map[string][]common.Upstream)
	}
	s.excluded[networkID] = ups
}

func newTestProber(t *testing.T, deps *stubEngine, cfg *ProbeConfig) (*Prober, *health.Tracker) {
	t.Helper()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	p := newProber(context.Background(), "evm:1", &logger, deps, tracker, cfg)
	return p, tracker
}

func makeProbeReq(t *testing.T, method string) *common.NormalizedRequest {
	t.Helper()
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`)
	return common.NewNormalizedRequest(body)
}

// waitForCalls busy-polls a stub upstream's call counter until it
// reaches `want` or `timeout` elapses. Returns the final value.
func waitForCalls(s *stubProbeUpstream, want int64, timeout time.Duration) int64 {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c := s.calls.Load(); c >= want {
			return c
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s.calls.Load()
}

// TestProber_MirrorsToExcludedUpstreams — the happy path: a published
// request fans out to every currently-excluded upstream.
func TestProber_MirrorsToExcludedUpstreams(t *testing.T) {
	deps := &stubEngine{}
	dead := &stubProbeUpstream{id: "dead"}
	deps.setExcluded("evm:1", []common.Upstream{dead})

	p, _ := newTestProber(t, deps, &ProbeConfig{
		SampleRate:    1.0,
		MaxConcurrent: 4,
		Timeout:       5 * time.Second,
	})
	defer p.Stop()

	p.Publish(makeProbeReq(t, "eth_getBalance"))
	got := waitForCalls(dead, 1, 1*time.Second)
	assert.Equal(t, int64(1), got, "prober must mirror one request to the excluded upstream")
}

// TestProber_SkipsWriteMethods — eth_sendRawTransaction (and friends)
// must NEVER be mirrored. Verifies the safety gate.
func TestProber_SkipsWriteMethods(t *testing.T) {
	deps := &stubEngine{}
	dead := &stubProbeUpstream{id: "dead"}
	deps.setExcluded("evm:1", []common.Upstream{dead})

	p, _ := newTestProber(t, deps, &ProbeConfig{
		SampleRate:    1.0,
		MaxConcurrent: 4,
		Timeout:       5 * time.Second,
	})
	defer p.Stop()

	for _, m := range []string{
		"eth_sendRawTransaction",
		"eth_sendTransaction",
		"eth_signTypedData_v4",
		"personal_sign",
	} {
		p.Publish(makeProbeReq(t, m))
	}
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, int64(0), dead.calls.Load(),
		"prober must skip write methods entirely (mutability risk)")
}

// TestProber_HonorsRoutingProbeOff — upstream with routing.probe="off"
// must never receive shadow traffic, even when excluded.
func TestProber_HonorsRoutingProbeOff(t *testing.T) {
	deps := &stubEngine{}
	dead := &stubProbeUpstream{
		id:      "dead",
		routing: &common.UpstreamRoutingConfig{Probe: common.ProbeModeOff},
	}
	deps.setExcluded("evm:1", []common.Upstream{dead})

	p, _ := newTestProber(t, deps, &ProbeConfig{
		SampleRate:    1.0,
		MaxConcurrent: 4,
		Timeout:       5 * time.Second,
	})
	defer p.Stop()

	p.Publish(makeProbeReq(t, "eth_getBalance"))
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, int64(0), dead.calls.Load(),
		"opted-out upstream (routing.probe=off) must receive zero probe traffic")
}

// TestProber_RespectsMaxConcurrent — with maxConcurrent=2 and a stub
// that blocks Forward indefinitely, the third publish should be
// gated. Once we release the held probes, the gate opens.
func TestProber_RespectsMaxConcurrent(t *testing.T) {
	deps := &stubEngine{}
	dead := &stubProbeUpstream{
		id:         "dead",
		blockUntil: make(chan struct{}),
	}
	deps.setExcluded("evm:1", []common.Upstream{dead})

	p, _ := newTestProber(t, deps, &ProbeConfig{
		SampleRate:    1.0,
		MaxConcurrent: 2,
		Timeout:       10 * time.Second,
	})
	defer p.Stop()

	// Publish 4 — only 2 should reach Forward; the others should be
	// skipped via the max_concurrent gate.
	p.Publish(makeProbeReq(t, "eth_getBalance"))
	p.Publish(makeProbeReq(t, "eth_getBalance"))
	// Give the dispatcher a moment to dispatch the first two before
	// the cap engages.
	time.Sleep(50 * time.Millisecond)
	p.Publish(makeProbeReq(t, "eth_getBalance"))
	p.Publish(makeProbeReq(t, "eth_getBalance"))

	// At this point we should have exactly 2 in-flight Forward calls,
	// and the other two should have been skipped via the
	// max_concurrent gate.
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, int64(2), dead.calls.Load(),
		"maxConcurrent=2 must cap concurrent probes; extras get skipped")

	// Release the held probes; they exit. Wait for the inflight
	// counter to drain so the new publishes don't race ahead of the
	// release.
	close(dead.blockUntil)
	dead.blockUntil = nil // subsequent Forwards return immediately
	counter := p.getInflight("dead")
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && counter.Load() > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, int64(0), counter.Load(),
		"inflight counter must drain after released probes complete")

	p.Publish(makeProbeReq(t, "eth_getBalance"))
	p.Publish(makeProbeReq(t, "eth_getBalance"))
	got := waitForCalls(dead, 4, 1*time.Second)
	assert.Equal(t, int64(4), got,
		"after held probes drained, subsequent publishes must dispatch normally")
}

// TestProber_FeedsTracker — every successful probe call increments
// the tracker's RequestsTotal for that (upstream, method). This is
// the WHOLE POINT: those samples are what drive `excludeIf` to
// naturally re-admit the upstream.
func TestProber_FeedsTracker(t *testing.T) {
	deps := &stubEngine{}
	dead := &stubProbeUpstream{id: "dead"}
	deps.setExcluded("evm:1", []common.Upstream{dead})

	p, tracker := newTestProber(t, deps, &ProbeConfig{
		SampleRate:    1.0,
		MaxConcurrent: 4,
		Timeout:       5 * time.Second,
	})
	defer p.Stop()

	for i := 0; i < 5; i++ {
		p.Publish(makeProbeReq(t, "eth_getBalance"))
	}
	waitForCalls(dead, 5, 1*time.Second)

	metrics := tracker.GetUpstreamMethodMetrics(dead, "eth_getBalance", common.DataFinalityStateUnknown)
	require.NotNil(t, metrics, "tracker must record probe samples under the (upstream, method) bucket")
	assert.GreaterOrEqual(t, metrics.RequestsTotal.Load(), int64(5),
		"each probe call must increment tracker.RequestsTotal — that's how the policy sees the upstream healing")
}

// TestProber_PublishNonBlockingOnFullBus — when the feed channel
// fills, additional Publishes drop on the floor; they MUST NOT
// block the calling goroutine (the request path).
func TestProber_PublishNonBlockingOnFullBus(t *testing.T) {
	deps := &stubEngine{}
	dead := &stubProbeUpstream{
		id:         "dead",
		blockUntil: make(chan struct{}), // jam the dispatcher
	}
	deps.setExcluded("evm:1", []common.Upstream{dead})

	p, _ := newTestProber(t, deps, &ProbeConfig{
		SampleRate:    1.0,
		MaxConcurrent: 1, // bottleneck
		Timeout:       30 * time.Second,
	})
	defer func() {
		close(dead.blockUntil)
		p.Stop()
	}()

	// Publish many more than the buffer + concurrency cap. Each
	// Publish should return immediately regardless.
	const N = probeFeedBufferSize * 4
	doneCh := make(chan struct{})
	go func() {
		for i := 0; i < N; i++ {
			p.Publish(makeProbeReq(t, "eth_getBalance"))
		}
		close(doneCh)
	}()

	select {
	case <-doneCh:
		// Good — Publish never blocked despite the dispatcher being jammed.
	case <-time.After(2 * time.Second):
		t.Fatalf("Publish blocked on full bus — request path would have stalled")
	}
}

// TestProber_UpdateConfigHotSwap — calling UpdateConfig with new
// values takes effect on subsequent publishes without restarting
// the prober.
func TestProber_UpdateConfigHotSwap(t *testing.T) {
	deps := &stubEngine{}
	dead := &stubProbeUpstream{id: "dead"}
	deps.setExcluded("evm:1", []common.Upstream{dead})

	p, _ := newTestProber(t, deps, &ProbeConfig{
		SampleRate:    0.0, // start disabled (everything sampled out)
		MaxConcurrent: 4,
		Timeout:       5 * time.Second,
	})
	defer p.Stop()

	// At sampleRate=0, every publish is sampled out — no calls.
	for i := 0; i < 10; i++ {
		p.Publish(makeProbeReq(t, "eth_getBalance"))
	}
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, int64(0), dead.calls.Load(),
		"sampleRate=0 must drop every probe")

	// Hot-swap config to sampleRate=1; subsequent publishes mirror.
	p.UpdateConfig(&ProbeConfig{
		SampleRate:    1.0,
		MaxConcurrent: 4,
		Timeout:       5 * time.Second,
	})
	p.Publish(makeProbeReq(t, "eth_getBalance"))
	got := waitForCalls(dead, 1, 1*time.Second)
	assert.Equal(t, int64(1), got,
		"after hot-swap to sampleRate=1, publishes must mirror")
}
