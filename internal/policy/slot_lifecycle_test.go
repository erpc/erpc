package policy

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// goTracked ties request-path re-evaluations (syncLagMask) to the slot's
// lifecycle: stop() waits for a running one, and none starts once stop()
// began, so a stopped slot never re-evaluates afterwards.
func TestSlot_GoTrackedDrainsOnStop(t *testing.T) {
	s := &Slot{stopCh: make(chan struct{})}
	release := make(chan struct{})
	var ran, finished atomic.Bool
	require.True(t, s.goTracked(func() {
		ran.Store(true)
		<-release
		finished.Store(true)
	}))
	require.Eventually(t, ran.Load, time.Second, time.Millisecond)

	stopped := make(chan struct{})
	go func() { s.stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop() returned while a tracked re-evaluation was running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-stopped
	require.True(t, finished.Load())

	require.False(t, s.goTracked(func() { t.Error("ran after stop") }), "nothing starts after stop")
}

// Concurrent goTracked/stop never trips the WaitGroup reuse check and never
// runs fn after stop() returned.
func TestSlot_GoTrackedStopRace(t *testing.T) {
	for i := 0; i < 200; i++ {
		s := &Slot{stopCh: make(chan struct{})}
		var after atomic.Bool
		var stoppedFlag atomic.Bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			for j := 0; j < 20; j++ {
				s.goTracked(func() {
					if stoppedFlag.Load() {
						after.Store(true)
					}
				})
			}
		}()
		s.stop()
		stoppedFlag.Store(true)
		<-done
		require.False(t, after.Load())
	}
}
