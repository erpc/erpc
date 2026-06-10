# KB: Failsafe — Hedge Policy

> status: complete
> source-dirs: failsafe/hedge.go, failsafe/hedge_test.go, common/config.go (HedgePolicyConfig), common/adaptive_duration.go, common/adaptive_duration_compat_test.go, common/defaults.go, common/errors.go, common/network.go, erpc/network_executor.go, erpc/http_server_hedge_test.go, architecture/evm/util.go, telemetry/metrics.go

## L1 — Capability (CTO view)

The hedge policy speculatively fires duplicate parallel RPC requests after a configurable delay, racing them against the primary attempt and returning the first acceptable response. When one upstream is slow (P99 tail latency), a hedge issued after P50–P70 delay typically returns from a faster upstream before the slow one finishes, dramatically cutting tail latency without affecting median cost. The winning response is kept; all losers are cleanly cancelled via context propagation. Hedge delay can be static (fixed milliseconds) or quantile-driven (computed from per-method P50/P90/P99 of observed response times).

## L2 — Mechanics (staff-engineer view)

**Wiring.** The hedge policy lives in `failsafe/hedge.go` as a generic `RunHedged[R]` function. `networkExecutor.runHedge` (in `erpc/network_executor.go`) wraps the upstream-sweep function and calls `RunHedged`. The executor composition for non-consensus requests is `retry(hedge(runUpstreamSweep))`, meaning one retry attempt fires the entire hedge race (`erpc/network_executor.go:L194-L203`). For consensus requests it is `consensus(retry(hedge(tryOneUpstream)))` where the hedge races individual upstream slots (`erpc/network_executor.go:L184-L187`).

**Race execution.** `RunHedged` fires the primary attempt immediately (goroutine 0). It then arms a pooled `time.Timer` to the delay returned by `delayFn(1)`; if the primary has not produced an acceptable result by that time, it spawns a second goroutine (hedge 1). After each hedge fires, it resets the timer for the next one, stopping when `fired-1 >= maxHedges`. All goroutines share a `siblingCtx` derived from a single `context.WithCancel(parentCtx)`; when a winner is selected, `cancelAll()` is called, propagating cancellation to any still-running siblings (`failsafe/hedge.go:L111-L255`).

**Result channel and keep predicate.** The channel is pre-allocated at `maxHedges+1` capacity so sends never block (`failsafe/hedge.go:L114`). Each goroutine writes exactly one `hedgeResult{r, err}`. The receive loop selects the first result for which `keep(r, err)` returns `true`. When `keep` returns `false`, the result is stored as `lastResult` but not released—the caller receives it as the final outcome if all legs finish unkept (`failsafe/hedge.go:L233-L253`).

**keep predicate for network use.** `networkExecutor.runHedge` implements `keep` to:
- return `false` for `ErrCodeNoUpstreamsLeftToSelect` (this leg exhausted its upstreams but siblings may still win, `erpc/network_executor.go:L578`)
- return `false` for empty `ErrUpstreamsExhausted` (no upstreams ever tried, `erpc/network_executor.go:L581-L585`)
- for other errors: `!IsRetryableTowardNetwork(err)` — non-retryable errors (client errors, execution reverts) are considered definitive and kept as the winner; retryable errors allow the race to continue (`erpc/network_executor.go:L588`)
- return `false` for null/empty responses when the method is NOT in `emptyResultAccept` — prevents a fast `{"result": null}` from cancelling siblings that may have real data (`erpc/network_executor.go:L591-L622`)
- return `true` otherwise (non-null, non-empty success) and emit `MetricNetworkHedgeWinnerTotal` (`erpc/network_executor.go:L551-L570`)

**release callback.** Losers that produced a response have their `*NormalizedResponse` released via `r.Release()` to return the underlying buffer to the pool (`erpc/network_executor.go:L626-L630`).

**Delay computation.** `delayFn` is called once per hedge index and calls `spec.ResolveForRequest(req)`. `ResolveForRequest` resolves the `AdaptiveDuration` by:
1. If `Quantile == 0`: return `Base` exactly (static mode)
2. If `Quantile > 0`: look up `network.GetMethodMetrics(method).GetResponseQuantiles()`, call `qt.GetQuantile(Quantile)`, add `Base`, clamp to `[Min, Max]`
3. Cold-start (no data yet, or `qt == nil`): fall back to `Min` if set, then the `[Min, Max]` clamp still applies (`common/adaptive_duration.go:L83-L109`, `common/adaptive_duration.go:L267-L290`)

**Defaults applied by SetDefaults.** When `hedge.delay.min == 0`, it is set to `defaultHedgeMinDelay = 100ms`. When `hedge.delay.max == 0`, it is set to `defaultHedgeMaxDelay = 999s`. When `hedge.maxCount == 0`, it defaults to `1`. These defaults apply to both explicitly configured hedges and to hedges inherited from parent defaults (`common/defaults.go:L2307-L2340`).

**System-level defaults.** The built-in project template (applied when `projects` is absent or has zero items) configures a network-scope hedge of `{quantile: 0.7, maxCount: 2}` with a 120s timeout (`common/defaults.go:L137-L141`). No upstream-scope hedge default is provided.

**Write method exclusion.** `runHedge` short-circuits to `inner(ctx, req)` (no hedging) for EVM methods identified by `evm.IsNonRetryableWriteMethod`: `eth_sendTransaction`, `eth_createAccessList`, `eth_submitTransaction`, `eth_submitWork`, `eth_newFilter`, `eth_newBlockFilter`, `eth_newPendingTransactionFilter`. `eth_sendRawTransaction` is intentionally NOT excluded (it supports idempotent retry/consensus handling elsewhere, `architecture/evm/util.go:L7-L17`). Composite requests (batches) are also excluded (`erpc/network_executor.go:L524`).

**Discard telemetry and ErrUpstreamHedgeCancelled.** When a hedge goroutine's context is cancelled by a sibling winner, the inner call returns `ErrEndpointRequestCanceled`. The network sweep loop detects `ErrCodeEndpointRequestCanceled` when `hedges > 0` and calls `recordHedgeDiscard` which increments `MetricNetworkHedgeDiscardsTotal` and returns `ErrUpstreamHedgeCancelled` (`erpc/networks.go:L1302-L1306`, `erpc/networks.go:L1883-L1903`).

`ErrUpstreamHedgeCancelled` (`ErrCodeUpstreamHedgeCancelled`, error code #33 in the taxonomy) is defined at `common/errors.go:L1447-L1462`. Key properties:
- **Retryability:** U:yes N:yes (both `IsRetryableTowardsUpstream` and `IsRetryableTowardNetwork` return `true`). The error struct carries no `retryableTowardNetwork=false` flag in its `Details` map, so the default "retryable" path applies in both functions.
- **`ErrUpstreamsExhausted.SummarizeCauses` bucket:** when `ErrUpstreamHedgeCancelled` is found in the error list of an `ErrUpstreamsExhausted`, it is counted in the `"cancelled"` bucket (`common/errors.go:L1000-L1002`). This means if ALL legs of a hedge race were cancelled (e.g., parent timeout kills the entire race before any winner is found), the `ErrUpstreamsExhausted` summary shows `cancelled=N` rather than lumping them into `other`.
- **Message:** `"hedged request cancelled in favor of another upstream response"`, with `upstreamId` in details identifying which upstream was cancelled.

**OnFire hook.** Each hedge fire (idx > 0) triggers `hooks.OnFire`, which increments both `req.ExecState().NetworkAttempts` and `req.ExecState().NetworkHedges` so the `X-ERPC-Attempts`/`X-ERPC-Hedges` response headers and attempt counters are accurate (`erpc/network_executor.go:L631-L641`).

**Timer pool.** `hedgeTimerPool` (a `sync.Pool`) reuses `*time.Timer` across requests. A newly-pool-created timer is immediately stopped to drain its channel; `borrowHedgeTimer` calls `t.Reset(d)` on the borrowed instance; `releaseHedgeTimer` calls `t.Stop()` + drains `t.C` before returning to the pool. This eliminates per-request `time.NewTimer` allocations on the hot path (profile showed this was visible at high RPS, `failsafe/hedge.go:L9-L60`).

**Interaction with retry.** Retry wraps hedge: each retry attempt fires a fresh hedge race. If the primary fails before the first hedge delay fires, the retry layer fires a new attempt immediately (no wait for the armed timer — `RunHedged` returns as soon as a winner or all-unkept scenario is resolved). If the hedge is in-flight (timer has already fired a sibling), retry waits for the hedge race to complete before deciding on the retry. This is locked in by `erpc/http_server_hedge_test.go:RetryImmediateWhenPrimaryFailsBeforeHedge` and `RetryWaitsForHedgeWhenHedgeIsRunning` tests.

**Scope restrictions.** Cache-connector failsafe (`CacheFailsafeConfig`) disallows `Hedge.Quantile > 0` because there are no per-method quantile trackers on cache reads (enforced by validation). Circuit breakers are not allowed at network scope.

## L3 — Exhaustive reference (agent view)

### Config fields

All fields are under `networks[].failsafe[].hedge` (or `upstreams[].failsafe[].hedge`, or project-level defaults). The `HedgePolicyConfig` struct is at `common/config.go:L1489-L1492`.

#### `HedgePolicyConfig`

| # | YAML path | Type | Default | Behavior |
|---|-----------|------|---------|----------|
| 1 | `hedge.delay` | `Duration \| AdaptiveDuration` | None (required when hedge is configured); after `SetDefaults`, `min=100ms`, `max=999s` are auto-populated if zero (`common/defaults.go:L2315-L2340`) | The inter-attempt delay. Scalar (`"200ms"`, `200`) sets `base` only; object `{base, quantile, min, max}` enables adaptive mode. **Static mode** (`quantile==0`): uses `base` exactly, ignoring `min`/`max`. **Adaptive mode** (`quantile>0`): resolves per-method latency quantile from the network's `QuantileTracker`, adds `base`, clamps to `[min, max]`. Cold-start (no data): falls back to `min`. Source: `common/config.go:L1490`, `common/adaptive_duration.go:L83-L109` |
| 2 | `hedge.delay.base` | `Duration` | `0` | Static addend. When `quantile==0`, this is the entire delay. When `quantile>0`, this is added to the quantile value before clamping. A scalar shorthand (`"100ms"`) sets this field exclusively. |
| 3 | `hedge.delay.quantile` | `float64` | `0` (static mode) | Percentile to query from the per-method `QuantileTracker`. Valid range `[0, 1]`. When `>0`, requires `base` or `max` to be set (enforced by `AdaptiveDuration.validate`, `common/adaptive_duration.go:L129`). Not allowed for cache-scope failsafe. Default network template: `0.7` (`common/defaults.go:L138`). |
| 4 | `hedge.delay.min` | `Duration` | `100ms` (auto-applied by `SetDefaults` when `0`, `common/defaults.go:L2324-L2326`) | Floor applied when `quantile>0`. **Rationale for 100ms floor:** the comment at `common/defaults.go:L2307-L2309` states this "prevents hedges firing before the primary has a real chance" — firing sooner than ~100ms would trigger a hedge before most upstreams have had meaningful time to respond, wasting upstream quota with little benefit. Cold-start fallback: when no quantile data exists, `min` is used as the resolved delay value (`common/adaptive_duration.go:L96-L98`). Static specs (`quantile==0`) ignore min entirely (`common/adaptive_duration_test.go:L196-L202`). |
| 5 | `hedge.delay.max` | `Duration` | `999s` (auto-applied by `SetDefaults` when `0`, `common/defaults.go:L2327-L2329`) | Ceiling applied when `quantile>0`. **Rationale for 999s ceiling:** the comment at `common/defaults.go:L2307-L2309` describes this as "effectively unbounded but defensive" — 999s is large enough to never realistically constrain a legitimate quantile value while still preventing a theoretically infinite or corrupt quantile reading from deferring the hedge indefinitely. Prevents a runaway quantile (e.g., a very slow method with multi-minute tail latency) from setting a hedge delay so large it never fires in practice. |
| 6 | `hedge.maxCount` | `int` | **`1`** (hard default from `SetDefaults` when `0` and no parent default, `common/defaults.go:L2331-L2337`); **NOTE:** the built-in system template sets `maxCount=2` (`common/defaults.go:L139`) — this template is only applied when eRPC auto-generates a project config (no `projects` block in YAML). **These are different defaults.** The `SetDefaults` value of `1` is what a manually-configured hedge with no explicit `maxCount` gets. The system template value of `2` is what applies to auto-generated "empty project" configurations. Users writing their own `networks[].failsafe[].hedge` block get `maxCount=1` unless they set `maxCount: 2` explicitly or inherit from a parent default. | Maximum number of *additional* (hedge) attempts beyond the primary. `maxCount=1` means primary + 1 hedge = 2 total concurrent requests. `maxCount=0` disables hedging (treated as "no hedge" by `HasHedge()`, `erpc/network_executor.go:L126-L131`). Negative values are treated as `0` inside `RunHedged` (`failsafe/hedge.go:L107-L109`). |

#### Legacy flat siblings (backward compatible)

Both YAML and JSON unmarshalers accept three additional sibling fields at the `hedge` level that fold into `hedge.delay.*` sub-fields at decode time. These fields exist for backward compatibility with configs written before the `delay` object form was introduced. New configs should use `delay: {base, quantile, min, max}` or the scalar shorthand `delay: "100ms"`.

The fold logic is implemented in `applyLegacySiblings` (`common/config.go:L1555-L1571`). The merge rules are:

- A non-zero legacy sibling only writes to the corresponding `delay.*` sub-field when that sub-field is **currently zero** (i.e., not already set by the `delay` object). This means the `delay` object form always takes precedence for any sub-field it specifies; legacy siblings only fill sub-fields that the object form left at zero.
- When at least one of the three legacy siblings is non-zero, `applyLegacySiblings` ensures `c.Delay` is non-nil, allocating an empty `AdaptiveDuration{}` if needed. This allows a bare `quantile: 0.7` (with no `delay` key at all) to produce a valid `*AdaptiveDuration{Quantile: 0.7}`.

| # | Legacy YAML/JSON key | Type | Maps to | Behavior |
|---|----------------------|------|---------|----------|
| L1 | `hedge.quantile` | `float64` | `hedge.delay.quantile` | Sets the percentile for adaptive delay. Equivalent to `delay.quantile`. Folded by `applyLegacySiblings` when `delay.quantile == 0`. Example: `quantile: 0.95` with no `delay` object produces `delay.quantile=0.95`. Locked in by `TestHedgePolicyConfig_LegacyYAML/"quantile-only (no base) via legacy form"` (`common/adaptive_duration_compat_test.go:L157-L163`). Source: `common/config.go:L1511, L1562-L1563`. |
| L2 | `hedge.minDelay` | `Duration` (YAML string or int ms; JSON raw message parsed via `parseJSONDuration`) | `hedge.delay.min` | Sets the floor for adaptive delay. Equivalent to `delay.min`. Folded by `applyLegacySiblings` when `delay.min == 0`. YAML example: `minDelay: "500ms"`. JSON example: `"minDelay": "500ms"` or `"minDelay": 500`. JSON errors surface as `"hedge.minDelay: <parse error>"` (`common/config.go:L1541-L1544`). Locked in by `TestHedgePolicyConfig_LegacyYAML/"legacy flat with quantile + bounds"` (`common/adaptive_duration_compat_test.go:L122-L136`). Source: `common/config.go:L1512, L1565-L1566`. |
| L3 | `hedge.maxDelay` | `Duration` (YAML string or int ms; JSON raw message parsed via `parseJSONDuration`) | `hedge.delay.max` | Sets the ceiling for adaptive delay. Equivalent to `delay.max`. Folded by `applyLegacySiblings` when `delay.max == 0`. JSON errors surface as `"hedge.maxDelay: <parse error>"` (`common/config.go:L1545-L1548`). Locked in by the same test case. Source: `common/config.go:L1513, L1568-L1570`. |

**Mixed use (object + legacy siblings):** When `delay` is given as an object AND a legacy sibling is present, only the sub-fields left unset by the object form are filled by the legacy sibling. Example (from `TestTimeoutPolicyConfig_LegacyYAML/"object form takes precedence; legacy siblings ignored when set"`, analogous for hedge):

```yaml
# delay.base and delay.quantile set by object; minDelay fills delay.min because object left it zero
delay:
  base: 100ms
  quantile: 0.95
minDelay: 50ms   # → delay.min = 50ms (object did not set min)
quantile: 0.50   # → IGNORED (delay.quantile already 0.95 from object)
```

Source: `common/config.go:L1507-L1571`, `common/adaptive_duration_compat_test.go:L102-L175`.

### Behaviors & algorithms

**`RunHedged` algorithm (pseudocode):**
```
channel = buffered(maxHedges+1)
fire(0)  // primary
pending = 1
arm timer for delayFn(1)

loop while pending > 0 OR timer armed:
  select:
    parentCtx.Done() → drain pending, return ctx.Err()
    timer fires:
      fire(fired); pending++; arm timer for delayFn(fired)
    result received:
      pending--
      if winnerSet: release result, continue
      if keep(r, err): set winner, cancelAll(), release timer, continue draining
      else: save as lastResult
        if pending==0 && !timer: return lastResult

return winner or lastResult or zero
```

**Delay function invariant.** `delayFn` is called with the hedge index (1-based) for each hedge fire. In practice, `erpc/network_executor.go` uses the same `AdaptiveDuration.ResolveForRequest(req)` for every index — delay is per-method, not per-attempt-index. The index parameter exists for extensibility (`erpc/network_executor.go:L542-L544`).

**Non-kept errors keep racing.** When `keep` returns `false` for an error, `RunHedged` does NOT release the result immediately; it tracks it as `lastResult`. This means the final fallback result is the last (not first) unkept result (`failsafe/hedge.go:L243-L253`).

**Sibling context.** All goroutines share `siblingCtx` derived from `context.WithCancel(parentCtx)`. Calling `cancelAll()` on winner-selection propagates through `siblingCtx` to all still-running goroutines. Goroutines detect this via `inner(siblingCtx)` returning a context-canceled error, which the upstream sweep loop surfaces as `ErrEndpointRequestCanceled` (`erpc/networks.go:L1302`).

**Hedge fires even after primary error.** The race continues if no winner was kept after a transient primary failure. The hedge timer arms regardless of whether the primary has already returned with an error (`failsafe/hedge.go:L211-L219`, comment at L216). This ensures recovery attempts fire even on fast-failing primaries—locked in by `HedgeDoesNotCancelOnFailuresWhenRunning` test.

**Winner counter.** `MetricNetworkHedgeWinnerTotal` is incremented inside the `keep` closure (deferred to run after `kept` is set), capturing which upstream won the hedge race. This fires for EVERY kept result, including the primary winning without any hedge ever firing (`erpc/network_executor.go:L553-L570`).

**Attempt counting.** The primary attempt does NOT increment `NetworkHedges`. Only `OnFire` (idx > 0) does. Both `NetworkAttempts` and `NetworkHedges` are incremented per hedge fire, so `X-ERPC-Hedges` header accurately counts additional (non-primary) fire events (`erpc/network_executor.go:L631-L641`).

### Observability

| Metric | Type | Labels | When fired | Source |
|--------|------|--------|------------|--------|
| `erpc_network_hedged_request_total` | counter | project, network, upstream, category, attempt, finality, user, agent_name | Each time a hedge request fires (primary fires are also counted when `hedges > 0` at that point) | `erpc/networks.go:L1288`, `telemetry/metrics.go:L425` |
| `erpc_network_hedge_discards_total` | counter | project, network, upstream, category, attempt, hedge, finality, user, agent_name | Hedge goroutine's context was cancelled by a winner (wasted work) | `erpc/networks.go:L1897`, `telemetry/metrics.go:L431` |
| `erpc_network_hedge_winner_total` | counter | project, network, upstream, category, finality | Each hedge race where a kept result was returned; labels identify which upstream won | `erpc/network_executor.go:L564`, `telemetry/metrics.go:L476` |
| `erpc_network_hedge_delay_seconds` | histogram | project, network, category, finality | **DORMANT** — registered with buckets `[0.01, 0.03, 0.05, 0.2, 0.3, 0.5, 0.7, 1, 3]` but no production `Observe` call exists (only touched by `telemetry/labeled_histogram_test.go:L151`) | `telemetry/metrics.go:L813` |
| `erpc_upstream_selection_total{reason="hedge"}` | counter | project, network, upstream, category, reason, finality | Upstream was selected for a hedge attempt | `upstream/upstream.go:L561`, `telemetry/metrics.go:L447` |
| `erpc_upstream_attempt_outcome_total{is_hedge="true"}` | counter | project, network, upstream, category, outcome, is_hedge, is_retry, finality | Hedge attempt terminal outcome | `upstream/upstream.go:L551`, `telemetry/metrics.go:L457` |

No dedicated trace spans for hedge; sibling goroutines share the parent span context.

### Edge cases & gotchas

1. **Static base ignores min/max.** When `quantile==0`, `AdaptiveDuration.Resolve` returns `base` unchanged regardless of configured `min`/`max` values. A `delay: 5ms` that fires before the default `min=100ms` WILL fire at 5ms. Min/max only constrain adaptive (quantile-driven) delays. Source: `common/adaptive_duration.go:L88-L89`, `common/adaptive_duration_test.go:L196-L202`.

2. **Cold start falls to min, not base.** When `quantile>0` and no latency data exists yet (QuantileTracker returns 0), `AdaptiveDuration.Resolve` returns `Min` (not `Base`). If `Min` is `0`, the result is `0`, meaning hedges fire immediately. `SetDefaults` auto-populates `min=100ms` to prevent this, but a manually-zero `min` on a quantile-mode spec fires instantly (`common/adaptive_duration.go:L96-L98`, `common/defaults.go:L2324-L2326`).

3. **maxCount=0 disables hedging silently.** `HasHedge()` returns `false` when `maxCount <= 0`. A config with `hedge: {delay: "100ms"}` and no explicit `maxCount` gets `maxCount=1` from `SetDefaults`, which IS enabled. But passing `maxCount: 0` explicitly disables it. Check: `erpc/network_executor.go:L126-L131`.

4. **Emptyish results do not cancel siblings (except emptyResultAccept methods).** A fast `{"result": null}` from one hedge leg does NOT win the race for methods like `eth_getBlockByNumber`, `eth_getTransactionByHash`, `eth_getTransactionReceipt` (not in `emptyResultAccept`). This prevents tip-lag upstreams from cancelling siblings that have the real data. For methods in `emptyResultAccept` (e.g. `eth_getLogs`, `eth_call`), empty IS kept. Source: `erpc/network_executor.go:L591-L622`, comment at L604.

5. **Non-retryable errors win immediately.** A deterministic error (e.g., `eth_call` returning execution-reverted) from ANY hedge leg is `keep=true` (`!IsRetryableTowardNetwork(err)=true`), causing it to win the race and cancel siblings. This mirrors the upstream-sweep behavior where client errors short-circuit immediately.

6. **Hedge fires continue even after all primaries return errors.** The timer-driven hedge schedule is independent of primary error/success. After a fast primary error, the hedge timer still fires its recovery attempt. Without this, a single transient primary failure would silently prevent hedges from running. Source: `failsafe/hedge.go:L216`, comment "hedge fires per its scheduled delay regardless…".

7. **Channel overflow protection.** The result channel is sized at `maxHedges+1`. The `default` branch in the goroutine's send handles the (theoretically impossible, given the cap) overflow case by releasing the result. Source: `failsafe/hedge.go:L127-L136`.

8. **Timer pool stale-tick safety.** `releaseHedgeTimer` calls `t.Stop()` + drains `t.C` with a non-blocking select before returning to pool. Without this, the next borrower could fire immediately on a stale tick from a previous lifecycle. Locked in by `TestHedgeTimerPool_BorrowReleaseLifecycle` and `TestHedgeTimerPool_ReleaseUnfiredTimer` (`failsafe/hedge_test.go:L18-L65`).

9. **Composite requests skip hedging.** Batch/composite requests bypass `runHedge` entirely (`erpc/network_executor.go:L524`). This is correct: batch fan-out has its own parallelism.

10. **Write methods skip hedging.** `evm.IsNonRetryableWriteMethod` excludes filter-creation and non-raw-tx methods. `eth_sendRawTransaction` is intentionally kept hedgeable for broadcast purposes (`architecture/evm/util.go:L7-L17`).

11. **ErrUpstreamHedgeCancelled is retryable, not fatal.** Cancelled hedge legs produce `ErrUpstreamHedgeCancelled` (U:yes N:yes). If the outer failsafe (e.g., an enclosing retry) re-evaluates after a complete hedge race failure, cancelled legs do not prevent a retry because `IsRetryableTowardNetwork` returns `true` for them. In `ErrUpstreamsExhausted.SummarizeCauses`, they appear in the `cancelled` bucket (not `other`), making log summaries like `"exhausted(cancelled=2)"` interpretable (`common/errors.go:L1000-L1002`).

12. **Single upstream + hedge = primary wins eventually.** When only one upstream is configured and a hedge fires, the hedge attempt calls `runUpstreamSweep` which finds no available upstream (the primary is consuming it), gets `ErrNoUpstreamsLeftToSelect`, `keep` returns `false`, and the race continues waiting for the primary. The primary eventually resolves, becoming the winner. Locked in by `SingleUpstreamHedgeContinuesPrimary` tests (`erpc/http_server_hedge_test.go:L1485-L1553`).

### Source map

- `failsafe/hedge.go` — Generic `RunHedged[R]` implementation, timer pool, `HedgeHooks`
- `failsafe/hedge_test.go` — Unit tests: timer pool lifecycle, hedge fires on slow primary, context cancellation, benchmark
- `common/config.go:L1489-L1571` — `HedgePolicyConfig` struct, YAML/JSON unmarshal (unified + legacy flat form), `applyLegacySiblings`
- `common/adaptive_duration.go` — `AdaptiveDuration` (Base/Quantile/Min/Max), `Resolve`, `ResolveForRequest`, `inheritFrom`, `validate`
- `common/adaptive_duration_test.go` — Unit tests for Resolve semantics (static, quantile, cold-start, min/max clamping)
- `common/adaptive_duration_compat_test.go` — Backward-compat tests for legacy flat sibling fields (`TestHedgePolicyConfig_LegacyYAML`), covering scalar-only, flat quantile+bounds, object form, quantile-only via legacy, and mixed object+legacy precedence rules
- `common/defaults.go:L2307-L2340` — `HedgePolicyConfig.SetDefaults`; constants `defaultHedgeMinDelay=100ms`, `defaultHedgeMaxDelay=999s`; system template at L137-L141
- `common/network.go:L51-L60` — `QuantileTracker` and `TrackedMetrics` interfaces
- `erpc/network_executor.go:L516-L645` — `runHedge` method; wires `AdaptiveDuration.ResolveForRequest` as `delayFn`; implements `keep`, `release`, `hooks`
- `erpc/network_executor.go:L141-L203` — `Run`, `runRetryHedge`; composition ordering
- `erpc/networks.go:L1284-L1306` — `MetricNetworkHedgedRequestTotal` increment + discard detection
- `erpc/networks.go:L1883-L1903` — `recordHedgeDiscard`
- `erpc/http_server_hedge_test.go` — Integration tests covering all hedge scenarios (fast primary wins, slow primary/hedge wins, failures, cancellation, single upstream, retry interaction)
- `architecture/evm/util.go:L7-L17` — `IsNonRetryableWriteMethod`
- `telemetry/metrics.go:L425-L435,L476-L479,L813-L818` — Metric declarations
- `common/errors.go:L1447-L1462` — `ErrUpstreamHedgeCancelled` / `ErrCodeUpstreamHedgeCancelled` struct, constructor, and error code
- `common/errors.go:L955-L1035` — `ErrUpstreamsExhausted.SummarizeCauses`; `cancelled` bucket for `ErrCodeUpstreamHedgeCancelled` at L1000-L1002
