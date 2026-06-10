# KB: Metrics: full catalog & labels (prometheus)

> status: complete
> source-dirs: telemetry/metrics.go, telemetry/labeled_histogram.go, telemetry/handles.go, erpc/init.go, cmd/erpc/initflags.go, common/config.go (MetricsConfig), common/defaults.go (MetricsConfig.SetDefaults), common/validation.go (MetricsConfig.Validate), common/errors.go (ErrorSummary/SetErrorLabelMode), health/tracker.go (idle sweep), monitoring/prometheus/, monitoring/grafana/, monitoring/catch-up-metrics.md

## L1 — Capability (CTO view)

eRPC exposes a dedicated Prometheus `/metrics` HTTP server (default port 4001) that emits 122 metric definitions — 79 counters, 23 gauges, 3 plain histograms, and 17 filter-aware `LabeledHistogram` wrappers — all under the `erpc_` namespace. These metrics cover every observable subsystem: upstream health and latency, network-level request accounting, cache efficiency, rate limiting, consensus, selection-policy scoring, cordon/circuit-breaker state, gRPC resilience, block-range heatmaps, CORS, and in-memory cache pressure. The `LabeledHistogram` mechanism lets operators drop high-cardinality labels globally (e.g. `user`) to control scrape payload size without losing those dimensions on counters, and per-metric overrides can re-add labels selectively. An idle-eviction sweep automatically removes stale label-sets from the Prometheus registry to prevent cardinality runaway under method-flood attacks.

## L2 — Mechanics (staff-engineer view)

**Initialization order.** All 79 counters and 23 gauges are registered eagerly at package-init time via `promauto` (`telemetry/metrics.go:12-729`). The 17 `LabeledHistogram` globals are initialised to unregistered, empty-filter wrappers by `telemetry.init()` so that early-startup or test code can observe without NPE (`telemetry/metrics.go:934-940`). `erpc.Init` then calls `telemetry.SetHistogramLabelFilter` (installs the drop/override config) immediately followed by `telemetry.SetHistogramBuckets` (rebuilds every LabeledHistogram under the now-active filter and registers them with `prometheus.DefaultRegisterer`) (`erpc/init.go:47-57`). `ResetHandleCache` is called inside `SetHistogramBuckets` to invalidate cached label-bound handles so subsequent observations pick up the new Vecs (`telemetry/metrics.go:973`).

**Label filtering (`LabeledHistogram`).** Each `LabeledHistogram` stores the full canonical label schema alongside the subset of positions that survive the active `HistogramLabelFilter`. `WithLabelValues` always receives the full schema in schema order; the wrapper projects to the active subset before forwarding to the underlying `prometheus.HistogramVec` (`telemetry/labeled_histogram.go:121-137`). Call sites never need to know which labels survived. Length mismatches panic immediately to surface wiring bugs. `ActiveLabelValues` returns the projected subset so handle-caches can key on the effective (post-filter) labels, preventing multiple full-label tuples that collapse to the same series from spawning duplicate cache entries (`telemetry/labeled_histogram.go:174-187`, `telemetry/handles.go:86-98`). Changing the label filter AFTER the first `SetHistogramBuckets` call panics (Prometheus disallows label-set changes on registered metrics) (`telemetry/metrics.go:942-950`).

**Handle caching.** `CounterHandle`, `GaugeHandle`, and `ObserverHandle` in `telemetry/handles.go` cache `prometheus.Counter`/`Gauge`/`Observer` children in `sync.Map`s keyed by `{Vec pointer, '\x1f'-joined label values}`. This avoids a per-observation map lookup and mutex contention inside the Prometheus library. For `LabeledHistogram` observers, the key uses the post-filter label values so the cache is filter-aware (`telemetry/handles.go:86-98`). `ResetHandleCache` wipes all three maps — called after `SetHistogramBuckets` when Vecs are re-created (`telemetry/handles.go:101-105`).

**Idle-series eviction.** The health tracker's `rotateMetricsLoop` runs a sweep every `sweepEveryRotations` (= 10 = `rollingBuckets`) rotation ticks (`health/tracker.go:557-563`). `sweepIdle` walks `upsMetrics` and `ntwMetrics` sync.Maps and deletes entries not accessed since `idleEvictionAfter` (default 30 min) (`health/tracker.go:598-631`). `sweepIdleObservers` additionally calls `MetricUpstreamRequestDuration.DeleteLabelValues` and `MetricRateLimitsTotal.DeleteLabelValues` to remove the corresponding Prometheus series so the `/metrics` endpoint stops emitting stale label combos — Prometheus' append-only registry otherwise keeps series forever (`health/tracker.go:640-667`). Cordoned entries and wildcard (`"*"`) rollups are never evicted (`health/tracker.go:603-608`, `618-620`).

**Metrics HTTP server.** `erpc.Init` starts a minimal `http.Server` bound to `:%d` (port only, all interfaces) serving `promhttp.Handler()` (`erpc/init.go:145-158`). This means the metrics port always listens on `0.0.0.0`; the `hostV4`/`hostV6` fields in `MetricsConfig` are defined and defaulted but **not used** by the current server construction (`erpc/init.go:149`). Shutdown is graceful with a 5-second budget (`erpc/init.go:162-168`). The path is implicitly `/metrics` (promhttp default). No TLS, no auth, no gzip. The handler serves `prometheus.DefaultGatherer`, which (unless `ERPC_NOMETRICS=1`) also includes the stock `go_*` and `process_*` collectors pre-registered by client_golang's default registry.

**`ERPC_NOMETRICS=1` env var.** When set before process start, `initflags.go:init()` replaces `prometheus.DefaultRegisterer` and `DefaultGatherer` with a fresh empty registry (`cmd/erpc/initflags.go:22-28`). All subsequent `promauto`/`prometheus.Register` calls succeed but produce no-op metrics that never accumulate label-pairs. The default `go_*`/`process_*` collectors are also dropped. Only affects the binary build (`//go:build !test`).

**`errorLabelMode` global.** `common.ErrorSummary` is the function used for the `error` label value on every metric that carries one (`upstream_request_errors_total`, `network_failed_request_total`, etc.). Its output is controlled by a global `errorLabelMode` variable (`common/errors.go:17`). In `compact` mode it produces short stable codes like `ErrEndpointCapacityExceeded/ErrJsonRpcExceptionInternal/-32000`; in `verbose` mode it emits the full human message including address/number details. Default before `erpc.Init` runs is `verbose`; `erpc.Init` sets it to the config value (default `compact`) (`erpc/init.go:138-140`, `common/defaults.go:762-763`). **This affects cardinality**: verbose mode can produce unbounded unique label values from messages containing block numbers or IP addresses, while compact mode yields a bounded set of code-path identifiers.

**`networkAlias` resolver.** At startup `erpc.Init` installs a `common.NetworkAliasResolver` callback that maps raw EVM chain IDs to human-readable network aliases (`erpc/init.go:62-77`). Components that only know a numeric chainId (e.g. the gRPC cache connector) use this resolver so their metric `network` labels match the alias used by every other metric in the system.

## L3 — Exhaustive reference (agent view)

### Config fields

All under `metrics.` in the YAML root config. Source: `common/config.go:2543-2564` (struct), `common/defaults.go:749-767` (SetDefaults), `common/validation.go:131-161` (Validate).

| # | YAML path | Type | Default | Behavior / notes |
|---|-----------|------|---------|------------------|
| 1 | `metrics.enabled` | *bool | `true` in production; `nil` (= disabled) under `go test` (`common/defaults.go:750-752`, `util.IsTest()`) | Whether to start the `/metrics` HTTP server at all. When false or nil, no server is started and metrics are still registered/counted — they just aren't scraped. |
| 2 | `metrics.hostV4` | *string | `"0.0.0.0"` (`common/defaults.go:753-755`) | Defined and defaulted, but **not used** in the current server bind address; `erpc/init.go:149` binds `":%d"` (all interfaces regardless of this value). Field exists for future use or validation. |
| 3 | `metrics.listenV4` | *bool | not defaulted (nil) | Defined in struct `common/config.go:2545` but never read in production code. Dead config field. |
| 4 | `metrics.hostV6` | *string | `"[::]"` (`common/defaults.go:756-758`) | Same caveat as `hostV4` — defined but not used in bind address. |
| 5 | `metrics.listenV6` | *bool | not defaulted (nil) | Defined in struct but never read. Dead config field. |
| 6 | `metrics.port` | *int | `4001` (`common/defaults.go:759-761`) | TCP port for the `/metrics` HTTP server. Validation requires this when `metrics.enabled = true` (`common/validation.go:142-144`). `erpc.Init` aborts with an error if nil when enabled (`erpc/init.go:141-143`). Default `4001` = `httpPortV4(4000) + 1`. |
| 7 | `metrics.errorLabelMode` | LabelMode (`string`) | `"compact"` (`common/defaults.go:762-763`) | Controls the `error` label produced by `common.ErrorSummary`. `"compact"` → short stable error codes (bounded cardinality, recommended for production). `"verbose"` → full human-readable messages including potentially unique values (unbounded cardinality risk). Applied via `common.SetErrorLabelMode` in `erpc.Init` (`erpc/init.go:138-140`). Validation: must be `""`, `"verbose"`, or `"compact"` (`common/validation.go:147-149`). Note: the in-code default of `errorLabelMode` before `erpc.Init` runs is `verbose` (`common/errors.go:17`); it flips to `compact` at startup. |
| 8 | `metrics.histogramBuckets` | string | `""` → `DefaultHistogramBuckets = [0.05, 0.5, 5, 30]` (`telemetry/metrics.go:731-736`) | Comma-separated float64 bucket boundaries for `LabeledHistogram` instances that draw from `DefaultHistogramBuckets`. Empty → use defaults. Values are parsed by `telemetry.ParseHistogramBuckets` which sorts them (`telemetry/metrics.go:997-1015`). Invalid float → warning + fallback to defaults (`erpc/init.go:55-57`). Validation pre-checks each part at config-load time (`common/validation.go:151-158`). **Histograms affected (will use custom buckets):** `erpc_upstream_request_duration_seconds`, `erpc_network_request_duration_seconds`, `erpc_consensus_duration_seconds`, `erpc_cache_set_success_duration_seconds`, `erpc_cache_set_error_duration_seconds`, `erpc_cache_get_success_hit_duration_seconds`, `erpc_cache_get_success_miss_duration_seconds`, `erpc_cache_get_error_duration_seconds` — all seven `LabeledHistogram` instances built in `buildFilterAwareHistograms` (`telemetry/metrics.go:755-932`). **Histograms NOT affected (hard-coded bucket sets, cannot be overridden without recompile):** `erpc_network_evm_get_logs_range_requested` and `erpc_network_evm_trace_filter_range_requested` (use `EvmGetLogsRangeHistogramBuckets`), `erpc_network_data_unavailable_wait_seconds` (uses `CatchUpWaitHistogramBuckets`), `erpc_network_hedge_delay_seconds` (inline [0.01..3] buckets), `erpc_network_timeout_duration_seconds` (inline [0.05..30] buckets), `erpc_selection_eval_duration_seconds` (inline [0.0005..1] buckets), `erpc_selection_readmit_age_seconds` (inline [1..3600] buckets), `erpc_upstream_cordon_duration_seconds` (inline [1..86400] buckets), `erpc_rate_limiter_remote_duration_seconds` (inline [0.001..5] buckets), `erpc_upstream_response_size_bytes` (inline [4096..104857600] byte buckets), `erpc_consensus_responses_collected` and `erpc_consensus_agreement_count` (linear 1–10 buckets). All hard-coded bucket definitions are at `telemetry/metrics.go:737-932`. |
| 9 | `metrics.histogramDropLabels` | []string | `nil` (no labels dropped) | List of label names to drop from EVERY `LabeledHistogram`. Counters and gauges are unaffected. Applied via `telemetry.SetHistogramLabelFilter` before `SetHistogramBuckets` (`erpc/init.go:53`). Example: `["user", "agent_name"]` removes these two dimensions from all histograms to reduce cardinality. The underlying `prometheus.HistogramVec` is built with the surviving label set — the drop is permanent for the lifetime of the process. |
| 10 | `metrics.histogramLabelOverrides` | map[string][]string | `nil` | Per-metric overrides that re-add labels even if they appear in `histogramDropLabels`. Key = metric Name **without** the `erpc_` namespace prefix (e.g. `"network_request_duration_seconds"`). Value = list of label names to preserve for that metric. Example: drop `user` globally but keep it on `network_request_duration_seconds` for user-level latency analysis. Wired at `erpc/init.go:53`. |

**Hardcoded server constants (not configurable):**
- Bind address: `":%d"` (all interfaces, port only) (`erpc/init.go:149`)
- Path: **`promhttp.Handler()` is registered as the root (`/`) handler** (`erpc/init.go:150`). Because Go's `http.ServeMux` default pattern matches every path and `promhttp.Handler()` is the only handler, **every URL path on the metrics port returns the full Prometheus metrics output**. `GET /`, `GET /metrics`, `GET /health`, `GET /anything-at-all` — all produce identical output. There is no path routing, no 404, no health-check path. Prometheus conventionally scrapes `/metrics` but that is entirely optional; any path works. Do NOT route traffic to the metrics port expecting a non-metrics response (e.g. a health probe returning 200 with an empty body). (`erpc/init.go:137-158`)
- Protocol: plain HTTP only (no TLS, no auth, no gzip, no rate limiting on the metrics endpoint)
- `ReadHeaderTimeout`: 10 seconds (`erpc/init.go:151`)
- Graceful shutdown budget: 5 seconds (`erpc/init.go:162-168`)

---

### Behaviors & algorithms

#### `errorLabelMode` — compact vs. verbose

`compact` mode (`common/errors.go:46-66`):
- `StandardError` → `string(be.Base().Code)`, e.g. `"ErrEndpointCapacityExceeded"`. For `ErrFailsafeRetryExceeded`, `ErrUpstreamRequest`, `ErrUpstreamRequestSkipped` it appends the cause code: `"ErrUpstreamRequest/ErrEndpointTransportFailure"`. For `ErrJsonRpcExceptionInternal` cause it appends the numeric code: `"ErrUpstreamRequest/ErrJsonRpcExceptionInternal/-32000"`.
- `context.DeadlineExceeded` → `"ContextDeadlineExceeded"`
- `context.Canceled` → `"ContextCanceled"`
- Plain error → `"GenericError"`
- String → `"StringError"`
- Unknown → `"UnknownError"`

`verbose` mode: passes the full message chain through `cleanUpMessage` (strips newlines, long hex strings, etc.), potentially including block numbers, IP addresses, or other unique values. Cardinality risk: each unique block number creates a new time series.

#### LabeledHistogram filter mechanics

1. `SetHistogramLabelFilter(drop, overrides)` builds a `HistogramLabelFilter` struct and stores it in a package-level `sync.RWMutex`-protected var (`telemetry/labeled_histogram.go:28-56`).
2. `SetHistogramBuckets(bucketsStr)` calls `buildFilterAwareHistograms` which calls `NewLabeledHistogram` for each of the 17 histograms. `NewLabeledHistogram` reads the current filter under `filterMu.RLock` and computes `activeIndices` — the subset of schema positions to retain (`telemetry/labeled_histogram.go:58-71`, `103-115`).
3. `activeIndices`: for each label in schema, if it appears in `drop` AND not in the per-metric `keepOverrides`, it is excluded. Otherwise it is retained.
4. The underlying `prometheus.HistogramVec` is created with only the active label names.
5. `registerOrReuse` registers the new `LabeledHistogram` with `prometheus.DefaultRegisterer`. If the same metric name with the identical label set is already registered (repeat call, same filter) it returns the existing one silently. If the label SET differs (filter changed after first registration) it panics — not supported (`telemetry/metrics.go:978-995`).

#### Idle-series eviction

Sweep fires every `sweepEveryRotations` (= 10) rotation ticks of `rotateMetricsLoop`. Rotation tick interval = `windowSize / rollingBuckets` (default 5min / 10 = 30s), so sweep fires roughly every 5 minutes. `idleEvictionAfter` default = 30 minutes (`health/tracker.go:487`).

What gets evicted:
- `upsMetrics` entries (per-upstream, per-method, per-finality) not accessed since cutoff — EXCEPT wildcard (`method == "*"`) rollups and cordoned entries.
- `ntwMetrics` entries (per-network, per-method) not accessed since cutoff — EXCEPT wildcard rollups.
- `urdObsCache` entries (cached `MetricUpstreamRequestDuration` observers): deletes the in-memory cache AND calls `MetricUpstreamRequestDuration.DeleteLabelValues` with the 8-label full schema to release the Prometheus series (`health/tracker.go:648-651`).
- `remoteRateLimitedCounterCache` entries: deletes AND calls `MetricRateLimitsTotal.DeleteLabelValues` (`health/tracker.go:661-664`).

`LabeledHistogram.DeleteLabelValues` uses the same full-schema convention as `WithLabelValues` — it projects down to the active filter subset before calling the underlying `HistogramVec.DeleteLabelValues` (`telemetry/labeled_histogram.go:155-168`).

#### Network alias resolution

At `erpc.Init`, if any project has a network with a non-empty `alias`, a `networkAliasResolver` callback is installed on `common` (`erpc/init.go:62-77`). Components using `util.EvmNetworkId(chainId)` as their raw key (e.g. gRPC cache connector discovery) look up this resolver to emit human-readable `network` label values (e.g. `"mainnet"` instead of `"evm:1"`).

---

### Metrics catalog — by subsystem area

All metric names carry the `erpc_` prefix (from `Namespace: "erpc"`). Full definitions at `telemetry/metrics.go:12-729`. LabeledHistograms at `telemetry/metrics.go:755-932`.

#### Area 1: Upstream request accounting

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_upstream_request_total` | counter | project, vendor, network, upstream, category, attempt, composite, finality, user, agent_name | each actual attempt sent to an upstream; **`composite` = composite request type** — one of the `CompositeType*` constants from `common/request.go:18-27`: `"none"` (plain request), `"logs-split-on-error"`, `"logs-split-proactive"`, `"trace-filter-split-on-error"`, `"trace-filter-split-proactive"`, `"query-blocks-shim"`, `"query-transactions-shim"`, `"query-logs-shim"`, `"query-traces-shim"`, `"query-transfers-shim"`. Empty string is normalized to `"none"` at `health/tracker.go:911-912`. | `upstream/upstream.go:613`, `common/request.go:18-27`, `health/tracker.go:910-912`, defined `telemetry/metrics.go:19` |
| `erpc_upstream_request_errors_total` | counter | project, vendor, network, upstream, category, error, severity, composite, finality, user, agent_name | upstream attempt returned an error; **`severity` ∈ {"critical", "warning", "info"}** via `common.ClassifySeverity` (`common/errors.go:2522-2548`): "info" for block-unavailable/missing-data/skipped-class errors; "warning" for rate-limit and timeout-class errors; "critical" for all other errors (transport failures, unknown errors). Also incremented with severity=info for block-availability-gated skips at `erpc/networks.go:1852`. Same `composite` values as request_total above. | `upstream/upstream.go:726`, `erpc/networks.go:1852`, `common/errors.go:2522-2548`, defined `telemetry/metrics.go:25` |
| `erpc_upstream_request_skipped_total` | counter | project, vendor, network, upstream, category, finality, user, agent_name | upstream pre-forward checks decided to skip | `upstream/upstream.go:679`, defined `telemetry/metrics.go:31` |
| `erpc_upstream_request_missing_data_error_total` | counter | project, vendor, network, upstream, category, finality, user, agent_name | upstream returned missing-data/not-synced error | `upstream/upstream.go:690`, defined `telemetry/metrics.go:37` |
| `erpc_upstream_request_empty_response_total` | counter | project, vendor, network, upstream, category, finality, user, agent_name | upstream returned an emptyish response | `upstream/upstream.go:791`, defined `telemetry/metrics.go:43` |
| `erpc_upstream_selection_total` | counter | project, network, upstream, category, reason, finality | upstream picked for an attempt; **reason ∈ {primary, retry, hedge, consensus_slot, sweep}** — "sweep" means the upstream was picked as part of `runUpstreamSweep` (a try-all-upstreams iteration in the non-consensus execution path, used when primary fails and retry loops through remaining upstreams in sequence). Defined at `common/exec_state.go:44` with comment "try-all-upstreams iteration". | `upstream/upstream.go:561`, `common/exec_state.go:33-44`, `erpc/networks.go:1411-1415`, defined `telemetry/metrics.go:447` |
| `erpc_upstream_attempt_outcome_total` | counter | project, network, upstream, category, outcome, is_hedge, is_retry, finality | terminal attempt outcome; outcome ∈ {success, empty, transport_error, server_error, client_error, rate_limited, missing_data, exec_revert, block_unavailable, breaker_open, cancelled, timeout, skipped}; **`is_hedge` ∈ {"true", "false"}** and **`is_retry` ∈ {"true", "false"}** — both via `boolStr()` which returns literal `"true"` or `"false"` (NOT `"1"`/`"0"` or `"yes"`/`"no"`). `is_hedge="true"` when the attempt was a hedge or part of a hedge fan-out; `is_retry="true"` when `snap.Retries > 0` | `upstream/upstream.go:551-559`, `upstream/upstream.go:80-85`, defined `telemetry/metrics.go:457` |
| `erpc_upstream_request_duration_seconds` | LabeledHistogram | project, vendor, network, upstream, category, composite, finality, user | duration of each upstream attempt; idle series swept every 30 min; **`composite`** same values as upstream_request_total above (`"none"`, `"logs-split-on-error"`, etc.) — high-cardinality tip: `composite` combined with `user` can create many series; both are candidates for `histogramDropLabels` in high-traffic deployments | `health/tracker.go:333-358`, `common/request.go:18-27`, defined `telemetry/metrics.go:785` |
| `erpc_upstream_response_size_bytes` | LabeledHistogram | project, network, category, finality | decoded post-gzip result-body byte count; tight label set by design (see note) | `upstream/upstream.go:654`, defined `telemetry/metrics.go:924` |
| `erpc_upstream_wrong_empty_response_total` | counter | project, vendor, network, upstream, category, finality, user, agent_name | upstream returned empty while consensus determined others had data | `erpc/networks.go:1555`, defined `telemetry/metrics.go:384` |

**Cardinality note on `upstream_request_duration_seconds`:** labels include `user` and `composite` which can be high-cardinality. The idle sweep (`DefaultIdleEvictionAfter = 30 min`) bounds the registered-series count. Consider adding `user` to `histogramDropLabels` for high-traffic deployments.

#### Area 2: Network request accounting

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_network_request_received_total` | counter | project, network, category, finality, user, agent_name | request received for a network | `erpc/projects.go:123`, defined `telemetry/metrics.go:407` |
| `erpc_network_failed_request_total` | counter | project, network, category, attempt, error, severity, finality, user, agent_name | request failed at network/project level; **`severity` ∈ {"critical", "warning", "info"}** — same `common.ClassifySeverity` classification as `upstream_request_errors_total` (`common/errors.go:2522-2548`). Use for severity-tiered alerting: page on `severity="critical"`, ticket on `severity="warning"`, ignore `severity="info"`. | `erpc/projects.go:231`, `common/errors.go:2522-2548`, defined `telemetry/metrics.go:491` |
| `erpc_network_successful_request_total` | counter | project, network, vendor, upstream, category, attempt, finality, emptyish, user, agent_name | request succeeded at network/project level; **`emptyish` ∈ {"true", "false"}** via `strconv.FormatBool(resp.IsResultEmptyish(ctx))` — "true" means the response was successfully routed but returned an empty-ish result (null, empty array, etc.); use this label to distinguish empty-result successes from real-data successes | `erpc/projects.go:192-202`, defined `telemetry/metrics.go:497` |
| `erpc_network_multiplexed_request_total` | counter | project, network, category, finality, user, agent_name | request de-duplicated into an identical in-flight request | `erpc/networks.go:2030`, defined `telemetry/metrics.go:413` |
| `erpc_network_static_response_served_total` | counter | project, network, category | served from a configured static response (no upstream) | `erpc/networks_static_responses.go:56`, defined `telemetry/metrics.go:419` |
| `erpc_network_timeout_fired_total` | counter | project, network, category, finality, scope | timeout policy killed a request; scope=network or scope=upstream; suppressed when retry-exhausted error wins | `erpc/networks.go:1440`, `upstream/upstream.go:845`, defined `telemetry/metrics.go:437` |
| `erpc_network_retry_attempt_total` | counter | project, network, category, reason, finality | network-scope retry; reason ∈ {empty_result, pending_tx, retryable_error, block_unavailable, missing_data} | `erpc/network_executor.go:292`, defined `telemetry/metrics.go:466` |
| `erpc_network_request_duration_seconds` | LabeledHistogram | project, network, vendor, upstream, category, finality, user | end-to-end network request duration on success and failure (vendor/upstream="\<error\>" on fail) | `erpc/projects.go:211`, `erpc/projects.go:242`, defined `telemetry/metrics.go:792` |
| `erpc_network_data_unavailable_wait_seconds` | LabeledHistogram | project, network, category, reason, finality | wall-clock catch-up delay before data-not-yet-available retry; dedicated buckets tuned for per-chain block times | `erpc/network_executor.go:324`, defined `telemetry/metrics.go:835` |
| `erpc_network_timeout_duration_seconds` | LabeledHistogram | project, network, category, finality | computed dynamic timeout per request | `common/timeout_func.go:74`, defined `telemetry/metrics.go:820` |

#### Area 3: Block number & tip tracking

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_upstream_latest_block_number` | gauge | project, vendor, network, upstream | upstream's latest block advanced | `health/tracker.go:413`, defined `telemetry/metrics.go:61` |
| `erpc_upstream_finalized_block_number` | gauge | project, vendor, network, upstream | upstream's finalized block advanced | `health/tracker.go:423`, defined `telemetry/metrics.go:67` |
| `erpc_upstream_block_head_lag` | gauge | project, vendor, network, upstream | blocks behind freshest upstream | `health/tracker.go:433`, defined `telemetry/metrics.go:49` |
| `erpc_upstream_finalization_lag` | gauge | project, vendor, network, upstream | finalized blocks behind freshest upstream | `health/tracker.go:443`, defined `telemetry/metrics.go:55` |
| `erpc_upstream_block_head_large_rollback` | gauge | project, vendor, network, upstream | block head rolled back by a large delta; **"large" is defined as > `DefaultToleratedBlockHeadRollback` = 1024 blocks** (constant at `architecture/evm/evm_state_poller.go:27`). Rollbacks ≤ 1024 blocks are silently ignored by the shared state variable. A non-zero value on this gauge indicates a rollback exceeding 1024 blocks — likely a real reorg, upstream reset, or misconfiguration. The gauge value = the absolute delta of the rollback. | `health/tracker.go:474`, `architecture/evm/evm_state_poller.go:27`, `data/shared_state_variable.go:208,303`, defined `telemetry/metrics.go:378` |
| `erpc_upstream_latest_block_polled_total` | counter | project, vendor, network, upstream | state poller polled latest block | `architecture/evm/evm_state_poller.go:406`, defined `telemetry/metrics.go:366` |
| `erpc_upstream_finalized_block_polled_total` | counter | project, vendor, network, upstream | state poller polled finalized block | `architecture/evm/evm_state_poller.go:508`, defined `telemetry/metrics.go:372` |
| `erpc_upstream_stale_latest_block_total` | counter | project, vendor, network, upstream, category | upstream returned stale latest block vs. others | `architecture/evm/eth_blockNumber.go:76`, `architecture/evm/eth_getBlockByNumber.go:173`, defined `telemetry/metrics.go:306` |
| `erpc_upstream_stale_finalized_block_total` | counter | project, vendor, network, upstream | upstream returned stale finalized block vs. others | `architecture/evm/eth_getBlockByNumber.go:232`, defined `telemetry/metrics.go:312` |
| `erpc_upstream_stale_upper_bound_total` | counter | project, vendor, network, upstream, category, confidence | request skipped: upstream latest < requested upper bound; **confidence ∈ {"blockHead", "finalizedBlock"}** — "blockHead" = `AvailbilityConfidenceBlockHead` (enum 1), "finalizedBlock" = `AvailbilityConfidenceFinalized` (enum 2), as returned by `AvailbilityConfidence.String()` | `upstream/upstream.go:1002,1029,1073`, `common/architecture_evm.go:40-46`, defined `telemetry/metrics.go:318` |
| `erpc_upstream_stale_lower_bound_total` | counter | project, vendor, network, upstream, category, confidence | request skipped: requested lower bound below upstream's available range; **confidence ∈ {"blockHead", "finalizedBlock"}** (same enum as stale_upper_bound_total above) | `upstream/upstream.go:985,1268,1295`, `common/architecture_evm.go:40-46`, defined `telemetry/metrics.go:324` |
| `erpc_network_latest_block_timestamp_distance_seconds` | gauge | project, network, origin | now − latest block timestamp; origin=evm_state_poller or network_response | `health/tracker.go:1315`, `architecture/evm/eth_getBlockByNumber.go:96`, defined `telemetry/metrics.go:109` |
| `erpc_network_dynamic_block_time_milliseconds` | gauge | project, network | EMA block-time estimate; **EMA algorithm**: α = 0.1 (constant `blockTimeEmaAlpha` at `health/tracker.go:1385`), effective window ~19 samples; minimum 3 samples (`blockTimeMinSamples=3`) before first emission (requires 4 total block observations); per-block sample = `(timestampDelta_sec × 1e9) / blockGap` (normalized to sub-second precision for fast chains like Arbitrum); sanity bounds: rejects values outside [10ms, 120s] and resets internal EMA to last published value. Returns **0** when fewer than 3 samples collected (startup or halted chain). Emitted as milliseconds (float64). | `health/tracker.go:1381-1458`, defined `telemetry/metrics.go:115` |
| `erpc_network_served_tip_block_number` | gauge | project, network, lane, axis | served-tip pick published per axis (latest/finalized); lane="all" = network-wide | `erpc/networks.go:821`, defined `telemetry/metrics.go:130` |
| `erpc_network_served_tip_lag_blocks` | gauge | project, network, lane, axis | lag of served tip behind freshest velocity-eligible upstream; ABSENT in default MAX mode | `erpc/networks.go:834`, defined `telemetry/metrics.go:145` |
| `erpc_network_served_tip_upstream_excluded_total` | counter | project, network, upstream, axis, reason | upstream excluded from served-tip pick; reason=velocity\|outlier; absent in MAX mode | `erpc/networks.go:845,849`, defined `telemetry/metrics.go:158` |

#### Area 4: Cache connector freshness

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_cache_connector_earliest_block_number` | gauge | connector, network | gRPC cache connector availability poll | `data/grpc.go:342`, defined `telemetry/metrics.go:73` |
| `erpc_cache_connector_latest_block_number` | gauge | connector, network | same poll, latest block | `data/grpc.go:358`, defined `telemetry/metrics.go:79` |
| `erpc_cache_connector_finalized_block_number` | gauge | connector, network | same poll, finalized block | `data/grpc.go:368`, defined `telemetry/metrics.go:85` |
| `erpc_cache_connector_earliest_block_timestamp_seconds` | gauge | connector, network | same poll, earliest block unix timestamp | `data/grpc.go:344`, defined `telemetry/metrics.go:91` |
| `erpc_cache_connector_latest_block_timestamp_seconds` | gauge | connector, network | same poll, latest block unix timestamp | `data/grpc.go:360`, defined `telemetry/metrics.go:97` |
| `erpc_cache_connector_finalized_block_timestamp_seconds` | gauge | connector, network | same poll, finalized block unix timestamp | `data/grpc.go:370`, defined `telemetry/metrics.go:103` |

#### Area 5: Cache operations (JSON-RPC cache)

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_cache_get_success_hit_total` | counter | project, network, category, connector, policy, ttl | cache get hit | `architecture/evm/json_rpc_cache.go:536`, defined `telemetry/metrics.go:568` |
| `erpc_cache_get_success_miss_total` | counter | project, network, category, connector, policy, ttl | cache get miss | `architecture/evm/json_rpc_cache.go:483,507`, defined `telemetry/metrics.go:574` |
| `erpc_cache_get_error_total` | counter | project, network, category, connector, policy, ttl, error | cache get errored | `architecture/evm/json_rpc_cache.go:294`, defined `telemetry/metrics.go:580` |
| `erpc_cache_get_skipped_total` | counter | project, network, category | cache get skipped (no matching policy) | `architecture/evm/json_rpc_cache.go:189`, defined `telemetry/metrics.go:586` |
| `erpc_cache_get_age_guard_reject_total` | counter | project, network, method, connector, policy, ttl | cached item rejected: block-timestamp age exceeded policy TTL | `architecture/evm/json_rpc_cache.go:910`, defined `telemetry/metrics.go:592` |
| `erpc_cache_set_success_total` | counter | project, network, category, connector, policy, ttl | cache set succeeded | `architecture/evm/json_rpc_cache.go:794`, defined `telemetry/metrics.go:550` |
| `erpc_cache_set_error_total` | counter | project, network, category, connector, policy, ttl, error | cache set errored | `architecture/evm/json_rpc_cache.go:700,775`, defined `telemetry/metrics.go:556` |
| `erpc_cache_set_skipped_total` | counter | project, network, category, connector, policy, ttl | cache set skipped by policy | `architecture/evm/json_rpc_cache.go:722`, defined `telemetry/metrics.go:562` |
| `erpc_cache_set_original_bytes_total` | counter | project, network, category, connector, policy, ttl | uncompressed bytes written on cache set | `architecture/evm/json_rpc_cache.go:736`, defined `telemetry/metrics.go:598` |
| `erpc_cache_set_compressed_bytes_total` | counter | project, network, category, connector, policy, ttl | compressed bytes written on cache set | `architecture/evm/json_rpc_cache.go:756`, defined `telemetry/metrics.go:604` |
| `erpc_cache_get_success_hit_duration_seconds` | LabeledHistogram | project, network, category, connector, policy, ttl | cache get hit duration | `architecture/evm/json_rpc_cache.go:544`, defined `telemetry/metrics.go:879` |
| `erpc_cache_get_success_miss_duration_seconds` | LabeledHistogram | project, network, category, connector, policy, ttl | cache get miss duration | `architecture/evm/json_rpc_cache.go:491,515`, defined `telemetry/metrics.go:886` |
| `erpc_cache_get_error_duration_seconds` | LabeledHistogram | project, network, category, connector, policy, ttl, error | cache get error duration | `architecture/evm/json_rpc_cache.go:303`, defined `telemetry/metrics.go:893` |
| `erpc_cache_set_success_duration_seconds` | LabeledHistogram | project, network, category, connector, policy, ttl | cache set success duration | `architecture/evm/json_rpc_cache.go:802`, defined `telemetry/metrics.go:865` |
| `erpc_cache_set_error_duration_seconds` | LabeledHistogram | project, network, category, connector, policy, ttl, error | cache set error duration | `architecture/evm/json_rpc_cache.go:709,784`, defined `telemetry/metrics.go:872` |
| `erpc_ristretto_cache_current_cost` | gauge | connector | Ristretto (memory connector) current total cost; **DISABLED BY DEFAULT** — only emitted when `memory.emitMetrics: true` (the `EmitMetrics *bool` field on the memory connector config, nil = disabled). If this gauge is absent from your metrics endpoint, check that `emitMetrics: true` is set in the memory connector config. (`data/memory.go:71`) | `data/memory.go:231-273`, defined `telemetry/metrics.go:628` |
| `erpc_ristretto_cache_sets_failed_total` | counter | connector | Ristretto set dropped/rejected | `data/memory.go:281`, defined `telemetry/metrics.go:634` |

#### Area 6: Rate limiting

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_rate_limits_total` | counter | project, network, vendor, upstream, category, finality, user, agent_name, budget, scope, auth, origin | unified rate-limit event; **`scope` ∈ {"user", "network", "ip", "" (or comma-joined combination)}** — the rule's `ScopeString()` output (which scopes the rule applies to: per-user, per-network, per-IP); **`origin` ∈ {"upstream", ""}** — "upstream" for remote upstream 429 passthroughs (`health/tracker.go:393`), empty string for local budget denies; **`budget`** = budget ID, or `"<remote>"` for remote 429 events (`health/tracker.go:390`); for remote 429 scope is `"remote"` (`health/tracker.go:391`). Event semantics: `scope="remote",origin="upstream",budget="<remote>"` = upstream returned 429; all other combos = local budget triggered. Idle series deleted by health-tracker sweep. | `upstream/ratelimiter_budget.go:199-201,235-239`, `health/tracker.go:381-393,661-664`, `common/config.go:1821-1835`, defined `telemetry/metrics.go:503` |
| `erpc_rate_limiter_budget_max_count` | gauge | budget, method, scope | budget's allowed req/s set on rule creation/update or auto-tuner; **`scope`** = `ScopeString()` of the rate limit rule: comma-joined enabled scope flags ("user", "network", "ip", or combinations). Distinguishes per-upstream from per-network budget scopes. Empty string = no per-scope flags set. | `upstream/ratelimiter_budget.go:94,138`, `upstream/ratelimiter_registry.go:201`, `common/config.go:1821-1835`, defined `telemetry/metrics.go:509` |
| `erpc_rate_limiter_budget_decision_total` | counter | project, network, category, finality, user, agent_name, budget, method, scope, decision | **DEPRECATED / DORMANT** — registered but zero production call sites; replaced by `erpc_rate_limits_total` | defined `telemetry/metrics.go:515` |
| `erpc_rate_limiter_failopen_total` | counter | project, network, user, agent_name, budget, category, reason | rate limiter failed open (allowed request due to error/timeout); **reason ∈ {"admission_full", "limit_timeout"}** — "admission_full" = admission semaphore was full (load shed, `ratelimiter_budget.go:369`); "limit_timeout" = the remote DoLimit call exceeded its deadline (`ratelimiter_budget.go:456`) | `upstream/ratelimiter_budget.go:367-371,452-459`, defined `telemetry/metrics.go:521` |
| `erpc_rate_limiter_remote_inflight` | gauge | budget | concurrent in-flight remote (e.g. Redis) DoLimit calls per budget | `upstream/ratelimiter_registry.go:176`, defined `telemetry/metrics.go:533` |
| `erpc_rate_limiter_remote_admission_shedded_total` | counter | budget | remote check fail-opened because admission semaphore full (load shed) | `upstream/ratelimiter_registry.go:177`, defined `telemetry/metrics.go:544` |
| `erpc_rate_limiter_remote_duration_seconds` | LabeledHistogram | budget, result | remote rate-limit check (e.g. Redis DoLimit) duration; fine sub-second buckets; **result ∈ {"ok", "over_limit", "fail_open"}** — "ok" = allowed, "over_limit" = denied by remote, "fail_open" = failed and allowed through. Handles pre-cached at `ratelimiter_registry.go:179-181` using these exact strings. | `upstream/ratelimiter_registry.go:178-181`, defined `telemetry/metrics.go:903` |

**Cardinality note:** `erpc_rate_limits_total` has 12 labels including `user` and `agent_name`; idle sweep (`health/tracker.go:661-664`) bounds Prometheus-registered series but the sweep is keyed on `(project, network, vendor, upstream, category, finality, user, agentName)` — the full remote-rate-limited key. In high-cardinality-user environments, add `user` to `histogramDropLabels` (does not affect this counter, but mitigates related histogram series) and consider a scrape-side label aggregation.

#### Area 7: Upstream health (cordon, circuit-breaker, probing)

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_upstream_cordoned` | gauge | project, vendor, network, upstream, category, reason | health tracker sets 1 on cordon, 0 on uncordon; **`category` holds the cordon scope (= method string or `"*"`)**. `"*"` means wholesale cordon (all methods on that upstream); a specific method string (e.g. `"eth_call"`) means per-method cordon. This is NOT the standard RPC request-category label — do not confuse with the `category` on request-accounting metrics. Vendor is `"n/a"` when vendor is not configured (fallback in `upstream.VendorName()` at `upstream/upstream.go:275-286`). | `health/tracker.go:458-465`, defined `telemetry/metrics.go:164` |
| `erpc_upstream_cordon_event_total` | counter | project, network, upstream, action | admin cordon/uncordon; action ∈ {cordon, uncordon} | `health/tracker.go:812,843`, defined `telemetry/metrics.go:293` |
| `erpc_upstream_cordon_duration_seconds` | histogram (plain promauto) | project, network, upstream | seconds spent cordoned, observed on uncordon | `health/tracker.go:838`, defined `telemetry/metrics.go:299` |
| `erpc_upstream_breaker_state_change_total` | counter | project, upstream, transition | circuit-breaker state transition; transition ∈ {closed_to_open, half_open_to_open, half_open_to_closed, open_to_half_open} | `upstream/upstream.go:93`, defined `telemetry/metrics.go:485` |
| `erpc_selection_probe_requests_total` | counter | network, upstream, method | probe-mirror request fired at excluded upstream | `internal/policy/prober.go:398`, defined `telemetry/metrics.go:269` |
| `erpc_selection_probe_errors_total` | counter | network, upstream, method, reason | probe request errored; reason ∈ {timeout, throttled, auth, skipped, error}; **"skipped" vs "error" distinction**: "skipped" fires when the upstream returned `ErrCodeUpstreamRequestSkipped` (the upstream intentionally rejected the probe, e.g. method not supported, capacity issue that is not a rate limit); "error" fires for all other non-nil, non-classified errors (generic transport or logic failures). The `"skipped"` path is NOT the same as `erpc_selection_probe_skipped_total` (which fires before the request is sent); this fires after a response is received. (`internal/policy/prober.go:449-465`) | `internal/policy/prober.go:416,449-465`, defined `telemetry/metrics.go:275` |
| `erpc_selection_probe_skipped_total` | counter | network, reason | probe candidate skipped pre-fire; reason ∈ {write_method, opt_out, sampled_out, max_concurrent, no_method} | `internal/policy/prober.go:200,208,254,274,289`, defined `telemetry/metrics.go:281` |
| `erpc_selection_probe_dropped_total` | counter | network, reason | probe-bus publish dropped, channel full | `internal/policy/prober.go:161`, defined `telemetry/metrics.go:287` |

#### Area 8: Selection policy

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_selection_position` | gauge | project, network, method, upstream | tick output: 0=primary, 1+=runner-up, −1=excluded | `internal/policy/slot.go:477,495`, defined `telemetry/metrics.go:174` |
| `erpc_selection_score` | gauge | project, network, method, upstream | per-upstream sortByScore score (lower=better); absent for upstreams that bypassed scoring | `internal/policy/slot.go:483`, defined `telemetry/metrics.go:221` |
| `erpc_selection_eligible_upstreams` | gauge | project, network, method | count of upstreams returned by most recent tick | `internal/policy/slot.go:471`, defined `telemetry/metrics.go:205` |
| `erpc_selection_rejection_total` | counter | project, network, method, upstream, step | tick rejected upstream at a std-lib step | `internal/policy/slot.go:500`, defined `telemetry/metrics.go:180` |
| `erpc_selection_exclusion_total` | counter | project, network, method, upstream, reason | exclusion event; reason = leaf-predicate slug | `internal/policy/slot.go:508`, defined `telemetry/metrics.go:227` |
| `erpc_selection_shadow_exclusion_total` | counter | project, network, method, upstream, reason | `shadowExcludeIf` would-have-excluded; upstream stays in rotation | `internal/policy/slot.go:535`, defined `telemetry/metrics.go:233` |
| `erpc_selection_excluded_seconds` | gauge | project, network, method, upstream | wall-clock seconds continuously excluded; 0 when in rotation | `internal/policy/slot.go:488,524`, defined `telemetry/metrics.go:239` |
| `erpc_selection_readmit_total` | counter | project, network, method, upstream | excluded → in-list transition | `internal/policy/slot.go:556`, defined `telemetry/metrics.go:245` |
| `erpc_selection_primary_switch_total` | counter | project, network, method, from, to | primary upstream changed between ticks | `internal/policy/slot.go:583`, defined `telemetry/metrics.go:186` |
| `erpc_selection_sticky_hold_total` | counter | project, network, method, upstream | stickyPrimary held a primary that would otherwise flip | `internal/policy/slot.go:570`, defined `telemetry/metrics.go:258` |
| `erpc_selection_eval_duration_seconds` | histogram (plain promauto) | project, network, method | per-tick selection-policy eval latency | `internal/policy/slot.go:457`, defined `telemetry/metrics.go:192` |
| `erpc_selection_eval_errors_total` | counter | project, network, method, kind | eval failure; kind ∈ {timeout, throw, invalid_return, fallback_default} | `internal/policy/slot.go:467`, defined `telemetry/metrics.go:199` |
| `erpc_selection_readmit_age_seconds` | histogram (plain promauto) | project, network, method | age at readmit (now − excludedSince) | `internal/policy/slot.go:560`, defined `telemetry/metrics.go:251` |

**Cardinality note:** `erpc_selection_*` metrics carry a `method` label. If the ingress exposes hundreds of distinct RPC methods to untrusted callers, this can produce hundreds of time series per upstream. Use method-level allow/ignore filters or accept the cardinality.

#### Area 9: Consensus

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_consensus_total` | counter | project, network, category, outcome, finality | consensus round completed by outcome; **outcome ∈ {"success", "consensus_on_error", "dispute", "low_participants", "generic_error", "caller_abandoned"}** — "success" = unanimous non-empty; "consensus_on_error" = consensus reached on a JSON-RPC error response; "dispute" = no consensus reached; "low_participants" = too few upstreams responded; "generic_error" = internal executor error; "caller_abandoned" = request context cancelled before outcome | `consensus/executor.go:329,1295-1326,1301,1327,1345`, defined `telemetry/metrics.go:665` |
| `erpc_consensus_misbehavior_detected_total` | counter | project, network, upstream, category, finality, response_type, larger_than_consensus | upstream returned different data than consensus; **response_type ∈ {"NonEmpty", "Empty", "ConsensusError", "InfrastructureError"}** — classification of the misbehaving upstream's own response type (from `ResponseType.String()` at `consensus/executor.go:66-75`) | `consensus/executor.go:968-975`, defined `telemetry/metrics.go:671` |
| `erpc_consensus_upstream_punished_total` | counter | project, network, upstream | upstream punished after misbehavior threshold | `consensus/executor.go:1216`, defined `telemetry/metrics.go:677` |
| `erpc_consensus_short_circuit_total` | counter | project, network, category, reason, finality | round short-circuited; **reason ∈ {"sendrawtx_first_success", "consensus_error_threshold", "unassailable_lead"}** — "sendrawtx_first_success" = first successful eth_sendRawTransaction response wins immediately; "consensus_error_threshold" = enough upstreams returned errors that no non-error consensus is possible; "unassailable_lead" = one response group has enough votes that no other group can catch up | `consensus/executor.go:578`, `consensus/rules.go:845,864,897`, defined `telemetry/metrics.go:683` |
| `erpc_consensus_wait_capped_total` | counter | project, network, category, trigger, finality | round resolved early because maxWaitOnResult/maxWaitOnEmpty fired; **trigger ∈ {"result", "empty"}** — "result" = `maxWaitOnResult` timer fired (at least one non-empty response was in the bag when the deadline hit); "empty" = `maxWaitOnEmpty` timer fired (only empty/no responses received so far) | `consensus/executor.go:582-593`, defined `telemetry/metrics.go:694` |
| `erpc_consensus_errors_total` | counter | project, network, category, error, finality | consensus-level error by type | `consensus/executor.go:1307,1363`, defined `telemetry/metrics.go:700` |
| `erpc_consensus_upstream_errors_total` | counter | project, network, upstream, category, finality, response_type, error_code | participant upstream errored during consensus; **response_type ∈ {"NonEmpty", "Empty", "ConsensusError", "InfrastructureError"}** — same classification as misbehavior_detected; "ConsensusError" = JSON-RPC error response; "InfrastructureError" = transport/timeout/connectivity error | `consensus/executor.go:940-952`, `consensus/executor.go:57-75`, defined `telemetry/metrics.go:706` |
| `erpc_consensus_panics_total` | counter | project, network, category, finality | panic recovered inside consensus | `consensus/executor.go:389,650`, defined `telemetry/metrics.go:712` |
| `erpc_consensus_cancellations_total` | counter | project, network, category, phase, finality | context cancelled during consensus, by phase; **phase ∈ {"before_execution", "after_execution", "caller_abandoned"}** — "before_execution" = cancelled before any upstream request was dispatched; "after_execution" = cancelled after at least one upstream response was received; "caller_abandoned" = caller's context cancelled before consensus could publish an outcome (the executor still completes in background) | `consensus/executor.go:326,657-658,671-672`, defined `telemetry/metrics.go:718` |
| `erpc_consensus_duration_seconds` | LabeledHistogram | project, network, category, outcome, finality | consensus round duration | `consensus/executor.go:332,1304,1346`, defined `telemetry/metrics.go:858` |
| `erpc_consensus_responses_collected` | LabeledHistogram | project, network, category, vendors, short_circuited, finality | responses collected before consensus decision; **`vendors` = comma-joined sorted vendor names** (e.g. `"alchemy,infura,quicknode"`) from `sort.Strings(vendorNames)` + `strings.Join`; **`short_circuited` ∈ {"true", "false"}** via `strconv.FormatBool` | `consensus/executor.go:555-572`, defined `telemetry/metrics.go:844` |
| `erpc_consensus_agreement_count` | LabeledHistogram | project, network, category, finality | upstreams agreeing on most common result | `consensus/executor.go:1349`, defined `telemetry/metrics.go:851` |

#### Area 10: Hedging

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_network_hedged_request_total` | counter | project, network, upstream, category, attempt, finality, user, agent_name | hedged request fired | `erpc/networks.go:1288`, defined `telemetry/metrics.go:425` |
| `erpc_network_hedge_discards_total` | counter | project, network, upstream, category, attempt, hedge, finality, user, agent_name | hedged request discarded (wasted work) | `erpc/networks.go:1897`, defined `telemetry/metrics.go:431` |
| `erpc_network_hedge_winner_total` | counter | project, network, upstream, category, finality | hedge race won by upstream whose response was kept | `erpc/network_executor.go:564`, defined `telemetry/metrics.go:476` |
| `erpc_network_hedge_delay_seconds` | LabeledHistogram | project, network, category, finality | **DORMANT** — registered but no production observe site | defined `telemetry/metrics.go:813` |

#### Area 11: EVM method-specific metrics

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_network_evm_get_logs_split_success_total` | counter | project, network, user, agent_name | a split eth_getLogs sub-request succeeded | `architecture/evm/eth_getLogs.go:764`, defined `telemetry/metrics.go:330` |
| `erpc_network_evm_get_logs_split_failure_total` | counter | project, network, user, agent_name | a split eth_getLogs sub-request failed | `architecture/evm/eth_getLogs.go:684,709,723,737,751`, defined `telemetry/metrics.go:336` |
| `erpc_network_evm_get_logs_forced_splits_total` | counter | project, network, dimension, user, agent_name | eth_getLogs forcibly split; dimension ∈ {block_range, addresses, **topics0**} — note: the topics split is over `topics[0]` OR-array only, hence `"topics0"` not `"topics"` | `architecture/evm/eth_getLogs.go:565,585,604-610`, defined `telemetry/metrics.go:342` |
| `erpc_network_evm_trace_filter_split_success_total` | counter | project, network, method, user, agent_name | split trace_filter/arbtrace_filter sub-request succeeded | `architecture/evm/trace_filter.go:573`, defined `telemetry/metrics.go:348` |
| `erpc_network_evm_trace_filter_split_failure_total` | counter | project, network, method, user, agent_name | split trace_filter/arbtrace_filter sub-request failed | `architecture/evm/trace_filter.go:502`, defined `telemetry/metrics.go:354` |
| `erpc_network_evm_trace_filter_forced_splits_total` | counter | project, network, method, dimension, user, agent_name | trace_filter split; dimension ∈ {block_range, from_address, to_address} | `architecture/evm/trace_filter.go:424,444,463`, defined `telemetry/metrics.go:360` |
| `erpc_network_evm_block_range_requested_total` | counter | project, network, vendor, upstream, category, user, finality, bucket, size | block-range heatmap; **`bucket` label uses a variable-resolution algorithm when tip is known** (via `ComputeBlockHeatmapBucket` at `erpc/block_heatmap.go:87`): special string tags for `"TIP"` (≤4 blocks from tip), `"LATEST"`, `"FINALIZED"`, `"SAFE"`, `"PENDING"`, `"FUTURE"` (ahead of tip); near-tip (within last 5M blocks) → relative labels like `"L100k"` (last 100k), `"100k-200k"`, etc.; older blocks → absolute labels like `"119m-120m"`. When tip is unknown, falls back to static `EvmBlockRangeBucketSize` = 100000 aligned buckets. `size` = the bucket block-count width as string. For `eth_blockNumber` the bucket is always `"TIP"`. | `erpc/block_heatmap.go:14-84`, defined `telemetry/metrics.go:724` |
| `erpc_network_evm_get_logs_range_requested` | LabeledHistogram | project, network, category, user, finality | eth_getLogs requested block-range size; **observed value = `toBlock - fromBlock` (number of blocks in the range, not byte size)**; buckets: 1, 10, 100, 500, 1000, 5000, 10000, 30000 blocks | `architecture/evm/eth_getLogs.go:118`, defined `telemetry/metrics.go:799` |
| `erpc_network_evm_trace_filter_range_requested` | LabeledHistogram | project, network, method, user, finality | trace_filter/arbtrace_filter requested block-range size; **observed value = `toBlock - fromBlock` (number of blocks, not bytes)**; same bucket boundaries as get_logs_range | `architecture/evm/trace_filter.go:99`, defined `telemetry/metrics.go:806` |

**Cardinality note on `erpc_network_evm_block_range_requested_total`:** carries a `bucket` label derived from block number divided by `EvmBlockRangeBucketSize` (100000). For a chain that has processed 20 million blocks, this creates up to 200 unique bucket values. Raising `EvmBlockRangeBucketSize` to 500000 or 1000000 (via code, not config) reduces this cardinality.

#### Area 12: gRPC BDS resilience

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_grpc_bds_hard_timeout_total` | counter | project, upstream, method | BDS gRPC call hit bounded-wait hard timeout (wedged H2 stream indicator); fires when a gRPC BDS call exceeds `bdsHardCallTimeout` = **20 seconds** (hard-coded constant at `clients/grpc_bds_resilience.go:34`). A non-zero rate on this counter indicates H2 stream wedging — the watchdog then force-replaces the connection (`erpc_grpc_bds_conn_replacements_total`). Not configurable without recompile. | `clients/grpc_bds_resilience.go:170`, `clients/grpc_bds_resilience.go:30-34`, defined `telemetry/metrics.go:393` |
| `erpc_grpc_bds_conn_replacements_total` | counter | project, upstream | BDS pool connection force-closed by stuck-call watchdog | `clients/grpc_bds_resilience.go:236`, defined `telemetry/metrics.go:401` |

#### Area 13: CORS

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_cors_requests_total` | counter | project, origin | request carrying an Origin header; **KNOWN MISLABELING**: the `project` label receives `r.URL.Path` (e.g. `/myproject/evm/1`), NOT the project ID string. All dashboard queries on CORS metrics must use path-style values (e.g. `project="/myproject/evm/1"`) not bare IDs. | `erpc/http_server.go:1020`, defined `telemetry/metrics.go:610` |
| `erpc_cors_preflight_requests_total` | counter | project, origin | allowed-origin OPTIONS preflight; **same `project` mislabeling as cors_requests_total** (`project` = URL path) | `erpc/http_server.go:1069`, defined `telemetry/metrics.go:616` |
| `erpc_cors_disallowed_origin_total` | counter | project, origin | request from disallowed origin; **same `project` mislabeling** (`project` = URL path) | `erpc/http_server.go:1038`, defined `telemetry/metrics.go:622` |

#### Area 14: Shadow testing

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_shadow_response_identical_total` | counter | project, vendor, network, upstream, category | shadow upstream response identical to expected | `erpc/shadow.go:218`, defined `telemetry/metrics.go:640` |
| `erpc_shadow_response_mismatch_total` | counter | project, vendor, network, upstream, category, finality, emptyish, larger | shadow upstream response differs from expected; **`emptyish` ∈ {"true", "false"}** (whether the shadow's response was empty-ish); **`larger` ∈ {"true", "false"}** (whether the shadow response body was larger than the primary's); both via `strconv.FormatBool` | `erpc/shadow.go:238-246`, defined `telemetry/metrics.go:646` |
| `erpc_shadow_response_error_total` | counter | project, vendor, network, upstream, category, error | shadow upstream request errored | `erpc/shadow.go:121,141,195`, defined `telemetry/metrics.go:652` |

#### Area 15: Auth

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_auth_failed_total` | counter | project, network, strategy, reason, agent_name | failed authentication attempt; **emitted ONLY by the database strategy** — `strategy` label is always `"database"`. Other auth strategies (secret, jwt, etc.) do NOT emit this counter. If you have multiple auth strategies, this counter only reflects database auth failures. (`auth/strategy_database.go:467-472`) | `auth/strategy_database.go:467-472`, defined `telemetry/metrics.go:659` |

#### Area 16: Panics

| metric | type | labels | fires when | line |
|--------|------|--------|-----------|------|
| `erpc_unexpected_panic_total` | counter | scope, extra, error | recovered panic; **`scope` ∈ {"request-handler", "final-error-writer", "top-level-handler", "timeout-handler", "validate-pattern", "redis-pubsub", "shared-state-registry", "matcher"}** — each value identifies the goroutine/handler location where the panic was recovered: "request-handler" = per-request HTTP handler; "final-error-writer" = error-response writer; "top-level-handler" = outermost HTTP handler; "timeout-handler" = request-timeout enforcement; "validate-pattern" = config validation pattern compilation; "redis-pubsub" = Redis pub/sub goroutine; "shared-state-registry" = shared state registry goroutine; "matcher" = method/rule matcher. Use `scope` to route panic alerts to the right team. | `erpc/http_server.go:446,759,789`, `erpc/http_timeout.go:53`, `common/matcher.go:123`, `data/redis_pubsub_manager.go:232,335`, `data/shared_state_registry.go:171`, defined `telemetry/metrics.go:13` |

---

### Histogram bucket sets

| bucket set constant | values | applies to | source |
|---|---|---|---|
| `DefaultHistogramBuckets` | 0.05, 0.5, 5, 30 (overridable via `metrics.histogramBuckets`) | upstream_request_duration_seconds, network_request_duration_seconds, consensus_duration_seconds, cache_{set,get}_{success,error}_duration_seconds (7 histograms total) | `telemetry/metrics.go:731-736`; parse `telemetry/metrics.go:997-1015` |
| `EvmGetLogsRangeHistogramBuckets` | 1, 10, 100, 500, 1000, 5000, 10000, 30000 | network_evm_get_logs_range_requested, network_evm_trace_filter_range_requested | `telemetry/metrics.go:743` |
| `CatchUpWaitHistogramBuckets` | 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64 | network_data_unavailable_wait_seconds | `telemetry/metrics.go:752` |
| hedge delay (hard-coded inline) | 0.01, 0.03, 0.05, 0.2, 0.3, 0.5, 0.7, 1, 3 | network_hedge_delay_seconds (dormant) | `telemetry/metrics.go:817` |
| timeout duration (hard-coded inline) | 0.05, 0.1, 0.3, 0.5, 1, 3, 5, 10, 30 | network_timeout_duration_seconds | `telemetry/metrics.go:824` |
| selection eval (hard-coded inline) | 0.0005, 0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1 | selection_eval_duration_seconds | `telemetry/metrics.go:196` |
| readmit age (hard-coded inline) | 1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600 | selection_readmit_age_seconds | `telemetry/metrics.go:255` |
| cordon duration (hard-coded inline) | 1, 10, 60, 300, 900, 1800, 3600, 7200, 21600, 86400 | upstream_cordon_duration_seconds | `telemetry/metrics.go:303` |
| rate-limiter remote (hard-coded inline) | 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5 | rate_limiter_remote_duration_seconds | `telemetry/metrics.go:907` |
| response size (hard-coded inline) | 4096, 65536, 1048576, 16777216, 104857600 | upstream_response_size_bytes | `telemetry/metrics.go:928` |
| consensus counts (LinearBuckets) | 1, 2, 3, 4, 5, 6, 7, 8, 9, 10 | consensus_responses_collected, consensus_agreement_count | `telemetry/metrics.go:848,855` |

Only `DefaultHistogramBuckets` are configurable. All other bucket sets are hard-coded and cannot be changed without recompiling.

---

### Observability

**The metrics endpoint:** `http://<host>:<metrics.port>/` (any path) — `promhttp.Handler()` is the root handler so every HTTP path on port 4001 returns the full Prometheus text exposition. The canonical Prometheus convention is to scrape `/metrics` but `GET /`, `GET /health`, or any other path returns identical output. Plain HTTP only, no auth, no path routing. Scrape with Prometheus `scrape_interval: 10s` (reference `monitoring/prometheus/prometheus.yml:2-4`). Default target in the bundled Prometheus config: `host.docker.internal:4001`. (`erpc/init.go:149-150`)

**Stock collectors** (from prometheus/client_golang default registry, present unless `ERPC_NOMETRICS=1`):
- `go_*` — Go runtime memory/GC/goroutine stats
- `process_*` — OS process CPU/memory/fd stats
- `promhttp_metric_handler_*` — metrics handler self-scrape stats

**Bundled alerting rules** (`monitoring/prometheus/alert.rules`):

| alert | expression | threshold |
|---|---|---|
| HighErrorRate | `rate(erpc_upstream_request_errors_total[5m]) / rate(erpc_upstream_request_total[5m]) > 0.05` by upstream | 5% error rate for 5 min |
| SlowRequests | `histogram_quantile(0.95, rate(erpc_upstream_request_duration_seconds_bucket[5m])) > 1` by upstream | p95 > 1 s for 5 min |
| HighRateLimiting | `rate(erpc_upstream_request_self_rate_limited_total[5m]) / rate(erpc_upstream_request_total[5m]) > 0.1` by upstream | 10% rate-limited for 5 min (note: `self_rate_limited_total` is a stale metric name — use `erpc_rate_limits_total`) |
| NetworkRateLimiting | `rate(erpc_network_request_self_rate_limited_total[5m]) by (network) > 10` | >10 rps rate-limited (stale name, same caveat) |
| HighRequestRate | `rate(erpc_upstream_request_total[5m]) by (upstream) > 1000` | >1000 rps per upstream for 5 min |
| LowRequestRate | `rate(erpc_upstream_request_total[5m]) by (upstream) < 1` | <1 rps per upstream for 15 min |

Note: two alert rules reference metric names that have changed since the rules were written (`erpc_upstream_request_self_rate_limited_total`, `erpc_network_request_self_rate_limited_total`). Use `erpc_rate_limits_total{scope="local"}` as the replacement.

**Notable log lines from the metrics server:**
- `"starting metrics server on port: %d"` (Info, `erpc/init.go:144`)
- `"error starting metrics server: %s"` (Error, `erpc/init.go:156`)
- `"shutting down metrics server..."` (Info, `erpc/init.go:161`)
- `"metrics server forced to shutdown: %s"` (Error, `erpc/init.go:164`)
- `"metrics server stopped"` (Info, `erpc/init.go:166`)
- `"failed to set histogram buckets, using defaults"` (Warn, `erpc/init.go:56`)

---

### Edge cases & gotchas

1. **`hostV4` and `hostV6` are defined but unused.** The metrics server binds `":%d"` (all interfaces), not `hostV4 + ":" + port`. Setting `metrics.hostV4: 127.0.0.1` does NOT make the metrics server listen only on loopback. Firewall or network policy must restrict scrape access. (`erpc/init.go:149`)

2. **`metrics.enabled` defaults to `true` in production but `nil` (disabled) under `go test`.** `util.IsTest()` detects the test build tag. Any integration test that starts a full `erpc.Init` will NOT spin up a metrics server unless it explicitly sets `Enabled: true`. (`common/defaults.go:750-752`)

3. **Changing `errorLabelMode` does not affect already-accumulated counter series.** Because counter Vec children are created lazily on the first `WithLabelValues` call, any series created before `erpc.Init` sets the mode (e.g. in init-time code paths) carry `verbose` labels while subsequent series carry `compact` labels. This mix will appear as two parallel label-value sets in Prometheus until the verbose series naturally expire or are deleted.

4. **Changing `histogramDropLabels` after the first `SetHistogramBuckets` call panics.** Prometheus does not allow changing the label set of a registered metric. Re-calling `SetHistogramBuckets` with a different filter is intentionally not supported (`telemetry/metrics.go:942-950`). Config-analyzer calls `SetHistogramBuckets` for validation (`erpc/config_analyzer.go:285`); if the same process then calls it again via `erpc.Init` with a different filter, it panics. In practice the config-analyzer runs in a separate code path before `Init`.

5. **`registerOrReuse` silently returns the existing `LabeledHistogram` on identical re-registration.** Calling `SetHistogramBuckets` twice with the same bucket string and same filter is idempotent: the second registration attempt returns the already-registered instance (`telemetry/metrics.go:984-995`). Only the global vars are updated; the Prometheus registry is not touched again.

6. **`erpc_rate_limiter_budget_decision_total` is a dead metric.** It is registered at startup via `promauto` (so it appears in `/metrics`) but has zero call sites in production code. Scraping it will always return 0. (`telemetry/metrics.go:515-519`, documented in `docs/kb/_census/metrics.md`)

7. **`erpc_network_hedge_delay_seconds` is a dormant `LabeledHistogram`.** It is registered via `SetHistogramBuckets` and appears in `/metrics`, but the only `Observe` call is in `telemetry/labeled_histogram_test.go:151`. All production hedge-related timing uses `erpc_network_request_duration_seconds` for actual duration and `erpc_network_hedged_request_total`/`hedge_discards_total` for counts. (`telemetry/metrics.go:813`)

8. **`erpc_cors_requests_total` `project` label receives the URL path, not the project id.** This is a known mislabeling — `r.URL.Path` is passed where the project id should go. The series will have values like `/myproject/evm/1` instead of `myproject`. (`erpc/http_server.go:1020`)

9. **Idle sweep protects only `upstream_request_duration_seconds` and `rate_limits_total` Prometheus series.** All other high-cardinality histograms and counters (e.g. `network_request_duration_seconds`, `upstream_request_total`) are never DeleteLabelValues'd — their cardinality is bounded only by the number of distinct (project, network, upstream, method) tuples, not by idle access time. For method-flood attack scenarios, consider also `histogramDropLabels: [user, agent_name]`.

10. **`ParseHistogramBuckets` silently sorts the input.** If the operator provides buckets out of order, they are sorted automatically (`telemetry/metrics.go:1011`). No warning is emitted. The result is valid but may differ from the user's intent if they relied on a specific order.

11. **`ERPC_NOMETRICS=1` drops the default `go_*`/`process_*` collectors too.** It replaces the entire default registry with a fresh empty one before any `promauto` init fires. The new registry has no pre-registered collectors, so no runtime or process stats are collected. (`cmd/erpc/initflags.go:22-28`)

12. **The idle sweep fires at the rotation-tick granularity, not in real-time.** With default 5-minute window and 10 buckets, each tick is 30 seconds; sweep fires every 10 ticks = every 5 minutes. An idle series is evicted up to `idleEvictionAfter + 5 min` after its last observation, not exactly at 30 minutes. (`health/tracker.go:557-563`)

13. **Cordoned upstreams are never swept.** If an upstream is cordoned, its tracker entry is preserved regardless of how long it sits idle (`health/tracker.go:607-609`). The associated `erpc_upstream_cordoned{...} = 1` gauge will remain in `/metrics` until the upstream is uncordoned and subsequently goes idle for `idleEvictionAfter`.

14. **`erpc_network_evm_block_range_requested_total` uses a `bucket` label derived from block number division.** With `EvmBlockRangeBucketSize = 100000` and a chain at block 20M, there are 200 bucket label values. This counter is NOT covered by the idle sweep. Long-running instances accessing many distinct block ranges accumulate high cardinality on this metric permanently.

15. **`erpc_selection_*` metrics carry `method` as a label.** For networks with permissive method filters and malicious callers, a flood of unique method names (via method injection) creates unbounded series. The selection policy engine does validate method names, but `erpc_selection_*` gauges are set/overwritten on every tick regardless of request rate, so the Prometheus Vec grows monotonically. Add `method` to a Prometheus relabeling drop rule for untrusted deployments.

16. **The bundled Prometheus scrape target includes a placeholder `REPLACE_SERVICE_ENDPOINT_HERE:REPLACE_SERVICE_PORT_HERE`.** The reference `monitoring/prometheus/prometheus.yml` has a Railway deployment placeholder that will cause a parse error in strict Prometheus configs. Remove it or replace it before using the bundled config in production.

17. **`MetricNetworkDynamicBlockTimeMilliseconds` is bounded 10ms–120s by the health tracker before emission.** Extremely fast chains (block time < 10ms) or stuck chains (no blocks for > 2 minutes) will report the floor/ceiling value, not the true EMA, and a separate log message is emitted by the health tracker. (`health/tracker.go:1456`, bound check in same file)

18. **`erpc_network_evm_block_range_requested_total` uses dynamic tip-relative bucket labels, not just static division.** When tip is known, `ComputeBlockHeatmapBucket` produces human-readable labels: `"TIP"`, `"LATEST"`, `"FINALIZED"`, `"L100k"`, `"100k-200k"`, `"119m-120m"`, etc. When tip is unknown (startup, before any block polled), falls back to static `EvmBlockRangeBucketSize=100000` aligned labels. This means the `bucket` label value space is unbounded for near-tip relative labels — plan Prometheus cardinality accordingly. (`erpc/block_heatmap.go:87-175`)

19. **`erpc_upstream_block_head_large_rollback` gauge is silent for rollbacks ≤ 1024 blocks.** `DefaultToleratedBlockHeadRollback = 1024` is the threshold at `architecture/evm/evm_state_poller.go:27`. The shared state variable silently ignores rollbacks at or below this value — they are treated as normal block-number noise. Only genuine large reorgs or upstream resets (> 1024 block drop) trigger the gauge. A gauge value of 0 is normal; non-zero warrants investigation. (`data/shared_state_variable.go:208,303`, `data/shared_state_variable_test.go:132-145`)

20. **`erpc_ristretto_cache_current_cost` requires `memory.emitMetrics: true` to be non-zero.** Without this flag the gauge is registered (appears in `/metrics`) but always 0 because the collection goroutine never starts. This is the only metric in the system that requires an explicit config opt-in to be meaningful. (`data/memory.go:71,224-231`)

21. **`erpc_upstream_cordoned` `category` label is the cordon scope (method), not a request-category label.** `"*"` = wholesale cordon on all methods; a specific method string = per-method cordon. This is unintuitive because every other metric uses `category` for the RPC request category (e.g. `"call"`, `"read"`). Do not join `category` from this metric with `category` from request-accounting metrics. (`health/tracker.go:458-465`, `health/tracker.go:851-867`)

22. **`erpc_upstream_cordoned` `vendor` label is `"n/a"` when vendor is not configured.** When no vendor plugin is matched and `config.VendorName` is empty, `VendorName()` returns `"n/a"` (not empty string, not the upstream ID). This affects dashboard JOINs between cordoned metrics and upstream request metrics — ensure the vendor filter matches `"n/a"` for unvendored upstreams. (`upstream/upstream.go:275-286`)

23. **`erpc_auth_failed_total` always has `strategy="database"`.** Only the database authentication strategy records this counter. Networks using secret, JWT, or other strategies will always show zero for this counter regardless of auth failure rate. Monitor per-strategy auth failures via `erpc_network_failed_request_total{error=~"ErrAuthUnauthorized.*"}` for other strategies. (`auth/strategy_database.go:467-472`)

24. **`erpc_upstream_attempt_outcome_total` `is_hedge` and `is_retry` are `"true"`/`"false"` strings (not `"1"`/`"0"`).** Produced by local `boolStr()` which returns the literal strings `"true"` or `"false"`. Use `{is_hedge="true"}` in Prometheus queries, not `{is_hedge="1"}`. (`upstream/upstream.go:80-85`)

25. **`erpc_upstream_request_duration_seconds` full schema has `user` but the response-size histogram does not.** This is intentional: the response size histogram is designed with tight cardinality (`project/network/category/finality` only) since user-level breakdown of response size is not actionable. To get per-user latency you must keep the full `upstream_request_duration_seconds` label set (i.e. do NOT add `user` to `histogramDropLabels` if per-user analysis is needed). (`telemetry/metrics.go:914-920`)

26. **`ErrEndpointServerSideException` (#54) preserves the upstream's original HTTP status code, but no dedicated Prometheus label exposes it.** When an upstream returns an HTTP 5xx that does not match any known error pattern, `error_normalizer.go` wraps it in `NewErrEndpointServerSideException(cause, details, r.StatusCode)` (`architecture/evm/error_normalizer.go:233,592,637,741,766,815,851,864`). The `originalStatusCode int` field is stored directly on the struct (`common/errors.go:1951-1979`) and is returned by `ErrorStatusCode()` so eRPC echoes the upstream's exact status code back to the caller (`common/errors.go:1974-1978`). **For debugging in metrics:** the error surfaces through `erpc_upstream_request_errors_total{error="ErrEndpointServerSideException"}` (compact mode) or with the full message chain in verbose mode; neither label carries the raw numeric status code. **For debugging in traces:** `SetTraceSpanError` serialises the full `StandardError` as JSON via `SonicCfg.MarshalToString(stdErr)` and attaches it as `span.RecordError(...)` plus `attribute.String("error.code", stdErr.CodeChain())` (`common/tracing_core.go:163-184`). The JSON payload includes `details` (the map passed at construction — see `error_normalizer.go` call sites), but `originalStatusCode` is a private struct field and is NOT in the JSON output (it is not part of `BaseError.Details`). **The only reliable way to see the original HTTP status code** is via structured logs: `BaseError.MarshalZerologObject` emits `"details"` as a zerolog interface (`common/errors.go:409-413`), and call-sites in `upstream/upstream.go` log the full error object, so the status code must be inferred from the upstream's response log lines (logged at DEBUG level with HTTP status before wrapping). There is no Prometheus label, no OTEL span attribute, and no log field named `originalStatusCode` or `http_status` — it flows only as part of the outer `ErrorStatusCode()` return value and the error string serialisation. (`common/errors.go:1951-1979`, `architecture/evm/error_normalizer.go:233`, `common/tracing_core.go:163-184`)

---

### Source map

- `telemetry/metrics.go` — all 122 metric definitions (counters, gauges, promauto histograms, LabeledHistograms); bucket constants; `DefaultHistogramBuckets`; `SetHistogramBuckets`; `buildFilterAwareHistograms`; `registerOrReuse`; `ParseHistogramBuckets`.
- `telemetry/labeled_histogram.go` — `HistogramLabelFilter`; `SetHistogramLabelFilter`; `LabeledHistogram` struct with `WithLabelValues`/`DeleteLabelValues`/`ActiveLabelValues`/`RegisterOrReplaceHistogram`.
- `telemetry/handles.go` — `CounterHandle`/`GaugeHandle`/`ObserverHandle` label-bound child caches using `sync.Map`; `ResetHandleCache`.
- `erpc/init.go:47-57` — metric initialization sequence: `SetHistogramLabelFilter` → `SetHistogramBuckets`; network alias resolver install; metrics HTTP server lifecycle.
- `erpc/init.go:137-170` — metrics HTTP server construction: `promhttp.Handler()` on `:%d`, ReadHeaderTimeout 10s, graceful shutdown 5s.
- `cmd/erpc/initflags.go` — `ERPC_NOMETRICS=1` and `ERPC_NOLOGS=1` env var handling; replaces default registry before promauto fires.
- `common/config.go:2543-2564` — `MetricsConfig` struct definition: Enabled, HostV4, HostV6, ListenV4/V6 (dead), Port, ErrorLabelMode, HistogramBuckets, HistogramDropLabels, HistogramLabelOverrides.
- `common/defaults.go:749-767` — `MetricsConfig.SetDefaults`: Enabled (true unless test), HostV4 (0.0.0.0), HostV6 ([::]), Port (4001), ErrorLabelMode (compact).
- `common/validation.go:131-161` — `MetricsConfig.Validate`: enabled requires host+port; errorLabelMode must be verbose/compact; histogramBuckets must be valid floats.
- `common/errors.go:17-114` — `errorLabelMode` global; `SetErrorLabelMode`; `ErrorSummary` (the function producing `error` label values for all error-labeled metrics).
- `health/tracker.go:481-667` — `DefaultIdleEvictionAfter` constant; `NewTracker`; `rotateMetricsLoop`; `sweepIdle`; `sweepIdleObservers` (evicts `upstream_request_duration_seconds` and `rate_limits_total` Prometheus series).
- `monitoring/prometheus/prometheus.yml` — reference scrape config (scrape interval 10s, target `host.docker.internal:4001`).
- `monitoring/prometheus/alert.rules` — 6 bundled alerting rules (note two reference stale metric names).
- `monitoring/catch-up-metrics.md` — detailed operator guide for reading `network_retry_attempt_total` + `network_data_unavailable_wait_seconds` together; covers bucket design rationale for `CatchUpWaitHistogramBuckets`, finality-label split, Little's Law pressure interpretation, and tuning knobs.
- `monitoring/grafana/` — pre-built Grafana dashboard JSON and datasource config (references `erpc_` metrics throughout).
