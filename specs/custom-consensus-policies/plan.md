# Custom Consensus Policies Engine — Implementation Plan

**Status**: Implemented (phases 1–5 + 7); Phase 6 / 8 deferred
**Last revised**: 2026-09-30
**Spec**: [feature.md](./feature.md) — **single source of truth for behavior**
**Issue**: [#1088](https://github.com/erpc/erpc/issues/1088)
**Delivery**: `feat/consensus-policy-engine-mvp`

Companion to [feature.md](./feature.md). This plan is delivery sequence +
acceptance gates only. Do not restate waiver / seal / hedge / PreferNonEmpty /
`extends` semantics here — cite the feature sections below.

---

## TLDR

Ship the selector engine first, then wire it into config, auth, and the
executor. Every phase lands green on its own acceptance gate.

- **Phases 0–2** — spec, standalone selector engine, config schema (named
  policies + `extends`).
- **Phases 3–5** — composition waivers (MissingData + empty-outside-retention),
  auth-strategy name + cordon classes, executor wiring with observability.
- **Phases 6–8** — optional decision cache, docs + E2E, optional DX extras.

---

## Locked decisions

Short index into the feature contract (full text lives there):

| Topic | Where |
|-------|--------|
| Pre-round JS → policy **name**; fail-closed inline default; `return "default"` | feature §1–§3, §4.1 |
| Auth = strategy `name` stamped on `User`; eval string-compares it; auth stays consensus-free | feature §4.3 |
| Named policies + one-level `extends` (v1) | feature §2 |
| Cordon classes + sitout on `health.Tracker` | feature §4.4–§4.5 |
| Caller-gated fallback / waived serving | feature §6–§7 |
| MissingData + empty-outside-retention waivers (both v1) | feature §7.1, R1–R12 |
| Metrics / header | feature §8 |
| Decision cache not in v1 | feature §5 |
| Dump / simulate = nice-to-have | feature §12 |
| Non-goals (post-round JS, mid-round switch, …) | feature §1 Non-goals |

---

## Phase 0 — Spec (landed)

- [x] Write `specs/custom-consensus-policies/feature.md`
- [x] Write this plan
- [x] Land for review; link from #1088

**Acceptance**: Spec reviewed and accepted as the implementation contract.

---

## Phase 1 — Standalone selector engine

Package: `internal/consensus/policy/` — no imports from `consensus/` executor.
Implements feature §3–§4.6 (stdlib, sandbox, Sobek pool).

- Sobek pool per feature §4.6 — every request with a selector still runs
  `evalFunction` (no capacity bypass)
- `EvalContext` + stdlib helpers from feature §3.1
- Fallthrough tests first (nil user, timeout, throw, empty set)
- Micro-benchmark; if p99 ≪ 1ms, leave decision cache unbuilt (feature §5)

**Acceptance**: `go test ./internal/consensus/policy/...` green; zero executor
imports; benchmark posted.

---

## Phase 2 — Config schema

Implements feature §2 field surface (named `policies` with `extends`,
`customPolicy`, waiver fields).

- One-level `extends` resolution + load-time validation (unknown base, cycle)
  per feature §2
- Validation: reserved `"default"`, no nested selection, never-waivable floor (R9)
- Inline block = anonymous default; `evalTimeout` default per feature §2
- Tygo regen

**Acceptance**: existing configs load unchanged; fixture with `policies` +
`extends` + `customPolicy` validates.

---

## Phase 3 — Composition waivers

Implements feature §7.1 + R1/R5/R9/R10/R11/R12 (both waivers).

- Wire `waiveAgreementOnMissingData` + `waiveAgreementOnEmptyOutsideRetention`
  in `enforceWinnerComposition`; block-proof order per §7.1.3
- `promoteAbstentionWinner`, hedge consensus-slot empty-keep, PreferNonEmpty
  count gate (§7.1.3–5, R11–R12)
- Wait-cap **seal** + re-run winner per §7.1 / R1
- Observability: `missing_data` / `empty_outside_retention` +
  `waiver_unproven_total` (§8)
- Characterization tests per feature §7.2 matrix rows

**Acceptance**: UC3 (caller-gated historical serving) passes; seal / no-seal
paths covered; behavior matches feature §7.1 (do not invent a second rule set
here).

---

## Phase 4 — Auth + health plumbing

Implements feature §4.3–§4.5, §6 gates, R7–R8.

- `name` on `AuthStrategyConfig`; `Auth` on `common.User`; stamped by every
  strategy (feature §4.3)
- Typed `cordonClass` + per-class tracker bits; sitout / rate limiter on tracker
- `anyPunished` / `anyOperatorCordon` over **network** set (incl. selection-dropped)
- `blockNumber` for eval ctx

**Acceptance**: matches feature §6 table (punish/admin → default; availability →
`fallback` for the fallback strategy).

---

## Phase 5 — Executor wiring

Implements feature §4.1, §8, §3 `"default"` resolve.

- Pre-round resolve → run existing executor under selected config
- Fail closed on error / timeout / unknown name (not on pool capacity — §4.6)
- `return "default"` resolve per §3
- Output-only `X-ERPC-Consensus-Policy` + policy metrics (§8)
- Load benchmark: selector on vs off; p99 delta ≪ 1ms

**Acceptance**: UC1–UC3 fixtures; no mid-round switch; `"default"` path covered.

---

## Phase 6 — Decision cache (optional)

Only if Phase 1 forces it. Design: feature §5.

**Acceptance**: per feature §5 (hits, invalidation, cardinality guard).

---

## Phase 7 — Docs + E2E

- Operator docs (`consensus.policies` / `customPolicy` / waivers); hedge exemplar
- E2E for UC1–UC3 + §7.2 matrix
- Update #1088

**Acceptance**: docs build; E2E green; issue updated.

---

## Phase 8 — Nice-to-haves (optional DX)

Implements feature §12 if chosen.

- Resolved-policy dump (§12.1); optional selector simulate (§12.2)
- `"default"` resolve already required in Phase 5 / §3

**Acceptance** (if built): per feature §12.

---

## Delivery status

Phases **1–5 and 7** are implemented on `feat/consensus-policy-engine-mvp`. Phases **6** (decision cache) and **8** (§12 dump/simulate) remain deferred — they are not part of this delivery.

## Out of scope

See feature §1 Non-goals and §12.3. Plan does not add a second list.

---

## Risks / watch-items

Build hazards already specified in the feature — track in review, don’t re-spec:

| Risk | Feature pointer |
|------|-----------------|
| Wait-cap seal vs `fireAndForget` | §7.1.2, R1 |
| Hedge empty-as-slot-vote | §7.1.4, R11 |
| PreferNonEmpty count gate vs PreferLarger | §7.1.5, R12 |
| Waiver floor ≥ `minAgreement` (not unanimous) | §7.1.2–3, §11.5 |
| Punished/operator scan includes selection-dropped nodes | §3.1, §4.5, R7–R8 |
| Empty-waiver head trust (served-tip / min-of-two) | §7.1.3 |
| Strategy-name stamping across all auth strategies | §4.3 |
| Decision-cache cardinality (if Phase 6) | §5 |
| Scope creep → post-round JS | §1 Non-goals |
