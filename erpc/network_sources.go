package erpc

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsupstream"
	"github.com/erpc/erpc/indexer/mempool"
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
	// long, while heads carry transactions, is subscribed again; each
	// further silence of the topic on that source waits twice as long, up
	// to pendingSilentMax, until it sends a message.
	pendingSilentAfter = util.NewDuration(60 * time.Second)
	pendingSilentMax   = util.NewDuration(30 * time.Minute)
	// pendingOpenWait bounds how long a subscribe that opens a variant
	// waits for its push sources' first answers.
	pendingOpenWait = util.NewDuration(10 * time.Second)
)

// sourceReconciler decides which topics each source of one network feeds:
//
//	push u: newHeads                    always, from network init
//	poll u: newHeads                    while demand has newHeads, admitted(u) and u serves latest
//	push u: newPendingTransactions:<v>  while demand has it and admitted(u)
//
// A push source whose newHeads the upstream refused as unsupported is polled
// under the poll rule until a subscribe succeeds. An upstream that refused
// latest as unsupported is no poll source until its give-up expires (#240).
// It runs on one goroutine, from network init whether or not a WebSocket or
// gRPC server runs, and wakes on demand changes, hash-voter changes,
// subscribe answers, and a timer at W. admitted(u) is topic admission
// (spec §6.2); each pass computes it first.
type sourceReconciler struct {
	nw *Network

	startOnce sync.Once
	wakeCh    chan struct{}
	syncCh    chan chan struct{}
	// demandChangedAt is when the network's demand last gained or lost a
	// topic, in unix nanoseconds.
	demandChangedAt atomic.Int64
	// pushes are the push sources by upstream id, as the last pass set them
	// up; read without a lock by routing and intake (pushSource).
	pushes atomic.Pointer[map[string]*wsupstream.Adapter]

	// Owned by the run goroutine.
	sources    map[string]*source
	demandSeen time.Time
	// watched holds the pollers whose head counter wakes the network's
	// tracker; a poller's callbacks cannot be removed, so an upstream that
	// leaves and comes back is not watched twice.
	watched map[*evm.EvmStatePoller]struct{}

	mu sync.Mutex
	// heads: some upstream is a subscription source; push: some push
	// source can carry pending transactions.
	heads, push bool
	// pending holds each pending-transaction topic that is held, demanded,
	// idle within sourceIdleStop, or still set on a source.
	pending map[indexer.Topic]*pendingTopic
}

// pendingTopic is one pending-transaction topic: who holds it, when it was
// last wanted, and the push sources it is set on with their answers.
type pendingTopic struct {
	// holders are the subscribes holding the topic; its variant's ring on
	// the mempool feed is open while there are some.
	holders int
	// open is the verdict every holder since the first waits for; nil
	// while there are no holders.
	open    *pendingOpen
	seen    time.Time
	on      map[string]*pendingOn
	answers int
	// changed is closed and replaced on each answer and each change of on.
	changed chan struct{}
}

// pendingOpen is one open of a topic: decided by the first holder to see
// the sources' answers settle, or open at deadline.
type pendingOpen struct {
	deadline time.Time
	decided  bool
	refusal  error
}

// pendingOn is the topic on one push source since it was set: its first
// subscribe answer, and when it was last subscribed again for silence.
type pendingOn struct {
	since time.Time
	// silentAfter is the silence that subscribes it again; 0 is
	// pendingSilentAfter.
	silentAfter time.Duration
	answered    bool
	err         error
	order       int
}

func (st *pendingTopic) notifyLocked() {
	close(st.changed)
	st.changed = make(chan struct{})
}

// activeLocked: st counts against mempool.MaxVariants while it is held or
// set on sources within sourceIdleStop of when it was last wanted.
func (st *pendingTopic) activeLocked(now time.Time, idle time.Duration) bool {
	return st.holders > 0 || (!st.seen.IsZero() && now.Sub(st.seen) < idle)
}

// verdictLocked decides an open from the answers of every source the topic
// is set on: open once one answered other than with a refusal; the first
// refusal once every one refused; not done while none accepted and some
// has not answered.
func (st *pendingTopic) verdictLocked() (refusal error, done bool) {
	first, waiting := -1, false
	for _, on := range st.on {
		switch {
		case !on.answered:
			waiting = true
		case !pendingRefusal(on.err):
			return nil, true
		case first < 0 || on.order < first:
			first, refusal = on.order, on.err
		}
	}
	if waiting {
		return nil, false
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
		watched: make(map[*evm.EvmStatePoller]struct{}),
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

// openPending holds variant's topic until release: its ring on the mempool
// feed opens with the first holder and closes with the last. A topic not
// counted against mempool.MaxVariants takes the place of the idle topic
// wanted longest ago, or is refused with ErrSubscriptionLimitExceeded when
// none is idle. Every subscribe from the first holder on waits for the
// same verdict, up to pendingOpenWait from the first: the first refusal
// when every push source a pass set the topic on refused it (spec §11).
func (r *sourceReconciler) openPending(ctx context.Context, variant string) (release func(), err error) {
	t := indexer.PendingTxTopic(variant)
	now, idle := time.Now(), sourceIdleStop.Load()
	r.mu.Lock()
	st := r.pending[t]
	if st == nil || !st.activeLocked(now, idle) {
		if err := r.admitLocked(now, idle); err != nil {
			r.mu.Unlock()
			return nil, err
		}
	}
	if st == nil {
		st = &pendingTopic{on: make(map[string]*pendingOn), changed: make(chan struct{})}
		r.pending[t] = st
	}
	if st.holders == 0 {
		if err := r.nw.mempool.Open(variant); err != nil {
			r.mu.Unlock()
			return nil, common.NewErrPendingVariantLimitExceeded(mempool.MaxVariants)
		}
		st.open = &pendingOpen{deadline: now.Add(pendingOpenWait.Load())}
	}
	st.holders++
	st.seen = now
	o := st.open
	r.mu.Unlock()

	var once sync.Once
	release = func() { once.Do(func() { r.releasePending(variant, st) }) }
	if err := r.awaitOpen(ctx, st, o); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// admitLocked makes room for one more active topic: at mempool.MaxVariants
// it stops the idle one wanted longest ago, so the next pass releases it
// on its sources.
func (r *sourceReconciler) admitLocked(now time.Time, idle time.Duration) error {
	active := 0
	var oldest *pendingTopic
	for _, st := range r.pending {
		if !st.activeLocked(now, idle) {
			continue
		}
		active++
		if st.holders == 0 && (oldest == nil || st.seen.Before(oldest.seen)) {
			oldest = st
		}
	}
	if active < mempool.MaxVariants {
		return nil
	}
	if oldest == nil {
		return common.NewErrPendingVariantLimitExceeded(mempool.MaxVariants)
	}
	oldest.seen = time.Time{}
	r.wake()
	return nil
}

// releasePending drops one holder of st. The last closes the ring; the
// topic then idles from now, or stops at the next pass if refused.
func (r *sourceReconciler) releasePending(variant string, st *pendingTopic) {
	r.mu.Lock()
	st.holders--
	last := st.holders == 0
	if last {
		if st.open.decided && st.open.refusal != nil {
			st.seen = time.Time{}
		} else {
			st.seen = time.Now()
		}
		st.open = nil
		r.nw.mempool.Close(variant)
	}
	r.mu.Unlock()
	if last {
		r.wake()
	}
}

// awaitOpen waits for o's verdict. A pass that started after the call sets
// the topic on the push sources first, so the answers are theirs; ctx
// ending first fails only this subscribe.
func (r *sourceReconciler) awaitOpen(ctx context.Context, st *pendingTopic, o *pendingOpen) error {
	wctx, cancel := context.WithDeadline(ctx, o.deadline)
	defer cancel()
	synced := r.sync(wctx) == nil
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.mu.Lock()
		if !o.decided {
			switch refusal, done := st.verdictLocked(); {
			case synced && done:
				o.decided, o.refusal = true, refusal
			case wctx.Err() != nil && ctx.Err() == nil:
				// The deadline, shared by every holder: the topic opens.
				o.decided = true
			}
		}
		decided, refusal, changed := o.decided, o.refusal, st.changed
		r.mu.Unlock()
		if decided {
			return refusal
		}
		select {
		case <-wctx.Done():
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
	next = r.nw.chainTracker.W()
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
	r.nw.intake.admit(now, r.nw.chainTracker.W())

	heads, push := false, false
	pushes := make(map[string]*wsupstream.Adapter)
	for _, s := range r.sources {
		heads = heads || s.isSource
		if s.push != nil && s.isSource {
			push = true
			pushes[s.id] = s.push
			r.setPush(s, pending, now, txHeadAt)
		}
		if s.poll == nil {
			continue
		}
		pollOn := polling && (s.push == nil || s.push.HeadsRefused()) && !s.poll.LatestUnsupported() && r.nw.intake.admits(s.id)
		if pollOn != s.pollOn {
			s.poll.Set(topicsIf(pollOn))
			s.pollOn = pollOn
			r.nw.logger.Debug().Str("upstreamId", s.id).Bool("polling", pollOn).Msg("poll source changed")
		}
	}
	if old := r.pushes.Load(); old == nil || !maps.Equal(*old, pushes) {
		r.pushes.Store(&pushes)
	}
	r.mu.Lock()
	r.heads, r.push = heads, push
	r.mu.Unlock()
	return next
}

// pendingWanted marks the demanded and held pending topics seen now and
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
		if st.holders > 0 {
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
// while s is admitted. While s is connected, each pending topic it
// carries that stayed silent for its silentAfter, while heads carried
// transactions, is subscribed again, and its next silentAfter doubles.
func (r *sourceReconciler) setPush(s *source, wanted []indexer.Topic, now, txHeadAt time.Time) {
	topics := []indexer.Topic{indexer.TopicNewHeads}
	if len(wanted) > 0 && r.nw.intake.admits(s.id) {
		topics = append(topics, wanted...)
	}
	type activity struct{ heard, subscribed time.Time }
	var acts []activity
	if s.push.Connected() {
		acts = make([]activity, len(topics))
		for i, t := range topics[1:] {
			acts[i+1].heard, acts[i+1].subscribed = s.push.Activity(t)
		}
	}
	minSilent, maxSilent := pendingSilentAfter.Load(), pendingSilentMax.Load()
	var silent []indexer.Topic
	r.mu.Lock()
	for t, st := range r.pending {
		on := st.on[s.id]
		i := slices.Index(topics, t)
		switch {
		case i < 0:
			if on != nil {
				delete(st.on, s.id)
				st.notifyLocked()
			}
		case on == nil:
			// Recorded before Set, so its first answer finds it.
			st.on[s.id] = &pendingOn{since: now}
			st.notifyLocked()
		case acts != nil:
			a := acts[i]
			if a.heard.After(on.since) {
				on.silentAfter = 0
			}
			after := max(on.silentAfter, minSilent)
			if base := later(later(on.since, a.heard), a.subscribed); now.Sub(base) >= after && txHeadAt.After(base) {
				silent = append(silent, t)
				on.since, on.silentAfter = now, min(2*after, maxSilent)
			}
		}
	}
	r.mu.Unlock()

	if !slices.Equal(topics, s.pushTopics) {
		s.push.Set(topics)
	}
	s.pushTopics = topics
	for _, t := range silent {
		r.nw.logger.Warn().Str("upstreamId", s.id).Str("topic", string(t)).
			Msg("pending-transaction subscription silent while heads carry transactions, subscribing again")
		s.push.Resubscribe(t)
	}
}

func topicsIf(newHeads bool) []indexer.Topic {
	if newHeads {
		return []indexer.Topic{indexer.TopicNewHeads}
	}
	return nil
}

// pushSource reports whether upstreamId is a push source (spec §6.1): a
// WebSocket subscription source whose newHeads the upstream did not refuse,
// so its requests ride the socket its pushes come on. A push source whose
// socket closed is one still: its expired vote moves it last.
func (r *sourceReconciler) pushSource(upstreamId string) bool {
	if r == nil {
		return false
	}
	ps := r.pushes.Load()
	if ps == nil {
		return false
	}
	a := (*ps)[upstreamId]
	return a != nil && !a.HeadsRefused()
}

// newSource sets up u's transports: its WebSocket client as a push source
// and its state poller as a poll source. Every poller's latest headers go
// through intake as u's hash votes (spec §6.3), source or not.
func (r *sourceReconciler) newSource(u *upstream.Upstream) *source {
	cfg := u.Config()
	s := &source{id: u.Id(), isSource: cfg.IsSubscriptionSource()}
	// Every move of its head counter, by a poll, a push or another instance,
	// may move the network's latest block, which caps every send (spec §6.5).
	p, ok := u.EvmStatePoller().(*evm.EvmStatePoller)
	if ok && !p.IsObjectNull() {
		if _, seen := r.watched[p]; !seen {
			r.watched[p] = struct{}{}
			p.OnLatestBlock(func(int64) { evm.LatestChanged(r.nw) })
		}
		p.BindIntake(r.nw.intake)
		if s.isSource {
			s.poll = p
		}
	}
	if util.IsWsEndpoint(cfg.Endpoint) {
		opts := wsupstream.Options{
			NetworkLabel: r.nw.Label(),
			HashAge: func() time.Duration {
				v, _ := r.nw.intake.hashVote(u)
				return v.Age
			},
			Disconnected: func() { r.nw.intake.disconnected(u.Id()) },
		}
		if a := wsupstream.New(u, indexerNetworkKey(r.nw), r.nw.intake, r.nw.logger, opts); a != nil {
			s.push = a
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
	r.nw.intake.forget(s.id)
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

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
