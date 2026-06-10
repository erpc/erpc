# KB: Upstream HTTP client & proxy pools

> status: complete
> source-dirs: clients/http_json_rpc_client.go, clients/registry.go, clients/proxy_pool_registry.go, common/config.go, common/defaults.go, common/validation.go, util/http_dialer.go, common/error_extractor.go, common/errors.go, architecture/evm/extractor.go, upstream/registry.go

## L1 — Capability (CTO view)

Every EVM upstream with an `http://` or `https://` endpoint is served by a single `GenericHttpJsonRpcClient`. The client owns a pre-warmed `http.Transport` with generous per-host connection pooling (up to 256 idle, unlimited active), kernel-level TCP keepalive at 15-second intervals, and full gzip support in both directions. Optionally, a `ProxyPool` can be bound to the upstream; each pool holds one `http.Client` per proxy URL and rotates them round-robin on every call. This allows outbound traffic to be tunneled through SOCKS5/HTTP proxies while still benefiting from the same transport settings.

## L2 — Mechanics (staff-engineer view)

**Client lifecycle.** `ClientRegistry.GetOrCreateClient` (`clients/registry.go:L47-L53`) uses a `sync.Map` keyed on `common.UniqueUpstreamKey(ups)`. The first call creates the client; all subsequent calls return the cached instance. Creation is guarded by a `sync.Once` (`clients/registry.go:L56-L125`). The registry also holds a reference to a `ProxyPoolRegistry`, resolving the `ProxyPool` by ID at creation time (`clients/registry.go:L72-L77`).

**Scheme dispatch.** `CreateClient` inspects `parsedUrl.Scheme`. `http`/`https` → `NewGenericHttpJsonRpcClient`. `ws`/`wss` → error (not yet implemented). `grpc`/`grpc+bds` → `NewGrpcBdsClient`. Any other scheme → error (`clients/registry.go:L84-L117`).

**Transport construction.** Each `GenericHttpJsonRpcClient` owns its own `http.Transport` built in `NewGenericHttpJsonRpcClient`:

- `DialContext` is provided by `util.DefaultOutboundDialer()`, a `net.Dialer{Timeout: 10s, KeepAlive: 15s}`. The 15-second probe interval means a wedged TCP flow is killed within ~45 seconds (3 missed probes), versus the Linux default `tcp_keepalive_time` of 7200 seconds (`util/http_dialer.go:L8-L24`).
- `MaxIdleConns: 1024`, `MaxIdleConnsPerHost: 256`, `MaxConnsPerHost: 0` (unlimited active), `IdleConnTimeout: 90s`, `ResponseHeaderTimeout: 30s`, `TLSHandshakeTimeout: 10s`, `ExpectContinueTimeout: 1s` (`clients/http_json_rpc_client.go:L109-L118`).
- The overall client timeout is 60 seconds (`clients/http_json_rpc_client.go:L125-L128`).
- `MaxConnsPerHost: 0` (unlimited) is intentional: the pre-fix default of 2 caused severe connection churn under 100 RPS with 400ms upstream latency (verified in `clients/http_connection_test.go:L141-L414`).

**Under test.** When `util.IsTest()` is true the client falls back to `http.DefaultTransport` instead (`clients/http_json_rpc_client.go:L120-L123`). This allows mock interceptors (gock) to work without touching the production transport.

**Proxy pool construction.** `createProxyPool` iterates `poolCfg.Urls` and builds one `*http.Client` per URL. Each client gets its own `http.Transport` with the same parameters as the direct transport plus `Proxy: http.ProxyURL(proxyURL)` (`clients/proxy_pool_registry.go:L82-L98`). The pool wraps those clients in a `ProxyPool` struct with an atomic `counter` for round-robin selection.

**Proxy selection per request.** `getHttpClient()` checks `c.proxyPool != nil`; if so it calls `ProxyPool.GetClient()`, which does `atomic.AddUint64(&p.counter, 1) % uint64(len(p.clients))` — a lock-free round-robin (`clients/proxy_pool_registry.go:L23-L29`). On error (empty pool), falls back to `c.httpClient` (the direct transport). The proxy being used is logged at trace level with the pool ID, transport pointer, and resolved proxy URL (`clients/http_json_rpc_client.go:L219-L234`).

**Request preparation.** `prepareRequest` builds an `http.Request` with:
1. Body as `bytes.NewReader(body)`, or a pooled gzip-compressed buffer when `enableGzip == true`.
2. Fixed headers: `Accept-Encoding: gzip`, `Content-Type: application/json`, `User-Agent: erpc (<version>/<sha>; Project/<projectId>)`.
3. If gzip-compressed: `Content-Encoding: gzip` added.
4. Custom upstream headers from `jsonRpcCfg.Headers` (applied via `Header.Set`, overwriting defaults for matching keys).
5. OTel W3C trace context injected via `propagation.NewCompositeTextMapPropagator(TraceContext{}, Baggage{})` → `HeaderCarrier` (`clients/http_json_rpc_client.go:L786-L849`).

**Forward headers (single-request only).** In `sendSingleRequest`, inbound headers that matched the project's `forwardHeaders` patterns are added to the upstream request via `req.ForwardHeaders` → `httpReq.Header.Add` (`clients/http_json_rpc_client.go:L736-L742`). This path is absent from batch sends (the batch share a single outbound call and have no per-request inbound header context).

**Gzip pools.** Both reading and writing reuse pooled objects: `util.GzipReaderPool` (sync.Pool of `*gzip.Reader`, reset on Get) and `util.GzipWriterPool` (sync.Pool of `*gzip.Writer`). Gzip reader is also pooled in `sendSingleRequest` via `WrapGzipReader` which returns the reader to the pool on `Close`. For batches, `readResponseBody` uses `gzipPool.GetReset`/`Put` with defer (`clients/http_json_rpc_client.go:L852-L867`).

**Batch aggregation.** When `supportsBatch` is true, `SendRequest` queues the request into `batchRequests` (map by JSON-RPC ID) rather than sending immediately. A `time.AfterFunc` fires `processBatch(false)` after `batchMaxWait`; `processBatch(true)` fires immediately when `len(batchRequests) >= batchMaxSize`. The batch context deadline is set to the earliest deadline among queued requests. The batch response is dispatched back to each caller via individual channels (`response` or `err`). If the upstream returns a JSON array the responses are matched back by ID; if it returns a single object it is fan-fanned to all queued callers; if it returns a non-JSON body it is attempted to be parsed as a JSON-RPC error and broadcast to all callers.

**Duplicate-ID handling.** If a second request with the same JSON-RPC ID arrives before the current batch fires, `queueRequest` stops the timer, processes the in-flight batch immediately, then recurses to queue the new request in the fresh batch — guaranteeing ID uniqueness within any single batch (`clients/http_json_rpc_client.go:L255-L261`).

**Error normalization.** `normalizeJsonRpcError` first reclassifies any JSON-RPC error whose message contains `"context canceled"` or `"context deadline exceeded"` as `ErrEndpointRequestCanceled`/`ErrEndpointRequestTimeout` respectively (guards against a context cancellation during body parsing surfacing as a server error). Then the architecture-specific `errorExtractor.Extract` runs. Any remaining `jr.Error != nil` is wrapped as `ErrJsonRpcExceptionInternal/ServerSideException` (`clients/http_json_rpc_client.go:L869-L951`).

**Shutdown.** A goroutine watches `appCtx.Done()` and calls `shutdown()` which drains the in-flight batch: acquires the mutex, stops the timer, calls `processBatch(true)`. Canceled app context drops all remaining queued requests silently (`clients/http_json_rpc_client.go:L211-L217`, `clients/http_json_rpc_client.go:L304-L330`).

**TLS to upstreams.** The `http.Transport` uses Go's default TLS configuration: system CA bundle via `tls.Config{}` default. There is no per-upstream `tlsClientConfig`; custom CA / client certs / `InsecureSkipVerify` are not supported for HTTP upstreams (as of current codebase). Proxy transports are identical in this respect. `TLSHandshakeTimeout` is 10 seconds for both direct and proxy transports.

**HTTP/2.** Neither the direct transport nor proxy transports set `ForceAttemptHTTP2` or configure `TLSNextProto`. HTTP/2 is therefore not explicitly enabled for upstream connections. Go's `net/http` will negotiate HTTP/2 via ALPN on HTTPS connections only if the server offers it, following standard Go behavior, but no explicit HTTP/2 support is configured or tested for upstream connections.

## L3 — Exhaustive reference (agent view)

### Config fields

#### `proxyPools[]` (top-level, `common/config.go:L48`)

This is a top-level array in the root `Config` struct. Each entry defines one named pool.

| Field | Type | Default | Behavior |
|-------|------|---------|----------|
| `proxyPools[].id` | `string` | required | Pool name referenced by `upstream.jsonRpc.proxyPool`. Used as map key in `ProxyPoolRegistry` (`clients/proxy_pool_registry.go:L58`). |
| `proxyPools[].urls` | `[]string` | required, min 1 | List of proxy URLs. Must contain at least one entry; an empty list fails with `"proxyPool.*.urls is required under proxyPool.*.id '<id>', add at least one URL"`. Each URL is validated by `ProxyPoolConfig.Validate()` (`common/validation.go:L230-L247`): the URL string is lowercased with `strings.ToLower` and then checked via `strings.HasPrefix` against exactly three allowed prefixes: `http://`, `https://`, or `socks5://`. This is a **case-insensitive prefix string check** — not `url.Parse` scheme extraction — so any URL that does not start with one of those three prefixes (regardless of case) fails at startup with `"proxyPool.*.urls under proxyPool.*.id '<id>' must be valid HTTP, HTTPS, or SOCKS5 URLs, got: <url>"`. No other schemes are accepted. Each valid URL gets its own `http.Client` with a dedicated transport (`clients/proxy_pool_registry.go:L82-L98`). |

#### `upstreams[].jsonRpc.*` (and `upstreamDefaults.jsonRpc.*`)

Nested under each upstream's `jsonRpc:` block. All fields are optional; `JsonRpcUpstreamConfig.SetDefaults()` is a no-op (`common/defaults.go:L1777-L1779`). When an upstream has no `jsonRpc:` block but `upstreamDefaults.jsonRpc` is set, the entire defaults block is shallow-copied to the upstream (`common/defaults.go:L1550-L1558`).

| Field | YAML path | Type | Default | Behavior / source citation |
|-------|-----------|------|---------|----------------------------|
| `supportsBatch` | `upstreams[].jsonRpc.supportsBatch` | `*bool` | `nil` (false) | Enables batch aggregation. When `nil` or `false`, every `SendRequest` is a direct single HTTP call. When `true`, requests are queued and coalesced (`clients/http_json_rpc_client.go:L132-L137`). |
| `batchMaxSize` | `upstreams[].jsonRpc.batchMaxSize` | `int` | `0` (no-op when batch disabled) | Maximum number of requests per batch. When the queue reaches this size the batch fires immediately, bypassing `batchMaxWait` (`clients/http_json_rpc_client.go:L294-L300`). A value of 0 with `supportsBatch=true` means the batch never fires by size alone — only by timer. |
| `batchMaxWait` | `upstreams[].jsonRpc.batchMaxWait` | `Duration` | zero (instant flush after first queued request, i.e. timer fires with 0 delay) | How long to wait for the batch to fill before flushing. A `time.AfterFunc(batchMaxWait, ...)` is started when the first request arrives (`clients/http_json_rpc_client.go:L289-L291`). |
| `enableGzip` | `upstreams[].jsonRpc.enableGzip` | `*bool` | `nil` (gzip disabled) | When `true`, compresses the outbound request body using a pooled `gzip.Writer` and sets `Content-Encoding: gzip`. Regardless of this flag, the client always sends `Accept-Encoding: gzip` and decompresses gzip responses (`clients/http_json_rpc_client.go:L139-L142`, `clients/http_json_rpc_client.go:L828-L835`). Note: `server.enableGzip` (default `true` at `common/defaults.go:L706-L707`) controls response compression to callers; this flag is independent and controls request compression to upstreams. |
| `headers` | `upstreams[].jsonRpc.headers` | `map[string]string` | `nil` | Static headers sent on every outbound request. Applied via `Header.Set` (overwrites). Set after the fixed eRPC defaults (`Content-Type`, `User-Agent`, `Accept-Encoding`) and before OTel injection (`clients/http_json_rpc_client.go:L838-L840`). Useful for API keys: `Authorization: Bearer <token>`. Verified in test `TestHttpJsonRpcClient_BatchRequests/CustomHeaders` (`clients/http_json_json_client_test.go:L422-L489`). |
| `proxyPool` | `upstreams[].jsonRpc.proxyPool` | `string` | `""` (no proxy) | Reference to a `proxyPools[].id`. At client creation time, `ClientRegistry.CreateClient` calls `ProxyPoolRegistry.GetPool(cfg.JsonRpc.ProxyPool)` and passes the resulting `*ProxyPool` to `NewGenericHttpJsonRpcClient`. Empty string → `GetPool` returns `nil, nil` (direct) (`clients/proxy_pool_registry.go:L107-L111`). Non-existent ID → error at startup (`clients/registry.go:L72-L76`). |

#### Fixed transport parameters (not user-configurable, for reference)

These are hardcoded in `clients/http_json_rpc_client.go:L109-L118` (direct transport) and `clients/proxy_pool_registry.go:L82-L97` (proxy transports). Both are identical.

| Parameter | Value | Purpose |
|-----------|-------|---------|
| `DialContext` | `util.DefaultOutboundDialer().DialContext` — `net.Dialer{Timeout: 10s, KeepAlive: 15s}` | Dial timeout + TCP keepalive |
| `MaxIdleConns` | `1024` | Global idle connection pool |
| `MaxIdleConnsPerHost` | `256` | Per-host idle connection limit |
| `MaxConnsPerHost` | `0` (unlimited) | Prevents connection queuing under high RPS / high latency |
| `IdleConnTimeout` | `90s` | Time before an idle connection is closed |
| `ResponseHeaderTimeout` | `30s` | Time to wait for first response header byte |
| `TLSHandshakeTimeout` | `10s` | TLS negotiation deadline |
| `ExpectContinueTimeout` | `1s` | 100-continue wait |
| `http.Client.Timeout` | `60s` | End-to-end call timeout (includes body read) |

### Behaviors & algorithms

**Outbound HTTP request shape (single).**
```
POST <upstream endpoint URL>
Content-Type: application/json
Accept-Encoding: gzip
User-Agent: erpc (<ErpcVersion>/<ErpcCommitSha>; Project/<projectId>)
[Content-Encoding: gzip]          — only when enableGzip=true
[<custom headers from jsonRpc.headers>]
[traceparent: ...]                — OTel W3C trace context
[baggage: ...]                    — OTel baggage
[<forwarded headers>]             — only on single requests from project.forwardHeaders matches

Body: {"jsonrpc":"2.0","method":"...","params":[...],"id":...}
```

**Outbound HTTP request shape (batch).**
Same headers except forwarded headers (absent). Body is a JSON array of `JsonRpcRequest` objects. All requests in the batch are serialized holding `RLock` on each request and then all are unlocked after `SonicCfg.Marshal` (`clients/http_json_rpc_client.go:L396-L413`).

**Round-robin proxy selection.**
`atomic.AddUint64(&p.counter, 1)` on every call, modulo pool size. The counter starts at 0 and the first call yields index 1 % len (so with a single proxy, always index 0). Counter is never reset; it wraps naturally at `uint64` overflow (after ~1.8 × 10^19 calls).

**Batch deadline propagation.**
When requests are queued, their context deadlines are inspected. The earliest deadline is stored in `batchDeadline`. When `processBatch` fires it creates `context.WithDeadline(appCtx, *batchDeadline)` as the shared batch context. If the upstream transport returns `context.DeadlineExceeded` and per-request contexts have a policy-attached cause (`ErrDynamicTimeoutExceeded`), the code waits up to 5ms for each request's context to close before reading its cause — a deliberate race-fix to prevent policy sentinels from being lost (`clients/http_json_rpc_client.go:L471-L479`).

**Batch response matching.**
- Array response: each element's `id` field is extracted, looked up in the `requests` map by value, matched response returned. Unmatched IDs produce a `"no response received for request ID: N"` error (`clients/http_json_rpc_client.go:L597-L605`). Extra response IDs (not in the request map) produce a warn log.
- Single-object response: broadcast to ALL callers in the batch. This handles the case where an upstream returns a single error object for the whole batch (e.g. rate-limit error with `id: null`).
- Non-JSON / non-object non-array response: the body is attempted to be parsed as a JSON-RPC error object and broadcast; if that fails too, all callers receive the parse error.

**Context cancellation before queue.**
`queueRequest` checks `req.ctx.Err()` before inserting. Already-canceled requests are immediately failed with `ErrEndpointRequestTimeout` or `ErrEndpointRequestCanceled` without touching the network (`clients/http_json_rpc_client.go:L241-L253`).

**Context cancellation after queue but before send.**
In `processBatch`, a pass over `requests` drops any entry whose `ctx.Err()` is non-nil before the HTTP call is made (`clients/http_json_rpc_client.go:L349-L361`).

**Response body gzip decompression.**
Both single (`sendSingleRequest`) and batch (`readResponseBody`) paths check `resp.Header.Get("Content-Encoding") == "gzip"` and decompress accordingly, regardless of the `enableGzip` request flag. The client always requests gzip via `Accept-Encoding: gzip`, so upstreams may compress even when request compression is off.

**OTel trace injection.**
`propagation.NewCompositeTextMapPropagator(TraceContext{}, Baggage{})` is constructed fresh on every `prepareRequest` call and injects W3C `traceparent` + `tracestate` + `baggage` headers. The span started in `sendSingleRequest` (`HttpJsonRpcClient.sendSingleRequest`) carries the upstream ID and network ID as attributes (`clients/http_json_rpc_client.go:L675-L681`).

**Error type mapping.**

| Condition | Error type |
|-----------|-----------|
| Transport-level error (non-context) | `ErrEndpointTransportFailure` |
| `context.DeadlineExceeded` or `ErrDynamicTimeoutExceeded` | `ErrEndpointRequestTimeout` |
| `context.Canceled` | `ErrEndpointRequestCanceled` |
| JSON-RPC error in response with `"context canceled"` in message | `ErrEndpointRequestCanceled` (reclassified) |
| JSON-RPC error in response with `"context deadline exceeded"` in message | `ErrEndpointRequestTimeout` (reclassified) |
| JSON body unparseable | `ErrJsonRpcExceptionInternal/ParseException` |
| Upstream `error` field in response | `ErrJsonRpcExceptionInternal/ServerSideException` (after extractor) |
| Malformed batch response (not array or object) | `ErrUpstreamMalformedResponse` |
| Pre-request creation error | `ErrHttp` (BaseError) |

**`ErrUpstreamMalformedResponse` — meaning, retryability, and status codes.**
Raised when a batch response body is neither a JSON array nor a JSON object (`clients/http_json_rpc_client.go:L634`). Struct definition at `common/errors.go:L835-L858`. Key properties:
- Message: `"malformed response from upstream"`. The `Cause` carries the raw parse failure (including a prefix of the raw body).
- `ErrorStatusCode()` returns `http.StatusBadRequest` (400) — this is the HTTP status eRPC returns to *its own caller* when this error propagates to the top of the stack (`common/errors.go:L856-L858`).
- Wire protocol: the response to the client is still a JSON-RPC envelope with HTTP 200 on the wire if the request arrived over HTTP (standard JSON-RPC transport), but the error object inside will carry code 400. This is the standard eRPC "method-level HTTP status vs wire-level HTTP status" split.
- Retryability: `ErrCodeUpstreamMalformedResponse` is **not** listed in the non-retryable set in `IsRetryableTowardsUpstream` (`common/errors.go:L2455-L2497`), so it defaults to `retryable=true` toward both the upstream (U:yes) and the network (N:yes). eRPC will attempt another upstream on this error.

**`ErrEndpointServerSideException` — `originalStatusCode` field.**
Struct at `common/errors.go:L1951-L1979`. `originalStatusCode int` is a private field set at construction time via `NewErrEndpointServerSideException(cause, details, originalStatusCode int)` (`common/errors.go:L1962-L1972`). The field feeds `ErrorStatusCode()`:
```go
func (e *ErrEndpointServerSideException) ErrorStatusCode() int {
    if e.originalStatusCode != 0 {
        return e.originalStatusCode
    }
    return http.StatusInternalServerError   // 500 fallback
}
```
(`common/errors.go:L1974-L1979`). The intent (documented in a code comment at L1954-L1957) is to pass the original upstream HTTP status verbatim to the caller so unknown vendor behaviors are not masked. Retryability: not in the non-retryable set → U:yes N:yes (`common/errors_retry_test.go:L99-L138`).

**`JsonRpcErrorExtractor` interface — plumbing and injection.**
The interface lives in `common/error_extractor.go:L10-L12`:
```go
type JsonRpcErrorExtractor interface {
    Extract(resp *http.Response, nr *NormalizedResponse, jr *JsonRpcResponse, upstream Upstream) error
}
```
The `JsonRpcErrorExtractorFunc` adapter (`common/error_extractor.go:L17-L21`) allows plain functions to satisfy the interface (http.HandlerFunc pattern). The only production implementation is `evm.JsonRpcErrorExtractor` (`architecture/evm/extractor.go:L11`), which delegates to `ExtractJsonRpcError` — the full EVM vendor/status-code normalization logic. Injection happens at `UpstreamsRegistry` construction: `evm.NewJsonRpcErrorExtractor()` is passed to `clients.NewClientRegistry(...)` → stored as `ClientRegistry.extractor` → forwarded to every `NewGenericHttpJsonRpcClient` call as the `extractor` parameter → stored as `client.errorExtractor` (`upstream/registry.go:L79`, `clients/registry.go:L75-L77`, `clients/http_json_rpc_client.go:L89-L101`). The extractor is called inside `normalizeJsonRpcError` *after* the context-sentinel reclassification pass and *before* the fallback `ErrJsonRpcExceptionInternal/ServerSideException` (`clients/http_json_rpc_client.go:L930`). This architecture allows architectures other than EVM to inject their own extractor without modifying the HTTP client.

### Observability

The HTTP client layer itself emits **no Prometheus metrics directly**. All upstream-level metrics (request counts, durations, errors, etc.) are emitted by `upstream/upstream.go` and `health/tracker.go` which wrap the client calls. See the metrics census for `erpc_upstream_request_total`, `erpc_upstream_request_errors_total`, `erpc_upstream_request_duration_seconds`, etc.

**Trace spans.**
- `HttpJsonRpcClient.sendSingleRequest` — span started for single requests only, with `network.id` and `upstream.id` attributes. `request.id` attribute added at detailed tracing level. `request.method` attribute added after JSON-RPC parse (`clients/http_json_rpc_client.go:L675-L683`).
- Batch requests do not create a per-call span; they execute under whatever context was propagated from the caller.

**Log messages.**
- `DEBUG` `"sending json rpc POST request (single)"` — host, raw request body, headers.
- `DEBUG` `"sending json rpc POST request (batch)"` — host, raw request body, headers.
- `DEBUG` `"queuing request ... for batch (current batch size: N)"`.
- `DEBUG` `"processing batch with N requests"`.
- `TRACE` `"using client from proxy pool"` — pool ID, transport pointer, resolved proxy URL.
- `TRACE` `"starting batch timer"`, `"setting batch deadline to earliest request deadline"`, `"committing batch to process total of N requests"`.
- `TRACE` response body (first 20 KiB + last 20 KiB if oversized).
- `ERROR` `"failed to get client from proxy pool"` — when `ProxyPool.GetClient` returns error (empty pool).
- `WARN` `"unexpected response received without ID"`, `"unexpected response received with ID: <id>"` — batch response mismatch.
- `ERROR` `"some requests did not receive a response (matching ID)"` — with full response body for debugging.

### Edge cases & gotchas

1. **`batchMaxSize: 0` with `supportsBatch: true` fires only by timer.** The size check is `len >= batchMaxSize`, and `0 >= 0` is always true, so the batch fires immediately on the first request queued — effectively disabling coalescing. Use `batchMaxSize: 1` to get single-request batching (pointless) or set a meaningful value ≥ 2. (`clients/http_json_rpc_client.go:L294`).

2. **`batchMaxWait` zero value fires instantly.** `time.AfterFunc(0, fn)` schedules `fn` in a new goroutine immediately; the first queued request will fire the batch before any further requests can join. Set a non-zero value (e.g. `"10ms"`) to allow coalescing. (`clients/http_json_rpc_client.go:L291`).

3. **Duplicate JSON-RPC IDs within a batch window flush the current batch.** If a second request arrives with the same `id` as one already queued, the pending batch is flushed and the new request starts a fresh batch. This preserves ID-to-response mapping correctness but can reduce batch efficiency for clients that reuse IDs. (`clients/http_json_rpc_client.go:L255-L261`).

4. **Custom `headers` overwrite, not append.** `Header.Set` is called for each key, so if a custom header key collides with a built-in (`Content-Type`, `User-Agent`, `Accept-Encoding`), the custom value wins. The OTel inject step runs after custom headers, so `traceparent` is set last and cannot be overridden. (`clients/http_json_rpc_client.go:L838-L847`).

5. **Forwarded headers are NOT sent on batch requests.** `req.ForwardHeaders` is consulted only in `sendSingleRequest`, not in `processBatch`. This is by design (batch requests aggregate multiple caller contexts), but callers expecting bearer tokens or `X-Custom` headers forwarded to upstreams must not rely on batching. (`clients/http_json_rpc_client.go:L736-L742`).

6. **ProxyPool reference is set twice when `jsonRpcCfg != nil`.** `NewGenericHttpJsonRpcClient` sets `client.proxyPool = proxyPool` in the struct literal at L91-L102, then unconditionally sets `client.proxyPool = proxyPool` again inside the `if jsonRpcCfg != nil` block at L147. The net effect is always the passed `proxyPool` value. This is harmless but redundant. (`clients/http_json_rpc_client.go:L91-L148`).

7. **Proxy URL scheme validation is a case-insensitive prefix string check, not URL parsing.** `ProxyPoolConfig.Validate()` does `strings.ToLower(url)` followed by `strings.HasPrefix` against `"http://"`, `"https://"`, and `"socks5://"` — exactly those three prefixes, nothing else (`common/validation.go:L238-L244`). A URL like `SOCKS5://proxy:1080` is accepted (lowercased to `socks5://proxy:1080`). A URL like `socks4://proxy:1080`, `ws://proxy`, or `ftp://proxy` fails even though `url.Parse` would accept them as syntactically valid URLs. The error is returned from `Validate()` at startup and prevents eRPC from starting. No URL is passed to `url.Parse` or `http.ProxyURL` until after this prefix gate.

8. **Empty proxy pool panics or returns error, falls back to direct.** If a `ProxyPool` is created with zero URLs (`createProxyPool` returns a pool + error), `GetClient` returns `fmt.Errorf("ProxyPool '%s' has no clients registered")`. `getHttpClient` logs an error and falls back to `c.httpClient` (direct). The proxy pool creation itself also returns an error, so this state should not normally be reached in production — startup validation would have caught it. (`clients/proxy_pool_registry.go:L69-L71`, `clients/http_json_rpc_client.go:L225-L231`).

9. **`MaxConnsPerHost: 0` (unlimited) is deliberate.** Prior behavior (Go default of 2) caused extreme connection churn at 100 RPS with 400ms upstream latency: with 2 connections and 400ms round-trip, only ~5 req/s could be served without queueing. With `MaxConnsPerHost: 0`, the OS-level TCP connection limit becomes the only cap. The fix was validated in `TestHTTPConnectionReuseUnderLoad` and `TestConnectionLimitsWithRealWorldScenario` (`clients/http_connection_test.go`).

10. **No per-upstream TLS configuration for HTTP upstreams.** `http.Transport` is built with zero `TLSClientConfig`, so Go uses the system CA bundle. There is no way to set a custom CA, client certificate, or `InsecureSkipVerify` for HTTP upstreams in the current config model. (gRPC upstreams do support TLS with system CAs, `clients/grpc_bds_client.go:L95-L99`).

11. **HTTP/2 is not explicitly enabled for upstream connections.** `ForceAttemptHTTP2` is not set on the transport. HTTP/2 may be negotiated via ALPN on `https://` upstreams that advertise it, but this is unverified and untested. Proxy transports are HTTP/1.1 tunnels in all cases.

12. **Batch response with a single object broadcasts to all callers.** When the upstream returns `{"jsonrpc":"2.0","error":{"code":-32000,...},"id":null}` (a single error, no array), every pending request in the batch receives the same cloned response. This handles rate-limit / server error responses that upstreams may send as a single object regardless of batch size. (`clients/http_json_rpc_client.go:L606-L636`, tested in `TestHttpJsonRpcClient_BatchRequests/SingleErrorForBatchRequest`).

13. **Partial batch response leaves some callers with an error.** If the upstream returns an array shorter than the number of pending requests, callers whose IDs are absent receive `"no response received for request ID: N"`. This is logged as an ERROR with the full response body. (`clients/http_json_rpc_client.go:L597-L605`, tested in `TestHttpJsonRpcClient_BatchRequests/PartialBatchResponse`).

14. **5ms race-fix window for policy-attached timeout sentinels in batch.** When a batch's `batchCtx` expires with `DeadlineExceeded`, each request's per-request context may still have `Cause == nil` for a few microseconds (the failsafe timer and the batch timer target the same nominal time). The 5ms `time.After` wait gives the per-request context's cause time to stabilize so the typed `ErrDynamicTimeoutExceeded` sentinel is preserved and the upstream-level classifier can promote it to `ErrFailsafeTimeoutExceeded`. (`clients/http_json_rpc_client.go:L471-L479`).

15. **`upstreamDefaults.jsonRpc` is all-or-nothing.** When an upstream has no `jsonRpc:` block at all, the full `upstreamDefaults.jsonRpc` struct is shallow-copied. But if an upstream has a `jsonRpc:` block (even empty `jsonRpc: {}`), the defaults block is not merged in — there is no field-level inheritance. (`common/defaults.go:L1550-L1558`).

16. **`ErrUpstreamMalformedResponse` is retryable — another upstream will be tried.** Unlike `ErrEndpointExecutionException` (which is a deterministic client-error), a malformed response may be transient (upstream briefly broken or returning an internal error page). `IsRetryableTowardsUpstream` returns `true` for this error code because it is not in the non-retryable allowlist (`common/errors.go:L2455-L2497`). If all upstreams consistently return malformed responses, the request ends as `ErrUpstreamsExhausted`. Operators can see this happening via `erpc_upstream_request_errors_total` with `error_type=ErrUpstreamMalformedResponse`.

17. **`ErrEndpointServerSideException.originalStatusCode` zero-value fallback is 500.** When `NewErrEndpointServerSideException` is called with `originalStatusCode == 0` (e.g. from gRPC paths that use a synthetic status), `ErrorStatusCode()` returns `http.StatusInternalServerError`. Callers should not assume that a 500 response from eRPC implies the upstream actually returned 500 — it may mean the original code was unavailable. (`common/errors.go:L1974-L1979`).

18. **`JsonRpcErrorExtractor` is not architecture-configurable at runtime.** The extractor is wired once at `UpstreamsRegistry` creation with `evm.NewJsonRpcErrorExtractor()`. There is no config field to swap or disable it. Non-EVM architectures would need a code change to inject a different extractor. (`upstream/registry.go:L79`).

19. **`ErrUpstreamMalformedResponse` method-level 400 vs wire 200 discrepancy.** `ErrorStatusCode()` returns 400, which flows into the JSON-RPC error envelope's HTTP status field used by eRPC's admin/metrics layer. However, over the JSON-RPC wire the response to the end-client is HTTP 200 (standard JSON-RPC transport). The 400 appears in logs and metrics labels, not in the HTTP response line seen by the caller. (`common/errors.go:L856-L858`).

### Source map

- `clients/http_json_rpc_client.go` — `GenericHttpJsonRpcClient`: transport construction, `prepareRequest`, single/batch send logic, gzip pools, OTel injection, error normalization, graceful shutdown.
- `clients/registry.go` — `ClientRegistry`: upstream → client lookup/creation (sync.Map), scheme dispatch, proxy pool resolution at creation time.
- `clients/proxy_pool_registry.go` — `ProxyPool` + `ProxyPoolRegistry`: one `http.Client` per proxy URL, round-robin via atomic counter, `GetPool` by ID.
- `common/config.go:L1072-L1079` — `JsonRpcUpstreamConfig` struct definition.
- `common/config.go:L1985-L1988` — `ProxyPoolConfig` struct definition.
- `common/validation.go:L230-L247` — `ProxyPoolConfig.Validate()`: enforces `id` required, `urls` non-empty, and the case-insensitive prefix check (`http://`, `https://`, `socks5://`) for every URL entry.
- `common/config.go:L48` — `ProxyPools []*ProxyPoolConfig` in top-level `Config`.
- `common/defaults.go:L1550-L1558` — upstream JsonRpc block inheritance from `upstreamDefaults`.
- `common/defaults.go:L1700-L1704` — unconditional `u.JsonRpc = &JsonRpcUpstreamConfig{}` if nil before `SetDefaults()`.
- `common/defaults.go:L1777-L1779` — `JsonRpcUpstreamConfig.SetDefaults()` is a no-op.
- `util/http_dialer.go` — `DefaultOutboundDialer()`: 10s dial timeout, 15s TCP keepalive.
- `clients/http_connection_test.go` — connection pool behavior tests under load (validates `MaxConnsPerHost:0` fix).
- `clients/http_json_json_client_test.go` — unit tests: timeout, batch scenarios, custom headers, proxy pool binding, error reclassification.
- `common/error_extractor.go` — `JsonRpcErrorExtractor` interface + `JsonRpcErrorExtractorFunc` adapter; avoids import cycles between `clients` and `architecture/evm`.
- `common/errors.go:L835-L858` — `ErrUpstreamMalformedResponse`: meaning (response not array/object), `ErrorStatusCode()` returns 400.
- `common/errors.go:L1951-L1979` — `ErrEndpointServerSideException`: `originalStatusCode` field, `ErrorStatusCode()` echos upstream HTTP status or 500.
- `common/errors.go:L2438-L2497` — `IsRetryableTowardsUpstream`: non-retryable allowlist (MalformedResponse not listed → retryable by default).
- `architecture/evm/extractor.go` — `evm.JsonRpcErrorExtractor`: production implementation of the interface, delegates to `ExtractJsonRpcError`.
- `upstream/registry.go:L79` — injection site: `evm.NewJsonRpcErrorExtractor()` passed into `ClientRegistry`.
