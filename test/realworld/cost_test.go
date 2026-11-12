package realworld

import (
	"bufio"
	"os"
	"strings"
	"time"

	"github.com/erpc/erpc/test/chainsim"
)

// The Z rows (§19) measure what one pod costs its vendors on a fast chain
// that no source pushes, as most networks of a fleet run: 4 HTTP vendors
// of 4 vendors, 0.25s blocks, a full poll every 10s.
var costVendors = []vendorSpec{
	{"http-a", "vendor-a", false}, {"http-b", "vendor-b", false},
	{"http-c", "vendor-c", false}, {"http-d", "vendor-d", false},
}

const (
	costBlock    = 250 * time.Millisecond
	costFullPoll = 10 * time.Second
	// costWarm is harness time: past the cold window and two full polls of
	// every vendor.
	costWarm    = 30 * time.Second
	costMeasure = 120 * time.Second
	// costMainPerBlock is origin/main's (f9d1885e) vendor calls per block
	// on Z1's setup, measured over 720 blocks; Z1 allows 10% more.
	costMainPerBlock = 0.200
	// demotionLog is the minority-fork demotion's log line (§17 Logs).
	demotionLog = "minority-fork demotion changed"
)

func costSpec(name string, integrity bool, clients, body func(*scenario)) spec {
	upEvm := map[string]map[string]interface{}{}
	for _, v := range costVendors {
		upEvm[v.key] = map[string]interface{}{"statePollerInterval": costFullPoll.String()}
	}
	if clients == nil {
		clients = func(*scenario) {}
	}
	return spec{name: name, pods: 1, vendors: costVendors, integrity: integrity, upEvm: upEvm, headless: []int{0},
		bound: time.Hour, sim: chainsim.Config{BlockTime: costBlock}, clients: clients, body: body}
}

// costProbe is one metric a Z row reads on pod 1, summed over labels.
type costProbe struct {
	name   string
	labels map[string]string
}

var (
	settles     = costProbe{"erpc_chain_tracker_settle_total", nil}
	tieSettles  = costProbe{"erpc_chain_tracker_settle_total", map[string]string{"reason": "tie"}}
	forkSettles = costProbe{"erpc_chain_tracker_settle_total", map[string]string{"reason": "fork"}}
	refreshAsks = costProbe{"erpc_head_ask_total", map[string]string{"reason": "refresh"}}
	headFetches = costProbe{"erpc_chain_tracker_fetch_total", map[string]string{"class": "head"}}
)

// vendorCalls are the JSON-RPC calls every vendor answered.
func (s *scenario) vendorCalls() (n int64) {
	for _, v := range s.vendors {
		n += v.Stats().Calls
	}
	return n
}

// costSpan measures pod 1 over d: the vendor calls per block minted, and
// the increase of each probe.
func (s *scenario) costSpan(d time.Duration, probes ...costProbe) (perBlock float64, inc []float64) {
	read := func() []float64 {
		out := make([]float64, len(probes))
		for i, p := range probes {
			out[i], _ = s.metric(0, p.name, p.labels)
		}
		return out
	}
	before, calls, tip := read(), s.vendorCalls(), s.chain.Tip().Number
	time.Sleep(d)
	after := read()
	blocks := float64(s.chain.Tip().Number - tip)
	perBlock = float64(s.vendorCalls()-calls) / max(blocks, 1)
	inc = make([]float64, len(probes))
	for i := range probes {
		inc[i] = after[i] - before[i]
	}
	return perBlock, inc
}

// logged counts the scenario network's log lines on pod, over every start
// of it, that hold msg.
func (s *scenario) logged(pod int, msg string) (n int) {
	p := s.h.pods[pod]
	p.mu.Lock()
	files := append([]string(nil), p.logs...)
	p.mu.Unlock()
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			if line := sc.Text(); strings.Contains(line, msg) && strings.Contains(line, `"`+s.network()+`"`) {
				n++
			}
		}
		_ = fh.Close()
	}
	return n
}

// requireNoForkEvidence: on an honest chain no vote is off the pins, so
// no fork refresh, fork poll or demotion runs (§6.3). Refresh Asks count
// fork polls only while no client asks `latest` (§6.8).
func (s *scenario) requireNoForkEvidence(forkSettles float64) {
	if forkSettles > 0 {
		s.violate(s.name, "pod-1: %v fork refresh rounds on an honest chain", forkSettles)
	}
	if n := s.logged(0, demotionLog); n > 0 {
		s.violate(s.name, "pod-1: %d minority-fork demotion changes on an honest chain", n)
	}
}

// Z1: idle, integrity off, no client. The tracker does not walk, so each
// poll's vote is a root of its own, above the head or below it: none is
// evidence of a fork, and a tie between them is rule 4's to break, with
// no settle. The pod costs its vendors at most main's calls per block
// plus 10%.
func z1() spec {
	return costSpec("Z1", false, nil, func(s *scenario) {
		time.Sleep(costWarm)
		perBlock, inc := s.costSpan(costMeasure, settles, forkSettles, refreshAsks)
		s.logf("Z1: %.3f vendor calls per block (main %.3f); settles %v, fork settles %v, refresh Asks %v", perBlock, costMainPerBlock, inc[0], inc[1], inc[2])
		s.requireNoForkEvidence(inc[1])
		if inc[2] > 0 {
			s.violate(s.name, "pod-1: %v fork polls (refresh Asks) with no client", inc[2])
		}
		if inc[0] > 0 {
			s.violate(s.name, "pod-1: %v settle rounds on an idle tracker that does not walk", inc[0])
		}
		if limit := costMainPerBlock * 1.1; perBlock > limit {
			s.violate(s.name, "pod-1: %.3f vendor calls per block, above main's %.3f + 10%%", perBlock, costMainPerBlock)
		}
	})
}

// Z2: Z1 with one client asking `latest` every second. Its pre-forward
// refreshes vote too (§6.8), and still no tie settles and nothing is
// demoted.
func z2() spec {
	clients := func(s *scenario) {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for s.ctx.Err() == nil {
				var res map[string]interface{}
				_ = s.call(0, &res, "eth_getBlockByNumber", "latest", false)
				sleepCtx(s.ctx, time.Second)
			}
		}()
	}
	return costSpec("Z2", false, clients, func(s *scenario) {
		time.Sleep(costWarm)
		perBlock, inc := s.costSpan(costMeasure, tieSettles, forkSettles)
		s.logf("Z2: %.3f vendor calls per block; tie settles %v, fork settles %v", perBlock, inc[0], inc[1])
		s.requireNoForkEvidence(inc[1])
		if inc[0] > 0 {
			s.violate(s.name, "pod-1: %v tie settle rounds on a tracker that does not walk", inc[0])
		}
	})
}

// Z3: Z1 with integrity on, which reads the tracker's history, and no
// client request at all, the pod's boot included. History starts at the
// network's first client request, as main started its chain view
// (§6.5 rule 1): until then the tracker walks nothing; that request starts
// the walks.
func z3() spec {
	sp := costSpec("Z3", true, nil, func(s *scenario) {
		time.Sleep(costWarm)
		perBlock, inc := s.costSpan(costMeasure, headFetches)
		s.logf("Z3: %.3f vendor calls per block; head-class fetches %v with no client", perBlock, inc[0])
		if inc[0] > 0 {
			s.violate(s.name, "pod-1: %v head-class fetches (walks) before any client request", inc[0])
		}
		before, _ := s.metric(0, headFetches.name, headFetches.labels)
		var res map[string]interface{}
		if err := s.call(0, &res, "eth_getBlockByNumber", "latest", false); err != nil {
			s.violate(s.name, "pod-1: the first client request failed: %v", err)
		}
		if v, ok := s.waitMetric(0, headFetches.name, headFetches.labels, 3*costFullPoll, func(v float64) bool { return v > before }); !ok {
			s.violate(s.name, "pod-1: head-class fetches stayed %v for %s after the first client request", v, 3*costFullPoll)
		}
	})
	sp.idle = true
	return sp
}
