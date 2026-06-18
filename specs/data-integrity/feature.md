# Data Integrity — Specification

**Status**: Draft — for review
**Owner**: TBD
**Last revised**: 2026-06-18

---

## 1. Purpose

The **Data Integrity** module validates that the data an upstream returns is internally consistent and consistent with what the chain actually contains, and rejects responses that provably cannot be correct. A rejected response is converted into a standard content-validation error so the existing retry/failover machinery routes around the offending upstream.

erpc already ships most of the *checks* — they live as request directives (`EnforceLogIndexStrictIncrements`, `ValidateTxHashUniqueness`, `ValidateTransactionIndex`, `ValidateLogsBloomMatch`, `ValidateReceiptTransactionMatch`, `ValidateTransactionsRoot`, `ReceiptsCountExact/AtLeast`, `ValidationExpectedBlockHash/Number`, the `GroundTruth*` cross-validators) consumed by the EVM post-forward validators. What is missing, and what this module adds, is:

1. **Memory** — a way to remember facts about blocks we have already seen, so later responses can be checked against them.
2. **Ground-truth feeding** — automatic population of the cross-validation inputs (`ValidationExpected*`, `GroundTruth*`) that are today only settable in library mode.
3. **Tiering & cost control** — a single per-project knob that selects how much corroboration to perform and how much (if anything) to spend fetching missing data.

The module is a **data + orchestration layer**. It does not re-implement validation logic; it decides which checks to run, gathers the data points each check needs as cheaply as possible, and hands them to the validators that already exist.

## 2. Goals & non-goals

**Goals**

- Catch corruption that consensus structurally cannot: data that is wrong but that the upstreams *agree* on (shared client/indexer bugs, systemic issues).
- Make the cheap checks free and always-on; make the expensive checks opt-in and budgeted.
- Reuse the existing validator catalog and error/failover plumbing verbatim.
- Be configurable with one word per project, with escape hatches for power users.

**Non-goals**

- **Not** a second agreement/quorum mechanism. Multi-source agreement is consensus's job (see §3). This module never builds its own voting.
- **Not** a new trusted-source concept. Authoritative data is fetched through the normal network path and inherits whatever integrity that network is already configured for.
- **Not** a data-rewriting layer. The module rejects bad responses; it never edits upstream data.

## 3. Relationship to consensus

Consensus and integrity are orthogonal and complementary:

- **Consensus** answers *"do my upstreams agree with each other?"* — horizontal agreement across the upstreams serving a request. It catches a minority of bad upstreams. It is blind to corruption the majority shares, and it pays a fan-out on every request.
- **Integrity** answers *"is this answer actually valid?"* — vertical validation against invariants and known facts. Its unique value is the case consensus cannot see: when every serving upstream returns the same wrong value. It is also far cheaper for the classes of bug it covers, so it can reduce reliance on consensus.

The module deliberately has **no quorum level**. Any place that would otherwise "make N sources agree" is delegated to consensus by routing the relevant fetch through the network (see §7).

## 4. Integrity levels

A single monotonic ladder. Each level is a superset of the previous one and maps directly to cost. One knob, four stops:

| Level | Name | Evidence used | Extra upstream cost |
|---|---|---|---|
| 0 | `off` | none | none |
| 1 | `intrinsic` | the single response only | none |
| 2 | `corroborated` | response + data points **already available** (cache / previously observed) | none (memory/cache only) |
| 3 | `authoritative` | response + data points **force-fetched** when missing | bounded, budgeted |

- **`intrinsic`** catches impossible or self-inconsistent data (an out-of-range index, a bloom that disagrees with its logs, duplicate transaction hashes, malformed field lengths, a transactions root that does not match the header).
- **`corroborated`** additionally checks against facts we happen to already hold — opportunistically, with no new upstream calls.
- **`authoritative`** additionally fetches the specific missing facts (see §5, §7) so the corroboration always runs.

There is intentionally **no level above `authoritative`**; stronger guarantees come from configuring consensus on the network whose data is used as ground truth (§7), not from a higher integrity tier.

## 5. Data points and the resolver

The core abstraction. Checks do not depend on *methods*; they depend on **data points** — atomic facts about a block. Each check declares the data points it needs, and a **resolver** satisfies each one from the cheapest source already available, force-fetching only the genuinely-missing points and only when the level permits.

Guiding principle: **best-effort — use as much already-available data as possible to avoid upstream calls.**

### 5.1 Data points and their cheapest providers

Each data point can be produced by several methods at different cost. The resolver fetches the *cheapest method that yields the missing point* — and a single canonical fetch typically yields several points at once.

| Data point | Cheapest provider | Notes |
|---|---|---|
| `blockHash`, `parentHash`, `transactionsRoot`, `receiptsRoot`, block `logsBloom`, full header | `eth_getBlockByNumber(false)` | header-only; enough for block-hash recompute |
| `txCount` | `eth_getBlockTransactionCountByNumber` | a single number — cheapest of all |
| `txHashes` + transaction index | `eth_getBlockByNumber(false)` | tx hashes are in the header response |
| `logCount`, `logIndex` ordering, per-receipt bloom | `eth_getBlockReceipts` | the only source for log-level facts |
| full transaction fields (contract creation, gas) | `eth_getBlockByNumber(true)` / `eth_getBlockReceipts` | heaviest |

Block-relative fields (`logIndex`, `transactionIndex`) are positions in the block's global ordering and have no meaning in isolation — they can only be validated against the block aggregate, which is why those checks require `getBlockReceipts`/`getBlockByNumber`, never a second copy of the narrow method.

### 5.2 The resolve algorithm

For a given response and the set of checks enabled at the configured level:

1. Collect the union of data points required by the enabled checks.
2. Satisfy each point from warm sources in cost order: the in-flight response itself → previously-observed metadata / cache → not available.
3. If nothing is missing → run the checks; **zero fetch**. (This can happen at `corroborated` *and* `authoritative`.)
4. If points are missing:
   - at `corroborated`: skip the dependent checks (or soft-flag — see §8); fetch nothing.
   - at `authoritative`: compute the **minimal canonical fetch that covers the most missing points** (usually one), fetch it (§7), then continue.
5. Mine the fetched/observed response for **all** data points it carries and feed them back into the store, so the next check on that block — and every other request touching it — is free.

Two consequences:

- `corroborated` and `authoritative` are the **same resolver** with a single flag: *may I pay for a miss?* Both maximize free hits.
- Because every observed block-bearing response feeds the store, **hot blocks self-satisfy** and force-fetch cost concentrates on cold/rare blocks — the cheapest possible cost shape.

## 6. Integrity check catalog

Checks fall into four strength tiers. Higher tiers give stronger guarantees; lower tiers are cheaper and catch a wider class of *malformed* (as opposed to maliciously-consistent) data. Each check is either an existing validator or a thin addition, and each maps to the data points it consumes (§5) and therefore to a level.

A foundational observation drives the level mapping: **the canonical block-aggregate responses carry their own cryptographic commitments**. Most strong checks are therefore *intrinsic* when validating a `getBlockByNumber`/`getBlockReceipts` response (the response proves itself), and become *authoritative* only when validating a narrow method (a single receipt/transaction/log) that lacks those commitments and must be checked against a fetched aggregate.

Where an existing directive already implements a check, the module **enables and feeds it** rather than re-implementing — it only adds the data-gathering and the handful of thin checks marked "new".

### 6.1 Tier A — Cryptographic commitment recomputation (strongest)

Recompute a value and compare it to the chain's own commitment carried in the same data. A match is cryptographic proof the data is authentic (down to the header itself).

| Check | Recompute | Compare to | Inputs | Level: aggregate / narrow |
|---|---|---|---|---|
| Block hash | `RLP(canonical header fields)` → keccak256 | `block.hash` | full header | intrinsic / authoritative |
| Transactions root | trie over `RLP(index) → typed-tx RLP` | header `transactionsRoot` | full tx bodies | intrinsic / authoritative |
| Receipts root | trie over `RLP(index) → typed-receipt RLP` | header `receiptsRoot` | receipts + `receiptsRoot` | intrinsic\* / authoritative |
| Logs bloom | 2048-bit bloom from each log's address + topics | receipt/header bloom — equality, or **superset** (every set bit present, node may add bits) | logs + bloom | intrinsic |

\* receipts-root needs the header's `receiptsRoot`; intrinsic when the aggregate under validation includes it, otherwise one corroborated data point.

Header fields RLP'd in order, appending upgrade-added fields only when active for the block (§6.5): parent hash, uncles hash, miner, state root, transactions root, receipts root, logs bloom, difficulty, number, gas limit, gas used, timestamp, extra data, mix hash, nonce; then base-fee-per-gas, withdrawals root, blob-gas-used + excess-blob-gas, parent-beacon-block-root, requests hash.

### 6.2 Tier B — Structural & cross-reference consistency

No cryptographic commitment, but strong invariants over the block's shape. These catch mixed-block/caching bugs and ordering corruption (the class that motivated the first intrinsic check).

- **Log-index contiguity** — `logIndex` over all logs in the block is exactly `0..N-1`, strictly increasing, no gaps.
- **Transaction-index contiguity** — `transactionIndex` over all transactions/receipts is exactly `0..N-1`.
- **Log metadata cross-reference** — each log's `blockHash`/`blockNumber` equals the block header, and its `transactionHash`/`transactionIndex` equals its parent receipt. (Catches a provider mixing data from different blocks.)
- **Receipt ↔ transaction correspondence** — receipts count equals transactions count; `receipt[i].transactionHash == transactions[i].hash`; `transactionIndex == i`.
- **Single block identity** — all receipts/logs share one `blockHash`, equal to the requested block's.
- **Contract-creation consistency** — `contractAddress` present iff the transaction has no `to`.
- **Uniqueness** — transaction hashes unique within the block.

### 6.3 Tier C — Per-item cryptographic authenticity

Verify a single transaction without the whole block; intrinsic to any method returning full transaction bodies.

- **Sender recovery** — recover the signer from `(r, s, v|yParity)` over the type-specific signing payload (accounting for replay-protection encodings, typed-transaction signing rules, and alternative/account-abstraction signature schemes) and require it to equal the announced `from`.
- **Transaction-hash verification** — `keccak256(canonical typed-tx RLP) == tx.hash`. For transactions whose body cannot be reconstructed from JSON (some system/synthetic transactions), fetch the canonical raw bytes via a raw-transaction method and verify `keccak256(raw) == hash`.

### 6.4 Tier D — Shape & sanity (cheap, intrinsic)

- **Index magnitude** — no `logIndex`/`transactionIndex` beyond a physically-possible bound (the already-shipped check).
- **Field shapes** — address = 20 bytes, topic/hash = 32 bytes, bounded topic count, well-formed hex quantities.
- **Bloom emptiness** — a zero bloom iff zero logs.
- **Hardfork-gated field presence** — see §6.5.

### 6.5 Encoding correctness & verification principles (cross-cutting)

The Tier-A/C recomputations are only correct if encoding rules are exact. Several principles keep them honest and are mandatory for any implementation:

- **Typed envelopes** — EIP-2718 type-prefixed RLP for legacy, access-list, dynamic-fee, blob, and authorization transactions, plus chain-family-specific types (rollup system/deposit/retryable transactions; sidechain synthetic state-sync transactions), each with its own field layout and trie-inclusion rule.
- **Receipt encoding variants** — some providers encode per-transaction gas where the standard uses cumulative gas; some chains append hardfork-gated trailing receipt fields. The recompute must follow the chain's actual rule.
- **Synthetic/system transactions** — some chains inject a system transaction at a fixed index, or append a synthetic transaction that is *excluded from* (older rule) or *included in* (newer rule) the tries. Handling must follow the chain's rule and may require out-of-band raw bytes (verified by hash) that standard block JSON cannot provide — this is the only check class that may need an auxiliary fetch beyond the block aggregate.
- **Fail-closed on fork state** — hardfork-dependent field presence is gated on an authoritative activation cutoff (block number/timestamp), **never inferred from the response**; otherwise a provider that strips a field could trick the verifier into accepting a wrong-shaped object.
- **Config-time validation** — an enabled check that would be a silent no-op for the configured chain (e.g. a chain-specific field check on a chain that lacks the field) is rejected at startup, so an operator can never believe integrity is enforced when it is not.
- **Normalization** — all hex comparisons are case-insensitive and zero-padding tolerant.
- **Independent toggles** — every check is independently switchable; a level (§4) is a named preset over these toggles plus the resolver/fetch settings.

### 6.6 Level mapping summary

| Tier | On aggregate methods | On narrow methods |
|---|---|---|
| A — commitments | intrinsic | authoritative (fetch the aggregate) |
| B — structural | intrinsic (whole-block) / corroborated (single item vs block) | corroborated → authoritative |
| C — per-item authenticity | intrinsic (if full tx body present) | intrinsic (if full tx body present) |
| D — shape / sanity | intrinsic | intrinsic |

This is why `intrinsic` is far more powerful than "a few cheap checks": for the aggregate methods it already includes full cryptographic self-verification, with no extra upstream calls.

## 7. Authoritative fetch

When a data point is missing and the level is `authoritative`, the module fetches the **canonical block aggregate** that provides it (the cheapest provider from §5.1), keyed by the block the narrow response claims to belong to — never a second copy of the narrow method.

The fetch is an **internal `network.Forward`** call. This is deliberate and load-bearing:

- It inherits the network's full failsafe stack — cache, retry, hedge, and **consensus if configured**. The integrity of the ground truth is therefore *exactly* what the network is already configured to provide. A network with consensus enabled yields a consensus-verified oracle for free; a network without it yields a best-effort one. There is no separate "trusted source" to build or rate.
- It is **cache-backed**. A finalized block is immutable, so it is fetched once and every later check on that block is a cache hit. The unit of ground truth and the unit of caching are the same.
- It treats whatever is behind the network as a black box. Any specific upstream or cache connector is just part of the pool the network already routes to.

**Recursion guard.** The internal fetch carries an internal/skip-integrity marker so it never triggers a force-fetch on itself (it still receives consensus and intrinsic checks).

**Coalescing.** Concurrent checks needing the same block share a single in-flight fetch (single-flight), so a burst of narrow requests for one block costs one canonical fetch.

## 8. Trust and provenance

Corroboration is only meaningful if a data point came from an independent-enough source; a fact fed by the very same upstream/request being validated is partly circular. To keep this honest without building a trust engine, every stored data point carries a **provenance** tag:

- `intrinsic` — derived from the response under validation (self-consistency only).
- `observed-single-source` — seen from one upstream, not failsafe-verified.
- `network-verified` — obtained through `network.Forward` with the network's failsafe applied (e.g. consensus).

A **hard-fail** check (returns a content-validation error) requires a source whose provenance is trustworthy enough for the configured level; otherwise it **soft-flags** (metric + log, no rejection). This is a single field, not a voting system. The circular case is further mitigated because intrinsic checks catch the unanimous-corruption case regardless of provenance, and stronger independence is available by enabling consensus on the network.

## 9. Configuration

One knob per project, overridable per network, mirroring the existing directive-defaults precedent. The `level` expands internally into the appropriate directive defaults plus resolver/fetch settings — it is sugar over the existing directive pipeline.

```yaml
# Project-wide default for all networks
integrity:
  level: corroborated          # off | intrinsic | corroborated | authoritative

networks:
  - architecture: evm
    evm: { chainId: 1 }

    # Per-network override
    integrity:
      level: authoritative
      forceFetch:
        enabled: true           # gate for level=authoritative fetches
        onlyFinalized: true      # do not chase the reorg-prone tip
        maxPerSecond: 50         # the cost lever (token bucket)
        maxConcurrent: 8
      # Optional: override individual checks on top of the level
      checks:
        validateLogsBloomMatch: off
        receiptsCountExact: on
```

| Field | Type | Default | Description |
|---|---|---|---|
| `level` | enum | `corroborated` | `off` / `intrinsic` / `corroborated` / `authoritative`. The only field most users set. |
| `forceFetch.enabled` | bool | `true` at `authoritative` | Master gate for on-demand canonical fetches. |
| `forceFetch.onlyFinalized` | bool | `true` | Only force-fetch for finalized blocks (immutable; no reorg ambiguity). |
| `forceFetch.maxPerSecond` | int | conservative | Token-bucket rate cap on canonical fetches; the primary cost control. |
| `forceFetch.maxConcurrent` | int | small | Concurrency cap on in-flight canonical fetches. |
| `checks.<name>` | bool | per-level | Per-check override, additive/subtractive on top of the level. Power users only. |

Rules that keep it simple:

- `level` alone is a complete configuration; every other field has a derived default.
- The `level → {directive defaults, resolver, fetch}` mapping lives in one place (defaults), and is auditable.
- Per-check overrides compose on top of the level; they never need to be set for the common case.
- `integrity` is available at the project level (applies to all networks) and the network level (overrides). Per-network is the finest granularity.

### 9.1 Level → behavior mapping

| Level | Intrinsic checks | Corroboration (warm data) | Force-fetch on miss |
|---|---|---|---|
| `off` | — | — | — |
| `intrinsic` | yes | — | — |
| `corroborated` | yes | yes | no |
| `authoritative` | yes | yes | yes (budgeted) |

## 10. Architecture & integration

A per-network **integrity manager** owns the resolver, the provenance-tagged metadata it accumulates, and the budget/rate limiter. It plugs into existing seams:

- **Passive feeding** (free, all levels ≥ `corroborated`): a single observer on the post-forward path records data points from any block-bearing response already flowing through the network. This is the "use already-seen data" core; it costs nothing because the traffic is already paid for.
- **Check execution**: in the upstream post-forward hook, before the existing method validators run, the manager resolves required data points (fetching per §7 when the level permits) and populates the corresponding `ValidationExpected*` / `GroundTruth*` directives. The existing validators then run unchanged and emit the standard content-validation error on failure.
- **Force-fetch**: a plain internal `network.Forward` (§7).
- **Storage**: because `network.Forward` is cache-backed, the cache already serves as the block-data store. A small dedicated metadata store is an optional optimization for data points derived from responses that are not cached as block data, and may be deferred until profiling justifies it.

No new validation logic, no new agreement logic, no new trusted-source logic.

## 11. Correctness & edge cases

- **Reorgs**: key block facts by hash, not number; near-tip mismatches soft-flag rather than hard-fail; `onlyFinalized` confines hard-fail force-fetches to immutable data.
- **Filtered `eth_getLogs`**: a filtered result is a *lower bound* on a block's log count — usable for "at least" checks, never for exact counts or completeness unless unfiltered.
- **Chain-specific semantics**: index/log conventions vary across chains and L2s; strict checks must be per-chain overridable so a legitimate chain quirk is not flagged.
- **Recursion**: internal canonical fetches must be marked so they do not re-enter the integrity manager (§7).
- **Latency**: a force-fetch adds a round trip to the user path; default `onlyFinalized`, coalescing, and cache amortization keep this rare. A future "verify-after-serve" mode (serve, then validate out-of-band and cordon a lying upstream) can remove it entirely for latency-sensitive projects — out of scope for the initial implementation.
- **Provenance/circularity**: see §8.

## 12. Cost model

- Levels `off`/`intrinsic`/`corroborated` add **no upstream calls** — only CPU for parsing/validation and memory for the metadata.
- `authoritative` adds at most one canonical fetch per *cold* block, shared across all narrow requests for that block and cached thereafter. Cost is **per-block, not per-request**, bounded by `forceFetch.maxPerSecond`. When the budget is exceeded the request degrades to `corroborated` behavior (skip the missing-data check, emit a throttled metric) and never blocks the user path.

## 13. Observability

- A metric family for checks run / passed / failed, by network, method, level, and check name.
- A metric for force-fetches issued, cache-hit ratio, and throttled (budget-exceeded) events.
- Failures surface as the existing content-validation error (already counted and traced), so failover behavior is unchanged and visible through current dashboards.

## 14. Open questions

1. Default level for new projects (proposed: `corroborated` — free and safe).
2. Whether to ship the dedicated metadata store in the first iteration or rely solely on the cache (§10).
3. Hard-fail vs verify-after-serve as the `authoritative` default (correctness vs latency).
4. Per-chain default check profiles (which strict checks are safe to enable by default per architecture/chain).
