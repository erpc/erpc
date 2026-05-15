# Traffic Simulator — Implementation Plan

**Branch**: `feat/traffic-simulator` (off `feat/selection-policy-rewrite` once it merges to main).
**Spec**: [feature.md](feature.md) — single source of truth.
**Goal**: Ship a single Go binary that boots a browser UI for tuning eRPC selection policies against synthetic traffic, using real `internal/policy.Engine` and real `upstream.Upstream` objects against a loopback fake-server.

The plan is incremental — each phase is shippable and independently testable. Total estimate: ~4.5 focused days to Phase 4 (full MVP); Phase 5 is its own arc.

---

## Glossary

- **DELETE-WHOLE** — remove the entire file.
- **DELETE-LINES** — remove specific lines/symbols within an otherwise-kept file.
- **REPLACE** — rewrite a section / function / type.
- **NEW** — create a new file/symbol.
- **KEEP** — touch only to verify; should not change as part of this work.

---

## Decisions resolved during spec drafting (locked in)

1. **Loopback fake-server, not mock transport.** `SimUpstream` is a real `upstream.Upstream`. Endpoint `http://127.0.0.1:<port>/sim/<id>` hits the simulator's own HTTP server. Rationale: uses 100% real eRPC code path; cheaper than building a parallel mock transport; future Phase-5 full-config import is a one-line endpoint rewrite.

2. **Policy-only forwarding in MVP.** Each "simulated request" calls `Engine.GetOrdered(network, method)` and directly invokes `winner.Forward(...)`. No `network.Forward` (which would bring in failsafe / hedging / retry / cache). Phase 5 swaps in the full network forward path.

3. **GUI stack: vanilla HTML + ES modules + Chart.js + CodeMirror 6.** Zero build, zero node, zero transpile. Everything embedded via `embed.FS`. CodeMirror 6 over Monaco because Monaco is ~5 MB + needs a worker; CM6 is ~150 KB with basic JS highlighting and is plenty for editing one function body.

4. **WebSocket for streaming.** Single bidirectional stream (`/api/events`). Server pushes; client doesn't speak (config goes through REST). Bounded fan-out channel; drop oldest on slow consumer.

5. **Single port.** GUI + API + WS + fake-server all mount on the same `http.ServeMux`. Default `8080`. Operators don't manage multiple ports.

6. **Co-located packages.** `cmd/erpc-simulator/` + `internal/simulator/`. Same module, same `go build`. Imports `common`, `internal/policy`, `internal/policy/stdlib`, `upstream`, `health` directly — no replication.

7. **Reuses `selectionPolicy.evalFunc` canonical name.** The simulator's policy editor and API use the same field name as production. Same stdlib install, same default policy.

8. **No persistent state in MVP.** Every boot starts from `--config` or the bundled defaults. Saving runs is Phase 5.

---

## Phase 0 — Skeleton (½ day)

**Goal**: One Go binary, one port, embedded HTML saying "hello", `/api/state` returns hardcoded sample state.

### 0.1 New files

- [ ] **NEW** `cmd/erpc-simulator/main.go`
  - `cli/v3` flag parsing: `--port`, `--config`, `--erpc-config` (Phase 5, parsed but unused).
  - Initialize logger.
  - Build `internal/simulator.Server`; mount on `:port`.
  - Graceful shutdown on SIGINT/SIGTERM.

- [ ] **NEW** `internal/simulator/server.go`
  - `type Server struct { mux *http.ServeMux; engine *Engine; logger *zerolog.Logger }`
  - `NewServer(cfg *Config, log *zerolog.Logger) (*Server, error)` — wires routes.
  - Routes registered (handlers are stubs):
    - `GET /` → serves embedded `index.html`.
    - `GET /api/state` → returns the in-memory state as JSON.
    - All other endpoints return `501 Not Implemented`.
  - `func (s *Server) ListenAndServe(addr string) error` — wraps `http.Server`.

- [ ] **NEW** `internal/simulator/config.go`
  - `type Config`, `type UpstreamSpec`, `type TrafficSpec`, `type NetworkSpec` — matches the YAML shape in feature.md §3.2.
  - YAML/JSON tags on every field.
  - `func LoadConfigOrDefaults(path string) (*Config, error)` — returns built-in defaults if path is empty.
  - `func DefaultConfig() *Config` — the 4-upstream / paused-traffic / default-policy bundle.

- [ ] **NEW** `internal/simulator/assets/index.html`
  - Minimal HTML with a `<script>` block that fetches `/api/state` and renders the JSON pretty-printed in a `<pre>`.

- [ ] **NEW** `internal/simulator/assets/embed.go`
  - `//go:embed *.html *.js *.css vendor/*` → exposes `Files embed.FS`.

### 0.2 Verification

- [ ] `go build ./cmd/erpc-simulator/...` succeeds.
- [ ] `go run ./cmd/erpc-simulator --port 8080` boots, browser shows the JSON state.
- [ ] No tests yet — phase 0 is a skeleton.

### 0.3 Acceptance

The binary builds, serves a page, returns a state snapshot. That's it.

---

## Phase 1 — Fake-server + one real upstream end-to-end (1 day)

**Goal**: Build the fake-server. Build a real `upstream.Upstream` pointed at it. Make one call. Confirm `health.Tracker` observed it.

### 1.1 New files

- [ ] **NEW** `internal/simulator/upstreams.go`
  - `type SimUpstream struct { Spec *UpstreamSpec; Real common.Upstream }` — `Real` is the real `*upstream.Upstream`.
  - `func buildSimUpstream(spec *UpstreamSpec, baseURL string) (*SimUpstream, error)`
    - Construct a `*common.UpstreamConfig` with:
      - `Id`, `Type: common.UpstreamTypeEvm`, `VendorName: spec.Vendor`, `Tags: spec.Tags`.
      - `Endpoint: baseURL + "/sim/" + spec.Id`.
      - `Evm: &common.EvmUpstreamConfig{ ChainId: networkChainId }` (parsed from the config's `network.id`).
      - `IgnoreMethods` / `AllowMethods` derived from `spec.MethodGlobs` (negation handled).
    - Call `upstream.NewUpstream(ctx, &log, projectId, cfg, ...)` to build the real upstream.
  - `func rebuildSimUpstreams(state *State, baseURL string) ([]common.Upstream, error)` — used on any spec change.

- [ ] **NEW** `internal/simulator/fakeserver.go`
  - `handleSim(w http.ResponseWriter, r *http.Request)` — mounted at `/sim/`.
  - Steps: parse path → look up spec → if `!available` close conn → if method not allowed return `-32601` → sleep `base ± jitter` → maybe error → return canned response.
  - Method-to-response map per feature.md §4.4.

- [ ] **NEW** `internal/simulator/state.go`
  - `type State struct { mu sync.RWMutex; cfg *Config; sims []*SimUpstream; chainHeight atomic.Int64 }`
  - `(s *State) UpdateUpstream(id string, spec *UpstreamSpec) error`
  - `(s *State) Get() *Config` — clone-on-read.
  - Goroutine to tick `chainHeight` every 2 s.

### 1.2 Engine wiring

- [ ] **NEW** `internal/simulator/engine.go`
  - `type Engine struct { state *State; policy *policy.Engine; tracker *health.Tracker; ... }`
  - `NewEngine(cfg *Config, baseURL string, log *zerolog.Logger) (*Engine, error)`:
    1. Build `health.Tracker`.
    2. Build sims via `buildSimUpstream`.
    3. Build `*policy.Engine` (same constructor as production).
    4. Register network: `policy.RegisterNetwork(cfg.Network.Id, func(){return commonUpstreams}, &SelectionPolicyConfig{EvalFunc: cfg.Network.EvalFunc, EvalInterval: cfg.Network.EvalInterval, EvalTimeout: cfg.Network.EvalTimeout})`.
  - `func (e *Engine) SimulateOnce(method string) (*SelectionEvent, error)`:
    1. `ordered := e.policy.GetOrdered(network, method)`.
    2. If empty → emit `no-upstream` event, return.
    3. `winner := ordered[0]`.
    4. Build a `NormalizedRequest`.
    5. `resp, err := winner.Forward(ctx, req, true, false)` — `byPassMethodExclusion=true` because we already pre-filtered via specs.
    6. Build `SelectionEvent` with top-5.

### 1.3 Policy hot-reload helper

The current `policy.Engine` doesn't expose an in-place update API. Add one:

- [ ] **MODIFY** `internal/policy/engine.go` — `UpdateNetworkPolicy(networkID string, cfg *common.SelectionPolicyConfig) error`. Internally: `e.UnregisterNetwork(networkID); return e.RegisterNetwork(networkID, registeredUpstreamsFn, cfg)`. Keep the existing upstreamsFn registered for that network. (If we can't reach the old fn cleanly, the simulator can pass its own closure.) **OR**: keep things outside `internal/policy` and just do `Unregister + Register` from the simulator side — that works without core changes. Decision in implementation: prefer the side-only approach unless tests require otherwise.

### 1.4 Verification

- [ ] **NEW** `internal/simulator/engine_test.go`
  - `TestSimulateOnce_HappyPath` — 2 upstreams, identity policy, fire 100 calls. Assert both got called. Assert latencies match specs (± jitter).
  - `TestSimulateOnce_RespectsAvailability` — toggle `available: false` on one upstream. Fire 50 calls. Assert all hit the other.
  - `TestSimulateOnce_RespectsMethodGlobs` — one upstream rejects `eth_traceTransaction`. Fire mixed traffic. Assert traces only hit the other.

### 1.5 Acceptance

Two real upstreams, one fake-server, real `policy.Engine`, real `health.Tracker`. Single `SimulateOnce` call returns a sensible `SelectionEvent`. Tests pass under `-race`.

---

## Phase 2 — Traffic loop + WebSocket fan-out (1 day)

**Goal**: A goroutine fires `SimulateOnce` at the configured `rps`. Events stream to subscribed WS clients.

### 2.1 New files

- [ ] **NEW** `internal/simulator/traffic.go`
  - `type TrafficGen struct { state *State; engine *Engine; bus *EventBus; stopCh chan struct{}; ... }`
  - `func (t *TrafficGen) Run(ctx context.Context)`:
    - Loop: `dt := time.Duration(1e9 / rps)`. `time.Sleep(dt)`.
    - Pick method by weighted random.
    - `evt := t.engine.SimulateOnce(method)`.
    - `t.bus.Publish(evt)`.
    - If `paused` or `rps==0`, sleep 100 ms and re-check.
  - Re-read `rps`/`methods`/`paused` from `state.Get()` on every tick (cheap; state is small).

- [ ] **NEW** `internal/simulator/events.go`
  - `type EventBus` with `Subscribe() (<-chan Event, func())` (returns the channel + unsubscribe).
  - `Publish(e Event)` — non-blocking fan-out; drops to slow subscribers (channel capacity 256).
  - `type Event` is an interface; concrete types: `SelectionEvent`, `StatsEvent`, `PolicyReloadEvent`, `EvalErrorEvent`.

- [ ] **NEW** `internal/simulator/stats.go`
  - Goroutine ticking every 500 ms.
  - Reads `health.Tracker` per-upstream method metrics for the network's "*" slot.
  - Reads `policy.Engine.GetOrdered(network, "*")` for position.
  - Emits `StatsEvent` to the bus.

### 2.2 Server wiring

- [ ] **MODIFY** `internal/simulator/server.go`
  - Add `/api/events` handler — upgrades to WebSocket (use `nhooyr.io/websocket` or `golang.org/x/net/websocket`; pick whichever is already in go.mod, otherwise nhooyr).
  - Per-client subscriber goroutine: reads from `bus`, writes JSON to the WS connection. On error/close, calls the unsubscribe func.

### 2.3 Verification

- [ ] **NEW** `internal/simulator/traffic_test.go`
  - `TestTrafficGen_HitsRPS` — set `rps=20`, run for 1 s, assert call count is in `[15, 25]` (loose).
  - `TestTrafficGen_PausedDoesNothing` — set `paused=true`, wait 500 ms, assert 0 calls.

- [ ] **Manual smoke**: `websocat ws://localhost:8080/api/events` while traffic runs; observe JSON events.

### 2.4 Acceptance

Traffic generator runs at `rps`. Events stream to any WS subscriber. Stats fire every 500 ms. Pause/resume reflects within ~100 ms.

---

## Phase 3 — GUI (1.5 days)

**Goal**: Browser shows the full layout from feature.md §5. All knobs work.

### 3.1 Vendored deps

Download once and commit under `internal/simulator/assets/vendor/`:

- [ ] `chart.js` (UMD, ≤ 200 KB minified)
- [ ] `chartjs-plugin-streaming` (≤ 30 KB) — for live-scrolling time axis
- [ ] `codemirror-bundle.js` (CodeMirror 6 with JS lang, ≤ 200 KB) — pre-built single-bundle from the CM6 starter

Update `embed.go` glob to pick them up.

### 3.2 HTML layout

- [ ] **REPLACE** `internal/simulator/assets/index.html`
  - Three-column flexbox per feature.md §5.2.
  - Top bar with status badges (running / policy-valid).
  - Empty containers for each panel; population done by `app.js`.

### 3.3 Frontend logic

- [ ] **NEW** `internal/simulator/assets/app.js` (ES module)
  - On load: `fetch('/api/state')` → render upstream cards, populate editor with `evalFunc`, populate traffic panel.
  - Open WebSocket to `/api/events`; route incoming events:
    - `selection` → prepend to event log (cap at 50)
    - `stats` → update per-upstream cards + push to chart streaming dataset
    - `policy_reload` → flash status badge
    - `eval_error` → red row in event log
  - Wire knob changes: `oninput` on each slider → debounced (200 ms) `POST /api/upstreams`.
  - Wire "Apply" button on editor → `POST /api/policy` with current source; render compile error inline if 400.
  - Wire traffic panel → `POST /api/traffic` on any change.

- [ ] **NEW** `internal/simulator/assets/style.css`
  - Minimal: layout, card shape, slider styling. Use system fonts. Dark-mode-aware via `prefers-color-scheme`.

### 3.4 Server endpoints

- [ ] **MODIFY** `internal/simulator/server.go`
  - Implement `POST /api/upstreams`, `POST /api/policy`, `POST /api/traffic`, `POST /api/reset`.
  - Each handler validates, mutates `state`, and (for policy) calls `engine.UpdatePolicy(src)`.
  - `engine.UpdatePolicy(src)`:
    1. `prog, err := common.CompileProgram(src)` — if err, return as `{ ok: false, error, line, col }`.
    2. Unregister + re-register the network on `policy.Engine`.
    3. Bus.Publish a `PolicyReloadEvent{ok: true}`.

### 3.5 Verification

- [ ] **Manual**: every interaction from feature.md §5 works. Tweaks reflect in < 1 s. Policy compile errors render inline. Chart updates smoothly with no flicker.
- [ ] **NEW** `internal/simulator/server_test.go` — integration tests for each POST endpoint (in-process httptest).

### 3.6 Acceptance

A new user can land on `http://localhost:8080/`, modify a knob, see the chart shift, edit the policy, hit Apply, and see the new behavior — all without opening a terminal.

---

## Phase 4 — Polish + the wow moment (1 day)

**Goal**: Make the demo feel inevitable. Smooth animations. Clear error states. README + GIF.

### 4.1 UX polish

- [ ] Smooth Chart.js streaming — no full re-draw, no flicker on stats tick.
- [ ] Policy compile errors highlight the failing line in the editor (use CodeMirror 6's `lintGutter` + a single `Diagnostic`).
- [ ] Per-upstream "inject failure" toggle that sets `available: false` for a click-configurable duration (default 30 s) and auto-reverts. Wired through `POST /api/upstreams`.
- [ ] Reset button on the top bar — `POST /api/reset` + clear local stats.
- [ ] Friendly empty states (no events yet, traffic paused, no eligible upstream).

### 4.2 Stdlib autocomplete

- [ ] Pre-populate CodeMirror's completion source with the stdlib primitives. Just static suggestions (no semantic LSP); enough to surface the API.
  - `byTag`, `byVendor`, `byId`, `preferTag`, `preferVendor`, `sortByScore`, `sortByLatency`, `removeByErrorRate`, `removeByLag`, `removeCordoned`, `keepHealthy`, `stickyPrimary`, `probeExcluded`, `whenEmpty`, `if`, `methodMatches`, `isFinalityRequest`.

### 4.3 Docs + demo

- [ ] **NEW** `cmd/erpc-simulator/README.md`
  - Quick start (one command).
  - The 4 demo workflows: "tune a slow upstream", "test a fallback tier", "rewrite the policy live", "watch the score adapt".
- [ ] **NEW** `docs/pages/tools/traffic-simulator.mdx` — public-docs page.
- [ ] Record a ≤ 30 s screen-cap GIF showing the rebuild-policy-live workflow; embed in README + docs.

### 4.4 Acceptance

The simulator is production-quality for the four demo workflows. Anyone running `erpc-simulator` against the bundled defaults can complete each workflow inside 30 s.

**Cumulative estimate to here: ~4.5 focused days.**

---

## Phase 5 — Full `erpc.yaml` import (deferred)

**Goal**: `--erpc-config /path/to/erpc.yaml` boots the simulator with the full eRPC stack against synthetic upstreams.

### 5.1 Loader

- [ ] **NEW** `internal/simulator/loader.go`
  - Read the user's config via `common.LoadConfig(fs, path, opts)` — already supports both YAML and TS through the unified pipeline.
  - For every `UpstreamConfig.Endpoint`: rewrite to `http://127.0.0.1:<port>/sim/<gen-id>` and record `gen-id → original-endpoint` in a side table (for display only).
  - Auto-generate `SimUpstreamSpec` per upstream — synthesize plausible defaults (`baseLatencyMs: 40, jitterMs: 20, errorRate: 0, available: true`), pre-fill `tags` from `UpstreamConfig.Tags`.
  - Skip the auth + database + rate-limiter + tracing sections of the config (simulator-isolated).
  - Hand the rewritten `*common.Config` to the same code path eRPC uses to bootstrap a project: `erpc.NewERPC(...)` against an in-memory upstream registry.

### 5.2 Forward path

Instead of `winner.Forward(...)` directly, route through `*Network.Forward(...)` — exercises failsafe, hedge, retry, cache. The simulator's traffic generator becomes a thin loop around `network.Forward`.

### 5.3 GUI updates

- [ ] Network dropdown (top bar) when multiple networks present.
- [ ] Upstream cards group by network.
- [ ] Per-method evaluation when `evalPerMethod=true`.

### 5.4 Risk: tracing + database

The full eRPC stack pulls in OpenTelemetry tracing and any configured database connectors. Both must be no-op'd in the simulator:

- Tracing: install a no-op exporter regardless of config.
- Database: rewrite `database.evmJsonRpcCache.connectors` to a single in-memory connector at config load time (same as the sanitization yq we used during the prod-manifest test).

### 5.5 Acceptance

`erpc-simulator --erpc-config /Users/aram/.../erpc.yaml` boots, all networks register, all upstreams point at `/sim/<gen-id>`, the simulator's GUI shows every network's upstream list, and per-network traffic generators work independently.

---

## Acceptance criteria (full project)

- [ ] **Faithfulness**: `internal/simulator/policy.go` does not contain a mock for `internal/policy.Engine`. The engine the simulator runs is byte-for-byte the production engine.
- [ ] **Same shape as production config**: the simulator's `evalFunc` field, the `tags` slice, every stdlib primitive, the default policy — all are the literal production names.
- [ ] **Zero new third-party Go deps** beyond what eRPC already vendors (sobek, zerolog, prometheus, gopkg.in/yaml.v3, etc.) — plus one WebSocket lib (nhooyr or x/net) and the embedded JS vendors.
- [ ] **Race-clean** under `go test -race -count=1 ./cmd/erpc-simulator/... ./internal/simulator/...`.
- [ ] **Build size**: the final binary < 50 MB stripped.
- [ ] **Boot time**: `erpc-simulator --port 8080` to page-renders-in-browser in < 2 s.

---

## Open questions / risks

1. **`health.Tracker` lifecycle.** Production wires the tracker into a `Project` and an `UpstreamRegistry`. The simulator skips those — does the tracker need them, or can it run standalone? Quick spike: build a `health.Tracker` directly with a network + project label, register a couple of upstream IDs, observe a request via `RecordUpstreamRequest`. If it works, no scaffold needed; if not, we extract a minimal helper.

2. **`policy.Engine` registration without a real `Project`.** Verify the engine's slot ticker fires cleanly when the registration is direct (not via `erpc.NewERPC`). Same spike as above.

3. **WebSocket library choice.** `nhooyr.io/websocket` is the cleanest API but adds a dep. `golang.org/x/net/websocket` is older but already commonly transitive. Pick at start of Phase 2.

4. **Method count cardinality.** If the operator's method-mix table grows large (50+ methods), the Prometheus `policy_selection_*` labels can explode. Cap at 16 methods in the UI; warn if more.

5. **Race-detector budget.** The simulator's traffic loop, event bus, and engine ticker are three concurrent goroutines. CI must run race-mode without timing out. Mitigation: per-phase tests are small + scoped; the traffic-loop test uses a short window (1 s) and a low rps (20).

6. **Default policy + tier:fallback.** The bundled defaults should include one `tier:fallback` upstream so the user can see `preferTag` working out of the box. Confirmed in §3.3 of the spec.

---

## Out of scope for this plan

- The simulator's GUI is a one-page tool. No multi-page navigation, no per-experiment URL routes.
- No SaaS deployment story. The simulator runs locally only.
- No auth. If anyone exposes it beyond localhost, that's their problem to solve (a flag-gated `--bind 0.0.0.0` is OK to add but defaults to `127.0.0.1`).
- No support for non-EVM architectures in the bundled defaults. The fake-server response shapes are EVM-specific. A user can supply any string for `methods[]`, but the canned responses won't be valid for non-EVM RPC dialects.
