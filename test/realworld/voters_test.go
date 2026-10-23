package realworld

import (
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Bounds of the head model (specs/chain-tracker/feature.md on the
// unified-head-engine branch); liveBound is W (§1).
const (
	// benchFloor: an upstream whose number age passes max(30s, 3 × its
	// backed-off cadence) is benched, never sooner than 30s (§6.1).
	benchFloor = 30 * time.Second
	// benchBound: one upstream ahead with its peers stalled holds Latest
	// until the bench, at most 90s (§6.1 bounds, §15.1).
	benchBound = 90 * time.Second
	// headSlackBlocks: Latest is the q-th highest voter number (§6.4), and
	// a voter is current while its report is at most W old (§1, §6.1), so
	// a healthy Latest stays within W of chain below the tip.
	headSlackBlocks = uint64(liveBound / blockTime)
	// cutOverDelivery: a voter cut off stops counting toward the hash
	// quorum once its hash age passes W (§6.1, §6.5); the next header
	// follows within one block.
	cutOverDelivery = healthyDelivery + blockTime
	// u10Lag, k6Lag: how far behind the faulty upstream serves (§19).
	u10Lag = 40
	k6Lag  = 5
	// u18PollInterval is harness, not spec: http-b's statePollerInterval,
	// short enough that each pod polls it several times during the fork.
	u18PollInterval = time.Second
)

// X9: 1 wss + 2 https non-sources, 3 pods, redis. Only pod-2 gets
// eth_blockNumber traffic, so only its polls keep the https counters fresh;
// pod-1 gets headers only from ws-a. Pod-1's taps never wait more than W:
// only voters that sent pod-1 a header within W count toward its hash
// quorum (§6.5). The oracle never asks pods 1 and 3 eth_blockNumber, so
// its own reads never make pod-1 poll a fresh https head.
func x9() spec {
	return spec{name: "X9", pods: 3, headless: []int{0, 2}, bound: healthyDelivery,
		vendors: []vendorSpec{{"ws-a", "vendor-a", true}, {"http-a", "vendor-a", false}, {"http-b", "vendor-b", false}},
		clients: func(s *scenario) {
			s.podClients(0, 0)
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				for s.ctx.Err() == nil {
					var bn hexutil.Uint64
					_ = s.call(1, &bn, "eth_blockNumber")
					sleepCtx(s.ctx, blockTime)
				}
			}()
		},
		body: func(s *scenario) { s.blocks(150) }}
}

// N3: pod-1 is cut from vendor-a (ws-a and http-a: sockets closed, new
// connections refused) for 50 blocks. Pod-1 flows on vendor-b's votes: no
// tap on it waits more than the cut-over bound.
func n3() spec {
	return spec{name: "N3", bound: cutOverDelivery, body: func(s *scenario) {
		s.blocks(25)
		p1 := s.h.pods[0]
		for _, key := range []string{"ws-a", "http-a"} {
			s.proxies[key].set(p1, linkFaults{Refuse: true})
			s.proxies[key].closeAll(p1)
		}
		cut := time.Now()
		s.logf("pod-1 cut from vendor-a")
		s.blocks(50)
		for _, key := range []string{"ws-a", "http-a"} {
			s.proxies[key].set(p1, linkFaults{})
		}
		s.logf("pod-1 path to vendor-a healed")
		for _, c := range s.heads {
			if c.currentPod() == 0 && !c.deliveredOn(0, cut.Add(cutOverDelivery)) {
				s.violate(c.name, "received nothing on pod-1 while it was cut from vendor-a")
			}
		}
		s.blocks(25)
	}}
}

// U10: ws-a serves 40 blocks behind the tip and keeps advancing. Latest
// stays at the tip (§6.4), ws-a's hash vote ages past W (§6.1), and ws-a
// is benched no sooner than 30s and within 90s (§6.1, §15.1).
func u10() spec {
	return spec{name: "U10", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		lagUpstream(s, "ws-a", u10Lag)
	}}
}

// lagUpstream runs U10's fault on vendor key: it serves lag blocks behind
// and keeps advancing until its bench; the checks are U10's.
func lagUpstream(s *scenario, key string, lag uint64) {
	up := s.upstream(key)
	benched := func(pod int) float64 {
		v, _ := s.metric(pod, "erpc_upstream_head_benched_total", map[string]string{"upstream": up})
		return v
	}
	var bench0 []float64
	for pod := range s.h.pods {
		bench0 = append(bench0, benched(pod))
	}
	stop := s.watchLatest(func(pod int, bn, tip uint64) {
		s.violate(s.name, "pod-%d: eth_blockNumber %d, %d below the tip %d while %s lags %d (slack %d)", pod+1, bn, tip-bn, tip, up, lag, headSlackBlocks)
	})
	s.vendors[key].Update(func(f *chainsim.Faults) { f.Lag = lag })
	at := time.Now()
	s.logf("%s serves %d blocks behind", up, lag)
	for pod := range s.h.pods {
		labels := map[string]string{"upstream": up, "evidence": "hash"}
		if v, ok := s.waitMetric(pod, "erpc_upstream_report_age_seconds", labels, liveBound+reconnectSlack, func(v float64) bool { return v > liveBound.Seconds() }); !ok {
			s.violate(s.name, "pod-%d: erpc_upstream_report_age_seconds{evidence=hash} of %s is %v, not above W (%s), %s after it fell behind", pod+1, up, v, liveBound, time.Since(at).Round(time.Second))
		}
	}
	time.Sleep(time.Until(at.Add(benchFloor - reconnectSlack)))
	for pod := range s.h.pods {
		if v := benched(pod); v > bench0[pod] {
			s.violate(s.name, "pod-%d: %s benched %s after it fell behind, before the %s floor", pod+1, up, time.Since(at).Round(time.Second), benchFloor)
		}
	}
	for pod := range s.h.pods {
		if _, ok := s.waitMetric(pod, "erpc_upstream_head_benched_total", map[string]string{"upstream": up}, time.Until(at.Add(benchBound+reconnectSlack)), func(v float64) bool { return v > bench0[pod] }); !ok {
			s.violate(s.name, "pod-%d: %s not benched within %s of falling %d behind", pod+1, up, benchBound, lag)
		}
	}
	stop()
	s.vendors[key].Update(func(f *chainsim.Faults) { f.Lag = 0 })
	s.blocks(25)
}

// watchLatest samples eth_blockNumber on every non-numbered pod each block
// until stop, calling bad once per pod when it falls more than
// headSlackBlocks below the chain tip, or rises above it.
func (s *scenario) watchLatest(bad func(pod int, bn, tip uint64)) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		flagged := map[int]bool{}
		for {
			select {
			case <-done:
				return
			case <-time.After(s.chain.BlockTime()):
			}
			for pod := range s.h.pods {
				if flagged[pod] || s.isHeadless(pod) {
					continue
				}
				low := s.chain.Tip().Number
				var bn hexutil.Uint64
				if err := s.call(pod, &bn, "eth_blockNumber"); err != nil {
					continue
				}
				high := s.chain.Tip().Number
				if uint64(bn)+headSlackBlocks < low || uint64(bn) > high {
					flagged[pod] = true
					bad(pod, uint64(bn), low)
				}
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// U11: two wss upstreams only, quorum 2; ws-b's head freezes while it
// still answers. Latest and the taps wait for ws-b's bench, then flow at
// once (§6.1, §15.1 "one ahead, peers stalled": at most 90s). The wait is
// longer than reorgWindow blocks, so each transport signals it as §10.2
// says: one newHeads restart (a number jump) per subscription, ws logs
// closed with 1013 within the close spread, each gRPC stream resumed at
// most once.
func u11() spec {
	return spec{name: "U11", bound: benchBound + liveBound, allow: []string{"grpc:Unavailable", "ws:jump", "ws:logs-end"},
		vendors: []vendorSpec{{"ws-a", "vendor-a", true}, {"ws-b", "vendor-b", true}},
		clients: func(s *scenario) {
			for pod := range s.h.pods {
				s.podClients(pod, 0)
			}
		},
		body: func(s *scenario) {
			s.blocks(25)
			wsb := s.vendors["ws-b"]
			wsb.Update(func(f *chainsim.Faults) { f.Frozen = true })
			frozen := wsb.Head().Number
			at := time.Now()
			s.logf("ws-b frozen at %d", frozen)
			waited := make([]bool, len(s.h.pods))
			for time.Since(at) < benchFloor-reconnectSlack {
				time.Sleep(scrapeEvery)
				if time.Since(at) < liveBound {
					continue
				}
				for pod := range s.h.pods {
					var bn hexutil.Uint64
					if err := s.call(pod, &bn, "eth_blockNumber"); err != nil || waited[pod] {
						continue
					}
					if uint64(bn) > frozen+1 {
						waited[pod] = true
						s.violate(s.name, "pod-%d: eth_blockNumber %d above ws-b's frozen %d, %s after the freeze: Latest did not wait for the bench", pod+1, bn, frozen, time.Since(at).Round(time.Second))
					}
				}
			}
			var flowed time.Time
			for pod := range s.h.pods {
				for {
					var bn hexutil.Uint64
					err := s.call(pod, &bn, "eth_blockNumber")
					if err == nil && uint64(bn)+headSlackBlocks >= s.chain.Tip().Number {
						s.logf("pod-%d: Latest flows again %s after the freeze", pod+1, time.Since(at).Round(time.Second))
						break
					}
					if time.Since(at) > benchBound+reconnectSlack {
						s.violate(s.name, "pod-%d: eth_blockNumber %d (err %v), tip %d, %s after ws-b froze: Latest did not flow after the bench", pod+1, bn, err, s.chain.Tip().Number, benchBound)
						break
					}
					time.Sleep(scrapeEvery)
				}
			}
			flowed = time.Now()
			s.blocks(25)
			s.requireLiveAfter(flowed.Add(liveBound), healthyDelivery)
			time.Sleep(time.Until(flowed.Add(logsCloseSpread + reconnectSlack)))
			for _, c := range s.heads {
				c.mu.Lock()
				n := 0
				for _, j := range c.jumps {
					if !j.newConn && j.at.After(at) {
						n++
					}
				}
				c.mu.Unlock()
				if n != 1 {
					s.violate(c.name, "newHeads restarted %d times on one connection after the freeze; the bench wait restarts it once", n)
				}
			}
			for _, c := range s.logs {
				if !c.endedWith(at, flowed.Add(logsCloseSpread+reconnectSlack), func(e clientEnd) bool { return e.server && closedWith(e.reason, 1013) }) {
					s.violate(c.name, "no 1013 close within %s of the flow resuming", logsCloseSpread)
				}
			}
			for _, g := range s.grpcs {
				g.mu.Lock()
				n := 0
				for _, e := range g.ends {
					if e.server {
						n++
					}
				}
				g.mu.Unlock()
				if n > 1 {
					s.violate(g.name, "StreamBlocks ended %d times; resumes at most once", n)
				}
			}
			wsb.Update(func(f *chainsim.Faults) { f.Frozen = false })
			s.blocks(25)
		}}
}

// requireLiveAfter: every header client received a block minted after
// from, and each such block within bound of its first header on the pod
// (waitFrom).
func (s *scenario) requireLiveAfter(from time.Time, bound time.Duration) {
	var cs []*clientBase
	for _, c := range s.heads {
		cs = append(cs, &c.clientBase)
	}
	for _, g := range s.grpcs {
		cs = append(cs, &g.clientBase)
	}
	for _, c := range cs {
		c.mu.Lock()
		got := 0
		for _, d := range c.deliveries {
			if !d.minted.After(from) {
				continue
			}
			got++
			if l := d.at.Sub(s.waitFrom(d.pod, d.hash, d.minted)); l > bound {
				s.violate(c.name, "pod-%d: block %d reached the client %s after its first header there, after the flow resumed (bound %s)", d.pod+1, d.number, l.Round(time.Millisecond), bound)
			}
		}
		c.mu.Unlock()
		if got == 0 {
			s.violate(c.name, "received no block minted after %s", from.Format("15:04:05.000"))
		}
	}
}

// U18: http-b, an HTTP vendor that is no source, serves a minority fork at
// the same heights for 20 blocks, and the selection policy ranks it first,
// so every read the head model does not steer reaches it. No tap sends a
// fork block; an HTTP read by number or tag may serve one until W, plus the
// tracker's processing, after a fork block first reached the pod as a
// latest answer: http-b's hash vote (§6.3), after which it loses its number
// vote and goes last. http-b polls every u18PollInterval, as a non-source an
// operator polls often does, so every pod sees the fork through its own
// polls and the bound runs.
func u18() spec {
	return spec{name: "U18", bound: settleDelivery,
		upEvm: map[string]map[string]interface{}{
			"http-b": {"statePollerInterval": u18PollInterval.String()},
		},
		network: map[string]interface{}{"selectionPolicy": map[string]interface{}{
			"evalFunc": `(upstreams, ctx) => upstreams.filter(u => true).sort((a, b) => (b.id.endsWith('-http-b') ? 1 : 0) - (a.id.endsWith('-http-b') ? 1 : 0))`,
		}},
		body: func(s *scenario) {
			s.blocks(35)
			forked := "http-b"
			f := s.chain.StartFork(1, 1, s.vendors[forked])
			at := time.Now()
			s.mu.Lock()
			s.fork = s.vendors[forked].Head()
			s.mu.Unlock()
			s.logf("%s, ranked first, on a minority fork", s.upstream(forked))
			done := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-done:
						return
					case <-time.After(s.chain.BlockTime()):
					}
					for pod := range s.h.pods {
						s.requireNoMinority(pod, at, "eth_getBlockByNumber(latest)", "latest")
						var bn hexutil.Uint64
						if err := s.call(pod, &bn, "eth_blockNumber"); err != nil {
							continue
						}
						// Older heights too: http-b's polls keep its head counter
						// a few blocks back, and only reads at or below it try
						// http-b first (the reached-N hint, §6.1).
						for _, n := range []uint64{uint64(bn), uint64(bn) - 1, uint64(bn) - 5, uint64(bn) - 8} {
							s.requireNoMinority(pod, at, fmt.Sprintf("eth_getBlockByNumber(%d)", n), hexutil.EncodeUint64(n))
						}
					}
				}
			}()
			s.blocks(20)
			s.chain.EndFork(f)
			s.logf("%s back on the canonical chain", s.upstream(forked))
			time.Sleep(liveBound)
			close(done)
			wg.Wait()
			served := s.minorityServed()
			if served == 0 {
				s.violate(s.name, "no read reached the forked vendor inside the grace, so demotion was never exercised")
			}
			s.logf("%d reads served a fork block inside the grace", served)
			s.requireNoMinorityDelivered()
			for pod := range s.h.pods {
				latest, header := s.forkFirsts(pod)
				if latest.IsZero() {
					s.h.headersMu.Lock()
					n := len(s.h.latests[pod])
					s.h.headersMu.Unlock()
					s.violate(s.name, "pod-%d: no fork block reached it as a latest answer (%d latest answers), so the bound never ran", pod+1, n)
					continue
				}
				s.logf("pod-%d: first fork latest answer %s after the fork's first header", pod+1, latest.Sub(header).Round(time.Millisecond))
			}
			s.blocks(25)
		}}
}

// requireNoMinority reads a block by tag or number on pod and fails on a
// minority-fork answer to a read that started more than W, plus the
// tracker's processing, after a fork block first reached pod as a latest
// answer (minorityGrace); it logs and counts the answers the grace allows.
func (s *scenario) requireNoMinority(pod int, forkAt time.Time, what, ref string) {
	start := time.Now()
	got := s.blockAnswer(pod, "", "eth_getBlockByNumber", ref, false)
	b := s.blockOf(got)
	if b == nil || !b.Minority() {
		return
	}
	latest, _ := s.forkFirsts(pod)
	if !s.minorityGrace(pod, start, got) {
		s.violate(s.name, "pod-%d: %s served minority-fork block %d %s, read %s after the fork, %s after its first fork latest answer (allowed for %s)",
			pod+1, what, b.Number, short(b.Hash), start.Sub(forkAt).Round(time.Millisecond), start.Sub(latest).Round(time.Millisecond), liveBound+tapProcessingSlack)
		return
	}
	since := "no fork latest answer yet"
	if !latest.IsZero() {
		since = start.Sub(latest).Round(time.Millisecond).String() + " after its first fork latest answer"
	}
	s.logf("pod-%d: %s served minority-fork block %d, read %s after the fork, %s", pod+1, what, b.Number, start.Sub(forkAt).Round(time.Millisecond), since)
	s.graceServed.Add(1)
}

// minorityServed is how many reads the minority grace allowed.
func (s *scenario) minorityServed() int64 { return s.graceServed.Load() }

// blockOf is the chain's block of a blockAnswer, nil for null or an error.
func (s *scenario) blockOf(answer string) *chainsim.Block {
	if len(answer) != 66 {
		return nil
	}
	h, ok := hexToHash(answer)
	if !ok {
		return nil
	}
	return s.chain.Block(h)
}

func hexToHash(s string) (hash, bool) {
	b, err := hexutil.Decode(s)
	if err != nil || len(b) != 32 {
		return hash{}, false
	}
	return hash(b), true
}

// requireNoMinorityDelivered: no client received a block or a log of a
// minority fork.
func (s *scenario) requireNoMinorityDelivered() {
	var cs []*clientBase
	for _, c := range s.heads {
		cs = append(cs, &c.clientBase)
	}
	for _, g := range s.grpcs {
		cs = append(cs, &g.clientBase)
	}
	for _, c := range s.logs {
		cs = append(cs, &c.clientBase)
	}
	for _, c := range cs {
		c.mu.Lock()
		for _, d := range c.deliveries {
			if b := s.chain.Block(d.hash); b != nil && b.Minority() {
				s.violate(c.name, "pod-%d: received minority-fork block %d %s", d.pod+1, d.number, short(d.hash))
			}
		}
		c.mu.Unlock()
	}
}

// K6: quorum majority, Latest guaranteed for vendor-b's group, vendor-a in
// the fallback tier with failover on. http-b serves k6Lag behind for 50
// blocks: Latest is clamped to the group's majority, no tap sends a block
// above it, and nothing deadlocks (§6.4 guarantees, §6.2 topic admission).
func k6() spec {
	return spec{name: "K6", bound: healthyDelivery + k6Lag*blockTime,
		evm: map[string]interface{}{"servedTip": map[string]interface{}{
			"quorum": "majority", "guaranteedFor": []string{"k6-*-b"},
		}},
		network: map[string]interface{}{"failover": map[string]interface{}{"onDefaultsExhausted": true}},
		tags:    map[string][]string{"ws-a": {"tier:fallback"}, "http-a": {"tier:fallback"}},
		body: func(s *scenario) {
			s.blocks(25)
			hb := s.vendors["http-b"]
			hb.Update(func(f *chainsim.Faults) { f.Lag = k6Lag })
			s.logf("http-b serves %d behind", k6Lag)
			for pod := range s.h.pods {
				var bn hexutil.Uint64
				var err error
				var behind uint64
				for end := time.Now().Add(10 * time.Second); time.Now().Before(end) && behind < 3; time.Sleep(scrapeEvery) {
					err = s.call(pod, &bn, "eth_blockNumber")
					if tip := s.chain.Tip().Number; err == nil && tip > uint64(bn) {
						behind = tip - uint64(bn)
					}
				}
				if behind < 3 {
					s.violate(s.name, "pod-%d: eth_blockNumber %d (err %v), %d behind the tip while the guaranteed group's majority is %d behind: no clamp", pod+1, bn, err, behind, k6Lag)
				}
			}
			s.blocks(50)
			hb.Update(func(f *chainsim.Faults) { f.Lag = 0 })
			s.blocks(25)
		}}
}
