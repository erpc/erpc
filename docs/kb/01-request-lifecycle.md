# KB: End-to-end request lifecycle & architecture

> status: complete
> source-dirs: erpc/request_processor.go, erpc/query_executor.go, erpc/network_executor.go, erpc/query_pipe_through.go, erpc/query_field_projection.go, erpc/query_shim.go, erpc/networks.go, erpc/projects.go, erpc/http_server.go, common/request.go, common/response.go, architecture/evm/hooks.go

## L1 — Capability (CTO view)

eRPC is an EVM RPC proxy that accepts JSON-RPC over HTTP (or gRPC for query streams) and routes each request through a layered pipeline: auth → project-level rate limiting → network resolution → cache check → failsafe executor (timeout → consensus → retry → hedge) → upstream selection loop → response normalization → async cache write. The pipeline eliminates duplicate in-flight requests (multiplexing), short-circuits on static responses and future-block lookups, and enriches every response with diagnostic headers (`X-ERPC-*`). The architecture outcome is lower latency (hedging, sticky failover), higher data fidelity (multi-upstream consensus, integrity checks), and operator visibility (Prometheus metrics + OpenTelemetry spans).

For structured EVM query-stream requests (blocks, logs, transactions, traces, transfers), a separate `EvmQueryExecutor` path routes natively to upstream gRPC BDS clients, or shims the query via sequential `eth_getBlockByNumber` / `eth_getLogs` / `trace_block` calls when no native support is present.

## L2 — Mechanics (staff-engineer view)

### Unary JSON-RPC path (HTTP → response)

**1. HTTP ingress** (`erpc/http_server.go`)

The HTTP handler (`createRequestHandler`) parses the URL path for `{projectId}/{architecture}/{chainId}` segments, applies aliasing rules if configured, sets `Content-Type: application/json` plus `X-ERPC-Version`/`X-ERPC-Commit` response headers, reads and decompresses the request body (gzip supported), and parses single or batch JSON-RPC payloads. Each item in a batch is dispatched concurrently as a goroutine; results are re-assembled at the end. The handler resolves the real client IP from trusted forwarder headers before any auth/rate-limit checks.

**2. RequestProcessor** (`erpc/request_processor.go:35-67`)

`RequestProcessor.ProcessUnary` is the entry point from both HTTP and gRPC. It:
1. Resolves the `Project` from `ERPC.GetProject(projectId)`.
2. Wraps the raw JSON bytes in `common.NormalizedRequest` and calls `Validate()` (empty body or missing `method` field → immediate error).
3. Sets client IP on the request.
4. Authenticates the consumer via `project.AuthenticateConsumer(ctx, nq, method, authPayload)`.
5. Sets the resolved `User` on the request.
6. Resolves `networkID = "{architecture}:{chainId}"` and calls `project.GetNetwork(ctx, networkID)`.
7. Calls `nq.ApplyDirectiveDefaults(network.Config().DirectiveDefaults)` to pre-populate request directives from config.
8. Delegates to `project.Forward(ctx, networkID, nq)`.

**3. Project layer** (`erpc/projects.go`)

`PreparedProject.Forward`:
1. Sets the network on the request (`nq.SetNetwork(network)`) for metric label resolution.
2. Acquires a project-level rate limit permit (`AcquireRateLimitPermit` → budget lookup → `TryAcquirePermit`). If the project has no `rateLimitBudget`, this is a no-op.
3. Increments `erpc_network_requests_received_total`.
4. Calls `doForward`, which fires the `HandleProjectPreForward` EVM hook first — handling `eth_blockNumber`, `eth_call`, `eth_chainId`, `eth_getLogs`, `trace_filter` method-specific pre-processing (e.g. early cache lookups, block-range splitting). If the hook handles the request, `HandleNetworkPostForward` is called on the result before returning.
5. If the hook did not handle: calls `network.Forward(ctx, nq)`.
6. Wraps the result with `HandleNetworkPostForward` for method-specific post-processing (`eth_getBlockByNumber`, `eth_getLogs`, `eth_sendRawTransaction`, `trace_filter`).
7. On success: emits `erpc_network_successful_request_total`, `erpc_network_request_duration_seconds`, and triggers shadow-upstream fan-out if configured.
8. On failure: emits `erpc_network_failed_request_total` and `erpc_network_request_duration_seconds` with `vendor=<error>`.

**4. Network layer** (`erpc/networks.go:931-1603`)

`Network.Forward` is the core dispatch function:

1. Sets network and cache DAL on request.
2. Re-applies `DirectiveDefaults` (defensive: the http_server path already called this, but gRPC callers may not have).
3. Binds request to `ctx` via `context.WithValue(ctx, common.RequestContextKey, req)` — used downstream for selector-scoped served-tip resolution.
4. **Static response check**: if `network.cfg.StaticResponses` is non-empty, attempts to match. Hit → return immediately.
5. **Multiplexing**: calls `handleMultiplexing`. If an identical in-flight request exists, this goroutine becomes a follower and waits on the leader's result (`mlx.Wait()`). If this goroutine is first, it becomes the leader and proceeds.
6. **Cache read**: if `cacheDal != nil` and `!req.ShouldSkipCacheRead("")`, calls `cacheDal.Get(ctx, req)`. Non-null cache hit → return immediately (closes multiplexer with result).
7. **Upstream ordering**: calls `policyEngine.GetOrdered(networkId, method, req.Finality(ctx).String())`. Falls back to raw registry order on cold start.
8. Publishes request to policy engine probe bus (non-blocking).
9. **EVM network pre-forward hook** (`HandleNetworkPreForward`): upstream-aware pre-processing for `eth_getLogs`, `eth_chainId`, `trace_filter` (e.g. range splitting, chainId shortcircuit).
10. **Future-block short-circuit**: for `eth_getBlockByNumber` with a concrete block number beyond all eligible upstream heads, returns a truthful `null` immediately without touching any upstream.
11. **Method guard**: `shouldHandleMethod` blocks `eth_accounts`/`eth_sign` and requires stateful methods to target a single upstream.
12. **Network rate limiting**: `acquireRateLimitPermit` checks network-level budget.
13. **Request preparation**: `prepareRequest` calls `evm.NormalizeHttpJsonRpc` to normalize params (block tag interpolation, EVM-specific transformations).
14. **Failsafe executor selection**: `getFailsafeExecutor` iterates `failsafeExecutors` in config order, returning the first whose `MatchMethod` (wildcard) and `MatchFinality` match this request. Panics if none found (misconfiguration — always returns at minimum the default `*` executor).
15. **Sweep closures**: builds `tryOneUpstream` (single upstream, for consensus slots) and `runUpstreamSweep` (all upstreams, for non-consensus path).
16. **Failsafe execution**: calls `failsafeExecutor.Run(ctx, req, tryOneUpstream, runUpstreamSweep)` — see §Failsafe executor chain.
17. **Post-execution**: normalizes errors (context timeout sentinel → typed `ErrDynamicTimeoutExceeded`), emits timeout metric if applicable, handles `LastValidResponse` fallback.
18. **Async cache write**: on successful non-null response, force-materializes the JSON-RPC response then fires `cacheDal.Set` in a goroutine with a 10-second deadline.
19. **State poller enrichment**: on success, calls `enrichStatePoller` — for `eth_getBlockByNumber?latest/finalized` and `eth_blockNumber`, pipes the returned block number back to the upstream's `EvmStatePoller.SuggestLatestBlock/SuggestFinalizedBlock`.
20. Closes multiplexer with final result.

**5. Failsafe executor chain** (`erpc/network_executor.go`)

The `networkExecutor.Run` method orchestrates the nesting policy stack:

```
timeout (wraps entire invocation)
  ↳ if consensus enabled && !SkipConsensus directive:
      consensus(
        retry(
          hedge(tryOneUpstream)   ← one upstream per slot
        )
      )
    else:
      retry(
        hedge(runUpstreamSweep)   ← all upstreams per sweep
      )
```

- **Timeout** (`e.timeout`): wraps the context with `context.WithTimeoutCause`. Uses `common.NewTimeoutFunc` which can be a static duration or an adaptive function (quantile-based per-method latency). Timeout fires `common.ErrDynamicTimeoutExceeded` as the cause.
- **Consensus**: delegated entirely to the `consensusRunner` interface (implemented by `consensus.*Consensus`). The executor does not import the consensus package directly.
- **Retry** (`runRetry`): iterates up to `MaxAttempts` times. On each iteration calls `hedged(ctx)`. Checks `shouldRetryWithReason` to decide whether to retry and emits `erpc_network_retry_attempt_total{reason}`. For data-unavailable retries (`block_unavailable`, `empty_result`, `missing_data`), calls `computeDelay` which uses the EMA block-time-relative delay (or `EmptyResultDelay` fallback). For genuine-error retries, uses exponential backoff via `failsafe.ComputeBackoff`. Special case: `eth_sendRawTransaction` with `ErrCodeEndpointExecutionException` is surfaced directly without retry-exhausted wrapping. Tracks `firstInformativeErr` so degenerate `ErrNoUpstreamsLeftToSelect` errors on later attempts don't mask the original cause.
- **Hedge** (`runHedge`): fires up to `MaxCount` hedge attempts after adaptive delay. Write methods (non-idempotent per `evm.IsNonRetryableWriteMethod`) are never hedged. Hedge winner election: `keep(resp, err)` — rejects `ErrNoUpstreamsLeftToSelect`, empty exhausted, retryable errors, and emptyish results for non-accepted methods (to let the race continue for a real result). Winner emits `erpc_network_hedge_winner_total`. Cancelled hedge legs emit `erpc_network_hedge_discards_total`.

**6. Upstream sweep loop** (`erpc/networks.go:1226-1407`)

Inside `sweepFn`, for each loop iteration:
1. Checks context for cancellation.
2. Calls `req.NextUpstream()` — atomic round-robin over `upstreamList`, skipping already-consumed (permanent errors) and selector-filtered upstreams. Returns `ErrNoUpstreamsLeftToSelect` when exhausted.
3. **Block availability gating**: `checkUpstreamBlockAvailability` compares the request's block number against the upstream's `EvmEffectiveLatestBlock`/`EvmEffectiveFinalizedBlock`. Retryable if block is within `MaxRetryableBlockDistance` (default 128) of upstream head; not retryable if below lower bound. Fail-open on poller errors.
4. Increments hedge metric if this is a hedged attempt.
5. Calls `doForward(ctx, u, req, false, isHedgeAttempt)` which:
   - Tries `HandleUpstreamPreForward` for `eth_getLogs`, `eth_chainId`, `trace_filter`, `eth_query*`.
   - Falls through to `u.Forward(ctx, req, false, isHedgeAttempt)`.
   - Always calls `HandleUpstreamPostForward` for validation and post-processing.
6. `normalizeResponse`: rewrites the JSON-RPC response ID to match the client's original request ID (byte-perfect, preserving large integers and fractional IDs via `IDRawBytes()`).
7. `MarkUpstreamCompleted`: stores error/empty-result in `ErrorsByUpstream`, releases from `ConsumedUpstreams` if the upstream is retryable (so it can be re-selected in the next failsafe retry round).
8. Deterministic errors (client errors, execution reverts): return immediately without trying more upstreams.
9. On success: records winning upstream in `ExecState`, sets `Attempts/Retries/Hedges` on response, returns.
10. If all upstreams exhausted: returns `ErrUpstreamsExhausted`.

**7. Post-forward and response write**

Back in `projects.go`, on success, counter/duration metrics are emitted. Back in `http_server.go`:
- `setResponseHeaders` emits the full `X-ERPC-*` diagnostic surface (see §Response headers).
- `determineResponseStatusCode` picks the HTTP status code (200 for JSON-RPC per spec, appropriate codes for transport-level errors).
- `v.WriteTo(w)` streams the JSON-RPC response body.
- `v.Release()` is called in a goroutine to free buffers after the async cache write completes.

### gRPC query-stream path

`RequestProcessor.ProcessQueryStream` (`erpc/request_processor.go:69-147`) handles EVM query methods (`eth_queryBlocks`, `eth_queryTransactions`, `eth_queryLogs`, `eth_queryTraces`, `eth_queryTransfers`):

1. Same auth flow as unary (project, consumer auth, network, rate limit).
2. Creates `EvmQueryExecutor` and calls `Execute(ctx, queryReq, onPage)`.
3. `EvmQueryExecutor.Execute` dispatches to the appropriate `queryBlocks/queryTransactions/queryLogs/queryTraces/queryTransfers` method.
4. Each query method: resolves `fromBlock/toBlock` via `resolveQueryBounds` (tags `latest/finalized/safe/earliest` resolved from network's `EvmHighestLatestBlockNumber/EvmHighestFinalizedBlockNumber`; cursor applied as block offset), then tries native upstream pass-through first, falls back to shim.

**Native pipe-through** (`erpc/query_pipe_through.go`): calls `tryQueryUpstreams` which iterates policy-ordered upstreams, checks for gRPC BDS client support (`supportsQueryMethods`), and opens a streaming RPC. Pages are received via `recvProtoStream` and forwarded to `onPage`. `StreamError.PageEmitted` gates retry safety — if a page was already emitted to the caller, retry is forbidden.

**Shim** (`erpc/query_shim.go`): translates the structured query into standard JSON-RPC subrequests via `qe.forwardSubrequest` (which calls `network.Forward` recursively). Each shim method iterates blocks with `blockIterator` (ASC or DESC), applies filters (`matchTransactionFilter`, `matchTraceFilter`, `matchTransferFilter`), and applies field projection (`ProjectBlockFields`, `ProjectTransactionFields`, etc.) before calling `onPage`. Log pagination uses `paginateLogsByBlock` (block-boundary-aware, never splits a block's logs across pages). Transfers shimmed via traces: `shimQueryTransfers` calls `shimQueryTraces` internally and converts with `evm.NativeTransfersFromTraces`. Traces attempt `trace_block` first; fall back to `debug_traceBlockByNumber?callTracer` if unsupported.

**Field projection** (`erpc/query_field_projection.go`): `ProjectBlockFields`, `ProjectTransactionFields`, `ProjectLogFields`, `ProjectTraceFields`, `ProjectTransferFields` zero out unselected proto fields. Hash fields are always preserved for cursor semantics (see `ProjectBlockFields:13-15` where `Hash` is never zeroed). Proto clones are made before projection to avoid mutating shared data.

### Directive system

Request directives are applied in layers:
1. `ApplyDirectiveDefaults` from `network.Config().DirectiveDefaults` — no-op if directives already set.
2. `EnrichFromHttp` from HTTP headers and query params — Copy-On-Write if directives already exist.

Directive precedence: HTTP headers → query params (query overrides headers for `use-upstream`, `retry-empty`, `retry-pending`, `skip-cache-read`, `skip-interpolation`, `skip-consensus`). Header-only directives (validation fields like `X-ERPC-Validate-*`) are not settable via query params.

Internal requests (state pollers, chainId probes) set `IsInternal=true` which bypasses retry, hedge, and circuit breaker policies; only per-attempt timeout applies.

## L3 — Exhaustive reference (agent view)

### Config fields

The lifecycle is primarily governed by `NetworkConfig.Failsafe` (an array of `NetworkFailsafeConfig`), `NetworkConfig.DirectiveDefaults`, and `NetworkConfig.SelectionPolicy`. Request-level directives override config-level defaults per-request.

| dotted yaml path | type | default | behavior | citation |
|---|---|---|---|---|
| `networks[].failsafe[].matchMethod` | string | `"*"` | Wildcard pattern; first matching executor wins | `erpc/network_executor.go:83-84` |
| `networks[].failsafe[].matchFinality` | `[]DataFinalityState` | `[]` (any) | Empty = match all finalities | `erpc/network_executor.go:79` |
| `networks[].failsafe[].timeout` | `*Duration` | nil (no timeout) | Wraps full executor invocation; cause is `ErrDynamicTimeoutExceeded` | `erpc/network_executor.go:86-89` |
| `networks[].failsafe[].retry.maxAttempts` | int | 1 (no retry) | Upper bound on retry iterations at network scope | `erpc/network_executor.go:210-213` |
| `networks[].failsafe[].retry.emptyResultMaxAttempts` | int | 0 (disabled) | Cap on data-unavailable retries (block_unavailable, empty_result, missing_data) independently of maxAttempts | `erpc/network_executor.go:363-369` |
| `networks[].failsafe[].retry.emptyResultDelay` | `*Duration` | nil | Fixed wait before data-unavailable retry, used before block-time EMA warms up | `erpc/network_executor.go:501-507` |
| `networks[].failsafe[].retry.emptyResultAccept` | `[]string` | `common.DefaultEmptyResultAccept()` | Methods for which an empty/null result is NOT retried (e.g. `eth_call`, `eth_getLogs`) | `erpc/network_executor.go:89-93` |
| `networks[].failsafe[].retry.backoffFactor` | float | set by `failsafe.ComputeBackoff` defaults | Exponential backoff multiplier for genuine-error retries | `erpc/network_executor.go:512-513` |
| `networks[].failsafe[].retry.backoffMaxDelay` | `*Duration` | per `failsafe.ComputeBackoff` | Maximum inter-retry delay | `erpc/network_executor.go:512-513` |
| `networks[].failsafe[].hedge.maxCount` | int | 0 (disabled) | Maximum concurrent hedge attempts | `erpc/network_executor.go:126-131` |
| `networks[].failsafe[].hedge.delay` | `AdaptiveDuration` | — | `Base` scalar or `Quantile`+tracker for adaptive timing | `erpc/network_executor.go:540-543` |
| `networks[].failsafe[].consensus` | `*ConsensusConfig` | nil | Enables consensus; delegates to `consensus.Run` interface | `erpc/network_executor.go:113-115` |
| `networks[].directiveDefaults.*` | `DirectiveDefaultsConfig` | see individual fields | Applied once before HTTP header enrichment; no-op if already set | `common/request.go:563` |
| `networks[].directiveDefaults.retryEmpty` | `*bool` | false | Retry on emptyish upstream responses | `common/request.go:575-578` |
| `networks[].directiveDefaults.retryPending` | `*bool` | true (implicit) | Retry tx-lookup methods until confirmed | `common/request.go:579-583` |
| `networks[].directiveDefaults.skipCacheRead` | `*string` | `""` (off) | `"true"` = skip all; connector-id pattern = skip matching | `common/request.go:584-590` |
| `networks[].directiveDefaults.useUpstream` | `*string` | `""` | Upstream id or glob filter; applied inside `NextUpstream` loop | `common/request.go:591-593` |
| `networks[].directiveDefaults.skipInterpolation` | `*bool` | false | Prevent block tag → hex replacement in outbound params | `common/request.go:594-596` |
| `networks[].directiveDefaults.skipConsensus` | `*bool` | false | Bypass consensus for this request; retry+hedge still apply | `common/request.go:597-599` |
| `networks[].evm.maxRetryableBlockDistance` | `*int64` | 128 | Blocks ahead of upstream head that are still considered retryable (not fatal) | `erpc/networks.go:1975-1979` |
| `projects[].rateLimitBudget` | string | `""` | Project-level rate limit budget ID; empty = no project rate limiting | `erpc/projects.go:270-273` |
| `networks[].rateLimitBudget` | string | `""` | Network-level rate limit budget ID | `erpc/networks.go:2269-2272` |
| `networks[].multiplexing.enabled` | bool | false (check `MultiplexingEnabled()`) | Deduplicate identical in-flight requests | `erpc/networks.go:1990` |
| `networks[].staticResponses` | `[]StaticResponseConfig` | `[]` | Matched before cache/upstream; return without any upstream contact | `erpc/networks.go:976-981` |
| `server.executionHeaders` | `*ExecutionHeadersMode` | `"all"` | `"all"` = full per-attempt trace; `"summary"` = counters only; `"off"` = none | `erpc/http_server.go:1082-1085` |

### Behaviors & algorithms

#### Upstream selection (round-robin with consumption tracking)

`NormalizedRequest.NextUpstream()` (`common/request.go:1408-1478`):
- Iterates `upstreamList` starting at `UpstreamIdx % len`, incrementing on each call.
- Skips: upstreams not matching `UseUpstream` pattern; upstreams already in `ConsumedUpstreams`; upstreams with a permanent error in `ErrorsByUpstream` (non-retryable errors stay permanently skipped within a round; retryable errors are re-admitted via `MarkUpstreamCompleted`).
- Atomically reserves via `ConsumedUpstreams.LoadOrStore` — safe for concurrent hedge goroutines.
- Returns `ErrNoUpstreamsLeftToSelect` when all are exhausted.

`MarkUpstreamCompleted` (`common/request.go:1480-1520`): releases from `ConsumedUpstreams` if the upstream had a retryable error, no response, or an empty result — making it available for the next failsafe retry round.

#### Block availability gating

`checkUpstreamBlockAvailability` (`erpc/networks.go:1919-1987`):
1. Only runs for EVM networks when `resolveEnforceBlockAvailability` returns true.
2. Reads block number from `req.EvmBlockNumber()` cache (set by `evm.NormalizeHttpJsonRpc`), or falls back to `evm.ExtractBlockReferenceFromRequest`.
3. If `bn <= 0` (unknown block): fail-open (allow request).
4. Calls `eu.EvmAssertBlockAvailability(ctx, method, AvailbilityConfidenceBlockHead, true, bn)`.
5. If unavailable: `bn > latestBlock && distance <= maxRetryableBlockDistance` → retryable skip; otherwise → non-retryable skip. Non-retryable skips trigger async `sp.PollLatestBlockNumber`.

`resolveEnforceBlockAvailability` precedence (`erpc/networks.go:1797-1831`):
1. Method-level user override (differs from system default).
2. `network.cfg.Evm.EnforceBlockAvailability`.
3. Upstream has explicit `BlockAvailability` bounds configured.
4. System default for the method (`DefaultWithBlockCacheMethods`).
5. Fallback: enabled.

#### Served tip (network head tracking)

`EvmHighestLatestBlockNumber` / `EvmHighestFinalizedBlockNumber` (`erpc/networks.go:517-698`):
- **Max mode** (default): returns `MAX(effective latest block)` across eligible non-syncing upstreams.
- **Cluster-min mode** (when `evm.servedTip.enabled`): `evm.ComputeServedTipCandidate` over policy-eligible upstreams, strict-monotonic clamp via shared counter (cross-pod consistency). Velocity-gate drops fast-moving outlier upstreams.
- **Selector-scoped**: when request has `UseUpstream` directive, resolves tip only among matching upstreams. Named tag groups get dedicated monotonic counters (capped at `maxServedTipPartitions=16`).
- **GuaranteedMethod floor**: caps the global network tip to the minimum cluster-min tip of `GuaranteedMethods`' supporting upstreams, preventing routes that promise `trace_*` from advertising a tip that trace-supporting upstreams can't serve.

#### Multiplexing

`handleMultiplexing` (`erpc/networks.go:1989-2060`): uses `inFlightRequests sync.Map` keyed by cache hash. First goroutine becomes leader; subsequent identical requests become followers and wait on `mlx.Wait()`. On leader close, followers get a copy via `CopyResponseForRequest` (response ID is patched to match each follower's request ID).

#### Response ID normalization

`normalizeResponse` (`erpc/networks.go:2234-2266`): after every upstream attempt, rewrites the JSON-RPC `id` field in the response to match the client's original request ID. Uses `IDRawBytes()` for byte-perfect round-trip (large integers, fractional IDs, string IDs). Falls back to typed `ID` if raw bytes unavailable.

#### Retry reasons and delays

`shouldRetryWithReason` (`erpc/network_executor.go:374-463`) returns:
- `"execution_exception_retryable"`: EVM execution exception with `retryableTowardNetwork=true`.
- `"block_unavailable"`: `ErrCodeUpstreamBlockUnavailable`, subject to `emptyResultMaxAttempts` cap.
- `"missing_data"`: `ErrCodeEndpointMissingData` unless `RetryEmpty=false` directive, subject to cap.
- `"retryable_error"`: generic `IsRetryableTowardNetwork` errors.
- `"empty_result"`: emptyish response with `RetryEmpty=true` directive, subject to cap, skipped for methods in `emptyResultAccept`.
- `"pending_tx"`: `eth_getTransactionReceipt`, `eth_getTransactionByHash`, etc. with `RetryPending=true`, subject to cap.

Composite requests (`IsCompositeRequest()`) are never retried at network scope.

`computeDelay` (`erpc/network_executor.go:481-514`): for `block_unavailable`, `empty_result`, `missing_data`:
1. Uses `dynamicBlockUnavailableDelay()` — EMA block-time × `BlockUnavailableDelayMultiplier` if warmed.
2. Falls back to `cfg.Retry.EmptyResultDelay` fixed duration.
3. For genuine-error retries: `failsafe.ComputeBackoff(cfg, attempt)` (exponential backoff).

#### Hedge winner election

`keep(resp, err)` in `runHedge` (`erpc/network_executor.go:551-625`):
- Rejects: `ErrNoUpstreamsLeftToSelect`, empty `ErrUpstreamsExhausted` (no upstreams tried), retryable wrapped errors.
- Rejects emptyish results unless method is in `emptyResultAccept` (so a fast `null` from one leg doesn't cancel siblings that may return real data).
- Accepts non-null, non-retryable, non-empty responses.
- Discarded legs emit `ErrUpstreamHedgeCancelled`.

#### Finality determination

`Network.GetFinality` (`erpc/networks.go:1645-1743`): for a (request, response) pair:
1. Method-level config override (`Methods.Definitions[method].Finalized/Realtime`).
2. Block reference extraction from request params.
3. If no block number: try extracting from response body.
4. Non-numeric block tags → `Realtime`.
5. Concrete block number: check against `EvmIsBlockFinalized` on the response's upstream, then the request's `LastUpstream`, then the network's `LowestFinalizedBlock` heuristic.

Finality is cached atomically on both `NormalizedRequest` and `NormalizedResponse` (lazy init, only stored once a definitive answer is available).

#### EVM hooks (architecture/evm/hooks.go)

Four injection points:
- `HandleProjectPreForward` (before cache): `eth_blockNumber` (early return from cache), `eth_call` (static call optimization), `eth_chainId` (served from config), `eth_getLogs` (proactive range splitting), `trace_filter` (splitting).
- `HandleNetworkPreForward` (after upstream selection): `eth_getLogs` (range enforcement), `eth_chainId` (shortcircuit), `trace_filter` (range enforcement).
- `HandleUpstreamPreForward` (per upstream): `eth_getLogs`, `eth_chainId`, `trace_filter`, `eth_query*`.
- `HandleUpstreamPostForward` (after each upstream response): validation directives for `eth_getLogs`, `eth_getBlockReceipts`, `eth_getBlockByNumber/Hash`; `eth_sendRawTransaction` idempotency; trace_filter normalization; `markUnexpectedEmpty` for methods in `MarkEmptyAsErrorMethods`.

Content-validation errors (`ErrCodeEndpointContentValidation`) in `HandleUpstreamPostForward` clear `lastValidResponse` so consensus/retry don't mistakenly use the invalid response.

#### Request directive reference

| header | query param | type | behavior |
|---|---|---|---|
| `X-ERPC-Retry-Empty` | `retry-empty` | bool | Retry emptyish upstream responses |
| `X-ERPC-Retry-Pending` | `retry-pending` | bool | Retry pending tx lookups |
| `X-ERPC-Skip-Cache-Read` | `skip-cache-read` | string (`true`/pattern) | Skip cache read for all or specific connectors |
| `X-ERPC-Use-Upstream` | `use-upstream` | string (id/glob/tag) | Pin request to matching upstream(s) |
| `X-ERPC-Skip-Interpolation` | `skip-interpolation` | bool | Don't replace block tags with hex in outbound params |
| `X-ERPC-Skip-Consensus` | `skip-consensus` | bool | Bypass consensus; retry+hedge still apply |
| `X-ERPC-Enforce-Highest-Block` | `enforce-highest-block` | bool | Block integrity validation |
| `X-ERPC-Enforce-GetLogs-Range` | `enforce-getlogs-range` | bool | Enforce getLogs block range |
| `X-ERPC-Enforce-Non-Null-Tagged-Blocks` | `enforce-non-null-tagged-blocks` | bool | Reject null on tagged block lookups |
| `X-ERPC-Enforce-Log-Index-Strict-Increments` | `enforce-log-index-strict-increments` | bool | Validate log index ordering |
| `X-ERPC-Validate-Logs-Bloom-Emptiness` | `validate-logs-bloom-emptiness` | bool | Bloom↔logs consistency check |
| `X-ERPC-Validate-Logs-Bloom-Match` | `validate-logs-bloom-match` | bool | Recalculate bloom from logs |
| `X-ERPC-Validate-Tx-Hash-Uniqueness` | `validate-tx-hash-uniqueness` | bool | No duplicate tx hashes in block |
| `X-ERPC-Validate-Transaction-Index` | `validate-transaction-index` | bool | Validate tx positions |
| `X-ERPC-Receipts-Count-Exact` | `receipts-count-exact` | int | Expected exact receipt count |
| `X-ERPC-Receipts-Count-At-Least` | `receipts-count-at-least` | int | Minimum receipt count |
| `X-ERPC-Validation-Expected-Block-Hash` | `validation-expected-block-hash` | string | Expected block hash ground truth |
| `X-ERPC-Validation-Expected-Block-Number` | `validation-expected-block-number` | int | Expected block number ground truth |
| `X-ERPC-Validate-Transactions-Root` | `validate-transactions-root` | bool | Check transactionsRoot consistency |
| `X-ERPC-Validate-Header-Field-Lengths` | `validate-header-field-lengths` | bool | Validate EVM header field byte lengths |
| `X-ERPC-Validate-Transaction-Fields` | `validate-transaction-fields` | bool | Validate transaction fields |
| `X-ERPC-Validate-Transaction-Block-Info` | `validate-transaction-block-info` | bool | Validate tx block info |
| `X-ERPC-Validate-Log-Fields` | `validate-log-fields` | bool | Validate log fields |

Source: `common/request.go:38-114`.

### Observability

#### Prometheus metrics (request lifecycle path)

| metric | labels | fires when | source |
|---|---|---|---|
| `erpc_network_requests_received_total` | project, network, category, finality, user, agent_name | request enters `project.Forward` | `telemetry/metrics.go:407`, `erpc/projects.go:123` |
| `erpc_network_successful_request_total` | project, network, vendor, upstream, category, attempt, finality, emptyish, user, agent_name | `project.Forward` returns non-error | `telemetry/metrics.go:497`, `erpc/projects.go:192` |
| `erpc_network_failed_request_total` | project, network, category, attempt, error, severity, finality, user, agent_name | `project.Forward` returns error | `telemetry/metrics.go:491`, `erpc/projects.go:231` |
| `erpc_network_request_duration_seconds` | project, network, vendor, upstream, category, finality, user | per-request total duration (success and error) | `telemetry/metrics.go:792`, `erpc/projects.go:211,242` |
| `erpc_network_retry_attempt_total` | project, network, category, reason, finality | each network-scope retry attempt | `telemetry/metrics.go:466`, `erpc/network_executor.go:289-299` |
| `erpc_network_data_unavailable_wait_seconds` | project, network, category, reason, finality | deliberate catch-up delay before data-unavailable retry | `telemetry/metrics.go:835`, `erpc/network_executor.go:323-331` |
| `erpc_network_hedged_request_total` | project, network, upstream, category, hedgeCount, finality, user, agent_name | hedge attempt fired to a specific upstream | `telemetry/metrics.go:425`, `erpc/networks.go:1289` |
| `erpc_network_hedge_discards_total` | project, network, upstream, category, attempt, hedge, finality, user, agent_name | hedged request discarded (another leg won) | `telemetry/metrics.go:431`, `erpc/networks.go:1897` |
| `erpc_network_hedge_winner_total` | project, network, upstream, category, finality | upstream won a hedge race | `telemetry/metrics.go:476`, `erpc/network_executor.go:564` |
| `erpc_network_timeout_fired_total` | project, network, category, finality, scope | network-scope timeout fired | `telemetry/metrics.go:437`, `erpc/networks.go:1437` |
| `erpc_upstream_request_errors_total` | ... | upstream skip due to block availability | `erpc/networks.go:1852` |
| `erpc_upstream_wrong_empty_response_total` | project, vendor, network, upstream, category, finality, user, agent_name | upstream returned empty while another returned data | `telemetry/metrics.go:384`, `erpc/networks.go:1554` |
| `erpc_network_multiplexed_requests_total` | project, network, category | follower piggybacks on in-flight leader | `telemetry/metrics.go:413` |
| `erpc_network_static_response_served_total` | project, network, category | static response matched | `telemetry/metrics.go:419` |
| `erpc_network_served_tip_block_number` | project, network, lane, axis | served-tip pick computed | `telemetry/metrics.go:130`, `erpc/networks.go:821` |
| `erpc_network_served_tip_lag_blocks` | project, network, lane, axis | deliberate cushion below freshest upstream | `telemetry/metrics.go:145`, `erpc/networks.go:834` |
| `erpc_network_served_tip_upstream_excluded_total` | project, network, upstream, axis, reason | upstream dropped from served-tip computation (velocity/outlier) | `telemetry/metrics.go:158`, `erpc/networks.go:845,849` |

#### Trace spans (OTel)

Key spans on the unary request path (all via `common.StartSpan` or `common.StartDetailSpan`):
- `Network.Forward` — top-level network span; `cache.hit`, `multiplexed`, `failsafe.matched_method` attributes.
- `PolicyEngine.GetOrdered` — upstream ordering time.
- `Network.forwardAttempt` — per-attempt span with `execution.attempt`, `execution.retry`, `execution.hedge`.
- `Network.UpstreamLoop` — per-upstream iteration; `upstream.id`, `upstream.latest_block`, `skipped`, `skip_reason` attributes.
- `Network.TryForward` — call to `doForward` per upstream.
- `Upstream.PreForwardHook` / `Upstream.PostForwardHook` — EVM method hooks.
- `Network.NormalizeResponse` — response ID rewrite.
- `Network.EnrichStatePoller` — state poller suggestion.
- `Project.Forward` → `Project.PreForwardHook` → `Network.PostForwardHook`.
- `QueryStream.Handle` — gRPC query stream top-level span.
- `Query.Execute`, `Query.ResolveQueryBounds`, `Query.ShimBlocks/Logs/Transactions/Traces/Transfers`, `Query.ForwardSubrequest`.

#### Response headers (HTTP)

Emitted by `setResponseHeaders` (`erpc/http_server.go:1105`); controlled by `server.executionHeaders`:

| header | when present | value |
|---|---|---|
| `X-ERPC-Version` | always | erpc version string |
| `X-ERPC-Commit` | always | git commit SHA |
| `X-ERPC-Attempts` | always (`ExecState`) | total physical ops (upstream + cache) |
| `X-ERPC-Upstream-Attempts` | always | upstream-scope attempt count |
| `X-ERPC-Upstream-Retries` | always | upstream-scope retry count |
| `X-ERPC-Upstream-Hedges` | always | upstream-scope hedge count |
| `X-ERPC-Network-Attempts` | always | network-scope attempt count (rotation count) |
| `X-ERPC-Network-Retries` | always | network-scope retry count |
| `X-ERPC-Network-Hedges` | always | network-scope hedge count |
| `X-ERPC-Cache-Attempts` | only if cache exercised | cache-scope attempt count |
| `X-ERPC-Consensus-Slots` | only if consensus used | number of consensus slots |
| `X-ERPC-Consensus-Disputes` | only if disputes | number of disputes |
| `X-ERPC-Cache` | if response available | `HIT` or `MISS` |
| `X-ERPC-Upstream` | if upstream response | upstream ID that served it |
| `X-ERPC-Duration` | if response | total request duration in milliseconds |
| `X-ERPC-Upstreams` | `executionHeaders=all` | per-attempt trace: `id=role:outcome:durationMs:won\|lost` |

### Edge cases & gotchas

1. **`ApplyDirectiveDefaults` is a no-op if directives are already set** (`common/request.go:563-565`). The HTTP server calls it before `EnrichFromHttp`; `Network.Forward` also calls it defensively. The guard ensures HTTP-set directives are never overwritten by the network config's defaults. A request constructed without HTTP headers will use the network defaults.

2. **Multiplexer race on close**: if a follower arrives while the leader's cleanup goroutine is running (after `mlx.closed=true`), the `LoadOrStore` loop retries. The follower will either find a new leader slot or become the new leader itself. Source: `erpc/networks.go:2017-2025`.

3. **Async cache write does not block the response**: the goroutine at `erpc/networks.go:1494-1518` uses `resp.AddRef()/DoneRef()` so `Release()` waits for the write to complete before freeing buffers. The context for the cache write uses `n.appCtx` (not the request context), so a client disconnect doesn't abort the write. The 10-second timeout is on the write, not on waiting for it.

4. **`ErrNoUpstreamsLeftToSelect` degeneration on retries** (`erpc/network_executor.go:265-284`): after the first retry round, `ConsumedUpstreams` may mark all upstreams as consumed, producing a bare `ErrNoUpstreamsLeftToSelect` that loses the root cause. The executor tracks `firstInformativeErr` and surfaces it in the final `ErrFailsafeRetryExceeded` wrapping.

5. **`eth_sendRawTransaction` is never wrapped in `ErrFailsafeRetryExceeded`** (`erpc/network_executor.go:278-283`): execution-reverted responses for this method are surfaced directly (the tx was broadcast and reverted — that IS the answer, not a retry condition).

6. **Hedge emptyish rejection** (`erpc/network_executor.go:607-624`): a hedge leg that returns an emptyish result (e.g. `null` block from a lagging upstream) does NOT win the race — its result is rejected and the other hedge legs continue. This prevents a fast `null` from cancelling in-flight attempts that could return real data. Methods in `emptyResultAccept` (like `eth_call`) are exempt.

7. **Composite requests skip retry and hedge** (`erpc/network_executor.go:378-380`, `521-526`): requests flagged as `IsCompositeRequest` (e.g. range-split `eth_getLogs`, query shim subrequests) bypass network-scope retry and hedge to avoid exponential amplification of sub-requests.

8. **Block availability check is fail-open**: if `EvmAssertBlockAvailability` errors (poller issues, partial state), the request proceeds to the upstream rather than being gated. Source: `erpc/networks.go:1949-1959`.

9. **Response ID fidelity for large integers** (`erpc/networks.go:2254-2259`): the response ID rewrite uses `IDRawBytes()` — the verbatim bytes from the original JSON. This is critical for JSON-RPC clients that pass 64-bit integer IDs (e.g. `9007199254740993`) which cannot be represented precisely as `float64`. The raw bytes path is taken when `jrq.IDRawBytes()` is non-empty; it falls back to the typed `jrq.ID` for programmatically constructed requests. Source: `erpc/networks_normalize_response_id_fidelity_test.go`.

10. **Cache write panic is recovered with metric** (`erpc/networks.go:1496-1508`): panics in the async cache-write goroutine are recovered and reported via `erpc_unexpected_panic_total{scope="cache-set"}` to prevent crashing the process on cache driver bugs.

11. **Stateful method enforcement** (`erpc/networks.go:2112-2143`): methods marked `Stateful` in `Methods.Definitions` require exactly one targeted upstream. If `UseUpstream` targets >1 or no selector is set and >1 upstreams exist, returns `ErrNotImplemented`. `eth_accounts` and `eth_sign` are hardcoded as unsupported.

12. **Query shim retry safety**: in `EvmQueryExecutor.tryQueryUpstreams` (`erpc/query_executor.go:247-253`), a stream error is only retried on the next upstream if no page was emitted yet (`canRetryQueryStream` checks `StreamError.PageEmitted`). Once a page has been sent to the caller, the stream is committed and the error is returned as-is to prevent partial-result/duplicate-page issues.

13. **`trace_block` → `debug_traceBlockByNumber` fallback in shim** (`erpc/query_shim.go:388-420`): `shimQueryTraces` first tries `trace_block`; if the upstream returns `ErrCodeEndpointUnsupported`, it retries with `debug_traceBlockByNumber?callTracer`. If that is also unsupported, returns gRPC `Unimplemented`.

14. **Paginatelogbyblock never splits a block** (`erpc/query_shim.go:617-649`): when adding a block's logs would exceed the limit and the page already has items, the function stops before that block (producing a cursor). If the first block alone exceeds the limit, all its logs are included (the limit is a soft cap on block boundaries, not absolute log count).

15. **`SkipConsensus` directive falls through to retry+hedge** (`erpc/network_executor.go:178-191`): when `SkipConsensus=true`, the executor skips the consensus branch entirely and falls through to `runRetryHedge(ctx, req, runUpstreamSweep)`. All other failsafe policies (retry, hedge, timeout) still apply in full.

16. **Hash-based projection preserves block hash** (`erpc/query_field_projection.go:12-15`): `ProjectBlockFields` explicitly never zeros the `Hash` field even when `sel.Hash=false`, because the hash is required for cursor semantics in paginated responses.

17. **`TranslateToJsonRpcException` dominant-code selection: all-skipped / all-unsupported edge case** (`common/json_rpc.go:L1499-L1536`): when an `ErrUpstreamsExhausted` is translated to a wire JSON-RPC error, the function scans child errors for the most-frequent non-noise error code. `ErrCodeUpstreamRequestSkipped` and `ErrCodeEndpointUnsupported` are explicitly excluded from the frequency count at L1523-1526 because they are considered "not interesting nor significant." The *fast-path* at L1519-1521 sets `domErr` to the first child in iteration order unconditionally, before any counts are incremented. When **every** upstream error is skipped or unsupported (all are `continue`d), `maxCount` stays 0 and the `counts[c] > maxCount` branch at L1528-1531 never fires, so `domErr` remains that first child. The resulting wire code depends on the first child's type:

   - **First child is `ErrUpstreamRequestSkipped`** (e.g. upstream filtered by `UseUpstream` directive, method-ignored, or block-availability permanent skip): `ErrUpstreamRequestSkipped` contains no `ErrJsonRpcExceptionInternal` in its chain (`common/errors.go:L1328-L1347`). It is not matched by any explicit branch in `TranslateToJsonRpcException`, so it falls through to the generic catch-all at L1616-1629 which emits **`-32603` `JsonRpcErrorServerSideException`** with the deepest message of the skip reason.

   - **First child is `ErrEndpointUnsupported` wrapping a `JsonRpcExceptionInternal`** (typical path when upstream returned a JSON-RPC error whose message matched the "method not found/disabled" pattern in `architecture/evm/error_normalizer.go:L404-L413`): `HasErrorCode(err, ErrCodeJsonRpcExceptionInternal)` at L1540 returns true and the error is returned as-is, yielding **`-32601` `JsonRpcErrorUnsupportedException`** (the upstream's original code).

   - **First child is `ErrEndpointUnsupported` *not* wrapping a `JsonRpcExceptionInternal`** (e.g. admin handler, gRPC shim `eth_query_shim.go:L481`, or a `grpc_errors.go` path where no upstream JSON-RPC exception was present): same fall-through as the `ErrUpstreamRequestSkipped` case → **`-32603`**.

   **Client impact**: a client that calls a method unsupported by all configured upstreams will receive either `-32601` or `-32603` depending on whether the upstream's original error was normalized via the EVM error-normalizer path. The client cannot reliably distinguish "method globally unsupported" from "internal server error" solely by JSON-RPC code. The `data` field (when `includeErrorDetails` is enabled in server config) carries the full eRPC error chain which does contain `ErrCodeEndpointUnsupported` or `ErrCodeUpstreamRequestSkipped` for programmatic detection.

   **Note on single-upstream simplification** (`erpc/http_server.go:L1399-L1403`): `buildErrorResponseBody` has a special case — if `ErrUpstreamsExhausted` contains exactly one child error, it unwraps and uses that child directly *before* calling `TranslateToJsonRpcException`. This means the all-skipped fast-path only applies when there are ≥2 children.

### Source map

- `erpc/request_processor.go` — `RequestProcessor`: unified unary + gRPC stream entry point; wires project, auth, network resolution.
- `erpc/query_executor.go` — `EvmQueryExecutor`: structured query dispatch; native pipe-through vs. shim routing; block bound resolution; upstream iteration for query streams.
- `erpc/network_executor.go` — `networkExecutor`: failsafe policy orchestration (timeout, consensus, retry, hedge); retry reason classification and delay computation.
- `erpc/query_pipe_through.go` — `pipeThroughQuery*`, `recvProtoStream`: native gRPC BDS stream forwarding; `StreamError` type with `PageEmitted` gate.
- `erpc/query_field_projection.go` — `ProjectBlock/Transaction/Log/Trace/TransferFields`: proto field zeroing for query responses.
- `erpc/query_shim.go` — `shimQuery*`, `fetchBlockViaForward`, `fetchLogsViaForward`, `fetchTracesViaForward`: standard JSON-RPC → structured query translation; block iteration, log pagination, filter matching.
- `erpc/networks.go` — `Network.Forward`: core dispatch; multiplexing; cache read/write; upstream ordering; block availability gating; served-tip computation; all pre/post-forward hooks; normalizeResponse; enrichStatePoller.
- `erpc/projects.go` — `PreparedProject.Forward`: project-level rate limiting, metrics, shadow upstreams, EVM hook wrapper.
- `erpc/http_server.go` — HTTP ingress; batch dispatch; directive enrichment; response header emission (`setResponseHeaders`); error response formatting.
- `common/request.go` — `NormalizedRequest`: directive system, upstream round-robin selection, composite type, finality caching, exec state.
- `common/response.go` — `NormalizedResponse`: lazy JSON parsing, ref-counting, response body lifecycle, ID normalization.
- `architecture/evm/hooks.go` — Four EVM lifecycle hooks (`HandleProjectPreForward`, `HandleNetworkPreForward`, `HandleUpstreamPreForward`, `HandleUpstreamPostForward`) dispatched by method name.
