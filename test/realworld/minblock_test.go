package realworld

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	// minBlockCode is the min-block refusal (streams spec §8).
	minBlockCode = -32015
	// m2Far is far above the near-future bound: (W + A) / bt is about 10
	// blocks on a 0.2s chain (W 2s).
	m2Far = 10_000
	// m4Lag: the lane's vendors serve this far behind; less than W of chain.
	m4Lag = 3
	// m4Batch calls in one batch, each with the header.
	m4Batch = 20
	// minBlockSlack is harness, not spec: an HTTP round trip of a -race
	// binary on top of W.
	minBlockSlack = time.Second
)

// minBlockCall runs eth_call(latest) to Multicall3 getBlockNumber() on pod
// through selector sel with X-ERPC-Min-Block n: the block it executed at.
func (s *scenario) minBlockCall(pod int, sel string, n uint64) (uint64, error) {
	c, err := s.minBlockClient(pod, sel, n)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), httpCheckWait)
	defer cancel()
	var res hexutil.Bytes
	call := map[string]interface{}{"to": chainsim.Multicall3.Hex(), "data": chainsim.GetBlockNumberSig}
	if err := c.CallContext(ctx, &res, "eth_call", call, "latest"); err != nil {
		return 0, err
	}
	return new(big.Int).SetBytes(res).Uint64(), nil
}

func (s *scenario) minBlockClient(pod int, sel string, n uint64) (*rpc.Client, error) {
	p := s.h.pods[pod]
	opts := []rpc.ClientOption{rpc.WithHTTPClient(p.httpc), rpc.WithHeader("X-ERPC-Min-Block", hexutil.EncodeUint64(n))}
	if sel != "" {
		opts = append(opts, rpc.WithHeader("X-ERPC-Use-Upstream", sel))
	}
	return rpc.DialOptions(context.Background(), p.httpURL(s.project, s.chain.ID()), opts...)
}

// isMinBlockRefusal: err is the -32015 refusal, over HTTP 503.
func isMinBlockRefusal(err error) bool {
	var he rpc.HTTPError
	if errors.As(err, &he) {
		return he.StatusCode == 503 && strings.Contains(string(he.Body), fmt.Sprintf(`"code":%d`, minBlockCode))
	}
	var re rpc.Error
	return errors.As(err, &re) && re.ErrorCode() == minBlockCode
}

// M1: X2's pods (pod-2 numbered only, hearing every vendor x2Latency
// late). For each block N a pod-1 newHeads client receives, eth_call(latest)
// to Multicall3 getBlockNumber() on pod-2 with X-ERPC-Min-Block N executes
// at N or above, every time (streams §8: the case it fixes).
func m1() spec {
	return spec{name: "M1", memory: true, numbered: []int{1}, bound: healthyDelivery,
		clients: func(s *scenario) { s.podClients(0, 0) },
		onHead: func(s *scenario, c *headsClient, pod int, n uint64, _ hash) {
			if pod != 0 || !s.first(fmt.Sprintf("m1/%d", n)) {
				return
			}
			s.checks.Add(1)
			go func() {
				defer s.checks.Done()
				if at, err := s.minBlockCall(1, "", n); err != nil || at < n {
					s.violate(c.name, "pod-2: eth_call(latest) with min-block %d executed at %d (err %v)", n, at, err)
				}
			}()
		},
		body: func(s *scenario) {
			for _, px := range s.proxies {
				px.set(s.h.pods[1], linkFaults{Latency: x2Latency})
			}
			s.blocks(150)
		}}
}

// M2: min-block 1–3 blocks above the tip is served once the chain reaches
// it, within W; m2Far above it is refused -32015 at once, with no eth_call
// dispatched, counted future.
func m2() spec {
	return spec{name: "M2", bound: healthyDelivery, body: func(s *scenario) {
		s.blocks(25)
		w := 2 * time.Second
		for i := range 20 {
			pod := i % len(s.h.pods)
			n := s.chain.Tip().Number + uint64(1+i%3)
			start := time.Now()
			at, err := s.minBlockCall(pod, "", n)
			switch took := time.Since(start); {
			case err != nil:
				s.violate(s.name, "pod-%d: min-block %d: %v", pod+1, n, err)
			case at < n:
				s.violate(s.name, "pod-%d: min-block %d executed at %d", pod+1, n, at)
			case took > w+minBlockSlack:
				s.violate(s.name, "pod-%d: min-block %d answered after %v, more than W", pod+1, n, took)
			}
			s.blocks(3)
		}
		for pod := range s.h.pods {
			calls0, _ := s.metric(pod, "erpc_upstream_request_total", map[string]string{"category": "eth_call"})
			future0, _ := s.metric(pod, "erpc_min_block_total", map[string]string{"outcome": "future"})
			n := s.chain.Tip().Number + m2Far
			start := time.Now()
			_, err := s.minBlockCall(pod, "", n)
			took := time.Since(start)
			calls1, _ := s.metric(pod, "erpc_upstream_request_total", map[string]string{"category": "eth_call"})
			future1, _ := s.metric(pod, "erpc_min_block_total", map[string]string{"outcome": "future"})
			switch {
			case !isMinBlockRefusal(err):
				s.violate(s.name, "pod-%d: min-block %d, far ahead: got %v, not a %d refusal", pod+1, n, err, minBlockCode)
			case took > minBlockSlack:
				s.violate(s.name, "pod-%d: min-block %d, far ahead, refused after %v, not at once", pod+1, n, took)
			case calls1 != calls0:
				s.violate(s.name, "pod-%d: min-block %d, far ahead, dispatched %v eth_calls", pod+1, n, calls1-calls0)
			case future1 != future0+1:
				s.violate(s.name, "pod-%d: erpc_min_block_total{outcome=future} %v → %v, not +1", pod+1, future0, future1)
			}
		}
	}}
}

// M3: pod-2 hears every vendor x2Latency late. eth_call(latest) is cached
// at the block r it ran at; once the chain is past r, the same call with
// min-block r+1 executes at r+1 or above, never the cached r.
func m3() spec {
	return spec{name: "M3", cache: true, numbered: []int{1}, bound: healthyDelivery,
		clients: func(s *scenario) { s.podClients(0, 0) },
		body: func(s *scenario) {
			for _, px := range s.proxies {
				px.set(s.h.pods[1], linkFaults{Latency: x2Latency})
			}
			s.blocks(25)
			call := map[string]interface{}{"to": chainsim.Multicall3.Hex(), "data": chainsim.GetBlockNumberSig}
			for range 20 {
				var res hexutil.Bytes
				if err := s.call(1, &res, "eth_call", call, "latest"); err != nil {
					s.violate(s.name, "pod-2: eth_call(latest): %v", err)
					continue
				}
				r := new(big.Int).SetBytes(res).Uint64()
				for s.chain.Tip().Number <= r {
					s.blocks(1)
				}
				if at, err := s.minBlockCall(1, "", r+1); err != nil || at <= r {
					s.violate(s.name, "pod-2: min-block %d after latest cached at %d executed at %d (err %v)", r+1, r, at, err)
				}
				s.blocks(5)
			}
		}}
}

// M4: a use-upstream lane (vendor-b) trails the network m4Lag blocks. A
// batch of m4Batch eth_call(latest) on the lane with min-block = the
// chain's tip: each executes at the tip or above, on the lane, and the
// wait Asks each lane voter once per round, not once per call.
func m4() spec {
	lag := chainsim.Faults{Lag: m4Lag}
	return spec{name: "M4", bound: healthyDelivery + m4Lag*blockTime,
		vendors: []vendorSpec{
			{"lead-ws", "vendor-a", true}, {"lead-http", "vendor-a", false},
			{"lane-ws", "vendor-b", true}, {"lane-http", "vendor-b", false},
		},
		faults: map[string]chainsim.Faults{"lane-ws": lag, "lane-http": lag},
		body: func(s *scenario) {
			s.blocks(25)
			const sel = "m4-lane-*"
			// One round per bt within W, at most 2 lane voters each.
			maxAsks := float64(2 * (int(2*time.Second/blockTime) + 1))
			for i := range 10 {
				pod := i % len(s.h.pods)
				n := s.chain.Tip().Number
				asks0, _ := s.metric(pod, "erpc_head_ask_total", map[string]string{"reason": "refresh", "outcome": "fetched"})
				c, err := s.minBlockClient(pod, sel, n)
				if err != nil {
					s.violate(s.name, "pod-%d: dial: %v", pod+1, err)
					continue
				}
				call := map[string]interface{}{"to": chainsim.Multicall3.Hex(), "data": chainsim.GetBlockNumberSig}
				batch := make([]rpc.BatchElem, m4Batch)
				res := make([]hexutil.Bytes, m4Batch)
				for j := range batch {
					batch[j] = rpc.BatchElem{Method: "eth_call", Args: []interface{}{call, "latest"}, Result: &res[j]}
				}
				ctx, cancel := context.WithTimeout(context.Background(), httpCheckWait)
				err = c.BatchCallContext(ctx, batch)
				cancel()
				c.Close()
				if err != nil {
					s.violate(s.name, "pod-%d: batch with min-block %d: %v", pod+1, n, err)
					continue
				}
				for j, e := range batch {
					if at := new(big.Int).SetBytes(res[j]).Uint64(); e.Error != nil || at < n {
						s.violate(s.name, "pod-%d: batch call %d with min-block %d on %s executed at %d (err %v)", pod+1, j, n, sel, at, e.Error)
					}
				}
				if asks1, _ := s.metric(pod, "erpc_head_ask_total", map[string]string{"reason": "refresh", "outcome": "fetched"}); asks1-asks0 > maxAsks {
					s.violate(s.name, "pod-%d: %v refresh Asks fetched for one batch of %d, more than %v", pod+1, asks1-asks0, m4Batch, maxAsks)
				}
				s.blocks(10)
			}
		}}
}
