# KB: HTTP server, listeners, TLS, timeouts, body limits, gzip

> status: complete
> source-dirs: erpc/http_server.go, erpc/http_timeout.go, erpc/http_batch_resp.go, erpc/healthcheck.go (drain/routing only), erpc/grpc_server.go (port sharing only), erpc/init.go, common/config.go (ServerConfig/TLSConfig/AliasingConfig/CORSConfig/ExecutionHeadersMode), common/defaults.go, common/validation.go, common/tls.go, common/tracing_util.go, common/matcher.go, util/gzip_pool.go, util/reader.go, util/bufpool.go, util/headers.go, util/http_dialer.go

## L1 — Capability (CTO view)

The HTTP server is eRPC's front door: dual-stack (IPv4/IPv6) JSON-RPC ingress with optional TLS (including mTLS), per-request global timeout, request/response gzip, JSON-RPC batch fan-out, domain-based URL aliasing, trusted-proxy client-IP resolution, CORS, and a rich `X-ERPC-*` diagnostic header surface. It guarantees JSON-RPC-spec-compliant transport behavior (POST errors stay HTTP 200 except for a small, deliberate set of transport-level failures) and graceful, drain-aware shutdown for zero-dropped-request deploys.

## L2 — Mechanics (staff-engineer view)

**Handler chain.** `NewHttpServer` composes, innermost to outermost: `createRequestHandler()` → optional `gzipHandler` (response compression) → `TimeoutHandler` (global request deadline = `server.maxTimeout`) → optionally an h2c/gRPC mux on IPv4 when gRPC shares the HTTP port (`erpc/http_server.go:L150-L177`). Two independent `http.Server` instances are created for IPv4 and IPv6 with identical handlers and timeouts (`erpc/http_server.go:L180-L199`); each is only created when its `listenV4`/`listenV6` flag is true, and `Start()` fails if neither exists (`erpc/http_server.go:L1518-L1522`). gRPC sharing only ever applies to the IPv4 server (`handlerV6` stays the plain HTTP handler, `erpc/http_server.go:L158-L177`).

**Request lifecycle.** The top-level handler starts an OTel server span (extracting W3C `traceparent` from request headers, honoring force-trace header/query), installs a once-only `writeFatalError` and panic recovery, then runs `handleRequest` (`erpc/http_server.go:L749-L807`). `handleRequest`: (1) strips the port from `Host` and evaluates `server.aliasing.rules` (first wildcard match on `matchDomain` wins) to preselect project/architecture/chain (`erpc/http_server.go:L232-L257`); (2) sets the always-on response headers (`Content-Type: application/json`, `X-ERPC-Version`, `X-ERPC-Commit`, plus any configured `server.responseHeaders`, `erpc/http_server.go:L259-L266`); (3) parses the URL path against the preselected values (`parseUrlPath`, `erpc/http_server.go:L810-L1006`); (4) routes to healthcheck/admin/proxy; (5) optionally gunzips the request body (`Content-Encoding: gzip`) via a pooled `gzip.Reader` (`erpc/http_server.go:L349-L370`, `util/gzip_pool.go:L30-L50`); (6) reads the whole body into a pooled buffer with **no size cap** (`util.ReadAll(bodyReader, 2048)` — 2048 is only a pre-grow hint, `erpc/http_server.go:L373-L395`, `util/reader.go:L12-L32`); (7) detects a batch purely by `body[0] == '['` (`erpc/http_server.go:L404-L427`); (8) fans out each sub-request to its own goroutine with per-goroutine panic recovery, where validation, forward-header matching, ignore/allow method filters, auth, network resolution (path → or `"networkId"` field in body), directive enrichment from headers/query params, and `project.Forward` happen (`erpc/http_server.go:L440-L676`); (9) after `wg.Wait()`, writes either a streamed batch array (always HTTP 200) or a single response whose status comes from `determineResponseStatusCode` (`erpc/http_server.go:L678-L746`).

**Routing model.** Any request that is not `POST` or `OPTIONS` is a healthcheck (`erpc/http_server.go:L1001-L1003`); a trailing `/healthcheck` path segment also forces healthcheck mode (`erpc/http_server.go:L841-L849`). `/admin` (exactly one segment, POST/OPTIONS only) is the admin endpoint (`erpc/http_server.go:L836-L838`). Everything else resolves `(project, architecture, chainId)` from preselected aliasing values + path segments, including project-scoped network aliases (e.g. `/myproject/arbitrum` resolved via `networksRegistry.ResolveAlias`, `erpc/http_server.go:L860-L866`, `erpc/networks_registry.go:L247-L255`). Architecture must currently be `evm` (`erpc/http_server.go:L997-L999`, `common/network.go:L35-L37`).

**Timeout machinery.** eRPC does NOT use `net/http`'s `TimeoutHandler`; it has its own (`erpc/http_timeout.go:L20-L143`). It wraps the connection in a `timeoutWriter` that buffers the entire response body in a pooled `bytes.Buffer` and stages headers in a private map; only when the inner handler finishes within the deadline does it copy headers + status + buffered body to the real connection. On timeout it writes a JSON-RPC `-32603` "http request handling timeout" body with HTTP 200 for POST (504 for non-POST); on non-timeout context cancellation it writes "request cancelled by client" with HTTP 200 for POST (503 with empty body for non-POST). After timeout, any late `Write` by the inner handler gets `ErrHandlerTimeout` and the buffer is dropped/recycled.

**Response compression.** `gzipHandler` only engages when the client sent `Accept-Encoding: gzip`. The `conditionalGzipWriter` decides on the FIRST write: if the first chunk is < 1024 bytes it never compresses (assumes single-write RPC responses); otherwise it deletes `Content-Length`, sets `Content-Encoding: gzip` + `Vary: Accept-Encoding`, and streams through a pooled `gzip.Writer` (`erpc/http_server.go:L36`, `erpc/http_server.go:L1685-L1780`, `util/gzip_pool.go:L79-L108`). Because the gzip layer sits inside the timeout layer, compressed bytes land in the timeout buffer, so headers/body remain consistent on timeout.

**Error shaping.** All error paths funnel through `processErrorBody`/`buildErrorResponseBody`: execution exceptions are unwrapped, a single-cause `ErrUpstreamsExhausted` is replaced by its only inner error, then `common.TranslateToJsonRpcException` produces a JSON-RPC error object `{code, message[, data]}` (`erpc/http_server.go:L1346-L1455`, `common/json_rpc.go:L1501+`). HTTP status mapping (applied identically in `determineResponseStatusCode` for normal writes and `handleErrorResponse` for early errors): 400 invalid URL/unmarshal/invalid request, 401 auth, 404 project/network not found, 429 rate-limits/capacity, otherwise 200 (`erpc/http_server.go:L1280-L1317`, `erpc/http_server.go:L1470-L1491`). `writeFatalError` (panics, write failures, premature ctx errors) always forces HTTP 200 for POST and emits `{"jsonrpc":"2.0","error":{"code":-32603,"message":...}}` without an `id` field (`erpc/http_server.go:L755-L785`).

**Client IP.** `resolveRealClientIP` trusts forwarding headers only when the direct peer (`RemoteAddr`) is inside `server.trustedIPForwarders` (default loopback only). It then walks `server.trustedIPHeaders` in order, parses each value XFF-style, strips trailing trusted proxies right-to-left, and picks the nearest untrusted IP; otherwise falls back to the socket peer IP (`erpc/http_server.go:L1782-L1905`). The resolved IP feeds auth network-strategy and per-IP rate limiting (`auth/strategy_network.go:L56`, `auth/authorizer.go:L145`).

**Shutdown.** Two goroutines watch the app context: one flips a `draining` flag immediately (healthcheck then returns 503 "shutting down", `erpc/http_server.go:L78-L83`, `erpc/healthcheck.go:L80-L83`); the other sleeps `server.waitBeforeShutdown` (let LB/readiness mark the pod NotReady), then calls `Shutdown` which gracefully drains both listeners with a hardcoded 30s budget (`erpc/http_server.go:L209-L221`, `erpc/http_server.go:L1639-L1683`). After the context is done, `Init` additionally sleeps `server.waitAfterShutdown` before process exit (`erpc/init.go:L172-L177`).

**gRPC port sharing.** When `grpcEnabled` and the gRPC v4 host:port equal the HTTP v4 host:port (which is the default derivation), the IPv4 HTTP handler multiplexes: HTTP/2 + `Content-Type: application/grpc` → in-process gRPC server (bypassing the timeout/gzip chain entirely); everything else → HTTP chain. Without TLS the combined handler is wrapped in h2c to accept cleartext HTTP/2 (`erpc/grpc_server.go:L42-L53`, `erpc/http_server.go:L161-L177`). A standalone gRPC server is started instead when ports differ (`erpc/init.go:L125-L136`).

## L3 — Exhaustive reference (agent view)

### Config fields

All under `server.` unless noted. Types are Go pointer types ⇒ "unset" is distinguishable from zero. `Duration` accepts Go duration strings (`"30s"`); bare integers are interpreted as **milliseconds** (`common/duration.go:L8-L32`).

| # | YAML path | Type | Default | Behavior / notes |
|---|-----------|------|---------|------------------|
| 1 | `server.listenV4` | *bool | `true` — EXCEPT under `go test` where it stays unset unless env `FORCE_TEST_LISTEN_V4=true` (`common/defaults.go:L640-L644`, `util/testing.go:L19-L21`) | Enables the IPv4 `http.Server` (`erpc/http_server.go:L180-L188`). Validation requires host+port when true (`common/validation.go:L70-L79`). |
| 2 | `server.httpHostV4` | *string | `"0.0.0.0"` (`common/defaults.go:L645-L647`) | Bind host for IPv4; combined as `host:port` (`erpc/http_server.go:L1533`). |
| 3 | `server.listenV6` | *bool | **unset** — SetDefaults never sets it; IPv6 is OFF unless explicitly `true` (`common/defaults.go` has no ListenV6 default; `erpc/http_server.go:L191`) | Enables the IPv6 `http.Server` (`erpc/http_server.go:L191-L199`). Validation requires host+port when true (`common/validation.go:L80-L89`). |
| 4 | `server.httpHostV6` | *string | `"[::]"` (`common/defaults.go:L648-L650`) | Bind host for IPv6 (`erpc/http_server.go:L1568`). |
| 5 | `server.httpPort` | *int | mirrors `httpPortV4` | **Deprecated** alias of `httpPortV4`. Exact migration logic in `SetDefaults` (`common/defaults.go:L652-L654`): if `httpPort` is set AND `httpPortV4` is unset, `httpPortV4` is assigned the value of `httpPort`; afterwards `httpPort` is unconditionally overwritten to equal `httpPortV4` (so the two are always in sync after `SetDefaults` runs). Effective default post-migration: `4000`. New configs should use `httpPortV4` directly (`common/config.go:L139`). |
| 6 | `server.httpPortV4` | *int | `4000` (`common/defaults.go:L655-L657`) | IPv4 port. `Start` errors `"server.httpPortV4 is not configured"` if nil while listening (`erpc/http_server.go:L1530-L1532`). |
| 7 | `server.httpPortV6` | *int | `httpPortV4 + 1000` ⇒ `5000` with defaults. Derivation: `HttpPort` is always backfilled from `HttpPortV4` first, so the `*HttpPort+1000` branch always runs; the literal `IntPtr(5000)` fallback at `common/defaults.go:L666-L668` is unreachable (`common/defaults.go:L658-L668`) | IPv6 port (avoids 4001 = default metrics port). |
| 8 | `server.grpcEnabled` | *bool | `false` (`common/defaults.go:L669-L671`) | Enables gRPC transport. With default host/port derivation (rows 9-12) gRPC **shares the HTTP v4 port** via in-handler mux + h2c (`erpc/grpc_server.go:L42-L53`, `erpc/http_server.go:L161-L177`); standalone server otherwise (`erpc/init.go:L125-L136`). |
| 9 | `server.grpcHostV4` | *string | copy of `httpHostV4` (`common/defaults.go:L672-L675`) | gRPC IPv4 host. |
| 10 | `server.grpcPortV4` | *int | copy of `httpPortV4` (`common/defaults.go:L676-L679`) | gRPC IPv4 port — equal to HTTP by default, which is what triggers port sharing. |
| 11 | `server.grpcHostV6` | *string | copy of `httpHostV6` (`common/defaults.go:L680-L683`) | gRPC IPv6 host (no v6 sharing logic exists). |
| 12 | `server.grpcPortV6` | *int | copy of `httpPortV6` (`common/defaults.go:L684-L687`) | gRPC IPv6 port. |
| 13 | `server.grpcMaxRecvMsgSize` | *int | `104857600` (100 MiB) (`common/defaults.go:L688-L690`) | gRPC server max receive message size (`erpc/grpc_server.go:L99`). |
| 14 | `server.grpcMaxSendMsgSize` | *int | `104857600` (100 MiB) (`common/defaults.go:L691-L693`) | gRPC server max send message size (`erpc/grpc_server.go:L100`). |
| 15 | `server.maxTimeout` | *Duration | `150s` (`common/defaults.go:L694-L697`; same fallback hardcoded in `erpc/http_server.go:L65-L68`) | Global per-HTTP-request deadline enforced by the custom `TimeoutHandler` (`erpc/http_server.go:L157`, `erpc/http_timeout.go:L36-L41`). **Required non-zero** by validation: `"server.maxTimeout is required"` (`common/validation.go:L90-L92`). |
| 16 | `server.readTimeout` | *Duration | `30s` (`common/defaults.go:L698-L701`; fallback `erpc/http_server.go:L69-L72`) | `http.Server.ReadTimeout` — covers reading headers+body of a request (`erpc/http_server.go:L183,L194`). |
| 17 | `server.writeTimeout` | *Duration | `120s` (`common/defaults.go:L702-L705`; fallback `erpc/http_server.go:L73-L76`) | `http.Server.WriteTimeout` — covers writing the response (`erpc/http_server.go:L184,L195`). |
| 18 | `server.enableGzip` | *bool | `true` (`common/defaults.go:L706-L708`) | Wraps handler in `gzipHandler` (`erpc/http_server.go:L152-L154`). Note: response compression only; gzipped **request** bodies are always accepted regardless of this flag (`erpc/http_server.go:L349-L370`). |
| 19 | `server.tls.enabled` | bool | `false` (zero value; no SetDefaults for TLSConfig) (`common/config.go:L375-L381`) | When true, both listeners use `ListenAndServeTLS(certFile, keyFile)` with MinVersion TLS 1.2 (`erpc/http_server.go:L1537-L1554`, `L1609-L1637`); also enables TLS creds on shared/standalone gRPC (`erpc/grpc_server.go:L105-L111`); disables h2c on the shared port (`erpc/http_server.go:L174-L176`). |
| 20 | `server.tls.certFile` | string | `""` | PEM cert path; load failure → `"failed to load TLS certificate and key"` (`erpc/http_server.go:L1614-L1618`). |
| 21 | `server.tls.keyFile` | string | `""` | PEM key path (same call). |
| 22 | `server.tls.caFile` | string | `""` | If set on the **server**, enables mandatory mutual TLS: CA pool becomes `ClientCAs` with `ClientAuth = RequireAndVerifyClientCert` (`erpc/http_server.go:L1621-L1633`). |
| 23 | `server.tls.insecureSkipVerify` | bool | `false` | Copied onto the server-side `tls.Config` (`erpc/http_server.go:L1635`). **Server-side effect: none.** The Go TLS stack ignores `InsecureSkipVerify` when operating in server mode — it is a client-dialing flag only. On an inbound TLS listener, client-certificate verification is governed exclusively by `ClientAuth` + `ClientCAs` (i.e. the `server.tls.caFile` field above). This field is meaningful only when the same `TLSConfig` struct is reused by client-side code paths (Redis `data/redis.go:L157`, tracing exporters `common/tracing_core.go:L189,L210` via `common.CreateTLSConfig`, `common/tls.go:L11-L35`). Setting it in a server-only config is a no-op for inbound connections and does NOT relax mTLS client-cert verification. |
| 24 | `server.aliasing.rules` | []*AliasingRuleConfig | `nil` — except when config has **zero projects**, where a default project `main` plus rule `{matchDomain:"*", serveProject:"main"}` is injected (`common/defaults.go:L100-L111`) | Evaluated per request against `Host` (port stripped) in order; first `WildcardMatch` wins (`erpc/http_server.go:L232-L257`). Match errors are logged and the rule skipped. |
| 25 | `server.aliasing.rules[].matchDomain` | string | — | Wildcard/boolean pattern (`*`, `|`, `&`, `!`, parens; `common/matcher.go:L34-L47,L221-L272`) matched against request host. |
| 26 | `server.aliasing.rules[].serveProject` | string | `""` | Preselected projectId (`erpc/http_server.go:L251`). |
| 27 | `server.aliasing.rules[].serveArchitecture` | string | `""` | Preselected architecture (`erpc/http_server.go:L252`). When set **without** `serveProject`, the arch-only case applies (`erpc/http_server.go:L963-L983`): 0 path segments is only valid for a healthcheck; 1 segment = project; 2 segments = project + chain; 3 segments = explicit project/arch/chain override; >3 segments → `ErrInvalidUrlPath`. Setting `serveArchitecture` alone is a valid aliasing pattern (e.g. a host dedicated to EVM chains) but requires the project to always be supplied in the URL path or via another segment. Combining `serveArchitecture` with `serveChain` but **without** `serveProject` triggers the `default` switch fall-through and produces `ErrInvalidUrlPath "invalid combination of path elements and/or aliasing rules"` (`erpc/http_server.go:L985-L990`). |
| 28 | `server.aliasing.rules[].serveChain` | string | `""` | Preselected chainId (`erpc/http_server.go:L253`). Combination project+chain WITHOUT architecture is rejected at request time (`erpc/http_server.go:L985-L990`). |
| 29 | `server.waitBeforeShutdown` | *Duration | `10s` (`common/defaults.go:L709-L712`) | Sleep between appCtx cancellation and starting HTTP `Shutdown` — lets readiness probes fail first (`erpc/http_server.go:L209-L221`). |
| 30 | `server.waitAfterShutdown` | *Duration | `10s` (`common/defaults.go:L713-L716`) | Sleep in `Init` after appCtx done, before process exit (`erpc/init.go:L172-L177`). |
| 31 | `server.includeErrorDetails` | *bool | `true` (`common/defaults.go:L717-L719`) | When true, error responses get `error.data` = full original error object — EXCEPT for `eth_call` (clients expect string revert data) and except when upstream already supplied `data` (`erpc/http_server.go:L1424-L1433`). Many early/client-side error paths force `true` regardless (`&common.TRUE` at e.g. `erpc/http_server.go:L279,L337,L363,L391,L420,L473`). |
| 32 | `server.trustedIPForwarders` | []string | `["127.0.0.1/8", "::1/128"]` (`common/defaults.go:L725-L729`) | IPs/CIDRs of proxies whose forwarding headers are trusted (`erpc/http_server.go:L98-L121`, `L1810-L1825`). Validated as IP or CIDR (`common/validation.go:L94-L109`); invalid entries at runtime are warned+ignored (`erpc/http_server.go:L111,L117`). |
| 33 | `server.trustedIPHeaders` | []string | `[]` — no headers trusted by default (`common/defaults.go:L730-L733`) | Ordered list of header names (e.g. `X-Forwarded-For`, `CF-Connecting-IP`) parsed XFF-style for the real client IP, only when the peer is a trusted forwarder (`erpc/http_server.go:L123-L133`, `L1782-L1808`). RFC 7239 `Forwarded` is NOT supported (parser exists but is dead code, `erpc/http_server.go:L1856-L1881`). |
| 34 | `server.responseHeaders` | map[string]string | `nil` | Static custom response headers added to EVERY response. Values are env-expanded once at startup via `os.ExpandEnv` (supports both `${VAR}` and `$VAR` syntax). **Empty-after-expansion values are silently dropped** — if `$MY_VAR` is unset or empty at startup time, the corresponding header is never emitted for any request; there is no warning, only a Debug log `"custom response header skipped (empty value after env expansion)"` (`erpc/http_server.go:L135-L148`). Expansion happens at `NewHttpServer` construction time, not per-request. Headers that expand to non-empty are logged at Info (`L143`). Set per-request at `erpc/http_server.go:L263-L266`. |
| 35 | `server.executionHeaders` | *ExecutionHeadersMode | `"all"` (`common/defaults.go:L720-L723`; nil-safe getter `erpc/http_server.go:L1081-L1086`) | `"all"` = counters + metadata + per-attempt `X-ERPC-Upstreams` log; `"summary"` = counters + metadata only; `"off"` = no `X-ERPC-*` diagnostic headers at all (`common/config.go:L170-L184`, `erpc/http_server.go:L1105-L1127`). |
| 36 | `healthCheck.mode` | HealthCheckMode | `"networks"` (`common/defaults.go:L738-L741`) | `simple` (plain `OK` text) / `networks` / `verbose` JSON shapes (`erpc/healthcheck.go:L337-L396`). Healthcheck internals are covered by the healthcheck KB; listed here because `NewHttpServer` consumes it. |
| 37 | `healthCheck.auth` | *AuthConfig | `nil` | When set, a dedicated auth registry guards healthcheck endpoints (`erpc/http_server.go:L201-L207`, `erpc/healthcheck.go:L85-L105`). |
| 38 | `healthCheck.defaultEval` | string | `"any:initializedUpstreams"` (`common/defaults.go:L742-L744`) | Default eval strategy when `?eval=` query param absent (`erpc/healthcheck.go:L106-L112`). |
| 39 | `projects[].cors` / `admin.cors` → `allowedOrigins` | []string | `["*"]` (`common/defaults.go:L2824-L2827`) | Wildcard-matched against `Origin` (`erpc/http_server.go:L1022-L1033`). CORS is configured per-project (and on admin), but enforced by this HTTP server. |
| 40 | `cors.allowedMethods` | []string | `["GET","POST","OPTIONS"]` (`common/defaults.go:L2828-L2830`) | Joined into `Access-Control-Allow-Methods` (`erpc/http_server.go:L1055`). |
| 41 | `cors.allowedHeaders` | []string | `["content-type","authorization","x-erpc-secret-token"]` (`common/defaults.go:L2831-L2837`) | Joined into `Access-Control-Allow-Headers` (`erpc/http_server.go:L1056`). |
| 42 | `cors.exposedHeaders` | []string | `nil` | Joined into `Access-Control-Expose-Headers` (`erpc/http_server.go:L1057`). |
| 43 | `cors.allowCredentials` | *bool | `false` (`common/defaults.go:L2838-L2840`) | Emits `Access-Control-Allow-Credentials: true` only when true (`erpc/http_server.go:L1059-L1061`). |
| 44 | `cors.maxAge` | int | `3600` (`common/defaults.go:L2841-L2843`) | Emits `Access-Control-Max-Age` when > 0 (`erpc/http_server.go:L1063-L1065`). |

Hardcoded (non-configurable) server constants:

- `IdleTimeout` = 300s on both servers (`erpc/http_server.go:L185,L196`).
- `MaxHeaderBytes` = 1 MiB (`1<<20`) — header bytes hard cap; **the only hard inbound size limit** (`erpc/http_server.go:L186,L197`).
- **No request-body size limit** — there is no `http.MaxBytesReader` or `Content-Length` guard anywhere in the request path; `util.ReadAll` copies until EOF with no upper bound (`util/reader.go:L12-L32`; see also edge-case #3). The 2048-byte argument is a pre-grow hint, not a cap. Effective limits are solely `readTimeout` and process memory. Body-read pre-grow hint = 2048 bytes; pooled buffers start at 64 KiB and are discarded back to GC above 256 KiB (`erpc/http_server.go:L374`, `util/bufpool.go:L8-L32`); pre-grow is capped at 200 MB (`util/reader.go:L7`).
- Graceful shutdown budget = 30s (`erpc/http_server.go:L1642`).
- Response compression threshold = 1024 bytes on the first write (`erpc/http_server.go:L36`).
- TLS `MinVersion` = TLS 1.2 (`erpc/http_server.go:L1610-L1612`).

### Behaviors & algorithms

**Startup & listen (IPv4/IPv6).**
- `Start` requires ≥1 server: error `"you must configure at least one of server.listenV4 or server.listenV6"` (`erpc/http_server.go:L1518-L1522`).
- Per family: build `host:port`, optionally build TLS config, run `ListenAndServe[TLS]` in a goroutine; first non-`ErrServerClosed` error aborts `Start` (`erpc/http_server.go:L1528-L1605`). `Init` exits the process with `ExitCodeHttpServerFailed` on such errors (`erpc/init.go:L111-L124`).
- Under `go test`, `listenV4` is NOT defaulted to true (avoids port binding in unit tests) — fixtures bind `127.0.0.1:0` manually and call `serverV4.Serve(listener)` (`common/defaults.go:L640-L644`, `erpc/http_server_test.go:L7794-L7826`).

**URL path parsing & aliasing precedence** (`parseUrlPath`, `erpc/http_server.go:L810-L1006`; tests `erpc/http_server_test.go:L3378-L3710`):
1. `path.Clean` the URL path, split on `/`, drop the leading empty segment (`L821-L825`).
2. `/admin` with exactly 1 segment and method POST/OPTIONS → `isAdmin` (`L836-L838`).
3. Trailing `healthcheck` segment → `isHealthCheck`, segment removed (`L841-L843`); empty path with non-POST/OPTIONS method → healthcheck (`L844-L849`).
4. Aliasing preselects `(project, arch, chain)`; remaining segments fill the gaps. Case analysis:
   - Nothing preselected: 1 seg = project; 2 segs = project + (network-alias | architecture); 3 segs = project/arch/chain; ≥4 → `ErrInvalidUrlPath` "must only provide ..." (`L853-L877`).
   - Project preselected: 1 seg = network-alias-or-architecture; 2 segs = arch/chain; 3 segs may still explicitly override everything (`L879-L904`).
   - Project+arch preselected: 1 seg = network-alias (must resolve both) else chainId; 3 segs override (`L906-L935`).
   - All preselected: only `/` (or healthcheck) allowed; >1 segment → error (`L937-L941`).
   - Arch+chain preselected: 1 seg = project; 3 segs override (`L943-L961`).
   - Arch-only preselected: 0 segs allowed only for healthcheck; 1 seg = project; 2 segs = project+chain; 3 segs override (`L963-L983`).
   - Project+chain WITHOUT arch preselected → hard error "it is not possible to alias for project and chain WITHOUT architecture" (`L985-L988`); any other combination → "invalid combination of path elements and/or aliasing rules" (`L989-L990`).
5. Post-checks: project required unless healthcheck (`L993-L995`); if arch or chain present, arch must be `evm` ("architecture is not valid (must be 'evm')", `L997-L999`); finally ANY non-POST/non-OPTIONS method forces `isHealthCheck = true` (`L1001-L1003`).
6. Network aliases (per-project `networks[].alias`) resolve one path segment to `(architecture, chainId)` (`erpc/networks_registry.go:L247-L255`).
7. If arch/chain are still unknown for a POST, the per-request handler falls back to a top-level `"networkId":"evm:42161"` field inside the JSON-RPC body (`erpc/http_server.go:L613-L634`); if still unresolved → 400 `ErrInvalidRequest` with guidance text (`L636-L642`).

**Batch handling** (`erpc/http_server.go:L404-L427`, `L703-L720`; `erpc/http_batch_resp.go`):
- Batch iff first body byte is `[`. Batch unmarshal failure → single `ErrJsonRpcRequestUnmarshal` response (HTTP 400).
- Each element runs in its own goroutine; results land in a positional slice; batch write streams `[`, comma-joined elements, `]` without buffering the whole array (`erpc/http_batch_resp.go:L21-L79`).
- Batch transport status is ALWAYS 200 (`erpc/http_server.go:L704`), and per-response `X-ERPC-*` headers are NOT emitted (no `setResponseHeaders` call on the batch path).
- Per-element error encoding: `*HttpJsonRpcErrorResponse` via hand-rolled writer (`erpc/http_batch_resp.go:L82-L130`); bare `error` values become `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,...}}` (`erpc/http_batch_resp.go:L45-L55`); unknown types are sonic-marshaled.

**Per-request pipeline order** (single goroutine per sub-request, `erpc/http_server.go:L440-L675`):
1. Panic recovery → error slot + `erpc_unexpected_panic_total{scope="request-handler"}` (`L443-L458`).
2. `NewNormalizedRequest`, client IP resolution + `SetClientIP` (`L460-L469`).
3. `nq.Validate()` — body present + `method` non-empty (`common/request.go:L1123-L1142`); failure → error response (forced details).
4. Project `forwardHeaders` wildcard-matched against incoming headers → `nq.ForwardHeaders` (`L478-L494`).
5. `ignoreMethods` then `allowMethods` wildcard filters (allow re-enables ignored methods); blocked → JSON-RPC error `-32601` `"method not supported: <m>"` (`L499-L550`, code constant `common/errors.go:L2160`).
6. Auth payload from headers/query (`auth.NewPayloadFromHttp`: `?token=` deprecated, `?secret=`, `X-ERPC-Secret-Token`, `Authorization: Basic|Bearer`, `?jwt=`, `?signature=&message=`, `X-Siwe-*` — `auth/http.go:L13+`), then admin auth or project consumer auth (`L552-L584`).
7. Admin requests → `AdminHandleRequest`; "admin is not enabled for this project" `ErrAuthUnauthorized` when `admin` config missing (`L586-L611`).
8. Network resolution (path or body `networkId`) → `project.GetNetwork` → `nq.SetNetwork` (`L613-L650`).
9. `nq.ApplyDirectiveDefaults(network.directiveDefaults)` then `nq.EnrichFromHttp(headers, queryArgs, uaMode)` — request directives from `X-ERPC-*` headers/query params; UA tracking mode from `project.userAgentMode` (default `simplified`) (`L652-L659`; directive header list `common/request.go:L39-L61` — covered by the directives KB).
10. `project.Forward` → response or error; on error any produced response is `Release()`d asynchronously (`L661-L674`).

**Response write (single).** Order matters because headers seal at `WriteHeader`:
1. `InjectHTTPResponseTraceContext` (adds `traceparent` to response when tracing enabled, `erpc/http_server.go:L701`, `common/tracing_util.go:L58-L65`).
2. `setResponseHeaders` (X-ERPC-* diagnostics; below) (`L723`).
3. `determineResponseStatusCode` (`L727-L728`).
4. Body: `NormalizedResponse.WriteTo` (zero-copy upstream bytes) / `writeJsonRpcError` / sonic-encode fallback (`L730-L738`). Write failure → `writeFatalError` (`L740-L742`).
If the request context is already done before writing: non-cancel/deadline causes get a fatal 500 (200 for POST) write; responses are released; cancel/deadline exit silently (timeout layer already responded) (`L683-L699`).

**HTTP status mapping** (both `determineResponseStatusCode` `erpc/http_server.go:L1280-L1317` and `handleErrorResponse` `L1470-L1491`):

| Status | Error codes |
|--------|-------------|
| 400 | `ErrInvalidUrlPath`, `ErrJsonRpcRequestUnmarshal`, `ErrInvalidRequest` |
| 401 | `ErrAuthUnauthorized`, `ErrEndpointUnauthorized` |
| 404 | `ErrProjectNotFound`, `ErrNetworkNotFound`, `ErrNetworkNotSupported` |
| 429 | `ErrAuthRateLimitRuleExceeded`, `ErrProjectRateLimitRuleExceeded`, `ErrNetworkRateLimitRuleExceeded`, `ErrEndpointCapacityExceeded` |
| 200 | everything else (JSON-RPC application errors stay 200) |

Note these apply to POST too — only `writeFatalError` and the timeout layer force 200-for-POST (`erpc/http_server.go:L770-L773`, `erpc/http_timeout.go:L109-L112,L125-L128`).

**Error response shapes.**
- Standard JSON-RPC error (single): `{"jsonrpc":"<ver>","id":<id|null>,"error":{"code":<int>,"message":"<msg>"[,"data":<any>]}}` — `HttpJsonRpcErrorResponse` with `Cause`/`Request` excluded from JSON (`erpc/http_server.go:L1319-L1328`, built `L1390-L1455`).
- Fatal-path body (panic/write-failure): `{"jsonrpc":"2.0","error":{"code":-32603,"message":<json-string>}}` — **no `id` field** (`erpc/http_server.go:L776-L782`).
- Timeout body: `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"http request handling timeout"}}`; status 200 (POST) / 504 (other) (`erpc/http_timeout.go:L107-L122`; test `erpc/http_timeout_test.go:L31-L53`).
- Client-cancel body (POST only): `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"request cancelled by client"}}`; status 200 (POST) / 503 + empty body (other) (`erpc/http_timeout.go:L123-L141`; test `erpc/http_timeout_test.go:L70-L119`).
- Rare fallback: when `processErrorBody` receives an error that is neither a `*common.BaseError` nor a `common.StandardError` after all unwrapping, it wraps it in `common.BaseError{Code: "ErrUnknown", Message: "unexpected server error", Cause: err}` (`erpc/http_server.go:L1450-L1454`). The resulting wire body is **not JSON-RPC shaped**: it is sonic-encoded as `{"code":"ErrUnknown","message":"unexpected server error","cause":{...}}` (the `BaseError` struct, not `{"jsonrpc":"2.0","error":{...}}`). `"ErrUnknown"` is a pseudo-code string constant — it has no numeric JSON-RPC error code and no corresponding `ErrCode` int constant in `common/errors.go`. Callers that parse `response.error.code` as an integer will fail to match it; matching must be done on the `code` string field of the outer object.
- Healthcheck while draining: HTTP 503, plain-text `"shutting down"` via `http.Error` (`erpc/healthcheck.go:L80-L83`).
- `includeErrorDetails` adds `error.data = <original error object>` except for `eth_call` (`erpc/http_server.go:L1424-L1433`); a single-cause `ErrUpstreamsExhausted` is unwrapped to that cause first (`L1399-L1403`); `ErrEndpointExecutionException` is surfaced preferentially (`L1391-L1395`).

**Timeout handler algorithm** (`erpc/http_timeout.go:L36-L143`):
1. Wrap request ctx with `context.WithTimeoutCause(dt, ErrHandlerTimeout)`.
2. Run inner handler in goroutine against a `timeoutWriter` (pooled buffer body, staged header map, recorded status). Panics are counted (`scope="timeout-handler"`), forwarded over a channel, and re-panicked on the serving goroutine (test `erpc/http_timeout_test.go:L145-L158`).
3. `select`: inner-done → if ctx already errored, drop everything silently; else copy staged headers, status (default 200), and buffered body to the real writer; write failures are logged (client disconnects at Debug via `common.IsClientDisconnect`, `common/errors.go:L129-L150`).
4. ctx-done → distinguish `context.Cause`: `DeadlineExceeded`/`ErrHandlerTimeout` → timeout body; anything else → cancel body. In both branches `tw.err` is set so late inner writes fail with `ErrHandlerTimeout` (`L170-L180`), and the buffer is returned to the pool.
5. `timeoutWriter` extras: `Push` delegates HTTP/2 push or `ErrNotSupported` (`L161-L166`); second `WriteHeader` is a Trace-level "superfluous" log, first status wins (`L182-L200`).

**Response gzip algorithm** (`erpc/http_server.go:L1685-L1780`):
1. Skip entirely unless request `Accept-Encoding` contains `gzip` (`L1763-L1766`).
2. First `Write` decides once: `len(firstChunk) < 1024` → passthrough forever; else delete `Content-Length`, set `Content-Encoding: gzip` + `Vary: Accept-Encoding` (+ `Content-Type: application/json` if missing), and route through pooled `gzip.Writer` (`L1699-L1735`).
3. `Flush` flushes gzip then the underlying writer (`L1738-L1746`); handler-level `Close` finalizes the gzip stream and re-pools the writer (`L1748-L1755`, called `L1778`).
4. Pools: `GzipWriterPool.Put` resets onto `io.Discard` to drop writer refs (`util/gzip_pool.go:L101-L108`); `GzipReaderPool.Put` resets onto an EOF flate reader to avoid bufio re-alloc (`util/gzip_pool.go:L43-L50`).

**Request gzip decompression** (`erpc/http_server.go:L349-L370`): exact header match `Content-Encoding: gzip`; pooled reader; invalid gzip → 400 `ErrInvalidRequest` "invalid gzip body"; reader returned to pool after body read.

**Client IP resolution** (`erpc/http_server.go:L1782-L1905`):
- `parseRemoteIP` splits host:port, strips IPv6 brackets; unparseable → `"n/a"`.
- Peer not in `trustedIPForwarders` → peer IP is the client.
- Else for each `trustedIPHeaders` name in order: parse value as comma-separated XFF (handles brackets, optional ports), trim trusted entries from the RIGHT, return the nearest untrusted hop; if every entry is trusted → next header → ultimately fall back to peer IP (`L1884-L1896`).

**X-ERPC diagnostic headers** (single responses only; `setResponseHeaders` `erpc/http_server.go:L1105-L1127`; tests `erpc/http_server_exec_headers_test.go`, `erpc/http_server_headers_test.go`):
- Group 1 — counters, emitted whenever mode ≠ `off`, zero-filled if no ExecState (nil-safe `Snapshot`, `L1149-L1184`): `X-ERPC-Attempts` (= UpstreamAttempts + CacheAttempts; NetworkAttempts deliberately excluded as a rotation count — `erpc/http_server_headers_test.go:L90-L97`), `X-ERPC-Upstream-Attempts`, `X-ERPC-Upstream-Retries`, `X-ERPC-Upstream-Hedges`, `X-ERPC-Network-Attempts`, `X-ERPC-Network-Retries`, `X-ERPC-Network-Hedges`; conditional: `X-ERPC-Cache-Attempts/-Retries/-Hedges` (only when cache scope exercised), `X-ERPC-Consensus-Slots`, `X-ERPC-Consensus-Disputes`, `X-ERPC-Consensus-Low-Participants` (only when > 0).
- Group 2 — response metadata (`L1189-L1204`): `X-ERPC-Cache: HIT|MISS`, `X-ERPC-Upstream: <winning upstream id>`, `X-ERPC-Duration: <milliseconds>` (NormalizedResponse only). Works on error paths too via `HttpJsonRpcErrorResponse.Cause` metadata lookup (`L1209-L1224`).
- Group 3 — participation log, mode `all` only (`L1124-L1126`, `L1239-L1272`): `X-ERPC-Upstreams: <id>=<reason>:<outcome>:<duration>ms[:won][;...]` — e.g. `alchemy=primary:success:50ms:won;quicknode=hedge:timeout:5000ms`.
- Emission happens BEFORE `WriteHeader` on the error path too (`L1492-L1497`).

**Complete response-header inventory** (everything the server can set):

| Header | When | Source |
|--------|------|--------|
| `Content-Type: application/json` | every proxied/admin/healthcheck-error response, set up-front; re-set for JSON healthcheck modes; gzip layer backfills if missing | `erpc/http_server.go:L259`, `erpc/healthcheck.go:L366`, `erpc/http_server.go:L1726-L1728` |
| `X-ERPC-Version` | always | build-time ldflags var, default `"dev"` (`erpc/http_server.go:L260`, `common/config.go:L23-L26`) |
| `X-ERPC-Commit` | always | default `"none"` (`erpc/http_server.go:L261`, `common/config.go:L28-L31`) |
| custom `server.responseHeaders` | always | `erpc/http_server.go:L263-L266` |
| `Access-Control-Allow-Origin` (echoes origin), `-Allow-Methods`, `-Allow-Headers`, `Access-Control-Expose-Headers` | allowed CORS origin only | `erpc/http_server.go:L1054-L1057` |
| `Access-Control-Allow-Credentials: true` | allowed origin + `allowCredentials` | `erpc/http_server.go:L1059-L1061` |
| `Access-Control-Max-Age` | allowed origin + `maxAge>0` | `erpc/http_server.go:L1063-L1065` |
| `traceparent` (+`tracestate`) | tracing enabled | `common/tracing_util.go:L58-L65` |
| `X-ERPC-Attempts`, `X-ERPC-Upstream-Attempts/-Retries/-Hedges`, `X-ERPC-Network-Attempts/-Retries/-Hedges` | single responses, mode ≠ off | `erpc/http_server.go:L1158-L1166` |
| `X-ERPC-Cache-Attempts/-Retries/-Hedges` | cache scope exercised | `erpc/http_server.go:L1170-L1174` |
| `X-ERPC-Consensus-Slots/-Disputes/-Low-Participants` | consensus exercised | `erpc/http_server.go:L1175-L1183` |
| `X-ERPC-Cache: HIT\|MISS`, `X-ERPC-Upstream`, `X-ERPC-Duration` | response metadata available | `erpc/http_server.go:L1189-L1204` |
| `X-ERPC-Upstreams` | mode == all, ≥1 attempt | `erpc/http_server.go:L1239-L1252` |
| `Content-Encoding: gzip`, `Vary: Accept-Encoding` | compressing | `erpc/http_server.go:L1723-L1725` |

**Request headers / query params with server-level effects:**
- `Host` — aliasing match (port stripped) (`erpc/http_server.go:L232-L236`).
- `Content-Encoding: gzip` — request body decompression (`L349-L370`).
- `Accept-Encoding: gzip` — response compression eligibility (`L1763`).
- `Content-Type: application/grpc` over HTTP/2 — gRPC mux on shared port (`L167-L173`). **Important**: when this path is taken, the request is handed directly to `sharedGrpcServer.server.ServeHTTP` and bypasses the `TimeoutHandler` and `gzipHandler` entirely (`erpc/http_server.go:L167-L173`). gRPC uses its own per-RPC deadline; response compression from eRPC's HTTP layer is not applied.
- `Origin` — CORS evaluation; absent Origin bypasses CORS entirely (`L1008-L1018`).
- `X-ERPC-Force-Trace: true|1|yes` or `?force-trace=true|1|yes` — force-sample this trace (`common/tracing_util.go:L106-L119`, constants `common/tracing_core.go:L29-L31`).
- `traceparent`/`tracestate` — W3C context extraction (`common/tracing_util.go:L67-L73`).
- Auth: `?token=` (deprecated), `?secret=`, `X-ERPC-Secret-Token`, `Authorization: Basic <b64 user:pass>` (password = secret), `Authorization: Bearer <jwt>`, `?jwt=`, `?signature=&message=` (SIWE), `X-Siwe-Message`/`X-Siwe-Signature` (`auth/http.go:L13+`).
- `?eval=` — healthcheck strategy override (`erpc/healthcheck.go:L106-L112`).
- Directive headers/query params (`X-ERPC-Retry-Empty`, `X-ERPC-Skip-Cache-Read`, `X-ERPC-Use-Upstream`, etc.) — consumed in `nq.EnrichFromHttp` (`erpc/http_server.go:L658`, list at `common/request.go:L39-L61`; details in the directives KB).
- Project `forwardHeaders` patterns — matched headers copied upstream (`erpc/http_server.go:L478-L494`).
- `trustedIPHeaders`-configured headers — client IP (`L1795-L1805`).

### Observability

**Prometheus metrics fired by this layer** (definitions in `telemetry/metrics.go`):
- `erpc_unexpected_panic_total{scope, extra, error}` (`telemetry/metrics.go:L13-L17`) — scopes used here: `"request-handler"` (per-request goroutine, extra=`project:<arch> network:<chain>`, `erpc/http_server.go:L446-L450`), `"final-error-writer"` (extra=`statusCode:<n>`, `L759-L763`), `"top-level-handler"` (`L789-L793`), `"timeout-handler"` (`erpc/http_timeout.go:L53-L58`), `"validate-pattern"` (wildcard engine, `common/matcher.go:L122-L127`).
- `erpc_cors_requests_total{project, origin}` — every request carrying an `Origin` header; **the `project` label actually receives `r.URL.Path`** (`erpc/http_server.go:L1020`, `telemetry/metrics.go:L610-L614`).
- `erpc_cors_preflight_requests_total{project, origin}` — allowed-origin OPTIONS preflights (`erpc/http_server.go:L1069`, `telemetry/metrics.go:L616-L620`).
- `erpc_cors_disallowed_origin_total{project, origin}` — origin matched no allowlist entry (`erpc/http_server.go:L1038`, `telemetry/metrics.go:L622-L626`).
- There is NO generic `http_requests_total`-style server metric; request accounting happens at network/upstream layers.

**Trace spans:** `Http.ReceivedRequest` (SpanKind=server; attrs `http.method`, `http.url`, `http.scheme`, `http.user_agent`; force-trace attrs, `common/tracing_util.go:L67-L104`); `Request.Handle` per sub-request (`common/tracing_util.go:L140-L188`, started `erpc/http_server.go:L465`); detail spans `Http.ReadBody` (`L373`), `Http.ParseRequests` (`L397`), `HttpServer.WriteResponse` (`L680`); span events `http.response_write_start` / `http.response_write_end{response_status}` on the error-write path (`L1498-L1515`); status/code enrichment via `EnrichHTTPServerSpan` (≥400 → Error status, `common/tracing_util.go:L121-L138`).

**Notable log lines:** `"received http request"` at **Info with the full raw JSON body** (`erpc/http_server.go:L398-L402`); `"entering draining mode → healthcheck will fail"` (`L82`); `"starting IPv4/IPv6 HTTP server on <addr>"` (`L1534,L1569`); `"TLS enabled for IPv4/IPv6 HTTP server"` (`L1543,L1578`); `"custom response header configured"` (`L143`); `"invalid CIDR/IP in trusted forwarders; ignoring"` (`L111,L117`); `"http server forced to shutdown"` / `"http server stopped"` (`L217-L219`); `"IPv4/IPv6 HTTP server stopped"` (`L1655,L1668`); `"failed to write batch response"` (`L715`); `"client disconnected while writing response"` (Debug, `erpc/http_timeout.go:L93-L95`); `"context canceled before writing response"` (Debug, `L76`); `"http: superfluous response.WriteHeader call"` (Trace, `L188`); `"CORS request from disallowed origin..."` (Debug, `erpc/http_server.go:L1037`).

### Edge cases & gotchas

1. **Unsupported-method responses always have `"id": null`** — the JSON-RPC id/version copy is guarded by an inverted `err != nil` check that never fires after `Validate()` (and would nil-deref if it did) (`erpc/http_server.go:L532-L537`).
2. **Any non-POST/non-OPTIONS request is a healthcheck**, including `GET /myproject/evm/1` (network-scoped health) and `GET /admin` (treated as project "admin" healthcheck → project-not-found) (`erpc/http_server.go:L1001-L1003`, `L836-L838`; test "Root healthcheck GET" `erpc/http_server_test.go:L3424-L3434`).
3. **No request body size limit** — only the 1 MiB header cap; a multi-GB body is read fully into memory (`util/reader.go:L12-L32`, `erpc/http_server.go:L186`). `expected=2048` in `ReadAll` is a pre-grow hint, not a cap.
4. **Batch detection is byte-exact**: leading whitespace before `[` makes the body parse as a single (invalid) request (`erpc/http_server.go:L405`).
5. **Batch responses are always HTTP 200 and carry no X-ERPC-* diagnostic headers** — `setResponseHeaders` only runs on the single-response path (`erpc/http_server.go:L703-L720` vs `L721-L728`).
6. **POST can still get non-200**: 400/401/404/429 transport mappings apply to JSON-RPC POSTs (`erpc/http_server.go:L1470-L1491`); only fatal-writer and timeout/cancel paths force 200-for-POST (`L770-L773`, `erpc/http_timeout.go:L109-L112`).
7. **Disallowed CORS origins are not blocked** for non-OPTIONS requests — the request proceeds without `Access-Control-*` headers (browser enforces); preflight gets a bare 204 (`erpc/http_server.go:L1036-L1051`; test `erpc/http_server_test.go:L3332+`). No `Origin` header ⇒ CORS skipped entirely (`L1010-L1018`).
8. **OPTIONS to a project without CORS config** falls through to normal request handling (empty body → JSON-RPC error) because the OPTIONS early-return lives inside the `CORS != nil` branch (`erpc/http_server.go:L343-L347`).
9. **`listenV6` has no default** — IPv6 must be opted into; `listenV4` defaults true except under `go test` (gated by `FORCE_TEST_LISTEN_V4`) (`common/defaults.go:L640-L650`).
10. **`httpPortV6` default is derived** as `httpPortV4 + 1000` (the literal 5000 fallback is unreachable because `httpPort` is always backfilled first) (`common/defaults.go:L651-L668`).
11. **`grpcEnabled: true` with default ports silently shares the HTTP IPv4 port** (gRPC host/port default-copy from HTTP), bypassing maxTimeout/gzip for gRPC traffic and adding h2c when TLS is off; sharing never applies to IPv6 (`erpc/grpc_server.go:L42-L53`, `erpc/http_server.go:L158-L177`).
12. **Whole responses are buffered in memory** by the timeout writer before hitting the socket — large responses cost RAM and `writeTimeout` only starts mattering at final copy (`erpc/http_timeout.go:L43-L48,L81-L91`); pooled buffers above 256 KiB are discarded to GC (`util/bufpool.go:L24-L32`).
13. **Gzip decision is first-write-only**: a response whose first chunk is < 1024 bytes is never compressed even if later writes are huge (`erpc/http_server.go:L1708-L1717`).
14. **Done/canceled race drops the response silently**: if the inner handler finishes but the request ctx errored, nothing is written at all (`erpc/http_timeout.go:L72-L80`).
15. **mTLS is implicit**: setting `server.tls.caFile` flips client auth to `RequireAndVerifyClientCert` — all clients must then present certs (`erpc/http_server.go:L1621-L1633`).
16. **RFC 7239 `Forwarded` is dead code** — only headers named in `trustedIPHeaders` are consulted, parsed XFF-style (`erpc/http_server.go:L1856-L1881` defined but never called).
17. **If every hop in the forwarded chain is trusted, the peer IP wins** (right-trim returns nil → fallback) (`erpc/http_server.go:L1884-L1896`, `L1799-L1806`).
18. **Full request bodies are logged at Info level** — sensitive params land in logs at default log level (`erpc/http_server.go:L398-L402`).
19. **`server.responseHeaders` values are env-expanded once at startup**; values that expand to empty string are silently dropped — the header is never set on any response and no warning is emitted (`erpc/http_server.go:L135-L148`). This is a common production footgun: if a CI/CD pipeline sets a custom header via an env var that is absent in a new deployment environment, the header disappears entirely with no observable error.
20. **Counter headers are zero-filled, not omitted**: even early errors (URL parse, project lookup) emit `X-ERPC-Attempts: 0` etc., because `writeCounterHeaders` runs on a nil-safe snapshot (`erpc/http_server.go:L1149-L1166`); only `executionHeaders: off` removes them.
21. **`X-ERPC-Attempts` ≠ sum of all attempts**: NetworkAttempts is excluded from the total to avoid double-counting rotations (`erpc/http_server.go:L1151-L1157`; asserted in `erpc/http_server_headers_test.go:L90-L97`).
22. **`error.data` is suppressed for `eth_call`** even with `includeErrorDetails: true` (clients expect string revert data) (`erpc/http_server.go:L1427-L1433`); many early error paths force details on regardless of config (`&common.TRUE` call sites, e.g. `L279,L337,L363`).
23. **`maxTimeout` is mandatory**: explicit `0` (or a config path that skips SetDefaults) fails validation with "server.maxTimeout is required" (`common/validation.go:L90-L92`).
24. **Zero-project configs self-alias**: a default `main` project plus `matchDomain:"*" → serveProject:"main"` rule is injected so `/evm/123` works without a project segment (`common/defaults.go:L100-L111`).
25. **CORS metric label mismatch**: the label named `project` is populated with the URL path, not the project id (`erpc/http_server.go:L1020,L1038,L1069`).
26. **Per-request goroutine panics produce a per-slot error response** (batch keeps other slots intact); top-level panics produce the fatal `-32603` body via once-only writer (`erpc/http_server.go:L443-L458`, `L787-L804`); timeout-handler panics re-panic on the serving goroutine (crash-loud) after metric increment (`erpc/http_timeout.go:L50-L67`; test `erpc/http_timeout_test.go:L145-L158`).
27. **Sub-request handling for unsupported/ignored methods, auth failures etc. still consumes a batch slot with a well-formed JSON-RPC error**, so batch positions always align with request positions (`erpc/http_server.go:L429-L675`, `erpc/http_batch_resp.go:L29-L74`).
28. **Sonic encoder writes a trailing newline** (`encoder.Encode`) on the early-error path, and HTML escaping is disabled globally (`erpc/http_server.go:L229-L230`, `common/sonic.go:L48-L58`).
29. **`Duration` integer YAML values are milliseconds**, not seconds (`common/duration.go:L20-L31`) — `maxTimeout: 150` means 150 ms.
30. **`server.tls.insecureSkipVerify` has no effect on inbound TLS connections** — `tls.Config.InsecureSkipVerify` is a client-mode flag; the Go `net/tls` server ignores it entirely. Inbound client-cert verification is controlled only by `ClientAuth`/`ClientCAs` (see `caFile`). This field only matters when the same `TLSConfig` struct is reused for outbound connections (Redis, tracing exporters) via `common.CreateTLSConfig` (`common/tls.go:L11-L35`). Setting it on a server-only deployment produces a misleading false sense of relaxed security (`erpc/http_server.go:L1635`).
31. **`ErrUnknown` fallback body is not JSON-RPC shaped** — `processErrorBody`'s last resort (`erpc/http_server.go:L1450-L1454`) produces `common.BaseError{Code:"ErrUnknown"}`, sonic-encoded as `{"code":"ErrUnknown","message":"unexpected server error","cause":{...}}`. This is a struct-dump, not `{"jsonrpc":"2.0","error":{...}}`. Clients that unconditionally parse `response.error.code` as an integer will panic or silently lose the error; code detection must branch on whether the outer object has a `code` string key (BaseError) vs an `error` object key (standard JSON-RPC).
32. **`serveArchitecture`-only aliasing requires a project in the URL path** — `serveArchitecture` without `serveProject` activates a separate URL parsing branch (`erpc/http_server.go:L963-L983`) where ≥1 path segment is required (except for global healthchecks). Forgetting to include the project segment in the URL results in `ErrInvalidUrlPath "for architecture-only alias must provide /<project>"`. Additionally, combining `serveArchitecture` + `serveChain` without `serveProject` is always invalid (`erpc/http_server.go:L985-L990`).
33. **`server.httpPort` (deprecated) is silently migrated before any validation or listen** — callers that read `serverCfg.HttpPort` after `SetDefaults` will always see the canonical `httpPortV4` value; there is no warning emitted during migration (`common/defaults.go:L652-L661`).
34. **`ErrorStatusCode()` on error types is dead code** — every error type in `common/errors.go` implements an `ErrorStatusCode() int` method (e.g. `ErrInvalidRequest` returns 400, `ErrNetworkInitializing` returns 503, `ErrUpstreamRateLimitRuleExceeded` returns 429, etc.), but **there are zero call sites in the entire codebase** (verified by grep). The wire HTTP status is determined entirely by the two explicit switch blocks in `determineResponseStatusCode` (`erpc/http_server.go:L1280-L1317`) and `handleErrorResponse` (`erpc/http_server.go:L1473-L1491`), both of which use `common.HasErrorCode` to match error codes — not the interface method. Reading an error type's `ErrorStatusCode()` implementation to infer the wire status will give a wrong answer: many types (e.g. `ErrNetworkInitializing` → 503, `ErrUpstreamRateLimitRuleExceeded` → 429) return codes that the two switch blocks do not produce because those types are not listed there.

### Source map

- `erpc/http_server.go` — server construction, handler chain, routing/aliasing, CORS, per-request fan-out, X-ERPC headers, status mapping, error bodies, TLS config, shutdown, gzip writer, client-IP resolution.
- `erpc/http_timeout.go` — custom buffered TimeoutHandler (timeout/cancel bodies, per-method status, panic propagation).
- `erpc/http_batch_resp.go` — streaming batch array writer + hand-rolled JSON-RPC error serializer.
- `erpc/healthcheck.go` — healthcheck endpoint handler (draining 503, modes, eval strategies; details in healthcheck KB).
- `erpc/grpc_server.go` — `grpcSharesHttpV4` port-sharing predicate + gRPC server (its own KB).
- `erpc/init.go` — wires HTTP/gRPC/metrics servers at boot; `waitAfterShutdown` sleep; process exit codes.
- `erpc/networks_registry.go` — `ResolveAlias` used by path parsing.
- `common/config.go` — `ServerConfig`, `ExecutionHeadersMode`, `AliasingConfig`, `TLSConfig`, `CORSConfig`, `HealthCheckConfig` definitions; `ErpcVersion`/`ErpcCommitSha`.
- `common/defaults.go` — `ServerConfig.SetDefaults` (all defaults above), zero-project aliasing injection, CORS defaults.
- `common/validation.go` — `ServerConfig.Validate` (listen host/port pairing, maxTimeout required, forwarder IP/CIDR syntax).
- `common/tls.go` — client-side `CreateTLSConfig` helper (Redis, tracing exporters).
- `common/tracing_util.go` — HTTP server span start/enrich, trace-context extract/inject, force-trace.
- `common/matcher.go` — wildcard/boolean pattern engine used by aliasing, method filters, forward headers, CORS origins.
- `common/duration.go` — `Duration` YAML semantics (string or integer-milliseconds).
- `util/gzip_pool.go` — pooled gzip readers/writers for request decompression and response compression.
- `util/reader.go`, `util/bufpool.go` — pooled unbounded body reader (64 KiB pool buffers, 256 KiB return cap, 200 MB pre-grow cap).
- `util/headers.go` — `ExtractUsefulHeaders` (x-*/trace/rate-limit/etc.) used to attach upstream response headers to error details (`architecture/evm/error_normalizer.go:L24`), not on the serving path.
- `util/http_dialer.go` — `DefaultOutboundDialer` (10s connect timeout, 15s TCP keepalive) for outbound clients (`clients/http_json_rpc_client.go:L110`, `clients/proxy_pool_registry.go:L83`, `data/dynamodb.go:L64,L73`); not used by the inbound server.
- Tests: `erpc/http_server_test.go` (path parsing matrix, CORS, timeouts, fixtures), `erpc/http_timeout_test.go` (status/body per method on timeout/cancel, panic), `erpc/http_server_headers_test.go` + `erpc/http_server_exec_headers_test.go` (header contract, all/summary/off modes), `erpc/http_server_logs_test.go` (getLogs splitting through the HTTP layer).
