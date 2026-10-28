package realworld

import (
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Bounds of the chain-behaviour scenarios (§19 C2, C6, C8, C10, C11, C14,
// C15).
const (
	// c6Halt: the chain makes no block this long (§19 C6); c6Sample: how
	// often Latest is read meanwhile (harness).
	c6Halt   = 3 * time.Minute
	c6Sample = 5 * time.Second

	// c8BlockTime: C8's chain runs at twice the baseline rate (§19 C8).
	c8BlockTime = 100 * time.Millisecond

	// c10Blocks: empty blocks in a row (§19 C10).
	c10Blocks = 100

	// c11Txs: receipts per big block (§19 C11); c11Blocks: block times the
	// chain mints them. c11Bound: a big block's receipts are one tracker
	// fetch, bounded by its 10s (§8), after the healthy bound.
	c11Txs    = 5000
	c11Blocks = 5
	c11Bound  = healthyDelivery + fetchTimeout

	// c14Blocks: blocks two of four voters stay on a branch that does not
	// link (harness).
	c14Blocks = 30

	// c15Run: C15 runs past the 60s after which a tap waiting on one
	// block's receipts closes 1013 (§10.2), with c15Empty empty blocks
	// planned in a row at its start (harness).
	c15Run   = logsCloseSpread + 20*time.Second
	c15Empty = 10
)

// C2: a reorg reorgWindow − 1 deep. As C1, every client that received a
// replaced block is told: newHeads and gRPC from the fork point + 1, logs
// with removed:true; no tap goes out of range (§19 C2; §10.2).
func c2() spec {
	return spec{name: "C2", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(50)
		oor := make([]float64, len(s.h.pods))
		for pod := range s.h.pods {
			oor[pod], _ = s.metric(pod, "erpc_tap_out_of_range_total", nil)
		}
		s.reorg(reorgWindow - 1)
		s.blocks(25)
		s.requireReorgsTold()
		for pod := range s.h.pods {
			if v, _ := s.metric(pod, "erpc_tap_out_of_range_total", nil); v > oor[pod] {
				s.violate(s.name, "pod-%d: %v taps out of range on a %d-deep reorg, inside reorgWindow %d", pod+1, v-oor[pod], reorgWindow-1, reorgWindow)
			}
		}
	}}
}

// C6: the chain makes no block for c6Halt while every client stays
// connected. No wss source reconnects or resubscribes, no gRPC stream or
// ws subscription ends (the clients' oracles), and Latest stays frozen at
// the halted head (§19 C6; §10.2: a Read at the head of a chain that makes
// no blocks waits without end; §15.1 the chain makes no blocks).
func c6() spec {
	return spec{name: "C6", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		wss := []string{"ws-a", "ws-b"}
		connects, subs := map[string]int64{}, map[string]int64{}
		for _, key := range wss {
			st := s.vendors[key].Stats()
			connects[key], subs[key] = st.Connects, st.HeadsSubscribes
		}
		s.chain.Halt(true)
		tip := s.chain.Tip()
		at := time.Now()
		s.logf("no blocks for %s from %d", c6Halt, tip.Number)
		time.Sleep(liveBound)
		frozen := make([]uint64, len(s.h.pods))
		moved := make([]bool, len(s.h.pods))
		for time.Since(at) < c6Halt {
			for pod := range s.h.pods {
				var bn hexutil.Uint64
				if err := s.call(pod, &bn, "eth_blockNumber"); err != nil {
					s.violate(s.name, "pod-%d: eth_blockNumber on a halted chain: %v", pod+1, err)
					continue
				}
				switch {
				case frozen[pod] == 0:
					frozen[pod] = uint64(bn)
					if uint64(bn)+headSlackBlocks < tip.Number {
						s.violate(s.name, "pod-%d: eth_blockNumber %d, %d below the halted head %d", pod+1, bn, tip.Number-uint64(bn), tip.Number)
					}
				case uint64(bn) != frozen[pod] && !moved[pod]:
					moved[pod] = true
					s.violate(s.name, "pod-%d: eth_blockNumber moved from %d to %d %s into a halt: Latest not frozen", pod+1, frozen[pod], bn, time.Since(at).Round(time.Second))
				}
			}
			time.Sleep(c6Sample)
		}
		for _, key := range wss {
			st := s.vendors[key].Stats()
			if n := st.Connects - connects[key]; n > 0 {
				s.violate(s.name, "%s got %d connections during a %s halt: a source reconnected", s.upstream(key), n, c6Halt)
			}
			if n := st.HeadsSubscribes - subs[key]; n > 0 {
				s.violate(s.name, "%s got %d newHeads subscribes during a %s halt: a source resubscribed", s.upstream(key), n, c6Halt)
			}
		}
		s.chain.Halt(false)
		s.logf("blocks again after %s", time.Since(at).Round(time.Second))
		s.blocks(25)
	}}
}

// C8: U5 and C1 at twice the rate: 100 ms blocks, every vendor answering
// 429 for u5Blocks, then 1–3 deep reorgs. The results are theirs: nobody
// benched, delivery resumes within u5ResumeBlocks block times, every
// client told of every reorg (§19 C8).
func c8() spec {
	return spec{name: "C8", bound: healthyDelivery, sim: chainsim.Config{BlockTime: c8BlockTime}, body: func(s *scenario) {
		s.blocks(50)
		throttleAll(s)
		s.blocks(25)
		for range 12 {
			s.blocks(15)
			s.reorg(1 + rand.IntN(3))
		}
		s.blocks(25)
		s.requireReorgsTold()
	}}
}

// C10: c10Blocks empty blocks in a row. No vendor gets a receipts call for
// any of them, and every header client gets every one (§19 C10; §11: an
// empty bloom needs no receipts).
func c10() spec {
	return spec{name: "C10", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		kinds := make([]chainsim.Kind, c10Blocks)
		for i := range kinds {
			kinds[i] = chainsim.KindEmpty
		}
		s.chain.Plan(kinds...)
		from := time.Now()
		s.blocks(c10Blocks)
		to := time.Now()
		s.blocks(25)
		var empty []*chainsim.Block
		for _, b := range s.chain.MintedBetween(from, to) {
			if b.Kind() == chainsim.KindEmpty {
				empty = append(empty, b)
			}
		}
		if len(empty) < c10Blocks/2 {
			s.violate("harness", "%d empty blocks minted, planned %d", len(empty), c10Blocks)
		}
		for _, b := range empty {
			for key, v := range s.vendors {
				if calls, _ := v.ReceiptsOf(b.Hash); calls > 0 {
					s.violate(s.name, "%s got %d receipts calls for empty block %d", s.upstream(key), calls, b.Number)
				}
			}
		}
		s.requireGot(empty)
	}}
}

// requireGot: every header client received every block of bs.
func (s *scenario) requireGot(bs []*chainsim.Block) {
	var cs []*clientBase
	for _, c := range s.heads {
		cs = append(cs, &c.clientBase)
	}
	for _, g := range s.grpcs {
		cs = append(cs, &g.clientBase)
	}
	for _, c := range cs {
		got := map[hash]bool{}
		c.mu.Lock()
		for _, d := range c.deliveries {
			got[d.hash] = true
		}
		c.mu.Unlock()
		for _, b := range bs {
			if !got[b.Hash] {
				s.violate(c.name, "never received block %d %s", b.Number, short(b.Hash))
				break
			}
		}
	}
}

// C11: for c11Blocks block times every non-empty block holds c11Txs
// receipts, and every upstream has a breaker that opens on 2 failures of 4.
// Every client's logs equal the canonical logs, and no breaker opens:
// tracker fetches never feed one (§19 C11; §8).
func c11() spec {
	breaker := map[string]interface{}{"failsafe": []map[string]interface{}{{
		"matchMethod": "*",
		"circuitBreaker": map[string]interface{}{
			"failureThresholdCount": 2, "failureThresholdCapacity": 4, "halfOpenAfter": "5m",
			"successThresholdCount": 1, "successThresholdCapacity": 1,
		},
	}}}
	up := map[string]map[string]interface{}{}
	for _, v := range baseVendors {
		up[v.key] = breaker
	}
	return spec{name: "C11", bound: c11Bound, up: up, body: func(s *scenario) {
		s.blocks(25)
		s.chain.BigBlocks(c11Txs)
		from := time.Now()
		s.logf("blocks of %d receipts for %d block times", c11Txs, c11Blocks)
		s.blocks(c11Blocks)
		s.chain.BigBlocks(0)
		to := time.Now()
		s.blocks(50)
		big := 0
		for _, b := range s.chain.MintedBetween(from, to) {
			if b.TxCount() == c11Txs {
				big++
			}
		}
		if big == 0 {
			s.violate("harness", "no block of %d receipts minted", c11Txs)
		}
		for pod := range s.h.pods {
			for key := range s.vendors {
				if v, _ := s.projectMetric(pod, "erpc_upstream_breaker_state_change_total", map[string]string{"upstream": s.upstream(key), "transition": "closed_to_open"}); v > 0 {
					s.violate(s.name, "pod-%d: %s's breaker opened %v times on %d-receipt blocks", pod+1, s.upstream(key), v, c11Txs)
				}
			}
		}
	}}
}

// C14: vendor-b (ws-b, http-b), 2 of 4 voters, moves to a branch at the
// canonical heights that links to no block of the chain, for c14Blocks.
// The canonical chain stays: no client gets a block of that branch, and
// Latest never stalls or leaves the canonical heights (§19 C14; §6.5).
func c14() spec {
	return spec{name: "C14", bound: settleDelivery, body: func(s *scenario) {
		s.blocks(25)
		wsb := s.vendors["ws-b"]
		stop := s.watchLatest(func(pod int, bn, tip uint64) {
			s.violate(s.name, "pod-%d: eth_blockNumber %d against the tip %d while vendor-b votes a branch that does not link (slack %d)", pod+1, bn, tip, headSlackBlocks)
		})
		f := s.chain.StartDetached(wsb, s.vendors["http-b"])
		s.mu.Lock()
		s.fork = wsb.Head()
		s.mu.Unlock()
		s.logf("vendor-b on a branch that links nowhere, from %d %s", s.fork.Number, short(s.fork.Hash))
		s.blocks(c14Blocks)
		stop()
		s.chain.EndFork(f)
		s.logf("vendor-b back on the canonical chain")
		s.blocks(25)
		s.requireNoMinorityDelivered()
	}}
}

// C15: a chain whose hashes and roots are not Ethereum's (Hedera, Kaia):
// no header hashes to its hash, and an empty block's transactionsRoot is
// keccak(""), not the empty-trie root. The network sets
// validateTransactionsRoot: false, so a header with gasUsed 0 is an empty
// block. One ws logs client per pod reads every block: no empty block's
// receipts are fetched, no receipts or logs fetch fails, and no tap closes
// 1013 (§19 C15; §11 receipts answer).
func c15() spec {
	return spec{name: "C15", bound: healthyDelivery, sim: chainsim.Config{Foreign: true},
		network: map[string]interface{}{"directiveDefaults": map[string]interface{}{"validateTransactionsRoot": false}},
		clients: func(s *scenario) {
			for pod := range s.h.pods {
				s.addLogs(fmt.Sprintf("p%d-logs", pod+1), pod, 0)
			}
		},
		body: func(s *scenario) {
			s.blocks(25)
			plan := make([]chainsim.Kind, c15Empty)
			for i := range plan {
				plan[i] = chainsim.KindEmpty
			}
			from := s.chain.Plan(plan...).Number + 1
			s.logf("%d empty blocks from %d, then %s of random blocks", c15Empty, from, c15Run)
			time.Sleep(c15Run)
			to := s.chain.Tip().Number
			empty := 0
			for n := from; n <= to; n++ {
				b := s.chain.CanonAt(n)
				if b == nil || b.Kind() != chainsim.KindEmpty {
					continue
				}
				empty++
				for key, v := range s.vendors {
					if calls, _ := v.ReceiptsOf(b.Hash); calls > 0 {
						s.violate(s.name, "%s answered %d receipts calls for empty block %d", s.upstream(key), calls, n)
					}
				}
			}
			if empty < c15Empty {
				s.violate("harness", "only %d empty blocks minted from %d to %d", empty, from, to)
			}
			for pod := range s.h.pods {
				for _, kind := range []string{"receipts", "logs"} {
					if v, _ := s.metric(pod, "erpc_chain_tracker_fetch_total", map[string]string{"kind": kind, "outcome": "error"}); v > 0 {
						s.violate(s.name, "pod-%d: %v failed %s fetches on a chain whose empty blocks carry a foreign root", pod+1, v, kind)
					}
				}
				if v, _ := s.metric(pod, "erpc_tap_out_of_range_total", nil); v > 0 {
					s.violate(s.name, "pod-%d: %v taps out of range, 1013 for a logs client", pod+1, v)
				}
			}
		}}
}
