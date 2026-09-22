package integrity

import (
	"context"

	"github.com/erpc/erpc/common"
)

// Cross-block continuity checks link a block against blocks observed on earlier
// requests (via History). They are ReorgSensitive: near the tip a disagreement
// between two observations can be a benign reorg rather than corruption, so the
// per-finality invalidBehavior decides reject vs record. On finalized data,
// where reorgs cannot happen, a disagreement is corruption.
//
// Both apply to eth_getBlockByNumber ONLY. A by-NUMBER lookup asks "what is the
// chain at height N", so comparing the answer against the committed pin is
// exactly the question. A by-HASH lookup asks "give me this exact block": the
// identity of the response is already guaranteed (the caller named the hash,
// and blockHashRecompute proves the block is real and self-consistent), while
// its canonicality is not what was asked — retrieving orphaned-but-real blocks
// by hash is how indexers unwind reorgs. Applying continuity there rejects data
// the caller explicitly requested, and since an orphan hash is unobtainable on
// the canonical fork, no failover can satisfy it — the request just fails.
// Correspondingly, a by-hash response never feeds the pin (see
// observeBlockView), so skipping the check cannot let an orphan poison it.

func init() {
	// parentHashLinkage — block N's parentHash must equal the hash observed for
	// block N-1. A broken link means the chain we are being served does not
	// connect to the one we saw before.
	register(&Check{
		ID: "parentHashLinkage", Family: FamilyContinuity, Class: ReorgSensitive,
		Methods: []string{MethodGetBlockByNumber},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			hist := historyFrom(ctx)
			if hist == nil {
				return Skipped
			}
			h := d.Header()
			if h == nil || h.Number == "" || h.ParentHash == "" {
				return Skipped
			}
			n, err := common.HexToInt64(h.Number)
			if err != nil || n <= 0 {
				return Skipped
			}
			// Inside a FOLLOWED segment this check is redundant, and worse, it
			// is the weaker of two overlapping tests. hashStability compares the
			// response's own hash against the verified chain at this height,
			// which is the direct question; blockHashRecompute independently
			// proves the header hashes to the hash it claims. Together those
			// force parentHash to equal chain[n-1] — so linking against the
			// PARENT's pin adds no coverage there while carrying the whole cost
			// of the parent-pin dispute (every false positive this module has
			// produced came through this path). Outside a followed segment the
			// parent pin may be the only thing we hold, so the check still earns
			// its place.
			if seg := segmentFrom(ctx); seg != nil {
				if from, to, ok := seg.FollowedRange(); ok && n >= from && n <= to {
					return Skipped
				}
			}
			prev, ok := hist.HashAt(n - 1)
			if !ok {
				return Skipped // parent not observed yet — nothing to link against
			}
			if !eqHex(prev, h.ParentHash) {
				// Anchored to the cached pin for the PARENT — after a reorg the
				// stale parent pin breaks every honest child, so let the engine
				// re-confirm it before the verdict.
				return failf("block %d parentHash %s does not link to observed parent hash %s", n, h.ParentHash, prev).disputes(n - 1)
			}
			return nil
		},
	})

	// hashStability — a block number's hash must not change from what we
	// previously observed for it (a finalized block is immutable).
	register(&Check{
		ID: "hashStability", Family: FamilyContinuity, Class: ReorgSensitive,
		Methods: []string{MethodGetBlockByNumber},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			hist := historyFrom(ctx)
			if hist == nil {
				return Skipped
			}
			h := d.Header()
			if h == nil || h.Number == "" || h.Hash == "" {
				return Skipped
			}
			n, err := common.HexToInt64(h.Number)
			if err != nil {
				return Skipped
			}
			prev, ok := hist.HashAt(n)
			if !ok {
				return Skipped // number not observed yet — no pin to compare
			}
			if !eqHex(prev, h.Hash) {
				// Anchored to the cached pin for this number — a reorg makes the
				// pin stale, so the engine re-confirms it before the verdict.
				return failf("block %d hash %s differs from previously observed hash %s", n, h.Hash, prev).disputes(n)
			}
			return nil
		},
	})
}
