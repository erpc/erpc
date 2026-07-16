package integrity

import (
	"context"
	"strings"

	"github.com/erpc/erpc/common"
)

// Cross-block continuity checks link a block against blocks observed on earlier
// requests (via History). They are ReorgSensitive: near the tip a disagreement
// between two observations can be a benign reorg rather than corruption, so the
// per-finality invalidBehavior decides reject vs record. On finalized data,
// where reorgs cannot happen, a disagreement is corruption.

// skipsByHashLookup reports whether this check is configured (param
// `byHashRequests: skip`) to exempt explicit by-hash lookups
// (eth_getBlockByHash) from canonical-pin comparison. A client that asks for a
// block by its exact hash receives exactly that block — the identity checks
// already pin response == requested — and whether that hash is canonical at
// its height is not the request's question: fetching orphaned-but-real blocks
// by hash is how indexers unwind reorgs, and one pool member that retains
// settled orphans would otherwise fail every such lookup for as long as
// clients retry it. The default ("validate") keeps the strict behavior:
// by-hash responses are held to the same one-consistent-fork view as by-number
// traffic.
func skipsByHashLookup(d *Decoded, cfg CheckConfig) bool {
	if d.method != MethodGetBlockByHash {
		return false
	}
	return strings.ToLower(strings.TrimSpace(cfg.param(common.IntegrityParamByHashRequests, ""))) == "skip"
}

func init() {
	// parentHashLinkage — block N's parentHash must equal the hash observed for
	// block N-1. A broken link means the chain we are being served does not
	// connect to the one we saw before.
	register(&Check{
		ID: "parentHashLinkage", Family: FamilyContinuity, Class: ReorgSensitive,
		Methods: []string{MethodGetBlockByNumber, MethodGetBlockByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			if skipsByHashLookup(d, cfg) {
				return Skipped
			}
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
			if skipsByHashLookup(d, cfg) {
				return Skipped
			}
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
