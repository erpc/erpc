package policy

import (
	"context"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// ProbeConfig holds the JS-side `probeExcluded(...)` options drained
// from the eval each tick. Nil = `probeExcluded` is not in the chain,
// so the prober for this network stays asleep.
type ProbeConfig struct {
	// SampleRate is the per-(request, excluded-upstream) probability of
	// mirroring (0.0–1.0). Default 1.0 — mirror every published request
	// up to the concurrency cap.
	SampleRate float64
	// MaxConcurrent is the in-flight probe cap PER excluded upstream.
	// Default 4 — this is the primary throttle.
	MaxConcurrent int
	// Timeout is the per-probe deadline. Probes that overrun are
	// cancelled and counted as failures in the tracker (so a hung
	// upstream registers as bad). Default 10s.
	Timeout time.Duration
}

// proberDeps is the narrow surface a Prober needs from the engine — kept
// small so tests can wire fakes without standing up a full Engine.
type proberDeps interface {
	GetExcluded(networkID, method, finality string) []common.Upstream
}

// Prober mirrors a sampled stream of real requests against currently-
// excluded upstreams. The mirrored calls feed the SAME health-tracker
// counters that drive the policy's `excludeIf` predicates, so an
// upstream that "heals" while excluded falls out of the excluded set
// naturally on the next tick — no separate re-admission timer.
//
// One Prober exists per (engine, networkID). It's lazy-created when
// the network's eval first emits a non-nil ProbeConfig, and torn
// down when the eval stops emitting one (operator removed
// `probeExcluded` from the chain).
type Prober struct {
	networkID string
	logger    *zerolog.Logger
	engine    proberDeps
	tracker   *health.Tracker

	// config is hot-swappable: each tick of the eval that calls
	// probeExcluded(...) atomically replaces it.
	config atomic.Pointer[ProbeConfig]

	// inflight counts in-flight probes per upstream-ID, enforced
	// against ProbeConfig.MaxConcurrent.
	inflightMu sync.Mutex
	inflight   map[string]*atomic.Int64

	feed   chan *common.NormalizedRequest
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// stopped is set when Stop() is called, so a late Publish race
	// against shutdown drops cleanly instead of panicking on a closed
	// channel.
	stopped atomic.Bool
}

// probeFeedBufferSize bounds the per-network request feed. When full,
// Publish drops with a metric — request path NEVER blocks on the bus.
const probeFeedBufferSize = 256

// newProber spins up a fresh per-network prober with the given initial
// config. Starts the background dispatch goroutine immediately. The
// prober's lifetime is bounded by `parentCtx` — when the engine's
// appCtx is cancelled (or the test parent ctx is cancelled), the
// prober tears down without an explicit Stop() call, so test-side
// hygiene failures don't leak goroutines.
func newProber(
	parentCtx context.Context,
	networkID string,
	logger *zerolog.Logger,
	engine proberDeps,
	tracker *health.Tracker,
	cfg *ProbeConfig,
) *Prober {
	p := &Prober{
		networkID: networkID,
		logger:    logger,
		engine:    engine,
		tracker:   tracker,
		inflight:  make(map[string]*atomic.Int64),
		feed:      make(chan *common.NormalizedRequest, probeFeedBufferSize),
	}
	p.config.Store(cfg)
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(parentCtx)
	p.cancel = cancel
	p.wg.Add(1)
	go p.run(ctx)
	return p
}

// UpdateConfig swaps the active probe config atomically. Called by the
// engine each tick that the eval emits a __probeConfig.
func (p *Prober) UpdateConfig(cfg *ProbeConfig) {
	p.config.Store(cfg)
}

// Config returns the currently-active probe config (read-only snapshot).
// Returns nil if no config has been registered yet.
func (p *Prober) Config() *ProbeConfig {
	return p.config.Load()
}

// Publish offers a request to the probe bus. Non-blocking: when the
// feed channel is full the request is dropped and a metric increments.
// Called from the request path — must NEVER block real traffic.
func (p *Prober) Publish(req *common.NormalizedRequest) {
	if p.stopped.Load() {
		return
	}
	select {
	case p.feed <- req:
		// queued
	default:
		telemetry.MetricSelectionProbeDropped.WithLabelValues(p.networkID, "bus_full").Inc()
	}
}

// Stop terminates the background dispatcher and waits for in-flight
// mirror goroutines to drain. Idempotent.
func (p *Prober) Stop() {
	if !p.stopped.CompareAndSwap(false, true) {
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

func (p *Prober) run(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-p.feed:
			if !ok {
				return
			}
			p.onRequest(ctx, req)
		}
	}
}

func (p *Prober) onRequest(ctx context.Context, req *common.NormalizedRequest) {
	cfg := p.config.Load()
	if cfg == nil {
		return
	}

	method, methErr := req.Method()
	if methErr != nil || method == "" {
		telemetry.MetricSelectionProbeSkipped.WithLabelValues(p.networkID, "no_method").Inc()
		return
	}

	// Write-method gate — never mirror state-mutating calls. eRPC has
	// the methods directives system but no central "is mutating"
	// helper today; we hard-code the well-known EVM write set for v1.
	if isProbeUnsafeMethod(method) {
		telemetry.MetricSelectionProbeSkipped.WithLabelValues(p.networkID, "write_method").Inc()
		return
	}

	finality := req.Finality(ctx).String()
	excluded := p.engine.GetExcluded(p.networkID, method, finality)
	if len(excluded) == 0 {
		return
	}

	for _, u := range excluded {
		if !p.shouldProbe(u, cfg) {
			continue
		}
		p.wg.Add(1)
		go p.mirror(req, u, cfg)
	}
}

func (p *Prober) shouldProbe(u common.Upstream, cfg *ProbeConfig) bool {
	if u == nil {
		return false
	}

	// Per-upstream opt-out via routing.probe: off.
	if cu := u.Config(); cu != nil && cu.Routing != nil && cu.Routing.Probe == common.ProbeModeOff {
		telemetry.MetricSelectionProbeSkipped.WithLabelValues(p.networkID, "opt_out").Inc()
		return false
	}

	// Probabilistic sampling.
	if cfg.SampleRate < 1.0 && rand.Float64() > cfg.SampleRate {
		telemetry.MetricSelectionProbeSkipped.WithLabelValues(p.networkID, "sampled_out").Inc()
		return false
	}

	// Per-upstream concurrency cap.
	counter := p.getInflight(u.Id())
	max := int64(cfg.MaxConcurrent)
	if max <= 0 {
		max = 4
	}
	if counter.Load() >= max {
		telemetry.MetricSelectionProbeSkipped.WithLabelValues(p.networkID, "max_concurrent").Inc()
		return false
	}

	return true
}

func (p *Prober) getInflight(id string) *atomic.Int64 {
	p.inflightMu.Lock()
	defer p.inflightMu.Unlock()
	if c, ok := p.inflight[id]; ok {
		return c
	}
	c := new(atomic.Int64)
	p.inflight[id] = c
	return c
}

// mirror fires the shadow probe against one excluded upstream. Runs on
// its own goroutine, fully detached from the user's request context.
// Records the result into the tracker indistinguishably from a real
// request — that's the WHOLE POINT: the policy's excludeIf chain sees
// fresh samples and naturally re-admits when they pass.
func (p *Prober) mirror(req *common.NormalizedRequest, u common.Upstream, cfg *ProbeConfig) {
	defer p.wg.Done()

	counter := p.getInflight(u.Id())
	counter.Add(1)
	defer counter.Add(-1)

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	method, _ := req.Method()
	finality := req.Finality(ctx)

	start := time.Now()
	telemetry.MetricSelectionProbeRequests.WithLabelValues(p.networkID, u.Id(), method).Inc()
	p.tracker.RecordUpstreamRequest(u, method, finality)

	// byPassMethodExclusion=true: an excluded upstream that returned
	// method-not-supported errors might still be tagged as
	// non-supporting; we want probe traffic to reach the upstream so
	// it can prove (or disprove) itself. isHedgeAttempt=false — probes
	// are not hedge fan-outs.
	_, err := u.Forward(ctx, req, true, false)
	duration := time.Since(start)

	isSuccess := err == nil
	// "probe" composite-type label keeps probe samples grouped under
	// the same tracker counters but distinguishable in latency
	// histograms — same path real traffic uses.
	p.tracker.RecordUpstreamDuration(u, method, duration, isSuccess, "probe", finality, "")
	if err != nil {
		p.tracker.RecordUpstreamFailure(u, method, finality, err)
		telemetry.MetricSelectionProbeErrors.WithLabelValues(p.networkID, u.Id(), method, classifyProbeErr(err)).Inc()
	}
}

// isProbeUnsafeMethod returns true for any method whose execution may
// mutate chain state. Mirroring these to an excluded upstream would
// risk double-broadcast / double-charge / double-signing. We err on
// the side of caution and skip any method with a known write
// signature.
func isProbeUnsafeMethod(method string) bool {
	if method == "" {
		return true // unknown method → skip
	}
	// Lowercase prefix check — covers eth_sendRawTransaction,
	// eth_sendTransaction, eth_sign*, personal_sign*,
	// eth_signTypedData_v3/v4, etc. Also catches any future write
	// method that follows the same naming convention.
	lower := strings.ToLower(method)
	if strings.HasPrefix(lower, "eth_send") {
		return true
	}
	if strings.HasPrefix(lower, "eth_sign") {
		return true
	}
	if strings.HasPrefix(lower, "personal_sign") {
		return true
	}
	if strings.HasPrefix(lower, "personal_sendtransaction") {
		return true
	}
	return false
}

func classifyProbeErr(err error) string {
	if err == nil {
		return "ok"
	}
	if common.HasErrorCode(err, common.ErrCodeUpstreamRequestSkipped) {
		return "skipped"
	}
	if common.HasErrorCode(err, common.ErrCodeEndpointRequestTimeout) {
		return "timeout"
	}
	if common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized) {
		return "auth"
	}
	if common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded) {
		return "throttled"
	}
	return "error"
}
