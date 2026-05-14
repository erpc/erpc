# Selection Policy — Specification

**Status**: Ready for implementation
**Owner**: TBD
**Last revised**: 2026-05-13

---

## 1. Purpose

The **Selection Policy** is the sole mechanism that decides which upstreams handle which requests and in what order. It runs as a per-network (optionally per-method) background loop. Each tick takes a snapshot of upstream identity, configuration, and tracked metrics, executes a user-defined JavaScript evaluation function, and produces an **ordered list of upstreams**. That list is the routing decision: requests are served from the head of the list, falling through to subsequent entries on retry/failure. Upstreams not present in the returned list are excluded for that tick.

The evaluation function has access to a Go-implemented standard library exposed as chainable methods on the upstream array. Authors compose built-in operators (`sortByScore`, `removeByLag`, `preferGroup`, `stickyPrimary`, etc.) or drop into plain JS when needed. The default policy is a single chain covering the common case.

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
      decisionHistory: 5m       # rolling decision-record log.    default: 5m
      evalTimeout: 100ms        # per-tick eval hard cap.         default: 100ms
      eval: |
        return upstreams
          .removeByLag({ blockHead: 5, finalization: 50 })
          .sortByScore(BALANCED)
          .stickyPrimary({ hysteresis: 0.10, minSwitchInterval: '30s' })
          .probeExcluded({ reAdmitAfter: '5m', maxConcurrent: 1 })
```

| Field | Type | Description |
|---|---|---|
| `evalInterval` | `Duration` | How often the eval runs. Default `1s`. |
| `evalPerMethod` | `bool` | If true, separate eval + cache per `(network, method)`. Default false. |
| `decisionHistory` | `Duration` | Retention window for the decision-record ring buffer (per slot). Default `5m`. |
| `evalTimeout` | `Duration` | Hard wall-clock cap on each eval. Default `100ms`. |
| `eval` | `string` | JavaScript function body. Must `return` an `Upstream[]`. If omitted, the [default policy](#7-default-policy) applies. |

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
  readonly endpoint: string                // redacted

  readonly config: UpstreamConfig          // raw user config (includes user-set `group`, etc.)
  readonly metrics: UpstreamMetrics        // snapshot taken at the start of this tick

  // Attached by std-lib steps; readable by subsequent steps and visible in decision records.
  readonly score?: number                  // set by sortByScore (lower = better)
  readonly penaltyBreakdown?: {            // set by sortByScore
    errorRate: number
    respLatency: number
    throttledRate: number
    blockHeadLag: number
    finalizationLag: number
    misbehaviors: number
    overall: number
  }
  readonly annotations?: string[]          // set by .annotate() / .mark() / std-lib steps
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
  cordonedReason: string | null            // set by external systems (failsafe circuit breaker)
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
| `BALANCED` | `8, 4, 3, 2, 1, 6` |
| `PREFER_FASTER` | `4, 12, 4, 1, 0, 2` |
| `PREFER_FEWER_ERRORS` | `15, 2, 6, 2, 1, 12` |
| `PREFER_FRESHER_HEAD` | `4, 2, 2, 15, 8, 3` |
| `PREFER_LESS_THROTTLED` | `8, 4, 15, 2, 1, 4` |
| `PREFER_CHEAP` | (no weights — alias for `BALANCED`; intended pairing with `preferGroup('cheap')`) |

#### Finality states

`REALTIME`, `UNFINALIZED`, `FINALIZED`, `UNKNOWN` — match values of `ctx.finality`.

#### Reasons (for cordoning/exclusion annotations)

`REASON_LAG`, `REASON_ERROR_RATE`, `REASON_LATENCY`, `REASON_THROTTLING`, `REASON_MISBEHAVIOR`, `REASON_CORDONED`, `REASON_GROUP`, `REASON_VENDOR`, `REASON_USER_FILTER` — used internally by std-lib steps when annotating rejections; available to user code as well.

---

### 4.2 Identity & label selection

```ts
.where(filter: { id?, group?, vendor?, type?, finality? })
.whereNot(filter: same as above)
.byId(id: string | string[])
.excludeId(id: string | string[])
.byGroup(name: string | string[])
.excludeGroup(name: string | string[])
.byVendor(name: string | string[])
.excludeVendor(name: string | string[])
.byType(type: string | string[])
```

Filter values support glob patterns: `byGroup('primary*')`, `byVendor('!alchemy')` (`!` = negation). Multiple fields in `.where({...})` are AND-ed; multiple values in an array are OR-ed.

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

Convenience composite:

```ts
.keepHealthy({
  maxErrorRate?: number   = 0.5,
  maxBlockHeadLag?: number = 10,
  maxP95Ms?: number       = 5000,
  maxThrottledRate?: number = 0.3,
})
```

---

### 4.4 Generic functional

```ts
.filter(fn: (u, idx, arr) => boolean, label?: string)
.reject(fn: (u, idx, arr) => boolean, label?: string)         // inverse of .filter
.find(fn: (u) => boolean) : Upstream | null
.partition(fn: (u) => boolean) : [Upstream[], Upstream[]]     // [matching, non-matching]
.unique(keyFn?: (u) => any)                                   // dedupe by id, or by keyFn
.concat(other: Upstream[])                                    // append; allows duplicates
.union(other: Upstream[])                                     // set union by id
.intersect(other: Upstream[])                                 // set intersection by id
.difference(other: Upstream[])                                // a \ b by id (alias: .except)
.slice(start: number, end?: number)
.reverse()
.length : number       // accessor (zero-arg method also works)
.isEmpty : boolean
.toArray() : Upstream[]                                       // explicit conversion
```

User-supplied `.filter` and `.reject` are auto-labeled by source position when no explicit label is given (e.g. `filter@evalLine:7`).

---

### 4.5 Sorting

The primary scoring entry point:

```ts
.sortByScore(
  weights: ScoreWeights | preset | ((u: Upstream) => ScoreWeights),
  opts?: {
    decay?: number              // EMA decay across ticks. default: 0.7
    latencyQuantile?: 'p50'|'p70'|'p90'|'p95'|'p99'   // default: 'p70'
    tieBreaker?: 'id' | 'random' | ((a, b) => number) // default: 'id'
    overall?: (u: Upstream) => number   // per-upstream multiplier on final penalty (>1 = penalize more, <1 = boost)
  }
) : Upstream[]
```

`ScoreWeights = { errorRate?, respLatency?, throttledRate?, blockHeadLag?, finalizationLag?, misbehaviors? }`. Missing keys default to 0. The first argument can be either:

- a **flat weight map** applied to every upstream (e.g. `{ errorRate: 8, respLatency: 4 }`);
- a **preset constant** like `BALANCED` or `PREFER_FASTER`;
- a **function** `(u) => ScoreWeights` returning per-upstream weights (useful when different upstream types or vendors merit different metric emphasis, or for migrating per-upstream multiplier config).

`penalty = Σ(metric × weight(u)) × overall(u)`, EMA-smoothed across ticks. Lower penalty = higher rank. Each upstream's resulting `score` and `penaltyBreakdown` are attached and visible to subsequent steps and in decision records.

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
.sortByRequestsTotal({ desc?: boolean = true })            // most experienced first by default
.sortByGroup(order: string[])              // explicit group ordering; groups not in `order` go last
.sortByVendor(order: string[])             // same idea for vendors
.sortById(order: string[])                 // explicit id ordering
```

Manual score adjustments (must run after `sortByScore` — operate on the attached `score`):

```ts
.boostBy(fn: (u) => number)                // multiplies u.score by fn(u); re-sorts
.penalizeBy(fn: (u) => number)             // divides u.score by fn(u); re-sorts
.boostByGroup(name: string, factor: number)
.boostByVendor(name: string, factor: number)
```

---

### 4.6 Randomization & rotation

```ts
.shuffle(seed?: number)
.randomize(seed?: number)                  // alias of shuffle
.rotateBy(n: number)                       // n may be derived from ctx.now for time-based RR
.weightedRandom(weightFn: (u) => number, seed?: number)   // weighted permutation
```

---

### 4.7 Stability (cross-tick)

```ts
.stickyPrimary({
  hysteresis?: number = 0.10,          // challenger must be this fraction "better" (lower score)
  minSwitchInterval?: Duration = '30s' // cooldown between switches
})
// Reads ctx.previousOrder[0] and ctx.lastSwitchAt. If a prior sortByScore ran,
// uses the attached `score` for the comparison; otherwise uses position only.
// Annotates the kept/switched primary with the reason.

.stickyOrder({
  hysteresis?: number = 0.05,
  minSwitchInterval?: Duration = '15s'
})
// Stabilizes the FULL order (not just position 0). Useful when retries cascade
// and you don't want positions 1, 2, 3 to flip-flop either.

.keepRecentPrimary(duration: Duration)
// If ctx.previousOrder[0] is still in the chain and within `duration` of
// ctx.lastSwitchAt, force it to position 0 regardless of score.
```

---

### 4.8 Grouping & multi-tier

```ts
.groupBy(keyFnOrField: ((u) => string) | keyof Upstream | 'group' | 'vendor' | 'type')
  : Group[]   // each Group is itself chainable as Upstream[]

// On Group[]:
.flat()                                    // back to Upstream[]
.pickTopPerGroup(n: number)                // keep first n in each group, flatten
.pickFromEachGroup(n: number)              // alias
.balanceAcrossGroups()                     // round-robin merge: [g1[0], g2[0], g1[1], g2[1], ...]
.interleaveGroups(weights?: number[])      // weighted round-robin
.sortGroupsBy(fn: (group) => number)
.mapGroups(fn: (group) => Upstream[])      // apply per-group transformation, flatten

// On Upstream[] (no prior groupBy):
.interleave(other: Upstream[])             // round-robin merge of two arrays
```

Convenience preference operators (the most common multi-tier patterns):

```ts
.preferGroup(name: string, opts?: {
  minHealthy?: number = 1,
  fallback?: string                        // group name; if minHealthy not met, switch to fallback group
})
.preferVendor(name: string, opts?: same)
.preferAttribute(
  keyFn: (u) => any,
  value: any,
  opts?: same
)
// Generic form: prefer upstreams where keyFn(u) === value.
```

---

### 4.9 Slicing & limits

```ts
.pickTop(n: number)                        // first n
.pickBottom(n: number)                     // last n
.dropTop(n: number)                        // skip first n
.dropBottom(n: number)                     // skip last n
.at(index: number) : Upstream | null
.head : Upstream | null                    // accessor
.tail : Upstream[]                         // all but head
.last : Upstream | null
.take(n)                                   // alias for pickTop
.skip(n)                                   // alias for dropTop
```

---

### 4.10 Probing & forced inclusion

```ts
.probeExcluded({
  reAdmitAfter: Duration,                  // re-admit upstreams that have been excluded for at least this long
  maxConcurrent?: number = 1,              // cap on simultaneously re-admitted upstreams per tick
  longestFirst?: boolean = true,           // prioritize upstreams excluded the longest
  position?: 'tail' | 'head' | 'random' = 'tail'
})
// Deterministic re-admission of excluded upstreams. Each tick, find upstreams
// whose ctx.excludedSince is at least `reAdmitAfter` old, sort by exclusion age
// (oldest first if longestFirst), pick up to `maxConcurrent`, and insert at
// the specified position so they receive traffic and their metrics refresh.

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
// its metrics recover. Pairs naturally with probeExcluded.

.warmup(duration: Duration)
// Removes upstreams whose ctx.excludedSince[id] is null OR re-admission
// happened within `duration`. Use when you want new/recovered upstreams to
// "prove themselves" via probe traffic before getting full ranking.
```

---

### 4.12 Combinators (control flow inside a chain)

```ts
.if(cond: boolean | ((arr) => boolean), thenFn: (arr) => Upstream[], elseFn?: (arr) => Upstream[])
.unless(cond: boolean | ((arr) => boolean), fn: (arr) => Upstream[])
.whenEmpty(fn: () => Upstream[])           // only invoke fn if chain currently empty
.whenNotEmpty(fn: (arr) => Upstream[])
.fallbackTo(arrOrFn: Upstream[] | ((ctx) => Upstream[]))   // if empty, replace with alternative
.coalesce(...arrs: Upstream[][])                            // first non-empty wins; chain continues
.ensureMin(n: number, fn: (arr) => Upstream[])              // if length < n, run fn to expand
```

---

### 4.13 Annotations & debug

```ts
.tap(fn: (arr) => void)                    // side effect; returns arr unchanged
.annotate(fn: (u) => string)               // attaches note to each upstream (visible in decision record)
.label(name: string)                       // names this chain step for clearer decision-record labeling
.mark(predicate: (u) => boolean, note: string)   // annotate only matching upstreams
.dump(level?: 'debug'|'info'|'warn'|'error')     // log the current chain state via console
```

---

### 4.14 Module-level helpers (free functions, available as globals)

```ts
upstreamsFromIds(ids: string[]) : Upstream[]
// Build a chainable from the original input set by ids.

methodMatches(patternOrPatterns: string | string[]) : boolean
// Test ctx.method against glob patterns. Sugar over manual ctx.method checks.

isFinalityRequest() : boolean
// Sugar for ctx.finality === FINALIZED.

inWindow(start: string, end: string, tz?: string) : boolean
// Time-of-day window check using ctx.now. Useful for cost/load scheduling.

durationMs(d: Duration | string) : number
// Parse a duration string into ms.

weightedPickIndex(weights: number[], seed?: number) : number
// Lower-level helper used by weightedRandom.
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
├── crossTickState:  previousOrder, lastSwitchAt, excludedSince, tickCount
├── ring:            decision record ring buffer (size = decisionHistory / evalInterval)
└── tick goroutine: snapshots metrics → builds ctx → runs eval → swaps cache → appends decision
```

### 5.2 Tick lifecycle

1. **Snapshot.** Read `TrackedMetrics` for every upstream in the network (single lock acquisition per upstream).
2. **Build inputs.** Construct `upstreams[]` (Go-side `Upstream` mapped to sobek `Object`) and `ctx` (carrying cross-tick state from the slot).
3. **Execute.** Run the eval in a pooled sobek runtime with std-lib pre-installed. Capture rejection annotations as each std-lib step runs.
4. **Validate return.** Must be an array; each entry must be an upstream object originally from the input set (identity by `id`). On failure, emit an `invalid_return` error and keep prior cache.
5. **Compute decision diff.** Order vs `ctx.previousOrder`, primary changed?, excluded set changed?, sticky decisions made?
6. **Build decision record.** §6.
7. **Atomic swap.** `Slot.cache.Store(&ordered)`. Append decision to ring. Update crossTickState.
8. **Emit observability.** Metrics counters + change-only info log. Per-tick debug log of decision record (gated).

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

## 6. Decision record

Every tick produces a decision, retained for `decisionHistory`:

```jsonc
{
  "id": "evm:1/eth_call/1715600000123",
  "tickAt": "2026-05-13T14:32:00.123Z",
  "evalDurationMs": 0.4,

  "input": {
    "upstreamCount": 4,
    "ctx": { "method": "eth_call", "finality": "realtime", ... }
  },

  "ordered": [
    {
      "id": "alchemy",
      "position": 0,
      "score": 0.42,
      "penaltyBreakdown": {
        "errorRate": 0.08, "respLatency": 0.30,
        "throttledRate": 0.00, "blockHeadLag": 0.04,
        "finalizationLag": 0.00, "misbehaviors": 0.00,
        "overall": 0.42
      },
      "metrics": { ... },
      "annotations": [
        "stickyPrimary: kept (margin 4% < needed 10%)"
      ]
    },
    {
      "id": "infura",
      "position": 1,
      "score": 0.46,
      "metrics": { ... }
    }
  ],

  "excluded": [
    {
      "id": "quicknode",
      "metrics": { ... },
      "rejectedBy": "removeByLag",
      "reason": "blockHeadLag=12 exceeds threshold 5",
      "excludedSince": "2026-05-13T14:18:00.000Z"
    },
    {
      "id": "drpc",
      "metrics": { ... },
      "rejectedBy": "filter@evalLine:7",
      "reason": "u.group === 'fallback' && primaryHealthy >= 2",
      "excludedSince": "2026-05-13T14:30:00.000Z"
    }
  ],

  "sticky": {
    "primary": "alchemy",
    "lastSwitchAt": "2026-05-13T14:18:00Z",
    "switchedThisTick": false,
    "challenger": "infura",
    "challengerScoreMargin": 0.04,
    "switchThreshold": 0.10
  },

  "changes": {
    "primaryChanged": false,
    "orderChanged": false,
    "excludedSetChanged": false
  }
}
```

Each std-lib step is responsible for tagging its rejections and annotations as it runs. User-supplied `.filter()` / `.reject()` are auto-labeled by AST source position (`filter@evalLine:N`) when no explicit label is given.

The ring buffer is **sparse**: only ticks where `changes.primaryChanged || changes.orderChanged || changes.excludedSetChanged` is true are kept long-term. The last 60 seconds of unchanged ticks are kept verbatim for "what happened just now" queries; older unchanged ticks are coalesced into "no change from <id>" pointers.

---

## 7. Default policy

When `selectionPolicy.eval` is omitted, the engine applies the production-ready default embedded as `internal/policy/default_policy.js` (also served at `GET /admin/selection/default-policy`):

```js
(upstreams, ctx) =>
  upstreams
    .removeCordoned()                  // 1. drop failsafe-cordoned
    .removeByErrorRate(0.8)            // 2. drop > 80% errors over window
    .whenEmpty(() => upstreams)        // 3. safety net: never empty
    .preferGroup('!fallback', {        // 4. primary tier = group !== 'fallback'
      minHealthy: 1,
      fallback: 'fallback',
    })
    .sortByScore(BALANCED)             // 5. rank by composite score
    .stickyPrimary({                   // 6. anti-flap
      hysteresis: 0.10,
      minSwitchInterval: '30s',
    })
    .probeExcluded({                   // 7. recover excluded upstreams
      reAdmitAfter: '5m',
      maxConcurrent: 1,
    })
```

**Step-by-step rationale:**

1. **Drop cordoned** — failsafe / circuit-breaker has explicitly marked the upstream bad.
2. **Drop clearly broken** — `errorRate > 0.8` is broken on any chain. Conservative threshold to avoid false positives.
3. **Safety net** — if filtering wiped everything (network-wide outage), serve from the raw set rather than fail closed.
4. **Tier** — `group !== 'fallback'` is the primary tier; `group === 'fallback'` is the second tier. Adding a fallback upstream is one config line. The `!` glob is the canonical eRPC convention.
5. **Score** — `BALANCED` weights (errorRate=8, respLatency=4, throttledRate=3, blockHeadLag=2, finalizationLag=1, misbehaviors=6). Chain-specific signals (lag, latency) contribute to *ranking*, not *filtering*, so this default works on Ethereum mainnet, L2s, sidechains, testnets alike.
6. **Sticky primary** — keep the current primary unless a challenger's score is ≥10% better AND 30s have passed since the last switch. Prevents flapping under transient blips.
7. **Probe excluded** — every tick, re-admit one upstream that has been out for 5+ minutes so its metrics refresh.

`BALANCED` weights are defined in §4.1.

**Tuning under prod load.** The conservative default may be too forgiving for high-traffic environments. Operators running tight finality SLOs should consider a stricter variant — `keepHealthy(...)` composite in step 2, an explicit `removeByLag` step before scoring, finality-conditional `stickyPrimary`, and a shorter probe re-admit window. The default ships *conservative* on purpose so it doesn't surprise users with aggressive exclusion; the "production-tuned" variant lives in the docs as a recommended override, not the default.

---

## 8. Observability

### 8.1 Admin endpoints

```
GET  /admin/selection/:net                          latest decision (method=*)
GET  /admin/selection/:net/:method                  latest decision
GET  /admin/selection/:net/:method?at=<RFC3339>     decision active at timestamp
GET  /admin/selection/:net/:method?since=<dur>      sparse log of decisions in window
GET  /admin/selection/:net/:method/state            current cache + crossTickState (debug)
GET  /admin/selection/explain/:requestId            request -> decision -> reasons
GET  /admin/selection/default-policy                source of the embedded default policy
POST /admin/selection/:net/:method/reeval           force re-evaluation
POST /admin/selection/:net/:method/reset            drop crossTickState, re-eval immediately
```

`/explain/:requestId` is the operator's primary tool for "why was upstream X used at time T?" — it joins the request's recorded `decision_id` to the decision record and renders the full reasoning.

### 8.2 Prometheus metrics

```
erpc_selection_position{project, network, method, upstream}                 gauge
  # 0 = primary; 1, 2, ... = runners-up; -1 = excluded this tick

erpc_selection_rejection_total{project, network, method, upstream, step}    counter
  # increments each tick the upstream is rejected by this std-lib step

erpc_selection_primary_switch_total{project, network, method, from, to}     counter

erpc_selection_eval_duration_seconds{project, network, method}              histogram

erpc_selection_eval_errors_total{project, network, method, kind}            counter
  # kind = "timeout" | "throw" | "invalid_return" | "fallback_default"

erpc_selection_eligible_upstreams{project, network, method}                 gauge
  # current number of upstreams in the cached order (informational)
```

The underlying per-upstream metric gauges (`erpc_upstream_block_head_lag`, `erpc_upstream_error_rate`, latency quantiles, etc.) are emitted by the metrics tracker and are out of scope of this spec.

### 8.3 Logs

- **Per request** (existing request span/log): add `selection.upstream`, `selection.decision_id`, `selection.position`, `selection.network`, `selection.method`.
- **Per tick (debug)**: full decision record. Gated by `decisionHistory > 0` and debug log level.
- **Per tick (info)**: emitted *only on change* — primary switched, excluded set changed, or eval error. Example:
  ```
  selection_change net=evm:1 method=eth_call primary=alchemy→infura
    cause="alchemy.blockHeadLag=8>5 → removeByLag"
    decision=evm:1/eth_call/1715600000123
  ```

### 8.4 Tracing

Upstream-forward span attributes:

```
selection.upstream           # id of the upstream actually used
selection.decision_id        # for joining to decision records
selection.position           # 0 = primary, 1+ = failover position used
selection.alternatives       # count of other eligible upstreams at decision time
```

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
  slot.go                # Slot: per-(network, method) state, cache, ticker, ring buffer
  eval.go                # execute one tick: snapshot → build ctx → run JS → validate
  decision.go            # decision record types, diff, sparse retention
  metrics.go             # prometheus emissions
  admin.go               # /admin/selection/* HTTP handlers
  default_policy.go      # //go:embed default_policy.js + presets
  default_policy.js      # source of the default policy
  errors.go

internal/policy/stdlib/
  install.go             # sobek wiring: install methods, constants, helpers
  identity.go            # where, byId, byGroup, ...
  health.go              # removeBy*, keepHealthy
  generic.go             # filter, reject, find, partition, set ops
  sort.go                # sortByScore (the big one), sortBy*, boost/penalize
  random.go              # shuffle, rotateBy, weightedRandom
  sticky.go              # stickyPrimary, stickyOrder, keepRecentPrimary
  group.go               # groupBy + Group methods + preferGroup
  limit.go               # pickTop, dropTop, at, head, tail, ...
  probe.go               # probeExcluded, forceInclude
  cooldown.go            # cooldown, warmup
  combinator.go          # if, unless, whenEmpty, fallbackTo, coalesce, ensureMin
  debug.go               # tap, annotate, label, mark, dump
  helpers.go             # methodMatches, upstreamsFromIds, durationMs, ...
  presets.go             # BALANCED, PREFER_*, REASON_*, finality constants

internal/policy/testing/
  harness.go             # EngineHarness for integration tests
  fixtures.go            # synthetic upstream+metric scenarios

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
| All "default" group unhealthy, "fallback" group healthy | `preferGroup` switches to fallback |
| Lag spike on one upstream | `removeByLag` excludes it; resampling re-admits later |
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
  .preferGroup('cheap', { minHealthy: 2, fallback: 'premium' })
  .sortByScore(BALANCED)
  .stickyPrimary({ hysteresis: 0.15, minSwitchInterval: '1m' })
```

### 12.2 Latency-critical, per-method

```yaml
selectionPolicy:
  evalPerMethod: true
  eval: |
    if (methodMatches(['eth_call', 'eth_getLogs'])) {
      return upstreams
        .removeByErrorRate(0.05)
        .removeByLatency({ p95Ms: 1500 })
        .sortByLatency('p95')
        .pickTop(3)
    }
    return upstreams.sortByScore(BALANCED).stickyPrimary()
```

### 12.3 Pure round-robin (zero scoring overhead)

```js
return upstreams.rotateBy(Math.floor(ctx.now / 1000))
```

### 12.4 Vendor diversification

```js
return upstreams
  .sortByScore(BALANCED)
  .groupBy('vendor').pickTopPerGroup(1)
  .stickyPrimary({ hysteresis: 0.10 })
```

### 12.5 Strict primary group, fallback only when nothing healthy

```js
const primaries = upstreams
  .byGroup('primary')
  .removeByErrorRate(0.10)
  .removeByLag({ blockHead: 5 })

if (primaries.length > 0) {
  return primaries.sortByScore(BALANCED).stickyPrimary()
}
return upstreams.byGroup('fallback').sortByScore(PREFER_FASTER)
```

### 12.6 Canary in rotation

```js
return upstreams
  .sortByScore(BALANCED)
  .forceInclude('canary-node', 'tail')        // always probe, even if score is bad
  .stickyPrimary()
```

### 12.7 Boost a specific vendor

```js
return upstreams
  .sortByScore(BALANCED)
  .boostByVendor('alchemy', 0.5)              // 2x preference (penalty × 0.5)
  .stickyPrimary()
```

### 12.8 Time-of-day routing

```js
const cheap = inWindow('00:00', '06:00', 'UTC')
return upstreams
  .if(cheap,
    arr => arr.preferGroup('cheap', { fallback: 'premium' }),
    arr => arr.preferGroup('premium'))
  .sortByScore(BALANCED)
  .stickyPrimary()
```

### 12.9 Holdout for newly-added upstream

```js
return upstreams
  .warmup('5m')                               // exclude upstreams added in last 5 min
  .sortByScore(BALANCED)
  .stickyPrimary()
```

### 12.10 Cooldown after exclusion

```js
return upstreams
  .removeByErrorRate(0.2)
  .cooldown('1m')                             // once excluded, hold out at least 1 min
  .sortByScore(BALANCED)
  .stickyPrimary()
  .probeExcluded({ reAdmitAfter: '5m', maxConcurrent: 1 })
```

### 12.11 Multi-tier with explicit ordering

```js
return upstreams
  .removeCordoned()
  .groupBy('group')
  .sortGroupsBy(g => ({ primary: 0, secondary: 1, fallback: 2 }[g[0].config.group] ?? 99))
  .mapGroups(g => g.sortByScore(BALANCED))
  .flat()
  .stickyOrder({ hysteresis: 0.10, minSwitchInterval: '30s' })
```

### 12.12 Fully imperative escape hatch

```js
const sorted = upstreams.sortByScore(BALANCED)
const result = []
for (const u of sorted) {
  if (u.metrics.errorRate > 0.5) continue
  if (u.config.group === 'experimental' && ctx.finality === FINALIZED) continue
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
- **Penalty** — Output of `sortByScore`: weighted sum of metric values. Lower = better.
- **Score** — Synonym for penalty in the decision record (lower = better). Naming is "score" externally for legibility; internally the math is penalty.

