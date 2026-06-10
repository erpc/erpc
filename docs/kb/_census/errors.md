# Census: Error codes & normalization rules

> status: complete
> source-dirs: common/errors.go, common/grpc_errors.go, common/error_extractor.go, common/json_rpc.go (TranslateToJsonRpcException), architecture/evm/error_normalizer.go, architecture/evm/util.go, architecture/evm/extractor.go, erpc/http_server.go (status mapping)

Scope: completeness checklist of every eRPC error code/type, its retryability semantics, HTTP status mapping, JSON-RPC numeric codes, and all vendor/gRPC error-normalization rules. Checklist only — KB detail files must cover every row.

Legend for the `retryable?` column — two independent axes evaluated by two predicates:

- `U` = `IsRetryableTowardsUpstream(err)` (`common/errors.go:L2438-L2498`): may the SAME upstream be retried. Returns `false` iff the code (anywhere in the cause chain, via `HasErrorCode`) is one of: `ErrCodeFailsafeCircuitBreakerOpen` (L2459), `ErrCodeUpstreamRequestSkipped` (L2462), `ErrCodeUpstreamMethodIgnored` (L2463), `ErrCodeEndpointUnsupported` (L2464), `ErrCodeEndpointBillingIssue` (L2467), `ErrCodeJsonRpcRequestUnmarshal` (L2470), `ErrCodeEndpointExecutionException` (L2473), `ErrCodeEndpointUnauthorized` (L2478), `ErrCodeEndpointRequestTooLarge` (L2479), `ErrCodeEndpointContentValidation` (L2486), or any `IsCapacityIssue` code (L2492 → `common/errors.go:L2500-L2509`: `ErrCodeProjectRateLimitRuleExceeded`, `ErrCodeNetworkRateLimitRuleExceeded`, `ErrCodeUpstreamRateLimitRuleExceeded`, `ErrCodeAuthRateLimitRuleExceeded`, `ErrCodeEndpointCapacityExceeded`). For `ErrUpstreamsExhausted` it recurses over children — retryable iff ANY child retryable (L2440-L2453). Everything else defaults to `true` (L2497).
- `N` = `IsRetryableTowardNetwork(err)` (`common/errors.go:L2379-L2436`): may a DIFFERENT upstream be tried. Default `true`; `false` only when (a) `ErrUpstreamsExhausted` with nil cause (L2393), (b) a multi-error cause where ALL children are non-retryable (L2400-L2411), or (c) any error along the linear (single) cause chain carries `Details["retryableTowardNetwork"]=false` (L2418-L2433; flag set via `WithRetryableTowardNetwork(false)`, `common/errors.go:L209-L217`). The flag walk stops at multi-error wrappers (L2429-L2431).

`HTTP (method)` column = the per-type `ErrorStatusCode()` method value. **Gotcha: `ErrorStatusCode()` has ZERO call sites in the repo** (verified by grep — only definitions in `common/errors.go` exist); the real wire status is decided by the switch in `erpc/http_server.go:L1473-L1491` (`handleErrorResponse`) and `erpc/http_server.go:L1280-L1317` (`determineResponseStatusCode`, called at `erpc/http_server.go:L727`). `wire:` annotations below mark codes that actually map to non-200 on the wire; all others return HTTP 200 with a JSON-RPC error body.

## 1. eRPC standard error codes (common/errors.go)

| # | error code constant | meaning (1 phrase) | retryable? (per code logic) | HTTP status mapped | file:line |
|---|---|---|---|---|---|
| 1 | `ErrInvalidRequest` (`ErrCodeInvalidRequest`) | invalid request body/headers | U:yes N:yes (default) | method:400 (`common/errors.go:L456`); wire:400 (`erpc/http_server.go:L1476`) | common/errors.go:L444 |
| 2 | `ErrInvalidUrlPath` (`ErrCodeInvalidUrlPath`) | malformed request URL path | U:yes N:yes (default) | method:400 (L476); wire:400 (`erpc/http_server.go:L1476`) | common/errors.go:L462 |
| 3 | `ErrInvalidConfig` | invalid configuration value | U:yes N:yes (default; startup-only) | — (wire:200) | common/errors.go:L485 |
| 4 | `ErrRequestTimeout` | request timed out before any upstream responded | U:yes N:yes (default) | method:504 (L505); wire:200 | common/errors.go:L496 |
| 5 | `ErrInternalServerError` | unexpected internal server error | U:yes N:yes (default) | — (wire:200) | common/errors.go:L514 |
| 6 | `ErrAuthUnauthorized` (`ErrCodeAuthUnauthorized`) | auth strategy rejected the request | U:yes N:yes (default) | method:401 (L541); wire:401 (`erpc/http_server.go:L1479-L1480`) | common/errors.go:L527 |
| 7 | `ErrAuthRateLimitRuleExceeded` (`ErrCodeAuthRateLimitRuleExceeded`) | auth-level rate-limit budget exceeded | U:no (capacity, L2506) N:yes | method:429 (L566); wire:429 (`erpc/http_server.go:L1485-L1490`) | common/errors.go:L547 |
| 8 | `ErrProjectNotFound` (`ErrCodeProjectNotFound`) | project id not configured | U:yes N:yes (default) | method:404 (L587); wire:404 (`erpc/http_server.go:L1482-L1483`) | common/errors.go:L576 |
| 9 | `ErrProjectAlreadyExists` | duplicate project registration | n/a (startup) | — | common/errors.go:L596 |
| 10 | `ErrNetworkNotFound` (`ErrCodeNetworkNotFound`) | network not configured | U:yes N:yes (default) | method:404 (L625); wire:404 (`erpc/http_server.go:L1482-L1483`) | common/errors.go:L609 |
| 11 | `ErrUnknownNetworkID` | cannot resolve network id from config or upstreams | U:yes N:yes (default) | method:400 (L643); wire:200 | common/errors.go:L634 |
| 12 | `ErrUnknownNetworkArchitecture` | unrecognized network architecture | U:yes N:yes (default) | method:400 (L661); wire:200 | common/errors.go:L652 |
| 13 | `ErrNotImplemented` | feature/method not implemented by eRPC | U:yes N:yes (default) | method:501 (L676); wire:200 | common/errors.go:L670 |
| 14 | `ErrInvalidEvmChainId` | EVM chain id is not numeric | U:yes N:yes (default) | method:400 (L694); wire:200 | common/errors.go:L685 |
| 15 | `ErrFinalizedBlockUnavailable` (`ErrCodeFinalizedBlockUnavailable`) | finalized/latest block unknown when checking finality | U:yes N:yes (default) | — | common/errors.go:L700 |
| 16 | `ErrUpstreamClientInitialization` | upstream client could not be initialized | U:yes N:yes (default); fatal for initializer ONLY when wrapped in `TaskFatalError` (comment L731-L737; test `common/errors_task_fatal_test.go:L34-L104`) | — | common/errors.go:L745 |
| 17 | `ErrUpstreamRequest` (`ErrCodeUpstreamRequest`) | wrapper: request to a specific upstream failed (carries durationMs/attempts/retries/hedges) | inherited from cause (flag walk descends, L2418-L2433) | — | common/errors.go:L760 |
| 18 | `ErrUpstreamMalformedResponse` | upstream response unparsable | U:yes N:yes (default) | method:400 (L856); wire:200 | common/errors.go:L846 |
| 19 | `ErrUpstreamsExhausted` (`ErrCodeUpstreamsExhausted`) | all upstream attempts failed (joined child errors) | U/N: recursive over children (U: L2440-L2453; N: L2400-L2411); N:no when cause nil (L2393) | — (status from dominant child via wire switch) | common/errors.go:L869 |
| 20 | `ErrNoUpstreamsLeftToSelect` (`ErrCodeNoUpstreamsLeftToSelect`) | selection pool empty after filtering (details map upstreamId→state) | U:yes N:yes (default) | — | common/errors.go:L1186 |
| 21 | `ErrNoUpstreamsDefined` | project has zero upstreams | U:yes N:yes (default) | method:404 (L1231); wire:200 | common/errors.go:L1222 |
| 22 | `ErrNoUpstreamsFound` | no upstream matches project+network | U:yes N:yes (default) | method:404 (L1248); wire:200 | common/errors.go:L1238 |
| 23 | `ErrNetworkInitializing` (`ErrCodeNetworkInitializing`) | network still bootstrapping, retry shortly | U:yes N:yes (default) | method:503 (L1269); wire:200 | common/errors.go:L1254 |
| 24 | `ErrNetworkNotSupported` (`ErrCodeNetworkNotSupported`) | no provider/static upstream supports network (init-fatal) | U:yes N:yes (default) | method:404 (L1290); wire:404 (`erpc/http_server.go:L1482-L1483`) | common/errors.go:L1275 |
| 25 | `ErrUpstreamNetworkNotDetected` | upstream network undetectable (config+endpoint) | U:yes N:yes (default) | — | common/errors.go:L1303 |
| 26 | `ErrUpstreamInitialization` | upstream bootstrap failed | U:yes N:yes (default) | — | common/errors.go:L1318 |
| 27 | `ErrUpstreamRequestSkipped` (`ErrCodeUpstreamRequestSkipped`) | request not forwarded to upstream (permanent skip reason in cause) | U:no (L2462) N:yes | method:406 (L1345); wire:200 | common/errors.go:L1330 |
| 28 | `ErrUpstreamBlockUnavailable` (`ErrCodeUpstreamBlockUnavailable`) | upstream lags the requested block (transient; unlike RequestSkipped) | U:yes N:yes (default; doc comment L1349-L1351) | method:503 (L1371); wire:200 | common/errors.go:L1354 |
| 29 | `ErrUpstreamMethodIgnored` (`ErrCodeUpstreamMethodIgnored`) | method in upstream's ignoreMethods config | U:no (L2463) N:yes | method:406 (L1392); wire:200 | common/errors.go:L1377 |
| 30 | `ErrUpstreamSyncing` (`ErrCodeUpstreamSyncing`) | upstream node still syncing | U:yes N:yes (default) | method:422 (L1412); wire:200 | common/errors.go:L1398 |
| 31 | `ErrUpstreamShadowing` (`ErrCodeUpstreamShadowing`) | shadow upstream must not serve real traffic | U:yes N:yes (default) | — | common/errors.go:L1418 |
| 32 | `ErrUpstreamNotAllowed` | upstream excluded by use-upstream directive | U:yes N:yes (default) | — | common/errors.go:L1437 |
| 33 | `ErrUpstreamHedgeCancelled` (`ErrCodeUpstreamHedgeCancelled`) | hedged request discarded after another won | U:yes N:yes (default); counted as "cancelled" in exhausted summary (L1000-L1002) | — | common/errors.go:L1449 |
| 34 | `ErrResponseWriteLock` | response already being written by another writer | U:yes N:yes (default) | — | common/errors.go:L1469 |
| 35 | `ErrJsonRpcRequestUnmarshal` (`ErrCodeJsonRpcRequestUnmarshal`) | client JSON-RPC body parse failure | U:no (L2470) N:no (flag set in all 3 ctor branches L1496/L1507/L1518) | method:400 (L1525); wire:400 (`erpc/http_server.go:L1476`) | common/errors.go:L1486 |
| 36 | `ErrJsonRpcRequestUnresolvableMethod` | no method field resolvable in request | U:yes N:no (flag L1538) | — | common/errors.go:L1534 |
| 37 | `ErrJsonRpcRequestPreparation` | failed to build upstream JSON-RPC request | U:yes N:no (flag defaulted L1562-L1564, overridable via details) | — | common/errors.go:L1551 |
| 38 | `ErrFailsafeConfiguration` | invalid failsafe policy config | n/a (startup) | — | common/errors.go:L1578 |
| 39 | `ErrFailsafeTimeoutExceeded` (`ErrCodeFailsafeTimeoutExceeded`) | failsafe timeout policy tripped (scope in message) | U:yes N:yes (default) | method:504 (L1610); wire:200 | common/errors.go:L1588 |
| 40 | `ErrFailsafeRetryExceeded` (`ErrCodeFailsafeRetryExceeded`) | retry policy exhausted (scope in message) | inherited from cause | — | common/errors.go:L1627 |
| 41 | `ErrFailsafeCircuitBreakerOpen` (`ErrCodeFailsafeCircuitBreakerOpen`) | circuit breaker open for upstream | U:no (L2459) N:yes | — | common/errors.go:L1667 |
| 42 | `ErrRateLimitBudgetNotFound` | unknown rate-limit budget id | n/a (config) | — | common/errors.go:L1714 |
| 43 | `ErrRateLimitRuleNotFound` | no rule for method in budget | n/a (config) | — | common/errors.go:L1728 |
| 44 | `ErrProjectRateLimitRuleExceeded` (`ErrCodeProjectRateLimitRuleExceeded`) | project-level budget exceeded | U:no (L2503) N:yes | method:429 (L1756); wire:429 (`erpc/http_server.go:L1485-L1490`) | common/errors.go:L1740 |
| 45 | `ErrNetworkRateLimitRuleExceeded` (`ErrCodeNetworkRateLimitRuleExceeded`) | network-level budget exceeded | U:no (L2504) N:yes | method:429 (L1779); wire:429 (`erpc/http_server.go:L1485-L1490`) | common/errors.go:L1762 |
| 46 | `ErrNetworkRequestTimeout` (`ErrCodeNetworkRequestTimeout`) | network-scope timeout across upstreams | U:yes N:yes (default) | method:504 (L1797); wire:200 | common/errors.go:L1785 |
| 47 | `ErrUpstreamRateLimitRuleExceeded` (`ErrCodeUpstreamRateLimitRuleExceeded`) | upstream-level budget exceeded | U:no (L2505) N:yes | method:429 (L1819); wire:200 — NOT in wire 429 list (`erpc/http_server.go:L1485-L1490`) | common/errors.go:L1803 |
| 48 | `ErrUpstreamExcludedByPolicy` (`ErrCodeUpstreamExcludedByPolicy`) | selection policy (script) excluded upstream | U:yes N:yes (default) | — | common/errors.go:L1825 |
| 49 | `ErrEndpointUnauthorized` (`ErrCodeEndpointUnauthorized`) | provider returned 401/403 | U:no (L2478) N:yes | method:401 (L1858); wire:401 (`erpc/http_server.go:L1479-L1480`) | common/errors.go:L1846 |
| 50 | `ErrEndpointUnsupported` (`ErrCodeEndpointUnsupported`) | provider does not support the method | U:no (L2464) N:yes | method:406 (L1876); wire:200 | common/errors.go:L1864 |
| 51 | `ErrEndpointClientSideException` (`ErrCodeEndpointClientSideException`) | provider blamed our request (4xx-class) | U:yes (not excluded; test `common/errors_retry_test.go:L175-L187`) N:yes by default, but several normalizer paths attach `WithRetryableTowardNetwork(false)` (see §4 rows 15,16c; gRPC InvalidArgument) | method:400, but 200 when cause's normalizedCode ∈ {3 EvmReverted, −32000 CallException, −32003 TransactionRejected} (L1894-L1905); wire:200 | common/errors.go:L1882 |
| 52 | `ErrEndpointExecutionException` (`ErrCodeEndpointExecutionException`) | EVM revert / execution failure on node | U:no (L2473) N:no (flag in ctor L1918) — except `eth_sendRawTransaction` normalizer overrides N:yes (`architecture/evm/error_normalizer.go:L263-L272,L282-L293,L382-L391`) | method:200 (L1924-L1927); wire:200 | common/errors.go:L1909 |
| 53 | `ErrEndpointTransportFailure` (`ErrCodeEndpointTransportFailure`) | transport/network failure reaching endpoint (URL stripped from cause msg L1934-L1938) | U:yes N:yes (default) | — | common/errors.go:L1931 |
| 54 | `ErrEndpointServerSideException` (`ErrCodeEndpointServerSideException`) | provider internal/5xx error | U:yes N:yes (default; tests `common/errors_retry_test.go:L99-L138`) | method: original upstream status, else 500 (L1974-L1979); wire:200 | common/errors.go:L1960 |
| 55 | `ErrEndpointRequestTimeout` (`ErrCodeEndpointRequestTimeout`) | endpoint call timed out | U:yes N:yes (default; test L189-L216) | method:504 (L2003); wire:200 | common/errors.go:L1988 |
| 56 | `ErrEndpointRequestCanceled` (`ErrCodeEndpointRequestCanceled`) | request canceled (e.g. discarded hedge) | U:yes N:yes (default); severity=warning (L2545) | — | common/errors.go:L2009 |
| 57 | `ErrEndpointCapacityExceeded` (`ErrCodeEndpointCapacityExceeded`) | provider rate-limited the request | U:no (L2507) N:yes | method:429 (L2035); wire:429 (`erpc/http_server.go:L1485-L1490`) | common/errors.go:L2023 |
| 58 | `ErrEndpointBillingIssue` (`ErrCodeEndpointBillingIssue`) | provider monthly quota/billing exhausted | U:no (L2467) N:yes | method:402 (L2053); wire:200 | common/errors.go:L2041 |
| 59 | `ErrEndpointMissingData` (`ErrCodeEndpointMissingData`) | data absent on this provider (details: latestBlock/finalizedBlock/maxAvailableRecentBlocks L2065-L2079) | U:yes N:yes (tests `common/errors_retry_test.go:L28-L97`) | method:200 (L2092-L2095); wire:200 | common/errors.go:L2062 |
| 60 | `ErrUpstreamNodeTypeMismatch` (`ErrCodeUpstreamNodeTypeMismatch`) | node type (archive/full) doesn't satisfy request | U:yes N:yes (default) | — | common/errors.go:L2099 |
| 61 | `ErrEndpointRequestTooLarge` (`ErrCodeEndpointRequestTooLarge`) | provider says request too large (complaint: `evm_block_range` L2121 / `evm_addresses` L2122) | U:no (L2479) N:yes | method:413 (L2137); wire:200 | common/errors.go:L2117 |
| 62 | `ErrJsonRpcExceptionInternal` (`ErrCodeJsonRpcExceptionInternal`) | normalized JSON-RPC error carrier (details: originalCode/normalizedCode L2185-L2190; CodeChain prefixes numeric code L2202-L2204) | wrapper — semantics from enclosing typed error | — | common/errors.go:L2179 |
| 63 | `ErrJsonRpcExceptionExternal` (struct, no ErrorCode) | raw upstream JSON-RPC error {code:int, message, data} | n/a (input to normalizer) | — | common/errors.go:L2225 |
| 64 | `ErrInvalidConnectorDriver` (`ErrCodeInvalidConnectorDriver`) | unknown cache/store driver | n/a (config) | — | common/errors.go:L2281 |
| 65 | `ErrRecordNotFound` (`ErrCodeRecordNotFound`) | store record not found (cache miss) | U:yes N:yes (default; internal) | — | common/errors.go:L2297 |
| 66 | `ErrRecordExpired` (`ErrCodeRecordExpired`) | store record past expiration | U:yes N:yes (default; internal) | — | common/errors.go:L2315 |
| 67 | `ErrConsensusDispute` (`ErrCodeConsensusDispute`) | consensus participants returned conflicting results | N: multi-error children rule (L2400-L2411) | method:409 (L2587); wire:200 | common/errors.go:L2559 |
| 68 | `ErrConsensusLowParticipants` (`ErrCodeConsensusLowParticipants`) | not enough valid consensus participants | N: multi-error children rule (L2400-L2411) | method:412 (L2621); wire:200 | common/errors.go:L2593 |
| 69 | `ErrGetLogsExceededMaxAllowedRange` (`ErrCodeGetLogsExceededMaxAllowedRange`) | client getLogs block range above eRPC cap | U:yes N:yes (default); IsClientError (L2516) | method:413 (L2673); wire:200 | common/errors.go:L2658 |
| 70 | `ErrGetLogsExceededMaxAllowedAddresses` (`ErrCodeGetLogsExceededMaxAllowedAddresses`) | client getLogs address count above cap | as above (L2517) | method:413 (L2694); wire:200 | common/errors.go:L2679 |
| 71 | `ErrGetLogsExceededMaxAllowedTopics` (`ErrCodeGetLogsExceededMaxAllowedTopics`) | client getLogs topic count above cap | as above (L2518) | method:413 (L2715); wire:200 | common/errors.go:L2700 |
| 72 | `ErrEndpointContentValidation` (`ErrCodeEndpointContentValidation`) | upstream response failed validation directives | U:no (L2486) N:yes (doc comment L2719-L2721) | method:502 (L2745); wire:200 | common/errors.go:L2727 |
| 73 | `ErrEndpointNonceException` (`ErrCodeEndpointNonceException`) | tx with this nonce already known/used (sendRawTransaction idempotency; reason: `already_known` L2754 / `nonce_too_low` L2756) | U:yes N:no (flag L2773) | method:200 (L2779); wire:200 | common/errors.go:L2763 |
| 74 | `TaskFatalError` (not a StandardError) | control signal: mark initializer task non-retryable; `IsTaskFatal()`=true (L434), `NewTaskFatal` (L436) | n/a — initializer stops retrying (tests `common/errors_task_fatal_test.go:L45-L104`) | — | common/errors.go:L421 |
| 75 | `ErrDynamicTimeoutExceeded` (sentinel `errors.New`) | context cause distinguishing failsafe-policy timeout from parent deadline | n/a (sentinel) | — | common/errors.go:L1984 |
| 76 | `ErrGeneric` / `ErrUnknown` (pseudo-codes) | placeholder codes for non-standard causes in JSON marshaling (L305, L351) and HTTP fallback body (`erpc/http_server.go:L1450-L1454`) | n/a | — | common/errors.go:L305,L351 |

## 2. JSON-RPC numeric error codes (`JsonRpcErrorNumber`, common/errors.go:L2151-L2174)

| constant | value | meaning | file:line |
|---|---|---|---|
| `JsonRpcErrorUnknown` | −99999 | unknown/unclassified | common/errors.go:L2154 |
| `JsonRpcErrorCallException` | −32000 | generic call exception (standard) | common/errors.go:L2157 |
| `JsonRpcErrorTransactionRejected` | −32003 | transaction rejected (standard) | common/errors.go:L2158 |
| `JsonRpcErrorClientSideException` | −32600 | invalid request (standard) | common/errors.go:L2159 |
| `JsonRpcErrorUnsupportedException` | −32601 | method not found (standard) | common/errors.go:L2160 |
| `JsonRpcErrorInvalidArgument` | −32602 | invalid params (standard) | common/errors.go:L2161 |
| `JsonRpcErrorServerSideException` | −32603 | internal error (standard) | common/errors.go:L2162 |
| `JsonRpcErrorParseException` | −32700 | parse error (standard) | common/errors.go:L2163 |
| `JsonRpcErrorEvmReverted` | 3 | EVM execution reverted (de-facto) | common/errors.go:L2166 |
| `JsonRpcErrorCapacityExceeded` | −32005 | rate-limit/capacity (eRPC-normalized) | common/errors.go:L2169 |
| `JsonRpcErrorEvmLargeRange` | −32012 | request range too large (eRPC-normalized) | common/errors.go:L2170 |
| `JsonRpcErrorMissingData` | −32014 | data missing on node (eRPC-normalized) | common/errors.go:L2171 |
| `JsonRpcErrorNodeTimeout` | −32015 | node-level timeout (eRPC-normalized) | common/errors.go:L2172 |
| `JsonRpcErrorUnauthorized` | −32016 | unauthorized (eRPC-normalized) | common/errors.go:L2173 |

## 3. Extractor plumbing

| item | role | file:line |
|---|---|---|
| `JsonRpcErrorExtractor` interface | inject arch-specific JSON-RPC error normalization into HTTP clients (avoids import cycles) | common/error_extractor.go:L10-L12 |
| `JsonRpcErrorExtractorFunc` | func adapter for the interface | common/error_extractor.go:L17-L21 |
| `evm.JsonRpcErrorExtractor` | EVM impl delegating to `ExtractJsonRpcError` | architecture/evm/extractor.go:L11-L17 |
| injection site | `evm.NewJsonRpcErrorExtractor()` passed to client registry | upstream/registry.go:L79 |
| invocation site (HTTP) | `c.errorExtractor.Extract(r, nr, jr, c.upstream)` | clients/http_json_rpc_client.go:L930 |
| invocation site (gRPC/BDS) | `common.ExtractGrpcErrorFromGrpcStatus(st, c.upstream)` | clients/grpc_bds_client.go:L925 |
| vendor hook | `getVendorSpecificErrorIfAny` → `vendor.GetVendorSpecificErrorIfAny(req, resp, jr, details)`; runs BEFORE all generic rules | architecture/evm/error_normalizer.go:L29-L31,L878-L900 |
| LEGACY (dead) | `evm.ExtractGrpcError` — duplicate gRPC normalizer, no non-test call sites; differs: BDS `TIMEOUT_ERROR`→ServerSideException (L740-L751), `DeadlineExceeded`→ServerSideException (L814-L825), no `Canceled` case | architecture/evm/error_normalizer.go:L660-L876 |

## 4. HTTP/JSON-RPC vendor error → normalized error (evm `ExtractJsonRpcError`, architecture/evm/error_normalizer.go:L20-L658)

Entry condition: `jr.Error != nil` OR HTTP status > 299 (L21). If status > 299 with no JSON-RPC error, a synthetic external error code −32603 is created (L32-L38). `details` always gets `statusCode` + useful headers (L23-L24). String `data` with `"Reverted "` prefix is stripped (L49-L53); data is appended to msg for matching (L55-L64). Rules evaluated IN ORDER (first match wins); `code` = original vendor numeric code, kept as `originalCode`:

| # | trigger (status / message patterns / code) | normalized typed error | normalized JSON-RPC code | retry flags | file:line |
|---|---|---|---|---|---|
| 0 | vendor-specific hook returns non-nil | (vendor-defined) | (vendor-defined) | — | architecture/evm/error_normalizer.go:L29-L31 |
| 1 | "Try with this block range", "block range is too wide", "this block range should work", "range too large", "exceeds the range", "max block range", "Max range", "logs over more", "response size should not", "returned more than", "exceeds max results", "range is too large", "too large, max is", "response too large", "query exceeds limit", "limit the query to", "maximum block range", "range limit exceeded", "too many results", "try paginating", ("maximum"+"blocks distance"), "eth_getLogs is limited" | `ErrEndpointRequestTooLarge` (complaint `evm_block_range`) | −32012 EvmLargeRange | U:no | architecture/evm/error_normalizer.go:L70-L101 |
| 2 | "specify less number of address", "addresses or topics per search position" (Alchemy/DRPC), "filters"+"current limit is" (Infura) | `ErrEndpointRequestTooLarge` (complaint `evm_addresses`) | −32012 EvmLargeRange | U:no | architecture/evm/error_normalizer.go:L102-L117 (test: architecture/evm/error_normalizer_test.go:L14-L57) |
| 3 | "sender is over rate limit" (OP sequencer; all providers hit same sequencer) | `ErrEndpointCapacityExceeded` | −32005 CapacityExceeded | U:no, N:no (explicit flag) | architecture/evm/error_normalizer.go:L125-L139 |
| 4 | status 402, "reached the free tier", "Monthly capacity limit", "limit for your current plan", "/billing" | `ErrEndpointBillingIssue` | −32005 CapacityExceeded | U:no | architecture/evm/error_normalizer.go:L141-L155 |
| 5 | status 429, "requests limited to", "has exceeded", "Exceeded the quota", "Too many requests", "Too Many Requests", "under too much load", "request limit reached", "No server available", "reached the quota", "upgrade your tier", "rate limit", "too many requests", "limit exceeded" | `ErrEndpointCapacityExceeded` | −32005 CapacityExceeded | U:no, N:yes | architecture/evm/error_normalizer.go:L156-L179 |
| 6 | block-tag unsupported: "pending block is not available", "pending block not found", "Pending block not found", "safe block not found", "Safe block not found", "finalized block not found", "Finalized block not found", "finalized is not a supported", "pending is not a supported", "safe is not a supported", "not a supported commitment", "malformed blocknumber" | `ErrEndpointClientSideException` | −32600 ClientSideException | U:yes N:yes (deliberately retryable, comment L198-L199) | architecture/evm/error_normalizer.go:L185-L209 |
| 7 | `IsMissingDataError(err)` — "missing trie node", "header not found", "could not find block", "unknown block", "Unknown block", "height must be less than or equal", "invalid blockhash finalized", "Expect block number from id", "block not found", "Block not found", "block height passed is invalid", "cannot query unfinalized", "height is not available", "genesis is not traceable", "could not find FinalizeBlock", "no historical rpc", ("blocks specified"+"cannot be found"), "transaction not found", "cannot find transaction", "after last accepted block", "No state available", ("historical state"+("is not available"\|"unavailable")), "trie does not", "greater than latest", "not currently canonical", "requested data is not available" | `ErrEndpointMissingData` | −32014 MissingData | U:yes N:yes | architecture/evm/error_normalizer.go:L215-L226; architecture/evm/util.go:L19-L49 |
| 8 | "execution timeout" | `ErrEndpointServerSideException` (originalStatusCode preserved) | −32015 NodeTimeout | U:yes N:yes | architecture/evm/error_normalizer.go:L232-L244 |
| 9 | reverts: "reverted", "VM execution error", "transaction: revert", "VM Exception", lower("intrinsic gas too high") | `ErrEndpointExecutionException` | 3 EvmReverted | U:no N:no — except method==`eth_sendRawTransaction` ⇒ N:yes | architecture/evm/error_normalizer.go:L249-L273 |
| 10 | "EVM error: InvalidJump" (Berachain) — message rewritten to "revert: invalid jump destination" | `ErrEndpointExecutionException` | 3 EvmReverted | same sendRawTransaction override | architecture/evm/error_normalizer.go:L275-L294 |
| 11 | duplicate-tx (lowercased msg): "already known", "known transaction", "already imported", "transaction already in mempool", "tx already in mempool", "already in the mempool", "transaction already exists", "already have transaction", "already exists in mempool" — MUST precede generic −32003 (comment L297-L301) | `ErrEndpointNonceException` (reason `already_known`) | −32003 TransactionRejected | U:yes N:no (ctor flag) | architecture/evm/error_normalizer.go:L303-L324 |
| 12 | nonce conflict (lowercased): "nonce too low", "nonce is too low", "nonce has already been used" | `ErrEndpointNonceException` (reason `nonce_too_low`) | −32003 TransactionRejected | U:yes N:no | architecture/evm/error_normalizer.go:L326-L341 |
| 13 | "insufficient funds", "insufficient balance" — NO sendRawTransaction retry override (comment L345-L348) | `ErrEndpointExecutionException` | −32003 TransactionRejected | U:no N:no | architecture/evm/error_normalizer.go:L350-L361 |
| 14 | code == −32003, or "out of gas", "gas too low", "IntrinsicGas" | `ErrEndpointExecutionException` | −32003 TransactionRejected | U:no N:no — except `eth_sendRawTransaction` ⇒ N:yes | architecture/evm/error_normalizer.go:L368-L392 |
| 15 | "not found"/"does not exist"/"not available"/"is disabled"/"is not available" AND ("Method"\|"method"\|"Module"\|"module") | `ErrEndpointUnsupported` | −32601 UnsupportedException | U:no N:yes | architecture/evm/error_normalizer.go:L398-L414 |
| 15b | same outer AND ("header"\|"block"\|"Header"\|"Block"\|"transaction"\|"Transaction"\|"state"\|"State") | `ErrEndpointMissingData` | −32014 MissingData | U:yes N:yes | architecture/evm/error_normalizer.go:L415-L432 |
| 15c | same outer, neither sub-match | `ErrEndpointClientSideException` | −32600 ClientSideException | U:yes N:yes (deliberately retryable, comment L434-L435) | architecture/evm/error_normalizer.go:L433-L445 |
| 16 | unsupported: status 415 or 405, code==−32601, code==−32004 or −32001, "Unsupported method", "not supported", "method is not whitelisted", "not allowed to access method", "is not included in your current plan" — message rewritten "The method %s does not exist/is not available"; vendor msg kept in `details.vendorMessage` (must stay AFTER "not found" rules, comment L452-L453) | `ErrEndpointUnsupported` | −32601 UnsupportedException | U:no N:yes | architecture/evm/error_normalizer.go:L455-L477 |
| 17 | malformed tx (lowercased): "typed transaction too short", "transaction too short", "rlp: expected input list", "rlp: input string too short", "rlp: value size exceeds", "invalid transaction", "transaction type not supported" | `ErrEndpointClientSideException` | −32602 InvalidArgument | U:yes N:no (explicit flag) | architecture/evm/error_normalizer.go:L484-L500 |
| 18 | "tx of type", "invalid type: map, expected BlockNumber, 'latest', or 'earliest'" (Envio) | `ErrEndpointClientSideException` | −32601 UnsupportedException | U:yes N:yes | architecture/evm/error_normalizer.go:L510-L520 |
| 18b | code==−32600 AND data.message contains "validation errors in batch" | `ErrEndpointClientSideException` | −32000 CallException | U:yes N:yes (caller may split batch) | architecture/evm/error_normalizer.go:L521-L537 |
| 18c | code==−32602 or −32600 (generic) | `ErrEndpointClientSideException` | −32602 InvalidArgument | U:yes N:yes | architecture/evm/error_normalizer.go:L538-L548 |
| 18d | "param is required", "Invalid Request", "validation errors", "invalid argument", "invalid params", "Bad request input parameters" | `ErrEndpointClientSideException` | −32602 InvalidArgument | U:yes N:no (explicit flag; likely user mistake) | architecture/evm/error_normalizer.go:L549-L566 |
| 19 | status 401 or 403, "not allowed to access", "invalid api key", "key is inactive", "unauthorized" | `ErrEndpointUnauthorized` | −32016 Unauthorized | U:no N:yes | architecture/evm/error_normalizer.go:L573-L587 |
| 20 | fallback (anything else) | `ErrEndpointServerSideException` (originalStatusCode = upstream HTTP status) | passthrough: normalized code = original vendor code | U:yes N:yes | architecture/evm/error_normalizer.go:L592-L602 |
| 21 | 200-OK revert payload: result begins with `0x08c379a0` (Error(string) selector; substring check `dt[1:11]` skips the JSON quote) | `ErrEndpointExecutionException` ("transaction reverted", revert data in details) | 3 EvmReverted | U:no N:no | architecture/evm/error_normalizer.go:L609-L624 |
| 22 | `trace_*`/`debug_*`/`eth_trace*` method AND result body contains "execution timeout" (string scan, no JSON parse, for 50MB traces) | `ErrEndpointServerSideException` | −32015 NodeTimeout | U:yes N:yes | architecture/evm/error_normalizer.go:L626-L652 |

## 5. gRPC/BDS error → normalized error (live path: `common.ExtractGrpcErrorFromGrpcStatus`, common/grpc_errors.go:L12-L225)

Nil status or `codes.OK` ⇒ nil (L13-L15). Details always include `grpcCode`, `grpcMessage`, `upstreamId`, plus BDS `bdsErrorCode`/cause/details when present (L17-L37). BDS error code checked FIRST (L42-L122), then gRPC code (L124-L224):

| trigger | normalized typed error | normalized JSON-RPC code | retry flags | file:line |
|---|---|---|---|---|
| BDS `UNSUPPORTED_BLOCK_TAG`, `UNSUPPORTED_METHOD` | `ErrEndpointUnsupported` | −32601 | U:no | common/grpc_errors.go:L44-L54 |
| BDS `RANGE_OUTSIDE_AVAILABLE` | `ErrEndpointMissingData` | −32014 | U:yes N:yes | common/grpc_errors.go:L55-L65 |
| BDS `INVALID_PARAMETER`, `INVALID_REQUEST` | `ErrEndpointClientSideException` | −32602 | N:no (explicit flag) | common/grpc_errors.go:L66-L75 |
| BDS `RATE_LIMITED` | `ErrEndpointCapacityExceeded` | −32005 | U:no | common/grpc_errors.go:L76-L85 |
| BDS `TIMEOUT_ERROR` | `ErrEndpointRequestTimeout` (dur=0) | −32015 | U:yes N:yes | common/grpc_errors.go:L86-L97 |
| BDS `RANGE_TOO_LARGE` | `ErrEndpointRequestTooLarge` (`evm_block_range`) | −32012 | U:no | common/grpc_errors.go:L98-L108 |
| BDS `INTERNAL_ERROR` | `ErrEndpointServerSideException` | −32603 | U:yes N:yes | common/grpc_errors.go:L109-L120 |
| gRPC `Canceled` | `ErrEndpointRequestCanceled` (codes 0/0) | none | severity warning | common/grpc_errors.go:L125-L136 |
| gRPC `Unimplemented` | `ErrEndpointUnsupported` | −32601 | U:no | common/grpc_errors.go:L137-L146 |
| gRPC `InvalidArgument` | `ErrEndpointClientSideException` | −32602 | N:no (explicit flag) | common/grpc_errors.go:L147-L156 |
| gRPC `ResourceExhausted` | `ErrEndpointCapacityExceeded` | −32005 | U:no | common/grpc_errors.go:L157-L166 |
| gRPC `DeadlineExceeded` | `ErrEndpointRequestTimeout` (dur=0) | −32015 | U:yes N:yes | common/grpc_errors.go:L167-L178 |
| gRPC `Unauthenticated`, `PermissionDenied` | `ErrEndpointUnauthorized` | −32016 | U:no | common/grpc_errors.go:L179-L188 |
| gRPC `NotFound`, `OutOfRange` | `ErrEndpointMissingData` | −32014 | U:yes N:yes | common/grpc_errors.go:L189-L199 |
| gRPC `Internal`, `Unknown`, `Unavailable` | `ErrEndpointServerSideException` | −32603 | U:yes N:yes | common/grpc_errors.go:L200-L211 |
| gRPC default (any other code) | `ErrEndpointServerSideException` | −32603 | U:yes N:yes | common/grpc_errors.go:L212-L224 |

Legacy duplicate `evm.ExtractGrpcError` (architecture/evm/error_normalizer.go:L660-L876, NO non-test callers) deviates: BDS `TIMEOUT_ERROR` → ServerSideException+NodeTimeout (L740-L751); gRPC `DeadlineExceeded` → ServerSideException+NodeTimeout (L814-L825); no `Canceled` branch (falls to default ServerSideException, L863-L875).

## 6. Internal error → client-facing JSON-RPC numeric code (`TranslateToJsonRpcException`, common/json_rpc.go:L1501-L1630)

| input condition | output normalized code / behavior | file:line |
|---|---|---|
| `ErrUpstreamsExhausted` | replaced by the DOMINANT child error (most frequent code; `ErrCodeUpstreamRequestSkipped` and `ErrCodeEndpointUnsupported` excluded from counting; earliest error per code wins ties) before translation | common/json_rpc.go:L1502-L1536 |
| chain already contains `ErrCodeJsonRpcExceptionInternal` | returned as-is (upstream-derived normalized code preserved) | common/json_rpc.go:L1538-L1542 |
| `ErrCodeAuthRateLimitRuleExceeded` / `ErrCodeProjectRateLimitRuleExceeded` / `ErrCodeNetworkRateLimitRuleExceeded` / `ErrCodeUpstreamRateLimitRuleExceeded` | −32005 CapacityExceeded, "rate-limit exceeded" | common/json_rpc.go:L1547-L1561 |
| `ErrCodeAuthUnauthorized` | −32016 Unauthorized, "unauthorized" | common/json_rpc.go:L1562-L1573 |
| `ErrCodeUpstreamMethodIgnored` | −32601 UnsupportedException, "method ignored by upstream: <deepest msg>" | common/json_rpc.go:L1574-L1588 |
| `ErrCodeJsonRpcRequestUnmarshal` | −32700 ParseException, "failed to parse json-rpc request" | common/json_rpc.go:L1589-L1597 |
| `ErrCodeInvalidRequest` / `ErrCodeInvalidUrlPath` | −32602 InvalidArgument, "invalid request url and/or body" | common/json_rpc.go:L1598-L1606 |
| `ErrCodeGetLogsExceededMaxAllowedRange` / `...Addresses` / `...Topics` | −32012 EvmLargeRange, "getLogs request exceeded max allowed range" | common/json_rpc.go:L1607-L1615 |
| anything else | −32603 ServerSideException with deepest message | common/json_rpc.go:L1616-L1629 |

Response body assembly (`buildErrorResponseBody`, erpc/http_server.go:L1390-L1455): unwraps `ErrEndpointExecutionException` via `errors.As` first (L1392-L1395); unwraps single-child `ErrUpstreamsExhausted` (L1397-L1403); wire `error.code` = `jre.NormalizedCode()` (L1421); `error.data` from `details["data"]`, else `origErr` when `includeErrorDetails` and method != `eth_call` (L1425-L1433); non-JSON-RPC errors fall back to BaseError body with pseudo-code `ErrUnknown` (L1450-L1454).

## 7. Wire HTTP status switch (default 200; erpc/http_server.go:L1457-L1491 = handleErrorResponse; identical logic in determineResponseStatusCode L1280-L1317, called at L727)

| wire status | error codes (anywhere in chain via `HasErrorCode`) | file:line |
|---|---|---|
| 400 | `ErrInvalidUrlPath`, `ErrJsonRpcRequestUnmarshal`, `ErrInvalidRequest` | erpc/http_server.go:L1475-L1477 |
| 401 | `ErrAuthUnauthorized`, `ErrEndpointUnauthorized` | erpc/http_server.go:L1478-L1480 |
| 404 | `ErrProjectNotFound`, `ErrNetworkNotFound`, `ErrNetworkNotSupported` | erpc/http_server.go:L1481-L1483 |
| 429 | `ErrAuthRateLimitRuleExceeded`, `ErrProjectRateLimitRuleExceeded`, `ErrNetworkRateLimitRuleExceeded`, `ErrEndpointCapacityExceeded` — note `ErrUpstreamRateLimitRuleExceeded` is NOT listed | erpc/http_server.go:L1484-L1490 |
| 200 | everything else (JSON-RPC application errors stay 200 with error body) | erpc/http_server.go:L1473 |

## 8. Helper predicates & utilities (must be covered by KB)

| helper | behavior | file:line |
|---|---|---|
| `HasErrorCode(err, codes...)` | code match across StandardError cause chains AND joined multi-errors | common/errors.go:L2333-L2359 (+ `BaseError.HasCode` L365-L388; test `common/errors_retry_test.go:L271-L279`) |
| `IsRetryableTowardNetwork` | see legend; tests common/errors_retry_test.go:L11-L268 | common/errors.go:L2379-L2436 |
| `IsRetryableTowardsUpstream` | see legend | common/errors.go:L2438-L2498 |
| `IsCapacityIssue` | 4 rate-limit codes + EndpointCapacityExceeded | common/errors.go:L2500-L2509 |
| `IsClientError` | EndpointClientSideException, JsonRpcRequestUnmarshal, 3×GetLogsExceeded codes | common/errors.go:L2511-L2520 |
| `ClassifySeverity` | info: nil/client-error/ExecutionException; warning: non-retryable-toward-upstream, EndpointRequestCanceled, context.Canceled; critical: everything else | common/errors.go:L2522-L2549 |
| `IsClientDisconnect` | context.Canceled/DeadlineExceeded or substrings "use of closed network connection", "broken pipe", "connection reset by peer", "ECONNRESET", "EPIPE" | common/errors.go:L129-L150 |
| `ErrorSummary` + `SetErrorLabelMode` | verbose: `CodeChain: cleanedDeepestMessage`; compact: code only, with compound `Code/CauseCode` for `ErrFailsafeRetryExceeded`/`ErrUpstreamRequest`/`ErrUpstreamRequestSkipped`, and `Code/<numeric>` when cause is JsonRpcExceptionInternal | common/errors.go:L17-L114 (tests common/errors_summary_test.go:L11-L72) |
| `ErrorFingerprint` | sanitized summary, ≤256 chars | common/errors.go:L116-L124 |
| `cleanUpMessage` | redacts hashes/addresses/IPs/digits/long alphanumerics, collapses whitespace, caps 512 chars | common/errors.go:L152-L178 |
| `RetryableError` / `WithRetryableTowardNetwork` | interface + flag setter writing `Details["retryableTowardNetwork"]` | common/errors.go:L204-L217 |
| `UpstreamAwareError` | embeds `Upstream()` accessor for upstream-attributed errors | common/errors.go:L718-L724 |
| `ErrUpstreamsExhausted.SummarizeCauses` | buckets children into 19 categories (unsupported/missing/timeout/serverError/rateLimit/cbOpen/billing/transport/excluded/ignores/skips/tooLarge/auth/unsynced/validation/other/nodeTypeMismatch/cancelled/client) for the message suffix | common/errors.go:L955-L1101 |
| `BaseError` JSON marshaling | `ErrGeneric`/`ErrUnknown`/empty codes collapse to message or cause-string; joined causes serialized as array | common/errors.go:L302-L363 |
| `IsNonRetryableWriteMethod` | write methods never retried/hedged (eth_sendTransaction, eth_createAccessList, eth_submitTransaction, eth_submitWork, eth_newFilter, eth_newBlockFilter, eth_newPendingTransactionFilter); eth_sendRawTransaction deliberately excluded | architecture/evm/util.go:L9-L17 |

## 9. Edge cases checklist (each must appear in KB)

1. `ErrUpstreamsExhausted` with NO cause is non-retryable toward network (common/errors.go:L2391-L2394; test common/errors_retry_test.go:L11-L26).
2. Exhausted with ALL-missing-data children IS retryable toward network (test common/errors_retry_test.go:L51-L74); mixed children retryable if any child is (L76-L96).
3. `ErrEndpointExecutionException` non-retryable both axes via ctor flag (common/errors.go:L1918; test common/errors_retry_test.go:L140-L172) — but normalizer flips N:yes for `eth_sendRawTransaction` reverts/out-of-gas (architecture/evm/error_normalizer.go:L263-L272,L382-L391), NOT for insufficient-funds (L350-L361).
4. `ErrEndpointClientSideException` retryable toward network by default (test common/errors_retry_test.go:L175-L187); only specific normalizer branches set the false flag (§4 rows 17, 18d).
5. The retryable-flag walk descends the SINGLE-cause chain only and stops at multi-error wrappers, avoiding the order-dependent DeepSearch bug (comment common/errors.go:L2373-L2378, code L2418-L2433).
6. `HasErrorCode` finds codes nested through joined errors (common/errors.go:L2350-L2356; test common/errors_retry_test.go:L271-L279), so a "non-retryable" code anywhere in a chain poisons `IsRetryableTowardsUpstream` for the whole chain.
7. `ErrUpstreamClientInitialization` is NOT initializer-fatal by default; call sites wrap with `NewTaskFatal` for permanent causes (comment common/errors.go:L731-L737; tests common/errors_task_fatal_test.go:L34-L104).
8. Per-type `ErrorStatusCode()` methods are dead code — zero call sites; wire status comes only from the two switches in erpc/http_server.go (L1280-L1317, L1473-L1491). Per-type values still document intent (e.g. ErrEndpointMissingData=200 "many clients expect 200 + error body", common/errors.go:L2092-L2095).
9. Wire 429 mapping excludes `ErrUpstreamRateLimitRuleExceeded` even though its method says 429 (erpc/http_server.go:L1484-L1490 vs common/errors.go:L1819-L1821).
10. `ErrEndpointClientSideException.ErrorStatusCode()` returns 200 (not 400) when the cause's normalizedCode is EvmReverted(3)/CallException(−32000)/TransactionRejected(−32003) (common/errors.go:L1894-L1905).
11. `ErrEndpointServerSideException` echoes the upstream's original HTTP status (or 500 if 0) (common/errors.go:L1951-L1979).
12. Nonce-duplicate detection MUST run before generic −32003 handling because some vendors use −32003 for "already known"/"nonce too low"; "replacement transaction underpriced" deliberately stays an error (comment architecture/evm/error_normalizer.go:L297-L301).
13. "Unsupported" detection deliberately ordered AFTER "not found" rules to avoid Tenderly-style bare "not found" being misread (comment architecture/evm/error_normalizer.go:L452-L453).
14. 200-OK revert detection checks `dt[1:11]=="0x08c379a0"` because index 0 is the JSON quote char of the result string (architecture/evm/error_normalizer.go:L609-L624).
15. trace/debug timeout detection scans the raw result string instead of JSON-parsing to stay fast on ~50MB traces (comment architecture/evm/error_normalizer.go:L626-L629).
16. OP-stack "sender is over rate limit" is capacity-exceeded but N:no, since every provider proxies the same sequencer (architecture/evm/error_normalizer.go:L125-L139).
17. Provider-specific too-large messages (Alchemy/DRPC "exceed max addresses or topics per search position", Infura "N filters ... current limit is") normalize to `ErrEndpointRequestTooLarge` so getLogs splitting can engage (architecture/evm/error_normalizer_test.go:L14-L57).
18. `ErrEndpointTransportFailure` strips the endpoint URL out of the cause message (credential hygiene) (common/errors.go:L1933-L1938).
19. Compact `ErrorSummary` compound labels only for `ErrFailsafeRetryExceeded`/`ErrUpstreamRequest`/`ErrUpstreamRequestSkipped` causes; JsonRpcExceptionInternal causes render `Code/<normalizedCode>` (common/errors.go:L46-L66; tests common/errors_summary_test.go:L16-L54).
20. `TranslateToJsonRpcException` dominant-code selection skips `UpstreamRequestSkipped` and `EndpointUnsupported` children when counting (common/json_rpc.go:L1523-L1526).
21. `buildErrorResponseBody` unwraps a single-child `ErrUpstreamsExhausted` so clients see the real upstream error (erpc/http_server.go:L1397-L1403).
22. `ErrDynamicTimeoutExceeded` sentinel distinguishes failsafe-policy timeouts from parent-context (HTTP server) deadlines via `context.WithTimeoutCause` (common/errors.go:L1981-L1984).
23. `ClassifySeverity` treats `ErrEndpointRequestCanceled`/context.Canceled as warning (hedge discards), client errors + execution exceptions as info, other retryables as critical (common/errors.go:L2531-L2549).
24. BDS gRPC `TIMEOUT_ERROR`/`DeadlineExceeded` map to `ErrEndpointRequestTimeout` in the live `common` extractor but to `ErrEndpointServerSideException` in the dead `evm` duplicate — divergence to flag if the legacy one is ever revived (common/grpc_errors.go:L86-L97,L167-L178 vs architecture/evm/error_normalizer.go:L740-L751,L814-L825).
