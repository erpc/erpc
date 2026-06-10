# KB: EVM architecture abstraction & misc glue

> status: complete
> source-dirs: common/architecture_evm.go, common/network.go, common/exec_state.go, common/runtime.go, common/object.go, common/compiler.go, common/console.go, architecture/evm/common.go, architecture/evm/hooks.go, architecture/evm/json_rpc.go, architecture/evm/json_rpc_cache.go, architecture/evm/block_ref.go, architecture/evm/extractor.go, architecture/evm/error_normalizer.go, architecture/evm/util.go, architecture/evm/evm_state_poller.go, architecture/evm/eth_sendRawTransaction.go, common/config.go (EvmNetworkConfig, EvmIntegrityConfig), common/defaults.go (EvmNetworkConfig.SetDefaults, DefaultMarkEmptyAsErrorMethods)

## L1 — Capability (CTO view)

eRPC is an architecture-pluggable proxy: its core routing, failsafe, and caching logic is architecture-agnostic and calls out to an `architecture/evm/` plugin for every EVM-specific concern — block-reference extraction, block-tag normalization ("latest"→concrete hex), method-level pre/post forward hooks, error normalization from raw HTTP responses and gRPC status codes, and cache key management. The EVM architecture is the only one implemented today (`common/network.go:L15,L36`), but the interfaces are designed to allow future addition of non-EVM chains without modifying core logic.

Additionally, two purely infrastructural modules (`common/exec_state.go`, `common/runtime.go`) provide per-request execution telemetry and a sandboxed JavaScript evaluation environment (Sobek/V8-compatible) used by user-defined selection policies and future scripting hooks.

## L2 — Mechanics (staff-engineer view)

**Architecture pluggability.** The `NetworkArchitecture` type (`common/network.go:L12`) is currently a string constant with only one valid value: `"evm"`. `IsValidArchitecture()` hard-codes the check (`common/network.go:L35-L37`). The network URL format is `evm:<chainId>` (`common/network.go:L40-L49`). The HTTP server validation rejects any architecture that is not `"evm"` (`erpc/http_server.go:L997-L999`). The `EvmUpstream` interface (`common/architecture_evm.go:L14-L31`) extends the base `Upstream` interface with EVM-specific state queries.

**Hook dispatch.** Four hook entry-points exist, each dispatched by `architecture/evm/hooks.go` based on a lowercased method name switch:
- `HandleProjectPreForward` — runs at project layer before cache and upstream selection; handles `eth_blockNumber`, `eth_call`, `eth_chainId`, `eth_getLogs`, `trace_filter`/`arbtrace_filter` (`architecture/evm/hooks.go:L13-L36`)
- `HandleNetworkPreForward` — runs after upstream selection, can short-circuit based on state; handles `eth_getLogs`, `eth_chainId`, `trace_filter`/`arbtrace_filter` (`architecture/evm/hooks.go:L40-L59`)
- `HandleNetworkPostForward` — called after the upstream response comes back, at network layer; handles `eth_getBlockByNumber`, `eth_getLogs`, `eth_sendRawTransaction`, `trace_filter`/`arbtrace_filter` (`architecture/evm/hooks.go:L63-L84`)
- `HandleUpstreamPostForward` — the richest hook, called per-upstream; checks the `MarkEmptyAsErrorMethods` list for the current network config, applies per-method validation, and marks unexpected empties as `ErrEndpointMissingData` (`architecture/evm/hooks.go:L109-L183`)

**Block tag normalization.** `NormalizeHttpJsonRpc` (`architecture/evm/json_rpc.go:L92-L321`) runs on every incoming JSON-RPC request. It reads a method config's `ReqRefs` (path specs like `[0]` or `[0,"blockNumber"]`) to locate block parameters, then:
1. Caches the numeric block number on the `NormalizedRequest` (used by cache-key generation, block-availability checks, etc.)
2. If the tag is `"latest"` or `"finalized"` and the network has a known head, replaces the tag with a concrete `0x...` hex (interpolation). Tags `"safe"` and `"pending"` are intentionally passed through unchanged.
3. Performs a deep-copy of params before writing to avoid race conditions between goroutines sharing the JSON-RPC request object.
4. Sets `EvmBlockRef` on the request: `"latest"`, `"finalized"`, or `"*"` when both tags appear in the same call (composite range methods). The `X-ERPC-Skip-Interpolation` directive or `skipInterpolation` request directive suppresses mutations while still caching the numeric block number.

**Block reference extraction.** `ExtractBlockReferenceFromRequest` and `ExtractBlockReferenceFromResponse` (`architecture/evm/block_ref.go`) are the authoritative way to derive a `(blockRef string, blockNumber int64)` pair for any request or response. The request extractor falls back to the last-valid response when the request params alone are insufficient. Special values:
- `"*"` = wildcard (method is transaction/receipt hash lookup, or composite range); uses `ConnectorReverseIndex` for cache lookups
- `"1"` = finalized-forever static data (chain ID, genesis block); cached indefinitely
- A numeric string (e.g. `"19827314"`) = concrete block; uses `ConnectorMainIndex`

**Empty-result handling.** `upstreamPostForward_markUnexpectedEmpty` (`architecture/evm/common.go:L11-L53`) converts a `null`/empty result to `ErrEndpointMissingData` when:
1. The method is in the network's `MarkEmptyAsErrorMethods` list (defaults to a fixed set of point-lookups; see below)
2. `req.Directives().RetryEmpty` is `true`
3. The block is NOT beyond the confidence head: `emptyResultBeyondConfidence` compares the requested block number against the highest known latest (or finalized) head, failing open when the head is unknown (`architecture/evm/common.go:L62-L89`).

**Error normalization.** `ExtractJsonRpcError` (`architecture/evm/error_normalizer.go:L20-L658`) maps raw upstream HTTP responses to strongly-typed `ErrEndpoint*` values. The mapping is purely text-based (no JSON parsing beyond the already-decoded `jr.Error`): it uses `strings.Contains` chains that cover ~30 distinct error patterns from known providers (Alchemy, DRPC, Infura, Tenderly, Avalanche, etc.). `ExtractGrpcError` does the same for gRPC status codes, also integrating BDS (`blockchain-data-standards/manifesto`) typed error details when present. A critical ordering rule in the normalizer: nonce/duplicate checks (lines 303-341) must precede insufficient-funds/rejected checks (lines 350-392), which must precede the generic `-32603` fallback, to avoid misclassifying `eth_sendRawTransaction` errors.

**JavaScript runtime.** `common.Runtime` wraps a Sobek (V8-compatible) JavaScript engine (`common/runtime.go`). A new `Runtime` instance:
- Has `process.env` pre-populated with OS environment variables (`common/runtime.go:L22-L36`)
- Has a `console` object with `debug`, `info`, `log`, `trace`, `warn` methods that bridge to zerolog (`common/console.go:L13-L40`)
- Uses `json` struct tags as JS property names via `TagFieldNameMapper`
- Is NOT safe for concurrent use — callers (selection policy pool at `internal/policy/runtime_pool.go:L51`) maintain a per-goroutine pool
- `CompileFunction` and `CompileProgram` (`common/compiler.go`) use esbuild for TypeScript/IIFE compilation and `sobek.Compile` for hot-path JS parsing; compiled programs can be shared across runtime instances

**ExecState — per-request execution telemetry.** `ExecState` (`common/exec_state.go:L105-L133`) is lazily initialized on first access via `NormalizedRequest.ExecState()` (`common/request.go:L362`). It tracks three orthogonal scope counters (upstream, network, cache) for attempts/retries/hedges, plus consensus slots/disputes/low-participant events. The `Snapshot()` totals model: `total Attempts = UpstreamAttempts + CacheAttempts` (NetworkAttempts is a rotation count, NOT summed in); `total Retries = UpstreamRetries + NetworkRetries + CacheRetries`; `total Hedges = UpstreamHedges + NetworkHedges + CacheHedges`. The `UpstreamAttemptLog` records every `(upstreamId, outcome, reason, duration, won)` tuple in order of start time, protected by a mutex for concurrent participant goroutines. `Apply(span)` emits all counters plus the parallel slices (`upstreams.tried`, `upstreams.outcomes`, `upstreams.reasons`, `upstreams.durations_ms`, `upstreams.won`) as span attributes.

**JavaScript-to-Go type mapping.** `MapJavascriptObjectToGo` (`common/object.go:L12-L60`) and `setJsValueToGoField` (`common/object.go:L62-L245`) use reflection to populate Go structs from Sobek JS objects. Special handling: `Duration` accepts string (`"5s"`) or integer (ms); `RateLimitPeriod` accepts string aliases (`"second"`, `"minute"`, `"1m"`, etc.) or integer enum values; slices, maps (string-keyed only), pointers, and nested structs are recursively handled.

## L3 — Exhaustive reference (agent view)

### Config fields

All under `networks[*].evm.` in YAML. Populated by `EvmNetworkConfig.SetDefaults()` (`common/defaults.go:L2060-L2115`).

| # | YAML path | Type | Default | Behavior |
|---|---|---|---|---|
| 1 | `evm.chainId` | int64 | required (0 = unset) | EVM chain ID. Used for `eth_chainId` responses and network ID formation (`evm:<chainId>`). `common/config.go:L2178` |
| 2 | `evm.fallbackFinalityDepth` | int64 | `1024` (`common/defaults.go:L1975`) | Depth used when the network cannot determine finality dynamically (no finalized block from poller). A block is considered finalized if `latestBlock - blockNumber >= fallbackFinalityDepth`. `common/config.go:L2179` |
| 3 | `evm.fallbackStatePollerDebounce` | Duration | `5s` (`common/defaults.go:L1976`) | Fallback poll interval for the state poller when dynamic block time is not yet known. `common/config.go:L2180` |
| 4 | `evm.integrity` | *EvmIntegrityConfig | defaults applied (see rows 4a-4c) | **Deprecated wrapper** for directive-defaults. New code uses `directiveDefaults` instead. `common/config.go:L2181` |
| 4a | `evm.integrity.enforceHighestBlock` | *bool | `true` (`common/defaults.go:L2118-L2120`) | **Deprecated** — migrates to `directiveDefaults.enforceHighestBlock`. |
| 4b | `evm.integrity.enforceGetLogsBlockRange` | *bool | `true` (`common/defaults.go:L2121-L2123`) | **Deprecated** — migrates to `directiveDefaults.enforceGetLogsBlockRange`. |
| 4c | `evm.integrity.enforceNonNullTaggedBlocks` | *bool | `true` (`common/defaults.go:L2124-L2126`) | **Deprecated** — migrates to `directiveDefaults.enforceNonNullTaggedBlocks`. |
| 5 | `evm.servedTip` | *EvmServedTipConfig | nil (max mode) | Controls how "latest"/"finalized" served tip is computed. See KB 25 for full detail. `common/config.go:L2188` |
| 5a | `evm.servedTip.enabledFor` | []string | `[]` | Tags using cluster-min mode; valid: `"latest"`, `"finalized"`, `"safe"`. `common/config.go:L2272` |
| 5b | `evm.servedTip.clusterDelta` | int64 | `0` (auto-derived from block time, clamped [2,10]) | Max block gap to group upstreams into one cluster. `common/config.go:L2277` |
| 5c | `evm.servedTip.guaranteedMethods` | []string | `[]` | Glob patterns for methods whose supporting-upstreams subset is used for cluster computation. `common/config.go:L2284` |
| 6 | `evm.getLogsMaxAllowedRange` | int64 | `30_000` (`common/defaults.go:L2092-L2094`) | Max block range for `eth_getLogs` before forced splitting. `common/config.go:L2189` |
| 7 | `evm.getLogsMaxAllowedAddresses` | int64 | `0` (unlimited) | Max address count in `eth_getLogs` filter. `common/config.go:L2190` |
| 8 | `evm.getLogsMaxAllowedTopics` | int64 | `0` (unlimited) | Max topic count in `eth_getLogs` filter. `common/config.go:L2191` |
| 9 | `evm.getLogsSplitOnError` | *bool | `true` (`common/defaults.go:L2095-L2097`) | When `eth_getLogs` gets a range-too-large error, split and retry. `common/config.go:L2192` |
| 10 | `evm.getLogsSplitConcurrency` | int | `10` (`common/defaults.go:L2098-L2100`) | Max concurrent sub-requests when splitting `eth_getLogs`. `common/config.go:L2193` |
| 11 | `evm.traceFilterSplitOnError` | *bool | `nil` (disabled) | When `trace_filter`/`arbtrace_filter` gets a range-too-large error, split and retry. Defaults to nil (opt-in required). `common/config.go:L2197` |
| 12 | `evm.traceFilterSplitConcurrency` | int | `10` (`common/defaults.go:L2105-L2107`) | Max concurrent sub-requests when splitting `trace_filter`. `common/config.go:L2200` |
| 13 | `evm.enforceBlockAvailability` | *bool | nil (true by default in logic; `false` in pre-canned upstream templates `common/defaults.go:L267,280,387,395`) | Whether to gate requests to upstreams based on their known block bounds. Nil resolves to true in most contexts. `common/config.go:L2204` |
| 14 | `evm.maxRetryableBlockDistance` | *int64 | `128` (hardcoded at `erpc/networks.go:L1975`) | Max block distance ahead of upstream's latest for which a `block_unavailable` error is retryable (upstream may catch up). Larger distance → not retryable. `common/config.go:L2211` |
| 15 | `evm.markEmptyAsErrorMethods` | []string | 11 methods (see `DefaultMarkEmptyAsErrorMethods()`) | Methods for which an empty/null upstream result is converted to `ErrEndpointMissingData` and retried. Default set: `eth_blockNumber`, `eth_getBlockByNumber`, `eth_getTransactionByHash`, `eth_getTransactionByBlockHashAndIndex`, `eth_getTransactionByBlockNumberAndIndex`, `eth_getUncleByBlockHashAndIndex`, `eth_getUncleByBlockNumberAndIndex`, `debug_traceTransaction`, `trace_transaction`, `trace_block`, `trace_get`. Explicitly omitted: `eth_getBlockByHash`, `eth_getTransactionReceipt`, `eth_getBlockReceipts`. `common/defaults.go:L2044-L2058` |
| 16 | `evm.dynamicBlockTimeDebounceMultiplier` | *float64 | `0.7` (`common/defaults.go:L1977`) | Scales EMA block time to derive the state-poller debounce interval. Lower = fresher but more polling. `common/config.go:L2225` |
| 17 | `evm.blockUnavailableDelayMultiplier` | *float64 | `1.0` (`common/defaults.go:L1978`) | Multiplies the EMA-estimated block time for the network to derive the dynamic retry delay used whenever a block or transaction is not yet available (`ErrUpstreamBlockUnavailable` or `ErrEndpointMissingData`). The delay is computed as `metricsTracker.GetNetworkBlockTime(networkId) × multiplier` (`erpc/networks_registry.go:L107-L112`). The dynamic delay returns `0` before the EMA warms up; in that case the retry falls back to the static `failsafe[*].retry.emptyResultDelay` value (`erpc/network_executor.go:L497-L507`). Interacts directly with `emptyResultDelay`: `emptyResultDelay` is the fixed bootstrap fallback; `blockUnavailableDelayMultiplier` governs the production-time dynamic path once the EMA is warm. `common/config.go:L2227-L2232` |
| 18 | `evm.idempotentTransactionBroadcast` | *bool | nil (enabled) | When enabled (nil or true), `eth_sendRawTransaction` idempotency handling converts "already known" errors to success and verifies "nonce too low" by polling eth_getTransactionByHash. Set to `false` to disable and return raw upstream errors. `common/config.go:L2239` |
| 19 | `evm.emptyResultConfidence` | AvailbilityConfidence | `blockHead` (= `1`) (`common/defaults.go:L2075-L2079`) | Confidence level for empty-result retries: `blockHead` (default) = retry empties for blocks ≤ latest head; `finalizedBlock` = stricter, only retry for blocks ≤ finalized head. `common/config.go:L2250` |
| 20 | `evm.maxFutureBlockRetryDistance` | *int64 | `nil` (field exists yaml-only; tagged `json:"-"` so it never appears in JSON/API output) | **Deprecated** — replaced by `emptyResultConfidence`. The field is retained as a yaml-only key so configs that still set it keep loading without parse errors. On `SetDefaults()` execution: if the pointer is non-nil, the warning `"config: evm.maxFutureBlockRetryDistance is deprecated and ignored; use evm.emptyResultConfidence (blockHead|finalizedBlock) instead"` is emitted via `log.Warn()` and the pointer is immediately set to `nil` — the value is never read at runtime. The old behavior (a numeric "block distance band" controlling how far ahead of latest was retryable) is now fully replaced by the binary `emptyResultConfidence` enum. `common/config.go:L2252-L2255`, `common/defaults.go:L2080-L2083` |

### Behaviors & algorithms

**EvmUpstream interface** (`common/architecture_evm.go:L14-L31`):
- `EvmGetChainId(ctx) (string, error)` — fetch chain ID from the upstream directly
- `EvmIsBlockFinalized(ctx, blockNumber, forceFreshIfStale) (bool, error)` — asks the state poller
- `EvmAssertBlockAvailability(ctx, method, confidence, forceFreshIfStale, blockNumber) (bool, error)` — checks both lower (earliest) and upper (latest/finalized) bounds
- `EvmSyncingState() EvmSyncingState` — returns `Unknown`/`Syncing`/`NotSyncing`
- `EvmStatePoller() EvmStatePoller` — returns the upstream's poller instance
- `EvmEffectiveLatestBlock() int64` — `min(latestBlock, upperBound)` if the upstream has `blockAvailability.upper`; otherwise raw latest
- `EvmEffectiveFinalizedBlock() int64` — same adjustment for finalized
- `EvmBlockAvailabilityBounds() (int64, int64)` — resolved `[min, max]`; `math.MinInt64`/`math.MaxInt64` for unset sides

**EvmNodeType** (`common/architecture_evm.go:L77-L83`): `unknown`, `full`, `archive`. Full nodes default to 128-block availability window; archive nodes are unbounded.

**AvailbilityConfidence** (`common/architecture_evm.go:L33-L75`): Values `1` = `blockHead`, `2` = `finalizedBlock`. Serializes to/from YAML/JSON as string (`"blockHead"`, `"finalizedBlock"`); also accepts `"1"` or `"2"` as numeric strings in YAML.

**EvmSyncingState** (`common/architecture_evm.go:L85-L101`): `iota` 0=`Unknown`, 1=`Syncing`, 2=`NotSyncing`. Syncing state is re-checked until confirmed non-syncing `FullySyncedThreshold` (= `4`) consecutive times (`architecture/evm/evm_state_poller.go:L21`). A single `syncing=true` resets the counter.

**EvmStatePoller interface** (`common/architecture_evm.go:L104-L121`):
- `Bootstrap(ctx)` — initial state fetch before first real request
- `Poll(ctx)` — single poll cycle (latest + finalized + syncing)
- `PollLatestBlockNumber(ctx) (int64, error)` — explicit latest-block poll
- `PollFinalizedBlockNumber(ctx) (int64, error)` — explicit finalized-block poll
- `PollEarliestBlockNumber(ctx, probe, staleness) (int64, error)` — probe-specific earliest detection
- `SuggestFinalizedBlock(blockNumber)` / `SuggestLatestBlock(blockNumber)` — allow other parts to hint the poller (e.g. from observed response headers)
- `SetNetworkConfig(cfg *NetworkConfig)` — called when network config changes for lazy-loaded networks
- `GetDiagnostics() *EvmStatePollerDiagnostics` — structured diagnostic snapshot for admin/debug

**EvmStatePollerDiagnostics** (`common/architecture_evm.go:L123-L158`): Contains block head, syncing state, per-probe earliest info, failure counts, and per-feature skip flags. Serialized as JSON in admin responses.

**EvmAvailabilityProbeType** (`common/config.go:L1211-L1216`): `blockHeader`, `eventLogs`, `callState`, `traceData`. Each probe type tracks its own earliest block independently.

**NormalizeHttpJsonRpc** (`architecture/evm/json_rpc.go:L92-L321`):
1. Reads lock → extract param values at paths from method config's `ReqRefs`
2. Release lock
3. For each param: parse as block hash / hex number / tag
4. For numeric: normalize to canonical hex, cache as `EvmBlockNumber`
5. For `"latest"`: if `translateLatestTag` not disabled and not `skipInterpolation`, resolve via `network.EvmHighestLatestBlockNumber(ctx)`; replace param with hex
6. For `"finalized"`: same pattern with `EvmHighestFinalizedBlockNumber`
7. For `"safe"`, `"pending"`, `"earliest"`, hash: pass through unchanged
8. Determine `EvmBlockRef`: `"latest"` if only latest seen, `"finalized"` if only finalized, `"*"` if both
9. If any changes: deep-copy params (`deepCopyParams`), apply path-based mutations, write-lock → update `jrq.Params`

**resolveBlockTagToHex** (`architecture/evm/json_rpc.go:L58-L83`): Translates `"latest"` → hex of `EvmHighestLatestBlockNumber`, `"finalized"` → hex of `EvmHighestFinalizedBlockNumber`. Returns `("", false)` when head is 0 (unknown). Does NOT handle `"safe"` or `"pending"` by design.

**ExtractBlockReferenceFromRequest** (`architecture/evm/block_ref.go:L18-L115`): Priority order: (1) cached on request struct, (2) JSON-RPC params via method config `ReqRefs`, (3) last-valid response's block ref. Special: for `Finalized` method configs, always returns `("*", 1)` (cache forever); for `Realtime` configs, returns `("*", 0)` (cache with realtime policy).

**ExtractBlockReferenceFromResponse** (`architecture/evm/block_ref.go:L117-L187`): Uses method config's `RespRefs` (response-side path specs). Falls back to numeric ref when no explicit ref found.

**Block ref wildcard semantics**: `blockRef == "*"` triggers `connector.Get(ctx, ConnectorReverseIndex, ...)` instead of `ConnectorMainIndex`. This allows hash-based or tx-hash-based lookups to find the cached entry without knowing the block number ahead of time.

**shouldAcceptCachedResult** (`architecture/evm/json_rpc_cache.go:L841-L924`): Only applies to `DataFinalityStateRealtime` data. Age guard logic:
1. Extract block timestamp from response (via `ExtractBlockTimestampFromResponse`)
2. If unavailable (eth_blockNumber, eth_gasPrice, eth_getLogs), fall back to connector's `CacheLatestBlockTimestamp` if it implements `data.CacheHeadReporter`
3. If still unknown: accept (fail-open)
4. If `age > policy.TTL`: reject with `erpc_cache_get_age_guard_reject_total` metric

**isNonRetryableWriteMethod** (`architecture/evm/util.go:L8-L17`): Returns true for `eth_sendTransaction`, `eth_createAccessList`, `eth_submitTransaction`, `eth_submitWork`, `eth_newFilter`, `eth_newBlockFilter`, `eth_newPendingTransactionFilter`. Note: `eth_sendRawTransaction` is intentionally excluded (has idempotency handling).

**IsMissingDataError** (`architecture/evm/util.go:L19-L49`): Text-match function for 20+ missing-data error patterns covering: missing trie nodes, header/block not found, unknown/invalid block height, finalized block issues, Avalanche unfinalized queries, trace-before-genesis, no-historical-rpc, transaction not found, and state unavailable patterns.

**ExecState counter model** (`common/exec_state.go:L82-L100`):
- `total Attempts = UpstreamAttempts + CacheAttempts` (NetworkAttempts is NOT summed in — it's a rotation count, not a physical call count)
- `total Retries = UpstreamRetries + NetworkRetries + CacheRetries`
- `total Hedges = UpstreamHedges + NetworkHedges + CacheHedges`
- All counters are `atomic.Int32`; `Snapshot()` captures them with independent `Load()` calls (no global lock) — under high concurrency totals may briefly drift from component sum

**UpstreamAttemptOutcome values** (`common/exec_state.go:L18-L31`): `success`, `empty`, `transport_error`, `server_error`, `client_error`, `rate_limited`, `missing_data`, `exec_revert`, `block_unavailable`, `breaker_open`, `cancelled`, `timeout`, `skipped`

**UpstreamSelectionReason values** (`common/exec_state.go:L39-L45`): `primary`, `retry`, `hedge`, `consensus_slot`, `sweep`

**MarkUpstreamAttemptWon** (`common/exec_state.go:L170-L182`): Walks the attempt log from the END (last attempt for the upstream is the one that produced the result). For consensus, called once per participant in the winning group.

**ExtractJsonRpcError error precedence** (`architecture/evm/error_normalizer.go`):
1. Range-too-large → `ErrEndpointRequestTooLarge` (various subtypes: `EvmBlockRangeTooLarge`, `EvmAddressesTooLarge`)
2. OP Stack sender rate limit → `ErrEndpointCapacityExceeded` (non-network-retryable)
3. Billing exhaustion (HTTP 402) → `ErrEndpointBillingIssue`
4. Rate limiting (HTTP 429 or text patterns) → `ErrEndpointCapacityExceeded`
5. Block tag unsupported (pending/safe/finalized not available) → `ErrEndpointClientSideException`
6. Missing data (IsMissingDataError) → `ErrEndpointMissingData`
7. Execution timeout → `ErrEndpointServerSideException`
8. EVM revert / VM exception → `ErrEndpointExecutionException`
9. Already-known / nonce-too-low (for idempotency) → `ErrEndpointNonceException`
10. Insufficient funds → `ErrEndpointExecutionException`
11. Transaction rejected / out-of-gas → `ErrEndpointExecutionException`
12. Not-found/disabled (method) → `ErrEndpointUnsupported`; (block/state) → `ErrEndpointMissingData`
13. Unsupported (HTTP 415/405, codes -32004/-32001) → `ErrEndpointUnsupported`
14. Malformed transaction (RLP errors) → `ErrEndpointClientSideException` (not network-retryable)
15. Invalid type/map errors → `ErrEndpointClientSideException` (retryable)
16. Invalid args (code -32602/-32600) → `ErrEndpointClientSideException`
17. Unauthorized (HTTP 401/403) → `ErrEndpointUnauthorized`
18. Fallback → `ErrEndpointServerSideException`

Special: 0x08c379a0 prefix in a successful result (keccak256 of "Error(string)") → revert even with HTTP 200. The check is `dt[1:11] == "0x08c379a0"` (starting at index 1, not 0) because `dt` is the raw JSON string returned by `jr.GetResultString()` — which includes the surrounding JSON quote character `"` at position 0. The prefix `"0x08c379a0` therefore lives at positions 1-10. A naive `dt[0:10] == "0x08c379a0"` check would always fail on a valid JSON string. (`architecture/evm/error_normalizer.go:L609-L624`). Trace/debug methods may embed "execution timeout" in their result JSON.

**ExtractGrpcError** (`architecture/evm/error_normalizer.go:L660-L876`): BDS error codes take precedence when present in gRPC status details. BDS codes `UNSUPPORTED_BLOCK_TAG`/`UNSUPPORTED_METHOD` → `ErrEndpointUnsupported`; `RANGE_OUTSIDE_AVAILABLE` → `ErrEndpointMissingData`; `RANGE_TOO_LARGE` → `ErrEndpointRequestTooLarge`; `RATE_LIMITED` → `ErrEndpointCapacityExceeded`; `TIMEOUT_ERROR` → `ErrEndpointServerSideException`; `INTERNAL_ERROR` → `ErrEndpointServerSideException`. gRPC codes without BDS: `Unimplemented` → Unsupported; `InvalidArgument` → ClientSide (not network-retryable); `ResourceExhausted` → CapacityExceeded; `DeadlineExceeded` → ServerSide; `Unauthenticated/PermissionDenied` → Unauthorized; `NotFound/OutOfRange` → MissingData; rest → ServerSide.

**cache.Get fan-out** (`architecture/evm/json_rpc_cache.go:L152-L565`): Spawns one goroutine per matching policy (deduplicated by connector). First hit cancels all others. Defensive 30s backstop added when the caller's context has no deadline. Drain loop handles races where the winner's send happens before `cancelFan()`. Miss reasons tracked: `ttl_rejected` (age guard) > `connector_miss` > `connector_error` for metric labeling.

**cache.Set behavior** (`architecture/evm/json_rpc_cache.go:L567-L828`): Parallel writes to all matching policies (no dedup by connector on Set). Responses with `jr.Error != nil` are never cached. Future-block empty results are never cached (`emptyResultBeyondConfidence` check). Compression: if `cfg.Compression.Enabled` and response bytes ≥ threshold, zstd-compress via pooled encoder; only stored compressed when `len(compressed) < len(original)`.

**cache key structure** (`architecture/evm/json_rpc_cache.go:L1095-L1109`):
- Primary key (groupKey): `"<networkId>:<blockRef>"` (e.g. `"evm:1:19827314"` or `"evm:1:*"`)
- Range key (requestKey): `req.CacheHash()` — a hash of the method + params
- Wildcard `blockRef == "*"` uses `ConnectorReverseIndex`; concrete refs use `ConnectorMainIndex`

### Observability

**Prometheus metrics** (labels as documented in `telemetry/metrics.go`):

| metric | labels | trigger |
|---|---|---|
| `erpc_cache_get_age_guard_reject_total` | project, network, method, connector, policy, ttl | Cached realtime result rejected because block timestamp age > TTL (`architecture/evm/json_rpc_cache.go:L910`) |
| `erpc_cache_get_success_hit_total` | project, network, category, connector, policy, ttl | Cache GET hit (`architecture/evm/json_rpc_cache.go:L536`) |
| `erpc_cache_get_success_miss_total` | project, network, category, connector, policy, ttl | Cache GET miss (`architecture/evm/json_rpc_cache.go:L483,507`) |
| `erpc_cache_get_error_total` | project, network, category, connector, policy, ttl, error | Cache GET connector error (`architecture/evm/json_rpc_cache.go:L294`) |
| `erpc_cache_get_skipped_total` | project, network, category | Cache GET skipped — no matching policy (`architecture/evm/json_rpc_cache.go:L189`) |
| `erpc_cache_set_success_total` | project, network, category, connector, policy, ttl | Cache SET succeeded (`architecture/evm/json_rpc_cache.go:L794`) |
| `erpc_cache_set_error_total` | project, network, category, connector, policy, ttl, error | Cache SET failed (`architecture/evm/json_rpc_cache.go:L700,775`) |
| `erpc_cache_set_skipped_total` | project, network, category, connector, policy, ttl | Cache SET skipped by policy (`architecture/evm/json_rpc_cache.go:L722`) |
| `erpc_cache_set_original_bytes_total` | project, network, category, connector, policy, ttl | Uncompressed bytes on cache SET (`architecture/evm/json_rpc_cache.go:L736`) |
| `erpc_cache_set_compressed_bytes_total` | project, network, category, connector, policy, ttl | Compressed bytes on cache SET (`architecture/evm/json_rpc_cache.go:L756`) |
| `erpc_upstream_attempt_outcome_total` | project, network, upstream, category, outcome, is_hedge, is_retry, finality | One increment per upstream attempt with its terminal outcome (`upstream/upstream.go:L551`) |

**Trace spans** (all start with `common.StartDetailSpan` unless noted):
- `Project.PreForwardHook` — `HandleProjectPreForward` span (`architecture/evm/hooks.go:L15`)
- `Network.PreForwardHook` — `HandleNetworkPreForward` span (`architecture/evm/hooks.go:L43`)
- `Network.PostForwardHook` — `HandleNetworkPostForward` span (`architecture/evm/hooks.go:L65`)
- `Upstream.PreForwardHook` — `HandleUpstreamPreForward` span (`architecture/evm/hooks.go:L88`)
- `Upstream.PostForwardHook` — `HandleUpstreamPostForward` span (`architecture/evm/hooks.go:L111`)
- `Cache.Get` — `common.StartSpan` (non-detail) with `network.id` attribute (`architecture/evm/json_rpc_cache.go:L153`)
- `Cache.Set` — `common.StartSpan` with `upstream.id` attribute (`architecture/evm/json_rpc_cache.go:L568`)
- `Cache.FindGetPolicies` — detail span inside Cache.Get
- `Cache.GetForPolicy` — per-connector-goroutine detail span with `cache.policy_summary`, `cache.connector_id`, `cache.method`
- `Evm.ExtractBlockReferenceFromRequest` — detail span (`architecture/evm/block_ref.go:L19`)
- `Evm.ExtractBlockReferenceFromResponse` — detail span (`architecture/evm/block_ref.go:L118`)
- `Evm.extractRefFromJsonRpcRequest` — detail span
- `Evm.extractRefFromJsonRpcResponse` — detail span
- `Evm.ExtractBlockTimestampFromResponse` — detail span
- `Upstream.PostForwardHook.eth_sendRawTransaction` — detail span in idempotency path

**Log messages** (zerolog):
- `"interpolated block tag to concrete block number"` — Debug — tag resolved, includes method, tag, resolvedHex, resolvedNumber, path, networkId (`architecture/evm/json_rpc.go:L228`)
- `"passed through block tag"` — Trace — tag not interpolated (`architecture/evm/json_rpc.go:L285`)
- `"will not cache the response because we cannot resolve a block reference"` — Debug/Trace — block ref empty on cache.Set (`architecture/evm/json_rpc_cache.go:L643-L658`)
- `"rejecting cached result because block age exceeds policy TTL"` — Debug — age guard rejection (`architecture/evm/json_rpc_cache.go:L900`)
- `"compressed cache value"` — Debug — includes originalSize, compressedSize, savings% (`architecture/evm/json_rpc_cache.go:L748`)
- `"cache connector errored during GET"` — Debug — connector returned error in fan-out (`architecture/evm/json_rpc_cache.go:L312`)
- `"returning cached response"` — Trace/Debug — cache hit served (`architecture/evm/json_rpc_cache.go:L553-L562`)
- `"initializing evm json rpc cache..."` — Info — on `NewEvmJsonRpcCache` (`architecture/evm/json_rpc_cache.go:L40`)

### Edge cases & gotchas

1. **Architecture is "evm"-only for now.** `IsValidArchitecture()` and `IsValidNetwork()` at `common/network.go:L35-L49` reject anything that is not `"evm"`. Adding a new architecture requires changes in these validation functions, the HTTP routing path, and providing hook implementations in a new `architecture/<name>/` package.

2. **"safe" and "pending" tags are intentionally NOT interpolated** (`architecture/evm/json_rpc.go:L76-L82`). eRPC does not track the "safe" checkpoint (between latest and finalized) or mempool state. Attempts to configure `translateLatestTag: false` on a method suppress all tag interpolation for that method.

3. **Block ref wildcard `"*"` for transaction lookups changes connector behavior.** When `blockRef == "*"`, the cache calls `connector.Get(ctx, ConnectorReverseIndex, ...)` (`architecture/evm/json_rpc_cache.go:L1013-L1017`). This is required because `eth_getTransactionByHash` requests don't know the block number ahead of time. Connectors that do not implement reverse indexing will miss these entries.

4. **ExecState.Snapshot() is eventually consistent.** Each counter is loaded independently; under high concurrency, `Snapshot().Attempts` may temporarily differ from `UpstreamAttempts + CacheAttempts`. This is documented as acceptable for observability use (`common/exec_state.go:L211-L215`).

5. **`NetworkAttempts` is NOT summed into the total.** This is a common confusion point: `NetworkAttempts` counts upstream rotations (how many times the network executor picked a different upstream), but each rotation already generates one `UpstreamAttempts` increment. Summing both would double-count physical attempts (`common/exec_state.go:L82-L100`).

6. **MarkEmptyAsErrorMethods only fires when `RetryEmpty` directive is true.** Even if a method is in the list, the conversion to `ErrEndpointMissingData` is gated on `req.Directives().RetryEmpty` (`architecture/evm/common.go:L23-L25`). Operators must set `retryEmpty: true` in the failsafe retry policy (or as a directive default) for this feature to activate for their requests.

7. **An explicitly-set empty `markEmptyAsErrorMethods: []` disables the feature entirely** — including for methods that are in the default list. The nil check in `SetDefaults` (`common/defaults.go:L2110-L2112`) means a non-nil empty slice is never replaced by the default. This is tested at `architecture/evm/hooks_test.go:L187-L199`.

8. **Custom `markEmptyAsErrorMethods` completely replaces the default set.** There is no merge/extend. If an operator sets `markEmptyAsErrorMethods: ["custom_method"]`, none of the default 11 methods will trigger empty-to-error conversion. Tested at `architecture/evm/hooks_test.go:L160-L185`.

9. **`eth_getBlockByHash` is intentionally excluded from `DefaultMarkEmptyAsErrorMethods`.** Subgraph-based upstreams commonly return null for this method (they index by number, not hash). Including it would cause spurious retry loops (`common/defaults.go:L2033`).

10. **`eth_getTransactionReceipt` and `eth_getBlockReceipts` are excluded from the default mark-empty list** because pending transactions legitimately return null for receipts, and zero-tx blocks return empty arrays for `eth_getBlockReceipts`. Adding these to the list would trigger false missing-data retries for valid empty responses (`common/defaults.go:L2035-L2043`).

11. **`emptyResultBeyondConfidence` fails open.** When the network head is unknown (poller hasn't bootstrapped yet, or polled 0 blocks), it returns `false` — meaning the empty result IS treated as retryable. This prevents spurious caching of null results during startup but allows retry loops while the head is unknown (`architecture/evm/common.go:L80-L87`).

12. **Error normalizer: nonce/duplicate checks must precede rejected/gas checks.** Some providers use JSON-RPC code `-32003` for both "already known" and "out-of-gas". The normalizer checks "already known" and "nonce too low" first (lines 303-341) before the generic `-32003` branch (lines 368-392) to ensure idempotency detection is not masked (`architecture/evm/error_normalizer.go:L299`).

13. **OP Stack "sender is over rate limit" is non-network-retryable.** All providers route to the same OP Stack sequencer, so retrying the same transaction on a different upstream would fail identically. The error is `WithRetryableTowardNetwork(false)` (`architecture/evm/error_normalizer.go:L124-L139`).

14. **Malformed RLP transactions are non-network-retryable.** If a raw transaction has invalid encoding (typed transaction too short, RLP errors), the error is `WithRetryableTowardNetwork(false)` — no upstream would accept it (`architecture/evm/error_normalizer.go:L484-L499`).

15. **JavaScript Runtime is single-threaded; each goroutine needs its own.** Sobek is not goroutine-safe. The selection policy system uses a per-goroutine pool (`internal/policy/runtime_pool.go:L51`). Any use of `common.Runtime` must NOT be shared across goroutines.

16. **`deepCopyParams` is called only when changes are needed.** `NormalizeHttpJsonRpc` defers the copy until it has confirmed at least one mutation is required (`architecture/evm/json_rpc.go:L308-L320`). Unchanged requests skip both the copy and the write-lock acquisition entirely — important for read-heavy trace/debug methods with large param objects.

17. **cache.Get fan-out has a 30s defensive backstop** when the caller has no deadline (`architecture/evm/json_rpc_cache.go:L219-L226`). This prevents FD/connection-pool leaks from hung connectors that lack a failsafe timeout of their own. Properly configured connectors should exit far earlier via their own timeout.

18. **Cache compression only occurs when compressed bytes < original bytes.** The compressor will not store the compressed form if the compression overhead exceeds the savings (`architecture/evm/json_rpc_cache.go:L1143-L1147`). The zstd magic number `[0x28, 0xB5, 0x2F, 0xFD]` is used to detect pre-compressed data on read (`architecture/evm/json_rpc_cache.go:L1151-L1157`).

19. **`FullySyncedThreshold = 4`** at `architecture/evm/evm_state_poller.go:L21` — a self-hosted node must return `eth_syncing: false` four consecutive times before eRPC stops sending syncing-check requests for that upstream. One `syncing=true` resets the counter to 1.

20. **`DefaultToleratedBlockHeadRollback = 1024`** at `architecture/evm/evm_state_poller.go:L26-L28` — the maximum block-head rollback the poller tolerates before treating it as a large rollback event (gauge metric `erpc_upstream_block_head_large_rollback`). Lazy-loaded networks may not have this value available when the poller is first initialized.

21. **200-OK revert detection uses `dt[1:11]` not `dt[0:10]`.** In `ExtractJsonRpcError`, the check for a self-contained EVM revert payload in a successful response is `dt[1:11] == "0x08c379a0"` (`architecture/evm/error_normalizer.go:L613`). The raw string `dt` is produced by `jr.GetResultString()` which returns the JSON-encoded value — the first byte (`dt[0]`) is the JSON double-quote character `"`. The actual `0x` hex string therefore begins at index 1. Anyone writing tests or tooling that mocks `GetResultString()` or inspects `jr.Result` directly must account for this: the JSON-quoted form `"0x08c379a0..."` has the selector at positions 1-10, while a Go `[]byte` of the decoded hex would start with the selector at index 0. A check starting at index 0 would compare `"0` against `0x`, which never matches. (`architecture/evm/error_normalizer.go:L609-L624`)

### Source map

- `common/architecture_evm.go` — `EvmUpstream` interface, `EvmStatePoller` interface, `EvmNodeType`, `EvmSyncingState`, `AvailbilityConfidence`, `EvmStatePollerDiagnostics`, `EvmProbeEarliestInfo` types
- `common/network.go` — `Network` interface, `NetworkArchitecture` type, `ArchitectureEvm` constant, `IsValidArchitecture()`, `IsValidNetwork()`
- `common/exec_state.go` — `ExecState` struct (per-request execution counter model), `UpstreamAttempt`, `UpstreamAttemptOutcome`, `UpstreamSelectionReason`, `ExecStateSnapshot`, `Apply(span)`
- `common/runtime.go` — `Runtime` wrapper over Sobek JS engine; `process.env`, `console` injection
- `common/console.go` — `console` JS object implementation (debug/info/log/trace/warn → zerolog)
- `common/compiler.go` — `CompileTypeScript`, `CompileFunction`, `CompileProgram` (esbuild + sobek)
- `common/object.go` — `MapJavascriptObjectToGo`, `setJsValueToGoField` — JS→Go type bridge with special cases for Duration, RateLimitPeriod
- `common/config.go:L2177-L2306` — `EvmNetworkConfig`, `EvmServedTipConfig`, `EvmIntegrityConfig` types
- `common/defaults.go:L1975-L2115` — `EvmNetworkConfig.SetDefaults()`, `DefaultMarkEmptyAsErrorMethods()`, default constants
- `architecture/evm/hooks.go` — Four hook dispatch entry-points (HandleProjectPreForward, HandleNetworkPreForward, HandleNetworkPostForward, HandleUpstreamPostForward); `isMethodInMarkEmptyList`
- `architecture/evm/common.go` — `upstreamPostForward_markUnexpectedEmpty`, `emptyResultBeyondConfidence`, `normalizeEmptyArrayResponse`
- `architecture/evm/json_rpc.go` — `NormalizeHttpJsonRpc`, `resolveBlockTagToHex`, `deepCopyParams`, `replaceParamAtPath`, `setByPath`
- `architecture/evm/block_ref.go` — `ExtractBlockReferenceFromRequest`, `ExtractBlockReferenceFromResponse`, `ExtractBlockTimestampFromResponse`, `extractRefFromJsonRpcRequest`, `extractRefFromJsonRpcResponse`, `getMethodConfig`, `parseCompositeBlockParam`
- `architecture/evm/json_rpc_cache.go` — `EvmJsonRpcCache` (GET/SET fan-out, policy matching, compression, age guard)
- `architecture/evm/error_normalizer.go` — `ExtractJsonRpcError`, `ExtractGrpcError`, `getVendorSpecificErrorIfAny`
- `architecture/evm/extractor.go` — `JsonRpcErrorExtractor` adapter implementing `common.JsonRpcErrorExtractor`
- `architecture/evm/util.go` — `IsNonRetryableWriteMethod`, `IsMissingDataError`
- `architecture/evm/evm_state_poller.go` — `EvmStatePoller` concrete implementation, `FullySyncedThreshold`, `DefaultToleratedBlockHeadRollback`
- `architecture/evm/eth_sendRawTransaction.go` — `isIdempotentBroadcastDisabled`, `upstreamPostForward_eth_sendRawTransaction`, `networkPostForwardVerifyTimeout` (3s)
- `architecture/evm/hooks_test.go` — tests for `HandleUpstreamPostForward` with mark-empty-as-error variants (listed/unlisted methods, RetryEmpty flag, custom and empty config)
