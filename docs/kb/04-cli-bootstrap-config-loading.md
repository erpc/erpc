# KB: CLI, flags, env vars, bootstrap, config loading & validation

> status: complete
> source-dirs: cmd/erpc/main.go, cmd/erpc/initflags.go, cmd/erpc/pprof.go, erpc/init.go, erpc/erpc.go, erpc/projects_registry.go, erpc/networks_registry.go, common/config.go, common/compiler.go, common/defaults.go, common/validation.go, common/runtime.go, data/shared_state_registry.go, consensus/export_utils.go, thirdparty/provider.go, util/exit.go

## L1 — Capability (CTO view)

eRPC's bootstrap layer converts a config file (YAML, TypeScript, or JavaScript) or bare command-line endpoint flags into a fully validated, default-filled `common.Config`, then orchestrates the startup sequence: database connectors, rate limiters, the ERPC core, HTTP/gRPC transports, metrics, and tracing — all bound to a context that cancels on SIGINT/SIGTERM for graceful drain. The CLI exposes a `start` command (the default action), a `validate` command that checks config and prints a structured JSON/Markdown report, and a `dump` command that renders the exact effective config the engine would run with. TypeScript configs are compiled in-process via esbuild (IIFE bundle) and evaluated with the sobek JS runtime, so closures and module-level helpers in selection-policy `evalFunc` arrow functions are preserved natively — no function serialisation.

## L2 — Mechanics (staff-engineer view)

**Startup sequence.** `main()` registers a `signal.NotifyContext` on SIGINT/SIGTERM (`cmd/erpc/main.go:L72`), builds a CLI app via `github.com/urfave/cli/v3`, and calls `cmd.Run`. For the `start`/default action, `baseCliAction` calls `getConfig` then `erpc.Init`. `erpc.Init` (`erpc/init.go:L19`) runs these steps in order:

1. Set zerolog global level from `cfg.LogLevel`.
2. Log the final config as JSON at INFO level (`erpc/init.go:L36-L43`).
3. Install histogram buckets and label filter (`erpc/init.go:L46-L57`).
4. Register a global network-alias resolver for metrics label consistency (`erpc/init.go:L59-L77`).
5. Init EVM JSON-RPC cache if `database.evmJsonRpcCache` is set (failures are warnings, not fatal; `erpc/init.go:L83-L98`).
6. Init shared-state registry if `database.sharedState` is set (same warn-on-fail pattern).
7. Build `ERPC` core via `NewERPC`, which initialises tracing, rate limiters, proxy pool, shared-state fallback, vendors registry, and projects registry (`erpc/erpc.go:L30-L107`).
8. Call `erpcInstance.Bootstrap(appCtx)` — kicks off eager network bootstrap in background goroutines (`erpc/erpc.go:L109-L111`, `erpc/projects_registry.go:L106-L115`).
9. Start HTTP server goroutine.
10. Optionally start a standalone gRPC server if gRPC is enabled and does not share the HTTP v4 port.
11. Start Prometheus metrics server.
12. Block on `<-appCtx.Done()`, then sleep `server.waitAfterShutdown` before returning.

**Config file search order.** When `--config`/positional-arg is not given and `--require-config` is not set, `getConfig` iterates a hardcoded list (`cmd/erpc/main.go:L279-L293`): `./erpc.yaml`, `./erpc.yml`, `./erpc.ts`, `./erpc.js`, `/erpc.yaml`, `/erpc.yml`, `/erpc.ts`, `/erpc.js`, `/root/erpc.yaml`, `/root/erpc.yml`, `/root/erpc.ts`, `/root/erpc.js`. The first file that `Stat` succeeds on wins. Absolute paths in the list are tried as-is; relative paths (`./…`) are joined with `os.Getwd()`.

**YAML loading.** `common.LoadConfig` reads the file, runs `os.ExpandEnv` on the raw bytes (environment variable substitution, `common/config.go:L102`), then decodes with `gopkg.in/yaml.v3` in strict mode (`KnownFields(true)` — unknown YAML keys are errors). After decode, it applies the legacy migration hook (`LegacyTranslateFn`, wired to `legacy.TranslateFromConfig` in `cmd/erpc/main.go:L36`), then `SetDefaults` and `Validate`.

**TypeScript/JS loading.** `loadConfigFromTypescript` (`common/config.go:L2686`):

1. Calls `CompileTypeScript(filename)` (`common/compiler.go:L10`) — esbuild with `Bundle:true`, `Format:IIFE`, `Target:ES2020`, `Platform:Node`, `GlobalName:"exports"`, loaders for `.ts`/`.js`/`.json`.
2. Appends the `tsLoaderWalker` JS fragment to the compiled output (`common/config.go:L2697`). The walker runs inside the IIFE and does a depth-first walk of `exports.default`, assigning sequential IDs (`fn_0`, `fn_1`, …) to every function-typed leaf, registering them in `globalThis.__erpcFns[id]`, and stamping `fn.__erpcFnId = id` on each function.
3. Compiles the combined source to a `sobek.Program` (`userScript`) via `sobek.Compile`.
4. Runs `userScript` in a temporary sobek runtime to populate `exports.default` and `__erpcFns`.
5. Checks that `exports.default` is non-nil/undefined — fails with `"config object must be default exported from TypeScript code AND must be the last statement in the file"` if missing.
6. Calls `JSON.stringify` with a replacer that converts any function whose `__erpcFnId` is set to its sentinel string `"__ts_fn__:fn_<n>"`, and drops any other functions.
7. Decodes the resulting JSON via `yaml.NewDecoder` with `KnownFields(true)` — all `UnmarshalYAML` hooks fire, providing the same strict validation as the YAML path.
8. Attaches `userScript` to `cfg.UserScript` — the policy engine's pool primer re-runs this program in each acquired sobek runtime to reconstruct `__erpcFns` with natively live functions.

**No `.toString()` round-trip.** The `EvalFunc` field in `SelectionPolicyConfig` holds either a JS source string (YAML path) or a `__ts_fn__:<id>` sentinel (TS path). When the engine evaluates a sentinel it pulls the function directly from `globalThis.__erpcFns[id]` — preserving closures and module-scope variables.

**Legacy migration.** `LegacyTranslateFn` is a package-level var in `common` (`common/config.go:L79`), nil by default. `cmd/erpc/main.go:L36` wires it to `legacy.TranslateFromConfig`. It fires after YAML decode, before `SetDefaults`, and may return deprecation warnings (emitted as zerolog Warn). Tests that exercise legacy YAML must set this var manually.

**Defaults cascade.** `Config.SetDefaults(opts)` is a deep in-order traversal that fills every nil pointer and zero value with a documented default (`common/defaults.go:L49-L176`). If `cfg.Projects` is empty, a synthetic `"main"` project with public repository + envio providers is injected and an aliasing rule is added so `/evm/<chainId>` resolves without a project prefix (`common/defaults.go:L100-L167`). `DefaultOptions.Endpoints` (from `--endpoint` flags) are injected as synthetic upstreams into the first project's upstream list when no providers/upstreams exist (`common/defaults.go:L1113-L1143`).

**Validation.** `Config.Validate()` (`common/validation.go:L15`) is called at the end of `LoadConfig`. It validates every subsection exhaustively; key rules include: `server.maxTimeout` must be non-zero; `projects` must be non-empty; each project needs at least one upstream or provider; rate limit budgets referenced by projects/upstreams/networks must exist; `selectionPolicy.evalTimeout` must be less than `evalInterval`. Validation errors cause `LoadConfig` to return an error, which causes `getConfig` to return an error and `main` to exit with code 1001.

**Lazy vs. eager network init.** `ProjectsRegistry.Bootstrap` launches background goroutines that eagerly initialise all statically-configured networks in parallel via `util.Initializer` (`erpc/projects_registry.go:L106-L115`, `erpc/networks_registry.go:L195-L212`). For networks not in the static config (e.g. a novel chain ID arriving in a request), `NetworksRegistry.GetNetwork` calls `nr.initializer.ExecuteTasks` with the caller's context — blocking until bootstrap finishes or the request context times out. The `util.Initializer` deduplicates concurrent tasks so a flood of simultaneous first requests for the same novel network only runs the bootstrap once.

**Graceful shutdown.** On SIGINT/SIGTERM: the `signal.NotifyContext` cancels `appCtx`. The HTTP server's internal goroutine wakes immediately (`erpc/http_server.go:L209`), sleeps `server.waitBeforeShutdown` (default 10s, so Kubernetes can mark the pod NotReady), then calls `srv.Shutdown(30s budget)`. The metrics server shuts down on context done with a 5s budget. `erpc.Init` blocks on `<-appCtx.Done()` and then sleeps `server.waitAfterShutdown` (default 10s) before returning, giving in-flight requests time to drain. The ERPC struct also starts a goroutine that shuts down the OTel tracer provider (5s) on context cancellation.

**pprof.** A `//go:build pprof` file (`cmd/erpc/pprof.go`) registers the standard `net/http/pprof` routes in an `init()` function and starts `http.ListenAndServe("0.0.0.0:<port>", nil)` in a goroutine. Port defaults to `6060`; override with `ERPC_PPROF_PORT`. Mutex and block profiling are enabled at rate 1. The binary must be built with `-tags pprof` to activate.

**`validate` subcommand.** Loads config via `getConfig`, calls `erpc.GenerateValidationReport` which builds a resource tree (projects → networks → upstreams, rate-limit budgets) and checks for orphan rate-limit budgets, public endpoints, static analysis issues. Output is JSON (default) or Markdown via `--format`. Exits 1 if `report.Errors` is non-empty. All zerolog output is silenced during validation via `zerolog.SetGlobalLevel(zerolog.Disabled)` (`cmd/erpc/main.go:L109`) — this is a global disable applied before config loading, so no warnings from `SetDefaults`, legacy migration, or `Validate` appear on stderr.

**`dump` subcommand.** Loads config, calls `policy.ResolveEffectiveSelectionPolicies` to fill in the effective selectionPolicy per network (as the engine would derive it at register-time), then marshals to YAML (default) or JSON via `--format`. Useful for debugging TS configs to see what `evalFunc` sentinels resolve to.

**Test harness.** `cmd/erpc/initflags.go` (build tag `!test`) honours two env vars at `init()`: `ERPC_NOLOGS=1` silences zerolog globally and replaces the logger with `io.Discard`; `ERPC_NOMETRICS=1` swaps the Prometheus default registerer/gatherer with a no-op registry. Both reduce test overhead.

## L3 — Exhaustive reference (agent view)

### Config fields

**Top-level `Config` fields** (`common/config.go:L38-L68`)

| YAML path | Type | Default | Behavior / notes |
|-----------|------|---------|-----------------|
| `logLevel` | string | `"INFO"` (`common/defaults.go:L50-L52`) | Parsed by zerolog; invalid value defaults to DEBUG with a warning (`erpc/init.go:L27-L32`). Valid values: `trace`, `debug`, `info`, `warn`, `error`, `fatal`, `panic`, `disabled`. Overridable at runtime by `LOG_LEVEL` env var (`cmd/erpc/main.go:L354-L363`). |
| `clusterKey` | string | `"erpc-default"` (`common/defaults.go:L53-L55`) | Identifies the logical replica group for shared-state scoping (`common/config.go:L40`). **Propagation:** `Config.SetDefaults` passes `c.ClusterKey` as `defClusterKey` to `Database.SetDefaults(c.ClusterKey)` (`common/defaults.go:L75`), which passes it on to `SharedStateConfig.SetDefaults(defClusterKey)` (`common/defaults.go:L831-L838`). Inside `SharedStateConfig.SetDefaults`, the value is applied only when `c.ClusterKey == ""` (`common/defaults.go:L803-L804`): an **explicit** `database.sharedState.clusterKey` in the YAML wins and is never overwritten. If `database.sharedState.clusterKey` is omitted or empty, it inherits the top-level `clusterKey`. So the precedence is: explicit `database.sharedState.clusterKey` > top-level `clusterKey` > hard-coded default `"erpc-default"`. |
| `server` | *ServerConfig | synthetic empty struct then `SetDefaults` | HTTP/gRPC server config; see 02-http-server.md. |
| `healthCheck` | *HealthCheckConfig | default mode=`networks`, defaultEval=`any:initializedUpstreams` | Health check endpoint behaviour. |
| `admin` | *AdminConfig | nil | Admin API; CORS defaults to `*` origin with no credentials. |
| `database` | *DatabaseConfig | nil | EVM JSON-RPC cache and/or shared state. |
| `projects` | []*ProjectConfig | synthetic `main` project with public+envio providers if empty (`common/defaults.go:L100-L167`) | Ordered list of project configs. |
| `rateLimiters` | *RateLimiterConfig | nil | Rate limiter budgets. |
| `metrics` | *MetricsConfig | enabled=true (non-test), port=4001, errorLabelMode=compact | Prometheus metrics server. |
| `proxyPools` | []*ProxyPoolConfig | nil | HTTP proxy pool definitions. |
| `tracing` | *TracingConfig | nil | OTel tracing; default protocol=grpc, endpoint=localhost:4317, sampleRate=1.0, serviceName=erpc. |

**MetricsConfig** (`common/config.go:L2543-L2563`, defaults at `common/defaults.go:L749-L767`)

| YAML path | Type | Default | Behavior |
|-----------|------|---------|----------|
| `metrics.enabled` | *bool | `true` in non-test; unset (nil) in test builds (`common/defaults.go:L750-L752`) | If false/nil, metrics server is not started. |
| `metrics.hostV4` | *string | `"0.0.0.0"` | Prometheus scrape endpoint IPv4 bind host. |
| `metrics.hostV6` | *string | `"[::]"` | IPv6 bind host. |
| `metrics.port` | *int | `4001` | Prometheus scrape port; validation requires non-nil when enabled. |
| `metrics.errorLabelMode` | LabelMode | `"compact"` | Controls the `error` label on `erpc_upstream_request_errors_total` etc.; `"compact"` condenses error codes, `"verbose"` emits full class names. |
| `metrics.histogramBuckets` | string | `""` (uses built-in defaults) | Comma-separated float64 bucket boundaries for request-duration histograms (`erpc/init.go:L47-L57`). Validation rejects non-float values (`common/validation.go:L151-L159`). |
| `metrics.histogramDropLabels` | []string | nil | Label names to remove from every histogram to reduce cardinality; counters/gauges are unaffected. |
| `metrics.histogramLabelOverrides` | map[string][]string | nil | Per-metric label keep-list; key is metric name without `erpc_` prefix (e.g. `"network_request_duration_seconds"`). |

**CLI flags** (`cmd/erpc/main.go:L76-L94`)

| Flag | Type | Default | Behavior |
|------|------|---------|----------|
| `--config` | string | `""` | Path to config file. When non-empty, sets `requireConfig=true` and skips auto-discovery. |
| `--endpoint` / `-e` | []string | `[]` | Zero or more upstream endpoint URLs injected as synthetic upstreams. Validated with `url.ParseRequestURI`; invalid URLs abort with exit 1001. |
| `--require-config` | bool | `false` | If true and no config file found in search path, aborts with error. |
| `validate --format` | string | `"json"` | Output format for the `validate` subcommand: `json` or `md`. |
| `dump --format` | string | `"yaml"` | Output format for the `dump` subcommand: `yaml`, `yml`, or `json`. Any other value (e.g. `csv`) triggers an `"unsupported format"` error and exits 1 (see edge case #22). |
| `--set` / `-s` | — | **COMMENTED OUT — not available** (`cmd/erpc/main.go:L86-L90`, `L207`, `L228`) | This flag was planned as a Helm-style dot-path config override (e.g. `-s logLevel=debug`) but was never implemented and is commented out of the source. Passing `-s` or `--set` to `erpc` will produce `"flag provided but not defined: -s"` from the CLI framework. Do not rely on it. |

**Positional argument.** First non-flag argument is treated as a config file path with `requireConfig` forced true (`cmd/erpc/main.go:L303-L305`).

**Environment variables reference**

All env vars consumed by the bootstrap/CLI layer. (TypeScript configs also receive the full OS environment as `process.env.<KEY>` — see the TS-config row below.)

| Env var | Source | Effect |
|---------|--------|--------|
| `LOG_LEVEL` | `cmd/erpc/main.go:L59-L66` (init-phase) and `cmd/erpc/main.go:L354-L363` (post-load override) | Zerolog global log level. Overrides `logLevel` in config file. Valid values: `trace`, `debug`, `info`, `warn`, `error`, `fatal`, `panic`, `disabled`. Invalid value falls back to `debug` with a warning. Applied twice: first in `init()` so config-loading logs respect it; second after config decode to override `cfg.LogLevel`. |
| `LOG_WRITER` | `cmd/erpc/main.go:L53-L57` | When set to `"console"`, switches zerolog from JSON to a human-readable console writer (colored, with time format `"04:05.000ms"`). Any other value (or absent) keeps the default JSON writer. Applied in `init()` before any log output. |
| `ERPC_NOLOGS` | `cmd/erpc/initflags.go` (build tag `!test`) | When `"1"`, sets zerolog global level to `Disabled` and replaces the default logger with an `io.Discard` writer. Suppresses all log output. Intended for test runs. |
| `ERPC_NOMETRICS` | `cmd/erpc/initflags.go` (build tag `!test`) | When `"1"`, swaps `prometheus.DefaultRegisterer` and `prometheus.DefaultGatherer` with a fresh empty no-op registry. Metric registrations succeed but accumulate no memory. Irreversible within the process. |
| `ERPC_PPROF_PORT` | `cmd/erpc/pprof.go` (build tag `pprof`) | Port for the pprof HTTP server. Default `6060`. Only effective when binary built with `-tags pprof`. **Security: the server binds `0.0.0.0:<port>` (all interfaces), NOT localhost-only** (`cmd/erpc/pprof.go:L24`). In production the port must be firewall-restricted or the binary built without `-tags pprof`; any host that can reach the port gets full Go runtime profiling access. |
| `FORCE_TEST_LISTEN_V4` | `common/defaults.go:L640-L644` | When `"true"` during tests (`util.IsTest()` returns true), forces `server.listenV4=true`. Without this, IPv4 listener is disabled in test builds to avoid port conflicts. |
| `INSTANCE_ID` | `data/shared_state_registry.go:L83`, `consensus/export_utils.go:L29` | First in the instance-identity priority chain (see below). Used as the `UpdatedBy` field in shared-state remote-sync writes and as the `{instanceId}` token in consensus dispute-log filenames. |
| `POD_NAME` | `data/shared_state_registry.go:L86`, `consensus/export_utils.go:L33` | Second in the priority chain (Kubernetes pod name). Used in same two contexts as `INSTANCE_ID`. |
| `HOSTNAME` | `data/shared_state_registry.go:L89`, `consensus/export_utils.go:L37` | Third in the priority chain (OS hostname env var, common in Docker). Used in same two contexts. |
| `$VAR` / `${VAR}` in YAML values | `common/config.go:L102` | `os.ExpandEnv` applied to raw YAML bytes before parse. Substitutes all `$VAR`/`${VAR}` patterns. Unset vars resolve to empty string. If result contains YAML-special chars (`:`, `{`…) and the value is unquoted, YAML will fail to parse. |
| `$VAR` / `${VAR}` in `server.responseHeaders` values | `erpc/http_server.go:L135-L148` | Expanded once at startup via `os.ExpandEnv`. If the expanded value is empty, the header entry is silently dropped (not sent). Distinct from the YAML-level expansion — runs after full config decode during HTTP server construction. |
| `$VAR` / `${VAR}` in provider-generated upstream endpoints | `thirdparty/provider.go:L67-L71` | After the vendor's `GenerateConfigs` returns each `UpstreamConfig`, `os.ExpandEnv` is called on each `Endpoint` string. Distinct from YAML-level expansion (which runs before parse on the config file bytes). Allows secrets in endpoint URLs to be injected at provider-resolution time. |
| `process.env.<KEY>` (TS config runtime) | `common/runtime.go:L19-L36` | TypeScript/JS config files receive the entire OS environment as a `process.env` object in the sobek runtime — all keys and values from `os.Environ()`. Also exposed as a flat `env` array of `"KEY=VALUE"` strings. Because `.env` is loaded in `init()` before the TS bundle is evaluated, `.env` values are available here too. This is distinct from YAML `$VAR` substitution; TS files use explicit `process.env.MY_KEY` JS expressions. |

**Instance-identity priority chain.** Two independent functions resolve the instance ID:

- `data.resolveSharedStateInstanceID()` (`data/shared_state_registry.go:L82-L98`): `INSTANCE_ID` → `POD_NAME` → `HOSTNAME` → `os.Hostname()` → `"unknown"`. Used for shared-state `UpdatedBy` field. Trims whitespace at each step.
- `consensus.getInstanceID()` (`consensus/export_utils.go:L26-L48`): `INSTANCE_ID` → `POD_NAME` → `HOSTNAME` → SHA-256 hash of `(unixNano + pid)` truncated to 8 hex chars (computed once via `sync.Once`). Used for consensus dispute-log filenames; characters problematic for filenames (`:`, `/`, `\`, ` `, `*`, `?`, `"`) are sanitized to `_` before use.

Note that the two functions share the same env-var priority order but differ in their final fallback: shared-state returns `"unknown"` while consensus generates a deterministic hash. In Kubernetes, setting `POD_NAME` (via the downward API) is the recommended approach; in bare-metal/Docker, `INSTANCE_ID` can be set explicitly or `HOSTNAME` is usually the container ID.

### Behaviors & algorithms

**Config search order** (from `cmd/erpc/main.go:L279-L293`):
1. `./erpc.yaml` (relative to `os.Getwd()`)
2. `./erpc.yml`
3. `./erpc.ts`
4. `./erpc.js`
5. `/erpc.yaml`
6. `/erpc.yml`
7. `/erpc.ts`
8. `/erpc.js`
9. `/root/erpc.yaml`
10. `/root/erpc.yml`
11. `/root/erpc.ts`
12. `/root/erpc.js`

The first path where `fs.Stat` does not return an error wins. If none match and no explicit path/require-config flag, eRPC starts with the synthetic default `main` project.

**`.env` file loading.** `github.com/joho/godotenv` loads `.env` from the current directory at `init()` time (`cmd/erpc/main.go:L42-L46`). Non-existence (`os.IsNotExist`) is silently ignored; other errors are logged at ERROR.

**Environment variable substitution (YAML only).** Before YAML decode, the raw file bytes are passed through `os.ExpandEnv` (`common/config.go:L102`). This substitutes `$VAR` and `${VAR}` patterns. TypeScript/JS files do NOT receive this substitution — they run in a JS runtime that can read env vars via `process.env` or similar if bundled.

**TypeScript function sentinel lifecycle:**
1. esbuild bundles the `.ts`/`.js` file as an IIFE, outputting a self-contained JS string.
2. The `tsLoaderWalker` fragment is appended — a IIFE that depth-first walks `exports.default` assigning IDs `fn_0`, `fn_1`, … to function values (`common/config.go:L2631-L2658`).
3. The combined source is compiled to a `sobek.Program` (the `userScript`).
4. A temporary runtime runs the program; `exports.default` holds the config object.
5. `JSON.stringify` with a replacer converts functions to `"__ts_fn__:fn_<n>"` sentinels.
6. The JSON is decoded through `yaml.Decoder` with strict validation.
7. `cfg.UserScript` is set to the compiled program.
8. At runtime, each policy-engine pool runtime runs `userScript` once (the primer) to re-populate `globalThis.__erpcFns`. Sentinels resolve to live function values via `globalThis.__erpcFns[id]`.

**Default project synthesis.** When `config.Projects` is empty after loading, `SetDefaults` injects (`common/defaults.go:L100-L167`):
- A `main` project with `repository` and `envio` providers.
- An aliasing rule matching all domains to the `main` project so `/evm/<chainId>` works without a project prefix.
- Network defaults: `evm.getLogsMaxAllowedRange=30_000`, `getLogsSplitOnError=true`, integrity enforcement enabled.
- Upstream defaults: `evm.getLogsAutoSplittingRangeThreshold=5000`.
- Network-level failsafe: retry(maxAttempts=5), timeout(120s), hedge(p70 quantile, maxCount=2).
- Upstream-level failsafe: retry(maxAttempts=1, delay=500ms), timeout(60s).

**Shorthand vendor endpoint syntax.** In `--endpoint` or in the `upstreams[].endpoint` field, non-HTTP/gRPC URLs are parsed as vendor shorthand: scheme becomes the vendor name (`alchemy://apikey` → vendor `alchemy`, apiKey=`apikey`). The shorthand is converted to a `ProviderConfig` at `SetDefaults` time via `convertUpstreamToProvider` (`common/defaults.go:L1193-L1241`). Supported vendors include: `alchemy`, `blastapi`, `drpc`, `envio`, `etherspot`, `infura`, `llama`, `pimlico`, `thirdweb`, `dwellir`, `conduit`, `superchain`, `tenderly`, `quicknode` (with `tagIds`/`tagLabels` query params), `chainstack`, `onfinality`, `blockpi`, `ankr`, `blockdaemon`, `erpc`, `repository`, `routemesh` (`common/defaults.go:L1243-L1424`). Unknown vendors return an error.

**Validation rules (key ones):**
- `server.maxTimeout` must be non-zero (`common/validation.go:L90-L92`).
- `healthCheck` must be present (synthesised by `SetDefaults`).
- `projects` must be non-empty (ensured by default injection).
- Each project needs at least one upstream or provider (`common/validation.go:L607-L609`).
- `project.*.providers.*.id` must be globally unique within the project.
- `project.*.upstreams.*.id` must be unique within the project.
- `project.*.networks.*.alias` must be unique within the project and match `[a-zA-Z0-9_-]+`.
- Referenced `rateLimitBudget` IDs must exist in `rateLimiters.budgets`.
- `onlyNetworks` and `ignoreNetworks` on a provider are mutually exclusive.
- `selectionPolicy.evalTimeout` must be strictly less than `selectionPolicy.evalInterval`.
- `selectionPolicy.evalFunc` must compile (or be a TS sentinel).
- `database.sharedState.lockMaxWait` and `updateMaxWait` must be less than `fallbackTimeout`.
- `database.sharedState.lockTtl` must be at least as long as `fallbackTimeout`.
- Connector failsafe may not use `consensus` or `hedge.quantile` (no latency source at connector level).

**Exit codes** (`util/exit.go:L7-L10`):
- `1001` (`ExitCodeERPCStartFailed`) — CLI/config load failure or `erpc.Init` error.
- `1002` (`ExitCodeHttpServerFailed`) — HTTP or gRPC server fatal error.

**`validate` command output schema** (JSON, `erpc/config_analyzer.go:L76-L123`):
```
{
  "errors":   [],
  "warnings": [],
  "notices":  [],
  "resources": {
    "totals": { "projectsTotal", "networksTotal", "upstreamsTotal", "rateLimitBudgetsTotal" },
    "tree": {
      "projects":     [ { "id", "networks": [ { "id", "alias?", "upstreams": [ { "id" } ] } ] } ],
      "rateLimiters": { "budgets": [ { "id", "rulesCount" } ] }
    }
  }
}
```
Exit 0 if `errors` is empty, exit 1 otherwise.

### Observability

The bootstrap/config layer does not emit Prometheus metrics of its own. Key log messages (zerolog structured JSON unless `LOG_WRITER=console`):

- `INFO`: `"executing command"` — logged at the start of every CLI action with fields `action`, `version`, `commit` (`cmd/erpc/main.go:L257-L263`).
- `INFO`: `""` with `config` field — final config JSON logged at INFO or lower before init (`erpc/init.go:L36-L43`). **May contain redacted secrets** (passwords serialised as `"REDACTED"` by custom MarshalJSON hooks on `RedisConnectorConfig`, `AwsAuthConfig`, `PostgreSQLConnectorConfig`, `ProviderConfig`).
- `INFO`: `"initializing eRPC core"` — logged before `NewERPC` (`erpc/init.go:L82`).
- `INFO`: `"initializing transports"` — after core init, before server start (`erpc/init.go:L110`).
- `INFO`: `"shutting down gracefully..."` — on context cancel (`erpc/init.go:L174`).
- `WARN`: `"no projects found in config; will add a default 'main' project"` — synthetic project injected (`common/defaults.go:L101`).
- `WARN`: `"no providers or upstreams found in project; will use default 'public' endpoints repository"` — public fallback injected (`common/defaults.go:L1125`).
- `WARN`: `"failed to initialize evm json rpc cache: ..."` — cache init failure is non-fatal (`erpc/init.go:L89-L91`).
- `WARN`: `"failed to initialize shared state registry: ..."` — same for shared state (`erpc/init.go:L95-L97`).
- `WARN`: `"invalid log level '...', defaulting to 'debug'"` — emitted both at init time (`cmd/erpc/main.go:L62-L65`) and at runtime if `LOG_LEVEL` has an invalid value.
- `INFO`: `"pprof server started at http://localhost:<port>"` — emitted only when built with `-tags pprof` (`cmd/erpc/pprof.go:L20`).
- `INFO`: `"networks bootstrap completed"` — emitted by `NetworksRegistry.Bootstrap` after all configured networks are initialised (`erpc/networks_registry.go:L209`).

### Edge cases & gotchas

1. **Providing `--config` sets `requireConfig=true` implicitly.** If the specified path does not exist, `LoadConfig` returns a file-not-found error and the process exits with code 1001. There is no fallback to auto-discovery when an explicit path is given (`cmd/erpc/main.go:L300-L302`).

2. **Positional argument works in ALL subcommands, including `validate` and `dump`.** `validate` and `dump` share the same `getConfig` function as `start`/default (`cmd/erpc/main.go:L111`, `L159`, `L263`). That function checks `cmd.Args().First()` at `cmd/erpc/main.go:L303-L305` and treats any first positional arg as a config path with `requireConfig=true`. Therefore `erpc validate ./my-config.yaml` and `erpc dump ./my-config.yaml` both work correctly — the positional-arg path is not restricted to the default/start action.

3. **YAML strict-mode unknown fields.** `KnownFields(true)` is set on the YAML decoder (`common/config.go:L103`). A typo in any YAML key will cause a fatal error. This applies to both the direct YAML path and the JSON-round-trip from TypeScript (the TS JSON is decoded through the same YAML decoder).

4. **`os.ExpandEnv` in YAML runs on raw bytes before parse.** A YAML value like `password: ${REDIS_PASSWORD}` is substituted before decode. If `REDIS_PASSWORD` contains YAML special characters (`:`, `{`, etc.), the resulting YAML will be malformed. Use quoted values: `password: "${REDIS_PASSWORD}"`.

5. **TypeScript default-export must be the LAST statement.** The sobek runtime's `exports.default` is inspected after the script runs. If the config object is created but not the last default export (e.g. there is a subsequent expression), the earlier export may be overwritten. Error message: `"config object must be default exported from TypeScript code AND must be the last statement in the file"` (`common/config.go:L2715-L2717`).

6. **TS functions inside arrays are not walked.** The `tsLoaderWalker` recurses into plain object properties only; array elements are iterated but only objects inside arrays are descended into — bare function-in-array is not walked. In practice selectionPolicy functions are always object property values, so this is not a concern for normal configs (`common/config.go:L2636-L2638`).

7. **`orphaned function` in TS is silently dropped.** If a function value in the config object was not reachable by the walker (e.g. due to a non-enumerable property), the JSON.stringify replacer drops it by returning `undefined` rather than emitting a string sentinel or a stringified source (`common/config.go:L2736-L2739`). The field will be missing in the decoded config.

8. **`.env` load happens before CLI parsing.** The `.env` file (if present in `os.Getwd()`) is loaded in the `init()` function before `main()` runs (`cmd/erpc/main.go:L42-L46`). This means `.env` variables ARE available for `os.ExpandEnv` substitution in YAML configs. Errors other than "file not found" are logged at ERROR but do not abort startup.

9. **`LOG_LEVEL` env var overrides config-file `logLevel` at two points.** First in the `init()` of `cmd/erpc/main.go` (applies to the config-loading phase log output); second in `getConfig` after config is loaded (overrides `cfg.LogLevel` itself, `cmd/erpc/main.go:L354-L363`). The second override means a config file setting of `logLevel: ERROR` can be trumped by `LOG_LEVEL=DEBUG` without editing the file.

10. **Metrics server disabled in test builds.** `MetricsConfig.SetDefaults` sets `Enabled` only when `!util.IsTest()` (`common/defaults.go:L750-L752`). In `go test` runs, metrics are off by default, so Prometheus counters still accumulate but are not exposed on a port. Tests that need metrics must set `metrics.enabled: true` explicitly in their config.

11. **`ERPC_NOMETRICS=1` replaces the Prometheus default registry.** `initflags.go:L22-L28` (build tag `!test`) swaps `prometheus.DefaultRegisterer` and `prometheus.DefaultGatherer` with a fresh empty registry. Any subsequent metric registrations succeed but never allocate label-pair memory. This is a process-level side-effect; it cannot be undone.

12. **pprof binary must be built with `-tags pprof`.** The `pprof.go` file has `//go:build pprof`. The standard binary does not include it. If pprof is needed in production it requires a custom build. The port is always `0.0.0.0:<port>` — not localhost-only — so it should be firewall-restricted.

13. **Eager network bootstrap runs on `appCtx`, not the caller's context.** The background `ExecuteTasks` goroutine in `NetworksRegistry.Bootstrap` uses the app-level context. If bootstrap is slow (e.g. provider network discovery), the HTTP server is already up and incoming requests for those networks will trigger the lazy path (`GetNetwork` → `initializer.ExecuteTasks` with the request context). The lazy path blocks the request until bootstrap completes or the request context times out.

14. **`grpcSharesHttpV4` gate in `Init`.** A standalone gRPC server is only started if `grpcEnabled` is true AND the gRPC v4 addr differs from the HTTP v4 addr (`erpc/init.go:L125`). If the ports are the same (the default derivation), gRPC shares the HTTP handler and no separate `grpcServer.Start` goroutine runs.

15. **Shorthand upstream URLs are consumed and removed from `p.Upstreams`.** `convertUpstreamToProvider` converts any upstream whose endpoint has a non-HTTP/gRPC scheme into a `ProviderConfig` and splices it out of `p.Upstreams` via `slices.Delete` in a backwards-iteration loop (`common/defaults.go:L1164-L1172`). After `SetDefaults`, a shorthand endpoint will NOT appear in `p.Upstreams` but WILL appear in `p.Providers`.

16. **Validate command (and dump) suppress all log output via a global zerolog disable.** `zerolog.SetGlobalLevel(zerolog.Disabled)` is called at the very start of both the `validate` and `dump` action handlers (`cmd/erpc/main.go:L109` for validate, `L156` for dump) — before `getConfig` is called. This means every zerolog call inside `LoadConfig`, `SetDefaults`, `Validate`, legacy migration, and TS compilation is silenced for the duration of the command. If a validation failure is hard to diagnose, this suppression is the reason no warnings appear on stderr; pipe stdout through `jq .errors` instead. To debug, temporarily add `LOG_LEVEL=debug` — but note that `validate` re-disables the level via `zerolog.SetGlobalLevel(zerolog.Disabled)` immediately, so the env-var override has no effect unless you modify the binary.

17. **`LOG_WRITER=console` must be set before the process starts; it cannot be changed at runtime.** The zerolog writer is configured in `init()` at `cmd/erpc/main.go:L53-L57`, before `main()` runs. There is no hot-reload mechanism. In container environments that aggregate JSON logs (Datadog, Grafana Loki, etc.), omit this variable; set it only for local dev.

18. **`INSTANCE_ID` / `POD_NAME` / `HOSTNAME` affect two separate systems with slightly different fallback logic.** The shared-state registry (`data/shared_state_registry.go:L82-L98`) falls back to `"unknown"` when `os.Hostname()` fails or returns empty. The consensus module (`consensus/export_utils.go:L26-L48`) instead falls back to a SHA-256 hash of `(time.Now().UnixNano() + os.Getpid())`, computed once. This means in a pathological environment with no env vars and `os.Hostname()` failing, a pod's shared-state `UpdatedBy` is `"unknown"` (collides across pods!) while its consensus dispute filenames get a unique 8-char hash. Setting `INSTANCE_ID` explicitly avoids both failure modes.

19. **Provider endpoint env expansion runs AFTER YAML-level expansion.** The pipeline is: (1) `os.ExpandEnv` on raw YAML bytes → YAML decode → `SetDefaults` (providers registered). Then at request time: (2) provider's `GenerateConfigs` builds endpoint strings → `expandEnvVars` calls `os.ExpandEnv` on each endpoint (`thirdparty/provider.go:L67-L71`). A `${SECRET}` in a provider's `settings` field will be expanded in step 1 (YAML-level); a `${SECRET}` inside an endpoint string returned by `GenerateConfigs` is expanded in step 2. Double-expansion is possible if the vendor builds an endpoint that itself contains `$VAR` literals.

20. **`server.responseHeaders` env expansion runs at server construction, not at request time.** The `resolvedResponseHeaders` map is built once in `NewHttpServer` (`erpc/http_server.go:L135-L148`). If the env var changes after startup (e.g. in a live container), the header value is NOT updated. Restart is required. Headers that expand to an empty string at startup are silently omitted from all responses — there is no runtime warning.

21. **TypeScript `process.env` is a snapshot, not a live view.** `common/runtime.go:L23-L29` builds `process["env"]` by iterating `os.Environ()` once when the sobek runtime is created. The map is not updated if env vars change after runtime creation. In the policy engine, each pool runtime is primed at acquisition time, so a restart-less env var change would require all policy engine runtimes to be recreated — which does not happen.

22. **`erpc dump --format csv` (or any unknown format) exits 1 via a distinct error path, not a marshal error.** The `dump` action has an explicit `default:` branch in its format switch at `cmd/erpc/main.go:L190-L194`: `fmt.Fprintf(os.Stderr, "error: unsupported format %q (use yaml or json)\n", format)` followed by `util.OsExit(1)`. This is separate from the YAML/JSON marshal-error branches at lines L183-L188 and L176-L180. The distinction matters for scripts: a marshal failure means config was loaded successfully but serialisation failed; an unsupported-format failure means the binary never attempted serialisation. Valid values are `"yaml"`, `"yml"`, and `"json"` only.

23. **`--set` / `-s` is commented out and will produce a framework-level flag error if used.** The flag definitions and the `configOverrides` variable are commented out at `cmd/erpc/main.go:L86-L90`, `L207`, `L228`, and `L297`. The urfave/cli framework does not register the flag, so passing `-s logLevel=debug` or `--set logLevel=debug` produces `"flag provided but not defined: -s"` from the framework before any application code runs, and the process exits non-zero. This is a footgun for users familiar with Helm or `kubectl --set`-style overrides. The feature has no planned release; do not attempt workarounds with creative flag naming.

### Source map

- `cmd/erpc/main.go` — CLI entry point: signal handling, CLI command definitions (`start`/`validate`/`dump`), `getConfig`, `baseCliAction`. Contains all auto-discovery paths and `--endpoint` injection.
- `cmd/erpc/initflags.go` — Build-tag `!test`: `ERPC_NOLOGS` and `ERPC_NOMETRICS` init hooks.
- `cmd/erpc/pprof.go` — Build-tag `pprof`: starts the pprof HTTP server with mutex+block profiling enabled.
- `cmd/erpc/main_test.go` — Integration tests for CLI entry point: flag forms, missing/invalid config, validate command, invalid endpoints.
- `cmd/erpc/init_test.go` — Registers test logger to reduce noise in test output.
- `erpc/init.go` — `Init(ctx, cfg, logger)`: full startup orchestration — log level, histogram config, alias resolver, database init, ERPC core, HTTP/gRPC/metrics servers, graceful drain wait.
- `erpc/erpc.go` — `ERPC` struct, `NewERPC` (initialises tracing, rate limiters, proxy pool, projects registry, admin auth), `Bootstrap` delegator.
- `erpc/projects_registry.go` — `ProjectsRegistry`: holds `preparedProjects`, `Bootstrap` (launches network eager-init goroutines), `RegisterProject`, `GetProject`, `GetAll`. Also contains `ScoreMetricsWindowSize` global (default 1 minute).
- `erpc/networks_registry.go` — `NetworksRegistry`: `Bootstrap` (eager background init of static networks), `GetNetwork` (lazy sync init via `util.Initializer`), alias registration and resolution.
- `erpc/config_analyzer.go` — `GenerateValidationReport`, `RenderValidationReportJSON`, `RenderValidationReportMarkdown`: static analysis of loaded config producing the structured validation report.
- `common/config.go` — `Config` struct and all sub-structs; `LoadConfig`; `loadConfigFromTypescript` and the `tsLoaderWalker`/`tsFunctionSentinelPrefix` constants; `IsTSFunctionSentinel`/`TSFunctionSentinelID`; `LegacyTranslateFn`/`LegacyTranslateLogger` hook vars.
- `common/compiler.go` — `CompileTypeScript` (esbuild IIFE bundle), `CompileFunction` (sobek single-function eval), `CompileProgram` (sobek.Compile with paren-wrap).
- `common/defaults.go` — `Config.SetDefaults`, all `*.SetDefaults` methods, default project synthesis, shorthand vendor URL parsing (`buildProviderSettings`, `convertUpstreamToProvider`), default cache method maps.
- `common/validation.go` — `Config.Validate` and all `*.Validate` methods; every validation rule cited above.
- `util/exit.go` — `OsExit` var (overridable in tests), `ExitCodeERPCStartFailed=1001`, `ExitCodeHttpServerFailed=1002`.
- `data/shared_state_registry.go` — `resolveSharedStateInstanceID()`: priority chain `INSTANCE_ID` → `POD_NAME` → `HOSTNAME` → `os.Hostname()` → `"unknown"`. Result stored in `sharedStateRegistry.instanceId` and stamped as `UpdatedBy` on every shared-state write.
- `consensus/export_utils.go` — `getInstanceID()`: same env-var priority chain but final fallback is a SHA-256(unixNano+pid) 8-char hex hash (computed once via `sync.Once`). Used as `{instanceId}` token in consensus dispute-log filenames.
- `common/runtime.go` — `NewRuntime()`: creates a sobek runtime, populates `process.env` map and `env` array from `os.Environ()`. Called for TypeScript selection-policy evaluation.
- `thirdparty/provider.go` — `expandEnvVars()` (L67-L71): applies `os.ExpandEnv` to each `UpstreamConfig.Endpoint` after the vendor's `GenerateConfigs` returns, enabling `${VAR}` injection in provider-generated endpoint URLs.
