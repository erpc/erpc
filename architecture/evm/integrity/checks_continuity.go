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

func init() {
	// parentHashLinkage — block N's parentHash must equal the hash observed for
	// block N-1. A broken link means the chain we are being served does not
	// connect to the one we saw before.
	register(&Check{
		ID: "parentHashLinkage", Family: FamilyContinuity, Class: ReorgSensitive,
		Methods: []string{MethodGetBlockByNumber, MethodGetBlockByHash},
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
		Methods: []string{MethodGetBlockByNumber, MethodGetBlockByHash},
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
