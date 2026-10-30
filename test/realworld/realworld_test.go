package realworld

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// TestRealWorld runs the catalogue scenarios (review §3): network-scoped
// ones concurrently on shared pods, pod-scoped ones each in their own phase
// with fresh pods and redis (selectedPhases). Phases run concurrently, up to
// phaseLimit at once, each on its own ports, pods, redis and simulators.
// The run waits for a quiet host before its first phase;
// REALWORLD_LOADED_HOST=1 starts it at any load, and every latency verdict
// taken above the limit still fails, labelled "host overloaded".
func TestRealWorld(t *testing.T) {
	phases, skipped, err := selectedPhases()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("", "erpc-realworld-run-")
	if err != nil {
		t.Fatal(err)
	}
	r := &run{root: root, host: startHostLoad()}
	for _, ph := range skipped {
		for _, sp := range ph.specs {
			tier := "full tier: make test-realworld runs it"
			if sp.long {
				tier = "long tier: make test-realworld-long runs it"
			}
			r.rows = append(r.rows, summaryRow{phase: ph.name, name: sp.name, result: "skip", load: tier})
		}
	}
	t.Cleanup(func() {
		r.host.stop()
		r.summary(t)
		if t.Failed() {
			fmt.Fprintf(os.Stderr, "realworld: run dir kept: %s\n", root)
			return
		}
		_ = os.RemoveAll(root)
	})
	if err := r.host.waitQuiet(t.Logf); err != nil {
		t.Fatal(err)
	}
	limit, why := phaseLimit()
	t.Logf("%d phases, at most %d at once (%s)", len(phases), limit, why)
	slots := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, ph := range phases {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			r.phase(t, ph.name, ph.chain, ph.specs)
		}()
	}
	wg.Wait()
}

// phaseSpec is one phase: its scenarios share fresh pods and redis, each on
// its own chain id from chain.
type phaseSpec struct {
	name  string
	chain int64
	specs []spec
}

// catalogue is every phase of the suite, longest first, so the phases that
// set the run's wall start at once.
func catalogue() []phaseSpec {
	return []phaseSpec{
		{"c17", 12501, []spec{c17()}},
		{"p12", 12801, []spec{p12()}},
		{"head", 9901, []spec{u12(), u14(), u19(), p1(), p5(), p6(), p10()}},
		{"n4-lone", 9601, []spec{n4Lone()}},
		{"n4", 9201, []spec{n4()}},
		{"chain", 11501, []spec{c2(), c6(), c8(), c10(), c11(), c14()}},
		{"finality", 10001, []spec{c12(), p7(), c13()}},
		{"pruned", 12701, []spec{u20(true), u20(false)}},
		{"n6", 11001, []spec{n6()}},
		{"c16", 12401, []spec{c16()}},
		{"voters", 9701, []spec{n3(), u10(), u11(), u18(), k6()}},
		{"shared", 9101, []spec{x1(), c1(), c3(), u3(), n5(), k2(), d1(), d2()}},
		{"r1", 13201, []spec{r1()}},
		{"upstream", 11301, []spec{u2(), u4(), u5(), u6(), u7(), u13(), u15()}},
		{"clients", 11601, []spec{d3(), d5(), d6(), d9()}},
		{"integrity", 10601, []spec{u8(false), u8(true), u9(), u9Trunc(1), u9Trunc(2), u9Bloom(1), u9Bloom(2), k5()}},
		{"fetch", 10401, []spec{u1(), c4(), c7(), d8()}},
		{"fetch-vars", 10501, []spec{u16(), u17()}},
		{"c15", 11551, []spec{c15()}},
		{"consistency", 12301, []spec{p2(), p3(), p4(), p8()}},
		{"counters", 10801, []spec{c5()}},
		{"config", 12201, []spec{k3(64), k3(32), k4()}},
		{"x7", 11101, []spec{x7()}},
		{"x4", 9301, []spec{x4()}},
		{"polling", 10101, []spec{k7(true), k7(false), p9()}},
		{"tagstate", 10701, []spec{p11()}},
		{"x5", 10901, []spec{x5()}},
		{"c18", 12601, []spec{c18()}},
		{"u3-lone", 9501, []spec{u3Lone()}},
		{"x9", 9801, []spec{x9()}},
		{"x2", 10201, []spec{x2()}},
		{"c9", 10301, []spec{c9()}},
		{"network", 11401, []spec{n1(), n2(), n7()}},
		{"nitro", 10751, []spec{p11Nitro()}},
		{"x8", 11201, []spec{x8()}},
		{"d4", 11701, []spec{d4()}},
		{"d7", 11801, []spec{d7()}},
		{"x3", 11901, []spec{x3()}},
		{"x6", 12001, []spec{x6()}},
		{"k1", 12101, []spec{k1()}},
		{"p13", 12901, []spec{p13()}},
		{"z1", 13001, []spec{z1()}},
		{"z2", 13051, []spec{z2()}},
		{"z3", 13101, []spec{z3()}},
		{"streams", 13201, []spec{w1(), w2(), w3(), w4(), w5(), w6(), w7(), w8()}},
		{"w9", 13301, []spec{w9()}},
		{"streams-final", 13401, []spec{f1(), f2(), f3(), f4(), f5(), f7()}},
		{"f6", 13501, []spec{f6()}},
	}
}

// fastScenarios are the fast tier: one or two scenarios per spec area,
// each run in its catalogue phase beside the phase's other fast ones.
var fastScenarios = []string{
	"P12",   // near-future reads above every vendor's head (§6.4)
	"N4",    // pod cut from every vendor 2 min; gRPC resume on the peer (§9.2, §10.3)
	"R1",    // read at once at a delivered block, on either pod (§19 oracle)
	"D1",    // slow ws and gRPC readers: tap restart, send wait (§9.2)
	"C1",    // tip reorgs told to newHeads, logs, gRPC (§10.2)
	"X4",    // pod SIGKILL during a reorg; clients resume on the peer
	"P1",    // head latency: catch-up asks, no stall (§6.8)
	"C7",    // halt then 100-block burst: fetch and receipts catch-up (§8)
	"C18",   // finality: final_hash_changed only on own evidence (§6.5)
	"U8-on", // integrity: forged header hashes dropped and counted (§7)
	"U3",    // half-open wss voter reconnects within 30 s (§13.1)
	"K2",    // minimal config: 1 wss + 1 https, quorum 2 (§6.3)
	"D5",    // subscribe/unsubscribe churn: no send after unsubscribe, taps drain (§17)
	"N3",    // voters: a vendor cut leaves the voter set (§6)
	"U18",   // minority fork answers only within W (§6.3)
}

// selectedPhases are the phases this run runs, and the phases its tier
// leaves out: REALWORLD_ALONE=C1,X4 runs only the named scenarios, each
// alone in its own phase; REALWORLD_PHASES=shared,n4 runs only the named
// phases; else REALWORLD_TIER picks: fast (fastScenarios, each alone),
// full (default: every phase but the long ones) or long (only those).
func selectedPhases() (phases, skipped []phaseSpec, err error) {
	all := catalogue()
	alone := func(names []string) ([]phaseSpec, error) {
		known := map[string]spec{}
		for _, ph := range all {
			for _, sp := range ph.specs {
				known[sp.name] = sp
			}
		}
		var out []phaseSpec
		for i, name := range names {
			sp, ok := known[name]
			if !ok {
				return nil, fmt.Errorf("no scenario %q", name)
			}
			// The index keeps a scenario named twice in its own run dir.
			out = append(out, phaseSpec{fmt.Sprintf("alone-%d-%s", i+1, name), 9401 + int64(i), []spec{sp}})
		}
		return out, nil
	}
	if names := os.Getenv("REALWORLD_ALONE"); names != "" {
		phases, err = alone(strings.Split(names, ","))
		return phases, nil, err
	}
	if only := os.Getenv("REALWORLD_PHASES"); only != "" {
		for _, ph := range all {
			if slices.Contains(strings.Split(only, ","), ph.name) {
				phases = append(phases, ph)
			}
		}
		return phases, nil, nil
	}
	switch tier := cmp.Or(os.Getenv("REALWORLD_TIER"), "full"); tier {
	case "fast":
		for _, ph := range all {
			var in, out []spec
			for _, sp := range ph.specs {
				if slices.Contains(fastScenarios, sp.name) {
					in = append(in, sp)
				} else {
					out = append(out, sp)
				}
			}
			// A phase's pods follow its first spec; every phase holding a
			// fast scenario boots default pods, so a subset boots the same.
			if len(in) > 0 {
				phases = append(phases, phaseSpec{ph.name, ph.chain, in})
			}
			if len(out) > 0 {
				skipped = append(skipped, phaseSpec{ph.name, ph.chain, out})
			}
		}
		if n := slices.IndexFunc(fastScenarios, func(name string) bool {
			return !slices.ContainsFunc(phases, func(ph phaseSpec) bool {
				return slices.ContainsFunc(ph.specs, func(sp spec) bool { return sp.name == name })
			})
		}); n >= 0 {
			return nil, nil, fmt.Errorf("fastScenarios names %q, no scenario", fastScenarios[n])
		}
	case "full", "long":
		for _, ph := range all {
			if ph.specs[0].long == (tier == "long") {
				phases = append(phases, ph)
			} else {
				skipped = append(skipped, ph)
			}
		}
	default:
		return nil, nil, fmt.Errorf("REALWORLD_TIER is %q: fast, full or long", tier)
	}
	return phases, skipped, nil
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
// either way and keeps both sockets. ws-a's hash vote ages past W on pod-1
// and pod-1 reconnects it within 30s; the other vendors carry every
// client.
func u3() spec {
	return spec{name: "U3", bound: healthyDelivery, body: halfOpenWsA}
}

// U3-lone: U3 on a single-instance deployment (one pod, shared state in
// memory), where only the dead socket itself can report ws-a's head.
func u3Lone() spec {
	return spec{name: "U3-lone", pods: 1, memory: true, bound: healthyDelivery, body: halfOpenWsA}
}

// hashAge labels an upstream's hash-age gauge (spec §17).
func hashAge(up string) map[string]string {
	return map[string]string{"upstream": up, "evidence": "hash"}
}

func halfOpenWsA(s *scenario) {
	s.blocks(40)
	p1, px, up := s.h.pods[0], s.proxies["ws-a"], s.upstream("ws-a")
	acc := px.accepts(p1)
	px.set(p1, linkFaults{BlackHole: true})
	cut := time.Now()
	// §19 U3: the vote expires W after another voter's next header, about
	// a block after the cut; the gauge shows it at its next refresh.
	expiry := liveBound + blockTime + ageGaugeEvery + scrapeEvery
	if _, ok := s.waitMetric(0, "erpc_upstream_report_age_seconds", hashAge(up), expiry, func(v float64) bool { return v > liveBound.Seconds() }); !ok {
		s.violate(s.name, "pod-1: %s still a hash voter %s after its socket went half-open", up, expiry)
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
	if _, ok := s.waitMetric(0, "erpc_upstream_report_age_seconds", hashAge(up), silentReconnect+reconnectSlack, func(v float64) bool { return v <= liveBound.Seconds() }); !ok {
		s.violate(s.name, "pod-1: %s not a hash voter again within %s of the path healing", up, silentReconnect+reconnectSlack)
	}
	s.blocks(25)
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
			if _, ok := s.waitMetric(pod, "erpc_upstream_report_age_seconds", hashAge(up), silentReconnect+reconnectSlack, func(v float64) bool { return v <= liveBound.Seconds() }); !ok {
				s.violate("N5", "pod-%d: %s not a hash voter again within %s of the last close", pod+1, up, silentReconnect+reconnectSlack)
			}
		}
		s.blocks(25)
	}}
}

// K2: a minimal config, 1 wss and 1 https upstream with no evm block and no
// network entry. The wss upstream is a source, the https one is not, the
// served tip needs both (quorum 2), and every subscription works. A
// non-source's polls vote too (§6.3), so what tells it from a poll source
// is its cadence: k2QuietAfter after the last tap closes, its latest polls
// fall to the full-poll interval, while a poll source keeps polling at its
// cadence for 60s. The network-wide `latest` is floored at the delivered
// head (feature.md: "the network-wide tracker's pin changes floor it"), and
// the lag gauge reads 0 at quorum ≤ 2, so the quorum shows on a
// use-upstream lane over both.
func k2() spec {
	return spec{name: "K2", project: "k2", minimal: true, bound: healthyDelivery,
		vendors: []vendorSpec{{"ws-a", "vendor-a", true}, {"http-b", "vendor-b", false}},
		clients: func(*scenario) {},
		body: func(s *scenario) {
			s.blocks(5)
			ws, http := s.upstream("ws-a"), s.upstream("http-b")
			var taps []func()
			for pod, p := range s.h.pods {
				cl, err := rpc.DialContext(s.ctx, p.wsURL(s.project, s.chain.ID()))
				if err != nil {
					s.t.Fatal(err)
				}
				ch := make(chan json.RawMessage, 1024)
				sub, err := cl.EthSubscribe(s.ctx, ch, "newHeads")
				if err != nil {
					s.violate("K2", "pod-%d: newHeads subscription: %v", pod+1, err)
					cl.Close()
					continue
				}
				taps = append(taps, func() { sub.Unsubscribe(); cl.Close() })
				got, end := 0, time.After(3*k2Heads*s.chain.BlockTime())
			recv:
				for got < k2Heads {
					select {
					case <-ch:
						got++
					case <-end:
						break recv
					}
				}
				if got < k2Heads {
					s.violate("K2", "pod-%d: newHeads delivered %d headers in %d block times, wants %d", pod+1, got, 3*k2Heads, k2Heads)
				}
			}
			s.blocks(10)
			for _, closeTap := range taps {
				closeTap()
			}
			time.Sleep(k2QuietAfter)
			polled := func(pod int) float64 {
				v, ok := s.metric(pod, "erpc_upstream_latest_block_polled_total", map[string]string{"upstream": http})
				if !ok {
					s.violate("K2", "pod-%d: %s exports no latest polls", pod+1, http)
				}
				return v
			}
			before := make([]float64, len(s.h.pods))
			for pod := range s.h.pods {
				before[pod] = polled(pod)
			}
			time.Sleep(k2QuietWindow)
			for pod := range s.h.pods {
				n := polled(pod) - before[pod]
				if n > 1 {
					s.violate("K2", "pod-%d: %s, an https upstream with subscriptionSource unset, polled latest %v times in %s from %s after the last tap closed: it polls as a source", pod+1, http, n, k2QuietWindow, k2QuietAfter)
				}
				s.logf("pod-%d: %s polled latest %v times in %s from %s after the last tap closed (%v before)", pod+1, http, n, k2QuietWindow, k2QuietAfter, before[pod])
				if v, _ := s.metric(pod, "erpc_chain_tracker_source_heads_total", map[string]string{"upstream": ws, "outcome": "voted"}); v == 0 {
					s.violate("K2", "pod-%d: %s, a wss upstream with subscriptionSource unset, voted no header", pod+1, ws)
				}
			}
			hb := s.vendors["http-b"]
			hb.Update(func(f *chainsim.Faults) { f.Lag = 5 })
			for pod, p := range s.h.pods {
				c, err := rpc.DialOptions(s.ctx, p.httpURL(s.project, s.chain.ID()), rpc.WithHTTPClient(p.httpc), rpc.WithHeader("X-ERPC-Use-Upstream", "k2-*"))
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

// k2Heads: the headers each K2 newHeads subscription must deliver.
// k2QuietAfter and k2QuietWindow: from k2QuietAfter after the last tap
// closes, the https non-source polls latest at most once in k2QuietWindow,
// a third of its 30s full-poll interval (§6.8, row K2).
const (
	k2Heads       = 5
	k2QuietAfter  = 5 * time.Second
	k2QuietWindow = 10 * time.Second
)

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
		lag := map[string]string{"transport": "ws", "kind": "newHeads", "reason": "lagging", "detail": "lag"}
		lag0, _ := s.metric(0, "erpc_tap_end_total", lag)
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
		if v, _ := s.metric(0, "erpc_tap_end_total", lag); v <= lag0 {
			s.violate(sh.name, "pod-1: erpc_tap_end_total{reason=lagging,detail=lag} stayed %v", v)
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
	return spec{name: "N4", bound: healthyDelivery, allow: []string{"grpc:Unavailable@pod-1", "ws:jump@pod-1", "ws:logs-end@pod-1"}, body: func(s *scenario) { cutPod1(s) }}
}

// N4-lone: N4 on a single-instance deployment (one pod, shared state in
// memory): no peer tells the cut pod the chain moved on. Its streams still
// end UNAVAILABLE within the read wait and resume on it once healed. The
// selection policy ranks http-a, ws-a, ws-b, so the cut pod's two voters
// use both channels; each stops being a hash voter before the streams end:
// http-a by its failed polls, ws-a by its socket.
func n4Lone() spec {
	return spec{name: "N4-lone", pods: 1, memory: true, bound: healthyDelivery, allow: []string{"grpc:Unavailable@pod-1", "ws:jump@pod-1", "ws:logs-end@pod-1"},
		network: map[string]interface{}{"selectionPolicy": map[string]interface{}{
			"evalFunc": `(upstreams, ctx) => {
				const rank = (u) => { const i = ['-http-a', '-ws-a', '-ws-b'].findIndex(k => u.id.endsWith(k)); return i < 0 ? 3 : i };
				return upstreams.filter(u => true).sort((a, b) => rank(a) - rank(b));
			}`,
		}},
		body: func(s *scenario) {
			s.blocks(20)
			for want, key := range []string{"http-a", "ws-a", "ws-b"} {
				if pos, ok := s.metric(0, "erpc_selection_position", map[string]string{"method": "*", "upstream": s.upstream(key)}); !ok || int(pos) != want {
					s.violate("harness", "%s ranks %v (scraped %v), not %d", key, pos, ok, want)
				}
			}
			cutPod1(s, "http-a", "ws-a")
		}}
}

// cutPod1 cuts pod-1 from every vendor for 2 minutes and returns when
// the cut began. Each voter in channels must stop being a hash voter on
// pod-1 (its hash-age gauge +Inf: its channel stopped answering) before a
// stream there ends.
func cutPod1(s *scenario, channels ...string) time.Time {
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
	ended := make([]chan time.Time, len(channels))
	for i, key := range channels {
		ended[i] = make(chan time.Time, 1)
		go func() {
			var at time.Time
			if _, ok := s.waitMetric(0, "erpc_upstream_report_age_seconds", hashAge(s.upstream(key)), grpcReadWait+reconnectSlack, func(v float64) bool { return math.IsInf(v, 1) }); ok {
				at = time.Now()
			}
			ended[i] <- at
		}()
	}
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
	channelEnds := make([]time.Time, len(channels))
	for i, key := range channels {
		if channelEnds[i] = <-ended[i]; channelEnds[i].IsZero() {
			s.violate(s.name, "pod-1: %s still a hash voter %s after the cut", key, grpcReadWait+reconnectSlack)
		} else {
			s.logf("pod-1: %s no hash voter %s after the cut", key, channelEnds[i].Sub(cut).Round(100*time.Millisecond))
		}
	}
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
		if end != nil {
			for i, key := range channels {
				// The gauge is seen one scrape after it changes.
				if at := channelEnds[i]; !at.IsZero() && at.Add(-scrapeEvery).After(end.at) {
					s.violate(g.name, "StreamBlocks on the cut pod-1 ended before %s stopped being a hash voter", key)
				}
			}
		}
		if resumes == 0 {
			s.violate(g.name, "never resumed from a cursor")
		}
	}
	return cut
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
