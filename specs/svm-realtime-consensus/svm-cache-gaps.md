# SVM Cache Gaps Inventory

**Last revised**: 2026-09-16
**Consensus feature**: [feature.md](./feature.md) — slot-grouped moving-head reads
**Consensus gaps**: [svm-consensus-gaps.md](./svm-consensus-gaps.md)
**Plan**: [plan.md](./plan.md)

Cache-layer gaps and open decisions that are **separate** from consensus
voting. §3 consensus and §4 paired finality in [feature.md](./feature.md)
do not require cache changes. Do not define paired finality as “make it
cacheable.”

---

## 1. Slot-aware cache

**Today** (`getAccountInfo`, not `getBalance`):

- Finality `realtime` → matches `finality: realtime` policies.
- Partition key `networkId:*` (or `minContextSlot` if present) — **no**
  poller tip, **no** `PickServedTip`, **no** response `context.slot`.
- Staleness = policy **TTL** only.
- `getBalance` / `getTokenAccountBalance` are hard-skipped by
  `neverCacheMethods` even under a realtime policy.

**Gap:** cache keys use `minContextSlot` or `*` — no served-tip slot
dimension. Once [feature.md](./feature.md) §4 can mark an answer
`finalized`, cache policies with `finality: finalized` can match. Get/Set
should key the slot dimension from the **network served finalized tip**
and/or the response `context.slot` on Set — **not** from client params.

Example:

```text
Request 1 (tip still 1000): Get(…, slot=1000) MISS → upstream → §4
  finalized → Set(…, slot=1000)
Request 2 (identical curl, tip still 1000): Get(…, slot=1000) HIT
Request 3 (tip now 1001): Get(…, slot=1001) MISS → refetch → Set(…, 1001)
```

Entry at 1000 must not answer tip 1001. `neverCacheMethods` still wins unless
revisited (§2). §4 can still classify `getBalance` as finalized for
non-cache consumers.

**Gating:** deferred until §3 soak looks healthy and the neverCache open
topic (§2) is settled. Prefer shipping §4 before this work.

**Acceptance (when un-deferred):** unpinned `getAccountInfo` Get uses served
finalized tip as slot key; tip advance → miss.

---

## 2. Open topic — `getBalance` vs `getAccountInfo` cacheability

Today both are moving-head / `realtime` for finality, but `getBalance` (and
`getTokenAccountBalance`) are in `neverCacheMethods` (hard Get/Set skip),
while `getAccountInfo` is not. §4 can still mark either `finalized`.

Open for **cache**:

- Keep status quo (`getAccountInfo` TTL-/finalized-cacheable; balances never
  stored)?
- Put account reads on never-cache too?
- Allow balances out of never-cache under slot-keyed finalized Get/Set?

Not blocking consensus (§3) or paired finality (§4); **blocks un-deferring
slot-aware cache (§1)**.
