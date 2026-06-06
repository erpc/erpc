# Selector-Scoped Isolation — Implementation Plan

Companion to [feature.md](./feature.md). Builds the dynamic, selector-driven
isolation model in dependency order. Each phase is independently shippable and
testable; the headline correctness fix lands in Phase 2.

> **Status (see feature.md §0):** Phase 0 (tag-aware `use-upstream`) and a
> narrowed Phase 2 — **selector-scoped served tip** with per-group monotonic
> shared-state counters (DDoS-bounded to configured tags) — are **implemented
> and tested**. The Phase 2 **lag fence** below (moving lag out of the
> background policy) was **NOT** built: the operator chose to leave the
> selection policy / lag exclusion exactly as today, and apply isolation only
> to the `latest`/`finalized` decision. Phases 1/2 here are retained for
> historical context; the shipped served-tip work corresponds to Phase 3 +
> Phase 4 (materialization) of this plan.

## Locked decisions

| Decision | Choice |
|----------|--------|
| Targeting mechanism | Broaden **`use-upstream`** to glob over **id ∪ tags** (no new directive) |
| Isolation trigger | Per-request selector; **no config**, no `defaultFamily`, `family` not special |
| Lag isolation | Move lag predicates from **background exclude** → **request-time fence over the resolved pool** |
| Durable state key | **Only** canonical tag values present on a network's upstreams (bounded); never the raw selector |
| Materialization | **Lazy**, **capped per network** (default 16), **idle-evictable** (default 30m TTL) |
| Empty/unmatched selector | **Hard-fail** `ErrNoUpstreams`, no state created |
| Cost rule | Zero overhead when no selector is used; O(pool) per-request, no per-request allocation |

## Glossary

- **Selector** — the `use-upstream` value; a glob matched against id ∪ tags.
- **Exact-tag selector** — a non-glob selector equal to a tag string present on
  ≥1 matched upstream. The *only* kind that materializes durable state.
- **Partition** — the set of upstreams sharing one canonical tag value; the unit
  of durable per-tag state.
- **Pool** — the selector-filtered, policy-eligible candidate set for a request.

---

## Phase 0 — Tag-aware selector (targeting + containment)

Pure win, no isolation semantics yet; unblocks everything.

### 0.1 `common/` — matcher
- Add `selectorMatches(pattern string, u Upstream) (bool, error)`: `WildcardMatch`
  against `u.Id()` OR any `u.Config().Tags` entry. Cap pattern length / count.
- Replace the three bare id matches:
  - [common/request.go:1442-1447](../../common/request.go) (`NextUpstream`),
  - [upstream/upstream.go:1527-1535](../../upstream/upstream.go) (`acceptsRequest`),
  - [erpc/networks.go:1851-1853](../../erpc/networks.go) (`shouldHandleMethod`).
- Add `req.SelectorKind()` / `req.SelectorPartitionKey()` helpers computed once:
  classify as exact-tag (→ partition key = the tag value), glob/id, or unmatched.

### Acceptance
- `use-upstream=family:systx` routes only to upstreams tagged `family:systx`;
  `use-upstream=base-norm-*` (id) still works; both contain failover.
- Classifier unit tests: exact-tag vs glob vs id vs unmatched; multi-tag
  upstreams; case/space normalization.
- Bench: `selectorMatches` allocation-free; no regression on the no-selector
  path (early return when directive empty).

---

## Phase 1 — Per-upstream head on the request path

Foundation for the lag fence; no behavior change yet.

### 1.1 Expose pool heads cheaply
- Confirm `EvmEffectiveLatestBlock()` is reachable for each candidate in the
  Forward pool without a poll (reads the cached atomic in
  [architecture/evm/evm_state_poller.go](../../architecture/evm/evm_state_poller.go)).
- Add a helper to compute `poolMax(pool)` = max effective latest over the pool,
  O(pool), no allocation.

### Acceptance
- Unit test: `poolMax` over a mixed pool; ignores syncing / zero-head upstreams
  (mirror `gatherEvmTipInputsForMethod` filters at
  [erpc/networks.go:283-298](../../erpc/networks.go)).

---

## Phase 2 — Lag fence (THE headline correctness fix)

### 2.1 Engine: defer lag predicates
- Separate `blockNumberLagAbove` / `blockSecondsLagAbove`
  ([internal/policy/stdlib/stdlib.js:1362-1372](../../internal/policy/stdlib/stdlib.js))
  from the background eligible decision so the rest of the policy
  (`removeCordoned`, error/throttle/latency excludes, `preferTag`, `sortByScore`,
  `stickyPrimary`, `probeExcluded`) still runs per tick, but lag is **not** a
  background exclude.
- Surface the declared lag thresholds to the request path (engine reads them off
  the compiled policy; default 16 blocks / 30s from
  [internal/policy/default_policy.js:43](../../internal/policy/default_policy.js)).

### 2.2 Forward: apply the fence
- In Forward, after the pool is built ([erpc/networks.go:757](../../erpc/networks.go))
  and selector-filtered (Phase 3), drop candidates with
  `poolMax − EvmEffectiveLatestBlock() > blockThreshold` (and the seconds
  variant), preserving the head-of-chain-race tolerance
  (`MaxRetryableBlockDistance`, see [erpc/networks.go:1005-1026](../../erpc/networks.go)).
- Keep computing the per-upstream `BlockHeadLag` metric/gauge for observability
  and `sortByScore` ([health/tracker.go:1339](../../health/tracker.go)) — only the
  *exclusion* moves.

### Acceptance (headline regression)
- Two families on one network; family A 50 blocks ahead. A request with
  `use-upstream=family:B` (or untargeted-but-single-family) does **not** fence
  out family-B nodes that are level with each other. This is the R1/R2 fix.
- A genuinely broken node 80 blocks behind its own family **is** fenced.
- Single-family characterization: identical exclusion set to today (fence over
  whole pool == old global exclude).
- Recovery: a fenced node that catches up is selectable on the very next request
  (no probe/re-admission delay).

---

## Phase 3 — Selector-scoped served tip

### 3.1 Pure path (glob / id / untargeted)
- `gatherEvmTipInputsForMethod` ([erpc/networks.go:275-305](../../erpc/networks.go))
  gains a selector filter.
- Untargeted → existing network counter (unchanged).
- Glob/id → stateless cluster-min over filtered inputs; optional per-pod
  `atomic.Int64` monotonic guard (in-memory, evictable, never shared).
- `ComputeServedTipCandidate` ([architecture/evm/served_tip.go:102-196](../../architecture/evm/served_tip.go))
  unchanged.

### 3.2 `EvmHighest*` resolve the partition
- `EvmHighestLatestBlockNumber` / `EvmHighestFinalizedBlockNumber`
  ([erpc/networks.go:331-349](../../erpc/networks.go), [:482-493](../../erpc/networks.go))
  read the request's `SelectorPartitionKey()` off ctx; default `""`.
- `guaranteedMethodFloor` ([erpc/networks.go:495-542](../../erpc/networks.go))
  computes within the partition.

### 3.3 Cache safety
- Mix the partition key (tag value, or `""`) into cache keys / block-range plans
  that consume `latest`/`finalized`
  ([architecture/evm/eth_blockNumber.go:58](../../architecture/evm/eth_blockNumber.go),
  `eth_getBlockByNumber`, `eth_getLogs`, [json_rpc.go:65](../../architecture/evm/json_rpc.go),
  [erpc/query_executor.go:321-328](../../erpc/query_executor.go)).

### Acceptance
- Targeted `family:systx` request resolves `latest` to systx's tip, never a
  faster family's block; no block-not-found churn.
- Untargeted single-family: identical served-tip values (golden).
- Cache test: a `family:A`-resolved `latest` does not serve a `family:B`-targeted
  request from cache.

---

## Phase 4 — Bounded, evictable materialization (exact-tag only)

### 4.1 Partition registry
- Per `Network`, an LRU/TTL map `partitionKey → *partitionState` where
  `partitionState` holds the per-tag served-tip shared counter(s) + velocity
  anchor + sticky scope. Replace the two scalar fields
  ([erpc/networks.go:59-62](../../erpc/networks.go)) with: the existing pair for
  the `""` partition + this lazy map for tag partitions.
- `GetOrCreatePartition(tagValue)`:
  - validate `tagValue` is a tag present on ≥1 network upstream (else no
    materialization → caller uses stateless path);
  - enforce per-network ceiling (default 16) → overflow uses stateless path;
  - lazily `GetCounterInt64("servedLatest/<net>/<tagValue>", …)`
    ([data/shared_state_registry.go:100](../../data/shared_state_registry.go)).
- TTL eviction (default 30m idle): drop in-pod handles; re-attach on next use.

### 4.2 Sticky per partition
- Extend the sticky key ([internal/policy/sticky.go:24-82](../../internal/policy/sticky.go),
  [stdlib.js:701-838](../../internal/policy/stdlib/stdlib.js)) with the partition
  key for exact-tag partitions so each family keeps its own stable primary.

### Acceptance
- Materialization count never exceeds distinct configured tags / the ceiling.
- DDoS test: 10k requests with random `use-upstream=<garbage>` and random
  `family:<garbage>` create **zero** new shared-state entries (assert
  `sync.Map` size stable).
- Eviction test: an idle partition is reclaimed after TTL; re-use re-materializes
  correctly and monotonically.
- Cross-pod monotonicity preserved per tag partition (shared-store test).

---

## Phase 5 — Observability

### 5.1 Metrics
- Optional `selector`/`partition` label on the served-tip and lag gauges
  ([telemetry/metrics.go](../../telemetry/metrics.go)); default `""`. Keep
  cardinality bounded (label only the materialized tag partitions).
- A gauge for live partition count per network (capacity/eviction visibility).

### 5.2 Docs
- `docs/pages/` operation page: `use-upstream` now matches tags; how dynamic
  isolation works; the DDoS/cost model; the lag-fence behavior change.

### Acceptance
- No-selector deployments emit unchanged series (label `""`).
- Partition-count gauge reflects materialization/eviction.

---

## Cross-cutting performance gates (must hold at 3k upstreams / 400 networks)

| Gate | Target |
|------|--------|
| No-selector request path | **No new allocation, no new branch cost** beyond a nil-directive check |
| Selector request path | O(pool) match + O(pool) fence, **zero heap allocation** in steady state |
| Durable state | Bounded by configured tags × ceiling; **idle-evicted**; DDoS test asserts `sync.Map` stability |
| Background work | Only for **live** partitions; reuses gathered poll inputs; no scan for inactive networks |
| Served-tip hot path | No increase in shared-store writes per request vs today |

Add benchmarks mirroring [health/tracker_bench_test.go](../../health/tracker.go)
and the policy eval benches for the selector + fence paths; gate CI on no
regression for the no-selector path.

## Risk register

| Risk | Mitigation |
|------|-----------|
| Lag-predicate deferral (Phase 2.1) is invasive in the engine | Ship behind the interim `evm.maxBlockHeadLag` Forward-path threshold if needed; keep the declared-in-policy target as the end state. |
| Lag no longer drives `probeExcluded` re-admission | Intended — the per-request fence is self-healing; verify no policy relies on lag-cordon for probing. |
| Shared-state `sync.Map` has no eviction | Never key it by raw selector; only bounded tag values; DDoS test as a guard. |
| Cache cross-contamination across partitions | Phase 3.3 partition-keyed cache; explicit test. |
| Per-pod glob monotonic guard adds complexity | Default to pure stateless cluster-min for glob/id; add the guard only if churn is observed. |

## Out of scope / deferred

- Making `latencyDeviationAbove` selector-relative (feature.md §11).
- Cross-family/verification fan-out (forbidden by the isolation contract).
- Per-partition export of every metric vector (cardinality; only lag/served-tip
  gauges get the optional label).
