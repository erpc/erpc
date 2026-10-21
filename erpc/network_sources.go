package erpc

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsupstream"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
)

// sourceIdleStop is how long polls keep running after the last newHeads tap
// closed; a var so tests can shorten it.
var sourceIdleStop = util.NewDuration(60 * time.Second)

// sourceReconciler decides which topics each source of one network feeds:
//
//	push u: newHeads   always, from network init
//	poll u: newHeads   while demand has newHeads and authoritative(u)
//
// A push source whose newHeads the upstream refused as unsupported is polled
// under the poll rule until a subscribe succeeds. It runs on one goroutine,
// from network init whether or not a WebSocket or gRPC server runs, and
// wakes on demand changes, liveness and authority changes, subscribe
// answers, and a timer at the live bound.
type sourceReconciler struct {
	nw *Network

	startOnce sync.Once
	wakeCh    chan struct{}
	syncCh    chan chan struct{}
	// demandChangedAt is when the network's demand last gained or lost a
	// topic, in unix nanoseconds.
	demandChangedAt atomic.Int64

	// Owned by the run goroutine.
	sources    map[string]*source
	demandSeen time.Time

	mu sync.Mutex
	// heads: some upstream is a subscription source; push: some upstream
	// carries logs and pending transactions until they move to sources.
	heads, push bool
}

// source is one upstream's transports: push over its WebSocket client, poll
// through its state poller.
type source struct {
	id       string
	isSource bool
	push     *wsupstream.Adapter
	poll     *evm.EvmStatePoller
	pushOn   bool
	pollOn   bool
}

func newSourceReconciler(nw *Network) *sourceReconciler {
	return &sourceReconciler{
		nw:      nw,
		wakeCh:  make(chan struct{}, 1),
		syncCh:  make(chan chan struct{}),
		sources: make(map[string]*source),
	}
}

// start runs the reconciler until ctx ends. Until wss logs and pending
// transactions move to sources, each push source is also an ingress of idx.
func (r *sourceReconciler) start(ctx context.Context, idx *indexer.Indexer) {
	if r == nil {
		return
	}
	r.startOnce.Do(func() {
		if idx != nil {
			key := indexerNetworkKey(r.nw)
			idx.RegisterNetwork(&networkHandle{nw: r.nw})
			idx.RegisterNetworkSelector(key, &subIngressSelector{nw: r.nw})
		}
		r.nw.bg.Go(func() { r.run(ctx, idx) })
	})
}

// wake asks for a pass; it never blocks.
func (r *sourceReconciler) wake() {
	if r == nil {
		return
	}
	select {
	case r.wakeCh <- struct{}{}:
	default:
	}
}

// demandChanged is the network's taps callback; it never blocks.
func (r *sourceReconciler) demandChanged() {
	if r == nil {
		return
	}
	r.demandChangedAt.Store(time.Now().UnixNano())
	r.wake()
}

// subscribed takes a push source's subscribe answer for t. Only an
// unsupported newHeads, and its later success, change a topic; the push
// source holds which one it is.
func (r *sourceReconciler) subscribed(t indexer.Topic, err error) {
	if r == nil || t != indexer.TopicNewHeads {
		return
	}
	if err == nil || common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) {
		r.wake()
	}
}

// sync waits for a pass that started after the call.
func (r *sourceReconciler) sync(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case r.syncCh <- done:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *sourceReconciler) run(ctx context.Context, idx *indexer.Indexer) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	var done []chan struct{}
	for {
		select {
		case <-ctx.Done():
			r.stopAll()
			return
		case <-r.wakeCh:
		case <-timer.C:
		case d := <-r.syncCh:
			done = append(done, d)
		}
		// Every caller queued by now shares this pass.
		for drained := false; !drained; {
			select {
			case d := <-r.syncCh:
				done = append(done, d)
			default:
				drained = true
			}
		}
		next := r.pass(ctx, idx)
		for _, d := range done {
			close(d)
		}
		done = done[:0]
		timer.Reset(next)
	}
}

// pass makes every source feed what the rules say now and returns when the
// next pass is due without a wake.
func (r *sourceReconciler) pass(ctx context.Context, idx *indexer.Indexer) (next time.Duration) {
	next = r.nw.chainTracker.LiveBound()
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"source-reconciler",
				fmt.Sprintf("project:%s network:%s", r.nw.projectId, r.nw.networkId),
				common.ErrorFingerprint(rec),
			).Inc()
			r.nw.logger.Error().Interface("panic", rec).Str("stack", string(debug.Stack())).
				Msg("unexpected panic while reconciling subscription sources")
		}
	}()

	now := time.Now()
	ups := r.nw.upstreamsRegistry.GetNetworkUpstreams(ctx, r.nw.networkId)
	present := make(map[string]struct{}, len(ups))
	for _, u := range ups {
		present[u.Id()] = struct{}{}
		if r.sources[u.Id()] == nil {
			r.sources[u.Id()] = r.newSource(ctx, idx, u)
		}
	}
	for id, s := range r.sources {
		if _, ok := present[id]; !ok {
			r.dropSource(ctx, idx, s)
		}
	}

	demanded := r.nw.EvmHeadsDemanded()
	if demanded {
		r.demandSeen = now
	} else if at := r.demandChangedAt.Load(); at != 0 && time.Unix(0, at).After(r.demandSeen) {
		// Demand was there until it last changed.
		r.demandSeen = time.Unix(0, at)
	}
	polling := demanded
	if !demanded && !r.demandSeen.IsZero() {
		if left := sourceIdleStop.Load() - now.Sub(r.demandSeen); left > 0 {
			polling = true
			next = min(next, left)
		}
	}

	heads, push := false, false
	for _, s := range r.sources {
		heads = heads || s.isSource
		push = push || s.push != nil
		if s.push != nil && s.isSource && !s.pushOn {
			s.push.Set([]indexer.Topic{indexer.TopicNewHeads})
			s.pushOn = true
		}
		if s.poll == nil {
			continue
		}
		pollOn := polling && (s.push == nil || s.push.HeadsRefused()) && r.nw.intake.authoritative(s.id)
		if pollOn != s.pollOn {
			s.poll.Set(topicsIf(pollOn))
			s.pollOn = pollOn
			r.nw.logger.Debug().Str("upstreamId", s.id).Bool("polling", pollOn).Msg("poll source changed")
		}
	}
	r.mu.Lock()
	r.heads, r.push = heads, push
	r.mu.Unlock()
	return next
}

func topicsIf(newHeads bool) []indexer.Topic {
	if newHeads {
		return []indexer.Topic{indexer.TopicNewHeads}
	}
	return nil
}

// newSource sets up u's transports: its WebSocket client as a push source
// and indexer ingress, and its state poller as a poll source.
func (r *sourceReconciler) newSource(ctx context.Context, idx *indexer.Indexer, u *upstream.Upstream) *source {
	cfg := u.Config()
	s := &source{id: u.Id(), isSource: cfg.IsSubscriptionSource()}
	if util.IsWsEndpoint(cfg.Endpoint) {
		opts := wsupstream.Options{NetworkLabel: r.nw.Label()}
		key := indexerNetworkKey(r.nw)
		if a := wsupstream.New(u, key, r.nw.intake, r.nw.logger, opts); a != nil {
			s.push = a
			if idx != nil {
				if err := idx.AddIngress(ctx, key, a); err != nil {
					r.nw.logger.Warn().Err(err).Str("upstreamId", u.Id()).Msg("failed to register upstream ingress with indexer")
				}
			}
		}
	}
	if s.isSource {
		if p, ok := u.EvmStatePoller().(*evm.EvmStatePoller); ok && !p.IsObjectNull() {
			p.BindIntake(r.nw.intake, r.nw.chainTracker.BlockTime)
			s.poll = p
		}
	}
	return s
}

// dropSource stops a source whose upstream left the network and removes
// its ingress; it stays known until both stopped, so a panic retries it.
func (r *sourceReconciler) dropSource(ctx context.Context, idx *indexer.Indexer, s *source) {
	if s.pushOn {
		s.push.Set(nil)
	}
	if s.pollOn {
		s.poll.Set(nil)
	}
	if s.push != nil && idx != nil {
		idx.RemoveIngress(ctx, indexerNetworkKey(r.nw), s.push.Name())
	}
	delete(r.sources, s.id)
	r.nw.logger.Debug().Str("upstreamId", s.id).Msg("subscription source left the network")
}

// stopAll stops every poll and push subscription when the network ends.
func (r *sourceReconciler) stopAll() {
	for _, s := range r.sources {
		if s.pushOn {
			s.push.Set(nil)
		}
		if s.pollOn {
			s.poll.Set(nil)
		}
	}
}

// requireSubscriptionSource fails with ErrNoSubscriptionSource when nothing
// on the network can feed subType, after the sources caught up with the
// network's upstreams. logs need heads and a feed that serves receipts or
// their fallback (spec §10).
func (n *Network) requireSubscriptionSource(ctx context.Context, subType string) error {
	if n.sources == nil {
		return common.NewErrNoSubscriptionSource(n.networkId, subType)
	}
	if err := n.sources.sync(ctx); err != nil {
		return err
	}
	n.sources.mu.Lock()
	heads, push := n.sources.heads, n.sources.push
	n.sources.mu.Unlock()
	var ok bool
	switch subType {
	case indexer.SubTypeNewHeads:
		ok = heads
	case indexer.SubTypeLogs:
		ok = heads && n.chainTracker != nil && n.chainTracker.ServesLogs()
	case indexer.SubTypeNewPendingTransactions:
		ok = push
	default:
		return nil
	}
	if !ok {
		return common.NewErrNoSubscriptionSource(n.networkId, subType)
	}
	return nil
}
