# Unified Metrics Manager — Specification

**Status**: Draft — for review
**Last revised**: 2026-08-19

---

## 1. Purpose

eRPC's telemetry has grown organically into **three registration mechanisms** and several call-site styles (§2). This spec defines a **unified metrics manager** — a single owner in `telemetry/` for how every metric is defined, registered, and recorded — and ships its first policy: **exposure control**, operator config choosing which metric families appear on `/metrics`.

The manager is the feature; exposure control is the first thing it makes easy. Once all definitions and recordings flow through one point, policies that are currently bolted on per metric kind — label shaving (`counterDropLabels` / `histogramDropLabels`), idle eviction, exposure — become manager decisions applied uniformly, and disabled families can be made allocation-free on the hot path.

### Goals

- **One registration path.** No bare `promauto` in `metrics.go`; every family is defined through the manager and registered by the manager, once, after config is read.
- **One recording path.** Call sites record through manager-issued handles that look identical for every metric kind.
- **Centralized policies.** Exposure, label projection, and idle eviction are manager configuration, not per-call-site or per-kind machinery.
- **Allocation avoidance.** A disabled or label-shaved family costs (near-)zero on the hot path — the manager hands out no-op handles.

### First policy: exposure control

eRPC exposes ~122 `erpc_*` metric families on `/metrics` (port 4001). Today there is **no config to choose which families appear on scrape** — `promhttp.Handler()` serves the full default registry. Operators can only disable the metrics server entirely (`metrics.enabled: false`), drop **labels** (`histogramDropLabels`, `counterDropLabels`), or filter at scrape time via Prometheus `metric_relabel_configs`.

Exposure control adds first-class config to limit **which families are exposed**, primarily to reduce scrape response size and scrape-side CPU, secondarily to avoid in-process collection cost for metrics nobody will ever read.

What it solves that label-dropping cannot:

- `histogramDropLabels` / `counterDropLabels` reduce **cardinality within a family**; they cannot remove a family from the scrape.
- Prometheus-side relabeling lives outside eRPC config, drifts per deployment, and still pays full in-process gather cost.
- High-cardinality or simply uninteresting families (e.g. `network_evm_block_range_requested_total` with its unbounded `bucket` label, or the `selection_*` probe family for operators who don't run selection-policy dashboards) can be excluded at the source.

### Non-goals

- **Not** a per-metric collection disable at compile time — metrics are still defined in code; exposure is a runtime/config decision.
- **Not** a general glob/regex matcher — exact family names and trailing-prefix subsystem patterns (`consensus_*`) only. The prefix commits to eRPC's existing first-segment = subsystem naming convention, nothing more.
- **Not** a change to any metric name or label under default config — `/metrics` is byte-identical when unconfigured.
- **Not** a fix for the dead `metrics.hostV4`/`hostV6` bind fields (separate cleanup).

## 2. Current state — three registration paths

eRPC registers metrics through **three different mechanisms** in `telemetry/metrics.go`:

| Path | Count | When it registers | Call-site style |
|---|---|---|---|
| **Deferred counters** (`LabeledCounter`) | ~36 | `RebuildFilteredCounters()` during `erpc.Init` | `WithLabelValues(...)` / `CounterHandle` cache |
| **Deferred histograms** (`LabeledHistogram`) | ~17 | `SetHistogramBuckets()` during `erpc.Init` | `WithLabelValues(...)` / `ObserverHandle` cache |
| **`promauto` metrics** (gauges, counters, histograms) | ~86 | Package init, before config is read | Direct `*prometheus.*Vec` — `WithLabelValues(...).Set()/Inc()/Observe()` |

The deferred paths exist to support `counterDropLabels` / `histogramDropLabels`: those metrics are built unregistered at package init so config can be applied before they hit the registry. This is the half-done unification the maintainers pointed at — counters already have `LabeledCounter` + `CounterHandle` + a single rebuild function; gauges and the `promauto` counter/histogram long tail never got the same treatment.

Consequences of the split:

- **No single gate.** "Don't expose family X" today requires three different mechanisms (skip a register call, skip another register call, unregister after the fact) — and the third requires a name → collector mapping that doesn't exist.
- **Policy logic is per-kind.** Label projection is implemented twice (`HistogramLabelFilter`, `CounterLabelFilter`) and is unavailable for gauges.
- **No no-op path.** Every call site pays full Prometheus cost even for families an operator will never scrape.

### Why scrape-time filtering alone is insufficient

A `FilteredGatherer` wrapping `prometheus.DefaultGatherer` (filtering `[]*dto.MetricFamily` after `Gather()`) is cheap to build and does shrink the HTTP response, but it still:

- Runs `Collect()` on every registered collector on every scrape
- Allocates the full `MetricFamily` set before dropping

For deployments where scrape CPU and response size both matter, filtering after gather is the wrong layer. The manager gates at **registration**: an unexposed family is never registered, so gather never sees it.

## 3. The manager

A single `Manager` in `telemetry/` owns the metric lifecycle:

```
  definition (package init)          erpc.Init (config available)         request hot path
  ─────────────────────────          ─────────────────────────────        ─────────────────
  manager.DefineCounter(...)  ──┐
  manager.DefineGauge(...)      │    manager.Configure(policies)
  manager.DefineHistogram(...)  │          │
                                │          ▼
                                │    for each definition:
                                │      exposed?  ── no ──► never registered;
                                │      │                    handles are no-op
                                │      yes
                                │      ▼
                                │    apply label projection
                                │    register on DefaultRegisterer
                                │          │
                                ▼          ▼
                          call sites record through
                          manager-issued handles
```

### Definition

All ~139 metric definitions route through the manager's factory (`DefineCounter` / `DefineGauge` / `DefineHistogram`). Definitions happen at package init as today, but **nothing registers at definition time** — the factory builds the vec, indexes it by resolved family name, and holds it unregistered. Package-level vars (`MetricUpstreamRequestTotal`, …) keep their existing wrapper types in Phase 1 so call sites don't change.

This deletes the three-path split: `promauto` disappears from `metrics.go`, and `RebuildFilteredCounters()` / `SetHistogramBuckets()` collapse into the manager's single `RegisterAll`.

### Registration

`manager.Configure(...)` is called once from `erpc.Init`, after config is read, alongside the existing label-filter installation. It applies, per family:

1. **Exposure** (§4) — unexposed families are never registered.
2. **Label projection** — today's `HistogramLabelFilter` / `CounterLabelFilter`, generalized to all kinds.
3. Registration on `prometheus.DefaultRegisterer`.

Because nothing registers before Init, there is **no unregister path** — gating is purely "never register." (The Prometheus label-set-hash-freeze constraint documented on `RebuildFilteredCounters` is sidestepped, not worked around.)

### Recording

Call sites record through manager-issued handles. Phase 1 keeps today's call sites byte-identical (package vars keep their types); the handle unification (Phase 2) gives every kind the same shape and lets the manager return a **no-op handle** for disabled families — the mechanism that removes hot-path cost (§5).

### Policies the manager owns

| Policy | Today | Under the manager |
|---|---|---|
| Exposure (`exposeMetrics` / `dropMetrics`) | doesn't exist | registration gate (§4) |
| Label projection (`histogramDropLabels`, `counterDropLabels`) | two per-kind filters | one projection applied to all kinds |
| Idle eviction (`counterIdleEvictionAfter`) | counter-only sweep | unchanged mechanically, owned by the manager |
| No-op / allocation avoidance | impossible | no-op handles for disabled families |

## 4. Policy: exposure control

Two optional list fields on the root `metrics` block, following the existing naming convention (eRPC metrics omit the `erpc_` prefix, same as `histogramLabelOverrides` / `counterLabelOverrides`):

```yaml
metrics:
  enabled: true
  port: 4001

  # Allowlist — when non-empty, ONLY these families are exposed
  exposeMetrics:
    - upstream_*                       # whole subsystem (trailing prefix)
    - network_request_duration_seconds # single family

  # Denylist — remove families from exposure (applied after exposeMetrics)
  dropMetrics:
    - consensus_*                      # whole subsystem
    - network_evm_block_range_requested_total
```

Entries are either **exact family names** or **trailing-prefix patterns** ending in `*`. A prefix matches every family whose resolved name starts with the stem — this is how operators think (`upstream_*`, `consensus_*`, `cache_*`) and matches the first-segment = subsystem convention the metrics catalog already follows. No general globs: `*` is only legal as the final character.

### Name resolution

| Config entry | Resolved match |
|---|---|
| `upstream_request_total` | family `erpc_upstream_request_total` |
| `erpc_upstream_request_total` | same (prefix accepted as-is) |
| `consensus_*` | every family whose name starts `erpc_consensus_` |
| `erpc_consensus_*` | same (prefix accepted as-is) |
| `go_goroutines` | `go_goroutines` (stock collector — full name) |
| `process_*` | all `process_*` stock collectors |
| `promhttp_metric_handler_requests_total` | full name (no auto-prefix) |

### Semantics

1. Both unset/empty → expose everything (today's behavior, backward compatible).
2. `exposeMetrics` only → allowlist: only listed families are registered/exposed.
3. `dropMetrics` only → denylist: listed families are not registered.
4. Both set → allowlist first, then denylist (denylist wins on overlap).
5. Unexposed families are **never registered** — they cost nothing at scrape time; with Phase 2 no-op handles they cost (near-)nothing on the hot path.

### Granularity

Entries match whole metric **families** (all series sharing a name) or whole **subsystems** via trailing prefix. Per-series control (dropping individual label-value combinations) is intentionally not offered: series are dynamic and caller-controlled, and registration gating acts on whole collectors. Use `counterDropLabels` / `histogramDropLabels` for series shaping within exposed families, or Prometheus `metric_relabel_configs` for label-value filtering at scrape.

Prefix semantics on future metrics: a `dropMetrics` prefix auto-drops families added to that subsystem later (fail-closed — what the operator meant); an `exposeMetrics` prefix auto-exposes them (the operator opted into the subsystem).

### Validation at config load

- Reject empty strings in either list.
- Reject `*` anywhere except as the final character (trailing-prefix only).
- Reject duplicates within each list (after normalization).
- Warn (log once, non-blocking) for any entry — exact name or prefix — that matches zero known families at Init. The manager's definition index makes this check exact (catches `consenus_*` typos) without enforcing a closed catalog (the metric set is open-ended across eRPC versions).

### Stock collectors

`go_*` / `process_*` / `promhttp_*` live on the default registry outside the manager. A thin `FilteredGatherer` around `prometheus.DefaultGatherer` at the HTTP handler applies the same exposure filter to them — this is a safety net for stock collectors only, not the CPU-saving mechanism for eRPC families. (`ERPC_NOMETRICS=1` already replaces the whole registry for the all-off case.)

## 5. What the manager buys beyond exposure

Registration gating removes **scrape-side** cost (`Collect()` + encoding) for unexposed families. With unified handles it also removes **hot-path** cost:

- **No-op handles.** Recording on a disabled family short-circuits before touching Prometheus internals — no map lookup, no label projection, no allocation. Call sites don't change; the handle the manager issued is already a no-op.
- **One label-projection implementation.** Label shaving stops being counter/histogram-only and gains gauge coverage; projection happens once at handle-issue time, not per observation.
- **Bounded allocations.** Precomputed handles and pooled label sets where profiling justifies — a manager concern, invisible to call sites.

These are Phase 2–3 deliverables; Phase 1 ships exposure control with call sites unchanged.

## 6. Interaction with existing knobs

| Knob | Relationship |
|---|---|
| `metrics.enabled` | Orthogonal — controls whether the HTTP server starts at all. |
| `ERPC_NOMETRICS=1` | Orthogonal — replaces the whole registry with a no-op; exposure config is meaningless under it. |
| `histogramDropLabels` / `counterDropLabels` | Become manager policies — same config fields, one implementation. |
| `counterIdleEvictionAfter` | Unchanged semantics; eviction sweep becomes manager-owned. |
| `memory.emitMetrics` | Unchanged — still gates the Ristretto collection goroutine; the gauge family itself can also be dropped via `dropMetrics`. |

## 7. Observability of the manager itself

- One INFO log at startup when exposure filtering is active: resolved allowlist/denylist sizes, e.g. `metrics exposure: allowlist=12 denylist=3`.
- One WARN per configured name that doesn't resolve to a known family, at Init (the definition index makes this exact).
- No new metric for "metrics dropped" — the absence of the family on `/metrics` is the signal; adding a metric about metrics would be self-defeating for a feature whose purpose is fewer metrics.

## 8. Backward compatibility

- Default (both fields unset) is byte-identical to today: all families registered and exposed, same names, same labels.
- No schema breakage — new optional fields only.
- Phase 1 changes no call sites; package-level metric vars keep their types.
- **Test-visible change:** `promauto` families no longer appear on `/metrics` in processes that never run manager registration (e.g. tests that scrape without `Init`). This extends the existing deferred-counter semantics ("counters stay unregistered until an Init with metrics config runs") to all families.

## 9. Open questions

1. **Should `exposeMetrics` implicitly include stock collectors?** An operator writing a 5-family allowlist probably doesn't mean to drop `go_*`/`process_*` — but silently keeping them violates allowlist semantics. *Leaning: explicit — document that stock collectors need full names in the allowlist if wanted.*
2. **Handle API shape.** Reuse the existing `CounterHandle` / `ObserverHandle` generics as the unified handle, or a new per-kind interface the manager issues? *Leaning: generalize the existing handle caches — they're already the hot-path optimization.*
3. **Definition syntax.** Keep package-level `var MetricX = manager.DefineCounter(...)` (minimal diff, vars keep types) vs a declarative table. *Leaning: package-level vars — call sites and docs references stay stable.*
4. **Per-family no-op threshold.** No-op handles for unexposed families only, or also for exposed-but-label-shaved projections? *Leaning: unexposed only; shaved projections still record.*

## Appendix — related documents

- `plan.md` — phased implementation plan.
- Jira: [TECHOPS-28974](https://circlepay.atlassian.net/browse/TECHOPS-28974) (under BRP H2 2026 epic [TECHOPS-27051](https://circlepay.atlassian.net/browse/TECHOPS-27051)).
- Metrics catalog: `docs/pages/reference/metrics.mdx` / <https://docs.erpc.cloud/reference/metrics>.
