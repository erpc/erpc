package telemetry

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNetworkUpstreamCallsPerRequestUsesTrimmedBucketBudget(t *testing.T) {
	MetricNetworkUpstreamCallsPerRequest.Reset()

	MetricNetworkUpstreamCallsPerRequest.WithLabelValues(
		"project-a",
		"evm:1",
		"eth_call",
		MetricsVariantLabel(),
		MetricsReleaseLabel(),
	).Observe(2)

	require.Equal(t, 5, metricHistogramBucketCountByName(t, "erpc_network_upstream_calls_per_request"))
}

func TestNetworkEvmBlockRangeRequestedOmitsVendorAndUpstreamLabels(t *testing.T) {
	MetricNetworkEvmBlockRangeRequested.Reset()

	MetricNetworkEvmBlockRangeRequested.WithLabelValues(
		"project-a",
		"evm:1",
		"eth_getBlockByNumber",
		"user-a",
		"finalized",
		"TIP",
		"100",
	).Inc()

	labels := metricLabelsByName(t, "erpc_network_evm_block_range_requested_total")
	require.Equal(t, "eth_getBlockByNumber", labels["category"])
	require.Equal(t, "TIP", labels["bucket"])
	require.Equal(t, "100", labels["size"])
	require.NotContains(t, labels, "vendor")
	require.NotContains(t, labels, "upstream")
}
