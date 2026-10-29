package realworld

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/test/chainsim"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type hash = chainsim.Hash

// transcript is one client's event log in the run dir.
type transcript struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

func newTranscript(dir, name string) *transcript {
	f, err := os.Create(filepath.Join(dir, name+".log"))
	if err != nil {
		return &transcript{}
	}
	return &transcript{f: f, w: bufio.NewWriter(f)}
}

func (t *transcript) logf(format string, args ...interface{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.w == nil {
		return
	}
	fmt.Fprintf(t.w, "%s ", time.Now().Format("15:04:05.000"))
	fmt.Fprintf(t.w, format, args...)
	t.w.WriteByte('\n')
}

func (t *transcript) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.w != nil {
		_ = t.w.Flush()
		_ = t.f.Close()
		t.w = nil
	}
}

// delivery is one block a client received, for latency and stall checks.
type delivery struct {
	pod       int
	number    uint64
	minted    time.Time
	at        time.Time
	connStart time.Time
	hash      hash
}

// clientBase is what every client driver shares: its pod, its deliveries,
// its transcript and its pause switch.
type clientBase struct {
	s    *scenario
	name string
	// slow clients stop reading on purpose; delivery bounds skip them.
	slow bool
	tr   *transcript
	// selector is the client's use-upstream lane ("" the network-wide
	// one): its connection and its oracle checks carry it.
	selector string
	// pinned clients redial their own pod, never another.
	pinned bool

	mu          sync.Mutex
	pod         int
	deliveries  []delivery
	ends        []clientEnd
	pausedUntil time.Time
	reconnect   chan struct{}
	// connAt is when the open connection opened; zero while none is open.
	connAt time.Time
	// allow holds the ends and jumps this client may see beyond its
	// scenario's.
	allow map[string]bool
}

type clientEnd struct {
	at     time.Time
	pod    int
	reason string
	server bool // the server or the network ended it, not the client
}

func (c *clientBase) currentPod() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pod
}

// connected marks a connection open since at, or closed (zero at).
func (c *clientBase) connected(at time.Time) {
	c.mu.Lock()
	c.connAt = at
	c.mu.Unlock()
}

// silence is the client's pod, how long since its last block or since its
// connection opened, whichever is later, and that block's number (0 none);
// ok is false while no connection is open.
func (c *clientBase) silence() (pod int, since time.Duration, last uint64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connAt.IsZero() {
		return 0, 0, 0, false
	}
	at := c.connAt
	if n := len(c.deliveries); n > 0 {
		last = c.deliveries[n-1].number
		if c.deliveries[n-1].at.After(at) {
			at = c.deliveries[n-1].at
		}
	}
	return c.pod, time.Since(at), last, true
}

func (c *clientBase) otherPod() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.pinned {
		c.pod = (c.pod + 1) % len(c.s.h.pods)
	}
	return c.pod
}

// pause stops the client reading its socket for d.
func (c *clientBase) pause(d time.Duration) {
	c.mu.Lock()
	c.pausedUntil = time.Now().Add(d)
	c.mu.Unlock()
	c.tr.logf("pause %s", d)
}

func (c *clientBase) waitPause(ctx context.Context) {
	for {
		c.mu.Lock()
		until := c.pausedUntil
		c.mu.Unlock()
		d := time.Until(until)
		if d <= 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
}

// allows reports whether the client may see what key names on pod: "ws:jump",
// "ws:heads-end", "ws:logs-end", "grpc:Unavailable", "grpc:OutOfRange"; a
// scenario may allow a key on one pod only, as "key@pod-N".
func (c *clientBase) allows(key string, pod int) bool {
	return c.allow[key] || c.s.allowed(key) || c.s.allowed(fmt.Sprintf("%s@pod-%d", key, pod+1))
}

func (c *clientBase) deliver(pod int, b *chainsim.Block, connStart time.Time) {
	c.mu.Lock()
	c.deliveries = append(c.deliveries, delivery{pod: pod, number: b.Number, minted: b.MintedAt, at: time.Now(), connStart: connStart, hash: b.Hash})
	c.mu.Unlock()
}

func (c *clientBase) ended(pod int, reason string, server bool) {
	c.mu.Lock()
	c.ends = append(c.ends, clientEnd{at: time.Now(), pod: pod, reason: reason, server: server})
	c.mu.Unlock()
	c.tr.logf("end pod=%d server=%v %s", pod+1, server, reason)
}

// headerChain applies headers as a client keeps them: a number at or below
// the last replaces it and every number above.
type headerChain struct {
	m        map[uint64]hash
	last     uint64
	segStart uint64
	started  bool
}

type applied string

const (
	appliedStart     applied = "start"
	appliedNext      applied = "next"
	appliedReplace   applied = "replace"
	appliedJump      applied = "jump"
	appliedDuplicate applied = "duplicate"
	appliedUnlinked  applied = "unlinked"
)

// apply classifies header n/h/parent against the chain and applies it,
// except a duplicate. A jump or an unlinked header starts a new segment.
func (hc *headerChain) apply(n uint64, h, parent hash) applied {
	if hc.m == nil {
		hc.m = make(map[uint64]hash)
	}
	switch {
	case !hc.started:
		hc.restart(n, h)
		return appliedStart
	case n == hc.last+1 && hc.m[hc.last] == parent:
		hc.m[n], hc.last = h, n
		return appliedNext
	case n <= hc.last && hc.m[n] == h:
		return appliedDuplicate
	case n <= hc.last && (n <= hc.segStart || hc.m[n-1] == parent):
		for k := n; k <= hc.last; k++ {
			delete(hc.m, k)
		}
		hc.m[n], hc.last = h, n
		if n < hc.segStart {
			hc.segStart = n
		}
		return appliedReplace
	case n > hc.last+1:
		hc.restart(n, h)
		return appliedJump
	}
	hc.restart(n, h)
	return appliedUnlinked
}

func (hc *headerChain) restart(n uint64, h hash) {
	for k := range hc.m {
		if k >= n {
			delete(hc.m, k)
		}
	}
	hc.m[n], hc.last, hc.segStart, hc.started = h, n, n, true
}

type headJSON struct {
	Number     hexutil.Uint64 `json:"number"`
	Hash       hash           `json:"hash"`
	ParentHash hash           `json:"parentHash"`
}

//
// --- ws newHeads ---
//

// headsClient is a ws newHeads subscriber: geth's rpc.Client, or, for a
// slow client that must stop reading its socket, a raw WebSocket with a
// small receive buffer.
type headsClient struct {
	clientBase

	hc           headerChain
	replacements []replacement
	jumps        []jump
	pending      []pendingNumber
}

// replacement is a reorg the client was told about: a header at or below
// its last number.
type replacement struct {
	number uint64
	at     time.Time
}

// jump is a header past the client's last + 1 on one connection, or
// (newConn) a new subscription's first header, wherever it lands.
type jump struct {
	at       time.Time
	from, to uint64
	newConn  bool
}

// pendingNumber is a by-number answer that did not match a delivered
// header; a later reorg at or below its number resolves it.
type pendingNumber struct {
	target    int
	number    uint64
	hash      hash
	got       string
	delivered time.Time
}

func (c *headsClient) run(ctx context.Context) {
	for ctx.Err() == nil {
		pod := c.currentPod()
		start := time.Now()
		msgs, errc, closeFn, err := c.open(ctx, pod)
		if err != nil {
			c.ended(pod, "dial: "+err.Error(), true)
			if !c.allows("ws:heads-end", pod) {
				c.s.violate(c.name, "pod-%d: newHeads subscribe failed: %v", pod+1, err)
			}
			c.otherPod()
			sleepCtx(ctx, clientRedial)
			continue
		}
		c.tr.logf("connected pod=%d", pod+1)
		c.connected(start)
		first := true
		server := false
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case <-c.reconnect:
				c.ended(pod, "client reconnect", false)
				break loop
			case err := <-errc:
				c.ended(pod, errString(err), true)
				if !c.allows("ws:heads-end", pod) {
					c.s.violate(c.name, "pod-%d: newHeads subscription ended: %s", pod+1, errString(err))
				}
				server = true
				break loop
			case raw := <-msgs:
				c.onHead(pod, raw, start, first)
				first = false
			}
		}
		closeFn()
		c.connected(time.Time{})
		if server {
			c.otherPod()
			sleepCtx(ctx, clientRedial)
		}
	}
}

func (c *headsClient) open(ctx context.Context, pod int) (<-chan json.RawMessage, <-chan error, func(), error) {
	url := c.s.h.pods[pod].wsURL(c.s.project, c.s.chain.ID())
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !c.slow {
		var opts []rpc.ClientOption
		if c.selector != "" {
			opts = append(opts, rpc.WithHeader("X-ERPC-Use-Upstream", c.selector))
		}
		cl, err := rpc.DialOptions(dctx, url, opts...)
		if err != nil {
			return nil, nil, nil, err
		}
		ch := make(chan json.RawMessage, 1024)
		sub, err := cl.EthSubscribe(dctx, ch, "newHeads")
		if err != nil {
			cl.Close()
			return nil, nil, nil, err
		}
		return ch, sub.Err(), func() { sub.Unsubscribe(); cl.Close() }, nil
	}
	d := websocket.Dialer{NetDialContext: smallRcvBuf.DialContext, HandshakeTimeout: 10 * time.Second}
	conn, _, err := d.DialContext(dctx, url, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "eth_subscribe", "params": []string{"newHeads"}}); err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	var ack struct {
		Result string          `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := conn.ReadJSON(&ack); err != nil || ack.Result == "" {
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("subscribe: %v %s", err, ack.Error)
	}
	msgs := make(chan json.RawMessage)
	errc := make(chan error, 1)
	rctx, rcancel := context.WithCancel(ctx)
	go func() {
		for {
			c.waitPause(rctx)
			var note struct {
				Params struct {
					Result json.RawMessage `json:"result"`
				} `json:"params"`
			}
			if err := conn.ReadJSON(&note); err != nil {
				errc <- err
				return
			}
			select {
			case msgs <- note.Params.Result:
			case <-rctx.Done():
				return
			}
		}
	}()
	return msgs, errc, func() { rcancel(); _ = conn.Close() }, nil
}

// smallRcvBuf dials with a 4 KiB receive buffer, so a client that stops
// reading backs up into eRPC's socket as soon as the kernel lets it: Linux
// honours the size; macOS autosizes anyway (see slowKernelHeld).
var smallRcvBuf = &net.Dialer{Timeout: 10 * time.Second, Control: func(_, _ string, rc syscall.RawConn) error {
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
	}); err != nil {
		return err
	}
	return serr
}}

func (c *headsClient) onHead(pod int, raw json.RawMessage, connStart time.Time, first bool) {
	var h headJSON
	if err := json.Unmarshal(raw, &h); err != nil {
		c.s.violate(c.name, "pod-%d: newHeads payload does not parse: %v: %.200s", pod+1, err, raw)
		return
	}
	n := uint64(h.Number)
	b := c.s.chain.Block(h.Hash)
	if b == nil {
		c.s.violate(c.name, "pod-%d: newHeads %d %s is no block of the chain", pod+1, n, h.Hash.Hex())
		return
	}
	if b.Number != n || b.ParentHash() != h.ParentHash {
		c.s.violate(c.name, "pod-%d: newHeads %d %s carries number/parentHash of another block", pod+1, n, h.Hash.Hex())
	}
	c.mu.Lock()
	last := c.hc.last
	kind := c.hc.apply(n, h.Hash, h.ParentHash)
	now := time.Now()
	if kind == appliedReplace {
		c.replacements = append(c.replacements, replacement{number: n, at: now})
	}
	switch {
	case first && kind != appliedStart:
		// Every new subscription's first header, linked or not: it starts
		// after the pod's head, so no link to the old one is owed.
		c.jumps = append(c.jumps, jump{at: now, from: last, to: n, newConn: true})
		if kind == appliedUnlinked {
			kind = appliedJump
		}
	case kind == appliedJump:
		c.jumps = append(c.jumps, jump{at: now, from: last, to: n})
	}
	c.mu.Unlock()
	c.tr.logf("head pod=%d n=%d h=%s p=%s %s", pod+1, n, short(h.Hash), short(h.ParentHash), kind)
	switch {
	case kind == appliedDuplicate:
		c.s.violate(c.name, "pod-%d: newHeads sent %d %s twice", pod+1, n, short(h.Hash))
		return
	case kind == appliedUnlinked:
		c.s.violate(c.name, "pod-%d: newHeads %d %s does not link: parent %s, client has %d", pod+1, n, short(h.Hash), short(h.ParentHash), last)
	case kind == appliedJump && !first && !c.allows("ws:jump", pod):
		c.s.violate(c.name, "pod-%d: newHeads jumped from %d to %d on one connection", pod+1, last, n)
	}
	c.deliver(pod, b, connStart)
	c.s.checkHead(c, pod, n, h.Hash, now)
	if c.s.onHead != nil {
		c.s.onHead(c.s, c, pod, n, h.Hash)
	}
}

// mismatch records a by-number answer that differs from a delivered header.
func (c *headsClient) mismatch(p pendingNumber) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, p)
}

// unresolved are the mismatches no later reorg at or below their number
// explains.
func (c *headsClient) unresolved() []pendingNumber {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []pendingNumber
	for _, p := range c.pending {
		explained := false
		for _, r := range c.replacements {
			if r.number <= p.number && r.at.After(p.delivered) {
				explained = true
				break
			}
		}
		if !explained {
			out = append(out, p)
		}
	}
	return out
}

//
// --- ws logs ---
//

type logsClient struct {
	clientBase
	filter int

	live  map[chainsim.LogKey]liveLog
	ever  map[chainsim.LogKey]bool
	conns []*logsConn
}

type liveLog struct {
	number uint64
	conn   int
}

// logsConn is one subscription's covered range, from the tap's first block
// to the last block it surely received in full. A tap opened while its pod
// has a confirmed head starts after it, so at or below the first block
// minted after the subscribe; one opened before the pod's first confirmed
// head starts at that head (spec §4 Head, §15.1 cold start), which the
// client cannot see: its range starts at its first log's block (from 0
// until then).
type logsConn struct {
	from, to uint64
	highest  uint64
	open     bool
	opened   time.Time
	closed   time.Time
}

type logJSON struct {
	Address     gethcommon.Address `json:"address"`
	Topics      []hash             `json:"topics"`
	Data        string             `json:"data"`
	BlockHash   hash               `json:"blockHash"`
	BlockNumber hexutil.Uint64     `json:"blockNumber"`
	LogIndex    hexutil.Uint       `json:"logIndex"`
	TxHash      hash               `json:"transactionHash"`
	Removed     bool               `json:"removed"`
}

func (c *logsClient) run(ctx context.Context) {
	for ctx.Err() == nil {
		pod := c.currentPod()
		warm := c.s.confirmedHeadSeen(pod)
		url := c.s.h.pods[pod].wsURL(c.s.project, c.s.chain.ID())
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cl, err := rpc.DialOptions(dctx, url)
		var sub *rpc.ClientSubscription
		ch := make(chan json.RawMessage, 4096)
		if err == nil {
			sub, err = cl.EthSubscribe(dctx, ch, "logs", chainsim.Filters[c.filter].Params())
			if err != nil {
				cl.Close()
			}
		}
		cancel()
		if err != nil {
			c.ended(pod, "subscribe: "+err.Error(), true)
			if !c.allows("ws:logs-end", pod) {
				c.s.violate(c.name, "pod-%d: logs subscribe failed: %v", pod+1, err)
			}
			c.otherPod()
			sleepCtx(ctx, clientRedial)
			continue
		}
		c.mu.Lock()
		conn := len(c.conns)
		lc := &logsConn{open: true, opened: time.Now()}
		if warm {
			lc.from = c.s.chain.Tip().Number + 1
		}
		c.conns = append(c.conns, lc)
		c.mu.Unlock()
		c.tr.logf("subscribed pod=%d conn=%d from=%d", pod+1, conn, lc.from)
		server := false
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case <-c.reconnect:
				c.ended(pod, "client reconnect", false)
				break loop
			case err := <-sub.Err():
				c.ended(pod, errString(err), true)
				if !c.allows("ws:logs-end", pod) {
					c.s.violate(c.name, "pod-%d: logs subscription ended: %s", pod+1, errString(err))
				}
				server = true
				break loop
			case raw := <-ch:
				c.onLog(pod, conn, raw)
			}
		}
		sub.Unsubscribe()
		cl.Close()
		c.closeConn(conn, server, ctx.Err() != nil)
		if server {
			c.otherPod()
			sleepCtx(ctx, clientRedial)
		}
	}
}

// closeConn ends a subscription's covered range. At the scenario's end it
// covers up to its lane's final head (finalHead); a client-side close covers up to the block
// before its last log. A server-side end (1013, a dead pod) tells the client
// nothing about how deep its logs went stale, so it covers nothing and its
// logs leave the live set: the client resubscribes and starts over. A range
// whose start the client never learned covers nothing.
func (c *logsClient) closeConn(conn int, server, final bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	lc := c.conns[conn]
	lc.open = false
	lc.closed = time.Now()
	switch {
	case final && lc.from > 0:
		lc.to = c.s.finalHead(c.selector).Number
	case server:
		lc.to = 0
		for k, l := range c.live {
			if l.conn == conn {
				delete(c.live, k)
			}
		}
	default:
		lc.to = 0
		if lc.highest > 0 {
			lc.to = lc.highest - 1
		}
	}
}

func (c *logsClient) onLog(pod, conn int, raw json.RawMessage) {
	var l logJSON
	if err := json.Unmarshal(raw, &l); err != nil {
		c.s.violate(c.name, "pod-%d: logs payload does not parse: %v: %.200s", pod+1, err, raw)
		return
	}
	key := chainsim.LogKey{BlockHash: l.BlockHash, LogIndex: int(l.LogIndex)}
	c.tr.logf("log pod=%d conn=%d n=%d h=%s i=%d removed=%v", pod+1, conn, l.BlockNumber, short(l.BlockHash), l.LogIndex, l.Removed)
	b := c.s.chain.Block(l.BlockHash)
	if b == nil {
		c.s.violate(c.name, "pod-%d: log of %s, no block of the chain", pod+1, short(l.BlockHash))
		return
	}
	logs := b.Logs()
	if int(l.LogIndex) >= len(logs) {
		c.s.violate(c.name, "pod-%d: log %d of block %d, which holds %d", pod+1, l.LogIndex, b.Number, len(logs))
		return
	}
	want := logs[l.LogIndex]
	if l.Data != "0x" || l.Address != want.Address || !slicesEqual(l.Topics, want.Topics) || l.TxHash != want.TxHash || uint64(l.BlockNumber) != b.Number {
		c.s.violate(c.name, "pod-%d: log %d of block %d differs from the chain's: %.300s", pod+1, l.LogIndex, b.Number, raw)
	}
	if !chainsim.Filters[c.filter].Matches(want) {
		c.s.violate(c.name, "pod-%d: log %d of block %d does not match the filter", pod+1, l.LogIndex, b.Number)
	}
	c.mu.Lock()
	if l.Removed {
		if !c.ever[key] {
			c.mu.Unlock()
			c.s.violate(c.name, "pod-%d: removed:true for log %d of block %d it never received", pod+1, l.LogIndex, b.Number)
			return
		}
		delete(c.live, key)
		c.mu.Unlock()
		return
	}
	c.live[key] = liveLog{number: b.Number, conn: conn}
	c.ever[key] = true
	lc := c.conns[conn]
	if lc.from == 0 {
		lc.from = b.Number
	}
	if b.Number > lc.highest {
		lc.highest = b.Number
	}
	start := lc.opened
	c.mu.Unlock()
	c.deliver(pod, b, start)
	c.s.checkLog(c, pod, want)
}

//
// --- gRPC StreamBlocks ---
//

// grpcClient applies StreamBlocks headers by the gRPC client rule (§9.2)
// and resumes from its last header, on the other pod, every few blocks and
// whenever a stream ends UNAVAILABLE. An UNAVAILABLE end at the server's
// connection age is the resume a real client makes (§10.2); any other is
// a violation unless the scenario allows it.
type grpcClient struct {
	clientBase
	// switchEvery: resume on the other pod after this many blocks, at
	// random up to twice it; 0 never.
	switchEvery int

	hc      headerChain
	replace []replacement
	resumes int
}

func (g *grpcClient) run(ctx context.Context) {
	for ctx.Err() == nil {
		pod := g.currentPod()
		code, msg, lived, planned := g.stream(ctx, pod)
		switch {
		case ctx.Err() != nil:
			return
		case planned:
			g.ended(pod, "client resumes on the other pod", false)
			g.otherPod()
		case code == codes.Unavailable:
			g.ended(pod, "UNAVAILABLE: "+msg, true)
			if lived < grpcConnAge && !g.allows("grpc:Unavailable", pod) {
				g.s.violate(g.name, "pod-%d: StreamBlocks ended UNAVAILABLE: %s", pod+1, msg)
			}
			g.otherPod()
			sleepCtx(ctx, clientRedial)
		case code == codes.OutOfRange:
			g.ended(pod, "OUT_OF_RANGE: "+msg, true)
			if !g.allows("grpc:OutOfRange", pod) {
				g.s.violate(g.name, "pod-%d: StreamBlocks ended OUT_OF_RANGE: %s", pod+1, msg)
			}
			g.mu.Lock()
			g.hc.started = false
			g.mu.Unlock()
		default:
			g.ended(pod, fmt.Sprintf("%s: %s", code, msg), true)
			g.s.violate(g.name, "pod-%d: StreamBlocks ended %s: %s", pod+1, code, msg)
			g.otherPod()
			sleepCtx(ctx, clientRedial)
		}
	}
}

// stream runs one StreamBlocks call on pod; lived is how long the stream
// was open, and planned is true when the client ended it to resume on the
// other pod.
func (g *grpcClient) stream(ctx context.Context, pod int) (codes.Code, string, time.Duration, bool) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if g.slow {
		// A fixed 64 KiB window: a client that stops reading stops the
		// server's sends within a few headers.
		opts = append(opts, grpc.WithInitialWindowSize(1<<16), grpc.WithInitialConnWindowSize(1<<16))
	}
	conn, err := grpc.NewClient(g.s.h.pods[pod].grpcAddr(), opts...)
	if err != nil {
		return codes.Unavailable, err.Error(), 0, false
	}
	defer conn.Close()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sctx = metadata.NewOutgoingContext(sctx, metadata.Pairs("x-erpc-project", g.s.project, "x-erpc-chain-id", fmt.Sprint(g.s.chain.ID())))
	req := &bdsevm.StreamBlocksRequest{}
	g.mu.Lock()
	if g.hc.started {
		n, h := g.hc.last, g.hc.m[g.hc.last]
		req.FromBlock, req.FromHash = &n, h.Bytes()
		g.resumes++
	}
	g.mu.Unlock()
	stream, err := bdsevm.NewStreamServiceClient(conn).StreamBlocks(sctx, req)
	if err != nil {
		st, _ := status.FromError(err)
		return st.Code(), st.Message(), 0, false
	}
	if req.FromBlock != nil {
		g.tr.logf("resume pod=%d from=%d %s", pod+1, *req.FromBlock, short(gethcommon.BytesToHash(req.FromHash)))
	} else {
		g.tr.logf("open pod=%d", pod+1)
	}
	start := time.Now()
	g.connected(start)
	defer g.connected(time.Time{})
	limit := 0
	if g.switchEvery > 0 {
		limit = g.switchEvery + rand.IntN(g.switchEvery+1)
	}
	got := 0
	for {
		g.waitPause(sctx)
		resp, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return codes.Canceled, "", time.Since(start), false
			}
			st, _ := status.FromError(err)
			return st.Code(), st.Message(), time.Since(start), false
		}
		g.onHeader(pod, resp.GetHeader(), start)
		got++
		if limit > 0 && got >= limit {
			return codes.Canceled, "", time.Since(start), true
		}
	}
}

func (g *grpcClient) onHeader(pod int, h *bdsevm.BlockHeader, connStart time.Time) {
	if h == nil {
		g.s.violate(g.name, "pod-%d: StreamBlocks message without a header", pod+1)
		return
	}
	hh, parent := gethcommon.BytesToHash(h.Hash), gethcommon.BytesToHash(h.ParentHash)
	b := g.s.chain.Block(hh)
	if b == nil {
		g.s.violate(g.name, "pod-%d: StreamBlocks header %d %s is no block of the chain", pod+1, h.Number, short(hh))
		return
	}
	if b.Number != h.Number || b.ParentHash() != parent {
		g.s.violate(g.name, "pod-%d: StreamBlocks header %d %s carries number/parentHash of another block", pod+1, h.Number, short(hh))
	}
	g.mu.Lock()
	last := g.hc.last
	kind := g.hc.apply(h.Number, hh, parent)
	if kind == appliedReplace {
		g.replace = append(g.replace, replacement{number: h.Number, at: time.Now()})
	}
	g.mu.Unlock()
	g.tr.logf("header pod=%d n=%d h=%s p=%s %s", pod+1, h.Number, short(hh), short(parent), kind)
	switch kind {
	case appliedDuplicate:
		g.s.violate(g.name, "pod-%d: StreamBlocks sent %d %s again", pod+1, h.Number, short(hh))
		return
	case appliedJump:
		g.s.violate(g.name, "pod-%d: StreamBlocks skipped from %d to %d", pod+1, last, h.Number)
	case appliedUnlinked:
		g.s.violate(g.name, "pod-%d: StreamBlocks header %d %s does not link to the client's %d", pod+1, h.Number, short(hh), h.Number-1)
	}
	g.deliver(pod, b, connStart)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func errString(err error) string {
	if err == nil {
		return "closed"
	}
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return fmt.Sprintf("close %d %s", ce.Code, ce.Text)
	}
	return err.Error()
}

func short(h hash) string { return h.Hex()[:10] }

func slicesEqual(a, b []hash) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// closedWith reports whether an end reason is a WebSocket close with code.
func closedWith(reason string, code int) bool {
	return strings.Contains(reason, fmt.Sprintf("close %d", code))
}
