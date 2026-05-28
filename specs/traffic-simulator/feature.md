# eRPC Traffic Simulator — Feature Spec

**Status**: Implemented (Phase 1)
**Last revised**: 2026-05-17

A local browser-based playground for **designing and testing eRPC
selection policies in real time**. Ships as a single Go binary
(`erpc-simulator`) that serves an embedded React/HTML/CSS app from a
local HTTP listener. Zero build step, zero external services.

> **Where this came from.** The simulator's UI was designed in Claude
> Design (see `claude-design-prompt.md` for the original brief; the
> chat transcripts and prototype source live in the design-handoff
> bundle the user pasted). The implementation in `cmd/erpc-simulator/`
> wraps that prototype in an `embed.FS` Go server so it can be built
> and shipped alongside the rest of the eRPC tree.

## What it does

The operator opens `http://localhost:8080` and lands in a single-page
dashboard:

```
┌──────────────────────────────────────────────────────────────────────────┐
│  top bar:  status · rps · success% · sim time · network · pause/reset    │
├───────────────────────────────────────────┬──────────────────────────────┤
│                                           │  TELEMETRY                   │
│        TRAFFIC FLOW                       │  selection share / sec       │
│  Client ──→ eRPC core ──→ upstream nodes  │  (stacked area, last 60s)    │
│  (canvas particle viz, top-down)          │                              │
│                                           ├──────────────────────────────┤
│                                           │  TRAFFIC GEN | EVENT LOG     │
├───────────────────────────────────────────┤  rps slider · method mix     │
│  SELECTION POLICY | UPSTREAM SYNTH | YAML │  shape · scenario player     │
│  ┌─────────────────────────────────────┐  │     ─ OR ─                   │
│  │ (upstreams) => upstreams            │  │  one row per request:        │
│  │   .filter(u => ...)                 │  │  ts · method · winner · ✓✗⚡↻ │
│  │   .sort((a,b) => ...)               │  │  (click → drawer with full   │
│  └─────────────────────────────────────┘  │   lifecycle: selection trail,│
│  signature · examples · live impact       │   attempts, retries, hedges) │
└───────────────────────────────────────────┴──────────────────────────────┘
```

The four resizable panels (drag the gutters; sizes persist to
`localStorage`) cover the entire feedback loop:

- **Traffic flow** — canvas-rendered top-down lane diagram. Client at
  the top, eRPC core in the middle, the 8 synthetic upstream nodes at
  the bottom. Real-request dots travel along the lines (one dot per
  sampled request). Color encodes outcome — green/yellow/orange/red/
  blue/gray-dim — so a glance tells the operator what their config is
  doing.
- **Selection policy** *(default tab)* — A JS/TS function editor
  (`(upstreams) => Upstream[]`). Compiles in-browser on Apply; on
  compile error falls back to score-desc default so traffic keeps
  flowing. Side panel shows the `Upstream` type signature and four
  example presets (default · vendor priority · lowest p90 · score only).
- **Upstream synth** — A row per synthetic upstream with sliders +
  editable number inputs for base latency, jitter, error rate, timeout
  rate, throttle rate, block lag, an available toggle, and an
  "inject 60s failure" quick-button. Bulk actions: all healthy / all
  degraded / outage all alchemy / restore alchemy / +6 blocks lag.
- **Config YAML** — A monospace YAML editor with line numbers,
  syntax-highlighted via a hand-rolled tokenizer, drop-zone for
  loading a `.yaml` file, diagnostics gutter, and a knob-mirror tab
  that surfaces the structural fields as a scrollable form (each row
  shows its YAML path, e.g. `failsafe.retry.maxAttempts`).
- **Telemetry** — A stacked-area chart of selections per upstream per
  second over the last 60 seconds, color-coded with the share palette.
- **Traffic generator** — RPS log slider (1 → 10 000), shape selector
  (poisson · constant · bursty), editable method mix, scenario player
  with built-ins:
  - `gradual-degradation` — one upstream's error rate rises in steps.
  - `vendor-region-outage` — Alchemy us-east latency × 4 → errors →
    unavailable → restored.
  - `traffic-spike` — RPS multiplies through 5× then 10× then back.
  - `hedge-saturator` — all upstreams jitter × 3, two go very slow.
  - `circuit-breaker-trip` — one upstream errors at 95%, another
    timeouts at 40%, a third throttles at 60%.
  - `slow-network` — all upstreams latency × 3, jitter × 2.
- **Event log + lifecycle drawer** — Last ~200 sampled requests in a
  scrolling list, filterable by all / err / hedge / retry / cache.
  Click a row → right-slide drawer with the full request lifecycle: the
  selection trail (ordering returned by the policy, including
  excluded entries with their reason), every attempt with its latency
  bar, hedge winners highlighted, hedge losers dimmed.

## Who it's for

A senior backend / RPC infra engineer who needs to tune a selection
policy or a failsafe block and wants to see the routing behaviour
respond to live signals before deploying to production. The audience
is comfortable with YAML, JavaScript, JSON-RPC, Prometheus, terminal
tools. The visual language matches that audience — Linear / Vercel /
Honeycomb density, no marketing flourish, dark mode by default.

## How it works

### Architecture

```
        ┌──────────────────────────────────────────────────────────┐
        │  erpc-simulator (Go binary)                              │
        │                                                          │
        │   embed.FS  ──►  net/http file server  ──►  127.0.0.1:8080
        │     │                                                    │
        │     └─ index.html, *.css, *.js, *.jsx                    │
        └──────────────────────────────────────────────────────────┘
                                       ↓ HTTP
        ┌──────────────────────────────────────────────────────────┐
        │  Browser tab                                             │
        │                                                          │
        │  React (UMD) + Babel-in-browser (UMD) from unpkg CDN     │
        │     │                                                    │
        │     ├─ simulator.js  ─ pure-JS simulation engine         │
        │     │   - sample method weights                          │
        │     │   - sample latency  (gaussian jitter)              │
        │     │   - roll outcome (throttle/timeout/error/ok)       │
        │     │   - retry + hedge + circuit-breaker mechanics      │
        │     │   - scoring (success-rate-weighted, latency-       │
        │     │     penalised)                                     │
        │     │   - selection: user policy fn → ordered upstreams  │
        │     │                                                    │
        │     ├─ yaml-util.js   ─ tokenizer + diagnostics          │
        │     │                                                    │
        │     └─ React components (flow stage, charts, knobs,      │
        │        policy editor, event log, drawer, top bar)        │
        └──────────────────────────────────────────────────────────┘
```

The Go side is intentionally trivial:

- `cmd/erpc-simulator/main.go` — ~80 LOC. Binds the listener, serves
  the `embed.FS` rooted at `cmd/erpc-simulator/web/`, handles
  graceful shutdown on SIGINT/SIGTERM.
- `cmd/erpc-simulator/web/` — all design assets verbatim (HTML, CSS,
  React/Babel scripts loaded from a CDN; the actual app code is
  vanilla JS modules + JSX files transpiled in-browser).

Everything reactive — the simulation tick, the selection-policy
evaluation, the particle animation, the chart history, the
event-lifecycle drawer — lives in the browser.

### Why client-side

Three pragmatic reasons:

1. **Zero install.** No npm, no webpack, no node. The Go binary owns
   the entire user experience. `make build && ./bin/erpc-simulator`
   and the operator is in business.
2. **Real selection-policy testing.** The user's policy function is
   real JavaScript — same language eRPC itself runs via sobek server-
   side. So the in-browser simulator is testing the *exact* function
   shape the operator will eventually paste into their `erpc.yaml`.
3. **Fast feedback loop.** Drag a slider, see the chart shift inside
   the same animation frame. There's no network round-trip for any
   interaction.

### Selection-policy contract

The simulator exposes the same `Upstream` shape the operator would
write against in production:

```ts
type Upstream = {
  id: string;
  vendor: string;
  tags: string[];
  chainId: number;
  available: boolean;
  blockLag: number;
  metrics: {
    errorRate: number;      // 0..1
    successRate: number;    // 0..1
    p50: number;            // ms
    p90: number;            // ms
    requestsPerSec: number;
    score: number;          // 0..1, computed by eRPC
    cbState: "closed" | "half" | "open";
  };
};

type SelectionPolicy = (upstreams: Upstream[]) => Upstream[];
```

Only structurally-eligible upstreams (available, CB closed/half, block
lag within tolerance for the request's finality bucket) are passed
in. Returning a non-array or throwing falls back to the default
score-desc ordering. The function re-runs on every request — keep it
cheap, no async, no fetch.

### Synthetic upstreams

The seed set is eight upstreams that mirror a realistic eth-mainnet
deployment:

| id | vendor | tags | base | jitter | error |
|---|---|---|---|---|---|
| alc-eth-1 | alchemy | eth, us-east | 38 | 18 | 0.005 |
| alc-eth-2 | alchemy | eth, us-west | 52 | 22 | 0.008 |
| drpc-eth-1 | drpc | eth | 68 | 35 | 0.012 |
| quik-eth-1 | quicknode | eth, premium | 41 | 14 | 0.004 |
| chainstack-eth-1 | chainstack | eth | 74 | 40 | 0.018 |
| conduit-eth-1 | conduit | eth, dedicated | 56 | 24 | 0.010 |
| self-eth-1 | self-hosted | eth, lhr | 28 | 12 | 0.022 |
| public-eth-1 | public | eth, fallback | 180 | 110 | 0.080 |

These are the defaults baked into `simulator.js`. The operator
mutates them live via the **Upstream synth** tab.

## Non-goals (v1)

- **Not boot a real eRPC instance.** The simulator is a faithful
  model of the routing dynamics, not a hosted eRPC. Policy code is
  evaluated in the browser via `new Function`, not in sobek. (Future
  work: a `-bridge` flag that pipes the policy + sliders to a real
  eRPC for parity testing.)
- **Not a YAML round-tripper.** The YAML editor renders text; the
  knob-mirror's regex-based parser handles a handful of canonical
  fields. Editing the structural shape of an `erpc.yaml` (adding
  networks, reordering upstreams) is best done in the textarea
  directly.
- **Not a multi-network simulator.** The top-bar network selector is
  presentational; the simulator runs one chain at a time.
- **Not a benchmark.** Particle throughput is throttled to ~50/s
  visible regardless of actual RPS. The chart and stats reflect the
  real underlying request count.

## Files at a glance

```
cmd/erpc-simulator/
├── main.go                 # ~80-line embed.FS HTTP server
└── web/
    ├── index.html          # Entrypoint; loads React/Babel + the JSX bundle
    ├── styles.css          # Tokens + top-bar + main grid + buttons
    ├── styles-2.css        # Config editor + flow stage v1 (legacy)
    ├── styles-3.css        # Charts, upstream knobs, right column, drawer
    ├── styles-flow-v2.css  # Top-down flow stage layout (current)
    ├── styles-policy.css   # Selection-policy editor
    ├── simulator.js        # ~660 LOC pure-JS sim engine (no React)
    ├── yaml-util.js        # YAML tokenizer + diagnostics
    ├── app.jsx             # Root component, state, sim-loop interval
    ├── top-bar.jsx
    ├── flow-stage.jsx      # Canvas particle viz
    ├── charts.jsx          # SVG stacked-area share chart
    ├── upstream-knobs.jsx
    ├── selection-policy.jsx  # JS/TS function editor
    ├── config-editor.jsx   # YAML + knob mirror
    ├── bottom-tabs.jsx     # Policy / Upstream / Config tab switcher
    ├── right-col.jsx       # Traffic-gen + event-log tabs + drawer
    └── resizer.jsx         # Drag-handle for the four panels
```

## Running it

```bash
make build               # produces ./bin/erpc-simulator (~6 MB, no CGo)
./bin/erpc-simulator     # listens on 127.0.0.1:8080 by default
# or
make run-simulator       # go run ./cmd/erpc-simulator
```

Flags:

- `-addr <host:port>` — bind address (default `127.0.0.1:8080`).
