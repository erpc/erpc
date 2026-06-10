# KB: Data integrity policy

> status: complete
> source-dirs: architecture/evm/eth_blockNumber.go, architecture/evm/eth_getBlockByNumber.go, architecture/evm/eth_getLogs.go, architecture/evm/eth_getBlockReceipts.go, architecture/evm/trace_filter.go, architecture/evm/block_range.go, architecture/evm/common.go, common/config.go (EvmIntegrityConfig, DirectiveDefaultsConfig, UpstreamIntegrityConfig), common/defaults.go, common/request.go (RequestDirectives), erpc/networks_integrity_test.go, architecture/evm/eth_getLogs_test.go, architecture/evm/eth_getBlockByNumber_test.go, architecture/evm/future_block_empty_test.go, architecture/evm/trace_filter_test.go, telemetry/metrics.go

## L1 — Capability (CTO view)

eRPC enforces a layered set of EVM response-integrity rules that prevent stale, malformed, or logically inconsistent upstream responses from reaching callers. Three core network-level rules are on by default (highest-block enforcement, getLogs block-range availability, and non-null tagged blocks), and a rich set of directive-gated checks covers block header field lengths, transaction consistency, receipt/log structure, Bloom filter correctness, and cross-entity ground-truth validation. Integrity violations are treated as retryable upstream errors: eRPC silently discards the bad response and retries against a different upstream, so a caller sees the correct answer rather than garbage data.

## L2 — Mechanics (staff-engineer view)

**Two configuration planes.** Config migrated from the older `networks[].evm.integrity` (type `EvmIntegrityConfig`) to `networks[].directiveDefaults` (type `DirectiveDefaultsConfig`). During `SetDefaults`, if `evm.Integrity` is populated and `directiveDefaults` is nil for the same field, the old value is promoted (`common/defaults.go:L1952-L1964`). Both the old and new paths reach the same `RequestDirectives` struct fields at runtime. The `EvmIntegrityConfig` type is explicitly marked `@deprecated` in source (`common/config.go:L2308`).

**Request directives as the runtime gate.** All integrity checks are controlled by fields on `RequestDirectives` (`common/request.go:L116-L215`). These are populated from three sources in priority order (highest last wins): (1) defaults computed from `networks[].directiveDefaults.SetDefaults` during `NormalizeWithContext`; (2) `X-ERPC-*` HTTP request headers; (3) `?<directive>=` query parameters. Library users can also set directives programmatically on `NormalizedRequest.SetDirectives`. Ground-truth fields (`GroundTruthTransactions`, `GroundTruthLogs`) are only settable programmatically — they cannot be passed via HTTP.

**Hook architecture.** Integrity checks live in architecture-specific pre/post-forward hooks. Network-level hooks run before or after the entire failsafe execution; upstream-level hooks run per attempt (before or after a single upstream call). The hooks are registered in an `EvmHookRegistry` (see `architecture/evm/` package init). Relevant hooks:
- `projectPreForward_eth_blockNumber` — network pre-forward; enforces highest block for `eth_blockNumber` (`architecture/evm/eth_blockNumber.go`).
- `networkPostForward_eth_getBlockByNumber` — network post-forward; calls `enforceHighestBlock` then `enforceNonNullBlock` (`architecture/evm/eth_getBlockByNumber.go:L43-L109`).
- `upstreamPreForward_eth_getLogs` — upstream pre-forward; validates block range availability before sending to that upstream (`architecture/evm/eth_getLogs.go:L273-L347`).
- `upstreamPostForward_eth_getLogs` — upstream post-forward; normalizes empty arrays (`architecture/evm/eth_getLogs.go:L349-L362`).
- `networkPostForward_eth_getLogs` — network post-forward; reactive splitting on 413-like errors (`architecture/evm/eth_getLogs.go:L364+`).
- `upstreamPreForward_trace_filter` — upstream pre-forward; same block-range availability check as getLogs, reusing `EnforceGetLogsBlockRange` (`architecture/evm/trace_filter.go:L280-L356`).
- `upstreamPostForward_eth_getBlockByNumber` — upstream post-forward; validates block structure fields (`architecture/evm/eth_getBlockByNumber.go:L423-L457`).
- `upstreamPostForward_eth_getBlockReceipts` — upstream post-forward; validates receipts/logs structure (`architecture/evm/eth_getBlockReceipts.go:L17-L53`).

**Enforcement order for `eth_getBlockByNumber` with `latest`/`finalized`.** The network-level `enforceHighestBlock` runs as a post-forward hook. If the returned block number is less than the network-known highest block (tracked by `EvmHighestLatestBlockNumber` / `EvmHighestFinalizedBlockNumber`), eRPC constructs a new request for the highest-known block, sets `SkipCacheRead=true`, excludes the slow upstream (via `UseUpstream=!<id>`), and forwards again. If the re-request returns a lower block, `pickHighestBlock` chooses the response with the higher block number to guard against a corrupted highest-block state (`architecture/evm/eth_getBlockByNumber.go:L160-L276`). Cache responses skip enforcement entirely to avoid polluting cache TTL logic.

**Block-range availability check.** For `eth_getLogs`, `trace_filter`, and `arbtrace_filter`, before forwarding to a specific upstream, the pre-forward hook calls `CheckBlockRangeAvailability`. This calls `EvmAssertBlockAvailability` twice: `toBlock` first with `forceFreshIfStale=true` (near the chain head), `fromBlock` second with `forceFreshIfStale=false` (historical). If either block is outside the upstream's available range, a retryable `ErrUpstreamBlockUnavailable` is returned so the network-level retry will route to a different upstream (`architecture/evm/block_range.go`). Block tags like `latest` are resolved to hex numbers via the network's state poller before the check; unresolvable tags (e.g. `pending`, `safe`, `earliest`) skip the check entirely (`architecture/evm/eth_getLogs.go:L26-L50`).

**Future-block empty-result guard.** Both `enforceNonNullBlock` (network post-forward) and `upstreamPostForward_markUnexpectedEmpty` (upstream post-forward, for point-lookups generally) call `emptyResultBeyondConfidence`. This function checks whether the concrete block number in the request is beyond the network's confidence head (latest or finalized, depending on `emptyResultConfidence`). If it is beyond confidence, the empty result is returned truthfully rather than being treated as retryable missing data, preventing infinite retry storms on not-yet-produced blocks (`architecture/evm/common.go:L55-L89`).

**Validation error semantics.** `ErrEndpointContentValidation` (code `ErrCodeEndpointContentValidation`) is used for structural validation failures. It is treated as retryable at network scope (try another upstream) but NOT retryable at the same upstream (`common/errors.go:L2719-L2747`). It feeds the `validation` bucket in `ErrUpstreamsExhausted` exhaustion accounting (`common/errors.go:L1030`). **Wire vs method HTTP status discrepancy**: `ErrorStatusCode()` returns 502 (`common/errors.go:L2745`), but this method is dead code — the wire HTTP status is always 200 with a JSON-RPC error body (code -32603), determined by `handleErrorResponse` in `erpc/http_server.go:L1473-L1491` which does not include `ErrCodeEndpointContentValidation` in any non-200 arm.

## L3 — Exhaustive reference (agent view)

### Config fields

#### Network-level: `networks[].directiveDefaults` (type `DirectiveDefaultsConfig`)

These fields set the default value for the corresponding `RequestDirectives` field for every request on this network. Per-request headers/query-params can override. All boolean fields default to `false` unless noted.

| # | YAML path | Type | Default | Behavior | Source |
|---|-----------|------|---------|----------|--------|
| 1 | `networks[].directiveDefaults.enforceHighestBlock` | `*bool` | `true` (set by `DirectiveDefaultsConfig.SetDefaults`) | When `true`, if `eth_blockNumber` or `eth_getBlockByNumber[latest/finalized]` returns a block lower than the network-known highest, re-fetches against the highest-known block from a different upstream. | `common/defaults.go:L1458-L1460` |
| 2 | `networks[].directiveDefaults.enforceGetLogsBlockRange` | `*bool` | `true` | Before forwarding `eth_getLogs`, `trace_filter`, or `arbtrace_filter` to a specific upstream, checks that both `fromBlock` and `toBlock` are within that upstream's available block range; returns `ErrUpstreamBlockUnavailable` (retryable) if not. | `common/defaults.go:L1461-L1463` |
| 3 | `networks[].directiveDefaults.enforceNonNullTaggedBlocks` | `*bool` | `true` | When `eth_getBlockByNumber` with a tag (`latest`, `finalized`, etc.) returns null/empty, converts to `ErrEndpointMissingData` (triggers retry). Has no effect for numeric blocks (those always error on null) or blocks beyond confidence. | `common/defaults.go:L1464-L1466` |
| 4 | `networks[].directiveDefaults.validateTransactionsRoot` | `*bool` | `true` | Checks that `transactionsRoot` is consistent with the transaction count: non-empty root + 0 txs → error; empty root + non-phantom txs → error. Disable for non-standard chains (e.g. ZKSync Era). | `common/defaults.go:L1467-L1469` |
| 5 | `networks[].directiveDefaults.validateHeaderFieldLengths` | `*bool` | `false` (zero value; `DirectiveDefaultsConfig.SetDefaults` does NOT set this field — only the four fields listed above get defaults) | When `true`, validates the hex-decoded byte-lengths of six block header fields in `eth_getBlockByNumber` responses: `hash`, `parentHash`, `stateRoot`, `transactionsRoot`, `receiptsRoot` must each decode to exactly 32 bytes; `logsBloom` must decode to exactly 256 bytes. **Absent fields are skipped** — if a field is empty string (`""`), the check for that field is bypassed entirely (no error). This means the validation is additive/opt-in per field, not exhaustive. An invalid hex string in a present field, or a present field with wrong byte count, triggers `ErrEndpointContentValidation`. | `common/config.go:L2123`, `architecture/evm/eth_getBlockByNumber.go:L615-L682` |
| 6 | `networks[].directiveDefaults.validateTransactionFields` | `*bool` | `false` | **Applies to `eth_getBlockByNumber` with `hydrated=true` (full tx objects) only — silently skips hash-only responses.** For each full transaction object: (1) tx hash must decode to exactly 32 bytes (invalid hex → error; wrong length → error); (2) no duplicate tx hashes within the block (case-insensitive compare). Emits `ErrEndpointContentValidation` on any failure. Does NOT fire if the `transactions` array contains only hash strings. | `common/config.go:L2126`, `architecture/evm/eth_getBlockByNumber.go:L686-L708` |
| 7 | `networks[].directiveDefaults.validateTransactionBlockInfo` | `*bool` | `false` | **Applies to `eth_getBlockByNumber` with `hydrated=true` only.** For each full tx object: (1) `tx.blockHash` must equal the block-level `hash` (if both present, case-insensitive, no 0x stripping); (2) `tx.blockNumber` must equal the block-level `number` (parsed via `HexToInt64`); (3) `tx.transactionIndex` must equal the array position `i` (0-based). The block-level values are sourced from the same response body. Emits `ErrEndpointContentValidation` with a descriptive message per mismatch type. | `common/config.go:L2127`, `architecture/evm/eth_getBlockByNumber.go:L710-L753` |
| 8 | `networks[].directiveDefaults.enforceLogIndexStrictIncrements` | `*bool` | `false` | **Applies to `eth_getBlockReceipts` only.** Maintains a single global counter `expectedLogIndex` starting at 0, iterating all logs across ALL receipts in array order. For each log: (1) missing `logIndex` field → error ("missing logIndex"); (2) non-UTF-8 string → error; (3) cannot parse hex → error; (4) value ≠ `expectedLogIndex` → error ("logIndex not contiguous: expected N got M at receipt R log L"). Counter increments by 1 after each log. Emits `ErrEndpointContentValidation`. The upstream-level `checkLogIndexStrictIncrements` field in `UpstreamIntegrityConfig` is a reserved/future placeholder — it is not read by any validation code; this directive is the only active control. | `common/config.go:L2130`, `architecture/evm/eth_getBlockReceipts.go:L399-L416` |
| 9 | `networks[].directiveDefaults.validateTxHashUniqueness` | `*bool` | `false` | Stricter than always-on duplicate check: also rejects empty `transactionHash` fields in receipts. | `common/config.go:L2131` |
| 10 | `networks[].directiveDefaults.validateTransactionIndex` | `*bool` | `false` | For `eth_getBlockReceipts`, validates `transactionIndex` in each receipt equals its array position. | `common/config.go:L2132` |
| 11 | `networks[].directiveDefaults.validateLogFields` | `*bool` | `false` | For `eth_getBlockReceipts`, validates log address length (20 bytes), topic count (≤4), topic lengths (32 bytes), and log/receipt cross-field consistency (block hash/number/tx hash/tx index). | `common/config.go:L2133` |
| 12 | `networks[].directiveDefaults.validateLogsBloomEmptiness` | `*bool` | `false` | For `eth_getBlockReceipts`, checks consistency between `logsBloom` and `logs`: non-zero bloom → logs must exist; logs present → bloom must not be zero. | `common/config.go:L2137` |
| 13 | `networks[].directiveDefaults.validateLogsBloomMatch` | `*bool` | `false` | For `eth_getBlockReceipts`, re-derives bloom from log addresses/topics using keccak256 and compares to the provided bloom. Full bloom recompute. | `common/config.go:L2139` |
| 14 | `networks[].directiveDefaults.validateReceiptTransactionMatch` | `*bool` | `false` | Library-mode only. Cross-validates `receipt[i].transactionHash == GroundTruthTransactions[i].hash`. Requires `GroundTruthTransactions` to be set programmatically. | `common/config.go:L2142` |
| 15 | `networks[].directiveDefaults.validateContractCreation` | `*bool` | `false` | Library-mode only. Cross-validates contract creation consistency with `GroundTruthTransactions`. | `common/config.go:L2143` |
| 16 | `networks[].directiveDefaults.receiptsCountExact` | `*int64` | `nil` (unchecked) | Exact receipt count assertion. `nil` means no check. | `common/config.go:L2146` |
| 17 | `networks[].directiveDefaults.receiptsCountAtLeast` | `*int64` | `nil` (unchecked) | Minimum receipt count assertion. | `common/config.go:L2147` |
| 18 | `networks[].directiveDefaults.validationExpectedBlockHash` | `*string` | `nil` | **Applies to `eth_getBlockReceipts` only.** When set (non-empty string), the validation compares `strings.ToLower(strings.TrimPrefix(hash, "0x"))` of every `receipt.blockHash` against the expected value. Receipts with empty `blockHash` are skipped (no error). A mismatch emits `ErrEndpointContentValidation` ("receipts block hash mismatch at index N: expected X got Y"). Use case: library-mode / programmatic calls where the caller knows the exact block being fetched and wants to guard against hash collisions or mis-routed responses. Can be set via `X-ERPC-Validation-Block-Hash` header or `validation-block-hash` query param (string value; `0x` prefix optional). Interacts with `directiveDefaults` if set statically; per-request header/param overrides the default. | `common/config.go:L2150`, `architecture/evm/eth_getBlockReceipts.go:L168-L179` |
| 19 | `networks[].directiveDefaults.validationExpectedBlockNumber` | `*int64` | `nil` | **Applies to `eth_getBlockReceipts` only.** When set (non-nil int64 pointer), validates that every `receipt.blockNumber` (parsed via `HexToInt64`) equals the expected value. Receipts with empty `blockNumber` are skipped. A mismatch emits `ErrEndpointContentValidation` ("receipts block number mismatch at index N: expected X got Y"). Use case: same as `validationExpectedBlockHash` — library-mode guard against mis-routed responses. Can be set via `X-ERPC-Validation-Block-Number` header or `validation-block-number` query param (decimal integer string). | `common/config.go:L2151`, `architecture/evm/eth_getBlockReceipts.go:L181-L199` |

#### Network-level (EVM): `networks[].evm` (type `EvmNetworkConfig`) — integrity-adjacent fields

| # | YAML path | Type | Default | Behavior | Source |
|---|-----------|------|---------|----------|--------|
| 20 | `networks[].evm.integrity.enforceHighestBlock` | `*bool` | nil → `true`; `EvmIntegrityConfig.SetDefaults` sets it to `true` if nil (`common/defaults.go:L2117-L2120`); zero value before `SetDefaults` is nil | **Deprecated.** Use `directiveDefaults.enforceHighestBlock`. Type `EvmIntegrityConfig` is marked `@deprecated` in source (`common/config.go:L2308`). **Migration trigger**: during `NetworkConfig.SetDefaults`, if `n.Evm != nil && n.Evm.Integrity != nil` (user has populated the deprecated block at all), AND `directiveDefaults.EnforceHighestBlock == nil` (user has NOT set the new field), then this value is copied into `directiveDefaults.EnforceHighestBlock` (`common/defaults.go:L1957-L1959`). After migration, `DirectiveDefaultsConfig.SetDefaults` fills in `true` for any still-nil boolean fields, so the migration only has net effect when the user explicitly sets this field to `false`. **Critical gotcha**: the `projectPreForward_eth_blockNumber` hook reads ONLY this deprecated field at runtime — it does not read `directiveDefaults` — so disabling via the new path alone does not disable `eth_blockNumber` enforcement. Maps to `directiveDefaults.enforceHighestBlock` in the directive system. | `common/config.go:L2310`, `common/defaults.go:L2117-L2120`, `common/defaults.go:L1957-L1959` |
| 21 | `networks[].evm.integrity.enforceGetLogsBlockRange` | `*bool` | nil → `true`; `EvmIntegrityConfig.SetDefaults` sets it to `true` if nil (`common/defaults.go:L2121-L2123`); zero value before `SetDefaults` is nil | **Deprecated.** Use `directiveDefaults.enforceGetLogsBlockRange`. **Migration trigger**: same condition as row 20 — if the deprecated `Integrity` block is present and the new directive field is nil, the value is copied into `directiveDefaults.EnforceGetLogsBlockRange` (`common/defaults.go:L1960-L1962`). **Critical gotcha**: the `upstreamPreForward_eth_getLogs` and `upstreamPreForward_trace_filter` hooks read this deprecated field at runtime, not `directiveDefaults` — so the migration path only covers `eth_getBlockByNumber`-style hooks. Setting only `directiveDefaults.enforceGetLogsBlockRange` does not disable `eth_getLogs` or `trace_filter` range checks. Maps to `directiveDefaults.enforceGetLogsBlockRange`. | `common/config.go:L2312`, `common/defaults.go:L2121-L2123`, `common/defaults.go:L1960-L1962`, `architecture/evm/eth_getLogs.go:L283-L285`, `architecture/evm/trace_filter.go:L290-L292` |
| 22 | `networks[].evm.integrity.enforceNonNullTaggedBlocks` | `*bool` | nil → `true`; `EvmIntegrityConfig.SetDefaults` sets it to `true` if nil (`common/defaults.go:L2124-L2126`); zero value before `SetDefaults` is nil | **Deprecated.** Use `directiveDefaults.enforceNonNullTaggedBlocks`. **Migration trigger**: same condition as rows 20-21 — if the deprecated `Integrity` block is present and the new directive field is nil, the value is copied into `directiveDefaults.EnforceNonNullTaggedBlocks` (`common/defaults.go:L1963-L1965`). The `enforceNonNullBlock` function reads the directive (`dirs.EnforceNonNullTaggedBlocks`), so this deprecated field is only relevant as a source for the migration, not at runtime. Maps to `directiveDefaults.enforceNonNullTaggedBlocks`. | `common/config.go:L2314`, `common/defaults.go:L2124-L2126`, `common/defaults.go:L1963-L1965` |
| 23 | `networks[].evm.emptyResultConfidence` | `AvailbilityConfidence` | `""` serialized; `SetDefaults` for `EvmNetworkConfig` sets it to `AvailbilityConfidenceBlockHead` (int value 1) when nil/zero (`common/defaults.go:L2078`) | Controls when a null/empty concrete-block result is treated as retryable missing data vs. truthful not-yet-produced. The confidence determines the "head" threshold: (1) `blockHead` (default, value `"blockHead"`): the threshold is the network's `EvmHighestLatestBlockNumber`. Any concrete block number ≤ latest head whose lookup returns empty is treated as retryable missing data — the response is promoted to `ErrEndpointMissingData` and the network retry rotates to a different upstream. A block number > latest head is considered not-yet-produced, so the empty is returned truthfully without retry. (2) `finalizedBlock` (value `"finalizedBlock"`): the threshold is the network's `EvmHighestFinalizedBlockNumber` instead. This is stricter: blocks between the finalized head and the latest head (unfinalized) have their empty results treated as "not-yet-confirmed" (pass through truthfully) rather than as retryable missing data. Use `finalizedBlock` when upstreams sometimes provide empty responses for recent unfinalized blocks normally, and you only want retries for fully-confirmed blocks. **Fail-open conditions**: if the network's head is unknown (≤ 0), or the request does not target a concrete numeric block (uses a tag like `latest`/`finalized`, or a block hash), the guard fails open — the empty result is promoted to `ErrEndpointMissingData` and retried. | `common/config.go:L2241-L2250`, `common/defaults.go:L2078`, `common/architecture_evm.go:L33-L70`, `architecture/evm/common.go:L55-L89` |

**Deprecated `evm.integrity` migration — exact conditions.** `NetworkConfig.SetDefaults` (`common/defaults.go:L1952-L1966`) performs the migration as follows, in this order:
1. If `n.DirectiveDefaults == nil`, initialize it to an empty struct (`L1948-L1950`).
2. If `n.Evm != nil && n.Evm.Integrity != nil` (user set the deprecated block):
   - Copy `integrity.EnforceHighestBlock` → `directiveDefaults.EnforceHighestBlock` **only if** `directiveDefaults.EnforceHighestBlock == nil` (i.e., user has NOT set the new field).
   - Copy `integrity.EnforceGetLogsBlockRange` → `directiveDefaults.EnforceGetLogsBlockRange` **only if** `directiveDefaults.EnforceGetLogsBlockRange == nil`.
   - Copy `integrity.EnforceNonNullTaggedBlocks` → `directiveDefaults.EnforceNonNullTaggedBlocks` **only if** `directiveDefaults.EnforceNonNullTaggedBlocks == nil`.
3. Call `DirectiveDefaultsConfig.SetDefaults()` which fills in `true` for any still-nil boolean fields (`L1468-L1470`).
   
The migration is a best-effort copy that preserves explicit `false` values from the old config (unlike a plain "set default true if nil" approach). No warning is emitted to logs when the deprecated field is detected. The struct itself has no warning-emission path — the deprecation is documented in source comments only.

**Note on `eth_blockNumber`/`eth_getLogs`/`trace_filter` using deprecated field at runtime.** Three hooks have NOT been migrated to read from `directiveDefaults`: `projectPreForward_eth_blockNumber` (`architecture/evm/eth_blockNumber.go:L23-L29`), `upstreamPreForward_eth_getLogs` (`architecture/evm/eth_getLogs.go:L283-L285`), and `upstreamPreForward_trace_filter` (`architecture/evm/trace_filter.go:L290-L292`) — all three read `ncfg.Evm.Integrity.*` directly. Since `SetDefaults` also calls `EvmIntegrityConfig.SetDefaults` (which sets all three to `true`), these hooks will always be active unless the user explicitly sets `evm.integrity.*: false`, regardless of what `directiveDefaults` says. The `eth_getBlockByNumber` network post-forward hook correctly reads `dirs.EnforceHighestBlock` (the directive, populated from `directiveDefaults`).

#### Upstream-level: `upstreams[].integrity` (type `UpstreamIntegrityConfig`)

This is a separate upstream-scoped integrity config, distinct from the network-level `EvmIntegrityConfig`. Currently only covers `eth_getBlockReceipts` metadata. **There is no `SetDefaults` method for `UpstreamIntegrityConfig` or `UpstreamIntegrityEthGetBlockReceiptsConfig`** — all fields default to Go zero values: `bool` fields to `false`, `*bool` fields to `nil`. `common/config.go:L988-L1031`.

| # | YAML path | Type | Default | Behavior | Source |
|---|-----------|------|---------|----------|--------|
| 24 | `upstreams[].integrity.eth_getBlockReceipts.enabled` | `bool` | `false` (Go zero value; no `SetDefaults`) | **Currently non-functional.** Setting `enabled: true` has no observable effect. The `enabled` field is defined in the struct (`UpstreamIntegrityEthGetBlockReceiptsConfig.Enabled`) and copied in `Copy()`, but it is **not read by any validation or hook code** anywhere in the codebase. The sub-fields `checkLogIndexStrictIncrements` and `checkLogsBloom` are also non-functional (see rows 25-26). The entire `upstreams[].integrity.eth_getBlockReceipts` sub-block is a reserved/future placeholder. **Operators configuring this block must use `networks[].directiveDefaults.*` instead** — those directives are the only active path for receipt/log validation. The relationship between `enabled` and its sub-fields (`checkLogIndexStrictIncrements`, `checkLogsBloom`) is: even if `enabled: true` were to become functional in a future release, the sub-fields (nil by default) would still need to be explicitly set since they are `*bool` pointers with nil zero values. There is no code that maps `enabled: true` to any sub-field default. | `common/config.go:L1006-L1010` |
| 25 | `upstreams[].integrity.eth_getBlockReceipts.checkLogIndexStrictIncrements` | `*bool` | `nil` (zero value; not set by any `SetDefaults`) | Reserved / future. Not read by any validation code. The active control is `networks[].directiveDefaults.enforceLogIndexStrictIncrements`. | `common/config.go:L1008` |
| 26 | `upstreams[].integrity.eth_getBlockReceipts.checkLogsBloom` | `*bool` | `nil` (zero value; not set by any `SetDefaults`) | Reserved / future. Not read by any validation code. The active control is `networks[].directiveDefaults.validateLogsBloomMatch`. | `common/config.go:L1009` |

### Behaviors & algorithms

#### `eth_blockNumber` — highest-block enforcement

Hook: `projectPreForward_eth_blockNumber` (network pre-forward, intercepts before normal forward).

1. Check `ncfg.Evm.Integrity.EnforceHighestBlock` (legacy field; not migrated to directives). If nil or false, skip.
2. Forward normally via `network.Forward`.
3. Skip if response is from cache (to preserve cache TTL semantics).
4. Parse block number from response via `ExtractBlockReferenceFromResponse`.
5. Call `network.EvmHighestLatestBlockNumber(ctx)` to get cross-upstream maximum.
6. If highest > response block: increment `erpc_upstream_stale_latest_block_total`; replace response with a synthetic JSON-RPC response containing the highest hex block number. No re-request to any upstream — the highest block number from the poller is returned directly as-is.

Source: `architecture/evm/eth_blockNumber.go:L15-L121`

#### `eth_getBlockByNumber[latest/finalized]` — highest-block enforcement

Hook: `networkPostForward_eth_getBlockByNumber` → `enforceHighestBlock` (network post-forward).

1. If request error, skip.
2. Check `dirs.EnforceHighestBlock` from directives. If false, return as-is.
3. Skip cache responses.
4. Only applies to `latest` and `finalized` block tags (params[0]).
5. For `latest`: compare against `network.EvmHighestLatestBlockNumber`. For `finalized`: compare against `network.EvmHighestFinalizedBlockNumber`.
6. If highest > response block number: increment stale metric. Build a new internal request for the highest block, set `SkipCacheRead=true` and `UseUpstream=!<current-upstream-id>` (to exclude the lagging upstream). Forward again.
7. If re-request fails or returns lower block: call `pickHighestBlock` to return whichever of the two responses contains the higher block number. This guards against a corrupted state pointer.
8. Re-request also happens when `respBlockNumber == 0` (response is a JSON-RPC error): in that case `UseUpstream` is NOT set (the original upstream is still tried) since we cannot confirm it's actually behind.

Source: `architecture/evm/eth_getBlockByNumber.go:L111-L277`

#### `eth_getBlockByNumber` — non-null block enforcement

Hook: `enforceNonNullBlock` (called from `networkPostForward_eth_getBlockByNumber`).

1. If response is non-null and non-empty, pass through.
2. Determine if the request used a tag (not a `0x`-prefixed number).
3. For tagged blocks: check `dirs.EnforceNonNullTaggedBlocks`. If false, allow null.
4. Call `emptyResultBeyondConfidence` regardless of tag/numeric. If the block is beyond the confidence head (latest or finalized per `emptyResultConfidence`), return null truthfully (not an error).
5. Otherwise: return `ErrEndpointMissingData` with code `JsonRpcErrorMissingData`.

Numeric blocks ALWAYS error on null (no directive gate for that path), UNLESS they are beyond confidence.

Source: `architecture/evm/eth_getBlockByNumber.go:L279-L333`

#### `eth_getBlockByNumber` — block structure validation

Hook: `upstreamPostForward_eth_getBlockByNumber` → `validateBlock`.

Runs only when `dirs != nil` (avoids JSON parse overhead for directive-less requests).

Checks run in order:
1. **ValidateTransactionsRoot** (default `true`): transactionsRoot must be consistent with txs array. Handles phantom transactions (from=0x0, gas=0x0 — some chains like Polygon PoS inject internal system txs that don't participate in the trie).
2. **ValidateHeaderFieldLengths** (default `false`): validates byte-lengths of six header fields: `hash`, `parentHash`, `stateRoot`, `transactionsRoot`, `receiptsRoot` must each hex-decode to exactly 32 bytes; `logsBloom` must hex-decode to exactly 256 bytes. **Absent fields (empty string `""`) are skipped entirely** — each field is guarded by `if block.FieldName != ""` before decoding, so a block response that omits `stateRoot` does not fail this check. Only present but malformed or wrong-length fields trigger `ErrEndpointContentValidation`. Source: `architecture/evm/eth_getBlockByNumber.go:L615-L682`.
3. **ValidateTransactionFields** (default `false`): tx hash must be 32 bytes, unique within block.
4. **ValidateTransactionBlockInfo** (default `false`): tx.blockHash, tx.blockNumber, tx.transactionIndex must match block-level values.

Source: `architecture/evm/eth_getBlockByNumber.go:L489-L756`

#### `eth_getLogs` block-range availability

Hook: `upstreamPreForward_eth_getLogs`.

Guard: `ncfg.Evm.Integrity.EnforceGetLogsBlockRange` (note: reads the deprecated field, not directive). Skip if nil or false.

1. Skip if `blockHash` is present (EIP-234 path; no block range to check).
2. Resolve `fromBlock`/`toBlock` tags to hex numbers via `resolveBlockTagForGetLogs`. If either cannot be resolved (tags like `pending`, `safe`, `earliest`, or no state available), skip range check entirely.
3. If `fromBlock > toBlock`: return `ErrInvalidRequest`.
4. Call `CheckBlockRangeAvailability(ctx, up, "eth_getLogs", fromBlock, toBlock)`.

`CheckBlockRangeAvailability`:
- Checks `toBlock` with `forceFreshIfStale=true` (near head; want fresh data).
- Checks `fromBlock` with `forceFreshIfStale=false` (historical).
- Returns `ErrUpstreamBlockUnavailable` (retryable) if either block is unavailable.

Source: `architecture/evm/eth_getLogs.go:L273-L347`, `architecture/evm/block_range.go`

#### `trace_filter` / `arbtrace_filter` block-range availability

Hook: `upstreamPreForward_trace_filter`.

Same logic as `eth_getLogs`, guarded by the same `EnforceGetLogsBlockRange` config field. Only differs in that `fromBlock`/`toBlock` must already be hex numbers (`0x`-prefixed); if not, the hook returns without checking. Method name passed to `CheckBlockRangeAvailability` is the actual method name (e.g. `trace_filter`).

Source: `architecture/evm/trace_filter.go:L280-L356`

#### `eth_getBlockReceipts` structural validation

Hook: `upstreamPostForward_eth_getBlockReceipts` → `validateGetBlockReceipts`.

Skips if response is empty/null. Runs only when `dirs != nil`.

**Always-on checks** (run regardless of directives, only on fields that are present):
- Duplicate `transactionHash` across receipts → error.
- All receipts must have the same `blockHash` (if present).

**Directive-gated checks** (in order):
1. `ReceiptsCountExact` (nil = skip): exact count match.
2. `ReceiptsCountAtLeast` (nil = skip): minimum count.
3. `ValidationExpectedBlockHash`: all `receipt.blockHash` must match.
4. `ValidationExpectedBlockNumber`: all `receipt.blockNumber` must match.
5. `ValidateTxHashUniqueness`: stricter uniqueness — also rejects empty `transactionHash`.
6. `ValidateTransactionIndex`: `receipt.transactionIndex` must equal array index position.
7. `ValidateReceiptTransactionMatch` + `GroundTruthTransactions`: receipt[i].txHash must equal groundTruth[i].hash; requires equal counts.
8. `ValidateContractCreation` (requires `ValidateReceiptTransactionMatch`): contract creation tx must have `contractAddress`; non-creation tx must NOT have `contractAddress`.
9. Per-receipt log loop:
   - `ValidateLogsBloomEmptiness`: non-zero bloom ↔ logs present.
   - `ValidateLogFields`: address length (20 bytes), topics ≤4 and each 32 bytes, log.blockHash matches receipt.blockHash, log.blockNumber matches receipt.blockNumber, log.transactionHash/Index match receipt.
   - `EnforceLogIndexStrictIncrements`: global log index counter starts at 0, must increment by exactly 1 for every log across all receipts in the block. Missing logIndex → error.
   - `ValidateLogsBloomMatch`: re-derives bloom from all log addresses and topics using keccak256 (EIP-55 compatible `bloomAdd` function); 2048-bit bloom, big-endian bit setting; compares byte-for-byte with `receipt.logsBloom`.

Source: `architecture/evm/eth_getBlockReceipts.go:L55-L460`

#### HTTP headers and query parameters for per-request directive override

All booleans accept `"true"` (case-insensitive) to enable; any other value disables.

| Directive | HTTP Header | Query Parameter |
|-----------|-------------|-----------------|
| `EnforceHighestBlock` | `X-ERPC-Enforce-Highest-Block` | `enforce-highest-block` |
| `EnforceGetLogsBlockRange` | `X-ERPC-Enforce-GetLogs-Range` | `enforce-getlogs-range` |
| `EnforceNonNullTaggedBlocks` | `X-ERPC-Enforce-Non-Null-Tagged-Blocks` | `enforce-non-null-tagged-blocks` |
| `EnforceLogIndexStrictIncrements` | `X-ERPC-Enforce-Log-Index-Strict-Increments` | `enforce-log-index-strict-increments` |
| `ValidateLogsBloomEmptiness` | `X-ERPC-Validate-Logs-Bloom-Emptiness` | `validate-logs-bloom-emptiness` |
| `ValidateLogsBloomMatch` | `X-ERPC-Validate-Logs-Bloom-Match` | `validate-logs-bloom-match` |
| `ValidateTxHashUniqueness` | `X-ERPC-Validate-Tx-Hash-Uniqueness` | `validate-tx-hash-uniqueness` |
| `ValidateTransactionIndex` | `X-ERPC-Validate-Transaction-Index` | `validate-transaction-index` |
| `ValidateTransactionsRoot` | `X-ERPC-Validate-Transactions-Root` | `validate-transactions-root` |
| `ValidateHeaderFieldLengths` | `X-ERPC-Validate-Header-Field-Lengths` | `validate-header-field-lengths` |
| `ValidateTransactionFields` | `X-ERPC-Validate-Transaction-Fields` | `validate-transaction-fields` |
| `ValidateTransactionBlockInfo` | `X-ERPC-Validate-Transaction-Block-Info` | `validate-transaction-block-info` |
| `ValidateLogFields` | `X-ERPC-Validate-Log-Fields` | `validate-log-fields` |
| `ReceiptsCountExact` | `X-ERPC-Receipts-Count-Exact` (int string) | `receipts-count-exact` |
| `ReceiptsCountAtLeast` | `X-ERPC-Receipts-Count-At-Least` (int string) | `receipts-count-at-least` |
| `ValidationExpectedBlockHash` | `X-ERPC-Validation-Block-Hash` | `validation-block-hash` |
| `ValidationExpectedBlockNumber` | `X-ERPC-Validation-Block-Number` (int string) | `validation-block-number` |

Source: `common/request.go:L45-L114`, `L750-L900`

**Not exposable via HTTP** (library/programmatic only): `GroundTruthTransactions`, `GroundTruthLogs`. Source: `common/request.go:L210-L215`

### Observability

**Prometheus metrics** (all from `telemetry/metrics.go`):

| Metric | Type | Labels | When fired | Source |
|--------|------|--------|------------|--------|
| `erpc_upstream_stale_latest_block_total` | counter | project, vendor, network, upstream, category | Upstream returned a block number less than the network-known latest head. Fired in both `eth_blockNumber` and `eth_getBlockByNumber[latest]` enforcement. `category` value is the method name. | `telemetry/metrics.go:L306`, `architecture/evm/eth_blockNumber.go:L76`, `architecture/evm/eth_getBlockByNumber.go:L173` |
| `erpc_upstream_stale_finalized_block_total` | counter | project, vendor, network, upstream | Upstream returned a finalized block less than the network-known finalized head. Fired in `eth_getBlockByNumber[finalized]` enforcement. | `telemetry/metrics.go:L312`, `architecture/evm/eth_getBlockByNumber.go:L232` |
| `erpc_upstream_stale_upper_bound_total` | counter | project, vendor, network, upstream, category, confidence | Request skipped: upstream's latest block < requested toBlock. Fired during upstream selection (not in the integrity hooks). | `telemetry/metrics.go:L318` |
| `erpc_upstream_stale_lower_bound_total` | counter | project, vendor, network, upstream, category, confidence | Request skipped: requested fromBlock is below upstream's available lower bound. | `telemetry/metrics.go:L324` |
| `erpc_upstream_attempt_outcome_total` | counter | project, network, upstream, category, outcome, ... | `outcome=block_unavailable` when block range check fails; `outcome=missing_data` when missing data is returned; no dedicated outcome for content validation (counted under `server_error` path). | `telemetry/metrics.go:L457` |
| `erpc_network_retry_attempt_total` | counter | project, network, category, reason, finality | `reason=block_unavailable` on retry for unavailable blocks; `reason=missing_data` on retry for null responses. | `telemetry/metrics.go:L466` |

**OTel trace spans:**
- `Project.PreForwardHook.eth_blockNumber`: spans the `eth_blockNumber` highest-block enforcement. Attributes: `request.id`, `network.id`, `block.number`, `block.ref`, `highest_block`, `block.number_lag` (only in detailed tracing mode).
- `Network.PostForward.eth_getBlockByNumber`: spans the block enforcement and null check. Attributes: `request.id`, `network.id`, block lag attributes in detailed mode.
- `Upstream.PreForwardHook.eth_getLogs`: spans the block range availability check. Attributes: `request.id`, `network.id`, `upstream.id`.
- `Upstream.PreForwardHook.trace_filter`: attributes: `request.id`, `network.id`, `upstream.id`, `method`.
- `Upstream.PostForwardHook.eth_getBlockByNumber`: spans upstream block structure validation.
- `Upstream.PostForwardHook.eth_getBlockReceipts`: spans upstream receipt/log validation.
- `Evm.PickHighestBlock`: spans the `pickHighestBlock` comparison.

**Notable log messages:**
- `DEBUG`: `"upstream returned older block than we known, falling back to highest known block"` — emitted on `eth_blockNumber` stale detection (`architecture/evm/eth_blockNumber.go:L83-L98`).
- `DEBUG`: `"enforcing highest latest block"` / `"enforcing highest finalized block"` — emitted on `eth_getBlockByNumber` stale detection (`architecture/evm/eth_getBlockByNumber.go:L161-L168`, `L227-L233`).
- `TRACE`: `"skipping enforcement of highest block number as response is from cache"` — emitted when cache response bypasses enforcement.
- `WARN`: `"passed upstream is not a common.EvmUpstream"` — emitted in getLogs/trace_filter pre-forward hooks when upstream type assertion fails.

### Edge cases & gotchas

1. **`eth_blockNumber` uses the deprecated `EvmIntegrityConfig` directly, not `directiveDefaults`.** The `projectPreForward_eth_blockNumber` hook reads `ncfg.Evm.Integrity.EnforceHighestBlock` (`architecture/evm/eth_blockNumber.go:L23-L29`), while `eth_getBlockByNumber` uses the directive (`dirs.EnforceHighestBlock`). Since `SetDefaults` copies old `EvmIntegrityConfig` into `DirectiveDefaultsConfig` (but not vice versa), a config that only sets `directiveDefaults.enforceHighestBlock` would disable enforcement for `eth_getBlockByNumber[latest]` but NOT for `eth_blockNumber` — unless the legacy `evm.integrity.enforceHighestBlock` is also set (or the defaults fill it in). In practice, `EvmIntegrityConfig.SetDefaults` always sets it to `true`, so this is a hidden dual path.

2. **Cache responses bypass all highest-block enforcement.** Both `eth_blockNumber` and `eth_getBlockByNumber` hooks explicitly skip enforcement when `resp.FromCache()` is true. This is intentional (caching a correct-but-stale block number is expected), but means that a cached `eth_getBlockByNumber[latest]` response will not be upgraded even if a newer block is known. Correct approach: set appropriate TTL on the realtime cache policy.

3. **Future blocks return null, not an error — `emptyResultConfidence` controls the retry scope.** `enforceNonNullBlock` and `upstreamPostForward_markUnexpectedEmpty` both call `emptyResultBeyondConfidence`. This function determines whether to convert an empty/null response into `ErrEndpointMissingData` (retry) or return it truthfully.

   **`blockHead` (default)**: the confidence head is `EvmHighestLatestBlockNumber`. Retries happen for blocks at/below latest; blocks above latest return empty truthfully. This is the right setting when upstreams are expected to have all recent unfinalized blocks available.

   **`finalizedBlock`**: the confidence head is `EvmHighestFinalizedBlockNumber`. Only blocks at/below the finalized head trigger retry on empty; blocks between finalized and latest (unfinalized) return empty truthfully without retry. Use this when upstreams legitimately return empty for recent unfinalized blocks (e.g. archive nodes with a reorg lag, or some pruned nodes). This prevents retry storms on unconfirmed blocks but means callers may receive truthful-empty for valid-but-not-yet-finalized blocks.

   **Fail-open conditions**: if the head is unknown (≤ 0), or the request is for a tag (`latest`, `finalized`, etc.) rather than a concrete block number, `emptyResultBeyondConfidence` returns `false` → the empty is promoted to `ErrEndpointMissingData` and retried regardless of confidence setting.

   Tested in `architecture/evm/future_block_empty_test.go`. Source: `architecture/evm/common.go:L55-L89`, `common/defaults.go:L2078`, `common/architecture_evm.go:L33-L70`.

4. **Block tag resolution for getLogs can silently skip range checks.** If `fromBlock` or `toBlock` is a tag (`pending`, `safe`, `earliest`) that cannot be resolved by the state poller, the range check is skipped entirely and the request is forwarded to the upstream without validation (`architecture/evm/eth_getLogs.go:L327-L333`). This means range-related retries won't happen for such tags.

5. **`trace_filter` range check requires hex-prefixed block values; non-hex passes through.** Unlike `eth_getLogs` which can resolve block tags, `trace_filter` requires the filter `fromBlock`/`toBlock` to start with `0x`; otherwise the hook returns `(false, nil, nil)` immediately (`architecture/evm/trace_filter.go:L323-L341`).

6. **Phantom transactions on Polygon PoS, BSC.** The `validateTransactionsRoot` check has a special path for "phantom" system transactions (`from=0x0, gas=0x0`). These don't participate in the transactions Merkle trie, so a block with ONLY phantom transactions legitimately has an empty trie root. `allPhantomTransactions` checks this condition; if the response contains ONLY hashes (not full tx objects), it conservatively treats them as non-phantom. `architecture/evm/eth_getBlockByNumber.go:L574-L613`

7. **`EnforceLogIndexStrictIncrements` uses a GLOBAL log index counter across all receipts, not per-receipt.** Log indices restart at 0 for the first log of the block and must increment by 1 for every subsequent log across ALL receipts. Non-sequential per-receipt patterns (e.g. `[0,1], [1]`) will fail even if each receipt's own logs are sequential. Tested in `erpc/networks_integrity_test.go:L71-L113`.

8. **`ValidateTransactionsRoot` is `true` by default via `SetDefaults`, not via `EvmIntegrityConfig`.** This is the only non-trivially-always-on check set by `DirectiveDefaultsConfig.SetDefaults`. It can be disabled per-request via `X-ERPC-Validate-Transactions-Root: false` header.

9. **`ErrEndpointContentValidation` method returns HTTP 502, but wire HTTP status is 200.** `ErrorStatusCode()` returns `http.StatusBadGateway` (502) (`common/errors.go:L2745-L2747`). However, `ErrorStatusCode()` has ZERO call sites in the codebase — it is dead code. The actual wire HTTP status is determined by the switch in `handleErrorResponse` (`erpc/http_server.go:L1473-L1491`) and `determineResponseStatusCode` (`erpc/http_server.go:L1280-L1317`). `ErrCodeEndpointContentValidation` is NOT present in any arm of either switch, so clients receive HTTP 200 with a JSON-RPC error body (`{"jsonrpc":"2.0","error":{"code":-32603,"message":"..."}}`, translated via `TranslateToJsonRpcException`). Readers who see "HTTP 502" for this error should understand it is the method-level annotation only, not what reaches the wire. Census §1 row 72 documents this discrepancy explicitly.

10. **Upstream-level `UpstreamIntegrityConfig` is currently non-functional — configuring it has no effect.** The `enabled` field (default `false`) and sub-fields `checkLogIndexStrictIncrements` (default `nil`) and `checkLogsBloom` (default `nil`) under `upstreams[].integrity.eth_getBlockReceipts` are defined in the struct and preserved by `Copy()`, but are **not read by any validation or hook code** anywhere in the codebase. `enabled: true` does not activate any validation, and sub-fields are never checked. There is no `SetDefaults` for this type, so all fields remain at Go zero values unless explicitly set. The operative control for per-receipt validation is exclusively `networks[].directiveDefaults.*` — specifically `enforceLogIndexStrictIncrements` and `validateLogsBloomMatch`. The `UpstreamIntegrityConfig` struct is a reserved/future placeholder for upstream-scoped override functionality. `common/config.go:L1006-L1031`.

11. **`pickHighestBlock` protects against a corrupted state poller.** After re-fetching against the highest-known block in `enforceHighestBlock`, if the re-fetch returns a lower block than the original response, `pickHighestBlock` returns the original. This guards against a bug where `EvmHighestLatestBlockNumber` returns an incorrect (too-high) value. `architecture/evm/eth_getBlockByNumber.go:L335-L415`.

12. **`ValidateLogsBloomMatch` recomputes bloom using keccak256 with EIP-55 bit-setting.** The `bloomAdd` function uses the standard 2048-bit bloom filter algorithm (3 byte-positions set per value, derived from keccak256 of the value). The byte order is big-endian (`byteIndex = 255 - int(bitpos>>3)`). `architecture/evm/eth_getBlockReceipts.go:L468-L482`.

13. **Setting `directiveDefaults.enforceHighestBlock: false` does NOT disable `eth_blockNumber` enforcement.** The `projectPreForward_eth_blockNumber` hook checks `ncfg.Evm.Integrity.EnforceHighestBlock` (the deprecated struct field). Since `EvmIntegrityConfig.SetDefaults` always fills this in as `true`, the hook is always active unless you explicitly set `evm.integrity.enforceHighestBlock: false`. Conversely, setting the deprecated field to `false` disables the hook even if `directiveDefaults.enforceHighestBlock: true` — the two code paths are entirely independent at runtime. `architecture/evm/eth_blockNumber.go:L23-L29`, `common/defaults.go:L2117-L2120`.

14. **Setting `directiveDefaults.enforceGetLogsBlockRange: false` does NOT disable `eth_getLogs` or `trace_filter` range checks.** Both `upstreamPreForward_eth_getLogs` (`architecture/evm/eth_getLogs.go:L283-L285`) and `upstreamPreForward_trace_filter` (`architecture/evm/trace_filter.go:L290-L292`) read `ncfg.Evm.Integrity.EnforceGetLogsBlockRange` directly, not the directive. Same split-path behavior as item 13 above.

15. **`validationExpectedBlockHash` and `validationExpectedBlockNumber` only apply to `eth_getBlockReceipts` — not to `eth_getBlockByNumber`.** These fields are checked only inside `validateGetBlockReceipts` (`architecture/evm/eth_getBlockReceipts.go:L168-L199`), which is called only from the `eth_getBlockReceipts` post-forward hook. Using them for `eth_getBlockByNumber` validation has no effect. The block-structure validation for `eth_getBlockByNumber` uses only `validateBlock` which does not read these fields.

16. **`validateTransactionFields` and `validateTransactionBlockInfo` silently skip hash-only transaction responses.** Both checks are inside the `if len(fullTxs) > 0` branch in `validateBlockTransactions` (`architecture/evm/eth_getBlockByNumber.go:L564-L568`). The outer loop at `L543-L568` converts each transaction: `map[string]interface{}` → `blockValidationTxLite` (full tx), `string` → `continue` (skipped). If all transactions are hashes (i.e., the request used `hydrated=false`), `fullTxs` remains empty and `validateBlockTransactions` is never called. No error, no warning — the directives have no effect for hash-only responses.

### Source map

- `architecture/evm/eth_blockNumber.go` — `projectPreForward_eth_blockNumber`: pre-forward hook for `eth_blockNumber`; reads legacy `EvmIntegrityConfig.EnforceHighestBlock` directly.
- `architecture/evm/eth_getBlockByNumber.go` — `networkPostForward_eth_getBlockByNumber`, `enforceHighestBlock`, `enforceNonNullBlock`, `pickHighestBlock`, `validateBlock`, `validateHeaderFieldLengths`, `validateBlockTransactions`, `allPhantomTransactions`: all block-level network and upstream integrity checks.
- `architecture/evm/eth_getLogs.go` — `upstreamPreForward_eth_getLogs`: block-range availability guard; `resolveBlockTagForGetLogs`: tag-to-hex resolution.
- `architecture/evm/eth_getBlockReceipts.go` — `upstreamPostForward_eth_getBlockReceipts`, `validateGetBlockReceipts`, `bloomAdd`, `isZeroBloom`: all receipt/log structural validation.
- `architecture/evm/trace_filter.go` — `upstreamPreForward_trace_filter`: block-range availability guard for trace methods, sharing `EnforceGetLogsBlockRange`.
- `architecture/evm/block_range.go` — `CheckBlockRangeAvailability`: shared block-range availability check using `EvmAssertBlockAvailability`.
- `architecture/evm/common.go` — `upstreamPostForward_markUnexpectedEmpty`, `emptyResultBeyondConfidence`: empty-result-to-error promotion and confidence-head guard.
- `common/config.go` — `EvmIntegrityConfig` (deprecated, lines 2308-2316), `DirectiveDefaultsConfig` (lines 2105-2152), `UpstreamIntegrityConfig` (lines 988-1031), `EvmNetworkConfig.EmptyResultConfidence` (line 2250): all integrity config type definitions.
- `common/defaults.go` — `DirectiveDefaultsConfig.SetDefaults` (lines 1454-1471): populates the three core defaults (`true` for enforceHighestBlock, enforceGetLogsBlockRange, enforceNonNullTaggedBlocks, validateTransactionsRoot); `EvmIntegrityConfig.SetDefaults` (lines 2117-2126): legacy struct defaults; migration logic (lines 1952-1964).
- `common/request.go` — `RequestDirectives` struct (lines 116-215), HTTP header/query-param names and parsing (lines 45-114, 745-900): the runtime directive surface exposed to HTTP callers.
- `erpc/networks_integrity_test.go` — integration tests for receipt/log validation via network.Forward, covering logIndex contiguity, bloom consistency, count assertions, ground-truth cross-validation, and retry-fallback behavior.
- `architecture/evm/eth_getBlockByNumber_test.go` — unit tests for `enforceNonNullBlock` and `TestEnforceNonNullTaggedBlocks`.
- `architecture/evm/future_block_empty_test.go` — unit tests for `emptyResultBeyondConfidence` with both blockHead and finalizedBlock confidence modes, and for `enforceNonNullBlock` with future blocks.
- `architecture/evm/eth_getLogs_test.go` — unit tests for block range availability integration.
- `architecture/evm/trace_filter_test.go` — unit tests for trace_filter block range enforcement.
- `telemetry/metrics.go` — `MetricUpstreamStaleLatestBlock` (line 306), `MetricUpstreamStaleFinalizedBlock` (line 312), `MetricUpstreamStaleUpperBound` (line 318), `MetricUpstreamStaleLowerBound` (line 324): stale-block counters fired by integrity enforcement.
