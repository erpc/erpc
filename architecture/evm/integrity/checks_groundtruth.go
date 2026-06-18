package integrity

import (
	"bytes"
	"context"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
)

// Caller-provided checks: these validate a response against expected values the
// caller supplies (library mode today; the authoritative resolver later). They
// are kept separate from the intrinsic checks because their inputs come from
// outside the response.

// GroundTruthTx is the package-local expected-transaction shape used by
// receiptTransactionMatch, so the integrity package stays decoupled from the
// request-directive types. The adapter converts into this.
type GroundTruthTx struct {
	Hash             []byte
	To               []byte  // nil/empty => contract creation
	TransactionIndex *uint32 // expected position, if known
}

func init() {
	// expectedBlock — every receipt's blockHash/blockNumber matches the value
	// the caller expected for the requested block. Params: "hash", "number".
	register(&Check{
		ID: "expectedBlock", Family: FamilyStructural, Class: ReorgSensitive,
		Methods: []string{MethodGetBlockReceipts},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			wantHash := cfg.param("hash", "")
			wantNumber := cfg.param("number", "")
			for i := range d.Receipts() {
				r := &d.receipts[i]
				if wantHash != "" && r.BlockHash != "" && !eqHex(r.BlockHash, wantHash) {
					return failf("receipt %d blockHash %s != expected %s", i, r.BlockHash, wantHash)
				}
				if wantNumber != "" && r.BlockNumber != "" {
					bn, err := common.HexToInt64(r.BlockNumber)
					if err != nil {
						return failf("receipt %d invalid blockNumber hex: %v", i, err)
					}
					want, err := strconv.ParseInt(wantNumber, 10, 64)
					if err == nil && bn != want {
						return failf("receipt %d blockNumber %d != expected %d", i, bn, want)
					}
				}
			}
			return nil
		},
	})

	// receiptsCount — the number of receipts matches an exact count or a floor.
	// Params: "exact", "atLeast" (decimal).
	register(&Check{
		ID: "receiptsCount", Family: FamilyStructural, Class: ReorgSensitive,
		Methods: []string{MethodGetBlockReceipts},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			n := int64(len(d.Receipts()))
			if s := cfg.param("exact", ""); s != "" {
				if want, err := strconv.ParseInt(s, 10, 64); err == nil && n != want {
					return failf("receipts count %d != expected %d", n, want)
				}
			}
			if s := cfg.param("atLeast", ""); s != "" {
				if want, err := strconv.ParseInt(s, 10, 64); err == nil && n < want {
					return failf("receipts count %d below expected minimum %d", n, want)
				}
			}
			return nil
		},
	})

	// receiptTransactionMatch — each receipt corresponds positionally to a
	// ground-truth transaction (hash match), and, with contractCreation=true,
	// the receipt's contractAddress is present iff the tx has no recipient.
	// Data: []GroundTruthTx.
	register(&Check{
		ID: "receiptTransactionMatch", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetBlockReceipts},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			gt, _ := cfg.Data.([]GroundTruthTx)
			if len(gt) == 0 {
				return nil
			}
			receipts := d.Receipts()
			if len(gt) != len(receipts) {
				return failf("receipts count %d != ground-truth transactions count %d", len(receipts), len(gt))
			}
			checkCreation := cfg.boolParam("contractCreation", false)
			for i := range receipts {
				r := &receipts[i]
				tx := gt[i]
				if tx.Hash == nil {
					continue
				}
				rHash, err := common.HexToBytes(r.TransactionHash)
				if err != nil {
					return failf("receipt %d: invalid transactionHash hex: %v", i, err)
				}
				if !bytes.Equal(rHash, tx.Hash) {
					return failf("receipt %d transactionHash mismatch", i)
				}
				if checkCreation {
					isCreation := len(tx.To) == 0
					hasContract := strings.TrimSpace(r.ContractAddress) != ""
					if isCreation && !hasContract {
						return failf("receipt %d: contract-creation tx but no contractAddress", i)
					}
					if !isCreation && hasContract {
						return failf("receipt %d: non-creation tx but has contractAddress", i)
					}
					if isCreation && hasContract {
						if v := checkByteLen("contractAddress", r.ContractAddress, addressLen); v != nil {
							return v
						}
					}
				}
			}
			return nil
		},
	})
}
