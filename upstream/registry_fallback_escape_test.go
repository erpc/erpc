package upstream

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupRegistryWithFallbacks builds a 4-upstream registry (2 primaries + 2
// fallbacks) for testing GetFallbackEscapeUpstreams.
func setupRegistryWithFallbacks(t *testing.T, ctx context.Context) (*UpstreamsRegistry, *health.Tracker) {
	t.Helper()
	util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	logger := &log.Logger
	metricsTracker := health.NewTracker(logger, "fb-test", 10*time.Second)
	metricsTracker.Bootstrap(ctx)

	upstreamConfigs := []*common.UpstreamConfig{
		{
			Id: "primary-1", Type: common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		},
		{
			Id: "primary-2", Type: common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		},
		{
			Id: "fallback-1", Type: common.UpstreamTypeEvm,
			Endpoint: "http://rpc3.localhost",
			Tags:     []string{common.TagTierFallback},
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		},
		{
			Id: "fallback-2", Type: common.UpstreamTypeEvm,
			Endpoint: "http://rpc4.localhost",
			Tags:     []string{common.TagTierFallback},
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		},
	}

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(logger, vr, nil, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)

	registry := NewUpstreamsRegistry(ctx, logger, "fb-test", upstreamConfigs,
		ssr, nil, vr, pr, nil, metricsTracker,
		nil,
	)

	registry.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, registry.PrepareUpstreamsForNetwork(ctx, "evm:123"))
	return registry, metricsTracker
}

func findUpstream(t *testing.T, registry *UpstreamsRegistry, id string) *Upstream {
	t.Helper()
	for _, u := range registry.GetNetworkUpstreams(context.Background(), "evm:123") {
		if u.Id() == id {
			return u
		}
	}
	t.Fatalf("upstream %q not found", id)
	return nil
}

// TestGetFallbackEscapeUpstreams_FiltersByGroup verifies only fallback-group
// upstreams are returned.
func TestGetFallbackEscapeUpstreams_FiltersByGroup(t *testing.T) {
	defer util.ResetGock()
	defer util.AssertNoPendingMocks(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry, _ := setupRegistryWithFallbacks(t, ctx)

	out := registry.GetFallbackEscapeUpstreams(ctx, "evm:123", "eth_call")
	ids := make([]string, 0, len(out))
	for _, u := range out {
		ids = append(ids, u.Id())
	}
	assert.ElementsMatch(t, []string{"fallback-1", "fallback-2"}, ids,
		"only group=fallback upstreams should be returned; got %v", ids)
}

// TestGetFallbackEscapeUpstreams_IgnoresCordon verifies cordoned fallbacks
// are STILL returned. This is the central property of the escape hatch:
// the cordon-by-selectionPolicy must NOT exclude upstreams from the escape
// path, because the cordon is exactly what the escape is designed to bypass.
func TestGetFallbackEscapeUpstreams_IgnoresCordon(t *testing.T) {
	defer util.ResetGock()
	defer util.AssertNoPendingMocks(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry, mt := setupRegistryWithFallbacks(t, ctx)

	fb1 := findUpstream(t, registry, "fallback-1")
	fb2 := findUpstream(t, registry, "fallback-2")

	// Simulate the selectionPolicy cordoning both fallbacks.
	mt.Cordon(fb1, "*", "excluded by selection policy")
	mt.Cordon(fb2, "*", "excluded by selection policy")
	require.True(t, mt.IsCordoned(fb1, "eth_call"), "fb1 should be cordoned")
	require.True(t, mt.IsCordoned(fb2, "eth_call"), "fb2 should be cordoned")

	out := registry.GetFallbackEscapeUpstreams(ctx, "evm:123", "eth_call")
	ids := make([]string, 0, len(out))
	for _, u := range out {
		ids = append(ids, u.Id())
	}
	assert.ElementsMatch(t, []string{"fallback-1", "fallback-2"}, ids,
		"cordoned fallbacks must still be returned for the escape path; got %v", ids)
}

// TestGetFallbackEscapeUpstreams_FiltersIgnoreMethods verifies fallbacks
// whose IgnoreMethods contains the caller method are excluded — the escape
// path respects per-upstream method capabilities.
func TestGetFallbackEscapeUpstreams_FiltersIgnoreMethods(t *testing.T) {
	defer util.ResetGock()
	defer util.AssertNoPendingMocks(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry, _ := setupRegistryWithFallbacks(t, ctx)

	fb1 := findUpstream(t, registry, "fallback-1")
	// Mark eth_call as ignored on fallback-1; AllowMethods left empty.
	fb1.config.IgnoreMethods = []string{"eth_call"}

	out := registry.GetFallbackEscapeUpstreams(ctx, "evm:123", "eth_call")
	ids := make([]string, 0, len(out))
	for _, u := range out {
		ids = append(ids, u.Id())
	}
	assert.ElementsMatch(t, []string{"fallback-2"}, ids,
		"fallback-1 should be excluded because eth_call is in its IgnoreMethods; got %v", ids)

	// Sanity: a different method that isn't ignored should include fallback-1.
	out2 := registry.GetFallbackEscapeUpstreams(ctx, "evm:123", "eth_getLogs")
	ids2 := make([]string, 0, len(out2))
	for _, u := range out2 {
		ids2 = append(ids2, u.Id())
	}
	assert.ElementsMatch(t, []string{"fallback-1", "fallback-2"}, ids2,
		"eth_getLogs is not ignored; both fallbacks should be returned; got %v", ids2)
}

// TestGetFallbackEscapeUpstreams_NoFallbacksReturnsEmpty verifies the
// helper returns an empty slice (not nil-causing-panic) when no fallbacks
// are configured.
func TestGetFallbackEscapeUpstreams_NoFallbacksReturnsEmpty(t *testing.T) {
	util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Build a registry with primaries only.
	logger := &log.Logger
	metricsTracker := health.NewTracker(logger, "fb-test", 10*time.Second)
	metricsTracker.Bootstrap(ctx)
	upstreamConfigs := []*common.UpstreamConfig{
		{Id: "primary-only", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	}
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(logger, vr, nil, nil)
	require.NoError(t, err)
	ssr, err := data.NewSharedStateRegistry(ctx, logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{Driver: "memory", Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "1GB"}},
	})
	require.NoError(t, err)
	registry := NewUpstreamsRegistry(ctx, logger, "fb-test", upstreamConfigs,
		ssr, nil, vr, pr, nil, metricsTracker,
		nil,
	)
	registry.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, registry.PrepareUpstreamsForNetwork(ctx, "evm:123"))

	out := registry.GetFallbackEscapeUpstreams(ctx, "evm:123", "eth_call")
	assert.Empty(t, out, "no fallback-group upstreams configured → empty slice")
}

// IsDown filter coverage: the IsDown filter is exercised indirectly via the
// higher-level erpc.TestFailover_EscapeHatch end-to-end test (no clean
// unit-level way to flip an Upstream's CB to open without driving real
// failures through the failsafe stack from this package).
