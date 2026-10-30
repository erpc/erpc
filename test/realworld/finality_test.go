package realworld

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/gorilla/websocket"
)

// Finality and reorg modes (specs/streams/feature.md §3.1, §4; §13
// F1–F7).
const (
	// finalWindow: F1 and F6 hold 5s of chain time, 25 blocks, far less
	// than the vendors' finality depth, as eth's 5 min window holds less
	// than its 64 blocks to finalized.
	finalWindow = 5 * time.Second
	// f6Depth, f6Span: F6 finalizes 4,000 blocks below the head, then
	// 25,000, past the 20,000-header hold (§10).
	f6Depth = 4000
	f6Span  = 25000
	// f5SafeDepth, f5FinalDepth: F5's vendors' safe and finalized tags.
	f5SafeDepth  = 8
	f5FinalDepth = 64
	// f7Lag: F7's lane upstream trails the network head by this much.
	f7Lag = 6
)

func finalityEvm() map[string]interface{} {
	return map[string]interface{}{"chainTracker": map[string]interface{}{"reorgWindow": reorgWindow, "maxBackfill": finalWindow.String()}}
}

func erpcOpts(kv ...interface{}) map[string]interface{} {
	o := map[string]interface{}{}
	for i := 0; i+1 < len(kv); i += 2 {
		o[kv[i].(string)] = kv[i+1]
	}
	return map[string]interface{}{"erpc": o}
}

// tipAt is the canonical tip when at came: the highest canonical block
// minted at or before it.
func (s *scenario) tipAt(at time.Time) uint64 {
	n := s.chain.Tip().Number
	for n > 0 {
		if b := s.chain.CanonAt(n); b == nil || !b.MintedAt.After(at) {
			break
		}
		n--
	}
	return n
}

// finalApplied is what a client that applies sub's heads, dropping what
// each undo names, holds.
type finalApplied struct {
	chain map[uint64]hash
	order []uint64 // 0 marks an undo
	at    map[uint64]time.Time
	undos []uint64
}

func (s *scenario) applyFinalHeads(ec *extConn, sub string) finalApplied {
	a := finalApplied{chain: map[uint64]hash{}, at: map[uint64]time.Time{}}
	ec.mu.Lock()
	frames := append([]timedFrame(nil), ec.frames...)
	ec.mu.Unlock()
	for _, tf := range frames {
		if tf.f.Params.Subscription != sub {
			continue
		}
		switch tf.f.Method {
		case "eth_subscription":
			var h struct {
				Number hexutil.Uint64 `json:"number"`
				Hash   hash           `json:"hash"`
			}
			if json.Unmarshal(tf.f.Params.Result, &h) != nil {
				continue
			}
			a.chain[uint64(h.Number)], a.at[uint64(h.Number)] = h.Hash, tf.at
			a.order = append(a.order, uint64(h.Number))
		case "erpc_subscription":
			var u struct {
				Type      string `json:"type"`
				ForkPoint *extRef
			}
			if json.Unmarshal(tf.f.Params.Result, &u) != nil || u.Type != "undo" || u.ForkPoint == nil {
				continue
			}
			fp := uint64(u.ForkPoint.Number)
			a.undos = append(a.undos, fp)
			for n := range a.chain {
				if n > fp {
					delete(a.chain, n)
				}
			}
			a.order = append(a.order, 0)
		}
	}
	return a
}

// requireFinal: the client holds heads, each canonical, each at least
// depth below the tip when it arrived; in order, an undo resuming after
// its fork point.
func (s *scenario) requireFinal(name string, a finalApplied, depth uint64) {
	if len(a.chain) < 10 {
		s.violate(name, "%d heads, want a flowing stream", len(a.chain))
		return
	}
	for n, h := range a.chain {
		if b := s.chain.CanonAt(n); b == nil || b.Hash != h {
			s.violate(name, "holds block %d %s off the canonical chain", n, short(h))
		}
		if tip := s.tipAt(a.at[n]); n+depth > tip {
			s.violate(name, "head %d sent at tip %d: above the bound, %d below the tip", n, tip, depth)
		}
	}
	var prev uint64
	undo := 0
	for _, n := range a.order {
		if n == 0 {
			prev = a.undos[undo]
			undo++
			continue
		}
		if prev != 0 && n != prev+1 {
			s.violate(name, "head %d after %d: not linked", n, prev)
		}
		prev = n
	}
}

// requireFinalLogs: blockEnds one per block, an undo resuming after its
// fork point; logs held over them canonical; no removed:true.
func (s *scenario) requireFinalLogs(name string, ec *extConn, sub string, filter int) (undos int) {
	held := map[chainsim.LogKey]uint64{}
	var first, last, prev uint64
	ec.mu.Lock()
	frames := append([]timedFrame(nil), ec.frames...)
	ec.mu.Unlock()
	for _, tf := range frames {
		if tf.f.Params.Subscription != sub {
			continue
		}
		switch tf.f.Method {
		case "eth_subscription":
			var l struct {
				BlockHash   hash           `json:"blockHash"`
				BlockNumber hexutil.Uint64 `json:"blockNumber"`
				LogIndex    hexutil.Uint64 `json:"logIndex"`
				Removed     bool           `json:"removed"`
			}
			if json.Unmarshal(tf.f.Params.Result, &l) != nil {
				continue
			}
			if l.Removed {
				s.violate(name, "removed:true for log %d of block %d", l.LogIndex, l.BlockNumber)
			}
			held[chainsim.LogKey{BlockHash: l.BlockHash, LogIndex: int(l.LogIndex)}] = uint64(l.BlockNumber)
		case "erpc_subscription":
			var r struct {
				Type      string  `json:"type"`
				Block     *extRef `json:"block"`
				ForkPoint *extRef `json:"forkPoint"`
			}
			if json.Unmarshal(tf.f.Params.Result, &r) != nil {
				continue
			}
			switch {
			case r.Type == "blockEnd" && r.Block != nil:
				n := uint64(r.Block.Number)
				if prev != 0 && n != prev+1 {
					s.violate(name, "blockEnd %d after %d", n, prev)
				}
				if first == 0 {
					first = n
				}
				prev, last = n, n
			case r.Type == "undo" && r.ForkPoint != nil:
				undos++
				prev = uint64(r.ForkPoint.Number)
				for k, n := range held {
					if n > prev {
						delete(held, k)
					}
				}
			}
		}
	}
	if first == 0 {
		s.violate(name, "no blockEnd")
		return undos
	}
	want := s.chain.CanonicalLogs(chainsim.Filters[filter], first, last)
	for k, n := range held {
		if n < first || n > last {
			continue
		}
		if _, ok := want[k]; !ok {
			s.violate(name, "holds log %d of block %d, not canonical", k.LogIndex, n)
		}
		delete(want, k)
	}
	if len(want) > 0 {
		s.violate(name, "misses %d canonical logs of blocks %d-%d", len(want), first, last)
	}
	return undos
}

// openFinal dials pod and subscribes params, nil on a violation.
func (s *scenario) openFinal(pod int, name string, params ...interface{}) (*extConn, string) {
	ec, _, err := s.dialExt(pod, "", websocket.DefaultDialer)
	if err != nil {
		s.violate(name, "pod-%d: ws dial: %v", pod+1, err)
		return nil, ""
	}
	sub, err := ec.subscribe(params...)
	if err != nil {
		s.violate(name, "pod-%d: subscribe %v: %v", pod+1, params, err)
		ec.close()
		return nil, ""
	}
	return ec, sub
}

// F1: vendors finalize 64 below their head; the window holds 25 blocks.
// finalized heads and logs (reorg none) on every pod: each block at or
// below Finalized when sent, never an undo, logs canonical; the hold
// gauge is up while they run and back to 0 once they close.
func f1() spec {
	return spec{name: "F1", bound: healthyDelivery, evm: finalityEvm(), clients: func(*scenario) {}, body: func(s *scenario) {
		s.blocks(chainsim.FinalizedLag + 10)
		type pair struct {
			ec          *extConn
			heads, logs string
		}
		var ps []pair
		for pod := range s.h.pods {
			ec, heads := s.openFinal(pod, s.name, "newHeads", erpcOpts("finality", "finalized"))
			if ec == nil {
				return
			}
			defer ec.close()
			logs, err := ec.subscribe("logs", chainsim.Filters[0].Params(), erpcOpts("finality", "finalized", "reorg", "none", "blockEnd", true))
			if err != nil {
				s.violate(s.name, "pod-%d: final logs subscribe: %v", pod+1, err)
				return
			}
			ps = append(ps, pair{ec, heads, logs})
		}
		for pod := range s.h.pods {
			if v, ok := s.waitMetric(pod, "erpc_chain_tracker_final_hold_headers", nil, 30*time.Second, func(v float64) bool { return v > 0 }); !ok {
				s.violate(s.name, "pod-%d: hold gauge %v, want the headers below the window held", pod+1, v)
			}
		}
		s.blocks(60)
		s.reorg(2)
		s.blocks(60)
		for i, p := range ps {
			name := fmt.Sprintf("%s/pod-%d", s.name, i+1)
			a := s.applyFinalHeads(p.ec, p.heads)
			s.requireFinal(name+"/heads", a, chainsim.FinalizedLag)
			if len(a.undos) > 0 {
				s.violate(name+"/heads", "%d undos on a final tap", len(a.undos))
			}
			if n := s.requireFinalLogs(name+"/logs", p.ec, p.logs, 0); n > 0 {
				s.violate(name+"/logs", "%d undos on a final tap", n)
			}
			p.ec.close()
		}
		for pod := range s.h.pods {
			if v, ok := s.waitMetric(pod, "erpc_chain_tracker_final_hold_headers", nil, 30*time.Second, func(v float64) bool { return v == 0 }); !ok {
				s.violate(s.name, "pod-%d: hold gauge %v after the final taps closed, want 0", pod+1, v)
			}
		}
	}}
}

// F2: a depth:3 undo tap; reorgs 1–3 deep (C1) never reach it; one
// reorgWindow − 1 deep (C2) arrives as one undo; the client ends
// canonical.
func f2() spec {
	return spec{name: "F2", bound: deepReorgDelivery, allow: []string{"ws:logs-end", "ws:jump"}, body: func(s *scenario) {
		s.blocks(25)
		ec, heads := s.openFinal(0, s.name, "newHeads", erpcOpts("finality", "depth:3", "reorg", "undo"))
		if ec == nil {
			return
		}
		defer ec.close()
		logs, err := ec.subscribe("logs", chainsim.Filters[1].Params(), erpcOpts("finality", "depth:3", "reorg", "undo", "blockEnd", true))
		if err != nil {
			s.violate(s.name, "depth logs subscribe: %v", err)
			return
		}
		for range 6 {
			s.blocks(15)
			s.reorg(1 + rand.IntN(3))
		}
		s.blocks(15)
		if a := s.applyFinalHeads(ec, heads); len(a.undos) > 0 {
			s.violate(s.name, "%d undos from reorgs at most 3 deep on a depth:3 tap", len(a.undos))
		}
		s.reorg(reorgWindow - 1)
		s.blocks(40)
		a := s.applyFinalHeads(ec, heads)
		s.requireFinal(s.name+"/heads", a, 3)
		if len(a.undos) != 1 {
			s.violate(s.name+"/heads", "%d undos, want one for the %d-deep reorg", len(a.undos), reorgWindow-1)
		}
		if n := s.requireFinalLogs(s.name+"/logs", ec, logs, 1); n != 1 {
			s.violate(s.name+"/logs", "%d undos, want 1", n)
		}
	}}
}

// F3: reorg undo on ws heads and logs at the tip under C1: one undo per
// reorg with its fork point, never removed:true; final logs canonical.
func f3() spec {
	return spec{name: "F3", bound: settleDelivery, body: func(s *scenario) {
		s.blocks(25)
		ec, heads := s.openFinal(1, s.name, "newHeads", erpcOpts("reorg", "undo"))
		if ec == nil {
			return
		}
		defer ec.close()
		logs, err := ec.subscribe("logs", chainsim.Filters[0].Params(), erpcOpts("reorg", "undo", "blockEnd", true))
		if err != nil {
			s.violate(s.name, "undo logs subscribe: %v", err)
			return
		}
		const reorgs = 6
		for range reorgs {
			s.blocks(15)
			s.reorg(1 + rand.IntN(3))
		}
		s.blocks(25)
		a := s.applyFinalHeads(ec, heads)
		s.requireFinal(s.name+"/heads", a, 0)
		if len(a.undos) != reorgs {
			s.violate(s.name+"/heads", "%d undos for %d reorgs", len(a.undos), reorgs)
		}
		if n := s.requireFinalLogs(s.name+"/logs", ec, logs, 0); n != reorgs {
			s.violate(s.name+"/logs", "%d undos for %d reorgs", n, reorgs)
		}
	}}
}

// F4: reorg none without finalized is -32602; with finalized, C12's
// majority revert of a final block ends it final_reverted.
func f4() spec {
	return spec{name: "F4", cache: true, bound: deepReorgDelivery, allow: []string{"ws:logs-end", "ws:jump"}, body: func(s *scenario) {
		s.blocks(50)
		ec, _, err := s.dialExt(0, "", websocket.DefaultDialer)
		if err != nil {
			s.violate(s.name, "ws dial: %v", err)
			return
		}
		defer ec.close()
		if _, err := ec.subscribe("newHeads", erpcOpts("reorg", "none")); err == nil || !containsAll(err.Error(), "-32602", `"reorg"`) {
			s.violate(s.name, "reorg none with latest: %v, want -32602 naming reorg", err)
		}
		sub, err := ec.subscribe("newHeads", erpcOpts("finality", "finalized", "reorg", "none"))
		if err != nil {
			s.violate(s.name, "final none subscribe: %v", err)
			return
		}
		s.blocks(25)
		at := time.Now()
		revertFinalized(s)
		s.blocks(25)
		ends := ec.controls(sub, "end", at)
		if len(ends) != 1 || ends[0].msg.Reason != "final_reverted" {
			s.violate(s.name, "ends after the final revert %+v, want one final_reverted", ends)
		}
	}}
}

// F5: vendors' safe 8 below, finalized 64; http-b refuses safe. A safe
// tap gets blocks at or below Head.Safe, from the other votes; after it
// closes no safe poll reaches a vendor; when every vendor refuses, Safe
// is Finalized.
func f5() spec {
	faults := map[string]chainsim.Faults{}
	for _, v := range baseVendors {
		faults[v.key] = chainsim.Faults{SafeDepth: f5SafeDepth, FinalityDepth: f5FinalDepth}
	}
	faults["http-b"] = chainsim.Faults{SafeDepth: f5SafeDepth, FinalityDepth: f5FinalDepth, NoSafe: true}
	return spec{name: "F5", bound: healthyDelivery, faults: faults, body: func(s *scenario) {
		safeCalls := func() (n int64) {
			for _, v := range s.vendors {
				n += v.Stats().SafeCalls
			}
			return n
		}
		s.blocks(f5FinalDepth + 10)
		if n := safeCalls(); n > 0 {
			s.violate(s.name, "%d safe polls with no safe tap", n)
		}
		ec, sub := s.openFinal(0, s.name, "newHeads", erpcOpts("finality", "safe"))
		if ec == nil {
			return
		}
		s.blocks(50)
		a := s.applyFinalHeads(ec, sub)
		s.requireFinal(s.name, a, f5SafeDepth)
		above := 0
		for n, at := range a.at {
			if n+f5FinalDepth > s.tipAt(at) {
				above++
			}
		}
		if above == 0 {
			s.violate(s.name, "no safe head above Finalized: Safe is not the safe ballot")
		}
		if s.vendors["http-b"].Stats().SafeCalls == 0 {
			s.violate(s.name, "http-b, which refuses safe, was never asked")
		}
		ec.close()
		time.Sleep(2 * time.Second)
		polled := safeCalls()
		s.blocks(50)
		if n := safeCalls() - polled; n > 0 {
			s.violate(s.name, "%d safe polls after the last safe tap closed", n)
		}
		for _, v := range s.vendors {
			v.Update(func(f *chainsim.Faults) { f.NoSafe = true })
		}
		ec, sub = s.openFinal(1, s.name, "newHeads", erpcOpts("finality", "safe"))
		if ec == nil {
			return
		}
		defer ec.close()
		s.blocks(50)
		s.requireFinal(s.name+"/no-safe", s.applyFinalHeads(ec, sub), f5FinalDepth)
	}}
}

// F6: 0.2s blocks, finalized 4,000 below the head: a final tap fills down
// to Finalized and flows, and the hold gauge is back to 0 once it
// closes. Then 25,000 below: once Head.Finalized follows (the finalized
// poll's debounce grows with the finality depth), a final subscribe is
// refused, and an open final tap, whose cursor stays 4,000 below, keeps
// its place without an end: the hold follows cursors, not Finalized. The
// 5s window ends baseline gRPC resumes below_window, so F6 runs no
// baseline clients.
func f6() spec {
	faults := map[string]chainsim.Faults{}
	for _, v := range baseVendors {
		faults[v.key] = chainsim.Faults{FinalityDepth: f6Depth}
	}
	return spec{name: "F6", bound: healthyDelivery, evm: finalityEvm(), faults: faults, sim: chainsim.Config{History: f6Span + 1000},
		clients: func(*scenario) {}, allow: []string{"ws:logs-end", "ws:jump"}, body: func(s *scenario) {
			s.blocks(25)
			ec, sub := s.openFinal(0, s.name, "newHeads", erpcOpts("finality", "finalized"))
			if ec == nil {
				return
			}
			// The first final block waits for the fill down to Finalized
			// (8 fetches in flight, CT§9) and for Finalized to move.
			for deadline := time.Now().Add(10 * time.Minute); len(s.applyFinalHeads(ec, sub).chain) < 10 && time.Now().Before(deadline); {
				time.Sleep(time.Second)
			}
			if v, ok := s.metric(0, "erpc_chain_tracker_final_hold_headers", nil); !ok || v < f6Depth/2 {
				s.violate(s.name, "hold gauge %v while the final tap flows, want about %d headers", v, f6Depth)
			}
			s.blocks(50)
			s.requireFinal(s.name, s.applyFinalHeads(ec, sub), f6Depth)
			ec.close()
			if v, ok := s.waitMetric(0, "erpc_chain_tracker_final_hold_headers", nil, 30*time.Second, func(v float64) bool { return v == 0 }); !ok {
				s.violate(s.name, "hold gauge %v after the tap closed, want 0", v)
			}
			ec, sub = s.openFinal(1, s.name, "newHeads", erpcOpts("finality", "finalized"))
			if ec == nil {
				return
			}
			defer ec.close()
			for _, v := range s.vendors {
				v.Update(func(f *chainsim.Faults) { f.FinalityDepth = f6Span })
			}
			finalized := map[string]string{"lane": "all", "axis": "finalized"}
			if v, ok := s.waitMetric(1, "erpc_network_served_tip_block_number", finalized, 10*time.Minute, func(v float64) bool {
				return uint64(v)+f6Span-1000 < s.chain.Tip().Number
			}); !ok {
				s.violate(s.name, "Head.Finalized %v never fell %d below the tip %d", v, f6Span, s.chain.Tip().Number)
				return
			}
			s.blocks(25)
			if ends := ec.controls(sub, "end", time.Time{}); len(ends) != 0 {
				s.violate(s.name, "open final tap 4,000 below the window ends %+v, want no end", ends)
			}
			if _, err := ec.subscribe("newHeads", erpcOpts("finality", "finalized")); err == nil || !containsAll(err.Error(), "-32005", "finality_span") {
				s.violate(s.name, "final subscribe at a %d span: %v, want -32005 finality_span", f6Span, err)
			}
		}}
}

// F7: a use-upstream lane whose upstream trails the network head by
// f7Lag, depth:2: never above min(lane Latest, confirmed − 2).
func f7() spec {
	return spec{name: "F7", bound: healthyDelivery, faults: map[string]chainsim.Faults{"http-a": {Lag: f7Lag}}, body: func(s *scenario) {
		s.blocks(25)
		ec, _, err := s.dialExt(0, "?use-upstream="+s.upstream("http-a"), websocket.DefaultDialer)
		if err != nil {
			s.violate(s.name, "ws dial: %v", err)
			return
		}
		defer ec.close()
		sub, err := ec.subscribe("newHeads", erpcOpts("finality", "depth:2"))
		if err != nil {
			s.violate(s.name, "depth:2 subscribe on the lane: %v", err)
			return
		}
		s.blocks(60)
		s.requireFinal(s.name, s.applyFinalHeads(ec, sub), max(f7Lag, 2))
	}}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
