package realworld

import (
	"encoding/json"
	"math/big"
	"sync"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Bounds of the shared-counter scenarios (spec §14; §19 X5, C5, N6).
const (
	// x5Skew: the skewed writer's clock runs this far ahead (§19 X5).
	x5Skew = 5 * time.Minute
	// flowWatch: how long a counter flow is watched (harness).
	flowWatch = 30 * time.Second
	// c5Depth: C5's rewind on every vendor (§19 C5). c5Follow: Latest,
	// eth_call(latest) and getBlockByNumber(latest) follow a rewound chain
	// within 2 bt (§14 rollback write). c5WatchBlocks: how long C5 checks
	// after that (harness).
	c5Depth       = 50
	c5Follow      = 2 * blockTime
	c5WatchBlocks = 25
	// n6Down: redis is down this long (§19 N6). n6After: how long N6
	// watches from redis's return (harness). n6Rejoin is harness, not
	// spec: the redis client's reconnect backoff, within which the pods
	// write counters again.
	n6Down   = 60 * time.Second
	n6After  = 60 * time.Second
	n6Rejoin = 30 * time.Second
)

// cut is one pod cut from one vendor key: that pod hears the vendor's head
// only through the other pod's shared counter.
type cut struct {
	pod int
	key string
}

// x5Cuts: pod-2 hears ws-a, and pod-1 ws-b, only from the other pod.
var x5Cuts = []cut{{1, "ws-a"}, {0, "ws-b"}}

// X5: a peer whose clock runs x5Skew ahead publishes ws-a's and ws-b's
// latest counters at their current heads. The honest pods' later, higher
// numbers carry older timestamps, and still win: a higher remote value is
// taken whatever its timestamp (§14, #1243). Each pod, cut from one wss
// vendor, keeps that vendor's latest block within W of the tip from the
// other pod's counter.
func x5() spec {
	return spec{name: "X5", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		s.cutLinks(x5Cuts, true)
		s.blocks(10)
		for _, c := range x5Cuts {
			key := upstreamCounterKey(s.upSpec(c.key), "latestBlock")
			st, err := s.h.redis.counter(key)
			if err != nil {
				s.violate("harness", "redis holds no counter %s for %s: %v", key, c.key, err)
				continue
			}
			v := max(int64(s.vendors[c.key].Head().Number), st.Value)
			if err := s.h.redis.writeCounter(key, v, time.Now().Add(x5Skew), "skewed-peer"); err != nil {
				s.t.Fatal(err)
			}
			s.logf("a peer %s ahead published %s's latest %d (was %d by %s)", x5Skew, c.key, v, st.Value, st.UpdatedBy)
		}
		s.requireFlow(x5Cuts, flowWatch, "after a peer published it with UpdatedAt +5 min")
		s.cutLinks(x5Cuts, false)
		s.blocks(25)
	}}
}

// cutLinks black-holes, or heals, each cut's link.
func (s *scenario) cutLinks(cs []cut, on bool) {
	for _, c := range cs {
		s.proxies[c.key].set(s.h.pods[c.pod], linkFaults{BlackHole: on})
	}
}

// requireFlow watches each cut pod's latest block of its cut vendor for
// watch: from W on, it stays within W of the tip (§14: a counter the
// other pod moved reaches this one).
func (s *scenario) requireFlow(cs []cut, watch time.Duration, why string) {
	start := time.Now()
	failed := map[int]bool{}
	for time.Since(start) < watch {
		time.Sleep(scrapeEvery)
		if time.Since(start) < liveBound {
			continue
		}
		for _, c := range cs {
			if failed[c.pod] {
				continue
			}
			tip := s.chain.Tip().Number
			v, ok := s.metric(c.pod, "erpc_upstream_latest_block_number", map[string]string{"upstream": s.upstream(c.key)})
			if !ok || uint64(v)+headSlackBlocks < tip {
				failed[c.pod] = true
				s.violate(s.name, "pod-%d: %s's latest block %v (exported %v), below the tip %d by more than %d, %s %s: the other pod's numbers stopped reaching it",
					c.pod+1, c.key, v, ok, tip, headSlackBlocks, time.Since(start).Round(time.Second), why)
			}
		}
	}
}

// C5: the head rewinds c5Depth blocks on every vendor; the chain goes on
// from there. From c5Follow after the rewind, every pod's
// eth_blockNumber, getBlockByNumber(latest) and eth_call(latest) succeed on
// the rewound chain (§14 rollback write). The rewind is deeper than
// reorgWindow, so newHeads restarts and ws logs close 1013, as in C3.
// Every pod's Latest falls below a block it sent once: one
// latest_regressed per pod (§17, row C5).
func c5() spec {
	return spec{name: "C5", bound: deepReorgDelivery, allow: []string{"ws:logs-end", "ws:jump"}, body: func(s *scenario) {
		s.blocks(50)
		r := s.rewind(c5Depth)
		for pod := range s.h.pods {
			s.owe(pod, "latest_regressed", 1)
		}
		time.Sleep(time.Until(r.at.Add(c5Follow)))
		for range c5WatchBlocks {
			for pod := range s.h.pods {
				s.requireLatestFollows(pod)
			}
			s.blocks(1)
		}
		// Logs taps close 1013 over logsCloseSpread; the logs oracle reads
		// their clients after the closes and reconnects, as C3 does.
		time.Sleep(time.Until(r.at.Add(max(deepReorgDelivery, logsCloseSpread) + reconnectSlack)))
		s.blocks(25)
	}}
}

// requireLatestFollows: pod's eth_blockNumber, getBlockByNumber(latest) and
// eth_call(latest) to Multicall3 getBlockNumber() succeed, at or below the
// chain's tip, on its canonical blocks.
func (s *scenario) requireLatestFollows(pod int) {
	var bn hexutil.Uint64
	if err := s.call(pod, &bn, "eth_blockNumber"); err != nil {
		s.violate(s.name, "pod-%d: eth_blockNumber: %v", pod+1, err)
	} else if tip := s.chain.Tip().Number; uint64(bn) > tip {
		s.violate(s.name, "pod-%d: eth_blockNumber %d above the rewound tip %d", pod+1, bn, tip)
	}
	var raw json.RawMessage
	var b struct {
		Number hexutil.Uint64 `json:"number"`
		Hash   hash           `json:"hash"`
	}
	switch err := s.call(pod, &raw, "eth_getBlockByNumber", "latest", false); {
	case err != nil:
		s.violate(s.name, "pod-%d: eth_getBlockByNumber(latest): %v", pod+1, err)
	case len(raw) == 0 || string(raw) == "null":
		s.violate(s.name, "pod-%d: eth_getBlockByNumber(latest) answered null", pod+1)
	case json.Unmarshal(raw, &b) != nil:
		s.violate(s.name, "pod-%d: eth_getBlockByNumber(latest) answered %s", pod+1, raw)
	default:
		if want := s.chain.CanonAt(uint64(b.Number)); want == nil || want.Hash != b.Hash {
			s.violate(s.name, "pod-%d: eth_getBlockByNumber(latest) answered %d %s, not a block of the rewound chain", pod+1, b.Number, short(b.Hash))
		}
	}
	var res hexutil.Bytes
	call := map[string]interface{}{"to": chainsim.Multicall3.Hex(), "data": chainsim.GetBlockNumberSig}
	if err := s.call(pod, &res, "eth_call", call, "latest"); err != nil {
		s.violate(s.name, "pod-%d: eth_call(latest): %v", pod+1, err)
	} else if at, tip := new(big.Int).SetBytes(res).Uint64(), s.chain.Tip().Number; at > tip {
		s.violate(s.name, "pod-%d: eth_call(latest) executed at %d, above the rewound tip %d", pod+1, at, tip)
	}
}

// N6: redis is down for n6Down, then back with no data. Every pod keeps
// serving the tip from its own polls throughout and after: eth_blockNumber
// answers, within W of the tip. Once redis is back the pods write their
// counters to it again, and a counter one pod moves reaches the other (no
// wedge).
func n6() spec {
	return spec{name: "N6", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		stop := s.watchServing()
		s.logf("redis down for %s", n6Down)
		if err := s.h.redis.down(n6Down); err != nil {
			s.t.Fatalf("redis restart: %v", err)
		}
		back := time.Now()
		s.logf("redis back, empty")
		key := upstreamCounterKey(s.upSpec("ws-a"), "latestBlock")
		for {
			st, err := s.h.redis.counter(key)
			if err == nil && uint64(st.Value)+headSlackBlocks >= s.chain.Tip().Number {
				s.logf("%s's counter back in redis at %d by %s, %s after redis returned", s.upstream("ws-a"), st.Value, st.UpdatedBy, time.Since(back).Round(100*time.Millisecond))
				break
			}
			if time.Since(back) > n6Rejoin {
				s.violate(s.name, "no pod wrote %s's counter to redis within %s of its return (last %d, %v)", s.upstream("ws-a"), n6Rejoin, st.Value, err)
				break
			}
			time.Sleep(scrapeEvery)
		}
		flow := []cut{{1, "ws-a"}}
		s.cutLinks(flow, true)
		s.requireFlow(flow, flowWatch, "after redis returned")
		s.cutLinks(flow, false)
		time.Sleep(time.Until(back.Add(n6After)))
		stop()
		s.blocks(25)
	}}
}

// watchServing samples eth_blockNumber on every pod each block until stop,
// and fails a pod once when a read fails or falls more than
// headSlackBlocks below the tip: a wedge or a frozen tip.
func (s *scenario) watchServing() (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		failed := map[int]bool{}
		for {
			select {
			case <-done:
				return
			case <-time.After(s.chain.BlockTime()):
			}
			for pod := range s.h.pods {
				if failed[pod] {
					continue
				}
				low := s.chain.Tip().Number
				var bn hexutil.Uint64
				err := s.call(pod, &bn, "eth_blockNumber")
				if err != nil || uint64(bn)+headSlackBlocks < low {
					failed[pod] = true
					s.violate(s.name, "pod-%d: eth_blockNumber %d (err %v) against the tip %d", pod+1, bn, err, low)
				}
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}
