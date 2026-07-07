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

	MetricCacheConnectorEarliestBlockNumber = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "cache_connector_earliest_block_number",
		Help:      "Earliest block number available in a read-through cache connector, per network.",
	}, []string{"connector", "network"})

	MetricCacheConnectorLatestBlockNumber = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "cache_connector_latest_block_number",
		Help:      "Latest block number available in a read-through cache connector, per network.",
	}, []string{"connector", "network"})

	MetricCacheConnectorFinalizedBlockNumber = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "cache_connector_finalized_block_number",
		Help:      "Finalized block number available in a read-through cache connector, per network.",
	}, []string{"connector", "network"})

	MetricCacheConnectorEarliestBlockTimestamp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "cache_connector_earliest_block_timestamp_seconds",
		Help:      "Unix timestamp (seconds) of the earliest block available in a read-through cache connector, per network.",
	}, []string{"connector", "network"})

	MetricCacheConnectorLatestBlockTimestamp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "cache_connector_latest_block_timestamp_seconds",
		Help:      "Unix timestamp (seconds) of the latest block available in a read-through cache connector, per network.",
	}, []string{"connector", "network"})

	MetricCacheConnectorFinalizedBlockTimestamp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "cache_connector_finalized_block_timestamp_seconds",
		Help:      "Unix timestamp (seconds) of the finalized block available in a read-through cache connector, per network.",
	}, []string{"connector", "network"})

	MetricNetworkLatestBlockTimestampDistance = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "network_latest_block_timestamp_distance_seconds",
		Help:      "Distance in seconds between the latest block timestamp and current time for a network.",
	}, []string{"project", "network", "origin"})

	MetricNetworkDynamicBlockTime = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "network_dynamic_block_time_milliseconds",
		Help:      "Dynamically computed block time per network in milliseconds.",
	}, []string{"project", "network"})

	// MetricNetworkServedTipBlockNumber is the block number the network actually
	// advertises/serves as the tip for a block tag (axis=latest|finalized): the
	// freshest block a strict MAJORITY of eligible upstreams already have (see
	// evm.PickServedTip). lane="all" is the network-wide pick; a named lane is a
	// use-upstream group's own pick (present only for networks receiving
	// targeted traffic).
	MetricNetworkServedTipBlockNumber = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "network_served_tip_block_number",
		Help:      "Block number served/advertised as the tip (freshest block a strict majority of eligible upstreams already have). lane=\"all\" = network-wide; a named lane = a use-upstream group's own tip.",
	}, []string{"project", "network", "lane", "axis"})

	// MetricNetworkServedTipLagBlocks is the deliberate, bounded served-tip lag:
	// blocks the majority tip sits behind the single freshest upstream view at
	// pick time (Freshest - Tip). Computed at the same instant as the pick, so it
	// is exact (no cross-metric scrape skew). NOTE: the series is ABSENT (not 0)
	// when served-tip is in default MAX mode (feature off) for that axis.
	MetricNetworkServedTipLagBlocks = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "network_served_tip_lag_blocks",
		Help:      "Blocks the majority served tip sits behind the single freshest upstream view at pick time, per network, lane and axis. lane=\"all\" is the network-wide pick; a named lane is a use-upstream group's own pick.",
	}, []string{"project", "network", "lane", "axis"})

	// MetricNetworkServedTipAdvanceAgeSeconds is the time since this process
	// last observed the served-tip VALUE change, exported at pick time. This
	// is the universal stuck-tip detector: regardless of WHICH mechanism would
	// freeze the tip (a failure mode nobody predicted included), a healthy
	// network advances every block or two, so `age >> block time` on a network
	// with live traffic is always alert-worthy. Absent until the process
	// observes its first change (and absent in default MAX mode).
	MetricNetworkServedTipAdvanceAgeSeconds = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "network_served_tip_advance_age_seconds",
		Help:      "Seconds since the served-tip value last changed (observed in-process, exported at pick time); sustained high values on a live chain = stuck tip.",
	}, []string{"project", "network", "lane", "axis"})

	MetricUpstreamCordoned = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "upstream_cordoned",
		Help:      "Whether upstream is un/cordoned (excluded from routing by selection policy).",
	}, []string{"project", "vendor", "network", "upstream", "category", "reason"})

	// ── Selection-policy engine metrics (see internal/policy + spec §8.2).
	// Cardinality is fixed (no per-method category like the legacy
	// scoreMetricsMode knob); operators don't get to dial it down per project.

	MetricSelectionPosition = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "selection_position",
		Help:      "Selection-policy output position for an upstream: 0 = primary, 1+ = runner-up, -1 = excluded this tick.",
	}, []string{"project", "network", "method", "upstream"})

	MetricSelectionRejectionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "selection_rejection_total",
		Help:      "Number of ticks each upstream was rejected by a specific std-lib step.",
	}, []string{"project", "network", "method", "upstream", "step"})

	MetricSelectionPrimarySwitchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "selection_primary_switch_total",
		Help:      "Primary-upstream changes per (project, network, method, from, to).",
	}, []string{"project", "network", "method", "from", "to"})

	MetricSelectionEvalDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "selection_eval_duration_seconds",
		Help:      "Per-tick eval latency for the selection policy.",
		Buckets:   []float64{0.0005, 0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	}, []string{"project", "network", "method"})

	MetricSelectionEvalErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "selection_eval_errors_total",
		Help:      "Eval failures by kind (timeout, throw, invalid_return, fallback_default).",
	}, []string{"project", "network", "method", "kind"})

	MetricSelectionEligibleUpstreams = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "selection_eligible_upstreams",
		Help:      "Count of upstreams returned by the most recent tick.",
	}, []string{"project", "network", "method"})

	// ── Selection-policy detail metrics. Bounded cardinality:
	//   * `score`, `excluded_seconds`, `readmit_total`, `sticky_hold_total`:
	//     per (project, network, method, upstream).
	//   * `exclusion_total`: same plus a `reason` slug bounded by the set of
	//     predicate factories. Compound predicates (`any`/`all`/`not`) emit
	//     one increment per LEAF reason that tripped, attributing exclusion
	//     to the actual signal rather than the compound boilerplate.
	//   * `readmit_age_seconds`: per (project, network, method) only — the
	//     per-upstream "how long out before readmit" lives in spans/logs.

	MetricSelectionScore = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "selection_score",
		Help:      "Per-upstream score produced by `sortByScore`. Lower = better. Missing for upstreams that bypassed scoring (probeExcluded/forceInclude additions and policies without a sortByScore step).",
	}, []string{"project", "network", "method", "upstream"})

	MetricSelectionExclusionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "selection_exclusion_total",
		Help:      "Per-upstream exclusion events labelled by the leaf-predicate slug that tripped (`error_rate_above`, `latency_p95_above`, `block_head_lag_above`, ...). Compound predicates emit one increment per leaf — exclusion is attributed to the actual signal, not the combinator.",
	}, []string{"project", "network", "method", "upstream", "reason"})

	MetricSelectionShadowExclusionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "selection_shadow_exclusion_total",
		Help:      "Per-upstream WOULD-HAVE-BEEN-EXCLUDED events from `shadowExcludeIf` predicates. Same leaf-slug attribution as `selection_exclusion_total`, but the upstream stays in rotation — meant for safely auditioning a new (or removed) exclusion rule in production before promoting it to a real `excludeIf`.",
	}, []string{"project", "network", "method", "upstream", "reason"})

	MetricSelectionExcludedSeconds = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "selection_excluded_seconds",
		Help:      "Wall-clock seconds the upstream has been continuously excluded. `0` when in rotation. Alert on `> 600` for `stuck excluded > 10m`.",
	}, []string{"project", "network", "method", "upstream"})

	MetricSelectionReadmitTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "selection_readmit_total",
		Help:      "Times an upstream was readmitted into rotation after a period of exclusion (transition from excluded → in-list).",
	}, []string{"project", "network", "method", "upstream"})

	MetricSelectionReadmitAgeSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "selection_readmit_age_seconds",
		Help:      "Distribution of `now - excludedSince` at readmit time. Tall left tail = readmitting too eagerly (probable flap); tall right tail = readmit cooldown too generous.",
		Buckets:   []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
	}, []string{"project", "network", "method"})

	MetricSelectionStickyHoldTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "selection_sticky_hold_total",
		Help:      "Ticks where `stickyPrimary` actively held the primary that would otherwise have flipped (challenger had a lower score AND/OR cooldown still in effect). The ratio against `selection_primary_switch_total` reveals how much smoothing the chain is doing.",
	}, []string{"project", "network", "method", "upstream"})

	// Probe family — shadow-mirror traffic the selection policy fires
	// at currently-excluded upstreams via `probeExcluded(...)`. Cardinality:
	// `network × upstream × method` for requests/errors; `network × reason`
	// for skip/drop counters (reason is a small fixed enum).

	MetricSelectionProbeRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "selection_probe_requests_total",
		Help:      "Probe-mirror requests sent to a currently-excluded upstream. Drives the re-admission story: as `errorRateAbove` predicates see the upstream's fresh shadow samples improve, it falls out of the excluded set on the next tick. Pairs with `selection_probe_errors_total` for probe-side error rate.",
	}, []string{"network", "upstream", "method"})

	MetricSelectionProbeErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "selection_probe_errors_total",
		Help:      "Probe-mirror requests that returned an error. Same upstream/method as requests; reason ∈ {timeout, throttled, auth, skipped, error}.",
	}, []string{"network", "upstream", "method", "reason"})

	MetricSelectionProbeSkipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "selection_probe_skipped_total",
		Help:      "Probe candidates that were skipped before firing. reason ∈ {write_method, opt_out, sampled_out, max_concurrent, no_method}. `write_method` and `opt_out` are safety/policy gates; `sampled_out` and `max_concurrent` are throughput controls.",
	}, []string{"network", "reason"})

	MetricSelectionProbeDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "selection_probe_dropped_total",
		Help:      "Probe-bus publishes that were dropped before reaching the dispatcher (the per-network feed channel was full). The request path NEVER blocks on the bus — overflow becomes a lost sample, not a stalled forward.",
	}, []string{"network", "reason"})

	MetricUpstreamCordonEventTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_cordon_event_total",
		Help:      "Admin-driven cordon transitions. `action` ∈ {`cordon`,`uncordon`}.",
	}, []string{"project", "network", "upstream", "action"})

	MetricUpstreamCordonDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "upstream_cordon_duration_seconds",
		Help:      "Time an upstream stayed cordoned, observed on each uncordon. Long tails are typically real outages; very short cordons are usually manual mis-fires.",
		Buckets:   []float64{1, 10, 60, 300, 900, 1800, 3600, 7200, 21600, 86400},
	}, []string{"project", "network", "upstream"})

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

	MetricNetworkEvmTraceFilterSplitSuccess = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_evm_trace_filter_split_success_total",
		Help:      "Total number of successful split trace_filter/arbtrace_filter sub-requests (network-scoped).",
	}, []string{"project", "network", "method", "user", "agent_name"})

	MetricNetworkEvmTraceFilterSplitFailure = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_evm_trace_filter_split_failure_total",
		Help:      "Total number of failed split trace_filter/arbtrace_filter sub-requests (network-scoped).",
	}, []string{"project", "network", "method", "user", "agent_name"})

	MetricNetworkEvmTraceFilterForcedSplits = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_evm_trace_filter_forced_splits_total",
		Help:      "Total number of trace_filter/arbtrace_filter request splits by dimension (block_range, from_address, to_address), network-scoped.",
	}, []string{"project", "network", "method", "dimension", "user", "agent_name"})

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

	// MetricGrpcBdsHardTimeoutTotal counts how many BDS gRPC calls hit the
	// hard per-call ceiling (the bounded-wait timeout in SendRequest). A
	// non-zero rate is the smoking-gun indicator of wedged H2 streams.
	MetricGrpcBdsHardTimeoutTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "grpc_bds_hard_timeout_total",
		Help:      "Total number of BDS gRPC calls that hit the bounded-wait hard timeout (caller abandoned the call before grpc-go returned).",
	}, []string{"project", "upstream", "method"})

	// MetricGrpcBdsConnReplacementsTotal counts how many times a BDS pool
	// connection was force-closed by the stuck-call watchdog.
	MetricGrpcBdsConnReplacementsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "grpc_bds_conn_replacements_total",
		Help:      "Total number of BDS pool connections force-closed by the stuck-call watchdog.",
	}, []string{"project", "upstream"})

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

	MetricNetworkStaticResponseServedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_static_response_served_total",
		Help:      "Total number of requests served from a configured static response without contacting any upstream.",
	}, []string{"project", "network", "category"})

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

	MetricNetworkTimeoutFiredTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_timeout_fired_total",
		Help:      "Total number of requests that were killed by the timeout policy (fixed or quantile-based).",
	}, []string{"project", "network", "category", "finality", "scope"})

	// MetricUpstreamSelectionTotal counts each upstream pick by the
	// reason for selection: primary / retry / hedge / consensus_slot /
	// sweep. Lets operators see whether one upstream is dominating one
	// selection path (e.g. always being chosen as the hedge target).
	MetricUpstreamSelectionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_selection_total",
		Help:      "Total upstream selections by reason (primary/retry/hedge/consensus_slot/sweep). One increment per attempt start.",
	}, []string{"project", "network", "upstream", "category", "reason", "finality"})

	// MetricUpstreamAttemptOutcomeTotal counts each upstream attempt's
	// terminal outcome. This is the canonical per-upstream-per-attempt
	// observability lens — answers "what happened with this upstream
	// for this request?".
	MetricUpstreamAttemptOutcomeTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_attempt_outcome_total",
		Help:      "Per-(upstream, method, outcome) attempt count. Outcomes: success/empty/transport_error/server_error/client_error/rate_limited/missing_data/exec_revert/block_unavailable/breaker_open/cancelled/timeout/skipped.",
	}, []string{"project", "network", "upstream", "category", "outcome", "is_hedge", "is_retry", "finality"})

	// MetricNetworkRetryAttemptTotal counts retry attempts at the
	// network scope, labeled by the reason for retry (empty_result /
	// pending_tx / retryable_error / block_unavailable / missing_data).
	MetricNetworkRetryAttemptTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_retry_attempt_total",
		Help:      "Total network-scope retry attempts by reason (empty_result/pending_tx/retryable_error/block_unavailable/missing_data).",
	}, []string{"project", "network", "category", "reason", "finality"})

	// MetricNetworkHedgeWinnerTotal counts hedge-race winners by
	// upstream. Operators use this to detect skew: is one upstream
	// consistently winning hedges (good — pick it as primary) or
	// consistently losing (bad — drop it from the pool)?
	MetricNetworkHedgeWinnerTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_hedge_winner_total",
		Help:      "Total hedge races won by upstream (the one whose response was kept).",
	}, []string{"project", "network", "upstream", "category", "finality"})

	// MetricUpstreamBreakerStateChange counts breaker state transitions
	// per upstream. Operators see frequency of open/close churn,
	// useful for debugging flapping upstreams.
	MetricUpstreamBreakerStateChange = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_breaker_state_change_total",
		Help:      "Total circuit-breaker state transitions per upstream and direction (closed_to_open/half_open_to_open/half_open_to_closed/open_to_half_open).",
	}, []string{"project", "upstream", "transition"})

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

	// MetricRateLimiterRemoteInflight is a per-budget gauge of concurrent in-flight
	// remote (e.g. Redis) DoLimit calls. When a remote rate limiter is overwhelmed
	// this gauge climbs without bound — the admission semaphore in
	// doLimitWithTimeout uses MetricRateLimiterAdmissionSheddedTotal to indicate
	// when the cap is reached, but this gauge is the canary that something is
	// queueing on the remote.
	MetricRateLimiterRemoteInflight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "rate_limiter_remote_inflight",
		Help:      "Current number of in-flight remote rate-limit checks per budget.",
	}, []string{"budget"})

	// MetricRateLimiterRemoteAdmissionSheddedTotal is a counter of fail-open events
	// caused by the per-budget admission semaphore being full. This is intentionally
	// distinct from "limit_timeout" in MetricRateLimiterFailopenTotal because it
	// indicates load shedding (we never even attempted the remote call) vs the
	// remote being slow.
	MetricRateLimiterRemoteAdmissionSheddedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "rate_limiter_remote_admission_shedded_total",
		Help:      "Total number of remote rate-limit checks fail-opened because the admission semaphore was full.",
	}, []string{"budget"})

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
	}, []string{"project", "network", "category", "outcome", "finality", "user", "agent_name"})

	MetricConsensusMisbehaviorDetected = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_misbehavior_detected_total",
		Help:      "Total number of times an upstream returned different data (not errors) than consensus.",
	}, []string{"project", "network", "upstream", "category", "finality", "response_type", "larger_than_consensus", "user", "agent_name"})

	MetricConsensusUpstreamPunished = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_upstream_punished_total",
		Help:      "Total number of times upstreams were punished.",
	}, []string{"project", "network", "upstream", "user", "agent_name"})

	MetricConsensusShortCircuit = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_short_circuit_total",
		Help:      "Total number of consensus rounds that short-circuited.",
	}, []string{"project", "network", "category", "reason", "finality", "user", "agent_name"})

	// MetricConsensusWaitCapped counts consensus rounds resolved early
	// because maxWaitOnResult / maxWaitOnEmpty fired before every
	// participant returned. High rates indicate persistently slow
	// upstreams dragging tail latency — operators can drop those
	// upstreams or tighten the wait caps further.
	MetricConsensusWaitCapped = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_wait_capped_total",
		Help:      "Total number of consensus rounds resolved early due to MaxWaitOnResult/MaxWaitOnEmpty firing.",
	}, []string{"project", "network", "category", "trigger", "finality", "user", "agent_name"})

	MetricConsensusErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_errors_total",
		Help:      "Total number of consensus errors by type.",
	}, []string{"project", "network", "category", "error", "finality", "user", "agent_name"})

	MetricConsensusUpstreamErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_upstream_errors_total",
		Help:      "Total number of errors from upstreams during consensus operations.",
	}, []string{"project", "network", "upstream", "category", "finality", "response_type", "error_code", "user", "agent_name"})

	MetricConsensusPanics = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_panics_total",
		Help:      "Total number of panic recoveries in consensus.",
	}, []string{"project", "network", "category", "finality", "user", "agent_name"})

	MetricConsensusCancellations = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "consensus_cancellations_total",
		Help:      "Total number of context cancellations during consensus.",
	}, []string{"project", "network", "category", "phase", "finality", "user", "agent_name"})

	MetricNetworkEvmBlockRangeRequested = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "network_evm_block_range_requested_total",
		Help:      "Total requests observed by block-number buckets for heatmap.",
	}, []string{"project", "network", "vendor", "upstream", "category", "user", "finality", "bucket", "size"})
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

// CatchUpWaitHistogramBuckets are dedicated buckets for the catch-up wait
// histogram (network_data_unavailable_wait_seconds). Catch-up waits cluster at
// ~one block time, which varies by chain from sub-second (fast L2s) to ~12s
// (Ethereum mainnet), with tails when retries stack — a range the global
// request-latency buckets resolve poorly (e.g. a 10s→30s gap puts mainnet's 12s
// wait in one coarse bucket, making p95 meaningless). These ~log2 buckets bracket
// the common block times and extend past 30s so long-wait tails stay visible.
var CatchUpWaitHistogramBuckets = []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64}

// Histograms are populated by SetHistogramBuckets so the label filter applies.
var (
	MetricUpstreamRequestDuration             *LabeledHistogram
	MetricNetworkRequestDuration              *LabeledHistogram
	MetricNetworkEvmGetLogsRangeRequested     *LabeledHistogram
	MetricNetworkEvmTraceFilterRangeRequested *LabeledHistogram
	MetricCacheEvmGetLogsRange                *LabeledHistogram
	MetricNetworkHedgeDelaySeconds            *LabeledHistogram
	MetricNetworkTimeoutDurationSeconds       *LabeledHistogram
	MetricNetworkDataUnavailableWaitSeconds   *LabeledHistogram
	MetricConsensusResponsesCollected         *LabeledHistogram
	MetricConsensusAgreementCount             *LabeledHistogram
	MetricConsensusDuration                   *LabeledHistogram
	MetricCacheSetSuccessDuration             *LabeledHistogram
	MetricCacheSetErrorDuration               *LabeledHistogram
	MetricCacheGetSuccessHitDuration          *LabeledHistogram
	MetricCacheGetSuccessMissDuration         *LabeledHistogram
	MetricCacheGetErrorDuration               *LabeledHistogram
	MetricRateLimiterRemoteDuration           *LabeledHistogram
	MetricUpstreamResponseSizeBytes           *LabeledHistogram
)

// buildFilterAwareHistograms creates every LabeledHistogram using the current
// filter. It does NOT register them — SetHistogramBuckets does that. init()
// calls this without registering so metric globals are non-nil for any code
// that observes before erpc.Init runs (tests, early startup paths).
func buildFilterAwareHistograms(bucketsStr string) error {
	buckets, parseErr := ParseHistogramBuckets(bucketsStr)
	if parseErr != nil {
		buckets = DefaultHistogramBuckets
	}

	MetricUpstreamRequestDuration = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "upstream_request_duration_seconds",
		Help:      "Duration of actual requests towards upstreams.",
		Buckets:   buckets,
	}, []string{"project", "vendor", "network", "upstream", "category", "composite", "finality", "user"})

	MetricNetworkRequestDuration = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "network_request_duration_seconds",
		Help:      "Duration of requests for a network.",
		Buckets:   buckets,
	}, []string{"project", "network", "vendor", "upstream", "category", "finality", "user"})

	MetricNetworkEvmGetLogsRangeRequested = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "network_evm_get_logs_range_requested",
		Help:      "eth_getLogs requested block-range sizes.",
		Buckets:   EvmGetLogsRangeHistogramBuckets,
	}, []string{"project", "network", "category", "user", "finality"})

	MetricNetworkEvmTraceFilterRangeRequested = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "network_evm_trace_filter_range_requested",
		Help:      "trace_filter/arbtrace_filter requested block-range sizes.",
		Buckets:   EvmGetLogsRangeHistogramBuckets,
	}, []string{"project", "network", "method", "user", "finality"})

	// Same block-range distribution as MetricNetworkEvmGetLogsRangeRequested but
	// observed at the cache layer, partitioned by the connector involved and the
	// hit/miss outcome. This exposes the per-connector served/missed range shape
	// (e.g. connectors that answer wide ranges in one read vs. those that only
	// serve narrow/single-block ranges, or the wide ranges a connector misses).
	MetricCacheEvmGetLogsRange = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_evm_get_logs_range",
		Help:      "eth_getLogs block-range sizes seen at the cache layer, partitioned by connector and hit/miss outcome.",
		Buckets:   EvmGetLogsRangeHistogramBuckets,
	}, []string{"project", "network", "connector", "policy", "ttl", "outcome"})

	MetricNetworkHedgeDelaySeconds = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "network_hedge_delay_seconds",
		Help:      "Hedge delay used for requests (seconds).",
		Buckets:   []float64{0.01, 0.03, 0.05, 0.2, 0.3, 0.5, 0.7, 1, 3},
	}, []string{"project", "network", "category", "finality"})

	MetricNetworkTimeoutDurationSeconds = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "network_timeout_duration_seconds",
		Help:      "Dynamic timeout duration computed for requests (seconds).",
		Buckets:   []float64{0.05, 0.1, 0.3, 0.5, 1, 3, 5, 10, 30},
	}, []string{"project", "network", "category", "finality"})

	// MetricNetworkDataUnavailableWaitSeconds measures the wall-clock delay
	// deliberately spent waiting for a not-yet-available block to appear (a
	// "catch-up" wait) before a retry: the block-time-relative backoff applied
	// when the requested block isn't on the upstream yet (reasons
	// block_unavailable / empty_result / missing_data — the cases that take the
	// block-time delay path in computeDelay). The duration companion to
	// network_retry_attempt_total{reason} (the count): together they show how
	// much retry latency is chain catch-up vs genuine-error failover.
	MetricNetworkDataUnavailableWaitSeconds = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "network_data_unavailable_wait_seconds",
		Help:      "Wall-clock catch-up delay before a data-not-yet-available retry, by reason (block_unavailable/empty_result/missing_data).",
		// Dedicated buckets (not the global request-latency buckets): catch-up
		// waits track per-chain block time, which the global buckets resolve poorly.
		Buckets: CatchUpWaitHistogramBuckets,
	}, []string{"project", "network", "category", "reason", "finality"})

	MetricConsensusResponsesCollected = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "consensus_responses_collected",
		Help:      "Number of responses collected before consensus decision.",
		Buckets:   prometheus.LinearBuckets(1, 1, 10),
	}, []string{"project", "network", "category", "vendors", "short_circuited", "finality", "user", "agent_name"})

	MetricConsensusAgreementCount = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "consensus_agreement_count",
		Help:      "Number of upstreams agreeing on the most common result.",
		Buckets:   prometheus.LinearBuckets(1, 1, 10),
	}, []string{"project", "network", "category", "finality", "user", "agent_name"})

	MetricConsensusDuration = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "consensus_duration_seconds",
		Help:      "Duration of consensus operations.",
		Buckets:   buckets,
	}, []string{"project", "network", "category", "outcome", "finality", "user", "agent_name"})

	MetricCacheSetSuccessDuration = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_set_success_duration_seconds",
		Help:      "Duration of cache set operations.",
		Buckets:   buckets,
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheSetErrorDuration = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_set_error_duration_seconds",
		Help:      "Duration of cache set errors.",
		Buckets:   buckets,
	}, []string{"project", "network", "category", "connector", "policy", "ttl", "error"})

	MetricCacheGetSuccessHitDuration = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_get_success_hit_duration_seconds",
		Help:      "Duration of cache get hits.",
		Buckets:   buckets,
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheGetSuccessMissDuration = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_get_success_miss_duration_seconds",
		Help:      "Duration of cache get misses.",
		Buckets:   buckets,
	}, []string{"project", "network", "category", "connector", "policy", "ttl"})

	MetricCacheGetErrorDuration = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_get_error_duration_seconds",
		Help:      "Duration of cache get errors.",
		Buckets:   buckets,
	}, []string{"project", "network", "category", "connector", "policy", "ttl", "error"})

	// Rate limiter remote-call duration uses fine-grained sub-second buckets
	// because the whole request budget is typically <500ms — the default 0.05/0.5/5/30
	// buckets give zero useful resolution here.
	MetricRateLimiterRemoteDuration = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "rate_limiter_remote_duration_seconds",
		Help:      "Duration of remote rate-limit checks (e.g. Redis DoLimit).",
		Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"budget", "result"})

	// Upstream response result-body size in bytes (decoded, post-gzip). Use
	// for finding networks/methods that produce huge responses — the
	// dominant driver of transient heap spikes (each response goes through
	// io.Copy → bytes.Buffer.grow → sonic.Unmarshal → []byte copy, peaking
	// at ~3-4× the response size in transient allocations).
	//
	// Cardinality is intentionally tight: only network/category/finality
	// dimensions, since identifying which net+method produces fat
	// responses is the actionable question (vendor/upstream/user are
	// derivable via traffic correlation in upstream_request_total).
	//
	// Buckets are coarse on purpose — we just need order-of-magnitude
	// signal: <4 KB (header-y), 64 KB (single block), 1 MB (small logs),
	// 16 MB (heavy logs), 100 MB+ (pathological).
	MetricUpstreamResponseSizeBytes = NewLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "upstream_response_size_bytes",
		Help:      "Size of the result body of upstream JSON-RPC responses in bytes (decoded post-gzip), per network/method/finality.",
		Buckets:   []float64{4096, 65536, 1048576, 16777216, 104857600},
	}, []string{"project", "network", "category", "finality"})

	return parseErr
}

// init bootstraps non-nil LabeledHistogram pointers with the empty filter
// (unregistered). This lets observations from tests or early-startup code
// complete without NPE. erpc.Init later calls SetHistogramBuckets which
// replaces these with filter-applied wrappers and registers them.
func init() {
	_ = buildFilterAwareHistograms("")
}

// SetHistogramBuckets builds every filter-aware histogram under the current
// filter and registers them with prometheus.DefaultRegisterer. After this
// call, metric globals point at the registered wrapper.
//
// Idempotent in the same-labels case (re-calls on the same registry with an
// unchanged filter reuse the existing registration). Calling again with a
// changed filter panics — Prometheus does not allow a metric's label set to
// change once registered. Tests that need a clean slate should swap in a
// fresh prometheus.DefaultRegisterer first.
func SetHistogramBuckets(bucketsStr string) error {
	parseErr := buildFilterAwareHistograms(bucketsStr)

	MetricUpstreamRequestDuration = registerOrReuse(MetricUpstreamRequestDuration)
	MetricNetworkRequestDuration = registerOrReuse(MetricNetworkRequestDuration)
	MetricNetworkEvmGetLogsRangeRequested = registerOrReuse(MetricNetworkEvmGetLogsRangeRequested)
	MetricNetworkEvmTraceFilterRangeRequested = registerOrReuse(MetricNetworkEvmTraceFilterRangeRequested)
	MetricCacheEvmGetLogsRange = registerOrReuse(MetricCacheEvmGetLogsRange)
	MetricNetworkHedgeDelaySeconds = registerOrReuse(MetricNetworkHedgeDelaySeconds)
	MetricNetworkTimeoutDurationSeconds = registerOrReuse(MetricNetworkTimeoutDurationSeconds)
	MetricNetworkDataUnavailableWaitSeconds = registerOrReuse(MetricNetworkDataUnavailableWaitSeconds)
	MetricConsensusResponsesCollected = registerOrReuse(MetricConsensusResponsesCollected)
	MetricConsensusAgreementCount = registerOrReuse(MetricConsensusAgreementCount)
	MetricConsensusDuration = registerOrReuse(MetricConsensusDuration)
	MetricCacheSetSuccessDuration = registerOrReuse(MetricCacheSetSuccessDuration)
	MetricCacheSetErrorDuration = registerOrReuse(MetricCacheSetErrorDuration)
	MetricCacheGetSuccessHitDuration = registerOrReuse(MetricCacheGetSuccessHitDuration)
	MetricCacheGetSuccessMissDuration = registerOrReuse(MetricCacheGetSuccessMissDuration)
	MetricCacheGetErrorDuration = registerOrReuse(MetricCacheGetErrorDuration)
	MetricRateLimiterRemoteDuration = registerOrReuse(MetricRateLimiterRemoteDuration)
	MetricUpstreamResponseSizeBytes = registerOrReuse(MetricUpstreamResponseSizeBytes)

	// Clear cached handles since the Vecs were re-created.
	ResetHandleCache()

	return parseErr
}

// registerOrReuse registers lh with prometheus.DefaultRegisterer. If a
// collector with the same name and identical label set is already registered
// (typical for repeat calls), it returns the existing collector so callers
// keep using the one prometheus actually knows about. Panics on any other
// registration error (including label-set mismatch, which means the filter
// changed after the first registration — not supported by Prometheus).
func registerOrReuse(lh *LabeledHistogram) *LabeledHistogram {
	err := prometheus.Register(lh)
	if err == nil {
		return lh
	}
	if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
		if existing, ok := are.ExistingCollector.(*LabeledHistogram); ok {
			return existing
		}
	}
	panic(err)
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
