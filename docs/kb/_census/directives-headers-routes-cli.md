# Census: Request directives, HTTP routes, admin endpoints, CLI, env vars, response headers

> status: complete
> scope: completeness checklist (not prose) of every externally-visible knob of the eRPC binary: X-ERPC-* directives (header + query), all HTTP/gRPC routes, admin JSON-RPC methods, CLI subcommands/flags, environment variables read anywhere in Go code, and every response header eRPC sets. All rows verified in source; line numbers refer to repo HEAD at time of writing.

---

## 1. Request directives (header / query-param pairs)

Parsed in `common/request.go` `EnrichFromHttp()` (headers: `common/request.go:730-808`, query params: `common/request.go:810-892`). Registry of all pairs: `common/request.go:90-114`. Header-name constants: `common/request.go:38-62`; query-name constants: `common/request.go:64-88`.

Precedence (lowest → highest): network `directiveDefaults` config (`erpc/http_server.go:652`, applied via `ApplyDirectiveDefaults` `common/request.go:563-676`, which is a no-op if directives were already populated `common/request.go:570-572`) → HTTP headers (`common/request.go:730`) → query params (`common/request.go:810-812` comment: query overrides headers). Boolean parse rule: `strings.ToLower(strings.TrimSpace(v)) == "true"` (anything else = false). Int parse: `strconv.ParseInt(v,10,64)`; parse failure silently ignores the directive.

| # | HTTP header | Query param | Value type | Directive field | Parse sites (hdr / qry) | Notes |
|---|---|---|---|---|---|---|
| 1 | `X-ERPC-Retry-Empty` (`common/request.go:39`) | `retry-empty` (`common/request.go:65`) | bool | `RetryEmpty` (`common/request.go:121`) | `common/request.go:731-733` / `common/request.go:815-817` | Go zero default false (struct comment `common/request.go:117-121`) |
| 2 | `X-ERPC-Retry-Pending` (`common/request.go:40`) | `retry-pending` (`common/request.go:66`) | bool | `RetryPending` (`common/request.go:128`) | `common/request.go:734-736` / `common/request.go:819-821` | Struct comment claims "true by default" (`common/request.go:125`) but no code sets it true unless `directiveDefaults.retryPending: true`; Go zero value is false — stale comment, verify before documenting. Consumed at `erpc/network_executor.go:445-448` |
| 3 | `X-ERPC-Skip-Cache-Read` (`common/request.go:41`) | `skip-cache-read` (`common/request.go:67`) | string: `"true"`/`"false"`/connector-id wildcard pattern (e.g. `redis*`, `memory*\|dynamo*`) | `SkipCacheRead` (`common/request.go:134`) | `common/request.go:737-739` / `common/request.go:823-825` | Matching logic `ShouldSkipCacheRead` `common/request.go:898-914` (case-insensitive true/false, else `WildcardMatch` vs connectorId) |
| 4 | `X-ERPC-Use-Upstream` (`common/request.go:42`) | `use-upstream` (`common/request.go:68`) | string (wildcard, e.g. `alchemy`, `my-own-*`) | `UseUpstream` (`common/request.go:139`) | `common/request.go:740-742` / `common/request.go:811-813` | Header value NOT trimmed; query value IS trimmed (`common/request.go:741` vs `:812`) |
| 5 | `X-ERPC-Skip-Interpolation` (`common/request.go:43`) | `skip-interpolation` (`common/request.go:69`) | bool | `SkipInterpolation` (`common/request.go:153`) | `common/request.go:743-745` / `common/request.go:827-829` | Block refs still computed/cached; outbound tag→hex rewrite suppressed (`common/request.go:150-153`) |
| 6 | `X-ERPC-Skip-Consensus` (`common/request.go:44`) | `skip-consensus` (`common/request.go:70`) | bool | `SkipConsensus` (`common/request.go:161`) | `common/request.go:746-748` / `common/request.go:831-833` | Retry/hedge/breaker/timeout still apply (`common/request.go:155-161`) |
| 7 | `X-ERPC-Enforce-Highest-Block` (`common/request.go:45`) | `enforce-highest-block` (`common/request.go:71`) | bool | `EnforceHighestBlock` (`common/request.go:164`) | `common/request.go:751-753` / `common/request.go:836-838` | |
| 8 | `X-ERPC-Enforce-GetLogs-Range` (`common/request.go:46`) | `enforce-getlogs-range` (`common/request.go:72`) | bool | `EnforceGetLogsBlockRange` (`common/request.go:165`) | `common/request.go:754-756` / `common/request.go:839-841` | |
| 9 | `X-ERPC-Enforce-Non-Null-Tagged-Blocks` (`common/request.go:47`) | `enforce-non-null-tagged-blocks` (`common/request.go:73`) | bool | `EnforceNonNullTaggedBlocks` (`common/request.go:166`) | `common/request.go:757-759` / `common/request.go:842-844` | |
| 10 | `X-ERPC-Enforce-Log-Index-Strict-Increments` (`common/request.go:48`) | `enforce-log-index-strict-increments` (`common/request.go:74`) | bool | `EnforceLogIndexStrictIncrements` (`common/request.go:180`) | `common/request.go:760-762` / `common/request.go:845-847` | |
| 11 | `X-ERPC-Validate-Logs-Bloom-Emptiness` (`common/request.go:49`) | `validate-logs-bloom-emptiness` (`common/request.go:75`) | bool | `ValidateLogsBloomEmptiness` (`common/request.go:186`) | `common/request.go:763-765` / `common/request.go:848-850` | |
| 12 | `X-ERPC-Validate-Logs-Bloom-Match` (`common/request.go:50`) | `validate-logs-bloom-match` (`common/request.go:76`) | bool | `ValidateLogsBloomMatch` (`common/request.go:189`) | `common/request.go:766-768` / `common/request.go:851-853` | |
| 13 | `X-ERPC-Validate-Tx-Hash-Uniqueness` (`common/request.go:51`) | `validate-tx-hash-uniqueness` (`common/request.go:77`) | bool | `ValidateTxHashUniqueness` (`common/request.go:181`) | `common/request.go:769-771` / `common/request.go:854-856` | |
| 14 | `X-ERPC-Validate-Transaction-Index` (`common/request.go:52`) | `validate-transaction-index` (`common/request.go:78`) | bool | `ValidateTransactionIndex` (`common/request.go:182`) | `common/request.go:772-774` / `common/request.go:857-859` | |
| 15 | `X-ERPC-Receipts-Count-Exact` (`common/request.go:53`) | `receipts-count-exact` (`common/request.go:79`) | int64 | `ReceiptsCountExact` (`common/request.go:198`) | `common/request.go:776-780` / `common/request.go:860-864` | nil = unset/don't check |
| 16 | `X-ERPC-Receipts-Count-At-Least` (`common/request.go:54`) | `receipts-count-at-least` (`common/request.go:80`) | int64 | `ReceiptsCountAtLeast` (`common/request.go:199`) | `common/request.go:781-785` / `common/request.go:865-869` | nil = unset |
| 17 | `X-ERPC-Validation-Expected-Block-Hash` (`common/request.go:55`) | `validation-expected-block-hash` (`common/request.go:81`) | string | `ValidationExpectedBlockHash` (`common/request.go:202`) | `common/request.go:786-788` / `common/request.go:870-872` | |
| 18 | `X-ERPC-Validation-Expected-Block-Number` (`common/request.go:56`) | `validation-expected-block-number` (`common/request.go:82`) | int64 (decimal) | `ValidationExpectedBlockNumber` (`common/request.go:203`) | `common/request.go:789-793` / `common/request.go:873-877` | |
| 19 | `X-ERPC-Validate-Transactions-Root` (`common/request.go:57`) | `validate-transactions-root` (`common/request.go:83`) | bool | `ValidateTransactionsRoot` (`common/request.go:170`) | `common/request.go:794-796` / `common/request.go:878-880` | |
| 20 | `X-ERPC-Validate-Header-Field-Lengths` (`common/request.go:58`) | `validate-header-field-lengths` (`common/request.go:84`) | bool | `ValidateHeaderFieldLengths` (`common/request.go:173`) | `common/request.go:797-799` / `common/request.go:881-883` | Struct comment "(only via config/library, not HTTP headers)" (`common/request.go:172`) is STALE — code does parse it from both header and query |
| 21 | `X-ERPC-Validate-Transaction-Fields` (`common/request.go:59`) | `validate-transaction-fields` (`common/request.go:85`) | bool | `ValidateTransactionFields` (`common/request.go:176`) | `common/request.go:800-802` / `common/request.go:884-886` | |
| 22 | `X-ERPC-Validate-Transaction-Block-Info` (`common/request.go:60`) | `validate-transaction-block-info` (`common/request.go:86`) | bool | `ValidateTransactionBlockInfo` (`common/request.go:177`) | `common/request.go:803-805` / `common/request.go:887-889` | |
| 23 | `X-ERPC-Validate-Log-Fields` (`common/request.go:61`) | `validate-log-fields` (`common/request.go:87`) | bool | `ValidateLogFields` (`common/request.go:183`) | `common/request.go:806-808` / `common/request.go:890-892` | |

Boolean headers #20-23 use `strings.ToLower(hv) == "true"` WITHOUT TrimSpace (`common/request.go:798,801,804,807`); all other boolean headers trim first.

### 1b. Directive-adjacent request inputs (not in the directive registry)

| Input | Kind | Values / behavior | Source |
|---|---|---|---|
| `X-ERPC-Force-Trace` | header | truthy: `true`/`1`/`yes` → bypass trace sampling | const `common/tracing_core.go:29`; check `common/tracing_util.go:107-111` |
| `force-trace` | query param | same truthy set | const `common/tracing_core.go:31`; check `common/tracing_util.go:113-116` |
| `user-agent` | query param | overrides `User-Agent` header for agent-name metrics tracking | `common/request.go:1299-1314` |
| `User-Agent` | header | agent-name metrics; stored raw or simplified per `project.userAgentMode` (`erpc/http_server.go:653-658`) | `common/request.go:1300-1311`, simplify `common/request.go:1316+` |
| `networkId` | JSON body field | `"evm:42161"`-style network selection when arch/chain absent from URL | `erpc/http_server.go:613-634` |

### 1c. Auth credentials (headers / query params), HTTP

All parsed in `auth.NewPayloadFromHttp` (`auth/http.go:13-88`), evaluated in this exact if/else order (first match wins):

| Order | Input | Kind | Auth type | Source |
|---|---|---|---|---|
| 1 | `?token=` | query | secret (deprecated) | `auth/http.go:18-22` |
| 2 | `?secret=` | query | secret | `auth/http.go:23-27` |
| 3 | `X-ERPC-Secret-Token` | header | secret | `auth/http.go:28-32` |
| 4 | `Authorization: Basic <b64 user:pass>` | header | secret (password only; username ignored) | `auth/http.go:33-53` |
| 5 | `Authorization: Bearer <jwt>` | header | jwt | `auth/http.go:54-59` |
| 6 | `?jwt=` | query | jwt | `auth/http.go:61-65` |
| 7 | `?signature=` + `?message=` (message may be base64) | query | siwe | `auth/http.go:66-71`, b64 normalize `auth/http.go:90-96` |
| 8 | `X-Siwe-Message` + `X-Siwe-Signature` | headers | siwe | `auth/http.go:72-79` |
| 9 | (none) | — | network strategy (IP-based) | `auth/http.go:82-85` |

### 1d. Directive fields NOT settable via HTTP (config / library only)

| Field | Settable via | Source |
|---|---|---|
| `ByPassMethodExclusion` | internal only (`json:"-"`) | `common/request.go:141-142` |
| `IsInternal` | internal only; bypasses retry/hedge/breaker | `common/request.go:144-148` |
| `ValidateReceiptTransactionMatch` | `directiveDefaults` config only (no header/query key) | field `common/request.go:193`, config `common/config.go:2142`, applied `common/request.go:651-653` |
| `ValidateContractCreation` | `directiveDefaults` config only | field `common/request.go:195`, config `common/config.go:2143`, applied `common/request.go:654-656` |
| `GroundTruthTransactions` / `GroundTruthLogs` | Go library mode only (`json:"-"`) | `common/request.go:205-214` |

`directiveDefaults` config block (network-level `projects[].networks[].directiveDefaults.*`, also failsafe-scoped at `common/config.go:597`): full struct `common/config.go:2105-2152`; `skipCacheRead` accepts bool or string in YAML/JSON, normalized to string (`common/config.go:2154-2175`).

---

## 2. HTTP routes

Single catch-all handler; routing is done by parsing the URL path inside `parseUrlPath` (`erpc/http_server.go:810-1006`). No mux. Servers: IPv4 (default `0.0.0.0:4000`, `common/defaults.go:645-656`), IPv6 (default `[::]:5000`, `common/defaults.go:648-667`), TLS optional (`erpc/http_server.go:1537-1543,1609-1637`). Architecture must be `evm` when present (`erpc/http_server.go:997-999`).

| Route | Method(s) | Behavior | Source |
|---|---|---|---|
| `POST /admin` | POST, OPTIONS | Admin JSON-RPC endpoint (see section 3). Exactly one path segment `admin` | `erpc/http_server.go:835-838` |
| `POST /<project>/<architecture>/<chainId>` | POST | Main JSON-RPC proxy endpoint (single or batch body; batch = body starts with `[`, `erpc/http_server.go:404-427`) | `erpc/http_server.go:867-870` |
| `POST /<project>/<networkAlias>` | POST | 2nd segment resolved via `networksRegistry.ResolveAlias`; falls back to treating it as architecture | `erpc/http_server.go:858-866`, `erpc/networks_registry.go:247-255` |
| `POST /<project>` | POST | Network resolved from `"networkId"` field in JSON body (`evm:42161`) | `erpc/http_server.go:855-857,613-634` |
| `POST /` (domain-aliased) | POST | `server.aliasing.rules[]` matched on Host (port stripped) preselect project/architecture/chain (`matchDomain` wildcard, `serveProject`, `serveArchitecture`, `serveChain`) | `erpc/http_server.go:232-257`, alias cases `erpc/http_server.go:879-991` |
| `/<...>/healthcheck` | any | Any path whose LAST segment is `healthcheck` → healthcheck for the preceding scope: `/healthcheck` (all projects), `/<project>/healthcheck`, `/<project>/<arch>/<chainId>/healthcheck` | `erpc/http_server.go:841-843` |
| `GET /` (or empty path) | non-POST/OPTIONS | Healthcheck (all projects) | `erpc/http_server.go:844-849` |
| any path, non-POST/non-OPTIONS | GET etc. | Treated as healthcheck for the parsed scope (e.g. `GET /main/evm/1`) | `erpc/http_server.go:1001-1003` |
| `OPTIONS <any proxy path>` | OPTIONS | CORS preflight; answered when project `cors` configured (allowed origin → 204 + CORS headers; disallowed → bare 204) | `erpc/http_server.go:343-347,1008-1077` |
| `OPTIONS /admin` | OPTIONS | CORS preflight via `admin.cors` | `erpc/http_server.go:295-301` |
| gRPC on HTTP port | HTTP/2 + `Content-Type: application/grpc` | When `server.grpcEnabled` and gRPC host/port == HTTP v4 host/port, gRPC is served on the same listener (h2c when no TLS) | `erpc/http_server.go:161-177`, `erpc/grpc_server.go:42-53` |
| Metrics: `ANY <any path>` on metrics port | any | `promhttp.Handler()` is the root handler of a separate server (default port 4001, `common/defaults.go:759-761`; enabled by default outside tests, `common/defaults.go:750-752`); every path serves Prometheus metrics | `erpc/init.go:137-158` |
| Standalone gRPC port | gRPC | When `grpcEnabled` and NOT sharing HTTP port: separate listener at `grpcHostV4:grpcPortV4` (defaults mirror HTTP host/port, `common/defaults.go:672-686`) | `erpc/init.go:125-136`, `erpc/grpc_server.go:119-131` |

Path-aliasing combination matrix (preselected via domain aliasing × extra path segments): nothing preselected (`erpc/http_server.go:853-877`), project-only (`:879-904`), project+arch (`:906-935`), all three (`:937-941`, extra segments rejected), arch+chain (`:943-961`), arch-only (`:963-983`), project+chain without arch = invalid (`:986-988`).

Healthcheck details:
- `?eval=` query param selects strategy (`erpc/healthcheck.go:106`); fallback `healthCheck.defaultEval` (`erpc/healthcheck.go:107-109`, config `common/config.go:189`), final fallback `any:initializedUpstreams` (`erpc/healthcheck.go:110-112`).
- Valid eval values (`common/config.go:200-209`): `any:initializedUpstreams`, `any:errorRateBelow90`, `all:errorRateBelow90`, `any:errorRateBelow100`, `all:errorRateBelow100`, `any:evm:eth_chainId`, `all:evm:eth_chainId`, `all:activeUpstreams`. Unknown value → ERROR (`erpc/healthcheck.go:550-551`).
- Response modes via `healthCheck.mode` (`common/config.go:194-198`): `simple` (default; plain `OK` body, `erpc/healthcheck.go:399-403,337-363`), `networks`, `verbose` (`erpc/healthcheck.go:405-408,365-397`).
- Status codes: 503 `shutting down` while draining (`erpc/healthcheck.go:80-83`); 502 when unhealthy (`erpc/healthcheck.go:332-335`); healthcheck has its own optional auth registry `healthCheck.auth` (`erpc/http_server.go:201-207`, `erpc/healthcheck.go:85-105`).

Transport status-code mapping (POST JSON-RPC errors generally stay 200): 400 invalid URL/unmarshal/invalid request; 401 auth; 404 project/network not found; 429 rate limits (`erpc/http_server.go:1280-1317` and duplicate logic `:1473-1491`). Final fatal-error writer forces 200 for POST (`erpc/http_server.go:770-773`). Timeout handler: 504 (non-POST) / 200 + `{"code":-32603,"message":"http request handling timeout"}` for POST (`erpc/http_timeout.go:106-122`); client-cancel: 503 (non-POST) / 200 + `request cancelled by client` (`erpc/http_timeout.go:123-141`). Request-body gzip supported via `Content-Encoding: gzip` (`erpc/http_server.go:349-370`). Max header bytes 1MB; idle timeout 300s (`erpc/http_server.go:181-198`); default maxTimeout 150s, readTimeout 30s, writeTimeout 120s (`erpc/http_server.go:65-76`).

---

## 3. Admin endpoints (JSON-RPC methods on `POST /admin`)

Dispatch: `AdminHandleRequest` (`erpc/admin.go:32-65`). Auth: `AdminAuthenticate` requires `admin.auth` configured, else "admin auth not configured" (`erpc/admin.go:25-30`); if `admin` section absent entirely → `ErrAuthUnauthorized` "admin is not enabled for this project" (`erpc/http_server.go:597-610`). Unknown method → `ErrEndpointUnsupported` (`erpc/admin.go:60-64`). CORS for admin via `admin.cors` (`erpc/http_server.go:295-301`).

| Method | Params | Result | Source |
|---|---|---|---|
| `erpc_taxonomy` | none | `{projects:[{id, networks:[{id, alias, upstreams:[{id, vendor}], providers}]}]}` | `erpc/admin.go:39-40,474-541` |
| `erpc_config` | none | full running config object | `erpc/admin.go:41-42,456-471` |
| `erpc_project` | `params[0]` = projectId (string, required) | `{config, health}` | `erpc/admin.go:43-44,544-588` |
| `erpc_addApiKey` | `params[0]` = `{projectId!, connectorId!, apiKey!, userId!, rateLimitBudget?, enabled? (default true)}` | `{success, apiKey, userId}` | `erpc/admin.go:45-46,103-192` |
| `erpc_listApiKeys` | `params[0]` = `{projectId!, connectorId!, limit? (default 50), paginationToken?}` | `{apiKeys:[{key,userId,rateLimitBudget,enabled,createdAt,updatedAt}], nextToken, hasMore, totalReturned}` | `erpc/admin.go:47-48,195-291`; ApiKey shape `erpc/admin.go:16-23` |
| `erpc_updateApiKey` | `params[0]` = `{projectId!, connectorId!, apiKey!, updates! (object; null value deletes key)}` | `{success, apiKey, updated}` | `erpc/admin.go:49-50,294-381` |
| `erpc_deleteApiKey` | `params[0]` = `{projectId!, connectorId!, apiKey!}` | `{success, apiKey, userId}` | `erpc/admin.go:51-52,384-453` |
| `erpc_cordonUpstream` | `params[0]` = `{projectId!, upstream!, method? (default "*"), reason? (default "admin: manual cordon")}` | `{projectId, upstream, method, cordoned:true, reason}` | `erpc/admin.go:53-54,606-687` |
| `erpc_uncordonUpstream` | same as cordon (default reason "admin: manual uncordon") | `{..., cordoned:false, ...}` | `erpc/admin.go:55-56,658-687` |
| `erpc_listCordoned` | `params[0]` = `{projectId!}` | `{projectId, cordoned:[{upstream, reason}]}` (only `"*"`-scope cordons listed) | `erpc/admin.go:57-58,692-729` |

API-key methods resolve the storage connector from the project's consumer-auth registry database strategies (`erpc/admin.go:82-100`).

---

## 4. CLI subcommands and flags (`cmd/erpc`)

Built with `urfave/cli/v3` (`cmd/erpc/main.go:24`). Binary: `erpc`. `Version: common.ErpcVersion` enables the library's version flag (`cmd/erpc/main.go:223`; version values: `"dev"` / commit `"none"` unless overridden by ldflags `common/config.go:23-31`).

| Command | Flags | Behavior | Source |
|---|---|---|---|
| `erpc [config file]` (root, no subcommand) | `--config <file>`, `--endpoint/-e <url>` (repeatable), `--require-config` (bool) | Same as `start`: load config, run `erpc.Init` | `cmd/erpc/main.go:219-244`; flags defined `cmd/erpc/main.go:76-94` |
| `erpc start` | `--endpoint/-e` (repeatable), `--require-config`; root `--config` also honored via flag lookup | Start the service | `cmd/erpc/main.go:200-216`; test proving `start --config` works: `cmd/erpc/main_test.go:81-105` |
| `erpc validate` | `--format json\|md` (default `json`) | Validation report; exit 1 on config-load error or any report errors (`util.OsExit(1)`); logs suppressed | `cmd/erpc/main.go:96-142` |
| `erpc dump` | `--format yaml\|json` (default `yaml`) | Dump effective config (selection policies resolved via `policy.ResolveEffectiveSelectionPolicies`); exit 1 on load/marshal error or unsupported format | `cmd/erpc/main.go:144-198` |

| Flag | Type | Default | Notes | Source |
|---|---|---|---|---|
| `--config` | string | "" | Config file; setting it (or a positional arg) implies require-config | `cmd/erpc/main.go:76-80,300-305` |
| `--endpoint` / `-e` | string slice | [] | Endpoint URLs when running config-less; each validated with `url.ParseRequestURI` | `cmd/erpc/main.go:81-85,328-336` |
| `--require-config` | bool | false | Refuse default project / public endpoints without a config file | `cmd/erpc/main.go:91-94,338-341` |
| `--set` / `-s` | — | — | COMMENTED OUT (not available) | `cmd/erpc/main.go:86-90,297` |

Config resolution order: `--config` flag → first positional arg → search list `./erpc.yaml`, `./erpc.yml`, `./erpc.ts`, `./erpc.js`, `/erpc.yaml`, `/erpc.yml`, `/erpc.ts`, `/erpc.js`, `/root/erpc.yaml`, `/root/erpc.yml`, `/root/erpc.ts`, `/root/erpc.js` (`cmd/erpc/main.go:279-322`). No config and no `--require-config` → built-in defaults via `cfg.SetDefaults` (`cmd/erpc/main.go:348-352`). Exit codes: start failure 1001 (`cmd/erpc/main.go:245-248`, `util/exit.go:8`), HTTP/metrics/gRPC server failure 1002 (`erpc/init.go:120,133,156`, `util/exit.go:9`). A `.env` file in cwd is auto-loaded at init via godotenv (`cmd/erpc/main.go:41-46`). pprof build tag adds an HTTP pprof server on port `ERPC_PPROF_PORT` (default 6060) (`cmd/erpc/pprof.go:15-26`).

---

## 5. Environment variables

| Variable | Effect | Source |
|---|---|---|
| `LOG_LEVEL` | zerolog global level at process init; also re-applied after config load (overrides `logLevel` config) | `cmd/erpc/main.go:59-66`, `cmd/erpc/main.go:354-363`; test logger `util/testing.go:25` |
| `LOG_WRITER` | `"console"` → human console writer instead of JSON | `cmd/erpc/main.go:53-57` |
| `ERPC_NOLOGS` | `"1"` → disable all zerolog output | `cmd/erpc/initflags.go:16-19` (build tag `!test`) |
| `ERPC_NOMETRICS` | `"1"` → swap default Prometheus registry for an empty one (metrics no-op) | `cmd/erpc/initflags.go:22-28` |
| `ERPC_PPROF_PORT` | pprof listen port (default `6060`); only with `pprof` build tag | `cmd/erpc/pprof.go:19-22` |
| `ERPC_IGNORE_LOCAL_ENDPOINT_VALIDATION` | `"true"` → config analyzer skips non-HTTP and local upstream endpoints during validation | `erpc/config_analyzer.go:1024-1031` |
| `FORCE_TEST_LISTEN_V4` | `"true"` → enable default `listenV4` even under `go test` | `common/defaults.go:640-643` |
| `INSTANCE_ID` | instance identity for shared-state remote-sync values and consensus dispute-log filenames (priority 1) | `data/shared_state_registry.go:82-84`, `consensus/export_utils.go:29-32` |
| `POD_NAME` | same, priority 2 (Kubernetes) | `data/shared_state_registry.go:86-88`, `consensus/export_utils.go:33-36` |
| `HOSTNAME` | same, priority 3 (then `os.Hostname()` / hash fallback) | `data/shared_state_registry.go:89-91`, `consensus/export_utils.go:37-40` |
| `OPENAI_API_KEY` | consensus-DSL test-data generation harness only (not server runtime) | `util/dsl.go:161-164` |
| Any `${VAR}` / `$VAR` in YAML config | whole config file is `os.ExpandEnv`-ed before YAML decode (YAML path only, not TS) | `common/config.go:102` |
| Any `${VAR}` / `$VAR` in `server.responseHeaders` values | expanded once at server startup; empty result drops the header | `erpc/http_server.go:135-148` |
| Any `${VAR}` / `$VAR` in provider-generated upstream endpoints | expanded when provider builds upstream configs | `thirdparty/provider.go:67-71` |
| ALL env vars (TS config) | entire environment exposed to TS/JS config runtime as `process.env.<KEY>` object and `env` array | `common/runtime.go:19-36` |
| `.env` file | auto-loaded into process env at CLI init (godotenv) | `cmd/erpc/main.go:41-46` |

Note (library-implicit, not read by eRPC code): OTLP trace exporters are constructed from `tracing` config (`common/tracing_core.go:186-204`); the OpenTelemetry SDK itself additionally honors standard `OTEL_*` env vars. TS tooling-only env reads (not server runtime): `CI` (`typescript/cli/src/install.ts:111`), `VERSION`/`COMMIT_SHA`/`CHECKSUMS_FILE` (`typescript/cli/src/script/generate-release-files.ts:65-67`).

---

## 6. Response headers set by eRPC

### Always (every HTTP response from the main handler)

| Header | Value | Source |
|---|---|---|
| `Content-Type` | `application/json` | `erpc/http_server.go:259` (healthcheck JSON mode re-sets: `erpc/healthcheck.go:366`) |
| `X-ERPC-Version` | `common.ErpcVersion` (`"dev"` unless ldflags) | `erpc/http_server.go:260`, `common/config.go:23-26` |
| `X-ERPC-Commit` | `common.ErpcCommitSha` (`"none"` unless ldflags) | `erpc/http_server.go:261`, `common/config.go:28-31` |
| custom `server.responseHeaders` map | static values, env-expanded at startup | `erpc/http_server.go:263-266,135-148`; config `common/config.go:160` |

### Execution-diagnostic headers (`setResponseHeaders`, single-response path only — NOT emitted for batch responses)

Controlled by `server.executionHeaders`: `all` (default) / `summary` / `off` (`common/config.go:162-184`; default resolution `erpc/http_server.go:1081-1086`; `off` short-circuits `erpc/http_server.go:1106-1108`; `summary` skips only the per-attempt trace header `erpc/http_server.go:1124-1126`). Called for success (`erpc/http_server.go:723`) and error paths (`erpc/http_server.go:1496`); batch branch never calls it (`erpc/http_server.go:703-720`).

| Header | Value | When | Source |
|---|---|---|---|
| `X-ERPC-Attempts` | int, total physical ops (upstream + cache; network rotations NOT summed) | always (0 when no ExecState) | `erpc/http_server.go:1160`, contract comment `:1151-1157` |
| `X-ERPC-Upstream-Attempts` | int | always | `erpc/http_server.go:1161` |
| `X-ERPC-Upstream-Retries` | int | always | `erpc/http_server.go:1162` |
| `X-ERPC-Upstream-Hedges` | int | always | `erpc/http_server.go:1163` |
| `X-ERPC-Network-Attempts` | int | always | `erpc/http_server.go:1164` |
| `X-ERPC-Network-Retries` | int | always | `erpc/http_server.go:1165` |
| `X-ERPC-Network-Hedges` | int | always | `erpc/http_server.go:1166` |
| `X-ERPC-Cache-Attempts` / `X-ERPC-Cache-Retries` / `X-ERPC-Cache-Hedges` | int | only when any cache counter > 0 | `erpc/http_server.go:1170-1174` |
| `X-ERPC-Consensus-Slots` | int | only when > 0 | `erpc/http_server.go:1175-1177` |
| `X-ERPC-Consensus-Disputes` | int | only when > 0 | `erpc/http_server.go:1178-1180` |
| `X-ERPC-Consensus-Low-Participants` | int | only when > 0 | `erpc/http_server.go:1181-1183` |
| `X-ERPC-Cache` | `HIT` / `MISS` | when response metadata exists & non-null | `erpc/http_server.go:1189-1196` |
| `X-ERPC-Upstream` | winning upstream id | when known | `erpc/http_server.go:1197-1199` |
| `X-ERPC-Duration` | int milliseconds | NormalizedResponse only | `erpc/http_server.go:1201-1203` |
| `X-ERPC-Upstreams` | per-attempt trace: `<upstreamId>=<reason>:<outcome>:<ms>ms[:won]` segments joined by `;` (e.g. `alchemy=primary:success:50ms:won;quicknode=hedge:timeout:5000ms`) | mode `all` only, when attempts logged | `erpc/http_server.go:1226-1252`, format `:1257-1272` |

### Conditional / other

| Header | When | Source |
|---|---|---|
| `traceparent` (+ `tracestate`) | tracing enabled; W3C TraceContext injected into response (single AND batch paths) | `erpc/http_server.go:701`, `common/tracing_util.go:58-65` |
| `Access-Control-Allow-Origin` | CORS-allowed origin | `erpc/http_server.go:1054` |
| `Access-Control-Allow-Methods` | joined `cors.allowedMethods` (default `GET, POST, OPTIONS`, `common/defaults.go:2828-2830`) | `erpc/http_server.go:1055` |
| `Access-Control-Allow-Headers` | joined `cors.allowedHeaders` (default `content-type, authorization, x-erpc-secret-token`, `common/defaults.go:2831-2837`) | `erpc/http_server.go:1056` |
| `Access-Control-Expose-Headers` | joined `cors.exposedHeaders` (no default) | `erpc/http_server.go:1057` |
| `Access-Control-Allow-Credentials` | `true` only when `cors.allowCredentials` (default false, `common/defaults.go:2838-2840`) | `erpc/http_server.go:1059-1061` |
| `Access-Control-Max-Age` | `cors.maxAge` seconds (default 3600, `common/defaults.go:2841-2843`) | `erpc/http_server.go:1063-1065` |
| `Content-Encoding: gzip` + `Vary: Accept-Encoding` | `server.enableGzip` AND client sent `Accept-Encoding: gzip` AND first write ≥ 1024 bytes (`compressionThreshold` `erpc/http_server.go:36`); `Content-Length` deleted | `erpc/http_server.go:1699-1735,1757-1780` |

### Request headers consumed by the server (non-directive)

| Header | Use | Source |
|---|---|---|
| `Host` | domain-aliasing rule match (port stripped) | `erpc/http_server.go:232-257` |
| `Content-Encoding: gzip` | request-body decompression | `erpc/http_server.go:351` |
| `Accept-Encoding` | response gzip eligibility | `erpc/http_server.go:1763` |
| `Origin` | CORS evaluation | `erpc/http_server.go:1009` |
| `Content-Type: application/grpc` | route HTTP/2 frames to shared gRPC server | `erpc/http_server.go:168` |
| `traceparent`/`tracestate` | trace-context extraction | `common/tracing_util.go:67-73` |
| configured `server.trustedIPHeaders` (XFF-style lists) | real-client-IP resolution, only when peer is in `server.trustedIPForwarders` | `erpc/http_server.go:1782-1825`; config `common/config.go:158-159` |
| any header matching `projects[].forwardHeaders` wildcards | copied onto `nq.ForwardHeaders` for upstream forwarding | `erpc/http_server.go:478-494` |

---

## 7. gRPC surface (bonus: non-HTTP routes + metadata "directives")

Services registered (`erpc/grpc_server.go:113-116`): `evm.RPCQueryService` and `evm.QueryService` (blockchain-data-standards manifesto protos).

| RPC | Maps to JSON-RPC method | Kind | Source |
|---|---|---|---|
| `ChainId` | `eth_chainId` | unary | `erpc/grpc_server.go:160-182` |
| `GetBlockByNumber` | `eth_getBlockByNumber` | unary | `erpc/grpc_server.go:184-214` |
| `GetBlockByHash` | `eth_getBlockByHash` | unary | `erpc/grpc_server.go:216-246` |
| `GetLogs` | `eth_getLogs` | unary | `erpc/grpc_server.go:248-309` |
| `GetTransactionByHash` | `eth_getTransactionByHash` | unary | `erpc/grpc_server.go:311-336` |
| `GetTransactionReceipt` | `eth_getTransactionReceipt` | unary | `erpc/grpc_server.go:338-363` |
| `GetBlockReceipts` | `eth_getBlockReceipts` | unary | `erpc/grpc_server.go:365-399` |
| `QueryBlocks` | `eth_queryBlocks` | server-stream | `erpc/grpc_server.go:401-409` |
| `QueryTransactions` | `eth_queryTransactions` | server-stream | `erpc/grpc_server.go:411-419` |
| `QueryLogs` | `eth_queryLogs` | server-stream | `erpc/grpc_server.go:421-429` |
| `QueryTraces` | `eth_queryTraces` | server-stream | `erpc/grpc_server.go:431-439` |
| `QueryTransfers` | `eth_queryTransfers` | server-stream | `erpc/grpc_server.go:441-449` |

gRPC metadata keys (replace URL path + headers):

| Metadata key | Required | Use | Source |
|---|---|---|---|
| `x-erpc-project` | yes (InvalidArgument if missing) | project id | `erpc/grpc_server.go:138-141` |
| `x-erpc-chain-id` | yes | chain id | `erpc/grpc_server.go:142-145` |
| `x-erpc-architecture` | no (default `evm`) | architecture | `erpc/grpc_server.go:152` |
| `x-erpc-secret-token` | no | secret auth | `auth/grpc.go:15-17` |
| `authorization` (`Basic`/`Bearer`) | no | secret/jwt auth | `auth/grpc.go:18-39` |
| `x-siwe-message` + `x-siwe-signature` | no | SIWE auth | `auth/grpc.go:40-48` |
| `user-agent` | no | agent tracking | `erpc/grpc_server.go:156` |
| configured `server.trustedIPHeaders` (lowercased) | no | client-IP resolution from metadata | `erpc/grpc_server.go:89-95,512-532` |

Note: HTTP X-ERPC-* request directives are NOT parsed on the gRPC path — `RequestProcessor.ProcessUnary` applies only network `directiveDefaults` and never calls `EnrichFromHttp` (`erpc/request_processor.go:35-67`). gRPC error mapping: `erpc/grpc_server.go:475-495`.
