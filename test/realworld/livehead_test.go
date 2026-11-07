package realworld

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
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
	// block, turnBound of the live rows' voters: polled every 4 s or less
	// often on 0.25 s blocks, their turns come 0.28 times a block, so one
	// fetch a block plus 5%.
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

// fetchMark is, at one instant, the chain's new blocks (ticks less missed
// slots), each pod's head fetches and requests through every vendor proxy
// and the scenario's Prism, and each new-image pod's head fetches by cause
// (clockMix).
type fetchMark struct {
	at       time.Time
	ticks    uint64
	fetches  []float64
	requests []float64
	mix      []map[string]float64
}

func (s *scenario) fetchMark() fetchMark {
	m := fetchMark{at: time.Now(), ticks: s.chain.Ticks() - s.chain.Missed(), mix: s.clockMix()}
	for _, p := range s.h.pods {
		var f, r float64
		for _, px := range s.proxies {
			for _, k := range headFetchKinds {
				f += float64(px.reads(p, k))
			}
			r += float64(px.reads(p, anyRead))
		}
		if s.prism != nil {
			f += float64(s.prism.reads(p.idx, false, "latest"))
			r += float64(s.prism.reads(p.idx, false, anyRead))
		}
		m.fetches = append(m.fetches, f)
		m.requests = append(m.requests, r)
	}
	return m
}

// fetchSpan sums new blocks, head fetches, requests and head fetches by
// cause per pod over one or more windows.
type fetchSpan struct {
	blocks            uint64
	dur               time.Duration
	fetches, requests []float64
	mix               []map[string]float64
}

func (fs *fetchSpan) add(a, b fetchMark) {
	if fs.fetches == nil {
		fs.fetches, fs.requests = make([]float64, len(a.fetches)), make([]float64, len(a.fetches))
		fs.mix = make([]map[string]float64, len(a.fetches))
	}
	fs.blocks += b.ticks - a.ticks
	fs.dur += b.at.Sub(a.at)
	for i := range a.fetches {
		fs.fetches[i] += b.fetches[i] - a.fetches[i]
		fs.requests[i] += b.requests[i] - a.requests[i]
		if b.mix[i] == nil {
			continue
		}
		if fs.mix[i] == nil {
			fs.mix[i] = map[string]float64{}
		}
		for k, v := range b.mix[i] {
			fs.mix[i][k] += v - a.mix[i][k]
		}
	}
}

// minus is fs without the windows of o, which lie inside it.
func (fs fetchSpan) minus(o fetchSpan) fetchSpan {
	out := fetchSpan{blocks: fs.blocks - o.blocks, dur: fs.dur - o.dur, mix: make([]map[string]float64, len(fs.fetches))}
	for i := range fs.fetches {
		var of, or float64
		if o.fetches != nil {
			of, or = o.fetches[i], o.requests[i]
		}
		out.fetches = append(out.fetches, fs.fetches[i]-of)
		out.requests = append(out.requests, fs.requests[i]-or)
		if fs.mix[i] == nil {
			continue
		}
		out.mix[i] = map[string]float64{}
		for k, v := range fs.mix[i] {
			if o.mix != nil {
				v -= o.mix[i][k]
			}
			out.mix[i][k] = v
		}
	}
	return out
}

func (fs fetchSpan) perBlock(pod int) float64 {
	if fs.blocks == 0 {
		return 0
	}
	return fs.fetches[pod] / float64(fs.blocks)
}

// judgeFetches logs each pod's head fetches per new block over fs, a
// new-image pod's also by cause, and flags a new-image pod above limit (0
// reports only).
func (s *scenario) judgeFetches(b *strings.Builder, what string, fs fetchSpan, limit float64) {
	for _, p := range s.h.pods {
		r := fs.perBlock(p.idx)
		fmt.Fprintf(b, "pod-%d (%s image) %s: %.0f head fetches (latest block and eth_blockNumber) for %d new blocks, %.3f per block\n",
			p.idx+1, s.image(p.idx), what, fs.fetches[p.idx], fs.blocks, r)
		if m := fs.mix[p.idx]; m != nil && fs.blocks > 0 {
			fmt.Fprintf(b, "pod-%d head fetches per new block by cause %s:", p.idx+1, what)
			for _, k := range slices.Sorted(maps.Keys(m)) {
				if m[k] > 0 {
					fmt.Fprintf(b, " %s %.3f;", k, m[k]/float64(fs.blocks))
				}
			}
			b.WriteString("\n")
		}
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
// noSlower: latency p50 and p90 at most the old image's plus
// latencyNoise; tail: latency p99 at most the old image's plus P19's p90
// slack, the canary's measure, and no read failing on the new image.
type liveRead struct {
	name     string
	load     freshLoad
	slack    [2]time.Duration
	noSlower bool
	tail     bool
}

// latencyNoise: how far a noSlower latency may sit above the old image's
// and still count as not slower: percentiles round to the millisecond, so
// 1 ms against 0 ms is rounding, not a slower path.
const latencyNoise = 2 * time.Millisecond

// liveReads are P19's head reads, rate a second per pod, half
// eth_blockNumber and half eth_getBlockByNumber(latest), through vl, on a
// chain of block time bt. At one read per W or more, neither is slower
// than the old image's; below, reads meet design section 5's idle edge,
// which waits for one round trip, so latency takes the P19 slacks.
func liveReads(rate float64, bt time.Duration, vl *viewLog) []liveRead {
	period := time.Duration(float64(2*time.Second) / rate)
	slack := [2]time.Duration{p19P50Slack, p19P90Slack}
	w := max(3*bt, 2*time.Second)
	busy := rate*w.Seconds() >= 1
	return []liveRead{
		{viewHead, freshLoad{period, "latest", vl.reader(viewHead)}, slack, busy, false},
		{viewLatest, freshLoad{period, "latest", vl.reader(viewLatest)}, slack, busy, false},
	}
}

// servedAges are the ages of ims' answers whose request keep accepts by
// its send time (nil all): from chainsim minting the answered block to the
// answer. A read sent inside a fault and answered after it is the fault's.
func (s *scenario) servedAges(ims []*freshImage, keep func(time.Time) bool) []time.Duration {
	var ages []time.Duration
	for _, im := range ims {
		im.mu.Lock()
		answers, took := slices.Clone(im.answers), slices.Clone(im.took)
		im.mu.Unlock()
		for i, a := range answers {
			if keep != nil && !keep(a.at.Add(-took[i])) {
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
			fmt.Fprintf(b, "%s image %s: %d answers, %d errors, served block age p50 %s p90 %s, latency p50 %s p90 %s p99 %s max %s\n",
				img, rd.name, len(c.took), c.errs, pctl(ages[img], 50), pctl(ages[img], 90), pctl(c.took, 50), pctl(c.took, 90), pctl(c.took, 99), pctl(c.took, 100))
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
		if rd.noSlower {
			for _, p := range []int{50, 90} {
				if pctl(n.took, p) > pctl(o.took, p)+latencyNoise {
					s.violate(s.name, "%s", s.h.host.verdict(from, to, fmt.Sprintf("new image %s latency p%d %s, above the old image's %s + %s", rd.name, p, pctl(n.took, p), pctl(o.took, p), latencyNoise)))
				}
			}
		}
		if rd.tail && pctl(n.took, 99) > pctl(o.took, 99)+p19P90Slack {
			s.violate(s.name, "%s", s.h.host.verdict(from, to, fmt.Sprintf("new image %s latency p99 %s, above the old image's %s + %s", rd.name, pctl(n.took, 99), pctl(o.took, 99), p19P90Slack)))
		}
		if rd.tail && n.errs > 0 {
			s.violate(s.name, "new image %s: %d reads failed (old image %d)", rd.name, n.errs, o.errs)
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
	liveJudge(s, liveReads(liveRate, s.chain.BlockTime(), &viewLog{}), liveFetchBound)
}

// liveCompareTail is liveCompare plus, at the same rate, the receipt of
// the newest block's first transaction (tipReceipt), every read also
// judged at p99 (tail) and never failing on the new image; head fetches
// per new block at most fetchBound. The slacks are P19's in blocks, a
// fifth and two fifths of one. The receipt keeps the slacks only, not
// noSlower: its block may be above the view, which it waits for (rule 5),
// a round trip of the upstream that answered it.
func liveCompareTail(fetchBound float64) func(*scenario) {
	return func(s *scenario) {
		s.blocks(10)
		bt := s.chain.BlockTime()
		reads := liveReads(liveRate, bt, &viewLog{})
		r := reads[0]
		reads = append(reads, liveRead{"eth_getTransactionReceipt(tip)", freshLoad{r.load.period, "latest", tipReceipt}, r.slack, false, true})
		for i := range reads {
			reads[i].tail, reads[i].slack = true, [2]time.Duration{bt / 5, 2 * bt / 5}
		}
		liveJudge(s, reads, fetchBound)
	}
}

func liveJudge(s *scenario, reads []liveRead, fetchBound float64) {
	imgs, all, _, from, to := s.liveRun(liveRun, reads, nil)
	var b strings.Builder
	s.judgeAgeLatency(&b, reads, imgs, from, to)
	s.judgeFetches(&b, "over the run", all, fetchBound)
	s.judgeRequests(&b, all)
	s.logf("%s", b.String())
}

// tipReceipt reads on pod the receipt of the first transaction of the
// chain's newest block that has one, as a wallet does for a transaction
// just mined: its block's number; a null or another block is an error.
func tipReceipt(s *scenario, pod int) (uint64, error) {
	b := s.chain.Tip()
	for b != nil && b.TxCount() == 0 {
		b = b.Parent
	}
	if b == nil {
		return 0, errors.New("no block with a transaction")
	}
	var raw json.RawMessage
	if err := s.call(pod, &raw, "eth_getTransactionReceipt", b.TxHash(0).Hex()); err != nil {
		return 0, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return 0, fmt.Errorf("null receipt of block %d's first transaction", b.Number)
	}
	var r receiptJSON
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, err
	}
	if r.BlockHash != b.Hash {
		return 0, fmt.Errorf("receipt of block %d's first transaction names block %s", b.Number, short(r.BlockHash))
	}
	return b.Number, nil
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
	// p20RTT: every vendor answers after this, a distant vendor's round
	// trip; p20Skew: how far one vendor's head leads or trails the
	// others', under a round trip.
	p20RTT  = 30 * time.Millisecond
	p20Skew = 15 * time.Millisecond
)

// P20: P19's layout at one round trip of 30 ms, the selection policy
// ranking vendor-a first; vendor-a serves each block p20Skew before the
// others (ahead) or after them (behind). As P19: the new image's served
// age and latency within P19's slacks of the old image's, at most 1.05
// head fetches per new block, no more upstream requests.
func p20(ahead bool) spec {
	name, lagged := "P20-behind", func(key string) bool { return key == "http-a" }
	if ahead {
		name, lagged = "P20-ahead", func(key string) bool { return key != "http-a" }
	}
	var ups []freshUp
	for _, u := range p19Ups {
		u.slow, u.delay = p20RTT, 0
		if lagged(u.key) {
			u.delay = p20Skew
		}
		ups = append(ups, u)
	}
	sp := freshSpec(name, 1, ups, chainsim.Config{BlockTime: liveBlockTime, FinalityStep: p17Step}, chainsim.Faults{FinalityDepth: p17Depth}, liveCompare)
	sp.cache = true
	sp.network = map[string]interface{}{"selectionPolicy": rankFirst("http-a")}
	return sp
}

const (
	// p20FastRTT: P20-asym's vendor-a round trip, zora's nearest vendor's;
	// p20SlowRTTs the others', 80-175 ms, as its other vendors' p50.
	p20FastRTT = 5 * time.Millisecond
	// p20AsymBlockTime: zora's blocks. Every vendor's round trip is then
	// within bt/4 of vendor-a's, so the clock's due turns are rungs that
	// may sight a block first, as on zora, not votes after vendor-a's
	// probe sighted it.
	p20AsymBlockTime = 2 * time.Second
	// p20AsymFetchBound: P19's voters' turns on 2 s blocks, Σ min(1,
	// bt/I) = 2.23, plus 5%: what their polls pay for (liveFetchBound).
	p20AsymFetchBound = 2.35
	// p20Lead: P20-flash's vendor-a serves each block this long before the
	// others, as a flashblocks node does.
	p20Lead = 200 * time.Millisecond
)

var p20SlowRTTs = map[string]time.Duration{
	"http-b": 80 * time.Millisecond, "http-c": 95 * time.Millisecond, "http-d": 110 * time.Millisecond,
	"http-e": 130 * time.Millisecond, "http-f": 150 * time.Millisecond, "http-g": 175 * time.Millisecond,
}

// P20-asym: P19's voters on zora's 2 s blocks, no head skew, the
// selection policy ranking vendor-a first, which answers in p20FastRTT,
// the others in p20SlowRTTs; no cache, so every latest read reaches a
// vendor, as on zora (no cache anchor). A thin Head's reads go to
// vendor-a, which holds every block another vendor does, and a tip
// receipt waits no block: latency p99 too within P19's p90 slack of the
// old image's (liveCompareTail), which sends every read there.
func p20Asym() spec {
	var ups []freshUp
	for _, u := range p19Ups {
		u.slow, u.delay = p20FastRTT, 0
		if rtt, ok := p20SlowRTTs[u.key]; ok {
			u.slow = rtt
		}
		ups = append(ups, u)
	}
	sp := freshSpec("P20-asym", 1, ups, chainsim.Config{BlockTime: p20AsymBlockTime, FinalityStep: p17Step}, chainsim.Faults{FinalityDepth: p17Depth}, liveCompareTail(p20AsymFetchBound))
	sp.network = map[string]interface{}{"selectionPolicy": rankFirst("http-a")}
	return sp
}

// P20-asym-slow: P20-asym with the cache on and the selection policy
// ranking vendor-g, the slowest, first, as the shadow's base-sepolia
// policy ranks vendors 63-158 ms away above one 9 ms away. A latest read
// that misses the cache seed at a fresh Head reads the Head's block, which
// every holder serves, so it goes to the holder the clock measured
// fastest: the new image's latest-read latency p90 within P19's p90 slack
// of vendor-a's round trip; as P20-asym, served age within P19's slacks
// of the old image's, which reads vendor-g, at most p20AsymFetchBound
// head fetches per new block.
func p20AsymSlow() spec {
	sp := p20Asym()
	sp.name = "P20-asym-slow"
	sp.cache = true
	sp.network = map[string]interface{}{"selectionPolicy": rankFirst("http-g")}
	sp.body = func(s *scenario) {
		s.blocks(10)
		reads := liveReads(liveRate, s.chain.BlockTime(), &viewLog{})
		imgs, all, _, from, to := s.liveRun(liveRun, reads, nil)
		var b strings.Builder
		s.judgeAgeLatency(&b, reads, imgs, from, to)
		s.judgeFetches(&b, "over the run", all, p20AsymFetchBound)
		s.judgeRequests(&b, all)
		bound := p20FastRTT + p19P90Slack
		for i, rd := range reads {
			if rd.name != viewLatest {
				continue
			}
			var took []time.Duration
			for _, im := range imgs[i] {
				if s.h.pods[im.pod].old {
					continue
				}
				im.mu.Lock()
				took = append(took, im.took...)
				im.mu.Unlock()
			}
			slices.Sort(took)
			if p90 := pctl(took, 90); p90 > bound {
				s.violate(s.name, "%s", s.h.host.verdict(from, to, fmt.Sprintf("new image %s latency p90 %s, above vendor-a's round trip + P19's p90 slack, %s", rd.name, p90, bound)))
			}
		}
		s.logf("%s", b.String())
	}
	return sp
}

// P20-flash: P20's layout, vendor-a serving each block p20Lead before
// the others, the selection policy ranking vendor-b first, no cache. A
// thin Head's reads go to vendor-a, the Leader, as no other vendor holds
// the block yet: no slower than the old image, which reads vendor-b's
// older head, at no more upstream requests (liveCompareTail).
func p20Flash() spec {
	var ups []freshUp
	for _, u := range p19Ups {
		u.slow, u.delay = p20RTT, p20Lead
		if u.key == "http-a" {
			u.delay = 0
		}
		ups = append(ups, u)
	}
	sp := freshSpec("P20-flash", 1, ups, chainsim.Config{BlockTime: liveBlockTime, FinalityStep: p17Step}, chainsim.Faults{FinalityDepth: p17Depth}, liveCompareTail(liveFetchBound))
	sp.network = map[string]interface{}{"selectionPolicy": rankFirst("http-b")}
	return sp
}

// P20-pair: P20-flash with vendor-a and vendor-b both serving each block
// p20Lead before the others, so the block has two holders and no single
// Leader, the selection policy ranking vendor-c first, and every vendor
// serving only blocks its node has synced (blockAvailability upper
// latestBlockMinus 0), as the shadow's monad-testnet vendors are. A
// latest read's ordering and its availability gate read one head per
// vendor, and a skip at or below the Head fetches nothing: as
// P20-flash, at most 1.05 head fetches per new block.
func p20Pair() spec {
	var ups []freshUp
	for _, u := range p19Ups {
		u.slow, u.delay = p20RTT, p20Lead
		if u.key == "http-a" || u.key == "http-b" {
			u.delay = 0
		}
		ups = append(ups, u)
	}
	sp := freshSpec("P20-pair", 1, ups, chainsim.Config{BlockTime: liveBlockTime, FinalityStep: p17Step}, chainsim.Faults{FinalityDepth: p17Depth}, liveCompareTail(liveFetchBound))
	bound := map[string]interface{}{"upper": map[string]interface{}{"latestBlockMinus": 0, "probe": "blockHeader"}}
	for _, ev := range sp.upEvm {
		ev["blockAvailability"] = bound
	}
	sp.network = map[string]interface{}{"selectionPolicy": rankFirst("http-c")}
	return sp
}

const (
	// p21HeadJitter: each vendor serves each block up to this after its
	// mint; p21MintJitter: blocks come 0.25 s ± this apart.
	p21HeadJitter = 20 * time.Millisecond
	p21MintJitter = 10 * time.Millisecond
	// p21EarlyShare: at most this share of block-clock fetches may answer
	// the block before.
	p21EarlyShare = 0.05
)

// P21: P19's vendors with jittered serve and mint times, no push source;
// two pods per image, each image's pair sharing redis. Per image, served
// age and latency as P19; per new-image pod at most 1.05 head fetches per
// new block, and at most 5% of block-clock fetches early.
func p21() spec {
	sp := freshSpec("P21", 1, p19Ups, chainsim.Config{BlockTime: liveBlockTime, MintJitter: p21MintJitter, FinalityStep: p17Step},
		chainsim.Faults{FinalityDepth: p17Depth, HeadJitter: p21HeadJitter}, func(s *scenario) {
			liveCompare(s)
			s.judgeClockEarly()
		})
	sp.pods, sp.oldPods, sp.memory, sp.splitState, sp.cache = 4, []int{0, 1}, false, true, true
	return sp
}

const (
	// p21AvaxBlockTime: avalanche's blocks; each slot's block is minted up
	// to p21AvaxSlotDelay into it, p21AvaxMissed of the slots none.
	p21AvaxBlockTime = time.Second
	p21AvaxSlotDelay = 600 * time.Millisecond
	p21AvaxMissed    = 0.03
	// p21AvaxHeadJitter: each vendor serves each block up to this after
	// its own delay.
	p21AvaxHeadJitter = 100 * time.Millisecond
)

// p21AvaxUps are avalanche's vendors as the shadow saw them, polled every
// 5 s or 10 s, Σ min(1, bt/I) 0.9 with Prism's: each serves a block 150-500
// ms after Prism learns it at mint, and vendor-e, the nearest, last.
var p21AvaxUps = []freshUp{
	{"http-a", "vendor-a", 30 * time.Millisecond, 5 * time.Second, 0, 150 * time.Millisecond},
	{"http-b", "vendor-b", 60 * time.Millisecond, 5 * time.Second, 0, 250 * time.Millisecond},
	{"http-c", "vendor-c", 80 * time.Millisecond, 10 * time.Second, 0, 300 * time.Millisecond},
	{"http-d", "vendor-d", 110 * time.Millisecond, 10 * time.Second, 0, 400 * time.Millisecond},
	{"http-e", "vendor-e", 20 * time.Millisecond, 10 * time.Second, 0, 500 * time.Millisecond},
}

// P21-avax: P21's pods on avalanche's shape: 1 s slots whose blocks come
// up to 0.6 s late or not at all, p21AvaxUps, and Prism, a voter polled
// every 5 s and the network's cache, which learns each block at its mint
// and serves its receipts. Reads as P21's plus the receipt of the newest
// block (tipReceipt), which Prism serves before any vendor holds it. As
// P21: served age and latency within P19's slacks of the old image's
// (the receipt's latency excepted: it waits for the view, rule 5), at
// most 1.05 head fetches per new block, at most 5% of block-clock
// fetches early.
func p21Avax() spec {
	slot := func() (time.Duration, bool) {
		if rand.Float64() < p21AvaxMissed {
			return 0, true
		}
		return rand.N(p21AvaxSlotDelay), false
	}
	sp := freshSpec("P21-avax", 1, p21AvaxUps, chainsim.Config{BlockTime: p21AvaxBlockTime, Slot: slot, FinalityStep: p17Step},
		chainsim.Faults{FinalityDepth: p17Depth, HeadJitter: p21AvaxHeadJitter}, func(s *scenario) {
			s.blocks(10)
			reads := liveReads(liveRate, s.chain.BlockTime(), &viewLog{})
			receipt := liveRead{"eth_getTransactionReceipt(tip)", freshLoad{reads[0].load.period, "latest", tipReceipt}, [2]time.Duration{}, false, false}
			cacheCalls := func(pod int) (all, latest int64) {
				return s.prism.reads(pod, true, anyRead), s.prism.reads(pod, true, "latest")
			}
			before := make([][2]int64, len(s.h.pods))
			for i := range s.h.pods {
				before[i][0], before[i][1] = cacheCalls(i)
			}
			imgs, all, _, from, to := s.liveRun(liveRun, append(reads, receipt), nil)
			var b strings.Builder
			s.judgeAgeLatency(&b, reads, imgs[:len(reads)], from, to)
			s.logReadLatency(&b, receipt.name, imgs[len(reads)])
			s.judgeFetches(&b, "over the run", all, liveFetchBound)
			s.judgeRequests(&b, all)
			for _, p := range s.h.pods {
				n, latest := cacheCalls(p.idx)
				fmt.Fprintf(&b, "pod-%d (%s image): %.2f Prism cache calls/s, %.2f of them latest heads\n", p.idx+1, s.image(p.idx),
					float64(n-before[p.idx][0])/all.dur.Seconds(), float64(latest-before[p.idx][1])/all.dur.Seconds())
			}
			s.logf("%s", b.String())
			s.judgeClockEarly()
		})
	sp.upEvm[prismKeyName] = map[string]interface{}{"statePollerInterval": (5 * time.Second).String()}
	sp.pods, sp.oldPods, sp.memory, sp.splitState, sp.cache, sp.prism = 4, []int{0, 1}, false, true, true, true
	return sp
}

// logReadLatency logs, per image, the answers, errors and latency of
// name's reads ims, pooled over its pods, judging none.
func (s *scenario) logReadLatency(b *strings.Builder, name string, ims []*freshImage) {
	for _, img := range []string{"old", "new"} {
		var took []time.Duration
		errs := 0
		for _, im := range ims {
			if s.image(im.pod) != img {
				continue
			}
			im.mu.Lock()
			took = append(took, im.took...)
			errs += im.errs
			im.mu.Unlock()
		}
		slices.Sort(took)
		fmt.Fprintf(b, "%s image %s: %d answers, %d errors, latency p50 %s p90 %s p99 %s max %s\n",
			img, name, len(took), errs, pctl(took, 50), pctl(took, 90), pctl(took, 99), pctl(took, 100))
	}
}

// judgeClockEarly flags a new-image pod whose block clock's probes and
// retries answered the block before (erpc_head_clock_total, outcome early
// or behind) on more than p21EarlyShare of its fetches (every outcome but
// joined and skipped_push), or has no clock series.
func (s *scenario) judgeClockEarly() {
	for _, p := range s.h.pods {
		if p.old {
			continue
		}
		total, ok := s.metric(p.idx, "erpc_head_clock_total", nil)
		if !ok || total == 0 {
			s.violate(s.name, "pod-%d: no erpc_head_clock_total series: the pod runs no block clock", p.idx+1)
			continue
		}
		for _, o := range []string{"joined", "skipped_push"} {
			v, _ := s.metric(p.idx, "erpc_head_clock_total", map[string]string{"outcome": o})
			total -= v
		}
		var early float64
		for _, kind := range []string{"probe", "retry"} {
			for _, o := range []string{"early", "behind"} {
				v, _ := s.metric(p.idx, "erpc_head_clock_total", map[string]string{"kind": kind, "outcome": o})
				early += v
			}
		}
		if total <= 0 || early/total > p21EarlyShare {
			s.violate(s.name, "pod-%d: %.0f of %.0f block-clock fetches early (%.1f%%), bound %.0f%%", p.idx+1, early, total, 100*early/max(total, 1), 100*p21EarlyShare)
		}
	}
}

const (
	// p22PushDelay: vendor-b's pushes trail its HTTP head by this.
	p22PushDelay = 100 * time.Millisecond
	// p22Blip, p22BlipEvery: vendor-a's socket goes black that long, that
	// often.
	p22Blip      = 2 * time.Second
	p22BlipEvery = 20 * time.Second
	p22BlipRun   = 4 * time.Minute
	// p22Stall: vendor-a's newHeads go silent this long, p22StallLead
	// into the run; the run goes on as long after.
	p22Stall     = 60 * time.Second
	p22StallLead = 60 * time.Second
	// p22TapP99: a block reaches a newHeads subscriber within 2 blocks of
	// its mint, at p99.
	p22TapP99 = 2 * liveBlockTime
)

// P22: two wss push sources, vendor-a first and vendor-b p22PushDelay
// behind, and three https vendors; one old-image and one new-image pod,
// own state each. Blip: vendor-a's socket goes black p22Blip every
// p22BlipEvery; stall: vendor-a's newHeads go silent p22Stall. On the new
// image: latest age p90 in the faults at most its p90 outside them + 1
// block; newHeads p99 at most 2 blocks after the mint; head fetches per
// new block at most 1.05 in the faults and 0.05 outside; served age and
// latency within P19's slacks of the old image's.
func p22(stall bool) spec {
	name := "P22-blip"
	if stall {
		name = "P22-stall"
	}
	return spec{name: name, pods: 2, oldPods: []int{0}, memory: true, cache: true, bound: healthyDelivery,
		sim: chainsim.Config{BlockTime: liveBlockTime},
		vendors: []vendorSpec{{"ws-a", "vendor-a", true}, {"ws-b", "vendor-b", true},
			{"http-c", "vendor-c", false}, {"http-d", "vendor-d", false}, {"http-e", "vendor-e", false}},
		faults: map[string]chainsim.Faults{
			"ws-a": {Slow: 10 * time.Millisecond}, "ws-b": {Slow: 20 * time.Millisecond, PushDelay: p22PushDelay},
			"http-c": {Slow: 25 * time.Millisecond}, "http-d": {Slow: 30 * time.Millisecond}, "http-e": {Slow: 40 * time.Millisecond},
		},
		clients: func(*scenario) {}, body: func(s *scenario) { p22Body(s, stall) }}
}

func p22Body(s *scenario, stall bool) {
	s.blocks(10)
	var tap *viewTap
	for _, p := range s.h.pods {
		if !p.old {
			tap = s.startViewTap(p.idx)
		}
	}
	s.blocks(8)
	vl := &viewLog{}
	reads := liveReads(liveRate, s.chain.BlockTime(), vl)
	run, fault := p22BlipRun, func(ctx context.Context, w *faultWindows) {
		for {
			sleepCtx(ctx, p22BlipEvery-p22Blip)
			if ctx.Err() != nil {
				return
			}
			w.fault(ctx, p22Blip, func() {
				for _, p := range s.h.pods {
					s.proxies["ws-a"].set(p, linkFaults{BlackHole: true})
				}
			}, func() {
				for _, p := range s.h.pods {
					s.proxies["ws-a"].set(p, linkFaults{})
				}
			})
		}
	}
	if stall {
		run, fault = p22StallLead+p22Stall+p22StallLead, func(ctx context.Context, w *faultWindows) {
			sleepCtx(ctx, p22StallLead)
			w.fault(ctx, p22Stall, func() {
				s.vendors["ws-a"].Update(func(f *chainsim.Faults) { f.SilentHeads = true })
			}, func() {
				s.vendors["ws-a"].Update(func(f *chainsim.Faults) { f.SilentHeads = false })
			})
		}
	}
	imgs, all, w, from, to := s.liveRun(run, reads, fault)
	var b strings.Builder
	s.judgeAgeLatency(&b, reads, imgs, from, to)
	s.judgeFetches(&b, "inside the faults", w.in, liveFetchBound)
	s.judgeFetches(&b, "outside the faults", all.minus(w.in), livePushFetchBound)
	for i, rd := range reads {
		var ims, olds []*freshImage
		for _, im := range imgs[i] {
			if s.h.pods[im.pod].old {
				olds = append(olds, im)
			} else {
				ims = append(ims, im)
			}
		}
		in := s.servedAges(ims, w.inside)
		out := s.servedAges(ims, func(t time.Time) bool { return !w.inside(t) })
		oldIn := s.servedAges(olds, w.inside)
		fmt.Fprintf(&b, "new image %s served age inside the faults p50 %s p90 %s (%d, %d slower than 1s), outside p50 %s p90 %s (%d); old image inside p50 %s p90 %s (%d, %d slower than 1s)\n",
			rd.name, pctl(in, 50), pctl(in, 90), len(in), slowReads(ims, w.inside), pctl(out, 50), pctl(out, 90), len(out),
			pctl(oldIn, 50), pctl(oldIn, 90), len(oldIn), slowReads(olds, w.inside))
		if pctl(in, 90) > pctl(out, 90)+liveBlockTime {
			s.violate(s.name, "new image %s served age p90 %s inside the faults, %s outside: more than a block (%s) older", rd.name, pctl(in, 90), pctl(out, 90), liveBlockTime)
		}
	}
	ds := s.mintDelays(tap, from, to)
	fmt.Fprintf(&b, "pod-%d newHeads mint to receipt: %d blocks, p50 %s p99 %s max %s\n", tap.pod+1, len(ds), pctl(ds, 50), pctl(ds, 99), pctl(ds, 100))
	if pctl(ds, 99) > p22TapP99 {
		s.violate(s.name, "%s", s.h.host.verdict(from, to, fmt.Sprintf("pod-%d newHeads mint to receipt p99 %s, bound %s (max %s)", tap.pod+1, pctl(ds, 99), p22TapP99, pctl(ds, 100))))
	}
	s.logf("%s", b.String())
}

// slowReads counts the reads of ims sent while keep holds that took over
// 1s: a read held on a silent upstream.
func slowReads(ims []*freshImage, keep func(time.Time) bool) int {
	n := 0
	for _, im := range ims {
		im.mu.Lock()
		for i, a := range im.answers {
			if keep(a.at.Add(-im.took[i])) && im.took[i] > time.Second {
				n++
			}
		}
		im.mu.Unlock()
	}
	return n
}

const (
	x10Run = 3 * time.Minute
	// x10Sample: pod-1's sampler reads each latest about this often;
	// x10PeerRate: pod-2's eth_blockNumber reads a second.
	x10Sample   = 10 * time.Millisecond
	x10PeerRate = 20
	// x10Far: pod-1's links to the https vendors add this each way, so
	// pod-2's reports of them reach pod-1 through redis before pod-1's
	// own fetches answer: a number another pod learned must not show on
	// pod-1's HTTP before its taps can send it.
	x10Far = 60 * time.Millisecond
)

// X10: one wss and two https vendors, 2 pods sharing redis, pod-1
// x10Far from the https vendors. Pod-2 takes x10PeerRate eth_blockNumber
// a second; pod-1 a ws newHeads subscriber and a sampler reading
// eth_blockNumber and eth_getBlockByNumber(latest) every x10Sample. On
// pod-1 HTTP and the stream show one view (judgeView, bound
// liveViewBound); every pod sends at most 1.05 head fetches per new
// block; no read fails.
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
	for _, key := range []string{"http-b", "http-c"} {
		s.proxies[key].set(s.h.pods[0], linkFaults{Latency: x10Far})
	}
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

const (
	c19Run = 2 * time.Minute
	// c19Rate: latest reads a second per pod, half eth_blockNumber, half
	// eth_getBlockByNumber(latest).
	c19Rate = 20
)

// C19: baseline B's vendors and clients at 0.25 s blocks, reorgs 1-3 deep
// every 4-8 blocks, c19Rate latest reads a second per pod, 2 pods sharing
// redis. No read fails; every answered hash is the chain's at its number;
// every stream keeps its order and is told of each reorg; each
// eth_getBlockByNumber(latest) hash is one the pod's newHeads stream holds
// or receives within a block, or reverted less than a block before.
func c19() spec {
	return spec{name: "C19", bound: healthyDelivery, sim: chainsim.Config{BlockTime: liveBlockTime}, body: c19Body}
}

func c19Body(s *scenario) {
	s.blocks(20)
	taps := []*viewTap{s.startViewTap(0), s.startViewTap(1)}
	s.blocks(8)
	vl := &viewLog{}
	reads := liveReads(c19Rate, s.chain.BlockTime(), vl)
	fault := func(ctx context.Context, _ *faultWindows) {
		for ctx.Err() == nil {
			sleepCtx(ctx, time.Duration(4+rand.IntN(5))*s.chain.BlockTime())
			if ctx.Err() == nil {
				s.reorg(1 + rand.IntN(3))
			}
		}
	}
	_, all, _, _, _ := s.liveRun(c19Run, reads, fault)
	s.blocks(4)
	var b strings.Builder
	s.judgeFetches(&b, "over the run", all, 0)
	for _, tap := range taps {
		s.judgeView(&b, tap, vl, viewRules{bound: s.chain.BlockTime(), grace: s.chain.BlockTime()})
	}
	s.judgeErrors(vl)
	s.logf("%s", b.String())
	s.requireReorgsTold()
}
