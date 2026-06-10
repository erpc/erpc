# KB: Caching: policies, TTLs, finality, compression, freshness

> status: complete
> source-dirs: data/cache_policy.go, data/cache_executor.go, architecture/evm/json_rpc_cache.go, common/cache_dal.go, common/data.go, common/config.go, common/defaults.go, common/request.go, common/json_rpc.go, data/connector.go, data/failsafe.go, data/grpc.go, telemetry/metrics.go, util/bytes.go, util/string.go, erpc/evm_json_rpc_cache_fanout_test.go, architecture/evm/json_rpc_cache_age_test.go, architecture/evm/json_rpc_cache_connector_freshness_test.go, data/memory_compression_test.go

## L1 — Capability (CTO view)

eRPC's EVM JSON-RPC cache layer intercepts every request before it reaches upstreams and stores responses behind a configurable set of policies. Each policy specifies which network/method/params/finality bucket it covers, which storage connector backs it, what TTL to enforce, and how to handle empty results. On reads the cache fans out in parallel across all matching connectors; the first acceptable hit cancels peers. On writes every matching policy receives the response concurrently. Compression (zstd) is transparent and on by default. A realtime freshness gate rejects stale cached results for time-sensitive methods before they are served, preventing thundering-herd situations from ever serving stale data. Callers can bypass cache reads per-connector using HTTP headers or query parameters.

## L2 — Mechanics (staff-engineer view)

**Policy matching pipeline.** The `EvmJsonRpcCache` holds an ordered slice of `CachePolicy` objects built at startup from `evmJsonRpcCache.policies`. Every incoming request passes through `findGetPolicies` (for reads) or `findSetPolicies` (for writes). Both walk the slice sequentially and test each policy's `network` (wildcard), `method` (wildcard), `params` (structural match), and `finality` fields against the request. For **Get** the finality matching is asymmetric: a finalized request also matches unfinalized policies (because a block finalized after write was stored under the unfinalized policy), a realtime request matches only realtime policies, and an unknown-finality request matches all policies — the first connector with data wins. For **Set** the finality must match exactly. `findGetPolicies` additionally deduplicates by connector so each connector is probed at most once per request (`architecture/evm/json_rpc_cache.go:L951-L978`).

**Cache key derivation.** Both Get and Set compute two keys: a partition key (`{networkId}:{blockRef}`) and a range key (`{method}:{sha256(params)}`). The block reference comes from `ExtractBlockReferenceFromRequest`; if it resolves to `*` (a wildcard tag like "latest") the connector is queried via the reverse index (`ConnectorReverseIndex = "idx_reverse"`) rather than the main index. If `blockRef == ""` (method not recognized/not cacheable), the whole cache lookup is skipped silently (`architecture/evm/json_rpc_cache.go:L984-L996`, `L642-L658`).

**Get fan-out.** `Get` spawns one goroutine per matching policy. All goroutines race under a shared `fanCtx`. The first goroutine to find an acceptable non-empty hit calls `cancelFan()` to signal peers, which observe cancellation and exit without posting to metrics. Results travel through a buffered `chan fanResult` sized to the number of spawned goroutines so no goroutine ever blocks after fanCtx is done. The consumer drain loop breaks as soon as `jrr != nil` — a slow peer that refuses to honour cancellation never pins the user-visible latency (`erpc/evm_json_rpc_cache_fanout_test.go:TestEvmJsonRpcCache_FanOut_SlowPeerDoesNotBlockFastWinner`). A 30-second backstop context wraps the fan-out when the parent has no deadline, protecting against FD leaks from misbehaving connectors (`architecture/evm/json_rpc_cache.go:L220-L225`).

**Set fan-out.** `Set` also fans out, but using `sync.WaitGroup` (fire-and-forget per policy, then `wg.Wait()`). There is a hard 5-second write timeout per connector (`architecture/evm/json_rpc_cache.go:L768`). All connector errors are collected and returned as a composite error, but a single-policy error is returned unwrapped. Responses with a JSON-RPC error field are never cached (`shouldCacheResponse:L1062`). Empty results for future/not-yet-produced blocks are never cached regardless of policy (`shouldCacheResponse:L1079` + `emptyResultBeyondConfidence`).

**Realtime freshness gate.** After a cache hit is retrieved, `shouldAcceptCachedResult` is called for every result whose finality is `realtime`. It compares the block's unix timestamp (extracted from the response via `ExtractBlockTimestampFromResponse`) against the policy TTL. If the response carries no block timestamp (eth_blockNumber, eth_gasPrice, eth_getLogs), it falls back to the connector's own latest-block timestamp via the `CacheHeadReporter` interface. If neither source is available, the result is accepted (fail-open). When age > TTL, the hit is reclassified as `ttl_rejected` and the fan-out loop continues looking at peer connectors. Non-realtime data (finalized, unfinalized, unknown) is never age-gated — a 2022 finalized block is still valid today (`architecture/evm/json_rpc_cache.go:L841-L920`).

**CacheHeadReporter.** Only `GrpcConnector` currently implements this interface. Its background block-head poller runs every 60 seconds and populates `latestTsByNetwork`. `FailsafeConnector` transparently forwards the call to the wrapped connector when it is head-aware (`data/failsafe.go:L209-L215`).

**Empty-result behavior.** The `empty` field on each policy controls three modes:
- `ignore` (iota=0, **default**): skip caching empty results on write; treat cached empty results as a miss on read.
- `allow` (iota=1): cache and serve empty results like any other result.
- `only` (iota=2): cache/serve ONLY empty results; skip non-empty.

An empty response is defined by `IsBytesEmptyish`: the result field is `null`, `""`, `"0x"`, `"0x0"`, `0`, `[]`, `{}`, or a hex string that is all-zeros after stripping the `0x` prefix (`util/bytes.go:L22-L57`).

**Compression.** When `evmJsonRpcCache.compression.enabled = true` (default), values are compressed with zstd before storage. Only values whose **pre-compression** size meets or exceeds `threshold` (default 1024 bytes) are compressed; smaller values are stored raw. If the compressed output is larger than the original it is also stored raw. Detection on read is by the zstd magic bytes `0x28 0xB5 0x2F 0xFD` (`architecture/evm/json_rpc_cache.go:L1151-L1157`). Encoder and decoder are pooled with `sync.Pool` for thread-safety. The memory connector has its own internal zstd compression for the in-process store; the `EvmJsonRpcCache` layer adds a second, independent compression pass before calling any connector's `Set`.

**gRPC connector (BDS read-through).** `GrpcConnector` is a read-only connector backed by BDS gRPC servers (blockchain-data-standards). It does not support `Set`. Supported methods are `eth_getBlockByNumber`, `eth_getBlockByHash`, `eth_getLogs`, `eth_getTransactionByHash`, `eth_getTransactionReceipt`, `eth_getBlockReceipts`, `eth_chainId`, and `eth_blockNumber`. `eth_blockNumber` has no native BDS call; the connector derives it by fetching the latest block and returning only its hex number (`data/grpc.go:L270-L278`). The gRPC connector performs a fast-miss check: if the request block number is below the earliest block the gRPC server has (`earliestByNetwork`), it returns `ErrRecordNotFound` immediately without a network call.

**Skip-cache-read directive.** Callers can set `X-ERPC-Skip-Cache-Read` header or `skip-cache-read` query parameter to bypass specific connectors by pattern (wildcard match against connector ID) or to skip all cache reads (`"true"`). The value is normalized to string on parse. `ShouldSkipCacheRead("")` (empty connectorId) returns false for pattern values — it is only evaluated when a specific connector is being considered (`common/request.go:L895-L914`).

**Connector failsafe wrapping.** When a connector config lists `failsafeForGets` or `failsafeForSets`, `NewConnector` wraps the raw connector in a `FailsafeConnector`. The `cacheExecutor` within applies retry, hedge (with static delay), and circuit-breaker per-call. Consensus is not supported at this scope (returns configuration error). Hedge quantile mode is also unsupported (no latency tracker at this scope) (`data/cache_executor.go:L31-L38`).

## L3 — Exhaustive reference (agent view)

### Config fields

All fields are under `projects[*].evmJsonRpcCache`. `Duration` values accept Go duration strings or bare integers (milliseconds).

#### `database.evmJsonRpcCache` (root field)

| YAML path | Type | Default | Behavior |
|-----------|------|---------|----------|
| `database.evmJsonRpcCache` | `*CacheConfig` | `nil` | **When nil (omitted): cache is entirely disabled.** `DatabaseConfig.SetDefaults` only calls `EvmJsonRpcCache.SetDefaults()` when the pointer is non-nil (`common/defaults.go:L831-L836`). Every sub-field documented below (policies, connectors, compression) is only initialized when this top-level object exists. Omitting the block in YAML produces a nil pointer and no caching occurs — requests always hit upstreams. **Auto-generated project note:** when no `projects:` are defined in the root config, eRPC auto-generates a project with `id: "main"` at `common/defaults.go:L100-L165`. This auto-project has retry, timeout, hedge, and integrity defaults injected — but it has **NO** `evmJsonRpcCache` config. The auto-project's cache pointer is nil and caching is disabled unless the user explicitly provides `database.evmJsonRpcCache:` in their config. The presence of an auto-project does not imply a default cache configuration. |

#### `evmJsonRpcCache.compression`

**Auto-created even when omitted.** `CacheConfig.SetDefaults` always creates a `&CompressionConfig{}` and calls its `SetDefaults()` when `c.Compression == nil` (`common/defaults.go:L482-L488`). This means a user who omits the entire `compression:` block still gets zstd compression enabled with threshold=1024, level=fastest. You must explicitly set `compression.enabled: false` to opt out.

| YAML path | Type | Default | Behavior |
|-----------|------|---------|----------|
| `compression.enabled` | *bool | `true` — `SetDefaults` sets `*bool(true)` when nil (`common/defaults.go:L580-L582`) | Enable zstd compression for cache values. When false, all values stored raw. |
| `compression.algorithm` | string | `"zstd"` (`common/defaults.go:L585-L587`) | Only `"zstd"` is implemented; field reserved for future algorithms. |
| `compression.zstdLevel` | string | `"fastest"` (`common/defaults.go:L590-L592`) | Valid values: `"fastest"`, `"default"`, `"better"`, `"best"`. Maps to `zstd.SpeedFastest` / `SpeedDefault` / `SpeedBetterCompression` / `SpeedBestCompression`. Unknown values fall back to `fastest` with a warning (`architecture/evm/json_rpc_cache.go:L84-L96`). |
| `compression.threshold` | int (bytes) | `1024` (`common/defaults.go:L597-L599`) | Minimum value size in bytes to attempt compression. Values smaller than this are stored raw. If the compressed output is larger than original it is also stored raw (`architecture/evm/json_rpc_cache.go:L1113-L1147`). Note: the inline comment in `NewEvmJsonRpcCache` shows the code default as 512 (`json_rpc_cache.go:L78-L79`) but `SetDefaults` overrides with 1024; `SetDefaults` wins at runtime. |

#### `evmJsonRpcCache.connectors[*]`

`ConnectorConfig.SetDefaults` is called with `scope = connectorScopeCache` (`common/defaults.go:L474-L480`). This scope drives the following automatic defaults that differ from other scopes:

- **`id`**: if blank, auto-set to `"cache-" + driver` (e.g. `"cache-postgresql"`) (`common/defaults.go:L847-L849`)
- **`postgresql.table`**: if blank, auto-set to `"erpc_json_rpc_cache"` (scope `connectorScopeCache` branch at `common/defaults.go:L1027`)
- **`postgresql.minConns`**: if 0, auto-set to `4` (non-auth scope default, `common/defaults.go:L1034-L1040`)
- **`postgresql.maxConns`**: if 0, auto-set to `32` (non-auth scope default, `common/defaults.go:L1041-L1047`)

| YAML path | Type | Default | Behavior |
|-----------|------|---------|----------|
| `connectors[*].id` | string | `"cache-<driver>"` (auto-derived when blank) | Unique connector ID referenced by policies. Auto-set to `"cache-" + driver` string if omitted (`common/defaults.go:L847-L849`). |
| `connectors[*].driver` | string | required | One of `memory`, `redis`, `dynamodb`, `postgresql`, `grpc`. |
| `connectors[*].memory.maxItems` | int | required | Max items in Ristretto. |
| `connectors[*].memory.maxTotalSize` | string | required | Max total memory, e.g. `"1GB"`. |
| `connectors[*].memory.emitMetrics` | *bool | nil (treated as false by Ristretto) | Emit Ristretto cost/set-failure metrics. |
| `connectors[*].redis.addr` | string | `""` | Redis `host:port`. Mutually exclusive with `uri`. |
| `connectors[*].redis.uri` | string | `""` | Full Redis URI. Preferred over `addr`. |
| `connectors[*].redis.password` | string | `""` | Redacted in JSON/YAML marshal. |
| `connectors[*].redis.db` | int | `0` | Redis DB index. |
| `connectors[*].redis.connPoolSize` | int | `0` (driver default) | Connection pool size. |
| `connectors[*].redis.initTimeout` | Duration | driver default | Timeout for initial connection. |
| `connectors[*].redis.getTimeout` | Duration | driver default | Per-GET timeout. |
| `connectors[*].redis.setTimeout` | Duration | driver default | Per-SET timeout. |
| `connectors[*].redis.lockRetryInterval` | Duration | driver default | Interval for distributed lock retries. |
| `connectors[*].redis.tls` | *TLSConfig | nil | Optional TLS for Redis. |
| `connectors[*].dynamodb.table` | string | required | DynamoDB table name. |
| `connectors[*].dynamodb.region` | string | required | AWS region. |
| `connectors[*].dynamodb.partitionKeyName` | string | required | Partition key attribute name. |
| `connectors[*].dynamodb.rangeKeyName` | string | required | Range key attribute name. |
| `connectors[*].dynamodb.reverseIndexName` | string | required | GSI name for reverse index (wildcard block lookups). |
| `connectors[*].dynamodb.ttlAttributeName` | string | required | DynamoDB TTL attribute name. |
| `connectors[*].dynamodb.initTimeout` | Duration | driver default | Init timeout. |
| `connectors[*].dynamodb.getTimeout` | Duration | driver default | Per-GET timeout. |
| `connectors[*].dynamodb.setTimeout` | Duration | driver default | Per-SET timeout. |
| `connectors[*].dynamodb.maxRetries` | int | `0` (driver default) | AWS SDK max retries. |
| `connectors[*].dynamodb.statePollInterval` | Duration | driver default | Interval for state poller. |
| `connectors[*].dynamodb.lockRetryInterval` | Duration | driver default | Distributed lock retry interval. |
| `connectors[*].postgresql.connectionUri` | string | required | PostgreSQL connection URI. Redacted in marshal. |
| `connectors[*].postgresql.table` | string | `"erpc_json_rpc_cache"` (cache scope) | Table name. Auto-set to `"erpc_json_rpc_cache"` when scope=cache and value is blank (`common/defaults.go:L1027`). |
| `connectors[*].postgresql.minConns` | int32 | `4` (cache scope) | Min pool connections. Auto-set to 4 for non-auth scopes including cache (`common/defaults.go:L1034-L1040`). |
| `connectors[*].postgresql.maxConns` | int32 | `32` (cache scope) | Max pool connections. Auto-set to 32 for non-auth scopes including cache (`common/defaults.go:L1041-L1047`). |
| `connectors[*].grpc.bootstrap` | string | `""` | HTTP URL to fetch gRPC server list from. |
| `connectors[*].grpc.servers` | []string | nil | Explicit gRPC server URLs. If both `bootstrap` and `servers` provided, explicit list takes precedence and bootstrap is also resolved and appended (`data/grpc.go:L85-L95`). |
| `connectors[*].grpc.headers` | map[string]string | nil | Headers sent with every gRPC request. |
| `connectors[*].grpc.getTimeout` | Duration | **`100ms`** — `ConnectorConfig.SetDefaults` sets `c.Grpc.GetTimeout = Duration(100 * time.Millisecond)` when zero (`common/defaults.go:L927-L929`). | Per-call timeout for gRPC gets. Applied when the existing context deadline is absent or farther in the future than `getTimeout` (`data/grpc.go:L258-L261`). Note: an earlier version of this file incorrectly stated `0 (no override)` — the correct runtime default is 100ms. |
| `connectors[*].failsafeForGets` | []*FailsafeConfig | nil | Per-connector failsafe configuration for Get operations. Consensus and hedge-quantile are not supported (`data/cache_executor.go:L31-L38`). |
| `connectors[*].failsafeForSets` | []*FailsafeConfig | nil | Per-connector failsafe configuration for Set operations. Same restrictions as Gets. |

#### `evmJsonRpcCache.policies[*]`

| YAML path | Type | Default | Behavior |
|-----------|------|---------|----------|
| `policies[*].connector` | string | required | ID of the connector to use; must match a connector in `connectors[*].id`. |
| `policies[*].network` | string | `"*"` (`common/defaults.go:L608-L610`) | Wildcard/glob network filter; matched against request `networkId`. |
| `policies[*].method` | string | `"*"` (`common/defaults.go:L605-L607`) | Wildcard/glob method filter. |
| `policies[*].params` | []interface{} | nil | Positional parameter pattern; nil means match any params. See param matching section below. |
| `policies[*].finality` | DataFinalityState | `finalized` (zero value of the iota) | One of `finalized`, `unfinalized`, `realtime`, `unknown`. Controls which finality bucket this policy stores and serves. **Zero-value trap**: omitting this field is indistinguishable from `finality: finalized`. A policy without an explicit `finality` will ONLY match requests whose resolved finality is `finalized` (or `unknown`). Methods whose block tag resolves to `realtime` — `eth_blockNumber`, `eth_call` with `"latest"`, `eth_gasPrice`, etc. — will never match this policy and will always hit upstreams. To cache realtime data you MUST add a separate policy (or duplicate connector entry) with `finality: realtime`. See edge case #1 and edge case #20. |
| `policies[*].empty` | CacheEmptyBehavior | `ignore` (zero value of the iota) | One of `ignore`, `allow`, `only`. |
| `policies[*].appliesTo` | CachePolicyAppliesTo | `"both"` (`common/defaults.go:L611-L613`) | One of `"both"`, `"get"`, `"set"`. Restricts whether the policy is evaluated for reads, writes, or both. |
| `policies[*].minItemSize` | *string | nil (no min) | ByteSize string (e.g. `"512B"`, `"1KB"`, `"2MB"`). Parsed by `util.ParseByteSize` (`util/string.go:L25-L54`). Supported suffixes: `B` (bytes, 1×), `KB` (kibibytes, 1024×), `MB` (mebibytes, 1048576×). Case-insensitive (normalized to uppercase). The measured size is the **pre-compression length of the JSON-RPC `result` field bytes** (`len(r.result)`) as returned by `JsonRpcResponse.ResultLength()` (`common/json_rpc.go:L77-L91`). This is the raw result bytes before any zstd pass — neither the whole response envelope nor the post-compression value. When a response is filtered out by this limit, caching is silently skipped (`shouldCacheResponse` returns false) and the request proceeds to upstream normally — there is NO error and NO metric other than `erpc_cache_set_skipped_total` (`architecture/evm/json_rpc_cache.go:L1069-L1072`). |
| `policies[*].maxItemSize` | *string | nil (no max) | ByteSize string; same format and measurement as `minItemSize`. Responses whose result byte length exceeds this are not cached. Filter-out behavior is identical: silent skip, request falls through to upstream. `MatchesSizeLimits` returns false when `size > maxSize` (`data/cache_policy.go:L152-L160`). |
| `policies[*].ttl` | Duration | nil / 0 (unlimited) | Time-to-live stored with each key. When set, also used as the freshness window for realtime age-gating. |

#### `networks[*].methods.definitions.<method>.reqRefs` / `respRefs`

These fields on `CacheMethodConfig` (`common/config.go:L302-L303`) tell the block-reference extraction logic **where in a JSON-RPC request or response** to look for a block number, tag, or hash. They are the mechanism by which eRPC decides (a) whether a request/response is cacheable at all and (b) what cache key partition to use.

| YAML path | Type | Default | Behavior |
|-----------|------|---------|----------|
| `networks[*].methods.definitions.<method>.reqRefs` | `[][]interface{}` | built-in per method in `defaults.go` | Array of selector paths. Each inner array is a navigation path through JSON-RPC `params`. |
| `networks[*].methods.definitions.<method>.respRefs` | `[][]interface{}` | built-in per method in `defaults.go` | Array of selector paths into the JSON-RPC `result` object of the response. Used to resolve a block reference when the request alone is insufficient (e.g. tx-hash lookups). |

**Path format.** Each selector path is an `[]interface{}` where each element is either:
- An **integer** (`int`/`float64`): index into a JSON array (e.g. `0` = first param)
- A **string**: key into a JSON object (e.g. `"blockNumber"` accesses `.blockNumber`)
- An **empty path** (`[]`): means the value at that position IS the scalar (used for `eth_blockNumber` where the result itself is the hex block number)
- The **special single-element path `["*"]`**: signals "arbitrary block" — the value is cacheable regardless of block number (used for tx-hash lookups like `eth_getTransactionByHash`). This produces `blockRef = "*"` which routes to the reverse index.

**reqRefs extraction algorithm** (`architecture/evm/block_ref.go:L277-L310`):
1. If `reqRefs` has exactly one path of length 1 and that element is `"*"`: return `blockRef="*"` immediately (arbitrary-block short-circuit).
2. Otherwise, for each path, call `r.PeekByPath(path...)` on the request params. Parse the value as a block parameter (hex number, decimal, or named tag like `"latest"`).
3. If multiple paths yield different block references (e.g. `eth_getLogs` with different `fromBlock`/`toBlock`): set `blockRef = "*"` (composite range — cannot key on a single block).
4. Accumulate the highest block number seen across all paths.
5. If no path resolves: `blockRef = ""` → cache bypassed entirely.

**respRefs extraction algorithm** (`architecture/evm/block_ref.go:L343-L368`):
1. Called only after a response exists. For each path, call `rpcResp.PeekStringByPath(path...)` on the result.
2. First non-`"*"` block ref wins; block number takes the maximum.
3. If only a block number was found (no specific ref), format it as decimal string.
4. Result supplements or overrides the request-derived ref for the cache key.

**Built-in defaults** are defined in `common/defaults.go` as reusable path variables:
- `FirstParam = [[0]]` — first request param is the block ref
- `SecondParam = [[1]]` — second request param is the block ref
- `ThirdParam = [[2]]`
- `NumberOrHashParam = [["number"], ["hash"]]` — response object fields
- `BlockNumberOrBlockHashParam = [["blockNumber"], ["blockHash"]]`
- `ArbitraryBlock = [["*"]]` — tx-hash methods (`common/defaults.go:L234-L256`)

**Custom method example** (YAML):
```yaml
networks:
  - methods:
      definitions:
        my_custom_method:
          reqRefs:
            - [1]          # second param is the block number
          respRefs:
            - ["blockHash"] # response result has a blockHash field
```

**Interaction with cache.** `ExtractBlockReferenceFromRequest` uses `reqRefs`; when both request and response are available, `ExtractBlockReferenceFromResponse` uses `respRefs` to refine the key. If both return `""`, caching is bypassed with a silent skip (no error, no metric). If `blockRef = "*"`, the connector is queried via `ConnectorReverseIndex` (`"idx_reverse"`) instead of the main index (`architecture/evm/json_rpc_cache.go:L1013-L1017`).

### Behaviors & algorithms

#### Finality state derivation and policy matching rules

The `DataFinalityState` enum (`common/data.go:L9-L27`):
- `finalized` = 0 (zero value / default)
- `unfinalized` = 1
- `realtime` = 2
- `unknown` = 3

**Get matching (`MatchesForGet`):**
- If request finality = `finalized`: policy finality must be `finalized` OR `unfinalized` (a block finalized after write was stored unfinalized)
- If request finality = `unfinalized`: policy finality must be `unfinalized` only
- If request finality = `realtime`: policy finality must be `realtime` only
- If request finality = `unknown`: any policy matches (first connector wins)
- `appliesTo` filter applied first: if `"set"`, policy is skipped for get

**Set matching (`MatchesForSet`):**
- Finality must match exactly (`policy.Finality == finality`)
- `empty` filter: if `CacheEmptyBehaviorIgnore` and `isEmptyish = true`, skip; if `CacheEmptyBehaviorOnly` and `isEmptyish = false`, skip
- `appliesTo` filter: if `"get"`, policy is skipped for set

#### Param pattern matching

When `params` is non-nil, each element is matched positionally against the request params (`data/cache_policy.go:L166-L184`):
- **string**: wildcard-matched via `common.WildcardMatch` (glob + boolean operators)
- **object** (`map[string]interface{}`): recursive key-by-key match; all specified keys must match
- **array** (`[]interface{}`): all elements must match by index; lengths must be equal
- **other types**: compared as strings via `paramToString`
- `nil` in pattern at position `i` matches any value or absent value
- Extra params beyond the pattern length are ignored

#### Cache key structure

- Partition key: `{networkId}:{blockRef}` where `blockRef` is a concrete hex block number or `*` for tags, or the method name for static methods
- Range key: `{method}:{hex(sha256(params))}`
- When `blockRef = "*"` (tag-based lookup): connector is queried via `ConnectorReverseIndex` (`"idx_reverse"`) instead of `ConnectorMainIndex` (`"idx_main"`) (`architecture/evm/json_rpc_cache.go:L1013-L1017`)
- When `blockRef = ""` (no block reference extractable): cache is bypassed entirely — no read and no write

#### shouldCacheResponse rules (Set-time, per-policy)

1. Response has JSON-RPC error field → skip (`json_rpc_cache.go:L1062-L1065`)
2. Response size outside `[minItemSize, maxItemSize]` → skip (`L1067-L1070`)
3. Response is empty AND request targets a block beyond the network's confidence head → skip, regardless of `empty` setting (`L1079-L1082`)
4. `empty=ignore` and response is emptyish → skip (`L1084-L1085`)
5. `empty=allow` → always cache (`L1086-L1087`)
6. `empty=only` → cache only if emptyish (`L1088-L1089`)

#### Realtime age guard (Get-time)

Applied only when `req.Finality == DataFinalityStateRealtime` and policy TTL > 0 (`architecture/evm/json_rpc_cache.go:L841-L920`).

**Critical distinction: this is BLOCK age, not cache-entry wall-clock age.** The guard compares the **block's unix timestamp** (seconds since epoch) against `time.Now().Unix()`. It does NOT check when the cache entry was written or how long it has been stored. A response cached 5 minutes ago is accepted if its block was produced only 3 seconds ago; a response cached 30 seconds ago is rejected if its block is 2 minutes old and TTL is 60s.

For responses that carry no block timestamp (e.g. `eth_blockNumber` returns a scalar hex, `eth_gasPrice` returns a scalar, `eth_getLogs` returns an array without a `.timestamp`), the guard falls back to the serving connector's **reported latest-block timestamp** via `CacheHeadReporter.CacheLatestBlockTimestamp(networkId)`. This means for these methods, the guard is checking the age of the connector's most recently known block head, not the cached value itself.

Step-by-step:
1. If `req.Finality != realtime`: skip guard entirely — finalized/unfinalized/unknown data is immutable (`architecture/evm/json_rpc_cache.go:L847-L854`)
2. If policy TTL == 0 or nil: skip guard (no TTL = no age limit, `L856-L859`)
3. Try `ExtractBlockTimestampFromResponse` — reads `result.timestamp` hex value from the cached response body
4. If no timestamp in response: try `connector.(CacheHeadReporter).CacheLatestBlockTimestamp(networkId)` — gRPC connector implements this interface and polls every 60s; FailsafeConnector forwards transparently
5. If still no timestamp available: **fail-open** (accept the cached result, `L879-L889`)
6. `age = time.Duration(time.Now().Unix() - blockTimestamp) * time.Second`
7. If `age > TTL`: reject, increment `erpc_cache_get_age_guard_reject_total`, fan-out loop continues to peer connectors
8. If `age <= TTL`: accept

The response-based timestamp is preferred over the connector head timestamp: a response with a fresh block timestamp is served even if the connector's polled head looks stale (`json_rpc_cache_connector_freshness_test.go:ResponseTimestampPreferredOverConnector`). This is critical: the gRPC connector polls every 60s so its `CacheLatestBlockTimestamp` can be up to 60s stale, but responses containing explicit block timestamps (e.g. `eth_getBlockByNumber`) are always evaluated against their own fresh timestamp.

#### Empty-result definition (`util/bytes.go:L22-L57`)

A result is "emptyish" if the raw JSON bytes are any of: `null`, `""`, `"0x"`, `"0x0"`, `0`, `[]`, `{}`, an empty byte slice, or a hex string whose non-prefix digits are all `0` (i.e. `"0x000…"`).

#### Set write timeout

A hard 5-second `context.WithTimeoutCause` is applied to every connector `Set` call, independent of any failsafe configuration (`architecture/evm/json_rpc_cache.go:L768`). Failsafe timeouts on the connector are additional and shorter (they fire first).

#### skip-cache-read directive matching

`ShouldSkipCacheRead(connectorId string) bool` (`common/request.go:L898-L914`):
- Value = `""` or `"false"` (case-insensitive) → false (normal)
- Value = `"true"` (case-insensitive) → true for any connector
- Other value: wildcard-matched against `connectorId`; passing `""` for connectorId always returns false for non-boolean values

#### gRPC connector eth_blockNumber special handling

`eth_blockNumber` has no native BDS gRPC method. The gRPC connector calls `GetBlockByNumber("latest")` internally and returns only the block number formatted as `"0x{hex}"`. The response passes through the normal realtime freshness gate, which uses the connector's `CacheLatestBlockTimestamp` as the fallback age source when the returned scalar has no block timestamp (`data/grpc.go:L266-L278`).

#### CacheHash (range key generation)

`JsonRpcRequest.CacheHash` (`common/json_rpc.go:L1385-L1409`): hashes params positionally using SHA-256 over `hashValue` (recursive type-aware), then produces `{method}:{hex(sha256(sum))}`. The hash is memoized on the request object.

### Observability

#### Prometheus counters (all under `erpc_` namespace)

| Metric | Labels | When |
|--------|--------|------|
| `erpc_cache_get_skipped_total` | project, network, category | No matching policy found for request |
| `erpc_cache_get_success_hit_total` | project, network, category, connector, policy, ttl | Cache hit returned to caller |
| `erpc_cache_get_success_miss_total` | project, network, category, connector, policy, ttl | All connectors confirmed miss |
| `erpc_cache_get_error_total` | project, network, category, connector, policy, ttl, error | Connector returned transport/non-semantic error |
| `erpc_cache_get_age_guard_reject_total` | project, network, **method**, connector, policy, ttl | Result rejected because **block timestamp age** (not wall-clock entry age) exceeded policy TTL. Only incremented for `realtime` finality requests with a non-zero TTL. Note: label is `method` not `category` here — unique among cache metrics (`architecture/evm/json_rpc_cache.go:L910-L917`). |
| `erpc_cache_set_success_total` | project, network, category, connector, policy, ttl | Connector Set succeeded |
| `erpc_cache_set_error_total` | project, network, category, connector, policy, ttl, error | Connector Set failed |
| `erpc_cache_set_skipped_total` | project, network, category, connector, policy, ttl | shouldCacheResponse returned false (not an error) |
| `erpc_cache_set_original_bytes_total` | project, network, category, connector, policy, ttl | **Counter** (not gauge). Cumulative bytes of the uncompressed value before any compression attempt. Emitted unconditionally for every successful set call (`architecture/evm/json_rpc_cache.go:L730-L743`). |
| `erpc_cache_set_compressed_bytes_total` | project, network, category, connector, policy, ttl | **Counter** (not gauge). Cumulative bytes of the value **after** zstd compression. **Only emitted when compression was actually applied** — i.e. `compressionEnabled && len(value) >= compressionThreshold && compressed_output < original` (`architecture/evm/json_rpc_cache.go:L745-L763`). When compression is disabled or the value is below the threshold, this counter is NOT incremented. When compression would produce a larger output, it is also NOT incremented (the raw value is stored instead). To compute compression ratio: `original_bytes / compressed_bytes`. When `compressed_bytes == 0` for a time window, compression was not applied to any stored value in that window. |
| `erpc_ristretto_cache_current_cost` | connector | Memory connector current cost (bytes), polled |
| `erpc_ristretto_cache_sets_failed_total` | connector | Ristretto dropped/rejected a set |

#### Prometheus histograms

| Metric | Labels |
|--------|--------|
| `erpc_cache_get_success_hit_duration_seconds` | project, network, category, connector, policy, ttl |
| `erpc_cache_get_success_miss_duration_seconds` | project, network, category, connector, policy, ttl |
| `erpc_cache_get_error_duration_seconds` | project, network, category, connector, policy, ttl, error |
| `erpc_cache_set_success_duration_seconds` | project, network, category, connector, policy, ttl |
| `erpc_cache_set_error_duration_seconds` | project, network, category, connector, policy, ttl, error |

#### Prometheus gauges (gRPC connector block head)

| Metric | Labels | Refresh |
|--------|--------|---------|
| `erpc_cache_connector_earliest_block_number` | connector, network | every 60s |
| `erpc_cache_connector_latest_block_number` | connector, network | every 60s |
| `erpc_cache_connector_finalized_block_number` | connector, network | every 60s |
| `erpc_cache_connector_earliest_block_timestamp_seconds` | connector, network | every 60s |
| `erpc_cache_connector_latest_block_timestamp_seconds` | connector, network | every 60s |
| `erpc_cache_connector_finalized_block_timestamp_seconds` | connector, network | every 60s |

#### OTel trace spans

| Span name | Level | Source |
|-----------|-------|--------|
| `Cache.Get` | normal | `architecture/evm/json_rpc_cache.go:L153` |
| `Cache.FindGetPolicies` | detail | `architecture/evm/json_rpc_cache.go:L173` |
| `Cache.GetForPolicy` | detail | `architecture/evm/json_rpc_cache.go:L241` |
| `Cache.Set` | normal | `architecture/evm/json_rpc_cache.go:L572` |
| `Evm.ExtractBlockReferenceFromRequest` | detail | `architecture/evm/block_ref.go:L18` |
| `Evm.ExtractBlockTimestampFromResponse` | detail | `architecture/evm/block_ref.go:L190` |
| `Request.GenerateCacheHash` | detail | `common/json_rpc.go:L1387` |

Span attributes on `Cache.Get`: `network.id`, `request.method`, `request.finality`, `cache.policies_matched`, `cache.hit` (bool), `cache.miss_reason`, `cache.miss_connector_id`, `cache.miss_policy`.
Span attributes on `Cache.GetForPolicy`: `cache.policy_summary`, `cache.connector_id`, `cache.method`, `cache.get_outcome` (`found`/`miss`/`ttl_rejected`/`empty_ignored`/`error`/`cancelled`), `cache.block_ref`, `cache.group_key`, `cache.request_key`.

#### Log messages

- `DEBUG "will not cache the response because we cannot resolve a block reference"` — blockRef is empty on Set
- `DEBUG "skip caching because response contains an error"` — shouldCacheResponse skipped due to error field
- `DEBUG "skip caching because response size does not match policy limits"` — size outside minItemSize/maxItemSize
- `DEBUG "skip caching empty result for a not-yet-produced (future) block"` — emptyResultBeyondConfidence
- `DEBUG "rejecting cached result because block age exceeds policy TTL"` — age guard rejection
- `DEBUG "cached result rejected due to age exceeding TTL"` — same, at Get fan-out level
- `DEBUG "cache connector errored during GET"` — connector transport error
- `DEBUG "skipping cache connector due to skip-cache-read directive pattern"` — SkipCacheRead directive matched
- `DEBUG "returning cached response"` — hit served (Trace level includes raw result)
- `DEBUG "compressed cache value"` — compression applied (with original/compressed/savings)
- `DEBUG "decompressed cache value"` — decompression applied on read

### Edge cases & gotchas

1. **Finality=finalized is the zero value — intentionally, as the safest cache default.** If a policy YAML omits `finality`, it defaults to `DataFinalityStateFinalized` (iota 0). The Go comment at `common/data.go:L10-L15` explicitly states: *"Finalized gets 0 intentionally so that when user has not specified finality, it defaults to finalized, which is safest sane default for caching."* Finalized data is immutable — serving a stale finalized block response is always correct. If you omit `finality`, you will only cache/serve finalized data; unfinalized, realtime, and unknown-finality responses will miss this policy and hit upstreams. A policy set intended to cache all traffic types must define separate policies for each finality bucket, or duplicate the connector reference four times.

2. **findGetPolicies deduplicates by connector.** Multiple policies pointing to the same connector object produce only one fan-out goroutine for Get. For Set, all matching policies write concurrently — so a connector can receive multiple writes for the same request if multiple policies point to it (`architecture/evm/json_rpc_cache.go:L951-L978` vs `L926-L948`).

3. **Emptyish hit under `empty=ignore` is detected in the fan-out goroutine, not post-fan-out.** Without this early detection, the emptyish result would win the race, cancel peers, and only then be reclassified as a miss — losing a chance for a peer with `empty=allow` and non-empty data to win. Both stages filter independently (`architecture/evm/json_rpc_cache.go:L343-L351`).

4. **ErrRecordNotFound and ErrEndpointMissingData are semantic misses, not errors.** These must NOT increment `erpc_cache_get_error_total`. A regression where this happened caused 36k+ spurious "errors" per 15 minutes on shadow deployments (`erpc/evm_json_rpc_cache_fanout_test.go:TestEvmJsonRpcCache_FanOut_MissAsErrorIsClassifiedAsMiss`).

5. **Cancellation guard uses `fanCtx.Err()`, not `errors.Is(err, context.Canceled)`.** An inner failsafe wrapper can produce an opaque typed error that `errors.Is` cannot unwind. Trusting `fanCtx.Err()` as the authoritative cancellation signal prevents a losing peer's wrapped error from inflating `cache_get_error_total` (`erpc/evm_json_rpc_cache_fanout_test.go:TestEvmJsonRpcCache_FanOut_WrappedCancellationDoesNotInflateError`).

6. **Empty results for future blocks are never cached, regardless of `empty` setting.** `emptyResultBeyondConfidence` checks the block number against the network's latest (or finalized) head. If the requested block number > head, the empty result is silently discarded. This prevents the cache from poisoning responses for blocks that simply haven't been produced yet (`architecture/evm/json_rpc_cache.go:L1079-L1082`, `architecture/evm/common.go:L55-L89`).

7. **Compression threshold has two definitions.** The inline code default in `NewEvmJsonRpcCache` is 512 bytes (`architecture/evm/json_rpc_cache.go:L77-L79`), but `SetDefaults` sets it to 1024 bytes (`common/defaults.go:L597-L599`). `SetDefaults` is always called at startup, so the effective default is 1024. The 512 value is only reachable if the cache object is constructed directly without going through `SetDefaults` (i.e., in tests).

8. **Set has a hard 5-second timeout per connector.** This is in addition to (not instead of) any failsafe timeout configured on the connector. If both are configured, the shorter one fires first. The 5-second fallback protects against connectors with no failsafe configured (`architecture/evm/json_rpc_cache.go:L768`).

9. **The gRPC connector's block-head timestamp is set to zero when the block number advances but the timestamp parse fails.** This is intentional: storing a stale timestamp from a previous block would make `CacheLatestBlockTimestamp` report a dangerously stale head. Storing 0 makes the realtime guard fail-open (accept the cached result) rather than reject based on wrong age data (`data/grpc.go:L350-L356`).

10. **`appliesTo` defaults to `"both"`.** Setting `appliesTo: "get"` on a policy means it will never be written to on cache Set, allowing a different write-only policy to populate the connector while a separate read-only policy (possibly with different TTL or finality) controls what is served.

11. **Memory connector has independent internal zstd compression.** The `data.MemoryConnector` applies its own zstd before inserting into Ristretto. The `EvmJsonRpcCache` layer adds a second compression pass before calling `connector.Set`. Combined, values may be double-compressed: first by the cache layer, then by the memory connector. The memory connector's compression is transparent on read. Both compression layers detect already-compressed data by magic bytes and short-circuit (`data/memory_compression_test.go` covers memory connector level; `architecture/evm/json_rpc_cache.go:L1150-L1157` covers cache layer detection).

12. **`SkipCacheRead` accepts both boolean and string YAML values.** Both are normalized to string on parse. `true` → `"true"`, `false` → `"false"`. The directive is also accepted from config (`directiveDefaults.skipCacheRead`), HTTP header `X-ERPC-Skip-Cache-Read`, and query param `skip-cache-read` (`common/request.go:L41, L67, L737-L738, L823-L824`).

13. **Policy ordering matters for Get when finality = unknown.** For unknown-finality requests, `findGetPolicies` returns all matching policies and they race in parallel. The first connector with data wins. Place the fastest/most-recently-populated connector first so it is most likely to win the race. For Set, all matching policies receive the write regardless of order.

14. **Missing `blockRef` skips cache silently on both Get and Set.** If `ExtractBlockReferenceFromRequest` returns an empty string (method not in the known block-extractable set), the cache layer returns `nil, nil` on Get and `nil` on Set — neither an error nor a hit. Trace attribute `cache.skip_reason = "empty_block_ref"` is set on the span (`architecture/evm/json_rpc_cache.go:L988-L996`).

15. **`database.evmJsonRpcCache: nil` completely disables the cache subsystem.** This is not just a "no policies match" situation — the entire `EvmJsonRpcCache` object is never created, saving memory and eliminating the overhead of block-ref extraction on every request. When adding a cache, the top-level `database.evmJsonRpcCache:` key must exist (even if empty), because `SetDefaults` is only called when the pointer is non-nil (`common/defaults.go:L831-L836`).

16. **Compression auto-creation means user cannot disable it by omitting the block.** `CacheConfig.SetDefaults` always creates `&CompressionConfig{}` if `c.Compression == nil` and then calls `SetDefaults()` on it, which sets `enabled=true` (`common/defaults.go:L482-L488`, `L580-L582`). The only way to disable compression is to explicitly set `compression.enabled: false` in YAML. This is a footgun: a config that silently enables zstd compression without any explicit opt-in.

17. **`erpc_cache_set_compressed_bytes_total` counter is never emitted when compression is disabled or values are too small.** Do not interpret a zero value for this counter as evidence of a bug. When compression is disabled (`compression.enabled: false`), zero is expected. When all cached values are smaller than `compressionThreshold` (default 1024 bytes), compression is never applied and the counter stays at zero. Compression ratio = `erpc_cache_set_original_bytes_total / erpc_cache_set_compressed_bytes_total`; this ratio is only meaningful when both counters are non-zero in the same time window.

18. **reqRefs with multiple conflicting block references collapse to `"*"`.** `eth_getLogs` has `reqRefs = [[{0,"fromBlock"}], [{0,"toBlock"}], [{0,"blockHash"}]]`. If `fromBlock` and `toBlock` differ, the extraction logic sets `blockRef = "*"` (composite range, `architecture/evm/block_ref.go:L296-L300`). This means log range queries are keyed with a wildcard partition, routed to the reverse index, and can collide with other `"*"` entries. The comment in `block_ref.go:L296` explicitly notes that reorg-invalidation for these would require bespoke logic. Users must cache only finalized data for range-query methods to avoid serving stale reorged data.

19. **Custom `reqRefs`/`respRefs` override built-in defaults entirely.**

20. **`finality: realtime` must be stated explicitly — omission silently drops all realtime traffic.** The zero value of `DataFinalityState` is `finalized` (iota=0, `common/data.go:L10-L15`). A policy YAML with no `finality:` key is byte-for-byte identical to `finality: finalized`. Methods resolved as `realtime` (`DataFinalityStateRealtime`) only match policies whose finality is explicitly `realtime` (`architecture/evm/json_rpc_cache.go:L951-L978`, `MatchesForGet`). Common realtime methods that will silently miss a policy without `finality: realtime`: `eth_blockNumber`, `eth_gasPrice`, `eth_call` with `"latest"` tag, `eth_getBalance` with `"latest"`. Users who intend to cache all traffic types must add at minimum four policies (one per finality bucket) or one policy per bucket per method group. This is the most common misconfiguration in the wild.

21. **`minItemSize`/`maxItemSize` measure the pre-compression result length, not the stored blob size.** The comparison happens in `shouldCacheResponse` using `rpcResp.ResultLength()` BEFORE any zstd pass is attempted (`architecture/evm/json_rpc_cache.go:L1067-L1072`). A 4 KB result that compresses to 400 bytes will still pass a `maxItemSize: 2KB` check (4096 > 2048 → filtered out). Conversely, setting `minItemSize: 1KB` to avoid caching tiny responses uses the uncompressed result byte count, which matches intuitive intent. The measured value is the raw JSON result bytes stored in `JsonRpcResponse.result` — not the full JSON-RPC envelope (no `id`, `jsonrpc`, `method` wrapper counted). When a response is filtered by size, the filter is a silent skip: `erpc_cache_set_skipped_total` is incremented, but no error is logged or returned to the caller, and the request proceeds to (or already came from) an upstream.

### Source map

- `data/cache_policy.go` — `CachePolicy` struct; `MatchesForGet`, `MatchesForSet`, `matchParams`, `matchParam`, `MatchesSizeLimits`, `GetTTL` methods
- `data/cache_executor.go` — `cacheExecutor` for per-connector failsafe (retry, hedge, circuit-breaker, timeout); `cacheBreakerOutcome`, `isTransportError` integration
- `data/connector.go` — `Connector` interface; `CacheHeadReporter` interface; `NewConnector` factory + failsafe wrapping; `ConnectorMainIndex`/`ConnectorReverseIndex` constants
- `data/failsafe.go` — `FailsafeConnector` (wraps raw connector); `CacheLatestBlockTimestamp` forwarding; `isTransportError` classification
- `data/grpc.go` — `GrpcConnector`; BDS gRPC read-through; `eth_blockNumber` derivation; background block-head poller (60s); `CacheLatestBlockTimestamp`; fast-miss rejection by earliest block
- `architecture/evm/json_rpc_cache.go` — `EvmJsonRpcCache`; `Get` (parallel fan-out); `Set` (parallel fan-out); `shouldAcceptCachedResult` (realtime age guard); `shouldCacheResponse`; `generateKeysForJsonRpcRequest`; compression (zstd pool, threshold, magic-byte detection)
- `architecture/evm/block_ref.go` — `ExtractBlockReferenceFromRequest`; `ExtractBlockReferenceFromResponse`; `ExtractBlockTimestampFromResponse`
- `architecture/evm/common.go` — `emptyResultBeyondConfidence` — guards against caching empty results for future/unconfirmed blocks
- `common/cache_dal.go` — `CacheDAL` interface (Get/Set/IsObjectNull) consumed by the network and project layers
- `common/data.go` — `DataFinalityState` enum (finalized=0, unfinalized=1, realtime=2, unknown=3); `CacheEmptyBehavior` enum (ignore=0, allow=1, only=2); `CachePolicyAppliesTo` type
- `common/config.go` — `CacheConfig`, `CachePolicyConfig`, `CompressionConfig`, `ConnectorConfig`, `MemoryConnectorConfig`, `RedisConnectorConfig`, `DynamoDBConnectorConfig`, `PostgreSQLConnectorConfig`, `GrpcConnectorConfig`; `RequestDirectives.SkipCacheRead`
- `common/defaults.go` — `CacheConfig.SetDefaults`, `CachePolicyConfig.SetDefaults`, `CompressionConfig.SetDefaults` — all effective defaults
- `common/request.go` — `ShouldSkipCacheRead`; directive parsing from header `X-ERPC-Skip-Cache-Read` and query `skip-cache-read`
- `common/json_rpc.go` — `JsonRpcRequest.CacheHash` (SHA-256 of params + method); `JsonRpcResponse.IsResultEmptyish`; `ResultLength`
- `util/bytes.go` — `IsBytesEmptyish` — defines the "empty result" concept
- `util/string.go` — `ParseByteSize` — parses `minItemSize`/`maxItemSize` strings (`B`, `KB`, `MB`)
- `telemetry/metrics.go` — all `MetricCache*` metrics; `MetricRistretto*` metrics; `MetricCacheConnector*` gauges
- `erpc/evm_json_rpc_cache_fanout_test.go` — integration tests for fan-out behavior, skip-cache-read, cancellation, miss/error classification, slow-peer non-blocking
- `architecture/evm/json_rpc_cache_age_test.go` — unit tests for realtime TTL age guard
- `architecture/evm/json_rpc_cache_connector_freshness_test.go` — unit tests for connector-fallback freshness path; FailsafeConnector forwarding; fail-open cases
- `data/memory_compression_test.go` — zstd compression at memory connector level; threshold, TTL, concurrency
