# SVM (Solana) Support in eRPC — Design v0.5

> Supersedes `DESIGN-MULTI-CHAIN-SOLANA.md` (v0). This revision reflects the
> rebase onto current upstream `main`, **multi-chain SVM** support
> (`svm:<chain>:<cluster>`), and four correctness fixes landed after review.
> It adds end-to-end workflow diagrams, an EVM-vs-Solana comparison, and an
> honest **Gaps & Limitations** section.

---

## 0. What changed since v0

| Area | v0 | v0.5 |
|------|----|------|
| Network identity | `svm:<cluster>` (Solana only) | `svm:<cluster>` **and** `svm:<chain>:<cluster>` (Fogo, Eclipse, forks) |
| Commitment injection | network pre-forward (after cache read) | **project pre-forward (before cache read)** — fixes a permanent cache miss |
| Finality of `getBlock`/`getTransaction`/`getBlocks`/`getSignaturesForAddress` | always finalized | **commitment-sensitive** (confirmed ⇒ unfinalized) |
| Commitment param injection | blind "append/mutate last map" | **per-method options-index, shape-aware** |
| `getInflationRate` | wrongly commitment-injected (→ `-32602`) | excluded (takes no params) |

All four fixes were verified against **live Solana mainnet-beta** and **Fogo
mainnet + testnet** (`https://mainnet.fogo.io`, `https://testnet.fogo.io`).

**Round-3 hardening (post-review):**

- **Write-path commitment normalization** — `sendTransaction` (→ `preflightCommitment`),
  `simulateTransaction` / `requestAirdrop` (→ `commitment`) now receive the network
  default at the correct method-specific field, mirroring the read path (§6).
- **`statePollerDebounce` now functional** — the config field was parsed but never
  wired to the poller; it now throttles the network fan-out (§5/poller). The ticker
  stays at the fixed one-slot cadence and the debounce is a *gate*.
- **Genesis validation is fail-closed for known clusters** — a `getGenesisHash`
  fetch failure (not just a mismatch) now fails bootstrap (§7).
- **Docs** — `svm.commitment` field comment corrected (no default; upstream's
  server-side default governs when unset).

---

## 1. Solana vs EVM — mental model for reviewers

Solana (SVM) and Ethereum (EVM) differ in the primitives eRPC's caching,
finality, and consensus machinery rely on:

| Concept | EVM | SVM (Solana) |
|---------|-----|--------------|
| Chain progress unit | **block number** (height) | **slot** (~400 ms; some slots are empty/skipped) |
| Block identity | `blockHash` (re-org → new hash) | `blockhash` of a slot; slots can be skipped |
| Finality model | depth-based (`latest`/`safe`/`finalized` ≈ N confirmations) | **commitment**: `processed` < `confirmed` < `finalized` |
| "Is this final?" | block ≤ finalized head | response commitment == `finalized` |
| Re-org exposure | re-org rewrites recent blocks | confirmed (non-rooted) slots can be dropped on fork switch |
| Chain id | numeric (`eth_chainId` = 1) | cluster genesis hash (immutable) |
| Tip freshness signal | `eth_blockNumber` | `getSlot` / `getLatestBlockhash` |
| Write idempotency | `eth_sendRawTransaction` (hash-keyed) | `sendTransaction` (must NOT be blindly retried across nodes) |
| Commitment in request | n/a | optional `{commitment}` options object, **position varies per method** |

Key consequence: **a "confirmed" Solana response is not final.** Treating it as
finalized (and caching it permanently) is unsound — hence fix #1.

---

## 2. Architecture: the `ArchitectureHandler` seam

eRPC is EVM-shaped at its core. SVM support is added behind a single polymorphic
interface (`common.ArchitectureHandler`) registered per architecture, so the
generic request pipeline never switches on architecture inline.

```
                        common.ArchitectureHandler (interface)
                                   │
              ┌────────────────────┴────────────────────┐
     architecture/evm.EvmArchitectureHandler   architecture/svm.SvmArchitectureHandler
       (wraps existing EVM hooks)                 (slot poller, finality, cache,
                                                   commitment, genesis, slot-lag)
```

Hook points (called by the generic pipeline):

| Hook | Layer | SVM responsibility |
|------|-------|--------------------|
| `HandleProjectPreForward` | project (pre-cache) | getGenesisHash short-circuit; **commitment injection** |
| `HandleNetworkPreForward` | network (post-selection) | `getSignaturesForAddress` slot-window guard |
| `HandleUpstreamPostForward` | upstream | `sendTransaction` no-retry; opportunistic slot tracking |
| `NewJsonRpcErrorExtractor` | upstream | SVM error-code normalization |

---

## 3. Request lifecycle — end to end

```mermaid
flowchart TD
    A[HTTP POST /main/svm/&lt;chain:cluster&gt;\nor body networkId svm:chain:cluster] --> B[parseUrlPath / body parse\nSplitN(networkId, \":\", 2)]
    B --> C[PreparedProject.doForward]
    C --> D{HandleProjectPreForward}
    D -- getGenesisHash known cluster --> D1[short-circuit:\nreturn hash from table] --> Z[response]
    D -- else --> E[inject default commitment\n(shape-aware, before cache)]
    E --> F[network.Forward]
    F --> G{cache GET\nkey = post-injection params}
    G -- hit --> Z
    G -- miss --> H[policy engine: ordered upstreams]
    H --> I{HandleNetworkPreForward}
    I -- getSignaturesForAddress out of slot window --> I1[reject -32602-class] --> Z
    I -- else --> J[per-upstream forward\n(failsafe: retry/hedge/cb/timeout)]
    J --> K[HandleUpstreamPreForward → upstream RPC → HandleUpstreamPostForward]
    K -- sendTransaction failed --> K1[strip retryability\n(no cross-node rebroadcast)]
    K --> L[GetFinality(req,resp)\ncommitment-aware]
    L --> M{cacheable?\nnot realtime}
    M -- yes --> N[cache SET\nkey = post-injection params]
    M -- no --> Z
    N --> Z
```

The critical ordering (fix #2): **commitment injection happens at step E, before
the cache GET at step G**, so GET and SET key on identical (post-injection)
params. In v0 injection happened between H and K, so GET keyed on pre-injection
params and SET on post-injection params — a permanent miss for any request that
relied on the network default commitment.

---

## 4. EVM vs SVM request workflow, side by side

```
EVM eth_getBalance                         SVM getBalance
──────────────────                         ──────────────
1. parse /main/evm/1                        1. parse /main/svm/mainnet-beta
                                               (or /main/svm/fogo:mainnet)
2. project pre-forward:                      2. project pre-forward:
   eth_chainId short-circuit                    getGenesisHash short-circuit
   (no commitment concept)                      + inject {commitment:"confirmed"}
                                                  at the method's options index
3. cache GET by (network, blockRef, params) 3. cache GET by (network, slotRef, params)
4. select upstreams (block-height aware)     4. select upstreams (slot aware)
5. forward; finality = block ≤ finalized     5. forward; finality = commitment level
6. cache SET if finalized/unfinalized        6. cache SET (realtime methods skipped)
```

Tip tracking parallels:

```
EVM evmStatePoller                          SVM svmStatePoller
   eth_blockNumber  → latest height            getSlot(processed)   → latest slot
   eth_getBlockByNumber(finalized)             getSlot(finalized)   → finalized slot
   → finalized height                          maxShredInsertSlot   → health/lag
```

---

## 5. Finality & caching semantics (corrected)

`architecture/svm/finality.go` resolves finality in priority order:

```
1. neverCacheMethods         → Realtime, AND hard-skipped by the cache layer
      sendTransaction, simulateTransaction, requestAirdrop,
      getLatestBlockhash, getFeeForMessage, getSignatureStatuses,
      getVoteAccounts, getLeaderSchedule, getEpochInfo, getSlotLeaders,
      getRecentPerformanceSamples, getRecentPrioritizationFees, …
2. alwaysFinalizedMethods    → Finalized (final by construction)
      getInflationReward (finalized epochs only), getBlockTime
3. effective commitment (resolveCommitment — the SAME predicate injection uses):
      finalized            → Finalized
      confirmed/processed   → Unfinalized
4. unknown (no effective commitment) → Unfinalized (re-org-aware; TTL bounds staleness)
```

**Never-cache is hard-enforced.** `Realtime` is still a *cacheable* finality at
the policy layer (EVM caches realtime reads with a short TTL + age guard), so to
honor the "never cached" guarantee the SVM cache `Get`/`Set` hard-skip any method
in `neverCacheMethods` before policy matching — an operator's stray
`finality: realtime` policy can no longer cache an effectful method.

**Finality tracks the *effective* commitment, not just the network default.**
Step 3 uses `resolveCommitment`, the single predicate that also drives injection.
So when injection legitimately skips a request (legacy `getBlock(slot,"base64")`
form, missing args, non-injectable method), no default reaches the upstream and
the response is classified Unfinalized — the network default is never
over-trusted. Because the predicate reads request shape + config (not mutation
state), this holds whether finality is computed before or after injection (it is
memoized on the first call, which happens pre-injection in `erpc/projects.go`).

`getBlock`, `getTransaction`, `getBlocks`, `getSignaturesForAddress` flow through
steps 3–4 (they were wrongly always-finalized in v0). A `confirmed` response is
**Unfinalized**, so it is cached re-org-aware (short TTL) rather than as a
permanent finalized record that a fork switch could falsify.

**Cache key**: `<networkId>:<slotRef>:<method>:<sha256(params)>`. `networkId`
carries the chain (`svm:fogo:mainnet` ≠ `svm:mainnet-beta`), so chains/clusters
never collide. `slotRef` is the request's `minContextSlot` or `*`.

---

## 6. Commitment injection — shape-aware (fix #3)

Solana's options/config object lives at a **method-specific param index**, and a
few methods take no params at all. Blind append/mutate corrupts requests. v0.5
uses a per-method `commitmentOptionsIndex` table:

```
optionsIndex 0  → getSlot, getEpochInfo, getSupply, getBlockHeight, …  ([] → [{commitment}])
optionsIndex 1  → getBalance, getAccountInfo, getBlock, getTransaction, …
optionsIndex 2  → getTokenAccountsByOwner/Delegate, getBlocksWithLimit
optionsAppend   → getBlocks (variable arity; trailing object)
(absent)        → getInflationRate, getVersion, getHealth, getGenesisHash, sendTransaction, …
```

Injection rules at the options index:

```
slot is an object with commitment   → honor caller (skip)
slot is an object without commitment → set commitment, invalidate CacheHash
slot is the next free position       → append {commitment}, invalidate CacheHash
slot occupied by a non-object        → SKIP (legacy getBlock(slot,"base64") form;
                                              never produce an invalid 3rd param)
required positional args missing      → SKIP (let upstream report it)
```

This fixes: `getInflationRate` (no params → `-32602`), the legacy
`getBlock(slot,"enc")` / `getTransaction(sig,"enc")` form, and commitment
landing on `getTokenAccountsByOwner`'s filter object instead of its config.

### Write-path commitment (round 3)

Write/effectful methods are excluded from the read table because they carry
commitment via their **own** config field — and the field name differs per
method, so a blanket `preflightCommitment` would be wrong:

```
sendTransaction     → options idx 1, field "preflightCommitment"
simulateTransaction → options idx 1, field "commitment"
requestAirdrop      → options idx 2, field "commitment"
```

A separate `networkPreForward_injectWriteCommitment` hook applies the network
default to these (same gating: caller value wins, skip on no-default / legacy
non-object slot / missing positional args). Verified live against Fogo: an
optionless `sendTransaction` egresses as `[...,{"preflightCommitment":"confirmed"}]`
while `simulateTransaction` gets `{"commitment":"confirmed"}`.

---

## 7. Multi-chain network identity (Fogo, Eclipse, forks)

```
SvmNetworkConfig{ Chain: "",     Cluster: "mainnet-beta" } → networkId "svm:mainnet-beta"   (back-compat)
SvmNetworkConfig{ Chain: "fogo", Cluster: "mainnet"      } → networkId "svm:fogo:mainnet"
```

`util.SvmNetworkId(chain, cluster)` is the single source of truth. Fix #4 made
all routing paths consistent with `IsValidNetworkId` by using
`SplitN(networkId, ":", 2)`:

- URL path `/main/svm/fogo:mainnet` (3rd segment carries the colon)
- request-body `"networkId":"svm:fogo:mainnet"`
- alias registration (eager + lazy)
- lazy network-config creation (parses `<chain>:<cluster>`)

### Genesis-hash validation flow

```mermaid
flowchart TD
    A[upstream Bootstrap] --> B{known (chain,cluster)\nin genesis table?}
    B -- yes --> C[fetch getGenesisHash once] --> D{fetch ok and\nmatches table?}
    D -- no --> E[fail bootstrap\n(mis-pointed OR unverifiable)]
    D -- yes --> OK[upstream ready]
    B -- no (e.g. Fogo) --> F{CheckGenesisHash: true?}
    F -- yes --> G[fetch getGenesisHash] --> H{fetch ok?}
    H -- no --> E
    H -- yes --> OK
    F -- no --> OK2[skip validation\n(operator opt-out)]
```

Validation is **fail-closed**: for a known cluster, both a hash mismatch and a
*fetch failure* fail bootstrap (we never register an upstream we could not
verify against the table). The only non-fatal path is an unknown cluster with
`checkGenesisHash` unset (private/local clusters with no published hash). Known
table currently: Solana `mainnet-beta` / `devnet` / `testnet`. Forks run via
`checkGenesisHash: true` (verified live for Fogo — genesis
`CDLtwKnaCoK157uaHQDj4fHu72AyD2519Cphmpiq6hvT`) or by adding their genesis hash
to the table.

---

## 8. Consensus & slot-lag pre-filter

For SVM networks with an active consensus policy and a `Finalized` request, the
network prunes upstreams whose `FinalizedSlot` trails the pool leader by more
than `MaxFinalizedSlotLag` (default 100 slots ≈ 40 s) before consensus runs, so
a lagging node can't drag the agreed result backward. Non-consensus paths rely
on the existing score-based selection.

---

## 9. Gaps & Limitations

Honest accounting of what this design does **not** yet do well:

1. **No live-backend cache test for SVM.** SVM cache logic is unit-tested against
   the in-memory connector; the Redis/DynamoDB container tests cover only the EVM
   cache. The connector layer is shared and unmodified, but there is no test
   asserting SVM keys round-trip through Redis/DynamoDB specifically.

2. **Genesis table is Solana-only.** Forks (Fogo, Eclipse) require
   `checkGenesisHash: true` or a manual table entry; otherwise cluster-membership
   validation silently no-ops. There is no automated discovery of fork genesis
   hashes.

3. **`getSignaturesForAddress` slot-window guard is coarse.** It bounds only via
   `minContextSlot` vs latest slot; `before`/`until` are signatures (not slots),
   so a large signature-range scan without `minContextSlot` is not bounded.

4. **Commitment options-index table is hand-maintained.** New Solana RPC methods
   (or vendor-specific methods) won't be commitment-injected until added to the
   table. Safe by default (unknown methods are skipped) but requires upkeep.

5. **Finality for `getBlockTime` is treated as final-by-construction.** A slot's
   timestamp can in principle change if a not-yet-rooted slot is dropped; this is
   a low-risk simplification, not a guarantee.

6. **No commitment-downgrade detection.** If an upstream silently returns data at
   a weaker commitment than requested (non-conforming node), eRPC trusts the
   request's commitment for finality classification — it does not re-derive
   finality from the response.

7. **Subscriptions / websockets out of scope.** `*Subscribe`/`*Unsubscribe` and
   streaming methods are not handled; only unary JSON-RPC over HTTP.

8. **No SVM-specific data-integrity validators.** EVM has rich integrity checks
   (tx root, receipts count, logs bloom, …). SVM has none beyond finality/commitment;
   malformed-but-well-typed responses are not cross-checked.

9. **Vendor auto-config (Phase 2) absent.** No built-in provider presets for
   Helius/Triton/QuickNode SVM endpoints; operators configure raw endpoints.

10. **Multi-chain tested narrowly.** Fogo mainnet/testnet validated live;
    Eclipse and other SVM forks are supported by construction but untested.

---

## 10. Verification evidence (v0.5)

- Unit: `architecture/svm` (incl. new finality + param-shaping regression tests),
  `common`, `consensus`, `upstream`, `data`, and the full `erpc` shards pass.
  (`TestEvmJsonRpcCache_DynamoDB/Redis` require Docker and are environment-gated.)
- Live Solana (`-tags svm_live`): genesis short-circuit (~0.3–0.6 ms), getSlot,
  cache series — pass.
- Live Fogo (mainnet + testnet) through the proxy: getSlot, getBlockHeight,
  getInflationRate, getVersion, getLatestBlockhash, getGenesisHash, body-routed
  `svm:fogo:mainnet` — all correct; load test 1000 req @ c=100, 0 failures.

### Round-3 evidence

- Unit: new `TestNetworkPreForward_InjectWriteCommitment`,
  `TestSvmStatePoller_SetDebounceInterval_UpdatesCadence`,
  `TestSvmStatePoller_Bootstrap_HonorsPresetDebounce`,
  `TestSvmVerifyGenesisHash_*`; full `architecture/svm`, `common`, `upstream`,
  `util`, and `erpc` SVM e2e suites pass.
- Live Fogo mainnet: `getGenesisHash` fetched + validated against a live fork
  (fail-closed path exercised). Outbound bodies confirmed via debug log:
  `sendTransaction` → `{"preflightCommitment":"confirmed"}`,
  `simulateTransaction` → `{"commitment":"confirmed"}`,
  `getAccountInfo` → `{"commitment":"confirmed"}`.
- Live Fogo mainnet: with `statePollerDebounce: 2s`, measured poll cadence
  ≈ 2.4 s (2 s gate + ≤ one-slot ticker), confirming the config is now honored
  (previously the field had no effect).
