# SVM (Solana) Support in eRPC — Design v0.5

> Supersedes `DESIGN-MULTI-CHAIN-SOLANA.md` (v0). This revision reflects the
> rebase onto current upstream `main`, **multi-chain SVM** support
> (`svm:<chain>:<cluster>`), and four correctness fixes landed after review.
> It adds end-to-end workflow diagrams, an EVM-vs-Solana comparison, and an
> honest **Gaps & Limitations** section.
>
> ### Corrections to this document
>
> v0.5 is the design of record, but review after it was written changed several decisions. The
> list is here so a reader does not have to diff the doc against the code; each item is also
> corrected inline at the section named. Where v0's corrections banner covers the same ground it
> is cited as *v0 item N*.
>
> 1. **The `getSignaturesForAddress` slot-window guard was deleted, not refined** (§2 hook
>    table, §3 diagram, §9 gap 3). v0.5 kept it and merely called it "coarse". It is gone: the
>    method paginates by *signature* cursors (`before`/`until`) plus a `limit`, so there is no
>    slot range to bound — and the `minContextSlot` the guard read is a node-freshness floor,
>    the minimum bank slot at which a request may be **evaluated**, never a bound on returned
>    history. Its `maxSlotsPerSignaturesQuery` config key went with it (v0 item 1).
> 2. **Finality is per-method, not a commitment-level fallback** (§0 table, §4, §5).
>    `commitment: finalized` is the latest **rooted** slot — a head advancing every ~400 ms, not
>    immutability. Only *slot-pinned* reads (`getBlock`, `getTransaction`) classify `Finalized`,
>    and only at `finalized`; every moving-head read is `Realtime` at **every** commitment level.
>    `getBlocks` and `getSignaturesForAddress` are moving-head, not slot-pinned — v0.5 grouped
>    them with `getBlock`/`getTransaction` (v0 item 4).
> 3. **`optionsAppend` was split into two sentinels** (§6). A single trailing-object bucket
>    cannot express both `getLeaderSchedule`, whose config may legally sit at param 0, and
>    `getBlocks`, which requires a positional start slot first. A `clampCommitmentForMethod`
>    step was added alongside it.
> 4. **The cache key is two parts and case-PRESERVING** (§5) — not the single
>    `<networkId>:<slotRef>:<method>:<sha256(params)>` string described there.
> 5. **`slot` and `blockHeight` are different counters** (§1 table). Only `getSlot` is
>    tip-corrected, and per commitment; `getBlockHeight` passes through untouched (v0 item 6).
> 6. **The non-retryable write set includes `requestAirdrop`** (§2 hook table, §3 diagram) —
>    it *mints* per call, so a failover after an effective first attempt mints twice
>    (v0 item 8).

---

## 0. What changed since v0

| Area | v0 | v0.5 |
|------|----|------|
| Network identity | `svm:<cluster>` (Solana only) | `svm:<cluster>` **and** `svm:<chain>:<cluster>` (Fogo, Eclipse, forks) |
| Commitment injection | network pre-forward (after cache read) | **project pre-forward (before cache read)** — fixes a permanent cache miss |
| Finality of `getBlock`/`getTransaction` | always finalized | **slot-pinned** — `Finalized` only at `commitment: finalized`, `Unfinalized` below it (corrections item 2) |
| Finality of `getBlocks`/`getSignaturesForAddress` | always finalized | **moving-head** — `Realtime` at every commitment level; both grow toward the head (corrections item 2) |
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
| Chain progress unit | **block number** (height) | **slot** (~400 ms; some slots are empty/skipped). Solana *also* has a block height, and it is a **different counter**: a skipped slot advances the slot but not the height, so `getSlot` and `getBlockHeight` diverge permanently (corrections item 5) |
| Block identity | `blockHash` (re-org → new hash) | `blockhash` of a slot; slots can be skipped |
| Finality model | depth-based (`latest`/`safe`/`finalized` ≈ N confirmations) | **commitment**: `processed` < `confirmed` < `finalized` |
| "Is this final?" | block ≤ finalized head | response commitment == `finalized` |
| Re-org exposure | re-org rewrites recent blocks | confirmed (non-rooted) slots can be dropped on fork switch |
| Chain id | numeric (`eth_chainId` = 1) | cluster genesis hash (immutable) |
| Tip freshness signal | `eth_blockNumber` | `getSlot` / `getLatestBlockhash` |
| Write idempotency | `eth_sendRawTransaction` (hash-keyed) | `sendTransaction`, `sendRawTransaction` and `requestAirdrop` must NOT be blindly retried across nodes — the first two can double-broadcast once the original propagates, and `requestAirdrop` *mints* per call |
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
| `HandleNetworkPreForward` | network (post-selection) | per-method validation gates (`getBlock` availability guard); `minContextSlot` and consensus slot-lag upstream prefilters. **No `getSignaturesForAddress` guard** — deleted (corrections item 1) |
| `HandleUpstreamPostForward` | upstream | non-retryable write guard (`sendTransaction`, `sendRawTransaction`, `requestAirdrop`); opportunistic slot tracking |
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
    I -- getBlock slot above pool's indexed frontier --> I1[short-circuit:\nindexing-lag retry] --> Z
    I -- else --> J[per-upstream forward\n(failsafe: retry/hedge/cb/timeout)]
    J --> K[HandleUpstreamPreForward → upstream RPC → HandleUpstreamPostForward]
    K -- write method failed\n(sendTransaction/sendRawTransaction/requestAirdrop) --> K1[strip retryability\n(no cross-node rebroadcast, no double mint)]
    K --> L[GetFinality(req,resp)\nper-method: neverCache / alwaysFinalized /\nslotPinned@finalized / else realtime]
    L --> M{neverCacheMethods?}
    M -- yes --> Z
    M -- no --> N[cache SET\nper matching policy's finality + TTL]
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
5. forward; finality = block ≤ finalized     5. forward; finality = per-method table (§5)
6. cache SET if finalized/unfinalized        6. cache SET (never-cache methods hard-skipped)
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

`architecture/svm/finality.go` resolves finality in priority order. **CORRECTED — step 3 is not
a commitment-level fallback** (corrections item 2); v0.5 still had one.

```
1. neverCacheMethods      → Realtime, AND hard-skipped by the cache layer
      sendTransaction, sendRawTransaction, simulateTransaction, requestAirdrop,
      getLatestBlockhash, getRecentBlockhash, getFeeForMessage, getSignatureStatuses,
      getVoteAccounts, getLeaderSchedule, getEpochInfo, getSlotLeaders,
      getRecentPerformanceSamples, getRecentPrioritizationFees
2. alwaysFinalizedMethods → Finalized (final by construction)
      getInflationReward (finalized epochs only), getBlockTime
3. slotPinnedMethods      → Finalized at commitment == finalized, else Unfinalized
      getBlock, getTransaction (+ getConfirmedBlock / getConfirmedTransaction aliases)
4. everything else        → Realtime (moving-head read), at EVERY commitment level
      getBalance, getAccountInfo, getProgramAccounts, getTokenAccountBalance,
      getBlocks, getBlocksWithLimit, getSignaturesForAddress, getEpochSchedule, …
```

**Why step 4 is the load-bearing rule.** `commitment: finalized` on Solana is the state at the
latest **rooted** slot, and the rooted slot advances roughly every 400 ms. It is a moving head,
not EVM's immutability horizon. A read whose answer depends on where the head is therefore
answers a different question every slot even at `finalized` — exactly like EVM's `latest` tag,
which `erpc/networks.go` already maps to Realtime. Classifying those `Finalized` would be a
permanent-cache bug: `DataFinalityStateFinalized` is the zero value a policy with no explicit
`finality` matches, and an unset TTL means "no expiry" in the connectors, so a cached balance
would never be invalidated by a later transfer.

**What changed from v0.5's own table.** v0.5 correctly demoted `getBlock`/`getTransaction` from
"always finalized" to commitment-sensitive, but put `getBlocks` and `getSignaturesForAddress` in
the same bucket. They do not belong there:

- `getSignaturesForAddress` — the signature list for an address *grows* as transactions land, so
  it tracks the head even though it names an address.
- `getBlocks` / `getBlocksWithLimit` — range queries. `getBlocks(start)` with no end slot runs to
  the current head, and either form can name an upper bound the chain has not reached, returning
  a partial list that grows. A fully-in-the-past `getBlocks(start, end)` range *is* immutable and
  knowingly gets only realtime-TTL caching: promoting it would require comparing `end` against
  the poller's finalized slot, which would make finality depend on mutable poller state and so
  vary over time for an identical request. Accepted knowingly — `getBlocks` is cheap next to
  `getBlock`.
- `getEpochSchedule` is not never-cache either: its constants (`slotsPerEpoch`,
  `leaderScheduleSlotOffset`, …) change only at epoch boundaries (~432,000 slots / ~2 days), so
  it falls through to step 4 and is cached under the realtime policy's TTL.

`minContextSlot` is not a promotion signal either: it is the minimum bank slot at which the
request may be *evaluated*, so `getBalance(pubkey, {minContextSlot: 1})` still answers at the
current head.

**Never-cache is hard-enforced.** `Realtime` is still a *cacheable* finality at the policy layer
(EVM caches realtime reads with a short TTL + age guard), so to honor the "never cached"
guarantee the SVM cache `Get`/`Set` hard-skip any method in `neverCacheMethods` before policy
matching — an operator's stray `finality: realtime` policy can no longer cache an effectful
method.

**Finality tracks the *effective* commitment, not just the network default.** Step 3 uses
`resolveCommitment`, the single predicate that also drives injection. So when injection
legitimately skips a request (legacy `getBlock(slot,"base64")` form, missing args,
non-injectable method), no default reaches the upstream and the response is classified
Unfinalized — the network default is never over-trusted. Because the predicate reads request
shape + config (not mutation state), this holds whether finality is computed before or after
injection (it is memoized on the first call, which happens pre-injection in `erpc/projects.go`).

**Routing asks a different question.** `IsFinalizedCommitment` answers "which slot does the node
evaluate this at" — true for `getBalance` at `commitment: finalized` — and is what the upstream
slot-lag prefilter consults. `GetFinality` answers "is this response immutable enough to cache".
Since the moving-head fix the two have diverged; conflating them is the trap to avoid.

**Cache key — CORRECTED** (corrections item 4). It is two parts, not one string, and the request
half is case-**preserving**:

```
partition key : <networkId>:<slotRef>
request key   : <method>:<type- and structure-delimited digest of params>
```

`networkId` carries the chain (`svm:fogo:mainnet` ≠ `svm:mainnet-beta`), so chains and clusters
never collide and an SVM network can share one Redis/DynamoDB connector with `evm:1`. `slotRef`
is the request's own `minContextSlot` or the literal `*` — derived from params, never from the
poller, so a given `(method, params)` tuple yields the same partition key on `Set` and on `Get`
(deriving it from live poller state would move the key under a request every 400 ms and
guarantee a permanent miss). The request key deliberately avoids the shared `req.CacheHash()`,
which lowercases string params: right for EVM hex, catastrophic for base58 pubkeys and
signatures, where `So111…` ≠ `so111…` and collapsing case would serve one account's data under
another's key. The digest is type- and structure-delimited, so `["1"]` and `[1]`, or `[[a],[b]]`
and `[a,b]`, cannot collide.

---

## 6. Commitment injection — shape-aware (fix #3)

Solana's options/config object lives at a **method-specific param index**, and a few methods take
no params at all. Blind append/mutate corrupts requests, so injection is driven by a per-method
`commitmentOptionsIndex` table (`architecture/svm/hooks.go`).

**CORRECTED — one trailing-object bucket was not enough** (corrections item 3). v0.5 had a single
`optionsAppend`. The shipped table has two sentinels, because "the options object is the trailing
param" means different things depending on whether a positional arg is *required* first:

```
optionsIndex 0               → getBlockHeight, getBlockProduction, getEpochInfo,
                               getInflationGovernor, getLargestAccounts, getLatestBlockhash,
                               getSlot, getSlotLeader, getStakeMinimumDelegation, getSupply,
                               getTransactionCount, getVoteAccounts
optionsIndex 1               → getAccountInfo, getBalance, getBlock, getTransaction,
                               getMultipleAccounts, getProgramAccounts, getSignaturesForAddress,
                               getStakeActivation, getTokenAccountBalance,
                               getTokenLargestAccounts, getTokenSupply, isBlockhashValid,
                               getMinimumBalanceForRentExemption
optionsIndex 2               → getBlocksWithLimit, getTokenAccountsByOwner,
                               getTokenAccountsByDelegate
optionsTrailing        (-1)  → getLeaderSchedule. NO positional arg is required, so the config
                               object may legally be param 0: its first arg is an OPTIONAL epoch
                               slot which may be omitted or null, and agave accepts a config
                               object in its place (RpcLeaderScheduleConfigWrapper is an untagged
                               SlotOnly|ConfigOnly enum). Shapes: [] | [{cfg}] | [slot] |
                               [slot, {cfg}]
optionsTrailingAfterOne (-2) → getBlocks. At least one positional arg MUST precede the config
                               object ([start] | [start, end]); appending one to [] would put it
                               where the required start slot belongs
(absent)                     → no-param methods — getInflationRate, getVersion, getHealth,
                               getGenesisHash, getIdentity, getBlockTime, … (appending an options
                               object yields -32602 "No parameters were expected");
                               getSignatureStatuses, whose only option is
                               searchTransactionHistory and which has no commitment field; and
                               every write method, which carries commitment in its own field
                               (below)
```

That distinction is exactly what lets `getLeaderSchedule` take its config at param 0 while
`getBlocks` keeps its required start slot.

Injection rules at the resolved index:

```
slot is an object with commitment     → honor caller (skip)
slot is an object without commitment  → set commitment, invalidate CacheHash
slot is the next free position        → create {commitment}, invalidate CacheHash
slot occupied by a non-object         → SKIP (legacy getBlock(slot,"base64") /
                                        getTransaction(sig,"json") encoding-string form;
                                        never produce an invalid param)
required positional args missing      → SKIP (let the upstream report it)
```

**Plus a clamp step, new since v0.5.** `clampCommitmentForMethod` narrows the commitment about to
be injected to a level the target method actually accepts. `getBlock`, `getBlocks`,
`getBlocksWithLimit`, `getSignaturesForAddress` and `getTransaction` reject `processed` outright —
agave answers `-32602` "Method does not support commitment below `confirmed`", because a
processed slot can sit on a minority fork that is later abandoned. A configured `processed`
default is therefore clamped to `confirmed` for those five. (`getBlockProduction`,
`getLeaderSchedule` and every write method do accept `processed`, verified field-by-field against
the JSON-RPC reference, and are deliberately not in the set.)

The policy is **clamp, not skip**. Skipping injection for these methods would leave each upstream
on its own server-side default — precisely the cross-upstream divergence injection exists to
eliminate — and would make `resolveCommitment` report `""`, so the finality classification and
the cache key would lose the commitment too. Clamping to the nearest legal level keeps every
upstream in lockstep and stays as close as legally possible to the operator's "freshest data"
intent. It applies **only** to the injected network default: a caller-supplied commitment is
classified explicit and never rewritten, so if a client explicitly asks `getBlock` for
`processed`, the upstream's `-32602` is the honest answer — silently upgrading it would hand back
data the client did not ask for.

Note that `getBlockHeight` *is* commitment-injectable at index 0 but is **not** tip-corrected:
block height is a different counter from slot (corrections item 5).

This fixes: `getInflationRate` (no params → `-32602`), the legacy `getBlock(slot,"enc")` /
`getTransaction(sig,"enc")` form, commitment landing on `getTokenAccountsByOwner`'s filter object
instead of its config, `getLeaderSchedule` and `getBlocks` needing opposite trailing-object
treatment, and an injected `processed` default drawing `-32602` from the five confirmed-or-higher
methods.

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

For SVM networks with an active consensus policy and a request whose resolved finality is
`Finalized` — which, per §5, means a slot-pinned read at `commitment: finalized` — the network
prunes upstreams whose `FinalizedSlot` trails the reference slot by more than
`maxFinalizedSlotLag` before consensus runs, so a lagging node cannot drag the agreed result
backward. Non-consensus paths rely on the existing score-based selection.

`maxFinalizedSlotLag` is a `*int64` so three states stay distinguishable: omitted → 100 slots
(≈40 s), explicit `0` → filter **disabled**, `>0` → that value. The reference slot is not the raw
pool max (see §11 item 3 for the single-liar clamp), and the filter never returns an empty pool:
if every upstream is excluded the original list passes through, because serving possibly-stale
data beats deadlocking the request — the failsafe consensus policy is the right layer to detect
divergence. An upstream with no finalized-slot sample yet also passes; unknown is not the same as
trailing.

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

3. **`getSignaturesForAddress` pagination is not bounded at all.** The slot-window guard this
   section described was deleted rather than refined (corrections item 1): it rested on reading
   `minContextSlot` as a bound on returned history, which it is not. Solana bounds the method by
   signature cursors (`before`/`until`) plus a `limit`, so eRPC has nothing slot-shaped to
   validate and passes the request straight through. A client that pages naively can still issue
   an expensive scan; cursor-following auto-pagination remains a follow-up.

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

## 11. Round-4 hardening deltas (comparative review vs Lava-derived routers)

A structured comparison against Magma-Devs/smart-router (Lava-derived) and the
agave `RpcCustomError` source produced these deltas — all SVM-scoped; the EVM
path is untouched:

1. **Error taxonomy corrected & completed** (`architecture/svm/error_normalizer.go`).
   Constants now map 1:1 to agave `custom_error.rs` (-32001 … -32019).
   Reclassified: `-32006` (TransactionPrecompileVerificationFailure) and
   `-32015` (UnsupportedTransactionVersion) are client-side, non-retryable —
   previously both failed over pointlessly. `-32009`
   (LongTermStorageSlotSkipped) is now terminal at network scope: it is an
   authoritative, cluster-wide skip verdict; cross-upstream retries cannot
   change it (the storage-outage case, `-32019`, stays retryable). Added:
   `-32001`/`-32010`/`-32011` → missing-data (index/history coverage is
   per-provider, so failover is correct), `-32012`/`-32019` → server-side,
   `-32017` → non-retryable chain-state condition, `-32018` → client-side.
   Codeless vendor variants ("missing in long-term storage", "ledger jump")
   are matched by message in the `-32000` bucket.

2. **Consensus works on RpcResponse-enveloped methods out of the box**
   (`common/defaults.go`). Default `ignoreFields` now strip `context.slot` and
   `context.apiVersion` for the enveloped SVM read methods — previously two
   healthy upstreams at adjacent slots registered dissent on identical values
   (the same defect that makes smart-router's cross-validation unusable on
   Solana). The `value` payload is still fully compared.

3. **Single-liar-safe consensus reference** (`architecture/svm/slot_lag.go`).
   The slot-lag prefilter's reference is no longer the raw pool max: when the
   leader outruns the runner-up by more than `maxFinalizedSlotLag`, the
   runner-up becomes the reference, so one upstream reporting an inflated
   finalized slot cannot shrink the consensus pool to itself.

4. **`minContextSlot` selection prefilter** (`slot_lag.go` + `erpc/networks.go`).
   Requests carrying `minContextSlot` skip upstreams whose tracked slot (at
   the request's commitment) is known to be behind it — avoiding guaranteed
   `-32016` round-trips. Defensive fallback keeps the pool non-empty.
   Full per-client monotonic-read injection (router-maintained seen-slot →
   `minContextSlot` stamping) remains a follow-up: it needs a client-identity
   store.

5. **Traffic-gated polling** (`svm_state_poller.go`, `hooks.go`). `context.slot`
   harvested from live responses now also feeds the finalized view (when the
   request's effective commitment is finalized) and doubles as freshness
   evidence: when both slot views are traffic-fresh within the debounce
   window, the poller skips its two `getSlot` calls (bounded at 4 consecutive
   skips; `getHealth`/`getMaxShredInsertSlot` always run). On busy networks
   this roughly halves background poll quota on paid vendors.

6. **Example config guidance** (`erpc.svm.example.yaml`): 150 ms initial retry
   delay for tip-propagation `-32004`s, and notes on the consensus/prefilter
   defaults above.

Deliberately NOT adopted from the comparison: previous-slot walk-back on
`-32004` (answering for a different slot than requested is tracker-internal
semantics, not proxy behavior), marking `-32010` non-retryable (index coverage
varies per provider — failover is correct), and the reference router's WS
stack (its Solana notification dispatch, unsubscribe derivation, and id
rewrite are demonstrably broken; only its edge-case catalog informs our
Phase-2 WS design).
