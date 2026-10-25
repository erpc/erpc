package realworld

import (
	"encoding/json"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Bounds of the network-fault scenarios (§19 N1, N2, N7).
const (
	// n1Latency: every pod hears ws-a this late (§19 N1); n1Blocks: for
	// this many blocks. n1P99: delivery p99 within 2 bt. n1Behind: Latest
	// from the faster voters stays within a block of the tip, and n1Below
	// is the share of samples allowed below it, as p99 is (harness).
	n1Latency = 300 * time.Millisecond
	n1Blocks  = 100
	n1P99     = 2 * blockTime
	n1Behind  = 1
	n1Below   = 0.01

	// n2Jitter: each chunk on every link waits 0 to 2 bt more (§19 N2),
	// for n2Blocks.
	n2Jitter = 2 * blockTime
	n2Blocks = 100

	// n7Txs: transactions per block, so a block's receipts body is at
	// least n7ReceiptsBytes, 2 MB (§19 N7); n7Blocks: blocks minted that
	// big. n7Bandwidth: vendor-a's links carry 1 Mbit/s each way.
	// n7LogsBound: a receipts attempt that times out inside the fetch's
	// 10s, then another vendor or the eth_getLogs fallback (§8, §11).
	n7Txs           = 1700
	n7ReceiptsBytes = 2 << 20
	n7Blocks        = 15
	n7Bandwidth     = 1_000_000 / 8
	n7LogsBound     = fetchTimeout + logsFallbackAfter + healthyDelivery
)

// N1: every pod hears ws-a n1Latency late, for n1Blocks. Each block
// reaches its clients within n1P99 of its first header at p99, and Latest
// comes from the faster voters: eth_blockNumber stays within n1Behind
// blocks of the tip on all but n1Below of the samples (§19 N1).
func n1() spec {
	return spec{name: "N1", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		px := s.proxies["ws-a"]
		for _, p := range s.h.pods {
			px.set(p, linkFaults{Latency: n1Latency})
		}
		from := time.Now()
		s.logf("every pod hears %s %s late", s.upstream("ws-a"), n1Latency)
		samples, below := 0, 0
		for end := from.Add(n1Blocks * blockTime); time.Now().Before(end); time.Sleep(blockTime / 2) {
			for pod := range s.h.pods {
				tip := s.chain.Tip().Number
				var bn hexutil.Uint64
				if err := s.call(pod, &bn, "eth_blockNumber"); err != nil {
					s.violate(s.name, "pod-%d: eth_blockNumber: %v", pod+1, err)
					continue
				}
				samples++
				if uint64(bn)+n1Behind < tip {
					below++
					s.tr.logf("pod-%d: eth_blockNumber %d, tip %d", pod+1, bn, tip)
				}
			}
		}
		for _, p := range s.h.pods {
			px.set(p, linkFaults{})
		}
		to := time.Now()
		if float64(below) > n1Below*float64(samples) {
			s.violate(s.name, "eth_blockNumber more than %d below the tip on %d of %d samples (at most %.0f%%) with only %s slow", n1Behind, below, samples, 100*n1Below, s.upstream("ws-a"))
		}
		lat := s.headerLatencies(from, to)
		if len(lat) == 0 {
			s.violate("harness", "no header delivered between %s and %s", from.Format("15:04:05"), to.Format("15:04:05"))
		} else if p99 := lat[len(lat)*99/100]; p99 > n1P99 {
			s.violate(s.name, "header delivery p99 %s over %d deliveries, above 2 bt (%s), with only %s slow", p99.Round(time.Millisecond), len(lat), n1P99, s.upstream("ws-a"))
		}
		s.blocks(25)
		s.requireClean()
	}}
}

// headerLatencies are the sorted latencies, from the block's first header
// on the pod, of every live header delivery of a block minted in
// [from, to].
func (s *scenario) headerLatencies(from, to time.Time) []time.Duration {
	var cs []*clientBase
	for _, c := range s.heads {
		cs = append(cs, &c.clientBase)
	}
	for _, g := range s.grpcs {
		cs = append(cs, &g.clientBase)
	}
	var out []time.Duration
	for _, c := range cs {
		c.mu.Lock()
		for _, d := range c.deliveries {
			if d.minted.Before(from) || d.minted.After(to) || d.minted.Before(d.connStart) {
				continue
			}
			out = append(out, d.at.Sub(s.waitFrom(d.pod, d.hash, d.minted)))
		}
		c.mu.Unlock()
	}
	slices.Sort(out)
	return out
}

// N2: every link jitters each chunk 0 to n2Jitter, for n2Blocks: pushes
// of one block from different vendors reach a pod out of order. Every
// newHeads client still gets its headers in order, parent-linked, with no
// duplicate, and every gRPC stream stays ascending and gap-free (the
// clients' oracles, §10.2; §19 N2).
func n2() spec {
	return spec{name: "N2", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		for _, px := range s.proxies {
			for _, p := range s.h.pods {
				px.set(p, linkFaults{Jitter: n2Jitter})
			}
		}
		from := time.Now()
		s.logf("every link jitters up to %s", n2Jitter)
		s.blocks(n2Blocks)
		for _, px := range s.proxies {
			for _, p := range s.h.pods {
				px.set(p, linkFaults{})
			}
		}
		to := time.Now()
		reordered := 0
		for pod := range s.h.pods {
			blocks := s.chain.MintedBetween(from, to)
			for i := 1; i < len(blocks); i++ {
				prev, okPrev := s.h.headerLastAt(pod, blocks[i-1].Hash)
				next, okNext := s.h.headerAt(pod, blocks[i].Hash)
				if okPrev && okNext && next.Before(prev) {
					reordered++
				}
			}
		}
		s.logf("%d blocks whose first header reached a pod before the last header of their parent", reordered)
		if reordered == 0 {
			s.violate("harness", "no block's header reached a pod before every vendor's header of its parent: the jitter reordered nothing")
		}
		s.blocks(25)
		s.requireClean()
	}}
}

// N7: for n7Blocks every non-empty block holds n7Txs transactions, a
// receipts body of at least n7ReceiptsBytes, while every pod's links to
// vendor-a (http-a ranked first) carry n7Bandwidth. Receipts attempts on
// vendor-a time out and the fetch goes to vendor-b or the eth_getLogs
// fallback: every client's logs equal the canonical logs within
// n7LogsBound, and headers keep within W of their first header (§19 N7;
// §8 one sweep, timeout 10s; §11).
func n7() spec {
	return spec{name: "N7", bound: n7LogsBound,
		network: map[string]interface{}{"selectionPolicy": rankFirst("http-a")},
		body: func(s *scenario) {
			s.blocks(25)
			for _, key := range vendorA {
				for _, p := range s.h.pods {
					s.proxies[key].set(p, linkFaults{Bandwidth: n7Bandwidth})
				}
			}
			a0 := s.receiptsCalls(vendorA)
			s.chain.BigBlocks(n7Txs)
			from := time.Now()
			s.logf("blocks of %d transactions; vendor-a's links at %d B/s", n7Txs, n7Bandwidth)
			s.blocks(n7Blocks)
			s.chain.BigBlocks(0)
			to := time.Now()
			s.blocks(int(math.Ceil(float64(n7LogsBound) / float64(blockTime))))
			for _, key := range vendorA {
				for _, p := range s.h.pods {
					s.proxies[key].set(p, linkFaults{})
				}
			}
			big := 0
			for _, b := range s.chain.MintedBetween(from, to) {
				if b.TxCount() == 0 {
					continue
				}
				big++
				if raw, _ := json.Marshal(b.Receipts()); len(raw) < n7ReceiptsBytes {
					s.violate("harness", "block %d's receipts body is %d bytes, below %d", b.Number, len(raw), n7ReceiptsBytes)
				}
			}
			if big == 0 {
				s.violate("harness", "no big block minted")
			}
			if s.receiptsCalls(vendorA) == a0 {
				s.violate("harness", "no receipts call reached vendor-a over its throttled links: no timeout exercised")
			}
			s.logf("%d big blocks; vendor-a answered %d receipts and logs calls", big, s.receiptsCalls(vendorA)-a0)
			lat := s.headerLatencies(from, to)
			if n := len(lat); n > 0 && lat[n-1] > healthyDelivery {
				late := n - sort.Search(n, func(i int) bool { return lat[i] > healthyDelivery })
				s.violate(s.name, "%d of %d header deliveries of big blocks took longer than W after their first header (max %s): a receipts timeout held headers", late, n, lat[n-1].Round(time.Millisecond))
			}
			s.blocks(25)
			s.requireClean()
		}}
}
