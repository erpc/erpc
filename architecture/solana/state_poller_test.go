package solana

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

// ── stubSlotVar ──────────────────────────────────────────────────────────────
// stubSlotVar is a test-only implementation of common.SlotSharedVariable.
// It has monotonic semantics matching data.CounterInt64SharedVariable: TryUpdate
// only advances if the new value is strictly greater.

type stubSlotVar struct {
	mu                 sync.Mutex
	val                int64
	onValueCbs         []func(int64)
	onLargeRollbackCbs []func(int64, int64)
	rollbackThreshold  int64
}

func newStubSlotVar(rollbackThreshold int64) *stubSlotVar {
	return &stubSlotVar{rollbackThreshold: rollbackThreshold}
}

func (s *stubSlotVar) GetValue() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.val
}

func (s *stubSlotVar) TryUpdate(_ context.Context, newVal int64) int64 {
	s.mu.Lock()
	if newVal <= s.val {
		result := s.val
		s.mu.Unlock()
		return result
	}
	s.val = newVal
	// Copy callbacks before unlocking to avoid holding lock during callback execution.
	cbs := append([]func(int64){}, s.onValueCbs...)
	s.mu.Unlock()

	for _, cb := range cbs {
		cb(newVal)
	}

	s.mu.Lock()
	result := s.val
	s.mu.Unlock()
	return result
}

func (s *stubSlotVar) OnValue(cb func(int64)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onValueCbs = append(s.onValueCbs, cb)
}

func (s *stubSlotVar) OnLargeRollback(cb func(currentVal, newVal int64)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onLargeRollbackCbs = append(s.onLargeRollbackCbs, cb)
}

func (s *stubSlotVar) IsStale(_ time.Duration) bool { return true }

// triggerLargeRollback allows tests to manually fire the rollback callbacks.
func (s *stubSlotVar) triggerLargeRollback(current, newVal int64) {
	s.mu.Lock()
	cbs := append([]func(int64, int64){}, s.onLargeRollbackCbs...)
	s.mu.Unlock()
	for _, cb := range cbs {
		cb(current, newVal)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func newTestPoller() *SolanaStatePoller {
	latestVar := newStubSlotVar(DefaultToleratedSlotRollback)
	finalizedVar := newStubSlotVar(DefaultToleratedSlotRollback)
	p := &SolanaStatePoller{
		Enabled:             true,
		appCtx:              context.Background(),
		latestSlotShared:    latestVar,
		finalizedSlotShared: finalizedVar,
	}
	p.healthy.Store(true)
	return p
}

func newTestPollerWithTracker(t *testing.T) (*SolanaStatePoller, *health.Tracker, *stubSlotVar, *stubSlotVar) {
	t.Helper()
	lg := zerolog.Nop()
	tracker := health.NewTracker(&lg, "test-project", 10*time.Minute)

	latestVar := newStubSlotVar(DefaultToleratedSlotRollback)
	finalizedVar := newStubSlotVar(DefaultToleratedSlotRollback)

	ups := &stubUpstream{id: "test-upstream", networkId: "solana:mainnet-beta", tracker: tracker}

	p := NewSolanaStatePoller(
		"test-project",
		context.Background(),
		&lg,
		ups,
		tracker,
		latestVar,
		finalizedVar,
	)
	return p, tracker, latestVar, finalizedVar
}

// stubUpstream is a minimal common.Upstream for tracker tests.
type stubUpstream struct {
	id        string
	networkId string
	tracker   *health.Tracker
}

func (u *stubUpstream) Id() string                     { return u.id }
func (u *stubUpstream) NetworkId() string              { return u.networkId }
func (u *stubUpstream) NetworkLabel() string           { return u.networkId }
func (u *stubUpstream) VendorName() string             { return "stub" }
func (u *stubUpstream) Config() *common.UpstreamConfig { return &common.UpstreamConfig{Id: u.id} }
func (u *stubUpstream) Logger() *zerolog.Logger {
	lg := zerolog.Nop()
	return &lg
}
func (u *stubUpstream) Forward(_ context.Context, _ *common.NormalizedRequest, _ bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (u *stubUpstream) MetricsTracker() *health.Tracker             { return u.tracker }
func (u *stubUpstream) Tracker() common.HealthTracker               { return nil }
func (u *stubUpstream) EvmStatePoller() common.EvmStatePoller       { return nil }
func (u *stubUpstream) SolanaStatePoller() common.SolanaStatePoller { return nil }
func (u *stubUpstream) Vendor() common.Vendor                       { return nil }
func (u *stubUpstream) Cordon(_ string, _ string)                   {}
func (u *stubUpstream) Uncordon(_ string, _ string)                 {}
func (u *stubUpstream) IgnoreMethod(_ string)                       {}

// ── Gap 1+2: Tracker integration via OnValue callbacks ───────────────────────

func TestTrackerIntegration_SuggestLatestSlotUpdatesTracker(t *testing.T) {
	p, tracker, _, _ := newTestPollerWithTracker(t)

	p.SuggestLatestSlot(500_000)

	metrics := tracker.GetUpstreamMethodMetrics(p.upstream, "*")
	require.NotNil(t, metrics, "tracker should have metrics for the upstream")
	// BlockHeadLag is relative — after one upstream update the network max equals
	// the upstream value so lag should be 0.
	assert.Equal(t, int64(0), metrics.BlockHeadLag.Load(),
		"block head lag should be 0 when only one upstream reports a slot")
}

func TestTrackerIntegration_FinalizedSlotUpdatesTracker(t *testing.T) {
	p, tracker, _, _ := newTestPollerWithTracker(t)

	p.SuggestFinalizedSlot(499_800)

	metrics := tracker.GetUpstreamMethodMetrics(p.upstream, "*")
	require.NotNil(t, metrics)
	// finalizationLag = 0 when only one upstream in network
	assert.Equal(t, int64(0), metrics.FinalizationLag.Load())
}

func TestTrackerIntegration_OnValueCallbackFires(t *testing.T) {
	lg := zerolog.Nop()
	tracker := health.NewTracker(&lg, "test-project", 10*time.Minute)
	ups := &stubUpstream{id: "ups-cb", networkId: "solana:mainnet-beta", tracker: tracker}
	lv := newStubSlotVar(DefaultToleratedSlotRollback)
	fv := newStubSlotVar(DefaultToleratedSlotRollback)

	p := NewSolanaStatePoller("proj", context.Background(), &lg, ups, tracker, lv, fv)

	// Suggest a slot — this routes through TryUpdate which fires all OnValue callbacks.
	p.SuggestLatestSlot(500_000)

	// Value must be advanced.
	assert.Equal(t, int64(500_000), p.LatestSlot())

	// The tracker should have metrics for the upstream because NewSolanaStatePoller
	// wires an OnValue callback that calls tracker.SetLatestBlockNumber.
	metrics := tracker.GetUpstreamMethodMetrics(p.upstream, "*")
	require.NotNil(t, metrics, "tracker should have metrics after a slot suggestion")
	// Lag is 0 because there is only one upstream in the network.
	assert.Equal(t, int64(0), metrics.BlockHeadLag.Load(),
		"lag should be 0 when only one upstream reports")
}

func TestTrackerIntegration_LargeRollbackCallbackFires(t *testing.T) {
	_, _, latestVar, _ := newTestPollerWithTracker(t)

	var rollbackFired atomic.Bool
	// Register a second rollback listener on top of the tracker one
	latestVar.OnLargeRollback(func(_, _ int64) {
		rollbackFired.Store(true)
	})

	// Manually trigger rollback — simulates a large slot reorg
	latestVar.triggerLargeRollback(500_000, 1_000)
	assert.True(t, rollbackFired.Load(), "large rollback callback should have fired")
}

// ── Gap 1+2: SlotSharedVariable nil safety ───────────────────────────────────

func TestSuggestLatestSlot_NilSharedVar_NoPanic(t *testing.T) {
	p := &SolanaStatePoller{Enabled: true, appCtx: context.Background()}
	// latestSlotShared is nil — should not panic
	assert.NotPanics(t, func() { p.SuggestLatestSlot(100) })
	assert.Equal(t, int64(0), p.LatestSlot())
}

func TestSuggestFinalizedSlot_NilSharedVar_NoPanic(t *testing.T) {
	p := &SolanaStatePoller{Enabled: true, appCtx: context.Background()}
	assert.NotPanics(t, func() { p.SuggestFinalizedSlot(100) })
	assert.Equal(t, int64(0), p.FinalizedSlot())
}

// ── SuggestLatestSlot monotonic ───────────────────────────────────────────────

func TestSuggestLatestSlot_AcceptsHigherSlot(t *testing.T) {
	p := newTestPoller()
	p.SuggestLatestSlot(100)
	assert.Equal(t, int64(100), p.LatestSlot())
}

func TestSuggestLatestSlot_IgnoresLowerSlot(t *testing.T) {
	p := newTestPoller()
	p.SuggestLatestSlot(500)
	p.SuggestLatestSlot(300)
	assert.Equal(t, int64(500), p.LatestSlot(), "lower slot must not overwrite higher")
}

func TestSuggestLatestSlot_IgnoresEqualSlot(t *testing.T) {
	p := newTestPoller()
	p.SuggestLatestSlot(200)
	p.SuggestLatestSlot(200)
	assert.Equal(t, int64(200), p.LatestSlot())
}

func TestSuggestLatestSlot_ConcurrentUpdates(t *testing.T) {
	p := newTestPoller()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.SuggestLatestSlot(int64(i))
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(99), p.LatestSlot(),
		"after concurrent updates, latest slot must be the maximum seen")
}

// ── SuggestFinalizedSlot ─────────────────────────────────────────────────────

func TestSuggestFinalizedSlot_AcceptsHigherSlot(t *testing.T) {
	p := newTestPoller()
	p.SuggestFinalizedSlot(50)
	assert.Equal(t, int64(50), p.FinalizedSlot())
}

func TestSuggestFinalizedSlot_IgnoresLowerSlot(t *testing.T) {
	p := newTestPoller()
	p.SuggestFinalizedSlot(400)
	p.SuggestFinalizedSlot(100)
	assert.Equal(t, int64(400), p.FinalizedSlot())
}

func TestSuggestFinalizedSlot_ConcurrentUpdates(t *testing.T) {
	p := newTestPoller()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.SuggestFinalizedSlot(int64(i * 2))
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(98), p.FinalizedSlot())
}

// ── IsObjectNull ─────────────────────────────────────────────────────────────

func TestIsObjectNull_NilPointer(t *testing.T) {
	var p *SolanaStatePoller
	assert.True(t, p.IsObjectNull())
}

func TestIsObjectNull_DisabledPoller(t *testing.T) {
	p := &SolanaStatePoller{Enabled: false}
	assert.True(t, p.IsObjectNull())
}

func TestIsObjectNull_EnabledPoller(t *testing.T) {
	p := newTestPoller()
	assert.False(t, p.IsObjectNull())
}

// ── IsHealthy ────────────────────────────────────────────────────────────────

func TestIsHealthy_DefaultsToTrue(t *testing.T) {
	p := newTestPoller()
	assert.True(t, p.IsHealthy(), "newly created poller should default to healthy")
}

func TestIsHealthy_CanBeSetFalse(t *testing.T) {
	p := newTestPoller()
	p.healthy.Store(false)
	assert.False(t, p.IsHealthy())
}

func TestIsHealthy_CanBeToggledBackToTrue(t *testing.T) {
	p := newTestPoller()
	p.healthy.Store(false)
	p.healthy.Store(true)
	assert.True(t, p.IsHealthy())
}

func TestIsHealthy_ConcurrentAccess(t *testing.T) {
	p := newTestPoller()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); p.healthy.Store(true) }()
		go func() { defer wg.Done(); _ = p.IsHealthy() }()
	}
	wg.Wait()
	// No race — just checking it doesn't panic or deadlock
}

// ── LatestSlot / FinalizedSlot zero values ───────────────────────────────────

func TestLatestSlot_InitiallyZero(t *testing.T) {
	p := newTestPoller()
	assert.Equal(t, int64(0), p.LatestSlot())
}

func TestFinalizedSlot_InitiallyZero(t *testing.T) {
	p := newTestPoller()
	assert.Equal(t, int64(0), p.FinalizedSlot())
}

// ── DefaultToleratedSlotRollback constant ────────────────────────────────────

func TestDefaultToleratedSlotRollback_Value(t *testing.T) {
	// Ensure the constant exists and has a sensible value (> 0, < max reasonable slot delta).
	assert.Greater(t, DefaultToleratedSlotRollback, int64(0))
	assert.Less(t, DefaultToleratedSlotRollback, int64(100_000))
}

// ── Gap 3: SlotSharedVariable OnValue callback wiring ────────────────────────

func TestOnValueCallback_FiredOnAdvance(t *testing.T) {
	lv := newStubSlotVar(512)
	var seen int64
	lv.OnValue(func(v int64) { seen = v })

	lv.TryUpdate(context.Background(), 42)
	assert.Equal(t, int64(42), seen, "OnValue callback should fire with the new slot value")
}

func TestOnValueCallback_NotFiredOnSameValue(t *testing.T) {
	lv := newStubSlotVar(512)
	lv.TryUpdate(context.Background(), 42)

	var calls int
	lv.OnValue(func(_ int64) { calls++ })
	lv.TryUpdate(context.Background(), 42) // same value, should not advance
	assert.Equal(t, 0, calls, "OnValue should not fire when value doesn't advance")
}

func TestOnValueCallback_NotFiredOnLowerValue(t *testing.T) {
	lv := newStubSlotVar(512)
	lv.TryUpdate(context.Background(), 100)

	var calls int
	lv.OnValue(func(_ int64) { calls++ })
	lv.TryUpdate(context.Background(), 50) // lower — should not advance
	assert.Equal(t, 0, calls, "OnValue should not fire when value doesn't advance")
}
