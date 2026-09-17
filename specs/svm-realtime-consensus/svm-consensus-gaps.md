# SVM Consensus Gaps Inventory

**Last revised**: 2026-09-16
**Feature in scope**: [feature.md](./feature.md) — slot-grouped moving-head reads
**Plan**: [plan.md](./plan.md)

Source-code gaps and wire/protocol facts that force the slot-grouped design.
Operator / helm failsafe wiring is out of scope. Behavior contract:
[feature.md](./feature.md).

---

## 1. In scope for this feature

| Gap | Kind | Notes |
|---|---|---|
| Naive hash consensus ignores `context.slot` | **Source** | End-state `ignoreFields` + count-first ([feature.md](./feature.md) §3) |
| Default `ignoreFields` still strips `context.slot` | **Source** | End state: ignore only `context.apiVersion` ([plan.md](./plan.md) Phase 1) |
| Moving-head reads always `realtime` even when slot ≤ tip under finalized commitment | **Source** | §4 paired finality |
| Cache keys / neverCache policy | **Source** | Tracked in [svm-cache-gaps.md](./svm-cache-gaps.md) — out of consensus feature |

---

## 2. Wire / protocol (not a missing eRPC API)

| Fact | Implication |
|---|---|
| Moving-head methods have **no** slot pin in the request | Cannot mirror EVM tag→block rewrite on the request |
| `commitment: finalized` = latest **rooted** head (advances every slot), not immutability | `GetFinality` correctly keeps these **realtime** today |
| `minContextSlot` is a floor, not a pin | Must not be used as a fake pin |
| Envelope carries `context.slot` | Response-side pinning is the weakest correct design |

Slot-time / lag implications for wait budgets: feature.md §2.

---

## 3. Related source gaps — **out of scope** for this feature

| Gap | Where | Why out of scope |
|---|---|---|
| `onlyBlockHeadLeader` / `preferBlockHeadLeader` only resolve on EVM | `consensus/analysis.go` | Production SVM policy uses `returnError`, not leader behaviors |
| `preferHighestValueFor` resolves only top-level / `"result"` | `consensus/utils.go` | Tip/highest-value path; non-goal for moving-head hash agreement |
| Bare `0` treated as emptyish | `util/bytes.go` | Envelope results are not bare `0`; tip integers are a separate edge |

Track separately if a future ticket needs them; do not block this feature.
