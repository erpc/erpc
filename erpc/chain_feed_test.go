package erpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/evm/logfilter"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsupstream"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Time scale of the chaos test. A spec duration d on a 12s chain lasts
// chaosBlocksFor(d) blocks here.
var (
	chaosBlock         = 200 * time.Millisecond
	chaosReorgWindow   = 16
	chaosMaxBackfill   = 3 * time.Second
	chaosHeadsWatch    = time.Second
	chaosStatePoll     = 300 * time.Millisecond
	chaosSeedBudget    = 30 * time.Second
	chaosSettleRound   = chaosBlock // a settle's asks and walks on local vendors
	chaosVendorBudget  = uint32(40) // requests per second per upstream; client load under -race exceeds 4x this
	chaosMailboxBurst  = 1500       // above the tracker's mailbox header cap of 1024
	chaosFinalizedLag  = uint64(64)
	chaosHistoryBlocks = 40
)

func chaosBlocksFor(d time.Duration) int {
	return int((d + 12*time.Second - 1) / (12 * time.Second))
}

// chaosScaled is spec duration d on the chaos chain.
func chaosScaled(d time.Duration) time.Duration {
	return time.Duration(chaosBlocksFor(d)) * chaosBlock
}

const (
	fReorgSmall   = "reorg of 1-3 blocks"
	fMinorityFork = "one vendor on a minority fork for 20 blocks"
	fAheadFork    = "one vendor 20 blocks ahead on its own fork"
	fDeepReorg    = "reorg deeper than the window"
	fSilent       = "vendor silent with its socket open"
	fLagging      = "primary 40 blocks behind that keeps advancing"
	fDown         = "vendor down for 50 blocks"
	fAllDown      = "every vendor down for 2 minutes"
	fHalt         = "no blocks for 3 minutes"
	f429          = "429 bursts on every vendor"
	fLate         = "answers 2 block times late"
	fLoad         = "client load using every upstream's rate budget for 30s"
	fSplit        = "1-vs-1 split settled by the third vendor"
	fNonLinking   = "2 of 4 votes on a branch that does not link"
	fRestart      = "restart where a minority header arrives first"
	fOrphanCached = "client answer for an orphan cached by number"
	fMailbox      = "burst that fills the mailbox"
	fPanicOnce    = "header that makes the tracker panic once"
	fPanicThrice  = "header that makes the tracker panic 3 times in a row"

	fNullReceipts   = "null receipts"
	fTruncReceipts  = "receipts that drop the last 10 receipts, from 2 upstreams of one vendor"
	fLateReceipts   = "receipts 2 block times late"
	fNoReceipts     = "one vendor without eth_getBlockReceipts"
	fReceiptsDown13 = "receipts failing on every vendor for 13s"
	fBlockDown60    = "one block failing on every vendor for 60s"
	fReceiptsDown90 = "every receipts and logs fetch failing for 90s"
	fWrongBloom     = "one vendor whose receipts have a wrong bloom"
	fChainBloom     = "every vendor's receipts carry a bloom other than the header's"
	fBloomFollower  = "client asking the receipts the tracker fetches while the first answer has a wrong bloom"
)

var chaosFaults = []string{
	fReorgSmall, fMinorityFork, fAheadFork, fDeepReorg, fSilent, fLagging, fDown, fAllDown, fHalt,
	f429, fLate, fLoad, fSplit, fNonLinking, fRestart, fOrphanCached, fMailbox, fPanicOnce, fPanicThrice,
	fNullReceipts, fTruncReceipts, fLateReceipts, fNoReceipts, fReceiptsDown13, fBlockDown60, fReceiptsDown90, fWrongBloom, fChainBloom, fBloomFollower,
}

// TestChainFeed_ConvergesUnderFaults runs real eRPC against 4 mocked vendors
// of one simulated chain (2 wss, 2 HTTP; 2 upstreams per vendor) while a
// seeded schedule injects network, node, reorg and receipts faults. Each ws
// newHeads client applies what it receives as a geth client does; its
// chain must end equal to the simulated canonical chain, it never sees a
// block off the canonical chain, and it never waits more than 3 live
// bounds while a vendor serves. Each ws logs client on one of 2 filters
// applies its logs, removed:true included; its logs must end equal to the
// canonical chain's matching logs, and only the 60s block, the 90s outage,
// the 2-minute outage of every vendor and a reorg deeper than the window
// may close it with 1013.
func TestChainFeed_ConvergesUnderFaults(t *testing.T) {
	origWatch := wsupstream.HeadsWatchInterval.Load()
	wsupstream.HeadsWatchInterval.Store(chaosHeadsWatch)
	scale := []struct {
		d *util.Duration
		v time.Duration
	}{
		{evm.LogsFallbackAfter, chaosScaled(5 * time.Second)},
		{evm.ReceiptsOutageAfter, chaosScaled(60 * time.Second)},
		{evm.ReceiptsProbeCeiling, chaosScaled(12 * time.Second)},
		{logsReceiptsWait, chaosScaled(60 * time.Second)},
		{logsCloseSpread, chaosScaled(60 * time.Second)},
	}
	orig := make([]time.Duration, len(scale))
	for i, sc := range scale {
		orig[i] = sc.d.Load()
		sc.d.Store(sc.v)
	}
	t.Cleanup(func() {
		wsupstream.HeadsWatchInterval.Store(origWatch)
		for i, sc := range scale {
			sc.d.Store(orig[i])
		}
	})

	// Each plan's faults happen in its seed's order, on vendors its seed
	// picks; together the plans cover every fault.
	plans := []struct {
		seed        int64
		pollSources bool
		faults      []string
	}{
		{11, true, []string{fMinorityFork, fAheadFork, f429, fLate, fMailbox, fPanicOnce, fTruncReceipts, fWrongBloom}},
		{23, true, []string{fDown, fLagging, fDeepReorg, fPanicThrice, fNonLinking, fReceiptsDown13}},
		{37, true, []string{fAllDown, fHalt, fLoad, fOrphanCached, fRestart, fChainBloom, fNullReceipts}},
		{41, false, []string{fSplit, fLate, fDeepReorg, fBlockDown60, fReceiptsDown90, fNoReceipts}},
		{53, true, []string{fSilent, fLate, fLateReceipts, fBloomFollower}},
	}
	covered := make(map[string][]int64)
	for _, p := range plans {
		ok := t.Run(fmt.Sprintf("seed=%d", p.seed), func(t *testing.T) {
			s := newChaosSeed(t, p.seed, p.pollSources)
			start := time.Now()
			s.run(p.faults)
			elapsed := time.Since(start)
			t.Logf("seed %d ran in %s", p.seed, elapsed)
			require.Less(t, elapsed, chaosSeedBudget, "each seed runs within its budget")
			for f := range s.occurred {
				covered[f] = append(covered[f], p.seed)
			}
			for f := range s.paths {
				covered[f] = append(covered[f], p.seed)
			}
		})
		if !ok {
			return
		}
	}
	for _, f := range chaosFaults {
		require.NotEmpty(t, covered[f], "fault %q occurred in no seed", f)
	}
	for _, p := range chaosPaths {
		require.NotEmpty(t, covered[p], "receipts path %q ran in no seed", p)
	}
}

//
// --- simulated chain ---
//

// simKind is what a block holds: no transactions, logs no test filter
// matches, or logs both test filters match.
type simKind uint8

const (
	simEmpty simKind = iota
	simQuiet
	simMatching
)

// Logs of simulated blocks use these addresses and topic0s; the test
// filters are {address: simAddrs[0]} and {topics: [simTopics[1]]}.
var (
	simAddrs  = []string{"0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000a2", "0x00000000000000000000000000000000000000a3", "0x00000000000000000000000000000000000000a4"}
	simTopics = []string{"0x" + strings.Repeat("0", 63) + "1", "0x" + strings.Repeat("0", 63) + "2", "0x" + strings.Repeat("0", 63) + "3"}
	// simFilters are the two filters logs clients subscribe, and what each
	// matches.
	simFilters = []struct {
		params  string
		matches func(simLog) bool
	}{
		{`{"address":"` + simAddrs[0] + `"}`, func(l simLog) bool { return l.addr == 0 }},
		{`{"topics":["` + simTopics[1] + `"]}`, func(l simLog) bool { return l.topic == 1 }},
	}
)

type simLog struct{ addr, topic int }

type simBlock struct {
	number   uint64
	hash     [32]byte
	parent   *simBlock
	ts       uint64
	minority bool // never canonical: a fork only some vendors serve
	kind     simKind
	tag      string     // seed/minted: names the block's transactions
	txs      [][]simLog // per transaction, its logs
	raw      json.RawMessage
}

func (b *simBlock) json() json.RawMessage { return b.raw }

// simContent draws a block's transactions from r: none for an empty block,
// 12-16 otherwise with 1-2 logs each. A quiet block's logs avoid both
// filters; a matching block holds a log of each.
func simContent(r *rand.Rand, kind simKind) [][]simLog {
	if kind == simEmpty {
		return nil
	}
	txs := make([][]simLog, 12+r.IntN(5))
	for i := range txs {
		for range 1 + r.IntN(2) {
			l := simLog{addr: r.IntN(len(simAddrs)), topic: r.IntN(len(simTopics))}
			if kind == simQuiet {
				l = simLog{addr: 2 + r.IntN(2), topic: []int{0, 2}[r.IntN(2)]}
			}
			txs[i] = append(txs[i], l)
		}
	}
	if kind == simMatching {
		txs[0][0] = simLog{addr: 0, topic: 1}
	}
	return txs
}

// simLogBloom adds a transaction's logs to bloom. Every log carries a
// second topic unique to its transaction, so receipts missing any
// transaction never match the header's bloom.
func simLogBloom(bloom []byte, tag string, tx int, logs []simLog) {
	for _, l := range logs {
		a, _ := hex.DecodeString(simAddrs[l.addr][2:])
		tp, _ := hex.DecodeString(simTopics[l.topic][2:])
		own, _ := hex.DecodeString(simTxTopic(tag, tx)[2:])
		logfilter.BloomAdd(bloom, a)
		logfilter.BloomAdd(bloom, tp)
		logfilter.BloomAdd(bloom, own)
	}
}

func simTxTopic(tag string, tx int) string {
	h := sha256.Sum256(fmt.Appendf(nil, "topic/%s/%d", tag, tx))
	return "0x" + hex.EncodeToString(h[:])
}

func simTxHash(block [32]byte, i int) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%x/%d", block, i))
	return "0x" + hex.EncodeToString(h[:])
}

// receipts are the block's receipts as a vendor answers them; badBloom
// zeroes every receipt's logsBloom, drop leaves out the last receipts.
func (b *simBlock) receipts(badBloom bool, drop int) []map[string]interface{} {
	key := "0x" + hex.EncodeToString(b.hash[:])
	n := max(0, len(b.txs)-drop)
	out := make([]map[string]interface{}, 0, n)
	logIndex := 0
	for i, tx := range b.txs[:n] {
		bloom := make([]byte, logfilter.BloomLen)
		if !badBloom {
			simLogBloom(bloom, b.tag, i, tx)
		}
		logs := make([]map[string]interface{}, 0, len(tx))
		for _, l := range tx {
			logs = append(logs, b.logJSON(i, logIndex, l))
			logIndex++
		}
		out = append(out, map[string]interface{}{
			"blockHash": key, "blockNumber": fmt.Sprintf("0x%x", b.number),
			"transactionHash": simTxHash(b.hash, i), "transactionIndex": fmt.Sprintf("0x%x", i),
			"status": "0x1", "cumulativeGasUsed": "0x5208", "gasUsed": "0x5208", "type": "0x0",
			"logsBloom": "0x" + hex.EncodeToString(bloom), "logs": logs,
		})
	}
	return out
}

func (b *simBlock) logJSON(tx, logIndex int, l simLog) map[string]interface{} {
	return map[string]interface{}{
		"address": simAddrs[l.addr], "topics": []string{simTopics[l.topic], simTxTopic(b.tag, tx)}, "data": "0x",
		"blockHash": "0x" + hex.EncodeToString(b.hash[:]), "blockNumber": fmt.Sprintf("0x%x", b.number),
		"transactionHash": simTxHash(b.hash, tx), "transactionIndex": fmt.Sprintf("0x%x", tx),
		"logIndex": fmt.Sprintf("0x%x", logIndex), "removed": false,
	}
}

// logsOf are the block's logs as eth_getLogs by blockHash answers them.
func (b *simBlock) logsOf() []map[string]interface{} {
	var out []map[string]interface{}
	logIndex := 0
	for i, tx := range b.txs {
		for _, l := range tx {
			out = append(out, b.logJSON(i, logIndex, l))
			logIndex++
		}
	}
	return out
}

// matched are the logIndexes of the block's logs that filter f matches.
func (b *simBlock) matched(f int) []int {
	var out []int
	logIndex := 0
	for _, tx := range b.txs {
		for _, l := range tx {
			if simFilters[f].matches(l) {
				out = append(out, logIndex)
			}
			logIndex++
		}
	}
	return out
}

// simFork is a branch some vendors serve instead of the canonical chain;
// while it grows, it gains a block every tick.
type simFork struct {
	tip   *simBlock
	grows bool
	ahead time.Duration // timestamps run this far ahead of the clock
}

// simChain is one simulated chain: its canonical branch, every block ever
// minted, and the vendors serving it. One lock guards the chain and every
// vendor's view.
type simChain struct {
	mu       sync.Mutex
	seed     int64
	rng      *rand.Rand
	minted   uint64
	canon    []*simBlock
	byHash   map[[32]byte]*simBlock
	halted   bool
	lastDeep time.Time
	vendors  []*simVendor
	// plan, while not empty, is the kind of the next blocks minted.
	plan []simKind

	// Receipts faults on every vendor: receipts calls fail until
	// receiptsDown, eth_getLogs calls until logsDown; badBloom gives every
	// receipt a wrong logsBloom. failNext makes the next matching block
	// minted failHash, whose receipts and logs fail until failUntil.
	receiptsDown time.Time
	logsDown     time.Time
	badBloom     bool
	failNext     bool
	failHash     [32]byte
	failUntil    time.Time
	// trapOn holds the first eth_getBlockReceipts call of each block with
	// transactions for 2 block times and answers it with a wrong bloom.
	trapOn bool
	traps  map[[32]byte]*simTrap
}

// simTrap is one block's held first receipts call.
type simTrap struct {
	answeredAt time.Time
	during     int // other receipts calls for the block while it was held
}

func newSimChain(seed int64) *simChain {
	c := &simChain{seed: seed, rng: rand.New(rand.NewPCG(uint64(seed), 1)), byHash: make(map[[32]byte]*simBlock)}
	start := time.Now().Add(-time.Duration(chaosHistoryBlocks) * chaosBlock)
	var parent *simBlock
	for i := range chaosHistoryBlocks {
		b := c.mintLocked(parent, 0x1000+uint64(i), start.Add(time.Duration(i)*chaosBlock), false)
		c.canon = append(c.canon, b)
		parent = b
	}
	return c
}

// mintLocked makes a block whose header hashes to its hash
// (blockHashRecompute), its logsBloom the OR of its logs.
func (c *simChain) mintLocked(parent *simBlock, number uint64, at time.Time, minority bool) *simBlock {
	c.minted++
	var ph [32]byte
	if parent != nil {
		ph = parent.hash
	}
	ts := uint64(at.Unix())
	if parent != nil && ts < parent.ts {
		ts = parent.ts
	}
	kind := simKind(0)
	if len(c.plan) > 0 {
		kind, c.plan = c.plan[0], c.plan[1:]
	} else {
		switch r := c.rng.IntN(8); {
		case r == 0:
			kind = simEmpty
		case r <= 2:
			kind = simQuiet
		default:
			kind = simMatching
		}
	}
	txs := simContent(c.rng, kind)
	bloom := make([]byte, logfilter.BloomLen)
	tag := fmt.Sprintf("%d/%d", c.seed, c.minted)
	for i, tx := range txs {
		simLogBloom(bloom, tag, i, tx)
	}
	hdr := &gethtypes.Header{
		ParentHash:  gethcommon.Hash(ph),
		UncleHash:   gethtypes.EmptyUncleHash,
		Coinbase:    gethcommon.HexToAddress("0x0000000000000000000000000000000000000001"),
		TxHash:      gethtypes.EmptyTxsHash,
		ReceiptHash: gethtypes.EmptyReceiptsHash,
		Bloom:       gethtypes.BytesToBloom(bloom),
		Difficulty:  big.NewInt(0),
		Number:      new(big.Int).SetUint64(number),
		GasLimit:    30_000_000,
		Time:        ts,
		Extra:       fmt.Appendf(nil, "%d/%d", c.seed, c.minted),
		BaseFee:     big.NewInt(7),
	}
	if len(txs) > 0 {
		hdr.TxHash = gethcommon.Hash(sha256.Sum256(fmt.Appendf(nil, "txs/%d/%d", c.seed, c.minted)))
		hdr.ReceiptHash = gethcommon.Hash(sha256.Sum256(fmt.Appendf(nil, "receipts/%d/%d", c.seed, c.minted)))
		hdr.GasUsed = uint64(len(txs)) * 21000
	}
	raw, err := hdr.MarshalJSON()
	if err != nil {
		panic(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		panic(err)
	}
	fields["size"] = json.RawMessage(`"0x220"`)
	fields["transactions"] = json.RawMessage(`[]`)
	fields["uncles"] = json.RawMessage(`[]`)
	raw, _ = json.Marshal(fields)
	b := &simBlock{number: number, hash: [32]byte(hdr.Hash()), parent: parent, ts: ts, minority: minority, kind: kind, tag: tag, txs: txs, raw: raw}
	c.byHash[b.hash] = b
	if c.failNext && kind == simMatching && !minority {
		c.failNext, c.failHash = false, b.hash
	}
	return b
}

func (c *simChain) childLocked(parent *simBlock, at time.Time, minority bool) *simBlock {
	return c.mintLocked(parent, parent.number+1, at, minority)
}

func (c *simChain) tipLocked() *simBlock { return c.canon[len(c.canon)-1] }

func (c *simChain) canonAtLocked(n uint64) *simBlock {
	base := c.canon[0].number
	if n < base || n-base >= uint64(len(c.canon)) {
		return nil
	}
	return c.canon[n-base]
}

func (c *simChain) tip() *simBlock {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tipLocked()
}

func (c *simChain) canonAt(n uint64) *simBlock {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.canonAtLocked(n)
}

func (c *simChain) block(h [32]byte) *simBlock {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byHash[h]
}

// tick adds a canonical block unless the chain halted, grows every growing
// fork, and lets each vendor follow.
func (c *simChain) tick() {
	c.mu.Lock()
	now := time.Now()
	if !c.halted {
		c.canon = append(c.canon, c.childLocked(c.tipLocked(), now, false))
	}
	grown := make(map[*simFork]bool)
	for _, v := range c.vendors {
		if f := v.fork; f != nil && f.grows && !grown[f] {
			grown[f] = true
			f.tip = c.childLocked(f.tip, now.Add(f.ahead), true)
		}
	}
	c.mu.Unlock()
	c.refresh()
}

// reorg replaces the top depth canonical blocks with a new branch one
// block longer; the old blocks become orphans.
func (c *simChain) reorg(depth int) *simBlock {
	c.mu.Lock()
	keep := len(c.canon) - depth
	old := slices.Clone(c.canon[keep:])
	c.canon = c.canon[:keep]
	p := c.tipLocked()
	for _, o := range old {
		p = c.childLocked(p, time.Unix(int64(o.ts), 0), false)
		c.canon = append(c.canon, p)
	}
	c.canon = append(c.canon, c.childLocked(p, time.Now(), false))
	if depth > chaosReorgWindow {
		c.lastDeep = time.Now()
	}
	tip := c.tipLocked()
	c.mu.Unlock()
	c.refresh()
	return tip
}

type simPush struct {
	conn *websocket.Conn
	sub  string
	blk  *simBlock
}

// refresh moves each vendor's tip to what its state serves and pushes
// each new tip on its newHeads subscriptions.
func (c *simChain) refresh() {
	c.mu.Lock()
	var pushes []simPush
	now := time.Now()
	for _, v := range c.vendors {
		target := v.targetLocked()
		if target == v.tip {
			continue
		}
		v.setTipLocked(target)
		if v.ws && !v.down && !v.silent && !now.Before(v.holdUntil) {
			pushes = v.pushesLocked(pushes)
		}
	}
	c.mu.Unlock()
	writePushes(pushes)
}

func writePushes(pushes []simPush) {
	for _, p := range pushes {
		_ = mockWriteJSON(p.conn, map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "eth_subscription",
			"params":  map[string]interface{}{"subscription": p.sub, "result": p.blk.json()},
		})
	}
}

//
// --- vendors ---
//

type simRPCError struct {
	code int
	msg  string
}

// simVendor is one mocked vendor of the chain: its view of the chain and
// the faults it currently has. View and faults are guarded by chain.mu.
type simVendor struct {
	id     string
	vendor string // upstreams of one vendor share it
	ws     bool
	chain  *simChain
	url    string

	tip           *simBlock
	seen          map[[32]byte]bool
	lag           uint64
	fork          *simFork
	down          bool
	silent        bool
	late          time.Duration
	throttleUntil time.Time
	holdUntil     time.Time
	conns         map[*websocket.Conn]string // newHeads subscription id, "" before one

	// Receipts faults: null answers, answers without their last 10
	// receipts, answers late by lateReceipts, -32601 for
	// eth_getBlockReceipts, receipts with a wrong logsBloom.
	nullReceipts  bool
	truncReceipts bool
	lateReceipts  time.Duration
	noReceipts    bool
	wrongBloom    bool

	connects      atomic.Int64
	subscribes    atomic.Int64
	logsSubscribe atomic.Int64 // eth_subscribe logs: never expected
	refused       atomic.Int64
	throttled     atomic.Int64
	lateAnswers   atomic.Int64
	lagAnswers    atomic.Int64
	minorityOut   atomic.Int64
	getLogsCalls  atomic.Int64

	byNumberMu sync.Mutex
	byNumber   map[uint64]int
	// receiptsCalls counts eth_getBlockReceipts calls per block, served
	// the blocks answered with their full receipts.
	receiptsCalls map[[32]byte]int
	served        map[[32]byte]bool
}

func (c *simChain) addVendor(t *testing.T, id, vendor string, ws bool) *simVendor {
	v := &simVendor{id: id, vendor: vendor, ws: ws, chain: c, seen: make(map[[32]byte]bool), conns: make(map[*websocket.Conn]string),
		byNumber: make(map[uint64]int), receiptsCalls: make(map[[32]byte]int), served: make(map[[32]byte]bool)}
	c.mu.Lock()
	v.setTipLocked(c.tipLocked())
	c.vendors = append(c.vendors, v)
	c.mu.Unlock()
	if ws {
		hs := httptestServer(t, http.HandlerFunc(v.serveWS))
		v.url = "ws" + strings.TrimPrefix(hs, "http")
	} else {
		v.url = httptestServer(t, http.HandlerFunc(v.serveHTTP))
	}
	return v
}

func (v *simVendor) targetLocked() *simBlock {
	if v.fork != nil {
		return v.fork.tip
	}
	tip := v.chain.tipLocked()
	if v.lag > 0 && tip.number > v.lag {
		if b := v.chain.canonAtLocked(tip.number - v.lag); b != nil {
			return b
		}
	}
	return tip
}

func (v *simVendor) setTipLocked(b *simBlock) {
	v.tip = b
	for a, i := b, 0; a != nil && !v.seen[a.hash] && i < 256; a, i = a.parent, i+1 {
		v.seen[a.hash] = true
	}
}

func (v *simVendor) pushesLocked(out []simPush) []simPush {
	if v.tip.minority {
		v.minorityOut.Add(1)
	}
	for conn, sub := range v.conns {
		if sub != "" {
			out = append(out, simPush{conn: conn, sub: sub, blk: v.tip})
		}
	}
	return out
}

// byNumberLocked walks the vendor's own branch down to n.
func (v *simVendor) byNumberLocked(n uint64) *simBlock {
	b := v.tip
	for b != nil && b.number > n {
		b = b.parent
	}
	if b == nil || b.number != n {
		return nil
	}
	return b
}

func (v *simVendor) setDown(down bool) {
	c := v.chain
	c.mu.Lock()
	v.down = down
	var conns []*websocket.Conn
	if down {
		for conn := range v.conns {
			conns = append(conns, conn)
		}
	}
	c.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	c.refresh()
}

func (v *simVendor) isDown() bool {
	v.chain.mu.Lock()
	defer v.chain.mu.Unlock()
	return v.down
}

func isSimBlockMethod(method string) bool {
	return method == "eth_getBlockByNumber" || method == "eth_getBlockByHash" || method == "eth_blockNumber"
}

func (v *simVendor) throttledNow() bool {
	v.chain.mu.Lock()
	defer v.chain.mu.Unlock()
	return time.Now().Before(v.throttleUntil)
}

// answer serves one JSON-RPC call from the vendor's view.
func (v *simVendor) answer(method string, params []interface{}) (interface{}, *simRPCError) {
	c := v.chain
	c.mu.Lock()
	late := v.late
	if method == "eth_getBlockReceipts" || method == "eth_getLogs" {
		late += v.lateReceipts
	}
	c.mu.Unlock()
	if method == "eth_getBlockReceipts" {
		if res, ok := v.trapped(params); ok {
			return res, nil
		}
	}
	if late > 0 {
		v.lateAnswers.Add(1)
		time.Sleep(late)
	}
	if isSimBlockMethod(method) {
		for {
			c.mu.Lock()
			hold := v.holdUntil
			c.mu.Unlock()
			if !time.Now().Before(hold) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if v.down {
		return nil, &simRPCError{-32603, "vendor down"}
	}
	switch method {
	case "eth_chainId":
		return "0x7b", nil
	case "net_version":
		return "123", nil
	case "eth_syncing":
		return false, nil
	case "eth_blockNumber":
		return fmt.Sprintf("0x%x", v.tip.number), nil
	case "eth_getBalance":
		return "0x0", nil
	case "eth_getBlockReceipts", "eth_getLogs":
		return v.receiptsLocked(method, params)
	case "eth_getBlockByHash":
		hs, _ := params[0].(string)
		var h [32]byte
		raw, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(hs), "0x"))
		if err != nil || len(raw) != 32 {
			return nil, &simRPCError{-32602, "invalid hash"}
		}
		copy(h[:], raw)
		if !v.seen[h] {
			return nil, nil
		}
		return c.byHash[h].json(), nil
	case "eth_getBlockByNumber":
		tag, _ := params[0].(string)
		var b *simBlock
		switch tag {
		case "latest", "pending":
			b = v.tip
			if v.lag >= 40 {
				v.lagAnswers.Add(1)
			}
		case "finalized", "safe":
			n := c.canon[0].number
			if v.tip.number > chaosFinalizedLag && v.tip.number-chaosFinalizedLag > n {
				n = v.tip.number - chaosFinalizedLag
			}
			b = v.byNumberLocked(n)
		case "earliest":
			b = c.canon[0]
		default:
			n, err := common.HexToInt64(tag)
			if err != nil || n < 0 {
				return nil, &simRPCError{-32602, "invalid block number"}
			}
			v.byNumberMu.Lock()
			v.byNumber[uint64(n)]++
			v.byNumberMu.Unlock()
			b = v.byNumberLocked(uint64(n))
		}
		if b == nil {
			return nil, nil
		}
		if b.minority {
			v.minorityOut.Add(1)
		}
		return b.json(), nil
	}
	return "0x0", nil
}

// receiptsLocked answers eth_getBlockReceipts by hash or eth_getLogs by
// blockHash under the chain's and the vendor's receipts faults.
func (v *simVendor) receiptsLocked(method string, params []interface{}) (interface{}, *simRPCError) {
	c := v.chain
	var hs string
	if method == "eth_getLogs" {
		v.getLogsCalls.Add(1)
		if f, ok := params[0].(map[string]interface{}); ok {
			hs, _ = f["blockHash"].(string)
		}
	} else {
		hs, _ = params[0].(string)
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(hs), "0x"))
	if err != nil || len(raw) != 32 {
		return nil, &simRPCError{-32602, "invalid block hash"}
	}
	h := [32]byte(raw)
	now := time.Now()
	if method == "eth_getBlockReceipts" {
		v.receiptsCalls[h]++
		if v.noReceipts {
			return nil, &simRPCError{-32601, "the method eth_getBlockReceipts does not exist/is not available"}
		}
	}
	switch {
	case h == c.failHash && now.Before(c.failUntil),
		method == "eth_getBlockReceipts" && now.Before(c.receiptsDown),
		method == "eth_getLogs" && now.Before(c.logsDown):
		return nil, &simRPCError{-32603, "receipts unavailable"}
	}
	b := c.byHash[h]
	if b == nil || !v.seen[h] {
		return nil, nil
	}
	if method == "eth_getLogs" {
		return b.logsOf(), nil
	}
	if v.nullReceipts {
		return nil, nil
	}
	drop := 0
	if v.truncReceipts {
		drop = 10
	}
	if drop == 0 && !v.wrongBloom && !c.badBloom {
		v.served[h] = true
	}
	return b.receipts(v.wrongBloom || c.badBloom, drop), nil
}

// trapped answers a block's first receipts call while the chain's trap is
// on: after 2 block times, with a wrong bloom.
func (v *simVendor) trapped(params []interface{}) (interface{}, bool) {
	if len(params) == 0 {
		return nil, false
	}
	hs, _ := params[0].(string)
	raw, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(hs), "0x"))
	if err != nil || len(raw) != 32 {
		return nil, false
	}
	h := [32]byte(raw)
	c := v.chain
	c.mu.Lock()
	tr, b := c.traps[h], c.byHash[h]
	if tr != nil && tr.answeredAt.IsZero() {
		tr.during++
	}
	if !c.trapOn || tr != nil || b == nil || len(b.txs) == 0 || !v.seen[h] || v.down || v.noReceipts {
		c.mu.Unlock()
		return nil, false
	}
	tr = &simTrap{}
	c.traps[h] = tr
	c.mu.Unlock()
	time.Sleep(2 * chaosBlock)
	c.mu.Lock()
	defer c.mu.Unlock()
	tr.answeredAt = time.Now()
	v.receiptsCalls[h]++
	return b.receipts(true, 0), true
}

// receiptsCallsOf is how many eth_getBlockReceipts calls the vendor got for
// block h.
func (v *simVendor) receiptsCallsOf(h [32]byte) int {
	v.chain.mu.Lock()
	defer v.chain.mu.Unlock()
	return v.receiptsCalls[h]
}

func (v *simVendor) byNumberCalls(n uint64) int {
	v.byNumberMu.Lock()
	defer v.byNumberMu.Unlock()
	return v.byNumber[n]
}

func simReply(id interface{}, result interface{}, rerr *simRPCError) map[string]interface{} {
	out := map[string]interface{}{"jsonrpc": "2.0", "id": id}
	if rerr != nil {
		out["error"] = map[string]interface{}{"code": rerr.code, "message": rerr.msg}
	} else {
		out["result"] = result
	}
	return out
}

func (v *simVendor) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if v.isDown() {
		v.refused.Add(1)
		http.Error(w, "vendor down", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if v.throttledNow() {
		v.throttled.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(simReply(nil, nil, &simRPCError{-32005, "rate limit exceeded"}))
		return
	}
	one := func(req map[string]interface{}) map[string]interface{} {
		method, _ := req["method"].(string)
		params, _ := req["params"].([]interface{})
		res, rerr := v.answer(method, params)
		return simReply(req["id"], res, rerr)
	}
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
		var reqs []map[string]interface{}
		_ = json.Unmarshal(body, &reqs)
		out := make([]map[string]interface{}, 0, len(reqs))
		for _, req := range reqs {
			out = append(out, one(req))
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	var req map[string]interface{}
	_ = json.Unmarshal(body, &req)
	_ = json.NewEncoder(w).Encode(one(req))
}

var simUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func (v *simVendor) serveWS(w http.ResponseWriter, r *http.Request) {
	if v.isDown() {
		v.refused.Add(1)
		http.Error(w, "vendor down", http.StatusServiceUnavailable)
		return
	}
	conn, err := simUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	v.connects.Add(1)
	c := v.chain
	c.mu.Lock()
	v.conns[conn] = ""
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(v.conns, conn)
		c.mu.Unlock()
		_ = conn.Close()
	}()
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req map[string]interface{}
		if json.Unmarshal(msg, &req) != nil {
			continue
		}
		go v.handleWS(conn, req)
	}
}

func (v *simVendor) handleWS(conn *websocket.Conn, req map[string]interface{}) {
	method, _ := req["method"].(string)
	params, _ := req["params"].([]interface{})
	c := v.chain
	switch {
	case v.throttledNow():
		v.throttled.Add(1)
		_ = mockWriteJSON(conn, simReply(req["id"], nil, &simRPCError{-32005, "rate limit exceeded"}))
	case method == "eth_subscribe":
		if len(params) > 0 && params[0] == "logs" {
			v.logsSubscribe.Add(1)
		}
		if len(params) == 0 || params[0] != "newHeads" {
			_ = mockWriteJSON(conn, simReply(req["id"], "0xother", nil))
			return
		}
		sub := fmt.Sprintf("0x%s%d", strings.ReplaceAll(v.id, "-", ""), v.subscribes.Add(1))
		c.mu.Lock()
		if _, ok := v.conns[conn]; ok {
			v.conns[conn] = sub
		}
		c.mu.Unlock()
		_ = mockWriteJSON(conn, simReply(req["id"], sub, nil))
	case method == "eth_unsubscribe":
		_ = mockWriteJSON(conn, simReply(req["id"], true, nil))
	default:
		res, rerr := v.answer(method, params)
		_ = mockWriteJSON(conn, simReply(req["id"], res, rerr))
	}
}

// burst pushes n headers on every newHeads subscription, alternating the
// vendor's tip and its parent, so intake's duplicate check passes each.
func (v *simVendor) burst(n int) int {
	c := v.chain
	c.mu.Lock()
	var pushes []simPush
	for conn, sub := range v.conns {
		if sub == "" {
			continue
		}
		for i := range n {
			b := v.tip
			if i%2 == 1 {
				b = v.tip.parent
			}
			pushes = append(pushes, simPush{conn: conn, sub: sub, blk: b})
		}
	}
	c.mu.Unlock()
	writePushes(pushes)
	return len(pushes)
}

//
// --- clients ---
//

type clientNote struct {
	at     time.Time
	number uint64
	hash   [32]byte
}

// chaosClient is one ws newHeads subscription. It applies each head as a
// geth client does: a number it has replaces that number and above.
type chaosClient struct {
	name string
	slow func() bool

	mu         sync.Mutex
	subscribed time.Time
	ended      time.Time
	endTip     uint64
	notes      []clientNote
	chain      map[uint64][32]byte
	first      uint64
	last       uint64
	floor      uint64 // a permitted restart: blocks below are not judged
	lastTaps   tapEnds
	problems   []string
	conn       *websocket.Conn
}

// tapEnds counts the ends of ws newHeads taps so far: out of range by
// reason, and panics.
type tapEnds map[string]float64

const tapPanic = "panic"

var tapEndReasons = []string{indexer.OutOfRangeLag, indexer.OutOfRangeBelowWindow, indexer.OutOfRangeForkLeftWindow,
	indexer.OutOfRangeCursorUnknown, indexer.OutOfRangeReceiptsTimeout}

func newHeadsTapEnds() tapEnds {
	out := make(tapEnds, len(tapEndReasons)+1)
	for _, r := range tapEndReasons {
		out[r] = testutil.ToFloat64(telemetry.MetricTapOutOfRangeTotal.WithLabelValues("test_ws", "evm:123", "ws", indexer.SubTypeNewHeads, r))
	}
	ch := make(chan prometheus.Metric, 64)
	go func() {
		telemetry.MetricUnexpectedPanicTotal.Collect(ch)
		close(ch)
	}()
	for m := range ch {
		var pb dto.Metric
		if m.Write(&pb) != nil {
			continue
		}
		for _, l := range pb.GetLabel() {
			if l.GetName() == "extra" && strings.Contains(l.GetValue(), "kind:"+indexer.SubTypeNewHeads) {
				out[tapPanic] += pb.GetCounter().GetValue()
			}
		}
	}
	return out
}

// restartContext names what may explain a jump: the newHeads tap ends
// since the client's previous head, the tap's cursor (that head), the
// tracker's head and sendable head now, and the faults that ran since
// shortly before the previous head.
func (s *chaosSeed) restartContext(prev, now tapEnds, cursor uint64, since time.Time) string {
	var ends []string
	for _, r := range append(slices.Clone(tapEndReasons), tapPanic) {
		if d := now[r] - prev[r]; d > 0 {
			ends = append(ends, fmt.Sprintf("%s+%g", r, d))
		}
	}
	if len(ends) == 0 {
		ends = append(ends, "none counted")
	}
	head, send := "none", "none"
	if tr := s.tracker(); tr != nil {
		if c, ok := tr.Tip(); ok {
			head = fmt.Sprint(c.Number)
		}
		if c, ok := tr.SendTip(); ok {
			send = fmt.Sprint(c.Number)
		}
	}
	return fmt.Sprintf("tap ends %s; tap cursor %d; tracker head %s; sendable head %s; faults %s",
		strings.Join(ends, " "), cursor, head, send, s.faultsSince(since.Add(-2*time.Second)))
}

func (s *chaosSeed) dialClient(name string, slow func() bool) *chaosClient {
	cl := &chaosClient{name: name, slow: slow, chain: make(map[uint64][32]byte)}
	conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/evm/123", s.addr), nil)
	require.NoError(s.t, err)
	require.NoError(s.t, conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)))
	require.NoError(s.t, conn.SetReadDeadline(time.Now().Add(15*time.Second)))
	_, msg, err := conn.ReadMessage()
	require.NoError(s.t, err)
	var resp struct {
		Result string          `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	require.NoError(s.t, json.Unmarshal(msg, &resp))
	require.Empty(s.t, resp.Error, "subscribe failed: %s", msg)
	require.NotEmpty(s.t, resp.Result, "client %s: the first message is the subscription id, as geth sends it: %s", name, msg)
	cl.conn = conn
	cl.subscribed = time.Now()
	cl.lastTaps = newHeadsTapEnds()
	s.mu.Lock()
	s.clients = append(s.clients, cl)
	s.mu.Unlock()
	go cl.read(s)
	return cl
}

func (cl *chaosClient) read(s *chaosSeed) {
	defer func() {
		cl.mu.Lock()
		cl.ended = time.Now()
		cl.endTip = s.chain.tip().number
		cl.mu.Unlock()
	}()
	for {
		if cl.slow != nil && cl.slow() {
			time.Sleep(chaosBlock * 3 / 2)
		}
		_ = cl.conn.SetReadDeadline(time.Now().Add(time.Minute))
		_, msg, err := cl.conn.ReadMessage()
		if err != nil {
			return
		}
		var note struct {
			Method string `json:"method"`
			Params struct {
				Result struct {
					Number string `json:"number"`
					Hash   string `json:"hash"`
				} `json:"result"`
			} `json:"params"`
		}
		if json.Unmarshal(msg, &note) != nil || note.Method != "eth_subscription" {
			cl.problem("unexpected message %s", msg)
			continue
		}
		n, err1 := common.HexToInt64(note.Params.Result.Number)
		raw, err2 := hex.DecodeString(strings.TrimPrefix(note.Params.Result.Hash, "0x"))
		if err1 != nil || err2 != nil || len(raw) != 32 {
			cl.problem("malformed head %s", msg)
			continue
		}
		var h [32]byte
		copy(h[:], raw)
		cl.apply(s, uint64(n), h)
	}
}

func (cl *chaosClient) problem(format string, args ...interface{}) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.problems = append(cl.problems, fmt.Sprintf(format, args...))
}

func (cl *chaosClient) apply(s *chaosSeed, n uint64, h [32]byte) {
	b := s.chain.block(h)
	ends := newHeadsTapEnds()
	cl.mu.Lock()
	defer cl.mu.Unlock()
	now := time.Now()
	switch {
	case b == nil:
		cl.problems = append(cl.problems, fmt.Sprintf("head %d %x: no vendor ever served it", n, h[:4]))
	case b.minority:
		cl.problems = append(cl.problems, fmt.Sprintf("head %d %x: a minority fork reached the client", n, h[:4]))
	case b.number != n:
		cl.problems = append(cl.problems, fmt.Sprintf("head %x: number %d, block %d", h[:4], n, b.number))
	}
	if cl.last != 0 && n > cl.last+1 {
		s.chain.mu.Lock()
		deep := !s.chain.lastDeep.IsZero() && now.Sub(s.chain.lastDeep) < 10*time.Second
		s.chain.mu.Unlock()
		since := cl.subscribed
		if len(cl.notes) > 0 {
			since = cl.notes[len(cl.notes)-1].at
		}
		why := s.restartContext(cl.lastTaps, ends, cl.last, since)
		// A tap more than the window behind restarts after the head (spec
		// §9.2), counted as lag or, once its cursor left the window, as
		// below_window; the jump then spans more than the window.
		behind := ends[indexer.OutOfRangeLag]+ends[indexer.OutOfRangeBelowWindow] >
			cl.lastTaps[indexer.OutOfRangeLag]+cl.lastTaps[indexer.OutOfRangeBelowWindow] &&
			n > cl.last+1+uint64(chaosReorgWindow)
		if deep || behind {
			cl.floor = n
			s.t.Logf("client %s: jump %d → %d after a restart; %s", cl.name, cl.last, n, why)
		} else {
			cl.problems = append(cl.problems, fmt.Sprintf("jump %d → %d with no fault that calls for a restart; %s", cl.last, n, why))
		}
	}
	cl.lastTaps = ends
	for k := range cl.chain {
		if k >= n {
			delete(cl.chain, k)
		}
	}
	cl.chain[n] = h
	if cl.first == 0 {
		cl.first = n
	}
	cl.last = n
	cl.notes = append(cl.notes, clientNote{at: now, number: n, hash: h})
}

// simLogKey names one log: its block and its index in the block.
type simLogKey struct {
	block [32]byte
	index uint64
}

// chaosLogsClient is one ws logs subscription on simFilters[filter]. It
// applies each log as a geth client does: removed:true drops a log it
// holds, any other log is added.
type chaosLogsClient struct {
	name   string
	filter int
	conn   *websocket.Conn

	mu        sync.Mutex
	held      map[simLogKey]uint64 // log -> its block number
	first     uint64               // lowest block of an added log, 0 before one
	high      uint64               // highest block of an added log
	added     int
	ended     time.Time
	endTip    uint64
	closeCode int  // the server's close code, 0 when it did not close
	byTest    bool // the test closed it
	notes     int  // notifications received
	noteBytes int  // their message lengths
	problems  []string
}

// subscribeLogs dials a ws connection and subscribes simFilters[filter]; on
// a refusal it returns the JSON-RPC error and closes the connection.
func (s *chaosSeed) subscribeLogs(filter int) (*websocket.Conn, map[string]interface{}) {
	conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/evm/123", s.addr), nil)
	require.NoError(s.t, err)
	require.NoError(s.t, conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["logs",`+simFilters[filter].params+`]}`)))
	require.NoError(s.t, conn.SetReadDeadline(time.Now().Add(15*time.Second)))
	_, msg, err := conn.ReadMessage()
	require.NoError(s.t, err)
	var resp map[string]interface{}
	require.NoError(s.t, json.Unmarshal(msg, &resp))
	if e, ok := resp["error"].(map[string]interface{}); ok {
		_ = conn.Close()
		return nil, e
	}
	require.IsType(s.t, "", resp["result"], "logs subscribe answer %s", msg)
	return conn, nil
}

// dialLogs opens a logs client on each filter, retrying while subscribes
// are refused.
func (s *chaosSeed) dialLogs(name string) {
	for f := range simFilters {
		var conn *websocket.Conn
		require.Eventually(s.t, func() bool {
			conn, _ = s.subscribeLogs(f)
			return conn != nil
		}, 15*time.Second, chaosBlock, "logs subscribe on filter %d is accepted", f)
		cl := &chaosLogsClient{name: fmt.Sprintf("%s/%d", name, f), filter: f, conn: conn, held: make(map[simLogKey]uint64)}
		s.mu.Lock()
		s.logs = append(s.logs, cl)
		s.mu.Unlock()
		go cl.read(s)
	}
}

// openLogs are the logs clients still open.
func (s *chaosSeed) openLogs() []*chaosLogsClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*chaosLogsClient
	for _, cl := range s.logs {
		cl.mu.Lock()
		if cl.ended.IsZero() {
			out = append(out, cl)
		}
		cl.mu.Unlock()
	}
	return out
}

// closed1013 counts the logs clients the server closed with 1013 since t.
func (s *chaosSeed) closed1013(since time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, cl := range s.logs {
		cl.mu.Lock()
		if cl.closeCode == websocket.CloseTryAgainLater && cl.ended.After(since) {
			n++
		}
		cl.mu.Unlock()
	}
	return n
}

func (cl *chaosLogsClient) read(s *chaosSeed) {
	for {
		_ = cl.conn.SetReadDeadline(time.Now().Add(time.Minute))
		_, msg, err := cl.conn.ReadMessage()
		if err != nil {
			cl.mu.Lock()
			cl.ended, cl.endTip = time.Now(), s.chain.tip().number
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				cl.closeCode = ce.Code
			}
			cl.mu.Unlock()
			return
		}
		var note struct {
			Method string `json:"method"`
			Params struct {
				Result struct {
					BlockHash   string `json:"blockHash"`
					BlockNumber string `json:"blockNumber"`
					LogIndex    string `json:"logIndex"`
					Removed     bool   `json:"removed"`
				} `json:"result"`
			} `json:"params"`
		}
		r := &note.Params.Result
		if json.Unmarshal(msg, &note) != nil || note.Method != "eth_subscription" {
			cl.problem("unexpected message %s", msg)
			continue
		}
		cl.mu.Lock()
		cl.notes++
		cl.noteBytes += len(msg)
		cl.mu.Unlock()
		raw, err1 := hex.DecodeString(strings.TrimPrefix(r.BlockHash, "0x"))
		n, err2 := common.HexToInt64(r.BlockNumber)
		idx, err3 := common.HexToInt64(r.LogIndex)
		if err1 != nil || err2 != nil || err3 != nil || len(raw) != 32 {
			cl.problem("malformed log %s", msg)
			continue
		}
		cl.apply(s, simLogKey{block: [32]byte(raw), index: uint64(idx)}, uint64(n), r.Removed)
	}
}

func (cl *chaosLogsClient) problem(format string, args ...interface{}) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.problems = append(cl.problems, fmt.Sprintf(format, args...))
}

func (cl *chaosLogsClient) apply(s *chaosSeed, k simLogKey, n uint64, removed bool) {
	b := s.chain.block(k.block)
	cl.mu.Lock()
	defer cl.mu.Unlock()
	switch {
	case b == nil || b.number != n:
		cl.problems = append(cl.problems, fmt.Sprintf("log of block %d %x: no such block", n, k.block[:4]))
		return
	case b.minority:
		cl.problems = append(cl.problems, fmt.Sprintf("log of block %d %x: a minority fork reached the client", n, k.block[:4]))
	case !slices.Contains(b.matched(cl.filter), int(k.index)):
		cl.problems = append(cl.problems, fmt.Sprintf("log %d of block %d: the filter does not match it", k.index, n))
	}
	_, held := cl.held[k]
	switch {
	case removed && !held:
		cl.problems = append(cl.problems, fmt.Sprintf("removed:true for log %d of block %d, never sent", k.index, n))
	case removed:
		delete(cl.held, k)
	case held:
		cl.problems = append(cl.problems, fmt.Sprintf("log %d of block %d sent twice", k.index, n))
	default:
		cl.held[k] = n
		cl.added++
		if cl.first == 0 || n < cl.first {
			cl.first = n
		}
		cl.high = max(cl.high, n)
	}
}

// logsDiffLocked compares the client's logs of blocks lo..hi with the
// canonical chain's logs its filter matches; "" when equal.
func (cl *chaosLogsClient) logsDiffLocked(s *chaosSeed, lo, hi uint64) string {
	want := make(map[simLogKey]uint64)
	for n := lo; n <= hi; n++ {
		b := s.chain.canonAt(n)
		if b == nil {
			return fmt.Sprintf("no canonical block %d", n)
		}
		for _, i := range b.matched(cl.filter) {
			want[simLogKey{block: b.hash, index: uint64(i)}] = n
		}
	}
	for k, n := range cl.held {
		if n < lo || n > hi {
			continue
		}
		if _, ok := want[k]; !ok {
			return fmt.Sprintf("holds log %d of block %d %x, not canonical", k.index, n, k.block[:4])
		}
		delete(want, k)
	}
	for k, n := range want {
		return fmt.Sprintf("misses log %d of block %d %x", k.index, n, k.block[:4])
	}
	return ""
}

//
// --- one seed ---
//

type interval struct{ from, to time.Time }

// closeWindow is when a fault may close logs clients with 1013. fork is the
// first block a reorg deeper than the window orphaned, 0 for a fault that
// orphans none: a client it closed keeps the orphans' logs, never told
// removed, and is judged below it.
type closeWindow struct {
	interval
	fork uint64
}

type chaosSeed struct {
	t           *testing.T
	seed        int64
	rng         *rand.Rand
	pollSources bool
	chain       *simChain
	wsA, wsB    *simVendor
	httpA       *simVendor
	httpB       *simVendor
	vendors     []*simVendor

	srv    *HttpServer
	addr   string
	cancel context.CancelFunc
	nw     atomic.Pointer[Network]

	mu       sync.Mutex
	clients  []*chaosClient
	logs     []*chaosLogsClient
	quiet    []interval    // no vendor serves the chain
	closeOK  []closeWindow // a logs client may be closed with 1013
	occurred map[string]string
	paths    map[string]string
	// ignore are methods every upstream ignores.
	ignore []string

	breakerMu     sync.Mutex
	breakerOpened string
	breakerAt     time.Time
	loadFrom      time.Time

	hook *chaosHook

	faultMu sync.Mutex
	spans   []faultSpan
}

// faultSpan is when one fault ran; to is zero while it runs.
type faultSpan struct {
	name     string
	from, to time.Time
}

// during runs fault f and records when it ran.
func (s *chaosSeed) during(f string, run func()) {
	s.faultMu.Lock()
	i := len(s.spans)
	s.spans = append(s.spans, faultSpan{name: f, from: time.Now()})
	s.faultMu.Unlock()
	defer func() {
		s.faultMu.Lock()
		s.spans[i].to = time.Now()
		s.faultMu.Unlock()
	}()
	run()
}

// faultsSince names the faults that ran at or after since, with their
// times relative to now.
func (s *chaosSeed) faultsSince(since time.Time) string {
	now := time.Now()
	s.faultMu.Lock()
	defer s.faultMu.Unlock()
	var out []string
	for _, sp := range s.spans {
		if !sp.to.IsZero() && sp.to.Before(since) {
			continue
		}
		end := "running"
		if !sp.to.IsZero() {
			end = fmt.Sprintf("ended %s ago", now.Sub(sp.to).Round(time.Millisecond))
		}
		out = append(out, fmt.Sprintf("%q (began %s ago, %s)", sp.name, now.Sub(sp.from).Round(time.Millisecond), end))
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

func newChaosSeed(t *testing.T, seed int64, pollSources bool) *chaosSeed {
	s := &chaosSeed{t: t, seed: seed, rng: rand.New(rand.NewPCG(uint64(seed), 0)), pollSources: pollSources,
		occurred: make(map[string]string), paths: make(map[string]string)}
	s.chain = newSimChain(seed)
	s.wsA = s.chain.addVendor(t, "ws-a", "vendor-a", true)
	s.wsB = s.chain.addVendor(t, "ws-b", "vendor-b", true)
	s.httpA = s.chain.addVendor(t, "http-a", "vendor-a", false)
	s.httpB = s.chain.addVendor(t, "http-b", "vendor-b", false)
	s.vendors = []*simVendor{s.wsA, s.wsB, s.httpA, s.httpB}
	s.hook = &chaosHook{s: s, first: make(map[*evm.ChainTracker]indexer.Cursor), firstFrom: make(map[*evm.ChainTracker]string)}
	fn := s.hook.on
	evm.TrackerMessageHook.Store(&fn)
	t.Cleanup(func() { evm.TrackerMessageHook.Store(nil) })
	t.Cleanup(s.stopERPC)
	return s
}

func (s *chaosSeed) config() *common.Config {
	var ups []*common.UpstreamConfig
	var budgets []*common.RateLimitBudgetConfig
	for _, v := range s.vendors {
		var source *bool
		if !v.ws {
			source = util.BoolPtr(s.pollSources)
		}
		u := sourceUpstream(v.id, v.url, source)
		u.Evm.StatePollerInterval = common.Duration(chaosStatePoll)
		u.RateLimitBudget = "budget-" + v.id
		u.VendorName = v.vendor
		u.IgnoreMethods = s.ignore
		u.Failsafe = []*common.FailsafeConfig{{
			MatchMethod: "*",
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount:    3,
				FailureThresholdCapacity: 6,
				HalfOpenAfter:            common.Duration(time.Minute),
				SuccessThresholdCount:    1,
				SuccessThresholdCapacity: 1,
			},
		}}
		ups = append(ups, u)
		budgets = append(budgets, &common.RateLimitBudgetConfig{
			Id:    "budget-" + v.id,
			Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: chaosVendorBudget, Period: common.RateLimitPeriodSecond}},
		})
	}
	cfg := sourcesConfig(func(nw *common.NetworkConfig) {
		nw.Evm.ChainTracker = &common.EvmChainTrackerConfig{ReorgWindow: chaosReorgWindow, MaxBackfill: common.Duration(chaosMaxBackfill)}
	}, ups...)
	cfg.RateLimiters = &common.RateLimiterConfig{Budgets: budgets}
	cfg.Database = &common.DatabaseConfig{EvmJsonRpcCache: &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{{
			Id: "mem", Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		}},
		Policies: []*common.CachePolicyConfig{{
			Network: "*", Method: "eth_getBlockByNumber", Finality: common.DataFinalityStateUnfinalized,
			Connector: "mem", TTL: common.FixedDuration(time.Minute),
		}},
	}}
	return cfg
}

// startERPC boots an eRPC instance and waits for its network.
func (s *chaosSeed) startERPC() {
	srv, addr, cancel := startTestERPCServer(s.t, s.config())
	s.srv, s.addr, s.cancel = srv, addr, cancel
	var nw *Network
	require.Eventually(s.t, func() bool {
		n, err := srv.erpc.GetNetwork(context.Background(), "test_ws", "evm:123")
		if err != nil || n == nil || n.chainTracker == nil {
			return false
		}
		nw = n
		return true
	}, 15*time.Second, 20*time.Millisecond, "network initializes")
	s.nw.Store(nw)
}

// stopERPC stops the running instance, if any, and waits for its
// network's goroutines; a failed seed stops it in cleanup, so nothing it
// runs outlives the seed into later tests.
func (s *chaosSeed) stopERPC() {
	if s.srv == nil {
		return
	}
	_ = s.srv.Shutdown(&zerolog.Logger{})
	s.cancel()
	s.srv = nil
	s.mu.Lock()
	clients, logs := slices.Clone(s.clients), slices.Clone(s.logs)
	s.mu.Unlock()
	for _, cl := range clients {
		_ = cl.conn.Close()
	}
	for _, cl := range logs {
		cl.mu.Lock()
		cl.byTest = cl.ended.IsZero()
		cl.mu.Unlock()
		_ = cl.conn.Close()
	}
	if nw := s.nw.Load(); nw != nil {
		nw.wait()
	}
}

func (s *chaosSeed) tracker() *evm.ChainTracker {
	if nw := s.nw.Load(); nw != nil {
		return nw.chainTracker
	}
	return nil
}

func (s *chaosSeed) blocks(n int) { time.Sleep(time.Duration(n) * chaosBlock) }

func (s *chaosSeed) occur(fault, evidence string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.occurred[fault] = evidence
	s.t.Logf("fault %q: %s", fault, evidence)
}

func (s *chaosSeed) addQuiet(iv interval) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quiet = append(s.quiet, iv)
}

func (s *chaosSeed) pick(vs ...*simVendor) *simVendor { return vs[s.rng.IntN(len(vs))] }

// watchBreakers records the first breaker any upstream opens.
func (s *chaosSeed) watchBreakers(ctx context.Context) {
	for ctx.Err() == nil {
		if nw := s.nw.Load(); nw != nil && nw.upstreamsRegistry != nil {
			for _, u := range nw.upstreamsRegistry.GetNetworkUpstreams(context.Background(), nw.networkId) {
				if st := u.CircuitBreakerState(); st != failsafe.StateClosed {
					s.breakerMu.Lock()
					if s.breakerOpened == "" {
						s.breakerOpened, s.breakerAt = u.Id(), time.Now()
					}
					s.breakerMu.Unlock()
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (s *chaosSeed) rpc(method string, params ...interface{}) (map[string]interface{}, int, error) {
	body, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(fmt.Sprintf("http://%s/test_ws/evm/123", s.addr), "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return out, resp.StatusCode, nil
}

func (s *chaosSeed) run(faults []string) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	s.startERPC()
	go s.watchBreakers(ctx)

	producing := make(chan struct{})
	go func() {
		defer close(producing)
		tk := time.NewTicker(chaosBlock)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				s.chain.tick()
			}
		}
	}()

	order := slices.Clone(faults)
	s.rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	// Votes on a branch that does not link need all 4 vendors live, so
	// they come first; a vendor down for 50 blocks then runs alongside the
	// faults after it (a wss vendor may take its reconnect backoff to vote
	// again).
	if i := slices.Index(order, fDown); i >= 0 {
		order = append([]string{fDown}, slices.Delete(order, i, i+1)...)
	}
	if i := slices.Index(order, fNonLinking); i >= 0 {
		order = append([]string{fNonLinking}, slices.Delete(order, i, i+1)...)
	}

	var slowFrom, slowTo atomic.Int64
	slow := func() bool {
		now := time.Now().UnixNano()
		return now >= slowFrom.Load() && now < slowTo.Load()
	}
	s.dialClient("first", nil)
	s.dialClient("slow", slow)
	s.dialLogs("first")
	s.blocks(4)

	var bg sync.WaitGroup
	for i, f := range order {
		switch i {
		case 1:
			slowFrom.Store(time.Now().UnixNano())
			slowTo.Store(time.Now().Add(20 * chaosBlock).UnixNano())
			s.dialClient("late-1", nil)
		case len(order) - 1:
			s.dialClient("late-2", nil)
			s.dialLogs("late")
		}
		if s.rng.IntN(2) == 0 {
			s.during(fReorgSmall, s.reorgSmall)
		}
		s.blocks(2 + s.rng.IntN(3))
		if f == fDown {
			bg.Add(1)
			v := s.pick(s.vendors...)
			go func() { defer bg.Done(); s.during(fDown, func() { s.vendorDown(v) }) }()
			s.blocks(2)
			continue
		}
		s.during(f, func() { s.inject(f) })
	}
	bg.Wait()
	if _, ok := s.occurred[fReorgSmall]; !ok {
		s.during(fReorgSmall, s.reorgSmall)
	}

	s.checkSkips()

	// Every vendor serves the canonical chain again; clients catch up.
	s.blocks(8)
	stop()
	<-producing
	tip := s.chain.tip()
	require.Eventually(s.t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, cl := range s.clients {
			cl.mu.Lock()
			open, last, h := cl.ended.IsZero(), cl.last, cl.chain[cl.last]
			cl.mu.Unlock()
			if open && (last != tip.number || h != tip.hash) {
				return false
			}
		}
		return true
	}, 15*time.Second, 20*time.Millisecond, "every open client reaches the canonical head %d", tip.number)
	diff := "not checked"
	for deadline := time.Now().Add(15 * time.Second); diff != "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		diff = ""
		for _, cl := range s.openLogs() {
			cl.mu.Lock()
			if cl.first != 0 {
				if d := cl.logsDiffLocked(s, cl.first, tip.number); d != "" {
					diff = cl.name + ": " + d
				}
			}
			cl.mu.Unlock()
		}
	}
	require.Empty(s.t, diff, "every open logs client holds the canonical logs up to %d", tip.number)
	end := time.Now()
	s.stopERPC()
	s.judge(end)
}

// checkSkips mints 9 blocks with matching logs, then a quiet block no
// filter may match and an empty block: once 8 matching blocks with
// transactions passed the bloom check in a row, neither is fetched.
func (s *chaosSeed) checkSkips() {
	c := s.chain
	c.mu.Lock()
	plan := make([]simKind, 0, 11)
	for range 9 {
		plan = append(plan, simMatching)
	}
	c.plan = append(plan, simQuiet, simEmpty)
	from := c.tipLocked().number
	c.mu.Unlock()
	s.blocks(14)
	quiet, empty := s.chain.canonAt(from+10), s.chain.canonAt(from+11)
	require.True(s.t, quiet != nil && quiet.kind == simQuiet && empty != nil && empty.kind == simEmpty,
		"the planned quiet and empty blocks follow %d", from)
	require.Zero(s.t, s.receiptsCallsOf(quiet.hash), "a block no filter may match is not fetched after 8 matching blocks")
	require.Zero(s.t, s.receiptsCallsOf(empty.hash), "a block without transactions is never fetched")
	s.path(pathBloomSkip, fmt.Sprintf("quiet block %d not fetched", quiet.number))
	s.path(pathEmptySkip, fmt.Sprintf("empty block %d not fetched", empty.number))
}

func (s *chaosSeed) receiptsCallsOf(h [32]byte) int {
	n := 0
	for _, v := range s.vendors {
		n += v.receiptsCallsOf(h)
	}
	return n
}

// vendorOf are the upstreams of vendor name.
func (s *chaosSeed) vendorOf(name string) []*simVendor {
	var out []*simVendor
	for _, v := range s.vendors {
		if v.vendor == name {
			out = append(out, v)
		}
	}
	return out
}

// Receipts paths of the chain feed each seed may run (spec §10).
const (
	pathRotation  = "vendor rotation after a wrong answer"
	pathBloomOff  = "bloom check off for the network, then on"
	pathFallback  = "eth_getLogs fallback"
	pathBloomSkip = "bloom skip"
	pathEmptySkip = "empty block skip"
)

var chaosPaths = []string{pathRotation, pathBloomOff, pathFallback, pathBloomSkip, pathEmptySkip}

func (s *chaosSeed) path(p, evidence string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths[p] = evidence
	s.t.Logf("receipts path %q: %s", p, evidence)
}

func (s *chaosSeed) inject(f string) {
	switch f {
	case fMinorityFork:
		s.minorityFork()
	case fAheadFork:
		s.aheadFork()
	case fDeepReorg:
		s.deepReorg()
	case fSilent:
		s.silentVendor()
	case fLagging:
		s.laggingVendor()
	case fAllDown:
		s.allDown()
	case fHalt:
		s.halt()
	case f429:
		s.throttle()
	case fLate:
		s.lateAnswers()
	case fLoad:
		s.clientLoad()
	case fSplit:
		s.split()
	case fNonLinking:
		s.nonLinking()
	case fRestart:
		s.restart()
	case fOrphanCached:
		s.orphanCached()
	case fMailbox:
		s.mailboxBurst()
	case fPanicOnce:
		s.panicOnce()
	case fNullReceipts:
		s.nullReceipts()
	case fTruncReceipts:
		s.truncReceipts()
	case fLateReceipts:
		s.lateReceipts()
	case fNoReceipts:
		s.noReceipts()
	case fReceiptsDown13:
		s.receiptsDown13()
	case fBlockDown60:
		s.blockDown60()
	case fReceiptsDown90:
		s.receiptsDown90()
	case fWrongBloom:
		s.wrongBloom()
	case fChainBloom:
		s.chainBloom()
	case fBloomFollower:
		s.bloomFollower()
	case fPanicThrice:
		s.panicThrice()
	default:
		s.t.Fatalf("unknown fault %q", f)
	}
}

//
// --- faults ---
//

func (s *chaosSeed) reorgSmall() {
	depth := 1 + s.rng.IntN(3)
	tip := s.chain.reorg(depth)
	s.occur(fReorgSmall, fmt.Sprintf("depth %d, new head %d", depth, tip.number))
}

func (s *chaosSeed) setFork(f *simFork, vs ...*simVendor) {
	s.chain.mu.Lock()
	for _, v := range vs {
		v.fork = f
	}
	s.chain.mu.Unlock()
	s.chain.refresh()
}

func (s *chaosSeed) stopFork(f *simFork) {
	s.chain.mu.Lock()
	f.grows = false
	s.chain.mu.Unlock()
}

func (s *chaosSeed) minorityFork() {
	v := s.pick(s.vendors...)
	before := v.minorityOut.Load()
	c := s.chain
	c.mu.Lock()
	tip := c.tipLocked()
	f := &simFork{tip: c.childLocked(tip.parent, time.Now(), true), grows: true}
	c.mu.Unlock()
	s.setFork(f, v)
	s.blocks(20)
	s.setFork(nil, v)
	s.occur(fMinorityFork, fmt.Sprintf("%s served %d minority heads from %d", v.id, v.minorityOut.Load()-before, tip.number))
}

func (s *chaosSeed) aheadFork() {
	v := s.pick(s.vendors...)
	c := s.chain
	c.mu.Lock()
	tip := c.tipLocked()
	ahead := 20 * chaosBlock
	b := tip
	for i := 1; i <= 20; i++ {
		b = c.childLocked(b, time.Now().Add(time.Duration(i)*chaosBlock), true)
	}
	f := &simFork{tip: b, grows: true, ahead: ahead}
	c.mu.Unlock()
	s.setFork(f, v)
	s.blocks(10)
	lead := f.tip.number - s.chain.tip().number
	s.setFork(nil, v)
	s.occur(fAheadFork, fmt.Sprintf("%s led the canonical head by %d blocks on its fork", v.id, lead))
}

// deepReorg: the logs taps' fork may leave the window, so they end with
// their transport's signal, 1013 (spec §13.1); new ones replace them.
func (s *chaosSeed) deepReorg() {
	depth := chaosReorgWindow + 4
	from := time.Now()
	tip := s.chain.reorg(depth)
	s.allowCloses(from, time.Now(), tip.number-uint64(depth))
	// Closes spread over logsCloseSpread: count them once it has passed.
	time.Sleep(time.Until(from.Add(logsCloseBound())))
	closed := s.closed1013(from)
	if closed > 0 {
		s.dialLogs("after-deep-reorg")
	}
	s.occur(fDeepReorg, fmt.Sprintf("depth %d > reorgWindow %d, new head %d; %d logs clients closed with 1013", depth, chaosReorgWindow, tip.number, closed))
}

func (s *chaosSeed) silentVendor() {
	v := s.pick(s.wsA, s.wsB)
	if v.isDown() {
		v = map[*simVendor]*simVendor{s.wsA: s.wsB, s.wsB: s.wsA}[v]
	}
	stalls := telemetry.MetricUpstreamWebsocketHeadsStallReconnectsTotal.WithLabelValues("test_ws", "evm:123", v.id)
	before := testutil.ToFloat64(stalls)
	live := telemetry.MetricSourceLive.WithLabelValues("test_ws", "evm:123", v.id)
	s.chain.mu.Lock()
	v.silent = true
	s.chain.mu.Unlock()
	wentDead := false
	deadline := time.Now().Add(20 * chaosBlock)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(live) == 0 {
			wentDead = true
		}
		time.Sleep(25 * time.Millisecond)
	}
	s.chain.mu.Lock()
	v.silent = false
	s.chain.mu.Unlock()
	reconnects := testutil.ToFloat64(stalls) - before
	require.True(s.t, wentDead, "%s stops being live while its newHeads is silent", v.id)
	require.Positive(s.t, reconnects, "%s reconnects its silent newHeads", v.id)
	s.occur(fSilent, fmt.Sprintf("%s silent for 20 blocks: not live, %v stall reconnects", v.id, reconnects))
}

func (s *chaosSeed) laggingVendor() {
	var candidates []*simVendor
	for _, v := range s.vendors {
		if !v.isDown() {
			candidates = append(candidates, v)
		}
	}
	v := s.pick(candidates...)
	before := v.lagAnswers.Load()
	start := s.chain.tip().number
	s.chain.mu.Lock()
	v.lag = 40
	s.chain.mu.Unlock()
	s.chain.refresh()
	s.blocks(20)
	s.chain.mu.Lock()
	advanced := v.tip.number
	v.lag = 0
	s.chain.mu.Unlock()
	s.chain.refresh()
	s.occur(fLagging, fmt.Sprintf("%s 40 behind, advanced to %d (head was %d at start), %d latest answers", v.id, advanced, start, v.lagAnswers.Load()-before))
}

func (s *chaosSeed) vendorDown(v *simVendor) {
	v.setDown(true)
	s.blocks(50)
	v.setDown(false)
	s.occur(fDown, fmt.Sprintf("%s down for 50 blocks, refused %d", v.id, v.refused.Load()))
}

// allDown also fails every receipts and logs fetch for longer than the 90s
// outage, so it may close logs clients with 1013 as that one does; new ones
// replace them.
func (s *chaosSeed) allDown() {
	from := time.Now()
	for _, v := range s.vendors {
		v.setDown(true)
	}
	s.blocks(chaosBlocksFor(2 * time.Minute))
	for _, v := range s.vendors {
		v.setDown(false)
	}
	up := time.Now()
	s.addQuiet(interval{from, up})
	s.allowCloses(from, up, 0)
	time.Sleep(time.Until(up.Add(logsCloseBound())))
	closed := s.closed1013(from)
	if closed > 0 {
		s.dialLogs("after-all-down")
	}
	s.occur(fAllDown, fmt.Sprintf("all 4 vendors down for %d blocks; %d logs clients closed with 1013", chaosBlocksFor(2*time.Minute), closed))
}

func (s *chaosSeed) wsCounts() []int64 {
	var out []int64
	for _, v := range []*simVendor{s.wsA, s.wsB} {
		out = append(out, v.connects.Load(), v.subscribes.Load(),
			int64(testutil.ToFloat64(telemetry.MetricUpstreamWebsocketHeadsStallReconnectsTotal.WithLabelValues("test_ws", "evm:123", v.id))))
	}
	return out
}

func (s *chaosSeed) halt() {
	from := time.Now()
	before := s.wsCounts()
	s.chain.mu.Lock()
	s.chain.halted = true
	s.chain.mu.Unlock()
	s.blocks(chaosBlocksFor(3 * time.Minute))
	after := s.wsCounts()
	s.chain.mu.Lock()
	s.chain.halted = false
	s.chain.mu.Unlock()
	s.addQuiet(interval{from, time.Now()})
	require.Equal(s.t, before, after, "a quiet chain reconnects and resubscribes nothing (connects, subscribes, stall reconnects per ws vendor)")
	s.occur(fHalt, fmt.Sprintf("no blocks for %d block times; nothing reconnected", chaosBlocksFor(3*time.Minute)))
}

func (s *chaosSeed) throttle() {
	var before []int64
	until := time.Now().Add(4 * chaosBlock)
	s.chain.mu.Lock()
	for _, v := range s.vendors {
		before = append(before, v.throttled.Load())
		v.throttleUntil = until
	}
	s.chain.mu.Unlock()
	s.blocks(6)
	var got []string
	for i, v := range s.vendors {
		n := v.throttled.Load() - before[i]
		require.Positive(s.t, n, "%s answers 429 during the burst", v.id)
		got = append(got, fmt.Sprintf("%s:%d", v.id, n))
	}
	s.occur(f429, "429s "+strings.Join(got, " "))
}

func (s *chaosSeed) lateAnswers() {
	var before int64
	s.chain.mu.Lock()
	for _, v := range s.vendors {
		before += v.lateAnswers.Load()
		v.late = 2 * chaosBlock
	}
	s.chain.mu.Unlock()
	s.blocks(10)
	var after int64
	s.chain.mu.Lock()
	for _, v := range s.vendors {
		v.late = 0
		after += v.lateAnswers.Load()
	}
	s.chain.mu.Unlock()
	require.Positive(s.t, after-before, "vendors answer late")
	s.occur(fLate, fmt.Sprintf("%d answers 2 block times late", after-before))
}

func (s *chaosSeed) clientLoad() {
	s.breakerMu.Lock()
	s.loadFrom = time.Now()
	s.breakerMu.Unlock()
	// The load lasts the scaled 30s and, on a slow machine, until the
	// budgets refuse it, at most 10 blocks more.
	until := time.Now().Add(time.Duration(chaosBlocksFor(30*time.Second)) * chaosBlock)
	limit := until.Add(10 * chaosBlock)
	var sent, limited atomic.Int64
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for now := time.Now(); now.Before(until) || limited.Load() == 0 && now.Before(limit); now = time.Now() {
				// Distinct params, so the in-flight multiplexer cannot merge them.
				out, status, err := s.rpc("eth_getBalance", fmt.Sprintf("0x%040x", sent.Add(1)), "latest")
				if err == nil && (status == http.StatusTooManyRequests || out["error"] != nil) {
					limited.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	require.Positive(s.t, limited.Load(), "client load exhausts the upstream budgets (%d requests sent)", sent.Load())
	s.occur(fLoad, fmt.Sprintf("%d client requests, %d refused for budget", sent.Load(), limited.Load()))
}

func (s *chaosSeed) split() {
	// The HTTP vendors are no sources here: only the 2 wss vendors vote,
	// and http-a is the one upstream that answers a settle.
	settled := telemetry.MetricChainTrackerSettleTotal.WithLabelValues("test_ws", "evm:123", "decided")
	before := testutil.ToFloat64(settled)
	s.httpB.setDown(true)
	c := s.chain
	c.mu.Lock()
	tip := c.tipLocked()
	// ws-b's sibling of the next canonical block, pushed as it lands.
	canon := c.childLocked(tip, time.Now(), false)
	c.canon = append(c.canon, canon)
	f := &simFork{tip: c.childLocked(tip, time.Now(), true), grows: true}
	s.wsB.fork = f
	c.mu.Unlock()
	c.refresh()
	// ws-b stays live on its growing fork; the settle alone must release
	// the branch it decided to the clients.
	require.Eventually(s.t, func() bool { return testutil.ToFloat64(settled) > before }, 5*chaosBlock, 5*time.Millisecond, "a settle decides the 1-vs-1 split")
	require.Eventually(s.t, func() bool { return s.clientsHave(canon) }, chaosSettleRound, 5*time.Millisecond,
		"clients receive block %d of the settled branch within one settle round while ws-b votes its fork", canon.number)
	s.blocks(5)
	s.setFork(nil, s.wsB)
	s.httpB.setDown(false)
	s.blocks(2)
	decided := testutil.ToFloat64(settled) - before
	s.occur(fSplit, fmt.Sprintf("ws-a vs ws-b at %d, %v settles decided by http-a", tip.number+1, decided))
}

// clientsHave reports whether every open client that is not reading slowly
// holds b.
func (s *chaosSeed) clientsHave(b *simBlock) bool {
	s.mu.Lock()
	clients := slices.Clone(s.clients)
	s.mu.Unlock()
	for _, cl := range clients {
		if cl.slow != nil && cl.slow() {
			continue
		}
		cl.mu.Lock()
		open, h := cl.ended.IsZero(), cl.chain[b.number]
		cl.mu.Unlock()
		if open && h != b.hash {
			return false
		}
	}
	return true
}

func (s *chaosSeed) nonLinking() {
	for _, v := range s.vendors {
		require.Equal(s.t, 1.0, testutil.ToFloat64(telemetry.MetricSourceLive.WithLabelValues("test_ws", "evm:123", v.id)), "%s is live before 2 of 4 votes leave", v.id)
	}
	pair := slices.Clone(s.vendors)
	s.rng.Shuffle(len(pair), func(i, j int) { pair[i], pair[j] = pair[j], pair[i] })
	pair = pair[:2]
	c := s.chain
	c.mu.Lock()
	tip := c.tipLocked()
	forkAt := c.canonAtLocked(tip.number - uint64(chaosReorgWindow) - 6)
	b := forkAt
	for b.number <= tip.number {
		at := time.Now()
		if cb := c.canonAtLocked(b.number + 1); cb != nil {
			at = time.Unix(int64(cb.ts), 0)
		}
		b = c.childLocked(b, at, true)
	}
	f := &simFork{tip: b, grows: true}
	c.mu.Unlock()
	s.setFork(f, pair...)
	s.blocks(10)
	s.setFork(nil, pair...)
	s.blocks(2)
	s.occur(fNonLinking, fmt.Sprintf("%s and %s on a branch from %d, %d below the window", pair[0].id, pair[1].id, forkAt.number, tip.number-forkAt.number))
}

func (s *chaosSeed) restart() {
	from := time.Now()
	s.stopERPC()
	m := s.pick(s.wsA, s.wsB)
	c := s.chain
	c.mu.Lock()
	tip := c.tipLocked()
	f := &simFork{tip: c.childLocked(tip.parent, time.Now(), true), grows: true}
	m.fork = f
	for _, v := range s.vendors {
		if v != m {
			v.holdUntil = time.Now().Add(time.Hour)
		}
	}
	c.mu.Unlock()
	c.refresh()
	s.startERPC()
	tr := s.tracker()
	s.dialClient("restart-1", nil)
	var first indexer.Cursor
	var from1 string
	require.Eventually(s.t, func() bool {
		var ok bool
		first, from1, ok = s.hook.firstHeader(tr)
		return ok
	}, 10*time.Second, 10*time.Millisecond, "the restarted tracker gets a header")
	c.mu.Lock()
	for _, v := range s.vendors {
		v.holdUntil = time.Time{}
	}
	var pushes []simPush
	for _, v := range s.vendors {
		if v.ws && v != m {
			pushes = v.pushesLocked(pushes)
		}
	}
	c.mu.Unlock()
	writePushes(pushes)
	s.dialClient("restart-2", nil)
	s.dialLogs("restart")
	s.blocks(3)
	s.setFork(nil, m)
	s.addQuiet(interval{from, time.Now()})
	b := s.chain.block(first.Hash)
	require.NotNil(s.t, b)
	require.True(s.t, b.minority && from1 == m.id, "the first header after the restart is %s's minority head (got %d from %s)", m.id, first.Number, from1)
	s.occur(fRestart, fmt.Sprintf("restarted; first header %d from %s on its minority fork", first.Number, m.id))
}

func (s *chaosSeed) orphanCached() {
	c := s.chain
	var tip *simBlock
	var h1 string
	require.Eventually(s.t, func() bool {
		tip = c.tip()
		out, _, err := s.rpc("eth_getBlockByNumber", fmt.Sprintf("0x%x", tip.number), false)
		if err != nil {
			return false
		}
		res, _ := out["result"].(map[string]interface{})
		h1, _ = res["hash"].(string)
		return h1 == "0x"+hex.EncodeToString(tip.hash[:])
	}, 5*time.Second, 20*time.Millisecond, "a client reads the head by number")
	calls := func() (n int) {
		for _, v := range s.vendors {
			n += v.byNumberCalls(tip.number)
		}
		return n
	}
	// The cache is written after the answer returns: repeat until a repeat
	// reaches no vendor.
	require.Eventually(s.t, func() bool {
		before := calls()
		out, _, err := s.rpc("eth_getBlockByNumber", fmt.Sprintf("0x%x", tip.number), false)
		if err != nil {
			return false
		}
		res, _ := out["result"].(map[string]interface{})
		return res["hash"] == h1 && calls() == before
	}, 2*time.Second, 20*time.Millisecond, "a repeat answer for block %d comes from the cache", tip.number)
	before := calls()
	c.mu.Lock()
	depth := int(c.tipLocked().number-tip.number) + 1
	c.mu.Unlock()
	c.reorg(depth)
	s.blocks(3)
	require.NotEqual(s.t, tip.hash, c.canonAt(tip.number).hash)
	s.occur(fOrphanCached, fmt.Sprintf("block %d %s cached by number, then reorged out (depth %d); vendors served %d by-number fetches of it after", tip.number, h1[:10], depth, calls()-before))
}

func (s *chaosSeed) mailboxBurst() {
	v := s.pick(s.wsA, s.wsB)
	heads := func() (n float64) {
		for _, o := range []string{"voted", "duplicate", "gated_authority", "gated_identity", "malformed"} {
			n += testutil.ToFloat64(telemetry.MetricChainTrackerSourceHeadsTotal.WithLabelValues("test_ws", "evm:123", v.id, o))
		}
		return n
	}
	stalled, release := s.hook.stallOn(v.id)
	select {
	case <-stalled:
	case <-time.After(5 * time.Second):
		s.t.Fatalf("the tracker never handled a header from %s", v.id)
	}
	from := time.Now()
	before := heads()
	pushed := v.burst(chaosMailboxBurst)
	require.Eventually(s.t, func() bool { return heads()-before >= float64(pushed) }, 5*time.Second, 10*time.Millisecond, "intake takes the burst")
	handled := s.hook.countFrom(v.id)
	release()
	s.addQuiet(interval{from, time.Now()})
	s.blocks(3)
	got := s.hook.countFrom(v.id) - handled
	require.Less(s.t, got, int64(pushed), "the mailbox sheds superseded headers once full")
	s.occur(fMailbox, fmt.Sprintf("%s pushed %d headers while the tracker stalled; it handled %d", v.id, pushed, got))
}

func trackerResets() float64 {
	return testutil.ToFloat64(telemetry.MetricChainTrackerResetTotal.WithLabelValues("test_ws", "evm:123"))
}

func (s *chaosSeed) panicOnce() {
	resets := trackerResets()
	tip := s.chain.tip().number
	done := s.hook.panicAfter(tip, 1)
	var r panicReport
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		s.t.Fatalf("no header above %d reached the tracker", tip)
	}
	require.Equal(s.t, r.before, r.after, "a single panic leaves the pins unchanged")
	s.blocks(4)
	require.Equal(s.t, resets, trackerResets(), "a single panic does not reset the tracker")
	s.occur(fPanicOnce, fmt.Sprintf("header %d panicked; head stayed %d", r.number, r.before.Number))
}

func (s *chaosSeed) panicThrice() {
	resets := trackerResets()
	tip := s.chain.tip().number
	done := s.hook.panicAfter(tip, 3)
	var r panicReport
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		s.t.Fatalf("no header above %d reached the tracker", tip)
	}
	require.Eventually(s.t, func() bool { return trackerResets() == resets+1 }, 2*time.Second, 10*time.Millisecond, "3 panics in a row reset the tracker")
	s.blocks(6)
	s.occur(fPanicThrice, fmt.Sprintf("header %d and the next 2 messages panicked; tracker reset", r.number))
}

//
// --- tracker hook ---
//

type panicReport struct {
	number        uint64
	before, after indexer.Cursor
}

// chaosHook acts on the current instance's network-wide tracker only.
type chaosHook struct {
	s *chaosSeed

	mu        sync.Mutex
	first     map[*evm.ChainTracker]indexer.Cursor
	firstFrom map[*evm.ChainTracker]string
	counts    map[string]int64

	stallUp   string
	stalled   chan struct{}
	unstall   chan struct{}
	panicAbv  uint64
	panicLeft int
	panicked  *panicReport
	panicDone chan panicReport
}

func (h *chaosHook) on(tr *evm.ChainTracker, upstreamId string, hdr *indexer.Cursor) {
	h.mu.Lock()
	if hdr != nil && upstreamId != "" {
		if _, ok := h.first[tr]; !ok {
			h.first[tr], h.firstFrom[tr] = *hdr, upstreamId
		}
	}
	if tr != h.s.tracker() {
		h.mu.Unlock()
		return
	}
	if hdr != nil {
		if h.counts == nil {
			h.counts = make(map[string]int64)
		}
		h.counts[upstreamId]++
	}
	if r := h.panicked; r != nil {
		// The message after the one that panicked first: the pins it left.
		h.panicked = nil
		r.after, _ = tr.Tip()
		done := h.panicDone
		if h.panicLeft > 0 {
			h.panicLeft--
			h.mu.Unlock()
			done <- *r
			panic("chaos: tracker message panics again")
		}
		h.mu.Unlock()
		done <- *r
		return
	}
	if h.panicDone != nil && h.panicLeft > 0 && hdr != nil && upstreamId != "" && hdr.Number > h.panicAbv && h.panicked == nil {
		h.panicLeft--
		before, _ := tr.Tip()
		h.panicked = &panicReport{number: hdr.Number, before: before}
		h.mu.Unlock()
		panic("chaos: header makes the tracker panic")
	}
	if h.stallUp != "" && h.stallUp == upstreamId && hdr != nil {
		h.stallUp = ""
		stalled, unstall := h.stalled, h.unstall
		h.mu.Unlock()
		close(stalled)
		select {
		case <-unstall:
		case <-time.After(10 * time.Second):
		}
		return
	}
	h.mu.Unlock()
}

func (h *chaosHook) firstHeader(tr *evm.ChainTracker) (indexer.Cursor, string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.first[tr]
	return c, h.firstFrom[tr], ok
}

func (h *chaosHook) countFrom(upstreamId string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[upstreamId]
}

// stallOn blocks the tracker on the next header from upstreamId until
// release.
func (h *chaosHook) stallOn(upstreamId string) (stalled <-chan struct{}, release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stallUp, h.stalled, h.unstall = upstreamId, make(chan struct{}), make(chan struct{})
	unstall := h.unstall
	return h.stalled, func() { close(unstall) }
}

// panicAfter makes the first source header above number panic, and the
// messages right after it until times panics ran in a row. The report
// carries the pins before the first panic and after it.
func (h *chaosHook) panicAfter(number uint64, times int) <-chan panicReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.panicAbv, h.panicLeft, h.panicked = number, times, nil
	h.panicDone = make(chan panicReport, times+1)
	return h.panicDone
}

//
// --- verdict ---
//

func overlap(a, b interval) time.Duration {
	from, to := a.from, a.to
	if b.from.After(from) {
		from = b.from
	}
	if b.to.Before(to) {
		to = b.to
	}
	if to.Before(from) {
		return 0
	}
	return to.Sub(from)
}

func (s *chaosSeed) judge(end time.Time) {
	t := s.t
	bound := 3 * max(3*chaosBlock, 2*time.Second)
	s.breakerMu.Lock()
	opened, openedAt, loadFrom := s.breakerOpened, s.breakerAt, s.loadFrom
	s.breakerMu.Unlock()
	if opened != "" {
		require.False(t, loadFrom.IsZero() || openedAt.Before(loadFrom),
			"tracker fetches alone open no breaker: %s opened at %s, before any client load", opened, openedAt.Format(time.StampMilli))
	}
	s.mu.Lock()
	clients, quiet := slices.Clone(s.clients), slices.Clone(s.quiet)
	logs, closeOK := slices.Clone(s.logs), slices.Clone(s.closeOK)
	s.mu.Unlock()
	for _, cl := range logs {
		cl.mu.Lock()
		require.Empty(t, cl.problems, "logs client %s", cl.name)
		closed := !cl.byTest && cl.closeCode == websocket.CloseTryAgainLater
		var fork uint64
		if closed {
			allowed := false
			for _, w := range closeOK {
				if !cl.ended.Before(w.from) && !cl.ended.After(w.to) {
					allowed = true
					if w.fork != 0 && (fork == 0 || w.fork < fork) {
						fork = w.fork
					}
				}
			}
			require.True(t, allowed, "logs client %s closed with 1013 at %s with no fault that calls for it", cl.name, cl.ended.Format(time.StampMilli))
		}
		// Clients open at the end were judged up to the head. A client that
		// ended earlier is judged where a reorg after its end can no
		// longer reach: 3 blocks, or past the window when 1013 closed it,
		// and below the fork of the deep reorg that closed it.
		if cl.first != 0 && cl.ended.Before(end) {
			reach := uint64(4)
			if closed {
				reach = uint64(chaosReorgWindow) + 5
			}
			if cl.endTip > reach && cl.endTip-reach >= cl.first {
				to := min(cl.high, cl.endTip-reach)
				if fork != 0 {
					to = min(to, fork-1)
				}
				if to >= cl.first {
					require.Empty(t, cl.logsDiffLocked(s, cl.first, to), "logs client %s, blocks %d..%d", cl.name, cl.first, to)
				}
			}
		}
		cl.mu.Unlock()
	}
	for _, cl := range clients {
		cl.mu.Lock()
		require.Empty(t, cl.problems, "client %s", cl.name)
		require.NotZero(t, cl.last, "client %s got no head", cl.name)

		// Waits, excluding the time no vendor served the chain.
		stamps := []time.Time{cl.subscribed}
		for _, n := range cl.notes {
			stamps = append(stamps, n.at)
		}
		stop := end
		if !cl.ended.IsZero() && cl.ended.Before(end) {
			stop = cl.ended
		}
		stamps = append(stamps, stop)
		for i := 1; i < len(stamps); i++ {
			gap := interval{stamps[i-1], stamps[i]}
			wait := gap.to.Sub(gap.from)
			for _, q := range quiet {
				wait -= overlap(gap, q)
			}
			require.LessOrEqual(t, wait, bound, "client %s waited %s for a head after %s while a vendor served", cl.name, wait, gap.from.Format(time.StampMilli))
		}

		// The final chain equals the canonical chain from the client's
		// first block, or from its permitted restart, on. A client the
		// eRPC restart ended is judged up to where a 3-block reorg after
		// its end can no longer reach.
		from, to := max(cl.first, cl.floor), cl.last
		if !cl.ended.IsZero() && cl.ended.Before(end) && cl.endTip > 4 {
			to = min(to, cl.endTip-4)
		}
		for n := from; n <= to; n++ {
			want := s.chain.canonAt(n)
			got, ok := cl.chain[n]
			require.True(t, ok, "client %s misses block %d (first %d, last %d)", cl.name, n, cl.first, cl.last)
			require.NotNil(t, want)
			require.Equal(t, hex.EncodeToString(want.hash[:]), hex.EncodeToString(got[:]), "client %s block %d", cl.name, n)
		}
		cl.mu.Unlock()
	}
}

//
// --- receipts faults ---
//

// logsCloseBound is how long after a receipts fault ends logs clients may
// still be closed with 1013: the receipts wait, the close spread, slack.
func logsCloseBound() time.Duration {
	return logsReceiptsWait.Load() + logsCloseSpread.Load() + 5*chaosBlock
}

func (s *chaosSeed) allowCloses(from, to time.Time, fork uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeOK = append(s.closeOK, closeWindow{interval{from, to.Add(logsCloseBound())}, fork})
}

// setVendor applies fn to every upstream of vendor name.
func (s *chaosSeed) setVendor(name string, fn func(v *simVendor)) {
	s.chain.mu.Lock()
	defer s.chain.mu.Unlock()
	for _, v := range s.vendorOf(name) {
		fn(v)
	}
}

func otherVendor(name string) string {
	if name == "vendor-a" {
		return "vendor-b"
	}
	return "vendor-a"
}

// rotated counts the canonical blocks from..to that vendor bad was asked
// for and the other vendor then served in full.
func (s *chaosSeed) rotated(bad string, from, to uint64) int {
	badUps, goodUps := s.vendorOf(bad), s.vendorOf(otherVendor(bad))
	c := s.chain
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k := from; k <= to; k++ {
		b := c.canonAtLocked(k)
		if b == nil {
			continue
		}
		asked, served := false, false
		for _, v := range badUps {
			asked = asked || v.receiptsCalls[b.hash] > 0
		}
		for _, v := range goodUps {
			served = served || v.served[b.hash]
		}
		if asked && served {
			n++
		}
	}
	return n
}

// callsTo sums the receipts calls vendor name got for canonical blocks
// from..to.
func (s *chaosSeed) callsTo(name string, from, to uint64) int {
	n := 0
	for k := from; k <= to; k++ {
		if b := s.chain.canonAt(k); b != nil {
			for _, v := range s.vendorOf(name) {
				n += v.receiptsCallsOf(b.hash)
			}
		}
	}
	return n
}

// receiptsVendor is the vendor of the first upstream a receipts sweep asks
// now: the selection order, past upstreams whose vendor is down. A fault set
// on any other vendor is never exercised while the first one answers.
func (s *chaosSeed) receiptsVendor() string {
	nw := s.nw.Load()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x` + strings.Repeat("0", 64) + `"]}`))
	req.SetNetwork(nw)
	byId := make(map[string]*simVendor, len(s.vendors))
	for _, v := range s.vendors {
		byId[v.id] = v
	}
	for _, u := range nw.trackerUpstreams(context.Background(), req, "eth_getBlockReceipts", "") {
		if v := byId[u.Id()]; v != nil && !v.isDown() {
			return v.vendor
		}
	}
	s.t.Fatalf("no upstream serves a receipts sweep")
	return ""
}

// faultyVendor gives the receipts fault set to the vendor a sweep asks
// first, for at least 10 and at most 30 blocks: until that vendor was asked
// and, when rotates, the other vendor served a block it was asked for. It
// returns the vendor, the blocks of the fault, the receipts calls the vendor
// got in them, and how many blocks rotated.
func (s *chaosSeed) faultyVendor(rotates bool, set func(v *simVendor, on bool)) (name string, from, to uint64, calls, rotated int) {
	require.NotEmpty(s.t, s.openLogs(), "logs clients are open, so receipts are wanted")
	name = s.receiptsVendor()
	from = s.chain.tip().number + 1
	s.setVendor(name, func(v *simVendor) { set(v, true) })
	for i := 0; i < 30; i += 2 {
		s.blocks(2)
		to = s.chain.tip().number
		calls, rotated = s.callsTo(name, from, to), s.rotated(name, from, to)
		if i >= 10 && calls > 0 && (!rotates || rotated > 0) {
			break
		}
	}
	s.setVendor(name, func(v *simVendor) { set(v, false) })
	require.Positive(s.t, calls, "%s, first in the receipts sweep, is asked for blocks %d..%d during its fault", name, from, to)
	if rotates {
		require.Positive(s.t, rotated, "blocks %s answered wrong in %d..%d are refetched from %s", name, from, to, otherVendor(name))
	}
	return name, from, to, calls, rotated
}

func (s *chaosSeed) nullReceipts() {
	name, from, to, calls, _ := s.faultyVendor(false, func(v *simVendor, on bool) { v.nullReceipts = on })
	s.occur(fNullReceipts, fmt.Sprintf("%s answered null to %d receipts calls, blocks %d..%d", name, calls, from, to))
}

func (s *chaosSeed) truncReceipts() {
	name, from, to, _, rotated := s.faultyVendor(true, func(v *simVendor, on bool) { v.truncReceipts = on })
	s.path(pathRotation, fmt.Sprintf("%d truncated blocks refetched from %s", rotated, otherVendor(name)))
	s.occur(fTruncReceipts, fmt.Sprintf("%s's 2 upstreams dropped the last 10 receipts, blocks %d..%d; %d refetched from the other vendor", name, from, to, rotated))
}

func (s *chaosSeed) wrongBloom() {
	name, from, to, _, rotated := s.faultyVendor(true, func(v *simVendor, on bool) { v.wrongBloom = on })
	s.path(pathRotation, fmt.Sprintf("%d wrong-bloom blocks refetched from %s", rotated, otherVendor(name)))
	s.occur(fWrongBloom, fmt.Sprintf("%s answered a wrong bloom, blocks %d..%d; %d refetched from the other vendor", name, from, to, rotated))
}

func (s *chaosSeed) noReceipts() {
	name, from, to, calls, _ := s.faultyVendor(false, func(v *simVendor, on bool) { v.noReceipts = on })
	s.occur(fNoReceipts, fmt.Sprintf("%s refused %d eth_getBlockReceipts calls, blocks %d..%d", name, calls, from, to))
}

func (s *chaosSeed) lateReceipts() {
	var before int64
	s.chain.mu.Lock()
	for _, v := range s.vendors {
		before += v.lateAnswers.Load()
		v.lateReceipts = 2 * chaosBlock
	}
	s.chain.mu.Unlock()
	s.blocks(10)
	var after int64
	s.chain.mu.Lock()
	for _, v := range s.vendors {
		v.lateReceipts = 0
		after += v.lateAnswers.Load()
	}
	s.chain.mu.Unlock()
	require.Positive(s.t, after-before, "vendors answer receipts late")
	s.occur(fLateReceipts, fmt.Sprintf("%d receipts answers 2 block times late", after-before))
}

func (s *chaosSeed) getLogsCalls() int64 {
	var n int64
	for _, v := range s.vendors {
		n += v.getLogsCalls.Load()
	}
	return n
}

// receiptsDown13 fails receipts on every vendor for 13s, scaled, on blocks
// with matching logs. Scaled, 13s is 2 block times, and a job's retry waits
// out the key's backoff, so the outage holds, at most 20 blocks, until a
// job has failed for LogsFallbackAfter and switched to eth_getLogs.
func (s *chaosSeed) receiptsDown13() {
	require.NotEmpty(s.t, s.openLogs(), "logs clients are open, so receipts are wanted")
	before := s.getLogsCalls()
	from := time.Now()
	c := s.chain
	c.mu.Lock()
	c.plan = []simKind{simMatching, simMatching, simMatching}
	c.receiptsDown = from.Add(chaosScaled(13 * time.Second))
	c.mu.Unlock()
	var calls int64
	for range 20 {
		s.blocks(1)
		if calls = s.getLogsCalls() - before; calls > 0 {
			break
		}
		c.mu.Lock()
		if until := time.Now().Add(chaosBlock); until.After(c.receiptsDown) {
			c.receiptsDown = until
		}
		c.mu.Unlock()
	}
	c.mu.Lock()
	held := c.receiptsDown.Sub(from)
	c.mu.Unlock()
	require.Positive(s.t, calls, "receipts failing past %s on every vendor switch blocks to eth_getLogs", evm.LogsFallbackAfter.Load())
	s.blocks(4)
	s.path(pathFallback, fmt.Sprintf("%d eth_getLogs calls by blockHash", calls))
	s.occur(fReceiptsDown13, fmt.Sprintf("receipts failed on every vendor for %s; %d eth_getLogs calls", held.Round(time.Millisecond), calls))
}

// awaitCloses waits until every logs client of open was closed with 1013,
// then opens new ones.
func (s *chaosSeed) awaitCloses(open []*chaosLogsClient, name string) {
	require.Eventually(s.t, func() bool {
		for _, cl := range open {
			cl.mu.Lock()
			closed := cl.closeCode == websocket.CloseTryAgainLater
			cl.mu.Unlock()
			if !closed {
				return false
			}
		}
		return true
	}, logsCloseBound()+5*time.Second, 20*time.Millisecond, "every logs client is closed with 1013")
	s.dialLogs(name)
}

func (s *chaosSeed) blockDown60() {
	from := time.Now()
	open := s.openLogs()
	c := s.chain
	c.mu.Lock()
	c.failNext, c.failUntil = true, time.Now().Add(time.Hour)
	c.mu.Unlock()
	var b *simBlock
	require.Eventually(s.t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.failNext {
			return false
		}
		b = c.byHash[c.failHash]
		c.failUntil = time.Now().Add(chaosScaled(60*time.Second) + 2*chaosBlock)
		return true
	}, 10*time.Second, 5*time.Millisecond, "a block with matching logs is minted")
	c.mu.Lock()
	until := c.failUntil
	c.mu.Unlock()
	s.allowCloses(from, until, 0)
	time.Sleep(time.Until(until))
	s.awaitCloses(open, "after-block")
	s.occur(fBlockDown60, fmt.Sprintf("block %d failed on every vendor for %s; %d logs clients closed with 1013", b.number, chaosScaled(60*time.Second), s.closed1013(from)))
}

func (s *chaosSeed) receiptsDown90() {
	from := time.Now()
	open := s.openLogs()
	until := from.Add(chaosScaled(90 * time.Second))
	s.chain.mu.Lock()
	s.chain.receiptsDown, s.chain.logsDown = until, until
	s.chain.mu.Unlock()
	s.allowCloses(from, until, 0)
	var refusedAt time.Time
	var refusal map[string]interface{}
	for time.Now().Before(until) && refusedAt.IsZero() {
		conn, e := s.subscribeLogs(0)
		if conn != nil {
			_ = conn.Close()
			time.Sleep(20 * time.Millisecond)
			continue
		}
		refusedAt, refusal = time.Now(), e
	}
	require.False(s.t, refusedAt.IsZero(), "a logs subscribe is refused while every receipts and logs fetch fails")
	require.GreaterOrEqual(s.t, refusedAt.Sub(from), evm.ReceiptsOutageAfter.Load(), "logs subscribes are refused only after %s of failures", evm.ReceiptsOutageAfter.Load())
	require.EqualValues(s.t, common.JsonRpcErrorUnsupportedException, refusal["code"], "the refusal is ErrNoSubscriptionSource: %v", refusal)
	time.Sleep(time.Until(until))
	s.awaitCloses(open, "after-outage")
	s.occur(fReceiptsDown90, fmt.Sprintf("every receipts and logs fetch failed for %s; subscribe refused after %s; %d logs clients closed with 1013",
		chaosScaled(90*time.Second), refusedAt.Sub(from).Round(time.Millisecond), s.closed1013(from)))
}

// chainBloom gives every vendor's receipts a bloom other than the header's:
// the network's check turns off and their logs reach clients; checkSkips
// later proves it turned on again.
func (s *chaosSeed) chainBloom() {
	c := s.chain
	c.mu.Lock()
	c.badBloom = true
	from := c.tipLocked().number + 1
	c.mu.Unlock()
	s.blocks(4)
	var got uint64
	require.Eventually(s.t, func() bool {
		to := s.chain.tip().number
		for _, cl := range s.openLogs() {
			cl.mu.Lock()
			for k, n := range cl.held {
				if n >= from && n <= to && s.chain.canonAt(n) != nil && s.chain.canonAt(n).hash == k.block {
					got = n
				}
			}
			cl.mu.Unlock()
		}
		return got != 0
	}, 30*chaosBlock, 20*time.Millisecond, "logs of blocks whose receipts no vendor answers with the header's bloom reach clients")
	c.mu.Lock()
	c.badBloom = false
	c.mu.Unlock()
	s.path(pathBloomOff, fmt.Sprintf("logs of block %d delivered from mismatching receipts", got))
	s.occur(fChainBloom, fmt.Sprintf("every vendor's receipts mismatched from block %d; check off, logs of block %d delivered", from, got))
}

func httptestServer(t *testing.T, h http.Handler) string {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// bloomFollower holds each block's first receipts call and answers it with
// a wrong bloom. While the tracker's sweep for a block waits on it, a
// client asks the same receipts: it joins the sweep, and gets only an
// answer whose blooms are the block's.
func (s *chaosSeed) bloomFollower() {
	require.NotEmpty(s.t, s.openLogs(), "logs clients are open, so receipts are wanted")
	c := s.chain
	c.mu.Lock()
	c.trapOn, c.traps = true, make(map[[32]byte]*simTrap)
	c.mu.Unlock()
	asked := make(map[[32]byte]bool)
	joined, failed := 0, 0
	for deadline := time.Now().Add(40 * chaosBlock); joined < 2 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		var b *simBlock
		c.mu.Lock()
		for h, tr := range c.traps {
			if !asked[h] && tr.answeredAt.IsZero() {
				b = c.byHash[h]
				break
			}
		}
		c.mu.Unlock()
		if b == nil {
			continue
		}
		asked[b.hash] = true
		sent := time.Now()
		out, _, err := s.rpc("eth_getBlockReceipts", "0x"+hex.EncodeToString(b.hash[:]))
		require.NoError(s.t, err)
		c.mu.Lock()
		tr := c.traps[b.hash]
		answeredAt, during := tr.answeredAt, tr.during
		c.mu.Unlock()
		if out["result"] == nil {
			failed++
			continue
		}
		var got []struct {
			LogsBloom string `json:"logsBloom"`
		}
		raw, _ := json.Marshal(out["result"])
		require.NoError(s.t, json.Unmarshal(raw, &got), "receipts answer %s", raw)
		want := b.receipts(false, 0)
		require.Len(s.t, got, len(want), "block %d's receipts reach the client in full", b.number)
		for i := range want {
			require.Equal(s.t, want[i]["logsBloom"], got[i].LogsBloom, "block %d receipt %d reaches the client with the block's bloom", b.number, i)
		}
		// Sent early in the hold, and no other call reached a vendor
		// while it lasted: the client waited on the tracker's sweep.
		if during == 0 && answeredAt.Sub(sent) > chaosBlock {
			joined++
		}
	}
	c.mu.Lock()
	c.trapOn = false
	held := len(c.traps)
	c.mu.Unlock()
	require.Positive(s.t, joined, "a client joins the tracker's receipts sweep while its first answer is held (%d blocks held)", held)
	s.occur(fBloomFollower, fmt.Sprintf("%d blocks' first receipts answers held with a wrong bloom; %d client requests joined the tracker's sweep and got the right blooms; %d got an error", held, joined, failed))
}
