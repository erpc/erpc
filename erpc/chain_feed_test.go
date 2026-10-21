package erpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsupstream"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
	chaosVendorBudget  = uint32(100) // requests per second per upstream
	chaosMailboxBurst  = 1500        // above the tracker's mailbox header cap of 1024
	chaosFinalizedLag  = uint64(64)
	chaosHistoryBlocks = 40
)

func chaosBlocksFor(d time.Duration) int {
	return int((d + 12*time.Second - 1) / (12 * time.Second))
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
)

var chaosFaults = []string{
	fReorgSmall, fMinorityFork, fAheadFork, fDeepReorg, fSilent, fLagging, fDown, fAllDown, fHalt,
	f429, fLate, fLoad, fSplit, fNonLinking, fRestart, fOrphanCached, fMailbox, fPanicOnce, fPanicThrice,
}

// TestChainFeed_ConvergesUnderFaults runs real eRPC against 4 mocked vendors
// of one simulated chain (2 wss, 2 HTTP) while a seeded schedule injects
// network, node and reorg faults. Each ws newHeads client applies what it
// receives as a geth client does; its chain must end equal to the
// simulated canonical chain, it never sees a block off the canonical
// chain, and it never waits more than 3 live bounds while a vendor serves.
func TestChainFeed_ConvergesUnderFaults(t *testing.T) {
	origWatch := wsupstream.HeadsWatchInterval.Load()
	wsupstream.HeadsWatchInterval.Store(chaosHeadsWatch)
	t.Cleanup(func() { wsupstream.HeadsWatchInterval.Store(origWatch) })

	// Each plan's faults happen in its seed's order, on vendors its seed
	// picks; together the 4 plans cover every fault.
	plans := []struct {
		seed        int64
		pollSources bool
		faults      []string
	}{
		{11, true, []string{fMinorityFork, fAheadFork, f429, fLate, fMailbox, fPanicOnce}},
		{23, true, []string{fDown, fLagging, fDeepReorg, fSilent, fPanicThrice, fNonLinking}},
		{37, true, []string{fAllDown, fHalt, fLoad, fOrphanCached, fRestart}},
		{41, false, []string{fSplit, fLate, fDeepReorg}},
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
		})
		if !ok {
			return
		}
	}
	for _, f := range chaosFaults {
		require.NotEmpty(t, covered[f], "fault %q occurred in no seed", f)
	}
}

//
// --- simulated chain ---
//

type simBlock struct {
	number   uint64
	hash     [32]byte
	parent   *simBlock
	ts       uint64
	minority bool // never canonical: a fork only some vendors serve
}

func (b *simBlock) json() map[string]interface{} {
	var parent [32]byte
	if b.parent != nil {
		parent = b.parent.hash
	}
	const emptyTrie = "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"
	return map[string]interface{}{
		"number":           fmt.Sprintf("0x%x", b.number),
		"hash":             "0x" + hex.EncodeToString(b.hash[:]),
		"parentHash":       "0x" + hex.EncodeToString(parent[:]),
		"timestamp":        fmt.Sprintf("0x%x", b.ts),
		"logsBloom":        "0x" + strings.Repeat("0", 512),
		"transactionsRoot": emptyTrie,
		"receiptsRoot":     emptyTrie,
		"sha3Uncles":       "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
		"miner":            "0x0000000000000000000000000000000000000001",
		"gasLimit":         "0x1c9c380",
		"gasUsed":          "0x0",
		"baseFeePerGas":    "0x7",
		"difficulty":       "0x0",
		"extraData":        "0x",
		"size":             "0x220",
		"transactions":     []interface{}{},
		"uncles":           []interface{}{},
	}
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
	minted   uint64
	canon    []*simBlock
	byHash   map[[32]byte]*simBlock
	halted   bool
	lastDeep time.Time
	vendors  []*simVendor
}

func newSimChain(seed int64) *simChain {
	c := &simChain{seed: seed, byHash: make(map[[32]byte]*simBlock)}
	start := time.Now().Add(-time.Duration(chaosHistoryBlocks) * chaosBlock)
	var parent *simBlock
	for i := range chaosHistoryBlocks {
		b := c.mintLocked(parent, 0x1000+uint64(i), start.Add(time.Duration(i)*chaosBlock), false)
		c.canon = append(c.canon, b)
		parent = b
	}
	return c
}

func (c *simChain) mintLocked(parent *simBlock, number uint64, at time.Time, minority bool) *simBlock {
	c.minted++
	var ph [32]byte
	if parent != nil {
		ph = parent.hash
	}
	h := sha256.Sum256(fmt.Appendf(nil, "%d/%d/%d/%x", c.seed, c.minted, number, ph))
	ts := uint64(at.Unix())
	if parent != nil && ts < parent.ts {
		ts = parent.ts
	}
	b := &simBlock{number: number, hash: h, parent: parent, ts: ts, minority: minority}
	c.byHash[h] = b
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
	id    string
	ws    bool
	chain *simChain
	url   string

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

	connects    atomic.Int64
	subscribes  atomic.Int64
	refused     atomic.Int64
	throttled   atomic.Int64
	lateAnswers atomic.Int64
	lagAnswers  atomic.Int64
	minorityOut atomic.Int64

	byNumberMu sync.Mutex
	byNumber   map[uint64]int
}

func (c *simChain) addVendor(t *testing.T, id string, ws bool) *simVendor {
	v := &simVendor{id: id, ws: ws, chain: c, seen: make(map[[32]byte]bool), conns: make(map[*websocket.Conn]string), byNumber: make(map[uint64]int)}
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
	c.mu.Unlock()
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
		mockWriteMus.Delete(conn)
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
	lastLag    float64
	problems   []string
	conn       *websocket.Conn
}

func lagRestarts() float64 {
	return testutil.ToFloat64(telemetry.MetricTapOutOfRangeTotal.WithLabelValues("test_ws", "evm:123", "ws", indexer.SubTypeNewHeads, indexer.OutOfRangeLag))
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
	cl.conn = conn
	cl.subscribed = time.Now()
	cl.lastLag = lagRestarts()
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
	lag := lagRestarts()
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
		if deep || lag > cl.lastLag {
			cl.floor = n
		} else {
			cl.problems = append(cl.problems, fmt.Sprintf("jump %d → %d with no fault that calls for a restart", cl.last, n))
		}
	}
	cl.lastLag = lag
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

//
// --- one seed ---
//

type interval struct{ from, to time.Time }

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
	quiet    []interval // no vendor serves the chain
	occurred map[string]string

	breakerMu     sync.Mutex
	breakerOpened string
	breakerAt     time.Time
	loadFrom      time.Time

	hook *chaosHook
}

func newChaosSeed(t *testing.T, seed int64, pollSources bool) *chaosSeed {
	s := &chaosSeed{t: t, seed: seed, rng: rand.New(rand.NewPCG(uint64(seed), 0)), pollSources: pollSources, occurred: make(map[string]string)}
	s.chain = newSimChain(seed)
	s.wsA = s.chain.addVendor(t, "ws-a", true)
	s.wsB = s.chain.addVendor(t, "ws-b", true)
	s.httpA = s.chain.addVendor(t, "http-a", false)
	s.httpB = s.chain.addVendor(t, "http-b", false)
	s.vendors = []*simVendor{s.wsA, s.wsB, s.httpA, s.httpB}
	s.hook = &chaosHook{s: s, first: make(map[*evm.ChainTracker]indexer.Cursor), firstFrom: make(map[*evm.ChainTracker]string)}
	fn := s.hook.on
	evm.TrackerMessageHook.Store(&fn)
	t.Cleanup(func() { evm.TrackerMessageHook.Store(nil) })
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

func (s *chaosSeed) stopERPC() {
	_ = s.srv.Shutdown(&zerolog.Logger{})
	s.cancel()
	s.mu.Lock()
	clients := slices.Clone(s.clients)
	s.mu.Unlock()
	for _, cl := range clients {
		_ = cl.conn.Close()
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
		}
		if s.rng.IntN(2) == 0 {
			s.reorgSmall()
		}
		s.blocks(2 + s.rng.IntN(3))
		if f == fDown {
			bg.Add(1)
			v := s.pick(s.vendors...)
			go func() { defer bg.Done(); s.vendorDown(v) }()
			s.blocks(2)
			continue
		}
		s.inject(f)
	}
	bg.Wait()
	if _, ok := s.occurred[fReorgSmall]; !ok {
		s.reorgSmall()
	}

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
	end := time.Now()
	s.stopERPC()
	s.judge(end)
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

func (s *chaosSeed) deepReorg() {
	depth := chaosReorgWindow + 4
	tip := s.chain.reorg(depth)
	s.blocks(4)
	s.occur(fDeepReorg, fmt.Sprintf("depth %d > reorgWindow %d, new head %d", depth, chaosReorgWindow, tip.number))
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

func (s *chaosSeed) allDown() {
	from := time.Now()
	for _, v := range s.vendors {
		v.setDown(true)
	}
	s.blocks(chaosBlocksFor(2 * time.Minute))
	for _, v := range s.vendors {
		v.setDown(false)
	}
	s.addQuiet(interval{from, time.Now()})
	s.blocks(4)
	s.occur(fAllDown, fmt.Sprintf("all 4 vendors down for %d blocks", chaosBlocksFor(2*time.Minute)))
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
	for range 32 {
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
	// The HTTP vendors are no sources here: only the 2 wss vendors vote.
	settled := telemetry.MetricChainTrackerSettleTotal.WithLabelValues("test_ws", "evm:123", "decided")
	before := testutil.ToFloat64(settled)
	s.httpB.setDown(true)
	c := s.chain
	c.mu.Lock()
	tip := c.tipLocked()
	// ws-b's sibling of the next canonical block, pushed as it lands.
	c.canon = append(c.canon, c.childLocked(tip, time.Now(), false))
	f := &simFork{tip: c.childLocked(tip, time.Now(), true), grows: true}
	s.wsB.fork = f
	c.mu.Unlock()
	c.refresh()
	s.blocks(5)
	s.setFork(nil, s.wsB)
	s.httpB.setDown(false)
	s.blocks(2)
	decided := testutil.ToFloat64(settled) - before
	require.Positive(s.t, decided, "a settle decides the 1-vs-1 split")
	s.occur(fSplit, fmt.Sprintf("ws-a vs ws-b at %d, %v settles decided by http-a", tip.number+1, decided))
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
	s.mu.Unlock()
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

func httptestServer(t *testing.T, h http.Handler) string {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}
