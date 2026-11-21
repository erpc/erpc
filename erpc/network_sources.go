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
	"github.com/erpc/erpc/architecture/evm/evmpubsub"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsupstream"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
)

// Source timing (spec §5.2, §11); vars so tests can shorten them.
var (
	// sourceIdleStop is how long a push topic keeps running after the
	// last tap needing it closed: an onDemand source's newHeads, every
	// relay topic.
	sourceIdleStop = util.NewDuration(60 * time.Second)
	// relayOpenWait bounds how long a subscribe that opens a relay topic
	// waits for its push sources' first answers.
	relayOpenWait = util.NewDuration(10 * time.Second)
)

// sourceReconciler decides which topics each source of one network feeds:
//
//	push u: newHeads                    always, or onDemand while heads are wanted
//	push u: <kind>:<variant>            while demand has it and admitted(u)
//
// Heads are wanted while a tap needs every block (pace 0: eth_subscribe
// newHeads or logs, a gRPC block stream) and sourceIdleStop after; a
// paced tap, the integrity follow tap, is fed by the block clock alone.
// Heads that no push brings come from each lane's block clock (spec §6.8),
// which fetches the voters' heads while taps demand them. It runs on one
// goroutine, from network init whether or not a WebSocket or gRPC server
// runs, and wakes on demand changes, hash-voter changes, subscribe
// answers, and a timer at W. admitted(u) is topic admission (spec §6.2);
// each pass computes it first.
type sourceReconciler struct {
	nw *Network

	startOnce sync.Once
	wakeCh    chan struct{}
	syncCh    chan chan struct{}
	// pushes are the push sources by upstream id, as the last pass set them
	// up; read without a lock by routing and intake (pushSource).
	pushes atomic.Pointer[map[string]*wsupstream.Adapter]

	// Owned by the run goroutine.
	sources map[string]*source
	mu      sync.Mutex
	// heads: some upstream is a subscription source or has a poller the
	// block clock fetches; push: some push source can carry relay
	// topics.
	heads, push bool
	// headsSeen is when a pass last saw heads wanted by a tap, or saw that
	// demand end; headsDemanded is whether the last pass saw it.
	headsSeen     time.Time
	headsDemanded bool
	// relays holds each relay topic that is held, demanded, idle within
	// sourceIdleStop, or still set on a source.
	relays map[indexer.Topic]*relayTopic
}

// relayTopic is one relay topic of kind: who holds it, when it was last
// wanted, and the push sources it is set on with their answers.
type relayTopic struct {
	kind string
	spec *common.RelaySpec
	// holders are the subscribes holding the topic; its ring on the relay
	// feed is open while there are some.
	holders int
	// open is the verdict every holder since the first waits for; nil
	// while there are no holders.
	open    *relayOpen
	seen    time.Time
	on      map[string]*relayOn
	answers int
	// changed is closed and replaced on each answer and each change of on.
	changed chan struct{}
}

// relayOpen is one open of a topic: decided by the first holder to see
// the sources' answers settle, or open at deadline.
type relayOpen struct {
	deadline time.Time
	decided  bool
	refusal  error
}

// relayOn is the topic on one push source since it was set: its first
// subscribe answer, and when it was last subscribed again for silence.
type relayOn struct {
	since time.Time
	// silentAfter is the silence that subscribes it again; 0 is the
	// kind's RelaySpec.SilentAfter.
	silentAfter time.Duration
	answered    bool
	err         error
	order       int
}

func (st *relayTopic) notifyLocked() {
	close(st.changed)
	st.changed = make(chan struct{})
}

// activeLocked: st counts against its kind's MaxVariants while it is held
// or set on sources within sourceIdleStop of when it was last wanted.
func (st *relayTopic) activeLocked(now time.Time, idle time.Duration) bool {
	return st.holders > 0 || (!st.seen.IsZero() && now.Sub(st.seen) < idle)
}

// verdictLocked decides an open from the answers of every source the topic
// is set on: open once one answered other than with a refusal; the first
// refusal once every one refused; not done while none accepted and some
// has not answered.
func (st *relayTopic) verdictLocked() (refusal error, done bool) {
	first, waiting := -1, false
	for _, on := range st.on {
		switch {
		case !on.answered:
			waiting = true
		case !relayRefusal(on.err):
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

// relayRefusal: the upstream itself answered the subscribe with an error
// other than a capacity error. Capacity, connection errors and timeouts
// leave the subscription open while the source retries.
func relayRefusal(err error) bool {
	if err == nil || common.IsCapacityIssue(err) {
		return false
	}
	var jre *common.ErrJsonRpcExceptionInternal
	return errors.As(err, &jre)
}

// source is one upstream's transports: push over its WebSocket client,
// when mode is not off; clocked: its state poller is bound to intake, so
// the block clock can fetch its heads.
type source struct {
	id      string
	mode    common.SubscriptionSourceMode
	clocked bool
	push    *wsupstream.Adapter
	// pushTopics is what push was last Set to; nil before.
	pushTopics []indexer.Topic
}

func newSourceReconciler(nw *Network) *sourceReconciler {
	return &sourceReconciler{
		nw:      nw,
		wakeCh:  make(chan struct{}, 1),
		syncCh:  make(chan chan struct{}),
		sources: make(map[string]*source),
		relays:  make(map[indexer.Topic]*relayTopic),
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
	r.wake()
}

// subscribed takes a push source's subscribe answer for t. For a relay
// topic it keeps the source's first answer since the topic was set on it.
// For newHeads only an unsupported answer, and its
// later success, change a topic; the push source holds which one it is.
func (r *sourceReconciler) subscribed(upstreamId string, t indexer.Topic, err error) {
	if r == nil {
		return
	}
	if _, _, ok := t.Relay(); ok {
		r.mu.Lock()
		if st := r.relays[t]; st != nil {
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

// openRelay holds relay topic t until release: its ring on the relay feed
// opens with the first holder and closes with the last. A topic not
// counted against its kind's MaxVariants takes the place of the kind's
// idle topic wanted longest ago, or is refused with
// ErrRelayVariantLimitExceeded when none is idle. Every subscribe from the
// first holder on waits for the same verdict, up to relayOpenWait from the
// first: the first refusal when every push source a pass set the topic on
// refused it (spec §12).
func (r *sourceReconciler) openRelay(ctx context.Context, t indexer.Topic) (release func(), err error) {
	kind, _, ok := t.Relay()
	spec := r.nw.relay.Spec(kind)
	if !ok || spec == nil {
		return nil, fmt.Errorf("%q is not a relay topic of the network", t)
	}
	now, idle := time.Now(), sourceIdleStop.Load()
	r.mu.Lock()
	st := r.relays[t]
	if st == nil || !st.activeLocked(now, idle) {
		if err := r.admitLocked(kind, spec, now, idle); err != nil {
			r.mu.Unlock()
			return nil, err
		}
	}
	if st == nil {
		st = newRelayTopic(kind, spec)
		r.relays[t] = st
	}
	if st.holders == 0 {
		if err := r.nw.relay.Open(t); err != nil {
			r.mu.Unlock()
			return nil, err
		}
		st.open = &relayOpen{deadline: now.Add(relayOpenWait.Load())}
	}
	st.holders++
	st.seen = now
	o := st.open
	r.mu.Unlock()

	var once sync.Once
	release = func() { once.Do(func() { r.releaseRelay(t, st) }) }
	if err := r.awaitOpen(ctx, st, o); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func newRelayTopic(kind string, spec *common.RelaySpec) *relayTopic {
	return &relayTopic{kind: kind, spec: spec, on: make(map[string]*relayOn), changed: make(chan struct{})}
}

// admitLocked makes room for one more active topic of kind: at the kind's
// MaxVariants it stops the kind's idle topic wanted longest ago, so the
// next pass releases it on its sources.
func (r *sourceReconciler) admitLocked(kind string, spec *common.RelaySpec, now time.Time, idle time.Duration) error {
	active := 0
	var oldest *relayTopic
	for _, st := range r.relays {
		if st.kind != kind || !st.activeLocked(now, idle) {
			continue
		}
		active++
		if st.holders == 0 && (oldest == nil || st.seen.Before(oldest.seen)) {
			oldest = st
		}
	}
	if active < spec.MaxVariants {
		return nil
	}
	if oldest == nil {
		return common.NewErrRelayVariantLimitExceeded(kind, spec.MaxVariants)
	}
	oldest.seen = time.Time{}
	r.wake()
	return nil
}

// releaseRelay drops one holder of st, topic t. The last closes the ring;
// the topic then idles from now, or stops at the next pass if refused.
func (r *sourceReconciler) releaseRelay(t indexer.Topic, st *relayTopic) {
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
		r.nw.relay.Close(t)
	}
	r.mu.Unlock()
	if last {
		r.wake()
	}
}

// awaitOpen waits for o's verdict. A pass that started after the call sets
// the topic on the push sources first, so the answers are theirs; ctx
// ending first fails only this subscribe.
func (r *sourceReconciler) awaitOpen(ctx context.Context, st *relayTopic, o *relayOpen) error {
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

// sourcePassStart, while set, runs as each source pass begins; tests hold
// it there for a slow pass.
var sourcePassStart atomic.Pointer[func()]

// pass makes every source feed what the rules say now and returns when the
// next pass is due without a wake.
func (r *sourceReconciler) pass(ctx context.Context) (next time.Duration) {
	if hold := sourcePassStart.Load(); hold != nil {
		(*hold)()
	}
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
	pace, demanded := r.nw.taps.Pace(r.nw.Label(), indexer.TopicNewHeads)
	wantHeads, relays, left := r.wanted(demand, demanded && pace == 0, now)
	if left > 0 {
		next = min(next, left)
	}
	txHeadAt := r.nw.chainTracker.TxHeadAt()
	if left := r.nw.intake.admit(now, r.nw.chainTracker.W()); left > 0 {
		next = min(next, left)
	}

	heads, push := false, false
	pushes := make(map[string]*wsupstream.Adapter)
	for _, s := range r.sources {
		isSource := s.mode != common.SubscriptionSourceOff
		heads = heads || isSource || s.clocked
		if s.push == nil || !isSource {
			continue
		}
		push = true
		r.setPush(s, wantHeads || s.mode == common.SubscriptionSourceAlways, relays, now, txHeadAt)
		if slices.Contains(s.pushTopics, indexer.TopicNewHeads) {
			pushes[s.id] = s.push
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

// wanted marks heads, when headsDemanded, and the demanded and held relay
// topics seen now. It returns whether heads were seen within
// sourceIdleStop, and, sorted, every relay topic that was, with the time
// until the next idle topic stops (0 if none). A relay topic no longer
// wanted and set on no source is forgotten.
func (r *sourceReconciler) wanted(demand []indexer.Topic, headsDemanded bool, now time.Time) (heads bool, wanted []indexer.Topic, left time.Duration) {
	idle := sourceIdleStop.Load()
	keep := func(seen time.Time) bool {
		l := idle - now.Sub(seen)
		if l > 0 && l < idle && (left == 0 || l < left) {
			left = l
		}
		return l > 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// The pass that sees the demand end runs as it ends (demandChanged).
	if headsDemanded || r.headsDemanded {
		r.headsSeen = now
	}
	r.headsDemanded = headsDemanded
	heads = keep(r.headsSeen)
	for _, t := range demand {
		kind, _, ok := t.Relay()
		if !ok {
			continue
		}
		st := r.relays[t]
		if st == nil {
			spec := r.nw.relay.Spec(kind)
			if spec == nil {
				continue
			}
			st = newRelayTopic(kind, spec)
			r.relays[t] = st
		}
		st.seen = now
	}
	for t, st := range r.relays {
		if st.holders > 0 {
			st.seen = now
		}
		switch {
		case keep(st.seen):
			wanted = append(wanted, t)
		case len(st.on) == 0:
			delete(r.relays, t)
		}
	}
	slices.Sort(wanted)
	return heads, wanted, left
}

// setPush sets push source s to newHeads, when heads, plus the wanted
// relay topics its kind's placement puts on s while s is admitted. While s
// is connected, each relay topic of a kind with a silence rule that s
// carries and that stayed silent for its silentAfter, while heads carried
// transactions, is subscribed again, and its next silentAfter doubles.
func (r *sourceReconciler) setPush(s *source, heads bool, wanted []indexer.Topic, now, txHeadAt time.Time) {
	var topics []indexer.Topic
	if heads {
		topics = append(topics, indexer.TopicNewHeads)
	}
	if len(wanted) > 0 && r.nw.intake.admits(s.id) {
		for _, t := range wanted {
			kind, _, _ := t.Relay()
			switch r.nw.relay.Spec(kind).Placement {
			case common.PlacementAll:
				topics = append(topics, t)
			}
		}
	}
	type activity struct{ heard, subscribed time.Time }
	var acts []activity
	if s.push.Connected() {
		acts = make([]activity, len(topics))
		for i, t := range topics {
			if t != indexer.TopicNewHeads {
				acts[i].heard, acts[i].subscribed = s.push.Activity(t)
			}
		}
	}
	var silent []indexer.Topic
	r.mu.Lock()
	for t, st := range r.relays {
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
			st.on[s.id] = &relayOn{since: now}
			st.notifyLocked()
		case acts != nil && st.spec.SilentAfter != nil:
			a := acts[i]
			if a.heard.After(on.since) {
				on.silentAfter = 0
			}
			after := max(on.silentAfter, st.spec.SilentAfter.Load())
			if base := later(later(on.since, a.heard), a.subscribed); now.Sub(base) >= after && txHeadAt.After(base) {
				silent = append(silent, t)
				on.since, on.silentAfter = now, min(2*after, st.spec.SilentMax)
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
			Msg("relay subscription silent while heads carry transactions, subscribing again")
		s.push.Resubscribe(t)
	}
}

// pushSource reports whether upstreamId is a push source (spec §6.1): a
// WebSocket subscription source the last pass set to newHeads, whose
// newHeads the upstream did not refuse, so its requests ride the socket
// its pushes come on. A push source whose socket closed is one still: its
// expired vote moves it last.
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

// newSource sets up u's transports: its WebSocket client as a push source.
// Every poller's latest headers go through intake as u's hash votes (spec
// §6.3), source or not.
func (r *sourceReconciler) newSource(u *upstream.Upstream) *source {
	cfg := u.Config()
	s := &source{id: u.Id(), mode: cfg.SubscriptionSource()}
	p, ok := u.EvmStatePoller().(*evm.EvmStatePoller)
	if ok && !p.IsObjectNull() {
		p.BindIntake(r.nw.intake)
		s.clocked = true
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
		if a := wsupstream.New(u, indexerNetworkKey(r.nw), pushIntake{r.nw.intake}, r.nw.logger, opts); a != nil {
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
	for _, st := range r.relays {
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
}

// stopAll stops every push subscription when the network ends.
func (r *sourceReconciler) stopAll() {
	for _, s := range r.sources {
		r.stop(s)
	}
}

// requireSubscriptionSource fails with ErrNoSubscriptionSource when nothing
// on the network can feed subType, after the sources caught up with the
// network's upstreams. logs need heads and a feed that serves receipts or
// their fallback (spec §10); relayed kinds need a push source (§12).
// A subscribe skips the source pass when the last pass found a source and
// one feeds now (sourceReconciler.live), so subscribes on a fed topic never
// queue behind a pass.
func (n *Network) requireSubscriptionSource(ctx context.Context, method, subType string) error {
	if n.sources == nil {
		return common.NewErrNoSubscriptionSource(n.networkId, method, subType)
	}
	if n.subscriptionSourced(subType) && n.sources.live(subType) {
		return nil
	}
	if err := n.sources.sync(ctx); err != nil {
		return err
	}
	if !n.subscriptionSourced(subType) {
		return common.NewErrNoSubscriptionSource(n.networkId, method, subType)
	}
	return nil
}

// subscriptionSourced reports whether the last source pass found a feed
// for subType; a subType with no source of its own always has one.
func (n *Network) subscriptionSourced(subType string) bool {
	n.sources.mu.Lock()
	heads, push := n.sources.heads, n.sources.push
	n.sources.mu.Unlock()
	switch {
	case subType == evmpubsub.KindNewHeads:
		return heads
	case subType == evmpubsub.KindLogs:
		return heads && n.chainTracker != nil && n.chainTracker.ServesLogs()
	case n.relay.Spec(subType) != nil:
		return push
	default:
		return true
	}
}

// live reports whether a source the last pass set feeds subType now: a
// connected push source, or for heads the block clock of a network with a
// poller.
func (r *sourceReconciler) live(subType string) bool {
	relayed := r.nw.relay.Spec(subType) != nil
	if subType != evmpubsub.KindNewHeads && subType != evmpubsub.KindLogs && !relayed {
		return true
	}
	if p := r.pushes.Load(); p != nil {
		for _, a := range *p {
			if a.Connected() {
				return true
			}
		}
	}
	if relayed {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.heads
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
