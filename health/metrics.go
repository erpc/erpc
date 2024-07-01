package health

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	MetricUpstreamRequestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_total",
		Help:      "Total number of requests to upstreams.",
	}, []string{"project", "network", "upstream", "category"})

	MetricUpstreamRequestDuration = promauto.NewSummaryVec(prometheus.SummaryOpts{
		Namespace: "erpc",
		Name:      "upstream_request_duration_seconds",
		Help:      "Duration of requests to upstreams.",
		Objectives: map[float64]float64{
			0.5:  0.05,  // 50th percentile with a max. absolute error of 0.05.
			0.9:  0.01,  // 90th percentile with a max. absolute error of 0.01.
			0.99: 0.001, // 99th percentile with a max. absolute error of 0.001.
		},
	}, []string{"project", "network", "upstream", "category"})

	MetricUpstreamRequestErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_errors_total",
		Help:      "Total number of errors for requests to upstreams.",
	}, []string{"project", "network", "upstream", "category", "error"})

	MetricUpstreamRequestLocalRateLimited = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_local_rate_limited_total",
		Help:      "Total number of locally rate limited requests to upstreams.",
	}, []string{"project", "network", "upstream", "category"})

	MetricNetworkRequestLocalRateLimited = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_request_local_rate_limited_total",
		Help:      "Total number of locally rate limited requests to network.",
	}, []string{"project", "network", "category"})

	MetricNetworkRequestsReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_requests_received_total",
		Help:      "Total number of requests received for a network.",
	}, []string{"project", "network", "category"})

	MetricNetworkMultiplexedRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_multiplexed_requests_total",
		Help:      "Total number of multiplexed requests for a network.",
	}, []string{"project", "network", "category"})

	MetricNetworkFailedRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_failed_requests_total",
		Help:      "Total number of failed requests for a network.",
	}, []string{"project", "network", "category", "error"})

	MetricNetworkSuccessfulRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_successful_requests_total",
		Help:      "Total number of successful requests for a network.",
	}, []string{"project", "network", "category"})

	MetricNetworkCacheHits = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_cache_hits_total",
		Help:      "Total number of cache hits for a network.",
	}, []string{"project", "network", "category"})

	MetricNetworkCacheMisses = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_cache_misses_total",
		Help:      "Total number of cache misses for a network.",
	}, []string{"project", "network", "category"})

	MetricNetworkRequestDuration = promauto.NewSummaryVec(prometheus.SummaryOpts{
		Namespace: "erpc",
		Name:      "network_request_duration_seconds",
		Help:      "Duration of requests for a network.",
	}, []string{"project", "network", "category"})
)
