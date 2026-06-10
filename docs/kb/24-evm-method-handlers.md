# KB: EVM per-method handlers & normalization

> status: complete
> source-dirs: architecture/evm/hooks.go, architecture/evm/eth_call.go, architecture/evm/eth_chainId.go, architecture/evm/eth_blockNumber.go, architecture/evm/eth_getBlockByNumber.go, architecture/evm/eth_getBlockReceipts.go, architecture/evm/eth_sendRawTransaction.go, architecture/evm/trace_filter.go, architecture/evm/eth_query.go, architecture/evm/eth_query_helpers.go, architecture/evm/eth_query_shim.go, architecture/evm/common.go, architecture/evm/extractor.go, architecture/evm/json_rpc.go, architecture/evm/error_normalizer.go, architecture/evm/hooks_test.go, architecture/evm/future_block_empty_test.go, architecture/evm/eth_sendRawTransaction_test.go, architecture/evm/trace_filter_normalize_test.go, architecture/evm/trace_filter_test.go, common/config.go, common/defaults.go, common/errors.go, telemetry/metrics.go

## L1 — Capability (CTO view)

eRPC's EVM per-method handlers are a plugin layer that intercepts JSON-RPC requests before and after upstream forwarding to add protocol-correctness guarantees that no individual upstream can provide. They short-circuit well-known cheap queries (eth_chainId, eth_blockNumber), validate and repair block-tag parameters for caching purity, protect against stale or missing block data, make eth_sendRawTransaction idempotent across retries, auto-split oversized trace_filter requests, and expose a synthetic eth_query* family of high-level read APIs built on standard primitives. Together they allow eRPC to present a single, semantically consistent EVM endpoint regardless of which individual nodes are behind it.

## L2 — Mechanics (staff-engineer view)

**Hook topology.** There are four hook layers, each with a pre/post variant, ordered around the cache and failsafe retry loop:

1. **Project.PreForward** (`HandleProjectPreForward`, `architecture/evm/hooks.go:L10`) — fires before cache read. Handles `eth_blockNumber` (returns highest known, replaces real response), `eth_call` (injects missing block param), `eth_chainId` (responds from config), `eth_getLogs`, and `trace_filter`/`arbtrace_filter` (records range histogram).
2. **Network.PreForward** (`HandleNetworkPreForward`, `architecture/evm/hooks.go:L40`) — fires after upstream selection. Handles `eth_getLogs`, `eth_chainId` (responds from config), and `trace_filter`/`arbtrace_filter` (proactive auto-splitting when range exceeds per-upstream threshold).
3. **Upstream.PreForward** (`HandleUpstreamPreForward`, `architecture/evm/hooks.go:L86`) — fires once per upstream attempt. Handles `eth_getLogs` (block range availability), `eth_chainId` (responds from upstream or network config), `trace_filter`/`arbtrace_filter` (block range availability), and `eth_query*` (shim translation).
4. **Upstream.PostForward** (`HandleUpstreamPostForward`, `architecture/evm/hooks.go:L109`) — fires after each upstream response. Handles `eth_getLogs`, `eth_getBlockReceipts`, `eth_getBlockByNumber`/`eth_getBlockByHash`, and `trace_filter`/`arbtrace_filter`. Also applies the configurable `markEmptyAsErrorMethods` gate to convert null/empty results to retryable `ErrEndpointMissingData`. `eth_sendRawTransaction` is a special case: it always passes through the upstream hook regardless of error.
5. **Network.PostForward** (`HandleNetworkPostForward`, `architecture/evm/hooks.go:L62`) — fires once after the failsafe loop. Handles `eth_getBlockByNumber`, `eth_getLogs`, `eth_sendRawTransaction`, and `trace_filter`/`arbtrace_filter`.

**Block-tag interpolation.** `NormalizeHttpJsonRpc` (`architecture/evm/json_rpc.go:L92`) runs on every request before upstream selection. It reads per-method `ReqRefs` path configurations to find block parameters, resolves `"latest"` to the highest known latest block number and `"finalized"` to the highest known finalized block number (if available), normalizes numeric hex representations to canonical form, caches the resulting block number in the request for downstream cache key generation, and sets `EvmBlockRef` metadata (`"latest"`, `"finalized"`, or `"*"` when both appear). `"safe"` and `"pending"` are intentionally passed through unchanged. Resolution is gated by the `SkipInterpolation` directive and per-method `TranslateLatestTag`/`TranslateFinalizedTag` flags. The function avoids lock contention by extracting param values under a read lock, doing the resolution unlocked, then applying writes under a write lock only if changes are needed; params are deep-copied only when mutations are required (`architecture/evm/json_rpc.go:L307-L320`).

**Future/missing block guard (emptyResultBeyondConfidence).** `emptyResultBeyondConfidence` (`architecture/evm/common.go:L62`) is called by both the upstream-level `markUnexpectedEmpty` check and the network-level `enforceNonNullBlock`. It compares the request's cached block number against the network's `EmptyResultConfidence` head (defaults to `EvmHighestLatestBlockNumber`; can be set to `EvmHighestFinalizedBlockNumber`). If the requested block is strictly above the head, the block does not yet exist and the empty result is truthful — no retry is triggered. If the head is unknown (0) the guard fails open, treating any block as below confidence. This ensures requests for not-yet-produced blocks receive the correct empty/null response without triggering retry storms.

**eth_sendRawTransaction idempotency.** The upstream-level hook (`upstreamPostForward_eth_sendRawTransaction`, `architecture/evm/eth_sendRawTransaction.go:L51`) intercepts `ErrCodeEndpointNonceException` errors. "Already known" → synthesize success with txHash. "Nonce too low" → probe `eth_getTransactionByHash` against the same upstream to determine whether the same tx is already mined (synthetic success) or a conflicting tx was mined (re-raise as `-32003`). The network-level hook (`networkPostForward_eth_sendRawTransaction`, `architecture/evm/eth_sendRawTransaction.go:L273`) fires after the failsafe loop exhausts all attempts (`ErrCodeUpstreamsExhausted`, `ErrCodeFailsafeRetryExceeded`, or `ErrCodeFailsafeTimeoutExceeded`). It re-probes `eth_getTransactionByHash` across the network using `context.WithoutCancel` so the parent deadline doesn't prevent the probe. The returned tx hash is cross-checked against the locally-derived hash (from `tx.UnmarshalBinary`) before granting synthetic success, preventing byzantine upstreams from faking success.

**trace_filter auto-splitting.** Two splitting modes:
- **Proactive** (`networkPreForward_trace_filter`): if the request range exceeds the minimum `TraceFilterAutoSplittingRangeThreshold` across selected upstreams, the request is immediately split into contiguous sub-requests of at most that threshold size and dispatched concurrently (`architecture/evm/trace_filter.go:L117-L220`). Re-entrancy is blocked via `ParentRequestId`.
- **Reactive** (`networkPostForward_trace_filter`): if an upstream returns a range-too-large error (`ErrCodeEndpointRequestTooLarge` or `JsonRpcErrorEvmLargeRange`) and `TraceFilterSplitOnError` is enabled, `splitTraceFilterRequest` bisects the request. Split order: block range first (halve), then `fromAddress` list, then `toAddress` list; errors if no further split is possible (single block + single/no addresses).

Sub-requests are dispatched concurrently via a semaphore of size `TraceFilterSplitConcurrency` (default 10), collected in index order, and merged by concatenating the result arrays (all disjoint by construction). Any sub-failure fails the whole merged response.

**eth_query shim.** Five synthetic methods (`eth_queryBlocks`, `eth_queryTransactions`, `eth_queryLogs`, `eth_queryTraces`, `eth_queryTransfers`) are translated to standard calls at the upstream level. The shim is configured per-upstream under `upstreams[].evm.queryShim` and only activates when `enabled: true`. `AllowedMethods` filters which query methods this upstream handles (wildcard match; empty = all). The shim parses a rich request object (fromBlock/toBlock as tags or hex, limit, cursor pagination, order asc/desc, filter, fields), resolves block tags to concrete numbers, fetches blocks concurrently, applies in-memory filtering and field projection, and returns a structured response with `data`, `fromBlock`, `toBlock`, and `cursorBlock` pagination fields.

For `eth_queryTraces`, the shim first tries `trace_block`; if the method is unsupported, falls back to `debug_traceBlockByNumber` with `callTracer`. Trace data is normalized to a canonical format via the `blockchain-data-standards/manifesto/evm` library. `eth_queryTransfers` delegates to `eth_queryTraces` internally and converts trace results to native transfer objects.

## L3 — Exhaustive reference (agent view)

### Method-specific subsections

#### eth_call

**Pre-forward (Project level):** If the request has exactly one param (missing block tag), appends `"latest"` as the second param (`architecture/evm/eth_call.go:L9-L32`). Then calls `network.Forward` and returns `handled=true`. This prevents upstreams that require the block parameter from failing.

**Note:** `eth_call` is excluded from `markEmptyAsErrorMethods` and from the include-error-details wrapping (callers expect string revert data, not a structured error object).

---

#### eth_chainId

**Pre-forward (Project level):** If `ShouldSkipCacheRead("")` is false and `network.Config.Evm.ChainId != 0`, returns a synthetic `NormalizedResponse` with the hex-encoded chainId without contacting any upstream (`architecture/evm/eth_chainId.go:L25-L67`). Span: `Project.PreForwardHook.eth_chainId`.

**Pre-forward (Network level):** Same logic, runs after upstream selection. Gates on `ShouldSkipCacheRead("")` and `network.Config.Evm.ChainId != 0` (`architecture/evm/eth_chainId.go:L70-L112`). Span: `Network.PreForwardHook.eth_chainId`.

**Pre-forward (Upstream level):** Same logic; prefers upstream's `uc.Evm.ChainId`, falls back to parsing from `upstreamNetworkId` (`"evm:123"`), then falls back to network config. Logs span `Upstream.PreForwardHook.eth_chainId` (`architecture/evm/eth_chainId.go:L116-L178`).

**Footgun:** All three layers skip when `ShouldSkipCacheRead("")` is true. This allows force-refresh calls to reach real upstreams for validation.

---

#### eth_blockNumber

**Pre-forward (Project level):** Controlled by `network.Config.Evm.Integrity.EnforceHighestBlock` (default `true`). Steps: (1) forward the request normally, (2) if response is from cache return it as-is, (3) parse block number from response, (4) get `EvmHighestLatestBlockNumber` from network's known poller state, (5) if highest known > upstream response, replace the response with a synthetic one containing the highest known block number and increment `erpc_upstream_stale_latest_block_total` (`architecture/evm/eth_blockNumber.go:L15-L121`). Span: `Project.PreForwardHook.eth_blockNumber`.

**Config:** `networks[].evm.integrity.enforceHighestBlock` (default: `true`). When false, the hook returns `handled=false` immediately.

---

#### eth_getBlockByNumber / eth_getBlockByHash

**Post-forward (Network level):** Runs two checks (`architecture/evm/eth_getBlockByNumber.go:L43-L109`):

1. **`enforceHighestBlock`:** Only when `dirs.EnforceHighestBlock` is true (from `DirectiveDefaults`). Only for `"latest"` or `"finalized"` block tags. If response is from cache, skips. If response block number < network's known highest, re-issues `BuildGetBlockByNumberRequest(highestBlockNumber, itx)` with `SkipCacheRead=true` and the current upstream excluded (when the original response contained a real block number), then calls `pickHighestBlock` to resolve between old and new response. Increments `erpc_upstream_stale_latest_block_total` (latest) or `erpc_upstream_stale_finalized_block_total` (finalized).

2. **`enforceNonNullBlock`:** Converts null/empty results to `ErrEndpointMissingData`. For numeric block params: always errors (genuinely missing/pruned). For tag params (e.g. `"latest"`): only errors when `dirs.EnforceNonNullTaggedBlocks` is true. Both branches are guarded by `emptyResultBeyondConfidence`; if the block number is above the confidence head the null is returned as-is.

**Post-forward (Upstream level):** Runs block validation via `validateBlock` when `dirs` is non-nil and result is non-empty (`architecture/evm/eth_getBlockByNumber.go:L423-L457`). Validation checks are opt-in via directives:
- `ValidateTransactionsRoot`: compares `transactionsRoot` against canonical/zero empty root vs transaction count. Handles Polygon phantom transactions (from=0x0, gas=0x0).
- `ValidateHeaderFieldLengths`: hash (32 bytes), parentHash (32 bytes), stateRoot (32 bytes), transactionsRoot (32 bytes), receiptsRoot (32 bytes), logsBloom (256 bytes).
- `ValidateTransactionFields` (full tx responses only): tx hash length (32 bytes) and uniqueness.
- `ValidateTransactionBlockInfo` (full tx responses only): blockHash, blockNumber, and transactionIndex match array position.

**`markUnexpectedEmpty` (Upstream level, gated by `MarkEmptyAsErrorMethods`):** `eth_getBlockByNumber` is in the default list. When `RetryEmpty=true` directive is set, converts null/empty to `ErrEndpointMissingData`. Guards against beyond-confidence blocks via `emptyResultBeyondConfidence`.

**Span:** `Network.PostForward.eth_getBlockByNumber`, `Upstream.PostForwardHook.eth_getBlockByNumber`.

**`pickHighestBlock` helper:** Compares `result.number` (hex) between two responses; returns the higher. If one response is null/empty, returns the other. Both null → returns the error from the re-forward (`architecture/evm/eth_getBlockByNumber.go:L335-L415`).

---

#### eth_getBlockReceipts

**Post-forward (Upstream level):** `upstreamPostForward_eth_getBlockReceipts` (`architecture/evm/eth_getBlockReceipts.go:L17-L53`). First applies `markUnexpectedEmpty` check if `eth_getBlockReceipts` is in `MarkEmptyAsErrorMethods` (NOT in default list — empty arrays are legitimate for 0-tx blocks). Then runs `validateGetBlockReceipts` when `dirs` is non-nil and result is non-empty.

**Always-on integrity checks (no directive required):**
- Duplicate `transactionHash` detection (if field present).
- All receipts must reference the same `blockHash` (if field present).

**Directive-gated checks:**
- `ReceiptsCountExact *int64`: exact count match.
- `ReceiptsCountAtLeast *int64`: minimum count.
- `ValidationExpectedBlockHash string`: all receipts' blockHash must equal this.
- `ValidationExpectedBlockNumber *int64`: all receipts' blockNumber must equal this.
- `ValidateTxHashUniqueness bool`: stricter than always-on; also rejects empty hashes.
- `ValidateTransactionIndex bool`: `transactionIndex` must equal array position.
- `ValidateReceiptTransactionMatch + GroundTruthTransactions`: cross-validates tx hashes against a ground truth tx list (library-mode only).
- `ValidateContractCreation bool` (requires `ValidateReceiptTransactionMatch`): contract creation txs (no `to`) must have non-null `contractAddress`; regular txs must not.
- `ValidateLogsBloomEmptiness bool`: consistency between `logsBloom` zero-ness and `logs` array.
- `ValidateLogFields bool`: address length (20 bytes), topic count (max per `evm.MaxTopics`), topic lengths (32 bytes), log block/tx hash/index matches receipt.
- `EnforceLogIndexStrictIncrements bool`: `logIndex` must be contiguous across all logs in all receipts.
- `ValidateLogsBloomMatch bool`: recalculates bloom from log addresses and topics using the 2048-bit keccak formula; must match `logsBloom` exactly.

**Span:** `Upstream.PostForwardHook.eth_getBlockReceipts`.

---

#### eth_sendRawTransaction

**Upstream-level hook** (`upstreamPostForward_eth_sendRawTransaction`, `architecture/evm/eth_sendRawTransaction.go:L51`):

Flow when error is not nil:
1. If `isIdempotentBroadcastDisabled` → return original error unchanged.
2. If error is not `ErrCodeEndpointNonceException` → return original error.
3. Extract `nonceExceptionReason` from error details.
4. Parse tx hash from request using `ethtypes.Transaction.UnmarshalBinary` (handles EIP-2718 typed txs including EIP-1559).
5. `NonceExceptionReasonAlreadyKnown` → `createSyntheticSuccessResponse(txHash)` (no upstream call).
6. `NonceExceptionReasonNonceTooLow` → `verifyAndHandleNonceTooLow`: calls `eth_getTransactionByHash` on the same upstream; if found → synthetic success, if not found → `-32003` (Transaction rejected, `retryableTowardNetwork=false`).
7. Unknown reason → return original error.

**Network-level hook** (`networkPostForward_eth_sendRawTransaction`, `architecture/evm/eth_sendRawTransaction.go:L273`):

Only activates when error has code `ErrCodeUpstreamsExhausted`, `ErrCodeFailsafeRetryExceeded`, or `ErrCodeFailsafeTimeoutExceeded` (exhausted-class errors indicating the broadcast may have reached a mempool but caused retry failures). Steps:
1. If `isIdempotentBroadcastDisabled` → return original error.
2. Extract tx hash from request.
3. Issue `eth_getTransactionByHash` via `n.Forward` using `context.WithoutCancel(ctx)` + 3-second independent timeout (`networkPostForwardVerifyTimeout`). This allows the probe to run even when the parent context is already cancelled/expired.
4. If tx found: cross-check returned `hash` field (case-insensitive) against locally-derived hash. Mismatch → return original error. Match → `createSyntheticSuccessResponse`.
5. If tx not found or probe fails → return original error.

**Config field:** `networks[].evm.idempotentTransactionBroadcast` (`*bool`, default `true` — nil means enabled; only `false` disables). Source: `architecture/evm/eth_sendRawTransaction.go:L30-L36`, `common/config.go:L2239`.

**Normalizer N:yes override for reverts and out-of-gas (but NOT insufficient funds).**
`ErrEndpointExecutionException` is `retryableTowardNetwork=false` by default (N:no). The error normalizer overrides this for `eth_sendRawTransaction` in two specific rule groups:

- **Revert/VM-execution rules** (`architecture/evm/error_normalizer.go:L249-L272`): When the error message contains `"reverted"`, `"VM execution error"`, `"transaction: revert"`, `"VM Exception"`, or `"intrinsic gas too high"`, the normalizer creates `ErrEndpointExecutionException` and then checks if the method is `eth_sendRawTransaction`; if so, calls `WithRetryableTowardNetwork(true)` to flip N:no → N:yes. Rationale: different providers may have different pre-check/simulation logic, so a revert on one might succeed on another.
- **Out-of-gas/-32003 rules** (`architecture/evm/error_normalizer.go:L368-L391`): Same N:yes flip for `out of gas`, `gas too low`, `IntrinsicGas`, and exact `-32003` code matches when the method is `eth_sendRawTransaction`.
- **Insufficient-funds rule** (`architecture/evm/error_normalizer.go:L350-L361`): `"insufficient funds"` / `"insufficient balance"` creates `ErrEndpointExecutionException` but does NOT apply the N:yes flip. It stays N:no because an insufficient-balance error is a deterministic client-side failure — retrying on a different upstream would yield the same result.
- **Nonce errors** are handled separately as `ErrEndpointNonceException` (see below) and never treated as `ErrEndpointExecutionException`.

The ordering in the normalizer is: revert → nonce (must precede -32003) → insufficient-funds → out-of-gas/-32003. Source: `architecture/evm/error_normalizer.go:L246-L392`.

**ErrEndpointNonceException — reason fields, N:no semantics, and ordering constraint.**
`ErrEndpointNonceException` (`common/errors.go:L2763`) is produced by the normalizer for nonce-conflict messages at code `-32003` and related codes. It carries two reason constants:

- `NonceExceptionReasonAlreadyKnown` (`"already_known"`, `common/errors.go:L2754`): The identical transaction is already in the mempool or already mined. The upstream-level hook converts this to synthetic success immediately (no verification probe needed). Message patterns matched: `"already known"`, `"known transaction"`, `"already imported"`, `"transaction already in mempool"`, `"tx already in mempool"`, `"already in the mempool"`, `"transaction already exists"`, `"already have transaction"`, `"already exists in mempool"` (normalizer `architecture/evm/error_normalizer.go:L304-L323`).
- `NonceExceptionReasonNonceTooLow` (`"nonce_too_low"`, `common/errors.go:L2756`): The nonce has been used by a *different* transaction. The upstream-level hook probes `eth_getTransactionByHash` to distinguish "same tx already mined" (synthetic success) from "conflicting tx with same nonce" (re-raise `-32003`). Message patterns matched: `"nonce too low"`, `"nonce is too low"`, `"nonce has already been used"` (normalizer `architecture/evm/error_normalizer.go:L326-L340`).

The `Details` map on `ErrEndpointNonceException` always has `"retryableTowardNetwork": false` (`common/errors.go:L2773`). This means N:no: if `already_known` is returned and the upstream-level hook doesn't intercept (e.g., because `idempotentTransactionBroadcast=false`), the error will NOT be retried on another upstream. The idempotency logic relies on this — once "already known" is established, trying another upstream would either echo the same result (if tx is globally mempool-known) or erroneously succeed.

**Critical ordering constraint:** The nonce detection rules (rules 11/12) MUST appear in the normalizer code before the generic `-32003` handler. Some vendors return `-32003` for "already known" or "nonce too low" messages. If the generic `-32003` rule fired first it would classify them as `ErrEndpointClientSideException` or `ErrEndpointServerSideException`, bypassing the idempotency machinery entirely. The comment at `architecture/evm/error_normalizer.go:L297-L301` makes this ordering constraint explicit: "This must come BEFORE the generic -32003 handling below, because some upstreams use -32003 for 'already known' or 'nonce too low' messages."

**Tx hash extraction:** Supports both legacy (type-0, RLP) and EIP-2718 typed transactions (EIP-1559 type-2, EIP-2930 type-1, etc.) via `ethtypes.Transaction.UnmarshalBinary`.

**Span:** `Upstream.PostForwardHook.eth_sendRawTransaction`, `Network.PostForward.eth_sendRawTransaction`.

---

#### trace_filter / arbtrace_filter

Both method names are treated identically (`TraceFilterMethods = []string{"trace_filter", "arbtrace_filter"}`, `architecture/evm/trace_filter.go:L22`).

**Pre-forward (Project level):** Records `erpc_network_evm_trace_filter_range_requested` histogram with labels `{project, network, method, user, finality}`. Never short-circuits (`handled=false` always).

**Pre-forward (Network level):** Proactive auto-splitting when the request's block range exceeds `min(TraceFilterAutoSplittingRangeThreshold)` across selected upstreams (only positive thresholds count; if all are zero, no split). Skips sub-requests (`ParentRequestId != nil`) and composite requests to avoid re-entrancy. Block tags are resolved to concrete numbers; if either bound is unresolvable, the request passes through without splitting. `fromBlock > toBlock` → immediate `-32600` (InvalidRequest). Split granularity: chunks of `effectiveThreshold` blocks, dispatched concurrently. Sets `CompositeTypeTraceFilterSplitProactive` on the request.

**Pre-forward (Upstream level):** Block range availability check using `CheckBlockRangeAvailability` (same logic as eth_getLogs). Gated by `network.Config.Evm.Integrity.EnforceGetLogsBlockRange` (default `true`). `fromBlock > toBlock` → `-32600`. Only runs when both bounds are hex-prefixed (0x) concrete numbers.

**Post-forward (Upstream level):** Normalizes null/empty/`""` / `{}` result to `[]` (empty array). Passes through when there is an error. This prevents consumer breakage on upstreams that return `null` for empty ranges.

**Post-forward (Network level):** Reactive splitting. Activated only when `TraceFilterSplitOnError=true`. Only activates on `ErrCodeEndpointRequestTooLarge` or `JsonRpcErrorEvmLargeRange`. Skips sub-requests. Uses `splitTraceFilterRequest` (bisection: block range → fromAddress list → toAddress list; fails if single block + single/no addresses). Sets `CompositeTypeTraceFilterSplitOnError`. Concurrency limit: `TraceFilterSplitConcurrency` (default 10).

**Merge semantics:** Sub-results are concatenated in request order; no deduplication. The result is always `fromCache=true` only when ALL sub-responses are from cache.

**Error semantics:** Any single sub-request failure fails the entire merged response (errors joined via `errors.Join`).

---

#### eth_query* (query shim)

Five synthetic methods: `eth_queryBlocks`, `eth_queryTransactions`, `eth_queryLogs`, `eth_queryTraces`, `eth_queryTransfers`. Handled at **Upstream.PreForward** level only. Enabled per-upstream via `upstreams[].evm.queryShim.enabled=true`.

**Request shape** (single params object):
```json
{
  "fromBlock": "latest|finalized|earliest|safe|0x...",
  "toBlock": "latest|...",
  "order": "asc|desc",
  "limit": 100,
  "cursor": {"number": "0x...", "hash": "0x...", "parentHash": "0x..."},
  "fields": {"blocks": true|[...], "transactions": [...], "logs": [...], "traces": [...], "transfers": [...]},
  "filter": {
    "from": ["0x..."],
    "to": ["0x..."],
    "selector": ["0x..."],  // 4-byte function selectors for tx/trace filtering
    "address": ["0x..."],   // for eth_queryLogs
    "topics": [["0x..."]],  // for eth_queryLogs
    "isTopLevel": true      // for eth_queryTraces/Transfers: only top-level calls
  }
}
```

**Response shape:**
```json
{
  "data": {"blocks": [...], "transactions": [...], "logs": [...], "traces": [...], "transfers": [...]},
  "fromBlock": {"number": "0x...", "hash": "0x...", "parentHash": "0x..."},
  "toBlock": {"number": "0x...", "hash": "0x...", "parentHash": "0x..."},
  "cursorBlock": null | {"number": "0x...", "hash": "0x...", "parentHash": "0x..."}
}
```
`cursorBlock` is non-null only when there are more results beyond the `limit`; pass it as `cursor` in the next request for continuation.

**Block tag resolution in shim** (`resolveBlockTag`, `architecture/evm/eth_query_helpers.go:L165`): `""` or `"latest"` → `EvmHighestLatestBlockNumber`; `"finalized"` → `EvmHighestFinalizedBlockNumber`; `"safe"` → finalized if available, otherwise latest; `"earliest"` → 0; `"pending"` → error (unsupported); hex string → parsed directly.

**eth_queryBlocks:** Fetches blocks using `fetchBlockRange` (`eth_getBlockByNumber` with `fullTx=false`) concurrently. Applies `limit` at block granularity. Returns `{blocks, fromBlock, toBlock, cursorBlock}`.

**eth_queryTransactions:** Fetches blocks with `fullTx=true`. Filters transactions using `matchesTransactionFilter` (from, to, 4-byte selector). Applies limit at block boundary (page breaks between blocks, not mid-block). Returns parent blocks when `fields.blocks != nil`.

**eth_queryLogs:** Issues `eth_getLogs` for the full range, then sorts by blockNumber+logIndex. Pages at block boundaries. Optionally fetches parent blocks and transactions.

**eth_queryTraces:** Fetches blocks with `fullTx=true`, then per-block issues `trace_block`; falls back to `debug_traceBlockByNumber` with `callTracer` if `trace_block` is unsupported or returns "method not found". Geth debug format is normalized to a canonical parity-style format via `bdsevm.TraceFromGethDebug`. Filters with `matchesTraceFilter` (from, to, 4-byte selector, `isTopLevel`). `isUnsupportedTraceMethod` detects both `ErrCodeEndpointUnsupported` and messages containing "method not found" or "unsupported".

**eth_queryTransfers:** Delegates to `shimQueryTraces` (with all trace fields), then converts each trace to native transfers using `bdsevm.NativeTransfersFromTraces`. Filters the resulting transfers with `matchesTransferFilter`. Only CALL-type traces with non-zero `value` generate transfers.

**Pagination (cursor):** For `desc` order, cursor block number is subtracted by 1 for the next `fromBlock`. For `asc` order, it is incremented by 1. A `nil` `cursorBlock` means no more results.

**Method guard:** `isQueryShimMethodAllowed` checks `AllowedMethods` via wildcard match; empty list means all five methods are allowed. When `AllowedMethods` is set to an explicit non-empty list and a method is NOT in that list, `isQueryShimMethodAllowed` returns `false`, which causes the upstream pre-forward hook to return `handled=false` — the request is **not rejected**; it falls through and is forwarded to the upstream as a normal JSON-RPC call. This means the upstream will receive the raw `eth_queryBlocks`/`eth_queryTransactions`/etc. method name, which it almost certainly does not understand, resulting in a "method not found" error from that upstream. The safeguard against this is to only set `allowedMethods` on upstreams that actually support the shim for all methods you list; unlisted methods should not be expected to succeed on standard EVM nodes. Source: `architecture/evm/eth_query.go:L87-L111`.

**Pin-to-upstream:** All sub-requests within a shim execution are pinned to the same upstream using `UseUpstream: pinToUpstreamId` directive, ensuring data consistency across calls.

---

### Config fields

All config fields are in `common/config.go`; defaults in `common/defaults.go`.

#### Network-level fields (`networks[].evm.*`)

| YAML path | Type | Default | Behavior | Source |
|---|---|---|---|---|
| `networks[].evm.integrity.enforceHighestBlock` | `*bool` | `true` | Controls whether `eth_blockNumber` and `eth_getBlockByNumber` (latest/finalized) hooks enforce the globally highest known block. Also read as `DirectiveDefaults.EnforceHighestBlock`. | `common/config.go:L2114`, `common/defaults.go:L1458-L1459`, `L2118-L2119` |
| `networks[].evm.integrity.enforceNonNullTaggedBlocks` | `*bool` | `true` | When true, null responses for tagged block queries (latest, finalized) cause `ErrEndpointMissingData`. Also read as `DirectiveDefaults.EnforceNonNullTaggedBlocks`. | `common/config.go:L2116`, `common/defaults.go:L1464-L1465`, `L2124-L2125` |
| `networks[].evm.integrity.enforceGetLogsBlockRange` | `*bool` | `true` | Enables upstream-level block range availability checking for `trace_filter`/`arbtrace_filter` (and `eth_getLogs`). | `common/config.go:L2115`, `common/defaults.go:L2121-L2122` |
| `networks[].evm.emptyResultConfidence` | `AvailbilityConfidence` | `blockHead` (enum = 1) | Confidence level for `emptyResultBeyondConfidence`: `blockHead` uses `EvmHighestLatestBlockNumber`; `finalizedBlock` uses `EvmHighestFinalizedBlockNumber`. Affects `markUnexpectedEmpty` and `enforceNonNullBlock`. | `common/config.go:L2250`, `common/defaults.go:L2075-L2078` |
| `networks[].evm.markEmptyAsErrorMethods` | `[]string` | default list (see below) | Methods for which null/empty upstream responses are converted to `ErrEndpointMissingData`. `nil` config uses the default list; `[]` (empty) disables the feature for all methods. | `common/config.go:L2218`, `common/defaults.go:L2111-L2113` |
| `networks[].evm.idempotentTransactionBroadcast` | `*bool` | `true` (nil = enabled) | Enables idempotency handling for `eth_sendRawTransaction`. Set to `false` to disable. | `common/config.go:L2239` |
| `networks[].evm.traceFilterSplitOnError` | `*bool` | `nil` (off) | Enables reactive splitting of `trace_filter`/`arbtrace_filter` on too-large errors. Intentionally off by default to preserve existing behavior. | `common/config.go:L2197`, `common/defaults.go:L2103-L2104` |
| `networks[].evm.traceFilterSplitConcurrency` | `int` | `10` | Max in-flight sub-requests during trace_filter splitting. | `common/config.go:L2200`, `common/defaults.go:L2105-L2106` |

**Default `markEmptyAsErrorMethods` list** (`common/defaults.go:L2044-L2064`):
- `eth_blockNumber`, `eth_getBlockByNumber`, `eth_getTransactionByHash`, `eth_getTransactionByBlockHashAndIndex`, `eth_getTransactionByBlockNumberAndIndex`, `eth_getUncleByBlockHashAndIndex`, `eth_getUncleByBlockNumberAndIndex`, `debug_traceTransaction`, `trace_transaction`, `trace_block`, `trace_get`
- **Excluded by design:** `eth_getBlockByHash` (subgraphs legitimately return empty), `eth_getTransactionReceipt` (null for pending txs is valid), `eth_getBlockReceipts` (empty array for 0-tx blocks is valid).

#### Per-method fields (`networks[].methods.definitions.<method>.*`)

These fields live in `CacheMethodConfig` (`common/config.go:L301`) which is keyed by method name under `networks[].methods.definitions`. They control how `NormalizeHttpJsonRpc` handles block-tag interpolation for that specific method.

| YAML path | Type | Default | Behavior | Source |
|---|---|---|---|---|
| `networks[].methods.definitions.<method>.translateLatestTag` | `*bool` | `nil` (= enabled) | When `nil` or `true`, `NormalizeHttpJsonRpc` replaces the `"latest"` tag in this method's block-parameter positions with the concrete hex block number from the network's `EvmHighestLatestBlockNumber` poller. Setting to `false` disables this interpolation for the method: `"latest"` is forwarded verbatim to upstreams, which means (a) the response will NOT be deduplicated against other requests for the same block (cache key won't contain a concrete block number), (b) upstreams that do not support `"latest"` as a block tag will receive it unchanged. Disable only when you intentionally want the upstream's real-time latest resolution rather than eRPC's highest-known-block tracking. | `common/config.go:L307-L309`, `architecture/evm/json_rpc.go:L130` |
| `networks[].methods.definitions.<method>.translateFinalizedTag` | `*bool` | `nil` (= enabled) | Same as `translateLatestTag` but for `"finalized"`. When `nil` or `true`, `"finalized"` is replaced with the concrete hex from `EvmHighestFinalizedBlockNumber`. When `false`, `"finalized"` passes through verbatim. Both flags are evaluated independently: you can disable `"latest"` translation while keeping `"finalized"` translation active, or vice versa. Note: even when translation is disabled, `seenLatest`/`seenFinalized` flags are still set so `EvmBlockRef` metadata (`"latest"`, `"finalized"`, or `"*"`) is still written to the request for routing/observability purposes. | `common/config.go:L310-L312`, `architecture/evm/json_rpc.go:L131,L204-L272` |

**Gate logic** (`architecture/evm/json_rpc.go:L130-L131`): `translateLatest := methodCfg.TranslateLatestTag == nil || *methodCfg.TranslateLatestTag`. Translation fires only when `!skipInterpolation && translateLatest` (for latest) or `!skipInterpolation && translateFinalized` (for finalized). The `skipInterpolation` flag comes from `nrq.Directives().SkipInterpolation` and overrides both method-level flags globally for that request.

**Consequence of disabling**: Disabling `translateLatestTag` causes `"latest"` in the block param to remain a string tag rather than a concrete hex. This means the cache key derivation for that request cannot include a stable block number — the `EvmBlockNumber` metadata on the request stays at the numeric value from prior params (or 0 if this is the only block param). Any cache policy using `realtime: true` or `finality` matching will see a different key from a concrete-block request for the same block. Source: `architecture/evm/json_rpc.go:L147-L153,L206-L238`.

#### Upstream-level fields (`upstreams[].evm.*`)

| YAML path | Type | Default | Behavior | Source |
|---|---|---|---|---|
| `upstreams[].evm.traceFilterAutoSplittingRangeThreshold` | `int64` | `0` (disabled) | Max block range per split chunk for proactive trace_filter splitting at this upstream. The effective threshold is `min` across all selected upstreams. Zero means this upstream doesn't constrain the range. | `common/config.go:L1134`, `common/defaults.go:L1546-L1547` |
| `upstreams[].evm.queryShim.enabled` | `*bool` | `nil` (disabled) | Enables the eth_query* shim for this upstream. | `common/config.go:L1157` |
| `upstreams[].evm.queryShim.allowedMethods` | `[]string` | `[]` (all allowed) | Wildcard-matched list of eth_query* methods this upstream handles. Empty = allow all five. | `common/config.go:L1159`, `architecture/evm/eth_query.go:L94-L111` |
| `upstreams[].evm.queryShim.concurrency` | `int` | `10` | Max in-flight sub-requests within a single shim execution. | `common/config.go:L1160`, `architecture/evm/eth_query_helpers.go:L21` |
| `upstreams[].evm.queryShim.maxBlockRange` | `int64` | `10000` | Max block range per query request. Requests exceeding this return `JsonRpcErrorCapacityExceeded`. | `common/config.go:L1161`, `architecture/evm/eth_query_helpers.go:L22` |
| `upstreams[].evm.queryShim.maxLimit` | `int` | `10000` | Max items per page. Requests exceeding this return `JsonRpcErrorCapacityExceeded`. | `common/config.go:L1162`, `architecture/evm/eth_query_helpers.go:L23` |
| `upstreams[].evm.queryShim.defaultLimit` | `int` | `100` | Limit used when the request omits `limit` or sets it to 0. | `common/config.go:L1163`, `architecture/evm/eth_query_helpers.go:L24` |

### Behaviors & algorithms

**NormalizeHttpJsonRpc flow** (`architecture/evm/json_rpc.go:L92-L321`):
1. Read-lock: extract `ReqRefs` param values into local slice.
2. Release read-lock; resolve tags to hex outside lock.
3. Accumulate only the changes that differ from the original value.
4. If any change needed and `!skipInterpolation`: deep-copy params, apply changes, write-lock to commit.
5. Set `EvmBlockRef` from observed tags (latest/finalized/both → `"*"`).
6. Tags `"safe"`, `"pending"`, `"earliest"`, and block hashes are passed through unchanged.

**emptyResultBeyondConfidence algorithm** (`architecture/evm/common.go:L62-L89`):
- Requires `rq.EvmBlockNumber()` to be a positive `int64`; tags and hash lookups return 0 → never "beyond".
- Reads `EmptyResultConfidence` from `nq.Network().Config().Evm.EmptyResultConfidence`.
- Compares `bn > head`; head=0 (unknown) → fail open (returns false).

**trace_filter split bisection algorithm** (`architecture/evm/trace_filter.go:L390-L479`):
1. Extract `fromBlock`/`toBlock` (must be hex).
2. `blockRange > 1`: split at midpoint `fb + (range/2)`, always 2 sub-requests.
3. `blockRange == 1` and `fromAddress` list has >1 entries: split at `len/2`.
4. `blockRange == 1` and `toAddress` list has >1 entries: split at `len/2`.
5. Else: return error "request cannot be split further".

**eth_sendRawTransaction idempotency guards** (test coverage: `architecture/evm/eth_sendRawTransaction_test.go`):
- Parent context already cancelled: probe still runs via `context.WithoutCancel`.
- Hash mismatch in verification response: returns original error (refuses synthetic success).
- Missing `hash` field in verification response: returns original error.
- Probe itself errors: returns original error.
- `FailsafeTimeoutExceeded` triggers the same verification path as `UpstreamsExhausted`.
- `idempotentTransactionBroadcast=false`: skips verification at both levels.

**eth_getBlockByNumber block enforcement behavior** (`future_block_empty_test.go`):
- Block at exactly `latest` head: not considered "beyond confidence" (retryable if empty).
- Block above `latest` head by 1: considered beyond confidence → returns truthful empty.
- Unknown head (0): fail open → any block is treated as retryable.
- With `EmptyResultConfidence=finalizedBlock`: blocks between finalized and latest are "beyond confidence".

**markUnexpectedEmpty with retryEmpty directive** (`architecture/evm/hooks_test.go`):
- Methods in `MarkEmptyAsErrorMethods` + `RetryEmpty=true`: null/empty → `ErrEndpointMissingData`.
- Methods in `MarkEmptyAsErrorMethods` + `RetryEmpty=false`: null/empty passes through unchanged.
- Methods NOT in `MarkEmptyAsErrorMethods`: null/empty never converted, regardless of `RetryEmpty`.
- `MarkEmptyAsErrorMethods=[]` (empty): feature disabled entirely.
- Custom methods can be added; default list is not merged with custom config.

### Observability

| Metric | Type | Labels | Fires when | Source |
|---|---|---|---|---|
| `erpc_upstream_stale_latest_block_total` | Counter | `project`, `vendor`, `network`, `upstream`, `category` | Upstream returned an older block than the network's known highest; `category` = `"eth_blockNumber"` or `"eth_getBlockByNumber"` | `telemetry/metrics.go:L306`, `architecture/evm/eth_blockNumber.go:L76`, `architecture/evm/eth_getBlockByNumber.go:L173` |
| `erpc_upstream_stale_finalized_block_total` | Counter | `project`, `vendor`, `network`, `upstream` | Same for finalized block in `eth_getBlockByNumber` | `telemetry/metrics.go:L312`, `architecture/evm/eth_getBlockByNumber.go:L232` |
| `erpc_network_latest_block_timestamp_distance_seconds` | Gauge | `project`, `network`, `origin` | `eth_getBlockByNumber(latest)` response received with `origin="network_response"` | `telemetry/metrics.go:L109`, `architecture/evm/eth_getBlockByNumber.go:L96` |
| `erpc_network_evm_trace_filter_range_requested` | Histogram | `project`, `network`, `method`, `user`, `finality` | Any `trace_filter`/`arbtrace_filter` request with resolvable block bounds | `telemetry/metrics.go:L806`, `architecture/evm/trace_filter.go:L99` |
| `erpc_network_evm_trace_filter_forced_splits_total` | Counter | `project`, `network`, `method`, `dimension`, `user`, `agent_name` | Each split by dimension (`block_range`, `from_address`, `to_address`) | `telemetry/metrics.go:L360`, `architecture/evm/trace_filter.go:L424,L444,L463` |
| `erpc_network_evm_trace_filter_split_success_total` | Counter | `project`, `network`, `method`, `user`, `agent_name` | Each successful sub-request in a split execution | `telemetry/metrics.go:L348`, `architecture/evm/trace_filter.go:L573` |
| `erpc_network_evm_trace_filter_split_failure_total` | Counter | `project`, `network`, `method`, `user`, `agent_name` | Each failed sub-request in a split execution | `telemetry/metrics.go:L354`, `architecture/evm/trace_filter.go:L502` |

**Trace span names:**
- `Project.PreForwardHook` — parent span for all project-level hooks
- `Project.PreForwardHook.eth_chainId`, `Project.PreForwardHook.eth_blockNumber`
- `Network.PreForwardHook`, `Network.PreForwardHook.eth_chainId`
- `Network.PostForward.eth_getBlockByNumber`, `Network.PostForward.eth_sendRawTransaction`
- `Upstream.PreForwardHook`, `Upstream.PreForwardHook.eth_chainId`, `Upstream.PreForwardHook.trace_filter`
- `Upstream.PostForwardHook.eth_getBlockByNumber`, `Upstream.PostForwardHook.eth_getBlockReceipts`, `Upstream.PostForwardHook.eth_sendRawTransaction`
- `Evm.PickHighestBlock`
- `extractTxHashFromSendRawTransaction`, `createSyntheticSuccessResponse`, `verifyAndHandleNonceTooLow`

**Notable log messages:**
- `"interpolated block tag to concrete block number"` (debug) — `architecture/evm/json_rpc.go:L226`, fires when `"latest"` or `"finalized"` is resolved to a concrete hex.
- `"passed through block tag"` (trace) — when `"safe"`, `"pending"`, etc. are left unchanged.
- `"upstream returned older block than we known, falling back to highest known block"` (debug) — `eth_blockNumber.go:L87` and `eth_getBlockByNumber.go:L164`.
- `"skipping enforcement of highest block number as response is from cache"` (trace) — `eth_blockNumber.go:L44`, `eth_getBlockByNumber.go:L129`.
- `"enforcing highest latest block"` / `"enforcing highest finalized block"` (debug) — `eth_getBlockByNumber.go:L165, L221`.
- `"converting 'already known' error to idempotent success"` (info) — `eth_sendRawTransaction.go:L117`.
- `"tx FOUND on-chain - converting 'nonce too low' to idempotent success"` (info) — `eth_sendRawTransaction.go:L253`.
- `"exhausted error overridden: tx found in network, returning synthetic success"` (info) — `eth_sendRawTransaction.go:L391`.
- `"verification response carries a different tx hash than submitted — refusing synthetic success"` (warn) — `eth_sendRawTransaction.go:L383`.
- `"eth_queryTraces requires trace_block or debug_traceBlockByNumber support"` (via `ErrEndpointUnsupported`) — `eth_query_shim.go:L481-L483`.

### Edge cases & gotchas

1. **eth_call missing block param**: If the client omits the second parameter entirely (only sends the call object), the project pre-forward hook automatically appends `"latest"` before forwarding. Without this, many upstreams would reject the request. Source: `architecture/evm/eth_call.go:L15-L28`.

2. **eth_chainId short-circuit and skip-cache-read**: All three chainId hook levels check `ShouldSkipCacheRead("")`. A request with `x-erpc-skip-cache-read: true` header bypasses all synthetic responses and reaches real upstreams. Without this, force-refresh requests would always get the static config value. Source: `architecture/evm/eth_chainId.go:L26,L75,L118`.

3. **eth_blockNumber from-cache bypass**: When the underlying forward returns a cached response, the `eth_blockNumber` hook returns it as-is without comparing against the known highest block. The rationale: cached values are already stale by design; the TTL is the correctness control. Comparing against live poller data would defeat caching. Source: `architecture/evm/eth_blockNumber.go:L40-L48`.

4. **enforceHighestBlock excludes current upstream**: When `eth_getBlockByNumber` detects a stale upstream response (block number was actually extracted from the response, not 0), the re-forward directive excludes that upstream via `UseUpstream: "!upstreamId"`. When the extracted block number is 0 (upstream returned a JSON-RPC error), the current upstream is NOT excluded (may be an intermittent issue). Source: `architecture/evm/eth_getBlockByNumber.go:L196-L202`.

5. **pickHighestBlock safety for corrupt block numbers**: After fetching the highest block, `pickHighestBlock` is called instead of directly returning the new response. This handles the case where the known highest block is corrupted (e.g., stale poller state claiming a very high non-existent block) — the old response is compared and the higher of the two valid responses wins. Source: `architecture/evm/eth_getBlockByNumber.go:L211,L270`.

6. **trace_filter null-to-empty normalization**: The upstream post-forward hook converts `null`, `""`, and `{}` to `[]`. This is required because some nodes (notably older Parity/OpenEthereum nodes) return `null` when no traces match a filter. JSON decoders expecting `[]` would fail on `null`. Source: `architecture/evm/trace_filter.go:L361-L374`, test: `architecture/evm/trace_filter_normalize_test.go:L18-L53`.

7. **trace_filter split re-entrancy prevention**: Both proactive and reactive splitting check `rq.ParentRequestId() != nil || rq.IsCompositeRequest()` and skip. Sub-requests spawned by the split executor set `ParentRequestId`, so they cannot trigger additional splits. Source: `architecture/evm/trace_filter.go:L122-L125,L232-L234`.

8. **trace_filter proactive split uses min threshold across upstreams**: The effective threshold is `min(positive thresholds)`. Upstreams with `TraceFilterAutoSplittingRangeThreshold=0` are ignored; if ALL upstreams are zero, no proactive split occurs. Source: `architecture/evm/trace_filter.go:L175-L187`, test: `architecture/evm/trace_filter_test.go:L502-L541`.

9. **eth_sendRawTransaction synthetic success requires hash cross-check**: The network-level hook verifies that the tx returned by `eth_getTransactionByHash` has a `hash` field matching the locally-computed hash. A missing `hash` field or a different hash causes the original error to propagate. This prevents a byzantine upstream that returns any non-null tx object from causing a false "success". Source: `architecture/evm/eth_sendRawTransaction.go:L375-L385`, test: `architecture/evm/eth_sendRawTransaction_test.go:L237-L299`.

10. **eth_sendRawTransaction independent probe context**: The network-level verification probe uses `context.WithoutCancel(ctx)` combined with a fixed `networkPostForwardVerifyTimeout` (3 seconds). The parent context is always expired by the time this hook runs (the failsafe loop has already consumed the budget). Without `WithoutCancel`, the probe would silently no-op and the feature would never activate in production. Source: `architecture/evm/eth_sendRawTransaction.go:L24-L26,L346-L347`, test: `architecture/evm/eth_sendRawTransaction_test.go:L328-L371`.

11. **eth_getBlockReceipts excluded from markEmptyAsErrorMethods by default**: Empty arrays are the correct response for blocks with zero transactions. Including it in the default list causes retry storms with `emptyResultDelay` that can outrun the outer network timeout. The method IS still covered by `enforceNonNullBlock` at the network level when response is fully null (not empty array). Source: `common/defaults.go:L2054-L2064`.

12. **Polygon phantom transactions**: Blocks with phantom system transactions (from=0x0, gas=0x0) legitimately have `transactionsRoot = emptyTrieRoot` even though the `transactions` array is non-empty. The `validateTransactionsRoot` check uses `allPhantomTransactions` to skip the inconsistency error in this case. Source: `architecture/evm/eth_getBlockByNumber.go:L526-L532,L579-L598`.

13. **eth_queryTraces trace_block → debug fallback**: If `trace_block` is unsupported, the shim tries `debug_traceBlockByNumber` with `callTracer`. If that also fails with "unsupported", the shim returns `ErrEndpointUnsupported` (causing this upstream to be skipped). Both Parity-style and Geth-style trace formats are normalized to the canonical proto trace format. Source: `architecture/evm/eth_query_shim.go:L448-L485`.

14. **eth_queryLogs reads via eth_getLogs, not block iteration**: Unlike `eth_queryBlocks` and `eth_queryTransactions` which iterate blocks, `eth_queryLogs` issues a single `eth_getLogs` call then paginates in-memory. This is more efficient for sparse log queries but may fail on nodes that limit `eth_getLogs` range. Source: `architecture/evm/eth_query_shim.go:L121-L249`.

15. **eth_queryTransfers delegates entirely to eth_queryTraces**: Native transfers are computed from call-type traces using `bdsevm.NativeTransfersFromTraces`. The intermediate `QueryRequest` sets `Fields.Traces=true` to ensure trace data is fetched. Source: `architecture/evm/eth_query_shim.go:L337-L398`.

16. **NormalizeHttpJsonRpc top-level map param avoidance**: When a `ReqRefs` path is a single integer index pointing to a map (object param), the function skips replacing that param even if the value is a numeric block — this prevents accidentally replacing a complex call-data object with a hex string. Only leaf-level block references within nested objects are replaced. Source: `architecture/evm/json_rpc.go:L168-L176,L188-L194`.

17. **Deprecated `evm.integrity.*` fields migrate to `DirectiveDefaults`**: `EnforceHighestBlock`, `EnforceGetLogsBlockRange`, and `EnforceNonNullTaggedBlocks` under `networks[].evm.integrity` are deprecated. Their values are propagated to `networks[].directiveDefaults` during `SetDefaults`. The directive-based checks are the new canonical path. Source: `common/defaults.go:L1957-L1964`, `common/config.go:L2310-L2315`.

18. **validateBlock requires dirs to be non-nil**: The upstream post-forward hook for `eth_getBlockByNumber` skips all validation when `rq.Directives()` returns `nil`. Validation only runs when directives are explicitly set (typically via library-mode usage or configured `DirectiveDefaults`). Source: `architecture/evm/eth_getBlockByNumber.go:L449-L454`.

19. **200-OK revert detection via ABI selector prefix (`0x08c379a0`)**: Some nodes return a 200 HTTP response with a JSON-RPC result field whose value starts with `0x08c379a0` — the ABI function selector for `Error(string)`. The normalizer rule (`architecture/evm/error_normalizer.go:L609-L624`) detects this and converts the response to `ErrEndpointExecutionException` with `JsonRpcErrorEvmReverted`. The check is `dt[1:11] == "0x08c379a0"` rather than `dt[0:10]` because `dt[0]` is the JSON quote character (`"`). Minimum length check is `len(dt) > 11`. This applies to all JSON-RPC methods, not just `eth_call`; any method that can trigger EVM execution on the node may return this in simulation. Source: `architecture/evm/error_normalizer.go:L609-L624`.

20. **`ErrEndpointClientSideException.ErrorStatusCode()` returns HTTP 200 for revert-class causes**: When `ErrEndpointClientSideException` wraps an `ErrJsonRpcExceptionInternal` whose `NormalizedCode()` is `JsonRpcErrorEvmReverted` (3), `JsonRpcErrorCallException` (-32000), or `JsonRpcErrorTransactionRejected` (-32003), `ErrorStatusCode()` returns `200` instead of `400`. These three codes represent EVM execution outcomes (contract revert, call exception, transaction rejection) that the client's request successfully triggered — the EVM responded correctly even though execution failed. Returning 400 would suggest a malformed request, which is semantically wrong. Callers that handle HTTP status codes to detect "client error" must check the JSON-RPC error code rather than relying solely on HTTP status to distinguish these execution failures from actual bad requests. Source: `common/errors.go:L1894-L1905`. Error code constants: `JsonRpcErrorEvmReverted=3`, `JsonRpcErrorCallException=-32000`, `JsonRpcErrorTransactionRejected=-32003` (`common/errors.go:L2157-L2166`).

21. **trace/debug timeout detection via raw-byte scan (no JSON parsing, handles ~50 MB responses)**: For methods beginning with `trace_`, `debug_`, or `eth_trace`, the normalizer includes a fast path for timeout detection in 200-OK responses (`architecture/evm/error_normalizer.go:L626-L652`). Because these responses can be very large (easily 50 MB for block-level traces), parsing the JSON to extract an error field would be prohibitively expensive. Instead, the normalizer does a `strings.Contains(dt, "execution timeout")` raw byte scan on the result string. A match produces `ErrEndpointServerSideException` with `JsonRpcErrorNodeTimeout` (`-32015`) and `retryable=true`, so retry/failover mechanisms will try the same and/or other upstreams. This scan only runs when `jr.ResultLength() > 0` (i.e., the response has a `result` field, not an `error` field). Source: `architecture/evm/error_normalizer.go:L626-L652`.

22. **queryShim allowedMethods fall-through footgun**: If `allowedMethods` is set to an explicit non-empty list and a query method is NOT in that list, `isQueryShimMethodAllowed` returns `false` and the upstream pre-forward hook returns `handled=false` without an error. The request is then forwarded to the upstream as a raw `eth_queryBlocks` / `eth_queryTransactions` / etc. JSON-RPC call that the upstream almost certainly does not support, resulting in a "method not found" error. Methods not in the list are NOT rejected with a clear error by eRPC; they silently fall through. When configuring `allowedMethods`, ensure every `eth_query*` method you want to support is included, or leave the list empty to allow all five. Source: `architecture/evm/eth_query.go:L87-L111`.

### Source map

- `architecture/evm/hooks.go` — hook dispatch table; four hook entry points routing by method name
- `architecture/evm/json_rpc.go` — `NormalizeHttpJsonRpc`; block tag resolution, param deep-copy, path rewriting
- `architecture/evm/common.go` — `emptyResultBeyondConfidence`; `upstreamPostForward_markUnexpectedEmpty`; `normalizeEmptyArrayResponse`
- `architecture/evm/extractor.go` — `JsonRpcErrorExtractor` adapter; delegates to `ExtractJsonRpcError`
- `architecture/evm/eth_call.go` — project pre-forward: missing block param injection
- `architecture/evm/eth_chainId.go` — three-layer synthetic chainId short-circuit; `BuildEthChainIdRequest`
- `architecture/evm/eth_blockNumber.go` — project pre-forward: highest-known-block enforcement
- `architecture/evm/eth_getBlockByNumber.go` — network post-forward: `enforceHighestBlock`, `enforceNonNullBlock`, `pickHighestBlock`; upstream post-forward: block header/transaction validation; `BuildGetBlockByNumberRequest`
- `architecture/evm/eth_getBlockReceipts.go` — upstream post-forward: receipt count/hash/bloom/log validation
- `architecture/evm/eth_sendRawTransaction.go` — upstream/network idempotency; `extractTxHashFromSendRawTransaction`; `createSyntheticSuccessResponse`; `verifyAndHandleNonceTooLow`
- `architecture/evm/trace_filter.go` — `TraceFilterMethods`; four hooks for trace_filter/arbtrace_filter; proactive/reactive splitting; `splitTraceFilterRequest`; `executeTraceFilterSubRequests`; `BuildTraceFilterRequest`
- `architecture/evm/eth_query.go` — `upstreamPreForward_eth_query`; `executeQueryShim`; request/response parsing and dispatch for all five query methods
- `architecture/evm/eth_query_helpers.go` — constants (`defaultQueryShimConcurrency=10`, `maxBlockRange=10000`, `maxLimit=10000`, `defaultLimit=100`); `fetchBlockRange`; `forwardSubRequest`; filter-matching functions; field projection; sorting; cursor management
- `architecture/evm/eth_query_shim.go` — per-method shim implementations (`shimQueryBlocks`, `shimQueryTransactions`, `shimQueryLogs`, `shimQueryTraces`, `shimQueryTransfers`); `fetchTracesForBlock` with trace_block→debug fallback; trace format normalization helpers
- `architecture/evm/error_normalizer.go` — `ExtractJsonRpcError`; all normalizer rules including revert-detection (L246-L272), nonce rules 11/12 (L297-L341), insufficient-funds rule (L350-L361), out-of-gas/-32003 N:yes flip (L368-L391), 200-OK ABI-selector revert detection (L609-L624), trace/debug raw-scan timeout detection (L626-L652)
- `common/errors.go` — `ErrEndpointNonceException` (L2763), `NonceExceptionReasonAlreadyKnown` (L2754), `NonceExceptionReasonNonceTooLow` (L2756), `retryableTowardNetwork=false` flag (L2773)
- `architecture/evm/hooks_test.go` — `markEmptyAsErrorMethods` behavior tests (listed methods, retryEmpty=false, non-listed methods, custom methods, empty config)
- `architecture/evm/future_block_empty_test.go` — `emptyResultBeyondConfidence` edge cases; `enforceNonNullBlock` future-block handling
- `architecture/evm/eth_sendRawTransaction_test.go` — idempotency safeguard tests (exhausted+tx found, exhausted+tx absent, non-exhausted passthrough, hash mismatch, missing hash, cancelled parent ctx, FailsafeTimeoutExceeded)
- `architecture/evm/trace_filter_test.go` — block range availability checks; split bisection; concurrent execution; proactive split with min-threshold; reactive split on error
- `architecture/evm/trace_filter_normalize_test.go` — null/empty/object → `[]` normalization; non-empty passthrough; error passthrough
- `common/config.go` — `EvmNetworkConfig`, `EvmUpstreamConfig`, `EvmQueryShimConfig`, `EvmIntegrityConfig` struct definitions
- `common/defaults.go` — `DefaultMarkEmptyAsErrorMethods`; `EvmNetworkConfig.SetDefaults`; `EvmIntegrityConfig.SetDefaults`; directive default migration
- `telemetry/metrics.go` — `MetricUpstreamStaleLatestBlock`, `MetricUpstreamStaleFinalizedBlock`, `MetricNetworkLatestBlockTimestampDistance`, `MetricNetworkEvmTraceFilterRangeRequested`, `MetricNetworkEvmTraceFilterForcedSplits`, `MetricNetworkEvmTraceFilterSplitSuccess`, `MetricNetworkEvmTraceFilterSplitFailure`
