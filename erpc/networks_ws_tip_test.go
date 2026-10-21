package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/indexer"
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

func tipHash(tag string, n uint64) string {
	return fmt.Sprintf("0x%s%0*x", tag, 64-len(tag), n)
}

// tipHead is block n of the linear chain tagged tag.
func tipHead(tag string, n uint64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"number":"0x%x","hash":"%s","parentHash":"%s","timestamp":"0x%x"}`,
		n, tipHash(tag, n), tipHash(tag, n-1), 1_700_000_000+n))
}

func eventuallyLatest(t *testing.T, ctx context.Context, network *Network, want int64, msg string) {
	t.Helper()
	require.Eventually(t, func() bool { return network.EvmHighestLatestBlockNumber(ctx) == want },
		2*time.Second, 10*time.Millisecond, msg)
}

// A source header moves its upstream's poller at once, and the network tip
// once the network-wide tracker pins it.
func TestNetworkIntake_HeaderAdvancesNetworkTip(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, u := setupWsTipNetwork(t, ctx, "rpc2", "http://rpc2.localhost")
	u.EvmStatePoller().SuggestLatestBlock(90677358)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int64(90677358), network.EvmHighestLatestBlockNumber(ctx))

	network.intake.Header("rpc2", tipHead("a", 90677359))

	assert.Equal(t, int64(90677359), u.EvmStatePoller().LatestBlock(), "per-upstream poller must advance")
	eventuallyLatest(t, ctx, network, 90677359, "the pinned head lifts the network tip")
	require.Eventually(t, func() bool { return network.deliveredLatestBlock.Load() >= 90677359 },
		2*time.Second, 10*time.Millisecond, "the tracker's pin change moves the delivered-head floor")
}

// An unknown source lifts nothing; an eligible one does.
func TestNetworkIntake_EligibleSourceLiftsLatest(t *testing.T) {
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{primaryLatest: "0x3e8", fallbackLatest: "0x3e8"})
	require.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx))

	network.intake.Header("unknown", tipHead("a", 1020))
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx), "an unknown source must not lift latest")

	network.intake.Header("primary-1", tipHead("a", 1001))
	eventuallyLatest(t, ctx, network, 1001, "an eligible source's head floors latest")
}

// An upstream the selection policy excludes never reaches clients: its own
// poller sees its heads, but they never vote, so no tap receives them.
func TestNetworkIntake_PolicyExcludedUpstreamNeverReachesClients(t *testing.T) {
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The default policy cordons the fallback tier while the primaries are healthy.
	network, ups, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{primaryLatest: "0x3e8", fallbackLatest: "0x3e8"})
	byId := map[string]*upstream.Upstream{}
	for _, u := range ups {
		byId[u.Id()] = u
	}

	got := make(chan uint64, 16)
	go func() {
		_ = indexer.NewTaps(nil).RunChain(ctx, network.networkId, "test", indexer.SubTypeNewHeads, network.chainTracker, nil, indexer.ReadOpts{},
			func(b indexer.Batch) error {
				for _, blk := range b.Applied {
					got <- blk.Number
				}
				return nil
			})
	}()

	network.intake.Header("fallback-1", tipHead("b", 1010))
	assert.Equal(t, int64(1010), byId["fallback-1"].EvmStatePoller().LatestBlock(), "its own poller still sees the head")
	network.intake.Header("primary-1", tipHead("a", 1001))
	network.intake.Header("primary-1", tipHead("a", 1002))

	for _, want := range []uint64{1001, 1002} {
		select {
		case n := <-got:
			require.Equal(t, want, n)
		case <-time.After(2 * time.Second):
			t.Fatalf("the client never received block %d", want)
		}
	}
	select {
	case n := <-got:
		t.Fatalf("the client received block %d, which only the excluded upstream has", n)
	case <-time.After(300 * time.Millisecond):
	}
	assert.Equal(t, int64(1002), network.EvmHighestLatestBlockNumber(ctx))
}

// With failover on, a fallback's head votes only while no eligible primary
// is a live source. Its own poller sees every head regardless.
func TestNetworkIntake_FallbackHeadsHeldWhilePrimariesStream(t *testing.T) {
	setup := func(t *testing.T, ctx context.Context, noPolicy bool) (*Network, map[string]*upstream.Upstream) {
		network, ups, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8", // 1000
			fallbackLatest: "0x3e8",
			enableFailover: true,
			noPolicy:       noPolicy,
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
		network, ups := setup(t, ctx, true)

		network.intake.Header("primary-2", tipHead("a", 1000))
		network.intake.Header("fallback-1", tipHead("b", 1010))
		assert.Equal(t, int64(1010), ups["fallback-1"].EvmStatePoller().LatestBlock(),
			"the fallback's poller still sees its head")
		time.Sleep(100 * time.Millisecond)
		assert.Less(t, network.deliveredLatestBlock.Load(), int64(1010), "a held head must not lift latest")

		network.intake.Header("primary-2", tipHead("a", 1001))
		eventuallyLatest(t, ctx, network, 1001, "the primary's head lifts latest")
	})

	t.Run("DeliveredWhenNoPrimaryStreams", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, _ := setup(t, ctx, true)

		network.intake.Header("fallback-1", tipHead("b", 1010))
		eventuallyLatest(t, ctx, network, 1010, "with no primary source, the fallback votes")
	})

	t.Run("DeliveredWhenPolicyExcludesPrimaries", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, ups := setup(t, ctx, false)

		network.intake.Header("primary-1", tipHead("a", 1000))
		network.intake.Header("primary-2", tipHead("a", 1000))
		network.intake.Header("fallback-1", tipHead("b", 1010))
		time.Sleep(100 * time.Millisecond)
		require.Less(t, network.deliveredLatestBlock.Load(), int64(1010))

		ups["primary-1"].Cordon("*", "test")
		ups["primary-2"].Cordon("*", "test")
		policy.TickForTest(network.policyEngine, network.networkId, "*")

		network.intake.Header("fallback-1", tipHead("b", 1011))
		eventuallyLatest(t, ctx, network, 1011, "excluded primaries no longer hold the fallback back")
	})

	t.Run("DeliveredWithFailoverOff", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8",
			fallbackLatest: "0x3e8",
			noPolicy:       true,
		})

		network.intake.Header("primary-1", tipHead("a", 1000))
		network.intake.Header("fallback-1", tipHead("a", 1001))
		eventuallyLatest(t, ctx, network, 1001, "without failover every eligible source votes")
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
// before the poller takes it. Until then it must not vote: a head from an
// endpoint now answering for another chain must never reach clients.
func TestNetworkIntake_WithholdsUnacceptedMajorJump(t *testing.T) {
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

	network.intake.Header("rpc2", tipHead("e", 5_000_000))
	time.Sleep(300 * time.Millisecond) // let the async verification finish (and fail)
	assert.Equal(t, int64(1000), u.EvmStatePoller().LatestBlock())
	assert.Less(t, network.deliveredLatestBlock.Load(), int64(5_000_000))
	network.intake.Header("rpc2", tipHead("a", 1001))
	network.intake.Header("rpc2", tipHead("a", 1002))
	require.Eventually(t, func() bool { return network.deliveredLatestBlock.Load() == 1002 },
		2*time.Second, 10*time.Millisecond)
}

// A head from a source the network does not know never votes and leaves the
// delivered-head floor alone.
func TestNetworkIntake_WithholdsUnverifiableSource(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupWsTipNetwork(t, ctx, "rpc3", "http://rpc3.localhost")
	floor := network.deliveredLatestBlock.Load()

	network.intake.Header("not-in-this-network", tipHead("a", 5_000_000))
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, floor, network.deliveredLatestBlock.Load())
}
