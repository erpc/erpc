# KB: Rate limiters: budgets, rules, auto-tuner

> status: complete
> source-dirs: upstream/ratelimiter_registry.go, upstream/ratelimiter_budget.go, upstream/ratelimiter_autotuner.go, upstream/ratelimiter_mem_cache.go, common/config.go (RateLimit*), common/defaults.go, common/errors.go, auth/authorizer.go, erpc/projects.go, erpc/networks.go, upstream/upstream.go

## L1 — Capability (CTO view)

eRPC implements a multi-layer, budget-based rate limiter that can apply independent request caps at the project, network, upstream, and per-user (auth) layers. Budgets are named pools of rules that reference Envoy's ratelimit library backed by either an in-process sharded memory store or a shared Redis store; multiple upstreams can point at the same budget to share a quota. An optional auto-tuner continuously adjusts MaxCount in response to upstream error rates, so the proxy self-calibrates its outbound rate limits instead of requiring manual tuning when upstream providers change their thresholds.

## L2 — Mechanics (staff-engineer view)

### Request pipeline order

Every inbound request passes through rate limit checks in this order, any one of which can reject with HTTP 429:

1. **Auth-level** — `auth/authorizer.go:acquireRateLimitPermit` fires immediately after authentication succeeds. The budget used is the *effective* budget: the user object's `RateLimitBudget` (set by the strategy) overrides the strategy's static `cfg.RateLimitBudget`. `origin="auth"`.
2. **Project-level** — `erpc/projects.go:AcquireRateLimitPermit` runs inside `Project.Forward` before handing off to the network. `origin="project"`.
3. **Network-level** — `erpc/networks.go:acquireRateLimitPermit` inside `Network.Forward` after the method-handling check. `origin="network"`.
4. **Upstream-level** — `upstream/upstream.go:Forward` checks the budget assigned to `UpstreamConfig.RateLimitBudget` immediately before preparing and sending the request. `origin="upstream"`.

### Budget and rule matching

`GetBudget(id)` does a sync.Map lookup; a missing id returns `ErrRateLimitBudgetNotFound`. A budget is a named collection of rules. `GetRulesByMethod(method)` iterates all rules and returns those whose `Config.Method` either matches exactly or via `common.WildcardMatch` (glob semantics, e.g. `eth_*`). Both the exact-match path and the wildcard path are checked so a rule `eth_call` also fires when `WildcardMatch("eth_call", "eth_call")` is true.

### Parallel evaluation of multiple rules

If a single rule matches, `TryAcquirePermit` calls `evaluateRule` directly without spawning goroutines. If multiple rules match, each rule is evaluated in a separate goroutine; an `atomic.Bool` short-circuits remaining evaluations as soon as one rule fires `OVER_LIMIT`. All goroutines drain before returning. The first blocking rule is used for telemetry.

### Envoy ratelimit wire protocol

Each call to `evaluateRule` constructs a `pb.RateLimitRequest` with `Domain=budgetId`, `HitsAddend=1`, and a descriptor containing entries for:
- `method` (always present)
- `ip` (only when `rule.PerIP==true` and `clientIP` is non-empty and not `"n/a"`)
- `user` (only when `rule.PerUser==true` and `userLabel` is non-empty and not `"n/a"`)
- `network` (only when `rule.PerNetwork==true` and `networkLabel` is non-empty and not `"n/a"`)

The cache key incorporates all descriptor entries plus the time window, so per-user/per-IP rules produce independent counters for each caller. Scopes produce combinatorial key space — e.g. `perUser+perIP` means each (user, IP) pair has its own counter.

### Memory store (default)

`memoryRateLimitCache` is a sharded (64 shards, FNV-1a dispatch) bucketed map. Each shard holds a `map[expiry]map[key]uint64`. Counters increment atomically under a per-shard mutex. Cleanup is lazy: once per second per shard, all buckets with `expiry <= now` are deleted. The shards use `BaseRateLimiter.GenerateCacheKeys` for key generation (includes the time-sliced window suffix). This is a pure in-process store — no shared state across eRPC instances.

### Redis store

When `store.driver = "redis"`, the registry connects via the `envoyproxy/ratelimit` Redis client (radix v3, implicit pipelining window 5ms, pipeline limit 32). The connection is attempted in a background retry loop — startup is non-blocking and rate limiting **fail-opens** until the connection succeeds. Once connected, `GetCache()` returns the Redis-backed cache under a `cacheMu` RWMutex.

### Admission semaphore (Redis only)

Because `envoyproxy/ratelimit`'s Redis client does not honor context cancellation, a slow or stalled Redis can accumulate unlimited goroutines (each waiting for a reply). The admission semaphore (`budget.admission`, a buffered channel) bounds the number of concurrent in-flight `DoLimit` goroutines per budget to `max(256, connPoolSize*32)`. When the semaphore is full, the check **fail-opens immediately** without spawning a goroutine. The goroutine spawned for an admitted call holds its slot until Redis answers (even after the caller's local timeout fires), then releases the slot via a deferred function that also handles panics from `checkError()` in the envoy library. This design was implemented after the 2026-05-07 incident where 25k+ goroutines accumulated during a Redis contention spike.

### Timeout path (Redis only)

`doLimitWithTimeout` selects on `resultCh` vs `timer.C` (duration = `store.redis.getTimeout`, default 1s). On timeout, the function fail-opens and increments `MetricRateLimiterFailopenTotal{reason="limit_timeout"}`. The goroutine continues running in the background to complete the Redis write — it will release the admission slot and update the counter. This means "fail-open" still updates Redis state; it only removes the caller's wait obligation.

### Auto-tuner

The auto-tuner is attached per upstream (not per budget). It holds `errorCounts[method]` (`totalCount`, `errorCount`) under a mutex. After every `RecordSuccess` or `RecordError` call, `maybeAdjust` checks whether `adjustmentPeriod` has elapsed since the last adjustment for that method. If so:
- Counters are reset unconditionally.
- If `totalCount < 10`, skip (insufficient signal).
- If `errorRate = errorCount/totalCount > errorRateThreshold` → decrease: `newMax = ceil(prev * decreaseFactor)`.
- If `errorRate == 0` → increase: `newMax = ceil(prev * increaseFactor)`.
- Otherwise (0 < errorRate ≤ threshold) → no change.
- `AdjustBudgetByFactor` clamps to `[minBudget, maxBudget]` and guards against float64 overflow above `math.MaxUint32`.

`recordRequestSuccess` is called after a successful upstream response. `recordRemoteRateLimit` is called when the upstream returns a 429 (detected in `metricsTracker.RecordUpstreamRemoteRateLimited`). Note: the auto-tuner fires on the **upstream's budget** and will affect **all** rules in that budget matching the method — including shared budgets used by other upstreams.

### Shared budgets

A budget id is a string. Any number of project/network/upstream/auth attachment points can reference the same id. They all use the same `RateLimiterBudget` object (the same counters), so the budget is truly shared. In Redis mode this extends across multiple eRPC instances.

### WaitTime deprecation

`RateLimitRuleConfig.WaitTime` is persisted in the config struct for backward compatibility but is explicitly ignored at validation time with a warning log. There is no sleep-and-retry behaviour in the rate limiter.

## L3 — Exhaustive reference (agent view)

### Config fields

#### `rateLimiters` (top-level config)

| Field | Type | Default | Description |
|---|---|---|---|
| `rateLimiters.store.driver` | string | `"memory"` | Backend store. Valid values: `"memory"`, `"redis"`. Set in `RateLimitStoreConfig.SetDefaults` (`common/defaults.go:2782`). |
| `rateLimiters.store.redis.uri` | string | `""` | Redis connection URI. Takes precedence over `addr`. `common/config.go:390`. |
| `rateLimiters.store.redis.addr` | string | `""` | Redis `host:port`. Used when `uri` is empty. `common/config.go:384`. |
| `rateLimiters.store.redis.username` | string | `""` | Redis ACL username. `common/config.go:385`. |
| `rateLimiters.store.redis.password` | string | `""` | Redis password; redacted in JSON/YAML marshal. `common/config.go:386`. |
| `rateLimiters.store.redis.db` | int | `0` | Redis DB index. `common/config.go:387`. |
| `rateLimiters.store.redis.tls.enabled` | bool | `false` | Enable TLS for Redis connection. `common/config.go:376`. |
| `rateLimiters.store.redis.tls.certFile` | string | `""` | Path to client certificate PEM file for mTLS. Required together with `keyFile` when the Redis server demands client authentication. Zero-value `""` means no client cert is presented. `common/config.go:377`. |
| `rateLimiters.store.redis.tls.keyFile` | string | `""` | Path to client private key PEM file for mTLS. Must match `certFile`. Zero-value `""` means no client key is loaded. `common/config.go:378`. |
| `rateLimiters.store.redis.tls.caFile` | string | `""` | Path to a custom CA bundle PEM file used to verify the Redis server's certificate. When empty, the system root CA pool is used. `common/config.go:379`. |
| `rateLimiters.store.redis.tls.insecureSkipVerify` | bool | `false` | When `true`, skips server certificate verification entirely (equivalent to `tls.InsecureSkipVerify`). Default `false` means server cert IS verified. Use only for local dev; dangerous in production. `common/config.go:380`. |
| `rateLimiters.store.redis.connPoolSize` | int | `8` | Connection pool size for the radix client. Default from `RedisConnectorConfig.SetDefaults` (`common/defaults.go:955`). Used to derive the admission cap: `max(256, connPoolSize*32)`. |
| `rateLimiters.store.redis.initTimeout` | Duration | `5s` | Connection establishment timeout for startup. `common/defaults.go:958`. |
| `rateLimiters.store.redis.getTimeout` | Duration | `1s` | Per-call DoLimit timeout. Controls when fail-open triggers. `common/defaults.go:961`. Stored as `budget.maxTimeout`. |
| `rateLimiters.store.redis.setTimeout` | Duration | `3s` | Unused by rate limiter (data-connector field present for struct sharing). `common/defaults.go:964`. |
| `rateLimiters.store.redis.lockRetryInterval` | Duration | `500ms` | Unused by rate limiter. `common/defaults.go:967`. |
| `rateLimiters.store.nearLimitRatio` | float32 | `0.8` | Envoy near-limit threshold fraction. Controls when the Envoy library transitions from incrementing `Stats.WithinLimit` to also incrementing `Stats.NearLimit` internal counters. Specifically: `nearLimitThreshold = floor(maxCount * nearLimitRatio)`. When the running counter after increment is in the range `(nearLimitThreshold, maxCount]` the request is still **allowed** (the envoy library returns `OK` status) but the `NearLimit` stats counter is incremented. The `NEAR_LIMIT` proto status code is never returned to eRPC callers — eRPC only checks for `OVER_LIMIT`. In practice, changing this value only affects internal Envoy stats counters (not surfaced by eRPC telemetry) and has no effect on eRPC blocking decisions. Passed to both memory and Redis caches. `upstream/ratelimiter_registry.go:74,137`. Default in `defaultNearLimitRatio` (`ratelimiter_registry.go:250`). Library logic: `envoyproxy/ratelimit@.../src/limiter/base_limiter.go:102,177-185`. |
| `rateLimiters.store.cacheKeyPrefix` | string | `"erpc_rl_"` | Prefix prepended to all Redis/memory cache keys before the budget ID and descriptor hash. Serves as a namespace separator. **When to change:** if you run multiple independent eRPC deployments (dev/staging/prod, or multiple tenants) against a single shared Redis instance, each deployment MUST use a distinct prefix; otherwise their rate-limit counters will collide and quotas will be incorrect. Example: `prefix: "erpc_prod_"` vs `"erpc_staging_"`. Not needed for single-deployment setups. Has no effect on the memory driver (keys are in-process only), but is passed to the memory cache constructor for parity. `upstream/ratelimiter_registry.go:76,138`. Default in `defaultCacheKeyPrefix` (`ratelimiter_registry.go:257-261`). |

**`rateLimiters` absent from config = full passthrough (default state for new users):** When `rateLimiters` is omitted from the config YAML entirely, `cfg.RateLimiters` is `nil` (`common/config.go:46`). `NewRateLimitersRegistry` is always called even with nil config (`erpc/erpc.go:35`). `bootstrap()` detects `r.cfg == nil` at `upstream/ratelimiter_registry.go:48`, logs a debug message `"no rate limiters defined which means all capacity of both local cpu/memory and remote upstreams will be used"`, and returns immediately — no budgets are registered, no store is created. All `GetBudget("")` calls (when attachment points have empty `rateLimitBudget`) return `(nil, nil)` immediately. All `AcquireRateLimitPermit` functions check the attachment field first (e.g. `p.Config.RateLimitBudget == ""` at `erpc/projects.go:271`) and return nil before calling `GetBudget`. Every request is allowed. This is the expected default state for users who have not configured rate limiting.

**`rateLimiters.store` auto-creation behavior:** when `rateLimiters` is present in config, `RateLimiterConfig.SetDefaults` at `common/defaults.go:2763-2769` unconditionally creates `r.Store = &RateLimitStoreConfig{}` if it is nil, then calls `RateLimitStoreConfig.SetDefaults` which sets `Driver = "memory"`. This means:
- The `store` block is **never nil** after defaults are applied — the memory driver is always active even if `store` is omitted from YAML.
- To switch to Redis, you must explicitly set `rateLimiters.store.driver: redis`. There is no way to disable the store once `rateLimiters` is declared.
- Validation enforces: if any `rateLimitBudget` attachment point references a budget id, that id must exist in `rateLimiters.budgets`; `common/validation.go` cross-checks these at startup. A missing budget id causes `ErrRateLimitBudgetNotFound` at runtime (not at startup validation), not a config validation error.

#### `rateLimiters.budgets[]`

| Field | Type | Default | Description |
|---|---|---|---|
| `budgets[].id` | string | required | Unique identifier; referenced by `rateLimitBudget` fields elsewhere. `common/config.go:1806`. |
| `budgets[].rules[]` | list | required (min 1) | Rules in this budget. Must have at least one rule. `common/validation.go:199`. |
| `budgets[].rules[].method` | string | `"*"` | JSON-RPC method name or wildcard (e.g. `eth_*`, `*`). Default set by `RateLimitRuleConfig.SetDefaults` (`common/defaults.go:2818`). |
| `budgets[].rules[].maxCount` | uint32 | required | Maximum requests allowed in `period`. Mutable at runtime by auto-tuner. **`maxCount=0` means always-blocked:** the Envoy library sets `overLimitThreshold = uint64(RequestsPerUnit)` = 0; any request with counter ≥ 1 after increment is immediately `OVER_LIMIT`. There is no "unlimited" semantic for `maxCount=0`; use no budget at all (omit `rateLimitBudget`) for unrestricted access. **`uint32` overflow via auto-tuner increase:** `AdjustBudgetByFactor` computes `raw = ceil(prev * increaseFactor)` as float64 and explicitly clamps to `math.MaxUint32` (= 4,294,967,295) before narrowing to uint32. Without this clamp, a large `increaseFactor` could cause float64→uint32 conversion to wrap to an arbitrary small value. `common/config.go:1812`, `upstream/ratelimiter_budget.go:109-119`, `envoyproxy/ratelimit@.../src/limiter/base_limiter.go:76-79,99-108`. |
| `budgets[].rules[].period` | RateLimitPeriod | `second` (iota=0) | Window granularity. Accepted string aliases (case-insensitive): `second`/`1s`, `minute`/`1m`/`60s`, `hour`/`1h`/`3600s`, `day`/`24h`/`1d`/`86400s`, `week`/`7d`/`168h`/`604800s`, `month`/`30d`/`720h`/`2592000s`, `year`/`365d`/`8760h`/`31536000s`. Integer enum values (0–6) also accepted. **Legacy Go `time.Duration` strings:** as a final fallback, `time.ParseDuration(s)` is attempted. The parsed `time.Duration` value must then equal **exactly** one of the seven canonical durations (`time.Second`, `time.Minute`, `time.Hour`, `24*time.Hour`, `7*24*time.Hour`, `30*24*time.Hour`, `365*24*time.Hour`). For example: `"1000000000"` (1 billion nanoseconds = `time.Second`) or `"1h0m0s"` (the canonical Go string representation of `time.Hour`) are accepted. Values that parse successfully but do not equal a canonical duration — e.g. `"90s"` (= 1.5 minutes) or `"2h"` — return the error `"rate limiter period must be one of: second, minute, hour, day, week, month, year (got 2h0m0s)"`. Values that fail `time.ParseDuration` — e.g. `"fortnight"` or `""` — also return an error. `common/config.go:1882-1950`. Default from `SetDefaults` (`common/defaults.go:2809`). |
| `budgets[].rules[].perIP` | bool | `false` | If true, each distinct client IP gets its own counter. **Fallback behavior:** when `perIP=true` but `clientIP` is empty or `"n/a"` (e.g. the request came through a non-HTTP transport, or IP extraction failed), the `ip` descriptor entry is silently omitted and the rule evaluates against the global (non-IP-scoped) counter shared by all callers without a valid IP. This means IP-less callers share one counter among themselves, which may allow far more traffic than intended. `common/config.go:1817`, `upstream/ratelimiter_budget.go:256-264`. |
| `budgets[].rules[].perUser` | bool | `false` | If true, each authenticated user ID gets a separate counter. Requires non-empty, non-`"n/a"` user ID. `common/config.go:1816`. |
| `budgets[].rules[].perNetwork` | bool | `false` | If true, each network ID gets a separate counter. `common/config.go:1818`. |
| `budgets[].rules[].waitTime` | Duration | `0` | **Deprecated — ignored with warning log.** `common/validation.go:214-216`. |

#### Attachment points

| Config path | Scope | How budget is applied | Source |
|---|---|---|---|
| `projects[].rateLimitBudget` | Project-level | `PreparedProject.AcquireRateLimitPermit`; fires before network forward. `origin="project"`. | `erpc/projects.go:113,270`, `common/config.go:516` |
| `projects[].networks[].rateLimitBudget` | Network-level | `Network.acquireRateLimitPermit`; fires in `Network.Forward` after method-handling check. `origin="network"`. | `erpc/networks.go:1112,2268`, `common/config.go:1997` |
| `projects[].upstreams[].rateLimitBudget` | Upstream-level | `Upstream.Forward`; fires before request normalization. `origin="upstream"`. | `upstream/upstream.go:460,477`, `common/config.go:735` |
| `projects[].upstreamDefaults.rateLimitBudget` | Upstream default inheritance | Inherited by each upstream with empty `rateLimitBudget`. `common/defaults.go:1514`. | `common/config.go:868` |
| `projects[].networkDefaults.rateLimitBudget` | Network default inheritance | Applied to networks with empty `rateLimitBudget`. `common/defaults.go:1783`. | `common/config.go:627` |
| `projects[].auth.strategies[].rateLimitBudget` | Auth strategy static | Applied to any request authenticated by this strategy unless user's budget overrides. `origin="auth"`. | `auth/authorizer.go:119`, `common/config.go:2450` |
| `projects[].auth.strategies[].secret.*.rateLimitBudget` | Per-secret budget | Set on `user.RateLimitBudget` by `SecretStrategy`. Overrides strategy-level budget. | `auth/strategy_secret.go:29`, `common/config.go:2464` |
| `projects[].auth.strategies[].jwt.rateLimitBudgetClaimName` | JWT per-user budget via claim | JWT claim named `rateLimitBudgetClaimName` (default `"rlm"`) sets user's budget. Overrides strategy-level. | `auth/strategy_jwt.go:87`, `common/defaults.go:2749`, `common/config.go:2517` |
| `projects[].auth.strategies[].siwe.rateLimitBudget` | SIWE per-session budget | Set on user from SIWE strategy. | `auth/strategy_siwe.go:53`, `common/config.go:2523` |
| `projects[].auth.strategies[].network.rateLimitBudget` | IP-network strategy budget | Applied to requests from allowed IPs/CIDRs. | `auth/strategy_network.go:65,75,89`, `common/config.go:2532` |
| `projects[].auth.strategies[].database.failOpen.rateLimitBudget` | DB fail-open budget | Applied when database auth fails open. | `auth/strategy_database.go:526`, `common/config.go:2505` |

Note: when both strategy-level `rateLimitBudget` and user-level `user.RateLimitBudget` are set, the user-level value always wins. `auth/authorizer.go:119-122`.

#### `upstreams[].rateLimitAutoTune`

Available only on `UpstreamConfig`; not on networks/projects. Auto-populated as `&RateLimitAutoTuneConfig{}` when `rateLimitBudget != ""` and `rateLimitAutoTune` is nil (`common/defaults.go:1674`), so enabling a budget implicitly enables auto-tuning with default parameters unless `rateLimitAutoTune` is explicitly set.

| Field | Type | Default | Description |
|---|---|---|---|
| `rateLimitAutoTune.enabled` | *bool | `true` | Enable/disable. Set by `SetDefaults` (`common/defaults.go:2491`). |
| `rateLimitAutoTune.adjustmentPeriod` | Duration | `1m` | Minimum elapsed time between adjustments per method. `common/defaults.go:2494`. |
| `rateLimitAutoTune.errorRateThreshold` | float64 | `0.1` | If error rate > this, decrease budget. `common/defaults.go:2497`. |
| `rateLimitAutoTune.increaseFactor` | float64 | `1.05` | Multiplier when error rate is 0 (increase direction). `common/defaults.go:2499`. |
| `rateLimitAutoTune.decreaseFactor` | float64 | `0.95` | Multiplier when error rate > threshold (decrease direction). `common/defaults.go:2503`. |
| `rateLimitAutoTune.minBudget` | int | `0` | Floor for MaxCount after adjustment. 0 means unclamped. `common/config.go:1057`. |
| `rateLimitAutoTune.maxBudget` | int | `100000` | Ceiling for MaxCount after adjustment. `common/defaults.go:2506`. |

### Behaviors & algorithms

#### Rule matching algorithm (`GetRulesByMethod`)

```
for each rule in budget.Rules:
    if rule.Config.Method == method → include
    else if WildcardMatch(rule.Config.Method, method) == true → include
return matched rules
```

The check is `==` OR wildcard, not XOR — a non-wildcard exact method that also satisfies `WildcardMatch` will match once (the exact check short-circuits). `upstream/ratelimiter_budget.go:69-76`.

#### TryAcquirePermit flow

1. `getCache()` — if nil, return `(true, nil)` immediately (fail-open).
2. `GetRulesByMethod(method)` — if empty, return `(true, nil)` (no rules match, always allow).
3. Validate: if any matching rule has `perIP/perUser/perNetwork == true` and `req == nil`, return error.
4. If exactly 1 rule: call `evaluateRule` directly, no goroutine.
5. If > 1 rules: fan-out to goroutines; short-circuit via `atomic.Bool` once any rule is `OVER_LIMIT`.
6. If any rule blocked: increment `MetricRateLimitsTotal`, return `(false, nil)`.
7. Otherwise return `(true, nil)`.

`upstream/ratelimiter_budget.go:153-244`.

#### OK / NEAR_LIMIT / OVER_LIMIT status semantics

The Envoy ratelimit library produces three possible `pb.RateLimitResponse_Code` values per descriptor. eRPC only inspects `OVER_LIMIT`:

| Status | Counter range after increment | eRPC action | Library stats incremented |
|---|---|---|---|
| `OK` | `counter ≤ nearLimitThreshold` | Allow | `Stats.WithinLimit` |
| `OK` (near limit) | `nearLimitThreshold < counter ≤ maxCount` | Allow | `Stats.WithinLimit` + `Stats.NearLimit` |
| `OVER_LIMIT` | `counter > maxCount` | Block (return `false`) | `Stats.OverLimit` |

Where `nearLimitThreshold = floor(maxCount * nearLimitRatio)`. Default ratio 0.8: for `maxCount=100`, near-limit threshold = 80. Requests 81–100 are allowed but increment `NearLimit` stats. Request 101 is `OVER_LIMIT` and is blocked. **eRPC never surfaces `NEAR_LIMIT` as an error to callers** — both OK states result in `TryAcquirePermit` returning `(true, nil)`. The `NearLimit` counter is tracked in envoy's internal stats only and is not exported by eRPC's Prometheus telemetry. `upstream/ratelimiter_budget.go:306`, `envoyproxy/ratelimit@.../src/limiter/base_limiter.go:83-142`.

#### evaluateRule flow

1. Build descriptor entries: always `{method: method}`, then conditionally ip/user/network.
2. Create a `pb.RateLimitRequest` with `HitsAddend=1`, domain=budget.Id.
3. Create an in-memory `config.RateLimit` with `MaxCount` and `Period.Unit()`.
4. If `maxTimeout > 0` (Redis mode): call `doLimitWithTimeout`; else call `cache.DoLimit` directly.
5. If `timedOut` → return `true` (fail-open).
6. If `statuses[0].Code == OVER_LIMIT` → return `false`.
7. Otherwise return `true`.

Cache key used by BaseRateLimiter: `{cacheKeyPrefix}{domain}_{descriptor_entries_concatenated}_{time_window_bucket}`. The `erpc_rl_` prefix ensures isolation from any other Envoy ratelimit users of the same Redis.

#### Auto-tuner decision logic

Called on every `RecordSuccess` or `RecordError`:

```
if time.Since(lastAdjustments[method]) < adjustmentPeriod → return  // debounce
reset counters; update lastAdjustments[method] = now
if totalCount < 10 → return  // insufficient samples
errorRate = errorCount / totalCount
if errorRate > errorRateThreshold → direction = "decrease", factor = decreaseFactor
else if errorRate == 0 → direction = "increase", factor = increaseFactor
else → return  // in acceptable range, no change

for each matching rule:
    newMax = clamp(ceil(prevMax * factor), minBudget, maxBudget)
    if newMax != prevMax: rule.Config.MaxCount = newMax; update gauge
```

`upstream/ratelimiter_autotuner.go:83-142`, `upstream/ratelimiter_budget.go:101-140`.

Float64 overflow guard: if `float64(prev) * factor > math.MaxUint32`, `next` is clamped to `math.MaxUint32` before narrowing to uint32. `upstream/ratelimiter_budget.go:113-114`.

#### Memory cache eviction

Per shard, on every `DoLimit` call, if `shard.lastPrune != now` (in unix seconds), the shard deletes all bucket entries with `expiry <= now`. This is O(#buckets) per shard, not O(#keys). Benchmark shows this scales linearly with `keys` per shard from ~4K to 524K expired keys. `upstream/ratelimiter_mem_cache.go:91-100`.

#### Admission cap derivation

`remoteAdmissionCap(connPoolSize) = max(256, connPoolSize * 32)`. The ×32 multiplier accounts for radix's implicit pipelining window (up to 32 commands per connection). A pool of 8 → cap of 256 (min floor). A pool of 64 → cap of 2048. `upstream/ratelimiter_registry.go:281-291`.

#### Fail-open conditions summary

| Condition | Metric label `reason` |
|---|---|
| Cache is nil (Redis not yet connected) | n/a — TryAcquirePermit returns `(true, nil)` before reaching `doLimitWithTimeout` |
| Admission semaphore full | `admission_full` — `MetricRateLimiterFailopenTotal` + `MetricRateLimiterRemoteAdmissionSheddedTotal` |
| `doLimitWithTimeout` timer expired | `limit_timeout` — `MetricRateLimiterFailopenTotal` |
| `cache.DoLimit` panics (Redis error) | fail-open via recovered panic; `MetricUnexpectedPanicTotal` + `MetricRateLimiterRemoteDuration{result="fail_open"}` |

#### Error HTTP status codes

| Error type | Code | HTTP status | When produced |
|---|---|---|---|
| `ErrAuthRateLimitRuleExceeded` | `ErrAuthRateLimitRuleExceeded` | 429 (wire) | Auth-level budget rule fires. `common/errors.go:545`. |
| `ErrProjectRateLimitRuleExceeded` | `ErrProjectRateLimitRuleExceeded` | 429 (wire) | Project-level budget rule fires. `common/errors.go:1738`. |
| `ErrNetworkRateLimitRuleExceeded` | `ErrNetworkRateLimitRuleExceeded` | 429 (wire) | Network-level budget rule fires. `common/errors.go:~1760`. |
| `ErrUpstreamRateLimitRuleExceeded` | `ErrUpstreamRateLimitRuleExceeded` | **200 (wire)** despite `ErrorStatusCode()=429` | Upstream-level budget rule fires. See gotcha #18. `common/errors.go:1801-1821`. |
| `ErrRateLimitBudgetNotFound` | `ErrRateLimitBudgetNotFound` | 200 (wire, wrapped) | Budget ID configured on a project/network/upstream/auth attachment does not exist in `rateLimiters.budgets`. Produced at first request, not at startup. `upstream/ratelimiter_registry.go:224`, `common/errors.go:1709`. |
| `ErrRateLimitRuleNotFound` | `ErrRateLimitRuleNotFound` | 200 (wire, wrapped) | Defined in `common/errors.go:1723` but has **no call sites in production code** — reserved for future use. |

Sources: `common/errors.go:545-567,1709-1821`; wire status switch: `erpc/http_server.go:1484-1490`.

### Observability

#### Prometheus metrics

| Metric | Type | Labels | When it fires | Source |
|---|---|---|---|---|
| `erpc_rate_limits_total` | counter | `project, network, vendor, upstream, category, finality, user, agent_name, budget, scope, auth, origin` | A budget rule denies a request. `origin` ∈ `{auth, project, network, upstream}`. `scope` = comma-separated scopes from `ScopeString()`. | `telemetry/metrics.go:503`, `upstream/ratelimiter_budget.go:199,237` |
| `erpc_rate_limiter_budget_max_count` | gauge | `budget, method, scope` | Set on budget initialization and every auto-tuner adjustment. | `telemetry/metrics.go:509`, `upstream/ratelimiter_registry.go:201`, `upstream/ratelimiter_budget.go:94,138` |
| `erpc_rate_limiter_budget_decision_total` | counter | `project, network, category, finality, user, agent_name, budget, method, scope, decision` | **DEPRECATED** — replaced by `erpc_rate_limits_total`. Zero call sites in production code. | `telemetry/metrics.go:515` |
| `erpc_rate_limiter_failopen_total` | counter | `project, network, user, agent_name, budget, category, reason` | Rate limiter allowed the request due to an error (`admission_full`, `limit_timeout`). | `telemetry/metrics.go:521`, `upstream/ratelimiter_budget.go:367,454` |
| `erpc_rate_limiter_remote_inflight` | gauge | `budget` | Incremented when a goroutine is spawned for a Redis `DoLimit` call; decremented when it completes (or panics). | `telemetry/metrics.go:533`, `upstream/ratelimiter_registry.go:176` |
| `erpc_rate_limiter_remote_admission_shedded_total` | counter | `budget` | Admission semaphore full — request fail-opened without spawning goroutine. Distinct from `limit_timeout`. | `telemetry/metrics.go:544`, `upstream/ratelimiter_registry.go:177` |
| `erpc_rate_limiter_remote_duration_seconds` | histogram | `budget, result` | Duration of each Redis DoLimit call. `result` ∈ `{ok, over_limit, fail_open}`. Buckets: 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5. | `telemetry/metrics.go:903`, `upstream/ratelimiter_budget.go:429,432,434` |
| `erpc_unexpected_panic_total` | counter | `origin, store, fingerprint` | Redis DoLimit goroutine panicked; recovered. `origin="ratelimiter-redis-dolimit"`. | `upstream/ratelimiter_budget.go:388` |

#### Trace spans

| Span name | Created by |
|---|---|
| `RateLimiter.TryAcquirePermit` | Detail span with attrs `budget`, `method`. `upstream/ratelimiter_budget.go:161`. |
| `RateLimiter.DoLimit` | Client-kind span with attrs `budget`, `method`, `scope`, `result`. One per rule evaluated. `upstream/ratelimiter_budget.go:279`. |

#### Log messages

| Level | Message | When |
|---|---|---|
| `Debug` | `"no rate limiters defined which means all capacity of both local cpu/memory and remote upstreams will be used"` | `cfg == nil` at bootstrap. `upstream/ratelimiter_registry.go:49`. |
| `Debug` | `"initializing rate limiter budget"` | Each budget initialized. `upstream/ratelimiter_registry.go:158`. |
| `Debug` | `"preparing rate limiter rule: ..."` | Each rule added to budget. `upstream/ratelimiter_registry.go:185`. |
| `Warn` | `"failed to initialize Redis rate limiter on first attempt (rate limiting will fail-open until connected, retrying in background)"` | Redis first-connect fails. `upstream/ratelimiter_registry.go:66`. |
| `Info` | `"successfully connected to Redis for rate limiting"` | Redis connected. `upstream/ratelimiter_registry.go:150`. |
| `Debug` | `"rate limiter timeout exceeded, failing open"` | `doLimitWithTimeout` timer fires. Only logged at ≤DEBUG level to avoid log spam under sustained Redis pressure. `upstream/ratelimiter_budget.go:446`. |
| `Warn` | `"adjusting rate limiter budget from: X to: Y"` | Auto-tuner changes MaxCount. `upstream/ratelimiter_budget.go:92`. |
| `Info` | `"auto-tuner: adjusting rate limit budget"` | Auto-tuner fires with method, prev, next, errorRate, samples, direction. `upstream/ratelimiter_autotuner.go:132`. |
| `Warn` | `"rateLimiter.*.budget.rules.*.waitTime is deprecated and will be ignored"` | WaitTime != 0 at validation. `common/validation.go:216`. |

### Edge cases & gotchas

1. **Nil cache = fail-open, not an error.** When the memory store is not configured and Redis hasn't connected yet, `TryAcquirePermit` returns `(true, nil)` — all traffic is allowed. There is no error surfaced to the caller. `upstream/ratelimiter_budget.go:157-159`.

2. **waitTime is a no-op.** The field is in the YAML struct for backward compatibility. Validation emits a warning and ignores it. There is no sleep-and-retry logic anywhere in the rate limiter. `common/validation.go:214-216`.

3. **Auto-tuner is per-upstream, budget is global.** If two upstreams share the same budget, and each has its own auto-tuner, each tuner independently adjusts `MaxCount` on the same rule objects. The tuner that fires last wins. There is no coordination between tuners on a shared budget. `upstream/upstream.go:1321-1339`, `upstream/ratelimiter_budget.go:81-96`.

4. **Auto-tuner fires on error rate = 0 only.** Partial error rates in `(0, errorRateThreshold]` produce no adjustment. The budget will never increase unless error rate is exactly 0.0. `upstream/ratelimiter_autotuner.go:107-112`.

5. **Auto-tuner adjustmentPeriod resets on any call, not just adjustments.** The `lastAdjustments[method]` timestamp is updated at the start of `maybeAdjust` regardless of whether an actual change occurred. A continuous stream of 9 requests in `adjustmentPeriod` (below the 10-sample floor) will keep resetting the clock and preventing any evaluation. `upstream/ratelimiter_autotuner.go:94`.

6. **Auto-tuner requires both `rateLimitBudget != ""` and `rateLimitAutoTune != nil`.** An upstream without a `rateLimitBudget` never gets an auto-tuner, even if `rateLimitAutoTune` is set. `upstream/upstream.go:1322`.

7. **Auto-tuner is implicitly enabled when rateLimitBudget is set.** `SetDefaults` at `common/defaults.go:1674` creates a `RateLimitAutoTuneConfig{}` when `rateLimitBudget != ""` and `rateLimitAutoTune == nil`. The subsequent `SetDefaults` on that config sets `Enabled = true`. Opt out explicitly with `rateLimitAutoTune.enabled: false`.

8. **Wildcard method rules affect auto-tuner scope.** `GetRulesByMethod("eth_call")` will match a rule with `Method: "eth_*"`. The auto-tuner calls `GetRulesByMethod(triggeredMethod)` to find matching rules, so a wildcard rule `eth_*` will have its MaxCount adjusted whenever any `eth_*` method hits the threshold — not just the specific method that was rate-limited. `upstream/ratelimiter_autotuner.go:113`.

9. **PerIP/perUser/perNetwork with empty/`"n/a"` values fall back to global counter.** If `clientIP == ""` or `"n/a"`, the ip descriptor entry is silently omitted. The rule evaluates against a global (non-IP-scoped) counter rather than per-IP. Same for user and network. `upstream/ratelimiter_budget.go:256-264`.

10. **Shared budget across multiple layers has additive counting.** If the same budget is referenced at both project and network level, each incoming request increments the counter twice — once at project check and once at network check. This is unlikely to be the intended design for a shared budget across layers.

11. **Memory store counters are not shared across eRPC processes.** Only Redis mode provides cross-instance rate limiting. In memory mode, each eRPC instance has its own independent counters; the effective limit for a fleet is `maxCount * instanceCount`.

12. **Admission cap floor is 256 regardless of pool size.** Even with `connPoolSize: 1`, the cap is 256. This means very small pools on low-traffic instances still allow 256 concurrent Redis calls per budget before shedding, which may exceed the actual pool capacity. `upstream/ratelimiter_registry.go:281-291`.

13. **Redis DoLimit goroutines continue after timeout.** After `doLimitWithTimeout` returns `(nil, true, "limit_timeout")`, the spawned goroutine is still running in the background and will complete the Redis write. This is intentional (prevents counter drift) but means Redis receives more writes than the caller observes, and goroutines are not truly cancelled by the timeout — they are only abandoned. `upstream/ratelimiter_budget.go:349`.

14. **`NilCache` path in evaluateRule vs TryAcquirePermit.** Both `TryAcquirePermit` (at the top) and `evaluateRule` (redundantly) check `cache == nil`. The double check is defensive but means the nil path is handled consistently even if `evaluateRule` is called independently of `TryAcquirePermit`.

15. **Budget validation requires at least one rule.** A budget with `rules: []` fails validation at `RateLimitBudgetConfig.Validate()` with `"rateLimiter.*.budget.rules is required, add at least one rule"`. `common/validation.go:199`.

16. **ConcurrentPermits test is permanently skipped.** `TestRateLimiter_ConcurrentPermits` and `TestRateLimiter_ExceedCapacity` are both skipped with notes about needing rework. The in-production concurrency guarantee for the memory store is validated only through higher-level forwarding integration tests. `upstream/ratelimiter_test.go:137,217`.

17. **No enforcement on nil request with non-scoped rules.** If `req == nil` is passed and all matching rules have `perIP=false`, `perUser=false`, `perNetwork=false`, validation does not reject the nil req — it proceeds and uses the method-only descriptor. `upstream/ratelimiter_budget.go:188-191`.

18. **`ErrUpstreamRateLimitRuleExceeded` does NOT produce HTTP 429 on the wire despite its `ErrorStatusCode()` returning 429.** The wire status code switch at `erpc/http_server.go:1484-1490` checks only for `ErrCodeAuthRateLimitRuleExceeded`, `ErrCodeProjectRateLimitRuleExceeded`, `ErrCodeNetworkRateLimitRuleExceeded`, and `ErrCodeEndpointCapacityExceeded`. `ErrCodeUpstreamRateLimitRuleExceeded` is absent from this list. When an upstream-level budget fires, the error is wrapped in a `ErrNoUpstreamsAvailable` or similar multi-upstream error by the routing layer, and the final response is HTTP 200 with a JSON-RPC error body — **not** HTTP 429. The `ErrorStatusCode()=429` method on the struct only affects code paths that call `ErrorStatusCode()` directly (e.g. the gRPC adapter), not the HTTP wire path. `erpc/http_server.go:1484-1490`, `common/errors.go:1819-1821`.

19. **`ErrRateLimitBudgetNotFound` is a runtime error, not a startup config error.** Budget IDs referenced in `rateLimitBudget` fields are not cross-validated against `rateLimiters.budgets` during startup config validation (only `validateBudgetIdExists` calls exist for some paths). The error is produced by `GetBudget(id)` at `upstream/ratelimiter_registry.go:224` the first time a request is processed that needs the missing budget. This means a typo in a `rateLimitBudget` field may go undetected until the first request hits that code path.

20. **`ErrRateLimitRuleNotFound` has no production call sites.** The error type is defined at `common/errors.go:1723` and the constructor at `common/errors.go:1725`, but `grep` reveals no production code calls `NewErrRateLimitRuleNotFound`. It appears reserved for future use — possibly a planned feature where dynamic rule lookup would fail when a method-specific rule is not found.

21. **`store` block is always auto-created with `driver="memory"` even if only Redis was intended.** If `rateLimiters` is present in config without an explicit `store.driver: redis`, the memory store is silently used. No warning is emitted. If you add `rateLimiters.store.redis.*` fields without setting `driver: redis`, those fields are ignored and memory is used. `common/defaults.go:2763-2793`.

22. **`rateLimiters` absent from config = complete passthrough; no blocking occurs.** When `cfg.RateLimiters == nil` (the default for a fresh installation with no `rateLimiters:` key in YAML), `bootstrap()` returns immediately and no budgets are initialized. Every budget attachment point (`rateLimitBudget` on project/network/upstream/auth) is effectively ignored. This is the intended no-rate-limiting state. Adding `rateLimiters:` to the config without any `budgets:` entries also produces a passthrough if no attachment point references a budget id — there will just be a store with no rules. `upstream/ratelimiter_registry.go:48-51`, `erpc/erpc.go:35-42`.

23. **`maxCount=0` means always-blocked, not unlimited.** The Envoy library sets `overLimitThreshold = uint64(RequestsPerUnit)`. With `maxCount=0`, `overLimitThreshold=0`, so any increment (HitsAddend=1) produces `limitAfterIncrease=1 > 0`, which is immediately `OVER_LIMIT`. There is no "unlimited" semantic exposed through `maxCount`; to allow unrestricted traffic for a given scope, omit the `rateLimitBudget` attachment. `envoyproxy/ratelimit@.../src/limiter/base_limiter.go:76-79`.

24. **`nearLimitRatio` has no effect on eRPC's blocking decisions.** eRPC only checks `statuses[0].Code == OVER_LIMIT` (`upstream/ratelimiter_budget.go:306`). The `NEAR_LIMIT` proto value and the internal `Stats.NearLimit` counter are never evaluated by eRPC. The only operational effect of changing `nearLimitRatio` is altering when the envoy library increments its internal `NearLimit` counter, which is not exported to Prometheus. `nearLimitRatio` should be left at its default `0.8` unless integrating with external Envoy stats tooling that reads those counters directly.

25. **`cacheKeyPrefix` collision between eRPC deployments sharing a Redis.** If two eRPC instances (or clusters) are configured with the same Redis URI and the same `cacheKeyPrefix` (both defaulting to `"erpc_rl_"`), their rate-limit counters will be merged — a request counted against deployment A also consumes quota from deployment B's budget with the same budget ID and method. The correct fix is distinct prefixes: `"erpc_prod_"` and `"erpc_staging_"`. The prefix is embedded in every cache key via `NewCacheKeyGenerator(cacheKeyPrefix)` in the Envoy `BaseRateLimiter`. `upstream/ratelimiter_registry.go:75-76,138`, `upstream/ratelimiter_mem_cache.go:59`.

26. **Legacy `time.Duration` period strings require exact canonical equivalence, not just a matching duration.** `time.ParseDuration("2h")` succeeds and returns `2*time.Hour`, but `2*time.Hour` does not equal any canonical period (the hour canonical is `time.Hour = 1*time.Hour`). The unmarshal code returns an error: `"rate limiter period must be one of: second, minute, hour, day, week, month, year (got 2h0m0s)"`. Only values that satisfy `time.ParseDuration` AND match exactly one of the 7 hardcoded constants are accepted. The "legacy" use-case is Go `time.Duration.String()` output like `"1h0m0s"` (which Go's `time.ParseDuration` accepts and maps to `time.Hour`). `common/config.go:1924-1944`.

### Source map

- `upstream/ratelimiter_registry.go` — Registry creation, Redis connect (with background retry), budget initialization, `GetBudget`, `AdjustBudget`, admission cap calculation (`remoteAdmissionCap`).
- `upstream/ratelimiter_budget.go` — `RateLimiterBudget` struct, `TryAcquirePermit`, parallel rule evaluation, `evaluateRule`, `doLimitWithTimeout` (admission semaphore + timeout + panic recovery), `AdjustBudget`, `AdjustBudgetByFactor`, `GetRulesByMethod`.
- `upstream/ratelimiter_autotuner.go` — `RateLimitAutoTuner`: `RecordSuccess`, `RecordError`, `maybeAdjust` with error rate logic.
- `upstream/ratelimiter_mem_cache.go` — In-process sharded bucketed counter cache implementing `limiter.RateLimitCache`. 64 FNV-1a shards; lazy O(#buckets) per-shard cleanup every second.
- `upstream/ratelimiter_test.go` — Unit tests for registry/budget/rule matching; two tests skipped pending Envoy-based limiter stabilization.
- `upstream/ratelimiter_leak_test.go` — Goroutine leak tests: `TestRateLimiterBudget_NoGoroutineLeakUnderRedisStall` (reproduces 2026-05-07 incident), `TestRateLimiterBudget_AdmissionPanicSafety`.
- `upstream/ratelimiter_budget_bench_test.go` — Budget benchmarks (single/multi rule, per-user/IP/network, with Redis latency simulation).
- `upstream/ratelimiter_mem_cache_bench_test.go` — Memory cache benchmarks including rollover cleanup cost at various cardinalities (4K–524K keys).
- `upstream/upstream.go` — `initRateLimitAutoTuner`, `recordRequestSuccess`, `recordRemoteRateLimit`, upstream-level `TryAcquirePermit` call.
- `common/config.go` — `RateLimiterConfig`, `RateLimitBudgetConfig`, `RateLimitRuleConfig`, `RateLimitAutoTuneConfig`, `RateLimitStoreConfig`, `RateLimitPeriod` enum + marshal/unmarshal, `ScopeString()`, `HasRateLimiterBudget()`.
- `common/defaults.go` — `SetDefaults` for all rate limit config types; auto-tune defaults; budget auto-populate logic.
- `common/validation.go` — `Validate` for budget/rule; `waitTime` deprecation warning; budget-id existence cross-checks.
- `common/errors.go` — `ErrAuthRateLimitRuleExceeded`, `ErrProjectRateLimitRuleExceeded`, `ErrNetworkRateLimitRuleExceeded`, `ErrUpstreamRateLimitRuleExceeded`, `ErrRateLimitBudgetNotFound`, `ErrRateLimitRuleNotFound`.
- `auth/authorizer.go` — `acquireRateLimitPermit`; user-level budget override logic.
- `auth/strategy_jwt.go`, `strategy_secret.go`, `strategy_siwe.go`, `strategy_network.go`, `strategy_database.go` — Set `user.RateLimitBudget` during authentication.
- `erpc/projects.go` — `PreparedProject.AcquireRateLimitPermit` (project-level check in Forward).
- `erpc/networks.go` — `Network.acquireRateLimitPermit` (network-level check in Forward).
- `telemetry/metrics.go` — All rate limiter Prometheus metric registrations.
