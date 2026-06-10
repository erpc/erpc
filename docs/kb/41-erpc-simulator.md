# KB: eRPC Simulator

> status: complete
> source-dirs: cmd/erpc-simulator/, internal/simulator/

## L1 — Capability (CTO view)

The eRPC simulator is a local browser-based playground that runs a real `erpc.ERPC` instance against synthetic ("fake") upstreams, driving it with configurable synthetic traffic to visualize routing behavior, selection-policy decisions, failsafe mechanics, and upstream health dynamics — all in real time without requiring live provider credentials. Every dot in the browser is a real `Network.Forward()` call; the only synthetic pieces are the upstream HTTP servers and the traffic generator. It is the primary tool for developing and validating selection policies, understanding failover behavior, and reproducing production routing issues offline.

## L2 — Mechanics (staff-engineer view)

**Architecture overview.** The simulator is a single Go binary (`cmd/erpc-simulator/main.go`) embedding all browser assets via `//go:embed all:web`. It binds one HTTP server on `127.0.0.1:8080` (configurable via `-addr`) serving: static assets at `/` (React + Babel JSX over vanilla JS, no bundler required) and a WebSocket endpoint at `/ws`. Behind the WebSocket lives an `Orchestrator` that owns the real `*erpc.ERPC` instance, a synthetic `UpstreamHub` HTTP server (on a random loopback port), and rolling stats counters (`internal/simulator/orchestrator.go:L51-L95`).

**Traffic generation.** Traffic generation is browser-side. The JavaScript simulator (`cmd/erpc-simulator/web/sim-runtime.js`) ticks at the configured RPS and for each tick builds a `send-batch` WebSocket frame with one or more `{id, method, params}` items. The backend's `Session.execute` goroutine calls `Orchestrator.Execute(ctx, method, params)` for each item, which calls `net.Forward(rctx, req)` — eRPC's real network handler. The real request path runs: selection policy → failsafe (retry/timeout/hedge/consensus) → HTTP transport to the fake upstream → response parsing. After Forward returns, `Execute` reads `req.ExecState().UpstreamAttemptLog()` to build a `TraceEvent` and appends it to the session's trace queue (`internal/simulator/orchestrator.go:L597-L708`).

**UpstreamHub.** A single `http.Server` multiplexed across N synthetic upstreams at paths `/sim/<id>`. Each request: (1) looks up `UpstreamKnobs` under RLock; (2) if `Available=false`, immediately returns HTTP 503; (3) rolls three thresholds in order — throttle rate (fast 429 before sleep), timeout rate (hang 30s post-sleep), error rate (HTTP 500/502/503 or JSON-RPC -32000/-32603 in a 5-way split); (4) on success, calls `synthResult` to produce a plausible method-specific JSON-RPC response. Latency is sampled as `max(1ms, base + |gaussian()|*jitter*0.6)` (`internal/simulator/upstream_sim.go:L401-L468`, `upstream_sim.go:L723-L730`). Batch requests are handled by routing each item through `respondOne` and stitching results; a single transport error aborts the whole batch (`upstream_sim.go:L351-L395`).

**Block model.** Each upstream maintains an independent `atomic.Int64` head, advanced by a single hub-wide goroutine ticking every 250ms (`upstream_sim.go:L135-L177`). Per-upstream `BlockTimeMs` (default 12,000ms) determines step size. `BlockLag` is subtracted from head at response time. A hub-level `hubHead` tracks the maximum head across all upstreams; new upstreams are seeded at `22_000_000` (plausible mainnet head, `upstream_sim.go:L95`). Block timestamps use `simGenesisUnix + block * simBlockTimeSec` (fixed 12s, `upstream_sim.go:L44-L63`) so the health tracker's block-time EMA converges correctly.

**Data availability model.** For sparse-data methods (`eth_getTransactionReceipt`, `eth_getBlockByHash`, `eth_getLogs`, `eth_getTransactionByHash`), a deterministic FNV-like hash of `(upstreamID, method, params)` determines whether the upstream "has" the data for that query (`upstream_sim.go:L655-L669`). Baselines: receipt/tx 80%, block-by-hash 90%, getLogs 85%. The `DataAvailability` knob (0..1) multiplies the baseline; at exactly 1.0 it overrides to 100% (never miss); at 0 it always misses (`upstream_sim.go:L613-L648`).

**WebSocket protocol.** JSON text frames with a `kind` discriminator. Client-to-server: `hello`, `send-batch`, `send-one`, `set-knob`, `validate-policy`, `apply-policy`, `validate-config`, `apply-config`, `set-paused`, `start-scenario`, `stop-scenario`. Server-to-client: `snapshot` (initial state on hello), `stats` (200ms periodic), `traces` (50ms batched, up to 256 items), `policy-result`, `policy-history` (piggybacked on stats, last 16 decisions), `knob-changed`, `config-result`, `config-validate-result`, `policy-validate-result`, `error`. The session's outbound channel holds 256 frames; when full, frames are dropped silently (stats rebuild from rolling counters anyway) (`internal/simulator/ws.go:L19-L64`, `ws.go:L352-L358`).

**Config apply flow.** When the browser sends `apply-config`, the YAML MUST have the `{SELECTION_POLICY_FUNC}` placeholder already substituted by the frontend (it sends the full policy source inline). The backend: (1) `DecodeConfigYAML` — YAML decode + legacy migration, `KnownFields(true)`; (2) `RewriteEndpoints` — every upstream's `endpoint` overwritten to `http://<hubAddr>/sim/<id>`, vendor type forced to `evm`; (3) `FinalizeConfig` — `SetDefaults` + `Validate`; (4) `syncUpstreamKnobs` — creates/refreshes/orphan-drops knob entries; (5) `erpc.NewERPC` + `Bootstrap` + `GetNetwork` + `net.Bootstrap`; (6) `net.SetPolicyStepLogEnabled(true)` — step trail always on in simulator; (7) re-apply pause state to new engine. Previous eRPC instance is replaced atomically; in-flight requests against the old pointer continue until done (`internal/simulator/orchestrator.go:L251-L298`).

**Selection policy live edit.** `apply-policy` compiles the JS via `common.CompileProgram` (sobek), then injects the source into the stored YAML via `injectPolicyIntoYAML` and re-boots (`orchestrator.go:L396-L421`). `validate-policy` compiles only, no apply. The frontend gets `validate-policy` for debounced lint and `apply-policy` on explicit "Apply" button press.

**Scenarios.** Six built-in scripted scenarios (`internal/simulator/scenarios.go`):
- `gradual-degradation` (90s): progressively degrades `alc-eth-1` error rate from 5% → 70%
- `vendor-region-outage` (60s): makes Alchemy us-east region unavailable then restores
- `traffic-spike` (60s): degrades all upstreams' latency and error rate then recovers
- `hedge-saturator` (60s): increases jitter on all upstreams, then slow-pins specific upstreams
- `policy-exclude-cascade` (50s): pushes different failure modes onto three different upstreams
- `slow-network` (60s): triples latency and doubles jitter on all upstreams

Scenarios advance via a 250ms `scenarioLoop` ticker; each step fires `at-or-after` its declared elapsed-seconds (`scenarios.go:L114-L145`). Running a scenario directly calls `Orchestrator.UpdateUpstream` with patches — the same code path as manual UI slider changes.

**Pause.** `SetPaused(true)` stops `Execute` from forwarding (returns nil) AND pauses the policy engine's eval ticker via `net.SetPolicyEnginePaused(true)`. This keeps the position/score pills in the UI stable while paused. Both state flags re-sync when a config/policy apply lands mid-pause (`orchestrator.go:L219-L234`, `orchestrator.go:L289-L296`).

**Rolling stats.** A 1-second bucket rolls every second. `Stats()` returns the previous second's bucket for the RPS display. Per-upstream rolling latency samples keep the last 300 values for p50/p70/p90/p95 displayed in the upstream panel. Policy scores and health tracker metrics are read live from the engine and tracker on every `Stats()` call — the simulator has no parallel bookkeeping (`orchestrator.go:L804-L970`).

**Dump file.** `--dump-file <path>` enables a JSONL forensic log. Each event kind: `boot`, `config.snapshot`, `config.apply`, `policy.apply`, `knob.change`, `knob.inject`, `scenario.start`, `scenario.stop`, `scenario.event`, `paused`, `request`, `stats`. The `request` event carries the full `TraceEvent` (per-attempt log, selection trail, outcome) plus raw params and JSON body. A companion `*.AGENTS.md` is written once at open with the schema, field descriptions, and idiomatic `jq` queries for investigation (`internal/simulator/dump.go`). The buffer is 64KB, flushed every 1s. A crash loses at most ~1s of trailing events.

**Seed YAML.** The default configuration is `simulator.SeedYAML` (a `const` in `internal/simulator/config.go`) — one project `sim`, one network `evm:1`, eight upstreams (alc-eth-1, alc-eth-2, drpc-eth-1, quik-eth-1, chainstack-eth-1, conduit-eth-1, self-eth-1, public-eth-1). Notably, `scoreMetricsWindowSize: 10s` and `selectionPolicy.evalInterval: 1s` are deliberately tighter than production defaults so health dynamics are visible in seconds. The frontend displays `SeedYAML` (with `{SELECTION_POLICY_FUNC}` placeholder) for its "↺ default" button; the orchestrator boots from `SeedYAMLExpanded` (placeholder substituted at process init via `init()`) (`config.go:L12-L22`, `config.go:L43-L196`).

**Legacy config migration.** The simulator wires `common.LegacyTranslateFn = legacy.TranslateFromConfig` (same as the production binary) so the YAML editor accepts old-style config without errors (`cmd/erpc-simulator/main.go:L73`). `AttemptErrorDetailMaxLen` is set to 0 (unlimited) so the lifecycle drawer shows the full per-attempt error chain (`main.go:L79`).

## L3 — Exhaustive reference (agent view)

### Config fields (CLI flags)

| # | Flag | Type | Default | Behavior |
|---|------|------|---------|----------|
| 1 | `-addr` | string | `"127.0.0.1:8080"` | Bind address for the UI HTTP server and `/ws` WebSocket. `cmd/erpc-simulator/main.go:L83` |
| 2 | `-log-level` | string | `"warn"` | zerolog level for the in-process eRPC instance. Valid: `trace`, `debug`, `info`, `warn`, `error`. `main.go:L84` |
| 3 | `-web-dir` | string | `""` (disabled) | Serve UI assets from this disk directory instead of the embedded filesystem. Useful for hot-reloading JSX/CSS without Go rebuilds. `main.go:L86-L91` |
| 4 | `-dump-file` | string | `""` (disabled) | JSONL dump file path. When non-empty, every observable event is written here. A companion `*.AGENTS.md` is created beside it. `main.go:L93-L99` |

### UpstreamKnobs fields (per-upstream live-tunable behavior)

All fields are readable/writable via the `set-knob` WebSocket message with an `UpstreamKnobPatch` (pointer fields; nil = unchanged).

| # | Field | JSON key | Type | Default (standard) | Default (premium tag) | Default (fallback tag) | Behavior |
|---|-------|----------|------|--------------------|-----------------------|------------------------|----------|
| 1 | `ID` | `id` | string | (from YAML) | — | — | Upstream identity key. Immutable. `internal/simulator/types.go:L47` |
| 2 | `Vendor` | `vendor` | string | (from YAML vendorName) | — | — | Vendor display name. `types.go:L48` |
| 3 | `Tags` | `tags` | []string | (from YAML) | — | — | Tags propagated to health tracker and policy stdlib `u.tags`. `types.go:L49` |
| 4 | `Endpoint` | `endpoint` | string | `http://<hubAddr>/sim/<id>` | — | — | Loopback URL rewritten at boot. `types.go:L50` |
| 5 | `BaseLatencyMs` | `base` | float64 | `50` | `35` | `180` | Mean latency in ms. Latency model: `max(1ms, base + |gaussian()|*jitter*0.6)`. `defaults.go:L14`, `upstream_sim.go:L723-L730` |
| 6 | `JitterMs` | `jitter` | float64 | `20` | `12` | `110` | Gaussian sigma multiplier for latency variance. `defaults.go:L15` |
| 7 | `ErrorRate` | `error` | float64 | `0.01` | `0.003` | `0.080` | Probability [0..1] of an error response. Error mix: 5-way random (HTTP 500, 502, 503, JSON-RPC -32000 revert, -32603). `defaults.go:L16`, `upstream_sim.go:L448-L465` |
| 8 | `TimeoutRate` | `timeoutRate` | float64 | `0.002` | `0.002` | `0.020` | Probability [0..1] of hanging 30s (simulating eRPC timeout). Evaluated after throttle check. `defaults.go:L17`, `upstream_sim.go:L436-L442` |
| 9 | `ThrottleRate` | `throttleRate` | float64 | `0.002` | `0.002` | `0.040` | Probability [0..1] of HTTP 429 (fast reject before sleep). First check in outcome model. `defaults.go:L18`, `upstream_sim.go:L420-L423` |
| 10 | `BlockLag` | `blockLag` | int | `0` | `0` | `4` | Blocks behind hub head reported in `eth_blockNumber` responses. `defaults.go:L19`, `upstream_sim.go:L485-L486` |
| 11 | `BlockTimeMs` | `blockTimeMs` | int | `12000` | `12000` | `12000` | Milliseconds between block advances for this upstream. 0 or negative treated as 12000. `defaults.go:L20`, `upstream_sim.go:L148-L152` |
| 12 | `Available` | `available` | bool | `true` | `true` | `true` | If false, upstream returns immediate HTTP 503. `defaults.go:L21`, `upstream_sim.go:L332-L337` |
| 13 | `DataAvailability` | `dataAvailability` | float64 | `1.0` | `1.0` | `1.0` | Multiplier [0..1] on per-method data baseline. 1.0 = always has data; 0 = always misses; 0.5 = half the baseline rate. `defaults.go:L22-L23`, `upstream_sim.go:L613-L648` |
| 14 | `InjectFailureUntilMs` | `injectFailureUntilMs` | int64 | `0` | — | — | Unix-ms deadline for injected failure override: forces `errorRate ≥ 0.9, timeoutRate ≥ 0.3`. 0 = disabled. UI "inject 60s failure" button sets this. `types.go:L69-L77`, `upstream_sim.go:L405-L413` |

**Default bias rules** (`internal/simulator/defaults.go`):
- Tag `premium`: `base=35`, `jitter=12`, `error=0.003`
- Tag `dedicated`: `base=45`, `jitter=18`, `error=0.005`
- Tag `tier:fallback`: `base=180`, `jitter=110`, `error=0.080`, `timeoutRate=0.020`, `throttleRate=0.040`, `blockLag=4`
- Vendor `self-hosted`: `base=28`, `jitter=12`, `error=0.02`
- Vendor `public` (no fallback tag): `error=0.08`, `base=180`, `jitter=110`, `blockLag=4`

### Behaviors & algorithms

**Outcome roll order** (per request, first match wins, `upstream_sim.go:L401-L465`):
1. `roll < throttleRate` → HTTP 429 (no sleep)
2. Sleep `max(1ms, base + |gaussian()|*jitter*0.6)`
3. `(roll - throttleRate) < timeoutRate` → hang 30s (simulates timeout)
4. `(roll - throttleRate - timeoutRate) < errorRate` → error (5-way mix)
5. Otherwise → success + `synthResult`

**InjectedFailure override** (`types.go:L75-L77`): active when `InjectFailureUntilMs > 0` AND `now.UnixMilli() < InjectFailureUntilMs`. While active: `errRate = max(errRate, 0.9)`, `timeoutRate = max(timeoutRate, 0.3)`.

**Latency formula** (`upstream_sim.go:L723-L730`):
```
sigma = |gaussian()|   // Box-Muller standard normal, absolute value
ms = base + sigma * jitter * 0.6
ms = max(1, ms)
delay = ms * time.Millisecond
```

**synthResult method coverage** (`upstream_sim.go:L481-L608`):
- `eth_blockNumber` — hex(head - blockLag)
- `eth_chainId` — `"0x1"` (always, regardless of configured chainId)
- `net_version` — `"1"`
- `eth_syncing` — `false`
- `eth_getBalance` — deterministic non-zero value from params hash
- `eth_getCode` — synthetic bytecode (always non-empty)
- `eth_call` — synthetic non-empty result
- `eth_estimateGas` — `"0x5208"` (21000)
- `eth_gasPrice` — `"0x3b9aca00"` (1 gwei)
- `eth_getTransactionCount` — deterministic 0-999 nonce
- `eth_getBlockByNumber` — block with number, hash, parentHash, timestamp, empty transactions; null for blocks above head
- `eth_getBlockByHash` — 90% data availability baseline (scaled by knob)
- `eth_getTransactionReceipt` — 80% baseline (scaled by knob)
- `eth_getTransactionByHash` — 80% baseline (scaled by knob)
- `eth_getLogs` — 85% baseline; returns one synthetic log entry on hit, empty array on miss
- `eth_feeHistory` — minimal stub
- all others — `"0x0"`

**WebSocket frame shapes (wire schema)**:

Client → server frames:
```json
{"kind":"hello"}
{"kind":"send-batch","items":[{"id":123,"method":"eth_blockNumber","params":[]},...]}
{"kind":"send-one","req":{"id":123,"method":"eth_blockNumber","params":[]}}
{"kind":"set-knob","id":"alc-eth-1","patch":{"error":0.1,"base":200}}
{"kind":"apply-policy","src":"<JS source>"}
{"kind":"validate-policy","src":"<JS source>"}
{"kind":"apply-config","yaml":"<YAML with policy inlined>"}
{"kind":"validate-config","yaml":"<YAML>"}
{"kind":"set-paused","paused":true}
{"kind":"start-scenario","name":"gradual-degradation"}
{"kind":"stop-scenario"}
```

Server → client frames:
```json
{"kind":"snapshot","body":{"yaml":"...","defaultYaml":"...","upstreams":[...],"policy":"...","defaultPolicy":"...","paused":false,"serverTimeMs":1234567890}}
{"kind":"stats","body":{"tick":...,"actualRps":...,"lastSecondTotal":...,"lastSecondSucc":...,"lastSecondErr":...,"lastSecondCache":...,"lastSecondRetryOk":...,"lastSecondHedge":...,"lastSecondMiss":...,"perUpstreamLastS":{...},"upstreams":{...},"policyLastSwitchMs":...,"scenario":...}}
{"kind":"traces","body":{"items":[...]}}
{"kind":"policy-history","body":{"items":[...]}}
{"kind":"policy-result","body":{"ok":true,"compiledAt":1234567890}}
{"kind":"policy-validate-result","body":{"ok":false,"error":"ReferenceError: x is not defined"}}
{"kind":"config-result","body":{"ok":true,"appliedAt":1234567890}}
{"kind":"config-validate-result","body":{"ok":false,"error":"yaml decode: ..."}}
{"kind":"knob-changed","body":{...UpstreamKnobs...}}
{"kind":"error","body":{"msg":"unknown kind: foo"}}
```

**TraceEvent outcome values**: `"ok"`, `"retry-ok"`, `"hedge-win"`, `"fail"`, `"cache-hit"` (`types.go:L107`).

**TraceAttempt selReason values**: `"primary"`, `"retry"`, `"hedge"`, `"consensus_slot"`, `"sweep"` (`types.go:L165`).

**StatsFrame upstreams map** — key is upstream ID, value is `UpstreamStatsRow`:
- `errorRate`, `throttleRate`, `misbehaviorRate` — from `health.Tracker.GetUpstreamMethodMetrics(up, "*", DataFinalityStateAll)`
- `p50`, `p70`, `p90`, `p95` — from `ResponseQuantiles.GetQuantile` in ms
- `blockHeadLag`, `finalizationLag` — block count from tracker
- `blockHeadLagSeconds`, `finalizationLagSeconds` — block count × tracker's EMA block-time in seconds
- `cordoned`, `cordonedReason` — from tracker cordon state
- `score` — from `Engine.GetScores("*")` — the JS `sortByScore` value (lower = better)
- `position` — 0 = primary, 1..N = fallback order, -1 = excluded by policy
- `selectionsLast` — count in the previous 1-second bucket

**PolicyDecisionFrame** (in `policy-history` frames):
- `id` — `<network>/<method>/<unix-ms>` slug
- `steps[]` — per-stdlib-step chain trail (always populated in simulator; production has it disabled)
- `metrics` — per-upstream metric snapshot the eval saw at that tick
- `perUpstreamScore` — JS-produced scores from that tick
- `excluded[]` — excluded upstreams with step and reason

**Response body preview cap**: 8KB, with truncation markers `"\n…[truncated]…\n"` if exceeded (`orchestrator.go:L713-L729`).

**WebSocket read limit**: 8MB per message (`ws.go:L53`).

**Per-request context timeout**: 30s hard cap in `Execute`, independent of eRPC's failsafe timeout (`orchestrator.go:L617`).

### Observability

The simulator does NOT export Prometheus metrics. Observability surfaces are:
- **WebSocket `stats` frames** — 200ms periodic aggregate including per-upstream health, RPS, latency, policy positions and scores
- **WebSocket `traces` frames** — 50ms batched per-request lifecycle with full attempt log
- **WebSocket `policy-history` frames** — piggybacked on stats frames, last 16 policy eval decisions with step trail and metric snapshots
- **`-dump-file` JSONL** — offline forensic log with every event type; companion `*.AGENTS.md` contains the investigation guide and idiomatic jq queries
- **stderr** — zerolog output for the in-process eRPC instance at level controlled by `-log-level` (default: `warn`)
- **Dumper flush warning on stderr** if the buffered writer fails

### Edge cases & gotchas

1. **`{SELECTION_POLICY_FUNC}` placeholder must be substituted by the browser before sending `apply-config`**. The backend does contain a safety net in `New()` and `expandPolicyPlaceholder` that substitutes the default policy if the placeholder leaks through (e.g. from a direct API call without a browser), but the browser is the authoritative substitution site (`config.go:L148-L153`, `orchestrator.go:L153`).

2. **`DecodeConfigYAML` uses `KnownFields(true)`** — any unrecognized YAML key causes a hard decode error. Typos in the YAML editor return an error immediately rather than silently ignoring the field (`config.go:L208`).

3. **`eth_chainId` always returns `"0x1"` regardless of configured chainId** in the SeedYAML (`upstream_sim.go:L489`). This is a known simplification for the simulator — the chain ID in `SeedYAML` is `1` (Ethereum mainnet) to match.

4. **Batch upstream request: first transport error aborts the whole batch** (`upstream_sim.go:L358-L364`). This matches real HTTP behavior where a 429 or 5xx on a batch means the whole transport failed.

5. **Orphan knob cleanup**: when `apply-config` removes an upstream from the YAML, its knob is dropped from the hub AND its rolling stats entry is deleted. Live operator-tuned values for that upstream are permanently lost on config reload (`orchestrator.go:L333-L341`).

6. **Policy engine re-applies pause state after every config/policy apply** — since a fresh `*erpc.Network` is created each time, and the new engine starts running by default, without re-applying pause the UI would show drifting policy decisions while the simulator is paused (`orchestrator.go:L289-L296`).

7. **Step log is always enabled** (`net.SetPolicyStepLogEnabled(true)` called at every boot). In production this flag is off by default; the simulator always wants the full per-step chain trail for the policy-history drawer. This means policy evals in the simulator carry slightly more overhead than in production (`orchestrator.go:L287`).

8. **`upstream_sim.go` uses `math/rand` (not `crypto/rand`)** for all simulation randomness (throttle/timeout/error dispatch, Box-Muller sampling). This is intentional and correct for statistical sampling; the `#nosec G404` annotations acknowledge the deliberate choice (`upstream_sim.go:L415`, `upstream_sim.go:L449`, `upstream_sim.go:L737`, `upstream_sim.go:L738`).

9. **`-dump-file` is append-only**. If the simulator is restarted against the same path, multiple `boot` events accumulate. When investigating, always bound your query with the most recent `boot` event timestamp or use `jq 'select(.kind=="boot")'` to find session boundaries.

10. **All cached assets are `no-store`** (`main.go:L188-L193`). Every page load re-fetches all JSX/CSS files. This prevents stale-asset bugs during development but adds ~100ms to initial load time even in production use.

11. **`DataAvailability = 1.0` bypasses per-method baseline** — at exactly 1.0, `scaleAvailability` returns 100 regardless of the method's natural miss rate (`upstream_sim.go:L635-L638`). This allows operators to test pure-routing scenarios (no empty-result retries) by setting all upstreams to full data availability.

12. **The `SeedYAML` const retains the `{SELECTION_POLICY_FUNC}` placeholder** by design — it's what the WS snapshot returns as `defaultYaml` for the browser's "↺ default" button (the frontend shows the templated view). Only `SeedYAMLExpanded` has the placeholder substituted, and it's tested in `seed_boot_test.go:TestSeedYAMLExpanded_IsValid`.

### Source map

- `cmd/erpc-simulator/main.go` — entry point: CLI flags (-addr, -log-level, -web-dir, -dump-file), HTTP server setup, signal handling, 5s shutdown grace
- `cmd/erpc-simulator/web/` — embedded browser assets: React+Babel JSX UI (app.jsx, charts.jsx, flow-stage.jsx, config-editor.jsx, policy-history.jsx, upstream-knobs.jsx, selection-policy.jsx, sim-runtime.js, top-bar.jsx, bottom-tabs.jsx)
- `internal/simulator/orchestrator.go` — Orchestrator: owns erpc.ERPC, UpstreamHub, rolling stats; Execute, Stats, ApplyConfig, ApplyPolicy, UpdateUpstream, scenarios, pause
- `internal/simulator/types.go` — wire types: UpstreamKnobs, TraceEvent, TraceAttempt, StatsFrame, UpstreamStatsRow, PolicyDecisionFrame, Counters, ScenarioFrame
- `internal/simulator/upstream_sim.go` — UpstreamHub HTTP server: outcome model, latency model, synthResult, data-availability model, block advancement ticker
- `internal/simulator/ws.go` — WebSocket handler, Session (reader/writer/statsTicker/traceFlusher), protocol dispatch
- `internal/simulator/config.go` — SeedYAML const, SeedYAMLExpanded init(), DecodeConfigYAML, FinalizeConfig, RewriteEndpoints, EndpointFor
- `internal/simulator/defaults.go` — defaultKnobsFor: tag/vendor-biased initial knob values
- `internal/simulator/scenarios.go` — 6 built-in scenario definitions + StartScenario/StopScenario/advanceScenario
- `internal/simulator/dump.go` — Dumper: JSONL event recorder, flushLoop, all Log* recorders, AGENTS.md companion template
- `internal/simulator/yaml_patch.go` — injectPolicyIntoYAML, expandPolicyPlaceholder
- `internal/simulator/seed_boot_test.go` — TestSeedYAMLExpanded_IsValid: verifies init() expansion, placeholder absent, default policy body present, comments survive
