package erpc

import (
	"context"
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
	"github.com/stretchr/testify/require"
)

// Tests focus on lazy-loading behavior and initializer states (Ready/Fatal/Initializing)
// while ensuring the first request can wait up to the adaptive timeout and later
// attempts short-circuit according to state.

func TestNetworksBootstrap_SlowProviderUpstreams_InitializeThenServe(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

	// Provider that generates a single upstream; we simulate slow endpoint readiness by delaying the first real call.
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{
		{
			Id:                 "erpc-provider",
			Vendor:             "erpc",
			UpstreamIdTemplate: "erpc-<NETWORK>",
			Settings: common.VendorSettings{
				// Use local mocked endpoint so vendor checks and bootstrap hit gock
				"endpoint": "http://rpc1.localhost",
				"secret":   "abc",
			},
		},
	}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{Driver: "memory", Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}},
	})
	require.NoError(t, err)

	rlr, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)

	upr := upstream.NewUpstreamsRegistry(
		ctx,
		&log.Logger,
		"prjA",
		[]*common.UpstreamConfig{},
		ssr,
		rlr,
		vr,
		pr,
		nil,
		mt,
		1*time.Second,
		nil,
	)

	// Network config without static upstreams to force provider path
	ncfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	net, err := NewNetwork(ctx, &log.Logger, "prjA", ncfg, rlr, upr, mt)
	require.NoError(t, err)

	// Prepare upstreams with provider tasks and wait; we expect initialization to succeed after provider config conversion.
	upr.Bootstrap(ctx)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, net.Bootstrap(ctx))
}

func TestNetworksBootstrap_UnsupportedNetwork_FatalFast(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

	vr := thirdparty.NewVendorsRegistry()
	// Providers list empty => unsupported network when there are no static upstreams
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: "memory", Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
	require.NoError(t, err)

	rlr, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)

	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)

	// No static upstreams; network id is valid but unsupported by providers -> fatal
	err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(999999))
	require.Error(t, err)
	require.Contains(t, err.Error(), string(common.ErrCodeNetworkNotSupported))
}

func TestNetworksBootstrap_ProviderInitializing_503Retry(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Short context to force the timeout path while provider tasks still initializing
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	// Use a provider but do not allow enough time for it to finish
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{
		{
			Id:                 "erpc-provider",
			Vendor:             "erpc",
			UpstreamIdTemplate: "erpc-<NETWORK>",
			Settings: common.VendorSettings{
				// Point to a slow/unavailable host to keep initializer in initializing state
				"endpoint": "http://rpc9.localhost",
				"secret":   "abc",
			},
		},
	}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: "memory", Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
	require.NoError(t, err)

	rlr, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)

	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)

	// Make provider JSON-RPC fail immediately to keep initializer in initializing state until context times out
	gock.New("http://rpc9.localhost").
		Post("").
		Times(1).
		Reply(500)

	err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
	require.Error(t, err)
	require.Contains(t, err.Error(), string(common.ErrCodeNetworkInitializing))
	_ = gock.IsDone()
}
