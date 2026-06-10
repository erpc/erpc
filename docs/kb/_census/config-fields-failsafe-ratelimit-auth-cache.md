# Census: config fields — failsafe, rateLimiters, auth, database/cache, methods/staticResponses/directiveDefaults

> Scope: every yaml-tagged struct field in `common/config.go` for FailsafeConfig (+ retry/timeout/hedge/circuitBreaker/consensus policies, AdaptiveDuration, matchers), integrity policies, rateLimiters (store/budgets/rules/autoTune), auth (all strategies), database/cache (all connectors + SharedState), and methods/staticResponses/directiveDefaults. This is the completeness contract for KB coverage — checklist only, no prose. All paths relative to repo root; "—" in default column = zero value with no SetDefaults/runtime fallback.

Notes on path anchors used below:

- `failsafe[*]` (type `FailsafeConfig`, config.go:1279) attaches at: `projects[*].networks[*].failsafe[*]` (config.go:1998), `projects[*].networkDefaults.failsafe[*]` (config.go:595), `projects[*].upstreams[*].failsafe[*]` (config.go:734), `projects[*].upstreamDefaults.failsafe[*]` (config.go:512 + 734), and cache connectors `database.evmJsonRpcCache.connectors[*].failsafeForGets[*]` / `.failsafeForSets[*]` (config.go:349-350). Scope aliases: `NetworkFailsafeConfig`/`UpstreamFailsafeConfig`/`CacheFailsafeConfig` are pure type aliases of `FailsafeConfig` (config.go:1292, 1298, 1303).
- Legacy compat: a single failsafe OBJECT (not array) is accepted and wrapped into a 1-element array, with `matchMethod` forced to `"*"` — networks: config.go:2041-2103; networkDefaults: config.go:603-656.
- `auth` (type `AuthConfig`, config.go:2443) attaches at: `projects[*].auth` (config.go:509), `admin.auth` (config.go:249), `healthCheck.auth` (config.go:188).
- Connector structs are shared by `database.evmJsonRpcCache.connectors[*]`, `database.sharedState.connector`, and `projects[*].auth.strategies[*].database.connector`; scope-dependent defaults (table names, conn counts, id prefix) noted per row. Connector scopes: `shared-state`, `cache`, `auth` (defaults.go:36-42).
- x402 auth strategy: **DOES NOT EXIST** in this codebase. `grep -ri x402` over all `.go`/`.ts`/`.yaml` returns nothing; auth strategies are exactly secret/jwt/siwe/network/database (config.go:2433-2441, auth/ package files: strategy_secret.go, strategy_jwt.go, strategy_siwe.go, strategy_network.go, strategy_database.go).

---

## 1. Failsafe

### 1.1 FailsafeConfig envelope (matchers + policy slots)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `failsafe[*].matchMethod` | `string` | `"*"` (defaults.go:2132-2138; also forced to `"*"` for legacy single-object form config.go:2096-2098 / 649-651, and for cache-connector failsafe defaults.go:855-857, 868-870). Empty after defaults is a validation error (validation.go:986-988) | config.go:1280 |
| `failsafe[*].matchFinality` | `[]DataFinalityState` | nil (no default; empty = matches any finality, defaults.go:19-34). Valid values: `finalized`, `unfinalized`, `realtime`, `unknown` (data.go:54-76) | config.go:1281 |
| `failsafe[*].retry` | `*RetryPolicyConfig` | nil (policy disabled unless set; inherited from matching scope-defaults entry when present, defaults.go:2156-2171) | config.go:1282 |
| `failsafe[*].circuitBreaker` | `*CircuitBreakerPolicyConfig` | nil (inherited from scope defaults when present, defaults.go:2188-2203). By convention not used at network scope (config.go:1289-1292) | config.go:1283 |
| `failsafe[*].timeout` | `*TimeoutPolicyConfig` | nil (inherited from scope defaults when present, defaults.go:2140-2155) | config.go:1284 |
| `failsafe[*].hedge` | `*HedgePolicyConfig` | nil (inherited from scope defaults when present, defaults.go:2172-2187) | config.go:1285 |
| `failsafe[*].consensus` | `*ConsensusPolicyConfig` | nil (no inheritance from defaults — only own SetDefaults applied when set, defaults.go:2204-2208). Network-scope only by convention (config.go:1294-1297); rejected in cache-connector failsafe (validation.go:487-499) | config.go:1286 |

NOTE (zero-config baseline): when NO projects are configured at all, the auto-generated "main" project carries networkDefaults failsafe `{matchMethod:"*", retry:{maxAttempts:5, delay:0, jitter:0, backoffMaxDelay:0, backoffFactor:1.0}, timeout:{duration:120s}, hedge:{delay:{quantile:0.7}, maxCount:2}}` and upstreamDefaults failsafe `{matchMethod:"*", retry:{maxAttempts:1, delay:500ms}, timeout:{duration:60s}}` (defaults.go:100-167). These are NOT applied when the user defines any project.

### 1.2 Retry policy (`failsafe[*].retry`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `failsafe[*].retry.maxAttempts` | `int` | `3` (defaults.go:2226-2232) | config.go:1343 |
| `failsafe[*].retry.delay` | `Duration` | `0ms` (defaults.go:2247-2253) | config.go:1344 |
| `failsafe[*].retry.backoffMaxDelay` | `Duration` | `3s` (defaults.go:2240-2246); must be ≠0 after defaults (validation.go:1024-1026) | config.go:1345 |
| `failsafe[*].retry.backoffFactor` | `float32` | `1.2` (defaults.go:2233-2239); must be >0 (validation.go:1021-1023) | config.go:1346 |
| `failsafe[*].retry.jitter` | `Duration` | `0ms` (defaults.go:2254-2260) | config.go:1347 |
| `failsafe[*].retry.emptyResultAccept` | `[]string` | `DefaultEmptyResultAccept()` = `[eth_getLogs, trace_filter, arbtrace_filter, eth_call, eth_getBalance, eth_getCode, eth_getStorageAt, eth_getTransactionCount]` (applied defaults.go:2283-2285; list defined defaults.go:2013-2026) | config.go:1350 |
| `failsafe[*].retry.emptyResultIgnore` | `[]string` | nil — DEPRECATED; migrated into `emptyResultAccept` when that is unset (defaults.go:2261-2264, inherit path defaults.go:2276-2282) | config.go:1352 |
| `failsafe[*].retry.emptyResultMaxAttempts` | `int` | `2` (`DefaultEmptyResultMaxAttempts` const defaults.go:1980-1984; applied defaults.go:2287-2296) | config.go:1354 |
| `failsafe[*].retry.emptyResultDelay` | `Duration` | `0` — no hardcoded fallback; inherits from scope defaults only (defaults.go:2298-2302). Runtime prefers dynamic EMA-block-time delay (comment config.go:1355-1360) | config.go:1361 |
| `failsafe[*].retry.blockUnavailableDelay` | `Duration` | `0` — DEPRECATED yaml-only key (`json:"-"`); migrated into `emptyResultDelay` then zeroed, with warning log (defaults.go:2265-2274) | config.go:1369 |

### 1.3 Circuit breaker policy (`failsafe[*].circuitBreaker`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `failsafe[*].circuitBreaker.failureThresholdCount` | `uint` | `20` (defaults.go:2343-2349); must be >0 and ≤ capacity (validation.go:1044-1049) | config.go:1382 |
| `failsafe[*].circuitBreaker.failureThresholdCapacity` | `uint` | `80` (defaults.go:2350-2356); must be >0 (validation.go:1041-1043) | config.go:1383 |
| `failsafe[*].circuitBreaker.halfOpenAfter` | `Duration` | `5m` (defaults.go:2364-2370); required ≠0 (validation.go:1038-1040) | config.go:1384 |
| `failsafe[*].circuitBreaker.successThresholdCount` | `uint` | `8` (defaults.go:2371-2377); must be >0 and ≤ successThresholdCapacity (validation.go:1050-1058) | config.go:1385 |
| `failsafe[*].circuitBreaker.successThresholdCapacity` | `uint` | `200` (defaults.go:2357-2363; note the second `if c.SuccessThresholdCapacity == 0` branch at defaults.go:2378-2384 with value 10 is unreachable dead code — capacity is already set to 200 above) | config.go:1386 |

### 1.4 Timeout policy (`failsafe[*].timeout`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `failsafe[*].timeout.duration` | `*AdaptiveDuration` (scalar shorthand or object) | nil — NO built-in default; only inherits/merges from scope defaults via `inheritFrom` (defaults.go:2213-2223). Required when timeout block present (validation.go:1013-1018) | config.go:1407 |
| `failsafe[*].timeout.quantile` (LEGACY flat sibling) | `float64` | 0 — folded into `duration.quantile` at unmarshal (config.go:1420-1434 YAML, 1437-1462 JSON, fold logic 1464-1480) | config.go:1423 (legacy shadow) |
| `failsafe[*].timeout.minDuration` (LEGACY flat sibling) | `Duration` | 0 — folded into `duration.min` (config.go:1464-1480) | config.go:1424 (legacy shadow) |
| `failsafe[*].timeout.maxDuration` (LEGACY flat sibling) | `Duration` | 0 — folded into `duration.max` (config.go:1464-1480) | config.go:1425 (legacy shadow) |

### 1.5 Hedge policy (`failsafe[*].hedge`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `failsafe[*].hedge.delay` | `*AdaptiveDuration` (scalar shorthand or object) | auto-created `&AdaptiveDuration{}` then `delay.min` defaults to `100ms` and `delay.max` to `999s` (consts defaults.go:2310-2313; applied defaults.go:2315-2329). Delay must be non-zero overall (validation.go:1030-1035). Hedge `quantile` rejected for cache-connector failsafe (config.go:1300-1303, validation.go:487-499) | config.go:1490 |
| `failsafe[*].hedge.maxCount` | `int` | `1` (defaults.go:2331-2337) | config.go:1491 |
| `failsafe[*].hedge.quantile` (LEGACY flat sibling) | `float64` | 0 — folded into `delay.quantile` (config.go:1507-1523 YAML, 1526-1553 JSON, fold logic 1555-1571) | config.go:1511 (legacy shadow) |
| `failsafe[*].hedge.minDelay` (LEGACY flat sibling) | `Duration` | 0 — folded into `delay.min` (config.go:1555-1571) | config.go:1512 (legacy shadow) |
| `failsafe[*].hedge.maxDelay` (LEGACY flat sibling) | `Duration` | 0 — folded into `delay.max` (config.go:1555-1571) | config.go:1513 (legacy shadow) |

### 1.6 AdaptiveDuration (shared shape: `timeout.duration`, `hedge.delay`, `consensus.maxWaitOnResult`, `consensus.maxWaitOnEmpty`)

Defined in common/adaptive_duration.go (not config.go). Scalar shorthand ("5s" or number-as-ms) populates `base` (YAML: adaptive_duration.go:162-190; JSON: 192-248). Resolution `final = base + adaptive(quantile)` clamped to [min,max] (adaptive_duration.go:33-58, 83-109). Validation: quantile ∈ [0,1]; quantile>0 requires base or max; all-zero spec invalid; min ≤ max (adaptive_duration.go:120-139).

| yaml path (full dotted) | Go type | exact default | source line |
|---|---|---|---|
| `<adaptive>.base` | `Duration` | `0` (no global default; per-use-site defaults listed in sections 1.4/1.5/1.7) | common/adaptive_duration.go:61 |
| `<adaptive>.quantile` | `float64` | `0` (= static; Min/Max only apply when quantile > 0, adaptive_duration.go:80-90) | common/adaptive_duration.go:62 |
| `<adaptive>.min` | `Duration` | `0` (per-use-site defaults apply, e.g. hedge 100ms) | common/adaptive_duration.go:63 |
| `<adaptive>.max` | `Duration` | `0` (per-use-site defaults apply, e.g. hedge 999s) | common/adaptive_duration.go:64 |

### 1.7 Consensus policy (`failsafe[*].consensus`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `failsafe[*].consensus.maxParticipants` | `int` | `5` (defaults.go:2390-2392); must be >0 and ≥ agreementThreshold (validation.go:1063-1071) | config.go:1592 |
| `failsafe[*].consensus.agreementThreshold` | `int` | `2` (defaults.go:2393-2395); must be >0 (validation.go:1066-1068) | config.go:1593 |
| `failsafe[*].consensus.disputeBehavior` | `ConsensusDisputeBehavior` | `"returnError"` (defaults.go:2396-2398). Valid: `returnError`, `acceptMostCommonValidResult`, `preferBlockHeadLeader`, `onlyBlockHeadLeader` (config.go:1582-1589) | config.go:1594 |
| `failsafe[*].consensus.lowParticipantsBehavior` | `ConsensusLowParticipantsBehavior` | `"acceptMostCommonValidResult"` (defaults.go:2399-2401). Valid: same four values (config.go:1573-1580) | config.go:1595 |
| `failsafe[*].consensus.punishMisbehavior` | `*PunishMisbehaviorConfig` | nil (disabled) | config.go:1596 |
| `failsafe[*].consensus.disputeLogLevel` | `string` | `"warn"` (defaults.go:2402-2404). Valid per comment: trace/debug/info/warn/error (config.go:1597) | config.go:1597 |
| `failsafe[*].consensus.ignoreFields` | `map[string][]string` | `{eth_getLogs: ["*.blockTimestamp"], eth_getTransactionReceipt: ["blockTimestamp","logs.*.blockTimestamp"], eth_getBlockReceipts: ["*.blockTimestamp","*.logs.*.blockTimestamp"]}` (defaults.go:2405-2419) | config.go:1598 |
| `failsafe[*].consensus.preferNonEmpty` | `*bool` | `true` (defaults.go:2420-2422) | config.go:1599 |
| `failsafe[*].consensus.preferLargerResponses` | `*bool` | `true` (defaults.go:2423-2425) | config.go:1600 |
| `failsafe[*].consensus.misbehaviorsDestination` | `*MisbehaviorsDestinationConfig` | nil (export disabled; sub-defaults applied only when set, defaults.go:2447-2452) | config.go:1601 |
| `failsafe[*].consensus.preferHighestValueFor` | `map[string][]string` | nil (no default) | config.go:1607 |
| `failsafe[*].consensus.fireAndForget` | `bool` | `false` (zero value; no SetDefaults — comment config.go:1608-1612) | config.go:1613 |
| `failsafe[*].consensus.maxWaitOnResult` | `*AdaptiveDuration` | `{quantile: 0.5, min: 5ms, max: 1s}` (defaults.go:2427-2438) | config.go:1625 |
| `failsafe[*].consensus.maxWaitOnEmpty` | `*AdaptiveDuration` | `{quantile: 0.9, min: 50ms, max: 2s}` (defaults.go:2439-2445) | config.go:1633 |
| `failsafe[*].consensus.requiredParticipants` | `[]*ConsensusRequiredParticipant` | nil (empty = disabled, comment config.go:1635-1647) | config.go:1648 |

### 1.8 `failsafe[*].consensus.requiredParticipants[*]` (ConsensusRequiredParticipant)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `failsafe[*].consensus.requiredParticipants[*].tag` | `string` | — (required, glob pattern; validation.go:1089-1091) | config.go:1657 |
| `failsafe[*].consensus.requiredParticipants[*].minParticipants` | `int` | — (required >0 and ≤ maxParticipants; validation.go:1092-1097) | config.go:1658 |

### 1.9 `failsafe[*].consensus.misbehaviorsDestination` (MisbehaviorsDestinationConfig)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `failsafe[*].consensus.misbehaviorsDestination.type` | `MisbehaviorsDestinationType` | `"file"` (defaults.go:2459-2462). Valid: `file`, `s3` (config.go:1709-1714; validation.go:1104-1131) | config.go:1718 |
| `failsafe[*].consensus.misbehaviorsDestination.path` | `string` | — (required; file type requires absolute path, validation.go:1107-1113) | config.go:1721 |
| `failsafe[*].consensus.misbehaviorsDestination.filePattern` | `string` | `"{timestampMs}-{method}-{networkId}"` (defaults.go:2463-2466); placeholders documented config.go:1723-1728 | config.go:1729 |
| `failsafe[*].consensus.misbehaviorsDestination.s3` | `*S3FlushConfig` | nil; auto-created `&S3FlushConfig{}` when type == s3 (defaults.go:2468-2471) | config.go:1732 |

### 1.10 `failsafe[*].consensus.misbehaviorsDestination.s3` (S3FlushConfig)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `…misbehaviorsDestination.s3.maxRecords` | `int` | `100` (defaults.go:2472-2474); must be ≥0 (validation.go:1137-1139) | config.go:1737 |
| `…misbehaviorsDestination.s3.maxSize` | `int64` | `1048576` (1 MiB, defaults.go:2475-2477); must be ≥0 (validation.go:1140-1142) | config.go:1740 |
| `…misbehaviorsDestination.s3.flushInterval` | `Duration` | `60s` (defaults.go:2478-2480); must be ≥0 (validation.go:1143-1145) | config.go:1743 |
| `…misbehaviorsDestination.s3.region` | `string` | `""` — intentionally left empty → AWS SDK env/IMDS chain (defaults.go:2484) | config.go:1746 |
| `…misbehaviorsDestination.s3.credentials` | `*AwsAuthConfig` | nil → standard AWS credential chain (comment config.go:1748-1752); mode validation validation.go:1146-1167 | config.go:1753 |
| `…misbehaviorsDestination.s3.contentType` | `string` | `"application/jsonl"` (defaults.go:2481-2483) | config.go:1756 |

### 1.11 `failsafe[*].consensus.punishMisbehavior` (PunishMisbehaviorConfig)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `failsafe[*].consensus.punishMisbehavior.disputeThreshold` | `uint` | — (no SetDefaults; required >0, validation.go:1172-1174) | config.go:1786 |
| `failsafe[*].consensus.punishMisbehavior.disputeWindow` | `Duration` | — (no SetDefaults; required >0, validation.go:1175-1177; runtime guard disables punishment when ≤0, consensus/executor.go:1189-1192) | config.go:1787 |
| `failsafe[*].consensus.punishMisbehavior.sitOutPenalty` | `Duration` | — (no SetDefaults; required >0, validation.go:1178-1180) | config.go:1788 |

### 1.12 Integrity policies

Upstream-level (`projects[*].upstreams[*].evm.integrity`, attach config.go:1136; type `UpstreamIntegrityConfig` config.go:988; inherited from `upstreamDefaults.evm.integrity` via Copy, defaults.go:1563-1568):

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `projects[*].upstreams[*].evm.integrity.eth_getBlockReceipts` | `*UpstreamIntegrityEthGetBlockReceiptsConfig` | nil (no SetDefaults exists for this struct) | config.go:989 |
| `projects[*].upstreams[*].evm.integrity.eth_getBlockReceipts.enabled` | `bool` | `false` (zero value; no SetDefaults) | config.go:1007 |
| `projects[*].upstreams[*].evm.integrity.eth_getBlockReceipts.checkLogIndexStrictIncrements` | `*bool` | nil (zero value; no SetDefaults) | config.go:1008 |
| `projects[*].upstreams[*].evm.integrity.eth_getBlockReceipts.checkLogsBloom` | `*bool` | nil (zero value; no SetDefaults) | config.go:1009 |

Network-level (DEPRECATED — `projects[*].networks[*].evm.integrity` attach config.go:2181, also `networkDefaults.evm.integrity`; type `EvmIntegrityConfig` config.go:2309; values migrated into `directiveDefaults` at defaults.go:1952-1966; still read directly by hooks architecture/evm/eth_blockNumber.go:25-27, eth_getLogs.go:283-285, trace_filter.go:290-292):

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `projects[*].networks[*].evm.integrity.enforceHighestBlock` | `*bool` | `true` (defaults.go:2117-2120) — deprecated; use `directiveDefaults.enforceHighestBlock` (config.go:2310) | config.go:2311 |
| `projects[*].networks[*].evm.integrity.enforceGetLogsBlockRange` | `*bool` | `true` (defaults.go:2121-2123) — deprecated (config.go:2312) | config.go:2313 |
| `projects[*].networks[*].evm.integrity.enforceNonNullTaggedBlocks` | `*bool` | `true` (defaults.go:2124-2126) — deprecated (config.go:2314) | config.go:2315 |

---

## 2. rateLimiters

### 2.1 Root (`rateLimiters`, type `RateLimiterConfig`, attach config.go:46)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `rateLimiters.store` | `*RateLimitStoreConfig` | auto-created `&RateLimitStoreConfig{}` (defaults.go:2763-2769); required by validation when rateLimiters present (validation.go:165-167) | config.go:1801 |
| `rateLimiters.budgets` | `[]*RateLimitBudgetConfig` | nil (no default budgets) | config.go:1802 |

### 2.2 Store (`rateLimiters.store`, type `RateLimitStoreConfig`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `rateLimiters.store.driver` | `string` | `"memory"` (defaults.go:2781-2784). Valid: `redis`, `memory` (validation.go:168-186) | config.go:2586 |
| `rateLimiters.store.redis` | `*RedisConnectorConfig` | nil; auto-created + RedisConnectorConfig.SetDefaults when driver == "redis" (defaults.go:2785-2792); required when driver redis (validation.go:170-172). Field defaults: see §4.8 | config.go:2587 |
| `rateLimiters.store.cacheKeyPrefix` | `string` | `""` in config; runtime fallback `"erpc_rl_"` (upstream/ratelimiter_registry.go:257-262) | config.go:2588 |
| `rateLimiters.store.nearLimitRatio` | `float32` | `0` in config; runtime fallback `0.8` (upstream/ratelimiter_registry.go:250-255); if set must satisfy 0 < x < 1 (validation.go:173-175) | config.go:2589 |

### 2.3 Budgets and rules

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `rateLimiters.budgets[*].id` | `string` | — (no default; referenced by `rateLimitBudget` fields elsewhere, lookup config.go:1973-1983) | config.go:1806 |
| `rateLimiters.budgets[*].rules` | `[]*RateLimitRuleConfig` | — (required, ≥1 rule; validation.go:198-201) | config.go:1807 |
| `rateLimiters.budgets[*].rules[*].method` | `string` | `"*"` (defaults.go:2817-2819); required non-empty post-defaults (validation.go:211-213) | config.go:1811 |
| `rateLimiters.budgets[*].rules[*].maxCount` | `uint32` | — (no default) | config.go:1812 |
| `rateLimiters.budgets[*].rules[*].period` | `RateLimitPeriod` (int enum, marshals as string) | `second` (invalid/unset coerced, defaults.go:2808-2816). Valid: second/minute/hour/day/week/month/year; unmarshal also accepts int enums and duration aliases like `1s`, `24h`, `7d`-equivalents (config.go:1882-1950); validated validation.go:219-226 | config.go:1814 |
| `rateLimiters.budgets[*].rules[*].waitTime` | `Duration` | `0` — DEPRECATED, warned and ignored (validation.go:214-217) | config.go:1815 |
| `rateLimiters.budgets[*].rules[*].perIP` | `bool` | `false` (zero value; scope string building config.go:1821-1835) | config.go:1816 |
| `rateLimiters.budgets[*].rules[*].perUser` | `bool` | `false` (zero value) | config.go:1817 |
| `rateLimiters.budgets[*].rules[*].perNetwork` | `bool` | `false` (zero value) | config.go:1818 |

### 2.4 Upstream auto-tuner (`projects[*].upstreams[*].rateLimitAutoTune`, type `RateLimitAutoTuneConfig`, attach config.go:736; also inherited from `upstreamDefaults` defaults.go:1517-1519)

Auto-created (empty, then defaulted) whenever the upstream has a non-empty `rateLimitBudget` and no explicit autoTune (defaults.go:1674-1681).

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `projects[*].upstreams[*].rateLimitAutoTune.enabled` | `*bool` | `true` (defaults.go:2490-2492) | config.go:1052 |
| `projects[*].upstreams[*].rateLimitAutoTune.adjustmentPeriod` | `Duration` | `1m` (defaults.go:2493-2495) | config.go:1053 |
| `projects[*].upstreams[*].rateLimitAutoTune.errorRateThreshold` | `float64` | `0.1` (defaults.go:2496-2498) | config.go:1054 |
| `projects[*].upstreams[*].rateLimitAutoTune.increaseFactor` | `float64` | `1.05` (defaults.go:2499-2501) | config.go:1055 |
| `projects[*].upstreams[*].rateLimitAutoTune.decreaseFactor` | `float64` | `0.95` (defaults.go:2502-2504) | config.go:1056 |
| `projects[*].upstreams[*].rateLimitAutoTune.minBudget` | `int` | `0` (no SetDefaults entry — stays zero; defaults.go:2489-2510 sets every other field) | config.go:1057 |
| `projects[*].upstreams[*].rateLimitAutoTune.maxBudget` | `int` | `100000` (defaults.go:2505-2507) | config.go:1058 |

---

## 3. auth

Paths below use `auth.*`; substitute attach point: `projects[*].auth`, `admin.auth`, or `healthCheck.auth`.

### 3.1 AuthConfig + strategy envelope

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `auth.strategies` | `[]*AuthStrategyConfig` | nil (per-strategy SetDefaults applied, defaults.go:2611-2621) | config.go:2444 |
| `auth.strategies[*].ignoreMethods` | `[]string` | nil | config.go:2448 |
| `auth.strategies[*].allowMethods` | `[]string` | nil | config.go:2449 |
| `auth.strategies[*].rateLimitBudget` | `string` | `""` | config.go:2450 |
| `auth.strategies[*].type` | `AuthType` | no literal default — INFERRED/OVERWRITTEN from whichever sub-config block is present: secret→`secret` (defaults.go:2636-2637), database→`database` (2645-2646), jwt→`jwt` (2655-2656), siwe→`siwe` (2665-2666); conversely setting `type` auto-creates the empty sub-config (network defaults.go:2624-2626, secret 2633-2635, database 2642-2644, jwt 2652-2654, siwe 2662-2664). Valid: `secret`, `database`, `jwt`, `siwe`, `network` (config.go:2433-2441) | config.go:2452 |
| `auth.strategies[*].network` | `*NetworkStrategyConfig` | nil; auto-created when type == network (defaults.go:2624-2626) | config.go:2453 |
| `auth.strategies[*].secret` | `*SecretStrategyConfig` | nil; auto-created when type == secret (defaults.go:2633-2635) | config.go:2454 |
| `auth.strategies[*].database` | `*DatabaseStrategyConfig` | nil; auto-created when type == database (defaults.go:2642-2644) | config.go:2455 |
| `auth.strategies[*].jwt` | `*JwtStrategyConfig` | nil; auto-created when type == jwt (defaults.go:2652-2654) | config.go:2456 |
| `auth.strategies[*].siwe` | `*SiweStrategyConfig` | nil; auto-created when type == siwe (defaults.go:2662-2664) | config.go:2457 |

### 3.2 Secret strategy (`auth.strategies[*].secret`)

No defaults (defaults.go:2744-2746). JSON marshal redacts value (config.go:2468-2472); YAML marshal redacts value (config.go:2474-2480).

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `auth.strategies[*].secret.id` | `string` | — | config.go:2461 |
| `auth.strategies[*].secret.value` | `string` | — | config.go:2462 |
| `auth.strategies[*].secret.rateLimitBudget` | `string` | `""` (applied as per-user budget when set, comment config.go:2463) | config.go:2464 |

### 3.3 JWT strategy (`auth.strategies[*].jwt`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `auth.strategies[*].jwt.allowedIssuers` | `[]string` | nil | config.go:2509 |
| `auth.strategies[*].jwt.allowedAudiences` | `[]string` | nil | config.go:2510 |
| `auth.strategies[*].jwt.allowedAlgorithms` | `[]string` | nil | config.go:2511 |
| `auth.strategies[*].jwt.requiredClaims` | `[]string` | nil | config.go:2512 |
| `auth.strategies[*].jwt.verificationKeys` | `map[string]string` | nil | config.go:2513 |
| `auth.strategies[*].jwt.rateLimitBudgetClaimName` | `string` | `"rlm"` (defaults.go:2748-2753) | config.go:2517 |

### 3.4 SIWE strategy (`auth.strategies[*].siwe`)

No defaults (defaults.go:2755-2757).

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `auth.strategies[*].siwe.allowedDomains` | `[]string` | nil | config.go:2521 |
| `auth.strategies[*].siwe.rateLimitBudget` | `string` | `""` | config.go:2523 |

### 3.5 Network strategy (`auth.strategies[*].network`)

No defaults (defaults.go:2759-2761).

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `auth.strategies[*].network.allowedIPs` | `[]string` | nil | config.go:2527 |
| `auth.strategies[*].network.allowedCIDRs` | `[]string` | nil | config.go:2528 |
| `auth.strategies[*].network.allowLocalhost` | `bool` | `false` (zero value) | config.go:2529 |
| `auth.strategies[*].network.trustedProxies` | `[]string` | nil | config.go:2530 |
| `auth.strategies[*].network.rateLimitBudget` | `string` | `""` (applied to client IP as user, comment config.go:2531) | config.go:2532 |
| `auth.strategies[*].network.ipAsUser` | `bool` | `false` (zero value) | config.go:2533 |

### 3.6 Database strategy (`auth.strategies[*].database`)

All sub-blocks auto-created by SetDefaults (defaults.go:2675-2689); connector defaulted with scope `auth` (defaults.go:2722).

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `auth.strategies[*].database.connector` | `*ConnectorConfig` | auto-created `&ConnectorConfig{}` then ConnectorConfig.SetDefaults(scope=auth) (defaults.go:2676-2678, 2722). Scope effects: id `auth-<driver>` (defaults.go:847-849); postgres table `erpc_auth`, minConns 1, maxConns 4 (defaults.go:1021-1047); dynamodb table `erpc_auth` (defaults.go:1061-1073). All connector fields: §4.6-4.11 | config.go:2483 |
| `auth.strategies[*].database.cache` | `*DatabaseStrategyCacheConfig` | auto-created (defaults.go:2680-2682) | config.go:2484 |
| `auth.strategies[*].database.cache.ttl` | `*time.Duration` (NOT common.Duration) | `1h` (defaults.go:2691-2694) | config.go:2491 |
| `auth.strategies[*].database.cache.maxSize` | `*int64` | `10000` (defaults.go:2696-2699) | config.go:2492 |
| `auth.strategies[*].database.cache.maxCost` | `*int64` | `1073741824` (1<<30 = 1 GiB, defaults.go:2701-2704) | config.go:2493 |
| `auth.strategies[*].database.cache.numCounters` | `*int64` | `100000` (defaults.go:2706-2709) | config.go:2494 |
| `auth.strategies[*].database.retry` | `*DatabaseRetryConfig` | auto-created (defaults.go:2684-2686) | config.go:2485 |
| `auth.strategies[*].database.retry.maxAttempts` | `int` | `3` (defaults.go:2725-2728) | config.go:2498 |
| `auth.strategies[*].database.retry.baseBackoff` | `Duration` | `100ms` (defaults.go:2729-2731) | config.go:2499 |
| `auth.strategies[*].database.failOpen` | `*DatabaseFailOpenConfig` | auto-created (defaults.go:2687-2689) | config.go:2486 |
| `auth.strategies[*].database.failOpen.enabled` | `bool` | `false` (kept as-is — "Disabled by default", defaults.go:2735-2737) | config.go:2503 |
| `auth.strategies[*].database.failOpen.userId` | `string` | `"emergency-failopen"` (defaults.go:2738-2740) | config.go:2504 |
| `auth.strategies[*].database.failOpen.rateLimitBudget` | `string` | `""` | config.go:2505 |
| `auth.strategies[*].database.maxWait` | `Duration` | `1s` (defaults.go:2718-2720) | config.go:2487 |

### 3.7 x402 strategy

**Not present in codebase** (no Go type, no yaml tag, no AuthType constant — verified by repo-wide grep over `.go`/`.ts`/`.yaml`/`.yml`). If a docs page mentions x402 it is stale.

---

## 4. database / cache

### 4.1 Root (`database`, type `DatabaseConfig`, attach config.go:44)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `database.evmJsonRpcCache` | `*CacheConfig` | nil (cache disabled; SetDefaults only when present, defaults.go:831-836) | config.go:265 |
| `database.sharedState` | `*SharedStateConfig` | nil at config level (SetDefaults only when block present, defaults.go:837-841); note runtime may still build an in-memory shared registry elsewhere — config census only | config.go:266 |

### 4.2 Shared state (`database.sharedState`, type `SharedStateConfig`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `database.sharedState.clusterKey` | `string` | inherits top-level `clusterKey` (defaults.go:803-805), whose own default is `"erpc-default"` (defaults.go:53-55) | config.go:271 |
| `database.sharedState.connector` | `*ConnectorConfig` | `{id: "memory", driver: memory, memory: {maxItems: 100000, maxTotalSize: "1GB"}}` (defaults.go:790-797); when user supplies connector without id, id = driver name (defaults.go:798-802); then ConnectorConfig.SetDefaults(scope=shared-state) (defaults.go:806-808) | config.go:273 |
| `database.sharedState.fallbackTimeout` | `Duration` | `3s` (defaults.go:809-813) | config.go:276 |
| `database.sharedState.lockTtl` | `Duration` | `4s` (defaults.go:814-818) | config.go:279 |
| `database.sharedState.lockMaxWait` | `Duration` | `100ms` (defaults.go:819-823) | config.go:282 |
| `database.sharedState.updateMaxWait` | `Duration` | `50ms` (defaults.go:824-827) | config.go:285 |

### 4.3 Cache root (`database.evmJsonRpcCache`, type `CacheConfig`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `database.evmJsonRpcCache.connectors` | `[]*ConnectorConfig` | nil (each gets ConnectorConfig.SetDefaults(scope=cache), defaults.go:474-480) | config.go:289 |
| `database.evmJsonRpcCache.policies` | `[]*CachePolicyConfig` | nil (each gets CachePolicyConfig.SetDefaults, defaults.go:467-473) | config.go:290 |
| `database.evmJsonRpcCache.compression` | `*CompressionConfig` | auto-created `&CompressionConfig{}` + defaults (defaults.go:482-488) | config.go:291 |

### 4.4 Compression (`database.evmJsonRpcCache.compression`, type `CompressionConfig`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `database.evmJsonRpcCache.compression.enabled` | `*bool` | `true` (defaults.go:579-582) | config.go:295 |
| `database.evmJsonRpcCache.compression.algorithm` | `string` | `"zstd"` (defaults.go:584-587; only zstd supported per comment config.go:296) | config.go:296 |
| `database.evmJsonRpcCache.compression.zstdLevel` | `string` | `"fastest"` (defaults.go:589-592). Valid per comment: fastest/default/better/best (config.go:297) | config.go:297 |
| `database.evmJsonRpcCache.compression.threshold` | `int` | `1024` bytes (defaults.go:594-599) | config.go:298 |

### 4.5 Cache policies (`database.evmJsonRpcCache.policies[*]`, type `CachePolicyConfig`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `database.evmJsonRpcCache.policies[*].connector` | `string` | — (must reference a connector id; validation.go:361+) | config.go:319 |
| `database.evmJsonRpcCache.policies[*].network` | `string` | `"*"` (defaults.go:608-610) | config.go:320 |
| `database.evmJsonRpcCache.policies[*].method` | `string` | `"*"` (defaults.go:605-607) | config.go:321 |
| `database.evmJsonRpcCache.policies[*].params` | `[]interface{}` | nil | config.go:322 |
| `database.evmJsonRpcCache.policies[*].finality` | `DataFinalityState` | `finalized` — int-enum zero value; INTENTIONAL safe default when omitted (data.go:10-15). Valid: finalized/unfinalized/realtime/unknown (data.go:54-76) | config.go:323 |
| `database.evmJsonRpcCache.policies[*].empty` | `CacheEmptyBehavior` | `ignore` — int-enum zero value (data.go:78-84). Valid: ignore/allow/only (data.go:98-116) | config.go:324 |
| `database.evmJsonRpcCache.policies[*].appliesTo` | `CachePolicyAppliesTo` | `"both"` (defaults.go:611-613). Valid: get/set/both (data.go:118-125) | config.go:325 |
| `database.evmJsonRpcCache.policies[*].minItemSize` | `*string` (ByteSize) | nil | config.go:326 |
| `database.evmJsonRpcCache.policies[*].maxItemSize` | `*string` (ByteSize) | nil | config.go:327 |
| `database.evmJsonRpcCache.policies[*].ttl` | `Duration` | `0` (no default → no TTL) | config.go:328 |

### 4.6 Connector envelope (type `ConnectorConfig`; used by cache connectors, sharedState.connector, auth database.connector)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `…connectors[*].id` | `string` | `"<scope>-<driver>"` e.g. `cache-memory`, `auth-redis` (defaults.go:846-849); EXCEPTION: sharedState pre-sets id to driver name or "memory" before this runs (defaults.go:790-802) | config.go:342 |
| `…connectors[*].driver` | `ConnectorDriverType` | no literal default — INFERRED/OVERWRITTEN from whichever driver sub-config block is present (memory defaults.go:876-878, redis 887-889, postgresql 898-900, dynamodb 909-911, grpc 920-922). Valid: memory/redis/postgresql/dynamodb/grpc (config.go:333-339) | config.go:343 |
| `…connectors[*].memory` | `*MemoryConnectorConfig` | nil; auto-created when driver == memory (defaults.go:879-886) | config.go:344 |
| `…connectors[*].redis` | `*RedisConnectorConfig` | nil; auto-created when driver == redis (defaults.go:890-897) | config.go:345 |
| `…connectors[*].dynamodb` | `*DynamoDBConnectorConfig` | nil; auto-created when driver == dynamodb (defaults.go:912-919) | config.go:346 |
| `…connectors[*].postgresql` | `*PostgreSQLConnectorConfig` | nil; auto-created when driver == postgresql (defaults.go:901-908) | config.go:347 |
| `…connectors[*].grpc` | `*GrpcConnectorConfig` | nil; auto-created when driver == grpc (defaults.go:923-930) | config.go:348 |
| `…connectors[*].failsafeForGets` | `[]*FailsafeConfig` | nil; per-entry matchMethod defaults `"*"` + FailsafeConfig.SetDefaults(nil) (defaults.go:850-862); consensus + hedge-quantile rejected (validation.go:474-499) | config.go:349 |
| `…connectors[*].failsafeForSets` | `[]*FailsafeConfig` | nil; same handling (defaults.go:863-875) | config.go:350 |
| (not yaml) `Mock` | `*MockConnectorConfig` | `yaml:"-" json:"-"` — test-only, not a config surface (struct config.go:367-373) | config.go:351 |

### 4.7 Memory connector (`…connectors[*].memory`, type `MemoryConnectorConfig`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `…connectors[*].memory.maxItems` | `int` | `100000` (defaults.go:935-938) | config.go:362 |
| `…connectors[*].memory.maxTotalSize` | `string` | `"1GB"` (defaults.go:939-941) | config.go:363 |
| `…connectors[*].memory.emitMetrics` | `*bool` | nil → metrics disabled (runtime check `cfg.EmitMetrics != nil && *cfg.EmitMetrics`, data/memory.go:71) | config.go:364 |

### 4.8 Redis connector (`…connectors[*].redis` / `rateLimiters.store.redis`, type `RedisConnectorConfig`)

Mutual exclusion: providing both `uri` and `addr` is a SetDefaults error (defaults.go:946-951). When only `addr`+friends given, a URI is constructed: missing port → `6379` (defaults.go:984-990), scheme `rediss` when `tls.enabled` (defaults.go:992-995), credentials URL-encoded (defaults.go:1002-1005), DB index appended as path (defaults.go:1007-1008); afterwards `addr/username/password/db` are CLEARED (defaults.go:1010-1015). Marshalers redact password/URI (config.go:397-426).

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `…redis.addr` | `string` | `""` (consumed into `uri` then cleared, defaults.go:976-1016; `redis://`/`rediss://` prefixes stripped defaults.go:978-980) | config.go:384 |
| `…redis.username` | `string` | `""` (folded into uri then cleared) | config.go:385 |
| `…redis.password` | `string` | `""` (folded into uri then cleared; json tag is `-`) | config.go:386 |
| `…redis.db` | `int` | `0` (folded into uri path then cleared) | config.go:387 |
| `…redis.tls` | `*TLSConfig` | nil (only `tls.enabled` flips constructed scheme to rediss, defaults.go:992-995) | config.go:388 |
| `…redis.connPoolSize` | `int` | `8` (defaults.go:954-957) | config.go:389 |
| `…redis.uri` | `string` | `""`; derived from addr fields when empty (defaults.go:971-1016) | config.go:390 |
| `…redis.initTimeout` | `Duration` | `5s` (defaults.go:958-960) | config.go:391 |
| `…redis.getTimeout` | `Duration` | `1s` (defaults.go:961-963) | config.go:392 |
| `…redis.setTimeout` | `Duration` | `3s` (defaults.go:964-966) | config.go:393 |
| `…redis.lockRetryInterval` | `Duration` | `500ms` (defaults.go:967-969) | config.go:394 |

TLS sub-block (type `TLSConfig`, shared; no SetDefaults anywhere):

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `…redis.tls.enabled` | `bool` | `false` (zero value) | config.go:376 |
| `…redis.tls.certFile` | `string` | `""` | config.go:377 |
| `…redis.tls.keyFile` | `string` | `""` | config.go:378 |
| `…redis.tls.caFile` | `string` | `""` | config.go:379 |
| `…redis.tls.insecureSkipVerify` | `bool` | `false` (zero value) | config.go:380 |

### 4.9 PostgreSQL connector (`…connectors[*].postgresql`, type `PostgreSQLConnectorConfig`)

Marshalers redact connectionUri (config.go:455-477).

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `…postgresql.connectionUri` | `string` | — (required; validation.go:532+) | config.go:446 |
| `…postgresql.table` | `string` | scope-dependent: cache → `"erpc_json_rpc_cache"`, shared-state → `"erpc_shared_state"`, auth → `"erpc_auth"` (defaults.go:1021-1033) | config.go:447 |
| `…postgresql.minConns` | `int32` | `4` (auth scope: `1`) (defaults.go:1034-1040) | config.go:448 |
| `…postgresql.maxConns` | `int32` | `32` (auth scope: `4`) (defaults.go:1041-1047) | config.go:449 |
| `…postgresql.initTimeout` | `Duration` | `5s` (defaults.go:1048-1050) | config.go:450 |
| `…postgresql.getTimeout` | `Duration` | `1s` (defaults.go:1051-1053) | config.go:451 |
| `…postgresql.setTimeout` | `Duration` | `2s` (defaults.go:1054-1056) | config.go:452 |

### 4.10 DynamoDB connector (`…connectors[*].dynamodb`, type `DynamoDBConnectorConfig`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `…dynamodb.table` | `string` | scope-dependent: cache → `"erpc_json_rpc_cache"`, shared-state → `"erpc_shared_state"`, auth → `"erpc_auth"` (defaults.go:1061-1073) | config.go:429 |
| `…dynamodb.region` | `string` | `""` (no default) | config.go:430 |
| `…dynamodb.endpoint` | `string` | `""` (no default) | config.go:431 |
| `…dynamodb.auth` | `*AwsAuthConfig` | nil (no default → AWS default credential chain) | config.go:432 |
| `…dynamodb.partitionKeyName` | `string` | `"groupKey"` (defaults.go:1074-1076) | config.go:433 |
| `…dynamodb.rangeKeyName` | `string` | `"requestKey"` (defaults.go:1077-1079) | config.go:434 |
| `…dynamodb.reverseIndexName` | `string` | `"idx_requestKey_groupKey"` (defaults.go:1080-1082) | config.go:435 |
| `…dynamodb.ttlAttributeName` | `string` | `"ttl"` (defaults.go:1083-1085) | config.go:436 |
| `…dynamodb.initTimeout` | `Duration` | `5s` (defaults.go:1086-1088) | config.go:437 |
| `…dynamodb.getTimeout` | `Duration` | `1s` (defaults.go:1089-1091) | config.go:438 |
| `…dynamodb.setTimeout` | `Duration` | `2s` (defaults.go:1092-1094) | config.go:439 |
| `…dynamodb.maxRetries` | `int` | `0` — no SetDefaults entry; passed raw to AWS SDK (data/dynamodb.go:135, 142) | config.go:440 |
| `…dynamodb.statePollInterval` | `Duration` | `5s` (defaults.go:1095-1097) | config.go:441 |
| `…dynamodb.lockRetryInterval` | `Duration` | `0` — no SetDefaults entry; used raw by lock loop (data/dynamodb.go:107, 671-673) | config.go:442 |

AwsAuthConfig sub-block (shared by `…dynamodb.auth` and `…misbehaviorsDestination.s3.credentials`; no SetDefaults; marshalers redact secretAccessKey config.go:487-505):

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `…dynamodb.auth.mode` | `string` | — Valid: `file`, `env`, `secret` (tstype comment config.go:480; enforced for S3 use at validation.go:1146-1167) | config.go:480 |
| `…dynamodb.auth.credentialsFile` | `string` | — (required when mode=file, validation.go:1151-1157) | config.go:481 |
| `…dynamodb.auth.profile` | `string` | — (required when mode=file, validation.go:1151-1157) | config.go:482 |
| `…dynamodb.auth.accessKeyID` | `string` | — (required when mode=secret, validation.go:1158-1161) | config.go:483 |
| `…dynamodb.auth.secretAccessKey` | `string` | — (required when mode=secret, validation.go:1158-1161) | config.go:484 |

### 4.11 gRPC connector (`…connectors[*].grpc`, type `GrpcConnectorConfig`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `…grpc.bootstrap` | `string` | `""` (no default) | config.go:355 |
| `…grpc.servers` | `[]string` | nil (no default) | config.go:356 |
| `…grpc.headers` | `map[string]string` | nil (no default) | config.go:357 |
| `…grpc.getTimeout` | `Duration` | `100ms` (defaults.go:923-930, inline in ConnectorConfig.SetDefaults — note GrpcConnectorConfig has no own SetDefaults method) | config.go:358 |

---

## 5. methods / staticResponses / directiveDefaults

### 5.1 Methods (`projects[*].networks[*].methods`, type `MethodsConfig`, attach config.go:2003; auto-created per network defaults.go:1918-1924)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `projects[*].networks[*].methods.preserveDefaultMethods` | `bool` | `false` (zero value; semantics: false + own definitions = REPLACE defaults except stateful markers are always re-applied, defaults.go:493-576, esp. 561-573) | config.go:1991 |
| `projects[*].networks[*].methods.definitions` | `map[string]*CacheMethodConfig` | merged default map = DefaultStaticCacheMethods (defaults.go:188-195: eth_chainId, net_version) + DefaultRealtimeCacheMethods (defaults.go:198-232: eth_hashrate, eth_mining, eth_syncing, net_peerCount, eth_gasPrice, eth_maxPriorityFeePerGas, eth_blobBaseFee, eth_blockNumber, erigon_blockNumber) + DefaultWithBlockCacheMethods (defaults.go:259-427: ~45 block-ref methods) + DefaultSpecialCacheMethods (defaults.go:437-464: 8 arbitrary-block methods) + stateful marks from DefaultStatefulMethodNames (defaults.go:178-185: eth_newFilter, eth_newBlockFilter, eth_newPendingTransactionFilter, eth_getFilterChanges, eth_getFilterLogs, eth_uninstallFilter). Merge logic defaults.go:493-576 | config.go:1992 |

### 5.2 CacheMethodConfig (values of `methods.definitions.<method>`)

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `…methods.definitions.<m>.reqRefs` | `[][]interface{}` | nil (built-in methods carry their own, e.g. eth_getLogs defaults.go:260-268) | config.go:302 |
| `…methods.definitions.<m>.respRefs` | `[][]interface{}` | nil | config.go:303 |
| `…methods.definitions.<m>.finalized` | `bool` | `false` (zero value) | config.go:304 |
| `…methods.definitions.<m>.realtime` | `bool` | `false` (zero value) | config.go:305 |
| `…methods.definitions.<m>.stateful` | `bool` | `false`; FORCED `true` for the 6 DefaultStatefulMethodNames even with user-supplied definitions (defaults.go:512-519, 546-553, 563-572) | config.go:306 |
| `…methods.definitions.<m>.translateLatestTag` | `*bool` | nil → enabled/true (comment config.go:307-309); built-in eth_getBlockByNumber sets explicit `false` (defaults.go:281-286) | config.go:309 |
| `…methods.definitions.<m>.translateFinalizedTag` | `*bool` | nil → enabled/true (comment config.go:310-312); built-in eth_getBlockByNumber sets explicit `false` (defaults.go:286) | config.go:312 |
| `…methods.definitions.<m>.enforceBlockAvailability` | `*bool` | nil → enforced/true (comment config.go:313-315); built-ins eth_getLogs / eth_getBlockByNumber / trace_filter / arbtrace_filter set explicit `false` (defaults.go:267, 280, 387, 395) | config.go:315 |

### 5.3 Static responses (`projects[*].networks[*].staticResponses[*]`, type `StaticResponseConfig`, attach config.go:2005)

No SetDefaults; constraints from validation only (validation.go:1278-1300).

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `projects[*].networks[*].staticResponses[*].method` | `string` | — (required, validation.go:1282-1284) | config.go:2015 |
| `projects[*].networks[*].staticResponses[*].params` | `[]interface{}` | nil (match-any when omitted) | config.go:2016 |
| `projects[*].networks[*].staticResponses[*].response` | `*StaticResponseBodyConfig` | — (required, validation.go:1285-1287) | config.go:2017 |
| `projects[*].networks[*].staticResponses[*].response.result` | `interface{}` | nil — exactly one of result/error must be set (validation.go:1288-1295) | config.go:2023 |
| `projects[*].networks[*].staticResponses[*].response.error` | `*StaticResponseErrorConfig` | nil — exactly one of result/error (validation.go:1288-1295) | config.go:2024 |
| `projects[*].networks[*].staticResponses[*].response.error.code` | `int` | `0` (no default) | config.go:2029 |
| `projects[*].networks[*].staticResponses[*].response.error.message` | `string` | — (required when error set, validation.go:1296-1298) | config.go:2030 |
| `projects[*].networks[*].staticResponses[*].response.error.data` | `interface{}` | nil | config.go:2031 |

### 5.4 Directive defaults (`projects[*].networks[*].directiveDefaults`, type `DirectiveDefaultsConfig`, attach config.go:2001; also `projects[*].networkDefaults.directiveDefaults` config.go:597)

Auto-created per network even when omitted (defaults.go:1947-1950); SetDefaults at defaults.go:1454-1471. Legacy `evm.integrity` values migrate in when the new keys are unset (defaults.go:1952-1966). `skipCacheRead` accepts bool OR string and is normalized to string at unmarshal (config.go:2154-2175).

| yaml path (full dotted) | Go type | exact default | config.go line |
|---|---|---|---|
| `…directiveDefaults.retryEmpty` | `*bool` | nil (no default) | config.go:2106 |
| `…directiveDefaults.retryPending` | `*bool` | nil (no default) | config.go:2107 |
| `…directiveDefaults.skipCacheRead` | `interface{}` (normalized to string) | nil (no default; normalization config.go:2159-2162, 2171-2173) | config.go:2108 |
| `…directiveDefaults.useUpstream` | `*string` | nil (no default) | config.go:2109 |
| `…directiveDefaults.skipInterpolation` | `*bool` | nil (no default) | config.go:2110 |
| `…directiveDefaults.skipConsensus` | `*bool` | nil (no default) | config.go:2111 |
| `…directiveDefaults.enforceHighestBlock` | `*bool` | `true` (defaults.go:1458-1460; legacy integrity migration first, defaults.go:1956-1959) | config.go:2114 |
| `…directiveDefaults.enforceGetLogsBlockRange` | `*bool` | `true` (defaults.go:1461-1463; migration defaults.go:1960-1962) | config.go:2115 |
| `…directiveDefaults.enforceNonNullTaggedBlocks` | `*bool` | `true` (defaults.go:1464-1466; migration defaults.go:1963-1965) | config.go:2116 |
| `…directiveDefaults.validateTransactionsRoot` | `*bool` | `true` (defaults.go:1467-1469) | config.go:2120 |
| `…directiveDefaults.validateHeaderFieldLengths` | `*bool` | nil (no default in SetDefaults) | config.go:2123 |
| `…directiveDefaults.validateTransactionFields` | `*bool` | nil | config.go:2126 |
| `…directiveDefaults.validateTransactionBlockInfo` | `*bool` | nil | config.go:2127 |
| `…directiveDefaults.enforceLogIndexStrictIncrements` | `*bool` | nil | config.go:2130 |
| `…directiveDefaults.validateTxHashUniqueness` | `*bool` | nil | config.go:2131 |
| `…directiveDefaults.validateTransactionIndex` | `*bool` | nil | config.go:2132 |
| `…directiveDefaults.validateLogFields` | `*bool` | nil | config.go:2133 |
| `…directiveDefaults.validateLogsBloomEmptiness` | `*bool` | nil | config.go:2137 |
| `…directiveDefaults.validateLogsBloomMatch` | `*bool` | nil | config.go:2139 |
| `…directiveDefaults.validateReceiptTransactionMatch` | `*bool` | nil (requires GroundTruthTransactions in library-mode, comment config.go:2141) | config.go:2142 |
| `…directiveDefaults.validateContractCreation` | `*bool` | nil | config.go:2143 |
| `…directiveDefaults.receiptsCountExact` | `*int64` | nil | config.go:2146 |
| `…directiveDefaults.receiptsCountAtLeast` | `*int64` | nil | config.go:2147 |
| `…directiveDefaults.validationExpectedBlockHash` | `*string` | nil | config.go:2150 |
| `…directiveDefaults.validationExpectedBlockNumber` | `*int64` | nil | config.go:2151 |
