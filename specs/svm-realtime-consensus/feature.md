# SVM Slot-Grouped Consensus for Moving-Head Reads — Specification

**Last revised**: 2026-09-16

Companions: [plan.md](./plan.md) · [svm-consensus-gaps.md](./svm-consensus-gaps.md) · [svm-cache-gaps.md](./svm-cache-gaps.md)

---

## 1. Purpose

Enable **multi-provider consensus** on Solana moving-head reads
(`getBalance`, `getAccountInfo`, `getTokenAccountBalance`, …) without the
false disputes that naive hash agreement produces when healthy upstreams
answer at adjacent rooted slots.

After this feature:

- Financially critical enveloped reads no longer trust a single upstream.
- Under end-state defaults, agreement requires the same payload at the same
  `context.slot` (slot is part of the hash digest).
- Winner selection is **count-first**, with highest slot only among equal top
  counts (§3.1) — a smaller group at a higher slot never beats a larger group.
- Slot-pinned strict consensus (`getBlock`, `getTransaction`, …) is unchanged.

### Goals

- Response-side pinning via `context.slot` (no request-side slot rewrite).
- End-state enveloped defaults + count-first / wait / misbehavior rules (§3).
- Optional **§4 paired finality** with/after §3. Slot-aware cache is tracked
  separately in [svm-cache-gaps.md](./svm-cache-gaps.md).
- Docs recommend raising `agreementThreshold` above 2 for financial methods
  when the upstream set is large enough (§3.3) — no product-default change.

### Non-goals

- Do **not** use `preferHighestValueFor` / `agreementThreshold: 1` on
  moving-head reads (tip routing, not consensus).
- Do **not** treat `minContextSlot` as a pin (floor only); do **not** invent
  a request-side historical slot the wire protocol does not provide.
- Out of scope ([svm-consensus-gaps.md](./svm-consensus-gaps.md)): SVM
  block-head leader behaviors, nested `preferHighestValueFor` paths,
  bare-`0` emptyish semantics. Operator failsafe / helm wiring is out of
  scope — this feature is executor behavior once a rule already matches.

---

## 2. Background (why today fails)

Why flat hash consensus cannot work for enveloped moving-head reads, using
`getBalance` as the running example.

### Moving-head reads

A moving-head read names *what* to look up (e.g. a pubkey) but not *which
slot* — each upstream answers at its current bank and reports that bank as
`context.slot`. Methods whose result *can* carry that envelope:
`contextSlotMethods` in
[`hooks.go`](https://github.com/erpc/erpc/blob/e8a375a1d5b740fe13c1d50a9f3b06758fa7c933/architecture/svm/hooks.go#L654-L672)
(see also §5).

**Request** — names a pubkey (and optional commitment), **not** a historical
slot:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "getBalance",
  "params": [
    "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
    { "commitment": "finalized" }
  ]
}
```

**Response** — Solana `RpcResponse<u64>`: bank tip stamped as `context.slot`,
balance in `value`:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "context": { "slot": 1000, "apiVersion": "2.0.15" },
    "value": 12345
  }
}
```

Under `commitment: finalized` that bank is still the latest **rooted** tip —
it advances roughly once per slot — so two healthy nodes routinely return the
**same `value` at adjacent slots** (e.g. `12345@1000` and `12345@1001`).

**Slot duration note:** mainnet slot time is **~300ms today** (staged
reduction from the historical **400ms**; target **200ms**). See
[Reduced Slot Times](https://solana.com/upgrades/reduced-slot-times)
(SIMD-0525). Do not hard-code 400ms into wait budgets — derive from observed
inter-provider root lag / adaptive caps.

`GetFinality` correctly classifies `getBalance` **realtime** at every
commitment (`architecture/svm/finality.go` — also in `neverCacheMethods`).
That is a cacheability fact, not a consensus strategy. Slot-pinned methods
(`getBlock`, `getTransaction`) are out of this problem: the request already
names the slot or signature.

### Why naive consensus fails

Today’s defaults ignore **both** `context.slot` and `context.apiVersion` in
the value hash (`common/defaults.go`). Ignoring `apiVersion` is right
(mixed validators). Ignoring **`context.slot`** collapses adjacent tips into
one bucket: flat agreement mixes **different questions**. That yields **false
disputes** when tip churn briefly splits values, or a **stale majority** when
an older slot outvotes a fresher tip, under `returnError` /
`agreementThreshold ≥ 2`.

Cross-slot lag is expected SVM behavior, not misbehavior. Same-slot
**`value`** split is a real dispute. Fix: end-state defaults + count-first
winner (§3) — dropping `context.slot` from ignores alone is not enough.

### Why EVM’s approach does not transfer

| | EVM | SVM `getBalance` (and other enveloped methods) |
|---|---|---|
| Pin location | Request (`latest`/`finalized` → block **number**) | No slot in `params` to rewrite |
| `minContextSlot` | N/A | Floor only, **not** a pin |
| Self-pin | Block number in request | **`result.context.slot` in the response** |

---

## 3. Solution — moving-head consensus

Consensus grouping is unchanged at the mechanism layer: hash each successful
response with that method’s `ignoreFields` (strips those paths from the
digest only — response body unchanged), then apply threshold / dispute /
prefer rules. This feature changes the **winner policy** when responses carry
`context.slot`.

For **`contextSlotMethods`**
([`hooks.go`](https://github.com/erpc/erpc/blob/e8a375a1d5b740fe13c1d50a9f3b06758fa7c933/architecture/svm/hooks.go#L654-L672))
under active consensus, **`context.slot` is always used** for slot-grouped
winner selection when the response has a parseable slot. End-state
**defaults** keep `context.slot` in the digest (ignore only
`context.apiVersion` — [plan.md](./plan.md) Phase 1) so hash cohorts match
slots. Do not invent a second grouping key beside the hash; do not change
`ignoreFields` semantics. Re-adding `context.slot` to ignores for these
methods collapses adjacent tips and defeats this feature.

`getBalance` responses already self-pin via `result.context.slot`.
`value @ rooted-slot-N` is immutable for that N. Under end-state defaults,
identical lamports at different slots are **different** hashes.

### 3.1 Algorithm

How one consensus round decides a winner for an enveloped read such as
`getBalance` under end-state defaults:

1. Fan out to `maxParticipants` upstreams (existing executor).
2. Hash each successful response with that method’s `ignoreFields` (end-state
   default for enveloped SVM: ignore only `context.apiVersion` — see plan).
3. A hash group **qualifies** when `count ≥ agreementThreshold`.
4. **Winner (count-first, slot-tiebreak):** only hash groups with a
   parseable `context.slot` compete. A **non-slotted** success (no usable
   `context.slot`) **never** wins in slot-grouped mode.
   - Let `C` = maximum `count` among qualifying **slotted** groups.
   - Among slotted groups with `count == C`, pick the **highest**
     `context.slot` (freshest among equal top counts).
   - If no slotted group qualifies → no winner from this path (`returnError`
     / low-participants under production policy) — do **not** fall back to a
     non-slotted majority.
5. Same slot + different values at/above threshold with no unique winner →
   real dispute → `disputeBehavior` (production: `returnError`).
6. Different slots alone are **not** a dispute; keep collecting until
   wait-caps fire or all participants answer.
7. If no group qualifies by round end (`maxWaitOnResult` / collection done) →
   `returnError` under production policy (or low-participants if
   `validParticipants < agreementThreshold`).

**Security note:** a minority at a fabricated high slot **cannot** beat a
larger honest group at a lower slot. Example: 3× `V@1000` and 2× `V'@1050`
(both ≥ threshold) → winner is `V@1000` (count 3 > 2). Slot only breaks ties
when counts are equal (e.g. 2× `V@1000` vs 2× `V@1002` → `V@1002`).

Example round for one `getBalance` (lamports @ slot), equal counts:

```
t=0:  QN=12345@1000, Alchemy=12350@1002, Helius=12345@1000
      → 12345@1000 (2) qualifies; 12350@1002 (1) does not
t=+Δ: Helius → 12350@1002
      → both groups count=2 → pick highest slot → 12350@1002
```

### 3.2 Wait semantics

While a qualifying group at count `C` exists, **wait** (subject to existing
`maxWaitOnResult` / collection rules) only while remaining participants could
still form another group with `count == C` at a **higher** `context.slot`.
Do **not** wait for a *smaller* higher-slot group to overturn a larger
majority. When the wait ends, apply §3.1.

### 3.3 Activation

Enter slot-grouped mode when **all** of:

- A consensus policy is active for the request, and
- At least one collected successful response has a parseable `context.slot`.

Fallthrough (no parseable `context.slot` on any success): existing hash
consensus unchanged. The hot path discovers the slot from the response body
(not a method-name switch). Failsafe `matchMethod` chooses which enveloped
methods (§5) operators enable under consensus.

No config knob whose “off” setting re-enables never-right naive hashing for
enveloped responses under `returnError`. Roll out with a **binary / network
canary**; rollback is redeploy of the previous binary (not a “restore ignore
`context.slot`” flag).

**Key rule under `agreementThreshold ≥ 2`:** a tip slot with one vote does not
qualify; “most updated” alone is still single-provider trust.

**Docs recommendation (no product-default change):** operators may raise
`agreementThreshold` above 2 for `getBalance`, `getAccountInfo`,
`getTokenAccountBalance`, `getMultipleAccounts`, and other financial /
priority-soak enveloped methods when the upstream set is large enough.

### 3.4 Misbehavior

| Situation | Misbehavior? |
|---|---|
| Same `context.slot`, different `value` vs winning group | **Yes** |
| Different `context.slot` (lag / ahead of winner) | **No** |
| Infrastructure / consensus-valid errors | Existing rules (errors ≠ data misbehavior) |

`punishMisbehavior` / cordon only applies to same-slot value dissenters when
the winning group has a clear majority (`count > validParticipants/2` within
the **winning slot cohort**, not the whole round).

### 3.5 Composition quotas

Mix quotas (`requiredParticipants` / `minAgreement`) still apply, but only
among upstreams that agreed on the winning value **at the winning slot**.
Cross-slot participants do not count toward the quota.

`eth_sendRawTransaction` / SVM send broadcast exemption unchanged (not this
path).

### 3.6 Short-circuit

Do **not** short-circuit while `R` remaining participant responses could
still raise some hash group’s count to the current top qualifying count `C`
at a **higher** `context.slot` than the provisional winner (or could create a
new top count). Only participant **count** enters the predicate — not observed
inter-provider lag.

- Do **not** short-circuit to a lone tip-slot response (`count < agreementThreshold`).
- Existing unassailable-lead / error-threshold short-circuits otherwise apply
  when the rule above says no higher equal-count tip can still form.

---

## 4. Paired finality (optional follow-on)

**Today:** every moving-head enveloped read is `realtime` for `GetFinality`
(including `commitment: finalized`), because the request names no slot.

**After §4:** classify the response **`finalized`** (immutable **at that
slot**) when **all** hold:

1. A successful response (consensus winner or single success) has parseable
   `result.context.slot` = `N`.
2. `N ≤` the network’s served finalized tip — prefer
   `Network.SvmHighestFinalizedSlot` (`PickServedTip` / majority-style over
   upstream pollers), not a single upstream’s poller alone (exact wiring in
   [plan.md](./plan.md)).
3. The request’s **effective commitment** is `finalized`
   (`IsFinalizedCommitment` / same predicate as injection).

**Must not:** promote `commitment: confirmed` / `processed` solely because
`context.slot ≤` tip.

**Example:** caller asks with `commitment: finalized`; answer has
`context.slot: 1000`; network served finalized tip is `1005` → response
finality = **`finalized`** (at slot 1000). This does **not** by itself write
the cache ([svm-cache-gaps.md](./svm-cache-gaps.md) / `neverCacheMethods`).

Prefer §3 first (canary binary/network). §4 optional with/after §3.

---

## 5. In scope / out of scope

**In scope:** enveloped moving-head methods (`contextSlotMethods` in
[`hooks.go`](https://github.com/erpc/erpc/blob/e8a375a1d5b740fe13c1d50a9f3b06758fa7c933/architecture/svm/hooks.go#L654-L672)).
Operators choose `matchMethod` coverage. **Priority soak:**
`getAccountInfo`, `getBalance`, `getTokenAccountBalance`,
`getMultipleAccounts`.

**Out of scope:** bare / non-envelope methods; tip policies already elsewhere
(`getSlot` / `getBlockHeight` freshest-wins, `getLatestBlockhash`
fastest-wins, `sendTransaction` broadcast); slot-pinned strict (`getBlock`,
`getTransaction`, …); items in [svm-consensus-gaps.md](./svm-consensus-gaps.md).

---

## 6. Observability

| Signal | Purpose |
|---|---|
| Existing `erpc_consensus_*` with `finality=realtime` | Baseline volume / dispute / low_participants |
| New (recommended): `erpc_consensus_slot_groups` / winning `context.slot` span attr | Prove grouping; debug false disputes |
| Misbehavior metric must not spike on cross-slot lag | Regress if 3.4 is broken |
| `erpc_consensus_wait_capped_total{trigger}` | Tune wait vs root lag |

---

## 7. Acceptance criteria

Framed on `getBalance` (same rules for other enveloped moving-head methods):

1. Same lamports, different `context.slot`, equal counts at threshold → **no**
   dispute; highest slot wins among equal counts (§3.1) — never punish for lag.
2. Same `context.slot`, different `value`, threshold unmet / tied → dispute
   under `returnError`.
3. **Count-first security:** 3× `V@1000` and 2× `V'@1050` (both ≥ threshold)
   → winner `V@1000`.
4. Tip with count below the current top count does not overturn; wait only
   while remaining participants can still form an **equal** top count at a
   higher slot (§3.2 / §3.6).
5. Non-slotted successes never win in slot-grouped mode (even if they outnumber
   slotted groups); if no slotted group reaches threshold → error, not
   non-slotted fallback.
6. Mix `minAgreement` enforced on winning **slot** cohort.
7. Slot-pinned strict path for `getBlock` / `getTransaction` unchanged.
8. §4 (if shipped): `commitment: confirmed` is **not** classified
   `finalized` solely because `context.slot ≤` tip; finalized commitment +
   slot ≤ `SvmHighestFinalizedSlot` may be.

---

## 8. Related

- Consensus gaps: [svm-consensus-gaps.md](./svm-consensus-gaps.md)
- Cache gaps: [svm-cache-gaps.md](./svm-cache-gaps.md)
- Implementation plan: [plan.md](./plan.md)
- Envelope inventory: [`architecture/svm/hooks.go#L654-L672`](https://github.com/erpc/erpc/blob/e8a375a1d5b740fe13c1d50a9f3b06758fa7c933/architecture/svm/hooks.go#L654-L672)
- SVM finality: `architecture/svm/finality.go`
- Consensus executor: `consensus/executor.go`, `consensus/analysis.go`
- Envelope ignore defaults: `common/defaults.go` ([plan.md](./plan.md) Phase 1)
