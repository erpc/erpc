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

// Only a head from an upstream the selection policy keeps eligible lifts
// "latest": a fallback-tier (cordoned) or unknown source still feeds its own
// poller, but must not advertise a block no eligible upstream reports.
func TestNetworkHandle_SuggestLatestBlock_OnlyTipCandidatesLiftLatest(t *testing.T) {
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
	handle.SuggestLatestBlock("ws:fallback-1", 1010)
	handle.SuggestLatestBlock("ws:unknown", 1020)

	assert.Equal(t, int64(1010), fallback.EvmStatePoller().LatestBlock(),
		"the fallback's own poller still advances")
	assert.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx),
		"a cordoned or unknown source must not lift latest")

	handle.SuggestLatestBlock("ws:primary-1", 1001)
	assert.Equal(t, int64(1001), network.EvmHighestLatestBlockNumber(ctx),
		"an eligible upstream's head floors latest")
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
