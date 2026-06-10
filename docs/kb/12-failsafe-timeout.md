# KB: Failsafe — Timeout Policy + Adaptive Durations

> status: complete
> source-dirs: common/config.go (TimeoutPolicyConfig), common/adaptive_duration.go, common/adaptive_duration_compat_test.go, common/timeout_func.go, common/defaults.go, erpc/http_timeout.go, erpc/network_executor.go, erpc/networks.go, erpc/networks_timeout_test.go, telemetry/metrics.go

## L1 — Capability (CTO view)

eRPC has a three-layer timeout hierarchy: an HTTP-server-level `maxTimeout` that hard-kills any request that hasn't responded within a wall-clock budget, a per-(network, method, finality) failsafe timeout that bounds the entire retry+hedge execution lifecycle, and optional per-upstream failsafe timeouts that bound individual upstream attempts. Timeouts can be static (fixed duration) or quantile-driven (computed from P50/P90/P99 of observed per-method response times), giving operators a latency budget that automatically tracks method-specific behavior without manual tuning. The dynamic path has cold-start fallback logic and a feedback-loop prevention floor.

## L2 — Mechanics (staff-engineer view)

**Three-layer hierarchy.** From outermost to innermost:

1. **HTTP server `maxTimeout`** (`server.maxTimeout`, default `150s`). Applied via `TimeoutHandler` wrapping the entire HTTP handler (`erpc/http_server.go:L157`, `erpc/http_timeout.go:L20-L143`). Uses `context.WithTimeoutCause(r.Context(), dt, ErrHandlerTimeout)`. Fires with HTTP 504 (GET) or HTTP 200 + JSON-RPC `-32603` body (POST). This is a global ceiling — no request survives it regardless of failsafe policy.

2. **Network-scope failsafe timeout** (`networks[].failsafe[].timeout`). Applied as `context.WithTimeoutCause(ctx, *td, common.ErrDynamicTimeoutExceeded)` wrapping the entire `networkExecutor.Run` call — so it covers ALL retries and hedges for that method match, not just a single attempt (`erpc/network_executor.go:L165-L171`). Fires with `ErrDynamicTimeoutExceeded` sentinel, which is later classified as `ErrFailsafeTimeoutExceeded{scope: "network"}`.

3. **Upstream-scope failsafe timeout** (`upstreams[].failsafe[].timeout`). Applied inside the upstream's own failsafe executor, bounding one upstream's entire retry budget. The same `ErrDynamicTimeoutExceeded` sentinel is used; classification becomes `ErrFailsafeTimeoutExceeded{scope: "upstream"}`.

**No per-attempt timeout.** There is NO isolated per-attempt timeout wrapping just one RPC call. The failsafe timeout is lifecycle-scoped (covers the whole executor run). This is intentional and tested: `NetworkTimeoutIsLifecycleScoped_BoundsRetryBudget` (`erpc/networks_timeout_test.go:L378`) verifies that a 500ms network timeout with 3 retries takes ~500ms total, not 500ms × 3.

**Timeout function construction.** `common.NewTimeoutFunc(logger, cfg)` builds a `TimeoutFunc` from `TimeoutPolicyConfig`. The returned function is called per-request and returns `*time.Duration` (nil when disabled, `common/timeout_func.go:L23-L83`). For static configs (`quantile==0`) the duration is pre-computed once at construction time. For quantile configs, the function queries the network's `QuantileTracker` on each call.

**Auto-floor for quantile timeouts.** When `quantile>0` and `min` is unset, `NewTimeoutFunc` auto-populates `min = base/2` (or `500ms` when `base==0`) before resolving. This prevents the feedback-loop where successful fast quantiles collapse the timeout to e.g. 50ms, which then causes more timeouts, which keep the quantile low (`common/timeout_func.go:L29-L37`).

**Cold-start fallback.** When no latency data is available yet, `AdaptiveDuration.Resolve` returns `Min` (which may be the auto-floor). Additionally, `coldStartFallback` in `NewTimeoutFunc` returns `Base` first, then `Max` as the fallback duration when `Resolve` returns 0 (`common/timeout_func.go:L85-L94`). This covers the case where `min` is also zero — `Max` provides the last resort.

**Error classification.** The sentinel `ErrDynamicTimeoutExceeded` is set as the `context.Cause` by `context.WithTimeoutCause`. When the executor returns and this cause is detected, the network layer wraps it as `ErrFailsafeTimeoutExceeded{scope}` (`erpc/networks.go:L1428-L1451`). The upstream layer does the same (`upstream/upstream.go`). This typed error propagates to callers and the HTTP layer for correct status-code and JSON-RPC error body mapping.

**Timeout attribution and counter guard.** `MetricNetworkTimeoutFiredTotal` is incremented by the scope (network or upstream) that OWNS the timeout policy. Three guards prevent double-counting:
1. Only fires when `failsafeExecutor.HasTimeout()` is true (this scope has a configured timeout, `erpc/networks.go:L1435`)
2. Only fires when `errors.Is(execErr, common.ErrDynamicTimeoutExceeded)` — the sentinel is present
3. Only fires when `!HasErrorCode(execErr, ErrCodeFailsafeTimeoutExceeded)` — another scope's timeout has not already classified it

Guard 3 means: if an upstream-scope timeout fires and bubbles up as `ErrFailsafeTimeoutExceeded`, the network-scope counter does NOT double-count it (`erpc/networks_timeout_test.go:L495-L538`).

**Retry-exhausted takes precedence.** When retry exhausts and the LAST attempt happened to time out, the top-level error is `ErrFailsafeRetryExceeded` (not `ErrFailsafeTimeoutExceeded`). Guard: `!HasErrorCode(execErr, ErrCodeFailsafeRetryExceeded)` prevents the timeout counter from firing on retry-exhausted-with-timeout-tail scenarios (`erpc/networks_timeout_test.go:L429-L493`, `erpc/networks.go:L1436`).

**Parent context vs failsafe timeout.** When the HTTP server's `maxTimeout` fires, the `ErrHandlerTimeout` is set as the context cause on the *outer* HTTP request context. The executor's inner context is derived from this, so `context.DeadlineExceeded` propagates. The network executor checks: if the error is `context.DeadlineExceeded` but NOT a `StandardError`, and the context cause is `ErrDynamicTimeoutExceeded`, it substitutes the cause; otherwise it propagates the parent error unchanged. This ensures a parent-context deadline is NOT misclassified as `ErrFailsafeTimeoutExceeded` (`erpc/networks_timeout_test.go:L101-L145`, `erpc/networks.go:L1427-L1432`).

**HTTP server timeout behavior.** `TimeoutHandler` buffers the entire response in a `timeoutWriter` (pooled `bytes.Buffer`). On timeout, it discards the buffer and writes a fixed JSON-RPC body. On normal completion within deadline, it copies buffered headers + body to the wire (`erpc/http_timeout.go:L36-L143`). POST requests always return HTTP 200 even on timeout; GET/other requests return HTTP 504 (`erpc/http_timeout.go:L109-L112`).

**Per-method quantile tracking.** The `Network` interface exposes `GetMethodMetrics(method string) TrackedMetrics`, which returns a `TrackedMetrics` containing a `QuantileTracker`. The tracker accumulates per-method response times and answers `GetQuantile(q)` queries (`common/network.go:L25`, `common/network.go:L51-L60`). The `QuantileTracker` is maintained by the network's method-metrics registry; its implementation uses a streaming quantile algorithm (not a histogram). The `AdaptiveDuration.ResolveForRequest` utility bridges from request context to the tracker (`common/adaptive_duration.go:L270-L290`).

## L3 — Exhaustive reference (agent view)

### Config fields

#### `server.maxTimeout` (HTTP server level)

| # | YAML path | Type | Default | Behavior |
|---|-----------|------|---------|----------|
| 1 | `server.maxTimeout` | `*Duration` | `150s` (hardcoded in `erpc/http_server.go:L65-L67`; `ServerConfig` has no explicit default for this in `defaults.go` — the code falls back to `150 * time.Second`) | Global ceiling on any HTTP request including setup, upstream selection, all retries and hedges. Applied by `TimeoutHandler` via `context.WithTimeoutCause`. POST: HTTP 200 + `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"http request handling timeout"}}`. Non-POST: HTTP 504. Source: `erpc/http_server.go:L65-L67`, `erpc/http_timeout.go:L107-L116` |

#### `TimeoutPolicyConfig` (network or upstream scope)

All fields under `networks[].failsafe[].timeout` or `upstreams[].failsafe[].timeout`. Struct at `common/config.go:L1406-L1408`. The `duration` field is an `AdaptiveDuration`.

| # | YAML path | Type | Default | Behavior |
|---|-----------|------|---------|----------|
| 2 | `timeout.duration` | `Duration \| AdaptiveDuration` | Inherited from parent defaults when absent. Network system default: `120s` static (`common/defaults.go:L134-L136`). Upstream system default: `60s` static (`common/defaults.go:L155-L157`). | The timeout budget. Scalar (`"5s"`, `5000`) sets `duration.base` only. Object `{base, quantile, min, max}` enables adaptive mode. When zero/nil, timeout is disabled for this scope. Source: `common/config.go:L1407`, `common/timeout_func.go:L24-L26` |
| 3 | `timeout.duration.base` | `Duration` | `0` unless inherited | Static fallback duration. When `quantile==0`, this IS the timeout. When `quantile>0`, this is the cold-start fallback AND the `base` addend before clamping. `coldStartFallback` prefers `base` over `max` (`common/timeout_func.go:L86-L88`). |
| 4 | `timeout.duration.quantile` | `float64` | `0` (static mode) | Percentile of per-method response times. Valid `[0, 1]`. When `>0`, requires `base` or `max` (`common/adaptive_duration.go:L129`). Example: `0.99` means "timeout = P99 of this method's latency, clamped to [min, max]". |
| 5 | `timeout.duration.min` | `Duration` | **Auto-floor**: when `quantile>0` and `min==0`, auto-set to `base/2` (or `500ms` when `base==0`) by `NewTimeoutFunc` (`common/timeout_func.go:L31-L36`). | Floor after quantile resolution. Prevents feedback-loop collapse (success quantiles shrink → timeout shrinks → more timeouts → repeat). The auto-floor is applied at `NewTimeoutFunc` build time, not at config-load time. |
| 6 | `timeout.duration.max` | `Duration` | `0` (uncapped unless set) | Ceiling after quantile resolution. Cold-start fallback of last resort when `base==0` and no quantile data: `coldStartFallback` returns `max` if `base==0` (`common/timeout_func.go:L89-L91`). Without `max` and without `base`, a pure-quantile cold-start returns nil (no timeout), which is fail-open. |

#### Legacy flat form (backward compatible) — migration path

`timeout` YAML (and JSON) accepts three flat sibling fields at the `timeout` level: `quantile`, `minDuration`, and `maxDuration`. These are NOT discrete config fields with their own semantic meaning — they are legacy aliases that get **folded into** the unified `duration` `AdaptiveDuration` object at unmarshal time via `applyLegacySiblings` (`common/config.go:L1464-L1480`). After unmarshal, `TimeoutPolicyConfig` contains only `Duration *AdaptiveDuration`; the flat siblings do not exist as struct fields.

**Folding semantics:**
1. If ALL THREE flat siblings are zero/unset, `applyLegacySiblings` is a no-op (early return at `common/config.go:L1465-L1467`).
2. If any sibling is non-zero, a `Duration` object is created if nil (`common/config.go:L1468-L1470`).
3. Each sibling is folded in **only if the corresponding `duration` sub-field is still zero** — the modern object form wins over legacy siblings for any field that is already set. Partial mixing is allowed: legacy can fill fields that the object form left unset. Source: `common/config.go:L1471-L1479`.

**Type accepted:**
- `timeout.quantile`: `float64`, same range `[0, 1]` as `timeout.duration.quantile`.
- `timeout.minDuration`: `Duration` (YAML string `"200ms"` or int-as-ms `200`; JSON string or number).
- `timeout.maxDuration`: `Duration` (same formats as `minDuration`).

**No default — these are input-only wires.** If absent in the config, they contribute nothing. The effective defaults are those of the `Duration` `AdaptiveDuration` fields they map to (rows 3-6 above).

| # | YAML path | Type | Default | Behavior |
|---|-----------|------|---------|----------|
| 7 | `timeout.quantile` | `float64` | `0` — not a standalone field; contributes to `timeout.duration.quantile` only when that sub-field is 0 | Legacy flat alias. Folded into `duration.Quantile` at YAML/JSON unmarshal if `duration.Quantile == 0` (`common/config.go:L1471-L1473`). Recognized by both `UnmarshalYAML` (`common/config.go:L1420-L1434`) and `UnmarshalJSON` (`common/config.go:L1436-L1462`). |
| 8 | `timeout.minDuration` | `Duration` (string or int-as-ms) | `0` — contributes to `timeout.duration.min` only when that sub-field is 0 | Legacy flat alias. Folded into `duration.Min` at unmarshal if `duration.Min == 0` (`common/config.go:L1474-L1476`). JSON path: `raw.MinDuration` parsed via `parseJSONDuration` (`common/config.go:L1451-L1453`). |
| 9 | `timeout.maxDuration` | `Duration` (string or int-as-ms) | `0` — contributes to `timeout.duration.max` only when that sub-field is 0 | Legacy flat alias. Folded into `duration.Max` at unmarshal if `duration.Max == 0` (`common/config.go:L1477-L1479`). JSON path: `raw.MaxDuration` parsed via `parseJSONDuration` (`common/config.go:L1454-L1457`). |

**Migration mapping — old YAML → new YAML:**

```yaml
# OLD (legacy flat form — still works, but deprecated style):
failsafe:
  - matchMethod: "*"
    timeout:
      duration: 5s          # populates duration.base
      quantile: 0.99        # flat sibling → duration.quantile
      minDuration: 200ms    # flat sibling → duration.min
      maxDuration: 10s      # flat sibling → duration.max

# NEW (unified object form — preferred):
failsafe:
  - matchMethod: "*"
    timeout:
      duration:
        base: 5s
        quantile: 0.99
        min: 200ms
        max: 10s
```

Both forms produce an identical in-memory `TimeoutPolicyConfig` after unmarshal. Test-locked in `common/adaptive_duration_compat_test.go:L16-L99` (cases `"legacy flat with quantile + bounds"` and `"new object form"`).

**Partial mixing — object form takes precedence for set fields:**

When a config uses the object form for some sub-fields and legacy siblings for others, the object form wins for any field it already set; legacy siblings fill only the gaps:

```yaml
# Object form has base + quantile; legacy sibling minDuration fills min (which was unset):
timeout:
  duration:
    base: 10s
    quantile: 0.95   # object form wins; legacy quantile: 0.50 is ignored
  quantile: 0.50     # IGNORED — duration.Quantile is already 0.95
  minDuration: 1s    # ACCEPTED — duration.Min was 0; fills it to 1s
```

Result: `{Base: 10s, Quantile: 0.95, Min: 1s, Max: 0}`. Test-locked: `common/adaptive_duration_compat_test.go:L75-L88` (case `"object form takes precedence; legacy siblings ignored when set"`).

**JSON wire format for legacy flat siblings:**

```json
{
  "timeout": {
    "duration": "5s",
    "quantile": 0.99,
    "minDuration": "200ms",
    "maxDuration": "10s"
  }
}
```

`minDuration` and `maxDuration` in JSON accept either a string (`"200ms"`) or a number interpreted as milliseconds (`200`). Source: `common/config.go:L1441-L1461` (`UnmarshalJSON`, `parseJSONDuration`).

#### `AdaptiveDuration` resolution rules (applies to both timeout and hedge delay)

Resolution algorithm for `AdaptiveDuration.Resolve(qt QuantileTracker)` (`common/adaptive_duration.go:L83-L109`):

```
if spec == nil || spec.IsZero(): return 0

if spec.Quantile <= 0:
    return spec.Base  // static mode; min/max ignored

// Adaptive path: query per-method latency histogram at the configured quantile.
// qt is nil or has no data on cold start (no prior requests for this method).
adaptive = qt.GetQuantile(spec.Quantile)  // e.g. p90 of observed response times
if adaptive <= 0:
    // Cold-start: no latency data yet — use Min as the entire adaptive value.
    // Note: this is NOT Base; Base is the addend, not the fallback here.
    adaptive = spec.Min  // will be 0 if Min is also unset

v = spec.Base + adaptive  // additive: static base + quantile-driven component

// Clamp to [min, max]
if spec.Min > 0 && v < spec.Min: v = spec.Min
if spec.Max > 0 && v > spec.Max: v = spec.Max
return v
```

**Concrete resolution formula:** `timeout = clamp(Base + quantile(p) × methodLatency, Min, Max)`
- `quantile(p)` is the observed p-th percentile of response times for the specific method (e.g. `eth_call`), sourced from the `QuantileTracker` maintained by the network's method-metrics registry.
- The `QuantileTracker` uses a streaming quantile algorithm (not a fixed histogram); it accumulates actual wall-clock response durations per method.
- `p` is the `timeout.duration.quantile` value (e.g. `0.9` for P90, `0.5` for P50). Source: `common/adaptive_duration.go:L83-L109`.

**Cold-start behavior (no latency data available):**
1. `Resolve` is called with a nil or empty tracker → `qt.GetQuantile` returns 0 → `adaptive = spec.Min` (which may be 0 if min is unset).
2. If `Base + Min == 0`, `Resolve` returns 0.
3. `NewTimeoutFunc` detects `dr <= 0` and calls `coldStartFallback(&resolved)`:
   - Returns `Base` if `Base > 0`
   - Else returns `Max` if `Max > 0`
   - Else returns nil (no timeout — fail-open)
4. So effective cold-start order: `Base > Max > nil`. Source: `common/timeout_func.go:L66-L70,L85-L94`.

**Feedback-loop prevention floor:** When `quantile > 0` and `min == 0`, `NewTimeoutFunc` auto-populates `resolved.Min = Base/2` (or `500ms` when Base is also 0) BEFORE the closure is built. This prevents the runaway scenario where short quantiles drive timeouts so low that all requests time out, keeping quantiles short in a self-reinforcing loop. The auto-floor is a LOCAL copy mutation — the original config is unchanged. Source: `common/timeout_func.go:L29-L37`.

Key invariants (test-locked in `common/adaptive_duration_test.go`):
- Static base ignores min/max (returns `base` unchanged)
- Quantile cold-start inside `Resolve` falls back to `min`, NOT `base`; `coldStartFallback` in `NewTimeoutFunc` then falls back to `base` or `max` at the next level
- `base + quantile` is additive before clamping
- `max` clamps high values regardless of how large the quantile is
- nil spec returns `0` (disabled/no-op)

### Behaviors & algorithms

**`ErrDynamicTimeoutExceeded` sentinel — full definition:**
- Declared as `var ErrDynamicTimeoutExceeded = errors.New("dynamic timeout exceeded")` at `common/errors.go:L1984`.
- It is a plain `*errors.errorString` — NOT a `StandardError` (not a `BaseError`, has no `Code`, `Message`, or `Details` fields). This is intentional: it is a control-flow signal, not a client-facing domain error.
- Set as the cause of the context via `context.WithTimeoutCause(ctx, duration, common.ErrDynamicTimeoutExceeded)` by the failsafe executor. Source: `erpc/network_executor.go:L164-L171`.
- Detection pattern: `context.Cause(ctx) == common.ErrDynamicTimeoutExceeded` (identity comparison, not `errors.Is`, because it is not a wrapped chain).
- Detection via chain: `errors.Is(err, common.ErrDynamicTimeoutExceeded)` works because `context.WithTimeoutCause` makes it the cause of the context error which propagates.
- Distinguishes failsafe-policy timeouts from HTTP-server-level timeouts (`ErrHandlerTimeout`) and from generic parent-context deadlines. When the HTTP server's context fires, the cause is `ErrHandlerTimeout`, NOT `ErrDynamicTimeoutExceeded` — the guard at `erpc/networks.go:L1428-L1431` prevents misclassification.
- After classification: the sentinel is wrapped by `NewErrFailsafeTimeoutExceeded(scope, cause, &startTime)` into a `StandardError` with code `ErrCodeFailsafeTimeoutExceeded` for downstream propagation. Source: `common/errors.go:L1588-L1603`.

**`NewTimeoutFunc` construction path:**
1. If `cfg == nil || cfg.Duration.IsZero()`: return nil (no timeout)
2. If `quantile <= 0`: pre-compute `dur = spec.Resolve(nil)`, return a constant function
3. If `quantile > 0` and `min == 0`: auto-set `resolved.Min = base/2` or `500ms`
4. Return a per-request closure that calls `ntw.GetMethodMetrics(m).GetResponseQuantiles()` then `resolved.Resolve(qt)`, emitting `MetricNetworkTimeoutDurationSeconds` on each call

**Timeout scope application:**
- Network scope: `context.WithTimeoutCause(ctx, *td, common.ErrDynamicTimeoutExceeded)` wraps the entire `networkExecutor.Run` call. Context is derived from the request context, so the HTTP-server maxTimeout is an outer layer (`erpc/network_executor.go:L164-L171`).
- Upstream scope: same pattern inside the upstream's failsafe executor (in `upstream/upstream.go`).

**Error chain and classification:**
- Timeout fires → `context.Cause(ctx) == ErrDynamicTimeoutExceeded`
- Network executor wraps: `NewErrFailsafeTimeoutExceeded(ScopeNetwork, cause, &startTime)` → `ErrCodeFailsafeTimeoutExceeded`
- Upstream executor wraps: same but `ScopeUpstream`
- HTTP server sees `ErrFailsafeTimeoutExceeded` → JSON-RPC error body with code `-32603` and message from the error chain

**Retry interaction:** Network timeout wraps ALL retry attempts. A 500ms network timeout with `maxAttempts: 3` has a 500ms total budget shared across all 3 attempts. This is lifecycle-scoped, not per-attempt. Inside the retry loop, context checks at the start and end of each attempt propagate the timeout cause early.

**Hedge interaction:** Network timeout also wraps hedge. All legs of a hedge race (primary + hedges) share the same deadline. If the deadline fires before any hedge leg wins, all legs are cancelled.

**`ErrHandlerTimeout` vs `ErrDynamicTimeoutExceeded`.** The HTTP server sets `ErrHandlerTimeout` as the cause of its `context.WithTimeoutCause`. The failsafe executor sets `ErrDynamicTimeoutExceeded`. These are DIFFERENT sentinels. The network executor checks for `ErrDynamicTimeoutExceeded` specifically — it will NOT misclassify the HTTP server's timeout as `ErrFailsafeTimeoutExceeded` (`erpc/networks_timeout_test.go:L101-L145`).

### Observability

| Metric | Type | Labels | When fired | Source |
|--------|------|--------|------------|--------|
| `erpc_network_timeout_fired_total` | counter | project, network, category, finality, scope | Timeout policy killed a request; `scope=network` or `scope=upstream`; suppressed when retry-exhausted error wins; suppressed when a lower scope already fired | `erpc/networks.go:L1440`, `upstream/upstream.go:L845`, `telemetry/metrics.go:L437` |
| `erpc_network_timeout_duration_seconds` | histogram | project, network, category, finality | Dynamic timeout duration computed per request (only when `quantile>0`); buckets `[0.05, 0.1, 0.3, 0.5, 1, 3, 5, 10, 30]` | `common/timeout_func.go:L73-L80`, `telemetry/metrics.go:L820` |

**`erpc_network_timeout_fired_total` suppression and scope mechanics:**

The counter has a `scope` label (`"network"` or `"upstream"`) and is guarded by three conditions at `erpc/networks.go:L1435-L1438` (network scope) and `upstream/upstream.go:L845` (upstream scope):

1. `failsafeExecutor.HasTimeout()` — this scope must have a configured timeout policy. If neither network nor upstream has a timeout, this check fails and the counter does not fire.
2. `errors.Is(execErr, common.ErrDynamicTimeoutExceeded)` — the eRPC-specific sentinel must be present in the error chain. Generic `context.DeadlineExceeded` from a parent context does NOT satisfy this.
3. `!common.HasErrorCode(execErr, common.ErrCodeFailsafeRetryExceeded)` — **retry-exhausted suppression**: when the retry policy fully exhausts and the last attempt timed out, the top-level error is `ErrFailsafeRetryExceeded`. Because this code check fires first, the timeout counter is NOT incremented. The retry counter instead captures the event. This prevents double-counting "timeout + retry exhausted" scenarios.
4. `!common.HasErrorCode(execErr, common.ErrCodeFailsafeTimeoutExceeded)` — **cross-scope suppression**: when an upstream-scope timeout fires and is already classified as `ErrFailsafeTimeoutExceeded{scope: upstream}`, the network-scope check at the outer layer sees this code and skips its own counter increment. A single timeout event fires exactly one counter at the scope that owns the policy.

Simultaneous timeout + retry-exhaustion: if both a timeout fires AND retry exhausts in the same execution, `ErrFailsafeRetryExceeded` wins as the top-level error. Guard 3 (`!HasErrorCode(execErr, ErrCodeFailsafeRetryExceeded)`) prevents `erpc_network_timeout_fired_total` from incrementing. Only `erpc_network_requests_received_total` (with error label) and the retry error path reflect this event. Source: `erpc/networks.go:L1435-L1438`, `erpc/networks_timeout_test.go:L429-L493`.

Note: `erpc_network_timeout_fired_total` has a `scope` label with values `"network"` and `"upstream"`. A single upstream timeout firing bumps only `scope=upstream`. A network timeout bumps only `scope=network`. Double-counting is actively prevented by the `!HasErrorCode(execErr, ErrCodeFailsafeTimeoutExceeded)` guard (`erpc/networks_timeout_test.go:L495-L538`).

Note: `erpc_network_timeout_duration_seconds` is emitted by the quantile-mode `TimeoutFunc` closure on each request, not just on timeout fires. It reflects what duration was COMPUTED, not whether the request timed out. `finality` comes from `req.Finality(ctx)` at compute time.

### Edge cases & gotchas

1. **Server maxTimeout is a hard ceiling regardless of failsafe.** `server.maxTimeout` (default 150s) wraps all other timeouts. A failsafe `timeout.duration: "200s"` will never run for 200s because the HTTP handler times out first. Always set failsafe timeouts shorter than `server.maxTimeout`. Source: `erpc/http_server.go:L65-L67`, `erpc/http_timeout.go:L37`.

2. **Quantile cold-start is fail-open when both `base` and `max` are zero.** If you configure `{quantile: 0.95}` with neither `base` nor `max`, `coldStartFallback` returns nil on cold start → no timeout applied. After data warms up, the auto-floor kicks in. Mitigation: always set `base` or `max` when using quantile mode. Source: `common/timeout_func.go:L85-L93`, `erpc/networks_timeout_test.go:L334-L376`.

3. **Auto-floor is applied at build time, not config-load time.** `NewTimeoutFunc` modifies a LOCAL copy of the spec (`resolved := *spec`). The original `TimeoutPolicyConfig` is not mutated. A second call to `NewTimeoutFunc` with the same config will apply the auto-floor again. Source: `common/timeout_func.go:L27-L36`.

4. **Static base ignores min/max clamp.** Same as for hedge delay: when `quantile==0`, `Resolve` returns `base` exactly. Setting `min` and `max` on a static-base timeout spec has no effect. Source: `common/adaptive_duration.go:L88-L89`.

5. **Quantile timeout cold-start uses `min` (NOT base) as the adaptive value.** When `qt.GetQuantile(q) == 0`, the formula substitutes `spec.Min` as `adaptive`. Then `v = spec.Base + spec.Min`. If `base == 0`, `v = min`. This is the expected behavior but differs from what one might expect ("use Base as the fallback"). Source: `common/adaptive_duration.go:L96-L98`.

6. **Retry-exhausted error wins over timeout-on-last-attempt.** When the retry policy exhausts and the last attempt's failure was caused by a timeout, the outer classification is `ErrFailsafeRetryExceeded` — NOT `ErrFailsafeTimeoutExceeded`. Callers must inspect `ErrCodeFailsafeRetryExceeded` for retry exhaustion. The timeout counter also does NOT fire in this scenario. Source: `erpc/networks_timeout_test.go:L429-L493`, `erpc/networks.go:L1436`.

7. **Upstream timeout bubbling to network scope.** When an upstream failsafe timeout fires (`scope=upstream`), it wraps the error as `ErrFailsafeTimeoutExceeded{scope: upstream}`. The network executor's outer check `!HasErrorCode(execErr, ErrCodeFailsafeTimeoutExceeded)` sees this code already set and skips the network-scope classification and counter increment. The original upstream-scope error propagates to the caller. Source: `erpc/networks_timeout_test.go:L495-L538`.

8. **`NetworkOnlyTimeout` does not count at upstream scope.** When only network scope has a timeout and the upstream has none, the ctx cancellation propagates into the upstream's attempt. Since `failsafeExecutor.timeout == nil` at the upstream level, the upstream does NOT increment its timeout counter and does NOT wrap the error as `ErrFailsafeTimeoutExceeded`. The network scope handles everything. Source: `erpc/networks_timeout_test.go:L540-L586`.

9. **Quantile histogram emits regardless of timeout fire.** `erpc_network_timeout_duration_seconds` observes every computed timeout duration, not just ones that actually fired. It is a "what was the budget" diagnostic, not a "did it fire" counter. The companion counter for "fired" is `erpc_network_timeout_fired_total`. Source: `common/timeout_func.go:L73-L80`, `erpc/networks_timeout_test.go:L630-L696`.

10. **HTTP response for POST timeout is always 200.** The JSON-RPC spec requires transport-layer 200 for method errors. `TimeoutHandler` writes `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"http request handling timeout"}}` with `Content-Type: application/json` and HTTP 200 for POST requests. For non-POST (GET healthchecks etc.) it returns HTTP 504. Source: `erpc/http_timeout.go:L107-L116`.

11. **Timeout context cause propagation through deadline.** `context.WithTimeoutCause` in Go 1.21+ sets a custom cause. `context.Cause(ctx)` returns that cause. `errors.Is(ctx.Err(), context.DeadlineExceeded)` is true, but `context.Cause(ctx) == ErrDynamicTimeoutExceeded` discriminates eRPC's timeout from the HTTP server's. Without this distinction, a parent-context deadline would be misclassified. Source: `erpc/networks.go:L1427-L1432`.

12. **Legacy flat siblings do not override already-set `duration` sub-fields.** `applyLegacySiblings` checks each sub-field individually before overwriting: `if c.Duration.Quantile == 0 { c.Duration.Quantile = quantile }`. This means if `duration: {quantile: 0.95}` is set in the object form and `quantile: 0.50` is also provided as a flat sibling, the result is `quantile=0.95` (object form wins). The flat sibling for an already-set field is silently dropped — no error, no warning. Test-locked: `common/adaptive_duration_compat_test.go:L75-L88` (case `"object form takes precedence; legacy siblings ignored when set"`). Source: `common/config.go:L1471-L1479`.

13. **Legacy flat siblings can partially fill an object-form `duration`.** If `duration` is given as an object but omits some fields, the legacy flat siblings CAN fill those omitted fields (because the `== 0` guard passes for unset sub-fields). This is intentional partial-mixing support — mixing old and new styles in the same `timeout` block is allowed. The effective result is the union of the two forms with object-form values taking precedence. Source: `common/config.go:L1464-L1480`, `common/adaptive_duration_compat_test.go:L75-L88`.

14. **`applyLegacySiblings` early-exit: all-zero flat siblings are free.** When `quantile==0 && minDuration==0 && maxDuration==0`, `applyLegacySiblings` returns immediately without allocating a `Duration` object. Old configs that only have `duration: 5s` (no flat quantile/min/max) don't pay any allocation cost. Source: `common/config.go:L1465-L1467`.

15. **Legacy flat `quantile`-only (no `duration` scalar) creates `duration` from nil.** A config block containing only `quantile: 0.7` with no `duration:` field at all is valid: `applyLegacySiblings` allocates `c.Duration = &AdaptiveDuration{}` and sets only `Quantile=0.7`. The `Duration` object has `Base=0, Min=0, Max=0` — this is the quantile-only case. At `NewTimeoutFunc` build time this passes `quantile>0` and `base==0`; the auto-floor will set `min=500ms`, and on cold start `coldStartFallback` will return `max` (also 0) → nil (fail-open). Operators using the legacy `quantile`-only form should add `maxDuration` to avoid cold-start fail-open. Test-locked (hedge equivalent): `common/adaptive_duration_compat_test.go:L157-L163` (case `"quantile-only (no base) via legacy form"`). Source: `common/config.go:L1468-L1473`.

### Source map

- `erpc/http_timeout.go` — `TimeoutHandler`, `timeoutWriter`; HTTP-server-level timeout (outermost layer); ErrHandlerTimeout sentinel
- `erpc/http_server.go:L65-L67,L157` — `maxTimeout` retrieval and `TimeoutHandler` instantiation
- `common/timeout_func.go` — `NewTimeoutFunc`; auto-floor logic; `coldStartFallback`; quantile resolution per request; `MetricNetworkTimeoutDurationSeconds` observation
- `common/adaptive_duration.go` — `AdaptiveDuration`; `Resolve`; `ResolveForRequest`; `inheritFrom`; `validate`; `IsZero`; wire format (scalar + object)
- `common/adaptive_duration_test.go` — Unit tests locking resolution semantics (static mode, quantile mode, clamp, nil, cold-start)
- `common/adaptive_duration_compat_test.go` — Backward-compat tests for legacy flat-sibling folding: `TestTimeoutPolicyConfig_LegacyYAML` (5 cases including partial-mixing and precedence) and `TestHedgePolicyConfig_LegacyYAML`; also `TestConsensusWaitCaps_YAML`
- `common/config.go:L1398-L1480` — `TimeoutPolicyConfig`; YAML unmarshal (`UnmarshalYAML`); JSON unmarshal (`UnmarshalJSON`); `applyLegacySiblings` (legacy flat sibling folding logic)
- `common/defaults.go:L2213-L2223` — `TimeoutPolicyConfig.SetDefaults`; inheritance logic; system-level network default (120s) and upstream default (60s)
- `common/network.go:L51-L60` — `QuantileTracker` and `TrackedMetrics` interfaces
- `erpc/network_executor.go:L86-L88,L103-L108,L164-L171` — `NewNetworkExecutor` wires `NewTimeoutFunc`; `HasTimeout`; `Run` applies lifecycle context
- `erpc/networks.go:L1424-L1451` — Timeout error classification after executor returns; `MetricNetworkTimeoutFiredTotal` increment with all three guards
- `erpc/networks_timeout_test.go` — Integration tests: fixed timeout, quantile timeout, cold-start, lifecycle semantics, scope attribution, double-count regression, parent-ctx non-misclassification
- `common/errors.go:L1588-L1603,L1981-L1984` — `ErrCodeFailsafeTimeoutExceeded`; `NewErrFailsafeTimeoutExceeded`; `ErrDynamicTimeoutExceeded` sentinel
- `telemetry/metrics.go:L437-L441,L820-L824` — Metric declarations: `MetricNetworkTimeoutFiredTotal`, `MetricNetworkTimeoutDurationSeconds`
