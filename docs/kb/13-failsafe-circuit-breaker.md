# KB: Failsafe: circuit breaker

> status: complete
> source-dirs: failsafe/breaker.go, upstream/upstream_executor.go, upstream/upstream.go, data/cache_executor.go, data/failsafe.go, common/config.go, common/defaults.go, common/validation.go, common/errors.go, telemetry/metrics.go, erpc/network_executor.go, erpc/networks_test.go, erpc/networks_skip_test.go

## L1 — Capability (CTO view)

The circuit breaker is a per-upstream (and per-cache-connector) in-process state machine that automatically stops routing traffic to a failing upstream after a configurable failure-rate threshold is exceeded. It moves through three states — Closed (normal), Open (blocked), and HalfOpen (probing) — and self-heals after a configurable cooldown delay. This prevents cascading failures, reduces latency wasted on doomed requests, and allows traffic to rotate to healthy upstreams while the broken one recovers.

## L2 — Mechanics (staff-engineer view)

**Scope.** Circuit breakers are configured at the **upstream** failsafe scope only. The network-level `FailsafeConfig` explicitly rejects a `circuitBreaker` block at construction time (`erpc/network_executor.go:L68-L73`). Cache connectors support their own circuit breakers via `FailsafeConnector` (`data/failsafe.go:L96-L133`). The comment at `common/config.go:L1289-L1291` documents this convention.

**Per-method granularity.** An upstream's `failsafe` config is a list of `FailsafeConfig` entries, each with optional `matchMethod` and `matchFinality` filters. Each entry gets its own independent `upstreamExecutor`, each of which owns its own `*failsafe.Breaker`. This allows method-specific breakers: e.g. `eth_getLogs` can trip independently of `eth_call` (`upstream/upstream_executor.go:L38-L76`). The `CircuitBreakerState()` method on `Upstream` surfaces only the catch-all (`matchMethod: "*"`, no finality filter) executor's breaker for UI/diagnostics (`upstream/upstream.go:L320-L333`).

**Ring-buffer window.** The breaker maintains a circular ring buffer of the last N outcomes (true = failure, false = success). N is `FailureThresholdCapacity`. The buffer is pre-allocated at `NewBreaker` time with capacity `max(FailureThresholdCapacity, FailureThresholdCount, 1)` (`failsafe/breaker.go:L107-L125`). Each `Record` call pushes into the buffer, evicting the oldest entry (and adjusting the `failures`/`successes` counters) when full — O(1) for all operations.

**Open condition (Closed → Open).** After every `OutcomeSuccess` or `OutcomeFailure` in the Closed state, `checkOpenLocked()` evaluates: if `count < FailureThresholdCapacity` (window not full), do nothing. Once the window fills, if `failures >= FailureThresholdCount`, transition to Open and record `openedAt = time.Now()` (`failsafe/breaker.go:L280-L300`). The semantic is a **count within a rolling window**, not a rate — the window must be full before tripping.

**Half-open delay.** Transitions Open → HalfOpen only when `time.Since(openedAt) >= HalfOpenAfter` and a caller calls `TryAcquirePermit()`. If the delay has not elapsed, `TryAcquirePermit()` returns false and the caller immediately gets `ErrCircuitOpen` (`failsafe/breaker.go:L158-L176`).

**Half-open probing.** HalfOpen grants at most `max(SuccessThresholdCapacity, SuccessThresholdCount, 1)` concurrent trial permits via `halfOpenInflight`. When `halfOpenSuccess + halfOpenFailure >= SuccessThresholdCapacity` (trial window full), the breaker decides: if `halfOpenSuccess >= SuccessThresholdCount` → Close; otherwise → re-Open with fresh `openedAt`. A single failure *before* the trial window fills also immediately re-opens via the early-exit branch at `failsafe/breaker.go:L232-L237` (`Record` path).

**What counts as a failure.** The `upstreamBreakerOutcome` function at `upstream/upstream_executor.go:L391-L421` classifies (response, error) pairs:
- **OutcomeFailure**: `ErrCodeEndpointServerSideException`, `ErrCodeEndpointTransportFailure`, `ErrCodeEndpointUnauthorized`, `ErrCodeEndpointBillingIssue` (upstream 5xx, transport failure, auth failure, billing issue), and a syncing upstream that returned an empty response (`EvmSyncingStateSyncing && resp.IsResultEmptyish()`).
- **OutcomeIgnore**: `ErrCodeEndpointRequestCanceled`, `ErrCodeUpstreamRequestSkipped`, all other errors (rate limits, timeout, capacity, client-side errors, missing data, execution exceptions, hedge cancellations).
- **OutcomeSuccess**: all non-error, non-syncing-empty responses.

**What is excluded from the breaker entirely.** Hedge attempts (`isHedge == true`), internal probes (`req.IsInternal()`), and composite requests (`req.IsCompositeRequest()`) all bypass `upstreamBreakerEligible` → `false` → no `TryAcquirePermit` call, no `Record` call (`upstream/upstream_executor.go:L368-L385`).

**Cache connector breaker.** Uses `cacheBreakerOutcome`: only transport errors (network I/O failures, gRPC `Unavailable`/`DeadlineExceeded`/`Aborted`, connection strings, Redis sentinel strings) count as `OutcomeFailure`. `RecordNotFound`, `RecordExpired`, and context cancellations are `OutcomeIgnore`. All other successes close (`data/cache_executor.go:L220-L234`, `data/failsafe.go:L21-L92`).

**Interaction with selection.** When the breaker refuses a permit, `callBreakerWithTimeout` returns `ErrFailsafeCircuitBreakerOpen` immediately without touching the transport. The upstream's `classifyUpstreamOutcome` maps this to `UpstreamOutcomeBreakerOpen` (`upstream/upstream.go:L47-L48`), which is recorded in `ExecState`. The network-level routing loop sees this error from the upstream `Forward` call and, because `ErrCodeFailsafeCircuitBreakerOpen` is listed in `IsRetryableTowardsUpstream` as **not retryable** (`common/errors.go:L2458-L2459`), it rotates to the next available upstream rather than retrying the same one. Integration test at `erpc/networks_skip_test.go:L186-L262` confirms rpc1's breaker-open causes network to serve rpc2 without dialing rpc1.

**Cordoning vs circuit breaker.** These are orthogonal mechanisms. Cordoning (`health.Tracker.Cordon/Uncordon`) is an operator-controlled flag that permanently removes an upstream from the selection policy until explicitly lifted. The circuit breaker is automatic and self-healing. The health tracker has no awareness of breaker state; breaker state has no awareness of cordon state. Both can be simultaneously active.

**OnTransition hook.** After every state machine transition, `transitionLocked` fires the `OnTransition` callback in a goroutine (to avoid holding the mutex) (`failsafe/breaker.go:L312-L334`). At `NewUpstream` time this is wired to `makeBreakerTransitionHook` which increments `erpc_upstream_breaker_state_change_total` (`upstream/upstream.go:L87-L99`, `upstream/upstream.go:L158-L162`). Cache executor breakers do NOT wire an `OnTransition` hook; no metric fires for cache breaker transitions.

## L3 — Exhaustive reference (agent view)

### Config fields

All fields live under `upstreams[*].failsafe[*].circuitBreaker`. The config struct is `CircuitBreakerPolicyConfig` at `common/config.go:L1381-L1387`. Defaults applied by `SetDefaults` at `common/defaults.go:L2342-L2387`.

| # | YAML path (relative to `...failsafe[*]`) | Type | Default | Valid range / constraint | Behavior |
|---|------------------------------------------|------|---------|--------------------------|----------|
| 1 | `circuitBreaker.failureThresholdCount` | `uint` | `20` (`common/defaults.go:L2343-L2348`) | `>0`; must be `<= failureThresholdCapacity` (validated at `common/validation.go:L1044-L1048`) | Number of failures within the rolling window that trips the breaker. The window must first fill to `failureThresholdCapacity` entries before this count is checked. |
| 2 | `circuitBreaker.failureThresholdCapacity` | `uint` | `80` (`common/defaults.go:L2350-L2355`) | `>0` | Size of the rolling window (ring buffer). The breaker does not evaluate the open condition until exactly this many outcomes have been recorded since last reset. Ring buffer allocation is `max(capacity, count, 1)` (`failsafe/breaker.go:L111-L117`). |
| 3 | `circuitBreaker.halfOpenAfter` | `Duration` | `5m` (`common/defaults.go:L2364-L2369`) | `>0`; required — validation errors if zero (`common/validation.go:L1038-L1040`) | How long the breaker stays Open before allowing a probe. Measured from `openedAt` which is set at the moment of tripping and again when a HalfOpen trial fails. |
| 4 | `circuitBreaker.successThresholdCount` | `uint` | `8` (`common/defaults.go:L2371-L2376`) | `>0`; must be `<= successThresholdCapacity` (`common/validation.go:L1056-L1058`) | Number of successes in the HalfOpen trial window required to re-close the breaker. |
| 5 | `circuitBreaker.successThresholdCapacity` | `uint` | **`200`** (`common/defaults.go:L2357-L2362` — first `if c.SuccessThresholdCapacity == 0` block; the second identical guard at L2378-L2384 sets `10` but is unreachable dead code) | `>0` | Size of the HalfOpen trial window. Controls how many concurrent permits are issued in HalfOpen (`TryAcquirePermit` blocks at `halfOpenInflight >= successThresholdCapacity`). The trial window evaluates once `halfOpenSuccess + halfOpenFailure >= successThresholdCapacity`. |

**Known dead-code bug — `successThresholdCapacity` duplicate defaults block.** `SetDefaults` at `common/defaults.go:L2357-L2384` contains two separate `if c.SuccessThresholdCapacity == 0` guards for the same field. The first block (L2357-L2363) sets the value to `200` (or the parent default). Because the field is now non-zero, the second identical guard (L2378-L2384) evaluates to `false` and its body — which would set the value to `10` — **is never executed**. The second block is unreachable dead code. **Effective default is `200`, not `10`.** The `SuccessThresholdCount` default of `8` (L2371-L2376) is sandwiched between the two capacity blocks, which is almost certainly the root cause: the second capacity block was likely intended to run after `SuccessThresholdCount` was set (perhaps to enforce `capacity >= count`), but the guard condition makes it a no-op. This discrepancy should be treated as a bug in the defaults logic, not intentional behavior.

**Network-scope restriction.** Setting `circuitBreaker` in a network-level `failsafe` block causes `NewNetworkExecutor` to return `ErrFailsafeConfiguration` with message `"circuit breaker does not make sense for network-level requests"` (`erpc/network_executor.go:L68-L73`). The process will not start.

**Minimum viable config (explicit, no defaults):**
```yaml
failsafe:
  - matchMethod: "*"
    circuitBreaker:
      failureThresholdCount: 5
      failureThresholdCapacity: 10
      halfOpenAfter: 30s
      successThresholdCount: 2
      successThresholdCapacity: 4
```

### Behaviors & algorithms

**State machine transitions:**

```
Closed  --[ failures >= failureThresholdCount, after window fills ]-->  Open
Open    --[ time.Since(openedAt) >= halfOpenAfter, on TryAcquirePermit ]-->  HalfOpen
HalfOpen --[ halfOpenSuccess >= successThresholdCount, after trial fills ]--> Closed
HalfOpen --[ any failure OR trial fills but successCount < threshold ]--> Open (openedAt reset)
```

**`TryAcquirePermit()` logic (caller asks: "may I proceed?"):**
1. `StateClosed`: always `true` (no lock needed — atomic load only).
2. `StateHalfOpen`: take `mu`, re-check state (race guard), compute `cap = max(SuccessThresholdCapacity, SuccessThresholdCount, 1)`, return `halfOpenInflight < cap`; if permitted, increment `halfOpenInflight` (`failsafe/breaker.go:L139-L157`).
3. `StateOpen`: take `mu`, re-check state (race guard with recursive re-evaluate), check `time.Since(openedAt) >= HalfOpenAfter`; if elapsed → `transitionLocked(HalfOpen, "half_open_delay_elapsed")`, set `halfOpenInflight = 1`, return `true`; otherwise return `false` (`failsafe/breaker.go:L158-L176`).

**`Record(outcome)` logic (caller reports what happened):**
- `OutcomeIgnore`: no-op (returns immediately).
- **Closed**: `pushLocked(isFailure)`, increment lifetime counters, call `checkOpenLocked()`. `checkOpenLocked` opens if `b.count >= failCap && b.failures >= failCount` (`failsafe/breaker.go:L280-L300`).
- **HalfOpen**: decrement `halfOpenInflight`, increment trial success/failure counter. Two evaluation paths:
  - If `halfOpenSuccess + halfOpenFailure >= successCap` (trial window full): success count sufficient → `resetWindowLocked(); transitionLocked(Closed, "half_open_success_threshold")`; insufficient → re-open.
  - If `OutcomeFailure` and `halfOpenFailure > 0` (first failure): immediately re-open even if window not yet full (`failsafe/breaker.go:L232-L237`).
- **Open**: only increments lifetime counters (caller bypassed `TryAcquirePermit`; should not normally happen) (`failsafe/breaker.go:L239-L247`).

**`resetWindowLocked()`**: zeros all ring buffer entries, resets `head=0`, `count=0`, `failures=0`, `successes=0`. Called on Open-trip (clears stale window) and on successful HalfOpen close (fresh slate for re-opened circuit) (`failsafe/breaker.go:L302-L310`).

**Concurrent permit cap in HalfOpen.** At most `max(SuccessThresholdCapacity, SuccessThresholdCount, 1)` in-flight probes. The cap is checked and `halfOpenInflight` is incremented together under `mu`, so no overshooting is possible. Each `Record` in HalfOpen decrements `halfOpenInflight`, keeping the semaphore balanced.

**Error produced when Open.** `NewErrFailsafeCircuitBreakerOpen(common.ScopeUpstream, failsafe.ErrCircuitOpen, &startTime)` — a `*ErrFailsafeCircuitBreakerOpen` wrapping `errCircuitOpen{}` with error code `"ErrFailsafeCircuitBreakerOpen"`, scope field `"upstream"`, and `durationMs` detail (`common/errors.go:L1665-L1692`). The cache connector uses `common.ScopeConnector` (`data/cache_executor.go:L192`).

**Retryability.** `IsRetryableTowardsUpstream` returns `false` for `ErrCodeFailsafeCircuitBreakerOpen` — same-upstream retry is never attempted when the breaker is open (`common/errors.go:L2458-L2459`). `IsRetryableTowardNetwork` returns `true` (default) because `ErrFailsafeCircuitBreakerOpen` does not set the `retryableTowardNetwork=false` detail field — so the network routing loop will try the next upstream.

**`classifyUpstreamOutcome` (internal outcome labeling for metrics):**
Mapping at `upstream/upstream.go:L36-L78`. When `ErrCodeFailsafeCircuitBreakerOpen` is present → `UpstreamOutcomeBreakerOpen` = `"breaker_open"`. This feeds `erpc_upstream_attempt_outcome_total`.

**`ErrUpstreamsExhausted` aggregation.** When all upstreams are exhausted and some returned `ErrCodeFailsafeCircuitBreakerOpen`, the error message includes `"N upstream circuit breaker open"` (`common/errors.go:L1054-L1055`).

**`upstreamBreakerEligible` exclusion rules** (`upstream/upstream_executor.go:L368-L385`):
- `isHedge == true` → excluded (hedge attempts must not penalize the breaker).
- `req.IsInternal()` → excluded (state-poller probes bypass the breaker in both directions).
- `req.IsCompositeRequest()` → excluded (batch fan-out sub-requests do not count).
- All other requests are eligible.

**Cache connector outcome classification** (`data/cache_executor.go:L220-L234`):
- `nil` error → `OutcomeSuccess`.
- `ErrCodeRecordNotFound` → `OutcomeIgnore` (cache miss is normal).
- `isTransportError(err)` → `OutcomeFailure` (network/gRPC transport failure).
- Anything else → `OutcomeIgnore`.

**`isTransportError` definition** (`data/failsafe.go:L21-L92`): `ErrCodeEndpointTransportFailure`; `net.Error` with `Timeout()`; `io.EOF`, `io.ErrUnexpectedEOF`, `syscall.ECONNREFUSED`, `syscall.ECONNRESET`, `syscall.EPIPE`, `syscall.ETIMEDOUT`; gRPC codes `Unavailable`, `DeadlineExceeded`, `Aborted`; string substrings: `"connection refused"`, `"connection reset"`, `"broken pipe"`, `"no such host"`, `"network is unreachable"`, `"tls handshake"`, `"i/o timeout"`, `"operation timed out"`, `"use of closed network connection"`, `"client is closed"`, `"unexpectedly closed"`, `"goaway"`, `"clusterdown"`, `"masterdown"`, `"tryagain"`, `"redis is loading"`.

**`CircuitBreakerState()` accessor** (`upstream/upstream.go:L320-L333`): scans `failsafeExecutors` for the catch-all entry (`MatchMethod() == "*" && len(MatchFinality()) == 0`), returns its `breaker.State()` via atomic load. Returns `StateClosed` if not found. Used by admin/simulator UI for the "circuit" pill indicator.

### Observability

**Metrics:**

| Metric | Type | Labels | When it fires | Source |
|--------|------|--------|---------------|--------|
| `erpc_upstream_breaker_state_change_total` | counter | `project`, `upstream`, `transition` | Every state transition. `transition` values: `closed_to_open`, `open_to_half_open`, `half_open_to_closed`, `half_open_to_open` | `telemetry/metrics.go:L485-L489`; wired at `upstream/upstream.go:L91-L99` |
| `erpc_upstream_attempt_outcome_total` | counter | `project`, `network`, `upstream`, `category`, `outcome`, `is_hedge`, `is_retry`, `finality` | Every attempt terminal; `outcome="breaker_open"` when breaker refused permit | `telemetry/metrics.go:L457-L462`; incremented at `upstream/upstream.go:L551-L560` |

Cache connector breakers do NOT emit `upstream_breaker_state_change_total` — the `OnTransition` hook is not wired for cache executors.

**Logs.** On every state transition, `transitionLocked` emits a `WARN`-level zerolog message (`failsafe/breaker.go:L318-L327`):
```
{"level":"warn","from":"closed","to":"open","reason":"failure_threshold","executions":N,"successes":N,"failures":N,"message":"circuit breaker state changed"}
```
Reason strings: `"failure_threshold"` (Closed→Open), `"half_open_delay_elapsed"` (Open→HalfOpen), `"half_open_success_threshold"` (HalfOpen→Closed), `"half_open_failure"` (HalfOpen→Open).

**Breaker `Metrics()` method** (`failsafe/breaker.go:L345-L350`): returns `(failures, successes, executions uint64)` as lifetime atomic counters. Used programmatically; not surfaced as a Prometheus gauge.

**Span attributes.** No dedicated span is created inside the circuit breaker itself. The surrounding `callBreakerWithTimeout` path is called within spans already created by `Upstream.Forward` → `upstreamExecutor.Run`.

### Edge cases & gotchas

1. **Window must be full before tripping.** If you set `failureThresholdCapacity=80` and `failureThresholdCount=1`, the breaker will NOT trip until 80 outcomes have been recorded — even if the first 79 were all failures. `checkOpenLocked` short-circuits with `if b.count < failCap { return }` (`failsafe/breaker.go:L292-L294`). To trip on the first failure, set both to `1`.

2. **`SuccessThresholdCapacity` effective default is `200`, not `10` — the second defaults block is unreachable dead code.** `SetDefaults` at `common/defaults.go:L2357-L2384` has two `if c.SuccessThresholdCapacity == 0` guards. The first (L2357-L2363) sets the value to `200` unconditionally (unless a parent default overrides). Because the field is now non-zero, the second identical guard (L2378-L2384) evaluates to `false` and the `10` value it contains is **never assigned**. Any documentation, census entry, or tooling that quotes the default as `10` is wrong. The effective default is `200`. This is a known discrepancy/bug in the defaults logic — the second block is dead code and the intended behavior (if any) is unclear.

3. **Single HalfOpen failure re-opens immediately.** Even before the trial window is full, any `OutcomeFailure` recorded in HalfOpen re-opens the circuit and resets `openedAt`. The operator must ensure `HalfOpenAfter` is long enough for the upstream to actually recover, or flapping will occur (`failsafe/breaker.go:L232-L237`).

4. **Hedge attempts are invisible to the breaker.** A hedge attempt that gets a 500 from the upstream does not count as a failure. Only the primary (non-hedge) attempt counts. This is intentional to avoid penalizing upstreams for aggressive hedging behavior.

5. **Internal probe calls bypass the breaker in both directions.** Health/state-poller probes (`req.IsInternal() == true`) never block on an open breaker and never record outcomes. An Open upstream will receive internal probes but user traffic is blocked.

6. **`circuitBreaker` at network scope causes startup failure.** Even if you only include `circuitBreaker: {}` in a network `failsafe` block, `NewNetworkExecutor` returns an error and eRPC will not start. This is enforced at `erpc/network_executor.go:L68-L73`.

7. **Per-method breakers are independent.** If you configure two `failsafe` entries — one for `eth_call` and one for `*` — they have separate `Breaker` instances. `eth_call` failures only trip the `eth_call` breaker; they don't affect the catch-all breaker. But `CircuitBreakerState()` on `Upstream` only exposes the `*` breaker state.

8. **Race safety in Open → HalfOpen transition.** `TryAcquirePermit` takes `mu`, re-checks state (atomic load inside lock), and handles the case where two goroutines race to transition: the second goroutine sees the state is no longer Open and recursively re-evaluates. This is safe: the recursion depth is bounded to one extra call (`failsafe/breaker.go:L160-L166`).

9. **`OnTransition` fires in a goroutine.** The metric increment is async. Under extremely high test parallelism this can appear as a missing metric increment immediately after a transition. The comment at `failsafe/breaker.go:L328-L331` warns callers: "must not recurse into the breaker."

10. **Cordon and circuit breaker are orthogonal.** A cordoned upstream also has its circuit breaker checked (if configured). The breaker can be open while cordon is also active — both independently prevent traffic. An uncordon does not reset the breaker.

11. **EVM syncing state triggers a failure.** If an upstream reports `EvmSyncingStateSyncing` AND returns an empty/null response, `upstreamBreakerOutcome` classifies it as `OutcomeFailure`. This means a syncing node can trip the breaker even without an error code (`upstream/upstream_executor.go:L412-L419`).

12. **`failsafe.circuitBreaker.halfOpenAfter` is required at validation.** Omitting `halfOpenAfter` from a `circuitBreaker` block in YAML will fail `Validate()` with `"failsafe.circuitBreaker.halfOpenAfter is required"` even though `SetDefaults` would have filled it. This only matters if you call `Validate()` before `SetDefaults()` — in practice eRPC's config loading calls `SetDefaults` first, then `Validate`.

13. **Window reset on every open/close.** Both tripping Closed→Open and closing HalfOpen→Closed call `resetWindowLocked()`. This means the failure window is always fresh after a recovery — a flapping upstream starts each cycle with a clean slate.

### Source map

- `failsafe/breaker.go` — Self-contained, policy-free circuit breaker state machine: ring buffer, `TryAcquirePermit`, `Record`, `transitionLocked`, `OnTransition` hook, `State`/`Metrics` accessors.
- `failsafe/doc.go` — Package-level doc comment for the failsafe package.
- `upstream/upstream_executor.go` — Wraps `*failsafe.Breaker` in `callBreakerWithTimeout`; implements `upstreamBreakerEligible` and `upstreamBreakerOutcome` classifiers; builds the executor from `FailsafeConfig`.
- `upstream/upstream.go` — Wires `OnTransition` to `makeBreakerTransitionHook` (metric); `classifyUpstreamOutcome` maps `ErrCodeFailsafeCircuitBreakerOpen` to `UpstreamOutcomeBreakerOpen`; `CircuitBreakerState()` exposes catch-all breaker state.
- `data/cache_executor.go` — Cache-scope equivalent of `upstreamExecutor`; `callBreaker` and `cacheBreakerOutcome` classifier.
- `data/failsafe.go` — `FailsafeConnector` wrapper for cache connectors; `isTransportError` helper; `scopeConnector` constant.
- `common/config.go:L1381-L1396` — `CircuitBreakerPolicyConfig` struct definition and `Copy()`.
- `common/defaults.go:L2342-L2387` — `CircuitBreakerPolicyConfig.SetDefaults()` with all defaults.
- `common/validation.go:L1037-L1059` — `CircuitBreakerPolicyConfig.Validate()` with all constraints.
- `common/errors.go:L1665-L1703` — `ErrFailsafeCircuitBreakerOpen` type, error code, constructor, and `DeepestMessage`.
- `common/errors.go:L2455-L2459` — `IsRetryableTowardsUpstream`: `ErrCodeFailsafeCircuitBreakerOpen` → not upstream-retryable.
- `common/errors.go:L962-L1055` — `ErrUpstreamsExhausted` aggregation includes circuit-breaker-open count in the message.
- `telemetry/metrics.go:L482-L489` — `MetricUpstreamBreakerStateChange` counter definition.
- `erpc/network_executor.go:L68-L73` — Enforces that `circuitBreaker` is not allowed at network scope.
- `erpc/networks_test.go:L6088-L6521` — Integration tests for circuit breaker: threshold tripping, half-open recovery after delay, and full open/close cycle.
- `erpc/networks_skip_test.go:L174-L262` — Integration test: BreakerOpen causes rotation to next upstream without dialing the failing one.
