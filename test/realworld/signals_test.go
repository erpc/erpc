package realworld

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Bounds of the rollout scenarios (spec §17 signals, §18 rollout; §19 X7,
// X8).
const (
	// x7Regress: no client sees latest regress across instances by more
	// than 1 bt, one block (§19 X7).
	x7Regress = 1
	// x7CounterSlack: the pods' counters agree within 1 bt, as the row
	// bounds latest, plus the block a back-to-back scrape pair can
	// straddle (harness).
	x7CounterSlack = 2
	// x7Watch, x7Readers, x7Every, x7Agree: how long X7 reads, with how
	// many clients, each this often, comparing counters this often
	// (harness).
	x7Watch   = 60 * time.Second
	x7Readers = 4
	x7Every   = blockTime / 2
	x7Agree   = 2 * time.Second
	// x8Bogus: the canary's bogus head above the tip (§19 X8); x8Probe: a
	// height above Latest and far below it.
	x8Bogus = 1000
	x8Probe = 50
	// x8ReportWait is harness, not spec: a published counter reaches
	// every pod.
	x8ReportWait = 10 * time.Second
)

// X7: pod-1 runs the old image (origin/main), pod-2 the branch's, on one
// redis (§18 rollout stage 2: a canary beside control). Clients that read
// eth_blockNumber from both, as behind a load balancer, never see it
// regress by more than x7Regress; both pods' per-upstream latest counters
// agree; the new pod counts no invariant violation (§17).
func x7() spec {
	return spec{name: "X7", oldPods: []int{0}, bound: healthyDelivery, clients: func(*scenario) {}, body: func(s *scenario) {
		s.blocks(25)
		ctx, cancel := context.WithCancel(s.ctx)
		var wg sync.WaitGroup
		for i := range x7Readers {
			wg.Add(1)
			go func() { defer wg.Done(); s.readAcross(ctx, i) }()
		}
		compared := map[string]int{}
		for end := time.Now().Add(x7Watch); time.Now().Before(end); time.Sleep(x7Agree) {
			s.requireCountersAgree(compared)
		}
		for _, u := range s.net.ups {
			if compared[u.id] == 0 {
				s.violate("harness", "%s's latest block was never exported by every pod: its counters were never compared", u.id)
			}
		}
		cancel()
		wg.Wait()
		for pod, p := range s.h.pods {
			if !p.old {
				s.requireNoInvariantViolation(pod, 0)
			}
		}
		s.blocks(25)
	}}
}

// image names the image pod runs.
func (s *scenario) image(pod int) string {
	if s.h.pods[pod].old {
		return "old"
	}
	return "new"
}

// readAcross reads eth_blockNumber from every pod in turn until ctx ends,
// and fails once when an answer is more than x7Regress below the highest
// this reader saw.
func (s *scenario) readAcross(ctx context.Context, reader int) {
	var high uint64
	var highPod int
	name := fmt.Sprintf("reader-%d", reader+1)
	for i := reader; ctx.Err() == nil; i++ {
		pod := i % len(s.h.pods)
		var bn hexutil.Uint64
		if err := s.call(pod, &bn, "eth_blockNumber"); err != nil {
			if ctx.Err() == nil {
				s.violate(name, "pod-%d (%s image): eth_blockNumber: %v", pod+1, s.image(pod), err)
			}
			return
		}
		if uint64(bn)+x7Regress < high {
			s.violate(name, "pod-%d (%s image): eth_blockNumber %d, %d below the %d pod-%d (%s image) answered before", pod+1, s.image(pod), bn, high-uint64(bn), high, highPod+1, s.image(highPod))
			return
		}
		if uint64(bn) > high {
			high, highPod = uint64(bn), pod
		}
		sleepCtx(ctx, x7Every)
	}
}

// requireCountersAgree: the pods' latest blocks of each upstream every pod
// exports lie within x7CounterSlack; compared counts the upstreams
// compared. An image exports an upstream only once it polled it, so the
// old image may not export one yet.
func (s *scenario) requireCountersAgree(compared map[string]int) {
	scrapes := make([][]series, len(s.h.pods))
	for pod, p := range s.h.pods {
		ss, err := p.scrape()
		if err != nil {
			s.violate(s.name, "pod-%d: scrape: %v", pod+1, err)
			return
		}
		scrapes[pod] = ss
	}
next:
	for _, u := range s.net.ups {
		var lo, hi float64
		for pod, ss := range scrapes {
			v, ok := sumOf(ss, "erpc_upstream_latest_block_number", map[string]string{"project": s.project, "network": s.network(), "upstream": u.id})
			if !ok {
				continue next
			}
			if pod == 0 || v < lo {
				lo = v
			}
			if pod == 0 || v > hi {
				hi = v
			}
		}
		compared[u.id]++
		if hi-lo > x7CounterSlack {
			s.violate(s.name, "the pods' latest blocks of %s lie %v apart (%v…%v), above %d", u.id, hi-lo, lo, hi, x7CounterSlack)
		}
	}
}

// requireNoInvariantViolation: pod exports
// erpc_chain_tracker_invariant_violations_total for the network, with
// every invariant but final_hash_changed at 0, and
// erpc_head_refusal_total{rule=retried_missing} is bandMisses: the
// scenario's own probes of blocks no vendor has inside the refusal band,
// each a retried miss by definition (§6.4, §17, §19).
func (s *scenario) requireNoInvariantViolation(pod int, bandMisses float64) {
	ss, err := s.h.pods[pod].scrape()
	if err != nil {
		s.violate(s.name, "pod-%d: scrape: %v", pod+1, err)
		return
	}
	exported := false
	for _, x := range ss {
		if x.name != "erpc_chain_tracker_invariant_violations_total" || x.labels["project"] != s.project || x.labels["network"] != s.network() {
			continue
		}
		exported = true
		if x.labels["invariant"] != "final_hash_changed" && x.value > 0 {
			s.violate(s.name, "pod-%d: erpc_chain_tracker_invariant_violations_total{invariant=%s} = %v, not 0", pod+1, x.labels["invariant"], x.value)
		}
	}
	if !exported {
		s.violate(s.name, "pod-%d does not export erpc_chain_tracker_invariant_violations_total for %s (§17)", pod+1, s.network())
	}
	if v, _ := sumOf(ss, "erpc_head_refusal_total", map[string]string{"project": s.project, "network": s.network(), "rule": "retried_missing"}); v != bandMisses {
		s.violate(s.name, "pod-%d: erpc_head_refusal_total{rule=retried_missing} = %v, not %v (the scenario's own band probes)", pod+1, v, bandMisses)
	}
}

// X8: a canary instance writes http-b's latest counter at the tip +
// x8Bogus (§18 rollout stage 4: the canary writes the counters control
// reads). On both control pods Latest and the other upstreams' lag stay as
// they were, and only the ceiling rises: a block above Latest is
// dispatched, not short-circuited (§6.4). No invariant is violated (§17).
func x8() spec {
	return spec{name: "X8", bound: healthyDelivery, evm: map[string]interface{}{"shortCircuitFutureBlocks": true}, body: func(s *scenario) {
		s.blocks(25)
		u := s.upSpec("http-b")
		key := upstreamCounterKey(u, "latestBlock")
		if _, err := s.h.redis.counter(key); err != nil {
			s.violate("harness", "redis holds no counter %s for %s: %v", key, u.id, err)
		}
		stop := s.watchLatest(func(pod int, bn, tip uint64) {
			s.violate(s.name, "pod-%d: eth_blockNumber %d against the tip %d after a canary wrote %s's counter +%d", pod+1, bn, tip, u.id, x8Bogus)
		})
		bogus := int64(s.chain.Tip().Number) + x8Bogus
		if err := s.h.redis.writeCounter(key, bogus, time.Now(), "canary"); err != nil {
			s.t.Fatal(err)
		}
		s.logf("a canary wrote %s's latest counter %d, %d above the tip", u.id, bogus, x8Bogus)
		for pod := range s.h.pods {
			s.awaitUpstreamHead(pod, u.id, float64(bogus), x8ReportWait)
		}
		s.blocks(25)
		for pod := range s.h.pods {
			for _, key := range []string{"ws-a", "ws-b", "http-a"} {
				v, found := s.metric(pod, "erpc_upstream_block_head_lag", map[string]string{"upstream": s.upstream(key)})
				if !found || v > float64(headSlackBlocks) {
					s.violate(s.name, "pod-%d: erpc_upstream_block_head_lag of %s is %v (exported %v) after the canary's bogus head", pod+1, s.upstream(key), v, found)
				}
			}
			n := s.chain.Tip().Number + x8Probe
			from := time.Now()
			got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(n), false)
			switch d := s.dispatches(n, from, time.Now()); {
			case d == 0:
				s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) answered %s with no dispatch, below the ceiling the canary's head raised", pod+1, n, got)
			case d > refusalDispatches:
				s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d), in the band, reached upstreams %d times (at most %d)", pod+1, n, d, refusalDispatches)
			}
			if got != "null" {
				s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d), a block no vendor has, answered %s", pod+1, n, got)
			}
		}
		stop()
		for pod := range s.h.pods {
			s.requireNoInvariantViolation(pod, 1)
		}
		s.blocks(25)
	}}
}
