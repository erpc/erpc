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

// Once a WS newHeads tip N is observed (and about to be fan-out), HTTP
// tip resolution via EvmHighestLatestBlockNumber must not return < N —
// even if every local poller still reports N-1.
func TestNoteObservedLatestBlock_FloorsEvmHighestLatest(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	up := &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc1",
		Endpoint: "http://rpc1.localhost",
		Evm:      &common.EvmUpstreamConfig{ChainId: 123},
	}

	gock.New("http://rpc1.localhost").
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
		[]*common.UpstreamConfig{up}, ssr, rateLimitersRegistry, vr, pr, nil,
		metricsTracker, nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}
	network, err := NewNetwork(ctx, &log.Logger, "test", networkConfig,
		rateLimitersRegistry, upstreamsRegistry, metricsTracker, nil)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.GetInitializer().WaitForTasks(ctx))
	require.NoError(t, network.Bootstrap(ctx))
	time.Sleep(250 * time.Millisecond)

	upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.Len(t, upsList, 1)
	u := upsList[0]

	u.EvmStatePoller().SuggestLatestBlock(1000)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx))

	// Simulate the WS ingest path: a head arrives that we are about to
	// fan-out, but the lagging HTTP poller view is still at 1000.
	network.NoteObservedLatestBlock(ctx, 1001)

	got := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(1001), got,
		"after WS tip observation, highest latest must be ≥ delivered head")

	// Even if the network shared counter is somehow still behind (or a
	// local aggregator race computes 1000), the process-local high-water
	// mark from NoteObservedLatestBlock must clamp the return.
	require.NotNil(t, network.latestBlockShared)
	// Shared already at 1001 from NoteObserved; verify lastReturned alone
	// is enough by calling apply path with a lower computed tip via the
	// monotonic guard — EvmHighest after noting must never go backwards.
	assert.GreaterOrEqual(t, network.lastReturnedLatestBlock.Load(), int64(1001))
	assert.Equal(t, int64(1001), network.EvmHighestLatestBlockNumber(ctx))
}

// End-to-end through networkHandle.SuggestLatestBlock — the Indexer hook
// that runs before fan-out.
func TestNetworkHandle_SuggestLatestBlock_AdvancesNetworkTipBeforeFanOut(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	up := &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "bor-1",
		Endpoint: "http://bor1.localhost",
		Evm:      &common.EvmUpstreamConfig{ChainId: 123},
	}

	gock.New("http://bor1.localhost").
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
		[]*common.UpstreamConfig{up}, ssr, rateLimitersRegistry, vr, pr, nil,
		metricsTracker, nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}
	network, err := NewNetwork(ctx, &log.Logger, "test", networkConfig,
		rateLimitersRegistry, upstreamsRegistry, metricsTracker, nil)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.GetInitializer().WaitForTasks(ctx))
	require.NoError(t, network.Bootstrap(ctx))
	time.Sleep(250 * time.Millisecond)

	upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.Len(t, upsList, 1)
	upsList[0].EvmStatePoller().SuggestLatestBlock(90677358)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int64(90677358), network.EvmHighestLatestBlockNumber(ctx))

	handle := &networkHandle{nw: network}
	// Mirrors indexer.Ingest ordering: SuggestLatestBlock then fan-out.
	wsHeader := []byte(`{"number":"0x56789cf","hash":"0xabc","parentHash":"0xdef"}`)
	handle.SuggestLatestBlock("ws:bor-1", 90677359, wsHeader)

	assert.Equal(t, int64(90677359), upsList[0].EvmStatePoller().LatestBlock(),
		"per-upstream poller must advance")
	assert.Equal(t, int64(90677359), network.EvmHighestLatestBlockNumber(ctx),
		"network tip must advance before any client would see the WS head")
	assert.GreaterOrEqual(t, network.lastReturnedLatestBlock.Load(), int64(90677359),
		"process-local high-water mark must cover the delivered WS tip")
	cachedNum, cachedPayload := network.LastObservedLatestHead()
	assert.Equal(t, int64(90677359), cachedNum)
	assert.Contains(t, string(cachedPayload), `"0x56789cf"`)
}

// EvmRefreshHighestLatestBlockNumber must not regress the tip after
// NoteObservedLatestBlock (sync publish path) has advanced TipHW.
func TestEvmRefreshHighestLatestBlockNumber_PreservesObservedTip(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	up := &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc1",
		Endpoint: "http://rpc1.localhost",
		Evm:      &common.EvmUpstreamConfig{ChainId: 123},
	}

	gock.New("http://rpc1.localhost").
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
		[]*common.UpstreamConfig{up}, ssr, rateLimitersRegistry, vr, pr, nil,
		metricsTracker, nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}
	network, err := NewNetwork(ctx, &log.Logger, "test", networkConfig,
		rateLimitersRegistry, upstreamsRegistry, metricsTracker, nil)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.GetInitializer().WaitForTasks(ctx))
	require.NoError(t, network.Bootstrap(ctx))
	time.Sleep(250 * time.Millisecond)

	upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.Len(t, upsList, 1)
	upsList[0].EvmStatePoller().SuggestLatestBlock(1000)
	time.Sleep(50 * time.Millisecond)

	network.NoteObservedLatestBlock(ctx, 1001)
	assert.Equal(t, int64(1001), network.EvmRefreshHighestLatestBlockNumber(ctx),
		"refresh after sync TipHW publish must keep the observed tip")
	assert.Equal(t, int64(1001), network.EvmHighestLatestBlockNumber(ctx))
}
