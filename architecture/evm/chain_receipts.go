package evm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/architecture/evm/logfilter"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
)

// Receipts skip bounds (spec §11); variables so tests run fast.
var (
	bloomTrustRun = 8 // matching blocks with transactions in a row before the bloom skip
	bloomSeenCap  = 1024
)

// Receipts timings (spec §10); exported so end-to-end tests can scale them.
var (
	// LogsFallbackAfter: receipts failing this long on every upstream switch
	// a block to eth_getLogs by blockHash.
	LogsFallbackAfter = util.NewDuration(5 * time.Second)
	// ReceiptsOutageAfter: every receipts and logs fetch failing this long
	// refuses new logs subscribes until one succeeds.
	ReceiptsOutageAfter = util.NewDuration(60 * time.Second)
	// ReceiptsProbeCeiling caps the spacing of the outage's head-block
	// fetches, which otherwise run every block time.
	ReceiptsProbeCeiling = util.NewDuration(12 * time.Second)
)

var (
	errReceiptsAnswer = errors.New("receipts answer fails the answer check")
	errBloomMismatch  = errors.New("receipts bloom differs from the header's logsBloom")
)

type bloomVerdict uint8

const (
	bloomUnknown  bloomVerdict = iota // no held header with a logsBloom to check against
	bloomMatch                        // the OR of the answer's blooms is the header's
	bloomMismatch                     // it is not
)

// bloomGate is the network's receipts profile and its "bloom trusted for
// skipping" (spec §6.9, §11), shared by its trackers. Trust allows the
// bloom skip: set after bloomTrustRun matching blocks with transactions
// in a row, cleared by any mismatch. The bloom check's suspension is the
// network's checkSuspensions, which never touches trust.
type bloomGate struct {
	network common.Network
	once    sync.Once
	profile bloomProfile // read from the network's config on first use

	trusted atomic.Bool

	mu    sync.Mutex
	txRun int
	seen  map[[32]byte]bloomVerdict
	order [][32]byte
}

// bloomProfile is what the network's config says of the check.
type bloomProfile struct {
	checkOff bool // the chain profile disables bloomMatch: no check, no skips
	skipsOff bool // the chain profile disables blockHashRecompute: neither skip
	// emptyOff: the chain profile disables transactionsRootRecompute, so
	// the root may miss committed system transactions and their receipts.
	emptyOff bool
	// agreeLogs: integrity is on, so logs from eth_getLogs by blockHash,
	// which stand in for receipts that keep failing, are held only once
	// vendors that meet the evidence quorum answered the same logs.
	agreeLogs bool
	// logIndexes: integrity checks logIndexContiguity, so logs from
	// eth_getLogs by blockHash must run 0..N-1 as receipts' do.
	logIndexes bool
}

func newBloomGate(n common.Network) *bloomGate {
	return &bloomGate{network: n, seen: make(map[[32]byte]bloomVerdict)}
}

func (g *bloomGate) prof() *bloomProfile {
	g.once.Do(func() {
		n := g.network
		if n == nil {
			return
		}
		if cfg := n.Config(); cfg != nil && cfg.Evm != nil {
			for _, id := range integrity.ChainProfileDisables(cfg.Evm.ChainId) {
				switch id {
				case "bloomMatch":
					g.profile.checkOff = true
				case "blockHashRecompute":
					g.profile.skipsOff = true
				case "transactionsRootRecompute":
					g.profile.emptyOff = true
				}
			}
		}
		cs, _, _, _ := resolveIntegrity(n, nil)
		g.profile.agreeLogs = len(cs) > 0
		g.profile.logIndexes = cs.For("logIndexContiguity").Enabled
	})
	return &g.profile
}

// checking reports whether a mismatching answer fails its fetch: the chain
// profile keeps the check and the network has not suspended it.
func (g *bloomGate) checking() bool {
	if g.prof().checkOff {
		return false
	}
	s := suspensionsOf(g.network)
	return s == nil || s.mode(bloomCheckID) == integrity.CheckEnforced
}

// bloomSkips: a block no registered MayMatch wants need not be fetched.
// Only for a header whose hash recomputes, so its bloom is the block's.
func (g *bloomGate) bloomSkips(h *chainHeader) bool {
	p := g.prof()
	return !p.checkOff && !p.skipsOff && g.trusted.Load() && h.hashRecomputes()
}

// emptySkips: the block has no transactions, so no receipts to fetch.
func (g *bloomGate) emptySkips(h *chainHeader) bool {
	p := g.prof()
	return !p.checkOff && !p.skipsOff && !p.emptyOff && h.emptyTxs && h.hashRecomputes()
}

// newLocked reports whether v is a new outcome for block h; a block counts
// once per outcome.
func (g *bloomGate) newLocked(h [32]byte, v bloomVerdict) bool {
	old, seen := g.seen[h]
	if seen && old == v {
		return false
	}
	if !seen {
		g.order = append(g.order, h)
		if len(g.order) > bloomSeenCap {
			delete(g.seen, g.order[0])
			g.order = g.order[1:]
		}
	}
	g.seen[h] = v
	return true
}

// noted records block h's bloom verdict for trust, once per block and
// verdict.
func (g *bloomGate) noted(h *chainHeader, v bloomVerdict) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.newLocked(h.hash, v) {
		return
	}
	switch {
	case v == bloomMismatch:
		g.txRun = 0
		g.trusted.Store(false)
	case !h.emptyTxs:
		if g.txRun++; g.txRun >= bloomTrustRun {
			g.trusted.Store(true)
		}
	}
}

// heldReceipts is one block's receipts as the feed holds them: verbatim,
// parsed once, indexed by address and topic0, with the logs each filter
// matched as positions.
type heldReceipts struct {
	hash     [32]byte
	h        *chainHeader // the header the answer was checked against; nil when none was held
	number   int64        // -1 when unknown
	raw      json.RawMessage
	fromLogs bool // built from eth_getLogs: receipts that carry only their logs
	bloom    bloomVerdict
	// digest is what vendors must agree on for logs from eth_getLogs.
	digest logsDigest

	logs     []json.RawMessage
	addr     []string   // per log, lowercase
	topics   [][]string // per log, lowercase
	byAddr   map[string][]int32
	byTopic0 map[string][]int32
	matches  sync.Map // filter key -> []int32

	view     sync.Once
	receipts []integrity.Receipt
}

type receiptLite struct {
	BlockHash   string            `json:"blockHash"`
	BlockNumber string            `json:"blockNumber"`
	LogsBloom   string            `json:"logsBloom"`
	Logs        []json.RawMessage `json:"logs"`
}

type logLite struct {
	Address          string   `json:"address"`
	Topics           []string `json:"topics"`
	Data             string   `json:"data"`
	BlockHash        string   `json:"blockHash"`
	BlockNumber      string   `json:"blockNumber"`
	TransactionHash  string   `json:"transactionHash"`
	TransactionIndex string   `json:"transactionIndex"`
	LogIndex         string   `json:"logIndex"`
}

// logsDigest is one block's logs from eth_getLogs as vendors must agree on
// them with integrity on: how many, the last logIndex, and a hash over
// every log's transaction, index, address, topics and data. A vendor that
// caps or drops logs answers another digest.
type logsDigest struct {
	count int
	last  int64 // -1 with no logs
	set   [32]byte
}

// method is the method that answered r.
func (r *heldReceipts) method() string {
	if r.fromLogs {
		return "eth_getLogs"
	}
	return "eth_getBlockReceipts"
}

func newHeldReceipts(hash [32]byte, h *chainHeader, raw json.RawMessage) *heldReceipts {
	r := &heldReceipts{hash: hash, h: h, number: -1, raw: raw,
		byAddr: make(map[string][]int32), byTopic0: make(map[string][]int32)}
	if h != nil {
		r.number = int64(h.number)
	}
	return r
}

// addLog parses and indexes one log.
func (r *heldReceipts) addLog(raw json.RawMessage) (logLite, error) {
	var l logLite
	if err := common.SonicCfg.Unmarshal(raw, &l); err != nil {
		return l, fmt.Errorf("%w: log: %v", errReceiptsAnswer, err)
	}
	i := int32(len(r.logs))
	addr := strings.ToLower(l.Address)
	topics := make([]string, len(l.Topics))
	for k, tp := range l.Topics {
		topics[k] = strings.ToLower(tp)
	}
	r.logs = append(r.logs, raw)
	r.addr = append(r.addr, addr)
	r.topics = append(r.topics, topics)
	r.byAddr[addr] = append(r.byAddr[addr], i)
	if len(topics) > 0 {
		r.byTopic0[topics[0]] = append(r.byTopic0[topics[0]], i)
	}
	return l, nil
}

func isNullResult(result []byte) bool {
	r := bytes.TrimSpace(result)
	return len(r) == 0 || bytes.Equal(r, []byte("null"))
}

// judgedBloom is h's logsBloom when an answer's bloom may be judged
// against it: held, on a header whose hash recomputes, so the bloom is the
// block's. Otherwise nil: a mismatch may be the header's fault.
func judgedBloom(h *chainHeader) []byte {
	if h == nil || h.bloom == nil || !h.hashRecomputes() {
		return nil
	}
	return h.bloom
}

// parseReceipts is the answer check of an eth_getBlockReceipts answer for
// block hash (spec §10): non-null, every receipt names hash, and non-empty
// unless h's transactionsRoot is the empty-trie root. Anything else fails
// the fetch; it never reads as "no logs". With h's judgedBloom, the
// verdict compares the OR of the receipt blooms with it.
func parseReceipts(result []byte, hash [32]byte, h *chainHeader) (*heldReceipts, error) {
	if isNullResult(result) {
		return nil, fmt.Errorf("%w: null", errReceiptsAnswer)
	}
	var rs []receiptLite
	if err := common.SonicCfg.Unmarshal(result, &rs); err != nil {
		return nil, fmt.Errorf("%w: %v", errReceiptsAnswer, err)
	}
	if len(rs) == 0 && (h == nil || !h.emptyTxs) {
		return nil, fmt.Errorf("%w: no receipts for a block with transactions", errReceiptsAnswer)
	}
	key := hexHash(hash)
	r := newHeldReceipts(hash, h, result)
	var bloom []byte
	if judgedBloom(h) != nil {
		bloom = make([]byte, logfilter.BloomLen)
	}
	bloomBad := false
	for i := range rs {
		if !strings.EqualFold(rs[i].BlockHash, key) {
			return nil, fmt.Errorf("%w: receipt %d names block %q", errReceiptsAnswer, i, rs[i].BlockHash)
		}
		if bloom != nil {
			b, err := common.HexToBytes(rs[i].LogsBloom)
			if err != nil || len(b) != logfilter.BloomLen {
				bloomBad = true
			} else {
				for k := range bloom {
					bloom[k] |= b[k]
				}
			}
		}
		for _, l := range rs[i].Logs {
			if _, err := r.addLog(l); err != nil {
				return nil, err
			}
		}
	}
	if r.number < 0 && len(rs) > 0 {
		if n, err := common.HexToInt64(rs[0].BlockNumber); err == nil {
			r.number = n
		}
	}
	if bloom != nil {
		r.bloom = verdictOf(!bloomBad && bytes.Equal(bloom, h.bloom))
	}
	return r, nil
}

type synthReceipt struct {
	BlockHash        string            `json:"blockHash"`
	BlockNumber      string            `json:"blockNumber"`
	TransactionHash  string            `json:"transactionHash"`
	TransactionIndex string            `json:"transactionIndex"`
	Logs             []json.RawMessage `json:"logs"`
}

// parseLogs is the answer check of an eth_getLogs answer by blockHash, the
// receipts fallback: non-null, and every log names hash; with logIndexes,
// their logIndexes run 0..N-1 (logIndexContiguity). No logs reads as "no
// logs" only when the bloom check proves it (checking, with h's
// judgedBloom) or h's transactionsRoot is the empty-trie root. Its logs are
// grouped into receipts by transaction; the bloom verdict compares the
// bloom of the logs with h's judgedBloom.
func parseLogs(result []byte, hash [32]byte, h *chainHeader, checking, logIndexes bool) (*heldReceipts, error) {
	if isNullResult(result) {
		return nil, fmt.Errorf("%w: null", errReceiptsAnswer)
	}
	var raws []json.RawMessage
	if err := common.SonicCfg.Unmarshal(result, &raws); err != nil {
		return nil, fmt.Errorf("%w: %v", errReceiptsAnswer, err)
	}
	if len(raws) == 0 && !(checking && judgedBloom(h) != nil) && (h == nil || !h.emptyTxs) {
		return nil, fmt.Errorf("%w: no logs, and neither the bloom nor the transactions root proves none", errReceiptsAnswer)
	}
	key := hexHash(hash)
	r := newHeldReceipts(hash, h, nil)
	r.fromLogs = true
	var bloom []byte
	if judgedBloom(h) != nil {
		bloom = make([]byte, logfilter.BloomLen)
	}
	bloomBad := false
	synth := []synthReceipt{}
	indexes := make([]string, 0, len(raws))
	set := sha256.New()
	r.digest = logsDigest{count: len(raws), last: -1}
	for i, raw := range raws {
		l, err := r.addLog(raw)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(l.BlockHash, key) {
			return nil, fmt.Errorf("%w: log %d names block %q", errReceiptsAnswer, i, l.BlockHash)
		}
		if bloom != nil {
			for _, v := range append([]string{l.Address}, l.Topics...) {
				b, err := common.HexToBytes(v)
				if err != nil {
					bloomBad = true
					continue
				}
				logfilter.BloomAdd(bloom, b)
			}
		}
		if n := len(synth); n == 0 || synth[n-1].TransactionHash != l.TransactionHash {
			synth = append(synth, synthReceipt{BlockHash: key, BlockNumber: l.BlockNumber,
				TransactionHash: l.TransactionHash, TransactionIndex: l.TransactionIndex})
		}
		synth[len(synth)-1].Logs = append(synth[len(synth)-1].Logs, raw)
		indexes = append(indexes, l.LogIndex)
		idx, err := common.HexToInt64(l.LogIndex)
		if err != nil {
			idx = -1
		}
		r.digest.last = idx
		fmt.Fprintf(set, "%s|%d|%s|%s|", strings.ToLower(l.TransactionHash), idx, r.addr[i], strings.ToLower(l.Data))
		for _, tp := range r.topics[i] {
			set.Write([]byte(tp))
			set.Write([]byte{','})
		}
		set.Write([]byte{'\n'})
	}
	if logIndexes {
		if err := integrity.LogIndexesContiguous(len(indexes), func(i int) string { return indexes[i] }); err != nil {
			return nil, fmt.Errorf("%w: %v", errReceiptsAnswer, err)
		}
	}
	set.Sum(r.digest.set[:0])
	raw, err := common.SonicCfg.Marshal(synth)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errReceiptsAnswer, err)
	}
	r.raw = raw
	if r.number < 0 && len(synth) > 0 {
		if n, err := common.HexToInt64(synth[0].BlockNumber); err == nil {
			r.number = n
		}
	}
	if bloom != nil {
		r.bloom = verdictOf(!bloomBad && bytes.Equal(bloom, h.bloom))
	}
	return r, nil
}

func verdictOf(match bool) bloomVerdict {
	if match {
		return bloomMatch
	}
	return bloomMismatch
}

// indexReceipts parses receipts already accepted, such as a Read's batch,
// without the answer check.
func indexReceipts(hash [32]byte, raw json.RawMessage) *heldReceipts {
	var rs []receiptLite
	if common.SonicCfg.Unmarshal(raw, &rs) != nil {
		return nil
	}
	r := newHeldReceipts(hash, nil, raw)
	for i := range rs {
		for _, l := range rs[i].Logs {
			if _, err := r.addLog(l); err != nil {
				return nil
			}
		}
	}
	return r
}

// integrityView is the receipts as integrity checks read them, parsed once.
func (r *heldReceipts) integrityView() []integrity.Receipt {
	r.view.Do(func() {
		if err := common.SonicCfg.Unmarshal(r.raw, &r.receipts); err != nil {
			r.receipts = nil
		}
	})
	return r.receipts
}

// match is the logs f matches, verbatim and in order. Their positions are
// computed once per (block, filter): candidates from the address index,
// else the topic0 index, else every log, each checked with f.Matches.
func (r *heldReceipts) match(f *logfilter.LogsFilter) []json.RawMessage {
	var pos []int32
	if v, ok := r.matches.Load(f.Key()); ok {
		pos = v.([]int32)
	} else {
		pos = r.positions(f)
		r.matches.Store(f.Key(), pos)
	}
	out := make([]json.RawMessage, len(pos))
	for i, p := range pos {
		out[i] = r.logs[p]
	}
	return out
}

func (r *heldReceipts) positions(f *logfilter.LogsFilter) []int32 {
	var cand []int32
	switch addrs, t0 := f.Addresses(), f.Topic0(); {
	case len(addrs) > 0:
		for _, a := range addrs {
			cand = append(cand, r.byAddr[a]...)
		}
	case len(t0) > 0:
		for _, tp := range t0 {
			cand = append(cand, r.byTopic0[tp]...)
		}
	default:
		cand = make([]int32, len(r.logs))
		for i := range cand {
			cand[i] = int32(i)
		}
	}
	slices.Sort(cand)
	cand = slices.Compact(cand)
	out := cand[:0]
	for _, p := range cand {
		if f.Matches(r.addr[p], r.topics[p]) {
			out = append(out, p)
		}
	}
	return slices.Clip(out)
}

// MatchedLogs are the logs of b that f matches, verbatim and in order, from
// the receipts the feed holds for b, else from b.Receipts.
func (t *ChainTracker) MatchedLogs(b indexer.Block, f *logfilter.LogsFilter) []json.RawMessage {
	r := t.heldReceiptsOf(b.Hash)
	if r == nil {
		if len(b.Receipts) == 0 {
			return nil
		}
		if r = indexReceipts(b.Hash, b.Receipts); r == nil {
			return nil
		}
	}
	return r.match(f)
}

func (t *ChainTracker) heldReceiptsOf(hash [32]byte) *heldReceipts {
	v, ok := t.receipts.Load(hexHash(hash))
	if !ok {
		return nil
	}
	return v.(*heldReceipts)
}

// receiptsSpec fetches block h's receipts by hash, or with logs its logs by
// blockHash. The decode is the answer check, then the bloom check against
// the held header: a mismatch fails the fetch while the network's check is
// on. h is nil when the caller holds no header; the decode then reads the
// held one.
func (t *ChainTracker) receiptsSpec(class fetchClass, hash [32]byte, h *chainHeader, logs bool) fetchSpec {
	key := hexHash(hash)
	spec := fetchSpec{
		class:    class,
		kind:     "receipts",
		selector: t.selector,
		method:   "eth_getBlockReceipts",
		params:   `["` + key + `"]`,
		byHash:   true,
	}
	gate := t.fetch.bloom
	parse := parseReceipts
	if logs {
		spec.kind, spec.method, spec.params = "logs", "eth_getLogs", `[{"blockHash":"`+key+`"}]`
		parse = func(result []byte, hash [32]byte, h *chainHeader) (*heldReceipts, error) {
			return parseLogs(result, hash, h, gate.checking(), gate.prof().logIndexes)
		}
	}
	spec.decode = func(result []byte) (any, error) {
		hdr := h
		if hdr == nil {
			hdr = t.heldHeader(hash)
		}
		r, err := parse(result, hash, hdr)
		if err != nil {
			return nil, err
		}
		if r.bloom == bloomMismatch && gate.checking() {
			return nil, errBloomMismatch
		}
		return r, nil
	}
	return spec
}

// holdReceipts holds r, answered by upstreamId ("" from the cache), while
// its block lies within reorgWindow of the head, orphans included; older
// receipts leave at once, and at most receiptsCap whose number is unknown
// stay. Its bloom verdict counts once for trust, and for the network's
// bloom check against upstreamId's vendor. A receipts answer from an
// upstream, by a job, the outage probe or a fetch's mailbox result, ends
// the network-wide switch to eth_getLogs (receiptsDown).
func (t *ChainTracker) holdReceipts(r *heldReceipts, upstreamId string) {
	if !r.fromLogs && upstreamId != "" {
		t.receiptsDown = time.Time{}
	}
	key := hexHash(r.hash)
	if _, seen := t.receipts.Load(key); !seen {
		t.receiptsOrder = append(t.receiptsOrder, receiptsEntry{key: key, number: r.number})
	}
	t.receipts.Store(key, r)
	t.evictReceipts()
	for len(t.receiptsOrder) > t.receiptsCap() {
		t.receipts.Delete(t.receiptsOrder[0].key)
		t.receiptsOrder = t.receiptsOrder[1:]
	}
	if r.bloom != bloomUnknown {
		t.fetch.bloom.noted(r.h, r.bloom)
		t.recordBloom(upstreamId, r.h, r.bloom == bloomMatch)
	}
	t.receiptsDirty = true
}

// evictReceipts drops the receipts of blocks more than reorgWindow below
// the head.
func (t *ChainTracker) evictReceipts() {
	ref := t.windowRef()
	if ref == nil || ref.number < uint64(t.window) {
		return
	}
	lo := int64(ref.number - uint64(t.window))
	t.receiptsOrder = slices.DeleteFunc(t.receiptsOrder, func(e receiptsEntry) bool {
		if e.number >= 0 && e.number < lo {
			t.receipts.Delete(e.key)
			return true
		}
		return false
	})
}

// receiptsJob fetches one block's receipts until they are held: at pin time
// for the feed (keep), or for the Reads that wait on it. A bloom mismatch
// moves the next attempt to an upstream of another vendor; receipts that
// keep failing switch to eth_getLogs. With integrity on, logs from
// eth_getLogs are held only once vendors that meet the evidence quorum
// answered the same logsDigest.
type receiptsJob struct {
	h       *chainHeader
	keep    bool // the feed wants this pinned block's receipts held
	readers int  // Reads waiting on it
	done    chan struct{}
	res     *heldReceipts // set before done closes; nil when the job dropped
	cancel  context.CancelFunc
	attempt uint64
	logs    bool // switched to the eth_getLogs fallback
	fresh   bool // a cached answer failed the checks: never read the cache again
	// failingSince is when the job's receipts sweeps began failing, set by
	// the first failed one while the job asks for receipts; zero again once
	// it leaves the logs stand-in, so time on logs never counts.
	failingSince time.Time
	mismatched map[string]bool // vendors whose answer mismatched, this round
	tried      map[string]bool // upstreams asked by name, this round
	agree      []logsVote      // the logs digests vendors answered, this round
}

// logsVote is one logsDigest and the vendors that answered it.
type logsVote struct {
	digest  logsDigest
	vendors []string
}

func (t *ChainTracker) startJob(h *chainHeader, keep bool) *receiptsJob {
	j := &receiptsJob{h: h, keep: keep, done: make(chan struct{})}
	j.newRound()
	t.jobs[h.hash] = j
	t.runJob(j)
	return j
}

func (j *receiptsJob) newRound() {
	j.mismatched, j.tried, j.agree = make(map[string]bool), make(map[string]bool), nil
}

// answered reports whether vendor answered logs this round.
func (j *receiptsJob) answered(vendor string) bool {
	for _, v := range j.agree {
		if slices.Contains(v.vendors, vendor) {
			return true
		}
	}
	return false
}

// runJob makes the job's next attempt: a sweep, or after a mismatch an
// upstream of a vendor not yet asked this round. With every vendor asked,
// a new round sweeps once the key's backoff ends; a bloom check the
// network suspended meanwhile accepts it. Logs waiting for agreement ask a
// vendor that has not answered them; with every vendor asked, a new round
// starts a block time later.
func (t *ChainTracker) runJob(j *receiptsJob) {
	if j.cancel != nil {
		j.cancel()
	}
	// A job's first attempt asks for receipts whatever the network-wide
	// clock says, so receipts that work again end the stand-in.
	after := LogsFallbackAfter.Load()
	jobDown := !j.failingSince.IsZero() && time.Since(j.failingSince) >= after
	netDown := !t.receiptsDown.IsZero() && time.Since(t.receiptsDown) >= after
	switch receipts, logs := t.usableMethods(); {
	case !j.logs && logs && (!receipts || jobDown || j.attempt > 0 && netDown):
		j.logs = true
		j.newRound()
	case j.logs && receipts && (!logs || t.receiptsDown.IsZero()):
		// Receipts are served again and a receipts answer ended the
		// network-wide switch, or logs no longer may stand in: receipts
		// prove the logs, so the job leaves the stand-in.
		j.logs, j.failingSince = false, time.Time{}
		j.newRound()
	}
	spec := t.receiptsSpec(t.fetchClassAt(int64(j.h.number), classTipReceipts), j.h.hash, j.h, j.logs)
	spec.fresh = j.fresh
	if len(j.mismatched) > 0 {
		// A wrong answer may sit in the cache under this key.
		spec.fresh = true
		if spec.upstream = t.nextVendorUpstream(j, spec.method); spec.upstream == "" {
			j.newRound()
		}
	} else if len(j.agree) > 0 {
		spec.fresh = true
		if spec.upstream = t.nextAgreeUpstream(j); spec.upstream == "" {
			j.newRound()
			t.runJobLater(j)
			return
		}
	}
	ctx, cancel := context.WithCancel(t.ctx)
	j.cancel = cancel
	j.attempt++
	attempt := j.attempt
	w := t.fetch.submit(nil, spec)
	t.fetch.bg.Go(func() {
		res := w.wait(ctx)
		t.mb.put(trackerMsg{kind: msgCall, fn: func() { t.jobResult(j, attempt, spec, res) }})
	})
}

// nextVendorUpstream is the first upstream serving method of a vendor that
// has not mismatched on the job's block this round, and that was not asked
// yet.
func (t *ChainTracker) nextVendorUpstream(j *receiptsJob, method string) string {
	for _, u := range t.methodUpstreams(method) {
		if !j.mismatched[VendorKey(u)] && !j.tried[u.Id()] {
			return u.Id()
		}
	}
	return ""
}

// nextAgreeUpstream is the first upstream serving eth_getLogs of a vendor
// that has not answered the job's logs this round, not asked yet.
func (t *ChainTracker) nextAgreeUpstream(j *receiptsJob) string {
	for _, u := range t.methodUpstreams("eth_getLogs") {
		if !j.answered(VendorKey(u)) && !j.tried[u.Id()] {
			return u.Id()
		}
	}
	return ""
}

// runJobLater makes j's next attempt a block time from now, at most
// ReceiptsProbeCeiling, unless j moved on or ended first.
func (t *ChainTracker) runJobLater(j *receiptsJob) {
	attempt := j.attempt
	time.AfterFunc(min(t.BlockTime(), ReceiptsProbeCeiling.Load()), func() {
		if t.ctx.Err() != nil {
			return
		}
		t.mb.put(trackerMsg{kind: msgCall, fn: func() {
			if t.jobs[j.h.hash] == j && j.attempt == attempt {
				t.runJob(j)
			}
		}})
	})
}

// logsAgreed notes that upstreamId answered r, logs from eth_getLogs, and
// reports whether the vendors that answered its digest this round meet the
// evidence quorum. A cached answer names no vendor: it counts for none,
// and the cache is not read again.
func (t *ChainTracker) logsAgreed(j *receiptsJob, upstreamId string, r *heldReceipts) bool {
	if upstreamId == "" {
		j.fresh = true
		return false
	}
	j.tried[upstreamId] = true
	vendor := t.vendorOf(upstreamId)
	if vendor == "" {
		return false
	}
	i := slices.IndexFunc(j.agree, func(v logsVote) bool { return v.digest == r.digest })
	if i < 0 {
		j.agree = append(j.agree, logsVote{digest: r.digest})
		i = len(j.agree) - 1
	}
	if !slices.Contains(j.agree[i].vendors, vendor) {
		j.agree[i].vendors = append(j.agree[i].vendors, vendor)
	}
	return meetsEvidence(len(j.agree[i].vendors), t.networkVendors())
}

func (t *ChainTracker) jobResult(j *receiptsJob, attempt uint64, spec fetchSpec, res fetchResult) {
	if t.jobs[j.h.hash] != j || j.attempt != attempt {
		return
	}
	switch {
	case res.err == nil:
		r := res.val.(*heldReceipts)
		t.noteReceiptsFetch(true)
		if r.fromLogs && t.fetch.bloom.prof().agreeLogs && !t.logsAgreed(j, res.upstream, r) {
			t.runJob(j)
			return
		}
		t.holdReceipts(r, res.upstream)
		t.endJob(j, r)
	case errors.Is(res.err, errSchedulerClosed) || t.ctx.Err() != nil:
		t.endJob(j, nil)
	case res.upstream == "" && (errors.Is(res.err, errBloomMismatch) || errors.Is(res.err, errReceiptsAnswer)):
		// A cached answer failed the checks: no vendor answered it, so
		// nothing counts; the next attempt skips the cache.
		j.fresh = true
		t.runJob(j)
	default:
		t.noteReceiptsFetch(false)
		if spec.upstream == "" && !j.logs {
			if j.failingSince.IsZero() {
				j.failingSince = time.Now()
			}
			if t.receiptsDown.IsZero() {
				t.receiptsDown = time.Now()
			}
		}
		if errors.Is(res.err, errBloomMismatch) {
			t.countBloomMismatch(res.upstream, spec.method, j.h)
			if v := t.vendorOf(res.upstream); v != "" {
				j.mismatched[v] = true
			}
		}
		for _, u := range []string{spec.upstream, res.upstream} {
			if u != "" {
				j.tried[u] = true
			}
		}
		t.runJob(j)
	}
}

// noteReceiptsFetch records a receipts or logs fetch's outcome on the
// tracker's goroutine: a success ends an outage; the first failure after
// one arms the probe for when the outage begins.
func (t *ChainTracker) noteReceiptsFetch(ok bool) {
	if ok {
		t.receiptsFailing.Store(0)
		if t.probe != nil {
			t.probe.Stop()
			t.probe = nil
		}
		return
	}
	if t.receiptsFailing.Load() != 0 {
		return
	}
	t.receiptsFailing.Store(time.Now().UnixNano())
	t.armProbe(ReceiptsOutageAfter.Load())
}

func (t *ChainTracker) armProbe(after time.Duration) {
	if t.probe != nil {
		t.probe.Stop()
	}
	t.probe = time.AfterFunc(after, func() {
		if t.ctx.Err() == nil {
			t.mb.put(trackerMsg{kind: msgCall, fn: t.runProbe})
		}
	})
}

// runProbe is one fetch of the head block's receipts during an outage
// (spec §10), or its logs when no upstream serves receipts or once
// receipts have failed for LogsFallbackAfter, as a job switches; the next
// one follows a block time later, at most ReceiptsProbeCeiling, until one
// succeeds. With integrity on, the logs it fetched end the outage but are
// not held: one upstream's logs never meet the evidence quorum's agreement.
func (t *ChainTracker) runProbe() {
	since := t.receiptsFailing.Load()
	if since == 0 {
		return
	}
	next := min(t.BlockTime(), ReceiptsProbeCeiling.Load())
	head := t.head()
	receipts, logs := t.usableMethods()
	if head == nil || !t.canFetch() || !receipts && !logs {
		t.armProbe(next)
		return
	}
	useLogs := !receipts || logs && time.Since(time.Unix(0, since)) >= LogsFallbackAfter.Load()
	spec := t.receiptsSpec(classTipReceipts, head.hash, head, useLogs)
	w := t.fetch.submit(nil, spec)
	t.fetch.bg.Go(func() {
		res := w.wait(t.ctx)
		t.mb.put(trackerMsg{kind: msgCall, fn: func() {
			switch {
			case res.err == nil:
				t.noteReceiptsFetch(true)
				if r := res.val.(*heldReceipts); !r.fromLogs || !t.fetch.bloom.prof().agreeLogs {
					t.holdReceipts(r, res.upstream)
				}
			case t.ctx.Err() == nil:
				t.armProbe(next)
			}
		}})
	})
}

// usableMethods reports whether an upstream of the group serves
// eth_getBlockReceipts, and whether eth_getLogs by blockHash may stand in:
// with integrity off when an upstream serves it, with integrity on when
// the vendors serving it can meet the evidence quorum, so their answers
// can be compared. That stand-in proves less than receipts: logs carry no
// receiptsRoot to check, only the header's bloom, their logIndexes and,
// with integrity on, the evidence quorum's agreement.
func (t *ChainTracker) usableMethods() (receipts, logs bool) {
	receipts = len(t.methodUpstreams("eth_getBlockReceipts")) > 0
	getLogs := t.methodUpstreams("eth_getLogs")
	if !t.fetch.bloom.prof().agreeLogs {
		return receipts, len(getLogs) > 0
	}
	return receipts, meetsEvidence(vendorsOf(getLogs), t.networkVendors())
}

// ServesLogs reports whether logs subscribes may start (spec §10): an
// upstream serves a usable method, and the fetches of those methods have
// not all failed for ReceiptsOutageAfter.
func (t *ChainTracker) ServesLogs() bool {
	if receipts, logs := t.usableMethods(); !receipts && !logs {
		return false
	}
	since := t.receiptsFailing.Load()
	return since == 0 || time.Since(time.Unix(0, since)) < ReceiptsOutageAfter.Load()
}

// endJob ends j: r, when set, is the receipts its waiters read.
func (t *ChainTracker) endJob(j *receiptsJob, r *heldReceipts) {
	if j.cancel != nil {
		j.cancel()
	}
	delete(t.jobs, j.h.hash)
	j.res = r
	close(j.done)
	t.receiptsDirty = true
}

// countBloomMismatch counts a mismatching answer against its upstream, as
// integrity counts a bloomMatch violation, in its misbehavior score, and
// for the network's bloom check. A cached answer names no upstream, and
// counts against none.
func (t *ChainTracker) countBloomMismatch(upstreamId, method string, h *chainHeader) {
	if upstreamId == "" {
		return
	}
	u := t.upstreamOf(upstreamId)
	vendor := ""
	if u != nil {
		vendor = u.VendorName()
	}
	if s := suspensionsOf(t.network); s != nil {
		s.recordBloom(h.number, t.vendorOf(upstreamId), false)
	}
	telemetry.MetricIntegrityViolation.WithLabelValues(
		t.projectId(), vendor, t.networkLabel(), upstreamId, strings.ToLower(method), "bloomMatch", "reject", t.finalityLabel(int64(h.number)),
	).Inc()
	if u != nil {
		if ht := u.Tracker(); ht != nil {
			ht.RecordUpstreamMisbehavior(u, strings.ToLower(method), common.DataFinalityStateUnfinalized)
		}
	}
}

// recordBloom notes for the network's bloom check that upstreamId's
// answer for block h matched its header's bloom, or not; a cached answer
// ("") names no vendor and counts for none.
func (t *ChainTracker) recordBloom(upstreamId string, h *chainHeader, matched bool) {
	if upstreamId == "" {
		return
	}
	if s := suspensionsOf(t.network); s != nil {
		s.recordBloom(h.number, t.vendorOf(upstreamId), matched)
	}
}

// refetchJobs makes every job's next attempt now, from the network rather
// than the cache and past its key's backoff: the bloom check was
// suspended, so the blocks its mismatches held back are fetched again.
func (t *ChainTracker) refetchJobs() {
	for _, j := range t.jobs {
		for _, logs := range []bool{false, true} {
			t.fetch.forgive(t.receiptsSpec(classTipReceipts, j.h.hash, j.h, logs))
		}
		j.fresh = true
		j.newRound()
		t.runJob(j)
	}
}

// networkUpstreams are every upstream of the network, in its order.
func (t *ChainTracker) networkUpstreams() []common.Upstream {
	n, ok := t.network.(interface {
		EvmAllUpstreams(context.Context) []common.Upstream
	})
	if !ok {
		return nil
	}
	return n.EvmAllUpstreams(t.ctx)
}

// receiptsUpstreams are the upstreams of the tracker's group, in the
// network's order.
func (t *ChainTracker) receiptsUpstreams() []common.Upstream {
	ups := t.networkUpstreams()
	if t.selector == "" {
		return ups
	}
	out := make([]common.Upstream, 0, len(ups))
	for _, u := range ups {
		if m, _ := common.UpstreamMatchesSelector(t.selector, u); m {
			out = append(out, u)
		}
	}
	return out
}

// methodUpstreams are the group's upstreams that serve method.
func (t *ChainTracker) methodUpstreams(method string) []common.Upstream {
	ups := t.receiptsUpstreams()
	out := ups[:0:0]
	for _, u := range ups {
		if ok, _ := u.ShouldHandleMethod(method); ok {
			out = append(out, u)
		}
	}
	return out
}

func (t *ChainTracker) upstreamOf(id string) common.Upstream {
	for _, u := range t.receiptsUpstreams() {
		if u.Id() == id {
			return u
		}
	}
	return nil
}

// vendorOf is the VendorKey of upstreamId among every upstream of the
// network; "" once it left the network, so its answer counts for no
// vendor.
func (t *ChainTracker) vendorOf(upstreamId string) string {
	for _, u := range t.networkUpstreams() {
		if u.Id() == upstreamId {
			return VendorKey(u)
		}
	}
	return ""
}

// networkVendors is how many vendors the network's upstreams name, which
// the evidence quorum reads: a group's own vendors do not lower it (spec
// §6.10).
func (t *ChainTracker) networkVendors() int {
	return vendorsOf(t.networkUpstreams())
}

// wantsSnapshot are the registered MayMatch tests.
func (t *ChainTracker) wantsSnapshot() []func([]byte) bool {
	t.wantMu.Lock()
	defer t.wantMu.Unlock()
	out := make([]func([]byte) bool, 0, len(t.wants))
	for _, o := range t.wants {
		if o.MayMatch != nil {
			out = append(out, o.MayMatch)
		}
	}
	return out
}

// feedNeeds reports whether the feed fetches block h's receipts at pin
// time: a MayMatch is registered, the block has transactions, and either a
// registered MayMatch wants it or its bloom cannot skip it yet. Decided once
// per block while the registry and the skip stay.
func (t *ChainTracker) feedNeeds(h *chainHeader) bool {
	v, ok := t.needs[h.hash]
	if !ok {
		v = t.needsOf(h, t.needsWants)
		t.needs[h.hash] = v
	}
	return v
}

func (t *ChainTracker) needsOf(h *chainHeader, wants []func([]byte) bool) bool {
	if len(wants) == 0 || t.fetch.bloom.emptySkips(h) {
		return false
	}
	if !t.fetch.bloom.bloomSkips(h) {
		return true
	}
	for _, may := range wants {
		if may(h.bloom) {
			return true
		}
	}
	return false
}

// tapNeeds reports whether a tap reading with o needs block h's receipts.
func (t *ChainTracker) tapNeeds(o indexer.ReadOpts, h *chainHeader) bool {
	if o.MayMatch == nil || t.fetch.bloom.emptySkips(h) {
		return false
	}
	return !t.fetch.bloom.bloomSkips(h) || o.MayMatch(h.bloom)
}

// planReceipts starts a job for every pin within reorgWindow of the head
// whose receipts the feed needs and lacks, and lets go of jobs for blocks
// that left the pins or the window. It runs when the pins, the registry,
// the skip or a job changed.
func (t *ChainTracker) planReceipts() {
	version, trusted := t.wantVersion.Load(), t.fetch.bloom.trusted.Load()
	if version != t.needsVersion || trusted != t.needsTrusted {
		t.needsVersion, t.needsTrusted = version, trusted
		t.needsWants = t.wantsSnapshot()
		clear(t.needs)
		t.receiptsDirty = true
	}
	if !t.receiptsDirty {
		return
	}
	t.receiptsDirty = false
	wanted := make(map[[32]byte]bool)
	if head := t.head(); head != nil && t.canFetch() {
		for i := len(t.pins) - 1; i >= 0; i-- {
			h := t.pins[i].h
			if h.number+uint64(t.window) < head.number {
				break
			}
			if !t.feedNeeds(h) {
				continue
			}
			wanted[h.hash] = true
			if t.heldReceiptsOf(h.hash) != nil {
				continue
			}
			if j := t.jobs[h.hash]; j != nil {
				j.keep = true
			} else {
				t.startJob(h, true)
			}
		}
	}
	for hash, j := range t.jobs {
		if !wanted[hash] {
			j.keep = false
			if j.readers == 0 {
				t.endJob(j, nil)
			}
		}
	}
	for hash := range t.needs {
		if _, held := t.nodes[hash]; !held {
			delete(t.needs, hash)
		}
	}
	t.publishLogsHead()
}

// logsHeadOf is the logs head over published pins: the highest confirmed
// pin up to which every block within reorgWindow of the head has its
// receipts held or not needed.
func (t *ChainTracker) logsHeadOf(s *feedState) indexer.Cursor {
	send := s.confirmedHead()
	if send == nil {
		return indexer.Cursor{}
	}
	head := s.head()
	lo := s.base
	if head.number > uint64(t.window) {
		lo = max(lo, head.number-uint64(t.window))
	}
	if send.number < lo {
		return send.cursor()
	}
	logs := s.pinAt(lo)
	if lo > s.base {
		logs = s.pinAt(lo - 1)
	}
	for n := lo; n <= send.number; n++ {
		p := s.pinAt(n)
		if t.feedNeeds(p) && t.heldReceiptsOf(p.hash) == nil {
			if n == lo && lo == s.base {
				return indexer.Cursor{Number: p.number - min(p.number, 1), Hash: p.parentHash}
			}
			return logs.cursor()
		}
		logs = p
	}
	return logs.cursor()
}

// publishLogsHead republishes the pins when the logs head moved.
func (t *ChainTracker) publishLogsHead() {
	pub := t.state.Load()
	logs := t.logsHeadOf(pub)
	if logs == pub.logs {
		return
	}
	s := *pub
	s.logs, s.changed = logs, make(chan struct{})
	t.stamp(&s)
	close(t.state.Swap(&s).changed)
}

// joinJob makes a Read wait on block h's receipts: the job that fetches
// them, started for the Read when none runs. A job whose receipts are
// held already is returned done.
func (t *ChainTracker) joinJob(ctx context.Context, h *chainHeader) *receiptsJob {
	reply := make(chan *receiptsJob, 1)
	t.mb.put(trackerMsg{kind: msgCall, fn: func() { reply <- t.readerJoin(h) }})
	select {
	case j := <-reply:
		return j
	case <-ctx.Done():
		t.fetch.bg.Go(func() {
			select {
			case j := <-reply:
				t.leaveJob(j)
			case <-t.ctx.Done():
			}
		})
		return nil
	}
}

func (t *ChainTracker) readerJoin(h *chainHeader) *receiptsJob {
	if r := t.heldReceiptsOf(h.hash); r != nil || !t.canFetch() {
		j := &receiptsJob{h: h, res: r, done: make(chan struct{})}
		close(j.done)
		return j
	}
	j := t.jobs[h.hash]
	if j == nil {
		j = t.startJob(h, false)
	}
	j.readers++
	return j
}

// leaveJob ends a Read's wait on j; a job only Reads wanted drops with
// its last Read.
func (t *ChainTracker) leaveJob(j *receiptsJob) {
	if j == nil {
		return
	}
	t.mb.put(trackerMsg{kind: msgCall, fn: func() {
		if t.jobs[j.h.hash] != j {
			return
		}
		j.readers--
		if j.readers <= 0 && !j.keep {
			t.endJob(j, nil)
		}
	}})
}
