# Traffic Simulator — Implementation Plan

**Branch**: `feat/traffic-simulator` (off `feat/selection-policy-rewrite` once it merges to main).
**Spec**: [feature.md](feature.md) — single source of truth for behavior, API, performance targets.
**Goal**: Ship `bin/erpc-simulator` — a single Go binary that boots real `erpc.NewERPC(...)` against a loopback fake-server, serves an embedded browser UI for live tuning, and sustains 10k rps while streaming events to the GUI.

The plan is incremental — every phase ships independently and is testable end-to-end. Estimated total: ~8 focused days to v1.

---

## Glossary

- **DELETE-WHOLE** — remove the entire file.
- **DELETE-LINES** — remove specific lines/symbols.
- **REPLACE** — rewrite a section / function / type.
- **NEW** — create a new file/symbol.
- **MODIFY** — small, targeted change in an existing file.

---

## Decisions resolved during spec drafting (locked in)

1. **Real eRPC stack, fake upstream transport.** The simulator imports `erpc.NewERPC(...)` and runs the FULL production code path — selection policy, failsafe, hedge, retry, timeout, circuit-breaker, metrics tracker, EVM integrity, cache. The ONLY mock is the upstream HTTP response, served by a loopback `/sim/{id}` handler inside the same binary.

2. **Full eRPC config is the primary input.** Not synthesized; not subset. The operator pastes / drops their real `erpc.yaml` (or `erpc.ts`). The simulator parses it via `common.LoadConfig` — same loader, same legacy translator — then rewrites every `upstream.endpoint` to `http://127.0.0.1:<port>/sim/<gen-id>` at apply time. Tracing → no-op exporter. Database connectors → in-memory only. RateLimiter store → memory.

3. **10k rps target on a developer laptop.** Drives the event-pipeline design: no per-request WS frames. Instead three tiers: in-process atomic counters → 100ms aggregated `StatsFrame` → 1% trace sampling for `TraceFrame`. Binary frames (msgpack), capped at ~70 KB/s of WS bandwidth.

4. **Animated flow stage, not raw events.** A particle on screen is a SAMPLED representation, throttled to ~50 particles/s regardless of actual rps. Aggregates drive the chart; animation drives intuition. (See feature.md §4.4.)

5. **YAML + knob bidirectional sync via a single AST.** CodeMirror 6 (YAML lang) shares an in-memory `yaml.Node` tree with the knob form. Knob edits mutate the AST → re-serialize preserving comments. YAML edits re-parse → re-derive knobs.

6. **TS configs are accepted at apply time, displayed text-only.** `loadConfigFromTypescript` runs; the unified pipeline produces a Config. But the editor's knob mirror requires YAML round-trip; we don't ship a TS AST mutator.

7. **Single port, embedded assets.** Default 8080. Bind to `127.0.0.1` only (CLI flag to override). Vendored Chart.js + CodeMirror 6 in `internal/simulator/assets/vendor/`. Zero node, zero bundler.

8. **Co-located packages.** `cmd/erpc-simulator/` + `internal/simulator/`. Same module, same `go build`.

---

## Phase 0 — Skeleton (½ day)

**Goal**: One binary serves an embedded "hello" page; `/api/state` returns a stub.

### 0.1 Files

- [ ] **NEW** `cmd/erpc-simulator/main.go`
  - `urfave/cli` flag parsing: `--port`, `--bind`, `--config`, `--scenario-dir`.
  - `wireLegacyTranslator()` — same hook setup as `cmd/erpc/main.go`.
  - Build `internal/simulator.Server`; `ListenAndServe`.
  - Graceful shutdown on SIGINT/SIGTERM.

- [ ] **NEW** `internal/simulator/server.go`
  - `type Server struct { mux, engine, logger }`.
  - Routes registered (handlers stubbed `501`):
    - `GET /` → embedded `index.html`.
    - `GET /api/state` → hardcoded snapshot.
    - All others → `501`.

- [ ] **NEW** `internal/simulator/assets/{index.html,embed.go}` — minimal page; `//go:embed *` exposes `Files embed.FS`.

### 0.2 Acceptance

`go run ./cmd/erpc-simulator --port 8080` boots, browser shows the state JSON. No tests yet.

---

## Phase 1 — Real eRPC boot from a config (1.5 days)

**Goal**: `--config erpc.yaml` boots the full eRPC stack with rewritten upstream endpoints. One curl to `/sim/<id>` produces a real, observed call.

### 1.1 Files

- [ ] **NEW** `internal/simulator/loader.go`
  - `func LoadAndRewrite(path, baseURL string) (*common.Config, []*UpstreamMapping, error)`
    1. `common.LoadConfig(fs, path, opts)` — full pipeline.
    2. Walk `cfg.Projects[].Upstreams` + `cfg.Projects[].Providers[].Overrides` to enumerate every upstream endpoint.
    3. Generate a stable per-upstream `genId` from `(project, networkId, original-id)` so reloads keep the same fake-URL.
    4. Rewrite each `Endpoint` to `<baseURL>/sim/<genId>`.
    5. Force-disable tracing (`cfg.Tracing = nil`).
    6. Rewrite `cfg.Database.EvmJsonRpcCache.Connectors` to a single in-memory connector.
    7. Force `cfg.RateLimiters.Store = nil` (memory).
    8. Return the mutated config + a side-table of `{ genId → originalEndpoint, displayId }` for the UI.

- [ ] **NEW** `internal/simulator/synth.go`
  - `type UpstreamSynthSpec struct { BaseLatencyMs, JitterMs, ErrorRate, TimeoutRate, ThrottleRate, BlockLagBlocks, … }`
  - `type SynthRegistry struct { mu, specs map[string]*UpstreamSynthSpec }` — concurrent-safe.
  - `(*SynthRegistry) Default(genId) *UpstreamSynthSpec` — populates from defaults.

- [ ] **NEW** `internal/simulator/fakeserver.go`
  - `handleSim(w, r, synth *SynthRegistry, chain *ChainState)` mounted at `/sim/`.
  - Parses path → looks up spec → applies behavior per feature.md §4.4 + §3.2.
  - `ChainState` is a goroutine that advances a per-network height every 2 s; the handler subtracts `blockLagBlocks` to synthesize lag.

- [ ] **NEW** `internal/simulator/engine.go`
  - `type Engine struct { erpc *erpc.ERPC; synth *SynthRegistry; chain *ChainState; cfg *common.Config; mappings []*UpstreamMapping; logger }`
  - `func NewEngine(cfgPath, baseURL string, log *zerolog.Logger) (*Engine, error)`:
    1. `cfg, mappings := loader.LoadAndRewrite(...)`.
    2. `erpc, err := erpc.NewERPC(ctx, log, cfg, ...)`.
    3. Populate default `UpstreamSynthSpec` per mapping.
  - `(*Engine) Forward(ctx, networkId, method, params) (*common.NormalizedResponse, error)` — wraps `erpc.GetProject(...).GetNetwork(...).Forward(ctx, req)`.
  - `(*Engine) ReloadConfig(yamlSrc string) error` — parse + rebuild + atomic swap (replaces `erpc.ERPC` instance, cancels old context).

### 1.2 Verification

- [ ] **NEW** `internal/simulator/engine_test.go`
  - `TestEngine_BootsFromMinimalConfig` — synthesizes a 2-upstream YAML inline, asserts `NewEngine` succeeds, asserts one `Forward` call returns a synthesized response.
  - `TestEngine_RewritesAllEndpoints` — asserts every upstream's runtime `endpoint` starts with the baseURL prefix.
  - `TestEngine_ReloadConfig_HotSwaps` — load A, reload B, assert subsequent `Forward` uses B's upstreams.

### 1.3 Acceptance

`erpc-simulator --config simulator-tests/minimal.yaml --port 8080` boots; `curl localhost:8080/sim/<id>` returns synthetic JSON-RPC. From the same binary, calling `Engine.Forward` exercises the full eRPC stack and returns a real response. Tests pass under `-race`.

---

## Phase 2 — Traffic generator + 10k-rps event pipeline (1.5 days)

**Goal**: Hit the engine at configurable rps. Events flow through the three-tier pipeline. WebSocket subscribers get binary frames at ≤ 70 KB/s.

### 2.1 Files

- [ ] **NEW** `internal/simulator/traffic.go`
  - `type TrafficGen struct { engine *Engine; state *State; bus *EventBus }`
  - `(*TrafficGen) Run(ctx)`:
    - Poisson-ish ticker driven by `state.RPS()`.
    - For each tick: pick method by weighted dist, build `NormalizedRequest`, async `engine.Forward(...)`.
    - Apply shape: `poisson` / `constant` / `bursty`.
  - Pool of N worker goroutines pulling from a channel; channel-depth `4 * N` bounded — drops on overflow with a counter.

- [ ] **NEW** `internal/simulator/observer.go`
  - Hooks into the real failsafe / selection / cache code paths to record lifecycle events. Three approaches, pick at impl time:
    - (a) **Context-attached observer**: stash an `*Observer` in `ctx`; key code paths in eRPC call `observer.Record(...)` if present. Cleanest but requires sprinkling hooks across `internal/policy`, failsafe, cache, network.
    - (b) **Span hook**: register an OTel span processor; convert spans → events. No core changes; relies on existing spans being comprehensive.
    - (c) **Metric hook**: read the existing Prometheus counters every 100 ms; diff against last snapshot. Cheap but loses per-request detail (no trace sampling).
  - Likely a hybrid: (c) for the stats frames + (a) with a minimal observer interface for the trace stream. Decide in Phase 2.

- [ ] **NEW** `internal/simulator/events.go`
  - `type EventBus { Subscribe() (<-chan Frame, cancel); Publish(Frame) }`.
  - `type Frame interface { Encode() []byte }` — binary serializer (msgpack or hand-rolled).
  - Per-subscriber bounded channel (256 cap); slow subscribers drop oldest frames.

- [ ] **NEW** `internal/simulator/aggregator.go`
  - 100 ms ticker → builds `StatsFrame` from atomic counters + tracker reads.
  - Reads `policy.Engine.GetOrdered(...)` for current position.
  - Reads `health.Tracker` for per-upstream method metrics.
  - Publishes to bus.

- [ ] **NEW** `internal/simulator/sampler.go`
  - Configurable trace-sample rate (default 1% of completed requests, capped at 100/s).
  - Builds `TraceFrame` from the observer's per-request record.
  - Publishes to bus.

- [ ] **MODIFY** `internal/simulator/server.go`
  - Implement `GET /api/events` — WS upgrade, subscribe to bus, write binary frames.
  - Implement `POST /api/traffic` — update `state.Traffic` atomically.

### 2.2 Bench

- [ ] **NEW** `internal/simulator/bench/main.go` (or `_test.go`)
  - Boots the engine, generator at 10k rps, runs for 30 s headless.
  - Verifies: 0 forward-loop panics, p99 wall-clock latency < 2× synthetic baseline, RSS < 1 GB, dropped-events counter < 0.1% of total.

### 2.3 Verification

- [ ] `TestTrafficGen_HitsRPS` — set 1000 rps, run 1 s, assert calls in `[850, 1150]`.
- [ ] `TestEventPipeline_NoLoss_AtSubscriberSide` — slow subscriber drops; fast subscriber gets full stream.
- [ ] `TestAggregator_StatsFrameShape` — every field non-nil, values within expected ranges.

### 2.4 Acceptance

Headless bench sustains 10k rps. `websocat ws://localhost:8080/api/events` while traffic runs shows readable binary frames decoding to sensible numbers.

---

## Phase 3 — Configuration UI (1.5 days)

**Goal**: Browser shows the YAML editor, knob mirror, paste / drop support, validation.

### 3.1 Vendored deps (committed under `assets/vendor/`)

- [ ] CodeMirror 6 with YAML + JSON langs (single-bundle, ~250 KB).
- [ ] (later phases) Chart.js + chartjs-plugin-streaming.

### 3.2 Frontend

- [ ] **NEW** `internal/simulator/assets/app.js` (ES module)
  - Boot: `fetch('/api/state')` → render initial state.
  - Open WS, route frames by type.
  - Config editor: CodeMirror 6 init, YAML mode, "Apply" / `Cmd-Enter`.
  - Knob mirror: derived from `yaml.parse(currentSrc)`; bidirectional sync via shared AST.
  - Paste / drop handler on the editor; reads file → POSTs to `/api/config`.
  - Compile-error rendering: inline diagnostics via CodeMirror's `lintGutter`.

- [ ] **NEW** `internal/simulator/assets/yaml-ast.js`
  - Tiny `yaml.Node`-style parser sufficient for the knob mirror. Could use the `yaml` npm pkg bundled — verify size first.

- [ ] **NEW** `internal/simulator/assets/style.css`
  - Three-column responsive grid. Dark-mode aware.

### 3.3 Server endpoints

- [ ] **MODIFY** `internal/simulator/server.go`
  - Implement `POST /api/config { yaml }`:
    1. Tee the YAML to a temp file (because `common.LoadConfig` takes a filesystem path).
    2. `common.LoadConfig(fs, path, opts)`.
    3. On error: return `400 { errors: [{line, col, msg}] }`.
    4. On OK: `engine.ReloadConfig(yamlSrc)`; publish `ConfigSwapFrame`.
  - Implement `POST /api/upstreams/{id}` — update synth atomic.

### 3.4 Verification

- [ ] **Manual**: paste a 100-line eRPC YAML, hit Apply, see knob mirror populate, edit a knob, see the YAML re-render with the edit.
- [ ] **Integration test**: in-process httptest server, `POST /api/config` with the sanitized prod manifest, assert 200 + `/api/state.configValid == true`.

### 3.5 Acceptance

A new operator can drop their `erpc.yaml` onto the page and within 2 s see the full knob mirror populated. Editing either side syncs the other.

---

## Phase 4 — Animated flow stage + charts + event log (1.5 days)

**Goal**: The wow visual. Animated particles, live charts, event log with full lifecycle drawer.

### 4.1 Flow stage

- [ ] **NEW** `internal/simulator/assets/flow.js`
  - SVG (or canvas) stage with three lanes (Client / eRPC / Upstreams).
  - Particle pool (recycle DOM/canvas objects; aim for 60 fps with 100 particles in flight).
  - On `TraceFrame`: spawn a particle, animate it left-to-right along its lane, color by outcome (green/yellow/orange/red/blue/gray per §4.4 of feature.md).
  - On hedge: spawn a SECOND particle that branches to the hedge upstream; whichever finishes first dims the other.
  - Hover lane → tooltip with current per-upstream stats.
  - Click particle → side drawer with full lifecycle.

### 4.2 Charts

- [ ] **NEW** `internal/simulator/assets/charts.js`
  - Selection-share stacked-area chart (Chart.js + streaming).
  - Per-upstream stat cards (driven by `StatsFrame.upstreams[]`).
  - Failsafe activity strip (retry-rate, hedge-rate, timeout-rate, CB-state per upstream).

### 4.3 Event log

- [ ] **NEW** `internal/simulator/assets/eventlog.js`
  - Last 50 sampled traces. Row click → drawer with full lifecycle.
  - Drawer renders selection trail (the ordered upstream list and which got picked) + every attempt with timing breakdown.

### 4.4 Acceptance

`./erpc-simulator --config prod.yaml`; set traffic to 5000 rps; tweak `errorRate` on one upstream → within 2 s see (a) the chart's selection-share for that upstream collapse, (b) red particles for failed requests, (c) the eval-error counter NOT increment (because keepHealthy filtered it out before scoring).

---

## Phase 5 — Scenarios + upstream knobs + polish (1 day)

**Goal**: The bundled scenario library is functional. Per-upstream knobs are smooth. Polish + docs + GIF.

### 5.1 Scenario engine

- [ ] **NEW** `internal/simulator/scenario.go`
  - YAML schema per feature.md §10.
  - `(*Engine) RunScenario(name string)` — non-blocking; emits `ScenarioFrame` events.
  - Mutates `state.UpstreamSynth` and `state.Traffic` on a timeline.

- [ ] **NEW** `internal/simulator/scenarios/*.yaml`
  - `gradual-degradation.yaml`, `vendor-region-outage.yaml`, `slow-network.yaml`, `traffic-spike.yaml`, `hedge-saturator.yaml`, `circuit-breaker-trip.yaml`.

### 5.2 Upstream-knobs panel

- [ ] **MODIFY** `app.js` — bind every knob to a debounced `POST /api/upstreams/{id}`. Surface bulk-actions ("outage all vendor=alchemy"). Failure-injection quick-button (60 s outage auto-revert).

### 5.3 Documentation

- [ ] **NEW** `cmd/erpc-simulator/README.md` — quickstart + the four canonical workflows.
- [ ] **NEW** `docs/pages/tools/traffic-simulator.mdx` — public docs.
- [ ] Record a 30 s GIF (paste a config → trigger scenario → watch flow + chart).

### 5.4 Acceptance

A new operator can: (1) paste their config, (2) pick `gradual-degradation`, (3) watch their failsafe + selection policy handle it. End-to-end inside 60 s. Cumulative estimate: ~6 focused days.

---

## Phase 6 — Bench mode + replay (stretch, ~2 days)

- [ ] `--bench-mode` — no GUI, no animation, just outputs a JSON report after running a named scenario for N seconds. Usable in CI to gate config changes.
- [ ] Export-import: dump the current state + last 60 s of traces to a file; `--replay <file>` reloads it.
- [ ] A/B compare: run two configs against the same scenario in parallel; report SLA delta per upstream.

---

## Acceptance criteria (full project)

- [ ] **Faithfulness**: `internal/simulator/` contains NO mock of `internal/policy`, `failsafe`, `health.Tracker`, `data.Cache`, `upstream.Upstream`, or `erpc.Network`. Only the loopback HTTP handler `handleSim` synthesizes anything.
- [ ] **Same config shape as prod**: the YAML the operator pastes is the YAML they ship — same loader, same translator, same field names.
- [ ] **10k rps sustained** in `--bench-mode` for 5 minutes on an M-series Mac. p99 traffic-loop overhead ≤ 50 µs / req beyond synthetic baseline.
- [ ] **Browser stays smooth** (60 fps animation) at 10k rps with GUI open.
- [ ] **Build size** ≤ 80 MB stripped.
- [ ] **Boot time** ≤ 2 s from CLI invocation to first browser paint.
- [ ] **Zero new third-party Go deps** beyond what eRPC already vendors + one WebSocket lib (`nhooyr.io/websocket`) + msgpack codec (`github.com/vmihailenco/msgpack/v5`).
- [ ] **Race-clean** under `go test -race -count=1 ./cmd/erpc-simulator/... ./internal/simulator/...`.

---

## Open questions / risks

1. **Observer hook coverage.** Option (a) from §2.1 of Phase 2 needs hooks sprinkled across `internal/policy`, failsafe, network, cache. Lots of code touched. Mitigation: start with (b) span-based events and only add explicit hooks where spans don't capture enough detail (likely: hedge winner attribution, retry-budget-consumed-but-rejected events).

2. **eRPC's failsafe is in-house** (see prior merge of `10513c82`). The simulator must use that exact failsafe stack; verify all observability hooks fire (retry counters, hedge counters, CB transitions are visible to the observer/spans).

3. **Tracing tear-down**. The simulator force-disables OTel exporters at config-load time, but the global tracer provider is set once per process. Need to confirm: switching configs across reloads doesn't try to re-init the global. Likely answer: install a no-op provider at simulator boot and never call any other tracing init.

4. **YAML AST library**. Go's `gopkg.in/yaml.v3` has `yaml.Node` with line-numbers + comments. JS-side, the `yaml` npm pkg (Eemeli Aro) is the gold standard but is ~250 KB bundled. Verify total page weight stays under our 1 MB target with CodeMirror + Chart.js + yaml + chartjs-streaming.

5. **CB / retry / hedge attribution in the observer**. Failsafe wraps multiple layers; we need to know which attempt corresponds to which strategy. The simulator's observer hook must record `(strategy, attempt#, upstreamId, outcome, latencyMs)` per attempt. May require adding a context-attached event sink to the failsafe code paths — small but real change.

6. **At 10k rps, message-pack encoding of `StatsFrame`** at 10 Hz × upstream-count × field-count is ~5 KB/frame. WS bandwidth easily fits. But JSON encoding the same on the browser side could spike GC; consider streaming the binary frames straight into a Float32Array-backed ring buffer in JS.

7. **Knob ↔ YAML AST sync UX**. Editing the YAML by hand mid-typing shouldn't constantly redraw knobs — debounce 200 ms after last keystroke. Editing a knob also shouldn't cause a full editor reflow — only mutate the affected lines. The implementation is finicky; budget a half-day for polish.

---

## Out of scope for this plan

- Production deployment. The simulator is local-only.
- Authentication / multi-user.
- Replay against a REAL remote eRPC instance (would need network-level state injection — out of scope; the simulator owns its own erpc.ERPC).
- A drag-and-drop visual policy builder. The policy is JS source; the editor is a text editor with linting.
- Saving simulation runs to a database (memory-only state; Phase 6 export/import is the only persistence path).
