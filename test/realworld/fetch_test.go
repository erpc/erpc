package realworld

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// Bounds of the fetch-schedule scenarios (spec §6.8; §19 U1, U16, U17, C4,
// C7, D8).
const (
	// u1Slow, u1FaultBlocks: http-a answers every call this late, for this
	// many blocks (§19 U1).
	u1Slow        = 15 * time.Second
	u1FaultBlocks = 30
	// u1HedgeDelay: the network's hedge delay (config, not spec). A read
	// whose first attempt reaches http-a is answered by the hedge within
	// u1HedgedBound: the delay plus one healthy answer (harness slack).
	u1HedgeDelay  = 300 * time.Millisecond
	u1HedgedBound = u1HedgeDelay + time.Second
	// u1UpstreamTimeout outlasts u1Slow, so a client call to http-a ends
	// in an answer, never in a failure its breaker could count; only a
	// tracker fetch, bound by its own 10s (§15.1 scheduler: fetch hangs),
	// fails on it.
	u1UpstreamTimeout = u1Slow + 5*time.Second
	// u1ProbeDepth: probes read this far below the head, a block every
	// counter reached, so the reached-N hint keeps the policy's order and
	// http-a goes first (§6.1 routing hints).
	u1ProbeDepth = 10

	// u16Reask: the 10 min re-ask of a refused newHeads (§5.1; §15.1 wss
	// source), shortened through wsupstream.HeadsReaskInterval.
	u16Reask = 20 * time.Second
	// pollWindow: how long requirePolledAtCadence watches (harness).
	pollWindow = 10 * time.Second

	// u17SkipExpiry: the 10 min re-check of a latest give-up (§6.8; §15.1
	// poller give-up), shortened through evm.LatestBlockSkipExpiry.
	u17SkipExpiry = 40 * time.Second
	// u17Refusals: refused latest polls before the give-up (§6.8).
	u17Refusals = 10
	// u17Settled: refusals that stopped this long are over (harness).
	u17Settled = 3 * time.Second

	// c4SplitBlocks: blocks the wss voters stay split 1-vs-1 (harness).
	c4SplitBlocks = 20

	// c7Halt, c7Burst, c7BurstSpan: the chain halts this long, then mints
	// this many blocks in this span (§19 C7); together far inside
	// maxBackfill (§9).
	c7Halt      = 20 * time.Second
	c7Burst     = 100
	c7BurstSpan = time.Second

	// d8Budget: each upstream's budget, requests per second on each pod;
	// d8LoadFactor: client load on each pod over the sum of its budgets,
	// so every budget stays spent (harness); d8Load: the load's length and
	// d8LoadDepth how far below the head its reads ask (§19 D8).
	d8Budget     = 20
	d8LoadFactor = 3
	d8Load       = 30 * time.Second
	d8LoadDepth  = 10
	// d8Method: the load reads with the method head fetches use, since a
	// request budget counts each method apart (upstream/ratelimiter_budget.go).
	d8Method = "eth_getBlockByNumber"
)

// U1: http-a answers every call u1Slow late for u1FaultBlocks blocks. The
// selection policy ranks it first and the network hedges after
// u1HedgeDelay. No tap waits more than W: the other voters carry every
// block. Reads whose first attempt reaches http-a are hedged. No breaker
// opens on http-a, whose only failures are tracker fetches that time out
// (§19 U1; §8: tracker fetches skip the breaker).
func u1() spec {
	return spec{name: "U1", bound: healthyDelivery,
		network: map[string]interface{}{
			"selectionPolicy": rankFirst("http-a"),
			"failsafe": []map[string]interface{}{{
				"matchMethod": "*",
				"hedge":       map[string]interface{}{"delay": u1HedgeDelay.String(), "maxCount": 1},
			}},
		},
		up: map[string]map[string]interface{}{"http-a": {"failsafe": []map[string]interface{}{{
			"matchMethod": "*",
			"timeout":     map[string]interface{}{"duration": u1UpstreamTimeout.String()},
			"circuitBreaker": map[string]interface{}{
				"failureThresholdCount": 2, "failureThresholdCapacity": 4, "halfOpenAfter": "5m",
				"successThresholdCount": 1, "successThresholdCapacity": 1,
			},
		}}}},
		body: func(s *scenario) {
			s.blocks(25)
			va, up := s.vendors["http-a"], s.upstream("http-a")
			hedged := make([]float64, len(s.h.pods))
			for pod := range s.h.pods {
				hedged[pod], _ = s.metric(pod, "erpc_network_hedged_request_total", nil)
			}
			slow0 := va.Stats().SlowAnswers
			probe := s.chain.CanonAt(s.chain.Tip().Number - u1ProbeDepth)
			va.Update(func(f *chainsim.Faults) { f.Slow = u1Slow })
			s.logf("%s answers %s late for %d blocks", up, u1Slow, u1FaultBlocks)
			for end := time.Now().Add(u1FaultBlocks * blockTime); time.Now().Before(end); s.blocks(2) {
				for pod := range s.h.pods {
					start := time.Now()
					got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(probe.Number), false)
					took := time.Since(start)
					if got != probe.Hash.Hex() {
						s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) answered %s, not %s", pod+1, probe.Number, got, short(probe.Hash))
					}
					if took > u1HedgedBound {
						s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) answered in %s with %s ranked first and %s late: not hedged within %s",
							pod+1, probe.Number, took.Round(time.Millisecond), up, u1Slow, u1HedgedBound)
					}
				}
			}
			va.Update(func(f *chainsim.Faults) { f.Slow = 0 })
			if va.Stats().SlowAnswers == slow0 {
				s.violate("harness", "no call reached %s during its fault: nothing exercised the hedge", up)
			}
			for pod := range s.h.pods {
				if v, _ := s.metric(pod, "erpc_network_hedged_request_total", nil); v <= hedged[pod] {
					s.violate(s.name, "pod-%d: erpc_network_hedged_request_total stayed %v while %s answered %s late", pod+1, v, up, u1Slow)
				}
			}
			// Calls sent during the fault answer up to u1Slow after it.
			time.Sleep(u1Slow)
			s.blocks(25)
			for pod := range s.h.pods {
				if v, _ := s.projectMetric(pod, "erpc_upstream_breaker_state_change_total", map[string]string{"upstream": up, "transition": "closed_to_open"}); v > 0 {
					s.violate(s.name, "pod-%d: %s's breaker opened %v times; only tracker fetches failed on it", pod+1, up, v)
				}
			}
		}}
}

// projectMetric sums a metric without a network label (the breaker's) on
// pod for this scenario's project, plus labels.
func (s *scenario) projectMetric(pod int, name string, labels map[string]string) (float64, bool) {
	ss, err := s.h.pods[pod].scrape()
	if err != nil {
		return 0, false
	}
	match := map[string]string{"project": s.project}
	maps.Copy(match, labels)
	return sumOf(ss, name, match)
}

// U16: ws-a refuses eth_subscribe newHeads as unsupported. Every pod polls
// it at its cadence, so its hash vote stays within W. It asks again on
// each reconnect and every u16Reask, and nothing else reconnects it; once
// the refusal ends, the next ask subscribes and ws-a votes by push (§19
// U16; §5.1; §5.2; §15.1 wss source: newHeads refused as unsupported).
func u16() spec {
	return spec{name: "U16", bound: healthyDelivery,
		faults: map[string]chainsim.Faults{"ws-a": {NoNewHeads: true}},
		vars:   map[string]time.Duration{"wsupstream.HeadsReaskInterval": u16Reask},
		body: func(s *scenario) {
			s.blocks(25)
			ws, px, up, pods := s.vendors["ws-a"], s.proxies["ws-a"], s.upstream("ws-a"), int64(len(s.h.pods))
			s.requirePolledAtCadence("ws-a")

			refused, acc := ws.Stats().HeadsRefused, make([]int64, pods)
			for i, p := range s.h.pods {
				acc[i] = px.accepts(p)
				px.closeAll(p)
			}
			cut := time.Now()
			reconnected := func() bool {
				for i, p := range s.h.pods {
					if px.accepts(p) == acc[i] {
						return false
					}
				}
				return ws.Stats().HeadsRefused-refused >= pods
			}
			for !reconnected() && time.Since(cut) < silentReconnect+reconnectSlack {
				time.Sleep(100 * time.Millisecond)
			}
			if n := ws.Stats().HeadsRefused - refused; n < pods {
				s.violate(s.name, "%d newHeads asks to %s within %s of closing every pod's socket; each reconnect asks again", n, up, silentReconnect+reconnectSlack)
			}
			s.logf("every pod reconnected %s and asked newHeads again after %s", up, time.Since(cut).Round(100*time.Millisecond))

			window := 5 * u16Reask / 2
			r0, c0 := ws.Stats().HeadsRefused, ws.Stats().Connects
			time.Sleep(window)
			n, conns := ws.Stats().HeadsRefused-r0, ws.Stats().Connects-c0
			s.logf("%d newHeads asks and %d connections to %s in %s", n, conns, up, window)
			if conns > 0 {
				s.violate(s.name, "%d connections to %s in %s with no fault on its sockets: a refused newHeads reconnected", conns, up, window)
			}
			if n < 2*pods || n > 3*pods {
				s.violate(s.name, "%d newHeads asks to %s in %s, outside [%d, %d]: not once per %s per pod", n, up, window, 2*pods, 3*pods, u16Reask)
			}

			subs := ws.Stats().HeadsSubscribes
			voted := make([]float64, pods)
			votedLabels := map[string]string{"upstream": up, "outcome": "voted"}
			for pod := range s.h.pods {
				voted[pod], _ = s.metric(pod, "erpc_chain_tracker_source_heads_total", votedLabels)
			}
			ws.Update(func(f *chainsim.Faults) { f.NoNewHeads = false })
			lifted := time.Now()
			for ws.Stats().HeadsSubscribes-subs < pods && time.Since(lifted) < u16Reask+reconnectSlack {
				time.Sleep(100 * time.Millisecond)
			}
			if n := ws.Stats().HeadsSubscribes - subs; n < pods {
				s.violate(s.name, "%d newHeads subscriptions to %s within %s of it supporting newHeads", n, up, u16Reask+reconnectSlack)
			}
			for pod := range s.h.pods {
				if _, ok := s.waitMetric(pod, "erpc_chain_tracker_source_heads_total", votedLabels, liveBound+scrapeEvery, func(v float64) bool { return v > voted[pod] }); !ok {
					s.violate(s.name, "pod-%d: %s subscribed newHeads but voted no pushed header", pod+1, up)
				}
			}
			s.blocks(25)
		}}
}

// requirePolledAtCadence: for pollWindow, the upstream of vendor key is a
// hash voter on every pod at each scrape (its hash age within W, plus the
// gauge's refresh), and its vendor answered at least one latest poll per W
// per pod (§6.1: a polled voter's hash vote lasts W; §6.8: a poll source
// is Asked once per cadence).
func (s *scenario) requirePolledAtCadence(key string) {
	v, up := s.vendors[key], s.upstream(key)
	from := time.Now()
	late := make([]bool, len(s.h.pods))
	for time.Since(from) < pollWindow {
		for pod := range s.h.pods {
			if age, ok := s.metric(pod, "erpc_upstream_report_age_seconds", hashAge(up)); !late[pod] && (!ok || age > (liveBound+ageGaugeEvery).Seconds()) {
				late[pod] = true
				s.violate(s.name, "pod-%d: %s hash age %vs (exported %v), above W %s plus the gauge refresh: not polled at its cadence", pod+1, up, age, ok, liveBound)
			}
		}
		time.Sleep(scrapeEvery)
	}
	calls := len(v.LatestCalls(from, time.Now()))
	if want := len(s.h.pods) * int(pollWindow/liveBound); calls < want {
		s.violate(s.name, "%s answered %d latest polls in %s, below one per W per pod (%d)", up, calls, pollWindow, want)
	}
	s.logf("%s answered %d latest polls in %s", up, calls, pollWindow)
}

// U17: http-a, an https poll source (subscriptionSource: true), refuses
// latest as unsupported from boot. Each pod's poller gives up after
// u17Refusals refusals: http-a stops being a poll source, and its latest
// calls fall below one per W per pod. The refusal then ends, but the
// give-up holds until its re-check u17SkipExpiry after it began; then
// http-a is polled at its cadence again: never a stuck poll source (§19
// U17; §6.8; §15.1 poller give-up).
func u17() spec {
	return spec{name: "U17", bound: healthyDelivery,
		faults: map[string]chainsim.Faults{"http-a": {RefuseLatest: true}},
		upEvm:  map[string]map[string]interface{}{"http-a": {"subscriptionSource": true}},
		vars:   map[string]time.Duration{"evm.LatestBlockSkipExpiry": u17SkipExpiry},
		body: func(s *scenario) {
			v, up, pods := s.vendors["http-a"], s.upstream("http-a"), len(s.h.pods)
			first, last, refused := awaitRefusalsEnd(s, v)
			s.logf("%s refused %d latest calls from %s to %s", up, refused, first.Format("15:04:05.000"), last.Format("15:04:05.000"))
			if refused < u17Refusals*pods {
				s.violate(s.name, "%s refused %d latest calls, fewer than %d per pod: the give-up was never reached", up, refused, u17Refusals)
			}
			v.Update(func(f *chainsim.Faults) { f.RefuseLatest = false })
			// The earliest give-up began after the first refusal.
			time.Sleep(time.Until(first.Add(u17SkipExpiry)))
			quiet := time.Since(last)
			if calls, limit := len(v.LatestCalls(last.Add(time.Nanosecond), time.Now())), pods*int(quiet/liveBound); calls >= limit {
				s.violate(s.name, "%s answered %d latest calls in the %s after its last refusal, a poll source's rate (%d): no give-up", up, calls, quiet.Round(time.Second), limit)
			}
			recheck := time.Until(last.Add(u17SkipExpiry + reconnectSlack))
			for pod := range s.h.pods {
				if _, ok := s.waitMetric(pod, "erpc_upstream_report_age_seconds", hashAge(up), recheck, func(v float64) bool { return v <= liveBound.Seconds() }); !ok {
					s.violate(s.name, "pod-%d: %s no hash voter %s after its give-up began, past its %s re-check: a stuck poll source", pod+1, up, u17SkipExpiry+reconnectSlack, u17SkipExpiry)
				}
			}
			s.requirePolledAtCadence("http-a")
			s.blocks(25)
		}}
}

// awaitRefusalsEnd waits until v's refused latest calls stopped for
// u17Settled, or podBootWait passed, and returns when the first and the
// last came, and how many there were.
func awaitRefusalsEnd(s *scenario, v *chainsim.Vendor) (first, last time.Time, n int) {
	deadline := time.Now().Add(podBootWait)
	for {
		first, last, n = time.Time{}, time.Time{}, 0
		for _, c := range v.LatestCalls(time.Time{}, time.Now()) {
			if c.Refused {
				if n == 0 {
					first = c.At
				}
				last, n = c.At, n+1
			}
		}
		if n > 0 && time.Since(last) >= u17Settled || time.Now().After(deadline) {
			return first, last, n
		}
		time.Sleep(scrapeEvery)
	}
}

// C4: two wss voters and one https voter. ws-b moves to a sibling of the
// next block and stays on that branch for c4SplitBlocks, so the wss voters
// split 1-vs-1 at every height. Settles ask http-c, which serves the
// canonical branch: only the settled hash is sent (§19 C4; §6.5 ties
// settle; §6.8 settle asks voters without a fresh hash vote, then
// non-voters).
func c4() spec {
	return spec{name: "C4", bound: settleDelivery,
		vendors: []vendorSpec{{"ws-a", "vendor-a", true}, {"ws-b", "vendor-b", true}, {"http-c", "vendor-c", false}},
		body: func(s *scenario) {
			s.blocks(25)
			decided := make([]float64, len(s.h.pods))
			outcome := map[string]string{"outcome": "decided"}
			for pod := range s.h.pods {
				decided[pod], _ = s.metric(pod, "erpc_chain_tracker_settle_total", outcome)
			}
			wsb := s.vendors["ws-b"]
			canon, f := s.chain.StartSplit(wsb)
			s.mu.Lock()
			s.fork = wsb.Head()
			s.mu.Unlock()
			s.logf("ws-b split at %d: %s canonical, %s on ws-b", canon.Number, short(canon.Hash), short(s.fork.Hash))
			s.blocks(c4SplitBlocks)
			s.chain.EndFork(f)
			s.logf("ws-b back on the canonical chain")
			s.blocks(25)
			s.requireNoMinorityDelivered()
			for pod := range s.h.pods {
				if v, _ := s.metric(pod, "erpc_chain_tracker_settle_total", outcome); v <= decided[pod] {
					s.violate(s.name, "pod-%d: erpc_chain_tracker_settle_total{decided} stayed %v through %d split heights", pod+1, v, c4SplitBlocks)
				}
			}
		}}
}

// C7: the chain halts for c7Halt, then mints c7Burst blocks in
// c7BurstSpan. Trackers catch up inside maxBackfill with no re-anchor;
// taps read at most reorgWindow blocks per batch and the next Read
// continues, so no tap restarts or goes out of range, and every header
// client gets every burst block within the healthy bound of its first
// header (§19 C7; §9 window; §15.2 Batch).
func c7() spec {
	return spec{name: "C7", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		reanchor, outOfRange := make([]float64, len(s.h.pods)), make([]float64, len(s.h.pods))
		for pod := range s.h.pods {
			reanchor[pod], _ = s.metric(pod, "erpc_chain_tracker_reanchor_total", nil)
			outOfRange[pod], _ = s.metric(pod, "erpc_tap_out_of_range_total", nil)
		}
		s.chain.Halt(true)
		s.logf("halted at %d for %s", s.chain.Tip().Number, c7Halt)
		time.Sleep(c7Halt)
		from := s.chain.Tip().Number + 1
		s.chain.Burst(c7Burst, c7BurstSpan)
		to := s.chain.Tip().Number
		s.chain.Halt(false)
		s.logf("minted %d to %d in %s", from, to, c7BurstSpan)
		s.blocks(50)
		for pod := range s.h.pods {
			if v, _ := s.metric(pod, "erpc_chain_tracker_reanchor_total", nil); v > reanchor[pod] {
				s.violate(s.name, "pod-%d: re-anchored %v times catching up %d blocks inside maxBackfill", pod+1, v-reanchor[pod], c7Burst)
			}
			if v, _ := s.metric(pod, "erpc_tap_out_of_range_total", nil); v > outOfRange[pod] {
				s.violate(s.name, "pod-%d: %v taps out of range on a %d-block burst", pod+1, v-outOfRange[pod], c7Burst)
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
			got := map[uint64]bool{}
			c.mu.Lock()
			for _, d := range c.deliveries {
				got[d.number] = true
			}
			c.mu.Unlock()
			for n := from; n <= to; n++ {
				if !got[n] {
					s.violate(c.name, "never received burst block %d (burst %d to %d)", n, from, to)
					break
				}
			}
		}
	}}
}

// D8: one wss source and two https non-sources, each with a budget of
// d8Budget requests per second on every pod. For d8Load, client reads run
// at d8LoadFactor times the sum of the budgets on each pod, so every
// budget is spent. Head Asks keep fetching on each pod
// (erpc_head_ask_total{outcome="fetched"} rises), and no tap waits more
// than W (§19 D8; §6.8 Ask).
func d8() spec {
	vendors := []vendorSpec{{"ws-a", "vendor-a", true}, {"http-a", "vendor-a", false}, {"http-b", "vendor-b", false}}
	budgets := map[string]uint32{}
	for _, v := range vendors {
		budgets[v.key] = d8Budget
	}
	return spec{name: "D8", bound: healthyDelivery, vendors: vendors, budgets: budgets, body: func(s *scenario) {
		s.blocks(25)
		limited := map[string][]float64{}
		spent := func(pod int, key string) float64 {
			n, _ := s.metric(pod, "erpc_rate_limits_total", map[string]string{"upstream": s.upstream(key), "category": d8Method})
			return n
		}
		fetched := func(pod int) (float64, bool) {
			return s.metric(pod, "erpc_head_ask_total", map[string]string{"outcome": "fetched"})
		}
		for _, v := range vendors {
			limited[v.key] = make([]float64, len(s.h.pods))
			for pod := range s.h.pods {
				limited[v.key][pod] = spent(pod, v.key)
			}
		}
		asked := make([]float64, len(s.h.pods))
		for pod := range s.h.pods {
			asked[pod], _ = fetched(pod)
		}
		ctx, cancel := context.WithTimeout(s.ctx, d8Load)
		defer cancel()
		var sent, failed atomic.Int64
		var wg sync.WaitGroup
		for pod := range s.h.pods {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.load(ctx, pod, d8LoadFactor*len(vendors)*d8Budget, &sent, &failed)
			}()
		}
		wg.Wait()
		s.logf("client load: %d reads in %s, %d failed", sent.Load(), d8Load, failed.Load())
		for pod := range s.h.pods {
			switch n, found := fetched(pod); {
			case !found:
				s.violate(s.name, "pod-%d does not export erpc_head_ask_total{outcome=\"fetched\"}", pod+1)
			case n <= asked[pod]:
				s.violate(s.name, "pod-%d: no head Ask fetched under client load (erpc_head_ask_total{outcome=\"fetched\"} %v)", pod+1, n)
			}
		}
		for _, v := range vendors {
			for pod := range s.h.pods {
				if n := spent(pod, v.key); n <= limited[v.key][pod] {
					s.violate("harness", "pod-%d: the load never spent %s's %s budget (erpc_rate_limits_total %v)", pod+1, s.upstream(v.key), d8Method, n)
				}
			}
		}
		s.blocks(25)
	}}
}

// load sends d8Method for a block d8LoadDepth below the head to pod, rate
// times a second until ctx ends; each read runs on its own, so a slow
// answer never lowers the rate. An old block raises no head counter.
func (s *scenario) load(ctx context.Context, pod, rate int, sent, failed *atomic.Int64) {
	c, err := rpc.DialOptions(ctx, s.h.pods[pod].httpURL(s.project, s.chain.ID()),
		rpc.WithHTTPClient(&http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: rate}, Timeout: httpCheckWait}))
	if err != nil {
		s.violate("harness", "pod-%d: load client: %v", pod+1, err)
		return
	}
	defer c.Close()
	at := hexutil.EncodeUint64(s.chain.Tip().Number - d8LoadDepth)
	t := time.NewTicker(time.Second / time.Duration(rate))
	defer t.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out json.RawMessage
			err := c.CallContext(ctx, &out, d8Method, at, false)
			sent.Add(1)
			if err != nil && ctx.Err() == nil {
				failed.Add(1)
			}
		}()
	}
}
