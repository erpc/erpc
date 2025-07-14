package telemetry

import (
	"sort"
	"strconv"
	"strings"

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
	}, []string{"project", "vendor", "network", "upstream", "category", "attempt", "composite", "finality"})

	MetricUpstreamErrorTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_errors_total",
		Help:      "Total number of errors for actual requests towards upstreams.",
	}, []string{"project", "vendor", "network", "upstream", "category", "error", "severity", "composite", "finality"})

	MetricUpstreamSelfRateLimitedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_self_rate_limited_total",
		Help:      "Total number of self-imposed rate limited requests before sending to upstreams.",
	}, []string{"project", "vendor", "network", "upstream", "category"})

	MetricUpstreamRemoteRateLimitedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_remote_rate_limited_total",
		Help:      "Total number of remote rate limited requests by upstreams.",
	}, []string{"project", "vendor", "network", "upstream", "category"})

	MetricUpstreamSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_skipped_total",
		Help:      "Total number of requests skipped by upstreams.",
	}, []string{"project", "vendor", "network", "upstream", "category", "finality"})

	MetricUpstreamMissingDataErrorTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_missing_data_error_total",
		Help:      "Total number of requests where upstream is missing data or not synced yet.",
	}, []string{"project", "vendor", "network", "upstream", "category", "finality"})

	MetricUpstreamEmptyResponseTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_empty_response_total",
		Help:      "Total number of empty responses from upstreams.",
	}, []string{"project", "vendor", "network", "upstream", "category", "finality"})

	MetricUpstreamBlockHeadLag = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_block_head_lag",
		Help:      "Total number of blocks (head) behind the most up-to-date upstream.",
	}, []string{"project", "vendor", "network", "upstream"})

	MetricUpstreamFinalizationLag = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_finalization_lag",
		Help:      "Total number of finalized blocks behind the most up-to-date upstream.",
	}, []string{"project", "vendor", "network", "upstream"})

	MetricUpstreamScoreOverall = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_score_overall",
		Help:      "Overall score of upstreams used for ordering during routing.",
	}, []string{"project", "vendor", "network", "upstream", "category"})

	MetricUpstreamLatestBlockNumber = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_latest_block_number",
		Help:      "Latest block number of upstreams.",
	}, []string{"project", "vendor", "network", "upstream"})

	MetricUpstreamFinalizedBlockNumber = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_finalized_block_number",
		Help:      "Finalized block number of upstreams.",
	}, []string{"project", "vendor", "network", "upstream"})

	MetricUpstreamCordoned = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_cordoned",
		Help:      "Whether upstream is un/cordoned (excluded from routing by selection policy).",
	}, []string{"project", "vendor", "network", "upstream", "category"})

	MetricUpstreamStaleLatestBlock = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_stale_latest_block_total",
		Help:      "Total number of times an upstream returned a stale (vs others) latest block number.",
	}, []string{"project", "vendor", "network", "upstream", "category"})

	MetricUpstreamStaleFinalizedBlock = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_stale_finalized_block_total",
		Help:      "Total number of times an upstream returned a stale (vs others) finalized block number.",
	}, []string{"project", "vendor", "network", "upstream"})

	MetricUpstreamStaleUpperBound = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_stale_upper_bound_total",
		Help:      "Total number of times a request was skipped due to upstream latest block being less than requested upper bound block.",
	}, []string{"project", "vendor", "network", "upstream", "category", "confidence"})

	MetricUpstreamStaleLowerBound = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_stale_lower_bound_total",
		Help:      "Total number of times a request was skipped due to requested lower bound block being less than upstream's available block range.",
	}, []string{"project", "vendor", "network", "upstream", "category", "confidence"})

	MetricUpstreamEvmGetLogsRangeExceededAutoSplittingThreshold = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_evm_get_logs_range_exceeded_auto_splitting_threshold_total",
		Help:      "Total number of times eth_getLogs request exceeded the block range threshold and needed splitting.",
	}, []string{"project", "vendor", "network", "upstream"})

	MetricUpstreamEvmGetLogsSplitSuccess = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_evm_get_logs_split_success_total",
		Help:      "Total number of successful split eth_getLogs sub-requests.",
	}, []string{"project", "vendor", "network", "upstream"})

	MetricUpstreamEvmGetLogsSplitFailure = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_evm_get_logs_split_failure_total",
		Help:      "Total number of failed split eth_getLogs sub-requests.",
	}, []string{"project", "vendor", "network", "upstream"})

	MetricUpstreamEvmGetLogsForcedSplits = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_evm_get_logs_forced_splits_total",
		Help:      "Total number of eth_getLogs request splits due to upstream complain by dimension (block_range, addresses, topics).",
	}, []string{"project", "vendor", "network", "upstream", "dimension"})

	MetricUpstreamLatestBlockPolled = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_latest_block_polled_total",
		Help:      "Total number of times the latest block was pro-actively polled from an upstream.",
	}, []string{"project", "vendor", "network", "upstream"})

	MetricUpstreamFinalizedBlockPolled = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_finalized_block_polled_total",
		Help:      "Total number of times the finalized block was pro-actively polled from an upstream.",
	}, []string{"project", "vendor", "network", "upstream"})

	MetricUpstreamBlockHeadLargeRollback = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_block_head_large_rollback",
		Help:      "Number of times block head rolled back by a large number vs previous latest block returned by the same upstream.",
	}, []string{"project", "vendor", "network", "upstream"})

	MetricUpstreamWrongEmptyResponseTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_wrong_empty_response_total",
		Help:      "Total number of times an upstream returned a wrong empty response even though other upstreams returned data.",
	}, []string{"project", "vendor", "network", "upstream", "category", "finality"})

	MetricNetworkRequestSelfRateLimited = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_request_self_rate_limited_total",
		Help:      "Total number of self-imposed (locally) rate limited requests towards the network.",
	}, []string{"project", "network", "category", "finality"})

	MetricNetworkRequestsReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_request_received_total",
		Help:      "Total number of requests received for a network.",
	}, []string{"project", "network", "category", "finality"})

	MetricNetworkMultiplexedRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_multiplexed_request_total",
		Help:      "Total number of multiplexed requests for a network.",
	}, []string{"project", "network", "category", "finality"})

	MetricNetworkHedgedRequestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_hedged_request_total",
		Help:      "Total number of hedged requests towards a network.",
	}, []string{"project", "network", "upstream", "category", "attempt", "finality"})

	MetricNetworkHedgeDiscardsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_hedge_discards_total",
		Help:      "Total number of hedged requests discarded towards a network (i.e. attempt > 1 means wasted requests).",
	}, []string{"project", "network", "upstream", "category", "attempt", "hedge", "finality"})

	MetricNetworkFailedRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_failed_request_total",
		Help:      "Total number of failed requests for a network.",
	}, []string{"project", "network", "category", "attempt", "error", "severity", "finality"})

	MetricNetworkSuccessfulRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_successful_request_total",
		Help:      "Total number of successful requests for a network.",
	}, []string{"project", "network", "vendor", "upstream", "category", "attempt", "finality", "emptyish"})

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

	MetricCacheSetErrorTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_set_error_total",
		Help:      "Total number of cache set errors.",
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

	MetricCacheGetSuccessMissTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_get_success_miss_total",
		Help:      "Total number of cache get misses.",
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheGetErrorTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_get_error_total",
		Help:      "Total number of cache get errors.",
	}, []string{"project", "network", "category", "connector", "policy", "ttl", "error"})

	MetricCacheGetSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_get_skipped_total",
		Help:      "Total number of cache get skips (i.e. no matching policy found).",
	}, []string{"project", "network", "category"})

	MetricCacheSetOriginalBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_set_original_bytes_total",
		Help:      "Total number of original (uncompressed) bytes for cache set operations.",
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheSetCompressedBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_set_compressed_bytes_total",
		Help:      "Total number of compressed bytes for cache set operations.",
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

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

	MetricRistrettoCacheCurrentCost = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "ristretto_cache_current_cost",
		Help:      "Current total cost (memory usage) of Ristretto cache.",
	}, []string{"connector"})

	MetricRistrettoCacheSetsFailedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "ristretto_cache_sets_failed_total",
		Help:      "Total number of set operations that failed (dropped or rejected) in Ristretto cache.",
	}, []string{"connector"})

	MetricShadowResponseIdenticalTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "shadow_response_identical_total",
		Help:      "Total number of shadow upstream responses identical to the expected response.",
	}, []string{"project", "vendor", "network", "upstream", "category"})

	MetricShadowResponseMismatchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "shadow_response_mismatch_total",
		Help:      "Total number of shadow upstream responses that differ from the expected response.",
	}, []string{"project", "vendor", "network", "upstream", "category", "emptyish", "larger"})

	MetricShadowResponseErrorTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "shadow_response_error_total",
		Help:      "Total number of shadow upstream requests that resulted in error.",
	}, []string{"project", "vendor", "network", "upstream", "category", "error"})

	MetricConsensusTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_total",
		Help:      "Total number of consensus operations attempted.",
	}, []string{"project", "network", "category", "outcome", "finality"}) // outcome: success, consensus_on_error, agreed_error, dispute, low_participants, error

	MetricConsensusAgreementRate = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "consensus_agreement_rate",
		Help:      "Rate of consensus agreements achieved (moving average).",
	}, []string{"project", "network"})

	MetricConsensusResponsesCollected = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "consensus_responses_collected",
		Help:      "Number of responses collected before consensus decision.",
		Buckets:   prometheus.LinearBuckets(1, 1, 10), // 1 to 10 participants
	}, []string{"project", "network", "category", "short_circuited", "finality"})

	MetricConsensusAgreementCount = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "consensus_agreement_count",
		Help:      "Number of upstreams agreeing on the most common result.",
		Buckets:   prometheus.LinearBuckets(1, 1, 10),
	}, []string{"project", "network", "category", "finality"})

	MetricConsensusMisbehaviorDetected = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_misbehavior_detected_total",
		Help:      "Total number of misbehaving upstream detections.",
	}, []string{"project", "network", "upstream", "category", "finality", "emptyish"})

	MetricConsensusUpstreamPunished = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_upstream_punished_total",
		Help:      "Total number of times upstreams were punished.",
	}, []string{"project", "network", "upstream"})

	MetricConsensusShortCircuit = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_short_circuit_total",
		Help:      "Total number of consensus rounds that short-circuited.",
	}, []string{"project", "network", "category", "reason", "finality"}) // reason: consensus_reached, impossible

	MetricConsensusErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_errors_total",
		Help:      "Total number of consensus errors by type.",
	}, []string{"project", "network", "category", "error", "finality"}) // error: low_participants, dispute, agreed_error, consensus_on_error, invalid_upstreams

	MetricConsensusPanics = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_panics_total",
		Help:      "Total number of panic recoveries in consensus.",
	}, []string{"project", "network", "category", "finality"})

	MetricConsensusCancellations = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_cancellations_total",
		Help:      "Total number of context cancellations during consensus.",
	}, []string{"project", "network", "category", "phase", "finality"}) // phase: before_execution, after_execution, collection
)

var slowHistogramBuckets = []float64{
	0.050, // 50ms
	0.100, // 100ms
	0.250, // 250ms
	0.500, // 500ms
	0.700, // 700ms
	0.800, // 800ms
	1,     // 1s
	2,     // 2s
	3,     // 3s
	4,     // 4s
	5,     // 5s
	6,     // 6s
	7,     // 7s
	8,     // 8s
	15,    // 15s
	30,    // 30s
	60,    // 60s
	120,   // 2m
	300,   // 5m
}

var DefaultHistogramBuckets = []float64{
	0.05, // 50 ms
	0.5,  // 500 ms
	5,    // 5 s
	30,   // 30 s
}

var (
	MetricUpstreamRequestDuration,
	MetricNetworkRequestDuration,
	MetricCacheSetSuccessDuration,
	MetricCacheSetErrorDuration,
	MetricCacheGetSuccessHitDuration,
	MetricCacheGetSuccessMissDuration,
	MetricCacheGetErrorDuration,
	MetricConsensusDuration *prometheus.HistogramVec
)

func SetHistogramBuckets(bucketsStr string) error {
	buckets, err := ParseHistogramBuckets(bucketsStr)
	if err != nil {
		return err
	}

	if MetricUpstreamRequestDuration != nil {
		prometheus.DefaultRegisterer.Unregister(MetricUpstreamRequestDuration)
		prometheus.DefaultRegisterer.Unregister(MetricNetworkRequestDuration)
		prometheus.DefaultRegisterer.Unregister(MetricCacheSetSuccessDuration)
		prometheus.DefaultRegisterer.Unregister(MetricCacheSetErrorDuration)
		prometheus.DefaultRegisterer.Unregister(MetricCacheGetSuccessHitDuration)
		prometheus.DefaultRegisterer.Unregister(MetricCacheGetSuccessMissDuration)
		prometheus.DefaultRegisterer.Unregister(MetricCacheGetErrorDuration)
		prometheus.DefaultRegisterer.Unregister(MetricConsensusDuration)
	}
	MetricUpstreamRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "upstream_request_duration_seconds",
		Help:      "Duration of actual requests towards upstreams.",
		Buckets:   buckets,
	}, []string{"project", "vendor", "network", "upstream", "category", "composite", "finality"})

	MetricNetworkRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "network_request_duration_seconds",
		Help:      "Duration of requests for a network.",
		Buckets:   buckets,
	}, []string{"project", "network", "vendor", "upstream", "category", "finality"})

	MetricCacheSetSuccessDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_set_success_duration_seconds",
		Help:      "Duration of cache set operations.",
		Buckets:   buckets,
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheSetErrorDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_set_error_duration_seconds",
		Help:      "Duration of cache set errors.",
		Buckets:   buckets,
	}, []string{"project", "network", "category", "connector", "policy", "ttl", "error"})

	MetricCacheGetSuccessHitDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_get_success_hit_duration_seconds",
		Help:      "Duration of cache get hits.",
		Buckets:   buckets,
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheGetSuccessMissDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_get_success_miss_duration_seconds",
		Help:      "Duration of cache get misses.",
		Buckets:   buckets,
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheGetErrorDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_get_error_duration_seconds",
		Help:      "Duration of cache get errors.",
		Buckets:   buckets,
	}, []string{"project", "network", "category", "connector", "policy", "ttl", "error"})

	MetricConsensusDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "consensus_duration_seconds",
		Help:      "Duration of consensus operations.",
		Buckets:   slowHistogramBuckets,
	}, []string{"project", "network", "category", "outcome", "finality"})

	return nil
}

func ParseHistogramBuckets(bucketsStr string) ([]float64, error) {
	if bucketsStr == "" {
		return DefaultHistogramBuckets, nil
	}

	parts := strings.Split(bucketsStr, ",")
	buckets := make([]float64, 0, len(parts))

	for _, part := range parts {
		value, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil, err
		}
		buckets = append(buckets, value)
	}

	sort.Float64s(buckets)
	return buckets, nil
}
