package integrity

import (
	"context"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
)

// Receipt/log family checks. These migrate the structural and shape validators
// that previously lived inline in validateGetBlockReceipts, each now a small
// single-responsibility unit over the shared decode (decode.go) and the shared
// field primitives (fields.go) — so the field-length and bloom logic exists in
// exactly one place.

func init() {
	// txHashUniqueness — transaction hashes are unique within the block. With
	// strict=true an empty hash is itself a violation (the stricter opt-in).
	register(&Check{
		ID: "txHashUniqueness", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetBlockReceipts},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			strict := cfg.boolParam("strict", false)
			seen := make(map[string]struct{})
			for i := range d.Receipts() {
				h := d.receipts[i].TransactionHash
				if h == "" {
					if strict {
						return failf("empty transactionHash at receipt %d", i)
					}
					continue
				}
				lo := strings.ToLower(h)
				if _, dup := seen[lo]; dup {
					return failf("duplicate transactionHash %s", h)
				}
				seen[lo] = struct{}{}
			}
			return nil
		},
	})

	// sameBlockHash — every receipt reports the same blockHash (mixed-block guard).
	register(&Check{
		ID: "sameBlockHash", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetBlockReceipts},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			first := ""
			for i := range d.Receipts() {
				bh := d.receipts[i].BlockHash
				if bh == "" {
					continue
				}
				lo := strings.ToLower(strings.TrimPrefix(bh, "0x"))
				if first == "" {
					first = lo
				} else if lo != first {
					return failf("receipt %d blockHash %s differs from first; receipts span multiple blocks", i, bh)
				}
			}
			return nil
		},
	})

	// transactionIndexConsistency — receipt.transactionIndex equals its position.
	register(&Check{
		ID: "transactionIndexConsistency", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetBlockReceipts},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			for i := range d.Receipts() {
				ti := d.receipts[i].TransactionIndex
				if ti == "" {
					continue
				}
				idx, err := common.HexToInt64(ti)
				if err != nil {
					return failf("invalid transactionIndex hex at receipt %d: %v", i, err)
				}
				if idx != int64(i) {
					return failf("transactionIndex mismatch at receipt %d: got %d", i, idx)
				}
			}
			return nil
		},
	})

	// logFieldShapes — each log's address is 20 bytes, each topic 32 bytes, with
	// a bounded topic count.
	register(&Check{
		ID: "logFieldShapes", Family: FamilyShape, Class: Deterministic,
		Methods: []string{MethodGetBlockReceipts, MethodGetTransactionReceipt, MethodGetLogs},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			for i := range d.Logs() {
				l := &d.logs[i]
				if v := checkByteLen(fmt.Sprintf("log %d address", i), l.Address, addressLen); v != nil {
					return v
				}
				if len(l.Topics) > maxTopics {
					return failf("log %d: too many topics: %d", i, len(l.Topics))
				}
				for k, t := range l.Topics {
					if v := checkByteLen(fmt.Sprintf("log %d topic %d", i, k), t, hashLen); v != nil {
						return v
					}
				}
			}
			return nil
		},
	})

	// logMetadata — each log's block/tx coordinates match its parent receipt
	// (catches a provider mixing data from different blocks/txs).
	register(&Check{
		ID: "logMetadata", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetBlockReceipts},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			for i := range d.Receipts() {
				r := &d.receipts[i]
				for j := range r.Logs {
					l := &r.Logs[j]
					if !eqHex(l.BlockHash, r.BlockHash) {
						return failf("receipt %d log %d: blockHash mismatch", i, j)
					}
					if !eqInt(l.BlockNumber, r.BlockNumber) {
						return failf("receipt %d log %d: blockNumber mismatch", i, j)
					}
					if !eqHex(l.TransactionHash, r.TransactionHash) {
						return failf("receipt %d log %d: transactionHash mismatch", i, j)
					}
					if !eqInt(l.TransactionIndex, r.TransactionIndex) {
						return failf("receipt %d log %d: transactionIndex mismatch", i, j)
					}
				}
			}
			return nil
		},
	})

	// bloomEmptiness — a receipt's logs bloom is zero iff it has no logs.
	register(&Check{
		ID: "bloomEmptiness", Family: FamilyShape, Class: Deterministic,
		Methods: []string{MethodGetBlockReceipts},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			for i := range d.Receipts() {
				r := &d.receipts[i]
				zero := isZeroBloomHex(r.LogsBloom)
				has := len(r.Logs) > 0
				if !zero && !has {
					return failf("receipt %d: non-zero logsBloom but 0 logs", i)
				}
				if zero && has {
					return failf("receipt %d: zero logsBloom but %d logs", i, len(r.Logs))
				}
			}
			return nil
		},
	})

	// bloomMatch — a receipt's logs bloom recomputed from its logs matches the
	// declared bloom (cryptographic-ish commitment over address+topics).
	register(&Check{
		ID: "bloomMatch", Family: FamilyCommitment, Class: Deterministic,
		Methods: []string{MethodGetBlockReceipts},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			for i := range d.Receipts() {
				r := &d.receipts[i]
				if len(r.Logs) == 0 {
					continue
				}
				computed, err := bloomFromLogs(r.Logs)
				if err != nil {
					return failf("receipt %d: invalid log field for bloom recompute: %v", i, err)
				}
				eq, err := bloomsEqual(r.LogsBloom, computed)
				if err != nil {
					return failf("receipt %d: invalid logsBloom hex: %v", i, err)
				}
				if !eq {
					return failf("receipt %d: logsBloom does not match logs content", i)
				}
			}
			return nil
		},
	})

	// logIndexContiguity — logIndex across all logs of the block is exactly
	// 0..N-1, strictly increasing, no gaps; a missing logIndex is a violation.
	register(&Check{
		ID: "logIndexContiguity", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetBlockReceipts},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			var expected uint64
			for i := range d.Logs() {
				lix := d.logs[i].LogIndex
				if lix == "" {
					return failf("missing logIndex at log %d", i)
				}
				v, err := common.HexToUint64(lix)
				if err != nil {
					return failf("invalid logIndex hex at log %d: %v", i, err)
				}
				if v != expected {
					return failf("logIndex not contiguous at log %d: expected %d got %d", i, expected, v)
				}
				expected++
			}
			return nil
		},
	})
}

// eqInt compares two hex-quantity strings by value, tolerating width
// differences (0x0 == 0x00). Empty operands are treated as equal (absent).
func eqInt(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	av, ae := common.HexToInt64(a)
	bv, be := common.HexToInt64(b)
	if ae != nil || be != nil {
		return false
	}
	return av == bv
}
