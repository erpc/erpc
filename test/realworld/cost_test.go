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
	forkSettles = costProbe{"erpc_chain_tracker_settle_total", map[string]string{"reason": "fork"}}
	refreshAsks = costProbe{"erpc_head_ask_total", map[string]string{"reason": "refresh"}}
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
// poll's vote is a root of its own, above the head or below it; none is
// evidence of a fork.
func z1() spec {
	return costSpec("Z1", false, nil, func(s *scenario) {
		time.Sleep(costWarm)
		perBlock, inc := s.costSpan(costMeasure, forkSettles, refreshAsks)
		s.logf("Z1: %.3f vendor calls per block; fork settles %v, refresh Asks %v", perBlock, inc[0], inc[1])
		s.requireNoForkEvidence(inc[0])
		if inc[1] > 0 {
			s.violate(s.name, "pod-1: %v fork polls (refresh Asks) with no client", inc[1])
		}
	})
}
