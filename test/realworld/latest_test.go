package realworld

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

const (
	// p13BlockTime: W is its 2s floor, 8 blocks (§1).
	p13BlockTime = 250 * time.Millisecond
	p13W         = 2 * time.Second
	// p13Idle is a sporadic reader's gap, longer than W and shorter than
	// the full poll, so each read finds the Head of the last full poll or
	// of the last read's refresh; p13Reads reads alternate the methods.
	p13Idle  = 7500 * time.Millisecond
	p13Reads = 24
	// p13Steady: reads at 1/s, alternating the methods, for p13SteadyFor.
	// Each lands one background refresh behind the tip (§6.8): at most
	// p13SteadyLag blocks at p99, as on main for eth_blockNumber.
	p13SteadyEvery = time.Second
	p13SteadyFor   = 90 * time.Second
	p13SteadyLag   = 5
	// p13Quorum is the default served-tip quorum: a refresh Asks at most
	// q voters (§6.8).
	p13Quorum = 2
)

var p13Methods = []string{"eth_getBlockByNumber", "eth_blockNumber"}

// P13: 4 HTTP vendors, 0.25s blocks, a full poll every 10s, no
// subscription. A sporadic reader's eth_getBlockByNumber(latest) and
// eth_blockNumber each answer a block no older than W of chain time when
// sent: a lane whose Latest rests on reports older than W waits for its
// refresh, at most W (§6.8, §7 families 1-2). Steady reads at 1/s keep
// their lag and cost: p99 at most p13SteadyLag blocks, at most q refresh
// Asks per read.
func p13() spec {
	upEvm := map[string]map[string]interface{}{}
	vendors := []vendorSpec{{"http-a", "vendor-a", false}, {"http-b", "vendor-b", false}, {"http-c", "vendor-c", false}, {"http-d", "vendor-d", false}}
	for _, v := range vendors {
		upEvm[v.key] = map[string]interface{}{"statePollerInterval": "10s"}
	}
	return spec{name: "P13", pods: 1, vendors: vendors, upEvm: upEvm, headless: []int{0}, bound: time.Hour,
		sim:     chainsim.Config{BlockTime: p13BlockTime},
		clients: func(*scenario) {},
		body: func(s *scenario) {
			asks := s.refreshAsks()
			var lags []string
			for i := range p13Reads {
				time.Sleep(p13Idle)
				method := p13Methods[i%len(p13Methods)]
				sent := time.Now()
				n, tip, err := p13Read(s, method)
				if err != nil {
					s.violate(s.name, "%s: %v", method, err)
					continue
				}
				age := time.Duration(0)
				if next := s.chain.CanonAt(n + 1); next != nil && next.MintedAt.Before(sent) {
					age = sent.Sub(next.MintedAt)
				}
				lags = append(lags, fmt.Sprintf("%s %d (%s)", strings.TrimPrefix(method, "eth_"), int64(tip)-int64(n), age.Round(time.Millisecond)))
				if age > p13W {
					s.violate(s.name, "%s after %s idle answered %d, %d blocks below the tip %d: the tip had moved on %s before the request, more than W %s", method, p13Idle, n, int64(tip)-int64(n), tip, age.Round(time.Millisecond), p13W)
				}
			}
			s.logf("sporadic lags, blocks (chain age at send): %s", strings.Join(lags, ", "))
			s.logf("sporadic refresh Asks: %.0f over %d reads", s.refreshAsks()-asks, p13Reads)

			asks = s.refreshAsks()
			var steady []int
			start := time.Now()
			for i := 0; time.Since(start) < p13SteadyFor; i++ {
				n, tip, err := p13Read(s, p13Methods[i%len(p13Methods)])
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

// p13Read sends method on pod-1 and returns the block it answered and the
// chain's tip when the answer arrived.
func p13Read(s *scenario, method string) (n, tip uint64, err error) {
	if method == "eth_blockNumber" {
		var bn hexutil.Uint64
		err = s.call(0, &bn, method)
		n = uint64(bn)
	} else {
		var res struct {
			Number hexutil.Uint64 `json:"number"`
		}
		err = s.call(0, &res, method, "latest", false)
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
