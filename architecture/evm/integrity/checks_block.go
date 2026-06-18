package integrity

import (
	"context"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
)

// Block family checks (eth_getBlockByNumber / eth_getBlockByHash), migrated from
// the inline validateBlock. They read the header and full transactions from the
// shared decode and reuse the same field primitives as the receipt checks.

// emptyTrieRoot is keccak256(RLP(empty)) — the transactionsRoot of a block with
// zero transactions. Some non-standard chains use the all-zero hash instead.
const (
	emptyTrieRoot = "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"
	zeroHash32    = "0x0000000000000000000000000000000000000000000000000000000000000000"
)

func init() {
	// transactionsRootConsistency — a non-empty transactionsRoot implies the
	// block carries transactions, and an empty trie root implies none (modulo
	// phantom/system transactions that don't enter the trie).
	register(&Check{
		ID: "transactionsRootConsistency", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetBlockByNumber, MethodGetBlockByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			h := d.Header()
			if h == nil || h.TransactionsRoot == "" {
				return nil
			}
			root := strings.ToLower(h.TransactionsRoot)
			isEmptyRoot := root == emptyTrieRoot || root == zeroHash32
			count := len(h.RawTransactions)
			if !isEmptyRoot && count == 0 {
				return failf("transactionsRoot %s is non-empty but block has 0 transactions; incomplete block data", h.TransactionsRoot)
			}
			if isEmptyRoot && count > 0 && !allPhantomRawTxs(h.RawTransactions) {
				return failf("transactionsRoot is empty trie root but block has %d non-phantom transactions; inconsistent block data", count)
			}
			return nil
		},
	})

	// headerFieldShapes — header hashes are 32 bytes and the logs bloom is 256.
	register(&Check{
		ID: "headerFieldShapes", Family: FamilyShape, Class: Deterministic,
		Methods: []string{MethodGetBlockByNumber, MethodGetBlockByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			h := d.Header()
			if h == nil {
				return nil
			}
			for _, f := range []struct {
				name, val string
				want      int
			}{
				{"block hash", h.Hash, hashLen},
				{"parentHash", h.ParentHash, hashLen},
				{"stateRoot", h.StateRoot, hashLen},
				{"transactionsRoot", h.TransactionsRoot, hashLen},
				{"receiptsRoot", h.ReceiptsRoot, hashLen},
				{"logsBloom", h.LogsBloom, bloomLen},
			} {
				if v := checkByteLen(f.name, f.val, f.want); v != nil {
					return v
				}
			}
			return nil
		},
	})

	// txFieldUniqueness — each transaction hash is 32 bytes and unique in the block.
	register(&Check{
		ID: "txFieldUniqueness", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetBlockByNumber, MethodGetBlockByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			seen := make(map[string]struct{})
			for i, tx := range d.Transactions() {
				if tx.Hash == "" {
					continue
				}
				if v := checkByteLen(fmt.Sprintf("tx %d hash", i), tx.Hash, hashLen); v != nil {
					return v
				}
				lo := strings.ToLower(tx.Hash)
				if _, dup := seen[lo]; dup {
					return failf("duplicate transaction hash in block: %s", tx.Hash)
				}
				seen[lo] = struct{}{}
			}
			return nil
		},
	})

	// txBlockInfo — each transaction's block coordinates match the enclosing
	// block and its index equals its position.
	register(&Check{
		ID: "txBlockInfo", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetBlockByNumber, MethodGetBlockByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			h := d.Header()
			if h == nil {
				return nil
			}
			for i, tx := range d.Transactions() {
				if !eqHex(tx.BlockHash, h.Hash) {
					return failf("tx %d: block hash mismatch", i)
				}
				if !eqInt(tx.BlockNumber, h.Number) {
					return failf("tx %d: block number mismatch", i)
				}
				if tx.TransactionIndex != "" {
					idx, err := common.HexToInt64(tx.TransactionIndex)
					if err != nil {
						return failf("tx %d: invalid transactionIndex hex: %v", i, err)
					}
					if idx != int64(i) {
						return failf("tx %d: transactionIndex mismatch: got %d", i, idx)
					}
				}
			}
			return nil
		},
	})
}

// allPhantomRawTxs reports whether every raw transaction is a phantom/system
// transaction (from=0x0, gas=0x0) that does not participate in the
// transactions trie. A hash-only entry is treated as a real transaction.
func allPhantomRawTxs(raw []any) bool {
	for _, t := range raw {
		obj, ok := t.(map[string]any)
		if !ok {
			return false
		}
		from, _ := obj["from"].(string)
		gas, _ := obj["gas"].(string)
		if !isZeroishHex(gas) || !isZeroishHex(from) {
			return false
		}
	}
	return true
}

// isZeroishHex reports whether a hex string represents zero (e.g. "0x", "0x0",
// "0x00"). An empty string is not zeroish (absent, not zero).
func isZeroishHex(h string) bool {
	if h == "" {
		return false
	}
	return strings.TrimLeft(strings.TrimPrefix(h, "0x"), "0") == ""
}
