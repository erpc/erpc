# Unified Metrics Manager — Implementation Plan

**Status**: Draft — for review
**Last revised**: 2026-08-19

Companion to [feature.md](./feature.md). The manager skeleton and its first policy (exposure control) ship together in Phase 1; handle unification and allocation work follow. Nothing in any phase is throwaway — each phase is a layer the next one builds on.

---

## Locked decisions

| Decision | Choice |
|----------|--------|
| Headline design | **Unified metrics manager** — one owner for define → register → record |
| First policy | **Exposure control** (`metrics.exposeMetrics` / `metrics.dropMetrics`), shipped in Phase 1 |
| Gating mechanism | **Never register** unexposed families — no unregister path, because nothing registers before Init |
| Name convention | eRPC metrics without `erpc_` prefix; stock collectors by full name |
| Patterns | Exact family names + trailing-prefix subsystem patterns (`consensus_*`); `*` legal only as final character |
| Precedence | Allowlist first, denylist second; both empty = expose all (backward compatible) |
| Stock collectors | `FilteredGatherer` safety net at the HTTP handler — not the CPU-saving mechanism |
| Call sites | Unchanged in Phase 1 (package vars keep types); unified handles in Phase 2 |

## Glossary

- **Manager** — the `telemetry/` singleton owning metric definitions, registration, policies, and handle issue.
- **Definition index** — the manager's name → definition table, populated at package init by the factory. Seeds registration gating and exact unknown-name validation.
- **Deferred metrics** — today's ~36 `LabeledCounter` + ~17 `LabeledHistogram` built unregistered at package init; the half-done unification this plan completes.
- **`promauto` metrics** — the ~86 metrics (28 gauges, 54 counters, 4 histograms) currently self-registering at package init; converted to the factory in Phase 1.
- **Family** — a Prometheus `MetricFamily` name, e.g. `erpc_upstream_request_total`.
- **Prefix pattern** — a config entry ending in `*`, matching every family whose resolved name starts with the stem (`consensus_*` → all `erpc_consensus_*`). Trailing-prefix only; no general globs.

---

## Phase 0 — Exposure filter core (pure, no behavior change)

The policy primitive the manager consults. Already prototyped in `telemetry/gather_filter.go` / `gather_filter_test.go` — polish and keep.

### 0.1 `telemetry/` — filter type
- `NormalizeMetricName(name string) string` — trims, accepts `erpc_`-prefixed as-is, passes `go_*`/`process_*`/`promhttp_*` through, prefixes everything else with `erpc_`.
- `MetricExposureFilter` holding exact-match sets + trailing-prefix lists for allow and drop, with `Exposed(name) bool` (exact hit or prefix hit).
- `NewMetricExposureFilter(expose, drop []string) (*MetricExposureFilter, error)` — validates empty strings, non-trailing `*`, and duplicates; splits entries into exact vs prefix at build.
- `UnmatchedEntries(filter, knownFamilies)` helper returning configured entries that matched nothing — drives the Init-time WARN (Phase 1.4).
- `FilteredGatherer` — `prometheus.Gatherer` wrapper filtering `[]*dto.MetricFamily` post-gather (stock-collector safety net, wired in Phase 2).

### Acceptance
- Unit tests: normalize (prefixed/unprefixed/stock/empty), exact-only, prefix-only, mixed exact+prefix, allow-only, drop-only, allow+drop precedence, passthrough when both empty, duplicate/empty/mid-string-`*` validation errors, zero-match detection.
- No registration-path changes yet; existing tests green.

---

## Phase 1 — Manager skeleton + exposure control (the headline)

One registration path for all ~139 families, with the exposure policy enforced at registration. Ships the operator-facing knob. **No call-site changes.**

### 1.1 `telemetry/` — manager + factory
- `Manager` struct: definition index (`map[string]definition` — name, kind, opts, schema, built vec), exposure filter, label filters.
- Factory: `DefineCounter(opts, schema)` / `DefineGauge(opts, schema)` / `DefineHistogram(opts, schema, buckets)` — build the vec, index it, return the wrapper; **never register**.
- `Manager.Configure(exposure *MetricExposureFilter, ...)`: per definition — skip if unexposed, else apply label projection and register on `prometheus.DefaultRegisterer`.
- Absorbs `RebuildFilteredCounters()` and `SetHistogramBuckets()` (including bucket parsing and the handle-cache reset).

### 1.2 `telemetry/metrics.go` — convert all definitions
- Deferred counters/histograms: mechanical swap to the factory (they're already unregistered-at-init).
- The ~86 `promauto` metrics: `promauto.New*(...)` → `manager.Define*(...)`. Package vars keep their existing types (`*prometheus.GaugeVec`, `LabeledCounter`, `LabeledHistogram`, …) so **every call site compiles unchanged**; observations on a not-yet-registered vec are legal and invisible until registration.
- End state: `grep -c promauto telemetry/metrics.go` → 0.

### 1.3 `common/config.go` — config fields
- Add to `MetricsConfig`:
  - `ExposeMetrics []string \`yaml:"exposeMetrics,omitempty" json:"exposeMetrics,omitempty"\``
  - `DropMetrics []string \`yaml:"dropMetrics,omitempty" json:"dropMetrics,omitempty"\``
- `common/validation.go`: validate via `NewMetricExposureFilter` from `MetricsConfig.Validate()`; reject duplicates/empties.
- No defaults in `common/defaults.go` (nil = expose all).

### 1.4 `erpc/init.go` — wiring
- In the `cfg.Metrics != nil` block (~L49-68): build the exposure filter, pass it with the existing label filters into `manager.Configure(...)` — replacing the `SetHistogramLabelFilter` / `SetCounterLabelFilter` / `RebuildFilteredCounters` / `SetHistogramBuckets` call sequence with the single manager entry point.
- INFO log when exposure is active: allowlist/denylist sizes.
- WARN per configured entry (exact name or prefix) matching nothing in the definition index — exact at Init, no first-scrape wait.

### Acceptance
- `dropMetrics: [network_evm_block_range_requested_total]` → family absent from `/metrics`; process boots; recording on it doesn't panic.
- `dropMetrics: [consensus_*]` → all consensus families absent; typo'd prefix (`consenus_*`) → WARN, boot succeeds.
- `dropMetrics` covering a former-`promauto` gauge (e.g. `upstream_block_head_lag`) → absent from scrape (no unregister needed — it was never registered).
- `exposeMetrics: [upstream_request_total]` → only that eRPC family registered (stock collectors still present until Phase 2's gatherer).
- Default config → scrape byte-comparable to baseline (all families, same names/labels).
- Typo'd name → WARN at Init, boot succeeds.
- `make test-fast` green; update tests that scrape without Init (formerly-`promauto` families now require manager registration — same semantics deferred counters already have).

---

## Phase 2 — Unified handles + no-op path

Every call site records through manager-issued handles; disabled families become hot-path no-ops.

### 2.1 Handle API
- Generalize `CounterHandle` / `ObserverHandle` (`telemetry/handles.go`) into the manager's per-kind handle issue; add the gauge equivalent.
- Handles are issued per (family, full-schema label tuple); for an unexposed family the manager returns a shared no-op handle — no map lookup, no allocation, no Prometheus call.
- Migrate call sites across `erpc/`, `upstream/`, `data/`, `internal/policy/`, `health/`, `architecture/` — mechanical, one pattern per kind.

### 2.2 FilteredGatherer wiring (stock-collector safety net)
- Metrics server block (~L159-172): wrap `prometheus.DefaultGatherer` with the Phase 0 `FilteredGatherer`, `promhttp.Handler()` → `promhttp.HandlerFor(gatherer, ...)`.
- `go_*` / `process_*` / `promhttp_*` now respect `exposeMetrics` allowlists.

### Acceptance
- Disabled family: benchmark shows near-zero ns/op, zero allocs on the record path.
- `exposeMetrics: [upstream_request_total]` → scrape contains exactly the allowlist (stock collectors filtered).
- All existing metric names/labels unchanged under default config; `make test-fast` green.

---

## Phase 3 — Allocation optimization

Only where profiling justifies.

- Pooled label sets / precomputed projections in handle issue.
- Consolidate the remaining handle caches into the manager.
- Benchmarks: record-path ns/op + allocs for exposed, label-shaved, and disabled families; scrape latency before/after a representative `dropMetrics` set.

### Acceptance
- Benchmark report in the PR; no regressions on the exposed-family hot path.

---

## Docs & generated artifacts (ride along with Phase 1)

- `docs/pages/reference/metrics.mdx` — config schema table: `exposeMetrics` / `dropMetrics` rows (type, default, semantics, name-resolution); worked example (slim allowlist); edge cases (precedence, stock-collector naming, unknown-name WARN); note the unified manager in "How it works" (initialization order section changes).
- `docs/pages/operation/monitoring.mdx` — operator guidance: recommended minimal set, footgun warning (over-restrictive allowlist silently breaks dashboards), interaction with `counterDropLabels` / `ERPC_NOMETRICS`.
- `typescript/config/src/generated.ts` — regenerate via `tygo generate` after `MetricsConfig` changes.
- Verify docs claims against code before writing (per CONTRIBUTING.md); cite `common/defaults.go` / `common/validation.go` line refs.

## Test plan summary

| Layer | Tests |
|---|---|
| Filter core | normalize, allow/deny/precedence, validation errors (Phase 0) |
| Manager | definition indexing; exposure gating per kind (counter/gauge/histogram); label projection under exposure; Init-twice safety (Phase 1) |
| Regression | default-config scrape byte-comparable to baseline; call-site safety on unregistered families (Phase 1) |
| Handles | no-op correctness; zero-alloc benchmark; call-site migration compile + behavior (Phase 2) |
| End-to-end | `Init` with allowlist → HTTP GET `:port/metrics` → assert families present/absent (Phase 2) |
| Suite | `make test-fast` green; `make test` (race) before merge |

## Risks / watch-items

- **Phase 1 diff size.** ~139 definition conversions is mechanical but large; mitigate with a pure-rename first commit (factory swap, no behavior change) and the exposure gate as a second commit in the same PR.
- **Tests scraping without Init.** Formerly-`promauto` families become Init-gated; audit tests that read `prometheus.DefaultGatherer` directly (`erpc/networks_timeout_test.go`, `erpc/networks_method_eligibility_test.go`, `test/fake_erpc.go`) and register through the manager in test setup.
- **Over-restrictive allowlists** silently break dashboards/alerts — docs must include a recommended minimal set; the Init-time unknown-name WARN is the typo backstop.
- **Init ordering**: manager configuration must complete before any registration-dependent code; the single `Configure` entry point replaces today's four-call sequence, which removes the ordering class of bugs rather than adding to it.
- **Scope discipline**: Phase 1 is definition + registration only. Handle migration (Phase 2) must not sneak into the Phase 1 PR.
