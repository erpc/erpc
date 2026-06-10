# KB: JSON-RPC batching & in-flight multiplexing

> status: complete
> source-dirs: erpc/http_batch_resp.go, erpc/multiplexer.go, erpc/networks.go, erpc/networks_registry.go, clients/http_json_rpc_client.go, common/config.go, common/defaults.go, common/json_rpc.go, common/response.go, telemetry/metrics.go

## L1 — Capability (CTO view)

eRPC handles JSON-RPC batch requests in two orthogonal ways. On the **inbound side**, a single POST body whose first byte is `[` is unpacked into N sub-requests that are dispatched in parallel goroutines with full per-request error isolation, and results are re-assembled in original order before streaming back to the client. On the **outbound side**, an upstream marked `supportsBatch: true` accumulates individual RPC calls into a timer+size-gated batch that is flushed as a single HTTP POST; the upstream client then fans results back to the individual waiting goroutines by JSON-RPC `id` matching. Independently, the **multiplexer** deduplicates identical in-flight requests at the network layer: the first arrival becomes the "leader" that hits the upstream, while all concurrent duplicates become "followers" that wait on the same in-memory channel and receive a copy of the leader's response — eliminating redundant upstream traffic for hot requests.

## L2 — Mechanics (staff-engineer view)

### Inbound batch handling

Detection is a single byte test: `body[0] == '['` (`erpc/http_server.go:L405`). If true, the body is unmarshalled into `[]json.RawMessage` with `SonicCfg.Unmarshal`. Each element becomes an independent `NormalizedRequest` and is dispatched into its own goroutine with a deferred `recover()` (`erpc/http_server.go:L440-L676`). All goroutines write into a pre-allocated `responses []interface{}` slice indexed by original position, so ordering is preserved by array index rather than by completion order. The main goroutine calls `wg.Wait()` and only then writes the response.

After `wg.Wait()`, if the top-level HTTP context is already cancelled the handler drops all responses and calls `writeFatalError` (`erpc/http_server.go:L683-L699`). Otherwise it always writes HTTP 200 and streams the array through `BatchResponseWriter.WriteTo` — no buffering of the entire response in memory (`erpc/http_batch_resp.go:L21-L79`). Each element is written as it comes: a `*common.NormalizedResponse` calls its own `WriteTo`, a `*HttpJsonRpcErrorResponse` uses a series of `w.Write` calls with zero allocation (`erpc/http_batch_resp.go:L82-L130`), an `error` is converted to a JSON-RPC `-32603` error object without a request ID (`erpc/http_batch_resp.go:L47-L55`), and any unknown type falls back to `SonicCfg.Marshal`.

Partial failures are isolated: one sub-request failing does not fail others. The failing goroutine stores an error response at `responses[index]` and the BatchResponseWriter emits it as a JSON-RPC error element in the array, while other elements carry normal results. There is no "fail the whole batch on first error" behavior.

### Outbound upstream batching

When `upstream.jsonRpc.supportsBatch: true`, `NewGenericHttpJsonRpcClient` initialises `client.supportsBatch = true` and allocates `client.batchRequests map[interface{}]*batchRequest` (`clients/http_json_rpc_client.go:L132-L137`). From that point, `SendRequest` no longer calls `sendSingleRequest`; instead it creates a `batchRequest` struct (with per-request `response` and `err` channels), locks `batchMu`, and calls `queueRequest` (`clients/http_json_rpc_client.go:L162-L209`).

`queueRequest` logic:
1. If the request's context is already cancelled, fail it immediately and do not queue (`clients/http_json_rpc_client.go:L241-L253`).
2. If a request with the same JSON-RPC `id` already exists in the pending map, flush the current batch immediately (to prevent ID collision in response matching), then re-queue the new request recursively (`clients/http_json_rpc_client.go:L255-L262`).
3. Store the request. Track the earliest deadline among all queued requests to create the batch context (`clients/http_json_rpc_client.go:L265-L273`).
4. When the map grows to 1 entry, start the `batchTimer` via `time.AfterFunc(batchMaxWait, ...)` (`clients/http_json_rpc_client.go:L289-L292`).
5. When `len(batchRequests) >= batchMaxSize`, stop the timer and call `processBatch(true)` immediately (`clients/http_json_rpc_client.go:L294-L297`).

`processBatch` collects the current map, drops any already-cancelled requests, creates a batch context with the earliest deadline (`clients/http_json_rpc_client.go:L362-L372`), serialises all requests to a JSON array, and fires a single HTTP POST (`clients/http_json_rpc_client.go:L406-L444`). On network error, all requests receive an error with a 5 ms grace window to let per-request failsafe sentinels (`ErrDynamicTimeoutExceeded`) become observable before error classification (`clients/http_json_rpc_client.go:L470-L476`). On success, `processBatchResponse` parses the upstream body.

`processBatchResponse` handles three shapes:
- **JSON array** (`V_ARRAY`): iterates elements, extracts `id`, matches to `batchRequests[id]`, delivers via `req.response` channel or cancels if request context is done; any IDs without a match get `"no response received for request ID: %d"` (`clients/http_json_rpc_client.go:L555-L605`).
- **JSON object** (`V_OBJECT`): upstream returned a single error object for the whole batch (e.g. rate-limit response); all queued requests receive the same error (`clients/http_json_rpc_client.go:L606-L636`).
- **Non-JSON / other type**: all queued requests receive `ErrUpstreamMalformedResponse` (`clients/http_json_rpc_client.go:L632-L636`).

### In-flight multiplexer

The multiplexer sits at `Network.Forward`, checked immediately after static-response short-circuit and before cache and upstream selection (`erpc/networks.go:L983-L1001`).

**Enabling condition**: `NetworkConfig.MultiplexingEnabled()` returns `true` when `cfg.Multiplexing == nil` (default) or `*cfg.Multiplexing == true` (`common/config.go:L2034-L2039`). There is no way to opt in at the request level; it is strictly a per-network toggle.

**Hash function**: `req.CacheHash()` → `JsonRpcRequest.CacheHash()` which hashes `method + SHA256(params)` into `"method:<hex>"` using `sha256.New()` + `hashValue()` per param; the result is cached atomically in `JsonRpcRequest.cacheHash` (`common/json_rpc.go:L1385-L1409`). The JSON-RPC request `id` is **not** part of the hash, so two requests with identical method+params but different IDs will be multiplexed. If `CacheHash` returns `""` or an error (e.g. unhashable param type), multiplexing is skipped for that request silently (`erpc/networks.go:L1996-L1999`).

**Leader / follower election**: `n.inFlightRequests.LoadOrStore(mlxHash, mlx)` atomically tries to insert a new `Multiplexer` keyed by hash (`erpc/networks.go:L2005`). If not loaded → this goroutine is the **leader** and receives the `*Multiplexer` back; it proceeds to cache lookup and upstream dispatch, then calls `mlx.Close(ctx, resp, err)` followed by `cleanupMultiplexer(mlx)` via `defer` (`erpc/networks.go:L1000`). If loaded → another request is already in flight; this goroutine becomes a **follower**.

**Follower registration**: the follower acquires `inf.mu.Lock()` and checks `inf.closed`. If `closed == true` cleanup has already started; the follower retries the whole `LoadOrStore` loop (`erpc/networks.go:L2016-L2023`). Otherwise it calls `inf.copyWg.Add(1)` under the lock, releases it, increments `erpc_network_multiplexed_request_total`, and calls `waitForMultiplexResult` (`erpc/networks.go:L2025-L2040`).

**Waiting**: `waitForMultiplexResult` selects on `mlx.done` (leader closed) or `ctx.Done()` (follower timeout). On `mlx.done`, it acquires `mlx.mu.RLock()` and calls `CopyResponseForRequest(ctx, mlx.resp, req)` which clones the parsed `JsonRpcResponse` and rewrites its `id` to the follower's request `id` (`common/response.go:L583-L646`). This ensures response ID fidelity even when the leader and follower sent different IDs (`erpc/networks.go:L2063-L2085`).

**Cleanup**: the leader's `defer cleanupMultiplexer(mlx)` marks `mlx.closed = true` under lock (blocks new followers), deletes the hash from `inFlightRequests`, then calls `mlx.copyWg.Wait()` to block until all active followers finish copying, and finally calls `mlx.resp.Release()` (`erpc/networks.go:L2088-L2110`). This ordering prevents use-after-free: followers always see a valid response pointer.

**Close semantics**: `Multiplexer.Close` uses `sync.Once` to ensure exactly one write. Inside `once.Do` it: acquires `mu.Lock()`, defers `close(m.done)` (always fires even on panic), calls `resp.JsonRpcResponse()` to parse, calls `jrr.Clone()` to produce a shareable copy, and stores `resp` + `err` in the struct (`erpc/multiplexer.go:L36-L88`). A panic inside is recovered and converted to a non-nil `m.err` so followers still unblock.

## L3 — Exhaustive reference (agent view)

### Config fields

#### Inbound batch (no configuration required)

Inbound batch handling is automatic and unconditional — any POST body whose first byte is `[` is treated as a batch (`erpc/http_server.go:L405`). There are no config knobs to enable, disable, or size-limit inbound batches.

#### Outbound upstream batching (`upstream.jsonRpc.*`)

| # | YAML path | Type | Default | Behavior / notes |
|---|-----------|------|---------|------------------|
| 1 | `upstream.jsonRpc.supportsBatch` | `*bool` | `nil` (batch **disabled**) — `SetDefaults()` for `JsonRpcUpstreamConfig` is a no-op (`common/defaults.go:L1777-L1779`); the field is only set when explicitly provided or when upstream-level `JsonRpc` is inherited from `networkDefaults.upstreams[].jsonRpc` (`common/defaults.go:L1550-L1556`) | When `true`, all requests through this upstream client are queued and dispatched as batches. When `nil` or `false`, every request uses `sendSingleRequest`. No other fields in `jsonRpc` are read unless `supportsBatch` is `true`. |
| 2 | `upstream.jsonRpc.batchMaxSize` | `int` | `0` (zero value, meaning no practical size limit but must be set explicitly; `SetDefaults()` is a no-op) | Maximum number of requests accumulated before flushing a batch. When `len(batchRequests) >= batchMaxSize` the batch fires immediately regardless of timer. A value of 0 means the batch will only fire on timer expiry. See `clients/http_json_rpc_client.go:L294`. |
| 3 | `upstream.jsonRpc.batchMaxWait` | `Duration` | `0` (zero value; `SetDefaults()` is a no-op) | Time to wait from the first request before flushing a partially-filled batch. Uses `time.AfterFunc`. Zero means the timer fires immediately (next event loop tick) — effectively disabling time-based accumulation unless a non-zero value is provided. See `clients/http_json_rpc_client.go:L291`. |

#### Multiplexer (`network.multiplexing`)

| # | YAML path | Type | Default | Behavior / notes |
|---|-----------|------|---------|------------------|
| 4 | `network.multiplexing` | `*bool` | `nil` → **enabled** (`true`) | **`nil` means multiplexing is ON.** `MultiplexingEnabled()` (`common/config.go:L2034-L2039`) returns `true` when `n == nil || n.Multiplexing == nil`; only returns `false` when the field is explicitly set to `false`. There is **no opt-in required** — omitting the field activates the multiplexer. To disable: `multiplexing: false`. Also available as `networkDefaults.multiplexing` (`common/config.go:L599`), which propagates to all per-network configs that do not set it explicitly (`common/defaults.go:L1841-L1843`). There is no project-level or global multiplexing toggle; the knob is strictly per-network (or via `networkDefaults`). |

> **nil = enabled — key semantics**: Because `*bool` uses a pointer, the zero value (`nil`) is distinct from `false`. The `MultiplexingEnabled()` guard treats `nil` as `true`, so a network block without any `multiplexing:` key gets multiplexing automatically. This is the primary default path for every eRPC deployment. Set `multiplexing: false` explicitly only when deduplication is undesirable (e.g., non-idempotent methods that eRPC cannot detect, or during debugging to trace individual upstream calls).

### Behaviors & algorithms

#### Inbound batch request flow

1. HTTP handler receives a POST body. First byte checked: `body[0] == '['` (`erpc/http_server.go:L405`).
2. If batch, body is unmarshalled as `[]json.RawMessage` via `SonicCfg.Unmarshal` (`erpc/http_server.go:L409`). Unmarshal failure → `ErrJsonRpcRequestUnmarshal` error with HTTP 400.
3. `responses := make([]interface{}, len(requests))` preserves positional ordering.
4. One goroutine per element. Each goroutine:
   - Parses and validates the sub-request (`nq.Validate()`).
   - Resolves client IP, runs forward-header matching, applies ignore/allow method filters.
   - Runs auth.
   - Resolves network from URL path or from `networkId` field in request body.
   - Applies directive defaults, enriches from headers/query params.
   - Calls `project.Forward(requestCtx, networkId, nq)`.
   - Stores `*NormalizedResponse` or `*HttpJsonRpcErrorResponse` at `responses[index]`.
   - Has a deferred `recover()` — panics turn into `-32603` error objects stored in the slot.
5. `wg.Wait()`.
6. If HTTP context already cancelled: drop all responses, `writeFatalError`, return.
7. `w.WriteHeader(http.StatusOK)` (always 200 for batches).
8. `BatchResponseWriter.WriteTo(w)` streams `[elem,elem,...]` directly to the response writer.

#### Inbound batch response shape

- HTTP status: always **200 OK** for a batch POST, regardless of individual sub-errors.
- Content-Type: `application/json` (set earlier in handler chain).
- Body: a JSON array of JSON-RPC response objects in the original request order.
- An element may be a success `{"jsonrpc":"2.0","id":...,"result":...}` or error `{"jsonrpc":"2.0","id":...,"error":{...}}`.
- If a goroutine panicked, its slot becomes `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"unexpected server panic..."}}` (no `id` because the request ID cannot be reliably determined at panic time — `erpc/http_batch_resp.go:L47-L55`).

#### Outbound batch accumulation and dispatch

1. `SendRequest` → `queueRequest(jrReq.ID, bReq)` under `batchMu`.
2. Duplicate-ID guard: if `batchRequests[id]` already exists, flush current batch first (recursive call after flush — `clients/http_json_rpc_client.go:L255-L262`).
3. Earliest deadline tracking across queued requests.
4. First-request timer: `time.AfterFunc(batchMaxWait, processBatch)`.
5. Size-limit flush: when `len >= batchMaxSize`.
6. `processBatch` drains cancelled requests before going to network.
7. Batch HTTP body: `[]common.JsonRpcRequest` marshalled by `SonicCfg.Marshal` — a flat JSON array of objects.
8. Response parsed using `sonic/ast.NewSearcher` for zero-copy JSON traversal.
9. ID matching: `batchRequests[jrResp.ID()]` → delivers to `req.response` channel.
10. Any request ID not found in the response → `req.err <- "no response received for request ID: %d"` (`clients/http_json_rpc_client.go:L600`).
11. App context cancelled during shutdown → `processBatch` drains silently without signalling pending requests (intentional: supervisor is shutting down — `clients/http_json_rpc_client.go:L305-L330`).

#### Multiplexer hash and dedup scope

- Hash: `"<method>:<sha256_hex_of_params>"` computed over `JsonRpcRequest.Params` using `hashValue()` which handles `bool`, `int`, `float64`, `string`, `[]interface{}`, and `map[string]interface{}` recursively (`common/json_rpc.go:L1453-L1500`).
- The JSON-RPC `id` field is **excluded** from the hash. Two requests differing only in `id` are treated as duplicates.
- The hash is computed at most once per request and cached in an atomic.Value on `JsonRpcRequest.cacheHash` (`common/json_rpc.go:L1394-L1407`).
- Dedup scope: per `Network` instance (one `sync.Map` per network, initialized in `NewNetwork` — `erpc/networks_registry.go:L164`).
- Cache reads via `n.cacheDal.Get` happen **after** multiplexer leader/follower election (`erpc/networks.go:L1003`), so a cache hit closes the multiplexer and all followers benefit from the cached response.

#### Response ID rewriting for followers

`CopyResponseForRequest` clones the parsed `JsonRpcResponse` from `mlx.resp`, then calls `jrr.SetID(jrq.ID)` where `jrq.ID` is the **follower's** original request ID (`common/response.go:L641`). This means:
- If leader sent `id: 1` and follower sent `id: 99`, the follower receives a response with `id: 99`.
- The upstream response carries the leader's `id` but followers never see it.
- Metadata fields (`upstream`, `fromCache`, `attempts`, `retries`, `hedges`, `evmBlockRef`, `evmBlockNumber`) are copied verbatim.

### Observability

#### Prometheus metrics

| Metric | Type | Labels | When it fires |
|--------|------|--------|---------------|
| `erpc_network_multiplexed_request_total` | counter | `project`, `network`, `category`, `finality`, `user`, `agent_name` | Incremented once per follower registration, i.e. each time a request is de-duplicated into an in-flight identical request (`erpc/networks.go:L2030`, `telemetry/metrics.go:L413-L417`). The leader itself is **not** counted here. |

No dedicated batch metrics exist; batch dispatch is transparent to the existing upstream-level counters (`erpc_upstream_request_duration_histogram`, etc.).

#### Trace spans

| Span name | Type | Attributes |
|-----------|------|------------|
| `Multiplexer.Close` | detail | `multiplexer.hash` — emitted inside `sync.Once`, only fires on the leader's goroutine (`erpc/multiplexer.go:L37-L39`) |
| `Network.WaitForMultiplexResult` | normal | `multiplexer.hash` — emitted for every follower, spans the entire wait time from follower registration to response copy (`erpc/networks.go:L2058-L2060`) |

The leader's `Forward` span additionally receives attributes `multiplexer.hash` and `multiplexer.role = "leader"` (`erpc/networks.go:L996-L998`); follower `Forward` spans receive `multiplexed = true` and `multiplexer.role = "follower"` (`erpc/networks.go:L986-L989`).

#### Log messages

| Level | Message pattern | Context |
|-------|----------------|---------|
| `TRACE` | `"checking if multiplexing is possible"` | Every request when multiplexing is enabled |
| `DEBUG` | `"could not get multiplexing hash for request"` | When `CacheHash()` returns error or empty |
| `DEBUG` | `"found identical request initiating multiplexer"` | Follower successfully registered |
| `TRACE` | `"multiplexed request result"` | After follower receives result |
| `WARN` | `"multiplexer follower got nil response and no error, retrying"` | Unexpected state, follower retries |
| `DEBUG` | `"queuing request %+v for batch (current batch size: %d)"` | Each request added to outbound batch |
| `DEBUG` | `"processing batch with %d requests"` | Each outbound batch flush |
| `TRACE` | `"starting batch timer"` | First request in an empty batch |
| `TRACE` | `"committing batch to process total of %d requests"` | Size-limit flush |
| `ERROR` | `"some requests did not receive a response (matching ID)"` | Upstream returned fewer responses than sent in batch (`clients/http_json_rpc_client.go:L603-L605`) |
| `WARN` | `"unexpected response received without ID"` | Element in upstream batch array has null/missing `id` |
| `WARN` | `"unexpected response received with ID: %s"` | Element `id` not found in pending map |

### Edge cases & gotchas

1. **Inbound batch, empty array `[]`**: `len(requests) == 0`, `responses` slice has zero elements, and `BatchResponseWriter` writes `[]`. No error is returned to the client. (`erpc/http_server.go:L429-L431`, `erpc/http_batch_resp.go:L21-L79`).

2. **Inbound batch always returns HTTP 200**: Even if every sub-request fails, the HTTP status is 200. Callers must inspect the JSON body for `"error"` fields — `erpc/http_server.go:L703-L707`.

3. **Outbound batch: zero `batchMaxSize` means no size cap**: If `batchMaxSize == 0` the condition `len(batchRequests) >= batchMaxSize` is always true, triggering an immediate flush on every queued request. This is functionally equivalent to `supportsBatch: false` unless `batchMaxWait` is also set. Always set both fields together — `clients/http_json_rpc_client.go:L294`.

4. **Outbound batch: duplicate JSON-RPC IDs trigger immediate flush**: If two concurrent requests carry the same `id` value, the second triggers a premature flush of the current batch to avoid ID collision in response matching. This can cause smaller-than-optimal batches under workloads with repetitive IDs — `clients/http_json_rpc_client.go:L255-L262`.

5. **Outbound batch: partial upstream response is an error for the missing items**: If the upstream returns an array shorter than the number of batched requests, the missing requests receive `"no response received for request ID"` errors. The other requests succeed normally — `clients/http_json_rpc_client.go:L597-L605`, test: `clients/http_json_json_client_test.go:L333-L377`.

6. **Outbound batch: upstream returns a single JSON object**: Interpreted as a broadcast error applying to all batched requests. All receive the same error — `clients/http_json_rpc_client.go:L606-L636`, test: `clients/http_json_json_client_test.go:L218-L257`.

7. **Outbound batch: non-JSON upstream body**: All batched requests receive `ErrUpstreamMalformedResponse` — test: `clients/http_json_json_client_test.go:L296-L331`.

8. **Multiplexer: followers do NOT bypass cache**: The leader path checks the cache; if it finds a hit, it calls `mlx.Close(ctx, cachedResp, nil)`. All followers then receive the cached response via `CopyResponseForRequest`. Followers cannot independently force a cache-miss — `erpc/networks.go:L1003-L1019`.

9. **Multiplexer: `skipCacheRead` directive on follower has no effect**: The directive is evaluated on the leader's path only. A follower that sends `X-ERPC-Skip-Cache-Read: true` will still receive the leader's (possibly cached) result — `erpc/networks.go:L983-L1001`.

10. **Multiplexer: closed state race handled by retry loop**: If a follower finds `inf.closed == true` (leader has started cleanup), it cannot register. It retries `LoadOrStore` in a loop; the old entry will be deleted by `cleanupMultiplexer` shortly, allowing the retrying goroutine to become the new leader — `erpc/networks.go:L2016-L2023`, test: `erpc/networks_multiplexer_test.go:L198-L275`.

11. **Multiplexer: `copyWg.Wait()` prevents use-after-free**: `cleanupMultiplexer` calls `mlx.copyWg.Wait()` after marking `closed = true`, ensuring all followers have finished copying before `mlx.resp.Release()` is called. `closed = true` prevents any new `copyWg.Add(1)` calls, making `Wait()` safe — `erpc/networks.go:L2088-L2110`.

12. **Multiplexer: response `id` is always rewritten**: `CopyResponseForRequest` always overwrites the cloned response's `id` with the follower's request `id`. If the upstream used a normalised/rewritten ID (e.g. via a proxy that renumbers IDs), followers still get their original `id` back — `common/response.go:L641`.

13. **Multiplexer disabled globally by default does NOT happen**: `MultiplexingEnabled()` returns `true` when `Multiplexing == nil`. It must be explicitly set to `false` to disable. There is no project-level or global override; it is per-network — `common/config.go:L2034-L2038`.

14. **Inbound batch: panic in one goroutine does not abort others**: Each goroutine has its own `recover()`. A panic stores a `-32603` response at that slot and calls `wg.Done()`; other goroutines are unaffected — `erpc/http_server.go:L443-L458`.

15. **Outbound batch context uses earliest requester deadline**: The batch HTTP request context uses `context.WithDeadline(appCtx, earliestDeadline)`. This means a short-deadline request in a batch can cut off other requests that had longer deadlines. If that happens, the batch fails and all requests receive `ErrEndpointRequestTimeout` — `clients/http_json_rpc_client.go:L362-L372`.

16. **Multiplexer: stress test verifies 1 upstream request per herd**: `TestNetwork_Multiplexer_FollowersReceiveResponse` verifies with 10, 50, and 3-wave×10 concurrent requests that exactly 1 upstream call is made per cohort and all goroutines receive the correct result — `erpc/networks_multiplexer_test.go:L34-L363`.

17. **`ErrResponseWriteLock` is dead code (vestigial, never fired)**: `ErrResponseWriteLock` (`common/errors.go:L1464-L1476`) and its constructor `NewErrResponseWriteLock(writerId string)` exist in the codebase but are **never called**. The type was introduced in commit `6b4679ac` (May 2024) when the early hedge implementation used a shared `http.ResponseWriter` guarded by a `sync.Mutex`; if two goroutines (e.g., a hedge winner and the original attempt) raced to write the response, the loser returned this error. The response model was refactored in commit `831c207f` (June 2024) to use `NormalizedResponse` (a value-typed, goroutine-safe struct), eliminating the shared-writer race entirely. The error type was left unreferenced in `errors.go`. **No running eRPC instance will ever produce an `ErrResponseWriteLock` error.** The contemporary mechanism by which hedge losers are discarded is `ErrUpstreamHedgeCancelled` (`common/errors.go:L1449`) — the failsafe hedge cancels losing goroutines via context cancellation; they return `ErrUpstreamHedgeCancelled` which is then surfaced only as a trace attribute, never propagated to the caller. For multiplexing, follower goroutines do not race to write at all; only the leader writes, and followers receive a cloned copy via `CopyResponseForRequest` — `erpc/networks.go:L2063-L2085`, `erpc/multiplexer.go:L36-L88`.

### Source map

- `erpc/http_batch_resp.go` — `BatchResponseWriter` and `writeJsonRpcError`: streaming inbound batch response serialization with zero full-body buffering.
- `erpc/multiplexer.go` — `Multiplexer` struct: leader/follower state machine, `Close()` (once-guarded response storage + channel close), OTel span.
- `erpc/networks.go:L1989-L2110` — `handleMultiplexing`, `waitForMultiplexResult`, `cleanupMultiplexer`: hash-based leader election via `sync.Map.LoadOrStore`, follower registration with `copyWg`, cleanup ordering.
- `erpc/networks_registry.go:L83-L167` — `NewNetwork`: allocates `inFlightRequests *sync.Map`.
- `erpc/http_server.go:L398-L746` — inbound batch detection (`body[0]=='['`), parallel goroutine dispatch, `wg.Wait()`, `BatchResponseWriter` invocation.
- `clients/http_json_rpc_client.go:L29-L637` — `GenericHttpJsonRpcClient`: outbound batch queue, timer, size-flush, `processBatch`, `processBatchResponse`, ID matching, partial/malformed response handling.
- `common/config.go:L1072-L1094` — `JsonRpcUpstreamConfig` with `SupportsBatch`, `BatchMaxSize`, `BatchMaxWait`.
- `common/config.go:L1995-L2039` — `NetworkConfig.Multiplexing` field and `MultiplexingEnabled()`.
- `common/config.go:L593-L600` — `NetworkDefaults.Multiplexing` for project-wide default.
- `common/defaults.go:L1777-L1779` — `JsonRpcUpstreamConfig.SetDefaults()` is a no-op (batch fields have no system defaults).
- `common/defaults.go:L1841-L1843` — propagates `NetworkDefaults.Multiplexing` to per-network configs.
- `common/json_rpc.go:L1385-L1409` — `JsonRpcRequest.CacheHash()`: `method:sha256(params)` with atomic caching.
- `common/response.go:L581-L647` — `CopyResponseForRequest`: deep-clone of response with follower `id` overwrite.
- `telemetry/metrics.go:L413-L417` — `MetricNetworkMultiplexedRequests` counter.
- `erpc/networks_multiplexer_test.go` — concurrent follower, stress, multi-wave, slow-upstream tests.
- `clients/http_json_json_client_test.go:L105-L421` — outbound batch race condition, timeout, partial response, single-object error, non-JSON response tests.
- `erpc/networks_client_test.go:L130-L322` — `TestNetwork_BatchRequests`: simple batch, duplicate IDs, single-object upstream error.
