# KB: URL structure & routing

> status: complete
> source-dirs: erpc/http_server.go (parseUrlPath, createRequestHandler, httpStatusCode, gRPC dispatch, gzip decompression, batch detection, healthcheck fallback, MaxHeaderBytes), erpc/networks_registry.go (ResolveAlias, registerAlias), common/config.go (AliasingConfig, AliasingRuleConfig, NetworkConfig.Alias), common/network_alias.go, common/network.go (IsValidArchitecture)

## L1 — Capability (CTO view)

eRPC accepts JSON-RPC requests on a structured URL hierarchy `/<project>/<architecture>/<chainId>` and routes them to the correct project+network pair. Two orthogonal aliasing layers let operators shorten or domain-bind URLs: domain aliasing (wildcard on the `Host` header maps any combination of project/arch/chain onto a custom domain) and per-project network aliasing (`NetworkConfig.alias` maps a human-readable name like `"arbitrum"` to `evm:42161` for use in the path). Unknown projects return HTTP 404; unknown networks return HTTP 404; invalid architecture returns HTTP 400. The admin endpoint lives at the special path `/admin`.

## L2 — Mechanics (staff-engineer view)

**Two-phase routing.** The request handler first evaluates `server.aliasing.rules` (domain aliasing) against the stripped `Host` header (port stripped, first-match wins), pre-populating up to all three of `(projectId, architecture, chainId)`. Then `parseUrlPath` reads the URL path and fills in any remaining un-set fields from path segments, supporting any combination of pre-populated vs. missing fields. (`erpc/http_server.go:L232-L268`)

**Domain aliasing wildcard matching.** Rule `matchDomain` is evaluated with `common.WildcardMatch`, which supports `*` and `?` wildcards (Go `filepath.Match` semantics). The Host header has its port stripped before matching. The first matching rule wins; remaining rules are not evaluated. Each rule sets zero or more of `(serveProject, serveArchitecture, serveChain)`; unset fields remain empty and must be supplied via the path or request body. (`erpc/http_server.go:L243-L256`)

**Path parsing state machine.** `parseUrlPath` has six distinct cases based on which of `(projectId, architecture, chainId)` were pre-populated by domain aliasing. Each case defines which path-segment counts are valid and how segments are interpreted. The `/healthcheck` sentinel is stripped before the case switch; a trailing segment `"healthcheck"` or an empty/root GET is always a healthcheck (`erpc/http_server.go:L810-L1005`).

**Non-POST/non-OPTIONS to any proxy path treated as healthcheck.** After all path parsing and validation, if the HTTP method is neither POST nor OPTIONS, `isHealthCheck` is forced to `true` regardless of the URL path. This means `GET /main/evm/1` (without a `/healthcheck` suffix) behaves identically to `GET /main/evm/1/healthcheck`. The check is applied after architecture validation, so invalid architecture still returns 400 even on GET. (`erpc/http_server.go:L1001-L1003`)

**Batch detection rule: `body[0] == '['`.** After reading and decompressing the request body, the router checks if the first byte is `[`. If so, the request is treated as a JSON array (batch). There is no Content-Type check and no full JSON parse at this stage — the decision is purely `len(body) > 0 && body[0] == '['`. Only then does the handler attempt `Unmarshal` into `[]json.RawMessage`. (`erpc/http_server.go:L405-L409`)

**gRPC dispatch via `Content-Type: application/grpc`.** When gRPC sharing is enabled (`server.grpc.listenV4` shares the same port as HTTP via `grpcSharesHttpV4`), the multiplexer at the server level checks `r.ProtoMajor == 2` AND `strings.HasPrefix(lower(Content-Type), "application/grpc")`. If both are true, the request is forwarded to the gRPC server handler and never reaches `parseUrlPath`. Otherwise it falls through to the normal HTTP handler. This routing happens at the `net/http` handler wrapper level, before any URL parsing. (`erpc/http_server.go:L167-L173`)

**Max header bytes limit.** Both IPv4 and IPv6 `http.Server` instances are configured with `MaxHeaderBytes: 1 << 20` (1 MiB). This is the **only hard inbound size limit**; there is no body-size cap. Requests with headers exceeding 1 MiB are rejected by Go's net/http layer with a 431 status before any eRPC handler runs. Reverse-proxy operators sending large header sets (e.g., long `Authorization` tokens or many `X-Forwarded-*` headers) must keep total header size under 1 MiB. (`erpc/http_server.go:L186`, `erpc/http_server.go:L197`)

**Invalid gzip returns 400 `ErrInvalidRequest`.** When `Content-Encoding: gzip` is present on the request, the server unconditionally attempts to decompress the body — regardless of the `server.enableGzip` setting (that setting controls response compression only). If `gzipPool.GetReset(r.Body)` fails (corrupt or truncated gzip stream), the handler immediately writes a 400 `ErrInvalidRequest` JSON-RPC error and returns without further processing. (`erpc/http_server.go:L351-L369`)

**Network alias resolution.** Inside path parsing, whenever a 1-segment path is being resolved as a potential architecture or chainId, the parser first calls `project.networksRegistry.ResolveAlias(segment)` to check if the segment is a human-readable alias. If it resolves, the architecture+chainId from the alias entry override the raw segment. This means you can write `/main/arbitrum` instead of `/main/evm/42161` if `networks[*].alias = "arbitrum"` is configured. (`erpc/http_server.go:L860-L866`, `erpc/networks_registry.go:L247-L255`)

**Alias registration timing.** Network aliases are registered eagerly at startup in `NewNetworksRegistry` from all statically configured networks. They are also registered lazily when a network is first bootstrapped (from a provider or on first request). If an alias collision occurs (two networks claim the same alias string), the first registration wins and a WARN log is emitted. (`erpc/networks_registry.go:L67-L79`, `erpc/networks_registry.go:L322-L347`)

**`networkId` fallback in request body.** For proxy (non-healthcheck) requests, if `architecture` or `chainId` are still empty after path parsing (e.g., `/main` with no domain alias for arch/chain), the handler inspects the JSON-RPC request body for a `"networkId"` field of the form `"evm:42161"`. This allows batch requests to self-describe their network without embedding it in the URL. (`erpc/http_server.go:L615-L633`)

**Architecture validation.** After path parsing, `common.IsValidArchitecture(architecture)` is called. Currently only `"evm"` passes validation. Any other value (including empty if chain is set) returns HTTP 400 "architecture is not valid (must be 'evm')". (`erpc/http_server.go:L997-L999`, `common/network.go:L35-L37`)

**Global `NetworkAlias` resolver.** A process-global atomic function pointer `common.networkAliasResolver` is installed at startup so that components which only know the raw `networkId` (e.g., the gRPC cache connector) can look up the configured alias for consistent metric labeling. This is distinct from per-project URL routing aliases. (`common/network_alias.go:L10-L34`)

**Admin endpoint.** Exactly one URL is the admin endpoint: `/admin` with `POST` or `OPTIONS` method. Any other path structure (e.g., `/admin/foo`) does not route to admin. (`erpc/http_server.go:L836-L838`)

**Error behavior for unknown project/network.** An unknown project resolves to `ErrProjectNotFound` (HTTP 404). An unknown network (valid projectId, non-existent networkId) resolves to `ErrNetworkNotFound` (HTTP 404). These errors are returned as JSON-RPC error bodies (`{"jsonrpc":"2.0","error":{"code":...,"message":"..."}}`), not as plain-text 404s, because the transport layer maps them to HTTP 404 while keeping the JSON-RPC envelope. (`erpc/projects_registry.go:L118`, `erpc/networks_registry.go:L232`, `erpc/http_server.go:L1302-L1305`)

## L3 — Exhaustive reference (agent view)

### Config fields

**Domain aliasing** — under `server.aliasing.rules[]`:

| # | YAML path | Type | Default | Behavior | Source |
|---|-----------|------|---------|----------|--------|
| 1 | `server.aliasing.rules[].matchDomain` | string | required | Wildcard pattern matched against the `Host` header (port stripped). Supports `*` and `?`. First matching rule wins. | `common/config.go:L258`, `erpc/http_server.go:L244-L250` |
| 2 | `server.aliasing.rules[].serveProject` | string | `""` (not pre-selected) | Project ID to inject when this rule matches. If empty, projectId must appear in the URL path. | `common/config.go:L259` |
| 3 | `server.aliasing.rules[].serveArchitecture` | string | `""` | Architecture to inject (e.g. `"evm"`). If empty, architecture must appear in path or be resolved from a network alias. | `common/config.go:L260` |
| 4 | `server.aliasing.rules[].serveChain` | string | `""` | Chain ID string to inject (e.g. `"1"`, `"42161"`). If empty, chainId must appear in path. | `common/config.go:L261` |

**Network (path) aliasing** — under `projects[].networks[].alias`:

| # | YAML path | Type | Default | Behavior | Source |
|---|-----------|------|---------|----------|--------|
| 5 | `projects[].networks[].alias` | string | `""` (no alias) | Human-readable name for this network (e.g. `"arbitrum"`). Used in URL path instead of the raw `<architecture>/<chainId>`. Also propagated to the process-global `NetworkAlias` resolver for metric-label consistency. Must be unique within the project; duplicate alias triggers WARN log and the second registration is dropped. | `common/config.go:L2002`, `erpc/networks_registry.go:L67-L79`, `common/network_alias.go:L27-L34` |

Total config fields in scope: **5**

### Behaviors & algorithms

**All accepted URL shapes** (for a POST/proxy request after alias application):

| URL shape | Pre-populated by alias | Path segments | Result |
|---|---|---|---|
| `POST /<project>/<arch>/<chain>` | none | 3 | Full explicit path — canonical form |
| `POST /<arch>/<chain>` | project only | 2 | project from alias, arch+chain from path |
| `POST /<chain>` | project + arch | 1 | project+arch from alias, chain from path |
| `POST /` | project + arch + chain | 0 | all from alias — used for single-chain dedicated domains |
| `POST /<project>/<alias>` | none | 2 (2nd is network alias) | network alias resolved to arch+chain |
| `POST /<alias>` | project only | 1 (alias) | network alias resolved |
| `POST /<project>` | none | 1 | projectId only (arch+chain from body `"networkId"` field) |
| `POST /<project>/<arch>` | none | 2 (2nd not an alias) | projectId + arch; chainId from body `"networkId"` field |

For healthcheck requests (non-POST, or any method with `/healthcheck` suffix):

| URL shape | Scope |
|---|---|
| `GET /` | Global (all projects) |
| `GET /healthcheck` | Global (all projects) |
| `GET /<project>/healthcheck` | Per-project |
| `GET /<project>/<arch>/<chain>/healthcheck` | Per-network |
| `GET /<project>/<alias>/healthcheck` | Per-network (alias resolved) |
| Aliased domain `GET /healthcheck` | Scope matches what alias preselects |
| Aliased domain `GET /` | Same as above |

Source: `erpc/http_server.go:L836-L1005`, `erpc/healthcheck_test.go:L3378-L3660`

**Path parsing state machine (6 cases):**

All cases strip the leading `/` and clean the path with `path.Clean` before splitting on `/`. Empty first segment from the leading slash is discarded. After the switch, the post-switch checks at `L993-L999` apply: (a) if `projectId` is still empty and not healthcheck → 400; (b) if `architecture` or `chainId` set but architecture fails `IsValidArchitecture` → 400.

| Case | Pre-selected fields | 0 segments | 1 segment | 2 segments | 3 segments | 4+ segments | Source |
|------|--------------------|-----------:|----------:|----------:|----------:|----------:|--------|
| 1 | none | healthcheck only (or error if POST) | `projectId` | `project` + (alias-resolve → arch+chain, or raw `arch`) | `project` + `arch` + `chain` | error "must only provide /project/arch/chainId" | `L853-L877` |
| 2 | projectId only | healthcheck only (or error if POST) | alias-resolve → arch+chain, or raw `arch` | `arch` + `chain` | explicit `project`+`arch`+`chain` override | error "for project-only alias must only provide /arch/chain" | `L879-L904` |
| 3 | projectId + arch | healthcheck only (or error if POST) | alias-resolve → arch+chain override, or raw `chainId` | error | explicit override | error | `L906-L935` |
| 4 | all three (project + arch + chain) | pass through | pass through (healthcheck suffix only) | error "must not provide anything on the path" | error | error | `L937-L941` |
| 5 | arch + chain only (no project) | healthcheck only (or error if POST) | `projectId` | error | explicit `project`+`arch`+`chain` override | error "for arch-and-chain alias must only provide /project" | `L943-L961` |
| 6 | arch only (no project, no chain) | healthcheck only (or error if POST) | `projectId` | `project` + `chainId` | explicit override | error "for arch-only alias must only provide /project" | `L963-L983` |

**Invalid/impossible case (7th branch — `default`):** `projectId` set + `chainId` set but `architecture` empty → error "it is not possible to alias for project and chain WITHOUT architecture". (`erpc/http_server.go:L985-L988`)

Source: `erpc/http_server.go:L852-L991`

**Invalid/forbidden combinations:**
- `project` pre-selected + `chain` pre-selected (no arch): always error "it is not possible to alias for project and chain WITHOUT architecture" (`erpc/http_server.go:L986-L988`)
- Architecture segment that doesn't pass `IsValidArchitecture`: HTTP 400 "architecture is not valid (must be 'evm')" (`erpc/http_server.go:L997-L999`)
- Missing projectId after all resolution: HTTP 400 "project is required either in path or via domain aliasing" (`erpc/http_server.go:L993-L995`)
- 4+ segments with no pre-selection: HTTP 400 "must only provide /\<project>/\<architecture>/\<chainId>" (`erpc/http_server.go:L876-L877`)

**Network alias lookup algorithm:**
1. Path parsing encounters a candidate segment.
2. `project.networksRegistry.ResolveAlias(segment)` is called under `aliasMu.RLock`.
3. If the alias map has an entry, returns `(architecture, chainID)` from the stored `aliasEntry`.
4. If no entry, returns `("", "")` — segment is interpreted as a raw architecture or chainId string.
5. Network aliases are pre-registered at startup from `projects[].networks[].alias` fields and also registered lazily when any network is bootstrapped.

(`erpc/networks_registry.go:L247-L255`, `erpc/networks_registry.go:L332-L348`)

**`networkId` from request body fallback:**
When both `architecture` and `chainId` are still empty after path parsing, the handler attempts to decode the JSON body (must be a valid JSON object) and looks for a `"networkId"` string field of the form `"<arch>:<chain>"`. This is parsed by splitting on `:` into `(architecture, chainId)`. Used for proxy requests only (not healthcheck). (`erpc/http_server.go:L615-L633`)

**HTTP status code mapping table:**

All responses to proxy (POST) requests use a JSON-RPC envelope. The HTTP status code is derived by `httpStatusCode(err)` (`erpc/http_server.go:L1285-L1317`):

| HTTP Status | Trigger error codes | Notes |
|-------------|-------------------|-------|
| 400 Bad Request | `ErrCodeInvalidUrlPath`, `ErrCodeJsonRpcRequestUnmarshal`, `ErrCodeInvalidRequest` | Malformed URL, invalid gzip body, bad JSON, invalid architecture, impossible alias combination |
| 401 Unauthorized | `ErrCodeAuthUnauthorized`, `ErrCodeEndpointUnauthorized` | Auth failure at project or upstream level |
| 404 Not Found | `ErrCodeProjectNotFound`, `ErrCodeNetworkNotFound`, `ErrCodeNetworkNotSupported` | Unknown projectId or networkId |
| 429 Too Many Requests | `ErrCodeAuthRateLimitRuleExceeded`, `ErrCodeProjectRateLimitRuleExceeded`, `ErrCodeNetworkRateLimitRuleExceeded`, `ErrCodeEndpointCapacityExceeded` | Rate limiter exceeded at any level |
| 200 OK | everything else (including upstream/JSON-RPC errors) | Even upstream errors (5xx from provider, etc.) are returned as HTTP 200 with a JSON-RPC error body |

Source: `erpc/http_server.go:L1285-L1317`

**Error behavior for unknown project:**
- `s.erpc.GetProject(projectId)` calls `projectsRegistry.GetProject` which returns `ErrProjectNotFound` if the ID is not in `preparedProjects` map.
- HTTP 404, JSON-RPC envelope with error code `ErrProjectNotFound`, message `"project <id> is not configured"`.
- Source: `erpc/projects_registry.go:L118`, `erpc/http_server.go:L1302-L1305`

**Error behavior for unknown network:**
- `project.GetNetwork(ctx, networkId)` calls `networksRegistry.GetNetwork` which returns `ErrNetworkNotFound` if no config and no dynamic resolution is possible.
- HTTP 404, JSON-RPC envelope.
- Note: networks are lazily initialized, so a network that has never been requested may succeed on first call (provider-based projects). An explicit `ErrNetworkNotFound` is returned only if the networkId format is valid but resolution definitively fails.
- Source: `erpc/networks_registry.go:L221-L232`

**`common.NetworkAlias` global resolver:**
- `common.SetNetworkAliasResolver(f)` installs a global `func(networkId string) string` stored in an atomic pointer.
- `common.NetworkAlias(networkId)` calls the installed resolver; returns the alias if found, else the raw networkId unchanged.
- Used by components that do not have access to the project context (e.g., gRPC cache connector) to emit consistent metric labels.
- Source: `common/network_alias.go:L10-L34`

### Observability

**Tracing:** All requests (including healthcheck, admin, proxy) share the same `Http.ReceivedRequest` OTel span. The span includes `http.method`, `http.url`, `http.scheme`, `http.user_agent` attributes. After routing, `common.SetForceTraceNetwork(ctx, architecture+":"+chainId)` injects the network into the span context for force-trace matching (`erpc/http_server.go:L285-L288`). On healthcheck, `EnrichHTTPServerSpan` sets `http.status_code`.

**CORS metrics** (these fire on CORS preflight paths, not routing per se):
- `erpc_cors_requests_total{project,origin}` — every request with an `Origin` header (`telemetry/metrics.go:L610-L614`)
- `erpc_cors_preflight_requests_total{project,origin}` — OPTIONS requests from allowed origins (`telemetry/metrics.go:L616-L620`)
- `erpc_cors_disallowed_origin_total{project,origin}` — requests from origins not in the allowlist (`telemetry/metrics.go:L622-L626`)

**Log messages:**
- `"received http request"` — INFO with `body` JSON field for non-empty bodies (`erpc/http_server.go:L398-L401`)
- `"received http request with empty body"` — INFO for empty-body requests
- `"failed to match aliasing rule"` — ERROR if `WildcardMatch` returns an error for a domain alias rule (`erpc/http_server.go:L248`)
- `"registered network alias"` — DEBUG on alias registration (`erpc/networks_registry.go:L347`)
- `"skipping duplicate alias registration with different target"` — WARN on alias collision (`erpc/networks_registry.go:L342`)

### Edge cases & gotchas

1. **Domain aliasing matches the Host header AFTER port stripping.** If an ingress forwards `Host: api.example.com:443`, the colon and port are stripped before matching. A rule `matchDomain: api.example.com` works regardless of port in the Host header. (`erpc/http_server.go:L234-L237`)

2. **First domain alias rule wins; no rule merging.** If two rules both match the incoming domain, only the first one takes effect. Order your rules from most-specific to least-specific. (`erpc/http_server.go:L244-L256`)

3. **Network aliases are per-project, not global.** Two projects can have different networks aliased as `"mainnet"` without conflict. The alias map is stored per `NetworksRegistry` instance which is per `PreparedProject`. (`erpc/networks_registry.go:L31`)

4. **`any:` architecture alias with no chainId alias is a valid (and useful) pattern.** A rule `serveArchitecture: "evm"` with no chain pre-selected lets the URL be `POST /<project>/<chainId>` — no need to include `evm` in every URL. (`erpc/http_server.go:L963-L983`)

5. **Impossible alias: project + chain without architecture.** The combination `serveProject` + `serveChain` with empty `serveArchitecture` is explicitly detected and returns an error to prevent silent misrouting (`erpc/http_server.go:L986-L988`). This is a footgun because architecture is part of the networkId format.

6. **Path aliases are resolved only in specific positions.** Alias resolution is attempted only when a single remaining segment is being interpreted as architecture/chainId or a combination. In a fully-explicit 3-segment path (`/project/evm/42161`), no alias resolution is performed. This means an alias of `"evm"` would NOT intercept a segment in the `architecture` position of a 3-segment path.

7. **`path.Clean` canonicalizes the URL path before parsing.** Double slashes (`/main//evm/1`) are collapsed; relative segments (`/main/../other/evm/1`) are resolved. This means paths with unusual characters that survive URL decoding are normalized before segment splitting. (`erpc/http_server.go:L821`)

8. **Admin endpoint is exactly `/admin`, POST or OPTIONS only.** `GET /admin` is treated as a healthcheck (non-POST/non-OPTIONS). `/admin/anything` does not route to admin — it fails with a URL parse error because 2-segment paths need the second segment to be a valid architecture string. (`erpc/http_server.go:L836-L838`, tests: `erpc/http_server_test.go:L3413-L3423`)

9. **Body-based `networkId` requires valid JSON.** If the body is not valid JSON when `architecture` is still empty, the handler returns a JSON-RPC parse error. This path is only hit for proxy requests (healthcheck does not read the body). (`erpc/http_server.go:L617-L622`)

10. **Network alias conflict: first-registration wins, silently for callers.** If a provider dynamically creates a network whose alias collides with a statically configured network, the static alias wins (registered first at `NewNetworksRegistry` time). The dynamic registration is dropped with a WARN log but no error is returned to the caller. (`erpc/networks_registry.go:L340-L344`)

11. **`parseUrlPath` is also called for healthcheck requests, and healthcheck passes through the same alias-resolution and architecture-validation logic.** A healthcheck on `/main/foo/healthcheck` where `foo` is not a known alias and not a valid architecture will fail with HTTP 400, not 200. (`erpc/http_server.go:L997-L999`)

12. **The `TestHttpServer_ParseUrlPath` test suite covers 30+ cases** including all alias pre-selection combinations, network alias resolution, invalid architecture segments, and healthcheck suffix placement. (`erpc/http_server_test.go:L3378-L3709`)

13. **`GET /main/evm/1` is a healthcheck, not an error.** Any request whose HTTP method is neither POST nor OPTIONS has `isHealthCheck` forced to `true` at `erpc/http_server.go:L1001-L1003`, after all path parsing and validation. This means a `GET` to a fully-specified proxy path silently becomes a network-scoped healthcheck. There is no indication in the response that this substitution occurred. This is intentional (allows load-balancer probes without a separate URL) but can be surprising.

14. **Batch detection has no Content-Type gate.** The check `body[0] == '['` fires for any POST body that begins with `[`, even if the `Content-Type` is `text/plain` or omitted entirely. Similarly, a body `[invalid json` will be detected as batch and then fail unmarshalling, returning a 400. There is no early rejection based on Content-Type. (`erpc/http_server.go:L405-L414`)

15. **`Content-Encoding: gzip` decompression is unconditional for requests.** The `server.enableGzip` config flag controls only response (outbound) compression, not inbound. Any request with `Content-Encoding: gzip` is always decompressed regardless of `enableGzip`. Corrupt gzip → 400 `ErrInvalidRequest` JSON-RPC error immediately. (`erpc/http_server.go:L349-L370`)

16. **gRPC shares the HTTP port only when `grpcSharesHttpV4` is true.** The content-type dispatch (`application/grpc` + HTTP/2) happens at the `handlerV4` wrapper level — before `TimeoutHandler`, before CORS, before any URL parsing. On cleartext HTTP/2 (no TLS), `h2c.NewHandler` is also wrapped so that HTTP/2 upgrade works without TLS. Only IPv4 gets gRPC sharing; IPv6 never gets a shared gRPC handler. (`erpc/http_server.go:L161-L177`)

17. **1 MiB header limit is the only hard inbound size limit.** Go's `net/http` enforces `MaxHeaderBytes: 1<<20` and rejects requests with oversized headers at the protocol level (431 status) before eRPC code runs. Body size has no configured cap — a client can send arbitrarily large bodies and they will be read into memory (via `util.ReadAll` with a 2 KiB initial buffer). Operators must apply body size limits at the reverse proxy / load balancer layer if needed. (`erpc/http_server.go:L186`, `erpc/http_server.go:L197`, `erpc/http_server.go:L374`)

### Source map

- `erpc/http_server.go:L226-L292` — domain alias evaluation in `handleRequest`; `Host` header stripping; routing dispatch
- `erpc/http_server.go:L810-L1006` — `parseUrlPath`: full path parsing state machine, all 6 cases, alias resolution calls, healthcheck/admin detection, validation
- `erpc/http_server.go:L615-L633` — `networkId` fallback from request body
- `erpc/networks_registry.go:L22-L81` — `NetworksRegistry` struct + `NewNetworksRegistry` (eager alias registration from static config)
- `erpc/networks_registry.go:L247-L255` — `ResolveAlias`: alias lookup
- `erpc/networks_registry.go:L322-L348` — `registerAlias`: lazy registration + collision guard
- `common/config.go:L253-L262` — `AliasingConfig`, `AliasingRuleConfig` fields
- `common/config.go:L1995-L2006` — `NetworkConfig.Alias` field
- `common/network_alias.go` — process-global `NetworkAlias` resolver (for cross-component metric label consistency)
- `common/network.go:L35-L37` — `IsValidArchitecture` (currently only `"evm"`)
- `erpc/http_server_test.go:L3378-L3709` — `TestHttpServer_ParseUrlPath` comprehensive path parsing tests
- `erpc/projects_registry.go:L118` — `ErrProjectNotFound` on unknown project
- `erpc/networks_registry.go:L221-L232` — `ErrNetworkNotFound` on unknown network
- `erpc/http_server.go:L161-L177` — gRPC/HTTP multiplexer: `Content-Type: application/grpc` + HTTP/2 dispatches to gRPC server, everything else to HTTP handler; h2c wrapping for cleartext HTTP/2
- `erpc/http_server.go:L186`, `L197` — `MaxHeaderBytes: 1<<20` (1 MiB) — only hard inbound size limit on both IPv4 and IPv6 servers
- `erpc/http_server.go:L349-L370` — unconditional request body gzip decompression; invalid gzip → 400 `ErrInvalidRequest`
- `erpc/http_server.go:L405-L414` — batch detection: `body[0] == '['` — no Content-Type check, no pre-parse
- `erpc/http_server.go:L1001-L1003` — non-POST/non-OPTIONS forces `isHealthCheck = true` for any proxy path
- `erpc/http_server.go:L1285-L1317` — `httpStatusCode(err)`: full HTTP status code mapping (400/401/404/429/200)
