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
	"slices"
	"sort"
	"strings"
	"sync/atomic"
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
	// pollEvery is the upstream's statePollerInterval; zero keeps background
	// polling off (1h) so tests drive polls explicitly.
	pollEvery time.Duration
	// pollsFail, while it holds true, makes the upstream's latest and
	// finalized polls answer a server error.
	pollsFail *atomic.Bool
}

// mockPollerHostWithLatest mocks one upstream host's state-poller RPCs with a
// caller-chosen latest/finalized height. The REAL poller fetches these over
// gock, so each upstream ends up with a genuine, distinct head tip — and thus
// a real block-head-lag — rather than the test injecting the lag directly.
// While pollsFail (nil: never) holds true, the head polls answer a 503.
func mockPollerHostWithLatest(host string, latest, finalized int64, pollsFail *atomic.Bool) {
	hx := func(n int64) string { return fmt.Sprintf("0x%x", n) }
	answers := func() bool { return pollsFail == nil || !pollsFail.Load() }
	headPoll := func(r *http.Request, tag string) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, tag)
	}
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_chainId") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x7b"}`)) // chainId 123
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool { return answers() && headPoll(r, "latest") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"` + hx(latest) + `","timestamp":"0x6702a8f0"}}`))
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool { return answers() && headPoll(r, "finalized") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"` + hx(finalized) + `","timestamp":"0x6702a8e0"}}`))
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool { return headPoll(r, "latest") || headPoll(r, "finalized") }).
		Reply(503).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"unavailable"}}`))
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
		mockPollerHostWithLatest(host, f.latest, finalized, f.pollsFail)
		pollEvery := f.pollEvery
		if pollEvery == 0 {
			pollEvery = time.Hour
		}
		upstreamConfigs = append(upstreamConfigs, &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       f.id,
			Endpoint: host,
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(pollEvery),
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
	stopAtEnd(t, network)
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
	// so every upstream's lag is recomputed against the final max. A copy: the
	// registry's own slice is read by the head loop meanwhile.
	upsList = slices.Clone(upsList)
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
	t.Cleanup(util.ResetGock)

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

	//    No probes either: probe responses never move a head, so probing a
	//    lag-only exclusion would bill its vendor without re-admitting it.
	for _, u := range network.policyEngine.GetExcluded(network.networkId, "*", "*") {
		assert.NotEqual(t, "frozen", u.Id(),
			"a lag-only exclusion must not be handed to the prober")
	}

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

// TestNetworkPolicy_RealPoll_FrozenUpstreamServesFinalizedBlocks: a node whose
// head froze far behind the tip but that still answers its polls holds every
// block up to its own finalized one. The tip ranking keeps excluding it; a
// client read of finalized block numbers it holds reaches it by the held
// ranking. A block above its own finalized one, a read of latest, and a node
// whose polls stopped answering never reach it.
func TestNetworkPolicy_RealPoll_FrozenUpstreamServesFinalizedBlocks(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Tip nodes at 1000 (finalized 990); two nodes at 100 (finalized 90).
	var deadFails atomic.Bool
	network := setupRealPollLagNetwork(t, ctx, []realPollFixture{
		{id: "tip-a", latest: 1000},
		{id: "tip-b", latest: 1000},
		{id: "tip-c", latest: 1000},
		{id: "frozen-archive", latest: 100},
		{id: "dead-archive", latest: 100, pollsFail: &deadFails},
	})
	deadFails.Store(true)

	tr := network.metricsTracker
	var nowNs atomic.Int64
	nowNs.Store(time.Now().UnixNano())
	tr.SetClockForTest(func() time.Time { return time.Unix(0, nowNs.Load()) })
	tips := []common.Upstream{upstreamByID(t, network, "tip-a"), upstreamByID(t, network, "tip-b"), upstreamByID(t, network, "tip-c")}
	frozen, dead := upstreamByID(t, network, "frozen-archive"), upstreamByID(t, network, "dead-archive")

	// Every fetch cadence is the 1h poll interval, so the stall window is 3h.
	// For 3h20m the tip moves a block every 10 minutes; the frozen node
	// answers each poll at 100, the dead node's polls fail.
	for i := int64(1); i <= 20; i++ {
		nowNs.Add(int64(10 * time.Minute))
		for _, u := range tips {
			tr.SetLatestBlockNumber(u, 1000+i, 0)
		}
		_, err := frozen.(common.EvmUpstream).EvmStatePoller().PollLatestBlockNumber(ctx)
		require.NoError(t, err)
		_, _ = dead.(common.EvmUpstream).EvmStatePoller().PollLatestBlockNumber(ctx)
	}
	require.True(t, tr.IsHeadBenched(frozen), "the frozen node stayed behind peers that kept moving")
	require.True(t, tr.IsHeadBenched(dead), "the dead node stayed behind peers that kept moving")
	policy.TickForTest(network.policyEngine, network.networkId, "*")

	// (e) The tip ranking still excludes both.
	_, excluded := policy.LatestDecisionOutputForTest(network.policyEngine, network.networkId, "*")
	assert.Contains(t, excluded, "frozen-archive", "the tip ranking keeps excluding the frozen node")
	assert.Contains(t, excluded, "dead-archive")

	block := func(n int64) string {
		return fmt.Sprintf(`{"number":"0x%x","hash":"0x%064x","parentHash":"0x%064x","timestamp":"0x6702a000"}`, n, n, n-1)
	}
	var frozenCalls, deadCalls atomic.Int64
	for _, hex := range []string{"0x32", "0x5f"} {
		for _, u := range tips {
			gock.New(upstreamHostFromID(u.Id())).Post("").Persist().
				Filter(func(r *http.Request) bool {
					b := util.SafeReadBody(r)
					return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, `"`+hex+`"`)
				}).
				Reply(503).
				JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"unavailable"}}`))
		}
	}
	mockGetBlockByNumberAt("0x32", block(50), &frozenCalls, "frozen-archive")
	mockGetBlockByNumberAt("0x5f", block(95), &frozenCalls, "frozen-archive")
	mockGetBlockByNumberAt("0x32", block(50), &deadCalls, "dead-archive")
	mockGetBlockByNumberAt("0x5f", block(95), &deadCalls, "dead-archive")
	for _, u := range tips {
		mockClean(t, upstreamHostFromID(u.Id()), "eth_getBalance", "0xaaa")
	}
	mockMethodAnswer("eth_getBalance", `"0xfff"`, &frozenCalls, "frozen-archive")
	mockMethodAnswer("eth_getBalance", `"0xfff"`, &deadCalls, "dead-archive")
	forward := func(body string) (*common.NormalizedResponse, error) {
		return network.Forward(ctx, common.NewNormalizedRequest([]byte(body)))
	}

	// (a) Block 50, at or below the network's and the frozen node's own
	// finalized block: every tip node fails, the frozen node serves it.
	resp, err := forward(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x32",false]}`)
	require.NoError(t, err, "a finalized block the frozen node holds must reach it when every tip node fails")
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), `"number":"0x32"`)
	assert.EqualValues(t, 1, frozenCalls.Load(), "the frozen node served block 50")

	// (b) Block 95 is above the frozen node's own finalized block 90.
	frozenCalls.Store(0)
	_, _ = forward(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x5f",false]}`)
	assert.EqualValues(t, 0, frozenCalls.Load(), "a block above the frozen node's own finalized block never reaches it")

	// (c) A read of latest keeps the tip ranking.
	_, err = forward(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`)
	require.NoError(t, err)
	assert.EqualValues(t, 0, frozenCalls.Load(), "a read of latest never reaches the frozen node")

	// (d) A node whose polls stopped answering is not frozen.
	assert.EqualValues(t, 0, deadCalls.Load(), "a node that stopped answering gets no request")
}

// TestNetworkPolicy_RealPoll_WithinTolerance_NoneExcluded guards the other
// direction: small, normal lag (a few blocks behind the tip) must NOT trip
// blockNumberLagAbove(16). Same real-poll path, no exclusion expected.
func TestNetworkPolicy_RealPoll_WithinTolerance_NoneExcluded(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupRealPollLagNetwork(t, ctx, []realPollFixture{
		{id: "u1", latest: 1000},
		{id: "u2", latest: 998}, // the corroborated head (second-highest)
		{id: "u3", latest: 995}, // 3 behind it — still within the 16-block tolerance
	})

	require.EqualValues(t, 0, blockHeadLagOf(t, network, "u1"))
	require.EqualValues(t, 0, blockHeadLagOf(t, network, "u2"))
	require.EqualValues(t, 3, blockHeadLagOf(t, network, "u3"))

	order, excluded := policy.LatestDecisionOutputForTest(network.policyEngine, network.networkId, "*")
	assert.NotContains(t, excluded, "u1")
	assert.NotContains(t, excluded, "u2")
	assert.NotContains(t, excluded, "u3")
	assert.ElementsMatch(t, []string{"u1", "u2", "u3"}, order,
		"all three within-tolerance upstreams must stay in rotation")
}

// TestNetworkPolicy_RealPoll_SlowPolledUpstreamAtTipStaysIn: an upstream
// polled every 10s on a 0.4s chain, always at the tip when asked, read up to
// 25 blocks behind late in each poll window, because peers polled every 500ms
// kept moving the network head past its stored one. A policy ticking every
// 10s too then read the same phase on every tick and excluded it for good on
// some processes. A healthy slow-polled upstream must stay in; one whose
// reports stop must still be excluded once a poll period has passed.
func TestNetworkPolicy_RealPoll_SlowPolledUpstreamAtTipStaysIn(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupRealPollLagNetwork(t, ctx, []realPollFixture{
		{id: "fast-a", latest: 1000},
		{id: "fast-b", latest: 1000},
		{id: "slow", latest: 1000, pollEvery: 10 * time.Second},
	})
	tr := network.metricsTracker
	var nowNs atomic.Int64
	nowNs.Store(time.Now().UnixNano())
	tr.SetClockForTest(func() time.Time { return time.Unix(0, nowNs.Load()) })
	fastA, fastB, slow := upstreamByID(t, network, "fast-a"), upstreamByID(t, network, "fast-b"), upstreamByID(t, network, "slow")

	// One 0.4s block per step; the fast pair reports every block, slow every
	// 25th (10s). Fast reports first, as their 500ms polls land first. Block
	// timestamps continue from the mocked poll's head block.
	const ts0 = int64(0x6702a8f0)
	step, slowReports := 0, true
	runTo := func(last int) {
		for ; step < last; step++ {
			i := int64(step + 1)
			nowNs.Add(int64(400 * time.Millisecond))
			head, ts := 1000+i, ts0+i*2/5
			tr.SetLatestBlockNumber(fastA, head, ts)
			tr.SetLatestBlockNumber(fastB, head, ts)
			if slowReports && i%25 == 0 {
				tr.SetLatestBlockNumber(slow, head, ts)
			}
		}
	}
	tick := func() (order, excluded []string) {
		policy.TickForTest(network.policyEngine, network.networkId, "*")
		return policy.LatestDecisionOutputForTest(network.policyEngine, network.networkId, "*")
	}
	runTo(70)

	// 8s after slow's report at step 50: the tip is 20 blocks past its stored
	// head, all of it made since slow was last asked.
	assert.LessOrEqual(t, blockHeadLagOf(t, network, "slow"), int64(1),
		"blocks made since the last poll are not lag")
	order, excluded := tick()
	assert.NotContains(t, excluded, "slow", "a slow-polled upstream at the tip must stay in rotation")
	assert.Contains(t, order, "slow")

	// Slow stops reporting. 26s after its last report the chain made 65
	// blocks; one poll period (25 blocks) is credited, the rest is lag.
	slowReports = false
	runTo(115)
	assert.GreaterOrEqual(t, blockHeadLagOf(t, network, "slow"), int64(39),
		"a report missing past one poll period counts as lag again")
	_, excluded = tick()
	assert.Contains(t, excluded, "slow", "an upstream whose reports stopped must be excluded")
}
