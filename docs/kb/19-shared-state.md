# KB: Shared state & distributed coordination (redis pubsub)

> status: complete
> source-dirs: data/shared_state_registry.go, data/shared_state_variable.go, data/shared_state_sources.go, data/redis_pubsub_manager.go, data/redis.go, data/postgresql.go, data/dynamodb.go, data/memory.go, data/connector.go, common/config.go, common/defaults.go, common/validation.go, architecture/evm/evm_state_poller.go, erpc/erpc.go, erpc/init.go, erpc/networks.go, telemetry/metrics.go

## L1 — Capability (CTO view)

eRPC's shared-state layer gives a horizontally-scaled fleet of eRPC pods a single logical view of per-upstream monotonic counters (latest block, finalized block, earliest block, served-tip). Every pod maintains a local atomic copy that is always readable at zero cost; it is kept in sync with its peers through a background publish-then-reconcile pipeline that never blocks the request path. The backing store is pluggable (Redis, PostgreSQL, DynamoDB, in-process memory) and is configured under `database.sharedState` in `erpc.yaml`. Without a shared-state config, eRPC silently falls back to a process-local in-memory registry that provides the same API but no cross-pod coordination.

## L2 — Mechanics (staff-engineer view)

**Registry and key space.** `NewSharedStateRegistry` wraps a single `Connector` and a `clusterKey` prefix (`data/shared_state_registry.go:L38-L80`). Every counter key is stored as `<clusterKey>/<key>` — e.g. `my-cluster/latestBlock/projectX/evm/1/alchemy`. This prefix lets multiple logical eRPC clusters share the same backing store without colliding. The registry holds a `sync.Map` of live `*counterInt64` instances; calling `GetCounterInt64` with the same key always returns the same object (idempotent `LoadOrStore`, `data/shared_state_registry.go:L102-L123`).

**Counter lifecycle.** On first `GetCounterInt64` call two async bootstrap tasks are started via `util.Initializer`:
1. `buildInitialValueTask` — does one synchronous `connector.Get` to seed the local counter from the backing store (`data/shared_state_registry.go:L135-L166`).
2. `buildCounterSyncTask` — calls `connector.WatchCounterInt64` to open a long-lived change stream. Incoming `CounterInt64State` messages are fed into `counter.processNewState(UpdateSourceRemoteSync, …)` (`data/shared_state_registry.go:L168-L222`).

**The write path (TryUpdate).** `TryUpdate` is synchronous and local-only; it never touches the network (`data/shared_state_variable.go:L364-L388`). After a successful local CAS it calls `scheduleBackgroundPushCurrent()`.

**Background push / reconcile (scheduleBackgroundPushCurrent).** A single deduped goroutine per counter coalesces pushes:
1. Publishes current state to `connector.PublishCounterInt64` immediately (no lock — fast propagation to peers, `data/shared_state_variable.go:L660-L663`).
2. Acquires the distributed lock (`connector.Lock`) with a `lockMaxWait` budget (`data/shared_state_variable.go:L667-L674`). If the lock cannot be obtained quickly the goroutine continues the loop — it will retry if another `bgPushRequested` is pending.
3. Under the lock: reads remote state, adopts it if remote is newer, then writes local state if local is newer (`data/shared_state_variable.go:L681-L703`).
4. Dedup invariant: `bgPushInProgress` (a `sync.Mutex.TryLock`) ensures only one goroutine runs the loop at a time; `bgPushRequested` (an `atomic.Bool`) records whether a value change arrived while the goroutine was running so a second pass is scheduled (`data/shared_state_variable.go:L627-L707`).

**TryUpdateIfStale.** Used by the EVM state poller to debounce expensive RPC calls. It is double-checked against `IsStale(staleness)`, executes the update function in a background goroutine, and waits at most `updateMaxWait` for the result before returning the current stale value and continuing asynchronously (`data/shared_state_variable.go:L390-L469`). The foreground path acquires `updateMu` (thundering-herd guard) but NEVER waits on the distributed lock — that stays background-only.

**Rollback handling.** Both local (`processNewValue`) and remote (`processNewState`) paths enforce the same rollback policy (`data/shared_state_variable.go:L124-L349`):
- `ignoreRollbackOf = 0` — all decreases accepted (used for earliest-block counters; pruning genuinely shifts the anchor).
- `ignoreRollbackOf = 1024` (`DefaultToleratedBlockHeadRollback`) — decreases up to 1024 blocks are silently ignored as noise from lagging upstreams; larger gaps (real reorgs or provider failures) are accepted and trigger the `OnLargeRollback` callback.

For remote updates with a newer timestamp that would imply a small rollback, the local timestamp is advanced past the remote's timestamp so that the local (higher) value wins the next reconciliation (`data/shared_state_variable.go:L211-L227`).

**Timestamp ordering.** `CounterInt64State.UpdatedAt` is unix milliseconds. `allocateUpdatedAtMs` ensures the local timestamp is strictly monotonic even under concurrent goroutines (CAS spin-loop, `data/shared_state_variable.go:L51-L62`). Remote state is only accepted when `st.UpdatedAt > currentTs`, providing idempotency and protection against out-of-order network delivery (`data/shared_state_variable.go:L141-L153`).

**Redis pubsub transport.** `RedisPubSubManager` owns a single `PSubscribe("counter:*")` connection shared across all counters (`data/redis_pubsub_manager.go:L155`). It reconnects automatically with exponential backoff (1 s → 30 s with jitter) and a 5-minute periodic poll fallback (`data/redis_pubsub_manager.go:L254-L286`, `data/redis_pubsub_manager.go:L333-L357`). Subscriber channels are buffered (size 1, drop-on-full), so slow consumers never block the message loop (`data/redis_pubsub_manager.go:L442-L460`). Subscriber management uses a copy-on-write slice per key stored in a `sync.Map` (`data/redis_pubsub_manager.go:L404-L438`).

**Connector transport differences.**
- Redis: pubsub push via `PUBLISH counter:<key>` + `PSubscribe counter:*`; distributed lock via `redsync`.
- PostgreSQL: push via `pg_notify` with channel name derived from key, LISTEN-based subscription with 30 s polling fallback (`data/postgresql.go:L620-L679`, `data/postgresql.go:L682-L718`).
- DynamoDB: no push; polling only at `statePollInterval` (default 5 s, `data/dynamodb.go:L723-L762`). `PublishCounterInt64` is a documented no-op; propagation relies on the polling loop.
- Memory: both `WatchCounterInt64` and `PublishCounterInt64` are no-ops (`data/memory.go:L195-L207`); in-process only.
- gRPC connector: both methods return an error; it cannot participate in shared state.

**State stored per counter.** A single key stores a JSON object `{"v":<int64>,"t":<unix_ms>,"b":"<instance_id>"}` under `ConnectorMainIndex` / field `"value"`. The `UpdatedAt` field (`t`) doubles as both a vector clock for conflict resolution and a staleness indicator (`data/connector.go:L28-L38`).

**What is shared.** Four counter families are materialised during normal operation:
1. `latestBlock/<projectId>/<arch>/<networkId>/<upstreamId>` — latest block per upstream (ignoreRollbackOf=1024).
2. `finalizedBlock/<projectId>/<arch>/<networkId>/<upstreamId>` — finalized block per upstream (ignoreRollbackOf=1024).
3. `earliestBlock/<projectId>/<arch>/<networkId>/<upstreamId>/<probeType>` — earliest available block per upstream per probe type (ignoreRollbackOf=0).
4. `servedLatestBlock/<networkId>/<grp:<hash>>` and `servedFinalizedBlock/<networkId>/<grp:<hash>>` — cross-pod monotonic served-tip per tag-group, capped at 16 partitions per network (`erpc/networks.go:L84`).

**Lock/deadlock protection.** The old design had a deadlock: `scheduleBackgroundPushCurrent` would acquire the distributed lock then try `updateMu`, while `TryUpdateIfStale` would acquire `updateMu` then try the distributed lock. The fix: `TryUpdate` and `TryUpdateIfStale` (foreground) NEVER touch the distributed lock; only the background `scheduleBackgroundPushCurrent` goroutine acquires it — after finishing any local updates (`data/shared_state_variable.go:L376-L388`, `data/shared_state_variable.go:L624-L707`). Test: `data/shared_state_variable_deadlock_test.go:L95-L212`.

**Instance ID resolution.** The `UpdatedBy` field is populated with the first non-empty value from: env `INSTANCE_ID` → env `POD_NAME` → env `HOSTNAME` → `os.Hostname()` → `"unknown"` (`data/shared_state_registry.go:L82-L98`). This is diagnostic-only.

**Fallback when shared state is nil.** `erpc/erpc.go:L49-L66` synthesises an in-memory registry if `sharedState == nil`; the same `GetCounterInt64` API works identically but no cross-pod propagation occurs. This means eRPC can start without any `database.sharedState` config and will simply track block numbers per-process.

## L3 — Exhaustive reference (agent view)

### Config fields

All fields live under `database.sharedState` in `erpc.yaml`. `database.sharedState` is optional; its absence causes a silent fallback to process-local memory.

| Full YAML path | Type | Default | Valid values / constraints | Behavior | Source |
|---|---|---|---|---|---|
| `database.sharedState` | `*SharedStateConfig` | `nil` | Present or absent | **Root pointer — nil by default at the config level.** `DatabaseConfig.SetDefaults` only calls `SharedStateConfig.SetDefaults` when the block is non-nil (`common/defaults.go:L837-L840`). When nil at runtime, `erpc/erpc.go:L49-L66` synthesises a fresh `SharedStateConfig` with a memory connector and calls `SetDefaults` on it, creating a process-local in-memory registry. The same `GetCounterInt64` API is available in both cases; the only difference is no cross-pod propagation. | `common/config.go:L266`, `common/defaults.go:L837-L841`, `erpc/erpc.go:L49-L66` |
| `database.sharedState.clusterKey` | string | Inherits from top-level `clusterKey` (default `"erpc-default"`) | Any non-empty string | Prefix prepended to every counter key: `<clusterKey>/<key>`. Lets multiple logical clusters share the same Redis/PG instance without collision. Multi-tenant deployments must use distinct cluster keys. | `common/config.go:L271`, `common/defaults.go:L803-L804` |
| `database.sharedState.connector` | `*ConnectorConfig` | Auto-defaulted to in-memory (`id="memory"`, `driver="memory"`) when `database.sharedState` block is present but `connector` is omitted. When `database.sharedState` is nil the entire fallback registry uses an identical in-memory config. | Memory, Redis, PostgreSQL, DynamoDB | The backing store for counter persistence and pub/sub. Each driver has its own sub-key (`connector.driver`, `connector.redis`, etc.). **ID-assignment rule (GOTCHA — differs from cache/auth connectors):** when the user supplies a `connector` block but omits `id`, the id is set to `string(connector.driver)` (e.g. driver `"redis"` → id `"redis"`). This is done at `common/defaults.go:L799-L801` *before* calling `ConnectorConfig.SetDefaults`, which would otherwise produce `"shared-state-redis"` (the `scope + "-" + driver` pattern used by cache and auth connectors: `common/defaults.go:L847-L849`). The result is a shorter id (`"redis"` vs `"shared-state-redis"`). If you copy a connector block from a `database.evmJsonRpcCache` scope (which would auto-assign `"cache-redis"`) into `database.sharedState` and rely on auto-assigned ids, the ids will differ. Always set `connector.id` explicitly to avoid surprises. See also edge case #15. | `common/config.go:L273`, `common/defaults.go:L790-L801`, `common/defaults.go:L847-L849` |
| `database.sharedState.fallbackTimeout` | Duration | `3s` | ≥ 100 ms | Timeout for **background** remote I/O operations (Get, Set, Publish). Has two usages: (1) the context deadline passed to every remote call in the background push/reconcile goroutine (`data/shared_state_variable.go:L426`); (2) the hard cap on the `continueAsyncRefresh` helper goroutine's lifetime — that goroutine is bounded to `fallbackTimeout + 1 s` (`data/shared_state_variable.go:L511`) so that a downstream RPC that ignores its context cannot leak the goroutine permanently. **This field does NOT bound request-path latency.** The foreground latency knob is `updateMaxWait` (see below); `TryUpdateIfStale` returns to the caller after at most `updateMaxWait`, regardless of how long the background RPC takes. Tuning `fallbackTimeout` for Redis latency alone (e.g. setting it very low) will cause background pushes and reconciliations to time out, degrading cross-pod propagation — it does not speed up responses to clients. | `common/config.go:L276`, `common/defaults.go:L809-L812`, `data/shared_state_variable.go:L426`, `data/shared_state_variable.go:L511` |
| `database.sharedState.lockTtl` | Duration | `4s` | ≥ 1 s, ≥ fallbackTimeout | Expiry of the distributed lock key in the backing store. Must exceed fallbackTimeout to allow one full Get+Set cycle under the lock. If a lock expires while the goroutine is still working, the unlock call logs "lock was already expired" at debug level (expected). | `common/config.go:L279`, `common/defaults.go:L814-L817`, `common/validation.go:L292-L295`, `data/shared_state_variable.go:L549-L553` |
| `database.sharedState.lockMaxWait` | Duration | `100ms` | > 0, < fallbackTimeout | Maximum time the background push goroutine will wait to acquire the distributed lock before giving up and relying on publish-only propagation. Background-only; never blocks request flow. | `common/config.go:L282`, `common/defaults.go:L820-L823`, `common/validation.go:L298-L301`, `data/shared_state_variable.go:L667-L668` |
| `database.sharedState.updateMaxWait` | Duration | `50ms` | > 0, < fallbackTimeout | **The foreground latency budget for block-number refreshes.** `TryUpdateIfStale` launches the refresh RPC in a goroutine and then waits at most `updateMaxWait` before returning the stale value to the caller and letting the refresh continue as a background goroutine (`data/shared_state_variable.go:L447-L468`). This is the primary knob controlling tail latency on the request path when block-number polling occurs. Contrast with `fallbackTimeout` (above): `fallbackTimeout` is the deadline given to the background RPC itself; `updateMaxWait` is how long the foreground caller will wait before giving up on that RPC. A value of `0` (not allowed by validation) would mean always async; the default `50ms` means fast Redis responses (typically <5 ms) complete synchronously, but slow responses are cut off and deferred. Must be less than `fallbackTimeout`. | `common/config.go:L285`, `common/defaults.go:L824-L827`, `common/validation.go:L303-L306`, `data/shared_state_variable.go:L447` |

**Top-level `clusterKey` field** (`common/config.go:L40`): defaults to `"erpc-default"`, propagated to `database.sharedState.clusterKey` if the latter is unset.

**DynamoDB-specific `statePollInterval`** (`common/config.go:L441`): default `5s` (`common/defaults.go:L1095-L1096`). Controls the polling rate inside `WatchCounterInt64` for DynamoDB since there is no pub/sub primitive available.

### Behaviors & algorithms

**Key naming.** `GetCounterInt64(key, ignoreRollbackOf)` stores the counter under `<clusterKey>/<key>` in the sync.Map and as the storage key in the connector (`data/shared_state_registry.go:L101-L103`). Callers use unqualified keys like `latestBlock/<upstreamUniqueKey>` and rely on the registry to prepend the cluster key.

**`UniqueUpstreamKey` key derivation.** The per-upstream counter key is `latestBlock/<common.UniqueUpstreamKey(up)>` which hashes the upstream's project/network/upstream triple (`common/upstream.go:L62-L65`). This prevents collisions between upstreams with the same ID in different networks/projects.

**Counter JSON payload.** `CounterInt64State` is serialised with sonic (JSON):
```json
{"v": 19500000, "t": 1718000000123, "b": "pod-a-7f8d9"}
```
Fields: `v` = counter value, `t` = unix milliseconds (vector clock), `b` = instance ID. `UpdatedAt=0` indicates uninitialised state; callers treat it as "no value" (`data/connector.go:L28-L38`).

**Redis channel naming.** Publish key is `counter:<fullKey>` e.g. `counter:my-cluster/latestBlock/...`. The manager subscribes with `PSubscribe("counter:*")` matching all counters regardless of cluster (`data/redis.go:L591`, `data/redis_pubsub_manager.go:L155`). Subscribers filter by exact key in `runMessageLoop` (`data/redis_pubsub_manager.go:L309`).

**PostgreSQL channel naming.** The channel is derived by `sanitizeChannelName(fmt.Sprintf("counter_%s", key))` to strip characters illegal in PostgreSQL NOTIFY channel names. Notification payload is the same JSON as Redis (`data/postgresql.go:L706-L712`).

**Rollback thresholds used in practice.**
- `ignoreRollbackOf = 1024` for `latestBlock`, `finalizedBlock`, `servedLatestBlock`, `servedFinalizedBlock` (`architecture/evm/evm_state_poller.go:L27`, `L108-L109`, `erpc/networks.go:L434-L435`).
- `ignoreRollbackOf = 0` for `earliestBlock` — the anchor can genuinely decrease when a node prunes historical state (`architecture/evm/evm_state_poller.go:L609`).

**`IsStale(d)` semantics.** Returns `true` if `updatedAtUnixMs <= 0` (never updated) or if `time.Since(time.UnixMilli(updatedAtUnixMs)) > d`. Used by `TryUpdateIfStale` as the gate before attempting a refresh (`data/shared_state_variable.go:L37-L43`).

**`OnValue` / `OnLargeRollback` callbacks.** Registered at construction time; called synchronously from `triggerValueCallback` within both `processNewValue` and `processNewState` after a successful CAS. The EVM state poller registers `OnValue` to push the new block number into the `health.Tracker` for metrics; `OnLargeRollback` triggers `tracker.RecordBlockHeadLargeRollback` (`architecture/evm/evm_state_poller.go:L126-L139`).

**`SuggestLatestBlock` fast path.** When the eRPC proxy observes a block number in a response, it calls `SuggestLatestBlock(blockNumber)` which calls `TryUpdate` if the new value is greater than the current local value. This avoids a polling round-trip and immediately triggers background propagation (`architecture/evm/evm_state_poller.go:L459-L481`).

**Served-tip partition logic.** Each tag/group that constitutes a real sub-group of the network's upstreams (≥2 matched, <all matched, simple glob selector) gets its own `servedTipPartition`. The partition key is `"grp:" + hex(sha256(sorted_upstream_ids)[:8])` — equivalent selectors map to the same key. Capped at 16 partitions per network. Counter keys stored as `servedLatestBlock/<networkId>/grp:<hash>` and `servedFinalizedBlock/<networkId>/grp:<hash>` (`erpc/networks.go:L412-L486`).

**Reconnect / startup ordering.** The `RedisPubSubManager` is started eagerly on creation; construction failures are non-fatal (subscribe calls will retry via `start()`). After each reconnect `pollAllKeys()` is triggered to close the gap during the reconnection window (`data/redis_pubsub_manager.go:L58-L71`, `data/redis_pubsub_manager.go:L173-L176`).

**Bootstrap task retry.** If `WatchCounterInt64` fails at startup (connector not ready, network error), `initCounterSync` marks the `counterSync/<key>` task as failed and the `Initializer` retries in the background. During this window the counter operates in local-only mode (`data/shared_state_registry.go:L168-L222`).

**Poll timeout derivation in EVM state poller.** The state poller computes its per-tick context timeout as `lockTtl + 15s` (minimum 30 s) to allow time for acquiring the distributed lock plus network operations (`architecture/evm/evm_state_poller.go:L174-L187`).

### Observability

**Prometheus metrics** (all in `erpc` namespace):

| Metric | Type | Labels | When fired | Source |
|---|---|---|---|---|
| `erpc_upstream_latest_block_number` | Gauge | `project`, `vendor`, `network`, `upstream` | On every `OnValue` callback from the `latestBlockShared` counter; set via `tracker.SetLatestBlockNumber` | `telemetry/metrics.go:L61-L65`, `architecture/evm/evm_state_poller.go:L126-L130` |
| `erpc_upstream_finalized_block_number` | Gauge | `project`, `vendor`, `network`, `upstream` | On every `OnValue` callback from `finalizedBlockShared`; set via `tracker.SetFinalizedBlockNumber` | `telemetry/metrics.go:L67-L71`, `architecture/evm/evm_state_poller.go:L131-L133` |
| `erpc_upstream_latest_block_polled_total` | Counter | `project`, `vendor`, `network`, `upstream` | Each time `PollLatestBlockNumber` actually issues the RPC call (not debounced) | `telemetry/metrics.go:L366-L370`, `architecture/evm/evm_state_poller.go:L406-L411` |
| `erpc_upstream_finalized_block_polled_total` | Counter | `project`, `vendor`, `network`, `upstream` | Each time `PollFinalizedBlockNumber` actually issues the RPC call | `telemetry/metrics.go:L372-L376`, `architecture/evm/evm_state_poller.go:L508-L513` |
| `erpc_upstream_block_head_large_rollback` | Gauge | `project`, `vendor`, `network`, `upstream` | On `OnLargeRollback` callback; value is the rollback size in blocks | `telemetry/metrics.go:L378-L382`, `health/tracker.go:L1553-L1564` |
| `erpc_unexpected_panic_total` | Counter | `component`, `context`, `fingerprint` | On panic recovery inside `initCounterSync` (`component="shared-state-counter-sync"`), `messageLoop` (`component="redis-pubsub-message-loop"`), `pollingLoop` (`component="redis-pubsub-polling-loop"`) | `telemetry/metrics.go:L13-L17`, `data/shared_state_registry.go:L171-L175`, `data/redis_pubsub_manager.go:L232-L237`, `data/redis_pubsub_manager.go:L335-L340` |

**OTel trace spans:**
- `CounterInt64.TryUpdate` — every foreground local-update call; attribute `key` always present; `new_value` when detailed tracing enabled.
- `CounterInt64.TryUpdateIfStale` — attributes: `key`, `staleness_ms`, `skipped_not_stale`, `skipped_not_stale_after_mutex`, `role`, `result_path` (`got_result` vs `timeout`), `async_refresh`, `foreground_remote_io_disabled`.
- `CounterInt64.TryUpdateIfStale.AcquireMutex` — measures mutex wait time inside `TryUpdateIfStale`.
- `CounterInt64.TryUpdateIfStale.ExecuteRefresh` — spans the actual RPC call inside the refresh goroutine; attribute `timeout_ms`.
- `RedisConnector.PublishCounterInt64` — spans each publish call; attributes `key`, `value`, `updated_at`, `updated_by` when detailed tracing enabled.
- `PostgreSQLConnector.PublishCounterInt64` — same pattern as Redis.

**Notable log messages** (component `sharedState`, log level in parens):
- `"no remote initial value found for counter"` (DEBUG) — normal first-run.
- `"fetched initial value for counter"` (DEBUG) — successful seed.
- `"received new value from shared state sync"` (DEBUG) — pubsub/watch update.
- `"failed to setup counter sync"` (ERROR) — watch setup failed; will retry via initializer.
- `"lock held by another instance, proceeding with local lock"` (DEBUG) — normal under load.
- `"lock acquisition timed out waiting for other instance"` (WARN) — lock contention above `lockMaxWait`.
- `"lock expired during operations (expected behavior)"` (DEBUG) — expected when operations take near `lockTtl`.
- `"published counter value to remote"` (DEBUG) — successful remote write.
- `"pubsub reconnected successfully"` (INFO) — Redis pubsub recovery.
- `"counter value increased (remote)"` (TRACE) — value advanced by remote event.
- `"small rollback ignored (remote)"` (TRACE) — rollback inside threshold, discarded.
- `"large rollback applied (remote)"` (TRACE) — real reorg or provider reset.

### Edge cases & gotchas

1. **Value=0 is valid, UpdatedAt=0 is not.** Genesis block (`earliestBlock=0`) is a legitimate counter value. Code consistently uses `UpdatedAt <= 0` as the uninitialised sentinel, never `Value == 0` (`data/connector.go:L32-L34`, `data/shared_state_variable.go:L655-L657`).

2. **Stale lock expiry is silent.** If a remote operation takes longer than `lockTtl`, the lock key expires in Redis/PG. `Unlock` then returns "lock was already expired" which is logged at DEBUG level and the code continues normally. Choosing `lockTtl < fallbackTimeout` (which is rejected by validation) would cause this in every call.

3. **Remote-ahead adoption happens in background.** When a background push reads a remote value with a newer timestamp than local, it adopts that value under `updateMu`. This means a fresh pod that just connected can have its local counter bumped forward by a peer even before the pod's first RPC poll completes (`data/shared_state_variable.go:L686-L691`; test `TestSharedStateRegistry_UpdateCounter_RemoteHigherValue`).

4. **Small-rollback rejection advances local timestamp.** When a remote update has a newer `UpdatedAt` but a value that would imply a small rollback, `advanceTimestampPast` is called. This ensures the local (higher) value will win the next reconciliation cycle rather than being displaced by the stale remote state (`data/shared_state_variable.go:L211-L218`).

5. **TryUpdate MUST NOT acquire `updateMu`.** The mutex is solely for thundering-herd control in `TryUpdateIfStale`. Holding it during `TryUpdate` would block a concurrent `TryUpdateIfStale` caller that already holds the mutex and could in turn be waiting for background operations — a classic deadlock. The old design had this bug; the test `TestLockOrderSimulation_OldBuggyBehavior` documents it (`data/shared_state_variable_deadlock_test.go:L95-L212`).

6. **`continueAsyncRefresh` goroutine leak protection.** If the downstream RPC call ignores its context and never returns, the async helper would leak forever. A bounded timer of `fallbackTimeout + 1s` kills the helper even if the worker is stuck (`data/shared_state_variable.go:L511`; test `TestContinueAsyncRefresh_DoesNotLeakOnStuckWorker`).

7. **Double-checked staleness.** `TryUpdateIfStale` checks `IsStale` before and after acquiring `updateMu`. The second check prevents a thundering herd where N goroutines all see stale, queue behind the mutex, and then all fire the refresh after the first one already updated the value (`data/shared_state_variable.go:L399-L413`).

8. **Redis pubsub channel is buffered=1, drop-on-full.** If the registry consumer goroutine falls behind, messages are silently dropped rather than blocking the message loop. The periodic 5-minute poll in `pollingLoop` is the recovery path (`data/redis_pubsub_manager.go:L450-L457`).

9. **DynamoDB PublishCounterInt64 is a no-op.** The DynamoDB connector only propagates via polling (`statePollInterval`). Cross-pod latency is therefore bounded by the poll interval (default 5 s), not by a push event. Using DynamoDB as the shared-state connector means significantly higher cross-pod convergence latency than Redis or PostgreSQL (`data/dynamodb.go:L825-L829`).

10. **`WatchCounterInt64` channel closed on app shutdown.** The Redis `RedisPubSubManager.stop()` closes all subscriber channels; the registry's goroutine observing the channel will receive `ok=false` and call `initializer.MarkTaskAsFailed`. This is benign since the app context is also cancelled at that point (`data/shared_state_registry.go:L200-L207`).

11. **clusterKey propagation order.** Top-level `config.clusterKey` defaults to `"erpc-default"`. The `Database.SetDefaults(c.ClusterKey)` call passes the top-level key to `SharedStateConfig.SetDefaults(defClusterKey)` which only uses `defClusterKey` if `SharedStateConfig.ClusterKey` is empty. Thus: explicit `database.sharedState.clusterKey` wins over implicit top-level `clusterKey` (`common/defaults.go:L49-L55`, `L74-L76`, `L803-L805`).

12. **servedTip partitions are capped at 16 per network.** Once the cap is reached, new tag-group selectors fall back to the stateless subnet-minimum rather than getting their own monotonic counter. This cap protects against cardinality explosion from pathological configs (`erpc/networks.go:L80-L84`, `L429-L431`).

13. **init.go treats shared-state failure as a warning, not fatal.** `erpc/init.go:L93-L97` logs a warning and continues with `sharedState == nil`, which means eRPC falls back to in-memory mode rather than refusing to start. A broken Redis configuration therefore produces degraded (per-pod) behaviour, not a hard startup failure.

14. **`database.sharedState` nil default means SetDefaults is never called on it.** `DatabaseConfig.SetDefaults` only invokes `SharedStateConfig.SetDefaults` when the pointer is non-nil (`common/defaults.go:L837-L840`). Omitting the block entirely is valid and intentional — the runtime synthesises an equivalent in-memory config at `erpc/erpc.go:L49-L66`. There is no implicit write-back to `cfg.Database.SharedState`; the synthesised config exists only in local scope and does not affect subsequent config inspection.

15. **Connector `id` auto-assignment differs between shared-state and other scopes (GOTCHA).** The `connectorScopeSharedState` constant is `"shared-state"` (`common/defaults.go:L39`), so the general `ConnectorConfig.SetDefaults(scope)` pattern (`id = scope + "-" + driver`, `common/defaults.go:L847-L849`) would produce `"shared-state-redis"`. However, `SharedStateConfig.SetDefaults` intercepts the id assignment *before* calling `ConnectorConfig.SetDefaults`: if `connector.id` is empty, it is set to `string(connector.driver)` (e.g. `"redis"`) at `common/defaults.go:L799-L801`. `ConnectorConfig.SetDefaults` then sees `id` already set and skips its own assignment. The result is a shorter id (`"redis"` rather than `"shared-state-redis"`) for shared-state connectors whose id is not explicitly configured. If you copy a connector block from `database.evmJsonRpcCache` (which produces `"cache-redis"`) into `database.sharedState`, the auto-assigned id changes — which can break metric label matching, connector logging, or config validation rules that reference ids by name. Always set `connector.id` explicitly to get predictable, portable ids.

### Source map

- `data/shared_state_registry.go` — `SharedStateRegistry` interface + `sharedStateRegistry` implementation; counter lifecycle (GetCounterInt64, bootstrap tasks, WatchCounterInt64 goroutine, instance ID resolution).
- `data/shared_state_variable.go` — `counterInt64` struct; all counter logic: `TryUpdate`, `TryUpdateIfStale`, `processNewValue`, `processNewState`, rollback handling, timestamp CAS helpers, `scheduleBackgroundPushCurrent`, lock acquisition, remote read/write, callbacks.
- `data/shared_state_sources.go` — named constants for the `source` string in log/trace fields (`UpdateSourceRemoteSync`, `UpdateSourceTryUpdate`, etc.).
- `data/redis_pubsub_manager.go` — self-healing centralised Redis `PSubscribe` manager; subscriber lifecycle (copy-on-write, buffered channels, reconnect with backoff, periodic poll fallback).
- `data/redis.go` — `RedisConnector.WatchCounterInt64` (delegates to pubsub manager) and `PublishCounterInt64` (Redis PUBLISH to `counter:<key>`).
- `data/postgresql.go` — `PostgreSQLConnector.WatchCounterInt64` (LISTEN + 30 s polling) and `PublishCounterInt64` (pg_notify).
- `data/dynamodb.go` — `DynamoDBConnector.WatchCounterInt64` (polling only) and `PublishCounterInt64` (explicit no-op comment).
- `data/memory.go` — `MemoryConnector.WatchCounterInt64` and `PublishCounterInt64` both no-ops.
- `data/connector.go` — `Connector` interface definition; `CounterInt64State` struct; `ConnectorMainIndex` constant.
- `common/config.go:L269-L286` — `SharedStateConfig` struct definition with field comments.
- `common/defaults.go:L789-L828` — `SharedStateConfig.SetDefaults`; all default values.
- `common/validation.go:L263-L309` — `SharedStateConfig.Validate`; constraint enforcement (min values, ordering relationships).
- `architecture/evm/evm_state_poller.go` — consumer: creates `latestBlock`, `finalizedBlock`, `earliestBlock` counters; calls `TryUpdateIfStale` for polls and `TryUpdate` for suggestions; registers `OnValue`/`OnLargeRollback` callbacks.
- `erpc/erpc.go:L49-L66` — in-memory fallback synthesis when no `sharedState` config provided.
- `erpc/init.go:L84-L98` — startup initialization; non-fatal on failure.
- `erpc/networks.go:L55-L96`, `L405-L486` — `servedLatestBlock`/`servedFinalizedBlock` network-wide and per-group partitions; partition key derivation from upstream set hash; cap enforcement.
- `telemetry/metrics.go:L61-L71`, `L366-L382` — Prometheus gauges/counters related to block tracking and large rollbacks.
- `data/shared_state_registry_test.go` — unit tests for counter update flows, lock failure fallback, remote adoption, concurrent updates, watch setup.
- `data/shared_state_variable_timeout_test.go` — unit tests for foreground latency bounds, background push deduplication, async refresh, goroutine leak prevention.
- `data/shared_state_variable_deadlock_test.go` — concurrency/deadlock tests demonstrating correct lock ordering.
