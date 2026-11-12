package realworld

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

const (
	// p13BlockTime: a sparse chain's 0.6s blocks; W is its 2s floor (§1).
	p13BlockTime = 600 * time.Millisecond
	p13W         = 2 * time.Second
	// p13Idle is a sporadic reader's gap, longer than 2 W, so each read
	// meets the idle edge, and shorter than the full poll; p13Reads reads
	// alternate the methods.
	p13Idle  = 7500 * time.Millisecond
	p13Reads = 40
	// p13NumberLatency: an idle eth_blockNumber answers from memory, as
	// the old image's: latency p99 at most this, or at most the old
	// image's.
	p13NumberLatency = 50 * time.Millisecond
	// p13Steady: reads at 1/s, alternating the methods, for p13SteadyFor.
	// Each lands one background refresh behind the tip (§6.8): at most
	// p13SteadyLag blocks at p99, as on main for eth_blockNumber.
	p13SteadyEvery = time.Second
	p13SteadyFor   = 90 * time.Second
	p13SteadyLag   = 5
	// p13Quorum is the default served-tip quorum: a refresh Asks at most
	// q voters (§6.8).
	p13Quorum = 2
	// p13Delay, p13Jitter: each vendor serves each block this long after
	// its mint, plus a share of the jitter fixed per vendor and block, so
	// the voters an idle edge asks often answer different heads.
	p13Delay  = 200 * time.Millisecond
	p13Jitter = 300 * time.Millisecond
)

var p13Methods = []string{"eth_getBlockByNumber", "eth_blockNumber"}

// P13: a sparse chain's 2 HTTP voters 80-100 ms away, 0.6s blocks served
// p13Delay plus up to p13Jitter after their mint, a full poll every 10s, no
// subscription; pod-1 runs the branch's image, pod-2 the old one, own
// state each. A sporadic reader sends each read to both pods at once.
// Its eth_getBlockByNumber(latest) on the new image answers a block no
// older than W of chain time when sent: a lane whose Latest rests on
// reports older than W waits for its refresh, at most W (§6.8, §7 family
// 2). Its eth_blockNumber never waits (§7 family 1): latency p99 within
// p13NumberLatency or the old image's, served age p50 and p90 no older
// than the old image's. Steady reads at 1/s on the new image keep their
// lag and cost: p99 at most p13SteadyLag blocks, at most q refresh Asks
// per read.
func p13() spec {
	upEvm := map[string]map[string]interface{}{}
	vendors := []vendorSpec{{"http-a", "vendor-a", false}, {"http-b", "vendor-b", false}}
	slow := []time.Duration{80 * time.Millisecond, 100 * time.Millisecond}
	faults := map[string]chainsim.Faults{}
	for i, v := range vendors {
		upEvm[v.key] = map[string]interface{}{"statePollerInterval": "10s"}
		faults[v.key] = chainsim.Faults{Slow: slow[i], HeadDelay: p13Delay, HeadJitter: p13Jitter}
	}
	return spec{name: "P13", pods: 2, oldPods: []int{1}, memory: true, vendors: vendors, upEvm: upEvm, faults: faults, headless: []int{0, 1}, bound: time.Hour,
		sim:     chainsim.Config{BlockTime: p13BlockTime},
		clients: func(*scenario) {},
		body: func(s *scenario) {
			asks := s.refreshAsks()
			from := time.Now()
			var mu sync.Mutex
			var lags []string
			// ages and took: per method and image, each idle read's served
			// age and latency.
			ages, took := map[string]map[string][]time.Duration{}, map[string]map[string][]time.Duration{}
			for _, m := range p13Methods {
				ages[m], took[m] = map[string][]time.Duration{}, map[string][]time.Duration{}
			}
			for i := range p13Reads {
				time.Sleep(p13Idle)
				method := p13Methods[i%len(p13Methods)]
				var wg sync.WaitGroup
				for _, p := range s.h.pods {
					wg.Add(1)
					go func() {
						defer wg.Done()
						img := s.image(p.idx)
						sent := time.Now()
						n, tip, err := p13Read(s, p.idx, method)
						d := time.Since(sent)
						if err != nil {
							s.violate(s.name, "pod-%d (%s image) %s: %v", p.idx+1, img, method, err)
							return
						}
						age := time.Duration(0)
						if next := s.chain.CanonAt(n + 1); next != nil && next.MintedAt.Before(sent) {
							age = sent.Sub(next.MintedAt)
						}
						mu.Lock()
						lags = append(lags, fmt.Sprintf("%s %s %d (%s, %s)", img, strings.TrimPrefix(method, "eth_"), int64(tip)-int64(n), age.Round(time.Millisecond), d.Round(time.Millisecond)))
						ages[method][img] = append(ages[method][img], age)
						took[method][img] = append(took[method][img], d)
						mu.Unlock()
						if !p.old && method == "eth_getBlockByNumber" && age > p13W {
							s.violate(s.name, "%s after %s idle answered %d, %d blocks below the tip %d: the tip had moved on %s before the request, more than W %s", method, p13Idle, n, int64(tip)-int64(n), tip, age.Round(time.Millisecond), p13W)
						}
					}()
				}
				wg.Wait()
			}
			s.logf("sporadic lags, blocks (chain age at send, latency): %s", strings.Join(lags, ", "))
			for _, m := range p13Methods {
				for _, img := range []string{"new", "old"} {
					slices.Sort(ages[m][img])
					slices.Sort(took[m][img])
					a, d := ages[m][img], took[m][img]
					s.logf("%s image %s after %s idle: %d answers, served age p50 %s p90 %s max %s, latency p50 %s p90 %s p99 %s max %s",
						img, m, p13Idle, len(d), pctl(a, 50), pctl(a, 90), pctl(a, 100), pctl(d, 50), pctl(d, 90), pctl(d, 99), pctl(d, 100))
				}
			}
			to := time.Now()
			nt, ot := took["eth_blockNumber"]["new"], took["eth_blockNumber"]["old"]
			if p99 := pctl(nt, 99); p99 > p13NumberLatency && p99 > pctl(ot, 99)+latencyNoise {
				s.violate(s.name, "%s", s.h.host.verdict(from, to, fmt.Sprintf("new image eth_blockNumber after %s idle: latency p99 %s, above %s and the old image's %s: it waited for the idle edge",
					p13Idle, p99, p13NumberLatency, pctl(ot, 99))))
			}
			na, oa := ages["eth_blockNumber"]["new"], ages["eth_blockNumber"]["old"]
			for _, p := range []int{50, 90} {
				if pctl(na, p) > pctl(oa, p) {
					s.violate(s.name, "new image eth_blockNumber after %s idle: served age p%d %s, older than the old image's %s", p13Idle, p, pctl(na, p), pctl(oa, p))
				}
			}
			s.logf("sporadic refresh Asks: %.0f over %d reads", s.refreshAsks()-asks, p13Reads)

			asks = s.refreshAsks()
			var steady []int
			start := time.Now()
			for i := 0; time.Since(start) < p13SteadyFor; i++ {
				n, tip, err := p13Read(s, 0, p13Methods[i%len(p13Methods)])
				if err != nil {
					s.violate(s.name, "%s: %v", p13Methods[i%len(p13Methods)], err)
				} else {
					steady = append(steady, int(int64(tip)-int64(n)))
				}
				time.Sleep(time.Until(start.Add(time.Duration(i+1) * p13SteadyEvery)))
			}
			spent, secs := s.refreshAsks()-asks, time.Since(start).Seconds()
			slices.Sort(steady)
			if len(steady) == 0 {
				s.violate(s.name, "no steady read answered")
				return
			}
			p50, p99 := steady[len(steady)/2], steady[min(len(steady)-1, len(steady)*99/100)]
			s.logf("steady: %d reads, lag p50 %d p99 %d max %d blocks; refresh Asks %.0f, %.2f/s", len(steady), p50, p99, steady[len(steady)-1], spent, spent/secs)
			if p99 > p13SteadyLag {
				s.violate(s.name, "steady reads at 1/s: lag p99 %d blocks, more than %d", p99, p13SteadyLag)
			}
			if limit := float64(p13Quorum * len(steady)); spent > limit {
				s.violate(s.name, "steady reads at 1/s: %.0f refresh Asks for %d reads, more than q (%d) per read", spent, len(steady), p13Quorum)
			}
		}}
}

// p13Read sends method on pod and returns the block it answered and the
// chain's tip when the answer arrived.
func p13Read(s *scenario, pod int, method string) (n, tip uint64, err error) {
	if method == "eth_blockNumber" {
		var bn hexutil.Uint64
		err = s.call(pod, &bn, method)
		n = uint64(bn)
	} else {
		var res struct {
			Number hexutil.Uint64 `json:"number"`
		}
		err = s.call(pod, &res, method, "latest", false)
		n = uint64(res.Number)
	}
	tip = s.chain.Tip().Number
	if err == nil && n == 0 {
		err = fmt.Errorf("answered no block")
	}
	return n, tip, err
}

// refreshAsks is pod-1's refresh Asks so far, every outcome.
func (s *scenario) refreshAsks() float64 {
	v, _ := s.metric(0, "erpc_head_ask_total", map[string]string{"reason": "refresh"})
	return v
}
