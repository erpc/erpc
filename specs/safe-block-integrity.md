# Safe Block Integrity

**Status:** proposed — implementation plan for a single PR
**Motivation thread:** [TECHOPS-25865](https://circlepay.atlassian.net/browse/TECHOPS-25865) — BRP / Op-stack safe block investigation

## 1. Problem

`eth_getBlockByNumber("safe")` is forwarded verbatim to every upstream. Each provider interprets
_"safe"_ according to its own `OP_NODE_VERIFIER_L1_CONFS` setting:

| Provider | `OP_NODE_VERIFIER_L1_CONFS` | Effective "safe" block |
|---|---|---|
| Internal 1P (op-node) | 15 | most conservative |
| dRPC | 4 | looser |
| Blockdaemon / Alchemy | 0 (default) | safe = latest |

`OP_NODE_VERIFIER_L1_CONFS` ([op-node reference](https://docs.optimism.io/node-operators/reference/op-node-config)):
_"Number of L1 blocks to keep distance from the L1 head before deriving L2 data from."_
The default is **0** — providers that do not override it treat every derived L2 block as "safe"
immediately, with no L1 confirmation buffer.

When `agreementThreshold=2`, two 3P providers can agree on a "safe" block that is ahead of
Internal's 15-confirmation guarantee and win the consensus round, silently violating it.
Internal services using `FINALIZECHECKPOINT=safe` — ledger stablecoin, CPN, web3 — are exposed
to this risk today.

**Root cause — two structural gaps in eRPC:**

1. `"safe"` is forwarded verbatim to upstreams (unlike `"latest"` / `"finalized"`, which are
   translated to concrete hex block numbers before forwarding in `resolveBlockTagToHex()`).
2. `GetFinality()` classifies every block tag — including `"safe"` and `"finalized"` — as
   `DataFinalityStateRealtime`, so the selection policy cannot distinguish `"safe"` from
   `"latest"` or `"pending"`.

**Note:** `servedTip` does not address this. It covers availability lag (upstreams that have not
seen a block yet). This is a definitional disagreement — different providers define "safe" at
different L1 confirmation depths.

---

## 2. Why "latest" and "finalized" avoid this problem

`resolveBlockTagToHex()` in `architecture/evm/json_rpc.go` already translates `"latest"` and
`"finalized"` to concrete hex block numbers **before forwarding to upstreams**. All upstreams
therefore receive `eth_getBlockByNumber(0x64, …)` — the same concrete number — so consensus
can agree on the response.

| Tag | Translated? | Direction |
|---|---|---|
| `"latest"` | Yes — `EvmHighestLatestBlockNumber()` | max across upstreams |
| `"finalized"` | Yes — `EvmHighestFinalizedBlockNumber()` | max across upstreams |
| `"safe"` | No — passed verbatim | — |

For `"safe"`, the correct direction is the **min** (most conservative). The fix translates
`"safe"` to `(highest-latest − latestBlockMinus)` before forwarding — a ceiling computed
entirely from the already-tracked latest block, with no dependence on per-upstream safe block
polling.

**Why not aggregate per-upstream safe blocks?** Taking the minimum of `SafeBlock()` across
upstreams creates a dependency on the sync state of every upstream in the pool. If an internal
node is running but lagging, its stale safe block would drag the ceiling down and make responses
very stale — a liveness failure. If an internal node is counted as non-syncing but is far behind,
the problem is silent. The `latestBlockMinus` approach avoids this entirely: the ceiling is
derived from `EvmHighestLatestBlockNumber()`, which already has health-check semantics and is
not affected by per-upstream safe block polling.

---

## 3. Design

Two axes, both required.

### 3.1 Tag rewrite in `resolveBlockTagToHex` (fixes mixed-consensus disagreement)

Filling the missing `case "safe":` in `resolveBlockTagToHex()` mirrors how `"finalized"` works.
The ceiling is `EvmHighestLatestBlockNumber() − latestBlockMinus`, where `latestBlockMinus` is
configured per network (see §3.2). No per-upstream safe block polling is required.

**File:** `architecture/evm/json_rpc.go`

```go
case "safe":
    if cfg := network.Config().Evm.SafeBlock; cfg != nil && cfg.LatestBlockMinus > 0 {
        if bn := network.EvmHighestLatestBlockNumber(ctx) - cfg.LatestBlockMinus; bn > 0 {
            if hx, err := common.NormalizeHex(bn); err == nil {
                return hx, true
            }
        }
    }
    // fall through: no cap configured — "safe" forwarded verbatim (same as today)
```

**Why this fixes mixed consensus:** without rewriting, 1P returns `safe=100` and 3P returns
`safe=120`. Different block numbers → different response hashes → no quorum. With the rewrite,
all upstreams receive the same concrete block number derived from the configured ceiling; consensus
can agree regardless of each upstream's internal L1 confirmation depth.

**Note on upstream response semantics:** after the tag rewrite, upstreams receive a concrete
block number and return that block's data regardless of whether they internally consider it
"safe" — they return block data, not a finality verdict. The safety guarantee is enforced
entirely by eRPC's ceiling, not by upstream behaviour.

**No `latestBlockMinus` configured:** `"safe"` is forwarded verbatim — same as today. This is
fail-open and a known limitation (see §6 open question 1).

### 3.2 `latestBlockMinus` — per-network ceiling (operator-configured)

Operators express their L1 confirmation requirement in L2 blocks. `OP_NODE_VERIFIER_L1_CONFS`
is an L1-block count; convert to L2 blocks as:

```
latestBlockMinus = L1_CONFS × (L1_block_time / L2_block_time)
                 = 15 × (12s / 2s)
                 = 90   # for a 2-second L2 (e.g. Base, OP Mainnet)
```

**Approximation note:** real chains do not have perfectly constant block times. The formula is
a median estimate. Add a safety margin (e.g. 10–20%) for strict guarantees:
`latestBlockMinus: 105` (= 90 × 1.15, rounded up).

**Config schema (new `safeBlock` block under `networks[].evm`):**

| Field | Type | Default | Description |
|---|---|---|---|
| `evm.safeBlock.latestBlockMinus` | `int64` | `0` (disabled) | Ceiling = `(EvmHighestLatestBlockNumber() − N)`, in **L2 blocks**. Convert: `N = L1_CONFS × (L1_block_time / L2_block_time)`. |

**Files:** `common/config.go` — add `SafeBlock.LatestBlockMinus` to `EvmNetworkConfig`;
`common/validation.go` — reject negative values.

### 3.3 Axis 3 — Introduce `DataFinalityStateSafe`

`GetFinality()` currently classifies every block tag as `DataFinalityStateRealtime`. In
Ethereum finality ordering `latest < safe < finalized`, so collapsing `"safe"` into either
`Realtime` or `Finalized` is semantically wrong. A new enum value is introduced so selection
policies can distinguish all three levels independently.

**Files:** `common/architecture_evm.go` (enum), `erpc/networks.go` — `GetFinality()`

```go
case "safe":
    return DataFinalityStateSafe
case "finalized":
    return DataFinalityStateFinalized
```

Operators can then route `"safe"` requests to internal-only upstreams via `evalScope`:

```yaml
selectionPolicy:
  evalScope: "network-finality"
  evalFunc: |
    if (finality === 'finalized' || finality === 'safe')
      return upstreams.filter(u => u.tags['tier'] === 'internal')
    return upstreams
```

### 3.4 Recommended production configuration

```yaml
networks:
  - architecture: evm
    evm:
      safeBlock:
        # 15 L1 confs × (12s L1 / 2s L2) = 90 L2 blocks; +15% margin → 105
        latestBlockMinus: 105

upstreams:
  - id: internal-1p-base
    tags:
      tier: internal
  - id: drpc-base
    tags:
      tier: external
  - id: blockdaemon-base
    tags:
      tier: external
  - id: alchemy-base
    tags:
      tier: external
```

This ceiling is computed from `EvmHighestLatestBlockNumber()` — the highest latest block across
all upstreams, which already has its own health-check semantics. It is unaffected by whether
internal nodes are in sync, lagging, or down.

---

## 4. Files changed

| File | Change |
|---|---|
| `common/architecture_evm.go` | Add `DataFinalityStateSafe` enum value |
| `common/config.go` | Add `SafeBlock.LatestBlockMinus` to `EvmNetworkConfig` |
| `common/validation.go` | Reject negative `latestBlockMinus` at startup |
| `erpc/networks.go` | Reclassify `"safe"` in `GetFinality()` → `DataFinalityStateSafe` |
| `architecture/evm/json_rpc.go` | `case "safe":` in `resolveBlockTagToHex()` |

~30 lines of code total. No new interface methods, no polling goroutines, no fakes to update.

---

## 5. Invariants (each backed by a test)

- **I1 — Verbatim fallback when unconfigured.** When `latestBlockMinus` is 0 or absent,
  `resolveBlockTagToHex` falls through and `"safe"` is forwarded verbatim — identical behavior
  to today.
- **I2 — Ceiling derived from highest-latest.** `EvmHighestLatestBlockNumber() − latestBlockMinus`
  is the ceiling. It does not depend on per-upstream safe block polling and is unaffected by
  internal node sync state.
- **I3 — All upstreams receive the same concrete block number.** After the rewrite, every
  upstream in a consensus round receives an identical hex block number for `"safe"` requests.
  Verified by inspecting outgoing requests in tests.
- **I4 — Mixed-consensus agrees.** With 1P and 3P upstreams and `latestBlockMinus` configured,
  both receive the same concrete ceiling block; consensus agrees regardless of each upstream's
  internal L1 confirmation depth.
- **I5 — `DataFinalityStateSafe` is distinct.** `GetFinality("safe")` = `DataFinalityStateSafe`,
  `GetFinality("finalized")` = `DataFinalityStateFinalized`, `GetFinality("latest")` =
  `DataFinalityStateRealtime`. No two tags share a value.
- **I6 — Config validation rejects negative `latestBlockMinus`.** Startup error on negative
  value; `0` is valid (disables the ceiling).

---

## 6. Open questions

1. **No `latestBlockMinus` configured — fail-open.** When no ceiling is configured, `"safe"` is
   forwarded verbatim and each upstream answers with its own interpretation. This is the current
   behavior (no regression), but operators who deploy without `latestBlockMinus` receive no
   protection. A future improvement could reject `"safe"` requests entirely when no ceiling is
   configured (fail-closed), at the cost of requiring all operators to set the value.

2. **`latestBlockMinus` and `servedTip` interaction.** When `(EvmHighestLatestBlockNumber() −
   latestBlockMinus)` falls below the `servedTip` window, upstreams may be excluded from
   selection. Operators should verify that `latestBlockMinus` does not push the effective safe
   block below `servedTip`.

3. **Chains that do not support `"safe"`.** No polling is introduced by this spec, so there are
   no polling errors. On such chains, `"safe"` will simply be forwarded verbatim when no
   `latestBlockMinus` is configured — same as today.

4. **`EvmHighestLatestBlockNumber()` staleness.** If all upstreams are significantly behind
   (e.g. during a major outage), the ceiling moves back with them — more conservative, not less.
   This is the correct direction.

---

## 7. Test matrix

| Area | Cases |
|---|---|
| Verbatim fallback (I1) | `latestBlockMinus` = 0 → `resolveBlockTagToHex("safe")` returns `("safe", false)` |
| Ceiling computation (I2) | `latestBlockMinus: 90`, `EvmHighestLatestBlockNumber()` = 190 → ceiling = 100 → `resolveBlockTagToHex` returns `("0x64", true)` |
| Concrete block forwarded (I3) | All upstreams in request receive `0x64`, never the string `"safe"` |
| Mixed consensus (I4) | 1P + 3P with different internal safe blocks → both receive `0x64`; consensus agrees |
| Finality enum (I5) | `GetFinality("safe")` = `DataFinalityStateSafe`; `GetFinality("finalized")` = `DataFinalityStateFinalized`; `GetFinality("latest")` = `DataFinalityStateRealtime` |
| Config validation (I6) | `latestBlockMinus: -1` → startup error; `latestBlockMinus: 0` → accepted |
| Selection policy routing | `evalScope: "network-finality"` routes `"safe"` to internal-tagged upstreams only |
| `make test-fast` | All existing tests pass |

---

## 8. Explicit non-goals

- **No per-upstream safe block polling.** Aggregating `SafeBlock()` across upstreams creates
  a dependency on each upstream's sync state; a lagging internal node would silently drag the
  ceiling down. The `latestBlockMinus` cap is derived from `EvmHighestLatestBlockNumber()`,
  which already has health-check semantics.
- **No `enforceLowestSafeBlock` post-forward ceiling check.** Response-time enforcement based
  on an aggregated ceiling is fragile (out-of-sync upstreams skew it) and redundant when the
  tag rewrite already sends a concrete block number to every upstream.
- **No change to how `"finalized"` is handled.** The existing `EvmHighestFinalizedBlockNumber()`
  path is untouched.
- **No client-visible API changes.** Callers send `eth_getBlockByNumber("safe", …)` unchanged;
  eRPC rewrites internally.
- **No removal of `OP_NODE_VERIFIER_L1_CONFS` from internal op-nodes.** That config stays in
  place. `latestBlockMinus` is an eRPC-layer enforcement of the same invariant for all upstreams.

---

## 9. Acceptance

- `make test-fast` green.
- Full test matrix (§7) green.
- Config-load validation rejects negative `latestBlockMinus` (I6).
- Integration test: `eth_getBlockByNumber("safe", true)` with `latestBlockMinus: 90` and
  `EvmHighestLatestBlockNumber()` = 190 → all upstreams receive `0x64`; response is block 100.
- Integration test: `latestBlockMinus` absent → `"safe"` forwarded verbatim to upstreams.
