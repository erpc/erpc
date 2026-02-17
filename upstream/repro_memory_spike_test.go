package upstream

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func buildRegistryWithSmallTopology(ctx context.Context, networkID, projectID string) (*UpstreamsRegistry, *health.Tracker) {
	logger := &log.Logger
	metricsTracker := health.NewTracker(logger, projectID, time.Minute)
	metricsTracker.Bootstrap(ctx)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(logger, vr, nil, nil)
	if err != nil {
		panic(err)
	}
	rlr, err := NewRateLimitersRegistry(ctx, nil, logger)
	if err != nil {
		panic(err)
	}

	ssr, err := data.NewSharedStateRegistry(ctx, logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000,
				MaxTotalSize: "1GB",
			},
		},
	})
	if err != nil {
		panic(err)
	}

	registry := NewUpstreamsRegistry(
		ctx,
		logger,
		projectID,
		nil,
		ssr,
		rlr,
		vr,
		pr,
		nil,
		metricsTracker,
		1*time.Second,
		nil,
	)

	cfg := &common.UpstreamConfig{
		Id:         "rpc-1",
		Type:       common.UpstreamTypeEvm,
		Endpoint:   "http://rpc1.localhost",
		VendorName: "memory",
		Evm: &common.EvmUpstreamConfig{ChainId: 123},
	}
	ups, err := registry.NewUpstream(cfg)
	if err != nil {
		panic(err)
	}
	ups.SetNetworkConfig(&common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Alias:        networkID,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	})
	registry.doRegisterBootstrappedUpstream(ups)

	return registry, metricsTracker
}

func TestRepro_HighMethodCardinalityAllocationGrowth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkID := "evm:123"
	registry, _ := buildRegistryWithSmallTopology(ctx, networkID, "test-project")

	totalMethods := sortedMethodMaxPerNetwork*3 + 1024
	for i := 0; i < totalMethods; i++ {
		method := fmt.Sprintf("eth_repro_method_%d", i)
		_, err := registry.GetSortedUpstreams(ctx, networkID, method)
		if !assert.NoError(t, err) {
			return
		}
	}

	assert.Greater(t, totalMethods, sortedMethodMaxPerNetwork)
	err := registry.RefreshUpstreamNetworkMethodScores()
	if !assert.NoError(t, err) {
		return
	}

	registry.RLockUpstreams()
	initialNetworkMethods := len(registry.sortedUpstreams[networkID])
	initialWildcardMethods := len(registry.sortedUpstreams[defaultNetworkMethod])
	registry.RUnlockUpstreams()

	assert.LessOrEqual(t, initialNetworkMethods, sortedMethodMaxPerNetwork)
	if !assert.Equal(t, initialWildcardMethods, initialNetworkMethods, "wildcard scope should mirror network scope entries") {
		return
	}

	// Add a second burst of methods after the first prune cycle.  These should
	// survive in the in-memory cache until the next refresh.
	burstMethods := sortedMethodMaxPerNetwork
	for i := 0; i < burstMethods; i++ {
		method := fmt.Sprintf("eth_burst_method_%d", i)
		_, err := registry.GetSortedUpstreams(ctx, networkID, method)
		if !assert.NoError(t, err) {
			return
		}
	}

	registry.RLockUpstreams()
	burstNetworkMethods := len(registry.sortedUpstreams[networkID])
	burstWildcardMethods := len(registry.sortedUpstreams[defaultNetworkMethod])
	registry.RUnlockUpstreams()

	assert.Greater(t, burstNetworkMethods, initialNetworkMethods)
	assert.Greater(t, burstWildcardMethods, initialWildcardMethods)

	old := time.Now().Add(-3 * sortedMethodUsageTTL)
	registry.sortedUpstreamsMethodUsage.Range(func(key, value any) bool {
		registry.sortedUpstreamsMethodUsage.Store(key, old)
		return true
	})

	now := time.Now()
	for i := 0; i < 64; i++ {
		method := fmt.Sprintf("eth_repro_method_%d", i)
		registry.touchMethodUsage(now, networkID, method)
		registry.touchMethodUsage(now, defaultNetworkMethod, method)
	}

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	for i := 0; i < 200; i++ {
		err := registry.RefreshUpstreamNetworkMethodScores()
		if !assert.NoError(t, err) {
			return
		}
	}

	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)

	registry.RLockUpstreams()
	postNetworkMethods := 0
	postWildcardMethods := 0
	if networkMethods, ok := registry.sortedUpstreams[networkID]; ok {
		postNetworkMethods = len(networkMethods)
	}
	if wildcardMethods, ok := registry.sortedUpstreams[defaultNetworkMethod]; ok {
		postWildcardMethods = len(wildcardMethods)
	}
	registry.RUnlockUpstreams()

	t.Logf("method entries: beforeNetwork=%d beforeWildcard=%d afterNetwork=%d afterWildcard=%d", initialNetworkMethods, initialWildcardMethods, postNetworkMethods, postWildcardMethods)
	t.Logf("mem alloc bytes: before=%d after=%d allocDelta=%d", before.Alloc, after.Alloc, int64(after.Alloc)-int64(before.Alloc))

	assert.LessOrEqual(t, postNetworkMethods, sortedMethodMaxPerNetwork)
	assert.LessOrEqual(t, postWildcardMethods, sortedMethodMaxPerNetwork)
	assert.Less(t, int64(after.Alloc)-int64(before.Alloc), int64(150*1024*1024), "steady-state refresh loop should not allocate more than 150MB in this scope")
}
