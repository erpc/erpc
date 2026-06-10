# KB: Static responses

> status: complete
> source-dirs: erpc/networks_static_responses.go, erpc/networks_static_responses_test.go, common/static_response.go, common/static_response_test.go, common/config.go, common/validation.go, common/errors.go, telemetry/metrics.go

## L1 — Capability (CTO view)

Static responses let operators configure per-network, per-(method, params) canned JSON-RPC replies that are served instantly without contacting any upstream. The feature is designed for chains whose genesis block is not block 0 (or similar deviations from client assumptions), where sending a request upstream would return errors or inconsistent data. Matched requests consume zero upstream quota and complete in microseconds.

## L2 — Mechanics (staff-engineer view)

**Position in the forwarding pipeline.** `Network.Forward` checks for static-response matches immediately after extracting the JSON-RPC method — before multiplexing, cache, and upstream selection (`erpc/networks.go:L973-L981`). A hit short-circuits the entire pipeline; neither cache reads nor upstream connections are attempted. A miss falls through to the normal path unmodified.

**Matching algorithm.** `FindStaticResponseMatch` (`common/static_response.go:L14`) iterates the `network.staticResponses` slice in declaration order and returns the first entry whose `method` equals the request's method (exact string match) AND whose `params` deep-equal the request's params. The params comparison (`paramsEqual`/`valueEqual`) is type-tolerant: YAML deserializes integers as `int` while JSON deserializes them as `float64`; both paths are treated as equal when numerically the same. Map keys are compared order-independently. Hex strings like `"0x0"` and `"0x00"` are NOT normalized — case-sensitive exact match only.

**Response construction.** On a hit, `tryServeStaticResponse` (`erpc/networks_static_responses.go:L15`) builds a `JsonRpcResponse` with the inbound request's `id` (not any stored id) and the configured `result` or `error` object, wraps it in a `NormalizedResponse`, and increments the Prometheus counter. The `id` echo is guaranteed: `common.NewJsonRpcResponse(req.ID(), ...)` always uses the inbound id.

**Validation.** `StaticResponseConfig.Validate()` (`common/validation.go:L1278`) enforces: `method` non-empty; `response` non-nil; exactly one of `result` or `error` set; when `error` is set, `message` must be non-empty. Validation runs at startup as part of `NetworkConfig.Validate()` which iterates all entries (`common/validation.go:L1270-L1274`).

**No defaults.** Neither `StaticResponseConfig` nor its sub-structs have any `SetDefaults` wiring — they are fully explicit. The list itself defaults to empty (nil slice).

**Precedence vs cache and upstreams.** Static responses fire BEFORE cache reads. They are also before multiplexer checks. The only things that run before static responses in `Forward` are: method extraction and OTel span setup.

## L3 — Exhaustive reference (agent view)

### Config fields

All fields live under `networks[*].staticResponses[*]` in YAML.

| # | YAML path | Type | Default | Behavior / notes | Source |
|---|-----------|------|---------|------------------|--------|
| 1 | `networks[*].staticResponses` | `[]*StaticResponseConfig` | `nil` (empty list, feature off) | Array of static entries; checked in declaration order; first match wins | `common/config.go:L2005` |
| 2 | `networks[*].staticResponses[*].method` | `string` | required | Exact JSON-RPC method name to match; case-sensitive | `common/config.go:L2015`, `common/validation.go:L1282-L1284` |
| 3 | `networks[*].staticResponses[*].params` | `[]interface{}` | `nil` (nil matches empty params too — see edge cases 3a/3b) | Params to match against inbound JSON-RPC params; supports numbers, strings, booleans, arrays, maps. `params:` omitted → nil; `params: []` → non-nil empty slice. Both have `len()==0` and are interchangeable in matching. YAML tag is `omitempty` so a nil params field is omitted on re-serialization, but an explicit `params: []` survives round-trip as a non-nil empty slice. | `common/config.go:L2016` |
| 4 | `networks[*].staticResponses[*].response` | `*StaticResponseBodyConfig` | required (nil rejected at validation) | Exactly one of `result` or `error` must be set | `common/config.go:L2017`, `common/validation.go:L1285-L1298` |
| 5 | `networks[*].staticResponses[*].response.result` | `interface{}` | `nil` | Any JSON-serializable value; set to serve a successful response; mutually exclusive with `error` | `common/config.go:L2023` |
| 6 | `networks[*].staticResponses[*].response.error` | `*StaticResponseErrorConfig` | `nil` | Set to serve a JSON-RPC error response; mutually exclusive with `result`. When set, must also supply at minimum `message`. Maps to the `StaticResponseErrorConfig` struct (rows 7–9). | `common/config.go:L2024` |
| 7 | `networks[*].staticResponses[*].response.error.code` | `int` | **`0`** (Go zero-value; no minimum or maximum enforced) | JSON-RPC error `code` integer. Typical values: `-32700` parse error, `-32600` invalid request, `-32601` method not found, `-32602` invalid params, `-32603` internal error. The value `0` passes validation but is **omitted from the wire JSON** because `ErrJsonRpcExceptionExternal.Code` uses `json:"code,omitempty"` — `omitempty` on an `int` suppresses the zero value. Set a non-zero code to ensure the `code` field appears in the response. | `common/config.go:L2029`, `common/errors.go:L2226` |
| 8 | `networks[*].staticResponses[*].response.error.message` | `string` | **required** (empty string rejected at startup) | Human-readable error description. Must be non-empty when `error` is set; `Validate()` returns `"response.error.message is required"` otherwise. | `common/config.go:L2030`, `common/validation.go:L1296-L1298` |
| 9 | `networks[*].staticResponses[*].response.error.data` | `interface{}` | **`nil`** (omitted from JSON output when nil due to `omitempty`) | Optional arbitrary extra data in the JSON-RPC error object (the `data` field). Accepts any JSON-serializable value: string, number, object, array. Omitted entirely from the response when nil. | `common/config.go:L2031` |

**Total config fields documented: 9**

### Behaviors & algorithms

**Match algorithm (`FindStaticResponseMatch`)** (`common/static_response.go:L14-L24`):
1. Iterate entries in declaration order.
2. Skip nil entries and entries whose `Method != method` (exact string equality).
3. Call `paramsEqual(entry.Params, requestParams)`.
4. Return the first match, or `nil` if none.

**`paramsEqual` semantics** (`common/static_response.go:L29-L39`):
- Lengths must match.
- Element-wise: `valueEqual(a[i], b[i])`.

**`valueEqual` semantics** (`common/static_response.go:L41-L71`):
- Both nil → true; one nil → false.
- Both numeric (any of float32/64, int/8/16/32/64, uint/8/16/32/64) → compare as float64.
- String → exact string equality (no hex normalization).
- Bool → exact equality.
- `[]interface{}` → recursive `sliceEqual`.
- `map[string]interface{}` → `mapEqual` (key-count check + recursive value comparison, order-independent).
- Anything else → `reflect.DeepEqual`.

**Response shape on a static hit:**
- `jsonrpc`: `"2.0"` (set by `NewJsonRpcResponse`).
- `id`: mirrored from inbound request (numeric or string, whatever the client sent).
- `result` XOR `error`: per configuration; never both.
- No `upstream` tracking metadata (the response's `Upstream()` is nil; `FromCache()` is false).

**Static response does NOT reset the request's span attributes** — the OTel span receives `attribute.Bool("static_response.hit", true)` at `erpc/networks.go:L978`.

**Error path in tryServeStaticResponse:** If `req.JsonRpcRequest()` fails (malformed request), the function returns `(nil, false)` — a debug log is emitted and the request falls through to normal handling; it is not aborted. If `NewJsonRpcResponse` fails (extremely unlikely — only if result/error marshaling panics), the function logs an error and returns `(nil, false)`.

### Observability

| Metric | Type | Labels | Fires when | Source |
|--------|------|--------|-----------|--------|
| `erpc_network_static_response_served_total` | counter | `project`, `network`, `category` | A request matched a static entry and was served without upstream contact | `erpc/networks_static_responses.go:L55-L58`, `telemetry/metrics.go:L419-L423` |

- `category` label = the JSON-RPC method name (e.g. `eth_getBlockByNumber`).
- OTel span attribute `static_response.hit=true` is set on the `Network.Forward` span on a hit (`erpc/networks.go:L978`).
- Debug log: `"served static response (no upstream contacted)"` at `zerolog.DebugLevel`, including `method` field (`erpc/networks_static_responses.go:L60-L63`).
- Debug log on match-check failure: `"skipping static response: cannot inspect request"` at `zerolog.DebugLevel` (`erpc/networks_static_responses.go:L23`).
- Error log: `"failed to build static response"` on `NewJsonRpcResponse` failure (rare) (`erpc/networks_static_responses.go:L48`).

### Edge cases & gotchas

1. **Hex string case-sensitivity.** `"0x0"` and `"0x00"` are different param values. Clients may send either form depending on their SDK. If the config uses `"0x0"` but the client sends `"0x00"`, no match occurs and the request falls through to an upstream. The test `TestParamsEqual_HexStringCaseSensitive` (`common/static_response_test.go:L84`) explicitly documents this — callers must use the exact form their clients send.

2. **YAML int vs JSON float64 tolerance.** YAML config `params: [0, false]` stores `int(0)` after unmarshaling; the inbound JSON request stores `float64(0)`. `toFloat64` normalizes both to `float64(0)` before comparison, so the match succeeds. Confirmed by `TestParamsEqual_NumericEquivalence` (`common/static_response_test.go:L59`).

3a. **nil config params matches empty-array inbound params (and vice-versa).** `paramsEqual` compares only `len(a) != len(b)` as its first gate (`common/static_response.go:L30`). Both nil and `[]interface{}{}` have `len() == 0`, so they are equal in every matching direction:
   - Config `params:` omitted (nil) vs inbound `"params":[]` → match. (`TestFindStaticResponseMatch/"nil slice vs empty slice are equivalent"`, `common/static_response_test.go:L49`, where the config entry uses `Params: []interface{}{}` and the test passes `nil` as the inbound params.)
   - Config `params: []` (explicit empty, non-nil) vs inbound `"params":null` or omitted → match. Same `len==0` logic.
   - Config `params: []` vs inbound `"params":[]` → match. (`TestFindStaticResponseMatch/"hit on empty params"`, `common/static_response_test.go:L44`.)

3b. **`params: []` in YAML produces non-nil empty slice, not nil.** YAML unmarshaling of `params: []` yields `[]interface{}{}` (non-nil, `len==0`), while omitting the field or writing `params:` yields `nil`. This distinction is irrelevant to matching (both produce `len==0`), but matters for: re-serialization (omitempty omits nil fields, not zero-length slices), reflection-based diffing tools, and test assertions that check `== nil` vs `len == 0`. The `omitempty` YAML tag on `Params` means that nil params are omitted from marshaled output, but an explicit `params: []` in YAML config survives deserialization as a non-nil slice. Verified empirically: `yaml.Unmarshal("params: []", &cfg)` → `cfg.Params != nil && len(cfg.Params) == 0`; `yaml.Unmarshal("params:", &cfg)` → `cfg.Params == nil`. (`common/config.go:L2016` tag `yaml:"params,omitempty"`)

4. **Declaration order matters.** First match wins. If two entries match the same (method, params), only the first is served.

5. **No params wildcard.** There is no glob or `*` support — every params slot must match exactly. To catch a method regardless of params, add an entry with an empty `params` array.

6. **Static responses bypass the cache.** A matched static response is never written to cache; the cache is not read. This means even if caching is configured for the same method, a static match always wins and never populates the cache.

7. **Static responses bypass multiplexing.** Multiplexer leader/follower logic runs after static response check; matched requests never enter the mux.

8. **Inbound id is echoed, not a hardcoded one.** The config's `Params` slice might be interpreted as storing an id, but it does not. The id comes from `req.ID()` at response build time (`erpc/networks_static_responses.go:L45`). `StringRequestIdEchoedOnStaticHit` test (`erpc/networks_static_responses_test.go:L190`) confirms string ids work too.

9. **Exactly one of result/error enforced at validation time.** Setting both or neither causes a startup error. `result: null` (YAML null) is treated as non-nil by the Go interface{} check — use `error` for "null result" semantics or set `result` to the intended null JSON value explicitly.

10. **Error code 0 is not rejected at validation but is omitted from the wire response.** `StaticResponseErrorConfig.Code` has no minimum/maximum enforcement; code `0` passes `Validate()`. However, `ErrJsonRpcExceptionExternal.Code` is tagged `json:"code,omitempty"` (`common/errors.go:L2226`), which in Go causes the zero integer value (`0`) to be omitted from JSON serialization. A client receiving the response will see no `code` field in the error object at all. Always set a non-zero error code in production configurations.

### Source map

- `erpc/networks_static_responses.go` — `tryServeStaticResponse`: fetches params, calls `FindStaticResponseMatch`, builds response, emits metric; called by `Network.Forward`.
- `common/static_response.go` — `FindStaticResponseMatch`, `paramsEqual`, `valueEqual`, `mapEqual`, `sliceEqual`, `toFloat64`: pure matching logic with no side effects.
- `common/config.go:L2008-L2032` — `StaticResponseConfig`, `StaticResponseBodyConfig`, `StaticResponseErrorConfig` struct definitions; `NetworkConfig.StaticResponses` field at `L2005`.
- `common/validation.go:L1270-L1300` — `NetworkConfig` iterates entries calling `Validate()`; `StaticResponseConfig.Validate()` enforces field requirements.
- `erpc/networks.go:L973-L981` — call site in `Network.Forward`; placement relative to multiplexer and cache.
- `telemetry/metrics.go:L419-L423` — `MetricNetworkStaticResponseServedTotal` counter definition.
- `common/static_response_test.go` — unit tests for matching logic (numeric equivalence, hex case, nil/empty params, map ordering, validation rules).
- `erpc/networks_static_responses_test.go` — integration tests: upstream not contacted on match, params fall-through, error-shaped stubs, string id echo.
- `common/errors.go:L2225-L2231` — `ErrJsonRpcExceptionExternal` struct; `Code` field uses `json:"code,omitempty"` causing zero code to be omitted from wire JSON.
