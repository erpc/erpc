# KB: Cache/database connectors: memory, redis, postgres, dynamodb, grpc

> status: complete
> source-dirs: data/connector.go, data/memory.go, data/redis.go, data/postgresql.go, data/dynamodb.go, data/grpc.go, data/failsafe.go, data/cache_executor.go, data/timeout_constants.go, data/redis_pubsub_manager.go, common/config.go (ConnectorConfig, *ConnectorConfig subtypes), common/defaults.go (SetDefaults for all connector configs), common/validation.go (Validate for connector configs), telemetry/metrics.go (Ristretto + gRPC connector metrics)

## L1 — Capability (CTO view)

eRPC has five interchangeable cache/storage connector back-ends: an in-process LRU (memory/ristretto), Redis, PostgreSQL, DynamoDB, and a read-only gRPC BDS (Blockchain Data Standards) connector. All five implement the same `Connector` interface (`data/connector.go:L40-L51`) so they are hot-swappable at YAML config time. Any connector can be optionally wrapped with a `FailsafeConnector` (`data/failsafe.go:L98-L103`) that applies per-operation retry, circuit-breaker, hedge, and timeout policies scoped to method+finality patterns — decoupling transient cache failures from upstream fan-out. The gRPC connector also implements `CacheHeadReporter` so the real-time cache age guard can enforce freshness for responses (e.g., `eth_blockNumber`) that carry no block timestamp of their own.

## L2 — Mechanics (staff-engineer view)

### Connector interface and factory

`data/connector.go:L40-L51` defines `Connector`:
- `Id() string`
- `Get(ctx, index, partitionKey, rangeKey, metadata) ([]byte, error)`
- `Set(ctx, partitionKey, rangeKey, value []byte, ttl *time.Duration) error`
- `Delete(ctx, partitionKey, rangeKey) error`
- `List(ctx, index, limit, paginationToken) ([]KeyValuePair, string, error)`
- `Lock(ctx, key, ttl) (DistributedLock, error)`
- `WatchCounterInt64(ctx, key) (<-chan CounterInt64State, func(), error)`
- `PublishCounterInt64(ctx, key, value) error`

`NewConnector` (`data/connector.go:L63-L103`) dispatches on `cfg.Driver` and, when `cfg.FailsafeForGets` or `cfg.FailsafeForSets` is non-empty, wraps the resulting connector in `NewFailsafeConnector`.

Two special index constants exist: `ConnectorMainIndex = "idx_main"` and `ConnectorReverseIndex = "idx_reverse"` (`data/connector.go:L12-L15`). Callers pass the index to `Get` to select which index path to use; `Set` always writes to the main index and, for `evm:`-prefixed partition keys, also writes a reverse index entry.

`CounterInt64State` (`data/connector.go:L34-L38`) is the canonical JSON payload for distributed int64 counters:
```json
{"v": 42, "t": 1718000000000, "b": "pod-name"}
```
- `v` = value
- `t` = unix milliseconds; `t ≤ 0` = uninitialized
- `b` = best-effort reporter identity (hostname/pod)

### Memory connector (ristretto)

Backed by [dgraph-io/ristretto v2](https://github.com/dgraph-io/ristretto). The underlying cache uses:
- `NumCounters = 3 × MaxItems` (frequency sketch size)
- `MaxCost = maxTotalSizeBytes` (integer capped at `math.MaxInt64`)
- `BufferItems = 64`
- cost function: `len(value) + 256` bytes per entry (`data/memory.go:L73-L82`)

TTL: `Set` calls `cache.SetWithTTL(key, value, 0, *ttl)` when ttl > 0, else `cache.Set(key, value, 0)` (`data/memory.go:L115-L119`). Ristretto handles expiry automatically; no background cleanup goroutine.

Reverse index for wildcard lookups: when a partition key starts with `evm:` and does NOT end with `*`, `Set` also stores `rvi#<wildcardPartitionKey>#<rangeKey> → partitionKey` (no TTL on the reverse index entry, `data/memory.go:L124-L131`). `Get` resolves this before the main lookup when `index == ConnectorReverseIndex` and partitionKey ends with `*`.

`WatchCounterInt64` and `PublishCounterInt64` are no-ops for memory (single-process; no pub/sub needed, `data/memory.go:L196-L208`).

`Lock` uses a `sync.Map` of `*sync.Mutex` with `TryLock` + backoff-sleep loop respecting context cancellation (`data/memory.go:L156-L178`). Retry starts at 2ms, increments by 1ms each attempt up to 20ms.

`List` is intentionally NOT implemented: ristretto has no efficient iteration. Returns an error with guidance to use Redis/PostgreSQL/DynamoDB for admin operations (`data/memory.go:L316-L321`).

Metrics: a background goroutine runs every 30 seconds when `emitMetrics == true`. It emits `erpc_ristretto_cache_current_cost` (gauge, bytes) and delta-accumulates `erpc_ristretto_cache_sets_failed_total` (counter) using `SetsDropped + SetsRejected` (`data/memory.go:L212-L294`).

### Redis connector

Uses [go-redis/v9](https://github.com/redis/go-redis/v9) plus [go-redsync/v4](https://github.com/go-redsync/redsync) for distributed locking. A single `*redis.Client` instance plus a `*redsync.Redsync` are created per connector.

**Connection lifecycle.** `NewRedisConnector` constructs the struct and enqueues a bootstrap task in `util.Initializer` (`data/redis.go:L76-L107`). If the first attempt fails the connector returns non-nil (operable) but not-ready; retries run in background. `connectTask` (`data/redis.go:L114-L222`) parses the URI, applies TLS if configured, dials + pings (timeout-gated by `initTimeout`), and installs a new `*redis.Client`. TLS merging logic: if both `tls.enabled: true` and a `rediss://` URI are provided, YAML certificates/CAs are merged onto the baseline TLS config (disabling `InsecureSkipVerify`).

**Ready check.** Every operation calls `checkReady` (`data/redis.go:L262-L277`) which checks `initializer.State() == StateReady` and that `client != nil` and `redsync != nil`. If not ready, the operation returns an error immediately (no queuing).

**Connection failure detection.** `markConnectionAsLostIfNecessary` (`data/redis.go:L225-L259`) fires only for a narrow allowlist of critical failure strings (e.g., `"connection refused"`, `"broken pipe"`, `"invalid connection"`). Transient errors, `redis.Nil`, `TxFailedErr`, and context cancellation/deadline are explicitly excluded to avoid spurious reconnects.

**TTL on Set.** When `ttl != nil && *ttl > 0` the Redis `SET key value EX` is used; otherwise the key is stored with no expiry (`duration == 0`). The reverse index entry inherits the same TTL (`data/redis.go:L307-L337`).

**Reverse index verification on Get.** When `index == ConnectorReverseIndex` and the partition key ends with `*`, Redis first fetches the reverse index key. If found, it then checks the TTL of the resolved key: `-2s` = key does not exist → return `ErrRecordNotFound`; `-1s` = persistent (OK); `> 0` = has TTL (OK) (`data/redis.go:L357-L395`).

**Distributed lock.** Uses `redsync.NewMutex` with `redsync.WithExpiry(ttl)`, `redsync.WithRetryDelay(lockRetryInterval)`, and computed `maxRetries` derived from context deadline and lock TTL (`data/redis.go:L434-L537`). The `DefaultOperationBuffer = 10s` is reserved before the context deadline for in-lock operations (`data/timeout_constants.go:L8`).

**Pub/sub.** `WatchCounterInt64` and `PublishCounterInt64` go through `RedisPubSubManager` which maintains a single persistent pubsub connection with transparent reconnection. Channel name: `counter:<key>`. Payload: JSON-serialized `CounterInt64State`.

**List.** Uses Redis `SCAN` with cursor-based pagination. Main index scans all keys with `*`; reverse index scans with `rvi#*` prefix. Values fetched in pipeline after SCAN (`data/redis.go:L681-L784`).

### PostgreSQL connector

Uses [jackc/pgx v4](https://github.com/jackc/pgx) with `pgxpool`. The connector maintains a read/write pool (`*pgxpool.Pool`) and a separate listener pool for LISTEN/NOTIFY.

**Init and reconnect design.** Schema setup (`applySchema`) runs at most once per process lifetime, gated by `schemaApplied bool` under `schemaMu` mutex. This prevents the `cron.schedule` call (which is not idempotent — each call inserts a new pg_cron job) and the `DROP COLUMN` migration from running on every reconnect (`data/postgresql.go:L257-L268`, `data/postgresql.go:L274-L373`). Pool swap is done under a brief write lock; the old pool is closed AFTER releasing the lock to prevent blocking hot paths during drain (`data/postgresql.go:L211-L233`).

**Schema.** `applySchema` creates (if not exists):
- Table `<table>` with columns: `partition_key TEXT`, `range_key TEXT`, `value BYTEA`, `expires_at TIMESTAMP WITH TIME ZONE`; PRIMARY KEY `(partition_key, range_key)`
- Migration from legacy `TEXT` value column to `BYTEA` (safe; idempotent via column-existence check)
- Index `idx_reverse ON (range_key, partition_key)`
- Index `idx_expires_at ON (expires_at) WHERE expires_at IS NOT NULL`
- pg_cron job `*/5 * * * *` to `DELETE FROM <table> WHERE expires_at <= NOW()` if the `pg_cron` extension exists. If pg_cron is available, the local 5-minute cleanup ticker is set to nil to avoid duplication (`data/postgresql.go:L344-L370`).

**TTL on Set.** Inserts/upserts with `expires_at = NOW() + ttl` when ttl is provided; otherwise the column is NULL. Get queries filter `expires_at IS NULL OR expires_at > NOW()` (`data/postgresql.go:L432-L497`).

**Wildcard Get.** When partition key or range key ends with `*`, `getWithWildcard` (`data/postgresql.go:L1005-L1062`) translates `*` to `%` and uses LIKE. For `ConnectorReverseIndex`: `WHERE range_key = $1 AND partition_key LIKE $2`; for main index: `WHERE partition_key = $1 AND range_key LIKE $2`. Returns the first row ordered by `partition_key DESC`.

**Distributed lock.** Uses PostgreSQL advisory transaction-level locks via `pg_try_advisory_xact_lock(hash64(key))`. FNV-64a hashes the key to an int64 lock ID. Lock held until `Unlock` which commits (or rolls back on failure). This is a non-blocking try: if the lock is already held, `acquired = false` → error returned immediately (`data/postgresql.go:L525-L586`).

**Pub/sub / WatchCounterInt64.** Uses `pg_notify(<channel>, <payload>)` for publish and a persistent LISTEN connection (from a separate pool) for subscribe. Falls back to 30-second polling if LISTEN is unavailable. Initial value is pushed synchronously at subscribe time. Channel names are sanitized to valid PostgreSQL identifier characters (max 63 bytes) via `sanitizeChannelName` (`data/postgresql.go:L1103-L1127`).

**Connection failure detection.** `isPostgresConnectionError` (`data/postgresql.go:L781-L840`) only matches: pgconn SQLSTATE codes starting with `"08"` (Connection Exception class), specific kernel syscall errors, and a narrow set of string patterns (explicitly excludes bare `"connection"` substring to avoid triggering on "too many connections" or its own `ErrConnectorNotReady`). Failure marks are coalesced with a 1s cooldown (`failureMarkCooldown`, `data/postgresql.go:L95`) via `atomic.Int64` CAS to prevent cascades.

**List.** Uses `SELECT ... LIMIT $1 OFFSET $2` with base64-encoded JSON pagination tokens carrying the `offset` integer (`data/postgresql.go:L1164-L1270`).

### DynamoDB connector

Uses [aws-sdk-go v1](https://github.com/aws/aws-sdk-go). Two separate `*dynamodb.DynamoDB` clients are used: `readClient` (2048 max idle conns, HTTP/2) and `writeClient` (256 max idle conns, HTTP/2) (`data/dynamodb.go:L62-L83`). This splits read and write traffic across independent connection pools.

**Init.** `connectTask` creates the AWS session via `createSession`, then calls `createTableIfNotExists` (idempotent; `BillingMode: PAY_PER_REQUEST`) and `ensureGlobalSecondaryIndexes` to add the reverse GSI if it doesn't exist yet (`data/dynamodb.go:L126-L161`).

**TTL.** Stored as a numeric DynamoDB attribute (unix epoch seconds). `Set` sets `<ttlAttributeName> = time.Now().Add(*ttl).Unix()` (`data/dynamodb.go:L394-L399`). DynamoDB native TTL auto-expires items (eventual, may lag up to 48h). **Client-side TTL check on Get:** even for a main-index GetItem, if the item's TTL attribute is set and `time.Now().Unix() > expirationTime`, returns `ErrRecordExpired` (`data/dynamodb.go:L547-L556`). For reverse-index Query: up to 10 items are fetched with a server-side `FilterExpression` and the first non-expired item is chosen (`data/dynamodb.go:L461-L511`).

**Backward compatibility.** Both `B` (binary) and `S` (string) value attributes are read; legacy string values are returned as `[]byte(*S)` (`data/dynamodb.go:L512-L520`, `data/dynamodb.go:L558-L566`).

**Distributed lock.** Uses a conditional `PutItem` with `ConditionExpression: attribute_not_exists(#pk) OR #expiry < :now`. Lock item has `expiry` numeric attribute (epoch seconds). Retries on `ConditionalCheckFailedException`, `ProvisionedThroughputExceededException`, `InternalServerError`, `LimitExceededException` with `lockRetryInterval` sleep. Non-retryable errors fail immediately (`data/dynamodb.go:L596-L688`). `Unlock` deletes the lock item via `DeleteItem` (`data/dynamodb.go:L690-L713`).

**WatchCounterInt64 / polling.** No native pub/sub; uses periodic polling with `statePollInterval` ticker. Only emits updates when `st.UpdatedAt > lastUpdatedAt`. `PublishCounterInt64` is a no-op for DynamoDB — callers rely on polling to pick up state changes (`data/dynamodb.go:L715-L830`).

**List.** Uses DynamoDB `Scan` with `ExclusiveStartKey` as the pagination cursor; tokens are base64-encoded JSON maps of DynamoDB key attributes. Expired items are filtered client-side (`data/dynamodb.go:L873-L983`).

### gRPC connector (read-only)

A read-through connector backed by BDS gRPC servers that serve historical EVM chain data. It is **read-only**: `Set`, `Delete`, `List`, `Lock`, `WatchCounterInt64`, and `PublishCounterInt64` all return errors (`data/grpc.go:L454-L478`).

**Bootstrap.** If `cfg.Bootstrap` (HTTP URL) is set, `fetchGrpcServers` fetches it, trying JSON array first then newline-separated text (`data/grpc.go:L422-L452`). If `cfg.Servers` is non-empty those are used directly. One `BootstrapTask` per server URL is created. Each task: parses URL, creates `clients.GrpcBdsClient`, applies headers, probes `eth_chainId` with a 5s timeout, derives `networkId = "evm:<decimal_chainId>"`, stores in `clientByNetwork` (`data/grpc.go:L119-L177`).

**Get flow.**
1. Require `metadata` to be `*common.NormalizedRequest` (else miss).
2. Check method is in `supportedMethods` (allowlist: `eth_getBlockByNumber`, `eth_getBlockByHash`, `eth_getLogs`, `eth_getTransactionByHash`, `eth_getTransactionReceipt`, `eth_getBlockReceipts`, `eth_chainId`, `eth_blockNumber`). Unsupported method → return `(nil, nil)` (fast skip, not an error).
3. Fast-reject if request block number < `earliestByNetwork[networkId]`.
4. Apply per-request timeout (`getTimeout`) only if existing deadline is longer or absent.
5. `eth_blockNumber` is special: no native BDS method; translated to `eth_getBlockByNumber("latest")` internally, returning the block's `number` field as a canonical hex string (`"0x1a2b3c"`, `data/grpc.go:L265-L278`).
6. Other methods: forward to `cli.SendRequest`, extract JSON-RPC result bytes.

**Background block head poller.** A goroutine started in `NewGrpcConnector` runs every 60 seconds (`data/grpc.go:L302-L313`). Each tick calls `pollBlockHeadsOnce` which snapshots `clientByNetwork` under a read lock then, for each network, calls `fetchTaggedBlock(ctx, cli, "earliest"|"latest"|"finalized")`. `fetchTaggedBlock` issues `eth_getBlockByNumber(<tag>, false)` and JSON-parses the block `number` and `timestamp` fields. Failed or unparsable responses leave the prior stored values and gauges **untouched** rather than resetting them to zero — transient network errors do not cause spurious freshness gate failures (`data/grpc.go:L322-L374`). Emits 6 gauges per network (block number + unix timestamp for each of earliest, latest, finalized). In addition, `latestTsByNetwork[nid]` is always updated in lockstep with the latest block number: if the block timestamp is absent (zero), zero is stored, causing `CacheLatestBlockTimestamp` to return `ok = false` (fails open) — ensuring an advanced block number is never evaluated against an older block's timestamp (`data/grpc.go:L349-L357`).

**CacheHeadReporter interface and freshness gate.** `GrpcConnector` implements `data.CacheHeadReporter` (`data/connector.go:L53-L61`, `data/grpc.go:L55`). The interface exposes `CacheLatestBlockTimestamp(networkId string) (unixSeconds int64, ok bool)`. The realtime cache age guard in `EvmJsonRpcCache.shouldAcceptCachedResult` (`architecture/evm/json_rpc_cache.go:L834-L924`) uses this interface as a fallback when the cached response itself carries no block timestamp (i.e., `eth_blockNumber`, `eth_gasPrice`, `eth_getLogs`, and similar scalar/log results). The gate logic: (1) extract block timestamp from the response; (2) if absent, call `reporter.CacheLatestBlockTimestamp(networkId)` on the serving connector; (3) if still unknown (connector not head-aware or `ok=false`), **fail open** (accept the cached result); (4) otherwise compare age to the policy TTL and reject if `age > TTL` (`architecture/evm/json_rpc_cache.go:L862-L920`). `FailsafeConnector` transparently forwards `CacheLatestBlockTimestamp` to its wrapped connector (`data/failsafe.go:L211-L216`), so the gate continues to work when the gRPC connector is wrapped with failsafe policies.

### FailsafeConnector

`FailsafeConnector` (`data/failsafe.go:L98-L103`) wraps any `Connector`. It holds two independent slices of `*cacheExecutor`: `getExecutors` (applied to `Get`) and `setExecutors` (applied to `Set` and `Delete`). `List`, `Lock`, `WatchCounterInt64`, and `PublishCounterInt64` are passed directly to the wrapped connector without any failsafe policies.

**Executor selection** (`pickCacheExecutor`, `data/failsafe.go:L159-L201`): four-tier priority (checked in order):
1. `method != "*" AND finalities non-empty` — exact method + finality match
2. `method != "*" AND finalities empty` — method match only
3. `method == "*" AND finalities non-empty` — wildcard method + finality match
4. `method == "*" AND finalities empty` — default/catch-all

A no-op fallback executor is always appended so unmatched operations proceed unconditionally (`data/failsafe.go:L153-L156`).

**cacheExecutor execution model** (`data/cache_executor.go`):
- `RunBytes` (for Get): retry loop → hedge → circuit breaker → timeout → `inner(ctx)`
- `RunVoid` (for Set/Delete): same pipeline, discards returned bytes
- Retry: only re-executes on `isTransportError(err)`. `ErrRecordNotFound`, `ErrRecordExpired`, `context.Canceled`, `context.DeadlineExceeded` are NOT transport errors and are NOT retried.
- Circuit breaker: `ErrRecordNotFound` and `ErrRecordExpired` → `OutcomeIgnore` (not counted). Transport errors → `OutcomeFailure`. Success → `OutcomeSuccess`.
- Hedge: quantile-based delay NOT supported (no latency data source at this layer); only static delays work. Uses `Delay.Resolve(nil)` (cold-start: `Base + Min`).
- Timeout: applied via `context.WithTimeoutCause`; on expiry translates `DeadlineExceeded` to `ErrFailsafeTimeoutExceeded` when the cause is `ErrDynamicTimeoutExceeded`.
- Consensus: explicitly rejected at construction time (`data/cache_executor.go:L31-L35`).

**isTransportError** (`data/failsafe.go:L25-L92`): recognizes net.Error timeouts, io.EOF/ErrUnexpectedEOF, syscall connection errors, gRPC status codes `Unavailable`/`DeadlineExceeded`/`Aborted`, and a specific set of string patterns including Redis cluster transients (`CLUSTERDOWN`, `MASTERDOWN`, `TRYAGAIN`, `LOADING`), HTTP/2 GOAWAY, `"use of closed network connection"`, etc.

**FailsafeConnector also implements CacheHeadReporter**: forwards to the wrapped connector if it implements the interface (`data/failsafe.go:L211-L216`).

## L3 — Exhaustive reference (agent view)

### Config fields

#### Top-level `ConnectorConfig` (`common/config.go:L341-L352`)

| YAML path | Type | Default | Behavior |
|-----------|------|---------|----------|
| `<connector>.id` | string | `"<scope>-<driver>"` (e.g., `"cache-memory"`) `common/defaults.go:L847-L848` | Unique identifier used in metrics labels (`connector` label) |
| `<connector>.driver` | string | Inferred from which sub-config is present; if both present, explicit `driver` field wins `common/defaults.go:L876-L930` | One of: `memory`, `redis`, `postgresql`, `dynamodb`, `grpc` |
| `<connector>.failsafeForGets` | `[]*FailsafeConfig` | nil (no failsafe) | List of per-(method, finality) failsafe policies applied to Get operations. Evaluated in declaration order by `pickCacheExecutor`. |
| `<connector>.failsafeForSets` | `[]*FailsafeConfig` | nil (no failsafe) | List of per-(method, finality) failsafe policies applied to Set and Delete operations. |

Each `FailsafeConfig` entry under `failsafeForGets`/`failsafeForSets` has:

| YAML field | Type | Default | Notes |
|-----------|------|---------|-------|
| `matchMethod` | string | `"*"` `common/defaults.go:L855-L857` | Glob/wildcard pattern matched against the request method. `"*"` = all methods. |
| `matchFinality` | `[]DataFinalityState` | nil (any finality) | Filter by finality state; nil = matches any finality. |
| `retry.maxAttempts` | int | — | Only retries on transport errors. |
| `retry.delay` | Duration | — | Base delay between attempts. |
| `retry.backoffMaxDelay` | Duration | — | Max delay with backoff. |
| `retry.backoffFactor` | float64 | — | Multiply delay by this factor each attempt. |
| `circuitBreaker.*` | — | — | Standard breaker config. Cache misses and expired records do NOT count as failures. |
| `timeout.duration` | Duration | — | Per-operation timeout. Context is replaced. |
| `hedge.delay` | Duration | — | Static delay before firing a hedged duplicate. Quantile-based not supported here. |
| `hedge.maxCount` | int | — | Max concurrent hedged copies. |
| `consensus` | — | nil (disabled) | **Rejected at construction time**: if non-nil, `NewCacheExecutor` returns `ErrFailsafeConfiguration` with message `"consensus is not supported for connector-level failsafe"` (`data/cache_executor.go:L31-L35`, `common/validation.go:L492-L494`). Reason: consensus semantics (waiting for a quorum of upstream responses) require a latency data source and multi-upstream context that does not exist at the single-node cache-connector layer. |
| `hedge.delay.quantile` | float64 | must be 0 | If > 0, `NewCacheExecutor` returns `ErrFailsafeConfiguration` with message `"hedge quantile is not supported for connector-level failsafe (no latency metric source)"` (`data/cache_executor.go:L37-L41`, `common/validation.go:L495-L497`). Reason: quantile-based hedge delays require per-method latency histograms that are only tracked at the upstream (network) layer, not at the connector layer. Only static `hedge.delay.base` / `hedge.delay.min` values work. |

#### Memory connector (`common/config.go:L361-L365`, defaults `common/defaults.go:L935-L943`)

| YAML path | Type | Default | Behavior |
|-----------|------|---------|----------|
| `memory.maxItems` | int | `100000` | ristretto `NumCounters = 3 × maxItems`. Must be > 0 or init fails. |
| `memory.maxTotalSize` | string | `"1GB"` | Parsed by `go-humanize`; becomes ristretto `MaxCost` in bytes. Must be > 0. |
| `memory.emitMetrics` | *bool | nil = disabled | When `true`, background goroutine emits ristretto metrics every 30s: `erpc_ristretto_cache_current_cost` (gauge, bytes currently used) and `erpc_ristretto_cache_sets_failed_total` (counter, delta of dropped+rejected sets). See Observability § for full metric definitions. `data/memory.go:L212-L294`, `telemetry/metrics.go:L628-L638`. |

#### Redis connector (`common/config.go:L383-L395`, defaults `common/defaults.go:L946-L1018`)

| YAML path | Type | Default | Behavior |
|-----------|------|---------|----------|
| `redis.uri` | string | — | Redis connection URI (e.g., `redis://user:pass@host:6379/0` or `rediss://...` for TLS). Mutually exclusive with `addr`. |
| `redis.addr` | string | — | Host:port (default port 6379 if omitted). Converted to URI at defaults time; cleared afterward. Mutually exclusive with `uri`. |
| `redis.username` | string | — | Used to construct URI; cleared after. |
| `redis.password` | string | — | Redacted in logs/JSON. Used to construct URI; cleared after. |
| `redis.db` | int | `0` | Database index. Used to construct URI (`/<db>`). |
| `redis.tls.enabled` | bool | `false` | When true, uses `rediss://` scheme. Merged with URI-inferred TLS if both present. |
| `redis.tls.certFile` | string | — | Client certificate for mTLS. |
| `redis.tls.keyFile` | string | — | Client private key for mTLS. |
| `redis.tls.caFile` | string | — | Custom CA bundle for server certificate verification. |
| `redis.tls.insecureSkipVerify` | bool | `false` | Skip server certificate verification. |
| `redis.connPoolSize` | int | `8` | go-redis connection pool size. |
| `redis.initTimeout` | Duration | `5s` | Timeout for Ping during initial connection (and per reconnect attempt). Falls back to `DialTimeout` from parsed URI if not set. |
| `redis.getTimeout` | Duration | `1s` | Per-Get context deadline. Falls back to `ReadTimeout` from URI. |
| `redis.setTimeout` | Duration | `3s` | Per-Set/Delete context deadline. Falls back to `WriteTimeout` from URI. Used for lock Unlock too. |
| `redis.lockRetryInterval` | Duration | `500ms` | How long to sleep between redsync mutex acquisition retries. |

#### PostgreSQL connector (`common/config.go:L445-L453`, defaults `common/defaults.go:L1021-L1058`)

| YAML path | Type | Default | Behavior |
|-----------|------|---------|----------|
| `postgresql.connectionUri` | string | — | pgx connection string (required). Redacted in logs. |
| `postgresql.table` | string | `"erpc_json_rpc_cache"` (cache scope), `"erpc_shared_state"` (shared state scope), `"erpc_auth"` (auth scope) | Table name created/used by this connector. |
| `postgresql.minConns` | int32 | `4` (non-auth), `1` (auth scope) | pgxpool `MinConns`. |
| `postgresql.maxConns` | int32 | `32` (non-auth), `4` (auth scope) | pgxpool `MaxConns`. |
| `postgresql.initTimeout` | Duration | `5s` | Context timeout for pool creation (`pgxpool.ConnectConfig`). **Not read from `connectionUri`** — pgx parses the URI for pool parameters (`MinConns`, `MaxConns`, etc.) but NOT for timeout values; these three fields are always applied as explicit Go context deadlines (`data/postgresql.go:L186`). Unlike Redis, PostgreSQL connection-string timeout params (e.g., `connect_timeout`) do NOT override these fields. |
| `postgresql.getTimeout` | Duration | `1s` | Per-Get/List query timeout. Applied as `context.WithTimeout` on every query; never falls back to URI-embedded values (`data/postgresql.go:L130-L134`). |
| `postgresql.setTimeout` | Duration | `2s` | Per-Set/Delete query timeout. Applied as `context.WithTimeout`; never falls back to URI values (`data/postgresql.go:L130-L134`). |

Pool settings hardcoded: `MaxConnLifetime = 5h`, `MaxConnIdleTime = 30m` (`data/postgresql.go:L183-L184`).

#### DynamoDB connector (`common/config.go:L428-L443`, defaults `common/defaults.go:L1061-L1099`)

| YAML path | Type | Default | Behavior |
|-----------|------|---------|----------|
| `dynamodb.table` | string | `"erpc_json_rpc_cache"` (cache), `"erpc_shared_state"` (shared state), `"erpc_auth"` (auth) | DynamoDB table name; created at init if not exists with `PAY_PER_REQUEST` billing. |
| `dynamodb.region` | string | — | AWS region (required; fatal if empty). |
| `dynamodb.endpoint` | string | — | Custom endpoint URL (e.g., DynamoDB Local). |
| `dynamodb.auth.mode` | string | — | One of: `"file"` (shared credentials), `"env"` (env vars), `"secret"` (static key). If `auth` is nil, default session credentials are used. |
| `dynamodb.auth.credentialsFile` | string | — | Path to AWS credentials file (when mode=`file`). |
| `dynamodb.auth.profile` | string | — | AWS profile in credentials file. |
| `dynamodb.auth.accessKeyID` | string | — | Static access key (when mode=`secret`). **YAML key is `accessKeyID` (capital `I` and capital `D`) — using `accessKeyId` (lowercase `d`) will NOT be recognized** because the struct tag is `yaml:"accessKeyID"` (`common/config.go:L483`). This is the same `AwsAuthConfig` struct reused for both DynamoDB and the S3 misbehaviors destination in consensus. |
| `dynamodb.auth.secretAccessKey` | string | — | Static secret key (when mode=`secret`). |
| `dynamodb.partitionKeyName` | string | `"groupKey"` | DynamoDB partition key attribute name. |
| `dynamodb.rangeKeyName` | string | `"requestKey"` | DynamoDB sort/range key attribute name. |
| `dynamodb.reverseIndexName` | string | `"idx_requestKey_groupKey"` | GSI name for reverse lookups (sort key = rangeKey, partition key = partitionKey in original). |
| `dynamodb.ttlAttributeName` | string | `"ttl"` | DynamoDB attribute name holding TTL (unix epoch seconds). Enabled via DynamoDB TTL API. |
| `dynamodb.initTimeout` | Duration | `5s` | Timeout for session creation and table/index init HTTP calls. |
| `dynamodb.getTimeout` | Duration | `1s` | Per-Get context deadline. |
| `dynamodb.setTimeout` | Duration | `2s` | Per-Set/Delete/Lock context deadline. |
| `dynamodb.maxRetries` | int | `0` (SDK default) | AWS SDK retry count for transient errors. |
| `dynamodb.statePollInterval` | Duration | `5s` | Polling interval for `WatchCounterInt64`. |
| `dynamodb.lockRetryInterval` | Duration | Not set in defaults (no default in `SetDefaults` for DynamoDB) — uses whatever the zero value is, effectively controlled in the loop | Interval between lock acquisition retries. Note: `defaults.go` for Redis sets `500ms`; DynamoDB has no explicit default — check `data/dynamodb.go:L673` for the retry logic using `d.lockRetryInterval`. |

Note: `DynamoDBConnectorConfig.SetDefaults` does NOT set `LockRetryInterval` (`common/defaults.go:L1061-L1099`). The zero duration is used in the `time.After(d.lockRetryInterval)` sleep, meaning retries happen as fast as the DynamoDB API responds.

#### gRPC connector (`common/config.go:L354-L359`, defaults `common/defaults.go:L927-L929`)

| YAML path | Type | Default | Behavior |
|-----------|------|---------|----------|
| `grpc.bootstrap` | string | — | HTTP URL returning a JSON array or newline-separated list of gRPC server URLs. Fetched once at startup. |
| `grpc.servers` | []string | — | Explicit list of gRPC server URLs. Combined with bootstrap if both are provided. |
| `grpc.headers` | map[string]string | — | Headers to set on every gRPC client (`cli.SetHeaders`). |
| `grpc.getTimeout` | Duration | `100ms` | Per-Get call timeout. Applied only when it's shorter than the existing context deadline. |

#### Timeout buffer constants (`data/timeout_constants.go:L7-L15`)

| Constant | Value | Usage |
|----------|-------|-------|
| `DefaultOperationBuffer` | `10s` | Time reserved after lock acquisition for in-lock operations; used by Redis lock retry budget calculation (`data/redis.go:L467`). |
| `PollOperationBuffer` | `15s` | Reserved in state poller after lock acquisition. |
| `MinPollTimeout` | `30s` | Minimum timeout for a complete poll cycle. |

### Behaviors & algorithms

#### Key encoding

All connectors encode the primary key as `<partitionKey>:<rangeKey>` for Get/Set. The `<partitionKey>` for EVM cache entries typically starts with `evm:` (e.g., `evm:1:0x12345678`). For Redis/Memory, the reverse index key is: `rvi#<wildcardPartitionKey>#<rangeKey>` where `wildcardPartitionKey = "evm:<chainId>:*"`.

#### Reverse index pattern

Applicable to memory, Redis, and PostgreSQL connectors. When a Get uses `index == ConnectorReverseIndex` and the partition key ends with `*` (wildcard), the connector first attempts to resolve the exact partition key through the reverse index (avoiding expensive SCAN/wildcard queries). DynamoDB uses a real GSI for this.

The reverse index is NOT maintained for partition keys that already end with `*` (i.e., only concrete keys get indexed). Reverse index entries inherit the same TTL as the main entry.

#### TTL behavior per connector

| Connector | Enforcement | Mechanism |
|-----------|-------------|-----------|
| Memory | Exact (in-process clock) | ristretto `SetWithTTL` |
| Redis | Exact (Redis server clock) | SET with `EXPIRE` / `EX` parameter |
| PostgreSQL | Exact (DB server clock) | `expires_at` column; `WHERE expires_at IS NULL OR expires_at > NOW()` |
| DynamoDB | Eventual (AWS native TTL, up to ~48h lag) + client-side check | `<ttlAttributeName>` numeric epoch; client-side check in Get returns `ErrRecordExpired` if expired |
| gRPC | N/A (read-only, no writes) | — |

#### Redis vs. PostgreSQL: URI-embedded timeout interaction

Redis and PostgreSQL differ in how URI-embedded timeout parameters interact with the explicit timeout config fields:

- **Redis**: if `initTimeout`, `getTimeout`, or `setTimeout` are zero (not configured), `connectTask` falls back to `DialTimeout`, `ReadTimeout`, and `WriteTimeout` parsed from the Redis URI (`data/redis.go:L138-L146`). This means Redis `rediss://host:6379?dial_timeout=3s&...` parameters are respected when the explicit fields are left at zero.
- **PostgreSQL**: the three timeout fields (`initTimeout`, `getTimeout`, `setTimeout`) are **always** applied as explicit `context.WithTimeout` deadlines. pgx parses `connectionUri` for pool size and authentication parameters only; it does not populate timeout fields from the URI into the connector struct. Setting `connect_timeout=5` in the PostgreSQL connection string does NOT override `postgresql.initTimeout`. Users must configure all three timeouts explicitly in YAML. (`data/postgresql.go:L130-L134`, `data/postgresql.go:L186`)

#### DynamoDB TTL timing edge case

DynamoDB native TTL is eventually consistent: an expired item MAY still be returned by GetItem for up to ~48 hours. The connector guards against this with a client-side epoch comparison (`data/dynamodb.go:L547-L556`). For the reverse-index Query path, a `FilterExpression` pre-filters server-side PLUS the first non-expired item is chosen client-side (`data/dynamodb.go:L474-L503`).

#### PostgreSQL pool lifecycle

`connectTask` holds the write lock only for the field swap (nanoseconds). Schema setup and pool creation happen OUTSIDE the lock. Old pool is closed AFTER releasing the lock. This was redesigned post the 2026-05-13 incident where holding connMu.Lock for the entire body of connectTask (~5s) serialized every Get/Set behind each reconnect attempt (`data/postgresql.go:L176-L250`).

#### PostgreSQL connection failure coalescing

`handleConnectionFailure` uses an atomic int64 `lastFailureMarkNanos` with a 1s cooldown to prevent N concurrent failing goroutines from each calling `MarkTaskAsFailed` (which logs at Error level). Only one goroutine per second triggers the reconnect cascade (`data/postgresql.go:L725-L760`).

#### gRPC eth_blockNumber translation

The BDS gRPC protocol has no native `eth_blockNumber` RPC. The connector intercepts this method and calls `eth_getBlockByNumber("latest", false)` internally, then returns only the `number` field as a canonical lowercase hex string (e.g., `"0x1a2b3c"`). Zero block serializes as `"0x0"`. Leading zeros are normalized via uint64 round-trip. On failure, returns `ErrRecordNotFound` rather than an error, so the request falls through to real upstreams (`data/grpc.go:L265-L278`, `data/grpc_blocknumber_test.go:L65-L121`).

#### gRPC fast-miss rejection

Before forwarding, if `req.EvmBlockNumber()` returns a value and `earliestByNetwork[networkId] > 0` and `blockNumber < earliest`, the connector returns `ErrRecordNotFound` immediately without an RPC call. This avoids hitting the gRPC server for blocks that are definitely before its historical range (`data/grpc.go:L244-L254`).

#### FailsafeConnector executor selection

`pickCacheExecutor` (`data/failsafe.go:L159-L201`) reads `method` and `finality` from the request attached to `ctx.Value(common.RequestContextKey)`. If no request is in context (background prefetch, test), `method = ""` and `finality = 0`. The four-pass loop ensures the most specific match wins: (method+finality) > (method only) > (finality only) > (wildcard default).

#### Retry is transport-error-only

`isTransportError` (`data/failsafe.go:L25-L92`) is the definitive retryability gate. Critical: `ErrRecordNotFound`, `ErrRecordExpired`, `context.Canceled`, and `context.DeadlineExceeded` are all NOT transport errors. A cache miss does not cause a retry. A caller-cancelled context does not cause a retry.

#### ErrInvalidConnectorDriver — startup validation

`NewConnector` (`data/connector.go:L63-L103`) dispatches on `cfg.Driver`. If the driver string is not one of `memory`, `redis`, `postgresql`, `dynamodb`, `grpc` (or `mock` in test mode), it returns `common.NewErrInvalidConnectorDriver(cfg.Driver)` (`data/connector.go:L86`). This is a **startup-only** error: it surfaces during `NewEvmJsonRpcCache` initialization (`architecture/evm/json_rpc_cache.go:L43-L50`) or equivalent initialization of the shared-state/auth cache, causing the process to fail to start. It is never returned during live request processing. Error code: `ErrInvalidConnectorDriver` (`common/errors.go:L2281`). The `Details` map includes `"driver": <configured value>` so the misconfigured driver name appears in the error message (`common/errors.go:L2283-L2293`).

### Observability

#### Prometheus metrics

| Metric | Type | Labels | When emitted |
|--------|------|--------|-------------|
| `erpc_ristretto_cache_current_cost` | gauge | `connector` | Every 30s by memory connector metrics loop when `emitMetrics=true`. Bytes currently used. `telemetry/metrics.go:L628-L633` |
| `erpc_ristretto_cache_sets_failed_total` | counter | `connector` | Every 30s; delta of `SetsDropped + SetsRejected`. `telemetry/metrics.go:L634-L638` |
| `erpc_cache_connector_earliest_block_number` | gauge | `connector`, `network` | Set by `pollBlockHeadsOnce` every 60 seconds when `fetchTaggedBlock(ctx, cli, "earliest")` succeeds. Value: block number as float64. Drives fast-miss rejection in `GrpcConnector.Get` — requests for block numbers below this value are rejected without an RPC call. Not updated on transient poll failure (value remains at last known good). `telemetry/metrics.go:L73-L77`, `data/grpc.go:L342` |
| `erpc_cache_connector_latest_block_number` | gauge | `connector`, `network` | Set every 60 seconds when `fetchTaggedBlock(ctx, cli, "latest")` succeeds. Value: latest block number as float64. Paired with `erpc_cache_connector_latest_block_timestamp_seconds` to track the gRPC connector's head position. `telemetry/metrics.go:L79-L83`, `data/grpc.go:L358` |
| `erpc_cache_connector_finalized_block_number` | gauge | `connector`, `network` | Set every 60 seconds when finalized block is fetchable. `telemetry/metrics.go:L85-L89`, `data/grpc.go:L368` |
| `erpc_cache_connector_earliest_block_timestamp_seconds` | gauge | `connector`, `network` | Unix timestamp (seconds) of the earliest block. Set only when block timestamp > 0. `telemetry/metrics.go:L91-L95`, `data/grpc.go:L344` |
| `erpc_cache_connector_latest_block_timestamp_seconds` | gauge | `connector`, `network` | Unix timestamp (seconds) of the latest block. Set only when block timestamp > 0. **This is the value that drives the real-time cache freshness gate**: `CacheLatestBlockTimestamp` reads `latestTsByNetwork`, which is updated in lockstep with this gauge. When the gauge is zero (unknown), the freshness gate fails open (accepts cached results). Alert on this value lagging wall-clock time by more than your expected block interval to detect gRPC connector staleness. `telemetry/metrics.go:L97-L101`, `data/grpc.go:L360` |
| `erpc_cache_connector_finalized_block_timestamp_seconds` | gauge | `connector`, `network` | Unix timestamp (seconds) of the finalized block. Set only when block timestamp > 0. `telemetry/metrics.go:L103-L107`, `data/grpc.go:L370` |

Cache set/get metrics (`erpc_cache_set_*`, `erpc_cache_get_*`) are labeled with `connector` but are emitted at the cache policy layer (not the connector layer itself). See KB for cache policies.

#### Trace spans

| Span name | Connector | Notes |
|-----------|-----------|-------|
| `RedisConnector.Set` | Redis | Attributes: `partition_key`, `range_key`, `value_size` (when detailed tracing) |
| `RedisConnector.Get` | Redis | Attributes: `index`, `partition_key`, `range_key`, `value_size` |
| `RedisConnector.Delete` | Redis | |
| `RedisConnector.List` | Redis | Attributes: `index`, `limit` |
| `RedisConnector.Lock` | Redis | Attributes: `lock_key`, `ttl_ms` |
| `RedisConnector.Unlock` | Redis | Attributes: `lock_key` |
| `RedisConnector.PublishCounterInt64` | Redis | Attributes: `key`, `value`, `updated_at`, `updated_by` |
| `DynamoDBConnector.Set` | DynamoDB | |
| `DynamoDBConnector.Get` | DynamoDB | |
| `DynamoDBConnector.Delete` | DynamoDB | |
| `DynamoDBConnector.List` | DynamoDB | |
| `DynamoDBConnector.Lock` | DynamoDB | |
| `DynamoDBConnector.Unlock` | DynamoDB | |
| `DynamoDBConnector.getSimpleValue` | DynamoDB | Detail span |
| `PostgreSQLConnector.Set` | PostgreSQL | |
| `PostgreSQLConnector.Get` | PostgreSQL | |
| `PostgreSQLConnector.Delete` | PostgreSQL | |
| `PostgreSQLConnector.List` | PostgreSQL | |
| `PostgreSQLConnector.Lock` | PostgreSQL | |
| `PostgreSQLConnector.Unlock` | PostgreSQL | |
| `PostgreSQLConnector.PublishCounterInt64` | PostgreSQL | |
| `PostgreSQLConnector.getCurrentValue` | PostgreSQL | Detail span |
| `PostgreSQLConnector.getWithWildcard` | PostgreSQL | Detail span |
| `ConnectorFailsafe.Get` | FailsafeConnector | Attributes: `connector.id`, `connector.partition_key`, `connector.range_key`, `failsafe.match_method`, `result.bytes` (or `error.summary`) |
| `ConnectorFailsafe.Set` | FailsafeConnector | Attributes: `connector.id`, `connector.partition_key`, `connector.range_key`, `failsafe.match_method`, `value.bytes`, `ttl.ms` |
| `ConnectorFailsafe.Delete` | FailsafeConnector | |

#### Notable log messages

- `"redis is not connected (state: %s), errors: %v"` — Redis checkReady failure (every Get/Set/Lock attempt when not ready)
- `"detected critical connection failure, marking for reconnection"` — Redis `markConnectionAsLostIfNecessary` fires (Warn)
- `"successfully connected to Redis"` — logged at Info after ping succeeds
- `"postgres connection lost; marking connector as failed for reinitialization"` — PostgreSQL handleConnectionFailure triggers reconnect (Warn)
- `"postgres connection error during reinit; not re-marking"` — PostgreSQL already retrying, suppressed (Debug)
- `"successfully connected to postgres"` — logged at Info after pool swap
- `"migrating value column from TEXT to BYTEA"` / `"successfully migrated value column to BYTEA"` — PostgreSQL schema migration (Info)
- `"successfully configured pg_cron cleanup job"` — pg_cron enabled (Info)
- `"starting local expired items cleanup routine"` — PostgreSQL local cleanup, no pg_cron (Debug)
- `"List operation on MemoryConnector is not efficiently supported"` — memory List (Warn)
- `"gRPC client initialized for network"` — after chain probe succeeds (Info)
- `"duplicate gRPC server for network detected; ignoring"` — two servers with same chainId (Error)
- `"Starting Ristretto metrics collection loop"` — memory connector, emitMetrics=true (Info)

### Edge cases & gotchas

1. **Memory `List` is not implemented.** Ristretto has no iteration API. Calling `List` on a MemoryConnector returns an error. Any subsystem that needs `List` (e.g., admin endpoints) must use Redis, PostgreSQL, or DynamoDB. (`data/memory.go:L316-L321`)

2. **Memory reverse index has no TTL.** When a main entry is stored with a TTL, the reverse index entry is stored without TTL (`data/memory.go:L128-L130`). After the main entry expires, the reverse index entry remains until evicted by Ristretto's cost-based eviction. A subsequent `Get` via reverse index that hits the stale pointer will call `cache.Get(mainKey)` and get a miss, which is handled correctly. Redis does propagate the TTL to the reverse index entry (`data/redis.go:L330`).

3. **DynamoDB TTL lag (~48h).** AWS DynamoDB TTL auto-expire is eventually consistent. Items past their TTL CAN be returned by GetItem. The connector applies a client-side time check and returns `ErrRecordExpired` for such items, but callers need to distinguish `ErrRecordExpired` from `ErrRecordNotFound` because expired items may have stale but technically existing data. (`data/dynamodb.go:L547-L556`)

4. **DynamoDB `LockRetryInterval` has no default — set it explicitly in production.** `DynamoDBConnectorConfig.SetDefaults` does not set `LockRetryInterval` (`common/defaults.go:L1061-L1099`). A zero duration means `time.After(0)` fires immediately (`data/dynamodb.go:L673`), so under lock contention the retry loop spins as fast as the DynamoDB HTTP API responds — potentially hundreds of retries per second when many pods compete for the same lock. This creates significant DynamoDB request volume and may trigger `ProvisionedThroughputExceededException` on tables that are not on PAY_PER_REQUEST. **Recommendation: always set `dynamodb.lockRetryInterval: 100ms` for production deployments** to cap retries at ~10/s per contended lock. Contrast with Redis where it defaults to `500ms`. (`data/dynamodb.go:L660-L688`)

5. **PostgreSQL advisory lock is non-blocking.** `pg_try_advisory_xact_lock` returns immediately with `false` if the lock is held. The caller receives an error. There is no retry loop for PostgreSQL locks. Callers (shared state machinery) must implement their own retry. (`data/postgresql.go:L558-L573`)

6. **PostgreSQL lock and pool coupling.** The `postgresLock` holds an open transaction on an acquired pool connection. Calling `Unlock` commits that transaction; on commit failure, rollback is attempted. The pool connection is NOT explicitly released — pgxpool handles this at transaction commit/rollback. (`data/postgresql.go:L588-L618`)

7. **gRPC connector supports no writes.** `Set`, `Delete`, `List`, `Lock`, `WatchCounterInt64`, `PublishCounterInt64` all return errors. If this connector is configured as the only connector for a write-through cache policy, all writes will silently fail. Use it only as a read-through layer before a write-capable connector. (`data/grpc.go:L454-L478`)

8. **gRPC bootstrap is not retried on failure.** If `fetchGrpcServers` fails, `NewGrpcConnector` returns an error. Individual server connection tasks ARE retried by `util.Initializer`. (`data/grpc.go:L88-L95`)

9. **FailsafeConnector does not wrap `List`, `Lock`, `WatchCounterInt64`, or `PublishCounterInt64`.** These four operations bypass retry/hedge/breaker. Only `Get`, `Set`, and `Delete` get failsafe treatment. (`data/failsafe.go:L308-L322`)

10. **Hedge quantile is rejected for connectors.** Unlike upstream-level hedging, connector-level hedging has no per-method latency samples, so `hedge.delay.quantile` > 0 fails validation. Only static base delays work. (`data/cache_executor.go:L37-L42`, `data/failsafe_test.go:L29-L42`)

11. **Redis `addr` fields are cleared after URI construction.** After `SetDefaults`, if `addr` was used, `addr`, `username`, `password`, and `db` are set to empty/zero and `uri` holds the constructed URL. Serialization (MarshalJSON/MarshalYAML) redacts the password and calls `util.RedactEndpoint` on the URI. (`common/defaults.go:L979-L1015`)

12. **Redis TLS merging.** When `rediss://` URI + `tls.enabled: true` are both present, YAML cert/CA overrides merge onto the URI-derived baseline and disable `InsecureSkipVerify`. When only `redis://` + `tls.enabled: true`, the YAML TLS config is used wholesale. A `rediss://`-only URI produces InsecureSkipVerify=true unless YAML config overrides it. (`data/redis.go:L156-L180`)

13. **PostgreSQL `cron.schedule` is not idempotent.** Each call inserts a new row in `cron.job`. `applySchema` is gated by `schemaApplied` (set only on first successful run per process) to prevent duplicate cron jobs on reconnect. (`data/postgresql.go:L257-L268`, `data/postgresql.go:L354-L369`)

14. **Memory cost function includes 256-byte overhead.** The `Cost` function returns `len(value) + 256`, meaning even a 1-byte value costs 257 bytes against `MaxCost`. Small values are "more expensive" relative to their actual byte count. The overhead models ristretto's internal metadata. (`data/memory.go:L78-L80`)

15. **Redis reconnect fires only for a narrow allowlist.** `markConnectionAsLostIfNecessary` does NOT fire for most errors. It only fires for: `"connection refused"`, `"no such host"`, `"network is unreachable"`, `"connection reset by peer"`, `"broken pipe"`, `"invalid connection"`, `"connection closed"`. Other errors are logged at Debug and go-redis handles internally. (`data/redis.go:L240-L256`)

16. **gRPC head poller does not fire at startup; first poll is at T+60s.** `startBlockHeadPoller` uses `time.NewTicker(60s)` which fires on the first tick after 60 seconds. The 6 block-head gauges are zero and `latestTsByNetwork` is empty until the first poll completes. During this window `CacheLatestBlockTimestamp` returns `ok=false` and the realtime age guard **fails open** — stale realtime responses may be served if they are already cached. (`data/grpc.go:L302-L313`)

17. **gRPC freshness gate: response timestamp takes precedence over connector head.** `shouldAcceptCachedResult` first extracts a block timestamp from the cached response itself. Only if the response carries no timestamp does it fall back to `CacheLatestBlockTimestamp`. So for `eth_getBlockByNumber` (which returns a full block with a timestamp), the response timestamp is used regardless of the connector's polled head — even if the polled head looks stale but the response is fresh, the result is accepted. Tests confirm this precedence (`architecture/evm/json_rpc_cache_connector_freshness_test.go:L156-L162`).

18. **gRPC freshness gate fails open when connector is not head-aware.** For non-gRPC connectors (memory, Redis, PostgreSQL, DynamoDB), the serving connector does not implement `CacheHeadReporter`. When `eth_gasPrice` or `eth_blockNumber` is fetched from one of these connectors, `shouldAcceptCachedResult` cannot determine the response age and unconditionally accepts the cached result. Tests confirm fail-open behavior (`architecture/evm/json_rpc_cache_connector_freshness_test.go:L142-L151`).

19. **gRPC block-head poller: zero `latestTs` means "unknown", not "block at genesis".** When `eth_getBlockByNumber("latest")` returns a block with a zero or absent `timestamp` field, `latestTsByNetwork[nid]` is stored as 0 and `CacheLatestBlockTimestamp` returns `ok=false`. The age guard then fails open. This design intentionally prefers serving a cached result over falsely rejecting it due to a missing timestamp. (`data/grpc.go:L349-L357`)

20. **`ErrInvalidConnectorDriver` is startup-only; never returned at runtime.** This error is produced only by `NewConnector` during initialization — if the YAML configures an unknown `driver` string. It propagates up through `NewEvmJsonRpcCache` (or shared-state/auth cache init) and causes process startup failure. It cannot be triggered by a live request. Error code string: `"ErrInvalidConnectorDriver"` (`common/errors.go:L2281`).

21. **FailsafeConnector wrapping does not break CacheHeadReporter.** `NewConnector` wraps the connector in `FailsafeConnector` when `failsafeForGets` or `failsafeForSets` is non-empty. Since `FailsafeConnector` forwards `CacheLatestBlockTimestamp` to the wrapped connector (`data/failsafe.go:L211-L216`), the freshness gate works correctly even in the typical production setup where the gRPC connector has failsafe policies. Tests confirm (`architecture/evm/json_rpc_cache_connector_freshness_test.go:L181-L211`).

22. **`dynamodb.auth.accessKeyID` YAML key is case-sensitive (capital `I` and `D`).** The struct tag is `yaml:"accessKeyID"` (`common/config.go:L483`). Using `accessKeyId` (lowercase `d`) silently skips the field, leaving `AccessKeyID` empty; AWS will then fall through to the default credential chain rather than using the intended static key. This is the same `AwsAuthConfig` struct shared with the consensus S3 `misbehaviorsDestination` config.

23. **PostgreSQL timeouts cannot be set via `connectionUri`.** Unlike Redis (which falls back to `DialTimeout`/`ReadTimeout`/`WriteTimeout` from the parsed URI when explicit fields are zero), PostgreSQL's three timeout fields (`initTimeout`, `getTimeout`, `setTimeout`) are always applied as explicit Go context deadlines. Setting `connect_timeout=5` in the PostgreSQL DSN does NOT populate `initTimeout`. Users must set all three fields explicitly in YAML. (`data/postgresql.go:L130-L134`, `data/postgresql.go:L186`; contrast with `data/redis.go:L138-L146`)

### Source map

- `data/connector.go` — `Connector` interface definition, `DistributedLock`, `KeyValuePair`, `CounterInt64State`, `CacheHeadReporter`, `NewConnector` factory
- `data/memory.go` — ristretto-backed in-process cache; TTL via ristretto; reverse index; no-op pub/sub; TryLock-based distributed lock; background metrics collection
- `data/redis.go` — Redis connector; go-redis client; redsync distributed locking; RedisPubSubManager for pub/sub; URI construction; TLS merging; narrow reconnect detection
- `data/redis_pubsub_manager.go` — Self-healing pubsub manager; transparent reconnection; copy-on-write subscriber management
- `data/postgresql.go` — pgxpool-based connector; schema migration (TEXT→BYTEA, expires_at, indexes); pg_cron or local cleanup; advisory transaction locks; LISTEN/NOTIFY pub/sub with fallback polling; connection failure coalescing
- `data/dynamodb.go` — DynamoDB connector; split read/write HTTP/2 clients; PAY_PER_REQUEST table creation; GSI for reverse index; native TTL + client-side expiry check; conditional PutItem locking; polling-based WatchCounterInt64
- `data/grpc.go` — Read-only gRPC BDS connector; bootstrap discovery; chain probing; fast-miss rejection; eth_blockNumber translation; 60s background head poller; CacheHeadReporter (`CacheLatestBlockTimestamp`)
- `architecture/evm/json_rpc_cache.go:L834-L924` — `shouldAcceptCachedResult`: realtime age guard; response-timestamp path then connector-fallback path via `CacheHeadReporter`; fail-open logic
- `architecture/evm/json_rpc_cache_connector_freshness_test.go` — full test coverage for freshness gate: response-timestamp path, connector-fallback path, FailsafeConnector wrapping, fail-open cases, precedence
- `data/failsafe.go` — FailsafeConnector wrapper; pickCacheExecutor four-tier selection; isTransportError retryability classification; CacheHeadReporter forwarding
- `data/cache_executor.go` — cacheExecutor: retry/hedge/breaker/timeout pipeline; transport-error-only retry; cacheBreakerOutcome classification; consensus/hedge-quantile rejection
- `data/timeout_constants.go` — DefaultOperationBuffer (10s), PollOperationBuffer (15s), MinPollTimeout (30s)
- `common/config.go:L341-L487` — Config structs: ConnectorConfig, MemoryConnectorConfig, RedisConnectorConfig, DynamoDBConnectorConfig, PostgreSQLConnectorConfig, GrpcConnectorConfig, TLSConfig, AwsAuthConfig
- `common/defaults.go:L846-L1099` — SetDefaults for all connector configs; driver inference; default table names by scope; timeout/pool defaults
- `common/validation.go:L414-L576` — Validate for ConnectorConfig, DynamoDBConnectorConfig, PostgreSQLConnectorConfig, RedisConnectorConfig, MemoryConnectorConfig
- `telemetry/metrics.go:L73-L107`, `L628-L638` — Connector-specific Prometheus metrics (gRPC block gauges, ristretto cost/failure)
