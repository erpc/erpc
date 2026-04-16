# eRPC metric cardinality audit

Date: 2026-04-02
Source ticket: `PLA-1058`

## Prod snapshot

From the 2026-04-01 Prometheus incident follow-up:

- `morpho-prd/erpc` owned about `328k` active series.
- Average scrape cost was about `32.7k` samples per target over 30m across 10 targets.
- Largest families:
  - `erpc_upstream_request_duration_seconds_bucket` about `51k`
  - `erpc_network_request_duration_seconds_bucket` about `41k`
  - `erpc_cache_get_error_duration_seconds_bucket` about `18k`
  - `erpc_network_hedge_delay_seconds_bucket` about `17k`

## Root causes

1. `erpc_network_request_duration_seconds` carried `vendor`, `upstream`, and `user` labels even though it is a network-scoped latency histogram.
2. `erpc_upstream_request_duration_seconds` carried `user`, multiplying every upstream/method/finality bucket by tenant cardinality.
3. Cache error metrics used `ErrorSummary(err)` instead of the more stable `ErrorFingerprint(err)`, so message text leaked into histogram and counter label space.
4. Cache success/miss duration histograms and `erpc_network_evm_get_logs_range_requested` still carried `user`, multiplying otherwise stable cache-policy and finality bucket families by tenant cardinality.
5. Prod configs can still opt into `scoreMetricsMode: detailed`, which is useful for debugging but expensive in steady-state.
6. Hot request counters still carried `agent_name` on top of `user`, multiplying already high-volume request and rate-limit series with low operational value.
7. `erpc_network_hedge_delay_seconds_bucket` is still method-scoped and bucket-heavy enough to remain a follow-up target.

## Changes in this patch

- Coarsened `erpc_network_request_duration_seconds` to `project, network, category, finality, outcome`.
- Coarsened `erpc_upstream_request_duration_seconds` to drop the `user` label.
- Coarsened cache duration histograms and `erpc_network_evm_get_logs_range_requested` to drop the `user` label.
- Normalized cache get/set error labels to `ErrorFingerprint(err)`.
- Dropped `agent_name` from the hottest request/rate-limit counters while preserving `user`.
- Trimmed `erpc_network_hedge_delay_seconds` from 9 explicit buckets to 5.
- Updated bundled Grafana and Datadog dashboards to query the coarsened histograms.
- Added regression tests to lock the new histogram label schemas.

## Expected impact

- `erpc_network_request_duration_seconds_bucket` should collapse by the removed `vendor x upstream x user` dimensions while keeping success/error/cache visibility through `outcome`.
- `erpc_upstream_request_duration_seconds_bucket` should collapse by the removed `user` dimension.
- Cache duration histograms and `erpc_network_evm_get_logs_range_requested_bucket` should collapse by the removed `user` dimension.
- `erpc_cache_get_error_duration_seconds_bucket` and `erpc_cache_set_error_duration_seconds_bucket` should stop churning on verbose error strings.
- Hot request counters should stop multiplying by `agent_name` while still preserving per-user drill-downs.
- `erpc_network_hedge_delay_seconds_bucket` should drop linearly with the reduced bucket count.

The duration histogram families are the largest app-side series owners in prod, so these remain the highest-leverage app changes available inside eRPC itself.

## Keep / coarsen / drop / relabel

- Keep:
  - request/error counters with upstream and user detail
  - block/head/finalization gauges
  - compact score metrics
- Coarsen:
  - network request latency histogram
  - upstream request latency histogram
  - cache duration histograms
  - `erpc_network_evm_get_logs_range_requested`
  - cache error labels
  - `agent_name` off the hottest request/rate-limit counters
  - `erpc_network_hedge_delay_seconds` bucket count
- Follow-up:
  - `PLA-1064`: hedge delay histogram dimensions
  - `PLA-1065`: CI guardrail that flags new histograms with upstream x user style label products
  - prod config audit to ensure `metrics.errorLabelMode=compact` and `scoreMetricsMode=compact`
