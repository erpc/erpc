# Claude Design Prompt — eRPC Traffic Simulator

Paste the section below into Claude Design as a single prompt.

---

## Design brief: eRPC Traffic Simulator

### What it is

**eRPC** is an open-source RPC routing engine that sits in front of dozens of blockchain RPC providers (Alchemy, QuickNode, dRPC, Conduit, self-hosted nodes, etc.) and decides — per request — which upstream to send it to, based on a programmable selection policy, configurable failsafe stack (retry, hedge, timeout, circuit-breaker), and live health metrics.

The **Traffic Simulator** is a local browser-based playground for **tuning and stress-testing an eRPC configuration** before deploying it to production. The operator (a backend infra engineer at an indexing company, MEV team, or RPC provider) drops in their `erpc.yaml`, sets a synthetic traffic shape, dials in upstream characteristics (latency, error rate, lag, throttling), and watches eRPC route real requests against a fake upstream pool — using the real production code paths for selection, failsafe, scoring, caching.

The "wow" moment: paste your prod config → drag one upstream's `errorRate` slider from 0 to 0.7 → within one second see hedges fire on the visualization, sticky-primary release, the policy switch to a different upstream, per-upstream latency histograms separate. Operator-driven incident rehearsal, locally.

### Who is the user

A senior backend engineer or RPC infra specialist. Comfortable with YAML, JavaScript, JSON-RPC, Prometheus, terminal tools. Not a designer; not a casual user. They live in Datadog / Grafana / VS Code / their terminal. The aesthetic should fit alongside those — sophisticated developer tooling, not consumer-grade marketing UI.

Reference points for tone + density:

- **Vercel dashboard** — clean, dense, neutral palette, opinionated typography.
- **Linear** — minimal chrome, snappy interactions, no decoration that doesn't serve a function.
- **Honeycomb BubbleUp** / **Datadog APM flame graphs** — comfortable with information density, hover tooltips, drill-in drawers.
- **PartyKit demos** / **Tinybird "Visualizer"** — for the real-time animated flow part.

### What the user does

1. Pastes (or drops a file of) their eRPC YAML config — could be 50 lines or 8000.
2. Sees their entire config schema light up as live knobs alongside the raw YAML editor.
3. Sets a synthetic traffic shape (network, method mix, rate, distribution).
4. Watches a left-to-right animated stage showing where every request lands. Color-coded particles encode outcome (success / hedge-win / retry-rescued / failure / cache-hit / never-selected).
5. Tweaks per-upstream synthetic characteristics — latency, jitter, error rate, throttling, block lag, availability.
6. Optionally plays a scripted "scenario" (e.g., `gradual-degradation`, `vendor-region-outage`, `traffic-spike`) and watches their failsafe + selection policy respond.
7. Iterates on the YAML — usually the `selectionPolicy.evalFunc` (JavaScript) or `failsafe` settings — and observes how routing behavior changes.
8. Once happy, copies the YAML back into prod.

### The layout you're designing

A **single-page web app**, served from a local Go binary on `http://localhost:8080`. Zero build, vanilla HTML + ES modules. The app lives in one tab; the operator never navigates.

Six functional regions. Suggest a layout — likely a two-row × three-column grid with a top bar — but be opinionated. The information density is high; the layout is the design.

**Top bar** (full width, slim):
- Status pill (running / paused / configuration error).
- Live rps counter (updates 10 Hz).
- "Policy valid ✓" / "Policy compile error" indicator.
- Network selector (dropdown — only visible when config defines multiple networks).
- Reset / pause / play controls.

**Region 1 — Config editor (large, scrollable)**:
- Left side: full YAML editor (CodeMirror 6, YAML highlighting, gutter line numbers, inline diagnostics for errors + deprecation warnings).
- Right side: knob mirror — a scrollable form that mirrors every editable field in the YAML. Editing a knob mutates the YAML; editing the YAML re-derives the knob form. Knob labels include their YAML path (e.g., `failsafe[0].retry.maxAttempts`).
- Drop zone behavior: drag a `.yaml` file anywhere onto the editor → it loads. Or paste with `Cmd-V` into the editor.
- "Apply" button + `Cmd-Enter` shortcut to commit changes.
- Show a "modified" badge until applied.
- When a legacy config is pasted, surface a small banner: "Legacy fields detected: scoreMetricsWindowSize, routingStrategy — migrated automatically. [show details]". Don't be loud about it.

**Region 2 — Traffic flow stage (the centerpiece, ~40% of viewport width)**:
- A left-to-right animated stage with THREE vertical lanes:
  - **Left lane**: "Client / traffic source" — one or two icons representing the traffic generator.
  - **Middle lane**: "eRPC core" — labeled boxes showing selection-policy + failsafe + cache. When a request flows through, briefly highlight the box that "handled" it (e.g., light up "cache" if it's a hit, light up "hedge" if hedge fired).
  - **Right lane**: "Upstream pool" — one horizontal row per upstream, with the upstream id + vendor + tags. The bar at the right of each row is a tiny live activity meter (last-second selections).
- **Particles** flow left-to-right along the lanes. Each particle represents a sampled request. Color encodes outcome:
  - **Green** — success on first attempt
  - **Yellow** — succeeded after retry
  - **Orange** — won a hedge race
  - **Red** — terminal failure
  - **Blue** — cache hit (never touched an upstream — the particle stops at the middle lane and bounces back)
  - **Gray-dim** — excluded by selection policy (a small "X" particle near the upstream that didn't get picked, to communicate exclusion visually)
- When a **hedge** fires, you see TWO particles fan out from the eRPC middle lane to two different upstreams; whichever finishes first keeps flowing, the loser dims out.
- When a **retry** fires, you see a particle bounce back from one upstream and reroute to another.
- When the **circuit-breaker** opens on an upstream, that upstream's row in the right lane gets a red border and incoming particles refuse to enter (visually deflect).
- Particle throughput is throttled to ~50 particles/s on screen regardless of actual rps (could be 10k underneath) — this is a representation, not a 1:1 view. Aggregate stats drive the chart; the animation drives intuition.
- Hover an upstream row → small tooltip with current stats. Click a particle → side drawer with the full lifecycle of that one request (selection trail, every attempt, every retry, every hedge with timing).

**Region 3 — Live charts + per-upstream cards**:
- A primary stacked-area chart showing **selection share** over the last 60 seconds (one band per upstream, y-axis = selections/sec, animated 100 ms-bucketed).
- Below it, a grid of **per-upstream stat cards** — one small card per upstream showing: id, vendor, tags, requests/sec, error rate, p50/p90 latency, current score (if scored), current position (0 = primary, 1..N runner-ups, -1 = excluded). Card background tints: green if position 0/1, yellow if 2+, red if excluded.
- Below the cards, a small **"failsafe activity strip"** showing horizontal bars per upstream: retry rate, hedge rate, timeout rate, current circuit-breaker state (closed / open / half-open). Helps the operator see which upstreams are absorbing failsafe overhead.

**Region 4 — Upstream synth knobs (one row per upstream)**:
- A compact horizontal-row layout. Each row shows the upstream id, vendor, tags on the left, then inline knobs: `baseLatency` slider, `jitter` slider, `errorRate` slider, `timeoutRate`, `throttleRate`, `blockLag`, an `available` toggle, and an "inject 60s failure" quick-button.
- Live indicator on the right of each row: current position (a tiny pill: "0", "1", "-1") + a tiny sparkline of error rate over the last minute.
- Bulk-action buttons at the top of this panel: "outage all alchemy" (selects by vendor tag), "make all healthy", "make all degraded", etc.

**Region 5 — Traffic generator panel (small)**:
- RPS slider, 1 → 10000, logarithmic. The slider has labeled stops at 1 / 10 / 100 / 1k / 10k. Above the slider, the live counter (also in the top bar).
- Method-mix table: rows of `[method-name input] [weight number] [delete row]`. Add row.
- Shape selector: dropdown — `poisson` / `constant` / `bursty` — with parameters appearing inline when relevant.
- Scenario player: a dropdown of named scenarios + play/pause + a progress bar when running. The scenario name list includes things like `gradual-degradation`, `vendor-region-outage`, `slow-network`, `traffic-spike`, `hedge-saturator`, `circuit-breaker-trip`.

**Region 6 — Event log (last 50 sampled traces, table)**:
- Newest at top. One row per request:
  - `timestamp | method | winning-upstream | outcome | duration | [eval ordering preview]`
  - Example: `14:32:18.452  eth_blockNumber   alc-1   ✓ 42ms      [alc-1, drpc-1, chainstack-1]`
  - Or: `14:32:18.451  eth_call          drpc-1  ↻×2 80ms     hedge won (alc-1 timed out)`
- Outcome icons: ✓ success, ↻ retried, ⚡ hedged, ✗ failed terminally.
- Click a row → drawer opens with the FULL lifecycle: selection trail (all considered upstreams with their reason if excluded), every attempt with its timing breakdown, every retry, every hedge, the final response time. This is the simulator's primary investigation tool.

### Visual aesthetic

- **Density**: high. This is a tool; chrome is overhead. Match Linear or the Honeycomb-style dense panels.
- **Palette**: neutral. Charcoal / dark gray backgrounds, near-white text. Accent colors are reserved for status (green/yellow/orange/red traffic-light semantics in the flow stage and stat-card tints). Avoid brand-y colors — no rainbow gradients, no marketing flourishes.
- **Typography**: a system sans for UI chrome (Inter / SF / system-ui), a mono for the editor + event-log timestamps + ids (JetBrains Mono / IBM Plex Mono / system-ui-monospace).
- **Motion**: minimal. The flow-stage animation is the one place motion is the point. Everything else (charts, knob updates, event-log new rows) should fade or smoothly transition (200–300 ms) without bouncing or pulsing. No animated icons in chrome.
- **Dark mode default**, but respect `prefers-color-scheme`. Both modes need to look like a serious tool — the dark mode is closer to Linear; the light mode is closer to GitHub.
- **Responsive**: the layout assumes a developer's laptop — 1280×800 minimum, 1920×1080 typical, ultrawide common. Below ~1100 px wide, collapse the knob mirror and event log into tabs.

### Specific things to nail

1. **The traffic-flow stage** is the visual centerpiece. Every other panel orbits it. It needs to be readable at a glance ("oh, all traffic is going to alc-1 right now") AND inspectable on click ("why did THIS particular request retry?"). Get the lane arrangement, particle motion, hedge-fan-out visual, and circuit-breaker visual ALL right.

2. **The YAML editor + knob mirror sync.** This is unusual UI. Editing one side updates the other instantly. The interaction model has to feel obvious: the knob is a shortcut to the YAML line. Some users will only edit YAML; some will only touch knobs; many will mix. Both modes are first-class.

3. **The status of being "modified but not applied"**. Both the YAML and knob edits can pile up unapplied. Show this clearly — a single "Apply" button per panel, with the count of pending changes ("3 pending changes — Apply"). Until applied, the running simulator still uses the previous config.

4. **The event log drawer**. When the user clicks a row, the drawer should render the request's full lifecycle in a visually digestible way — a vertical timeline with attempts as nodes, their latency as horizontal bars, hedge winners highlighted, retry attempts indented. Treat this like a per-request flame graph.

5. **The scenario player**. When a scenario is playing, the entire UI should make that visible — a progress bar at the top, the upstream knobs flashing/animating as the scenario writes to them, and the chart marking timeline events ("t=60s: alc-1 errorRate → 0.5"). The operator should always know what's happening to their inputs.

### What NOT to design

- No login flow. No settings page. No nav bar with multiple sections. No marketing landing page. The simulator is a single-page tool that opens directly into the dashboard.
- No bottom drawer or modal-heavy interactions. Drawers slide from the right.
- No third-party integrations panel. The simulator is self-contained.
- No mobile layout. This is a developer-laptop tool.
- No dashboard-style "tiles" with logos. No friendly empty states with illustrations — empty states are terse text.
- No social / "share this" buttons. No comments. No collaboration.

### Deliverable

A high-fidelity mockup or interactive prototype showing the full single-page layout with all six regions. Both dark and light variants. Pay attention to:

- The traffic-flow stage in motion (or a clear static depiction of how it works).
- The YAML-editor / knob-mirror dual view.
- The event-log row → drawer interaction.
- The per-upstream cards arrangement.
- The scenario player when active vs idle.

Hover states, tooltips, and the per-request drawer would all be valuable. If you produce an interactive prototype, prioritize the flow-stage and the YAML/knob sync over the other parts.
