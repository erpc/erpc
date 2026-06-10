# KB: Lanes and concurrency (served-tip partitions)

> status: complete
> source-dirs: common/lane.go, common/lane_test.go, erpc/networks.go (servedTipPartition, partitionKeyFor, LaneName usage), telemetry/metrics.go, common/matcher.go, common/request.go (UseUpstream directive), common/config.go (DirectivesConfig.UseUpstream)

## L1 — Capability (CTO view)

"Lanes" in eRPC are named groups of upstreams that a `use-upstream` selector targets. Each lane gets its own per-group served-tip counter (shared across pods via the shared-state registry), so a request routed to a specific subset of upstreams (e.g. "only flashblocks upstreams") gets a served-tip block number that reflects that subset rather than the broader network tip. This prevents `block not found` churn that would occur if a sub-group pinned to one provider class were told to serve a block number only visible to a different provider class. The `LaneName` function deterministically derives a short, human-readable label (e.g. `"flashblocks"`, `"systx"`) from the matched upstream set; that label becomes the `lane` dimension on Prometheus metrics.

## L2 — Mechanics (staff-engineer view)

**What is a lane?** A lane is not a standalone config object. It is an emergent concept: when a request carries a `use-upstream` selector (via `X-ERPC-Use-Upstream` header, `use-upstream` query param, or `DirectivesConfig.UseUpstream`), the selector names a subset of upstreams. If that subset is "simple" (single glob token, optionally negated, 2 ≤ match count < all), eRPC materializes a per-group served-tip partition for it. The partition's `lane` field is `LaneName(matchedIds)`.

**LaneName algorithm** (`common/lane.go:L25`):
1. Sort the upstream ids alphabetically to ensure order-independence.
2. Walk the tokens (hyphen-split) of the first sorted id. For each token, check whether it appears in every other id. Return the first such "universally shared" token.
3. If no token is shared by all ids, fall back to a concatenation of the first 2 characters of each id (up to 5 ids).
4. Empty input → `""`.

The algorithm is deterministic and order-independent (guaranteed by sort). Cross-pod consistency is guaranteed: every pod derives the same name from the same upstream topology.

**Partition materialization** (`erpc/networks.go:L412-L442`):
1. `servedTipPartitionFor(ctx, selector)` is called from `EvmHighestLatestBlockNumber` and `EvmLowestFinalizedBlockNumber` when served-tip cluster mode is enabled.
2. `partitionKeyFor` is called first to produce a stable key: if the selector is "simple", resolve the matched upstream ids, sort them, SHA-256 hash the `\x00`-joined list, take the first 8 bytes as hex → `"grp:<hex>"`. If the selector is not simple or the match count is outside `[2, all)`, return `""` (use stateless fallback).
3. If key is non-empty: load from `sync.Map` (fast path) or create new `servedTipPartition` with `LaneName(ids)` and two `CounterInt64SharedVariable` counters (cross-pod monotonic counters for latest and finalized blocks).
4. A per-network atomic count (`servedTipPartitionCount`) caps materialization at `maxServedTipPartitions = 16`.

**What qualifies as a "simple group selector"** (`erpc/networks.go:L493-L506`):
- Non-empty, ≤ 128 characters.
- No whitespace, no `|`, `&`, `(`, `)`, `,` — excludes boolean combinators.
- At most one `!`, only at position 0 — allows negation prefix but no embedded negation.
- Examples that qualify: `flashblocks*`, `!flashblocks*`, `family:systx`, `alchemy-*`.
- Examples that do NOT qualify: `alchemy | quicknode`, `(a | b)`, `a!b`.

**Stateless fallback.** When a selector is complex (boolean expression) or resolves to 0/1/all upstreams, `servedTipPartitionFor` returns nil. In that case `clusteredServedTip` is called with `lane=servedTipLaneNone` (`"\x00scoped"`), and `observeServedTipMetrics` emits NOTHING for that call — preventing a subset value from overwriting any gauge.

**Metric labels.** `observeServedTipMetrics` (`erpc/networks.go:L802`) uses `laneLabel`:
- `lane=""` (network-wide call, no selector) → `laneLabel = "all"` (`servedTipLaneAll`).
- `lane=servedTipLaneNone` → function returns early, emits nothing.
- `lane=<LaneName>` (named group) → `laneLabel = LaneName`, emits block-number and lag gauges for the group. Per-upstream exclusion counters (velocity/outlier) are only emitted for the network-wide (`lane=""`) pick.

**Concurrency properties:**
- `servedTipPartitions` is a `sync.Map`; `LoadOrStore` guarantees exactly one partition per key across concurrent goroutines.
- `servedTipPartitionCount` is an `atomic.Int32`; the `>= maxServedTipPartitions` check is a soft cap (the race is safe to lose — at worst one extra partition is created).
- Partition shared counters use `CounterInt64SharedVariable` which is a cross-pod strict-monotonic counter: values never decrease across time or pods.

**UseUpstream directive flow:**
- Header `X-ERPC-Use-Upstream: <selector>` or query `?use-upstream=<selector>` sets `request.directives.UseUpstream` (`common/request.go:L740-L741, L811-L812`).
- Config-level `networks[*].directivesDefaults.useUpstream` sets the default for all requests on that network (`common/request.go:L589-L590`).
- In `Network.Forward` → `tipCandidateUpstreams` → `requestSelector(ctx)` reads `d.UseUpstream` from the request bound to ctx (`erpc/networks.go:L396-L402`).
- The selector is also used by `UpstreamMatchesSelector` in the upstream selection loop to filter which upstreams to route to (`common/request.go:L1424-L1445`).

## L3 — Exhaustive reference (agent view)

### Config fields

Lanes are derived from runtime state, not directly configured. The related config field:

| # | YAML path | Type | Default | Behavior / notes | Source |
|---|-----------|------|---------|------------------|--------|
| 1 | `networks[*].directivesDefaults.useUpstream` | `*string` | `nil` (no default selector) | Default `use-upstream` selector applied to all requests on this network when the client does not send one; identical semantics to the header/query directive | `common/config.go:L2109`, `common/request.go:L589-L590` |

The served-tip lane behavior is also gated on `networks[*].evm.servedTip` configuration (see the served-tip KB for `enabledFor`, `velocityGate`, etc.) — lanes only materialize when `servedTip` cluster mode is enabled for the given axis.

**Total config fields documented: 1**

### Behaviors & algorithms

**`LaneName(ids []string) string`** (`common/lane.go:L25`):

```
Input:  []string of upstream ids
Output: short stable human-readable group name
```

Algorithm:
1. If `len(ids) == 0` → return `""`.
2. `sorted = sort(ids)`.
3. Build a token-set per id: `strings.Split(id, "-")` → set of non-empty tokens.
4. Iterate tokens of `sorted[0]` in order. For each token T: if T appears in EVERY other id's token set → return T immediately.
5. Fallback: for each id in `sorted` (up to 5): append `id[:2]` (or full id if len < 2). Return concatenated string.

Key properties:
- Order-independent (sort ensures it).
- Deterministic (same input always → same output).
- Prefers tokens that appear earliest in the first sorted id when multiple tokens are shared.
- Caps fallback at 5 ids to keep label cardinality bounded.

**`isSimpleGroupSelector(selector string) bool`** (`erpc/networks.go:L493-L506`):
- Trims whitespace; rejects empty or > 128 chars.
- Rejects if contains any of: ` \t\r\n()|&,`.
- Rejects if `!` appears at any position other than index 0, or appears more than once.
- Accepts: `flashblocks*`, `!flashblocks*`, `family:systx`, `alchemy`, `quicknode-*`.

**`partitionKeyFor(ctx, selector) (key string, ids []string)`** (`erpc/networks.go:L466-L486`):
- Returns `"", nil` if selector is not simple, or `networkUpstreams < 2`.
- Resolves matched ids via `UpstreamMatchesSelector`.
- Returns `"", nil` if `len(matched) < 2 || len(matched) >= len(all)`.
- Otherwise: `sha256(sort(matched).join("\x00"))[0:8]` → `"grp:<hex16>"`.

**`servedTipPartitionFor(ctx, selector)` → `*servedTipPartition` or nil** (`erpc/networks.go:L412-L442`):
- Calls `partitionKeyFor` → on `""` return nil immediately.
- Fast path: `sync.Map.Load(key)` → return existing.
- Slow path: check `servedTipPartitionCount >= 16` → return nil (stateless fallback).
- Create: `servedTipPartition{lane: LaneName(ids), latestShared: ssr.GetCounterInt64("servedLatestBlock/<net>/<key>", rollback), finalizedShared: ...}`.
- `LoadOrStore` race: one partition wins; count incremented only for the winner.

**`observeServedTipMetrics(axis, served, res, lane)`** (`erpc/networks.go:L802-L852`):
- `lane == servedTipLaneNone` → return immediately (no gauge written).
- `laneLabel = lane == "" ? "all" : lane`.
- Emit `MetricNetworkServedTipBlockNumber` and `MetricNetworkServedTipLagBlocks` for any `served > 0`.
- Emit per-upstream exclusion counters ONLY when `lane == ""` (network-wide pick).

**`MatchesSelector(pattern, id, tags) (bool, error)`** (`common/matcher.go:L73`):
- Always evaluates against `id` first (via `WildcardMatch`, which supports glob + boolean grammar).
- If id matches → true.
- If `tags` is empty OR pattern contains `!` → false (no tag matching for negated patterns).
- Otherwise: evaluate pattern against each tag; return true on first tag match.

**`UpstreamMatchesSelector(pattern, upstream) (bool, error)`** (`common/matcher.go:L106`): reads `upstream.Config().Tags` (nil-safe) and calls `MatchesSelector`.

**UseUpstream directive precedence** (highest to lowest):
1. Per-request `X-ERPC-Use-Upstream` header (`common/request.go:L740`).
2. Per-request `?use-upstream=<value>` query param (`common/request.go:L811`).
3. `networks[*].directivesDefaults.useUpstream` config default (`common/request.go:L589`).

### Observability

| Metric | Type | Labels | Notes | Source |
|--------|------|--------|-------|--------|
| `erpc_network_served_tip_block_number` | gauge | `project`, `network`, `lane`, `axis` | `lane="all"` = network-wide; `lane=<LaneName>` = per-group; absent when stateless fallback | `telemetry/metrics.go:L130-L134`, `erpc/networks.go:L821` |
| `erpc_network_served_tip_lag_blocks` | gauge | `project`, `network`, `lane`, `axis` | Blocks behind freshest velocity-eligible tip; ABSENT in MAX mode (feature off) | `telemetry/metrics.go:L145-L149`, `erpc/networks.go:L834` |
| `erpc_network_served_tip_upstream_excluded_total` | counter | `project`, `network`, `upstream`, `axis`, `reason` | Only emitted for `lane="all"` (network-wide pick); reason = `velocity` or `outlier` | `telemetry/metrics.go:L158-L162`, `erpc/networks.go:L844-L851` |

**Label values:**
- `lane="all"` = network-wide pick (no `use-upstream` selector or wildcard matching all).
- `lane=<LaneName>` = derived name for a use-upstream group (e.g. `"flashblocks"`, `"systx"`).
- The `lane` series for a named group is only present for networks that actually receive targeted `use-upstream` traffic — absent otherwise.
- `axis` = `"latest"` or `"finalized"`.

### Edge cases & gotchas

1. **A single upstream does not form a lane.** `partitionKeyFor` requires `len(matched) >= 2`. A `use-upstream: specific-node-id` selector targeting exactly one upstream gets the stateless fallback (`servedTipLaneNone`) and emits no gauge — the per-upstream poller already provides a monotonic block number for single-node targeting.

2. **Matching ALL upstreams does not form a lane.** `len(matched) >= len(all)` also uses the stateless fallback. This prevents a match-all selector from shadowing the `lane="all"` gauge.

3. **Boolean combinator selectors are never partitioned.** `alchemy | quicknode` is not a simple selector (contains `|`); it always uses the stateless fallback. Such selectors still route correctly via `UpstreamMatchesSelector` in the forwarding loop, but no per-group tip counter or gauge is maintained.

4. **The 16-partition cap is a soft guard.** Concurrent goroutines can transiently exceed 16 if the count is read just below the cap by multiple goroutines simultaneously. The actual maximum is `16 + concurrency` in the pathological case. Beyond the cap, new groups fall back to stateless rather than being tracked.

5. **Partition key = hash of matched upstream set, not of selector text.** `flashblocks*`, `fl*`, and a `family:flashblocks` tag that all match the same 3 upstreams produce the same key → one partition. This bounds cardinality to the actual upstream topology, not to the diversity of selector expressions clients might send.

6. **`LaneName` is input-order-independent but not unique.** Two different upstream sets could theoretically produce the same `LaneName` (e.g. both have `"systx"` as a shared token). The Prometheus `lane` label is decorative — the partition key is the stable identifier for state; the name is only for human readability on dashboards.

7. **Negated selectors match by id only, never by tag.** `MatchesSelector` with a pattern containing `!` evaluates tag matching as false regardless. A selector like `!alchemy-*` will exclude upstreams whose id matches `alchemy-*` but will NOT use tags to rescue non-matching ids.

8. **Stateless fallback emits no gauge at all.** `servedTipLaneNone` causes `observeServedTipMetrics` to return immediately without writing any gauge. This means a complex-selector request does not overwrite the `lane="all"` gauge with a potentially incorrect subset value.

9. **Lane names appear in Prometheus as-is.** If `LaneName` produces a name containing unusual characters (edge case: upstream ids with hyphens producing multi-char first tokens), that becomes the label value. The label is not sanitized further.

10. **Sort in `LaneName` makes `(quicknode-systx, systx-quicknode)` → `"quicknode"` not `"systx"`.** After sort, the sorted-first id is `quicknode-systx` (q < s alphabetically). Its first token is `quicknode`, and `quicknode` appears in `systx-quicknode` → result is `"quicknode"`. The `TestLaneName/"order independent"` test (`common/lane_test.go:L16`) encodes this: `want = "quicknode"`.

11. **`servedTip` must be enabled for lanes to have effect.** If `networks[*].evm.servedTip` is not configured (default = MAX mode), `servedTipEnabledFor` returns false, `EvmHighestLatestBlockNumber` calls the max path that never touches partitions, and no `lane`-labelled metrics are ever emitted (except `lane="all"` would not be emitted either in MAX mode since `observeServedTipMetrics` is only called from the cluster path).

### Source map

- `common/lane.go` — `LaneName(ids []string) string`: naming algorithm, pure function.
- `common/lane_test.go` — unit tests for `LaneName`: shared-token precedence, order-independence, fallback combo, single-id, empty input, determinism.
- `erpc/networks.go:L80-L105` — constants and `servedTipPartition` struct; `maxServedTipPartitions = 16`.
- `erpc/networks.go:L405-L442` — `servedTipPartitionFor`: lazy partition materialization with cap and sync.Map.
- `erpc/networks.go:L444-L506` — `partitionKeyFor`: selector → SHA-256 key derivation; `isSimpleGroupSelector` filter.
- `erpc/networks.go:L517-L540` — `EvmHighestLatestBlockNumber`: cluster-mode + per-lane branching.
- `erpc/networks.go:L802-L852` — `observeServedTipMetrics`: lane label assignment and gauge emission.
- `common/matcher.go:L73-L112` — `MatchesSelector` + `UpstreamMatchesSelector`: id+tag matching primitive.
- `common/request.go:L42,L68,L94,L739-L741,L811-L812` — `UseUpstream` directive header/query extraction.
- `common/config.go:L2109` — `DirectivesConfig.UseUpstream` field.
- `telemetry/metrics.go:L130-L162` — `MetricNetworkServedTipBlockNumber`, `MetricNetworkServedTipLagBlocks`, `MetricNetworkServedTipUpstreamExcludedTotal`.
