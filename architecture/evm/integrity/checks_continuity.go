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
				return nil
			}
			h := d.Header()
			if h == nil || h.Number == "" || h.ParentHash == "" {
				return nil
			}
			n, err := common.HexToInt64(h.Number)
			if err != nil || n <= 0 {
				return nil
			}
			prev, ok := hist.HashAt(n - 1)
			if !ok {
				return nil // parent not observed yet
			}
			if !eqHex(prev, h.ParentHash) {
				return failf("block %d parentHash %s does not link to observed parent hash %s", n, h.ParentHash, prev)
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
				return nil
			}
			h := d.Header()
			if h == nil || h.Number == "" || h.Hash == "" {
				return nil
			}
			n, err := common.HexToInt64(h.Number)
			if err != nil {
				return nil
			}
			prev, ok := hist.HashAt(n)
			if !ok {
				return nil
			}
			if !eqHex(prev, h.Hash) {
				return failf("block %d hash %s differs from previously observed hash %s", n, h.Hash, prev)
			}
			return nil
		},
	})
}
