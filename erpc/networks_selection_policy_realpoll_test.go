// Network-level integration tests that drive block-head-lag exclusion through
// the REAL state-poller → tracker → selection-policy path, with ONLY the
// upstream HTTP endpoints mocked (via gock).
//
// Why a separate file from networks_selection_policy_test.go:
//
//	The tests in that file seed lag with a DIRECT `BlockHeadLag.Store` (the
//	`seedSpec.blockHeadLag` knob). That writes the final value straight into
//	the bucket the policy reads — which means it cannot catch a bug in HOW the
//	lag travels from a poll into that bucket. Exactly such a bug shipped
//	(#907: with per-finality tracking on, the optimized lag-write only touched
//	the dedup-index key and left the `{method, All}` rollup the policy reads
//	starved, so `blockNumberLagAbove` silently no-oped in prod).
//
// These tests close that gap. Each one:
//   - enables per-finality tracking (the condition under which #907 manifests),
//   - seeds a little per-finality request traffic (so the dedup index holds a
//     specific-finality representative, reproducing the starvation setup),
//   - lets the REAL EvmStatePoller fetch each upstream's tip over gock-mocked
//     `eth_getBlockByNumber("latest")` — i.e. lag is computed by the real
//     poll → tracker.SetLatestBlockNumber → updateNetworkLagMetrics path,
//   - forces one policy tick and asserts the laggard is excluded end-to-end.
//
// Run against a pre-#907 binary, TestNetworkPolicy_RealPoll_LaggingUpstreamExcluded
// fails at the `{*, All}` lag assertion (lag never reaches the policy) — which is
// precisely the regression we want a network-level test to catch.
package erpc

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
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

// realPollFixture describes one upstream whose state-poller will report
// `latest` as its head tip via a gock-mocked eth_getBlockByNumber("latest").
type realPollFixture struct {
	id     string
	latest int64
}

// mockPollerHostWithLatest mocks one upstream host's state-poller RPCs with a
// caller-chosen latest/finalized height. The REAL poller fetches these over
// gock, so each upstream ends up with a genuine, distinct head tip — and thus
// a real block-head-lag — rather than the test injecting the lag directly.
func mockPollerHostWithLatest(host string, latest, finalized int64) {
	hx := func(n int64) string { return fmt.Sprintf("0x%x", n) }
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_chainId") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x7b"}`)) // chainId 123
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "latest")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"` + hx(latest) + `","timestamp":"0x6702a8f0"}}`))
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "finalized")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"` + hx(finalized) + `","timestamp":"0x6702a8e0"}}`))
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_syncing") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":false}`))
}

// setupRealPollLagNetwork builds the full real stack (UpstreamsRegistry,
// health.Tracker, policy.Engine + the embedded default policy) and establishes
// per-upstream block-head-lag through the REAL poll path. Only HTTP is mocked.
//
// The returned network has had one policy tick forced; its cached ordering
// reflects the lag-based decision.
func setupRealPollLagNetwork(t *testing.T, ctx context.Context, fixtures []realPollFixture) *Network {
	t.Helper()
	setupGockObserver(t)

	upstreamConfigs := make([]*common.UpstreamConfig, 0, len(fixtures))
	for _, f := range fixtures {
		host := upstreamHostFromID(f.id)
		finalized := f.latest - 10
		if finalized < 1 {
			finalized = 1
		}
		mockPollerHostWithLatest(host, f.latest, finalized)
		upstreamConfigs = append(upstreamConfigs, &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       f.id,
			Endpoint: host,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
				// No background polling — tests drive PollLatestBlockNumber
				// explicitly for determinism.
				StatePollerInterval: common.Duration(time.Hour),
				// Tiny (non-zero) debounce so each explicit re-poll actually
				// re-fetches instead of returning the cached value. A zero
				// debounce would fall through resolveDebounce to a ~block-time
				// / 1s floor and skip our rapid re-polls.
				StatePollerDebounce: common.Duration(time.Nanosecond),
			},
			// Keep excluded upstreams off probe traffic so HTTP-hit assertions
			// measure routing, not probeExcluded shadow-mirroring.
			Routing: &common.UpstreamRoutingConfig{Probe: common.ProbeModeOff},
		})
	}

	rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)

	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)
	// Per-finality tracking ON — this is the condition under which the #907
	// lag-rollup-starvation bug manifests, so the test exercises that path.
	metricsTracker.EnableFinalityTracking()

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)

	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "test", upstreamConfigs, ssr, rateLimitersRegistry, vr, pr, nil, metricsTracker, nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		// Empty EvalFunc + frozen ticker → Bootstrap installs the embedded
		// default policy (which contains blockNumberLagAbove(16)); tests drive
		// ticks via policy.TickForTest.
		SelectionPolicy: &common.SelectionPolicyConfig{EvalInterval: 0},
	}

	policyEngine := policy.NewEngine(ctx, &log.Logger, "test", metricsTracker, policystdlib.Install, nil)
	network, err := NewNetwork(ctx, &log.Logger, "test", networkConfig, rateLimitersRegistry, upstreamsRegistry, metricsTracker, policyEngine)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.Len(t, upsList, len(fixtures))
	for _, ups := range upsList {
		require.NoError(t, ups.Bootstrap(ctx))
	}

	// Seed a little per-finality request traffic so the (ups,method) dedup
	// index holds a SPECIFIC-finality representative key. Without this the
	// lag would have nowhere realistic to land and the #907 starvation path
	// wouldn't be exercised. Clean + fast so no error/latency predicate trips.
	for _, ups := range upsList {
		for i := 0; i < 12; i++ {
			metricsTracker.RecordUpstreamRequest(ups, "eth_getBalance", common.DataFinalityStateUnfinalized)
			metricsTracker.RecordUpstreamDuration(
				ups, "eth_getBalance", 10*time.Millisecond,
				true, "none", common.DataFinalityStateUnfinalized, "n/a",
			)
		}
	}

	// Drive the REAL poll, highest tip first so the network-level max settles
	// to the true head before laggards compute their lag against it. Two passes
	// so every upstream's lag is recomputed against the final max.
	sort.SliceStable(upsList, func(i, j int) bool {
		return latestForFixtureID(fixtures, upsList[i].Id()) > latestForFixtureID(fixtures, upsList[j].Id())
	})
	for pass := 0; pass < 2; pass++ {
		for _, ups := range upsList {
			_, perr := ups.EvmStatePoller().PollLatestBlockNumber(ctx)
			require.NoError(t, perr)
		}
	}

	policy.ResetSlotStateForTest(network.policyEngine, network.networkId, "*")
	policy.TickForTest(network.policyEngine, network.networkId, "*")

	resetGockHits() // count only post-setup request traffic
	return network
}

func latestForFixtureID(fixtures []realPollFixture, id string) int64 {
	for _, f := range fixtures {
		if f.id == id {
			return f.latest
		}
	}
	return 0
}

// blockHeadLagOf reads the {*, All} rollup the network-scope policy consumes.
func blockHeadLagOf(t *testing.T, network *Network, id string) int64 {
	t.Helper()
	u := upstreamByID(t, network, id)
	m := network.metricsTracker.GetUpstreamMethodMetrics(u, "*", common.DataFinalityStateAll)
	require.NotNil(t, m, "%s must have a {*, All} metrics bucket after polling", id)
	return m.BlockHeadLag.Load()
}

// TestNetworkPolicy_RealPoll_LaggingUpstreamExcluded is the end-to-end
// regression for the frozen-upstream incident: a healthy pool plus one node
// stuck far behind the head. With only HTTP mocked, the real poller derives
// the lag, the real default policy must exclude the laggard, and a real
// request must never reach it.
//
// This is the network-level guard that #907 was missing — a direct
// BlockHeadLag.Store would have masked the rollup bug.
func TestNetworkPolicy_RealPoll_LaggingUpstreamExcluded(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Two healthy nodes at the tip (1000) + one frozen 900 blocks behind (100).
	// 900 is far past the default policy's blockNumberLagAbove(16) threshold.
	network := setupRealPollLagNetwork(t, ctx, []realPollFixture{
		{id: "healthy-a", latest: 1000},
		{id: "healthy-b", latest: 1000},
		{id: "frozen", latest: 100},
	})

	// 1) The real poll → tracker rollup populated the {*, All} bucket the
	//    policy reads. This is the exact hop #907 fixed; pre-#907 this is 0.
	require.EqualValues(t, 900, blockHeadLagOf(t, network, "frozen"),
		"real poll must land lag on the {*, All} rollup (poll→SetLatestBlockNumber→updateNetworkLagMetrics)")
	require.EqualValues(t, 0, blockHeadLagOf(t, network, "healthy-a"))

	// 2) The default selection policy excluded the laggard.
	order, excluded := policy.LatestDecisionOutputForTest(network.policyEngine, network.networkId, "*")
	assert.Contains(t, excluded, "frozen",
		"frozen (lag 900 > blockNumberLagAbove(16)) must be excluded by the default policy")
	assert.NotContains(t, order, "frozen")
	assert.Contains(t, order, "healthy-a")
	assert.Contains(t, order, "healthy-b")

	// 3) End-to-end routing: a real request never reaches the excluded node.
	mockClean(t, upstreamHostFromID("healthy-a"), "eth_getBalance", "0xaaa")
	mockClean(t, upstreamHostFromID("healthy-b"), "eth_getBalance", "0xbbb")
	mockClean(t, upstreamHostFromID("frozen"), "eth_getBalance", "0xfff")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, 0, gockHits(upstreamHostFromID("frozen")),
		"frozen upstream must receive zero traffic — excluded by the lag predicate")
}

// TestNetworkPolicy_RealPoll_WithinTolerance_NoneExcluded guards the other
// direction: small, normal lag (a few blocks behind the tip) must NOT trip
// blockNumberLagAbove(16). Same real-poll path, no exclusion expected.
func TestNetworkPolicy_RealPoll_WithinTolerance_NoneExcluded(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupRealPollLagNetwork(t, ctx, []realPollFixture{
		{id: "u1", latest: 1000},
		{id: "u2", latest: 998}, // 2 behind
		{id: "u3", latest: 995}, // 5 behind — still within the 16-block tolerance
	})

	require.EqualValues(t, 0, blockHeadLagOf(t, network, "u1"))
	require.EqualValues(t, 2, blockHeadLagOf(t, network, "u2"))
	require.EqualValues(t, 5, blockHeadLagOf(t, network, "u3"))

	order, excluded := policy.LatestDecisionOutputForTest(network.policyEngine, network.networkId, "*")
	assert.NotContains(t, excluded, "u1")
	assert.NotContains(t, excluded, "u2")
	assert.NotContains(t, excluded, "u3")
	assert.ElementsMatch(t, []string{"u1", "u2", "u3"}, order,
		"all three within-tolerance upstreams must stay in rotation")
}
