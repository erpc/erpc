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
			resolver := resolverFrom(ctx)
			if resolver == nil {
				return nil // corroboration unavailable — no-op
			}
			receipts := d.Receipts()
			if len(receipts) != 1 {
				return nil
			}
			want := receipts[0]
			ref := d.BlockRef()
			if ref == "" {
				return nil
			}
			block, ok := resolver.CanonicalReceipts(ctx, ref)
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
				return failf("transaction %s not found in canonical block %s", want.TransactionHash, ref)
			}
			if !eqHex(want.BlockHash, match.BlockHash) {
				return failf("receipt blockHash %s differs from canonical %s", want.BlockHash, match.BlockHash)
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
