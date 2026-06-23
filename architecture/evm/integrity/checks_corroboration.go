package integrity

import "context"

// Corroboration checks compare a narrow response against the canonical block
// aggregate, force-fetched through the resolver. They are reorg-sensitive: a
// reorg between the original fetch and the corroborating fetch can produce a
// benign mismatch near the tip (handled by the per-finality ReorgPolicy).

func init() {
	// receiptVsBlock — a single eth_getTransactionReceipt is corroborated
	// against the block's canonical receipts: the receipt must appear in the
	// block (by transaction hash) with the same logs (count + logIndexes) and
	// block hash. Catches subtle receipt corruption (e.g. a plausible-but-wrong
	// logIndex) that intrinsic checks cannot see without the block context.
	register(&Check{
		ID: "receiptVsBlock", Family: FamilyStructural, Class: ReorgSensitive,
		Methods: []string{MethodGetTransactionReceipt},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			receipts := d.Receipts()
			if len(receipts) != 1 {
				return nil
			}
			want := receipts[0]
			if want.BlockHash == "" {
				return nil
			}

			// 1. Consistency: the receipt's block must be the one we committed to
			// serving for its number (no "mixed-up node"). Anchored to the ChainView
			// pin — NOT a fresh number-fetch — so a sub-second tip reorg can't make a
			// valid receipt look wrong. Only checked once we have a committed view.
			if hist := historyFrom(ctx); hist != nil {
				if pin, known := hist.HashAt(d.BlockNumber()); known && !eqHex(want.BlockHash, pin) {
					return failf("receipt blockHash %s is not the committed block %s for number %d", want.BlockHash, pin, d.BlockNumber())
				}
			}

			// 2. Log corroboration: fetch the canonical receipts BY THE BLOCK HASH
			// (immutable — same block, no reorg race) and compare logs for this tx.
			// This is what catches subtle receipt corruption (e.g. a plausible-but-
			// wrong logIndex) the intrinsic checks can't see.
			resolver := resolverFrom(ctx)
			if resolver == nil {
				return nil // corroboration unavailable — no-op
			}
			block, ok := resolver.CanonicalReceipts(ctx, want.BlockHash)
			if !ok {
				return nil // couldn't fetch the canonical block — no-op
			}

			var match *Receipt
			for i := range block {
				if want.TransactionHash != "" && eqHex(block[i].TransactionHash, want.TransactionHash) {
					match = &block[i]
					break
				}
			}
			if match == nil {
				return failf("transaction %s not found in canonical block %s", want.TransactionHash, want.BlockHash)
			}
			if len(want.Logs) != len(match.Logs) {
				return failf("receipt has %d logs but canonical block has %d for this tx", len(want.Logs), len(match.Logs))
			}
			for j := range want.Logs {
				if !eqInt(want.Logs[j].LogIndex, match.Logs[j].LogIndex) {
					return failf("receipt log %d logIndex %q differs from canonical %q", j, want.Logs[j].LogIndex, match.Logs[j].LogIndex)
				}
			}
			return nil
		},
	})
}
