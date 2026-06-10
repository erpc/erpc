# KB: Auth: all strategies (secret, jwt, siwe, network, database)

> status: complete
> source-dirs: auth/*.go, common/config.go (AuthConfig + strategy configs, L2433-L2534; ConnectorConfig L341-L353; GrpcConnectorConfig L354-L359; MemoryConnectorConfig L361-L365; RedisConnectorConfig L383-L395; DynamoDBConnectorConfig L428-L443; PostgreSQLConnectorConfig L445-L453; AwsAuthConfig L479-L495; FailsafeConfig L1279-L1287), common/defaults.go (auth SetDefaults L2611-L2761; ConnectorConfig.SetDefaults L846-L933; MemoryConnectorConfig.SetDefaults L935-L943; RedisConnectorConfig.SetDefaults L946-L1018; PostgreSQLConnectorConfig.SetDefaults L1021-L1059; DynamoDBConnectorConfig.SetDefaults L1061-L1100), common/data.go (DataFinalityState enum L8-L36), common/user.go, common/request.go (SetUser/User/UserId/ClientIP, L402-L1282), common/errors.go (ErrAuthUnauthorized/ErrAuthRateLimitRuleExceeded, L524-L568), erpc/admin.go, erpc/projects.go, erpc/projects_registry.go, erpc/erpc.go, erpc/http_server.go (payload extraction + auth call, L552-L584), erpc/grpc_server.go (gRPC payload extraction, L133-L157), erpc/healthcheck.go (healthcheck auth, L85-L104), telemetry/metrics.go (MetricAuthFailedTotal, L659-L663), data/memory.go (emitMetrics L71-L99; metricsCollectionLoop L210-L226), data/dynamodb.go (AwsAuth credential construction L177-L187), data/redis.go (TLS merge logic L149-L179), data/grpc.go (bootstrap + servers resolution L78-L98)

## L1 — Capability (CTO view)

eRPC's auth subsystem is a pluggable, per-request gate that runs on every incoming JSON-RPC request before it is forwarded to any upstream. It supports five strategies — `secret` (static token/API key), `jwt` (signed bearer token), `siwe` (EIP-4361 Ethereum sign-in), `network` (allowlist IP/CIDR), and `database` (dynamic API key lookup against PostgreSQL, DynamoDB, Redis, or any supported connector) — which can be stacked per project and applied selectively to methods. Auth is configured independently at three scopes: per-project consumer traffic (`projects[*].auth`), the admin API (`admin.auth`), and the optional healthcheck endpoint (`healthCheck.auth`). The `database` strategy includes an in-process Ristretto cache, a singleflight deduplicator for concurrent misses, configurable retry/backoff, and a fail-open circuit-breaker pattern that was hardened after a 2026-05-13 production incident where DB outage triggered a log-write fd-lock cascade.

**Note on x402:** the x402 nanopayment strategy does NOT exist in this codebase. `grep -ri x402` over all `.go`/`.ts`/`.yaml` files returns nothing. Auth strategies are exactly the five types enumerated in `common/config.go:L2436-L2440`.

## L2 — Mechanics (staff-engineer view)

**Three auth scopes and their registries.** At startup three independent `AuthRegistry` instances can be created:

1. **Admin scope** — created in `erpc/erpc.go:L83-L86` using `cfg.Admin.Auth` with project-id `"admin"`. Referenced as `e.adminAuthRegistry`. If `admin.auth` is nil no registry is created and the admin endpoint is always denied (`erpc/admin.go:L28-L29`, HTTP handler path `erpc/http_server.go:L597-L608`).
2. **Project/consumer scope** — created per-project in `erpc/projects_registry.go:L187-L191` using `prjCfg.Auth` with the project's own id. Stored as `pp.consumerAuthRegistry`. If the project has no auth config (`prjCfg.Auth == nil`), the registry is nil and `AuthenticateConsumer` returns `nil, nil` (allow-all, `erpc/projects.go:L94-L99`).
3. **Healthcheck scope** — created in `erpc/http_server.go:L201-L205` using `healthCheckCfg.Auth` with project-id `"healthcheck"`. Pass `nil` for the `rateLimitersRegistry` argument (healthcheck auth does not support rate-limit budgets, `erpc/http_server.go:L203`).

**Request flow for HTTP.** For every sub-request in the handler goroutine (single or batch fan-out):
1. `auth.NewPayloadFromHttp(method, r.RemoteAddr, headers, queryArgs)` extracts credential signals from query params and headers in a fixed priority order (`auth/http.go:L13-L88`).
2. `project.AuthenticateConsumer(ctx, nq, method, ap)` (project scope) or `s.erpc.AdminAuthenticate(ctx, nq, method, ap)` (admin scope) is called (`erpc/http_server.go:L566-L584`).
3. `AuthRegistry.Authenticate` iterates strategies in config order, skipping those that do not pass `shouldApplyToMethod` and `Supports`, calling `strategy.Authenticate` on the first match. First success wins; auth errors from non-matching strategies are silently dropped; auth errors from matching strategies are accumulated and joined if no strategy succeeds (`auth/registry.go:L46-L96`).
4. On success the `*common.User` (with `.Id` and optional `.RateLimitBudget`) is stored on the `NormalizedRequest` via `req.SetUser(user)` (`auth/registry.go:L74-L76`, `erpc/http_server.go:L583`). The user id then flows into log context and Prometheus labels.
5. **Rate-limit budget** is applied after strategy authentication: `acquireRateLimitPermit` picks the effective budget — `user.RateLimitBudget` (per-user override, set by strategy) > `strategy.RateLimitBudget` (strategy-level default) — and calls `rateLimitersRegistry.GetBudget(id).TryAcquirePermit(...)` with `origin="auth"` and `authLabel="<type>:<index>"` (`auth/authorizer.go:L117-L157`). Budgets with no matching rules pass immediately.

**Request flow for gRPC.** `GrpcServer.extractRequestInput` reads gRPC metadata and calls `auth.NewPayloadFromGrpc(method, md)` (`erpc/grpc_server.go:L146`). The resulting `*RequestInput.AuthPayload` is passed through `request_processor.go` which calls `project.AuthenticateConsumer` (`erpc/request_processor.go:L52`, `L112`). gRPC auth has **no query-param or `?token=` / `?secret=` support** — only metadata headers are examined (`auth/grpc.go`).

**HTTP payload extraction priority (first match wins):**
1. `?token=` (deprecated alias for `?secret=`)
2. `?secret=` → `AuthTypeSecret`
3. `X-ERPC-Secret-Token` header → `AuthTypeSecret`
4. `Authorization: Basic <b64(user:pass)>` → password part → `AuthTypeSecret` (username is discarded)
5. `Authorization: Bearer <token>` → `AuthTypeJwt`
6. `?jwt=` → `AuthTypeJwt`
7. `?signature=` + `?message=` → `AuthTypeSiwe`
8. `X-Siwe-Message` + `X-Siwe-Signature` headers → `AuthTypeSiwe`
9. *fallback* → `AuthTypeNetwork` (the network strategy is the only one invoked when no credential is presented)

**Strategy selection.** `AuthRegistry.Authenticate` iterates all configured strategies in declaration order. Each strategy's `Supports(ap)` method acts as a type filter — it checks `ap.Type`. The network strategy accepts only `AuthTypeNetwork`; secret accepts only `AuthTypeSecret`; database accepts `AuthTypeSecret` **or** `AuthTypeDatabase`; jwt accepts `AuthTypeJwt`; siwe accepts `AuthTypeSiwe`. Strategies whose `Supports` returns false are skipped entirely (no error added). Only strategies whose `Supports` returns true AND whose `shouldApplyToMethod` passes are eligible to produce an auth error.

**Method filtering.** `shouldApplyToMethod` applies `ignoreMethods` first (wildcard match, if any match → `shouldApply = false`), then `allowMethods` (wildcard match, if any match → `shouldApply = true` override). This means `allowMethods` always overrides `ignoreMethods`. The canonical pattern for "only allow `eth_getLogs`" is `ignoreMethods: ["*"]` + `allowMethods: ["eth_getLogs"]` (`auth/authorizer.go:L83-L114`).

**Database strategy — connector-down circuit-breaker.** This was added after the 2026-05-13 production incident. The strategy maintains two atomic values: `connectorDown bool` and `connectorDownSince int64` (unix-nanos). When a DB error classified as transport/timeout/not-ready occurs, `markConnectorDown()` is called (CAS-guarded so only one goroutine logs the transition). While `connectorDown` is true, `tryFastFailOpen()` returns the emergency user immediately — bypassing singleflight, connector.Get, error logging, and metric increment — for all callers EXCEPT one probe per `connectorDownProbeInterval` (1 second). The elected probe (CAS on `connectorDownSince`) runs the real DB path; if it succeeds, `markConnectorUp()` clears the latch. This bounds per-request DB load and log volume during a sustained outage to approximately 1 query/second (`auth/strategy_database.go:L25-L54`, `L271-L357`).

**SIWE message normalization.** The `message` payload (query param or header) is attempted as base64-decode first; if that fails the raw string is used as-is. This allows both raw EIP-4361 messages and base64-encoded messages (`auth/http.go:L90-L96`).

**User object and rate-limit budget cascade.** A `common.User{Id, RateLimitBudget}` is returned from each strategy. `RateLimitBudget` can be set at three levels, with descending priority:
1. Per-user from strategy (secret: `SecretStrategyConfig.RateLimitBudget`; siwe: `SiweStrategyConfig.RateLimitBudget`; network: `NetworkStrategyConfig.RateLimitBudget`; database: record's `rateLimitBudget` field; jwt: claim named by `rateLimitBudgetClaimName`).
2. Strategy-level default from `AuthStrategyConfig.RateLimitBudget`.
3. No budget → `acquireRateLimitPermit` is a no-op (all requests pass).

In `acquireRateLimitPermit`, the user's budget (if set) overrides the strategy-level budget (`auth/authorizer.go:L119-L127`).

## L3 — Exhaustive reference (agent view)

### Config fields

#### `auth` attachment points

| YAML path | Type | Notes |
|---|---|---|
| `projects[*].auth` | `*AuthConfig` | Consumer auth for that project. Nil = allow-all. `common/config.go:L509` |
| `admin.auth` | `*AuthConfig` | Admin endpoint auth. Nil = admin endpoint always returns error. `common/config.go:L249` |
| `healthCheck.auth` | `*AuthConfig` | Healthcheck endpoint auth. Nil = healthcheck is unguarded. `common/config.go:L188` |

#### `AuthConfig`

| YAML path | Go type | Default | Notes |
|---|---|---|---|
| `auth.strategies` | `[]*AuthStrategyConfig` | nil (no strategies = allow-all) | Ordered list; first strategy that `Supports` the payload type AND passes method filters AND authenticates successfully wins. `common/config.go:L2444` |

#### `AuthStrategyConfig` (common fields, all strategies)

| YAML path | Go type | Default | Notes | Source |
|---|---|---|---|---|
| `auth.strategies[*].type` | `AuthType` string | **Inferred/overwritten** from sub-config block presence (see type-inference algorithm below); no literal default if no sub-config is present | Determines which sub-config is required and which `Supports()` impl is used. If a `secret` block is present, `type` is forced to `"secret"` regardless of the `type` field value. Same for `database`, `jwt`, `siwe`. `network` block does NOT overwrite `type` — it only initializes the sub-struct when `type == "network"`. `common/config.go:L2452` | `common/defaults.go:L2636-L2666` |
| `auth.strategies[*].ignoreMethods` | `[]string` | `nil` (no methods ignored) | Wildcard patterns (supports `*`, `\|`, `&`, `!`, parens via `common.WildcardMatch`). Applied before `allowMethods`. `common/config.go:L2448` | `auth/authorizer.go:L86-L98` |
| `auth.strategies[*].allowMethods` | `[]string` | `nil` (no override) | Wildcard patterns. Overrides `ignoreMethods`; any matching allow re-enables the strategy for that method. `common/config.go:L2449` | `auth/authorizer.go:L100-L113` |
| `auth.strategies[*].rateLimitBudget` | `string` | `""` (no budget) | Strategy-level budget ID. Overridden per-user by the user object's `RateLimitBudget` field when set. `common/config.go:L2450` | `auth/authorizer.go:L119-L127` |

#### `secret` strategy — `SecretStrategyConfig`

YAML prefix: `auth.strategies[*].secret`

| Field | Go type | Default | Notes | Source |
|---|---|---|---|---|
| `secret.id` | `string` | `""` | Used as the `User.Id` returned on success. May be empty — callers may see `User{Id:""}`. `common/config.go:L2461` | `auth/strategy_secret.go:L28` |
| `secret.value` | `string` | required | The exact token to match. Compared by string equality only (no hashing, no timing-safe compare). **Redacted** in JSON/YAML marshal (`common/config.go:L2468-L2480`). | `auth/strategy_secret.go:L24` |
| `secret.rateLimitBudget` | `string` | `""` | Attached to the returned `User.RateLimitBudget` if non-empty. `common/config.go:L2464` | `auth/strategy_secret.go:L29-L31` |

`SetDefaults`: no-op (`common/defaults.go:L2744-L2746`).

`Supports`: only `AuthTypeSecret` (`auth/strategy_secret.go:L19-L21`).

`Authenticate`: exact equality match `ap.Secret.Value != s.cfg.Value`. Returns `ErrAuthUnauthorized("secret", "invalid secret")` on mismatch.

#### `jwt` strategy — `JwtStrategyConfig`

YAML prefix: `auth.strategies[*].jwt`

| Field | Go type | Default | Notes | Source |
|---|---|---|---|---|
| `jwt.verificationKeys` | `map[string]string` | `nil` | Map of `kid → keyData`. `keyData` may be: (a) a PEM-encoded RSA/EC public key string, (b) `"file:///path/to/key.pem"` (file read at startup), (c) a bare string treated as HMAC-SHA secret. Multiple keys for key rotation. **CRITICAL SECURITY FOOTGUN**: when `verificationKeys` is `nil` or an empty map, `s.keys` is also empty — `findVerificationKey` will iterate an empty map and immediately return `"no suitable verification key found"`. In this case, `jwt.Parse` is called with a keyfunc that always returns an error → **JWT signature validation is effectively skipped** and any token (including expired or malformed ones) will fail the signature check and be rejected. **Wait — this is the opposite of "any token passes"**: an empty key map means ALL JWTs are rejected (no key can be found to verify any signature). The only way JWT auth accepts any token is if at least one key is configured that matches the token's algorithm. `common/config.go:L2513` | `auth/strategy_jwt.go:L25-L42`, `L100-L116` |
| `jwt.allowedAlgorithms` | `[]string` | `nil` (any algorithm accepted) | e.g. `["RS256","ES256"]`. Validated against `token.Method.Alg()`. `common/config.go:L2511` | `auth/strategy_jwt.go:L54-L58` |
| `jwt.allowedIssuers` | `[]string` | `nil` (any issuer accepted) | Exact string match against `iss` claim. If list non-empty and `iss` missing, returns error. `common/config.go:L2509` | `auth/strategy_jwt.go:L123-L130` |
| `jwt.allowedAudiences` | `[]string` | `nil` (any audience accepted) | Exact match against `aud` claim (string only, not array). If list non-empty and `aud` missing, returns error. `common/config.go:L2510` | `auth/strategy_jwt.go:L132-L139` |
| `jwt.requiredClaims` | `[]string` | `nil` (no extra claims required) | Claim names that must be present (any value). `common/config.go:L2512` | `auth/strategy_jwt.go:L143-L148` |
| `jwt.rateLimitBudgetClaimName` | `string` | `"rlm"` (set by `SetDefaults` when empty, `common/defaults.go:L2749-L2751`) | The JWT claim name from which the rate-limit budget ID is extracted. After successful signature and claims validation, the strategy reads `claims[claimName]` as a string and assigns it to `User.RateLimitBudget` (`auth/strategy_jwt.go:L87-L95`). **When the claim is absent from the token**, `User.RateLimitBudget` is left empty — no budget is applied (all requests pass the rate-limit check). **When the claim is present but its value is not a string** (e.g., an integer), the type assertion fails silently and no budget is applied. The budget ID from the claim overrides the strategy-level `rateLimitBudget` field (per-user budget takes priority, `auth/authorizer.go:L119-L127`). To disable per-token budgets and use only the strategy-level budget, set `rateLimitBudgetClaimName: ""` — but note `SetDefaults` re-sets it to `"rlm"` if the field was never declared; to truly suppress it, configure a claim name that is never present in your tokens. `common/config.go:L2517` | `auth/strategy_jwt.go:L87-L95`, `common/defaults.go:L2749-L2751` |

`SetDefaults`: sets `RateLimitBudgetClaimName = "rlm"` if empty (`common/defaults.go:L2748-L2752`).

`Supports`: only `AuthTypeJwt` (`auth/strategy_jwt.go:L44-L46`).

`Authenticate` flow:
1. `jwt.Parser.ParseUnverified` with `jwt.WithoutClaimsValidation()` to extract header (`kid`, `alg`) and claims without signature check.
2. Algorithm allowlist check (if configured).
3. Key lookup: if `kid` present in header, look up in `s.keys[kid]`; else iterate all keys and pick the first whose type is compatible with the algorithm (RSA key for RS*, EC key for ES*, bytes for HS*).
4. Full `jwt.Parse` with the found key function — validates signature.
5. `claims.Valid()` — validates `exp`, `nbf`, `iat` (standard JWT temporal claims).
6. Issuer/audience/required-claims validation.
7. Extract `sub` as `User.Id` (missing `sub` → error).
8. Extract optional `rlm` (or configured claim name) as `User.RateLimitBudget`.

**Footgun**: `jwt.Parser` is created with `jwt.WithoutClaimsValidation()` at init time. The `ParseUnverified` call intentionally skips claim validation to read the `kid`. Claim validation happens in step 5 via `claims.Valid()` during the second `jwt.Parse` call (`auth/strategy_jwt.go:L38-L42`, `L48-L49`, `L66-L69`).

#### `siwe` strategy — `SiweStrategyConfig`

YAML prefix: `auth.strategies[*].siwe`

| Field | Go type | Default | Notes | Source |
|---|---|---|---|---|
| `siwe.allowedDomains` | `[]string` | `nil` (no domain allowed — rejects all) | Exact string match against the `domain` field of the EIP-4361 message. Empty list → all SIWEs rejected. `common/config.go:L2521` | `auth/strategy_siwe.go:L59-L67` |
| `siwe.rateLimitBudget` | `string` | `""` | Attached to `User.RateLimitBudget` if non-empty. `common/config.go:L2523` | `auth/strategy_siwe.go:L53-L55` |

`SetDefaults`: no-op (`common/defaults.go:L2755-L2757`).

`Supports`: only `AuthTypeSiwe` (`auth/strategy_siwe.go:L22-L24`).

`Authenticate` flow:
1. `siwe.ParseMessage(ap.Siwe.Message)` — parses EIP-4361 message.
2. `message.VerifyEIP191(ap.Siwe.Signature)` — cryptographic signature verification.
3. Domain allowlist check (`isDomainAllowed`).
4. `message.ValidNow()` — verifies message is within its validity window (expiration / not-before from the SIWE spec).
5. Returns `User{Id: strings.ToLower(message.GetAddress().String())}` — Ethereum address as lowercase hex.

**Transport**: the SIWE message payload is base64-decoded if possible; raw string used otherwise. This allows both forms over the wire (`auth/http.go:L90-L96`, `auth/grpc.go` calls same `normalizeSiweMessage`).

#### `network` strategy — `NetworkStrategyConfig`

YAML prefix: `auth.strategies[*].network`

| Field | Go type | Default | Notes | Source |
|---|---|---|---|---|
| `network.allowedIPs` | `[]string` | `nil` | Exact IP addresses (IPv4 or IPv6). Parsed at startup; invalid IP → startup error. `common/config.go:L2527` | `auth/strategy_network.go:L26-L33` |
| `network.allowedCIDRs` | `[]string` | `nil` | CIDR ranges. Parsed at startup; invalid CIDR → startup error. `common/config.go:L2528` | `auth/strategy_network.go:L35-L44` |
| `network.allowLocalhost` | `bool` | `false` (zero value; no SetDefault sets it) | When true, any loopback address (`ip.IsLoopback()`, `127.0.0.1`, `::1`) is allowed regardless of `allowedIPs`/`allowedCIDRs`. `common/config.go:L2529` | `auth/strategy_network.go:L63-L69` |
| `network.allowLocalhost` | `bool` | `false` (zero value; no SetDefault sets it) | When `true`, any loopback address (`ip.IsLoopback()`, covering `127.0.0.1` and `::1`, checked via `isLocalhost` helper `auth/strategy_network.go:L99-L101`) is allowed **unconditionally** — the check runs before `allowedIPs` and `allowedCIDRs`. `User.Id` is set to `clientIP.String()`. Useful for sidecar deployments or internal health probes that originate from localhost but should not appear in the allowedIPs list. `common/config.go:L2529` | `auth/strategy_network.go:L63-L71` |
| `network.trustedProxies` | `[]string` | `nil` | **This field is NOT USED by NetworkStrategy itself and is present in config only.** The network strategy never reads `trustedProxies` (`auth/strategy_network.go` contains no reference to this field). **Why it is safe but useless here**: client IP resolution (X-Forwarded-For unwrapping) is architecturally the responsibility of the HTTP server layer, which uses `server.trustedIPForwarders` / `server.trustedIPHeaders` config fields to resolve the real client IP before any auth strategy runs. By the time `NetworkStrategy.Authenticate` is called, `req.ClientIP()` already contains the correctly resolved IP. **Actionable guidance**: to enable X-Forwarded-For unwrapping, configure `server.trustedIPForwarders` or `server.trustedIPHeaders` at the server level — not this field. Setting `network.trustedProxies` to any value has zero effect on IP resolution. `common/config.go:L2530` | `auth/strategy_network.go` (field unused) |
| `network.rateLimitBudget` | `string` | `""` | Attached to `User.RateLimitBudget` for all IPs that match via localhost, exact IP, or CIDR. `common/config.go:L2532` | `auth/strategy_network.go:L65-L68`, `L75-L79`, `L88-L93` |
| `network.ipAsUser` | `bool` | `false` | Controls `User.Id` assignment when the client IP matches via a CIDR range (not an exact IP). When `false` (default), `User.Id` is set to `cidr.String()` (the CIDR notation, e.g. `"10.0.0.0/8"`) — meaning all IPs in that range share one user identity and one rate-limit bucket. When `true`, `User.Id` is set to `clientIP.String()` (the actual individual IP) — each unique IP gets its own user identity and its own rate-limit tracking. For allowLocalhost and exact `allowedIPs` matches, `User.Id` is always `clientIP.String()` regardless of `ipAsUser`. `common/config.go:L2533` | `auth/strategy_network.go:L63-L93` |

`SetDefaults`: no-op (`common/defaults.go:L2759-L2761`).

`Supports`: only `AuthTypeNetwork` (`auth/strategy_network.go:L47-L49`). The network strategy is the **default fallback** — it is invoked for any request that carries no credential signal (see payload extraction fallback at `auth/http.go:L83-L87`, `auth/grpc.go:L50-L53`).

`Authenticate`: reads `req.ClientIP()` (already proxy-resolved by HTTP ingress). Checks localhost → exact IP list → CIDR list in order. First match wins. No match → `ErrAuthUnauthorized("network", "IP X is not allowed")`.

**User.Id assignment**: for `allowLocalhost` and exact `allowedIPs` matches, `User.Id = clientIP.String()`. For CIDR matches, `User.Id = cidr.String()` (CIDR notation) unless `ipAsUser = true`, in which case `User.Id = clientIP.String()`.

#### `database` strategy — `DatabaseStrategyConfig`

YAML prefix: `auth.strategies[*].database`

| Field | Go type | Default | Notes | Source |
|---|---|---|---|---|
| `database.connector` | `*ConnectorConfig` | `&ConnectorConfig{}` (auto-created by `SetDefaults`, `common/defaults.go:L2676-L2678`) | Supported drivers: `postgresql`, `dynamodb`, `redis`, `memory`, **`grpc`**. Sub-block is always present after SetDefaults. Scope = `"auth"` for all defaults; see per-driver sub-tables below. `common/config.go:L2483` | `common/defaults.go:L2722` |
| `database.cache` | `*DatabaseStrategyCacheConfig` | **auto-created** (nil → `&DatabaseStrategyCacheConfig{}`, `common/defaults.go:L2680-L2682`) | The entire cache sub-block is always present after SetDefaults; users never need to declare it to get caching — caching is always on with the defaults below. | `common/defaults.go:L2680-L2682` |
| `database.cache.ttl` | `*time.Duration` | `1h` (`common/defaults.go:L2691-L2694`) | Positive-cache TTL for authenticated users in Ristretto. `common/config.go:L2491` | `auth/strategy_database.go:L261-L264` |
| `database.cache.maxSize` | `*int64` | `10000` entries (`common/defaults.go:L2696-L2699`) | Max number of entries in positive cache. `common/config.go:L2492` | `auth/strategy_database.go:L77-L79` |
| `database.cache.maxCost` | `*int64` | `1073741824` (1 GiB, `common/defaults.go:L2701-L2704`) | Ristretto `MaxCost` for positive cache. `common/config.go:L2493` | `auth/strategy_database.go:L77-L79` |
| `database.cache.numCounters` | `*int64` | `100000` (`common/defaults.go:L2706-L2709`) | Ristretto `NumCounters` for positive cache (also reused as `NumCounters` for the negative cache). `common/config.go:L2494` | `auth/strategy_database.go:L77-L97` |
| `database.retry` | `*DatabaseRetryConfig` | **auto-created** (nil → `&DatabaseRetryConfig{}`, `common/defaults.go:L2684-L2686`) | Always present after SetDefaults; retry is always enabled with the defaults below. | `common/defaults.go:L2684-L2686` |
| `database.retry.maxAttempts` | `int` | `3` (`common/defaults.go:L2726-L2728`) | Max DB lookup attempts before giving up. `common/config.go:L2498` | `auth/strategy_database.go:L371-L417` |
| `database.retry.baseBackoff` | `Duration` | `100ms` (`common/defaults.go:L2729-L2731`) | Exponential backoff base. Each attempt uses `baseBackoff << (attempt-1)` (left-shift = doubling). `common/config.go:L2499` | `auth/strategy_database.go:L395-L413` |
| `database.failOpen` | `*DatabaseFailOpenConfig` | **auto-created** (nil → `&DatabaseFailOpenConfig{}`, `common/defaults.go:L2687-L2689`) | Block always exists in memory; `enabled` defaults to `false`. | `common/defaults.go:L2687-L2689`, `L2735-L2742` |
| `database.failOpen.enabled` | `bool` | `false` (zero value; `SetDefaults` only auto-creates the struct, does not set `enabled=true`, `common/defaults.go:L2687-L2689`, `L2735-L2742`) | When `false` (default), any DB error causes auth rejection with `ErrAuthUnauthorized`. When `true`, DB errors cause the strategy to **treat the request as successfully authenticated** using the synthetic `failOpen.userId` user — all downstream logic (rate-limit budgets, logging, etc.) runs as if that user had authenticated via a real API key lookup. The connector-down circuit-breaker fast path (`tryFastFailOpen`) also respects this flag: fail-open only triggers when `enabled=true`. `common/config.go:L2503` | `auth/strategy_database.go:L519-L530`, `L272-L302` |
| `database.failOpen.userId` | `string` | `"emergency-failopen"` (`common/defaults.go:L2738-L2740`) | The `User.Id` value returned for all fail-open authenticated requests. This synthetic user ID flows into Prometheus labels (`erpc_auth_failed_total`), log fields, and rate-limit budget lookups. Can be set to any non-empty string. If `rateLimitBudget` is also set, that budget ID is applied to fail-open traffic — useful for rate-capping emergency traffic separately. `common/config.go:L2504` | `auth/strategy_database.go:L519-L530` |
| `database.failOpen.rateLimitBudget` | `string` | `""` | `User.RateLimitBudget` returned on fail-open. `common/config.go:L2505` | `auth/strategy_database.go:L526-L528` |
| `database.maxWait` | `Duration` | `1s` (`common/defaults.go:L2718-L2720`) | Per-request context timeout for the DB lookup (context with timeout applied around singleflight + `getWithRetries`). `common/config.go:L2487` | `auth/strategy_database.go:L173-L178` |

**Negative cache** (unauthenticated / disabled keys): TTL is hardcoded to `5 * time.Second` (`auth/strategy_database.go:L75`). Shares `NumCounters` with the positive cache config but uses a fixed `MaxCost = 1 << 20` (1 MiB). Not configurable. There is no separate configuration block for the negative cache; only the positive cache sub-block is user-configurable.

**Connector scope defaults for auth**: connector id defaults to `"auth-<driver>"` (e.g., `"auth-memory"`, `"auth-redis"`, `"auth-postgresql"`) — set in `common/defaults.go:L847-L849`. This differs from cache scope which uses `"cache-<driver>"`. All sub-driver defaults below also differ from cache/shared-state scope defaults.

#### `database.connector` — per-driver sub-fields (auth scope)

**`database.connector.id`**

| Field | Go type | Auth-scope default | Notes | Source |
|---|---|---|---|---|
| `connector.id` | `string` | `"auth-<driver>"` (e.g., `"auth-memory"`, `"auth-redis"`) | Set automatically from scope + driver name if empty. `common/defaults.go:L847-L849` | `common/defaults.go:L847-L849` |

**`database.connector.memory.*`**

| Field | Go type | Auth-scope default | Notes | Source |
|---|---|---|---|---|
| `connector.memory.maxItems` | `int` | `100000` | Ristretto item count limit. `common/defaults.go:L936-L938` | `common/defaults.go:L936-L938` |
| `connector.memory.maxTotalSize` | `string` | `"1GB"` | Max total size of values in cache. `common/defaults.go:L939-L941` | `common/defaults.go:L939-L941` |
| `connector.memory.emitMetrics` | `*bool` | `nil` (disabled) | When `true`, a background goroutine emits Ristretto metrics to Prometheus every 30 seconds (`data/memory.go:L213`). Useful per-connector observability toggle. Must be explicitly set to `true` to enable. `common/config.go:L365` | `data/memory.go:L71`, `L213` |

**`database.connector.redis.*`**

| Field | Go type | Auth-scope default | Notes | Source |
|---|---|---|---|---|
| `connector.redis.uri` | `string` | `""` | Full Redis URI (e.g., `redis://user:pass@host:6379/0`). **Mutually exclusive with `addr`** — providing both causes a startup error: `"redis connector: provide either 'uri' or 'addr/username/password/db', not both"` (`common/defaults.go:L947-L950`). When `uri` is set, `addr`/`username`/`password`/`db` are ignored entirely and no folding occurs. `common/config.go:L390` | `common/defaults.go:L946-L950`, `L972-L974` |
| `connector.redis.addr` | `string` | `""` | Redis `host:port`. Used when `uri` is not set. **Folded into `uri` and cleared by `SetDefaults`**: the value is stripped of `redis://` and `rediss://` scheme prefixes first (`common/defaults.go:L979-L980`), then combined with `username`, `password`, `db`, and `tls.enabled` to construct a full `uri` string (`common/defaults.go:L984-L1010`). After folding, `addr` is set to `""` (`common/defaults.go:L1012`). Port defaults to `6379` when missing from `addr` (`common/defaults.go:L988-L989`). `common/config.go:L384` | `common/defaults.go:L978-L1015` |
| `connector.redis.username` | `string` | `""` | Redis username. **Folded into `uri` and cleared**: if `addr` is provided, username and password are combined via `url.UserPassword(r.Username, r.Password)` into the constructed URI, then `username` is set to `""` (`common/defaults.go:L1003-L1013`). The `json:"-"` tag does NOT apply to username; only password is marshal-suppressed. `common/config.go:L385` | `common/defaults.go:L1003-L1013` |
| `connector.redis.password` | `string` | `""` | Redis password. **Folded into `uri` and cleared** (same as `username` above). Tagged `json:"-"` in the struct definition (`common/config.go:L386`) — never marshalled to JSON output, preventing credential leakage in serialized config dumps. `common/config.go:L386` | `common/defaults.go:L1003-L1014` |
| `connector.redis.db` | `int` | `0` | Redis database index. **Folded into `uri` path and cleared**: always appended as `"/<db>"` path component when building the URI from `addr` (`common/defaults.go:L1008`), then `db` is set to `0` (`common/defaults.go:L1015`). Database `0` (the Redis default) is still explicitly included in the constructed URI. `common/config.go:L387` | `common/defaults.go:L1008`, `L1015` |
| `connector.redis.connPoolSize` | `int` | `8` | Connection pool size. `common/defaults.go:L955-L957` | `common/defaults.go:L955-L957` |
| `connector.redis.initTimeout` | `Duration` | `5s` | Connection init timeout. `common/defaults.go:L958-L960` | `common/defaults.go:L958-L960` |
| `connector.redis.getTimeout` | `Duration` | `1s` | Per-get operation timeout. `common/defaults.go:L961-L963` | `common/defaults.go:L961-L963` |
| `connector.redis.setTimeout` | `Duration` | `3s` | Per-set operation timeout. `common/defaults.go:L964-L966` | `common/defaults.go:L964-L966` |
| `connector.redis.lockRetryInterval` | `Duration` | `500ms` | Interval between lock-acquire retries. `common/defaults.go:L967-L969` | `common/defaults.go:L967-L969` |
| `connector.redis.tls` | `*TLSConfig` | `nil` | Optional TLS configuration with 5 sub-fields (same schema as `server.tls.*`): `enabled` (bool), `certFile` (string), `keyFile` (string), `caFile` (string), `insecureSkipVerify` (bool). **Critical behavior**: when `tls.enabled=true` and `addr` is set, `SetDefaults` sets the URI scheme to `rediss://` (`common/defaults.go:L992-L995`). When `uri` is provided directly with `rediss://` scheme, the `redis.ParseURL` call from the go-redis library creates a minimal TLS config; setting `tls.*` fields additionally will **merge** certificate/CA settings onto that baseline — `tls.Certificates` and `tls.RootCAs` are copied into the existing options (`data/redis.go:L162-L176`). When `uri` is `redis://` (plain) and `tls.enabled=true`, the YAML `tls` config is applied wholesale (`data/redis.go:L163-L165`). `common/config.go:L388` | `common/defaults.go:L992-L995`, `data/redis.go:L149-L179` |

**`database.connector.postgresql.*`**

| Field | Go type | Auth-scope default | Notes | Source |
|---|---|---|---|---|
| `connector.postgresql.connectionUri` | `string` | required | PostgreSQL DSN / connection string. Redacted in marshal. `common/config.go:L446` | — |
| `connector.postgresql.table` | `string` | **`"erpc_auth"`** | Table name for API key lookups. **Differs from cache scope** (`"erpc_json_rpc_cache"`) and shared-state scope (`"erpc_shared_state"`). `common/defaults.go:L1028-L1029` | `common/defaults.go:L1028-L1029` |
| `connector.postgresql.minConns` | `int32` | **`1`** | Min pool connections. **Differs from cache scope** (4). `common/defaults.go:L1035-L1036` | `common/defaults.go:L1034-L1036` |
| `connector.postgresql.maxConns` | `int32` | **`4`** | Max pool connections. **Differs from cache scope** (32). `common/defaults.go:L1042-L1043` | `common/defaults.go:L1041-L1043` |
| `connector.postgresql.initTimeout` | `Duration` | `5s` | Pool init timeout. `common/defaults.go:L1048-L1050` | `common/defaults.go:L1048-L1050` |
| `connector.postgresql.getTimeout` | `Duration` | `1s` | Per-query timeout for reads. `common/defaults.go:L1051-L1053` | `common/defaults.go:L1051-L1053` |
| `connector.postgresql.setTimeout` | `Duration` | `2s` | Per-query timeout for writes. `common/defaults.go:L1054-L1056` | `common/defaults.go:L1054-L1056` |

**`database.connector.dynamodb.*`**

| Field | Go type | Auth-scope default | Notes | Source |
|---|---|---|---|---|
| `connector.dynamodb.table` | `string` | **`"erpc_auth"`** | DynamoDB table name. **Differs from cache scope** (`"erpc_json_rpc_cache"`). `common/defaults.go:L1068-L1069` | `common/defaults.go:L1068-L1069` |
| `connector.dynamodb.region` | `string` | `""` | AWS region. Required unless resolved from environment/instance profile. `common/config.go:L430` | — |
| `connector.dynamodb.endpoint` | `string` | `""` | Custom endpoint URL (for local DynamoDB or LocalStack). `common/config.go:L431` | — |
| `connector.dynamodb.auth` | `*AwsAuthConfig` | `nil` | AWS credential config; see `AwsAuthConfig` table below. `common/config.go:L432` | — |
| `connector.dynamodb.partitionKeyName` | `string` | `"groupKey"` | DynamoDB partition key attribute name. `common/defaults.go:L1074-L1076` | `common/defaults.go:L1074-L1076` |
| `connector.dynamodb.rangeKeyName` | `string` | `"requestKey"` | DynamoDB range/sort key attribute name. `common/defaults.go:L1077-L1079` | `common/defaults.go:L1077-L1079` |
| `connector.dynamodb.reverseIndexName` | `string` | `"idx_requestKey_groupKey"` | GSI name for reverse-key lookups. `common/defaults.go:L1080-L1082` | `common/defaults.go:L1080-L1082` |
| `connector.dynamodb.ttlAttributeName` | `string` | `"ttl"` | DynamoDB TTL attribute name. `common/defaults.go:L1083-L1085` | `common/defaults.go:L1083-L1085` |
| `connector.dynamodb.initTimeout` | `Duration` | `5s` | SDK init timeout. `common/defaults.go:L1086-L1088` | `common/defaults.go:L1086-L1088` |
| `connector.dynamodb.getTimeout` | `Duration` | `1s` | Per-get operation timeout. `common/defaults.go:L1089-L1091` | `common/defaults.go:L1089-L1091` |
| `connector.dynamodb.setTimeout` | `Duration` | `2s` | Per-set operation timeout. `common/defaults.go:L1092-L1094` | `common/defaults.go:L1092-L1094` |
| `connector.dynamodb.maxRetries` | `int` | `0` (SDK default retry behavior) | Max DynamoDB SDK retries. Zero means use SDK default (typically 3). `common/config.go:L440` | `common/config.go:L440` |
| `connector.dynamodb.statePollInterval` | `Duration` | `5s` | Interval for polling DynamoDB connection state. `common/defaults.go:L1095-L1097` | `common/defaults.go:L1095-L1097` |
| `connector.dynamodb.lockRetryInterval` | `Duration` | `0` (no sleep; effectively spin-waits) | **FOOTGUN**: Zero means retries busy-spin against DynamoDB API with no sleep between attempts. If auth uses DynamoDB with lock contention, this will generate rapid API calls and may exhaust DynamoDB capacity or incur unexpected cost. Set to a non-zero value (e.g., `100ms`) in production. `common/config.go:L442` | `common/config.go:L442` |

**`AwsAuthConfig`** (used by `connector.dynamodb.auth`):

| Field | Go type | Default | Notes | Source |
|---|---|---|---|---|
| `auth.mode` | `string` | `""` (SDK default credential chain) | Valid values: `"file"` (reads credentials file), `"env"` (reads `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`), `"secret"` (uses `accessKeyID`/`secretAccessKey` fields directly). When nil/empty, the AWS SDK default credential chain applies (env → shared credentials file → EC2 instance profile). `common/config.go:L480` | `common/config.go:L479-L485` |
| `auth.credentialsFile` | `string` | `""` | Path to AWS credentials file (used when `mode="file"`). `common/config.go:L481` | — |
| `auth.profile` | `string` | `""` (never defaulted) | AWS named profile for multi-profile credentials files. **Only meaningful when `mode="file"`**: passed directly as the second argument to `credentials.NewSharedCredentials(cfg.Auth.CredentialsFile, cfg.Auth.Profile)` (`data/dynamodb.go:L181`). When `profile` is `""` and `mode="file"`, the AWS SDK uses the `[default]` section of the credentials file. When `mode="env"` or `mode="secret"`, `profile` is **completely ignored** — the switch statement (`data/dynamodb.go:L179-L187`) only reads `profile` in the `"file"` case; setting it to a non-empty value with other modes is silently discarded. When `mode=""` (empty/SDK default chain), `profile` is also ignored — the SDK applies its own resolution order: env vars → `~/.aws/credentials [default]` → EC2 instance metadata. There is no way to specify a named profile through the SDK default chain path; use `mode="file"` with `credentialsFile` and `profile` for that. `common/config.go:L482` | `data/dynamodb.go:L177-L187` |
| `auth.accessKeyID` | `string` | `""` | AWS access key ID (used when `mode="secret"`). `common/config.go:L483` | — |
| `auth.secretAccessKey` | `string` | `""` | AWS secret access key (used when `mode="secret"`). Redacted in JSON marshal. `common/config.go:L484` | `common/config.go:L487-L495` |

**`database.connector.grpc.*`**

The gRPC driver is supported for auth connectors. `auth/authorizer.go` does not exclude it; `data/grpc.go` implements `Connector` and can be used for any connector scope including auth. The existing statement in this KB that only `postgresql`, `dynamodb`, `redis`, `memory` are supported was incorrect — `grpc` is also valid.

| Field | Go type | Auth-scope default | Notes | Source |
|---|---|---|---|---|
| `connector.grpc.bootstrap` | `string` | `""` (never defaulted) | xDS bootstrap file path or inline config for gRPC server discovery. When non-empty, `fetchGrpcServers` is called with this value to obtain server addresses; the result is **appended** to any addresses already in `servers` (`data/grpc.go:L89-L95`). The format can be a file path or an inline JSON/YAML xDS bootstrap document. **Precedence**: `servers` entries are loaded first; `bootstrap`-discovered servers are appended. **Edge case**: if both `bootstrap` and `servers` are empty, the connector initializes with zero gRPC clients and emits `warn: "no gRPC servers provided or discovered"` (`data/grpc.go:L96-L98`). The connector still starts without error but all `Get` calls will return `ErrConnectorNotReady` until a server is added dynamically. `common/config.go:L355` | `data/grpc.go:L83-L98` |
| `connector.grpc.servers` | `[]string` | `nil` | Explicit list of gRPC server addresses (e.g., `["grpc://host:443"]`). Used as the primary server source; if provided, these are loaded before any `bootstrap`-discovered servers. `common/config.go:L356` | `data/grpc.go:L84-L87` |
| `connector.grpc.headers` | `map[string]string` | `nil` | Static headers attached to all outbound gRPC requests. `common/config.go:L357` | — |
| `connector.grpc.getTimeout` | `Duration` | **`100ms`** | Per-Get timeout. Much tighter than other drivers. `common/defaults.go:L927-L929` | `common/defaults.go:L927-L929` |

**`database.connector.failsafeForGets` / `failsafeForSets`**

Auth-scope connectors support the same failsafe wrapping as cache-scope connectors. Both fields are `[]*FailsafeConfig` on `ConnectorConfig` (`common/config.go:L349-L350`). When configured, each entry wraps the connector's `Get` (for `failsafeForGets`) or `Set` (for `failsafeForSets`) operations with the specified policies.

| Field | Go type | Auth-scope default | Notes | Source |
|---|---|---|---|---|
| `connector.failsafeForGets[*].matchMethod` | `string` | `"*"` (set in `SetDefaults` if empty, `common/defaults.go:L855-L857`) | Wildcard method pattern; `"*"` matches all methods. | `common/defaults.go:L851-L875` |
| `connector.failsafeForGets[*].matchFinality` | `[]DataFinalityState` | `nil` (matches any finality; never defaulted) | When nil, the failsafe policy applies regardless of request finality. When non-nil, only requests whose resolved finality matches one of the listed values are wrapped by this failsafe entry. Valid `DataFinalityState` string values: `"finalized"` (iota 0 — block confirmed past the finalized head), `"unfinalized"` (iota 1 — block after finalized but not yet safe), `"realtime"` (iota 2 — methods like `eth_gasPrice` that always return current state), `"unknown"` (iota 3 — finality cannot be determined, e.g. trace-by-hash calls). `DataFinalityStateAll` (-1) is an internal sentinel for health-tracker aggregations and is NOT a valid user-facing value. `common/data.go:L8-L36`, `common/config.go:L1281` | `common/data.go:L8-L36`, `common/config.go:L1279-L1287` |
| `connector.failsafeForGets[*].retry` | `*RetryPolicyConfig` | `nil` | Retry policy for Get operations. | `common/config.go:L1282` |
| `connector.failsafeForGets[*].circuitBreaker` | `*CircuitBreakerPolicyConfig` | `nil` | Circuit-breaker policy for Get operations. | `common/config.go:L1283` |
| `connector.failsafeForGets[*].timeout` | `*TimeoutPolicyConfig` | `nil` | Timeout policy. | `common/config.go:L1284` |
| `connector.failsafeForGets[*].hedge` | `*HedgePolicyConfig` | `nil` | Hedge policy. | `common/config.go:L1285` |
| `connector.failsafeForGets[*].consensus` | `*ConsensusPolicyConfig` | `nil` | Consensus policy. | `common/config.go:L1286` |

The same sub-fields apply to `connector.failsafeForSets[*]`. These are the same `FailsafeConfig` fields used by cache-scope connectors; the auth connector supports full failsafe wrapping as an important operational capability for handling transient DB failures on authentication hot paths.

`Supports`: `AuthTypeSecret` OR `AuthTypeDatabase` (`auth/strategy_database.go:L118-L120`). This means the database strategy is invoked when credentials arrive via any of the secret-delivery channels (header `X-ERPC-Secret-Token`, `Authorization: Basic`, query `?secret=`/`?token=`).

**DB record format** (JSON stored in connector):
```json
{
  "userId":          "string (required)",
  "enabled":         true,
  "rateLimitBudget": "budget-id (optional)"
}
```
`userId` missing → auth error. `enabled` absent → treated as `true`. `enabled: false` → `ErrAuthUnauthorized("database", "API key is disabled")` with negative-cache entry (`auth/strategy_database.go:L228-L236`).

`Authenticate` fast paths (in evaluation order):
1. Positive Ristretto cache hit → return cached `User` immediately.
2. Negative Ristretto cache hit (known-bad key) → return `ErrAuthUnauthorized`.
3. Connector-down fast path (`tryFastFailOpen`) → return emergency user if fail-open enabled.
4. Singleflight deduplicated DB lookup → `getWithRetries` → parse JSON → cache or negative-cache.

### Behaviors & algorithms

**`auth.strategies[*].type` inference / overwrite algorithm** (`common/defaults.go:L2623-L2673`):

The `type` field undergoes bidirectional inference during `SetDefaults`:

1. If `type == "network"` and `network` block is nil → auto-create `NetworkStrategyConfig{}`. The `network` block presence does NOT overwrite `type` (asymmetric).
2. If `type == "secret"` and `secret` block is nil → auto-create `SecretStrategyConfig{}`.
3. If `secret` block is non-nil → **force** `type = "secret"` (overwrites any explicit `type` value the user set).
4. If `type == "database"` and `database` block is nil → auto-create `DatabaseStrategyConfig{}`.
5. If `database` block is non-nil → **force** `type = "database"`.
6. If `type == "jwt"` and `jwt` block is nil → auto-create `JwtStrategyConfig{}`.
7. If `jwt` block is non-nil → **force** `type = "jwt"`.
8. If `type == "siwe"` and `siwe` block is nil → auto-create `SiweStrategyConfig{}`.
9. If `siwe` block is non-nil → **force** `type = "siwe"`.

**Conflict resolution**: if a user sets `type: "network"` but also provides a `secret` block, step 3 will overwrite `type` to `"secret"`. Block presence always wins over the explicit `type` field for `secret`, `database`, `jwt`, `siwe`. For `network`, only `type: "network"` can trigger auto-creation; the block itself cannot overwrite type — adding a `network: {...}` block to a strategy that has `type: "secret"` will cause the network block to be initialized (because `SetDefaults` calls `s.Network.SetDefaults()` if `s.Network != nil`) but `type` remains `"secret"` and the network strategy's `Authenticate` is never reached. **If multiple sub-config blocks are set simultaneously** (e.g., both `secret` and `jwt` are present), whichever is evaluated last in the code sequence wins — the evaluation order is `secret → database → jwt → siwe`. So `jwt` wins over `secret` if both are set. This is a footgun; do not set multiple sub-config blocks in one strategy entry. `common/defaults.go:L2623-L2673`.

**AuthRegistry.Authenticate — full decision tree** (`auth/registry.go:L46-L96`):

```
if len(strategies) == 0:
    return nil, nil  // allow-all (no auth configured)

for each strategy in order:
    if !shouldApplyToMethod(method):
        skip
    if !strategy.Supports(ap):
        skip
    user, err = strategy.Authenticate(ctx, req, ap)
    if err != nil:
        errs = append(errs, err)
        continue
    req.SetUser(user)           // early — for rate-limiter labels
    acquireRateLimitPermit(...)  // may return ErrAuthRateLimitRuleExceeded
    return user, nil            // STOP — first success wins

if len(errs) == 0:
    return nil, ErrAuthUnauthorized("n/a", "no auth strategy matched...")
if len(errs) == 1:
    return nil, errs[0]
return nil, ErrAuthUnauthorized("n/a", errors.Join(errs...))
```

**Key insight**: if NO strategy's `Supports` returns true (e.g., a JWT token arrives but only `network` is configured), the result is "no auth strategy matched" — not an auth failure from any strategy. This can happen when the request carries the wrong credential type.

**acquireRateLimitPermit label construction** (`auth/authorizer.go:L137`):
- `authLabel` = `fmt.Sprintf("%s:%d", string(a.cfg.Type), a.index)` — e.g. `"secret:0"`, `"database:1"`
- `origin` = `"auth"`
- These flow into Prometheus `erpc_rate_limits_total{..., authLabel, origin}`.

**HTTP payload extraction — full priority table** (`auth/http.go:L13-L88`):

| Priority | Signal | PayloadType | Value extracted |
|---|---|---|---|
| 1 | `?token=<v>` (deprecated) | `AuthTypeSecret` | `v` as `SecretPayload.Value` |
| 2 | `?secret=<v>` | `AuthTypeSecret` | `v` |
| 3 | `X-ERPC-Secret-Token: <v>` | `AuthTypeSecret` | `v` |
| 4 | `Authorization: Basic <b64>` | `AuthTypeSecret` | password part of decoded `user:pass` |
| 5 | `Authorization: Bearer <token>` | `AuthTypeJwt` | `token` as `JwtPayload.Token` |
| 6 | `?jwt=<token>` | `AuthTypeJwt` | `token` |
| 7 | `?signature=<s>&?message=<m>` | `AuthTypeSiwe` | both, message base64-decoded if possible |
| 8 | `X-Siwe-Message` + `X-Siwe-Signature` headers | `AuthTypeSiwe` | both, message base64-decoded if possible |
| 9 | *(none)* | `AuthTypeNetwork` | no credential payload; type signals IP-based auth |

**gRPC metadata extraction — full priority table** (`auth/grpc.go:L12-L54`):

| Priority | Signal | PayloadType | Notes |
|---|---|---|---|
| 1 | `x-erpc-secret-token` metadata | `AuthTypeSecret` | first value used |
| 2 | `authorization: Basic <b64>` | `AuthTypeSecret` | password part |
| 3 | `authorization: Bearer <token>` | `AuthTypeJwt` | token |
| 4 | `x-siwe-message` + `x-siwe-signature` | `AuthTypeSiwe` | both required |
| 5 | *(none)* | `AuthTypeNetwork` | fallback |

Note: gRPC has no `?token=` / `?secret=` / `?jwt=` equivalents — query params are not available in gRPC metadata.

**gRPC required metadata** (`erpc/grpc_server.go:L138-L145`):
- `x-erpc-project` — required; if missing: `codes.InvalidArgument "missing metadata"`.
- `x-erpc-chain-id` — required.
- `x-erpc-architecture` — optional, defaults to `"evm"`.

**JWT key resolution** (`auth/strategy_jwt.go:L100-L116`):
1. If token header has `kid`, look up `s.keys[kid]` directly.
2. Else iterate all keys; pick first whose underlying key type is compatible with the token's signing method (RSA public key → RS*, EC public key → ES*, bytes → HS*).
3. No compatible key found → `ErrAuthUnauthorized("jwt", "no suitable verification key found")`.
4. When `verificationKeys` map is nil or empty, `s.keys` is also empty → step 1 finds nothing, step 2 iterates zero entries → `findVerificationKey` returns error → **all JWTs are rejected**. There is no "pass-through" mode for JWT without keys — an empty key map is a deny-all configuration. See gotcha 22.

**Database connector error classification** (`auth/strategy_database.go:L487-L517`):

| Error | Label | Sets connectorDown? |
|---|---|---|
| `data.ErrConnectorNotReady` (wrapped OK) | `db_not_ready` | yes |
| `context.DeadlineExceeded`, substring `"deadline exceeded"`, `"timeout"` | `db_timeout` | yes |
| substrings: `"connection refused"`, `"connection reset"`, `"broken pipe"`, `"no route to host"`, `"EOF"`, `"use of closed network connection"` | `db_connection` | yes |
| `ErrRecordNotFound` | *(treated separately; not error-classified)* | no — marks connector UP |
| everything else (parse errors, Postgres `53300` "too many connections", syntax errors) | `db_query_error` | no |

**getWithRetries** abort conditions (`auth/strategy_database.go:L367-L417`):
- `ErrRecordNotFound` → abort immediately (no retry needed; DB is healthy).
- `data.ErrConnectorNotReady` → abort immediately (connector's own reconnect loop handles recovery; retrying burns auth budget without effect).
- Context done → abort immediately.
- Any other error → retry with `baseBackoff << (attempt-1)` up to `maxAttempts`.

**Error responses and HTTP status codes** (`common/errors.go:L524-L568`):
- `ErrAuthUnauthorized` (code `"ErrAuthUnauthorized"`) → HTTP 401. Carries `{strategy}` in `Details`. Produced when a strategy's credential check fails, or when no strategy matches the payload type. `common/errors.go:L525-L542`.
- `ErrAuthRateLimitRuleExceeded` (code `"ErrAuthRateLimitRuleExceeded"`) → HTTP 429. Produced by `acquireRateLimitPermit` when a matched rate-limit budget rule is exhausted. Carries `{projectId, strategy, budget, rule, userId, clientIp}` in `Details`. The `rule` value is formatted as `"method:<rpc_method>"`. `common/errors.go:L545-L568`, `auth/authorizer.go:L147-L154`.

**`ErrAuthRateLimitRuleExceeded` vs project/network rate-limit errors**: this error is auth-scope only — it fires when the **auth-strategy-level rate-limit budget** is exceeded (budget attached to a strategy via `rateLimitBudget`, overridable per-user). It does NOT fire for upstream/network rate-limit budgets (those produce `ErrProjectRateLimitRuleExceeded` or `ErrNetworkRateLimitRuleExceeded`). The `U:no` retryability classification means upstreams will NOT retry a request blocked by auth rate-limit; the `N:yes` means the network layer propagates it back to the caller immediately as a 429. `common/errors.go:L547`, `auth/authorizer.go:L117-L157`.

### Observability

#### Prometheus metrics

| Metric | Type | Labels | When fired | Source |
|---|---|---|---|---|
| `erpc_auth_failed_total` | counter | `project`, `network`, `strategy`, `reason`, `agent_name` | On every failed authentication event in the **database strategy only** (other strategies do not record to this metric directly). | `telemetry/metrics.go:L659-L663`, `auth/strategy_database.go:L452-L474` |

`erpc_auth_failed_total` label values:
- `strategy` = always `"database"` (only the database strategy calls `recordAuthFailureMetric`).
- `reason` values: `missing_secret`, `empty_secret`, `cached_unknown_api_key`, `db_fail_open_fast_path`, `db_not_ready`, `db_timeout`, `db_connection`, `db_query_error`, `invalid_api_key`, `disabled_key`, `db_record_parse_error`, `db_record_missing_user_id`, `internal_error` — see `auth/strategy_database.go:L452-L474`, `L487-L517`.

**Rate-limit related auth metric**: `erpc_rate_limits_total` fires when `acquireRateLimitPermit` blocks, with `origin="auth"` and `authLabel="<type>:<index>"`. This is the only rate-limit metric emitted for auth-level budgets.

#### Log messages

| Level | Message | Context | Source |
|---|---|---|---|
| `warn` | `"database connector marked DOWN; subsequent requests will fast-path to fail-open until next probe succeeds"` | `connectorId` | `auth/strategy_database.go:L337-L343` |
| `warn` | `"database connector marked UP; resuming normal auth flow"` | `connectorId` | `auth/strategy_database.go:L350-L355` |
| `error` | `"database query failed during authentication"` | `apiKey`, `driver`, `connectorId`, `err` | `auth/strategy_database.go:L194-L200` |
| `error` | `"auth DB error; fail-open enabled, granting emergency user"` | `userId` | `auth/strategy_database.go:L203` |
| `warn` | `"singleflight error; fail-open enabled, granting emergency user"` | `userId`, `err` | `auth/strategy_database.go:L247` |
| `warn` | `"database authentication lookup failed; retrying"` | `driver`, `connectorId`, `attempt`, `backoff`, `err` | `auth/strategy_database.go:L397-L405` |
| `info` | `"initialized API key cache for database authentication strategy"` | `ttl`, `negTtl`, `maxSize`, `maxCost`, `numCounters` | `auth/strategy_database.go:L99-L106` |
| `debug` | `"API key found in cache"` / `"not found in cache"` | `apiKey` | `auth/strategy_database.go:L137-L140` |
| `debug` | `"API key found in negative cache"` | `apiKey` | `auth/strategy_database.go:L146-L149` |
| `debug` | `"cached API key data"` | `apiKey`, `ttl` | `auth/strategy_database.go:L263-L264` |
| `debug` | `"user authenticated successfully"` | `apiKey`, `userId`, `budget` | `auth/strategy_database.go:L267` |
| `debug` | `"invalidated API key cache entry"` | `apiKey` | `auth/strategy_database.go:L427-L429` |

#### Trace spans

No dedicated auth trace spans. Auth runs inline inside the HTTP handler goroutine within the `http_server.go` OTel request span. The rate-limiter `TryAcquirePermit` call (if a budget is configured) creates a `"RateLimiter.TryAcquirePermit"` detail span.

### Edge cases & gotchas

1. **No strategies = allow-all.** If `auth.strategies` is empty or `auth` is nil, `AuthRegistry.Authenticate` returns `nil, nil` which the caller treats as "unauthenticated but allowed" (`auth/registry.go:L51-L54`, `erpc/projects.go:L94-L99`). This is intentional — auth is opt-in.

2. **network strategy is the credential-less fallback.** Any request arriving without a recognizable credential (no token, no JWT, no SIWE params) gets `AuthTypeNetwork` in its payload. If no `network` strategy is configured, the "no auth strategy matched" error fires — NOT "unauthorized" from network. This means a misconfigured setup that has only a `secret` strategy will reject all requests that arrive without a token with the error "no auth strategy matched", not "invalid secret".

3. **secret vs database strategy both consume `AuthTypeSecret` payloads.** If both are configured, the secret strategy runs first (lower index wins in config order). A request with `X-ERPC-Secret-Token` could authenticate against either. To use only database, omit the `secret` strategy.

4. **database strategy `Supports` also accepts `AuthTypeDatabase`.** The enum value `AuthTypeDatabase` exists (`common/config.go:L2437`) but is never set by any current payload extractor (`auth/http.go`, `auth/grpc.go` only set `AuthTypeSecret` for token-style credentials). It is reserved for future use or direct programmatic injection.

5. **JWT `allowedAudiences` only supports scalar `aud` claim.** The implementation casts `claims["aud"]` to `string` (`auth/strategy_jwt.go:L134`). The OIDC/JWT spec allows `aud` to be a string array. If the token has `"aud": ["a","b"]` (array), the cast fails and the check returns an error even if a matching audience is in the array.

6. **SIWE `allowedDomains` is empty = reject all.** An empty `allowedDomains` list (the zero value) causes `isDomainAllowed` to return false for any domain (`auth/strategy_siwe.go:L59-L67`). Operators must explicitly list allowed domains or SIWE auth always fails.

7. **NetworkStrategyConfig.trustedProxies is an intentional no-op.** The field exists in config struct (`common/config.go:L2530`) but `NetworkStrategy` never reads it. **Why**: client IP resolution is architecturally the responsibility of the HTTP server layer (`erpc/http_server.go`), which uses `server.trustedIPForwarders`/`server.trustedIPHeaders` to resolve the real client IP before the request reaches any strategy. By the time `NetworkStrategy.Authenticate` runs, `req.ClientIP()` already contains the correctly resolved IP. Placing proxy trust config at the strategy layer would be a layering violation — it would mean each strategy silently re-resolves the IP independently, potentially disagreeing with other strategies and the rest of the system. Operators who need X-Forwarded-For support should configure `server.trustedIPForwarders` / `server.trustedIPHeaders`, not `network.trustedProxies`.

8. **Rate-limit budget priority: user > strategy.** After a successful auth, `acquireRateLimitPermit` uses `user.RateLimitBudget` (set by the strategy) when non-empty, otherwise falls back to `strategy.RateLimitBudget` from `AuthStrategyConfig` (`auth/authorizer.go:L119-L127`). An empty user budget does NOT fall through to strategy budget — only `user.RateLimitBudget != ""` triggers the override. Strategies that never set `user.RateLimitBudget` (secret without `secret.rateLimitBudget`, network without `network.rateLimitBudget`) always use the strategy-level budget.

9. **healthcheck auth registry ignores rate-limit budgets.** `NewAuthRegistry` for the healthcheck scope is called with `rateLimitersRegistry = nil` (`erpc/http_server.go:L203`). In `acquireRateLimitPermit`, `a.rateLimitersRegistry.GetBudget(...)` will panic on a nil registry. In practice this is safe only because `acquireRateLimitPermit` short-circuits when `effectiveBudget == ""` (the normal case for healthcheck strategies) and budget lookup is never reached. Setting a `rateLimitBudget` on a healthcheck auth strategy would cause a nil pointer dereference (`auth/authorizer.go:L129`).

10. **secret strategy uses non-constant-time string comparison.** `ap.Secret.Value != s.cfg.Value` is a plain Go string inequality — timing-side-channel vulnerable. For high-security use cases (public internet), prefer the database strategy which hashes are not performed, or use JWT (`auth/strategy_secret.go:L24`).

11. **database strategy negative cache hardcoded TTL.** The negative cache TTL is always 5 seconds regardless of config: `negTTL := 5 * time.Second` (`auth/strategy_database.go:L75`). There is no config field to change it. Disabled/invalid API keys stay in negative cache for up to 5 seconds after deletion/re-enabling.

12. **Connector-down probe is per-process.** The `connectorDown` and `connectorDownSince` atomics are per-`DatabaseStrategy` instance (per-process, per-project-strategy). In a multi-replica fleet, each replica runs its own probe independently — there is no cross-replica coordination. The 1 request/second probe rate applies per instance.

13. **singleflight scope is per API key.** The singleflight group (`s.sf`) uses the raw API key string as the deduplication key. Concurrent requests with the same API key during a cache miss are coalesced into a single DB lookup. Requests with different API keys run in parallel.

14. **SIWE address is normalized to lowercase.** `User.Id = strings.ToLower(message.GetAddress().String())` (`auth/strategy_siwe.go:L52`). Rate-limit budget lookups and log labels always show the lowercase form.

15. **auth registry for admin accepts no rate-limit budget.** The admin `AuthRegistry` is created with the project's `rateLimitersRegistry` (same as projects), so rate-limit budgets on admin auth strategies ARE applied if configured. The healthcheck one is nil (see gotcha 9).

16. **`AuthConfig` is shared type across all three scopes** (`projects[*].auth`, `admin.auth`, `healthCheck.auth`). All five strategy types can be used in any scope. There is no validation that blocks, e.g., a `database` strategy on healthcheck auth — the only hazard is the nil `rateLimitersRegistry` (gotcha 9).

17. **When `admin.auth` is nil, the admin endpoint hard-errors.** `ERPC.AdminAuthenticate` returns `fmt.Errorf("admin auth not configured")` (not a typed error) when `adminAuthRegistry == nil` (`erpc/admin.go:L26-L30`). This error produces HTTP 500 (not 401) since it doesn't carry `ErrCodeAuthUnauthorized`. To disable the admin endpoint intentionally, do not configure `admin` at all; to protect it, configure `admin.auth`.

18. **type inference conflict: multiple sub-config blocks in one strategy entry.** Setting both `secret:` and `jwt:` (or any two sub-config blocks) in a single strategy object causes the later-evaluated block to silently overwrite `type`. Evaluation order in `SetDefaults`: `secret → database → jwt → siwe`. So `siwe` always wins if present. There is no validation error — the earlier block's config is silently initialized but the strategy behaves as the last-evaluated type. `common/defaults.go:L2636-L2666`.

19. **database.cache and database.retry and database.failOpen are always auto-created.** After `SetDefaults`, all three sub-blocks exist in memory even if the user never declares them in YAML. This means: (a) caching is always enabled with 1h TTL unless explicitly changed, (b) retry is always enabled with 3 attempts, (c) failOpen block always exists but `enabled=false` until explicitly set. Operators who want to disable caching cannot do so by omitting the block — they would need to set `database.cache.ttl: 0` or some other mechanism (none currently exists; the cache is always on). `common/defaults.go:L2680-L2689`.

20. **User.RateLimitBudget cascade — auth + project integration.** Auth strategies produce a `User{Id, RateLimitBudget}`. The full precedence chain for rate-limit budget applied on an auth-gated request is:
    1. **Per-user budget** from the strategy result (`user.RateLimitBudget`, set by: database strategy from record field; JWT strategy from `rateLimitBudgetClaimName` claim; secret/network/siwe strategies from their `rateLimitBudget` config field).
    2. **Strategy-level budget** (`auth.strategies[*].rateLimitBudget` from `AuthStrategyConfig`), used when the per-user budget is empty string.
    3. **No budget** → `acquireRateLimitPermit` is a no-op; all requests pass.
    This means a user authenticated via the database strategy who has `"rateLimitBudget": "premium"` in their DB record will be subject to the `premium` budget rules, regardless of what `auth.strategies[*].rateLimitBudget` says. This pattern allows per-user rate limiting without separate strategy entries. The budget selection happens in `auth/authorizer.go:L119-L127`.

21. **Negative cache has no separate configuration.** The database strategy maintains two Ristretto caches: a positive cache (configurable via `database.cache.*`) and a negative cache (for invalid/disabled API keys). The negative cache TTL is hardcoded at 5 seconds (`auth/strategy_database.go:L75`), its `MaxCost` is fixed at 1 MiB (`1 << 20`), and its `NumCounters` is shared with the positive cache config value. There is no user-facing config to tune or disable the negative cache. Operators who re-enable a previously-disabled API key will see up to 5 seconds of continued rejection from the negative cache.

22. **JWT verificationKeys nil/empty = ALL JWTs are rejected (not "pass-all").** Despite the intuition that "no key configured = skip check", the actual behavior is the opposite. When `verificationKeys` is nil or `{}`, `s.keys` is empty, `findVerificationKey` exhausts both the `kid`-lookup and the type-compatible scan without finding a key, returns an error, and the entire `Authenticate` call returns `ErrAuthUnauthorized("jwt", "no suitable verification key found")`. **There is no bypass path** — every JWT token will fail. This is correct secure behavior, but operators who expect "allow all JWTs when no keys configured" (e.g., for development) will see all JWTs rejected. To allow any token during dev, configure a dummy HMAC key with a known secret. `auth/strategy_jwt.go:L60-L63`, `L100-L116`.

23. **SIWE `allowedDomains: nil` = deny-all, looks like allow-all.** In YAML/JSON, `allowedDomains: []` and omitting the field entirely both produce `nil` in the Go struct. `isDomainAllowed` iterates the nil slice and returns `false` for any domain. The visual appearance of `siwe: {}` (empty siwe block) in config gives no indication that it is a blanket deny, making accidental SIWE lockout easy. Operators must always supply at least one domain in `allowedDomains`. `auth/strategy_siwe.go:L59-L67`.

24. **`Authorization: Basic` username is silently discarded.** The HTTP payload extractor decodes `Authorization: Basic <base64(username:password)>` and uses only the password field as the secret value (`auth/http.go:L51-L52`). The username portion is ignored entirely. This means `Authorization: Basic base64("alice:mysecret")` and `Authorization: Basic base64("bob:mysecret")` are treated identically — both authenticate as the secret `"mysecret"`. If you expect the username to distinguish users, it will not: use the database strategy with per-key `userId` records instead. `auth/http.go:L45-L53`.

25. **`?token=` is deprecated; do not use in new implementations.** The `?token=` query parameter is a deprecated alias for `?secret=`. It has the same priority-1 position in the extraction chain (`auth/http.go:L18-L22`). New implementations should use `?secret=` or the `X-ERPC-Secret-Token` header. The `?token=` param will continue to work but may be removed in a future version.

26. **SIWE requires both `?signature=` AND `?message=` together.** The HTTP extractor only sets `AuthTypeSiwe` when `args.Get("signature") != ""` **and** `args.Get("message") != ""` are both true simultaneously (`auth/http.go:L66`). If only `?signature=` is present without `?message=` (or vice versa), the extractor falls through to the next check (`?jwt=`, headers, then network fallback). The request will NOT produce a SIWE auth attempt — it will instead receive the network-strategy payload. Similarly, the header form requires both `X-Siwe-Message` and `X-Siwe-Signature` to be present (`auth/http.go:L72-L79`). A missing partner header causes silent fallthrough. `auth/http.go:L66-L79`.

27. **SIWE `?message=` is base64-normalized before parsing.** The `normalizeSiweMessage` function at `auth/http.go:L90-L96` attempts `base64.StdEncoding.DecodeString(msg)`; if that succeeds, the decoded bytes are used as the EIP-4361 message; if it fails (the string is not valid base64), the raw input is used unchanged. This means clients may send either the raw EIP-4361 message text or a base64-encoded form. Both forms work. The same normalization is applied to the `X-Siwe-Message` header and gRPC metadata. `auth/http.go:L70`, `L77`, `L90-L96`.

28. **`healthCheck.auth` is a separate registry with no rate-limit support.** The healthcheck endpoint at `GET /healthcheck` (and variants) has its own independent `AuthConfig` via `healthCheck.auth`. It is documented in the healthcheck KB (KB-30). When using auth on healthcheck, operators must NOT set `rateLimitBudget` on any healthcheck strategy — the healthcheck auth registry is created with `rateLimitersRegistry=nil`, and any strategy that reaches the rate-limit acquisition path will panic (see gotcha 9). The healthcheck auth supports all five strategy types but rate-limit budgets are effectively unsupported.

29. **`type: "network"` auto-creates `network {}` sub-struct; the converse is asymmetric.** Setting `type: "network"` in a strategy entry without a `network:` block causes `SetDefaults` to auto-create `NetworkStrategyConfig{}` (`common/defaults.go:L2624-L2626`). However, adding a `network:` block does NOT overwrite `type` — unlike `secret`/`database`/`jwt`/`siwe` which all force-overwrite `type` when their sub-block is present. This means: if you set `type: "secret"` and also include a `network: {...}` block, the `network` block is initialized (because `SetDefaults` runs `s.Network.SetDefaults()` unconditionally if non-nil) but `type` remains `"secret"` because the network block does not include an overwrite step. The network strategy's `Authenticate` is never called since `type` is wrong. This is different from all other strategies. `common/defaults.go:L2623-L2631`.

30. **Redis addr + username + password + db are all cleared after URI construction.** After `SetDefaults` runs on a `RedisConnectorConfig` that had `addr` set, the fields `addr`, `username`, `password`, and `db` are ALL zeroed (`common/defaults.go:L1012-L1015`). Any code that reads these fields after `SetDefaults` (e.g., config export, debug logging) will see empty values even if the operator configured them. The canonical state after initialization is always `uri` only. This is important when serializing config to YAML for display — the exported config will show only `uri` (with credentials embedded), not the discrete fields. Because `password` has `json:"-"`, a JSON export of the config after folding will show the `uri` with credentials URL-encoded in it.

### Source map

- `auth/authorizer.go` — `Authorizer` struct: strategy selection, method filtering (`shouldApplyToMethod`), rate-limit permit acquisition (`acquireRateLimitPermit`). Central coordinator per strategy instance.
- `auth/registry.go` — `AuthRegistry`: owns the ordered slice of `Authorizer`s, implements `Authenticate` (first-win loop), `FindDatabaseConnector` (admin API key management helper).
- `auth/strategy.go` — `AuthStrategy` interface: two methods `Supports(ap) bool` and `Authenticate(ctx, req, ap) (*User, error)`.
- `auth/payload.go` — `AuthPayload` struct and sub-payload types (`SecretPayload`, `JwtPayload`, `SiwePayload`).
- `auth/http.go` — `NewPayloadFromHttp`: HTTP-specific payload extraction from query params and headers. Priority order + fallback to `AuthTypeNetwork`.
- `auth/grpc.go` — `NewPayloadFromGrpc`: gRPC metadata-specific payload extraction. Mirrors HTTP but metadata only.
- `auth/grpc_test.go` — Tests for gRPC bearer and basic auth parsing.
- `auth/strategy_secret.go` — `SecretStrategy`: exact string token comparison.
- `auth/strategy_jwt.go` — `JwtStrategy`: JWT parsing, key selection (kid + type-compatible), claim validation, `rlm` budget claim extraction.
- `auth/strategy_siwe.go` — `SiweStrategy`: EIP-4361 message parse + EIP-191 signature verify + domain allowlist + temporal validity.
- `auth/strategy_network.go` — `NetworkStrategy`: IP/CIDR allowlist, localhost shortcut, `ipAsUser` CIDR-vs-IP user-id mode.
- `auth/strategy_database.go` — `DatabaseStrategy`: dynamic API key lookup via data connector, Ristretto positive+negative cache, singleflight deduplication, retry/backoff, connector-down circuit-breaker, fail-open, admin cache invalidation (`InvalidateCache`, `ClearCache`).
- `auth/strategy_database_test.go` — Comprehensive tests for connector-down circuit-breaker, probe election, fail-open behavior, `ErrConnectorNotReady` abort, `classifyDbError` label enumeration.
- `common/config.go:L2433-L2534` — All auth config type declarations: `AuthType` constants, `AuthConfig`, `AuthStrategyConfig`, and all five strategy configs with YAML/JSON tags.
- `common/config.go:L330-L495` — Connector type declarations: `ConnectorDriverType` constants, `ConnectorConfig`, `GrpcConnectorConfig`, `MemoryConnectorConfig`, `RedisConnectorConfig`, `DynamoDBConnectorConfig`, `PostgreSQLConnectorConfig`, `AwsAuthConfig`, `FailsafeConfig`.
- `common/defaults.go:L2611-L2761` — `SetDefaults` implementations for `AuthConfig`, `AuthStrategyConfig`, `DatabaseStrategyConfig`, `DatabaseRetryConfig`, `DatabaseFailOpenConfig`, `JwtStrategyConfig`, `SiweStrategyConfig`, `SecretStrategyConfig`, `NetworkStrategyConfig`.
- `common/defaults.go:L846-L1100` — `ConnectorConfig.SetDefaults` (scope-aware dispatch including `connectorScopeAuth`); per-driver `SetDefaults` for Memory, Redis, PostgreSQL, DynamoDB — each with auth-scope-specific table names and connection pool sizing.
- `data/memory.go` — `MemoryConnector` implementation; `emitMetrics` toggle; `metricsCollectionLoop` (30-second interval).
- `common/user.go` — `User{Id, RateLimitBudget}` struct. The universal auth result type.
- `common/request.go:L402-L417`, `L1033-L1282` — `SetUser`, `User`, `SetClientIP`, `ClientIP`, `UserId` on `NormalizedRequest`. Thread-safe via `atomic.Value` for `user` field.
- `common/errors.go:L524-L568` — `ErrAuthUnauthorized` (HTTP 401) and `ErrAuthRateLimitRuleExceeded` (HTTP 429) with typed error codes.
- `erpc/erpc.go:L83-L104` — Admin `AuthRegistry` creation at startup.
- `erpc/projects_registry.go:L187-L191` — Per-project consumer `AuthRegistry` creation.
- `erpc/http_server.go:L201-L207` — Healthcheck `AuthRegistry` creation; `L552-L584` — HTTP auth call in handler goroutine.
- `erpc/grpc_server.go:L133-L157` — gRPC auth payload extraction + `ClientIP` resolution; `L512-L532` — `grpcClientIP` method.
- `erpc/projects.go:L94-L99` — `AuthenticateConsumer` (nil registry = allow-all).
- `erpc/admin.go:L25-L30` — `AdminAuthenticate` (nil registry = error); `L94-L99` — `FindDatabaseConnector` for admin API key management.
- `erpc/healthcheck.go:L85-L104` — Healthcheck auth guard.
- `telemetry/metrics.go:L659-L663` — `MetricAuthFailedTotal` definition (`erpc_auth_failed_total`).
