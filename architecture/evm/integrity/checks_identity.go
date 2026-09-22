package integrity

import (
	"context"

	"github.com/erpc/erpc/common"
)

// Identity checks: the response must be about the ENTITY the request asked
// for. Nothing else covers this — a mixed-up node returning a perfectly VALID
// transaction, receipt, or block for the WRONG hash passes every intrinsic
// check (roots, shapes, signatures all verify; they just belong to another
// entity).

// requestedHash extracts the 32-byte hash a by-hash request asked for. Empty
// when the params aren't a plausible 32-byte hex hash (never guess).
func requestedHash(d *Decoded) string {
	if len(d.reqParams) == 0 {
		return ""
	}
	s, _ := d.reqParams[0].(string)
	if len(s) != 66 || s[0] != '0' || (s[1] != 'x' && s[1] != 'X') {
		return ""
	}
	return s
}

// requestedBlockNumber extracts the block number an eth_getBlockByNumber
// request explicitly asked for. Returns ok=false for the tag forms
// ("latest"/"finalized"/"safe"/"pending"/"earliest"), where the caller named no
// specific height and the answer is whatever the chain currently says — that is
// the served-tip/enforceHighestBlock layer's question, not identity's.
func requestedBlockNumber(d *Decoded) (int64, bool) {
	if len(d.reqParams) == 0 {
		return 0, false
	}
	s, _ := d.reqParams[0].(string)
	if len(s) < 3 || s[0] != '0' || (s[1] != 'x' && s[1] != 'X') {
		return 0, false
	}
	n, err := common.HexToInt64(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func init() {
	// blockByNumberIdentity — eth_getBlockByNumber with an explicit height must
	// return THAT height. Nothing else covers this: Layer-1 enforceHighestBlock
	// only inspects the "latest"/"finalized" tags and returns early for explicit
	// numbers, and continuity anchors on the number the RESPONSE claims — so a
	// node answering with a different (but genuine and canonical) block passes
	// every check, and the wrong block then gets cached under the requested key.
	//
	// Compared numerically, never as strings: "0x0123" and "0x123" are the same
	// height, and rejecting that pair would be rejecting valid data.
	register(&Check{
		ID: "blockByNumberIdentity", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetBlockByNumber},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			want, ok := requestedBlockNumber(d)
			if !ok {
				return Skipped // a tag, or params we don't model — never guess
			}
			h := d.Header()
			if h == nil || h.Number == "" {
				return Skipped
			}
			got, err := common.HexToInt64(h.Number)
			if err != nil {
				return Skipped
			}
			if got != want {
				return failf("response is block %d but block %d was requested", got, want)
			}
			return nil
		},
	})

	// blockByHashIdentity — eth_getBlockByHash must return the block that was
	// requested: response.hash == params[0].
	//
	// This is the by-hash counterpart of the tx/receipt identity checks, and on
	// this method it is the ONLY thing standing between a caller and a
	// wrong-but-valid block: every other check verifies the response against
	// itself (blockHashRecompute proves the header hashes to its own claimed
	// hash — of whatever block it happens to be), and the continuity pair
	// deliberately does not judge by-hash lookups (see checks_continuity.go).
	// Cheap, deterministic, and it cannot false-positive: the caller named the
	// hash, so returning a different one is unambiguously wrong.
	register(&Check{
		ID: "blockByHashIdentity", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetBlockByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			want := requestedHash(d)
			if want == "" {
				return Skipped
			}
			h := d.Header()
			if h == nil || h.Hash == "" {
				return Skipped
			}
			if !eqHex(h.Hash, want) {
				return failf("response block hash %s is not the requested %s", h.Hash, want)
			}
			return nil
		},
	})

	// txByHashIdentity — eth_getTransactionByHash must return the transaction
	// that was requested: response.hash == params[0].
	register(&Check{
		ID: "txByHashIdentity", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetTransactionByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			want := requestedHash(d)
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
			want := requestedHash(d)
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
