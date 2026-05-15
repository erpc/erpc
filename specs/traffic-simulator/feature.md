# Traffic Simulator — Specification

**Status**: Ready for implementation
**Owner**: TBD
**Last revised**: 2026-05-15

---

## 1. Purpose

The **Traffic Simulator** is a single-binary local tool for evaluating eRPC selection policies under live, deterministic traffic. The operator boots the binary, opens the browser, and gets:

- a list of synthetic upstreams whose performance (latency, jitter, error rate, lag, method support, availability) is live-tunable from the UI;
- a code editor holding the network's `selectionPolicy.evalFunc`, with the full stdlib pre-loaded and hot-reload on every keystroke ("Apply");
- a traffic-generator panel (rate, method mix);
- a real-time streaming chart showing which upstream wins each request, the runner-up ranking, and per-upstream stats;
- a last-N event log linking every request to the eval output that picked it.

The "wow" moment: change one knob — make an upstream slow, or rewrite the policy mid-flight — and see the chart react within one eval-tick (≤ 1 s by default).

The simulator **imports and runs the real eRPC code paths** for selection: same `internal/policy.Engine`, same sobek runtime, same `internal/policy/stdlib` install, same `health.Tracker`, same metric tracker. Only the upstream HTTP transport is synthesized — and even that goes through real `upstream.Upstream` objects pointed at a loopback HTTP server inside the same binary. There is no policy-engine mock and no metrics-tracker mock; what you see in the simulator is what eRPC will do in prod against an upstream with those exact observed characteristics.

### 1.1 Non-goals

- **Not a load tester.** The simulator's traffic generator is for behavior-shaping, not throughput benchmarking. Target ≤ 1000 rps.
- **Not a multi-region replica.** Single-process, single-network in MVP. Multi-network + full-config import is Phase 5.
- **Not a recorder.** No persistent capture of simulation runs in MVP (Phase 5 export-to-test).
- **Not a production dashboard.** Operators run the simulator against synthetic traffic to validate a policy before shipping; once shipped, the prod observability stack (traces + `policy_selection_*` metrics) takes over.

---

## 2. Architecture

One Go binary. One port. All assets embedded. All real eRPC libs.

```
┌──────────────────────────────────────────────────────────────┐
│  Browser GUI (single page, zero build)                       │
│                                                              │
│  ┌──── Upstream knobs ────┐   ┌──── Policy editor ────────┐ │
│  │ • id / vendor / tags   │   │ CodeMirror 6, JS mode.    │ │
│  │ • baseLatencyMs        │   │ Default policy preloaded. │ │
│  │ • jitterMs             │   │ "Apply" → POST /policy →  │ │
│  │ • errorRate            │   │ sobek compile → swap.     │ │
│  │ • blockLag             │   └───────────────────────────┘ │
│  │ • methodGlobs          │                                 │
│  │ • available toggle     │   ┌──── Traffic gen ──────────┐ │
│  │ • inject-failure (X)   │   │ • rps slider              │ │
│  └────────────────────────┘   │ • method mix (weighted)   │ │
│                               │ • start / stop / pause    │ │
│  ┌──── Live timeline ─────┐   └───────────────────────────┘ │
│  │ Stacked-area, color =  │                                 │
│  │ upstream, y = selects/s│   ┌──── Per-upstream cards ───┐ │
│  │ x = last 60s.          │   │ req routed, err rate,     │ │
│  └────────────────────────┘   │ p50/p90 latency, score,   │ │
│                               │ position (0=primary).     │ │
│  ┌──── Event log ─────────┐   └───────────────────────────┘ │
│  │ last 50 events:        │                                 │
│  │ ts | method | winner | │                                 │
│  │ top-5 ranking | dur ms │                                 │
│  └────────────────────────┘                                 │
└──────────────────────────────────────────────────────────────┘
     │  HTTP REST (config)     │  WebSocket (events)
     ▼                         ▼
┌──────────────────────────────────────────────────────────────┐
│  cmd/erpc-simulator                                          │
│                                                              │
│  HTTP routes (single port — default 8080)                    │
│   GET  /                  → embedded GUI (HTML + JS + CSS)   │
│   GET  /api/state         → current upstreams + policy +     │
│                              traffic config (JSON snapshot)  │
│   POST /api/upstreams     → upsert one upstream spec         │
│   POST /api/policy        → compile + swap evalFunc          │
│   POST /api/traffic       → start/stop/configure generator   │
│   POST /api/reset         → clear stats + restart loop       │
│   GET  /api/events        → WebSocket fan-out                │
│   GET  /metrics           → Prometheus /metrics (optional)   │
│   ANY  /sim/{upstreamID}  → fake JSON-RPC endpoint           │
│                                                              │
│  Simulation engine                                           │
│   ┌────────────────────────────────────────────────────┐    │
│   │ TrafficGen ─► Poisson loop @ rps                   │    │
│   │     │                                              │    │
│   │     ▼ pick method (weighted)                       │    │
│   │     ▼                                              │    │
│   │ Engine.Evaluate(network, method, upstreams)        │    │
│   │     │  ← REAL internal/policy.Engine               │    │
│   │     ▼ winner = upstreams[0], also expose top-5     │    │
│   │     ▼                                              │    │
│   │ winner.Forward(ctx, req)                           │    │
│   │     │  ← REAL upstream.Upstream                    │    │
│   │     ▼ HTTP to /sim/<id> on the same binary         │    │
│   │     ▼ fake server sleeps base±jitter, maybe errors │    │
│   │     ▼                                              │    │
│   │ health.Tracker observes latency + outcome          │    │
│   │     │  ← REAL metrics path                         │    │
│   │     ▼                                              │    │
│   │ EventBus.Emit({ts, method, winner, top5,           │    │
│   │                 latencyMs, err, tickId})           │    │
│   └────────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────────┘
```

### 2.1 The fake-server trick

Each `SimUpstream` is a **real** `upstream.Upstream` whose endpoint is `http://127.0.0.1:<port>/sim/<id>` (loopback to ourselves). The `/sim/<id>` handler reads the current spec for `<id>` from the simulator's state, sleeps `baseLatencyMs ± jitterMs`, optionally returns a synthetic error (rate-controlled), and responds with a minimal JSON-RPC payload. Every other line of code — vendor detection, `health.Tracker` observation, sobek JS eval, score updater, sticky-primary state — is unchanged production code.

This is the central design choice: the simulator does NOT mock the policy engine. It feeds the production engine a fake transport and watches it work.

### 2.2 Where it lives

- `cmd/erpc-simulator/main.go` — flag parsing, embedded-assets http.ListenAndServe, lifecycle.
- `internal/simulator/`
  - `server.go` — HTTP + WS routes.
  - `engine.go` — simulation loop + traffic generator.
  - `upstreams.go` — `SimUpstream` builder, fake-server handler.
  - `policy.go` — wraps `internal/policy.Engine`, owns compile/swap.
  - `events.go` — event types + WS broadcaster (fan-out, drop on slow client).
  - `config.go` — `SimulatorConfig` (UpstreamSpec, TrafficSpec, PolicySpec).
  - `assets/` — embedded GUI (index.html, app.js, style.css, vendored deps).

Co-located with the eRPC library so it imports `common`, `internal/policy`, `internal/policy/stdlib`, `upstream`, `health` directly.

---

## 3. Configuration

### 3.1 Boot config (optional CLI)

```bash
# minimal
erpc-simulator

# explicit
erpc-simulator \
  --port 8080 \
  --config simulator.yaml \
  --erpc-config /path/to/erpc.yaml      # Phase 5
```

| Flag | Default | Description |
|---|---|---|
| `--port` | `8080` | HTTP listen port (single port for GUI + API + WS + fake-server). |
| `--config` | `""` | Pre-seed simulator state from a YAML file (otherwise starts empty + the operator builds state in the GUI). |
| `--erpc-config` | `""` | **Phase 5 only**: import a full `erpc.yaml`/`erpc.ts`; upstream endpoints rewritten to `/sim/<gen-id>` and the full eRPC stack (failsafe / hedging / retry) runs end-to-end. |

### 3.2 Simulator config (the YAML the simulator owns)

```yaml
# simulator.yaml
network:
  id: evm:1                                # arbitrary string; the policy sees it as ctx.network
  evalInterval: 1s
  evalTimeout: 100ms
  evalFunc: |
    (upstreams, ctx) =>
      upstreams
        .sortByScore(BALANCED)
        .stickyPrimary({ hysteresis: 0.10, minSwitchInterval: '30s' })

upstreams:
  - id: alc-1
    vendor: alchemy                        # cosmetic; surfaced as u.vendor in the JS eval
    tags: [tier:main, region:us-east]
    baseLatencyMs: 40
    jitterMs: 20
    errorRate: 0.0                         # 0..1
    blockLag: 0                            # blockHeadLag observed by the eval
    available: true                        # if false, /sim/<id> returns immediate transport error
    methodGlobs: ["*"]                     # what methods to "support"; unsupported returns -32601

  - id: drpc-1
    vendor: drpc
    tags: [tier:fallback]
    baseLatencyMs: 80
    jitterMs: 30
    errorRate: 0.05
    blockLag: 0
    available: true
    methodGlobs: ["eth_*", "!eth_traceTransaction"]

traffic:
  rps: 50
  methods:
    - { method: eth_blockNumber, weight: 4 }
    - { method: eth_getBlockByNumber, weight: 2 }
    - { method: eth_call, weight: 3 }
    - { method: eth_traceTransaction, weight: 1 }
  paused: false
```

| Section | Field | Notes |
|---|---|---|
| `network` | `id` | Arbitrary string. Acts as `ctx.network` in the JS eval. |
|  | `evalInterval`, `evalTimeout` | Mirror the corresponding `SelectionPolicyConfig` fields (1 s / 100 ms defaults). |
|  | `evalFunc` | JavaScript source. Same shape and stdlib surface as the production `selectionPolicy.evalFunc`. |
| `upstreams[]` | `id` | Unique. Used in routing + chart legends. |
|  | `vendor` | Cosmetic label, surfaced as `u.vendor`. |
|  | `tags` | `prefix:value` slice consumed by stdlib (`byTag`, `preferTag`, `spreadAcrossTags`). |
|  | `baseLatencyMs` / `jitterMs` | Synthetic response time. Actual sleep ≈ `baseLatencyMs ± jitterMs` uniform. |
|  | `errorRate` | `0..1`. Probability of returning a fake `-32099` upstream error. |
|  | `blockLag` | Reported as `metrics.blockHeadLag` (the fake server populates a per-method block-tag if asked). |
|  | `available` | `false` → `/sim/<id>` returns transport-level error (connection refused / 503). |
|  | `methodGlobs` | Glob patterns (same as `upstream.ignoreMethods` syntax with `!` negation). Unsupported method → `-32601 method not supported`. |
| `traffic` | `rps` | Target request rate. Poisson-ish (Bernoulli per millisecond). |
|  | `methods[]` | Weighted distribution. |
|  | `paused` | If `true`, generator is loaded but idle. |

### 3.3 Defaults when the file is absent

Starting the simulator without `--config` boots with:

- 4 upstreams pre-populated (`fast-a`, `fast-b`, `slow-a`, `fallback-a` — last tagged `tier:fallback`).
- The embedded default policy as `evalFunc`.
- Traffic generator paused, rps `50`, method mix `eth_blockNumber 4 / eth_getBlockByNumber 2 / eth_call 3`.

The operator can then tweak everything from the GUI.

---

## 4. Runtime model

### 4.1 Engine

The simulator instantiates a single `*policy.Engine` (same one production uses). On boot:

1. Build `SimUpstream`s, each backed by a real `*upstream.Upstream` whose `endpoint` is `http://127.0.0.1:<port>/sim/<id>`.
2. Wire a `health.Tracker` (per-project) and pass it to the engine.
3. Register a single network on the engine: `engine.RegisterNetwork(network.id, upstreamsFn, &SelectionPolicyConfig{EvalFunc: cfg.network.evalFunc, ...})`.
4. The engine's internal slot ticks at `evalInterval` and the policy hot-reload swaps the program in place via `Engine.UpdateNetworkPolicy(id, newCfg)` (added if it doesn't exist; otherwise rebuilds the slot — see plan §1.3).

### 4.2 Traffic generator

A goroutine runs at `rps`. Each tick:

1. Sample a method by weighted random.
2. Build a minimal `NormalizedRequest`.
3. Resolve the order: `ordered := engine.GetOrdered(network.id, method)`.
4. Pick `winner := ordered[0]`.
5. Call `winner.Forward(ctx, req)` — this hits `/sim/<winner.id>` via real HTTP.
6. Emit a `SelectionEvent`:
   ```go
   type SelectionEvent struct {
       Ts        time.Time   `json:"ts"`
       Method    string      `json:"method"`
       Winner    string      `json:"winner"`
       Top5      []string    `json:"top5"`
       LatencyMs float64     `json:"latencyMs"`
       Err       string      `json:"err,omitempty"`
       TickId    string      `json:"tickId"`  // (network/method/tickUnixMillis)
   }
   ```

If the policy returns `[]` (no eligible upstream), the simulator emits a `no-upstream` event with no `Forward` call and surfaces it in the UI's event log + a count badge.

### 4.3 Fake-server (`/sim/<id>`)

```
POST /sim/<id>
Content-Type: application/json
Body: {"jsonrpc":"2.0","id":<N>,"method":"<M>","params":[...]}
```

Handler logic:

1. Look up `<id>` in current state. If not found → 404.
2. If `spec.available == false` → close TCP connection without response (forces transport-level error in eRPC).
3. If `spec.methodGlobs` doesn't match `M` → `{"jsonrpc":"2.0","id":N,"error":{"code":-32601,"message":"method not supported"}}` with `200 OK`.
4. Compute sleep: `dur := baseLatencyMs + uniform(-jitterMs, +jitterMs)`; clamp to ≥ 0; sleep.
5. With probability `spec.errorRate`: return `{"error":{"code":-32099,"message":"synthetic error"}}`.
6. Otherwise return a minimal canned response per method (see §4.5).

The fake-server is a regular `http.Handler` mounted on the same `http.ServeMux` as the GUI/API. Because everything is loopback, latency overhead beyond the simulated sleep is < 1 ms.

### 4.4 Synthesized responses

Just enough JSON-RPC to keep `health.Tracker` + integrity layers happy. Each method has a canned shape:

| Method | Response result |
|---|---|
| `eth_chainId` | `"0x1"` (or whatever the network.id resolves to) |
| `eth_blockNumber` | `"0x" + hex(currentHeight)` — auto-incremented every 2 s by a goroutine, biased by upstream-specific `blockLag` |
| `eth_getBlockByNumber` | A stub block object with the requested number (or `currentHeight` for `"latest"`) |
| `eth_getBalance` | `"0x0"` |
| `eth_call` | `"0x"` |
| `eth_gasPrice` | `"0x3b9aca00"` |
| `eth_estimateGas` | `"0x5208"` |
| (anything else) | `null` with `200 OK` (lets the eval still observe a latency) |

`blockLag` is implemented by returning `currentHeight - blockLag` instead of `currentHeight` for that upstream. The state-poller in eRPC will compare to other upstreams and the tracker will populate `metrics.blockHeadLag` accordingly.

### 4.5 Hot-reload

Operator edits `evalFunc` in the editor → clicks "Apply" → `POST /api/policy { "evalFunc": "..." }`:

1. Server compiles via `common.CompileProgram(src)`. On error, returns 400 with the sobek error message; the GUI underlines the failing line.
2. On success, the server constructs a fresh `*SelectionPolicyConfig` and re-registers the network: `engine.UnregisterNetwork(id)` + `engine.RegisterNetwork(id, upstreamsFn, newCfg)`. The slot's ticker stops, fresh state is rebuilt; cross-tick state (previousOrder, lastSwitchAt) is intentionally cleared so the operator sees a clean transition.
3. Next tick uses the new policy. Visible in the GUI within `evalInterval` (≤ 1 s).

### 4.6 Upstream knob updates

`POST /api/upstreams { "id": "alc-1", "spec": { "baseLatencyMs": 200, ... } }`:

1. Validate the spec (positive numbers, methodGlobs non-empty, etc.).
2. Atomically swap into the simulator's in-memory state.
3. The `/sim/<id>` handler reads the new spec on the next request — no eRPC re-registration needed.
4. The change shows up in the chart within `evalInterval + (1/rps)` seconds.

If the operator toggles `available: false`, the next request to `/sim/<id>` errors → `health.Tracker` records the failure → the eval next tick may exclude that upstream via `removeCordoned` / `keepHealthy`.

### 4.7 Reset

`POST /api/reset`: stops traffic, drops cross-tick state, drops `health.Tracker` history, restarts the policy slot. The chart clears, the per-upstream cards reset to zero. Used when the operator wants a clean baseline after a destructive experiment.

---

## 5. Browser GUI

### 5.1 Single page, zero build

- One `index.html` with ES modules.
- Vendored deps via `embed.FS`: Chart.js (UMD), CodeMirror 6 (single-bundle build), reset.css.
- No bundler, no node, no transpile step. Total page weight target < 300 KB.
- The page uses standard HTML form elements (number inputs, sliders, checkboxes) — no UI framework.

### 5.2 Layout

Three columns. Top-row spans full width.

```
┌──────────────────────────────────────────────────────────────────┐
│  Top bar: ● running 50 rps  |  policy ✔ valid  |  reset  | pause │
├──────────────┬───────────────────────┬───────────────────────────┤
│              │                       │                           │
│  Upstreams   │   Live chart          │   Policy editor           │
│  (8 cards)   │   (stacked-area,      │   (CodeMirror 6, JS)      │
│              │    60 s window)       │                           │
│              ├───────────────────────┤                           │
│              │   Per-upstream stats  │   Traffic generator       │
│              │   (small bar chart    │   rps slider              │
│              │    per upstream)      │   method weights table    │
│              │                       │   start/stop              │
├──────────────┴───────────────────────┴───────────────────────────┤
│  Event log: last 50 events. ts | method | winner | top5 | dur ms │
└──────────────────────────────────────────────────────────────────┘
```

### 5.3 Upstream card

```
┌─────────────────────────────┐
│ alc-1   [available ✓]  [✕]  │   ✕ = inject failure (toggle .available)
│ alchemy   tier:main         │
│                             │
│ latency [40 ms ± 20]  ───●  │   slider 0–2000, click value for input
│ errorRate [0.00]      ●──   │   slider 0–1
│ blockLag  [0]                │   int input
│ methods   [*]                │   click → modal with method-glob editor
└─────────────────────────────┘
```

Every edit POSTs to `/api/upstreams` with optimistic UI (the slider responds instantly; if the server rejects, the slider snaps back).

### 5.4 Live chart

- **Type**: stacked-area, one series per upstream + one "errors" series (red, on top).
- **X axis**: rolling 60 s, advancing every 500 ms.
- **Y axis**: `selections per second`.
- **Smoothing**: 1 s buckets.
- **Animations**: incremental — no full re-draw (use Chart.js streaming).
- **Tooltip**: hover a point → "at 14:32:18, 30 selections: 24 alc-1, 6 drpc-1".

The simulator emits a periodic stats snapshot (every 500 ms) over the WebSocket so the chart drives off that, not raw per-request events.

### 5.5 Policy editor

- CodeMirror 6 with the JavaScript language pack.
- Pre-loaded with the embedded `internal/policy/default_policy.js` content on first boot.
- "Apply" button (or `Cmd-Enter` / `Ctrl-Enter`):
  - if compile fails, an inline error appears at the failing line with the sobek message
  - if compile succeeds, the button briefly flashes green; the next event in the timeline is labeled "(policy reloaded)"
- A read-only sidebar lists the stdlib primitives (one line each: `byTag(pat)`, `preferTag(pat, opts)`, `sortByScore(weights)`, etc.) so the operator doesn't need to leave the page to discover the API.

### 5.6 Traffic generator panel

- `rps` slider (1–1000) with log-ish scale (1, 5, 10, 50, 100, 500, 1000).
- Method-mix table: `[method] [weight]` rows; add/remove rows; weights normalized client-side. Method strings free-form (any JSON-RPC method).
- "Start" / "Pause" / "Stop": start/pause is a single toggle on the generator; stop clears the queue.

### 5.7 Per-upstream stats

For each upstream, the right-rail shows a compact card with:

- requests routed (counter)
- success / error split
- p50, p90 latency
- current `score` (last eval output's score for this upstream; `—` if `sortByScore` not in the chain)
- current `position` in the cached order (`0` = primary, `-1` = excluded)

These come from the periodic stats snapshot.

### 5.8 Event log

Last 50 events, newest at top. Each row:

```
14:32:18.452  eth_blockNumber  →  alc-1   [alc-1 → drpc-1 → fast-b → slow-a]   42 ms
```

The bracketed list is the top-5 of the eval's output for that tick (joins the request to the eval via `tick_id`). Errors render red. Hover a row → tooltip shows the full event payload (JSON).

---

## 6. API surface

### 6.1 REST

| Method | Path | Body / Response |
|---|---|---|
| `GET` | `/` | embedded `index.html` + assets |
| `GET` | `/api/state` | full snapshot: `{ network, upstreams[], traffic, policyValid: bool, stats }` |
| `POST` | `/api/upstreams` | `{ id, spec }` → 200/400; missing id → 404 |
| `POST` | `/api/upstreams/add` | `{ spec }` (no id → auto-generated) → 200 with full state |
| `POST` | `/api/upstreams/remove` | `{ id }` → 200 with full state |
| `POST` | `/api/policy` | `{ evalFunc }` → 200 on compile-OK, 400 with `{ error: "...", line: N, col: N }` on fail |
| `POST` | `/api/traffic` | `{ rps?, methods?, paused? }` (partial updates OK) → 200 |
| `POST` | `/api/reset` | empty → 200; clears stats, drops tracker state, restarts slot |
| `GET` | `/metrics` | Prometheus exposition (same `policy_selection_*` families as eRPC core) |
| `ANY` | `/sim/{id}` | the synthetic JSON-RPC fake-server (described in §4.3) |

### 6.2 WebSocket — `GET /api/events`

Single bidirectional stream. Server pushes:

```jsonc
// per request (one per traffic-gen call)
{ "type": "selection", "ts": "...", "method": "eth_blockNumber",
  "winner": "alc-1", "top5": ["alc-1","drpc-1","fast-b","slow-a"],
  "latencyMs": 42.3, "err": null, "tickId": "evm:1/eth_blockNumber/1715600000123" }

// every 500 ms — drives the chart
{ "type": "stats", "ts": "...",
  "perUpstream": [
    { "id":"alc-1", "requests":42, "errors":0, "p50":40, "p90":58,
      "score":0.34, "position":0 },
    ...
  ],
  "selectionsLastSecond": { "alc-1": 28, "drpc-1": 6, "_errors": 0 }
}

// on policy hot-reload
{ "type": "policy_reload", "ts": "...", "ok": true,
  "error": null /* or { line, col, message } if ok=false */ }

// on tick failure (eval timeout/throw/invalid_return)
{ "type": "eval_error", "ts": "...", "tickId": "...", "kind": "timeout|throw|invalid_return", "message": "..." }
```

Client doesn't send anything in MVP (config changes go through REST). Server drops events for slow consumers (bounded channel, fixed-size; oldest gets evicted).

---

## 7. Observability

The simulator emits the **same Prometheus families** as core eRPC, on `/metrics`:

- `policy_selection_position{upstream}`
- `policy_selection_rejection_total{upstream, step}`
- `policy_selection_primary_switch_total{from, to}`
- `policy_selection_eval_duration_seconds`
- `policy_selection_eval_errors_total{kind}`
- `policy_selection_eligible_upstreams`

This is a free side-benefit of using `internal/policy.Engine` directly — the metrics it emits are already labeled with `(project, network, method, upstream)`. The simulator uses `project=simulator` and the operator's chosen `network.id`.

Operators wanting to debug a policy in Grafana can point a local Prom server at `http://localhost:8080/metrics`.

---

## 8. Validation & error handling

### 8.1 Config

- `network.evalInterval` ≥ `evalTimeout`; both > 0.
- `network.evalFunc` compiles in sobek; if not, the server refuses to boot with that policy and falls back to the embedded default + a logged warning.
- Every `upstream.id` unique; non-empty.
- `errorRate ∈ [0, 1]`, `baseLatencyMs ≥ 0`, `jitterMs ≥ 0` and `≤ baseLatencyMs`, `blockLag ≥ 0`.
- `methodGlobs` non-empty (default `["*"]` if not provided).
- `traffic.rps ∈ [0, 1000]` (0 = paused).
- Each `methods[].weight ≥ 0`; total weight > 0 unless paused.

### 8.2 Policy compile errors

When `POST /api/policy` receives bad JS, the response is `400` with:

```jsonc
{ "error": "SyntaxError: Unexpected token", "line": 5, "col": 21 }
```

The GUI surfaces this inline. The previous (valid) policy continues to run; traffic is uninterrupted.

### 8.3 Runtime errors

| Source | Visibility |
|---|---|
| `evalFunc` throws | event log: red row with the thrown message; `policy_selection_eval_errors_total{kind=throw}` increments; traffic continues with the prior tick's cache |
| `evalFunc` times out | event log: red row "eval timed out after Xms"; `kind=timeout` |
| `evalFunc` returns non-array | event log: "invalid eval return"; `kind=invalid_return` |
| `/sim/<id>` 5xx | the upstream's request shows up red; `health.Tracker` records failure |
| No eligible upstream | event log: "no upstream eligible"; no request fired; stats badge increments |

---

## 9. Future scope (Phase 5+)

| Item | Sketch |
|---|---|
| **Full `erpc.yaml` import** | `--erpc-config /path/to/erpc.yaml`. Loader rewrites every upstream's `endpoint` to `http://127.0.0.1:<port>/sim/<gen-id>` and feeds the full Config through `common.LoadConfig` (so failsafe / hedging / retry / cache all run). The GUI gains a network selector. |
| **Multi-network** | One simulation tab per network; share the upstream pool but with per-network specs. |
| **Failure injection scripts** | Time-based scenarios (`at t=60s, crash alc-1 for 30s`). YAML-defined; replayable. |
| **Time-warp** | 10× speed for accelerated tests; ticker.NewTicker(intv / warp). Tricky for `health.Tracker`'s time-windowed metrics; deferred until needed. |
| **Capture → test** | Record a 60 s simulation window; emit a `network_test.go` scenario using the captured upstream specs + traffic mix. |
| **Slack export** | One-click "share a snapshot": serialize state + last-60s events to a gist, share URL. |
| **Auth + multi-user** | Token-gated `/api/*`; required if the simulator gets deployed beyond localhost. |

---

## 10. Out of scope (won't build)

- Reading or writing files outside the working directory.
- Connecting to a real upstream over the network — the simulator is hermetic by design.
- Persistent state between simulator runs (every boot starts from `--config` or the bundled defaults).
- Editing the simulator's own source code from inside the GUI.
- Running multiple simulator instances on the same port (single-tenant).
