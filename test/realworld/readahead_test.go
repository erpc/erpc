package realworld

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/erpc/erpc/test/chainsim"
)

const (
	// p23BlockTime: a 0.1 s chain, so W is its 2 s floor.
	p23BlockTime = 100 * time.Millisecond
	// p23MintJitter: blocks come 0.1 s ± this apart, so the block clock's
	// predicted sight misses some blocks and its tail finds them.
	p23MintJitter = 20 * time.Millisecond
	// p23HeadRate: eth_blockNumber reads a second per pod, the demand
	// that runs the block clock in both runs.
	p23HeadRate = 10.0
	// p23AheadRate: per pod, by-number reads a second one block above the
	// pod's eth_blockNumber, half eth_getBlockByNumber, half eth_getLogs.
	p23AheadRate = 50.0
	// p23Back: each by-hash read of a new block has a control read of
	// the block this far below it, which the view holds: the answering
	// vendor's round trip through the pod with no wait.
	p23Back = 20
	// p23Base, p23Hash, p23Run: the clock-only run, the by-hash run, then
	// the read-ahead run.
	p23Base = time.Minute
	p23Hash = time.Minute
	p23Run  = 2 * time.Minute
	// p23CostSlack: the read-ahead run's head fetches per new block at
	// most the clock-only run's times this.
	p23CostSlack = 1.05
	// p23Noise is harness, not spec: the spread of a loopback round trip
	// and a -race pod's scheduling between a p99 and a p50.
	p23Noise = 10 * time.Millisecond
	// p23Examples: failed reads quoted per kind.
	p23Examples = 5
)

// p23Ups: six https vendors, no push source, 20-45 ms away, each polled
// every 10 s: their turns come 0.06 times a block, so the clock's budget
// is one fetch a block (turnBound).
var p23Ups = []freshUp{
	{"http-a", "vendor-a", 20 * time.Millisecond, 10 * time.Second, 0, 0},
	{"http-b", "vendor-b", 25 * time.Millisecond, 10 * time.Second, 0, 0},
	{"http-c", "vendor-c", 30 * time.Millisecond, 10 * time.Second, 0, 0},
	{"http-d", "vendor-d", 35 * time.Millisecond, 10 * time.Second, 0, 0},
	{"http-e", "vendor-e", 40 * time.Millisecond, 10 * time.Second, 0, 0},
	{"http-f", "vendor-f", 45 * time.Millisecond, 10 * time.Second, 0, 0},
}

// P23: clients that read ahead of the Head, on a 0.1 s chain behind
// p23Ups, no cache, one pod. First p23Base with eth_blockNumber readers
// only: its head fetches per new block are the bar. Then p23Hash with,
// beside them, as each block is minted (every vendor serves it then, a
// fast source outside eRPC), its eth_getBlockByHash and its first
// transaction's receipt: the block clock alone moves the view. Then
// p23Run with those and p23AheadRate by-number reads a second of the
// pod's eth_blockNumber + 1 (eth_getBlockByNumber and eth_getLogs of that
// block). Head fetches per new block of the read-ahead run at most
// p23CostSlack times the bar; in both runs each by-hash read's latency
// above the pod's p50 for the same read of a block p23Back below (the
// answering vendor's round trip) at most one such round trip at p99;
// every read answers the chain's block, none null; no by-hash answer
// ahead of the pod's newHeads stream (liveViewBound).
func p23() spec {
	sp := freshSpec("P23", 1, p23Ups, chainsim.Config{BlockTime: p23BlockTime, MintJitter: p23MintJitter}, chainsim.Faults{}, p23Body)
	sp.pods, sp.oldPods = 1, nil
	return sp
}

// p23Reads is one pod's read-ahead tally per kind: latencies, failures
// and the first p23Examples of them.
type p23Reads struct {
	mu    sync.Mutex
	took  map[string][]time.Duration
	fails map[string]int
	ex    map[string][]string
	// empty counts by-number reads answered empty or failed: a band
	// read answered so is a retried_missing (§17).
	empty int
}

func newP23Reads() *p23Reads {
	return &p23Reads{took: map[string][]time.Duration{}, fails: map[string]int{}, ex: map[string][]string{}}
}

func (r *p23Reads) ok(kind string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.took[kind] = append(r.took[kind], d)
}

func (r *p23Reads) fail(kind, format string, args ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fails[kind]++
	if len(r.ex[kind]) < p23Examples {
		r.ex[kind] = append(r.ex[kind], time.Now().Format("15:04:05.000")+" "+fmt.Sprintf(format, args...))
	}
}

func p23Body(s *scenario) {
	s.blocks(20)
	vl := &viewLog{}
	heads := []liveRead{{viewHead, freshLoad{time.Duration(float64(time.Second) / p23HeadRate), "latest", vl.reader(viewHead)}, [2]time.Duration{}, false, false}}
	taps := make([]*viewTap, len(s.h.pods))
	for _, p := range s.h.pods {
		taps[p.idx] = s.startViewTap(p.idx)
	}
	s.blocks(10)

	var b strings.Builder
	ask0 := s.p23Asks()
	_, base, _, _, _ := s.liveRun(p23Base, heads, nil)
	ask1 := s.p23Asks()
	s.judgeFetches(&b, "clock only", base, 0)

	newReads := func() []*p23Reads {
		out := make([]*p23Reads, len(s.h.pods))
		for i := range out {
			out[i] = newP23Reads()
		}
		return out
	}
	hashReads, reads := newReads(), newReads()
	_, byHash, _, _, _ := s.liveRun(p23Hash, heads, func(ctx context.Context, _ *faultWindows) {
		var wg sync.WaitGroup
		for _, p := range s.h.pods {
			wg.Go(func() { s.p23ByHash(ctx, p.idx, hashReads[p.idx], vl) })
		}
		wg.Wait()
	})
	ask2 := s.p23Asks()
	s.judgeFetches(&b, "by-hash reads", byHash, 0)

	missing0 := make([]float64, len(s.h.pods))
	for pod := range s.h.pods {
		missing0[pod], _ = s.metric(pod, "erpc_head_refusal_total", map[string]string{"rule": "retried_missing"})
	}
	_, ahead, _, _, _ := s.liveRun(p23Run, heads, func(ctx context.Context, _ *faultWindows) {
		var wg sync.WaitGroup
		for _, p := range s.h.pods {
			wg.Go(func() { s.p23ByNumber(ctx, p.idx, reads[p.idx]) })
			wg.Go(func() { s.p23ByHash(ctx, p.idx, reads[p.idx], vl) })
		}
		wg.Wait()
	})
	ask3 := s.p23Asks()
	s.judgeFetches(&b, "read-ahead", ahead, 0)
	s.blocks(10)

	for _, p := range s.h.pods {
		fmt.Fprintf(&b, "pod-%d refusal Asks clock only: %s; by-hash reads: %s; read-ahead: %s\n", p.idx+1,
			p23AskLine(ask0[p.idx], ask1[p.idx], base.blocks), p23AskLine(ask1[p.idx], ask2[p.idx], byHash.blocks), p23AskLine(ask2[p.idx], ask3[p.idx], ahead.blocks))
		if bar := base.perBlock(p.idx) * p23CostSlack; ahead.perBlock(p.idx) > bar {
			s.violate(s.name, "pod-%d: %.3f head fetches per new block with read-ahead clients, above %.3f, %.2f times its %.3f with readers of latest only",
				p.idx+1, ahead.perBlock(p.idx), bar, p23CostSlack, base.perBlock(p.idx))
		}
		s.p23Judge(&b, p.idx, "by-hash reads", hashReads[p.idx])
		s.p23Judge(&b, p.idx, "read-ahead", reads[p.idx])
		s.judgeView(&b, taps[p.idx], vl, viewRules{bound: liveViewBound, numbers: true})
	}
	s.judgeErrors(vl)
	for pod := range s.h.pods {
		v, _ := s.metric(pod, "erpc_head_refusal_total", map[string]string{"rule": "retried_missing"})
		if d := v - missing0[pod]; d > float64(reads[pod].empty) {
			s.violate(s.name, "pod-%d: %v retried_missing, more than its %d empty or failed by-number reads", pod+1, d, reads[pod].empty)
		} else {
			s.owe(pod, "retried_missing", d)
		}
	}
	s.logf("%s", b.String())
}

// p23ByNumber reads, about p23AheadRate times a second until ctx ends,
// block view + 1 on pod, view the pod's newest eth_blockNumber answer
// (polled every block), alternating eth_getBlockByNumber and eth_getLogs.
func (s *scenario) p23ByNumber(ctx context.Context, pod int, r *p23Reads) {
	var view atomic.Uint64
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			if n, err := freshHead(s, pod); err == nil && n > view.Load() {
				view.Store(n)
			}
			sleepCtx(ctx, s.chain.BlockTime())
		}
	}()
	period := time.Duration(float64(time.Second) / p23AheadRate)
	for i := 0; ctx.Err() == nil; i++ {
		sleepCtx(ctx, period/2+rand.N(period))
		v := view.Load()
		if v == 0 || ctx.Err() != nil {
			continue
		}
		method := "eth_getBlockByNumber"
		if i%2 == 1 {
			method = "eth_getLogs"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := v + 1
			start := time.Now()
			got, err := s.p12Read(pod, method, n)
			took := time.Since(start)
			if err != nil || got == "null" || got == "0 logs of []" {
				r.mu.Lock()
				r.empty++
				r.mu.Unlock()
			}
			switch want := s.chain.CanonAt(n); {
			case err != nil:
				r.fail(method, "%s(%d) failed after %s: %v", method, n, took.Round(time.Millisecond), err)
			case want == nil:
				r.fail(method, "%s(%d) answered %s before the chain made it", method, n, got)
			case got != p12Want(method, want):
				r.fail(method, "%s(%d) answered %s, not %s, after %s", method, n, got, p12Want(method, want), took.Round(time.Millisecond))
			default:
				r.ok(method, took)
			}
		}()
	}
}

// p23ByHash reads on pod, as each block is minted, its eth_getBlockByHash
// and its first transaction's receipt, logged in vl for the one-view
// checks, and the same reads of the block p23Back below as control.
func (s *scenario) p23ByHash(ctx context.Context, pod int, r *p23Reads, vl *viewLog) {
	var wg sync.WaitGroup
	defer wg.Wait()
	last := s.chain.Tip().Number
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		tip := s.chain.Tip()
		for n := last + 1; n <= tip.Number; n++ {
			b, back := s.chain.CanonAt(n), s.chain.CanonAt(n-p23Back)
			if b == nil || back == nil {
				continue
			}
			wg.Add(2)
			go func() { defer wg.Done(); s.p23HashReads(pod, r, vl, b, "") }()
			go func() { defer wg.Done(); s.p23HashReads(pod, r, nil, back, " control") }()
		}
		last = tip.Number
	}
}

// p23HashReads reads b by hash, and its first transaction's receipt when
// it has one, at once and side by side, tallied under kind+suffix; with
// vl, each answer is a one-view sample of a new-image pod.
func (s *scenario) p23HashReads(pod int, r *p23Reads, vl *viewLog, b *chainsim.Block, suffix string) {
	var wg sync.WaitGroup
	defer wg.Wait()
	sample := func(method string, start, end time.Time) {
		if vl == nil || s.h.pods[pod].old {
			return
		}
		vl.mu.Lock()
		vl.samples = append(vl.samples, viewSample{pod, method, start, end, b.Number, b.Hash})
		vl.mu.Unlock()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		kind := "eth_getBlockByHash" + suffix
		var raw json.RawMessage
		start := time.Now()
		err := s.call(pod, &raw, "eth_getBlockByHash", b.Hash.Hex(), false)
		end := time.Now()
		switch got := answerHash(raw, err); {
		case err != nil || got != b.Hash.Hex():
			r.fail(kind, "block %d %s answered %s after %s", b.Number, short(b.Hash), got, end.Sub(start).Round(time.Millisecond))
		default:
			r.ok(kind, end.Sub(start))
			sample("eth_getBlockByHash", start, end)
		}
	}()
	if b.TxCount() == 0 {
		return
	}
	kind := "eth_getTransactionReceipt" + suffix
	var raw json.RawMessage
	start := time.Now()
	err := s.call(pod, &raw, "eth_getTransactionReceipt", b.TxHash(0).Hex())
	end := time.Now()
	var rc struct {
		BlockHash   hash           `json:"blockHash"`
		BlockNumber hexutil.Uint64 `json:"blockNumber"`
	}
	switch {
	case err != nil:
		r.fail(kind, "receipt of block %d's first transaction failed after %s: %v", b.Number, end.Sub(start).Round(time.Millisecond), err)
	case len(raw) == 0 || string(raw) == "null":
		r.fail(kind, "receipt of block %d's first transaction null after %s", b.Number, end.Sub(start).Round(time.Millisecond))
	case json.Unmarshal(raw, &rc) != nil || rc.BlockHash != b.Hash || uint64(rc.BlockNumber) != b.Number:
		r.fail(kind, "receipt of block %d's first transaction names block %d %s", b.Number, rc.BlockNumber, short(rc.BlockHash))
	default:
		r.ok(kind, end.Sub(start))
		sample("eth_getTransactionReceipt", start, end)
	}
}

// p23Judge reports pod's reads of phase and flags failed reads and by-hash
// reads whose added latency at p99, above the pod's p50 of the same read
// of a held block, is more than that p50 plus p23Noise.
func (s *scenario) p23Judge(b *strings.Builder, pod int, phase string, r *p23Reads) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kinds := slices.Sorted(maps.Keys(r.took))
	for k := range r.fails {
		if !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	for _, k := range kinds {
		ds := r.took[k]
		slices.Sort(ds)
		fmt.Fprintf(b, "pod-%d %s, %s: %d answered, %d failed; latency p50 %s p99 %s max %s\n",
			pod+1, phase, k, len(ds), r.fails[k], pctl(ds, 50), pctl(ds, 99), pctl(ds, 100))
		if r.fails[k] > 0 {
			s.violate(s.name, "pod-%d %s: %d %s reads failed: %s", pod+1, phase, r.fails[k], k, strings.Join(r.ex[k], "; "))
		}
	}
	for _, m := range []string{"eth_getBlockByHash", "eth_getTransactionReceipt"} {
		ds, ctl := r.took[m], slices.Clone(r.took[m+" control"])
		slices.Sort(ctl)
		rtt := pctl(ctl, 50)
		if len(ds) == 0 || len(ctl) == 0 {
			s.violate(s.name, "pod-%d %s: no %s reads answered (%d) or no control (%d)", pod+1, phase, m, len(ds), len(ctl))
			continue
		}
		added := make([]time.Duration, len(ds))
		for i, d := range ds {
			added[i] = max(d-rtt, 0)
		}
		slices.Sort(added)
		fmt.Fprintf(b, "pod-%d %s, %s of a new block: added latency over its round trip %s: p50 %s p99 %s max %s\n",
			pod+1, phase, m, rtt, pctl(added, 50), pctl(added, 99), pctl(added, 100))
		if pctl(added, 99) > rtt+p23Noise {
			s.violate(s.name, "pod-%d %s: %s of a new block added %s at p99 over its round trip %s, more than one round trip (+%s)", pod+1, phase, m, pctl(added, 99), rtt, p23Noise)
		}
	}
}

// p23Asks are, per pod, the network's refusal Asks by outcome
// (erpc_head_ask_total{reason=refusal}); nil for a failed scrape.
func (s *scenario) p23Asks() []map[string]float64 {
	out := make([]map[string]float64, len(s.h.pods))
	for _, p := range s.h.pods {
		ss, err := p.scrape()
		if err != nil {
			continue
		}
		m := map[string]float64{}
		for _, x := range ss {
			if x.name == "erpc_head_ask_total" && x.labels["reason"] == "refusal" && x.labels["project"] == s.project && x.labels["network"] == s.network() {
				m[x.labels["outcome"]] += x.value
			}
		}
		out[p.idx] = m
	}
	return out
}

// p23AskLine is to's refusal Asks less from's, per new block, by outcome.
func p23AskLine(from, to map[string]float64, blocks uint64) string {
	if to == nil || blocks == 0 {
		return "none scraped"
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(to)) {
		parts = append(parts, fmt.Sprintf("%s %.3f", k, (to[k]-from[k])/float64(blocks)))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ") + " per new block"
}
