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
	Lag           uint64        // serves the canonical block this far below the head
	Frozen        bool          // head stops moving; still answers what it holds
	WrongChainID  bool          // eth_chainId answers another chain
	NullHeld      bool          // null for blocks by number or hash, even below its head
	WrongHashes   bool          // newHeads pushes carry a hash the header does not hash to
	TruncReceipts bool          // receipts without their last 10
	ForgedLog     bool          // receipts whose first log's data is forged
	WrongBloom    bool          // receipts whose logsBloom is zero
	NoReceipts    bool          // eth_getBlockReceipts answers -32601
	Throttled     bool          // HTTP 429; -32005 over ws
	Capacity      bool          // -32005 compute units exceeded, on every method
	NoNewHeads    bool          // eth_subscribe newHeads answers unsupported
	RefuseLatest  bool          // eth_getBlockByNumber(latest) answers an error
	Slow          time.Duration // every answer waits this long
	SilentHeads   bool          // newHeads subscriptions stay open and push nothing
	SilentPending bool          // newPendingTransactions subscriptions push nothing
}

// Stats are what a vendor counted since it started.
type Stats struct {
	Connects, HeadsSubscribes, PendingSubscribes, LogsSubscribes, Throttled int64
	// ReceiptsCalls counts eth_getBlockReceipts and eth_getLogs calls.
	ReceiptsCalls int64
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

	connects, headsSubs, pendingSubs, logsSubs, throttled, receiptsCalls atomic.Int64
	subSeq                                                               atomic.Int64
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

func writePushes(ps []push) {
	for _, p := range ps {
		select {
		case p.conn.pushes <- map[string]interface{}{
			"jsonrpc": "2.0", "method": "eth_subscription",
			"params": map[string]interface{}{"subscription": p.sub, "result": p.res},
		}:
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
	v := &Vendor{ID: id, Name: name, WS: ws, chain: c, ln: ln, seen: make(map[Hash]bool), conns: make(map[*wsConn]bool)}
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
func (v *Vendor) SetFaults(f Faults) {
	c := v.chain
	c.mu.Lock()
	v.faults = f
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
	c.refresh()
}

// Update changes some of the vendor's faults.
func (v *Vendor) Update(fn func(*Faults)) {
	f := v.Faults()
	fn(&f)
	v.SetFaults(f)
}

// Head is the block the vendor serves as latest.
func (v *Vendor) Head() *Block {
	v.chain.mu.Lock()
	defer v.chain.mu.Unlock()
	return v.tip
}

// Stats are the vendor's counters.
func (v *Vendor) Stats() Stats {
	return Stats{Connects: v.connects.Load(), HeadsSubscribes: v.headsSubs.Load(), PendingSubscribes: v.pendingSubs.Load(),
		LogsSubscribes: v.logsSubs.Load(), Throttled: v.throttled.Load(), ReceiptsCalls: v.receiptsCalls.Load()}
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
	if !v.WS || v.faults.Down || v.faults.SilentHeads {
		return out
	}
	raw := v.tip.JSON()
	if v.faults.WrongHashes {
		raw = forgeHash(raw)
	}
	for w := range v.conns {
		for sub := range w.heads {
			out = append(out, push{w, sub, raw})
		}
	}
	return out
}

func (v *Vendor) pendingPushesLocked(b *Block, out []push) []push {
	if !v.WS || v.faults.Down || v.faults.SilentPending {
		return out
	}
	for i := range b.txs {
		h := txHash(b.Hash, i).Hex()
		for w := range v.conns {
			for sub := range w.pending {
				out = append(out, push{w, sub, h})
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
	if slow := v.Faults().Slow; slow > 0 {
		time.Sleep(slow)
	}
	c := v.chain
	c.mu.Lock()
	defer c.mu.Unlock()
	f := v.faults
	if f.Capacity {
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
		return false, nil
	case "eth_blockNumber":
		return hexU(v.tip.Number), nil
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
			if f.RefuseLatest {
				return nil, &rpcError{-32000, "latest block unavailable"}
			}
			b = v.tip
		case "finalized", "safe":
			n := c.canon[0].Number
			if v.tip.Number > FinalizedLag && v.tip.Number-FinalizedLag > n {
				n = v.tip.Number - FinalizedLag
			}
			b = v.byNumberLocked(n)
		case "earliest":
			b = c.canon[0]
		default:
			n, ok := parseNumber(tag)
			if !ok {
				return nil, &rpcError{-32602, "invalid block number"}
			}
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
		if f.NoReceipts {
			return nil, &rpcError{-32601, "the method eth_getBlockReceipts does not exist/is not available"}
		}
		hs, _ := firstString(params)
		h, ok := parseHash(hs)
		if !ok {
			return nil, &rpcError{-32602, "invalid block hash"}
		}
		b := c.byHash[h]
		if b == nil || !v.seen[h] || f.NullHeld {
			return nil, nil
		}
		drop := 0
		if f.TruncReceipts {
			drop = 10
		}
		return b.receipts(f.WrongBloom, drop, f.ForgedLog), nil
	case "eth_getLogs":
		v.receiptsCalls.Add(1)
		return v.logsLocked(params)
	}
	return "0x0", nil
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
		for _, l := range b.Logs() {
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
