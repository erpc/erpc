package realworld

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// TestRealWorld runs the catalogue scenarios (review §3): network-scoped
// ones concurrently on shared pods, pod-scoped ones each in their own phase
// with fresh pods and redis. REALWORLD_ALONE=C1,X4 instead runs only the
// named scenarios, each alone in its own phase; REALWORLD_PHASES=shared,n4
// runs only the named phases.
func TestRealWorld(t *testing.T) {
	root, err := os.MkdirTemp("", "erpc-realworld-run-")
	if err != nil {
		t.Fatal(err)
	}
	r := &run{root: root, host: startHostLoad()}
	t.Cleanup(func() {
		r.host.stop()
		r.summary(t)
		if t.Failed() {
			fmt.Fprintf(os.Stderr, "realworld: run dir kept: %s\n", root)
			return
		}
		_ = os.RemoveAll(root)
	})
	phases := []struct {
		name  string
		chain int64
		specs []spec
	}{
		{"shared", 9101, []spec{x1(), c1(), c3(), u3(), u8(false), u8(true), u9(), n5(), k2(), d1(), d2()}},
		{"n4", 9201, []spec{n4()}},
		{"x4", 9301, []spec{x4()}},
		{"u3-lone", 9501, []spec{u3Lone()}},
		{"n4-lone", 9601, []spec{n4Lone()}},
		{"voters", 9701, []spec{n3(), u10(), u11(), u18(), k6()}},
		{"x9", 9801, []spec{x9()}},
		{"head", 9901, []spec{u12(), u14(), p1(), p5(), p6(), p10()}},
		{"finality", 10001, []spec{c12(), p7(), c13()}},
		{"polling", 10101, []spec{k7(true), k7(false), p9()}},
		{"x2", 10201, []spec{x2()}},
		{"c9", 10301, []spec{c9()}},
	}
	if alone := os.Getenv("REALWORLD_ALONE"); alone != "" {
		known := map[string]spec{}
		for _, ph := range phases {
			for _, sp := range ph.specs {
				known[sp.name] = sp
			}
		}
		for i, name := range strings.Split(alone, ",") {
			sp, ok := known[name]
			if !ok {
				t.Fatalf("REALWORLD_ALONE names %q, no scenario", name)
			}
			r.phase(t, "alone-"+name, 9401+int64(i), []spec{sp})
		}
		return
	}
	only := os.Getenv("REALWORLD_PHASES")
	for _, ph := range phases {
		if only == "" || slices.Contains(strings.Split(only, ","), ph.name) {
			r.phase(t, ph.name, ph.chain, ph.specs)
		}
	}
}

// reconnectSlack is harness time, not spec: a scrape, a dial, a backoff
// timer firing late under -race.
const reconnectSlack = 5 * time.Second

// X1: quiet chain with 1–3 deep reorgs; the oracle holds for every block on
// both pods.
func x1() spec {
	return spec{name: "X1", bound: healthyDelivery, body: func(s *scenario) {
		for end := time.Now().Add(60 * time.Second); time.Now().Before(end); {
			s.blocks(40 + rand.IntN(35))
			s.reorg(1 + rand.IntN(3))
		}
		s.blocks(25)
	}}
}

// C1: reorgs of 1–3 blocks at the tip. Every client that received a
// replaced block is told: newHeads and gRPC get the new branch from the
// fork point + 1, logs get removed:true for every log of the old blocks.
func c1() spec {
	return spec{name: "C1", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		for range 12 {
			s.blocks(15)
			s.reorg(1 + rand.IntN(3))
		}
		s.blocks(25)
		s.requireReorgsTold()
	}}
}

// requireReorgsTold: each header client that received a block a reorg
// replaced got a header at the fork point + 1 after the reorg; each logs
// client holds none of the logs of replaced blocks.
func (s *scenario) requireReorgsTold() {
	s.mu.Lock()
	reorgs, hs, grpcs, logs := s.reorgs, s.heads, s.grpcs, s.logs
	s.mu.Unlock()
	for _, r := range reorgs {
		old := map[hash]bool{}
		for _, b := range r.old {
			old[b.Hash] = true
		}
		first := r.old[0].Number
		check := func(c *clientBase, reps []replacement) {
			got := false
			for _, d := range c.deliveries {
				got = got || old[d.hash]
			}
			if !got {
				return
			}
			for _, rep := range reps {
				if rep.number == first && rep.at.After(r.at) {
					return
				}
			}
			s.violate(c.name, "received a block of the branch the reorg at %s replaced, but no header at its fork point + 1 (%d) after it", r.at.Format("15:04:05.000"), first)
		}
		for _, c := range hs {
			c.mu.Lock()
			check(&c.clientBase, c.replacements)
			c.mu.Unlock()
		}
		for _, g := range grpcs {
			g.mu.Lock()
			check(&g.clientBase, g.replace)
			g.mu.Unlock()
		}
		for _, c := range logs {
			c.mu.Lock()
			for k, l := range c.live {
				if old[k.BlockHash] {
					s.violate(c.name, "holds log %d of block %d, which the reorg at %s replaced, without removed:true", k.LogIndex, l.number, r.at.Format("15:04:05.000"))
				}
			}
			c.mu.Unlock()
		}
	}
}

// C3: a reorg reorgWindow+4 deep. Trackers re-anchor, and fill joins the
// new branch to the fork point, which the window still holds (maxBackfill
// ≫ the depth): newHeads and gRPC clients get the new branch from the fork
// point + 1, and every ws logs tap, more than reorgWindow behind the logs
// head, ends with 1013 within the close spread.
func c3() spec {
	return spec{name: "C3", bound: deepReorgDelivery, allow: []string{"ws:logs-end"}, body: func(s *scenario) {
		s.blocks(50)
		var before [2]float64
		for pod := range s.h.pods {
			before[pod], _ = s.metric(pod, "erpc_chain_tracker_reanchor_total", nil)
		}
		r := s.reorg(reorgWindow + 4)
		time.Sleep(logsCloseSpread + reconnectSlack)
		for pod := range s.h.pods {
			if v, ok := s.waitMetric(pod, "erpc_chain_tracker_reanchor_total", nil, reconnectSlack, func(v float64) bool { return v > before[pod] }); !ok {
				s.violate("C3", "pod-%d: erpc_chain_tracker_reanchor_total stayed %v after a %d-deep reorg", pod+1, v, reorgWindow+4)
			}
		}
		for _, c := range s.logs {
			if !c.endedWith(r.at, r.at.Add(logsCloseSpread+reconnectSlack), func(e clientEnd) bool { return e.server && closedWith(e.reason, 1013) }) {
				s.violate(c.name, "no 1013 close within %s of the %d-deep reorg", logsCloseSpread, reorgWindow+4)
			}
		}
		s.requireReorgsTold()
		s.blocks(25)
	}}
}

// endedWith reports whether an end in [from, to] satisfies ok.
func (c *clientBase) endedWith(from, to time.Time, ok func(clientEnd) bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.ends {
		if !e.at.Before(from) && !e.at.After(to) && ok(e) {
			return true
		}
	}
	return false
}

// deliveredOn reports whether the client received a block on pod after t.
func (c *clientBase) deliveredOn(pod int, after time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.deliveries {
		if d.pod == pod && d.at.After(after) {
			return true
		}
	}
	return false
}

// U3: pod-1's TCP path to ws-a goes half-open: the proxy forwards nothing
// either way and keeps both sockets. ws-a stops being live on pod-1 within
// the live bound and pod-1 reconnects it within 30s; the other vendors
// carry every client.
func u3() spec {
	return spec{name: "U3", bound: healthyDelivery, body: halfOpenWsA}
}

// U3-lone: U3 on a single-instance deployment (one pod, shared state in
// memory), where only the dead socket itself can report ws-a's head.
func u3Lone() spec {
	return spec{name: "U3-lone", pods: 1, memory: true, bound: healthyDelivery, body: halfOpenWsA}
}

func halfOpenWsA(s *scenario) {
	s.blocks(40)
	p1, px, up := s.h.pods[0], s.proxies["ws-a"], s.upstream("ws-a")
	acc := px.accepts(p1)
	px.set(p1, linkFaults{BlackHole: true})
	cut := time.Now()
	if _, ok := s.waitMetric(0, "erpc_source_live", map[string]string{"upstream": up}, liveBound+blockTime+scrapeEvery, func(v float64) bool { return v == 0 }); !ok {
		s.violate(s.name, "pod-1: %s still live %s after its socket went half-open", up, liveBound+blockTime+scrapeEvery)
	}
	for px.accepts(p1) == acc && time.Since(cut) < silentReconnect+reconnectSlack {
		time.Sleep(100 * time.Millisecond)
	}
	if px.accepts(p1) == acc {
		s.violate(s.name, "pod-1: no reconnect to %s within %s of the half-open socket", up, silentReconnect+reconnectSlack)
	} else {
		s.logf("pod-1 reconnected %s after %s", up, time.Since(cut).Round(100*time.Millisecond))
	}
	px.set(p1, linkFaults{})
	if _, ok := s.waitMetric(0, "erpc_source_live", map[string]string{"upstream": up}, silentReconnect+reconnectSlack, func(v float64) bool { return v == 1 }); !ok {
		s.violate(s.name, "pod-1: %s not live again within %s of the path healing", up, silentReconnect+reconnectSlack)
	}
	s.blocks(25)
}

// U8: ws-a pushes headers whose hash the header does not hash to, for 100
// blocks. No client ever receives one (the oracle rejects unknown hashes).
// With integrity on, intake drops them and counts the violations on ws-a
// only. With integrity off, ws-b registers after the network started: its
// heads vote from the first one, so ws-a's never stand alone.
func u8(integrity bool) spec {
	name := "U8-off"
	var late []string
	if integrity {
		name = "U8-on"
	} else {
		late = []string{"ws-b"}
	}
	return spec{name: name, integrity: integrity, late: late, bound: settleDelivery, body: func(s *scenario) {
		s.blocks(25)
		ws := s.vendors["ws-a"]
		ws.Update(func(f *chainsim.Faults) { f.WrongHashes = true })
		s.blocks(100)
		ws.Update(func(f *chainsim.Faults) { f.WrongHashes = false })
		s.blocks(25)
		if !integrity {
			return
		}
		up := s.upstream("ws-a")
		for pod := range s.h.pods {
			if v, _ := s.metric(pod, "erpc_chain_tracker_source_heads_total", map[string]string{"upstream": up, "outcome": "gated_integrity"}); v == 0 {
				s.violate(name, "pod-%d: no %s header counted gated_integrity", pod+1, up)
			}
			mine, _ := s.metric(pod, "erpc_integrity_violation_total", map[string]string{"upstream": up})
			all, _ := s.metric(pod, "erpc_integrity_violation_total", nil)
			if mine == 0 {
				s.violate(name, "pod-%d: erpc_integrity_violation_total counts nothing on %s", pod+1, up)
			}
			if all > mine {
				s.violate(name, "pod-%d: erpc_integrity_violation_total counts %v on upstreams other than %s", pod+1, all-mine, up)
			}
		}
	}}
}

// u9VendorBLatency is harness, not spec: enough for the selection policy
// to rank vendor-b's upstreams below vendor-a's.
const u9VendorBLatency = 100 * time.Millisecond

// U9: vendor-a (ws-a and http-a) serves truncated receipts, then a forged
// log, then a zero bloom, 50 blocks each, integrity on. Receipts are
// refetched from vendor-b: every client's logs equal the canonical logs,
// no forged log reaches a client, and violations count on vendor-a only.
// vendor-b's links run u9VendorBLatency slower throughout, so the
// selection policy asks vendor-a first and its faults are exercised.
func u9() spec {
	return spec{name: "U9", integrity: true, bound: healthyDelivery, body: func(s *scenario) {
		for _, key := range []string{"ws-b", "http-b"} {
			for _, p := range s.h.pods {
				s.proxies[key].set(p, linkFaults{Latency: u9VendorBLatency})
			}
		}
		s.blocks(25)
		va := []*chainsim.Vendor{s.vendors["ws-a"], s.vendors["http-a"]}
		vb := []*chainsim.Vendor{s.vendors["ws-b"], s.vendors["http-b"]}
		calls := func(vs []*chainsim.Vendor) int64 { return vs[0].Stats().ReceiptsCalls + vs[1].Stats().ReceiptsCalls }
		var during, duringB int64
		for _, fault := range []func(*chainsim.Faults, bool){
			func(f *chainsim.Faults, on bool) { f.TruncReceipts = on },
			func(f *chainsim.Faults, on bool) { f.ForgedLog = on },
			func(f *chainsim.Faults, on bool) { f.WrongBloom = on },
		} {
			c0, b0 := calls(va), calls(vb)
			for _, v := range va {
				v.Update(func(f *chainsim.Faults) { fault(f, true) })
			}
			s.blocks(50)
			for _, v := range va {
				v.Update(func(f *chainsim.Faults) { fault(f, false) })
			}
			during += calls(va) - c0
			duringB += calls(vb) - b0
			s.blocks(10)
		}
		s.logf("under vendor-a's faults, vendor-a answered %d receipts calls, vendor-b %d", during, duringB)
		if during == 0 {
			s.violate("U9", "no receipts call reached vendor-a during its faults: nothing exercised them")
		}
		var mine, other float64
		for pod := range s.h.pods {
			for _, key := range []string{"ws-a", "http-a"} {
				v, _ := s.metric(pod, "erpc_integrity_violation_total", map[string]string{"upstream": s.upstream(key)})
				mine += v
			}
			for _, key := range []string{"ws-b", "http-b"} {
				v, _ := s.metric(pod, "erpc_integrity_violation_total", map[string]string{"upstream": s.upstream(key)})
				other += v
			}
		}
		if during > 0 && mine == 0 {
			s.violate("U9", "vendor-a answered %d faulty receipts calls; erpc_integrity_violation_total counts none on its upstreams", during)
		}
		if other > 0 {
			s.violate("U9", "erpc_integrity_violation_total counts %v on vendor-b, which served no fault", other)
		}
		s.blocks(25)
	}}
}

// N5: the network closes every socket to ws-a every 2 blocks, for 30
// blocks. Pods reconnect with backoff of at least 1s, subscribe once per
// connection, and the other vendors carry every client.
func n5() spec {
	return spec{name: "N5", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		px, v, up := s.proxies["ws-a"], s.vendors["ws-a"], s.upstream("ws-a")
		var acc [2]int64
		for i, p := range s.h.pods {
			acc[i] = px.accepts(p)
		}
		subs := v.Stats().HeadsSubscribes
		start := time.Now()
		for range 15 {
			for _, p := range s.h.pods {
				px.closeAll(p)
			}
			s.blocks(2)
		}
		window := time.Since(start)
		var total int64
		for i, p := range s.h.pods {
			n := px.accepts(p) - acc[i]
			total += n
			limit := int64(window/minReconnectBackoff) + 1
			s.logf("pod-%d opened %d connections to %s in %s", i+1, n, up, window.Round(100*time.Millisecond))
			if n > limit {
				s.violate("N5", "pod-%d: %d connections to %s in %s, above %d at a 1s minimum backoff", i+1, n, up, window.Round(100*time.Millisecond), limit)
			}
		}
		if d := v.Stats().HeadsSubscribes - subs; d > total {
			s.violate("N5", "%d newHeads subscribes on %d connections to %s", d, total, up)
		}
		for pod := range s.h.pods {
			if _, ok := s.waitMetric(pod, "erpc_source_live", map[string]string{"upstream": up}, silentReconnect+reconnectSlack, func(v float64) bool { return v == 1 }); !ok {
				s.violate("N5", "pod-%d: %s not live again within %s of the last close", pod+1, up, silentReconnect+reconnectSlack)
			}
		}
		s.blocks(25)
	}}
}

// K2: a minimal config, 1 wss and 1 https upstream with no evm block and no
// network entry. The wss upstream is a source, the https one is not, the
// served tip needs both (quorum 2), and every subscription works. The
// network-wide `latest` is floored at the delivered head (feature.md: "the
// network-wide tracker's pin changes floor it"), and the lag gauge reads 0
// at quorum ≤ 2, so the quorum shows on a use-upstream lane over both.
func k2() spec {
	return spec{name: "K2", project: "k2", minimal: true, bound: healthyDelivery,
		vendors: []vendorSpec{{"ws-a", "vendor-a", true}, {"http-b", "vendor-b", false}},
		body: func(s *scenario) {
			s.blocks(25)
			ws, http := s.upstream("ws-a"), s.upstream("http-b")
			for pod := range s.h.pods {
				if v, _ := s.metric(pod, "erpc_chain_tracker_source_heads_total", map[string]string{"upstream": ws, "outcome": "voted"}); v == 0 {
					s.violate("K2", "pod-%d: %s, a wss upstream with subscriptionSource unset, voted no header", pod+1, ws)
				}
				if v, _ := s.metric(pod, "erpc_chain_tracker_source_heads_total", map[string]string{"upstream": http}); v > 0 {
					s.violate("K2", "pod-%d: %s, an https upstream with subscriptionSource unset, fed %v source headers", pod+1, http, v)
				}
				if v, _ := s.metric(pod, "erpc_source_live", map[string]string{"upstream": http}); v > 0 {
					s.violate("K2", "pod-%d: %s counted as a live source", pod+1, http)
				}
			}
			hb := s.vendors["http-b"]
			hb.Update(func(f *chainsim.Faults) { f.Lag = 5 })
			for pod, p := range s.h.pods {
				c, err := rpc.DialOptions(s.ctx, p.httpURL(s.project, s.chain.ID()), rpc.WithHeader("X-ERPC-Use-Upstream", "k2-*"))
				if err != nil {
					s.t.Fatal(err)
				}
				var bn hexutil.Uint64
				var behind uint64
				for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(scrapeEvery) {
					ctx, cancel := context.WithTimeout(s.ctx, httpCheckWait)
					err = c.CallContext(ctx, &bn, "eth_blockNumber")
					cancel()
					if tip := s.chain.Tip().Number; err == nil && tip > uint64(bn) {
						behind = tip - uint64(bn)
					}
					if behind >= 3 {
						break
					}
				}
				c.Close()
				if behind < 3 {
					s.violate("K2", "pod-%d: eth_blockNumber over both upstreams %d (err %v), %d behind the tip while %s is 5 blocks behind: the quorum is not 2", pod+1, bn, err, behind, http)
				}
			}
			hb.Update(func(f *chainsim.Faults) { f.Lag = 0 })
			s.blocks(25)
		}}
}

// slowHeaderPad and slowPauseBlocks: D1's header size, and how long its
// ws client stops reading. Loopback TCP holds up to slowKernelHeld bytes
// between eRPC's write and the client's read whatever SO_RCVBUF says
// (macOS autosizes both buffers: 479 KiB measured with a 4 KiB SO_RCVBUF),
// so the tap falls behind only once those are full. The pause covers
// them, then reorgWindow blocks, then 4 more, and stays inside the 10s ws
// write deadline (erpc/ws_server.go wsWriteDeadline).
const (
	slowHeaderPad   = 64 << 10
	slowKernelHeld  = 512 << 10
	slowPauseBlocks = slowKernelHeld/slowHeaderPad + reorgWindow + 4
)

// D1: on a chain with 64 KiB headers, a ws newHeads client stops reading
// for slowPauseBlocks and a gRPC client for twice the send wait. The
// newHeads tap restarts past reorgWindow, the stream ends UNAVAILABLE after
// its send wait, and no other tap waits.
func d1() spec {
	return spec{name: "D1", sim: chainsim.Config{HeaderPad: slowHeaderPad}, bound: healthyDelivery, body: func(s *scenario) {
		sh := s.addHeads("p1-heads-slow", 0, true, "ws:jump")
		sg := s.addGrpc("p1-grpc-slow", 0, 0, true, "grpc:Unavailable")
		s.blocks(25)
		lag := map[string]string{"transport": "ws", "kind": "newHeads", "reason": "lag"}
		lag0, _ := s.metric(0, "erpc_tap_out_of_range_total", lag)
		at := time.Now()
		sh.pause(slowPauseBlocks * blockTime)
		sg.pause(2 * grpcSendWait)
		time.Sleep(2*grpcSendWait + reconnectSlack)
		sh.mu.Lock()
		restarted := false
		for _, j := range sh.jumps {
			restarted = restarted || (!j.newConn && j.at.After(at) && j.to-j.from > reorgWindow)
		}
		sh.mu.Unlock()
		if !restarted {
			s.violate(sh.name, "no restart past reorgWindow after it stopped reading for %d blocks", slowPauseBlocks)
		}
		if v, _ := s.metric(0, "erpc_tap_out_of_range_total", lag); v <= lag0 {
			s.violate(sh.name, "pod-1: erpc_tap_out_of_range_total{reason=lag} stayed %v", v)
		}
		if !sg.endedWith(at, time.Now(), func(e clientEnd) bool { return e.server && strings.HasPrefix(e.reason, "UNAVAILABLE") }) {
			s.violate(sg.name, "StreamBlocks did not end UNAVAILABLE while the client read nothing for %s", 2*grpcSendWait)
		}
		s.blocks(25)
	}}
}

// D2: every ws client reconnects every 10 blocks. A new newHeads
// subscription starts after the head (a jump in numbers, never a repeat);
// every logs subscription gets logs.
func d2() spec {
	return spec{name: "D2", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(10)
		for end := time.Now().Add(40 * time.Second); time.Now().Before(end); {
			s.blocks(10)
			for _, c := range s.heads {
				nudge(&c.clientBase)
			}
			for _, c := range s.logs {
				nudge(&c.clientBase)
			}
		}
		s.blocks(25)
		for _, c := range s.heads {
			c.mu.Lock()
			n := 0
			for _, j := range c.jumps {
				if !j.newConn {
					continue
				}
				n++
				if j.to <= j.from {
					s.violate(c.name, "a new subscription started at %d, at or below the %d the client had", j.to, j.from)
				}
			}
			c.mu.Unlock()
			if n < 3 {
				s.violate(c.name, "only %d new subscriptions in 40s of reconnects", n)
			}
		}
		for _, c := range s.logs {
			c.mu.Lock()
			for i, lc := range c.conns {
				if !lc.open && lc.closed.Sub(lc.opened) >= 8*blockTime && lc.highest == 0 {
					s.violate(c.name, "subscription %d lasted %s and received no log", i, lc.closed.Sub(lc.opened).Round(time.Millisecond))
				}
			}
			c.mu.Unlock()
		}
	}}
}

// nudge asks a client to reconnect, unless it is already about to.
func nudge(c *clientBase) {
	select {
	case c.reconnect <- struct{}{}:
	default:
	}
}

// N4 (+D3): pod-1 is cut from every vendor for 2 minutes, redis up. Each
// gRPC stream on pod-1 ends UNAVAILABLE within the read wait and resumes on
// pod-2 from its last header with no gap; pod-2's clients never wait.
func n4() spec {
	return spec{name: "N4", bound: healthyDelivery, allow: []string{"grpc:Unavailable@pod-1", "ws:jump@pod-1", "ws:logs-end@pod-1"}, body: cutPod1}
}

// N4-lone: N4 on a single-instance deployment (one pod, shared state in
// memory): no peer tells the cut pod the chain moved on. Its streams still
// end UNAVAILABLE within the read wait and resume on it once healed.
func n4Lone() spec {
	return spec{name: "N4-lone", pods: 1, memory: true, bound: healthyDelivery, allow: []string{"grpc:Unavailable@pod-1", "ws:jump@pod-1", "ws:logs-end@pod-1"}, body: cutPod1}
}

func cutPod1(s *scenario) {
	s.blocks(50)
	p1 := s.h.pods[0]
	resumeOn := 1 % len(s.h.pods)
	var onP1 []*grpcClient
	for _, g := range s.grpcs {
		if g.currentPod() == 0 {
			onP1 = append(onP1, g)
		}
	}
	ex := s.excuse(0, time.Now())
	for _, px := range s.proxies {
		px.set(p1, linkFaults{BlackHole: true})
	}
	cut := time.Now()
	s.logf("pod-1 cut from every vendor; %d gRPC streams on it", len(onP1))
	time.Sleep(2 * time.Minute)
	for _, px := range s.proxies {
		px.set(p1, linkFaults{})
	}
	lift := time.Now()
	s.logf("pod-1 path healed")
	// A dropped wss source reconnects within 30s (§13.1).
	s.endExcuse(ex, lift.Add(silentReconnect))
	// pod-1's logs taps, out of range at the heal, close with 1013 up to
	// logsCloseSpread later; their clients then need blocks on pod-2 to
	// cover.
	time.Sleep(max(silentReconnect, logsCloseSpread+reconnectSlack) + 10*time.Second)
	for _, g := range onP1 {
		var end *clientEnd
		g.mu.Lock()
		for i, e := range g.ends {
			if e.at.After(cut) && e.pod == 0 {
				end = &g.ends[i]
				break
			}
		}
		resumes := g.resumes
		g.mu.Unlock()
		switch {
		case end == nil:
			s.violate(g.name, "StreamBlocks on the cut pod-1 never ended")
		case !end.server || !strings.HasPrefix(end.reason, "UNAVAILABLE"):
			s.violate(g.name, "StreamBlocks on the cut pod-1 ended %q, not UNAVAILABLE", end.reason)
		case end.at.Sub(cut) > grpcReadWait+reconnectSlack:
			s.violate(g.name, "StreamBlocks on the cut pod-1 ended UNAVAILABLE %s after the cut, above the %s read wait", end.at.Sub(cut).Round(time.Second), grpcReadWait)
		case !g.deliveredOn(resumeOn, end.at):
			s.violate(g.name, "did not resume on pod-%d after UNAVAILABLE", resumeOn+1)
		}
		if resumes == 0 {
			s.violate(g.name, "never resumed from a cursor")
		}
	}
}

// X4: pod-1 is killed with SIGKILL during a 3-block reorg. Its clients
// resume on pod-2 and end canonical; pod-1 restarts and serves again.
func x4() spec {
	return spec{name: "X4", bound: healthyDelivery, allow: []string{"grpc:Unavailable@pod-1", "ws:heads-end@pod-1", "ws:logs-end@pod-1"}, body: func(s *scenario) {
		s.blocks(50)
		p1 := s.h.pods[0]
		var onP1 []*clientBase
		for _, c := range s.heads {
			if c.currentPod() == 0 {
				onP1 = append(onP1, &c.clientBase)
			}
		}
		for _, c := range s.logs {
			if c.currentPod() == 0 {
				onP1 = append(onP1, &c.clientBase)
			}
		}
		for _, g := range s.grpcs {
			if g.currentPod() == 0 {
				onP1 = append(onP1, &g.clientBase)
			}
		}
		ex := s.excuse(0, time.Now())
		s.reorg(3)
		// Clients see the reset while kill waits for the process to exit.
		killed := time.Now()
		p1.kill()
		s.logf("pod-1 killed with %d clients on it", len(onP1))
		s.blocks(75)
		if err := p1.start(); err != nil {
			s.t.Fatalf("pod-1 restart: %v", err)
		}
		s.waitServing(0)
		s.endExcuse(ex, time.Now().Add(liveBound))
		s.logf("pod-1 restarted and serving")
		s.blocks(100)
		for _, c := range onP1 {
			if !c.endedWith(killed, time.Now(), func(e clientEnd) bool { return e.server && e.pod == 0 }) {
				s.violate(c.name, "saw no end when pod-1 was killed")
			}
			if !c.deliveredOn(1, killed) {
				s.violate(c.name, "received nothing on pod-2 after pod-1 was killed")
			}
		}
	}}
}
