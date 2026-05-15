package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
)

// ---- HTTP-based mock upstream (gock-free, parallel-safe) ----

type benchMockUpstream struct {
	server       *httptest.Server
	requestCount atomic.Int64
}

// newBenchUpstream returns a server that answers chainId / blockNumber /
// getBlockByNumber / syncing for the bootstrap, plus a configurable
// response for the workload method. The behaviour function lets
// individual benches inject delays or failures.
func newBenchUpstream(targetMethod string, behaviour func(reqNum int64) (status int, errBody string, delay time.Duration)) *benchMockUpstream {
	m := &benchMockUpstream{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqNum := m.requestCount.Add(1)

		body, _ := io.ReadAll(r.Body)
		defer r.Body.Close()

		var jrq map[string]interface{}
		_ = json.Unmarshal(body, &jrq)

		method, _ := jrq["method"].(string)
		id := jrq["id"]

		// Bootstrap / state-poller endpoints — always succeed instantly.
		switch method {
		case "eth_chainId":
			writeJsonRpc(w, id, "0x7b")
			return
		case "eth_blockNumber":
			writeJsonRpc(w, id, "0x11118888")
			return
		case "eth_getBlockByNumber":
			writeJsonRpc(w, id, map[string]interface{}{
				"number":    "0x11118888",
				"timestamp": "0x6702a8f0",
				"hash":      "0xabc",
			})
			return
		case "eth_syncing":
			writeJsonRpc(w, id, false)
			return
		}

		// Workload method — apply behaviour.
		if behaviour != nil && method == targetMethod {
			status, errBody, delay := behaviour(reqNum)
			if delay > 0 {
				time.Sleep(delay)
			}
			if status >= 500 || errBody != "" {
				w.Header().Set("Content-Type", "application/json")
				if errBody == "" {
					errBody = `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"mock error"}}`
				}
				if status == 0 {
					status = 500
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(errBody))
				return
			}
		}
		writeJsonRpc(w, id, "0x1234")
	})
	m.server = httptest.NewServer(handler)
	return m
}

func writeJsonRpc(w http.ResponseWriter, id interface{}, result interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func (s *benchMockUpstream) URL() string { return s.server.URL }
func (s *benchMockUpstream) Close()      { s.server.Close() }

// ---- shared bench fixture: Network + upstreams ready for Forward ----

type benchNetwork struct {
	ntw    *Network
	cancel context.CancelFunc
	mocks  []*benchMockUpstream
}

func (bn *benchNetwork) Close() {
	bn.cancel()
	for _, m := range bn.mocks {
		m.Close()
	}
}

func setupBenchNetwork(b testing.TB, fsCfg []*common.FailsafeConfig, mocks []*benchMockUpstream) *benchNetwork {
	util.ConfigureTestLogger()
	util.ResetGock() // allow localhost passthrough

	ctx, cancel := context.WithCancel(context.Background())

	rlr, err := upstream.NewRateLimitersRegistry(ctx, &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	}, &log.Logger)
	if err != nil {
		cancel()
		b.Fatalf("rate limiter registry: %v", err)
	}
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	if err != nil {
		cancel()
		b.Fatalf("providers registry: %v", err)
	}
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000, MaxTotalSize: "1GB",
			},
		},
	})
	if err != nil {
		cancel()
		b.Fatalf("shared state registry: %v", err)
	}

	mt := health.NewTracker(&log.Logger, "benchProject", 2*time.Second)

	upCfgs := make([]*common.UpstreamConfig, len(mocks))
	for i, m := range mocks {
		upCfgs[i] = &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       fmt.Sprintf("up%d", i+1),
			Endpoint: m.URL(),
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		}
	}

	upr := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "benchProject",
		upCfgs, ssr, rlr, vr, pr, nil, mt,
		1*time.Second, nil, nil,
	)
	upr.Bootstrap(ctx)
	time.Sleep(300 * time.Millisecond)
	if err := upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)); err != nil {
		cancel()
		b.Fatalf("prepare upstreams: %v", err)
	}

	ntw, err := NewNetwork(
		ctx, &log.Logger, "benchProject",
		&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
			Failsafe:     fsCfg,
		},
		rlr, upr, mt,
	)
	if err != nil {
		cancel()
		b.Fatalf("new network: %v", err)
	}
	return &benchNetwork{ntw: ntw, cancel: cancel, mocks: mocks}
}

const benchMethod = "eth_getBalance"

func benchRequestBody() []byte {
	return []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)
}

func runForwardBench(b *testing.B, bn *benchNetwork) {
	body := benchRequestBody()
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := common.NewNormalizedRequest(body)
			resp, _ := bn.ntw.Forward(context.Background(), req)
			if resp != nil {
				resp.Release()
			}
		}
	})
}

// ============================================================================
// Production-scenario benchmarks.
// Each test layers the policies the way a real operator would in a given
// workload pattern, so the measurements reflect actual on-the-wire cost
// of the failsafe stack — not synthetic single-policy isolation.
// ============================================================================

// 1) Baseline read serving a "warm head" workload typical of DeFi /
// indexer-tail traffic: short network timeout, small retry budget, an
// aggressive hedge to keep p99 tight. Two upstreams, both healthy.
func BenchmarkPerf_Defi_RealtimeRead(b *testing.B) {
	mocks := []*benchMockUpstream{
		newBenchUpstream(benchMethod, nil),
		newBenchUpstream(benchMethod, nil),
	}
	bn := setupBenchNetwork(b, []*common.FailsafeConfig{{
		Timeout: &common.TimeoutPolicyConfig{Duration: common.NewStaticDurationSpec(2 * time.Second)},
		Retry:   &common.RetryPolicyConfig{MaxAttempts: 2, Delay: common.Duration(50 * time.Millisecond)},
		Hedge: &common.HedgePolicyConfig{
			Delay:    common.NewStaticDurationSpec(50 * time.Millisecond),
			MaxCount: 1,
		},
	}}, mocks)
	defer bn.Close()
	runForwardBench(b, bn)
}

// 2) Archival / indexer historical workload: long lifecycle timeout,
// large retry budget with backoff, breaker per upstream so a sick
// archive node gets dropped. Hedging is disabled (write-amp on heavy
// queries doesn't pay). Two upstreams.
func BenchmarkPerf_Indexer_ArchivalRead(b *testing.B) {
	mocks := []*benchMockUpstream{
		newBenchUpstream(benchMethod, nil),
		newBenchUpstream(benchMethod, nil),
	}
	bn := setupBenchNetwork(b, []*common.FailsafeConfig{{
		Timeout: &common.TimeoutPolicyConfig{Duration: common.NewStaticDurationSpec(60 * time.Second)},
		Retry: &common.RetryPolicyConfig{
			MaxAttempts:     5,
			Delay:           common.Duration(200 * time.Millisecond),
			BackoffMaxDelay: common.Duration(5 * time.Second),
			BackoffFactor:   2,
			Jitter:          common.Duration(100 * time.Millisecond),
		},
	}}, mocks)
	defer bn.Close()
	runForwardBench(b, bn)
}

// 3) High-trust consensus 3-of-5: retry around consensus slots so a
// participant rolling to a new upstream still finishes, lifecycle
// timeout caps total wall-clock. No hedge (consensus already fans
// out). Five healthy upstreams.
func BenchmarkPerf_Consensus_3of5_WithRetry(b *testing.B) {
	mocks := make([]*benchMockUpstream, 5)
	for i := range mocks {
		mocks[i] = newBenchUpstream(benchMethod, nil)
	}
	bn := setupBenchNetwork(b, []*common.FailsafeConfig{{
		Timeout: &common.TimeoutPolicyConfig{Duration: common.NewStaticDurationSpec(10 * time.Second)},
		Retry:   &common.RetryPolicyConfig{MaxAttempts: 2, Delay: common.Duration(0)},
		Consensus: &common.ConsensusPolicyConfig{
			MaxParticipants:    5,
			AgreementThreshold: 3,
		},
	}}, mocks)
	defer bn.Close()
	runForwardBench(b, bn)
}

// 4) Tail-latency-capped consensus: same 3-of-5 as above, but one
// participant is consistently 200ms slow. With `maxWaitOnResult: 50ms`
// the analyzer resolves once any 3 agree, capping p99 well below the
// straggler's latency. Demonstrates the new wait-cap feature against
// realistic upstream-skew conditions.
func BenchmarkPerf_Consensus_TailLatencyCapped(b *testing.B) {
	mocks := []*benchMockUpstream{
		newBenchUpstream(benchMethod, nil), // fast
		newBenchUpstream(benchMethod, nil), // fast
		newBenchUpstream(benchMethod, nil), // fast
		newBenchUpstream(benchMethod, nil), // fast
		newBenchUpstream(benchMethod, func(_ int64) (int, string, time.Duration) {
			return 0, "", 200 * time.Millisecond // straggler
		}),
	}
	bn := setupBenchNetwork(b, []*common.FailsafeConfig{{
		Timeout: &common.TimeoutPolicyConfig{Duration: common.NewStaticDurationSpec(10 * time.Second)},
		Consensus: &common.ConsensusPolicyConfig{
			MaxParticipants:    5,
			AgreementThreshold: 3,
			MaxWaitOnResult:    common.NewStaticDurationSpec(50 * time.Millisecond),
			MaxWaitOnEmpty:     common.NewStaticDurationSpec(2 * time.Second),
		},
	}}, mocks)
	defer bn.Close()
	runForwardBench(b, bn)
}

// 5) Write broadcast (eth_sendRawTransaction-style): fan-out to as
// many upstreams as possible, return on first success, leave the rest
// running in the background. Timeout per-attempt protects against any
// single upstream hanging the response.
func BenchmarkPerf_Write_BroadcastFireAndForget(b *testing.B) {
	mocks := make([]*benchMockUpstream, 5)
	for i := range mocks {
		mocks[i] = newBenchUpstream(benchMethod, nil)
	}
	bn := setupBenchNetwork(b, []*common.FailsafeConfig{{
		Timeout: &common.TimeoutPolicyConfig{Duration: common.NewStaticDurationSpec(5 * time.Second)},
		Consensus: &common.ConsensusPolicyConfig{
			MaxParticipants:    5,
			AgreementThreshold: 1,
			FireAndForget:      true,
		},
	}}, mocks)
	defer bn.Close()
	runForwardBench(b, bn)
}

// 6) Realistic failover: primary upstream returns 5xx ~30% of the
// time (chronically unhealthy), secondary is healthy. Retry + hedge
// + timeout work together to mask the bad upstream — the request
// SHOULD succeed every time. Measures the cost of paying for that
// resilience.
func BenchmarkPerf_Failover_UnreliablePrimary(b *testing.B) {
	mocks := []*benchMockUpstream{
		newBenchUpstream(benchMethod, func(_ int64) (int, string, time.Duration) {
			// nolint:gosec // not crypto
			if rand.Float64() < 0.3 {
				return 500, "", 0
			}
			return 0, "", 0
		}),
		newBenchUpstream(benchMethod, nil),
	}
	bn := setupBenchNetwork(b, []*common.FailsafeConfig{{
		Timeout: &common.TimeoutPolicyConfig{Duration: common.NewStaticDurationSpec(3 * time.Second)},
		Retry:   &common.RetryPolicyConfig{MaxAttempts: 3, Delay: common.Duration(20 * time.Millisecond)},
		Hedge: &common.HedgePolicyConfig{
			Delay:    common.NewStaticDurationSpec(100 * time.Millisecond),
			MaxCount: 1,
		},
	}}, mocks)
	defer bn.Close()
	runForwardBench(b, bn)
}

// 7) Memory snapshot under the high-trust consensus-with-retry combo
// (the heaviest realistic mix). Reports per-request heap delta +
// malloc count on top of the usual ns/op + B/op.
func BenchmarkPerf_Memory_ConsensusFullStack(b *testing.B) {
	mocks := make([]*benchMockUpstream, 5)
	for i := range mocks {
		mocks[i] = newBenchUpstream(benchMethod, nil)
	}
	bn := setupBenchNetwork(b, []*common.FailsafeConfig{{
		Timeout: &common.TimeoutPolicyConfig{Duration: common.NewStaticDurationSpec(10 * time.Second)},
		Retry:   &common.RetryPolicyConfig{MaxAttempts: 2, Delay: common.Duration(0)},
		Consensus: &common.ConsensusPolicyConfig{
			MaxParticipants:    5,
			AgreementThreshold: 3,
			MaxWaitOnResult:    common.NewStaticDurationSpec(100 * time.Millisecond),
		},
	}}, mocks)
	defer bn.Close()

	const reqs = 500
	body := benchRequestBody()

	// Warm-up to amortize lazy-init allocs (poller bookkeeping, etc.).
	for i := 0; i < 50; i++ {
		req := common.NewNormalizedRequest(body)
		resp, _ := bn.ntw.Forward(context.Background(), req)
		if resp != nil {
			resp.Release()
		}
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for j := 0; j < reqs; j++ {
			req := common.NewNormalizedRequest(body)
			resp, _ := bn.ntw.Forward(context.Background(), req)
			if resp != nil {
				resp.Release()
			}
		}
	}
	b.StopTimer()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	mallocs := after.Mallocs - before.Mallocs
	heapBytes := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	b.ReportMetric(float64(mallocs)/float64(int64(b.N)*reqs), "mallocs/req")
	b.ReportMetric(float64(heapBytes)/float64(int64(b.N)*reqs), "heap-delta-B/req")
}

// guard unused-import / strings reference for portability
var _ = strings.Split
var _ = rand.Int63
