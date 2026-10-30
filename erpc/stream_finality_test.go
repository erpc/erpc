package erpc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/test/chainsim"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// streamFrame is one frame an extended ws client read, with the chain's
// tip when it arrived.
type streamFrame struct {
	at     time.Time
	tip    uint64
	id     int
	result json.RawMessage
	err    map[string]interface{}
	method string
	sub    string
	data   json.RawMessage
}

// control is the frame's erpc_subscription result.
func (f streamFrame) control() map[string]interface{} {
	var m map[string]interface{}
	if f.method == "erpc_subscription" {
		_ = json.Unmarshal(f.data, &m)
	}
	return m
}

// streamConn is one ws connection of a finality test; its reader keeps
// every frame.
type streamConn struct {
	t      *testing.T
	s      *chaosSeed
	conn   *websocket.Conn
	nextID int
	mu     sync.Mutex
	frames []streamFrame
	closed error
}

func dialStream(t *testing.T, s *chaosSeed, query string, header http.Header) (*streamConn, *http.Response, error) {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/evm/123%s", s.addr, query), header)
	if err != nil {
		return nil, resp, err
	}
	c := &streamConn{t: t, s: s, conn: conn}
	t.Cleanup(func() { _ = conn.Close() })
	go c.read()
	return c, resp, nil
}

func (c *streamConn) read() {
	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			c.mu.Lock()
			c.closed = err
			c.mu.Unlock()
			return
		}
		var raw struct {
			ID     *int                   `json:"id"`
			Result json.RawMessage        `json:"result"`
			Error  map[string]interface{} `json:"error"`
			Method string                 `json:"method"`
			Params struct {
				Subscription string          `json:"subscription"`
				Result       json.RawMessage `json:"result"`
			} `json:"params"`
		}
		if json.Unmarshal(msg, &raw) != nil {
			continue
		}
		f := streamFrame{at: time.Now(), tip: c.s.chain.Tip().Number, result: raw.Result, err: raw.Error,
			method: raw.Method, sub: raw.Params.Subscription, data: raw.Params.Result, id: -1}
		if raw.ID != nil {
			f.id = *raw.ID
		}
		c.mu.Lock()
		c.frames = append(c.frames, f)
		c.mu.Unlock()
	}
}

// call sends method with params and waits for its answer.
func (c *streamConn) call(method string, params ...interface{}) streamFrame {
	c.t.Helper()
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	req, err := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	require.NoError(c.t, err)
	require.NoError(c.t, c.conn.WriteMessage(websocket.TextMessage, req))
	var answer streamFrame
	require.Eventually(c.t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, f := range c.frames {
			if f.id == id {
				answer = f
				return true
			}
		}
		return false
	}, 20*time.Second, 10*time.Millisecond, "answer to %s %v", method, params)
	return answer
}

// subscribe subscribes params; it fails the test on a refusal.
func (c *streamConn) subscribe(params ...interface{}) string {
	c.t.Helper()
	f := c.call("eth_subscribe", params...)
	require.Nil(c.t, f.err, "subscribe %v refused: %v", params, f.err)
	var id string
	require.NoError(c.t, json.Unmarshal(f.result, &id), "subscribe answer %s", f.result)
	return id
}

// refused subscribes params and returns the refusal's code and message.
func (c *streamConn) refused(params ...interface{}) (code int, message string) {
	c.t.Helper()
	f := c.call("eth_subscribe", params...)
	require.NotNil(c.t, f.err, "subscribe %v is refused, got %s", params, f.result)
	n, _ := f.err["code"].(float64)
	m, _ := f.err["message"].(string)
	return int(n), m
}

// of are the frames of subscription sub.
func (c *streamConn) of(sub string) []streamFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []streamFrame
	for _, f := range c.frames {
		if f.sub == sub {
			out = append(out, f)
		}
	}
	return out
}

// controls are sub's control messages of type typ.
func (c *streamConn) controls(sub, typ string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, f := range c.of(sub) {
		if m := f.control(); m != nil && m["type"] == typ {
			out = append(out, m)
		}
	}
	return out
}

func ext(opts map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"erpc": opts}
}

func hexNumber(t *testing.T, v interface{}) uint64 {
	t.Helper()
	s, _ := v.(string)
	n, err := common.HexToInt64(s)
	require.NoError(t, err, "quantity %v", v)
	return uint64(n)
}

// headsApplied is what a client that applies sub's heads, and drops what
// an undo names, holds: hash by number; numbers in arrival order; each
// head's chain tip on arrival; and each undo's fork point.
type headsApplied struct {
	chain map[uint64]gethcommon.Hash
	order []uint64
	tips  map[uint64]uint64
	undos []uint64
}

func (c *streamConn) applyHeads(sub string) headsApplied {
	c.t.Helper()
	a := headsApplied{chain: map[uint64]gethcommon.Hash{}, tips: map[uint64]uint64{}}
	for _, f := range c.of(sub) {
		switch f.method {
		case "eth_subscription":
			var h struct{ Number, Hash string }
			require.NoError(c.t, json.Unmarshal(f.data, &h))
			n := hexNumber(c.t, h.Number)
			a.chain[n] = gethcommon.HexToHash(h.Hash)
			a.order = append(a.order, n)
			a.tips[n] = f.tip
		case "erpc_subscription":
			m := f.control()
			if m["type"] != "undo" {
				continue
			}
			fp, _ := m["forkPoint"].(map[string]interface{})
			n := hexNumber(c.t, fp["number"])
			a.undos = append(a.undos, n)
			for k := range a.chain {
				if k > n {
					delete(a.chain, k)
				}
			}
			a.order = append(a.order, 0)
		}
	}
	return a
}

// requireCanonical: every head the client holds is the canonical block at
// its number; each head follows the one before it, or the fork point of
// the undo before it.
func (a headsApplied) requireCanonical(t *testing.T, s *chaosSeed, name string) {
	t.Helper()
	require.NotEmpty(t, a.chain, "%s got heads", name)
	for n, h := range a.chain {
		b := s.chain.CanonAt(n)
		require.NotNil(t, b, "%s: block %d", name, n)
		assert.Equal(t, b.Hash, h, "%s holds block %d off the canonical chain", name, n)
	}
	prev, undo := uint64(0), 0
	for _, n := range a.order {
		if n == 0 {
			prev = a.undos[undo]
			undo++
			continue
		}
		if prev != 0 {
			assert.Equal(t, prev+1, n, "%s: heads in order, once each", name)
		}
		prev = n
	}
}

// logsApplied is what a logs client that applies sub's logs, blockEnds
// and undos holds.
type logsApplied struct {
	held      map[chainsim.LogKey]uint64
	blockEnds []uint64
	undos     []uint64
	removed   int
}

func (c *streamConn) applyLogs(sub string) logsApplied {
	c.t.Helper()
	a := logsApplied{held: map[chainsim.LogKey]uint64{}}
	for _, f := range c.of(sub) {
		switch f.method {
		case "eth_subscription":
			var l struct {
				BlockHash, BlockNumber, LogIndex string
				Removed                          bool
			}
			require.NoError(c.t, json.Unmarshal(f.data, &l))
			k := chainsim.LogKey{BlockHash: gethcommon.HexToHash(l.BlockHash), LogIndex: int(hexNumber(c.t, l.LogIndex))}
			if l.Removed {
				a.removed++
				delete(a.held, k)
				continue
			}
			a.held[k] = hexNumber(c.t, l.BlockNumber)
		case "erpc_subscription":
			m := f.control()
			switch m["type"] {
			case "blockEnd":
				blk, _ := m["block"].(map[string]interface{})
				a.blockEnds = append(a.blockEnds, hexNumber(c.t, blk["number"]))
			case "undo":
				fp, _ := m["forkPoint"].(map[string]interface{})
				n := hexNumber(c.t, fp["number"])
				a.undos = append(a.undos, n)
				for k, bn := range a.held {
					if bn > n {
						delete(a.held, k)
					}
				}
				a.blockEnds = append(a.blockEnds, 0, n)
			}
		}
	}
	return a
}

// requireCanonical: the blockEnds run one per block, an undo resuming
// after its fork point, and the logs held over them are the canonical
// chain's logs of filter.
func (a logsApplied) requireCanonical(t *testing.T, s *chaosSeed, name string, filter int) {
	t.Helper()
	require.NotEmpty(t, a.blockEnds, "%s got block ends", name)
	var first, last, prev uint64
	for i := 0; i < len(a.blockEnds); i++ {
		n := a.blockEnds[i]
		if n == 0 {
			i++
			prev = a.blockEnds[i]
			continue
		}
		if prev != 0 {
			require.Equal(t, prev+1, n, "%s: one blockEnd per block, in order", name)
		}
		if first == 0 {
			first = n
		}
		prev, last = n, n
	}
	want := s.chain.CanonicalLogs(chainsim.Filters[filter], first, last)
	for k, n := range a.held {
		if n < first || n > last {
			continue
		}
		_, ok := want[k]
		assert.True(t, ok, "%s holds log %d of block %d, not canonical", name, k.LogIndex, n)
		delete(want, k)
	}
	assert.Empty(t, want, "%s misses canonical logs of blocks %d-%d", name, first, last)
}

// gathered sums the series of metric c whose labels match labels; ok is
// false while none is exported. The test binary registers no counter or
// gauge on the default registry, so c is gathered on its own.
func gathered(t *testing.T, c prometheus.Collector, labels map[string]string) (sum float64, ok bool) {
	t.Helper()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
	metrics:
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if v, want := labels[lp.GetName()]; want && v != lp.GetValue() {
					continue metrics
				}
			}
			ok = true
			sum += m.GetGauge().GetValue() + m.GetCounter().GetValue()
		}
	}
	return sum, ok
}

func startFinalitySeed(t *testing.T, seed int64, faults chainsim.Faults) *chaosSeed {
	t.Helper()
	s := newChaosSeed(t, seed, true, chaosOpts{})
	s.setAll(func(f *chainsim.Faults) { *f = faults })
	s.startERPC()
	s.chain.Start()
	s.blocks(5)
	return s
}

var trackerLabels = map[string]string{"project": "test_ws", "network": "evm:123"}

// Streams spec §3.1: finality and reorg values, their kinds and the reorg
// none rule, per subscription and as URL defaults.
func TestStreamFinality_Options(t *testing.T) {
	s := startFinalitySeed(t, 101, chainsim.Faults{})
	c, _, err := dialStream(t, s, "", nil)
	require.NoError(t, err)
	for _, opts := range []map[string]interface{}{
		{"finality": "finalized", "reorg": "none"},
		{"finality": "depth:3", "reorg": "undo"},
		{"finality": "depth:16"},
		{"finality": "latest", "reorg": "geth"},
	} {
		c.subscribe("newHeads", ext(opts))
	}
	c.subscribe("logs", chainsim.Filters[0].Params(), ext(map[string]interface{}{"finality": "finalized", "reorg": "undo"}))
	for _, tc := range []struct {
		opts map[string]interface{}
		key  string
	}{
		{map[string]interface{}{"reorg": "none"}, "reorg"},
		{map[string]interface{}{"finality": "safe", "reorg": "none"}, "reorg"},
		{map[string]interface{}{"finality": "depth:0"}, "finality"},
		{map[string]interface{}{"finality": "depth:17"}, "finality"},
		{map[string]interface{}{"finality": "depth:x"}, "finality"},
		{map[string]interface{}{"finality": "soon"}, "finality"},
		{map[string]interface{}{"finality": 3}, "finality"},
		{map[string]interface{}{"reorg": "sideways"}, "reorg"},
	} {
		code, msg := c.refused("newHeads", ext(tc.opts))
		assert.EqualValues(t, common.JsonRpcErrorInvalidArgument, code, "%v", tc.opts)
		assert.Contains(t, msg, `"`+tc.key+`"`, "%v names the key", tc.opts)
		assert.NotContains(t, msg, "unknown option", "%v", tc.opts)
	}
	code, msg := c.refused("newPendingTransactions", ext(map[string]interface{}{"finality": "finalized"}))
	assert.EqualValues(t, common.JsonRpcErrorInvalidArgument, code)
	assert.Contains(t, msg, "does not apply")

	u, _, err := dialStream(t, s, "?finality=finalized&reorg=none", nil)
	require.NoError(t, err)
	u.subscribe("newHeads")
	code, msg = u.refused("newHeads", ext(map[string]interface{}{"finality": "latest"}))
	assert.EqualValues(t, common.JsonRpcErrorInvalidArgument, code)
	assert.Contains(t, msg, `"reorg"`)
	_, resp, err := dialStream(t, s, "?reorg=sideways", nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// F1 and F6's hold: vendors finalize 64 blocks below their head, far below
// a window of 16 blocks and 3s. Final taps make the tracker hold the
// headers down to Finalized: heads and logs flow, every block at or below
// Finalized when sent, never an undo, logs canonical; the hold gauge is
// back to 0 once they close.
func TestStreamFinality_FinalizedHoldsTheWindow(t *testing.T) {
	s := startFinalitySeed(t, 103, chainsim.Faults{})
	s.blocks(chainsim.FinalizedLag)
	c, _, err := dialStream(t, s, "", nil)
	require.NoError(t, err)
	heads := c.subscribe("newHeads", ext(map[string]interface{}{"finality": "finalized"}))
	logs := c.subscribe("logs", chainsim.Filters[0].Params(), ext(map[string]interface{}{"finality": "finalized", "reorg": "none", "blockEnd": true}))
	var held float64
	require.Eventually(t, func() bool {
		held, _ = gathered(t, telemetry.MetricChainTrackerFinalHoldHeaders, trackerLabels)
		return held > 0
	}, 20*time.Second, 50*time.Millisecond, "final taps hold headers below the window")
	s.blocks(20)
	s.reorg(2)
	s.blocks(30)

	h := c.applyHeads(heads)
	h.requireCanonical(t, s, "finalized heads")
	assert.GreaterOrEqual(t, len(h.chain), 30, "finalized heads flow")
	assert.Empty(t, h.undos, "a final tap never gets an undo")
	for n, tip := range h.tips {
		assert.LessOrEqual(t, n+chainsim.FinalizedLag, tip, "head %d at or below Finalized when sent (tip %d)", n, tip)
	}
	require.Empty(t, c.controls(heads, "end"))
	require.Empty(t, c.controls(logs, "end"))
	l := c.applyLogs(logs)
	l.requireCanonical(t, s, "finalized logs", 0)
	assert.Empty(t, l.undos)
	assert.Zero(t, l.removed)

	_ = c.conn.Close()
	require.Eventually(t, func() bool {
		v, ok := gathered(t, telemetry.MetricChainTrackerFinalHoldHeaders, trackerLabels)
		return ok && v == 0
	}, 20*time.Second, 50*time.Millisecond, "the hold ends with its taps")
}

// F6's span: a hold of more than FinalHoldMax headers below the window
// refuses final subscribes with -32005 finality_span and ends open final
// taps out_of_range/finality_span; depth taps are unaffected.
func TestStreamFinality_FinalitySpan(t *testing.T) {
	prev := evm.FinalHoldMax.Load()
	t.Cleanup(func() { evm.FinalHoldMax.Store(prev) })
	s := startFinalitySeed(t, 107, chainsim.Faults{})
	s.blocks(chainsim.FinalizedLag)
	c, _, err := dialStream(t, s, "", nil)
	require.NoError(t, err)

	evm.FinalHoldMax.Store(200)
	open := c.subscribe("newHeads", ext(map[string]interface{}{"finality": "finalized"}))
	require.Eventually(t, func() bool { return len(c.applyHeads(open).chain) >= 3 }, 20*time.Second, 50*time.Millisecond, "the final tap flows")

	// Finalized trails 64 blocks; the window holds 16, so the hold is
	// about 48 headers.
	evm.FinalHoldMax.Store(10)
	require.Eventually(t, func() bool { return len(c.controls(open, "end")) == 1 }, 20*time.Second, 50*time.Millisecond, "the open final tap ends")
	end := c.controls(open, "end")[0]
	assert.Equal(t, "out_of_range", end["reason"])
	assert.Equal(t, "finality_span", end["detail"])
	code, msg := c.refused("newHeads", ext(map[string]interface{}{"finality": "finalized"}))
	assert.EqualValues(t, common.JsonRpcErrorCapacityExceeded, code)
	assert.Contains(t, msg, "finality_span")
	depth := c.subscribe("newHeads", ext(map[string]interface{}{"finality": "depth:4"}))
	s.blocks(5)
	assert.NotEmpty(t, c.applyHeads(depth).chain, "a depth tap needs no hold")
	v, _ := gathered(t, telemetry.MetricTapEndTotal, map[string]string{"project": "test_ws", "network": "evm:123", "transport": "ws", "kind": "newHeads", "reason": "out_of_range", "detail": "finality_span"})
	assert.GreaterOrEqual(t, v, 1.0)
}

// F2 and F3: reorgs 2 deep never reach a depth:3 tap; an 8-deep one reaches
// it as one undo. Latest undo taps get one undo per reorg, never a
// removed:true log; every client ends canonical.
func TestStreamFinality_DepthAndUndo(t *testing.T) {
	s := startFinalitySeed(t, 109, chainsim.Faults{})
	s.blocks(10)
	c, _, err := dialStream(t, s, "", nil)
	require.NoError(t, err)
	depthHeads := c.subscribe("newHeads", ext(map[string]interface{}{"finality": "depth:3", "reorg": "undo"}))
	undoHeads := c.subscribe("newHeads", ext(map[string]interface{}{"reorg": "undo"}))
	undoLogs := c.subscribe("logs", chainsim.Filters[0].Params(), ext(map[string]interface{}{"reorg": "undo", "blockEnd": true}))
	depthLogs := c.subscribe("logs", chainsim.Filters[1].Params(), ext(map[string]interface{}{"finality": "depth:3", "reorg": "undo", "blockEnd": true}))
	s.blocks(10)
	s.reorg(2)
	s.blocks(8)
	s.reorg(8)
	s.blocks(12)

	dh := c.applyHeads(depthHeads)
	dh.requireCanonical(t, s, "depth:3 heads")
	assert.Len(t, dh.undos, 1, "only the 8-deep reorg reaches a depth:3 tap")
	for n, tip := range dh.tips {
		assert.LessOrEqual(t, n+3, tip, "head %d at least 3 below the tip %d", n, tip)
	}
	uh := c.applyHeads(undoHeads)
	uh.requireCanonical(t, s, "undo heads")
	assert.Len(t, uh.undos, 2, "one undo per reorg")
	ul := c.applyLogs(undoLogs)
	ul.requireCanonical(t, s, "undo logs", 0)
	assert.Len(t, ul.undos, 2)
	assert.Zero(t, ul.removed, "undo mode sends no removed:true")
	dl := c.applyLogs(depthLogs)
	dl.requireCanonical(t, s, "depth:3 logs", 1)
	assert.Len(t, dl.undos, 1)
	assert.Zero(t, dl.removed)
}

// F4: reorg none with finalized ends final_reverted once a reorg reverts a
// final block it sent; the subscription is gone.
func TestStreamFinality_FinalReverted(t *testing.T) {
	s := startFinalitySeed(t, 113, chainsim.Faults{FinalityDepth: 4})
	s.blocks(10)
	c, _, err := dialStream(t, s, "", nil)
	require.NoError(t, err)
	heads := c.subscribe("newHeads", ext(map[string]interface{}{"finality": "finalized", "reorg": "none"}))
	require.Eventually(t, func() bool { return len(c.applyHeads(heads).chain) >= 3 }, 20*time.Second, 50*time.Millisecond, "the final tap flows")
	endLabels := map[string]string{"project": "test_ws", "network": "evm:123", "transport": "ws", "kind": "newHeads", "reason": "final_reverted"}
	before, _ := gathered(t, telemetry.MetricTapEndTotal, endLabels)
	s.reorg(10)
	require.Eventually(t, func() bool { return len(c.controls(heads, "end")) == 1 }, 20*time.Second, 50*time.Millisecond, "the tap ends")
	end := c.controls(heads, "end")[0]
	assert.Equal(t, "final_reverted", end["reason"])
	blk, _ := end["block"].(map[string]interface{})
	require.NotNil(t, blk, "the end names the reverted block: %v", end)
	n := hexNumber(t, blk["number"])
	assert.NotEqual(t, s.chain.CanonAt(n).Hash.Hex(), blk["hash"], "the named block is no longer canonical")
	assert.Equal(t, n, c.applyHeads(heads).order[len(c.applyHeads(heads).order)-1], "the newest block the client applied")
	assert.Empty(t, c.controls(heads, "undo"))
	after, _ := gathered(t, telemetry.MetricTapEndTotal, endLabels)
	assert.Equal(t, before+1, after)
	var ok bool
	require.NoError(t, json.Unmarshal(c.call("eth_unsubscribe", heads).result, &ok))
	assert.False(t, ok, "the subscription ended")
}

// F5: vendors' safe tags 8 below their head, finalized 20; ws-b refuses
// safe. A safe tap gets every block at or below Head.Safe, from the
// others' safe votes; heartbeats carry safe; the served tip exports
// axis safe. After the last safe tap closes no safe poll reaches any
// vendor and the safe series is gone. When every vendor refuses safe,
// Safe is Finalized.
func TestStreamFinality_SafeOnDemand(t *testing.T) {
	s := startFinalitySeed(t, 127, chainsim.Faults{SafeDepth: 8, FinalityDepth: 20})
	s.wsB.Update(func(f *chainsim.Faults) { f.NoSafe = true })
	s.blocks(25)
	safeCalls := func() (n int64) {
		for _, v := range s.vendors {
			n += v.Stats().SafeCalls
		}
		return n
	}
	require.Zero(t, safeCalls(), "no safe tap, no safe poll")
	safeLabels := map[string]string{"project": "test_ws", "network": "evm:123", "lane": "all", "axis": "safe"}
	_, exported := gathered(t, telemetry.MetricNetworkServedTipBlockNumber, safeLabels)
	require.False(t, exported, "no safe axis while Head.Safe is not computed")

	c, _, err := dialStream(t, s, "", nil)
	require.NoError(t, err)
	safe := c.subscribe("newHeads", ext(map[string]interface{}{"finality": "safe"}))
	quiet := c.subscribe("logs", map[string]interface{}{"address": "0x00000000000000000000000000000000000000ff"}, ext(map[string]interface{}{"heartbeat": 1}))
	s.blocks(25)
	h := c.applyHeads(safe)
	h.requireCanonical(t, s, "safe heads")
	aboveFinal := 0
	for n, tip := range h.tips {
		assert.LessOrEqual(t, n+8, tip, "head %d at or below Safe (tip %d)", n, tip)
		if n+20 > tip {
			aboveFinal++
		}
	}
	assert.Positive(t, aboveFinal, "safe heads run above Finalized")
	assert.Positive(t, s.wsB.Stats().SafeCalls, "ws-b is polled and refuses")
	beats := c.controls(quiet, "heartbeat")
	require.NotEmpty(t, beats)
	last := beats[len(beats)-1]
	require.Contains(t, last, "safe", "heartbeats carry safe while it is computed: %v", last)
	assert.Greater(t, hexNumber(t, last["safe"]), hexNumber(t, last["finalized"]))
	v, exported := gathered(t, telemetry.MetricNetworkServedTipBlockNumber, safeLabels)
	require.True(t, exported)
	assert.Positive(t, v)

	_ = c.conn.Close()
	time.Sleep(time.Second)
	polled := safeCalls()
	s.blocks(15)
	assert.Equal(t, polled, safeCalls(), "no safe poll after the last safe tap closed")
	_, exported = gathered(t, telemetry.MetricNetworkServedTipBlockNumber, safeLabels)
	assert.False(t, exported, "the safe axis leaves with Head.Safe")

	s.setAll(func(f *chainsim.Faults) { f.NoSafe = true })
	c2, _, err := dialStream(t, s, "", nil)
	require.NoError(t, err)
	safe2 := c2.subscribe("newHeads", ext(map[string]interface{}{"finality": "safe"}))
	quiet2 := c2.subscribe("logs", map[string]interface{}{"address": "0x00000000000000000000000000000000000000ff"}, ext(map[string]interface{}{"heartbeat": 1}))
	s.blocks(20)
	for n, tip := range c2.applyHeads(safe2).tips {
		assert.LessOrEqual(t, n+20, tip, "with no safe vote Safe is Finalized: head %d, tip %d", n, tip)
	}
	beats = c2.controls(quiet2, "heartbeat")
	require.NotEmpty(t, beats)
	last = beats[len(beats)-1]
	assert.Equal(t, last["finalized"], last["safe"], "Safe = Finalized: %v", last)
}

// F7: a depth:2 tap on a use-upstream lane whose upstream trails 6 blocks
// never gets a block above min(lane Latest, confirmed − 2).
func TestStreamFinality_DepthOnLane(t *testing.T) {
	s := startFinalitySeed(t, 131, chainsim.Faults{})
	s.httpA.Update(func(f *chainsim.Faults) { f.Lag = 6 })
	s.blocks(10)
	hdr := http.Header{}
	hdr.Set("X-ERPC-Use-Upstream", s.httpA.ID)
	c, _, err := dialStream(t, s, "", hdr)
	require.NoError(t, err)
	sub := c.subscribe("newHeads", ext(map[string]interface{}{"finality": "depth:2"}))
	s.blocks(20)
	h := c.applyHeads(sub)
	h.requireCanonical(t, s, "lane depth:2 heads")
	assert.GreaterOrEqual(t, len(h.chain), 10)
	for n, tip := range h.tips {
		assert.LessOrEqual(t, n+6, tip, "head %d at or below the lane's Latest (tip %d)", n, tip)
	}
	assert.False(t, strings.Contains(fmt.Sprint(c.controls(sub, "end")), "reason"), "the tap keeps flowing")
}

// A finalized tap on a use-upstream lane whose upstream finalizes 50
// blocks below its head, while the others finalize 8 below: the lane's
// Finalized sits far below the network-wide one, so the final hold must
// cover the lane tap's start and cursor, not the network's Finalized.
// The tap starts, flows canonical, at or below the lane's Finalized,
// and never ends.
func TestStreamFinality_FinalizedOnLaneBelowTheNetwork(t *testing.T) {
	s := startFinalitySeed(t, 137, chainsim.Faults{FinalityDepth: 8})
	s.httpA.Update(func(f *chainsim.Faults) { f.FinalityDepth = 50 })
	s.blocks(60)
	hdr := http.Header{}
	hdr.Set("X-ERPC-Use-Upstream", s.httpA.ID)
	c, _, err := dialStream(t, s, "", hdr)
	require.NoError(t, err)
	sub := c.subscribe("newHeads", ext(map[string]interface{}{"finality": "finalized"}))
	require.Eventually(t, func() bool { return len(c.applyHeads(sub).chain) >= 10 }, 20*time.Second, 50*time.Millisecond,
		"the lane's final tap flows: %v", c.controls(sub, "end"))
	h := c.applyHeads(sub)
	h.requireCanonical(t, s, "lane finalized heads")
	for n, tip := range h.tips {
		assert.LessOrEqual(t, n+50, tip, "head %d at or below the lane's Finalized (tip %d)", n, tip)
	}
	assert.Empty(t, c.controls(sub, "end"), "the tap keeps flowing")
}

// F6's shape: 0.2s blocks, every vendor finalizes 4,000 blocks below its
// head, far below a 5s window, and answers history 25ms late, as a remote
// vendor's round trip. A finalized subscribe's start is the Finalized
// block, pinned once fill reaches it: within 10 W (20s), where one fetch
// at a time took 4,000 round trips. Its first block comes when Finalized
// next moves. Fill takes a rate permit only while half the budget is
// free: 500 per second per upstream here, so the round trip, not the
// budget, bounds it.
func TestStreamFinality_DeepFinalityFirstBlock(t *testing.T) {
	const depth = 4000
	s := newChaosSeed(t, 139, true, chaosOpts{history: depth + 200, maxBackfill: 5 * time.Second, budget: 1000})
	s.setAll(func(f *chainsim.Faults) { f.FinalityDepth, f.SlowHistory = depth, 25*time.Millisecond })
	s.startERPC()
	s.chain.Start()
	s.blocks(10)
	c, _, err := dialStream(t, s, "", nil)
	require.NoError(t, err)
	nw := s.nw.Load()
	fin := nw.headFor("").Finalized
	require.Positive(t, fin)
	start := time.Now()
	sub := c.subscribe("newHeads", ext(map[string]interface{}{"finality": "finalized"}))
	require.Eventually(t, func() bool {
		from, _, ok := nw.chainTracker.FollowedRange()
		return ok && from <= fin
	}, 2*time.Minute, 10*time.Millisecond, "fill pins Finalized %d", fin)
	took := time.Since(start)
	t.Logf("fill held Finalized %s after subscribe, %d headers below the head", took, depth)
	assert.Less(t, took, 20*time.Second, "fill reaches the start within 10 W")
	require.Eventually(t, func() bool { return len(c.applyHeads(sub).chain) > 0 }, 2*time.Minute, 20*time.Millisecond,
		"the first final block once Finalized moves: %v", c.controls(sub, "end"))
	h := c.applyHeads(sub)
	h.requireCanonical(t, s, "deep finalized heads")
	for n, tip := range h.tips {
		assert.LessOrEqual(t, n+depth, tip, "head %d at or below Finalized (tip %d)", n, tip)
	}
	assert.Empty(t, c.controls(sub, "end"))
}
