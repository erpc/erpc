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
	}, []string{"project", "network", "upstream"})

	MetricUpstreamRequestDuration = promauto.NewSummaryVec(prometheus.SummaryOpts{
		Namespace: "erpc",
		Name:      "upstream_request_duration_seconds",
		Help:      "Duration of requests to upstreams.",
		Objectives: map[float64]float64{
			0.5:  0.05,  // 50th percentile with a max. absolute error of 0.05.
			0.9:  0.01,  // 90th percentile with a max. absolute error of 0.01.
			0.99: 0.001, // 99th percentile with a max. absolute error of 0.001.
		},
	}, []string{"project", "network", "upstream"})

	MetricUpstreamRequestErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_errors_total",
		Help:      "Total number of errors for requests to upstreams.",
	}, []string{"project", "network", "upstream"})

	MetricUpstreamRequestLocalRateLimited = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_local_rate_limited_total",
		Help:      "Total number of locally rate limited requests to upstreams.",
	}, []string{"project", "network", "upstream"})
)
