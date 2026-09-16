# SVM Slot-Grouped Consensus — Implementation Plan

**Last revised**: 2026-09-16
**Spec**: [feature.md](./feature.md) — single source of truth for behavior
**Gaps**: [svm-consensus-gaps.md](./svm-consensus-gaps.md)
**Cache**: [svm-cache-gaps.md](./svm-cache-gaps.md) — out of this plan

Companion to [feature.md](./feature.md). Phased so each step ships value
independently; slot-grouped voting is the headline correctness piece.

---

## Decisions

Settled choices — implement these unless new evidence forces a reopen.
Behavior detail lives in feature.md; this table is the implementer’s checklist.

| Decision | Choice |
|----------|--------|
| Pin location | Response `context.slot` (not request rewrite) |
| Agreement / hashing | Full result minus per-method `ignoreFields`. End-state SVM envelope defaults: ignore only `context.apiVersion` (Phase 1) |
| Winner / wait / short-circuit / misbehavior / mix | Count-first + slot-tiebreak; wait/short-circuit only for equal top count at higher slot; cross-slot ≠ misbehavior; mix on winning slot cohort (feature.md §3.1–3.6) |
| Mixed slotted / non-slotted | Prefer slotted qualifying groups (§3.1) |
| Activation / rollout | Auto on parseable `context.slot`; binary/network canary; rollback = redeploy (§3.3) |
| Financial threshold | No code default change; **docs recommend** raising `agreementThreshold` above 2 for `getBalance` / `getAccountInfo` / other financial soak methods when upstreams allow (§3.3) |
| Paired finality (§4) | Optional with/after §3; tip = `SvmHighestFinalizedSlot` (`PickServedTip`) |
| Slot-aware cache / neverCache | Out of this plan — [svm-cache-gaps.md](./svm-cache-gaps.md) |
| Nested preferHighestValueFor / SVM leader / bare-0 emptyish | Out of scope (gaps doc) |
| Operator failsafe / helm wiring | Out of scope |

---

## Phase 0 — Spec (this PR)

- [x] Write `specs/svm-realtime-consensus/feature.md`
- [x] Write this plan
- [x] Write `svm-consensus-gaps.md`
- [x] Write `svm-cache-gaps.md`
- [ ] Land draft for review

**Acceptance**: Spec reviewed and accepted as the implementation contract.

---

## Phase 1 — Moving-head consensus defaults + count-first winner

Implement feature.md §3 (defaults, winner, wait/short-circuit, misbehavior,
composition, canary rollout). Keep EVM and non-envelope paths untouched.

1. **Defaults:** in `common/defaults.go`, change enveloped-method
   `ignoreFields` from `["context.slot","context.apiVersion"]` to
   `["context.apiVersion"]` only. Map is per-method, operator-overridable
   (set replacement). Update `defaults_test.go` and consensus docs. Hashing
   stays `CanonicalHashWithIgnoredFields`; do not add a parallel hash path.
2. **Winner / misbehavior / composition / wait / short-circuit** (feature.md
   §3.1–3.6).
3. **Rollout** (feature.md §3.3): canary binary/network; no restore-old-ignore
   flag.
4. **Tests** (map to §7 acceptance):
   - No `context.slot` → legacy path.
   - Equal counts, different slots → highest slot wins.
   - **3× V@1000 vs 2× V'@1050** → V@1000 (count-first security).
   - Same slot, different values → dispute under `returnError`.
   - Mixed slotted + non-slotted → slotted wins.
   - Wait/short-circuit with fake clock.
   - Mix quota on winning slot cohort; cross-slot ≠ misbehavior.
   - Default `ignoreFields` for `getBalance` is `["context.apiVersion"]` only.

**Acceptance**: `go test ./consensus/...` + `./common/...` green; EVM /
non-envelope SVM broadcast paths unchanged.

---

## Phase 2 — Paired finality (§4)

Optional with/after Phase 1. Implement feature.md §4.

1. After successful enveloped response: `context.slot ≤ SvmHighestFinalizedSlot`
   (PickServedTip) **and** `effectiveCommitment == finalized` → finality
   `finalized`.
2. Do not promote confirmed/processed via this path.
3. Tests: finalized + slot ≤ tip → `GetFinality` finalized; confirmed →
   remains realtime.

**Acceptance**: Finality/metrics correct; no implication that never-cache
methods are stored.

---

## Phase 3 — Docs (ride along with Phase 1–2)

1. Update `docs/pages/config/failsafe/consensus.mdx` — count-first winner,
   wait/short-circuit, misbehavior, canary rollout, **recommended**
   `agreementThreshold` above 2 for financial methods (§3.3).
2. Update finality-related docs when §4 lands. Cache docs when work in
   [svm-cache-gaps.md](./svm-cache-gaps.md) ships (separate track).

**Acceptance**: Agent/docs panels match shipped behavior.

---

## Non-goals in this plan

- Slot-aware cache / neverCache policy ([svm-cache-gaps.md](./svm-cache-gaps.md))
- Operator / helm failsafe enablement (separate from this source change)
- SVM `*BlockHeadLeader` leader selection
- Nested field paths for `preferHighestValueFor`
- Architecture-aware emptyish for bare `0`
- Custom consensus policies engine (#1088) — orthogonal; may compose later
