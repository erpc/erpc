package erpc

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
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

// Source timing (spec §5.2, §11); vars so tests can shorten them.
var (
	// sourceIdleStop is how long polls and pending-transaction
	// subscriptions keep running after the last tap needing them closed.
	sourceIdleStop = util.NewDuration(60 * time.Second)
	// pendingSilentAfter: a pending-transaction topic with no message this
	// long, while heads carry transactions, is subscribed again.
	pendingSilentAfter = util.NewDuration(60 * time.Second)
	// pendingOpenWait bounds how long a subscribe that opens a variant
	// waits for its push sources' first answers.
	pendingOpenWait = util.NewDuration(10 * time.Second)
)

// sourceReconciler decides which topics each source of one network feeds:
//
//	push u: newHeads                    always, from network init
//	poll u: newHeads                    while demand has newHeads and authoritative(u)
//	push u: newPendingTransactions:<v>  while demand has it and authoritative(u)
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
	// heads: some upstream is a subscription source; push: some push
	// source can carry pending transactions.
	heads, push bool
	// pending holds each pending-transaction topic that is demanded, being
	// opened, idle within sourceIdleStop, or still set on a source.
	pending map[indexer.Topic]*pendingTopic
}

// pendingTopic is one pending-transaction topic: who opens it, when it was
// last wanted, and the push sources it is set on with their answers.
type pendingTopic struct {
	opening int
	seen    time.Time
	on      map[string]*pendingOn
	answers int
	// changed is closed and replaced on each answer and each change of on.
	changed chan struct{}
}

// pendingOn is the topic on one push source since it was set (or set
// again): its last message and its first subscribe answer.
type pendingOn struct {
	since    time.Time
	heard    time.Time
	answered bool
	err      error
	order    int
}

func (st *pendingTopic) notifyLocked() {
	close(st.changed)
	st.changed = make(chan struct{})
}

// verdictLocked decides an opening subscribe: done once every source the
// topic is set on answered or one answered other than with a refusal; the
// refusal is the first one when every source refused.
func (st *pendingTopic) verdictLocked() (refusal error, done bool) {
	first := -1
	for _, on := range st.on {
		if !on.answered {
			return nil, false
		}
		if !pendingRefusal(on.err) {
			return nil, true
		}
		if first < 0 || on.order < first {
			first, refusal = on.order, on.err
		}
	}
	return refusal, true
}

// pendingRefusal: the upstream itself answered the subscribe with an error
// other than a capacity error. Capacity, connection errors and timeouts
// leave the subscription open while the source retries.
func pendingRefusal(err error) bool {
	if err == nil || common.IsCapacityIssue(err) {
		return false
	}
	var jre *common.ErrJsonRpcExceptionInternal
	return errors.As(err, &jre)
}

// source is one upstream's transports: push over its WebSocket client, poll
// through its state poller.
type source struct {
	id       string
	isSource bool
	push     *wsupstream.Adapter
	poll     *evm.EvmStatePoller
	// pushTopics is what push was last Set to; nil before.
	pushTopics []indexer.Topic
	pollOn     bool
}

func newSourceReconciler(nw *Network) *sourceReconciler {
	return &sourceReconciler{
		nw:      nw,
		wakeCh:  make(chan struct{}, 1),
		syncCh:  make(chan chan struct{}),
		sources: make(map[string]*source),
		pending: make(map[indexer.Topic]*pendingTopic),
	}
}

// start runs the reconciler until ctx ends.
func (r *sourceReconciler) start(ctx context.Context) {
	if r == nil {
		return
	}
	r.startOnce.Do(func() {
		r.nw.bg.Go(func() { r.run(ctx) })
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

// subscribed takes a push source's subscribe answer for t. For a
// pending-transaction topic it keeps the source's first answer since the
// topic was set on it. For newHeads only an unsupported answer, and its
// later success, change a topic; the push source holds which one it is.
func (r *sourceReconciler) subscribed(upstreamId string, t indexer.Topic, err error) {
	if r == nil {
		return
	}
	if _, ok := t.PendingTxVariant(); ok {
		r.mu.Lock()
		if st := r.pending[t]; st != nil {
			if on := st.on[upstreamId]; on != nil && !on.answered {
				on.answered, on.err, on.order = true, err, st.answers
				st.answers++
				st.notifyLocked()
			}
		}
		r.mu.Unlock()
		return
	}
	if t != indexer.TopicNewHeads {
		return
	}
	if err == nil || common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) {
		r.wake()
	}
}

// heard notes a message of t from upstreamId, before any intake check.
func (r *sourceReconciler) heard(upstreamId string, t indexer.Topic) {
	if r == nil {
		return
	}
	now := time.Now()
	r.mu.Lock()
	if st := r.pending[t]; st != nil {
		if on := st.on[upstreamId]; on != nil {
			on.heard = now
		}
	}
	r.mu.Unlock()
}

// awaitPending opens t: it holds t as demand while it waits, up to
// pendingOpenWait, for the first answers of the push sources a pass set t
// on. It returns the first refusal when every one of them refused (spec
// §11), else nil. A refused topic stops at the next pass unless a tap
// demands it.
func (r *sourceReconciler) awaitPending(ctx context.Context, t indexer.Topic) (refusal error) {
	r.mu.Lock()
	st := r.pending[t]
	if st == nil {
		st = &pendingTopic{on: make(map[string]*pendingOn), changed: make(chan struct{})}
		r.pending[t] = st
	}
	st.opening++
	st.seen = time.Now()
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		st.opening--
		if refusal != nil {
			st.seen = time.Time{}
		}
		r.mu.Unlock()
		if refusal != nil {
			r.wake()
		}
	}()

	wctx, cancel := context.WithTimeout(ctx, pendingOpenWait.Load())
	defer cancel()
	if r.sync(wctx) != nil {
		return nil
	}
	for {
		r.mu.Lock()
		err, done := st.verdictLocked()
		changed := st.changed
		r.mu.Unlock()
		if done {
			return err
		}
		select {
		case <-wctx.Done():
			return nil
		case <-changed:
		}
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

func (r *sourceReconciler) run(ctx context.Context) {
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
		next := r.pass(ctx)
		for _, d := range done {
			close(d)
		}
		done = done[:0]
		timer.Reset(next)
	}
}

// pass makes every source feed what the rules say now and returns when the
// next pass is due without a wake.
func (r *sourceReconciler) pass(ctx context.Context) (next time.Duration) {
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
			r.sources[u.Id()] = r.newSource(u)
		}
	}
	for id, s := range r.sources {
		if _, ok := present[id]; !ok {
			r.dropSource(s)
		}
	}

	demand := r.nw.taps.Demand(r.nw.Label())
	demanded := slices.Contains(demand, indexer.TopicNewHeads)
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
	pending, left := r.pendingWanted(demand, now)
	if left > 0 {
		next = min(next, left)
	}
	txHeadAt := r.nw.chainTracker.TxHeadAt()

	heads, push := false, false
	for _, s := range r.sources {
		heads = heads || s.isSource
		if s.push != nil && s.isSource {
			push = true
			r.setPush(s, pending, now, txHeadAt)
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

// pendingWanted marks the demanded and opening pending topics seen now and
// returns, sorted, every topic seen within sourceIdleStop, with the time
// until the next idle one stops (0 if none). A topic no longer wanted and
// set on no source is forgotten.
func (r *sourceReconciler) pendingWanted(demand []indexer.Topic, now time.Time) (wanted []indexer.Topic, left time.Duration) {
	idle := sourceIdleStop.Load()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range demand {
		if _, ok := t.PendingTxVariant(); !ok {
			continue
		}
		st := r.pending[t]
		if st == nil {
			st = &pendingTopic{on: make(map[string]*pendingOn), changed: make(chan struct{})}
			r.pending[t] = st
		}
		st.seen = now
	}
	for t, st := range r.pending {
		if st.opening > 0 {
			st.seen = now
		}
		l := idle - now.Sub(st.seen)
		switch {
		case l <= 0 && len(st.on) == 0:
			delete(r.pending, t)
		case l <= 0:
		default:
			wanted = append(wanted, t)
			if l < idle && (left == 0 || l < left) {
				left = l
			}
		}
	}
	slices.Sort(wanted)
	return wanted, left
}

// setPush sets push source s to newHeads plus the wanted pending topics
// while s is authoritative, and subscribes again each pending topic that
// stayed silent for pendingSilentAfter while heads carried transactions.
func (r *sourceReconciler) setPush(s *source, wanted []indexer.Topic, now, txHeadAt time.Time) {
	topics := []indexer.Topic{indexer.TopicNewHeads}
	if len(wanted) > 0 && r.nw.intake.authoritative(s.id) {
		topics = append(topics, wanted...)
	}
	silentAfter := pendingSilentAfter.Load()
	var silent []indexer.Topic
	r.mu.Lock()
	for t, st := range r.pending {
		on := st.on[s.id]
		switch {
		case !slices.Contains(topics, t):
			if on != nil {
				delete(st.on, s.id)
				st.notifyLocked()
			}
		case on == nil:
			// Recorded before Set, so its first answer finds it.
			st.on[s.id] = &pendingOn{since: now}
			st.notifyLocked()
		default:
			if base := later(on.since, on.heard); now.Sub(base) >= silentAfter && txHeadAt.After(base) {
				silent = append(silent, t)
				*on = pendingOn{since: now}
				st.notifyLocked()
			}
		}
	}
	r.mu.Unlock()

	if len(silent) > 0 {
		s.push.Set(slices.DeleteFunc(slices.Clone(topics), func(t indexer.Topic) bool { return slices.Contains(silent, t) }))
		for _, t := range silent {
			telemetry.CounterHandle(telemetry.MetricSourceResubscribeTotal, r.nw.projectId, r.nw.Label(), s.id, string(t)).Inc()
			r.nw.logger.Warn().Str("upstreamId", s.id).Str("topic", string(t)).
				Msg("pending-transaction subscription silent while heads carry transactions, subscribing again")
		}
		s.push.Set(topics)
	} else if !slices.Equal(topics, s.pushTopics) {
		s.push.Set(topics)
	}
	s.pushTopics = topics
}

func topicsIf(newHeads bool) []indexer.Topic {
	if newHeads {
		return []indexer.Topic{indexer.TopicNewHeads}
	}
	return nil
}

// newSource sets up u's transports: its WebSocket client as a push source
// and its state poller as a poll source.
func (r *sourceReconciler) newSource(u *upstream.Upstream) *source {
	cfg := u.Config()
	s := &source{id: u.Id(), isSource: cfg.IsSubscriptionSource()}
	if util.IsWsEndpoint(cfg.Endpoint) {
		opts := wsupstream.Options{NetworkLabel: r.nw.Label()}
		if a := wsupstream.New(u, indexerNetworkKey(r.nw), r.nw.intake, r.nw.logger, opts); a != nil {
			s.push = a
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

// dropSource stops a source whose upstream left the network; it stays
// known until it stopped, so a panic retries it.
func (r *sourceReconciler) dropSource(s *source) {
	r.stop(s)
	r.mu.Lock()
	for _, st := range r.pending {
		if st.on[s.id] != nil {
			delete(st.on, s.id)
			st.notifyLocked()
		}
	}
	r.mu.Unlock()
	delete(r.sources, s.id)
	r.nw.logger.Debug().Str("upstreamId", s.id).Msg("subscription source left the network")
}

func (r *sourceReconciler) stop(s *source) {
	if s.pushTopics != nil {
		s.push.Set(nil)
		s.pushTopics = nil
	}
	if s.pollOn {
		s.poll.Set(nil)
		s.pollOn = false
	}
}

// stopAll stops every poll and push subscription when the network ends.
func (r *sourceReconciler) stopAll() {
	for _, s := range r.sources {
		r.stop(s)
	}
}

// requireSubscriptionSource fails with ErrNoSubscriptionSource when nothing
// on the network can feed subType, after the sources caught up with the
// network's upstreams. logs need heads and a feed that serves receipts or
// their fallback (spec §10); pending transactions need a push source (§11).
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
