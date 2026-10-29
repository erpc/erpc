package realworld

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Bounds of Head, the refusal rule and finality (specs/chain-tracker/
// feature.md on the unified-head-engine branch).
const (
	// refusalDispatches: in the refusal band an empty answer is a
	// retryable miss once (§6.4), so a request reaches upstreams at most
	// twice; with shortCircuitFutureBlocks off at most 2 as well (§19 K7).
	refusalDispatches = 2
	// peerBlockNumberWait: another instance's eth_blockNumber reaches a
	// block delivered here within 1 bt, plus the tracker's processing
	// (§19 oracle; P1 "on the other within 1 Ask").
	peerBlockNumberWait = blockTime + tapProcessingSlack
	// finalityMinAge: a voter without a finalized tag reports latest − the
	// derived depth, at least 30 min of chain (§6.4 Finality, #1079).
	finalityMinAge = 30 * time.Minute
	// c13History covers the derived depth at C13's first check, at least
	// 5s into a pod's life: 30 min at the block time floor, (5s − 1s) / 25
	// blocks = 0.16s, is 11,250 blocks (§6.4, §6.7); a later check reads a
	// floor nearer 0.2s, so a shallower depth.
	c13History = 15000
	// c13FinalizedReads bounds the finalized reads a cold pod answers
	// with the vendors' refusal. A tagless voter votes latest − the
	// derived depth after giveUpRefusals (10) refused finalized polls
	// (architecture/evm/evm_state_poller.go), the bootstrap poll the
	// first; one vote decides a ballot with fewer votes than the quorum.
	// Each read refreshes the finalized axis at most once per cadence, on
	// the quorum (2) of the 4 voters in routing order (refreshLatest,
	// §6.8), and that order may rotate: one voter's 9 more polls take at
	// most 9 × 4 / 2 = 18 reads, and the next read answers.
	c13FinalizedReads = 19
	// c13ReadEvery: a finalized read per cold poll cadence (0.7s, §6.7)
	// plus the refresh's round trip.
	c13ReadEvery = time.Second
	// c9BlockTime and coldHeaders: C9's chain, and the headers by which a
	// cold instance's first block time sample lands: it spans the first two
	// timestamp changes (§6.7; §19 C9). c9WatchHeaders: how long C9 checks
	// the estimate after that.
	c9BlockTime    = 400 * time.Millisecond
	coldHeaders    = 6
	c9WatchHeaders = 25
	// blockTimeQuantization: integer-second timestamps at 0.4s blocks make
	// one sample 1s over 2 or 3 blocks, 0.33–0.5s (§6.7, §19 C9).
	blockTimeQuantization = 0.25
	// u12Ahead and u12Probe: U12's bogus report, and a height above Latest
	// and far below that report.
	u12Ahead = 30_000_000
	u12Probe = 50
	// u12ReportWait is harness, not spec: a non-source upstream's head
	// reaches a pod by its state poll, every 30s (§6.8), plus slack.
	u12ReportWait = 40 * time.Second
	// u14Ahead: the syncing archive leads the other vendors by 5; the
	// probe asks Latest + 3 (§19 U14).
	u14Ahead = 5
	u14Probe = 3
	// u19Ban, u19Window: U19's vendor answers 403 for u19Ban; the
	// scenario judges its last u19Window. Ask backoff doubles from the
	// cadence to AskMaxBackoff, 60s, in about 72s (§6.8), so the window
	// sees at most 2 backed-off fetches per pod; u19PodAsks adds harness
	// slack for a fetch in flight at the window's edges.
	u19Ban        = 150 * time.Second
	u19Window     = 60 * time.Second
	u19PodAsks    = 4
	u19Probe      = 10
	u19ProbeEvery = time.Second
	// p6Latency is harness, not spec: every vendor answers this late, so
	// the tip moves while an eth_call is in flight.
	p6Latency = 2 * blockTime
	// p6Advance: the one block Latest may gain between the eth_blockNumber
	// answer and the next request's arrival (§7: one Head read per
	// request).
	p6Advance = 1
	// revertBelowTag: C12's revert starts this many blocks below the lowest
	// finalized tag the pods answered just before it, each read refreshing
	// Head.Finalized (§6.4 Finality; §19 C12); its read heights are the
	// blocks between. Without that read Head.Finalized trails up to one
	// full poll, and a revert above it is followed without
	// final_hash_changed (§6.5).
	revertBelowTag = 5
	// p7FinalityDepth: http-b finalizes this deep, below Head.Finalized
	// (§6.4: permanent writes need the serving upstream's finalized).
	p7FinalityDepth = 200
	// p10ReorgWindow and p10NormalLag: the normal group serves
	// p10NormalLag blocks behind the flashblocks group, more than P10's
	// reorgWindow, so a lane tap that measured lag from the network-wide
	// stamp would restart (§10.2); and fewer than the default selection
	// policy's 10-block lag, which would remove the whole lane.
	p10ReorgWindow = 4
	p10NormalLag   = p10ReorgWindow + 4
	// probeMethod is read by the cache probes only, so its cache writes are
	// theirs.
	probeMethod = "eth_getBlockTransactionCountByNumber"
	// x2Latency is harness, not spec: pod-2 hears every vendor this late,
	// so a block pod-1 delivers is above pod-2's own heads when asked.
	x2Latency = 3 * blockTime
	// p5Spread is harness, not spec: every node but ws-a sees each block
	// this many blocks late, as nodes of several vendors do; Latest
	// (quorum 2) is the tip − p5Spread, and ws-a alone holds the tip.
	p5Spread = 1
)

// X2: 2 pods, each with in-memory shared state; pod-2 serves only numbered
// calls, no client subscribes there, and it hears every vendor x2Latency
// late. Every block a pod-1 client receives is served on pod-2 by number
// on the first call, never null (§6.4 refusal rule: a block above pod-2's
// Latest is refreshed, its reports older than 1 bt Asked, and retried
// once, never answered "future").
func x2() spec {
	return spec{name: "X2", memory: true, numbered: []int{1}, bound: healthyDelivery,
		clients: func(s *scenario) { s.podClients(0, 0) },
		body: func(s *scenario) {
			for _, px := range s.proxies {
				px.set(s.h.pods[1], linkFaults{Latency: x2Latency})
			}
			s.blocks(150)
			for _, rule := range []string{"future", "future_after_refresh", "future_after_wait", "retried_missing"} {
				s.requireZeroIfExported(1, "erpc_head_refusal_total", map[string]string{"rule": rule})
			}
		}}
}

// requireZeroIfExported: the counter is 0 on pod; a counter the pod does
// not export yet (§17 signals land later) is logged, not judged.
func (s *scenario) requireZeroIfExported(pod int, name string, labels map[string]string) {
	v, found := s.metric(pod, name, labels)
	switch {
	case !found:
		s.logf("pod-%d does not export %s%v", pod+1, name, labels)
	case v > 0:
		s.violate(s.name, "pod-%d: %s%v = %v, not 0", pod+1, name, labels, v)
	}
}

// U12: http-b reports a head 30M above its own, same chain id, with
// shortCircuitFutureBlocks on. Latest and the other upstreams' lag stay
// as they were; the ceiling rises, so a block above Latest is dispatched,
// not short-circuited (§6.4 Head: Latest, ceiling; §6.6 lag reference).
func u12() spec {
	return spec{name: "U12", bound: healthyDelivery, evm: map[string]interface{}{"shortCircuitFutureBlocks": true},
		body: func(s *scenario) {
			s.blocks(25)
			up := s.upstream("http-b")
			hb := s.vendors["http-b"]
			stop := s.watchLatest(func(pod int, bn, tip uint64) {
				s.violate(s.name, "pod-%d: eth_blockNumber %d against the tip %d while %s reports +%d", pod+1, bn, tip, up, u12Ahead)
			})
			hb.Update(func(f *chainsim.Faults) { f.Ahead = u12Ahead })
			s.logf("%s reports %d above its head", up, u12Ahead)
			// http-b is no source: its report reaches a pod by its next
			// state poll (u12ReportWait), not by a block. The scenario
			// judges a ceiling the report raised, so it waits for it.
			for pod := range s.h.pods {
				s.awaitUpstreamHead(pod, up, float64(u12Ahead), u12ReportWait)
			}
			s.blocks(25)
			for pod := range s.h.pods {
				for _, key := range []string{"ws-a", "ws-b", "http-a"} {
					v, found := s.metric(pod, "erpc_upstream_block_head_lag", map[string]string{"upstream": s.upstream(key)})
					if !found || v > float64(headSlackBlocks) {
						s.violate(s.name, "pod-%d: erpc_upstream_block_head_lag of %s is %v (exported %v) while %s reports +%d", pod+1, s.upstream(key), v, found, up, u12Ahead)
					}
				}
				n := s.chain.Tip().Number + u12Probe
				from := time.Now()
				got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(n), false)
				s.owe(pod, "retried_missing", 1)
				switch d := s.dispatches(n, false, from, time.Now()); {
				case d == 0:
					s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) answered %s with no dispatch, below the ceiling %s raised", pod+1, n, got, up)
				case d > refusalDispatches:
					s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d), in the band, reached upstreams %d times (at most %d)", pod+1, n, d, refusalDispatches)
				}
				if got != "null" {
					s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d), a block no vendor has, answered %s", pod+1, n, got)
				}
			}
			s.blocks(25)
			stop()
			hb.Update(func(f *chainsim.Faults) { f.Ahead = 0 })
			s.blocks(25)
		}}
}

// U19: http-b answers HTTP 403 with an HTML body, as a CDN ban or a
// revoked key does, for u19Ban, under head+1 polling with
// shortCircuitFutureBlocks on. Its polls fail (§6.1): its Asks back off,
// and it leaves the ceiling, so a block u19Probe above the tip, above
// every other voter's head, is null with no dispatch (§6.4, #1238).
func u19() spec {
	return spec{name: "U19", bound: healthyDelivery, evm: map[string]interface{}{"shortCircuitFutureBlocks": true},
		body: func(s *scenario) {
			s.blocks(25)
			up, hb := s.upstream("http-b"), s.vendors["http-b"]
			hb.Update(func(f *chainsim.Faults) { f.Forbidden = true })
			banned := time.Now()
			s.logf("%s answers 403 with an HTML body", up)
			pollNext(s, true)
			time.Sleep(time.Until(banned.Add(u19Ban - u19Window)))
			from := hb.Stats().ForbiddenLatest
			probes := 0
			for end := time.Now().Add(u19Window); time.Now().Before(end); time.Sleep(u19ProbeEvery) {
				for pod := range s.h.pods {
					n := s.chain.Tip().Number + u19Probe
					at := time.Now()
					got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(n), false)
					probes++
					if d := s.dispatches(n, false, at, time.Now()); got != "null" || d > 0 {
						s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d), above every voter's head while %s answers 403, answered %s after %d dispatches", pod+1, n, up, got, d)
					}
				}
			}
			asks := hb.Stats().ForbiddenLatest - from
			s.logf("%d probes; %s refused %d latest polls in the ban's last %s", probes, up, asks, u19Window)
			if limit := int64(u19PodAsks * len(s.h.pods)); asks > limit {
				s.violate(s.name, "%s, answering 403, got %d latest polls in %s, more than %d: its Asks do not back off", up, asks, u19Window, limit)
			}
			hb.Update(func(f *chainsim.Faults) { f.Forbidden = false })
			s.blocks(25)
		}}
}

// awaitUpstreamHead waits until pod exports a latest block of at least min
// for upstream up on the scenario's network (pods also export a stale
// network="n/a" series per upstream, which a sum would add), or violates
// after within.
func (s *scenario) awaitUpstreamHead(pod int, up string, min float64, within time.Duration) {
	for deadline := time.Now().Add(within); ; time.Sleep(scrapeEvery) {
		if v, ok := s.metric(pod, "erpc_upstream_latest_block_number", map[string]string{"upstream": up}); ok && v >= min {
			s.logf("pod-%d: %s reports %v", pod+1, up, v)
			return
		}
		if time.Now().After(deadline) {
			s.violate(s.name, "pod-%d: %s's report never arrived within %s", pod+1, up, within)
			return
		}
	}
}

// dispatches counts the eth_getBlockByNumber(n) calls with full
// transactions as full that every vendor answered in [from, to].
func (s *scenario) dispatches(n uint64, full bool, from, to time.Time) int {
	d := 0
	for _, v := range s.vendors {
		d += v.NumberCalls(n, full, from, to)
	}
	return d
}

// U14: every vendor serves u14Ahead behind a syncing archive (vendor-c,
// https). eth_getBlockByNumber(Latest + 3) is served by the archive, in
// the refusal band, not null (§6.4: the ceiling counts syncing upstreams;
// the retry orders head ≥ N first). The archive is no source, and its
// state poll keeps the default interval, so its head reaches the ceiling
// by the band's refresh, not a sweep of every upstream.
func u14() spec {
	lag := chainsim.Faults{Lag: u14Ahead}
	return spec{name: "U14", bound: healthyDelivery + u14Ahead*blockTime,
		vendors: []vendorSpec{
			{"ws-a", "vendor-a", true}, {"ws-b", "vendor-b", true},
			{"http-a", "vendor-a", false}, {"http-b", "vendor-b", false},
			{"archive", "vendor-c", false},
		},
		faults: map[string]chainsim.Faults{"ws-a": lag, "ws-b": lag, "http-a": lag, "http-b": lag, "archive": {Syncing: true}},
		body: func(s *scenario) {
			s.blocks(25)
			archive := s.vendors["archive"]
			for i := range 20 {
				pod := i % len(s.h.pods)
				var bn hexutil.Uint64
				if err := s.call(pod, &bn, "eth_blockNumber"); err != nil {
					s.violate(s.name, "pod-%d: eth_blockNumber: %v", pod+1, err)
					continue
				}
				n := uint64(bn) + u14Probe
				from := time.Now()
				got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(n), false)
				to := time.Now()
				want := s.chain.CanonAt(n)
				switch {
				case want == nil || archive.Head().Number < n:
					s.logf("pod-%d: Latest %d + %d is above the archive's %d; skipped", pod+1, bn, u14Probe, archive.Head().Number)
				case got != want.Hash.Hex():
					s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d), Latest %d + %d, answered %s, not the archive's %s", pod+1, n, bn, u14Probe, got, short(want.Hash))
				case archive.NumberCalls(n, false, from, to) == 0:
					s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) answered without asking the archive", pod+1, n)
				case s.dispatches(n, false, from, to) > refusalDispatches:
					s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) reached upstreams %d times (at most %d)", pod+1, n, s.dispatches(n, false, from, to), refusalDispatches)
				}
				s.blocks(5)
			}
			for key, v := range s.vendors {
				if key != "archive" {
					v.Update(func(f *chainsim.Faults) { f.Lag = 0 })
				}
			}
			s.blocks(25)
		}}
}

// P1: U10 (ws-a 40 behind, advancing) and C1 (1–3 deep reorgs). Every
// block a client receives is at or below eth_blockNumber on its pod at
// once (the oracle) and on every other pod within peerBlockNumberWait: the
// first call's pre-forward refresh is one Ask with a number need (§6.8),
// and the answer does not wait for it (§21).
func p1() spec {
	return spec{name: "P1", bound: healthyDelivery, onHead: peerBlockNumber, body: func(s *scenario) {
		s.blocks(25)
		ws := s.vendors["ws-a"]
		ws.Update(func(f *chainsim.Faults) { f.Lag = u10Lag })
		s.logf("ws-a serves %d blocks behind", u10Lag)
		for range 8 {
			s.blocks(15)
			s.reorg(1 + rand.IntN(3))
		}
		ws.Update(func(f *chainsim.Faults) { f.Lag = 0 })
		s.blocks(25)
		s.requireReorgsTold()
	}}
}

func peerBlockNumber(s *scenario, c *headsClient, pod int, n uint64, _ hash) {
	delivered := time.Now()
	s.checks.Add(1)
	go func() {
		defer s.checks.Done()
		for other := range s.h.pods {
			if other == pod {
				continue
			}
			var bn hexutil.Uint64
			if err := s.call(other, &bn, "eth_blockNumber"); err == nil && uint64(bn) >= n {
				continue
			}
			time.Sleep(time.Until(delivered.Add(peerBlockNumberWait)))
			if err := s.call(other, &bn, "eth_blockNumber"); err != nil || uint64(bn) < n {
				s.violate(c.name, "pod-%d: eth_blockNumber %d (err %v) below block %d delivered on pod-%d %s before", other+1, bn, err, n, pod+1, peerBlockNumberWait)
			}
		}
	}()
}

// P5: no fault; every node but ws-a sees each block p5Spread late. For
// every delivered head N: eth_blockNumber ≥ N, and
// getBlockByNumber("latest").number lies between eth_blockNumber read just
// before and just after it: the tag reads the same Latest, never ws-a's
// higher head (§7 families 1 and 2; §20: no post-forward raise).
func p5() spec {
	spread := chainsim.Faults{Lag: p5Spread}
	return spec{name: "P5", bound: healthyDelivery + p5Spread*blockTime,
		faults: map[string]chainsim.Faults{"ws-b": spread, "http-a": spread, "http-b": spread},
		onHead: func(s *scenario, c *headsClient, pod int, n uint64, _ hash) {
			s.checks.Add(1)
			go func() {
				defer s.checks.Done()
				if !s.first(fmt.Sprintf("p5/%d/%d", pod, n)) {
					return
				}
				var before, after hexutil.Uint64
				var latest struct {
					Number hexutil.Uint64 `json:"number"`
				}
				err1 := s.call(pod, &before, "eth_blockNumber")
				err2 := s.call(pod, &latest, "eth_getBlockByNumber", "latest", false)
				err3 := s.call(pod, &after, "eth_blockNumber")
				switch {
				case err1 != nil || err2 != nil || err3 != nil:
					s.violate(c.name, "pod-%d: reads failed: %v, %v, %v", pod+1, err1, err2, err3)
				case uint64(before) < n:
					s.violate(c.name, "pod-%d: eth_blockNumber %d below the delivered %d", pod+1, before, n)
				case latest.Number < before || latest.Number > after:
					s.violate(c.name, "pod-%d: getBlockByNumber(latest).number %d outside eth_blockNumber %d…%d around it", pod+1, latest.Number, before, after)
				}
			}()
		}, body: func(s *scenario) { s.blocks(150) }}
}

// P6: every vendor answers p6Latency late, so the tip moves while a
// request is in flight. eth_call(latest) to Multicall3 getBlockNumber()
// executes at the number the request read from Head (§7 family 3: tags
// interpolated before forward), and is cached under it: the same call at
// that number is a cache hit.
func p6() spec {
	return spec{name: "P6", cache: true, bound: healthyDelivery + 2*p6Latency, body: func(s *scenario) {
		for _, px := range s.proxies {
			for _, p := range s.h.pods {
				px.set(p, linkFaults{Latency: p6Latency})
			}
		}
		s.blocks(10)
		call := map[string]interface{}{"to": chainsim.Multicall3.Hex(), "data": chainsim.GetBlockNumberSig}
		hits := func(pod int) float64 {
			v, _ := s.metric(pod, "erpc_cache_get_success_hit_total", map[string]string{"category": "eth_call"})
			return v
		}
		for i := range 30 {
			pod := i % len(s.h.pods)
			var bn hexutil.Uint64
			var at hexutil.Bytes
			if err := s.call(pod, &bn, "eth_blockNumber"); err != nil {
				s.violate(s.name, "pod-%d: eth_blockNumber: %v", pod+1, err)
				continue
			}
			if err := s.call(pod, &at, "eth_call", call, "latest"); err != nil {
				s.violate(s.name, "pod-%d: eth_call(latest): %v", pod+1, err)
				continue
			}
			r := new(big.Int).SetBytes(at).Uint64()
			if r < uint64(bn) || r > uint64(bn)+p6Advance {
				s.violate(s.name, "pod-%d: eth_call(latest) executed at %d; eth_blockNumber just before was %d", pod+1, r, bn)
			}
			time.Sleep(200 * time.Millisecond)
			h0 := hits(pod)
			var again hexutil.Bytes
			if err := s.call(pod, &again, "eth_call", call, hexutil.EncodeUint64(r)); err != nil || new(big.Int).SetBytes(again).Uint64() != r {
				s.violate(s.name, "pod-%d: eth_call(%d) answered %s (err %v)", pod+1, r, again, err)
			}
			if h1 := hits(pod); h1 <= h0 {
				s.violate(s.name, "pod-%d: eth_call(%d) after eth_call(latest) executed at %d was no cache hit: not cached under the number", pod+1, r, r)
			}
			s.blocks(5)
		}
		for _, px := range s.proxies {
			for _, p := range s.h.pods {
				px.set(p, linkFaults{})
			}
		}
		s.blocks(25)
	}}
}

// C12: the majority (every vendor) reverts from revertBelowTag blocks below
// each pod's finalized tag and each vendor's. Clients follow the new branch
// from the fork point + 1, no permanent cache entry keeps a reverted block
// on any pod: by-number reads before the revert do not survive it (§6.5
// "every reverted pin at or below Finalized … is read past its permanent
// cache entry", §15.3).
// At least one pod logs final_hash_changed: only a pod whose own finalized
// polls covered a reverted pin does (§6.5). The revert is deeper than
// reorgWindow, so newHeads restarts after the reverted batch (a jump) and
// ws logs end 1013 within their close spread (§10.2, §15.1), as in C3.
func c12() spec {
	return spec{name: "C12", cache: true, bound: deepReorgDelivery, allow: []string{"ws:logs-end", "ws:jump"}, body: func(s *scenario) {
		s.blocks(50)
		revertFinalized(s)
		s.blocks(25)
	}}
}

// revertFinalized runs C12's revert and its checks: a finalized-tag read
// on each pod refreshes its Head.Finalized just before the majority
// reverts below it. A pod whose own finalized poll answered that read
// holds the pin with its own evidence and counts final_hash_changed; a pod
// that took the number from the shared counter has no header for it and
// counts none (§6.5, C18's rule).
func revertFinalized(s *scenario) {
	polled := func(pod int) float64 {
		v, _ := s.metric(pod, "erpc_upstream_finalized_block_polled_total", nil)
		return v
	}
	alarms := func(pod int) float64 {
		v, _ := s.metric(pod, "erpc_chain_tracker_invariant_violations_total", map[string]string{"invariant": "final_hash_changed"})
		return v
	}
	covered, fins, alarms0 := make([]bool, len(s.h.pods)), make([]uint64, len(s.h.pods)), make([]float64, len(s.h.pods))
	for pod := range s.h.pods {
		polled0 := polled(pod)
		alarms0[pod] = alarms(pod)
		if b := s.blockOf(s.blockAnswer(pod, "", "eth_getBlockByNumber", "finalized", false)); b != nil {
			fins[pod] = b.Number
			covered[pod] = polled(pod) > polled0
		}
		s.logf("pod-%d: finalized tag %d before the revert, own finalized poll %v", pod+1, fins[pod], covered[pod])
	}
	if !slices.Contains(covered, true) {
		s.violate("harness", "no pod's finalized-tag read polled finalized itself: no pod holds a final pin with its own evidence")
	}
	// The read heights end below the lowest finalized tag a pod answered
	// and below every vendor's: a loaded host slows the reads, and a
	// revert counted back from the tip after them would start above them.
	from := s.chain.Tip().Number - chainsim.FinalizedLag
	for _, f := range fins {
		if f > 0 {
			from = min(from, f)
		}
	}
	from -= revertBelowTag
	var heights []uint64
	for n := from; n < from+revertBelowTag; n++ {
		heights = append(heights, n)
	}
	for pod := range s.h.pods {
		for _, n := range heights {
			if got, want := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(n), false), s.chain.CanonAt(n).Hash.Hex(); got != want {
				s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) before the revert answered %s, not %s", pod+1, n, got, want)
			}
		}
	}
	r := s.reorg(int(s.chain.Tip().Number - from + 1))
	replaced := 0
	for _, n := range heights {
		if n >= r.old[0].Number {
			replaced++
		}
	}
	if replaced == 0 {
		s.violate(s.name, "harness: no read height %d…%d was reverted (fork point %d)", heights[0], heights[len(heights)-1], r.old[0].Number-1)
	}
	time.Sleep(max(deepReorgDelivery, logsCloseSpread) + reconnectSlack)
	for pod := range s.h.pods {
		for _, n := range heights {
			if got, want := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(n), false), s.chain.CanonAt(n).Hash.Hex(); got != want {
				s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) after the revert answered %s, not the new branch's %s: a permanent entry kept the reverted block", pod+1, n, got, want)
			}
		}
		d := alarms(pod) - alarms0[pod]
		switch {
		case fins[pod] < r.old[0].Number:
			s.violate("harness", "pod-%d: the revert from %d is above its finalized tag %d", pod+1, r.old[0].Number, fins[pod])
		case covered[pod] && d == 0:
			s.violate(s.name, "pod-%d: its own finalized poll answered %d, the majority reverted from %d, and final_hash_changed did not count", pod+1, fins[pod], r.old[0].Number)
		case !covered[pod] && d > 0:
			s.violate(s.name, "pod-%d: %v final_hash_changed for the revert from %d, which no finalized poll of its own covered (finalized tag %d from the shared counter)", pod+1, d, r.old[0].Number, fins[pod])
		default:
			s.logf("pod-%d: final_hash_changed +%v, own evidence %v", pod+1, d, covered[pod])
		}
	}
	s.requireReorgsTold()
}

// P7: C12, with http-b finalizing p7FinalityDepth deep and ws-b with no
// finalized tag. The finalized tag answers Head.Finalized (Latest −
// FinalizedLag: the q-th highest voter finalized); a block at or below it
// keeps its hash unless the majority reverts it; a permanent cache write
// needs the serving upstream to have finalized the block (§6.4 Finality,
// #90). vendor-b ranks first, so a finalized refresh in routing order
// would ask http-b and ws-b alone and vendor-a's finalized would stay at
// its first poll; ws-b's finalized report stays the oldest once its tag is
// given up, so a refresh that gave it a place would leave one for the
// other three (§6.8).
func p7() spec {
	return spec{name: "P7", cache: true, bound: deepReorgDelivery, allow: []string{"ws:logs-end", "ws:jump"},
		network: map[string]interface{}{"selectionPolicy": rankFirst("b")},
		faults: map[string]chainsim.Faults{
			"http-b": {FinalityDepth: p7FinalityDepth},
			"ws-b":   {NoFinalized: true},
		},
		body: func(s *scenario) {
			s.blocks(50)
			var finals []*chainsim.Block
			for i := range 10 {
				if b := s.requireFinalizedTag(i % len(s.h.pods)); b != nil {
					finals = append(finals, b)
				}
				s.blocks(2)
			}
			if len(finals) > 0 {
				f := finals[len(finals)-1].Number
				// http-b finalizes p7FinalityDepth deep, so f−10 is final
				// for the network but not for http-b; ws-a finalized f−20.
				s.requirePermanence(f-10, s.upstream("http-b"), false)
				s.requirePermanence(f-20, s.upstream("ws-a"), true)
			}
			revertFinalized(s)
			fork := s.reorgs[len(s.reorgs)-1].old[0].Number
			for _, b := range finals {
				if b.Number >= fork {
					continue
				}
				for pod := range s.h.pods {
					if got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(b.Number), false); got != b.Hash.Hex() {
						s.violate(s.name, "pod-%d: finalized block %d %s, below the revert's fork point, now answers %s", pod+1, b.Number, short(b.Hash), got)
					}
				}
			}
			s.blocks(25)
		}}
}

// requireFinalizedTag reads the finalized tag on pod between two
// eth_blockNumber reads; it is Latest − FinalizedLag, the finalized every
// vendor but http-b reports, as fresh as the voters' finalized polls: the
// tag refreshes them, at most once per finalized debounce, so it may
// trail by up to W (§6.8 #1265). It returns the answered block.
func (s *scenario) requireFinalizedTag(pod int) *chainsim.Block {
	var before, after hexutil.Uint64
	err1 := s.call(pod, &before, "eth_blockNumber")
	got := s.blockAnswer(pod, "", "eth_getBlockByNumber", "finalized", false)
	err2 := s.call(pod, &after, "eth_blockNumber")
	b := s.blockOf(got)
	switch {
	case err1 != nil || err2 != nil:
		s.violate(s.name, "pod-%d: eth_blockNumber: %v, %v", pod+1, err1, err2)
	case b == nil:
		s.violate(s.name, "pod-%d: eth_getBlockByNumber(finalized) answered %s", pod+1, got)
	case b.Number+chainsim.FinalizedLag+uint64(liveBound/blockTime) < uint64(before) || b.Number+chainsim.FinalizedLag > uint64(after):
		s.violate(s.name, "pod-%d: finalized is %d; Head.Finalized is Latest − %d, at most W behind, Latest %d…%d", pod+1, b.Number, chainsim.FinalizedLag, before, after)
	}
	return b
}

// requirePermanence reads probeMethod at the nearest block at or below n
// that holds transactions, from upstream up only, and checks whether the
// answer went to the permanent connector.
func (s *scenario) requirePermanence(n uint64, up string, permanent bool) {
	b := s.probeAt(n)
	if b == nil {
		s.violate(s.name, "harness: no block of the chain at or below %d holds transactions", n)
		return
	}
	labels := map[string]string{"connector": cacheFinal, "category": probeMethod}
	for pod := range s.h.pods {
		before, _ := s.metric(pod, "erpc_cache_set_success_total", labels)
		if err := s.probe(pod, up, b); err != nil {
			s.violate(s.name, "pod-%d: %s(%d) via %s: %v", pod+1, probeMethod, b.Number, up, err)
			continue
		}
		after, _ := s.waitMetric(pod, "erpc_cache_set_success_total", labels, 2*time.Second, func(v float64) bool { return v > before })
		switch {
		case permanent && after <= before:
			s.violate(s.name, "pod-%d: control: %s(%d) from %s, which finalized it, was not cached permanently", pod+1, probeMethod, b.Number, up)
		case !permanent && after > before:
			s.violate(s.name, "pod-%d: %s(%d) from %s, which has not finalized it, was cached permanently", pod+1, probeMethod, b.Number, up)
		}
	}
}

// probeAt is the nearest canonical block at or below n that holds
// transactions, nil when no block of the chain does.
func (s *scenario) probeAt(n uint64) *chainsim.Block {
	b := s.chain.CanonAt(n)
	for b != nil && b.TxCount() == 0 {
		b = s.chain.CanonAt(b.Number - 1)
	}
	return b
}

// probe reads probeMethod at b on pod through selector sel; any answer but
// b's transaction count is an error that quotes it.
func (s *scenario) probe(pod int, sel string, b *chainsim.Block) error {
	var raw json.RawMessage
	if err := s.callAs(pod, sel, &raw, probeMethod, hexutil.EncodeUint64(b.Number)); err != nil {
		return err
	}
	var count hexutil.Uint64
	if err := json.Unmarshal(raw, &count); err != nil || int(count) != b.TxCount() {
		return fmt.Errorf("answered %s, not %d transactions", raw, b.TxCount())
	}
	return nil
}

// C13: a chain without a finalized tag on every vendor. Within
// c13FinalizedReads reads, one per c13ReadEvery, the finalized tag answers
// latest − the derived depth, at least finalityMinAge of chain (§6.4
// Finality); reads before it answer the vendors' refusal. The cache uses
// it: a block below it is cached permanently, one above it only as recent.
func c13() spec {
	return spec{name: "C13", cache: true, bound: healthyDelivery, sim: chainsim.Config{History: c13History, NoFinalized: true},
		body: func(s *scenario) {
			s.blocks(25)
			for pod := range s.h.pods {
				got := s.blockAnswer(pod, "", "eth_getBlockByNumber", "finalized", false)
				reads := 1
				for ; strings.HasPrefix(got, "error: ") && reads < c13FinalizedReads; reads++ {
					time.Sleep(c13ReadEvery)
					got = s.blockAnswer(pod, "", "eth_getBlockByNumber", "finalized", false)
				}
				s.logf("pod-%d: eth_getBlockByNumber(finalized) read %d answered %s", pod+1, reads, got)
				fin := s.blockOf(got)
				latest := s.blockOf(s.blockAnswer(pod, "", "eth_getBlockByNumber", "latest", false))
				switch {
				case fin == nil:
					s.violate(s.name, "pod-%d: eth_getBlockByNumber(finalized) read %d answered %s", pod+1, reads, got)
					continue
				case latest == nil:
					s.violate(s.name, "pod-%d: eth_getBlockByNumber(latest) answered no block", pod+1)
					continue
				case time.Duration(latest.Time-fin.Time)*time.Second < finalityMinAge:
					s.violate(s.name, "pod-%d: finalized %d is %s of chain below latest %d, under %s", pod+1, fin.Number, time.Duration(latest.Time-fin.Time)*time.Second, latest.Number, finalityMinAge)
				}
				s.requireCachedAs(pod, fin.Number-10, cacheFinal)
				s.requireCachedAs(pod, fin.Number+(latest.Number-fin.Number)/2, cacheRecent)
			}
			s.blocks(25)
		}}
}

// requireCachedAs reads probeMethod at the nearest block at or below n that
// holds transactions on pod and checks which connector its answer went to.
func (s *scenario) requireCachedAs(pod int, n uint64, connector string) {
	b := s.probeAt(n)
	if b == nil {
		s.violate(s.name, "harness: no block of the chain at or below %d holds transactions", n)
		return
	}
	set := func(c string) float64 {
		v, _ := s.metric(pod, "erpc_cache_set_success_total", map[string]string{"connector": c, "category": probeMethod})
		return v
	}
	f0, r0 := set(cacheFinal), set(cacheRecent)
	if err := s.probe(pod, "", b); err != nil {
		s.violate(s.name, "pod-%d: %s(%d): %v", pod+1, probeMethod, b.Number, err)
		return
	}
	time.Sleep(time.Second)
	f1, r1 := set(cacheFinal), set(cacheRecent)
	if got := map[bool]string{true: cacheFinal, false: cacheRecent}[f1 > f0]; (f1 > f0) == (r1 > r0) || got != connector {
		s.violate(s.name, "pod-%d: %s(%d) cached as final +%v, recent +%v; wants %s only", pod+1, probeMethod, b.Number, f1-f0, r1-r0, connector)
	}
}

// C9: 0.4s blocks with integer-second timestamps. pod-1 restarts cold; its
// block time stays the cold 1s (no gauge) until the second timestamp
// change, at most coldHeaders headers of one source, then reads the
// chain's within the timestamps' quantization; never 0 or 12s, and W stays
// at its 2s floor (§6.7).
func c9() spec {
	return spec{name: "C9", bound: healthyDelivery, sim: chainsim.Config{BlockTime: c9BlockTime},
		clients: func(s *scenario) {
			s.podClients(1, 0)
			s.addHeadsOn("p1-heads-pinned", 0, "", true, "ws:heads-end")
		},
		body: func(s *scenario) {
			s.blocks(10)
			p := s.h.pods[0]
			ex := s.excuse(0, time.Now())
			p.kill()
			if err := p.start(); err != nil {
				s.t.Fatalf("pod-1 restart: %v", err)
			}
			s.logf("pod-1 restarted cold")
			// A read adds the counts before the restart; the cold process's
			// own are those above them.
			voted0 := map[string]float64{}
			for key := range s.vendors {
				voted0[key] = p.counters.carriedSum("erpc_chain_tracker_source_heads_total", map[string]string{"project": s.project, "network": s.network(), "upstream": s.upstream(key), "outcome": "voted"})
			}
			want := float64(c9BlockTime.Milliseconds())
			firstAt := -1.0
			deadline := time.Now().Add(podBootWait)
			for {
				if time.Now().After(deadline) {
					s.violate(s.name, "pod-1: no source delivered %d headers within %s of the restart", coldHeaders+c9WatchHeaders, podBootWait)
					break
				}
				time.Sleep(50 * time.Millisecond)
				ss, err := p.scrape()
				if err != nil {
					continue
				}
				most := 0.0
				for key := range s.vendors {
					v, _ := sumOf(ss, "erpc_chain_tracker_source_heads_total", map[string]string{"project": s.project, "network": s.network(), "upstream": s.upstream(key), "outcome": "voted"})
					most = math.Max(most, v-voted0[key])
				}
				bt, found := sumOf(ss, "erpc_network_dynamic_block_time_milliseconds", map[string]string{"project": s.project, "network": s.network()})
				if !found {
					if most > coldHeaders {
						s.violate(s.name, "pod-1: block time still cold after %v voted headers of one source (bound %d)", most, coldHeaders)
						break
					}
					continue
				}
				if firstAt < 0 {
					firstAt = most
					s.logf("pod-1: first block time %vms after %v headers", bt, most)
				}
				w := math.Max(3*bt, 2000)
				if bt == 0 || bt >= 12000 || math.Abs(bt-want) > want*blockTimeQuantization || w != 2000 {
					s.violate(s.name, "pod-1: block time %vms, W %vms, after %v voted headers of one source; the chain's is %vms", bt, w, most, want)
					break
				}
				if most >= firstAt+c9WatchHeaders {
					s.logf("pod-1: block time %vms after %v headers", bt, most)
					break
				}
			}
			s.waitServing(0)
			s.endExcuse(ex, time.Now().Add(liveBound))
			s.blocks(25)
		}}
}

// K7 (on, off) and P9: head+1 polling, no subscriptions. A poller asks
// eth_getBlockByNumber(next) until it answers, pod by pod. Each request
// reaches upstreams at most refusalDispatches times. With
// shortCircuitFutureBlocks on, a block far above the ceiling is null with
// no dispatch (§6.4 refusal rule, #1238). Each null poll in the band is a
// band probe: one retried_missing (row K7).
func k7(on bool) spec {
	name := "K7-off"
	if on {
		name = "K7-on"
	}
	return spec{name: name, bound: healthyDelivery, evm: map[string]interface{}{"shortCircuitFutureBlocks": on},
		clients: func(*scenario) {},
		body:    func(s *scenario) { pollNext(s, on) }}
}

// p9 is K7-off on its own chain: spec §19 names it as K7.
func p9() spec {
	sp := k7(false)
	sp.name = "P9"
	return sp
}

// pollNext's polls ask full transactions, so the vendors count their
// dispatches apart from the scenario's clients, which read each block they
// receive by number without them at the moment the poll answers it.
func pollNext(s *scenario, farFuture bool) {
	s.blocks(10)
	var bn hexutil.Uint64
	if err := s.call(0, &bn, "eth_blockNumber"); err != nil {
		s.t.Fatalf("eth_blockNumber: %v", err)
	}
	next, polls := uint64(bn)+1, 0
	// nulls counts each pod's null answers: each is one refusal, retried
	// in the band or future above the ceiling (§6.4, §17).
	nulls := make([]float64, len(s.h.pods))
	for end := time.Now().Add(40 * time.Second); time.Now().Before(end); polls++ {
		pod := int(next % uint64(len(s.h.pods)))
		from := time.Now()
		got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(next), true)
		if d := s.dispatches(next, true, from, time.Now()); d > refusalDispatches {
			s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) reached upstreams %d times (at most %d); answered %s", pod+1, next, d, refusalDispatches, got)
		}
		if got == "null" {
			nulls[pod]++
			time.Sleep(s.chain.BlockTime() / 4)
			continue
		}
		if want := s.chain.CanonAt(next); want == nil || got != want.Hash.Hex() {
			s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) answered %s", pod+1, next, got)
		}
		next++
	}
	s.logf("%d polls reached block %d", polls, next-1)
	if farFuture {
		for i := range 10 {
			pod := i % len(s.h.pods)
			n := s.chain.Tip().Number + 1000 + uint64(i)
			from := time.Now()
			got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(n), true)
			if got == "null" {
				nulls[pod]++
			}
			if d := s.dispatches(n, true, from, time.Now()); got != "null" || d > 0 {
				s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d), above the ceiling, answered %s after %d dispatches", pod+1, n, got, d)
			}
		}
	}
	oweBandProbes(s, nulls)
}

// oweBandProbes owes each pod one retried_missing per band probe: whether a
// null poll was in the band or above the ceiling is the pod's verdict at
// that instant, which only its counters show, so the band probes are its
// null answers less those it counted future. A null the pod counted as no
// refusal at all (a block at or below Latest) leaves retried_missing short
// of what is owed, and a retried_missing from anything but these polls
// exceeds it.
func oweBandProbes(s *scenario, nulls []float64) {
	for pod, n := range nulls {
		ss, err := s.h.pods[pod].scrape()
		if err != nil {
			s.violate("harness", "pod-%d: scrape: %v", pod+1, err)
			continue
		}
		future := 0.0
		for _, rule := range []string{"future", "future_after_refresh", "future_after_wait"} {
			v, _ := sumOf(ss, "erpc_head_refusal_total", map[string]string{"project": s.project, "network": s.network(), "rule": rule})
			future += v
		}
		s.logf("pod-%d: %v null polls, %v counted future, %v band probes", pod+1, n, future, n-future)
		s.owe(pod, "retried_missing", n-future)
	}
}

// P10: a flashblocks group (vendor-a) at the tip and a normal group
// (vendor-b) p10NormalLag behind, each a use-upstream lane. A newHeads
// client on a lane gets blocks its lane holds, and fetches each over HTTP
// on the same lane on every pod (§6.4 lanes, §6.5 a use-upstream
// connection gates on its lane); no retry misses (retried_missing 0). The
// normal lane trails by more than reorgWindow blocks, yet its taps never
// restart: a lane tap's block is ready when its lane's Latest reached it
// (§10.2).
func p10() spec {
	lag := chainsim.Faults{Lag: p10NormalLag}
	return spec{name: "P10", bound: healthyDelivery + p10NormalLag*blockTime,
		evm: map[string]interface{}{"chainTracker": map[string]interface{}{"reorgWindow": p10ReorgWindow, "maxBackfill": maxBackfill.String()}},
		vendors: []vendorSpec{
			{"flashblocks-ws", "vendor-a", true}, {"flashblocks-http", "vendor-a", false},
			{"normal-ws", "vendor-b", true}, {"normal-http", "vendor-b", false},
		},
		faults: map[string]chainsim.Faults{"normal-ws": lag, "normal-http": lag},
		clients: func(s *scenario) {
			s.baseClients()
			for pod := range s.h.pods {
				s.addHeadsOn(fmt.Sprintf("p%d-heads-flashblocks", pod+1), pod, "p10-flashblocks-*", false)
				s.addHeadsOn(fmt.Sprintf("p%d-heads-normal", pod+1), pod, "p10-normal-*", false)
			}
		},
		body: func(s *scenario) {
			s.blocks(150)
			for pod := range s.h.pods {
				s.requireZeroIfExported(pod, "erpc_head_refusal_total", map[string]string{"rule": "retried_missing"})
			}
		}}
}
