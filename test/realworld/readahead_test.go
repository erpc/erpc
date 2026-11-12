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

	"github.com/erpc/erpc/architecture/evm/integrity"
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
	// p23SplitRate: split reads a second per pod (p23SplitReads).
	p23SplitRate = 10.0
	// p23SplitBlocks: each split read's range.
	p23SplitBlocks = 8
	// p23SplitKind tallies the split reads.
	p23SplitKind = "eth_getLogs split"
	// p23ExtraAsks: a new-image pod's upstream asks for eth_getLogs beyond
	// one per client read (a split read's pieces count as one ask), per
	// read the logs gate checked, at most this: it asks one holder at a
	// time, the next on an error, a null or its hedge delay (spec §6.4
	// logs gate).
	p23ExtraAsks = 1.3
	// p23OldRun: the old split reads' run (p23OldReads).
	p23OldRun = 20 * time.Second
	// p23BusyRate: busy reads a second per pod (p23BusyReads).
	p23BusyRate = 10.0
	// p23AbsentKind tallies the busy reads filtered by p23AbsentAddr.
	p23AbsentKind = "eth_getLogs absent"
	// p23AbsentAddr: no log of any simulated chain names it (chainsim
	// Addrs), so its filter misses every header's bloom but by chance.
	p23AbsentAddr = "0x00000000000000000000000000000000000000b1"
	// p23BusyNo: on a chain whose header blooms hold every log, at least
	// this share of the absent reads' new-block verdicts are no. Before
	// bloom trust, only empty blocks' were (1 in 8 of chainsim's), and a
	// checked read, held toward W, shares its verdict with the identical
	// reads that join it: 0.553-0.596 on main 4e30a0a2, 0.990 here.
	p23BusyNo = 0.9
	// p23ThinNo: on a chain whose header blooms omit logs, at most this
	// share: trust is never earned, so only empty blocks' are no (0.460 on
	// main, 0.603 with trust on demand); an earned trust rules out 0.97.
	p23ThinNo = 0.8
	// p23TrustRun and p23TrustEvery are bloomTrustRun and
	// bloomTrustCheck (spec §11 bloom trust): a trust run fetches at
	// most p23TrustRun blocks' receipts, and one starts at most every
	// p23TrustEvery.
	p23TrustRun   = 8
	p23TrustEvery = time.Minute
	// p23TagRate: P23-busy-off's skip-interpolation eth_call(latest) reads
	// a second per pod (p23BusyReads), tag requests the state prober
	// probes the upstreams of while it runs.
	p23TagRate = 1.0
	// p23TagKind tallies those reads.
	p23TagKind = "eth_call tag"
)

// p23Verdicts are the logs gate's verdicts (erpc_logs_empty_gate_total);
// p23Checked those of the reads it checked by asking other holders.
var (
	p23Verdicts = []string{"old", "no", "maybe_alone", "maybe_agreed", "maybe_found", "maybe_reasked"}
	p23Checked  = []string{"maybe_agreed", "maybe_found", "maybe_reasked"}
)

// p23AuxFetches are the tracker fetch kinds (erpc_chain_tracker_fetch_total)
// no latest tracking sends: the logs gate's logs_hash and bloom_trust, and
// the receipts and logs a logs tap or an integrity check reads.
var p23AuxFetches = []string{"logs_hash", "bloom_trust", "receipts", "logs"}

// p23Origins are the methods a P23-busy-off pod may send its vendors, by
// origin: its client reads' own (client), its state pollers' polls (poll)
// and latest tracking's head and header fetches (latest). Any other is an
// auxiliary request.
var p23Origins = map[string]string{
	"eth_getLogs":          "client",
	"eth_call":             "client",
	"eth_blockNumber":      "client, poll",
	"eth_chainId":          "poll",
	"eth_syncing":          "poll",
	"eth_getBlockByNumber": "poll, latest",
	"eth_getBlockByHash":   "latest",
}

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
// (p23Gate). With split, every vendor splits an eth_getLogs range longer
// than split blocks into pieces pinned to it
// (getLogsAutoSplittingRangeThreshold), and the body is the tip logs reads
// alone (p23SplitBody). With lone, the body is those reads unsplit, on
// vendors no other of which holds the tip within W. With busy, the body is
// the busy reads alone (p23BusyBody); with thin, on a chain whose header
// blooms omit logs (chainsim Config.ThinBloom). Every shape's network
// enables the logs gate's check alone (integrity.GetLogsEmptyGate, the
// config that keeps the gate where integrity is otherwise off), on its
// new-image pods; with off, no integrity at all (p23OffBody).
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
	split             int64
	lone              bool
	busy, thin, off   bool
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
// this branch: its eth_getLogs p99, no worse than there plus 5%, judged
// against it instead of the old image's; no refusal Ask at all, since its
// probe asks only upstreams whose evidence holds the block; and at most
// p23ExtraAsks per checked read (p23Cost).
type p23Gate struct {
	logsP99 time.Duration
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

// p23Split: P23's chain and vendors, each splitting an eth_getLogs range
// longer than 2 blocks into pieces pinned to it, read over 8 blocks ending
// at the tip with a filter the held blooms call maybe that matches no log
// (p23SplitReads). A pinned piece may ask no other upstream: gated as a
// read with peers, its empty answer waited W for a holder it could not
// reserve, then failed, and so did its upstream's whole split, upstream
// after upstream. The vendors are polled every block, so other holders of
// the tip exist when the parent's merged empty answer is gated (p23Lone
// reads with none).
var p23Split = p23Shape{name: "P23-split", bt: 100 * time.Millisecond, jitter: 20 * time.Millisecond, warm: 300,
	ahead: time.Minute, split: 2,
	ups: []freshUp{
		{"http-a", "vendor-a", 20 * time.Millisecond, 100 * time.Millisecond, 0, 0},
		{"http-b", "vendor-b", 25 * time.Millisecond, 100 * time.Millisecond, 0, 0},
		{"http-c", "vendor-c", 30 * time.Millisecond, 100 * time.Millisecond, 0, 0},
		{"http-d", "vendor-d", 35 * time.Millisecond, 100 * time.Millisecond, 0, 0},
		{"http-e", "vendor-e", 40 * time.Millisecond, 100 * time.Millisecond, 0, 0},
		{"http-f", "vendor-f", 45 * time.Millisecond, 100 * time.Millisecond, 0, 0},
	}}

// p23Lone: p23Split's reads, unsplit, on P23's vendors, polled every 10 s:
// the clock's one fetch a block leaves no other holder of the tip within
// W, so a gated read's probe finds none and asks the first upstream again
// (maybe_reasked). Ending in a block-unavailable error instead, 451 of 605
// such reads failed.
var p23Lone = p23Shape{name: "P23-lone", bt: 100 * time.Millisecond, jitter: 20 * time.Millisecond, warm: 300,
	ahead: time.Minute, lone: true, ups: p23Fast.ups}

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
// serves the block by number for 250 ms; its eth_getLogs, by range or
// blockHash, answer [] for the block until vendor-c serves it too, as a
// vendor whose log index trails its head. Main's retry waited for the
// block clock's next turn: by-number reads 0.56 s later than the first
// vendor's serve at p50 (production, mantle and optimism: retried_found
// reads 1-3 s on 2 s blocks). No read can answer before a vendor serves
// the block. A read at or below Latest of a block vendor-a's tag
// announced fails while no vendor serves it, on main as here. The bars of
// those checks (pre) are 172a2ec7's highest plus 5%, from earlier runs.
// Runs with this harness, alternated, 4 each, main cd3e82d4 then this
// branch: wrong empty eth_getLogs per new pod 917-934 on main, 0 here;
// eth_getBlockByNumber failed on 4.02%, 4.14%, 4.25%, 3.79% of main's
// reads, 3.74%, 4.17%, 3.82%, 4.73% here, p99 629, 662, 351, 590 ms on
// main, 950, 691, 664, 708 ms here; served_missing per pod at most 57,
// 56, 59, 53 on main, 52, 63, 63, 67 here; no null at or below Latest on
// either. Head fetches per new block, polls left out, the higher pod of
// each run, at most 1.933 on main, 1.978 here (budget). The logs gate's
// own costs (gate), its parallel probe of the other holders: no refusal
// Ask in any run (0.000 per new block on every pod); eth_getLogs p99 719,
// 742, 759 ms (978 ms in a run that started at host load 17.5), against
// main's 23-24 ms, which served the wrong empty answers.
var p23Band = p23Shape{name: "P23-band", bt: 2 * time.Second, jitter: 200 * time.Millisecond, warm: 25,
	base: 2 * time.Minute, hash: 2 * time.Minute, ahead: 3 * time.Minute,
	blockLag: 300 * time.Millisecond, logsLag: 500 * time.Millisecond, budget: 2.00, old: true, peer: true,
	pre:  &p23Pre{p99: 683 * time.Millisecond, failed: 0.0463, servedMissing: 70},
	gate: &p23Gate{logsP99: 759 * time.Millisecond},
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

// p23Busy: P23-band's chain and vendors, vendor-a's log index trailing its
// head by 500 ms, every block but 1 in 8 with logs, so a non-zero bloom,
// and no client reads receipts: as a busy production chain, whose tracker
// fetched none in 20 min, so its bloom was never trusted and every recent
// empty eth_getLogs answer of a block with transactions was checked
// (optimism: 36% of its log reads asked a second upstream). Its reads
// are at each pod's tip, filtered by an address no log holds
// (p23BusyReads). On main 4e30a0a2, 2 runs, the gate checked every such
// read of a block with transactions: absent reads p50 2.12-2.21 s, p90
// 5.37 s, p99 6.04 s (their control 21 ms), 0.553-0.596 of their
// new-block verdicts no, no receipts fetched. With trust on demand: p50
// 21 ms, p90 24 ms, p99 5.21 s (17 absent-read verdicts still checked,
// with the reads that joined them), 0.990 no, 10 trust fetches in 3 min.
var p23Busy = p23Shape{name: "P23-busy", bt: 2 * time.Second, jitter: 200 * time.Millisecond, warm: 25,
	ahead: 3 * time.Minute, logsLag: 500 * time.Millisecond, busy: true, ups: p23Band.ups}

// p23BusyThin: P23-busy on a chain whose header blooms leave out the logs
// of one address its blocks hold: receipts never prove such a bloom, so
// trust is never earned, and a read filtered by that address, which
// vendor-a answers [] while its log index trails, is checked, never ruled
// out by the header.
var p23BusyThin = p23Shape{name: "P23-busy-thin", bt: 2 * time.Second, jitter: 200 * time.Millisecond, warm: 25,
	ahead: 3 * time.Minute, logsLag: 500 * time.Millisecond, busy: true, thin: true, ups: p23Band.ups}

// p23BusyOff: P23-busy on a network whose integrity is off, its reads
// beside a skip-interpolation eth_call(latest) a second (p23TagRate), a
// tag request the state prober would probe for (spec §6.11 Probe), and no
// WebSocket client: the gate judges nothing and asks no other vendor, and
// the pod sends no auxiliary request (p23OffBody). On main d9483b54 the
// gate judged these reads, earned bloom trust by receipts fetches and
// probed each vendor's latest state.
var p23BusyOff = p23Shape{name: "P23-busy-off", bt: 2 * time.Second, jitter: 200 * time.Millisecond, warm: 25,
	ahead: 3 * time.Minute, logsLag: 500 * time.Millisecond, busy: true, off: true, ups: p23Band.ups}

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
	sp := freshSpec(sh.name, 1, sh.ups, chainsim.Config{BlockTime: sh.bt, MintJitter: sh.jitter, ThinBloom: sh.thin}, sh.faults, func(s *scenario) { p23Body(s, sh) })
	sp.pods, sp.oldPods = 1, nil
	if !sh.off {
		sp.checks = []string{integrity.GetLogsEmptyGate}
	}
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
	if sh.split > 0 {
		for _, ev := range sp.upEvm {
			ev["getLogsAutoSplittingRangeThreshold"] = sh.split
		}
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

// logsReads counts r's client eth_getLogs reads of every kind, answered,
// null or failed.
func (r *p23Reads) logsReads() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int
	for _, m := range []map[string]int{r.nulls, r.fails} {
		for k, v := range m {
			if strings.HasPrefix(k, "eth_getLogs") {
				n += v
			}
		}
	}
	for k, ds := range r.took {
		if strings.HasPrefix(k, "eth_getLogs") {
			n += len(ds)
		}
	}
	return float64(n)
}

func p23Body(s *scenario, sh p23Shape) {
	// The bar waits until the block time estimate settled (sh.warm).
	s.blocks(sh.warm)
	vl := &viewLog{}
	// Both latest reads: eth_blockNumber alone is no clock demand.
	heads := liveReads(p23HeadRate, s.chain.BlockTime(), vl)
	if sh.split > 0 || sh.lone {
		s.p23SplitBody(sh, heads)
		return
	}
	if sh.busy {
		s.p23BusyBody(sh, heads)
		return
	}
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
	// With the gate's bars, each block's logs are read by its blockHash
	// too, as the first vendor's tag names it: a hash no tracker holds yet.
	hashCalls0, hashGates0 := s.p23LogsCalls(), s.p23Gates()
	_, byHash, _, _, _ := s.liveRun(sh.hash, heads, func(ctx context.Context, _ *faultWindows) {
		var wg sync.WaitGroup
		for _, p := range s.h.pods {
			wg.Go(func() { s.p23ByHash(ctx, p.idx, hashReads[p.idx], vl, sh.gate != nil) })
		}
		wg.Wait()
	})
	hashCalls1, hashGates1 := s.p23LogsCalls(), s.p23Gates()
	ask2 := s.p23Asks()
	s.judgeFetches(&b, "by-hash reads", byHash, 0)

	missing0 := make([]float64, len(s.h.pods))
	for pod := range s.h.pods {
		missing0[pod], _ = s.metric(pod, "erpc_head_refusal_total", map[string]string{"rule": "retried_missing"})
	}
	calls0, gates0 := s.p23LogsCalls(), s.p23Gates()
	_, ahead, _, _, _ := s.liveRun(sh.ahead, heads, func(ctx context.Context, _ *faultWindows) {
		var wg sync.WaitGroup
		for _, p := range s.h.pods {
			wg.Go(func() { s.p23ByNumber(ctx, p.idx, reads[p.idx], sh) })
			wg.Go(func() { s.p23ByHash(ctx, p.idx, reads[p.idx], vl, false) })
		}
		wg.Wait()
	})
	calls1, gates1 := s.p23LogsCalls(), s.p23Gates()
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
		spent := ahead.perBlock(p.idx) - polls
		if sh.gate != nil && judged && asks > 0 {
			s.violate(s.name, "pod-%d: %.3f refusal Asks per new block with read-ahead clients: the logs gate probes holders only, with no Ask",
				p.idx+1, asks)
		}
		if judged && spent > budget {
			s.violate(s.name, "pod-%d: %.3f head fetches per new block with read-ahead clients, polls left out, above %.2f",
				p.idx+1, spent, budget)
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
		for _, v := range p23Verdicts {
			gate = append(gate, fmt.Sprintf("%s %.0f", v, gates1[p.idx][v]))
		}
		for _, ph := range []struct {
			name string
			r    *p23Reads
		}{{"by-hash reads", hashReads[p.idx]}, {"read-ahead", reads[p.idx]}} {
			fmt.Fprintf(&b, "pod-%d (%s image) %s: eth_getLogs answered [] for a block with matching logs (wrongEmpty): %d\n",
				p.idx+1, s.image(p.idx), ph.name, ph.r.wrongEmpty)
			if !p.old && ph.r.wrongEmpty > 0 {
				s.violate(s.name, "pod-%d %s: %d eth_getLogs answered [] for a block with matching logs", p.idx+1, ph.name, ph.r.wrongEmpty)
			}
		}
		fmt.Fprintf(&b, "pod-%d (%s image) logs gate verdicts: %s\n", p.idx+1, s.image(p.idx), strings.Join(gate, ", "))
		if !p.old {
			s.judgeView(&b, taps[p.idx], vl, viewRules{bound: liveViewBound, numbers: true})
		}
	}
	if sh.gate != nil {
		s.p23Cost(&b, "by-hash reads", hashCalls0, hashCalls1, hashGates0, hashGates1, hashReads, 1, true)
		for _, p := range s.h.pods {
			fmt.Fprintf(&b, "pod-%d (%s image) by-hash reads: logs gate header lookups found %.0f, found none %.0f (checked at the block after the head)\n",
				p.idx+1, s.image(p.idx), hashGates1[p.idx][p23LookupFound]-hashGates0[p.idx][p23LookupFound], hashGates1[p.idx][p23LookupNone]-hashGates0[p.idx][p23LookupNone])
		}
	}
	s.p23Cost(&b, "read-ahead", calls0, calls1, gates0, gates1, reads, 1, sh.gate != nil)
	if sh.old {
		// By-hash reads above Latest go out as they came (§6.4 by-hash
		// answer), as the old image's: shown, not judged here.
		s.p23Versus(&b, "by-hash reads", hashReads, p23HashMethods, sh.slack, false, nil, nil)
		s.p23Versus(&b, "read-ahead", reads, p23HashMethods, sh.slack, false, nil, nil)
		s.p23Versus(&b, "read-ahead", reads, p23NumberMethods, sh.slack, sh.peer, sh.pre, sh.gate)
		s.p23Attempts(&b, ahead, reads, sh.peer)
		s.p23Nulls(&b, reads, sh.peer, sh.pre)
		s.p23HashLogs(&b, reads)
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

// p23SplitBody runs the tip logs reads (p23SplitReads) on each pod beside
// heads for sh.ahead, then judges each pod: every read answers the
// chain's logs, none failed. Split, p99 latency at most a quarter of W,
// since a pinned piece's empty answer is served at once (maybe_alone), not
// after the probe's wait, no maybe_reasked verdict, and at most
// p23ExtraAsks per checked read (p23Cost); then the old split reads
// (p23OldReads). Lone, a gated read waits W for a holder, then re-asks:
// p99 at most W plus two round trips of the slowest vendor (the first
// answer, the re-ask) plus p23Noise, and some maybe_reasked verdicts, else
// no read met a tip no other upstream held.
func (s *scenario) p23SplitBody(sh p23Shape, heads []liveRead) {
	pieces := 1.0
	if sh.split > 0 {
		pieces = float64((p23SplitBlocks + sh.split - 1) / sh.split)
	}
	reads := make([]*p23Reads, len(s.h.pods))
	for _, p := range s.h.pods {
		reads[p.idx] = newP23Reads()
	}
	calls0, gates0 := s.p23LogsCalls(), s.p23Gates()
	_, _, _, from, to := s.liveRun(sh.ahead, heads, func(ctx context.Context, _ *faultWindows) {
		var wg sync.WaitGroup
		for _, p := range s.h.pods {
			wg.Go(func() { s.p23SplitReads(ctx, p.idx, reads[p.idx], 0) })
		}
		wg.Wait()
	})
	calls1, gates1 := s.p23LogsCalls(), s.p23Gates()
	w := max(3*s.chain.BlockTime(), liveBound)
	bar, why := w/4, "a pinned piece's empty answer waited"
	if sh.lone {
		var rtt time.Duration
		for _, u := range sh.ups {
			rtt = max(rtt, u.slow)
		}
		bar, why = w+2*rtt+p23Noise, "a gated read waited past W and its re-ask"
	}
	var b strings.Builder
	for _, p := range s.h.pods {
		r := reads[p.idx]
		s.p23Judge(&b, p.idx, "split reads", r, nil, true, nil)
		delta := map[string]float64{}
		var line []string
		for _, v := range p23Verdicts {
			delta[v] = gates1[p.idx][v] - gates0[p.idx][v]
			line = append(line, fmt.Sprintf("%s %.0f", v, delta[v]))
		}
		fmt.Fprintf(&b, "pod-%d %s reads: logs gate verdicts %s\n", p.idx+1, sh.name, strings.Join(line, ", "))
		ds := slices.Sorted(slices.Values(r.took[p23SplitKind]))
		switch {
		case len(ds) == 0:
			s.violate(s.name, "pod-%d: no split read answered", p.idx+1)
		case pctl(ds, 99) > bar:
			s.violate(s.name, "%s", s.h.host.verdict(from, to, fmt.Sprintf("pod-%d: tip logs reads p99 %s, above %s (W %s): %s", p.idx+1, pctl(ds, 99), bar, w, why)))
		}
		switch {
		case sh.lone && delta["maybe_reasked"] == 0:
			s.violate(s.name, "pod-%d: no maybe_reasked verdict: no read met a tip no other upstream held", p.idx+1)
		case !sh.lone && delta["maybe_reasked"] > 0:
			s.violate(s.name, "pod-%d: %.0f maybe_reasked verdicts on split reads: a pinned piece has no peer to ask", p.idx+1, delta["maybe_reasked"])
		}
	}
	s.p23Cost(&b, sh.name+" reads", calls0, calls1, gates0, gates1, reads, pieces, !sh.lone)
	if !sh.lone {
		s.p23OldReads(&b, heads, pieces, w)
	}
	s.logf("%s", b.String())
}

// p23OldReads runs p23SplitReads for p23OldRun over ranges ending 2 W
// below each pod's eth_blockNumber, with the same filter: the blooms call
// it maybe, it matches no log. Each new-image pod is judged: every read
// answers the chain's logs (none), none fails; the logs gate judges only
// blocks newer than W (spec §6.4 logs gate), so each read counts old
// verdicts (its pieces' and its own), none of another verdict, and the
// vendors get no eth_getLogs call beyond the reads' own pieces.
func (s *scenario) p23OldReads(b *strings.Builder, heads []liveRead, pieces float64, w time.Duration) {
	back := uint64(2 * w / s.chain.BlockTime())
	reads := make([]*p23Reads, len(s.h.pods))
	for _, p := range s.h.pods {
		reads[p.idx] = newP23Reads()
	}
	calls0, gates0 := s.p23LogsCalls(), s.p23Gates()
	s.liveRun(p23OldRun, heads, func(ctx context.Context, _ *faultWindows) {
		var wg sync.WaitGroup
		for _, p := range s.h.pods {
			wg.Go(func() { s.p23SplitReads(ctx, p.idx, reads[p.idx], back) })
		}
		wg.Wait()
	})
	calls1, gates1 := s.p23LogsCalls(), s.p23Gates()
	for _, p := range s.h.pods {
		if p.old {
			continue
		}
		r := reads[p.idx]
		s.p23Judge(b, p.idx, "old split reads", r, nil, true, nil)
		n := r.logsReads()
		extra := calls1[p.idx].calls - calls0[p.idx].calls - pieces*n
		var line []string
		var judged float64
		for _, v := range p23Verdicts {
			d := gates1[p.idx][v] - gates0[p.idx][v]
			line = append(line, fmt.Sprintf("%s %.0f", v, d))
			if v != "old" {
				judged += d
			}
		}
		old := gates1[p.idx]["old"] - gates0[p.idx]["old"]
		fmt.Fprintf(b, "pod-%d old split reads, %d blocks below the tip (W %s): %.0f reads, %.0f eth_getLogs calls beyond their pieces; logs gate verdicts %s\n",
			p.idx+1, back, w, n, extra, strings.Join(line, ", "))
		switch {
		case n == 0:
			s.violate(s.name, "pod-%d: no old split read", p.idx+1)
		case old < n:
			s.violate(s.name, "pod-%d: %.0f old verdicts for %.0f old split reads: a block that reached the view head W ago was judged", p.idx+1, old, n)
		}
		if judged > 0 {
			s.violate(s.name, "pod-%d: %.0f logs gate verdicts other than old on old split reads", p.idx+1, judged)
		}
		if extra > 0 {
			s.violate(s.name, "pod-%d: %.0f eth_getLogs calls beyond the old split reads' own pieces: an old block's empty answer asked other holders", p.idx+1, extra)
		}
	}
}

// p23BusyBody runs the busy reads (p23BusyReads) on each pod beside heads
// for sh.ahead, then judges each pod: every read answers the chain's logs,
// none failed, none answered [] for a block with matching logs
// (wrongEmpty); at most p23ExtraAsks per checked read (p23Cost). Bloom
// trust (spec §11): the gate's first demand starts a trust run of at most
// p23TrustRun receipts fetches, and while the gate relies on trust one
// block is checked again at most every p23TrustEvery. The gate's verdicts
// on new blocks, less one per filtered read, which the bloom may hold, are
// the absent reads' (one per upstream request: identical reads in flight
// share it). With blooms that hold every log, at least p23BusyNo of them
// are no; an absent read at most one round trip of its control, a held
// block's, later at p50 and p90 (a checked read holds toward W while no
// other vendor's poll shows the block, and identical reads join it); at
// most p23TrustRun fetches plus one a p23TrustEvery. Thin, at most
// p23ThinNo of them are no, and at most p23TrustRun fetches a
// p23TrustEvery.
func (s *scenario) p23BusyBody(sh p23Shape, heads []liveRead) {
	if sh.off {
		s.p23OffBody(sh, heads)
		return
	}
	reads := make([]*p23Reads, len(s.h.pods))
	for _, p := range s.h.pods {
		reads[p.idx] = newP23Reads()
	}
	calls0, gates0, trust0, held0 := s.p23LogsCalls(), s.p23Gates(), s.p23Fetches("bloom_trust"), s.p23Fetches("receipts")
	s.liveRun(sh.ahead, heads, func(ctx context.Context, _ *faultWindows) {
		var wg sync.WaitGroup
		for _, p := range s.h.pods {
			wg.Go(func() { s.p23BusyReads(ctx, p.idx, reads[p.idx]) })
		}
		wg.Wait()
	})
	calls1, gates1, trust1, held1 := s.p23LogsCalls(), s.p23Gates(), s.p23Fetches("bloom_trust"), s.p23Fetches("receipts")
	checks := float64(sh.ahead/p23TrustEvery) + 1
	var b strings.Builder
	for _, p := range s.h.pods {
		r := reads[p.idx]
		s.p23Judge(&b, p.idx, sh.name+" reads", r, nil, true, nil)
		var line []string
		for _, v := range p23Verdicts {
			line = append(line, fmt.Sprintf("%s %.0f", v, gates1[p.idx][v]-gates0[p.idx][v]))
		}
		r.mu.Lock()
		absent := float64(len(r.took[p23AbsentKind]) + r.fails[p23AbsentKind])
		filtered := float64(len(r.took["eth_getLogs filtered"]) + r.fails["eth_getLogs filtered"])
		wrong := r.wrongEmpty
		ds := slices.Sorted(slices.Values(r.took[p23AbsentKind]))
		rtt := pctl(slices.Sorted(slices.Values(r.took[p23AbsentKind+" control"])), 50)
		r.mu.Unlock()
		fmt.Fprintf(&b, "pod-%d %s reads: absent p50 %s p90 %s p99 %s; control p50 %s\n", p.idx+1, sh.name, pctl(ds, 50), pctl(ds, 90), pctl(ds, 99), rtt)
		if bound := 2*rtt + p23Noise; !sh.thin && (pctl(ds, 50) > bound || pctl(ds, 90) > bound) {
			s.violate(s.name, "pod-%d: absent reads p50 %s p90 %s, above %s (two control round trips +%s): their header did not rule them out", p.idx+1, pctl(ds, 50), pctl(ds, 90), bound, p23Noise)
		}
		var judged float64
		for _, v := range p23Verdicts {
			if v != "old" {
				judged += gates1[p.idx][v] - gates0[p.idx][v]
			}
		}
		judged -= filtered
		no := (gates1[p.idx]["no"] - gates0[p.idx]["no"]) / max(judged, 1)
		fetches := trust1[p.idx] - trust0[p.idx]
		fmt.Fprintf(&b, "pod-%d %s reads: logs gate verdicts %s; %.0f absent reads, %.0f new-block verdicts of theirs, %.3f of them no; %.0f bloom trust fetches, %.0f other receipts fetches; wrongEmpty %d\n",
			p.idx+1, sh.name, strings.Join(line, ", "), absent, judged, no, fetches, held1[p.idx]-held0[p.idx], wrong)
		if wrong > 0 {
			s.violate(s.name, "pod-%d: %d eth_getLogs answered [] for a block with matching logs", p.idx+1, wrong)
		}
		bar := p23TrustRun + checks
		switch {
		case absent == 0 || judged <= 0:
			s.violate(s.name, "pod-%d: %.0f absent reads, %.0f new-block verdicts of theirs", p.idx+1, absent, judged)
		case sh.thin && no > p23ThinNo:
			s.violate(s.name, "pod-%d: headers ruled out %.3f of the absent reads' new-block verdicts, above %.2f: bloom trust earned on a chain whose blooms omit logs", p.idx+1, no, p23ThinNo)
		case !sh.thin && no < p23BusyNo:
			s.violate(s.name, "pod-%d: headers ruled out %.3f of the absent reads' new-block verdicts, below %.2f: bloom trust not earned on a busy chain", p.idx+1, no, p23BusyNo)
		}
		if sh.thin {
			bar = p23TrustRun * checks
		}
		if fetches > bar {
			s.violate(s.name, "pod-%d: %.0f bloom trust fetches in %s, above %.0f", p.idx+1, fetches, sh.ahead, bar)
		}
	}
	s.p23Cost(&b, sh.name+" reads", calls0, calls1, gates0, gates1, reads, 1, true)
	s.logf("%s", b.String())
}

// p23OffBody runs the busy reads (p23BusyReads) and the tag reads
// (p23TagReads) on each pod beside heads for sh.ahead, on a network whose
// integrity is off and that no WebSocket client reads, then counts every
// request each pod sent its vendors by origin and judges each pod: the
// logs gate judged no answer; no eth_getLogs went out again beyond each
// client read's own (p23LogsCalls repeats); no tracker fetch of a kind
// latest tracking never sends (p23AuxFetches), no state probe and no
// auxiliary request (erpc_integrity_aux_request_total); and every method
// its vendors got has an origin (p23Origins). Reads are reported only: an
// empty answer is served as it came, as before the gate.
func (s *scenario) p23OffBody(sh p23Shape, heads []liveRead) {
	reads := make([]*p23Reads, len(s.h.pods))
	for _, p := range s.h.pods {
		reads[p.idx] = newP23Reads()
	}
	// counts are, per pod, its aux signals and its vendor requests by
	// method ("method " keys), from one scrape.
	counts := func() []map[string]float64 {
		out := make([]map[string]float64, len(s.h.pods))
		for _, p := range s.h.pods {
			c := map[string]float64{}
			out[p.idx] = c
			for _, px := range s.proxies {
				for m, n := range px.methodReads(p) {
					c[methodRead(m)] += float64(n)
				}
			}
			ss, err := p.scrape()
			if err != nil {
				s.violate("harness", "pod-%d: scrape: %v", p.idx+1, err)
				continue
			}
			sum := func(name string, labels map[string]string) float64 {
				match := map[string]string{"project": s.project, "network": s.network()}
				maps.Copy(match, labels)
				v, _ := sumOf(ss, name, match)
				return v
			}
			c["logs gate verdicts"] = sum("erpc_logs_empty_gate_total", nil)
			for _, k := range p23AuxFetches {
				c[k+" fetches"] = sum("erpc_chain_tracker_fetch_total", map[string]string{"kind": k})
			}
			c["state probes"] = sum("erpc_upstream_state_probe_total", nil)
			c["integrity aux requests"] = sum("erpc_integrity_aux_request_total", nil)
		}
		return out
	}
	calls0, before := s.p23LogsCalls(), counts()
	s.liveRun(sh.ahead, heads, func(ctx context.Context, _ *faultWindows) {
		var wg sync.WaitGroup
		for _, p := range s.h.pods {
			wg.Go(func() { s.p23BusyReads(ctx, p.idx, reads[p.idx]) })
			wg.Go(func() { s.p23TagReads(ctx, p.idx, reads[p.idx]) })
		}
		wg.Wait()
	})
	calls1, after := s.p23LogsCalls(), counts()
	var b strings.Builder
	for _, p := range s.h.pods {
		r := reads[p.idx]
		s.p23Judge(&b, p.idx, sh.name+" reads", r, nil, false, nil)
		r.mu.Lock()
		wrong := r.wrongEmpty
		r.mu.Unlock()
		n := r.logsReads()
		asks := calls1[p.idx].repeats - calls0[p.idx].repeats
		fmt.Fprintf(&b, "pod-%d %s reads: %.0f eth_getLogs reads, %.0f vendor eth_getLogs calls, %.0f extra asks; wrongEmpty %d (served as it came)\n",
			p.idx+1, sh.name, n, calls1[p.idx].calls-calls0[p.idx].calls, asks, wrong)
		if asks > 0 {
			s.violate(s.name, "pod-%d: %.0f eth_getLogs asks beyond the client reads' own with integrity off", p.idx+1, asks)
		}
		for _, k := range slices.Sorted(maps.Keys(after[p.idx])) {
			d := after[p.idx][k] - before[p.idx][k]
			m, isMethod := strings.CutPrefix(k, methodRead(""))
			if !isMethod {
				fmt.Fprintf(&b, "pod-%d %s: %s %.0f\n", p.idx+1, sh.name, k, d)
				if d > 0 {
					s.violate(s.name, "pod-%d: %.0f %s with integrity off", p.idx+1, d, k)
				}
				continue
			}
			origin, ok := p23Origins[m]
			if !ok {
				origin = "auxiliary"
			}
			fmt.Fprintf(&b, "pod-%d %s: vendor requests %s %.0f (%s)\n", p.idx+1, sh.name, m, d, origin)
			if !ok && d > 0 {
				s.violate(s.name, "pod-%d: %.0f %s requests to vendors with integrity off: no client, poll or latest tracking sends it", p.idx+1, d, m)
			}
		}
	}
	s.logf("%s", b.String())
}

// p23TagReads reads on pod, about p23TagRate times a second until ctx
// ends, a skip-interpolation eth_call(latest) to Multicall3
// getBlockNumber(), tallied as p23TagKind: a tag request (spec §6.11
// Applies to), so each vendor it reaches is one the state prober probes.
func (s *scenario) p23TagReads(ctx context.Context, pod int, r *p23Reads) {
	c, err := rpc.DialOptions(ctx, s.h.pods[pod].httpURL(s.project, s.chain.ID()),
		rpc.WithHTTPClient(s.h.podClient()), rpc.WithHeader("X-ERPC-Skip-Interpolation", "true"))
	if err != nil {
		s.violate("harness", "pod-%d: tag client: %v", pod+1, err)
		return
	}
	defer c.Close()
	period := time.Duration(float64(time.Second) / p23TagRate)
	call := map[string]interface{}{"to": chainsim.Multicall3.Hex(), "data": chainsim.GetBlockNumberSig}
	for ctx.Err() == nil {
		sleepCtx(ctx, period/2+rand.N(period))
		cctx, cancel := context.WithTimeout(context.Background(), httpCheckWait)
		var res hexutil.Bytes
		start := time.Now()
		err := c.CallContext(cctx, &res, "eth_call", call, "latest")
		cancel()
		if err != nil {
			r.fail(p23TagKind, "eth_call(latest) failed after %s: %v", time.Since(start).Round(time.Millisecond), err)
			continue
		}
		r.ok(p23TagKind, time.Since(start))
	}
}

// p23BusyReads reads on pod, about p23BusyRate times a second until ctx
// ends, the block of the pod's eth_blockNumber filtered by p23AbsentAddr
// (p23Absent), beside it the same read of the block p23Back below as
// control, which the logs gate counts old, and each new block's first-log
// filter (p23Filtered), which its bloom may hold.
func (s *scenario) p23BusyReads(ctx context.Context, pod int, r *p23Reads) {
	c, err := rpc.DialOptions(ctx, s.h.pods[pod].httpURL(s.project, s.chain.ID()), rpc.WithHTTPClient(s.h.podClient()))
	if err != nil {
		s.violate("harness", "pod-%d: busy client: %v", pod+1, err)
		return
	}
	defer c.Close()
	call := func(result interface{}, method string, args ...interface{}) error {
		ctx, cancel := context.WithTimeout(context.Background(), httpCheckWait)
		defer cancel()
		return c.CallContext(ctx, result, method, args...)
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	period := time.Duration(float64(time.Second) / p23BusyRate)
	var filtered uint64
	for ctx.Err() == nil {
		sleepCtx(ctx, period/2+rand.N(period))
		var tip hexutil.Uint64
		if err := call(&tip, "eth_blockNumber"); err != nil || ctx.Err() != nil {
			continue
		}
		n := uint64(tip)
		wg.Go(func() { p23Absent(call, r, n, p23AbsentKind) })
		if n > p23Back {
			wg.Go(func() { p23Absent(call, r, n-p23Back, p23AbsentKind+" control") })
		}
		if n > filtered {
			filtered = n
			wg.Go(func() { s.p23Filtered(call, r, n, false) })
		}
	}
}

// p23Absent reads eth_getLogs(n, n) filtered by p23AbsentAddr, tallied as
// kind: the answer must be none.
func p23Absent(call func(result interface{}, method string, args ...interface{}) error, r *p23Reads, n uint64, kind string) {
	q := map[string]interface{}{"fromBlock": hexutil.EncodeUint64(n), "toBlock": hexutil.EncodeUint64(n), "address": p23AbsentAddr}
	var logs []json.RawMessage
	start := time.Now()
	err := call(&logs, "eth_getLogs", q)
	took := time.Since(start)
	switch {
	case err != nil:
		r.fail(kind, "eth_getLogs(%d) of %s failed after %s: %v", n, p23AbsentAddr, took.Round(time.Millisecond), err)
	case len(logs) > 0:
		r.fail(kind, "eth_getLogs(%d) of %s answered %d logs, not none, after %s", n, p23AbsentAddr, len(logs), took.Round(time.Millisecond))
	default:
		r.ok(kind, took)
	}
}

// p23Fetches are, per pod, its tracker's fetches of kind
// (erpc_chain_tracker_fetch_total): bloom_trust, a trust check's
// receipts; receipts, every other receipts fetch.
func (s *scenario) p23Fetches(kind string) []float64 {
	out := make([]float64, len(s.h.pods))
	for _, p := range s.h.pods {
		out[p.idx], _ = s.metric(p.idx, "erpc_chain_tracker_fetch_total", map[string]string{"kind": kind})
	}
	return out
}

// p23Cost reports, per pod, the upstream asks for eth_getLogs from calls0
// to calls1 beyond each client read's own: on a new-image pod per read
// the logs gate checked from gates0 to gates1 (p23Checked), on an
// old-image pod per client read. Unsplit (pieces 1), they are the client
// reads' ids sent again (p23Calls.repeats): the tracker answers some reads
// from held logs with no call. Split, the pieces carry ids of their own,
// so they are the calls beyond pieces per client read, in asks of pieces
// calls. Judged, a new-image pod's above p23ExtraAsks is flagged. Retries
// of a band read's miss count too, so this bounds the gate's own asks from
// above.
func (s *scenario) p23Cost(b *strings.Builder, phase string, calls0, calls1 []p23Calls, gates0, gates1 []map[string]float64, reads []*p23Reads, pieces float64, judged bool) {
	for _, p := range s.h.pods {
		n := reads[p.idx].logsReads()
		calls := calls1[p.idx].calls - calls0[p.idx].calls
		asks := calls1[p.idx].repeats - calls0[p.idx].repeats
		if pieces > 1 {
			asks = (calls - pieces*n) / pieces
		}
		if p.old {
			fmt.Fprintf(b, "pod-%d (old image) %s: %.0f vendor eth_getLogs calls for %.0f client reads, %.0f extra asks: %.3f per client read\n",
				p.idx+1, phase, calls, n, asks, asks/max(n, 1))
			continue
		}
		var checked float64
		for _, v := range p23Checked {
			checked += gates1[p.idx][v] - gates0[p.idx][v]
		}
		per := asks / max(checked, 1)
		fmt.Fprintf(b, "pod-%d (new image) %s: %.0f vendor eth_getLogs calls for %.0f client reads (%.0f calls an ask), %.0f extra asks, %.0f reads checked by the logs gate: %.3f extra asks per checked read\n",
			p.idx+1, phase, calls, n, pieces, asks, checked, per)
		if judged && per > p23ExtraAsks {
			s.violate(s.name, "pod-%d %s: %.3f extra eth_getLogs asks per checked read, above %.1f: the logs gate asks one holder at a time", p.idx+1, phase, per, p23ExtraAsks)
		}
	}
}

// p23Calls are a pod's eth_getLogs requests through every vendor proxy:
// calls, less its tracker's logs fetches
// (erpc_chain_tracker_fetch_total{kind="logs"}), which no client read
// sends; repeats, each JSON-RPC id's requests past its first (link.logIDs).
type p23Calls struct{ calls, repeats float64 }

func (s *scenario) p23LogsCalls() []p23Calls {
	out := make([]p23Calls, len(s.h.pods))
	for _, p := range s.h.pods {
		ids := map[string]int{}
		for _, px := range s.proxies {
			out[p.idx].calls += float64(px.reads(p, getLogsRead))
			for id, n := range px.logsIDs(p) {
				ids[id] += n
			}
		}
		for _, n := range ids {
			out[p.idx].repeats += float64(n - 1)
		}
		fetched, _ := s.metric(p.idx, "erpc_chain_tracker_fetch_total", map[string]string{"kind": "logs"})
		out[p.idx].calls -= fetched
	}
	return out
}

// p23LookupFound and p23LookupNone key p23Gates' counts of the gate's
// header lookups of a blockHash the tracker does not hold
// (erpc_chain_tracker_fetch_total{kind="logs_hash"}) that found the
// header, and that found none.
const (
	p23LookupFound = "lookup found"
	p23LookupNone  = "lookup none"
)

// p23Gates is, per pod, its logs gate verdicts (p23Verdicts) and header
// lookups (p23LookupFound, p23LookupNone).
func (s *scenario) p23Gates() []map[string]float64 {
	out := make([]map[string]float64, len(s.h.pods))
	for _, p := range s.h.pods {
		out[p.idx] = map[string]float64{}
		for _, v := range p23Verdicts {
			out[p.idx][v], _ = s.metric(p.idx, "erpc_logs_empty_gate_total", map[string]string{"verdict": v})
		}
		out[p.idx][p23LookupFound], _ = s.metric(p.idx, "erpc_chain_tracker_fetch_total", map[string]string{"kind": "logs_hash", "outcome": "ok"})
		out[p.idx][p23LookupNone], _ = s.metric(p.idx, "erpc_chain_tracker_fetch_total", map[string]string{"kind": "logs_hash", "outcome": "error"})
	}
	return out
}

// p23SplitReads reads on pod, about p23SplitRate times a second until ctx
// ends, eth_getLogs over the p23SplitBlocks blocks ending back blocks
// below the pod's eth_blockNumber, more than the shape's split, so each
// vendor's attempt splits it into pieces pinned to that vendor. The
// filter is p23SplitFilter's; the answer must be the chain's logs for it:
// none.
func (s *scenario) p23SplitReads(ctx context.Context, pod int, r *p23Reads, back uint64) {
	c, err := rpc.DialOptions(ctx, s.h.pods[pod].httpURL(s.project, s.chain.ID()), rpc.WithHTTPClient(s.h.podClient()))
	if err != nil {
		s.violate("harness", "pod-%d: split client: %v", pod+1, err)
		return
	}
	defer c.Close()
	call := func(result interface{}, method string, args ...interface{}) error {
		ctx, cancel := context.WithTimeout(context.Background(), httpCheckWait)
		defer cancel()
		return c.CallContext(ctx, result, method, args...)
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	period := time.Duration(float64(time.Second) / p23SplitRate)
	for ctx.Err() == nil {
		sleepCtx(ctx, period/2+rand.N(period))
		var tip hexutil.Uint64
		if err := call(&tip, "eth_blockNumber"); err != nil || ctx.Err() != nil {
			continue
		}
		to := uint64(tip) - back
		from := to - p23SplitBlocks + 1
		q, want, ok := s.p23SplitFilter(from, to)
		if !ok {
			continue
		}
		wg.Go(func() {
			read := fmt.Sprintf("eth_getLogs(%d-%d, topic %s)", from, to, q["topics"].([]interface{})[1])
			var logs []struct {
				BlockHash hash           `json:"blockHash"`
				LogIndex  hexutil.Uint64 `json:"logIndex"`
			}
			start := time.Now()
			err := call(&logs, "eth_getLogs", q)
			took := time.Since(start)
			got := map[chainsim.LogKey]bool{}
			for _, l := range logs {
				got[chainsim.LogKey{BlockHash: l.BlockHash, LogIndex: int(l.LogIndex)}] = true
			}
			switch {
			case err != nil:
				r.fail(p23SplitKind, "%s failed after %s: %v", read, took.Round(time.Millisecond), err)
			case len(logs) != len(want) || !maps.Equal(got, want):
				r.fail(p23SplitKind, "%s answered %d logs, not the chain's %d, after %s", read, len(logs), len(want), took.Round(time.Millisecond))
			default:
				r.ok(p23SplitKind, took)
			}
		})
	}
}

// p23SplitFilter is a filter over from..to that the held headers' blooms
// call maybe and that matches no log: the address Addrs[0] and, at the
// second topic position, the topic of one transaction of the range's
// newest block that has a log of Addrs[0] in another transaction but none
// in that one (chainsim gives each transaction's logs a topic of its own).
// That block's bloom holds both, every other block's lacks the topic. want
// are the chain's logs for it; ok is false for a range with no such block.
func (s *scenario) p23SplitFilter(from, to uint64) (q map[string]interface{}, want map[chainsim.LogKey]bool, ok bool) {
	addr := chainsim.Addrs[0]
	for i := uint64(0); i <= to-from; i++ {
		b := s.chain.CanonAt(to - i)
		if b == nil {
			continue
		}
		holds := map[int]bool{}
		for _, l := range b.Logs() {
			if l.Address == addr {
				holds[l.TxIndex] = true
			}
		}
		if len(holds) == 0 {
			continue
		}
		for _, l := range b.Logs() {
			if holds[l.TxIndex] {
				continue
			}
			topic := l.Topics[1]
			want = map[chainsim.LogKey]bool{}
			for n := from; n <= to; n++ {
				c := s.chain.CanonAt(n)
				if c == nil {
					continue
				}
				for _, x := range c.Logs() {
					if x.Address == addr && len(x.Topics) > 1 && x.Topics[1] == topic {
						want[x.Key()] = true
					}
				}
			}
			return map[string]interface{}{
				"fromBlock": hexutil.EncodeUint64(from), "toBlock": hexutil.EncodeUint64(to),
				"address": addr.Hex(), "topics": []interface{}{nil, topic.Hex()},
			}, want, true
		}
	}
	return nil, nil, false
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
// its first log, so the logs gate tests a filter against the header's
// bloom, and one by its blockHash (p23Filtered, §6.4 logs gate).
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
			wg.Add(2)
			go func() { defer wg.Done(); s.p23Filtered(call, r, n, false) }()
			go func() { defer wg.Done(); s.p23Filtered(call, r, n, true) }()
		}
		if sh.old && v > p23Back {
			wg.Add(1)
			go read(v+ahead-p23Back, method+" control")
		}
	}
}

// p23Filtered reads eth_getLogs(n, n) filtered by the address and first
// topic of block n's first log, tallied as "eth_getLogs filtered", or,
// byHash, all of block n's logs by its blockHash, tallied as "eth_getLogs
// by hash" (production: a vendor answered a blockHash read [] for a block
// with logs); a block the chain has not made, or one with no logs, is not
// read. An answer of [] is a wrongEmpty.
func (s *scenario) p23Filtered(call func(result interface{}, method string, args ...interface{}) error, r *p23Reads, n uint64, byHash bool) {
	kind, read := "eth_getLogs filtered", fmt.Sprintf("eth_getLogs(%d) filtered", n)
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
	if byHash {
		kind, read = p23HashLogsKind, fmt.Sprintf("eth_getLogs(%s)", b.Hash.Hex())
		q, want = map[string]interface{}{"blockHash": b.Hash.Hex()}, len(b.Logs())
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
		r.fail(kind, "%s failed after %s: %v", read, took, err)
	case len(logs) == 0:
		r.fail(kind, "%s answered [], not %d logs, after %s", read, want, took)
	case len(logs) != want || !onBlock():
		r.fail(kind, "%s answered %d logs, not %d of %s, after %s", read, len(logs), want, b.Hash.Hex(), took)
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
// checks, and the same reads of the block p23Back below as control. With
// logs, also each new block's logs by its blockHash as the first vendor's
// tag names the block (p23Filtered), as a client fed by that vendor's
// heads asks, before the pod's tracker holds that hash: its empty
// answer's check must ask one other holder, not every routed upstream.
func (s *scenario) p23ByHash(ctx context.Context, pod int, r *p23Reads, vl *viewLog, logs bool) {
	var wg sync.WaitGroup
	defer wg.Wait()
	last := s.chain.Tip().Number
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	call := func(result interface{}, method string, args ...interface{}) error {
		return s.call(pod, result, method, args...)
	}
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
			if logs {
				wg.Go(func() {
					time.Sleep(time.Until(s.p23FirstServe(b, "eth_blockNumber")))
					s.p23Filtered(call, r, b.Number, true)
				})
			}
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
		// share against the parent's (p23Nulls); eth_getLogs by hash
		// against the old image's (p23HashLogs).
		if r.fails[k] > 0 && strict && !s.h.pods[pod].old && !(pre != nil && k == "eth_getBlockByNumber") && k != p23HashLogsKind {
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

// p23HashLogsKind tallies eth_getLogs reads by blockHash (p23Filtered).
const p23HashLogsKind = "eth_getLogs by hash"

// p23HashLogs reports each image's failed eth_getLogs reads by blockHash
// and flags the new image's above the old's: a read of a block no vendor
// has seen yet fails "unknown block" on either image (block availability),
// while one answered [] for a block with logs is a wrongEmpty, judged on
// its own.
func (s *scenario) p23HashLogs(b *strings.Builder, reads []*p23Reads) {
	var fails [2]int
	for _, p := range s.h.pods {
		r := reads[p.idx]
		r.mu.Lock()
		if p.old {
			fails[0] += r.fails[p23HashLogsKind]
		} else {
			fails[1] += r.fails[p23HashLogsKind]
		}
		r.mu.Unlock()
	}
	fmt.Fprintf(b, "read-ahead: %s failed: new image %d, old image %d\n", p23HashLogsKind, fails[1], fails[0])
	if fails[1] > fails[0] {
		s.violate(s.name, "read-ahead: %d %s reads failed on the new image, above the old's %d", fails[1], p23HashLogsKind, fails[0])
	}
}

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
