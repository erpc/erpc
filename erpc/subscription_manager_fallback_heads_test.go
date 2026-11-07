package erpc

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeHeadsSwitch struct {
	mu    sync.Mutex
	calls []bool
}

func (f *fakeHeadsSwitch) SetHeadsWanted(_ context.Context, want bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, want)
}

func (f *fakeHeadsSwitch) history() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.calls...)
}

func shortenFallbackHeadsGate(t *testing.T) {
	check, grace, release := fallbackHeadsCheckInterval, fallbackHeadsStartupGrace, fallbackHeadsReleaseAfter
	fallbackHeadsCheckInterval = 10 * time.Millisecond
	fallbackHeadsStartupGrace = 50 * time.Millisecond
	fallbackHeadsReleaseAfter = 300 * time.Millisecond
	t.Cleanup(func() {
		fallbackHeadsCheckInterval, fallbackHeadsStartupGrace, fallbackHeadsReleaseAfter = check, grace, release
	})
}

// TestGateFallbackHeads: fallback heads are wanted as soon as the primaries
// stop streaming, and released only after they streamed for the release
// window without a break.
func TestGateFallbackHeads(t *testing.T) {
	shortenFallbackHeadsGate(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var live atomic.Bool
	live.Store(true)
	f1, f2 := &fakeHeadsSwitch{}, &fakeHeadsSwitch{}
	var reconciles atomic.Int32
	go gateFallbackHeads(ctx, func(context.Context) bool { return live.Load() }, []headsSwitch{f1, f2}, func() { reconciles.Add(1) })

	// Primaries up: fallbacks are never asked for heads.
	time.Sleep(150 * time.Millisecond)
	require.Empty(t, f1.history())

	// Outage: both fallbacks subscribe within a check interval or two.
	live.Store(false)
	require.Eventually(t, func() bool { return len(f1.history()) == 1 && len(f2.history()) == 1 },
		time.Second, 5*time.Millisecond)
	require.Equal(t, []bool{true}, f1.history())
	require.Eventually(t, func() bool { return reconciles.Load() == 1 }, time.Second, 5*time.Millisecond,
		"filters must be reconciled as soon as the primaries drop")

	// Primaries flap back briefly: no release.
	live.Store(true)
	time.Sleep(150 * time.Millisecond)
	live.Store(false)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, []bool{true}, f1.history())

	// Primaries back for good: released after the full window.
	live.Store(true)
	start := time.Now()
	require.Eventually(t, func() bool { return len(f1.history()) == 2 }, 2*time.Second, 5*time.Millisecond)
	require.GreaterOrEqual(t, time.Since(start), fallbackHeadsReleaseAfter)
	require.Equal(t, []bool{true, false}, f1.history())
	require.Equal(t, []bool{true, false}, f2.history())
	// one reconcile per change: down, up, down, up
	require.Equal(t, int32(4), reconciles.Load())
}

// TestGateFallbackHeadsOutageAtStartup: with no primary streaming once the
// startup grace ends, the fallbacks subscribe.
func TestGateFallbackHeadsOutageAtStartup(t *testing.T) {
	shortenFallbackHeadsGate(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeHeadsSwitch{}
	start := time.Now()
	go gateFallbackHeads(ctx, func(context.Context) bool { return false }, []headsSwitch{f}, func() {})
	require.Eventually(t, func() bool { return len(f.history()) == 1 }, time.Second, 5*time.Millisecond)
	require.GreaterOrEqual(t, time.Since(start), fallbackHeadsStartupGrace)
}
