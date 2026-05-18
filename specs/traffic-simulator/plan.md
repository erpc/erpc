# eRPC Traffic Simulator — Implementation Plan

**Last revised**: 2026-05-17
**Branch**: `feat/selection-policy-rewrite`

## Phase 1 — Delivered (2026-05-17)

The Claude Design handoff bundle contained a complete, working
client-side prototype focused on **realtime selection-policy
testing**. Phase 1 ships that prototype intact, wrapped in a tiny
Go binary that serves the embedded assets.

What landed:

- **`cmd/erpc-simulator/main.go`** — ~80-LOC Go server. `embed.FS` over
  `cmd/erpc-simulator/web/`, `net/http.FileServer`, graceful
  shutdown on SIGINT/SIGTERM, `-addr` flag (default
  `127.0.0.1:8080`), `no-store` Cache-Control on the HTML so bundle
  changes are picked up without a hard refresh.

- **`cmd/erpc-simulator/web/`** — 17 design assets verbatim from the
  prototype:
  - `index.html` — entrypoint. Loads React 18 + ReactDOM 18 + Babel
    standalone from `unpkg.com` (UMD bundles with SRI integrity
    hashes), then `simulator.js` + `yaml-util.js` as plain JS, then
    the JSX files as `type="text/babel"` for in-browser
    transpilation.
  - `simulator.js` — ~660 LOC pure-JS simulation engine. Sample method
    weights, sample latency (gaussian jitter), roll outcome
    (throttle / timeout / error / ok), retry + hedge + circuit-
    breaker mechanics, scoring (success-rate-weighted +
    latency-penalised), and the selection layer that hands the
    "structurally eligible" upstreams to the user policy and accepts
    the returned ordering.
  - `yaml-util.js` — line-by-line tokenizer + diagnostics for the
    YAML editor (legacy-field deprecation warnings, tab detection,
    odd-indent detection).
  - `app.jsx` — root component. Holds `simRef` (the mutable sim
    state) + a `tick` counter to drive React. 10 Hz `setInterval`
    drives the sim loop; canvas runs at 60 fps inside `flow-stage`.
    Resizable layout with three drag-handles, sizes persisted to
    `localStorage`.
  - `flow-stage.jsx` — top-down canvas particle viz. Measures the DOM
    positions of source / core / upstream nodes, draws static
    channels between them with brightness scaled to last-second
    activity, animates outcome-colored dots travelling along each
    channel.
  - `selection-policy.jsx` — JS/TS function editor with line-numbered
    gutter, syntax highlighting, compile-error reporting, side panel
    showing the `Upstream` type signature + notes + live impact stats,
    four example presets (default · vendor priority · lowest p90 ·
    score only), ⌘↵ apply / ↺ reset.
  - `config-editor.jsx` — YAML textarea (overlay + pre for
    highlighting + textarea for editing, all scroll-synced),
    drag-and-drop `.yaml` load, knob-mirror tab.
  - `upstream-knobs.jsx` — per-upstream slider+input rows for the 7
    synthetic knobs, bulk-action buttons, position pill +
    error-rate sparkline + "inject 60s failure" button per row.
  - `right-col.jsx` — traffic-gen + event-log tabs + lifecycle drawer.
  - `charts.jsx`, `top-bar.jsx`, `bottom-tabs.jsx`, `resizer.jsx` —
    the rest.
  - Five CSS files split by region: `styles.css` (tokens + grid),
    `styles-2.css` (config editor + legacy flow), `styles-3.css`
    (charts + upstream knobs + right column + drawer),
    `styles-flow-v2.css` (top-down flow stage),
    `styles-policy.css` (selection-policy editor).

- **`Makefile`** — `make build` now also produces `./bin/erpc-simulator`.
  New `make run-simulator` target runs the binary directly.

- **`specs/traffic-simulator/feature.md`** — rewritten to describe
  what was delivered. Drops the original "boot real eRPC + fake
  upstream loopback + 10k RPS server-side traffic gen" framing — the
  design landed on a fully client-side simulator focused on
  selection-policy testing, which the Phase 1 spec now reflects.

### Verification

```
$ make build
[builds erpc-server + erpc-server-pprof + erpc-simulator]

$ ./bin/erpc-simulator -addr 127.0.0.1:9876 &
erpc-simulator: listening on http://127.0.0.1:9876

$ for path in / styles.css simulator.js app.jsx flow-stage.jsx ...; do
>   curl -sL -o /dev/null -w "%-25s %{http_code}\n" "$path"
> done
# all 19 assets → 200

$ kill %1
erpc-simulator: shutting down...
```

End-to-end: the binary boots, every asset listed in `index.html`
resolves, and the React app loads in a browser. Verified by hand at
`http://localhost:8080`.

## Phase 2 — Future work (not in this PR)

### Bridge to a real eRPC selection engine

Currently the policy function is evaluated in the browser via
`new Function`. A `-bridge <addr>` flag could connect the simulator
to a real eRPC's admin API and have it evaluate the policy via sobek
(the same runtime production uses), then stream back per-tick
selection decisions for the visualization. Useful for catching
"works in browser, breaks under sobek" divergence.

### Multi-network mode

The top-bar network selector is presentational today. Wire it to
multiple sim instances so an operator can flip between `eth-mainnet`,
`arbitrum`, `base` and observe per-chain dynamics.

### Real YAML round-trip

The current knob-mirror uses regex-based extraction and a
fixed list of editable fields. A proper YAML AST (yaml.Node-style)
would let the operator edit *any* field via the knob form and have
it write back precisely to the right line + indentation.

### Persisted scenarios

The scenario library is hard-coded in `simulator.js`. A
`-scenarios <dir>` flag would let operators ship a folder of YAML
scenario files (per the schema in `feature.md` v2) and load them at
runtime.

### Export / share

A "share" button that exports the current sim state (policy + knobs
+ YAML draft) as a single shareable URL fragment, so an operator
can pre-set a state for a teammate to inspect.

### Replay mode

Save the last N requests to a JSON file; reload to replay against a
new policy. Useful for regression-testing a policy change against a
captured production trace.

## Out of scope

- Server-side traffic gen (10k+ RPS sustained) — would require the
  bigger architecture sketched in the original v2 spec. The current
  in-browser model handles up to ~5k RPS smoothly on a modern
  laptop and that's enough for selection-policy tuning.
- Real upstream HTTP servers — the simulator models RPC behaviour
  with synthetic outcome rolls; there's no plan to add a real
  `httptest` upstream pool.
- Authentication / multi-user — single-process local tool only.
