# ChainView — central state for the data-integrity module

## Problem

The integrity state today is fragmented and not reorg-aware:

- `blockHistory` is a per-network `number → hash` map, **last-write-wins**, hashes
  only, no fork detection / finality guard / descendant invalidation.
- The resolver force-fetches the canonical **independently** — never consults or
  populates the history, re-fetches every time. This is the `receiptVsBlock`
  tip-lag false positive (it compares a receipt against a *fresh, racy* canonical
  fetch) and wasted upstream load.

There is no "fetch block N once" and no consistent view, so erpc can serve a block
from one fork and a receipt from another → a "mixed-up node".

## Goal

One in-memory, reorg-aware store per network that every stateful check reads and
writes, auto-populated from **user requests and the module's own aux fetches**, so:

1. block N's anchor is fetched **once** (then reused) unless a reorg invalidates it,
2. everything erpc serves for a number agrees (block ↔ receipts ↔ logs), and
3. reorgs are handled minimally (adopt + invalidate), bounded by finality.

## Decisions (locked)

1. **No ChainView-level consensus.** Resolve uses `network.Forward`, which already
   applies the user's network-level failsafe/consensus config. We trust whatever it
   returns — fork adoption = "trust the latest `network.Forward` result".
2. **Window = smallest necessary.** Default **32 blocks**, per-network override
   (e.g. polygon `256`). The window is both the reorg-tracking depth and the
   retention near the tip; below `tip − window` we evict and stop tracking.
3. **Isolated store, reuse code.** Reuse erpc's decode/`network.Forward`/header
   helpers, but a **dedicated in-memory map** — NOT the shared cache DAL, since many
   users have no cache configured. The module must work standalone.

## Shape

Per network (`sync.Map` keyed by networkId), built only when integrity is enabled
and a stateful check is active:

```
type chainView struct {
    mu            sync.RWMutex
    canonical     map[int64]string           // number → committed hash (the "pin")
    headers       map[string]*Header         // hash → header (content-addressed, immutable)
    headerOrder   []string                   // FIFO for header eviction
    receipts      map[string][]Receipt       // hash → canonical receipts (immutable)
    receiptsOrder []string                   // FIFO for receipts eviction
    tip           int64                      // highest number observed (eviction anchor)
    window        int                        // default 32, per-network override
    hInflight     map[string]*flight[*Header]  // singleflight: each header fetched once
    rInflight     map[string]*flight[[]Receipt] // singleflight: each block's receipts once
}
```

`headers` + `receipts` are content-addressed by block hash (immutable), window-bounded.
A generic `flight[T]`/`doOnce[T]` singleflight serves both so a block's header *and* its
receipts are each fetched at most once. Bodies/tx aren't cached — the checks recompute
those from the *response*; extensible behind the same window if a check ever needs them.

## Operations

- **observe(number, hash, header)** — pin + (optional) header upsert; runs the reorg
  check; evicts past the window. Fed from validated **block** responses.
- **observeNarrowAnchors(fin, body)** — pin `number→hash` from a **narrow** response
  (receipts/tx), but **only for finalized blocks** (`number ≤ fin`). A single narrow
  response must not redefine the canonical block for N at a jittery tip (thrash);
  finalized is settled, so this is safe and gives cross-receipt consistency even for
  blocks no `getBlock` pulled.
- **observeReceipts(hash, receipts)** — cache a block's canonical receipts by hash.
- **HashAt(number)** — the pin, for `hashStability` / `parentHashLinkage` / the
  `receiptVsBlock` pin check.
- **headerByHash / headerByNumber / receiptsByHash** — store hit → return in-memory;
  miss → `singleflight` → `network.Forward` **once** → cache → return.

### Contribution & dedup (Q1 / Q3)

- **All traffic contributes.** Block responses feed pin+header; narrow responses feed
  the finalized pin (above) and warm the by-hash receipts/header caches when a check
  resolves them. Observe runs only on a **passing** validation, so a rejected/corrupt
  response never poisons the store.
- **Fetched once, no matter how.** Two layers: (1) the ChainView singleflight collapses
  concurrent misses per key; (2) `network.Forward` itself multiplexes on `CacheHash()`
  (method+params, ignoring `IsInternal`), so an internal fetch coalesces with any
  concurrent **user** request for the same block. All check fetches go by hash → one
  keyspace → block N's header and receipts each hit the upstream once.

## Reorg algorithm (in Observe)

On `Observe(N, H)`:

- `canonical[N]` absent → set it.
- `canonical[N] == H` → no-op (pin unchanged).
- `canonical[N] == H′ ≠ H` → **reorg at N**: **adopt** `H` (trust `network.Forward`
  — decision 1) and **invalidate `canonical[N+1 …]`** (descendants of the old fork;
  they re-populate as the new fork extends).

No separate finality guard: below `tip − window` the pin + header are evicted, so
reorgs deeper than the window aren't tracked — rare, and a finalized block that
nonetheless changes is caught instead by the **deterministic** self-consistency
checks (`blockHashRecompute`) and the **pin-consistency** guard below. This keeps
the store cohesive with decision 1 (no internal consensus) — we never re-litigate
what the network already served.

## Check integration (re-anchor everything to the store)

- `hashStability` → `HashAt(N)` vs response hash.
- `parentHashLinkage` → response `parentHash` vs `HashAt(N-1)`.
- `receiptVsBlock` (two parts):
  1. **Pin consistency** — `receipt.blockHash` vs `HashAt(receipt.blockNumber)`. If
     they disagree, the receipt is from a different fork than the block we committed
     to serving → reject (no "mixed-up node"). Anchored to the pin, so a sub-second
     tip reorg can't false-positive a valid receipt. Skipped until the number is
     pinned.
  2. **Log corroboration** — fetch the canonical receipts **by the block hash**
     (`CanonicalReceipts(receipt.blockHash)`, immutable → no reorg race) and compare
     this tx's logs (count + logIndexes). Catches subtle receipt corruption (the
     Amoy logIndex underflow). Previously fetched **by number**, which raced the tip
     and produced the false positives we observed on the shadow.
- `receiptsRootRecompute` → `CanonicalHeader(receipt.blockHash).receiptsRoot`, now
  ChainView-backed (deduped + pinned).

One store → one consistent view → block and receipts can't disagree.

## Config

`integrity.reorgWindow` (int, blocks; default 32) on `IntegritySettings`
(project ⊕ network merge as usual). Bounds both reorg depth and retention.

## Memory

Pin + headers only, per network, window-bounded (`32 × (header ~1KB + hash)` ≈ tens
of KB/network at default). Built only when integrity is enabled. No shared cache.

## Migration

Replace `blockHistory` + the ad-hoc resolver with `ChainView`. `observeBlock`
becomes `ChainView.Observe`; `integrity.History`/`Resolver` interfaces back onto the
ChainView. Delete `integrity_history.go`'s standalone store. Re-run the shadow trace
sanity check — the `receiptVsBlock` rejects should become real (or vanish).
