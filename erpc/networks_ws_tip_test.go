package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// setupWsTipNetwork bootstraps a single-upstream EVM network backed by an
// in-memory shared state registry.
func setupWsTipNetwork(t *testing.T, ctx context.Context, id, endpoint string) (*Network, *upstream.Upstream) {
	t.Helper()
	util.SetupMocksForEvmStatePoller()
	gock.New(endpoint).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), `eth_chainId`)
		}).
		Reply(200).
		JSON([]byte(`{"result":"0x7b"}`))

	rateLimitersRegistry, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)
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
		ctx, &log.Logger, "test",
		[]*common.UpstreamConfig{{
			Type:     common.UpstreamTypeEvm,
			Id:       id,
			Endpoint: endpoint,
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		}}, ssr, rateLimitersRegistry, vr, pr, nil,
		metricsTracker, nil,
	)
	network, err := NewNetwork(ctx, &log.Logger, "test", &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}, rateLimitersRegistry, upstreamsRegistry, metricsTracker, nil)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.GetInitializer().WaitForTasks(ctx))
	require.NoError(t, network.Bootstrap(ctx))
	time.Sleep(250 * time.Millisecond)

	upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.Len(t, upsList, 1)
	return network, upsList[0]
}

// Once a newHeads head N is about to be delivered, "latest" must not return
// less than N even while every poller still reports N-1.
func TestNoteObservedLatestBlock_FloorsEvmHighestLatest(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, u := setupWsTipNetwork(t, ctx, "rpc1", "http://rpc1.localhost")
	u.EvmStatePoller().SuggestLatestBlock(1000)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx))

	network.NoteObservedLatestBlock(ctx, 1001)

	assert.Equal(t, int64(1001), network.EvmHighestLatestBlockNumber(ctx),
		"after WS tip observation, highest latest must be ≥ delivered head")
	require.NotNil(t, network.latestBlockShared)
	assert.GreaterOrEqual(t, network.deliveredLatestBlock.Load(), int64(1001))
	assert.Equal(t, int64(1001), network.EvmHighestLatestBlockNumber(ctx))
}

// End-to-end through networkHandle.SuggestLatestBlock, which runs before
// fan-out.
func TestNetworkHandle_SuggestLatestBlock_AdvancesNetworkTipBeforeFanOut(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, u := setupWsTipNetwork(t, ctx, "rpc2", "http://rpc2.localhost")
	u.EvmStatePoller().SuggestLatestBlock(90677358)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int64(90677358), network.EvmHighestLatestBlockNumber(ctx))

	handle := &networkHandle{nw: network}
	handle.SuggestLatestBlock("ws:rpc2", 90677359)

	assert.Equal(t, int64(90677359), u.EvmStatePoller().LatestBlock(),
		"per-upstream poller must advance")
	assert.Equal(t, int64(90677359), network.EvmHighestLatestBlockNumber(ctx),
		"network tip must advance before any client would see the WS head")
	assert.GreaterOrEqual(t, network.deliveredLatestBlock.Load(), int64(90677359),
		"process-local high-water mark must cover the delivered WS tip")
}

// Clients receive heads from whichever source delivers first, so a head from
// a cordoned fallback lifts "latest" too; an unknown source does not.
func TestNetworkHandle_SuggestLatestBlock_EveryKnownSourceLiftsLatest(t *testing.T) {
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
		primaryLatest:  "0x3e8", // 1000
		fallbackLatest: "0x3e8",
	})
	require.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx))
	var fallback *upstream.Upstream
	for _, u := range ups {
		if u.Id() == "fallback-1" {
			fallback = u
		}
	}
	require.NotNil(t, fallback)

	handle := &networkHandle{nw: network}
	handle.SuggestLatestBlock("ws:unknown", 1020)
	assert.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx),
		"an unknown source must not lift latest")

	handle.SuggestLatestBlock("ws:fallback-1", 1010)
	assert.Equal(t, int64(1010), fallback.EvmStatePoller().LatestBlock())
	assert.Equal(t, int64(1010), network.EvmHighestLatestBlockNumber(ctx),
		"a head a client can receive from the cordoned fallback floors latest")
}

type fakeHeadSource bool

func (f fakeHeadSource) HeadsLive() bool { return bool(f) }

// With failover on, a fallback's head reaches clients (and lifts latest) only
// when no primary the policy routes to is streaming heads itself. The
// fallback's own poller sees every head regardless.
func TestNetworkHandle_FallbackHeadsHeldWhilePrimariesStream(t *testing.T) {
	setup := func(t *testing.T, ctx context.Context) (*Network, map[string]*upstream.Upstream) {
		network, ups, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8", // 1000
			fallbackLatest: "0x3e8",
			enableFailover: true,
		})
		require.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx))
		byId := make(map[string]*upstream.Upstream, len(ups))
		for _, u := range ups {
			byId[u.Id()] = u
		}
		return network, byId
	}

	t.Run("HeldWhileAPrimaryStreams", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, ups := setup(t, ctx)

		handle := &networkHandle{nw: network, heads: map[string]headSource{
			"primary-1":  fakeHeadSource(false),
			"primary-2":  fakeHeadSource(true),
			"fallback-1": fakeHeadSource(true),
		}}
		assert.False(t, handle.SuggestLatestBlock("ws:fallback-1", 1010))
		assert.Equal(t, int64(1010), ups["fallback-1"].EvmStatePoller().LatestBlock(),
			"the fallback's poller still sees its head")
		assert.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx),
			"a held head must not lift latest")

		assert.True(t, handle.SuggestLatestBlock("ws:primary-2", 1001))
		assert.Equal(t, int64(1001), network.EvmHighestLatestBlockNumber(ctx))
	})

	t.Run("DeliveredWhenNoPrimaryStreams", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, _ := setup(t, ctx)

		// Primaries connected over HTTP only, or with no live subscription.
		for _, heads := range []map[string]headSource{
			{"fallback-1": fakeHeadSource(true)},
			{"primary-1": fakeHeadSource(false), "fallback-1": fakeHeadSource(true)},
		} {
			handle := &networkHandle{nw: network, heads: heads}
			assert.True(t, handle.SuggestLatestBlock("ws:fallback-1", 1010))
		}
		assert.Equal(t, int64(1010), network.EvmHighestLatestBlockNumber(ctx))
	})

	t.Run("DeliveredWhenPolicyExcludesPrimaries", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, ups := setup(t, ctx)

		handle := &networkHandle{nw: network, heads: map[string]headSource{
			"primary-1":  fakeHeadSource(true),
			"primary-2":  fakeHeadSource(true),
			"fallback-1": fakeHeadSource(true),
		}}
		require.False(t, handle.SuggestLatestBlock("ws:fallback-1", 1010))

		ups["primary-1"].Cordon("*", "test")
		ups["primary-2"].Cordon("*", "test")
		policy.TickForTest(network.policyEngine, network.networkId, "*")

		assert.True(t, handle.SuggestLatestBlock("ws:fallback-1", 1011))
		assert.Equal(t, int64(1011), network.EvmHighestLatestBlockNumber(ctx))
	})

	t.Run("DeliveredWithFailoverOff", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8",
			fallbackLatest: "0x3e8",
		})

		handle := &networkHandle{nw: network, heads: map[string]headSource{
			"primary-1":  fakeHeadSource(true),
			"fallback-1": fakeHeadSource(true),
		}}
		assert.True(t, handle.SuggestLatestBlock("ws:fallback-1", 1010))
	})
}

// The delivered-head floor is network-wide: a use-upstream-scoped request is
// answered from its group's own head.
func TestDeliveredHeadFloor_SkipsSelectorScopedRequests(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "fast-1", chainID: 123, latestBlock: 1050, tags: []string{"family:fast"}},
		{id: "fast-2", chainID: 123, latestBlock: 1050, tags: []string{"family:fast"}},
		{id: "slow-1", chainID: 123, latestBlock: 1000, tags: []string{"family:slow"}},
		{id: "slow-2", chainID: 123, latestBlock: 1000, tags: []string{"family:slow"}},
	}, &common.EvmServedTipConfig{})
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	req.SetDirectives(&common.RequestDirectives{UseUpstream: "family:slow"})
	slowCtx := context.WithValue(ctx, common.RequestContextKey, req)

	network.NoteObservedLatestBlock(ctx, 1051)
	assert.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(slowCtx))
	assert.Equal(t, int64(1051), network.EvmHighestLatestBlockNumber(ctx))
}

// Projects sharing a process (and so a shared-state registry) must not floor
// each other's "latest" for the same chain.
func TestDeliveredHeadFloor_IsScopedPerProject(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "a", chainID: 123, latestBlock: 1000},
	}, &common.EvmServedTipConfig{})
	sibling, err := NewNetwork(ctx, &log.Logger, "sibling", network.cfg,
		network.rateLimitersRegistry, network.upstreamsRegistry, network.metricsTracker, nil)
	require.NoError(t, err)
	require.NotNil(t, sibling.latestBlockShared)

	network.NoteObservedLatestBlock(ctx, 1001)
	assert.Equal(t, int64(1001), network.latestBlockShared.GetValue())
	assert.Equal(t, int64(0), sibling.latestBlockShared.GetValue())
}

// Filter subscriptions follow the same tiers as heads: eligible primaries
// carry them, the fallback tier stands in, and without failover every
// WebSocket upstream is a default.
func TestTierWsIngresses(t *testing.T) {
	ws := []common.Upstream{
		common.NewFakeUpstream("p1"),
		common.NewFakeUpstream("p2"),
		common.NewFakeUpstream("fb", common.WithTags(common.TagTierFallback)),
	}
	eligible := map[string]struct{}{"p1": {}, "fb": {}}

	d, f := tierWsIngresses(ws, eligible, true)
	assert.Equal(t, []string{"ws:p1"}, d, "an ineligible primary does not carry filters")
	assert.Equal(t, []string{"ws:fb"}, f)

	d, f = tierWsIngresses(ws, nil, true)
	assert.Empty(t, d, "no eligible primary: only the fallbacks remain")
	assert.Equal(t, []string{"ws:fb"}, f)

	d, f = tierWsIngresses(ws, nil, false)
	assert.Equal(t, []string{"ws:p1", "ws:p2", "ws:fb"}, d)
	assert.Empty(t, f)
}

// A head far past the poller's (a major jump) is verified asynchronously
// before the poller takes it. Until then it must not reach clients: once
// delivered it would also advance the indexer's head dedup marker, and a
// head from an endpoint now answering for another chain would suppress
// every real head below it.
func TestNetworkHandle_SuggestLatestBlock_WithholdsUnacceptedMajorJump(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, u := setupWsTipNetwork(t, ctx, "rpc2", "http://rpc2.localhost")
	u.EvmStatePoller().SuggestLatestBlock(1000)
	require.Equal(t, int64(1000), u.EvmStatePoller().LatestBlock())

	// From here the endpoint answers for another chain.
	util.ResetGock()
	gock.New("http://rpc2.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), `eth_chainId`) }).
		Reply(200).JSON([]byte(`{"result":"0x1"}`))

	handle := &networkHandle{nw: network}
	assert.False(t, handle.SuggestLatestBlock("ws:rpc2", 5_000_000), "an unaccepted major jump must not reach clients")
	time.Sleep(300 * time.Millisecond) // let the async verification finish (and fail)
	assert.Equal(t, int64(1000), u.EvmStatePoller().LatestBlock())
	assert.Less(t, network.deliveredLatestBlock.Load(), int64(5_000_000))
	assert.True(t, handle.SuggestLatestBlock("ws:rpc2", 1001))
	assert.True(t, handle.SuggestLatestBlock("ws:rpc2", 1002))
}

// Heads are delivered only once a tip tracker has accepted them, so a head
// from a source the network can't verify (no such upstream, or one with no
// tracker) is withheld and leaves the delivered-head floor alone.
func TestNetworkHandle_SuggestLatestBlock_WithholdsUnverifiableSource(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupWsTipNetwork(t, ctx, "rpc3", "http://rpc3.localhost")
	handle := &networkHandle{nw: network}
	floor := network.deliveredLatestBlock.Load()

	assert.False(t, handle.SuggestLatestBlock("ws:not-in-this-network", 5_000_000))
	assert.Equal(t, floor, network.deliveredLatestBlock.Load())
}
