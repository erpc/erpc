package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	MetricUnexpectedPanicTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "unexpected_panic_total",
		Help:      "Total number of unexpected panics.",
	}, []string{"scope", "extra", "error"})

	MetricUpstreamRequestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_total",
		Help:      "Total number of actual requests to upstreams.",
	}, []string{"project", "network", "upstream", "category", "attempt"})

	MetricUpstreamRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "upstream_request_duration_seconds",
		Help:      "Duration of actual requests towards upstreams.",
		Buckets: []float64{
			0.005, // 5 ms
			0.01,  // 10 ms
			0.025, // 25 ms
			0.05,  // 50 ms
			0.1,   // 100 ms
			0.25,  // 250 ms
			0.5,   // 500 ms
			1,     // 1 s
			2.5,   // 2.5 s
			5,     // 5 s
			10,    // 10 s
			30,    // 30 s
			60,    // 60 s
			300,   // 5 min
		},
	}, []string{"project", "network", "upstream", "category"})

	MetricUpstreamErrorTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_errors_total",
		Help:      "Total number of errors for actual requests towards upstreams.",
	}, []string{"project", "network", "upstream", "category", "error", "severity"})

	MetricUpstreamSelfRateLimitedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_self_rate_limited_total",
		Help:      "Total number of self-imposed rate limited requests before sending to upstreams.",
	}, []string{"project", "network", "upstream", "category"})

	MetricUpstreamRemoteRateLimitedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_remote_rate_limited_total",
		Help:      "Total number of remote rate limited requests by upstreams.",
	}, []string{"project", "network", "upstream", "category"})

	MetricUpstreamSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_skipped_total",
		Help:      "Total number of requests skipped by upstreams.",
	}, []string{"project", "network", "upstream", "category"})

	MetricUpstreamMissingDataErrorTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_missing_data_error_total",
		Help:      "Total number of requests where upstream is missing data or not synced yet.",
	}, []string{"project", "network", "upstream", "category"})

	MetricUpstreamEmptyResponseTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_empty_response_total",
		Help:      "Total number of empty responses from upstreams.",
	}, []string{"project", "network", "upstream", "category"})

	MetricUpstreamBlockHeadLag = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_block_head_lag",
		Help:      "Total number of blocks (head) behind the most up-to-date upstream.",
	}, []string{"project", "network", "upstream"})

	MetricUpstreamFinalizationLag = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_finalization_lag",
		Help:      "Total number of finalized blocks behind the most up-to-date upstream.",
	}, []string{"project", "network", "upstream"})

	MetricUpstreamScoreOverall = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_score_overall",
		Help:      "Overall score of upstreams used for ordering during routing.",
	}, []string{"project", "network", "upstream", "category"})

	MetricUpstreamLatestBlockNumber = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_latest_block_number",
		Help:      "Latest block number of upstreams.",
	}, []string{"project", "network", "upstream"})

	MetricUpstreamFinalizedBlockNumber = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_finalized_block_number",
		Help:      "Finalized block number of upstreams.",
	}, []string{"project", "network", "upstream"})

	MetricUpstreamCordoned = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_cordoned",
		Help:      "Whether upstream is un/cordoned (excluded from routing by selection policy).",
	}, []string{"project", "network", "upstream", "category"})

	MetricUpstreamStaleLatestBlock = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_stale_latest_block_total",
		Help:      "Total number of times an upstream returned a stale (vs others) latest block number.",
	}, []string{"project", "network", "upstream", "category"})

	MetricUpstreamStaleFinalizedBlock = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_stale_finalized_block_total",
		Help:      "Total number of times an upstream returned a stale (vs others) finalized block number.",
	}, []string{"project", "network", "upstream", "category"})

	MetricUpstreamEvmGetLogsStaleUpperBound = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_evm_get_logs_stale_upper_bound_total",
		Help:      "Total number of times eth_getLogs was skipped due to upstream latest block being less than requested toBlock.",
	}, []string{"project", "network", "upstream"})

	MetricUpstreamEvmGetLogsStaleLowerBound = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_evm_get_logs_stale_lower_bound_total",
		Help:      "Total number of times eth_getLogs was skipped due to fromBlock being less than upstream's available block range.",
	}, []string{"project", "network", "upstream"})

	MetricUpstreamEvmGetLogsRangeExceededAutoSplittingThreshold = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_evm_get_logs_range_exceeded_auto_splitting_threshold_total",
		Help:      "Total number of times eth_getLogs request exceeded the block range threshold and needed splitting.",
	}, []string{"project", "network", "upstream"})

	MetricUpstreamEvmGetLogsSplitSuccess = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_evm_get_logs_split_success_total",
		Help:      "Total number of successful split eth_getLogs sub-requests.",
	}, []string{"project", "network", "upstream"})

	MetricUpstreamEvmGetLogsSplitFailure = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_evm_get_logs_split_failure_total",
		Help:      "Total number of failed split eth_getLogs sub-requests.",
	}, []string{"project", "network", "upstream"})

	MetricUpstreamEvmGetLogsForcedSplits = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_evm_get_logs_forced_splits_total",
		Help:      "Total number of eth_getLogs request splits due to upstream complain by dimension (block_range, addresses, topics).",
	}, []string{"project", "network", "upstream", "dimension"})

	MetricUpstreamLatestBlockPolled = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_latest_block_polled_total",
		Help:      "Total number of times the latest block was pro-actively polled from an upstream.",
	}, []string{"project", "network", "upstream"})

	MetricUpstreamFinalizedBlockPolled = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_finalized_block_polled_total",
		Help:      "Total number of times the finalized block was pro-actively polled from an upstream.",
	}, []string{"project", "network", "upstream"})

	MetricNetworkRequestSelfRateLimited = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_request_self_rate_limited_total",
		Help:      "Total number of self-imposed (locally) rate limited requests towards the network.",
	}, []string{"project", "network", "category"})

	MetricNetworkRequestsReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_request_received_total",
		Help:      "Total number of requests received for a network.",
	}, []string{"project", "network", "category"})

	MetricNetworkMultiplexedRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_multiplexed_request_total",
		Help:      "Total number of multiplexed requests for a network.",
	}, []string{"project", "network", "category"})

	MetricNetworkHedgedRequestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_hedged_request_total",
		Help:      "Total number of hedged requests towards a network.",
	}, []string{"project", "network", "upstream", "category", "attempt"})

	MetricNetworkHedgeDiscardsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_hedge_discards_total",
		Help:      "Total number of hedged requests discarded towards a network (i.e. attempt > 1 means wasted requests).",
	}, []string{"project", "network", "upstream", "category", "attempt", "hedge"})

	MetricNetworkFailedRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_failed_request_total",
		Help:      "Total number of failed requests for a network.",
	}, []string{"project", "network", "category", "attempt", "error"})

	MetricNetworkSuccessfulRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_successful_request_total",
		Help:      "Total number of successful requests for a network.",
	}, []string{"project", "network", "category", "attempt"})

	MetricNetworkRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "network_request_duration_seconds",
		Help:      "Duration of requests for a network.",
		Buckets: []float64{
			0.005, // 5 ms
			0.01,  // 10 ms
			0.025, // 25 ms
			0.05,  // 50 ms
			0.1,   // 100 ms
			0.25,  // 250 ms
			0.5,   // 500 ms
			1,     // 1 s
			2.5,   // 2.5 s
			5,     // 5 s
			10,    // 10 s
			30,    // 30 s
			60,    // 60 s
			300,   // 5 min
		},
	}, []string{"project", "network", "category"})

	MetricProjectRequestSelfRateLimited = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "project_request_self_rate_limited_total",
		Help:      "Total number of self-imposed (locally) rate limited requests towards the project.",
	}, []string{"project", "category"})

	MetricRateLimiterBudgetMaxCount = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "rate_limiter_budget_max_count",
		Help:      "Maximum number of requests allowed per second for a rate limiter budget (including auto-tuner).",
	}, []string{"budget", "method"})

	MetricAuthRequestSelfRateLimited = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "auth_request_self_rate_limited_total",
		Help:      "Total number of self-imposed (locally) rate limited requests due to auth config for a project.",
	}, []string{"project", "strategy", "category"})

	MetricCacheSetSuccessTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_set_success_total",
		Help:      "Total number of cache set operations.",
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheSetSuccessDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_set_success_duration_seconds",
		Help:      "Duration of cache set operations.",
		Buckets: []float64{
			0.05, // 50 ms
			0.1,  // 100 ms
			0.25, // 250 ms
			0.5,  // 500 ms
			1,    // 1 s
			5,    // 5 s
			10,   // 10 s
			30,   // 30 s
		},
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheSetErrorTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_set_error_total",
		Help:      "Total number of cache set errors.",
	}, []string{"project", "network", "category", "connector", "policy", "ttl", "error"})

	MetricCacheSetErrorDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_set_error_duration_seconds",
		Help:      "Duration of cache set errors.",
		Buckets: []float64{
			0.05, // 50 ms
			0.1,  // 100 ms
			0.25, // 250 ms
			0.5,  // 500 ms
			1,    // 1 s
			5,    // 5 s
			10,   // 10 s
			30,   // 30 s
		},
	}, []string{"project", "network", "category", "connector", "policy", "ttl", "error"})

	MetricCacheSetSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_set_skipped_total",
		Help:      "Total number of cache set skips.",
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheGetSuccessHitTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_get_success_hit_total",
		Help:      "Total number of cache get hits.",
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheGetSuccessHitDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_get_success_hit_duration_seconds",
		Help:      "Duration of cache get hits.",
		Buckets: []float64{
			0.05, // 50 ms
			0.1,  // 100 ms
			0.25, // 250 ms
			0.5,  // 500 ms
			1,    // 1 s
			5,    // 5 s
			10,   // 10 s
			30,   // 30 s
		},
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheGetSuccessMissTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_get_success_miss_total",
		Help:      "Total number of cache get misses.",
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheGetSuccessMissDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_get_success_miss_duration_seconds",
		Help:      "Duration of cache get misses.",
		Buckets: []float64{
			0.05, // 50 ms
			0.1,  // 100 ms
			0.25, // 250 ms
			0.5,  // 500 ms
			1,    // 1 s
			5,    // 5 s
			10,   // 10 s
			30,   // 30 s
		},
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheGetErrorTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_get_error_total",
		Help:      "Total number of cache get errors.",
	}, []string{"project", "network", "category", "connector", "policy", "ttl", "error"})

	MetricCacheGetErrorDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_get_error_duration_seconds",
		Help:      "Duration of cache get errors.",
		Buckets: []float64{
			0.05, // 50 ms
			0.1,  // 100 ms
			0.25, // 250 ms
			0.5,  // 500 ms
			1,    // 1 s
			5,    // 5 s
			10,   // 10 s
			30,   // 30 s
		},
	}, []string{"project", "network", "category", "connector", "policy", "ttl", "error"})

	MetricCacheGetSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_get_skipped_total",
		Help:      "Total number of cache get skips (i.e. no matching policy found).",
	}, []string{"project", "network", "category"})

	MetricCORSRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cors_requests_total",
		Help:      "Total number of CORS requests received.",
	}, []string{"project", "origin"})

	MetricCORSPreflightRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cors_preflight_requests_total",
		Help:      "Total number of CORS preflight requests received.",
	}, []string{"project", "origin"})

	MetricCORSDisallowedOriginTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cors_disallowed_origin_total",
		Help:      "Total number of CORS requests from disallowed origins.",
	}, []string{"project", "origin"})
)
