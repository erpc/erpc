# Census: Prometheus metrics & trace spans

> Scope: every Prometheus metric defined in `telemetry/` (metrics.go, labeled_histogram.go, handles.go), a repo-wide sweep for metrics registered anywhere else, and the full inventory of OTel tracing span names. This is the completeness contract for KB coverage — checklist only, no prose.

Totals: **122 metric definitions** (79 counters, 23 gauges, 3 plain `promauto` histograms, 17 `LabeledHistogram` filter-aware histograms), all defined in `telemetry/metrics.go`. **126 unique trace span names** across the repo. Repo-wide grep for `promauto.`, `prometheus.NewCounterVec|NewGaugeVec|NewHistogramVec|NewSummaryVec|GaugeFunc|CounterFunc|MustRegister` confirms **no metric is defined outside `telemetry/`** (the only non-telemetry hits are registry plumbing, listed in §3).

All metric names below carry the `erpc_` namespace prefix (`Namespace: "erpc"` on every definition).

## 1. Counters & gauges (eagerly registered via `promauto` at package init, `telemetry/metrics.go:12-729`)

| metric name | type | labels | when it fires | defined at |
|---|---|---|---|---|
| `erpc_unexpected_panic_total` | counter | scope, extra, error | recovered panic in any guarded goroutine/handler (HTTP server `erpc/http_server.go:446,759`, matcher `common/matcher.go:123`, redis pubsub `data/redis_pubsub_manager.go:232,335`, shared-state registry `data/shared_state_registry.go:171`, …) | telemetry/metrics.go:13 |
| `erpc_upstream_request_total` | counter | project, vendor, network, upstream, category, attempt, composite, finality, user, agent_name | each actual attempt sent to an upstream (`upstream/upstream.go:613`) | telemetry/metrics.go:19 |
| `erpc_upstream_request_errors_total` | counter | project, vendor, network, upstream, category, error, severity, composite, finality, user, agent_name | upstream attempt returned an error (`upstream/upstream.go:726`); also incremented with severity=info when an upstream is skipped by block-availability gating (`erpc/networks.go:1852`) | telemetry/metrics.go:25 |
| `erpc_upstream_request_skipped_total` | counter | project, vendor, network, upstream, category, finality, user, agent_name | upstream pre-forward checks decided to skip the upstream (`upstream/upstream.go:679`) | telemetry/metrics.go:31 |
| `erpc_upstream_request_missing_data_error_total` | counter | project, vendor, network, upstream, category, finality, user, agent_name | upstream returned missing-data/not-synced error (`upstream/upstream.go:690`) | telemetry/metrics.go:37 |
| `erpc_upstream_request_empty_response_total` | counter | project, vendor, network, upstream, category, finality, user, agent_name | upstream returned an emptyish response (`upstream/upstream.go:791`) | telemetry/metrics.go:43 |
| `erpc_upstream_block_head_lag` | gauge | project, vendor, network, upstream | set by health tracker on block-head updates: blocks behind freshest upstream (`health/tracker.go:433`) | telemetry/metrics.go:49 |
| `erpc_upstream_finalization_lag` | gauge | project, vendor, network, upstream | set by health tracker on finalized-block updates (`health/tracker.go:443`) | telemetry/metrics.go:55 |
| `erpc_upstream_latest_block_number` | gauge | project, vendor, network, upstream | upstream's latest block advanced (`health/tracker.go:413`) | telemetry/metrics.go:61 |
| `erpc_upstream_finalized_block_number` | gauge | project, vendor, network, upstream | upstream's finalized block advanced (`health/tracker.go:423`) | telemetry/metrics.go:67 |
| `erpc_cache_connector_earliest_block_number` | gauge | connector, network | gRPC cache connector availability poll reports earliest block (`data/grpc.go:342`) | telemetry/metrics.go:73 |
| `erpc_cache_connector_latest_block_number` | gauge | connector, network | gRPC cache connector availability poll reports latest block (`data/grpc.go:358`) | telemetry/metrics.go:79 |
| `erpc_cache_connector_finalized_block_number` | gauge | connector, network | gRPC cache connector availability poll reports finalized block (`data/grpc.go:368`) | telemetry/metrics.go:85 |
| `erpc_cache_connector_earliest_block_timestamp_seconds` | gauge | connector, network | same poll, earliest block unix timestamp (`data/grpc.go:344`) | telemetry/metrics.go:91 |
| `erpc_cache_connector_latest_block_timestamp_seconds` | gauge | connector, network | same poll, latest block unix timestamp (`data/grpc.go:360`) | telemetry/metrics.go:97 |
| `erpc_cache_connector_finalized_block_timestamp_seconds` | gauge | connector, network | same poll, finalized block unix timestamp (`data/grpc.go:370`) | telemetry/metrics.go:103 |
| `erpc_network_latest_block_timestamp_distance_seconds` | gauge | project, network, origin | now − latest block timestamp; origin=`evm_state_poller` (`health/tracker.go:1315`) or origin=`network_response` from eth_getBlockByNumber post-forward (`architecture/evm/eth_getBlockByNumber.go:96`) | telemetry/metrics.go:109 |
| `erpc_network_dynamic_block_time_milliseconds` | gauge | project, network | EMA block-time estimate recomputed from on-chain timestamps (`health/tracker.go:1456`; sanity-bounded 10ms–120s) | telemetry/metrics.go:115 |
| `erpc_network_served_tip_block_number` | gauge | project, network, lane, axis | served-tip pick published per axis (latest/finalized); lane="all" = network-wide, named lane = use-upstream group (`erpc/networks.go:821`) | telemetry/metrics.go:130 |
| `erpc_network_served_tip_lag_blocks` | gauge | project, network, lane, axis | blocks served tip sits behind freshest velocity-eligible upstream at pick time; series ABSENT in default MAX mode (`erpc/networks.go:834`) | telemetry/metrics.go:145 |
| `erpc_network_served_tip_upstream_excluded_total` | counter | project, network, upstream, axis, reason | upstream excluded from served-tip pick, reason=velocity\|outlier; absent in MAX mode (`erpc/networks.go:845,849`) | telemetry/metrics.go:158 |
| `erpc_upstream_cordoned` | gauge | project, vendor, network, upstream, category, reason | health tracker sets 1/0 on cordon/uncordon state (`health/tracker.go:462`) | telemetry/metrics.go:164 |
| `erpc_selection_position` | gauge | project, network, method, upstream | selection-policy tick output: 0=primary, 1+=runner-up, −1=excluded (`internal/policy/slot.go:477,495`) | telemetry/metrics.go:174 |
| `erpc_selection_rejection_total` | counter | project, network, method, upstream, step | tick rejected upstream at a specific std-lib step (`internal/policy/slot.go:500`) | telemetry/metrics.go:180 |
| `erpc_selection_primary_switch_total` | counter | project, network, method, from, to | primary upstream changed between ticks (`internal/policy/slot.go:583`) | telemetry/metrics.go:186 |
| `erpc_selection_eval_duration_seconds` | histogram | project, network, method | per-tick selection-policy eval latency; buckets 0.0005…1 (`internal/policy/slot.go:457`) | telemetry/metrics.go:192 |
| `erpc_selection_eval_errors_total` | counter | project, network, method, kind | eval failure: timeout/throw/invalid_return/fallback_default (`internal/policy/slot.go:467`) | telemetry/metrics.go:199 |
| `erpc_selection_eligible_upstreams` | gauge | project, network, method | count of upstreams returned by most recent tick (`internal/policy/slot.go:471`) | telemetry/metrics.go:205 |
| `erpc_selection_score` | gauge | project, network, method, upstream | per-upstream `sortByScore` score (lower=better); missing for upstreams that bypassed scoring (`internal/policy/slot.go:483`) | telemetry/metrics.go:221 |
| `erpc_selection_exclusion_total` | counter | project, network, method, upstream, reason | exclusion event, reason=leaf-predicate slug; compound predicates emit one increment per tripped leaf (`internal/policy/slot.go:508`) | telemetry/metrics.go:227 |
| `erpc_selection_shadow_exclusion_total` | counter | project, network, method, upstream, reason | `shadowExcludeIf` would-have-excluded event; upstream stays in rotation (`internal/policy/slot.go:535`) | telemetry/metrics.go:233 |
| `erpc_selection_excluded_seconds` | gauge | project, network, method, upstream | wall-clock seconds continuously excluded; 0 when in rotation (`internal/policy/slot.go:488,524`) | telemetry/metrics.go:239 |
| `erpc_selection_readmit_total` | counter | project, network, method, upstream | excluded → in-list transition (`internal/policy/slot.go:556`) | telemetry/metrics.go:245 |
| `erpc_selection_readmit_age_seconds` | histogram | project, network, method | now − excludedSince at readmit; buckets 1…3600 (`internal/policy/slot.go:560`) | telemetry/metrics.go:251 |
| `erpc_selection_sticky_hold_total` | counter | project, network, method, upstream | tick where `stickyPrimary` held a primary that would otherwise flip (`internal/policy/slot.go:570`) | telemetry/metrics.go:258 |
| `erpc_selection_probe_requests_total` | counter | network, upstream, method | probe-mirror request fired at a currently-excluded upstream (`internal/policy/prober.go:398`) | telemetry/metrics.go:269 |
| `erpc_selection_probe_errors_total` | counter | network, upstream, method, reason | probe-mirror request errored; reason ∈ {timeout, throttled, auth, skipped, error} (`internal/policy/prober.go:416`) | telemetry/metrics.go:275 |
| `erpc_selection_probe_skipped_total` | counter | network, reason | probe candidate skipped pre-fire; reason ∈ {write_method, opt_out, sampled_out, max_concurrent, no_method} (`internal/policy/prober.go:200,208,254,274,289`) | telemetry/metrics.go:281 |
| `erpc_selection_probe_dropped_total` | counter | network, reason | probe-bus publish dropped, per-network feed channel full (`internal/policy/prober.go:161`) | telemetry/metrics.go:287 |
| `erpc_upstream_cordon_event_total` | counter | project, network, upstream, action | admin cordon/uncordon transition; action ∈ {cordon, uncordon} (`health/tracker.go:812,843`) | telemetry/metrics.go:293 |
| `erpc_upstream_cordon_duration_seconds` | histogram | project, network, upstream | observed on each uncordon: time spent cordoned; buckets 1…86400 (`health/tracker.go:838`) | telemetry/metrics.go:299 |
| `erpc_upstream_stale_latest_block_total` | counter | project, vendor, network, upstream, category | upstream returned a stale-vs-others latest block (`architecture/evm/eth_blockNumber.go:76`, `architecture/evm/eth_getBlockByNumber.go:173`) | telemetry/metrics.go:306 |
| `erpc_upstream_stale_finalized_block_total` | counter | project, vendor, network, upstream | upstream returned a stale-vs-others finalized block (`architecture/evm/eth_getBlockByNumber.go:232`) | telemetry/metrics.go:312 |
| `erpc_upstream_stale_upper_bound_total` | counter | project, vendor, network, upstream, category, confidence | request skipped: upstream latest block < requested upper bound (`upstream/upstream.go:1002,1029,1073`) | telemetry/metrics.go:318 |
| `erpc_upstream_stale_lower_bound_total` | counter | project, vendor, network, upstream, category, confidence | request skipped: requested lower bound below upstream's available range (`upstream/upstream.go:985,1268,1295`) | telemetry/metrics.go:324 |
| `erpc_network_evm_get_logs_split_success_total` | counter | project, network, user, agent_name | a split eth_getLogs sub-request succeeded (`architecture/evm/eth_getLogs.go:764`) | telemetry/metrics.go:330 |
| `erpc_network_evm_get_logs_split_failure_total` | counter | project, network, user, agent_name | a split eth_getLogs sub-request failed (`architecture/evm/eth_getLogs.go:684,709,723,737,751`) | telemetry/metrics.go:336 |
| `erpc_network_evm_get_logs_forced_splits_total` | counter | project, network, dimension, user, agent_name | eth_getLogs forcibly split, dimension ∈ {block_range, addresses, topics} (`architecture/evm/eth_getLogs.go:565,585,604`) | telemetry/metrics.go:342 |
| `erpc_network_evm_trace_filter_split_success_total` | counter | project, network, method, user, agent_name | split trace_filter/arbtrace_filter sub-request succeeded (`architecture/evm/trace_filter.go:573`) | telemetry/metrics.go:348 |
| `erpc_network_evm_trace_filter_split_failure_total` | counter | project, network, method, user, agent_name | split trace_filter/arbtrace_filter sub-request failed (`architecture/evm/trace_filter.go:502`) | telemetry/metrics.go:354 |
| `erpc_network_evm_trace_filter_forced_splits_total` | counter | project, network, method, dimension, user, agent_name | trace_filter split, dimension ∈ {block_range, from_address, to_address} (`architecture/evm/trace_filter.go:424,444,463`) | telemetry/metrics.go:360 |
| `erpc_upstream_latest_block_polled_total` | counter | project, vendor, network, upstream | state poller proactively polled latest block (`architecture/evm/evm_state_poller.go:406`) | telemetry/metrics.go:366 |
| `erpc_upstream_finalized_block_polled_total` | counter | project, vendor, network, upstream | state poller proactively polled finalized block (`architecture/evm/evm_state_poller.go:508`) | telemetry/metrics.go:372 |
| `erpc_upstream_block_head_large_rollback` | gauge | project, vendor, network, upstream | block head rolled back by a large delta vs same upstream's previous value (`health/tracker.go:474`) | telemetry/metrics.go:378 |
| `erpc_upstream_wrong_empty_response_total` | counter | project, vendor, network, upstream, category, finality, user, agent_name | upstream returned empty while others returned data (`erpc/networks.go:1555`) | telemetry/metrics.go:384 |
| `erpc_grpc_bds_hard_timeout_total` | counter | project, upstream, method | BDS gRPC call hit the bounded-wait hard timeout (wedged H2 stream indicator) (`clients/grpc_bds_resilience.go:170`) | telemetry/metrics.go:393 |
| `erpc_grpc_bds_conn_replacements_total` | counter | project, upstream | BDS pool connection force-closed by stuck-call watchdog (`clients/grpc_bds_resilience.go:236`) | telemetry/metrics.go:401 |
| `erpc_network_request_received_total` | counter | project, network, category, finality, user, agent_name | request received for a network (`erpc/projects.go:123`) | telemetry/metrics.go:407 |
| `erpc_network_multiplexed_request_total` | counter | project, network, category, finality, user, agent_name | request de-duplicated into an in-flight identical request (`erpc/networks.go:2030`) | telemetry/metrics.go:413 |
| `erpc_network_static_response_served_total` | counter | project, network, category | request served from configured static response, no upstream touched (`erpc/networks_static_responses.go:56`) | telemetry/metrics.go:419 |
| `erpc_network_hedged_request_total` | counter | project, network, upstream, category, attempt, finality, user, agent_name | hedged request fired (`erpc/networks.go:1288`) | telemetry/metrics.go:425 |
| `erpc_network_hedge_discards_total` | counter | project, network, upstream, category, attempt, hedge, finality, user, agent_name | hedged request discarded (wasted work) (`erpc/networks.go:1897`) | telemetry/metrics.go:431 |
| `erpc_network_timeout_fired_total` | counter | project, network, category, finality, scope | timeout policy killed a request; scope=network (`erpc/networks.go:1440`) or scope=upstream (`upstream/upstream.go:845`); suppressed when retry-exhausted error wins | telemetry/metrics.go:437 |
| `erpc_upstream_selection_total` | counter | project, network, upstream, category, reason, finality | upstream picked for an attempt; reason ∈ {primary, retry, hedge, consensus_slot, sweep} (`upstream/upstream.go:561`) | telemetry/metrics.go:447 |
| `erpc_upstream_attempt_outcome_total` | counter | project, network, upstream, category, outcome, is_hedge, is_retry, finality | attempt terminal outcome; outcome ∈ {success, empty, transport_error, server_error, client_error, rate_limited, missing_data, exec_revert, block_unavailable, breaker_open, cancelled, timeout, skipped} (`upstream/upstream.go:551`) | telemetry/metrics.go:457 |
| `erpc_network_retry_attempt_total` | counter | project, network, category, reason, finality | network-scope retry attempt; reason ∈ {empty_result, pending_tx, retryable_error, block_unavailable, missing_data} (`erpc/network_executor.go:292`) | telemetry/metrics.go:466 |
| `erpc_network_hedge_winner_total` | counter | project, network, upstream, category, finality | hedge race won by upstream whose response was kept (`erpc/network_executor.go:564`) | telemetry/metrics.go:476 |
| `erpc_upstream_breaker_state_change_total` | counter | project, upstream, transition | circuit-breaker transition; transition ∈ {closed_to_open, half_open_to_open, half_open_to_closed, open_to_half_open} (`upstream/upstream.go:93`) | telemetry/metrics.go:485 |
| `erpc_network_failed_request_total` | counter | project, network, category, attempt, error, severity, finality, user, agent_name | request failed at network/project level (`erpc/projects.go:231`) | telemetry/metrics.go:491 |
| `erpc_network_successful_request_total` | counter | project, network, vendor, upstream, category, attempt, finality, emptyish, user, agent_name | request succeeded at network/project level (`erpc/projects.go:192`) | telemetry/metrics.go:497 |
| `erpc_rate_limits_total` | counter | project, network, vendor, upstream, category, finality, user, agent_name, budget, scope, auth, origin | unified rate-limit event: local budget deny (`upstream/ratelimiter_budget.go:199,237`) or remote upstream 429 with budget="\<remote\>", scope=remote, origin=upstream (`health/tracker.go:381`; series idle-swept at `health/tracker.go:661`) | telemetry/metrics.go:503 |
| `erpc_rate_limiter_budget_max_count` | gauge | budget, method, scope | budget's allowed req/s set on rule creation/update incl. auto-tuner (`upstream/ratelimiter_budget.go:94,138`, `upstream/ratelimiter_registry.go:201`) | telemetry/metrics.go:509 |
| `erpc_rate_limiter_budget_decision_total` | counter | project, network, category, finality, user, agent_name, budget, method, scope, decision | **DEPRECATED, dormant** — replaced by `erpc_rate_limits_total`; registered but zero call sites in production code (repo grep) | telemetry/metrics.go:515 |
| `erpc_rate_limiter_failopen_total` | counter | project, network, user, agent_name, budget, category, reason | rate limiter failed open (request allowed due to error/timeout) (`upstream/ratelimiter_budget.go:367,454`) | telemetry/metrics.go:521 |
| `erpc_rate_limiter_remote_inflight` | gauge | budget | concurrent in-flight remote (e.g. Redis) DoLimit calls per budget (`upstream/ratelimiter_registry.go:176`) | telemetry/metrics.go:533 |
| `erpc_rate_limiter_remote_admission_shedded_total` | counter | budget | remote check fail-opened because admission semaphore full (load shed, remote never attempted) (`upstream/ratelimiter_registry.go:177`) | telemetry/metrics.go:544 |
| `erpc_cache_set_success_total` | counter | project, network, category, connector, policy, ttl | cache set succeeded (`architecture/evm/json_rpc_cache.go:794`) | telemetry/metrics.go:550 |
| `erpc_cache_set_error_total` | counter | project, network, category, connector, policy, ttl, error | cache set errored (`architecture/evm/json_rpc_cache.go:700,775`) | telemetry/metrics.go:556 |
| `erpc_cache_set_skipped_total` | counter | project, network, category, connector, policy, ttl | cache set skipped by policy (`architecture/evm/json_rpc_cache.go:722`) | telemetry/metrics.go:562 |
| `erpc_cache_get_success_hit_total` | counter | project, network, category, connector, policy, ttl | cache get hit (`architecture/evm/json_rpc_cache.go:536`) | telemetry/metrics.go:568 |
| `erpc_cache_get_success_miss_total` | counter | project, network, category, connector, policy, ttl | cache get miss (`architecture/evm/json_rpc_cache.go:483,507`) | telemetry/metrics.go:574 |
| `erpc_cache_get_error_total` | counter | project, network, category, connector, policy, ttl, error | cache get errored (`architecture/evm/json_rpc_cache.go:294`) | telemetry/metrics.go:580 |
| `erpc_cache_get_skipped_total` | counter | project, network, category | cache get skipped — no matching policy (`architecture/evm/json_rpc_cache.go:189`) | telemetry/metrics.go:586 |
| `erpc_cache_get_age_guard_reject_total` | counter | project, network, method, connector, policy, ttl | cached item rejected: block-timestamp age exceeded policy TTL (`architecture/evm/json_rpc_cache.go:910`) | telemetry/metrics.go:592 |
| `erpc_cache_set_original_bytes_total` | counter | project, network, category, connector, policy, ttl | uncompressed bytes written on cache set (`architecture/evm/json_rpc_cache.go:736`) | telemetry/metrics.go:598 |
| `erpc_cache_set_compressed_bytes_total` | counter | project, network, category, connector, policy, ttl | compressed bytes written on cache set (`architecture/evm/json_rpc_cache.go:756`) | telemetry/metrics.go:604 |
| `erpc_cors_requests_total` | counter | project, origin | CORS request received (`erpc/http_server.go:1020`) | telemetry/metrics.go:610 |
| `erpc_cors_preflight_requests_total` | counter | project, origin | CORS preflight (OPTIONS) received (`erpc/http_server.go:1069`) | telemetry/metrics.go:616 |
| `erpc_cors_disallowed_origin_total` | counter | project, origin | CORS request from disallowed origin (`erpc/http_server.go:1038`) | telemetry/metrics.go:622 |
| `erpc_ristretto_cache_current_cost` | gauge | connector | Ristretto (memory connector) current total cost, updated by poll loop (`data/memory.go:273`) | telemetry/metrics.go:628 |
| `erpc_ristretto_cache_sets_failed_total` | counter | connector | Ristretto set dropped/rejected (`data/memory.go:281`) | telemetry/metrics.go:634 |
| `erpc_shadow_response_identical_total` | counter | project, vendor, network, upstream, category | shadow upstream response identical to expected (`erpc/shadow.go:218`) | telemetry/metrics.go:640 |
| `erpc_shadow_response_mismatch_total` | counter | project, vendor, network, upstream, category, finality, emptyish, larger | shadow upstream response differs from expected (`erpc/shadow.go:238`) | telemetry/metrics.go:646 |
| `erpc_shadow_response_error_total` | counter | project, vendor, network, upstream, category, error | shadow upstream request errored (`erpc/shadow.go:121,141,195`) | telemetry/metrics.go:652 |
| `erpc_auth_failed_total` | counter | project, network, strategy, reason, agent_name | failed authentication attempt (`auth/strategy_database.go:467`) | telemetry/metrics.go:659 |
| `erpc_consensus_total` | counter | project, network, category, outcome, finality | consensus round completed, by outcome (`consensus/executor.go:329,1301,1345`) | telemetry/metrics.go:665 |
| `erpc_consensus_misbehavior_detected_total` | counter | project, network, upstream, category, finality, response_type, larger_than_consensus | upstream returned different data (not errors) than consensus (`consensus/executor.go:968`) | telemetry/metrics.go:671 |
| `erpc_consensus_upstream_punished_total` | counter | project, network, upstream | upstream punished after misbehavior threshold (`consensus/executor.go:1216`) | telemetry/metrics.go:677 |
| `erpc_consensus_short_circuit_total` | counter | project, network, category, reason, finality | consensus round short-circuited (`consensus/executor.go:578`) | telemetry/metrics.go:683 |
| `erpc_consensus_wait_capped_total` | counter | project, network, category, trigger, finality | round resolved early because maxWaitOnResult/maxWaitOnEmpty fired (`consensus/executor.go:593`) | telemetry/metrics.go:694 |
| `erpc_consensus_errors_total` | counter | project, network, category, error, finality | consensus-level error by type (`consensus/executor.go:1307,1363`) | telemetry/metrics.go:700 |
| `erpc_consensus_upstream_errors_total` | counter | project, network, upstream, category, finality, response_type, error_code | participant upstream errored during consensus (`consensus/executor.go:945`) | telemetry/metrics.go:706 |
| `erpc_consensus_panics_total` | counter | project, network, category, finality | panic recovered inside consensus (`consensus/executor.go:389,650`) | telemetry/metrics.go:712 |
| `erpc_consensus_cancellations_total` | counter | project, network, category, phase, finality | context cancelled during consensus, by phase (`consensus/executor.go:326,657,671`) | telemetry/metrics.go:718 |
| `erpc_network_evm_block_range_requested_total` | counter | project, network, vendor, upstream, category, user, finality, bucket, size | block-number heatmap: request observed per block bucket (bucket size `EvmBlockRangeBucketSize` = 100000, telemetry/metrics.go:740) (`erpc/block_heatmap.go:74`) | telemetry/metrics.go:724 |

## 2. Filter-aware histograms (`*LabeledHistogram`, built by `buildFilterAwareHistograms`, registered by `SetHistogramBuckets` — telemetry/metrics.go:755-976)

These honor the histogram label filter (`telemetry/labeled_histogram.go:16-71`): labels listed below are the FULL schema; the registered label set is schema minus `metrics.histogramDropLabels` plus per-metric `metrics.histogramLabelOverrides` (wired in `erpc/init.go:53`). Call sites always pass the full schema; `WithLabelValues` projects to the active subset and panics on length mismatch (`telemetry/labeled_histogram.go:124-137`).

| metric name | type | labels (full schema) | when it fires | defined at |
|---|---|---|---|---|
| `erpc_upstream_request_duration_seconds` | histogram | project, vendor, network, upstream, category, composite, finality, user | duration of each actual upstream request, observed via health-tracker cached observer (`health/tracker.go:333-358`); idle series deleted by sweep (`health/tracker.go:641-652`) | telemetry/metrics.go:785 |
| `erpc_network_request_duration_seconds` | histogram | project, network, vendor, upstream, category, finality, user | end-to-end network request duration on success (`erpc/projects.go:211`) and failure with vendor/upstream="\<error\>" (`erpc/projects.go:242`) | telemetry/metrics.go:792 |
| `erpc_network_evm_get_logs_range_requested` | histogram | project, network, category, user, finality | eth_getLogs requested block-range size; buckets 1…30000 (`architecture/evm/eth_getLogs.go:118`) | telemetry/metrics.go:799 |
| `erpc_network_evm_trace_filter_range_requested` | histogram | project, network, method, user, finality | trace_filter/arbtrace_filter requested block-range size; buckets 1…30000 (`architecture/evm/trace_filter.go:99`) | telemetry/metrics.go:806 |
| `erpc_network_hedge_delay_seconds` | histogram | project, network, category, finality | **DORMANT** — registered, but no production observe site (only `telemetry/labeled_histogram_test.go:151`); buckets 0.01…3 | telemetry/metrics.go:813 |
| `erpc_network_timeout_duration_seconds` | histogram | project, network, category, finality | dynamic timeout duration computed per request; buckets 0.05…30 (`common/timeout_func.go:74`) | telemetry/metrics.go:820 |
| `erpc_network_data_unavailable_wait_seconds` | histogram | project, network, category, reason, finality | wall-clock catch-up delay before a data-not-yet-available retry (reasons block_unavailable/empty_result/missing_data); dedicated buckets `CatchUpWaitHistogramBuckets` 0.1…64 (telemetry/metrics.go:752) (`erpc/network_executor.go:324`) | telemetry/metrics.go:835 |
| `erpc_consensus_responses_collected` | histogram | project, network, category, vendors, short_circuited, finality | responses collected before consensus decision; linear buckets 1..10 (`consensus/executor.go:563`) | telemetry/metrics.go:844 |
| `erpc_consensus_agreement_count` | histogram | project, network, category, finality | upstreams agreeing on most common result; linear buckets 1..10 (`consensus/executor.go:1349`) | telemetry/metrics.go:851 |
| `erpc_consensus_duration_seconds` | histogram | project, network, category, outcome, finality | consensus round duration; global buckets (`consensus/executor.go:332,1304,1346`) | telemetry/metrics.go:858 |
| `erpc_cache_set_success_duration_seconds` | histogram | project, network, category, connector, policy, ttl | cache set success duration (`architecture/evm/json_rpc_cache.go:802`) | telemetry/metrics.go:865 |
| `erpc_cache_set_error_duration_seconds` | histogram | project, network, category, connector, policy, ttl, error | cache set error duration (`architecture/evm/json_rpc_cache.go:709,784`) | telemetry/metrics.go:872 |
| `erpc_cache_get_success_hit_duration_seconds` | histogram | project, network, category, connector, policy, ttl | cache get hit duration (`architecture/evm/json_rpc_cache.go:544`) | telemetry/metrics.go:879 |
| `erpc_cache_get_success_miss_duration_seconds` | histogram | project, network, category, connector, policy, ttl | cache get miss duration (`architecture/evm/json_rpc_cache.go:491,515`) | telemetry/metrics.go:886 |
| `erpc_cache_get_error_duration_seconds` | histogram | project, network, category, connector, policy, ttl, error | cache get error duration (`architecture/evm/json_rpc_cache.go:303`) | telemetry/metrics.go:893 |
| `erpc_rate_limiter_remote_duration_seconds` | histogram | budget, result | remote rate-limit check (e.g. Redis DoLimit) duration; fine sub-second buckets 0.001…5 (`upstream/ratelimiter_registry.go:178-181`) | telemetry/metrics.go:903 |
| `erpc_upstream_response_size_bytes` | histogram | project, network, category, finality | decoded post-gzip result-body size of upstream JSON-RPC responses; coarse buckets 4KB/64KB/1MB/16MB/100MB (`upstream/upstream.go:654`) | telemetry/metrics.go:924 |

### Histogram bucket sets (for cross-reference)

| bucket set | values | applies to | source |
|---|---|---|---|
| `DefaultHistogramBuckets` (overridable via `metrics.histogramBuckets` comma string) | 0.05, 0.5, 5, 30 | upstream_request_duration, network_request_duration, consensus_duration, all 5 cache duration histograms | telemetry/metrics.go:731-736, parse: 997-1015; config wiring `erpc/init.go:47-57`, `erpc/config_analyzer.go:285,982` |
| `EvmGetLogsRangeHistogramBuckets` | 1, 10, 100, 500, 1000, 5000, 10000, 30000 | get_logs_range_requested, trace_filter_range_requested | telemetry/metrics.go:743 |
| `CatchUpWaitHistogramBuckets` | 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64 | network_data_unavailable_wait_seconds | telemetry/metrics.go:752 |
| hedge delay | 0.01, 0.03, 0.05, 0.2, 0.3, 0.5, 0.7, 1, 3 | network_hedge_delay_seconds | telemetry/metrics.go:817 |
| timeout duration | 0.05, 0.1, 0.3, 0.5, 1, 3, 5, 10, 30 | network_timeout_duration_seconds | telemetry/metrics.go:824 |
| selection eval | 0.0005, 0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1 | selection_eval_duration_seconds | telemetry/metrics.go:196 |
| readmit age | 1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600 | selection_readmit_age_seconds | telemetry/metrics.go:255 |
| cordon duration | 1, 10, 60, 300, 900, 1800, 3600, 7200, 21600, 86400 | upstream_cordon_duration_seconds | telemetry/metrics.go:303 |
| rate limiter remote | 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5 | rate_limiter_remote_duration_seconds | telemetry/metrics.go:907 |
| response size | 4096, 65536, 1048576, 16777216, 104857600 | upstream_response_size_bytes | telemetry/metrics.go:928 |
| consensus counts | LinearBuckets(1,1,10) = 1..10 | consensus_responses_collected, consensus_agreement_count | telemetry/metrics.go:848,855 |

## 3. Metrics registered elsewhere in the codebase

Repo-wide grep result: **none**. Every metric definition lives in `telemetry/metrics.go`. Related registry plumbing found outside `telemetry/`:

- [ ] `cmd/erpc/initflags.go:22-28` — `ERPC_NOMETRICS=1` swaps `prometheus.DefaultRegisterer`/`DefaultGatherer` for a fresh empty registry before package init metrics land (also drops client_golang's default `go_*`/`process_*` collectors).
- [ ] `erpc/init.go:137-152` — `/metrics` server: `promhttp.Handler()` on `metrics.port` when `metrics.enabled`; serves `prometheus.DefaultGatherer`, which (unless ERPC_NOMETRICS) also exposes the stock `go_*` and `process_*` collectors pre-registered by client_golang's default registry — not defined in this repo. `metrics.errorLabelMode` is applied here via `common.SetErrorLabelMode` (`erpc/init.go:138-140`).
- [ ] `erpc/init.go:47-57` + `erpc/config_analyzer.go:285,982` — only callers of `telemetry.SetHistogramLabelFilter` / `telemetry.SetHistogramBuckets`.
- [ ] `telemetry/labeled_histogram.go:90-97` (`RegisterOrReplaceHistogram`, uses `prometheus.MustRegister`) and `telemetry/metrics.go:984-995` (`registerOrReuse`, uses `prometheus.Register` and reuses on `AlreadyRegisteredError`) — internal registration helpers; re-calling `SetHistogramBuckets` with a CHANGED filter panics (telemetry/metrics.go:942-950).
- [ ] `telemetry/handles.go:59-98` — `CounterHandle`/`GaugeHandle`/`ObserverHandle` cache label-bound children keyed by Vec pointer + `\x1f`-joined labels; `ObserverHandle` keys on post-filter labels for LabeledHistograms; `ResetHandleCache` (telemetry/handles.go:101-105) is invoked by `SetHistogramBuckets` (telemetry/metrics.go:973).
- [ ] Series eviction: `LabeledHistogram.DeleteLabelValues` (telemetry/labeled_histogram.go:155-168) + counter deletes are used by the health tracker idle sweep (`health/tracker.go:641-668`) to bound /metrics cardinality under method-flood.
- [ ] `test/fake_erpc.go:402` — test-only Gather() of the default registry (no definitions).

Dormant definitions to remember when writing docs:

- [ ] `erpc_rate_limiter_budget_decision_total` — deprecated, zero call sites (telemetry/metrics.go:515-519).
- [ ] `erpc_network_hedge_delay_seconds` — registered but never observed in production code; only touched by `telemetry/labeled_histogram_test.go:151`.

## 4. Tracing span names (OTel)

No central name constants; names are string literals at call sites, all routed through `common.StartSpan` (emitted whenever `tracing.enabled`) or `common.StartDetailSpan` (only when `tracing.enabled && tracing.detailed`) — helpers in `common/tracing_util.go:32-47`; tracer init/sampler/force-trace in `common/tracing_core.go:50-150,228-273` (instrumentation name `github.com/erpc/erpc`, common/tracing_core.go:26). `Request.Handle` is started directly via `tracer.Start` in `StartRequestSpan` (`common/tracing_util.go:165`). Force-trace bypasses sampling via header `X-ERPC-Force-Trace`, query param `force-trace`, or `tracing.forceTraceMatchers` (common/tracing_core.go:28-35,279-330). 126 unique span names:

| span name | mode | call site(s) |
|---|---|---|
| `Cache.FindGetPolicies` | detail | architecture/evm/json_rpc_cache.go:L173 |
| `Cache.Get` | normal | architecture/evm/json_rpc_cache.go:L153 |
| `Cache.GetForPolicy` | detail | architecture/evm/json_rpc_cache.go:L241 |
| `Cache.Set` | normal | architecture/evm/json_rpc_cache.go:L572 |
| `ConnectorFailsafe.Delete` | detail | data/failsafe.go:L286 |
| `ConnectorFailsafe.Get` | detail | data/failsafe.go:L224 |
| `ConnectorFailsafe.Set` | detail | data/failsafe.go:L253 |
| `Consensus.CollectResponses` | detail | consensus/executor.go:L189 |
| `Consensus.Run` | normal | consensus/executor.go:L1282 |
| `CounterInt64.TryUpdate` | normal | data/shared_state_variable.go:L365 |
| `CounterInt64.TryUpdateIfStale` | normal | data/shared_state_variable.go:L391 |
| `CounterInt64.TryUpdateIfStale.AcquireMutex` | normal | data/shared_state_variable.go:L405 |
| `CounterInt64.TryUpdateIfStale.ExecuteRefresh` | normal | data/shared_state_variable.go:L430 |
| `DynamoDBConnector.Delete` | normal | data/dynamodb.go:L833 |
| `DynamoDBConnector.Get` | normal | data/dynamodb.go:L414 |
| `DynamoDBConnector.List` | normal | data/dynamodb.go:L874 |
| `DynamoDBConnector.Lock` | normal | data/dynamodb.go:L579 |
| `DynamoDBConnector.Set` | normal | data/dynamodb.go:L355 |
| `DynamoDBConnector.Unlock` | normal | data/dynamodb.go:L691 |
| `DynamoDBConnector.getSimpleValue` | detail | data/dynamodb.go:L766 |
| `Evm.ExtractBlockReferenceFromRequest` | detail | architecture/evm/block_ref.go:L19 |
| `Evm.ExtractBlockReferenceFromResponse` | detail | architecture/evm/block_ref.go:L118 |
| `Evm.ExtractBlockTimestampFromResponse` | detail | architecture/evm/block_ref.go:L191 |
| `Evm.PickHighestBlock` | detail | architecture/evm/eth_getBlockByNumber.go:L336 |
| `Evm.extractRefFromJsonRpcRequest` | detail | architecture/evm/block_ref.go:L240 |
| `Evm.extractRefFromJsonRpcResponse` | detail | architecture/evm/block_ref.go:L314 |
| `EvmStatePoller.PollFinalizedBlockNumber` | detail | architecture/evm/evm_state_poller.go:L491 |
| `EvmStatePoller.PollLatestBlockNumber` | detail | architecture/evm/evm_state_poller.go:L394 |
| `GrpcBdsClient.GetBlockByHash` | detail | clients/grpc_bds_client.go:L365 |
| `GrpcBdsClient.GetBlockByNumber` | detail | clients/grpc_bds_client.go:L422 |
| `GrpcBdsClient.GetLogs` | detail | clients/grpc_bds_client.go:L632 |
| `GrpcBdsClient.QueryBlocks` | detail | clients/grpc_bds_client.go:L1097 |
| `GrpcBdsClient.QueryLogs` | detail | clients/grpc_bds_client.go:L1173 |
| `GrpcBdsClient.QueryTraces` | detail | clients/grpc_bds_client.go:L1209 |
| `GrpcBdsClient.QueryTransactions` | detail | clients/grpc_bds_client.go:L1138 |
| `GrpcBdsClient.QueryTransfers` | detail | clients/grpc_bds_client.go:L1245 |
| `GrpcBdsClient.SendRequest` | normal | clients/grpc_bds_client.go:L194 |
| `Http.ParseRequests` | detail | erpc/http_server.go:L397 |
| `Http.ReadBody` | detail | erpc/http_server.go:L373 |
| `Http.ReceivedRequest` | normal | common/tracing_util.go:L96 (via `StartHTTPServerSpan`) |
| `HttpJsonRpcClient.sendSingleRequest` | normal | clients/http_json_rpc_client.go:L675 |
| `HttpServer.WriteResponse` | detail | erpc/http_server.go:L680 |
| `JsonRpcRequest.Lock` | detail | common/json_rpc.go:L1229 |
| `JsonRpcRequest.RLock` | detail | common/json_rpc.go:L1235 |
| `JsonRpcResponse.IsResultEmptyish` | detail | common/json_rpc.go:L799 |
| `JsonRpcResponse.ParseFromStream` | detail | common/json_rpc.go:L288 |
| `JsonRpcResponse.PeekBytesByPath` | detail | common/json_rpc.go:L486 |
| `JsonRpcResponse.PeekStringByPath` | detail | common/json_rpc.go:L464 |
| `Multiplexer.Close` | detail | erpc/multiplexer.go:L37 |
| `Network.EnrichStatePoller` | detail | erpc/networks.go:L2146 |
| `Network.EvmHighestFinalizedBlockNumber` | detail | erpc/networks.go:L679 |
| `Network.EvmHighestLatestBlockNumber` | detail | erpc/networks.go:L518 |
| `Network.EvmLowestFinalizedBlockNumber` | detail | erpc/networks.go:L855 |
| `Network.Forward` | normal | erpc/networks.go:L943 |
| `Network.GetFinality` | detail | erpc/networks.go:L1646 |
| `Network.NormalizeResponse` | detail | erpc/networks.go:L2235 |
| `Network.PostForward.eth_getBlockByNumber` | detail | architecture/evm/eth_getBlockByNumber.go:L44 |
| `Network.PostForward.eth_sendRawTransaction` | detail | architecture/evm/eth_sendRawTransaction.go:L280 |
| `Network.PostForwardHook` | detail | architecture/evm/hooks.go:L64 |
| `Network.PreForwardHook` | detail | architecture/evm/hooks.go:L41 |
| `Network.PreForwardHook.eth_chainId` | detail | architecture/evm/eth_chainId.go:L79 |
| `Network.TryForward` | detail | erpc/networks.go:L1137 |
| `Network.UpstreamLoop` | detail | erpc/networks.go:L1227 |
| `Network.WaitForMultiplexResult` | normal | erpc/networks.go:L2058 |
| `Network.forwardAttempt` | normal | erpc/networks.go:L1188 |
| `PolicyEngine.GetOrdered` | detail | erpc/networks.go:L1023 |
| `PostgreSQLConnector.Delete` | normal | data/postgresql.go:L1130 |
| `PostgreSQLConnector.Get` | normal | data/postgresql.go:L466 |
| `PostgreSQLConnector.List` | normal | data/postgresql.go:L1165 |
| `PostgreSQLConnector.Lock` | normal | data/postgresql.go:L526 |
| `PostgreSQLConnector.PublishCounterInt64` | normal | data/postgresql.go:L683 |
| `PostgreSQLConnector.Set` | normal | data/postgresql.go:L409 |
| `PostgreSQLConnector.Unlock` | normal | data/postgresql.go:L589 |
| `PostgreSQLConnector.getCurrentValue` | detail | data/postgresql.go:L974 |
| `PostgreSQLConnector.getWithWildcard` | detail | data/postgresql.go:L1006 |
| `Project.Forward` | detail | erpc/projects.go:L103 |
| `Project.PreForwardHook` | detail | architecture/evm/hooks.go:L14 |
| `Project.PreForwardHook.eth_blockNumber` | detail | architecture/evm/eth_blockNumber.go:L16 |
| `Project.PreForwardHook.eth_chainId` | detail | architecture/evm/eth_chainId.go:L30 |
| `Project.executeShadowRequest` | detail | erpc/shadow.go:L82 |
| `Query.Execute` | detail | erpc/query_executor.go:L45, L78, L111, L144, L177 |
| `Query.ForwardSubrequest` | detail | erpc/query_shim.go:L427 |
| `Query.ResolveQueryBounds` | detail | erpc/query_executor.go:L277 |
| `Query.ShimBlocks` | detail | erpc/query_shim.go:L18 |
| `Query.ShimLogs` | detail | erpc/query_shim.go:L112 |
| `Query.ShimTraces` | detail | erpc/query_shim.go:L187 |
| `Query.ShimTransactions` | detail | erpc/query_shim.go:L55 |
| `QueryStream.Handle` | normal | erpc/request_processor.go:L76 |
| `RateLimiter.DoLimit` | normal | upstream/ratelimiter_budget.go:L279 |
| `RateLimiter.TryAcquirePermit` | detail | upstream/ratelimiter_budget.go:L161 |
| `RedisConnector.Delete` | normal | data/redis.go:L636 |
| `RedisConnector.Get` | normal | data/redis.go:L341 |
| `RedisConnector.List` | normal | data/redis.go:L682 |
| `RedisConnector.Lock` | normal | data/redis.go:L435 |
| `RedisConnector.PublishCounterInt64` | normal | data/redis.go:L558 |
| `RedisConnector.Set` | normal | data/redis.go:L281 |
| `RedisConnector.Unlock` | normal | data/redis.go:L612 |
| `Request.GenerateCacheHash` | detail | common/json_rpc.go:L1387 |
| `Request.Handle` | direct (`tracer.Start`, fires whenever tracing enabled) | common/tracing_util.go:L165 (via `StartRequestSpan`) |
| `Request.Lock` | detail | common/request.go:L925 |
| `Request.RLock` | detail | common/request.go:L931 |
| `Request.ResolveJsonRpc` | detail | common/request.go:L939 |
| `Response.IsObjectNull` | detail | common/response.go:L420 |
| `Response.Lock` | detail | common/response.go:L84 |
| `Response.RLock` | detail | common/response.go:L90 |
| `Response.ResolveJsonRpc` | detail | common/response.go:L293 |
| `Upstream.Forward` | normal | upstream/upstream.go:L416 |
| `Upstream.PostForwardHook` | detail | architecture/evm/hooks.go:L110 |
| `Upstream.PostForwardHook.eth_getBlockByNumber` | detail | architecture/evm/eth_getBlockByNumber.go:L435 |
| `Upstream.PostForwardHook.eth_getBlockReceipts` | detail | architecture/evm/eth_getBlockReceipts.go:L31 |
| `Upstream.PostForwardHook.eth_getLogs` | detail | architecture/evm/eth_getLogs.go:L350 |
| `Upstream.PostForwardHook.eth_sendRawTransaction` | detail | architecture/evm/eth_sendRawTransaction.go:L59 |
| `Upstream.PostForwardHook.trace_filter` | detail | architecture/evm/trace_filter.go:L362 |
| `Upstream.PreForwardHook` | detail | architecture/evm/hooks.go:L87 |
| `Upstream.PreForwardHook.eth_chainId` | detail | architecture/evm/eth_chainId.go:L124 |
| `Upstream.PreForwardHook.eth_getLogs` | detail | architecture/evm/eth_getLogs.go:L289 |
| `Upstream.PreForwardHook.trace_filter` | detail | architecture/evm/trace_filter.go:L298 |
| `Upstream.tryForward.PreRequest` | detail | upstream/upstream.go:L572 |
| `Upstream.tryForward.SendRequest` | detail | upstream/upstream.go:L630 |
| `UpstreamsRegistry.GetNetworkUpstreams` | detail | upstream/registry.go:L349 |
| `UpstreamsRegistry.GetSortedUpstreams` | detail | upstream/registry.go:L387 |
| `UpstreamsRegistry.buildProviderBootstrapTask` | detail | upstream/registry.go:L485 |
| `UpstreamsRegistry.buildUpstreamBootstrapTask` | detail | upstream/registry.go:L431 |
| `createSyntheticSuccessResponse` | detail | architecture/evm/eth_sendRawTransaction.go:L179 |
| `extractTxHashFromSendRawTransaction` | detail | architecture/evm/eth_sendRawTransaction.go:L136 |
| `verifyAndHandleNonceTooLow` | detail | architecture/evm/eth_sendRawTransaction.go:L206 |
