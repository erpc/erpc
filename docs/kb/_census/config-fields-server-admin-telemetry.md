# Census: Config fields — root Config, server, admin, metrics, tracing, proxyPools

> status: complete
> source-dirs: common/config.go, common/defaults.go, common/validation.go (defaults cross-checked in erpc/init.go, erpc/http_server.go, telemetry/metrics.go, data/memory.go, util/testing.go)

Scope: every yaml-tagged field reachable from the root `Config` struct under the sections `logLevel`/`clusterKey`/root pointers, `server` (incl. TLS + aliasing), `admin` (incl. full auth-strategy tree and auth-scope connector defaults), `metrics`, `tracing`, `proxyPools`, plus the small root-level `healthCheck` struct; columns = yaml path | declared Go type | exact default after `Config.SetDefaults` (with `common/defaults.go` citation; "—" = zero value, never defaulted) | struct-field line in `common/config.go`. Deep interiors of the shared `FailsafeConfig` policy structs (retry/circuitBreaker/timeout/hedge/consensus bodies, AdaptiveDuration) are intentionally NOT expanded here — they are the failsafe census's contract; only their attachment points under `admin.*` connectors are listed.

Defaults pipeline (order matters): `LoadConfig` → env expansion `os.ExpandEnv` on YAML bytes (common/config.go:L102) → strict decode rejecting unknown keys (`decoder.KnownFields(true)`, common/config.go:L104) → `LegacyTranslateFn` migration hook (common/config.go:L111-L121) → `Config.SetDefaults` (common/defaults.go:L49-L176) → `Config.Validate` (common/validation.go:L15-L67).

## 1. Root `Config` (common/config.go:L38-L68)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `logLevel` | `string` | `"INFO"` — defaults.go:L50-L52 | L39 |
| `clusterKey` | `string` | `"erpc-default"` — defaults.go:L53-L55 | L40 |
| `server` | `*ServerConfig` | auto-created `&ServerConfig{}` + `SetDefaults()` when nil — defaults.go:L56-L61; required by Validate (validation.go:L16-L22) | L41 |
| `healthCheck` | `*HealthCheckConfig` | auto-created `&HealthCheckConfig{}` + `SetDefaults()` when nil — defaults.go:L62-L67; required by Validate (validation.go:L23-L29) | L42 |
| `admin` | `*AdminConfig` | `nil` (defaults applied only when section present) — defaults.go:L87-L91 | L43 |
| `database` | `*DatabaseConfig` | `nil` (defaults applied only when section present) — defaults.go:L74-L78 | L44 |
| `projects` | `[]*ProjectConfig` | `nil`; if empty after load, a default `main` project is synthesized (with network/upstream failsafe defaults) AND `server.aliasing` is overwritten with a catch-all rule `{matchDomain:"*", serveProject:"main"}` — defaults.go:L100-L167; Validate requires non-nil (validation.go:L45-L53, always satisfied post-SetDefaults) | L45 |
| `rateLimiters` | `*RateLimiterConfig` | `nil` (defaults applied only when section present) — defaults.go:L169-L173 | L46 |
| `metrics` | `*MetricsConfig` | auto-created `&MetricsConfig{}` + `SetDefaults()` when nil — defaults.go:L80-L85 | L47 |
| `proxyPools` | `[]*ProxyPoolConfig` | `nil`; no SetDefaults exists; per-pool Validate only (validation.go:L59-L65, L230-L247) | L48 |
| `tracing` | `*TracingConfig` | `nil` (defaults applied only when section present) — defaults.go:L68-L72 | L49 |

Excluded non-config field: `Config.UserScript *sobek.Program` is `yaml:"-" json:"-"` (compiled TS program, never serialized) — config.go:L67.

## 2. Root `healthCheck.*` (HealthCheckConfig, common/config.go:L186-L190)

(Included for root completeness; `healthCheck.auth` shares the `AuthConfig` shape enumerated in section 4.)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `healthCheck.mode` | `HealthCheckMode` (string) | `"networks"` — defaults.go:L739-L741; valid: `simple` \| `networks` \| `verbose` (config.go:L192-L198) | L187 |
| `healthCheck.auth` | `*AuthConfig` | `nil` — no auto-create; same shape/defaults as `admin.auth` (section 4); validated via validation.go:L114-L121 | L188 |
| `healthCheck.defaultEval` | `string` | `"any:initializedUpstreams"` — defaults.go:L742-L744; known eval constants config.go:L200-L209 | L189 |

## 3. `server.*` (ServerConfig, common/config.go:L134-L168; SetDefaults defaults.go:L639-L736; Validate validation.go:L69-L112)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `server.listenV4` | `*bool` | `true` — defaults.go:L640-L644; NOT defaulted when running under `go test` unless env `FORCE_TEST_LISTEN_V4=true` (`util.IsTest()` = `flag.Lookup("test.v") != nil`, util/testing.go:L19-L21) | L135 |
| `server.httpHostV4` | `*string` | `"0.0.0.0"` — defaults.go:L645-L647 | L136 |
| `server.listenV6` | `*bool` | `nil` — never defaulted (no assignment in defaults.go:L639-L736); nil ⇒ IPv6 listener disabled (erpc/http_server.go:L191) | L137 |
| `server.httpHostV6` | `*string` | `"[::]"` — defaults.go:L648-L650 | L138 |
| `server.httpPort` | `*int` | Deprecated (use `httpPortV4`, comment config.go:L139). If set and `httpPortV4` unset, migrated into `httpPortV4` (defaults.go:L652-L654); afterwards always mirrors `httpPortV4` ⇒ effective `4000` (defaults.go:L659-L661) | L139 |
| `server.httpPortV4` | `*int` | `4000` — defaults.go:L655-L657 | L140 |
| `server.httpPortV6` | `*int` | derived `*httpPort + 1000` ⇒ `5000` by default — defaults.go:L663-L665 (fallback literal `5000` at L666-L668 is unreachable after the derivation) | L141 |
| `server.grpcEnabled` | `*bool` | `false` — defaults.go:L669-L671 | L142 |
| `server.grpcHostV4` | `*string` | inherits `*httpHostV4` ⇒ `"0.0.0.0"` — defaults.go:L672-L675 | L143 |
| `server.grpcPortV4` | `*int` | inherits `*httpPortV4` ⇒ `4000` — defaults.go:L676-L679 | L144 |
| `server.grpcHostV6` | `*string` | inherits `*httpHostV6` ⇒ `"[::]"` — defaults.go:L680-L683 | L145 |
| `server.grpcPortV6` | `*int` | inherits `*httpPortV6` ⇒ `5000` — defaults.go:L684-L687 | L146 |
| `server.grpcMaxRecvMsgSize` | `*int` | `104857600` (100*1024*1024 bytes = 100 MiB) — defaults.go:L688-L690 | L147 |
| `server.grpcMaxSendMsgSize` | `*int` | `104857600` (100 MiB) — defaults.go:L691-L693 | L148 |
| `server.maxTimeout` | `*Duration` | `150s` — defaults.go:L694-L697; required non-zero by Validate (validation.go:L90-L92) | L149 |
| `server.readTimeout` | `*Duration` | `30s` — defaults.go:L698-L701 | L150 |
| `server.writeTimeout` | `*Duration` | `120s` — defaults.go:L702-L705 | L151 |
| `server.enableGzip` | `*bool` | `true` — defaults.go:L706-L708 | L152 |
| `server.tls` | `*TLSConfig` | `nil` — no TLSConfig SetDefaults exists anywhere in common/defaults.go | L153 |
| `server.aliasing` | `*AliasingConfig` | `nil`; overwritten with catch-all rule `{matchDomain:"*", serveProject:"main"}` when config has zero projects — defaults.go:L100-L110 | L154 |
| `server.waitBeforeShutdown` | `*Duration` | `10s` — defaults.go:L709-L712 | L155 |
| `server.waitAfterShutdown` | `*Duration` | `10s` — defaults.go:L713-L716 | L156 |
| `server.includeErrorDetails` | `*bool` | `true` — defaults.go:L717-L719 | L157 |
| `server.trustedIPForwarders` | `[]string` | `["127.0.0.1/8", "::1/128"]` (applied when len==0) — defaults.go:L726-L729; entries validated as IP or CIDR (validation.go:L95-L109) | L158 |
| `server.trustedIPHeaders` | `[]string` | `[]` (empty slice when nil) — defaults.go:L730-L733 | L159 |
| `server.responseHeaders` | `map[string]string` | `nil` — never defaulted | L160 |
| `server.executionHeaders` | `*ExecutionHeadersMode` (string) | `"all"` — defaults.go:L720-L723; valid: `all` \| `summary` \| `off` (config.go:L174-L184) | L167 |

### 3a. `server.tls.*` (TLSConfig, common/config.go:L375-L381 — shared shape, no SetDefaults)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `server.tls.enabled` | `bool` | `false` — zero value, never defaulted | L376 |
| `server.tls.certFile` | `string` | `""` — never defaulted | L377 |
| `server.tls.keyFile` | `string` | `""` — never defaulted | L378 |
| `server.tls.caFile` | `string` | `""` — never defaulted | L379 |
| `server.tls.insecureSkipVerify` | `bool` | `false` — never defaulted | L380 |

### 3b. `server.aliasing.*` (AliasingConfig config.go:L253-L255, AliasingRuleConfig config.go:L257-L262 — no SetDefaults/Validate)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `server.aliasing.rules` | `[]*AliasingRuleConfig` | `nil` (except the synthesized catch-all rule when zero projects, defaults.go:L102-L110) | L254 |
| `server.aliasing.rules[].matchDomain` | `string` | `""` — never defaulted | L258 |
| `server.aliasing.rules[].serveProject` | `string` | `""` — never defaulted | L259 |
| `server.aliasing.rules[].serveArchitecture` | `string` | `""` — never defaulted | L260 |
| `server.aliasing.rules[].serveChain` | `string` | `""` — never defaulted | L261 |

## 4. `admin.*` (AdminConfig, common/config.go:L248-L251; SetDefaults defaults.go:L769-L787; Validate validation.go:L123-L135)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.auth` | `*AuthConfig` | `nil` (defaults only descend when present — defaults.go:L770-L774) | L249 |
| `admin.cors` | `*CORSConfig` | auto-created `{allowedOrigins:["*"], allowCredentials:false}` when nil — defaults.go:L775-L781; then CORS SetDefaults always runs (defaults.go:L782-L784) | L250 |

### 4a. `admin.cors.*` (CORSConfig, common/config.go:L658-L665; SetDefaults defaults.go:L2824-L2846; Validate validation.go:L773-L778)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.cors.allowedOrigins` | `[]string` | `["*"]` — defaults.go:L2825-L2827; required ≥1 by Validate (validation.go:L774-L776) | L659 |
| `admin.cors.allowedMethods` | `[]string` | `["GET", "POST", "OPTIONS"]` — defaults.go:L2828-L2830 | L660 |
| `admin.cors.allowedHeaders` | `[]string` | `["content-type", "authorization", "x-erpc-secret-token"]` — defaults.go:L2831-L2837 | L661 |
| `admin.cors.exposedHeaders` | `[]string` | `nil` — never defaulted (not touched by SetDefaults) | L662 |
| `admin.cors.allowCredentials` | `*bool` | `false` — defaults.go:L2838-L2840 | L663 |
| `admin.cors.maxAge` | `int` | `3600` — defaults.go:L2841-L2843 | L664 |

### 4b. `admin.auth.*` (AuthConfig config.go:L2443-L2445; AuthStrategyConfig config.go:L2447-L2458; SetDefaults defaults.go:L2611-L2673; Validate validation.go:L648-L725)

Same shape is reused at `healthCheck.auth` and `projects[].auth`; defaults identical (`AuthConfig.SetDefaults` has no scope parameter).

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.auth.strategies` | `[]*AuthStrategyConfig` | `nil`; required ≥1 by Validate when auth present (validation.go:L649-L651) | L2444 |
| `admin.auth.strategies[].ignoreMethods` | `[]string` | `nil` — never defaulted | L2448 |
| `admin.auth.strategies[].allowMethods` | `[]string` | `nil` — never defaulted | L2449 |
| `admin.auth.strategies[].rateLimitBudget` | `string` | `""` — never defaulted | L2450 |
| `admin.auth.strategies[].type` | `AuthType` (string) | no static default; force-set from whichever sub-struct is populated (`secret`→`secret`, `database`→`database`, `jwt`→`jwt`, `siwe`→`siwe` — defaults.go:L2636-L2667); required + must be one of `network`\|`secret`\|`jwt`\|`siwe`\|`database` (config.go:L2435-L2441; validation.go:L675-L723) | L2452 |
| `admin.auth.strategies[].network` | `*NetworkStrategyConfig` | `nil`; auto-created `{}` when `type: network` — defaults.go:L2624-L2626 | L2453 |
| `admin.auth.strategies[].secret` | `*SecretStrategyConfig` | `nil`; auto-created `{}` when `type: secret` — defaults.go:L2633-L2635 | L2454 |
| `admin.auth.strategies[].database` | `*DatabaseStrategyConfig` | `nil`; auto-created `{}` when `type: database` — defaults.go:L2642-L2644 | L2455 |
| `admin.auth.strategies[].jwt` | `*JwtStrategyConfig` | `nil`; auto-created `{}` when `type: jwt` — defaults.go:L2652-L2654 | L2456 |
| `admin.auth.strategies[].siwe` | `*SiweStrategyConfig` | `nil`; auto-created `{}` when `type: siwe` — defaults.go:L2662-L2664 | L2457 |

#### 4b-1. `…strategies[].network.*` (NetworkStrategyConfig, config.go:L2526-L2534; SetDefaults is a no-op, defaults.go:L2759-L2761)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.auth.strategies[].network.allowedIPs` | `[]string` | `nil` — never defaulted | L2527 |
| `admin.auth.strategies[].network.allowedCIDRs` | `[]string` | `nil` — never defaulted | L2528 |
| `admin.auth.strategies[].network.allowLocalhost` | `bool` | `false` — never defaulted | L2529 |
| `admin.auth.strategies[].network.trustedProxies` | `[]string` | `nil` — never defaulted | L2530 |
| `admin.auth.strategies[].network.rateLimitBudget` | `string` | `""` — never defaulted | L2532 |
| `admin.auth.strategies[].network.ipAsUser` | `bool` | `false` — never defaulted | L2533 |

#### 4b-2. `…strategies[].secret.*` (SecretStrategyConfig, config.go:L2460-L2465; SetDefaults no-op defaults.go:L2744-L2746)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.auth.strategies[].secret.id` | `string` | `""` — never defaulted | L2461 |
| `admin.auth.strategies[].secret.value` | `string` | `""`; required by Validate (validation.go:L754-L759); redacted in JSON/YAML marshal (config.go:L2468-L2480) | L2462 |
| `admin.auth.strategies[].secret.rateLimitBudget` | `string` | `""` — never defaulted | L2464 |

#### 4b-3. `…strategies[].jwt.*` (JwtStrategyConfig, config.go:L2508-L2518; SetDefaults defaults.go:L2748-L2753)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.auth.strategies[].jwt.allowedIssuers` | `[]string` | `nil` — never defaulted | L2509 |
| `admin.auth.strategies[].jwt.allowedAudiences` | `[]string` | `nil` — never defaulted | L2510 |
| `admin.auth.strategies[].jwt.allowedAlgorithms` | `[]string` | `nil` — never defaulted | L2511 |
| `admin.auth.strategies[].jwt.requiredClaims` | `[]string` | `nil` — never defaulted | L2512 |
| `admin.auth.strategies[].jwt.verificationKeys` | `map[string]string` | `nil`; required ≥1 entry by Validate (validation.go:L761-L767) | L2513 |
| `admin.auth.strategies[].jwt.rateLimitBudgetClaimName` | `string` | `"rlm"` — defaults.go:L2749-L2751 | L2517 |

#### 4b-4. `…strategies[].siwe.*` (SiweStrategyConfig, config.go:L2520-L2524; SetDefaults no-op defaults.go:L2755-L2757)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.auth.strategies[].siwe.allowedDomains` | `[]string` | `nil` — never defaulted | L2521 |
| `admin.auth.strategies[].siwe.rateLimitBudget` | `string` | `""` — never defaulted | L2523 |

#### 4b-5. `…strategies[].database.*` (DatabaseStrategyConfig, config.go:L2482-L2488; SetDefaults defaults.go:L2675-L2723; Validate validation.go:L727-L748)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.auth.strategies[].database.connector` | `*ConnectorConfig` | auto-created `&ConnectorConfig{}` when nil (defaults.go:L2676-L2678) then `SetDefaults(connectorScopeAuth)` (defaults.go:L2722); required by Validate (validation.go:L728-L730); connector `id` must be unique across database strategies (validation.go:L653-L670) | L2483 |
| `admin.auth.strategies[].database.cache` | `*DatabaseStrategyCacheConfig` | auto-created `{}` when nil — defaults.go:L2680-L2682 | L2484 |
| `admin.auth.strategies[].database.cache.ttl` | `*time.Duration` | `1h` (`time.Hour`) — defaults.go:L2691-L2694; must be ≥0 (validation.go:L733-L735) | L2491 |
| `admin.auth.strategies[].database.cache.maxSize` | `*int64` | `10000` — defaults.go:L2696-L2699; must be >0 (validation.go:L736-L738) | L2492 |
| `admin.auth.strategies[].database.cache.maxCost` | `*int64` | `1073741824` (1<<30 = 1 GiB) — defaults.go:L2701-L2704; must be >0 (validation.go:L739-L741) | L2493 |
| `admin.auth.strategies[].database.cache.numCounters` | `*int64` | `100000` — defaults.go:L2706-L2709; must be >0 (validation.go:L742-L744) | L2494 |
| `admin.auth.strategies[].database.retry` | `*DatabaseRetryConfig` | auto-created `{}` when nil — defaults.go:L2684-L2686 | L2485 |
| `admin.auth.strategies[].database.retry.maxAttempts` | `int` | `3` (applied when ≤0) — defaults.go:L2725-L2728 | L2498 |
| `admin.auth.strategies[].database.retry.baseBackoff` | `Duration` | `100ms` — defaults.go:L2729-L2731 | L2499 |
| `admin.auth.strategies[].database.failOpen` | `*DatabaseFailOpenConfig` | auto-created `{}` when nil — defaults.go:L2687-L2689 | L2486 |
| `admin.auth.strategies[].database.failOpen.enabled` | `bool` | `false` — deliberately left as zero value (comment defaults.go:L2735-L2737) | L2503 |
| `admin.auth.strategies[].database.failOpen.userId` | `string` | `"emergency-failopen"` — defaults.go:L2738-L2740 | L2504 |
| `admin.auth.strategies[].database.failOpen.rateLimitBudget` | `string` | `""` — never defaulted | L2505 |
| `admin.auth.strategies[].database.maxWait` | `Duration` | `1s` (applied when 0) — defaults.go:L2718-L2720 | L2487 |

#### 4b-6. `…strategies[].database.connector.*` (ConnectorConfig, config.go:L341-L352; SetDefaults with scope `"auth"` defaults.go:L846-L933)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.auth.strategies[].database.connector.id` | `string` | `"auth-" + driver` when empty (e.g. `auth-memory`) — defaults.go:L846-L849 | L342 |
| `admin.auth.strategies[].database.connector.driver` | `ConnectorDriverType` (string) | no static default; force-set from whichever driver block is populated (memory/redis/postgresql/dynamodb/grpc — defaults.go:L876-L925); valid values config.go:L333-L339 | L343 |
| `admin.auth.strategies[].database.connector.memory` | `*MemoryConnectorConfig` | `nil`; auto-created when `driver: memory` — defaults.go:L879-L885 | L344 |
| `admin.auth.strategies[].database.connector.redis` | `*RedisConnectorConfig` | `nil`; auto-created when `driver: redis` — defaults.go:L890-L896 | L345 |
| `admin.auth.strategies[].database.connector.dynamodb` | `*DynamoDBConnectorConfig` | `nil`; auto-created when `driver: dynamodb` — defaults.go:L912-L918 | L346 |
| `admin.auth.strategies[].database.connector.postgresql` | `*PostgreSQLConnectorConfig` | `nil`; auto-created when `driver: postgresql` — defaults.go:L901-L907 | L347 |
| `admin.auth.strategies[].database.connector.grpc` | `*GrpcConnectorConfig` | `nil`; auto-created when `driver: grpc` — defaults.go:L923-L926 | L348 |
| `admin.auth.strategies[].database.connector.failsafeForGets` | `[]*FailsafeConfig` | `nil`; per entry `matchMethod` defaults to `"*"` then `SetDefaults(nil)` — defaults.go:L850-L862 | L349 |
| `admin.auth.strategies[].database.connector.failsafeForSets` | `[]*FailsafeConfig` | `nil`; same handling — defaults.go:L863-L875 | L350 |

Excluded non-config field: `ConnectorConfig.Mock` is `yaml:"-" json:"-"` (config.go:L351).

`FailsafeConfig` attachment fields (shared type, config.go:L1279-L1287) reachable under both `failsafeForGets[]`/`failsafeForSets[]`:

| yaml path (suffix under either list) | Go type | exact default | config.go |
|---|---|---|---|
| `….matchMethod` | `string` | `"*"` (set at connector scope defaults.go:L855-L857/L868-L870, and again by `FailsafeConfig.SetDefaults` defaults.go:L2132-L2138) | L1280 |
| `….matchFinality` | `[]DataFinalityState` | `nil` — never defaulted | L1281 |
| `….retry` | `*RetryPolicyConfig` | `nil` (interior defaulted only when present — defaults.go:L2156-L2171) | L1282 |
| `….circuitBreaker` | `*CircuitBreakerPolicyConfig` | `nil` (interior defaulted only when present — defaults.go:L2188-L2203) | L1283 |
| `….timeout` | `*TimeoutPolicyConfig` | `nil` (interior defaulted only when present — defaults.go:L2140-L2155) | L1284 |
| `….hedge` | `*HedgePolicyConfig` | `nil` (interior defaulted only when present — defaults.go:L2172-L2187) | L1285 |
| `….consensus` | `*ConsensusPolicyConfig` | `nil` (interior defaulted only when present — defaults.go:L2204-L2208) | L1286 |

> Cross-reference: the INTERIOR fields of `RetryPolicyConfig` (config.go:L1342-L1370), `CircuitBreakerPolicyConfig` (L1381-L1387), `TimeoutPolicyConfig` (L1406-L1408), `HedgePolicyConfig` (L1489-L1492), `ConsensusPolicyConfig` (L1591-L1649) and `AdaptiveDuration` are enumerated in the failsafe census file, not duplicated here.

#### 4b-7. `…database.connector.memory.*` (MemoryConnectorConfig, config.go:L361-L365; SetDefaults defaults.go:L935-L944)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.auth.strategies[].database.connector.memory.maxItems` | `int` | `100000` (applied when 0) — defaults.go:L936-L938 | L362 |
| `admin.auth.strategies[].database.connector.memory.maxTotalSize` | `string` | `"1GB"` (applied when "") — defaults.go:L939-L941 | L363 |
| `admin.auth.strategies[].database.connector.memory.emitMetrics` | `*bool` | `nil` — never defaulted; nil ⇒ metrics disabled (data/memory.go:L71) | L364 |

#### 4b-8. `…database.connector.redis.*` (RedisConnectorConfig, config.go:L383-L395; SetDefaults defaults.go:L946-L1019 — scope-independent)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.auth.strategies[].database.connector.redis.addr` | `string` | `""`; if set, folded into `uri` (host:port, port defaults to `6379` when missing) and then cleared — defaults.go:L979-L1016; error if both `addr` and `uri` set — defaults.go:L947-L951 | L384 |
| `admin.auth.strategies[].database.connector.redis.username` | `string` | `""`; folded into `uri` then cleared — defaults.go:L1003-L1013 | L385 |
| `admin.auth.strategies[].database.connector.redis.password` | `string` | `""`; folded into `uri` then cleared; `json:"-"` (never marshalled) | L386 |
| `admin.auth.strategies[].database.connector.redis.db` | `int` | `0`; folded into `uri` path then cleared — defaults.go:L1007-L1015 | L387 |
| `admin.auth.strategies[].database.connector.redis.tls` | `*TLSConfig` | `nil`; same 5 fields as `server.tls.*` (config.go:L375-L381); `tls.enabled` switches constructed URI scheme to `rediss://` — defaults.go:L992-L995 | L388 |
| `admin.auth.strategies[].database.connector.redis.connPoolSize` | `int` | `8` (applied when 0) — defaults.go:L955-L957 | L389 |
| `admin.auth.strategies[].database.connector.redis.uri` | `string` | `""`; constructed from discrete fields when `addr` provided — defaults.go:L971-L1016 | L390 |
| `admin.auth.strategies[].database.connector.redis.initTimeout` | `Duration` | `5s` — defaults.go:L958-L960 | L391 |
| `admin.auth.strategies[].database.connector.redis.getTimeout` | `Duration` | `1s` — defaults.go:L961-L963 | L392 |
| `admin.auth.strategies[].database.connector.redis.setTimeout` | `Duration` | `3s` — defaults.go:L964-L966 | L393 |
| `admin.auth.strategies[].database.connector.redis.lockRetryInterval` | `Duration` | `500ms` — defaults.go:L967-L969 | L394 |

#### 4b-9. `…database.connector.dynamodb.*` (DynamoDBConnectorConfig, config.go:L428-L443; SetDefaults defaults.go:L1061-L1100 — scope-aware, scope=`auth` here)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.auth.strategies[].database.connector.dynamodb.table` | `string` | `"erpc_auth"` (auth scope; `erpc_shared_state` / `erpc_json_rpc_cache` for other scopes) — defaults.go:L1062-L1073 | L429 |
| `admin.auth.strategies[].database.connector.dynamodb.region` | `string` | `""` — never defaulted | L430 |
| `admin.auth.strategies[].database.connector.dynamodb.endpoint` | `string` | `""` — never defaulted | L431 |
| `admin.auth.strategies[].database.connector.dynamodb.auth` | `*AwsAuthConfig` | `nil` — never defaulted | L432 |
| `admin.auth.strategies[].database.connector.dynamodb.partitionKeyName` | `string` | `"groupKey"` — defaults.go:L1074-L1076 | L433 |
| `admin.auth.strategies[].database.connector.dynamodb.rangeKeyName` | `string` | `"requestKey"` — defaults.go:L1077-L1079 | L434 |
| `admin.auth.strategies[].database.connector.dynamodb.reverseIndexName` | `string` | `"idx_requestKey_groupKey"` — defaults.go:L1080-L1082 | L435 |
| `admin.auth.strategies[].database.connector.dynamodb.ttlAttributeName` | `string` | `"ttl"` — defaults.go:L1083-L1085 | L436 |
| `admin.auth.strategies[].database.connector.dynamodb.initTimeout` | `Duration` | `5s` — defaults.go:L1086-L1088 | L437 |
| `admin.auth.strategies[].database.connector.dynamodb.getTimeout` | `Duration` | `1s` — defaults.go:L1089-L1091 | L438 |
| `admin.auth.strategies[].database.connector.dynamodb.setTimeout` | `Duration` | `2s` — defaults.go:L1092-L1094 | L439 |
| `admin.auth.strategies[].database.connector.dynamodb.maxRetries` | `int` | `0` — never defaulted | L440 |
| `admin.auth.strategies[].database.connector.dynamodb.statePollInterval` | `Duration` | `5s` — defaults.go:L1095-L1097 | L441 |
| `admin.auth.strategies[].database.connector.dynamodb.lockRetryInterval` | `Duration` | `0` — never defaulted (not in DynamoDB SetDefaults) | L442 |

`AwsAuthConfig` (config.go:L479-L485 — no SetDefaults):

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `…dynamodb.auth.mode` | `string` | `""` — never defaulted; tstype hints `'file' \| 'env' \| 'secret'` (config.go:L480) | L480 |
| `…dynamodb.auth.credentialsFile` | `string` | `""` — never defaulted | L481 |
| `…dynamodb.auth.profile` | `string` | `""` — never defaulted | L482 |
| `…dynamodb.auth.accessKeyID` | `string` | `""` — never defaulted | L483 |
| `…dynamodb.auth.secretAccessKey` | `string` | `""` — never defaulted | L484 |

#### 4b-10. `…database.connector.postgresql.*` (PostgreSQLConnectorConfig, config.go:L445-L453; SetDefaults defaults.go:L1021-L1059 — scope-aware, scope=`auth` here)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.auth.strategies[].database.connector.postgresql.connectionUri` | `string` | `""` — never defaulted | L446 |
| `admin.auth.strategies[].database.connector.postgresql.table` | `string` | `"erpc_auth"` (auth scope; `erpc_shared_state` / `erpc_json_rpc_cache` for other scopes) — defaults.go:L1022-L1033 | L447 |
| `admin.auth.strategies[].database.connector.postgresql.minConns` | `int32` | `1` (auth scope; `4` for other scopes) — defaults.go:L1034-L1040 | L448 |
| `admin.auth.strategies[].database.connector.postgresql.maxConns` | `int32` | `4` (auth scope; `32` for other scopes) — defaults.go:L1041-L1047 | L449 |
| `admin.auth.strategies[].database.connector.postgresql.initTimeout` | `Duration` | `5s` — defaults.go:L1048-L1050 | L450 |
| `admin.auth.strategies[].database.connector.postgresql.getTimeout` | `Duration` | `1s` — defaults.go:L1051-L1053 | L451 |
| `admin.auth.strategies[].database.connector.postgresql.setTimeout` | `Duration` | `2s` — defaults.go:L1054-L1056 | L452 |

#### 4b-11. `…database.connector.grpc.*` (GrpcConnectorConfig, config.go:L354-L359; defaults inlined in ConnectorConfig.SetDefaults)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `admin.auth.strategies[].database.connector.grpc.bootstrap` | `string` | `""` — never defaulted | L355 |
| `admin.auth.strategies[].database.connector.grpc.servers` | `[]string` | `nil` — never defaulted | L356 |
| `admin.auth.strategies[].database.connector.grpc.headers` | `map[string]string` | `nil` — never defaulted | L357 |
| `admin.auth.strategies[].database.connector.grpc.getTimeout` | `Duration` | `100ms` (applied when 0) — defaults.go:L927-L929 | L358 |

## 5. `metrics.*` (MetricsConfig, common/config.go:L2543-L2564; SetDefaults defaults.go:L749-L767; Validate validation.go:L137-L161)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `metrics.enabled` | `*bool` | `true` — defaults.go:L750-L752; NOT defaulted under `go test` (`util.IsTest()`, util/testing.go:L19-L21) so stays `nil` in tests | L2544 |
| `metrics.listenV4` | `*bool` | `nil` — never defaulted; not consumed by the metrics listener, which binds `:port` on all interfaces (erpc/init.go:L145-L152) | L2545 |
| `metrics.hostV4` | `*string` | `"0.0.0.0"` — defaults.go:L753-L755; only used by Validate (hostV4-or-hostV6 required when enabled, validation.go:L139-L141), not by the listener | L2546 |
| `metrics.listenV6` | `*bool` | `nil` — never defaulted; unused by listener | L2547 |
| `metrics.hostV6` | `*string` | `"[::]"` — defaults.go:L756-L758 | L2548 |
| `metrics.port` | `*int` | `4001` — defaults.go:L759-L761; required when enabled (validation.go:L142-L144; erpc/init.go:L141-L143) | L2549 |
| `metrics.errorLabelMode` | `LabelMode` (string) | `"compact"` — defaults.go:L762-L764; valid: `verbose` \| `compact` (config.go:L2536-L2541; validation.go:L147-L149) | L2550 |
| `metrics.histogramBuckets` | `string` | `""` (comma-separated floats; validated validation.go:L151-L158); empty ⇒ `telemetry.DefaultHistogramBuckets` fallback (telemetry/metrics.go:L731-L742, applied via SetHistogramBuckets fallback telemetry/metrics.go:L780-L783; wired in erpc/init.go:L48-L52) | L2551 |
| `metrics.histogramDropLabels` | `[]string` | `nil` — never defaulted; consumed by `telemetry.SetHistogramLabelFilter` (erpc/init.go:L53) | L2557 |
| `metrics.histogramLabelOverrides` | `map[string][]string` | `nil` — never defaulted; keys are metric names without the `erpc_` prefix (comment config.go:L2559-L2562) | L2563 |

## 6. `tracing.*` (TracingConfig, common/config.go:L218-L235; SetDefaults defaults.go:L618-L637 — runs ONLY if the `tracing:` section is present, defaults.go:L68-L72)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `tracing.enabled` | `bool` | `false` — zero value, never defaulted | L219 |
| `tracing.endpoint` | `string` | derived: `"localhost:4317"` when protocol is `grpc`, else `"http://localhost:4318"` — defaults.go:L622-L628 | L220 |
| `tracing.protocol` | `TracingProtocol` (string) | `"grpc"` — defaults.go:L619-L621; valid: `http` \| `grpc` (config.go:L211-L216) | L221 |
| `tracing.sampleRate` | `float64` | `1.0` (applied when 0) — defaults.go:L629-L631 | L222 |
| `tracing.detailed` | `bool` | `false` — never defaulted | L223 |
| `tracing.serviceName` | `string` | `"erpc"` — defaults.go:L632-L634 | L224 |
| `tracing.headers` | `map[string]string` | `nil` — never defaulted | L225 |
| `tracing.tls` | `*TLSConfig` | `nil`; same 5 fields as `server.tls.*` (shared TLSConfig, config.go:L375-L381, no SetDefaults) | L226 |
| `tracing.resourceAttributes` | `map[string]string` | `nil` — never defaulted | L227 |
| `tracing.forceTraceMatchers` | `[]*ForceTraceMatcher` | `nil` — never defaulted; semantics: `\|`-separated OR within a field, AND between fields (comment config.go:L229-L234) | L234 |
| `tracing.forceTraceMatchers[].network` | `string` | `""` — never defaulted | L241 |
| `tracing.forceTraceMatchers[].method` | `string` | `""` — never defaulted | L245 |

## 7. `proxyPools[].*` (ProxyPoolConfig, common/config.go:L1985-L1988; no SetDefaults; Validate validation.go:L230-L247)

| yaml path | Go type | exact default | config.go |
|---|---|---|---|
| `proxyPools[].id` | `string` | `""` — no default; required by Validate (validation.go:L231-L233) | L1986 |
| `proxyPools[].urls` | `[]string` | `nil` — no default; required ≥1; each URL must start with `http://`, `https://`, or `socks5://` (case-insensitive) — validation.go:L234-L245 | L1987 |

## Notes (checklist caveats)

1. `Duration` = `common.Duration` (nanosecond int wrapper accepting Go duration strings); `AdaptiveDuration` accepts scalar or `{base, quantile, min, max}` — interiors belong to the failsafe census.
2. yaml-tagged-but-internal fields excluded: `Config.UserScript` (`yaml:"-"`, config.go:L67), `ConnectorConfig.Mock` (`yaml:"-"`, config.go:L351).
3. Two defaults are test-mode-gated via `util.IsTest()` (util/testing.go:L19-L21): `server.listenV4` (defaults.go:L640-L644, override with `FORCE_TEST_LISTEN_V4=true`) and `metrics.enabled` (defaults.go:L750-L752).
4. YAML configs get environment-variable expansion (`os.ExpandEnv`) before decode and unknown yaml keys are a hard decode error (`KnownFields(true)`) — common/config.go:L102-L104. TS/JS configs bypass both (common/config.go:L95-L100).
5. `server.aliasing` and `projects` have a coupled derived default: zero projects ⇒ synthesized `main` project + catch-all aliasing rule — defaults.go:L100-L167.
6. The `admin.auth` tree shape (section 4b) is byte-identical for `healthCheck.auth` and `projects[].auth`; auth-scope connector defaults (`erpc_auth` table, minConns 1 / maxConns 4) apply wherever `AuthConfig` is used since `DatabaseStrategyConfig.SetDefaults` always passes `connectorScopeAuth` (defaults.go:L2722).
