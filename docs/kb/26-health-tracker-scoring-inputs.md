# KB: Health tracking: rolling windows, quantiles, error rates

> status: complete
> source-dirs: health/tracker.go, health/rolling.go, health/quantile.go, health/tracker_test.go, health/quantile_test.go, internal/policy/eval.go, internal/policy/slot.go, internal/policy/decision.go, common/adaptive_duration.go, common/timeout_func.go, upstream/upstream_executor.go, telemetry/metrics.go

## L1 — Capability (CTO view)

The health tracker is the in-process, lock-free signal store that every routing decision in eRPC reads from. It maintains rolling-window counters (requests, errors, throttles, misbehaviors) and a DDSketch-based latency quantile tracker for every `(upstream, method)` pair — plus cross-method and per-network aggregates — inside `rollingBuckets = 10` sub-buckets that slide forward at `windowSize / 10` cadence. The tracker also records point-in-time state metrics (block-head lag, finalization lag, cordon flags) that drive upstream exclusion and selection scoring. Latency quantile data doubles as the live input to adaptive hedge delays and timeout durations, so infrastructure decisions adapt to observed upstream behavior without operator intervention.

## L2 — Mechanics (staff-engineer view)

**Key dimensions.** Every measurement is stored in up to four dimensions simultaneously: `(upstream, method, finality)` for the most-specific bucket, two rollup aggregates `(upstream, method, All)` and `(upstream, "*", All)`, and optionally `(upstream, "*", finality)` when finality tracking is enabled. A parallel `(network, method)` hierarchy is maintained for network-level signal (also with a `"*"` wildcard). This fanout is performed by `getUpsKeys` and `getNtwKeys` on every `Record*` call (`health/tracker.go:L689-L740`).

**Rolling-window implementation.** `RollingCounter` (`health/rolling.go:L31-L98`) is a ring of `rollingBuckets = 10` `atomic.Int64` slots. `Add` writes to the newest slot; `Load` sums all ten; `RotateOldest` zeroes the oldest slot and advances the head pointer. The result is a sliding window with no tumble cliff: each rotation drops at most 1/10th of the accumulated data. The `QuantileTracker` (`health/quantile.go:L20-L169`) uses the same ring structure, but each slot is a `ddsketch.DDSketch` (1% relative accuracy). `RotateOldest` re-inits the oldest sketch; `GetQuantile` / `GetQuantiles` merges all ten sketches into an ephemeral copy, then reads from it. A single `mergedSnapshot` serves all five quantile reads to avoid five redundant merges per tick (`health/quantile.go:L113-L124`).

**Rotation loop.** `Tracker.Bootstrap` starts `rotateMetricsLoop` (`health/tracker.go:L522-L566`). The loop ticks at `windowSize / rollingBuckets`. Every tick it calls `TrackedMetrics.Rotate()` on every entry in both `upsMetrics` and `ntwMetrics` sync.Maps, advancing each RollingCounter and each QuantileTracker one slot. Every `sweepEveryRotations = rollingBuckets = 10` ticks (i.e. once per full window), `sweepIdle()` evicts entries whose `LastAccessedAtMs` is older than `idleEvictionAfter` (default 30 min). State metrics (`BlockHeadLag`, `FinalizationLag`, `Cordoned`) are intentionally NOT touched by `Rotate`.

**What is and is not counted as an error.** `RecordUpstreamFailure` (`health/tracker.go:L957-L996`) silently drops calls for these error codes: `ErrCodeEndpointExecutionException` (EVM reverts — legitimate upstream behavior), `ErrCodeUpstreamExcludedByPolicy`, `ErrCodeUpstreamRequestSkipped`, `ErrCodeUpstreamShadowing`, `ErrCodeEndpointUnsupported`, `ErrCodeEndpointCapacityExceeded` (remote 429, already penalized via ThrottledRate), `ErrCodeEndpointClientSideException` (bad request — user's fault), `ErrCodeEndpointRequestCanceled` and `ErrCodeUpstreamHedgeCancelled` (client disconnects and hedge races — indistinguishable from "client gave up", and slowness is already captured in ResponseQuantiles). Only genuine upstream-quality failures (`connection refused`, `5xx`, genuine timeouts, etc.) reach `ErrorsTotal`.

**What feeds ResponseQuantiles.** `RecordUpstreamDuration` only calls `ResponseQuantiles.Add` when `isSuccess = true` (`health/tracker.go:L931-L950`). The callers in `upstream.go` pass `isSuccess=true` for: successful responses, EVM reverts (upstream did real work), and canceled-by-hedge / client-disconnect / engine-timeout observations (which represent a lower-bound on actual upstream latency). Fast error paths (connection refused, 5xx) pass `isSuccess=false` so a failing-fast upstream does not get labeled "fastest in the pool".

**Block-head and finalization lag.** `SetLatestBlockNumber` and `SetFinalizedBlockNumber` maintain a per-network maximum via `NetworkMetadata` (`health/tracker.go:L54-L72`). When any upstream reports a new network-level maximum, lag is recomputed for every upstream in that network using the `upstreamsByNetwork` index (O(network keys) rather than full map scan). The lag value (`networkMax - upstreamValue`) is stored in `TrackedMetrics.BlockHeadLag` / `FinalizationLag` (`atomic.Int64`) — it is NOT part of the rolling window and persists across rotations by design (`health/tracker_test.go:L387-L389`).

**Dynamic block time EMA.** `SetLatestBlockNumber` feeds each new `(blockNumber, blockTimestamp)` pair into an exponential moving average with `alpha = 0.1` (effective window ~19 samples). Sub-second precision is recovered on fast chains (Arbitrum, etc.) by accumulating block gaps when consecutive blocks share the same timestamp and dividing the timestamp delta by the block count when the timestamp ticks (`health/tracker.go:L1394-L1459`). The EMA is not published until at least `blockTimeMinSamples = 3` pairs have been collected. Sanity bounds: values outside `[10ms, 120s]` are rejected and the internal EMA is reset to the last published value to prevent delayed spikes after a chain halt. The result is stored in `NetworkMetadata.evmBlockTime` (nanoseconds) and read via `GetNetworkBlockTime`.

**How lag becomes wall-clock time.** The policy engine reads `GetNetworkBlockTime(networkId)` and multiplies block-count lag by the EMA-estimated block time to produce `BlockHeadLagSeconds` and `FinalizationLagSeconds` fields on the `UpstreamMetrics` snapshot (`internal/policy/eval.go:L93-L97`). If block time is not yet available (EMA cold), these stay 0 and any policy predicate that trips on time-based lag simply does not fire — conservative behavior by design.

**Metrics as scoring inputs.** On each policy slot tick, `snapshotMetrics` (`internal/policy/slot.go:L614-L649`) calls `readUpstreamMetrics` (`internal/policy/eval.go:L63-L103`) for every upstream. This reads the `TrackedMetrics` for `(upstream, method, finality)` and converts it into `UpstreamMetrics` — the JS-visible snapshot the selection policy evaluates. Three views are captured: the per-(method, finality) local view (`u.metrics`), the per-(upstream, "*", All) aggregate (`u.metricsAcrossMethods`), and a per-method breakdown (`u.metricsByMethod`). `GetQuantiles` with a shared merge is used to read p50/p70/p90/p95/p99 in one shot.

**Adaptive hedge and timeout.** `AdaptiveDuration.Resolve(qt)` (`common/adaptive_duration.go:L83-L109`) takes a `QuantileTracker` and returns `base + qt.GetQuantile(quantile)` clamped to `[min, max]`. When the tracker has no data (cold start), `adaptive` falls back to `min` so the cap is sensible immediately after boot. `ResolveForRequest` (`common/adaptive_duration.go:L270-L290`) looks up the network's per-method `TrackedMetrics.GetResponseQuantiles()` automatically. Both the upstream-level hedge delay (`upstream/upstream_executor.go:L289`) and the quantile-driven timeout (`common/timeout_func.go:L49-L82`) use this path. An auto-floor `min = base/2` (or 500ms) is injected when `quantile > 0` but `min` is unset, to prevent a feedback loop where the timeout shrinks to the quantile and fast-failing requests collapse the quantile to the floor (`common/timeout_func.go:L30-L37`).

**Idle eviction and cardinality defense.** Each `TrackedMetrics` carries `LastAccessedAtMs` (unix milliseconds). Every `Record*` and `Get*` call updates it. The sweep loop, firing every full window (every `rollingBuckets` rotation ticks), deletes entries older than `idleEvictionAfter` (default 30 minutes) from both `upsMetrics` and `ntwMetrics`, and also calls `DeleteLabelValues` on the matching Prometheus `MetricVec` entries to actually release cardinality from the registry. Method wildcards (`method == "*"`) and cordoned entries are never evicted. This defends against method-flood attacks where a hostile client sends random JSON-RPC method names to grow the per-method map without bound.

**Cordon state.** Cordon flags are stored on the all-finalities `(upstream, method, All)` key to be finality-agnostic (`health/tracker.go:L804-L818`). `IsCordoned` checks both the method-specific and the `"*"` wildcard slots (`health/tracker.go:L853-L863`). Cordon is monotonic: operators must call `Uncordon` explicitly; rotation and idle eviction never clear it. `Uncordon` observes a `MetricUpstreamCordonDurationSeconds` histogram sample before clearing the flag.

## L3 — Exhaustive reference (agent view)

### Config fields

The tracker has no YAML config. It is constructed by `NewTracker(logger, projectId, windowSize)` where `windowSize` is derived from `SelectionPolicyConfig.ScoreMetricsWindowSize` (a duration field in each network/project selection policy configuration). `DefaultIdleEvictionAfter = 30 * time.Minute` is a package-level constant (`health/tracker.go:L487`). `SetIdleEvictionAfter(0)` disables sweeping entirely (matches pre-fix behavior and is used by some tests).

The sliding-window bucket count is the package constant `rollingBuckets = 10` (`health/rolling.go:L18`), not configurable at runtime.

The DDSketch relative accuracy is hardcoded at `0.01` (1%) (`health/quantile.go:L36`).

The block-time EMA alpha is hardcoded at `blockTimeEmaAlpha = 0.1`; minimum samples before publishing is `blockTimeMinSamples = 3` (`health/tracker.go:L1385-L1387`).

The block-time sanity bounds are hardcoded at `[10ms, 120s]` (`health/tracker.go:L1447`).

`AdaptiveDuration` config fields used to access tracker quantiles (relevant for hedge/timeout/consensus):

| YAML field | Type | Default | Behavior |
|---|---|---|---|
| `quantile` | `float64` | `0` (static mode) | Which quantile (0–1) to read from `QuantileTracker.GetQuantile`. When `0`, `Resolve` returns `Base` unchanged. |
| `base` | `Duration` | `0` | Static component added before the quantile value. |
| `min` | `Duration` | `0` (auto-floor = `base/2` or `500ms` when `quantile > 0` in timeout config) | Floor after adding `base + adaptive`. Also the cold-start fallback when quantile data is absent. |
| `max` | `Duration` | `0` (no ceiling) | Ceiling for the combined result. Required when `quantile > 0` and `base == 0` (`common/adaptive_duration.go:L129-L131`). |

Source: `common/adaptive_duration.go:L60-L65`, validation `L122-L138`.

### Behaviors & algorithms

**`RollingCounter.Add(delta)`**: atomically adds `delta` to the newest bucket `(head + N - 1) % N`. Safe concurrent with `RotateOldest`; worst case is a delta landing in a bucket that is about to be evicted (one-rotation rounding error). `health/rolling.go:L52-L56`.

**`RollingCounter.Load()`**: sums all `N = 10` bucket atomics. O(10). `health/rolling.go:L60-L66`.

**`RollingCounter.RotateOldest()`**: zeroes bucket at `head`, advances `head = (head + 1) % N`. `health/rolling.go:L71-L76`.

**`QuantileTracker.Add(value float64)`**: writes to sketch at `(head + N - 1) % N` under write lock. `value` is in seconds (the callers divide `time.Duration.Seconds()` before calling). Logs a warning if `ddsketch.Add` returns an error. `health/quantile.go:L42-L49`.

**`QuantileTracker.GetQuantile(qtile float64) time.Duration`**: calls `mergedSnapshot()` to produce a merged ephemeral DDSketch from all 10 sub-buckets (under read lock), then calls `ddsketch.GetValueAtQuantile(qtile)`. Guards against `error`, `NaN`, and `Inf` — all return `0`. On empty sketch returns `0`. `health/quantile.go:L92-L96`, `L150-L168`.

**`QuantileTracker.GetQuantiles(qtiles []float64) []time.Duration`**: calls `mergedSnapshot()` ONCE and reads all quantiles from the single merged sketch. Semantically identical to calling `GetQuantile` N times but with 1 merge instead of N. Crucial optimization: `snapshotMetrics` reads 5 quantiles per (upstream, method) per tick; with 5 upstreams × 30 methods = 150 calls that would produce 750 merges without this optimization, reduced to 150. `health/quantile.go:L113-L124`.

**`TrackedMetrics.ErrorRate()`**: `ErrorsTotal.Load() / RequestsTotal.Load()`, returns `0` when requests is 0. `health/tracker.go:L161-L167`.

**`TrackedMetrics.ThrottledRate()`**: `RemoteRateLimitedTotal.Load() / RequestsTotal.Load()`. `health/tracker.go:L173-L179`.

**`TrackedMetrics.MisbehaviorRate()`**: `MisbehaviorsTotal.Load() / RequestsTotal.Load()`. `health/tracker.go:L182-L188`.

**`TrackedMetrics.Rotate()`**: calls `RotateOldest` on each counter and the quantile tracker. Does NOT touch `BlockHeadLag`, `FinalizationLag`, `Cordoned`, `LastCordonedReason`. `health/tracker.go:L218-L224`.

**`Tracker.getUpsKeys(upstream, method, finality)`** (`health/tracker.go:L689-L711`):
- `trackByFinality == false` OR `finality == DataFinalityStateAll`: returns 2 keys: `(ups, method, All)` and `(ups, "*", All)`.
- `trackByFinality == true` AND `finality` is a specific state: returns 4 keys: `(ups, method, finality)`, `(ups, method, All)`, `(ups, "*", finality)`, `(ups, "*", All)`.

**`Tracker.getNtwKeys(upstream, method)`**: always returns 2 keys: `(networkId, method)` and `(networkId, "*")`. `health/tracker.go:L734-L740`.

**`Tracker.GetUpstreamMethodMetrics(up, method, finality)`** (`health/tracker.go:L1057-L1076`): if `finality == All` OR `trackByFinality == false`, returns the `(ups, method, All)` bucket (creating it if needed). Otherwise: attempts to load `(ups, method, finality)` — if present, returns it; if absent (no Record* has fed it yet), falls back to `(ups, method, All)`. This prevents the eval from seeing an empty metrics object for a specific-finality query.

**`Tracker.GetUpstreamMetrics(ups)`** (`health/tracker.go:L1098-L1138`): returns a `map[string]*TrackedMetrics` keyed by method name using the `upstreamsByNetwork` index for O(per-network keys) lookup. Falls back to a full `upsMetrics.Range` scan (cold path) if the index is not yet populated. The returned map contains only the `(method, All)` aggregate per method (never per-finality buckets).

**`readUpstreamMetrics` in eval.go** (`internal/policy/eval.go:L63-L103`):
- Reads `TrackedMetrics` for `(upstream, method, finality)`.
- Reads five quantiles via `GetQuantiles([0.50, 0.70, 0.90, 0.95, 0.99])` (one merge).
- Converts block-count lag to wall-clock seconds using `GetNetworkBlockTime`. Zero if block time not yet estimated.
- Extracts `CordonedReason` if the upstream is cordoned.

**`snapshotMetrics` in slot.go** (`internal/policy/slot.go:L614-L649`):
- `local[upstreamId]`: per-(method, finality) view.
- `acrossMethods[upstreamId]`: per-(upstream, "*", All) view (shared with `local` when slot method = "*" and finality = All).
- `byMethod[upstreamId][method]`: per-method map built by walking `GetUpstreamMetrics(u)` — excludes the wildcard `"*"` bucket and methods with zero requests.

**`AdaptiveDuration.Resolve(qt QuantileTracker)`** (`common/adaptive_duration.go:L83-L109`):
1. If `Quantile <= 0`: return `Base`.
2. If `qt != nil`: `adaptive = qt.GetQuantile(Quantile)`.
3. If `adaptive <= 0`: `adaptive = Min` (cold-start fallback).
4. `v = Base + adaptive`.
5. Clamp `v` to `[Min, Max]` if set.
6. Return `v`. Zero means "disabled" to callers.

**`NewTimeoutFunc` auto-floor** (`common/timeout_func.go:L30-L37`): when `Quantile > 0` and `Min` is unset, auto-sets `Min = Base/2` (or `500ms` if Base is also zero). Prevents feedback loops where successes only arrive under the quantile cap, collapsing the quantile until the cap is too small to allow any success.

### Observability

| Metric | Type | Labels | When it fires |
|---|---|---|---|
| `erpc_upstream_request_duration_seconds` | Histogram | `project, vendor, network, upstream, category, composite, finality, user` | `RecordUpstreamDuration` — fires for ALL calls (success AND failure). The rolling quantile tracker only receives `isSuccess=true` samples; Prometheus receives all. `health/tracker.go:L952-L954` |
| `erpc_upstream_block_head_lag` | Gauge | `project, vendor, network, upstream` | `SetLatestBlockNumber` — set to `networkMax - upstreamLatest` for each upstream. `health/tracker.go:L1340-L1341` |
| `erpc_upstream_finalization_lag` | Gauge | `project, vendor, network, upstream` | `SetFinalizedBlockNumber` — set to `networkFinalizedMax - upstreamFinalized`. `health/tracker.go:L1523-L1524` |
| `erpc_upstream_latest_block_number` | Gauge | `project, vendor, network, upstream` | `SetLatestBlockNumber` — per-upstream latest block, plus `("*", "*")` for the network-level maximum. `health/tracker.go:L1295-L1296, L1328-L1329` |
| `erpc_upstream_finalized_block_number` | Gauge | `project, vendor, network, upstream` | `SetFinalizedBlockNumber`. `health/tracker.go:L1498-L1501, L1508-L1509` |
| `erpc_network_latest_block_timestamp_distance_seconds` | Gauge | `project, network, origin` | `SetLatestBlockNumber` when `blockTimestamp > 0` and the new block is the network maximum. Distance = `(now_ms - blockTimestamp * 1000) / 1000`. `health/tracker.go:L1313-L1320` |
| `erpc_network_dynamic_block_time_milliseconds` | Gauge | `project, network` | `updateBlockTimeSample` when EMA is updated and within sanity bounds `[10ms, 120s]`. `health/tracker.go:L1456-L1458` |
| `erpc_upstream_cordoned` | Gauge | `project, vendor, network, upstream, category, reason` | `Cordon` sets to `1`; `Uncordon` sets to `0`. `health/tracker.go:L817, L848` |
| `erpc_upstream_cordon_event_total` | Counter | `project, network, upstream, action` | `Cordon` (action=`"cordon"`) on OFF→ON transition; `Uncordon` (action=`"uncordon"`) on ON→OFF transition. `health/tracker.go:L812-L815, L842-L845` |
| `erpc_upstream_cordon_duration_seconds` | Histogram | `project, network, upstream` | `Uncordon` — records the duration from `CordonedAtMs` to `Uncordon` time. `health/tracker.go:L834-L840` |
| `erpc_upstream_block_head_large_rollback` | Gauge | `project, vendor, network, upstream` | `RecordBlockHeadLargeRollback` — set to `currentVal - newVal`. `health/tracker.go:L1553-L1564` |
| `erpc_rate_limits_total` | Counter | `project, network, vendor, upstream, category, finality, user, agent_name, budget, scope, auth, origin` | `RecordUpstreamRemoteRateLimited` with `scope="remote"`, `budget="<remote>"`, `origin="upstream"`. `health/tracker.go:L1038` |
| `erpc_network_timeout_duration_seconds` | Histogram | `project, network, category, finality` | `NewTimeoutFunc` resolver — every time a dynamic timeout is computed (not just when it fires). `common/timeout_func.go:L73-L79` |

### Edge cases & gotchas

1. **Block-head lag persists across window resets.** `BlockHeadLag` and `FinalizationLag` are `atomic.Int64` state metrics, not rolling counters. `TrackedMetrics.Rotate()` deliberately skips them. A test explicitly verifies this: after `windowSize + 50ms`, requests and errors reset to 0 but the lag values are unchanged (`health/tracker_test.go:L387-L389`). This means a lagged upstream stays penalized even during a quiet period.

2. **Cordon is never auto-cleared.** The idle sweep explicitly preserves cordoned entries (`health/tracker.go:L607-L609`). Even after `idleEvictionAfter` (30 min) of silence, a cordoned upstream is never evicted. If a cordoned upstream has been completely silent for 30 min, its entry would otherwise vanish and the cordon would re-appear on next access — this is prevented.

3. **Quantile tracks only quality latency, not error latency.** A fast-failing upstream (e.g. connection refused in 1ms) has `isSuccess=false` everywhere, so its `ResponseQuantiles` stay empty and `GetQuantile()` returns 0. The policy eval sees p50/p90/p99 all 0. This is intentional: a 0 quantile is a signal that no reliable latency data exists, which a well-written policy should interpret conservatively rather than as "ultra-fast". Callers that trip on `p90 < threshold` will see `p90 = 0 < threshold` = true — a policy must explicitly handle the cold/empty case.

4. **Hedge-cancel latency IS in the quantile.** When the primary request loses a hedge race, `upstream.go` passes `isSuccess=true` and the elapsed time to `RecordUpstreamDuration`. This means the primary's canceled-time is a lower bound on its real completion time and does appear in ResponseQuantiles — deliberately. A test verifies this: `TestRecordUpstreamDuration_OnlySuccessInQuantile / error_latency_not_in_quantile` vs. the hedge test `HedgeWins_LatencyCapturedInResponseQuantiles` (`health/tracker_test.go:L849-L857`, `erpc/networks_hedge_test.go:L1131`).

5. **Execution-exception latency IS in the quantile.** EVM reverts count as `isSuccess=true` for duration tracking. A test at `health/tracker_test.go:L873-L884` verifies this. Rationale: the upstream did real work (decoded, executed, responded); its latency is a valid input for routing.

6. **Method-flood defense via idle sweep.** A hostile client sending random `eth_randomXXX` method names creates a `TrackedMetrics` entry per method. The idle sweep (default: fires once per full window = every `windowSize / rollingBuckets * rollingBuckets = windowSize`) evicts per-method entries with `LastAccessedAtMs < now - 30min`. The matching Prometheus MetricVec label sets are also deleted (`health/tracker.go:L640-L667`). Without `DeleteLabelValues`, the Prometheus registry keeps series forever (append-only model) even after the in-memory cache is evicted.

7. **`upstreamsByNetwork` index dedup preserves only one finality per (upstreamId, method).** The index stored in `upstreamsByNetwork` deduplicates on `(ups.Id(), method)`, keeping whichever entry is written first. Lag updates therefore use the index entry's finality key when walking the map, but then also explicitly mirror onto the `(method, All)` rollup to ensure lag is visible at the all-finalities grain — the grain that selection policies read (`health/tracker.go:L1215-L1219`, `L1371-L1372`).

8. **Block time EMA cold start.** `GetNetworkBlockTime` returns `0` until `blockTimeMinSamples = 3` pairs have been collected AND their timestamps differ. On fast chains where all three consecutive blocks share the same second-resolution timestamp, the EMA never advances its `prevTimestamp` on same-timestamp samples — `prevBlock` also does not advance, so the block gap accumulates until the timestamp ticks (`health/tracker.go:L1413-L1419`). This means on a 12-blocks/second chain (Arbitrum), the EMA cold-start may take several seconds for the timestamp to advance enough.

9. **Block time sanity-bound reset.** If `evmBlockTimeEmaNs` goes outside `[10ms, 120s]` (e.g. chain halt followed by burst catch-up), the new sample is rejected AND the internal EMA is reset to the last published value. This prevents a post-halt spike from temporarily reporting absurd block times (`health/tracker.go:L1447-L1451`).

10. **GetUpstreamMethodMetrics creates the entry on read.** Calling `GetUpstreamMethodMetrics` with a new `(upstream, method, finality)` tuple lazily creates the entry via `getUpsMetrics` → `LoadOrStore`. This means the policy's per-tick read path may trigger an insert and populate `upstreamsByNetwork` even if no `Record*` has run. The entry will have all-zero counters and an empty quantile sketch, but it will be "alive" (`LastAccessedAtMs` set to now) and will not be evicted for 30 minutes.

11. **Tracker window reset is gradual, not instantaneous.** After `windowSize` has elapsed, counters approach zero as all 10 buckets rotate through. A test (`TestTracker / MetricsOverTime`) waits `windowSize + 10ms` after writing, then writes fresh data, and asserts only the new data is visible (`health/tracker_test.go:L66-L73`). This works because `windowSize + 10ms` is just slightly more than the full rotation cycle, leaving no residual from the first window.

12. **`convertTrackedMetrics` vs. `readUpstreamMetrics`.** Both build an `UpstreamMetrics` from a `*TrackedMetrics`, but `readUpstreamMetrics` calls the tracker directly (which may fall back from finality-specific to all-finalities), while `convertTrackedMetrics` (`internal/policy/slot.go`) is used in the `byMethod` path where the bucket is already the all-finalities aggregate. They share the same `quantileFractions` array constant (`internal/policy/eval.go:L111`).

### Source map

- `health/tracker.go` — the main `Tracker` struct, all `Record*` / `Set*` / `Get*` / `Cordon*` methods, `rotateMetricsLoop`, `sweepIdle`, `updateBlockTimeSample`, `upstreamsByNetwork` index, and all Prometheus gauge/counter caches.
- `health/rolling.go` — `RollingCounter`: the lock-free sliding-window counter with `rollingBuckets = 10` sub-buckets; also defines the package constant used by `QuantileTracker`.
- `health/quantile.go` — `QuantileTracker`: DDSketch ring buffer with `Add`, `GetQuantile`, `GetQuantiles` (batched), `RotateOldest`, `Reset`, and NaN/Inf guards. `mergedSnapshot` is the shared merge used by all query paths.
- `health/tracker_test.go` — integration tests for rolling window semantics, block lag persistence, error filter matrix, latency-in-quantile rules, and method flood defense.
- `health/quantile_test.go` — unit tests for DDSketch accuracy, NaN/Inf guards, cold start, `GetQuantiles` semantic equivalence, and benchmark comparing batched vs. loop quantile reading.
- `internal/policy/eval.go` — `readUpstreamMetrics`: converts `TrackedMetrics` → `UpstreamMetrics` (the JS-visible snapshot); uses `GetQuantiles` for one-merge quantile reading; `quantileFractions` constant; `installSharedHelpers` for the JS `u.metrics.latencyP(q)` and `u.hasTag(t)` helpers.
- `internal/policy/slot.go` — `snapshotMetrics`: builds the three per-tick metric views (`local`, `acrossMethods`, `byMethod`) from tracker data; drives the slot's rotation-independent eval cycle.
- `internal/policy/decision.go` — `UpstreamMetrics` struct: the full JS-visible field spec with both block-count and wall-clock lag fields.
- `common/adaptive_duration.go` — `AdaptiveDuration.Resolve(qt)` and `ResolveForRequest(req)`: the quantile-driven duration resolution used by hedge and timeout configs.
- `common/timeout_func.go` — `NewTimeoutFunc`: builds a `TimeoutFunc` that reads per-method quantile data via `Network.GetMethodMetrics(m).GetResponseQuantiles()` at request time; includes the auto-floor logic.
- `upstream/upstream_executor.go` — calls `spec.ResolveForRequest(req)` for hedge delay; this ultimately reads the network's `TrackedMetrics.ResponseQuantiles`.
- `telemetry/metrics.go` — Prometheus metric declarations for all block-lag gauges, duration histograms, cordon counters/histograms, block-time gauge, and timestamp distance gauge referenced by the tracker.
