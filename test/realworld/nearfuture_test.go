package realworld

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

const (
	// p12BlockTime: a 0.3s chain, so W is its 2s floor and the near future
	// W / bt + 1 = 7 blocks (§6.4).
	p12BlockTime = 300 * time.Millisecond
	// p12Ahead: clients ask 1 to p12Ahead blocks above every vendor's
	// head; p12Far is more than W of chain above it (§19 P12).
	p12Ahead = 5
	p12Far   = 50
	// p12Probes near-future reads per method; p12FarProbes far ones.
	p12Probes    = 40
	p12FarProbes = 6
	// p12Slack is harness, not spec: an HTTP round trip and a head fetch
	// of a -race binary on top of W.
	p12Slack = 500 * time.Millisecond
	// p12Lag: http-c serves this far behind the chain and declares no
	// upper bound, so its consensus slot misses the blocks it lacks.
	p12Lag = 2
	// p12StaleAnswer: in the second half every http-c answer takes this
	// long, more than a block, so its reports stay older than 1 bt even
	// right after an Ask.
	p12StaleAnswer = 400 * time.Millisecond
)

// p12Policy is a tip policy of a 0.3s chain's production config: 1 of 3
// upstreams agreeing, non-empty preferred, one hedge, retries with no
// delay and one data-unavailable attempt.
func p12Policy(method string) map[string]interface{} {
	return map[string]interface{}{
		"matchMethod":   method,
		"matchFinality": []string{"unknown", "realtime", "unfinalized"},
		"timeout":       map[string]interface{}{"duration": "60s", "quantile": 0.99, "minDuration": "40s", "maxDuration": "60s"},
		"hedge":         map[string]interface{}{"maxCount": 1, "delay": map[string]interface{}{"quantile": 0.95, "min": "100ms", "max": "1s"}},
		"retry":         map[string]interface{}{"maxAttempts": 6, "delay": "0s", "emptyResultDelay": "200ms", "emptyResultMaxAttempts": 1},
		"consensus": map[string]interface{}{
			"maxParticipants": 3, "agreementThreshold": 1, "preferNonEmpty": true, "preferLargerResponses": false,
			"lowParticipantsBehavior": "acceptMostCommonValidResult", "disputeBehavior": "acceptMostCommonValidResult",
			"ignoreFields": map[string][]string{"eth_getLogs": {"*.blockTimestamp"}},
		},
	}
}

// P12: a 0.3s chain behind three https vendors, eth_getLogs and
// eth_getBlockByNumber through 1-of-3 consensus slots and a hedge. Clients
// ask 1–5 blocks above every vendor's head: each is answered once the
// chain reaches its block, within W, never with a block-unavailable error
// (§6.4 near future), first with fresh reports, then with http-c's
// reports older than 1 bt. http-c lags, so its slot misses blocks the
// others serve: its siblings are not stopped (§6.4 every leg). A block
// more than W of chain ahead gets an error: refused at once, counted
// future, or, with a stale report, after the band's retry.
func p12() spec {
	bound := map[string]interface{}{"upper": map[string]interface{}{"latestBlockMinus": 0, "probe": "blockHeader"}}
	return spec{name: "P12", bound: healthyDelivery, sim: chainsim.Config{BlockTime: p12BlockTime},
		vendors: []vendorSpec{{"http-a", "vendor-a", false}, {"http-b", "vendor-b", false}, {"http-c", "vendor-c", false}},
		faults:  map[string]chainsim.Faults{"http-c": {Lag: p12Lag}},
		upEvm: map[string]map[string]interface{}{
			"http-a": {"statePollerInterval": "500ms", "blockAvailability": bound},
			"http-b": {"statePollerInterval": "500ms", "blockAvailability": bound},
			"http-c": {"statePollerInterval": "5s"},
		},
		evm: map[string]interface{}{"getLogsSplitConcurrency": 5},
		network: map[string]interface{}{"failsafe": []map[string]interface{}{
			p12Policy("eth_getLogs"), p12Policy("eth_getBlockByNumber"),
			{"matchMethod": "*", "retry": map[string]interface{}{"maxAttempts": 3, "delay": "0s"}},
		}},
		clients: func(*scenario) {},
		body:    p12Body,
	}
}

func p12Body(s *scenario) {
	s.blocks(25)
	missing0 := make([]float64, len(s.h.pods))
	for pod := range s.h.pods {
		missing0[pod], _ = s.metric(pod, "erpc_head_refusal_total", map[string]string{"rule": "retried_missing"})
	}
	empty := make([]float64, len(s.h.pods))
	s.p12Probe("fresh reports", false, empty)
	s.vendors["http-c"].Update(func(f *chainsim.Faults) { f.Slow = p12StaleAnswer })
	s.blocks(10)
	s.p12Probe("http-c stale", true, empty)
	s.blocks(10)
	// A band read answered empty or missing is a retried_missing (§17);
	// a read that got its data never is.
	for pod := range s.h.pods {
		v, _ := s.metric(pod, "erpc_head_refusal_total", map[string]string{"rule": "retried_missing"})
		if d := v - missing0[pod]; d > empty[pod] {
			s.violate(s.name, "pod-%d: %v retried_missing, more than its %v empty or missing answers", pod+1, d, empty[pod])
		} else {
			s.owe(pod, "retried_missing", d)
		}
	}
}

// p12Probe runs P12's near-future and far reads once; stale says http-c's
// reports are older than 1 bt, so a far block may take the band's retry.
// empty counts, per pod, the reads answered empty or with an error.
func (s *scenario) p12Probe(label string, stale bool, empty []float64) {
	waits := map[string][]time.Duration{}
	unavailable := map[string]int{}
	for i := range 2 * p12Probes {
		pod := (i / 2) % len(s.h.pods)
		method := "eth_getLogs"
		if i%2 == 1 {
			method = "eth_getBlockByNumber"
		}
		k := uint64(1 + (i/2)%p12Ahead)
		n := s.chain.Tip().Number + k
		start := time.Now()
		got, err := s.p12Read(pod, method, n)
		took := time.Since(start)
		waits[method] = append(waits[method], took)
		if err != nil || got == "null" || got == "0 logs of []" {
			empty[pod]++
		}
		switch want := s.chain.CanonAt(n); {
		case err != nil:
			if strings.Contains(err.Error(), "highest known block") || strings.Contains(err.Error(), "not found") {
				unavailable[method]++
			}
			s.violate(s.name, "%s, pod-%d: %s(%d), %d above the tip, answered %v after %s", label, pod+1, method, n, k, err, took.Round(time.Millisecond))
		case want == nil:
			s.violate(s.name, "%s, pod-%d: %s(%d) answered %s before the chain reached it", label, pod+1, method, n, got)
		case got != p12Want(method, want):
			if got == "null" {
				unavailable[method]++
			}
			s.violate(s.name, "%s, pod-%d: %s(%d), %d above the tip, answered %s, not %s", label, pod+1, method, n, k, got, p12Want(method, want))
		case took > liveBound+p12Slack:
			s.violate(s.name, "%s", s.h.host.verdict(start, time.Now(), fmt.Sprintf("%s, pod-%d: %s(%d), %d above the tip, answered after %s (bound %s)", label, pod+1, method, n, k, took.Round(time.Millisecond), liveBound+p12Slack)))
		}
		s.blocks(1)
	}
	for _, method := range []string{"eth_getLogs", "eth_getBlockByNumber"} {
		w := waits[method]
		s.logf("%s: %s: %d near-future reads, %d block-unavailable errors, wait p50 %s p99 %s", label, method, len(w), unavailable[method],
			p12Quantile(w, 0.5).Round(time.Millisecond), p12Quantile(w, 0.99).Round(time.Millisecond))
	}

	future := func(pod int) float64 {
		total := 0.0
		for _, rule := range []string{"future", "future_after_refresh"} {
			v, _ := s.metric(pod, "erpc_head_refusal_total", map[string]string{"rule": rule})
			total += v
		}
		return total
	}
	before := make([]float64, len(s.h.pods))
	for pod := range s.h.pods {
		before[pod] = future(pod)
	}
	refused := make([]float64, len(s.h.pods))
	for i := range p12FarProbes {
		pod := i % len(s.h.pods)
		n := s.chain.Tip().Number + p12Far
		start := time.Now()
		got, err := s.p12Read(pod, "eth_getLogs", n)
		took := time.Since(start)
		if err == nil {
			s.violate(s.name, "%s, pod-%d: eth_getLogs(%d), %d above the tip, answered %s", label, pod+1, n, p12Far, got)
			continue
		}
		empty[pod]++
		switch {
		case strings.Contains(err.Error(), "highest known block"):
			refused[pod]++
			if took >= liveBound {
				s.violate(s.name, "%s", s.h.host.verdict(start, time.Now(), fmt.Sprintf("%s, pod-%d: eth_getLogs(%d), %d above the tip, refused after %s, not at once", label, pod+1, n, p12Far, took.Round(time.Millisecond))))
			}
		case !stale:
			s.violate(s.name, "%s, pod-%d: eth_getLogs(%d), %d above the tip, failed with %v, not a block-unavailable refusal", label, pod+1, n, p12Far, err)
		case took > liveBound+p12Slack:
			s.violate(s.name, "%s", s.h.host.verdict(start, time.Now(), fmt.Sprintf("%s, pod-%d: eth_getLogs(%d), %d above the tip, failed after the band's retry in %s (bound %s)", label, pod+1, n, p12Far, took.Round(time.Millisecond), liveBound+p12Slack)))
		}
	}
	for pod := range s.h.pods {
		if got := future(pod) - before[pod]; got < refused[pod] {
			s.violate(s.name, "%s, pod-%d: %v far-future reads counted future, not %v", label, pod+1, got, refused[pod])
		}
	}
}

// p12Read is a read of block n: eth_getLogs over [n, n] as the block hash
// and the count of its logs, or eth_getBlockByNumber's hash.
func (s *scenario) p12Read(pod int, method string, n uint64) (string, error) {
	if method == "eth_getBlockByNumber" {
		var raw json.RawMessage
		err := s.call(pod, &raw, method, hexutil.EncodeUint64(n), false)
		if err != nil {
			return "", err
		}
		return answerHash(raw, nil), nil
	}
	var logs []struct {
		BlockHash   hash           `json:"blockHash"`
		BlockNumber hexutil.Uint64 `json:"blockNumber"`
	}
	q := map[string]interface{}{"fromBlock": hexutil.EncodeUint64(n), "toBlock": hexutil.EncodeUint64(n)}
	if err := s.call(pod, &logs, method, q); err != nil {
		return "", err
	}
	var hashes []string
	for _, l := range logs {
		if uint64(l.BlockNumber) != n {
			return fmt.Sprintf("a log of block %d", l.BlockNumber), nil
		}
		if !slices.Contains(hashes, l.BlockHash.Hex()) {
			hashes = append(hashes, l.BlockHash.Hex())
		}
	}
	return fmt.Sprintf("%d logs of %v", len(logs), hashes), nil
}

// p12Want is what p12Read answers for block b.
func p12Want(method string, b *chainsim.Block) string {
	if method == "eth_getBlockByNumber" {
		return b.Hash.Hex()
	}
	var hashes []string
	if len(b.Logs()) > 0 {
		hashes = []string{b.Hash.Hex()}
	}
	return fmt.Sprintf("%d logs of %v", len(b.Logs()), hashes)
}

// p12Quantile is the q-quantile of ws, 0 when empty.
func p12Quantile(ws []time.Duration, q float64) time.Duration {
	if len(ws) == 0 {
		return 0
	}
	s := slices.Clone(ws)
	slices.Sort(s)
	return s[min(len(s)-1, int(q*float64(len(s))))]
}
