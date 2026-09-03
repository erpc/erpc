# Plan 001: Land a unified ConfigController for static vs dynamic config

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**:
> `git diff --stat 8459b053..HEAD -- common/ erpc/ data/ auth/ upstream/ internal/policy/ cmd/erpc/ docs/pages/config/ docs/pages/deployment/`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: L
- **Risk**: HIGH
- **Depends on**: none
- **Category**: direction
- **Planned at**: commit `8459b053`, 2026-08-05

## Why this matters

Today eRPC loads `*common.Config` once at process start (`common.LoadConfig` →
`erpc.Init` → `NewERPC`) and freezes almost everything. Ops docs require a pod
restart after ConfigMap edits. A few ad-hoc mutation paths exist (admin API
keys, cordon, rate-limit MaxCount auto-tune, selection-policy re-register,
cache `SetPolicies`), but each feature that wants live updates currently
reimplements its own plumbing.

This plan lands **one** central pattern so any feature can opt into live config:

1. Developer picks **config mode**: `static` (file, current behavior) or
   `dynamic` (DB/connector-backed document with fast reflection).
2. A single **ConfigController** owns load → validate → diff → apply → notify.
3. Runtime components implement a small **Reloadable** contract instead of
   inventing watch loops, atomic swaps, or admin RPCs per feature.

After this lands, adding live-reload to a new surface is "implement
`Reloadable` + register with the controller", not a new subsystem.

## Architecture (read fully before coding)

### Modes

```
configSource:
  mode: static | dynamic          # default: static (backward compatible)
  # dynamic-only:
  connector: { id, driver, ... }  # reuse common.ConnectorConfig
  documentKey: "erpc/config"      # partition/range key convention below
  pollInterval: 2s                # safety net even when Watch works
  applyTimeout: 30s
```

- **`static`**: exactly today's path. `LoadConfig(file)` → `Init`. No watcher.
  No behavior change for existing deployments.
- **`dynamic`**: bootstrap file still required (chicken-and-egg). The bootstrap
  file holds **only** process-identity + configSource + anything marked
  restart-required. The **authoritative** `*common.Config` body (projects,
  upstreams, failsafe, rate limiters, cache policies, auth strategies, …)
  lives in the connector as a versioned JSON document. Controller fetches,
  runs the same `SetDefaults` → `Validate` pipeline as `LoadConfig`, then
  applies diffs.

Bootstrap file MUST remain able to supply a full static config when
`mode: static` (or when `configSource` is omitted — treat as static).

### Document shape (v1)

Store **one logical config document** per cluster, not normalized tables:

```
partitionKey = "{clusterKey}|config"     # clusterKey from bootstrap / cfg.ClusterKey
rangeKey     = "v1"                      # schema of the envelope, not of erpc config
value        = JSON ConfigEnvelope:
{
  "revision": 42,                        // monotonic int64; controller ignores stale
  "hash": "<sha256 of canonical payload>",
  "updatedAt": "RFC3339",
  "updatedBy": "admin|seed|external",
  "payload": { ... same shape as common.Config JSON ... }
}
```

Canonicalization: `json.Marshal` of the payload after `SetDefaults` is **not**
required for hash — hash the raw stored payload bytes the writer persisted,
so readers can detect no-op writes without re-serializing Go structs.

Revision notification (multi-replica):

1. Writer bumps `revision` and `Set`s the document.
2. Writer also `PublishCounterInt64` on key `{clusterKey}|config|rev` with
   `value = revision` (reuse existing connector pub/sub — see
   `data.Connector.WatchCounterInt64` / `PublishCounterInt64`).
3. Each replica’s controller: Watch the counter → on bump, Get the document →
   if `revision > local` apply; else ignore. Poll every `pollInterval` as
   fallback (DynamoDB/memory Watch quality varies).

### Central components

Put new packages under `configctl/` at repo root (new top-level package;
avoids circular imports with `common` ↔ `erpc` ↔ `data`):

```
configctl/
  source.go          # ConfigSource interface + StaticSource + ConnectorSource
  envelope.go        # ConfigEnvelope encode/decode + hash
  controller.go      # ConfigController: Start, current snapshot, apply loop
  diff.go            # Diff(old, new *common.Config) -> []Change
  reloadable.go      # Reloadable interface + Registry
  apply.go           # ordered apply of changes to registered Reloadables
  tiers.go           # ReloadTier classification helpers
  *_test.go
```

**`ConfigSource`**:

```go
type ConfigSource interface {
    // Load returns the latest envelope. StaticSource wraps a *common.Config
    // already loaded from file (revision=0, never changes).
    Load(ctx context.Context) (*ConfigEnvelope, error)
    // Watch yields when a newer revision may be available. StaticSource's
    // Watch blocks until ctx cancel and never sends. Caller must still
    // Load() after a signal (signals are edge-triggered hints, not payloads).
    Watch(ctx context.Context) (<-chan struct{}, error)
    Close() error
}
```

**`Reloadable`** (the extension point every feature implements):

```go
type ReloadTier int
const (
    TierInPlace ReloadTier = iota // mutate existing object; request path already reads pointer
    TierRebuild                   // tear down + build replacement; drain in-flight
    TierRestartRequired           // cannot hot-apply; log + metric + surface via admin
)

type Change struct {
    Path    string      // e.g. "projects[main].networks[evm:1].failsafe"
    Tier    ReloadTier
    Old, New any        // typed nils ok
}

type Reloadable interface {
    // Name is stable for logs/metrics, e.g. "network:main/evm:1", "cache-policies".
    Name() string
    // Handles reports which change-path prefixes this component owns
    // (glob-style, matched by controller). Example: "projects.*.networks.*.failsafe"
    Handles() []string
    // Apply is called only for Changes this Reloadable Handles.
    // Must be idempotent. On error, controller aborts remaining applies for
    // this revision and keeps the previous snapshot (see apply semantics).
    Apply(ctx context.Context, changes []Change) error
}
```

**`ConfigController`**:

```go
type Snapshot struct {
    Revision int64
    Hash     string
    Config   *common.Config  // immutable after publish; readers use atomic.Pointer
}

type Controller struct {
    // ...
}

func NewController(logger, source ConfigSource, opts ...) *Controller
func (c *Controller) Register(r Reloadable)
func (c *Controller) Start(ctx context.Context) error // initial Load + Watch loop
func (c *Controller) Snapshot() *Snapshot             // atomic.Pointer load
func (c *Controller) ForceLoad(ctx context.Context) error // admin kick
```

Apply semantics (critical):

1. Load envelope → decode payload → run **the same** migrate/`SetDefaults`/
   `Validate` path `common.LoadConfig` uses (extract a shared
   `common.FinalizeConfig(cfg, opts)` if not already factorable — simulator
   already has `FinalizeConfig` in `internal/simulator`; prefer lifting a
   neutral helper into `common` rather than importing simulator).
2. Diff against `Snapshot().Config`.
3. If any change is `TierRestartRequired`, **do not partially apply** silent
   mutations that would diverge from the document. Record them on the
   snapshot as `PendingRestart []Change`, emit metric
   `erpc_config_reload_restart_required`, log at Error once per revision,
   still apply the TierInPlace/TierRebuild changes that are safe.
   (Alternative harder rule — reject whole revision — is worse for ops;
    document the chosen rule in docs.)
4. Apply TierInPlace then TierRebuild, grouped by Reloadable, in a stable
   dependency order defined in `apply.go`:
   `rateLimiters → proxyPools → cache.policies → projects.* → networks.* → upstreams.* → auth.* → selectionPolicy.*`
5. Only after all Applies succeed: `atomic.Store` new Snapshot.
6. If any Apply fails: keep old Snapshot, metric `erpc_config_reload_errors`,
   do **not** advance local revision cursor (retry on next Watch/poll).

Concurrency model — copy the comment block style from
`thirdparty/remote_cache.go:50-78`:

- Hot path reads `Controller.Snapshot()` via `atomic.Pointer` only.
- Refresh/apply runs on one goroutine (single-flight). Never hold a mutex
  across connector I/O except a tiny inflight flag.
- Reloadable.Apply may take component locks; it must not call back into
  Controller.Load/ForceLoad.

### Reload tiers for the current config tree

Classify every top-level / nested field. Executor must encode this table in
`configctl/tiers.go` as path→tier rules (longest-prefix match):

| Path | Tier | Notes / existing seam |
|------|------|------------------------|
| `server.*` (listen, TLS, HTTP timeouts baked into `http.Server`) | RestartRequired | `erpc/http_server.go` |
| `metrics.*` listen | RestartRequired | `erpc/init.go` |
| `tracing.*` | RestartRequired | `initOnce.Do` |
| `logLevel` | InPlace | already overridable; set zerolog level |
| `clusterKey` | RestartRequired | identity / shared-state namespace |
| `database.evmJsonRpcCache.connectors.*` | RestartRequired (v1) | connections opened once |
| `database.evmJsonRpcCache.policies` | InPlace | `EvmJsonRpcCache.SetPolicies` |
| `database.sharedState.*` | RestartRequired | connector lifecycle |
| `rateLimiters.budgets[*].rules[*].maxCount` | InPlace | `AdjustBudget` |
| `rateLimiters` topology (add/remove budget/store) | Rebuild | needs new registry API — **out of scope for 001 exemplar** |
| `proxyPools.*` | RestartRequired (v1) | HTTP client built once |
| `projects[*].cors` | InPlace | request path reads `project.Config.CORS` |
| `projects[*].networks[*]` directive/method/rateLimitBudget/servedTip fields | InPlace | `networks.go` derefs `n.cfg` |
| `projects[*].networks[*].failsafe` | Rebuild | need `Network.ReplaceFailsafe` — follow-on |
| `projects[*].networks[*].selectionPolicy` | Rebuild | `policy.Engine.RegisterNetwork` already supports re-register |
| `projects[*].upstreams[*]` endpoint/headers/identity | Rebuild | add/remove lifecycle — follow-on |
| `projects[*].upstreams[*].ignoreMethods` | InPlace | existing `cfgMu` |
| `admin.auth` strategy list | Rebuild | follow-on |
| `configSource.*` | RestartRequired | changing store mid-flight unsupported |

Plan 001 implements the controller + tiers table + **two** Reloadables as
exemplars (prove InPlace and Rebuild):

1. **InPlace exemplar**: `database.evmJsonRpcCache.policies` via existing
   `EvmJsonRpcCache.SetPolicies` (`architecture/evm/json_rpc_cache.go`).
2. **Rebuild exemplar**: `projects[*].networks[*].selectionPolicy` via
   `policy.Engine.RegisterNetwork` (`internal/policy/engine.go`).

Everything else in the tiers table is classified and **reported** as
restart-required or "no Reloadable registered" but not yet wired. That is
intentional — the plumbing must exist before mass migration.

### Wiring into startup

Today (`erpc/init.go`):

```
LoadConfig (in main) → Init(cfg) → NewEvmJsonRpcCache / SharedState / NewERPC / servers
```

Target:

```
main:
  bootstrapCfg := LoadConfig(file)                    // always
  source := configctl.NewSourceFromBootstrap(bootstrapCfg)
  ctrl := configctl.NewController(source, ...)
  envelope0, err := source.Load(ctx)                  // static: returns bootstrap body
  runtimeCfg := envelope0.Payload                     // after Finalize
  // merge rule: restart-required fields ALWAYS come from bootstrap file;
  // dynamic payload may not override server/metrics/tracing/configSource/database.sharedState connector.
  erpc.Init(appCtx, runtimeCfg, logger, ctrl)
```

Inside `Init` / `NewERPC`:

- Construct cache, registries, projects as today from `runtimeCfg`.
- Register Reloadables on `ctrl`.
- `ctrl.Start(appCtx)` **after** initial component construction (so first
  Watch-driven apply has targets). The initial Load already produced
  `runtimeCfg` — Start must NOT re-apply revision 0.

Merge rule (bootstrap wins for infrastructure): implement
`configctl.MergeBootstrap(bootstrap, dynamic *common.Config) *common.Config`
that copies `Server`, `Metrics`, `Tracing`, `HealthCheck`, `Database.SharedState`,
and `configSource` (new field) from bootstrap over the dynamic payload.
Dynamic payload owns `Projects`, `RateLimiters`, `ProxyPools`,
`Database.EvmJsonRpcCache.Policies` (and connectors only if bootstrap left
them nil — document this).

### Admin surface (minimal in 001)

Extend `erpc/admin.go` with read/kick only (no full CRUD editor yet):

- `erpc_config` — already returns config; include `revision`, `hash`,
  `mode`, `pendingRestart` from controller snapshot.
- `erpc_config_reload` — calls `Controller.ForceLoad` (ops escape hatch).

Do **not** add `erpc_config_apply` body-write in 001 unless the store write
path is trivial; external writers can `Set` the envelope directly. A write
API is a follow-on plan.

### Metrics

Add (names must match repo prometheus style — check `telemetry/` before
finalizing; use `erpc_config_*` prefix):

- `erpc_config_revision` (gauge) — current applied revision
- `erpc_config_reload_total{result="success|error|noop"}`
- `erpc_config_reload_duration_seconds`
- `erpc_config_reload_restart_required` (gauge / counter of pending paths)
- `erpc_config_reload_apply_errors_total{reloadable=...}`

### What NOT to copy

- Do **not** make production call `simulator.bootFromYAML` / full `NewERPC`
  swap. Simulator stays a toy full-reboot path.
- Do **not** add `fsnotify` file watching in 001.
- Do **not** change the public YAML schema of existing fields without
  version markers + docs (new `configSource` block is additive).
- Do **not** disable or skip existing tests to land this.

## Current state

Key files and roles:

- `common/config.go` — `Config` struct + `LoadConfig` (lines 37–120 region)
- `common/defaults.go` — `SetDefaults` cascade
- `common/validation.go` — `Validate`
- `cmd/erpc/main.go` — discovery + `getConfig` → `erpc.Init`
- `erpc/init.go` — startup wiring (cache, shared state, NewERPC, HTTP/gRPC)
- `erpc/erpc.go` — root object; holds `cfg *common.Config`
- `erpc/admin.go` — `erpc_config` read-only dump; API keys + cordon mutate
- `data/connector.go` — `Connector` with Get/Set/WatchCounterInt64/PublishCounterInt64
- `architecture/evm/json_rpc_cache.go` — `SetPolicies` InPlace seam (~152)
- `internal/policy/engine.go` — `RegisterNetwork` Rebuild seam (~261–309)
- `thirdparty/remote_cache.go` — atomic.Pointer COW pattern to mirror
- `internal/simulator/orchestrator.go` — full reboot ApplyConfig (anti-pattern for prod)
- `docs/pages/deployment/kubernetes.mdx` — currently says restart to reload
- `auth/strategy_database.go` — precedent for connector-backed mutable data

Excerpts to orient (confirm against live code during drift check):

```38:49:common/config.go
type Config struct {
	LogLevel     string             `yaml:"logLevel,omitempty" json:"logLevel" tstype:"LogLevel"`
	ClusterKey   string             `yaml:"clusterKey,omitempty" json:"clusterKey"`
	Server       *ServerConfig      `yaml:"server,omitempty" json:"server"`
	HealthCheck  *HealthCheckConfig `yaml:"healthCheck,omitempty" json:"healthCheck"`
	Admin        *AdminConfig       `yaml:"admin,omitempty" json:"admin"`
	Database     *DatabaseConfig    `yaml:"database,omitempty" json:"database"`
	Projects     []*ProjectConfig   `yaml:"projects,omitempty" json:"projects"`
	RateLimiters *RateLimiterConfig `yaml:"rateLimiters,omitempty" json:"rateLimiters"`
	Metrics      *MetricsConfig     `yaml:"metrics,omitempty" json:"metrics"`
	ProxyPools   []*ProxyPoolConfig `yaml:"proxyPools,omitempty" json:"proxyPools"`
	Tracing      *TracingConfig     `yaml:"tracing,omitempty" json:"tracing"`
```

```40:51:data/connector.go
type Connector interface {
	Id() string
	Get(ctx context.Context, index, partitionKey, rangeKey string, metadata interface{}) ([]byte, error)
	Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error
	Delete(ctx context.Context, partitionKey, rangeKey string) error
	List(ctx context.Context, index string, limit int, paginationToken string) ([]KeyValuePair, string, error)
	Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error)
	WatchCounterInt64(ctx context.Context, key string) (<-chan CounterInt64State, func(), error)
	PublishCounterInt64(ctx context.Context, key string, value CounterInt64State) error
}
```

Conventions to match:

- Zerolog only (`log.Logger` / passed `*zerolog.Logger`).
- Wrap errors with `fmt.Errorf("...: %w", err)`.
- Test files that log: `func init() { util.ConfigureTestLogger() }`.
- Gock mocks before network init; `util.ResetGock()` + defer.
- Docs: agent-first page under `docs/pages/config/` with `<AISection>` —
  see exemplar `docs/pages/config/failsafe/hedge.mdx`. New `configSource`
  field needs docs in the same PR.
- Public schema changes need version markers per `.cursor/rules/erpc.md`.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Format | `make fmt` | exit 0 |
| Build | `make build` | exit 0 |
| Focused tests | `go test -count=1 ./configctl/...` | all pass |
| Exemplar integration | `LOG_LEVEL=debug go test -count=1 -run 'TestConfigController_' ./erpc/ ./configctl/` | all pass |
| Fast suite smoke | `make test-fast` | only if time; not required every step |
| Tygo (if Config gains fields) | follow repo `tygo.yaml` / existing generate path | `typescript/config` types updated |

## Scope

**In scope** (create/modify):

- `configctl/` (new package) — all files listed in Architecture
- `common/config.go` — additive `ConfigSourceConfig` field on `Config` (name
  TBD but YAML key `configSource`); any small `FinalizeConfig` extraction
- `common/defaults.go` / `common/validation.go` — defaults + validation for
  `configSource`
- `cmd/erpc/main.go` — wire source + controller bootstrap
- `erpc/init.go`, `erpc/erpc.go` — accept controller, register reloadables
- `erpc/admin.go` — snapshot fields + `erpc_config_reload`
- `architecture/evm/json_rpc_cache.go` — only if SetPolicies needs a thin
  wrapper to satisfy Reloadable (prefer adapter in `erpc/` or `configctl/`
  over changing cache API)
- `erpc/` adapters implementing the two exemplar Reloadables
- `telemetry/` — new metrics
- `docs/pages/config/` — new page `config-source.mdx` (or similar) + `_meta.js`
  entry; update kubernetes.mdx note that dynamic mode exists
- `typescript/config` generated types if tygo is part of the usual flow for
  new Config fields
- `configctl/*_test.go`, plus 1–2 tests under `erpc/` proving end-to-end
  apply of the two exemplars with a memory connector

**Out of scope** (do NOT touch in this plan):

- Upstream add/remove/endpoint rebuild APIs
- Network failsafe `ReplaceFailsafe`
- Rate-limiter budget topology rebuild
- Auth strategy list reload
- Proxy pool rebuild
- Cache connector add/remove
- `fsnotify` file watching
- Simulator Rewrite to use ConfigController (optional later)
- Admin rich config editor / `erpc_config_apply` write API
- Changing Kubernetes docs beyond a short pointer to the new config-source page
- Any SaaS control-plane UI

## Git workflow

- Branch: `feat/dynamic-config-controller` (or `advisor/001-dynamic-config-controller`)
- Commit style: conventional, matching recent history
  (`feat(config): ...`, `test(config): ...`, `docs(config): ...`)
- Example from log: `feat(consensus): winner-composition quota via requiredParticipants[].minAgreement (#1008)`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Drift check + package scaffold

Run the drift-check command in the Executor instructions. Create
`configctl/` with stub files compiling:

- `source.go`, `envelope.go`, `controller.go`, `diff.go`, `reloadable.go`,
  `apply.go`, `tiers.go`
- empty interfaces + `var _ ConfigSource = (*StaticSource)(nil)` etc.

**Verify**: `go build ./configctl/` → exit 0

### Step 2: Envelope + StaticSource + ConnectorSource

Implement:

- `ConfigEnvelope` JSON (de)serialization and SHA-256 hash helper.
- `StaticSource` from `*common.Config`.
- `ConnectorSource` using `data.NewConnector` from bootstrap
  `configSource.connector`. Keys as specified in Architecture.
- Watch: subscribe `WatchCounterInt64` on rev key; also start poll ticker.
- On Write helper for tests: `PutEnvelope(ctx, connector, env)` that Set +
  PublishCounterInt64 (test-only or `configctl` export for admin follow-on).

**Verify**:
`go test -count=1 -run 'TestEnvelope|TestStaticSource|TestConnectorSource' ./configctl/`
→ pass. Use `DriverMemory` connector for ConnectorSource tests.

### Step 3: Diff + tiers

Implement structural diff over `*common.Config` sufficient to emit `Change`
paths for at least:

- `database.evmJsonRpcCache.policies` (slice replace)
- `projects[*].networks[*].selectionPolicy`
- `server.listenV4` (or equivalent listen field — confirm name in
  `ServerConfig`) as RestartRequired fixture

Use longest-prefix tier matching from `tiers.go`. Unknown paths default to
`TierRestartRequired` (safe default).

**Verify**: `go test -count=1 -run 'TestDiff|TestTier' ./configctl/` → pass,
including a case that classifies server listen as RestartRequired and
policies as InPlace.

### Step 4: Controller apply loop

Implement `Controller` with atomic snapshot, single-flight apply, metrics
hooks (can use existing telemetry helpers), Register/Start/ForceLoad.

Unit-test with fake Reloadables that record Apply calls.

**Verify**:
`go test -count=1 -run 'TestController_' ./configctl/` → covers:
success apply, noop same hash, apply error keeps old snapshot, restart-
required changes recorded without failing the whole revision.

### Step 5: Schema — `configSource` on `common.Config`

Add:

```go
type ConfigSourceConfig struct {
    Mode         string           `yaml:"mode,omitempty" json:"mode"` // "static"|"dynamic"
    Connector    *ConnectorConfig `yaml:"connector,omitempty" json:"connector"`
    DocumentKey  string           `yaml:"documentKey,omitempty" json:"documentKey"`
    PollInterval Duration         `yaml:"pollInterval,omitempty" json:"pollInterval"`
    ApplyTimeout Duration         `yaml:"applyTimeout,omitempty" json:"applyTimeout"`
}
```

Field on `Config`: `ConfigSource *ConfigSourceConfig \`yaml:"configSource,omitempty" ...\``

Defaults: mode=static when nil; pollInterval=2s; documentKey default
`"erpc/config"`; validate dynamic ⇒ connector required.

Extract or add `common.FinalizeConfig(cfg *Config, opts *DefaultOptions) error`
that runs LegacyTranslateFn + SetDefaults + Validate — used by LoadConfig and
by Controller. Refactor `LoadConfig` to call it (behavior-identical).

**Verify**:
`go test -count=1 -run 'Test.*Config|TestSetDefaults|TestValidate' ./common/`
→ existing tests still pass; add validation tests for dynamic-without-connector.

### Step 6: Exemplar Reloadables + Init wiring

1. Adapter `CachePoliciesReloadable` → `SetPolicies`.
2. Adapter `SelectionPolicyReloadable` → find project/network →
   `policyEngine.RegisterNetwork` with new config (mirror `Network.Bootstrap`
   registration path in `erpc/networks.go`).

Wire in `erpc.Init` / `NewERPC`:

- Build components from initial config.
- Register adapters.
- Start controller after registration.

`main.go`: construct source from bootstrap; if dynamic, Load+MergeBootstrap
before Init.

**Verify**:
Integration test with memory connector:
1. Start with bootstrap dynamic + seed envelope revision 1 (policies A,
   selection policy P1).
2. Put revision 2 with policies B → assert cache uses B (via SetPolicies
   spy or behavioral check).
3. Put revision 3 with selection policy P2 → assert RegisterNetwork called /
   engine serves new eval (follow existing policy engine test style under
   `internal/policy/`).

`LOG_LEVEL=debug go test -count=1 -run TestConfigController_ ./erpc/ ./configctl/`
→ pass.

### Step 7: Admin + metrics + docs

- Extend `erpc_config` response; add `erpc_config_reload`.
- Emit metrics listed above.
- Docs page `docs/pages/config/config-source.mdx` (YAML+TS quick-taste,
  AISection with schema table citing `common/defaults.go` permalinks,
  edge cases: bootstrap merge, restart-required, multi-replica Watch+poll,
  failed apply keeps old snapshot).
- `_meta.js` entry; do not hand-edit `.llms.txt`.
- Short note on `docs/pages/deployment/kubernetes.mdx` that dynamic mode
  can avoid restart for supported paths.

**Verify**: `make fmt` && `make build` → exit 0; docs file exists; admin
method registered (grep / unit test).

### Step 8: Final gate

**Verify ALL**:

- `go test -count=1 ./configctl/...` pass
- `LOG_LEVEL=debug go test -count=1 -run TestConfigController_ ./erpc/` pass
- `make build` pass
- `git status` — only in-scope paths (plus generated TS if applicable)
- Update `plans/README.md` row to DONE

## Test plan

New tests (model logging `init()` and table-driven style after nearby
packages — e.g. `data/*_test.go` for connector, `internal/policy/*_test.go`
for RegisterNetwork):

| Test | File | Cases |
|------|------|-------|
| Envelope round-trip + hash stability | `configctl/envelope_test.go` | happy, bad JSON |
| StaticSource never signals | `configctl/source_test.go` | Watch+cancel |
| ConnectorSource Load/Watch/Put | `configctl/source_test.go` | memory driver, revision bump |
| Diff policies + selectionPolicy + server | `configctl/diff_test.go` | tier classification |
| Controller success/noop/error/restart | `configctl/controller_test.go` | fake Reloadables |
| FinalizeConfig parity | `common/config_test.go` or existing | LoadConfig == Finalize path |
| E2E policies InPlace | `erpc/config_controller_test.go` | memory source |
| E2E selectionPolicy Rebuild | `erpc/config_controller_test.go` | RegisterNetwork effect |
| Validation dynamic mode | `common/validation` tests | missing connector |

Avoid `t.Parallel()` with gock. Prefer memory connector over mocked HTTP.

## Done criteria

- [ ] `go build ./configctl/ ./erpc/ ./cmd/erpc/` exits 0
- [ ] `go test -count=1 ./configctl/...` exits 0
- [ ] E2E tests for both exemplars pass
- [ ] Omitting `configSource` preserves today’s static behavior (existing
      config load tests still pass)
- [ ] New `configSource` documented under `docs/pages/config/` with AISection
- [ ] Admin exposes revision/mode + reload kick
- [ ] Metrics exist for reload success/error/revision
- [ ] No production path performs full `NewERPC` swap for reload
- [ ] No files outside Scope modified
- [ ] `plans/README.md` status row → DONE

## STOP conditions

Stop and report back (do not improvise) if:

- Drift check shows `LoadConfig` / `Init` / `Connector` interface changed in
  ways that invalidate the excerpts above.
- Implementing MergeBootstrap cleanly requires redesigning how cache
  connectors are constructed (escalate — may need a follow-on plan before
  exemplars can wire).
- `policy.Engine.RegisterNetwork` cannot be called safely post-bootstrap
  without deadlocks (re-read engine.go; if true, switch Rebuild exemplar to
  another existing seam and report).
- `SetPolicies` is not safe under concurrent request traffic (add mutex or
  STOP).
- Tygo/TS generation is mandatory in CI for the new field but the generate
  toolchain is broken — fix generate or STOP; do not hand-edit huge
  `generated.ts` without following repo process.
- Scope creep pressure to implement upstream lifecycle inside 001 — refuse;
  keep exemplars only.

## Maintenance notes

- **Every new hot-reloadable feature** must: (1) add/adjust a tiers.go rule,
  (2) implement `Reloadable`, (3) register in Init, (4) extend the docs
  "Hot-reload support matrix" table, (5) add a focused apply test.
- Reviewers should scrutinize: apply ordering, failed-apply atomicity
  (old snapshot retained), bootstrap merge (dynamic payload must not hijack
  listen ports), and that request hot paths only `atomic.Load` the snapshot
  or component-local pointers — never block on connector I/O.
- Follow-ons (separate plans): upstream registry add/drain/remove; network
  `ReplaceFailsafe`; rate-limiter topology rebuild; auth strategy reload;
  admin `erpc_config_apply`; optional seed-on-boot from bootstrap into empty
  store; simulator migration onto Controller.
- Multi-replica correctness depends on writers bumping revision **and**
  publishing the rev counter; document that external writers must do both
  (provide `configctl.PutEnvelope` as the supported write helper).
