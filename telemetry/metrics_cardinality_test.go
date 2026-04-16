package telemetry

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func metricFamilyByName(t *testing.T, familyName string) *dto.MetricFamily {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != familyName {
			continue
		}
		return family
	}

	t.Fatalf("metric family %s not found", familyName)
	return nil
}

func metricLabelsByName(t *testing.T, familyName string) map[string]string {
	t.Helper()

	family := metricFamilyByName(t, familyName)
	require.NotEmpty(t, family.Metric, "metric family %s has no samples", familyName)
	labels := make(map[string]string, len(family.Metric[0].Label))
	for _, pair := range family.Metric[0].Label {
		labels[pair.GetName()] = pair.GetValue()
	}
	return labels
}

func metricHistogramBucketCountByName(t *testing.T, familyName string) int {
	t.Helper()

	family := metricFamilyByName(t, familyName)
	require.NotEmpty(t, family.Metric, "metric family %s has no samples", familyName)
	histogram := family.Metric[0].GetHistogram()
	require.NotNil(t, histogram, "metric family %s is not a histogram", familyName)
	return len(histogram.Bucket)
}

func TestUpstreamRequestDurationOmitsUserLabel(t *testing.T) {
	require.NoError(t, SetHistogramBuckets(""))
	MetricUpstreamRequestDuration.Reset()

	MetricUpstreamRequestDuration.WithLabelValues(
		"project-a",
		"vendor-a",
		"evm:1",
		"upstream-a",
		"eth_call",
		"none",
		"finalized",
	).Observe(0.1)

	labels := metricLabelsByName(t, "erpc_upstream_request_duration_seconds")
	require.Equal(t, "upstream-a", labels["upstream"])
	require.Equal(t, "vendor-a", labels["vendor"])
	require.NotContains(t, labels, "user")
}

func TestNetworkRequestDurationUsesOutcomeInsteadOfHighCardinalityLabels(t *testing.T) {
	require.NoError(t, SetHistogramBuckets(""))
	MetricNetworkRequestDuration.Reset()

	MetricNetworkRequestDuration.WithLabelValues(
		"project-a",
		"evm:1",
		"eth_call",
		"finalized",
		"cache",
	).Observe(0.1)

	labels := metricLabelsByName(t, "erpc_network_request_duration_seconds")
	require.Equal(t, "cache", labels["outcome"])
	require.NotContains(t, labels, "vendor")
	require.NotContains(t, labels, "upstream")
	require.NotContains(t, labels, "user")
}

func TestCacheDurationHistogramsOmitUserLabel(t *testing.T) {
	require.NoError(t, SetHistogramBuckets(""))

	MetricCacheSetSuccessDuration.Reset()
	MetricCacheSetErrorDuration.Reset()
	MetricCacheGetSuccessHitDuration.Reset()
	MetricCacheGetSuccessMissDuration.Reset()
	MetricCacheGetErrorDuration.Reset()

	MetricCacheSetSuccessDuration.WithLabelValues(
		"project-a",
		"evm:1",
		"eth_call",
		"redis-connector",
		"policy-a",
		"5s",
	).Observe(0.1)
	MetricCacheSetErrorDuration.WithLabelValues(
		"project-a",
		"evm:1",
		"eth_call",
		"redis-connector",
		"policy-a",
		"5s",
		"ContextCanceled",
	).Observe(0.1)
	MetricCacheGetSuccessHitDuration.WithLabelValues(
		"project-a",
		"evm:1",
		"eth_call",
		"redis-connector",
		"policy-a",
		"5s",
	).Observe(0.1)
	MetricCacheGetSuccessMissDuration.WithLabelValues(
		"project-a",
		"evm:1",
		"eth_call",
		"redis-connector",
		"policy-a",
		"5s",
	).Observe(0.1)
	MetricCacheGetErrorDuration.WithLabelValues(
		"project-a",
		"evm:1",
		"eth_call",
		"redis-connector",
		"policy-a",
		"5s",
		"ErrRecordNotFound",
	).Observe(0.1)

	successLabels := metricLabelsByName(t, "erpc_cache_set_success_duration_seconds")
	require.Equal(t, "redis-connector", successLabels["connector"])
	require.NotContains(t, successLabels, "user")

	setErrorLabels := metricLabelsByName(t, "erpc_cache_set_error_duration_seconds")
	require.Equal(t, "ContextCanceled", setErrorLabels["error"])
	require.NotContains(t, setErrorLabels, "user")

	hitLabels := metricLabelsByName(t, "erpc_cache_get_success_hit_duration_seconds")
	require.Equal(t, "policy-a", hitLabels["policy"])
	require.NotContains(t, hitLabels, "user")

	missLabels := metricLabelsByName(t, "erpc_cache_get_success_miss_duration_seconds")
	require.Equal(t, "5s", missLabels["ttl"])
	require.NotContains(t, missLabels, "user")

	getErrorLabels := metricLabelsByName(t, "erpc_cache_get_error_duration_seconds")
	require.Equal(t, "ErrRecordNotFound", getErrorLabels["error"])
	require.NotContains(t, getErrorLabels, "user")
}

func TestNetworkEvmGetLogsRangeRequestedOmitsUserLabel(t *testing.T) {
	MetricNetworkEvmGetLogsRangeRequested.Reset()

	MetricNetworkEvmGetLogsRangeRequested.WithLabelValues(
		"project-a",
		"evm:1",
		"eth_getLogs",
		"finalized",
	).Observe(16)

	labels := metricLabelsByName(t, "erpc_network_evm_get_logs_range_requested")
	require.Equal(t, "eth_getLogs", labels["category"])
	require.Equal(t, "finalized", labels["finality"])
	require.NotContains(t, labels, "user")
}

func TestHotRequestCountersKeepUserButDropAgentName(t *testing.T) {
	testCases := []struct {
		name       string
		familyName string
		reset      func()
		observe    func()
	}{
		{
			name:       "upstream request total",
			familyName: "erpc_upstream_request_total",
			reset:      MetricUpstreamRequestTotal.Reset,
			observe: func() {
				MetricUpstreamRequestTotal.WithLabelValues(
					"project-a",
					"vendor-a",
					"evm:1",
					"upstream-a",
					"eth_call",
					"1",
					"none",
					"finalized",
					"user-a",
				).Inc()
			},
		},
		{
			name:       "upstream error total",
			familyName: "erpc_upstream_request_errors_total",
			reset:      MetricUpstreamErrorTotal.Reset,
			observe: func() {
				MetricUpstreamErrorTotal.WithLabelValues(
					"project-a",
					"vendor-a",
					"evm:1",
					"upstream-a",
					"eth_call",
					"ErrEndpointTimeout",
					"warning",
					"none",
					"finalized",
					"user-a",
				).Inc()
			},
		},
		{
			name:       "network success total",
			familyName: "erpc_network_successful_request_total",
			reset:      MetricNetworkSuccessfulRequests.Reset,
			observe: func() {
				MetricNetworkSuccessfulRequests.WithLabelValues(
					"project-a",
					"evm:1",
					"vendor-a",
					"upstream-a",
					"eth_call",
					"1",
					"finalized",
					"false",
					"user-a",
				).Inc()
			},
		},
		{
			name:       "network failed total",
			familyName: "erpc_network_failed_request_total",
			reset:      MetricNetworkFailedRequests.Reset,
			observe: func() {
				MetricNetworkFailedRequests.WithLabelValues(
					"project-a",
					"evm:1",
					"eth_call",
					"1",
					"ErrEndpointTimeout",
					"warning",
					"finalized",
					"user-a",
				).Inc()
			},
		},
		{
			name:       "rate limits total",
			familyName: "erpc_rate_limits_total",
			reset:      MetricRateLimitsTotal.Reset,
			observe: func() {
				MetricRateLimitsTotal.WithLabelValues(
					"project-a",
					"evm:1",
					"vendor-a",
					"upstream-a",
					"eth_call",
					"finalized",
					"user-a",
					"budget-a",
					"network",
					"jwt",
					"project",
				).Inc()
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.reset()
			tc.observe()

			labels := metricLabelsByName(t, tc.familyName)
			require.Equal(t, "user-a", labels["user"])
			require.NotContains(t, labels, "agent_name")
		})
	}
}

func TestNetworkHedgeDelayHistogramUsesTrimmedBucketBudget(t *testing.T) {
	MetricNetworkHedgeDelaySeconds.Reset()

	MetricNetworkHedgeDelaySeconds.WithLabelValues(
		"project-a",
		"evm:1",
		"eth_call",
		"finalized",
	).Observe(0.1)

	require.Equal(t, 5, metricHistogramBucketCountByName(t, "erpc_network_hedge_delay_seconds"))
}
