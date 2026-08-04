# Data Integrity — Specification

**Status**: As-built (rewritten to match the implementation)
**Last revised**: 2026-07-03

---

## 1. Purpose

The **data-integrity module** validates that the data an upstream returns is internally consistent and consistent with what the chain actually contains, and rejects responses that provably cannot be correct. A rejected response becomes a standard content-validation error (`ErrEndpointContentValidation`), so the existing retry/failover machinery routes around the offending upstream and — in almost every observed case — serves the client a correct response from another one.

The module is a self-contained check engine (`architecture/evm/integrity/`) with its own decoded view of responses, plus a per-network reorg-aware state store (the **ChainView**, `architecture/evm/integrity_chainview.go`) and a canonical-fetch **resolver**. It is invoked from the EVM upstream post-forward hook on every applicable response. An earlier draft of this spec described the module as an orchestration layer over the then-existing per-request validation directives; that design was abandoned — the directives were deleted and the module owns its checks end-to-end.

### What it catches that consensus cannot

- Corruption the upstreams *agree* on (shared client/indexer bugs) — consensus is majority-blind.
- Corruption on requests with no fan-out at all — intrinsic checks are free and always-on per response.
- Field-level provable inconsistencies (a block whose header commits to transactions the body doesn't carry; a receipt whose logs don't match the canonical block's) that byte-comparison voting can't see, and that can even *win* a size-preferring consensus dispute.

### Non-goals

- **Not** a quorum mechanism (consensus's job) and **not** a data-rewriting layer: the module rejects, it never edits.
- **Not** a new trusted source: canonical fetches go through the normal network path and the same upstream pool (group-scoped, see §6).

## 2. Check engine

Checks self-register (`register()` in `check.go`) into a per-method registry. `Validate` (engine.go) runs every enabled, applicable check over a **single decode** of the response (`Decoded`, decode.go — lazy per-shape accessors for header/transactions/receipts/logs plus the originating request's params).

Each check declares:

- **ID** — stable name used in config, metrics, and the catalog registered into config validation (`common.RegisterIntegrityCheckID`).
- **Family** — descriptive grouping (commitment / authenticity / structural / shape / continuity).
- **Class** — `Deterministic` (provable from committed data; always rejects on violation) or `ReorgSensitive` (may be a transient reorg artifact; verdict resolved per finality, §4).
- **AllowEmptyish** — opt-in to see emptyish (`[]`/null) responses, which the engine otherwise short-circuits (only `getLogsCompleteness` opts in: an empty result is the everything-dropped corruption shape).

### Outcomes

Every evaluation records one outcome (metric `erpc_integrity_check_total{check,outcome}`):

| outcome | meaning |
|---|---|
| `pass` | an actual verification ran and found no violation — **earned**, never a folded no-op |
| `skip` | the check could not evaluate (missing wiring, cold cache, data not fully modeled) — `Skipped` sentinel |
| `reject` | violation; response rejected (failover takes over) |
| `soft_flag` | reorg-sensitive violation recorded but served |
| `reconfirmed` | a pin-anchored mismatch cleared after the stale pin was re-confirmed (a reorg, not corruption) |
| `off` | disabled for this finality or check |

The pass/skip split is what turns "no rejects" into the positive statement *"N verified against pins/canonical, 0 mismatches."*

### Chain-safety invariant (do not break)

Every recompute is chain-safe: its known-field sets are **derived from go-ethereum at runtime** (version-proof), and anything a check cannot fully model — custom L2 header/receipt fields, synthetic/system transactions, hashes-only responses — **skips, never rejects**. An integrity module must never reject valid data.

## 3. Check catalog and levels

The front-door knob is `integrity.level`; each level enables its row plus all lower rows (`levels.go`, membership enforced by test):

- **intrinsic** (free, zero upstream cost — 22 checks): shape/sanity (schemaConformance, headerFieldShapes, logFieldShapes, logMetadata, indexMagnitude, bloomEmptiness), structural cross-reference (sameBlockHash, txHashUniqueness, txFieldUniqueness, transactionIndexConsistency, logIndexContiguity, txBlockInfo, transactionsRootConsistency, bloomMatch), cryptographic recompute from response-local data (blockHashRecompute, transactionsRootRecompute, senderRecovery), request-identity (getLogsFilterSanity — response logs must match the request's filter, exact go-ethereum filterLogs semantics; blockByHashIdentity / blockByNumberIdentity / txByHashIdentity / receiptIdentity — the response must be for the *requested* entity, closing the mixed-up-node gap; blockByNumberIdentity compares heights NUMERICALLY and skips the tag forms, whose answer is the served-tip layer's question). Identity is not implied by recompute: `blockHashRecompute` proves the returned header hashes to the hash it claims (real, self-consistent), which a *different* valid block also satisfies — only `blockByHashIdentity` ties the answer to the question, and on `eth_getBlockByHash` it is the sole check that does so now that continuity is by-number only.
- **corroborated** (uses remembered state, still no forced fetches — 4 checks): parentHashLinkage, hashStability (continuity vs the ChainView pin), getLogsCompleteness (per-block hash-anchored diff vs *cached* canonical receipts: missing/extra/duplicated/corrupted logs, plus finalized absent-block detection), txPinConsistency (a mined tx's claimed coordinates vs the pin). The continuity pair applies to `eth_getBlockByNumber` **only**: a by-number lookup asks "what is the chain at height N", which is exactly what the pin records, whereas a by-hash lookup asks for one named block whose canonicality was never the question — orphaned-but-real blocks are retrieved by hash precisely to unwind reorgs, and an orphan hash is unobtainable on the canonical fork, so continuity there rejects explicitly-requested data that no failover can replace. Symmetrically, a by-hash response feeds the header cache but never the pin (§5), so an orphan cannot be adopted as canonical.
- **authoritative** (adds canonical force-fetches — 2 checks): receiptVsBlock (a receipt corroborated against the block's canonical receipts by hash), receiptsRootRecompute (recompute a receipts root against the canonical header's commitment).

## 4. Verdicts, reorg policy, and corroborate-before-verdict

`invalidBehavior: {finalized, unfinalized}` maps finality → `reject | soft-flag | off` for **reorg-sensitive** checks (deterministic checks always reject; per-check `onFailure` overrides either). The safe default is `{finalized: reject, unfinalized: soft-flag}`.

**Corroborate-before-verdict (the reorg self-heal):** a reorg-sensitive violation anchored to a *cached* pin (`Violation.DisputedPin`) may only mean the pin is stale after a routine reorg. Before applying the verdict, the engine re-confirms the disputed pin via `PinReconfirmer.ReconfirmPin` — a singleflighted, cooldown-bounded canonical fetch by number that adopts whatever the network now serves — and re-runs the check. A reorg clears (`reconfirmed`); a mismatch that survives the fresh pin is genuine. Without this, strict `unfinalized: reject` self-blocks after every reorg: the stale pin rejects every honest new-fork response, and since the pin adopts only from passing responses it never recovers (observed live as episodic all-upstream reject bursts and client failures).

`ReconfirmPin` reports one of three states, and only `PinFresh` licenses acting on the result. Inside the fetch-rate cooldown it answers `PinRateLimited` — the cached pin, carrying no new evidence — and the engine then degrades a would-be reject to a soft-flag (recorded and served) rather than rejecting on unverified state; `PinUnverifiable` (fetch failed) leaves the strict class verdict alone. The distinction is load-bearing: while the cooldown reported its cached answer as a confirmation, a single non-canonical pin hard-rejected 24 honest responses from three independent upstreams inside ~700ms, erroring 8 client requests with nothing to fail over to (mainnet 25589196, 2026-07-22 — canonical later confirmed every rejected response was correct).

## 5. ChainView (per-network, per-group state)

A bounded, reorg-aware store fed from validated traffic: committed `number→hash` pins, a content-addressed header cache, and a by-hash canonical receipts cache; window-bounded by `integrity.reorgWindow` (default 32). Block responses populate pin+header after passing validation; narrow responses (receipts/tx) pin only **finalized** blocks (tip-thrash safety). Resolve-on-miss is singleflighted and multiplexed with concurrent user requests for the same block. A changed hash at a pinned number is a reorg: adopt and roll back stale descendants.

**Fetch-anchor invariant (hard-learned):** a canonical fetch must **prove it returned the requested block** before being trusted, observed, or cached — every returned receipt must claim the requested blockHash; a by-hash header fetch must return that hash; a numeric by-number fetch must return that number. A node still on a losing fork answering a by-hash receipts request with the *other fork's* receipts poisoned the corroboration and cache and hard-rejected every honest receipt for the real block. Mismatched or empty answers are "canonical unavailable" (skip) — never evidence. Empty receipt sets are never cached.

## 6. Group scoping

Integrity state and corroboration are scoped to the node **group** the request was pinned to (its use-upstream selector), reusing erpc's served-tip grouping. Chains that run heterogeneous node families (e.g. nodes that include protocol/system transactions in indexes vs nodes that don't) produce cross-family false mismatches otherwise. Unpinned traffic uses the network-wide view; a per-network `directiveDefaults.useUpstream` gives unpinned traffic a home group.

## 7. Configuration

```yaml
integrity:                     # project- and/or network-level (network overrides, maps union)
  level: authoritative         # off | intrinsic | corroborated | authoritative
  invalidBehavior: {finalized: reject, unfinalized: soft-flag}
  reorgWindow: 32
  checks:
    someCheckId: {enabled: false, onFailure: soft-flag, params: {...}}
  misbehaviorsDestination: {type: s3, path: "s3://bucket/prefix/"}   # JSONL catch archive
  headerMode: off              # off | profiles | full (X-ERPC-Integrity per-request knob)
  profiles: {strict: {level: authoritative}}
  # budget: reserved in the schema; enforcement was removed (a throttled fetch
  # never warms the cache → feedback loop starves corroboration). A future
  # budget must be cache-warming-aware.
```

- **Validation at load** (`common/validation.go`): unknown level / check id / behavior string / headerMode fails boot — every one of these was previously a silent no-op (a typo'd level enabled *zero* checks).
- **Chain profiles** (`chain_profiles.go`): protocol-quirk disables ship as defaults for chains whose protocol commits synthetic/system transactions into header roots but omits them from RPC lists (the whole root family is unreproducible there). Applied between the level preset and operator overrides — an explicit `enabled: true` wins. Diagnostic rule for finding new ones: **a check rejecting across ALL upstreams of one chain is a chain quirk; scattered per-upstream rejects are real catches.**

## 8. Integration effects on the request path

- A reject becomes `ErrEndpointContentValidation` → retry/failover; consensus excludes such errors from `preferLargerResponses`, so a corrupt-but-larger response can no longer dispute an honest majority.
- **Last-valid-response safety:** rejected responses are marked (`MarkIntegrityRejected`) and refused by `SetLastValidResponse`; reject paths clear the LVR **identity-checked** (`ClearLastValidResponseIf`) so a concurrent valid hedge response is never dropped and a corrupt hedge can never be re-served.
- **Health scoring:** a *deterministic* reject records `RecordUpstreamMisbehavior`, so routing learns to avoid chronically-corrupt nodes (content validation runs after per-attempt outcome classification and was previously invisible to scoring). Reorg-sensitive rejects don't score.
- Internal requests (the corroboration fetches themselves) skip the engine — no recursion.

## 9. Observability

- `erpc_integrity_check_total{check,outcome}` — every evaluation (sum = attempts; see §2 outcomes).
- `erpc_integrity_violation_total{check,verdict,finality}` — rejects/soft-flags only, with the target block's finality (genuine finalized/deterministic catches separate from reorg-prone unfinalized ones).
- `erpc_integrity_saved_total` / `erpc_integrity_failed_total` — request-level outcome after a catch: failover served good data vs the request erred (the check id says why).
- `erpc_integrity_aux_request_total{group,kind,method,finality,outcome}` — canonical force-fetches (dedup keeps them rare; `error` includes anchor-mismatch refusals).
- `erpc_integrity_overhead_seconds` — added latency, config-driven buckets.
- A durable WARN log per hard reject and per soft-flag with the verbatim expected-vs-actual reason (100%-detailed traces proved un-queryable at volume; the log is the greppable record).
- `integrity.misbehaviorsDestination` — the JSONL archive (file/S3, shared machinery with the consensus policy's destination): timestamp, project/network/upstream/vendor, method, check, class, verdict, finality, reason, offending body (capped).
- Detailed tracing adds per-check outcome attributes and the mismatch values to the `Integrity.Validate` span.

## 10. Production findings that shaped the design

1. **The value concentrates in the free tiers.** All genuine catches to date came from intrinsic checks — at scale from `transactionsRootConsistency` catching an internal indexer-backing node serving valid-header/empty-transaction-list blocks (~158k catches, ~154k requests saved via failover, and the detection led to the routing/bounds fix on the primary cluster); historically from the index-magnitude class (a vendor's int32 logIndex underflow that had won a consensus dispute). The authoritative force-fetch tier has so far caught only its own false-positive classes — each now fixed structurally (group scoping, by-hash anchoring, tip-lag skip, pin reconfirm, fetch-anchor enforcement).
2. **Every false-positive class had the same signature:** rejects spread across all upstreams of one chain. That diagnostic is now a documented operating rule, the source of chain profiles, and since 2026-08-04 it is detected automatically — see `RecordExhaustion` and `erpc_integrity_protocol_suspect_total` in §9.
3. **False-positive risk is concentrated at check *introduction*, not spread over time.** A 7-day shadow window measured ~984M passing evaluations against ~755 violations, of which ~702 were false positives — but the hour-resolution timeline shows each class fired in a single 1–3 hour burst immediately after a new check was deployed, was diagnosed, and never recurred (`baseFeeDerivation` on polygon/bsc, 3h, none in 49h; `transactionsRootConsistency` on hyperevm, 1h, none in 46h; `traceBlockGasReconciliation` on polygon, 1h, none in 24h). The aggregate must therefore **not** be quoted as a steady-state precision: after each fix the rate is zero. What the number really measures is that a check meeting an unfamiliar chain for the first time is far more likely to be protocol-invalid there than to be catching corruption.
4. **The harm from a false positive is asymmetric, which is why it outweighs a missed catch.** A check that rejects on a *single* upstream is nearly harmless — failover absorbs it and the request is still served correctly. A check that rejects across *every* upstream **defeats failover**, so each rejection becomes a client-facing error 1:1. In that same 7-day window all-upstream false positives produced **95 client errors** while every genuine catch produced **27 saves** — a net-negative week caused entirely by checks that were protocol-invalid on one chain.
5. **Therefore: a check meeting a chain for the first time should be soft-flag there until it has a clean window, and promoted to reject only after.** The per-chain profile is the mechanism; the discipline is to add a new check with reject enabled only on chains whose protocol behaviour was actually measured (see the fee-model entries in `chain_architecture.go`, which record *why* each chain is or is not derivable). This would have converted all ~702 false positives and all 95 client errors into zero-harm observations.
6. **Strict-at-the-tip needs the reconfirm.** `unfinalized: reject` without corroborate-before-verdict is self-defeating (see §4).
7. **Bounds fix leaks; checks make leaks harmless.** After the routing bound shipped for the corrupt internal node, occasional blocks still slipped through the availability edge — the intrinsic check backstops each one.

## 11. Known limitations / future work

- **Consensus-for-aux / per-method retirement:** today the canonical fetch inherits whatever failsafe the method+finality happens to have — not deliberate. Target: a request-kind failsafe axis (user|internal) so *aux* fetches get consensus (quorum-verified ground truth, ~once per block) while user data methods rely on integrity; consensus remains permanently for state/value methods (eth_call, getBalance, gas) which have no canonical to check against.
- **Budget:** the config field is reserved but unenforced (see §7).
- **eth_getTransactionByHash checks are traffic-starved** in shadow testing (the mirror carries none); they are unit-verified only.
- ChainView group views are never evicted across a network's lifetime (bounded by real group count; a cap like served-tip partitions would be cleaner).
- Legacy `DirectiveDefaults` validation flags migrate to `integrity.checks` via `migrateLegacyIntegrityChecks` (config-defaults layer only; no runtime compat path).

## Appendix — related specs

- `chain-view.md` — the ChainView store in detail.
- `getlogs-receipt-crosscheck.md` — the getLogs completeness design (implemented; the spec's budget-capped authoritative variant for cold ranges remains future work).
- `plan.md` — original build plan (historical).
