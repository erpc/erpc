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

// These tests pin the NEW semantics of Network.EvmHighestLatestBlockNumber:
//
//   - Returns the MIN of the dominant cluster of upstream tips (NOT max).
//   - Strict monotonic forward progress via a network-level shared variable;
//     a second computation that would regress is rejected.
//
// They will FAIL against the existing MAX-based implementation. Each test
// case is designed so MAX(tips) != cluster-MIN to make the failure visible.

// servedTipFixture is a compact description of one upstream's poller state
// at test time. Used to drive the lighter setupServedTipNetwork helper.
type servedTipFixture struct {
	id           string
	chainID      int64
	latestBlock  int64
	syncing      bool
}

// setupServedTipNetwork bootstraps a Network with the supplied upstream
// fixtures and pushes each upstream's poller to the requested state.
//
// Each fixture maps to one upstream with a unique localhost endpoint mocked
// via gock to answer eth_chainId so initial validation succeeds. After
// bootstrap, SuggestLatestBlock and SetSyncingState are applied per fixture.
//
// Returns the network and the upstream slice in input order.
func setupServedTipNetwork(t *testing.T, ctx context.Context, fixtures []servedTipFixture) (*Network, []*upstream.Upstream) {
	t.Helper()

	if len(fixtures) == 0 {
		t.Fatal("setupServedTipNetwork requires at least one fixture")
	}

	chainID := fixtures[0].chainID
	if chainID == 0 {
		chainID = 123
	}

	upstreamConfigs := make([]*common.UpstreamConfig, 0, len(fixtures))
	for _, f := range fixtures {
		cid := f.chainID
		if cid == 0 {
			cid = chainID
		}
		endpoint := "http://" + f.id + ".localhost"
		upstreamConfigs = append(upstreamConfigs, &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       f.id,
			Endpoint: endpoint,
			Evm: &common.EvmUpstreamConfig{
				ChainId: cid,
			},
		})

		gock.New(endpoint).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, `eth_chainId`)
			}).
			Reply(200).
			JSON([]byte(`{"result":"0x7b"}`))
	}

	rateLimitersRegistry, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(
		&log.Logger,
		vr,
		[]*common.ProviderConfig{},
		nil,
	)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000, MaxTotalSize: "1GB",
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
		Evm: &common.EvmNetworkConfig{
			ChainId: chainID,
		},
	}

	network, err := NewNetwork(
		ctx,
		&log.Logger,
		"test",
		networkConfig,
		rateLimitersRegistry,
		upstreamsRegistry,
		metricsTracker,
		nil,
	)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)

	initErr := upstreamsRegistry.GetInitializer().WaitForTasks(ctx)
	require.NoError(t, initErr, "Upstream initializer failed to complete tasks")

	err = network.Bootstrap(ctx)
	require.NoError(t, err)
	network.PinUpstreamOrderForTest()
	time.Sleep(250 * time.Millisecond)

	upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(chainID))
	require.Len(t, upsList, len(fixtures))

	// Re-order returned upstreams to match fixtures' input order.
	ordered := make([]*upstream.Upstream, len(fixtures))
	for i, f := range fixtures {
		for _, u := range upsList {
			if u.Id() == f.id {
				ordered[i] = u
				break
			}
		}
		require.NotNil(t, ordered[i], "fixture %q not found in upstreams list", f.id)
	}

	// Push each upstream's poller to the requested state.
	for i, f := range fixtures {
		if f.latestBlock > 0 {
			ordered[i].EvmStatePoller().SuggestLatestBlock(f.latestBlock)
		}
		if f.syncing {
			ordered[i].EvmStatePoller().SetSyncingState(common.EvmSyncingStateSyncing)
		} else {
			ordered[i].EvmStatePoller().SetSyncingState(common.EvmSyncingStateNotSyncing)
		}
	}
	time.Sleep(50 * time.Millisecond)

	return network, ordered
}

// ----- new-semantics scenarios ----------------------------------------------

// Three close upstreams: MIN of the cluster (not MAX) is the served tip.
// MAX semantics (old) would return 100; new semantics returns 98.
func TestServedTip_ThreeUpstreams_AllClose_ReturnsMin(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100},
		{id: "u2", chainID: 123, latestBlock: 99},
		{id: "u3", chainID: 123, latestBlock: 98},
	})

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(98), served,
		"served tip must be MIN of dominant cluster, not MAX; "+
			"this returns 100 under the old EvmHighestLatestBlockNumber semantics")
}

// One leader + two laggers: dominant cluster is the laggers; their MIN wins.
// MAX semantics would return 100; new semantics returns 49.
// (In real serving, the monotonic clamp would refuse 49 if last-served was
// higher — but this test bypasses that by being the first call.)
func TestServedTip_OneLeader_TwoLaggers_ReturnsLaggerMin(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "leader", chainID: 123, latestBlock: 100},
		{id: "lagger1", chainID: 123, latestBlock: 50},
		{id: "lagger2", chainID: 123, latestBlock: 49},
	})

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(49), served,
		"with 1 leader vs 2 laggers, trust the lagging cluster; "+
			"this returns 100 under the old MAX semantics")
}

// Three close + two way-behind: dominant cluster is the 3-close; MIN of that
// cluster wins (98). Old MAX semantics returns 100.
func TestServedTip_FiveUpstreams_ThreeClose_TwoBehind(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100},
		{id: "u2", chainID: 123, latestBlock: 99},
		{id: "u3", chainID: 123, latestBlock: 98},
		{id: "u4", chainID: 123, latestBlock: 50},
		{id: "u5", chainID: 123, latestBlock: 49},
	})

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(98), served,
		"dominant cluster (3 close) wins; MIN of that cluster served; "+
			"this returns 100 under the old MAX semantics")
}

// Strict monotonic forward progress: a second computation that would
// regress (because upstreams retreated) is held at the last-served value.
//
// This exercises the network-level shared-variable TryUpdate clamp on top
// of the cluster picker. Under the old implementation that returns MAX of
// current tips with no monotonic guarantee, the second call returns 80
// (the new MAX) — so this test fails twice over: first because cluster-MIN
// expects 99 on the first call, and again because the second call must
// stay at 99 (not retreat to 80).
func TestServedTip_MonotonicClamp_HoldsLastServedOnRegression(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100},
		{id: "u2", chainID: 123, latestBlock: 100},
		{id: "u3", chainID: 123, latestBlock: 99},
	})

	served1 := network.EvmHighestLatestBlockNumber(ctx)
	require.Equal(t, int64(99), served1,
		"first call: cluster MIN of [99,100,100] = 99")

	// All upstreams retreat (e.g., transient outage causing pollers to lose
	// state, or a partial chain reorg perceived by some upstreams).
	ups[0].EvmStatePoller().SuggestLatestBlock(80)
	ups[1].EvmStatePoller().SuggestLatestBlock(79)
	ups[2].EvmStatePoller().SuggestLatestBlock(78)
	time.Sleep(50 * time.Millisecond)

	served2 := network.EvmHighestLatestBlockNumber(ctx)
	// NOTE: the per-upstream poller is itself monotonic via CounterInt64.
	// Its TryUpdate will reject the retreat — SuggestLatestBlock(80) won't
	// actually move u1's GetValue() down from 100. So this test ALSO checks
	// that the network method correctly relies on the (clamped) per-upstream
	// state and stays at 99, not on raw SuggestLatestBlock arguments.
	assert.GreaterOrEqual(t, served2, served1,
		"served tip must never regress; expected >= %d, got %d", served1, served2)
}

// Cold start: no upstreams populated yet. Returns 0.
func TestServedTip_NoUpstreamsHaveBlock_ReturnsZero(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 0},
		{id: "u2", chainID: 123, latestBlock: 0},
	})

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(0), served, "no upstream observations yet → 0")
}

// All upstreams syncing → filtered out → 0.
func TestServedTip_AllSyncing_ReturnsZero(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100, syncing: true},
		{id: "u2", chainID: 123, latestBlock: 200, syncing: true},
	})

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(0), served, "all upstreams syncing → no candidates → 0")
}
