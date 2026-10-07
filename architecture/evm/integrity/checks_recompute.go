package integrity

import (
	"context"
	"encoding/json"
	"math/big"

	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
)

// Cryptographic-commitment checks recompute a hash from the response's own
// fields and compare it to the value the response claims. They are
// Deterministic (finality-independent): a header that does not hash to its own
// claimed hash is corrupt no matter how recent the block is.
//
// These recomputations depend on the exact field set and encoding of a chain.
// To stay correct across the EVM ecosystem they run conservatively: only when
// the response is fully understood (every field is one the reference encoder
// knows). A chain that adds custom header fields is skipped, never false-flagged.

// knownBlockFields is the set of JSON keys a standard eth_getBlock* response
// carries: every field the reference header encoder knows (derived from it, so
// it tracks the encoder's version automatically) plus the RPC-added meta fields
// that are not part of the hash. A response with any other key describes a
// header the encoder does not fully understand, so the recompute is skipped
// rather than risk a false mismatch.
var knownBlockFields = deriveKnownBlockFields()

func deriveKnownBlockFields() map[string]struct{} {
	set := map[string]struct{}{
		// RPC-added fields that are not part of the header struct / hash
		"hash": {}, "size": {}, "totalDifficulty": {}, "transactions": {},
		"uncles": {}, "withdrawals": {},
	}
	// Every key the reference encoder emits is a field it knows how to hash.
	raw, err := (&gethtypes.Header{Number: big.NewInt(0), Difficulty: big.NewInt(0)}).MarshalJSON()
	if err == nil {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) == nil {
			for k := range m {
				set[k] = struct{}{}
			}
		}
	}
	return set
}

func init() {
	// blockHashRecompute — keccak(RLP(header)) must equal the reported hash.
	register(&Check{
		ID: "blockHashRecompute", Family: FamilyCommitment, Class: Deterministic,
		Methods: []string{MethodGetBlockByNumber, MethodGetBlockByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			h := d.Header()
			if h == nil || h.Hash == "" {
				return Skipped
			}

			gh, ok := d.referenceHeader()
			if !ok {
				return Skipped // not an object, custom field, or missing a required header field
			}
			if got := gh.Hash().Hex(); !eqHex(got, h.Hash) {
				return failf("block hash %s does not match recomputed %s", h.Hash, got)
			}
			return nil
		},
	})

	// transactionsRootRecompute — the Merkle-Patricia root of the block's full
	// transactions must equal the header's transactionsRoot. Stronger than the
	// structural transactionsRootConsistency (which only checks empty/non-empty):
	// it catches tampered or substituted transaction bodies. Conservative — if
	// the response carries transaction hashes only, or any transaction the
	// reference decoder can't fully model (proven by its hash recomputing), the
	// whole recompute is skipped rather than risk a false mismatch.
	register(&Check{
		ID: "transactionsRootRecompute", Family: FamilyCommitment, Class: Deterministic,
		Methods: []string{MethodGetBlockByNumber, MethodGetBlockByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			h := d.Header()
			if h == nil || h.TransactionsRoot == "" {
				return Skipped
			}
			txs, ok := d.referenceTransactions()
			if !ok {
				return Skipped // hash-only list, or a tx the reference decoder can't fully model
			}

			if got := gethtypes.DeriveSha(txs, trie.NewStackTrie(nil)).Hex(); !eqHex(got, h.TransactionsRoot) {
				return failf("transactionsRoot %s does not match recomputed %s", h.TransactionsRoot, got)
			}
			return nil
		},
	})

	// receiptsRootRecompute — the Merkle-Patricia root of a block's receipts must
	// equal the header's receiptsRoot. Cross-method: the receipts come from the
	// response, the committed root is force-fetched from the header (by block
	// hash, so it is the same immutable block — hence Deterministic). Conservative
	// via a receipt field-allowlist: a receipt carrying any field the reference
	// decoder doesn't know (e.g. an L2 with custom receipt fields) skips the whole
	// recompute rather than false-flag.
	register(&Check{
		ID: "receiptsRootRecompute", Family: FamilyCommitment, Class: Deterministic,
		Methods: []string{MethodGetBlockReceipts},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			resolver := resolverFrom(ctx)
			if resolver == nil {
				return Skipped
			}
			var rawReceipts []json.RawMessage
			if err := json.Unmarshal(d.raw, &rawReceipts); err != nil || len(rawReceipts) == 0 {
				return Skipped
			}
			ref := d.BlockRef()
			if ref == "" {
				return Skipped
			}
			header, ok := resolver.CanonicalHeader(ctx, ref)
			if !ok || header == nil || header.ReceiptsRoot == "" {
				return Skipped
			}

			typed := make(gethtypes.Receipts, 0, len(rawReceipts))
			for _, rr := range rawReceipts {
				var fields map[string]json.RawMessage
				if json.Unmarshal(rr, &fields) != nil {
					return Skipped
				}
				for k := range fields {
					if _, ok := knownReceiptFields[k]; !ok {
						return Skipped // custom receipt field → not fully modeled
					}
				}
				var rcpt gethtypes.Receipt
				if rcpt.UnmarshalJSON(rr) != nil {
					return Skipped
				}
				typed = append(typed, &rcpt)
			}

			if got := gethtypes.DeriveSha(typed, trie.NewStackTrie(nil)).Hex(); !eqHex(got, header.ReceiptsRoot) {
				return failf("receiptsRoot %s does not match recomputed %s", header.ReceiptsRoot, got)
			}
			return nil
		},
	})
}

// knownReceiptFields is the set of JSON keys a standard receipt carries:
// everything the reference receipt encoder knows (derived from it) plus the
// RPC-added meta fields. A receipt with any other key is not fully modeled here.
var knownReceiptFields = deriveKnownReceiptFields()

func deriveKnownReceiptFields() map[string]struct{} {
	set := map[string]struct{}{
		// RPC-added fields not present on the consensus receipt struct
		"from": {}, "to": {},
	}
	// A fully-populated receipt so MarshalJSON emits every field (an empty one
	// omits the zero-valued ones like blockHash/transactionHash).
	sample := &gethtypes.Receipt{
		Type:              gethtypes.DynamicFeeTxType,
		Status:            gethtypes.ReceiptStatusSuccessful,
		CumulativeGasUsed: 1,
		TxHash:            gethcommon.Hash{0x1},
		ContractAddress:   gethcommon.Address{0x1},
		GasUsed:           1,
		EffectiveGasPrice: big.NewInt(1),
		BlobGasUsed:       1,
		BlobGasPrice:      big.NewInt(1),
		BlockHash:         gethcommon.Hash{0x1},
		BlockNumber:       big.NewInt(1),
		TransactionIndex:  1,
	}
	if raw, err := sample.MarshalJSON(); err == nil {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) == nil {
			for k := range m {
				set[k] = struct{}{}
			}
		}
	}
	return set
}

// referenceHeader decodes the block with the reference (geth) header decoder,
// provided every top-level key is one it understands. ok=false means the
// recompute must skip: not an object, a custom/unknown field (header not
// fully understood), or a missing/invalid required header field.
//
// On the shared fast path (a valid, plain, duplicate-free document) the key
// set comes from the shared split and geth decodes a header-only rebuild of
// the object: the bodies it would only skip over are left out, so the decode
// is exact while scanning ~1.5KB instead of the whole block.
func (d *Decoded) referenceHeader() (*gethtypes.Header, bool) {
	d.Header()
	var input []byte
	if doc := d.doc; doc != nil {
		for _, m := range doc.members {
			if _, ok := knownBlockFields[m.key]; !ok {
				return nil, false
			}
		}
		input = doc.rebuildHeaderDoc()
	} else {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(d.raw, &fields); err != nil {
			return nil, false
		}
		for k := range fields {
			if _, ok := knownBlockFields[k]; !ok {
				return nil, false
			}
		}
		input = d.raw
	}
	var gh gethtypes.Header
	if err := gh.UnmarshalJSON(input); err != nil {
		return nil, false
	}
	return &gh, true
}

// referenceTransactions decodes every block transaction with the reference
// decoder and proves each one fully modeled (its recomputed hash equals the
// claimed hash). ok=false (skip) when the list is absent/empty, hash-only, or
// any tx fails either test.
//
// On the shared fast path each transaction is decoded from the shared split
// (decodeTxSonic's port of geth's decoder, which also yields the claimed
// hash); otherwise the original per-check decode runs.
func (d *Decoded) referenceTransactions() (gethtypes.Transactions, bool) {
	d.Header()
	if d.doc != nil {
		raws := d.doc.txs
		if len(raws) == 0 {
			return nil, false
		}
		split := d.splitTxs()
		txs := make(gethtypes.Transactions, 0, len(raws))
		for i, rawTx := range raws {
			if rawTx[0] == '"' {
				return nil, false // hashes-only response → cannot recompute
			}
			var (
				tx      *gethtypes.Transaction
				claimed string
				err     error
			)
			if st := split[i]; st != nil && st.ok {
				tx, claimed, err = st.decode()
			} else {
				tx, claimed, err = decodeTxGeth(rawBytes(rawTx))
			}
			if err != nil || claimed == "" || !eqHex(tx.Hash().Hex(), claimed) {
				return nil, false // tx not fully modeled → a correct root can't be computed
			}
			txs = append(txs, tx)
		}
		return txs, true
	}

	var block struct {
		Transactions []json.RawMessage `json:"transactions"`
	}
	if err := json.Unmarshal(d.raw, &block); err != nil || len(block.Transactions) == 0 {
		return nil, false
	}
	txs := make(gethtypes.Transactions, 0, len(block.Transactions))
	for _, rawTx := range block.Transactions {
		if len(rawTx) == 0 || rawTx[0] == '"' {
			return nil, false
		}
		tx, claimed, err := decodeTxGeth(rawTx)
		if err != nil || claimed == "" || !eqHex(tx.Hash().Hex(), claimed) {
			return nil, false
		}
		txs = append(txs, tx)
	}
	return txs, true
}
