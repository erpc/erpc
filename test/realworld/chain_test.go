package realworld

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"google.golang.org/grpc/codes"
)

// Bounds of the chain-behaviour scenarios (§19 C2, C6, C8, C10, C11, C14,
// C15, C16, C17, C18).
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

	// c16First: C16's chain runs in the millions before its reset;
	// c16ResetHead: every vendor's head after it (§19 C16). c16Follow:
	// Latest follows a fall past the regression guard's 1024 blocks once
	// the guard's 60s hold fails open (§6.4, erpc/networks.go
	// servedTipRegressionTTL), plus 60s. c16FullPoll: statePollerInterval's
	// default, the full poll that moves the counter of an upstream no
	// source, Ask or client request moves (§6.8).
	c16First     = 5_000_000
	c16ResetHead = 10
	c16Follow    = 2 * time.Minute
	c16FullPoll  = 30 * time.Second
	// c16DeadCursor: a stream resumed from an old-chain cursor ends
	// OUT_OF_RANGE within W of the ceiling's fall (§10.1); the harness
	// opens it once every counter followed, after the fall.
	c16DeadCursor = liveBound + reconnectSlack

	// c17Halt: C17's chain makes no block this long, past maxBackfill
	// (§19 C17). Then c17Blocks blocks come one at a time, each up to
	// c17MaxGap after the last. The EMA grows at most 1.2× per sample
	// (alpha 0.1, samples capped at 3× the EMA, §6.7), so from the
	// chain's 0.2s it needs 26 samples to reach 20s, where W = 3 × bt
	// meets its cap (§1): W stays below the cap for c17Blocks unless one
	// sample across the halt moves it. wCap: W's cap (§1).
	c17Halt   = 30 * time.Minute
	c17Blocks = 20
	c17MaxGap = 60 * time.Second
	wCap      = 60 * time.Second

	// c18FinalityDepth: C18's one vendor finalizes this far below its
	// head, as a parachain whose finality follows its relay chain closely;
	// c18Depth: the reorg replaces the head and the 2 siblings below it,
	// from the fork point 3 below the head, so a pin at the vendor's
	// finalized height reverts (§19 C18). c18PollInterval keeps the full
	// poll, which polls finalized, out of the reorg's few seconds on most
	// pods (harness).
	c18FinalityDepth = 2
	c18Depth         = 3
	c18PollInterval  = 5 * time.Minute
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

// C16: a testnet reset. Every vendor's head drops from the millions to
// c16ResetHead on a new genesis, same chain id. Each pod's eth_blockNumber
// follows within c16Follow; every tap restarts on the new chain (ws logs
// end 1013 and resubscribe, gRPC ends OUT_OF_RANGE and starts over); every
// pod's latest counter of every upstream, and the shared one, follow the
// new chain; a stream resumed from an old-chain cursor no tree holds ends
// OUT_OF_RANGE; and no read is answered from the old chain, though its
// finalized block was cached permanently. A read of an old-chain block
// while the regression guard holds Latest may count retried_missing; no
// other may (§6.4, §14, §10.1, §10.2; §19 C16).
func c16() spec {
	return spec{name: "C16", cache: true, bound: healthyDelivery, sim: chainsim.Config{First: c16First},
		allow: []string{"ws:logs-end", "ws:jump", "grpc:OutOfRange"},
		body: func(s *scenario) {
			s.blocks(25)
			// Finalized trails the vendors' tag by up to one full poll
			// (§6.5); a request naming the tag waits for its refresh (§6.4).
			fin := s.chain.Tip().Number - chainsim.FinalizedLag
			for pod := range s.h.pods {
				b := s.blockOf(s.blockAnswer(pod, "", "eth_getBlockByNumber", "finalized", false))
				if b == nil {
					s.violate(s.name, "pod-%d: eth_getBlockByNumber(finalized) answered no block", pod+1)
					return
				}
				fin = min(fin, b.Number)
			}
			old := s.probeAt(fin - 20)
			if old == nil {
				s.violate("harness", "no old block below the finalized tag holds transactions")
				return
			}
			for pod := range s.h.pods {
				s.requireCachedAs(pod, old.Number, cacheFinal)
				if got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(old.Number), false); got != old.Hash.Hex() {
					s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) before the reset answered %s", pod+1, old.Number, got)
				}
				if got := s.blockAnswer(pod, "", "eth_getBlockByHash", old.Hash.Hex(), false); got != old.Hash.Hex() {
					s.violate(s.name, "pod-%d: eth_getBlockByHash(%s) before the reset answered %s", pod+1, short(old.Hash), got)
				}
			}
			was := s.chain.Tip()
			// Below the old tip by more than reorgWindow: the old tree,
			// held while its top is inside maxBackfill, no longer holds it.
			dead := s.chain.CanonAt(was.Number - 2*reorgWindow)
			missing0 := make([]float64, len(s.h.pods))
			for pod := range s.h.pods {
				missing0[pod], _ = s.metric(pod, "erpc_head_refusal_total", map[string]string{"rule": "retried_missing"})
			}
			at := time.Now()
			ex := s.excuse(-1, at)
			tip := s.chain.Reset(c16ResetHead)
			for pod := range s.h.pods {
				s.owe(pod, "latest_regressed", 1)
			}
			s.logf("testnet reset: head %d %s → %d %s", was.Number, short(was.Hash), tip.Number, short(tip.Hash))
			followed := make([]time.Time, len(s.h.pods))
			for left := len(s.h.pods); left > 0 && time.Since(at) < c16Follow+reconnectSlack; {
				time.Sleep(scrapeEvery)
				for pod := range s.h.pods {
					if !followed[pod].IsZero() {
						continue
					}
					var bn hexutil.Uint64
					if err := s.call(pod, &bn, "eth_blockNumber"); err == nil && uint64(bn) < c16First {
						followed[pod] = time.Now()
						left--
						s.logf("pod-%d: eth_blockNumber %d, %s after the reset", pod+1, bn, time.Since(at).Round(100*time.Millisecond))
					}
				}
			}
			last := at
			for pod, t := range followed {
				switch {
				case t.IsZero():
					s.violate(s.name, "pod-%d: eth_blockNumber still on the old chain %s after the reset (bound %s)", pod+1, time.Since(at).Round(time.Second), c16Follow)
				case t.Sub(at) > c16Follow:
					s.violate(s.name, "pod-%d: eth_blockNumber followed the reset after %s (bound %s)", pod+1, t.Sub(at).Round(time.Second), c16Follow)
				}
				if t.After(last) {
					last = t
				}
			}
			// Reads sent while the guard held Latest end within W of its
			// release, which the pod's eth_blockNumber showed.
			time.Sleep(time.Until(last.Add(liveBound)))
			for pod := range s.h.pods {
				v, _ := s.metric(pod, "erpc_head_refusal_total", map[string]string{"rule": "retried_missing"})
				s.logf("pod-%d: %v retried_missing while the guard held Latest", pod+1, v-missing0[pod])
				s.owe(pod, "retried_missing", v-missing0[pod])
			}
			// Taps restart once the pins re-anchor, after Latest follows,
			// within c16Follow; ws logs then close 1013 over
			// logsCloseSpread from their end (§10.2).
			time.Sleep(time.Until(last.Add(logsCloseSpread + reconnectSlack)))
			s.requireCountersFollow()
			for pod := range s.h.pods {
				rctx, cancel := context.WithCancel(s.ctx)
				n := dead.Number
				e, ok := next(s.openStream(rctx, pod, &n, dead.Hash.Bytes()), c16DeadCursor)
				if !ok || e.h != nil || e.code != codes.OutOfRange {
					got := fmt.Sprintf("end %s %s", e.code, e.msg)
					if e.h != nil {
						got = fmt.Sprintf("header %d", e.h.Number)
					} else if !ok {
						got = "nothing"
					}
					s.violate(s.name, "pod-%d: stream resumed from old-chain cursor %d %s: %s within %s, not OUT_OF_RANGE", pod+1, n, short(dead.Hash), got, c16DeadCursor)
				}
				cancel()
			}
			for pod := range s.h.pods {
				var raw json.RawMessage
				err := s.call(pod, &raw, probeMethod, hexutil.EncodeUint64(old.Number))
				if err == nil && len(raw) > 0 && string(raw) != "null" {
					s.violate(s.name, "pod-%d: %s(%d), an old-chain block cached as final, answered %s after the reset", pod+1, probeMethod, old.Number, raw)
				}
				if got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(old.Number), false); got == old.Hash.Hex() {
					s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) answered the old chain's block after the reset", pod+1, old.Number)
				}
				if got := s.blockAnswer(pod, "", "eth_getBlockByHash", old.Hash.Hex(), false); got == old.Hash.Hex() {
					s.violate(s.name, "pod-%d: eth_getBlockByHash(%s) answered the old chain's block after the reset", pod+1, short(old.Hash))
				}
			}
			var taps []*clientBase
			for _, c := range s.heads {
				taps = append(taps, &c.clientBase)
			}
			for _, g := range s.grpcs {
				taps = append(taps, &g.clientBase)
			}
			for _, c := range s.logs {
				taps = append(taps, &c.clientBase)
			}
			restarted := func(c *clientBase) bool {
				c.mu.Lock()
				defer c.mu.Unlock()
				return slices.ContainsFunc(c.deliveries, func(d delivery) bool { return d.number < c16First && d.at.After(at) })
			}
			waiting := func(c *clientBase) bool { return !restarted(c) }
			for tapsBy := at.Add(c16Follow + logsCloseSpread + reconnectSlack); time.Now().Before(tapsBy) && slices.ContainsFunc(taps, waiting); {
				time.Sleep(scrapeEvery)
			}
			s.endExcuse(ex, time.Now())
			for _, c := range taps {
				if !restarted(c) {
					s.violate(c.name, "received no block of the new chain within %s of the reset", time.Since(at).Round(time.Second))
				}
			}
			for range c5WatchBlocks {
				for pod := range s.h.pods {
					s.requireLatestFollows(pod)
				}
				s.blocks(1)
			}
		}}
}

// requireCountersFollow: on every pod, each upstream's latest block, and
// its shared counter, reach the chain's tip within one full poll and stand
// at or below it: no instance keeps a counter of the old chain (§14).
func (s *scenario) requireCountersFollow() {
	near := func(v float64) bool {
		tip := s.chain.Tip().Number
		return uint64(v) <= tip && uint64(v)+headSlackBlocks >= tip
	}
	within := c16FullPoll + liveBound + reconnectSlack
	for key := range s.vendors {
		up := s.upstream(key)
		for pod := range s.h.pods {
			if v, ok := s.waitMetric(pod, "erpc_upstream_latest_block_number", map[string]string{"upstream": up}, within, near); !ok {
				s.violate(s.name, "pod-%d: %s's latest block %v, not within %d of the tip %d", pod+1, up, v, headSlackBlocks, s.chain.Tip().Number)
			}
		}
		redisKey := upstreamCounterKey(s.upSpec(key), "latestBlock")
		deadline := time.Now().Add(within)
		for {
			st, err := s.h.redis.counter(redisKey)
			if err == nil && near(float64(st.Value)) {
				break
			}
			if time.Now().After(deadline) {
				s.violate(s.name, "shared counter of %s is %d (by %s, err %v), not within %d of the tip %d", up, st.Value, st.UpdatedBy, err, headSlackBlocks, s.chain.Tip().Number)
				break
			}
			time.Sleep(scrapeEvery)
		}
	}
}

// C17: the chain makes no block for c17Halt, past maxBackfill, then
// c17Blocks blocks on demand, each 0 to c17MaxGap after the last, as an
// Orbit chain that mints on traffic. No upstream is benched, W stays
// below its cap while the blocks flow, and every tap gets every block
// within its bound (§6.1, §6.5 rule 1, §6.7; §19 C17).
func c17() spec {
	return spec{name: "C17", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		benched := func(pod int) float64 {
			v, _ := s.metric(pod, "erpc_upstream_head_benched_total", nil)
			return v
		}
		bench0 := make([]float64, len(s.h.pods))
		for pod := range s.h.pods {
			bench0[pod] = benched(pod)
		}
		s.chain.Halt(true)
		s.logf("no blocks for %s from %d", c17Halt, s.chain.Tip().Number)
		time.Sleep(c17Halt)
		capped := make([]bool, len(s.h.pods))
		watchW := func(d time.Duration) {
			for end := time.Now().Add(d); ; {
				for pod := range s.h.pods {
					bt, ok := s.metric(pod, "erpc_network_dynamic_block_time_milliseconds", nil)
					w := time.Duration(min(max(3*bt, float64(liveBound.Milliseconds())), float64(wCap.Milliseconds()))) * time.Millisecond
					if ok && w >= wCap && !capped[pod] {
						capped[pod] = true
						s.violate(s.name, "pod-%d: block time %vms, W at its %s cap, while blocks flow after a %s halt", pod+1, bt, wCap, c17Halt)
					}
				}
				if time.Now().After(end) {
					return
				}
				time.Sleep(time.Second)
			}
		}
		for i := range c17Blocks {
			s.chain.Burst(1, 0)
			b := s.chain.Tip()
			gap := time.Duration(rand.Int64N(int64(c17MaxGap) + 1))
			s.logf("on-demand block %d/%d: %d %s; next in %s", i+1, c17Blocks, b.Number, short(b.Hash), gap.Round(time.Second))
			watchW(max(gap, healthyDelivery))
		}
		for pod := range s.h.pods {
			bt, _ := s.metric(pod, "erpc_network_dynamic_block_time_milliseconds", nil)
			s.logf("pod-%d: block time %vms after %d on-demand blocks", pod+1, bt, c17Blocks)
			if v := benched(pod); v > bench0[pod] {
				s.violate(s.name, "pod-%d: %v benches across a %s halt and on-demand blocks", pod+1, v-bench0[pod], c17Halt)
			}
		}
		s.chain.Halt(false)
		s.blocks(25)
	}}
}

// C18: one upstream, a wss vendor that finalizes c18FinalityDepth below
// its head. With the chain halted at H, pod-1 polls the finalized tag: its
// own poll answers H − c18FinalityDepth on the pinned path, which covers
// that pin. The other pod reads Head.Finalized from the shared counter
// only. A reorg then replaces H − 2 … H. Each pod counts
// final_hash_changed exactly when its own finalized poll covered the
// reverted pin, and every client is told of the reorg (§6.5 finality; §19
// C18).
func c18() spec {
	return spec{name: "C18", bound: healthyDelivery,
		vendors: []vendorSpec{{"ws-a", "vendor-a", true}},
		faults:  map[string]chainsim.Faults{"ws-a": {FinalityDepth: c18FinalityDepth}},
		upEvm:   map[string]map[string]interface{}{"ws-a": {"statePollerInterval": c18PollInterval.String()}},
		body: func(s *scenario) {
			s.blocks(50)
			up := s.upstream("ws-a")
			polled := func(pod int) float64 {
				v, _ := s.metric(pod, "erpc_upstream_finalized_block_polled_total", map[string]string{"upstream": up})
				return v
			}
			alarms := func(pod int) float64 {
				v, _ := s.metric(pod, "erpc_chain_tracker_invariant_violations_total", map[string]string{"invariant": "final_hash_changed"})
				return v
			}
			finalized := map[string]string{"lane": "all", "axis": "finalized"}
			s.chain.Halt(true)
			time.Sleep(healthyDelivery + reconnectSlack)
			head := s.chain.Tip()
			fin := head.Number - c18FinalityDepth
			polled0, alarms0 := make([]float64, len(s.h.pods)), make([]float64, len(s.h.pods))
			for pod := range s.h.pods {
				polled0[pod], alarms0[pod] = polled(pod), alarms(pod)
			}
			for deadline := time.Now().Add(httpCheckWait); ; {
				got := s.blockAnswer(0, "", "eth_getBlockByNumber", "finalized", false)
				v, _ := s.metric(0, "erpc_network_served_tip_block_number", finalized)
				if polled(0) > polled0[0] && uint64(v) == fin {
					s.logf("pod-1 polled finalized %d with the chain halted at %d (tag answered %s)", fin, head.Number, got)
					break
				}
				if time.Now().After(deadline) {
					s.violate("harness", "pod-1: no finalized poll reached %d within %s (polls %v → %v, Head.Finalized %v, tag %s)", fin, httpCheckWait, polled0[0], polled(0), v, got)
					s.chain.Halt(false)
					return
				}
				time.Sleep(scrapeEvery)
			}
			covered := make([]bool, len(s.h.pods))
			for pod := range s.h.pods {
				if v, ok := s.waitMetric(pod, "erpc_network_served_tip_block_number", finalized, httpCheckWait, func(v float64) bool { return uint64(v) == fin }); !ok {
					s.violate(s.name, "pod-%d: Head.Finalized %v, not the vendor's %d, %s after pod-1 polled it", pod+1, v, fin, httpCheckWait)
				}
				covered[pod] = polled(pod) > polled0[pod]
				s.logf("pod-%d: own finalized poll since the halt: %v", pod+1, covered[pod])
			}
			if !slices.Contains(covered, false) {
				s.violate("harness", "every pod polled finalized during the halt: no pod holds Finalized without its own evidence")
			}
			r := s.reorg(c18Depth)
			s.chain.Halt(false)
			if r.old[0].Number != fin {
				s.violate("harness", "the reorg reverts from %d, not the finalized pin %d", r.old[0].Number, fin)
			}
			time.Sleep(deepReorgDelivery + reconnectSlack)
			for pod := range s.h.pods {
				d := alarms(pod) - alarms0[pod]
				switch {
				case covered[pod] && d == 0:
					s.violate(s.name, "pod-%d: its own finalized poll answered %d %s on the pinned path, the reorg reverted it, and final_hash_changed did not count", pod+1, fin, short(r.old[0].Hash))
				case !covered[pod] && d > 0:
					s.violate(s.name, "pod-%d: %v final_hash_changed for the revert of %d…%d, which no finalized poll of its own covered (Head.Finalized %d from the shared counter)", pod+1, d, r.old[0].Number, head.Number, fin)
				default:
					s.logf("pod-%d: final_hash_changed +%v, own evidence %v", pod+1, d, covered[pod])
				}
			}
			s.requireReorgsTold()
			s.blocks(25)
		}}
}
