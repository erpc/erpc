// Network-level integration tests for the production-hardened default
// selection policy. Each test reproduces a common operational failure
// class and asserts that the default policy contains it.
//
// "Real objects everywhere except HTTP": every test builds a real
// `*Network` with a real `*UpstreamsRegistry`, real `*health.Tracker`,
// real `*policy.Engine`, real `internal/policy/default_policy.js`. Only
// the upstream HTTP endpoints are mocked via gock. Tracker metrics are
// seeded through the tracker's PUBLIC Record* API (the same API real
// HTTP traffic would call), so the routing decision is made by the real
// policy against real-shaped data.
//
// Test pattern:
//   1. Build the network (frozen ticker; tests drive ticks).
//   2. Seed metrics on the tracker to simulate the observed prod state
//      (slow upstream / erroring upstream / lagging / throttled / etc.).
//   3. Force one policy tick — the engine reads metrics, runs the
//      default policy chain, atomically swaps the cached upstream order.
//   4. Set up gock mocks for the upstreams.
//   5. Send one real request through `network.Forward(ctx, req)`.
//   6. Assert which mocks were consumed (gock.Pending() bookkeeping).
package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	policystdlib "github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ──────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────

// setupSelectionPolicyNetwork builds a real Network with the production-
// hardened default selection policy. Unlike `setupTestNetworkWithScoring`
// it does NOT pin the upstream order — the engine actually drives routing.
// The ticker is frozen (EvalInterval=0); tests force ticks via
// `policy.TickForTest`.
func setupSelectionPolicyNetwork(t *testing.T, ctx context.Context, upstreamConfigs []*common.UpstreamConfig) *Network {
	t.Helper()

	setupGockObserver(t)
	// State-poller mocks per upstream host. `util.SetupMocksForEvmStatePoller`
	// only covers rpc1..rpc5.localhost; our tests use semantic IDs.
	for _, c := range upstreamConfigs {
		mockStatePollerForHost(c.Endpoint)
		// Opt every test upstream OUT of probe traffic by default —
		// the focused selection-policy tests in this file assert
		// "excluded = zero gock HTTP hits", which would be falsified
		// by the default policy's `probeExcluded` shadow-mirroring.
		// Tests that explicitly exercise probe traffic build their
		// own configs.
		if c.Routing == nil {
			c.Routing = &common.UpstreamRoutingConfig{}
		}
		if c.Routing.Probe == "" {
			c.Routing.Probe = common.ProbeModeOff
		}
	}

	rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)

	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)

	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx,
		&log.Logger,
		"test",
		upstreamConfigs,
		ssr,
		rateLimitersRegistry,
		vr,
		pr,
		nil,
		metricsTracker,
		nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		SelectionPolicy: &common.SelectionPolicyConfig{
			EvalInterval: 0, // frozen — tests drive ticks
		},
	}

	policyEngine := policy.NewEngine(ctx, &log.Logger, "test", metricsTracker, policystdlib.Install, nil)
	network, err := NewNetwork(
		ctx,
		&log.Logger,
		"test",
		networkConfig,
		rateLimitersRegistry,
		upstreamsRegistry,
		metricsTracker,
		policyEngine,
	)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))

	require.NoError(t, network.Bootstrap(ctx))

	// State pollers — give each upstream a known tip so block-head-lag is
	// 0 by default. Tests that exercise lag will set `BlockHeadLag.Store`
	// directly on the tracker after this point.
	upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	for _, ups := range upsList {
		require.NoError(t, ups.Bootstrap(ctx))
		ups.EvmStatePoller().SuggestLatestBlock(1000)
		ups.EvmStatePoller().SuggestFinalizedBlock(900)
	}
	expectedCount := len(upstreamConfigs)
	// Wait until the registry returns every configured upstream — the
	// per-upstream Bootstrap above can complete asynchronously and the
	// helper's caller relies on the policy snapshot seeing the full
	// set. Without this gate, the re-tick below would race the registry
	// and record a partial `previousOrder` that stickyPrimary later
	// keys off (producing test-order-sensitive failures).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
		if len(got) >= expectedCount {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Len(t, upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123)),
		expectedCount, "registry never converged on the full upstream set")

	// The synchronous initial tick fired inside RegisterNetwork ran
	// BEFORE the per-upstream bootstraps above completed — it might
	// have seen a partial slice and recorded a stale `previousOrder`.
	// Reset cross-tick state, then re-tick against the complete
	// upstream set so the slot's baseline matches reality before the
	// test seeds metrics.
	// Wipe per-upstream tracker metrics that the state poller seeded
	// during bootstrap. Without this the policy enters the test with
	// uneven request counts + latencies per upstream — scores diverge
	// by a tiny margin, the lower-scoring upstream wins by score
	// instead of by alphabetical tiebreak, and stickyPrimary locks
	// that pick in for the rest of the test. Per-test seedDegraded
	// will then add the metrics the test cares about against a clean
	// baseline.
	for _, ups := range upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123)) {
		if m := network.metricsTracker.GetUpstreamMethodMetrics(ups, "*", common.DataFinalityStateAll); m != nil {
			m.Reset()
		}
	}
	policy.ResetSlotStateForTest(network.policyEngine, network.networkId, "*")
	policy.TickForTest(network.policyEngine, network.networkId, "*")

	// Reset the gock hit counter so test assertions count only the
	// post-setup request, not the state-poller traffic generated above.
	resetGockHits()
	return network
}

// upstreamByID returns the registered upstream with the given ID.
// Panics if not found — tests should know what IDs they registered.
func upstreamByID(t *testing.T, network *Network, id string) common.Upstream {
	t.Helper()
	for _, u := range network.upstreamsRegistry.GetNetworkUpstreams(context.Background(), network.networkId) {
		if u.Id() == id {
			return u
		}
	}
	t.Fatalf("upstream %q not registered", id)
	return nil
}

// seedDegraded drives the tracker as if `n` requests had been observed
// against `ups` with the given mix of outcomes. It uses the tracker's
// PUBLIC Record* methods — the same path real HTTP traffic would take.
// The goal is to put the tracker in the state a real prod incident would
// have produced, fast and deterministically.
type seedSpec struct {
	method       string // metric method bucket; defaults to "eth_call" — see helper
	successful   int    // count of successful requests
	successAvgMs int    // duration in ms per successful request
	failed       int    // count of failed requests
	throttled    int    // count of throttled (429) responses
	blockHeadLag int64  // direct block-head-lag value (atomic store)
	misbehaviors int    // count of misbehaviors recorded
}

func seedDegraded(tracker *health.Tracker, ups common.Upstream, s seedSpec) {
	method := s.method
	if method == "" {
		// Seed under a real method (not "*") so the per-method
		// tracker buckets get populated — latencyDeviationAbove
		// reads u.metricsByMethod which excludes the "*" aggregate.
		// getUpsKeys fan-out propagates the write to the wildcard
		// aggregate too, so u.metrics (slot reads "*") still sees
		// the data.
		method = "eth_call"
	}

	for i := 0; i < s.successful; i++ {
		tracker.RecordUpstreamRequest(ups, method, common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(
			ups, method,
			time.Duration(s.successAvgMs)*time.Millisecond,
			true, "none", common.DataFinalityStateUnknown, "n/a",
		)
	}
	for i := 0; i < s.failed; i++ {
		tracker.RecordUpstreamRequest(ups, method, common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(ups, method, common.DataFinalityStateUnknown, fmt.Errorf("seed: synth failure"))
	}
	for i := 0; i < s.throttled; i++ {
		tracker.RecordUpstreamRequest(ups, method, common.DataFinalityStateUnknown)
		// RecordUpstreamFailure with CapacityExceeded is INTENTIONALLY a
		// no-op (see RecordUpstreamFailure's early-return list — "429
		// already penalized via ThrottledRate"). Driving the throttle
		// counter requires the actual RemoteRateLimited path, which is
		// what the live upstream layer uses when it sees a 429. Without
		// this, ThrottledRate stays at zero and the policy's
		// throttleRateAbove(0.3) predicate doesn't trip — making
		// TestNetworkPolicy_ThrottledUpstream_Excluded coin-flip on the
		// alphabetical sortByScore tiebreak instead of deterministically
		// excluding the throttled upstream.
		tracker.RecordUpstreamRemoteRateLimited(context.Background(), ups, method, nil)
	}
	for i := 0; i < s.misbehaviors; i++ {
		tracker.RecordUpstreamMisbehavior(ups, method, common.DataFinalityStateUnknown)
	}
	if s.blockHeadLag > 0 {
		m := tracker.GetUpstreamMethodMetrics(ups, method, common.DataFinalityStateAll)
		require.NotNilf(nilTBOnPanic{}, m, "tracker has no metrics for %s/%s yet", ups.Id(), method)
		m.BlockHeadLag.Store(s.blockHeadLag)
	}
}

// nilTBOnPanic is a tiny shim so seedDegraded can require.NotNil outside
// of a *testing.T context. We never expect it to fire — the only way for
// GetUpstreamMethodMetrics to return nil is if no Record* was called
// first, but seedDegraded always records at least one request before
// touching block-head-lag in the test flows that use it.
type nilTBOnPanic struct{}

func (nilTBOnPanic) Errorf(format string, args ...interface{}) {
	panic(fmt.Sprintf(format, args...))
}
func (nilTBOnPanic) FailNow() { panic("FailNow from seedDegraded shim") }

// upstreamHostFromID maps a test upstream id ("fast", "slow", "rpc1", …)
// to a deterministic mock host. Mirrors the convention in the other
// network tests: id=foo → http://foo.localhost.
func upstreamHostFromID(id string) string { return "http://" + id + ".localhost" }

// mockStatePollerForHost stages the 4 mocks the EVM state poller needs
// during upstream Bootstrap (`eth_chainId`, `eth_getBlockByNumber latest`,
// `eth_getBlockByNumber finalized`, `eth_syncing`). `util.SetupMocksForEvmStatePoller`
// only covers the canonical `rpc1.localhost`…`rpc5.localhost` hosts; our
// scenario tests use semantic IDs (`fast`, `slow`, `lagger`, …) so we
// register the same mocks per-host here.
//
// `chainId` is the network's chain ID as hex (e.g. "0x7b" for 123).
// All other returned blocks/state are dummy values — the test cares
// about routing decisions, not block contents.
func mockStatePollerForHost(host string) {
	chainId := "0x7b" // chainId 123 — matches setupSelectionPolicyNetwork's networkConfig
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_chainId") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + chainId + `"}`))
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "latest")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x3e8","timestamp":"0x6702a8f0"}}`))
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "finalized")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x384","timestamp":"0x6702a8e0"}}`))
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_syncing")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":false}`))
}

// mockClean stages a gock response that succeeds quickly. The matcher
// is keyed on the test's method so multiple parallel mocks don't
// collide (or `Persist` so one mock serves many requests).
func mockClean(t *testing.T, host, method, result string) {
	t.Helper()
	gock.New(host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), method)
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": result})
}

// gockCalls reports how many times a mock for the given host has been
// consumed since the last ResetGock. We do this by looking at which
// mocks have a non-zero hit count — gock's API exposes this via
// `Mock.Request().Counter` but it's awkward; counting "Pending" is
// simpler for our purposes.
//
// We use a different approach: register Persist() mocks for all hosts
// up front, then count which ones were hit by inspecting which
// requests were observed in gock's interceptor stats. Since gock's
// public surface for "was this host called?" is limited, we instead
// add an `Observer` to capture call counts.
func gockHits(host string) int {
	// gock doesn't expose per-mock hit counts cleanly; we track hits
	// via the package-level `hits` map populated by an HTTP-level
	// observer set up in setupGockObserver.
	hitsMu.Lock()
	defer hitsMu.Unlock()
	return hits[host]
}

var (
	hits   = map[string]int{}
	hitsMu = sync.Mutex{}
)

// setupGockObserver installs a hit counter that captures every gock
// match by host. Call once per test (after ResetGock).
//
// Counts include EVERY gock-matched request — including the
// state-poller traffic that fires during upstream Bootstrap. Tests
// that want to count only the post-setup request should call
// `resetGockHits()` after the helper returns and before the
// assertion-relevant Forward.
func setupGockObserver(t *testing.T) {
	t.Helper()
	hitsMu.Lock()
	hits = map[string]int{}
	hitsMu.Unlock()
	gock.Observe(func(req *http.Request, mock gock.Mock) {
		if req == nil || req.URL == nil {
			return
		}
		host := "http://" + req.URL.Host
		hitsMu.Lock()
		hits[host]++
		hitsMu.Unlock()
	})
}

// resetGockHits zeroes the per-host hit counter. Tests call this
// AFTER setup (which generates state-poller traffic) and BEFORE the
// assertion-relevant Forward, so the assertions count only the
// network's routing decision for the actual test request.
func resetGockHits() {
	hitsMu.Lock()
	hits = map[string]int{}
	hitsMu.Unlock()
}

// ──────────────────────────────────────────────────────────────────────
// Scenarios
// ──────────────────────────────────────────────────────────────────────

// TestNetworkPolicy_SlowUpstream_Excluded reproduces the "slow vendor
// drags pool p95" class of incident. A single upstream consistently
// answering at ~12s pulls the pool's blended latency percentile down
// (poisoning hedge / consensus timeouts) and — without filtering — wins
// individual requests until its score finally crosses a ranking
// threshold. The hardened default's `keepHealthy(maxP95Ms: 10_000)`
// excludes it BEFORE scoring.
func TestNetworkPolicy_SlowUpstream_Excluded(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupSelectionPolicyNetwork(t, ctx, []*common.UpstreamConfig{
		{Type: common.UpstreamTypeEvm, Id: "fast", Endpoint: upstreamHostFromID("fast"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "slow", Endpoint: upstreamHostFromID("slow"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	})

	// Simulate prod-observed state: "slow" has served 100 requests at
	// 12s p95 latency; "fast" has served 100 at 10ms.
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "fast"), seedSpec{
		successful: 100, successAvgMs: 10,
	})
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "slow"), seedSpec{
		successful: 100, successAvgMs: 12_000,
	})

	policy.TickForTest(network.policyEngine, network.networkId, "*")

	// Mock both upstreams — the test asserts on which one was hit.
	mockClean(t, upstreamHostFromID("fast"), "eth_getBalance", "0x1111")
	mockClean(t, upstreamHostFromID("slow"), "eth_getBalance", "0x2222")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.GreaterOrEqual(t, gockHits(upstreamHostFromID("fast")), 1, "fast must serve the request")
	assert.Equal(t, 0, gockHits(upstreamHostFromID("slow")),
		"slow (p95=12s) must be excluded by keepHealthy(maxP95Ms:10_000); never gets HTTP")
}

// TestNetworkPolicy_ErroringUpstream_Excluded — the most common
// observed pattern: one upstream starts erroring fundamentally (>70%
// error rate over the window) and the policy should reroute. Without
// filtering it would still pull traffic on every retry. With the
// hardened default's `excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))`,
// it's out.
func TestNetworkPolicy_ErroringUpstream_Excluded(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupSelectionPolicyNetwork(t, ctx, []*common.UpstreamConfig{
		{Type: common.UpstreamTypeEvm, Id: "good", Endpoint: upstreamHostFromID("good"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "errs", Endpoint: upstreamHostFromID("errs"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	})

	// "errs": 80% error rate (80 failures + 20 successful) — clearly
	// past the default's 0.7 threshold, with 100 samples to clear
	// samplesAbove(10).
	// "good": 100 clean successes.
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "errs"), seedSpec{
		successful: 20, successAvgMs: 10, failed: 80,
	})
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "good"), seedSpec{
		successful: 100, successAvgMs: 10,
	})

	policy.TickForTest(network.policyEngine, network.networkId, "*")

	mockClean(t, upstreamHostFromID("good"), "eth_getBalance", "0xgood")
	mockClean(t, upstreamHostFromID("errs"), "eth_getBalance", "0xerrs")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.GreaterOrEqual(t, gockHits(upstreamHostFromID("good")), 1, "good must serve the request")
	assert.Equal(t, 0, gockHits(upstreamHostFromID("errs")),
		"errs (errorRate=0.8) must be excluded by excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))")
}

// TestNetworkPolicy_LaggingUpstream_Excluded — reproduces the
// "lagging-but-error-free upstream stayed in rotation" class. The
// upstream returns 200s for whatever data it has, but its head is
// 30 blocks behind tip. Without lag-filtering the policy only ranks
// it lower; with the hardened default's `keepHealthy(maxBlockHeadLag: 16)`
// it's filtered out entirely.
func TestNetworkPolicy_LaggingUpstream_Excluded(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupSelectionPolicyNetwork(t, ctx, []*common.UpstreamConfig{
		{Type: common.UpstreamTypeEvm, Id: "tip", Endpoint: upstreamHostFromID("tip"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "lagger", Endpoint: upstreamHostFromID("lagger"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	})

	// Both serve 100 clean requests at 10ms. Then we directly set
	// lagger's blockHeadLag to 30 (clearly past the maxBlockHeadLag:16
	// threshold). This is the same mechanism the state-poller would
	// produce when an upstream's head trails network tip — the metric
	// store is updated atomically without going through Record*.
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "tip"), seedSpec{
		successful: 100, successAvgMs: 10,
	})
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "lagger"), seedSpec{
		successful: 100, successAvgMs: 10, blockHeadLag: 30,
	})

	policy.TickForTest(network.policyEngine, network.networkId, "*")

	mockClean(t, upstreamHostFromID("tip"), "eth_getBlockByNumber", `{"number":"0x3e8"}`)
	mockClean(t, upstreamHostFromID("lagger"), "eth_getBlockByNumber", `{"number":"0x3d2"}`)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.GreaterOrEqual(t, gockHits(upstreamHostFromID("tip")), 1, "tip must serve")
	assert.Equal(t, 0, gockHits(upstreamHostFromID("lagger")),
		"lagger (blockHeadLag=30) must be excluded by keepHealthy(maxBlockHeadLag:16)")
}

// TestNetworkPolicy_ThrottledUpstream_Excluded — vendor is
// rate-limiting us (429s consistently). The policy should reroute
// before quota burns. Hardened default's
// `excludeIf(all(samplesAbove(10), throttleRateAbove(0.4)))` covers this.
//
// Also defends against the rate-limit cascade pattern: lag on one
// vendor shifts traffic to another, which then rate-limits because
// it isn't sized for full pool traffic. Filtering by throttle rate
// prevents the throttled vendor from being selected at all.
func TestNetworkPolicy_ThrottledUpstream_Excluded(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupSelectionPolicyNetwork(t, ctx, []*common.UpstreamConfig{
		{Type: common.UpstreamTypeEvm, Id: "open", Endpoint: upstreamHostFromID("open"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "quota", Endpoint: upstreamHostFromID("quota"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	})

	// "quota": 50% throttled (50 throttled + 50 successful out of 100).
	// "open": clean.
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "open"), seedSpec{
		successful: 100, successAvgMs: 10,
	})
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "quota"), seedSpec{
		successful: 50, successAvgMs: 10, throttled: 50,
	})

	policy.TickForTest(network.policyEngine, network.networkId, "*")

	mockClean(t, upstreamHostFromID("open"), "eth_getBalance", "0xopen")
	mockClean(t, upstreamHostFromID("quota"), "eth_getBalance", "0xquota")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.GreaterOrEqual(t, gockHits(upstreamHostFromID("open")), 1, "open must serve")
	assert.Equal(t, 0, gockHits(upstreamHostFromID("quota")),
		"quota (throttledRate=0.5) must be excluded by excludeIf(all(samplesAbove(10), throttleRateAbove(0.4)))")
}

// TestNetworkPolicy_MixedDegraded_OnlyHealthyServed reproduces a real
// mixed-incident state: 3 upstreams, one degraded in each of three
// different ways (slow, erroring, lagging). The healthy fourth is the
// only one that should receive traffic.
func TestNetworkPolicy_MixedDegraded_OnlyHealthyServed(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupSelectionPolicyNetwork(t, ctx, []*common.UpstreamConfig{
		{Type: common.UpstreamTypeEvm, Id: "healthy", Endpoint: upstreamHostFromID("healthy"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "slow", Endpoint: upstreamHostFromID("slow"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "errs", Endpoint: upstreamHostFromID("errs"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "lagger", Endpoint: upstreamHostFromID("lagger"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	})

	seedDegraded(network.metricsTracker, upstreamByID(t, network, "healthy"), seedSpec{successful: 100, successAvgMs: 10})
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "slow"), seedSpec{successful: 100, successAvgMs: 15_000})
	// errs: 85% errors (15 successful + 85 failed) — clearly past 0.7
	// default threshold with 100 samples to clear samplesAbove(10).
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "errs"), seedSpec{successful: 15, successAvgMs: 10, failed: 85})
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "lagger"), seedSpec{successful: 100, successAvgMs: 10, blockHeadLag: 25})

	policy.TickForTest(network.policyEngine, network.networkId, "*")

	mockClean(t, upstreamHostFromID("healthy"), "eth_getBalance", "0xhealthy")
	mockClean(t, upstreamHostFromID("slow"), "eth_getBalance", "0xslow")
	mockClean(t, upstreamHostFromID("errs"), "eth_getBalance", "0xerrs")
	mockClean(t, upstreamHostFromID("lagger"), "eth_getBalance", "0xlagger")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.GreaterOrEqual(t, gockHits(upstreamHostFromID("healthy")), 1, "healthy must serve")
	for _, bad := range []string{"slow", "errs", "lagger"} {
		assert.Equal(t, 0, gockHits(upstreamHostFromID(bad)),
			"%s must be excluded by keepHealthy", bad)
	}
}

// TestNetworkPolicy_FallbackTier_ActivatesWhenPrimaryAllBroken
// reproduces the case where both primary upstreams are degraded and
// the secondary tier (tag: tier:fallback) has to take over. The
// hardened default does `keepHealthy → whenEmpty(() => upstreams) →
// preferTag('!tier:fallback', {fallback:'tier:fallback'})`. When
// primaries fail keepHealthy AND the safety net puts them back AND
// preferTag sees no healthy primaries, it switches to the fallback tier.
func TestNetworkPolicy_FallbackTier_ActivatesWhenPrimaryAllBroken(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupSelectionPolicyNetwork(t, ctx, []*common.UpstreamConfig{
		{Type: common.UpstreamTypeEvm, Id: "primA", Endpoint: upstreamHostFromID("primA"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "primB", Endpoint: upstreamHostFromID("primB"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "backup", Tags: []string{"tier:fallback"}, Endpoint: upstreamHostFromID("backup"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	})

	// Both primaries broken; backup is healthy.
	for _, id := range []string{"primA", "primB"} {
		seedDegraded(network.metricsTracker, upstreamByID(t, network, id),
			seedSpec{successful: 20, successAvgMs: 10, failed: 80})
	}
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "backup"),
		seedSpec{successful: 100, successAvgMs: 10})

	policy.TickForTest(network.policyEngine, network.networkId, "*")

	mockClean(t, upstreamHostFromID("primA"), "eth_getBalance", "0xA")
	mockClean(t, upstreamHostFromID("primB"), "eth_getBalance", "0xB")
	mockClean(t, upstreamHostFromID("backup"), "eth_getBalance", "0xbackup")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.GreaterOrEqual(t, gockHits(upstreamHostFromID("backup")), 1,
		"backup (tier:fallback) must take over when primary tier is empty")
	assert.Equal(t, 0, gockHits(upstreamHostFromID("primA")),
		"primA filtered by keepHealthy; preferTag picks fallback")
	assert.Equal(t, 0, gockHits(upstreamHostFromID("primB")),
		"primB filtered; preferTag picks fallback")
}

// TestNetworkPolicy_SafetyNet_WhenAllBroken — every upstream is
// degraded and there's no fallback group. Step 2 of the default
// (keepHealthy) empties the list; step 3 (whenEmpty → upstreams)
// restores the raw set so the network attempts SOMETHING rather than
// failing closed. Better a flaky response than no response.
func TestNetworkPolicy_SafetyNet_WhenAllBroken(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupSelectionPolicyNetwork(t, ctx, []*common.UpstreamConfig{
		{Type: common.UpstreamTypeEvm, Id: "broken1", Endpoint: upstreamHostFromID("broken1"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "broken2", Endpoint: upstreamHostFromID("broken2"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	})

	// Both broken: 90% errors.
	for _, id := range []string{"broken1", "broken2"} {
		seedDegraded(network.metricsTracker, upstreamByID(t, network, id),
			seedSpec{successful: 10, successAvgMs: 10, failed: 90})
	}

	policy.TickForTest(network.policyEngine, network.networkId, "*")

	mockClean(t, upstreamHostFromID("broken1"), "eth_getBalance", "0xbroken1")
	mockClean(t, upstreamHostFromID("broken2"), "eth_getBalance", "0xbroken2")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp,
		"safety net: when every upstream fails the filter, return the raw set rather than fail closed")

	total := gockHits(upstreamHostFromID("broken1")) + gockHits(upstreamHostFromID("broken2"))
	assert.GreaterOrEqual(t, total, 1, "at least one upstream must have been attempted")
}

// TestNetworkPolicy_StickyPrimary_AllFinalities — the hardened
// default's stickyPrimary holds across the minSwitchInterval cooldown
// for every finality (realtime / unfinalized / finalized). Flapping
// between similarly-ranked upstreams every tick costs more in
// connection-setup + cache-locality than the marginal ranking gain,
// regardless of whether the request is reorg-tolerant.
//
// We exercise the JS branching by forcing ctx.finality via
// `policy.SetFinalityForTest`, which bypasses request-side finality
// classification but still goes through the real policy engine.
func TestNetworkPolicy_StickyPrimary_AllFinalities(t *testing.T) {
	for _, finality := range []string{"realtime", "unfinalized", "finalized"} {
		t.Run(finality+"_holds_prior_primary", func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()
			setupGockObserver(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			network := setupSelectionPolicyNetwork(t, ctx, []*common.UpstreamConfig{
				{Type: common.UpstreamTypeEvm, Id: "rpcA", Endpoint: upstreamHostFromID("rpcA"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
				{Type: common.UpstreamTypeEvm, Id: "rpcB", Endpoint: upstreamHostFromID("rpcB"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
			})

			policy.SetFinalityForTest(network.policyEngine, network.networkId, "*", finality)

			// Tick 1 — equal metrics; whichever upstream wins by score
			// tiebreak becomes the incumbent. The engine records
			// lastSwitchAt here, starting the sticky cooldown.
			for _, id := range []string{"rpcA", "rpcB"} {
				seedDegraded(network.metricsTracker, upstreamByID(t, network, id),
					seedSpec{successful: 100, successAvgMs: 10})
			}
			policy.TickForTest(network.policyEngine, network.networkId, "*")

			order := network.policyEngine.GetOrdered(network.networkId, "*", "*")
			require.Len(t, order, 2)
			incumbent := order[0].Id()
			challenger := order[1].Id()

			// Degrade the incumbent (errorRate 0.23 — well below the 0.7
			// default ceiling so it still passes excludeIf) so score
			// favours the challenger. The 30s sticky cooldown should hold
			// the incumbent regardless of finality.
			seedDegraded(network.metricsTracker, upstreamByID(t, network, incumbent),
				seedSpec{failed: 30})
			policy.TickForTest(network.policyEngine, network.networkId, "*")

			mockClean(t, upstreamHostFromID("rpcA"), "eth_getBalance", "0xA")
			mockClean(t, upstreamHostFromID("rpcB"), "eth_getBalance", "0xB")

			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err)
			require.NotNil(t, resp)

			assert.GreaterOrEqual(t, gockHits(upstreamHostFromID(incumbent)), 1,
				"%s must remain primary under finality=%s (incumbent=%s, challenger=%s)",
				incumbent, finality, incumbent, challenger)
		})
	}
}

// TestNetworkPolicy_NoTimeBasedReadmit_OnlyMetricsHeal — with the
// new probe-driven re-admission model, an excluded upstream STAYS
// excluded forever regardless of elapsed time, until its tracker
// counters fall back below the excludeIf threshold. Verifies that:
//   1. A degraded upstream is excluded on the first tick.
//   2. Even after arbitrary virtual-time advancement, it stays excluded.
//   3. Once clean samples drag its rolling error rate below 0.7, the
//      next tick re-admits it.
//
// The probe subsystem itself feeds those clean samples in production
// (mirrored real traffic); this test simulates that by directly
// seeding the tracker, the same way a probe call would.
func TestNetworkPolicy_NoTimeBasedReadmit_OnlyMetricsHeal(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupSelectionPolicyNetwork(t, ctx, []*common.UpstreamConfig{
		{Type: common.UpstreamTypeEvm, Id: "alive", Endpoint: upstreamHostFromID("alive"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "dead", Endpoint: upstreamHostFromID("dead"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	})

	// "dead" is broken (90% error rate); "alive" is healthy.
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "alive"),
		seedSpec{successful: 100, successAvgMs: 10})
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "dead"),
		seedSpec{successful: 10, successAvgMs: 10, failed: 90})

	// Tick 1 — dead excluded.
	policy.TickForTest(network.policyEngine, network.networkId, "*")
	require.Equal(t, []string{"alive"},
		idsFromUpstreams(network.policyEngine.GetOrdered(network.networkId, "*", "*")))

	// Advance virtual time arbitrarily — dead MUST stay excluded since
	// no metric improvement has occurred. The new probeExcluded has no
	// time-based readmit.
	policy.AdvanceEvalNowForTest(network.policyEngine, network.networkId, "*", 10*time.Minute)
	policy.TickForTest(network.policyEngine, network.networkId, "*")
	require.Equal(t, []string{"alive"},
		idsFromUpstreams(network.policyEngine.GetOrdered(network.networkId, "*", "*")),
		"dead must still be excluded after arbitrary time — no time-based readmit")

	// Now simulate the prober feeding clean samples to "dead" — enough
	// to swamp its prior 90 failures and drag the rolling error rate
	// below the 0.7 threshold.
	seedDegraded(network.metricsTracker, upstreamByID(t, network, "dead"),
		seedSpec{successful: 2000, successAvgMs: 10})

	policy.TickForTest(network.policyEngine, network.networkId, "*")
	order := network.policyEngine.GetOrdered(network.networkId, "*", "*")
	assert.Contains(t, idsFromUpstreams(order), "dead",
		"once shadow-probe-fed samples healed dead's error rate, next tick must re-admit it")
	assert.Contains(t, idsFromUpstreams(order), "alive")
}

func idsFromUpstreams(ups []common.Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Id()
	}
	return out
}

// TestNetworkPolicy_SpreadAcrossTags_PreventsCohortCascade verifies
// the spreadAcrossTags primitive at network level. With 4 upstreams
// (3 sharing one cohort:* tag, 1 in another), the failover order
// should interleave cohorts rather than fill the top 3 positions with
// the shared-fate group — i.e. one cohort outage shouldn't take out
// the primary AND the first two fallbacks. The default policy doesn't
// include spreadAcrossTags; operators wanting this behaviour opt in
// via a custom eval.
func TestNetworkPolicy_SpreadAcrossTags_PreventsCohortCascade(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tagCohort1 := []string{"cohort:cohort-1"}
	tagCohort2 := []string{"cohort:cohort-2"}
	upstreamConfigs := []*common.UpstreamConfig{
		{Type: common.UpstreamTypeEvm, Id: "cohort1-A", Tags: tagCohort1, Endpoint: upstreamHostFromID("cohort1-A"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "cohort1-B", Tags: tagCohort1, Endpoint: upstreamHostFromID("cohort1-B"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "cohort1-C", Tags: tagCohort1, Endpoint: upstreamHostFromID("cohort1-C"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "cohort2-A", Tags: tagCohort2, Endpoint: upstreamHostFromID("cohort2-A"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	}
	network := setupSelectionPolicyNetworkWithEval(t, ctx, upstreamConfigs,
		`(upstreams, ctx) =>
			upstreams.sortByScore(PREFER_FASTEST).spreadAcrossTags('cohort:')`)

	// All clean.
	for _, u := range upstreamConfigs {
		seedDegraded(network.metricsTracker, upstreamByID(t, network, u.Id),
			seedSpec{successful: 100, successAvgMs: 10})
	}
	policy.TickForTest(network.policyEngine, network.networkId, "*")

	order := network.policyEngine.GetOrdered(network.networkId, "*", "*")
	require.Len(t, order, 4)

	// position[0] is cohort-1 best; position[1] MUST be cohort-2 (the
	// only non-cohort-1 upstream); positions[2,3] are the remaining
	// cohort-1s. Critical assertion: positions 0 and 1 must NOT share
	// a cohort.
	c0 := firstTagWithPrefix(order[0].Config().Tags, "cohort:")
	c1 := firstTagWithPrefix(order[1].Config().Tags, "cohort:")
	assert.NotEqual(t, c0, c1,
		"primary and first runner-up must be different cohorts (no cascade); got %s, %s",
		c0, c1)
	assert.Equal(t, "cohort:cohort-2", c1,
		"cohort:cohort-2 (the only one) must be position[1] under spreadAcrossTags('cohort:')")
}

// firstTagWithPrefix returns the first tag in `tags` whose value
// begins with `prefix`, or "" if none. Test-only helper used to
// inspect tag-encoded labels in network-level assertions.
func firstTagWithPrefix(tags []string, prefix string) string {
	for _, t := range tags {
		if len(t) >= len(prefix) && t[:len(prefix)] == prefix {
			return t
		}
	}
	return ""
}

// setupSelectionPolicyNetworkWithEval is a sibling of
// setupSelectionPolicyNetwork that lets the test override the
// selection-policy eval body. Only the cohort-cascade scenario uses
// this — the other tests verify the EMBEDDED default policy directly.
func setupSelectionPolicyNetworkWithEval(t *testing.T, ctx context.Context, upstreamConfigs []*common.UpstreamConfig, evalSrc string) *Network {
	t.Helper()

	setupGockObserver(t)
	for _, c := range upstreamConfigs {
		mockStatePollerForHost(c.Endpoint)
	}

	rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)

	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)

	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "test", upstreamConfigs, ssr, rateLimitersRegistry, vr, pr, nil, metricsTracker, nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		SelectionPolicy: &common.SelectionPolicyConfig{
			EvalInterval: 0,
			EvalFunc:     evalSrc,
		},
	}

	policyEngine := policy.NewEngine(ctx, &log.Logger, "test", metricsTracker, policystdlib.Install, nil)
	network, err := NewNetwork(ctx, &log.Logger, "test", networkConfig,
		rateLimitersRegistry, upstreamsRegistry, metricsTracker, policyEngine)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	for _, ups := range upsList {
		require.NoError(t, ups.Bootstrap(ctx))
		ups.EvmStatePoller().SuggestLatestBlock(1000)
		ups.EvmStatePoller().SuggestFinalizedBlock(900)
	}
	time.Sleep(50 * time.Millisecond)

	resetGockHits()
	return network
}
