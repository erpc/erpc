# Multicall3 Batching (Network-Level)

Status: Implemented

## Context
The current Multicall3 batching lives in the HTTP batch handler. It only applies
to JSON-RPC batch requests and is tightly coupled to the HTTP layer, which is
not ideal for an EVM-only feature. The goal is to move batching to a deeper
layer so it can batch any incoming `eth_call` (batch or single), while keeping
network-level behaviors (cache, failover, circuit breakers).

## Goals
- Batch `eth_call` across all entrypoints (HTTP batch + single requests + gRPC).
- Preserve network-level behaviors (cache, failover, upstream selection).
- Preserve per-request rate limits and per-request metrics.
- Maintain per-call cache writes and reuse existing Multicall3 encode/decode.
- Keep batching opt-in and configurable per network.

## Non-Goals
- Batching non-EVM methods.
- Supporting `eth_call` fields beyond `to` + `data|input`.
- Enforcing upstream-specific caps in v1 (network-level caps only).

## Placement
Preferred: a pre-cache hook in `Network.Forward` (or in `PreparedProject.Forward`
right before `p.doForward`). This keeps batching entrypoint-agnostic while still
using `Network.Forward` for caching and failover.

Avoid: `PreUpstream` batching, which is too late (upstream-selected) and makes
cross-request aggregation difficult.

## Eligibility and Block Reference Normalization
A request is eligible if:
- Method is `eth_call`.
- Call object contains only `to` and `data|input`.
- Request is not already a multicall (recursion guard).
- Target contract is not in the `bypassContracts` list.
- Calls with any of `from`, `gas`, `gasPrice`, `maxFeePerGas`,
  `maxPriorityFeePerGas`, `value`, or a state override (third param) are
  ineligible.

## Bypass Contracts

Some contracts check `msg.sender` using `extcodesize()` and revert if the caller
has code (i.e., is a contract). When using Multicall3, `msg.sender` becomes the
Multicall3 contract address, causing these calls to fail.

Use `bypassContracts` to exclude specific contracts from batching:

```yaml
evm:
  multicall3Aggregation:
    enabled: true
    bypassContracts:
      # Chronicle Oracle feeds check msg.sender code size
      - "0x057f30e63A69175C69A4Af5656b8C9EE647De3D0"
      # Other contracts that revert on contract callers
      - "0xABCDEF0123456789ABCDEF0123456789ABCDEF01"
```

Addresses are case-insensitive and can be specified with or without the `0x`
prefix. Calls to bypassed contracts are forwarded individually.

## Auto-Detect Bypass

Enable `autoDetectBypass` to automatically detect contracts that revert when
called via Multicall3 but succeed when called individually:

```yaml
evm:
  multicall3Aggregation:
    enabled: true
    autoDetectBypass: true
```

When a call reverts within a Multicall3 batch and `autoDetectBypass` is enabled:
1. The call is retried individually (bypassing Multicall3).
2. If the individual call succeeds, the contract is added to a runtime bypass
   cache and future calls skip batching.
3. If the individual call also fails, the original error is returned (no bypass
   is added).

This allows automatic discovery of contracts that check `msg.sender` code size
(e.g., Chronicle Oracle) without requiring manual configuration.

Metrics:
- `erpc_multicall3_runtime_bypass_total{project, network}`: Contracts added to
  runtime bypass.
- `erpc_multicall3_auto_detect_retry_total{project, network, outcome}`: Retry
  tracking. Outcome values: `attempt` (retry initiated), `detected` (bypass
  discovered - individual call succeeded), `same_error` (individual call also
  failed, not a bypass candidate).

Note: The runtime bypass cache is in-memory and resets on restart. For known
contracts, use `bypassContracts` for persistent configuration.

Block reference normalization:
- Use `NormalizeBlockParam` on the `eth_call` block parameter.
- `nil` becomes `latest`.
- `0x` block numbers are normalized to decimal strings.
- Block hash (`0x` + 32 bytes) stays hex.
- Object params use `blockHash`, `blockNumber`, or `blockTag`.
- Known tags (`latest`, `finalized`, `safe`, `earliest`, `pending`) are
  lower-cased for keying to avoid duplicates.

Batching with tags:
- Default: allow `latest`, `finalized`, `safe`, `earliest`.
- `pending` is disabled by default (configurable).
- We do not resolve `latest` to a specific block number; all calls share the
  same tag, and the execution block is the one seen by the upstream at call time.

## Batching Key (User Isolation + Directive Key)
Key fields:
- `projectId`
- `networkId`
- `blockRef`
- `directivesKey`
- `userId` if `allowCrossUserBatching` is false

Directives key uses a stable, versioned subset:
- `UseUpstream`
- `SkipInterpolation`
- `RetryEmpty`
- `RetryPending`
- `SkipCacheRead` (optional; can be per-request, but include for clarity)

Any new directive must be explicitly added to the subset or ignored. The
directives key version is defined in code (not config) to avoid cross-node
mismatches; a version bump should be part of a release note.

## Deduplication (Within Batch)
To avoid TOCTOU cache misses and duplicated calls:
- Maintain a `callKey` map inside each batch.
- `callKey` uses the same derivation as the cache key (method + params).
- Directives are already part of the batch key, so differing directives never
  share a batch or a `callKey`.
- Multiple identical requests share one multicall slot and fan out results
  to all waiters.

## Batching Window and Deadlines
Configurable timing:
- `windowMs`: max wait time for a batch.
- `minWaitMs`: minimum wait to allow other requests to join.
- `safetyMarginMs`: subtracted from the earliest request deadline.
- `onlyIfPending`: no extra latency unless a batch is already open.

Deadline-aware flush rules:
- If `deadline <= now + minWaitMs`, bypass batching and forward individually.
- Otherwise, `flushTime = min(flushTime, deadline - safetyMarginMs)`.
- Clamp `flushTime` to at least `now + minWaitMs`.
- If `flushTime <= now`, flush immediately.

Concurrent flush behavior:
- If a batch for a key is already flushing, a new request for the same key
  starts a new batch (next window) rather than joining the in-flight batch.

## Caps and Backpressure
Network-level caps:
- `maxCalls`
- `maxCalldataBytes`
- `maxQueueSize` (global or per-key)
- `maxPendingBatches`

Behavior on overflow:
- Prefer bypassing batching (forward individually) and increment a metric.
- Avoid unbounded memory growth.
- `maxQueueSize` is the total enqueued requests across all batches; `maxPendingBatches`
  is the number of distinct batch keys. If either limit would be exceeded, bypass
  batching for that request.

## Forwarding, Fallback, and Partial Failures
1. Acquire per-request project + network rate limits.
2. Build Multicall3 request, mark as composite
   (e.g., `CompositeTypeMulticall3`) and set `skipNetworkRateLimit` context
   (via `withSkipNetworkRateLimit`).
3. Forward via `Network.Forward`. Fallback forwarding also uses
   `skipNetworkRateLimit` to avoid double-counting.

Fallback criteria:
- Multicall request error with `ShouldFallbackMulticall3` (unsupported endpoint,
  missing contract, known provider patterns).
- Invalid or unusable response (RPC error, invalid hex, ABI decode failure,
  length mismatch).
Pattern matching is implementation-defined; see `architecture/evm/multicall3.go`.

Partial failures:
- If Multicall succeeds but an inner call returns `success=false`, return a
  per-call `execution reverted` error. Do not fallback.
- If Multicall succeeds and results count matches, map directly, no fallback.

If fallback fails, propagate a single infrastructure error to all requests.

## Error Propagation Semantics
Per-call revert:
- Return a standard `execution reverted` error with `data` field.

Infrastructure failure:
- All requests receive the same error.
- Include metadata such as `{ "multicall3": true, "stage": "decode", "reason": "..." }`
  to distinguish infra failures from per-call failures.

## Cancellation Handling
- If a request is canceled before flush, remove it from the batch and return
  a context error for that request only.
- Cancellation does not affect other requests in the batch.
- Rate limit permits are not released (standard behavior). This keeps rate
  limiting conservative and avoids races; a future enhancement could reclaim
  permits for requests canceled before a flush.

## Upstream Capability Handling
Some upstreams may not support Multicall3 or have stricter limits.
v1 decision: rely on `ShouldFallbackMulticall3` to detect unsupported upstreams
and fallback to individual calls. A future v2 can add explicit upstream
capability flags or filtering to avoid first-failure latency.

## Cache Behavior
Pre-batch cache lookup:
- If `SkipCacheRead` is set, bypass per-request cache lookup.
- Cached responses are returned immediately and removed from the batch.
- For deduped requests, a single cached response satisfies all waiters.

Per-call cache writes:
- On multicall success and cache eligible, write each call as if it were a
  standalone `eth_call`.
- Cache key is identical to a standalone request (method + params), which
  matches `callKey` derivation for dedupe.

## Observability (Metrics + Tracing)
Metrics:
- `multicall3_aggregation_total{outcome}`
- `multicall3_fallback_total{reason}`
- `multicall3_cache_hits_total`
- Optional: `multicall3_batch_size`, `multicall3_batch_wait_ms`,
  `multicall3_queue_len`, `multicall3_queue_overflow_total`

Aggregation outcome values:
- `success`
- `all_cached`
- `fallback`
- `error`
- `bypassed` (ineligible, deadline-too-tight, or backpressure)

Tracing:
- Add spans for `Batcher.Enqueue`, `Batcher.Wait`, `Batcher.Flush`.
- Link request spans to the multicall span; if batch size is small, also link
  the multicall span back to request spans. Always attach a shared `batch.id`
  attribute to both sides for correlation.

## Config Proposal
Extend `EvmNetworkConfig`:

```yaml
evm:
  multicall3Aggregation:
    enabled: true
    windowMs: 25
    minWaitMs: 2
    safetyMarginMs: 2
    onlyIfPending: false
    maxCalls: 20
    maxCalldataBytes: 64000
    maxQueueSize: 1000
    maxPendingBatches: 200
    cachePerCall: true
    allowCrossUserBatching: true
    allowPendingTagBatching: false
    autoDetectBypass: false
    bypassContracts:
      # Contracts that check msg.sender code size (e.g., Chronicle Oracle)
      - "0x057f30e63A69175C69A4Af5656b8C9EE647De3D0"
```

Validation and defaults:
- `windowMs > 0`, `minWaitMs >= 0`, `minWaitMs <= windowMs`
- `safetyMarginMs` defaults to `min(2, minWaitMs)` when omitted
  (note: if `minWaitMs=0`, this yields `0`; operators should set a non-zero
  safety margin if they expect tight deadlines)
- `maxCalls > 1`, `maxCalldataBytes > 0`, `maxQueueSize > 0`
- If invalid, disable batching for that network and log a warning.

Maintain backward compatibility with existing `multicall3Aggregation: true|false`.

## Algorithm Sketch
```
Enqueue(req):
  if not eligible or deadline too tight: return notHandled
  if SkipCacheRead == false:
    if cache hit: return cached response
  if batch for key is flushing: create a new batch for key
  add to batch key
  if callKey duplicate: attach waiter and wait for result
  adjust batch flush time (deadline-aware)
  if caps reached: flush now
  wait for flush or immediate join result

Flush(batch):
  build multicall req (mark as composite, skip rate limit)
  forward via Network.Forward
  if success: map responses + per-call cache writes
  else if fallback-eligible: forward individually
  else: propagate infra error to all
```

## Testing Plan
- Unit: eligibility, normalization, keying, caps, deadline-aware flush.
- Concurrency: batcher with multiple goroutines, cancellations, timeouts.
- Concurrency: request arrives during flush for same batch key (new batch).
- Integration: mixed batch/single calls, cache hits, fallback paths.
- Rate limit: ensure per-request budgets are only consumed once.
- Recursion: multicall request is never re-batched.
- Chaos: inject upstream errors, invalid responses, length mismatches.

## Migration Plan
1. Implement batching in the network layer behind config.
2. Keep HTTP-layer batching behind a kill switch or remove once stable.
3. Enable on a subset of networks, monitor metrics, tune caps.

## Open Questions
- Should `SkipCacheRead` live in the key (simpler, more fragmentation) or be
  handled per-request (more complex, better batching)?
- Should batching of `pending` tag be allowed by default?
- Should upstream-specific caps be introduced in v2?
- Should permits be reclaimed for requests canceled before flush?
