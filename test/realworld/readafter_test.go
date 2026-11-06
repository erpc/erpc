package realworld

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Bounds of the read-after-event scenario (§19 R1).
const (
	// r1Behind: one vendor serves this many blocks behind the head.
	r1Behind = 2
	// r1Stale: one vendor executes eth_call(latest) this far below its head.
	r1Stale = 5
	// r1ReadBound: a read sent at delivery answers within W plus the
	// tracker's processing: a block the request names is at or below the
	// delivering pod's Latest; on another pod a by-hash answer waits at
	// most W (§6.4), a number in the band at most W.
	r1ReadBound = liveBound + tapProcessingSlack
	// r1CatchUp: another pod's latest reaches a delivered N within 1 bt
	// (§19 oracle; P1's peerBlockNumberWait).
	r1CatchUp = peerBlockNumberWait
	// r1Follow: harness, how often a lower latest on another pod is read
	// again to measure when it reaches N.
	r1Follow = 10 * time.Millisecond
	// r1ReceiptP99: the old image answers a receipt from its first
	// upstream; the new image's p99 may add P19's p90 slack to that
	// vendor round trip, never a hold of the block's wait.
	r1ReceiptP99 = p19P90Slack
)

// R1: clients on both pods (ws newHeads, ws logs, gRPC StreamBlocks) read at
// once at each delivered block N, on the delivering pod and on the other:
// eth_call Multicall3 getBlockNumber() at latest, at N, at N's hash
// (EIP-1898), eth_getBlockByNumber(latest), eth_blockNumber,
// eth_getTransactionReceipt of N's first transaction, and for logs
// clients eth_getLogs by blockHash. One vendor serves r1Behind blocks
// behind, one executes latest r1Stale behind, reorgs 1–3 deep. No answer
// errs or is null; at N answers N; latest answers ≥ N on the delivering
// pod, and on the other ≥ N within r1CatchUp, its lower answers counted
// apart; receipts answer within r1ReceiptP99 at p99 per side.
func r1() spec {
	ra := &readAfter{stats: map[string]*raStat{}, own: map[string]time.Time{}}
	return spec{name: "R1", bound: healthyDelivery,
		faults:    map[string]chainsim.Faults{"http-a": {Lag: r1Behind}, "http-b": {StaleState: r1Stale}},
		onDeliver: ra.onDeliver,
		body: func(s *scenario) {
			s.blocks(25)
			for end := time.Now().Add(60 * time.Second); time.Now().Before(end); {
				s.blocks(15 + rand.IntN(15))
				s.reorg(1 + rand.IntN(3))
			}
			s.blocks(25)
			s.requireReorgsTold()
		},
		after: ra.report,
	}
}

// readAfter keeps R1's counts per check and side, and every lower latest
// another pod answered.
type readAfter struct {
	mu    sync.Mutex
	stats map[string]*raStat // "<check>/<same|other>"
	gaps  []raGap
	// own: when a client on a pod first received a block, by "pod/hash".
	own map[string]time.Time
}

// ownAt is when a client on pod first received block h; zero not yet.
func (ra *readAfter) ownAt(pod int, h hash) time.Time {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	return ra.own[fmt.Sprintf("%d/%s", pod, h.Hex())]
}

type raStat struct {
	ok, fail, lower int
	lat             []time.Duration
}

// raGap: another pod's latest answered blocks below a delivered N. A read
// reads Head after it is sent, so its pod's Latest was still below N at
// least trailed after the delivery (the send of its last lower read), and
// reached N by caught (the send of its first read at N or above; 0 never
// within httpCheckWait). Read latency is in neither.
type raGap struct {
	check           string
	blocks          uint64
	trailed, caught time.Duration
}

// raCheck is one read: its answer's block number, or a problem.
type raCheck struct {
	name   string
	latest bool // answers the pod's latest: ≥ N, not exactly N
	read   func(s *scenario, target int, b *chainsim.Block, filter int) (uint64, string)
}

var r1Call = map[string]interface{}{"to": chainsim.Multicall3.Hex(), "data": chainsim.GetBlockNumberSig}

func raCallAt(s *scenario, target int, at interface{}) (uint64, string) {
	var out hexutil.Bytes
	if err := s.call(target, &out, "eth_call", r1Call, at); err != nil {
		return 0, "error: " + err.Error()
	}
	if len(out) == 0 {
		return 0, "empty answer"
	}
	return new(big.Int).SetBytes(out).Uint64(), ""
}

var raChecks = []raCheck{
	{"call-latest", true, func(s *scenario, t int, _ *chainsim.Block, _ int) (uint64, string) {
		return raCallAt(s, t, "latest")
	}},
	{"call-number", false, func(s *scenario, t int, b *chainsim.Block, _ int) (uint64, string) {
		return raCallAt(s, t, hexutil.EncodeUint64(b.Number))
	}},
	{"call-hash", false, func(s *scenario, t int, b *chainsim.Block, _ int) (uint64, string) {
		return raCallAt(s, t, map[string]interface{}{"blockHash": b.Hash.Hex()})
	}},
	{"block-latest", true, func(s *scenario, t int, _ *chainsim.Block, _ int) (uint64, string) {
		var raw json.RawMessage
		if err := s.call(t, &raw, "eth_getBlockByNumber", "latest", false); err != nil {
			return 0, "error: " + err.Error()
		}
		if len(raw) == 0 || string(raw) == "null" {
			return 0, "null"
		}
		var h struct {
			Number hexutil.Uint64 `json:"number"`
			Hash   hash           `json:"hash"`
		}
		if err := json.Unmarshal(raw, &h); err != nil {
			return 0, "unparsable: " + err.Error()
		}
		if cb := s.chain.Block(h.Hash); cb == nil || cb.Number != uint64(h.Number) {
			return 0, fmt.Sprintf("block %d %s is no block of the chain at that number", h.Number, short(h.Hash))
		}
		return uint64(h.Number), ""
	}},
	{"block-number", true, func(s *scenario, t int, _ *chainsim.Block, _ int) (uint64, string) {
		var bn hexutil.Uint64
		if err := s.call(t, &bn, "eth_blockNumber"); err != nil {
			return 0, "error: " + err.Error()
		}
		return uint64(bn), ""
	}},
}

// raLogs: eth_getLogs by N's hash answers exactly the block's logs the
// client's filter matches.
var raLogs = raCheck{"logs-hash", false, func(s *scenario, t int, b *chainsim.Block, filter int) (uint64, string) {
	f := chainsim.Filters[filter]
	q := f.Params()
	q["blockHash"] = b.Hash.Hex()
	var logs []logJSON
	if err := s.call(t, &logs, "eth_getLogs", q); err != nil {
		return 0, "error: " + err.Error()
	}
	var want, got []int
	for _, l := range b.Logs() {
		if f.Matches(l) {
			want = append(want, l.LogIndex)
		}
	}
	for _, l := range logs {
		if l.BlockHash != b.Hash || uint64(l.BlockNumber) != b.Number {
			return 0, fmt.Sprintf("log %d of %d %s", l.LogIndex, l.BlockNumber, short(l.BlockHash))
		}
		got = append(got, int(l.LogIndex))
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		return 0, fmt.Sprintf("log indexes %v, want %v", got, want)
	}
	return b.Number, ""
}}

// raReceipt: eth_getTransactionReceipt of N's first transaction answers
// its receipt in N; null only once a reorg took N off the chain.
var raReceipt = raCheck{"receipt", false, func(s *scenario, t int, b *chainsim.Block, _ int) (uint64, string) {
	var raw json.RawMessage
	if err := s.call(t, &raw, "eth_getTransactionReceipt", b.TxHash(0).Hex()); err != nil {
		return 0, "error: " + err.Error()
	}
	if len(raw) == 0 || string(raw) == "null" {
		if cb := s.chain.CanonAt(b.Number); cb == nil || cb.Hash != b.Hash {
			return b.Number, ""
		}
		return 0, "null"
	}
	var r receiptJSON
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, "unparsable: " + err.Error()
	}
	if r.BlockHash != b.Hash || r.TxHash != b.TxHash(0) {
		return 0, fmt.Sprintf("receipt of %s in block %s", short(r.TxHash), short(r.BlockHash))
	}
	return b.Number, ""
}}

// onDeliver sends R1's reads for the first delivery of each block per
// client kind and pod, every read at once, to every pod.
func (ra *readAfter) onDeliver(s *scenario, c *clientBase, pod int, b *chainsim.Block) {
	kind, filter := s.clientKind(c)
	delivered := time.Now()
	ra.mu.Lock()
	if key := fmt.Sprintf("%d/%s", pod, b.Hash.Hex()); ra.own[key].IsZero() {
		ra.own[key] = delivered
	}
	ra.mu.Unlock()
	if !s.first(fmt.Sprintf("r1/%s/%d/%s", kind, pod, b.Hash.Hex())) {
		return
	}
	checks := raChecks
	if b.TxCount() > 0 {
		checks = append(slices.Clone(checks), raReceipt)
	}
	if kind == "logs" {
		checks = append(slices.Clone(checks), raLogs)
	}
	for target := range s.h.pods {
		for _, ck := range checks {
			s.checks.Add(1)
			go func() {
				defer s.checks.Done()
				ra.run(s, c.name, ck, pod, target, b, filter, delivered)
			}()
		}
	}
}

func (ra *readAfter) run(s *scenario, client string, ck raCheck, pod, target int, b *chainsim.Block, filter int, delivered time.Time) {
	side := "same"
	if target != pod {
		side = "other"
	}
	start := time.Now()
	got, problem := ck.read(s, target, b, filter)
	took := time.Since(start)
	where := fmt.Sprintf("pod-%d: %s for block %d %s delivered on pod-%d", target+1, ck.name, b.Number, short(b.Hash), pod+1)
	result := "ok"
	switch {
	case problem != "":
		result = "fail"
		s.violate(client, "%s: %s", where, problem)
	case !ck.latest && got != b.Number:
		result = "fail"
		s.violate(client, "%s answered %d", where, got)
	case ck.latest && got < b.Number && side == "same":
		result = "fail"
		s.violate(client, "%s answered %d, below the delivered block", where, got)
	case ck.latest && got < b.Number:
		result = "lower"
		g := ra.follow(s, ck, target, b, delivered, start)
		g.blocks = b.Number - got
		ra.mu.Lock()
		ra.gaps = append(ra.gaps, g)
		ra.mu.Unlock()
		if g.caught == 0 || g.trailed > r1CatchUp {
			s.violate(client, "%s", s.h.host.verdict(delivered, time.Now(), fmt.Sprintf("%s answered %d, %d below; a read sent %s after the delivery still answered below it, the first at or above it %s after (bound %s)",
				where, got, g.blocks, g.trailed.Round(time.Millisecond), g.caught.Round(time.Millisecond), r1CatchUp)))
		}
		if own := ra.ownAt(target, b.Hash); !own.IsZero() && delivered.Add(g.trailed).After(own) {
			s.violate(client, "%s: a read sent %s after pod-%d itself delivered the block answered below it", where, delivered.Add(g.trailed).Sub(own).Round(time.Millisecond), target+1)
		}
	}
	if result == "ok" && took > r1ReadBound {
		s.violate(client, "%s", s.h.host.verdict(start, time.Now(), fmt.Sprintf("%s answered after %s (bound %s)", where, took.Round(time.Millisecond), r1ReadBound)))
	}
	ra.mu.Lock()
	st := ra.stats[ck.name+"/"+side]
	if st == nil {
		st = &raStat{}
		ra.stats[ck.name+"/"+side] = st
	}
	switch result {
	case "ok":
		st.ok++
	case "fail":
		st.fail++
	case "lower":
		st.lower++
	}
	st.lat = append(st.lat, took)
	ra.mu.Unlock()
}

// follow reads a lower latest again until it reaches N; first is the send
// time of the read that answered lower.
func (ra *readAfter) follow(s *scenario, ck raCheck, target int, b *chainsim.Block, delivered, first time.Time) raGap {
	g := raGap{check: ck.name, trailed: first.Sub(delivered)}
	for time.Since(delivered) < httpCheckWait {
		time.Sleep(r1Follow)
		sent := time.Now()
		got, problem := ck.read(s, target, b, 0)
		switch {
		case problem == "" && got >= b.Number:
			g.caught = sent.Sub(delivered)
			return g
		case problem == "":
			g.trailed = sent.Sub(delivered)
		}
	}
	return g
}

// report logs R1's counts and latencies per check and side, and the
// lower-latest gaps, to the transcript and r1-stats.txt.
func (ra *readAfter) report(s *scenario) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	var b strings.Builder
	keys := make([]string, 0, len(ra.stats))
	for k := range ra.stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("check/side          ok  fail lower   p50     p99     max\n")
	for _, k := range keys {
		st := ra.stats[k]
		slices.Sort(st.lat)
		fmt.Fprintf(&b, "%-18s %5d %5d %5d %7s %7s %7s\n", k, st.ok, st.fail, st.lower, pctl(st.lat, 50), pctl(st.lat, 99), pctl(st.lat, 100))
		if strings.HasPrefix(k, raReceipt.name+"/") && pctl(st.lat, 99) > r1ReceiptP99 {
			s.violate(s.name, "%s latency p99 %s, bound %s (max %s)", k, pctl(st.lat, 99), r1ReceiptP99, pctl(st.lat, 100))
		}
	}
	if len(ra.gaps) > 0 {
		blocks := map[uint64]int{}
		var trailed, caught []time.Duration
		never := 0
		for _, g := range ra.gaps {
			blocks[g.blocks]++
			trailed = append(trailed, g.trailed)
			if g.caught == 0 {
				never++
				continue
			}
			caught = append(caught, g.caught)
		}
		slices.Sort(trailed)
		slices.Sort(caught)
		fmt.Fprintf(&b, "other pod latest lower: %d (blocks below N: %v); still lower at a read sent p50 %s p99 %s max %s after delivery; at N by a read sent p50 %s p99 %s max %s after; never %d\n",
			len(ra.gaps), blocks, pctl(trailed, 50), pctl(trailed, 99), pctl(trailed, 100), pctl(caught, 50), pctl(caught, 99), pctl(caught, 100), never)
	}
	s.logf("R1 reads:\n%s", b.String())
	_ = os.WriteFile(filepath.Join(s.dir, "r1-stats.txt"), []byte(b.String()), 0o644)
}

const (
	r2Run = 3 * time.Minute
	// r2Every: each pod's HTTP follower reads latest and follows up about
	// this often.
	r2Every = 100 * time.Millisecond
)

// R2: one wss push vendor and three https vendors at 0.25 s blocks, every
// https vendor answering after p20RTT; the selection policy ranks http-a
// first, which serves each block p20Skew after the others (P20-behind's
// skew). 2 pods sharing redis, each with one ws newHeads client. At each
// block N a client receives, and about every r2Every on each pod:
// eth_getBlockByNumber(latest), then at once on the same pod eth_call
// (Multicall3 getBlockNumber()) and eth_getBalance at the answered block
// M, and eth_call at latest. No error and no null; at M both answer M; at
// latest ≥ M, and the block read ≥ N; each within W.
func r2() spec {
	fu := &followUps{stats: map[string]*raStat{}}
	return spec{name: "R2", bound: healthyDelivery, sim: chainsim.Config{BlockTime: liveBlockTime},
		vendors: []vendorSpec{{"ws-b", "vendor-b", true}, {"http-a", "vendor-a", false}, {"http-c", "vendor-c", false}, {"http-d", "vendor-d", false}},
		faults: map[string]chainsim.Faults{
			"ws-b": {Slow: 10 * time.Millisecond}, "http-a": {Slow: p20RTT, HeadDelay: p20Skew},
			"http-c": {Slow: p20RTT}, "http-d": {Slow: p20RTT},
		},
		network: map[string]interface{}{"selectionPolicy": rankFirst("http-a")},
		clients: func(s *scenario) {
			for pod := range s.h.pods {
				s.addHeadsOn(fmt.Sprintf("r2-heads-%d", pod+1), pod, "", true)
			}
		},
		onHead: fu.onHead,
		body: func(s *scenario) {
			s.blocks(10)
			ctx, cancel := context.WithTimeout(s.ctx, r2Run)
			defer cancel()
			var wg sync.WaitGroup
			for pod := range s.h.pods {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						sleepCtx(ctx, r2Every/2+rand.N(r2Every))
						if ctx.Err() != nil {
							return
						}
						fu.follow(s, "http", pod, 0)
					}
				}()
			}
			wg.Wait()
		},
		after: fu.report,
	}
}

// followUps keeps R2's counts per trigger and check.
type followUps struct {
	mu    sync.Mutex
	stats map[string]*raStat // "<trigger>/<check>"
}

func (fu *followUps) onHead(s *scenario, _ *headsClient, pod int, n uint64, h hash) {
	if !s.first(fmt.Sprintf("r2/%d/%s", pod, h.Hex())) {
		return
	}
	s.checks.Add(1)
	go func() {
		defer s.checks.Done()
		fu.follow(s, "newHeads", pod, n)
	}()
}

// follow reads pod's latest block, then at once the follow-ups at it; the
// block read must reach floor (0 none).
func (fu *followUps) follow(s *scenario, trigger string, pod int, floor uint64) {
	where := fmt.Sprintf("pod-%d after %s", pod+1, trigger)
	start := time.Now()
	n, h, err := viewRead(s, pod, viewLatest)
	took := time.Since(start)
	if err != nil {
		fu.note(s, trigger, "block-latest", start, took, fmt.Sprintf("%s: eth_getBlockByNumber(latest): %v", where, err))
		return
	}
	problem := ""
	if cb := s.chain.Block(h); cb == nil || cb.Number != n {
		problem = fmt.Sprintf("%s: eth_getBlockByNumber(latest) answered %d %s, no block of the chain at that number", where, n, short(h))
	} else if n < floor {
		problem = fmt.Sprintf("%s: eth_getBlockByNumber(latest) answered %d, below the delivered %d", where, n, floor)
	}
	fu.note(s, trigger, "block-latest", start, took, problem)
	at := hexutil.EncodeUint64(n)
	checks := []struct {
		name  string
		exact bool // answers n; else at least n
		run   func() (uint64, string)
	}{
		{"call-at-block", true, func() (uint64, string) { return raCallAt(s, pod, at) }},
		{"balance-at-block", true, func() (uint64, string) { return r2BalanceAt(s, pod, at) }},
		{"call-latest", false, func() (uint64, string) { return raCallAt(s, pod, "latest") }},
	}
	var wg sync.WaitGroup
	for _, ck := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			got, problem := ck.run()
			took := time.Since(start)
			switch {
			case problem != "":
				problem = fmt.Sprintf("%s: %s at %d: %s", where, ck.name, n, problem)
			case ck.exact && got != n:
				problem = fmt.Sprintf("%s: %s at %d executed at %d", where, ck.name, n, got)
			case !ck.exact && got < n:
				problem = fmt.Sprintf("%s: %s executed at %d, below the block read %d", where, ck.name, got, n)
			}
			fu.note(s, trigger, ck.name, start, took, problem)
		}()
	}
	wg.Wait()
}

// note counts one read and flags its problem, or its wait past W.
func (fu *followUps) note(s *scenario, trigger, check string, start time.Time, took time.Duration, problem string) {
	switch {
	case problem != "":
		s.violate(s.name, "%s", problem)
	case took > r1ReadBound:
		s.violate(s.name, "%s", s.h.host.verdict(start, start.Add(took), fmt.Sprintf("%s after %s answered after %s (bound %s)", check, trigger, took.Round(time.Millisecond), r1ReadBound)))
	}
	fu.mu.Lock()
	defer fu.mu.Unlock()
	st := fu.stats[trigger+"/"+check]
	if st == nil {
		st = &raStat{}
		fu.stats[trigger+"/"+check] = st
	}
	if problem != "" {
		st.fail++
	} else {
		st.ok++
	}
	st.lat = append(st.lat, took)
}

// r2BalanceAt is the balance pod answers at block reference at: the number
// of the block it read state at (chainsim's eth_getBalance).
func r2BalanceAt(s *scenario, pod int, at interface{}) (uint64, string) {
	var out hexutil.Big
	if err := s.call(pod, &out, "eth_getBalance", chainsim.Multicall3.Hex(), at); err != nil {
		return 0, "error: " + err.Error()
	}
	return out.ToInt().Uint64(), ""
}

// report logs R2's counts and latencies to the transcript and r2-stats.txt.
func (fu *followUps) report(s *scenario) {
	fu.mu.Lock()
	defer fu.mu.Unlock()
	var b strings.Builder
	keys := make([]string, 0, len(fu.stats))
	for k := range fu.stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("trigger/check                ok  fail   p50     p99     max\n")
	for _, k := range keys {
		st := fu.stats[k]
		slices.Sort(st.lat)
		fmt.Fprintf(&b, "%-26s %5d %5d %7s %7s %7s\n", k, st.ok, st.fail, pctl(st.lat, 50), pctl(st.lat, 99), pctl(st.lat, 100))
	}
	s.logf("R2 reads:\n%s", b.String())
	_ = os.WriteFile(filepath.Join(s.dir, "r2-stats.txt"), []byte(b.String()), 0o644)
}

// pctl is the p-th percentile of sorted ds, rounded to the millisecond.
func pctl(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	return ds[min(len(ds)-1, len(ds)*p/100)].Round(time.Millisecond)
}

// clientKind names c's driver, and its filter for a logs client.
func (s *scenario) clientKind(c *clientBase) (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.logs {
		if &l.clientBase == c {
			return "logs", l.filter
		}
	}
	for _, g := range s.grpcs {
		if &g.clientBase == c {
			return "grpc", 0
		}
	}
	return "heads", 0
}
