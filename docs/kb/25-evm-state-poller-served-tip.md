# KB: EVM state poller, block tracking, served tip

> status: complete
> source-dirs: architecture/evm/evm_state_poller.go, architecture/evm/served_tip.go, architecture/evm/block_ref.go, architecture/evm/block_range.go, architecture/evm/served_tip_test.go, erpc/networks.go, erpc/networks_served_tip_test.go, health/tracker.go, telemetry/metrics.go, common/config.go, common/defaults.go, common/validation.go

## L1 — Capability (CTO view)

The EVM state poller is a per-upstream background goroutine that tracks the chain head (latest block), finality (finalized block), node sync status, and optional earliest-available block bounds. Its outputs feed two downstream consumers: the **served tip** — the block number eRPC advertises as `"latest"` or `"finalized"` for a network — and the **block availability** gate that prevents routing requests to upstreams that cannot serve the requested block range. The served tip is either the legacy MAX across eligible non-syncing upstreams, or the cluster-minimum of the dominant agreement cluster among upstreams (the opt-in mode), ensuring that any upstream in the cluster can actually serve the advertised block and eliminating "block not found" churn caused by route-to-lagger races.

## L2 — Mechanics (staff-engineer view)

### Polling cadence

Each `EvmStatePoller` is bootstrapped by calling `Bootstrap(ctx)` (`evm_state_poller.go:L145`), which launches a long-lived goroutine running a `time.Ticker` at the `StatePollerInterval` period (default 30 s). On each tick, `Poll()` fires three concurrent goroutines: latest block, finalized block, and syncing state. A fourth goroutine handles earliest-block detection (only if `blockAvailability` bounds use `earliestBlockPlus`). `Bootstrap` also calls `Poll()` synchronously once before returning to guarantee the upstream is seeded before any request traffic arrives.

Within `PollLatestBlockNumber` and `PollFinalizedBlockNumber` the actual fetch is gated by `TryUpdateIfStale(ctx, dbi, fn)` on the shared counter — this coalesces concurrent callers across goroutines AND across pods (when a Redis-backed shared-state registry is used). The debounce interval `dbi` is resolved by `resolveDebounce()`:

1. If `upstream.evm.statePollerDebounce` is set, use it directly.
2. If the EMA-estimated network block time is known (`tracker.GetNetworkBlockTime`), multiply by `DynamicBlockTimeDebounceMultiplier` (default 0.7).
3. If `network.evm.fallbackStatePollerDebounce` is configured, use it.
4. Otherwise fall back to a hard 1 s floor.

The result is that on a 12 s Ethereum Mainnet chain the debounce is ~8.4 s (70% of 12 s), so multiple upstream pollers within the same tick interval share a single real fetch, and the interval is always shorter than one block to guarantee freshness.

### Dynamic block time EMA

The EMA lives in `health.Tracker.NetworkMetadata.evmBlockTimeEmaNs` (`health/tracker.go:L66-L71`). It is updated in `SetLatestBlockNumber` (`tracker.go:L1306`) whenever the network-level latest block advances and the call includes a non-zero `blockTimestamp` extracted from the `eth_getBlockByNumber` response. The EMA formula is:

```
EMA_new = alpha * sample_ns + (1 - alpha) * EMA_old
```

with `alpha = 0.1` (≈ 19-sample window, `tracker.go:L1385`). The sample is `(blockTimestampDelta_sec × 1e9) / blockGap`, normalizing for skipped blocks on fast chains (Arbitrum-style) where consecutive blocks share the same integer second. Samples are skipped when the timestamp has not advanced, accumulating `blockGap` until the timestamp ticks. The value is published to `evmBlockTime` (an `atomic.Int64`) only after at least `blockTimeMinSamples = 3` samples AND only when the raw EMA is in `[10ms, 120s]`; values outside that range reset the internal EMA to the last-published value to prevent runaway spikes after a chain halt.

Consumers call `GetNetworkBlockTime(networkId)` which returns the atomic value or 0 if not yet available (`tracker.go:L1463`). A 0 return value disables the velocity gate in the served-tip picker and causes the debounce to fall through to the fallback.

### Latest/finalized tracking per upstream

Both `latestBlockShared` and `finalizedBlockShared` are `data.CounterInt64SharedVariable` instances keyed by `"latestBlock/<upstreamKey>"` and `"finalizedBlock/<upstreamKey>"` respectively. They enforce:

- **Forward-only progress**: `TryUpdate` rejects new values that are ≤ the current value.
- **Large rollback detection**: if a new value would roll back more than `DefaultToleratedBlockHeadRollback = 1024` blocks, the `OnLargeRollback` callback fires and records the rollback in the Prometheus `upstream_block_head_large_rollback` gauge.
- **Cross-pod synchronization**: when a Redis-backed shared state is configured, the counter is shared across all eRPC replicas, so only one replica performs the actual RPC fetch per debounce window (`evm_state_poller.go:L97-L143`).

`SuggestLatestBlock(blockNumber)` is a fast-path, non-blocking path (used when a response's block number is observed during request forwarding) that calls `TryUpdate` directly without debounce coordination (`evm_state_poller.go:L459-L481`). `SuggestFinalizedBlock` is more conservative: it tries a mutex lock and, on success, performs the update in a goroutine with a 5 s timeout (`evm_state_poller.go:L560-L598`).

### Syncing state

`fetchSyncingState` calls `eth_syncing` with a 5 s timeout sub-context. It handles three result shapes:
- `false` → not syncing.
- `{currentBlock, ...}` or `{msgCount, ...}` (Arbitrum) → syncing.
- `{Ok: bool}` or `{ok: bool}` (non-standard) → `!Ok`.

The counter `synced` increments on each "not syncing" response and decrements (resets to 1) on "syncing"; the node is declared fully synced only when `synced >= FullySyncedThreshold = 4` consecutive not-syncing responses (`evm_state_poller.go:L21`, `evm_state_poller.go:L325`). After 10 consecutive failures (with no prior success), `skipSyncingCheck` is set to true and all future syncing polls are silently skipped.

### Failure thresholds for latest/finalized

Both `skipLatestBlockCheck` and `skipFinalizedCheck` follow the same pattern: 10 consecutive failures with no prior success sets the skip flag permanently. Once the upstream has had at least one success (`*SuccessfulOnce = true`), failures are only logged and never trigger the skip. This prevents false-positive disablement during transient degradations (`evm_state_poller.go:L420-L445`).

### Block refs for cache

`ExtractBlockReferenceFromRequest` (`block_ref.go:L18`) produces a `(blockRef, blockNumber)` pair used by the cache layer. The resolution order:
1. Request-level cache (already set on `NormalizedRequest`).
2. `extractRefFromJsonRpcRequest`: uses `MethodConfig.ReqRefs` to extract the block parameter from the JSON-RPC params.
3. If still incomplete, checks `r.LastValidResponse()` and calls `ExtractBlockReferenceFromResponse`.

Special cases for `blockRef`:
- Static methods (`Finalized = true`): returns `("*", 1)` — cached forever.
- Realtime methods (`Realtime = true`): returns `("*", 0)`.
- Methods with a single `["*"]` `ReqRefs`: returns `("*", _)` — cache-key-agnostic.
- Methods with multiple block parameters (e.g. `eth_getLogs` fromBlock/toBlock): returns `("*", max(blockNumbers))` to avoid cross-block cache collisions.
- Tag strings (`latest`, `earliest`, `pending`): become the `blockRef` directly.
- Numeric hex blocks: become `blockRef = strconv.FormatInt(blockNumber, 10)`.
- Block hashes: become `blockRef = "0x<hash>"`.

The `blockRef` from the request is never overwritten when already a non-`""` non-`"*"` value; the response may augment `blockNumber` but preserves the original request tag (`block_ref.go:L69-L85`).

### Block availability and block ranges

`CheckBlockRangeAvailability` (`block_range.go:L19`) validates that both `fromBlock` and `toBlock` are servable by an upstream. It first checks the upper bound (typically near head) with `forceFreshIfStale=true`, then the lower bound with `forceFreshIfStale=false`. Failure produces `ErrUpstreamBlockUnavailable` (retryable at the network level). This is used by `eth_getLogs`, `trace_filter`, and `arbtrace_filter`.

### Served tip (cluster mode)

When `network.evm.servedTip.enabledFor` includes `"latest"` or `"finalized"`, `EvmHighestLatestBlockNumber` / `EvmHighestFinalizedBlockNumber` on the `Network` struct delegate to `clusteredServedTip()` (`networks.go:L617`).

**Input gathering (`gatherEvmTipInputsForMethod`):** builds `[]ServedTipInput` from upstreams that are (a) policy-eligible for the method, (b) NOT in `EvmSyncingStateSyncing`, and (c) have a positive effective latest/finalized block. Cordoned or policy-excluded upstreams are automatically absent.

**Pure picker (`ComputeServedTipCandidate`):**
1. Drop inputs with `BlockNumber <= 0`.
2. **Velocity gate**: if `lastServedBlock > 0` AND `cfg.BlockTimeSeconds > 0`, compute `expectedMax = lastServedBlock + ceil(elapsedBlocks * VelocitySlack) + VelocityBufferBlocks`. Inputs exceeding `expectedMax` are dropped into `VelocityDropped`. Both conditions required — cold start or unknown block time disables the gate entirely.
3. Sort remaining inputs ascending.
4. **Greedy clustering**: split when gap between adjacent values > `ClusterDelta`.
5. **Dominant cluster**: max by (size descending, then min descending — ties go to the chain-forward cluster).
6. Return `MIN` of the dominant cluster as `Candidate`.

**Monotonic clamp**: the caller (`clusteredServedTip`) applies `shared.TryUpdate(ctx, res.Candidate)` so the network-wide served tip never regresses. The actual returned value is `shared.GetValue()` post-update (the clamped value, not the raw candidate).

**Guaranteed method floor**: if `GuaranteedMethods` is configured, `guaranteedMethodFloor()` runs the cluster picker for each method's supporting subset and returns the minimum candidate across all methods. The global tip is clamped down to this floor before the monotonic clamp is applied (`networks.go:L645-L647`).

**Selector-scoped served tip**: a `use-upstream` selector in the request context causes `EvmHighestLatestBlockNumber` to scope `tipCandidateUpstreams` to the matching upstreams only. For "simple group selectors" (single glob, optionally negated, ≥2 upstreams, < all upstreams), a **per-group `servedTipPartition`** is lazily materialized with its own cross-pod shared counter, so the group's monotonic tip is independent of the network-wide tip. Selectors that don't form a real group use a stateless cluster-min (no shared counter, no monotonic guarantee). Up to `maxServedTipPartitions = 16` partitions are allowed per network; beyond that, extra selectors use the stateless path.

### blockHeadLag / finalizationLag semantics

`BlockHeadLag` in `TrackedMetrics` is `max_network_latest - this_upstream_latest` (both from the tracker's metadata, not from the shared counter). It is recomputed by `SetLatestBlockNumber` every time any upstream in the network advances the network-level block. A value of 0 means the upstream is at or ahead of the network maximum; a higher value means it is that many blocks behind. The same logic applies to `FinalizationLag` for the finalized axis (`health/tracker.go:L1339-L1372`, `health/tracker.go:L1513-L1550`). These fields are used by selection-policy predicates such as `blockNumberLagAbove`.

## L3 — Exhaustive reference (agent view)

### Config fields

#### Upstream-level (`upstreams[*].evm.*`)

| YAML path | Type | Default | Behavior | Source |
|-----------|------|---------|----------|--------|
| `upstreams[*].evm.statePollerInterval` | Duration | `30s` | How often `Poll()` fires. **Setting to `0` disables the poller entirely**: `Bootstrap` returns immediately without launching the goroutine, without calling the initial synchronous `Poll()`, and without setting `Enabled = true`. Consequences: (a) the upstream has no block numbers (`LatestBlock()` returns 0, `FinalizedBlock()` returns 0); (b) syncing state stays `EvmSyncingStateUnknown` forever; (c) `skipWhenSyncing` never triggers exclusion (because `Unknown != Syncing`); (d) block availability checks fail-open (bound computed as `[MinInt64, MaxInt64]`); (e) `ErrFinalizedBlockUnavailable` is returned on any `IsBlockFinalized` call. This is an intentional opt-out for chains where polling is expensive or unnecessary. | `evm_state_poller.go:L147-L151`, `evm_state_poller.go:L160`, `upstream/upstream.go:L932-L936`, `upstream/upstream.go:L1511-L1514`, `common/defaults.go:L1718-L1722` |
| `upstreams[*].evm.statePollerDebounce` | Duration | `0` (not set; defer to dynamic/fallback) | Hard override for the per-poll debounce interval. When non-zero, the **entire** three-step resolution algorithm in `resolveDebounce` is short-circuited: the value is used directly as the `TryUpdateIfStale` staleness window, bypassing both the EMA-derived path and the `fallbackStatePollerDebounce`. Setting this on a per-upstream basis makes that upstream's poll cadence fully independent of the network's block-time estimate. | `evm_state_poller.go:L360-L363`, `common/config.go:L1127` |
| `upstreams[*].evm.blockAvailability.lower.updateRate` | Duration | `0` (freeze after first detection) | Recompute cadence for `earliestBlockPlus`-based lower bounds. `0` means the binary-search result from the first successful `Poll()` is never refreshed for this process lifetime. Values > 0 launch a per-instance scheduler goroutine that calls `PollEarliestBlockNumber` at this interval. **Ignored entirely when the bound uses `latestBlockMinus` or `exactBlock`** — those are computed on-demand from the live latest-block value without any scheduler. The scheduler goroutine is started once and runs until process shutdown (tied to `appCtx`); it is NOT restarted on config hot-reload. When multiple bounds for the same probe have different `updateRate` values, the **minimum non-zero rate** across both Lower and Upper bounds for that probe is used. Cross-pod coordination: `TryUpdateIfStale` ensures only one pod per `updateRate` window executes the binary search. | `common/config.go:L1209-L1210`, `evm_state_poller.go:L694-L700`, `evm_state_poller.go:L745-L765`, `common/validation.go:L976-L978` |
| `upstreams[*].evm.blockAvailability.upper.updateRate` | Duration | `0` (freeze after first detection) | Same semantics as `lower.updateRate`. Ignored when bound is `latestBlockMinus` or `exactBlock`. The minimum non-zero rate across Lower and Upper for the same probe is used to schedule the goroutine. | `common/config.go:L1209-L1210`, `evm_state_poller.go:L694-L700` |

#### Network-level (`networks[*].evm.*`)

| YAML path | Type | Default | Behavior | Source |
|-----------|------|---------|----------|--------|
| `networks[*].evm.fallbackStatePollerDebounce` | Duration | `5s` | Debounce fallback when block time EMA is not yet available. Used by `resolveDebounce()` step 3. | `common/defaults.go:L1975-L1976`, `evm_state_poller.go:L373-L377` |
| `networks[*].evm.fallbackFinalityDepth` | int64 | `1024` | Blocks subtracted from latest to infer a finalized block when the upstream does not support `eth_getBlockByNumber("finalized")`. Applied only when `finalizedBlock == 0`. | `common/defaults.go:L1975`, `evm_state_poller.go:L856-L869` |
| `networks[*].evm.dynamicBlockTimeDebounceMultiplier` | *float64 | `0.7` | Scaling factor applied to the EMA block time to compute the polling debounce. Lower values prefer freshness (more RPC calls); higher values prefer call savings. **Range**: any positive float64 is accepted; the code reads `> 0` as the guard (`evm_state_poller.go:L369`), so `0` is treated as "not set" (falls back to the default `0.7`). Negative values are similarly treated as unset. **Multiplier = 0 is effectively ignored**: explicitly setting `dynamicBlockTimeDebounceMultiplier: 0` leaves `mult = DefaultDynamicBlockTimeDebounceMultiplier = 0.7` because the guard condition `*cfg.DynamicBlockTimeDebounceMultiplier > 0` is false. There is no way to force the debounce to zero via this field; use `statePollerDebounce` on the upstream instead. **Interplay with `statePollerDebounce`**: this field is never evaluated when any upstream in the network has `statePollerDebounce > 0` configured — that per-upstream override short-circuits `resolveDebounce` at step 1, before the multiplier is ever read. **Useful range**: `0.1`–`1.0`. Values below `0.1` produce a debounce shorter than one-tenth of a block time, defeating its purpose and causing frequent repeated RPC calls. Values above `1.0` make the debounce longer than one block time, causing stale block data between `Poll()` cycles on high-traffic paths. Values above `statePollerInterval / blockTime` make the debounce longer than the polling interval, which means the interval governs freshness instead. | `common/defaults.go:L1977`, `evm_state_poller.go:L368-L372` |
| `networks[*].evm.servedTip` | *EvmServedTipConfig | `nil` (disabled; max mode) | Controls the served-tip algorithm for this network. When nil, `EvmHighestLatestBlockNumber` returns MAX across eligible upstreams. | `common/config.go:L2188`, `erpc/networks.go:L521` |
| `networks[*].evm.servedTip.enabledFor` | []string | `[]` (max mode for all tags) | Tags for which cluster-min mode is active. Valid values: `"latest"`, `"finalized"`, `"safe"` (alias for `"finalized"`). | `common/config.go:L2272`, `common/config.go:L2292-L2303` |
| `networks[*].evm.servedTip.clusterDelta` | int64 | `0` (auto-derive) | Max block gap between adjacent sorted upstream tips that still groups them into one cluster. **Auto-derive formula when `clusterDelta = 0`**: `delta = clamp(ceil(2.0 / blockTimeSec), 2, 10)`. The 2.0 s numerator models vendor-to-vendor block propagation slack typical on EVM networks. The clamp enforces a floor of 2 (for slow chains like Ethereum Mainnet at 12 s → `ceil(2/12) = 1`, clamped to 2) and a ceiling of 10 (for fast chains: any `blockTimeSec ≤ 0.2 s` → `ceil(2/0.2) = 10`, and anything faster caps at 10). **When block time is unknown** (`blockTimeSec = 0`, i.e. EMA not yet warmed up), the auto-derive returns 2 as a conservative fallback (`served_tip.go:L235-L237`). **Examples**: Ethereum (12 s) → 2; Polygon (2 s) → 1, clamped to 2; Arbitrum (0.25 s) → 8; Arbitrum-style sub-100ms hypothetical → 10. Setting an explicit positive value overrides auto-derive entirely for the lifetime of the process. | `architecture/evm/served_tip.go:L123-L125`, `architecture/evm/served_tip.go:L225-L246` |
| `networks[*].evm.servedTip.guaranteedMethods` | []string | `[]` | Glob patterns (e.g. `"trace_*"`). For each pattern, the served tip is clamped down to the cluster-min of supporting upstreams only, ensuring `"latest"` is always servable for these methods. | `common/config.go:L2285`, `erpc/networks.go:L705-L747` |

### Behaviors & algorithms

#### Debounce resolution (`resolveDebounce`)

Priority chain (first non-zero value wins):
1. `upstream.evm.statePollerDebounce` — explicit per-upstream override. When set, stops here; no further steps evaluated. (`evm_state_poller.go:L361-L363`)
2. `tracker.GetNetworkBlockTime(networkId) × dynamicBlockTimeDebounceMultiplier` — EMA-derived; only active after ≥3 EMA samples AND the returned value is non-zero. The exact formula: `debounce = float64(blockTimeNs) × mult` where `blockTimeNs` is the EMA value in nanoseconds (a `time.Duration`) and `mult` is the configured or default multiplier. Default `mult = 0.7` (`DefaultDynamicBlockTimeDebounceMultiplier`). Example: 12 s Ethereum Mainnet → EMA ≈ 12 s → debounce ≈ 8.4 s. (`evm_state_poller.go:L364-L372`)
3. `network.evm.fallbackStatePollerDebounce` — static network default (default `5s`). Used when EMA returns 0 (not yet warmed up) and no per-upstream override is set. (`evm_state_poller.go:L374-L376`)
4. Hard `1s` floor — final fallback when neither EMA nor fallback is available. (`evm_state_poller.go:L377`)

**Interaction between upstream-level and network-level:** `dynamicBlockTimeDebounceMultiplier` is a **network-level** field that scales the **network-level** EMA block time; it is used by the `resolveDebounce` function regardless of which upstream calls it (all upstreams on the same network see the same EMA and the same multiplier). The per-upstream `statePollerDebounce` field bypasses this computation entirely — it is the only upstream-level influence on the debounce. When `statePollerDebounce > 0` on any upstream, `dynamicBlockTimeDebounceMultiplier` is never read for that upstream.

**`dynamicBlockTimeDebounceMultiplier = 0` is treated as not set.** The guard condition is `*cfg.DynamicBlockTimeDebounceMultiplier > 0` (`evm_state_poller.go:L369`). Setting it to `0` keeps `mult = 0.7`. Negative values similarly fall through to the default. There is no valid way to set the multiplier to zero or negative through config; use `statePollerDebounce` to impose a fixed debounce instead.

**Useful multiplier range:** `0.1`–`1.0`. Values < `0.1` make the debounce shorter than 10% of a block, causing many redundant RPC calls within each block window. Values > `1.0` make the debounce longer than one block time, potentially causing stale data between polls on high-traffic request paths. Values between `1.0` and `statePollerInterval/blockTime` are theoretically safe but the polling interval then dominates freshness.

**1 s floor edge case:** The `1s` floor applies only when BOTH step 2 and step 3 yield zero. In practice, if `fallbackStatePollerDebounce` is set (default `5s`), the floor is never reached. The floor is only reached if someone explicitly sets `fallbackStatePollerDebounce: 0` at the network level while the EMA has not warmed up.

Source: `evm_state_poller.go:L355-L378`.

#### Dynamic block time EMA algorithm

- **Alpha**: 0.1 (`tracker.go:L1385`).
- **Effective window**: ~19 samples.
- **Warm-up**: EMA is not published until `evmBlockTimeSamples >= 3` (`blockTimeMinSamples`; requires 4 total observations due to the first sample being stored as "previous" without incrementing the counter) (`tracker.go:L1386`, `tracker.go:L1436-L1439`).
- **Fast-chain handling**: if `blockTimestampDelta <= 0` (consecutive blocks share the same integer second), the sample is skipped and `prevBlock`/`prevTimestamp` are NOT advanced. The blockGap accumulates until the timestamp ticks, then `sampleNs = timestampDeltaSec * 1e9 / blockGap` recovers sub-second precision.
- **Sanity bounds**: values outside `[10ms, 120s]` are rejected; the internal `evmBlockTimeEmaNs` is reset to the last valid published value to avoid delayed spikes after chain halts (`tracker.go:L1447-L1452`).
- **Only advances on network-level update**: the EMA is only fed when `blockNumber > ntwMeta.evmLatestBlockNumber` — i.e., when this upstream is reporting the new network maximum. Per-upstream block number advances never feed the EMA directly.

#### Served tip clustering algorithm (`ComputeServedTipCandidate`)

Full algorithm with defaults:

```
1. Filter: discard inputs where BlockNumber <= 0
2. VelocityGate (only if lastServedBlock > 0 AND cfg.BlockTimeSeconds > 0):
     elapsedBlocks = elapsedSinceLast.Seconds() / cfg.BlockTimeSeconds
     expectedMax = lastServedBlock + ceil(elapsedBlocks * VelocitySlack) + VelocityBufferBlocks
     discard any input.BlockNumber > expectedMax → VelocityDropped
3. Sort remaining ascending by BlockNumber
   MaxEligible = max of remaining (after velocity gate)
4. Greedy cluster: start cluster[0] = [inputs[0]]
   for i in 1..N:
     if inputs[i].BlockNumber - cluster[-1][-1].BlockNumber > clusterDelta:
       start new cluster
     else: append to current cluster
5. Pick dominant cluster: argmax by (len(cluster) DESC, cluster[0].BlockNumber DESC)
6. Candidate = dominant[0].BlockNumber  (MIN of dominant cluster)
```

Defaults when ServedTipConfig fields are 0:
- `VelocitySlack` → 2.0
- `VelocityBufferBlocks` → 5
- `ClusterDelta` → `clamp(ceil(2.0 / blockTimeSec), 2, 10)`; if `blockTimeSec == 0` → 2

Source: `architecture/evm/served_tip.go:L114-L223`.

#### `IsBlockFinalized(blockNumber int64) (bool, error)`

Decision tree:
1. If both `finalizedBlock == 0` and `latestBlock == 0` → return `(false, ErrFinalizedBlockUnavailable)`.
2. If `finalizedBlock > 0` → return `blockNumber <= finalizedBlock`.
3. If `finalizedBlock == 0` and `latestBlock > 0` → infer `finalizedBlock = latestBlock - FallbackFinalityDepth` (default 1024) and compare.

Source: `evm_state_poller.go:L828-L878`.

#### `ErrFinalizedBlockUnavailable` — error semantics

- **Error code:** `ErrCodeFinalizedBlockUnavailable` (`"ErrFinalizedBlockUnavailable"`) — `common/errors.go:L700`.
- **Trigger condition:** BOTH `finalizedBlockShared.GetValue() == 0` AND `latestBlockShared.GetValue() == 0` at the time of the `IsBlockFinalized` call. This happens during cold start before the first successful poll, or after `skipLatestBlockCheck` and `skipFinalizedCheck` have both been set (i.e., the upstream never responded successfully to either poll type). (`evm_state_poller.go:L829-L837`)
- **Error message:** `"finalized/latest blocks are not available yet when checking block finality"`. Details include `blockNumber` (the block being checked). (`common/errors.go:L702-L711`)
- **Retryability:** `ErrFinalizedBlockUnavailable` does NOT carry an explicit `retryableTowardNetwork: false` flag in its `Details` map; it also has no `GetCause()` chain. Per `IsRetryableTowardNetwork` logic at `common/errors.go:L2379-L2436`, all errors without an explicit opt-out flag default to retryable. Therefore: **retryable toward upstream (U:yes)** and **retryable toward network (N:yes)** — a request will be retried on a different upstream.
- **Wire HTTP status:** `ErrFinalizedBlockUnavailable` does NOT implement `ErrorStatusCode()`; it has no `ErrorStatusCode` method. Per `determineResponseStatusCode` in the HTTP server, errors without a status code override are mapped to HTTP 200 (for POST / JSON-RPC). Wire clients see a JSON-RPC error body with code `-32603` (internal error, via `TranslateToJsonRpcException`), not an HTTP error status.
- **JSON-RPC error code on wire:** `-32603` (mapped by `TranslateToJsonRpcException` for unrecognized error types). (`common/json_rpc.go:L1501+`)

#### Syncing state thresholds

| State | Condition |
|-------|-----------|
| `EvmSyncingStateNotSyncing` | `synced counter >= 4` consecutive not-syncing responses |
| `EvmSyncingStateSyncing` | Any syncing response resets counter to 1 |
| `EvmSyncingStateUnknown` | counter in [1, 3], or eth_syncing returned error, or skipSyncingCheck=true |

After 10 consecutive failures with no prior success: `skipSyncingCheck = true`, state fixed at `Unknown`. Source: `evm_state_poller.go:L280-L336`.

#### Earliest block detection (binary search)

Used only when `upstream.evm.blockAvailability.lower.earliestBlockPlus` or `upper.earliestBlockPlus` is set. `binarySearchEarliest()` finds the first block in `[0, latestBlock]` where the probe method returns data. Fast paths check blocks 0 and 1 first. The probe type controls which RPC method is called:
- `blockHeader`: `eth_getBlockByNumber(<hex>, false)` — non-empty result = available.
- `eventLogs`: `eth_getLogs({blockHash})` — must return ≥1 log.
- `callState`: `eth_getBalance("0x000...", <hex>)` — non-null result = available.
- `traceData`: tries `trace_block`, then `debug_traceBlockByHash`, then `trace_replayBlockTransactions` in order; first non-empty result = available.

Results are stored in `earliestByProbe` `CounterInt64SharedVariable` (keyed `"earliestBlock/<upstreamKey>/<probe>"`), coordinated cross-pod via `TryUpdateIfStale`. Initial detection happens once per probe per instance; periodic re-detection is started as a goroutine if `blockAvailability.*.updateRate` is set.

**`updateRate` goroutine lifecycle:**

- `initializeEarliestBlockDetectionAndStartScheduler` is called from `Poll()` on every tick but guards against repeat work with two in-memory flags per probe: `earliestInitialDetectionDone[probe]` and `earliestSchedulerStarted[probe]`. Both flags live in-process; they are NOT persisted to shared state. (`evm_state_poller.go:L663-L801`)
- On the **first** `Poll()` call after `Bootstrap`, the function computes the minimum non-zero `updateRate` across all bounds that use `earliestBlockPlus` for this probe, spawns a goroutine via `go e.runPeriodicEarliestBlockBoundUpdateLoop(probe, rate)`, and flips `earliestSchedulerStarted[probe] = true`. The scheduler goroutine runs until `e.appCtx` is cancelled (i.e., process shutdown). (`evm_state_poller.go:L762-L798`)
- The scheduler goroutine uses a `time.NewTicker(rate)` and calls `PollEarliestBlockNumber(e.appCtx, probe, rate)` on each tick. `PollEarliestBlockNumber` in turn calls `TryUpdateIfStale(ctx, staleness=rate, fn)` on the shared counter, so only one pod per `rate` window actually executes the binary search. (`evm_state_poller.go:L806-L826`)
- **`updateRate` is ignored for `latestBlockMinus` and `exactBlock` bounds.** The `consider()` helper only calls `UpdateRate` accumulation when `b.EarliestBlockPlus != nil` (`evm_state_poller.go:L682`). `latestBlockMinus` bounds compute their value on-demand by reading the continuously-updated latest block from `evmStatePoller.LatestBlock()` in `upstream.go:L1161-L1168` — no scheduler, no binary search, no shared state. Setting `updateRate` alongside `latestBlockMinus` logs a validation warning: `"upstream.*.evm.blockAvailability.*.updateRate is ignored when latestBlockMinus is set"` (`common/validation.go:L976-L978`).
- **Goroutine is NOT restarted on config hot-reload.** `earliestSchedulerStarted[probe]` is an in-process flag. A live config reload that changes `updateRate` has no effect; the existing goroutine continues at the original rate. Only a full process restart picks up the new value. (`evm_state_poller.go:L745-L765`)
- **Cross-pod sharing behavior.** Each eRPC replica independently runs initial detection and, if configured, starts its own periodic goroutine. However, both paths use `TryUpdateIfStale` with the `earliestByProbe[probe]` shared counter, so only one pod per `updateRate` window actually runs the binary search. Other pods receive the cached value from the shared counter. This is the same coordination mechanism used by `PollLatestBlockNumber`. The earliest block counter is **forward-only** (monotonically non-decreasing), so if an upstream is pruned to a higher earliest block after restart, the new value propagates. If restored to a lower earliest block (e.g., from backup), the stale higher value in shared state blocks the update until the staleness window expires.
- **Interaction with bootstrap synchronous poll:** `Bootstrap` calls `Poll()` synchronously once before returning. `initializeEarliestBlockDetectionAndStartScheduler` is called within `Poll()`; it requires `latestBlockShared.GetValue() > 0`. If the synchronous initial `PollLatestBlockNumber` succeeds (which it does in-process, bypassing cross-pod debounce via `staleness=1ms`), earliest detection proceeds immediately during bootstrap. If `latestBlock` is still 0 after the synchronous poll (e.g., upstream is unreachable), earliest detection is deferred to the next `Poll()` cycle with a warning. (`evm_state_poller.go:L709-L731`)

Source: `evm_state_poller.go:L604-L826`, `evm_state_poller.go:L1111-L1441`.

#### Selector-scoped served tip and partitions

`isSimpleGroupSelector(s)`: rejects selectors with whitespace, `()|&,`, or `!` not at position 0. Length ≤128. Single `!` only at the start is allowed (negation). Source: `erpc/networks.go:L493-L506`.

`partitionKeyFor`: resolves the matched upstream ID set, sorts it, SHA-256 hashes the sorted `\x00`-joined IDs, truncates to 8 bytes, encodes as hex. Key = `"grp:" + hex`. Partition only materialized if 2 ≤ matched < all. Equivalent selectors (same matched set) get the same key and dedup to one partition. Source: `erpc/networks.go:L466-L486`.

Maximum partitions: 16 (`maxServedTipPartitions`). Beyond the cap, extra selectors fall back to the stateless path (compute cluster-min but apply no monotonic clamp and emit no metrics). Source: `erpc/networks.go:L80-L84`.

### Observability

#### Prometheus metrics

| Metric | Labels | Description | When emitted | Source |
|--------|--------|-------------|--------------|--------|
| `erpc_upstream_latest_block_polled_total` | `project, vendor, network, upstream` | Counter incremented each time `PollLatestBlockNumber` actually calls `fetchBlock` (i.e., the debounce window has elapsed). Incremented BEFORE the fetch attempt, so a failed fetch still counts. | Two distinct call paths (see below) | `evm_state_poller.go:L406-L411` |
| `erpc_upstream_finalized_block_polled_total` | `project, vendor, network, upstream` | Counter for actual `eth_getBlockByNumber("finalized")` calls; same debounce and dual-path semantics as the latest counter. | Two distinct call paths (see below) | `evm_state_poller.go:L508-L513` |
| `erpc_upstream_latest_block_number` | `project, vendor, network, upstream` | Gauge: latest block number per upstream; `vendor="*"` / `upstream="*"` = network-wide max | Updated in `tracker.SetLatestBlockNumber` | `health/tracker.go:L1328-L1330` |
| `erpc_upstream_finalized_block_number` | `project, vendor, network, upstream` | Gauge: finalized block number per upstream | Updated in `tracker.SetFinalizedBlockNumber` | `health/tracker.go:L1504-L1509` |
| `erpc_upstream_block_head_lag` | `project, vendor, network, upstream` | Gauge: `max_network_latest - upstream_latest` (blocks behind) | Recomputed on each `SetLatestBlockNumber` call | `health/tracker.go:L1340-L1341`, `telemetry/metrics.go:L49-L53` |
| `erpc_upstream_finalization_lag` | `project, vendor, network, upstream` | Gauge: `max_network_finalized - upstream_finalized` | Recomputed on each `SetFinalizedBlockNumber` call | `telemetry/metrics.go:L55-L59` |
| `erpc_upstream_block_head_large_rollback` | `project, vendor, network, upstream` | Gauge: rollback amount when a new value would regress > 1024 blocks | Fired by `OnLargeRollback` callback on shared counter | `evm_state_poller.go:L135-L141` |
| `erpc_network_dynamic_block_time_milliseconds` | `project, network` | Gauge: current EMA block time estimate in ms | Updated when EMA crosses the warm-up threshold and is within sanity bounds | `health/tracker.go:L1456-L1458` |
| `erpc_network_latest_block_timestamp_distance_seconds` | `project, network, origin` | Gauge: `(now_ms - block.timestamp * 1000) / 1000.0` seconds; `origin="evm_state_poller"` | Updated when the network-level latest block advances and has a timestamp | `health/tracker.go:L1313-L1320` |
| `erpc_network_served_tip_block_number` | `project, network, lane, axis` | Gauge: post-clamp served tip block number. `lane="all"` = network-wide; named lane = use-upstream group. `axis="latest"` or `"finalized"` | Only in cluster mode; ABSENT in max mode | `erpc/networks.go:L821-L827`, `telemetry/metrics.go:L130-L134` |
| `erpc_network_served_tip_lag_blocks` | `project, network, lane, axis` | Gauge: `MaxEligible - served` (deliberate lag after velocity gate; excludes garbage future tips). **ABSENT in default max mode.** The gauge reflects the POST-CLAMP served value (not the raw picker candidate), so it accurately shows how far behind clients are even when the monotonic clamp is holding a stale tip (see edge case §15 below). During a **monotonic clamp hold-down** (all upstreams have regressed but the clamp holds the old high-water mark), `served` stays constant while `MaxEligible` follows the cluster — so the lag gauge can grow unboundedly until new blocks arrive. A persistently growing lag with a flat `served_tip_block_number` is the diagnostic signature of a fleet regression clamped by the monotonic counter. | Only in cluster mode; ABSENT in max mode | `erpc/networks.go:L829-L836`, `telemetry/metrics.go:L145-L149` |
| `erpc_network_served_tip_upstream_excluded_total` | `project, network, upstream, axis, reason` | Counter: upstream excluded from pick. `reason="velocity"` (fantasy future) or `"outlier"` (survived velocity gate but not in dominant cluster). Only on network-wide pick, not per-lane. | Only in cluster mode; incremented per exclusion event | `erpc/networks.go:L843-L851`, `telemetry/metrics.go:L158-L162` |

#### Poll trigger distinctions for `erpc_upstream_latest_block_polled_total` / `erpc_upstream_finalized_block_polled_total`

Both counters are incremented inside the `TryUpdateIfStale` callback — i.e., only when the debounce has elapsed and an actual RPC fetch is about to be issued. There are **two independent call paths** that can trigger a real fetch, and both increment the same counter:

1. **Proactive timer-driven poll** (`Poll()` → `PollLatestBlockNumber` / `PollFinalizedBlockNumber`): fires once per `statePollerInterval` tick on the background goroutine. This is the normal steady-state path — one increment per upstream per tick when the debounce window has elapsed. (`evm_state_poller.go:L162-L204`)

2. **Forced fresh poll triggered by block-availability check** (`EvmAssertBlockAvailability` / `EvmIsBlockFinalized` with `forceFreshIfStale=true`): fires when a request arrives for a block number above the current known latest (or when finality is not yet confirmed for a requested block) and the availability check forces a fresh fetch. Specifically: in `EvmAssertBlockAvailability`, line `upstream/upstream.go:L1063-L1068` calls `statePoller.PollLatestBlockNumber(ctx)` when `blockNumber > latestBlock && forceFreshIfStale`. Similarly, `EvmIsBlockFinalized` at `upstream/upstream.go:L921-L926` calls `PollFinalizedBlockNumber(ctx)` when the block is not yet finalized and `forceFreshIfStale=true`. This forced path bypasses the ticker but still respects the debounce window — if another pod fetched within the debounce window, `TryUpdateIfStale` returns the cached value without incrementing the counter.

**Operational implication:** during block-edge traffic spikes (many requests arrive just after a new block, all asking for the new block number), the forced path can produce a **burst** of debounce-bypassed fetches if the debounce is very short. With default debounce (EMA-derived ≈ 70% of block time), this is normally limited to one forced fetch per debounce window across all concurrent callers. However, in Redis-backed shared-state deployments, multiple pods can each independently trigger a forced fetch within the same debounce window if their local `TryUpdateIfStale` calls race before the shared counter propagates.

#### Trace spans (OTel)

| Span name | Where | Attributes |
|-----------|-------|------------|
| `EvmStatePoller.PollLatestBlockNumber` | Per polling cycle | `upstream.id`, `network.id` |
| `EvmStatePoller.PollFinalizedBlockNumber` | Per polling cycle | `upstream.id`, `network.id` |
| `Network.EvmHighestLatestBlockNumber` | Per call | `served_tip.*` attributes under detailed tracing |
| `Network.EvmHighestFinalizedBlockNumber` | Per call | Same |
| `Evm.ExtractBlockReferenceFromRequest` | Per request | `block.ref`, `block.number` (detailed only) |
| `Evm.ExtractBlockReferenceFromResponse` | Per response | `block.ref`, `block.number`, `block.timestamp` (detailed only) |

Detailed span attributes on the network span (under `common.IsTracingDetailed`): `served_tip.axis`, `served_tip.candidate`, `served_tip.max_observed`, `served_tip.cluster_count`, `served_tip.dominant_size`, `served_tip.outliers_count`, `served_tip.velocity_dropped`, `served_tip.lag_vs_max`. Source: `erpc/networks.go:L777-L793`.

#### Notable log messages

| Level | Message | Trigger |
|-------|---------|---------|
| `INFO` | `"bootstrapped evm state poller to track upstream latest/finalized blocks and syncing states"` | Successful `Bootstrap` |
| `INFO` | `"node is marked as fully synced"` | `synced counter >= 4` |
| `WARN` | `"upstream does not support fetching syncing state ... after N consecutive failures, will give up"` | `skipSyncingCheck` flip |
| `WARN` | `"upstream does not support fetching latest block number ... after N consecutive failures, will give up"` | `skipLatestBlockCheck` flip |
| `WARN` | `"upstream does not support fetching finalized block number ... after N consecutive failures, will give up"` | `skipFinalizedCheck` flip |
| `WARN` | `"failed to poll evm state"` | `Poll()` returns a non-cancel error |
| `INFO` | `"initial earliest block detection completed for this instance"` | Binary search completes successfully |
| `INFO` | `"started periodic scheduler for earliest block availability bound"` | `updateRate` configured and scheduler goroutine launched |

### Edge cases & gotchas

1. **Interval = 0 disables the poller entirely — full consequences.** If `statePollerInterval == 0`, `Bootstrap` returns immediately without launching the ticker goroutine, without calling the initial synchronous `Poll()`, and without setting `Enabled = true`. Full downstream effects: (a) `LatestBlock()` returns 0 permanently; (b) `FinalizedBlock()` returns 0 permanently; (c) `SyncingState()` returns `EvmSyncingStateUnknown`; (d) `skipWhenSyncing` in the routing policy never fires exclusion (because `Unknown != Syncing`); (e) `IsBlockFinalized` immediately returns `ErrFinalizedBlockUnavailable` because both block counters are 0; (f) block availability checks (`CheckBlockRangeAvailability`) fail-open (bounds computed as `[MinInt64, MaxInt64]`, all requests pass); (g) the upstream is not excluded from served-tip gathering — it contributes `0` block number which `ComputeServedTipCandidate` drops at step 1 (filter `BlockNumber <= 0`), so it doesn't corrupt the cluster but also doesn't contribute. This is an intentional opt-out for chains where polling is expensive or unnecessary (e.g., archive nodes where the operator manages routing externally). Source: `evm_state_poller.go:L147-L160`, `upstream/upstream.go:L932-L936`, `upstream/upstream.go:L1511-L1514`, `architecture/evm/served_tip.go:L114-L120`.

2. **`SuggestLatestBlock` is monotonic within a process but NOT across restarts.** It calls `TryUpdate` which the shared counter rejects if the value is ≤ current. However, across pod restarts the shared counter (Redis) holds the last value, so an upstream restart reporting a lower block number after a reorg is silently ignored for up to `DefaultToleratedBlockHeadRollback = 1024` blocks of rollback. Source: `evm_state_poller.go:L108-L109`, `data/counter_int64.go` (shared counter behavior).

3. **`SuggestFinalizedBlock` is async and may be dropped under load.** It uses `TryLock`; if an update is already in progress the new suggestion is discarded. Under high request volume, finalized block updates may lag. Source: `evm_state_poller.go:L560-L597`.

4. **The `OnValue` callback for `latestBlockShared` passes timestamp=0 when triggered by cross-pod replication.** Only the replica that actually fetches the block emits the `SetLatestBlockNumber` call with a real `blockTimestamp`; replicas receiving the shared-counter value via the `OnValue` callback always use timestamp=0. This ensures only one pod per network feeds the EMA, preventing duplicate samples. Source: `evm_state_poller.go:L126-L133`.

5. **Finalized block is NOT fetched by default on unsupported chains.** After 10 consecutive `eth_getBlockByNumber("finalized")` failures without a single success, `skipFinalizedCheck = true`. `IsBlockFinalized` then falls back to `latestBlock - FallbackFinalityDepth`. On PoW chains or chains without finality, this is the only path. Source: `evm_state_poller.go:L526-L533`.

6. **Velocity gate requires BOTH a prior anchor AND a known block time.** If either `lastServedBlock == 0` (cold start) or `cfg.BlockTimeSeconds == 0` (EMA not yet available), the velocity gate is inactive and all candidates pass to clustering. A misbehaving upstream reporting a far-future block during cold start will land in its own cluster; if it is the only upstream it becomes the sole candidate and wins the tiebreak. Source: `architecture/evm/served_tip.go:L153`, `erpc/networks_served_tip_test.go:TestServedTip_VelocityGate_NoLastServed_DisablesGate`.

7. **Monotonic clamp in cluster mode can hold the tip above the current cluster minimum after fleet degradation.** If all upstreams temporarily retreat (e.g., due to a partial reorg), the per-upstream poller is itself monotonic (`CounterInt64SharedVariable`) so `SuggestLatestBlock(lowerValue)` is ignored; the per-upstream `GetValue()` stays at the previous high-water mark. The served tip therefore also stays at its previous value. Source: `erpc/networks_served_tip_test.go:TestServedTip_MonotonicClamp_HoldsLastServedOnRegression`.

8. **Selector-scoped tip is stateless by default; only "simple group selectors" get monotonic tracking.** A `use-upstream=(fast*|slow*)` boolean expression is rejected by `isSimpleGroupSelector` and always uses the stateless (no-monotonic-clamp) path. Source: `erpc/networks.go:L493-L506`, `erpc/networks_served_tip_test.go:TestServedTip_SelectorScoped`.

9. **ClusterDelta=10 is the maximum for sub-100ms chains.** `resolveAutoClusterDelta(0.05)` gives `ceil(2/0.05)=40`, clamped to 10. A 10-block gap is therefore always within the cluster on such chains. Source: `architecture/evm/served_tip.go:L234-L246`, `architecture/evm/served_tip_test.go:TestComputeServedTipCandidate_ClusterDelta_AutoMax10`.

10. **`eth_syncing` result `{Ok: false}` is interpreted as SYNCING.** The non-standard `{Ok: bool}` response form is handled by `!Ok`: `Ok=false` means not-ok = still syncing. Source: `evm_state_poller.go:L1087-L1100`.

11. **`eventLogs` probe requires ≥1 log entry.** A block with no events returns an empty array, which is treated as "NOT available" even if the block header itself is present. This can cause the binary search to incorrectly mark data-sparse blocks as unavailable. Source: `evm_state_poller.go:L1266-L1274`.

12. **Earliest block detection is cross-pod via shared state, but the scheduler is per-instance.** Each pod does its own initial binary search (gated by `earliestInitialDetectionDone`) and if `updateRate` is configured starts its own periodic goroutine — but `PollEarliestBlockNumber` uses `TryUpdateIfStale`, so only one pod per debounce period actually does the search. Other pods return the cached shared value. Source: `evm_state_poller.go:L607-L651`.

13. **`MaxEligible` (not `MaxObserved`) is the lag reference.** A far-future tip from a misbehaving upstream is velocity-dropped, so it does not inflate `MaxEligible`. The served-tip lag gauge (`erpc_network_served_tip_lag_blocks`) uses `MaxEligible - served`, not `MaxObserved - served`. Source: `architecture/evm/served_tip_test.go:TestComputeServedTipCandidate_MaxEligible_ExcludesVelocityDroppedGarbage`.

14. **The `"safe"` tag is an alias for `"finalized"` in `ServedTipEnabledFor`.** Listing `"safe"` in `enabledFor` enables cluster mode for the finalized axis. Source: `common/config.go:L2300-L2302`.

15. **`erpc_network_served_tip_lag_blocks` can grow unboundedly during a monotonic clamp hold-down after fleet regression.** When a partial reorg or mass node reboot causes all upstreams to report a lower block number, the per-upstream `CounterInt64SharedVariable` monotonic counters reject lower values, so `MaxEligible` from the cluster picker reflects the clamped (stale-high) upstream values. However if the regression is so severe that ALL upstreams return 0 or block numbers below the clamp, `res.Candidate = 0` and the cluster returns no new inputs — `served = shared.GetValue()` stays at the old value. `MaxEligible = 0` in this case, making `lag = MaxEligible - served < 0`, which is clamped to 0. The metric therefore goes to 0 during a complete outage (no eligible inputs) but rises steadily during a partial regression where some upstreams have clamped values above the current chain tip but below the old served value. **Alert calibration implication:** a lag spike followed by a flat served-tip is diagnostic of fleet regression; a lag of 0 with a flat served-tip is diagnostic of total outage. Source: `erpc/networks.go:L630-L671`, `erpc/networks.go:L825-L836`.

16. **`erpc_network_served_tip_lag_blocks` and `erpc_network_served_tip_block_number` are ABSENT in default MAX mode.** Both gauges are only emitted by `observeServedTipMetrics`, which is only called by `clusteredServedTip`. The `evmHighestBlockMax` code path (used when `servedTip` config is nil or `enabledFor` is empty) does not call `observeServedTipMetrics`. Therefore, in deployments that have NOT opted in to served-tip cluster mode, these series will never appear in Prometheus, alerting rules that reference them will silently produce no data, and dashboards will show empty graphs. Source: `erpc/networks.go:L521-L527` (max-mode short-circuit), `erpc/networks.go:L671` (cluster path calls `observeServedTipMetrics`).

17. **`ErrFinalizedBlockUnavailable` never fires during cold start if only `skipLatestBlockCheck` is set (not both).** The error requires BOTH counters to be 0. If the upstream supports `eth_getBlockByNumber("latest")` but not `"finalized"`, `finalizedBlock = 0` but `latestBlock > 0`. `IsBlockFinalized` then falls back to the `latestBlock - FallbackFinalityDepth` path instead of returning the error. The error is only reachable when the upstream is completely unpolled (brand new, or both polls skipped after 10 consecutive failures). Source: `evm_state_poller.go:L829-L837`.

18. **`updateRate` goroutine is NOT restarted on upstream config reload.** The `earliestSchedulerStarted[probe]` flag is in-process memory. If a live upstream config reload changes `blockAvailability.*.updateRate`, the running goroutine continues at the old rate, and no new goroutine is started (the flag is already `true`). Only a process restart picks up the new `updateRate`. Source: `evm_state_poller.go:L745-L765` (flag checked before starting).

19. **`dynamicBlockTimeDebounceMultiplier: 0` is silently ignored; the default 0.7 is used.** The guard is `*cfg.DynamicBlockTimeDebounceMultiplier > 0` (`evm_state_poller.go:L369`). Setting the field to zero (or any non-positive value) causes the code to keep `mult = DefaultDynamicBlockTimeDebounceMultiplier = 0.7`. There is no way to reduce the EMA-derived debounce to zero via this multiplier; to eliminate debounce entirely use `statePollerDebounce: 1ms` (or any tiny value) on the upstream instead.

20. **`updateRate` alongside `latestBlockMinus` produces a validation warning, not an error.** Config validation calls `log.Warn()` (not `return error`) when `b.LatestBlockMinus != nil && b.UpdateRate > 0` (`common/validation.go:L976-L978`). The config is accepted and runs normally; the `updateRate` value is never read during operation for `latestBlockMinus` bounds. This means misconfigured YAML silently degrades to the correct behavior, but the operator may not notice the warning if they are not watching logs.

21. **`clusterDelta` auto-derive uses the EMA block time, not a configured block time.** `buildServedTipConfig` in `networks.go` calls `tracker.GetNetworkBlockTime(networkId)` to get `blockTimeSec` for `resolveAutoClusterDelta`. If the EMA has not yet warmed up (< 3 samples), `blockTimeSec` is 0 and `clusterDelta` defaults to 2. During this warm-up period, a two-block gap between an early and late upstream will erroneously split them into separate clusters. This is transient and resolves once the EMA has ≥3 samples (typically within the first 3 blocks produced after eRPC starts). Source: `architecture/evm/served_tip.go:L234-L246`, `erpc/networks.go` (`buildServedTipConfig` call).

### Source map

- `architecture/evm/evm_state_poller.go` — `EvmStatePoller` struct, `Bootstrap`/`Poll`/`PollLatestBlockNumber`/`PollFinalizedBlockNumber`/`PollEarliestBlockNumber`, syncing state machine, binary search, all probe implementations, `resolveDebounce`, `IsBlockFinalized`, `SuggestLatestBlock`/`SuggestFinalizedBlock`.
- `architecture/evm/served_tip.go` — Pure `ComputeServedTipCandidate` function, `ServedTipInput`/`ServedTipConfig`/`ServedTipResult` types, `resolveAutoClusterDelta`.
- `architecture/evm/block_ref.go` — `ExtractBlockReferenceFromRequest`, `ExtractBlockReferenceFromResponse`, `ExtractBlockTimestampFromResponse`, `extractRefFromJsonRpcRequest`, `extractRefFromJsonRpcResponse`, `parseCompositeBlockParam`.
- `architecture/evm/block_range.go` — `CheckBlockRangeAvailability` for range methods.
- `architecture/evm/served_tip_test.go` — Unit tests for `ComputeServedTipCandidate`, all clustering and velocity-gate scenarios.
- `erpc/networks.go` — `Network.EvmHighestLatestBlockNumber`, `EvmHighestFinalizedBlockNumber`, `clusteredServedTip`, `gatherEvmTipInputsForMethod`, `tipCandidateUpstreams`, `servedTipPartitionFor`, `partitionKeyFor`, `isSimpleGroupSelector`, `guaranteedMethodFloor`, `buildServedTipConfig`, `observeServedTipResult`, `observeServedTipMetrics`, `evmHighestBlockMax`, `tryShortCircuitFutureBlock`, `servedTipPartition` type.
- `erpc/networks_served_tip_test.go` — Integration tests for network-level served tip: cluster-min, monotonic clamp, selector scoping, partition dedup, guaranteed method floor, max mode.
- `health/tracker.go` — `SetLatestBlockNumber`, `SetFinalizedBlockNumber`, `updateBlockTimeSample`, `GetNetworkBlockTime`, `RecordBlockHeadLargeRollback`; `NetworkMetadata` with EMA fields; `TrackedMetrics.BlockHeadLag`/`FinalizationLag` atomics.
- `telemetry/metrics.go` — Prometheus metric declarations for all block-tracking and served-tip metrics.
- `common/config.go` — `EvmUpstreamConfig.StatePollerInterval`/`StatePollerDebounce`, `EvmNetworkConfig.FallbackStatePollerDebounce`/`FallbackFinalityDepth`/`DynamicBlockTimeDebounceMultiplier`/`ServedTip`, `EvmServedTipConfig`, `ServedTipEnabledFor`.
- `common/defaults.go` — `DefaultEvmFinalityDepth=1024`, `DefaultEvmStatePollerDebounce=5s`, `DefaultDynamicBlockTimeDebounceMultiplier=0.7`, `DefaultToleratedBlockHeadRollback=1024` (in `evm_state_poller.go:L27`), `EvmNetworkConfig.SetDefaults`.
