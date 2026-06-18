# Data Integrity — Implementation Plan

**Status**: Draft — for review
**Last revised**: 2026-06-18

Companion to [feature.md](./feature.md). Phased so each step ships value independently and the cheap/safe tiers land first.

---

## Phase 0 — Already shipped

- **Level 1 seed**: the intrinsic `logIndex`/`transactionIndex` magnitude check in the EVM upstream post-forward hook, returning the standard content-validation error. Establishes the pattern: validate at the post-forward layer, reject by converting to an error, let existing failover route around it.

Acceptance: corrupt index → content-validation error → failover; clean responses untouched. (Done.)

## Phase 1 — Levels + corroboration from already-seen data (no new upstream cost)

The first real module. Everything here is free at runtime.

1. **Config surface**: an `integrity` block on the project-wide network defaults and per-network config, with `level` as the only required field. Wire `level → directive defaults` mapping in one auditable place.
2. **Data-point model + resolver**: the `requiredDataPoints` declaration per check and the cost-ordered resolve function (feature.md §5). Phase 1 resolves from warm sources only (in-flight response, cache, observed metadata) — no force-fetch.
3. **Passive feeder**: a single observer on the post-forward path that records data points (`blockHash`, `txCount`, …) with provenance tags from any block-bearing response already flowing through.
4. **Corroboration checks**: block-hash agreement and receipt/tx-count agreement, implemented by auto-filling the existing `ValidationExpected*` directives from resolved data points. Soft-flag when the only available source is `observed-single-source`; hard-fail when `network-verified`.
5. **Complete the intrinsic tier** for aggregate methods (feature.md §6): enable the existing self-verifying validators (transactions-root, bloom, field shapes) via the level, and add the thin missing ones — block-hash recompute, receipts-root recompute, log/transaction-index contiguity, log-metadata cross-reference. All are pure functions, unit-testable in isolation, with no upstream cost.
6. **Metrics**: checks run/passed/failed by network/method/level/check.

Acceptance: with `level: corroborated`, a response whose block hash or tx count contradicts already-seen, network-verified data is rejected; nothing extra is fetched; hot blocks self-satisfy.

## Phase 2 — Authoritative tier (force-fetch + cross-validation)

1. **Force-fetch**: extend the resolver to issue the minimal canonical `network.Forward` fetch for missing data points when `level: authoritative` and `forceFetch.enabled` (feature.md §7), with the recursion-skip marker.
2. **Budget/coalescing**: token-bucket `maxPerSecond`, `maxConcurrent`, single-flight per block, `onlyFinalized` default; graceful degradation to `corroborated` on budget exhaustion.
3. **Cross-validation checks**: receipt ↔ block (transaction index, logIndex range), exact receipt count, contract-creation consistency, log completeness — by auto-filling `GroundTruth*` / `ReceiptsCountExact` from the force-fetched canonical block.
4. **Per-item authenticity + encoding correctness**: transaction-hash and sender recovery (feature.md §6.3), with the typed-envelope, receipt-variant, synthetic-transaction, and fail-closed fork-activation rules (§6.5) needed to make recomputation correct across chain families.
5. **Metrics**: force-fetches issued, cache-hit ratio, throttled events.

Acceptance: with `level: authoritative` on a cold block, a single canonical fetch is issued, shared and cached; a receipt whose logIndex range or count contradicts the block is rejected; budget caps hold.

## Phase 3 — Optional enhancements (only if asked)

- Dedicated bounded metadata store for facts derived from non-cached responses.
- Verify-after-serve mode for latency-sensitive projects (serve, validate out-of-band, cordon a lying upstream).
- Per-chain default check profiles.
- Proactive seeding (e.g. fetching finalized-block aggregates ahead of demand).

## Risks / watch-items

- **Provenance discipline**: never hard-fail against `observed-single-source` data, or a single bad upstream poisons the check. Enforced by the provenance tag (feature.md §8).
- **Chain quirks**: ship strict checks behind per-chain overrides to avoid false positives on legitimate chain-specific conventions.
- **Encoding correctness**: the cryptographic recomputations (Tiers A/C) are only valid if typed-envelope, receipt-variant, synthetic-transaction, and hardfork-activation rules are exact per chain. Fork state is gated on an authoritative cutoff (never inferred from the response), and any check that would be a silent no-op for the configured chain is rejected at startup.
- **Latency**: keep `authoritative` force-fetches confined to finalized blocks and coalesced; revisit verify-after-serve if user-path latency regresses.
- **Scope creep**: the module is data + orchestration only. Any temptation to add agreement/quorum or a trusted-source concept is out of scope — those are consensus and network config respectively.
