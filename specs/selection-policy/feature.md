# Selection Policy — Specification

**Status**: Ready for implementation
**Owner**: TBD
**Last revised**: 2026-05-13

---

## 1. Purpose

The **Selection Policy** is the sole mechanism that decides which upstreams handle which requests and in what order. It runs as a per-network (optionally per-method) background loop. Each tick takes a snapshot of upstream identity, configuration, and tracked metrics, executes a user-defined JavaScript evaluation function, and produces an **ordered list of upstreams**. That list is the routing decision: requests are served from the head of the list, falling through to subsequent entries on retry/failure. Upstreams not present in the returned list are excluded for that tick.

The evaluation function has access to a Go-implemented standard library exposed as chainable methods on the upstream array. Authors compose built-in operators (`sortByScore`, `removeByLag`, `preferTag`, `stickyPrimary`, etc.) or drop into plain JS when needed. The default policy is a single chain covering the common case.

There is no separate "scoring" subsystem, no `routingStrategy` knob, no permit-acquisition gate at request time, and no cordon table. Routing behavior is *entirely* expressed by the eval function.

---

## 2. Configuration

```yaml
networks:
  - architecture: evm
    evm: { chainId: 1 }

    selectionPolicy:
      evalInterval: 1s          # tick frequency.                 default: 1s
      evalPerMethod: false      # maintain a cache per method.    default: false
      evalTimeout: 100ms        # per-tick eval hard cap.         default: 100ms
      evalFunc: |
        return upstreams
          .excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
          .excludeIf(blockNumberLagAbove(50))
          .sortByScore(PREFER_FASTEST)
          .stickyPrimary({ hysteresis: 0.30, minSwitchInterval: '30s' })
          .readmitExcluded({ reAdmitAfter: '90s', maxConcurrent: 2, position: 'tail' })
```

| Field | Type | Description |
|---|---|---|
| `evalInterval` | `Duration` | How often the eval runs. Default `1s`. |
| `evalPerMethod` | `bool` | If true, separate eval + cache per `(network, method)`. Default false. |
| `evalTimeout` | `Duration` | Hard wall-clock cap on each eval. Default `100ms`. |
| `evalFunc` | `string` | JavaScript function body. Signature `(upstreams, ctx) => Upstream[]`. If omitted, the [default policy](#7-default-policy) applies. |

> Investigations go through standard observability: per-tick reasoning lands on OTLP tracing spans (one per tick + per request) and the `policy_selection_*` Prometheus families. There is no in-memory decision-record buffer and no `decisionHistory` config field.

`selectionPolicy` is defined at the network level. There are **no project-level** routing settings.

---

## 3. Eval inputs

The eval function body has two in-scope variables: `upstreams` and `ctx`.

### 3.1 `upstreams: Upstream[]`

```ts
type Upstream = {
  readonly id: string
  readonly vendor: string                  // e.g. "alchemy", "infura", "drpc"
  readonly type: 'evm' | string            // upstream architecture
  readonly tags: readonly string[]         // user-applied labels (convention: 'prefix:value');
                                           // consumed by byTag / preferTag / spreadAcrossTags

  // Tag-check helpers — `is` is an alias of `hasTag` (same call,
  // different name to read naturally in different contexts).
  hasTag(t: string): boolean
  is(t: string):     boolean

  readonly metrics: UpstreamMetrics        // snapshot taken at the start of this tick

  // Per-upstream score multipliers resolved for the current (network, method,
  // finality) tick. Absent when the upstream has no matching entry under
  // `routing.scoreMultipliers`. `sortByScore` reads this automatically per
  // its `multipliers` option ('merge' | 'override' | 'off').
  readonly scoreMultipliers?: {
    errorRate?: number
    respLatency?: number
    throttledRate?: number
    blockHeadLag?: number
    finalizationLag?: number
    misbehaviors?: number
    overall?: number
  }

  // Attached by std-lib steps; readable by subsequent steps and visible in decision records.
  readonly score?: number                  // set by sortByScore (HIGHER = better)
  readonly annotations?: string[]          // set by std-lib steps
}

type UpstreamMetrics = {
  errorRate: number                        // 0..1, in current window
  errorsTotal: number
  requestsTotal: number
  throttledRate: number                    // 0..1 (remote-rate-limit responses / requests)
  misbehaviorRate: number
  p50ResponseSeconds: number
  p70ResponseSeconds: number
  p90ResponseSeconds: number
  p95ResponseSeconds: number
  p99ResponseSeconds: number
  blockHeadLag: number                     // block-number delta from network tip
  finalizationLag: number                  // block-number delta from finalized tip
  // Wall-clock lag — block-count × network's EMA-estimated block time
  // (`tracker.GetNetworkBlockTime`). Zero until the tracker has enough
  // samples to estimate block time; policies relying on these will be
  // no-ops during the first few seconds after boot, by design.
  blockHeadLagSeconds:    number           // seconds behind network tip
  finalizationLagSeconds: number           // seconds behind finalized tip
  cordonedReason: string | null            // set by external systems (failsafe circuit breaker)

  // Quantile reader — returns latency in MS at the given quantile.
  // Accepts either 95 (percentile) or 0.95 (fraction); auto-normalized.
  // Snaps to the nearest precomputed bucket (p50/p70/p90/p95/p99).
  latencyP(q: number): number              // ms
}
```

The `metrics` object is a **snapshot** captured once at the tick start; the eval sees a consistent view.

### 3.2 `ctx: EvalContext`

```ts
type EvalContext = {
  network: string                                // e.g. "evm:1"
  method: string                                 // "*" if evalPerMethod=false
  finality: 'realtime' | 'unfinalized' | 'finalized' | 'unknown'
  now: number                                    // unix ms

  // Cross-tick state, fed back from previous tick's output:
  previousOrder: string[]                        // upstream IDs returned last tick
  previousExcluded: string[]                     // upstream IDs absent from last tick's output
  lastSwitchAt: number | null                    // unix ms of last sticky-primary switch
  excludedSince: { [id: string]: number }        // unix ms when this upstream first dropped out
  tickCount: number                              // monotonic; resets on slot reset
}
```

`ctx` is the **only** carrier of cross-tick state. The engine itself stores nothing beyond what is serialized in/out of `ctx`, making evals testable as pure functions.

### 3.3 Globals

In addition to the upstream array and `ctx`, the runtime exposes:

- `console` — `log`, `info`, `warn`, `error` (sink: structured logger at debug level).
- `Math`, `Date`, `JSON`, `Number`, `String`, `Array`, `Object` — standard ECMAScript.
- `process.env` — environment variables, read-only.
- All constants from §4.1 as bare identifiers.

---

## 4. Standard library reference

Methods are exposed on the upstream array (and on every chain result) as instance methods. Each returns an `Upstream[]` (or `Group[]` where noted) so they compose.

### 4.1 Constants

#### Score presets (weight maps for `sortByScore`)

| Constant | Weights `{errorRate, respLatency, throttledRate, blockHeadLag, finalizationLag, misbehaviors}` |
|---|---|
| `PREFER_FASTEST` | `4, 15, 4, 1, 0, 2` |
| `PREFER_FRESHEST` | `4, 2, 2, 15, 8, 3` |
| `PREFER_LEAST_ERRORS` | `15, 2, 6, 2, 1, 12` |

Each preset emphasizes ONE primary axis (`15`) while keeping the others balanced enough that an obviously bad upstream on a secondary signal still loses. `PREFER_FASTEST` is the default when `sortByScore` is called without a base weight map — the assumption is that the `excludeIf` chain has already dropped broken upstreams, so ranking reduces to "which of the healthy ones answers first?". Custom weight maps may be passed as an object literal.

#### Finality bit-flags

`REALTIME` (1), `UNFINALIZED` (2), `FINALIZED` (4), `UNKNOWN` (8) — powers of two intended for the `.when(mask, fn)` chainable, which runs its sub-chain only when the request's finality matches the bitwise-OR'd mask:

```js
.when(REALTIME | UNFINALIZED | UNKNOWN, u => u.stickyPrimary({ scope: NETWORK }))
```

For direct comparisons against `ctx.finality` use the string literals (`'realtime'`, `'unfinalized'`, `'finalized'`, `'unknown'`) — that's the value-type `ctx.finality` actually carries.

#### `stickyPrimary` scope constants

`NETWORK` (`'network'`), `NETWORK_METHOD` (`'network-method'`), `NETWORK_FINALITY` (`'network-finality'`), `NETWORK_METHOD_FINALITY` (`'network-method-finality'`). Slots mapped to the same scope value share a primary register and converge via `stickyPrimary`'s hysteresis + minSwitchInterval. The TS SDK exports the same CAPITAL_SNAKE_CASE names; an operator can write `scope: NETWORK` in both the eval function and (via the SDK) the YAML config.

#### Reasons (for cordoning/exclusion annotations)

`REASON_LAG`, `REASON_ERROR_RATE`, `REASON_LATENCY`, `REASON_THROTTLING`, `REASON_MISBEHAVIOR`, `REASON_CORDONED`, `REASON_GROUP`, `REASON_VENDOR`, `REASON_USER_FILTER` — used internally by std-lib steps when annotating rejections; available to user code as well.

---

### 4.2 Identity & label selection

Upstreams carry an open-ended `tags []string` slice. Recommended convention `<dimension>:<value>` (e.g. `tier:main`, `region:us-east`, `sequencer:op-base`). Bare strings (no prefix) work too.

```ts
.where(filter: { id?, tag?, vendor?, type? })
.whereNot(filter: same as above)
.byId(id: string | string[])
.excludeId(id: string | string[])
.byTag(pat: string | string[])
.excludeTag(pat: string | string[])
.byVendor(name: string | string[])
.excludeVendor(name: string | string[])
.byType(type: string | string[])
```

Filter values support glob patterns: `byTag('tier:primary*')`, `byVendor('!alchemy')` (`!` = negation). Multiple fields in `.where({...})` are AND-ed; multiple values in an array are OR-ed.

**Tag negation semantic.** `byTag('!X')` matches upstreams that have NO tag matching `X` (it's the dual of `byTag('X')`). Array form mixes: positive patterns are OR'd, negated patterns are AND'd, and an upstream matches iff `(no positives in array OR at least one positive matches) AND (all negations hold)`.

**Legacy YAML keys.** `group: X` / `cohort: Y` at the upstream level are accepted at config-load time only: the loader rewrites them as `tier:X` / `cohort:Y` tags. There is no Go-level `Group` or `Cohort` field on `UpstreamConfig`, and the stdlib does not expose `byGroup`, `preferGroup`, or `spreadAcrossGroups` — those names have been removed.

---

### 4.3 Health filters (`remove*`)

Each removes upstreams failing the predicate. Rejected upstreams are annotated `{rejectedBy: <step>, reason: <human string>}` in the decision record.

```ts
.removeByErrorRate(maxRate: number)                       // drop if metrics.errorRate > maxRate
.removeByLatency({ p50Ms?, p70Ms?, p90Ms?, p95Ms?, p99Ms? })
.removeByThrottling(maxRate: number)
.removeByMisbehavior(maxRate: number)
.removeByLag({ blockHead?: number, finalization?: number })
.removeByMinRequests(min: number)                          // drop if requestsTotal < min (not enough samples)
.removeCordoned()                                          // drop if metrics.cordonedReason != null
.removeStale(maxAgeMs: number)                             // drop if last metric update older than maxAgeMs (uses ctx.now)
```

Convenience composite (soft-deprecated; prefer `excludeIf` + predicate factories below for per-rule cooldowns and arbitrary compound conditions):

```ts
.keepHealthy({
  maxErrorRate?: number   = 0.5,
  maxBlockHeadLag?: number = 10,
  maxP95Ms?: number       = 5000,
  maxThrottledRate?: number = 0.3,
})
```

---

### 4.3a Admission control: `excludeIf` + predicate factories

The composable replacement for `keepHealthy`. One primitive (`excludeIf`) plus a small library of predicate factories cover everything `keepHealthy` did AND enable compound conditions and tier-scoped trips.

#### Primitive

```ts
.excludeIf(predicate: (u: Upstream) => boolean, reasonOverride?: string)
```

Semantics:

- If `predicate(u)` is **true** this tick → drop `u`.
- Drops are auto-annotated for diagnostics. Factory-built predicates (`errorRateAbove`, `latencyDeviationAbove`, `all`/`any`/`not`, …) stamp `policyReason` / `policySlug` metadata describing what they check; `excludeIf` surfaces those in `Decision.Output.Annotations[id]`, the simulator's policy-history pane, and DEBUG eRPC logs. Inline custom predicates can supply an explicit `reasonOverride` string as the optional 2nd positional arg.
- `excludeIf` itself has NO cross-tick / cooldown memory. Once the predicate no longer matches, the upstream is admitted again on the next tick. Anti-flap timing is centralized in `readmitExcluded({ reAdmitAfter })` at the end of the chain — that primitive controls how long an excluded upstream stays out before it is allowed back into rotation, regardless of why it was excluded.

The slot's existing `excludedSince` map (the input to `readmitExcluded`) is updated automatically based on whether an upstream survives the chain — no per-rule bookkeeping.

#### Predicate factories (globals)

Each returns a `(u: Upstream) => boolean` predicate suitable for `excludeIf`. Naming convention: `<signal><Above|Below>(threshold)`.

```ts
// Rates (0..1 fractions)
errorRateAbove(rate)        / errorRateBelow(rate)
throttleRateAbove(rate)     / throttleRateBelow(rate)
misbehaviorRateAbove(rate)

// Latency (ms thresholds) — value is the first arg; quantile is optional (defaults to p70)
latencyAbove(ms, quantile?)

// Latency (relative; trip if p<q> > multiplier × fastest peer's p<q>)
latencyDeviationAbove(multiplier, quantile?)

// Lag (block counts)
blockNumberLagAbove(blocks)
finalizationLagAbove(blocks)

// Lag (wall-clock seconds — block-count × network's EMA block-time)
blockSecondsLagAbove(seconds)
finalizationSecondsLagAbove(seconds)

// Sample-size guards (useful as AND-terms to avoid tripping on low-sample noise)
samplesBelow(n)
samplesAbove(n)
```

#### Combinators (globals)

Flat, variadic. Returns predicates; compose freely:

```ts
all(...preds)   // AND
any(...preds)   // OR
not(pred)       // NOT
```

#### Examples

```js
// Health trip rules — pair with a single `readmitExcluded({ reAdmitAfter })`
// at the end of the chain to control how long excluded upstreams stay out.
.excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
.excludeIf(all(samplesAbove(10), throttleRateAbove(0.4)))
.excludeIf(any(all(samplesAbove(20), latencyDeviationAbove(3)), latencyAbove(30_000)))
.excludeIf(any(blockNumberLagAbove(16), blockSecondsLagAbove(30)))

// Compound: trip only when errors AND latency are both bad
.excludeIf(all(errorRateAbove(0.2), latencyAbove(2_000, 99)))

// Sample-size guard: don't trip on noise from low-sample upstreams
.excludeIf(all(samplesAbove(10), errorRateAbove(0.3)))

// Tier-scoped — free-tier upstreams get tighter SLO
.excludeIf(all(u => u.is('tier:free'), errorRateAbove(0.1)))

// Custom inline predicate with a reason label
.excludeIf(u => u.metrics.errorRate > 0.3 && u.metrics.latencyP(99) > 1_500,
           'errorRate>0.3 && p99>1.5s')
```

#### Tag helpers on `Upstream`

`u.hasTag(t)` and `u.is(t)` (alias) check tag membership without `u.tags.includes(t)` boilerplate. Authors writing their own factories should prefer them.

#### Latency: `u.metrics.latencyP(q)`

Returns milliseconds. Accepts either `0..1` fractions (`0.95`) or `0..100` percentiles (`95`). Snaps to the nearest precomputed quantile bucket (p50/p70/p90/p95/p99). Use `latencyAbove(ms, quantile?)` (quantile defaults to p70) to build absolute-threshold predicates; use `latencyDeviationAbove(multiplier, quantile?)` to trip when p<q> exceeds the fastest peer's p<q> by `multiplier`×.

#### Authoring custom factories

A factory is just a function returning a predicate. Operators can define their own in their policy file:

```js
function indexerStaleByBlocks(n) {
  return u => u.is('vendor:indexer') && u.metrics.blockHeadLag > n;
}

upstreams
  .excludeIf(indexerStaleByBlocks(50), 'indexer stale')
  .excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
```

No registration required. Same surface as the stdlib factories.

---

### 4.4 Generic functional

```ts
.filter(fn: (u, idx, arr) => boolean)                         // JS Array.prototype.filter
.reject(fn: (u, idx, arr) => boolean)                         // inverse of .filter
.partition(fn: (u) => boolean) : [Upstream[], Upstream[]]     // [matching, non-matching]
.unique(keyFn?: (u) => any)                                   // dedupe by id, or by keyFn
.union(other: Upstream[])                                     // set union by id
.intersect(other: Upstream[])                                 // set intersection by id
.difference(other: Upstream[])                                // a \ b by id
.isEmpty : boolean                                            // accessor
```

JS Array prototype methods (`.find`, `.slice`, `.reverse`, `.concat`, `.length`, `.map`, `.forEach`, …) work as usual — the stdlib only ADDS chainable verbs.

---

### 4.5 Sorting

The primary scoring entry point:

```ts
.sortByScore(
  base?: ScoreWeights | preset | ((u: Upstream) => ScoreWeights),
  opts?: {
    multipliers?: 'merge' | 'override' | 'off'    // default: 'merge'
    latencyQuantile?: 'p50'|'p70'|'p90'|'p95'|'p99'   // default: 'p70'
    overall?: (u: Upstream) => number             // per-upstream multiplicative dial folded on top of u.scoreMultipliers.overall
  }
) : Upstream[]
```

`ScoreWeights = { errorRate?, respLatency?, throttledRate?, blockHeadLag?, finalizationLag?, misbehaviors? }`. Missing keys default to 0. The first argument can be either:

- a **flat weight map** applied to every upstream (e.g. `{ errorRate: 8, respLatency: 4 }`);
- a **preset constant** — `PREFER_FASTEST` (default if omitted), `PREFER_FRESHEST`, or `PREFER_LEAST_ERRORS`;
- a **function** `(u) => ScoreWeights` returning per-upstream weights (useful when different upstream types or vendors merit different metric emphasis).

`score(u) = overall(u) / (1 + Σ metricᵢ × weightᵢ)` — HIGHER score wins (the chain is best-first). A clean upstream (zero penalty) scores `overall` (default 1); accrued errors / latency / lag divide that back down. Per-upstream `routing.scoreMultipliers` (config) arrive as `u.scoreMultipliers` and combine with `base` per `opts.multipliers`:

- `'merge'` (default) — per-upstream keys override matching base keys; unset keys inherit base. `overall` lifts the final score.
- `'override'` — configured upstreams rank by THEIR weights only; upstreams without config use `base`.
- `'off'` — ignore `u.scoreMultipliers` entirely; rank by `base`.

Tiebreaker is alphabetical id, applied automatically. Each upstream's resulting `score` is attached and visible to subsequent steps and in decision records.

Other sorts (ascending unless noted):

```ts
.sortBy(fn: (u) => number, opts?: { desc?: boolean })
.sortByDesc(fn: (u) => number)             // alias: sortBy(fn, {desc:true})
.sortByLatency(quantile?: 'p50'|'p70'|'p90'|'p95'|'p99')   // default 'p70'
.sortByErrorRate()
.sortByThrottling()
.sortByMisbehavior()
.sortByHeadLag()
.sortByFinalizationLag()
```

---

### 4.6 Randomization & rotation

```ts
.shuffle(seed?: number)
.rotateBy(n: number)                       // n may be derived from ctx.now for time-based RR
```

---

### 4.7 Stability (cross-tick)

```ts
.stickyPrimary({
  scope?: 'network' | 'network-method' | 'network-finality' | 'network-method-finality',  // default 'network'
  hysteresis?: number = 0.10,          // challenger's score must clear incumbent by this margin (HIGHER is better)
  minSwitchInterval?: Duration = '30s' // cooldown between switches
})
// Holds the previous primary across ticks unless BOTH
//   (a) challenger.score > prev.score × (1 + hysteresis), AND
//   (b) at least `minSwitchInterval` has elapsed since the last switch.
// `scope` controls WHICH set of slots agree on this primary. Cross-method
// scopes ('network', 'network-finality') score off the wildcard
// `u.metricsAcrossMethods` aggregate so every participating slot reaches
// the same hold/switch verdict; slot-grain scope reads the slot's own
// per-method `u.score`.
```

---

### 4.8 Grouping & multi-tier

The canonical multi-tier surface is the pair `preferTag` (tier selection
with fallback) and `spreadAcrossTags` (partition + interleave by a tag
prefix). There is no `groupBy` / `Group[]` type and no separate "group"
data model — tags carry every dimension of upstream classification.

```ts
// On Upstream[]:
.interleave(other: Upstream[])             // round-robin merge of two arrays
```

Convenience preference operators (the most common multi-tier patterns):

```ts
.preferTag(pat: string, opts?: {
  minHealthy?: number = 1,
  fallback?: string                        // tag pattern; if minHealthy not met, switch to fallback set
})
.preferVendor(name: string, opts?: same)
.preferAttribute(
  keyFn: (u) => any,
  value: any,
  opts?: same
)
// Generic form: prefer upstreams where keyFn(u) === value.
```

The default policy uses `preferTag('!tier:fallback', { fallback: 'tier:fallback' })` to express "primary tier = upstreams NOT tagged tier:fallback; if empty, fall through to tier:fallback".

Blast-radius interleave (used after sorting):

```ts
.spreadAcrossTags(prefix: string)
```

Re-interleaves an already-sorted list so adjacent positions don't share
the same tag matching `prefix` (e.g. `'cohort:'` to spread across cohorts).
Use after `sortByScore` to ensure the failover order has fault-domain
diversity — the primary stays the best by score, the first runner-up
comes from a different bucket, and so on. Stable within each bucket.
Upstreams with no matching tag share the empty-key bucket.

To partition by vendor (a non-user-controlled attribute), apply
explicit `vendor:<name>` tags to your upstreams and call
`spreadAcrossTags('vendor:')`. There is no separate `spreadAcrossVendors`
primitive: one canonical spreader, one canonical data model.

---

### 4.9 Slicing & limits

```ts
.pickTop(n: number)                        // first n
.pickBottom(n: number)                     // last n
.dropTop(n: number)                        // skip first n
.dropBottom(n: number)                     // skip last n
.take(n)                                   // alias for pickTop
.skip(n)                                   // alias for dropTop
.at_(index: number) : Upstream | null      // single-upstream getter (avoids shadowing JS .at)
```

---

### 4.10 Probing & forced inclusion

```ts
.readmitExcluded({
  reAdmitAfter: Duration,                  // re-admit upstreams excluded for at least this long
  maxConcurrent?: number = 1,              // cap on simultaneously re-admitted upstreams per tick
  longestFirst?: boolean = true,           // prioritize upstreams excluded the longest
  position?: 'tail' | 'head' | 'random' = 'tail'   // 'tail' = cautious; only retry/hedge fallback hits
})
// Deterministic re-admission of excluded upstreams. Each tick, find upstreams
// whose ctx.excludedSince is at least `reAdmitAfter` old, sort by exclusion age
// (oldest first if longestFirst), pick up to `maxConcurrent`, and insert at
// the specified position so they receive traffic and their metrics refresh.
//
// This is the SINGLE anti-flap mechanism in the chain. `excludeIf` itself has
// no cooldown — once a predicate stops tripping, the upstream would be
// admitted again on the next tick. `readmitExcluded({reAdmitAfter})` is what
// gates that re-entry: an upstream that fell out for ANY reason has to stay
// out for at least `reAdmitAfter` before being eligible for re-admission, and
// then only `maxConcurrent` candidates re-enter per tick. Place this step at
// the END of the chain so re-admitted upstreams land at the tail of the final
// ordering (lowest blast radius); putting it earlier lets subsequent sort
// steps reorder them, which is the aggressive variant.
//
// Aliased as `probeExcluded` for backward compatibility.

.probeExcluded(...)
// Deprecated alias for `readmitExcluded`. Same semantics; keep using it in
// existing policies. New policies should use `readmitExcluded`.

.forceInclude(idOrFn: string | string[] | ((u) => boolean), position?: 'head'|'tail' = 'tail')
// Always include matching upstream(s), bypassing prior filters. Useful for
// canary/maintenance modes where you explicitly want an upstream in rotation.
```

---

### 4.11 Cooldown & time-windowed exclusion

```ts
.cooldown(duration: Duration)
// Removes upstreams whose ctx.excludedSince[id] is within `duration` of ctx.now.
// I.e., once excluded, an upstream is held out for at least this long even if
// its metrics recover. Pairs naturally with readmitExcluded.
```

---

### 4.12 Combinators (control flow inside a chain)

```ts
.if(cond: boolean | ((arr) => boolean), thenFn: (arr) => Upstream[], elseFn?: (arr) => Upstream[])
.unless(cond: boolean | ((arr) => boolean), fn: (arr) => Upstream[])
.whenEmpty(fn: () => Upstream[])           // only invoke fn if chain currently empty
.whenNotEmpty(fn: (arr) => Upstream[])
.fallbackTo(arrOrFn: Upstream[] | ((ctx) => Upstream[]))   // if empty, replace with alternative
.ensureMin(n: number, fn: (arr) => Upstream[])              // if length < n, run fn to expand

.when(mask: number, fn: (arr) => Upstream[])
// Finality-conditional sub-chain. `mask` is a bitwise-OR of REALTIME |
// UNFINALIZED | FINALIZED | UNKNOWN (each a power of two). When the
// request's ctx.finality matches the mask, fn(this) runs; otherwise the
// array passes through. Typical use:
//   .when(REALTIME | UNFINALIZED | UNKNOWN,
//     u => u.stickyPrimary({ scope: NETWORK }))

.byFinality(handlers: {
  realtime?:    (arr) => Upstream[]
  unfinalized?: (arr) => Upstream[]
  finalized?:   (arr) => Upstream[]
  unknown?:     (arr) => Upstream[]
})
```

`byFinality` routes the chain to the handler matching `ctx.finality`.
A missing handler for the current finality bucket is a passthrough
(returns input unchanged), so `byFinality({ finalized: f })` only
branches on FINALIZED requests and is a no-op elsewhere.

Use case: the recurring "strict consensus on latest, trust-any on
finalized, custom for pending" pattern collapses to one expression
instead of nested `.if(ctx.finality === ..., ...)` calls.

---

### 4.13 Annotations & debug

```ts
.tap(fn: (arr) => void)                    // side effect; returns arr unchanged
.label(name: string)                       // names this chain step for clearer decision-record labeling
.dump(level?: 'debug'|'info'|'warn'|'error')     // log the current chain state via console
```

---

### 4.14 Module-level helpers (free functions, available as globals)

```ts
methodMatches(patternOrPatterns: string | string[]) : boolean
// Test ctx.method against glob patterns. Sugar over manual ctx.method checks.

isFinalityRequest() : boolean
// Sugar for ctx.finality === 'finalized'.

durationMs(d: Duration | string) : number
// Parse a duration string into ms.
```

**Predicate factories** (return a `(u) => boolean` predicate, used as the first argument to `.excludeIf` — full reference in §4.3a):

```ts
// Rates
errorRateAbove(rate)       / errorRateBelow(rate)
throttleRateAbove(rate)    / throttleRateBelow(rate)
misbehaviorRateAbove(rate)

// Latency (ms) — value is the first arg; quantile is optional (defaults to p70)
latencyAbove(ms, quantile?)

// Latency (relative; trip if p<q> > multiplier × fastest peer's p<q>)
latencyDeviationAbove(multiplier, quantile?)

// Lag (block counts)
blockNumberLagAbove(blocks)
finalizationLagAbove(blocks)

// Lag (wall-clock seconds via tracker's EMA block-time)
blockSecondsLagAbove(seconds)
finalizationSecondsLagAbove(seconds)

// Sample-size guards
samplesBelow(n) / samplesAbove(n)
```

**Combinators** (operate on predicates):

```ts
all(...preds: Predicate[]) : Predicate     // AND
any(...preds: Predicate[]) : Predicate     // OR
not(pred: Predicate)       : Predicate     // NOT
```

---

## 5. Runtime semantics

### 5.1 Engine architecture

```
policy.Engine (per project)
├── slot registry: map[(network, method)] -> *Slot
└── program cache: map[sourceHash] -> compiled sobek.Program

policy.Slot (per (network, method))
├── cache:           atomic.Pointer[[]Upstream]
├── ticker:          *time.Ticker (evalInterval)
├── crossTickState:  previousOrder, lastSwitchAt, excludedSince, tickCount, lastEvalAt
└── tick goroutine: snapshots metrics → builds ctx → runs eval → swaps cache → emits metrics + trace span
```

### 5.2 Tick lifecycle

1. **Snapshot.** Read `TrackedMetrics` for every upstream in the network (single lock acquisition per upstream).
2. **Build inputs.** Construct `upstreams[]` (Go-side `Upstream` mapped to sobek `Object`) and `ctx` (carrying cross-tick state from the slot).
3. **Execute.** Run the eval in a pooled sobek runtime with std-lib pre-installed. Capture rejection annotations as each std-lib step runs.
4. **Validate return.** Must be an array; each entry must be an upstream object originally from the input set (identity by `id`). On failure, emit an `invalid_return` error and keep prior cache.
5. **Compute decision diff.** Order vs `ctx.previousOrder`, primary changed?, excluded set changed?, sticky decisions made?
6. **Atomic swap.** `Slot.cache.Store(&ordered)`. Update crossTickState.
7. **Emit observability.** Prometheus counters (`policy_selection_*`) + OTLP tracing span per tick + change-only info log. Failure cases also bump the `policy_selection_eval_errors_total` counter.

Eval timeout (`evalTimeout`, default 100ms): on timeout the engine logs `kind=timeout`, keeps prior cache.

### 5.3 Triggered re-evaluation

In addition to the periodic timer, the engine re-evaluates a slot immediately on:

- Upstream added/removed from registry (subscribes to registry events).
- Config hot-reload that changes the slot's `eval`, presets, or interval.
- `POST /admin/selection/:net/:method/reeval`.

### 5.4 Request-time path

```go
func (n *Network) selectUpstreams(ctx context.Context, method string) ([]*Upstream, error) {
    ordered := n.policyEngine.GetOrdered(n.id, method)  // O(1) atomic load
    if len(ordered) == 0 {
        return nil, ErrNoEligibleUpstream
    }
    return ordered, nil
}
```

`GetOrdered`:

```go
func (e *Engine) GetOrdered(network, method string) []*Upstream {
    if slot, ok := e.slots[key(network, method)]; ok {
        return *slot.cache.Load()
    }
    // Fallback to (network, "*") if evalPerMethod=true but no method-specific slot
    if slot, ok := e.slots[key(network, "*")]; ok {
        return *slot.cache.Load()
    }
    return nil
}
```

No permit check, no cordon table lookup. The ordered list IS the gate.

### 5.5 Concurrency model

- `Slot.cache`: `atomic.Pointer[[]Upstream]`. Readers are wait-free.
- `Slot.crossTickState`: accessed only from the slot's tick goroutine — no locking.
- `Slot.ring`: protected by a small mutex; admin reads take a snapshot.
- One sobek runtime is pooled per engine (sobek runtimes are NOT safe for concurrent use; the pool guarantees serialized access).

### 5.6 Slot lifecycle

| Event | Effect |
|---|---|
| Network created | Engine creates `(network, "*")` slot. Initial eval runs synchronously before the network accepts traffic. |
| `evalPerMethod=true` and first request for new method | Engine lazily creates `(network, method)` slot. First eval runs synchronously. Until ready, requests use `(network, "*")` slot. |
| Network deleted | Engine drains slot, cancels ticker, releases runtime to pool. |
| Eval source changes (hot reload) | New compiled program replaces old in cache; current slot transitions to new program on next tick. Cross-tick state is preserved. |
| 3 consecutive eval failures | Slot falls back to default policy with a warning log. Hot reload of the user's policy resumes it. |

### 5.7 Failure semantics

| Eval outcome | Action | Metric |
|---|---|---|
| Returns valid `Upstream[]` | Swap cache, write decision | — |
| Returns empty `Upstream[]` | Valid; cache becomes empty; requests fail with `ErrNoEligibleUpstream` | — |
| Throws | Keep prior cache, log error | `erpc_selection_eval_errors_total{kind="throw"}` |
| Times out (>`evalTimeout`) | Keep prior cache, log error | `kind="timeout"` |
| Returns non-array / unknown ids | Keep prior cache, log error | `kind="invalid_return"` |
| 3 consecutive failures | Switch slot to default policy, warning log | `kind="fallback_default"` |

The "keep prior cache" property means a single bad eval never causes a routing outage; the worst case is stale routing for one tick.

---

## 6. Observability

Every tick emits standard observability signals. There is no
in-memory decision-record buffer and no `decisionHistory` config field;
the engine talks exclusively through Prometheus + tracing.

**Prometheus families** (one set per `(project, network, method)`):

```
policy_selection_eval_duration_seconds      # histogram
policy_selection_eval_errors_total{kind}    # throw|timeout|invalid_return
policy_selection_eligible_upstreams         # gauge
policy_selection_position{upstream}         # 0 = primary, 1+ = runner-ups, -1 = excluded
policy_selection_rejection_total{upstream,step}
policy_selection_primary_switch_total{from,to}
```

**OTLP tracing** (one span per tick, per `(network, method)`):

- attributes: `policy.tick_id`, `policy.network`, `policy.method`,
  `policy.eval_duration_ms`, `policy.upstream_count`,
  `policy.primary_changed`, `policy.order_changed`
- events: per-upstream exclusion with reason + step
- the per-request span gets `policy.selected_upstream` +
  `policy.tick_id` so the request span joins back to the tick span

When `evalFunc` throws or times out, the failing tick is logged at
warn level with `tick_id` for direct correlation to the tracing
span / metric increment.

---

## 7. Default policy

When `selectionPolicy.evalFunc` is omitted, the engine applies the production-hardened default embedded as `internal/policy/default_policy.js` (also served at `GET /admin/selection/default-policy`):

```js
(upstreams, ctx) =>
  upstreams
    .removeCordoned()                                                     // 1
    // 2 — health excludes, one per signal, all gated on sample volume
    .excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
    .excludeIf(all(samplesAbove(10), throttleRateAbove(0.4)))
    .excludeIf(any(all(samplesAbove(20), latencyDeviationAbove(3)), latencyAbove(30_000)))
    .excludeIf(any(blockNumberLagAbove(16), blockSecondsLagAbove(30)))
    .whenEmpty(() => upstreams)                                           // 3 — safety net
    .preferTag('!tier:fallback', { minHealthy: 1, fallback: 'tier:fallback' })  // 4
    .sortByScore(PREFER_FASTEST)                                          // 5 — rank survivors by p70 latency
    .stickyPrimary({ hysteresis: 0.30, minSwitchInterval: '30s' })        // 6 — hold primary stable
    .readmitExcluded({                                                    // 7 — cautious re-admit
      reAdmitAfter:  '90s',
      maxConcurrent: 2,
      position:      'tail',
    })
```

**Step-by-step rationale:**

1. **Drop cordoned** — failsafe / circuit-breaker has explicitly marked the upstream bad.

2. **Health excludes** — one `excludeIf` per signal. Each rule is intentionally loose: failsafe (retry / hedge / consensus) already absorbs routine error spurts, so the exclusion chain only needs to evict upstreams that are *fundamentally* broken. Anti-flap timing is centralized in step 7 (`readmitExcluded`) — every rule shares the same single re-admission window rather than carrying its own cooldown:
   - `all(samplesAbove(10), errorRateAbove(0.7))` — gated on ≥10 samples so a single failed call on a fresh-pod tracker (errorRate=1.0 on one request) can't cascade-evict every upstream before the rolling window has a meaningful denominator. The 0.7 ceiling recognizes that anything below "more than 70% erroring" is something failsafe can paper over.
   - `all(samplesAbove(10), throttleRateAbove(0.4))` — same sample guard. 0.4 catches a vendor visibly rate-limiting while letting the routine throttle-burst through.
   - `any(all(samplesAbove(20), latencyDeviationAbove(3)), latencyAbove(30_000))` — relative branch (`latencyDeviationAbove(3)`) trips when this upstream's p70 is >3× the fastest peer's p70, gated on ≥20 samples so the comparison is meaningful. Absolute branch (`latencyAbove(30_000)`) is unguarded — 30 s is catastrophic at any sample count. Both branches use p70 (the predicate factories default), which is the same quantile `sortByScore(PREFER_FASTEST)` ranks by — exclusion axis and rank axis agree on what "fast" means.
   - `any(blockNumberLagAbove(16), blockSecondsLagAbove(30))` — either 16 blocks OR 30 seconds behind tip is degraded. Block-count handles chains where wall-clock estimation isn't seeded yet; seconds-lag handles chains with wildly varying block times.

   Filtering on these signals (rather than only ranking via `sortByScore`) is the key defence against the "slow or erroring upstream still wins by score" class of incident.

3. **Safety net** — if step 2 wiped everything (network-wide outage), serve from the raw set rather than fail closed.

4. **Tier** — `!tier:fallback` is the primary tier; `tier:fallback` is the second tier. Adding a fallback upstream is one config line. The `!` glob is the canonical eRPC convention.

5. **Score** — `PREFER_FASTEST` weights (errorRate=4, respLatency=15, throttledRate=4, blockHeadLag=1, finalizationLag=0, misbehaviors=2). Ranks the survivors of step 2 by p70 latency, with the other dimensions as light tiebreakers.

6. **Sticky primary** — keep the current primary unless a challenger's score is decisively better (≥30% margin) AND 30 s have passed since the last switch. The 0.30 hysteresis is wider than a naive flap-defender because the chain is best-first by score — without a meaningful margin the primary would oscillate every tick on noise.

7. **Readmit excluded** — re-admit up to two excluded upstreams every 90 s at the **tail** of the order so they get a chance to prove they've recovered without taking primary traffic. Matches typical transient-blip recovery windows (rate-limit reset, regional networking dip). Placed at the end of the chain so subsequent sort/sticky steps can't reorder the probed upstreams off the tail.

`PREFER_FASTEST` weights are defined in §4.1.

**Loosening for low-traffic / dev environments.** Single-upstream setups, or environments where serving from a degraded upstream beats failing closed, can ship an explicit `evalFunc` that drops the `excludeIf` steps in favor of a single permissive `excludeIf(errorRateAbove(0.8))`. Pre-rewrite eRPC users who depended on permissive routing can recreate the old default via explicit `evalFunc`. The new default is "safe-for-most-prod"; we explicitly choose to surprise low-traffic users rather than let prod users suffer the inverse default.

---

## 8. Observability

### 8.1 Admin endpoints

```
GET  /admin/selection/:net                          latest decision (method=*)
GET  /admin/selection/default-policy                source of the embedded default policy
POST /admin/selection/:net/:method/reeval           force re-evaluation
```

The admin surface is intentionally minimal: there is no
`/admin/selection/<net>/<method>` endpoint exposing the latest
decision, no `?since=` log endpoint, no `/explain/:requestId`. All of
those are subsumed by Prometheus + tracing.

### 8.2 Prometheus metrics

```
policy_selection_position{project, network, method, upstream}                 gauge
  # 0 = primary; 1, 2, ... = runners-up; -1 = excluded this tick

policy_selection_rejection_total{project, network, method, upstream, step}    counter
  # increments each tick the upstream is rejected by this std-lib step

policy_selection_primary_switch_total{project, network, method, from, to}     counter

policy_selection_eval_duration_seconds{project, network, method}              histogram

policy_selection_eval_errors_total{project, network, method, kind}            counter
  # kind = "timeout" | "throw" | "invalid_return"

policy_selection_eligible_upstreams{project, network, method}                 gauge
  # current number of upstreams in the cached order (informational)
```

The underlying per-upstream metric gauges (`erpc_upstream_block_head_lag`, `erpc_upstream_error_rate`, latency quantiles, etc.) are emitted by the metrics tracker and are out of scope of this spec.

### 8.3 Logs

- **Per request** (existing request span/log): adds `policy.selected_upstream`, `policy.tick_id`, `policy.position`.
- **Per tick (info)**: emitted *only on change* — primary switched, excluded set changed, or eval error. Example:
  ```
  selection_change net=evm:1 method=eth_call primary=alchemy→infura
    cause="alchemy.blockHeadLag=8>5 → removeByLag"
    tick_id=evm:1/eth_call/1715600000123
  ```

### 8.4 Tracing

One span per tick (`policy.eval.tick`) carrying:

```
policy.tick_id              # network/method/tick_unix_ms
policy.network              # network ID
policy.method               # method matched
policy.eval_duration_ms     # how long the JS ran
policy.upstream_count       # input universe size
policy.primary_changed      # bool
policy.order_changed        # bool
```

Per-request spans add `policy.selected_upstream` + `policy.tick_id` to
join back to the owning eval. This is the canonical "why was upstream
X used at time T" path.

---

## 9. Validation & error handling

### 9.1 Config-load time

For each `selectionPolicy.eval`:

- Source must compile under sobek.
- Smoke run: invoke against an **empty** upstream array and a synthetic `ctx`. Must return an array (length 0 acceptable).
- Smoke run: invoke against a **two-upstream** synthetic array. Must return an array; entries must be from the input set.

Failures at this stage are **fatal** for the project's load (consistent with other config-validation errors).

### 9.2 Runtime errors

| Error | Handling |
|---|---|
| Eval throws | Keep prior cache; `eval_errors_total{kind="throw"}`; error log with stack |
| Eval times out | Keep prior cache; `kind="timeout"` |
| Returns non-array | Keep prior cache; `kind="invalid_return"` |
| Returns entries not in input set | Keep prior cache; `kind="invalid_return"` |
| 3 consecutive failures | Fall back to default policy; warning log; `kind="fallback_default"` |
| Empty array | Valid; cache becomes empty; requests fail fast with `ErrNoEligibleUpstream` |

The "keep prior cache" property is critical: a single bad eval never causes a routing outage; the worst case is one tick of stale routing.

---

## 10. Implementation plan

### 10.1 Package layout

```
internal/policy/
  engine.go              # Engine: registry, lifecycle, hot-reload coordination
  slot.go                # Slot: per-(network, method) state, cache, ticker
  eval.go                # execute one tick: snapshot → build ctx → run JS → validate
  decision.go            # decision record types
  default_policy.go      # //go:embed default_policy.js
  default_policy.js      # source of the default policy
  sticky.go              # cross-slot sticky-primary register
  runtime_pool.go        # pooled sobek runtimes
  testing.go             # harness for tests
  errors.go

internal/policy/stdlib/
  install.go             # sobek wiring: install presets, REASON_*, finality bits,
                         #   scope constants, durationMs; run stdlib.js
  stdlib.js              # all chainable Array.prototype methods + predicate
                         #   factories + combinators (single source of truth)

common/config.go                  # SelectionPolicyConfig type
common/defaults.go                # SelectionPolicyConfig.SetDefaults
erpc/networks.go                  # selectUpstreams calls engine.GetOrdered
```

### 10.2 Dependencies

- `github.com/grafana/sobek` — JS runtime, already in tree.
- `health.Tracker` — read-only consumer for metric snapshots.
- Existing prometheus registry, structured logger, admin HTTP mux.

### 10.3 Size estimate

| Component | Approx LOC |
|---|---|
| `engine.go` + `slot.go` + `eval.go` | 400 |
| `stdlib/*` (all files combined) | 700 |
| `decision.go` + `metrics.go` | 200 |
| `admin.go` | 150 |
| `default_policy.js` | 30 |
| Config + defaults wiring | 50 |
| **Total Go + JS** | **~1500** |
| Tests (unit + integration + golden) | ~1500 |

### 10.4 Build order

1. **Config types and validation** (`common/config.go`, `common/defaults.go`).
2. **Engine skeleton + slot lifecycle** (no eval yet — empty cache, no-op tick).
3. **sobek runtime wiring** (`stdlib/install.go`, runtime pool, ctx builder).
4. **Std-lib in dependency order**:
   - Generic (filter, reject, find, set ops) — no state.
   - Identity (where, by*, exclude*).
   - Health (removeBy*, keepHealthy).
   - Sort (sortBy*, sortByScore is the centerpiece).
   - Random, limit, group, debug, combinator.
   - Sticky/probe/cooldown (need ctx state).
5. **Decision records + diff + ring buffer**.
6. **Metrics + admin endpoints**.
7. **Default policy embedding**.
8. **Wire `Network.selectUpstreams` → `engine.GetOrdered`**.
9. **Integration tests**.

Each step is independently testable; std-lib functions can be unit-tested without the engine by directly invoking the Go implementations against synthetic upstream arrays.

---

## 11. Testing

### 11.1 Unit tests

- One test file per std-lib file. Each function tested against:
  - Empty input.
  - Single upstream.
  - Mixed groups/vendors/types.
  - Edge cases per metric (NaN, zero requests, all errors, etc.).
  - Annotation correctness on rejected upstreams.

### 11.2 Integration tests

`EngineHarness` runs a real `policy.Engine` against a fake `health.Tracker`, allowing tests to:

- Drive metric updates over time.
- Tick the engine programmatically (no wall clock).
- Assert decision records tick-by-tick.
- Assert atomic-cache reads under concurrent forward traffic.

### 11.3 Policy-scenario tests

The default policy is exercised against canonical fixtures:

| Scenario | Expectation |
|---|---|
| All healthy, two upstreams | Both ordered by score, sticky kept |
| Primary degrades (error rate spikes) | Sticky retains until hysteresis exceeded, then switches |
| Primary recovers within minSwitchInterval | No switch back |
| All `tier:default` upstreams unhealthy, `tier:fallback` healthy | `preferTag('!tier:fallback', { fallback: 'tier:fallback' })` switches to fallback |
| Lag spike on one upstream | `excludeIf(any(blockNumberLagAbove(16), blockSecondsLagAbove(30)))` excludes it; `readmitExcluded` re-admits later |
| Eval throws | Cache preserved, error metric emitted |
| Eval times out | Same |
| New upstream added | Re-eval triggered, new upstream appears in next cache |

### 11.4 Concurrency tests

Race-detector test: forward 10k requests across 100 goroutines while the ticker fires; assert:

- No torn reads of the ordered list.
- Every recorded `decision_id` resolves in the ring buffer.
- No data race reported.

### 11.5 Golden-file tests

For each fixture scenario, snapshot the decision record to `testdata/decisions/<scenario>.json`. Catches regressions in:

- Annotation text and labels.
- Penalty breakdown numeric stability.
- Sparse-retention behavior.

### 11.6 Benchmarks

- `BenchmarkTick_4Upstreams` — single-tick latency for the default policy.
- `BenchmarkTick_50Upstreams` — high upstream count.
- `BenchmarkGetOrdered` — request-path cache read.
- Target: tick latency < 1 ms for 50 upstreams; `GetOrdered` < 50 ns.

---

## 12. Examples

### 12.1 Cost-aware routing

```js
return upstreams
  .removeByLag({ blockHead: 10 })
  .preferTag('tier:cheap', { minHealthy: 2, fallback: 'tier:premium' })
  .sortByScore(PREFER_FASTEST)
  .stickyPrimary({ hysteresis: 0.15, minSwitchInterval: '1m' })
```

### 12.2 Latency-critical, per-method

```yaml
selectionPolicy:
  evalPerMethod: true
  evalFunc: |
    if (methodMatches(['eth_call', 'eth_getLogs'])) {
      return upstreams
        .removeByErrorRate(0.05)
        .removeByLatency({ p95Ms: 1500 })
        .sortByLatency('p95')
        .pickTop(3)
    }
    return upstreams.sortByScore(PREFER_FASTEST).stickyPrimary()
```

### 12.3 Pure round-robin (zero scoring overhead)

```js
return upstreams.rotateBy(Math.floor(ctx.now / 1000))
```

### 12.4 Vendor diversification (via explicit vendor tags)

Tag your upstreams with `vendor:<name>` and:

```js
return upstreams
  .sortByScore(PREFER_FASTEST)
  .spreadAcrossTags('vendor:')
  .stickyPrimary({ hysteresis: 0.10 })
```

### 12.5 Strict primary tier, fallback only when nothing healthy

```js
const primaries = upstreams
  .byTag('tier:primary')
  .removeByErrorRate(0.10)
  .removeByLag({ blockHead: 5 })

if (primaries.length > 0) {
  return primaries.sortByScore(PREFER_FASTEST).stickyPrimary()
}
return upstreams.byTag('tier:fallback').sortByScore(PREFER_FASTEST)
```

### 12.6 Canary in rotation

```js
return upstreams
  .sortByScore(PREFER_FASTEST)
  .forceInclude('canary-node', 'tail')        // always probe, even if score is bad
  .stickyPrimary()
```

### 12.7 Time-of-day routing

```js
const hourUTC = new Date(ctx.now).getUTCHours()
const cheap = hourUTC >= 0 && hourUTC < 6
return upstreams
  .if(cheap,
    arr => arr.preferTag('tier:cheap',   { fallback: 'tier:premium' }),
    arr => arr.preferTag('tier:premium'))
  .sortByScore(PREFER_FASTEST)
  .stickyPrimary()
```

### 12.8 Cooldown after exclusion

```js
return upstreams
  .removeByErrorRate(0.2)
  .cooldown('1m')                             // once excluded, hold out at least 1 min
  .sortByScore(PREFER_FASTEST)
  .stickyPrimary()
  .readmitExcluded({ reAdmitAfter: '5m', maxConcurrent: 1 })
```

### 12.9 Multi-tier with explicit ordering

```js
// Each upstream is tagged `tier:primary` / `tier:secondary` / `tier:fallback`.
const priority = { 'tier:primary': 0, 'tier:secondary': 1, 'tier:fallback': 2 }
return upstreams
  .removeCordoned()
  .sortByScore(PREFER_FASTEST)
  .sortBy(u => {
    for (const t of u.tags || []) {
      if (priority[t] != null) return priority[t]
    }
    return 99
  })
  .stickyPrimary({ hysteresis: 0.10, minSwitchInterval: '30s' })
```

### 12.10 Fully imperative escape hatch

```js
const sorted = upstreams.sortByScore(PREFER_FASTEST)
const result = []
for (const u of sorted) {
  if (u.metrics.errorRate > 0.5) continue
  if ((u.tags || []).includes('tier:experimental') && ctx.finality === 'finalized') continue
  result.push(u)
}
return result
```

---

## 13. Open questions (resolve before merge)

- **Cross-network policies.** Should there be a project-level eval that can see all networks (for global vendor budgets)? Out of scope for v1; revisit if needed.
- **Per-upstream overrides.** Should an upstream be able to declare "always include me" in its own config? Currently expressible via `.forceInclude` in the policy; consider syntactic sugar.
- **Hot-reloading the std-lib.** v1 ships the std-lib as Go code; std-lib changes require a binary rebuild. Future: consider exposing a user-supplied prelude.
- **Per-request policy override.** Headers like `X-eRPC-Force-Upstream` are out of scope of selection policy and remain in the existing request-handling layer.

---

## 14. Glossary

- **Slot** — Engine state for a single `(network, method)` pair: cache + ticker + ring buffer + cross-tick state.
- **Cross-tick state** — `previousOrder`, `lastSwitchAt`, `excludedSince`, `tickCount`. The only state that survives across ticks. Carried in `ctx`.
- **Decision** — The structured record of one tick's evaluation: ordered list + excluded list with reasons + sticky decisions.
- **Sticky primary** — A position-0 upstream kept across ticks even if a marginally-better challenger appears, to avoid flapping.
- **Probe / resample** — Periodic re-admission of an excluded upstream so its metrics refresh.
- **Penalty** — Intermediate quantity inside `sortByScore`: weighted sum of metric values. Lower = healthier.
- **Score** — Final per-upstream rank value attached by `sortByScore`: `overall(u) / (1 + penalty(u))`. HIGHER = better; the chain is sorted best-first.

