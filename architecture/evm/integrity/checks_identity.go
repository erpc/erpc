package integrity

import (
	"context"

	"github.com/erpc/erpc/common"
)

// Identity checks: the response must be about the ENTITY the request asked
// for. Nothing else covers this — a mixed-up node returning a perfectly VALID
// transaction or receipt for the WRONG hash passes every intrinsic check
// (roots, shapes, signatures all verify; they just belong to another tx).

// requestedTxHash extracts the tx hash a by-hash request asked for. Empty when
// the params aren't a plausible 32-byte hex hash (never guess).
func requestedTxHash(d *Decoded) string {
	if len(d.reqParams) == 0 {
		return ""
	}
	s, _ := d.reqParams[0].(string)
	if len(s) != 66 || s[0] != '0' || (s[1] != 'x' && s[1] != 'X') {
		return ""
	}
	return s
}

func init() {
	// txByHashIdentity — eth_getTransactionByHash must return the transaction
	// that was requested: response.hash == params[0].
	register(&Check{
		ID: "txByHashIdentity", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetTransactionByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			want := requestedTxHash(d)
			if want == "" {
				return Skipped
			}
			txs := d.Transactions()
			if len(txs) != 1 || txs[0].Hash == "" {
				return Skipped
			}
			if !eqHex(txs[0].Hash, want) {
				return failf("response transaction hash %s is not the requested %s", txs[0].Hash, want)
			}
			return nil
		},
	})

	// receiptIdentity — eth_getTransactionReceipt must return the receipt of
	// the transaction that was requested: response.transactionHash == params[0].
	register(&Check{
		ID: "receiptIdentity", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetTransactionReceipt},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			want := requestedTxHash(d)
			if want == "" {
				return Skipped
			}
			receipts := d.Receipts()
			if len(receipts) != 1 || receipts[0].TransactionHash == "" {
				return Skipped
			}
			if !eqHex(receipts[0].TransactionHash, want) {
				return failf("response receipt is for transaction %s, not the requested %s", receipts[0].TransactionHash, want)
			}
			return nil
		},
	})

	// txPinConsistency — the block coordinates a mined transaction claims must
	// agree with the block we committed to serving for that number (the
	// ChainView pin). Pin-anchored → the engine re-confirms a stale pin before
	// the verdict (reorg-safe), mirroring receiptVsBlock's pin branch.
	register(&Check{
		ID: "txPinConsistency", Family: FamilyContinuity, Class: ReorgSensitive,
		Methods: []string{MethodGetTransactionByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			hist := historyFrom(ctx)
			if hist == nil {
				return Skipped
			}
			txs := d.Transactions()
			if len(txs) != 1 || txs[0].BlockHash == "" || txs[0].BlockNumber == "" {
				return Skipped // pending tx (null coords) or undecodable — not ours to judge
			}
			n, err := common.HexToInt64(txs[0].BlockNumber)
			if err != nil {
				return Skipped
			}
			pin, known := hist.HashAt(n)
			if !known {
				return Skipped
			}
			if !eqHex(txs[0].BlockHash, pin) {
				return failf("transaction claims block %s at height %d but the committed block is %s", txs[0].BlockHash, n, pin).disputes(n)
			}
			return nil
		},
	})
}
