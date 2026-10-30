package realworld

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/gorilla/websocket"
)

// Bounds of the stream scenarios (specs/streams/feature.md §3, §5; §14
// W1–W9).
const (
	// extSwitchBlocks: extended logs clients resume on the other pod every
	// 15–30 blocks, as the gRPC ones do (§14: each resuming on the other
	// instance).
	extSwitchBlocks = 15
	// w1CutBlocks: W1's client link is down this many blocks.
	w1CutBlocks = 10
	// w3Heartbeat, w7URLHeartbeat, w7SubHeartbeat: the heartbeat options
	// W3 and W7 set (§3.6: sent only after that many idle seconds).
	w3Heartbeat    = 5 * time.Second
	w7URLHeartbeat = 10 * time.Second
	w7SubHeartbeat = 20 * time.Second
	// w7Halt: W7's quiet chain, long enough for 3 beats of the slower one.
	w7Halt = 3*w7SubHeartbeat + 5*time.Second
	// beatSlack: harness, not spec: a heartbeat timer firing late under
	// -race, and the frame's way to the client.
	beatSlack = time.Second
	// w9Degraded: status degraded within W + 1s of the cut (§14 W9): the
	// last hash vote expires W after the cut (§3.4 degraded).
	w9Degraded = liveBound + time.Second + tapProcessingSlack
	// w4MatchEvery: W4's filter matches one block in this many.
	w4MatchEvery = 10
)

// extLogsClient is an extended ws logs subscriber (§5): a raw WebSocket
// that subscribes with {"erpc": {"blockEnd": true, "from": token}}, keeps
// its cursor at the last blockEnd, and resumes from it, on the other pod
// after any end and every few blocks. Its logs are applied per block, at
// the block's blockEnd; logs of a block it has not ended are dropped with
// the connection and come again after the resume.
type extLogsClient struct {
	clientBase
	filter      int
	switchEvery int
	// fromStart: the first subscribe also carries from, the pod's latest
	// block, so every subscription is resumable (§3.4 stalled).
	fromStart bool
	// link: the client reaches each pod through it; nil dials the pods.
	link *clientLink

	hc      headerChain // blockEnd blocks
	live    map[chainsim.LogKey]liveLog
	ever    map[chainsim.LogKey]bool
	removed map[chainsim.LogKey]bool
	// covFrom: the first block the client covers, 0 until known.
	covFrom    uint64
	resumes    int
	blockEnds  int
	emptyEnds  int
	statuses   []extControl
	heartbeats []extControl
}

// extControl is one erpc_subscription message the client received.
type extControl struct {
	at  time.Time
	pod int
	msg extResult
}

// extResult is an erpc_subscription result (§5.2).
type extResult struct {
	Type   string  `json:"type"`
	Block  *extRef `json:"block"`
	Logs   int     `json:"logs"`
	Head   *extRef `json:"head"`
	Final  string  `json:"finalized"`
	State  string  `json:"state"`
	Reason string  `json:"reason"`
	Detail string  `json:"detail"`
	Oldest *extRef `json:"oldest"`
}

type extRef struct {
	Number     hexutil.Uint64 `json:"number"`
	Hash       hash           `json:"hash"`
	ParentHash hash           `json:"parentHash"`
}

// extFrame is any frame on an extended connection.
type extFrame struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Method string `json:"method"`
	Params struct {
		Subscription string          `json:"subscription"`
		Result       json.RawMessage `json:"result"`
	} `json:"params"`
}

func (s *scenario) addExtLogs(name string, pod, filter, switchEvery int, fromStart bool, link *clientLink, allow ...string) *extLogsClient {
	c := &extLogsClient{filter: filter, switchEvery: switchEvery, fromStart: fromStart, link: link,
		live: map[chainsim.LogKey]liveLog{}, ever: map[chainsim.LogKey]bool{}, removed: map[chainsim.LogKey]bool{}}
	s.initBase(&c.clientBase, name, pod, false, allow)
	s.mu.Lock()
	s.exts = append(s.exts, c)
	s.mu.Unlock()
	s.wg.Add(1)
	go func() { defer s.wg.Done(); c.run(s.ctx) }()
	return c
}

// extClients are baseline B's clients plus 2 extended ws logs clients per
// pod (one per filter), each resuming on the other pod every switchEvery
// blocks (§14).
func extClients(switchEvery int, fromStart bool) func(s *scenario) {
	return func(s *scenario) {
		s.baseClients()
		for _, p := range s.h.pods {
			for i := range 2 {
				s.addExtLogs(fmt.Sprintf("p%d-ext-%d", p.idx+1, i+1), p.idx, i, switchEvery, fromStart, nil)
			}
		}
	}
}

// token is a cursor as §3.5 writes it.
func token(n uint64, h hash) string { return hexutil.EncodeUint64(n) + ":" + h.Hex() }

func (c *extLogsClient) url(pod int) string {
	u := c.s.h.pods[pod].wsURL(c.s.project, c.s.chain.ID())
	if c.link != nil {
		return c.link.wsURL(pod, c.s.project, c.s.chain.ID())
	}
	return u
}

func (c *extLogsClient) run(ctx context.Context) {
	for ctx.Err() == nil {
		pod := c.currentPod()
		ended, server := c.connect(ctx, pod)
		switch {
		case ctx.Err() != nil:
			return
		case ended == "dial":
			sleepCtx(ctx, clientRedial)
		case server:
			c.otherPod()
			if !strings.HasPrefix(ended, "end ") {
				sleepCtx(ctx, clientRedial)
			}
		default:
			c.otherPod()
		}
	}
}

// connect runs one subscription on pod until it ends: ended names how,
// server tells whether the server or the network ended it.
func (c *extLogsClient) connect(ctx context.Context, pod int) (ended string, server bool) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.DefaultDialer.DialContext(dctx, c.url(pod), nil)
	cancel()
	if err != nil {
		if c.link == nil || !c.link.isDown() {
			c.ended(pod, "dial: "+err.Error(), true)
			if !c.allows("ws:logs-end", pod) {
				c.s.violate(c.name, "pod-%d: dial: %v", pod+1, err)
			}
		}
		return "dial", true
	}
	defer conn.Close()
	opts := map[string]interface{}{"blockEnd": true}
	c.mu.Lock()
	resumed := c.hc.started
	if resumed {
		opts["from"] = token(c.hc.last, c.hc.m[c.hc.last])
		c.resumes++
	}
	c.mu.Unlock()
	if !resumed && c.fromStart {
		var head headJSON
		if err := c.s.call(pod, &head, "eth_getBlockByNumber", "latest", false); err != nil {
			c.ended(pod, "latest: "+err.Error(), true)
			return "dial", true
		}
		opts["from"] = token(uint64(head.Number), head.Hash)
		c.mu.Lock()
		c.hc.restart(uint64(head.Number), head.Hash)
		if c.covFrom == 0 {
			c.covFrom = uint64(head.Number) + 1
		}
		c.mu.Unlock()
	}
	warm := c.s.confirmedHeadSeen(pod)
	if err := conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "eth_subscribe",
		"params": []interface{}{"logs", chainsim.Filters[c.filter].Params(), map[string]interface{}{"erpc": opts}}}); err != nil {
		return "write: " + err.Error(), true
	}
	_ = conn.SetReadDeadline(time.Now().Add(d3Reply))
	var ack extFrame
	if err := conn.ReadJSON(&ack); err != nil || ack.Error != nil || len(ack.Result) == 0 {
		c.ended(pod, fmt.Sprintf("subscribe: %v %+v", err, ack.Error), true)
		if !c.allows("ws:logs-end", pod) {
			c.s.violate(c.name, "pod-%d: extended logs subscribe failed: %v %+v", pod+1, err, ack.Error)
		}
		return "subscribe", true
	}
	_ = conn.SetReadDeadline(time.Time{})
	start := time.Now()
	c.mu.Lock()
	if c.covFrom == 0 && warm {
		c.covFrom = c.s.chain.Tip().Number + 1
	}
	c.mu.Unlock()
	c.tr.logf("subscribed pod=%d from=%v", pod+1, opts["from"])
	c.connected(start)
	defer c.connected(time.Time{})

	frames := make(chan extFrame, 4096)
	errc := make(chan error, 1)
	go func() {
		for {
			var f extFrame
			if err := conn.ReadJSON(&f); err != nil {
				errc <- err
				return
			}
			frames <- f
		}
	}()
	limit := 0
	if c.switchEvery > 0 {
		limit = c.switchEvery + rand.IntN(c.switchEvery+1)
	}
	ends := 0
	pend := map[hash][]chainsim.LogKey{}
	for {
		select {
		case <-ctx.Done():
			return "ctx", false
		case <-c.reconnect:
			c.ended(pod, "client reconnect", false)
			return "client", false
		case err := <-errc:
			reason := errString(err)
			c.ended(pod, reason, true)
			if (c.link == nil || !c.link.isDown()) && !c.allows("ws:logs-end", pod) {
				c.s.violate(c.name, "pod-%d: extended logs connection ended: %s", pod+1, reason)
			}
			return reason, true
		case f := <-frames:
			switch f.Method {
			case "eth_subscription":
				c.onLog(pod, f.Params.Result, pend)
			case "erpc_subscription":
				var r extResult
				if err := json.Unmarshal(f.Params.Result, &r); err != nil {
					c.s.violate(c.name, "pod-%d: erpc_subscription does not parse: %v: %.200s", pod+1, err, f.Params.Result)
					continue
				}
				if done := c.onControl(pod, r, pend, start); done != "" {
					return done, true
				}
				if r.Type == "blockEnd" {
					ends++
					if limit > 0 && ends >= limit {
						c.ended(pod, "client resumes on the other pod", false)
						return "switch", false
					}
				}
			}
		}
	}
}

// onControl applies an erpc_subscription message; an end returns how the
// subscription ended.
func (c *extLogsClient) onControl(pod int, r extResult, pend map[hash][]chainsim.LogKey, connStart time.Time) string {
	now := time.Now()
	switch r.Type {
	case "blockEnd":
		if r.Block == nil {
			c.s.violate(c.name, "pod-%d: blockEnd without a block", pod+1)
			return ""
		}
		n, h, p := uint64(r.Block.Number), r.Block.Hash, r.Block.ParentHash
		b := c.s.chain.Block(h)
		if b == nil || b.Number != n || b.ParentHash() != p {
			c.s.violate(c.name, "pod-%d: blockEnd %d %s is no block of the chain, or carries another's number or parent", pod+1, n, short(h))
			return ""
		}
		got := pend[h]
		if len(got) != r.Logs {
			c.s.violate(c.name, "pod-%d: blockEnd %d %s says %d logs, the client got %d", pod+1, n, short(h), r.Logs, len(got))
		}
		delete(pend, h)
		c.mu.Lock()
		last := c.hc.last
		kind := c.hc.apply(n, h, p)
		if kind != appliedDuplicate {
			for _, k := range got {
				c.live[k] = liveLog{number: n}
				c.ever[k] = true
			}
		}
		if c.covFrom == 0 {
			c.covFrom = n
		}
		c.blockEnds++
		if r.Logs == 0 {
			c.emptyEnds++
		}
		c.mu.Unlock()
		c.tr.logf("blockEnd pod=%d n=%d h=%s p=%s logs=%d %s", pod+1, n, short(h), short(p), r.Logs, kind)
		switch kind {
		case appliedDuplicate:
			c.s.violate(c.name, "pod-%d: blockEnd %d %s twice", pod+1, n, short(h))
		case appliedJump:
			c.s.violate(c.name, "pod-%d: blockEnd skipped from %d to %d", pod+1, last, n)
		case appliedUnlinked:
			c.s.violate(c.name, "pod-%d: blockEnd %d %s does not link to the client's %d", pod+1, n, short(h), last)
		}
		c.deliver(pod, b, connStart)
	case "heartbeat":
		c.mu.Lock()
		c.heartbeats = append(c.heartbeats, extControl{now, pod, r})
		c.mu.Unlock()
	case "status":
		c.mu.Lock()
		c.statuses = append(c.statuses, extControl{now, pod, r})
		c.mu.Unlock()
		c.tr.logf("status pod=%d %s %s", pod+1, r.State, r.Reason)
	case "end":
		reason := "end " + r.Reason + "/" + r.Detail
		c.ended(pod, reason, true)
		if !c.allows("ws-ext:"+r.Reason, pod) {
			c.s.violate(c.name, "pod-%d: subscription ended %s", pod+1, reason)
		}
		if r.Reason == "out_of_range" {
			c.mu.Lock()
			c.hc.started = false
			c.mu.Unlock()
		}
		return reason
	default:
		c.s.violate(c.name, "pod-%d: erpc_subscription of unknown type %q", pod+1, r.Type)
	}
	return ""
}

// onLog applies a log: removed:true at once, any other at its block's
// blockEnd (pend), as a client applies whole blocks.
func (c *extLogsClient) onLog(pod int, raw json.RawMessage, pend map[hash][]chainsim.LogKey) {
	var l logJSON
	if err := json.Unmarshal(raw, &l); err != nil {
		c.s.violate(c.name, "pod-%d: logs payload does not parse: %v: %.200s", pod+1, err, raw)
		return
	}
	key := chainsim.LogKey{BlockHash: l.BlockHash, LogIndex: int(l.LogIndex)}
	c.tr.logf("log pod=%d n=%d h=%s i=%d removed=%v", pod+1, l.BlockNumber, short(l.BlockHash), l.LogIndex, l.Removed)
	b := c.s.chain.Block(l.BlockHash)
	if b == nil || int(l.LogIndex) >= len(b.Logs()) || !chainsim.Filters[c.filter].Matches(b.Logs()[l.LogIndex]) {
		c.s.violate(c.name, "pod-%d: log %d of %s is no matching log of the chain", pod+1, l.LogIndex, short(l.BlockHash))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if l.Removed {
		if !c.ever[key] {
			c.s.violate(c.name, "pod-%d: removed:true for log %d of block %d it never received", pod+1, l.LogIndex, b.Number)
			return
		}
		c.removed[key] = true
		delete(c.live, key)
		return
	}
	if c.hc.started && b.Number <= c.hc.last && c.hc.m[b.Number] == b.Hash {
		c.s.violate(c.name, "pod-%d: log %d of block %d again, after its blockEnd", pod+1, l.LogIndex, b.Number)
	}
	pend[b.Hash] = append(pend[b.Hash], key)
}

// judgeExt: the client's blockEnd chain is canonical from its start to the
// final head, and its live logs over that range are the canonical logs of
// its filter.
func (s *scenario) judgeExt(c *extLogsClient) {
	final := s.finalHead("")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blockEnds == 0 {
		s.violate(c.name, "received no blockEnd")
	}
	if c.hc.started {
		for n := max(c.hc.segStart, c.covFrom); n <= final.Number; n++ {
			if got, ok := c.hc.m[n]; !ok || got != s.chain.CanonAt(n).Hash {
				s.violate(c.name, "blockEnd chain at %d is %s, canonical %s", n, short(got), short(s.chain.CanonAt(n).Hash))
				break
			}
		}
	}
	if c.covFrom == 0 {
		s.violate(c.name, "covered no block")
		return
	}
	want := s.chain.CanonicalLogs(chainsim.Filters[c.filter], c.covFrom, final.Number)
	missing, extra := 0, 0
	var sample []string
	for k, l := range want {
		if _, ok := c.live[k]; !ok {
			missing++
			if len(sample) < 5 {
				sample = append(sample, fmt.Sprintf("missing %d %s/%d", l.BlockNumber, short(k.BlockHash), k.LogIndex))
			}
		}
	}
	for k, l := range c.live {
		if l.number >= c.covFrom && l.number <= final.Number {
			if _, ok := want[k]; !ok {
				extra++
				if len(sample) < 10 {
					sample = append(sample, fmt.Sprintf("extra %d %s/%d", l.number, short(k.BlockHash), k.LogIndex))
				}
			}
		}
	}
	if missing+extra > 0 {
		s.violate(c.name, "live logs over %d..%d differ from canonical: %d missing, %d not canonical: %s", c.covFrom, final.Number, missing, extra, strings.Join(sample, ", "))
	}
}

//
// --- client link faults ---
//

// clientLink stands between one client and every pod: while down it
// refuses new connections and has closed the open ones, as a client whose
// network dropped.
type clientLink struct {
	h     *harness
	lns   []net.Listener
	down  atomic.Bool
	mu    sync.Mutex
	conns map[net.Conn]bool
}

func (s *scenario) newClientLink() *clientLink {
	cl := &clientLink{h: s.h, conns: map[net.Conn]bool{}}
	for _, p := range s.h.pods {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			s.t.Fatal(err)
		}
		cl.lns = append(cl.lns, ln)
		target := fmt.Sprintf("127.0.0.1:%d", p.httpPort)
		go cl.serve(ln, target)
	}
	s.wg.Add(1)
	go func() { defer s.wg.Done(); <-s.ctx.Done(); cl.close() }()
	return cl
}

func (cl *clientLink) wsURL(pod int, project string, chainID int64) string {
	return fmt.Sprintf("ws://%s/%s/evm/%d", cl.lns[pod].Addr(), project, chainID)
}

func (cl *clientLink) isDown() bool { return cl.down.Load() }

// set takes the link down, closing every open connection, or up.
func (cl *clientLink) set(down bool) {
	cl.down.Store(down)
	if !down {
		return
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for c := range cl.conns {
		_ = c.Close()
	}
}

func (cl *clientLink) close() {
	for _, ln := range cl.lns {
		_ = ln.Close()
	}
	cl.set(true)
}

func (cl *clientLink) serve(ln net.Listener, target string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if cl.down.Load() {
			_ = c.Close()
			continue
		}
		go cl.pipe(c, target)
	}
}

func (cl *clientLink) pipe(c net.Conn, target string) {
	s, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		_ = c.Close()
		return
	}
	cl.mu.Lock()
	cl.conns[c], cl.conns[s] = true, true
	cl.mu.Unlock()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go cp(s, c)
	go cp(c, s)
	<-done
	_ = c.Close()
	_ = s.Close()
	cl.mu.Lock()
	delete(cl.conns, c)
	delete(cl.conns, s)
	cl.mu.Unlock()
}

//
// --- raw extended connections ---
//

// extConn is a raw ws connection that records every frame it reads.
type extConn struct {
	s    *scenario
	conn *websocket.Conn
	next int

	mu     sync.Mutex
	frames []timedFrame
	answer map[int]chan extFrame
	paused atomic.Bool
	resume chan struct{}
	done   chan struct{}
}

type timedFrame struct {
	at time.Time
	f  extFrame
}

// dialExt dials pod's ws URL plus query (with its "?") through dialer.
func (s *scenario) dialExt(pod int, query string, dialer *websocket.Dialer) (*extConn, *http.Response, error) {
	conn, resp, err := dialer.DialContext(s.ctx, s.h.pods[pod].wsURL(s.project, s.chain.ID())+query, nil)
	if err != nil {
		return nil, resp, err
	}
	ec := &extConn{s: s, conn: conn, answer: map[int]chan extFrame{}, resume: make(chan struct{}), done: make(chan struct{})}
	go ec.read()
	return ec, resp, nil
}

func (ec *extConn) read() {
	defer close(ec.done)
	for {
		if ec.paused.Load() {
			<-ec.resume
		}
		var f extFrame
		if err := ec.conn.ReadJSON(&f); err != nil {
			return
		}
		ec.mu.Lock()
		ec.frames = append(ec.frames, timedFrame{time.Now(), f})
		ch := (chan extFrame)(nil)
		if f.ID != nil {
			ch = ec.answer[*f.ID]
		}
		ec.mu.Unlock()
		if ch != nil {
			ch <- f
		}
	}
}

// call sends method and waits for its answer.
func (ec *extConn) call(method string, params ...interface{}) (extFrame, error) {
	ec.mu.Lock()
	ec.next++
	id := ec.next
	ch := make(chan extFrame, 1)
	ec.answer[id] = ch
	ec.mu.Unlock()
	if err := ec.conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return extFrame{}, err
	}
	select {
	case f := <-ch:
		return f, nil
	case <-ec.done:
		return extFrame{}, fmt.Errorf("connection closed before the answer to %s", method)
	case <-time.After(d3Reply):
		return extFrame{}, fmt.Errorf("no answer to %s within %s", method, d3Reply)
	}
}

// subscribe returns the subscription id, or the error.
func (ec *extConn) subscribe(params ...interface{}) (string, error) {
	f, err := ec.call("eth_subscribe", params...)
	if err != nil {
		return "", err
	}
	if f.Error != nil {
		return "", fmt.Errorf("%d %s", f.Error.Code, f.Error.Message)
	}
	var id string
	if err := json.Unmarshal(f.Result, &id); err != nil {
		return "", fmt.Errorf("result %s: %v", f.Result, err)
	}
	return id, nil
}

// pause stops reading the socket until unpause.
func (ec *extConn) pause() { ec.paused.Store(true) }

func (ec *extConn) unpause() {
	ec.paused.Store(false)
	select {
	case ec.resume <- struct{}{}:
	default:
	}
}

// of are the frames of subscription sub with method, from at on.
func (ec *extConn) of(sub, method string, from time.Time) []timedFrame {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	var out []timedFrame
	for _, tf := range ec.frames {
		if tf.f.Params.Subscription == sub && tf.f.Method == method && !tf.at.Before(from) {
			out = append(out, tf)
		}
	}
	return out
}

// controls are sub's erpc_subscription results of type typ from at on.
func (ec *extConn) controls(sub, typ string, from time.Time) []extControl {
	var out []extControl
	for _, tf := range ec.of(sub, "erpc_subscription", from) {
		var r extResult
		if json.Unmarshal(tf.f.Params.Result, &r) == nil && r.Type == typ {
			out = append(out, extControl{at: tf.at, msg: r})
		}
	}
	return out
}

func (ec *extConn) close() { _ = ec.conn.Close() }

// answeredFirst reports whether the subscribe answer carrying sub came
// before every message of sub.
func (ec *extConn) answeredFirst(sub string) bool {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	id := `"` + sub + `"`
	for _, tf := range ec.frames {
		if tf.f.ID != nil && string(tf.f.Result) == id {
			return true
		}
		if tf.f.Params.Subscription == sub {
			return false
		}
	}
	return false
}

// requireBeats: sub's heartbeats in [from, to] come every want, idle
// only, each with the halted chain's head and a finalized number.
func (s *scenario) requireBeats(ec *extConn, name, sub string, want time.Duration, from, to time.Time, tip *chainsim.Block) {
	beats := ec.controls(sub, "heartbeat", from)
	var in []extControl
	for _, b := range beats {
		if !b.at.After(to) {
			in = append(in, b)
		}
	}
	if least := int(to.Sub(from)/want) - 1; len(in) < max(least, 1) {
		s.violate(name, "%d heartbeats in %s, want one every %s", len(in), to.Sub(from).Round(time.Second), want)
		return
	}
	for i, b := range in {
		if i > 0 {
			if gap := b.at.Sub(in[i-1].at); gap < want-tapProcessingSlack || gap > want+beatSlack {
				s.violate(name, "heartbeat %d came %s after the one before, want %s", i, gap.Round(time.Millisecond), want)
			}
		}
		h := b.msg.Head
		switch {
		case h == nil:
			s.violate(name, "heartbeat %d has no head", i)
		case uint64(h.Number)+headSlackBlocks < tip.Number || uint64(h.Number) > tip.Number || s.chain.CanonAt(uint64(h.Number)).Hash != h.Hash:
			s.violate(name, "heartbeat %d head %d %s, not the halted chain's %d", i, h.Number, short(h.Hash), tip.Number)
		case b.msg.Final == "":
			s.violate(name, "heartbeat %d has no finalized", i)
		}
	}
}

//
// --- scenarios ---
//

// W1: an extended logs client with from and blockEnd on pod-1 loses its
// network for w1CutBlocks blocks, then resumes on pod-2 from its cursor:
// no gap, no log twice, a blockEnd for every block (§14 W1; §3.3, §3.5).
func w1() spec {
	return spec{name: "W1", bound: healthyDelivery, clients: extClients(extSwitchBlocks, false), body: func(s *scenario) {
		w1Body(s, 0)
	}}
}

// W2: W1 with a 3-deep reorg during the cut, geth mode: on resume the
// client gets removed:true for exactly the orphan logs it applied, then
// the new branch (§14 W2; §4 removed_unsent).
func w2() spec {
	return spec{name: "W2", bound: healthyDelivery, clients: extClients(extSwitchBlocks, false), body: func(s *scenario) {
		w1Body(s, 3)
	}}
}

func w1Body(s *scenario, reorgDepth int) {
	link := s.newClientLink()
	c := s.addExtLogs("w-cut", 0, 0, 0, true, link)
	s.blocks(25)
	if reorgDepth > 0 {
		kinds := make([]chainsim.Kind, 2*reorgDepth)
		for i := range kinds {
			kinds[i] = chainsim.KindMatching
		}
		s.chain.Plan(kinds...)
		s.blocks(2 * reorgDepth)
	}
	// The client holds the tip, so a reorg reverts blocks it applied.
	caught := time.Now().Add(10 * time.Second)
	for time.Now().Before(caught) {
		c.mu.Lock()
		at := c.hc.last
		c.mu.Unlock()
		if at >= s.chain.Tip().Number {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	link.set(true)
	cut := time.Now()
	s.logf("client link down at %d", s.chain.Tip().Number)
	var r reorg
	if reorgDepth > 0 {
		r = s.reorg(reorgDepth)
	}
	s.blocks(w1CutBlocks)
	link.set(false)
	s.logf("client link up")
	s.blocks(25)
	if !c.deliveredOn(1, cut) {
		s.violate(c.name, "did not resume on pod-2 after the cut")
	}
	c.mu.Lock()
	resumes := c.resumes
	var applied, removed []string
	if reorgDepth > 0 {
		for _, b := range r.old {
			for _, l := range b.Logs() {
				if chainsim.Filters[c.filter].Matches(l) && c.ever[l.Key()] {
					applied = append(applied, fmt.Sprintf("%d/%d", b.Number, l.LogIndex))
					if !c.removed[l.Key()] {
						removed = append(removed, fmt.Sprintf("%d/%d", b.Number, l.LogIndex))
					}
				}
			}
		}
	}
	c.mu.Unlock()
	if resumes == 0 {
		s.violate(c.name, "never resumed from a cursor")
	}
	if reorgDepth > 0 {
		if len(applied) == 0 {
			s.violate("harness", "the client applied no log of the %d orphan blocks before the cut", reorgDepth)
		}
		if len(removed) > 0 {
			s.violate(c.name, "no removed:true for orphan logs it applied: %s", strings.Join(removed, ", "))
		}
	}
}

// W3: no blocks for 3 minutes (CT C6) with an extended and a standard
// newHeads subscription on one connection: the extended one gets a
// heartbeat every w3Heartbeat with the head and finalized, the standard
// one nothing (§14 W3; §3.6).
func w3() spec {
	return spec{name: "W3", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		ec, _, err := s.dialExt(0, "", websocket.DefaultDialer)
		if err != nil {
			s.violate(s.name, "pod-1: ws dial: %v", err)
			return
		}
		defer ec.close()
		ext, err := ec.subscribe("newHeads", map[string]interface{}{"erpc": map[string]interface{}{"heartbeat": int(w3Heartbeat / time.Second)}})
		if err != nil {
			s.violate(s.name, "pod-1: extended newHeads subscribe: %v", err)
			return
		}
		std, err := ec.subscribe("newHeads")
		if err != nil {
			s.violate(s.name, "pod-1: newHeads subscribe: %v", err)
			return
		}
		s.blocks(10)
		s.chain.Halt(true)
		tip := s.chain.Tip()
		at := time.Now()
		s.logf("no blocks for %s from %d", c6Halt, tip.Number)
		time.Sleep(c6Halt)
		end := time.Now()
		s.chain.Halt(false)
		// The last heads reach the client within a live bound of the halt.
		s.requireBeats(ec, s.name+"/extended", ext, w3Heartbeat, at.Add(liveBound), end, tip)
		if n := len(ec.of(std, "erpc_subscription", time.Time{})); n > 0 {
			s.violate(s.name+"/standard", "%d erpc_subscription frames on a standard subscription", n)
		}
		if n := len(ec.of(std, "eth_subscription", at.Add(liveBound))); n > 0 {
			s.violate(s.name+"/standard", "%d frames during the halt", n)
		}
		s.blocks(25)
	}}
}

// W4: a filter matching one block in w4MatchEvery: every extended logs
// client gets a blockEnd for every block, linked by parentHash, most of
// them with no log (§14 W4; §3.2 blockEnd).
func w4() spec {
	return spec{name: "W4", bound: healthyDelivery, clients: extClients(extSwitchBlocks, false), body: func(s *scenario) {
		kinds := make([]chainsim.Kind, 4000)
		for i := range kinds {
			kinds[i] = chainsim.KindQuiet
			if i%w4MatchEvery == 0 {
				kinds[i] = chainsim.KindMatching
			}
		}
		s.chain.Plan(kinds...)
		s.blocks(100)
		for _, c := range s.exts {
			c.mu.Lock()
			ends, empty := c.blockEnds, c.emptyEnds
			c.mu.Unlock()
			if empty < ends/2 {
				s.violate(c.name, "%d of %d blockEnds without a log, with a filter matching 1 block in %d", empty, ends, w4MatchEvery)
			}
		}
	}}
}

// W5: on one connection an extended logs subscription from a cursor below
// the window ends out_of_range/below_window with the oldest held block;
// an extended newHeads subscription whose client stops reading (CT D1)
// ends lagging; the connection still answers eth_blockNumber (§14 W5;
// §3.4).
func w5() spec {
	return spec{name: "W5", bound: healthyDelivery, sim: chainsim.Config{History: d3History, HeaderPad: slowHeaderPad}, body: func(s *scenario) {
		s.blocks(25)
		ec, _, err := s.dialExt(0, "", &websocket.Dialer{NetDialContext: smallRcvBuf.DialContext, HandshakeTimeout: 10 * time.Second})
		if err != nil {
			s.violate(s.name, "pod-1: ws dial: %v", err)
			return
		}
		defer ec.close()
		low := s.chain.CanonAt(s.chain.Tip().Number - uint64(d3History) + 10)
		at := time.Now()
		logs, err := ec.subscribe("logs", chainsim.Filters[0].Params(), map[string]interface{}{"erpc": map[string]interface{}{"from": token(low.Number, low.Hash)}})
		if err != nil {
			s.violate(s.name, "pod-1: extended logs subscribe from %d: %v", low.Number, err)
			return
		}
		time.Sleep(d3Reply)
		ends := ec.controls(logs, "end", at)
		switch {
		case len(ends) != 1:
			s.violate(s.name, "logs from %d below the window: %d end messages, want 1", low.Number, len(ends))
		case ends[0].msg.Reason != "out_of_range" || ends[0].msg.Detail != "below_window":
			s.violate(s.name, "logs from %d below the window ended %s/%s", low.Number, ends[0].msg.Reason, ends[0].msg.Detail)
		case ends[0].msg.Oldest == nil || uint64(ends[0].msg.Oldest.Number) <= low.Number || s.chain.Block(ends[0].msg.Oldest.Hash) == nil:
			s.violate(s.name, "logs end out_of_range with oldest %+v, not a held block above %d", ends[0].msg.Oldest, low.Number)
		}
		if !ec.answeredFirst(logs) {
			s.violate(s.name, "a message of subscription %s came before its id (§5.3)", logs)
		}
		for i := range 2 {
			if f, err := ec.call("eth_unsubscribe", logs); err != nil || string(f.Result) != "false" {
				s.violate(s.name, "eth_unsubscribe %d of an ended subscription answered %s %+v (%v), not false", i+1, f.Result, f.Error, err)
			}
		}
		heads, err := ec.subscribe("newHeads", map[string]interface{}{"erpc": map[string]interface{}{}})
		if err != nil {
			s.violate(s.name, "pod-1: extended newHeads subscribe: %v", err)
			return
		}
		s.blocks(5)
		paused := time.Now()
		ec.pause()
		time.Sleep(slowPauseBlocks * blockTime)
		ec.unpause()
		time.Sleep(d3Reply)
		ends = ec.controls(heads, "end", paused)
		if len(ends) != 1 || ends[0].msg.Reason != "lagging" || ends[0].msg.Detail != "lag" {
			s.violate(s.name, "newHeads client that stopped reading %d blocks: ends %+v, want one lagging/lag", slowPauseBlocks, ends)
		}
		var bn hexutil.Uint64
		if f, err := ec.call("eth_blockNumber"); err != nil || f.Error != nil || json.Unmarshal(f.Result, &bn) != nil || bn == 0 {
			s.violate(s.name, "eth_blockNumber on the connection after the ends: %s %+v %v", f.Result, f.Error, err)
		}
		s.blocks(10)
	}}
}

// W6 (guard): standard subscriptions keep today's transcript (CT§10.2):
// newHeads sends each canonical head once, oldest first, its payload the
// vendor's header fields, and after a reorg the new branch from the fork
// point + 1; logs sends each matching log once, in block and logIndex
// order, the vendor's fields, and after a reorg removed:true for exactly
// the orphan logs it sent, newest first, before the new branch's. No
// erpc_subscription frame. geth's rpc.Client runs as baseline B (§14 W6;
// §5.4).
func w6() spec {
	return spec{name: "W6", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(10)
		type raw struct {
			pod         int
			ec          *extConn
			heads, logs string
		}
		var conns []raw
		for pod := range s.h.pods {
			ec, _, err := s.dialExt(pod, "", websocket.DefaultDialer)
			if err != nil {
				s.violate(s.name, "pod-%d: ws dial: %v", pod+1, err)
				continue
			}
			defer ec.close()
			heads, err1 := ec.subscribe("newHeads")
			logs, err2 := ec.subscribe("logs", chainsim.Filters[0].Params())
			if err1 != nil || err2 != nil {
				s.violate(s.name, "pod-%d: subscribes: %v, %v", pod+1, err1, err2)
				ec.close()
				continue
			}
			conns = append(conns, raw{pod, ec, heads, logs})
		}
		s.blocks(20)
		r := s.reorg(2)
		s.blocks(20)
		for _, c := range conns {
			name := fmt.Sprintf("%s/pod-%d", s.name, c.pod+1)
			ec := c.ec
			ec.mu.Lock()
			frames := append([]timedFrame(nil), ec.frames...)
			ec.mu.Unlock()
			for _, tf := range frames {
				if tf.f.ID == nil && (tf.f.Method != "eth_subscription" || len(tf.f.Params.Result) == 0) {
					s.violate(name, "frame %q for %q: not an eth_subscription notification", tf.f.Method, tf.f.Params.Subscription)
				}
			}
			s.requireHeadsTranscript(name, ec.of(c.heads, "eth_subscription", time.Time{}), r)
			s.requireLogsTranscript(name, ec.of(c.logs, "eth_subscription", time.Time{}), r)
		}
	}}
}

// requireHeadsTranscript: each head is a canonical or orphan block whose
// fields equal the vendor's header, one per number, linked, and the reorg
// r shows as the new branch from its fork point + 1 when the client had
// an orphan.
func (s *scenario) requireHeadsTranscript(name string, frames []timedFrame, r reorg) {
	if len(frames) == 0 {
		s.violate(name, "no newHeads notification")
		return
	}
	var hc headerChain
	gotOrphan, toldAt := false, uint64(0)
	orphan := map[hash]bool{}
	for _, b := range r.old {
		orphan[b.Hash] = true
	}
	for i, tf := range frames {
		var fields map[string]json.RawMessage
		var h headJSON
		if json.Unmarshal(tf.f.Params.Result, &fields) != nil || json.Unmarshal(tf.f.Params.Result, &h) != nil {
			s.violate(name, "head %d does not parse: %.200s", i, tf.f.Params.Result)
			return
		}
		b := s.chain.Block(h.Hash)
		if b == nil {
			s.violate(name, "head %d %s is no block of the chain", h.Number, short(h.Hash))
			return
		}
		var vendor map[string]json.RawMessage
		_ = json.Unmarshal(b.JSON(), &vendor)
		for k, v := range fields {
			if want, ok := vendor[k]; !ok || string(want) != string(v) {
				s.violate(name, "head %d field %s is %.80s, the vendor's %.80s", h.Number, k, v, want)
			}
		}
		gotOrphan = gotOrphan || orphan[h.Hash]
		switch kind := hc.apply(uint64(h.Number), h.Hash, h.ParentHash); kind {
		case appliedStart, appliedNext:
		case appliedReplace:
			if !gotOrphan || toldAt != 0 {
				s.violate(name, "head %d replaces a branch outside the reorg", h.Number)
			}
			toldAt = uint64(h.Number)
		default:
			s.violate(name, "head %d %s %s", h.Number, short(h.Hash), kind)
		}
	}
	if gotOrphan && toldAt != r.old[0].Number {
		s.violate(name, "received orphan heads, then no new branch from the fork point + 1 (%d): replaced at %d", r.old[0].Number, toldAt)
	}
}

// requireLogsTranscript: each log equals the chain's, sent once, in block
// and logIndex order; the reorg r sends removed:true for exactly the
// orphan logs sent, newest first, before any new-branch log at or above
// its fork point + 1.
func (s *scenario) requireLogsTranscript(name string, frames []timedFrame, r reorg) {
	if len(frames) == 0 {
		s.violate(name, "no logs notification")
		return
	}
	orphan := map[hash]bool{}
	for _, b := range r.old {
		orphan[b.Hash] = true
	}
	sent := map[chainsim.LogKey]bool{}
	var sentOrphans, removed []chainsim.LogKey
	var lastN uint64
	lastI := -1
	inRemoval, removalDone := false, false
	for _, tf := range frames {
		var l logJSON
		if err := json.Unmarshal(tf.f.Params.Result, &l); err != nil {
			s.violate(name, "log does not parse: %v: %.200s", err, tf.f.Params.Result)
			return
		}
		b := s.chain.Block(l.BlockHash)
		if b == nil || int(l.LogIndex) >= len(b.Logs()) {
			s.violate(name, "log %d of %s is no log of the chain", l.LogIndex, short(l.BlockHash))
			return
		}
		want := b.Logs()[l.LogIndex]
		if l.Data != "0x" || l.Address != want.Address || !slicesEqual(l.Topics, want.Topics) || l.TxHash != want.TxHash || uint64(l.BlockNumber) != b.Number {
			s.violate(name, "log %d of block %d differs from the chain's", l.LogIndex, b.Number)
		}
		key := want.Key()
		if l.Removed {
			if removalDone {
				s.violate(name, "removed:true for log %d of block %d after the new branch began", l.LogIndex, b.Number)
			}
			inRemoval = true
			removed = append(removed, key)
			continue
		}
		if inRemoval {
			inRemoval, removalDone = false, true
			lastN, lastI = 0, -1
		}
		switch {
		case sent[key]:
			s.violate(name, "log %d of block %d twice", l.LogIndex, b.Number)
		case b.Number < lastN || b.Number == lastN && int(l.LogIndex) <= lastI:
			s.violate(name, "log %d of block %d after log %d of block %d", l.LogIndex, b.Number, lastI, lastN)
		}
		sent[key] = true
		lastN, lastI = b.Number, int(l.LogIndex)
		if orphan[b.Hash] {
			sentOrphans = append(sentOrphans, key)
		}
	}
	slices.Reverse(sentOrphans)
	if !slices.Equal(sentOrphans, removed) {
		s.violate(name, "removed:true for %v, want the sent orphan logs newest first %v", removed, sentOrphans)
	}
}

// W7: the URL sets heartbeat=10 for every subscription on the connection;
// one subscription's own heartbeat 20 wins for it (§14 W7; §5.1).
func w7() spec {
	return spec{name: "W7", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		ec, _, err := s.dialExt(0, fmt.Sprintf("?heartbeat=%d", int(w7URLHeartbeat/time.Second)), websocket.DefaultDialer)
		if err != nil {
			s.violate(s.name, "pod-1: ws dial with ?heartbeat: %v", err)
			return
		}
		defer ec.close()
		heads, err1 := ec.subscribe("newHeads")
		logs, err2 := ec.subscribe("logs", chainsim.Filters[0].Params())
		own, err3 := ec.subscribe("newHeads", map[string]interface{}{"erpc": map[string]interface{}{"heartbeat": int(w7SubHeartbeat / time.Second)}})
		if err1 != nil || err2 != nil || err3 != nil {
			s.violate(s.name, "pod-1: subscribes: %v, %v, %v", err1, err2, err3)
			return
		}
		s.blocks(10)
		s.chain.Halt(true)
		tip := s.chain.Tip()
		at := time.Now()
		time.Sleep(w7Halt)
		end := time.Now()
		s.chain.Halt(false)
		s.requireBeats(ec, s.name+"/url-heads", heads, w7URLHeartbeat, at.Add(liveBound), end, tip)
		s.requireBeats(ec, s.name+"/url-logs", logs, w7URLHeartbeat, at.Add(liveBound), end, tip)
		s.requireBeats(ec, s.name+"/own", own, w7SubHeartbeat, at.Add(liveBound), end, tip)
		s.blocks(10)
	}}
}

// W8: a heartbeat of 0.5s, an unknown key, a malformed token and an
// option on a kind it does not apply to are -32602 naming the key; from
// in the URL is 400 at the upgrade (§14 W8; §3.1, §5.1).
func w8() spec {
	return spec{name: "W8", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(10)
		for pod := range s.h.pods {
			ec, _, err := s.dialExt(pod, "", websocket.DefaultDialer)
			if err != nil {
				s.violate(s.name, "pod-%d: ws dial: %v", pod+1, err)
				continue
			}
			for _, c := range []struct {
				key    string
				params []interface{}
			}{
				{"heartbeat", []interface{}{"newHeads", map[string]interface{}{"erpc": map[string]interface{}{"heartbeat": 0.5}}}},
				{"bogus", []interface{}{"newHeads", map[string]interface{}{"erpc": map[string]interface{}{"bogus": 1}}}},
				{"from", []interface{}{"logs", chainsim.Filters[0].Params(), map[string]interface{}{"erpc": map[string]interface{}{"from": "0x12:0xzz"}}}},
				{"blockEnd", []interface{}{"newHeads", map[string]interface{}{"erpc": map[string]interface{}{"blockEnd": true}}}},
			} {
				f, err := ec.call("eth_subscribe", c.params...)
				switch {
				case err != nil:
					s.violate(s.name, "pod-%d: subscribe with bad %s: %v", pod+1, c.key, err)
				case f.Error == nil || f.Error.Code != -32602 || !strings.Contains(f.Error.Message, c.key):
					s.violate(s.name, "pod-%d: subscribe with bad %s answered %s %+v, not -32602 naming it", pod+1, c.key, f.Result, f.Error)
				}
			}
			ec.close()
			tip := s.chain.Tip()
			bad, resp, err := s.dialExt(pod, "?from="+token(tip.Number, tip.Hash), websocket.DefaultDialer)
			if err == nil {
				bad.close()
			}
			if resp == nil || resp.StatusCode != http.StatusBadRequest {
				code := 0
				if resp != nil {
					code = resp.StatusCode
				}
				s.violate(s.name, "pod-%d: ws URL with from: status %d (err %v), not 400", pod+1, code, err)
			}
		}
		s.blocks(10)
	}}
}

// W9: pod-1 is cut from every vendor for 2 minutes (CT N4). Its extended
// logs clients, all resumable, get status degraded within W + 1s and end
// stalled at the 60s read wait, then resume on pod-2 with no gap (§14 W9;
// §3.4).
func w9() spec {
	return spec{name: "W9", bound: healthyDelivery, clients: extClients(0, true),
		allow: []string{"grpc:Unavailable@pod-1", "ws:jump@pod-1", "ws:logs-end@pod-1", "ws-ext:stalled@pod-1"},
		body: func(s *scenario) {
			var onP1 []*extLogsClient
			for _, c := range s.exts {
				if c.currentPod() == 0 {
					onP1 = append(onP1, c)
				}
			}
			cut := cutPod1(s)
			for _, c := range onP1 {
				c.mu.Lock()
				var degraded time.Time
				for _, st := range c.statuses {
					if st.pod == 0 && st.msg.State == "degraded" && st.at.After(cut) {
						degraded = st.at
						break
					}
				}
				c.mu.Unlock()
				switch {
				case degraded.IsZero():
					s.violate(c.name, "no status degraded on the cut pod-1")
				case degraded.Sub(cut) > w9Degraded:
					s.violate(c.name, "status degraded %s after the cut, above %s", degraded.Sub(cut).Round(time.Millisecond), w9Degraded)
				}
				var end *clientEnd
				c.mu.Lock()
				for i, e := range c.ends {
					if e.at.After(cut) && e.pod == 0 {
						end = &c.ends[i]
						break
					}
				}
				c.mu.Unlock()
				switch {
				case end == nil:
					s.violate(c.name, "never ended on the cut pod-1")
				case end.reason != "end stalled/":
					s.violate(c.name, "ended %q on the cut pod-1, not stalled", end.reason)
				case end.at.Sub(cut) > grpcReadWait+reconnectSlack:
					s.violate(c.name, "ended stalled %s after the cut, above the %s read wait", end.at.Sub(cut).Round(time.Second), grpcReadWait)
				case !c.deliveredOn(1, end.at):
					s.violate(c.name, "did not resume on pod-2 after ending stalled")
				}
			}
		}}
}
