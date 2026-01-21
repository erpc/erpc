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
	}, []string{"project", "vendor", "network", "upstream", "category", "attempt", "composite", "finality", "user", "agent_name"})

	MetricUpstreamErrorTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_errors_total",
		Help:      "Total number of errors for actual requests towards upstreams.",
	}, []string{"project", "vendor", "network", "upstream", "category", "error", "severity", "composite", "finality", "user", "agent_name"})

	MetricUpstreamSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_skipped_total",
		Help:      "Total number of requests skipped by upstreams.",
	}, []string{"project", "vendor", "network", "upstream", "category", "finality", "user", "agent_name"})

	MetricUpstreamMissingDataErrorTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_missing_data_error_total",
		Help:      "Total number of requests where upstream is missing data or not synced yet.",
	}, []string{"project", "vendor", "network", "upstream", "category", "finality", "user", "agent_name"})

	MetricUpstreamEmptyResponseTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_empty_response_total",
		Help:      "Total number of empty responses from upstreams.",
	}, []string{"project", "vendor", "network", "upstream", "category", "finality", "user", "agent_name"})

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

	MetricNetworkLatestBlockTimestampDistance = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "network_latest_block_timestamp_distance_seconds",
		Help:      "Distance in seconds between the latest block timestamp and current time for a network.",
	}, []string{"project", "network", "origin"})

	MetricUpstreamCordoned = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_cordoned",
		Help:      "Whether upstream is un/cordoned (excluded from routing by selection policy).",
	}, []string{"project", "vendor", "network", "upstream", "category", "reason"})

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

	MetricNetworkEvmGetLogsSplitSuccess = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_evm_get_logs_split_success_total",
		Help:      "Total number of successful split eth_getLogs sub-requests (network-scoped).",
	}, []string{"project", "network", "user", "agent_name"})

	MetricNetworkEvmGetLogsSplitFailure = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_evm_get_logs_split_failure_total",
		Help:      "Total number of failed split eth_getLogs sub-requests (network-scoped).",
	}, []string{"project", "network", "user", "agent_name"})

	MetricNetworkEvmGetLogsForcedSplits = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_evm_get_logs_forced_splits_total",
		Help:      "Total number of eth_getLogs request splits by dimension (block_range, addresses, topics), network-scoped.",
	}, []string{"project", "network", "dimension", "user", "agent_name"})

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
	}, []string{"project", "vendor", "network", "upstream", "category", "finality", "user", "agent_name"})

	MetricNetworkRequestsReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_request_received_total",
		Help:      "Total number of requests received for a network.",
	}, []string{"project", "network", "category", "finality", "user", "agent_name"})

	MetricNetworkMultiplexedRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_multiplexed_request_total",
		Help:      "Total number of multiplexed requests for a network.",
	}, []string{"project", "network", "category", "finality", "user", "agent_name"})

	MetricNetworkHedgedRequestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_hedged_request_total",
		Help:      "Total number of hedged requests towards a network.",
	}, []string{"project", "network", "upstream", "category", "attempt", "finality", "user", "agent_name"})

	MetricNetworkHedgeDiscardsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_hedge_discards_total",
		Help:      "Total number of hedged requests discarded towards a network (i.e. attempt > 1 means wasted requests).",
	}, []string{"project", "network", "upstream", "category", "attempt", "hedge", "finality", "user", "agent_name"})

	MetricNetworkHedgeDelaySeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "network_hedge_delay_seconds",
		Help:      "Hedge delay used for requests (seconds).",
		Buckets:   []float64{0.01, 0.03, 0.05, 0.2, 0.3, 0.5, 0.7, 1, 3},
	}, []string{"project", "network", "category", "finality"})

	MetricNetworkFailedRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_failed_request_total",
		Help:      "Total number of failed requests for a network.",
	}, []string{"project", "network", "category", "attempt", "error", "severity", "finality", "user", "agent_name"})

	MetricNetworkSuccessfulRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_successful_request_total",
		Help:      "Total number of successful requests for a network.",
	}, []string{"project", "network", "vendor", "upstream", "category", "attempt", "finality", "emptyish", "user", "agent_name"})

	MetricRateLimitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "rate_limits_total",
		Help:      "Unified rate limiting events (remote limits and budget decisions).",
	}, []string{"project", "network", "vendor", "upstream", "category", "finality", "user", "agent_name", "budget", "scope", "auth", "origin"})

	MetricRateLimiterBudgetMaxCount = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "rate_limiter_budget_max_count",
		Help:      "Maximum number of requests allowed per second for a rate limiter budget (including auto-tuner).",
	}, []string{"budget", "method", "scope"})

	MetricRateLimiterBudgetDecisionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "rate_limiter_budget_decision_total",
		Help:      "[DEPRECATED] Replaced by rate_limits_total. Total number of local rate-limit decisions by budget.",
	}, []string{"project", "network", "category", "finality", "user", "agent_name", "budget", "method", "scope", "decision"})

	MetricRateLimiterFailopenTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "rate_limiter_failopen_total",
		Help:      "Total number of rate limiter fail-open events (requests allowed due to errors/timeouts).",
	}, []string{"project", "network", "user", "agent_name", "budget", "category", "reason"})

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

	MetricCacheGetAgeGuardRejectTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_get_age_guard_reject_total",
		Help:      "Total number of cached items rejected due to block timestamp age exceeding policy TTL",
	}, []string{"project", "network", "method", "connector", "policy", "ttl"})

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
	}, []string{"project", "vendor", "network", "upstream", "category", "finality", "emptyish", "larger"})

	MetricShadowResponseErrorTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "shadow_response_error_total",
		Help:      "Total number of shadow upstream requests that resulted in error.",
	}, []string{"project", "vendor", "network", "upstream", "category", "error"})

	// Authentication metrics
	MetricAuthFailedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "auth_failed_total",
		Help:      "Total number of failed authentication attempts.",
	}, []string{"project", "network", "strategy", "reason", "agent_name"})

	MetricConsensusTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_total",
		Help:      "Total number of consensus operations attempted.",
	}, []string{"project", "network", "category", "outcome", "finality"})

	MetricConsensusResponsesCollected = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "consensus_responses_collected",
		Help:      "Number of responses collected before consensus decision.",
		Buckets:   prometheus.LinearBuckets(1, 1, 10),
	}, []string{"project", "network", "category", "vendors", "short_circuited", "finality"})

	MetricConsensusAgreementCount = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "consensus_agreement_count",
		Help:      "Number of upstreams agreeing on the most common result.",
		Buckets:   prometheus.LinearBuckets(1, 1, 10),
	}, []string{"project", "network", "category", "finality"})

	MetricConsensusMisbehaviorDetected = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_misbehavior_detected_total",
		Help:      "Total number of times an upstream returned different data (not errors) than consensus.",
	}, []string{"project", "network", "upstream", "category", "finality", "response_type", "larger_than_consensus"})

	MetricConsensusUpstreamPunished = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_upstream_punished_total",
		Help:      "Total number of times upstreams were punished.",
	}, []string{"project", "network", "upstream"})

	MetricConsensusShortCircuit = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_short_circuit_total",
		Help:      "Total number of consensus rounds that short-circuited.",
	}, []string{"project", "network", "category", "reason", "finality"})

	MetricConsensusErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_errors_total",
		Help:      "Total number of consensus errors by type.",
	}, []string{"project", "network", "category", "error", "finality"})

	MetricConsensusUpstreamErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_upstream_errors_total",
		Help:      "Total number of errors from upstreams during consensus operations.",
	}, []string{"project", "network", "upstream", "category", "finality", "response_type", "error_code"})

	MetricConsensusPanics = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_panics_total",
		Help:      "Total number of panic recoveries in consensus.",
	}, []string{"project", "network", "category", "finality"})

	MetricConsensusCancellations = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_cancellations_total",
		Help:      "Total number of context cancellations during consensus.",
	}, []string{"project", "network", "category", "phase", "finality"})

	MetricNetworkEvmBlockRangeRequested = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_evm_block_range_requested_total",
		Help:      "Total requests observed by block-number buckets for heatmap.",
	}, []string{"project", "network", "vendor", "upstream", "category", "user", "finality", "bucket", "size"})

	MetricNetworkEvmGetLogsRangeRequested = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "network_evm_get_logs_range_requested",
		Help:      "eth_getLogs requested block-range sizes.",
		Buckets:   EvmGetLogsRangeHistogramBuckets,
	}, []string{"project", "network", "category", "user", "finality"})

	// Multicall3 aggregation metrics
	MetricMulticall3AggregationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "multicall3_aggregation_total",
		Help:      "Total number of multicall3 aggregation attempts.",
	}, []string{"project", "network", "outcome"})

	MetricMulticall3FallbackTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "multicall3_fallback_total",
		Help:      "Total number of multicall3 fallbacks to individual requests.",
	}, []string{"project", "network", "reason"})

	MetricMulticall3CacheHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "multicall3_cache_hits_total",
		Help:      "Total number of per-call cache hits in multicall3 batch aggregation.",
	}, []string{"project", "network"})

	// Network-level Multicall3 batching metrics
	MetricMulticall3BatchSize = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "multicall3_batch_size",
		Help:      "Number of unique calls per Multicall3 batch.",
		Buckets:   []float64{1, 2, 5, 10, 15, 20, 30, 50},
	}, []string{"project", "network"})

	MetricMulticall3BatchWaitMs = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "multicall3_batch_wait_ms",
		Help:      "Time requests waited in batch before flush (milliseconds).",
		Buckets:   []float64{1, 2, 5, 10, 15, 20, 25, 30, 50},
	}, []string{"project", "network"})

	MetricMulticall3QueueLen = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "multicall3_queue_len",
		Help:      "Current number of requests queued for batching.",
	}, []string{"project", "network"})

	MetricMulticall3QueueOverflowTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "multicall3_queue_overflow_total",
		Help:      "Total number of requests that bypassed batching due to queue overflow.",
	}, []string{"project", "network", "reason"})

	MetricMulticall3DedupeTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "multicall3_dedupe_total",
		Help:      "Total number of deduplicated requests within batches.",
	}, []string{"project", "network"})

	MetricMulticall3CacheWriteErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "multicall3_cache_write_errors_total",
		Help:      "Total number of per-call cache write errors in multicall3 batch responses.",
	}, []string{"project", "network"})

	MetricMulticall3CacheReadErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "multicall3_cache_read_errors_total",
		Help:      "Total number of cache read errors during multicall3 pre-aggregation cache check.",
	}, []string{"project", "network"})

	MetricMulticall3FallbackRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "multicall3_fallback_requests_total",
		Help:      "Total number of individual requests during multicall3 fallback, labeled by outcome.",
	}, []string{"project", "network", "outcome"})

	MetricMulticall3AbandonedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "multicall3_abandoned_total",
		Help:      "Total number of multicall3 batch results not delivered because caller context was cancelled.",
	}, []string{"project", "network"})

	MetricMulticall3PanicTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "multicall3_panic_total",
		Help:      "Total number of panics recovered in multicall3 batch processing.",
	}, []string{"project", "network", "location"})

	MetricMulticall3CacheWriteDroppedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "multicall3_cache_write_dropped_total",
		Help:      "Total number of multicall3 per-call cache writes dropped due to backpressure.",
	}, []string{"project", "network"})

	MetricMulticall3RuntimeBypassTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "multicall3_runtime_bypass_total",
		Help:      "Total number of contracts auto-detected as requiring bypass (revert via multicall3 but succeed individually).",
	}, []string{"project", "network"})

	MetricMulticall3AutoDetectRetryTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "multicall3_auto_detect_retry_total",
		Help:      "Total number of auto-detect retry attempts for reverted calls.",
	}, []string{"project", "network", "outcome"})
)

var DefaultHistogramBuckets = []float64{
	0.05,
	0.5,
	5,
	30,
}

// Default bucket size for EVM block-range heatmap metrics.
// Can be tuned to 50000 or 100000 to control series cardinality.
var EvmBlockRangeBucketSize int64 = 100000

// Histogram buckets for eth_getLogs requested block-range sizes
var EvmGetLogsRangeHistogramBuckets = []float64{1, 10, 100, 500, 1000, 5000, 10000, 30000}

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

// ScoreMetricsMode controls how score metrics are emitted.
// "compact": emit compact series by setting upstream and category to 'n/a'
// "detailed": emit full project/vendor/network/upstream/category series
// "none": do not emit score metrics
type ScoreMetricsMode string

const (
	ScoreModeCompact  ScoreMetricsMode = "compact"
	ScoreModeDetailed ScoreMetricsMode = "detailed"
	ScoreModeNone     ScoreMetricsMode = "none"
)

var currentScoreMetricsMode = ScoreModeCompact

func SetScoreMetricsMode(v string) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "detailed":
		currentScoreMetricsMode = ScoreModeDetailed
	case "none":
		currentScoreMetricsMode = ScoreModeNone
	default:
		currentScoreMetricsMode = ScoreModeCompact
	}
}

func GetScoreMetricsMode() ScoreMetricsMode {
	return currentScoreMetricsMode
}

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
	}, []string{"project", "vendor", "network", "upstream", "category", "composite", "finality", "user"})

	MetricNetworkRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "network_request_duration_seconds",
		Help:      "Duration of requests for a network.",
		Buckets:   buckets,
	}, []string{"project", "network", "vendor", "upstream", "category", "finality", "user"})

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
		Buckets:   buckets,
	}, []string{"project", "network", "category", "outcome", "finality"})

	// Clear cached handles since the Vecs were re-created.
	ResetHandleCache()

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
