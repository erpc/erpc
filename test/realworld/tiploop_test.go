package realworld

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/test/chainsim"
)

const (
	// p15BlockTime: a fast chain's 0.1s blocks.
	p15BlockTime = 100 * time.Millisecond
	// p15Calls and p15Blocks: each image's client rate, eth_call(latest)
	// and eth_getBlockByNumber(latest) per second.
	p15Calls  = 30
	p15Blocks = 2
	p15Run    = 90 * time.Second
	// p15FirstLag: the policy's first upstream serves this many blocks
	// below the tip, so its head stays below a Latest pinned from the others.
	p15FirstLag = 3
	// p15ShareSlack and p15P50Slack: how far the new image's far-node share
	// and client p50 may exceed the old image's.
	p15ShareSlack = 5.0
	p15P50Slack   = 20 * time.Millisecond
)

// p15Ups: the policy order, rpc-a first, a far self-hosted node second,
// then three near upstreams.
var p15Ups = []struct {
	key, vendor string
	slow        time.Duration
	lag         uint64
}{
	{"http-a", "vendor-a", 10 * time.Millisecond, p15FirstLag},
	{"http-n", "vendor-n", 170 * time.Millisecond, 0},
	{"http-b", "vendor-b", 5 * time.Millisecond, 0},
	{"http-c", "vendor-c", 10 * time.Millisecond, 0},
	{"http-d", "vendor-d", 20 * time.Millisecond, 0},
}

// P15: a 0.1s chain behind five https upstreams polled every 1s; the policy
// puts rpc-a first, 3 blocks behind the tip, and a node 170 ms away second.
// Pod 1 runs the old image, pod 2 the branch's; one client per image sends
// 30 eth_call(latest) and 2 eth_getBlockByNumber(latest) per second. A tag
// names no block (spec §6.1), so the new image's first eth_call attempts
// follow the policy as the old image's do, though its pinned Latest is
// above rpc-a's head.
func p15() spec {
	var vendors []vendorSpec
	faults := map[string]chainsim.Faults{}
	upEvm := map[string]map[string]interface{}{}
	var order []string
	for _, u := range p15Ups {
		vendors = append(vendors, vendorSpec{u.key, u.vendor, false})
		faults[u.key] = chainsim.Faults{Slow: u.slow, Lag: u.lag}
		upEvm[u.key] = map[string]interface{}{"statePollerInterval": "1s"}
		order = append(order, "'"+u.key+"'")
	}
	evalFunc := fmt.Sprintf(`(upstreams, ctx) => { const o = [%s]; const r = u => o.findIndex(k => u.id.endsWith('-' + k)); return upstreams.filter(u => true).sort((a, b) => r(a) - r(b)) }`, strings.Join(order, ", "))
	network := map[string]interface{}{
		"selectionPolicy": map[string]interface{}{"evalFunc": evalFunc},
		"failsafe":        []map[string]interface{}{{"matchMethod": "*", "retry": map[string]interface{}{"maxAttempts": 3, "delay": "0s"}}},
	}
	return spec{name: "P15", pods: 2, oldPods: []int{0}, memory: true, bound: healthyDelivery,
		sim: chainsim.Config{BlockTime: p15BlockTime}, vendors: vendors, faults: faults, upEvm: upEvm,
		network: network, clients: func(*scenario) {}, body: p15Body}
}

// p15Image is one image's tally over the run.
type p15Image struct {
	pod         int
	mu          sync.Mutex
	lat         []time.Duration
	errs        int
	first, last map[string]float64
}

func p15Body(s *scenario) {
	s.blocks(50)
	var imgs []*p15Image
	for _, p := range s.h.pods {
		imgs = append(imgs, &p15Image{pod: p.idx})
	}
	ctx, cancel := context.WithTimeout(s.ctx, p15Run)
	defer cancel()
	var wg sync.WaitGroup
	for _, im := range imgs {
		im.first = s.p15FirstAttempts(im.pod)
		wg.Add(1)
		go func() { defer wg.Done(); s.p15Client(ctx, im) }()
	}
	wg.Wait()
	var b strings.Builder
	share, p50 := map[string]float64{}, map[string]time.Duration{}
	for _, im := range imgs {
		im.last = s.p15FirstAttempts(im.pod)
		img := s.image(im.pod)
		share[img], p50[img] = s.p15Report(&b, im)
	}
	s.logf("%s", b.String())
	if share["new"] > share["old"]+p15ShareSlack {
		s.violate(s.name, "new image sent %.0f%% of first eth_call(latest) attempts to the far node, old %.0f%%: a pinned tag ordered upstreams by head over the policy", share["new"], share["old"])
	}
	if p50["new"] > p50["old"]+p15P50Slack {
		s.violate(s.name, "new image eth_call(latest) p50 %s, old %s", p50["new"], p50["old"])
	}
}

// p15FirstAttempts is pod's first eth_call attempts per upstream key.
func (s *scenario) p15FirstAttempts(pod int) map[string]float64 {
	out := map[string]float64{}
	ss, err := s.h.pods[pod].scrape()
	if err != nil {
		return out
	}
	for _, u := range p15Ups {
		out[u.key], _ = sumOf(ss, "erpc_upstream_request_total", map[string]string{"project": s.project, "upstream": s.upstream(u.key), "category": "eth_call", "attempt": "1"})
	}
	return out
}

// p15Client sends p15Calls eth_call(latest) and p15Blocks
// eth_getBlockByNumber(latest) per second to im's pod until ctx ends.
func (s *scenario) p15Client(ctx context.Context, im *p15Image) {
	var wg sync.WaitGroup
	defer wg.Wait()
	call := map[string]interface{}{"to": chainsim.Multicall3.Hex(), "data": chainsim.GetBlockNumberSig}
	tick := time.NewTicker(time.Second / p15Calls)
	defer tick.Stop()
	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			var res interface{}
			err := s.call(im.pod, &res, "eth_call", call, "latest")
			d := time.Since(t0)
			im.mu.Lock()
			defer im.mu.Unlock()
			if err != nil {
				im.errs++
				return
			}
			im.lat = append(im.lat, d)
		}()
		if n%(p15Calls/p15Blocks) == 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var raw map[string]interface{}
				_ = s.call(im.pod, &raw, "eth_getBlockByNumber", "latest", false)
			}()
		}
	}
}

// p15Report writes im's tally to b and returns the far node's share of
// first eth_call attempts, in percent, and the client's p50.
func (s *scenario) p15Report(b *strings.Builder, im *p15Image) (float64, time.Duration) {
	im.mu.Lock()
	defer im.mu.Unlock()
	lat := slices.Clone(im.lat)
	slices.Sort(lat)
	var total float64
	for _, u := range p15Ups {
		total += im.last[u.key] - im.first[u.key]
	}
	fmt.Fprintf(b, "pod-%d (%s image): %d eth_call ok, %d failed, p50 %s p90 %s p99 %s; first attempts:",
		im.pod+1, s.image(im.pod), len(lat), im.errs, pctl(lat, 50), pctl(lat, 90), pctl(lat, 99))
	share := map[string]float64{}
	for _, u := range p15Ups {
		if total > 0 {
			share[u.key] = 100 * (im.last[u.key] - im.first[u.key]) / total
		}
		fmt.Fprintf(b, " %s %.1f%%", u.vendor, share[u.key])
	}
	b.WriteString("\n")
	return share["http-n"], pctl(lat, 50)
}
