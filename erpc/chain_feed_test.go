package erpc

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsupstream"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/test/chainsim"
	"github.com/erpc/erpc/util"
	gethcommon "github.com/ethereum/go-ethereum/common"
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
	chaosSeedBudget    = uint64(150) // block times a seed may take
	chaosSettleRound   = chaosBlock  // a settle's asks and walks on local vendors
	chaosVendorBudget  = uint32(40)  // requests per second per upstream; client load under -race exceeds 4x this
	chaosMailboxBurst  = 1500        // above the tracker's mailbox header cap of 1024
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
	// fPendingSilent: one wss vendor's pending-transaction stream goes
	// silent with its socket open, while its heads keep flowing.
	fPendingSilent = "pending-transaction stream silent with its socket open"

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
	fFetchPanic     = "client asking the receipts the tracker fetches while the fetch's answer check panics"

	fForgedLog      = "one vendor serves receipts with a forged log"
	fForgedHash     = "one vendor pushes headers with a wrong hash, first at 2 heights in a row"
	fReorg3         = "a 3-block reorg"
	fClientGetLogs  = "client eth_getLogs for a block a logs tap fetched"
	fBadBaseFee     = "every vendor serves headers whose base fee breaks baseFeeDerivation"
	fBadBaseFeeLone = "a network with one vendor serves headers whose base fee breaks baseFeeDerivation"
	fForgedAhead    = "one vendor pushes headers with garbage hashes near and far ahead of the chain"
	fSlowWalk       = "a reorg whose walk is slow, while clients ask the new fork under a strict profile"

	fStreamResume     = "gRPC streams that disconnect at random and resume on the other instance"
	fStreamOutOfRange = "gRPC resume from a header older than the window, and from an orphan no vendor serves"
	fStreamAhead      = "gRPC resume above a fresh instance's head"
	fStreamSlow       = "gRPC client that stops reading"
	fStreamBadHeader  = "gRPC header that cannot be encoded"
	fStreamForkTree   = "gRPC resume from one vendor's branch deeper than reorgWindow, held in a tree without the pins"
)

var chaosFaults = []string{
	fReorgSmall, fMinorityFork, fAheadFork, fDeepReorg, fSilent, fLagging, fDown, fAllDown, fHalt,
	f429, fLate, fLoad, fSplit, fNonLinking, fRestart, fOrphanCached, fMailbox, fPanicOnce, fPanicThrice, fPendingSilent,
	fNullReceipts, fTruncReceipts, fLateReceipts, fNoReceipts, fReceiptsDown13, fBlockDown60, fReceiptsDown90, fWrongBloom, fChainBloom, fBloomFollower, fFetchPanic,
	fForgedLog, fForgedHash, fReorg3, fClientGetLogs, fBadBaseFee, fBadBaseFeeLone, fForgedAhead, fSlowWalk,
	fStreamResume, fStreamOutOfRange, fStreamAhead, fStreamSlow, fStreamBadHeader, fStreamForkTree,
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
// the 2-minute outage of every vendor, a reorg deeper than the window and
// a tracker stall of the receipts wait or the window may close it with
// 1013. In the gRPC seed a second eRPC instance runs, and
// gRPC StreamBlocks clients apply headers by the gRPC client rule, resuming
// on the other instance from their last header; each stream's chain must
// end equal to the canonical chain, every header linked to the one below.
func TestChainFeed_ConvergesUnderFaults(t *testing.T) {
	origWatch := wsupstream.HeadsWatchInterval.Load()
	wsupstream.HeadsWatchInterval.Store(chaosHeadsWatch)
	// A suspended check runs observe-only after 40 blocks here, so a seed
	// sees it suspended, then observe-only, within its budget.
	origSuspend := evm.CheckSuspendBlocks.Load()
	evm.CheckSuspendBlocks.Store(chaosSuspendBlocks)
	scale := []struct {
		d *util.Duration
		v time.Duration
	}{
		{evm.LogsFallbackAfter, chaosScaled(5 * time.Second)},
		{evm.ReceiptsOutageAfter, chaosScaled(60 * time.Second)},
		{evm.ReceiptsProbeCeiling, chaosScaled(12 * time.Second)},
		{logsReceiptsWait, chaosScaled(60 * time.Second)},
		{logsCloseSpread, chaosScaled(60 * time.Second)},
		{pendingSilentAfter, chaosScaled(60 * time.Second)},
		// Above the waits of the gRPC seed's other faults, so only the
		// faults that test them end a stream with UNAVAILABLE.
		{streamBlocksReadWait, 15 * chaosBlock},
		{streamBlocksSendWait, 5 * chaosBlock},
	}
	orig := make([]time.Duration, len(scale))
	for i, sc := range scale {
		orig[i] = sc.d.Load()
		sc.d.Store(sc.v)
	}
	t.Cleanup(func() {
		wsupstream.HeadsWatchInterval.Store(origWatch)
		evm.CheckSuspendBlocks.Store(origSuspend)
		for i, sc := range scale {
			sc.d.Store(orig[i])
		}
	})

	// Each plan's faults happen in its seed's order, on vendors its seed
	// picks; together the plans cover every fault.
	plans := []struct {
		seed        int64
		pollSources bool
		opts        chaosOpts
		faults      []string
	}{
		{11, true, chaosOpts{}, []string{fMinorityFork, fAheadFork, f429, fLate, fMailbox, fPanicOnce, fTruncReceipts, fWrongBloom}},
		{23, true, chaosOpts{}, []string{fDown, fLagging, fDeepReorg, fPanicThrice, fNonLinking, fReceiptsDown13}},
		{37, true, chaosOpts{}, []string{fAllDown, fHalt, fLoad, fOrphanCached, fRestart, fChainBloom, fNullReceipts}},
		{41, false, chaosOpts{}, []string{fSplit, fLate, fDeepReorg, fBlockDown60, fReceiptsDown90, fNoReceipts}},
		{53, true, chaosOpts{}, []string{fSilent, fLate, fLateReceipts, fBloomFollower, fPendingSilent, fFetchPanic}},
		{61, true, chaosOpts{integrity: true}, []string{fForgedLog, fForgedHash, fForgedAhead, fReorg3, fClientGetLogs}},
		{67, true, chaosOpts{integrity: true, badBaseFee: true}, []string{fBadBaseFee}},
		{71, true, chaosOpts{integrity: true, badBaseFee: true, oneVendor: true}, []string{fBadBaseFeeLone}},
		{73, true, chaosOpts{integrity: true, strict: true}, []string{fSlowWalk}},
		// A window of 10s holds the deep reorg's fork point, 20 blocks
		// down, even when load slows block production 2x.
		{79, true, chaosOpts{grpc: true, maxBackfill: 10 * time.Second}, []string{fStreamResume, fStreamOutOfRange, fStreamAhead, fStreamSlow, fStreamBadHeader, fStreamForkTree, fReorg3, fDeepReorg}},
	}
	before := erpcGoroutines()
	covered := make(map[string][]int64)
	for _, p := range plans {
		ok := t.Run(fmt.Sprintf("seed=%d", p.seed), func(t *testing.T) {
			s := newChaosSeed(t, p.seed, p.pollSources, p.opts)
			start := time.Now()
			s.run(p.faults)
			ticks := s.chain.Ticks()
			t.Logf("seed %d ran in %s, %d block times", p.seed, time.Since(start), ticks)
			require.LessOrEqual(t, ticks, chaosSeedBudget, "each seed runs within its budget of block times")
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
	// Every seed stopped its eRPC: no goroutine of eRPC or the harness
	// outlives it, so a load-dependent timing flake is not one.
	var leaked []string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		leaked = leaked[:0]
		for id, stack := range erpcGoroutines() {
			if _, ok := before[id]; !ok {
				leaked = append(leaked, stack)
			}
		}
		if len(leaked) == 0 || time.Now().After(deadline) {
			break
		}
	}
	require.Empty(t, leaked, "goroutines outlive every seed's eRPC")
}

// erpcGoroutines are the live goroutines, by their "goroutine N" line,
// that run eRPC or harness code, test runners excepted.
func erpcGoroutines() map[string]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	out := make(map[string]string)
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, "github.com/erpc/erpc/") || strings.Contains(g, "testing.tRunner") {
			continue
		}
		id, _, _ := strings.Cut(g, " [")
		out[id] = g
	}
	return out
}

//
// --- clients ---
//

type clientNote struct {
	at     time.Time
	number uint64
	hash   chainsim.Hash
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
	chain      map[uint64]chainsim.Hash
	first      uint64
	last       uint64
	floor      uint64 // a permitted restart: blocks below are not judged
	problems   []string
	conn       *websocket.Conn
}

// tapEnds counts the ends of one kind of ws taps so far: out of range by
// reason, and panics.
type tapEnds map[string]float64

const tapPanic = "panic"

var tapEndReasons = []string{indexer.OutOfRangeLag, indexer.OutOfRangeBelowWindow, indexer.OutOfRangeForkLeftWindow,
	indexer.OutOfRangeCursorUnknown, indexer.OutOfRangeReorgDepth, indexer.OutOfRangeReceiptsTimeout}

func tapEndsOf(kind string) tapEnds {
	out := make(tapEnds, len(tapEndReasons)+1)
	for _, r := range tapEndReasons {
		out[r] = testutil.ToFloat64(telemetry.MetricTapOutOfRangeTotal.WithLabelValues("test_ws", "evm:123", "ws", kind, r))
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
			if l.GetName() == "extra" && strings.Contains(l.GetValue(), "kind:"+kind) {
				out[tapPanic] += pb.GetCounter().GetValue()
			}
		}
	}
	return out
}

// tapEndsAt is the tap ends counted as of at.
type tapEndsAt struct {
	at   time.Time
	ends tapEnds
}

// recordHeadsEnds appends the newHeads tap ends to s.headsEnds whenever
// the count changed, until ctx ends. A change is stamped up to one poll
// late, so headsEndsAt may miss an end just before t: it then counts it
// as later, never earlier.
func (s *chaosSeed) recordHeadsEnds(ctx context.Context) {
	s.endsMu.Lock()
	last := s.headsEnds[len(s.headsEnds)-1].ends
	s.endsMu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
		if now := tapEndsOf(indexer.SubTypeNewHeads); !maps.Equal(now, last) {
			s.endsMu.Lock()
			s.headsEnds = append(s.headsEnds, tapEndsAt{at: time.Now(), ends: now})
			s.endsMu.Unlock()
			last = now
		}
	}
}

// headsEndsAt is the newHeads tap ends counted as of t.
func (s *chaosSeed) headsEndsAt(t time.Time) tapEnds {
	s.endsMu.Lock()
	defer s.endsMu.Unlock()
	// The first entry stamped after t; the one before it holds as of t.
	i, found := slices.BinarySearchFunc(s.headsEnds, t, func(e tapEndsAt, t time.Time) int { return e.at.Compare(t) })
	if found {
		i++
	}
	return s.headsEnds[max(i, 1)-1].ends
}

// restartContext names what may explain a jump: the newHeads tap ends
// since the client's previous head could first be sent, the tap's cursor
// (that head), the tracker's head and sendable head now, and the faults
// that ran since shortly before the client applied that head.
func (s *chaosSeed) restartContext(prev, now tapEnds, cursor uint64, since time.Time) string {
	return fmt.Sprintf("tap ends %s; tap cursor %d; %s", endsSince(prev, now), cursor, s.trackerContext(since))
}

// endsSince names the tap ends counted from prev to now.
func endsSince(prev, now tapEnds) string {
	var ends []string
	for _, r := range append(slices.Clone(tapEndReasons), tapPanic) {
		if d := now[r] - prev[r]; d > 0 {
			ends = append(ends, fmt.Sprintf("%s+%g", r, d))
		}
	}
	if len(ends) == 0 {
		return "none counted"
	}
	return strings.Join(ends, " ")
}

// trackerContext is the tracker's head and sendable head now, and the
// faults that ran since shortly before since.
func (s *chaosSeed) trackerContext(since time.Time) string {
	head, send := "none", "none"
	if tr := s.tracker(); tr != nil {
		if c, ok := tr.Tip(); ok {
			head = fmt.Sprint(c.Number)
		}
		if c, ok := tr.Confirmed(); ok {
			send = fmt.Sprint(c.Number)
		}
	}
	return fmt.Sprintf("tracker head %s; sendable head %s; faults %s", head, send, s.faultsSince(since.Add(-2*time.Second)))
}

func (s *chaosSeed) dialClient(name string, slow func() bool) *chaosClient {
	cl := &chaosClient{name: name, slow: slow, chain: make(map[uint64]chainsim.Hash)}
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
		cl.endTip = s.chain.Tip().Number
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
		cl.apply(s, uint64(n), chainsim.Hash(raw))
	}
}

func (cl *chaosClient) problem(format string, args ...interface{}) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.problems = append(cl.problems, fmt.Sprintf(format, args...))
}

func (cl *chaosClient) apply(s *chaosSeed, n uint64, h chainsim.Hash) {
	b := s.chain.Block(h)
	ends := tapEndsOf(indexer.SubTypeNewHeads)
	cl.mu.Lock()
	defer cl.mu.Unlock()
	now := time.Now()
	switch {
	case b == nil:
		cl.problems = append(cl.problems, fmt.Sprintf("head %d %x: no vendor ever served it", n, h[:4]))
	case b.Minority():
		cl.problems = append(cl.problems, fmt.Sprintf("head %d %x: a minority fork reached the client", n, h[:4]))
	case b.Number != n:
		cl.problems = append(cl.problems, fmt.Sprintf("head %x: number %d, block %d", h[:4], n, b.Number))
	}
	if cl.last != 0 && n > cl.last+1 {
		deepAt := s.lastDeep.Load()
		deep := deepAt != 0 && now.Sub(time.Unix(0, deepAt)) < 10*time.Second
		since := cl.subscribed
		if len(cl.notes) > 0 {
			since = cl.notes[len(cl.notes)-1].at
		}
		// The tap that sent cl.last may have ended before the client read
		// it: count the ends since that head could first be sent, its
		// mint, not since the client applied it.
		sent := s.headsEndsAt(cl.subscribed)
		if prev := s.chain.Block(cl.chain[cl.last]); prev != nil && prev.MintedAt.After(cl.subscribed) {
			sent = s.headsEndsAt(prev.MintedAt)
		}
		why := s.restartContext(sent, ends, cl.last, since)
		// A tap more than the window behind restarts after the head (spec
		// §9.2), counted as lag or, once its cursor left the window, as
		// below_window; the jump then spans more than the window.
		behind := ends[indexer.OutOfRangeLag]+ends[indexer.OutOfRangeBelowWindow] >
			sent[indexer.OutOfRangeLag]+sent[indexer.OutOfRangeBelowWindow] &&
			n > cl.last+1+uint64(chaosReorgWindow)
		if deep || behind {
			cl.floor = n
			s.t.Logf("client %s: jump %d → %d after a restart; %s", cl.name, cl.last, n, why)
		} else {
			cl.problems = append(cl.problems, fmt.Sprintf("jump %d → %d with no fault that calls for a restart; %s", cl.last, n, why))
		}
	}
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

// chaosLogsClient is one ws logs subscription on chainsim.Filters[filter]. It
// applies each log as a geth client does: removed:true drops a log it
// holds, any other log is added.
type chaosLogsClient struct {
	name   string
	filter int
	conn   *websocket.Conn

	mu        sync.Mutex
	held      map[chainsim.LogKey]uint64 // log -> its block number
	first     uint64                     // lowest block of an added log, 0 before one
	high      uint64                     // highest block of an added log
	added     int
	ended     time.Time
	endTip    uint64
	closeCode int     // the server's close code, 0 when it did not close
	why       string  // what may explain a close: tap ends, tracker, faults
	byTest    bool    // the test closed it
	taps      tapEnds // logs tap ends as it opened
	// subscribedTip is the chain's tip once the subscribe was answered: the
	// tap starts at or below it, so it sends from at most subscribedTip+1.
	subscribedTip uint64
	notes         int // notifications received
	noteBytes     int // their message lengths
	problems      []string
}

// subscribeLogs dials a ws connection and subscribes chainsim.Filters[filter]; on
// a refusal it returns the JSON-RPC error and closes the connection.
func (s *chaosSeed) subscribeLogs(filter int) (*websocket.Conn, map[string]interface{}) {
	conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/evm/123", s.addr), nil)
	require.NoError(s.t, err)
	req, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "eth_subscribe",
		"params": []interface{}{"logs", chainsim.Filters[filter].Params()}})
	require.NoError(s.t, conn.WriteMessage(websocket.TextMessage, req))
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
	for f := range chainsim.Filters {
		var conn *websocket.Conn
		require.Eventually(s.t, func() bool {
			conn, _ = s.subscribeLogs(f)
			return conn != nil
		}, 15*time.Second, chaosBlock, "logs subscribe on filter %d is accepted", f)
		cl := &chaosLogsClient{name: fmt.Sprintf("%s/%d", name, f), filter: f, conn: conn, held: make(map[chainsim.LogKey]uint64),
			taps: tapEndsOf(indexer.SubTypeLogs), subscribedTip: s.chain.Tip().Number}
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
			ended := time.Now()
			why := fmt.Sprintf("logs tap ends since it opened %s; %s", endsSince(cl.taps, tapEndsOf(indexer.SubTypeLogs)), s.trackerContext(ended.Add(-logsCloseBound())))
			cl.mu.Lock()
			cl.ended, cl.endTip, cl.why = ended, s.chain.Tip().Number, why
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
					Data        string `json:"data"`
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
		if r.Data != "0x" {
			cl.problem("log %s of block %s carries data %s no block of the chain holds", r.LogIndex, r.BlockNumber, r.Data)
		}
		cl.apply(s, chainsim.LogKey{BlockHash: chainsim.Hash(raw), LogIndex: int(idx)}, uint64(n), r.Removed)
	}
}

func (cl *chaosLogsClient) problem(format string, args ...interface{}) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.problems = append(cl.problems, fmt.Sprintf(format, args...))
}

func (cl *chaosLogsClient) apply(s *chaosSeed, k chainsim.LogKey, n uint64, removed bool) {
	b := s.chain.Block(k.BlockHash)
	cl.mu.Lock()
	defer cl.mu.Unlock()
	switch {
	case b == nil || b.Number != n:
		cl.problems = append(cl.problems, fmt.Sprintf("log of block %d %x: no such block", n, k.BlockHash[:4]))
		return
	case b.Minority():
		cl.problems = append(cl.problems, fmt.Sprintf("log of block %d %x: a minority fork reached the client", n, k.BlockHash[:4]))
	case !slices.ContainsFunc(b.Logs(), func(l chainsim.Log) bool { return l.LogIndex == k.LogIndex && chainsim.Filters[cl.filter].Matches(l) }):
		cl.problems = append(cl.problems, fmt.Sprintf("log %d of block %d: the filter does not match it", k.LogIndex, n))
	}
	_, held := cl.held[k]
	switch {
	case removed && !held:
		cl.problems = append(cl.problems, fmt.Sprintf("removed:true for log %d of block %d, never sent", k.LogIndex, n))
	case removed:
		delete(cl.held, k)
	case held:
		cl.problems = append(cl.problems, fmt.Sprintf("log %d of block %d sent twice", k.LogIndex, n))
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
	if b := s.chain.CanonAt(hi); b == nil {
		return fmt.Sprintf("no canonical block %d", hi)
	}
	want := s.chain.CanonicalLogs(chainsim.Filters[cl.filter], lo, hi)
	for k, n := range cl.held {
		if n < lo || n > hi {
			continue
		}
		if _, ok := want[k]; !ok {
			return fmt.Sprintf("holds log %d of block %d %x, not canonical", k.LogIndex, n, k.BlockHash[:4])
		}
		delete(want, k)
	}
	for k, l := range want {
		return fmt.Sprintf("misses log %d of block %d %x", k.LogIndex, l.BlockNumber, k.BlockHash[:4])
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
	chain       *chainsim.Chain
	opts        chaosOpts
	// baseline is each metric this seed judges, as the seed began.
	baseline chaosMetrics
	wsA, wsB *chainsim.Vendor
	httpA    *chainsim.Vendor
	httpB    *chainsim.Vendor
	vendors  []*chainsim.Vendor
	// lastDeep is when the last reorg deeper than the window ran, in Unix
	// nanoseconds; 0 before one.
	lastDeep atomic.Int64

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
	// liveAddr is addr for goroutines other than the run's.
	liveAddr string
	pending  *chaosPendingClient

	breakerMu     sync.Mutex
	breakerOpened string
	breakerAt     time.Time
	loadFrom      time.Time

	hook *chaosHook

	faultMu sync.Mutex
	spans   []faultSpan

	// headsEnds are the newHeads tap ends counted so far, each time the
	// count changed, oldest first.
	endsMu    sync.Mutex
	headsEnds []tapEndsAt

	// violators are the upstreams that serve forged data, the only ones
	// integrity may count violations against.
	violators map[string]bool

	// The gRPC seed's instances, 0 the primary and 1 a second eRPC, each
	// nil while down; its stream clients; and the eth_getBlockByNumber
	// requests clients had sent through the request path when streams
	// began.
	grpcMu        sync.Mutex
	grpcs         [2]*grpcInstance
	streams       []*chaosStream
	streamsWG     sync.WaitGroup
	stopStreams   context.CancelFunc
	byNumberAsked float64
	// internalOK are the times a stream may end with INTERNAL: a header
	// that cannot be encoded is pinned; to is zero while it lasts.
	internalOK []interval
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

// chaosOpts shape one seed: integrity on (checks configured as an operator
// would: level authoritative, baseFeeDerivation enabled for the simulated
// chain), strict (a reorg-sensitive mismatch on an unfinalized block is a
// hard reject), a chain whose base fee no parent derives, a network of one
// vendor, gRPC streams on 2 eRPC instances (with headers padded by
// chaosStreamPad), a maxBackfill other than chaosMaxBackfill, matching
// blocks a capped eth_getLogs answer can cut short (chainsim MatchLast).
type chaosOpts struct {
	integrity   bool
	strict      bool
	badBaseFee  bool
	oneVendor   bool
	grpc        bool
	maxBackfill time.Duration
	matchLast   bool
}

func newChaosSeed(t *testing.T, seed int64, pollSources bool, opts chaosOpts) *chaosSeed {
	s := &chaosSeed{t: t, seed: seed, rng: rand.New(rand.NewPCG(uint64(seed), 0)), pollSources: pollSources, opts: opts,
		occurred: make(map[string]string), paths: make(map[string]string)}
	pad := 0
	if opts.grpc {
		pad = chaosStreamPad
	}
	s.chain = chainsim.NewChain(chainsim.Config{ChainID: 123, BlockTime: chaosBlock, Seed: uint64(seed), HeaderPad: pad,
		History: chaosHistoryBlocks, BadBaseFee: opts.badBaseFee, PendingNoise: true, MatchLast: opts.matchLast})
	t.Cleanup(s.chain.Close)
	add := func(id, name string, ws bool) *chainsim.Vendor {
		v, err := s.chain.AddVendor(id, name, ws)
		require.NoError(t, err)
		return v
	}
	if opts.oneVendor {
		s.wsA = add("ws-a", "vendor-a", true)
		s.httpA = add("http-a", "vendor-a", false)
		s.vendors = []*chainsim.Vendor{s.wsA, s.httpA}
	} else {
		s.wsA = add("ws-a", "vendor-a", true)
		s.wsB = add("ws-b", "vendor-b", true)
		s.httpA = add("http-a", "vendor-a", false)
		s.httpB = add("http-b", "vendor-b", false)
		s.vendors = []*chainsim.Vendor{s.wsA, s.wsB, s.httpA, s.httpB}
	}
	s.baseline = s.metrics()
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
		if !v.WS {
			source = util.BoolPtr(s.pollSources)
		}
		u := sourceUpstream(v.ID, v.URL(), source)
		u.Evm.StatePollerInterval = common.Duration(chaosStatePoll)
		u.RateLimitBudget = "budget-" + v.ID
		u.VendorName = v.Name
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
			Id:    "budget-" + v.ID,
			Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: chaosVendorBudget, Period: common.RateLimitPeriodSecond}},
		})
	}
	cfg := sourcesConfig(func(nw *common.NetworkConfig) {
		nw.Evm.ChainTracker = &common.EvmChainTrackerConfig{ReorgWindow: chaosReorgWindow, MaxBackfill: common.Duration(s.maxBackfill())}
		if s.opts.integrity {
			nw.Integrity = &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{
				Level:  "authoritative",
				Checks: map[string]*common.IntegrityCheckConfig{"baseFeeDerivation": {Enabled: util.BoolPtr(true)}},
			}}
			if s.opts.strict {
				nw.Integrity.InvalidBehavior = &common.IntegrityInvalidBehaviorConfig{Unfinalized: "hardReject"}
			}
		}
	}, ups...)
	// No readiness grace: its 10s default sleep outlives the 10s the
	// leak check waits after the last seed.
	cfg.Server.WaitBeforeShutdown = common.Duration(0).Ptr()
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

func (s *chaosSeed) maxBackfill() time.Duration {
	return cmp.Or(s.opts.maxBackfill, chaosMaxBackfill)
}

// startERPC boots the primary eRPC instance and, in the gRPC seed, its
// gRPC server.
func (s *chaosSeed) startERPC() {
	srv, addr, cancel, nw := s.bootERPC()
	s.srv, s.addr, s.cancel = srv, addr, cancel
	s.mu.Lock()
	s.liveAddr = addr
	s.mu.Unlock()
	s.nw.Store(nw)
	if s.opts.grpc {
		s.startGrpc(0, srv, nil, nw)
	}
}

// bootERPC boots an eRPC instance and waits for its network.
func (s *chaosSeed) bootERPC() (*HttpServer, string, context.CancelFunc, *Network) {
	srv, addr, cancel := startTestERPCServer(s.t, s.config())
	var nw *Network
	require.Eventually(s.t, func() bool {
		n, err := srv.erpc.GetNetwork(context.Background(), "test_ws", "evm:123")
		if err != nil || n == nil || n.chainTracker == nil {
			return false
		}
		nw = n
		return true
	}, 15*time.Second, 20*time.Millisecond, "network initializes")
	return srv, addr, cancel, nw
}

// stopERPC stops the running instances, if any, and waits for their
// networks' goroutines; a failed seed stops them in cleanup, so nothing
// they run outlives the seed into later tests.
func (s *chaosSeed) stopERPC() {
	s.stopGrpc(1)
	s.stopGrpc(0)
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

// blocks waits n block times of the chain, so a fault lasts as many
// blocks however slowly the host runs.
func (s *chaosSeed) blocks(n int) { s.chain.WaitTicks(uint64(n)) }

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

func (s *chaosSeed) pick(vs ...*chainsim.Vendor) *chainsim.Vendor { return vs[s.rng.IntN(len(vs))] }

// setAll changes the faults of every vendor.
func (s *chaosSeed) setAll(fn func(f *chainsim.Faults)) {
	for _, v := range s.vendors {
		v.Update(fn)
	}
}

// reorg replaces the top depth blocks; one deeper than the window marks
// lastDeep before any vendor serves it.
func (s *chaosSeed) reorg(depth int) *chainsim.Block {
	if depth > chaosReorgWindow {
		s.lastDeep.Store(time.Now().UnixNano())
	}
	_, tip := s.chain.Reorg(depth)
	return tip
}

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
	s.headsEnds = []tapEndsAt{{at: time.Now(), ends: tapEndsOf(indexer.SubTypeNewHeads)}}
	go s.recordHeadsEnds(ctx)
	s.chain.Start()

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
	s.pending = &chaosPendingClient{done: make(chan struct{})}
	go s.pending.run(ctx, s)
	if s.opts.grpc {
		streamsCtx, stopStreams := context.WithCancel(context.Background())
		defer stopStreams()
		s.startStreams(streamsCtx, stopStreams)
	}
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
	s.chain.Stop()
	<-s.pending.done
	tip := s.chain.Tip()
	require.Eventually(s.t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, cl := range s.clients {
			cl.mu.Lock()
			open, last, h := cl.ended.IsZero(), cl.last, cl.chain[cl.last]
			cl.mu.Unlock()
			if open && (last != tip.Number || h != tip.Hash) {
				return false
			}
		}
		return true
	}, 15*time.Second, 20*time.Millisecond, "every open client reaches the canonical head %d", tip.Number)
	diff := "not checked"
	for deadline := time.Now().Add(15 * time.Second); diff != "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		diff = ""
		for _, cl := range s.openLogs() {
			cl.mu.Lock()
			if cl.first != 0 {
				if d := cl.logsDiffLocked(s, cl.first, tip.Number); d != "" {
					diff = cl.name + ": " + d
				}
			}
			cl.mu.Unlock()
		}
	}
	require.Empty(s.t, diff, "every open logs client holds the canonical logs up to %d", tip.Number)
	if s.opts.grpc {
		s.awaitStreams(tip)
	}
	end := time.Now()
	s.stopERPC()
	s.judge(end)
}

// checkSkips mints 9 blocks with matching logs, then a quiet block no
// filter may match and an empty block: once 8 matching blocks with
// transactions passed the bloom check in a row, neither is fetched.
func (s *chaosSeed) checkSkips() {
	plan := make([]chainsim.Kind, 0, 11)
	for range 9 {
		plan = append(plan, chainsim.KindMatching)
	}
	from := s.chain.Plan(append(plan, chainsim.KindQuiet, chainsim.KindEmpty)...).Number
	s.blocks(14)
	quiet, empty := s.chain.CanonAt(from+10), s.chain.CanonAt(from+11)
	require.True(s.t, quiet != nil && quiet.Kind() == chainsim.KindQuiet && empty != nil && empty.Kind() == chainsim.KindEmpty,
		"the planned quiet and empty blocks follow %d", from)
	require.Zero(s.t, s.receiptsCallsOf(quiet.Hash), "a block no filter may match is not fetched after 8 matching blocks")
	require.Zero(s.t, s.receiptsCallsOf(empty.Hash), "a block without transactions is never fetched")
	s.path(pathBloomSkip, fmt.Sprintf("quiet block %d not fetched", quiet.Number))
	s.path(pathEmptySkip, fmt.Sprintf("empty block %d not fetched", empty.Number))
}

func (s *chaosSeed) receiptsCallsOf(h chainsim.Hash) int {
	n := 0
	for _, v := range s.vendors {
		calls, _ := v.ReceiptsOf(h)
		n += calls
	}
	return n
}

// vendorOf are the upstreams of vendor name.
func (s *chaosSeed) vendorOf(name string) []*chainsim.Vendor {
	var out []*chainsim.Vendor
	for _, v := range s.vendors {
		if v.Name == name {
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
	case fFetchPanic:
		s.fetchPanic()
	case fPanicThrice:
		s.panicThrice()
	case fPendingSilent:
		s.pendingSilentVendor()
	case fForgedLog:
		s.forgedLog()
	case fForgedHash:
		s.forgedHash()
	case fReorg3:
		s.reorg3()
	case fStreamResume:
		s.streamResume()
	case fStreamOutOfRange:
		s.streamOutOfRange()
	case fStreamAhead:
		s.streamAhead()
	case fStreamSlow:
		s.streamSlow()
	case fStreamBadHeader:
		s.streamBadHeader()
	case fStreamForkTree:
		s.streamForkTree()
	case fForgedAhead:
		s.forgedAhead()
	case fSlowWalk:
		s.slowWalk()
	case fClientGetLogs:
		s.clientGetLogs()
	case fBadBaseFee, fBadBaseFeeLone:
		s.badBaseFee(f)
	default:
		s.t.Fatalf("unknown fault %q", f)
	}
}

//
// --- faults ---
//

func (s *chaosSeed) reorgSmall() {
	depth := 1 + s.rng.IntN(3)
	tip := s.reorg(depth)
	s.occur(fReorgSmall, fmt.Sprintf("depth %d, new head %d", depth, tip.Number))
}

func (s *chaosSeed) minorityFork() {
	v := s.pick(s.vendors...)
	from := s.chain.Tip().Number
	f := s.chain.StartFork(1, 1, v)
	s.blocks(20)
	head := v.Head()
	s.chain.EndFork(f)
	s.occur(fMinorityFork, fmt.Sprintf("%s on a minority fork from %d to %d (minority head: %t)", v.ID, from, head.Number, head.Minority()))
}

func (s *chaosSeed) aheadFork() {
	v := s.pick(s.vendors...)
	f := s.chain.StartFork(0, 20, v)
	s.blocks(10)
	lead := int64(v.Head().Number) - int64(s.chain.Tip().Number)
	s.chain.EndFork(f)
	s.occur(fAheadFork, fmt.Sprintf("%s led the canonical head by %d blocks on its fork", v.ID, lead))
}

// deepReorg: a logs tap whose sent blocks the reorg reverts more than
// reorgWindow deep ends with 1013 (spec §10.2); others may end so too when
// their fork point leaves the window (§13.1). New ones are dialed once the
// sure closes landed, so the next fault finds open clients. In the gRPC
// seed the window holds the fork point, so the streams get the new branch
// resent from the fork point + 1 (spec §10.1).
func (s *chaosSeed) deepReorg() {
	depth := chaosReorgWindow + 4
	oldTip := s.chain.Tip().Number
	var deep []*chaosLogsClient
	for _, cl := range s.openLogs() {
		if cl.subscribedTip+uint64(chaosReorgWindow) < oldTip {
			deep = append(deep, cl)
		}
	}
	from := time.Now()
	tip := s.reorg(depth)
	caught := s.caughtUp()
	s.allowCloses(from, caught, tip.Number-uint64(depth))
	resent := ""
	if s.opts.grpc {
		resent = "; " + s.awaitResend(tip.Number-uint64(depth), from)
	}
	s.awaitCloses(deep, "after-deep-reorg")
	s.occur(fDeepReorg, fmt.Sprintf("depth %d > reorgWindow %d, new head %d; %d logs clients sent more than the window closed with 1013, %d closed in all%s",
		depth, chaosReorgWindow, tip.Number, len(deep), s.closed1013(from), resent))
}

// caughtUp waits until the tracker may send the chain's head as of now: a
// fault's taps end once eRPC sees the chain again, however long that took.
func (s *chaosSeed) caughtUp() time.Time {
	want := s.chain.Tip()
	require.Eventually(s.t, func() bool {
		c, ok := s.tracker().Confirmed()
		return ok && (c.Number > want.Number || c.Hash == want.Hash)
	}, 15*time.Second, 10*time.Millisecond, "the tracker may send the head %d again", want.Number)
	return time.Now()
}

// liveWS is one of the wss vendors, not one that is down.
func (s *chaosSeed) liveWS() *chainsim.Vendor {
	v := s.pick(s.wsA, s.wsB)
	if v.Faults().Down {
		v = map[*chainsim.Vendor]*chainsim.Vendor{s.wsA: s.wsB, s.wsB: s.wsA}[v]
	}
	return v
}

func (s *chaosSeed) silentVendor() {
	v := s.liveWS()
	stalls := telemetry.MetricUpstreamWebsocketHeadsStallReconnectsTotal.WithLabelValues("test_ws", "evm:123", v.ID)
	before := testutil.ToFloat64(stalls)
	age := telemetry.MetricUpstreamReportAgeSeconds.WithLabelValues("test_ws", "evm:123", v.ID, "hash")
	// Every latest poll is a hash vote (spec §6.3), so the vendor refuses
	// latest too: only then does nothing it says keep its vote fresh.
	v.Update(func(f *chainsim.Faults) { f.SilentHeads, f.RefuseLatest = true, true })
	agedOut := false
	deadline := time.Now().Add(20 * chaosBlock)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(age) > s.tracker().W().Seconds() {
			agedOut = true
		}
		time.Sleep(25 * time.Millisecond)
	}
	v.Update(func(f *chainsim.Faults) { f.SilentHeads, f.RefuseLatest = false, false })
	reconnects := testutil.ToFloat64(stalls) - before
	require.True(s.t, agedOut, "%s's hash vote ages past W while its newHeads is silent", v.ID)
	require.Positive(s.t, reconnects, "%s reconnects its silent newHeads", v.ID)
	s.occur(fSilent, fmt.Sprintf("%s silent for 20 blocks: hash vote aged past W, %v stall reconnects", v.ID, reconnects))
}

func (s *chaosSeed) laggingVendor() {
	var candidates []*chainsim.Vendor
	for _, v := range s.vendors {
		if !v.Faults().Down {
			candidates = append(candidates, v)
		}
	}
	v := s.pick(candidates...)
	start := s.chain.Tip().Number
	v.Update(func(f *chainsim.Faults) { f.Lag = 40 })
	s.blocks(20)
	advanced := v.Head().Number
	v.Update(func(f *chainsim.Faults) { f.Lag = 0 })
	s.occur(fLagging, fmt.Sprintf("%s 40 behind, advanced to %d (head was %d at start)", v.ID, advanced, start))
}

func (s *chaosSeed) vendorDown(v *chainsim.Vendor) {
	v.Update(func(f *chainsim.Faults) { f.Down = true })
	s.blocks(50)
	v.Update(func(f *chainsim.Faults) { f.Down = false })
	s.occur(fDown, fmt.Sprintf("%s down for 50 blocks", v.ID))
}

// allDown also fails every receipts and logs fetch for longer than the 90s
// outage, so it may close logs clients with 1013 as that one does; new ones
// replace them.
func (s *chaosSeed) allDown() {
	from := time.Now()
	s.setAll(func(f *chainsim.Faults) { f.Down = true })
	s.blocks(chaosBlocksFor(2 * time.Minute))
	s.setAll(func(f *chainsim.Faults) { f.Down = false })
	up := time.Now()
	s.addQuiet(interval{from, up})
	caught := s.caughtUp()
	s.allowCloses(from, caught, 0)
	time.Sleep(time.Until(caught.Add(logsCloseBound())))
	closed := s.closed1013(from)
	if closed > 0 {
		s.dialLogs("after-all-down")
	}
	s.occur(fAllDown, fmt.Sprintf("all 4 vendors down for %d blocks; %d logs clients closed with 1013", chaosBlocksFor(2*time.Minute), closed))
}

func (s *chaosSeed) wsCounts() []int64 {
	var out []int64
	for _, v := range []*chainsim.Vendor{s.wsA, s.wsB} {
		st := v.Stats()
		out = append(out, st.Connects, st.HeadsSubscribes,
			int64(testutil.ToFloat64(telemetry.MetricUpstreamWebsocketHeadsStallReconnectsTotal.WithLabelValues("test_ws", "evm:123", v.ID))))
	}
	return out
}

func (s *chaosSeed) halt() {
	from := time.Now()
	before := s.wsCounts()
	s.chain.Halt(true)
	s.blocks(chaosBlocksFor(3 * time.Minute))
	after := s.wsCounts()
	s.chain.Halt(false)
	s.addQuiet(interval{from, time.Now()})
	require.Equal(s.t, before, after, "a quiet chain reconnects and resubscribes nothing (connects, subscribes, stall reconnects per ws vendor)")
	s.occur(fHalt, fmt.Sprintf("no blocks for %d block times; nothing reconnected", chaosBlocksFor(3*time.Minute)))
}

// throttle answers 429 on every vendor for 2 block times of wall clock, on
// a chain whose next 2 blocks carry matching logs, and logs of the first
// reach a client within 2 block times of the burst's end (§19 U5). A key
// fails at most twice in the burst, backing off 200 then 400 ms, so its
// next fetch comes at most 2 block times after the burst: burst plus
// backoff, 4 block times, stays under the 5 of logsReceiptsWait, as
// 48s + at most 5s stays under 60s in production. A burst in ticks would
// stretch under load past a third failure, whose 800 ms backoff the wait
// does not hold. That the capacity backoff ends at the next answered fetch
// is shown by TestFetchScheduler_CapacityRefusals (chain_fetch_test.go:66).
func (s *chaosSeed) throttle() {
	var before []int64
	for _, v := range s.vendors {
		before = append(before, v.Stats().Throttled)
	}
	first := s.chain.Plan(chainsim.KindMatching, chainsim.KindMatching).Number + 1
	s.setAll(func(f *chainsim.Faults) { f.Throttled = true })
	time.Sleep(2 * chaosBlock)
	s.setAll(func(f *chainsim.Faults) { f.Throttled = false })
	end := time.Now()
	var resumed time.Duration
	require.Eventually(s.t, func() bool {
		b := s.chain.CanonAt(first)
		if b == nil {
			return false
		}
		for _, cl := range s.openLogs() {
			cl.mu.Lock()
			for k, n := range cl.held {
				if n == first && k.BlockHash == b.Hash {
					resumed = time.Since(end)
				}
			}
			cl.mu.Unlock()
		}
		return resumed != 0
	}, 2*chaosBlock, 5*time.Millisecond, "logs of block %d, minted in the 429 burst, reach a client within 2 block times of its end", first)
	s.blocks(2)
	var got []string
	for i, v := range s.vendors {
		n := v.Stats().Throttled - before[i]
		require.Positive(s.t, n, "%s answers 429 during the burst", v.ID)
		got = append(got, fmt.Sprintf("%s:%d", v.ID, n))
	}
	s.occur(f429, fmt.Sprintf("429s %s for %s; logs of block %d delivered %s after", strings.Join(got, " "), 2*chaosBlock, first, resumed.Round(time.Millisecond)))
}

// slowAnswers sums the answers a Slow fault delayed on every vendor.
func (s *chaosSeed) slowAnswers() (n int64) {
	for _, v := range s.vendors {
		n += v.Stats().SlowAnswers
	}
	return n
}

func (s *chaosSeed) lateAnswers() {
	before := s.slowAnswers()
	s.setAll(func(f *chainsim.Faults) { f.Slow = 2 * chaosBlock })
	s.blocks(10)
	s.setAll(func(f *chainsim.Faults) { f.Slow = 0 })
	late := s.slowAnswers() - before
	require.Positive(s.t, late, "vendors answer late")
	s.occur(fLate, fmt.Sprintf("%d answers 2 block times late", late))
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
	// Only the 2 wss vendors' votes count here, and http-a is the one
	// upstream that answers a settle. Every latest poll votes (spec §6.3),
	// so http-a is held for W first: its vote stops counting, the fork ties
	// 1-vs-1, and the settle's ask waits on http-a until it is released.
	s.httpB.Update(func(f *chainsim.Faults) { f.Down = true })
	s.httpA.Update(func(f *chainsim.Faults) { f.Held = true })
	s.blocks(int(health.FreshnessWindow(chaosBlock)/chaosBlock) + 2)
	settled := telemetry.MetricChainTrackerSettleTotal.WithLabelValues("test_ws", "evm:123", "decided")
	before := testutil.ToFloat64(settled)
	// ws-b's sibling of the next canonical block, pushed as it lands.
	canon, f := s.chain.StartSplit(s.wsB)
	s.blocks(1)
	s.httpA.Update(func(f *chainsim.Faults) { f.Held = false })
	// ws-b stays live on its growing fork; the settle alone must release
	// the branch it decided to the clients.
	require.Eventually(s.t, func() bool { return testutil.ToFloat64(settled) > before }, 5*chaosBlock, 5*time.Millisecond, "a settle decides the 1-vs-1 split")
	require.Eventually(s.t, func() bool { return s.clientsHave(canon) }, chaosSettleRound, 5*time.Millisecond,
		"clients receive block %d of the settled branch within one settle round while ws-b votes its fork", canon.Number)
	s.blocks(5)
	s.chain.EndFork(f)
	s.httpB.Update(func(f *chainsim.Faults) { f.Down = false })
	s.blocks(2)
	decided := testutil.ToFloat64(settled) - before
	s.occur(fSplit, fmt.Sprintf("ws-a vs ws-b at %d, %v settles decided by http-a", canon.Number, decided))
}

// clientsHave reports whether every open client that is not reading slowly
// holds b.
func (s *chaosSeed) clientsHave(b *chainsim.Block) bool {
	s.mu.Lock()
	clients := slices.Clone(s.clients)
	s.mu.Unlock()
	for _, cl := range clients {
		if cl.slow != nil && cl.slow() {
			continue
		}
		cl.mu.Lock()
		open, h := cl.ended.IsZero(), cl.chain[b.Number]
		cl.mu.Unlock()
		if open && h != b.Hash {
			return false
		}
	}
	return true
}

func (s *chaosSeed) nonLinking() {
	for _, v := range s.vendors {
		require.LessOrEqual(s.t, testutil.ToFloat64(telemetry.MetricUpstreamReportAgeSeconds.WithLabelValues("test_ws", "evm:123", v.ID, "hash")), s.tracker().W().Seconds(), "%s is a hash voter before 2 of 4 votes leave", v.ID)
	}
	pair := slices.Clone(s.vendors)
	s.rng.Shuffle(len(pair), func(i, j int) { pair[i], pair[j] = pair[j], pair[i] })
	pair = pair[:2]
	depth := chaosReorgWindow + 6
	f := s.chain.StartFork(depth, depth+1, pair...)
	forkAt := pair[0].Head().Number - uint64(depth) - 1
	s.blocks(10)
	s.chain.EndFork(f)
	s.blocks(2)
	s.occur(fNonLinking, fmt.Sprintf("%s and %s on a branch from %d, %d below the window", pair[0].ID, pair[1].ID, forkAt, depth))
}

func (s *chaosSeed) restart() {
	from := time.Now()
	s.stopERPC()
	m := s.pick(s.wsA, s.wsB)
	for _, v := range s.vendors {
		if v != m {
			v.Update(func(f *chainsim.Faults) { f.Held = true })
		}
	}
	f := s.chain.StartFork(1, 1, m)
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
	for _, v := range s.vendors {
		v.Update(func(f *chainsim.Faults) { f.Held = false })
	}
	s.dialClient("restart-2", nil)
	s.dialLogs("restart")
	s.blocks(3)
	s.chain.EndFork(f)
	s.addQuiet(interval{from, time.Now()})
	b := s.chain.Block(first.Hash)
	require.NotNil(s.t, b)
	require.True(s.t, b.Minority() && from1 == m.ID, "the first header after the restart is %s's minority head (got %d from %s)", m.ID, first.Number, from1)
	s.occur(fRestart, fmt.Sprintf("restarted; first header %d from %s on its minority fork", first.Number, m.ID))
}

func (s *chaosSeed) orphanCached() {
	c := s.chain
	var tip *chainsim.Block
	var h1 string
	require.Eventually(s.t, func() bool {
		tip = c.Tip()
		out, _, err := s.rpc("eth_getBlockByNumber", fmt.Sprintf("0x%x", tip.Number), false)
		if err != nil {
			return false
		}
		res, _ := out["result"].(map[string]interface{})
		h1, _ = res["hash"].(string)
		return h1 == tip.Hash.Hex()
	}, 5*time.Second, 20*time.Millisecond, "a client reads the head by number")
	calls := func() (n int) {
		for _, v := range s.vendors {
			n += v.NumberCalls(tip.Number, time.Time{}, time.Now().Add(time.Hour))
		}
		return n
	}
	// The cache is written after the answer returns: repeat until a repeat
	// reaches no vendor.
	require.Eventually(s.t, func() bool {
		before := calls()
		out, _, err := s.rpc("eth_getBlockByNumber", fmt.Sprintf("0x%x", tip.Number), false)
		if err != nil {
			return false
		}
		res, _ := out["result"].(map[string]interface{})
		return res["hash"] == h1 && calls() == before
	}, 2*time.Second, 20*time.Millisecond, "a repeat answer for block %d comes from the cache", tip.Number)
	before := calls()
	depth := int(c.Tip().Number-tip.Number) + 1
	s.reorg(depth)
	s.blocks(3)
	require.NotEqual(s.t, tip.Hash, c.CanonAt(tip.Number).Hash)
	s.occur(fOrphanCached, fmt.Sprintf("block %d %s cached by number, then reorged out (depth %d); vendors served %d by-number fetches of it after", tip.Number, h1[:10], depth, calls()-before))
}

func (s *chaosSeed) mailboxBurst() {
	v := s.pick(s.wsA, s.wsB)
	heads := func() (n float64) {
		for _, o := range []string{"voted", "duplicate", "gated_voter", "gated_identity", "malformed"} {
			n += testutil.ToFloat64(telemetry.MetricChainTrackerSourceHeadsTotal.WithLabelValues("test_ws", "evm:123", v.ID, o))
		}
		return n
	}
	stalled, release := s.hook.stallOn(v.ID)
	select {
	case <-stalled:
	case <-time.After(5 * time.Second):
		s.t.Fatalf("the tracker never handled a header from %s", v.ID)
	}
	from, fromTip := time.Now(), s.chain.Tip().Number
	before := heads()
	// The vendor's tip and its parent in turn, so intake's duplicate check
	// passes each.
	head := v.Head()
	burst := make([]json.RawMessage, chaosMailboxBurst)
	for i := range burst {
		b := head
		if i%2 == 1 {
			b = head.Parent
		}
		burst[i] = b.JSON()
	}
	pushed := v.Push(burst...)
	require.Eventually(s.t, func() bool { return heads()-before >= float64(pushed) }, 5*time.Second, 10*time.Millisecond, "intake takes the burst")
	handled := s.hook.countFrom(v.ID)
	release()
	s.addQuiet(interval{from, time.Now()})
	stalledFor, stall := s.chain.Tip().Number-fromTip, time.Since(from)
	// A stall longer than the window leaves logs taps more than the window
	// behind the head (spec §9.2); fill does not join the gap below the
	// window for ws taps, so they may close. A stalled tracker holds no
	// receipts answer either, so a stall of the receipts wait, less the
	// block time a tap may already wait on a fresh block's receipts when it
	// begins, is 60s on one block's receipts: out of range (§10.2, §15.1).
	if stalledFor > uint64(chaosReorgWindow) || stall >= logsReceiptsWait.Load()-chaosBlock {
		s.allowCloses(from, time.Now(), 0)
	}
	s.blocks(3)
	got := s.hook.countFrom(v.ID) - handled
	require.Less(s.t, got, int64(pushed), "the mailbox sheds superseded headers once full")
	s.occur(fMailbox, fmt.Sprintf("%s pushed %d headers while the tracker stalled %d blocks (%s); it handled %d", v.ID, pushed, stalledFor, stall.Round(time.Millisecond), got))
}

func trackerResets() float64 {
	return testutil.ToFloat64(telemetry.MetricChainTrackerResetTotal.WithLabelValues("test_ws", "evm:123"))
}

func (s *chaosSeed) panicOnce() {
	resets := trackerResets()
	tip := s.chain.Tip().Number
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
	tip := s.chain.Tip().Number
	done := s.hook.panicAfter(tip, 3)
	var r panicReport
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		s.t.Fatalf("no header above %d reached the tracker", tip)
	}
	require.Eventually(s.t, func() bool { return trackerResets() == resets+1 }, 2*time.Second, 10*time.Millisecond, "3 panics in a row reset the tracker")
	s.blocks(6)
	s.occur(fPanicThrice, fmt.Sprintf("source header %d and the next 2 source headers panicked; tracker reset", r.number))
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

func (h *chaosHook) on(tr *evm.ChainTracker, upstreamId string, hdr *indexer.Cursor, sourceVote bool) {
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
	if r := h.panicked; r != nil && r.after == (indexer.Cursor{}) {
		// The message after the one that panicked first: the pins it left.
		r.after, _ = tr.Tip()
		if h.panicLeft == 0 {
			h.panicked = nil
			done := h.panicDone
			h.mu.Unlock()
			done <- *r
			return
		}
	}
	if r := h.panicked; r != nil && sourceVote && hdr != nil {
		// The next source headers panic too, while panics are left: one
		// run of source-vote panics, whatever other messages come between.
		h.panicLeft--
		last := h.panicLeft == 0
		if last {
			h.panicked = nil
		}
		done := h.panicDone
		h.mu.Unlock()
		if last {
			done <- *r
		}
		panic("chaos: tracker source header panics again")
	}
	if h.panicDone != nil && h.panicLeft > 0 && sourceVote && hdr != nil && hdr.Number > h.panicAbv && h.panicked == nil {
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
// next times-1 source headers, other messages between them handled as
// usual: one run of source-vote panics. The report carries the pins
// before the first panic and after it.
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
	// The chain's ticker shares the host and the chain's lock with every
	// vendor answer, so a span without a tick is a stall of the vendors, and
	// of the receipts answers they owe. A stall of the receipts wait, less
	// the block time a tap may already wait on a fresh block's receipts when
	// it begins, is 60s on one block's receipts: out of range (§10.2, §15.1).
	for _, st := range s.chain.Stalls(logsReceiptsWait.Load() - chaosBlock) {
		s.allowCloses(st.From, st.To, 0)
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
			require.True(t, allowed, "logs client %s closed with 1013 at %s with no fault that calls for it; %s", cl.name, cl.ended.Format(time.StampMilli), cl.why)
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
			want := s.chain.CanonAt(n)
			got, ok := cl.chain[n]
			require.True(t, ok, "client %s misses block %d (first %d, last %d)", cl.name, n, cl.first, cl.last)
			require.NotNil(t, want)
			require.Equal(t, want.Hash.Hex(), got.Hex(), "client %s block %d", cl.name, n)
		}
		cl.mu.Unlock()
	}
	p := s.pending
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Empty(t, p.problems, "pending-transaction client")
	require.NotZero(t, p.received, "the pending-transaction client got no transaction")
	t.Logf("pending-transaction client: %d transactions over %d subscriptions", p.received, p.sessions)
	s.judgeIntegrity()
	requireInvariantsHeld(t, "test_ws", "evm:123")
	if s.opts.grpc {
		s.judgeStreams()
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

// setVendor changes the faults of every upstream of vendor name.
func (s *chaosSeed) setVendor(name string, fn func(f *chainsim.Faults)) {
	for _, v := range s.vendorOf(name) {
		v.Update(fn)
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
	n := 0
	for k := from; k <= to; k++ {
		b := s.chain.CanonAt(k)
		if b == nil {
			continue
		}
		asked, served := false, false
		for _, v := range badUps {
			calls, _ := v.ReceiptsOf(b.Hash)
			asked = asked || calls > 0
		}
		for _, v := range goodUps {
			_, ok := v.ReceiptsOf(b.Hash)
			served = served || ok
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
		if b := s.chain.CanonAt(k); b != nil {
			for _, v := range s.vendorOf(name) {
				calls, _ := v.ReceiptsOf(b.Hash)
				n += calls
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
	byId := make(map[string]*chainsim.Vendor, len(s.vendors))
	for _, v := range s.vendors {
		byId[v.ID] = v
	}
	for _, u := range nw.trackerUpstreams(context.Background(), req, "eth_getBlockReceipts", "") {
		if v := byId[u.Id()]; v != nil && !v.Faults().Down {
			return v.Name
		}
	}
	s.t.Fatalf("no upstream serves a receipts sweep")
	return ""
}

// faultyVendor gives the receipts fault set to the vendor a sweep asks
// first, for at least 10 and at most 30 blocks: until that vendor was asked
// and, when rotates, the other vendor served a block it was asked for.
// When the sweeps ask the other vendor instead, by the selection order now
// or by the calls of the fault's first blocks, the fault moves to it and
// starts again, at most twice. The selection order moves it only while the
// vendor got no receipts call since the fault was set: the failures the
// fault causes lower the vendor's score, so the order moves away from a
// vendor the sweeps do ask. Receipt fetches that land late, under -race,
// are counted while the fault is still on. It returns the vendor, the
// blocks of the fault, the receipts calls the vendor got in them, and how
// many blocks rotated.
func (s *chaosSeed) faultyVendor(rotates bool, set func(f *chainsim.Faults, on bool)) (name string, from, to uint64, calls, rotated int) {
	require.NotEmpty(s.t, s.openLogs(), "logs clients are open, so receipts are wanted")
	asked := func(name string) (n int64) {
		for _, v := range s.vendorOf(name) {
			n += v.Stats().ReceiptsCalls
		}
		return n
	}
	name = s.receiptsVendor()
	from = s.chain.Tip().Number + 1
	asked0 := asked(name)
	s.setVendor(name, func(f *chainsim.Faults) { set(f, true) })
	exercised := func() bool {
		calls, rotated = s.callsTo(name, from, to), s.rotated(name, from, to)
		return calls > 0 && (!rotates || rotated > 0)
	}
	moves := 0
	for i := 0; i < 30; i += 2 {
		now := name
		if asked(name) == asked0 {
			now = s.receiptsVendor()
		}
		if i >= 4 && s.callsTo(name, from, to) == 0 && s.callsTo(otherVendor(name), from, to) > 0 {
			now = otherVendor(name)
		}
		if now != name && moves < 2 {
			moves++
			s.setVendor(name, func(f *chainsim.Faults) { set(f, false) })
			name, from, i = now, s.chain.Tip().Number+1, 0
			asked0 = asked(name)
			s.setVendor(name, func(f *chainsim.Faults) { set(f, true) })
		}
		s.blocks(2)
		to = s.chain.Tip().Number
		if i >= 10 && exercised() {
			break
		}
	}
	for deadline := time.Now().Add(5 * time.Second); !exercised() && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	s.setVendor(name, func(f *chainsim.Faults) { set(f, false) })
	require.Positive(s.t, calls, "%s, first in the receipts sweep, is asked for blocks %d..%d during its fault; %s got %d calls; the fault moved %d times",
		name, from, to, otherVendor(name), s.callsTo(otherVendor(name), from, to), moves)
	if rotates {
		require.Positive(s.t, rotated, "blocks %s answered wrong in %d..%d are refetched from %s", name, from, to, otherVendor(name))
	}
	return name, from, to, calls, rotated
}

//
// --- integrity faults ---
//

// chaosSuspendBlocks is evm.CheckSuspendBlocks on the chaos chain.
const chaosSuspendBlocks = 40

// chaosMetrics are the integrity counters a seed judges.
type chaosMetrics struct {
	violations map[string]float64 // per upstream
	suspended  map[string]float64 // per header check
	gated      float64            // source headers integrity held back
}

// metricSum sums the counters of c whose labels hold every value of match.
func metricSum(c prometheus.Collector, match map[string]string) float64 {
	ch := make(chan prometheus.Metric, 256)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	sum := 0.0
	for m := range ch {
		var pb dto.Metric
		if m.Write(&pb) != nil {
			continue
		}
		have := make(map[string]string, len(pb.GetLabel()))
		for _, l := range pb.GetLabel() {
			have[l.GetName()] = l.GetValue()
		}
		ok := true
		for k, v := range match {
			ok = ok && have[k] == v
		}
		if ok {
			sum += pb.GetCounter().GetValue()
		}
	}
	return sum
}

// requireInvariantsHeld: the network exports every series of
// erpc_chain_tracker_invariant_violations_total, and every one but
// final_hash_changed (a followed majority revert) is 0 (spec §17).
func requireInvariantsHeld(t *testing.T, project, network string) {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	go func() {
		telemetry.MetricChainTrackerInvariantViolationsTotal.Collect(ch)
		close(ch)
	}()
	got := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		if m.Write(&pb) != nil {
			continue
		}
		have := map[string]string{}
		for _, l := range pb.GetLabel() {
			have[l.GetName()] = l.GetValue()
		}
		if have["project"] == project && have["network"] == network {
			got[have["invariant"]] = pb.GetCounter().GetValue()
		}
	}
	for _, inv := range []common.Invariant{
		common.InvariantSentAboveLatest, common.InvariantSentUnconfirmed, common.InvariantTapGap, common.InvariantRemovedUnsent,
		common.InvariantPinsUnlinked, common.InvariantFinalHashChanged, common.InvariantLatestRegressed,
	} {
		v, ok := got[string(inv)]
		require.True(t, ok, "%s/%s exports no erpc_chain_tracker_invariant_violations_total{invariant=%s}", project, network, inv)
		if inv != common.InvariantFinalHashChanged {
			require.Zero(t, v, "%s/%s: erpc_chain_tracker_invariant_violations_total{invariant=%s}", project, network, inv)
		}
	}
}

func chaosLabels(kv ...string) map[string]string {
	m := map[string]string{"project": "test_ws", "network": "evm:123"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func (s *chaosSeed) metrics() chaosMetrics {
	m := chaosMetrics{violations: make(map[string]float64), suspended: make(map[string]float64)}
	for _, id := range []string{"ws-a", "ws-b", "http-a", "http-b"} {
		m.violations[id] = metricSum(telemetry.MetricIntegrityViolation, chaosLabels("upstream", id))
	}
	for _, id := range integrity.HeaderChecks {
		m.suspended[id] = metricSum(telemetry.MetricIntegrityCheckSuspendedTotal, chaosLabels("check", id))
	}
	m.gated = metricSum(telemetry.MetricChainTrackerSourceHeadsTotal, chaosLabels("outcome", "gated_integrity"))
	return m
}

func (s *chaosSeed) violator(upstreamId string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.violators == nil {
		s.violators = make(map[string]bool)
	}
	s.violators[upstreamId] = true
}

// forgedLog: the vendor a receipts sweep asks first serves receipts whose
// first log carries data no block holds; receiptsRootRecompute rejects
// them, and the other vendor serves the block.
func (s *chaosSeed) forgedLog() {
	name, from, to, calls, rotated := s.faultyVendor(true, func(f *chainsim.Faults, on bool) { f.ForgedLog = on })
	rose := 0.0
	for _, v := range s.vendorOf(name) {
		s.violator(v.ID)
		rose += metricSum(telemetry.MetricIntegrityViolation, chaosLabels("upstream", v.ID)) - s.baseline.violations[v.ID]
	}
	require.Positive(s.t, rose, "integrity counts the forged receipts against %s", name)
	s.occur(fForgedLog, fmt.Sprintf("%s forged a log in %d receipts answers, blocks %d..%d; %d refetched from the other vendor", name, calls, from, to, rotated))
}

// forgedHash: one wss vendor pushes its next 2 heads with a wrong hash,
// then, 4 blocks later, one more; intake drops each before the head
// counter, and no client sees one.
func (s *chaosSeed) forgedHash() {
	v := s.pick(s.wsA, s.wsB)
	s.violator(v.ID)
	gated := func() float64 {
		return metricSum(telemetry.MetricChainTrackerSourceHeadsTotal, chaosLabels("upstream", v.ID, "outcome", "gated_integrity"))
	}
	violations := func() float64 {
		return metricSum(telemetry.MetricIntegrityViolation, chaosLabels("upstream", v.ID, "check", "blockHashRecompute"))
	}
	gated0, violations0, forged0 := gated(), violations(), v.Stats().ForgedHeads
	forge := func(n int64) {
		before := v.Stats().ForgedHeads
		v.Update(func(f *chainsim.Faults) { f.WrongHashes = true })
		require.Eventually(s.t, func() bool { return v.Stats().ForgedHeads-before >= n }, 20*chaosBlock, 5*time.Millisecond, "%s pushes its forged heads", v.ID)
		v.Update(func(f *chainsim.Faults) { f.WrongHashes = false })
	}
	forge(2)
	s.blocks(4)
	forge(1)
	s.blocks(2)
	forged := v.Stats().ForgedHeads - forged0
	// Intake counts a push after the vendor wrote it: under load the last
	// one may still be in flight.
	for end := time.Now().Add(10 * chaosBlock); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if gated()-gated0 >= float64(forged) && violations()-violations0 >= float64(forged) {
			break
		}
	}
	require.Equal(s.t, float64(forged), gated()-gated0, "intake holds back every forged head of %s", v.ID)
	require.Equal(s.t, float64(forged), violations()-violations0, "each forged head counts against %s", v.ID)
	s.occur(fForgedHash, fmt.Sprintf("%s pushed %d heads with a wrong hash, the first 2 in a row", v.ID, forged))
}

func (s *chaosSeed) reorg3() {
	from := time.Now()
	if !s.opts.grpc {
		tip := s.reorg(3)
		s.blocks(3)
		s.occur(fReorg3, fmt.Sprintf("new head %d", tip.Number))
		return
	}
	// The second instance is down through the reorg, so it never holds
	// the old branch.
	s.stopGrpc(1)
	orphan := s.chain.Tip()
	tip := s.reorg(3)
	g := s.startSecond()
	s.blocks(3)
	// reorg(3) mints 4 blocks above the fork point.
	evidence := fmt.Sprintf("new head %d; %s; %s", tip.Number, s.awaitResend(tip.Number-3, from), s.resumeOrphan(g, orphan, tip.Number-4))
	s.occur(fReorg3, evidence)
}

// forgedAhead: one wss vendor pushes, for 6 blocks, headers with garbage
// hashes 1 and 2 blocks above its tip and 1000 and 1001 above it. They fail
// blockHashRecompute at 2 heights in a row for longer than a live bound,
// but one vendor alone, and far ahead of the head, never suspends the
// check; intake drops each, and no client sees one. The one 1000 above
// carries its timestamp as a JSON integer: check 0 reads it, so check 1
// still judges that header.
func (s *chaosSeed) forgedAhead() {
	v := s.pick(s.wsA, s.wsB)
	s.violator(v.ID)
	gated := func() float64 {
		return metricSum(telemetry.MetricChainTrackerSourceHeadsTotal, chaosLabels("upstream", v.ID, "outcome", "gated_integrity"))
	}
	gated0, pushed := gated(), 0
	for range 6 {
		head := v.Head()
		var forged []json.RawMessage
		for _, ahead := range []uint64{1, 2, 1000, 1001} {
			raw := chainsim.ForgeHash(chainsim.WithNumber(head.JSON(), head.Number+ahead))
			if ahead == 1000 {
				raw = withIntegerTimestamp(s.t, raw)
			}
			forged = append(forged, raw)
		}
		pushed += v.Push(forged...)
		s.blocks(1)
	}
	require.Positive(s.t, pushed, "%s has a newHeads subscription to push on", v.ID)
	require.Eventually(s.t, func() bool { return gated()-gated0 == float64(pushed) }, 10*chaosBlock, 10*time.Millisecond,
		"intake holds back every forged head of %s", v.ID)
	for _, id := range integrity.HeaderChecks {
		require.Equal(s.t, s.baseline.suspended[id], metricSum(telemetry.MetricIntegrityCheckSuspendedTotal, chaosLabels("check", id)),
			"one vendor's forged heads never suspend %s", id)
	}
	s.occur(fForgedAhead, fmt.Sprintf("%s pushed %d forged heads, near and far ahead", v.ID, pushed))
}

// withIntegerTimestamp is raw with its hex timestamp written as a JSON
// integer, as some nodes send it.
func withIntegerTimestamp(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	var hexTs string
	require.NoError(t, json.Unmarshal(fields["timestamp"], &hexTs))
	ts, err := strconv.ParseUint(strings.TrimPrefix(hexTs, "0x"), 16, 64)
	require.NoError(t, err)
	fields["timestamp"] = json.RawMessage(strconv.FormatUint(ts, 10))
	out, err := json.Marshal(fields)
	require.NoError(t, err)
	return out
}

// slowWalk: every answer by hash or below the tip takes 2 block times, so
// the walk of a 3-block reorg crawls while the sources' new votes wait on
// it. Clients meanwhile ask the new fork's blocks by number under a strict
// profile: the pins the walking votes dispute settle before any verdict,
// so every honest answer is served.
func (s *chaosSeed) slowWalk() {
	s.setAll(func(f *chainsim.Faults) { f.SlowHistory = 2 * chaosBlock })
	defer s.setAll(func(f *chainsim.Faults) { f.SlowHistory = 0 })
	rejects := func() float64 {
		return metricSum(telemetry.MetricIntegrityViolation, chaosLabels("verdict", "reject"))
	}
	rejects0 := rejects()
	tip := s.reorg(3)
	time.Sleep(chaosBlock / 2) // the sources' new votes reach the trackers
	var asked []*chainsim.Block
	for b := tip; b != nil && b.Number+4 > tip.Number; b = b.Parent {
		asked = append(asked, b)
	}
	type answer struct {
		b    *chainsim.Block
		resp map[string]interface{}
		code int
		err  error
	}
	answers := make(chan answer, len(asked))
	for _, b := range asked {
		go func() {
			resp, code, err := s.rpc("eth_getBlockByNumber", fmt.Sprintf("0x%x", b.Number), false)
			answers <- answer{b, resp, code, err}
		}()
	}
	for range asked {
		a := <-answers
		require.NoError(s.t, a.err)
		require.Equal(s.t, http.StatusOK, a.code, "block %d: %v", a.b.Number, a.resp)
		require.Nil(s.t, a.resp["error"], "block %d of the new fork is served, not rejected", a.b.Number)
		res, _ := a.resp["result"].(map[string]interface{})
		require.NotNil(s.t, res, "block %d: %v", a.b.Number, a.resp)
	}
	s.blocks(3)
	// Mismatches against the old pins are recorded, never a reject: the
	// walk's disputed pins are no verdict's evidence.
	require.Equal(s.t, rejects0, rejects(), "no honest answer is hard-rejected during the walk")
	s.occur(fSlowWalk, fmt.Sprintf("new head %d; %d blocks of the new fork served during the walk", tip.Number, len(asked)))
}

// clientGetLogs: a client asks eth_getLogs for a block whose receipts the
// feed holds for a logs tap; getLogsCompleteness compares them with no
// canonical_receipts fetch.
func (s *chaosSeed) clientGetLogs() {
	tr := s.tracker()
	var b *chainsim.Block
	require.Eventually(s.t, func() bool {
		tip := s.chain.Tip()
		for x := tip; x != nil && x.Number+8 > tip.Number; x = x.Parent {
			if x.Kind() == chainsim.KindMatching {
				if _, ok := tr.CachedReceipts(x.Hash.Hex()); ok {
					b = x
					return true
				}
			}
		}
		return false
	}, 20*chaosBlock, 10*time.Millisecond, "the feed holds a recent matching block's receipts")
	passed := func() float64 {
		return metricSum(telemetry.MetricIntegrityCheck, chaosLabels("check", "getLogsCompleteness", "outcome", "pass"))
	}
	fetched := func() float64 {
		return metricSum(telemetry.MetricIntegrityAuxRequest, chaosLabels("kind", "canonical_receipts"))
	}
	passed0, fetched0 := passed(), fetched()
	resp, code, err := s.rpc("eth_getLogs", map[string]interface{}{"blockHash": b.Hash.Hex()})
	require.NoError(s.t, err)
	require.Equal(s.t, http.StatusOK, code, "eth_getLogs answer %v", resp)
	logs, _ := resp["result"].([]interface{})
	require.Len(s.t, logs, len(b.Logs()), "the client gets block %d's logs", b.Number)
	require.Positive(s.t, passed()-passed0, "getLogsCompleteness verified the answer against the held receipts")
	require.Equal(s.t, fetched0, fetched(), "no canonical_receipts fetch")
	s.occur(fClientGetLogs, fmt.Sprintf("block %d: %d logs verified against the feed's receipts", b.Number, len(logs)))
}

// badBaseFee: every header of the chain carries a base fee its parent does
// not derive. No header passes baseFeeDerivation at 2 heights, so the
// check is suspended and heads keep flowing; chaosSuspendBlocks later it
// runs observe-only, still suspended, and holds back no header.
func (s *chaosSeed) badBaseFee(fault string) {
	suspended := func() float64 {
		return metricSum(telemetry.MetricIntegrityCheckSuspendedTotal, chaosLabels("check", "baseFeeDerivation"))
	}
	wouldReject := func() float64 {
		return metricSum(telemetry.MetricIntegrityCheck, chaosLabels("check", "baseFeeDerivation", "outcome", "would_reject"))
	}
	before := s.baseline.suspended["baseFeeDerivation"]
	require.Eventually(s.t, func() bool { return suspended() > before }, 60*chaosBlock, 10*time.Millisecond, "baseFeeDerivation is suspended")
	s.blocks(1)
	gated0, observed0 := s.metrics().gated, wouldReject()
	from := s.chain.Tip().Number
	s.blocks(chaosSuspendBlocks + 10)
	require.Equal(s.t, before+1, suspended(), "the check stays suspended")
	require.Positive(s.t, wouldReject()-observed0, "after %d blocks the check runs observe-only", chaosSuspendBlocks)
	require.Equal(s.t, gated0, s.metrics().gated, "no header held back while the check is suspended")
	s.occur(fault, fmt.Sprintf("suspended by block %d; observe-only by %d", from, s.chain.Tip().Number))
}

// judgeIntegrity: with integrity on, a check is suspended only where no
// upstream passes it, and violations count only against the upstreams
// that served forged data.
func (s *chaosSeed) judgeIntegrity() {
	if !s.opts.integrity {
		return
	}
	now := s.metrics()
	for _, id := range integrity.HeaderChecks {
		want := 0.0
		if s.opts.badBaseFee && id == "baseFeeDerivation" {
			want = 1
		}
		require.Equal(s.t, want, now.suspended[id]-s.baseline.suspended[id], "suspensions of %s", id)
	}
	if s.opts.badBaseFee || s.opts.strict {
		// badBaseFee: every upstream served the base fee the check
		// rejected. strict: a reorg's mismatches against the old pins are
		// recorded against honest upstreams; slowWalk judges the rejects.
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.vendors {
		if !s.violators[v.ID] {
			require.Zero(s.t, now.violations[v.ID]-s.baseline.violations[v.ID], "integrity violations of %s, which served no forged data", v.ID)
		}
	}
}

func (s *chaosSeed) nullReceipts() {
	name, from, to, calls, _ := s.faultyVendor(false, func(f *chainsim.Faults, on bool) { f.NullReceipts = on })
	s.occur(fNullReceipts, fmt.Sprintf("%s answered null to %d receipts calls, blocks %d..%d", name, calls, from, to))
}

func (s *chaosSeed) truncReceipts() {
	name, from, to, _, rotated := s.faultyVendor(true, func(f *chainsim.Faults, on bool) { f.TruncReceipts = on })
	s.path(pathRotation, fmt.Sprintf("%d truncated blocks refetched from %s", rotated, otherVendor(name)))
	s.occur(fTruncReceipts, fmt.Sprintf("%s's 2 upstreams dropped the last 10 receipts, blocks %d..%d; %d refetched from the other vendor", name, from, to, rotated))
}

func (s *chaosSeed) wrongBloom() {
	name, from, to, _, rotated := s.faultyVendor(true, func(f *chainsim.Faults, on bool) { f.WrongBloom = on })
	s.path(pathRotation, fmt.Sprintf("%d wrong-bloom blocks refetched from %s", rotated, otherVendor(name)))
	s.occur(fWrongBloom, fmt.Sprintf("%s answered a wrong bloom, blocks %d..%d; %d refetched from the other vendor", name, from, to, rotated))
}

func (s *chaosSeed) noReceipts() {
	name, from, to, calls, _ := s.faultyVendor(false, func(f *chainsim.Faults, on bool) { f.NoReceipts = on })
	s.occur(fNoReceipts, fmt.Sprintf("%s refused %d eth_getBlockReceipts calls, blocks %d..%d", name, calls, from, to))
}

func (s *chaosSeed) lateReceipts() {
	before := s.slowAnswers()
	s.setAll(func(f *chainsim.Faults) { f.SlowReceipts = 2 * chaosBlock })
	s.blocks(10)
	s.setAll(func(f *chainsim.Faults) { f.SlowReceipts = 0 })
	late := s.slowAnswers() - before
	require.Positive(s.t, late, "vendors answer receipts late")
	s.occur(fLateReceipts, fmt.Sprintf("%d receipts answers 2 block times late", late))
}

func (s *chaosSeed) getLogsCalls() int64 {
	var n int64
	for _, v := range s.vendors {
		n += v.Stats().LogsCalls
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
	s.chain.Plan(chainsim.KindMatching, chainsim.KindMatching, chainsim.KindMatching)
	until := from.Add(chaosScaled(13 * time.Second))
	s.setAll(func(f *chainsim.Faults) { f.ReceiptsDown = true })
	var calls int64
	for range 20 {
		s.blocks(1)
		if calls = s.getLogsCalls() - before; calls > 0 {
			break
		}
		until = later(until, time.Now().Add(chaosBlock))
	}
	time.Sleep(time.Until(until))
	s.setAll(func(f *chainsim.Faults) { f.ReceiptsDown = false })
	require.Positive(s.t, calls, "receipts failing past %s on every vendor switch blocks to eth_getLogs", evm.LogsFallbackAfter.Load())
	s.blocks(4)
	// A receipts answer ends the network-wide switch, and a job's clock
	// counts only its receipts failures: once receipts work again, no block
	// is fetched from eth_getLogs.
	settled := s.getLogsCalls()
	s.blocks(4)
	require.Equal(s.t, settled, s.getLogsCalls(), "blocks after receipts recovered are fetched by receipts, not eth_getLogs")
	s.path(pathFallback, fmt.Sprintf("%d eth_getLogs calls by blockHash", calls))
	s.occur(fReceiptsDown13, fmt.Sprintf("receipts failed on every vendor for %s; %d eth_getLogs calls", until.Sub(from).Round(time.Millisecond), calls))
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
	hold := chaosScaled(60*time.Second) + 2*chaosBlock
	s.chain.FailNext(chainsim.KindMatching, hold)
	var b *chainsim.Block
	require.Eventually(s.t, func() bool {
		b = s.chain.Failing()
		return b != nil
	}, 10*time.Second, 5*time.Millisecond, "a block with matching logs is minted")
	until := b.MintedAt.Add(hold)
	s.allowCloses(from, until, 0)
	time.Sleep(time.Until(until))
	s.awaitCloses(open, "after-block")
	s.occur(fBlockDown60, fmt.Sprintf("block %d failed on every vendor for %s; %d logs clients closed with 1013", b.Number, chaosScaled(60*time.Second), s.closed1013(from)))
}

func (s *chaosSeed) receiptsDown90() {
	from := time.Now()
	open := s.openLogs()
	until := from.Add(chaosScaled(90 * time.Second))
	s.setAll(func(f *chainsim.Faults) { f.ReceiptsDown, f.LogsDown = true, true })
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
	s.setAll(func(f *chainsim.Faults) { f.ReceiptsDown, f.LogsDown = false, false })
	s.awaitCloses(open, "after-outage")
	s.occur(fReceiptsDown90, fmt.Sprintf("every receipts and logs fetch failed for %s; subscribe refused after %s; %d logs clients closed with 1013",
		chaosScaled(90*time.Second), refusedAt.Sub(from).Round(time.Millisecond), s.closed1013(from)))
}

// chainBloom gives every vendor's receipts a bloom other than the header's:
// the network's check turns off and their logs reach clients; checkSkips
// later proves it turned on again.
func (s *chaosSeed) chainBloom() {
	s.setAll(func(f *chainsim.Faults) { f.WrongBloom = true })
	from := s.chain.Tip().Number + 1
	s.blocks(4)
	var got uint64
	require.Eventually(s.t, func() bool {
		to := s.chain.Tip().Number
		for _, cl := range s.openLogs() {
			cl.mu.Lock()
			for k, n := range cl.held {
				if n >= from && n <= to {
					if b := s.chain.CanonAt(n); b != nil && b.Hash == k.BlockHash {
						got = n
					}
				}
			}
			cl.mu.Unlock()
		}
		return got != 0
	}, 30*chaosBlock, 20*time.Millisecond, "logs of blocks whose receipts no vendor answers with the header's bloom reach clients")
	s.setAll(func(f *chainsim.Faults) { f.WrongBloom = false })
	s.path(pathBloomOff, fmt.Sprintf("logs of block %d delivered from mismatching receipts", got))
	s.occur(fChainBloom, fmt.Sprintf("every vendor's receipts mismatched from block %d; check off, logs of block %d delivered", from, got))
}

// bloomFollower holds each block's first receipts call and answers it with
// a wrong bloom. While the tracker's sweep for a block waits on it, a
// client asks the same receipts: it joins the sweep, and gets only an
// answer whose blooms are the block's.
func (s *chaosSeed) bloomFollower() {
	held, joined, failed := s.followTrapped()
	require.Positive(s.t, joined, "a client joins the tracker's receipts sweep while its first answer is held (%d blocks held)", held)
	s.occur(fBloomFollower, fmt.Sprintf("%d blocks' first receipts answers held with a wrong bloom; %d client requests joined the tracker's sweep and got the right blooms; %d got an error", held, joined, failed))
}

// fetchPanic is bloomFollower with the tracker's answer check panicking on
// each held answer (#1060): the panic fails the tracker's fetch, counted in
// erpc_unexpected_panic_total, and the client that joined its sweep gets
// an answer, never the unchecked one: only the block's own receipts.
func (s *chaosSeed) fetchPanic() {
	panics := func() float64 {
		return metricSum(telemetry.MetricUnexpectedPanicTotal, map[string]string{"scope": "chain-fetch"})
	}
	before := panics()
	var mu sync.Mutex
	panicked := make(map[chainsim.Hash]bool) // guarded by mu
	hook := func(method, params string) {
		var refs []string
		if method != "eth_getBlockReceipts" || json.Unmarshal([]byte(params), &refs) != nil || len(refs) != 1 {
			return
		}
		raw, err := hex.DecodeString(strings.TrimPrefix(refs[0], "0x"))
		if err != nil || len(raw) != 32 {
			return
		}
		h := chainsim.Hash(raw)
		tr, ok := s.chain.Traps()[h]
		mu.Lock()
		first := ok && !tr.AnsweredAt.IsZero() && !panicked[h]
		panicked[h] = panicked[h] || first
		mu.Unlock()
		if first {
			panic(fmt.Sprintf("chaos: the receipts check of block %x panics", h[:4]))
		}
	}
	evm.TrackerFetchHook.Store(&hook)
	defer evm.TrackerFetchHook.Store(nil)
	held, joined, failed := s.followTrapped()
	require.Positive(s.t, joined, "a client joins the tracker's receipts sweep whose check panics (%d blocks held)", held)
	// A hook marks its block before it panics, and the fetch counts the
	// panic once it unwinds: with the hook gone, the count catches up.
	evm.TrackerFetchHook.Store(nil)
	mu.Lock()
	n := len(panicked)
	mu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); panics()-before < float64(n) && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if got := panics() - before; got < float64(n) {
		var fetches []string
		for _, g := range erpcGoroutines() {
			if strings.Contains(g, "fetchScheduler") {
				fetches = append(fetches, g)
			}
		}
		s.t.Fatalf("%g of the %d panicking fetches are counted; tracker fetch goroutines:\n%s", got, n, strings.Join(fetches, "\n\n"))
	}
	s.occur(fFetchPanic, fmt.Sprintf("%d receipts checks panicked; %d client requests that joined those sweeps got the block's own receipts; %d got an error", n, joined, failed))
}

// followTrapped holds each block's first receipts call, answered with a
// wrong bloom, and asks the same receipts as a client while it is held,
// until 2 clients joined a held sweep or 40 blocks passed. Every answer a
// client gets carries the block's receipts in full with their blooms.
func (s *chaosSeed) followTrapped() (held, joined, failed int) {
	require.NotEmpty(s.t, s.openLogs(), "logs clients are open, so receipts are wanted")
	c := s.chain
	c.SetTrap(2 * chaosBlock)
	asked := make(map[chainsim.Hash]bool)
	for deadline := time.Now().Add(40 * chaosBlock); joined < 2 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		var b *chainsim.Block
		for h, tr := range c.Traps() {
			if !asked[h] && tr.AnsweredAt.IsZero() {
				b = c.Block(h)
				break
			}
		}
		if b == nil {
			continue
		}
		asked[b.Hash] = true
		sent := time.Now()
		out, _, err := s.rpc("eth_getBlockReceipts", b.Hash.Hex())
		require.NoError(s.t, err)
		tr := c.Traps()[b.Hash]
		if out["result"] == nil {
			failed++
			continue
		}
		var got []struct {
			LogsBloom string `json:"logsBloom"`
		}
		raw, _ := json.Marshal(out["result"])
		require.NoError(s.t, json.Unmarshal(raw, &got), "receipts answer %s", raw)
		want := b.Receipts()
		require.Len(s.t, got, len(want), "block %d's receipts reach the client in full", b.Number)
		for i := range want {
			require.Equal(s.t, want[i]["logsBloom"], got[i].LogsBloom, "block %d receipt %d reaches the client with the block's bloom", b.Number, i)
		}
		// Sent early in the hold, and no other call reached a vendor
		// while it lasted: the client waited on the tracker's sweep.
		if tr.During == 0 && tr.AnsweredAt.Sub(sent) > chaosBlock {
			joined++
		}
	}
	c.SetTrap(0)
	return len(c.Traps()), joined, failed
}

//
// --- pending transactions ---
//

// chaosPendingClient is the seed's ws newPendingTransactions client. It
// subscribes again whenever its connection ends (an eRPC restart); each
// subscription must get every transaction at most once, and only ones a
// vendor pushed.
type chaosPendingClient struct {
	done chan struct{}

	mu       sync.Mutex
	received int
	sessions int
	problems []string
}

func (p *chaosPendingClient) problem(format string, args ...interface{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.problems = append(p.problems, fmt.Sprintf(format, args...))
}

func (p *chaosPendingClient) run(ctx context.Context, s *chaosSeed) {
	defer close(p.done)
	for ctx.Err() == nil {
		s.mu.Lock()
		addr := s.liveAddr
		s.mu.Unlock()
		conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/evm/123", addr), nil)
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		unblock := context.AfterFunc(ctx, func() { _ = conn.Close() })
		if p.subscribe(conn) {
			p.read(s, conn)
		}
		unblock()
		_ = conn.Close()
	}
}

func (p *chaosPendingClient) subscribe(conn *websocket.Conn) bool {
	if conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newPendingTransactions"]}`)) != nil {
		return false
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return false
	}
	var resp struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(msg, &resp) != nil || resp.Result == "" {
		// A refusal while the instance starts; the next connection retries.
		time.Sleep(50 * time.Millisecond)
		return false
	}
	p.mu.Lock()
	p.sessions++
	p.mu.Unlock()
	return true
}

func (p *chaosPendingClient) read(s *chaosSeed, conn *websocket.Conn) {
	seen := make(map[string]bool)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var note struct {
			Method string `json:"method"`
			Params struct {
				Result string `json:"result"`
			} `json:"params"`
		}
		if json.Unmarshal(msg, &note) != nil || note.Method != "eth_subscription" {
			p.problem("unexpected message %s", msg)
			continue
		}
		h := note.Params.Result
		switch {
		case !s.chain.Pushed(gethcommon.HexToHash(h)):
			p.problem("transaction %s no vendor pushed", h)
		case seen[h]:
			p.problem("transaction %s reached the client twice", h)
		}
		seen[h] = true
		p.mu.Lock()
		p.received++
		p.mu.Unlock()
	}
}

// pendingSilentVendor silences one wss vendor's pending-transaction pushes
// while its heads, which carry transactions, keep flowing: after
// pendingSilentAfter the source subscribes the topic again.
func (s *chaosSeed) pendingSilentVendor() {
	v := s.liveWS()
	topic := string(indexer.PendingTxTopic("[]"))
	resubs := func() float64 {
		return testutil.ToFloat64(telemetry.MetricSourceResubscribeTotal.WithLabelValues("test_ws", "evm:123", v.ID, topic))
	}
	before, subs := resubs(), v.Stats().PendingSubscribes
	from := time.Now()
	v.Update(func(f *chainsim.Faults) { f.SilentPending = true })
	require.Eventually(s.t, func() bool { return resubs() > before }, 10*time.Second, 20*time.Millisecond,
		"%s's silent pending-transaction topic is subscribed again", v.ID)
	took := time.Since(from)
	v.Update(func(f *chainsim.Faults) { f.SilentPending = false })
	require.GreaterOrEqual(s.t, took, pendingSilentAfter.Load(), "only a topic silent for pendingSilentAfter is subscribed again")
	require.Eventually(s.t, func() bool { return v.Stats().PendingSubscribes > subs }, 5*time.Second, 20*time.Millisecond,
		"%s gets the new subscribe", v.ID)
	s.occur(fPendingSilent, fmt.Sprintf("%s silent; subscribed again after %s", v.ID, took.Round(time.Millisecond)))
}
