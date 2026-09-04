# Unified Metrics Manager — Implementation Plan

**Status**: Draft — for review
**Last revised**: 2026-09-01

Companion to [feature.md](./feature.md). Phase 1 ships the manager skeleton and `metrics.customizations` together; handle unification and allocation work follow. Nothing in any phase is throwaway — each phase is a layer the next one builds on.

---

## Locked decisions

| Decision | Choice |
|----------|--------|
| Headline design | Unified metrics manager — `Define*` at init, `Configure` after config |
| Operator surface | **`metrics.customizations`** — one list for exposure, labels, buckets |
| Family / label actions | **`keep` \| `drop`** only (no `expose` / `include` / `exclude`) |
| Allowlist shape | Explicit: `subject: "*"`, `action: drop`, then `action: keep` carve-outs |
| Precedence | **Specificity**, not list order: exact > longer prefix > shorter > `*`; ties → later entry; legacy below equal-spec customization |
| Gating | **Never register** dropped eRPC families; `FilteredGatherer` only for stock collectors |
| Buckets field | `customizations[].buckets: []float64` (strictly increasing); global `histogramBuckets` string retained |
| Histograms | **`DefineLabeledHistogram` only** — no plain `DefineHistogram` / `promauto` histograms |
| Gauges | **No label projection** |
| Legacy label knobs | Deprecated, desugared with **kind masks** (counter drops stay counter-only) |
| `exposeMetrics` / `dropMetrics` | Do not add — superseded by `customizations` before any release |
| Call sites (Phase 1) | Unchanged package-var types; `rebuildInPlace` keeps pointers stable |

## Glossary

- **Manager** — `telemetry/manager.go`: definition index, `Configure`, `Gatherer`, diagnostics.
- **MetricPolicy** — `telemetry/policy.go`: compiled rules; `Exposed` / `labelIndices` / `BucketsFor`.
- **Customization** — one `metrics.customizations[]` entry (`subject`, optional `action`/`labels`/`buckets`).
- **Family** — Prometheus metric family name, e.g. `erpc_upstream_request_total`.
- **Stock collectors** — `go_*` / `process_*` / `promhttp_*` on the default registry; scrape-filtered only.

---

## Phase 1 — Manager + `customizations`

One registration path for all families, with `metrics.customizations` enforced at registration. **No call-site changes.** New families added on main after this work starts (e.g. `upstream_misbehavior_total`) go through `Define*` like everything else — open-ended set, no catalog hard-coding.

### 1.1 Core packages

| Path | Role |
|---|---|
| `telemetry/manager.go` | `DefineCounter` / `DefineGauge` / `DefineLabeledCounter` / `DefineLabeledHistogram`, `Configure`, `SetHistogramBuckets`, `Gatherer`, `KnownFamilies`, `ExposedFamilyCount`, `UnmatchedSubjects`, `IgnoredCustomizations`, `ErrNothingRegistered` |
| `telemetry/policy.go` | `NewMetricPolicy`, specificity sort, legacy desugar, validation helpers |
| `telemetry/gather_filter.go` | `FilteredGatherer` for stock (and any other) families still on the registry |
| `telemetry/labeled_counter.go` / `labeled_histogram.go` | `rebuildInPlace`; projection via current policy |
| `telemetry/metrics.go` | All families via `Define*`; `promauto` gone |
| `common/config.go` | `Customizations`, `MetricsCustomizationConfig`, `MetricLabelCustomizationConfig`, `TelemetryOptions()`; legacy fields marked `Deprecated` |
| `common/validation.go` | `MetricsConfig.Validate` compiles policy |
| `erpc/init.go` | `Configure` + severity split; startup INFO/WARN; metrics server uses `telemetry.Gatherer(...)` |

### 1.2 Acceptance

- `customizations: [{subject: consensus_*, action: drop}]` → consensus families absent; process boots; recording does not panic.
- Drop-all + keep carve-out → scrape contains only kept eRPC families (+ kept stock if named); order of entries does not change the result.
- `subject: "*"`, label `user`/`agent_name` drop → projected counters/histograms lose those labels; gauges unchanged.
- Per-family `buckets: [...]` overrides global and code defaults; non-increasing buckets fail Validate.
- Legacy `counterDropLabels` still counter-only; equal-spec customization wins over legacy.
- Malformed customizations → `ErrNothingRegistered` → Init **Error**; bad `histogramBuckets` → Init **Warn**, families still registered.
- Default config → behaviour comparable to pre-change scrape (all families, prior labels/buckets).
- `make test-fast` green; update tests that scrape without Init (formerly-`promauto` families now require manager registration — same semantics deferred counters already have).

### 1.3 Docs & generated types (ride along with Phase 1)

- `docs/pages/reference/metrics.mdx` — `customizations` schema, precedence, worked examples (drop subsystem, allowlist via drop-all + keep, label carve-out, per-family buckets), edge cases, observability.
- `docs/pages/operation/monitoring.mdx` — operator guidance; deprecate the four legacy label fields in the tables; interaction with `ERPC_NOMETRICS`.
- Related ops pages (`production.mdx`, `cli.mdx`, `config/example.mdx`) as needed for stale anchors.
- `typescript/config/src/generated.ts` — regenerate via `tygo generate` (or hand-mirror if tygo is release-only in the workflow).
- Verify every `SourceLink` / default cite against the landing tree; refresh family counts (~141 at design time; expect drift).

---

## Phase 2 — Unified handles + no-op path

Every call site records through manager-issued handles; dropped families become hot-path no-ops (near-zero ns/op, zero allocs).

Phase 1 leaves call sites on package vars. Dropped families are already absent from scrape/collection; they still pay whatever cost the unregistered Vec path has.

### Scope

- Generalize handle caches; issue no-op handles when `!Exposed(family)`.
- Migrate call sites mechanically across `erpc/`, `upstream/`, `data/`, `health/`, `architecture/`, …
- Keep default-config names/labels identical.

### Acceptance

- Benchmark: record on a dropped family ≈ no-op.
- `make test-fast` green; no default-config series shape change.

---

## Phase 3 — Allocation optimization

Only where profiling justifies: pooled label sets, consolidate remaining handle caches into the manager, scrape-latency benchmarks before/after a representative drop set.

---

## Test plan summary

| Layer | Tests |
|---|---|
| Policy compile / specificity / legacy / buckets / validation | `telemetry/policy_test.go` |
| Manager register / skip / rebuild / gatherer / error sentinel | `telemetry/manager_test.go`, `gather_filter_test.go` |
| Labeled counter/histogram projection | labeled_* tests updated for policy |
| Config Validate | `common` validation tests |
| Init severity split | Configure error sentinel + Init logging contract |
| End-to-end HTTP scrape allow/deny | `Init` + GET `:port/metrics` assert families present/absent |
| No-op handle benchmarks | Phase 2 |
| Suite | `make test-fast` green; `make test` (race) before merge |

---

## Risks / watch-items

- **Phase 1 diff size.** ~140 definition conversions is mechanical but large; mitigate with a pure-rename first commit (factory swap, no behaviour change) and customizations as a second commit in the same PR.
- **Prometheus freeze.** Label/bucket/exposure changes are startup-only; a second `Configure` on the same registry does not reshape already-registered families — document and do not pretend otherwise.
- **Tests scraping without Init.** Formerly-`promauto` families become Init-gated; audit tests that read `prometheus.DefaultGatherer` directly and register through the manager in test setup.
- **Over-restrictive allowlists** silently break dashboards — docs must include a recommended minimal set; the Init-time unmatched-subject WARN is the typo backstop.
- **Gauge label temptation.** Do not extend projection to gauges without a new aggregation story.
- **Third-party collectors** outside `go_`/`process_`/`promhttp_`: unreachable by exact `keep` name; `*: drop` still filters them at scrape.
- **Scope discipline.** Phase 1 is definition + registration + customizations only. Handle migration (Phase 2) must not sneak into the Phase 1 PR.
- **Deprecation removal.** Delete the four legacy label fields only after a migration window; until then kind-masked desugar is the compatibility contract.

## Appendix — mapping from earlier draft

| Earlier draft | This plan |
|---|---|
| `exposeMetrics` / `dropMetrics` | Replaced by `customizations` `keep`/`drop` |
| Allowlist = non-empty expose list | Allowlist = `*: drop` + `keep` carve-outs |
| `buckets` as string on customization | `[]float64` |
| "Any expose flips mode" | Rejected — too order-/presence-dependent |
| Kind-agnostic label drop including gauges | Labels on labeled counters/histograms only; gauges fixed |
| Separate exposure filter then manager | One `MetricPolicy` owns exposure, labels, and buckets |
