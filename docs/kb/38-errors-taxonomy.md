# KB: Error taxonomy & JSON-RPC error normalization

> status: complete
> source-dirs: common/errors.go, common/grpc_errors.go, common/error_extractor.go, common/json_rpc.go, common/request.go, architecture/evm/error_normalizer.go, architecture/evm/util.go, architecture/evm/extractor.go, common/config.go, common/defaults.go, common/validation.go, erpc/http_server.go, upstream/upstream_executor.go, thirdparty/ (vendor hooks), common/errors_retry_test.go, common/errors_summary_test.go, common/errors_task_fatal_test.go, telemetry/metrics.go

## L1 — Capability (CTO view)

eRPC's error system is a two-layer taxonomy: an internal Go type hierarchy (`BaseError`-rooted structs with string `ErrorCode` constants) that drives retry logic, circuit-breaker scoring, and metric labels; and a translation layer that converts any internal error to a JSON-RPC-spec numeric code before it reaches the wire. Every vendor's idiosyncratic error message is normalized into one of ~20 typed internal errors by the EVM error normalizer (HTTP) or gRPC extractor (BDS), so the rest of the stack never needs to know which provider returned "Too many requests" vs "has exceeded" vs "rate limit". Retryability is a dual-axis predicate: `IsRetryableTowardsUpstream` (retry on the same upstream) and `IsRetryableTowardNetwork` (try a different upstream), and every error defaults to retryable on both axes unless explicitly opted out.

## L2 — Mechanics (staff-engineer view)

**Internal type hierarchy.** All domain errors embed `BaseError` (fields: `Code ErrorCode`, `Message string`, `Cause error`, `Details map[string]interface{}`). `BaseError` implements `StandardError` (interface with `HasCode`, `CodeChain`, `DeepestMessage`, `DeepSearch`, `GetCause`, `Base`, `MarshalZerologObject`, `Error`). Upstream-attributed errors additionally embed `UpstreamAwareError` (provides `Upstream() Upstream` accessor). `ErrJsonRpcExceptionExternal` is intentionally NOT a `StandardError` — it is a raw upstream JSON-RPC error `{code int, message, data}` that is only used as input to normalizers; it never propagates beyond the client layer.

**Error flow from upstream response.** HTTP client (`clients/http_json_rpc_client.go:L930`) calls `c.errorExtractor.Extract(r, nr, jr, c.upstream)` which invokes the EVM `ExtractJsonRpcError` function. That function first runs the vendor-specific hook (`getVendorSpecificErrorIfAny` → `vn.GetVendorSpecificErrorIfAny(req, rp, jr, details)`, `architecture/evm/error_normalizer.go:L29-L31,L878-L900`), then applies generic pattern-matching rules in fixed order (first match wins). BDS gRPC client (`clients/grpc_bds_client.go:L925`) calls `common.ExtractGrpcErrorFromGrpcStatus(st, c.upstream)` directly. Both paths produce a typed internal error wrapping an `ErrJsonRpcExceptionInternal` that carries `originalCode` (the vendor numeric code) and `normalizedCode` (an eRPC `JsonRpcErrorNumber`).

**Retryability mechanics.** `IsRetryableTowardsUpstream` checks whether the SAME upstream should be retried immediately. It returns `false` if `HasErrorCode` finds any of 10 specific codes anywhere in the cause chain (including joined multi-errors). `IsRetryableTowardNetwork` checks whether a DIFFERENT upstream can be tried. Its default is `true`; it returns `false` only in three situations: (a) an `ErrUpstreamsExhausted` with nil cause, (b) a multi-error wrapper cause where ALL children are non-retryable, or (c) any error along the linear single-cause chain has `Details["retryableTowardNetwork"]=false`. The flag walk along a single-cause chain deliberately STOPS before entering multi-error wrappers (avoiding order-dependent bugs that `DeepSearch` had). The flag is set via `WithRetryableTowardNetwork(false)` which writes `Details["retryableTowardNetwork"]` through the `RetryableError` interface.

**ErrUpstreamsExhausted** is a multi-error aggregator: its `Cause` is an `errors.Join` of all per-upstream errors. `SummarizeCauses()` buckets 19 categories and appends a human-readable suffix to the message. `TranslateToJsonRpcException` scans it to find the "dominant" error code (highest occurrence count, skipping `ErrCodeUpstreamRequestSkipped` and `ErrCodeEndpointUnsupported`; earliest error per code wins ties) and translates that single error to the wire JSON-RPC code.

**Wire translation pipeline.** `TranslateToJsonRpcException` (`common/json_rpc.go:L1501`) converts internal errors to `ErrJsonRpcExceptionInternal` with the right `normalizedCode`. `buildErrorResponseBody` (`erpc/http_server.go:L1390-L1455`) then: (1) unwraps `ErrEndpointExecutionException` via `errors.As`; (2) replaces a single-child `ErrUpstreamsExhausted` with the child; (3) calls `TranslateToJsonRpcException`; (4) produces `{"jsonrpc":"2.0","error":{"code":<normalizedCode>,"message":"..."[,"data":"..."]}}`. HTTP status is determined by one of two identical switch functions (`determineResponseStatusCode` for normal responses, `handleErrorResponse` for early errors), not by per-type `ErrorStatusCode()` methods (which are dead code — zero call sites).

**Severity classification.** `ClassifySeverity(err)` categorizes errors for the `severity` Prometheus label: `info` for nil/client errors/execution exceptions; `warning` for non-retryable-toward-upstream errors, `ErrEndpointRequestCanceled`, `context.Canceled`; `critical` for everything else. This feeds the `severity` label on `erpc_upstream_request_errors_total` and `erpc_network_failed_request_total`.

**Error label for metrics.** Both error metrics use `common.ErrorFingerprint(err)` as the `error` label value (`upstream/upstream.go:L529,L732`, `erpc/projects.go:L236`). `ErrorFingerprint` calls `ErrorSummary` then redacts hashes/addresses/IPs/digits/long alphanumerics and caps at 256 characters. The mode of `ErrorSummary` (verbose vs compact) is controlled by `metrics.errorLabelMode`, defaulting to `compact` (code-only labels, preventing high cardinality).

**TaskFatalError.** A separate, non-`StandardError` control signal (`common/errors.go:L421-L436`) that instructs the `util.Initializer` retry loop to stop retrying immediately. It is NOT embedded in domain types — call sites that know a failure is permanent (chain-ID mismatch, unsupported type) explicitly wrap with `NewTaskFatal(...)`.

## L3 — Exhaustive reference (agent view)

### Config fields

| YAML path | Type | Default | Behavior | Source |
|---|---|---|---|---|
| `metrics.errorLabelMode` | `LabelMode` string | `"compact"` (`common/defaults.go:L762-L764`) | Controls `ErrorSummary` output format. `"compact"`: code-only labels (e.g. `ErrUpstreamRequest/ErrEndpointRequestTimeout`), low cardinality. `"verbose"`: `CodeChain: cleanedDeepestMessage`, human-readable but high cardinality. Only accepted values are `"compact"` and `"verbose"` (`common/validation.go:L147-L148`). Applied globally via `common.SetErrorLabelMode`. | `common/config.go:L2536-L2550` |

No other error-specific config fields exist. Vendor-specific normalization is not configurable — it runs unconditionally for any upstream with a matching `Vendor()`.

### Behaviors & algorithms

#### Error class hierarchy

```
StandardError interface
└── BaseError (embedded in all domain errors)
    ├── UpstreamAwareError (embedded in upstream-attributed errors)
    │
    ├── Server / request lifecycle
    │   ├── ErrInvalidRequest          (ErrCodeInvalidRequest)
    │   ├── ErrInvalidUrlPath          (ErrCodeInvalidUrlPath)
    │   ├── ErrInvalidConfig
    │   ├── ErrRequestTimeout
    │   ├── ErrInternalServerError
    │   └── ErrNotImplemented
    │
    ├── Auth
    │   ├── ErrAuthUnauthorized        (ErrCodeAuthUnauthorized)
    │   └── ErrAuthRateLimitRuleExceeded (ErrCodeAuthRateLimitRuleExceeded)
    │
    ├── Project / Network
    │   ├── ErrProjectNotFound         (ErrCodeProjectNotFound)
    │   ├── ErrProjectAlreadyExists
    │   ├── ErrNetworkNotFound         (ErrCodeNetworkNotFound)
    │   ├── ErrUnknownNetworkID
    │   ├── ErrUnknownNetworkArchitecture
    │   ├── ErrInvalidEvmChainId
    │   ├── ErrFinalizedBlockUnavailable (ErrCodeFinalizedBlockUnavailable)
    │   ├── ErrNetworkInitializing     (ErrCodeNetworkInitializing)
    │   ├── ErrNetworkNotSupported     (ErrCodeNetworkNotSupported)
    │   └── ErrNetworkRequestTimeout   (ErrCodeNetworkRequestTimeout)
    │
    ├── Upstream lifecycle
    │   ├── ErrUpstreamClientInitialization
    │   ├── ErrUpstreamNetworkNotDetected
    │   ├── ErrUpstreamInitialization
    │   ├── ErrNoUpstreamsLeftToSelect (ErrCodeNoUpstreamsLeftToSelect)
    │   ├── ErrNoUpstreamsDefined
    │   └── ErrNoUpstreamsFound
    │
    ├── Upstream request routing
    │   ├── ErrUpstreamRequest         (ErrCodeUpstreamRequest) — wrapper
    │   ├── ErrUpstreamRequestSkipped  (ErrCodeUpstreamRequestSkipped)
    │   ├── ErrUpstreamBlockUnavailable (ErrCodeUpstreamBlockUnavailable)
    │   ├── ErrUpstreamMethodIgnored   (ErrCodeUpstreamMethodIgnored)
    │   ├── ErrUpstreamSyncing         (ErrCodeUpstreamSyncing)
    │   ├── ErrUpstreamShadowing       (ErrCodeUpstreamShadowing)
    │   ├── ErrUpstreamNotAllowed
    │   ├── ErrUpstreamHedgeCancelled  (ErrCodeUpstreamHedgeCancelled)
    │   ├── ErrUpstreamExcludedByPolicy (ErrCodeUpstreamExcludedByPolicy)
    │   ├── ErrUpstreamNodeTypeMismatch (ErrCodeUpstreamNodeTypeMismatch)
    │   ├── ErrUpstreamMalformedResponse
    │   └── ErrUpstreamsExhausted      (ErrCodeUpstreamsExhausted) — multi-error aggregator
    │
    ├── Rate limiting
    │   ├── ErrRateLimitBudgetNotFound
    │   ├── ErrRateLimitRuleNotFound
    │   ├── ErrProjectRateLimitRuleExceeded  (ErrCodeProjectRateLimitRuleExceeded)
    │   ├── ErrNetworkRateLimitRuleExceeded  (ErrCodeNetworkRateLimitRuleExceeded)
    │   └── ErrUpstreamRateLimitRuleExceeded (ErrCodeUpstreamRateLimitRuleExceeded)
    │
    ├── Endpoint (upstream HTTP/gRPC responses)
    │   ├── ErrEndpointUnauthorized    (ErrCodeEndpointUnauthorized)
    │   ├── ErrEndpointUnsupported     (ErrCodeEndpointUnsupported)
    │   ├── ErrEndpointClientSideException  (ErrCodeEndpointClientSideException)
    │   ├── ErrEndpointExecutionException   (ErrCodeEndpointExecutionException)
    │   ├── ErrEndpointTransportFailure     (ErrCodeEndpointTransportFailure)
    │   ├── ErrEndpointServerSideException  (ErrCodeEndpointServerSideException)
    │   ├── ErrEndpointRequestTimeout       (ErrCodeEndpointRequestTimeout)
    │   ├── ErrEndpointRequestCanceled      (ErrCodeEndpointRequestCanceled)
    │   ├── ErrEndpointCapacityExceeded     (ErrCodeEndpointCapacityExceeded)
    │   ├── ErrEndpointBillingIssue         (ErrCodeEndpointBillingIssue)
    │   ├── ErrEndpointMissingData          (ErrCodeEndpointMissingData)
    │   ├── ErrEndpointRequestTooLarge      (ErrCodeEndpointRequestTooLarge)
    │   ├── ErrEndpointContentValidation    (ErrCodeEndpointContentValidation)
    │   └── ErrEndpointNonceException       (ErrCodeEndpointNonceException)
    │
    ├── Failsafe policies
    │   ├── ErrFailsafeConfiguration
    │   ├── ErrFailsafeTimeoutExceeded  (ErrCodeFailsafeTimeoutExceeded)
    │   ├── ErrFailsafeRetryExceeded    (ErrCodeFailsafeRetryExceeded)
    │   └── ErrFailsafeCircuitBreakerOpen (ErrCodeFailsafeCircuitBreakerOpen)
    │
    ├── Consensus
    │   ├── ErrConsensusDispute         (ErrCodeConsensusDispute)
    │   └── ErrConsensusLowParticipants (ErrCodeConsensusLowParticipants)
    │
    ├── getLogs / client range errors
    │   ├── ErrGetLogsExceededMaxAllowedRange     (ErrCodeGetLogsExceededMaxAllowedRange)
    │   ├── ErrGetLogsExceededMaxAllowedAddresses (ErrCodeGetLogsExceededMaxAllowedAddresses)
    │   └── ErrGetLogsExceededMaxAllowedTopics    (ErrCodeGetLogsExceededMaxAllowedTopics)
    │
    ├── JSON-RPC (carriers)
    │   ├── ErrJsonRpcRequestUnmarshal           (ErrCodeJsonRpcRequestUnmarshal)
    │   ├── ErrJsonRpcRequestUnresolvableMethod
    │   ├── ErrJsonRpcRequestPreparation
    │   └── ErrJsonRpcExceptionInternal          (ErrCodeJsonRpcExceptionInternal) — normalized carrier
    │
    └── Store / cache
        ├── ErrInvalidConnectorDriver  (ErrCodeInvalidConnectorDriver)
        ├── ErrRecordNotFound          (ErrCodeRecordNotFound)
        └── ErrRecordExpired           (ErrCodeRecordExpired)

Not StandardError (standalone types):
├── ErrJsonRpcExceptionExternal — raw upstream JSON-RPC error {code int, message, data}
├── TaskFatalError              — initializer control signal; IsTaskFatal()=true
└── ErrDynamicTimeoutExceeded   — sentinel errors.New for context.WithTimeoutCause
```

#### Complete error code table with retryability and HTTP status

Legend:
- **U** = `IsRetryableTowardsUpstream` result (`common/errors.go:L2438-L2498`)
- **N** = `IsRetryableTowardNetwork` result (`common/errors.go:L2379-L2436`)
- **wire status** = actual HTTP status on the wire from `determineResponseStatusCode`/`handleErrorResponse` (`erpc/http_server.go:L1280-L1317, L1457-L1491`)
- **method status** = `ErrorStatusCode()` method value (dead code — zero call sites)

| # | Error code | U | N | Wire HTTP | JSON-RPC code (wire) | Notes | Source |
|---|---|---|---|---|---|---|---|
| 1 | `ErrInvalidRequest` | yes | yes | 400 | −32602 | invalid body/headers | `common/errors.go:L444` |
| 2 | `ErrInvalidUrlPath` | yes | yes | 400 | −32602 | malformed URL path | `common/errors.go:L462` |
| 3 | `ErrInvalidConfig` | yes | yes | 200 | −32603 | startup-only | `common/errors.go:L485` |
| 4 | `ErrRequestTimeout` | yes | yes | 200 | −32603 | pre-upstream timeout | `common/errors.go:L496` |
| 5 | `ErrInternalServerError` | yes | yes | 200 | −32603 | internal | `common/errors.go:L514` |
| 6 | `ErrAuthUnauthorized` | yes | yes | 401 | −32016 | auth strategy rejected | `common/errors.go:L527` |
| 7 | `ErrAuthRateLimitRuleExceeded` | no (capacity) | yes | 429 | −32005 | auth-level budget | `common/errors.go:L547` |
| 8 | `ErrProjectNotFound` | yes | yes | 404 | −32603 | project not configured | `common/errors.go:L576` |
| 9 | `ErrProjectAlreadyExists` | n/a | n/a | — | — | startup duplicate | `common/errors.go:L596` |
| 10 | `ErrNetworkNotFound` | yes | yes | 404 | −32603 | network not configured | `common/errors.go:L609` |
| 11 | `ErrUnknownNetworkID` | yes | yes | 200 | −32603 | | `common/errors.go:L634` |
| 12 | `ErrUnknownNetworkArchitecture` | yes | yes | 200 | −32603 | | `common/errors.go:L652` |
| 13 | `ErrNotImplemented` | yes | yes | 200 | −32603 | | `common/errors.go:L670` |
| 14 | `ErrInvalidEvmChainId` | yes | yes | 200 | −32603 | | `common/errors.go:L685` |
| 15 | `ErrFinalizedBlockUnavailable` | yes | yes | 200 | −32603 | block finality check | `common/errors.go:L700` |
| 16 | `ErrUpstreamClientInitialization` | yes | yes | — | — | NOT fatal unless wrapped in `TaskFatalError` | `common/errors.go:L745` |
| 17 | `ErrUpstreamRequest` | inherited | inherited | — | inherited | wrapper carrying durationMs/attempts/retries/hedges | `common/errors.go:L760` |
| 18 | `ErrUpstreamMalformedResponse` | yes | yes | 200 | −32603 | | `common/errors.go:L846` |
| 19 | `ErrUpstreamsExhausted` | recursive | recursive | 200 | from dominant child | multi-error aggregator | `common/errors.go:L869` |
| 20 | `ErrNoUpstreamsLeftToSelect` | yes | yes | 200 | −32603 | per-upstream states in details | `common/errors.go:L1186` |
| 21 | `ErrNoUpstreamsDefined` | yes | yes | 200 | −32603 | **Dead code — `NewErrNoUpstreamsDefined` has zero production call sites.** Config validation uses a plain `fmt.Errorf` for the empty-upstream-list check instead; the actual empty-network runtime case uses `ErrNoUpstreamsFound` (#22). Cross-ref: `08-upstreams-lifecycle.md:L235`. | `common/errors.go:L1222` |
| 22 | `ErrNoUpstreamsFound` | yes | yes | 200 | −32603 | | `common/errors.go:L1238` |
| 23 | `ErrNetworkInitializing` | yes | yes | 200 | −32603 | retry shortly | `common/errors.go:L1254` |
| 24 | `ErrNetworkNotSupported` | yes | yes | 404 | −32603 | no provider covers network | `common/errors.go:L1275` |
| 25 | `ErrUpstreamNetworkNotDetected` | yes | yes | — | — | **Dead code — `NewErrUpstreamNetworkNotDetected` has zero production call sites in the current codebase.** The constructor exists for future use if network-detection logic is re-introduced. Cross-ref: `08-upstreams-lifecycle.md:L231`. | `common/errors.go:L1303` |
| 26 | `ErrUpstreamInitialization` | yes | yes | — | — | | `common/errors.go:L1318` |
| 27 | `ErrUpstreamRequestSkipped` | no | yes | 200 | −32603 | permanent skip | `common/errors.go:L1330` |
| 28 | `ErrUpstreamBlockUnavailable` | yes | yes | 200 | −32603 | transient, unlike Skipped | `common/errors.go:L1354` |
| 29 | `ErrUpstreamMethodIgnored` | no | yes | 200 | −32601 | `ignoreMethods` config | `common/errors.go:L1377` |
| 30 | `ErrUpstreamSyncing` | yes | yes | 200 | −32603 | | `common/errors.go:L1398` |
| 31 | `ErrUpstreamShadowing` | yes | yes | — | — | shadow mode | `common/errors.go:L1418` |
| 32 | `ErrUpstreamNotAllowed` | yes | yes | — | — | use-upstream directive | `common/errors.go:L1437` |
| 33 | `ErrUpstreamHedgeCancelled` | yes | yes | — | — | hedge discard | `common/errors.go:L1449` |
| 34 | `ErrResponseWriteLock` | yes | yes | — | — | **Vestigial/dead code — `NewErrResponseWriteLock` has zero production call sites.** The concurrent-write-contention semantics this error encoded were superseded when the hedge response model was refactored in 2024; the contemporary mechanism for discard of racing hedge responses is `ErrUpstreamHedgeCancelled` (#33). Cross-ref: `33-batching-multiplexing.md:L208`. | `common/errors.go:L1469` |
| 35 | `ErrJsonRpcRequestUnmarshal` | no | no | 400 | −32700 | retryableTowardNetwork=false always | `common/errors.go:L1486` |
| 36 | `ErrJsonRpcRequestUnresolvableMethod` | yes | no | 200 | −32603 | retryableTowardNetwork=false | `common/errors.go:L1534` |
| 37 | `ErrJsonRpcRequestPreparation` | yes | no | 200 | −32603 | **Dead code — `NewErrJsonRpcRequestPreparation` has zero non-test production call sites.** N=false by default (overridable via `details` arg — the only constructor in the taxonomy where N default is caller-overridable). See extended note in §Behaviors below. | `common/errors.go:L1544-L1567` |
| 38 | `ErrFailsafeConfiguration` | n/a | n/a | — | — | startup | `common/errors.go:L1578` |
| 39 | `ErrFailsafeTimeoutExceeded` | yes | yes | 200 | −32603 | scope in message | `common/errors.go:L1588` |
| 40 | `ErrFailsafeRetryExceeded` | inherited | inherited | — | inherited | scope in message | `common/errors.go:L1627` |
| 41 | `ErrFailsafeCircuitBreakerOpen` | no | yes | 200 | −32603 | | `common/errors.go:L1667` |
| 42 | `ErrRateLimitBudgetNotFound` | n/a | n/a | — | — | config | `common/errors.go:L1714` |
| 43 | `ErrRateLimitRuleNotFound` | n/a | n/a | — | — | **Dead code — `NewErrRateLimitRuleNotFound` has zero production call sites; reserved for future use.** Cross-ref: `16-rate-limiters.md`. | `common/errors.go:L1728` |
| 44 | `ErrProjectRateLimitRuleExceeded` | no (capacity) | yes | 429 | −32005 | | `common/errors.go:L1740` |
| 45 | `ErrNetworkRateLimitRuleExceeded` | no (capacity) | yes | 429 | −32005 | | `common/errors.go:L1762` |
| 46 | `ErrNetworkRequestTimeout` | yes | yes | 200 | −32603 | network-scope timeout | `common/errors.go:L1785` |
| 47 | `ErrUpstreamRateLimitRuleExceeded` | no (capacity) | yes | 200 | −32005 | NOT in wire 429 list | `common/errors.go:L1803` |
| 48 | `ErrUpstreamExcludedByPolicy` | yes | yes | — | — | **Dead code — `NewErrUpstreamExcludedByPolicy` has no production call sites (only a test fixture).** The policy engine achieves upstream exclusion structurally (by not including upstreams in the candidate set), not by emitting this error. Cross-ref: `22-selection-policies-scoring.md:L535`. | `common/errors.go:L1825` |
| 49 | `ErrEndpointUnauthorized` | no | yes | 401 | −32016 | provider 401/403 | `common/errors.go:L1846` |
| 50 | `ErrEndpointUnsupported` | no | yes | 200 | −32601 | | `common/errors.go:L1864` |
| 51 | `ErrEndpointClientSideException` | yes | yes (default) | 200 | from cause | N:no set by some normalizer branches | `common/errors.go:L1882` |
| 52 | `ErrEndpointExecutionException` | no | no | 200 | 3 or −32003 | N:yes overridden for eth_sendRawTransaction reverts | `common/errors.go:L1909` |
| 53 | `ErrEndpointTransportFailure` | yes | yes | — | −32603 | URL stripped from cause msg | `common/errors.go:L1931` |
| 54 | `ErrEndpointServerSideException` | yes | yes | 200 | from cause | `originalStatusCode` stored for observability | `common/errors.go:L1960` |
| 55 | `ErrEndpointRequestTimeout` | yes | yes | 200 | −32015 | | `common/errors.go:L1988` |
| 56 | `ErrEndpointRequestCanceled` | yes | yes | — | — | severity=warning | `common/errors.go:L2009` |
| 57 | `ErrEndpointCapacityExceeded` | no (capacity) | yes | 429 | −32005 | | `common/errors.go:L2023` |
| 58 | `ErrEndpointBillingIssue` | no | yes | 200 | −32005 | | `common/errors.go:L2041` |
| 59 | `ErrEndpointMissingData` | yes | yes | 200 | −32014 | details: latestBlock/finalizedBlock/maxAvailableRecentBlocks | `common/errors.go:L2062` |
| 60 | `ErrUpstreamNodeTypeMismatch` | yes | yes | — | −32603 | archive vs full node | `common/errors.go:L2099` |
| 61 | `ErrEndpointRequestTooLarge` | no | yes | 200 | −32012 | complaint: `evm_block_range` or `evm_addresses` | `common/errors.go:L2117` |
| 62 | `ErrJsonRpcExceptionInternal` | wrapper | wrapper | — | `normalizedCode` field | internal carrier for normalized codes | `common/errors.go:L2179` |
| 63 | `ErrJsonRpcExceptionExternal` | n/a (input) | n/a | — | — | raw upstream JSON-RPC; normalizer input only | `common/errors.go:L2225` |
| 64 | `ErrInvalidConnectorDriver` | n/a | n/a | — | — | config | `common/errors.go:L2281` |
| 65 | `ErrRecordNotFound` | yes | yes | — | — | cache miss | `common/errors.go:L2297` |
| 66 | `ErrRecordExpired` | yes | yes | — | — | cache TTL exceeded | `common/errors.go:L2315` |
| 67 | `ErrConsensusDispute` | multi | multi | 200 | −32603 | participants in details; retryability: `IsRetryableTowardNetwork` returns `true` if **ANY** child error is retryable (multi-error children rule at `common/errors.go:L2397-L2410`); `IsRetryableTowardsUpstream` recursively applies the same any-child rule over the `errors.Join` children. "multi" in both U/N columns means the result is determined by the children, not a fixed value. | `common/errors.go:L2559` |
| 68 | `ErrConsensusLowParticipants` | multi | multi | 200 | −32603 | participant summary in DeepestMessage; same multi-error children retryability rule as `ErrConsensusDispute` — `true` if ANY child is retryable. | `common/errors.go:L2593` |
| 69 | `ErrGetLogsExceededMaxAllowedRange` | yes (IsClientError) | yes | 200 | −32012 | eRPC cap, not upstream | `common/errors.go:L2658` |
| 70 | `ErrGetLogsExceededMaxAllowedAddresses` | yes (IsClientError) | yes | 200 | −32012 | | `common/errors.go:L2679` |
| 71 | `ErrGetLogsExceededMaxAllowedTopics` | yes (IsClientError) | yes | 200 | −32012 | | `common/errors.go:L2700` |
| 72 | `ErrEndpointContentValidation` | no | yes | 200 | −32603 | try different upstream | `common/errors.go:L2727` |
| 73 | `ErrEndpointNonceException` | yes | no | 200 | −32003 | reason: `already_known` or `nonce_too_low`; idempotency | `common/errors.go:L2763` |

#### JSON-RPC numeric error codes (`JsonRpcErrorNumber`)

All defined at `common/errors.go:L2151-L2174`.

| Constant | Value | Category | Notes |
|---|---|---|---|
| `JsonRpcErrorUnknown` | −99999 | eRPC internal | unclassified fallback |
| `JsonRpcErrorCallException` | −32000 | standard | generic call exception |
| `JsonRpcErrorTransactionRejected` | −32003 | standard | transaction rejected; also used for nonce exceptions |
| `JsonRpcErrorClientSideException` | −32600 | standard | invalid request |
| `JsonRpcErrorUnsupportedException` | −32601 | standard | method not found/unsupported |
| `JsonRpcErrorInvalidArgument` | −32602 | standard | invalid params |
| `JsonRpcErrorServerSideException` | −32603 | standard | internal error; eRPC default for unmapped errors |
| `JsonRpcErrorParseException` | −32700 | standard | JSON parse error |
| `JsonRpcErrorEvmReverted` | 3 | de-facto | EVM execution reverted; positive code, de-facto standard |
| `JsonRpcErrorCapacityExceeded` | −32005 | eRPC-normalized | rate-limit / capacity; maps all rate-limit types |
| `JsonRpcErrorEvmLargeRange` | −32012 | eRPC-normalized | request range too large (block range, addresses) |
| `JsonRpcErrorMissingData` | −32014 | eRPC-normalized | data not on this node |
| `JsonRpcErrorNodeTimeout` | −32015 | eRPC-normalized | node-level timeout |
| `JsonRpcErrorUnauthorized` | −32016 | eRPC-normalized | unauthorized/forbidden |

#### Wire HTTP status mapping

Evaluated by two identical switch functions (`erpc/http_server.go:L1280-L1317` = `determineResponseStatusCode`, called at `L727`; `erpc/http_server.go:L1457-L1491` = `handleErrorResponse` for early errors). `HasErrorCode` traverses the full cause chain including joined multi-errors.

| Wire HTTP status | Triggered by (HasErrorCode anywhere in chain) | Source |
|---|---|---|
| 400 | `ErrInvalidUrlPath`, `ErrJsonRpcRequestUnmarshal`, `ErrInvalidRequest` | `erpc/http_server.go:L1475-L1477` |
| 401 | `ErrAuthUnauthorized`, `ErrEndpointUnauthorized` | `erpc/http_server.go:L1478-L1480` |
| 404 | `ErrProjectNotFound`, `ErrNetworkNotFound`, `ErrNetworkNotSupported` | `erpc/http_server.go:L1481-L1483` |
| 429 | `ErrAuthRateLimitRuleExceeded`, `ErrProjectRateLimitRuleExceeded`, `ErrNetworkRateLimitRuleExceeded`, `ErrEndpointCapacityExceeded` | `erpc/http_server.go:L1484-L1490` |
| 200 | everything else | `erpc/http_server.go:L1473` |

**Important:** `ErrUpstreamRateLimitRuleExceeded` is NOT in the 429 list despite its method returning 429. It returns 200 on the wire.

#### HTTP/JSON-RPC vendor error normalization (`ExtractJsonRpcError`)

Entry point: `architecture/evm/error_normalizer.go:L20`. Triggered when `jr.Error != nil` OR HTTP status > 299 (`L21`). If status > 299 with no JSON-RPC error body, a synthetic `−32603` external error is created (`L32-L38`). `details` always populated with `statusCode` and useful response headers (`L23-L24`). String `data` with `"Reverted "` prefix is stripped (`L49-L53`). Data appended to `msg` for text-matching rules (`L55-L64`). Rules evaluated in fixed order — **first match wins**:

| Rule # | Trigger condition | Normalized error | Normalized code | Retry flags | Source |
|---|---|---|---|---|---|
| 0 | Vendor hook `vn.GetVendorSpecificErrorIfAny(...)` returns non-nil | vendor-defined | vendor-defined | vendor-defined | `L29-L31, L878-L900` |
| 1 | Block-range-too-large strings: "Try with this block range", "block range is too wide", "range too large", "exceeds the range", "max block range", "logs over more", "response size should not", "returned more than", "exceeds max results", "range is too large", "too large, max is", "response too large", "query exceeds limit", "limit the query to", "maximum block range", "range limit exceeded", "too many results", "try paginating", ("maximum"+"blocks distance"), "eth_getLogs is limited", "this block range should work", "Max range" | `ErrEndpointRequestTooLarge` (complaint `evm_block_range`) | −32012 | U:no | `L70-L101` |
| 2 | Address-count-too-large: "specify less number of address", "addresses or topics per search position" (Alchemy/DRPC), "filters"+"current limit is" (Infura) | `ErrEndpointRequestTooLarge` (complaint `evm_addresses`) | −32012 | U:no | `L102-L117` |
| 3 | "sender is over rate limit" (OP sequencer) | `ErrEndpointCapacityExceeded` | −32005 | U:no, **N:no** (all providers hit same sequencer) | `L125-L139` |
| 4 | status 402, "reached the free tier", "Monthly capacity limit", "limit for your current plan", "/billing" | `ErrEndpointBillingIssue` | −32005 | U:no | `L141-L155` |
| 5 | status 429, "requests limited to", "has exceeded", "Exceeded the quota", "Too many requests", "Too Many Requests", "under too much load", "request limit reached", "No server available", "reached the quota", "upgrade your tier", "rate limit", "too many requests", "limit exceeded" | `ErrEndpointCapacityExceeded` | −32005 | U:no, N:yes | `L156-L179` |
| 6 | Block-tag unsupported: "pending block is not available", "pending block not found", "Pending block not found", "safe block not found", "Safe block not found", "finalized block not found", "Finalized block not found", "finalized is not a supported", "pending is not a supported", "safe is not a supported", "not a supported commitment", "malformed blocknumber" | `ErrEndpointClientSideException` | −32600 | U:yes, N:yes (deliberately retryable) | `L185-L209` |
| 7 | Missing data (`IsMissingDataError`): "missing trie node", "header not found", "could not find block", "unknown block", "Unknown block", "height must be less than or equal", "invalid blockhash finalized", "Expect block number from id", "block not found", "Block not found", "block height passed is invalid", "cannot query unfinalized", "height is not available", "genesis is not traceable", "could not find FinalizeBlock", "no historical rpc", ("blocks specified"+"cannot be found"), "transaction not found", "cannot find transaction", "after last accepted block", "No state available", ("historical state"+("is not available"\|"unavailable")), "trie does not", "greater than latest", "not currently canonical", "requested data is not available" | `ErrEndpointMissingData` | −32014 | U:yes, N:yes | `L215-L226`; patterns at `architecture/evm/util.go:L19-L49` |
| 8 | "execution timeout" | `ErrEndpointServerSideException` | −32015 | U:yes, N:yes | `L232-L244` |
| 9 | Reverts (case-sensitive): "reverted", "VM execution error", "transaction: revert", "VM Exception"; lower("intrinsic gas too high") | `ErrEndpointExecutionException` | 3 (EvmReverted) | U:no, N:no — **EXCEPT** method==`eth_sendRawTransaction` ⇒ N:yes | `L249-L273` |
| 10 | "EVM error: InvalidJump" (Berachain) — message rewritten to "revert: invalid jump destination" | `ErrEndpointExecutionException` | 3 | same eth_sendRawTransaction N:yes override | `L275-L294` |
| 11 | Duplicate tx (lowercased): "already known", "known transaction", "already imported", "transaction already in mempool", "tx already in mempool", "already in the mempool", "transaction already exists", "already have transaction", "already exists in mempool" | `ErrEndpointNonceException` (reason `already_known`) | −32003 | U:yes, N:no | `L303-L324` |
| 12 | Nonce conflict (lowercased): "nonce too low", "nonce is too low", "nonce has already been used" | `ErrEndpointNonceException` (reason `nonce_too_low`) | −32003 | U:yes, N:no | `L326-L341` |
| 13 | "insufficient funds", "insufficient balance" | `ErrEndpointExecutionException` | −32003 | U:no, N:no (NO sendRawTransaction override) | `L350-L361` |
| 14 | code==−32003, or "out of gas", "gas too low", "IntrinsicGas" | `ErrEndpointExecutionException` | −32003 | U:no, N:no — **EXCEPT** `eth_sendRawTransaction` ⇒ N:yes | `L368-L392` |
| 15a | "not found"/"does not exist"/"not available"/"is disabled"/"is not available" AND ("Method"\|"method"\|"Module"\|"module") | `ErrEndpointUnsupported` | −32601 | U:no, N:yes | `L398-L414` |
| 15b | Same outer AND ("header"\|"block"\|"Header"\|"Block"\|"transaction"\|"Transaction"\|"state"\|"State") | `ErrEndpointMissingData` | −32014 | U:yes, N:yes | `L415-L432` |
| 15c | Same outer, neither 15a nor 15b | `ErrEndpointClientSideException` | −32600 | U:yes, N:yes | `L433-L445` |
| 16 | status 415 or 405, code==−32601/−32004/−32001, "Unsupported method", "not supported", "method is not whitelisted", "not allowed to access method", "is not included in your current plan"; message rewritten to "The method %s does not exist/is not available"; original in `details.vendorMessage` | `ErrEndpointUnsupported` | −32601 | U:no, N:yes | `L455-L477` |
| 17 | Malformed tx (lowercased): "typed transaction too short", "transaction too short", "rlp: expected input list", "rlp: input string too short", "rlp: value size exceeds", "invalid transaction", "transaction type not supported" | `ErrEndpointClientSideException` | −32602 | U:yes, **N:no** | `L484-L500` |
| 18a | "tx of type", "invalid type: map, expected BlockNumber..." (Envio) | `ErrEndpointClientSideException` | −32601 | U:yes, N:yes | `L510-L520` |
| 18b | code==−32600 AND data.message contains "validation errors in batch" | `ErrEndpointClientSideException` | −32000 | U:yes, N:yes (caller may split batch) | `L521-L537` |
| 18c | code==−32602 or −32600 (generic) | `ErrEndpointClientSideException` | −32602 | U:yes, N:yes | `L538-L548` |
| 18d | "param is required", "Invalid Request", "validation errors", "invalid argument", "invalid params", "Bad request input parameters" | `ErrEndpointClientSideException` | −32602 | U:yes, **N:no** (likely user mistake) | `L549-L566` |
| 19 | status 401 or 403, "not allowed to access", "invalid api key", "key is inactive", "unauthorized" | `ErrEndpointUnauthorized` | −32016 | U:no, N:yes | `L573-L587` |
| 20 | Fallback (everything else) | `ErrEndpointServerSideException` | passthrough original vendor code | U:yes, N:yes | `L592-L602` |
| 21 | 200-OK revert payload: result begins with `0x08c379a0` (Error(string) ABI selector; `dt[1:11]` check skips JSON quote char) | `ErrEndpointExecutionException` ("transaction reverted", revert data in details) | 3 | U:no, N:no | `L609-L624` |
| 22 | `trace_*`/`debug_*`/`eth_trace*` method AND result body contains "execution timeout" (raw string scan, no JSON parse) | `ErrEndpointServerSideException` | −32015 | U:yes, N:yes | `L626-L652` |

#### Vendor-specific error hooks

Each vendor in `thirdparty/` implements `GetVendorSpecificErrorIfAny` (`common/vendors.go:L15`). This hook fires BEFORE all generic rules (`architecture/evm/error_normalizer.go:L29-L31`). Implemented by: Alchemy, DRPC, Infura, BlastAPI, OnFinality, Blockdaemon, Pimlico, Chainstack, Repository, BlockPi, Envio, Quicknode, Dwellir, Conduit, Thirdweb, Tenderly, eRPC self-vendor. These add vendor-specific billing/capacity/auth patterns that generic rules cannot reliably detect.

#### gRPC/BDS error normalization (`ExtractGrpcErrorFromGrpcStatus`)

Entry point: `common/grpc_errors.go:L12`. Nil status or `codes.OK` returns nil. `details` always populated with `grpcCode`, `grpcMessage`, `upstreamId`, plus BDS `bdsErrorCode`/cause/details when present (`L17-L37`). BDS error code checked FIRST (`L42-L122`), then gRPC code (`L124-L224`):

| Trigger | Normalized error | Normalized code | Retry flags | Source |
|---|---|---|---|---|
| BDS `UNSUPPORTED_BLOCK_TAG`, `UNSUPPORTED_METHOD` | `ErrEndpointUnsupported` | −32601 | U:no | `L44-L54` |
| BDS `RANGE_OUTSIDE_AVAILABLE` | `ErrEndpointMissingData` | −32014 | U:yes, N:yes | `L55-L65` |
| BDS `INVALID_PARAMETER`, `INVALID_REQUEST` | `ErrEndpointClientSideException` | −32602 | N:no (explicit flag) | `L66-L75` |
| BDS `RATE_LIMITED` | `ErrEndpointCapacityExceeded` | −32005 | U:no | `L76-L85` |
| BDS `TIMEOUT_ERROR` | `ErrEndpointRequestTimeout` (dur=0) | −32015 | U:yes, N:yes | `L86-L97` |
| BDS `RANGE_TOO_LARGE` | `ErrEndpointRequestTooLarge` (complaint `evm_block_range`) | −32012 | U:no | `L98-L108` |
| BDS `INTERNAL_ERROR` | `ErrEndpointServerSideException` | −32603 | U:yes, N:yes | `L109-L120` |
| gRPC `Canceled` | `ErrEndpointRequestCanceled` (codes 0/0) | — | severity warning | `L125-L136` |
| gRPC `Unimplemented` | `ErrEndpointUnsupported` | −32601 | U:no | `L137-L146` |
| gRPC `InvalidArgument` | `ErrEndpointClientSideException` | −32602 | N:no (explicit flag) | `L147-L156` |
| gRPC `ResourceExhausted` | `ErrEndpointCapacityExceeded` | −32005 | U:no | `L157-L166` |
| gRPC `DeadlineExceeded` | `ErrEndpointRequestTimeout` (dur=0) | −32015 | U:yes, N:yes | `L167-L178` |
| gRPC `Unauthenticated`, `PermissionDenied` | `ErrEndpointUnauthorized` | −32016 | U:no | `L179-L188` |
| gRPC `NotFound`, `OutOfRange` | `ErrEndpointMissingData` | −32014 | U:yes, N:yes | `L189-L199` |
| gRPC `Internal`, `Unknown`, `Unavailable` | `ErrEndpointServerSideException` | −32603 | U:yes, N:yes | `L200-L211` |
| gRPC default (any other code) | `ErrEndpointServerSideException` | −32603 | U:yes, N:yes | `L212-L224` |

#### Internal error → wire JSON-RPC code (`TranslateToJsonRpcException`)

Entry: `common/json_rpc.go:L1501`. Called from `buildErrorResponseBody` (`erpc/http_server.go:L1412`).

1. If `ErrUpstreamsExhausted`: scan children for dominant code (highest count, `ErrCodeUpstreamRequestSkipped` and `ErrCodeEndpointUnsupported` excluded from counting; earliest error per code wins ties). Replace with dominant child. (`L1502-L1536`)
2. If chain already contains `ErrCodeJsonRpcExceptionInternal`: return as-is (upstream-derived normalized code preserved). (`L1538-L1542`)
3. Internal rate-limit codes → −32005, message "rate-limit exceeded". (`L1547-L1561`)
4. `ErrCodeAuthUnauthorized` → −32016, message "unauthorized". (`L1562-L1573`)
5. `ErrCodeUpstreamMethodIgnored` → −32601, message "method ignored by upstream: \<deepest msg\>". (`L1574-L1588`)
6. `ErrCodeJsonRpcRequestUnmarshal` → −32700, message "failed to parse json-rpc request". (`L1589-L1597`)
7. `ErrCodeInvalidRequest`/`ErrCodeInvalidUrlPath` → −32602, message "invalid request url and/or body". (`L1598-L1606`)
8. GetLogs exceeded codes → −32012, message "getLogs request exceeded max allowed range". (`L1607-L1615`)
9. Fallback → −32603, deepest message. (`L1616-L1629`)

#### Error summary format (`ErrorSummary` / `ErrorFingerprint`)

`ErrorSummary` (`common/errors.go:L38-L114`) formats errors for labels and tracing. Mode controlled by package-level `errorLabelMode` (set via `SetErrorLabelMode`, driven by `metrics.errorLabelMode` config):

**Compact mode** (`ErrorLabelModeCompact`, default since `common/defaults.go:L762-L763`):
- `StandardError`: just `string(be.Code)`. EXCEPTION for `ErrFailsafeRetryExceeded`, `ErrUpstreamRequest`, `ErrUpstreamRequestSkipped`: if cause is a `StandardError`, appends `/CauseCode`. If cause is `ErrJsonRpcExceptionInternal`, appends `/<normalizedCode>` (numeric). (`L46-L66`)
- `interface{ Unwrap() []error }` (non-StandardError multi): if one child, recurse; else `CodeChain` of each joined, comma-separated.
- `context.DeadlineExceeded` → `"ContextDeadlineExceeded"`; `context.Canceled` → `"ContextCanceled"`; other errors → `"GenericError"`; strings → `"StringError"`; else → `"UnknownError"`.

**Verbose mode** (`ErrorLabelModeVerbose`):
- `StandardError`: `"CodeChain: cleanedDeepestMessage"` where `CodeChain` is `"A <- B <- C"` format.
- Other errors: `cleanUpMessage(err.Error())`.

`ErrorFingerprint` (`L116-L124`) calls `ErrorSummary` then: replaces `[^a-zA-Z0-9\s_\.-]+` with space, collapses multiple spaces, caps at 256 chars. Used as the `error` label value in `erpc_upstream_request_errors_total` and `erpc_network_failed_request_total`.

`cleanUpMessage` (`L162-L178`): redacts 0x-prefixed hashes (`0xREDACTED`), tx hash patterns, trie node patterns, execution revert addresses, IP addresses → `X.X.X.X`, 2+ digit numbers → `XX`, newlines → space, collapses whitespace, 31+ alphanumeric strings → `XX`, caps at 512 chars.

#### `ErrUpstreamsExhausted.SummarizeCauses()` bucket categories

`common/errors.go:L955-L1101`. Iterates joined children and buckets into 19 categories. Category labels appended to error message as `"(N upstream X, M upstream Y)"`:

| Category | Codes detected |
|---|---|
| unsupported | `ErrCodeEndpointUnsupported` |
| missing | `ErrCodeEndpointMissingData` |
| rateLimit | `ErrCodeEndpointCapacityExceeded`, `ErrCodeUpstreamRateLimitRuleExceeded` |
| billing | `ErrCodeEndpointBillingIssue` |
| cbOpen | `ErrCodeFailsafeCircuitBreakerOpen` |
| timeout | `context.DeadlineExceeded`, `ErrCodeEndpointRequestTimeout`, `ErrCodeNetworkRequestTimeout`, `ErrCodeFailsafeTimeoutExceeded` |
| serverError | `ErrCodeEndpointServerSideException` |
| cancelled | `ErrCodeUpstreamHedgeCancelled` |
| client | `ErrCodeEndpointClientSideException`, `ErrCodeJsonRpcRequestUnmarshal`, `ErrCodeInvalidRequest`, `ErrCodeInvalidUrlPath` |
| transport | `ErrCodeEndpointTransportFailure` |
| unsynced | `ErrCodeUpstreamSyncing`, `ErrCodeUpstreamBlockUnavailable` |
| excluded | `ErrCodeUpstreamExcludedByPolicy` |
| nodeTypeMismatch | `ErrCodeUpstreamNodeTypeMismatch` |
| ignores | `ErrCodeUpstreamMethodIgnored` |
| skips | `ErrCodeUpstreamRequestSkipped` |
| tooLarge | `ErrCodeEndpointRequestTooLarge`, `ErrCodeGetLogsExceededMaxAllowedRange`, `ErrCodeGetLogsExceededMaxAllowedAddresses`, `ErrCodeGetLogsExceededMaxAllowedTopics` |
| auth | `ErrCodeEndpointUnauthorized` |
| validation | `ErrCodeEndpointContentValidation` |
| other | anything not matched above |

#### `IsNonRetryableWriteMethod` — write-method guard

`architecture/evm/util.go:L7-L17`. Returns `true` for methods that MUST NOT be retried or hedged because they mutate state non-idempotently:

```
eth_sendTransaction
eth_createAccessList
eth_submitTransaction
eth_submitWork
eth_newFilter
eth_newBlockFilter
eth_newPendingTransactionFilter
```

**`eth_sendRawTransaction` is deliberately excluded.** The comment at `architecture/evm/util.go:L8` explains: it supports idempotency handling (the "already known"/"nonce too low" `ErrEndpointNonceException` path lets eRPC detect and safely retry/hedge sendRawTransaction without double-submission risk).

**Call sites** — used in two places in `upstream/upstream_executor.go`:
1. **Retry guard** (`L243`): if the method is a non-retryable write, `isRetryableTowardsUpstream` returns `false` even if the error would otherwise qualify. This prevents re-sending a state-mutating call to the same upstream.
2. **Hedge guard** (`L281`): if the method is a non-retryable write, the hedge path is bypassed entirely and the call goes straight to `callBreakerWithTimeout`. This prevents parallel duplicate submissions.

**Interaction with `ErrEndpointExecutionException` N-flag override.** For `eth_sendRawTransaction`, the normalizer overrides `retryableTowardNetwork=true` for revert/out-of-gas results (rules 9 and 14 in `architecture/evm/error_normalizer.go`). This is safe precisely because `eth_sendRawTransaction` is NOT in `IsNonRetryableWriteMethod` — the two predicates are complementary.

#### `ErrJsonRpcRequestUnresolvableMethod` (#36) and `ErrJsonRpcRequestPreparation` (#37) — emission context

**`ErrJsonRpcRequestUnresolvableMethod`** (`common/errors.go:L1527-L1542`)

Emitted exclusively by `NormalizedRequest` in `common/request.go`:
- `JsonRpcRequest()` method (`L957`): after successfully unmarshalling the body into a `JsonRpcRequest` struct, if `rpcReq.Method == ""` (method field absent or empty), this error is returned.
- `Method()` method (`L991`): when both the cached `r.method` field and `r.jsonRpcRequest` are unset and `r.body` is empty/nil — i.e., no method can be resolved from any source.

`retryableTowardNetwork` is hardcoded to `false` in the constructor's `Details` map (`L1538`). This is NOT overridable by callers. `IsRetryableTowardsUpstream` defaults to `true` (no explicit false code, no capacity issue). No `ErrorStatusCode()` method; falls through to 200 on the wire.

Wire JSON-RPC code: falls through to `TranslateToJsonRpcException` step 9 (fallback −32603) because no specific rule matches `ErrCodeJsonRpcRequestUnresolvableMethod` in steps 1–8. Note: `ErrCodeJsonRpcRequestUnmarshal` is handled at step 6, but `ErrCodeJsonRpcRequestUnresolvableMethod` is NOT — it gets −32603 (internal error), not −32700 or −32602.

**`ErrJsonRpcRequestPreparation`** (`common/errors.go:L1544-L1567`)

A placeholder error type for failures that occur while building/preparing a JSON-RPC request before sending it upstream. As of the current codebase, `NewErrJsonRpcRequestPreparation` has zero call sites in non-test production code — the constructor exists to be used by future method-handler or request-transformation code that needs to signal a preparation failure.

`retryableTowardNetwork` defaults to `false` in the constructor (`L1562-L1563`), but the check uses `if _, ok := err.Details["retryableTowardNetwork"]; !ok` — meaning callers CAN override by passing `details` that already contain the key. This is the only error in the taxonomy where the default N=false is overridable via the `details` argument to the constructor. All other errors with N=false either hardcode it unconditionally or use `WithRetryableTowardNetwork(false)` after construction.

Wire JSON-RPC code: same as `ErrJsonRpcRequestUnresolvableMethod` — falls through to −32603 (no specific rule in `TranslateToJsonRpcException`).

#### Helper predicates

| Predicate | Returns false when | Source |
|---|---|---|
| `HasErrorCode(err, codes...)` | code not found anywhere in StandardError cause chain or joined multi-errors | `common/errors.go:L2333-L2359` |
| `IsRetryableTowardNetwork(err)` | `ErrUpstreamsExhausted` with nil cause; OR multi-error cause where ALL children non-retryable; OR linear cause chain has `retryableTowardNetwork=false` | `common/errors.go:L2379-L2436` |
| `IsRetryableTowardsUpstream(err)` | specific non-retryable codes anywhere in chain, or `IsCapacityIssue`; for `ErrUpstreamsExhausted` recurses over children | `common/errors.go:L2438-L2498` |
| `IsCapacityIssue(err)` | not one of: `ErrCodeProjectRateLimitRuleExceeded`, `ErrCodeNetworkRateLimitRuleExceeded`, `ErrCodeUpstreamRateLimitRuleExceeded`, `ErrCodeAuthRateLimitRuleExceeded`, `ErrCodeEndpointCapacityExceeded` | `common/errors.go:L2500-L2509` |
| `IsClientError(err)` | not one of: `ErrCodeEndpointClientSideException`, `ErrCodeJsonRpcRequestUnmarshal`, `ErrCodeGetLogsExceededMaxAllowedRange`, `ErrCodeGetLogsExceededMaxAllowedAddresses`, `ErrCodeGetLogsExceededMaxAllowedTopics` | `common/errors.go:L2511-L2520` |
| `ClassifySeverity(err)` | n/a — returns `SeverityInfo`, `SeverityWarning`, or `SeverityCritical` | `common/errors.go:L2531-L2549` |
| `IsClientDisconnect(err)` | Returns `true` for: `errors.Is(err, context.Canceled)`, `errors.Is(err, context.DeadlineExceeded)`, or `err.Error()` exactly equals or contains any of: `"use of closed network connection"`, `"broken pipe"`, `"connection reset by peer"`, `"ECONNRESET"`, `"EPIPE"`. The exact-equality check for `"use of closed network connection"` fires before the `strings.Contains` check on the same string — both branches exist for historical reasons but are effectively redundant. Returns `false` for all other errors. | `common/errors.go:L129-L150` |
| `IsNull(err)` | err is nil OR is a `*BaseError` with empty Code | `common/errors.go:L25-L36` |

#### `ClassifySeverity` decision tree

```
if nil → SeverityInfo
if IsClientError OR HasCode(ExecutionException) → SeverityInfo
if NOT IsRetryableTowardsUpstream → SeverityWarning
if HasCode(EndpointRequestCanceled) OR errors.Is(Canceled) → SeverityWarning
else → SeverityCritical
```
Source: `common/errors.go:L2531-L2549`

#### `BaseError` JSON marshaling

`common/errors.go:L302-L363`. Special cases:
- `Code == ""`, `"ErrUnknown"`, or `"ErrGeneric"` with non-nil cause: serializes as `{"code":"...","message":"...","cause":"<cause.Error()>"}`. With nil cause: serializes as the message string directly.
- `Cause` is `interface{ Unwrap() []error }` (joined): serializes `cause` as JSON array.
- `Cause` implements `StandardError`: serializes `cause` as the error object.
- Other non-nil cause: wrapped in `BaseError{Code:"ErrGeneric", Message:cause.Error()}`.

#### `ErrJsonRpcExceptionInternal` special behaviors

- `CodeChain()` overridden to prefix with numeric normalized code: `"<normalizedCode> <- ErrJsonRpcExceptionInternal"` (`common/errors.go:L2202-L2204`)
- `NormalizedCode()` returns `details["normalizedCode"].(JsonRpcErrorNumber)` (`L2206-L2211`)
- `OriginalCode()` returns `details["originalCode"]` handling both `int` and `float64` types (JSON roundtrip issue) (`L2213-L2222`)

#### Extractor plumbing

| Component | Role | Source |
|---|---|---|
| `JsonRpcErrorExtractor` interface | Injects arch-specific normalization into HTTP clients; avoids import cycles | `common/error_extractor.go:L10-L12` |
| `JsonRpcErrorExtractorFunc` | `http.HandlerFunc`-style adapter | `common/error_extractor.go:L17-L21` |
| `evm.JsonRpcErrorExtractor` | EVM impl delegating to `ExtractJsonRpcError` | `architecture/evm/extractor.go:L11-L17` |
| Injection site | `evm.NewJsonRpcErrorExtractor()` passed to client registry | `upstream/registry.go:L79` |
| HTTP invocation | `c.errorExtractor.Extract(r, nr, jr, c.upstream)` | `clients/http_json_rpc_client.go:L930` |
| gRPC invocation | `common.ExtractGrpcErrorFromGrpcStatus(st, c.upstream)` | `clients/grpc_bds_client.go:L925` |
| Vendor hook | `getVendorSpecificErrorIfAny` → `vn.GetVendorSpecificErrorIfAny(...)` | `architecture/evm/error_normalizer.go:L878-L900` |

#### Legacy dead code: `evm.ExtractGrpcError`

`architecture/evm/error_normalizer.go:L660-L876`. No non-test callers. Deviates from live path (`common.ExtractGrpcErrorFromGrpcStatus`) in these ways:
- BDS `TIMEOUT_ERROR` → `ErrEndpointServerSideException` + −32015 (live: `ErrEndpointRequestTimeout`)
- gRPC `DeadlineExceeded` → `ErrEndpointServerSideException` + −32015 (live: `ErrEndpointRequestTimeout`)
- No `Canceled` branch (falls through to default `ErrEndpointServerSideException`, live: `ErrEndpointRequestCanceled`)

If this function is ever resurrected, these divergences will change retry behavior.

### Observability

#### Prometheus metrics carrying the `error` label

The `error` label value is `common.ErrorFingerprint(err)` (compact `ErrorSummary` + redaction + 256-char cap).

| Metric | Labels including error | Fired at | Source |
|---|---|---|---|
| `erpc_upstream_request_errors_total` | project, vendor, network, upstream, category, **error**, severity, composite, finality, user, agent_name | Each upstream attempt that returned an error (`upstream/upstream.go:L726`); also with severity=info when skipped by block-availability gating | `telemetry/metrics.go:L25` |
| `erpc_network_failed_request_total` | project, network, category, attempt, **error**, severity, finality, user, agent_name | Request failed at network/project level (`erpc/projects.go:L231`) | `telemetry/metrics.go:L491` |
| `erpc_cache_set_error_total` | project, network, category, connector, policy, ttl, **error** | Cache set errored | `telemetry/metrics.go:L556` |
| `erpc_cache_get_error_total` | project, network, category, connector, policy, ttl, **error** | Cache get errored | `telemetry/metrics.go:L580` |
| `erpc_unexpected_panic_total` | scope, extra, **error** | Recovered panic | `telemetry/metrics.go:L13` |

#### The `severity` label

Present on `erpc_upstream_request_errors_total` and `erpc_network_failed_request_total`. Value from `common.ClassifySeverity(err)`: `"critical"`, `"warning"`, or `"info"`. Useful for alert routing: alert only on `severity="critical"`.

#### Tracing

`common.ErrorSummary(err)` is used as the OTel span status description on error (`common/tracing_core.go:L181`). The `error.summary` span attribute is set in failsafe operations (`data/failsafe.go:L240,L274,L302`). Cache errors set `cache.error` / `cache.connector_error` span attributes. gRPC send errors set `grpc.send_error` attribute.

### Edge cases & gotchas

1. **`ErrUpstreamsExhausted` with NO cause is terminal (N=false).** An exhausted error that wraps zero children means no upstream was ever tried — retrying at the network scope cannot make progress. Test: `common/errors_retry_test.go:L11-L26`. Source: `common/errors.go:L2393-L2394`.

2. **All-missing-data exhausted IS retryable.** `ErrUpstreamsExhausted` wrapping only `ErrEndpointMissingData` children is still N=true because `IsRetryableTowardNetwork` recurses children and any retryable child makes the whole thing retryable. Test: `common/errors_retry_test.go:L51-L74`.

3. **`ErrEndpointExecutionException` is N=false by default — but `eth_sendRawTransaction` flips it.** The ctor sets `retryableTowardNetwork=false`. The normalizer overrides this for `eth_sendRawTransaction` reverts/out-of-gas at `architecture/evm/error_normalizer.go:L263-L272, L382-L391`. Insufficient-funds (rule 13) does NOT get the override — it stays N=false even for sendRawTransaction. Test: `common/errors_retry_test.go:L140-L172`.

4. **`ErrEndpointClientSideException` is N=true by default.** Only specific normalizer rules (17 malformed tx, 18d invalid params, gRPC `InvalidArgument`, BDS `INVALID_PARAMETER`/`INVALID_REQUEST`) set N=false. Rule 6 block-tag-unsupported deliberately keeps N=true to allow other upstreams to attempt a supported tag. Test: `common/errors_retry_test.go:L175-L187`.

5. **Retryable flag walk stops at multi-error wrappers.** `IsRetryableTowardNetwork` only descends into a multi-error cause via explicit iteration (not `DeepSearch`), preventing order-dependent results where the flag on the first child would poison the whole set. Source: `common/errors.go:L2373-L2378, L2418-L2433`.

6. **`HasErrorCode` finds codes inside joined errors.** A "non-retryable" code anywhere in a joined cause poisons `IsRetryableTowardsUpstream` for the whole chain. `HasErrorCode` recurses `Unwrap() []error`. Test: `common/errors_retry_test.go:L271-L279`.

7. **`ErrUpstreamClientInitialization` is NOT initializer-fatal by default.** The same type is used for both permanent failures (chainId mismatch) and transient ones (provider unreachable at startup). Callers with permanent causes MUST wrap in `NewTaskFatal(...)`. Test: `common/errors_task_fatal_test.go:L34-L104`. Source: comment at `common/errors.go:L731-L737`.

8. **`ErrorStatusCode()` methods are dead code — zero call sites.** Wire status comes only from the two switch functions in `erpc/http_server.go`. The per-type values still document design intent but are never executed. Source: `docs/kb/_census/errors.md` §8.

9. **`ErrUpstreamRateLimitRuleExceeded` returns 429 in `ErrorStatusCode()` but 200 on the wire.** It is NOT in the `handleErrorResponse` wire 429 switch (`erpc/http_server.go:L1484-L1490`). Only project/network/auth/capacity codes get wire 429.

10. **Nonce-duplicate detection MUST run before generic −32003.** Some vendors use −32003 for "already known"/"nonce too low". Rule 11/12 (nonce exceptions) appear before rule 14 (generic −32003) in the normalizer. "Replacement transaction underpriced" deliberately stays as a plain error — it means a user error, not a retry candidate. Source: comment at `architecture/evm/error_normalizer.go:L297-L301`.

11. **"Unsupported method" detection ordered AFTER "not found" rules.** Prevents bare "not found" strings from being misread as method-unsupported (e.g. Tenderly). Source: comment at `architecture/evm/error_normalizer.go:L452-L453`.

12. **200-OK revert detection index is 1, not 0.** `dt[1:11]=="0x08c379a0"` because `dt[0]` is the JSON quote character of the result string. Index 0 is `"`. Source: `architecture/evm/error_normalizer.go:L609-L624`.

13. **Trace/debug timeout detection scans raw bytes, not parsed JSON.** Rule 22 does a raw string search on the response body to detect "execution timeout" in `trace_*`/`debug_*` responses. This avoids JSON-parsing up to ~50MB trace responses. Source: comment at `architecture/evm/error_normalizer.go:L626-L629`.

14. **OP-stack "sender is over rate limit" is N=false (unique among capacity errors).** All eRPC provider entries proxy the same OP sequencer, so retrying on a different provider would hit the same sequencer limit. All other `ErrEndpointCapacityExceeded` are N=true. Source: `architecture/evm/error_normalizer.go:L125-L139`.

15. **`ErrEndpointTransportFailure` strips the endpoint URL from the cause message.** Credential hygiene — endpoint URLs may contain API keys. Source: `common/errors.go:L1933-L1938`.

16. **`ErrEndpointServerSideException` preserves the upstream's original HTTP status code** in `originalStatusCode` field (exposed via `ErrorStatusCode()`) but the wire status is always 200 for POST requests. The field survives in details for observability.

17. **`ErrDynamicTimeoutExceeded` distinguishes failsafe-policy timeouts from HTTP server deadlines.** It is set as the `context.WithTimeoutCause` cause by the timeout policy. Checking `context.Cause(ctx) == ErrDynamicTimeoutExceeded` tells you the failsafe policy fired, not the HTTP server's global deadline. Source: `common/errors.go:L1981-L1984`.

18. **`buildErrorResponseBody` unwraps single-child `ErrUpstreamsExhausted`.** When there is exactly one upstream in the exhausted set, the single child error is returned to the client directly instead of the `ErrUpstreamsExhausted` wrapper, giving a more useful error message. Source: `erpc/http_server.go:L1397-L1403`.

19. **Compact `ErrorSummary` compound label format is not universal.** Only `ErrFailsafeRetryExceeded`, `ErrUpstreamRequest`, and `ErrUpstreamRequestSkipped` produce `"OuterCode/InnerCode"` compound labels. `ErrFailsafeTimeoutExceeded` with a cause does NOT compound (test: `common/errors_summary_test.go:L24-L30`). Non-StandardError causes on retry exceeded produce just the outer code (test: `L39-L45`).

20. **`TranslateToJsonRpcException` dominant-code counting skips `UpstreamRequestSkipped` and `EndpointUnsupported`.** These are considered low-signal noise relative to actual failure codes. Source: `common/json_rpc.go:L1523-L1526`. **What the client sees:** when ALL upstreams were skipped (`ErrCodeUpstreamRequestSkipped`) or unsupported (`ErrCodeEndpointUnsupported`), the dominant-code loop excludes both codes from the `counts` map, leaving `maxCount=0`. In that case `domErr` is set to the very first child error encountered (the fast-path at `L1519-L1521`) regardless of its code, so the client sees the wire JSON-RPC code of whatever the first upstream produced — which will typically be −32601 (unsupported) or −32603 (skipped), matching what the first child produces via `TranslateToJsonRpcException` steps 5 and 9. If there are NO children at all, `domErr` remains nil and the outer `ErrUpstreamsExhausted` itself is translated (fallback −32603). This means a request where all upstreams skip/refuse the method surfaces as −32601 or −32603 depending on whether the first child was `ErrUpstreamMethodIgnored` or `ErrUpstreamRequestSkipped`.

21. **gRPC legacy extractor (`evm.ExtractGrpcError`) maps `TIMEOUT_ERROR`/`DeadlineExceeded` differently from live path.** Live `common.ExtractGrpcErrorFromGrpcStatus` maps both to `ErrEndpointRequestTimeout`; legacy maps both to `ErrEndpointServerSideException`. If the legacy function is ever restored, BDS timeout handling will regress. Source: `common/grpc_errors.go:L86-L97, L167-L178` vs `architecture/evm/error_normalizer.go:L740-L751, L814-L825`.

22. **`ErrEndpointClientSideException.ErrorStatusCode()` returns 200 for revert-class causes.** Even though it is a "client-side exception", when the inner `ErrJsonRpcExceptionInternal` has `normalizedCode` ∈ {3 (`JsonRpcErrorEvmReverted`), −32000 (`JsonRpcErrorCallException`), −32003 (`JsonRpcErrorTransactionRejected`)} the method returns 200, not 400. This is because an EVM revert, call exception, or rejected transaction is a valid execution outcome — the client should interpret the JSON-RPC `error` body, not an HTTP 4xx, as the response. The general case (cause is nil, or has any other normalizedCode) returns 400. Note: `ErrorStatusCode()` is dead code (zero call sites, see edge case 8), but the wire behavior for these cases follows the same logic — `determineResponseStatusCode` / `handleErrorResponse` do NOT check the inner normalizedCode of a `ClientSideException`; these normalized errors always result in 200 on the wire via the default case anyway. Source: `common/errors.go:L1894-L1905`.

23. **`ErrConsensusDispute` and `ErrConsensusLowParticipants` retryability follows the multi-error children rule.** Both store their per-upstream participant errors as the cause via `errors.Join`. `IsRetryableTowardNetwork` at `common/errors.go:L2397-L2410` iterates the joined children: if ANY single child returns `IsRetryableTowardNetwork=true`, the whole consensus error is retryable; only if ALL children are non-retryable does the consensus error become non-retryable. `IsRetryableTowardsUpstream` applies the same any-child semantics via the identical multi-error branch at `common/errors.go:L2443-L2452`. This means a consensus dispute where even one upstream's sub-error is retryable will trigger a network-level retry. Cross-ref: `14-consensus.md:L250`. Source: `common/errors.go:L2397-L2411`.

### Source map

- `common/errors.go` — Complete error type definitions, `BaseError`, `StandardError` interface, `RetryableError`, `UpstreamAwareError`, all `ErrCode*` constants, retryability predicates (`IsRetryableTowardNetwork`, `IsRetryableTowardsUpstream`, `IsCapacityIssue`, `IsClientError`), severity classification, `ErrorSummary`, `ErrorFingerprint`, `cleanUpMessage`, `HasErrorCode`, `IsClientDisconnect`, `IsNull`, `TaskFatalError`, `ErrDynamicTimeoutExceeded`, `JsonRpcErrorNumber` constants, `ErrJsonRpcExceptionInternal`, `ErrJsonRpcExceptionExternal`
- `common/grpc_errors.go` — Live gRPC/BDS error normalization (`ExtractGrpcErrorFromGrpcStatus`); BDS error code priority then gRPC code fallback
- `common/error_extractor.go` — `JsonRpcErrorExtractor` interface and `JsonRpcErrorExtractorFunc` adapter; avoids import cycles between clients and architecture packages
- `common/json_rpc.go` — `TranslateToJsonRpcException` (internal → wire JSON-RPC code conversion); dominant-code selection from `ErrUpstreamsExhausted`
- `common/config.go:L2536-L2551` — `LabelMode` type, `ErrorLabelModeVerbose`/`ErrorLabelModeCompact` constants, `MetricsConfig.ErrorLabelMode` field
- `common/defaults.go:L762-L763` — Default `ErrorLabelMode = compact`
- `common/validation.go:L147-L148` — Validates `errorLabelMode` value
- `architecture/evm/error_normalizer.go` — `ExtractJsonRpcError` (HTTP response normalizer); all 22 generic rules in order; `getVendorSpecificErrorIfAny` vendor hook; dead `evm.ExtractGrpcError` legacy function
- `architecture/evm/util.go:L7-L49` — `IsNonRetryableWriteMethod` (retry/hedge guard for non-idempotent write methods); `IsMissingDataError` string patterns for rule 7
- `upstream/upstream_executor.go:L243, L281` — Call sites of `IsNonRetryableWriteMethod`; retry guard at L243, hedge guard at L281
- `common/request.go:L957, L991` — Emission sites of `NewErrJsonRpcRequestUnresolvableMethod`: after unmarshal when method is empty, and when body is nil/empty in `Method()`
- `architecture/evm/extractor.go:L11-L17` — EVM `JsonRpcErrorExtractor` implementation
- `erpc/http_server.go:L1280-L1317, L1390-L1491` — `determineResponseStatusCode`, `buildErrorResponseBody`, `handleErrorResponse`; the two wire-status switch functions that actually control HTTP status
- `common/errors_retry_test.go` — Tests for `IsRetryableTowardNetwork`/`IsRetryableTowardsUpstream`/`HasErrorCode`
- `common/errors_summary_test.go` — Tests for `ErrorSummary` compact vs verbose modes and compound labels
- `common/errors_task_fatal_test.go` — Tests for `TaskFatalError` / `ErrUpstreamClientInitialization` initializer behavior
- `telemetry/metrics.go:L25, L491` — `erpc_upstream_request_errors_total`, `erpc_network_failed_request_total` — the two primary error-by-type metrics
- `thirdparty/*.go` — Per-vendor `GetVendorSpecificErrorIfAny` implementations
- `docs/kb/_census/errors.md` — Completeness checklist; all rows verified against source
