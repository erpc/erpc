# Traffic Simulator — Specification

**Status**: Ready for implementation
**Owner**: TBD
**Last revised**: 2026-05-15

---

## 1. Purpose

The **Traffic Simulator** is a local browser-based playground for evaluating eRPC behavior — failsafe, selection policy, scoring — under fully controllable synthetic traffic, against the **real eRPC code paths**. The operator drops in their prod config (or starts from scratch), tunes upstream characteristics and traffic shape, and watches:

- where every request is routed, in real time;
- how the selection policy reacts when an upstream degrades;
- how failsafe (retry / hedge / timeout / circuit-breaker) compounds in incidents;
- how scoring weights and tier-fallback play out over minutes of traffic;
- whether a candidate config change would have produced better routing in a past incident.

The "wow" moment: paste your prod `erpc.yaml`, slide an upstream's `errorRate` from 0 → 0.7, and within a second see hedges fire on the chart, sticky-primary release, the policy switch to the next upstream, and the per-upstream latency histograms separate.

The simulator is a single Go binary, single port, embedded UI, **zero external services**. It boots `erpc.NewERPC(...)` directly with the operator's config, but rewrites every upstream endpoint to a loopback fake server inside the same binary. The fake server is the only mock — the policy engine, failsafe stack, hedging logic, retry budget, metrics tracker, state pollers, integrity checks, score evaluator are all production code.

### 1.1 Non-goals

- **Not a load tester.** The aggregator targets 10k rps for realistic incident reproduction — not benchmark numbers. Anyone needing per-request raw latency measurements should use a dedicated load tool.
- **Not a deployment surface.** The simulator is for tuning + validation. Its output is a tweaked YAML the operator copy-pastes back into prod.
- **Not multi-user.** Single operator on localhost. Auth + sharing is out of scope.
- **Not a replacement for the prod observability stack.** Once a config ships, traces + Prometheus take over. The simulator is the rehearsal room.

---

## 2. Architecture

```
┌────────────────────────────────────────────────────────────────────────────┐
│  Browser (single page, zero build)                                         │
│                                                                            │
│  ┌────────────────┐  ┌──────────────────────┐  ┌──────────────────────┐  │
│  │ Config editor  │  │ Traffic-flow stage   │  │ Charts + event log    │  │
│  │ (YAML rich +   │  │ (animated, left-to-  │  │ (selection over time, │  │
│  │  knob mirror;  │  │  right; sampled live │  │  per-upstream stats,  │  │
│  │  paste support)│  │  events from server) │  │  recent events)       │  │
│  └────────────────┘  └──────────────────────┘  └──────────────────────┘  │
│  ┌────────────────┐  ┌──────────────────────┐                              │
│  │ Upstream knobs │  │ Traffic generator    │   Top bar: status, rps,    │
│  │ (per-upstream  │  │ (network, method mix,│   policy validity, reset,  │
│  │  synth quality)│  │  rps, scenarios)     │   pause                    │
│  └────────────────┘  └──────────────────────┘                              │
└────────────────────────────────────────────────────────────────────────────┘
       │  REST (config + state)        │  WebSocket (events, binary frames)
       ▼                               ▼
┌────────────────────────────────────────────────────────────────────────────┐
│  cmd/erpc-simulator  (single binary, single port — default 8080)           │
│                                                                            │
│  HTTP routes                                                               │
│   GET  /                       embedded GUI                                │
│   GET  /api/state              full snapshot (Config + UpstreamSynth +    │
│                                 Traffic + stats)                           │
│   POST /api/config             validate + apply a full erpc.yaml/erpc.ts   │
│   POST /api/upstreams/{id}     per-upstream synthetic quality knobs        │
│   POST /api/traffic            generator settings                          │
│   POST /api/scenario           start a scripted scenario                   │
│   POST /api/reset              clear stats + restart                       │
│   GET  /api/events             WebSocket fan-out (binary frames)           │
│   GET  /metrics                Prometheus exposition                       │
│   ANY  /sim/{upstreamId}       fake JSON-RPC endpoint                      │
│                                                                            │
│  Real eRPC core (imported as library, NOT replicated)                      │
│  ┌──────────────────────────────────────────────────────────────────┐    │
│  │  erpc.NewERPC(cfg)                                               │    │
│  │   ├── upstream.Registry        (real)                            │    │
│  │   ├── health.Tracker            (real)                           │    │
│  │   ├── internal/policy.Engine    (real, with stdlib)              │    │
│  │   ├── failsafe stack            (real: retry, hedge, timeout,    │    │
│  │   │                              circuit-breaker — in-house impl)│    │
│  │   ├── data.Cache                (real, memory connector only)    │    │
│  │   └── erpc.Network              (real Forward path)              │    │
│  └──────────────────────────────────────────────────────────────────┘    │
│  ┌──────────────────────────────────────────────────────────────────┐    │
│  │  Synthetic layer (the ONLY mock)                                 │    │
│  │   ├── /sim/{id} handler   — synthesizes latency/errors/blockLag  │    │
│  │   ├── chainHeightTicker   — drives state-poller responses        │    │
│  │   └── UpstreamSynthSpec[] — per-id quality config                 │    │
│  └──────────────────────────────────────────────────────────────────┘    │
│  ┌──────────────────────────────────────────────────────────────────┐    │
│  │  Event pipeline (10k-rps-safe)                                   │    │
│  │   ├── per-request hook       (lifecycle observer; cheap)         │    │
│  │   ├── 100ms aggregator        (bucketed stats; primary chart src)│    │
│  │   ├── trace sampler           (1% individual events; animation + │    │
│  │   │                            event log)                        │    │
│  │   └── WebSocket fan-out       (binary frames, drop on backpress) │    │
│  └──────────────────────────────────────────────────────────────────┘    │
└────────────────────────────────────────────────────────────────────────────┘
```

### 2.1 What is real, what is synthetic

| Component | Real (production code) | Synthetic |
|---|---|---|
| Project / Network / Upstream config | ✅ exact `common.Config` shape |  |
| Selection-policy engine + stdlib + sobek | ✅ |  |
| Failsafe stack (retry/hedge/timeout/circuit-breaker) | ✅ in-house impl |  |
| Metrics tracker (`health.Tracker`) | ✅ |  |
| EVM state poller, integrity layer | ✅ |  |
| Cache (memory connector) | ✅ |  |
| `erpc.Network.Forward(...)` (the full request path) | ✅ |  |
| Upstream HTTP response |  | ✅ synthesized: base±jitter latency, errorRate, blockLag, method support |

The operator's traffic generator submits to `network.Forward(ctx, req)`. The eval engine picks a winner. The failsafe stack may retry / hedge. Each call lands on `/sim/<id>`, which synthesizes a response per that upstream's `UpstreamSynthSpec`. The tracker observes; the policy adapts; the simulator emits events.

### 2.2 Loopback fake-server trick

Same as the prior MVP design: each upstream's `endpoint` is rewritten to `http://127.0.0.1:<port>/sim/<id>` at config-load time. The fake server reads the current `UpstreamSynthSpec` for `<id>` and synthesizes a JSON-RPC response. Real failsafe + selection runs on top.

### 2.3 Where it lives

- `cmd/erpc-simulator/main.go` — CLI, embedded assets, `http.ListenAndServe`.
- `internal/simulator/`
  - `server.go` — HTTP + WS routes.
  - `engine.go` — owns the `*erpc.ERPC` instance, drives traffic.
  - `synth.go` — `UpstreamSynthSpec` + fake-server handler.
  - `config.go` — `SimState` (eRPC `*common.Config` + synth specs + traffic gen).
  - `events.go` — pipeline + WS broadcast.
  - `traffic.go` — generators (steady-rps, scenario-script).
  - `scenarios.go` — built-in scenario library (incident replays).
  - `assets/` — embedded GUI.

---

## 3. Inputs

Three categories. The operator can edit ANY input at runtime; changes apply within one `evalInterval` tick (≤ 1 s).

### 3.1 eRPC configuration (primary input)

The simulator accepts the **full eRPC config** — same `common.Config` shape, same YAML / TS schema, same `LoadConfig` pipeline. This is the operator's actual or candidate prod config.

Surface in the UI:

- **YAML editor**: rich, schema-aware, full eRPC YAML. CodeMirror 6 with YAML highlighting + inline validation against the canonical schema (synthesize from the Go struct tags via `tygo` output).
- **Paste / drop**: drag-and-drop a `.yaml` or `.ts` file, or paste into the editor. The simulator validates → re-runs `common.LoadConfig` (including the legacy translator) → if OK, swap the live config; if errors, surface inline.
- **Knob mirror**: every editable field surfaces as a knob in the side panel. Editing a knob mutates the YAML; editing the YAML re-derives the knobs. (See §6.3 for the sync model.)

Notable sub-sections the operator commonly tunes:

| Path | Knob category |
|---|---|
| `projects[].networks[].failsafe[]` | Retry maxAttempts, delay, hedge maxCount + delay quantile, timeout duration + quantile, circuitBreaker thresholds |
| `projects[].networks[].selectionPolicy` | `evalInterval`, `evalTimeout`, `evalPerMethod`, `evalFunc` (full JS, with stdlib) |
| `projects[].upstreams[]` | `tags`, `ignoreMethods`, `allowMethods`, `rateLimitBudget`, per-upstream failsafe overrides |
| `projects[].upstreamDefaults` | failsafe defaults, autoTune |
| `projects[].providers[].overrides[]` | per-`evm:*` provider tuning |
| `rateLimiters.budgets[]` | per-budget rps + burst |
| `database.evmJsonRpcCache.policies[]` | cache TTL / scope / finality |

If the operator pastes a legacy config (any of the deprecated fields the translator recognizes), the simulator runs the same migration the prod loader does and surfaces the deprecation warnings in a small banner. They see exactly what prod will warn about.

### 3.2 Upstream synthetic quality (per-upstream overlay)

For every upstream defined in the loaded config, the simulator manages a parallel `UpstreamSynthSpec`. These are NOT part of the eRPC config — they only affect the loopback fake-server's response shape:

```yaml
# Implicit overlay, edited via the upstream-knobs panel
synth:
  alc-1:
    baseLatencyMs: 40
    jitterMs: 20
    errorRate: 0.0                     # 0..1: fraction of requests returning -32099
    timeoutRate: 0.0                   # 0..1: fraction that hang for > maxLatency (forces failsafe timeout)
    throttleRate: 0.0                  # 0..1: fraction returning -32005 / 429 (drives rateLimitBudget)
    blockLagBlocks: 0                  # how far behind tip this upstream reports
    finalizationLagBlocks: 0           # finalized-tip lag
    available: true                    # false = transport-level error (connection refused)
    methodGlobs: ["*"]                 # extends the config's ignoreMethods/allowMethods
    misbehavior:
      wrongChainIdRate: 0.0            # fraction of eth_chainId returning a bogus value
      emptyResultRate: 0.0             # fraction returning [] / "0x" / null
      reorgDepth: 0                    # max blocks this upstream "regresses" on getBlockByNumber
```

| Knob | Effect on real eRPC behavior |
|---|---|
| `baseLatencyMs` / `jitterMs` | Drives p50/p95 in `health.Tracker` → score weight `respLatency` → policy reordering |
| `errorRate` | Drives `metrics.errorRate` → `keepHealthy` filter / circuit-breaker thresholds |
| `timeoutRate` | Forces hedge fire if `failsafe.hedge.delay` < timeout duration |
| `throttleRate` | Drives `metrics.throttledRate` → rateLimitAutoTune |
| `blockLagBlocks` | Drives `metrics.blockHeadLag` → `removeByLag` |
| `available: false` | Transport-level fail → retry / circuit-breaker exercise |
| `methodGlobs` | Drives `-32601` → `ErrEndpointUnsupported` → `IgnoreMethods` autotune |
| `misbehavior.*` | Drives `metrics.misbehaviorRate` → integrity checks → cordoning |

Defaults when a config is freshly loaded: every upstream gets `{ baseLatencyMs: 40, jitterMs: 20, errorRate: 0, available: true, methodGlobs: ['*'] }`. The operator opens the upstream-knobs panel to depart from steady-state.

### 3.3 Traffic generator

Defines what the simulator FIRES at `network.Forward`:

```yaml
traffic:
  network: evm:1                       # which configured network
  rps: 5000                            # target rate, 1..10000
  paused: false
  methods:                             # weighted method mix
    - { method: eth_blockNumber,        weight: 5 }
    - { method: eth_getBlockByNumber,   weight: 3, params: ["latest", false] }
    - { method: eth_call,               weight: 4 }
    - { method: eth_getLogs,            weight: 1, params: [{ fromBlock: "0x1", toBlock: "0x2" }] }
  shape:
    distribution: poisson              # poisson | constant | bursty
    burstSizeMax: 50                   # if bursty
    burstQuietMs: 1000                 # idle between bursts
  perRequest:
    headers: {}                        # echoed on requests (`User-Agent`, etc.)
    finalityHint: auto                 # auto | realtime | unfinalized | finalized
```

Surface in the UI:

- **RPS slider** (1–10000, logarithmic increments).
- **Method-mix table** (free-form method strings, weights, default params).
- **Shape selector** (Poisson / constant / bursty with two parameters).
- **Network dropdown** when more than one is configured.
- **Pause / play / step-once** controls.

### 3.4 Scenarios (scripted timelines)

A scenario is a YAML script that mutates the inputs over time, replaying an incident or stress test. Bundled scenarios in `internal/simulator/scenarios/`:

| Scenario | Timeline |
|---|---|
| `gradual-degradation` | t=0 baseline → t=60s ramp `alc-1.errorRate` from 0 to 0.5 over 30s → t=120s revert |
| `vendor-region-outage` | t=0 baseline → t=30s `alc-*.available = false` for 60s → revert |
| `slow-network` | t=0 baseline → t=20s every upstream `baseLatencyMs *= 4` for 90s |
| `traffic-spike` | t=0 rps=100 → t=30s rps=8000 (constant) for 60s → revert |
| `hedge-saturator` | high `timeoutRate` on primary → hedges fire on every request |
| `circuit-breaker-trip` | continuous high `errorRate` to trip circuit-breaker; observe re-admission |

Scenarios are first-class — the operator picks one from a dropdown, hits "play", and watches the simulator drive both the synth knobs and the traffic generator on a schedule. Useful for reproducing a past incident under a candidate config.

---

## 4. Runtime model

### 4.1 Boot

1. CLI flags parsed; `--config` (eRPC config path) optional. If absent, boot with a built-in starter config (3 synthetic networks, a few upstreams, the default selection policy).
2. `common.LoadConfig(fs, path, opts)` loads and validates the eRPC config (legacy translator fires here).
3. The simulator **rewrites every upstream's `endpoint`** to `http://127.0.0.1:<port>/sim/<gen-id>` and stashes the original-endpoint string only for display. Tracing config is force-disabled (no-op exporter). Database connectors are rewritten to a single in-memory cache. RateLimiter store is forced to in-memory.
4. `erpc.NewERPC(cfg, ...)` boots the full eRPC instance.
5. `UpstreamSynthSpec` defaults are populated for every upstream.
6. HTTP server starts: GUI + API + WS + `/sim/<id>` all on the configured port.

### 4.2 Per-request lifecycle

```
traffic-gen tick
  → pick method by weighted mix
  → build NormalizedRequest
  → erpc.Network.Forward(ctx, req)
    │
    ├── selection-policy slot returns ordered upstreams (REAL eval, REAL stdlib)
    ├── failsafe wraps: timeout + retry + hedge + circuit-breaker
    ├── attempt 1 → upstream.Forward(ctx, req)
    │     │
    │     ├── http POST http://127.0.0.1:<port>/sim/<id>
    │     ├── fake server: sleep base±jitter, maybe error/throttle/timeout/wrong-chain
    │     ├── response → tracker observes
    │     └── return up the call stack
    ├── if attempt 1 fails AND retry budget allows → attempt 2 → next upstream in order
    └── on success → cache write (if cacheable) → return to gen
  ←
emit lifecycle events to the pipeline
```

Every step in this chain is the same code that runs in prod. The simulator wraps `Network.Forward` in a thin hook that records lifecycle events (`selection`, `attempt_start`, `attempt_end`, `hedge_fire`, `retry_fire`, `cb_state_change`, `cache_hit`, `cache_miss`, `request_complete`).

### 4.3 Event pipeline (10k-rps safe)

At 10k rps, ~10 events per request × 10k = 100k events/s. The WebSocket can't carry that, and the browser can't render it. The pipeline has three tiers:

1. **In-process counters** (free). Every event increments per-(upstream, attempt-outcome, method) atomic counters. These feed the periodic stats snapshot.
2. **100 ms aggregator**. Every 100 ms, the simulator builds a `StatsFrame`: per-upstream {requests, successes, errors_by_kind, hedges, retries, p50, p90, current_score, position, cordoned_reason}. Pushed to all WS subscribers — single frame, ~2 KB binary.
3. **Sampled trace stream**. 1% of completed requests (configurable; capped at 100/s) get their full lifecycle serialized and pushed as a `TraceFrame`. The frontend uses these to drive the animated flow stage + the event log.

Subscribers get binary frames (msgpack or a hand-rolled compact encoding) — JSON encoding alone would burn a noticeable fraction of CPU at 10k rps.

### 4.4 Animated flow stage

The frontend renders a left-to-right stage with three lanes:

```
┌──────────┐     ┌────────────────┐     ┌─────────────────────┐
│  Client  │ ──► │   eRPC core    │ ──► │  Upstream pool      │
│ (gen)    │     │ selection +    │     │  alc-1 ┃┃┃┃┃        │
│          │     │ failsafe       │     │  drpc-1 ┃           │
│          │     │                │     │  chnstk-1 ┃┃        │
└──────────┘     └────────────────┘     └─────────────────────┘
```

A particle (dot) is spawned per sampled trace, leaving the Client, transiting eRPC core, fanning to the chosen upstream. Particle color encodes outcome:

| Color | Meaning |
|---|---|
| Green | success on first attempt |
| Yellow | succeeded after retry |
| Orange | won a hedge race |
| Red | failed terminally |
| Blue | cache hit (no upstream contact) |
| Gray | excluded by selection policy (never picked) |

When the policy excludes an upstream, its lane dims. When the circuit-breaker opens on an upstream, its lane gets a red border. When hedges fire on a request, you see two particles fanning from eRPC core to two different upstreams (the first to respond keeps moving; the loser dims).

Particle density is throttled to a steady ~50/s on screen regardless of actual rps — the animation is a representation, not a 1:1 view. Statistics drive the chart; the animation drives intuition.

### 4.5 Config hot-reload

`POST /api/config { yaml: "..." }`:

1. Parse via `common.LoadConfig` against the same pipeline prod uses.
2. If errors, return `{ ok: false, errors: [{ line, col, message }] }`.
3. If OK, build a fresh `*erpc.ERPC` instance with rewritten endpoints, gracefully shutdown the old one, swap. Cross-tick state (sticky primary, exclusion history) is intentionally dropped — operators are testing a CHANGED config and want a clean baseline.
4. Frontend receives a `config_swap` WS event and re-renders the knob mirror + upstream list.

### 4.6 Upstream-synth hot-reload

`POST /api/upstreams/{id} { synth: { errorRate: 0.3, ... } }`:

1. Validate.
2. Atomic swap into the in-memory `UpstreamSynthSpec` table.
3. Next request to `/sim/<id>` reads the new spec — no eRPC restart.

### 4.7 Reset

`POST /api/reset`: drop tracker state, drop selection-policy slot state, drop in-memory cache, restart traffic generator. The chart clears; the animation restarts.

---

## 5. Performance budget

Target: **10,000 rps sustained on a developer laptop** (M-series Mac or recent x86 Linux) with the GUI open in one browser tab. Concretely:

| Metric | Target | Strategy |
|---|---|---|
| `network.Forward` overhead at simulator layer | ≤ 50 µs / req beyond the synthetic sleep | All paths are real eRPC code; the simulator's wrapping is a single observer hook. |
| Event-pipeline overhead | ≤ 20 µs / req | Atomic counters only on the hot path. Aggregation happens off-thread. |
| WebSocket bandwidth | ≤ 1 Mbps | Binary frames; aggregated stats, not raw events. Sample traces at ≤ 100 frames/s. |
| Browser frame budget | 60 fps animation | Particles are pure CSS transforms + a small canvas; no React re-renders during animation. |
| Memory | < 1 GB RSS at 10k rps for 5 min | In-memory cache size-capped; tracker windowing keeps history bounded. |

To verify: load test the simulator against itself in `--no-gui --bench-mode` (Phase 5 stretch). Confirm 10k rps with median p99 within 2× the synthetic baseline latency.

---

## 6. Browser GUI

### 6.1 Layout

Four primary panels arranged in a 2-row × 3-column grid. Top bar spans full width.

```
┌────────────────────────────────────────────────────────────────────────┐
│ Top bar: status pill | rps live counter | policy valid badge | reset   │
├──────────────────────────────────────────────────────────────────────  │
│  CONFIG          │       TRAFFIC FLOW          │     CHARTS             │
│  (YAML editor    │    (animated stage,         │  (stacked selection-   │
│   + knob panel)  │     left-to-right)          │   share over 60s)      │
│                  │                             │                        │
│                  │                             │  per-upstream cards:   │
│                  │                             │   req | err | p50/p90  │
│                  │                             │   score | position     │
├──────────────────┼─────────────────────────────┼────────────────────────┤
│  UPSTREAM        │  TRAFFIC GENERATOR          │  EVENT LOG             │
│  SYNTH KNOBS     │  rps + method-mix +         │  last 50 traces        │
│  (one row per    │  shape + scenario picker    │  (sampled, with full   │
│   upstream)      │                             │   lifecycle drawer)    │
└────────────────────────────────────────────────────────────────────────┘
```

### 6.2 Panel: config editor

- **YAML view**: CodeMirror 6, YAML language pack, schema lint, soft-wrapped, gutter line numbers, search (Cmd-F).
- **Knob mirror** (right-rail): a scrollable form pre-derived from the YAML. Each knob is one field, label includes the YAML path (e.g., `failsafe[0].retry.maxAttempts`). Editing a knob mutates the YAML in place (with cursor jumping to the changed line + brief highlight). Editing the YAML manually re-derives the knobs on every change (debounced 200 ms).
- **Paste / drop**: drop a `.yaml` / `.ts` file anywhere on the editor → file is read → loaded via `/api/config`.
- **Apply button** + `Cmd-Enter`. Until applied, the editor shows a "modified" badge.
- **Errors**: load-time validation errors render as inline diagnostics. The translator's deprecation warnings render as info-level annotations (yellow underlines, not red).

### 6.3 YAML ↔ knob sync model

Both views render off the SAME in-memory parsed AST (yaml.Node tree, line-numbers preserved). Knob edits walk the AST to the target node, mutate the value, re-serialize the YAML preserving comments + indentation. YAML edits re-parse → re-build the knob form. This avoids the "two sources of truth" trap.

For TS / JS configs, the editor falls back to text-only mode (no knob mirror) — TS configs go through `loadConfigFromTypescript` at apply time but the round-trip back to TS-source isn't supported (would require an AST mutator for TS). The operator can paste a TS config, see it run, but knobs require switching to YAML mode.

### 6.4 Panel: traffic flow

Animated SVG/canvas stage per §4.4. Real-time, driven by the `trace` frames over WebSocket. Hovering a lane shows a tooltip with current per-upstream stats. Clicking a particle pauses it mid-flight and shows the request's full lifecycle in a side drawer (selection-step trail, attempt history, hedge log, response time per attempt).

### 6.5 Panel: charts

- **Selection share over 60 s**: stacked-area, one band per upstream, y = selections/s. Animated, 100 ms-bucketed.
- **Per-upstream cards**: req, err, p50, p90, score, position. Updated on every stats frame. Color-coded backgrounds: green if position 0 / 1, yellow if 2+, red if -1 (excluded).
- **Failsafe activity strip**: small horizontal bars showing retry-rate, hedge-rate, timeout-rate, circuit-breaker-state per upstream. Helps the operator see which upstreams are absorbing recovery overhead.

### 6.6 Panel: upstream synth knobs

One row per upstream. Each row shows:
- ID, vendor, tags
- `available` toggle + `failure-injection` quick-button (60 s outage)
- Compact knobs: `baseLatency`, `jitter`, `errorRate`, `timeoutRate`, `throttleRate`, `blockLag`, `methodGlobs`
- Live indicator: current selection position + a tiny sparkline of error-rate over the last minute

Bulk actions: "make all healthy" / "make all degraded" / "outage all alchemy" (uses vendor tag) — useful for incident reproduction.

### 6.7 Panel: traffic generator

- RPS slider 1..10000 (log scale).
- Method mix: editable rows.
- Shape: dropdown + parameters.
- Scenario player: dropdown of built-in scenarios + play/stop + a progress bar.

### 6.8 Panel: event log

Last 50 sampled traces, newest first. Each row:

```
14:32:18.452   eth_blockNumber   alc-1   ✓ 42ms        [ord: alc-1, drpc-1, ...]
14:32:18.451   eth_call          drpc-1  ↻ 2 attempts  hedge won, 80ms (alc-1 timed out)
14:32:18.450   eth_traceTx       —       ✗ no upstream eligible
```

Clicking a row opens a drawer with the full lifecycle (every selection step, every attempt, every hedge, every retry, every error). This is the simulator's primary investigation tool — Datadog APM, locally and for free.

---

## 7. API surface

### 7.1 REST

| Method | Path | Body / Response |
|---|---|---|
| `GET`  | `/` | embedded `index.html` + assets |
| `GET`  | `/api/state` | `{ config, configValid, upstreamSynth, traffic, scenarios, stats }` |
| `POST` | `/api/config` | `{ yaml: string }` → 200 with new state; 400 with `{ errors: [{line,col,msg}] }` |
| `POST` | `/api/upstreams/{id}` | `{ synth: UpstreamSynthSpec }` |
| `POST` | `/api/traffic` | partial `TrafficSpec` (any subset of fields) |
| `POST` | `/api/scenario` | `{ name }` to start, `{}` to stop |
| `POST` | `/api/reset` | empty |
| `GET`  | `/metrics` | Prometheus exposition |
| `ANY`  | `/sim/{id}` | synthetic JSON-RPC endpoint |

### 7.2 WebSocket — `GET /api/events`

Binary frames. Each frame is a length-prefixed msgpack message with a type discriminator:

```
type StatsFrame {
  ts: int64                  // unix ms
  network: string
  rps: float32               // last 100ms
  upstreams: []{
    id: string
    requests: uint32
    successes: uint32
    errorsByKind: map[string]uint32   // transport / 5xx / timeout / throttled / misbehavior
    hedges: uint32
    retries: uint32
    p50Ms: float32
    p90Ms: float32
    p99Ms: float32
    currentScore: float32 nullable
    position: int8                     // 0..N or -1
    cordonedReason: string nullable
    cbState: string                    // closed / open / half-open
  }
  policyEvalDurationMs: float32
  policyEvalErrors: uint32             // last 100ms
}

type TraceFrame {
  tickId: string
  ts: int64
  method: string
  finality: string
  selection: { order: []string, excluded: []string, evalDurationUs: int32 }
  attempts: []{
    upstreamId: string
    startedAtMs: int32      // offset from ts
    finishedAtMs: int32
    outcome: string         // ok | err | timeout | throttled
    latencyMs: float32
    errKind: string nullable
    isHedge: bool
    isRetry: bool
  }
  cacheHit: bool
  totalDurationMs: float32
}

type ConfigSwapFrame { ts, summary, deprecationWarnings: []string }
type EvalErrorFrame  { ts, tickId, kind, message }
type ScenarioFrame   { ts, name, phase, progress: float32 }
```

Subscriber bandwidth at 10k rps with stats every 100 ms (10/s × ~2KB) + 1% trace sampling (100/s × ~500B) ≈ 70 KB/s. Easy budget.

---

## 8. Observability

The simulator emits the production Prometheus families verbatim:

- `policy_selection_*` (position, rejection, primary-switch, eval duration, eval errors, eligible_upstreams)
- `erpc_upstream_request_duration_seconds`
- `erpc_upstream_request_errors_total`
- `erpc_failsafe_*` (retries, hedges, timeouts, circuit-breaker transitions)
- `erpc_cache_*`

Operators can point a local Prom server at `http://localhost:8080/metrics` for Grafana-side investigation.

---

## 9. Validation & error handling

| Failure mode | Visible response |
|---|---|
| Pasted YAML fails strict-decode | inline diagnostics in editor; current config keeps running |
| Pasted YAML has legacy translator warnings | yellow-banner above editor; config still applies |
| `evalFunc` fails sobek compile | inline diagnostic at failing line in the YAML; current policy keeps running |
| `evalFunc` throws at runtime | red event in log + `policy_selection_eval_errors_total{kind=throw}++` |
| `evalFunc` times out | red event + `kind=timeout` |
| Traffic generator's method isn't supported by any upstream | event log "no upstream eligible"; bumps a counter |
| `/sim/<id>` panic | log + 500 returned to eRPC; failsafe handles it; chart shows red |
| WS client lags | drop oldest events; client reconciles on next stats frame |
| Config swap fails partway | rollback to previous `*erpc.ERPC`; banner explains |

---

## 10. Scenario library schema

Scenarios live as YAML under `internal/simulator/scenarios/`. Schema:

```yaml
name: gradual-degradation
description: alc-1 errorRate ramps to 50% over 30s; reverts at t=120s
duration: 180s
steps:
  - at: 0s
    set:
      upstreams.alc-1.errorRate: 0.0
      traffic.rps: 500
  - at: 60s
    interp: linear        # linear interpolation to next set
    set:
      upstreams.alc-1.errorRate: 0.5
    until: 90s
  - at: 120s
    set:
      upstreams.alc-1.errorRate: 0.0
```

Operators can write their own scenarios and drop them into a config-pointed dir. The bundled set covers the common debugging needs (see §3.4).

---

## 11. Future scope (Phase 6+)

| Item | Sketch |
|---|---|
| Replay an export | Save a 60 s window's full event stream + the input config; replay against a CHANGED config to A/B compare |
| Multi-config bench | Run N candidate configs in parallel on the same traffic; show the policy chosen and the SLA each delivered |
| Scenario authoring UI | Visual scenario editor (timeline of knob changes) |
| Headless `--bench-mode` | No GUI; output a single JSON report with per-upstream stats over a fixed scenario; usable in CI |
| Chaos export to k8s | Generate kubectl manifests that replay a scenario against a real eRPC deployment (chaos engineering) |

---

## 12. Out of scope (won't build)

- Production telemetry export (the simulator is local; for prod metrics use the prod stack).
- Multi-user sharing of running sessions.
- Auth, TLS, anything internet-facing — `127.0.0.1` only.
- Editing the simulator's own source code from inside the GUI.
- A drag-and-drop visual policy builder. The policy is JS code; the editor is text.
- Saving past simulation runs to disk (memory-only state).
