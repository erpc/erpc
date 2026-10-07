package erpc

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/data"
)

// headTrackerBalancer spreads head-tracker leases across replicas. Without
// it the first replica to boot grabs every network's lease (the others only
// ever see held leases), and that one pod pays every network's poll, decode,
// integrity check and cache write: on lat-dev ~2 cores for 31 networks, and
// polls slowed enough to fall behind fast chains.
//
// Rule (a soft preference, never a correctness condition): a replica that
// already leads >= ceil(networks / replicas) networks does not try to acquire
// a FREE lease for a short while, giving a less loaded replica the first
// chance. It still takes the lease if nobody else has after that delay, so
// a network is never left without a leader because of balancing. A leader
// over its share hands over one lease at a time (releases it) only when some
// other replica is below its share, which the shared leader counts show.
type headTrackerBalancer struct {
	ssr data.SharedStateRegistry

	mu       sync.Mutex
	trackers map[*headTracker]struct{}

	replicas atomic.Int64
	started  atomic.Bool
}

var (
	balancersMu sync.Mutex
	balancers   = map[data.SharedStateRegistry]*headTrackerBalancer{}
)

// headTrackerBalancerFor returns the per-registry (= per-process, per-cluster)
// balancer.
func headTrackerBalancerFor(ssr data.SharedStateRegistry) *headTrackerBalancer {
	balancersMu.Lock()
	defer balancersMu.Unlock()
	b, ok := balancers[ssr]
	if !ok {
		b = &headTrackerBalancer{ssr: ssr, trackers: map[*headTracker]struct{}{}}
		b.replicas.Store(1)
		balancers[ssr] = b
	}
	return b
}

const headTrackerReplicaHeartbeat = 5 * time.Second

func (b *headTrackerBalancer) register(ctx context.Context, t *headTracker) {
	b.mu.Lock()
	b.trackers[t] = struct{}{}
	b.mu.Unlock()
	if b.started.Swap(true) {
		return
	}
	go func() {
		tk := time.NewTicker(headTrackerReplicaHeartbeat)
		defer tk.Stop()
		for {
			hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			n, err := b.ssr.HeartbeatReplicas(hctx, 3*headTrackerReplicaHeartbeat)
			cancel()
			if err == nil && n > 0 {
				b.replicas.Store(int64(n))
			}
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
		}
	}()
}

func (b *headTrackerBalancer) unregister(t *headTracker) {
	b.mu.Lock()
	delete(b.trackers, t)
	b.mu.Unlock()
}

// share is ceil(networks / replicas): the number of leases a replica may
// hold before it defers acquiring more.
func (b *headTrackerBalancer) share() int {
	b.mu.Lock()
	n := len(b.trackers)
	b.mu.Unlock()
	r := max(b.replicas.Load(), 1)
	return int(math.Ceil(float64(n) / float64(r)))
}

func (b *headTrackerBalancer) leading() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := 0
	for t := range b.trackers {
		if t.IsLeader() {
			c++
		}
	}
	return c
}

// shouldDefer reports whether this replica should wait before trying to
// acquire a free lease: it already leads at least its share.
func (b *headTrackerBalancer) shouldDefer() bool {
	if b == nil || b.replicas.Load() <= 1 {
		return false
	}
	return b.leading() >= b.share()
}

// overShare reports whether this replica leads MORE than its share, i.e. it
// should hand one lease over.
func (b *headTrackerBalancer) overShare() bool {
	if b == nil || b.replicas.Load() <= 1 {
		return false
	}
	return b.leading() > b.share()
}
