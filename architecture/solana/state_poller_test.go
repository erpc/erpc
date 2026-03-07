package solana

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// newTestPoller creates a SolanaStatePoller with nil upstream — safe for
// testing pure in-memory operations that don't call Forward.
func newTestPoller() *SolanaStatePoller {
	p := &SolanaStatePoller{Enabled: true}
	p.healthy.Store(true)
	return p
}

// ── SuggestLatestSlot ────────────────────────────────────────────────────────

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
	// After all goroutines, latest must be the maximum value seen
	assert.Equal(t, int64(99), p.LatestSlot())
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

// ── LatestSlot / FinalizedSlot zero values ───────────────────────────────────

func TestLatestSlot_InitiallyZero(t *testing.T) {
	p := newTestPoller()
	assert.Equal(t, int64(0), p.LatestSlot())
}

func TestFinalizedSlot_InitiallyZero(t *testing.T) {
	p := newTestPoller()
	assert.Equal(t, int64(0), p.FinalizedSlot())
}
