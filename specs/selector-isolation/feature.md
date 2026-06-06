# Selector-Scoped Isolation — Specification

> Dynamically isolate same-chain node groups (e.g. Base `flashblocks` vs
> `normal`, HyperEVM `systx` vs `standard`) **per request**, driven entirely by
> a tag/ID **selector** on the existing `use-upstream` directive. No bespoke
> `family` concept, no isolation config, no `defaultFamily`, no policy-primitive
> requirement. `family` is just a tag value the operator happens to pick.

## 0. Implementation status (what was actually built)

The design below explored a request-time **lag fence** (§5). During
implementation the scope was deliberately narrowed (operator decision): **the
selection policy and its lag exclusion are left exactly as-is** — lagging
upstreams are excluded today's way. Isolation is applied **only to the
`latest`/`finalized` (served-tip) decision**, which is where the cross-group
problem actually bites (a flashblocks-ahead group must not define `latest` for a
request pinned to the normal group). Built so far:

- **Phase 0 — tag-aware `use-upstream`** *(done)*: `common.MatchesSelector` /
  `UpstreamMatchesSelector` match the id **or** any tag (positive patterns);
  negation stays id-only. Wired into the three selection sites
  ([common/request.go](../../common/request.go) NextUpstream,
  [upstream/upstream.go](../../upstream/upstream.go) acceptsRequest,
  [erpc/networks.go](../../erpc/networks.go) shouldHandleMethod). Targeting +
  failover containment.
- **Phase 2′ — selector-scoped served tip** *(done)*: the request is bound to
  ctx at the top of `Forward`; `tipCandidateUpstreams` filters the served-tip
  input set by the request's selector, so both max-mode (`evmHighestBlockMax`)
  and cluster-mode (`clusteredServedTip`) resolve `latest`/`finalized` AMONG the
  selected subset (plus the future-block short-circuit and
  `guaranteedMethodFloor`). Selection-policy lag exclusion is **unchanged**.
- **Per-group monotonic shared state** *(done)*: a selector that **exactly
  matches a configured tag** materializes a lazy, cross-pod, strict-monotonic
  per-group counter (`servedLatestBlock/<net>/sel:<tag>`), so each group has its
  own monotonic tip — bounded to the operator's configured tags and capped
  (`maxServedTipPartitions`), so attacker-supplied selectors create **no** state
  (they fall back to a stateless cluster-min over the subset). See §6–§8.

Not built (explicitly out of scope per the operator decision): the §5 lag fence
/ any change to the background selection policy.

## 1. Goals & non-negotiable constraints

- **Generic.** Targeting and isolation work over *any* tag (or upstream id),
  not a hard-coded dimension. `use-upstream=family:systx`,
  `use-upstream=region:us-*`, `use-upstream=base-norm-1` are all first-class.
- **Dynamic.** When a request carries a selector, lag comparison, served-tip
  resolution, and failover all operate **among the matched subset**, computed
  on the fly. Nothing is declared ahead of time.
- **Cheap at scale.** Production is **3,000 upstreams × 400 networks in one
  pod**. The feature must add **zero overhead to networks/requests that don't
  use a selector**, allocate nothing per-request on the hot path beyond O(pool),
  and never grow unbounded state.
- **DDoS-safe.** A selector is **attacker-controlled input**. It must never
  become a key that allocates durable state. Only a **bounded, operator-defined
  universe** (the distinct tag values actually present on a network's upstreams)
  may key persistent state.

## 2. Core principle: stateless by default, stateful only for bounded tags

Every quantity is one of two kinds:

- **Pure** — a function of the *current* upstream set (candidate pool, the
  cluster-min tip *candidate*). Trivially recomputed per request; **never keyed,
  never stored**.
- **Stateful** — accumulates over time and/or is shared across pods (the
  served-tip **monotonic counter + velocity gate**, sticky-primary). Needs a
  **stable key** to accumulate against.

The selector is attacker-controlled, so it cannot key stateful things. The
**only** legal keys for stateful state are the **distinct tag values present on
a network's upstreams** — an operator-bounded universe. This single rule is what
makes the design both dynamic and safe.

| Selector kind | Pool filter | Lag fence (§5) | Served tip (§6) | Materializes durable state? |
|---|---|---|---|---|
| **none** (untargeted) | whole network | whole-network max — *today's behavior* | network-wide monotonic counter — *today's* | **no** |
| **exact tag** matching ≥1 upstream (`family:systx`) | matching upstreams | subset max | **per-tag-value** monotonic counter, lazily materialized (§7) | **yes — bounded by config** |
| **glob over tags/ids** (`family:sy*`, `base-*`) | matching upstreams | subset max | **stateless** cluster-min over the subset (best-effort, per-pod guard) | **no** |
| **exact/glob id** (`base-norm-1`) | matching upstreams | subset max | stateless cluster-min over the subset | **no** |
| **unmatched** (`family:ghost`) | empty | — | — | **no** (fast-fail `ErrNoUpstreams`) |

Only the **exact-tag** row creates durable state, and its cardinality is
strictly ≤ the number of distinct tags the operator configured. Everything else
is pure/stateless and safe under arbitrary input.

## 3. The selector — tag-aware `use-upstream`

`use-upstream` is broadened from "glob over upstream id" to "glob over the
upstream's **id ∪ tags**". A selector token matches an upstream if it
glob-matches the id **or** any tag string.

- Parsed already at [common/request.go:740-741](../../common/request.go) (header
  `X-ERPC-Use-Upstream`) and [:811-812](../../common/request.go) (query
  `use-upstream`); no new directive.
- A single matcher helper `selectorMatches(pattern, upstream) bool` replaces the
  three bare id matches:
  - [common/request.go:1443](../../common/request.go) (`NextUpstream` loop),
  - [upstream/upstream.go:1528](../../upstream/upstream.go) (`acceptsRequest`),
  - [erpc/networks.go:1853](../../erpc/networks.go) (`shouldHandleMethod` count).
- Matching is `WildcardMatch` over `{id} ∪ tags`; existing id-only configs are a
  strict subset of the new behavior (an id pattern still matches the id).
- **Classification** (drives §2's table), computed once per request:
  - *exact-tag* — the pattern contains no glob metachars **and** equals a tag
    string present on ≥1 matched upstream;
  - *glob/id* — anything else that matches ≥1 upstream;
  - *unmatched* — matches nothing → fast-fail.

There is nothing special about `family:`. The classifier keys off "is this an
exact match of a real tag," so `region:`, `tier:`, or any convention inherits
the identical capability.

## 4. Failover containment (free)

Because `use-upstream` already filters the pool in `NextUpstream`
([common/request.go:1435-1447](../../common/request.go)) and gates per-upstream
in `acceptsRequest`, retry / hedge / consensus inherit the selector boundary
with no extra work — they consume the same filtered pool. Broadening the matcher
to tags is the only change; containment is automatic.

## 5. Lag isolation — request-time fence (the central change)

**Today** block-head-lag exclusion is a *background* policy predicate
(`.excludeIf(any(blockNumberLagAbove(16), blockSecondsLagAbove(30)))`,
[internal/policy/default_policy.js:43](../../internal/policy/default_policy.js))
reading the *precomputed network-wide scalar* `BlockHeadLag`
([health/tracker.go:1339](../../health/tracker.go)). That decision is computed
once per `(network, method, finality)` tick, request-independent, so a
per-request selector cannot re-scope it — and the scalar is global, so it
penalizes a slower family for trailing a faster one.

**Change:** reinterpret the lag predicates as a **request-time fence over the
resolved candidate pool**, not a background exclude over the global metric.

- The operator still **declares** the fence in the policy
  (`blockNumberLagAbove(16)` / `blockSecondsLagAbove(30)`) — authorability is
  preserved.
- The engine **defers** these specific predicates out of the background eligible
  decision and **applies them on the request path** against
  `poolMax = max(EvmEffectiveLatestBlock() over the resolved pool)`, dropping any
  candidate more than the threshold behind `poolMax` (retaining the existing
  head-of-chain-race tolerance / `MaxRetryableBlockDistance`).
- Per-upstream heads are already tracked
  ([architecture/evm/evm_state_poller.go](../../architecture/evm/evm_state_poller.go),
  exposed via `EvmEffectiveLatestBlock()`), so the fence is **O(pool), stateless,
  zero-allocation, zero shared state**.

Consequences (intended):

- A genuinely lagging (broken) node is **fenced per request** instead of
  cordoned-and-probed. Recovery is automatic and immediate (once it catches up
  it passes the fence) — more responsive than tick-based cordon + `probeExcluded`
  re-admission for this fast-moving signal.
- All *other* background predicates stay in the tick unchanged — they are
  **per-upstream-intrinsic** (error rate, throttle, absolute latency) and need no
  peer baseline. (`latencyDeviationAbove` is comparative but per-method and not a
  correctness hazard across families; left in the tick for now — see §11.)
- The exported `BlockHeadLag` gauge and `sortByScore`'s lag weight stay
  network-wide. Within a selector-scoped (single-family) pool the lag term is a
  near-common offset, so intra-family ranking is unaffected; correctness comes
  from the fence, not the score.

This is the one change that makes "lag is evaluated *among* the selector" true
with **no config, no new shared state, and full operator control**.

## 6. Served-tip isolation

`EvmHighestLatestBlockNumber` / `EvmHighestFinalizedBlockNumber`
([erpc/networks.go:331-349](../../erpc/networks.go), [:482-493](../../erpc/networks.go))
resolve `latest`/`finalized` and gate block availability. They must reflect the
request's selector so a `systx`-targeted request never resolves `latest` to a
block only a faster family has.

- `ComputeServedTipCandidate` ([architecture/evm/served_tip.go:102-196](../../architecture/evm/served_tip.go))
  stays a **pure, unchanged** function. `gatherEvmTipInputsForMethod`
  ([erpc/networks.go:275-305](../../erpc/networks.go)) gains a selector filter on
  its inputs.
- **Untargeted** → existing network-wide monotonic counter
  (`servedLatestBlockShared`, [erpc/networks.go:59-62](../../erpc/networks.go)).
  Byte-identical to today.
- **Exact-tag selector** → a **per-tag-value** monotonic counter + velocity-gate
  anchor, lazily materialized (§7), preserving cross-pod strict monotonicity for
  that partition.
- **Glob/id selector** → **stateless**: cluster-min over the selector-filtered
  inputs, with an optional cheap **per-pod** monotonic guard (an in-memory
  `atomic.Int64` keyed by the partition, never shared, evictable). No durable
  state. Best-effort monotonicity is acceptable on this opt-in path.
- **Cache safety:** any `latest`/`finalized` value that feeds a cache key or
  block-range plan ([architecture/evm/eth_blockNumber.go:58](../../architecture/evm/eth_blockNumber.go),
  `eth_getBlockByNumber`, `eth_getLogs`, [json_rpc.go:65](../../architecture/evm/json_rpc.go))
  must incorporate the selector partition, or a faster family's tip could serve a
  slower-family-targeted request from cache. The partition id (tag value, or
  `""` for untargeted) is mixed into the relevant cache key.

`guaranteedMethodFloor` ([erpc/networks.go:495-542](../../erpc/networks.go))
computes within the resolved partition.

## 7. Lazy materialization (bounded, evictable)

Only **exact-tag** selectors materialize durable per-partition state.

- **Key:** the canonical tag value (e.g. `family:systx`), validated to be a tag
  present on ≥1 of the network's upstreams. The raw request string is **never** a
  key.
- **Lazy:** the first request that resolves to tag value `T` on network `N`
  creates `N`'s partition registry entry for `T` (its served-tip shared
  counter(s) + velocity anchor + sticky scope). Networks/tags never selected
  cost nothing.
- **Bounded:** materialized partitions per network ≤ distinct tags present on its
  upstreams, with a hard ceiling (default **16**, configurable). Beyond the
  ceiling, extra exact-tag selectors fall back to the stateless glob path.
- **Evictable:** the in-pod partition registry is an LRU/TTL map; a partition
  idle for `> partitionIdleTTL` (default **30m**) is evicted and its in-pod
  handles dropped. (The shared-store counter persists harmlessly and is
  re-attached on next use; or, if the backing store is in-memory, eviction
  reclaims it.) This bounds steady-state memory to *active* partitions, not the
  config maximum.
- **Background refresh:** the per-partition served tip is refreshed only for
  **live** (non-evicted) partitions, reusing the already-gathered poll inputs —
  it is a group-by over data the poller already has, O(upstreams-in-network).

## 8. DDoS / abuse analysis

| Vector | Mitigation |
|---|---|
| Flood of distinct **arbitrary** selectors (`use-upstream=<random>`) | Arbitrary/glob/id selectors take the **stateless** path — pool filter + O(pool) fence + stateless tip. They allocate **no durable state**, so the shared-state `sync.Map` ([data/shared_state_registry.go:30](../../data/shared_state_registry.go), which has **no eviction**) never grows. |
| Flood of distinct **tag-looking** selectors (`family:<random>`) | Only selectors that **exactly match a tag present on the network** materialize. Unmatched → empty pool → fast-fail, no state. Matched → bounded by config. |
| Flood of exact matches to **all real tags** | Cardinality ≤ distinct configured tags, capped at the per-network ceiling (16) and reclaimed by idle eviction (§7). Operator-bounded by construction. |
| CPU amplification via huge/complex selector patterns | Cap selector length and pattern count; `WildcardMatch` is linear; pool match is O(pool). |
| Targeting an empty partition to force errors | Returns `ErrNoUpstreams` fast; no retry storm (the failover loop sees an empty filtered pool and exits). |

**Invariant:** durable, shared, or unbounded state is keyed **only** by the
operator-controlled tag universe — never by request input.

## 9. Memory & CPU budget (3k upstreams / 400 networks / 1 pod)

- **Networks/requests with no selector:** **zero** added cost. Same single
  network served-tip counters, same background lag metric, same code path. The
  feature is inert until a selector is used.
- **Per-request hot path (any request):** the lag fence is O(pool) (~7 upstreams
  avg) over already-resident `int64` heads, no allocation. `selectorMatches`
  is O(tags) per candidate, only when a selector is present.
- **Per active exact-tag partition:** 1–2 shared `int64` counters + a small
  velocity anchor + a sticky entry. Bounded by config, capped at 16/network,
  evicted when idle. Realistic steady state (a handful of multi-family networks ×
  2 partitions) is **tens** of counters, not thousands.
- **No per-request shared-store writes added** beyond what served-tip resolution
  already does today (the partition just changes *which* counter is updated, not
  *how many*).
- **Background:** per-partition tip refresh runs only for live partitions and
  reuses gathered inputs — no new polling, no per-network scan for networks
  without active partitions.

Explicit non-goals for cost: no per-upstream map keyed by selector, no
per-request map allocation, no background work proportional to (networks ×
possible selectors).

## 10. Backward compatibility

- No new config, no new directive, no schema change.
- Existing `use-upstream=<id-glob>` configs behave identically (id still
  matches; tag matching is purely additive).
- Single-family / untagged networks: untargeted requests keep the exact current
  lag and served-tip behavior (whole-network baseline, network-wide monotonic
  counter). The only model change they see is lag exclusion moving from the
  background tick to a request-time fence over the *whole* pool — which for a
  single family is the same set of exclusions, just evaluated later.
- The default policy still declares `blockNumberLagAbove(16)` /
  `blockSecondsLagAbove(30)`; the engine reinterprets *where* they run (§5).

## 11. Open questions

- **`latencyDeviationAbove` scope.** It is peer-comparative (per method,
  [stdlib.js:1187-1356](../../internal/policy/stdlib/stdlib.js)). Within a
  selector-scoped pool it's already apples-to-apples on the targeted family; for
  untargeted multi-family it compares across families. Leave in the background
  tick (v1) or also defer to a request-time, pool-relative check? *Leaning: leave
  it; latency profiles differing by family is acceptable soft signal, unlike lag.*
- **Engine deferral mechanics.** Cleanly separating the lag predicates from the
  rest of the background decision (so they run at request-time with the pool
  baseline) is the highest-design-care item. Interim fallback if deferral proves
  invasive: a network-level `evm.maxBlockHeadLag` threshold applied by the
  Forward path (reintroduces one config knob — avoid unless necessary).
- **Sticky-primary per partition.** Exact-tag partitions get their own sticky
  scope (§7). Confirm the sticky key extension
  ([internal/policy/sticky.go:24-82](../../internal/policy/sticky.go)) is keyed by
  the bounded tag value, never the raw selector.
- **Glob served-tip guard.** Is the per-pod best-effort monotonic guard worth its
  small complexity, or is a pure stateless cluster-min (no guard) acceptable for
  glob/id selectors? *Leaning: pure stateless; revisit if churn observed.*
- **Untargeted multi-family.** Such requests keep cross-family behavior by
  design (no selector ⇒ no declared scope). Acceptable as opt-in, or warn at
  config load when a network has multiple tag-families but receives untargeted
  traffic? *Leaning: document; no runtime default.*
