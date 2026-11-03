package realworld

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/erpc/erpc/test/chainsim"
)

// Bounds of the live-head rows (§19 P19-P22, X10, C19, R2).
const (
	// liveBlockTime: the live-head rows' blocks, P19's 0.25 s.
	liveBlockTime = p19BlockTime
	// liveFetchBound: head fetches a new-image instance may send per new
	// block: one clocked fetch plus early retries and hedges.
	liveFetchBound = 1.05
	// livePushFetchBound: the same while a push source brings every
	// block: full polls only.
	livePushFetchBound = 0.05
	// liveViewBound: once an HTTP latest read on a pod answers N, a ws
	// newHeads subscriber on that pod has N within this. The view the read
	// loaded already made N emittable on every tap; what is left is the
	// tap's write queue: a goroutine wake, one JSON encode, a loopback
	// write, under 1 ms on an idle host. 100 ms holds a GC pause and the
	// scheduling delay of a loaded shared host, and stays 20× below W
	// (2 s), the wait a hash quorum can put between the two.
	liveViewBound = 100 * time.Millisecond
	// liveRate: reads a second per pod in P20-P22, half eth_blockNumber,
	// half eth_getBlockByNumber(latest): above one read per block, where
	// the cost bound (no more requests than the old image) applies.
	liveRate = 5.0
	liveRun  = 5 * time.Minute
)

// The two latest reads a view sample takes.
const (
	viewHead   = "eth_blockNumber"
	viewLatest = "eth_getBlockByNumber(latest)"
)

//
// --- head fetches ---
//

// headFetchKinds are the requests that fetch a head: the latest block by
// tag, and eth_blockNumber.
var headFetchKinds = []string{"latest", blockNumberRead}

// fetchMark is, at one instant, the chain's ticks and each pod's head
// fetches and requests through every vendor proxy of the scenario.
type fetchMark struct {
	at       time.Time
	ticks    uint64
	fetches  []float64
	requests []float64
}

func (s *scenario) fetchMark() fetchMark {
	m := fetchMark{at: time.Now(), ticks: s.chain.Ticks()}
	for _, p := range s.h.pods {
		var f, r float64
		for _, px := range s.proxies {
			for _, k := range headFetchKinds {
				f += float64(px.reads(p, k))
			}
			r += float64(px.reads(p, anyRead))
		}
		m.fetches = append(m.fetches, f)
		m.requests = append(m.requests, r)
	}
	return m
}

// fetchSpan sums new blocks, head fetches and requests per pod over one or
// more windows.
type fetchSpan struct {
	blocks            uint64
	dur               time.Duration
	fetches, requests []float64
}

func (fs *fetchSpan) add(a, b fetchMark) {
	if fs.fetches == nil {
		fs.fetches, fs.requests = make([]float64, len(a.fetches)), make([]float64, len(a.fetches))
	}
	fs.blocks += b.ticks - a.ticks
	fs.dur += b.at.Sub(a.at)
	for i := range a.fetches {
		fs.fetches[i] += b.fetches[i] - a.fetches[i]
		fs.requests[i] += b.requests[i] - a.requests[i]
	}
}

// minus is fs without the windows of o, which lie inside it.
func (fs fetchSpan) minus(o fetchSpan) fetchSpan {
	out := fetchSpan{blocks: fs.blocks - o.blocks, dur: fs.dur - o.dur}
	for i := range fs.fetches {
		var of, or float64
		if o.fetches != nil {
			of, or = o.fetches[i], o.requests[i]
		}
		out.fetches = append(out.fetches, fs.fetches[i]-of)
		out.requests = append(out.requests, fs.requests[i]-or)
	}
	return out
}

func (fs fetchSpan) perBlock(pod int) float64 {
	if fs.blocks == 0 {
		return 0
	}
	return fs.fetches[pod] / float64(fs.blocks)
}

// judgeFetches logs each pod's head fetches per new block over fs and
// flags a new-image pod above limit (0 reports only).
func (s *scenario) judgeFetches(b *strings.Builder, what string, fs fetchSpan, limit float64) {
	for _, p := range s.h.pods {
		r := fs.perBlock(p.idx)
		fmt.Fprintf(b, "pod-%d (%s image) %s: %.0f head fetches (latest block and eth_blockNumber) for %d new blocks, %.3f per block\n",
			p.idx+1, s.image(p.idx), what, fs.fetches[p.idx], fs.blocks, r)
		if !p.old && limit > 0 && r > limit {
			s.violate(s.name, "pod-%d: %.3f head fetches per new block %s (%.0f for %d blocks), bound %.2f", p.idx+1, r, what, fs.fetches[p.idx], fs.blocks, limit)
		}
	}
}

// judgeRequests flags a new image that sent its upstreams more requests
// per pod over fs than the old image.
func (s *scenario) judgeRequests(b *strings.Builder, fs fetchSpan) {
	sum := map[string]float64{}
	pods := map[string]float64{}
	for _, p := range s.h.pods {
		sum[s.image(p.idx)] += fs.requests[p.idx]
		pods[s.image(p.idx)]++
	}
	if pods["old"] == 0 || pods["new"] == 0 {
		return
	}
	secs := fs.dur.Seconds()
	o, n := sum["old"]/pods["old"]/secs, sum["new"]/pods["new"]/secs
	fmt.Fprintf(b, "upstream requests per pod: old image %.2f/s, new image %.2f/s\n", o, n)
	if n > o {
		s.violate(s.name, "new image sent its upstreams %.2f requests/s per pod, old %.2f", n, o)
	}
}

//
// --- reads ---
//

// liveRead is one method's reads on every pod and its bounds against the
// old image: the slacks on served age and latency at p50 and p90, and
// noSlower: latency p50 at most the old image's.
type liveRead struct {
	name     string
	load     freshLoad
	slack    [2]time.Duration
	noSlower bool
}

// liveReads are P19's head reads, rate a second per pod, half
// eth_blockNumber and half eth_getBlockByNumber(latest), through vl.
func liveReads(rate float64, vl *viewLog) []liveRead {
	period := time.Duration(float64(2*time.Second) / rate)
	slack := [2]time.Duration{p19P50Slack, p19P90Slack}
	return []liveRead{
		{viewHead, freshLoad{period, "latest", vl.reader(viewHead)}, slack, true},
		{viewLatest, freshLoad{period, "latest", vl.reader(viewLatest)}, slack, true},
	}
}

// servedAges are the ages of ims' answers whose time keep accepts (nil
// all): from chainsim minting the answered block to the answer.
func (s *scenario) servedAges(ims []*freshImage, keep func(time.Time) bool) []time.Duration {
	var ages []time.Duration
	for _, im := range ims {
		im.mu.Lock()
		answers := slices.Clone(im.answers)
		im.mu.Unlock()
		for _, a := range answers {
			if keep != nil && !keep(a.at) {
				continue
			}
			if bl := s.chain.CanonAt(a.n); bl != nil {
				ages = append(ages, max(a.at.Sub(bl.MintedAt), 0))
			}
		}
	}
	slices.Sort(ages)
	return ages
}

// judgeAgeLatency logs, per read, each image's served block age and
// latency, pooled over its pods, and flags the new image past the read's
// bounds against the old one.
func (s *scenario) judgeAgeLatency(b *strings.Builder, reads []liveRead, imgs [][]*freshImage, from, to time.Time) {
	for i, rd := range reads {
		type cell struct {
			ims  []*freshImage
			took []time.Duration
			errs int
		}
		by := map[string]*cell{}
		for _, im := range imgs[i] {
			img := s.image(im.pod)
			if by[img] == nil {
				by[img] = &cell{}
			}
			c := by[img]
			c.ims = append(c.ims, im)
			im.mu.Lock()
			c.took = append(c.took, im.took...)
			c.errs += im.errs
			im.mu.Unlock()
		}
		ages := map[string][]time.Duration{}
		for _, img := range []string{"old", "new"} {
			c := by[img]
			if c == nil {
				continue
			}
			slices.Sort(c.took)
			ages[img] = s.servedAges(c.ims, nil)
			fmt.Fprintf(b, "%s image %s: %d answers, %d errors, served block age p50 %s p90 %s, latency p50 %s p90 %s max %s\n",
				img, rd.name, len(c.took), c.errs, pctl(ages[img], 50), pctl(ages[img], 90), pctl(c.took, 50), pctl(c.took, 90), pctl(c.took, 100))
		}
		o, n := by["old"], by["new"]
		if o == nil || n == nil {
			continue
		}
		for _, m := range []struct {
			what   string
			on, nn []time.Duration
		}{{"served block age", ages["old"], ages["new"]}, {"latency", o.took, n.took}} {
			if pctl(m.nn, 50) > pctl(m.on, 50)+rd.slack[0] || pctl(m.nn, 90) > pctl(m.on, 90)+rd.slack[1] {
				s.violate(s.name, "%s", s.h.host.verdict(from, to, fmt.Sprintf("new image %s %s p50 %s p90 %s, old image p50 %s p90 %s (slack %s, %s)",
					rd.name, m.what, pctl(m.nn, 50), pctl(m.nn, 90), pctl(m.on, 50), pctl(m.on, 90), rd.slack[0], rd.slack[1])))
			}
		}
		if rd.noSlower && pctl(n.took, 50) > pctl(o.took, 50) {
			s.violate(s.name, "%s", s.h.host.verdict(from, to, fmt.Sprintf("new image %s latency p50 %s, above the old image's %s", rd.name, pctl(n.took, 50), pctl(o.took, 50))))
		}
	}
}

// faultWindows are the spans a scenario's fault ran, and the head fetches
// inside them.
type faultWindows struct {
	s      *scenario
	mu     sync.Mutex
	spans  []span
	in     fetchSpan
	openAt fetchMark
}

// fault runs on, waits d or until ctx ends, runs off, and counts the span
// between as a window.
func (w *faultWindows) fault(ctx context.Context, d time.Duration, on, off func()) {
	a := w.s.fetchMark()
	on()
	sleepCtx(ctx, d)
	off()
	z := w.s.fetchMark()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.in.add(a, z)
	w.spans = append(w.spans, span{a.at, z.at})
}

// inside reports whether t falls in a window.
func (w *faultWindows) inside(t time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, sp := range w.spans {
		if !t.Before(sp.from) && !t.After(sp.to) {
			return true
		}
	}
	return false
}

// liveRun runs reads on every pod for run while fault (nil none) runs
// beside them, and returns each read's tallies, the head fetches over the
// run, the fault's windows, and the run's span.
func (s *scenario) liveRun(run time.Duration, reads []liveRead, fault func(context.Context, *faultWindows)) (imgs [][]*freshImage, all fetchSpan, w *faultWindows, from, to time.Time) {
	w = &faultWindows{s: s}
	ctx, cancel := context.WithTimeout(s.ctx, run)
	defer cancel()
	from = time.Now()
	a := s.fetchMark()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if fault != nil {
			fault(ctx, w)
		}
	}()
	var fl []freshLoad
	for _, r := range reads {
		fl = append(fl, r.load)
	}
	imgs, _ = s.freshRun(run, fl...)
	cancel()
	<-done
	all.add(a, s.fetchMark())
	return imgs, all, w, from, time.Now()
}

// liveCompare: liveReads at liveRate on every pod for liveRun; per image,
// served age and latency against the old image; per new-image pod, head
// fetches per new block; per image, upstream requests.
func liveCompare(s *scenario) {
	s.blocks(10)
	vl := &viewLog{}
	reads := liveReads(liveRate, vl)
	imgs, all, _, from, to := s.liveRun(liveRun, reads, nil)
	var b strings.Builder
	s.judgeAgeLatency(&b, reads, imgs, from, to)
	s.judgeFetches(&b, "over the run", all, liveFetchBound)
	s.judgeRequests(&b, all)
	s.logf("%s", b.String())
}

//
// --- one view ---
//

// viewSample is one latest read of a new-image pod: when it was sent and
// answered, the number, and for the block its hash.
type viewSample struct {
	pod        int
	method     string
	start, end time.Time
	n          uint64
	h          hash
}

// viewLog holds the latest reads of new-image pods, and every read's
// error.
type viewLog struct {
	mu      sync.Mutex
	samples []viewSample
	errs    []string
}

// viewRead reads pod's latest once by method: its number, and for the
// block its hash; a null block is an error.
func viewRead(s *scenario, pod int, method string) (uint64, hash, error) {
	if method == viewHead {
		n, err := freshHead(s, pod)
		return n, hash{}, err
	}
	var raw json.RawMessage
	if err := s.call(pod, &raw, "eth_getBlockByNumber", "latest", false); err != nil {
		return 0, hash{}, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return 0, hash{}, errors.New("null block")
	}
	var bl struct {
		Number hexutil.Uint64 `json:"number"`
		Hash   hash           `json:"hash"`
	}
	if err := json.Unmarshal(raw, &bl); err != nil {
		return 0, hash{}, err
	}
	return uint64(bl.Number), bl.Hash, nil
}

// reader is a freshLoad read of pod's latest by method, logged in vl.
func (vl *viewLog) reader(method string) func(s *scenario, pod int) (uint64, error) {
	return func(s *scenario, pod int) (uint64, error) {
		start := time.Now()
		n, h, err := viewRead(s, pod, method)
		end := time.Now()
		vl.mu.Lock()
		defer vl.mu.Unlock()
		if err != nil {
			vl.errs = append(vl.errs, fmt.Sprintf("%s pod-%d %s: %v", start.Format("15:04:05.000"), pod+1, method, err))
			return 0, err
		}
		if !s.h.pods[pod].old {
			vl.samples = append(vl.samples, viewSample{pod, method, start, end, n, h})
		}
		return n, nil
	}
}

// of are pod's samples, by send time.
func (vl *viewLog) of(pod int) []viewSample {
	vl.mu.Lock()
	defer vl.mu.Unlock()
	var out []viewSample
	for _, sm := range vl.samples {
		if sm.pod == pod {
			out = append(out, sm)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	return out
}

// judgeErrors flags every read error: a latest read never fails once the
// pod serves.
func (s *scenario) judgeErrors(vl *viewLog) {
	vl.mu.Lock()
	errs := slices.Clone(vl.errs)
	vl.mu.Unlock()
	for _, e := range errs[:min(len(errs), 20)] {
		s.violate(s.name, "latest read failed: %s", e)
	}
	if len(errs) > 20 {
		s.violate(s.name, "%d more latest reads failed", len(errs)-20)
	}
}

type span struct{ from, to time.Time }

// viewEvent is one header a view tap received.
type viewEvent struct {
	at        time.Time
	seg       int // the subscription, from 1
	n         uint64
	h, parent hash
}

// viewTap is a ws newHeads subscriber that stays on one pod and records
// each header with its receipt time, outside the scenario's oracle: what
// the one-view checks hold HTTP answers against. down are the spans no
// subscription was open, downFrom the start of the current one.
type viewTap struct {
	pod      int
	mu       sync.Mutex
	events   []viewEvent
	down     []span
	downFrom time.Time
	seg      int
}

func (s *scenario) startViewTap(pod int) *viewTap {
	vt := &viewTap{pod: pod, downFrom: time.Now()}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		vt.run(s)
	}()
	return vt
}

func (vt *viewTap) run(s *scenario) {
	for s.ctx.Err() == nil {
		dctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
		ch := make(chan json.RawMessage, 4096)
		cl, err := rpc.DialContext(dctx, s.h.pods[vt.pod].wsURL(s.project, s.chain.ID()))
		var sub *rpc.ClientSubscription
		if err == nil {
			if sub, err = cl.EthSubscribe(dctx, ch, "newHeads"); err != nil {
				cl.Close()
			}
		}
		cancel()
		if err != nil {
			if s.ctx.Err() == nil {
				s.violate(s.name, "pod-%d: view tap: newHeads subscribe: %v", vt.pod+1, err)
			}
			sleepCtx(s.ctx, clientRedial)
			continue
		}
		vt.mu.Lock()
		vt.seg++
		seg := vt.seg
		vt.down = append(vt.down, span{vt.downFrom, time.Now()})
		vt.downFrom = time.Time{}
		vt.mu.Unlock()
	read:
		for {
			select {
			case <-s.ctx.Done():
				break read
			case err := <-sub.Err():
				s.violate(s.name, "pod-%d: view tap: newHeads subscription ended: %s", vt.pod+1, errString(err))
				break read
			case raw := <-ch:
				at := time.Now()
				var h headJSON
				if err := json.Unmarshal(raw, &h); err != nil {
					s.violate(s.name, "pod-%d: view tap: newHeads payload does not parse: %v", vt.pod+1, err)
					continue
				}
				vt.mu.Lock()
				vt.events = append(vt.events, viewEvent{at, seg, uint64(h.Number), h.Hash, h.ParentHash})
				vt.mu.Unlock()
			}
		}
		vt.mu.Lock()
		vt.downFrom = time.Now()
		vt.mu.Unlock()
		sub.Unsubscribe()
		cl.Close()
	}
}

// snapshot is the tap's events so far and its down spans, the open one
// running into the future.
func (vt *viewTap) snapshot() ([]viewEvent, []span) {
	vt.mu.Lock()
	defer vt.mu.Unlock()
	down := slices.Clone(vt.down)
	if !vt.downFrom.IsZero() {
		down = append(down, span{vt.downFrom, time.Now().Add(time.Hour)})
	}
	return slices.Clone(vt.events), down
}

// mintDelays are, for each canonical block the tap first received in
// [from, to], the time from its mint to that receipt.
func (s *scenario) mintDelays(vt *viewTap, from, to time.Time) []time.Duration {
	events, _ := vt.snapshot()
	seen := map[hash]bool{}
	var out []time.Duration
	for _, e := range events {
		if e.at.Before(from) || e.at.After(to) || seen[e.h] {
			continue
		}
		seen[e.h] = true
		if b := s.chain.CanonAt(e.n); b != nil && b.Hash == e.h {
			out = append(out, max(e.at.Sub(b.MintedAt), 0))
		}
	}
	slices.Sort(out)
	return out
}

// viewKey is a block a stream holds: its number and hash.
type viewKey struct {
	n uint64
	h hash
}

// streamState is a newHeads client's chain: a header at or below the last
// replaces it and every number above it; a new subscription starts over.
type streamState struct {
	m           map[uint64]hash
	first, last uint64
	seg         int
}

// apply applies e and names how: start, next, replace, duplicate, jump or
// unlinked (the last three out of order); removed are the blocks a replace
// reverted.
func (st *streamState) apply(e viewEvent) (kind string, removed []viewKey) {
	if st.m == nil {
		st.m = map[uint64]hash{}
	}
	restart := func(k string) (string, []viewKey) {
		clear(st.m)
		st.m[e.n], st.first, st.last, st.seg = e.h, e.n, e.n, e.seg
		return k, nil
	}
	switch {
	case e.seg != st.seg:
		return restart("start")
	case e.n == st.last+1 && st.m[st.last] == e.parent:
		st.m[e.n], st.last = e.h, e.n
		return "next", nil
	case e.n <= st.last && st.m[e.n] == e.h:
		return "duplicate", nil
	case e.n <= st.last && (e.n <= st.first || st.m[e.n-1] == e.parent):
		for k := e.n; k <= st.last; k++ {
			if h, ok := st.m[k]; ok {
				removed = append(removed, viewKey{k, h})
			}
			delete(st.m, k)
		}
		st.m[e.n], st.last = e.h, e.n
		st.first = min(st.first, e.n)
		return "replace", removed
	case e.n > st.last+1:
		return restart("jump")
	}
	return restart("unlinked")
}

// viewRules are what judgeView asserts. bound: how long after an HTTP
// answer the stream may still owe it; grace: how long before an answer
// the stream may have reverted the answered block; numbers: also the
// number checks, (a) for both reads and (b) for eth_blockNumber.
type viewRules struct {
	bound, grace time.Duration
	numbers      bool
}

// viewTally is one method's one-view verdicts on one pod.
type viewTally struct {
	samples, skipped           int
	behind, unknown, reverted  int
	late, never                int
	ahead                      []time.Duration // stream receipt − HTTP answer, for answers HTTP gave first
	exBehind, exLate, exRevert []string
}

func example(ex *[]string, format string, args ...interface{}) {
	if len(*ex) < 3 {
		*ex = append(*ex, fmt.Sprintf(format, args...))
	}
}

// judgeView holds the latest reads vl took on vt's pod against vt's stream:
//
//	(a) a read sent after the stream received N answers ≥ N, unless the
//	    stream reverts below it within bound of the answer;
//	(b) a read that answers N (block N with hash H) finds the stream
//	    holding it, or the stream receives it within bound of the answer;
//	(c) a read never answers a block the stream reverted, more than grace
//	    before the answer; and the stream itself keeps its order: each
//	    header next, or a replay from a fork point (§10.2).
//
// Every answered hash must be a block of the chain at that number.
func (s *scenario) judgeView(b *strings.Builder, vt *viewTap, vl *viewLog, r viewRules) {
	events, down := vt.snapshot()
	held := map[viewKey][]time.Time{}
	reverted := map[viewKey][]time.Time{}
	var order []string
	var st streamState
	for _, e := range events {
		kind, removed := st.apply(e)
		switch kind {
		case "duplicate", "jump", "unlinked":
			order = append(order, fmt.Sprintf("%s %s %d %s parent %s", e.at.Format("15:04:05.000"), kind, e.n, short(e.h), short(e.parent)))
			if kind == "duplicate" {
				continue
			}
		}
		held[viewKey{e.n, e.h}] = append(held[viewKey{e.n, e.h}], e.at)
		for _, k := range removed {
			reverted[k] = append(reverted[k], e.at)
		}
	}
	for _, o := range order[:min(len(order), 10)] {
		s.violate(s.name, "pod-%d: view tap newHeads out of order: %s", vt.pod+1, o)
	}
	if len(order) > 10 {
		s.violate(s.name, "pod-%d: view tap newHeads out of order %d more times", vt.pod+1, len(order)-10)
	}
	// firstAfter is the first time in ts after t; false none.
	firstAfter := func(ts []time.Time, t time.Time) (time.Time, bool) {
		for _, x := range ts {
			if x.After(t) {
				return x, true
			}
		}
		return time.Time{}, false
	}
	lastBefore := func(ts []time.Time, t time.Time) (time.Time, bool) {
		var out time.Time
		for _, x := range ts {
			if x.Before(t) {
				out = x
			}
		}
		return out, !out.IsZero()
	}
	tallies := map[string]*viewTally{}
	st = streamState{}
	next := 0
	for _, sm := range vl.of(vt.pod) {
		t := tallies[sm.method]
		if t == nil {
			t = &viewTally{}
			tallies[sm.method] = t
		}
		for next < len(events) && !events[next].at.After(sm.start) {
			st.apply(events[next])
			next++
		}
		owedBy := sm.end.Add(r.bound)
		if next == 0 || slices.ContainsFunc(down, func(d span) bool { return !d.to.Before(sm.start) && !d.from.After(owedBy) }) {
			t.skipped++
			continue
		}
		t.samples++
		at := sm.start.Format("15:04:05.000")
		// The events the stream received while the read ran and within
		// bound after it.
		var within []viewEvent
		for _, e := range events[next:] {
			if e.at.After(owedBy) {
				break
			}
			within = append(within, e)
		}
		hashed := sm.h != (hash{})
		if hashed {
			if cb := s.chain.Block(sm.h); cb == nil || cb.Number != sm.n {
				t.unknown++
				example(&t.exRevert, "%s answered %d %s, no block of the chain at that number", at, sm.n, short(sm.h))
				continue
			}
		}
		// (a)
		if r.numbers && sm.n < st.last && !slices.ContainsFunc(within, func(e viewEvent) bool { return e.n <= sm.n+1 && e.seg == st.seg && e.n <= st.last }) {
			t.behind++
			example(&t.exBehind, "%s sent with the stream at %d, answered %d", at, st.last, sm.n)
		}
		// (b), (c)
		switch {
		case hashed:
			k := viewKey{sm.n, sm.h}
			if st.m[sm.n] == sm.h {
				break
			}
			if got, ok := firstAfter(held[k], sm.start); ok {
				t.ahead = append(t.ahead, max(got.Sub(sm.end), 0))
				if got.After(owedBy) {
					t.late++
					example(&t.exLate, "%s answered %d %s, the stream got it %s after the answer", at, sm.n, short(sm.h), got.Sub(sm.end).Round(time.Millisecond))
				}
				break
			}
			if rt, ok := lastBefore(reverted[k], sm.start); ok {
				if rt.Before(sm.end.Add(-r.grace)) {
					t.reverted++
					example(&t.exRevert, "%s answered %d %s, which the stream reverted %s before the answer", at, sm.n, short(sm.h), sm.end.Sub(rt).Round(time.Millisecond))
				}
				break
			}
			t.never++
			example(&t.exLate, "%s answered %d %s, which the stream never sent", at, sm.n, short(sm.h))
		case r.numbers && sm.n > st.last:
			var got time.Time
			for _, e := range events[next:] {
				if e.n >= sm.n {
					got = e.at
					break
				}
			}
			if got.IsZero() {
				t.never++
				example(&t.exLate, "%s answered %d, the stream never reached it", at, sm.n)
				break
			}
			t.ahead = append(t.ahead, max(got.Sub(sm.end), 0))
			if got.After(owedBy) {
				t.late++
				example(&t.exLate, "%s answered %d, the stream reached it %s after the answer", at, sm.n, got.Sub(sm.end).Round(time.Millisecond))
			}
		}
	}
	for _, method := range []string{viewHead, viewLatest} {
		t := tallies[method]
		if t == nil {
			continue
		}
		slices.Sort(t.ahead)
		fmt.Fprintf(b, "pod-%d one view, %s: %d reads (%d skipped while the tap was down), %d behind the stream, %d not owned by the chain, %d reverted, %d late, %d never streamed; stream behind HTTP %d times, by p50 %s p90 %s max %s (bound %s)\n",
			vt.pod+1, method, t.samples, t.skipped, t.behind, t.unknown, t.reverted, t.late, t.never, len(t.ahead), pctl(t.ahead, 50), pctl(t.ahead, 90), pctl(t.ahead, 100), r.bound)
		if t.behind > 0 {
			s.violate(s.name, "pod-%d: %d of %d %s reads sent after the stream received a block answered below it: %s", vt.pod+1, t.behind, t.samples, method, strings.Join(t.exBehind, "; "))
		}
		if t.late+t.never > 0 {
			s.violate(s.name, "pod-%d: %d of %d %s answers reached the pod's newHeads stream more than %s later (%d never; stream behind by p90 %s, max %s): %s",
				vt.pod+1, t.late+t.never, t.samples, method, r.bound, t.never, pctl(t.ahead, 90), pctl(t.ahead, 100), strings.Join(t.exLate, "; "))
		}
		if t.unknown+t.reverted > 0 {
			s.violate(s.name, "pod-%d: %d of %d %s answers named a hash the chain or the stream does not hold (%d reverted by the stream more than %s before): %s",
				vt.pod+1, t.unknown+t.reverted, t.samples, method, t.reverted, r.grace, strings.Join(t.exRevert, "; "))
		}
	}
}

//
// --- rows ---
//

const (
	x10Run = 3 * time.Minute
	// x10Sample: pod-1's sampler reads each latest about this often;
	// x10PeerRate: pod-2's eth_blockNumber reads a second.
	x10Sample   = 10 * time.Millisecond
	x10PeerRate = 20
)

// X10: one wss and two https vendors, 2 pods sharing redis. Pod-2 takes
// x10PeerRate eth_blockNumber a second; pod-1 a ws newHeads subscriber
// and a sampler reading eth_blockNumber and eth_getBlockByNumber(latest)
// every x10Sample. On pod-1 HTTP and the stream show one view
// (judgeView, bound liveViewBound); every pod sends at most 1.05 head
// fetches per new block; no read fails.
func x10() spec {
	return spec{name: "X10", bound: healthyDelivery, sim: chainsim.Config{BlockTime: liveBlockTime},
		vendors: []vendorSpec{{"ws-a", "vendor-a", true}, {"http-b", "vendor-b", false}, {"http-c", "vendor-c", false}},
		faults: map[string]chainsim.Faults{
			"ws-a": {Slow: 10 * time.Millisecond}, "http-b": {Slow: 20 * time.Millisecond}, "http-c": {Slow: 30 * time.Millisecond},
		},
		clients: func(*scenario) {}, body: x10Body}
}

func x10Body(s *scenario) {
	s.blocks(10)
	tap := s.startViewTap(0)
	s.blocks(8)
	vl := &viewLog{}
	ctx, cancel := context.WithTimeout(s.ctx, x10Run)
	defer cancel()
	a := s.fetchMark()
	var wg sync.WaitGroup
	run := func(im *freshImage, period time.Duration, read func(*scenario, int) (uint64, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.freshClient(ctx, im, period, func(pod int) (uint64, error) { return read(s, pod) })
		}()
	}
	run(&freshImage{pod: 1}, time.Second/x10PeerRate, vl.reader(viewHead))
	run(&freshImage{pod: 0}, x10Sample, vl.reader(viewHead))
	run(&freshImage{pod: 0}, x10Sample, vl.reader(viewLatest))
	wg.Wait()
	var all fetchSpan
	all.add(a, s.fetchMark())
	time.Sleep(liveViewBound + tapProcessingSlack)
	var b strings.Builder
	s.judgeFetches(&b, "over the run", all, liveFetchBound)
	s.judgeView(&b, tap, vl, viewRules{bound: liveViewBound, numbers: true})
	s.judgeErrors(vl)
	s.logf("%s", b.String())
}
