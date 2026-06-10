# KB: gRPC server & JSON-RPC bridge & BDS protocol

> status: complete
> source-dirs: erpc/grpc_server.go, erpc/grpc_json_rpc_bridge.go, erpc/request_processor.go, clients/grpc_bds_client.go, clients/grpc_bds_resilience.go, clients/registry.go, auth/grpc.go, common/grpc_errors.go, common/config.go, common/defaults.go, util/bounded_call.go, telemetry/metrics.go

## L1 — Capability (CTO view)

eRPC has a dual gRPC role: it is simultaneously a **gRPC server** (exposing typed BDS/EVM protobuf APIs to callers) and a **gRPC client** (consuming `grpc://`/`grpc+bds://` upstreams that speak the same BDS EVM service). The server side implements the Blockchain Data Standards `RPCQueryService` (unary) and `QueryService` (server-streaming) interfaces, translating each protobuf call into an internal JSON-RPC request that flows through the same routing, caching, and auth pipeline as HTTP requests. The client side pools three independent gRPC connections per upstream, wraps every call in a bounded-wait defense against H2 flow-control deadlocks, and runs a per-connection watchdog that force-replaces stuck connections while leaving healthy ones untouched.

## L2 — Mechanics (staff-engineer view)

### Server side — port sharing and startup

By default `server.grpcHostV4`/`grpcPortV4` are copied from `httpHostV4`/`httpPortV4`, so both protocols share a single TCP port. When `grpcSharesHttpV4` returns true (both enabled and addresses equal, `erpc/grpc_server.go:L42-L53`), `NewHttpServer` wraps the IPv4 handler in an h2c mux: HTTP/2 frames with `content-type: application/grpc` are routed to the in-process `*grpc.Server`; everything else falls through to the HTTP chain. The gRPC server therefore bypasses the HTTP timeout/gzip layers entirely (`erpc/http_server.go:L161-L177`, covered in KB 02). When the ports differ, `erpc/init.go:L125-L136` starts a standalone TCP listener for gRPC.

**Failure exit code**: when the standalone gRPC server's `Serve()` call returns an error, `erpc/init.go:L133` calls `util.OsExit(util.ExitCodeHttpServerFailed)` — exit code **1002** (defined at `util/exit.go:L9`). Process supervisors and Kubernetes readiness probes should treat exit code 1002 as a fatal gRPC startup failure (same code as HTTP server failure).

Graceful shutdown: a goroutine watches `appCtx.Done()` and calls `gs.server.GracefulStop()`, which drains in-flight RPCs before closing (`erpc/grpc_server.go:L126-L130`).

### Server side — request lifecycle

Every gRPC handler entry point calls `extractRequestInput` first (`erpc/grpc_server.go:L133-L158`). This function:
1. Reads gRPC incoming metadata; returns `codes.InvalidArgument` if absent.
2. Requires `x-erpc-project` and `x-erpc-chain-id` metadata; returns `codes.InvalidArgument` if missing.
3. Calls `auth.NewPayloadFromGrpc` to build an `AuthPayload` from `x-erpc-secret-token`, `authorization` (Basic or Bearer), or `x-siwe-message`/`x-siwe-signature` metadata. Falls back to `AuthTypeNetwork` if none match.
4. Resolves client IP via the same trusted-forwarder/header chain as the HTTP server (see §Client IP below).
5. Returns a `RequestInput{ProjectId, Architecture="evm", ChainId, AuthPayload, ClientIP, UserAgent}`.

**Unary methods** (`ChainId`, `GetBlockByNumber`, `GetBlockByHash`, `GetLogs`, `GetTransactionByHash`, `GetTransactionReceipt`, `GetBlockReceipts`) each call `gs.processor.ProcessUnary`:
- Constructs a synthetic `json.RawMessage` JSON-RPC request via `buildJSONRPCRequest` (random `id`, `erpc/grpc_json_rpc_bridge.go:L13-L21`).
- Validates it, authenticates the consumer, resolves the network, applies directive defaults, and calls `project.Forward`.
- Unpacks the `NormalizedResponse`, calls `parseJSONRPCResult` which copies the result bytes out of the pooled buffer, then deserializes the JSON-RPC result into the protobuf shape.
- On `null` results (block/tx/receipt not found), returns an empty proto response rather than an error (`erpc/grpc_server.go:L197-L199`, `L228-L230`, etc.).

**Streaming methods** (`QueryBlocks`, `QueryTransactions`, `QueryLogs`, `QueryTraces`, `QueryTransfers`) call `gs.processor.ProcessQueryStream`:
- Constructs a synthetic `NormalizedRequest` from the method name with empty params (auth/rate-limit only, no forwarding).
- Acquires a rate-limit permit, instantiates `EvmQueryExecutor`, and streams pages back via the `onPage` callback that calls `stream.Send`.
- No timeout layer is applied — the caller's context controls lifetime.

**Error mapping** (`mapToGRPCStatus`): eRPC error codes → gRPC status codes:
- `ErrCodeEndpointUnsupported` → `codes.Unimplemented`
- `ErrCodeEndpointUnauthorized` → `codes.Unauthenticated`
- `ErrCodeEndpointRequestTimeout` → `codes.DeadlineExceeded`
- `ErrCodeEndpointCapacityExceeded`, `ErrCodeEndpointRequestTooLarge` → `codes.ResourceExhausted`
- `ErrCodeEndpointMissingData` → `codes.NotFound`
- `ErrCodeEndpointClientSideException` → `codes.InvalidArgument`
- everything else → `codes.Internal`

**Panic recovery**: both unary and streaming interceptors catch panics, log them with stack trace, and return `codes.Internal "internal server error"` (`erpc/grpc_server.go:L451-L473`).

### Client side — URL schemes and TLS

`grpc://` and `grpc+bds://` both construct a `GenericGrpcBdsClient` (`clients/registry.go:L102-L111`). TLS selection in `pickTargetForBDS` (`clients/grpc_bds_resilience.go:L272-L285`):
- Port 443 → TLS (system CA, `tls.VersionTLS12` minimum).
- Scheme starts with `grpcs` or contains `tls` → TLS.
- Otherwise → `insecure.NewCredentials()`.
- If no port is in the URL, defaults to port 50051.
- All targets are prefixed with `dns:///` to enable gRPC name resolution and round-robin LB across DNS A-records.

### Client side — connection pool

`newBdsPool` dials exactly `bdsPoolSize` (= 3) independent `grpc.ClientConn` instances at startup (`clients/grpc_bds_resilience.go:L88-L116`). Each dial applies:
- `otelgrpc.NewClientHandler()` stats handler for OTel tracing.
- `grpcResponseMetadataInterceptor()` unary interceptor (records response metadata as span attributes when `common.IsTracingDetailed`).
- 100 MiB max recv/send per call.
- Service config: `round_robin` LB + transparent retries on `UNAVAILABLE` (max 2 attempts, 1s→5s backoff).
- `waitForReady: true` — queues RPCs during reconnects.
- Keepalive: ping every 30s, 5s timeout, permit-without-stream.
- Connect params: 3s min connect timeout, 100ms→1s backoff with 1.5× multiplier.

`Pick()` increments an atomic cursor modulo pool size — pure round-robin with `RLock` (`clients/grpc_bds_resilience.go:L154-L162`).

### Client side — bounded-wait defense and watchdog

`SendRequest` applies a hard cap via `context.WithTimeoutCause(ctx, bdsHardCallTimeout, common.ErrDynamicTimeoutExceeded)` as the first line of defense (`clients/grpc_bds_client.go:L190-L192`). Every actual gRPC call inside the method handlers uses `callBoundedT` / `callBounded` — wrappers around `util.BoundedCallT` — which race a goroutine executing the call against `ctx.Done()`. If the context fires first, the caller is unblocked immediately and the goroutine is left to clean up on its own timeline (`util/bounded_call.go:L27-L60`).

After each call, `SendRequest` inspects the error with `context.Cause(ctx)`:
- If `ourHardCap = errors.Is(err, common.ErrDynamicTimeoutExceeded)` → our cap fired. Feed to `pool.OnBoundedTimeout(conn, method)`, which increments the metric and may trigger `replaceConn`. Return `NewErrEndpointRequestTimeout`.
- If parent ctx deadline fired (not our cap) → normal caller timeout, NOT a wedge. Do NOT feed the watchdog. Return `NewErrEndpointRequestTimeout`.
- If `context.Canceled` → return `NewErrEndpointRequestCanceled`.
- Otherwise → unwrap gRPC status chain, call `normalizeGrpcError` → `ExtractGrpcErrorFromGrpcStatus`.

`ErrDynamicTimeoutExceeded` is the global sentinel defined at `common/errors.go:L1981-L1984` as `var ErrDynamicTimeoutExceeded = errors.New("dynamic timeout exceeded")`. The hard-cap context is created with `context.WithTimeoutCause(ctx, bdsHardCallTimeout, common.ErrDynamicTimeoutExceeded)`, so `context.Cause(ctx)` returns this sentinel (not generic `context.DeadlineExceeded`) when the 20s cap fires. The same sentinel is used by the failsafe timeout policy (`12-failsafe-timeout.md`) and the HTTP server deadline. Using `errors.Is(err, common.ErrDynamicTimeoutExceeded)` rather than checking `context.DeadlineExceeded` is what allows precise disambiguation between eRPC's own timeout policy and the caller's context deadline.

**Watchdog** (`recordStuck`, `replaceConn`): `recordStuck` maintains a rolling window of `bdsStuckCallThreshold` (= 3) stuck-call timestamps within `bdsStuckCallWindow` (= 60s) per connection. When the threshold is reached, `replaceConn` dials a new connection first (if dial fails, leaves old conn in place), then atomically swaps the pool slot and closes the old conn. A `bdsReplacementDedupWindow` (= 5s) dedup guard prevents double-replacement when multiple concurrent callers hit the threshold simultaneously (`clients/grpc_bds_resilience.go:L169-L242`).

### Client side — header propagation

Headers configured under `upstream.grpc.headers` are loaded into `GenericGrpcBdsClient.headers` at construction (`clients/grpc_bds_client.go:L83-L87`). The field is a `map[string]string` with a default of `{}` (nil/empty — no headers sent). At construction, `client.SetHeaders(cfg.Grpc.Headers)` copies the map into the client. On each request, if `len(c.headers) > 0`, a new `metadata.MD` is built from the map via `metadata.New(c.headers)` and attached to the outgoing context with `metadata.NewOutgoingContext(ctx, md)` — replacing any existing outgoing metadata for that call (`clients/grpc_bds_client.go:L231-L234`). This is the primary mechanism for sending `authorization: Bearer <token>` or vendor routing headers (e.g. a BDS provider API key) to `grpc://` upstreams. The same approach is used by the gRPC cache connector (`data/grpc.go`).

### Client side — error normalization

`normalizeGrpcError` walks the error `Unwrap` chain (since handler methods wrap gRPC errors with `fmt.Errorf`) to find a gRPC status, then calls `ExtractGrpcErrorFromGrpcStatus` (`common/grpc_errors.go:L12-L225`). BDS-specific error codes take priority over generic gRPC codes. Mapping:
- BDS `UNSUPPORTED_BLOCK_TAG`, `UNSUPPORTED_METHOD` → `ErrEndpointUnsupported`
- BDS `RANGE_OUTSIDE_AVAILABLE` → `ErrEndpointMissingData`
- BDS `INVALID_PARAMETER`, `INVALID_REQUEST` → `ErrEndpointClientSideException` (non-retryable toward network)
- BDS `RATE_LIMITED` → `ErrEndpointCapacityExceeded`
- BDS `TIMEOUT_ERROR` → `ErrEndpointRequestTimeout`
- BDS `RANGE_TOO_LARGE` → `ErrEndpointRequestTooLarge`
- BDS `INTERNAL_ERROR` → `ErrEndpointServerSideException`
- gRPC `Canceled` → `ErrEndpointRequestCanceled`
- gRPC `Unimplemented` → `ErrEndpointUnsupported`
- gRPC `InvalidArgument` → `ErrEndpointClientSideException` (non-retryable)
- gRPC `ResourceExhausted` → `ErrEndpointCapacityExceeded`
- gRPC `DeadlineExceeded` → `ErrEndpointRequestTimeout`
- gRPC `Unauthenticated`, `PermissionDenied` → `ErrEndpointUnauthorized`
- gRPC `NotFound`, `OutOfRange` → `ErrEndpointMissingData`
- gRPC `Internal`, `Unknown`, `Unavailable`, and all others → `ErrEndpointServerSideException`

All error detail maps carry `grpcCode`, `grpcMessage`, and `upstreamId`; BDS errors additionally carry `bdsErrorCode`, optional `cause`, and any BDS `Details` map entries.

### Client side — eth_getLogs limitation

`handleGetLogs` only accepts numeric `fromBlock`/`toBlock` hex strings. If either is a block tag (`latest`, `earliest`, `pending`), it returns an error: "special block numbers not yet supported via gRPC for eth_getLogs" (`clients/grpc_bds_client.go:L587-L589`).

### Client side — eth_getBlockByNumber → GetBlockByHash dispatch

If the first param of `eth_getBlockByNumber` is an object with `blockHash` (EIP-1898), `handleGetBlockByNumber` extracts the hash bytes and silently routes to `GetBlockByHash` instead of `GetBlockByNumber` (`clients/grpc_bds_client.go:L341-L407`).

### Query streaming (client side)

`eth_queryBlocks`, `eth_queryTransactions`, `eth_queryLogs`, `eth_queryTraces`, `eth_queryTransfers` open a server-streaming call on the BDS `QueryService` and drain all pages with `recvQueryStream`. Pages are accumulated into a single aggregate response (blocks/txs/logs/traces/transfers appended; `FromBlock`/`ToBlock` first-wins; `CursorBlock` last-wins via `applyQueryRangeBounds`). The entire stream lifetime (open + recv loop) is wrapped in `callBoundedT` so that H2 flow-control deadlocks during stream-open are also bounded (`clients/grpc_bds_client.go:L1100-L1125`).

The aggregate is then converted to JSON-RPC shape via `evm.Query*ResponseToJsonRpc` helpers from the BDS manifesto library.

### Client IP resolution (gRPC server)

The `grpcClientIP` method mirrors HTTP's `resolveRealClientIP`. The direct peer IP comes from the gRPC `peer.Peer` context. If the peer is in `trustedForwarderIPs` or `trustedForwarderNets`, the server walks `trustedIPHeaders` metadata values in order, strips trailing trusted proxies right-to-left via `trimRightTrustedAndPick`, and returns the nearest untrusted IP. The same `server.trustedIPForwarders` / `server.trustedIPHeaders` config fields used by the HTTP server drive this behavior (`erpc/grpc_server.go:L512-L532`).

## L3 — Exhaustive reference (agent view)

### Config fields

#### Server-side gRPC fields (under `server.`)

Port sharing with HTTP is already documented in KB 02 (rows 8–12). Summarized here for completeness; see KB 02 for full derivation details.

| # | YAML path | Type | Default | Behavior / notes |
|---|-----------|------|---------|------------------|
| 1 | `server.grpcEnabled` | `*bool` | `false` (`common/defaults.go:L669-L671`) | Master enable. When true and host/port match HTTP v4, shares the HTTP port via h2c mux. When true and ports differ, starts a standalone listener. |
| 2 | `server.grpcHostV4` | `*string` | copy of `httpHostV4` (`common/defaults.go:L672-L675`) | gRPC IPv4 bind host. |
| 3 | `server.grpcPortV4` | `*int` | copy of `httpPortV4` (`common/defaults.go:L676-L679`) | gRPC IPv4 bind port. Equal to HTTP by default → port sharing. |
| 4 | `server.grpcHostV6` | `*string` | copy of `httpHostV6` (`common/defaults.go:L680-L683`) | gRPC IPv6 bind host (no v6 sharing logic). |
| 5 | `server.grpcPortV6` | `*int` | copy of `httpPortV6` (`common/defaults.go:L684-L687`) | gRPC IPv6 bind port. |
| 6 | `server.grpcMaxRecvMsgSize` | `*int` | `104857600` (100 MiB) (`common/defaults.go:L688-L690`) | Server-side max incoming protobuf message bytes (`erpc/grpc_server.go:L99`). |
| 7 | `server.grpcMaxSendMsgSize` | `*int` | `104857600` (100 MiB) (`common/defaults.go:L691-L693`) | Server-side max outgoing protobuf message bytes (`erpc/grpc_server.go:L100`). |
| 8 | `server.tls.enabled` | `bool` | `false` | When true, loads `certFile`/`keyFile` and creates `grpc.Creds`. Applied to standalone gRPC listener only (port sharing uses h2c which doesn't support TLS independently; the HTTP TLS layer covers the shared port). Source: `erpc/grpc_server.go:L105-L111`. |
| 9 | `server.trustedIPForwarders` | `[]string` | `[]` (loopback only in HTTP; gRPC has same logic) | CIDRs and IPs whose forwarded-IP headers are trusted for client IP resolution. Both HTTP and gRPC servers parse this. `erpc/grpc_server.go:L69-L96`. |
| 10 | `server.trustedIPHeaders` | `[]string` | `[]` | Header names (lowercased) to inspect for the real client IP when the peer is a trusted forwarder. `erpc/grpc_server.go:L89-L95`. |

#### Upstream-side gRPC fields (under `upstreams[].grpc.`)

| # | YAML path | Type | Default | Behavior / notes |
|---|-----------|------|---------|------------------|
| 11 | `upstreams[].endpoint` | `string` | — | Must start with `grpc://` or `grpc+bds://` to activate the BDS client. `grpcs://` and scheme containing `tls` also trigger TLS. No default — required. `clients/registry.go:L102`. |
| 12 | `upstreams[].grpc.headers` | `map[string]string` | `nil`/`{}` — no default set in `SetDefaults`; absence means no metadata headers sent. `common/config.go:L1100-L1102` | Key/value pairs sent as outgoing gRPC metadata on every call. Loaded at client construction via `client.SetHeaders(cfg.Grpc.Headers)` (`clients/grpc_bds_client.go:L83-L87`). On each `SendRequest`, if `len(c.headers) > 0`, a new `metadata.MD` is built and injected via `metadata.NewOutgoingContext(ctx, md)` (`clients/grpc_bds_client.go:L231-L234`). This replaces any existing outgoing metadata for that call (addToOutgoingContext is NOT used). Common uses: `authorization: Bearer <api_key>` for authenticated BDS providers, or custom vendor routing headers. Keys must be lowercase per gRPC metadata conventions. |

### Behaviors & algorithms

#### gRPC server — registered services

Two BDS services are registered on the same `*grpc.Server` instance (`erpc/grpc_server.go:L114-L115`):
- `evm.RPCQueryService` — unary methods for per-item lookups.
- `evm.QueryService` — server-streaming methods for bulk/paginated queries.

#### gRPC server — required metadata

Every inbound RPC must carry:
- `x-erpc-project` — project ID (string, mandatory; `codes.InvalidArgument` if absent).
- `x-erpc-chain-id` — chain ID string (mandatory; `codes.InvalidArgument` if absent).
- `x-erpc-architecture` — defaults to `"evm"` if absent.
- `user-agent` — optional; if present, read by `extractRequestInput` and stored in `RequestInput.UserAgent` (`erpc/grpc_server.go:L156`). `ProcessQueryStream` calls `nq.SetAgentName(input.UserAgent)` which populates the `agent_name` label on downstream Prometheus metrics (same as the HTTP `User-Agent` header effect). Not mandatory; absent = empty string in `RequestInput`, no `agent_name` label set.

**Important**: the `trustedIPHeaders` metadata keys must be **lowercase** in gRPC clients. `NewGrpcServer` lowercases all entries from `cfg.TrustedIPHeaders` at construction (`erpc/grpc_server.go:L89-L95`). gRPC metadata keys are inherently lowercase on the wire, so a client sending `X-Forwarded-For` (mixed case) will be lowercased by the gRPC framework. However, if `trustedIPHeaders` is configured with mixed-case values in YAML, they are normalized to lowercase at server init, ensuring consistent lookup via `firstMD(md, hdr)` (`erpc/grpc_server.go:L524`).

**Critical limitation — HTTP `X-ERPC-*` request directives are NOT parsed on the gRPC path.** `ProcessUnary` calls only `nq.ApplyDirectiveDefaults(network.Config().DirectiveDefaults)` (`erpc/request_processor.go:L64`); it never calls the HTTP directive enrichment (`EnrichFromHttp`). gRPC metadata keys like `x-erpc-skip-cache-read`, `x-erpc-use-upstream`, `x-erpc-retry-hint`, or any other `X-ERPC-*` HTTP request directive are **silently ignored** — they do not affect behavior. Only the network-level `directiveDefaults` configuration applies. Users expecting per-call cache bypass or upstream pinning via gRPC metadata will find these silently no-oped.

#### gRPC server — auth metadata (via `auth/grpc.go:L12-L54`)

Priority order:
1. `x-erpc-secret-token` → `AuthTypeSecret`.
2. `authorization: Basic <base64>` → `AuthTypeSecret` (password field used as secret).
3. `authorization: Bearer <token>` → `AuthTypeJwt`.
4. `x-siwe-message` + `x-siwe-signature` → `AuthTypeSiwe`.
5. None of the above → `AuthTypeNetwork` (IP-based).

#### gRPC server — unary method dispatch table

| gRPC method | JSON-RPC method | Params construction | Special handling |
|-------------|-----------------|---------------------|------------------|
| `ChainId` | `eth_chainId` | `[]` | Hex→uint64 conversion on result |
| `GetBlockByNumber` | `eth_getBlockByNumber` | `[req.BlockNumber, req.IncludeTransactions]` | `null` result → empty proto response |
| `GetBlockByHash` | `eth_getBlockByHash` | `[hex(req.BlockHash), req.IncludeTransactions]` | `null` result → empty proto response |
| `GetLogs` | `eth_getLogs` | filter object with hex fromBlock/toBlock; single addr vs array | Topic position-preserving (TopicFilter with empty Values = wildcard) |
| `GetTransactionByHash` | `eth_getTransactionReceipt` | `[hex(req.TransactionHash)]` | `null` → empty proto |
| `GetTransactionReceipt` | `eth_getTransactionReceipt` | `[hex(req.TransactionHash)]` | `null` → empty proto |
| `GetBlockReceipts` | `eth_getBlockReceipts` | blockHash OR blockNumber | `codes.InvalidArgument` if neither present |

#### gRPC server — streaming method dispatch table

| gRPC method | JSON-RPC method | Auth/rate-limit | Notes |
|-------------|-----------------|-----------------|-------|
| `QueryBlocks` | `eth_queryBlocks` | Yes | Pages streamed via `onPage` callback |
| `QueryTransactions` | `eth_queryTransactions` | Yes | " |
| `QueryLogs` | `eth_queryLogs` | Yes | " |
| `QueryTraces` | `eth_queryTraces` | Yes | " |
| `QueryTransfers` | `eth_queryTransfers` | Yes | " |

Streaming methods use `NewEvmQueryExecutor` for actual execution and do NOT call `project.Forward` (`erpc/request_processor.go:L134-L136`).

#### BDS client — supported JSON-RPC methods (switch in `SendRequest`)

Supported: `eth_getBlockByNumber`, `eth_getBlockByHash`, `eth_getLogs`, `eth_getTransactionByHash`, `eth_getTransactionReceipt`, `eth_getBlockReceipts`, `eth_chainId`, `eth_queryBlocks`, `eth_queryTransactions`, `eth_queryLogs`, `eth_queryTraces`, `eth_queryTransfers`.

Unsupported: any other method returns `ErrEndpointUnsupported` immediately without consuming a pool connection (`clients/grpc_bds_client.go:L270-L275`).

#### BDS client — TLS selection logic (`pickTargetForBDS`, `clients/grpc_bds_resilience.go:L272-L285`)

```
port == 443                 → TLS (system CA)
scheme starts with "grpcs"  → TLS (system CA)
scheme contains "tls"       → TLS (system CA)
otherwise                   → insecure
```
Target is always `dns:///host:port`. Port 50051 is the fallback when no port in URL.

#### BDS client — `ExtractGrpcErrorFromGrpcStatus` full mapping (`common/grpc_errors.go:L12-L225`)

`normalizeGrpcError` (in `clients/grpc_bds_client.go`) walks the error `Unwrap` chain to find a gRPC `*status.Status`, then delegates to `common.ExtractGrpcErrorFromGrpcStatus`. BDS-layer error codes are checked first (via `bdscommon.FromGRPCStatus`); if no BDS detail is present, the raw gRPC code is used. Every returned error carries a `details` map with `grpcCode`, `grpcMessage`, and `upstreamId`; BDS errors also carry `bdsErrorCode`, optional `cause`, and any BDS `Details` map entries.

**BDS error-code priority path** (`common/grpc_errors.go:L43-L122`):

| BDS `ErrorCode` | eRPC error | Retryable toward network? | Notes |
|-----------------|-----------|--------------------------|-------|
| `UNSUPPORTED_BLOCK_TAG`, `UNSUPPORTED_METHOD` | `ErrEndpointUnsupported` | default | `JsonRpcErrorUnsupportedException` nested |
| `RANGE_OUTSIDE_AVAILABLE` | `ErrEndpointMissingData` | default | `JsonRpcErrorMissingData` nested; carries `upstream` |
| `INVALID_PARAMETER`, `INVALID_REQUEST` | `ErrEndpointClientSideException` | **false** (`.WithRetryableTowardNetwork(false)`) | `JsonRpcErrorInvalidArgument` nested |
| `RATE_LIMITED` | `ErrEndpointCapacityExceeded` | default | `JsonRpcErrorCapacityExceeded` nested |
| `TIMEOUT_ERROR` | `ErrEndpointRequestTimeout` | default | `JsonRpcErrorNodeTimeout` nested; treated as upstream timeout, **not** a server-side exception |
| `RANGE_TOO_LARGE` | `ErrEndpointRequestTooLarge` | default | `JsonRpcErrorEvmLargeRange` nested; `EvmBlockRangeTooLarge` reason |
| `INTERNAL_ERROR` | `ErrEndpointServerSideException` | default | `JsonRpcErrorServerSideException` nested |

**gRPC code fallback path** (`common/grpc_errors.go:L124-L224`; only reached when `hasBdsError` is false or BDS code is unrecognized):

| gRPC `code` | eRPC error | Retryable toward network? |
|-------------|-----------|--------------------------|
| `Canceled` | `ErrEndpointRequestCanceled` | n/a (hedging cancellation) |
| `Unimplemented` | `ErrEndpointUnsupported` | default |
| `InvalidArgument` | `ErrEndpointClientSideException` | **false** |
| `ResourceExhausted` | `ErrEndpointCapacityExceeded` | default |
| `DeadlineExceeded` | `ErrEndpointRequestTimeout` | default |
| `Unauthenticated`, `PermissionDenied` | `ErrEndpointUnauthorized` | default |
| `NotFound`, `OutOfRange` | `ErrEndpointMissingData` | default; carries `upstream` |
| `Internal`, `Unknown`, `Unavailable` | `ErrEndpointServerSideException` | default |
| all other codes | `ErrEndpointServerSideException` | default |

These eRPC error codes then feed `mapToGRPCStatus` on the **server** side when re-encoding errors back to the caller (see error mapping table in L2 above).

#### BDS client — service config (embedded JSON, `clients/grpc_bds_client.go:L111-L124`)

```json
{
  "loadBalancingConfig": [{"round_robin":{}}],
  "methodConfig": [{
    "name": [{"service": ""}],
    "waitForReady": true,
    "retryPolicy": {
      "maxAttempts": 2,
      "initialBackoff": "1s",
      "maxBackoff": "5s",
      "backoffMultiplier": 2,
      "retryableStatusCodes": ["UNAVAILABLE"]
    }
  }]
}
```

#### BDS client — resilience tunables (all `var` in `clients/grpc_bds_resilience.go:L29-L51`)

| Variable | Value | Meaning |
|----------|-------|---------|
| `bdsHardCallTimeout` | `20s` | Absolute per-call ceiling; fires as `ErrDynamicTimeoutExceeded` |
| `bdsPoolSize` | `3` | Number of independent connections per upstream |
| `bdsStuckCallThreshold` | `3` | Number of hard-cap firings within window before watchdog fires |
| `bdsStuckCallWindow` | `60s` | Rolling window for stuck-call counting |
| `bdsReplacementDedupWindow` | `5s` | Minimum gap between replacements of the same slot |

All declared as `var`, not `const`, so tests override them safely.

#### BDS client — dial parameters (`clients/grpc_bds_resilience.go:L118-L151`)

- Max recv/send per call: 100 MiB each.
- Keepalive: ping every 30s, 5s timeout, `PermitWithoutStream=true`.
- Connect timeout: 3s minimum; backoff 100ms base, 1.5× multiplier, 20% jitter, 1s max.

#### `util.BoundedCallT` algorithm (`util/bounded_call.go:L27-L60`)

Fast-path: if `ctx.Err() != nil`, return `context.Cause(ctx)` without spawning a goroutine.
Otherwise: spawn goroutine executing `fn(ctx)`; `select` on `done chan` vs `ctx.Done()`. If context fires first: return `context.Cause(ctx)` (exposes the explicit cause, not generic `DeadlineExceeded`). Goroutine is left to clean up on its own. Panics inside `fn` are caught by a deferred `recover` and sent as errors on `done`.

### Observability

#### Prometheus metrics

| Metric | Labels | When it fires |
|--------|--------|---------------|
| `erpc_grpc_bds_hard_timeout_total` | `project`, `upstream`, `method` | Each time `SendRequest` detects our `bdsHardCallTimeout` fired (i.e. `context.Cause == ErrDynamicTimeoutExceeded`). Non-zero rate = wedged H2 streams. `telemetry/metrics.go:L393-L397`, fired at `clients/grpc_bds_resilience.go:L170`. |
| `erpc_grpc_bds_conn_replacements_total` | `project`, `upstream` | Each successful watchdog-driven connection replacement (dial succeeded and slot swapped). `telemetry/metrics.go:L401-L405`, fired at `clients/grpc_bds_resilience.go:L236`. |

OTel gRPC instrumentation (`otelgrpc.NewServerHandler()` / `otelgrpc.NewClientHandler()`) generates standard gRPC spans on both server and client sides. `grpcResponseMetadataInterceptor` adds `grpc.response.metadata.*` attributes to the client span when `common.IsTracingDetailed` is true.

#### Trace spans

- `GrpcBdsClient.SendRequest` — root client span per `SendRequest` call; attributes `network.id`, `upstream.id`, `request.id` (detail-only), `request.method`.
- `GrpcBdsClient.GetBlockByNumber` / `GetBlockByHash` — detail span; `block_number`, `is_block_tag`, `original_param`, `response_has_block`, `response_block_number`, `response_block_hash`.
- `GrpcBdsClient.GetLogs` — detail span; `from_block`, `to_block`.
- `GrpcBdsClient.QueryBlocks` / `QueryTransactions` / `QueryLogs` / `QueryTraces` / `QueryTransfers` — detail spans; no extra attributes currently.
- `QueryStream.Handle` — server-side root span per streaming request; attributes `query.method`, `project.id`, `network.id`.

#### Log messages

- `"starting gRPC server"` — INFO at startup, carries `addr`. (`erpc/grpc_server.go:L125`)
- `"gRPC unary panic"` / `"gRPC stream panic"` — ERROR with `panic` and `stack` fields. (`erpc/grpc_server.go:L455`, `L467`)
- `"created gRPC BDS client"` — DEBUG; carries `target`, `pool_size`, `hard_call_timeout`. (`clients/grpc_bds_client.go:L138-L143`)
- `"BDS bounded-wait timeout fired"` — WARN; carries `err`, `network.id`, `upstream.id`, `method`, `request.id`, `our_hardcap`. (`clients/grpc_bds_client.go:L293-L304`)
- `"BDS watchdog: replacing wedged connection"` — WARN; carries `target`, `upstream.id`, `slot`. (`clients/grpc_bds_resilience.go:L231-L235`)
- `"BDS watchdog: failed to dial replacement; old conn left in place for grpc-go to reconnect"` — ERROR. (`clients/grpc_bds_resilience.go:L225`)
- `"processing query stream request"` — INFO per stream; carries `component`, `projectId`, `networkId`, `method`, `clientIP`. (`erpc/request_processor.go:L102`)
- `"query stream completed with error"` / `"query stream completed successfully"` — INFO; carries `durationMs`. (`erpc/request_processor.go:L140-L144`)
- `"invalid CIDR in trusted forwarders; ignoring"` / `"invalid IP in trusted forwarders; ignoring"` — WARN at construction. (`erpc/grpc_server.go:L79`, `L85`)

### Edge cases & gotchas

1. **Topic null-position collapsing.** `eth_getLogs` topics with `null` entries (wildcards) MUST preserve positional alignment. Dropping a null shifts subsequent topic filters left — e.g. `[selector, null, toAddr]` becoming `[selector, toAddr]` would match logs where topic[1]=toAddr instead of topic[2]=toAddr, silently returning zero results. `buildTopicFilters` emits an empty `TopicFilter{Values: nil}` for null entries to preserve positions. Regression test: `clients/grpc_bds_client_test.go:TestBuildTopicFiltersPreservesNullPositions`.

2. **`eth_getLogs` does not support block tags.** `"latest"`, `"earliest"`, `"pending"` in `fromBlock`/`toBlock` return an error instead of being resolved. Only numeric hex strings work. `clients/grpc_bds_client.go:L587-L589`.

3. **Watchdog does NOT fire on caller-side timeouts.** Only our own `bdsHardCallTimeout` (identified via `context.Cause == ErrDynamicTimeoutExceeded`) triggers watchdog logic. A caller's 500ms deadline firing on a wedged server does NOT increment the stuck counter. This prevents legitimate slow-path caller timeouts from churning the pool. `clients/grpc_bds_client.go:L291-L304`, `clients/grpc_bds_resilience_test.go:TestGrpcBdsClient_WatchdogIgnoresCallerDeadline`.

4. **Goroutine leak is intentional for wedged streams.** When `callBoundedT` abandons an in-flight gRPC call because the context fired, the goroutine executing the call is left running. This is a deliberate trade: the caller is unblocked immediately, and grpc-go will clean up the goroutine when its own stream management eventually honors the cancellation. The leaked goroutine is bounded in practice because grpc-go does honor context eventually. `util/bounded_call.go:L56-L59`, test: `clients/grpc_bds_resilience_test.go:TestCallBoundedAbandonsStuckFunc`.

5. **Dual-replacement dedup.** When many concurrent callers all hit the threshold simultaneously, all call `replaceConn` concurrently. The `bdsReplacementDedupWindow` guard (`c.closedAt` atomic with 5s window) ensures only the first replacement executes; subsequent attempts within 5s are no-ops. `clients/grpc_bds_resilience.go:L213-L216`, test: `clients/grpc_bds_resilience_extras_test.go:TestPool_ReplaceConn_DedupWithin5s`.

6. **Dial-first replacement.** If the watchdog dial fails (e.g. DNS hiccup), the old (broken) connection is left in place rather than parking the slot with a permanently-closed conn. This is strictly better because grpc-go keeps retrying through the existing conn with its own reconnect backoff. `clients/grpc_bds_resilience.go:L218-L227`.

7. **TLS on port 443 only, not on arbitrary ports.** Unlike scheme-based detection, port-based TLS detection only fires for port 443 exactly. Port 8443 with `grpc://` stays insecure. Use `grpcs://` scheme to force TLS on non-443 ports. `clients/grpc_bds_resilience.go:L279-L281`.

8. **Port sharing bypasses HTTP timeout and gzip layers.** gRPC traffic multiplexed on the HTTP port is handled entirely by the in-process `*grpc.Server`; the HTTP `TimeoutHandler` and `gzipHandler` never see gRPC frames. gRPC has its own per-RPC deadline machinery. `erpc/http_server.go:L161-L177`.

9. **Nil upstream at BDS client construction.** `NewGrpcBdsClient` accepts a nil `upstream` (used in tests and possibly during early bootstrap). Header loading and error classification are nil-safe; `upstream.Id()` returns `"n/a"` in error/metric labels. `clients/grpc_bds_client.go:L62-L65`, `L83-L87`.

10. **Query methods must not be rejected as unsupported at the `SendRequest` level.** All five `eth_query*` methods are in the dispatch switch. A missing entry would return `ErrEndpointUnsupported`, which would permanently disqualify the upstream from carrying that traffic. Regression test: `clients/grpc_bds_client_test.go:TestGrpcBdsClientQueryMethodsDoNotShortCircuit`.

11. **`grpc+bds://` is an alias for `grpc://`.** Both trigger identical `GenericGrpcBdsClient` construction. The scheme distinction exists for semantic clarity in config but has no behavioral difference. `clients/registry.go:L102`.

12. **Legacy dead `evm.ExtractGrpcError` has divergent semantics and must NOT be revived.** `architecture/evm/error_normalizer.go:L660-L876` contains a second, older copy of the BDS/gRPC error normalizer with no non-test callers. It differs from the live `common.ExtractGrpcErrorFromGrpcStatus` in two critical ways that would silently change retry behavior if re-activated:
   - BDS `TIMEOUT_ERROR` maps to `ErrEndpointServerSideException` (via `NewErrEndpointServerSideException`) instead of `ErrEndpointRequestTimeout`. This means upstream timeouts would be classified as internal errors rather than request-timeouts, breaking the retry/hedging logic that specifically detects `ErrEndpointRequestTimeout`.
   - gRPC `DeadlineExceeded` maps to `ErrEndpointServerSideException` instead of `ErrEndpointRequestTimeout` — same wrong classification.
   - There is **no `Canceled` branch** in the gRPC fallback path; `codes.Canceled` would fall through to the `default` case and produce `ErrEndpointServerSideException` instead of `ErrEndpointRequestCanceled`, causing hedging-cancel signals to be misclassified.
   Any future gRPC client work must use `common.ExtractGrpcErrorFromGrpcStatus` exclusively. Source: `architecture/evm/error_normalizer.go:L740-L825`.

13. **Upstream `waitForReady: true` can mask unreachable upstreams.** RPCs queue indefinitely during reconnects instead of failing immediately with `UNAVAILABLE`. This is desirable for brief network hiccups but means a permanently unreachable upstream will silently absorb RPCs until the caller's context deadline fires. The `bdsHardCallTimeout` caps this at 20s.

14. **`X-ERPC-*` HTTP request directives are silently ignored on the gRPC path.** `ProcessUnary` only calls `nq.ApplyDirectiveDefaults(...)` (`erpc/request_processor.go:L64`); it never calls `nq.EnrichFromHttp(...)`. A gRPC client sending `x-erpc-skip-cache-read: true`, `x-erpc-use-upstream: myupstream`, or any other `X-ERPC-*` directive in metadata will find these silently no-oped — the fields are never parsed, never set on the `NormalizedRequest`. Only network-level `directiveDefaults` config applies to gRPC calls. There is no error or warning; the directive is simply absent. This is a deliberate simplification of the gRPC path.

15. **`trustedIPHeaders` config values are lowercased at server init; gRPC metadata keys are always lowercase.** `NewGrpcServer` lowercases all entries from `cfg.TrustedIPHeaders` (`erpc/grpc_server.go:L89-L95`). gRPC metadata keys on the wire are inherently lowercase (the gRPC spec mandates this). A YAML config with `trustedIPHeaders: ["X-Forwarded-For"]` works correctly because the server normalizes it to `x-forwarded-for`. However, a gRPC client that sends mixed-case metadata will have its values lowercased by grpc-go before the server sees them. The `firstMD(md, hdr)` lookup (`erpc/grpc_server.go:L524`) uses the pre-lowercased `hdr` value. Contrast with HTTP where the `net/http` package handles case-insensitive header lookup automatically.

16. **`bdsHardCallTimeout` uses `ErrDynamicTimeoutExceeded` as context cause — same sentinel as failsafe policy.** Both the BDS client's hard cap (`clients/grpc_bds_client.go:L190-L192`) and the failsafe timeout policy use `context.WithTimeoutCause(ctx, d, common.ErrDynamicTimeoutExceeded)`. When a BDS call is wrapped inside a failsafe policy (e.g. the network's upstream selection applies a timeout), the inner `bdsHardCallTimeout` fires first if the BDS cap (20s) is shorter than the policy deadline. The `errors.Is(err, common.ErrDynamicTimeoutExceeded)` check in `SendRequest` will correctly catch this and trigger the watchdog. If the outer policy timeout fires first (policy deadline < 20s), `context.Cause` returns `ErrDynamicTimeoutExceeded` from the policy layer, which is still `errors.Is` equal to the sentinel, so the watchdog ALSO fires. Operators should set failsafe policy timeouts > 20s if they want to guarantee the BDS hard cap is the outer bound; shorter policy timeouts will also feed the watchdog. `common/errors.go:L1981-L1984`.

17. **Standalone gRPC server failure produces exit code 1002.** When the standalone gRPC server's `Serve()` returns an error (port in use, permission denied, TLS cert load failure), `erpc/init.go:L133` calls `util.OsExit(util.ExitCodeHttpServerFailed)` which is exit code **1002** (`util/exit.go:L9`). This is the same exit code as HTTP server failure. Kubernetes readiness probes and process supervisors (systemd, supervisord) should treat exit code 1002 as fatal and not attempt automatic restart without correcting the underlying configuration.

### Source map

- `erpc/grpc_server.go` — `GrpcServer` struct, `NewGrpcServer`, `Start`, method handlers for both BDS services, `extractRequestInput`, `mapToGRPCStatus`, panic interceptors, client-IP resolution.
- `erpc/grpc_json_rpc_bridge.go` — `buildJSONRPCRequest` (synthetic JSON-RPC request factory with random ID), `parseJSONRPCResult` (unwraps `NormalizedResponse` into raw bytes).
- `erpc/request_processor.go` — `RequestProcessor`, `ProcessUnary` (auth + forward), `ProcessQueryStream` (auth + rate-limit + `EvmQueryExecutor`), `queryMethodFromProto` dispatch helper.
- `clients/grpc_bds_client.go` — `GenericGrpcBdsClient`, `SendRequest` (bounded-wait + method dispatch), per-method handlers for all 12 supported methods, `normalizeGrpcError`, `buildTopicFilters`, streaming aggregation helpers.
- `clients/grpc_bds_resilience.go` — `bdsPool`, `bdsConn`, `newBdsPool`, `dial`, `Pick`, `OnBoundedTimeout`, `recordStuck`, `replaceConn`, `Shutdown`, `pickTargetForBDS`, resilience tunables.
- `clients/registry.go` — scheme dispatch (`grpc://`, `grpc+bds://` → `NewGrpcBdsClient`).
- `auth/grpc.go` — `NewPayloadFromGrpc` — maps gRPC metadata to `AuthPayload`.
- `common/grpc_errors.go` — `ExtractGrpcErrorFromGrpcStatus` — BDS + gRPC status → eRPC error code mapping.
- `util/bounded_call.go` — `BoundedCallT`, `BoundedCall` — context-bounded goroutine wrapper, foundation of the wedged-stream defense.
- `common/defaults.go:L669-L693` — gRPC server config defaults.
- `common/config.go:L1096-L1118` — `GrpcUpstreamConfig` struct.
- `common/errors.go:L1981-L1984` — `ErrDynamicTimeoutExceeded` sentinel definition.
- `util/exit.go:L8-L9` — `ExitCodeERPCStartFailed` (1001) and `ExitCodeHttpServerFailed` (1002) exit code constants.
- `erpc/init.go:L125-L136` — standalone gRPC server startup and exit-code-1002 failure path.
- `architecture/evm/error_normalizer.go:L660-L876` — dead legacy `evm.ExtractGrpcError` function; diverges from live path.
- `telemetry/metrics.go:L390-L405` — `MetricGrpcBdsHardTimeoutTotal`, `MetricGrpcBdsConnReplacementsTotal`.
