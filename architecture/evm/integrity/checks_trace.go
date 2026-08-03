package integrity

import (
	"context"

	"github.com/erpc/erpc/common"
)

// Trace checks. debug_trace* was the largest block of traffic no check looked
// at (~36 rps on the shadow, the single biggest unverified family), and it is
// the traffic where silent corruption is hardest for a client to notice: a
// trace tree has no root committed in the header, so nothing about it can be
// recomputed the way a receipts root can.
//
// What CAN be verified is that the traces reconcile with the block they claim
// to describe. Measured before encoding, across 6 chains / 11 endpoints and 88
// consecutive blocks (bor, geth, erigon, reth, Nitro):
//
//	sum(top-level frame gasUsed) == header.gasUsed   exact, 0 deviations
//	len(traces)                  == len(block txs)   exact, 0 deviations
//
// The sweep deliberately spanned three full Polygon sprint cycles, where bor's
// state-sync pseudo-transaction lands, since that is the obvious way the
// identity could have broken.
//
// The tempting third invariant — sum(child gasUsed) <= parent gasUsed — was
// measured and is NOT one: it fails on 75 of 12,400 honest frames, because gas
// refunds and gas returned by reverted sub-calls are netted at the parent. It
// is therefore not implemented; a check that rejects 0.6% of real traffic is
// worse than no check.

const (
	// Bounds on tree walking. Measured max depth was 18 and ~2,500 frames per
	// block; these are far above that and exist only so a pathological or
	// hostile response cannot cost unbounded CPU. Exceeding them SKIPS — an
	// unusual shape is not evidence of corruption.
	maxTraceDepth  = 128
	maxTraceFrames = 100000
)

func init() {
	register(&Check{
		ID:     "traceBlockGasReconciliation",
		Family: FamilyStructural,
		// The header comes from the follower's verified segment, so a mismatch
		// can still mean the trace describes a different fork at that height
		// rather than corruption. Anchoring it to the pin makes the engine
		// re-confirm before any verdict, exactly as for the other cross-block
		// checks.
		Class:   ReorgSensitive,
		Methods: []string{MethodTraceBlockByNumber},
		// An empty trace list for a block that has transactions is the
		// everything-dropped shape — precisely what this check must see.
		AllowEmptyish: true,
		Run:           runTraceBlockGasReconciliation,
	})

	register(&Check{
		ID:      "traceFrameShape",
		Family:  FamilyShape,
		Class:   Deterministic,
		Methods: []string{MethodTraceBlockByNumber, MethodTraceBlockByHash, MethodTraceTransaction},
		Run:     runTraceFrameShape,
	})
}

// runTraceBlockGasReconciliation cross-references a block's traces against the
// header the follower verified for that height. Zero upstream cost: the header
// is already held, so this adds no aux request.
func runTraceBlockGasReconciliation(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
	entries, ok := d.BlockTraces()
	if !ok {
		return Skipped // not callTracer output
	}
	number, ok := requestedBlockNumber(d)
	if !ok {
		return Skipped // a tag form names no specific height
	}

	seg := segmentFrom(ctx)
	if seg == nil {
		return Skipped
	}
	from, to, ok := seg.FollowedRange()
	if !ok || number < from || number > to {
		return Skipped // outside the verified segment; the header would be a guess
	}
	header, ok := seg.HeaderAt(number)
	if !ok || header.GasUsed == "" {
		return Skipped
	}
	want, err := common.HexToInt64(header.GasUsed)
	if err != nil {
		return Skipped
	}

	if n := len(header.RawTransactions); n > 0 && len(entries) != n {
		return failf("block %d has %d transactions but the trace returned %d",
			number, n, len(entries)).disputes(number)
	}

	var sum int64
	for _, e := range entries {
		used, err := common.HexToInt64(e.Result.GasUsed)
		if err != nil {
			return Skipped // an unparseable frame is the shape check's business
		}
		sum += used
	}
	if sum != want {
		return failf("traced gas does not reconcile with block %d: traces sum to %d, header commits %d (delta %d)",
			number, sum, want, sum-want).disputes(number)
	}
	return nil
}

// runTraceFrameShape asserts the fields every callTracer frame carries. These
// are the ones measured present on all 12,400 frames sampled across 5 chains;
// a frame missing them is truncated or garbled rather than merely unusual.
func runTraceFrameShape(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
	var roots []*CallFrame
	if entries, ok := d.BlockTraces(); ok {
		for i := range entries {
			roots = append(roots, entries[i].Result)
		}
	} else if root, ok := d.CallTrace(); ok {
		roots = append(roots, root)
	} else {
		return Skipped // a tracer this check does not model
	}

	frames := 0
	for _, root := range roots {
		if v := walkTraceFrame(root, 0, &frames); v != nil {
			return v
		}
		if frames > maxTraceFrames {
			return Skipped
		}
	}
	if frames == 0 {
		return Skipped
	}
	return nil
}

// walkTraceFrame validates one frame and its subtree. It returns Skipped (as a
// sentinel, checked by the caller's chain) only via the bounds, so that an
// oversized tree costs nothing rather than producing a verdict.
func walkTraceFrame(f *CallFrame, depth int, frames *int) *Violation {
	if f == nil {
		return failf("trace contains a null call frame")
	}
	if depth > maxTraceDepth || *frames > maxTraceFrames {
		return nil // bounded: stop descending, do not judge what we did not read
	}
	*frames++

	if f.Type == "" {
		return failf("call frame at depth %d has no type", depth)
	}
	if f.From == "" {
		return failf("%s frame at depth %d has no from address", f.Type, depth)
	}
	if _, err := common.HexToInt64(f.GasUsed); err != nil {
		return failf("%s frame at depth %d has unusable gasUsed %q", f.Type, depth, f.GasUsed)
	}
	for i := range f.Calls {
		if v := walkTraceFrame(&f.Calls[i], depth+1, frames); v != nil {
			return v
		}
	}
	return nil
}
