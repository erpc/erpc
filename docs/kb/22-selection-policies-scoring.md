# KB: Upstream selection, scoring, and selection policies

> status: complete
> source-dirs: internal/policy, erpc/networks.go, erpc/networks_selection_policy_test.go, erpc/networks_selection_policy_realpoll_test.go, erpc/upstream_selection_test.go, common/config.go, common/defaults.go, telemetry/metrics.go, health/tracker.go

## L1 — Capability (CTO view)

The selection-policy engine is the per-project subsystem that decides **which upstreams serve each request, in what order**. On every tick (default 15 s) it runs a user-supplied or built-in JavaScript eval function against a snapshot of health-tracker metrics, producing a ranked list of upstreams per `(network, method, finality)` slot. The request path reads this pre-computed list in a wait-free atomic load, so routing overhead is near zero. The embedded default policy excludes unhealthy upstreams (by error rate, throttle rate, latency, and block-head lag), prefers the non-fallback tier, ranks survivors by p70 latency score, applies sticky-primary smoothing across ticks, and shadow-probes excluded upstreams in the background to enable metric-driven re-admission — all without an explicit re-admission timer.

## L2 — Mechanics (staff-engineer view)

### Overall architecture

The entry point is `policy.Engine`, which owns a map of `*Slot` values keyed by `(network, method, finality)`. Each slot has its own ticker goroutine; the request path ONLY reads `slot.cache` via `atomic.Pointer[[]common.Upstream].Load()` — completely wait-free.

When `Engine.RegisterNetwork` is called (at network bootstrap), the engine:
1. Upgrades the `EvalFunc` from the trivial `(upstreams, ctx) => upstreams` placeholder to `default_policy.js` (embedded at compile time via `//go:embed`). `internal/policy/default_policy.go:upgradeDefaultPolicy`.
2. Creates the wildcard `("*","*")` slot for the network and runs an initial **synchronous** tick so the first request sees a populated cache. `internal/policy/engine.go:RegisterNetwork:L305`.
3. Starts the slot's ticker goroutine for subsequent ticks.

Per-method and per-finality narrowing is controlled by `EvalScope` (`"network"` | `"network-method"` | `"network-finality"` | `"network-method-finality"`). Under the default `"network"` scope only the wildcard slot exists; narrower scopes lazy-create additional slots on first request. `internal/policy/engine.go:lookupSlotWithFallback`.

### Per-tick eval cycle (`tickOnce`)

Located at `internal/policy/slot.go:tickOnce`.

1. **Metric snapshot** — `snapshotMetrics(tracker, ups, method, finality)` builds three Go maps:
   - `local`: `u.metrics` (slot-local, method + finality specific)
   - `acrossMethods`: `u.metricsAcrossMethods` (wildcard `"*"/All` aggregate used by cross-slot `stickyPrimary`)
   - `byMethod`: `u.metricsByMethod[method]` (per-method for `latencyDeviationAbove`)
   `internal/policy/slot.go:snapshotMetrics:L614`.

2. **EvalContext** — `buildEvalContext` assembles `{network, method, finality, now, previousOrder, lastSwitchAt, tickCount}`. `internal/policy/eval.go:buildEvalContext:L26`.

3. **Sobek eval** — the engine acquires a pooled sobek VM (JS runtime), installs per-tick globals (`__policyCtx`, `__policyAllUpstreams`, `__policyLeafReasons`, etc.), calls the compiled JS function `(upstreams, ctx) => ...`, and drains results. Runs in a goroutine guarded by an optional timeout (`EvalTimeout`, default 100 ms). `internal/policy/eval.go:runEval`.

4. **Result materialization** — `materializeOrder` maps JS-returned IDs back to `common.Upstream` pointers, recording which upstreams were excluded. `internal/policy/slot.go:materializeOrder:L739`.

5. **Atomic cache swap** — `slot.cache.Store(&ordered)` and `slot.excludedCache.Store(&excludedUps)`. Both are `atomic.Pointer`; readers hold no locks. `internal/policy/slot.go:L308-311`.

6. **Probe reconciliation** — if the eval emitted `__probeConfig` (from `probeExcluded(opts)`), the engine reconciles the network's `Prober`: lazy-create, hot-swap config, or tear down. `internal/policy/engine.go:reconcileProbeConfig:L477`.

7. **Cross-tick state update** — `tickCount`, `previousOrder`, `lastSwitchAt`, `excludedSince` map, `lastScores`, decision ring are updated under `slot.mu`. `internal/policy/slot.go:L319-347`.

### JS stdlib chainable methods

All methods are installed on `Array.prototype` inside sobek VMs by `internal/policy/stdlib/install.go`. Source at `internal/policy/stdlib/stdlib.js`. The chain is purely functional — each step returns a new array (or the same slice), enabling method chaining.

**Identity/label filters**: `byId`, `excludeId`, `byTag`, `excludeTag`, `byVendor`, `excludeVendor`, `byType`, `where`, `whereNot`.

**Health filters**: `removeCordoned`, `removeByErrorRate`, `removeByThrottling`, `removeByMisbehavior`, `removeByLag`, `removeByLatency`, `keepHealthy`, `removeByMinRequests`.

**Predicate-driven exclusion**: `excludeIf(predicate)`, `shadowExcludeIf(predicate)`. `excludeIf` drops upstreams matching the predicate AND records leaf-slug attribution in `__policyLeafReasons[id]` for Prometheus. `shadowExcludeIf` records into `__policyShadowReasons[id]` but does NOT drop — used to audition new rules in production.

**Predicate factories** (all are global JS functions):
- `errorRateAbove(rate)`, `errorRateBelow(rate)`
- `throttleRateAbove(rate)`, `throttleRateBelow(rate)`
- `misbehaviorRateAbove(rate)`
- `latencyAbove(ms, quantile?)` — absolute latency threshold at given quantile (default p70)
- `latencyDeviationAbove(multiplier, opts?)` — relative peer comparison, per-method, with geomean/majority/veto modes and exponential damping
- `blockNumberLagAbove(blocks)`, `finalizationLagAbove(blocks)`, `blockSecondsLagAbove(seconds)`, `finalizationSecondsLagAbove(seconds)`
- `samplesAbove(n)`, `samplesBelow(n)` — sample-count guards (marked `isGuard=true` so they don't pollute exclusion metric labels)
- `all(...)`, `any(...)`, `not(pred)` — logical combinators with leaf attribution

**Scoring**: `sortByScore(base, opts?)` — computes `score = overall / (1 + Σ metricᵢ × weightᵢ)`. Descending sort; alphabetical upstream-ID as stable tiebreak. Three presets: `PREFER_FASTEST`, `PREFER_FRESHEST`, `PREFER_LEAST_ERRORS`.

**Stability**: `stickyPrimary(opts?)` — prevents unnecessary primary flapping across ticks.

**Tier selection**: `preferTag(pat, opts?)`, `preferVendor(name, opts?)`.

**Diversity**: `spreadAcrossTags(prefix)` — interleaves sorted list so adjacent positions come from different tag-partitions.

**Safety net**: `whenEmpty(fn)` — falls back to `fn()` if the array is empty.

**Probe**: `probeExcluded(opts)` — deposits `__probeConfig` so the engine knows to run the shadow-probe Prober.

**Conditional/finality**: `when(mask, fn)`, `byFinality(handlers)`, `if`, `unless`, `fallbackTo`, `ensureMin`.

### Default policy chain

`internal/policy/default_policy.js`:
```
removeCordoned()
.excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
.excludeIf(all(samplesAbove(10), throttleRateAbove(0.4)))
.excludeIf(any(
    all(samplesAbove(20), latencyAbove(3000), latencyDeviationAbove(3, { mode: 'majority' })),
    latencyAbove(10_000)
))
.excludeIf(any(blockNumberLagAbove(16), blockSecondsLagAbove(30)))
.whenEmpty(() => upstreams)                          // safety net
.preferTag('!tier:fallback', { minHealthy: 1, fallback: 'tier:fallback' })
.sortByScore(PREFER_FASTEST)
.stickyPrimary({ hysteresis: 0.30, minSwitchInterval: '30s' })
.probeExcluded({ sampleRate: 0.1, minSamples: 10, minSamplesWindow: '60s', maxConcurrent: 4, timeout: '10s' })
```

### Score formula

```
score(u) = overall / (1 + Σ metricᵢ(u) × weightᵢ)
```

- `overall` default = 1; `u.scoreMultipliers.overall` overrides it.
- Metric weights for `PREFER_FASTEST`: `{errorRate:4, respLatency:15, throttledRate:4, blockHeadLag:1, finalizationLag:0, misbehaviors:2}`.
- `respLatency` reads `u.metrics.p70ResponseSeconds` (configurable via `sortByScore({latencyQuantile:'p90'})`).
- Clean upstream (zero penalty) scores `1.0`; every unit of penalty divides the score.
- `sortByScore` sets `u.score` on the JS object; the Go side extracts it via `extractOrderedResult` and stores it in `slot.lastScores`. Exposed via `Engine.GetScores`.

### stickyPrimary mechanics

Located at `internal/policy/stdlib/stdlib.js:L724`.

Hold logic:
- If the current head is still the previous primary → confirm, return unchanged.
- If previous primary not in survivors → fall-through (method-disagreement escape hatch, does NOT update shared register).
- If `lastSwitchAt != null && (ctx.now - lastSwitchAt) < minSwitchMs` → hold (cooldown).
- If `curScore > prevScore × (1 + hysteresis)` → switch; otherwise → hold.

`scope` parameter controls the shared register key:
- `"network"` (default) — all slots for this network agree on one primary, scored from `u.metricsAcrossMethods` (`PREFER_FASTEST` wildcard weights). Stored in engine's `stickyStore` via `__getSharedSticky` / `__setSharedSticky` Go helpers injected at eval time.
- `"network-method"` — one primary per method.
- `"network-finality"` — one primary per finality bucket.
- `"network-method-finality"` — per slot; reads `u.score` from the slot's own `sortByScore`.

When sticky holds, it sets `__policyStickyHeld = true`; Go side reads this and emits `selection_sticky_hold_total`. `internal/policy/slot.go:L569`.

### Shadow-probe subsystem (`Prober`)

`internal/policy/prober.go`. One `Prober` per network, lazy-created when any slot's eval deposits a non-nil `__probeConfig`.

Flow:
1. `Engine.PublishRequest(networkID, req)` is called from `networks.go:L1074` after a successful upstream selection (not on cache hits). Non-blocking — drops to `selection_probe_dropped_total` on channel overflow.
2. The Prober's background goroutine dispatches goroutines for each currently-excluded upstream (`Engine.GetExcluded`):
   - Skips if `routing.probe == "off"` on the upstream.
   - Skips write methods (safety: mutations must not be mirrored).
   - Skips if in-flight count ≥ `MaxConcurrent`.
   - Skips based on `sampleRate` unless `windows[id].count < MinSamples` (then bypass the rate gate).
   - Fires the request against the excluded upstream using the same network `Forward` path.
3. Results feed the same tracker counters as real traffic → upstream re-admission is purely metric-driven.

Per-upstream opt-out: `routing.probe: "off"` (`common.ProbeModeOff`). `common/config.go:L787`.

### scoreMultipliers resolution

`internal/policy/eval.go:resolveScoreMultipliers`. Called once per upstream per tick; the first matching entry in `u.Routing.ScoreMultipliers` wins. Matcher semantics:
- `network` / `method`: glob via `common.WildcardMatch`; empty / `"*"` = any.
- `finality`: membership check against `[]DataFinalityState`; empty = any.
- A specific-method entry only matches when the slot runs with a specific method (requires `evalScope: network-method` or finer). In the wildcard slot `ctx.method = "*"` so `Method: "eth_getLogs"` never fires.

Merge modes (set via `sortByScore({ multipliers: 'merge' | 'override' | 'off' })`):
- `"merge"` (default): per-upstream keys override matching base-weight keys; unset keys inherit base; `overall` lifts the final score multiplicatively.
- `"override"`: configured upstreams rank by THEIR weights only; unconfigured fall back to base.
- `"off"`: ignore `u.scoreMultipliers` entirely.

### Request path integration

`erpc/networks.go:Forward:L1023-L1074`:
```
upsList = policyEngine.GetOrdered(networkId, method, req.Finality(ctx).String())
```
If `len(upsList) == 0` (engine not yet ticked) → falls back to raw registry order. `networks.go:L1032-1038`.

After dispatch, `policyEngine.PublishRequest` feeds the probe bus.

The ordered list is passed to `req.SetUpstreams(upsList)`; the failsafe layer (hedge/retry) walks through the list sequentially, never selecting the same upstream twice per execution (`req.ConsumedUpstreams` / `req.ErrorsByUpstream` sync.Maps track what has been tried). `erpc/upstream_selection_test.go` verifies centralized selection across hedge and retry paths.

### Idle slot eviction

Narrow per-(method, finality) slots that haven't been read in `idleEvictionAfter` (default 1 hour, `DefaultEngineIdleEvictionAfter`) are stopped and removed. The wildcard `("*","*")` slot is exempt. The `stickyStore` uses the same threshold. `internal/policy/engine.go:sweepIdleSlots:L214`.

---

## L3 — Exhaustive reference (agent view)

### Config fields

#### `selectionPolicy` (on `NetworkConfig`)

Full YAML path: `networks[].selectionPolicy`  
Type: `*SelectionPolicyConfig`  
Default: if omitted, `SetDefaults` creates a config with all sub-fields defaulted. `common/defaults.go:L1436`.

| Field | YAML key | Type | Default | Notes |
|---|---|---|---|---|
| `EvalInterval` | `evalInterval` | `Duration` | `15s` | Ticker period for the slot. `common/defaults.go:L2532`. Zero disables ticker (frozen; tests use this). |
| `EvalTimeout` | `evalTimeout` | `Duration` | `100ms` | JS eval deadline per tick. Exceeded → error logged, cache unchanged. `common/defaults.go:L2534-2535`. |
| `EvalScope` | `evalScope` | `EvalScope` enum | `"network"` | `"network"` \| `"network-method"` \| `"network-finality"` \| `"network-method-finality"`. After `SetDefaults`, `EvalPerMethod`/`EvalPerFinality` are niled. `common/defaults.go:L2556-2583`. |
| `EvalPerMethod` | `evalPerMethod` | `*bool` | `nil` (key absent) | **Deprecated alias.** Pointer-typed so `SetDefaults` can distinguish "key absent" (`nil`) from "explicitly false" (`*false`) from "explicitly true" (`*true`). Accepted values: `true` or `false`. Translation: `nil` → no effect; `*true` → `evalScope: network-method` (or `network-method-finality` when both bools are true); `*false` → treated as "key was present but false", i.e. no-method axis — same as absent for scope derivation. After `SetDefaults` runs the field is **niled unconditionally**; downstream code only reads `EvalScope`. Excluded from TS public surface (`tstype:"-"`). `common/config.go:L2366-2372`, `common/defaults.go:L2551-2583`. |
| `EvalPerFinality` | `evalPerFinality` | `*bool` | `nil` (key absent) | **Deprecated alias for the finality axis.** Same pointer-bool mechanics as `evalPerMethod`. Accepted values: `true` or `false`. Translation: `*true` → `evalScope: network-finality` (or `network-method-finality` when both bools are true). After `SetDefaults` runs the field is **niled unconditionally**. `common/config.go:L2373-2375`, `common/defaults.go:L2551-2583`. |
| `EvalFunc` | `evalFunc` | `string` (JS source or TS sentinel) | `DefaultSelectionPolicySource` placeholder → upgraded to `default_policy.js` at engine register time | Full JS function `(upstreams, ctx) => Upstream[]`. In TS configs a sentinel ID references a compiled arrow function. `common/defaults.go:L2586-2609`. |

#### Legacy `selectionPolicy` fields (`evalFunction`, `resampleExcluded`, `resampleInterval`, `resampleCount`)

These four keys are accepted by `SelectionPolicyConfig.UnmarshalYAML` (via a shadow struct that inlines `LegacySelectionPolicyFields`) and stashed into `SelectionPolicyConfig.LegacySelectionPolicy`. They are **never stored on the canonical config struct** — they are removed at config-load time by the legacy translator (`common/legacy.TranslateFromConfig`), which runs immediately after all `SetDefaults` hooks complete.

| YAML key | Go field | Type | Zero/absent value | Behavior |
|---|---|---|---|---|
| `evalFunction` | `LegacySelectionPolicyFields.EvalFunction` | `string` | `""` (empty string = absent) | The old-shape eval function: `(upstreams, method) => Upstream[]`. When non-empty and no canonical `evalFunc` is also set, `wrapLegacyEvalFunction` wraps it into the new shape: `(upstreams, ctx) => { const __legacyFn = <source>; const __sorted = upstreams.sortByScore(PREFER_FASTEST); let result = __legacyFn(__sorted, ctx.method); return result; }`. If `resampleExcluded=true && resampleInterval>0`, `.probeExcluded({ sampleRate: 1.0, maxConcurrent: 1, timeout: '10s' })` is appended. If `evalFunc` IS also set on the same block, the legacy function is **ignored** (canonical wins). A deprecation warning is emitted via `WarnLegacySelectionPolicy()`. `common/legacy/eval_synthesis.go:L8-47`, `common/legacy/warnings.go:L26-31`. |
| `resampleExcluded` | `LegacySelectionPolicyFields.ResampleExcluded` | `bool` | `false` | When `true` AND `resampleInterval > 0`, causes the legacy translator to append `.probeExcluded({ sampleRate: 1.0, maxConcurrent: 1, timeout: '10s' })` to the synthesized eval (either the wrapped `evalFunction` or the synthesized `sortByScore` chain). The legacy `ResampleInterval` value is captured but **not used** — the new `probeExcluded` primitive is sample-driven, not time-driven; `maxConcurrent: 1` preserves the legacy "one at a time" cadence. A deprecation warning is emitted via `WarnResampleExcluded()`. `common/legacy/eval_synthesis.go:L33-43`, `common/legacy/warnings.go:L33-38`. |
| `resampleInterval` | `LegacySelectionPolicyFields.ResampleInterval` | `Duration` | `0` (zero duration = absent) | The legacy polling interval for re-admitting excluded upstreams. **No behavioral mapping** in the new system. Checked only to gate whether `resampleExcluded` appends `probeExcluded` (condition: `resampleExcluded && resampleInterval > 0`). The actual duration value is explicitly ignored (`_ = sp.ResampleInterval`). `common/legacy/eval_synthesis.go:L33,L41`. |
| `resampleCount` | `LegacySelectionPolicyFields.ResampleCount` | `int` | `0` | **No behavioral mapping.** The legacy count of samples to collect before re-admitting. Checked in `hasSemanticLegacy` (non-zero triggers legacy translation path), but never used in synthesized eval output. `common/legacy/translate.go:L146-149`. |

**Migration behavior summary**: the legacy translator (`common/legacy`) runs once at config-load time, inspects `LegacySelectionPolicy`, synthesizes or wraps a modern `EvalFunc`, clears the stash by setting `LegacySelectionPolicy = nil`, then returns deprecation warning strings that the caller logs. None of the four legacy keys survive to the policy engine runtime. `common/legacy/translate.go:L318-328`.

#### `upstream.routing` (`UpstreamRoutingConfig`)

Full YAML path: `upstreams[].routing`  
Type: `*UpstreamRoutingConfig`  
Default: nil (no routing overrides). Inherited from `upstreamDefaults.routing` all-or-nothing at config load (`ApplyDefaults`). `common/config.go:L754`.

| Field | YAML key | Type | Default | Notes |
|---|---|---|---|---|
| `ScoreMultipliers` | `scoreMultipliers` | `[]*ScoreMultiplierConfig` | nil | First matching entry wins. Matcher fields empty/`"*"` = any. |
| `ScoreLatencyQuantile` | `scoreLatencyQuantile` | `float64` | 0 (inherits policy `sortByScore` default p70) | If set, overrides the latency quantile used when scoring THIS upstream. Not yet wired into the engine (kept for future use). |
| `Probe` | `probe` | `ProbeMode` | `"on"` | `"on"` = allow shadow-probe traffic; `"off"` = never mirror. `common/config.go:L773`. |

#### `ScoreMultiplierConfig` fields

| Field | YAML key | Type | Notes |
|---|---|---|---|
| `Network` | `network` | `string` | Glob; empty = any |
| `Method` | `method` | `string` | Glob; empty = any; specific methods only match in per-method slots |
| `Finality` | `finality` | `[]DataFinalityState` | Membership; empty = any. **See finality value duality note below.** |
| `Overall` | `overall` | `*float64` | Multiplies final score (>1 prefer, <1 avoid) |
| `ErrorRate` | `errorRate` | `*float64` | Weight for error-rate term |
| `RespLatency` | `respLatency` | `*float64` | Weight for latency term |
| `ThrottledRate` | `throttledRate` | `*float64` | Weight for throttle-rate term |
| `BlockHeadLag` | `blockHeadLag` | `*float64` | Weight for block-head-lag term |
| `FinalizationLag` | `finalizationLag` | `*float64` | Weight for finalization-lag term |
| `Misbehaviors` | `misbehaviors` | `*float64` | Weight for misbehavior-rate term |
| `TotalRequests` | `totalRequests` | `*float64` | `nil` (unset) | **Accepted silently; has zero runtime effect.** Preserved for backward compatibility so existing YAML configs that set `totalRequests` do not fail validation. The field is declared in `ScoreMultiplierConfig` but is **never read by any scoring code**. `resolveScoreMultipliers` (`internal/policy/eval.go`) only transfers `Overall`, `ErrorRate`, `RespLatency`, `ThrottledRate`, `BlockHeadLag`, `FinalizationLag`, `Misbehaviors` into the JS `u.scoreMultipliers` map; `TotalRequests` is deliberately omitted because the score formula operates on rolling-window rates, not absolute request counts. `common/config.go:L808-811`. |

Pointer fields: `nil` = unset (inherit base). `0.0` = explicitly zero (removes that metric's contribution). `common/config.go:L797-812`.

##### scoreMultipliers.finality — YAML integer vs TS bit-flag duality

The `finality` field on `ScoreMultiplierConfig` is typed `[]DataFinalityState` — a Go integer enum. **This is NOT the same as the `FINALIZED/REALTIME/UNFINALIZED/UNKNOWN` bit-flag constants available inside JS `evalFunc` bodies.**

**YAML / TS struct config (integer enum)** — used in `scoreMultipliers[].finality` to select which finality buckets a multiplier entry matches:

| YAML string | TS constant (generated.ts) | Go const | Int value |
|---|---|---|---|
| `"finalized"` or `"0"` | `DataFinalityStateFinalized` | `DataFinalityStateFinalized` | `0` |
| `"unfinalized"` or `"1"` | `DataFinalityStateUnfinalized` | `DataFinalityStateUnfinalized` | `1` |
| `"realtime"` or `"2"` | `DataFinalityStateRealtime` | `DataFinalityStateRealtime` | `2` |
| `"unknown"` or `"3"` | `DataFinalityStateUnknown` | `DataFinalityStateUnknown` | `3` |

Sources: `common/data.go:L10-L27` (Go enum with `iota`), `common/data.go:L54-L76` (`UnmarshalYAML` accepting both string names and integer strings `"0"`–`"3"`), `typescript/config/src/generated.ts:L1456-L1487`.

**JS `evalFunc` global constants (bit-flags)** — used with `.when(mask, fn)` and `byFinality(handlers)` inside the eval function body. These are bitwise masks installed by `internal/policy/stdlib/install.go:L74-L77`:

| JS global | Bit value | Purpose |
|---|---|---|
| `REALTIME` | `1` (1<<0) | Bit-mask for realtime finality |
| `UNFINALIZED` | `2` (1<<1) | Bit-mask for unfinalized finality |
| `FINALIZED` | `4` (1<<2) | Bit-mask for finalized finality |
| `UNKNOWN` | `8` (1<<3) | Bit-mask for unknown finality |

The integer ordering differs from the Go enum order (Go: Finalized=0, Unfinalized=1, Realtime=2, Unknown=3; JS bitmask: Realtime=1, Unfinalized=2, Finalized=4, Unknown=8). This is intentional — bit-flags must be powers of two for OR-composability; the Go enum is an `iota` sequence with `Finalized` first because it is the safest default for zero-value. The two systems are used in completely different contexts and never substituted for each other. `common/data.go:L10-L15` explains the `Finalized=0` choice; `internal/policy/stdlib/install.go:L64-L82` explains the bit-flag choice.

#### Score preset weight tables

`internal/policy/stdlib/stdlib.js:L552-556`.

| Preset | errorRate | respLatency | throttledRate | blockHeadLag | finalizationLag | misbehaviors |
|---|---|---|---|---|---|---|
| `PREFER_FASTEST` | 4 | 15 | 4 | 1 | 0 | 2 |
| `PREFER_FRESHEST` | 4 | 2 | 2 | 15 | 8 | 3 |
| `PREFER_LEAST_ERRORS` | 15 | 2 | 6 | 2 | 1 | 12 |

#### `probeExcluded` options (JS)

Defaults from `internal/policy/eval.go:readProbeConfig:L793-799` and `internal/policy/stdlib/stdlib.js:L999-1009`.

| Option | Default | Notes |
|---|---|---|
| `sampleRate` | `0.1` | 0.0–1.0 probability per (request, excluded-upstream) of triggering a probe. Bypassed when `count < minSamples`. |
| `minSamples` | `10` | Per-upstream floor within `minSamplesWindow`. Below this, every request is a probe candidate. Should be ≥ `samplesAbove(N)` in the chain. |
| `minSamplesWindow` | `"60s"` | Rolling window for `minSamples`. String duration or numeric ms. |
| `maxConcurrent` | `4` | In-flight probe cap per excluded upstream. |
| `timeout` | `"10s"` | Per-probe deadline. Overrun → failure recorded in tracker. |

#### `stickyPrimary` options (JS)

Defaults from `internal/policy/stdlib/stdlib.js:L725-727`.

| Option | Default | Notes |
|---|---|---|
| `hysteresis` | `0.10` | Challenger must exceed incumbent score by `incumbent × hysteresis` fraction to trigger switch (default 10%; default policy uses 0.30 / 30%). |
| `minSwitchInterval` | `"30s"` (30 000 ms) | Cooldown after a switch; no switching during cooldown regardless of score gap. |
| `scope` | `"network"` | `"network"` \| `"network-method"` \| `"network-finality"` \| `"network-method-finality"`. Coarser scopes share the stickyStore key across multiple slots. |

#### `latencyDeviationAbove` options

`internal/policy/stdlib/stdlib.js:L1343-1358`.

| Option | Default | Notes |
|---|---|---|
| `quantile` | `70` | p50/p70/p90/p95/p99 |
| `mode` | `"geomean"` | `"geomean"` \| `"majority"` \| `"veto"` |
| `minMethodSamples` | `50` | Per-method sample floor; methods below this are skipped entirely |
| `dampingMs` | `30` | Exponential damping scale (ms). Ratio is multiplied by `1 - exp(-myLatency / dampingMs)`. Set to 0 to disable. |

### Behaviors & algorithms

#### EvalScope and slot key resolution

`internal/policy/engine.go:effectiveKey:L545`, `scopeAxes:L562`.

| EvalScope | Slot key | ctx.method | ctx.finality | Notes |
|---|---|---|---|---|
| `"network"` | `(network, "*", "*")` | `"*"` | `"unknown"` | Single slot per network; method/finality never influence ordering. **`scoreMultipliers` entries with a specific `Method` pattern (e.g. `"eth_getLogs"`) NEVER match** because `ctx.method == "*"` and `resolveScoreMultipliers` uses `WildcardMatch(entry.Method, "*")` — `"eth_getLogs"` does not glob-match `"*"`. Only entries with empty/`"*"` method match. |
| `"network-method"` | `(network, method, "*")` | actual method | `"unknown"` | One slot per observed method; finality ignored. `scoreMultipliers` method-specific entries match correctly here. |
| `"network-finality"` | `(network, "*", finality)` | `"*"` | actual finality string | One slot per finality bucket; method ignored. `ctx.method` is still `"*"` → **`scoreMultipliers` method-specific entries still NEVER match** in these slots. Only `scoreMultipliers.finality` matching is useful here. |
| `"network-method-finality"` | `(network, method, finality)` | actual method | actual finality string | One slot per (method, finality) pair. Both `scoreMultipliers.method` and `scoreMultipliers.finality` matching work correctly. |

Cold-start fallback: if the narrow slot exists but its cache is empty, `GetOrdered` returns the wildcard slot's cache. `internal/policy/engine.go:GetOrdered:L343-362`.

##### evalScope `"network"` (default) — scoreMultipliers.method never matches

This is the most important default-behavior consequence of `evalScope: network`. In the wildcard slot the engine always sets `ctx.method = "*"`. Inside `resolveScoreMultipliers` (`internal/policy/eval.go:L442`), the match condition for a `ScoreMultiplierConfig` entry is:

```
networkMatch = entry.Network == "" || WildcardMatch(entry.Network, upstream.NetworkId)
methodMatch  = entry.Method  == "" || WildcardMatch(entry.Method,  slotMethod)   // slotMethod = "*"
```

`WildcardMatch("eth_getLogs", "*")` returns `false` — a specific method pattern does NOT match the wildcard method. Only entries with `Method: ""` (empty) or `Method: "*"` are applied in the default `evalScope: network` configuration. Operators who configure per-method `scoreMultipliers` without also setting `evalScope: network-method` (or finer) will silently see those entries ignored at runtime. This is covered by edge case #2 and `erpc/eval_multipliers_test.go:TestResolveScoreMultipliers`.

##### EvalScope: cardinality and performance implications

Each slot has its own ticker goroutine (one goroutine per tick interval) and its own sobek JS evaluation cycle. Narrow scopes multiply the number of running tickers and JS evals.

- **`"network"` (default)**: 1 slot per network. One JS eval per tick for the whole network regardless of method/finality diversity. Lowest overhead. The `ctx.method` is always `"*"` inside the eval, so predicates that branch on method (e.g. `methodMatches(...)`) always see `"*"` and match all methods. `scoreMultipliers` entries with a specific `Method` field **never match** in the wildcard slot because `ctx.method == "*"` but `effectiveKey` sets `m = "*"` unless the slot's method is non-wildcard. `internal/policy/eval.go:resolveScoreMultipliers`.

- **`"network-method"`: N × methods observed** — a new slot is lazy-created the first time a request with that method is seen. Each unique method (e.g. `eth_call`, `eth_getLogs`, `eth_getTransactionReceipt`, …) becomes its own slot. Networks with many distinct callers can accumulate dozens of method-slots. Each survives until `idleEvictionAfter` (default 1 h) without traffic. `ctx.method` is set to the real method inside those slot evals. `scoreMultipliers.method` matching works correctly here.

- **`"network-finality"`: up to 4 slots per network** — one per finality bucket (`finalized`, `unfinalized`, `realtime`, `unknown`). Only buckets that receive traffic are created. `ctx.finality` is the real finality string in those slots; the wildcard slot still gets `"unknown"` (its default). `scoreMultipliers.finality` matching works correctly here.

- **`"network-method-finality"`: N × 4 potential slots per network** — highest cardinality, highest per-upstream tracking granularity, most ticker goroutines. Use only when per-method AND per-finality ranking isolation is needed.

Slot creation: first request with a new (method or finality) key pays one write-lock acquisition and starts the slot's ticker goroutine. Subsequent requests are wait-free RLock reads. The slot's cache is nil until the first tick completes; `GetOrdered` falls back to the wildcard slot cache during that window. `internal/policy/engine.go:lookupSlotWithFallback:L607-L654`.

#### Upstream JS object shape

Built by `buildJSUpstreams` at `internal/policy/eval.go:L312`. Fields available to JS eval:

| Field | Type | Source |
|---|---|---|
| `id` | `string` | `u.Id()` |
| `vendor` | `string` | `u.VendorName()` |
| `type` | `string` | `cfg.Type` |
| `tags` | `string[]` | `cfg.Tags` |
| `hasTag(tag)` / `is(tag)` | `(tag: string) => boolean` | Shared VM singleton |
| `metrics` | `UpstreamMetrics` | Slot-local metrics snapshot |
| `metricsAcrossMethods` | `UpstreamMetrics` | Wildcard aggregate `("*", All)` |
| `metricsByMethod` | `{[method]: {requestsTotal, p50ms, p70ms, p90ms, p95ms, p99ms}}` | Per-method, all-finalities |
| `scoreMultipliers` | `{[key]: float64}` or absent | From `resolveScoreMultipliers` |
| `score` | `float64` (set by `sortByScore`) | Higher = better; `overall / (1 + penalty)` |

#### UpstreamMetrics object shape (JS `u.metrics`)

From `internal/policy/eval.go:buildMetricsObject:L283`, `readUpstreamMetrics:L63`.

| Field | Type | Notes |
|---|---|---|
| `errorRate` | `float64` | EMA rolling error rate |
| `errorsTotal` | `int64` | Cumulative error count |
| `requestsTotal` | `int64` | Cumulative request count (window-bounded) |
| `throttledRate` | `float64` | EMA rolling throttle (429) rate |
| `misbehaviorRate` | `float64` | EMA rolling misbehavior rate |
| `blockHeadLag` | `int64` | Blocks behind network tip |
| `finalizationLag` | `int64` | Finalization blocks behind tip |
| `blockHeadLagSeconds` | `float64` | `blockHeadLag × networkBlockTime` |
| `finalizationLagSeconds` | `float64` | `finalizationLag × networkBlockTime` |
| `p50ResponseSeconds` | `float64` | p50 latency in seconds |
| `p70ResponseSeconds` | `float64` | p70 latency in seconds |
| `p90ResponseSeconds` | `float64` | p90 latency in seconds |
| `p95ResponseSeconds` | `float64` | p95 latency in seconds |
| `p99ResponseSeconds` | `float64` | p99 latency in seconds |
| `cordonedReason` | `string` or absent | Set when upstream is cordoned |
| `latencyP(q)` | `(q: number) => ms` | Shared singleton; snaps to nearest pre-computed bucket; input 0–1 or 0–100 |

Note: `blockHeadLagSeconds` / `finalizationLagSeconds` are `0` until the tracker has enough block-time samples for EMA estimation. `internal/policy/eval.go:L89-96`.

#### EvalContext (`ctx`) shape

`internal/policy/eval.go:EvalContext:L17`. Passed as second argument to eval.

| Field | Notes |
|---|---|
| `network` | Network ID string |
| `method` | Slot method (`"*"` for wildcard slots) |
| `finality` | Slot finality (`"unknown"` default for wildcard, real values for per-finality slots) |
| `now` | Unix milliseconds |
| `previousOrder` | `string[]` upstream IDs from last tick |
| `lastSwitchAt` | `int64` unix-ms or null |
| `tickCount` | `uint64` monotonic tick counter |

Global JS helpers available:
- `methodMatches(pat)` — tests `ctx.method` against a pattern
- `isFinalityRequest()` — `ctx.finality === 'finalized'`
- Preset globals: `PREFER_FASTEST`, `PREFER_FRESHEST`, `PREFER_LEAST_ERRORS`
- Finality mask constants: `REALTIME` (1), `UNFINALIZED` (2), `FINALIZED` (4), `UNKNOWN` (8) (installed by stdlib)

#### `hasMatchingTag` semantics

`internal/policy/stdlib/stdlib.js:L267-293`.

- Positive pattern (`"tier:main"`, `"region:us-*"`): matches if ANY tag on the upstream matches.
- Negated pattern (`"!tier:fallback"`): matches if NO tag matches the un-negated pattern.
- Array of patterns: positives are OR'd; negations are AND'd. Upstream matches iff (no positives OR at least one positive matches) AND (all negations hold).

#### `preferTag` semantics

`internal/policy/stdlib/stdlib.js:L854-865`.

1. Filter to upstreams matching primary pattern.
2. If ≥ `minHealthy` match → return that subset.
3. Else if `fallback` pattern is set → filter to fallback pattern.
4. Else → return input unchanged.

#### Exclusion attribution (leaf reasons)

Each `excludeIf` call logs which **leaf** predicate actually tripped per upstream into `__policyLeafReasons[id]`. The Go side reads this after eval; `splitStepFromLeafReasons` separates `"@step:NAME"` sentinel entries (which step first dropped the upstream) from the leaf slugs. Both feed separate Prometheus counters. `internal/policy/slot.go:L282-291`.

Bounded cardinality slugs from the predicate factories:

`error_rate_above`, `error_rate_below`, `throttle_rate_above`, `throttle_rate_below`, `misbehavior_rate_above`, `latency_p<Q>_above`, `latency_p<Q>_deviation_above`, `block_head_lag_above`, `finalization_lag_above`, `block_head_lag_seconds_above`, `finalization_lag_seconds_above`, `samples_below`, `samples_above` (guard), `not_<child_slug>`, `custom`.

#### `latencyDeviationAbove` algorithm

`internal/policy/stdlib/stdlib.js:L1228-1324`.

1. At factory construction time (per tick, per `excludeIf` call): build `topByMethod[method] = { v1, id1, v2 }` — top-2 fastest values across all upstreams for each method, gated by `minMethodSamples`.
2. Per upstream in the filter: for each method, look up `fastestPeer` (if `top.id1 == u.id` use `v2` else `v1`). Compute `rawRatio = my / fastestPeer`. Apply exponential damping: `ratio = rawRatio × (1 - exp(-my / dampingMs))`.
3. Collapse across methods per `mode`:
   - `"geomean"`: trip if `exp(mean(log(ratio))) > multiplier`.
   - `"majority"`: trip if ≥50% of compared methods show `ratio > multiplier`.
   - `"veto"`: trip if ANY method shows `ratio > multiplier`.
4. Skip the upstream entirely if `compared < 1` (no peers with data).

#### Fallback to raw registry order

If `policyEngine == nil` OR `len(upsList) == 0` after `GetOrdered`, the network falls back to the raw `upstreamsRegistry.GetNetworkUpstreams` order. `erpc/networks.go:L1032-1038`. This guarantees the network serves requests even before the first tick completes.

#### Legacy key translation

`SelectionPolicyConfig.UnmarshalYAML` captures `evalFunction`, `resampleExcluded`, `resampleInterval`, `resampleCount` into `LegacySelectionPolicy` for deferred warning. The translator wraps the legacy `evalFunction` into a modern `EvalFunc`. None of these legacy fields survive to runtime. `common/config.go:L2415-2431`.

Upstream-level legacy `group:` and `cohort:` YAML keys are rewritten as `tier:<value>` and `cohort:<value>` tags at config-load time. `common/config.go:mergeLegacyLabelKeysIntoTags`.

### Observability

All selection metrics are in `telemetry/metrics.go`. Labels are `project`, `network`, `method` unless noted.

| Metric | Type | Labels | Fires when |
|---|---|---|---|
| `erpc_selection_position` | Gauge | `project, network, method, upstream` | Every tick; value = 0-based index in the ordered list (`0`=primary, `1`=second choice, `2`=third choice, …); `-1` for all excluded upstreams. See position semantics below. |
| `erpc_selection_score` | Gauge | `project, network, method, upstream` | Every tick; value is `u.score` from `sortByScore`. Missing for probed/forced-included additions. |
| `erpc_selection_eligible_upstreams` | Gauge | `project, network, method` | Every successful tick; count of ordered (in-rotation) upstreams |
| `erpc_selection_eval_duration_seconds` | Histogram | `project, network, method` | Every tick; JS eval wall-clock time. Buckets: 0.5ms–1s |
| `erpc_selection_eval_errors_total` | Counter | `project, network, method, kind` | Tick failure; `kind ∈ {timeout, throw, invalid_return}`. See eval error kinds below. |
| `erpc_selection_rejection_total` | Counter | `project, network, method, upstream, step` | Per tick × excluded upstream; `step` = first stdlib primitive that dropped the upstream (`removeCordoned`, `excludeIf`, `take`, ...) |
| `erpc_selection_exclusion_total` | Counter | `project, network, method, upstream, reason` | Per tick × excluded upstream × leaf slug; compound predicates emit one per LEAF |
| `erpc_selection_shadow_exclusion_total` | Counter | `project, network, method, upstream, reason` | `shadowExcludeIf` would-have-excluded; upstream STAYS IN ROTATION (not dropped). See shadow exclusion semantics below. |
| `erpc_selection_excluded_seconds` | Gauge | `project, network, method, upstream` | Every tick; wall-clock seconds the upstream has been continuously excluded. Reset to 0 on readmit |
| `erpc_selection_readmit_total` | Counter | `project, network, method, upstream` | Transition from excluded → in-rotation |
| `erpc_selection_readmit_age_seconds` | Histogram | `project, network, method` | Distribution of exclusion age at readmit. Buckets: 1s–3600s |
| `erpc_selection_primary_switch_total` | Counter | `project, network, method, from, to` | Primary upstream ID changes |
| `erpc_selection_sticky_hold_total` | Counter | `project, network, method, upstream` | Ticks where `stickyPrimary` actively held the primary (would have flipped without sticky) |
| `erpc_selection_probe_requests_total` | Counter | `network, upstream, method` | Shadow-probe request fired to excluded upstream |
| `erpc_selection_probe_errors_total` | Counter | `network, upstream, method, reason` | Probe request errored; `reason ∈ {timeout, throttled, auth, skipped, error}` |
| `erpc_selection_probe_skipped_total` | Counter | `network, reason` | Probe candidate skipped; `reason ∈ {write_method, opt_out, sampled_out, max_concurrent, no_method}` |
| `erpc_selection_probe_dropped_total` | Counter | `network, reason` | Probe publish dropped (feed channel full); request path never blocks |

##### erpc_selection_position — value semantics

Assigned at `internal/policy/slot.go:L477` using 0-based `for i, id := range d.Output.Order` then `Set(float64(i))`. The `d.Output.Order` slice is the JS eval result — the array returned by the eval function, in the order returned.

- `0` — primary upstream (first element of the JS result array). This is the upstream that will serve the next request when no failover occurs.
- `1` — second choice (first runner-up). This upstream is tried if the primary fails or is consumed by a concurrent hedge.
- `2`, `3`, … — third, fourth, … choices in ranked order. Positions above 0 are tried sequentially by `NextUpstream` in the failsafe retry/hedge path.
- `-1` — excluded from rotation (assigned to ALL upstreams in `d.Output.Excluded`). All excluded upstreams share the same `-1` value regardless of how many there are or which predicates dropped them. No ordering exists among excluded upstreams.

The position values are set independently per slot (per `(network, method)` tuple as shown on the metric label). An upstream can have `position=0` in a wildcard slot and `position=1` in a per-method slot simultaneously.

##### erpc_selection_eval_errors_total — kind values

Produced at `internal/policy/slot.go:L460-L467`. The `kind` label is classified from the error string after a failed tick:

- `"timeout"` — the JS eval exceeded `EvalTimeout` (default 100 ms). Detected by `strings.Contains(d.Error, "timed out")`. The eval goroutine was cancelled; the previous tick's cache is retained unchanged.
- `"throw"` — the JS function threw an uncaught exception (runtime error, invalid method call, user-thrown error). This is the fallback kind when neither of the two string checks match.
- `"invalid_return"` — the eval completed without throwing but returned a non-array, null/undefined, an array with non-object entries, entries without an `id` field, or entries with an unknown upstream ID. Detected by `strings.Contains(d.Error, ErrInvalidReturn.Error())`. Produced by `internal/policy/eval.go:extractOrderedResult` at lines L937, L941, L951, L964, L968, L972.

**`"fallback_default"` is listed in the metric Help string (`telemetry/metrics.go:L202`) and in `internal/simulator/types.go:L196` as a comment, but no production code path currently emits this label value.** The slot.go classification at `L461-L466` only produces the three kinds above. The metric Help string is aspirational / forward-compatible documentation for a possible future path. Treat `fallback_default` as currently dead code from a metrics cardinality perspective.

After any error kind, the previous tick's cache is retained — the request path continues to use the last-good ordering. `internal/policy/slot.go:L250-L263`.

##### erpc_selection_shadow_exclusion_total vs erpc_selection_exclusion_total

These two counters represent fundamentally different outcomes:

- `erpc_selection_exclusion_total` — the upstream **was removed** from the ordered list. It will not serve traffic. The JS `excludeIf(predicate)` chain dropped it; `d.Output.Excluded` contains the upstream; `erpc_selection_position` is set to `-1`.

- `erpc_selection_shadow_exclusion_total` — the upstream **was NOT removed** and remains in rotation (still appears in `d.Output.Order` with a non-negative position), but the `shadowExcludeIf(predicate)` predicate would have excluded it if used as `excludeIf`. The upstream continues to serve traffic normally. The counter is incremented for monitoring/auditing purposes only — it lets operators compare the exclusion rate of a new rule (in shadow mode) against the real exclusion rate before promoting it to `excludeIf`. The same leaf-slug attribution is applied. `internal/policy/slot.go:L528-L537`, `internal/policy/stdlib/stdlib.js:L53-L55`.

The semantic distinction: `exclusion_total` means "this upstream was blocked this tick"; `shadow_exclusion_total` means "this upstream would have been blocked IF the rule were real, but wasn't blocked."

#### Tracing

OpenTelemetry span `"PolicyEngine.GetOrdered"` wraps the `GetOrdered` call in the request path. `erpc/networks.go:L1023`. Attributes: `upstreams.count`, `upstreams.sorted` (detailed tracing only).

#### Log messages

- `WARN "selection policy eval failed; retaining previous cache"` — tick error; JSON fields: `network`, `method`, `tick_id`, `error`.
- `DEBUG "policy step"` — one line per stdlib step when `stepLogEnabled=true`; fields: `network`, `method`, `tick_id`, `idx`, `step`, `in`, `out`, `dropped`, `added`, `reordered`, `args`.
- `DEBUG "policy excluded upstream"` — one per excluded upstream when `stepLogEnabled=true`; fields: `network`, `method`, `tick_id`, `upstream`, `step`, `reason`, `leaf_reasons`.

### Edge cases & gotchas

1. **Cold-start wildcard fallback**: on the first request before the wildcard slot has ticked, `GetOrdered` returns nil → `networks.go` falls back to raw registry order. The synchronous initial tick in `RegisterNetwork` prevents this in practice, but tests that create networks without `Bootstrap` may see it. `erpc/networks.go:L1032-1038`.

2. **Method-specific `scoreMultipliers` require `evalScope: network-method`**: an entry with `Method: "eth_getLogs"` never matches the wildcard slot because `ctx.method == "*"`. Only per-method slots see a specific method value. `internal/policy/eval.go:resolveScoreMultipliers:L442`, `erpc/eval_multipliers_test.go:TestResolveScoreMultipliers`.

3. **`blockHeadLagSeconds` / `finalizationLagSeconds` are 0 until block-time samples exist**: the tracker's EMA-estimated block time is 0 on cold start. Predicates like `blockSecondsLagAbove(30)` silently no-op until the tracker has enough samples. Use `blockNumberLagAbove(16)` as the primary guard; `blockSecondsLagAbove` as a secondary. `internal/policy/eval.go:L89-96`.

4. **Bug #907 regression**: with per-finality tracking enabled (`evalScope: network-finality` or finer), block-head-lag writes must go through both the specific-finality bucket AND the `{*, All}` rollup. Pre-fix, the policy's `blockNumberLagAbove` read the rollup (starved) and silently no-oped. Fixed by ensuring `updateNetworkLagMetrics` fans out to the wildcard aggregate. Covered by `TestNetworkPolicy_RealPoll_LaggingUpstreamExcluded`. `erpc/networks_selection_policy_realpoll_test.go`.

5. **Safety-net `whenEmpty(() => upstreams)`**: if all upstreams fail every `excludeIf` rule, the safety net restores the full unfiltered set. The network sends traffic to broken upstreams rather than failing closed. `internal/policy/default_policy.js:L46`. Covered by `TestNetworkPolicy_SafetyNet_WhenAllBroken`.

6. **No time-based re-admission**: excluded upstreams stay excluded until their tracker metrics improve. `probeExcluded` shadow-probes them to accumulate fresh samples; time alone is NOT a re-admission trigger. `TestNetworkPolicy_NoTimeBasedReadmit_OnlyMetricsHeal` verifies that arbitrary virtual-time advancement does not re-admit a degraded upstream.

7. **stickyPrimary: previous primary excluded in THIS method but not others**: `prevIdx < 0` → the slot returns the current sort order WITHOUT updating the shared register, so other slots (with that upstream still in their survivor set) keep it as primary. This is the "method-disagreement escape hatch". `internal/policy/stdlib/stdlib.js:L795-802`.

8. **stickyPrimary hysteresis direction**: score is HIGHER-is-better. The challenger must have a HIGHER score than the incumbent × (1 + hysteresis) to trigger a switch. A slower challenger does not trigger a switch (the correct, non-flapping behavior). `internal/policy/stdlib/stdlib.js:L833`.

9. **`probeExcluded` is a no-op array transform**: it does NOT modify the upstream list. Its only effect is depositing `__probeConfig` for the engine. The Prober's activity is entirely background. `internal/policy/stdlib/stdlib.js:L999-1010`.

10. **Probe skip reason `write_method`**: probes are never fired for methods classified as write operations (e.g. `eth_sendRawTransaction`). Mutating calls should not be mirrored. `internal/policy/prober.go`.

11. **`routing.probe: "off"` permanently excludes from probing**: the upstream accumulates no shadow samples and stays excluded forever once predicates trip — until structural signals (e.g. lag clearing) naturally drive re-admission, or an operator manually uncordons. Cost-sensitive pay-per-call vendors are the primary use case. `common/config.go:L784-788`.

12. **Engine idle eviction rate**: the sweep runs every `engineSweepInterval = 1 minute`. A slot with `idleEvictionAfter = 1h` may persist for up to 1h + 1m after its last access. `internal/policy/engine.go:engineSweepInterval:L208`.

13. **`latencyDeviationAbove` is constructed fresh per excludeIf call per tick**: the closure captures `topByMethod` at factory time (per tick). The factory call inside `excludeIf(latencyDeviationAbove(3, ...))` runs once per tick because the JS chain executes once per tick. Do NOT put `latencyDeviationAbove` inside a JS variable and reuse across ticks — it would use stale peer data. This is safe in the default policy because the chain is a stateless expression evaluated fresh each tick.

14. **Step-log disabled by default**: `stepLogEnabled` is false in production. The per-step chain trail (useful for diagnostics) is only captured when `Engine.SetStepLogEnabled(true)` is called (by the simulator or when log level is DEBUG). Zero overhead in production beyond one function-call indirection per stdlib step. `internal/policy/engine.go:L679-695`.

15. **Alphabetical tiebreak in `sortByScore`**: when two upstreams have identical scores (e.g. both clean on cold start), the one with the lexicographically earlier ID wins. This makes test assertions order-sensitive until `seedDegraded` diverges their metrics. `internal/policy/stdlib/stdlib.js:L656`.

16. **`upstreamDefaults.routing` is all-or-nothing**: if `upstream.routing` is nil, the defaults routing block is cloned onto it. If `upstream.routing` is non-nil (even empty), the defaults routing block is NOT applied. `erpc/eval_multipliers_test.go:TestApplyDefaults_InheritsRouting`.

17. **`__policyStepLog` allocation gated**: when `__policyStepLogEnabled` is false the JS wrapper never calls `_recordStep` and never allocates the `log` entries. Metrics attribution (`__policyLeafReasons`, `__policyShadowReasons`) is ALWAYS-on regardless. `internal/policy/stdlib/stdlib.js:L190-194`.

18. **Probe feed channel size**: `probeFeedBufferSize = 256`. Overflow drops the publish (never blocks the request path) and increments `erpc_selection_probe_dropped_total`. `internal/policy/prober.go:L102`.

19. **`ErrUpstreamExcludedByPolicy` (error code `ErrCodeUpstreamExcludedByPolicy`)**: defined at `common/errors.go:L1823-L1836`. Error code string: `"ErrUpstreamExcludedByPolicy"`. Retryability: U:yes (retryable towards upstream — it is NOT in the `IsRetryableTowardsUpstream` non-retryable list), N:yes (retryable at network level). The error is designed to be emitted when an upstream is attempted but the policy has excluded it. However: **`NewErrUpstreamExcludedByPolicy` has no production call site in the current codebase** (only referenced in `health/tracker_test.go:L763` as a test fixture). The error type exists for forward compatibility and is consumed by: (1) `health.Tracker.RecordUpstreamFailure` which silently ignores it (`health/tracker.go:L973`) — so it does not penalize the upstream's error rate metrics; (2) `common.ErrUpstreamsExhausted` classifier logic at `common/errors.go:L1012` which routes it to the `excluded` counter bucket. The policy engine achieves exclusion structurally (upstreams not placed in the ordered list are never attempted by `NextUpstream`) rather than by returning this error code.

20. **`scoreMultipliers.finality` YAML integer vs TS bit-flag confusion**: the `finality` field on `ScoreMultiplierConfig` uses the Go `DataFinalityState` integer enum (`0`=finalized, `1`=unfinalized, `2`=realtime, `3`=unknown). In YAML, use string names (`"finalized"`, `"realtime"`, etc.) or quoted integer strings `"0"`–`"3"`. In TS config, use the `DataFinalityState*` integer constants from `generated.ts`. **Do NOT use the `FINALIZED/REALTIME/UNFINALIZED/UNKNOWN` JS bit-flag constants (4/1/2/8) from `evalFunc` globals here** — those are bitmasks for `.when(mask, fn)` inside the eval function body, not for the config struct field. Setting `finality: [4]` in YAML will attempt to match integer `4` against valid values `0–3` and will fail to match any request. `common/data.go:L54-L76`, `internal/policy/stdlib/install.go:L74-L77`.

### Source map

- `internal/policy/engine.go` — `Engine` struct; `RegisterNetwork`, `GetOrdered`, `GetExcluded`, `PublishRequest`, `GetScores`, `RecentDecisions`; idle slot eviction; probe config reconciliation.
- `internal/policy/slot.go` — `Slot` struct; `tickOnce` (the full eval cycle); `emitMetrics`; `snapshotMetrics`; `materializeOrder`; `logStepTrail`; decision ring buffer.
- `internal/policy/eval.go` — `runEval`; JS globals setup/teardown; `buildJSUpstreams`; `buildMetricsObject`; `readUpstreamMetrics`; `resolveScoreMultipliers`; `extractOrderedResult`; `readProbeConfig`; `readReasonsMap`.
- `internal/policy/default_policy.go` — `DefaultPolicySource()`; `upgradeDefaultPolicy` (swaps placeholder for embedded JS at register time).
- `internal/policy/default_policy.js` — embedded default policy chain (score exclusion rules, safety net, tier split, sortByScore, stickyPrimary, probeExcluded).
- `internal/policy/stdlib/stdlib.js` — all chainable Array.prototype methods; predicate factories; presets; `latencyDeviationAbove` algorithm; `stickyPrimary` logic; `probeExcluded` no-op + config deposit.
- `internal/policy/stdlib/install.go` — Go-side installation of stdlib into each sobek runtime.
- `internal/policy/sticky.go` — `stickyStore`; cross-slot shared primary register; idle eviction.
- `internal/policy/prober.go` — `Prober`; shadow-mirror request dispatch; `windowCounter` for `minSamples` floor; per-upstream in-flight cap.
- `internal/policy/decision.go` — `Decision`, `DecisionDiff`, `ExcludedUpstream` structs; `DecisionState`.
- `internal/policy/runtime_pool.go` — sobek `runtimePool`; acquires/releases VMs; runs `runtimePrimer` on fresh runtimes.
- `internal/policy/testing.go` — test helpers: `TickForTest`, `ResetSlotStateForTest`, `AdvanceEvalNowForTest`, `SetFinalityForTest`, `LatestDecisionOutputForTest`, `OverrideOrderForTest`.
- `common/config.go:L2352-2430` — `SelectionPolicyConfig`, `EvalScope`, `UpstreamRoutingConfig`, `ScoreMultiplierConfig`, `ProbeMode`, `LegacySelectionPolicyFields`, `UnmarshalYAML` (legacy key capture).
- `common/defaults.go:L2512-2609` — `SelectionPolicyConfig.SetDefaults`; default `EvalInterval`, `EvalTimeout`, `EvalScope` translation.
- `erpc/networks.go` — `policyEngine` field; `Bootstrap` → `RegisterNetwork`; `Forward` → `GetOrdered` + `PublishRequest`; `GetSelectionScores`, `GetRecentDecisions`, `GetLastSwitchAt`, `GetCurrentOrder`; `PinUpstreamOrderForTest`.
- `telemetry/metrics.go:L174-291` — all `MetricSelection*` Prometheus vars.
- `erpc/networks_selection_policy_test.go` — integration tests for the default policy: slow/erroring/lagging/throttled upstream exclusion; fallback tier activation; safety-net; sticky primary; no-time-based readmit; `spreadAcrossTags`.
- `erpc/networks_selection_policy_realpoll_test.go` — real state-poller → tracker → policy path tests; regression for #907 lag rollup bug.
- `erpc/upstream_selection_test.go` — centralized upstream selection across hedge and retry; mixed response type routing; four-attempt scenario.
