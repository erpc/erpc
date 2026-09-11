# Unified Metrics Manager — Specification

**Status**: Draft — for review
**Last revised**: 2026-09-01

---

## 1. Purpose

eRPC's telemetry had grown into **three registration mechanisms** and several call-site styles (§2). The **unified metrics manager** is a single owner in `telemetry/` for how every metric is defined, registered, and (for labeled kinds) projected — with one operator surface, `metrics.customizations`, covering family exposure, label projection, and per-family histogram buckets.

### Goals

- **One registration path.** No `promauto` in `metrics.go`; every family is `Define*` at package init and registered by `telemetry.Configure` after config is read.
- **One policy engine.** Exposure, label projection, and bucket overrides compile into one `MetricPolicy` (`telemetry/policy.go`) with one specificity ordering.
- **One customization config.** `metrics.customizations` replaces parallel exposure/label knobs; the four legacy label fields remain as deprecated aliases.
- **Scrape-size reduction that actually saves cost.** A dropped eRPC family is **never registered**, so it costs no series and no collection — not merely filtered after gather.

### Non-goals

- **Not** compile-time collection disable — metrics stay defined in code; exposure is a startup/config decision (restart to change).
- **Not** a general glob/regex matcher — exact names and trailing-prefix patterns only (`*` only as final character).
- **Not** label projection on gauges — collapsing gauge series would report whichever writer wrote last, not a coarser aggregate.
- **Not** hot-path no-op handles for dropped families in Phase 1 (call sites still hold the wrapper; an unregistered Vec is simply invisible on scrape). Allocation avoidance is Phase 2+ — see `plan.md`.
- **Not** a fix for the dead `metrics.hostV4`/`hostV6` bind fields.

## 2. Problem / prior state

Three registration paths in `telemetry/metrics.go`:

| Path | Approx. count | When it registered | Could see label filters? |
|---|---|---|---|
| Deferred counters (`LabeledCounter`) | ~37 | `RebuildFilteredCounters` in `erpc.Init` | yes |
| Deferred histograms (`LabeledHistogram`) | ~17–19 | `SetHistogramBuckets` in `erpc.Init` | yes |
| `promauto` (gauges, counters, histograms) | ~86–87 | package init, before config | **no** |

Only the deferred paths could honour `counterDropLabels` / `histogramDropLabels`, because Prometheus freezes a family's label-set hash on first registration (`dimHashesByName` survives `Unregister`). Anything that changes a label set — or that wants a family off `/metrics` entirely — must run **before** the first registration. `promauto` registers too early.

Operators also had no family-level exposure knob: production scrapes ~160MB with all ~141 families always present. Bucket tuning was a single global `histogramBuckets` string for the `DefaultHistogramBuckets` set.

## 3. The manager (`telemetry/manager.go`)

```
  definition (package init)          erpc.Init                          request hot path
  ─────────────────────────          ────────                           ─────────────────
  DefineCounter(...)          ──┐
  DefineGauge(...)              │    Configure(Options)
  DefineLabeledCounter(...)     │      │
  DefineLabeledHistogram(...)   │      ▼
                                │    NewMetricPolicy(customizations, legacy)
                                │      │
                                │      ▼
                                │    for each definition:
                                │      Exposed? ── no ──► skip (never register)
                                │      │
                                │      yes → rebuildInPlace (labels/buckets)
                                │           → Register on DefaultRegisterer
                                │           → ResetHandleCache
                                ▼
                          call sites use package vars (unchanged types)
```

### Factories

| Factory | Kind | Label projection | Bucket override |
|---|---|---|---|
| `DefineCounter` | counter, fixed labels | no | n/a |
| `DefineLabeledCounter` | counter, caller-controlled labels | yes | n/a |
| `DefineGauge` | gauge | **never** | n/a |
| `DefineLabeledHistogram` | histogram | yes | yes |

There is no `DefineHistogram`. Plain histograms became `DefineLabeledHistogram` so every histogram honours label and bucket customizations. Leaving `opts.Buckets` empty takes boundaries from `metrics.histogramBuckets`; a non-empty code default is kept unless a customization overrides it. A bucketless plain histogram can no longer silently inherit Prometheus `DefBuckets`.

`rebuildInPlace` keeps package-level pointers stable across configure/rebuild so call sites do not reassign.

### `Configure` / errors

`erpc.Init` calls `telemetry.Configure(cfg.Metrics.TelemetryOptions())`.

| Failure | Sentinel / shape | Init log severity | Outcome |
|---|---|---|---|
| Malformed customizations / policy compile | wrapped in `ErrNothingRegistered` | **Error** — `"no metric families are registered"` | nothing registered |
| Unparseable `histogramBuckets` | plain error (defaults substituted) | **Warn** — `"falling back to default histogram buckets"` | all exposed families still register |

`MetricsConfig.Validate` also compiles the policy up front so the CLI rejects bad config before start. Init still handles both paths because callers can assemble `*common.Config` by hand.

`SetHistogramBuckets` remains as a **histogram-only** entry point for config validation and tests that need histograms scrapeable without freezing counter label sets. Production goes through `Configure`.

### Startup diagnostics

When `metrics.customizations` is non-empty:

- INFO `metric customizations applied` with `customizations` / `exposed` / `total` counts.
- WARN for subjects matching no known eRPC family (typos); stock-collector subjects are skipped in this check (they are not in the definition index).
- WARN for exact-subject rules a named family cannot honour (buckets on a non-histogram; labels on a family with no `rebuild` / fixed label set). Broad prefixes do not warn per swept-up family.

## 4. Policy: `metrics.customizations`

One ordered list on the root `metrics` block. Each entry has a `subject` and zero or more of: `action`, `labels`, `buckets`. An entry that sets none of those three is rejected (no-op).

```yaml
metrics:
  enabled: true
  port: 4001
  histogramBuckets: "0.05,0.5,5,30"   # global default for histograms with empty code buckets

  customizations:
    # Denylist a subsystem
    - subject: "consensus_*"
      action: drop

    # Allowlist + fleet-wide label trim (one "*" entry — duplicates rejected)
    - subject: "*"
      action: drop
      labels:
        - subject: "user"
          action: drop
        - subject: "agent_name"
          action: drop
    - subject: "upstream_*"
      action: keep
    - subject: "network_request_duration_seconds"
      action: keep
    - subject: "go_goroutines"
      action: keep

    # Per-metric label carve-out after the fleet-wide drop
    - subject: "upstream_request_total"
      labels:
        - subject: "user"
          action: keep

    # Per-family histogram buckets ([]float64, strictly increasing)
    - subject: "upstream_request_duration_seconds"
      buckets: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 3]
```

### Subject matching

| Config subject | Matches |
|---|---|
| `upstream_request_total` | `erpc_upstream_request_total` |
| `erpc_upstream_request_total` | same (`erpc_` accepted as-is) |
| `consensus_*` | every family whose name starts `erpc_consensus_` |
| `*` | every family (eRPC +, via gatherer filter, stock collectors) |
| `go_goroutines` / `process_*` / `promhttp_*` | stock collectors by full name (no `erpc_` auto-prefix) |

`NormalizeMetricName` prefixes non-stock names with `erpc_`. Duplicate subjects across customization entries are rejected (merge them).

### Actions

Family and label actions are only `keep` | `drop` (not `expose`).

- **`drop`** — family off `/metrics` (never registered for eRPC families; scrape-filtered for stock).
- **`keep`** — spare matched families from a broader `drop`. Omit `action` to touch only labels/buckets.
- Default with no exposure actions: every family exposed (byte-compatible with pre-feature behaviour).

Allowlisting is **explicit**: `subject: "*", action: drop` then `action: keep` carve-outs. There is no implicit "any keep flips allowlist mode" — that would make meaning depend on which other keys are present.

### Precedence (specificity, not list order)

Rules sort **broadest first**; lookups walk and take the last match:

1. Exact family name beats any prefix.
2. Longer prefix beats shorter prefix; `*` is weakest.
3. Equal specificity → later list entry wins.
4. Desugared **legacy** knobs sit **below** an equally specific customization so migrating a field takes effect.

Same rules apply inside a `labels` list (`agent_*: drop` then `agent_name: keep`).

### Label projection

Applied only to families with a `rebuild` path (`LabeledCounter`, `LabeledHistogram`). Call sites always pass the full canonical schema; the wrapper forwards retained positions only. Gauges and fixed-label `DefineCounter` families ignore label rules (exact-subject attempts WARN via `IgnoredCustomizations`).

Legacy kind masks preserve today's scope when desugaring: `counterDropLabels` → counters-only `*` rule; `histogramDropLabels` → histograms-only.

### Buckets

`customizations[].buckets` is `[]float64`, strictly increasing (validated; NaN rejected). An explicit override wins over both `metrics.histogramBuckets` and the definition's code buckets. Global `histogramBuckets` remains a comma-separated string for histograms that leave code buckets empty.

### Stock collectors

`go_*` / `process_*` / `promhttp_*` live on the default registry outside the manager. `telemetry.Gatherer` wraps `DefaultGatherer` with `FilteredGatherer` when the policy is active and has exposure actions — shrinks the scrape for stock families a `drop` matched. Passthrough when nothing is customized. Third-party collectors under other prefixes cannot be `keep`'d by name (normalization would prepend `erpc_`); `subject: "*", action: drop` still removes them via the filter.

### Validation (hard fail at config load)

- Empty subject; `*` not final; unknown action; duplicate family/label subjects; entry with no action/labels/buckets; non-increasing / NaN buckets.

## 5. Leftovers / deprecations

| Knob | Status |
|---|---|
| `metrics.customizations` | **Canonical** surface |
| `histogramBuckets` | Retained — global default for empty-code-bucket histograms |
| `counterIdleEvictionAfter` | Unchanged; applied via `Configure` |
| `histogramDropLabels` / `histogramLabelOverrides` | **Deprecated** — desugared into kind-masked rules |
| `counterDropLabels` / `counterLabelOverrides` | **Deprecated** — same |
| `exposeMetrics` / `dropMetrics` | **Do not add** — superseded by `customizations` |
| `metrics.enabled` / `ERPC_NOMETRICS=1` | Orthogonal (HTTP server / empty registry) |

## 6. Observability of the manager

- INFO when customizations applied (`customizations`, `exposed`, `total`).
- WARN unmatched subjects; WARN ignored exact-subject rules.
- Error vs Warn on Configure failure (§3) — do not page on bucket typos.
- No meta-metric for "families dropped" — absence on `/metrics` is the signal.

## 7. Backward compatibility

- Empty customizations + unused legacy fields → scrape behaviour matches pre-manager defaults (all families, full labels, prior bucket rules).
- Legacy label fields keep working with identical kind scope.
- Package-level metric vars keep types; call sites unchanged.
- **Test-visible:** formerly-`promauto` families require `Configure` (or test `SetHistogramBuckets` for histograms-only) before they appear on gather — same class of Init-gating deferred counters already had.
- Repeat `Configure` on the same registry leaves already-registered families untouched (frozen label-set hash); changing customizations needs a process restart.

## 8. Locked vs follow-on

Locked in this design:

- Stock collectors: explicit — name them under `keep` if an allowlist should retain them.
- Definition syntax: package-level `Define*` vars.
- Family exposure actions: `keep`/`drop` with drop-all + keep carve-out for allowlists.
- Legacy counter-drop scope: kind mask preserves counter-only behaviour.

Follow-on (see `plan.md`):

1. **No-op / allocation-free record path** for dropped families.
2. **Unified handle API** across kinds (Phase 2).
3. **When to remove** the four deprecated label fields (config-level compat until operators migrate).

## Appendix — related documents

- `plan.md` — phased implementation plan.
- Metrics catalog: `docs/pages/reference/metrics.mdx` / <https://docs.erpc.cloud/reference/metrics>.
- Operator monitoring: `docs/pages/operation/monitoring.mdx`.
