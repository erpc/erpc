package realworld

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// Bounds of the upstream-fault scenarios (spec §6.4, §8, §15.1; §19 U2,
// U4–U7, U13, U15).
const (
	// u2Cuts: the receipts cut stays on until every pod had this many
	// connections to http-a cut half way, within u2CutBudget blocks that all
	// carry logs. u2FaultBlocks: how long the header cut lasts (harness).
	u2Cuts        = 15
	u2CutBudget   = 60
	u2FaultBlocks = 30

	// fetchTimeout: one tracker fetch gives up after 10s (§8; §15.1
	// scheduler: fetch hangs).
	fetchTimeout = 10 * time.Second
	// u4Recover: a pool whose sockets went dead costs one fetch timeout,
	// then new connections answer within a poll cadence of W (§19 U4).
	u4Recover = fetchTimeout + liveBound + reconnectSlack

	// u5Blocks: every vendor answers 429 this many blocks (§19 U5);
	// u5ResumeBlocks: delivery resumes within this many block times of
	// the end (§19 U5, C8).
	u5Blocks       = 4
	u5ResumeBlocks = 2

	// u6Fault: vendor-a answers -32005 compute units this long; then
	// every vendor does for u6AllFault (§19 U6). u6GrowBlocks: the class
	// a refusal everywhere halved gains a slot back within this many blocks.
	u6Fault      = 60 * time.Second
	u6AllFault   = 10 * time.Second
	u6GrowBlocks = 25

	// u7Below: U7 reads blocks from this far below the head when its
	// clients opened, below every vendor's head and Latest (§19 U7).
	// u7Probes: how many it reads.
	u7Below  = 10
	u7Probes = 10

	// u13Ahead: http-b's bogus head jump, past the major-move threshold of
	// 60s of chain (§5.3 check 2: identity is verified on a major jump).
	// u13Probe: a height above Latest and far below that claim.
	u13Ahead = 1000
	u13Probe = 50
	// u13Recheck: the 5 min eth_chainId re-check of a cordon (§5.3; §15.1
	// chain-identity cordon), shortened through evm.ChainIdCordonRecheck;
	// 2 matches in a row uncordon. u13PollInterval: http-b's state poll,
	// so its jump reaches every pod within a second (harness).
	u13Recheck      = 20 * time.Second
	u13PollInterval = time.Second

	// u20Pruned: http-a keeps only this many blocks below its head, as a
	// tip-only plan does (§19 U20). u20Depth: a gRPC cursor this far below
	// the head, inside the window (maxBackfill of chain) and past
	// http-a's history. u20History: the chain holds that depth from its
	// start. u20Rounds: how many times each pod is resumed that deep.
	u20Pruned  = 100
	u20Depth   = 200
	u20History = 2 * u20Depth
	u20Rounds  = 3
)

// U2: a network of 3 https vendors, http-a ranked first, http-b and
// http-c clean, all poll sources, so a cut voter never holds Latest by
// quorum. http-a's answers are cut half way (FIN, then RST): first every
// receipts body, over blocks that all carry logs so every block's
// receipts are fetched, until each pod had u2Cuts of them, then every
// header answer, with time to recover between, since the header cuts
// demote http-a in routing. Each sweep tries http-a first and fails over
// at once, no header of http-a is voted malformed, and every client's logs
// equal the canonical logs within the delivery bound (§19 U2; §5.3 check
// 0; §8 one sweep).
func u2() spec {
	return spec{name: "U2", bound: healthyDelivery,
		vendors: []vendorSpec{{"http-a", "vendor-a", false}, {"http-b", "vendor-b", false}, {"http-c", "vendor-c", false}},
		network: map[string]interface{}{"selectionPolicy": rankFirst("http-a")},
		upEvm: map[string]map[string]interface{}{
			"http-a": {"subscriptionSource": true},
			"http-b": {"subscriptionSource": true},
			"http-c": {"subscriptionSource": true},
		},
		body: func(s *scenario) {
			px := s.proxies["http-a"]
			// cutA cuts every pod's http-a answers to method half way while
			// hold runs, and returns how many connections each pod had cut.
			cutA := func(what, method string, hold func(cuts func() []int64)) []int64 {
				base := make([]int64, len(s.h.pods))
				for i, p := range s.h.pods {
					base[i] = px.resets(p)
					px.set(p, linkFaults{RSTMid: method})
				}
				cuts := func() []int64 {
					n := make([]int64, len(s.h.pods))
					for i, p := range s.h.pods {
						n[i] = px.resets(p) - base[i]
					}
					return n
				}
				s.logf("http-a: cut half way into %s", what)
				hold(cuts)
				for _, p := range s.h.pods {
					px.set(p, linkFaults{})
				}
				return cuts()
			}

			s.blocks(25)
			// Each tick mints a planned block and plans one more, so every
			// block of the cut carries logs and at most 2 planned outlive it.
			s.chain.Plan(chainsim.KindMatching, chainsim.KindMatching)
			blocks := 0
			n := cutA("a receipts body", "eth_getBlockReceipts", func(cuts func() []int64) {
				for ; blocks < u2CutBudget && slices.Min(cuts()) < u2Cuts; blocks++ {
					s.chain.WaitTicks(1)
					s.chain.Plan(chainsim.KindMatching)
				}
			})
			for i, c := range n {
				if c < u2Cuts {
					s.violate("harness", "pod-%d: %d connections to http-a cut half way into a receipts body over %d blocks that all carry logs (want %d)", i+1, c, blocks, u2Cuts)
				} else {
					s.logf("pod-%d: %d connections to http-a cut half way into a receipts body over %d blocks", i+1, c, blocks)
				}
			}
			s.blocks(25)

			n = cutA("a header answer", "eth_getBlockByNumber", func(func() []int64) { s.blocks(u2FaultBlocks) })
			for i, c := range n {
				if c == 0 {
					s.violate("harness", "pod-%d: no connection to http-a was cut half way into a header answer", i+1)
				} else {
					s.logf("pod-%d: %d connections to http-a cut half way into a header answer", i+1, c)
				}
			}
			s.blocks(50)
			for pod := range s.h.pods {
				for _, key := range []string{"http-a"} {
					up := s.upstream(key)
					heads := func(outcome string) (float64, bool) {
						return s.metric(pod, "erpc_chain_tracker_source_heads_total", map[string]string{"upstream": up, "outcome": outcome})
					}
					if v, found := heads("voted"); !found || v == 0 {
						s.violate(s.name, "pod-%d: erpc_chain_tracker_source_heads_total{outcome=voted} of %s is %v (exported %v): a source that votes nothing", pod+1, up, v, found)
					}
					if v, _ := heads("malformed"); v > 0 {
						s.violate(s.name, "pod-%d: %v heads of %s counted malformed: a cut answer reached intake", pod+1, v, up)
					}
				}
			}
		}}
}

// U4: http-a, a poll source, has its keep-alive pool black-holed on every
// pod: each open socket stays open and carries nothing. A pod spends one
// fetch timeout on it, then opens new connections, and http-a is polled at
// its cadence again within u4Recover; Latest keeps moving throughout
// (§19 U4; §8 fetch timeout).
func u4() spec {
	return spec{name: "U4", bound: healthyDelivery,
		upEvm: map[string]map[string]interface{}{"http-a": {"subscriptionSource": true}},
		body: func(s *scenario) {
			s.blocks(25)
			px, up := s.proxies["http-a"], s.upstream("http-a")
			stop := s.watchLatest(func(pod int, bn, tip uint64) {
				s.violate(s.name, "pod-%d: eth_blockNumber %d against the tip %d while %s's pool is black-holed (slack %d)", pod+1, bn, tip, up, headSlackBlocks)
			})
			acc := make([]int64, len(s.h.pods))
			for i, p := range s.h.pods {
				acc[i] = px.accepts(p)
				if n := px.holdOpen(p); n == 0 {
					s.violate("harness", "pod-%d holds no open connection to %s to black-hole", i+1, up)
				} else {
					s.logf("pod-%d: %d open connections to %s black-holed", i+1, n, up)
				}
			}
			at := time.Now()
			time.Sleep(time.Until(at.Add(u4Recover)))
			for i, p := range s.h.pods {
				if px.accepts(p) == acc[i] {
					s.violate(s.name, "pod-%d opened no new connection to %s within %s of its pool going dead", i+1, up, u4Recover)
				}
			}
			s.requirePolledAtCadence("http-a")
			stop()
			s.blocks(25)
		}}
}

// U5: every vendor answers 429 (-32005 over ws) for u5Blocks blocks. No
// upstream is benched, and delivery resumes within u5ResumeBlocks block
// times of the end (§19 U5; §15.1 poll source: errors, 429).
func u5() spec {
	return spec{name: "U5", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		throttleAll(s)
		s.blocks(25)
	}}
}

// throttleAll runs U5's fault and checks: every vendor throttled for
// u5Blocks blocks, which the chain plans as matching blocks so every logs
// client is owed a log of each; afterwards no upstream was benched, every
// header client received a block, and every logs client every throttled
// block's logs, within u5ResumeBlocks block times of the end, plus the
// next mint and the tracker's processing.
func throttleAll(s *scenario) {
	bt := s.chain.BlockTime()
	benched := func(pod int, key string) float64 {
		v, _ := s.metric(pod, "erpc_upstream_head_benched_total", map[string]string{"upstream": s.upstream(key)})
		return v
	}
	bench0 := map[string][]float64{}
	for key := range s.vendors {
		for pod := range s.h.pods {
			bench0[key] = append(bench0[key], benched(pod, key))
		}
	}
	kinds := make([]chainsim.Kind, u5Blocks+u5ResumeBlocks+1)
	for i := range kinds {
		kinds[i] = chainsim.KindMatching
	}
	s.chain.Plan(kinds...)
	thr0 := map[string]int64{}
	for key, v := range s.vendors {
		thr0[key] = v.Stats().Throttled
		v.Update(func(f *chainsim.Faults) { f.Throttled = true })
	}
	start := time.Now()
	s.logf("every vendor throttled for %d blocks", u5Blocks)
	s.blocks(u5Blocks)
	for _, v := range s.vendors {
		v.Update(func(f *chainsim.Faults) { f.Throttled = false })
	}
	end := time.Now()
	resumeBy := end.Add(u5ResumeBlocks*bt + bt + tapProcessingSlack)
	time.Sleep(time.Until(resumeBy.Add(reconnectSlack)))
	for key, v := range s.vendors {
		if v.Stats().Throttled == thr0[key] {
			s.violate("harness", "%s answered no call with 429 while throttled", s.upstream(key))
		}
	}
	for key := range s.vendors {
		for pod := range s.h.pods {
			if n := benched(pod, key); n > bench0[key][pod] {
				s.violate(s.name, "pod-%d: %s benched %v times through a %d-block 429 on every vendor", pod+1, s.upstream(key), n-bench0[key][pod], u5Blocks)
			}
		}
	}
	var cs []*clientBase
	for _, c := range s.heads {
		cs = append(cs, &c.clientBase)
	}
	for _, g := range s.grpcs {
		cs = append(cs, &g.clientBase)
	}
	for _, c := range cs {
		if !c.deliveredBetween(end, resumeBy) {
			s.violate(c.name, "no block delivered within %s of the 429s ending", resumeBy.Sub(end))
		}
	}
	for _, c := range s.logs {
		f := chainsim.Filters[c.filter]
		for _, b := range s.chain.MintedBetween(start, end) {
			if len(s.chain.CanonicalLogs(f, b.Number, b.Number)) == 0 {
				continue
			}
			if at, ok := c.deliveredAt(b.Hash); !ok || at.After(resumeBy) {
				s.violate(c.name, "logs of block %d, minted under the 429s, delivered %s after they ended (received %v), bound %s", b.Number, at.Sub(end).Round(time.Millisecond), ok, resumeBy.Sub(end))
			}
		}
	}
}

// deliveredBetween reports whether the client received a block in (from, to].
func (c *clientBase) deliveredBetween(from, to time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.deliveries {
		if d.at.After(from) && !d.at.After(to) {
			return true
		}
	}
	return false
}

// deliveredAt is when the client first received block h.
func (c *clientBase) deliveredAt(h hash) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.deliveries {
		if d.hash == h {
			return d.at, true
		}
	}
	return time.Time{}, false
}

// U6: vendor-a (ws-a, http-a, http-a ranked first) answers -32005
// compute units on every call for u6Fault. The selection policy bypasses
// it: each sweep succeeds on vendor-b, so no class of the fetch scheduler
// loses a slot, and every client is served. Then every vendor refuses for
// u6AllFault: each pod halves the lowest busy class's share, which grows
// back once capacity returns (§19 U6; §8: one throttled upstream is the
// policy's job, a share halves when every upstream tried was out of
// capacity, whatever is in flight).
func u6() spec {
	return spec{name: "U6", bound: healthyDelivery,
		network: map[string]interface{}{"selectionPolicy": rankFirst("http-a")},
		body: func(s *scenario) {
			s.blocks(25)
			classes := []string{"head", "tip_receipts", "fill"}
			slots0 := map[string][]float64{}
			exported := false
			for _, class := range classes {
				for pod := range s.h.pods {
					v, found := s.metric(pod, "erpc_chain_tracker_fetch_slots", map[string]string{"class": class})
					if !found {
						v = -1
					}
					exported = exported || found
					slots0[class] = append(slots0[class], v)
				}
			}
			if !exported {
				s.violate(s.name, "no pod exports erpc_chain_tracker_fetch_slots for %s (§17)", s.network())
			}
			refused0 := int64(0)
			for _, key := range vendorA {
				refused0 += s.vendors[key].Stats().CapacityRefused
				s.vendors[key].Update(func(f *chainsim.Faults) { f.Capacity = true })
			}
			s.logf("vendor-a answers -32005 compute units for %s", u6Fault)
			flagged := map[string]bool{}
			for end := time.Now().Add(u6Fault); time.Now().Before(end); time.Sleep(scrapeEvery) {
				for _, class := range classes {
					for pod := range s.h.pods {
						key := fmt.Sprintf("%s/%d", class, pod)
						v, found := s.metric(pod, "erpc_chain_tracker_fetch_slots", map[string]string{"class": class})
						if found && slots0[class][pod] >= 0 && v < slots0[class][pod] && !flagged[key] {
							flagged[key] = true
							s.violate(s.name, "pod-%d: erpc_chain_tracker_fetch_slots{class=%s} fell from %v to %v with one vendor of two out of capacity", pod+1, class, slots0[class][pod], v)
						}
					}
				}
			}
			refused := int64(0)
			for _, key := range vendorA {
				s.vendors[key].Update(func(f *chainsim.Faults) { f.Capacity = false })
				refused += s.vendors[key].Stats().CapacityRefused
			}
			if refused == refused0 {
				s.violate("harness", "vendor-a refused no call for capacity: nothing exercised the bypass")
			}
			s.logf("vendor-a refused %d calls for capacity", refused-refused0)
			s.blocks(5)
			refuseEverywhere(s, classes)
		}}
}

// refuseEverywhere runs U6's second fault: every vendor answers -32005
// compute units for u6AllFault. Each pod halves some class's
// erpc_chain_tracker_fetch_slots, and every class it halved has a slot
// more u6GrowBlocks blocks after the refusals end. Clients are excused
// until delivery may resume, as for U5.
func refuseEverywhere(s *scenario, classes []string) {
	slots := func(pod int, class string) float64 {
		v, _ := s.metric(pod, "erpc_chain_tracker_fetch_slots", map[string]string{"class": class})
		return v
	}
	start := make([]map[string]float64, len(s.h.pods))
	low := make([]map[string]float64, len(s.h.pods))
	for pod := range s.h.pods {
		start[pod], low[pod] = map[string]float64{}, map[string]float64{}
		for _, class := range classes {
			start[pod][class] = slots(pod, class)
			low[pod][class] = start[pod][class]
		}
	}
	ex := s.excuse(-1, time.Now())
	for _, v := range s.vendors {
		v.Update(func(f *chainsim.Faults) { f.Capacity = true })
	}
	s.logf("every vendor answers -32005 compute units for %s", u6AllFault)
	for end := time.Now().Add(u6AllFault); time.Now().Before(end); time.Sleep(scrapeEvery) {
		for pod := range s.h.pods {
			for _, class := range classes {
				low[pod][class] = min(low[pod][class], slots(pod, class))
			}
		}
	}
	for _, v := range s.vendors {
		v.Update(func(f *chainsim.Faults) { f.Capacity = false })
	}
	bt := s.chain.BlockTime()
	s.endExcuse(ex, time.Now().Add(u5ResumeBlocks*bt+bt+tapProcessingSlack))
	s.blocks(u6GrowBlocks)
	for pod := range s.h.pods {
		halved := false
		for _, class := range classes {
			if start[pod][class] < 2 || low[pod][class] > start[pod][class]/2 {
				continue
			}
			halved = true
			if now := slots(pod, class); now <= low[pod][class] {
				s.violate(s.name, "pod-%d: erpc_chain_tracker_fetch_slots{class=%s} fell from %v to %v while every vendor refused for capacity, and is %v %d blocks later", pod+1, class, start[pod][class], low[pod][class], now, u6GrowBlocks)
			}
		}
		if !halved {
			s.violate(s.name, "pod-%d: no class's erpc_chain_tracker_fetch_slots halved while every vendor refused for capacity for %s: from %v, lowest %v", pod+1, u6AllFault, start[pod], low[pod])
		}
	}
}

// U7: http-a, ranked first, answers null for every block by number and by
// hash, at or below its own head, which keeps moving. A read of a block
// below Latest that skips cache reads (the tracker's own fetches cache
// every recent block) reaches http-a, is retried on another upstream and
// answers the block; a second read, cache allowed, answers it too: the
// null was never cached (§19 U7; §6.4 N ≤ Latest is served; §7).
func u7() spec {
	return spec{name: "U7", cache: true, bound: healthyDelivery,
		network: map[string]interface{}{"selectionPolicy": rankFirst("http-a")},
		body: func(s *scenario) {
			base := s.chain.Tip().Number - u7Below
			s.blocks(25)
			fresh := make([]*rpc.Client, len(s.h.pods))
			for pod, p := range s.h.pods {
				c, err := rpc.DialOptions(s.ctx, p.httpURL(s.project, s.chain.ID()), rpc.WithHTTPClient(p.httpc), rpc.WithHeader("X-ERPC-Skip-Cache-Read", "true"))
				if err != nil {
					s.t.Fatal(err)
				}
				defer c.Close()
				fresh[pod] = c
			}
			hv := s.vendors["http-a"]
			hv.Update(func(f *chainsim.Faults) { f.NullHeld = true })
			s.logf("%s answers null for every block", s.upstream("http-a"))
			asked, dispatched := 0, 0
			for i := range uint64(u7Probes) {
				s.blocks(2)
				b := s.chain.CanonAt(base - i)
				from := time.Now()
				for pod := range s.h.pods {
					for _, q := range []struct{ method, arg string }{
						{"eth_getBlockByNumber", hexutil.EncodeUint64(b.Number)},
						{"eth_getBlockByHash", b.Hash.Hex()},
					} {
						var raw json.RawMessage
						ctx, cancel := context.WithTimeout(s.ctx, httpCheckWait)
						err := fresh[pod].CallContext(ctx, &raw, q.method, q.arg, false)
						cancel()
						for read, got := range []string{answerHash(raw, err), s.blockAnswer(pod, "", q.method, q.arg, false)} {
							if got != b.Hash.Hex() {
								s.violate(s.name, "pod-%d: %s(%s), read %d, answered %s, not block %d %s", pod+1, q.method, q.arg, read+1, got, b.Number, short(b.Hash))
							}
						}
					}
				}
				asked += hv.NumberCalls(b.Number, false, from, time.Now())
				dispatched += s.dispatches(b.Number, false, from, time.Now())
			}
			hv.Update(func(f *chainsim.Faults) { f.NullHeld = false })
			s.logf("probe reads reached %s %d times, every vendor %d times", s.upstream("http-a"), asked, dispatched)
			if asked == 0 {
				s.violate("harness", "no read reached %s while it answered null (every vendor: %d): nothing exercised the retry", s.upstream("http-a"), dispatched)
			}
			s.blocks(25)
		}}
}

// U13: http-b reports a head u13Ahead above its own and answers another
// chain id, with shortCircuitFutureBlocks on. The jump's identity check
// cordons it: it leaves the ceiling, so a block above Latest inside its
// claim is short-circuited. Its chain id then matches again; it stays
// cordoned through the first matching re-check and is uncordoned by the
// second, back in the ceiling: the same block is dispatched (§19 U13;
// §5.3; §6.4 ceiling; §15.1 chain-identity cordon).
func u13() spec {
	return spec{name: "U13", bound: healthyDelivery,
		evm:   map[string]interface{}{"shortCircuitFutureBlocks": true},
		upEvm: map[string]map[string]interface{}{"http-b": {"statePollerInterval": u13PollInterval.String()}},
		vars:  map[string]time.Duration{"evm.ChainIdCordonRecheck": u13Recheck},
		body: func(s *scenario) {
			s.blocks(25)
			up, hb := s.upstream("http-b"), s.vendors["http-b"]
			cordon := map[string]string{"upstream": up, "category": "*"}
			hb.Update(func(f *chainsim.Faults) { f.Ahead, f.WrongChainID = u13Ahead, true })
			s.logf("%s reports +%d and answers another chain id", up, u13Ahead)
			for pod := range s.h.pods {
				if v, ok := s.waitMetric(pod, "erpc_upstream_cordoned", cordon, u13PollInterval+liveBound+reconnectSlack, func(v float64) bool { return v > 0 }); !ok {
					s.violate(s.name, "pod-%d: %s not cordoned (erpc_upstream_cordoned %v) after answering another chain id on a %d-block jump", pod+1, up, v, u13Ahead)
				}
			}
			s.requireCeiling(false, "while "+up+" is cordoned")
			hb.Update(func(f *chainsim.Faults) { f.WrongChainID = false })
			fixed := time.Now()
			s.logf("%s answers the network's chain id again", up)
			time.Sleep(time.Until(fixed.Add(u13Recheck - reconnectSlack)))
			for pod := range s.h.pods {
				if v, found := s.metric(pod, "erpc_upstream_cordoned", cordon); !found || v == 0 {
					s.violate(s.name, "pod-%d: %s uncordoned %s after its chain id matched again, before a second re-check %s apart", pod+1, up, time.Since(fixed).Round(time.Second), u13Recheck)
				}
			}
			for pod := range s.h.pods {
				deadline := fixed.Add(2*u13Recheck + reconnectSlack)
				for {
					v, found := s.metric(pod, "erpc_upstream_cordoned", cordon)
					if found && v == 0 {
						s.logf("pod-%d: %s uncordoned %s after its chain id matched again", pod+1, up, time.Since(fixed).Round(time.Second))
						break
					}
					if time.Now().After(deadline) {
						s.violate(s.name, "pod-%d: %s still cordoned (%v, exported %v) %s after its chain id matched again: 2 re-checks %s apart uncordon it", pod+1, up, v, found, 2*u13Recheck+reconnectSlack, u13Recheck)
						break
					}
					time.Sleep(scrapeEvery)
				}
			}
			s.requireCeiling(true, "once "+up+" is uncordoned")
			hb.Update(func(f *chainsim.Faults) { f.Ahead = 0 })
			s.blocks(25)
		}}
}

// requireCeiling probes the tip + u13Probe, a block no vendor has, on
// every pod: inside the ceiling (in) it is dispatched, at most
// refusalDispatches times; outside it is short-circuited with no
// dispatch. Either way it answers null.
func (s *scenario) requireCeiling(in bool, when string) {
	for pod := range s.h.pods {
		n := s.chain.Tip().Number + u13Probe
		from := time.Now()
		got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(n), false)
		if in {
			s.owe(pod, "retried_missing", 1)
		}
		d := s.dispatches(n, false, from, time.Now())
		switch {
		case in && d == 0:
			s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) short-circuited %s: the ceiling left it out", pod+1, n, when)
		case in && d > refusalDispatches:
			s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d), in the band, reached upstreams %d times (at most %d) %s", pod+1, n, d, refusalDispatches, when)
		case !in && d > 0:
			s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) dispatched %d times %s: its claim still raises the ceiling", pod+1, n, d, when)
		}
		if got != "null" {
			s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d), a block no vendor has, answered %s %s", pod+1, n, got, when)
		}
	}
}

// U15: vendor-a (ws-a, http-a, http-a ranked first) answers
// eth_getBlockReceipts with "method not found" from boot. Every logs
// subscription is accepted, one opened mid-run included, and every
// client's logs equal the canonical logs, from vendor-b (§19 U15; §8 one
// sweep; §11).
func u15() spec {
	noReceipts := chainsim.Faults{NoReceipts: true}
	return spec{name: "U15", bound: healthyDelivery,
		network: map[string]interface{}{"selectionPolicy": rankFirst("http-a")},
		faults:  map[string]chainsim.Faults{"ws-a": noReceipts, "http-a": noReceipts},
		body: func(s *scenario) {
			s.blocks(25)
			for pod := range s.h.pods {
				s.addLogs(fmt.Sprintf("p%d-logs-late", pod+1), pod, 0)
			}
			s.blocks(50)
			if s.receiptsCalls([]string{"http-a"})+s.receiptsCalls([]string{"ws-a"}) == 0 {
				s.violate("harness", "no receipts or logs call reached vendor-a: its missing method was never exercised")
			}
		}}
}

// U20: http-a, ranked first, has pruned everything more than u20Pruned
// below its head: it answers HTTP 403 with an HTML body (U20-403) or null
// (U20-null) there. Fill and resume use the other vendors: a gRPC stream
// resumed u20Depth below the head, on every pod, gets every canonical
// header from the cursor + 1 to the head, and no tap goes out of range
// (§9 window; §10.3; §19 U20).
func u20(forbidden bool) spec {
	name := "U20-null"
	if forbidden {
		name = "U20-403"
	}
	return spec{name: name, bound: healthyDelivery, sim: chainsim.Config{History: u20History},
		network: map[string]interface{}{"selectionPolicy": rankFirst("http-a")},
		faults:  map[string]chainsim.Faults{"http-a": {Pruned: u20Pruned, PrunedForbidden: forbidden}},
		body: func(s *scenario) {
			s.blocks(25)
			oor := make([]float64, len(s.h.pods))
			for pod := range s.h.pods {
				oor[pod] = s.tapsLost(pod)
			}
			for range u20Rounds {
				for pod := range s.h.pods {
					s.resumeDeep(pod, u20Depth)
				}
				s.blocks(25)
			}
			if n := s.vendors["http-a"].Stats().PrunedRefused; n == 0 {
				s.violate("harness", "http-a refused no call for pruned history: nothing asked it below its head − %d", u20Pruned)
			} else {
				s.logf("http-a refused %d calls for pruned history", n)
			}
			for pod := range s.h.pods {
				if v := s.tapsLost(pod); v > oor[pod] {
					s.violate(s.name, "pod-%d: %v taps out of range while http-a's history was pruned", pod+1, v-oor[pod])
				}
			}
		}}
}

// resumeDeep resumes a StreamBlocks stream on pod from the canonical block
// depth below the head and reads it up to that head: every header is the
// canonical one after the last, with no end.
func (s *scenario) resumeDeep(pod int, depth uint64) {
	tip := s.chain.Tip()
	cur := s.chain.CanonAt(tip.Number - depth)
	if cur == nil {
		s.violate("harness", "the chain holds no block %d below its head %d", depth, tip.Number)
		return
	}
	rctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	n := cur.Number
	ev := s.openStream(rctx, pod, &n, cur.Hash.Bytes())
	start := time.Now()
	for prev := cur; prev.Number < tip.Number; {
		e, ok := next(ev, d3Reply)
		switch {
		case !ok:
			s.violate(s.name, "pod-%d: resumed %d deep from %d %s: nothing after %d within %s", pod+1, depth, cur.Number, short(cur.Hash), prev.Number, d3Reply)
			return
		case e.h == nil:
			s.violate(s.name, "pod-%d: resumed %d deep from %d %s: ended %s %s after %d", pod+1, depth, cur.Number, short(cur.Hash), e.code, e.msg, prev.Number)
			return
		}
		want := s.chain.CanonAt(prev.Number + 1)
		if got := gethcommon.BytesToHash(e.h.Hash); e.h.Number != want.Number || got != want.Hash {
			s.violate(s.name, "pod-%d: resumed %d deep from %d: header %d %s after %d, not the canonical %d %s", pod+1, depth, cur.Number, e.h.Number, short(got), prev.Number, want.Number, short(want.Hash))
			return
		}
		prev = want
	}
	s.logf("pod-%d: resumed %d deep from %d, read to %d in %s", pod+1, depth, cur.Number, tip.Number, time.Since(start).Round(time.Millisecond))
}
