# Group-Aware Served Tip — Implementation Plan

Companion to [feature.md](./feature.md). Builds the group-aware served tip in
small, independently reviewable steps.

## Locked decisions

| Decision | Choice |
|---|---|
| Group declaration | `networks[].evm.servedTip.requiredGroups` list of upstream selectors |
| Selector syntax | Existing `use-upstream` selectors: id glob or tag glob (`common.UpstreamMatchesSelector`) |
| Group tip | Strict majority (`evm.PickServedTip`) over selector-matched upstreams |
| Global tip | `min(tip_G)` across required groups |
| Trajectory referee | Disabled for network-wide lane when `requiredGroups` is active |
| Regression guard reference | `min(group corroborated heads)` / `min(group max heads)` |
| Guaranteed-method floor | Reuse the same group-aware logic over method supporters |
| Selector-scoped requests | `requiredGroups` not applied; existing scoped majority behavior |
| Empty group selector | Warn and ignore (fail-open) in v1 |
| Per-group metrics | New gauges with `group` label |

## Phase 1 — Config & validation

### 1.1 Add `RequiredGroups` field
- File: `common/config.go`
- Add `RequiredGroups []string` to `EvmServedTipConfig` after `GuaranteedMethods`.

### 1.2 Validate selectors
- File: `common/validation.go`
- In `EvmNetworkConfig.Validate`, iterate `ServedTip.RequiredGroups` and call
  `ValidatePattern`; return a clear error naming `requiredGroups`.

### 1.3 Tests
- File: `common/validation_servedtip_test.go`
- Add valid-selector and invalid-selector cases.

### Acceptance
- `requiredGroups: ["type:external", "type:internal"]` validates.
- `requiredGroups: ["type:&"]` fails validation with `requiredGroups` in the
  error.

## Phase 2 — Per-group metrics

### 2.1 Add gauges
- File: `telemetry/metrics.go`
- Add `MetricNetworkServedTipGroupBlockNumber` and
  `MetricNetworkServedTipGroupLagBlocks` with labels `project`, `network`,
  `group`, `axis`.

### Acceptance
- Metrics compile; no labels collide with existing series.

## Phase 3 — Group-aware pick helper

### 3.1 Implement `pickGroupAwareServedTip`
- File: `erpc/networks.go`
- Signature:

```go
func (n *Network) pickGroupAwareServedTip(
    ups []common.Upstream,
    useFinalized bool,
    groups []string,
) (evm.ServedTipPick, servedTipReference)
```

- Build the global ballot for `Freshest`/`Inputs`/`Sorted`.
- For each group selector, filter `ups` with `common.UpstreamMatchesSelector`,
  run `evmTipBallot` + `evm.PickServedTip`, track the minimum tip and the
  minimum `Corroborated`/`Max` references.
- Emit per-group metrics.
- Warn (once per evaluation is too noisy; warn periodically or on transition)
  if a selector matches zero upstreams and skip that group.

### 3.2 Add `requiredGroups()` accessor
- File: `erpc/networks.go`
- Return `n.cfg.Evm.ServedTip.RequiredGroups` when served tip is enabled.

### Acceptance
- Unit-style test via `setupServedTipNetworkWith`: 2 external at 200, 2 internal
  at 100 → global tip 100.

## Phase 4 — Wire into `servedTip`

### 4.1 Branch in `Network.servedTip`
- When `len(requiredGroups) > 0 && requestSelector(ctx) == ""`:
  - Use `pickGroupAwareServedTip` over the candidate upstreams.
  - Skip the trajectory referee for the network-wide lane.
  - Pass the group-aware reference to `guardServedTipRegression`.
- Otherwise keep the existing path.

### 4.2 Update tracing
- Add attributes indicating group-aware mode when active.

### Acceptance
- Existing tests still pass.
- New group-aware tests from feature.md §8 pass.

## Phase 5 — Extend guaranteed-method floor

### 5.1 Reuse group-aware logic
- File: `erpc/networks.go`
- In `guaranteedMethodFloor`, when `requiredGroups` is non-empty, compute the
  floor as the minimum group-majority tip over the supporting upstreams for each
  configured guaranteed method.

### Acceptance
- Test: trace-capable external {200}, internal {100} → floor = 100.

## Phase 6 — Selector-scoped behavior

### 6.1 Confirm no group-aware application
- `tipCandidateUpstreams` already filters by `use-upstream` before the served-tip
  computation. Ensure `servedTip` only applies `requiredGroups` when
  `requestSelector(ctx) == ""`.

### Acceptance
- Test: `use-upstream=type:external` on the 2×200/2×100 pool returns 200, not
  the global 100.

## Phase 7 — Docs

### 7.1 Design doc
- Create `docs/design/group-aware-served-tip.md` summarizing schema, algorithm,
  guard/referee interactions, and telemetry.

### 7.2 Reference docs
- Update `docs/pages/reference/evm/block-tracking.mdx`:
  - Add `requiredGroups` to the config schema table.
  - Fix stale `clusterDelta` / velocity-gate language to match the current
    majority-order-statistic implementation.
  - Document group-aware behavior, metrics, and interaction with selector-scoped
    lanes.

## Phase 8 — Final verification

- `make fmt`
- `make test-fast`
- Optional: `make test` (race) for the served-tip package.

## Risk register

| Risk | Mitigation |
|---|---|
| Disabling the referee removes a safety net | Documented trade-off: `requiredGroups` is a stronger guarantee than the referee can provide without breaking it. Operators can omit `requiredGroups` to keep the referee. |
| Empty group selector silently ignored | Warning log; future metric can make it alertable. |
| Per-request cost with many groups | Typical deployment has 1–3 groups; algorithm is O(groups × pool). |
| Regression guard with min reference misses some poison patterns | Acceptable: the guard catches global drops below what every group corroborates; per-group drops are visible in per-group lag metrics. |
| Single-upstream group can hold back the whole tip | By design: a group with one member has its head as its majority, so that member's lag clamps the global tip. Operators who need redundancy inside a group should tag multiple upstreams with the same group selector. |
