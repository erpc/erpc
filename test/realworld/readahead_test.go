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
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/erpc/erpc/test/chainsim"
)

const (
	// p23HeadRate: eth_blockNumber reads a second per pod, the demand
	// that runs the block clock in every run.
	p23HeadRate = 10.0
	// p23AheadRate: per pod, by-number reads a second one block above the
	// pod's eth_blockNumber, half eth_getBlockByNumber, half eth_getLogs.
	p23AheadRate = 50.0
	// p23Back: each by-hash read of a new block has a control read of
	// the block this far below it, which the view holds: the answering
	// vendor's round trip through the pod with no wait.
	p23Back = 20
	// p23FetchBudget: the read-ahead run's head fetches per new block,
	// polls left out, at most this.
	p23FetchBudget = 1.10
	// p23Noise is harness, not spec: the spread of a loopback round trip
	// and a -race pod's scheduling between a p99 and a p50.
	p23Noise = 10 * time.Millisecond
	// p23Examples: failed reads quoted per kind.
	p23Examples = 5
)

// p23Shape is one chain P23 runs on: its vendors, block time and mint
// jitter (blocks come bt ± jitter apart, so the block clock's predicted
// sight misses some and its tail finds them), the faults every vendor
// has, vendor-a's BlockLag and LogsLag, the blocks before the bar (warm),
// the clock-only, by-hash and read-ahead runs' lengths, and the read-ahead
// run's head fetch budget (0: p23FetchBudget). With old, it runs 2
// old-image and 2 new-image pods, each image's pair sharing redis as a
// fleet's pods do, and reports the new image's reads beside the old's
// (p23Versus). With peer, its by-number readers read the other pod's head
// (p23ByNumber), and the new image's are judged: slack round trips later
// than the old's at most. With future, both images set
// evm.shortCircuitFutureBlocks (shortCircuited). With pre, the checks
// that failed the same way before this change are judged against it
// (p23Pre). With gate, the logs gate's own costs are judged against it
// (p23Gate).
type p23Shape struct {
	name              string
	ups               []freshUp
	bt, jitter        time.Duration
	faults            chainsim.Faults
	blockLag, logsLag time.Duration
	warm              int
	base, hash, ahead time.Duration
	budget            float64
	old, peer, future bool
	slack             int
	pre               *p23Pre
	gate              *p23Gate
}

// p23Pre are a shape's bars for checks that already failed before this
// change, measured on its parent: no worse than there, plus 5%.
type p23Pre struct {
	// p99: the new image's eth_getBlockByNumber p99, judged against it
	// instead of the old image's.
	p99 time.Duration
	// failed: the share of the new image's eth_getBlockByNumber reads
	// that failed, judged against it instead of the old image's.
	failed float64
	// servedMissing: each new-image pod's served_missing.
	servedMissing float64
}

// p23Gate are the logs gate's own bars (spec §6.4 logs gate), measured on
// this branch, no worse than there plus 5%: its eth_getLogs p99, judged
// against it instead of the old image's, and its range checks' refusal
// Asks per new block, judged against it and left out of the shape's head
// fetch budget.
type p23Gate struct {
	logsP99 time.Duration
	asks    float64
}

// p23PreSlack: a measured bar's margin.
const p23PreSlack = 1.05

// shortCircuited is sh with evm.shortCircuitFutureBlocks on, named -sc.
func (sh p23Shape) shortCircuited() p23Shape {
	sh.name, sh.future = sh.name+"-sc", true
	return sh
}

// p23Fast: six https vendors, no push source, 20-45 ms away, each polled
// every 10 s, on a 0.1 s chain, so W is its 2 s floor: their turns come
// 0.06 times a block, so the clock's budget is one fetch a block
// (turnBound). Whole-second timestamps sample a 0.1 s chain's block time
// once a second: the bar waits 300 blocks for its estimate to settle.
var p23Fast = p23Shape{name: "P23", bt: 100 * time.Millisecond, jitter: 20 * time.Millisecond, warm: 300,
	base: time.Minute, hash: time.Minute, ahead: 2 * time.Minute,
	ups: []freshUp{
		{"http-a", "vendor-a", 20 * time.Millisecond, 10 * time.Second, 0, 0},
		{"http-b", "vendor-b", 25 * time.Millisecond, 10 * time.Second, 0, 0},
		{"http-c", "vendor-c", 30 * time.Millisecond, 10 * time.Second, 0, 0},
		{"http-d", "vendor-d", 35 * time.Millisecond, 10 * time.Second, 0, 0},
		{"http-e", "vendor-e", 40 * time.Millisecond, 10 * time.Second, 0, 0},
		{"http-f", "vendor-f", 45 * time.Millisecond, 10 * time.Second, 0, 0},
	}}

// p23Slow: four https vendors 20-35 ms away, polled every 10 s, on a 2 s
// chain, W 6 s, that serve each block 0-0.75 s after it is minted, and
// whose latest tag names a block only 3 s after they serve it, as load
// balancers whose head readers trail their block readers: the by-hash
// readers, fed by a source as fast as the first vendor, ask for a block
// the view lacks while every vendor's head is below it. On such chains
// main's by-hash reads held toward W: p99 2.6 s against an upstream p99
// of 0.25-0.6 s.
var p23Slow = p23Shape{name: "P23-slow", bt: 2 * time.Second, jitter: 200 * time.Millisecond, warm: 25,
	base: 2 * time.Minute, hash: 4 * time.Minute, ahead: 3 * time.Minute,
	faults: chainsim.Faults{TagDelay: 3 * time.Second},
	ups: []freshUp{
		{"http-a", "vendor-a", 20 * time.Millisecond, 10 * time.Second, 0, 0},
		{"http-b", "vendor-b", 25 * time.Millisecond, 10 * time.Second, 0, 250 * time.Millisecond},
		{"http-c", "vendor-c", 30 * time.Millisecond, 10 * time.Second, 0, 500 * time.Millisecond},
		{"http-d", "vendor-d", 35 * time.Millisecond, 10 * time.Second, 0, 750 * time.Millisecond},
	}}

// p23Band: P23-slow's vendors without TagDelay, vendor-a's block readers
// trailing its tag by 300 ms (BlockLag) and its log index by 500 ms
// (LogsLag): its tag names each block first, so a read of it is a band
// read whose first attempt, vendor-a, answers it empty, and no vendor
// serves the block by number for 250 ms; its eth_getLogs answer [] for
// the block until vendor-c serves it too, as a vendor whose log index
// trails its head. Main's retry waited for the block clock's next turn:
// by-number reads 0.56 s later than the first vendor's serve at p50
// (production, mantle and optimism: retried_found reads 1-3 s on 2 s
// blocks). No read can answer before a vendor serves the block. A read at
// or below Latest of a block vendor-a's tag announced fails while no
// vendor serves it, on this branch's base 172a2ec7 as here. Runs with
// this harness (LogsLag included), alternated, 4 each, base 5fed7f9c
// then this branch: eth_getBlockByNumber failed on 5.27%, 4.40%, 4.48%,
// 4.58% of the base's reads, 1.38%, 0.69%, 1.54%, 1.46% of this
// branch's; served_missing per pod at most 77, 59, 67, 64 on the base,
// 20, 12, 23, 23 here. Judged against the base 172a2ec7's highest plus 5%
// (pre). Head fetches per new block, polls and the logs gate's refusal
// Asks left out, the higher pod of each run, at most 2.100 on the base,
// 1.878 here; bar 1.900 x 1.05. The logs gate's own costs (gate):
// refusal Asks per new block, the higher pod, 0.500, 0.556, 0.667, 0.467;
// eth_getLogs p99 1.679, 1.868, 1.897, 1.835 s, against the base's 23-24
// ms, which served 815-845 wrong empty answers per pod.
var p23Band = p23Shape{name: "P23-band", bt: 2 * time.Second, jitter: 200 * time.Millisecond, warm: 25,
	base: 2 * time.Minute, hash: 2 * time.Minute, ahead: 3 * time.Minute,
	blockLag: 300 * time.Millisecond, logsLag: 500 * time.Millisecond, budget: 2.00, old: true, peer: true,
	pre:  &p23Pre{p99: 683 * time.Millisecond, failed: 0.0463, servedMissing: 70},
	gate: &p23Gate{logsP99: 1897 * time.Millisecond, asks: 0.667},
	ups: []freshUp{
		{"http-a", "vendor-a", 20 * time.Millisecond, 10 * time.Second, 0, 0},
		{"http-b", "vendor-b", 25 * time.Millisecond, 10 * time.Second, 0, 250 * time.Millisecond},
		{"http-c", "vendor-c", 30 * time.Millisecond, 10 * time.Second, 0, 500 * time.Millisecond},
		{"http-d", "vendor-d", 35 * time.Millisecond, 10 * time.Second, 0, 750 * time.Millisecond},
	}}

// p23BandProd: P23-band as production's 2 s chains: vendor-b serves each
// block at its mint, so a holder exists when vendor-a answers empty, and
// the old image's immediate sweep found it within a round trip (3460a579
// in production: unfinalized eth_getBlockByNumber p99 20-80 ms). The new
// image's reads are at most a round trip later than the old's. Head
// fetches per new block, polls left out, the higher pod of each run, runs
// alternated with one harness: a788b613 (this branch's parent) 1.800,
// 1.811, 1.922; this branch 2.000, 1.922, 1.944; bar 1.922 x 1.05.
var p23BandProd = p23Shape{name: "P23-band-prod", bt: 2 * time.Second, jitter: 200 * time.Millisecond, warm: 25,
	base: 2 * time.Minute, hash: 2 * time.Minute, ahead: 3 * time.Minute,
	blockLag: 300 * time.Millisecond, budget: 2.02, old: true, peer: true, slack: 1,
	ups: p23ProdUps}

// p23BandAhead: P23-band-prod's chain read a block ahead of each pod's own
// head, reported beside the old image only: the old image answers a block
// no vendor serves yet null at once, the new one waits for it (§6.4 near
// future), so their latencies compare different reads. Main (3614c258):
// by-number p99 870 ms against the old image's non-null 47 ms, the old
// image 14% null; 1.89-1.96 head fetches per new block.
var p23BandAhead = p23Shape{name: "P23-band-ahead", bt: 2 * time.Second, jitter: 200 * time.Millisecond, warm: 25,
	base: 2 * time.Minute, hash: 2 * time.Minute, ahead: 3 * time.Minute,
	blockLag: 300 * time.Millisecond, budget: 1.96, old: true,
	ups: p23ProdUps}

// p23ProdUps are P23-band's vendors with vendor-b serving each block at
// its mint.
var p23ProdUps = []freshUp{
	{"http-a", "vendor-a", 20 * time.Millisecond, 10 * time.Second, 0, 0},
	{"http-b", "vendor-b", 25 * time.Millisecond, 10 * time.Second, 0, 0},
	{"http-c", "vendor-c", 30 * time.Millisecond, 10 * time.Second, 0, 500 * time.Millisecond},
	{"http-d", "vendor-d", 35 * time.Millisecond, 10 * time.Second, 0, 750 * time.Millisecond},
}

// P23: clients that read ahead of the Head, on a shape's chain, no
// cache. First the clock-only run with eth_blockNumber readers
// only: its head fetches per new block are the bar. Then the by-hash run
// with, beside them, as the first vendor serves each block (a fast source
// outside eRPC), its eth_getBlockByHash and its first transaction's
// receipt: the block clock alone moves the view. Then the read-ahead run
// with those and p23AheadRate by-number reads a second of each pod's
// eth_blockNumber + 1 (eth_getBlockByNumber and eth_getLogs of that
// block). Each new-image pod's head fetches per new block of the
// read-ahead run, polls left out, at most the shape's budget. Each
// new-image pod's reads answer the chain's block, none failed; an
// eth_getBlockByNumber may answer null, what its upstreams answered (§6.4:
// it never waits for a block), judged by its share beside the old image;
// no by-hash answer ahead of the pod's newHeads stream (liveViewBound).
// Alone, each by-hash read's latency above the pod's p50 for the same read
// of a block p23Back below (the answering vendor's round trip) at most one
// such round trip at p50 and two at p99 (p23Judge). Beside the old image,
// each read's latency from its start or the first vendor's answer,
// whichever is later (p23Took), reported per image (p23Versus); with peer
// readers, the new image's by-number reads at p50 and p99 at most the old
// pods' answered reads' plus slack round trips, and their share of
// eth_getBlockByNumber reads answered null at most the old pods'. The new
// pods' numbered block reads per client eth_getBlockByNumber at most the
// old pods' plus 5%.
func p23(sh p23Shape) spec {
	sp := freshSpec(sh.name, 1, sh.ups, chainsim.Config{BlockTime: sh.bt, MintJitter: sh.jitter}, sh.faults, func(s *scenario) { p23Body(s, sh) })
	sp.pods, sp.oldPods = 1, nil
	if sh.old {
		sp.pods, sp.oldPods, sp.memory, sp.splitState = 4, []int{0, 1}, false, true
	}
	if sh.blockLag > 0 || sh.logsLag > 0 {
		f := sp.faults["http-a"]
		f.BlockLag, f.LogsLag = sh.blockLag, sh.logsLag
		sp.faults["http-a"] = f
	}
	if sh.future {
		sp.evm = map[string]interface{}{"shortCircuitFutureBlocks": true}
	}
	return sp
}

// p23Reads is one pod's read-ahead tally per kind: latencies (by number,
// p23Took), nulls, failures and the first p23Examples of them.
type p23Reads struct {
	mu    sync.Mutex
	took  map[string][]time.Duration
	nulls map[string]int
	fails map[string]int
	ex    map[string][]string
	// empty counts by-number reads answered empty or failed: a band
	// read answered so is a retried_missing (§17).
	empty int
	// wrongEmpty counts eth_getLogs answered [] for a block whose
	// canonical logs match the read (§6.4 logs gate): wrong data.
	wrongEmpty int
	// byClass tallies the by-number reads again under "method class",
	// class the read's block against the pod's Latest just before it
	// (p23Class); nil when the shape has no peer readers.
	byClass *p23Reads
}

func newP23Reads() *p23Reads {
	return &p23Reads{took: map[string][]time.Duration{}, nulls: map[string]int{}, fails: map[string]int{}, ex: map[string][]string{}}
}

func (r *p23Reads) ok(kind string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.took[kind] = append(r.took[kind], d)
}

// null tallies an eth_getBlockByNumber answered null: what its upstreams
// answered (spec §6.4 band sweep). A read that joins an identical read in
// flight takes that read's null after a vendor served the block, on either
// image alike, so nulls are judged by their share beside the old image
// (p23Nulls), not one by one.
func (r *p23Reads) null(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nulls[kind]++
}

func (r *p23Reads) fail(kind, format string, args ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fails[kind]++
	if len(r.ex[kind]) < p23Examples {
		r.ex[kind] = append(r.ex[kind], time.Now().Format("15:04:05.000")+" "+fmt.Sprintf(format, args...))
	}
}

func p23Body(s *scenario, sh p23Shape) {
	// The bar waits until the block time estimate settled (sh.warm).
	s.blocks(sh.warm)
	vl := &viewLog{}
	// Both latest reads: eth_blockNumber alone is no clock demand.
	heads := liveReads(p23HeadRate, s.chain.BlockTime(), vl)
	taps := make([]*viewTap, len(s.h.pods))
	for _, p := range s.h.pods {
		if !p.old {
			taps[p.idx] = s.startViewTap(p.idx)
		}
	}
	s.blocks(10)

	var b strings.Builder
	ask0 := s.p23Asks()
	_, base, _, _, _ := s.liveRun(sh.base, heads, nil)
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
	if sh.peer {
		for _, r := range reads {
			r.byClass = newP23Reads()
		}
	}
	_, byHash, _, _, _ := s.liveRun(sh.hash, heads, func(ctx context.Context, _ *faultWindows) {
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
	_, ahead, _, _, _ := s.liveRun(sh.ahead, heads, func(ctx context.Context, _ *faultWindows) {
		var wg sync.WaitGroup
		for _, p := range s.h.pods {
			wg.Go(func() { s.p23ByNumber(ctx, p.idx, reads[p.idx], sh) })
			wg.Go(func() { s.p23ByHash(ctx, p.idx, reads[p.idx], vl) })
		}
		wg.Wait()
	})
	ask3 := s.p23Asks()
	s.judgeFetches(&b, "read-ahead", ahead, 0)
	s.blocks(10)

	budget := sh.budget
	if budget == 0 {
		budget = p23FetchBudget
	}
	for _, p := range s.h.pods {
		fmt.Fprintf(&b, "pod-%d refusal Asks clock only: %s; by-hash reads: %s; read-ahead: %s\n", p.idx+1,
			p23AskLine(ask0[p.idx], ask1[p.idx], base.blocks), p23AskLine(ask1[p.idx], ask2[p.idx], byHash.blocks), p23AskLine(ask2[p.idx], ask3[p.idx], ahead.blocks))
		// The rule's budget is about one head fetch a block, a shared
		// early fetch or a by-hash read's held fetch included. Polls are
		// background, the same with or without readers, and stay out:
		// with no fetch of latest left to meet them, the 10 s polls of a
		// 2 s chain's 4 vendors cost 0.37 a block (P23-slow).
		var polls, asks float64
		if ahead.blocks > 0 {
			polls = ahead.mix[p.idx]["ask poll fetched"] / float64(ahead.blocks)
			asks = ahead.mix[p.idx]["ask refusal fetched"] / float64(ahead.blocks)
		}
		fmt.Fprintf(&b, "pod-%d (%s image) read-ahead: %.3f head fetches per new block, %.3f of them polls, %.3f refusal Asks; clock only: %.3f\n",
			p.idx+1, s.image(p.idx), ahead.perBlock(p.idx), polls, asks, base.perBlock(p.idx))
		// Beside the old image, head+1 readers are reported only (p23BandAhead).
		judged := !p.old && (!sh.old || sh.peer)
		spent, left := ahead.perBlock(p.idx)-polls, "polls"
		if sh.gate != nil {
			spent, left = spent-asks, "polls and the logs gate's refusal Asks"
			if bar := sh.gate.asks * p23PreSlack; judged && asks > bar {
				s.violate(s.name, "pod-%d: %.3f refusal Asks per new block with read-ahead clients, above the logs gate's %.3f + 5%%",
					p.idx+1, asks, sh.gate.asks)
			}
		}
		if judged && spent > budget {
			s.violate(s.name, "pod-%d: %.3f head fetches per new block with read-ahead clients, %s left out, above %.2f",
				p.idx+1, spent, left, budget)
		}
		var hashJudged []string
		if !sh.old {
			hashJudged = p23HashMethods
		}
		// Head + 1 beside the old image is reported only (p23BandAhead).
		strict := !sh.old || sh.peer
		s.p23Judge(&b, p.idx, "by-hash reads", hashReads[p.idx], hashJudged, strict, sh.pre)
		s.p23Judge(&b, p.idx, "read-ahead", reads[p.idx], hashJudged, strict, sh.pre)
		fmt.Fprintf(&b, "pod-%d (%s image) read-ahead: %d by-number reads answered empty or failed\n", p.idx+1, s.image(p.idx), reads[p.idx].empty)
		var gate []string
		for _, v := range []string{"no", "maybe_alone", "maybe_agreed", "maybe_found", "maybe_unavailable"} {
			n, _ := s.metric(p.idx, "erpc_logs_empty_gate_total", map[string]string{"verdict": v})
			gate = append(gate, fmt.Sprintf("%s %.0f", v, n))
		}
		fmt.Fprintf(&b, "pod-%d (%s image) eth_getLogs answered [] for a block with matching logs (wrongEmpty): %d; logs gate verdicts: %s\n",
			p.idx+1, s.image(p.idx), reads[p.idx].wrongEmpty, strings.Join(gate, ", "))
		if !p.old && reads[p.idx].wrongEmpty > 0 {
			s.violate(s.name, "pod-%d: %d eth_getLogs answered [] for a block with matching logs", p.idx+1, reads[p.idx].wrongEmpty)
		}
		if !p.old {
			s.judgeView(&b, taps[p.idx], vl, viewRules{bound: liveViewBound, numbers: true})
		}
	}
	if sh.old {
		// By-hash reads above Latest go out as they came (§6.4 by-hash
		// answer), as the old image's: shown, not judged here.
		s.p23Versus(&b, "by-hash reads", hashReads, p23HashMethods, sh.slack, false, nil, nil)
		s.p23Versus(&b, "read-ahead", reads, p23HashMethods, sh.slack, false, nil, nil)
		s.p23Versus(&b, "read-ahead", reads, p23NumberMethods, sh.slack, sh.peer, sh.pre, sh.gate)
		s.p23Attempts(&b, ahead, reads, sh.peer)
		s.p23Nulls(&b, reads, sh.peer, sh.pre)
		s.p23Classes(&b, reads)
	}
	for _, p := range s.h.pods {
		if p.old || sh.pre == nil {
			continue
		}
		v, _ := s.metric(p.idx, "erpc_head_refusal_total", map[string]string{"rule": "served_missing"})
		if bar := sh.pre.servedMissing * p23PreSlack; v > bar {
			s.violate(s.name, "pod-%d: served_missing %v, above its parent's %v + 5%%", p.idx+1, v, sh.pre.servedMissing)
		} else {
			s.owe(p.idx, "served_missing", v)
		}
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
// (polled every block), alternating eth_getBlockByNumber and eth_getLogs,
// on connections of their own, as another client's are: waiting for
// blocks up to 2 s away, they held all podConns of the pod's shared
// client, and a by-hash read and its control behind them waited 0.4-0.8 s
// for a connection while eRPC answered each in under 50 ms. With the
// shape's peer, block view on pod, view the other same-image pod's newest
// eth_blockNumber answer (polled every bt/20): a client's eth_blockNumber
// lands on one pod and its read of that block on another, the band's
// source in production. With the shape's old, each read has beside it
// the same read of the block p23Back below. Each block's first
// eth_getLogs read has beside it one filtered by an address and topic of
// its first log (p23Filtered), so the logs gate tests a filter against
// the header's bloom (§6.4 logs gate).
func (s *scenario) p23ByNumber(ctx context.Context, pod int, r *p23Reads, sh p23Shape) {
	c, err := rpc.DialOptions(ctx, s.h.pods[pod].httpURL(s.project, s.chain.ID()), rpc.WithHTTPClient(s.h.podClient()))
	if err != nil {
		s.violate("harness", "pod-%d: by-number client: %v", pod+1, err)
		return
	}
	defer c.Close()
	call := func(result interface{}, method string, args ...interface{}) error {
		ctx, cancel := context.WithTimeout(context.Background(), httpCheckWait)
		defer cancel()
		return c.CallContext(ctx, result, method, args...)
	}
	var view atomic.Uint64
	var wg sync.WaitGroup
	defer wg.Wait()
	from, ahead, poll := pod, uint64(1), s.chain.BlockTime()
	if sh.peer {
		from, ahead, poll = pod^1, 0, s.chain.BlockTime()/20
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			if n, err := freshHead(s, from); err == nil && n > view.Load() {
				view.Store(n)
			}
			sleepCtx(ctx, poll)
		}
	}()
	period := time.Duration(float64(time.Second) / p23AheadRate)
	var filtered uint64
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
		read := func(n uint64, kind string) {
			defer wg.Done()
			var class string
			if r.byClass != nil {
				class = p23Class(call, n)
			}
			start := time.Now()
			got, err := blockRead(call, method, n)
			end := time.Now()
			took := end.Sub(start)
			if err != nil || got == "null" || got == "0 logs of []" {
				r.mu.Lock()
				r.empty++
				r.mu.Unlock()
			}
			if want := s.chain.CanonAt(n); err == nil && method == "eth_getLogs" && got == "0 logs of []" && want != nil && got != p12Want(method, want) {
				r.mu.Lock()
				r.wrongEmpty++
				r.mu.Unlock()
			}
			tally := []*p23Reads{r}
			if class != "" {
				tally = append(tally, r.byClass)
			}
			for i, t := range tally {
				k := kind
				if i == 1 {
					k = kind + " " + class
				}
				switch want := s.chain.CanonAt(n); {
				case err != nil:
					t.fail(k, "%s(%d) failed after %s: %v", method, n, took.Round(time.Millisecond), err)
				case method == "eth_getBlockByNumber" && got == "null":
					t.null(k)
				case want == nil:
					t.fail(k, "%s(%d) answered %s before the chain made it", method, n, got)
				case got != p12Want(method, want):
					t.fail(k, "%s(%d) answered %s, not %s, after %s", method, n, got, p12Want(method, want), took.Round(time.Millisecond))
				default:
					t.ok(k, s.p23Took(want, method, start, end))
				}
			}
		}
		wg.Add(1)
		go read(v+ahead, method)
		if n := v + ahead; method == "eth_getLogs" && n > filtered {
			filtered = n
			wg.Add(1)
			go func() { defer wg.Done(); s.p23Filtered(call, r, n) }()
		}
		if sh.old && v > p23Back {
			wg.Add(1)
			go read(v+ahead-p23Back, method+" control")
		}
	}
}

// p23Filtered reads eth_getLogs(n, n) filtered by the address and first
// topic of block n's first log, tallied as "eth_getLogs filtered"; a block
// the chain has not made, or one with no logs, is not read. An answer of
// [] is a wrongEmpty.
func (s *scenario) p23Filtered(call func(result interface{}, method string, args ...interface{}) error, r *p23Reads, n uint64) {
	const kind = "eth_getLogs filtered"
	b := s.chain.CanonAt(n)
	if b == nil || len(b.Logs()) == 0 {
		return
	}
	first := b.Logs()[0]
	want := 0
	for _, l := range b.Logs() {
		if l.Address == first.Address && l.Topics[0] == first.Topics[0] {
			want++
		}
	}
	q := map[string]interface{}{
		"fromBlock": hexutil.EncodeUint64(n), "toBlock": hexutil.EncodeUint64(n),
		"address": first.Address.Hex(), "topics": []interface{}{first.Topics[0].Hex()},
	}
	var logs []struct {
		BlockHash hash `json:"blockHash"`
	}
	onBlock := func() bool {
		for _, l := range logs {
			if l.BlockHash.Hex() != b.Hash.Hex() {
				return false
			}
		}
		return true
	}
	start := time.Now()
	err := call(&logs, "eth_getLogs", q)
	end := time.Now()
	took := end.Sub(start).Round(time.Millisecond)
	if err != nil || len(logs) == 0 {
		r.mu.Lock()
		r.empty++
		if err == nil {
			r.wrongEmpty++
		}
		r.mu.Unlock()
	}
	switch {
	case err != nil:
		r.fail(kind, "eth_getLogs(%d) filtered failed after %s: %v", n, took, err)
	case len(logs) == 0:
		r.fail(kind, "eth_getLogs(%d) filtered answered [], not %d logs, after %s", n, want, took)
	case len(logs) != want || !onBlock():
		r.fail(kind, "eth_getLogs(%d) filtered answered %d logs, not %d of %s, after %s", n, len(logs), want, b.Hash.Hex(), took)
	default:
		r.ok(kind, s.p23Took(b, "eth_getLogs", start, end))
	}
}

// p23Took is a read of b by method's latency from its start or the first
// vendor's answer of it (p23FirstServe), whichever is later: no read
// answers before a vendor serves the block.
func (s *scenario) p23Took(b *chainsim.Block, method string, start, end time.Time) time.Duration {
	if first := s.p23FirstServe(b, method); first.After(start) {
		start = first
	}
	return end.Sub(start)
}

// p23FirstServe is when the first vendor answers method for b (ServesAt).
func (s *scenario) p23FirstServe(b *chainsim.Block, method string) time.Time {
	var first time.Time
	for _, v := range s.vendors {
		if at := v.ServesAt(b, method); first.IsZero() || at.Before(first) {
			first = at
		}
	}
	return first
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

// p23HashReads reads b by hash once the first vendor serves it that way
// (p23FirstServe: a fast source outside eRPC feeds the reader), and its
// first transaction's receipt when it has one, side by side, tallied
// under kind+suffix; with vl, each answer is a one-view sample of a
// new-image pod.
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
		time.Sleep(time.Until(s.p23FirstServe(b, "eth_getBlockByHash")))
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

// p23HashMethods are the by-hash reads P23 judges; p23NumberMethods the
// by-number reads, judged beside the old image.
var (
	p23HashMethods   = []string{"eth_getBlockByHash", "eth_getTransactionReceipt"}
	p23NumberMethods = []string{"eth_getBlockByNumber", "eth_getLogs"}
)

// p23Judge reports pod's reads of phase and flags a new-image pod's failed
// reads, and reads of methods whose added latency, above the pod's p50 of
// the same read of a held block (the round trip), is more than one round
// trip plus p23Noise at p50 or two at p99: a by-hash answer above Latest
// is sent as it came (spec §6.4 by-hash answer), and a by-number read
// waits at most for the clock's fetch out (§6.8 proof probe). An
// old-image pod's failed reads are counted only, as are all with strict
// off.
func (s *scenario) p23Judge(b *strings.Builder, pod int, phase string, r *p23Reads, methods []string, strict bool, pre *p23Pre) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kinds := slices.Sorted(maps.Keys(r.took))
	for k := range maps.Keys(r.fails) {
		if !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	for k := range maps.Keys(r.nulls) {
		if !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	for _, k := range kinds {
		ds := r.took[k]
		slices.Sort(ds)
		fmt.Fprintf(b, "pod-%d (%s image) %s, %s: %d answered, %d null, %d failed; latency p50 %s p99 %s max %s\n",
			pod+1, s.image(pod), phase, k, len(ds), r.nulls[k], r.fails[k], pctl(ds, 50), pctl(ds, 99), pctl(ds, 100))
		// With pre, failed eth_getBlockByNumber reads are judged by their
		// share against the parent's (p23Nulls).
		if r.fails[k] > 0 && strict && !s.h.pods[pod].old && !(pre != nil && k == "eth_getBlockByNumber") {
			s.violate(s.name, "pod-%d %s: %d %s reads failed: %s", pod+1, phase, r.fails[k], k, strings.Join(r.ex[k], "; "))
		}
	}
	for _, m := range methods {
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
		if pctl(added, 50) > rtt+p23Noise {
			s.violate(s.name, "pod-%d %s: %s of a new block added %s at p50 over its round trip %s, more than one round trip (+%s)", pod+1, phase, m, pctl(added, 50), rtt, p23Noise)
		}
		if pctl(added, 99) > 2*rtt+p23Noise {
			s.violate(s.name, "pod-%d %s: %s of a new block added %s at p99 over its round trip %s, more than two round trips (+%s)", pod+1, phase, m, pctl(added, 99), rtt, p23Noise)
		}
	}
}

// p23Versus reports phase's reads of methods on the new-image pods beside
// the old-image pods' (p23Took latencies of answered reads, pooled per
// image) and, when judged, flags a p50 or p99 more than the old's plus
// slack round trips (the new pods' p50 of the method's control read) plus
// p23Noise; with pre, an eth_getBlockByNumber p99 above pre's plus 5%
// instead; with gate, an eth_getLogs p99 above the logs gate's plus 5%.
func (s *scenario) p23Versus(b *strings.Builder, phase string, reads []*p23Reads, methods []string, slack int, judged bool, pre *p23Pre, gate *p23Gate) {
	pool := func(old bool, kind string) []time.Duration {
		var ds []time.Duration
		for _, p := range s.h.pods {
			if p.old == old {
				r := reads[p.idx]
				r.mu.Lock()
				ds = append(ds, r.took[kind]...)
				r.mu.Unlock()
			}
		}
		slices.Sort(ds)
		return ds
	}
	for _, m := range methods {
		nw, od, ctl := pool(false, m), pool(true, m), pool(false, m+" control")
		if len(nw) == 0 || len(od) == 0 {
			s.violate(s.name, "%s: no %s reads answered on the new image (%d) or the old (%d)", phase, m, len(nw), len(od))
			continue
		}
		allow := time.Duration(slack)*pctl(ctl, 50) + p23Noise
		fmt.Fprintf(b, "%s, %s: new image p50 %s p99 %s (%d answered), old image p50 %s p99 %s (%d answered), allowed %s more\n",
			phase, m, pctl(nw, 50), pctl(nw, 99), len(nw), pctl(od, 50), pctl(od, 99), len(od), allow)
		for _, q := range []int{50, 99} {
			if !judged {
				break
			}
			if q == 99 && pre != nil && m == "eth_getBlockByNumber" {
				if bar := time.Duration(float64(pre.p99) * p23PreSlack); pctl(nw, q) > bar {
					s.violate(s.name, "%s: %s p99 %s on the new image, above its parent's %s + 5%%", phase, m, pctl(nw, q), pre.p99)
				}
				continue
			}
			if q == 99 && gate != nil && m == "eth_getLogs" {
				if bar := time.Duration(float64(gate.logsP99) * p23PreSlack); pctl(nw, q) > bar {
					s.violate(s.name, "%s: %s p99 %s on the new image, above the logs gate's %s + 5%%", phase, m, pctl(nw, q), gate.logsP99)
				}
				continue
			}
			if pctl(nw, q) > pctl(od, q)+allow {
				s.violate(s.name, "%s: %s p%d %s on the new image against %s on the old, more than %s later", phase, m, q, pctl(nw, q), pctl(od, q), allow)
			}
		}
	}
}

// p23Attempts reports the read-ahead run's numbered block reads through
// the vendors (each client eth_getBlockByNumber's attempts, and the
// clock's block fetches) per client eth_getBlockByNumber, pooled per
// image, and, when judged, flags the new image's above the old's plus 5%.
func (s *scenario) p23Attempts(b *strings.Builder, fs fetchSpan, reads []*p23Reads, judged bool) {
	per := func(old bool) float64 {
		var numbered, clients float64
		for _, p := range s.h.pods {
			if p.old != old {
				continue
			}
			for _, v := range fs.numbered[p.idx] {
				numbered += v
			}
			r := reads[p.idx]
			r.mu.Lock()
			for _, k := range []string{"eth_getBlockByNumber", "eth_getBlockByNumber control"} {
				clients += float64(len(r.took[k]) + r.nulls[k] + r.fails[k])
			}
			r.mu.Unlock()
		}
		if clients == 0 {
			return 0
		}
		return numbered / clients
	}
	nw, od := per(false), per(true)
	fmt.Fprintf(b, "read-ahead: numbered block reads per client eth_getBlockByNumber: new image %.3f, old image %.3f\n", nw, od)
	if judged && nw > od*1.05 {
		s.violate(s.name, "numbered block reads per client eth_getBlockByNumber %.3f on the new image, above the old's %.3f + 5%%", nw, od)
	}
}

// p23NullSlack: the share of reads above the old image's that may answer
// null on the new one. An eth_getBlockByNumber above Latest asks only
// the upstreams whose evidence reaches its block (spec §6.4 band): while
// none does, its null is the truthful answer, where the old image's sweep
// sometimes found a vendor that held the block before it said so.
const p23NullSlack = 0.001

// p23Nulls reports the read-ahead run's shares of client
// eth_getBlockByNumber reads answered null and failed, pooled per image,
// and, when judged, flags: the new image's nulls above the old's plus
// p23NullSlack or its share of reads above Latest (p23Class), whichever is
// more; any null at or below Latest; failures above the old's, or with pre
// above pre's plus 5%.
func (s *scenario) p23Nulls(b *strings.Builder, reads []*p23Reads, judged bool, pre *p23Pre) {
	share := func(old bool) (nulls, fails, above float64) {
		var all float64
		for _, p := range s.h.pods {
			if p.old != old {
				continue
			}
			r := reads[p.idx]
			r.mu.Lock()
			k := "eth_getBlockByNumber"
			nulls += float64(r.nulls[k])
			fails += float64(r.fails[k])
			all += float64(len(r.took[k]) + r.nulls[k] + r.fails[k])
			r.mu.Unlock()
			if c := r.byClass; c != nil {
				c.mu.Lock()
				ka := k + " above Latest"
				above += float64(len(c.took[ka]) + c.nulls[ka] + c.fails[ka])
				c.mu.Unlock()
			}
		}
		if all == 0 {
			return 0, 0, 0
		}
		return nulls / all, fails / all, above / all
	}
	nw, nwFail, nwAbove := share(false)
	od, odFail, _ := share(true)
	fmt.Fprintf(b, "read-ahead: eth_getBlockByNumber null: new image %.2f%%, old image %.2f%%; failed: new image %.2f%%, old image %.2f%%; new image reads above Latest %.2f%%\n", 100*nw, 100*od, 100*nwFail, 100*odFail, 100*nwAbove)
	if !judged {
		return
	}
	// A null is the answer only while no upstream's evidence holds the
	// block, which only a block above Latest can lack.
	if allow := max(p23NullSlack, nwAbove); nw > od+allow {
		s.violate(s.name, "eth_getBlockByNumber null on %.2f%% of the new image's reads, above the old's %.2f%% + %.2f%%: a null is the answer only while no upstream's evidence holds the block (§6.4 band, holders only)", 100*nw, 100*od, 100*allow)
	}
	for _, p := range s.h.pods {
		if c := reads[p.idx].byClass; !p.old && c != nil {
			c.mu.Lock()
			n := c.nulls["eth_getBlockByNumber at or below Latest"]
			c.mu.Unlock()
			if n > 0 {
				s.violate(s.name, "pod-%d: %d eth_getBlockByNumber reads at or below Latest answered null: we announced those blocks", p.idx+1, n)
			}
		}
	}
	failBar, of := odFail, "the old's"
	if pre != nil {
		failBar, of = pre.failed*p23PreSlack, "its parent's + 5%"
	}
	if nwFail > failBar {
		s.violate(s.name, "eth_getBlockByNumber failed on %.2f%% of the new image's reads, above %s %.2f%%", 100*nwFail, of, 100*failBar)
	}
}

// p23Class is block n against the Latest pod's eth_blockNumber names just
// before its read: "above Latest" (the band or the future) or "at or
// below Latest" (thin or served).
func p23Class(call func(result interface{}, method string, args ...interface{}) error, n uint64) string {
	var latest hexutil.Uint64
	if err := call(&latest, "eth_blockNumber"); err != nil {
		return "unknown"
	}
	if n > uint64(latest) {
		return "above Latest"
	}
	return "at or below Latest"
}

// p23Classes reports the read-ahead run's by-number reads per image,
// method and class (p23Class): reads, nulls, failures, p50 and p99, and
// each new-image pod's served_missing. Reported only.
func (s *scenario) p23Classes(b *strings.Builder, reads []*p23Reads) {
	keys := map[string]bool{}
	for _, r := range reads {
		if r.byClass == nil {
			return
		}
		r.byClass.mu.Lock()
		for k := range r.byClass.took {
			keys[k] = true
		}
		for k := range r.byClass.nulls {
			keys[k] = true
		}
		for k := range r.byClass.fails {
			keys[k] = true
		}
		r.byClass.mu.Unlock()
	}
	for _, old := range []bool{false, true} {
		for _, k := range slices.Sorted(maps.Keys(keys)) {
			var ds []time.Duration
			nulls, fails := 0, 0
			for _, p := range s.h.pods {
				if p.old != old {
					continue
				}
				c := reads[p.idx].byClass
				c.mu.Lock()
				ds = append(ds, c.took[k]...)
				nulls += c.nulls[k]
				fails += c.fails[k]
				c.mu.Unlock()
			}
			slices.Sort(ds)
			img := "new"
			if old {
				img = "old"
			}
			fmt.Fprintf(b, "read-ahead by class, %s image, %s: %d reads, %d null, %d failed, p50 %s p99 %s\n", img, k, len(ds)+nulls+fails, nulls, fails, pctl(ds, 50), pctl(ds, 99))
		}
	}
	for _, p := range s.h.pods {
		if !p.old {
			v, _ := s.metric(p.idx, "erpc_head_refusal_total", map[string]string{"rule": "served_missing"})
			fmt.Fprintf(b, "pod-%d (new image) served_missing %v\n", p.idx+1, v)
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
