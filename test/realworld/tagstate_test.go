package realworld

import (
	"math/big"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Bounds of the tag-state gate (spec §6.11; §19 P11).
const (
	// p11Stale: the stale vendor executes latest this far below its head:
	// past W, the gate's tolerance, and inside the 60s after which an
	// all-stale request is refused (§6.11).
	p11Stale = 50
	// p11Evidence: the gate holds stale evidence on an upstream within a
	// probe, at most every 2s per upstream, plus W for the tolerance
	// (§6.11), plus one scrape.
	p11Evidence = 2*time.Second + liveBound + scrapeEvery
	// p11Reads: reads per P11 phase (harness).
	p11Reads = 20
	// p11Gap: the all-stale depths lie this far apart, so an answer of the
	// highest tag-state head tells from the next one's (harness).
	p11Gap = 10
)

// p11Depths: in P11's all-stale phase every vendor executes latest this far
// below its head; ws-a holds the highest tag-state head, http-a, ranked
// first, the lowest.
var p11Depths = map[string]uint64{"ws-a": 20, "http-b": 20 + p11Gap, "ws-b": 20 + 2*p11Gap, "http-a": 20 + 3*p11Gap}

// P11: eth_call keeps its latest tag to upstreams (translateLatestTag:
// false), and the selection policy ranks http-a first. http-a executes
// latest p11Stale blocks below its head while its header head is current.
// The gate skips it for tag requests: every eth_call(latest) to Multicall3
// getBlockNumber() executes within W of Latest, and every pod shows http-a
// stale. Then every vendor runs stale, by p11Depths: each call is routed
// to ws-a, the highest tag-state head, and counted; none fails, -32603
// least of all (§6.11 Skip, All stale).
func p11() spec {
	return spec{name: "P11", bound: healthyDelivery,
		network: map[string]interface{}{
			"selectionPolicy": rankFirst("http-a"),
			"methods": map[string]interface{}{"preserveDefaultMethods": true, "definitions": map[string]interface{}{
				"eth_call": map[string]interface{}{"reqRefs": [][]interface{}{{1}}, "translateLatestTag": false},
			}},
		},
		body: func(s *scenario) {
			s.blocks(25)
			up := s.upstream("http-a")
			s.vendors["http-a"].Update(func(f *chainsim.Faults) { f.StaleState = p11Stale })
			s.logf("%s executes latest %d blocks below its head", up, p11Stale)
			for pod := range s.h.pods {
				if v, ok := s.waitMetric(pod, "erpc_upstream_tag_state_stale", map[string]string{"upstream": up}, p11Evidence, func(v float64) bool { return v == 1 }); !ok {
					s.violate(s.name, "pod-%d: erpc_upstream_tag_state_stale of %s is %v, not 1, %s after its latest state fell %d behind", pod+1, up, v, p11Evidence, p11Stale)
				}
			}
			for i := range p11Reads {
				pod := i % len(s.h.pods)
				if bn, at, err := s.latestCall(pod); err != nil {
					s.violate(s.name, "pod-%d: eth_call(latest): %v", pod+1, err)
				} else if at+headSlackBlocks < bn {
					s.violate(s.name, "pod-%d: eth_call(latest) executed at %d, %d below Latest %d: the stale %s was not skipped", pod+1, at, bn-at, bn, up)
				}
				s.blocks(2)
			}
			routed := make([]float64, len(s.h.pods))
			for pod := range s.h.pods {
				routed[pod], _ = s.metric(pod, "erpc_upstream_tag_state_all_stale_total", map[string]string{"outcome": "routed"})
			}
			for key, d := range p11Depths {
				s.vendors[key].Update(func(f *chainsim.Faults) { f.StaleState = d })
			}
			s.logf("every vendor executes latest stale: %v", p11Depths)
			time.Sleep(p11Evidence)
			best := p11Depths["ws-a"]
			for i := range p11Reads {
				pod := i % len(s.h.pods)
				if bn, at, err := s.latestCall(pod); err != nil {
					s.violate(s.name, "pod-%d: eth_call(latest) with every upstream stale: %v", pod+1, err)
				} else if at+best+p11Gap/2 < bn {
					s.violate(s.name, "pod-%d: eth_call(latest) with every upstream stale executed at %d, %d below Latest %d: not routed to the highest tag-state head, %d below", pod+1, at, bn-at, bn, best)
				}
				s.blocks(2)
			}
			for pod := range s.h.pods {
				if v, _ := s.metric(pod, "erpc_upstream_tag_state_all_stale_total", map[string]string{"outcome": "routed"}); v <= routed[pod] {
					s.violate(s.name, "pod-%d: erpc_upstream_tag_state_all_stale_total{outcome=routed} stayed %v while every upstream was stale", pod+1, v)
				}
				if v, _ := s.metric(pod, "erpc_upstream_tag_state_all_stale_total", map[string]string{"outcome": "refused"}); v > 0 {
					s.violate(s.name, "pod-%d: erpc_upstream_tag_state_all_stale_total{outcome=refused} is %v with the highest tag-state head %d blocks behind, inside 60s", pod+1, v, best)
				}
			}
			for key := range p11Depths {
				s.vendors[key].Update(func(f *chainsim.Faults) { f.StaleState = 0 })
			}
			s.blocks(25)
		}}
}

// latestCall reads eth_blockNumber, then eth_call(latest) to Multicall3
// getBlockNumber() on pod: Latest, and the block the call executed at.
func (s *scenario) latestCall(pod int) (latest, at uint64, err error) {
	var bn hexutil.Uint64
	if err := s.call(pod, &bn, "eth_blockNumber"); err != nil {
		return 0, 0, err
	}
	var res hexutil.Bytes
	call := map[string]interface{}{"to": chainsim.Multicall3.Hex(), "data": chainsim.GetBlockNumberSig}
	if err := s.call(pod, &res, "eth_call", call, "latest"); err != nil {
		return uint64(bn), 0, err
	}
	return uint64(bn), new(big.Int).SetBytes(res).Uint64(), nil
}
