package chainsim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/gorilla/websocket"
)

// Faults are a vendor's app-level faults; the zero value is a healthy node.
type Faults struct {
	Down          bool          // HTTP 503, ws upgrades refused, open sockets closed
	Held          bool          // block and head answers wait until it clears; newHeads pushes nothing meanwhile, then its head
	Lag           uint64        // serves the canonical block this far below the head
	Frozen        bool          // head stops moving; still answers what it holds
	WrongChainID  bool          // eth_chainId answers another chain
	ChainIDHeld   bool          // eth_chainId answers wait until it clears
	NullHeld      bool          // null for blocks by number or hash, even below its head
	WrongHashes   bool          // newHeads pushes carry a hash the header does not hash to
	NullReceipts  bool          // eth_getBlockReceipts answers null
	ReceiptsDown  bool          // eth_getBlockReceipts answers -32603
	LogsDown      bool          // eth_getLogs answers -32603
	CapLogs       bool          // eth_getLogs one log short where the bloom and logIndexes hide it (lastLogCovered)
	TruncReceipts bool          // receipts without their last 10
	ForgedLog     bool          // receipts whose first log's data is forged
	WrongBloom    bool          // receipts whose logsBloom is zero
	ParentHash    bool          // receipts that name the parent's hash as their blockHash
	OffBloom      bool          // receipts whose logsBloom is set but not the header's (bloomMatch)
	NoReceipts    bool          // eth_getBlockReceipts answers -32601
	Throttled     bool          // HTTP 429; -32005 over ws
	Capacity      bool          // -32005 compute units exceeded, on every method
	NoNewHeads    bool          // eth_subscribe newHeads answers unsupported
	RefuseLatest  bool          // eth_getBlockByNumber(latest) answers unsupported
	Slow          time.Duration // every answer waits this long
	SlowReceipts  time.Duration // receipts and logs answers wait this long more
	SlowHistory   time.Duration // answers by hash, and by number below its head, wait this long more
	SilentHeads   bool          // newHeads subscriptions stay open and push nothing
	SilentPending bool          // newPendingTransactions subscriptions push nothing
	Ahead         uint64        // eth_blockNumber and the latest tag claim a head this far above its own
	Syncing       bool          // eth_syncing answers in progress; still serves what it holds
	FinalityDepth uint64        // finalized tag this far below its head; 0 is FinalizedLag
	StaleState    uint64        // eth_call at latest (or no block) executes this far below its head; numbered calls are right
}

// Stats are what a vendor counted since it started.
type Stats struct {
	Connects, HeadsSubscribes, PendingSubscribes, LogsSubscribes, Throttled int64
	HeadsRefused                                                            int64 // newHeads subscribes NoNewHeads refused
	ReceiptsCalls, LogsCalls                                                int64 // eth_getBlockReceipts, eth_getLogs calls
	SlowAnswers                                                             int64 // answers a Slow fault delayed
	ForgedHeads                                                             int64 // heads pushed with a wrong hash, once per head
	CappedLogs                                                              int64 // eth_getLogs answers CapLogs cut short
	CapacityRefused                                                         int64 // calls a Capacity fault refused
}

// Vendor is one simulated node of a chain, on its own listener.
type Vendor struct {
	ID   string
	Name string // upstreams of one vendor share it
	WS   bool

	chain *Chain
	ln    net.Listener
	srv   *http.Server

	// Guarded by chain.mu.
	tip    *Block
	seen   map[Hash]bool
	faults Faults
	fork   *Fork
	conns  map[*wsConn]bool
	// held closes when ChainIDHeld clears, blocksHeld when Held clears;
	// each nil while its fault is off.
	held, blocksHeld chan struct{}
	// heldTip is the tip when Held was set: a release pushes a tip that
	// moved since.
	heldTip *Block
	// numberCalls are when eth_getBlockByNumber asked each explicit number.
	numberCalls map[uint64][]time.Time
	// receipts counts eth_getBlockReceipts calls per block; served are the
	// blocks answered with their receipts in full.
	receipts map[Hash]int
	served   map[Hash]bool
	// latestCalls are the eth_getBlockByNumber(latest) calls answered.
	latestCalls []LatestCall

	connects, headsSubs, pendingSubs, logsSubs, throttled, receiptsCalls  atomic.Int64
	logsCalls, slowAnswers, forgedHeads, cappedLogs, subSeq, headsRefused atomic.Int64
	capacityRefused                                                       atomic.Int64
}

// LatestCall is one eth_getBlockByNumber(latest) call a vendor answered.
type LatestCall struct {
	At      time.Time
	Refused bool // RefuseLatest refused it
}

type wsConn struct {
	c  *websocket.Conn
	mu sync.Mutex
	// pushes queues subscription notifications for the connection's own
	// writer, so a peer that stops reading never stalls the chain's tick
	// for every other connection; done ends the writer.
	pushes chan interface{}
	done   chan struct{}
	// Guarded by chain.mu.
	heads   map[string]bool
	pending map[string]bool
}

// wsPushQueue is how many notifications a connection may owe its peer;
// past it the vendor drops the peer, as a node drops a subscriber that
// does not read.
const wsPushQueue = 1024

func (w *wsConn) write(v interface{}) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return w.c.WriteJSON(v)
}

func (w *wsConn) writePushes() {
	for {
		select {
		case <-w.done:
			return
		case m := <-w.pushes:
			if w.write(m) != nil {
				_ = w.c.Close()
				return
			}
		}
	}
}

type push struct {
	conn *wsConn
	sub  string
	res  interface{}
}

func notification(sub string, res interface{}) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0", "method": "eth_subscription",
		"params": map[string]interface{}{"subscription": sub, "result": res},
	}
}

func writePushes(ps []push) {
	for _, p := range ps {
		select {
		case p.conn.pushes <- notification(p.sub, p.res):
		default:
			_ = p.conn.c.Close()
		}
	}
}

// AddVendor starts a vendor on a free 127.0.0.1 port. A ws vendor answers
// JSON-RPC over WebSocket; every vendor answers HTTP POST too.
func (c *Chain) AddVendor(id, name string, ws bool) (*Vendor, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	v := &Vendor{ID: id, Name: name, WS: ws, chain: c, ln: ln, seen: make(map[Hash]bool), conns: make(map[*wsConn]bool),
		numberCalls: make(map[uint64][]time.Time), receipts: make(map[Hash]int), served: make(map[Hash]bool)}
	v.srv = &http.Server{Handler: v, ReadHeaderTimeout: 10 * time.Second}
	c.mu.Lock()
	v.setTipLocked(c.tipLocked())
	c.vendors = append(c.vendors, v)
	c.mu.Unlock()
	go func() { _ = v.srv.Serve(ln) }()
	return v, nil
}

// Addr is the vendor's host:port.
func (v *Vendor) Addr() string { return v.ln.Addr().String() }

// URL is the endpoint an upstream config names.
func (v *Vendor) URL() string {
	if v.WS {
		return "ws://" + v.Addr()
	}
	return "http://" + v.Addr()
}

// Faults are the vendor's current faults.
func (v *Vendor) Faults() Faults {
	v.chain.mu.Lock()
	defer v.chain.mu.Unlock()
	return v.faults
}

// SetFaults replaces the vendor's faults; its view follows at once.
func (v *Vendor) SetFaults(f Faults) { v.Update(func(old *Faults) { *old = f }) }

// Update changes some of the vendor's faults in one step; its view follows
// at once.
func (v *Vendor) Update(fn func(*Faults)) {
	c := v.chain
	c.mu.Lock()
	fn(&v.faults)
	f := v.faults
	switch {
	case f.ChainIDHeld && v.held == nil:
		v.held = make(chan struct{})
	case !f.ChainIDHeld && v.held != nil:
		close(v.held)
		v.held = nil
	}
	var pushes []push
	switch {
	case f.Held && v.blocksHeld == nil:
		v.blocksHeld, v.heldTip = make(chan struct{}), v.tip
	case !f.Held && v.blocksHeld != nil:
		close(v.blocksHeld)
		v.blocksHeld = nil
		if v.tip != v.heldTip {
			pushes = v.headPushesLocked(pushes)
		}
	}
	var drop []*wsConn
	if f.Down {
		for w := range v.conns {
			drop = append(drop, w)
		}
	}
	c.mu.Unlock()
	for _, w := range drop {
		_ = w.c.Close()
	}
	writePushes(pushes)
	c.refresh()
}

// Head is the block the vendor serves as latest.
func (v *Vendor) Head() *Block {
	v.chain.mu.Lock()
	defer v.chain.mu.Unlock()
	return v.tip
}

// LatestCalls are the eth_getBlockByNumber(latest) calls the vendor
// answered in [from, to], oldest first.
func (v *Vendor) LatestCalls(from, to time.Time) []LatestCall {
	v.chain.mu.Lock()
	defer v.chain.mu.Unlock()
	var out []LatestCall
	for _, c := range v.latestCalls {
		if !c.At.Before(from) && !c.At.After(to) {
			out = append(out, c)
		}
	}
	return out
}

// NumberCalls counts the eth_getBlockByNumber calls for explicit number n
// the vendor answered in [from, to].
func (v *Vendor) NumberCalls(n uint64, from, to time.Time) int {
	v.chain.mu.Lock()
	defer v.chain.mu.Unlock()
	k := 0
	for _, at := range v.numberCalls[n] {
		if !at.Before(from) && !at.After(to) {
			k++
		}
	}
	return k
}

// Stats are the vendor's counters.
func (v *Vendor) Stats() Stats {
	return Stats{Connects: v.connects.Load(), HeadsSubscribes: v.headsSubs.Load(), PendingSubscribes: v.pendingSubs.Load(),
		LogsSubscribes: v.logsSubs.Load(), Throttled: v.throttled.Load(), HeadsRefused: v.headsRefused.Load(), ReceiptsCalls: v.receiptsCalls.Load(),
		LogsCalls: v.logsCalls.Load(), SlowAnswers: v.slowAnswers.Load(), ForgedHeads: v.forgedHeads.Load(), CappedLogs: v.cappedLogs.Load(),
		CapacityRefused: v.capacityRefused.Load()}
}

// ReceiptsOf are the eth_getBlockReceipts calls the vendor got for block h,
// and whether it answered one with the block's receipts in full.
func (v *Vendor) ReceiptsOf(h Hash) (calls int, served bool) {
	v.chain.mu.Lock()
	defer v.chain.mu.Unlock()
	return v.receipts[h], v.served[h]
}

// Push writes raws, in order, on each of the vendor's newHeads
// subscriptions at once, past its push queue, as a node that floods its
// subscribers. It returns how many messages it wrote.
func (v *Vendor) Push(raws ...json.RawMessage) int {
	type sub struct {
		w  *wsConn
		id string
	}
	var subs []sub
	v.chain.mu.Lock()
	for w := range v.conns {
		for id := range w.heads {
			subs = append(subs, sub{w, id})
		}
	}
	v.chain.mu.Unlock()
	n := 0
	for _, s := range subs {
		for _, raw := range raws {
			if s.w.write(notification(s.id, raw)) != nil {
				break
			}
			n++
		}
	}
	return n
}

func (v *Vendor) close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = v.srv.Shutdown(ctx)
	_ = v.srv.Close()
	v.chain.mu.Lock()
	var conns []*wsConn
	for w := range v.conns {
		conns = append(conns, w)
	}
	v.chain.mu.Unlock()
	for _, w := range conns {
		_ = w.c.Close()
	}
}

func (v *Vendor) targetLocked() *Block {
	switch {
	case v.faults.Frozen:
		return v.tip
	case v.fork != nil:
		return v.fork.tip
	}
	tip := v.chain.tipLocked()
	if v.faults.Lag > 0 && tip.Number > v.faults.Lag {
		if b := v.chain.canonAtLocked(tip.Number - v.faults.Lag); b != nil {
			return b
		}
	}
	return tip
}

func (v *Vendor) setTipLocked(b *Block) {
	v.tip = b
	for a, i := b, 0; a != nil && !v.seen[a.Hash] && i < 4096; a, i = a.Parent, i+1 {
		v.seen[a.Hash] = true
	}
}

// followLocked moves the vendor to its target and adds the pushes of its
// new tip.
func (v *Vendor) followLocked(out []push) []push {
	target := v.targetLocked()
	if target == v.tip {
		return out
	}
	v.setTipLocked(target)
	if v.faults.Held {
		return out
	}
	return v.headPushesLocked(out)
}

// headPushesLocked adds the pushes of the vendor's tip on its newHeads
// subscriptions.
func (v *Vendor) headPushesLocked(out []push) []push {
	if !v.WS || v.faults.Down || v.faults.SilentHeads {
		return out
	}
	raw := v.tip.JSON()
	if v.faults.WrongHashes {
		raw = ForgeHash(raw)
	}
	n := len(out)
	for w := range v.conns {
		for sub := range w.heads {
			out = append(out, push{w, sub, raw})
		}
	}
	if v.faults.WrongHashes && len(out) > n {
		v.forgedHeads.Add(1)
	}
	return out
}

// pendingPushesLocked adds b's transactions on the vendor's pending
// subscriptions; with PendingNoise a quarter are dropped and a quarter
// sent twice.
func (v *Vendor) pendingPushesLocked(b *Block, out []push) []push {
	if !v.WS || v.faults.Down || v.faults.SilentPending {
		return out
	}
	c := v.chain
	for i := range b.txs {
		h := txHash(b.Hash, i)
		for w := range v.conns {
			for sub := range w.pending {
				times := 1
				if c.cfg.PendingNoise {
					times = []int{0, 2, 1, 1}[c.noise.IntN(4)]
				}
				for range times {
					out = append(out, push{w, sub, h.Hex()})
					c.pushed[h] = true
				}
			}
		}
	}
	return out
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func reply(id interface{}, res interface{}, e *rpcError) map[string]interface{} {
	out := map[string]interface{}{"jsonrpc": "2.0", "id": id}
	if e != nil {
		out["error"] = e
	} else {
		out["result"] = res
	}
	return out
}

func (v *Vendor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f := v.Faults()
	if f.Down {
		http.Error(w, "vendor down", http.StatusServiceUnavailable)
		return
	}
	if websocket.IsWebSocketUpgrade(r) {
		v.serveWS(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	if f.Throttled {
		v.throttled.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(reply(nil, nil, &rpcError{-32005, "rate limit exceeded"}))
		return
	}
	one := func(req map[string]interface{}) map[string]interface{} {
		method, _ := req["method"].(string)
		params, _ := req["params"].([]interface{})
		res, e := v.answer(method, params)
		return reply(req["id"], res, e)
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

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func (v *Vendor) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	v.connects.Add(1)
	wc := &wsConn{c: conn, pushes: make(chan interface{}, wsPushQueue), done: make(chan struct{}),
		heads: make(map[string]bool), pending: make(map[string]bool)}
	go wc.writePushes()
	c := v.chain
	c.mu.Lock()
	v.conns[wc] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(v.conns, wc)
		c.mu.Unlock()
		close(wc.done)
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
		go v.handleWS(wc, req)
	}
}

func (v *Vendor) handleWS(wc *wsConn, req map[string]interface{}) {
	method, _ := req["method"].(string)
	params, _ := req["params"].([]interface{})
	f := v.Faults()
	c := v.chain
	var res interface{}
	var e *rpcError
	switch {
	case f.Throttled:
		v.throttled.Add(1)
		e = &rpcError{-32005, "rate limit exceeded"}
	case method == "eth_subscribe":
		topic, _ := firstString(params)
		sub := fmt.Sprintf("0x%x", v.subSeq.Add(1))
		c.mu.Lock()
		_, open := v.conns[wc]
		switch {
		case topic == "newHeads" && f.NoNewHeads:
			v.headsRefused.Add(1)
			e = &rpcError{-32601, "notifications not supported"}
		case topic == "newHeads":
			v.headsSubs.Add(1)
			if open {
				wc.heads[sub] = true
			}
		case topic == "newPendingTransactions":
			v.pendingSubs.Add(1)
			if open {
				wc.pending[sub] = true
			}
		case topic == "logs":
			v.logsSubs.Add(1)
		default:
			e = &rpcError{-32602, "unknown subscription " + topic}
		}
		c.mu.Unlock()
		res = sub
	case method == "eth_unsubscribe":
		sub, _ := firstString(params)
		c.mu.Lock()
		delete(wc.heads, sub)
		delete(wc.pending, sub)
		c.mu.Unlock()
		res = true
	default:
		res, e = v.answer(method, params)
	}
	if wc.write(reply(req["id"], res, e)) != nil {
		_ = wc.c.Close()
	}
}

func firstString(params []interface{}) (string, bool) {
	if len(params) == 0 {
		return "", false
	}
	s, ok := params[0].(string)
	return s, ok
}

func parseHash(s string) (Hash, bool) {
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	if len(s) != 64 {
		return Hash{}, false
	}
	return gethcommon.HexToHash(s), true
}

func parseNumber(s string) (uint64, bool) {
	var n uint64
	if _, err := fmt.Sscanf(strings.ToLower(s), "0x%x", &n); err != nil {
		return 0, false
	}
	return n, true
}

// byNumberLocked walks the vendor's own branch down to n.
func (v *Vendor) byNumberLocked(n uint64) *Block {
	b := v.tip
	for b != nil && b.Number > n {
		b = b.Parent
	}
	if b == nil || b.Number != n {
		return nil
	}
	return b
}

// answer serves one JSON-RPC call from the vendor's view and faults.
func (v *Vendor) answer(method string, params []interface{}) (interface{}, *rpcError) {
	if method == "eth_getBlockReceipts" {
		if res, ok := v.trapped(params); ok {
			return res, nil
		}
	}
	if d := v.delay(method, params); d > 0 {
		v.slowAnswers.Add(1)
		time.Sleep(d)
	}
	c := v.chain
	c.mu.Lock()
	var held chan struct{}
	switch method {
	case "eth_chainId":
		held = v.held
	case "eth_blockNumber", "eth_getBlockByNumber", "eth_getBlockByHash":
		held = v.blocksHeld
	}
	c.mu.Unlock()
	if held != nil {
		<-held
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	f := v.faults
	if f.Capacity {
		v.capacityRefused.Add(1)
		return nil, &rpcError{-32005, "compute units exceeded"}
	}
	switch method {
	case "eth_chainId":
		id := c.cfg.ChainID
		if f.WrongChainID {
			id++
		}
		return hexU(uint64(id)), nil
	case "net_version":
		return fmt.Sprint(c.cfg.ChainID), nil
	case "eth_syncing":
		if f.Syncing {
			return map[string]interface{}{"startingBlock": hexU(c.canon[0].Number), "currentBlock": hexU(v.tip.Number),
				"highestBlock": hexU(c.tipLocked().Number + 1000)}, nil
		}
		return false, nil
	case "eth_blockNumber":
		return hexU(v.tip.Number + f.Ahead), nil
	case "eth_call":
		return v.callLocked(params)
	case "eth_getBlockByHash":
		hs, _ := firstString(params)
		h, ok := parseHash(hs)
		if !ok {
			return nil, &rpcError{-32602, "invalid hash"}
		}
		if f.NullHeld || !v.seen[h] {
			return nil, nil
		}
		return c.byHash[h].JSON(), nil
	case "eth_getBlockByNumber":
		tag, _ := firstString(params)
		var b *Block
		switch tag {
		case "latest", "pending":
			v.latestCalls = append(v.latestCalls, LatestCall{At: time.Now(), Refused: f.RefuseLatest})
			if f.RefuseLatest {
				return nil, &rpcError{-32000, "latest block tag not supported"}
			}
			if f.Ahead > 0 {
				return WithNumber(v.tip.JSON(), v.tip.Number+f.Ahead), nil
			}
			b = v.tip
		case "finalized", "safe":
			if c.cfg.NoFinalized {
				return nil, &rpcError{-32602, "invalid block number tag " + tag}
			}
			depth := uint64(FinalizedLag)
			if f.FinalityDepth > 0 {
				depth = f.FinalityDepth
			}
			n := c.canon[0].Number
			if v.tip.Number > depth && v.tip.Number-depth > n {
				n = v.tip.Number - depth
			}
			b = v.byNumberLocked(n)
		case "earliest":
			b = c.canon[0]
		default:
			n, ok := parseNumber(tag)
			if !ok {
				return nil, &rpcError{-32602, "invalid block number"}
			}
			v.numberCalls[n] = append(v.numberCalls[n], time.Now())
			if f.NullHeld {
				return nil, nil
			}
			b = v.byNumberLocked(n)
		}
		if b == nil {
			return nil, nil
		}
		return b.JSON(), nil
	case "eth_getBlockReceipts":
		v.receiptsCalls.Add(1)
		hs, _ := firstString(params)
		h, ok := parseHash(hs)
		if !ok {
			return nil, &rpcError{-32602, "invalid block hash"}
		}
		v.receipts[h]++
		switch {
		case f.NoReceipts:
			return nil, &rpcError{-32601, "the method eth_getBlockReceipts does not exist/is not available"}
		case f.ReceiptsDown || c.failingLocked(h):
			return nil, &rpcError{-32603, "receipts unavailable"}
		}
		b := c.byHash[h]
		if b == nil || !v.seen[h] || f.NullHeld || f.NullReceipts {
			return nil, nil
		}
		drop := 0
		if f.TruncReceipts {
			drop = 10
		}
		if drop == 0 && !f.WrongBloom && !f.OffBloom && !f.ForgedLog && !f.ParentHash {
			v.served[h] = true
		}
		rs := b.receipts(f.WrongBloom, f.OffBloom, drop, f.ForgedLog)
		if f.ParentHash {
			for _, r := range rs {
				r["blockHash"] = b.ParentHash().Hex()
			}
		}
		return rs, nil
	case "eth_getTransactionReceipt":
		hs, _ := firstString(params)
		h, ok := parseHash(hs)
		if !ok {
			return nil, &rpcError{-32602, "invalid transaction hash"}
		}
		tx, ok := c.byTx[h]
		if !ok || f.NullHeld || v.byNumberLocked(tx.b.Number) != tx.b {
			return nil, nil
		}
		return tx.b.Receipt(tx.i), nil
	case "eth_getLogs":
		v.logsCalls.Add(1)
		if f.LogsDown {
			return nil, &rpcError{-32603, "logs unavailable"}
		}
		return v.logsLocked(params)
	case "eth_getBlockTransactionCountByNumber":
		tag, _ := firstString(params)
		n, ok := parseNumber(tag)
		if !ok {
			return nil, &rpcError{-32602, "invalid block number"}
		}
		b := v.byNumberLocked(n)
		if b == nil {
			return nil, nil
		}
		return hexU(uint64(len(b.txs))), nil
	}
	return "0x0", nil
}

// delay is how long the vendor's Slow faults hold an answer to method.
func (v *Vendor) delay(method string, params []interface{}) time.Duration {
	c := v.chain
	c.mu.Lock()
	defer c.mu.Unlock()
	f := v.faults
	d := f.Slow
	switch method {
	case "eth_getBlockReceipts", "eth_getLogs":
		d += f.SlowReceipts
	case "eth_getBlockByHash":
		d += f.SlowHistory
	case "eth_getBlockByNumber":
		tag, _ := firstString(params)
		if n, ok := parseNumber(tag); ok && n < v.tip.Number {
			d += f.SlowHistory
		}
	}
	return d
}

// trapped answers a block's first receipts call while the chain's trap is
// set: after the trap's hold, with zero logsBlooms.
func (v *Vendor) trapped(params []interface{}) (interface{}, bool) {
	hs, _ := firstString(params)
	h, ok := parseHash(hs)
	if !ok {
		return nil, false
	}
	c := v.chain
	c.mu.Lock()
	tr, b := c.traps[h], c.byHash[h]
	if tr != nil && tr.AnsweredAt.IsZero() {
		tr.During++
	}
	if c.trapHold == 0 || tr != nil || b == nil || len(b.txs) == 0 || !v.seen[h] || v.faults.Down || v.faults.NoReceipts {
		c.mu.Unlock()
		return nil, false
	}
	tr = &Trap{}
	c.traps[h] = tr
	hold := c.trapHold
	c.mu.Unlock()
	time.Sleep(hold)
	c.mu.Lock()
	defer c.mu.Unlock()
	tr.AnsweredAt = time.Now()
	v.receipts[h]++
	v.receiptsCalls.Add(1)
	return b.receipts(true, false, 0, false), true
}

// Multicall3 and its getBlockNumber() selector: the call returns the
// number of the block it executes at.
var (
	Multicall3        = gethcommon.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11")
	GetBlockNumberSig = "0x42cbb15c"
)

// callLocked answers eth_call: Multicall3 getBlockNumber() at the block the
// call names (latest, a number, or an EIP-1898 object) on the vendor's
// branch; any other call "0x0".
func (v *Vendor) callLocked(params []interface{}) (interface{}, *rpcError) {
	if len(params) == 0 {
		return nil, &rpcError{-32602, "missing call"}
	}
	tx, _ := params[0].(map[string]interface{})
	to, _ := tx["to"].(string)
	data, _ := tx["data"].(string)
	if data == "" {
		data, _ = tx["input"].(string)
	}
	if !strings.EqualFold(to, Multicall3.Hex()) || !strings.EqualFold(data, GetBlockNumberSig) {
		return "0x0", nil
	}
	at := v.tip
	if s := v.faults.StaleState; s > 0 {
		at = v.byNumberLocked(max(v.tip.Number, v.chain.canon[0].Number+s) - s)
	}
	if len(params) > 1 {
		switch ref := params[1].(type) {
		case string:
			switch ref {
			case "latest", "pending":
			default:
				n, ok := parseNumber(ref)
				if !ok {
					return nil, &rpcError{-32602, "invalid block number"}
				}
				at = v.byNumberLocked(n)
			}
		case map[string]interface{}:
			if s, ok := ref["blockNumber"].(string); ok {
				n, _ := parseNumber(s)
				at = v.byNumberLocked(n)
			} else if s, ok := ref["blockHash"].(string); ok {
				h, _ := parseHash(s)
				if at = v.chain.byHash[h]; at != nil && !v.seen[h] {
					at = nil
				}
			}
		}
	}
	if at == nil {
		return nil, &rpcError{-32000, "header not found"}
	}
	return fmt.Sprintf("0x%064x", at.Number), nil
}

// logsLocked answers eth_getLogs by blockHash or by range on the vendor's
// branch, with the filter's address and topics.
func (v *Vendor) logsLocked(params []interface{}) (interface{}, *rpcError) {
	q, _ := params[0].(map[string]interface{})
	if q == nil {
		return nil, &rpcError{-32602, "invalid filter"}
	}
	var blocks []*Block
	if hs, ok := q["blockHash"].(string); ok {
		h, ok := parseHash(hs)
		if !ok {
			return nil, &rpcError{-32602, "invalid block hash"}
		}
		if v.chain.failingLocked(h) {
			return nil, &rpcError{-32603, "logs unavailable"}
		}
		b := v.chain.byHash[h]
		if b == nil || !v.seen[h] {
			return nil, &rpcError{-32000, "unknown block"}
		}
		blocks = []*Block{b}
	} else {
		from, to := v.tip.Number, v.tip.Number
		if s, ok := q["fromBlock"].(string); ok && s != "latest" {
			from, _ = parseNumber(s)
		}
		if s, ok := q["toBlock"].(string); ok && s != "latest" {
			to, _ = parseNumber(s)
		}
		for n := from; n <= to && n <= v.tip.Number; n++ {
			if b := v.byNumberLocked(n); b != nil {
				blocks = append(blocks, b)
			}
		}
	}
	addrs, topics := queryOf(q)
	out := []map[string]interface{}{}
	for _, b := range blocks {
		logs := b.Logs()
		if v.faults.CapLogs && b.lastLogCovered() {
			logs = logs[:len(logs)-1]
			v.cappedLogs.Add(1)
		}
		for _, l := range logs {
			if matchQuery(l, addrs, topics) {
				out = append(out, l.json())
			}
		}
	}
	return out, nil
}

// queryOf parses a filter's address (one or a list) and positional topics
// (null, one, or a list each), as geth does.
func queryOf(q map[string]interface{}) (map[gethcommon.Address]bool, []map[Hash]bool) {
	var addrs map[gethcommon.Address]bool
	add := func(s interface{}) {
		if str, ok := s.(string); ok {
			addrs[gethcommon.HexToAddress(str)] = true
		}
	}
	switch a := q["address"].(type) {
	case string:
		addrs = map[gethcommon.Address]bool{}
		add(a)
	case []interface{}:
		addrs = map[gethcommon.Address]bool{}
		for _, s := range a {
			add(s)
		}
	}
	var topics []map[Hash]bool
	ts, _ := q["topics"].([]interface{})
	for _, t := range ts {
		switch t := t.(type) {
		case string:
			topics = append(topics, map[Hash]bool{gethcommon.HexToHash(t): true})
		case []interface{}:
			set := map[Hash]bool{}
			for _, s := range t {
				if str, ok := s.(string); ok {
					set[gethcommon.HexToHash(str)] = true
				}
			}
			topics = append(topics, set)
		default:
			topics = append(topics, nil)
		}
	}
	return addrs, topics
}

func matchQuery(l Log, addrs map[gethcommon.Address]bool, topics []map[Hash]bool) bool {
	if addrs != nil && !addrs[l.Address] {
		return false
	}
	if len(topics) > len(l.Topics) {
		return false
	}
	for i, set := range topics {
		if len(set) > 0 && !set[l.Topics[i]] {
			return false
		}
	}
	return true
}
