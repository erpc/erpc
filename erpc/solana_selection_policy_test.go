package erpc

import (
	"context"
	"net/http"
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

// mockSolanaStatePollerForHost registers the persistent state-poller mocks a
// Solana upstream needs to bootstrap and poll: genesis hash (cluster gate),
// getHealth, getSlot, and getMaxShredInsertSlot. Every upstream reports the same
// slot — the finalization lag under test is seeded directly on the tracker (see
// the test), not via these mocks, so upstream selection is the only variable.
func mockSolanaStatePollerForHost(host string) {
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "getGenesisHash") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"}`)) // mainnet-beta

	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "getHealth") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"ok"}`))

	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "getSlot") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":300000000}`))

	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "getMaxShredInsertSlot") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":300000000}`))
}

// setupSolanaSelectionPolicyNetwork builds a real Solana Network with a custom
// selection policy and a frozen policy ticker (EvalInterval=0; tests drive ticks
// via policy.TickForTest). Mirrors setupSelectionPolicyNetwork (EVM) in
// networks_selection_policy_test.go but for architecture: solana.
func setupSolanaSelectionPolicyNetwork(t *testing.T, ctx context.Context, upstreamConfigs []*common.UpstreamConfig, evalFunc string) *Network {
	t.Helper()

	setupGockObserver(t)
	for _, c := range upstreamConfigs {
		mockSolanaStatePollerForHost(c.Endpoint)
		// Opt out of probe traffic so "excluded == zero getBalance hits" holds.
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
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)

	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "test", upstreamConfigs, ssr,
		rateLimitersRegistry, vr, pr, nil, metricsTracker, nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureSolana,
		Solana:       &common.SolanaNetworkConfig{Cluster: "mainnet-beta"},
		SelectionPolicy: &common.SelectionPolicyConfig{
			EvalInterval: 0, // frozen — tests drive ticks
			EvalFunc:     evalFunc,
		},
	}

	policyEngine := policy.NewEngine(ctx, &log.Logger, "test", metricsTracker, policystdlib.Install, nil)
	network, err := NewNetwork(
		ctx, &log.Logger, "test", networkConfig,
		rateLimitersRegistry, upstreamsRegistry, metricsTracker, policyEngine,
	)
	require.NoError(t, err)

	netID := util.SolanaNetworkId("mainnet-beta")
	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, netID))
	require.NoError(t, network.Bootstrap(ctx))

	for _, ups := range upstreamsRegistry.GetNetworkUpstreams(ctx, netID) {
		require.NoError(t, ups.Bootstrap(ctx))
	}

	// Wait until the registry has converged on the full upstream set.
	expectedCount := len(upstreamConfigs)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(upstreamsRegistry.GetNetworkUpstreams(ctx, netID)) >= expectedCount {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Len(t, upstreamsRegistry.GetNetworkUpstreams(ctx, netID), expectedCount,
		"registry never converged on the full upstream set")

	return network
}

// solanaConcreteUpstream returns the concrete *upstream.Upstream for an id (it
// exposes SolanaStatePoller(), which is not on the common.Upstream interface).
func solanaConcreteUpstream(t *testing.T, network *Network, id string) *upstream.Upstream {
	t.Helper()
	for _, u := range network.upstreamsRegistry.GetNetworkUpstreams(context.Background(), network.networkId) {
		if u.Id() == id {
			return u
		}
	}
	t.Fatalf("upstream %q not registered", id)
	return nil
}

// TestSolanaSelectionPolicy_FinalizationLagExclusion proves the Solana failover
// chain end-to-end: a finalization-lag-aware selection policy hard-excludes a
// finalized-lagging upstream so a finalized read is served ONLY by the near-tip
// upstream. The finalizationLag the policy reads is the exact metric the state
// poller feeds via tracker.SetFinalizedBlockNumber.
//
// Determinism: the lag is seeded DIRECTLY on the tracker's {*, All} bucket — the
// same bucket the eval reads and the same mechanism the EVM lag test uses
// (seedDegraded). We do NOT rely on poller convergence: the poller's finalized
// OnValue is change-triggered, so it fires at most once per upstream during
// bootstrap; every node reports the same slot here, so the poller computes lag=0
// for both and the seeded value below is the sole source of the laggard's lag
// and is never overwritten. The policy ticker is frozen (EvalInterval=0) and
// driven via TickForTest, and getBalance hits are counted per host (guarded) so
// background poll traffic cannot pollute the assertion.
func TestSolanaSelectionPolicy_FinalizationLagExclusion(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tipHost := upstreamHostFromID("sol_tip")
	lagHost := upstreamHostFromID("sol_laggard")

	// Exclude any upstream whose finalized slot lags by > 50; never fail closed.
	evalFunc := `(upstreams, ctx) => upstreams.excludeIf(finalizationLagAbove(50), { probe: false }).whenEmpty(() => upstreams)`

	network := setupSolanaSelectionPolicyNetwork(t, ctx, []*common.UpstreamConfig{
		{Type: common.UpstreamTypeSolana, Id: "sol_tip", Endpoint: tipHost, Solana: &common.SolanaUpstreamConfig{Cluster: "mainnet-beta"}},
		{Type: common.UpstreamTypeSolana, Id: "sol_laggard", Endpoint: lagHost, Solana: &common.SolanaUpstreamConfig{Cluster: "mainnet-beta"}},
	}, evalFunc)

	tipUps := solanaConcreteUpstream(t, network, "sol_tip")
	lagUps := solanaConcreteUpstream(t, network, "sol_laggard")

	// Seed the laggard's finalization lag deterministically on the {*, All}
	// bucket the selection eval reads (mirrors seedDegraded's direct .Store for
	// blockHeadLag). The bootstrap poller one-shot has already run by now and
	// computed lag=0 for both (identical slots); this store is the sole source
	// of the laggard's lag and the poller will not fire again (slot unchanged).
	lagMetrics := network.metricsTracker.GetUpstreamMethodMetrics(lagUps, "*", common.DataFinalityStateAll)
	require.NotNil(t, lagMetrics)
	lagMetrics.FinalizationLag.Store(100)

	// Guard: the tip must NOT be lagging — otherwise both would be excluded and
	// whenEmpty() would restore the full set (laggard included), masking the test.
	tipMetrics := network.metricsTracker.GetUpstreamMethodMetrics(tipUps, "*", common.DataFinalityStateAll)
	require.NotNil(t, tipMetrics)
	require.Equal(t, int64(0), tipMetrics.FinalizationLag.Load(),
		"tip must not be finalization-lagging (else whenEmpty fail-open masks the exclusion)")

	// Drive one policy eval against the seeded metrics.
	policy.ResetSlotStateForTest(network.policyEngine, network.networkId, "*")
	policy.TickForTest(network.policyEngine, network.networkId, "*")

	// Count getBalance hits per host (guarded so background poll traffic and the
	// other host's mock-filter evaluation never inflate the count).
	var tipBalance, lagBalance atomic.Int32
	gock.New(tipHost).Post("").Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host == "sol_tip.localhost" && strings.Contains(util.SafeReadBody(r), "getBalance") {
				tipBalance.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000000},"value":1111}}`))
	gock.New(lagHost).Post("").Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host == "sol_laggard.localhost" && strings.Contains(util.SafeReadBody(r), "getBalance") {
				lagBalance.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000000},"value":2222}}`))

	// Forward a finalized getBalance.
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin",{"commitment":"finalized"}]}`,
	))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Nil(t, jrr.Error, "finalized request should succeed")

	assert.GreaterOrEqual(t, int(tipBalance.Load()), 1, "near-tip upstream must serve the finalized request")
	assert.Equal(t, int32(0), lagBalance.Load(),
		"laggard (finalizationLag=100 > threshold 50) must be excluded by the selection policy and never receive getBalance")
}
