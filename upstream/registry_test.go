package upstream

import (
	"context"
	"testing"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/vendors"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestUpstreamsRegistry(t *testing.T) {
	logger := zerolog.New(zerolog.NewConsoleWriter())
	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_call"
	windowsSize := 2 * time.Second
	refreshInterval := 1 * time.Second

	t.Run("ErrorRateAffectsScoreAndOrder", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowsSize, refreshInterval)
		// Initialize score tracking for this method
		_, _ = registry.GetSortedUpstreams(networkID, method)

		// Simulate requests and errors for upstream A, B, and C
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 20)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 10)

		// Force score refresh
		registry.refreshUpstreamNetworkMethodScores()

		// Get sorted upstreams
		sortedUpstreams, err := registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)
		assert.Equal(t, "upstream-c", sortedUpstreams[0].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams[1].Config().Id)
		assert.Equal(t, "upstream-b", sortedUpstreams[2].Config().Id)
	})

	t.Run("P90LatencyAffectsSortOrder", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowsSize, refreshInterval)
		// Initialize score tracking for this method
		_, _ = registry.GetSortedUpstreams(networkID, method)

		// Simulate requests with different latencies
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method, 10, 0.05)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method, 10, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method, 10, 0.01)

		// Force score refresh for latency
		time.Sleep(refreshInterval)
		registry.refreshUpstreamNetworkMethodScores()

		// Get sorted upstreams
		sortedUpstreams, err := registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)
		assert.Equal(t, "upstream-c", sortedUpstreams[0].Config().Id)
		assert.Equal(t, "upstream-b", sortedUpstreams[1].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams[2].Config().Id)
	})

	t.Run("DynamicErrorRateChangesAffectOrder", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowsSize, refreshInterval)
		// Initialize score tracking for this method
		_, _ = registry.GetSortedUpstreams(networkID, method)

		// Initial phase: A has more errors than B
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 20)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 10)

		registry.refreshUpstreamNetworkMethodScores()

		sortedUpstreams, err := registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)
		assert.Equal(t, "upstream-c", sortedUpstreams[0].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams[1].Config().Id)
		assert.Equal(t, "upstream-b", sortedUpstreams[2].Config().Id)

		// Simulate time passing and metrics reset
		time.Sleep(windowsSize)

		// Second phase: B now has more errors than A
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 15)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 5)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 20)

		registry.refreshUpstreamNetworkMethodScores()

		sortedUpstreams, err = registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)
		assert.Equal(t, "upstream-b", sortedUpstreams[0].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams[1].Config().Id)
		assert.Equal(t, "upstream-c", sortedUpstreams[2].Config().Id)
	})

	t.Run("RateLimitingAffectsScore", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowsSize, refreshInterval)
		// Initialize score tracking for this method
		_, _ = registry.GetSortedUpstreams(networkID, method)
		
		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-a", method, 100, 10, 5)
		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-b", method, 100, 5, 2)
		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-c", method, 100, 2, 1)

		registry.refreshUpstreamNetworkMethodScores()

		sortedUpstreams, err := registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)
		assert.Equal(t, "upstream-c", sortedUpstreams[0].Config().Id)
		assert.Equal(t, "upstream-b", sortedUpstreams[1].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams[2].Config().Id)
	})

	t.Run("TotalRequestsAffectLoadBalancing", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowsSize, refreshInterval)
		// Initialize score tracking for this method
		_, _ = registry.GetSortedUpstreams(networkID, method)

		// Simulate different number of requests for each upstream
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 50, 0)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 0)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 25, 0)

		registry.refreshUpstreamNetworkMethodScores()

		sortedUpstreams, err := registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)
		assert.Equal(t, "upstream-c", sortedUpstreams[0].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams[1].Config().Id)
		assert.Equal(t, "upstream-b", sortedUpstreams[2].Config().Id)
	})
}

func createTestRegistry(projectID string, logger *zerolog.Logger, windowsSize, refreshInterval time.Duration) (*UpstreamsRegistry, *health.Tracker) {
	metricsTracker := health.NewTracker(projectID, windowsSize, refreshInterval)
	metricsTracker.Bootstrap(context.Background())

	upstreamConfigs := []*common.UpstreamConfig{
		{Id: "upstream-a", Endpoint: "http://upstream-a.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "upstream-b", Endpoint: "http://upstream-b.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "upstream-c", Endpoint: "http://upstream-c.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	}

	registry := NewUpstreamsRegistry(
		logger,
		projectID,
		upstreamConfigs,
		nil, // RateLimitersRegistry not needed for these tests
		vendors.NewVendorsRegistry(),
		metricsTracker,
	)

	err := registry.Bootstrap(context.Background())
	if err != nil {
		panic(err)
	}

	err = registry.PrepareUpstreamsForNetwork("evm:123")
	if err != nil {
		panic(err)
	}

	return registry, metricsTracker
}

func simulateRequests(tracker *health.Tracker, network, upstream, method string, total, errors int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(network, upstream, method)
		if i < errors {
			tracker.RecordUpstreamFailure(network, upstream, method, "test-error")
		}
		timer := tracker.RecordUpstreamDurationStart(network, upstream, method)
		timer.ObserveDuration()
	}
}

func simulateRequestsWithRateLimiting(tracker *health.Tracker, network, upstream, method string, total, selfLimited, remoteLimited int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(network, upstream, method)
		if i < selfLimited {
			tracker.RecordUpstreamSelfRateLimited(network, upstream, method)
		}
		if i >= selfLimited && i < selfLimited+remoteLimited {
			tracker.RecordUpstreamRemoteRateLimited(network, upstream, method)
		}
		timer := tracker.RecordUpstreamDurationStart(network, upstream, method)
		timer.ObserveDuration()
	}
}

func simulateRequestsWithLatency(tracker *health.Tracker, network, upstream, method string, total int, latency float64) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(network, upstream, method)
		timer := tracker.RecordUpstreamDurationStart(network, upstream, method)
		time.Sleep(time.Duration(latency * float64(time.Second)))
		timer.ObserveDuration()
	}
}
