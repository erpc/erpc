package evm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Scheduler bounds (spec §7, §13.2); variables so tests run fast.
var (
	fetchSlots         = 16
	fetchTimeout       = 10 * time.Second
	fetchBackoffMin    = 200 * time.Millisecond
	fetchBackoffMax    = 5 * time.Second
	fetchSlotRecovery  = 10 // successes in a row in a class that add a slot back
	fetchBackoffPrune  = 1024
	errNoTrackerSweep  = errors.New("network cannot sweep upstreams for the chain tracker")
	errFetchUndecoded  = errors.New("tracker fetch answer does not decode")
	errFetchNotFound   = errors.New("tracker fetch answered null: no such block")
	errFetchBackoff    = errors.New("tracker fetch key is in backoff")
	errSchedulerClosed = errors.New("chain fetch scheduler closed")
)

// TrackerFetchHook, when set, runs in each tracker fetch's answer check,
// before the answer decodes, with the fetch's method and params; a panic in
// it is the fetch's panic. Tests use it to panic a fetch; production never
// sets it.
var TrackerFetchHook atomic.Pointer[func(method, params string)]

// fetchClass orders tracker fetches: a lower class starts first, and every
// class keeps at least one slot.
type fetchClass uint8

const (
	classHead        fetchClass = iota // walks, by-hash mismatches, integrity's client-path header fetches
	classTipReceipts                   // receipts within reorgWindow of the head
	classFill                          // fill, cursor fetches, older receipts
	fetchClasses
)

var fetchClassLabels = [fetchClasses]string{"head", "tip_receipts", "fill"}

type fetchKey struct {
	selector, upstream, method, params string
}

// fetchSpec is one tracker fetch. decode turns the answer of from (nil for
// a cached answer) into what the tracker holds; an error fails the fetch,
// and one with ErrCodeEndpointContentValidation only from's attempt.
type fetchSpec struct {
	class    fetchClass
	kind     string // metric label: header, receipts, logs
	selector string // use-upstream selector of a group tracker
	upstream string // "" sweeps the selection order
	method   string
	params   string // JSON array
	byHash   bool
	fresh    bool // a refetch after a wrong answer: never from the cache
	client   bool // on a client request path: never waits out the key's backoff
	// deferChecks: the integrity checks run after decode, outside the
	// upstream attempt, and client waiters take the decoded answer before
	// them (spec §11); every other waiter, the cache and the tracker get it
	// only once they pass.
	deferChecks bool
	decode      func(result []byte, from common.Upstream) (any, error)
}

// key single-flights fetches; one that names an upstream is shared by every
// tracker of the network.
func (s fetchSpec) key() fetchKey {
	if s.upstream != "" {
		return fetchKey{upstream: s.upstream, method: s.method, params: s.params}
	}
	return fetchKey{selector: s.selector, method: s.method, params: s.params}
}

// directives mark the fetch internal, so integrity checks it against held
// data only, after decode when deferChecks, and select the tracker's
// upstream policies, which for fill yield rate permits to clients. Only an
// answer addressed by hash may come from the cache (spec §8): a block by
// number, finalized included, can be reverted by the majority (§6.5), and
// a cached one would hide that.
func (s fetchSpec) directives() *common.RequestDirectives {
	d := &common.RequestDirectives{UpstreamPolicies: common.UpstreamPoliciesTracker}
	if s.class == classFill {
		d.UpstreamPolicies = common.UpstreamPoliciesTrackerFill
	}
	if s.upstream == "" {
		d.UseUpstream = s.selector
	}
	if !s.byHash || s.fresh {
		d.SkipCacheRead = "true"
	}
	d.DeferIntegrity = s.deferChecks
	return d
}

type fetchResult struct {
	upstream string
	val      any
	err      error
}

type fetchRequest struct {
	key       fetchKey
	spec      fetchSpec
	notBefore time.Time
	waiters   int
	trackers  []*ChainTracker
	started   bool
	halvings  uint64 // the scheduler's halvings when the sweep started
	res       fetchResult
	done      chan struct{} // closed once res is set
	// ans is the answer client waiters take: the decoded answer before a
	// deferred check runs, else res. answered closes once it is set.
	ans      fetchResult
	answered chan struct{}
	answer   sync.Once
}

// answerWith sets the answer client waiters take, once.
func (r *fetchRequest) answerWith(res fetchResult) {
	r.answer.Do(func() {
		r.ans = res
		close(r.answered)
	})
}

type fetchBackoff struct {
	next  time.Time
	delay time.Duration
	// capacity: the last failure was a refusal for capacity everywhere,
	// whose backoff ends at the network's next successful fetch.
	capacity bool
}

// fetchScheduler runs every tracker fetch of one network on this pod:
// at most fetchSlots in flight, a share per class, single flight per key,
// backoff per key. Its lifetime is the network's ctx; bg holds every
// goroutine of the network's trackers and fetches.
type fetchScheduler struct {
	ctx     context.Context
	network common.Network
	// sweep runs check on the answer before identical client requests that
	// joined the sweep get it, and caches only an answer check passed.
	sweep func(ctx context.Context, req *common.NormalizedRequest, upstreamId string, check func(*common.NormalizedResponse) error) (*common.NormalizedResponse, error)
	bg    util.Goroutines

	mu       sync.Mutex
	pending  map[fetchKey]*fetchRequest // queued or in flight
	queues   [fetchClasses][]*fetchRequest
	inflight [fetchClasses]int
	share    [fetchClasses]int
	streak   [fetchClasses]int
	halvings uint64
	backoff  map[fetchKey]fetchBackoff
	timer    *time.Timer
	gauges   bool

	// bloom is the network's receipts bloom check, shared by its trackers.
	bloom *bloomGate
	// counts are the fetch counters, bound once each.
	counts telemetry.CounterSet[fetchCount]
}

// fetchCount is one series of erpc_chain_tracker_fetch_total.
type fetchCount struct {
	project, network, kind string
	class                  fetchClass
	outcome                string
}

func fetchCountLabels(k fetchCount) []string {
	return []string{k.project, k.network, k.kind, fetchClassLabels[k.class], k.outcome}
}

func newFetchScheduler(ctx context.Context, n common.Network) *fetchScheduler {
	s := &fetchScheduler{
		ctx:     ctx,
		network: n,
		pending: make(map[fetchKey]*fetchRequest),
		backoff: make(map[fetchKey]fetchBackoff),
		bloom:   newBloomGate(n),
	}
	for c := range s.share {
		s.share[c] = fetchSlots
	}
	if sw, ok := n.(interface {
		SweepUpstreams(context.Context, *common.NormalizedRequest, string, func(*common.NormalizedResponse) error) (*common.NormalizedResponse, error)
	}); ok {
		s.sweep = sw.SweepUpstreams
	}
	context.AfterFunc(ctx, s.close)
	return s
}

// fetchWaiter is one submitter's hold on a request; a client one takes
// the request's answer (fetchRequest.ans), every other its result.
type fetchWaiter struct {
	s      *fetchScheduler
	r      *fetchRequest
	client bool
	left   bool // guarded by s.mu
}

// submit queues spec, or joins the request already queued or in flight for
// its key. It never blocks. t, when set, gets a successful result in its
// mailbox whether or not the waiter stays. A client spec whose key waits
// out a backoff fails at once with errFetchBackoff.
func (s *fetchScheduler) submit(t *ChainTracker, spec fetchSpec) *fetchWaiter {
	key := spec.key()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return s.doneWaiter(key, spec, errSchedulerClosed)
	}
	r := s.pending[key]
	if spec.client && s.waitsLocked(key, r) {
		return s.doneWaiter(key, spec, errFetchBackoff)
	}
	if r == nil {
		r = &fetchRequest{key: key, spec: spec, done: make(chan struct{}), answered: make(chan struct{})}
		if b, ok := s.backoff[key]; ok {
			r.notBefore = b.next
		}
		s.pending[key] = r
		s.queues[spec.class] = append(s.queues[spec.class], r)
	} else if !r.started && spec.class < r.spec.class {
		s.unqueueLocked(r)
		r.spec.class = spec.class
		s.queues[spec.class] = append(s.queues[spec.class], r)
	}
	r.waiters++
	if t != nil && !slices.Contains(r.trackers, t) {
		r.trackers = append(r.trackers, t)
	}
	s.pumpLocked()
	return &fetchWaiter{s: s, r: r, client: spec.client}
}

// doneWaiter holds a request that ended before it was queued.
func (s *fetchScheduler) doneWaiter(key fetchKey, spec fetchSpec, err error) *fetchWaiter {
	r := &fetchRequest{key: key, spec: spec, done: make(chan struct{}), answered: make(chan struct{})}
	r.fail(err)
	return &fetchWaiter{s: s, r: r, client: spec.client}
}

// backoffEnd is when spec's key may be fetched again; zero when it may now.
func (s *fetchScheduler) backoffEnd(spec fetchSpec) time.Time {
	key := spec.key()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backoff[key].next
}

// forgive ends the backoff of spec's key, so a request queued for it may
// start now: what failed it changed.
func (s *fetchScheduler) forgive(spec fetchSpec) {
	key := spec.key()
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.backoff, key)
	if r := s.pending[key]; r != nil && !r.started {
		r.notBefore = time.Time{}
	}
	s.pumpLocked()
}

// waitsLocked: r, the request queued for key if any, has not started and
// the key's backoff runs past now.
func (s *fetchScheduler) waitsLocked(key fetchKey, r *fetchRequest) bool {
	if r != nil && r.started {
		return false
	}
	var notBefore time.Time
	if r != nil {
		notBefore = r.notBefore
	} else if b, ok := s.backoff[key]; ok {
		notBefore = b.next
	}
	return notBefore.After(time.Now())
}

// fetch submits spec for t and waits for its result, bounded by ctx.
func (s *fetchScheduler) fetch(ctx context.Context, t *ChainTracker, spec fetchSpec) fetchResult {
	return s.submit(t, spec).wait(ctx)
}

// wait returns the request's result, or a client waiter's answer; when ctx
// ends first the waiter leaves.
func (w *fetchWaiter) wait(ctx context.Context) fetchResult {
	ready, res := w.r.done, &w.r.res
	if w.client {
		ready, res = w.r.answered, &w.r.ans
	}
	select {
	case <-ready:
		return *res
	case <-ctx.Done():
		w.leave()
		return fetchResult{err: context.Cause(ctx)}
	}
}

// leave drops the waiter; a queued request drops with its last waiter.
func (w *fetchWaiter) leave() {
	s := w.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if w.left {
		return
	}
	w.left = true
	r := w.r
	r.waiters--
	if r.waiters > 0 || r.started || s.pending[r.key] != r {
		return
	}
	delete(s.pending, r.key)
	s.unqueueLocked(r)
}

func (s *fetchScheduler) unqueueLocked(r *fetchRequest) {
	q := s.queues[r.spec.class]
	if i := slices.Index(q, r); i >= 0 {
		s.queues[r.spec.class] = slices.Delete(q, i, i+1)
	}
}

// pumpLocked starts every queued request a slot allows, lowest class
// first, and arms the timer for the earliest one still in backoff.
func (s *fetchScheduler) pumpLocked() {
	// close runs in its own goroutine after ctx ends; until it fails the
	// queued requests, a fetch that ended with ctx must not start them.
	if s.ctx.Err() != nil {
		return
	}
	now := time.Now()
	total := 0
	for _, n := range s.inflight {
		total += n
	}
	var wakeAt time.Time
	for c := range fetchClasses {
		q := s.queues[c]
		for i := 0; i < len(q); {
			r := q[i]
			if r.notBefore.After(now) {
				if wakeAt.IsZero() || r.notBefore.Before(wakeAt) {
					wakeAt = r.notBefore
				}
				i++
				continue
			}
			if !s.mayStartLocked(c, total) {
				break
			}
			q = slices.Delete(q, i, i+1)
			s.startLocked(r)
			total++
		}
		s.queues[c] = q
	}
	if !wakeAt.IsZero() {
		if s.timer != nil {
			s.timer.Stop()
		}
		s.timer = time.AfterFunc(time.Until(wakeAt), func() {
			s.mu.Lock()
			s.pumpLocked()
			s.mu.Unlock()
		})
	}
}

// mayStartLocked: within the limit and the class's share, and leaving one
// slot for every other class with none in flight, whether or not it has
// work yet. A busy class so holds at most fetchSlots-(fetchClasses-1).
func (s *fetchScheduler) mayStartLocked(c fetchClass, total int) bool {
	if total >= fetchSlots || s.inflight[c] >= s.share[c] {
		return false
	}
	if s.inflight[c] == 0 {
		return true
	}
	free := fetchSlots - total
	for o := range fetchClasses {
		if o != c && s.inflight[o] == 0 {
			free--
		}
	}
	return free > 0
}

func (s *fetchScheduler) startLocked(r *fetchRequest) {
	if !s.gauges {
		s.gauges = true
		for c := range fetchClasses {
			s.setSlotsGaugeLocked(c)
		}
	}
	r.started = true
	r.halvings = s.halvings
	s.inflight[r.spec.class]++
	if !s.bg.Go(func() { s.run(r) }) {
		s.inflight[r.spec.class]--
		delete(s.pending, r.key)
		r.fail(errSchedulerClosed)
	}
}

// fail ends a request that never ran: every waiter, client ones included,
// gets err.
func (r *fetchRequest) fail(err error) {
	r.res = fetchResult{err: err}
	r.answerWith(r.res)
	close(r.done)
}

func (s *fetchScheduler) run(r *fetchRequest) {
	res := s.do(r)
	outcome := fetchOutcome(res.err)
	c := r.spec.class

	s.mu.Lock()
	switch outcome {
	case "ok":
		delete(s.backoff, r.key)
		// A cached answer is no evidence that vendors have capacity again.
		if res.upstream != "" {
			s.endCapacityBackoffsLocked()
		}
		s.streak[c]++
		if s.streak[c] >= fetchSlotRecovery {
			s.streak[c] = 0
			if s.share[c] < fetchSlots {
				s.share[c]++
				s.setSlotsGaugeLocked(c)
			}
		}
	case "capacity":
		s.streak[c] = 0
		s.backOffLocked(r.key, true)
		s.halveLowestBusyLocked(r.halvings)
	default:
		s.streak[c] = 0
		s.backOffLocked(r.key, false)
	}
	s.inflight[c]--
	if s.pending[r.key] == r {
		delete(s.pending, r.key)
	}
	trackers := r.trackers
	s.pumpLocked()
	s.mu.Unlock()

	s.counts.Inc(telemetry.MetricChainTrackerFetchTotal,
		fetchCount{s.projectId(), s.networkLabel(), r.spec.kind, c, outcome}, fetchCountLabels)
	if res.err == nil {
		for _, t := range trackers {
			t.fetched(res)
		}
	}
	r.res = res
	r.answerWith(res)
	close(r.done)
}

// do makes the request's one sweep. A panic in it, in the sweep, its
// answer check or decode, fails the fetch: its waiters get the error, run
// frees its slot and backs its key off.
func (s *fetchScheduler) do(r *fetchRequest) (res fetchResult) {
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"chain-fetch",
				fmt.Sprintf("project:%s network:%s method:%s", s.projectId(), s.networkLabel(), r.spec.method),
				common.ErrorFingerprint(rec),
			).Inc()
			log.Error().Interface("panic", rec).Str("stack", string(debug.Stack())).
				Str("network", s.networkLabel()).Str("method", r.spec.method).Msg("chain tracker fetch panicked")
			res = fetchResult{err: fmt.Errorf("chain tracker fetch panicked: %v", rec)}
		}
	}()
	if s.sweep == nil {
		return fetchResult{err: errNoTrackerSweep}
	}
	ctx, span := common.StartDetailSpan(s.ctx, "ChainTracker.Fetch", trace.WithAttributes(
		attribute.String("kind", r.spec.kind),
		attribute.String("class", fetchClassLabels[r.spec.class]),
		attribute.String("method", r.spec.method),
	))
	defer func() {
		span.SetAttributes(attribute.String("upstream.id", res.upstream), attribute.String("outcome", fetchOutcome(res.err)))
		if res.err != nil {
			span.SetStatus(codes.Error, res.err.Error())
		}
		span.End()
	}()
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + r.spec.method + `","params":` + r.spec.params + `}`))
	req.SetDirectives(r.spec.directives())
	var val any
	resp, err := s.sweep(ctx, req, r.spec.upstream, func(resp *common.NormalizedResponse) error {
		// A cached answer names no upstream: no vendor answered it.
		var from common.Upstream
		if u := resp.Upstream(); u != nil && !resp.FromCache() {
			from = u
		}
		if hook := TrackerFetchHook.Load(); hook != nil {
			(*hook)(r.spec.method, r.spec.params)
		}
		jrr, err := resp.JsonRpcResponse(ctx)
		if err != nil {
			return err
		}
		if jrr == nil || jrr.Error != nil {
			return errFetchUndecoded
		}
		v, err := r.spec.decode(bytes.Clone(jrr.GetResultBytes()), from)
		if err != nil {
			return err
		}
		if r.spec.deferChecks && from != nil {
			// A cached answer passed the checks before it was cached.
			r.answerWith(fetchResult{upstream: from.Id(), val: v})
			if err := validateIntegrity(ctx, s.network, from, req, resp, strings.ToLower(r.spec.method)); err != nil {
				return fmt.Errorf("%w: %w", errReceiptsCheck, err)
			}
		}
		// Set only by an answer that passed: the sweep may go on past later
		// ones and return this one.
		val = v
		return nil
	})
	upstreamId := ""
	if resp != nil && !resp.FromCache() && resp.Upstream() != nil {
		upstreamId = resp.Upstream().Id()
	}
	if err != nil {
		return fetchResult{upstream: upstreamId, err: err}
	}
	return fetchResult{upstream: upstreamId, val: val}
}

// fetchOutcome is capacity when every upstream the sweep tried refused for
// capacity: a vendor 429, -32005 or compute units, or a denied upstream
// permit. Breaker-open and unavailable upstreams were never tried.
func fetchOutcome(err error) string {
	if err == nil {
		return "ok"
	}
	var ex *common.ErrUpstreamsExhausted
	if !errors.As(err, &ex) {
		return "error"
	}
	tried := 0
	for _, e := range ex.Errors() {
		if !common.HasErrorCode(e, common.ErrCodeEndpointCapacityExceeded, common.ErrCodeUpstreamRateLimitRuleExceeded) {
			return "error"
		}
		tried++
	}
	if tried == 0 {
		return "error"
	}
	return "capacity"
}

// backOffLocked spaces the key's next attempt: 200 ms, doubling to 5 s. A
// failure long after the last one starts over.
func (s *fetchScheduler) backOffLocked(key fetchKey, capacity bool) {
	now := time.Now()
	b := s.backoff[key]
	if b.delay == 0 || now.Sub(b.next) > fetchBackoffMax {
		b.delay = fetchBackoffMin
	} else {
		b.delay = min(2*b.delay, fetchBackoffMax)
	}
	b.next = now.Add(b.delay)
	b.capacity = capacity
	s.backoff[key] = b
	if len(s.backoff) > fetchBackoffPrune {
		for k, o := range s.backoff {
			if now.Sub(o.next) > fetchBackoffMax {
				delete(s.backoff, k)
			}
		}
	}
}

// endCapacityBackoffsLocked ends every capacity backoff, queued requests'
// included: a vendor answered, so capacity is back (spec §8).
func (s *fetchScheduler) endCapacityBackoffsLocked() {
	for k, b := range s.backoff {
		if !b.capacity {
			continue
		}
		delete(s.backoff, k)
		if r := s.pending[k]; r != nil && !r.started {
			r.notBefore = time.Time{}
		}
	}
}

// halveLowestBusyLocked halves the share of the lowest class with work,
// never below one slot, whatever its fetches in flight. Sweeps that fail
// together halve once: only a sweep that started after the last halving
// (seen) halves.
func (s *fetchScheduler) halveLowestBusyLocked(seen uint64) {
	if seen != s.halvings {
		return
	}
	for c := fetchClasses - 1; ; c-- {
		if s.inflight[c] > 0 || len(s.queues[c]) > 0 {
			if s.share[c] > 1 {
				s.share[c] = max(1, s.share[c]/2)
				s.halvings++
				s.setSlotsGaugeLocked(c)
			}
			return
		}
		if c == 0 {
			return
		}
	}
}

func (s *fetchScheduler) setSlotsGaugeLocked(c fetchClass) {
	telemetry.GaugeHandle(telemetry.MetricChainTrackerFetchSlots,
		s.projectId(), s.networkLabel(), fetchClassLabels[c]).Set(float64(s.share[c]))
}

// close fails every queued request once the network's ctx ends.
func (s *fetchScheduler) close() {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
	}
	var dropped []*fetchRequest
	for c := range s.queues {
		dropped = append(dropped, s.queues[c]...)
		s.queues[c] = nil
	}
	for _, r := range dropped {
		delete(s.pending, r.key)
	}
	s.mu.Unlock()
	for _, r := range dropped {
		r.fail(errSchedulerClosed)
	}
}

func (s *fetchScheduler) projectId() string {
	if s.network == nil {
		return ""
	}
	return s.network.ProjectId()
}

func (s *fetchScheduler) networkLabel() string {
	if s.network == nil {
		return ""
	}
	return s.network.Label()
}
