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
		if family.GetName() == familyName {
			return family
		}
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
