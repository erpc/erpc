# Census: config fields — projects / networks / upstreams / providers+vendors / healthCheck / cors / selectionPolicy / deprecated

Scope: completeness checklist (no behavior) of every yaml-tagged struct field reachable from `projects[]` (ProjectConfig tree), plus root `healthCheck`, shared `cors`, `selectionPolicy`, provider/vendor settings, and deprecated-but-still-parsed keys. Struct definitions from `common/config.go`; defaults from `common/defaults.go` SetDefaults methods (other files cited explicitly when a default is applied at runtime instead).

Conventions:

- Path placeholders (all paths rooted at the top of erpc.yaml):
  - `P.` = `projects[].`
  - `<UPS>` = any UpstreamConfig mount: `projects[].upstreams[]` | `projects[].upstreamDefaults` | `projects[].providers[].overrides.<pattern>` (overrides map declared at common/config.go:676)
  - `<NET>` = `projects[].networks[]`
  - `<FS>` = any FailsafeConfig mount in this census's scope: `projects[].networks[].failsafe[]` | `projects[].networkDefaults.failsafe[]` | `<UPS>.failsafe[]` (cache-connector `failsafeForGets`/`failsafeForSets` reuse the same struct — covered by the database census; mounts: common/config.go:595, 734, 1998, 349-350)
  - `<SP>` = any SelectionPolicyConfig mount: `projects[].networks[].selectionPolicy` | `projects[].networkDefaults.selectionPolicy` (common/config.go:596, 2000)
  - `<DD>` = any DirectiveDefaultsConfig mount: `projects[].networks[].directiveDefaults` | `projects[].networkDefaults.directiveDefaults` (common/config.go:597, 2001)
  - `<ENET>` = any EvmNetworkConfig mount: `projects[].networks[].evm` | `projects[].networkDefaults.evm` (common/config.go:598, 1999)
  - `<AUTH>` = any AuthConfig mount in scope: `projects[].auth` | `healthCheck.auth` (common/config.go:509, 188; `admin.auth` at config.go:249 reuses the same struct — server census)
  - `<CORS>` = `projects[].cors` (common/config.go:510; `admin.cors` at config.go:250 reuses the same struct — server census)
- `Duration` = custom string-duration type (common/duration.go). `AdaptiveDuration` = scalar duration shorthand OR `{base, quantile, min, max}` object (common/adaptive_duration.go:61-64).
- Default column: "—" = plain Go zero value (nil / "" / 0 / false) with no SetDefaults override; "required" = Validate() rejects empty. Inheritance chains noted inline.
- Line column = `common/config.go` line of the field declaration unless another file is named.

## 1. ProjectConfig (`projects[]`) — common/config.go:507-539

When `projects` is entirely absent, a default project `id: main` is synthesized with networkDefaults failsafe (retry 5 attempts / timeout 120s / hedge p70 max 2), upstreamDefaults failsafe (retry 1 / timeout 60s), getLogs defaults, plus a `*`→`main` server aliasing rule (defaults.go:100-167).

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `projects[].id` | `string` | required (validation.go:581-582); auto `"main"` only when `projects` absent (defaults.go:111-113) | 508 |
| `projects[].auth` | `*AuthConfig` | — (nil) | 509 |
| `projects[].cors` | `*CORSConfig` | — (nil) | 510 |
| `projects[].providers` | `[]*ProviderConfig` | — (nil); auto `public` (repository) + `envio` providers when project has no providers AND no upstreams AND no CLI endpoints (defaults.go:1113-1143) | 511 |
| `projects[].upstreamDefaults` | `*UpstreamConfig` | — (nil) | 512 |
| `projects[].upstreams` | `[]*UpstreamConfig` | — (nil); synthesized from CLI/env endpoints `opts.Endpoints` when provided (defaults.go:1114-1123); non-http(s)/grpc shorthand endpoints are converted into providers and removed from this list (defaults.go:1164-1171, 1193-1241) | 513 |
| `projects[].networkDefaults` | `*NetworkDefaults` | — (nil) | 514 |
| `projects[].networks` | `[]*NetworkConfig` | — (nil; networks lazily registered at runtime) | 515 |
| `projects[].rateLimitBudget` | `string` | — (""); must reference existing budget if set (validation.go) | 516 |
| `projects[].userAgentMode` | `UserAgentTrackingMode` | — (""); effective `"simplified"` at use-site (erpc/http_server.go:654-656); values: `simplified`\|`raw` (config.go:584-589) | 518 |
| `projects[].forwardHeaders` | `[]string` | — (nil) | 519 |
| `projects[].ignoreMethods` | `[]string` | — (nil) | 520 |
| `projects[].allowMethods` | `[]string` | — (nil) | 521 |
| `projects[].scoreMetricsWindowSize` | `Duration` | 0; effective fallback `1m` via package var `ScoreMetricsWindowSize` (erpc/projects_registry.go:50, applied 133-136) | 533 |

Non-yaml runtime field (excluded from census proper): `LegacyProject *LegacyProjectFields` `yaml:"-"` (config.go:538) — populated by ProjectConfig.UnmarshalYAML from the deprecated keys in §17.1.

## 2. NetworkDefaults (`projects[].networkDefaults`) — common/config.go:593-600

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `P.networkDefaults.rateLimitBudget` | `string` | — ("") | 594 |
| `P.networkDefaults.failsafe[]` | `[]*FailsafeConfig` | — (nil); see §12 for fields; legacy single-object form accepted (config.go:626-656) | 595 |
| `P.networkDefaults.selectionPolicy` | `*SelectionPolicyConfig` | — (nil); see §15 | 596 |
| `P.networkDefaults.directiveDefaults` | `*DirectiveDefaultsConfig` | — (nil); see §6 | 597 |
| `P.networkDefaults.evm` | `*EvmNetworkConfig` | — (nil); see §7 | 598 |
| `P.networkDefaults.multiplexing` | `*bool` | — (nil); copied into each network when network leaves it nil (defaults.go:1841-1844) | 599 |

## 3. NetworkConfig (`projects[].networks[]`) — common/config.go:1995-2006

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<NET>.architecture` | `NetworkArchitecture` | "" → `"evm"` when an `evm` block exists (defaults.go:1908-1912); required after defaults (validation.go:1236-1238); only valid value: `evm` (common/network.go:12-15) | 1996 |
| `<NET>.rateLimitBudget` | `string` | "" → inherits `networkDefaults.rateLimitBudget` (defaults.go:1783-1785) | 1997 |
| `<NET>.failsafe[]` | `[]*FailsafeConfig` | nil → deep-copies `networkDefaults.failsafe` (defaults.go:1786-1792); per-entry merge by matchMethod/matchFinality when both set (defaults.go:1793-1832); legacy single-object form accepted (config.go:2064-2100); see §12 | 1998 |
| `<NET>.evm` | `*EvmNetworkConfig` | nil → copied from `networkDefaults.evm` (defaults.go:1890-1893), else auto-created empty when architecture=evm (defaults.go:1914-1916); field-by-field inheritance when both set (defaults.go:1845-1889); see §7 | 1999 |
| `<NET>.selectionPolicy` | `*SelectionPolicyConfig` | nil → copies `networkDefaults.selectionPolicy` (defaults.go:1833-1836); auto-attached default policy when any upstream has tag `tier:fallback` and none set (defaults.go:1932-1940); see §15 | 2000 |
| `<NET>.directiveDefaults` | `*DirectiveDefaultsConfig` | nil → copies `networkDefaults.directiveDefaults` (defaults.go:1837-1840); always created + SetDefaults applied (defaults.go:1948-1950, 1968); see §6 | 2001 |
| `<NET>.alias` | `string` | — (""); must be alphanumeric/dash/underscore when set (validation.go:1264-1269) | 2002 |
| `<NET>.methods` | `*MethodsConfig` | nil → auto-created and filled with merged default method map (defaults.go:1919-1924, 493-576); see §4 | 2003 |
| `<NET>.multiplexing` | `*bool` | nil → inherits `networkDefaults.multiplexing` (defaults.go:1841-1844); nil = enabled (`MultiplexingEnabled`, config.go:2034-2039) | 2004 |
| `<NET>.staticResponses[]` | `[]*StaticResponseConfig` | — (nil); see §5 | 2005 |

## 4. MethodsConfig + CacheMethodConfig (`<NET>.methods`) — common/config.go:1990-1993, 301-316

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<NET>.methods.preserveDefaultMethods` | `bool` | false; governs merge of user `definitions` with built-in maps (defaults.go:493-576) | 1991 |
| `<NET>.methods.definitions` | `map[string]*CacheMethodConfig` | nil → merged DefaultStaticCacheMethods + DefaultRealtimeCacheMethods + DefaultWithBlockCacheMethods + DefaultSpecialCacheMethods + stateful markers (defaults.go:188-464, 493-576) | 1992 |
| `<NET>.methods.definitions.<method>.reqRefs` | `[][]interface{}` | — (nil); built-ins per method in defaults.go:259-464 | 302 |
| `<NET>.methods.definitions.<method>.respRefs` | `[][]interface{}` | — (nil); built-ins per method | 303 |
| `<NET>.methods.definitions.<method>.finalized` | `bool` | false; built-in true for `eth_chainId`, `net_version` (defaults.go:188-195) | 304 |
| `<NET>.methods.definitions.<method>.realtime` | `bool` | false; built-in true for realtime set (defaults.go:198-232) | 305 |
| `<NET>.methods.definitions.<method>.stateful` | `bool` | false; forced true for DefaultStatefulMethodNames even with custom definitions (defaults.go:178-185, 512-519, 546-553, 562-573) | 306 |
| `<NET>.methods.definitions.<method>.translateLatestTag` | `*bool` | nil = enabled (config.go:307-309); built-in false for `eth_getBlockByNumber` (defaults.go:285) | 309 |
| `<NET>.methods.definitions.<method>.translateFinalizedTag` | `*bool` | nil = enabled (config.go:310-312); built-in false for `eth_getBlockByNumber` (defaults.go:286) | 312 |
| `<NET>.methods.definitions.<method>.enforceBlockAvailability` | `*bool` | nil = enabled (config.go:313-315); built-in false for `eth_getLogs`, `eth_getBlockByNumber`, `trace_filter`, `arbtrace_filter` (defaults.go:267, 280, 387, 395) | 315 |

## 5. StaticResponseConfig (`<NET>.staticResponses[]`) — common/config.go:2014-2032

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<NET>.staticResponses[].method` | `string` | required (validation.go:1282-1284) | 2015 |
| `<NET>.staticResponses[].params` | `[]interface{}` | — (nil) | 2016 |
| `<NET>.staticResponses[].response` | `*StaticResponseBodyConfig` | required (validation.go:1285-1287) | 2017 |
| `<NET>.staticResponses[].response.result` | `interface{}` | — (nil); exactly one of result/error must be set (validation.go:1288-1295) | 2023 |
| `<NET>.staticResponses[].response.error` | `*StaticResponseErrorConfig` | — (nil); exactly one of result/error | 2024 |
| `<NET>.staticResponses[].response.error.code` | `int` | — (0) | 2029 |
| `<NET>.staticResponses[].response.error.message` | `string` | required when error set (validation.go:1296-1298) | 2030 |
| `<NET>.staticResponses[].response.error.data` | `interface{}` | — (nil) | 2031 |

## 6. DirectiveDefaultsConfig (`<DD>`) — common/config.go:2105-2152

SetDefaults: defaults.go:1454-1471. Legacy `evm.integrity.*` values migrate into the first three enforce fields when those are unset (defaults.go:1952-1966).

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<DD>.retryEmpty` | `*bool` | — (nil) | 2106 |
| `<DD>.retryPending` | `*bool` | — (nil) | 2107 |
| `<DD>.skipCacheRead` | `interface{}` | — (nil); bool or string accepted, normalized to string at decode (config.go:2154-2175) | 2108 |
| `<DD>.useUpstream` | `*string` | — (nil) | 2109 |
| `<DD>.skipInterpolation` | `*bool` | — (nil) | 2110 |
| `<DD>.skipConsensus` | `*bool` | — (nil) | 2111 |
| `<DD>.enforceHighestBlock` | `*bool` | nil → `true` (defaults.go:1458-1460) | 2114 |
| `<DD>.enforceGetLogsBlockRange` | `*bool` | nil → `true` (defaults.go:1461-1463) | 2115 |
| `<DD>.enforceNonNullTaggedBlocks` | `*bool` | nil → `true` (defaults.go:1464-1466) | 2116 |
| `<DD>.validateTransactionsRoot` | `*bool` | nil → `true` (defaults.go:1467-1469) | 2120 |
| `<DD>.validateHeaderFieldLengths` | `*bool` | — (nil) | 2123 |
| `<DD>.validateTransactionFields` | `*bool` | — (nil) | 2126 |
| `<DD>.validateTransactionBlockInfo` | `*bool` | — (nil) | 2127 |
| `<DD>.enforceLogIndexStrictIncrements` | `*bool` | — (nil) | 2130 |
| `<DD>.validateTxHashUniqueness` | `*bool` | — (nil) | 2131 |
| `<DD>.validateTransactionIndex` | `*bool` | — (nil) | 2132 |
| `<DD>.validateLogFields` | `*bool` | — (nil) | 2133 |
| `<DD>.validateLogsBloomEmptiness` | `*bool` | — (nil) | 2137 |
| `<DD>.validateLogsBloomMatch` | `*bool` | — (nil) | 2139 |
| `<DD>.validateReceiptTransactionMatch` | `*bool` | — (nil) | 2142 |
| `<DD>.validateContractCreation` | `*bool` | — (nil) | 2143 |
| `<DD>.receiptsCountExact` | `*int64` | — (nil) | 2146 |
| `<DD>.receiptsCountAtLeast` | `*int64` | — (nil) | 2147 |
| `<DD>.validationExpectedBlockHash` | `*string` | — (nil) | 2150 |
| `<DD>.validationExpectedBlockNumber` | `*int64` | — (nil) | 2151 |

## 7. EvmNetworkConfig (`<ENET>`) — common/config.go:2177-2256

SetDefaults: defaults.go:2060-2115. When the network defines `evm` and networkDefaults also defines `evm`, field-by-field inheritance applies first (defaults.go:1845-1889).

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<ENET>.chainId` | `int64` | — (0); identifies the network (`NetworkId()`, config.go:2592-2603); no SetDefaults | 2178 |
| `<ENET>.fallbackFinalityDepth` | `int64` | 0 → `1024` (`DefaultEvmFinalityDepth` defaults.go:1975, applied 2061-2063); must be >0 (validation.go:1303-1305) | 2179 |
| `<ENET>.fallbackStatePollerDebounce` | `Duration` | 0 → `5s` (`DefaultEvmStatePollerDebounce` defaults.go:1976, applied 2064-2066); inherits networkDefaults first (defaults.go:1850-1852) | 2180 |
| `<ENET>.integrity` | `*EvmIntegrityConfig` | DEPRECATED; nil → created + all three enforce fields `true` (defaults.go:2084-2089, 2117-2128); migrated into directiveDefaults (defaults.go:1952-1966); see §17.4 | 2181 |
| `<ENET>.servedTip` | `*EvmServedTipConfig` | — (nil = max mode); inherits networkDefaults copy (defaults.go:1883-1886); see §8 | 2188 |
| `<ENET>.getLogsMaxAllowedRange` | `int64` | 0 → `30000` (defaults.go:2092-2094); must be >0 (validation.go:1309-1311) | 2189 |
| `<ENET>.getLogsMaxAllowedAddresses` | `int64` | — (0 = unlimited); inherits networkDefaults (defaults.go:1862-1864) | 2190 |
| `<ENET>.getLogsMaxAllowedTopics` | `int64` | — (0 = unlimited); inherits networkDefaults (defaults.go:1865-1867) | 2191 |
| `<ENET>.getLogsSplitOnError` | `*bool` | nil → `true` (defaults.go:2095-2097); inherits networkDefaults first (defaults.go:1868-1870) | 2192 |
| `<ENET>.getLogsSplitConcurrency` | `int` | 0 → `10` (defaults.go:2098-2100); inherits networkDefaults first (defaults.go:1874-1876) | 2193 |
| `<ENET>.traceFilterSplitOnError` | `*bool` | — (nil = feature off; intentional opt-in, defaults.go:2102-2104); inherits networkDefaults (defaults.go:1877-1879) | 2197 |
| `<ENET>.traceFilterSplitConcurrency` | `int` | 0 → `10` (defaults.go:2105-2107); inherits networkDefaults first (defaults.go:1880-1882) | 2200 |
| `<ENET>.enforceBlockAvailability` | `*bool` | — (nil = enforcement enabled; config.go:2201-2203); no SetDefaults | 2204 |
| `<ENET>.maxRetryableBlockDistance` | `*int64` | — (nil); effective `128` at runtime (erpc/networks.go:1975-1978) | 2211 |
| `<ENET>.markEmptyAsErrorMethods` | `[]string` | nil → `DefaultMarkEmptyAsErrorMethods()` = [eth_blockNumber, eth_getBlockByNumber, eth_getTransactionByHash, eth_getTransactionByBlockHashAndIndex, eth_getTransactionByBlockNumberAndIndex, eth_getUncleByBlockHashAndIndex, eth_getUncleByBlockNumberAndIndex, debug_traceTransaction, trace_transaction, trace_block, trace_get] (defaults.go:2044-2058, applied 2110-2112) | 2218 |
| `<ENET>.dynamicBlockTimeDebounceMultiplier` | `*float64` | nil → `0.7` (defaults.go:1977, applied 2067-2070); inherits networkDefaults first (defaults.go:1853-1855) | 2225 |
| `<ENET>.blockUnavailableDelayMultiplier` | `*float64` | nil → `1.0` (defaults.go:1978, applied 2071-2074); inherits networkDefaults first (defaults.go:1856-1858) | 2232 |
| `<ENET>.idempotentTransactionBroadcast` | `*bool` | — (nil = enabled; architecture/evm/eth_sendRawTransaction.go:30-36) | 2239 |
| `<ENET>.emptyResultConfidence` | `AvailbilityConfidence` | 0 → `blockHead` (=1) (defaults.go:2075-2079); values: `blockHead`(1) \| `finalizedBlock`(2) (common/architecture_evm.go:33-75); inherits networkDefaults first (defaults.go:1887-1889) | 2250 |
| `<ENET>.maxFutureBlockRetryDistance` | `*int64` | DEPRECATED yaml-only (`json:"-"`); warned + cleared in SetDefaults (defaults.go:2080-2083); see §17.4 | 2255 |

## 8. EvmServedTipConfig (`<ENET>.servedTip`) — common/config.go:2268-2286

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<ENET>.servedTip.enabledFor` | `[]string` | — (nil = max mode for all tags); valid entries `latest`/`finalized`/`safe` (validation.go:1312-1319; "safe" resolves to finalized axis, config.go:2292-2306) | 2272 |
| `<ENET>.servedTip.clusterDelta` | `int64` | — (0 = auto-derive from estimated block time, clamped [2,10]; config.go:2274-2277); must be ≥0 (validation.go:1320-1322) | 2277 |
| `<ENET>.servedTip.guaranteedMethods` | `[]string` | — (nil = only global cluster computed); glob patterns validated (validation.go:1323-1327) | 2285 |

## 9. Network alias config

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<NET>.alias` | `string` | — (""); see §3 row | 2002 |
| `server.aliasing.rules` | `[]*AliasingRuleConfig` | — (nil); auto `{matchDomain:"*", serveProject:"main"}` only when `projects` absent (defaults.go:102-110); (mount under `server` — server census owns the rest of ServerConfig) | 254 |
| `server.aliasing.rules[].matchDomain` | `string` | — ("") | 258 |
| `server.aliasing.rules[].serveProject` | `string` | — ("") | 259 |
| `server.aliasing.rules[].serveArchitecture` | `string` | — ("") | 260 |
| `server.aliasing.rules[].serveChain` | `string` | — ("") | 261 |

(Runtime-only alias resolver in common/network_alias.go has no yaml fields.)

## 10. UpstreamConfig (`<UPS>`) — common/config.go:702-748

`upstreamDefaults` inheritance into each upstream: ApplyDefaults (defaults.go:1491-1594; all-or-nothing for tags/failsafe/routing). SetDefaults: defaults.go:1596-1715.

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<UPS>.id` | `string` | "" → auto-derived: native-protocol endpoint host + counter, else scheme + counter, else vendorName + counter (defaults.go:1597-1615) | 703 |
| `<UPS>.type` | `UpstreamType` | "" → `"evm"` (defaults.go:1616-1619; const common/architecture_evm.go:11) | 704 |
| `<UPS>.tags` | `[]string` | — (nil); all-or-nothing inherit from upstreamDefaults (defaults.go:1505-1510); legacy `group`/`cohort` keys rewritten to `tier:<v>`/`cohort:<v>` tags (config.go:825-940, see §17.2) | 724 |
| `<UPS>.vendorName` | `string` | "" → inherits upstreamDefaults (defaults.go:1502-1504) | 726 |
| `<UPS>.endpoint` | `string` | required for upstreams (validation.go:818-819); inherits upstreamDefaults (defaults.go:1496-1498) | 727 |
| `<UPS>.evm` | `*EvmUpstreamConfig` | nil → auto-created when type starts with `evm` (defaults.go:1683-1687); copied/merged from upstreamDefaults.evm (defaults.go:1522-1549); see §11.4 | 728 |
| `<UPS>.jsonRpc` | `*JsonRpcUpstreamConfig` | nil → auto-created empty (defaults.go:1700-1705); copied from upstreamDefaults.jsonRpc when absent (defaults.go:1550-1559); see §11.2 | 729 |
| `<UPS>.grpc` | `*GrpcUpstreamConfig` | — (nil); copied from upstreamDefaults.grpc (defaults.go:1560-1562); see §11.3 | 730 |
| `<UPS>.ignoreMethods` | `[]string` | — (nil); → `["*"]` when `allowMethods` set and ignoreMethods nil (defaults.go:1706-1712); inherits upstreamDefaults (defaults.go:1572-1574) | 731 |
| `<UPS>.allowMethods` | `[]string` | — (nil); inherits upstreamDefaults (defaults.go:1569-1571) | 732 |
| `<UPS>.autoIgnoreUnsupportedMethods` | `*bool` | — (nil = off at runtime, upstream/upstream.go:1308-1312); inherits upstreamDefaults (defaults.go:1575-1577); repository vendor sets `true` when unset (thirdparty/repository.go:89-92) | 733 |
| `<UPS>.failsafe[]` | `[]*FailsafeConfig` | — (nil); deep-copies upstreamDefaults.failsafe when absent, else per-entry merge matched by matchMethod/matchFinality (defaults.go:1621-1672); legacy single-object form accepted (config.go:851-898); see §12 | 734 |
| `<UPS>.rateLimitBudget` | `string` | "" → inherits upstreamDefaults (defaults.go:1514-1516); must reference existing budget (validation.go:843-847) | 735 |
| `<UPS>.rateLimitAutoTune` | `*RateLimitAutoTuneConfig` | nil → auto-created when `rateLimitBudget` set (defaults.go:1674-1681); inherits upstreamDefaults object (defaults.go:1517-1519); see §11.1 | 736 |
| `<UPS>.shadow` | `*ShadowUpstreamConfig` | — (nil); see §11.5 | 737 |
| `<UPS>.routing` | `*UpstreamRoutingConfig` | — (nil); all-or-nothing clone from upstreamDefaults.routing (defaults.go:1578-1591); see §11.6 | 747 |

### 11.1 RateLimitAutoTuneConfig (`<UPS>.rateLimitAutoTune`) — common/config.go:1051-1059; SetDefaults defaults.go:2489-2510

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<UPS>.rateLimitAutoTune.enabled` | `*bool` | nil → `true` (defaults.go:2490-2492) | 1052 |
| `<UPS>.rateLimitAutoTune.adjustmentPeriod` | `Duration` | 0 → `1m` (defaults.go:2493-2495) | 1053 |
| `<UPS>.rateLimitAutoTune.errorRateThreshold` | `float64` | 0 → `0.1` (defaults.go:2496-2498) | 1054 |
| `<UPS>.rateLimitAutoTune.increaseFactor` | `float64` | 0 → `1.05` (defaults.go:2499-2501) | 1055 |
| `<UPS>.rateLimitAutoTune.decreaseFactor` | `float64` | 0 → `0.95` (defaults.go:2502-2504) | 1056 |
| `<UPS>.rateLimitAutoTune.minBudget` | `int` | — (0; no SetDefaults) | 1057 |
| `<UPS>.rateLimitAutoTune.maxBudget` | `int` | 0 → `100000` (defaults.go:2505-2507) | 1058 |

### 11.2 JsonRpcUpstreamConfig (`<UPS>.jsonRpc`) — common/config.go:1072-1079; SetDefaults is a no-op (defaults.go:1777-1779)

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<UPS>.jsonRpc.supportsBatch` | `*bool` | — (nil = batching disabled, clients/http_json_rpc_client.go:132) | 1073 |
| `<UPS>.jsonRpc.batchMaxSize` | `int` | — (0); required >0 when supportsBatch=true (validation.go:1184-1191) | 1074 |
| `<UPS>.jsonRpc.batchMaxWait` | `Duration` | — (0); required >0 when supportsBatch=true (validation.go:1184-1191) | 1075 |
| `<UPS>.jsonRpc.enableGzip` | `*bool` | — (nil = gzip off for upstream requests; clients/http_json_rpc_client.go:42, 139-141) | 1076 |
| `<UPS>.jsonRpc.headers` | `map[string]string` | — (nil) | 1077 |
| `<UPS>.jsonRpc.proxyPool` | `string` | — (""); must reference existing `proxyPools[].id` when set (validation.go) | 1078 |

### 11.3 GrpcUpstreamConfig (`<UPS>.grpc`) — common/config.go:1100-1102

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<UPS>.grpc.headers` | `map[string]string` | — (nil); sent as gRPC metadata on every outbound request (config.go:1096-1099) | 1101 |

### 11.4 EvmUpstreamConfig (`<UPS>.evm`) — common/config.go:1120-1154; SetDefaults defaults.go:1717-1775

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<UPS>.evm.chainId` | `int64` | — (0 = auto-detect via `eth_chainId`); MUST be 0 for non-http native-protocol endpoints (validation.go:851-856); inherited from upstreamDefaults.evm (defaults.go:1523-1524) | 1121 |
| `<UPS>.evm.statePollerInterval` | `Duration` | 0 → `30s` (defaults.go:1718-1724); required >0 after defaults (validation.go:858-860) | 1122 |
| `<UPS>.evm.statePollerDebounce` | `Duration` | — (0 = dynamic: inferred from observed block time → network `fallbackStatePollerDebounce` → 1s floor; config.go:1123-1127); inherits upstreamDefaults (defaults.go:1725-1727) | 1127 |
| `<UPS>.evm.blockAvailability` | `*EvmBlockAvailabilityConfig` | — (nil = off); back-compat synth: `lower.latestBlockMinus = maxAvailableRecentBlocks` when that legacy field >0 (defaults.go:1749-1756); `lower.latestBlockMinus=128` when nodeType=full and nothing else set (defaults.go:1758-1764); see §11.7 | 1128 |
| `<UPS>.evm.getLogsAutoSplittingRangeThreshold` | `int64` | — (0 = proactive splitting disabled; effective threshold = min across upstreams with >0, architecture/evm/eth_getLogs.go:219-235); auto-default project's upstreamDefaults uses 5000 (defaults.go:146); inherits upstreamDefaults (defaults.go:1543-1545) | 1129 |
| `<UPS>.evm.traceFilterAutoSplittingRangeThreshold` | `int64` | — (0 = disabled; config.go:1130-1133); inherits upstreamDefaults (defaults.go:1546-1548) | 1134 |
| `<UPS>.evm.skipWhenSyncing` | `*bool` | nil → `false` (defaults.go:1766-1772) | 1135 |
| `<UPS>.evm.integrity` | `*UpstreamIntegrityConfig` | — (nil); copied from upstreamDefaults.evm.integrity (defaults.go:1564-1568); see §11.8 | 1136 |
| `<UPS>.evm.nodeType` | `EvmNodeType` | DEPRECATED; "" → `"unknown"` (defaults.go:1728-1734); valid: unknown/full/archive (validation.go:861-871; consts common/architecture_evm.go:77-83); see §17.3 | 1139 |
| `<UPS>.evm.maxAvailableRecentBlocks` | `int64` | DEPRECATED; 0 → `128` only when nodeType=full (defaults.go:1736-1747); mapped into blockAvailability.lower (defaults.go:1749-1756); see §17.3 | 1141 |
| `<UPS>.evm.getLogsMaxAllowedRange` | `int64` | DEPRECATED yaml-only (`json:"-"`); — (parsed, not used at runtime); see §17.3 | 1143 |
| `<UPS>.evm.getLogsMaxAllowedAddresses` | `int64` | DEPRECATED yaml-only; — | 1145 |
| `<UPS>.evm.getLogsMaxAllowedTopics` | `int64` | DEPRECATED yaml-only; — | 1147 |
| `<UPS>.evm.getLogsSplitOnError` | `*bool` | DEPRECATED yaml-only; — (nil) | 1149 |
| `<UPS>.evm.getLogsMaxBlockRange` | `int64` | DEPRECATED yaml-only; — | 1151 |
| `<UPS>.evm.queryShim` | `*EvmQueryShimConfig` | — (nil); see §11.9 | 1153 |

### 11.5 ShadowUpstreamConfig (`<UPS>.shadow`) — common/config.go:982-986

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<UPS>.shadow.enabled` | `bool` | — (false) | 983 |
| `<UPS>.shadow.sampleRate` | `*float64` | — (nil = `1.0` at runtime, erpc/shadow.go:63-67) | 984 |
| `<UPS>.shadow.ignoreFields` | `map[string][]string` | — (nil) | 985 |

### 11.6 UpstreamRoutingConfig + ScoreMultiplierConfig (`<UPS>.routing`) — common/config.go:754-812

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<UPS>.routing.scoreMultipliers[]` | `[]*ScoreMultiplierConfig` | — (nil) | 760 |
| `<UPS>.routing.scoreLatencyQuantile` | `float64` | — (0 = policy `sortByScore` default p70 applies; internal/policy/stdlib/stdlib.js:612) | 764 |
| `<UPS>.routing.probe` | `ProbeMode` | — ("" = `"on"`; only explicit `"off"` disables, internal/policy/prober.go:253; consts config.go:779-788) | 773 |
| `<UPS>.routing.scoreMultipliers[].network` | `string` | — ("" = matches any) | 798 |
| `<UPS>.routing.scoreMultipliers[].method` | `string` | — ("" = matches any) | 799 |
| `<UPS>.routing.scoreMultipliers[].finality` | `[]DataFinalityState` | — (nil = any); values finalized(0)/unfinalized(1)/realtime(2)/unknown(3) (common/data.go:8-27) | 800 |
| `<UPS>.routing.scoreMultipliers[].overall` | `*float64` | — (nil = inherit policy base weight; 0 removes contribution — config.go:790-796) | 801 |
| `<UPS>.routing.scoreMultipliers[].errorRate` | `*float64` | — (nil = inherit) | 802 |
| `<UPS>.routing.scoreMultipliers[].respLatency` | `*float64` | — (nil = inherit) | 803 |
| `<UPS>.routing.scoreMultipliers[].throttledRate` | `*float64` | — (nil = inherit) | 804 |
| `<UPS>.routing.scoreMultipliers[].blockHeadLag` | `*float64` | — (nil = inherit) | 805 |
| `<UPS>.routing.scoreMultipliers[].finalizationLag` | `*float64` | — (nil = inherit) | 806 |
| `<UPS>.routing.scoreMultipliers[].misbehaviors` | `*float64` | — (nil = inherit) | 807 |
| `<UPS>.routing.scoreMultipliers[].totalRequests` | `*float64` | DEPRECATED no-op (accepted, never read; config.go:808-811); see §17.5 | 811 |

### 11.7 EvmBlockAvailabilityConfig + EvmAvailabilityBoundConfig (`<UPS>.evm.blockAvailability`) — common/config.go:1188-1226

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<UPS>.evm.blockAvailability.lower` | `*EvmAvailabilityBoundConfig` | — (nil) | 1189 |
| `<UPS>.evm.blockAvailability.upper` | `*EvmAvailabilityBoundConfig` | — (nil) | 1190 |
| `...blockAvailability.<lower\|upper>.exactBlock` | `*int64` | — (nil); exactly one of exactBlock/latestBlockMinus/earliestBlockPlus per bound (config.go:1207-1208; validated common/validation.go BlockAvailability.Validate) | 1221 |
| `...blockAvailability.<lower\|upper>.latestBlockMinus` | `*int64` | — (nil) | 1222 |
| `...blockAvailability.<lower\|upper>.earliestBlockPlus` | `*int64` | — (nil) | 1223 |
| `...blockAvailability.<lower\|upper>.probe` | `EvmAvailabilityProbeType` | — (""); values blockHeader/eventLogs/callState/traceData (config.go:1213-1218) | 1224 |
| `...blockAvailability.<lower\|upper>.updateRate` | `Duration` | — (0 = freeze at first evaluation for earliestBlockPlus; ignored for latestBlockMinus — config.go:1207-1210) | 1225 |

### 11.8 UpstreamIntegrityConfig (`<UPS>.evm.integrity`) — common/config.go:988-1010

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<UPS>.evm.integrity.eth_getBlockReceipts` | `*UpstreamIntegrityEthGetBlockReceiptsConfig` | — (nil) | 989 |
| `<UPS>.evm.integrity.eth_getBlockReceipts.enabled` | `bool` | — (false) | 1007 |
| `<UPS>.evm.integrity.eth_getBlockReceipts.checkLogIndexStrictIncrements` | `*bool` | — (nil) | 1008 |
| `<UPS>.evm.integrity.eth_getBlockReceipts.checkLogsBloom` | `*bool` | — (nil) | 1009 |

### 11.9 EvmQueryShimConfig (`<UPS>.evm.queryShim`) — common/config.go:1156-1163; no SetDefaults — runtime fallbacks in architecture/evm/eth_query_helpers.go:21-24, 361-384

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<UPS>.evm.queryShim.enabled` | `*bool` | — (nil = disabled; architecture/evm/eth_query.go:79-81) | 1157 |
| `<UPS>.evm.queryShim.allowedMethods` | `[]string` | — (nil = all methods allowed when enabled; architecture/evm/eth_query.go:95-110) | 1158 |
| `<UPS>.evm.queryShim.concurrency` | `int` | — (0 → runtime `10`, eth_query_helpers.go:21) | 1159 |
| `<UPS>.evm.queryShim.maxBlockRange` | `int64` | — (0 → runtime `10000`, eth_query_helpers.go:22) | 1160 |
| `<UPS>.evm.queryShim.maxLimit` | `int` | — (0 → runtime `10000`, eth_query_helpers.go:23) | 1161 |
| `<UPS>.evm.queryShim.defaultLimit` | `int` | — (0 → runtime `100`, eth_query_helpers.go:24) | 1162 |

## 12. FailsafeConfig (`<FS>`) — common/config.go:1279-1287; SetDefaults defaults.go:2130-2211

Scope aliases (same struct): `NetworkFailsafeConfig` (no circuitBreaker at network scope), `UpstreamFailsafeConfig` (no consensus at upstream scope), `CacheFailsafeConfig` (no hedge quantile) — config.go:1289-1303; enforced by validation.

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<FS>.matchMethod` | `string` | "" → `"*"` (defaults.go:2131-2138) | 1280 |
| `<FS>.matchFinality` | `[]DataFinalityState` | — (nil = any finality; defaults.go:17-34) | 1281 |
| `<FS>.retry` | `*RetryPolicyConfig` | — (nil; inherited from matching defaults entry when present, defaults.go:2156-2171); see §12.1 | 1282 |
| `<FS>.circuitBreaker` | `*CircuitBreakerPolicyConfig` | — (nil; inherited as above, defaults.go:2188-2203); see §12.2 | 1283 |
| `<FS>.timeout` | `*TimeoutPolicyConfig` | — (nil; inherited, defaults.go:2140-2155); see §12.3 | 1284 |
| `<FS>.hedge` | `*HedgePolicyConfig` | — (nil; inherited, defaults.go:2172-2187); see §12.4 | 1285 |
| `<FS>.consensus` | `*ConsensusPolicyConfig` | — (nil); see §12.5 | 1286 |

### 12.1 RetryPolicyConfig (`<FS>.retry`) — common/config.go:1342-1370; SetDefaults defaults.go:2225-2305

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<FS>.retry.maxAttempts` | `int` | 0 → `3` (defaults.go:2226-2232) | 1343 |
| `<FS>.retry.delay` | `Duration` | 0 → `0ms` (defaults.go:2247-2253) | 1344 |
| `<FS>.retry.backoffMaxDelay` | `Duration` | 0 → `3s` (defaults.go:2240-2246) | 1345 |
| `<FS>.retry.backoffFactor` | `float32` | 0 → `1.2` (defaults.go:2233-2239) | 1346 |
| `<FS>.retry.jitter` | `Duration` | 0 → `0ms` (defaults.go:2254-2260) | 1347 |
| `<FS>.retry.emptyResultAccept` | `[]string` | nil → `DefaultEmptyResultAccept()` = [eth_getLogs, trace_filter, arbtrace_filter, eth_call, eth_getBalance, eth_getCode, eth_getStorageAt, eth_getTransactionCount] (defaults.go:2013-2026, applied 2276-2285) | 1350 |
| `<FS>.retry.emptyResultIgnore` | `[]string` | DEPRECATED; migrated into emptyResultAccept when that is nil (defaults.go:2261-2264, 2276-2282); see §17.5 | 1352 |
| `<FS>.retry.emptyResultMaxAttempts` | `int` | 0 → `2` (`DefaultEmptyResultMaxAttempts` defaults.go:1980-1984, applied 2287-2296) | 1354 |
| `<FS>.retry.emptyResultDelay` | `Duration` | — (0; inherits only from defaults entry, no hardcoded fallback — defaults.go:2298-2302; dynamic block-time delay preferred at runtime, config.go:1354-1361) | 1361 |
| `<FS>.retry.blockUnavailableDelay` | `Duration` | DEPRECATED yaml-only (`json:"-"`); migrated into emptyResultDelay + warning, then cleared (defaults.go:2265-2274); see §17.5 | 1369 |

### 12.2 CircuitBreakerPolicyConfig (`<FS>.circuitBreaker`) — common/config.go:1381-1387; SetDefaults defaults.go:2342-2387

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<FS>.circuitBreaker.failureThresholdCount` | `uint` | 0 → `20` (defaults.go:2343-2349) | 1382 |
| `<FS>.circuitBreaker.failureThresholdCapacity` | `uint` | 0 → `80` (defaults.go:2350-2356) | 1383 |
| `<FS>.circuitBreaker.halfOpenAfter` | `Duration` | 0 → `5m` (defaults.go:2364-2370) | 1384 |
| `<FS>.circuitBreaker.successThresholdCount` | `uint` | 0 → `8` (defaults.go:2371-2377) | 1385 |
| `<FS>.circuitBreaker.successThresholdCapacity` | `uint` | 0 → `200` (first block defaults.go:2357-2363; the duplicate later block 2378-2384 with value 10 is unreachable because 200 is already set) | 1386 |

### 12.3 TimeoutPolicyConfig (`<FS>.timeout`) — common/config.go:1406-1408; SetDefaults defaults.go:2213-2223 (inherit-only, no hardcoded value)

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<FS>.timeout.duration` | `*AdaptiveDuration` (scalar or `{base,quantile,min,max}`) | — (nil; inherits matching defaults entry via inheritFrom, defaults.go:2213-2223) | 1407 |
| `<FS>.timeout.quantile` | `float64` (legacy flat sibling) | DEPRECATED-shape; folded into `duration.quantile` at decode (config.go:1420-1434, 1464-1480); see §17.5 | 1423 (legacy shadow) |
| `<FS>.timeout.minDuration` | `Duration` (legacy flat sibling) | folded into `duration.min` (config.go:1424, 1474-1476) | 1424 (legacy shadow) |
| `<FS>.timeout.maxDuration` | `Duration` (legacy flat sibling) | folded into `duration.max` (config.go:1425, 1477-1479) | 1425 (legacy shadow) |

### 12.4 HedgePolicyConfig (`<FS>.hedge`) — common/config.go:1489-1492; SetDefaults defaults.go:2310-2340

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<FS>.hedge.delay` | `*AdaptiveDuration` | nil → created; after inherit: `min` floors at `100ms`, `max` ceilings at `999s` (consts defaults.go:2310-2313, applied 2316-2329) | 1490 |
| `<FS>.hedge.maxCount` | `int` | 0 → `1` (defaults.go:2331-2337) | 1491 |
| `<FS>.hedge.quantile` | `float64` (legacy flat sibling) | folded into `delay.quantile` at decode (config.go:1507-1523, 1555-1571); see §17.5 | 1511 (legacy shadow) |
| `<FS>.hedge.minDelay` | `Duration` (legacy flat sibling) | folded into `delay.min` (config.go:1512, 1565-1567) | 1512 (legacy shadow) |
| `<FS>.hedge.maxDelay` | `Duration` (legacy flat sibling) | folded into `delay.max` (config.go:1513, 1568-1570) | 1513 (legacy shadow) |

### 12.5 AdaptiveDuration object form (used by `<FS>.timeout.duration`, `<FS>.hedge.delay`, `<FS>.consensus.maxWaitOnResult`, `<FS>.consensus.maxWaitOnEmpty`) — common/adaptive_duration.go:61-64

| yaml path | Go type | exact default | line (adaptive_duration.go) |
|---|---|---|---|
| `....base` | `Duration` | — (0) | 61 |
| `....quantile` | `float64` | — (0) | 62 |
| `....min` | `Duration` | — (0; hedge floors 100ms, defaults.go:2324-2326) | 63 |
| `....max` | `Duration` | — (0; hedge ceilings 999s, defaults.go:2327-2329) | 64 |

### 12.6 ConsensusPolicyConfig (`<FS>.consensus`) — common/config.go:1591-1649; SetDefaults defaults.go:2389-2455

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<FS>.consensus.maxParticipants` | `int` | 0 → `5` (defaults.go:2390-2392); must be ≥ agreementThreshold (validation.go:1069-1071) | 1592 |
| `<FS>.consensus.agreementThreshold` | `int` | 0 → `2` (defaults.go:2393-2395); must be >0 (validation.go:1066-1068) | 1593 |
| `<FS>.consensus.disputeBehavior` | `ConsensusDisputeBehavior` | "" → `returnError` (defaults.go:2396-2398); values: returnError/acceptMostCommonValidResult/preferBlockHeadLeader/onlyBlockHeadLeader (config.go:1582-1589) | 1594 |
| `<FS>.consensus.lowParticipantsBehavior` | `ConsensusLowParticipantsBehavior` | "" → `acceptMostCommonValidResult` (defaults.go:2399-2401); same value set (config.go:1573-1580) | 1595 |
| `<FS>.consensus.punishMisbehavior` | `*PunishMisbehaviorConfig` | — (nil); see §12.7 | 1596 |
| `<FS>.consensus.disputeLogLevel` | `string` | "" → `"warn"` (defaults.go:2402-2404); values trace/debug/info/warn/error (config.go:1597) | 1597 |
| `<FS>.consensus.ignoreFields` | `map[string][]string` | nil → `{eth_getLogs: ["*.blockTimestamp"], eth_getTransactionReceipt: ["blockTimestamp","logs.*.blockTimestamp"], eth_getBlockReceipts: ["*.blockTimestamp","*.logs.*.blockTimestamp"]}` (defaults.go:2405-2419) | 1598 |
| `<FS>.consensus.preferNonEmpty` | `*bool` | nil → `true` (defaults.go:2420-2422) | 1599 |
| `<FS>.consensus.preferLargerResponses` | `*bool` | nil → `true` (defaults.go:2423-2425) | 1600 |
| `<FS>.consensus.misbehaviorsDestination` | `*MisbehaviorsDestinationConfig` | — (nil); see §12.8 | 1601 |
| `<FS>.consensus.preferHighestValueFor` | `map[string][]string` | — (nil) | 1607 |
| `<FS>.consensus.fireAndForget` | `bool` | — (false; config.go:1608-1613) | 1613 |
| `<FS>.consensus.maxWaitOnResult` | `*AdaptiveDuration` | nil → `{quantile: 0.5, min: 5ms, max: 1s}` (defaults.go:2432-2438) | 1625 |
| `<FS>.consensus.maxWaitOnEmpty` | `*AdaptiveDuration` | nil → `{quantile: 0.9, min: 50ms, max: 2s}` (defaults.go:2439-2445) | 1633 |
| `<FS>.consensus.requiredParticipants[]` | `[]*ConsensusRequiredParticipant` | — (nil = disabled; config.go:1635-1648) | 1648 |
| `<FS>.consensus.requiredParticipants[].tag` | `string` | required non-empty (validation.go:1285-1291 area: requiredParticipants loop) | 1657 |
| `<FS>.consensus.requiredParticipants[].minParticipants` | `int` | — (0; must be meaningful per validation loop) | 1658 |

### 12.7 PunishMisbehaviorConfig (`<FS>.consensus.punishMisbehavior`) — common/config.go:1785-1789; no SetDefaults — all required when block present (validation.go:1171-1182)

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<FS>.consensus.punishMisbehavior.disputeThreshold` | `uint` | — (0); required >0 (validation.go:1172-1174) | 1786 |
| `<FS>.consensus.punishMisbehavior.disputeWindow` | `Duration` | — (0); required >0 (validation.go:1175-1177); ≤0 disables punishment at runtime (consensus/executor.go:1189-1192) | 1787 |
| `<FS>.consensus.punishMisbehavior.sitOutPenalty` | `Duration` | — (0); required >0 (validation.go:1178-1180) | 1788 |

### 12.8 MisbehaviorsDestinationConfig + S3FlushConfig + AwsAuthConfig (`<FS>.consensus.misbehaviorsDestination`) — common/config.go:1716-1757, 479-485; SetDefaults defaults.go:2458-2487

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<FS>.consensus.misbehaviorsDestination.type` | `MisbehaviorsDestinationType` | "" → `"file"` (defaults.go:2460-2462); values file/s3 (config.go:1711-1714) | 1718 |
| `<FS>.consensus.misbehaviorsDestination.path` | `string` | — (""; file path or s3:// URI) | 1721 |
| `<FS>.consensus.misbehaviorsDestination.filePattern` | `string` | "" → `"{timestampMs}-{method}-{networkId}"` (defaults.go:2464-2466) | 1729 |
| `<FS>.consensus.misbehaviorsDestination.s3` | `*S3FlushConfig` | nil → auto-created when type=s3 (defaults.go:2468-2471) | 1732 |
| `....s3.maxRecords` | `int` | 0 → `100` (defaults.go:2472-2474) | 1737 |
| `....s3.maxSize` | `int64` | 0 → `1048576` (1MB; defaults.go:2475-2477) | 1740 |
| `....s3.flushInterval` | `Duration` | 0 → `60s` (defaults.go:2478-2480) | 1743 |
| `....s3.region` | `string` | — ("" → AWS env/IMDS; defaults.go:2484) | 1746 |
| `....s3.credentials` | `*AwsAuthConfig` | — (nil = standard AWS credential chain; config.go:1748-1753) | 1753 |
| `....s3.contentType` | `string` | "" → `"application/jsonl"` (defaults.go:2481-2483) | 1756 |
| `....s3.credentials.mode` | `string` | — (""); values file/env/secret (config.go:480) | 480 |
| `....s3.credentials.credentialsFile` | `string` | — ("") | 481 |
| `....s3.credentials.profile` | `string` | — ("") | 482 |
| `....s3.credentials.accessKeyID` | `string` | — ("") | 483 |
| `....s3.credentials.secretAccessKey` | `string` | — (""; redacted in marshal, config.go:487-505) | 484 |

## 13. ProviderConfig (`projects[].providers[]`) — common/config.go:669-677; SetDefaults defaults.go:1473-1489

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `P.providers[].id` | `string` | "" → defaults to vendor name (defaults.go:1474-1476); required after defaults (validation.go:782) | 670 |
| `P.providers[].vendor` | `string` | required; registered vendors: alchemy, blastapi, conduit, drpc, dwellir, envio, etherspot, infura, pimlico, quicknode, llama, thirdweb, repository, superchain, tenderly, chainstack, onfinality, erpc, blockpi, ankr, routemesh, blockdaemon (thirdparty/vendors_registry.go:9-35) | 671 |
| `P.providers[].settings` | `VendorSettings` (`map[string]interface{}`, config.go:667) | — (nil); per-vendor keys in §14 | 672 |
| `P.providers[].onlyNetworks` | `[]string` | — (nil) | 673 |
| `P.providers[].ignoreNetworks` | `[]string` | — (nil) | 674 |
| `P.providers[].upstreamIdTemplate` | `string` | "" → `"<PROVIDER>-<NETWORK>"` (defaults.go:1477-1479) | 675 |
| `P.providers[].overrides` | `map[string]*UpstreamConfig` | — (nil); each value is a full UpstreamConfig (§10); SetDefaults applied per override (defaults.go:1480-1486); shorthand-upstream conversion synthesizes `overrides["*"]` (defaults.go:1227-1235) | 676 |

## 14. Vendor settings keys (`P.providers[].settings.*`) — dynamic map; keys read by each vendor in thirdparty/

These are NOT Go struct yaml tags — `VendorSettings` is an opaque map (config.go:667). Keys below are exactly what each vendor reads. `recheckInterval` values are type-asserted to `time.Duration` (only settable via TS config / programmatic use; a YAML duration string fails the assert and the default applies).

| yaml path | Go type (asserted) | exact default | source line |
|---|---|---|---|
| `settings.apiKey` (alchemy) | `string` | required (or endpoint-derived; defaults.go:1245-1248) | thirdparty/alchemy.go:221 |
| `settings.chainsUrl` (alchemy) | `string` | `"https://app-api.alchemy.com/trpc/config.getNetworkConfig"` | thirdparty/alchemy.go:158, 196-198 |
| `settings.recheckInterval` (alchemy) | `time.Duration` | `24h` | thirdparty/alchemy.go:154, 205-207 |
| `settings.apiKey` (ankr) | `string` | optional (appended to URL) | thirdparty/ankr.go:96 |
| `settings.apiKey` (blastapi) | `string` | optional | thirdparty/blastapi.go:128 |
| `settings.apiKey` (blockdaemon) | `string` | required | thirdparty/blockdaemon.go:85 |
| `settings.apiKey` (blockpi) | `string` | optional | thirdparty/blockpi.go:105 |
| `settings.apiKey` (chainstack) | `string` | required | thirdparty/chainstack.go:83 |
| `settings.recheckInterval` (chainstack) | `time.Duration` | `1h` | thirdparty/chainstack.go:59, 88-91 |
| `settings.project` (chainstack) | `string` | — (filter) | thirdparty/chainstack.go:192 |
| `settings.organization` (chainstack) | `string` | — (filter) | thirdparty/chainstack.go:195 |
| `settings.region` (chainstack) | `string` | — (filter) | thirdparty/chainstack.go:198 |
| `settings.provider` (chainstack) | `string` | — (filter) | thirdparty/chainstack.go:201 |
| `settings.type` (chainstack) | `string` | — (filter) | thirdparty/chainstack.go:204 |
| `settings.apiKey` (conduit) | `string` | required | thirdparty/conduit.go:121 |
| `settings.networksUrl` (conduit) | `string` | `"https://api.conduit.xyz/public/network/all"` | thirdparty/conduit.go:16, 58-61 |
| `settings.recheckInterval` (conduit) | `time.Duration` | `24h` | thirdparty/conduit.go:17, 63-66 |
| `settings.apiKey` (drpc) | `string` | required | thirdparty/drpc.go:258 |
| `settings.chainsUrl` (drpc) | `string` | `"https://lb.drpc.org/networks"` | thirdparty/drpc.go:173, 217-220 |
| `settings.recheckInterval` (drpc) | `time.Duration` | `24h` | thirdparty/drpc.go:175, 226-229 |
| `settings.apiKey` (dwellir) | `string` | required | thirdparty/dwellir.go:130 |
| `settings.rootDomain` (envio) | `string` | `"rpc.hypersync.xyz"` | thirdparty/envio.go:20, 115-118 |
| `settings.apiKey` (envio) | `string` | optional | thirdparty/envio.go:120 |
| `settings.endpoint` (erpc) | `string` | required | thirdparty/erpc.go:127 |
| `settings.secret` (erpc) | `string` | optional | thirdparty/erpc.go:132 |
| `settings.apiKey` (etherspot) | `string` | optional | thirdparty/etherspot.go:96 |
| `settings.apiKey` (infura) | `string` | optional | thirdparty/infura.go:80 |
| `settings.apiKey` (llama) | `string` | optional | thirdparty/llama.go:53 |
| `settings.apiKey` (onfinality) | `string` | optional | thirdparty/onfinality.go:74 |
| `settings.apiKey` (pimlico) | `string` | required ("public" allowed) | thirdparty/pimlico.go:120, 175 |
| `settings.apiKey` (quicknode) | `string` | required | thirdparty/quicknode.go:109 |
| `settings.recheckInterval` (quicknode) | `time.Duration` | `1h` | thirdparty/quicknode.go:44, 114-116 |
| `settings.tagIds` (quicknode) | `int` \| `[]int` | — (endpoint filter; also parsed from shorthand URL query, defaults.go:1306-1334) | thirdparty/quicknode.go:60 |
| `settings.tagLabels` (quicknode) | `string` \| `[]string` | — (endpoint filter; shorthand URL query, defaults.go:1336-1348) | thirdparty/quicknode.go:76 |
| `settings.repositoryUrl` (repository) | `string` | `"https://evm-public-endpoints.erpc.cloud"` | thirdparty/repository.go:17, 51-54 |
| `settings.recheckInterval` (repository) | `time.Duration` | `1h` | thirdparty/repository.go:18, 56-59 |
| `settings.baseURL` (routemesh) | `string` | `"lb.routemes.sh"` | thirdparty/routemesh.go:20, 47-50 |
| `settings.apiKey` (routemesh) | `string` | required | thirdparty/routemesh.go:52 |
| `settings.registryUrl` (superchain) | `string` | `"https://raw.githubusercontent.com/ethereum-optimism/superchain-registry/main/chainList.json"` | thirdparty/superchain.go:16, 122-125 |
| `settings.recheckInterval` (superchain) | `time.Duration` | `24h` | thirdparty/superchain.go:17, 132-135 |
| `settings.apiKey` (tenderly) | `string` | required | thirdparty/tenderly.go:92 |
| `settings.recheckInterval` (tenderly) | `time.Duration` | `24h` | thirdparty/tenderly.go:34, 56-59 |
| `settings.clientId` (thirdweb) | `string` | required | thirdparty/thirdweb.go:45, 99 |

Shorthand endpoint → settings synthesis (e.g. `endpoint: alchemy://KEY` becomes `settings.apiKey`): buildProviderSettings, defaults.go:1243-1425.

## 15. SelectionPolicyConfig (`<SP>`) — common/config.go:2359-2401; SetDefaults defaults.go:2517-2609

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<SP>.evalInterval` | `Duration` | 0 → `15s` (defaults.go:2518-2533); must be >0, and evalTimeout < evalInterval (validation.go:1339-1344) | 2360 |
| `<SP>.evalScope` | `EvalScope` | "" → `"network"` (or translated from alias bools; defaults.go:2556-2568); values: network / network-method / network-finality / network-method-finality (config.go:2333-2350); invalid → error (defaults.go:2573-2583) | 2365 |
| `<SP>.evalPerMethod` | `*bool` | DEPRECATED alias of evalScope; translated then niled out (defaults.go:2551-2585); see §17.5 | 2372 |
| `<SP>.evalPerFinality` | `*bool` | DEPRECATED alias of evalScope; translated then niled out (defaults.go:2551-2585); see §17.5 | 2375 |
| `<SP>.evalTimeout` | `Duration` | 0 → `100ms` (defaults.go:2534-2536) | 2376 |
| `<SP>.evalFunc` | `string` (JS source; TS configs carry `__ts_fn__:<id>` sentinel) | "" → `"(upstreams, ctx) => upstreams"` (`DefaultSelectionPolicySource` defaults.go:2512-2515, applied 2586-2588); compiled at SetDefaults (defaults.go:2602-2607) | 2382 |

Non-yaml runtime fields (excluded): `DisableTickerForTest`, `CompiledProgram`, `EvalFuncOriginal`, `LegacySelectionPolicy` (config.go:2389-2400, all `yaml:"-"`).

## 16. healthCheck + cors + auth trees

### 16.1 HealthCheckConfig (`healthCheck`) — common/config.go:186-190 (mount: config.go:42); SetDefaults defaults.go:738-747

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `healthCheck.mode` | `HealthCheckMode` | "" → `"networks"` (defaults.go:739-741); values simple/networks/verbose (config.go:194-198) | 187 |
| `healthCheck.auth` | `*AuthConfig` | — (nil); see §16.3 | 188 |
| `healthCheck.defaultEval` | `string` | "" → `"any:initializedUpstreams"` (defaults.go:742-744); known evals: any:initializedUpstreams, any:errorRateBelow90, all:errorRateBelow90, any:errorRateBelow100, all:errorRateBelow100, any:evm:eth_chainId, all:evm:eth_chainId, all:activeUpstreams (config.go:200-209) | 189 |

### 16.2 CORSConfig (`<CORS>`) — common/config.go:658-665; SetDefaults defaults.go:2824-2846

`admin.cors` (server census) gets a pre-default of `{allowedOrigins: ["*"], allowCredentials: false}` when absent (defaults.go:775-784) before the same SetDefaults runs.

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<CORS>.allowedOrigins` | `[]string` | nil → `["*"]` (defaults.go:2825-2827) | 659 |
| `<CORS>.allowedMethods` | `[]string` | nil → `["GET","POST","OPTIONS"]` (defaults.go:2828-2830) | 660 |
| `<CORS>.allowedHeaders` | `[]string` | nil → `["content-type","authorization","x-erpc-secret-token"]` (defaults.go:2831-2837) | 661 |
| `<CORS>.exposedHeaders` | `[]string` | — (nil; no default) | 662 |
| `<CORS>.allowCredentials` | `*bool` | nil → `false` (defaults.go:2838-2840) | 663 |
| `<CORS>.maxAge` | `int` | 0 → `3600` (defaults.go:2841-2843) | 664 |

### 16.3 AuthConfig + strategies (`<AUTH>`) — common/config.go:2443-2534; SetDefaults defaults.go:2611-2761

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<AUTH>.strategies[]` | `[]*AuthStrategyConfig` | — (nil) | 2444 |
| `<AUTH>.strategies[].ignoreMethods` | `[]string` | — (nil) | 2448 |
| `<AUTH>.strategies[].allowMethods` | `[]string` | — (nil) | 2449 |
| `<AUTH>.strategies[].rateLimitBudget` | `string` | — ("") | 2450 |
| `<AUTH>.strategies[].type` | `AuthType` | inferred: forced to match whichever strategy block is present (secret/database/jwt/siwe; defaults.go:2633-2670); conversely a declared type auto-creates its empty block (defaults.go:2624-2663); values secret/database/jwt/siwe/network (config.go:2435-2441) | 2452 |
| `<AUTH>.strategies[].network` | `*NetworkStrategyConfig` | nil → auto-created when type=network (defaults.go:2624-2626) | 2453 |
| `<AUTH>.strategies[].secret` | `*SecretStrategyConfig` | nil → auto-created when type=secret (defaults.go:2633-2635) | 2454 |
| `<AUTH>.strategies[].database` | `*DatabaseStrategyConfig` | nil → auto-created when type=database (defaults.go:2642-2644) | 2455 |
| `<AUTH>.strategies[].jwt` | `*JwtStrategyConfig` | nil → auto-created when type=jwt (defaults.go:2652-2654) | 2456 |
| `<AUTH>.strategies[].siwe` | `*SiweStrategyConfig` | nil → auto-created when type=siwe (defaults.go:2662-2664) | 2457 |
| `<AUTH>.strategies[].secret.id` | `string` | — ("") | 2461 |
| `<AUTH>.strategies[].secret.value` | `string` | — (""; redacted in marshal, config.go:2467-2480); no defaults (defaults.go:2744-2746) | 2462 |
| `<AUTH>.strategies[].secret.rateLimitBudget` | `string` | — ("") | 2464 |
| `<AUTH>.strategies[].database.connector` | `*ConnectorConfig` | nil → auto-created empty, SetDefaults with auth scope (defaults.go:2676-2678, 2722); see §16.4 | 2483 |
| `<AUTH>.strategies[].database.cache` | `*DatabaseStrategyCacheConfig` | nil → auto-created (defaults.go:2680-2682) | 2484 |
| `<AUTH>.strategies[].database.retry` | `*DatabaseRetryConfig` | nil → auto-created (defaults.go:2684-2686) | 2485 |
| `<AUTH>.strategies[].database.failOpen` | `*DatabaseFailOpenConfig` | nil → auto-created (defaults.go:2687-2689) | 2486 |
| `<AUTH>.strategies[].database.maxWait` | `Duration` | 0 → `1s` (defaults.go:2718-2720) | 2487 |
| `<AUTH>.strategies[].database.cache.ttl` | `*time.Duration` | nil → `1h` (defaults.go:2691-2694) | 2491 |
| `<AUTH>.strategies[].database.cache.maxSize` | `*int64` | nil → `10000` (defaults.go:2696-2699) | 2492 |
| `<AUTH>.strategies[].database.cache.maxCost` | `*int64` | nil → `1073741824` (1<<30 = 1GB; defaults.go:2701-2704) | 2493 |
| `<AUTH>.strategies[].database.cache.numCounters` | `*int64` | nil → `100000` (defaults.go:2706-2709) | 2494 |
| `<AUTH>.strategies[].database.retry.maxAttempts` | `int` | ≤0 → `3` (defaults.go:2726-2728) | 2498 |
| `<AUTH>.strategies[].database.retry.baseBackoff` | `Duration` | 0 → `100ms` (defaults.go:2729-2731) | 2499 |
| `<AUTH>.strategies[].database.failOpen.enabled` | `bool` | — (false; "disabled by default", defaults.go:2735-2737) | 2503 |
| `<AUTH>.strategies[].database.failOpen.userId` | `string` | "" → `"emergency-failopen"` (defaults.go:2738-2740) | 2504 |
| `<AUTH>.strategies[].database.failOpen.rateLimitBudget` | `string` | — ("") | 2505 |
| `<AUTH>.strategies[].jwt.allowedIssuers` | `[]string` | — (nil) | 2509 |
| `<AUTH>.strategies[].jwt.allowedAudiences` | `[]string` | — (nil) | 2510 |
| `<AUTH>.strategies[].jwt.allowedAlgorithms` | `[]string` | — (nil) | 2511 |
| `<AUTH>.strategies[].jwt.requiredClaims` | `[]string` | — (nil) | 2512 |
| `<AUTH>.strategies[].jwt.verificationKeys` | `map[string]string` | — (nil) | 2513 |
| `<AUTH>.strategies[].jwt.rateLimitBudgetClaimName` | `string` | "" → `"rlm"` (defaults.go:2748-2753) | 2517 |
| `<AUTH>.strategies[].siwe.allowedDomains` | `[]string` | — (nil); no defaults (defaults.go:2755-2757) | 2521 |
| `<AUTH>.strategies[].siwe.rateLimitBudget` | `string` | — ("") | 2523 |
| `<AUTH>.strategies[].network.allowedIPs` | `[]string` | — (nil); no defaults (defaults.go:2759-2761) | 2527 |
| `<AUTH>.strategies[].network.allowedCIDRs` | `[]string` | — (nil) | 2528 |
| `<AUTH>.strategies[].network.allowLocalhost` | `bool` | — (false) | 2529 |
| `<AUTH>.strategies[].network.trustedProxies` | `[]string` | — (nil) | 2530 |
| `<AUTH>.strategies[].network.rateLimitBudget` | `string` | — ("") | 2532 |
| `<AUTH>.strategies[].network.ipAsUser` | `bool` | — (false) | 2533 |

### 16.4 ConnectorConfig under auth (`<AUTH>.strategies[].database.connector`) — common/config.go:341-453; shared struct, fully covered by the database census; auth-scope defaults listed here for completeness

| yaml path | Go type | exact default (auth scope) | config.go line |
|---|---|---|---|
| `....connector.id` | `string` | "" → `"auth-<driver>"` (defaults.go:847-849) | 342 |
| `....connector.driver` | `ConnectorDriverType` | inferred from which driver block is present (defaults.go:876-930); values memory/redis/postgresql/dynamodb/grpc (config.go:333-339) | 343 |
| `....connector.memory` | `*MemoryConnectorConfig` | auto-created when driver=memory; maxItems 0→`100000`, maxTotalSize ""→`"1GB"` (defaults.go:879-886, 935-944); fields config.go:362-364 | 344 |
| `....connector.redis` | `*RedisConnectorConfig` | auto-created when driver=redis; connPoolSize 0→`8`, initTimeout→`5s`, getTimeout→`1s`, setTimeout→`3s`, lockRetryInterval→`500ms`; uri/addr mutually exclusive; addr+fields fold into uri (defaults.go:946-1019); fields config.go:384-394 | 345 |
| `....connector.dynamodb` | `*DynamoDBConnectorConfig` | auto-created when driver=dynamodb; table→`"erpc_auth"` (auth scope), partitionKeyName→`"groupKey"`, rangeKeyName→`"requestKey"`, reverseIndexName→`"idx_requestKey_groupKey"`, ttlAttributeName→`"ttl"`, initTimeout→`5s`, getTimeout→`1s`, setTimeout→`2s`, statePollInterval→`5s` (defaults.go:1061-1100); fields config.go:429-442 | 346 |
| `....connector.postgresql` | `*PostgreSQLConnectorConfig` | auto-created when driver=postgresql; table→`"erpc_auth"` (auth scope), minConns→`1` (auth; 4 otherwise), maxConns→`4` (auth; 32 otherwise), initTimeout→`5s`, getTimeout→`1s`, setTimeout→`2s` (defaults.go:1021-1059); fields config.go:446-452 | 347 |
| `....connector.grpc` | `*GrpcConnectorConfig` | auto-created when driver=grpc; getTimeout 0→`100ms` (defaults.go:920-930); fields (bootstrap/servers/headers/getTimeout) config.go:355-358 | 348 |
| `....connector.failsafeForGets[]` | `[]*FailsafeConfig` | — (nil); matchMethod ""→`"*"` per entry (defaults.go:850-862); struct = §12 | 349 |
| `....connector.failsafeForSets[]` | `[]*FailsafeConfig` | — (nil); same (defaults.go:863-875) | 350 |

## 17. Deprecated-but-still-parsed fields (consolidated checklist)

### 17.1 Project-level legacy scoring/routing keys (`LegacyProjectFields`, captured by ProjectConfig.UnmarshalYAML config.go:563-579 into `yaml:"-"` LegacyProject; consumed + cleared by legacy translator `common/legacy/translate.go` via hook config.go:79-83, 111-121)

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `projects[].routingStrategy` | `string` | — (translator synthesizes selectionPolicy eval + warning) | 550 |
| `projects[].scoreGranularity` | `string` | — | 551 |
| `projects[].scorePenaltyDecayRate` | `float64` | — | 552 |
| `projects[].scoreSwitchHysteresis` | `float64` | — | 553 |
| `projects[].scoreMinSwitchInterval` | `Duration` | — | 554 |
| `projects[].scoreMetricsMode` | `string` | — | 555 |
| `projects[].scoreRefreshInterval` | `Duration` | — | 556 |

### 17.2 Upstream legacy label keys (load-time-only rewrites; config.go:825-940)

| yaml path | Go type | exact default | config.go line |
|---|---|---|---|
| `<UPS>.group` | `string` | — ; rewritten to tag `tier:<group>` then forgotten (config.go:831, 921-933) | 831 |
| `<UPS>.cohort` | `string` | — ; rewritten to tag `cohort:<cohort>` then forgotten (config.go:832, 934-939) | 832 |
| `<UPS>.failsafe` as single object | `*FailsafeConfig` | accepted; wrapped into 1-element list, matchMethod defaulted `"*"` (config.go:851-898) | 852-871 |

### 17.3 Upstream EVM legacy fields (still first-class parsed; see §11.4 rows for lines)

| yaml path | status |
|---|---|
| `<UPS>.evm.nodeType` | deprecated → use blockAvailability bounds (config.go:1138-1139); "" → "unknown"; `full` synthesizes 128-block lower bound (defaults.go:1742-1747, 1758-1764) |
| `<UPS>.evm.maxAvailableRecentBlocks` | deprecated; mapped to `blockAvailability.lower.latestBlockMinus` (defaults.go:1749-1756) |
| `<UPS>.evm.getLogsMaxAllowedRange` | deprecated yaml-only (`json:"-"`), parsed but unused (config.go:1142-1143) |
| `<UPS>.evm.getLogsMaxAllowedAddresses` | deprecated yaml-only (config.go:1144-1145) |
| `<UPS>.evm.getLogsMaxAllowedTopics` | deprecated yaml-only (config.go:1146-1147) |
| `<UPS>.evm.getLogsSplitOnError` | deprecated yaml-only (config.go:1148-1149) |
| `<UPS>.evm.getLogsMaxBlockRange` | deprecated yaml-only (config.go:1150-1151) |

### 17.4 Network-level legacy fields

| yaml path | status |
|---|---|
| `<NET>.failsafe` / `P.networkDefaults.failsafe` as single object | accepted; wrapped into list, matchMethod `"*"` (config.go:2041-2103, 602-656) |
| `<ENET>.integrity.enforceHighestBlock` | deprecated (config.go:2309-2311); nil → `true` (defaults.go:2118-2120); migrated into directiveDefaults when those unset (defaults.go:1952-1966) | 2311 |
| `<ENET>.integrity.enforceGetLogsBlockRange` | deprecated; nil → `true` (defaults.go:2121-2123); migrated (config.go:2313) |
| `<ENET>.integrity.enforceNonNullTaggedBlocks` | deprecated; nil → `true` (defaults.go:2124-2126); migrated (config.go:2315) |
| `<ENET>.maxFutureBlockRetryDistance` | deprecated yaml-only (`json:"-"`); warned + cleared; use `emptyResultConfidence` (config.go:2252-2255, defaults.go:2080-2083) |

### 17.5 Failsafe / routing / selectionPolicy legacy keys

| yaml path | status |
|---|---|
| `<FS>.retry.emptyResultIgnore` | deprecated → `emptyResultAccept` (config.go:1351-1352; migration defaults.go:2261-2264) |
| `<FS>.retry.blockUnavailableDelay` | deprecated yaml-only → merged into `emptyResultDelay` + warning, then cleared (config.go:1363-1369; defaults.go:2265-2274) |
| `<FS>.timeout.quantile` / `.minDuration` / `.maxDuration` | legacy flat siblings folded into `timeout.duration` AdaptiveDuration at decode (config.go:1417-1480) |
| `<FS>.hedge.quantile` / `.minDelay` / `.maxDelay` | legacy flat siblings folded into `hedge.delay` AdaptiveDuration at decode (config.go:1504-1571) |
| `<UPS>.routing.scoreMultipliers[].totalRequests` | accepted, no scoring effect (config.go:808-811) |
| `<SP>.evalPerMethod` / `<SP>.evalPerFinality` | load-time aliases → translated into `evalScope`, then niled (config.go:2366-2375; defaults.go:2537-2585) |
| `<SP>.evalFunction`, `<SP>.resampleExcluded`, `<SP>.resampleInterval`, `<SP>.resampleCount` | legacy keys captured into LegacySelectionPolicy by UnmarshalYAML (config.go:2403-2431); translator wraps evalFunction into modern eval + warnings (common/legacy/types.go:34-39, translate.go) |

### 17.6 Other deprecated keys parsed elsewhere in config.go (owned by the server census, listed for cross-reference)

| yaml path | status |
|---|---|
| `server.httpPort` | deprecated → `httpPortV4`; migrated in ServerConfig.SetDefaults (config.go:139; defaults.go:651-665) |

— End of census. Field-row count (yaml-tagged leaves + deprecated keys enumerated above): 14 (project) + 7 (legacy project) + 6 (networkDefaults) + 10 (network) + 10 (methods) + 8 (staticResponses) + 25 (directiveDefaults) + 20 (evmNetwork) + 3 (servedTip) + 3 (evmIntegrity) + 6 (aliasing) + 16 (upstream) + 7 (rateLimitAutoTune) + 6 (jsonRpc) + 1 (grpc) + 16 (evmUpstream) + 3 (shadow) + 14 (routing/scoreMultipliers) + 7 (blockAvailability) + 4 (upstreamIntegrity) + 6 (queryShim) + 7 (failsafe) + 10 (retry) + 5 (circuitBreaker) + 4 (timeout incl. legacy) + 5 (hedge incl. legacy) + 4 (adaptiveDuration) + 17 (consensus) + 3 (punishMisbehavior) + 16 (misbehaviorsDestination/s3/awsAuth) + 7 (provider) + 43 (vendor settings) + 6 (selectionPolicy) + 3 (healthCheck) + 6 (cors) + 38 (auth tree) + 9 (connector-auth) + 3 (upstream legacy label) + 4 (selectionPolicy legacy) + 1 (server.httpPort xref) = 403 rows.
