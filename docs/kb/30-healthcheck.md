# KB: Healthcheck endpoint

> status: complete
> source-dirs: erpc/healthcheck.go, erpc/http_server.go, erpc/healthcheck_test.go, common/config.go (HealthCheckConfig), erpc/networks_registry.go

## L1 — Capability (CTO view)

eRPC exposes a `/healthcheck` endpoint (and variants) that evaluates the live state of upstream connections and returns an HTTP status code appropriate for Kubernetes liveness/readiness probes. Seven evaluation strategies cover everything from "at least one upstream appeared" (fast startup gate) to chain-ID live verification. Response verbosity ranges from a plain-text `OK` byte to full per-upstream diagnostic JSON. Auth can be independently locked down on the healthcheck path so monitoring systems can reach it without sharing project API secrets.

## L2 — Mechanics (staff-engineer view)

**Trigger paths.** Any request that is not `POST` or `OPTIONS` is treated as a healthcheck by `parseUrlPath` (`erpc/http_server.go:L1001-L1003`). A trailing `/healthcheck` path segment also forces the healthcheck branch regardless of HTTP method (`erpc/http_server.go:L841-L849`). The handler `handleHealthCheck` is called with whatever `(projectId, architecture, chainId)` the path parser extracted, meaning the scope can be root (all projects), per-project, or per-network.

**Draining guard.** Before any evaluation, the handler checks the `draining` flag. When the server context is cancelled (shutdown initiated), `draining` is flipped to `true` and the endpoint immediately returns HTTP 503 "shutting down" (`erpc/healthcheck.go:L80-L82`, `erpc/http_server.go:L78-L83`). This is the k8s readiness-probe hook: pods stop receiving traffic before connections drain.

**Auth.** If `healthCheck.auth` is configured, an `AuthRegistry` is created at startup (`erpc/http_server.go:L201-L207`). On every healthcheck call the registry authenticates a synthetic `NormalizedRequest` carrying the real client IP (resolved from trusted-proxy headers). Auth failures return the same error shapes as the main proxy (`erpc/healthcheck.go:L85-L105`). The healthcheck auth is completely independent from per-project auth.

**Scope resolution.** When `projectId` is empty, the handler fetches all registered projects (`s.erpc.GetProjects()`); error 500 is returned only if there are zero projects. When `projectId` is set, `GetProject` is called; a missing project returns the same 404 error structure as the proxy path (`erpc/healthcheck.go:L121-L134`).

**Lazy-init for healthcheck.** If no upstreams are registered for the requested network and none are statically configured, `PrepareUpstreamsForNetwork` is called to lazy-initialize them so the healthcheck has something to evaluate (`erpc/healthcheck.go:L162-L179`). This avoids a false-negative "no upstreams initialized" on the very first probe after startup.

**Provider-only shortcut.** If a project has providers configured but zero statically-defined upstreams and zero initialized upstreams, it reports healthy with status `"OK"` and a human-readable explanation ("send first actual request to initialize the upstreams") — this prevents a provider-based project from being reported unhealthy before its first real RPC call (`erpc/healthcheck.go:L194-L199`).

**Evaluation strategies.** The `eval` query parameter (or `healthCheck.defaultEval` config field, or the hard-coded default `any:initializedUpstreams`) selects the evaluation strategy. Evaluation runs per-network first (granular visibility), then the project-level result is derived: if `architecture+chainId` are specified, the project result mirrors the matching network; otherwise `evaluateProjectHealth` aggregates across all upstreams. If the strategy is unknown, the result is unhealthy with message "unknown evaluation strategy: …".

**Error-rate strategies.** Four strategies compute error rate as `errorsTotal / requestsTotal` from the health tracker's aggregated `*` method and `DataFinalityStateAll` bucket. Only upstreams with at least one request are included; upstreams with zero requests do not count toward threshold violations but also do not count as healthy for the `all:…` strategies (`erpc/healthcheck.go:L450-L477`).

**Chain-ID strategy.** `any:evm:eth_chainId` and `all:evm:eth_chainId` call `checkEvmChainId`, which fans out `eth_chainId` RPC calls concurrently (semaphore capped at 10 parallel requests, 5 s per-upstream timeout) and compares each response to `ups.Config().Evm.ChainId`. For `any:`, one success makes the probe pass; for `all:`, every upstream must pass. Results are wired back into the per-upstream health detail objects (`erpc/healthcheck.go:L816-L913`).

**Active-upstreams strategy.** `all:activeUpstreams` checks: (1) at least one upstream or provider is configured; (2) if static upstreams exist, all of them must be initialized; (3) none of the initialized upstreams are cordoned. For provider-only setups the uninitialized-upstream check is skipped because providers may have zero initialized upstreams at startup (`erpc/healthcheck.go:L507-L548`, `erpc/healthcheck.go:L648-L679`).

**Response modes.** Three modes (controlled by `healthCheck.mode`):
- **simple**: returns the plain ASCII bytes `OK` with HTTP 200 on health, or a JSON-RPC error body with HTTP 502 on failure.
- **networks**: returns `Content-Type: application/json`, HTTP 200/502, body `{"projectId": [{id, alias, blockTimeMs, state}]}` per project.
- **verbose**: returns full JSON `{status, message, details:{projectId:{status, message, config:{networks,upstreams,providers}, initializer, upstreams:{id:{network, metrics, evmDiagnostics, ...}}, networks:{id:{status, alias, blockTimeMs}}}}}`.

In simple mode, the unhealthy JSON body is the `ErrHealthCheckFailed` eRPC error structure (not a plain string), so clients parsing JSON-RPC errors get structured data.

**Status codes.** HTTP 200 = all healthy, HTTP 502 Bad Gateway = one or more projects unhealthy, HTTP 503 Service Unavailable = draining/shutting down. (Note: 503 for draining is intentional — it tells k8s readiness to stop routing before shutdown completes.)

## L3 — Exhaustive reference (agent view)

### Config fields

All under `healthCheck.` at the top level of the eRPC config (parallel to `server.`):

| # | YAML path | Type | Default | Behavior | Source |
|---|-----------|------|---------|----------|--------|
| 1 | `healthCheck.mode` | string enum | `"networks"` (`common/defaults.go:L738-L741`, `HealthCheckConfig.SetDefaults` assigns `HealthCheckModeNetworks`) | Controls response verbosity: `"simple"` (plain-text OK or JSON error), `"networks"` (per-project network list JSON), `"verbose"` (full diagnostic JSON with upstream metrics and EVM diagnostics). Note: `erpc/healthcheck.go:L399-L403` has a nil-safe fallback that treats a missing `HealthCheckConfig` as `"simple"`, but this is only reached if `SetDefaults` was never called; the canonical configured default is `"networks"`. | `common/config.go:L186-L197` |
| 2 | `healthCheck.defaultEval` | string | `"any:initializedUpstreams"` (hard-coded fallback in handler when both query param and config field are empty, `erpc/healthcheck.go:L107-L112`) | Default eval strategy when `?eval=…` query parameter is absent. Can be any of the 8 strategy constants. | `common/config.go:L189` |
| 3 | `healthCheck.auth` | `*AuthConfig` | `nil` (no auth on healthcheck) | When set, creates an independent `AuthRegistry` for the healthcheck path. All auth strategies supported by `AuthConfig` work here. Credentials must be supplied via the same mechanisms as project auth (headers, query params). | `common/config.go:L188`, `erpc/http_server.go:L201-L207` |

Evaluation strategy constants (for `healthCheck.defaultEval` or `?eval=` query parameter):

All 8 known strategy strings (census §16.1; source `common/config.go:L200-L208`):

| Constant value | Go constant name | Semantics | Healthy when… |
|---|---|---|---|
| `any:initializedUpstreams` | `EvalAnyInitializedUpstreams` | Registry presence check; no traffic required | ≥ 1 upstream registered in the upstreams registry |
| `any:errorRateBelow90` | `EvalAnyErrorRateBelow90` | Traffic-based; upstreams with 0 requests excluded | ≥ 1 upstream has seen traffic AND error rate < 90% |
| `all:errorRateBelow90` | `EvalAllErrorRateBelow90` | Traffic-based; upstreams with 0 requests excluded | All traffic-bearing upstreams have error rate < 90% |
| `any:errorRateBelow100` | `EvalAnyErrorRateBelow100` | Traffic-based; upstreams with 0 requests excluded | ≥ 1 upstream has error rate < 100% (i.e., not total failure) |
| `all:errorRateBelow100` | `EvalAllErrorRateBelow100` | Traffic-based; upstreams with 0 requests excluded | All traffic-bearing upstreams have error rate < 100% |
| `any:evm:eth_chainId` | `EvalAnyEvmEthChainId` | Live RPC probe; semaphore=10, 5s timeout per upstream | ≥ 1 upstream returns the expected chain ID via live `eth_chainId` call |
| `all:evm:eth_chainId` | `EvalAllEvmEthChainId` | Live RPC probe; semaphore=10, 5s timeout per upstream | ALL upstreams return the expected chain ID |
| `all:activeUpstreams` | `EvalAllActiveUpstreams` | Cordon + initialization check | All statically configured upstreams are initialized AND none are cordoned |

**Unknown eval value error message.** If an unrecognized string is passed via `?eval=` or `healthCheck.defaultEval`, `evaluateNetworkHealth` falls through to the `default:` branch and returns unhealthy with the exact message: `"unknown evaluation strategy: <value>"` where `<value>` is the verbatim string supplied (`erpc/healthcheck.go:L550-L551`). This message appears in the healthcheck response body (in the `message` or network `state` field depending on mode) — operators should look for this exact prefix when debugging misconfigured eval strings.

Source: `common/config.go:L200-L208`

### Behaviors & algorithms

**HTTP status codes returned by the healthcheck endpoint:**

| HTTP status | Condition | Response body | Source |
|---|---|---|---|
| `200 OK` | All evaluated projects/networks are healthy | In `simple` mode: plain text `OK`; in `networks` mode: JSON list; in `verbose` mode: JSON `{status:"OK",…}` | `erpc/healthcheck.go:L332-L341` |
| `502 Bad Gateway` | One or more projects/networks are unhealthy (eval strategy returned false) | In `simple` mode: JSON-RPC error body with `ErrHealthCheckFailed`; in `networks`/`verbose` mode: JSON body with `state:"ERROR"` entries | `erpc/healthcheck.go:L333-L335`, `erpc/healthcheck.go:L343-L361` |
| `503 Service Unavailable` | Server is in draining/shutdown state (readiness probe hook) | Plain text `shutting down` (not JSON) | `erpc/healthcheck.go:L80-L82` |

Note: 503 uses `http.Error(w, "shutting down", http.StatusServiceUnavailable)` which sets `Content-Type: text/plain; charset=utf-8` — clients that always expect JSON will fail to parse this response. 502 is used for unhealthy (not 503) so that k8s distinguishes "pod is up but unhealthy" from "pod is shutting down".

**URL shapes for healthcheck:**
- `GET /` — global healthcheck (all projects). Non-POST/non-OPTIONS on root (`erpc/http_server.go:L844-L849`)
- `GET /healthcheck` — same as above
- `GET /<projectId>/healthcheck` — per-project healthcheck
- `GET /<projectId>/<architecture>/<chainId>/healthcheck` — per-network healthcheck (e.g. `GET /main/evm/1/healthcheck`)
- Any aliased URL shape + `/healthcheck` suffix — aliasing is resolved first, then the healthcheck suffix is stripped

For domain-aliased deployments (e.g., a domain alias that preselects all three of project+arch+chain), `GET /` or `GET /healthcheck` is the per-network probe after alias resolution.

**`?eval=` query parameter:** Always overrides `healthCheck.defaultEval`. Scope is per-request. Example: `GET /healthcheck?eval=all:activeUpstreams`.

**Lazy upstream initialization:** When healthcheck is requested for a specific network (`architecture` + `chainId` both set) and no upstreams are registered for that network, the handler calls `project.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, networkId)` — the same init path used by the first real RPC request. This means the first healthcheck probe may be slower than subsequent ones. (`erpc/healthcheck.go:L175-L181`)

**Provider-only project shortcut:** When `staticProvidersCount > 0 && len(projHealthInfo.Upstreams) == 0 && staticUpsCount == 0`, the project is immediately marked healthy without running any eval strategy. Message: `"no upstreams initialized yet, and no networks configured, but there are providers configured, send first actual request to initialize the upstreams"`. (`erpc/healthcheck.go:L194-L199`)

**EVM diagnostics in verbose mode:** For EVM upstreams in `verbose` mode, `EvmStatePoller.GetDiagnostics()` is called and included as `evmDiagnostics` in the per-upstream JSON. This is skipped in `simple` and `networks` modes. (`erpc/healthcheck.go:L263-L269`)

**Metrics visibility in verbose mode:** In `verbose` mode, `metricsTracker.GetUpstreamMethodMetrics(ups, "*", DataFinalityStateAll)` is included as `metrics` in the per-upstream JSON. In `simple` and `networks` modes, metrics are used only for evaluation, not exposed. (`erpc/healthcheck.go:L259-L262`)

**Error-rate evaluation with no traffic:** For all `errorRate*` strategies, upstreams with zero `requestsTotal` are completely excluded from evaluation. If ALL upstreams have zero traffic (i.e., `allErrorRates` is empty), the result is unhealthy: `"no error rate data available yet"`. This prevents a freshly-restarted instance from passing health checks prematurely when using traffic-based strategies. (`erpc/healthcheck.go:L465-L471`)

**all:activeUpstreams network scope:** When evaluating a specific network (`architecture+chainId` set), only upstreams whose `Evm.ChainId` matches the requested chain are counted against `networkStaticUpsCount`. Cross-network upstreams are not penalized. (`erpc/healthcheck.go:L507-L519`)

**all:activeUpstreams provider bypass:** For provider-based setups where `staticUpsCount == 0`, `hasUninitializedUpstreams` is always `false`. The check effectively becomes "any upstreams active, not cordoned" rather than "all statically declared ones". (`erpc/healthcheck.go:L663-L670`)

**chain-ID concurrency:** `checkEvmChainId` spawns one goroutine per upstream with a semaphore of 10 and a 5-second per-call timeout. Results are accumulated atomically. (`erpc/healthcheck.go:L823-L891`)

**chain-ID for `any:` with partial success:** If `failureCount > 0 && successCount > 0` under `any:evm:eth_chainId`, the result is healthy with message `"N / M upstreams passed (K failed)"`. Under `all:`, any failure makes the probe unhealthy. (`erpc/healthcheck.go:L896-L905`)

**Verbose mode backward compatibility:** The `verbose` response shape uses the same `details.{projectId}` structure as earlier versions, with the per-upstream details flattened from the internal per-network grouping. The `networks` sub-object in verbose mode only contains `status`, `alias` (if set), and `blockTimeMs` (if measured). (`erpc/healthcheck.go:L728-L806`)

**networks mode response shape:**
```json
{
  "projectId": [
    {"id": "evm:1", "alias": "mainnet", "blockTimeMs": 12000.0, "state": "OK"},
    {"id": "evm:42161", "state": "ERROR"}
  ]
}
```
(`erpc/healthcheck.go:L702-L724`)

**verbose mode response shape:**
```json
{
  "status": "OK",
  "message": "all systems operational",
  "details": {
    "projectId": {
      "status": "OK",
      "message": "...",
      "config": {"networks": 2, "upstreams": 3, "providers": 0},
      "initializer": <InitializerStatus>,
      "upstreams": {
        "upsId": {
          "network": "evm:1",
          "metrics": {...},
          "evmDiagnostics": {...}
        }
      },
      "networks": {
        "evm:1": {"status": "OK", "alias": "mainnet", "blockTimeMs": 12000.0}
      }
    }
  }
}
```

### Observability

**No dedicated Prometheus metrics for the healthcheck endpoint itself.** The healthcheck handler does not increment any custom counter. The general `erpc_unexpected_panic_total` counter fires if the handler panics.

**OTel tracing:** `handleHealthCheck` is called within the `Http.ReceivedRequest` span started by the top-level handler. The span is enriched via `common.EnrichHTTPServerSpan(ctx, statusCode, nil)` before the response is written, setting the `http.status_code` attribute. In simple-healthy mode this is called with HTTP 200 (`erpc/healthcheck.go:L340-L341`); for all other outcomes the enrichment is inside `handleErrorResponse` or at the JSON write path (`erpc/healthcheck.go:L367-L368`).

**Log messages:**
- `"entering draining mode → healthcheck will fail"` — emitted when the app context is cancelled (INFO) (`erpc/http_server.go:L82`)
- `"failed to encode health check response"` — ERROR on JSON encoding failure (`erpc/healthcheck.go:L394-L396`)
- `"registered network alias"` — DEBUG on each alias registration (`erpc/networks_registry.go:L347`)
- `"skipping duplicate alias registration with different target"` — WARN if two networks claim the same alias string (`erpc/networks_registry.go:L342`)

### Edge cases & gotchas

1. **Draining returns 503 not 502.** When the server is shutting down, the healthcheck returns HTTP 503 with plain text "shutting down" (not the JSON error body). Kubernetes treats this as "pod not ready" and stops routing. k8s liveness and readiness probes should be configured separately: use readiness with a liberal threshold (e.g., `failureThreshold=1`) so that a single 503 removes the pod from the load balancer pool; liveness should use a higher threshold to avoid premature pod restart during slow shutdown. (`erpc/healthcheck.go:L80-L82`)

2. **`any:initializedUpstreams` does not guarantee the upstream can serve requests.** An upstream registers itself in the registry even if its chain-ID probe is still pending or has failed. Use `all:activeUpstreams` or a chain-ID strategy for stronger guarantees.

3. **Error-rate strategies require prior traffic.** Fresh instances immediately after startup will return "no error rate data available yet" under any `errorRate*` strategy, which means HTTP 502. k8s startup probes (separate from readiness) with `any:initializedUpstreams` or a brief `initialDelaySeconds` are recommended to avoid premature failure marking.

4. **Per-network healthcheck lazy-inits upstreams.** The first `GET /project/evm/1/healthcheck` will call `PrepareUpstreamsForNetwork` if no upstreams are registered for `evm:1` yet. This may take hundreds of milliseconds (chain-ID RPC probes). Subsequent probes are fast. (`erpc/healthcheck.go:L175-L181`)

5. **Global healthcheck (no projectId) requires at least one project.** If `s.erpc.GetProjects()` returns an empty slice, the handler returns an error (not HTTP 200). This can surprise deployments where all projects fail to register at startup — the root healthcheck probe would fail with "no projects found" even if the server itself is up. (`erpc/healthcheck.go:L122-L127`)

6. **Project-not-found returns `&common.TRUE` for includeErrorDetails.** Unlike most proxy errors which respect `server.includeErrorDetails`, healthcheck project/network resolution errors always include full details (`erpc/healthcheck.go:L130-L133`). This means the project ID is visible in the error body.

7. **Duplicate alias registration is silently skipped with a WARN log.** If two `NetworkConfig` entries have the same `alias` value, only the first registration wins. The second is dropped. (`erpc/networks_registry.go:L340-L344`)

8. **all:activeUpstreams with provider-only projects always passes the initialization check.** Even if the provider has initialized 0 upstreams (e.g., no network has been hit yet), the provider-based path sets `hasUninitializedUpstreams = false`. The probe can still fail if there are 0 initialized upstreams AND 0 configured upstreams AND 0 providers — but that shouldn't happen in a properly configured project. (`erpc/healthcheck.go:L663-L670`)

9. **`TestHealthCheckLastEvaluation` pins that `LastEvalAt` is non-zero after Bootstrap.** Health-check exporters use this to detect stale selection state (no policy tick since startup). (`erpc/healthcheck_test.go:L26-L45`)

10. **Verbose mode metrics field uses `*` wildcard and `DataFinalityStateAll`.** This is the rolled-up aggregate across all methods and all finality states. Method-level breakdown is not exposed via healthcheck — use Prometheus metrics for that. (`erpc/healthcheck.go:L260`)

### Source map

- `erpc/healthcheck.go` — all healthcheck logic: `handleHealthCheck`, `evaluateNetworkHealth`, `evaluateProjectHealth`, `formatHealthDataForMode`, `checkEvmChainId`; response struct definitions
- `erpc/healthcheck_test.go` — integration tests for all eval strategies, drain mode, auth, provider-only, specific network scoping
- `erpc/http_server.go:L78-L83` — draining flag set when app context done
- `erpc/http_server.go:L201-L207` — healthcheck auth registry construction
- `erpc/http_server.go:L289-L292` — healthcheck dispatch in request handler
- `erpc/http_server.go:L836-L849` — URL path parsing: healthcheck detection logic
- `erpc/networks_registry.go:L247-L255` — `ResolveAlias` used to resolve per-project network aliases during healthcheck path parsing
- `common/config.go:L186-L208` — `HealthCheckConfig`, `HealthCheckMode` constants, eval-strategy constants
