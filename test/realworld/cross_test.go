package realworld

import (
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Bounds of the cross-instance scenarios (§19 X3, X6).
const (
	// x6Reads: client reads pod-1 sends while its link to http-a refuses
	// connections; x6Open: its breaker opens within this (harness).
	x6Reads = 20
	x6Open  = 10 * time.Second
)

// X3: pod-2 boots cold, mid-chain, on redis pod-1 keeps current. Its first
// eth_blockNumber answer is the chain's head within the healthy slack:
// Latest is known at once, from the shared counters (§14). Clients on both
// pods then subscribe at once; each block they receive is at or below the
// pod's Latest then (the oracle), so taps waited for the first confirmed
// head and sent nothing above Latest (§19 X3; §15.1 Head: cold start).
func x3() spec {
	return spec{name: "X3", cold: []int{1}, bound: healthyDelivery,
		clients: func(*scenario) {},
		body: func(s *scenario) {
			s.blocks(25)
			if err := s.h.pods[1].start(); err != nil {
				s.t.Fatal(err)
			}
			booted := time.Now()
			tip := s.chain.Tip().Number
			var bn hexutil.Uint64
			err := s.call(1, &bn, "eth_blockNumber")
			took := time.Since(booted)
			if err != nil || uint64(bn)+headSlackBlocks < tip {
				s.violate(s.name, "pod-2: first eth_blockNumber after a cold boot answered %d (err %v) in %s, head %d: Latest not known at once", bn, err, took.Round(time.Millisecond), tip)
			} else {
				s.logf("pod-2: first eth_blockNumber after a cold boot answered %d in %s, head %d", bn, took.Round(time.Millisecond), tip)
			}
			s.podClients(0, 0)
			s.podClients(1, 0)
			s.blocks(50)
}

// X6: pod-1's link to http-a, ranked first with a breaker that opens on 2
// failures of 4 and stays open 5 minutes, refuses connections while pod-1
// serves reads, so http-a's breaker opens on pod-1 only. The link heals,
// the breaker stays open: Latest may differ by instance, and the oracle
// holds for every block on both pods (§19 X6).
func x6() spec {
	return spec{name: "X6", bound: healthyDelivery,
		network: map[string]interface{}{"selectionPolicy": rankFirst("http-a")},
		up: map[string]map[string]interface{}{"http-a": {"failsafe": []map[string]interface{}{{
			"matchMethod": "*",
			"circuitBreaker": map[string]interface{}{
				"failureThresholdCount": 2, "failureThresholdCapacity": 4, "halfOpenAfter": "5m",
				"successThresholdCount": 1, "successThresholdCapacity": 1,
			},
		}}}},
		body: func(s *scenario) {
			s.blocks(25)
			px, p1, up := s.proxies["http-a"], s.h.pods[0], s.upstream("http-a")
			opened := map[string]string{"upstream": up, "transition": "closed_to_open"}
			px.set(p1, linkFaults{Refuse: true})
			px.closeAll(p1)
			for range x6Reads {
				b := s.chain.CanonAt(s.chain.Tip().Number - u1ProbeDepth)
				if got := s.blockAnswer(0, "", "eth_getBlockByNumber", hexutil.EncodeUint64(b.Number), false); got != b.Hash.Hex() {
					s.violate(s.name, "pod-1: eth_getBlockByNumber(%d) answered %s, not %s, with only %s refusing", b.Number, got, short(b.Hash), up)
				}
			}
			deadline := time.Now().Add(x6Open)
			for {
				if v, _ := s.projectMetric(0, "erpc_upstream_breaker_state_change_total", opened); v > 0 {
					break
				}
				if time.Now().After(deadline) {
					s.violate("harness", "pod-1: %s's breaker never opened while its link refused %d reads", up, x6Reads)
					break
				}
				time.Sleep(scrapeEvery)
			}
			px.set(p1, linkFaults{})
			s.logf("%s's breaker open on pod-1; link healed", up)
			s.blocks(50)
			if v, _ := s.projectMetric(0, "erpc_upstream_breaker_state_change_total", map[string]string{"upstream": up, "transition": "open_to_half_open"}); v > 0 {
				s.violate("harness", "pod-1: %s's breaker left open during the scenario", up)
			}
			if v, _ := s.projectMetric(1, "erpc_upstream_breaker_state_change_total", opened); v > 0 {
				s.violate("harness", "pod-2: %s's breaker opened, though only pod-1's link failed", up)
			}
		}}
