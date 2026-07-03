# getLogs ↔ receipts cross-check — opportunistic completeness validation

Status: **implemented** as two checks in `checks_getlogs.go` —
`getLogsFilterSanity` (intrinsic: every returned log must match the request's
filter/range/blockHash) and `getLogsCompleteness` (corroborated: per-block
hash-anchored comparison against the ChainView's cached canonical receipts +
finalized absent-block detection, pin-anchored so corroborate-before-verdict
reconfirm applies). Filter matching is implemented in `logfilter.go` with
go-ethereum's exact `filterLogs` semantics (the "reuse erpc's existing filter
code" idea below turned out to be moot — erpc only *splits* getLogs requests
and never had a log matcher). Known limitation: a fully-empty `[]` response is
gated out before the engine (emptyish gate), so the everything-dropped case
remains covered by `retryEmpty`, not these checks.

## Problem

`eth_getLogs` is the one method that **cannot be validated intrinsically.**
Every other check recomputes a commitment from the response itself (block hash,
transactions root, receipts root, logs bloom). A `getLogs` response is a
*filtered subset* of logs across a block range — there is no root to recompute
and no self-contained invariant. So an upstream that silently drops logs (the
single worst failure mode for an indexer) passes every existing check.

Today `getLogs` has **zero** integrity coverage for completeness.

## Idea

Cross-validate a `getLogs` response against **block receipts we already hold**
for the blocks in the request's range. A block's receipts contain *all* of its
logs, authoritatively. So: reconstruct the complete per-block log set from the
cached receipts, apply the request's exact filter, and compare against the logs
the `getLogs` response returned for that block. A set mismatch = missing / extra
/ duplicated / reordered logs.

Receipts are the natural ground truth for `getLogs` — `getLogs` is essentially a
filtered aggregation over receipts' logs.

## Cost model — opportunistic, zero network cost

Runs **if and only if** the receipts for a block are already in the ChainView
receipts cache (fetched by earlier, unrelated operations). It **never forces a
fetch**. When receipts aren't cached, the block is skipped (no-op). This is
intrinsic-tier cost (a cache read + a filter pass) with corroboration-tier
power. It deliberately does not add to the authoritative force-fetch budget.

## Scope — per-block, opportunistic

For each block in `[fromBlock, toBlock]`:

- If ChainView holds receipts for that block **at the same `blockHash` the
  `getLogs` logs reference** → validate that block.
- Otherwise → skip that block.

A stricter all-or-nothing variant (validate only if *every* block in the range
is present) is simpler but covers less; per-block opportunistic gives strictly
more coverage at the same cost. Either is sound.

## Correctness requirements (must get right)

1. **Anchor by blockHash, never block number.** Only compare when the
   response's per-log `blockHash` matches the hash of the receipts held. On a
   mismatch it's a fork/reorg — **skip, never reject.** Number-based comparison
   would reintroduce reorg false positives.
2. **Finality.** Finalized blocks are safe (immutable log set) — the natural
   first target. On unfinalized blocks, `blockHash` anchoring already guards
   most reorg cases; behavior otherwise follows `invalidBehavior`.
3. **Filter semantics must be byte-exact with EVM `getLogs`.** address OR-set;
   up to 4 topic positions; `null` = wildcard at a position; array-at-position =
   OR; position-sensitive. This is the #1 false-positive risk — **reuse erpc's
   existing filter-matching code, do not reimplement**, and fuzz it hard.
4. **Source independence.** This is genuine corroboration only when the cached
   receipts came from a *different* fetch/upstream than the `getLogs` response.
   If the same upstream consistently drops the same logs from both, the
   cross-check passes (false negative). Track the receipts' source and prefer
   cross-source comparison where possible; document the limitation.
5. **Bound the work.** Cap logs/blocks compared per request so a large warm
   range can't add a latency spike.

## What it catches / misses

- **Catches:** missing, extra, duplicated, or reordered logs on blocks that are
  both cached and canonical. Compare on the set of
  `(address, topics, data, txHash, logIndex)`.
- **Misses:** ranges not in cache (large cold backfills); consistent drops from
  a single bad source that fed both sides.

## Fit

- Reuses the ChainView **receipts cache** and number→hash pins — the
  infrastructure already exists (mini-indexer).
- New check, e.g. `getLogsReceiptCompleteness`, category `eth_getLogs`,
  cost-tier **intrinsic** (no force-fetch).
- Because catches are cross-source + hash-anchored + (ideally) finalized, they
  are **high-confidence genuine** — a clean value signal, unlike reorg-sensitive
  continuity checks.

## Later / open

- Optional authoritative variant: when a `getLogs` completeness reject is
  suspected and receipts are *not* cached, force-fetch the block receipts to
  confirm before rejecting. Opt-in, separate from this zero-cost check.
