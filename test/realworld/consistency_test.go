package realworld

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Bounds of the HTTP-consistency scenarios (§7; §19 P2–P4, P8).
const (
	// p8Rounds: by-number reads each followed by a reorg that replaces the
	// block read (harness); p8Notice: a reorg notice reaches a header
	// client within the healthy bound.
	p8Rounds = 6
	p8Notice = healthyDelivery + reconnectSlack
)

// reorgLoop is C1's chain: 1–3 deep reorgs every 15 blocks, 12 times;
// each runs between with, if set, called before every reorg.
func reorgLoop(s *scenario, between func(i int)) {
	s.blocks(25)
	for i := range 12 {
		if between != nil {
			between(i)
		}
		s.blocks(15)
		s.reorg(1 + rand.IntN(3))
	}
	s.blocks(25)
}

// told reports whether a header client received, after at, a header at or
// below n: a reorg notice for n.
func (c *headsClient) told(n uint64, at time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.replacements {
		if r.number <= n && r.at.After(at) {
			return true
		}
	}
	return false
}

// requireDelivered re-reads, on every pod, every block a header client
// received and was not later told a reorg replaced: by number (byHash
// false) it is the delivered hash, by hash it is not null (§19 P2, P3).
func (s *scenario) requireDelivered(byHash bool) {
	read := map[string]string{}
	checked := 0
	for _, c := range s.heads {
		c.mu.Lock()
		ds := append([]delivery(nil), c.deliveries...)
		c.mu.Unlock()
		for _, d := range ds {
			if c.told(d.number, d.at) {
				continue
			}
			for pod := range s.h.pods {
				method, arg := "eth_getBlockByNumber", hexutil.EncodeUint64(d.number)
				if byHash {
					method, arg = "eth_getBlockByHash", d.hash.Hex()
				}
				key := fmt.Sprintf("%d/%s", pod, arg)
				got, ok := read[key]
				if !ok {
					got = s.blockAnswer(pod, "", method, arg, false)
					read[key] = got
					checked++
				}
				if got != d.hash.Hex() && !c.told(d.number, d.at) {
					s.violate(c.name, "pod-%d: %s(%s) answered %s after the run; the client received %d %s and no reorg notice", pod+1, method, arg, got, d.number, short(d.hash))
				}
			}
		}
	}
	s.logf("%d reads of delivered blocks", checked)
	if checked == 0 {
		s.violate("harness", "no delivered block to read")
	}
}

// P2: C1's reorgs, cache on. After the run, eth_getBlockByNumber(N) on
// every pod is the hash each header client received for N, unless the
// client was told of a reorg at or below N (§19 P2; the oracle checks the
// same at each delivery).
func p2() spec {
	return spec{name: "P2", cache: true, bound: healthyDelivery, body: func(s *scenario) {
		reorgLoop(s, nil)
		s.requireDelivered(false)
		s.requireClean()
	}}
}

// P3: C1's reorgs, cache on. After the run, eth_getBlockByHash of every
// delivered block, of no reorg notice since, is not null on every pod
// (§19 P3).
func p3() spec {
	return spec{name: "P3", cache: true, bound: healthyDelivery, body: func(s *scenario) {
		reorgLoop(s, nil)
		s.requireDelivered(true)
		s.requireClean()
	}}
}

// P4: C1's reorgs, and U9's receipts faults on vendor-a in turn before
// each, integrity on. After the run, eth_getLogs(N, N) with each logs
// client's filter, on every pod, is the client's live logs of block N
// (§19 P4).
func p4() spec {
	faults := []func(*chainsim.Faults, bool){
		func(f *chainsim.Faults, on bool) { f.TruncReceipts = on },
		func(f *chainsim.Faults, on bool) { f.ForgedLog = on },
		func(f *chainsim.Faults, on bool) { f.WrongBloom = on },
	}
	return spec{name: "P4", integrity: true, bound: healthyDelivery, body: func(s *scenario) {
		s.slowVendorB()
		a0 := s.receiptsCalls(vendorA)
		reorgLoop(s, func(i int) { s.fault(vendorA, faults[i%len(faults)], 10) })
		if s.receiptsCalls(vendorA) == a0 {
			s.violate("harness", "no receipts call reached vendor-a during its faults")
		}
		s.requireLogsServed()
		s.requireClean()
	}}
}

// requireLogsServed: for each logs client and each block N it holds live
// logs of, eth_getLogs(N, N) with its filter on every pod returns exactly
// those logs (§19 P4).
func (s *scenario) requireLogsServed() {
	checked := 0
	for _, c := range s.logs {
		c.mu.Lock()
		byNumber := map[uint64]map[string]bool{}
		for k, l := range c.live {
			if byNumber[l.number] == nil {
				byNumber[l.number] = map[string]bool{}
			}
			byNumber[l.number][fmt.Sprintf("%s/%d", k.BlockHash.Hex(), k.LogIndex)] = true
		}
		c.mu.Unlock()
		for n, want := range byNumber {
			for pod := range s.h.pods {
				q := chainsim.Filters[c.filter].Params()
				q["fromBlock"], q["toBlock"] = hexutil.EncodeUint64(n), hexutil.EncodeUint64(n)
				var logs []logJSON
				if err := s.call(pod, &logs, "eth_getLogs", q); err != nil {
					s.violate(c.name, "pod-%d: eth_getLogs(%d, %d): %v", pod+1, n, n, err)
					continue
				}
				checked++
				got := map[string]bool{}
				for _, l := range logs {
					got[fmt.Sprintf("%s/%d", l.BlockHash.Hex(), l.LogIndex)] = true
				}
				var diff []string
				for k := range want {
					if !got[k] {
						diff = append(diff, "missing "+k[:10]+k[strings.LastIndex(k, "/"):])
					}
				}
				for k := range got {
					if !want[k] {
						diff = append(diff, "extra "+k[:10]+k[strings.LastIndex(k, "/"):])
					}
				}
				if len(diff) > 0 {
					s.violate(c.name, "pod-%d: eth_getLogs(%d, %d) differs from the delivered logs: %s", pod+1, n, n, strings.Join(diff[:min(len(diff), 5)], ", "))
				}
			}
		}
	}
	s.logf("%d eth_getLogs reads of delivered blocks", checked)
	if checked == 0 {
		s.violate("harness", "no delivered log to read")
	}
}

// P8: cache on. Each round reads the head block by number on every pod,
// so the answer is cached, then a reorg replaces that block. Once a header
// client on a pod is told of the reorg, the same read on that pod answers
// the new canonical block, never the cached orphan (§19 P8; §7).
func p8() spec {
	return spec{name: "P8", cache: true, bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		for range p8Rounds {
			s.blocks(10)
			var bn hexutil.Uint64
			if err := s.call(0, &bn, "eth_blockNumber"); err != nil {
				s.violate(s.name, "pod-1: eth_blockNumber: %v", err)
				continue
			}
			n := uint64(bn)
			old := s.chain.CanonAt(n)
			for pod := range s.h.pods {
				if got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(n), false); got != old.Hash.Hex() {
					s.logf("pod-%d: eth_getBlockByNumber(%d) answered %s before the reorg, not %s", pod+1, n, got, short(old.Hash))
				}
			}
			r := s.reorg(int(s.chain.Tip().Number-n) + 1)
			canon := s.chain.CanonAt(n)
			for pod := range s.h.pods {
				var notice bool
				for deadline := time.Now().Add(p8Notice); !notice && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
					for _, c := range s.heads {
						notice = notice || c.currentPod() == pod && c.told(n, r.at)
					}
				}
				if !notice {
					s.violate(s.name, "pod-%d: no header client told of the reorg at %d within %s", pod+1, n, p8Notice)
					continue
				}
				if got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(n), false); got != canon.Hash.Hex() {
					s.violate(s.name, "pod-%d: eth_getBlockByNumber(%d) answered %s after the reorg notice, not the new %s (orphan %s)", pod+1, n, got, short(canon.Hash), short(old.Hash))
				}
			}
		}
		s.blocks(25)
		s.requireClean()
	}}
}
