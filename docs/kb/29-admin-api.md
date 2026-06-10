# KB: Admin API: endpoints, config analyzer, block heatmap, cache ops

> status: complete
> source-dirs: erpc/admin.go, erpc/config_analyzer.go, erpc/block_heatmap.go, erpc/http_server.go, erpc/erpc.go, erpc/projects.go, common/config.go, common/defaults.go, common/validation.go, cmd/erpc/main.go, telemetry/metrics.go, upstream/registry.go, auth/registry.go, data/connector.go

## L1 — Capability (CTO view)

The Admin API is eRPC's operator control plane: a JSON-RPC 2.0 endpoint (`POST /admin`) that ships 10 built-in methods covering runtime introspection (`erpc_taxonomy`, `erpc_config`, `erpc_project`), API key management via database connectors (`erpc_addApiKey`, `erpc_listApiKeys`, `erpc_updateApiKey`, `erpc_deleteApiKey`), and upstream cordon/uncordon operations (`erpc_cordonUpstream`, `erpc_uncordonUpstream`, `erpc_listCordoned`). It is completely separate from the project consumer endpoint, uses its own auth registry (secret/JWT/network strategies only — any of the same `AuthConfig` types as projects), and has an independently configured CORS policy that defaults to `allowedOrigins: ["*"]` with `allowCredentials: false` because admin requests are protected by secret tokens rather than origin checks. The `erpc validate` CLI command uses the same config-analyzer logic (`GenerateValidationReport`) to perform static and live upstream checks (chain ID fetching, genesis-block comparison, historical-block-hash majority voting) and emit structured JSON/Markdown output. The block heatmap subsystem automatically records every successfully-routed EVM request against a dynamic bucket grid (telemetry counter `erpc_network_evm_block_range_requested_total`) to let operators build Grafana-style block-access heat maps.

## L2 — Mechanics (staff-engineer view)

**Endpoint detection.** The HTTP server's `parseUrlPath` function treats any path with exactly one segment equal to `"admin"` (on a POST or OPTIONS request) as the admin endpoint (`erpc/http_server.go:L835-L838`). No project/architecture/chain ID segments are parsed; `isAdmin = true` is returned to the main handler.

**Admin dispatch flow.**
1. The main handler detects `isAdmin`, applies admin CORS (`s.adminCfg.CORS`, `erpc/http_server.go:L295-L301`). **Critically, CORS handling and OPTIONS preflight happen here — before any auth check.** If `s.adminCfg != nil && s.adminCfg.CORS != nil`, `handleCORS` is called; if that returns `false` (disallowed origin or valid preflight) OR the method is `OPTIONS`, the handler returns immediately with a 204 response. Auth is never evaluated for OPTIONS requests. See "OPTIONS /admin CORS preflight" below for full detail.
2. An `AuthPayload` is built from the HTTP headers/query (same `auth.NewPayloadFromHttp` as consumer requests, `erpc/http_server.go:L557-L559`).
3. `s.erpc.AdminAuthenticate` is called (`erpc/http_server.go:L567`). This resolves to `e.adminAuthRegistry.Authenticate(ctx, req, method, ap)` (`erpc/admin.go:L26-L29`). If `admin.auth` is not configured, `adminAuthRegistry` is nil and every request gets `fmt.Errorf("admin auth not configured")` — note this is a plain error string, distinct from `ErrAuthUnauthorized` (`erpc/admin.go:L26-L30`).
4. If `s.adminCfg` is nil (the top-level `admin:` section is entirely absent from the config), the handler skips `AdminHandleRequest` and instead returns `ErrAuthUnauthorized("", "admin is not enabled for this project")` (`erpc/http_server.go:L586-L610`). This is a distinct error from step 3: step 3 fires when `admin:` is present but `admin.auth:` is absent; step 4 fires when the entire `admin:` block is missing.
5. On success, `AdminHandleRequest` switch-dispatches on the JSON-RPC method name (`erpc/admin.go:L38-L64`). Unrecognized methods produce `ErrEndpointUnsupported`.

**OPTIONS /admin CORS preflight.** `OPTIONS /admin` is recognized as the admin endpoint by `parseUrlPath` (`erpc/http_server.go:L836`) and routes into the admin CORS block at `erpc/http_server.go:L295-L301`. The condition `r.Method == http.MethodOptions` causes an immediate `return` after `handleCORS` — auth is never invoked. Inside `handleCORS` (`erpc/http_server.go:L1008-L1077`):
- If the `Origin` header is absent: `handleCORS` returns `true` (no CORS headers set), then the `r.Method == http.MethodOptions` guard still fires, so the handler returns — but without a 204 (since `handleCORS` returned true and the outer `return` happens before any body write, yielding an implicit 200 with no body).
- If the `Origin` header is present and the origin is **disallowed**: 204 is written with no CORS headers (`erpc/http_server.go:L1041-L1044`).
- If the `Origin` header is present and the origin is **allowed**: `Access-Control-Allow-Origin`, `Access-Control-Allow-Methods`, `Access-Control-Allow-Headers`, `Access-Control-Expose-Headers`, optionally `Access-Control-Allow-Credentials`, and optionally `Access-Control-Max-Age` are all set, then 204 is written (`erpc/http_server.go:L1054-L1072`).
The admin CORS config (`s.adminCfg.CORS`) is used exclusively for this path; project-level CORS is never consulted for `/admin` requests.

**Auth registry construction.** `adminAuthRegistry` is built at startup in `NewERPC` using `auth.NewAuthRegistry(appCtx, logger, "admin", cfg.Admin.Auth, rateLimitersRegistry)` — exactly the same factory as project consumer auth, supporting all strategy types (secret, JWT, SIWE, network, database) (`erpc/erpc.go:L83-L88`).

**CORS defaults.** `AdminConfig.SetDefaults()` synthesizes `{allowedOrigins: ["*"], allowCredentials: false}` when no `cors:` block is present, then immediately calls `CORSConfig.SetDefaults()` which fills in `allowedMethods: ["GET","POST","OPTIONS"]`, `allowedHeaders: ["content-type","authorization","x-erpc-secret-token"]`, and `maxAge: 3600` (`common/defaults.go:L775-L784`). This is intentional; operators are advised to gate access via secret token rather than origin restriction. `exposedHeaders` is never set by defaults and remains nil, meaning no `Access-Control-Expose-Headers` header is emitted unless explicitly configured.

**Config analyzer (`erpc validate` / `GenerateValidationReport`).**
- Runs entirely outside the running server; the CLI's `validate` subcommand calls it directly (`cmd/erpc/main.go:L127`).
- Static phase: builds `ValidationResources` tree (projects → networks → upstreams + rate-limiter budgets), detects orphan rate-limit budgets, emits notices for rate-limit budgets without auto-tune enabled, and detects networks missing failsafe policies (`erpc/config_analyzer.go:L127-L279`).
- Live upstream phase: spawns up to 50 concurrent goroutines (semaphore channel) per project. For each upstream: creates an ephemeral `Upstream` object with a silent logger, calls `eth_chainId` with up to 3 retries (500ms/1000ms backoff), fetches genesis block hash (skipped for `EvmNodeTypeFull` or when `maxAvailableRecentBlocks > 0`), fetches latest and finalized block numbers (`erpc/config_analyzer.go:L319-L460`).
- Cross-upstream comparison: groups upstreams by `projectId::chainId`. Within each group, majority-votes on genesis hashes and on a stable historical block hash (finalized - 64, or latest - 1024 if finalized unsupported). Disagreements with ≥2 majority from ≥3 upstreams produce errors; smaller sample sizes produce warnings (`erpc/config_analyzer.go:L464-L727`).
- Exit code: CLI exits with code 1 when `report.Errors` is non-empty (`cmd/erpc/main.go:L137-L139`).

**Block heatmap.**
- `recordEvmBlockRangeHeatmap` is called from `project.Forward` after each successful forward, passing the project, network, method, request, and response (`erpc/projects.go:L189-L190`).
- For `eth_blockNumber` it directly emits a `TIP`/`Realtime` bucket. For all other methods it extracts blockNumber first from the request (via `evm.ExtractBlockReferenceFromRequest`), then from the response if not found in request. If blockNumber is still ≤ 0 it silently returns without recording.
- `ComputeBlockHeatmapBucket(blockNumber, tip, blockRef)` implements a variable-resolution grid: bucket size grows with distance from tip (100 → 1M), the label reflects whether the request is at TIP (±4 blocks), LATEST/FINALIZED/SAFE/PENDING (by blockRef tag), FUTURE (blockNumber > tip+4), or a relative `L<n>` / absolute `<start>-<end>` distance label (`erpc/block_heatmap.go:L91-L178`). If tip is unknown (≤ 0), a static fallback uses `EvmBlockRangeBucketSize` (default 100000) with absolute labels.
- The counter `erpc_network_evm_block_range_requested_total` receives labels `{project, network, vendor, upstream, category, user, finality, bucket, size}` (`telemetry/metrics.go:L724-L728`).

**Cordon mechanics.** `erpc_cordonUpstream` / `erpc_uncordonUpstream` walk the upstream registry to find the target by ID, then call `upstream.Cordon(method, reason)` or `upstream.Uncordon(method, reason)`, which delegate to the health tracker (`upstream/upstream.go:L1568-L1573`). The `method` parameter defaults to `"*"` if not supplied, cordoning the upstream for all methods. Cordon state persists in the health tracker in-memory across window rotations until explicitly uncordoned. The default selection policy honours `removeCordoned()` in its chain; custom `evalFunc` implementations must call it explicitly to respect cordon state. `erpc_listCordoned` only returns upstreams cordoned for `"*"` (whole-upstream cordon), not method-scoped cordons (`erpc/admin.go:L721-L722`).

**API key management.** Methods `erpc_addApiKey`, `erpc_listApiKeys`, `erpc_updateApiKey`, `erpc_deleteApiKey` manage records in a `data.Connector` belonging to a project's consumer auth DatabaseStrategy. The connector is resolved by `findDatabaseConnectorById(projectId, connectorId)`, which walks the *consumer* (not admin) auth registry of the named project (`erpc/admin.go:L82-L100`). Records are keyed `(apiKey → userId)` on `ConnectorMainIndex`. The stored JSON blob contains `{userId, enabled, rateLimitBudget?}`. Admin endpoints performing these calls must be authenticated by the admin auth registry, not the project consumer auth.

## L3 — Exhaustive reference (agent view)

### Config fields

All fields are under the top-level `admin:` key.

| # | YAML path | Type | Default | Behavior / notes |
|---|-----------|------|---------|------------------|
| 1 | `admin` | `*AdminConfig` | `nil` (absent) | When nil, every request to `/admin` gets 401 "admin is not enabled for this project" (`erpc/http_server.go:L597-L610`). |
| 2 | `admin.auth` | `*AuthConfig` | `nil` | Standard auth config; see KB-20 for full strategy reference. When nil, `adminAuthRegistry` is nil and `AdminAuthenticate` always returns `"admin auth not configured"` (`erpc/admin.go:L26-L30`). Supports types: `secret`, `jwt`, `siwe`, `network`, `database`. |
| 3 | `admin.auth.strategies` | `[]*AuthStrategyConfig` | `[]` | Same as project consumer auth strategies. Most deployments use `type: secret` with a static token. |
| 4 | `admin.cors` | `*CORSConfig` | auto-synthesised: `{allowedOrigins: ["*"], allowCredentials: false}` — `SetDefaults` creates this when nil (`common/defaults.go:L775-L781`) | Admin-specific CORS. Intentionally defaults to `*` because the endpoint is token-gated. Override to restrict origins if needed. |
| 5 | `admin.cors.allowedOrigins` | `[]string` | `["*"]` (from SetDefaults) | Origins. `"*"` matches all; explicit values match only those. |
| 6 | `admin.cors.allowedMethods` | `[]string` | `["GET", "POST", "OPTIONS"]` — set by `CORSConfig.SetDefaults` when nil (`common/defaults.go:L2828-L2829`) | HTTP methods permitted in CORS. `AdminConfig.SetDefaults` calls `a.CORS.SetDefaults()` immediately after synthesising the CORS struct (`common/defaults.go:L782-L784`), so `CORSConfig.SetDefaults` always runs and fills this list if nil. |
| 7 | `admin.cors.allowedHeaders` | `[]string` | `["content-type", "authorization", "x-erpc-secret-token"]` — set by `CORSConfig.SetDefaults` when nil (`common/defaults.go:L2831-L2836`) | Request headers that browsers are allowed to send. The third entry `x-erpc-secret-token` is the standard header used by eRPC secret-token auth strategies. |
| 8 | `admin.cors.exposedHeaders` | `[]string` | `nil` / `[]` — `CORSConfig.SetDefaults` never sets this field | Headers exposed to browser scripts via `Access-Control-Expose-Headers`. When nil or empty, the CORS middleware omits the `Access-Control-Expose-Headers` response header entirely. Set explicitly if browser clients need to read custom response headers. |
| 9 | `admin.cors.allowCredentials` | `*bool` | pointer to `false` — set via `util.BoolPtr(false)` when nil (`common/defaults.go:L2838-L2840`) | Typed as `*bool` (pointer); `nil` is treated as unset (SetDefaults sets it to `false`). Must stay `false` when `allowedOrigins: ["*"]` by CORS spec. |
| 10 | `admin.cors.maxAge` | `int` | `3600` — set by `CORSConfig.SetDefaults` when `0` (`common/defaults.go:L2841-L2843`) | Preflight cache duration in seconds. The CORS middleware only emits the `Access-Control-Max-Age` response header when this value is > 0; since SetDefaults sets it to 3600, the header is always present in default deployments unless explicitly overridden to a non-zero value or suppressed. |

### Behaviors & algorithms

#### Transport layer

- **URL**: `POST /admin` (single path segment, no project/network segments).
- **HTTP method**: POST (for RPC calls) or OPTIONS (for preflight). Non-POST/OPTIONS to `/admin` falls through to healthcheck logic.
- **Content-Type**: `application/json`. Standard JSON-RPC 2.0 envelope: `{"jsonrpc":"2.0","id":<id>,"method":"<name>","params":[...]}`.
- **Auth**: evaluated per-request before dispatch. Failure returns a JSON-RPC error with code corresponding to `ErrAuthUnauthorized` (HTTP 401 from `determineResponseStatusCode`). Two distinct "unauthorized" error strings exist: (a) `"admin is not enabled for this project"` when `admin:` is entirely absent from config (`erpc/http_server.go:L597-L610`); (b) `"admin auth not configured"` when `admin:` is present but `admin.auth:` is omitted (`erpc/admin.go:L26-L30`).
- **CORS / OPTIONS preflight**: evaluated BEFORE auth; OPTIONS returns immediately after CORS response (HTTP 204), so preflight never requires auth credentials (`erpc/http_server.go:L295-L301`). Uses `admin.cors` exclusively — never the project-level CORS config. If `admin.cors` is nil (admin section absent) the CORS block is skipped entirely and an OPTIONS request returns with the admin-absent 401 error instead of 204.
- **Batch requests**: the admin endpoint participates in the HTTP server's batch fan-out (`body[0] == '['`), so multiple admin methods can be batched in one HTTP request.
- **Response**: standard JSON-RPC 2.0 `{"jsonrpc":"2.0","id":<id>,"result":<obj>}` on success, `{"jsonrpc":"2.0","id":<id>,"error":{...}}` on failure. All `makeSelectionResponse` calls wrap result in `common.NewJsonRpcResponse` (`erpc/admin.go:L69-L79`).

#### Method: `erpc_taxonomy`

**Params**: none.

**Response shape**:
```json
{
  "projects": [
    {
      "id": "myProject",
      "networks": [
        {
          "id": "evm:1",
          "alias": "ethereum",
          "upstreams": [{"id": "alchemy-mainnet", "vendor": "alchemy"}],
          "providers": []
        }
      ]
    }
  ]
}
```
- Lists all loaded projects, their networks (with aliases if configured), and per-network upstreams with vendor name. `providers` field is present in the struct definition but only `upstreams` is populated in the current implementation (`erpc/admin.go:L480-L540`).
- `vendor` is the string returned by `upstream.Vendor().Name()`, empty string if vendor is nil.
- Source: `erpc/admin.go:L474-L541`.

#### Method: `erpc_config`

**Params**: none.

**Response shape**: the full `*common.Config` struct serialized as JSON. Sensitive fields (`secret.value`) are `REDACTED` by the `SecretStrategyConfig.MarshalJSON()` custom serializer (`common/config.go:L2468-L2472`).

Source: `erpc/admin.go:L456-L471`.

#### Method: `erpc_project`

**Params**: `[<projectId: string>]`.

**Response shape**:
```json
{
  "config": { /* *common.ProjectConfig, current state including lazy-loaded networks */ },
  "health": {
    "upstreams": [ /* []*upstream.Upstream JSON */ ],
    "initialization": { /* *util.InitializerStatus */ }
  }
}
```
- `health.upstreams` is the live upstream list from `UpstreamsRegistry.GetUpstreamsHealth()` which returns all registered upstreams (including their metrics, cordon state, etc. via `Upstream.MarshalJSON` if defined).
- `health.initialization` reflects network initializer status (e.g., lazy-initialization phase).
- Returns `ErrInvalidRequest` if param is missing or not a string; relays `GetProject` errors (project not found).
- Source: `erpc/admin.go:L544-L588`, `erpc/projects.go:L83-L92`.

#### Method: `erpc_addApiKey`

**Params**: `[{"projectId": string, "connectorId": string, "apiKey": string, "userId": string, "rateLimitBudget"?: string, "enabled"?: bool}]`.

**Behavior**:
1. Validates required fields (projectId, connectorId, apiKey, userId); errors on missing/wrong type.
2. `enabled` defaults to `true` if not provided.
3. Resolves `data.Connector` via `findDatabaseConnectorById(projectId, connectorId)` — looks in the project's *consumer* auth DatabaseStrategy connectors.
4. Marshals `{userId, enabled, rateLimitBudget?}` and calls `connector.Set(ctx, apiKey, userId, data, nil)`.

**Response**:
```json
{"success": true, "apiKey": "<key>", "userId": "<userId>"}
```
Source: `erpc/admin.go:L102-L192`.

#### Method: `erpc_listApiKeys`

**Params**: `[{"projectId": string, "connectorId": string, "limit"?: number, "paginationToken"?: string}]`.

- `limit` default: `50` (`erpc/admin.go:L220`).
- `paginationToken`: opaque string from previous response's `nextToken` field.
- Calls `connector.List(ctx, ConnectorMainIndex, limit, paginationToken)`.

**Response**:
```json
{
  "apiKeys": [
    {
      "key": "abc123",
      "userId": "user1",
      "rateLimitBudget": "standard",
      "enabled": true,
      "createdAt": "...",
      "updatedAt": "..."
    }
  ],
  "nextToken": "<opaque>",
  "hasMore": true,
  "totalReturned": 50
}
```
Note: `createdAt`/`updatedAt` are `time.Now()` in the current implementation (TODO marker in code), not actual creation timestamps (`erpc/admin.go:L257-L259`).

Source: `erpc/admin.go:L195-L291`.

#### Method: `erpc_updateApiKey`

**Params**: `[{"projectId": string, "connectorId": string, "apiKey": string, "updates": object}]`.

- Fetches current record with `connector.Get(ctx, ConnectorMainIndex, apiKey, "*", nil)`.
- Merges `updates` fields: `null` values delete the key; non-null values overwrite.
- Writes back with `connector.Set(ctx, apiKey, userId, updatedData, nil)`.

**Response**:
```json
{"success": true, "apiKey": "<key>", "updated": {<updates object>}}
```
Source: `erpc/admin.go:L294-L381`.

#### Method: `erpc_deleteApiKey`

**Params**: `[{"projectId": string, "connectorId": string, "apiKey": string}]`.

- Fetches current record to retrieve `userId` (the range key used for deletion).
- Calls `connector.Delete(ctx, apiKey, userId)`.

**Response**:
```json
{"success": true, "apiKey": "<key>", "userId": "<userId>"}
```
Source: `erpc/admin.go:L384-L453`.

#### Method: `erpc_cordonUpstream`

**Params**: `[{"projectId": string, "upstream": string, "method"?: string, "reason"?: string}]`.

- `method` defaults to `"*"` (all methods) when not provided (`erpc/admin.go:L630-L635`).
- `reason` defaults to `"admin: manual cordon"` when not provided (`erpc/admin.go:L669-L671`).
- Finds upstream by ID across the project's upstream registry (`erpc/admin.go:L641-L654`).
- Calls `upstream.Cordon(method, reason)` → `metricsTracker.Cordon(u, method, reason)`.

**Response**:
```json
{"projectId": "...", "upstream": "...", "method": "*", "cordoned": true, "reason": "admin: manual cordon"}
```
Source: `erpc/admin.go:L658-L687`.

#### Method: `erpc_uncordonUpstream`

**Params**: `[{"projectId": string, "upstream": string, "method"?: string, "reason"?: string}]`.

- `method` defaults to `"*"` (all methods) when not provided (`erpc/admin.go:L632-L634`) — same parsing path as `erpc_cordonUpstream`.
- `reason` defaults to `"admin: manual uncordon"` when not provided (`erpc/admin.go:L668-L673`). Note: the default reason string is **different** from `erpc_cordonUpstream` which defaults to `"admin: manual cordon"`.
- Calls `upstream.Uncordon(method, reason)`.

**Response**:
```json
{"projectId": "...", "upstream": "...", "method": "*", "cordoned": false, "reason": "admin: manual uncordon"}
```

Source: `erpc/admin.go:L658-L687` (shared `handleCordonUpstream` handler, `cordon=false`).

#### Method: `erpc_listCordoned`

**Params**: `[{"projectId": string}]`.

- Iterates all upstreams in the project registry.
- Only reports upstreams where `CordonedReason("*")` returns `cordoned=true` — method-scoped cordons (e.g. cordoned only for `eth_getLogs`) are NOT listed (`erpc/admin.go:L721-L724`).

**Response**:
```json
{
  "projectId": "myProject",
  "cordoned": [
    {"upstream": "alchemy-mainnet", "reason": "admin: manual cordon"}
  ]
}
```
Source: `erpc/admin.go:L692-L729`.

#### Config analyzer (`erpc validate` CLI)

**Invocation**:
```
erpc validate --config <path> [--format json|md]
```
- Default format: `json`.
- Logs are suppressed (zerolog disabled).
- Exit code 0 = no errors; exit code 1 = errors present OR config load failed.
- Source: `cmd/erpc/main.go:L97-L142`.

**`ValidationReport` JSON schema** (`erpc/config_analyzer.go:L76-L123`):
```json
{
  "errors": ["<string>", ...],
  "warnings": ["<string>", ...],
  "notices": ["<string>", ...],
  "resources": {
    "totals": {
      "projectsTotal": 2,
      "networksTotal": 5,
      "upstreamsTotal": 12,
      "rateLimitBudgetsTotal": 3
    },
    "tree": {
      "projects": [
        {
          "id": "myProject",
          "networks": [
            {
              "id": "evm:1",
              "alias": "ethereum",
              "upstreams": [{"id": "alchemy-mainnet"}]
            }
          ]
        }
      ],
      "rateLimiters": {
        "budgets": [
          {"id": "standard", "rulesCount": 2}
        ]
      }
    }
  }
}
```

**Static checks performed** (`erpc/config_analyzer.go:L126-L280`):
- Invalid `metrics.histogramBuckets` → error.
- Upstream with empty `endpoint` → error.
- Rate-limit budget defined but never used anywhere → notice (orphan budget).
- Rate-limit budget used by an upstream without `rateLimitAutoTune.enabled: true` → notice.
- Network with no failsafe policies (neither `networkDefaults.failsafe` nor per-network `failsafe`) → notice.

**Live upstream checks** (concurrent, up to 50 goroutines, 5s timeout each, 3 retries per operation):
- `eth_chainId` fetch: chain-ID mismatch between config and live → error; empty or parse-failed chain ID → warning.
- Genesis block hash (`eth_getBlockByNumber("0x0", false)`) — skipped for `EvmNodeTypeFull` or non-zero `maxAvailableRecentBlocks`. Majority consensus across upstreams on same chain:
  - ≥2 majority from ≥3 total: disagreement → error.
  - Fewer upstreams: → warning.
- Historical block hash at `min(finalized) - 64` or `min(latest) - 1024` (fallback). Same majority rules as genesis.
- Source: `erpc/config_analyzer.go:L281-L728`.

**`ERPC_IGNORE_LOCAL_ENDPOINT_VALIDATION=true`**: skips live checks for local/private endpoint URLs. Detected by hostname being `localhost`, `127.0.0.1`, `::1`, `*.cluster.local`, or a private-range IP (`erpc/config_analyzer.go:L28-L59`, `erpc/config_analyzer.go:L1024-L1038`).

#### Block heatmap algorithm (`erpc/block_heatmap.go`)

`ComputeBlockHeatmapBucket(blockNumber, tip, blockRef)` → `(start, end, size, label)`:

1. Returns zeros if `blockNumber ≤ 0` or `tip ≤ 0`.
2. Computes `distance = max(0, tip - blockNumber)`.
3. Selects `size` via `selectDynamicBucketSize(distance)`:

| distance threshold | bucket size |
|--------------------|-------------|
| ≤ 1,000 | 100 |
| ≤ 5,000 | 1,000 |
| ≤ 20,000 | 5,000 |
| ≤ 50,000 | 10,000 |
| ≤ 100,000 | 30,000 |
| ≤ 300,000 | 50,000 |
| ≤ 1,000,000 | 100,000 |
| ≤ 10,000,000 | 1,000,000 |
| ≤ 30,000,000 | 10,000,000 |
| ≤ 100,000,000 | 50,000,000 |
| > 100,000,000 | 50,000,000 |

4. `start = (blockNumber / size) * size` (floor-aligned to size boundary).
5. `end = start + size`; if `end > tip` then `end = tip` (snap to tip).
6. If `start ≥ tip`: snaps last aligned bucket ending at tip.
7. Label selection (first match wins):
   - `blockRef == "latest"` → `"LATEST"`.
   - `blockRef == "finalized"` → `"FINALIZED"`.
   - `blockRef == "safe"` → `"SAFE"`.
   - `blockRef == "pending"` → `"PENDING"`.
   - `|blockNumber - tip| ≤ 4` → `"TIP"` (±4 block tolerance, tested in `erpc/block_heatmap_test.go:L295-L353`).
   - `blockNumber > tip` → `"FUTURE"`.
   - `(tip - start) ≤ 5,000,000` (near-tip zone):
     - If `end == tip`: `"L<size>"` (e.g. `"L100k"`).
     - Otherwise: `"L<roundedStart>-L<roundedEnd>"` where distances are rounded to nearest `size` multiple. If `roundedEnd == 0`, emits `"L<roundedStart>"` (no `-L0` suffix; tested in `erpc/block_heatmap_test.go:L355-L400`).
   - Beyond 5M blocks from tip: absolute `"<start><unit>-<end><unit>"` format using k/m/b suffixes based on size (< 1M → thousands, < 1B → millions, else billions; `erpc/block_heatmap.go:L182-L193`).

Fallback when `ComputeBlockHeatmapBucket` returns `size ≤ 0` (tip unknown): uses global `telemetry.EvmBlockRangeBucketSize` (default `100000`) for absolute bucket labels (`erpc/block_heatmap.go:L63-L72`).

### Observability

| Metric | Type | Labels | Notes |
|--------|------|--------|-------|
| `erpc_network_evm_block_range_requested_total` | counter | `project`, `network`, `vendor`, `upstream`, `category`, `user`, `finality`, `bucket`, `size` | Incremented for every successfully-forwarded EVM request. `bucket` = human-readable bucket label (e.g. `"TIP"`, `"L100k"`, `"119m-120m"`). `size` = numeric bucket size as string (e.g. `"100000"`). Source: `telemetry/metrics.go:L724-L728`, incremented at `erpc/block_heatmap.go:L74-L84`. |
| `erpc_upstream_cordoned` | gauge | `project`, `vendor`, `network`, `upstream`, `category`, `reason` | Set to 1 on cordon, 0 on uncordon by health tracker. Source: `telemetry/metrics.go:L164-L168`. |
| `erpc_upstream_cordon_duration_seconds` | histogram | `project`, `network`, `upstream` | Recorded on uncordon: time spent cordoned. Buckets 1…86400. Source: `telemetry/metrics.go:L299`. |

No dedicated trace spans for admin methods. The HTTP server's standard OTel server span (`erpc/http_server.go:L749-L807`) covers the entire request.

Log messages:
- `component=admin` logger used for admin requests (`erpc/http_server.go:L304-L308`).
- Config analyzer uses a silent `zerolog.New(io.Discard)` logger for live upstream checks to avoid polluting CLI output (`erpc/config_analyzer.go:L282`).
- `printConfigStats` writes project/network/upstream/rate-limit summary at INFO level on process start (used by the older `AnalyseConfig` path, not `validate` CLI) (`erpc/config_analyzer.go:L946-L978`).

### Edge cases & gotchas

1. **Two distinct "admin not available" errors — `admin:` absent vs `admin.auth:` absent.** These are two separate failure modes with different error messages and different code paths:
   - **`admin:` section entirely absent**: `s.adminCfg` is nil at HTTP handler time. CORS/OPTIONS is not applied (the `s.adminCfg != nil` guard at `erpc/http_server.go:L296` skips it). `AdminAuthenticate` is still called (auth step runs), but then `s.adminCfg == nil` causes `NewErrAuthUnauthorized("", "admin is not enabled for this project")` to be returned via `processErrorBody` (`erpc/http_server.go:L586-L610`). The error message is `"admin is not enabled for this project"`.
   - **`admin:` present but `admin.auth:` absent**: `s.adminCfg` is non-nil, so CORS and OPTIONS preflight work normally. `AdminAuthenticate` is called and returns `fmt.Errorf("admin auth not configured")` because `e.adminAuthRegistry` is nil (`erpc/admin.go:L26-L30`). The error message is `"admin auth not configured"`.
   A correct minimal config needs both `admin:` (non-empty) and at least one `admin.auth.strategies` entry.

2. **`admin:` absent + OPTIONS = 401 not 204.** Since `s.adminCfg != nil` is required to enter the CORS block (`erpc/http_server.go:L296`), an `OPTIONS /admin` request with no `admin:` config skips CORS entirely and falls through to the `AdminAuthenticate` + admin-absent-check path, resulting in a 401 JSON-RPC error instead of the expected 204 preflight response. Operators adding a browser-based admin dashboard must ensure the `admin:` section is present in their config even if only to make preflight work.

3. **`erpc_listCordoned` only shows `"*"` (full) cordons.** Method-scoped cordons (`method: "eth_getLogs"`) are invisible to this method. To check method-scoped cordon state you must read health tracker internals or rely on `erpc_upstream_cordoned` metric labels (`erpc/admin.go:L721-L724`).

4. **`erpc_addApiKey` / `erpc_deleteApiKey` use the project's consumer auth connector, not the admin registry.** The `connectorId` parameter must match a `database` strategy connector ID on the named project's consumer auth config, not the admin auth config (`erpc/admin.go:L82-L100`, `auth/registry.go:L99-L111`).

5. **`erpc_listApiKeys` timestamps are always `time.Now()`.** There is a TODO in the code; actual creation/update timestamps are not stored (`erpc/admin.go:L257-L259`).

6. **`erpc_updateApiKey` applies patch semantics:** `null` values in `updates` delete fields from the stored blob; non-null overwrite. No schema enforcement; any JSON field can be added (`erpc/admin.go:L351-L357`).

7. **Config analyzer genesis check is skipped for full nodes.** Setting `evm.nodeType: full` or `evm.maxAvailableRecentBlocks > 0` suppresses genesis hash fetching with a notice, since full nodes may not have block 0 (`erpc/config_analyzer.go:L400-L403`).

8. **Config analyzer groups upstreams by resolved chain ID.** If `eth_chainId` fails, the upstream is grouped under `"unknown"`. Two upstreams both under `"unknown"` skip cross-comparison to avoid false positives from different chains (`erpc/config_analyzer.go:L475-L489`). Config chain ID is used as a fallback grouping key (`erpc/config_analyzer.go:L449-L455`).

9. **Config analyzer historical block uses finalized - 64, not latest.** If any upstream supports `eth_getBlockByNumber("finalized", false)`, the minimum finalized number across all upstreams minus 64 is used as the comparison target. This avoids reorg false-positives. Falls back to `min(latest) - 1024` (`erpc/config_analyzer.go:L589-L631`).

10. **Block heatmap `category` label uses the JSON-RPC method name.** This mirrors the same `category` label used by other per-network counters (e.g. `MetricNetworkRequestsReceived`). High-cardinality method names can increase time-series count if many distinct RPC methods are called.

11. **Block heatmap: `eth_blockNumber` always emits `"TIP"` label with `Realtime` finality** — no block number extraction occurs. This is a special-cased path in `recordEvmBlockRangeHeatmap` (`erpc/block_heatmap.go:L37-L40`).

12. **Block heatmap bucket start is always size-aligned.** `start = (blockNumber / size) * size`. This means two requests for block N and N+1 that straddle a size boundary land in different buckets. Tests verify start alignment: `start % size == 0` (`erpc/block_heatmap_test.go:L281-L283`).

13. **CORS defaults are fully populated for admin, not just origins.** `AdminConfig.SetDefaults` synthesises a `CORSConfig{AllowedOrigins: ["*"], AllowCredentials: false}` when no `cors:` block is present, then immediately calls `CORSConfig.SetDefaults()` which fills in the remaining fields: `allowedMethods: ["GET","POST","OPTIONS"]`, `allowedHeaders: ["content-type","authorization","x-erpc-secret-token"]`, `maxAge: 3600` (`common/defaults.go:L775-L784`). These are the real effective values even when `admin.cors` is omitted from the config file. Override `admin.cors.allowedOrigins: ["https://dashboard.example.com"]` in production if origin restriction is desirable — all other fields can stay at defaults.

14. **Batched admin requests are supported.** Since the HTTP server's batch detection (`body[0] == '['`) happens before the admin/consumer routing split, a JSON-RPC batch array sent to `/admin` is fanned out to per-method goroutines, each independently authenticated (`erpc/http_server.go:L404-L427`).

### Source map

- `erpc/admin.go` — all 10 admin RPC handlers, `AdminAuthenticate`, `AdminHandleRequest`, `makeSelectionResponse`, `findDatabaseConnectorById`, cordon param parsing.
- `erpc/config_analyzer.go` — `GenerateValidationReport` (static + live checks), `AnalyseConfig` (legacy startup-time path), `ValidationReport`/`ValidationResources` types, `RenderValidationReportJSON`/`RenderValidationReportMarkdown`, helper functions `fetchBlockHashByNumber`, `fetchBlockNumber`, `fetchLatestNumber`, `isLocalEndpoint`.
- `erpc/block_heatmap.go` — `recordEvmBlockRangeHeatmap` (called post-forward), `ComputeBlockHeatmapBucket` (pure, testable), `selectDynamicBucketSize`, `humanizeDistance`, `formatAbsoluteBucketLabel`.
- `erpc/block_heatmap_test.go` — comprehensive bucket label tests with real Arbitrum block numbers, TIP tolerance, no-L0 guarantee, blockRef special tags.
- `erpc/erpc.go` — `NewERPC` creates `adminAuthRegistry` from `cfg.Admin.Auth`; wires into ERPC struct.
- `erpc/http_server.go` — URL parsing detecting `/admin` segment; CORS, auth, dispatch to `AdminHandleRequest`; "admin not enabled" error path.
- `erpc/init.go` — passes `cfg.Admin` to `NewHttpServer` at `L112`.
- `erpc/projects.go` — `ProjectHealthInfo`, `GatherHealthInfo`, `recordEvmBlockRangeHeatmap` call site at `L189-L190`.
- `common/config.go` — `AdminConfig`, `CORSConfig`, `AuthStrategyConfig`, `SecretStrategyConfig` with `MarshalJSON` redaction.
- `common/defaults.go` — `AdminConfig.SetDefaults` (auto-CORS), `AuthConfig.SetDefaults`.
- `common/validation.go` — `AdminConfig.Validate` delegates to `Auth.Validate` and `CORS.Validate`.
- `telemetry/metrics.go` — `MetricNetworkEvmBlockRangeRequested` counter definition with label set; `MetricUpstreamCordoned` gauge; `EvmBlockRangeBucketSize` default var.
- `upstream/registry.go` — `UpstreamsHealth`, `GetUpstreamsHealth` (source for `erpc_project` health field).
- `upstream/upstream.go` — `Cordon`, `Uncordon`, `CordonedReason` methods.
- `auth/registry.go` — `FindDatabaseConnector` (used by API key management).
- `data/connector.go` — `ConnectorMainIndex = "idx_main"` constant.
- `cmd/erpc/main.go` — `validate` subcommand; calls `GenerateValidationReport`, renders output, exits 1 on errors.
