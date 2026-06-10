# KB: Failsafe — Retry policy & backoff

> status: complete
> source-dirs: common/config.go (RetryPolicyConfig, FailsafeConfig, DirectiveDefaultsConfig, EvmNetworkConfig), common/defaults.go (SetDefaults, DefaultEmptyResultAccept, DefaultMarkEmptyAsErrorMethods), common/errors.go (IsRetryableTowardNetwork, IsRetryableTowardsUpstream, error type definitions), common/errors_retry_test.go, failsafe/backoff.go, erpc/network_executor.go, erpc/network_executor_delay_test.go, erpc/networks_registry.go, upstream/upstream_executor.go, architecture/evm/common.go, architecture/evm/hooks.go, telemetry/metrics.go, common/exec_state.go, erpc/networks_retry_missing_data_test.go

## L1 — Capability (CTO view)

eRPC implements a two-scope retry system — network-scope (rotate to a different upstream) and upstream-scope (retry the same upstream) — driven by independent `RetryPolicyConfig` objects per `FailsafeConfig` entry. The system classifies every error and empty response into one of several retryable reasons, then applies exponential-backoff-with-jitter for genuine errors and a block-time-relative delay (EMA block time × multiplier) for "data not yet available" conditions (missing trie nodes, empty point-lookups, block-unavailable preflight blocks). The `retryEmpty` per-request directive is the master gate: when off, neither scope retries empty/missing-data results. An `EmptyResultMaxAttempts` cap limits data-availability retries independently of the `MaxAttempts` limit that governs hard-error failover, preventing the retry loop from hammering upstreams waiting for a block that may not arrive for seconds.

## L2 — Mechanics (staff-engineer view)

**Two retry scopes, one config shape.** Both the network executor (`erpc/network_executor.go`) and the upstream executor (`upstream/upstream_executor.go`) consume the same `RetryPolicyConfig` struct, but their semantics differ:
- Network-scope retry (`networkExecutor.runRetry`): after all upstreams have been tried within one execution round and still produced no satisfying response, the retry loop fires another round. Each round is an upstream sweep (all upstreams, ordered by the selection policy). This is the level that handles "every upstream is seeing the same error right now" scenarios.
- Upstream-scope retry (`upstreamExecutor.runRetry`): retries the same upstream's HTTP transport for transient connectivity blips. The default upstream `MaxAttempts=1` disables it in practice; production configs typically set it to 2–3 only for write-safe idempotent operations.

**Execution flow (non-consensus, non-hedge case).** `networkExecutor.Run` → `runRetryHedge` → `runRetry` calls `hedged(ctx)` which is ultimately `runUpstreamSweep`. Within one sweep, every upstream is tried in priority order until success or all return errors. After the sweep returns `(resp, err)`, `shouldRetryWithReason` classifies the outcome:

1. `err == nil` → no retry (success).
2. `ErrEndpointExecutionException` with `retryableTowardNetwork: false` → no retry.
3. `ErrUpstreamBlockUnavailable` → retry reason `"block_unavailable"` (unless `EmptyResultMaxAttempts` cap reached).
4. `ErrEndpointMissingData` + `RetryEmpty == false` directive → no retry; otherwise reason `"missing_data"` (unless cap reached).
5. `IsRetryableTowardNetwork(err) == true` → reason `"retryable_error"`.
6. `resp.IsResultEmptyish()` + `RetryEmpty == true` directive + method NOT in `emptyResultAccept` → reason `"empty_result"` (unless cap reached).
7. `RetryPending == true` + tx-lookup method → reason `"pending_tx"` (unless cap reached).

**"Data not yet available" unified delay path.** When the retry reason is `block_unavailable`, `empty_result`, or `missing_data`, `computeDelay` takes a special path: it first asks the per-network `dynamicBlockUnavailableDelay()` closure, which computes `EMA_block_time × BlockUnavailableDelayMultiplier` (default multiplier 1.0). If the EMA hasn't warmed up yet (returns 0), it falls back to the static `EmptyResultDelay` configured in `RetryPolicyConfig`. For genuine errors (`retryable_error`, `pending_tx`) it uses the standard `ComputeBackoff` formula (`failsafe/backoff.go`).

**`emptyResultDelay` is a static fallback, NOT a floor.** The dynamic EMA-based delay is unconditionally preferred when the EMA is warm (returns > 0). `emptyResultDelay` only activates before the EMA has seen enough block data (cold start). Setting `emptyResultDelay` does NOT set a floor on the dynamic path — a warm EMA overrides any configured static value. `emptyResultDelay = 0` with a cold EMA means retries fire immediately with no delay. See `common/config.go:L1355-L1361` for the field comment and `erpc/network_executor.go:L497-L507` for the priority code.

**`emptyResultMaxAttempts` is a SEPARATE counter from `maxAttempts`.** `emptyResultMaxAttempts` caps the number of attempts (including the first try) that can be made for data-unavailability reasons (`empty_result`, `missing_data`, `block_unavailable`, `pending_tx`). `maxAttempts` caps the total number of network retry rounds for genuine errors. These two counters are independent: a request that gets a retryable error in round 1 then empty results in rounds 2–N will consume both budgets. The theoretical worst case is `maxAttempts` rounds × `emptyResultMaxAttempts` within each round's empty-result phase. With defaults (`maxAttempts=3`, `emptyResultMaxAttempts=2`) the system could attempt up to 6 total calls if each error-retry round also triggers one empty-result retry. In practice the single shared `dataUnavailableCapReached` counter prevents more than `emptyResultMaxAttempts` empty-type retries across ALL rounds (`erpc/network_executor.go:L358-L377`).

**Failsafe entry merge algorithm (network scope).** When a network has its own `failsafe` array AND `networkDefaults.failsafe` is also set, `EvmNetworkConfig.SetDefaults` (or the analogous network config path at `common/defaults.go:L1793-L1832`) applies per-entry matching: for each entry `fs` in `n.Failsafe`, the code iterates `defaults.Failsafe` looking for a default entry that matches `(fs.MatchMethod, fs.MatchFinality)`. Method matching uses `WildcardMatch(dfs.MatchMethod, fs.MatchMethod)` — a wildcard in the defaults (`*`) matches any method in the network entry. Finality matching uses `MatchFinalities`. When a match is found, `fs.SetDefaults(matchedDefault)` is called so every unset sub-field (retry, hedge, timeout, etc.) inherits from the matched default entry. When NO match is found, `fs.SetDefaults(nil)` is called (system defaults only). A catch-all default (`matchMethod: "*"`) therefore acts as a universal base for all network-specific entries. If the network has NO `failsafe` entries but the defaults do, the entire defaults array is cloned into the network (`common/defaults.go:L1787-L1792`). For upstream-level merging the same logic applies at `common/defaults.go:L1621-L1672`.

**Failsafe entry merge algorithm (upstream scope).** Same logic as network scope but at `common/defaults.go:L1621-L1672`. When the upstream has its own `failsafe` entries: each entry is matched against the upstream defaults using the same `WildcardMatch` + `MatchFinalities` algorithm. One notable difference from network: if `defaultFs == nil` (no match in defaults), `fs.SetDefaults(nil)` is called, so the entry still gets system-level defaults for all unset fields. When the upstream has no entries but defaults do, entries are cloned from the defaults array verbatim then re-defaulted against themselves.

**`ErrFailsafeConfiguration` is startup-only.** This error (`common/errors.go:L1573-L1584`) is returned by `NewNetworkExecutor` and `NewUpstreamExecutor` during initialization, not at request time. Known triggers: (1) a network-scope failsafe entry has a `circuitBreaker` policy — circuit breakers are upstream-scope only (`erpc/network_executor.go:L68-L72`); (2) an upstream-scope failsafe entry has a `consensus` policy — consensus is network-scope only (`upstream/upstream_executor.go:L46-L53`). Any `ErrFailsafeConfiguration` will abort the eRPC startup before any request is served.

**Backoff formula.** `ComputeBackoff(cfg, attempt)` where `attempt` is 0-based (0 = first retry). If `BackoffFactor > 0` and `attempt > 0`: `delay = Delay × BackoffFactor^attempt`. Capped at `BackoffMaxDelay`. Then additive uniform jitter in `[0, Jitter)` using `rand.Int63n`. If `Delay == 0`, returns 0 immediately regardless of other params (`failsafe/backoff.go:L21-L48`).

**Empty-as-error conversion.** The EVM post-forward hook (`architecture/evm/common.go:upstreamPostForward_markUnexpectedEmpty`) converts `result: null` for methods in `MarkEmptyAsErrorMethods` into `ErrEndpointMissingData`. This fires only when `RetryEmpty == true` in the request's directives (checked inside the hook). The confidence guard (`emptyResultBeyondConfidence`) stops the conversion if the requested block number is above the latest known head (for `emptyResultConfidence: blockHead`, the default) or finalized head (for `finalizedBlock`), returning the truthful null instead.

**`IsRetryableTowardNetwork` algorithm.** Traverses the error chain looking for an opt-out:
1. Empty `ErrUpstreamsExhausted` (no cause) → false (terminal).
2. Multi-error cause (e.g. `errors.Join`): if ANY child is retryable → true; if ALL are non-retryable → false.
3. Single-cause chain: walks looking for `base.Details["retryableTowardNetwork"] == false` → false.
4. Default → true.

Key implications: `ErrEndpointExecutionException` (hard-coded `retryableTowardNetwork: false`) always returns false unless overridden; `ErrEndpointServerSideException` has no flag set → returns true; `ErrEndpointMissingData` has no flag → returns true (letting the MissingData-specific path in `shouldRetryWithReason` handle directive gating separately).

**`IsRetryableTowardsUpstream` algorithm.** Blocklist of error codes: `ErrCodeFailsafeCircuitBreakerOpen`, `ErrCodeUpstreamRequestSkipped`, `ErrCodeUpstreamMethodIgnored`, `ErrCodeEndpointUnsupported`, `ErrCodeEndpointBillingIssue`, `ErrCodeJsonRpcRequestUnmarshal`, `ErrCodeEndpointExecutionException`, `ErrCodeEndpointUnauthorized`, `ErrCodeEndpointRequestTooLarge`, `ErrCodeEndpointContentValidation`. Also blocks when `IsCapacityIssue` returns true (rate-limits). Missing-data errors respect `RetryEmpty == false` at this level too.

**First-informative-error capture.** The network retry loop captures the first non-degenerate error in `firstInformativeErr`. Later retry rounds can degenerate into bare `ErrNoUpstreamsLeftToSelect` (all previously-tried upstreams marked consumed). On final wrap, the richer first error is surfaced rather than the degenerate one (`erpc/network_executor.go:L302-L309`).

**BestResponse preference.** While retrying, the loop tracks `bestResp` — the last non-nil response seen. When the final attempt fails with an error but `bestResp != nil` (i.e. a partial or emptyish response was available), the response is returned with `nil` error, preferring a result over an error (`erpc/network_executor.go:L264-L267`, `L342-L348`).

**`eth_sendRawTransaction` special-case.** If retries exhausted and the final cause is `ErrEndpointExecutionException` for `eth_sendRawTransaction`, the raw execution error is returned unwrapped (not in `ErrFailsafeRetryExceeded`) because the "execution reverted" IS the answer — the tx was broadcast and reverted (`erpc/network_executor.go:L279-L283`).

**Upstream-scope retry differences.** The upstream executor does NOT have data-unavailability logic in `computeDelay` — it always uses `ComputeBackoff`. It does not track `bestResp` for emptyish responses. Its `shouldRetry` is simpler: only error-based, no response-based retry, no reason string.

**Executor lifecycle per request.** `ExecState` (lazy-alloc) on `NormalizedRequest` accumulates `UpstreamAttempts`, `UpstreamRetries`, `NetworkAttempts`, `NetworkRetries` atomically. Network executor increments `NetworkRetries` at the start of each retry round (not the first attempt). The `Snapshot()` output feeds span attributes and log labels.

## L3 — Exhaustive reference (agent view)

### Config fields

**Top-level container.** `RetryPolicyConfig` lives at `networks[].failsafe[].retry` or `upstreamDefaults.failsafe[].retry`. `FailsafeConfig` wraps the `retry` policy alongside its `matchMethod` and `matchFinality` envelope selectors; the first matching entry (by `SelectExecutor` 4-tier priority) wins. Multiple entries in the `failsafe` array are allowed for per-method tuning. `NetworkFailsafeConfig` and `UpstreamFailsafeConfig` are type aliases for `FailsafeConfig` (`common/config.go:L1289-L1298`).

#### Envelope selector fields (FailsafeConfig level)

These fields sit one level ABOVE `retry` in the config hierarchy (`networks[].failsafe[i].matchMethod`, not `networks[].failsafe[i].retry.matchMethod`). They are documented here so users reading this policy KB understand the full selector envelope.

| Field | YAML path | Type | Default | Behavior | Citation |
|---|---|---|---|---|---|
| `matchMethod` | `*.failsafe[].matchMethod` | string | `"*"` — `FailsafeConfig.SetDefaults` sets `"*"` when the field is empty and the parent default also has no method set. Inherited from a matched parent default entry if that entry has a non-empty `matchMethod`. | WildcardMatch pattern against the request JSON-RPC method. `"*"` matches all methods. **`Validate()` rejects the empty string** — after `SetDefaults`, an empty `matchMethod` is a configuration error: `"failsafe.matchMethod cannot be empty, use '*' to match any method"`. For backward-compatibility, legacy single-object `failsafe` YAML (pre-array format) forces `matchMethod = "*"` automatically during YAML parse. | `common/config.go:L1280`, `common/defaults.go:L2131-L2137`, `common/validation.go:L985-L988` |
| `matchFinality` | `*.failsafe[].matchFinality` | []DataFinalityState | `nil` / `[]` — empty = matches ANY finality tier | List of finality states this entry applies to. Valid values: `finalized`, `unfinalized`, `realtime`, `unknown` (`common/data.go:L15-L26`). Empty list (the default) acts as a catch-all finality wildcard. Matched by `slices.Contains(fList, requestFinality)` — all listed states are eligible, no glob syntax inside the list. **Set-overlap semantics for defaults merging**: during `SetDefaults` inheritance, two `matchFinality` arrays match if either is empty OR they share at least one element (`MatchFinalities` at `common/defaults.go:L17-L34`). At request-dispatch time (within `SelectExecutor`), matching uses `slices.Contains` directly — the empty-list wildcard still applies. | `common/config.go:L1281`, `common/match.go:L38-L41`, `common/defaults.go:L17-L34` |

**`SelectExecutor` 4-tier priority** (controls which `failsafe` entry is selected per request):
1. Specific method + specific finality (highest priority)
2. Specific method, any finality
3. Wildcard method (`"*"`), specific finality
4. Wildcard method, any finality — catch-all (lowest priority)

Within each tier, the first matching entry in config order wins. See `common/match.go:L18-L78`.

#### RetryPolicyConfig fields (`failsafe[].retry`)

| Field | YAML path (relative to `failsafe[i].retry`) | Type | Default | Behavior |
|---|---|---|---|---|
| `maxAttempts` | `maxAttempts` | int | `3` (via `SetDefaults`) but `5` in the injected network default and `1` in the upstream default (`common/defaults.go:L127-L157`, `L2226-L2232`) | Total execution rounds including the first. When `1`, no retries fire. At the network scope the out-of-box defaults inject `maxAttempts: 5`. |
| `delay` | `delay` | Duration | `0` (`common/defaults.go:L2247-L2252`) | Base delay before the first retry. Accepts Go duration string or bare integer (milliseconds). When 0, no base delay. |
| `backoffFactor` | `backoffFactor` | float32 | `1.2` (`common/defaults.go:L2233-L2238`) | Exponential multiplier applied to `delay` on each successive attempt (0-indexed). `1.0` = no growth. Must be > 0 or `Validate()` returns an error (`common/validation.go:L1021-L1022`). |
| `backoffMaxDelay` | `backoffMaxDelay` | Duration | `3s` (`common/defaults.go:L2240-L2244`) | Cap on the computed backoff before jitter is added. Must be non-zero after defaults (the validation checks `BackoffMaxDelay == 0` and returns an error, `common/validation.go:L1024-L1025`). |
| `jitter` | `jitter` | Duration | `0` (`common/defaults.go:L2254-L2259`) | Uniform random additive jitter in `[0, jitter)`. Jitter uses `math/rand` (not crypto) (`failsafe/backoff.go:L44`). |
| `emptyResultAccept` | `emptyResultAccept` | []string | `DefaultEmptyResultAccept()` — eth_getLogs, trace_filter, arbtrace_filter, eth_call, eth_getBalance, eth_getCode, eth_getStorageAt, eth_getTransactionCount (`common/defaults.go:L2013-L2026`) | Methods for which an empty/null result is accepted immediately and NOT retried. Overrides the response-based retry for those methods. Both network and upstream executors read this; hedge uses it to decide whether to accept an emptyish winner. |
| `emptyResultIgnore` | `emptyResultIgnore` | []string | `nil` (deprecated alias; YAML-only, never set programmatically) | **DEPRECATED alias for `emptyResultAccept`.** Type: `[]string`. Default: `nil`. Migration rule (at `SetDefaults` time, `common/defaults.go:L2261-L2263`): **only migrated when `emptyResultAccept` is nil** — `if r.EmptyResultAccept == nil && r.EmptyResultIgnore != nil { r.EmptyResultAccept = r.EmptyResultIgnore }`. **If both `emptyResultIgnore` AND `emptyResultAccept` are set, `emptyResultIgnore` is silently ignored entirely** (the condition `emptyResultAccept == nil` is false, so the migration never runs). The field is then left in place — it is NOT cleared after migration, unlike `blockUnavailableDelay`. The key upgrade confusion: operators who upgrade to `emptyResultAccept` but leave an old `emptyResultIgnore` in their config will have `emptyResultIgnore` silently ignored. After migration, the in-memory `EmptyResultIgnore` still holds the old value but all runtime code reads only `EmptyResultAccept`. |
| `emptyResultMaxAttempts` | `emptyResultMaxAttempts` | int | `2` (= `DefaultEmptyResultMaxAttempts`) (`common/defaults.go:L1984`, `L2290-L2295`) | Shared cap for all "data not yet available" retry reasons: `block_unavailable`, `missing_data`, `empty_result`, `pending_tx`. **Independent from `maxAttempts`** (a separate counter). With default value 2, one original attempt + one empty-result retry fires. The two budgets do NOT multiply: a single shared `dataUnavailableAttemptsCount` counter gates all empty-type retries across ALL network retry rounds. However, because `maxAttempts` governs outer retry rounds and `emptyResultMaxAttempts` governs per-trigger behavior within those rounds for empty classification, a request that alternates between genuine errors (consuming `maxAttempts`) and empty results (consuming `emptyResultMaxAttempts`) can in theory exhaust both counters across different rounds. See `erpc/network_executor.go:L358-L377`. |
| `emptyResultDelay` | `emptyResultDelay` | Duration | `0` (no hardcoded fallback; inherits from parent defaults only) (`common/defaults.go:L2298-L2302`; `common/config.go:L1355-L1361`) | Fixed fallback delay before a data-availability retry **only when the EMA block time has not yet warmed up**. When the EMA is warm (returns > 0), the dynamic delay `EMA_block_time × BlockUnavailableDelayMultiplier` is used unconditionally and `emptyResultDelay` is never consulted. When `emptyResultDelay = 0` AND the EMA is cold, retries for empty/missing/block-unavailable fire with zero delay. `0` means "use dynamic" — not "no delay" — because the field is defined as a static fallback, not the primary mechanism. Production configs targeting Ethereum should set this to ~1s for cold-start safety. |
| `blockUnavailableDelay` | `blockUnavailableDelay` | Duration | `nil` / zero Duration. **DEPRECATED. YAML-only: `json:"-"` so it never appears in generated TS types or JSON API.** (`common/config.go:L1363-L1369`) | Old field with the same purpose as `emptyResultDelay` (fixed fallback delay for data-not-available retries). **Migration rule** (`common/defaults.go:L2265-L2274`): if `BlockUnavailableDelay > 0` at `SetDefaults` time, its value is merged into `EmptyResultDelay` **only when `EmptyResultDelay == 0`** (the old value is not overwritten if the user also sets `emptyResultDelay`). After migration, `BlockUnavailableDelay` is **unconditionally cleared to 0** (unlike `emptyResultIgnore`, this field IS zeroed after migration). A `log.Warn` is also emitted — see Observability section for the exact message. The field is never read at runtime; only `emptyResultDelay` is. |

**Related fields not in `RetryPolicyConfig`.**

| Field | YAML path | Type | Default | Behavior |
|---|---|---|---|---|
| `retryEmpty` | `networks[].directiveDefaults.retryEmpty` | *bool | `nil` (unset → directives default to `false`) | Per-request master gate. When `false`, neither scope retries missing-data, empty-result, or block-unavailable. Can be overridden per-request via `X-ERPC-Retry-Empty: true` header or `?retry-empty=true` query param. |
| `retryPending` | `networks[].directiveDefaults.retryPending` | *bool | `nil` | Retries tx-lookup methods (`eth_getTransactionReceipt`, etc.) until `emptyResultMaxAttempts` is reached. |
| `blockUnavailableDelayMultiplier` | `networks[].evm.blockUnavailableDelayMultiplier` | *float64 | `1.0` (`common/defaults.go:L1978`) | Scales the EMA block time to derive dynamic delay: `delay = blockTime × multiplier`. Overrides the shape of the warm-path delay. |
| `emptyResultConfidence` | `networks[].evm.emptyResultConfidence` | AvailbilityConfidence | `blockHead` (`common/defaults.go:L2075-L2079`) | Controls which head the empty-result confidence guard compares against. `blockHead` (default): only retry empties for blocks at or below the latest head. `finalizedBlock`: stricter — only retry for finalized blocks. |
| `markEmptyAsErrorMethods` | `networks[].evm.markEmptyAsErrorMethods` | []string | `DefaultMarkEmptyAsErrorMethods()` — eth_blockNumber, eth_getBlockByNumber, eth_getTransactionByHash, eth_getTransactionByBlockHashAndIndex, eth_getTransactionByBlockNumberAndIndex, eth_getUncleByBlockHashAndIndex, eth_getUncleByBlockNumberAndIndex, debug_traceTransaction, trace_transaction, trace_block, trace_get (`common/defaults.go:L2044-L2058`) | Methods whose `null` result is converted to `ErrEndpointMissingData` in the post-forward hook. Requires `RetryEmpty == true` in the request directives to actually convert; otherwise the null is passed through unchanged. |

#### AdaptiveDuration — reusable type (cross-reference)

`AdaptiveDuration` is a shared sub-type used in sibling policies (hedge.delay, timeout.duration, consensus.maxWaitOnResult, consensus.maxWaitOnEmpty). The retry policy does NOT use `AdaptiveDuration` — its delay fields are plain `Duration` scalars. This section exists so readers navigating from a retry context can understand the type they'll find when reading KB 11 (hedge) or KB 12 (timeout).

**Definition** (`common/adaptive_duration.go:L33-L65`):

```yaml
# Scalar shorthand: populates Base only
delay: 500ms

# Object form: all fields optional
delay:
  base: 100ms      # static additive component
  quantile: 0.9    # p90 of per-method response latency
  min: 50ms        # floor (applies when quantile > 0)
  max: 2s          # ceiling (applies when quantile > 0)
```

| Sub-field | Type | Default | Behavior |
|---|---|---|---|
| `base` | Duration | `0` | Static duration component. Always added to adaptive component. |
| `quantile` | float64 | `0` (disabled) | When > 0, the adaptive component is `qt.GetQuantile(quantile)` from the per-method latency tracker. Must be in `[0, 1]`. |
| `min` | Duration | `0` (no floor) | Floor on `Base + adaptive` when `quantile > 0`. Also used as the cold-start value when quantile data is not yet available (no observations). |
| `max` | Duration | `0` (no ceiling) | Ceiling on `Base + adaptive` when `quantile > 0`. |

**Resolution algorithm** (`common/adaptive_duration.go:L83-L109`):
1. If `Quantile <= 0`: return `Base` (pure static).
2. If `Quantile > 0`: `adaptive = qt.GetQuantile(Quantile)`. If cold (adaptive ≤ 0): `adaptive = Min`.
3. `v = Base + adaptive`.
4. Clamp: if `Min > 0 && v < Min` → `v = Min`; if `Max > 0 && v > Max` → `v = Max`.
5. Return `v`.

**Scalar shorthand parsing.** When the YAML/JSON value is a plain string like `"5s"` or a bare number (treated as milliseconds), `UnmarshalYAML`/`UnmarshalJSON` sets `Base` only — `Quantile`, `Min`, and `Max` remain zero (`common/adaptive_duration.go:L164-L188`).

**Validation** (`common/adaptive_duration.go:L122-L138`):
- `quantile` must be in `[0, 1]`.
- When `quantile > 0`, either `base` or `max` must be set (otherwise the computed value has no useful bound).
- When all of `base`, `quantile`, `min`, `max` are zero, the spec is invalid — callers receive an error via `validate(field)`.
- `min <= max` when both are set.

**Where used** (retry does NOT use `AdaptiveDuration`):
- `hedge.delay` — `HedgePolicyConfig.Delay *AdaptiveDuration` (`common/config.go`; see KB 11).
- `timeout.duration` — `TimeoutPolicyConfig.Duration *AdaptiveDuration` (see KB 12).
- `consensus.maxWaitOnResult`, `consensus.maxWaitOnEmpty` — `ConsensusPolicyConfig` (see KB 13).

### Behaviors & algorithms

**Backoff math in detail** (`failsafe/backoff.go:L21-L48`):
```
if Delay <= 0: return 0
d = Delay
if BackoffFactor > 0 && attempt > 0:
    factor = 1.0
    for i in 0..attempt: factor *= BackoffFactor
    d = Delay × factor
if BackoffMaxDelay > 0 && d > BackoffMaxDelay:
    d = BackoffMaxDelay
if Jitter > 0:
    d += rand.Int63n(Jitter)   // uniform, non-crypto
return d
```
Attempt is 0-indexed from the retry loop: first retry = attempt 0, so `BackoffFactor^0 = 1`, i.e. the first retry always uses the base `Delay`. Second retry = attempt 1, uses `Delay × BackoffFactor`.

**Data-unavailability delay priority** (`erpc/network_executor.go:L481-L513`):
1. `isBlockUnavailable || isEmptyResult` (where `isEmptyResult` covers both empty response AND `ErrEndpointMissingData`)?
2. If yes: try `dynamicBlockUnavailableDelay()` → EMA block time × multiplier.
3. If dynamic returns 0 (EMA not warmed): try `cfg.EmptyResultDelay`.
4. If still 0: no delay.
5. For other reasons: `ComputeBackoff(cfg, attempt)`.

`SleepCtx` respects context cancellation: returns `ctx.Err()` immediately if the context fires during sleep (`failsafe/backoff.go:L53-L65`).

**Network retry loop exit conditions** (`erpc/network_executor.go:L227-L349`):
- Context cancelled/timed-out: returns `context.Cause(ctx)` or `ctx.Err()` immediately, at both the loop pre-check and post-attempt check.
- `attempt + 1 >= maxAttempts` and `shouldRetryWithReason` returns `""`: returns wrapped `ErrFailsafeRetryExceeded(ScopeNetwork, lastErr)` with `durationMs` in Details.
- `shouldRetryWithReason` returns reason: emits `MetricNetworkRetryAttemptTotal`, optionally emits `MetricNetworkDataUnavailableWaitSeconds`, sleeps delay, continues loop.

**Upstream retry loop exit conditions** (`upstream/upstream_executor.go:L186-L224`):
- `attempt + 1 >= maxAttempts` or `shouldRetry == false`: returns `ErrFailsafeRetryExceeded(ScopeUpstream, lastErr)` if retries happened; otherwise returns the raw `(resp, err)`.
- Context cancellation mid-sleep: returns `lastResp, serr`.

**`IsRetryableTowardNetwork` full truth table** (as exercised by `common/errors_retry_test.go`):

| Error type | Retryable toward network? |
|---|---|
| `ErrUpstreamsExhausted` with NO cause | false (terminal) |
| `ErrUpstreamsExhausted` wrapping ALL `ErrEndpointMissingData` | true |
| `ErrUpstreamsExhausted` wrapping mixed (timeout + missing data) | true |
| `ErrUpstreamsExhausted` wrapping ALL `ErrEndpointServerSideException` | true |
| `ErrUpstreamsExhausted` wrapping ALL `ErrEndpointExecutionException` | false |
| `ErrEndpointExecutionException` (standalone) | false |
| `ErrEndpointServerSideException` (standalone) | true |
| `ErrEndpointMissingData` (standalone) | true |
| `ErrEndpointRequestTimeout` (standalone) | true |
| `ErrEndpointClientSideException` (standalone) | true (no explicit flag) |

**`IsRetryableTowardsUpstream` blocklist** (`common/errors.go:L2455-L2497`):
Blocked (returns false): `ErrCodeFailsafeCircuitBreakerOpen`, `ErrCodeUpstreamRequestSkipped`, `ErrCodeUpstreamMethodIgnored`, `ErrCodeEndpointUnsupported`, `ErrCodeEndpointBillingIssue`, `ErrCodeJsonRpcRequestUnmarshal`, `ErrCodeEndpointExecutionException`, `ErrCodeEndpointUnauthorized`, `ErrCodeEndpointRequestTooLarge`, `ErrCodeEndpointContentValidation`, and any `IsCapacityIssue` (rate-limits: `ErrCodeProjectRateLimitRuleExceeded`, `ErrCodeNetworkRateLimitRuleExceeded`, `ErrCodeUpstreamRateLimitRuleExceeded`, `ErrCodeAuthRateLimitRuleExceeded`, `ErrCodeEndpointCapacityExceeded`).

**Empty-result retry via `markEmptyAsErrorMethods`** (the full pipeline for `eth_getBlockByNumber` with `retryEmpty: true`):
1. Upstream returns `{"result": null}` → `HandleUpstreamPostForward` called.
2. `isMethodInMarkEmptyList` returns true.
3. `upstreamPostForward_markUnexpectedEmpty` called: checks `RetryEmpty == true` in directives; checks `emptyResultBeyondConfidence` (confidence guard — if block > latest head, returns null truthfully).
4. Converts to `ErrEndpointMissingData`.
5. Network sweep loop sees `ErrEndpointMissingData` → marks it in `ErrorsByUpstream` → tries next upstream.
6. If all upstreams return `ErrEndpointMissingData` → `ErrUpstreamsExhausted` wrapping all missing-data → `IsRetryableTowardNetwork` returns true → network retry fires after `emptyResultDelay`/dynamic delay.

**BestResponse preference on retry exhaustion** (`erpc/network_executor.go:L264-L267`):
When the final attempt has `err != nil` but `bestResp != nil` (any upstream returned a non-nil response, even emptyish, in a prior round), `bestResp` is returned with `nil` error. This avoids surfacing a server error when an empty-but-valid response was seen.

**`emptyResultAccept` interaction with sweep vs. retry**. Within one sweep, when an upstream returns empty for a method in `emptyResultAccept` (e.g. `eth_call` returning `0x`), the response is accepted immediately — no `ErrEndpointMissingData` conversion, no further upstream tries. The retry layer never sees an empty that needs deciding; it receives a success response.

**Composite requests bypass retry.** Both `networkExecutor.shouldRetryWithReason` and `upstreamExecutor.shouldRetry` return `""` / `false` immediately for composite (batch) requests (`erpc/network_executor.go:L378-L380`, `upstream/upstream_executor.go:L230-L232`).

**Non-retryable write methods bypass upstream retry.** `evm.IsNonRetryableWriteMethod` gates the upstream-scope retry (`upstream/upstream_executor.go:L243-L246`). `eth_sendRawTransaction` still uses network-scope retry due to idempotency handling, but the upstream executor won't re-issue it.

### Observability

**Prometheus metrics:**

| Metric | Type | Labels | When fired | Source |
|---|---|---|---|---|
| `erpc_network_retry_attempt_total` | counter | project, network, category, reason, finality | Every network-scope retry attempt that fires; `reason` ∈ {empty_result, pending_tx, retryable_error, block_unavailable, missing_data} | `erpc/network_executor.go:L292-L299` |
| `erpc_network_data_unavailable_wait_seconds` | histogram | project, network, category, reason, finality | Wall-clock catch-up delay slept before a data-unavailability retry (only for reasons in `isDataUnavailableReason`: block_unavailable, empty_result, missing_data — NOT pending_tx); buckets: 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64 | `erpc/network_executor.go:L322-L337` |
| `erpc_upstream_attempt_outcome_total` | counter | project, network, upstream, category, outcome, is_hedge, is_retry, finality | Per physical upstream attempt; `is_retry="true"` when `snap.Retries > 0` at the upstream scope | `upstream/upstream.go:L551-L560` |
| `erpc_network_failed_request_total` | counter | project, network, category, attempt, error, severity, finality, user, agent_name | Final failed request after retries exhausted; `attempt` label holds total attempt count | `telemetry/metrics.go:L491` |

**OTel span attributes** (set on the span wrapping each upstream attempt, `common/exec_state.go:L279-L285`):
- `execution.attempts` — total physical upstream calls across all scopes
- `execution.retries` — total retry events (upstream + network scopes)
- `execution.hedges` — total hedge fires
- `execution.upstream_attempts`, `execution.upstream_retries`, `execution.upstream_hedges`
- `execution.network_attempts`, `execution.network_retries`, `execution.network_hedges`

**Error codes produced:**

| Error | When produced | Source |
|---|---|---|
| `ErrFailsafeConfiguration` | Startup only — invalid policy placement (circuit breaker at network scope, consensus at upstream scope). Server aborts before serving any request. | `common/errors.go:L1573-L1584`; `erpc/network_executor.go:L68-L72`; `upstream/upstream_executor.go:L46-L53` |
| `ErrFailsafeRetryExceeded` | After all retry rounds exhausted; wraps the last error with `ScopeNetwork` or `ScopeUpstream` and `durationMs` | `erpc/network_executor.go:L279-L283`, `upstream/upstream_executor.go:L199-L210` |

**Log messages:**
- `config: retry.blockUnavailableDelay is deprecated and has been merged into retry.emptyResultDelay; please update your config` — emitted as `log.Warn` at `RetryPolicyConfig.SetDefaults` time whenever `BlockUnavailableDelay > 0` is found in the loaded config, regardless of whether migration actually changes `EmptyResultDelay` (the warn fires even if `emptyResultDelay` was already set and the old value was discarded) (`common/defaults.go:L2272`). This is the warning users will see when grepping logs for `blockUnavailableDelay` — the config-field row in the table above should be cross-referenced here.
- `config: evm.maxFutureBlockRetryDistance is deprecated and ignored; use evm.emptyResultConfidence (blockHead|finalizedBlock) instead` — emitted as `log.Warn` at `EvmNetworkConfig.SetDefaults` (`common/defaults.go:L2081`).

### Edge cases & gotchas

1. **`emptyResultMaxAttempts` gates ALL data-availability reasons, not just empty results.** `block_unavailable`, `missing_data`, and `empty_result` (and `pending_tx`) all hit `dataUnavailableCapReached`. With default `emptyResultMaxAttempts=2`, only ONE retry fires for any not-ready condition, even if `maxAttempts=5`. Confirmed by test `TestNetworkExecutor_ShouldRetry_DataUnavailableSharesOneCap` (`erpc/network_executor_delay_test.go:L64-L85`).

2. **Empty EMA → no dynamic delay → emptyResultDelay fallback → if also 0 → immediate retry.** When `blockTime` is unknown (first few minutes after startup) AND `emptyResultDelay == 0`, retries for missing data fire with zero delay. Production configs should set `emptyResultDelay` to a reasonable value (e.g. 1s for Ethereum, 200ms for fast chains). Confirmed by `TestNetworkExecutor_ComputeDelay_EmptyResultFallsBackToFixed` (`erpc/network_executor_delay_test.go:L43-L58`).

3. **`emptyResultDelay` is the unified fallback for BOTH empty-result AND block-unavailable retries.** The former `blockUnavailableDelay` was merged into `emptyResultDelay`. A config with `blockUnavailableDelay` set still loads but emits a warning, then uses the value as `emptyResultDelay`. Confirmed by `TestNetworkExecutor_ComputeDelay_BlockUnavailableUsesEmptyResultDelay` (`erpc/network_executor_delay_test.go:L89-L105`).

4. **`RetryEmpty == false` gates both mark-empty-as-error (upstream) and retry decisions (network+upstream).** When `retryEmpty: false`, `upstreamPostForward_markUnexpectedEmpty` returns the raw null without converting. Even if converted by some other path, both `shouldRetryWithReason` and upstream `shouldRetry` check the directive again and block the retry (`architecture/evm/common.go:L22-L26`, `erpc/network_executor.go:L401-L404`, `upstream/upstream_executor.go:L251-L255`). Confirmed by `TestNetworkForward_RetryEmptyDisabled_BlocksMissingData` (`erpc/networks_retry_missing_data_test.go:L1743-L1815`).

5. **Within-round upstream fallback uses no delay.** Within a single execution sweep, when upstream A returns `ErrEndpointMissingData` or a 500, the loop immediately tries upstream B with NO sleep. The `emptyResultDelay` / backoff only fires between rounds (between network retry iterations), not between upstreams within a round. Confirmed by `TestNetworkForward_TryAllUpstreams_FallbackWithinSameRound` (`erpc/networks_retry_missing_data_test.go:L522-L660`).

6. **Upstreams that returned MissingData in round N are eligible for round N+1.** The upstream sweep does NOT permanently gate upstreams that returned missing-data or empty errors — they re-enter the selection pool in the next retry round. This was a bug in earlier versions (ErrorsByUpstream permanently blocked re-selection). Confirmed by `TestNetworkForward_UpstreamReselection_MissingDataSucceedsOnRetry` (`erpc/networks_retry_missing_data_test.go:L1203-L1308`).

7. **All-upstreams-missing-data produces `MaxAttempts` rounds × N upstreams total calls.** With `maxAttempts=3` and 2 upstreams both returning `ErrEndpointMissingData`, the system makes 6 total upstream calls (3 rounds × 2 each). Confirmed by `AllUpstreamsMissingData_NetworkRetryHappens` (`erpc/networks_retry_missing_data_test.go:L93-L172`).

8. **`ErrEndpointExecutionException` is always non-retryable at network scope unless the inner JSON-RPC error has `retryableTowardNetwork: true` explicitly set** (non-standard; this flag is `false` by default in `NewErrEndpointExecutionException`). Confirmed by `TestNetworkRetry_ExecutionException_NoRetry` (`erpc/networks_retry_missing_data_test.go:L309-L372`) and `TestIsRetryableTowardNetwork_ExecutionException` (`common/errors_retry_test.go:L140-L173`).

9. **Client-side errors (ErrEndpointClientSideException, -32600) ARE retryable at the network scope by default.** There is no `retryableTowardNetwork: false` on them, so `IsRetryableTowardNetwork` returns true. Another upstream might accept the request. Confirmed by `TestIsRetryableTowardNetwork_ClientError` (`common/errors_retry_test.go:L175-L187`).

10. **`bestResp` preference: a prior empty response beats a subsequent error.** When round 1 produces a null result (treated as `bestResp`) and round 2 produces an error, the final outcome is `(bestResp, nil)` rather than the error. This means emptyish responses can mask retry-round errors (`erpc/network_executor.go:L263-L267`). Confirmed by `TestNetworkForward_TryAllUpstreams_MixedErrorAndEmpty` (`erpc/networks_retry_missing_data_test.go:L906-L974`).

11. **`emptyResultAccept` silences `empty_result` retry but NOT `missing_data` retry.** If `eth_call` is in `emptyResultAccept` and the upstream explicitly returns a `result: null` with no error, the empty is accepted. But if the upstream returns an explicit JSON-RPC error that gets classified as `ErrEndpointMissingData` (e.g. `missing trie node`), that error bypasses `emptyResultAccept` and triggers retry unless `RetryEmpty == false`. Confirmed by `TestNetworkForward_TryAllUpstreams_AllEmpty_DelayBetweenRounds` (`erpc/networks_retry_missing_data_test.go:L661-L715`).

12. **Wrong-empty punishment vs. retry.** When an upstream returns `null` for a method in `MarkEmptyAsErrorMethods` and another upstream returns real data, the first upstream is recorded in `ErrorsByUpstream` for metrics. The upstream's `MisbehaviorsTotal` is incremented UNLESS the response block falls outside the upstream's configured `blockAvailability` bounds (in which case it's expected behavior). Confirmed by `TestNetworkForward_WrongEmpty_SkipPunishment_BlockAvailabilityBounds` (`erpc/networks_retry_missing_data_test.go:L1390-L1678`).

13. **`eth_sendRawTransaction` execution-reverted is returned unwrapped on retry exhaustion.** The network retry wraps the last error in `ErrFailsafeRetryExceeded` but explicitly unwraps it for `eth_sendRawTransaction` when the cause is `ErrEndpointExecutionException` — the client gets the original reverted JSON-RPC error, not the "gave up retrying" wrapper (`erpc/network_executor.go:L279-L283`).

14. **Backoff attempt index is 0-based.** The first retry uses `attempt=0`, meaning `BackoffFactor^0 = 1` — the first retry always waits exactly `Delay` with no exponential growth. Only from the second retry onward does the factor multiply. This differs from some libraries where attempt=0 is the initial attempt.

15. **Circuit breaker at network scope is forbidden.** `NewNetworkExecutor` returns an error if `cfg.CircuitBreaker != nil` (`erpc/network_executor.go:L68-L72`). Circuit breakers are upstream-scope only.

16. **`IsRetryableTowardNetwork` uses explicit iteration over multi-error wrappers, not `DeepSearch`.** The function handles `errors.Join`-style multi-error wrappers by calling `Unwrap() []error` and iterating over children explicitly (`common/errors.go:L2400-L2410`). It deliberately stops the single-cause chain walk before entering any multi-error wrapper (`common/errors.go:L2429-L2431`). This design avoids the order-dependency bug that `DeepSearch`-style traversal would introduce: with `DeepSearch`, whether an `errors.Join([retryable, non-retryable])` returns true or false depends on the iteration order, making retryability non-deterministic. The explicit loop rule (any child retryable → true) is order-independent. This means: if you see surprising retryable=false for a mixed error bundle, verify that the multi-error wrapper properly implements `Unwrap() []error`; an `Unwrap() error` interface would cause the code to fall through to the single-cause chain walk which would only see the first child.

17. **`ErrFailsafeConfiguration` aborts startup, not runtime.** This error (`common/errors.go:L1573-L1584`) is returned by `NewNetworkExecutor` or `NewUpstreamExecutor` at server initialization time — it is never returned during request processing. The two known triggers are: (a) a `failsafe` array entry at network scope includes a `circuitBreaker` policy (`erpc/network_executor.go:L68-L72`); (b) a `failsafe` array entry at upstream scope includes a `consensus` policy (`upstream/upstream_executor.go:L46-L53`). A misconfigured server will fail to start rather than silently misbehave at request time.

18. **Failsafe defaults merge uses wildcard method matching, not exact-match.** During defaults inheritance, `WildcardMatch(dfs.MatchMethod, fs.MatchMethod)` is called where the default entry's method is the pattern and the network/upstream entry's method is the subject (`common/defaults.go:L1804`, `L1631`). This means a defaults entry with `matchMethod: "eth_*"` will provide defaults for any network-entry with `matchMethod: "eth_getBlockByNumber"`. However, if ONLY ONE of the two has a method set (e.g. default has `matchMethod: "eth_call"` but the network entry has no `matchMethod`), the pair is NOT matched — `methodMatch = false` (`common/defaults.go:L1805-L1808`, `L1632-L1635`). A catch-all default (`matchMethod: "*"` or both fields empty) is required to cover all network entries uniformly.

19. **`emptyResultDelay` 0 = "use dynamic, no static fallback" NOT "no delay".** The value 0 is the unset sentinel in `RetryPolicyConfig.SetDefaults` (`common/defaults.go:L2300`): `if r.EmptyResultDelay == 0 && defaults != nil && defaults.EmptyResultDelay != 0 { inherit }`. There is no hardcoded fallback value for `EmptyResultDelay` — if neither the entry nor any parent default sets it, it remains 0. At runtime (`erpc/network_executor.go:L505-L507`), `cfg.EmptyResultDelay.Duration() > 0` is the guard, so 0 silently skips the static fallback path. The intent is "prefer dynamic; static is only for cold start". Operators who want guaranteed minimum spacing should set `emptyResultDelay` explicitly.

20. **`backoffFactor` must be > 0; `backoffMaxDelay` must be non-zero.** Both are validated by `RetryPolicyConfig.Validate()` (`common/validation.go:L1020-L1027`). Since `SetDefaults` always fills `backoffFactor = 1.2` and `backoffMaxDelay = 3s` when unset, these validation errors only surface when a config EXPLICITLY sets `backoffFactor: 0` or `backoffMaxDelay: 0`. The error messages reference `upstream.*.failsafe.retry.*` paths regardless of whether the entry is at network or upstream scope (cosmetic inconsistency in the validation error message, not a behavioral bug).

21. **`emptyResultIgnore` is silently ignored (NOT migrated) when `emptyResultAccept` is ALSO set.** The migration condition is `r.EmptyResultAccept == nil` (`common/defaults.go:L2262`). If an operator sets both fields in their config, `emptyResultIgnore` is completely ignored — its value never reaches `EmptyResultAccept`. The field is also NOT cleared (unlike `blockUnavailableDelay`), so `r.EmptyResultIgnore` retains the user-supplied value in memory but is never read at runtime. The practical failure mode during upgrade: operator adds `emptyResultAccept` to their config without removing the old `emptyResultIgnore`, expecting both to merge — they do NOT merge. Only `emptyResultAccept` takes effect.

22. **`blockUnavailableDelay` migration does NOT overwrite a user-set `emptyResultDelay`.** The merge condition is `r.EmptyResultDelay == 0` (`common/defaults.go:L2269`). If a config sets both `blockUnavailableDelay: 500ms` and `emptyResultDelay: 1s`, the `blockUnavailableDelay` value is silently discarded (the deprecation warning still fires, but the 500ms is dropped). Only the `1s` takes effect. After migration, `BlockUnavailableDelay` is always zeroed (`common/defaults.go:L2273`), so the old value leaves no trace in the in-memory struct.

23. **`matchMethod: ""` (empty string) after `SetDefaults` is a validation error, not a silent wildcard.** Although `SetDefaults` fills `"*"` for any empty `matchMethod`, if something upstream explicitly sets `MatchMethod = ""` AFTER `SetDefaults` runs, `Validate()` will reject it (`common/validation.go:L985-L988`). This matters for code paths that construct `FailsafeConfig` programmatically without going through `SetDefaults`. The config-file path is safe because `SetDefaults` always runs before `Validate`.

24. **`matchFinality` values must be exact string tokens.** Valid values are the four canonical strings: `finalized`, `unfinalized`, `realtime`, `unknown` (`common/data.go:L38-L71`). Numeric values `"0"`, `"1"`, `"2"`, `"3"` are also accepted by `UnmarshalYAML` for backward compatibility. Any other string causes an unmarshal error at config load time. There is no wildcard syntax within the `matchFinality` array — to match multiple states, list them all explicitly (e.g., `matchFinality: [unfinalized, realtime]`).

### Source map

- `common/config.go:L1279-L1387` — `FailsafeConfig`, `RetryPolicyConfig` struct definitions with field comments; `matchMethod` at L1280, `matchFinality` at L1281; `emptyResultIgnore` deprecation comment at L1351-L1352; `blockUnavailableDelay` deprecation comment at L1363-L1369 including `json:"-"` tag; `emptyResultDelay` field comment at L1355-L1361 is the authoritative description of the 0='use dynamic' semantic; type aliases `NetworkFailsafeConfig`, `UpstreamFailsafeConfig` at L1289-L1298
- `common/config.go:L2105-L2152` — `DirectiveDefaultsConfig` (RetryEmpty, RetryPending)
- `common/config.go:L2177-L2256` — `EvmNetworkConfig` (MarkEmptyAsErrorMethods, BlockUnavailableDelayMultiplier, EmptyResultConfidence)
- `common/adaptive_duration.go` — full `AdaptiveDuration` type: struct definition L60-L65, resolution algorithm L83-L109, YAML scalar shorthand parsing L164-L188, JSON parsing L195-L248, validation L122-L138, `inheritFrom` L144-L160
- `common/match.go:L18-L78` — `SelectExecutor` 4-tier failsafe executor selection by (matchMethod, matchFinality); `matchMethodPattern` helper L80-L92
- `common/defaults.go:L17-L34` — `MatchFinalities` function: set-overlap semantics for defaults merging (empty = wildcard)
- `common/defaults.go:L2130-L2138` — `FailsafeConfig.SetDefaults`: matchMethod default to `"*"` logic
- `common/defaults.go:L2225-L2305` — `RetryPolicyConfig.SetDefaults` — all per-field defaults with inheritance; `BackoffFactor` default 1.2 at L2233-L2238; `BackoffMaxDelay` default 3s at L2240-L2244; `emptyResultIgnore` migration at L2261-L2263; `blockUnavailableDelay` migration + warn at L2265-L2274; `EmptyResultDelay` inherits-only at L2298-L2302
- `common/defaults.go:L1793-L1832` — network-scope failsafe entry merge by matchMethod/matchFinality
- `common/defaults.go:L1621-L1672` — upstream-scope failsafe entry merge by matchMethod/matchFinality
- `common/defaults.go:L1978-L2058` — constants and `DefaultEmptyResultAccept()`, `DefaultMarkEmptyAsErrorMethods()`, `DefaultEmptyResultMaxAttempts=2`
- `common/defaults.go:L1454-L1471` — `DirectiveDefaultsConfig.SetDefaults` (no RetryEmpty default — nil = false)
- `common/data.go:L8-L71` — `DataFinalityState` type, iota constants, and `UnmarshalYAML` (accepts both string tokens and numeric `"0"–"3"`)
- `common/errors.go:L1573-L1584` — `ErrFailsafeConfiguration` type and constructor; returned at startup only
- `common/errors.go:L2361-L2509` — `IsRetryableTowardNetwork` (L2379-L2436; multi-error explicit iteration at L2400-L2410; chain-walk stops before multi-error at L2429-L2431), `IsRetryableTowardsUpstream`, `IsCapacityIssue`
- `common/errors.go:L1907-L2090` — error type constructors with `retryableTowardNetwork` flags
- `common/errors_retry_test.go` — unit tests covering all retryable classification combinations
- `common/validation.go:L1020-L1027` — `RetryPolicyConfig.Validate()`: `BackoffFactor > 0` required, `BackoffMaxDelay != 0` required
- `failsafe/backoff.go` — `ComputeBackoff` (pure function), `SleepCtx` (context-aware sleep)
- `erpc/network_executor.go` — `networkExecutor.runRetry`, `shouldRetryWithReason`, `computeDelay`, `isDataUnavailableReason`, `runHedge`; the main network-scope retry orchestration
- `erpc/network_executor_delay_test.go` — tests for delay computation edge cases (dynamic vs. fixed, shared cap, block-unavailable fallback)
- `erpc/networks_registry.go:L98-L137` — constructs `dynamicBlockUnavailableDelay` closure (EMA block time × multiplier) and wires it into each `networkExecutor`
- `upstream/upstream_executor.go` — `upstreamExecutor.runRetry`, `shouldRetry`, `computeDelay`; upstream-scope retry with simpler semantics (no data-availability logic, always `ComputeBackoff`)
- `architecture/evm/common.go` — `upstreamPostForward_markUnexpectedEmpty`, `emptyResultBeyondConfidence` — converts null results to `ErrEndpointMissingData`
- `architecture/evm/hooks.go:L109-L183` — `HandleUpstreamPostForward` dispatcher that calls `markUnexpectedEmpty` for methods in `MarkEmptyAsErrorMethods`
- `common/exec_state.go` — `ExecState` struct with per-scope atomic counters; `ExecStateSnapshot` for span/log labeling
- `telemetry/metrics.go:L463-L470` — `MetricNetworkRetryAttemptTotal`
- `telemetry/metrics.go:L835-L842` — `MetricNetworkDataUnavailableWaitSeconds`
- `erpc/networks_retry_missing_data_test.go` — integration tests covering network retry, missing data, execution exception, retry-empty directive, upstream re-selection, wrong-empty punishment
