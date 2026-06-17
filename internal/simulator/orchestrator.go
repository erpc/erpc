package simulator

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/erpc"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/rs/zerolog"
)

// buildRPCBody assembles the JSON-RPC request payload eRPC will parse.
// `paramsRaw` should be a well-formed JSON array (possibly empty / null).
func buildRPCBody(method string, paramsRaw json.RawMessage) []byte {
	if len(paramsRaw) == 0 || string(paramsRaw) == "null" {
		paramsRaw = json.RawMessage(`[]`)
	}
	// Random per-request ID. Using nanos is plenty: the simulator's
	// per-session ids are uint64 elsewhere; we just need uniqueness
	// across concurrent requests in this process so eRPC's multiplexer
	// hashes by method+params and not by id.
	id := time.Now().UnixNano() & 0x7fffffff
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, string(paramsRaw)))
}

func policyDefault() string { return policy.DefaultPolicySource() }

// Orchestrator is the simulator's runtime. It owns:
//
//   - The real *erpc.ERPC instance + the *Network pointer the WebSocket
//     handler forwards browser-issued requests through.
//
//   - The UpstreamHub serving the synthetic upstreams.
//
//   - The rolling stats / counters that feed periodic WebSocket frames.
//
// Traffic generation lives in the BROWSER: the browser ticks at its
// chosen rate, samples a method per request, and sends a `send-batch`
// frame to the backend. The backend's Execute method is what each item
// in the batch turns into.
//
// Concurrency model: every external surface (Execute, Stats, UpdateKnob,
// ApplyPolicy, scenarios) is independently safe to call from any number
// of goroutines.
type Orchestrator struct {
	logger zerolog.Logger

	// dumper is an optional JSONL recorder for forensic analysis.
	// nil when `-dump-file` is not set; all Log* calls on nil are
	// safe no-ops. See dump.go for the schema.
	dumper *Dumper

	hub *UpstreamHub

	cfgMu       sync.Mutex
	currentCfg  *common.Config
	// currentYAML is the last-applied YAML source, with the policy body
	// fully inlined (no `{SELECTION_POLICY_FUNC}` placeholder). The
	// backend treats this as opaque text — substitution of the
	// placeholder is the FRONTEND's job (it happens before sending
	// apply-config, and the frontend templatizes incoming YAML for
	// display in the editor).
	currentYAML string

	erpc    atomic.Pointer[erpc.ERPC]
	network atomic.Pointer[erpc.Network]

	paused atomic.Bool

	// Rolling stats / counters.
	counters Counters
	statsMu  sync.Mutex
	// rolling per-second bucket
	curBucket    bucketTotals
	lastBucket   bucketTotals
	lastBucketAt time.Time
	// per-upstream rolling history
	upstreamStats map[string]*upstreamRollingStats

	// Scenario.
	scnMu     sync.Mutex
	scnName   string
	scnStart  time.Time
	scnIdx    int
	scnEvents []ScenarioEventBubble

	stopCh chan struct{}
	stopWg sync.WaitGroup
}

// bucketTotals are the rolling 1-second counters reset each whole second.
type bucketTotals struct {
	total       int
	success     int
	failure     int
	cacheHit    int
	retryOk     int // requests that succeeded via a retry (>=1 retry, no hedge)
	hedgeWin    int // requests that succeeded via a hedge winner
	miss        int // requests whose first attempt was empty/missing-data
	perUpstream map[string]int
}

// upstreamRollingStats keeps the recent latency samples + per-second
// selection counts for one upstream. Used to derive p50/p90/p95 and
// power the sparkline.
type upstreamRollingStats struct {
	mu   sync.Mutex
	lats []float64 // last ~300 samples
	succ int
	fail int
}

// Options configures the orchestrator at boot.
type Options struct {
	Logger          zerolog.Logger
	SeedYAML        string // initial config in YAML form. The orchestrator
	                       // parses + validates + boots eRPC from it.
	UpstreamHubBind string // typically "127.0.0.1:0"
	// Dumper, if non-nil, receives a JSONL record of every observable
	// event (boot, knob change, request/response, …). See dump.go.
	Dumper *Dumper
}

// DefaultPolicySource is re-exported here for convenience so callers
// don't have to import internal/policy directly.
func DefaultPolicySource() string {
	return policyDefault()
}

// New constructs an Orchestrator with the given options. Call Start to
// parse the seed YAML, boot eRPC, and kick off the stats loops.
func New(opts Options) (*Orchestrator, error) {
	hub, err := NewUpstreamHub(opts.UpstreamHubBind)
	if err != nil {
		return nil, err
	}
	if opts.SeedYAML == "" {
		// Boot from the placeholder-expanded seed (built in init()) so
		// the very first parse sees a fully-formed YAML.
		opts.SeedYAML = SeedYAMLExpanded
	}
	// Forgive callers that hand us the templated form by mistake — if
	// the placeholder is still present, expand it with the default
	// policy on the fly. Without this, eRPC's policy engine would
	// later try to evaluate `{SELECTION_POLICY_FUNC}` as JS and crash
	// with `ReferenceError: SELECTION_POLICY_FUNC is not defined`.
	opts.SeedYAML = expandPolicyPlaceholder(opts.SeedYAML, policyDefault())
	o := &Orchestrator{
		logger:         opts.Logger,
		dumper:         opts.Dumper,
		hub:            hub,
		currentYAML:    opts.SeedYAML,
		stopCh:         make(chan struct{}),
		upstreamStats:  make(map[string]*upstreamRollingStats),
		curBucket:      bucketTotals{perUpstream: make(map[string]int)},
		lastBucket:     bucketTotals{perUpstream: make(map[string]int)},
	}
	return o, nil
}

// Start boots the real eRPC instance against the synthetic upstreams
// and kicks off the stats + scenario loops.
func (o *Orchestrator) Start(ctx context.Context) error {
	o.stopWg.Add(1)
	go func() {
		defer o.stopWg.Done()
		if err := o.hub.Serve(ctx); err != nil {
			o.logger.Error().Err(err).Msg("upstream hub serve failed")
		}
	}()

	if err := o.bootFromYAML(ctx, o.currentYAML); err != nil {
		return err
	}

	o.lastBucketAt = time.Now()
	o.counters.StartedAtNs.Store(time.Now().UnixNano())

	// Capture initial state for any attached dumper. Done AFTER the
	// first bootFromYAML so the parsed config + initial knobs are
	// populated.
	if o.dumper != nil {
		o.dumper.LogBoot(o.currentYAML, o.CurrentPolicy(), o.hub.Snapshot(), map[string]any{
			"hubAddr": o.hub.Addr(),
		})
		o.cfgMu.Lock()
		cfg := o.currentCfg
		o.cfgMu.Unlock()
		o.dumper.SnapshotConfig(o.currentYAML, o.CurrentPolicy(), cfg)
	}

	o.stopWg.Add(2)
	go o.bucketLoop(ctx)
	go o.scenarioLoop(ctx)
	return nil
}

// Stop tears down the orchestrator. Idempotent.
func (o *Orchestrator) Stop() {
	select {
	case <-o.stopCh:
	default:
		close(o.stopCh)
	}
	o.stopWg.Wait()
	if o.dumper != nil {
		_ = o.dumper.Close()
	}
}

// SetPaused pauses/resumes new request execution. Useful for the
// browser's pause button — incoming send-batch frames are dropped
// silently while paused.
//
// Also gates the policy engine's per-slot ticker. Without this, the
// engine would keep re-evaluating against drifting tracker metrics
// while the operator stares at a frozen UI — the cached verdict
// (Position pills, score chips, primary-switch flashes) would not
// stay coherent with what `Forward` would actually pick the moment
// pause ends. Freezing the engine alongside execution keeps the
// "what's the policy doing right now?" view trustworthy.
func (o *Orchestrator) SetPaused(p bool) {
	o.paused.Store(p)
	if net := o.network.Load(); net != nil {
		net.SetPolicyEnginePaused(p)
	}
	o.dumper.LogPaused(p)
}

// IsPaused reports the current pause state.
func (o *Orchestrator) IsPaused() bool { return o.paused.Load() }

// bootFromYAML parses + validates the YAML, rewrites every upstream's
// endpoint to point at the in-process hub, syncs the per-upstream knob
// map (creating/refreshing entries for newly-named upstreams), and
// finally boots a fresh `*erpc.ERPC` against the resulting config.
//
// `yamlSrc` MUST be a fully-formed YAML — the placeholder substitution
// is the frontend's job before sending. The backend treats this as
// opaque text and just parses it.
//
// Called once at Start and on every `config.apply`. The previous eRPC
// instance is replaced atomically; in-flight requests against the old
// instance continue against its captured pointer until they finish.
func (o *Orchestrator) bootFromYAML(ctx context.Context, yamlSrc string) error {
	cfg, err := DecodeConfigYAML([]byte(yamlSrc))
	if err != nil {
		return err
	}
	idents := RewriteEndpoints(cfg, o.hub.Addr())
	if err := FinalizeConfig(cfg); err != nil {
		return err
	}

	// Sync the synthetic-knob set against what the YAML declares.
	o.syncUpstreamKnobs(idents)

	o.cfgMu.Lock()
	o.currentCfg = cfg
	o.currentYAML = yamlSrc
	o.cfgMu.Unlock()

	e, err := erpc.NewERPC(ctx, &o.logger, nil, nil, nil, cfg)
	if err != nil {
		return fmt.Errorf("simulator: NewERPC: %w", err)
	}
	e.Bootstrap(ctx)
	net, err := e.GetNetwork(ctx, "sim", "evm:1")
	if err != nil {
		return fmt.Errorf("simulator: GetNetwork: %w", err)
	}
	if err := net.Bootstrap(ctx); err != nil {
		return fmt.Errorf("simulator: network Bootstrap: %w", err)
	}
	o.erpc.Store(e)
	o.network.Store(net)
	// The simulator always wants the per-step chain trail on every tick
	// — the policy-history drawer renders it as the primary diagnostic
	// surface. Production callers leave this off; cost is one
	// function-call indirection per stdlib step.
	net.SetPolicyStepLogEnabled(true)
	// Re-apply the current pause state to the freshly-booted policy
	// engine. ApplyConfig / ApplyPolicy can land mid-pause and the new
	// engine starts running by default; without this its ticker would
	// happily churn while the rest of the simulator sits frozen, and
	// the verdict displayed in the UI would drift from what the next
	// (post-pause) request would actually see.
	if o.paused.Load() {
		net.SetPolicyEnginePaused(true)
	}
	return nil
}

// syncUpstreamKnobs ensures that for every upstream ID declared in the
// freshly-loaded config the hub has a matching `UpstreamKnobs` entry.
// New entries get sensible defaults biased by their tags (premium →
// low latency, fallback → degraded). Existing entries are left
// untouched so live operator tunings survive a config reload.
//
// Orphan entries (knobs whose upstream ID was removed from YAML) are
// dropped — both from the hub and from the stats roll.
func (o *Orchestrator) syncUpstreamKnobs(idents []UpstreamIdentity) {
	want := make(map[string]UpstreamIdentity, len(idents))
	for _, id := range idents {
		want[id.ID] = id
	}
	existing := o.hub.Snapshot()
	have := make(map[string]struct{}, len(existing))
	for _, k := range existing {
		have[k.ID] = struct{}{}
	}
	// Add missing.
	for _, id := range idents {
		if _, ok := have[id.ID]; ok {
			// Refresh identity-side fields (vendor / tags) only.
			o.hub.RefreshIdentity(id.ID, id.Vendor, id.Tags)
			continue
		}
		k := defaultKnobsFor(id)
		k.Endpoint = EndpointFor(o.hub.Addr(), id.ID)
		o.hub.AddUpstream(k)
		o.statsMu.Lock()
		o.upstreamStats[id.ID] = &upstreamRollingStats{}
		o.statsMu.Unlock()
	}
	// Drop orphans.
	for _, k := range existing {
		if _, ok := want[k.ID]; ok {
			continue
		}
		o.hub.RemoveUpstream(k.ID)
		o.statsMu.Lock()
		delete(o.upstreamStats, k.ID)
		o.statsMu.Unlock()
	}
}

// ApplyConfig parses + validates the YAML server-side and, on success,
// re-boots eRPC against the new config. On any error (parse / validate /
// boot) the running instance is left untouched.
//
// The frontend is expected to have substituted any `{SELECTION_POLICY_FUNC}`
// placeholder for the actual policy body before sending. The backend
// treats the YAML as opaque text.
func (o *Orchestrator) ApplyConfig(ctx context.Context, yamlSrc string) ConfigApplyResult {
	if err := o.bootFromYAML(ctx, yamlSrc); err != nil {
		o.dumper.LogConfigApply(yamlSrc, false, err.Error())
		return ConfigApplyResult{Ok: false, Error: err.Error()}
	}
	o.dumper.LogConfigApply(yamlSrc, true, "")
	if o.dumper != nil {
		o.cfgMu.Lock()
		cfg := o.currentCfg
		o.cfgMu.Unlock()
		o.dumper.SnapshotConfig(o.currentYAML, o.CurrentPolicy(), cfg)
	}
	return ConfigApplyResult{Ok: true, AppliedAtMs: time.Now().UnixMilli()}
}

// ValidateConfig parses + validates without applying. Side-effect free.
// Same pipeline as the apply path (decode → endpoint-rewrite →
// finalize) so the operator sees the same errors here that they'd see
// if they hit Apply.
func (o *Orchestrator) ValidateConfig(yamlSrc string) ConfigApplyResult {
	cfg, err := DecodeConfigYAML([]byte(yamlSrc))
	if err != nil {
		return ConfigApplyResult{Ok: false, Error: err.Error()}
	}
	RewriteEndpoints(cfg, o.hub.Addr())
	if err := FinalizeConfig(cfg); err != nil {
		return ConfigApplyResult{Ok: false, Error: err.Error()}
	}
	return ConfigApplyResult{Ok: true}
}

// ValidatePolicy compiles the user's selection-policy JS server-side
// (sobek) WITHOUT swapping it into the running engine. Used by the
// editor's debounced-typing lint path.
func (o *Orchestrator) ValidatePolicy(src string) PolicyApplyResult {
	if _, err := common.CompileProgram(src); err != nil {
		return PolicyApplyResult{Ok: false, Error: err.Error()}
	}
	return PolicyApplyResult{Ok: true}
}

// ApplyPolicy compiles + injects the new selection-policy source by
// editing the stored YAML's `selectionPolicy.evalFunc` and re-booting.
// The YAML editor will see the updated evalFunc on the next snapshot
// (the frontend re-templatizes it for display).
func (o *Orchestrator) ApplyPolicy(ctx context.Context, src string) PolicyApplyResult {
	if _, err := common.CompileProgram(src); err != nil {
		o.dumper.LogPolicyApply(src, false, err.Error())
		return PolicyApplyResult{Ok: false, Error: err.Error()}
	}
	o.cfgMu.Lock()
	yamlSrc := o.currentYAML
	o.cfgMu.Unlock()
	newYAML, err := injectPolicyIntoYAML(yamlSrc, src)
	if err != nil {
		o.dumper.LogPolicyApply(src, false, err.Error())
		return PolicyApplyResult{Ok: false, Error: err.Error()}
	}
	if err := o.bootFromYAML(ctx, newYAML); err != nil {
		o.dumper.LogPolicyApply(src, false, err.Error())
		return PolicyApplyResult{Ok: false, Error: err.Error()}
	}
	o.dumper.LogPolicyApply(src, true, "")
	if o.dumper != nil {
		o.cfgMu.Lock()
		cfg := o.currentCfg
		o.cfgMu.Unlock()
		o.dumper.SnapshotConfig(o.currentYAML, src, cfg)
	}
	return PolicyApplyResult{Ok: true, CompiledAtMs: time.Now().UnixMilli()}
}

// CurrentYAML returns the source-of-truth YAML the engine was last
// booted from. Used by the WS layer's snapshot frame.
func (o *Orchestrator) CurrentYAML() string {
	o.cfgMu.Lock()
	defer o.cfgMu.Unlock()
	return o.currentYAML
}

// CurrentPolicy returns the JS source the engine was last compiled
// with — read directly from the parsed eRPC config (the source of
// truth, since the YAML carries the evalFunc inline now).
func (o *Orchestrator) CurrentPolicy() string {
	o.cfgMu.Lock()
	cfg := o.currentCfg
	o.cfgMu.Unlock()
	if cfg == nil {
		return ""
	}
	for _, p := range cfg.Projects {
		for _, n := range p.Networks {
			if n.SelectionPolicy != nil && n.SelectionPolicy.EvalFunc != "" {
				return n.SelectionPolicy.EvalFunc
			}
		}
	}
	return ""
}

// RecentPolicyDecisions returns up to `limit` of the most-recent
// policy engine ticks for the network/method, formatted for the WS
// wire (compact, no input-metric snapshots — those live in
// StatsFrame's per-upstream rows on every tick anyway).
//
// `method = ""` defaults to the wildcard "*" slot — that's what the
// simulator's selectionPolicy editor evaluates against.
func (o *Orchestrator) RecentPolicyDecisions(method string, limit int) []PolicyDecisionFrame {
	net := o.network.Load()
	if net == nil {
		return nil
	}
	if method == "" {
		method = "*"
	}
	decisions := net.RecentPolicyDecisions(method, limit)
	if len(decisions) == 0 {
		return nil
	}
	// Single tick's score map captured once per decision — the engine's
	// GetScores returns the LATEST only, so for historical ticks we
	// don't surface scores (they'd be misleading anyway: each tick
	// produces its own scores and we don't retain them per-decision).
	out := make([]PolicyDecisionFrame, 0, len(decisions))
	for _, d := range decisions {
		if d == nil {
			continue
		}
		frame := PolicyDecisionFrame{
			DecisionID:     d.ID,
			TickMs:         d.TickAt.UnixMilli(),
			EvalDurationUs: d.EvalDuration.Microseconds(),
			NetworkID:      d.NetworkID,
			Method:         d.Method,
			Order:          append([]string(nil), d.Output.Order...),
			PrimaryChanged: d.Diff.PrimaryChanged,
			OrderChanged:   d.Diff.OrderChanged,
			Added:          append([]string(nil), d.Diff.Added...),
			Removed:        append([]string(nil), d.Diff.Removed...),
			Error:          d.Error,
		}
		if len(d.Output.Excluded) > 0 {
			frame.Excluded = make([]PolicyDecisionExcludedRow, 0, len(d.Output.Excluded))
			for _, ex := range d.Output.Excluded {
				frame.Excluded = append(frame.Excluded, PolicyDecisionExcludedRow{
					ID:     ex.ID,
					Step:   ex.Step,
					Reason: ex.Reason,
				})
			}
		}
		// Step trail lands on the wire only when the engine's step-log
		// toggle is enabled — the simulator's orchestrator flips it on
		// at boot so every decision carries it.
		if len(d.Output.StepLog) > 0 {
			frame.Steps = make([]PolicyDecisionStep, 0, len(d.Output.StepLog))
			for _, step := range d.Output.StepLog {
				frame.Steps = append(frame.Steps, PolicyDecisionStep{
					Step:      step.Step,
					Args:      step.Args,
					InIDs:     append([]string(nil), step.InIDs...),
					OutIDs:    append([]string(nil), step.OutIDs...),
					Dropped:   append([]string(nil), step.Dropped...),
					Added:     append([]string(nil), step.Added...),
					Reordered: step.Reordered,
				})
			}
		}
		// Per-tick metric snapshot the eval saw — drives the modal's
		// per-upstream hover tooltip + the sortByScore breakdown. We
		// copy by value (PolicyDecisionMetrics is plain numeric fields)
		// so the wire payload doesn't reference the slot's snapshot map.
		if len(d.Input.Metrics) > 0 {
			frame.Metrics = make(map[string]PolicyDecisionMetrics, len(d.Input.Metrics))
			for id, m := range d.Input.Metrics {
				frame.Metrics[id] = PolicyDecisionMetrics{
					ErrorRate:              m.ErrorRate,
					ThrottledRate:          m.ThrottledRate,
					MisbehaviorRate:        m.MisbehaviorRate,
					BlockHeadLag:           m.BlockHeadLag,
					FinalizationLag:        m.FinalizationLag,
					BlockHeadLagSeconds:    m.BlockHeadLagSeconds,
					FinalizationLagSeconds: m.FinalizationLagSeconds,
					P50ResponseMs:          m.P50ResponseSeconds * 1000,
					P70ResponseMs:          m.P70ResponseSeconds * 1000,
					P90ResponseMs:          m.P90ResponseSeconds * 1000,
					P95ResponseMs:          m.P95ResponseSeconds * 1000,
					P99ResponseMs:          m.P99ResponseSeconds * 1000,
					RequestsTotal:          m.RequestsTotal,
					ErrorsTotal:            m.ErrorsTotal,
				}
			}
		}
		if len(d.Output.Scores) > 0 {
			frame.PerUpstreamScore = make(map[string]float64, len(d.Output.Scores))
			for id, s := range d.Output.Scores {
				frame.PerUpstreamScore[id] = s
			}
		}
		out = append(out, frame)
	}
	return out
}

// UpdateUpstream patches one upstream's knobs and returns the new
// snapshot. The patch is visible to the next inbound request via the
// hub's RLock-on-handle path.
func (o *Orchestrator) UpdateUpstream(id string, patch UpstreamKnobPatch) (UpstreamKnobs, bool) {
	// Capture pre-image for the dump. UpdateKnob does the merge.
	var before UpstreamKnobs
	for _, k := range o.hub.Snapshot() {
		if k.ID == id {
			before = k
			break
		}
	}
	after, ok := o.hub.UpdateKnob(id, patch)
	if ok {
		o.dumper.LogKnobChange(id, before, after, patch)
		// Detect the "inject 60s failure" path — InjectFailureUntilMs
		// is set in patch via the inject button. Surface as its own
		// kind so dumps are easy to filter by inject events.
		if patch.InjectFailureUntilMs != nil {
			durMs := *patch.InjectFailureUntilMs - time.Now().UnixMilli()
			if durMs > 0 {
				o.dumper.LogInjectFailure(id, durMs)
			}
		}
	}
	return after, ok
}

// Hub returns the underlying UpstreamHub. Useful for the WS layer to
// snapshot knobs for the initial state push.
func (o *Orchestrator) Hub() *UpstreamHub { return o.hub }

// Execute fires a single real eRPC request through the network and
// returns a TraceEvent built from the request's execution state.
// Called by the WS handler for each item in a `send-batch` frame.
//
// `paramsRaw` is the JSON-encoded params array as the browser supplied
// it (e.g. `["latest", true]` or `[{"to":"0x..","data":"0x.."}]`).
// Empty/null falls back to no-params so the simplest patterns
// (eth_blockNumber, eth_chainId) work without ceremony.
//
// Returns nil if the orchestrator is paused or eRPC hasn't booted yet.
func (o *Orchestrator) Execute(ctx context.Context, method string, paramsRaw json.RawMessage) *TraceEvent {
	if o.paused.Load() {
		return nil
	}
	net := o.network.Load()
	if net == nil {
		return nil
	}
	if method == "" {
		method = "eth_blockNumber"
	}

	body := buildRPCBody(method, paramsRaw)
	req := common.NewNormalizedRequest(body)
	start := time.Now()

	// Each request is bounded so a hung fake upstream can't pile up
	// goroutines forever. eRPC's own per-attempt timeout (from the
	// failsafe block) usually fires first.
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := net.Forward(rctx, req)
	dur := time.Since(start)

	// Capture the final response the CLIENT saw. For success, this is
	// the JSON-RPC body (truncated to keep WS frames small); for
	// failure, the full error chain (so the drawer can show the
	// consensus dispute / multiplexer error / etc. that wasn't
	// visible from any single attempt).
	var responseBody, requestError string
	if err != nil {
		requestError = err.Error()
	} else if resp != nil {
		responseBody = previewResponseBody(rctx, resp)
	}

	o.recordCounters(req, err)

	outcome := "ok"
	winner := ""
	usedHedge := false
	usedRetry := false
	if err != nil {
		outcome = "fail"
	} else {
		snap := req.ExecState().Snapshot()
		if snap.UpstreamHedges > 0 || snap.NetworkHedges > 0 {
			usedHedge = true
			outcome = "hedge-win"
		} else if snap.UpstreamRetries > 0 || snap.NetworkRetries > 0 {
			usedRetry = true
			outcome = "retry-ok"
		}
	}
	for _, a := range req.ExecState().UpstreamAttemptLog() {
		if a.Won {
			winner = a.UpstreamId
			break
		}
	}

	attempts := req.ExecState().UpstreamAttemptLog()
	tAttempts := make([]TraceAttempt, 0, len(attempts))
	for _, a := range attempts {
		t0 := float64(a.StartedAt.Sub(start).Microseconds()) / 1000.0
		out := string(a.Outcome)
		if a.IsHedge && a.Won {
			out = "hedge-winner"
		}
		tAttempts = append(tAttempts, TraceAttempt{
			ID:              a.UpstreamId,
			T0Ms:            t0,
			DurationMs:      float64(a.Duration.Microseconds()) / 1000.0,
			Outcome:         out,
			Reason:          a.ErrorDetail,
			Winner:          a.Won,
			SelectionReason: string(a.Reason),
			IsHedge:         a.IsHedge,
			IsRetry:         a.IsRetry,
			AttemptIdx:      a.AttemptIdx,
		})
	}

	// Consensus bookkeeping comes from the ExecState snapshot. When
	// the request went through the consensus executor, these counters
	// are non-zero — the drawer surfaces them as meta-pills so the
	// operator can see "this request was a consensus race" at a glance.
	csSnap := req.ExecState().Snapshot()
	ev := &TraceEvent{
		ID:                uint64(time.Now().UnixNano()),
		StartedAt:         start,
		Method:            method,
		Outcome:           outcome,
		Winner:            winner,
		DurationMs:        float64(dur.Microseconds()) / 1000.0,
		UsedHedge:         usedHedge,
		UsedRetry:         usedRetry,
		ConsensusSlots:    csSnap.ConsensusSlots,
		ConsensusDisputes: csSnap.ConsensusDisputes,
		ConsensusLowParts: csSnap.ConsensusLowParticipants,
		RequestError:      requestError,
		ResponseBody:      responseBody,
		Attempts:          tAttempts,
	}
	// Record the FULL request lifecycle (event + raw params + response
	// body bytes) to the dump file if one is attached. The dumper's
	// per-write JSON encode is cheap and the file path absorbs spikes
	// via a 64KB buffer + per-second flush.
	o.dumper.LogRequest(ev, paramsRaw, json.RawMessage(body))
	return ev
}

// previewResponseBody serializes the response's JSON-RPC body (or a
// short preview) for display in the drawer. Capped at 8KB so giant
// getLogs results don't blow up the WS frame budget.
func previewResponseBody(ctx context.Context, resp *common.NormalizedResponse) string {
	if resp == nil {
		return ""
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return ""
	}
	b, err := jrr.MarshalJSON()
	if err != nil {
		return ""
	}
	const maxLen = 8 * 1024
	if len(b) > maxLen {
		// Keep both ends so the operator can see the shape AND
		// whether the response was truncated.
		return string(b[:maxLen/2]) + "\n…[truncated]…\n" + string(b[len(b)-maxLen/2:])
	}
	return string(b)
}

// recordCounters writes per-attempt + aggregate stats from a completed
// request. Pulled out of Execute so the request can also fail before
// any attempts (e.g. all upstreams excluded) and we still bump the
// overall counters.
func (o *Orchestrator) recordCounters(req *common.NormalizedRequest, err error) {
	o.counters.Total.Add(1)
	if err != nil {
		o.counters.Failure.Add(1)
	} else {
		o.counters.Success.Add(1)
	}

	// Walk the snapshot to classify the request into per-second
	// buckets the UI uses to drive the "ops strip" sparklines.
	snap := req.ExecState().Snapshot()
	usedRetry := snap.UpstreamRetries > 0 || snap.NetworkRetries > 0
	usedHedge := snap.UpstreamHedges > 0 || snap.NetworkHedges > 0
	// "first attempt was a miss" — i.e. the policy had to look past
	// the primary because the primary returned empty. Counted only on
	// successful requests; on failed ones the miss-count would
	// dominate and skew the panel.
	firstAttemptMissed := false
	attempts := req.ExecState().UpstreamAttemptLog()
	if len(attempts) > 0 {
		o0 := attempts[0].Outcome
		if o0 == common.UpstreamOutcomeEmpty ||
			o0 == common.UpstreamOutcomeMissingData ||
			o0 == common.UpstreamOutcomeBlockUnavailable {
			firstAttemptMissed = true
		}
	}

	o.statsMu.Lock()
	defer o.statsMu.Unlock()
	o.curBucket.total++
	if err != nil {
		o.curBucket.failure++
	} else {
		o.curBucket.success++
		if usedHedge {
			o.curBucket.hedgeWin++
		} else if usedRetry {
			o.curBucket.retryOk++
		}
		if firstAttemptMissed {
			o.curBucket.miss++
		}
	}
	for _, a := range req.ExecState().UpstreamAttemptLog() {
		urs, ok := o.upstreamStats[a.UpstreamId]
		if !ok {
			urs = &upstreamRollingStats{}
			o.upstreamStats[a.UpstreamId] = urs
		}
		o.curBucket.perUpstream[a.UpstreamId]++
		urs.mu.Lock()
		urs.lats = append(urs.lats, float64(a.Duration.Milliseconds()))
		if len(urs.lats) > 300 {
			urs.lats = urs.lats[len(urs.lats)-300:]
		}
		if a.Outcome == common.UpstreamOutcomeSuccess {
			urs.succ++
		} else if a.Outcome != common.UpstreamOutcomeSkipped {
			urs.fail++
		}
		urs.mu.Unlock()
	}
}

// Stats produces a fresh StatsFrame from the rolling counters.
func (o *Orchestrator) Stats() StatsFrame {
	o.statsMu.Lock()
	last := o.lastBucket
	o.statsMu.Unlock()
	// `actualRps` is the rolling-second rate (lastBucket.total). The
	// since-boot average grows uninteresting over time and obscures
	// the real "what's happening right now" signal the UI wants.
	actualRps := float64(last.total)
	upstreams := o.snapshotUpstreamRows(last)

	// Surface the policy engine's last primary-switch timestamp so the
	// frontend can render `stickyPrimary` cooldown countdowns. Real
	// engine data — no client-side simulation of sticky behavior.
	var lastSwitchMs int64
	if net := o.network.Load(); net != nil {
		if t := net.PolicyLastSwitchAt("*"); !t.IsZero() {
			lastSwitchMs = t.UnixMilli()
		}
	}

	frame := StatsFrame{
		TickMs:            time.Now().UnixMilli(),
		ActualRps:         actualRps,
		LastSecondTotal:   last.total,
		LastSecondSucc:    last.success,
		LastSecondErr:     last.failure,
		LastSecondCache:   last.cacheHit,
		LastSecondRetryOk: last.retryOk,
		LastSecondHedge:   last.hedgeWin,
		LastSecondMiss:    last.miss,
		PerUpstreamLastS:   copyIntMap(last.perUpstream),
		Upstreams:          upstreams,
		PolicyLastSwitchMs: lastSwitchMs,
	}
	if scn := o.scenarioFrame(); scn != nil {
		frame.Scenario = scn
	}
	return frame
}

func copyIntMap(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// snapshotUpstreamRows reads each upstream's CURRENT state directly
// from eRPC's health.Tracker and policy engine — the same objects the
// real request path consumes on every Forward(). The simulator does
// NOT maintain a parallel health tally; the UI is a pure mirror of
// what the engine sees.
//
// Position semantics:
//   * 0     = primary
//   * 1..N  = fallback order
//   * -1    = upstream is in the config but the selection policy
//             excluded it this tick (failed an excludeIf rule, or it
//             carries a tag the chain steered away from).
//
// Position changes only when the policy re-evaluates (every
// `evalInterval`, default 15s). The ordering stays stable when the
// simulator is paused — no traffic doesn't trigger a re-rank.
//
// All health metrics (errorRate, throttledRate, misbehaviorRate, p50,
// p90, p95, blockHeadLag, finalizationLag, cordoned) come from
// `metricsTracker.GetUpstreamMethodMetrics(up, "*", common.DataFinalityStateAll)` — eRPC's
// any-method aggregate for that upstream. The "*" aggregate gets
// incremented alongside per-method counters (see tracker.getUpsKeys),
// so it reflects the upstream's overall observed quality.
//
// PenaltyScore comes straight from Engine.GetScores — the JS
// `sortByScore` step is the single source of truth for ranking.
func (o *Orchestrator) snapshotUpstreamRows(last bucketTotals) map[string]UpstreamStatsRow {
	rows := make(map[string]UpstreamStatsRow)

	// Pull the live policy ordering AND the tracker handle from the
	// running network. If either is missing (engine hasn't ticked,
	// or simulator hasn't fully booted), the per-row fields fall back
	// to zero values gracefully.
	var ordered []string
	var tracker *health.Tracker
	var scores map[string]float64
	var lastSwitchAt time.Time
	if net := o.network.Load(); net != nil {
		ordered = net.PolicyOrderedUpstreams("*")
		tracker = net.MetricsTracker()
		scores = net.PolicyScores("*")
		lastSwitchAt = net.PolicyLastSwitchAt("*")
	}
	_ = lastSwitchAt // surfaced via Stats() below; reserved here for future per-row use
	policyPos := make(map[string]int, len(ordered))
	for i, id := range ordered {
		policyPos[id] = i
	}

	// Resolve *common.Upstream handles so we can ask the tracker for
	// each one's metrics. The tracker keys by (upstream, method) and
	// we want the "*" aggregate for the row view.
	upstreamsByID := o.resolveUpstreamHandles()

	for _, k := range o.hub.Snapshot() {
		row := UpstreamStatsRow{}

		// Tracker-side metrics (the source of truth the policy uses).
		if tracker != nil {
			if up := upstreamsByID[k.ID]; up != nil {
				m := tracker.GetUpstreamMethodMetrics(up, "*", common.DataFinalityStateAll)
				if m != nil {
					row.ErrorRate = m.ErrorRate()
					row.ThrottleRate = m.ThrottledRate()
					row.MisbehaviorRate = m.MisbehaviorRate()
					row.BlockHeadLag = int(m.BlockHeadLag.Load())
					row.FinalizationLag = int(m.FinalizationLag.Load())
					// Wall-clock lag — same conversion the policy engine
					// uses internally, so the tooltip's "Xs behind tip"
					// matches what `blockSecondsLagAbove(...)` rules see.
					if bt := tracker.GetNetworkBlockTime(up.NetworkId()); bt > 0 {
						btSec := bt.Seconds()
						row.BlockHeadLagSeconds = float64(row.BlockHeadLag) * btSec
						row.FinalizationLagSeconds = float64(row.FinalizationLag) * btSec
					}
					row.RequestsTotal = m.RequestsTotal.Load()
					row.ErrorsTotal = m.ErrorsTotal.Load()
					if m.Cordoned.Load() {
						row.Cordoned = true
						if r, ok := m.LastCordonedReason.Load().(string); ok {
							row.CordonedReason = r
						}
					}
					if m.ResponseQuantiles != nil {
						row.P50Ms = m.ResponseQuantiles.GetQuantile(0.50).Seconds() * 1000
						row.P70Ms = m.ResponseQuantiles.GetQuantile(0.70).Seconds() * 1000
						row.P90Ms = m.ResponseQuantiles.GetQuantile(0.90).Seconds() * 1000
						row.P95Ms = m.ResponseQuantiles.GetQuantile(0.95).Seconds() * 1000
					}
				}
			}
		}

		// Score is the value the policy engine's last `sortByScore`
		// step produced for this upstream, pulled from the slot via
		// Engine.GetScores. The JS is the single source of truth for
		// ranking — probe-included entries have no score, sticky
		// reorderings preserve scores. Defaults to 0 when the policy
		// has no scoring step.
		if s, ok := scores[k.ID]; ok {
			row.PenaltyScore = s
		}

		row.SelectionsLast = last.perUpstream[k.ID]
		if pos, ok := policyPos[k.ID]; ok {
			row.Position = pos
		} else if len(ordered) > 0 {
			// Engine has SOME ordering but this upstream isn't in it
			// → the policy excluded it. Reflects reality.
			row.Position = -1
		} else {
			// Engine hasn't ticked yet. Use alphabetical so the UI
			// at least stays consistent across renders.
			row.Position = 0
		}
		rows[k.ID] = row
	}
	return rows
}

// resolveUpstreamHandles returns a map of upstream ID → *common.Upstream
// from the current eRPC network. Used to look up per-upstream tracker
// metrics. Returns an empty map if the orchestrator hasn't booted yet.
func (o *Orchestrator) resolveUpstreamHandles() map[string]common.Upstream {
	out := make(map[string]common.Upstream)
	net := o.network.Load()
	if net == nil {
		return out
	}
	for _, up := range net.AllUpstreams() {
		out[up.Id()] = up
	}
	return out
}

// (computeBalancedScore was retired in favor of reading the JS-computed
// score directly from the policy engine via Network.PolicyScores.
// Replicating the PREFER_FASTEST formula in Go caused drift: e.g. the JS
// uses p70 latency by default while a hand-coded Go copy started with
// p90, producing scores that didn't match the engine's actual ranking.
// The engine is now the single source of truth for upstream scoring.)

// bucketLoop rolls the current 1-second bucket every second.
func (o *Orchestrator) bucketLoop(ctx context.Context) {
	defer o.stopWg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.stopCh:
			return
		case now := <-t.C:
			o.statsMu.Lock()
			o.lastBucket = o.curBucket
			o.curBucket = bucketTotals{perUpstream: make(map[string]int)}
			o.lastBucketAt = now
			o.statsMu.Unlock()
		}
	}
}

// scenarioLoop ticks the scenario state-machine every 250ms.
func (o *Orchestrator) scenarioLoop(ctx context.Context) {
	defer o.stopWg.Done()
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.stopCh:
			return
		case <-t.C:
			o.advanceScenario()
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
