package policy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// Slot is the per-(network, method, finality) state container. Each
// Slot owns a goroutine driven by its ticker; the request-path never
// touches anything here except `cache` via an atomic load. `method` and
// `finality` are `"*"` when the corresponding `EvalPerMethod` /
// `EvalPerFinality` knob is off — only the wildcard slot exists then.
type Slot struct {
	engine    *Engine
	networkID string
	// networkLabel is the human-friendly alias used as the Prometheus
	// `network` label (so `mainnet`/`base` instead of `evm:1`/`evm:8453`).
	// Matches the convention used by every other erpc_* metric (filed via
	// Network.Label()). Falls back to networkID if the caller didn't pass
	// one through RegisterNetwork.
	networkLabel string
	method       string
	finality     string

	upstreamsFn func() []common.Upstream
	cfg         *common.SelectionPolicyConfig

	// cache holds the most recent eval output. nil before the first tick.
	cache atomic.Pointer[[]common.Upstream]

	// lastAccessedAtMs is unix-millis of the last tick OR GetOrdered
	// touching this slot. Drives the engine's idle-slot eviction —
	// per-(method, finality) slots that haven't seen traffic in a
	// while get stopped and removed so a method-flood attacker can't
	// grow the engine's slot map without bound. The wildcard
	// (`("*", "*")`) slot is exempt; only narrow lazily-created slots
	// are sweepable.
	lastAccessedAtMs atomic.Int64

	// crossTick state — touched only inside tickOnce, no locking.
	mu               sync.Mutex // protects fields below from admin reads
	tickCount        uint64
	previousOrder    []string
	previousExcluded []string
	lastSwitchAt     *time.Time
	excludedSince    map[string]int64
	// lastScores holds the per-upstream `score` values the JS produced on
	// the most recent successful tick (via `sortByScore(...)` setting
	// `u.score = overall / (1 + penalty)`, higher = better). Entries are
	// missing for upstreams
	// added after the scoring step (probeExcluded / forceInclude). Read
	// by Engine.GetScores for diagnostics — single source of truth for
	// "what does the policy rank this upstream at?".
	lastScores map[string]float64
	// decisions is a small ring buffer of the most-recent Decisions
	// produced by tickOnce. Capped at decisionsRingSize so an idle slot
	// can't accumulate unbounded memory. Read by Engine.RecentDecisions
	// for diagnostic tooling that wants a tick-by-tick replay (the
	// erpc-simulator's "policy history" panel, primarily).
	decisions      [decisionsRingSize]*Decision
	decisionsHead  int // next write index
	decisionsCount int // valid entries (clamped to ring size)
	// lastEvalAt is the wall-clock timestamp of the most recent
	// successful tick. Updated under mu at the end of tickOnce.
	lastEvalAt time.Time

	// Bad-eval streak counter; spec §5.7 falls back to the default policy
	// after 3 consecutive failures. (Hookup deferred to Phase 5.15.)
	consecutiveFails int

	// Test-only overrides applied inside tickOnce after buildEvalContext.
	// Zero values mean "no override". Guarded by `mu` along with the
	// cross-tick state. SetFinalityForTest / AdvanceEvalNowForTest in
	// testing.go are the only call sites.
	testFinality  string
	testNowOffset int64 // added to ctx.Now (milliseconds)

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func newSlot(e *Engine, networkID, networkLabel, method, finality string, upstreamsFn func() []common.Upstream, cfg *common.SelectionPolicyConfig) *Slot {
	if networkLabel == "" {
		networkLabel = networkID
	}
	s := &Slot{
		engine:        e,
		networkID:     networkID,
		networkLabel:  networkLabel,
		method:        method,
		finality:      finality,
		upstreamsFn:   upstreamsFn,
		cfg:           cfg,
		excludedSince: make(map[string]int64),
		stopCh:        make(chan struct{}),
	}
	// Seed lastAccessedAtMs to "now" so a freshly-created slot doesn't
	// look idle to the very next sweep tick.
	s.lastAccessedAtMs.Store(time.Now().UnixMilli())
	return s
}

// start spawns the ticker goroutine. If evalInterval is zero or negative
// (or DisableTickerForTest is set) the slot is "frozen" — only manual
// TickForTest or admin reeval will fire.
func (s *Slot) start(ctx context.Context) {
	if s.cfg.DisableTickerForTest {
		return
	}
	interval := s.cfg.EvalInterval.Duration()
	if interval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stopCh:
				return
			case <-ticker.C:
				// Engine-level pause gate: simulator's pause button (and
				// any future "freeze the policy verdict" caller) flips
				// this so the cache stays at the last verdict for the
				// duration of the pause. Skipping the call is enough —
				// nothing else in the slot's tick path runs.
				if s.engine.paused.Load() {
					continue
				}
				s.tickOnce()
			}
		}
	}()
}

func (s *Slot) stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

// decisionsRingSize bounds the slot's in-memory decision history.
// 64 ticks = ~64 s at the default 1 s evalInterval — enough for
// the simulator's "what just happened?" panel; not enough to be
// confused with the canonical metrics/traces story.
const decisionsRingSize = 64

// tickOnce runs one eval cycle synchronously. Exported via TickForTest.
func (s *Slot) tickOnce() {
	start := time.Now()
	// Mark the slot active so the engine's idle-sweep keeps it alive
	// even if no request has hit GetOrdered between ticks. Ticking IS
	// activity — the JS evaluator burned CPU on this slot, evicting
	// would be wasteful.
	s.lastAccessedAtMs.Store(start.UnixMilli())
	timeout := s.cfg.EvalTimeout.Duration()

	// Re-resolve upstreams each tick so newly-bootstrapped ones become visible.
	ups := s.upstreamsFn()

	// 1. Snapshot metrics for every upstream.
	metrics, metricsAcrossMethods := snapshotMetrics(s.engine.tracker, ups, s.method, parseFinality(s.finality))

	// 2. Build EvalContext from cross-tick state.
	s.mu.Lock()
	state := DecisionState{
		PreviousOrder:    cloneStrings(s.previousOrder),
		PreviousExcluded: cloneStrings(s.previousExcluded),
		LastSwitchAt:     copyTimePtr(s.lastSwitchAt),
		ExcludedSince:    cloneInt64Map(s.excludedSince),
		TickCount:        s.tickCount,
	}
	evalCtx := buildEvalContext(s.networkID, s.method, state)
	// For per-finality slots `s.finality` carries the bucket value
	// (`realtime`/`unfinalized`/`finalized`/`unknown`); for wildcard
	// slots it stays `"*"` and we fall back to the legacy default
	// (`unknown`) so existing `byFinality({...})` evals don't see a
	// surprising literal `"*"`.
	if s.finality != "" && s.finality != "*" {
		evalCtx.Finality = s.finality
	}
	// Apply test-only overrides (no-ops in production where these stay zero).
	if s.testFinality != "" {
		evalCtx.Finality = s.testFinality
	}
	if s.testNowOffset != 0 {
		evalCtx.Now += s.testNowOffset
	}
	s.mu.Unlock()

	// 3. Run the eval (with optional timeout). Scores are the per-upstream
	// `score` values the JS attached during `sortByScore(...)` — we cache
	// them on the slot so diagnostic tooling (admin endpoint, simulator)
	// can read the engine's authoritative ranking without re-implementing
	// the PREFER_FASTEST formula in Go. The chronological step log is
	// captured only when the engine's debug flag is on (simulator always,
	// eRPC when the logger is at DEBUG) — see `Engine.SetStepLogEnabled`.
	var (
		evalRes *EvalResult
		evalErr error
	)
	stepLogEnabled := s.engine.stepLogEnabled.Load()
	done := make(chan struct{})
	go func() {
		defer close(done)
		evalRes, evalErr = runEval(s.engine.pool, s.cfg, ups, metrics, metricsAcrossMethods, evalCtx, stepLogEnabled, s.engine.sticky)
	}()

	if timeout > 0 {
		select {
		case <-done:
		case <-time.After(timeout):
			evalErr = fmt.Errorf("%w after %s", ErrEvalTimeout, timeout)
			<-done // let the goroutine finish — sobek doesn't support interrupt mid-call cleanly
		}
	} else {
		<-done
	}

	decision := &Decision{
		ID:           fmt.Sprintf("%s/%s/%d", s.networkID, s.method, start.UnixMilli()),
		NetworkID:    s.networkID,
		Method:       s.method,
		TickAt:       start,
		EvalDuration: time.Since(start),
		Input: DecisionInput{
			UpstreamIDs: upstreamIDs(ups),
			Metrics:     metrics,
		},
		State: state,
	}

	if evalErr != nil {
		decision.Error = evalErr.Error()
		s.consecutiveFails++
		s.engine.logger.Warn().
			Str("network", s.networkID).
			Str("method", s.method).
			Str("tick_id", decision.ID).
			Err(evalErr).
			Msg("selection policy eval failed; retaining previous cache")
		s.mu.Lock()
		s.appendDecisionLocked(decision)
		s.mu.Unlock()
		s.emitMetrics(decision, state)
		return
	}
	s.consecutiveFails = 0
	orderedIDs := evalRes.OrderedIDs
	scores := evalRes.Scores

	// 4. Validate + materialize the ordered upstream slice. The JS side
	// folds two signals into one `__policyLeafReasons[id]` array:
	//   * `"@step:NAME"` sentinel entries — the first stdlib primitive
	//     that dropped the upstream (`removeCordoned`, `excludeIf`,
	//     `take`, `byTag`, ...). Drives
	//     `selection_rejection_total{step}` after the sentinel is split
	//     out into `ExcludedUpstream.Step`. Bounded cardinality.
	//   * leaf-reason slugs — `excludeIf`'s option-(c) attribution
	//     (`error_rate_above`, `latency_p95_above`, ...). Drives
	//     `selection_exclusion_total{reason}` after the sentinel is
	//     filtered out of `LeafReasons`.
	//
	// `Reason` ← first leaf slug if any, else the step name, else the
	// default "not in eval result" set by materializeOrder. Diagnostic
	// surface for `RecentDecisions` / simulator UI — metrics never read it.
	ordered, excluded := materializeOrder(ups, orderedIDs)
	for i := range excluded {
		entries, ok := evalRes.LeafReasons[excluded[i].ID]
		if !ok || len(entries) == 0 {
			continue
		}
		excluded[i].Step, excluded[i].LeafReasons = splitStepFromLeafReasons(entries)
	}
	enrichExcluded(excluded)
	decision.Output = DecisionOutput{
		Order:         orderedIDs,
		Excluded:      excluded,
		Scores:        scores,
		StepLog:       evalRes.StepLog,
		ShadowReasons: evalRes.ShadowReasons,
	}

	// 5. Compute diff against previous tick.
	decision.Diff = diffAgainst(state.PreviousOrder, orderedIDs)
	decision.Diff.StickyHeld = evalRes.StickyHeld

	// 6. Atomic swap cache.
	ordered2 := ordered
	s.cache.Store(&ordered2)

	// 7. Update cross-tick state.
	s.mu.Lock()
	s.tickCount++
	s.previousOrder = orderedIDs
	s.previousExcluded = excludedIDs(excluded)
	s.lastScores = scores
	now := start.UnixMilli()
	newExcludedSince := make(map[string]int64, len(s.previousExcluded))
	for _, id := range s.previousExcluded {
		if existing, ok := s.excludedSince[id]; ok {
			newExcludedSince[id] = existing
		} else {
			newExcludedSince[id] = now
		}
	}
	s.excludedSince = newExcludedSince
	if decision.Diff.PrimaryChanged {
		t := start
		s.lastSwitchAt = &t
	}
	s.lastEvalAt = start
	// Append to the bounded decisions ring so diagnostic tooling can
	// replay the last N ticks. We retain even error-path decisions
	// (returned earlier in tickOnce); this is only the success-path
	// append. Failed evals are appended in the error branch too — see
	// `consecutiveFails` handler higher up.
	s.appendDecisionLocked(decision)
	s.mu.Unlock()

	s.emitMetrics(decision, state)
	s.logStepTrail(decision)
}

// logStepTrail emits DEBUG-level structured logs for the per-step trail
// when the engine's step-log toggle is on. No-op otherwise. Designed to
// be cheap when the underlying zerolog level filters DEBUG out — the
// short-circuit `Debug()` check (zerolog API contract) returns a
// `*Event` whose chained calls are nil-receiver no-ops.
//
// Format: one log line per stdlib step, plus one summary line per
// excluded upstream with its annotation trail. Keeps cardinality bounded
// (≤ ~30 stdlib steps per tick, ≤ N upstreams). Operators tail this
// log to see WHY a specific routing decision was made without
// re-running the simulator.
func (s *Slot) logStepTrail(d *Decision) {
	if d == nil || (len(d.Output.StepLog) == 0 && len(d.Output.Excluded) == 0) {
		return
	}
	// Cheap level check: we already know the trail data exists, but
	// zerolog only formats arguments when the level is enabled. Build
	// once, log once. The `Logger.Debug()` returns a no-op event when
	// the log level is above DEBUG so callers don't pay format costs.
	logger := s.engine.logger
	for i, step := range d.Output.StepLog {
		ev := logger.Debug().
			Str("network", s.networkID).
			Str("method", s.method).
			Str("tick_id", d.ID).
			Int("idx", i).
			Str("step", step.Step).
			Int("in", len(step.InIDs)).
			Int("out", len(step.OutIDs))
		if len(step.Dropped) > 0 {
			ev = ev.Strs("dropped", step.Dropped)
		}
		if len(step.Added) > 0 {
			ev = ev.Strs("added", step.Added)
		}
		if step.Reordered {
			ev = ev.Bool("reordered", true)
		}
		if len(step.Args) > 0 {
			ev = ev.RawJSON("args", step.Args)
		}
		ev.Msg("policy step")
	}
	for _, ex := range d.Output.Excluded {
		ev := logger.Debug().
			Str("network", s.networkID).
			Str("method", s.method).
			Str("tick_id", d.ID).
			Str("upstream", ex.ID).
			Str("step", ex.Step).
			Str("reason", ex.Reason)
		if len(ex.LeafReasons) > 0 {
			ev = ev.Strs("leaf_reasons", ex.LeafReasons)
		}
		ev.Msg("policy excluded upstream")
	}
}

// appendDecisionLocked pushes `d` onto the slot's bounded ring buffer.
// Caller MUST hold s.mu. O(1); evicts the oldest entry when full.
func (s *Slot) appendDecisionLocked(d *Decision) {
	if d == nil {
		return
	}
	s.decisions[s.decisionsHead] = d
	s.decisionsHead = (s.decisionsHead + 1) % decisionsRingSize
	if s.decisionsCount < decisionsRingSize {
		s.decisionsCount++
	}
}

// recentDecisions returns up to `limit` of the most-recent decisions
// from the ring, in OLDEST-first order. limit <= 0 means "all retained
// entries". Caller-owned shallow copies of the *Decision pointers; the
// underlying structs are NOT mutated after publishing.
func (s *Slot) recentDecisions(limit int) []*Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.decisionsCount
	if limit > 0 && n > limit {
		n = limit
	}
	if n == 0 {
		return nil
	}
	out := make([]*Decision, 0, n)
	start := s.decisionsHead - n
	if start < 0 {
		start += decisionsRingSize
	}
	for i := 0; i < n; i++ {
		idx := (start + i) % decisionsRingSize
		if s.decisions[idx] != nil {
			out = append(out, s.decisions[idx])
		}
	}
	return out
}

// emitMetrics translates a Decision into Prometheus signal. Cardinality
// is bounded by (project, network, method, upstream) — no per-step
// proliferation beyond the steps the user's eval actually fires.
func (s *Slot) emitMetrics(d *Decision, prevState DecisionState) {
	project := s.engine.projectID
	labels := []string{project, s.networkLabel, s.method}

	telemetry.MetricSelectionEvalDurationSeconds.WithLabelValues(labels...).
		Observe(d.EvalDuration.Seconds())

	if d.Error != "" {
		kind := "throw"
		if strings.Contains(d.Error, "timed out") {
			kind = "timeout"
		} else if strings.Contains(d.Error, ErrInvalidReturn.Error()) {
			kind = "invalid_return"
		}
		telemetry.MetricSelectionEvalErrorsTotal.WithLabelValues(project, s.networkLabel, s.method, kind).Inc()
		return
	}

	telemetry.MetricSelectionEligibleUpstreams.WithLabelValues(labels...).Set(float64(len(d.Output.Order)))

	// Build a set of currently-in-rotation IDs for excluded-seconds and
	// readmit-age gauges. O(n) memory, n = ordered set size.
	inOrder := make(map[string]struct{}, len(d.Output.Order))
	for i, id := range d.Output.Order {
		telemetry.MetricSelectionPosition.WithLabelValues(project, s.networkLabel, s.method, id).Set(float64(i))
		inOrder[id] = struct{}{}
		// Score gauge — populated per upstream that survived sortByScore.
		// Upstreams added after scoring (probeExcluded/forceInclude) or
		// policies without a sortByScore step have no entry.
		if score, ok := d.Output.Scores[id]; ok {
			telemetry.MetricSelectionScore.WithLabelValues(project, s.networkLabel, s.method, id).Set(score)
		}
		// In-rotation upstreams report 0 excluded-seconds. Resets the
		// gauge cleanly when a previously-excluded upstream comes back —
		// dashboards see the transition immediately.
		telemetry.MetricSelectionExcludedSeconds.WithLabelValues(project, s.networkLabel, s.method, id).Set(0)
	}

	// Excluded upstreams: position=-1, per-leaf exclusion counters, and
	// excluded-seconds gauge.
	nowMs := d.TickAt.UnixMilli()
	for _, ex := range d.Output.Excluded {
		telemetry.MetricSelectionPosition.WithLabelValues(project, s.networkLabel, s.method, ex.ID).Set(-1)
		step := ex.Step
		if step == "" {
			step = "eval"
		}
		telemetry.MetricSelectionRejectionTotal.WithLabelValues(project, s.networkLabel, s.method, ex.ID, step).Inc()
		// Per-leaf exclusion attribution. The Reason field is the
		// display string (carries thresholds — high cardinality), so we
		// do NOT emit it as a label. Instead emit one increment per leaf
		// SLUG (bounded by the predicate-factory set, ~25 unique).
		// Excluded upstreams dropped by non-`excludeIf` steps have no
		// leaves — they're already counted by `selection_rejection_total{step}`.
		for _, slug := range ex.LeafReasons {
			telemetry.MetricSelectionExclusionTotal.WithLabelValues(project, s.networkLabel, s.method, ex.ID, slug).Inc()
		}
		// `prevState.ExcludedSince` is the pre-tick clone captured under
		// s.mu at the start of tickOnce. Reading the LIVE `s.excludedSince`
		// here would be a data race with the next tick (emitMetrics runs
		// outside s.mu). For upstreams newly excluded this tick the prev
		// clone has no entry — we don't emit and the gauge stays at its
		// previous value (`0` from when it was in rotation), which is the
		// correct cold-start age. For upstreams already excluded last
		// tick the timestamp is identical (the slot preserves it across
		// ticks).
		if since, ok := prevState.ExcludedSince[ex.ID]; ok && since > 0 {
			age := time.Duration(nowMs-since) * time.Millisecond
			if age < 0 {
				age = 0
			}
			telemetry.MetricSelectionExcludedSeconds.WithLabelValues(project, s.networkLabel, s.method, ex.ID).Set(age.Seconds())
		}
	}

	// Shadow exclusions — predicates wrapped in `shadowExcludeIf` that
	// WOULD have dropped the upstream but didn't. The upstream is still
	// in `d.Output.Order`; we just count the leaf slugs so operators can
	// compare shadow rate to real exclusion rate when auditioning a new
	// rule. Same option-(c) attribution as the real counter.
	for id, slugs := range d.Output.ShadowReasons {
		for _, slug := range slugs {
			telemetry.MetricSelectionShadowExclusionTotal.WithLabelValues(project, s.networkLabel, s.method, id, slug).Inc()
		}
	}

	// Readmit signal: any ID in this tick's Order that was excluded last
	// tick — i.e. transitioned from -1 → in-rotation. Increment per-upstream
	// counter and observe the per-network histogram of "how long was it out".
	if len(prevState.PreviousExcluded) > 0 {
		prevExcludedSet := make(map[string]int64, len(prevState.PreviousExcluded))
		for _, id := range prevState.PreviousExcluded {
			if since, ok := prevState.ExcludedSince[id]; ok {
				prevExcludedSet[id] = since
			} else {
				prevExcludedSet[id] = 0
			}
		}
		for id := range inOrder {
			since, wasExcluded := prevExcludedSet[id]
			if !wasExcluded {
				continue
			}
			telemetry.MetricSelectionReadmitTotal.WithLabelValues(project, s.networkLabel, s.method, id).Inc()
			if since > 0 {
				age := time.Duration(nowMs-since) * time.Millisecond
				if age > 0 {
					telemetry.MetricSelectionReadmitAgeSeconds.WithLabelValues(labels...).Observe(age.Seconds())
				}
			}
		}
	}

	// Sticky-primary holds: chain set `__policyStickyHeld = true` when
	// the previous primary was kept against a challenger (cooldown
	// active or hysteresis not exceeded). Attribute to the held primary.
	if d.Diff.StickyHeld && len(d.Output.Order) > 0 {
		telemetry.MetricSelectionStickyHoldTotal.WithLabelValues(project, s.networkLabel, s.method, d.Output.Order[0]).Inc()
	}

	if d.Diff.PrimaryChanged {
		from := ""
		if len(prevState.PreviousOrder) > 0 {
			from = prevState.PreviousOrder[0]
		}
		to := ""
		if len(d.Output.Order) > 0 {
			to = d.Output.Order[0]
		}
		if from != to {
			telemetry.MetricSelectionPrimarySwitchTotal.WithLabelValues(project, s.networkLabel, s.method, from, to).Inc()
		}
	}
}

// snapshotMetrics captures one consistent view of every upstream's metrics
// for the given (method, finality). The eval sees this snapshot;
// subsequent updates during the eval don't change what the JS reads.
//
// `local` is the per-(upstream, method, finality) view exposed as
// `u.metrics` — the most-specific tracker bucket the slot's grain
// supports. When the tracker hasn't opted into per-finality tracking,
// the per-finality lookup transparently falls back to the
// (upstream, method, *) all-finalities aggregate, so callers always
// see populated metrics.
//
// `acrossMethods` is the per-(upstream, "*", "*") full wildcard
// aggregate exposed as `u.metricsAcrossMethods` — used by
// `stickyPrimary({scope: NETWORK})` (and other cross-slot primitives)
// to score a primary from a metric every slot in the scope sees
// identically. When the slot itself runs at method="*" + finality=All
// the two slices share the same map by reference (no separate
// aggregate needed).
func snapshotMetrics(tr healthTracker, ups []common.Upstream, method string, finality common.DataFinalityState) (local, acrossMethods map[string]UpstreamMetrics) {
	local = make(map[string]UpstreamMetrics, len(ups))
	for _, u := range ups {
		local[u.Id()] = readUpstreamMetrics(tr, u, method, finality)
	}
	if method == "*" && finality == common.DataFinalityStateAll {
		return local, local
	}
	acrossMethods = make(map[string]UpstreamMetrics, len(ups))
	for _, u := range ups {
		acrossMethods[u.Id()] = readUpstreamMetrics(tr, u, "*", common.DataFinalityStateAll)
	}
	return local, acrossMethods
}

// parseFinality maps a slot's string finality (the canonical
// `realtime`/`unfinalized`/`finalized`/`unknown` plus the engine's
// `"*"` wildcard) to the tracker-side `DataFinalityState`. The
// wildcard maps to `DataFinalityStateAll` — the cross-finality
// aggregate the tracker maintains regardless of whether
// per-finality tracking is on.
func parseFinality(s string) common.DataFinalityState {
	switch s {
	case "realtime":
		return common.DataFinalityStateRealtime
	case "unfinalized":
		return common.DataFinalityStateUnfinalized
	case "finalized":
		return common.DataFinalityStateFinalized
	case "unknown":
		return common.DataFinalityStateUnknown
	default:
		return common.DataFinalityStateAll
	}
}

// upstreamIDs extracts the IDs of every upstream in the given slice.
func upstreamIDs(ups []common.Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Id()
	}
	return out
}

// stepPrefix is the sentinel the JS `define()` wrapper prepends to
// `__policyLeafReasons[id]` to record the first stdlib primitive that
// dropped the upstream — folded into the leaf-reason channel so we
// only maintain ONE per-tick map instead of two parallel ones.
const stepPrefix = "@step:"

// splitStepFromLeafReasons separates the `"@step:NAME"` sentinel
// entries from the real leaf-reason slugs in a single LeafReasons
// array. First sentinel wins (matches JS-side first-step-wins). Any
// subsequent sentinels are silently dropped — defensive; the JS guard
// already prevents them — so a future writer regression can't leak
// `@step:*` into the `selection_exclusion_total{reason}` label.
func splitStepFromLeafReasons(entries []string) (step string, leaves []string) {
	leaves = entries[:0]
	for _, e := range entries {
		if len(e) >= len(stepPrefix) && e[:len(stepPrefix)] == stepPrefix {
			if step == "" {
				step = e[len(stepPrefix):]
			}
			continue
		}
		leaves = append(leaves, e)
	}
	if len(leaves) == 0 {
		leaves = nil
	}
	return step, leaves
}

// enrichExcluded resolves a diagnostic `Reason` for each excluded
// upstream. Preferred order:
//  1. the first `LeafReasons` slug (rich, set by `excludeIf`'s
//     option-(c) walk),
//  2. the `Step` name (`removeCordoned`, `byTag`, ...),
//  3. the default "not in eval result" set by materializeOrder for raw
//     `Array.filter` fall-throughs.
//
// `Reason` is the diagnostic surface — `RecentDecisions` and the
// simulator UI render it; metrics emit `Step` + `LeafReasons` directly,
// not `Reason`.
//
// Must run AFTER `splitStepFromLeafReasons` has populated `Step` and
// `LeafReasons` on each excluded upstream.
func enrichExcluded(excluded []ExcludedUpstream) {
	for i := range excluded {
		if len(excluded[i].LeafReasons) > 0 {
			excluded[i].Reason = excluded[i].LeafReasons[0]
		} else if excluded[i].Step != "" {
			excluded[i].Reason = excluded[i].Step
		}
	}
}

// materializeOrder maps an ordered slice of upstream IDs back to the real
// Upstream pointers. Any ID returned by the eval that isn't present in the
// input set is silently dropped (the validate step in eval.go already
// rejects truly unknown ids). Returns the resolved order plus the list of
// inputs that were excluded.
func materializeOrder(ups []common.Upstream, orderedIDs []string) ([]common.Upstream, []ExcludedUpstream) {
	index := make(map[string]common.Upstream, len(ups))
	for _, u := range ups {
		index[u.Id()] = u
	}
	ordered := make([]common.Upstream, 0, len(orderedIDs))
	used := make(map[string]bool, len(orderedIDs))
	for _, id := range orderedIDs {
		if u, ok := index[id]; ok && !used[id] {
			ordered = append(ordered, u)
			used[id] = true
		}
	}
	var excluded []ExcludedUpstream
	for _, u := range ups {
		if !used[u.Id()] {
			excluded = append(excluded, ExcludedUpstream{ID: u.Id(), Reason: "not in eval result"})
		}
	}
	return ordered, excluded
}

func excludedIDs(ex []ExcludedUpstream) []string {
	out := make([]string, len(ex))
	for i, e := range ex {
		out[i] = e.ID
	}
	return out
}

func diffAgainst(prev, current []string) DecisionDiff {
	d := DecisionDiff{}
	prevSet := make(map[string]bool, len(prev))
	for _, id := range prev {
		prevSet[id] = true
	}
	curSet := make(map[string]bool, len(current))
	for _, id := range current {
		curSet[id] = true
	}
	for _, id := range current {
		if !prevSet[id] {
			d.Added = append(d.Added, id)
		}
	}
	for _, id := range prev {
		if !curSet[id] {
			d.Removed = append(d.Removed, id)
		}
	}
	d.OrderChanged = !stringSlicesEqual(prev, current)
	if len(current) > 0 && (len(prev) == 0 || prev[0] != current[0]) {
		d.PrimaryChanged = true
	}
	return d
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func cloneInt64Map(m map[string]int64) map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}
