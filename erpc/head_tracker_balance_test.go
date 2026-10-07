package erpc

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/data"
	"github.com/stretchr/testify/require"
)

type countingHeartbeatSSR struct {
	data.SharedStateRegistry
	calls atomic.Int64
	n     atomic.Int64
}

func (s *countingHeartbeatSSR) HeartbeatReplicas(context.Context, time.Duration) (int, error) {
	s.calls.Add(1)
	return int(s.n.Load()), nil
}

// The replica heartbeat survives the tracker whose context started it: it
// hands over to another registered tracker, and a register after every
// tracker stopped starts a new loop.
func TestHeadTrackerBalancer_HeartbeatOutlivesFirstTracker(t *testing.T) {
	ssr := &countingHeartbeatSSR{}
	ssr.n.Store(3)
	b := &headTrackerBalancer{ssr: ssr, trackers: map[*headTracker]struct{}{}, ctxs: map[*headTracker]context.Context{}}
	b.replicas.Store(1)

	ctx1, cancel1 := context.WithCancel(t.Context())
	ctx2, cancel2 := context.WithCancel(t.Context())
	t1, t2 := &headTracker{}, &headTracker{}
	b.register(ctx1, t1)
	b.register(ctx2, t2)
	require.Eventually(t, func() bool { return b.replicas.Load() == 3 }, time.Second, time.Millisecond)

	// The first tracker stops: the loop must continue on t2's context.
	cancel1()
	b.unregister(t1)
	ssr.n.Store(4)
	require.Eventually(t, func() bool { return b.replicas.Load() == 4 }, 2*time.Second, time.Millisecond,
		"heartbeat continues after the first tracker stopped")

	// Every tracker stops: the loop ends; a new register restarts it.
	cancel2()
	b.unregister(t2)
	require.Eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return !b.running }, 2*time.Second, time.Millisecond)
	ssr.n.Store(5)
	b.register(t.Context(), &headTracker{})
	require.Eventually(t, func() bool { return b.replicas.Load() == 5 }, time.Second, time.Millisecond)
}
