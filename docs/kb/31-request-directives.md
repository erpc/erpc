# KB: Request directives (headers & query params)

> status: complete
> source-dirs: common/request.go, common/config.go, common/defaults.go, common/matcher.go, common/tracing_core.go, common/tracing_util.go, erpc/http_server.go, erpc/network_executor.go, erpc/networks.go, architecture/evm/

## L1 — Capability (CTO view)

Request directives are per-request overrides that callers send as HTTP headers (`X-ERPC-*`) or URL query parameters to modify the proxy's routing, caching, validation, and consensus behavior for that single request without changing server-side configuration. The same 23 directives can also be wired as network-level `directiveDefaults` to set defaults for all requests on a given network. Response headers (`X-ERPC-*` diagnostics) expose what actually happened: which upstream won, cache hit/miss, retry/hedge counts, and per-upstream attempt traces.

## L2 — Mechanics (staff-engineer view)

**Parsing pipeline.** For every HTTP request, `EnrichFromHttp` (`common/request.go:702`) is called after `ApplyDirectiveDefaults` (`erpc/http_server.go:652-658`). The precedence order, lowest to highest, is: (1) network `directiveDefaults` config block (applied once, no-op on subsequent calls), (2) HTTP headers, (3) URL query parameters. `ApplyDirectiveDefaults` is a no-op if directives are already populated (`common/request.go:570-572`), so any explicit value from HTTP/query always wins over config defaults — per-request overrides are always honored.

**Early-exit on no directives.** `EnrichFromHttp` first scans all 23 registered header/query names via `hasDirectiveInHeaders` / `hasDirectiveInQueryParams` (`common/request.go:678-700`). If none present, it returns immediately after extracting the User-Agent; no lock is taken and no allocation happens. This keeps the fast path zero-cost.

**Copy-on-write directives.** When directives ARE present, `EnrichFromHttp` clones any pre-existing directives struct before mutating it (`common/request.go:724-728`). This prevents race conditions when sub-requests in batch mode share the same directive pointer.

**Boolean parse rule.** For ALL `X-ERPC-*` directive headers (#1-23): truthy = `strings.ToLower(strings.TrimSpace(v)) == "true"`. The ONLY truthy value is the exact string `"true"` (case-insensitive, surrounding whitespace stripped). `"1"` and `"yes"` are NOT truthy for directive headers — `ToLower("1") == "1"` which is not equal to `"true"`, and similarly for `"yes"`. Anything other than `"true"` (after trim+lower) evaluates to `false`. (Confirmed: `common/request_test.go:406-427`). **Exception for headers #20-23:** `X-ERPC-Validate-Header-Field-Lengths`, `X-ERPC-Validate-Transaction-Fields`, `X-ERPC-Validate-Transaction-Block-Info`, `X-ERPC-Validate-Log-Fields` use `strings.ToLower(hv) == "true"` WITHOUT `TrimSpace` (`common/request.go:798,801,804,807`), so `"  true  "` evaluates to `false` for these four headers. Query-param parsers always apply `TrimSpace` before comparison. **Asymmetry with `X-ERPC-Force-Trace`:** `X-ERPC-Force-Trace` uses a DIFFERENT truthy set: `"true"`, `"1"`, or `"yes"` are ALL truthy (`common/tracing_util.go:107-115`). This header is NOT a directive header — it is handled by the tracing subsystem separately. A user switching between `X-ERPC-Force-Trace: 1` (works) and `X-ERPC-Retry-Empty: 1` (does NOT work) will see confusing behavior. Always use `"true"` as the canonical truthy value for all `X-ERPC-*` headers to avoid this asymmetry.

**`X-ERPC-Skip-Cache-Read` special value.** Accepts `"true"` (skip all connectors), `"false"` / `""` (skip none), or a connector-ID wildcard pattern (e.g., `"redis*"`, `"memory*|dynamo*"`). Matching via `ShouldSkipCacheRead` calls `WildcardMatch(pattern, connectorId)` (`common/request.go:895-914`). The pattern grammar is a full boolean glob: `*` wildcard, `|` OR, `&` AND, `!` NOT, `(` `)` grouping (`common/matcher.go:220-265`).

**`X-ERPC-Use-Upstream` routing.** When `UseUpstream == ""` (nil config / absent header), `shouldSkip` never calls `UpstreamMatchesSelector` — all upstreams are eligible. When non-empty, the value is a selector pattern matched against upstream ID and upstream tags via `UpstreamMatchesSelector` → `MatchesSelector` (`common/matcher.go:73-112`, `upstream/upstream.go:1526-1534`). Matching: (a) first tests the pattern against upstream `.id`; (b) only for purely-positive patterns (no `!`), also tests against each element of upstream `.Tags` — any tag match → true. Negated patterns (`!`) match ID only. Header value is NOT trimmed; query param IS trimmed (`common/request.go:741` vs `:812`). When the selector matches NO upstream: every upstream returns `ErrUpstreamNotAllowed`; after the full sweep exhausts all candidates, the final error is `ErrUpstreamsExhausted` wrapping the collection of `ErrUpstreamNotAllowed` errors. The caller sees an exhausted-upstreams error, not a "no upstream matched" specific error. Tested: `erpc/networks_forward_test.go:592-610`. Selector-scoped served-tip: when `use-upstream` is active, the network's `latest`/`finalized` tip is computed only among the matching upstream subset, preventing "block not found" churn when groups have different head blocks (`erpc/networks.go:374-402`).

**`X-ERPC-Skip-Consensus` routing.** Checked in `networkExecutor.Run` (`erpc/network_executor.go:179-188`). When true, bypasses the entire consensus branch and falls through to the standard `retry(hedge(runUpstreamSweep))` path. Retry, hedge, breaker, and timeout still apply. Only the consensus dispute/agreement step is skipped. This is end-to-end tested in `erpc/skip_consensus_directive_test.go`.

**`X-ERPC-Retry-Empty` and `X-ERPC-Retry-Pending`.** Checked in `shouldRetryWithReason` (`erpc/network_executor.go:374-462`). `RetryEmpty` triggers re-attempt on `IsResultEmptyish()` responses (null block, empty array). `RetryPending` triggers re-attempt specifically on `eth_getTransactionReceipt`, `eth_getTransactionByHash`, `eth_getTransactionByBlockHashAndIndex`, `eth_getTransactionByBlockNumberAndIndex` when the response shows the tx is still pending. Both are subject to `EmptyResultMaxAttempts` cap (`erpc/network_executor.go:363-368`). The struct comment "true by default" for `RetryPending` (`common/request.go:125`) is STALE — no code sets a Go-level default; Go zero value is `false`, and only an explicit `directiveDefaults.retryPending: true` in config sets it.

**`X-ERPC-Skip-Interpolation`.** Checked in the EVM normalization layer (`architecture/evm/json_rpc.go:132`). When true, block-tag → hex-number substitution is suppressed in outbound request params. Block references are still computed and cached internally (for finality/metrics), but `"latest"`/`"finalized"` are preserved as-is in the forwarded payload.

**`X-ERPC-Enforce-Highest-Block`.** Default `true` via `DirectiveDefaultsConfig.SetDefaults` (`common/defaults.go:1458-1460`). Applied in `enforceHighestBlock` (`architecture/evm/eth_getBlockByNumber.go:111`): skips enforcement for cached responses; for `"latest"`/`"finalized"` params, compares the response block number against the network's known highest and re-routes if the response is behind.

**`X-ERPC-Enforce-GetLogs-Range` and `X-ERPC-Enforce-Non-Null-Tagged-Blocks`.** Default `true`. `EnforceGetLogsBlockRange` is currently consumed via the legacy `ncfg.Evm.Integrity` path (`architecture/evm/eth_getLogs.go:284`, `architecture/evm/trace_filter.go:291`) — the network's `Integrity` field is backfilled from `DirectiveDefaults` during `SetDefaults` (`common/defaults.go:1957-1964`). `EnforceNonNullTaggedBlocks` is checked in `architecture/evm/eth_getBlockByNumber.go:304`.

**`X-ERPC-Validate-Transactions-Root`.** Default `true` via `DirectiveDefaultsConfig.SetDefaults` (`common/defaults.go:1467-1469`). Checked in block validation at `architecture/evm/eth_getBlockByNumber.go:515`. Headers and query params can override the config default either direction (tested in `common/request_test.go:328-385`).

**Validation directives (receipts, logs, bloom).** The receipt/log validation cluster (`ReceiptsCountExact`, `ReceiptsCountAtLeast`, `ValidationExpectedBlockHash`, `ValidationExpectedBlockNumber`, `ValidateLogsBloomEmptiness`, `ValidateLogsBloomMatch`, `ValidateLogFields`, `ValidateReceiptTransactionMatch`, `ValidateContractCreation`) are consumed in `architecture/evm/eth_getBlockReceipts.go:149-419`. `ValidateReceiptTransactionMatch` and `ValidateContractCreation` require `GroundTruthTransactions` which is library-mode only (not HTTP-settable).

**Response diagnostics.** The `X-ERPC-*` response headers expose execution counters, cache hit/miss, winning upstream id, duration, and a per-attempt trace. They are emitted via `setResponseHeaders` → `writeCounterHeaders` / `writeResponseMetadataHeaders` / `writeUpstreamTraceHeaders` (`erpc/http_server.go:1088-1272`). The `executionHeaders` server config (`all` / `summary` / `off`) controls which subset is emitted; default is `all` (`erpc/http_server.go:1081-1086`). These headers are NOT emitted for batch responses (`erpc/http_server.go:703-720`).

**gRPC path.** HTTP X-ERPC-* request directives are NOT parsed on the gRPC path — `RequestProcessor.ProcessUnary` applies only `directiveDefaults` and never calls `EnrichFromHttp` (census note, `erpc/request_processor.go:35-67`).

**`directiveDefaults` config placement.** Settable at:
- `projects[].networks[].directiveDefaults` (network-level) — `common/config.go:2001`
- `projects[].networks[].failsafe.directiveDefaults` (failsafe-scope) — `common/config.go:597, 630`

---

## L3 — Exhaustive reference (agent view)

### Config fields

**`directiveDefaults` block** (`projects[].networks[].directiveDefaults.*`). Full struct: `common/config.go:2105-2152`. Applied via `ApplyDirectiveDefaults` (`common/request.go:563-676`). `skipCacheRead` accepts `bool` or `string` in YAML/JSON; normalized to string by custom UnmarshalYAML/UnmarshalJSON (`common/config.go:2154-2175`).

**Nil vs false semantics for `*bool` fields.** Every field in `DirectiveDefaultsConfig` that is a `*bool` has three possible states: (1) `nil` — field was not set in config; `ApplyDirectiveDefaults` skips it entirely (`common/request.go:575-579`), so the request's `RequestDirectives` field stays at its Go zero value (`false`); (2) pointer to `false` — field was explicitly set to `false` in config; `ApplyDirectiveDefaults` writes `false` into the request directive, overriding any implicit default that might come from elsewhere; (3) pointer to `true` — field was explicitly set to `true` in config; applied as `true`. The practical implication: if you set `retryPending: false` at `networkDefaults` level and omit it at a specific network, the network inherits `false`. If you omit it at `networkDefaults` AND the specific network, both stay at Go zero `false`. There is no parent-inheriting "nil means use parent value" chain at runtime — the only parent is the `directiveDefaults` block that was passed to `ApplyDirectiveDefaults`. The nil check in `ApplyDirectiveDefaults` is purely a "was this field explicitly set?" guard, not a multi-level inheritance chain. (`common/request.go:563-676`, `common/config.go:2105-2152`)

| Field | Type | Default (request struct zero / SetDefaults) | Notes | Source |
|---|---|---|---|---|
| `retryEmpty` | `*bool` | **nil** → request field stays `false` (Go zero). Nil means unset; false means explicitly disabled. Only a non-nil `*bool` is applied to the request directive. | No SetDefaults default. Setting `retryEmpty: false` in `directiveDefaults` is meaningless (same as omitting it) unless a per-request HTTP header would otherwise set it. | `common/config.go:2106`, `common/request.go:575-577` |
| `retryPending` | `*bool` | **nil** → request field stays `false` (Go zero). Struct comment at `common/request.go:125` says "true by default" — this is STALE. No code sets a default. Nil and false both produce `false` at runtime; only an explicit `retryPending: true` in config or `X-ERPC-Retry-Pending: true` header enables retries. | No SetDefaults default. See edge-case #1. | `common/config.go:2107`, `common/request.go:578-580` |
| `skipCacheRead` | `interface{}` (bool or string in YAML/JSON; stored as string after unmarshal) | nil → `""` (no skip). If `true` in YAML, normalized to `"true"` string. If `false`, normalized to `"false"` string. If a connector-id pattern string, kept as-is. The normalization happens in `UnmarshalYAML` / `UnmarshalJSON` via `fmt.Sprintf("%v", v)`. At `ApplyDirectiveDefaults`, a nil interface is skipped; a non-nil interface is written as-is to `SkipCacheRead string`. | `"true"` = skip ALL connectors; `"false"` / `""` = skip none; any other string = connector-id WildcardMatch pattern (e.g., `"redis*"`, `"memory*\|dynamo*"`). See bool-to-string normalization details below. | `common/config.go:2108`, `common/config.go:2154-2175`, `common/request.go:581-588` |
| `useUpstream` | `*string` | **nil** → runtime field stays `""` (no filter — any upstream can serve). A non-nil pointer copies the string value. When the runtime field is `""`, `shouldSkip` in `upstream.go` never checks `UpstreamMatchesSelector`, so every upstream is eligible. | Supports same WildcardMatch / tag-matching selector syntax as used by `MatchesSelector`. Negated patterns (`!`) match upstream `id` only (tag matching disabled for negations). When the selector matches NO upstream, every upstream returns `ErrUpstreamNotAllowed`; the sweep exhausts all candidates and the final error is `ErrUpstreamsExhausted`. Tested: `erpc/networks_forward_test.go:592-610`. | `common/config.go:2109`, `common/request.go:589-591`, `upstream/upstream.go:1526-1534`, `common/errors.go:1432-1440` |
| `skipInterpolation` | `*bool` | nil → `false` | | `common/config.go:2110` |
| `skipConsensus` | `*bool` | nil → `false` | | `common/config.go:2111` |
| `enforceHighestBlock` | `*bool` | **`true`** via `SetDefaults` | | `common/defaults.go:1458-1460` |
| `enforceGetLogsBlockRange` | `*bool` | **`true`** via `SetDefaults` | | `common/defaults.go:1461-1463` |
| `enforceNonNullTaggedBlocks` | `*bool` | **`true`** via `SetDefaults` | | `common/defaults.go:1464-1466` |
| `validateTransactionsRoot` | `*bool` | **`true`** via `SetDefaults` | Disable for non-standard chains | `common/defaults.go:1467-1469` |
| `validateHeaderFieldLengths` | `*bool` | nil → `false` | | `common/config.go:2123` |
| `validateTransactionFields` | `*bool` | nil → `false` | | `common/config.go:2126` |
| `validateTransactionBlockInfo` | `*bool` | nil → `false` | | `common/config.go:2127` |
| `enforceLogIndexStrictIncrements` | `*bool` | nil → `false` | | `common/config.go:2130` |
| `validateTxHashUniqueness` | `*bool` | nil → `false` | | `common/config.go:2131` |
| `validateTransactionIndex` | `*bool` | nil → `false` | | `common/config.go:2132` |
| `validateLogFields` | `*bool` | nil → `false` | | `common/config.go:2133` |
| `validateLogsBloomEmptiness` | `*bool` | nil → `false` | | `common/config.go:2137` |
| `validateLogsBloomMatch` | `*bool` | nil → `false` | | `common/config.go:2139` |
| `validateReceiptTransactionMatch` | `*bool` | nil → `false` | Config + library mode only; no HTTP header/query. **Behavior:** When `true` AND `GroundTruthTransactions` is non-empty (library mode only), asserts that `len(receipts) == len(groundTruthTransactions)` and that `receipt[i].transactionHash` (decoded from hex) == `groundTruthTransactions[i].Hash` (raw bytes) for every `i`. Any length mismatch or hash mismatch triggers `ErrEndpointContentValidation`. If `GroundTruthTransactions` is empty/nil (e.g., over HTTP), the check is silently skipped even when the directive is `true`. `ValidateContractCreation` is only evaluated inside the same loop, so it too requires `GroundTruthTransactions`. (`architecture/evm/eth_getBlockReceipts.go:235-300`) | `common/config.go:2142`, `architecture/evm/eth_getBlockReceipts.go:235-262` |
| `validateContractCreation` | `*bool` | nil → `false` | Config + library mode only; no HTTP header/query. **Behavior:** Only active inside the `ValidateReceiptTransactionMatch` loop (requires `GroundTruthTransactions` to be non-empty). Per transaction `gtTx`: if `gtTx.To` is empty/nil (contract-creation tx), receipt MUST have a non-empty `contractAddress` that decodes to exactly 20 bytes (`evm.AddressLength`); if `gtTx.To` is non-empty (regular tx), receipt MUST NOT have `contractAddress`. Any violation triggers `ErrEndpointContentValidation`. (`architecture/evm/eth_getBlockReceipts.go:264-299`) | `common/config.go:2143`, `architecture/evm/eth_getBlockReceipts.go:264-299` |
| `receiptsCountExact` | `*int64` | nil (don't check) | | `common/config.go:2146` |
| `receiptsCountAtLeast` | `*int64` | nil (don't check) | | `common/config.go:2147` |
| `validationExpectedBlockHash` | `*string` | nil | | `common/config.go:2150` |
| `validationExpectedBlockNumber` | `*int64` | nil | | `common/config.go:2151` |

**`server.executionHeaders`** — controls which response diagnostic headers are emitted. Type `ExecutionHeadersMode` (`common/config.go:172-183`). Values: `"all"` (default; full counter + per-attempt trace), `"summary"` (counters only — skips ONLY `X-ERPC-Upstreams`, all other `X-ERPC-*` headers still emitted), `"off"` (skips ALL `X-ERPC-*` diagnostic headers — no counters, no cache, no winner, nothing). Default resolution: nil pointer → `all` (`erpc/http_server.go:1082-1083`). Important distinction: `"summary"` removes only the per-attempt trace header (`X-ERPC-Upstreams`); `"off"` removes the entire diagnostic surface. The always-present `X-ERPC-Version` and `X-ERPC-Commit` headers are NOT gated by this setting — they are emitted on every response regardless (`erpc/http_server.go:259-261`). (`erpc/http_server.go:1105-1127`)

---

### Behaviors & algorithms

#### Complete directive registry (23 directives)

All parsed in `EnrichFromHttp` (`common/request.go:730-892`). Header names: `common/request.go:38-62`. Query names: `common/request.go:64-88`. Registry: `common/request.go:90-114`.

Precedence (lowest → highest): `directiveDefaults` config → HTTP header → URL query param.

| # | HTTP header | Query param | Value type | Field | Default | Effect | Consumed at |
|---|---|---|---|---|---|---|---|
| 1 | `X-ERPC-Retry-Empty` | `retry-empty` | bool | `RetryEmpty` | `false` | On `true`: retry when upstream returns empty/null result (null block, empty logs array). Subject to `EmptyResultMaxAttempts` cap. | `erpc/network_executor.go:427-442` |
| 2 | `X-ERPC-Retry-Pending` | `retry-pending` | bool | `RetryPending` | `false` (struct comment "true by default" is STALE) | On `true`: retry `eth_getTransactionReceipt`, `eth_getTransactionByHash`, `eth_getTransactionByBlockHashAndIndex`, `eth_getTransactionByBlockNumberAndIndex` when tx is pending. Subject to `EmptyResultMaxAttempts` cap. | `erpc/network_executor.go:445-459` |
| 3 | `X-ERPC-Skip-Cache-Read` | `skip-cache-read` | string (but accepts bool in `directiveDefaults` config — normalized to string at unmarshal) | `SkipCacheRead` | `""` (no skip) | **Three modes:** (1) `"true"` (case-insensitive) = skip ALL connectors; (2) `"false"` / `""` (case-insensitive or empty) = use cache normally; (3) any other non-empty string = connector-id WildcardMatch pattern — skip only matching connectors. **Bool-to-string normalization:** in `directiveDefaults` config, setting `skipCacheRead: true` (YAML bool) is automatically normalized to the string `"true"` by custom `UnmarshalYAML`/`UnmarshalJSON` via `fmt.Sprintf("%v", v)` (`common/config.go:2154-2175`). Setting `skipCacheRead: false` becomes `"false"` (equivalent to empty — no skip). This means YAML `true`/`false` and JSON `true`/`false` both work. **Pattern matching:** `ShouldSkipCacheRead(connectorId)` checks `WildcardMatch(pattern, connectorId)`; when `connectorId` is `""` (pre-consultation), returns false. Grammar: glob `*`, `\|` OR, `&` AND, `!` NOT, `()` grouping. Header value is NOT trimmed; query param IS trimmed. | `common/request.go:898-914`, `common/config.go:2154-2175` |
| 4 | `X-ERPC-Use-Upstream` | `use-upstream` | string | `UseUpstream` | `""` (nil in config = no filter — any upstream can serve; `""` in request directive = same, no filtering) | **Nil / empty behavior:** `UseUpstream == ""` means `shouldSkip` skips the selector check entirely and every upstream is eligible (`upstream/upstream.go:1526-1527`). **Non-empty selector:** matched against upstream `.id` first; for purely-positive patterns (no `!`), also matched against each `.Tags` element (any tag match → true). Negated patterns (`!`) match ID only (not tags). Header is NOT trimmed; query IS trimmed. **When no upstream matches:** each upstream returns `ErrUpstreamNotAllowed`; the sweep exhausts all candidates → final error is `ErrUpstreamsExhausted` (`erpc/networks_forward_test.go:592-610`). Activates selector-scoped served-tip partitioning (`erpc/networks.go:374-402`). Stateful methods with multiple matching upstreams fail with `ErrNotImplemented`. | `common/request.go:1424-1448`, `erpc/networks.go:382-402`, `upstream/upstream.go:1526-1534` |
| 5 | `X-ERPC-Skip-Interpolation` | `skip-interpolation` | bool | `SkipInterpolation` | `false` | Suppresses block-tag → hex substitution in outbound params. Block references still computed/cached internally. | `architecture/evm/json_rpc.go:132` |
| 6 | `X-ERPC-Skip-Consensus` | `skip-consensus` | bool | `SkipConsensus` | `false` | Bypasses consensus branch; falls through to `retry(hedge(runUpstreamSweep))`. Retry, hedge, breaker, timeout still apply. | `erpc/network_executor.go:179-188` |
| 7 | `X-ERPC-Enforce-Highest-Block` | `enforce-highest-block` | bool | `EnforceHighestBlock` | **`true`** | For `eth_getBlockByNumber` with `"latest"`/`"finalized"`: reject responses behind the network's known highest block and re-route. Skipped for cached responses. | `architecture/evm/eth_getBlockByNumber.go:111-170` |
| 8 | `X-ERPC-Enforce-GetLogs-Range` | `enforce-getlogs-range` | bool | `EnforceGetLogsBlockRange` | **`true`** | Pre-forward hook rejects `eth_getLogs` / `trace_filter` requests whose block range exceeds `getLogsMaxAllowedRange`. | `architecture/evm/eth_getLogs.go:280-288`, `architecture/evm/trace_filter.go:285-295` |
| 9 | `X-ERPC-Enforce-Non-Null-Tagged-Blocks` | `enforce-non-null-tagged-blocks` | bool | `EnforceNonNullTaggedBlocks` | **`true`** | Treats null block response for tag-based `eth_getBlockByNumber` as an error (triggers retry). | `architecture/evm/eth_getBlockByNumber.go:304` |
| 10 | `X-ERPC-Enforce-Log-Index-Strict-Increments` | `enforce-log-index-strict-increments` | bool | `EnforceLogIndexStrictIncrements` | `false` | Consumed in `validateGetBlockReceipts` (`architecture/evm/eth_getBlockReceipts.go:399-416`), which is called from `eth_getBlockReceipts` post-forward hook. Iterates every log across all receipts in order. For each log: (a) rejects an empty `logIndex` field, (b) rejects invalid UTF-8 in `logIndex`, (c) parses `logIndex` as hex uint64, (d) compares to a monotonically-incrementing expected counter starting at 0. Fails with `ErrEndpointContentValidation` if any log's index is not exactly `previous + 1`. Absent logs (receipts with empty log array) are skipped. The counter does NOT reset between receipts — indices must be globally contiguous across all receipts in the block response. | `architecture/evm/eth_getBlockReceipts.go:399-416` |
| 11 | `X-ERPC-Validate-Logs-Bloom-Emptiness` | `validate-logs-bloom-emptiness` | bool | `ValidateLogsBloomEmptiness` | `false` | If logs exist, bloom must be non-zero; if bloom is non-zero, logs must exist. | `architecture/evm/eth_getBlockReceipts.go:307-321` |
| 12 | `X-ERPC-Validate-Logs-Bloom-Match` | `validate-logs-bloom-match` | bool | `ValidateLogsBloomMatch` | `false` | Recalculates the 2048-bit Bloom filter from each receipt's logs and verifies it matches `receipt.logsBloom`. **Algorithm:** for each log, feeds the 20-byte address and each 32-byte topic into `bloomAdd` which computes three bit positions using keccak256 (SHA3): `pos[k] = (keccak256(value)[2k] << 8 \| keccak256(value)[2k+1]) & 0x7FF` for k∈{0,1,2}; each position sets one bit in the big-endian 256-byte array. (`architecture/evm/eth_getBlockReceipts.go:468-480`). **For `eth_getBlockReceipts`:** no ground truth needed — logs are embedded in the response. **For other methods** (e.g., `eth_getBlockByNumber`): `GroundTruthLogs` must be injected in library mode, but the consumption path for non-receipt methods is not yet fully implemented (see `GroundTruthLogs` entry). Skips receipts where `len(r.Logs) == 0`. | `architecture/evm/eth_getBlockReceipts.go:419-456, 468-480` |
| 13 | `X-ERPC-Validate-Tx-Hash-Uniqueness` | `validate-tx-hash-uniqueness` | bool | `ValidateTxHashUniqueness` | `false` | Consumed in `validateGetBlockReceipts` (`architecture/evm/eth_getBlockReceipts.go:202-213`), which is called from the `eth_getBlockReceipts` post-forward hook. Stricter than the always-on duplicate check: (a) **rejects any receipt with an empty `transactionHash`** (always-on only rejects duplicates when the field is present), (b) lower-cases and deduplicates all hashes in a map; any duplicate triggers `ErrEndpointContentValidation`. Both checks — empty-hash rejection and duplicate-hash rejection — are active together when the directive is true. The always-on check at `eth_getBlockReceipts.go:119-129` remains active regardless of this directive and only catches duplicates among non-empty hashes. | `architecture/evm/eth_getBlockReceipts.go:202-213` |
| 14 | `X-ERPC-Validate-Transaction-Index` | `validate-transaction-index` | bool | `ValidateTransactionIndex` | `false` | Consumed in `validateGetBlockReceipts` (`architecture/evm/eth_getBlockReceipts.go:217-233`), called from the `eth_getBlockReceipts` post-forward hook. For every receipt in the response array (at array position `i`): if `transactionIndex` is absent/empty, skips that receipt; otherwise parses the field as hex int64 and asserts `transactionIndex == i`. Any mismatch triggers `ErrEndpointContentValidation`. Detects reordered receipts or responses where receipts were assembled out-of-order. | `architecture/evm/eth_getBlockReceipts.go:217-233` |
| 15 | `X-ERPC-Receipts-Count-Exact` | `receipts-count-exact` | int64 (decimal) | `ReceiptsCountExact` | nil (don't check) | Requires exactly N receipts in `eth_getBlockReceipts` response. nil = unset. Parse failure silently ignores the directive. | `architecture/evm/eth_getBlockReceipts.go:149-155` |
| 16 | `X-ERPC-Receipts-Count-At-Least` | `receipts-count-at-least` | int64 (decimal) | `ReceiptsCountAtLeast` | nil (don't check) | Requires at least N receipts in `eth_getBlockReceipts` response. | `architecture/evm/eth_getBlockReceipts.go:157-163` |
| 17 | `X-ERPC-Validation-Expected-Block-Hash` | `validation-expected-block-hash` | string | `ValidationExpectedBlockHash` | `""` | Expected block hash for receipt validation. Case-insensitive, `0x` prefix stripped at comparison site. | `architecture/evm/eth_getBlockReceipts.go:168-170` |
| 18 | `X-ERPC-Validation-Expected-Block-Number` | `validation-expected-block-number` | int64 (decimal) | `ValidationExpectedBlockNumber` | nil | Expected block number for receipt validation. | `architecture/evm/eth_getBlockReceipts.go:182-184` |
| 19 | `X-ERPC-Validate-Transactions-Root` | `validate-transactions-root` | bool | `ValidateTransactionsRoot` | **`true`** | Checks transactionsRoot against transaction count. Disable for non-standard chains with unusual trie roots. | `architecture/evm/eth_getBlockByNumber.go:515` |
| 20 | `X-ERPC-Validate-Header-Field-Lengths` | `validate-header-field-lengths` | bool | `ValidateHeaderFieldLengths` | `false` | **Stale struct comment** at `common/request.go:172` says "only via config/library, not HTTP headers" — this is WRONG. The code at `common/request.go:797-798` and `:881-882` parses this directive from both the HTTP header and the `validate-header-field-lengths` query param. Consumed in `validateBlock` → `validateHeaderFieldLengths` (`architecture/evm/eth_getBlockByNumber.go:535-538`, `615-683`), called from `upstreamPostForward_eth_getBlockByNumber`. Checks that the following block header fields, when present, decode to the correct byte length: `hash` (32 bytes), `parentHash` (32 bytes), `stateRoot` (32 bytes), `transactionsRoot` (32 bytes), `receiptsRoot` (32 bytes), `logsBloom` (256 bytes). Any field that is empty/absent is skipped. Parse: header uses `strings.ToLower(hv) == "true"` WITHOUT TrimSpace; query param uses TrimSpace. | `architecture/evm/eth_getBlockByNumber.go:535-538`, `615-683`; stale comment `common/request.go:172` |
| 21 | `X-ERPC-Validate-Transaction-Fields` | `validate-transaction-fields` | bool | `ValidateTransactionFields` | `false` | Consumed in `validateBlock` → `validateBlockTransactions` (`architecture/evm/eth_getBlockByNumber.go:687-708`), called only when the block response contains full transaction objects (not hash-only). For each transaction: (a) if `hash` is present, decodes it and asserts length == 32 bytes; (b) lower-cases the hash and checks a seen-map — duplicate hashes trigger `ErrEndpointContentValidation`. Absent/empty `hash` fields are skipped. Parse: header uses `strings.ToLower(hv) == "true"` WITHOUT TrimSpace. | `architecture/evm/eth_getBlockByNumber.go:687-708` |
| 22 | `X-ERPC-Validate-Transaction-Block-Info` | `validate-transaction-block-info` | bool | `ValidateTransactionBlockInfo` | `false` | Consumed in `validateBlock` → `validateBlockTransactions` (`architecture/evm/eth_getBlockByNumber.go:711-753`), called only for full transaction objects. Three cross-checks per transaction, each skipped if the field is absent: (a) `tx.blockHash` must equal the parent block's `hash` (case-insensitive, `0x` stripped); (b) `tx.blockNumber` decoded as hex int64 must equal the parent block's `number`; (c) `tx.transactionIndex` decoded as hex int64 must equal the transaction's array position `i`. Any mismatch triggers `ErrEndpointContentValidation`. Parse: header uses `strings.ToLower(hv) == "true"` WITHOUT TrimSpace. | `architecture/evm/eth_getBlockByNumber.go:711-753` |
| 23 | `X-ERPC-Validate-Log-Fields` | `validate-log-fields` | bool | `ValidateLogFields` | `false` | Consumed in `validateGetBlockReceipts` inner log loop (`architecture/evm/eth_getBlockReceipts.go:324-397`), called from the `eth_getBlockReceipts` post-forward hook. Per log (when field is present and non-empty): (a) `address` decoded as hex must be exactly 20 bytes (`evm.AddressLength`); (b) `topics` array length must not exceed `evm.MaxTopics`; each topic decoded as hex must be exactly 32 bytes (`evm.TopicLength`); (c) `log.blockHash` must match `receipt.blockHash` (case-insensitive, `0x` stripped); (d) `log.blockNumber` hex must equal `receipt.blockNumber` hex; (e) `log.transactionHash` must match `receipt.transactionHash`; (f) `log.transactionIndex` hex must equal `receipt.transactionIndex` hex. Empty/absent fields are always skipped. Parse: header uses `strings.ToLower(hv) == "true"` WITHOUT TrimSpace. | `architecture/evm/eth_getBlockReceipts.go:324-397` |

#### Directive-adjacent request inputs (not in the directive registry)

| Input | Kind | Values / behavior | Source |
|---|---|---|---|
| `X-ERPC-Force-Trace` | header | `"true"`, `"1"`, `"yes"` → bypass OTel trace sampling for this request | `common/tracing_core.go:29`, `common/tracing_util.go:107-110` |
| `force-trace` | query param | same three truthy values | `common/tracing_core.go:31`, `common/tracing_util.go:113-115` |
| `user-agent` | query param | overrides `User-Agent` header for agent-name metrics tracking | `common/request.go:1299-1314` |
| `User-Agent` | header | stored raw or simplified per `project.userAgentMode`; drives agent-name metric labels | `common/request.go:1300-1311` |
| `networkId` | JSON body field | `"evm:42161"`-style; network selection when arch/chain absent from URL | `erpc/http_server.go:613-634` |

#### Directive fields NOT settable via HTTP (config or library only)

| Field | Settable via | Notes | Source |
|---|---|---|---|
| `ByPassMethodExclusion` | internal only (`json:"-"`) | **Behavior:** When `true`, `Upstream.Forward` skips the `shouldSkip` call (`upstream/upstream.go:444-450`). `shouldSkip` is where `ShouldHandleMethod` is evaluated — i.e., where `ignoreMethods` / `allowMethods` configured on the upstream are checked (`upstream/upstream.go:1375-1376`, `1517-1524`). With `ByPassMethodExclusion=true`, a request that would otherwise be rejected as "method not allowed or ignored" is forwarded anyway. This is used exclusively by the shadow execution path (`erpc/shadow.go:119`), which calls `ups.Forward(ctx, shadowReq, true, false)` — the `true` parameter is the `byPassMethodExclusion` argument. The shadow path performs its own per-upstream `ShouldHandleMethod` check earlier (`erpc/shadow.go:54-62`) and needs to bypass the second check inside `Forward`. Note: the TODO comment at `upstream/upstream.go:411` acknowledges this should perhaps be moved to directives but raises a security concern (preventing clients from setting it). Currently `json:"-"` ensures it is never exposed over HTTP. | `common/request.go:142`, `upstream/upstream.go:410-450`, `erpc/shadow.go:54-62, 119` |
| `IsInternal` | internal only; bypasses retry/hedge/breaker | Set by state poller, chainId probe, vendor detection | `common/request.go:144-148` |
| `ValidateReceiptTransactionMatch` | `directiveDefaults` config or library mode | No HTTP header/query key | `common/request.go:193`, `common/config.go:2142` |
| `ValidateContractCreation` | `directiveDefaults` config or library mode | No HTTP header/query key | `common/request.go:195`, `common/config.go:2143` |
| `GroundTruthTransactions` | Go library mode only (`json:"-"`) | **Type:** `[]*GroundTruthTransaction` where each element has `Hash []byte` (required for matching), `To []byte` (nil/empty = contract-creation), and `TransactionIndex *uint32`. **Purpose:** enables `ValidateReceiptTransactionMatch` and `ValidateContractCreation`. These two directives are silently no-ops when `GroundTruthTransactions` is nil or empty — there is no way to enable them over HTTP since the data cannot be passed as a header. Library callers inject this slice before calling `network.Forward`. The slice must have exactly as many entries as receipts are expected in the response; a count mismatch triggers `ErrEndpointContentValidation` before any per-receipt checks run. (`common/request.go:206-226`, `architecture/evm/eth_getBlockReceipts.go:235-300`) | `common/request.go:210`, `architecture/evm/eth_getBlockReceipts.go:235-262` |
| `GroundTruthLogs` | Go library mode only (`json:"-"`) | **Type:** `[]*GroundTruthLog` where each element has `Address []byte` and `Topics [][]byte`. **Purpose:** enables bloom validation (`ValidateLogsBloomMatch`) for methods whose response does not contain logs directly (e.g., `eth_getBlockByNumber`). For `eth_getBlockReceipts`, no ground truth is needed because logs are embedded in every receipt in the response — the bloom check at `eth_getBlockReceipts.go:419-456` iterates `r.Logs` directly. For other methods (e.g., `eth_getBlockByNumber`), log data is absent from the response so `GroundTruthLogs` must be injected programmatically. Currently, the `GroundTruthLogs` field is defined and cloned (`common/request.go:297-311`) but the consumption path for non-receipt methods is not yet implemented in the architecture layer (no code reads `dirs.GroundTruthLogs` outside `common/request.go`). The field is forward-compatible infrastructure. Verify actual consumption in the architecture layer before relying on this for non-`eth_getBlockReceipts` methods. (`common/request.go:212-214`, `architecture/evm/eth_getBlockReceipts.go:419-456`) | `common/request.go:214`, `architecture/evm/eth_getBlockReceipts.go:419-456` |

#### Auth directives (headers / query params) — not in directive registry

Parsed in `auth.NewPayloadFromHttp` (`auth/http.go:13-88`). First match wins:

| Order | Input | Kind | Auth type |
|---|---|---|---|
| 1 | `?token=` | query | secret (deprecated) |
| 2 | `?secret=` | query | secret |
| 3 | `X-ERPC-Secret-Token` | header | secret |
| 4 | `Authorization: Basic <b64 user:pass>` | header | secret (password only) |
| 5 | `Authorization: Bearer <jwt>` | header | jwt |
| 6 | `?jwt=` | query | jwt |
| 7 | `?signature=` + `?message=` | query | siwe |
| 8 | `X-Siwe-Message` + `X-Siwe-Signature` | headers | siwe |
| 9 | (none) | — | network/IP strategy |

#### `X-ERPC-Use-Upstream` selector grammar

The selector is evaluated by `UpstreamMatchesSelector` → `MatchesSelector` → `WildcardMatch` (`common/matcher.go:34-112`):

- `*` — matches any substring (glob wildcard)
- `|` — OR operator
- `&` — AND operator  
- `!` — NOT operator (negation; when used, tag matching is disabled — ID match only)
- `(` `)` — grouping
- Spaces are token delimiters (not part of patterns)

Examples: `"alchemy"`, `"my-*"`, `"alchemy|quicknode"`, `"family:systx"` (tag match), `"!drpc"` (exclude by ID), `"(alchemy|quicknode)&!beta"`.

The pattern is first tested against the upstream's `id`; on failure and only for purely-positive patterns (no `!`), it is also tested against each element of `cfg.Tags` (any tag match → true).

#### `X-ERPC-Skip-Cache-Read` connector pattern grammar

Same `WildcardMatch` grammar as use-upstream, but applied to `connectorId` strings. Special cases handled before pattern matching (`common/request.go:898-913`):
- `""` or `"false"` (case-insensitive) → never skip
- `"true"` (case-insensitive) → always skip all connectors
- Any other string → `WildcardMatch(value, connectorId)` — skip only matching connectors

When `connectorId` is empty string (pre-consultation check, not evaluating a specific connector), pattern-matching is skipped and `false` is returned.

#### Response headers set by eRPC

**Always on every HTTP response:**

| Header | Value | Source |
|---|---|---|
| `Content-Type` | `application/json` | `erpc/http_server.go:259` |
| `X-ERPC-Version` | `common.ErpcVersion` | `erpc/http_server.go:260` |
| `X-ERPC-Commit` | `common.ErpcCommitSha` | `erpc/http_server.go:261` |
| custom `server.responseHeaders` | static/env-expanded | `erpc/http_server.go:263-266` |

**Execution diagnostic headers** (single-response only, NOT batch; controlled by `server.executionHeaders`):

`setResponseHeaders` is called for success (`erpc/http_server.go:723`) and error paths (`erpc/http_server.go:1496`). Batch branch never calls it (`erpc/http_server.go:703-720`).

| Header | Value | When emitted | Source |
|---|---|---|---|
| `X-ERPC-Attempts` | int, total physical ops (upstream + cache; network rotations not summed) | always (0 when no ExecState) | `erpc/http_server.go:1160` |
| `X-ERPC-Upstream-Attempts` | int | always | `erpc/http_server.go:1161` |
| `X-ERPC-Upstream-Retries` | int | always | `erpc/http_server.go:1162` |
| `X-ERPC-Upstream-Hedges` | int | always | `erpc/http_server.go:1163` |
| `X-ERPC-Network-Attempts` | int (network rotation count) | always | `erpc/http_server.go:1164` |
| `X-ERPC-Network-Retries` | int | always | `erpc/http_server.go:1165` |
| `X-ERPC-Network-Hedges` | int | always | `erpc/http_server.go:1166` |
| `X-ERPC-Cache-Attempts` | int | only when > 0 | `erpc/http_server.go:1171` |
| `X-ERPC-Cache-Retries` | int | only when > 0 | `erpc/http_server.go:1172` |
| `X-ERPC-Cache-Hedges` | int | only when > 0 | `erpc/http_server.go:1173` |
| `X-ERPC-Consensus-Slots` | int | only when > 0 | `erpc/http_server.go:1175-1177` |
| `X-ERPC-Consensus-Disputes` | int | only when > 0 | `erpc/http_server.go:1178-1180` |
| `X-ERPC-Consensus-Low-Participants` | int | only when > 0 | `erpc/http_server.go:1181-1183` |
| `X-ERPC-Cache` | `HIT` or `MISS` | when response metadata exists & non-null | `erpc/http_server.go:1193-1195` |
| `X-ERPC-Upstream` | winning upstream id | when known | `erpc/http_server.go:1198` |
| `X-ERPC-Duration` | milliseconds (int64) | NormalizedResponse only | `erpc/http_server.go:1202` |
| `X-ERPC-Upstreams` | per-attempt trace string: segments joined by `;`, each with format `<upstreamId>=<reason>:<outcome>:<durationMs>ms[:won]`. Fields: `upstreamId` = upstream config id; `reason` = attempt reason string (e.g., `primary`, `hedge`, `retry`, `consensus_slot`); `outcome` = attempt outcome string (e.g., `success`, `timeout`, `exec_revert`, `rate_limited`); `durationMs` = integer milliseconds; `:won` suffix appended only when `a.Won == true` (i.e., this attempt's response contributed to the final answer — for single-winner requests that's the winning upstream; for consensus, every member of the winning agreement group). Example: `alchemy=primary:success:50ms:won;quicknode=hedge:timeout:5000ms;drpc=consensus_slot:exec_revert:20ms` | mode `all` only (absent in `summary` and `off` modes); only set when `len(attempts) > 0` | `erpc/http_server.go:1226-1272` |

**Tracing / CORS / compression headers** (see KB 02-http-server for full details):

| Header | When |
|---|---|
| `traceparent` / `tracestate` | tracing enabled |
| `Access-Control-*` | CORS origin matches |
| `Content-Encoding: gzip` + `Vary: Accept-Encoding` | `server.enableGzip` AND client sent `Accept-Encoding: gzip` AND first write ≥ 1024 bytes |

---

### Observability

Directives themselves have no dedicated Prometheus metrics; their effects are captured by existing metrics:
- Retry counts due to `RetryEmpty` / `RetryPending` appear in `erpc_upstream_request_retries_total{reason="empty_result"}` and `{reason="pending_tx"}` (see `erpc/network_executor.go:441,458`).
- `SkipConsensus` = true means consensus metrics (`ConsensusSlots`, `ConsensusDisputes`) are never incremented for that request.
- Response headers `X-ERPC-Attempts`, `X-ERPC-Upstream-Retries`, etc. reflect the same counters as ExecState which feeds Prometheus.

Trace spans:
- `Request.Lock` / `Request.RLock` (`common/request.go:924-934`) — detail spans inside directive mutations.
- `X-ERPC-Force-Trace: true` forces the OTel sampler to record the span regardless of sampling rate; the span attribute `erpc.force_trace = true` is set (`common/tracing_util.go:89-100`).

Log:
- `trace` level: `applied request directives` with `Interface("directives", nq.Directives())` after every `EnrichFromHttp` call (`erpc/http_server.go:659`).

---

### Edge cases & gotchas

1. **`RetryPending` stale struct comment.** `common/request.go:125` says "This value is 'true' by default" — this is incorrect. The Go zero value is `false`. No `SetDefaults` sets it. Must be explicitly enabled via `directiveDefaults.retryPending: true` in config or `X-ERPC-Retry-Pending: true` header. (`common/request.go:125-128`, verified: no entry in `common/defaults.go`)

2. **`X-ERPC-Use-Upstream` header is NOT trimmed; query IS.** `common/request.go:741` stores the raw header value; `common/request.go:812` wraps with `strings.TrimSpace`. A header with leading/trailing whitespace won't match any upstream.

3. **Headers #20-23 lack TrimSpace.** `X-ERPC-Validate-Header-Field-Lengths`, `X-ERPC-Validate-Transaction-Fields`, `X-ERPC-Validate-Transaction-Block-Info`, `X-ERPC-Validate-Log-Fields` use `strings.ToLower(hv) == "true"` without TrimSpace. `"  true  "` in these headers evaluates to `false`. All other boolean headers DO trim first. (`common/request.go:798,801,804,807` vs `:732`)

4. **`ApplyDirectiveDefaults` is idempotent / no-op after first call.** If any directive source has already initialized `r.directives` (even to an empty struct), the config defaults are silently skipped. This is by design: HTTP headers → query params always win over config defaults. But the guard checks `r.directives != nil`, so code that calls `ApplyDirectiveDefaults` twice or calls it after `EnrichFromHttp` will never overwrite request-level overrides. (`common/request.go:570-572`)

5. **`EnforceGetLogsBlockRange` and `EnforceHighestBlock` are consumed from `ncfg.Evm.Integrity` in the architecture layer, not from the request directive.** The `SetDefaults` migration path copies `DirectiveDefaults` values into `Evm.Integrity` (`common/defaults.go:1957-1964`), so the directive does take effect, but the consumption path is the legacy `EvmIntegrityConfig` struct in `eth_getLogs.go:284` and `trace_filter.go:291`, NOT a per-request directive check. This means these two directives currently cannot be overridden per-request via HTTP headers even though they are in the directive registry — the architecture layer always reads from the network config. This may be a future bug or intentional design.

6. **Selector-scoped served-tip partitions are capped.** When `use-upstream` is active, eRPC may materialize a served-tip partition (shared-state counter) for the selector. The count is capped at `maxServedTipPartitions` per network instance (`erpc/networks.go:429`). Beyond the cap the partition is not created and the stateless fallback is used silently — no error.

7. **`X-ERPC-Skip-Consensus: false` with an explicit string `"false"` actively disables SkipConsensus.** Because the value `"false"` is not equal to `"true"`, the parsed bool is `false` and the consensus branch runs normally. The absence of the header leaves `SkipConsensus` at its config-default value; presence with `"false"` overrides it to false. Tested: `common/request_test.go:409`, `erpc/skip_consensus_directive_test.go:125-153`.

8. **`ReceiptsCountExact` / `ReceiptsCountAtLeast` parse failures are silent.** If `strconv.ParseInt` fails on the header/query value, the directive pointer stays nil and no validation fires. No error or warning is logged. (`common/request.go:777-779`, `common/request.go:782-784`)

9. **`ValidateTransactionsRoot` default is `true` from `SetDefaults`, but only when `SetDefaults` runs on a `DirectiveDefaultsConfig` that exists.** If no `directiveDefaults` block is declared in the network config, `SetDefaults` is called on a freshly allocated `DirectiveDefaultsConfig` when the network's defaults are computed (`common/defaults.go:1454-1471`). If for any reason the config path skips `SetDefaults`, the Go zero value is `false`. Verified correct path: `common/defaults.go:1467-1469` sets `true`.

10. **gRPC requests never receive HTTP directives.** `RequestProcessor.ProcessUnary` only calls `ApplyDirectiveDefaults`; it never calls `EnrichFromHttp`. Callers using the gRPC interface cannot override directives on a per-request basis; they must use network-level `directiveDefaults` config. (`erpc/request_processor.go:35-67`)

11. **Batch responses never emit `X-ERPC-*` diagnostic headers.** The `setResponseHeaders` call is inside the single-response write path; the batch write path (`erpc/http_server.go:703-720`) does not call it. Clients using JSON-RPC batch must inspect per-response `id` fields rather than headers for diagnostics.

12. **`SkipInterpolation` suppresses outbound rewrites but not internal caching.** Block references (finality state, block numbers for cache keying) are still extracted and cached internally when `SkipInterpolation=true`. Only the outbound JSON params are left unchanged. (`architecture/evm/json_rpc.go:132`, `common/request.go:150-153`)

13. **`ValidateHeaderFieldLengths` struct comment is STALE.** `common/request.go:172` says "only via config/library, not HTTP headers." In reality the code at `common/request.go:797-798` parses `X-ERPC-Validate-Header-Field-Lengths` from the HTTP header, and `common/request.go:881-882` parses `validate-header-field-lengths` from query params. The directive is fully HTTP-settable. The struct comment is a copy-paste leftover and should be removed.

14. **`EnforceLogIndexStrictIncrements` counts globally across all receipts, not per-receipt.** The expected counter starts at 0 and increments with every log across every receipt in the array. A block with receipt[0] having 3 logs and receipt[1] having 2 logs expects logIndex sequence 0,1,2,3,4 — not 0,1,2 then 0,1 again. Upstream nodes that reset logIndex per-receipt will fail this check. (`architecture/evm/eth_getBlockReceipts.go:304`, `399-415`)

15. **`ValidateTxHashUniqueness` is strictly stronger than the always-on duplicate check.** The always-on check at `eth_getBlockReceipts.go:119-129` skips receipts where `transactionHash == ""`. The directive-gated `ValidateTxHashUniqueness` at `:202-213` rejects any empty hash outright. Enabling this directive on a chain that legitimately omits transaction hashes from receipts will cause all responses to fail.

16. **`ValidateTransactionFields` only fires for full-object transactions; hash-only blocks skip it.** When `eth_getBlockByNumber` is called with `false` for the full-transactions flag, the block response contains an array of hash strings, not objects. The validation code at `architecture/evm/eth_getBlockByNumber.go:542-562` iterates the array and skips any element that is a `string` rather than a `map[string]interface{}`. If the array is all strings, `fullTxs` remains nil and validation is entirely bypassed. `ValidateTransactionBlockInfo` has the same skip-if-hashes behavior.

17. **`ValidateLogFields` cross-checks log fields against the enclosing receipt, not the block.** When `log.blockHash` differs from `receipt.blockHash`, the error is triggered. But both are only compared when non-empty — a log with an absent `blockHash` field passes silently even if the receipt's `blockHash` is set. This means a partially-populated response (e.g. an RPC node that omits log context fields) will not be caught unless all fields are present.

18. **`nil *bool` in `DirectiveDefaultsConfig` ≠ `false *bool`: the distinction matters for multi-level configs.** In `DirectiveDefaultsConfig`, a nil `*bool` means "not set — do not touch the request directive field". A `*false` pointer means "explicitly disabled". In `ApplyDirectiveDefaults`, only non-nil pointers are applied. Consequence: if you set `retryPending: false` at `networkDefaults` level, it writes `false` to request directives. If a sub-network omits `retryPending`, it inherits the `false` because `ApplyDirectiveDefaults` is idempotent (once directives are set for a request, the next call is a no-op). However, if both levels omit `retryPending`, the request directive stays at Go zero `false`. There is no cascading "use grandparent value" logic — the single `directiveDefaults` block passed to `ApplyDirectiveDefaults` is the only source. (`common/request.go:570-580`)

19. **`"true"` is the ONLY truthy value for `X-ERPC-*` directive headers; `"1"` and `"yes"` will silently evaluate to `false`.** The parse expression `strings.ToLower(strings.TrimSpace(v)) == "true"` means only `"true"` (any case, with optional whitespace) is truthy. Values like `"1"`, `"yes"`, `"on"`, `"enabled"` all evaluate to `false`. This is unlike `X-ERPC-Force-Trace` which accepts `"true"`, `"1"`, or `"yes"` as truthy (`common/tracing_util.go:107-115`). Always use `"true"` for all `X-ERPC-*` headers to avoid ambiguity. (`common/request_test.go:406-427`)

20. **`executionHeaders: summary` removes ONLY `X-ERPC-Upstreams`; `executionHeaders: off` removes ALL `X-ERPC-*` diagnostics.** A common misconception is that `summary` is a "quieter" mode that removes most headers — it only suppresses the per-attempt trace (`X-ERPC-Upstreams`). All counter headers (`X-ERPC-Attempts`, `X-ERPC-Upstream-Retries`, etc.) and metadata headers (`X-ERPC-Cache`, `X-ERPC-Upstream`, `X-ERPC-Duration`) are still emitted in `summary` mode. Use `off` to eliminate all diagnostic headers. `X-ERPC-Version` and `X-ERPC-Commit` are NOT controlled by `executionHeaders` at all — they are always emitted. (`erpc/http_server.go:1105-1127`)

21. **`ValidateReceiptTransactionMatch` and `ValidateContractCreation` are silently no-ops when called via HTTP** because `GroundTruthTransactions` cannot be supplied via HTTP headers. The directive fields (`X-ERPC-Validate-Receipt-Transaction-Match` / config `validateReceiptTransactionMatch`) can be set, but the code at `eth_getBlockReceipts.go:236` guards: `if dirs.ValidateReceiptTransactionMatch && len(dirs.GroundTruthTransactions) > 0`. Without `GroundTruthTransactions`, no validation fires. Library callers must inject both the directive and the ground-truth slice. (`architecture/evm/eth_getBlockReceipts.go:235-300`)

22. **`UseUpstream` selector failure produces `ErrUpstreamsExhausted`, not a selector-specific error.** When the selector matches no upstream, each upstream individually returns `ErrUpstreamNotAllowed`. The exhaustion collector wraps these into `ErrUpstreamsExhausted`. The HTTP response will have a JSON-RPC error body with code `ErrUpstreamsExhausted` — there is no distinct error code for "selector matched no upstream". Operators must parse the error message text to diagnose selector mismatches. (`common/errors.go:1432-1440`, `erpc/networks_forward_test.go:592-610`)

---

### Source map

- `common/request.go` — `RequestDirectives` struct definition (L116-215), `EnrichFromHttp` (L702-893), `ApplyDirectiveDefaults` (L563-676), `ShouldSkipCacheRead` (L898-914), `NextUpstream` (L1407-1478), `Clone` (L237-315)
- `common/config.go` — `DirectiveDefaultsConfig` struct (L2105-2152), custom YAML/JSON unmarshal for `skipCacheRead` (L2154-2175), `ExecutionHeadersMode` type and constants (L170-183), `EvmIntegrityConfig` deprecated struct (L2308-2316)
- `common/defaults.go` — `DirectiveDefaultsConfig.SetDefaults` (L1454-1471), defaults for `enforceHighestBlock/enforceGetLogsBlockRange/enforceNonNullTaggedBlocks/validateTransactionsRoot` (L1458-1469), migration from `EvmIntegrityConfig` to `DirectiveDefaults` (L1952-1964)
- `common/matcher.go` — `WildcardMatch` (L34-47), `MatchesSelector` (L49-99), `UpstreamMatchesSelector` (L102-112), tokenizer grammar (L220-265)
- `common/tracing_core.go` — `ForceTraceHeader` / `ForceTraceQueryParam` constants (L28-31)
- `common/tracing_util.go` — `shouldForceTrace` truthy-value parsing (L106-119)
- `erpc/http_server.go` — directive application pipeline (L652-658), `executionHeadersMode` (L1081-1086), `setResponseHeaders` (L1105-1127), `writeCounterHeaders` (L1158-1184), `writeResponseMetadataHeaders` (L1189-1204), `writeUpstreamTraceHeaders` (L1226-1252), `formatUpstreamAttempt` (L1254-1272)
- `erpc/network_executor.go` — `SkipConsensus` branch (L179-188), `shouldRetryWithReason` covering `RetryEmpty` / `RetryPending` / `RetryEmpty` cap (L374-462)
- `erpc/networks.go` — `requestSelector` (L394-403), selector-scoped served-tip (`L374-392`), `servedTipPartitionFor` (L412-441)
- `erpc/skip_consensus_directive_test.go` — end-to-end tests for `SkipConsensus` via header, query, config, false-value, no-directive, retry-still-applies
- `common/request_test.go` — unit tests for `EnrichFromHttp` bloom headers (L293-323), `ValidateTransactionsRoot` header-overrides-config (L328-385), `SkipConsensus` directive full suite (L391-541)
- `architecture/evm/eth_getBlockByNumber.go` — `enforceHighestBlock` (L111-170), `EnforceNonNullTaggedBlocks` check (L304), `validateBlock` entry (L492-571), `ValidateTransactionsRoot` check (L515), `ValidateHeaderFieldLengths` → `validateHeaderFieldLengths` (L535-538, L615-683), `ValidateTransactionFields` → `validateBlockTransactions` (L687-708), `ValidateTransactionBlockInfo` (L711-753)
- `architecture/evm/eth_getBlockReceipts.go` — receipts validation directives consumption: always-on dedup (L114-143), `ValidateTxHashUniqueness` (L202-213), `ValidateTransactionIndex` (L217-233), `ValidateReceiptTransactionMatch` + `ValidateContractCreation` (L235-300), `ValidateLogFields` per-log loop (L324-397), `EnforceLogIndexStrictIncrements` (L399-416), `ValidateLogsBloomEmptiness` (L307-319), `ValidateLogsBloomMatch` + `bloomAdd` algorithm (L419-480)
- `architecture/evm/json_rpc.go` — `SkipInterpolation` check (L132)
- `auth/http.go` — auth credential precedence parsing (L13-88)
- `upstream/upstream.go` — `Forward` entry point with `byPassMethodExclusion` param (L410-450), `shouldSkip` method covering `ShouldHandleMethod` + `UseUpstream` check (L1505-1548), `IgnoreMethod` dynamic exclusion (L1308-1316)
- `erpc/shadow.go` — shadow execution path that passes `byPassMethodExclusion=true` to `ups.Forward` (L16-180)
- `erpc/networks_forward_test.go` — `UseUpstream` tag-selector + no-match tests (L498-611)
