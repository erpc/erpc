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

// Slot is the per-(network, method) state container. Each Slot owns a
// goroutine driven by its ticker; the request-path never touches anything
// here except `cache` via an atomic load.
type Slot struct {
	engine    *Engine
	networkID string
	method    string

	upstreamsFn func() []common.Upstream
	cfg         *common.SelectionPolicyConfig

	// cache holds the most recent eval output. nil before the first tick.
	cache atomic.Pointer[[]common.Upstream]

	// crossTick state — touched only inside tickOnce, no locking.
	mu               sync.Mutex // protects fields below from admin reads
	tickCount        uint64
	previousOrder    []string
	previousExcluded []string
	lastSwitchAt     *time.Time
	excludedSince    map[string]int64

	// Bad-eval streak counter; spec §5.7 falls back to the default policy
	// after 3 consecutive failures. (Hookup deferred to Phase 5.15.)
	consecutiveFails int

	ring *ringBuffer

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func newSlot(e *Engine, networkID, method string, upstreamsFn func() []common.Upstream, cfg *common.SelectionPolicyConfig) *Slot {
	historyTicks := 1
	if cfg.EvalInterval > 0 && cfg.DecisionHistory > 0 {
		historyTicks = int(cfg.DecisionHistory.Duration() / cfg.EvalInterval.Duration())
		if historyTicks < 1 {
			historyTicks = 1
		}
	}
	return &Slot{
		engine:        e,
		networkID:     networkID,
		method:        method,
		upstreamsFn:   upstreamsFn,
		cfg:           cfg,
		excludedSince: make(map[string]int64),
		ring:          newRingBuffer(historyTicks),
		stopCh:        make(chan struct{}),
	}
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
				s.tickOnce()
			}
		}
	}()
}

func (s *Slot) stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

// tickOnce runs one eval cycle synchronously. Exported via TickForTest.
func (s *Slot) tickOnce() {
	start := time.Now()
	timeout := s.cfg.EvalTimeout.Duration()

	// Re-resolve upstreams each tick so newly-bootstrapped ones become visible.
	ups := s.upstreamsFn()

	// 1. Snapshot metrics for every upstream.
	metrics := snapshotMetrics(s.engine.tracker, ups, s.method)

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
	s.mu.Unlock()

	// 3. Run the eval (with optional timeout).
	var (
		orderedIDs []string
		evalErr    error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		orderedIDs, evalErr = runEval(s.engine.pool, s.cfg, ups, metrics, evalCtx)
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
		s.ring.append(decision)
		s.engine.logger.Warn().
			Str("network", s.networkID).
			Str("method", s.method).
			Err(evalErr).
			Msg("selection policy eval failed; retaining previous cache")
		return
	}
	s.consecutiveFails = 0

	// 4. Validate + materialize the ordered upstream slice.
	ordered, excluded := materializeOrder(ups, orderedIDs)
	decision.Output = DecisionOutput{Order: orderedIDs, Excluded: excluded}

	// 5. Compute diff against previous tick.
	decision.Diff = diffAgainst(state.PreviousOrder, orderedIDs)

	// 6. Atomic swap cache.
	ordered2 := ordered
	s.cache.Store(&ordered2)

	// 7. Update cross-tick state.
	s.mu.Lock()
	s.tickCount++
	s.previousOrder = orderedIDs
	s.previousExcluded = excludedIDs(excluded)
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
	s.mu.Unlock()

	s.ring.append(decision)
	s.emitMetrics(decision, state)
}

// emitMetrics translates a Decision into Prometheus signal. Cardinality
// is bounded by (project, network, method, upstream) — no per-step
// proliferation beyond the steps the user's eval actually fires.
func (s *Slot) emitMetrics(d *Decision, prevState DecisionState) {
	project := s.engine.projectID
	labels := []string{project, s.networkID, s.method}

	telemetry.MetricSelectionEvalDurationSeconds.WithLabelValues(labels...).
		Observe(d.EvalDuration.Seconds())

	if d.Error != "" {
		kind := "throw"
		if strings.Contains(d.Error, "timed out") {
			kind = "timeout"
		} else if strings.Contains(d.Error, ErrInvalidReturn.Error()) {
			kind = "invalid_return"
		}
		telemetry.MetricSelectionEvalErrorsTotal.WithLabelValues(project, s.networkID, s.method, kind).Inc()
		return
	}

	telemetry.MetricSelectionEligibleUpstreams.WithLabelValues(labels...).Set(float64(len(d.Output.Order)))

	for i, id := range d.Output.Order {
		telemetry.MetricSelectionPosition.WithLabelValues(project, s.networkID, s.method, id).Set(float64(i))
	}
	for _, ex := range d.Output.Excluded {
		telemetry.MetricSelectionPosition.WithLabelValues(project, s.networkID, s.method, ex.ID).Set(-1)
		step := ex.Step
		if step == "" {
			step = "eval"
		}
		telemetry.MetricSelectionRejectionTotal.WithLabelValues(project, s.networkID, s.method, ex.ID, step).Inc()
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
			telemetry.MetricSelectionPrimarySwitchTotal.WithLabelValues(project, s.networkID, s.method, from, to).Inc()
		}
	}
}

// snapshotMetrics captures one consistent view of every upstream's metrics
// for the given method. The eval sees this snapshot; subsequent updates
// during the eval don't change what the JS reads.
func snapshotMetrics(tr healthTracker, ups []common.Upstream, method string) map[string]UpstreamMetrics {
	out := make(map[string]UpstreamMetrics, len(ups))
	for _, u := range ups {
		out[u.Id()] = readUpstreamMetrics(tr, u, method)
	}
	return out
}

// upstreamIDs extracts the IDs of every upstream in the given slice.
func upstreamIDs(ups []common.Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Id()
	}
	return out
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
