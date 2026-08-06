package integrity

import "context"

// indexMagnitude — no logIndex / transactionIndex may exceed a physically
// possible block index. Catches 32-bit underflow garbage (e.g. 0xfffffff7 ==
// 4294967287). This is an always-on sanity check for the receipt/log family;
// it supersedes the standalone recursive walk that previously lived inline.
func init() {
	register(&Check{
		ID:      "indexMagnitude",
		Family:  FamilyShape,
		Class:   Deterministic,
		Methods: []string{MethodGetTransactionReceipt, MethodGetBlockReceipts, MethodGetLogs},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			for i := range d.Logs() {
				l := &d.logs[i]
				if isImplausibleIndex(l.LogIndex) {
					return failf("logIndex %q exceeds maximum plausible block index %d (likely a 32-bit underflow from upstream)", l.LogIndex, maxPlausibleEvmIndex)
				}
				if isImplausibleIndex(l.TransactionIndex) {
					return failf("log transactionIndex %q exceeds maximum plausible block index %d", l.TransactionIndex, maxPlausibleEvmIndex)
				}
			}
			for i := range d.Receipts() {
				if isImplausibleIndex(d.receipts[i].TransactionIndex) {
					return failf("transactionIndex %q exceeds maximum plausible block index %d", d.receipts[i].TransactionIndex, maxPlausibleEvmIndex)
				}
			}
			return nil
		},
	})
}
