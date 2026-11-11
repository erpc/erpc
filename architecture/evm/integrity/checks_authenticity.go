package integrity

import (
	"context"
	"encoding/json"

	gethtypes "github.com/ethereum/go-ethereum/core/types"
)

// Per-item authenticity checks recover a cryptographic fact from a transaction's
// own signature and compare it to what the response claims. Deterministic and
// finality-independent: a signature that does not recover the claimed sender is
// corrupt regardless of block age.
//
// Safety across chains: recovery is trusted only when the reference decoder
// fully models the transaction — proven by the transaction hash recomputing to
// the claimed hash. A system/deposit tx, an unknown type, or a chain with custom
// signing fails that gate and is skipped, never false-flagged.

func init() {
	// senderRecovery — ecrecover(signature) must equal the reported `from`.
	register(&Check{
		ID: "senderRecovery", Family: FamilyAuthenticity, Class: Deterministic,
		Methods: []string{MethodGetTransactionByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			var meta struct {
				From string `json:"from"`
				Hash string `json:"hash"`
				R    string `json:"r"`
			}
			if json.Unmarshal(d.raw, &meta) != nil {
				return nil
			}
			// Need a claimed sender and a signature; unsigned/system txs skip.
			if meta.From == "" || meta.Hash == "" || meta.R == "" {
				return nil
			}

			var tx gethtypes.Transaction
			if tx.UnmarshalJSON(d.raw) != nil {
				return nil
			}
			// Completeness gate: only trust recovery when the decoder fully models
			// the tx — i.e. its hash recomputes to the claimed hash.
			if !eqHex(tx.Hash().Hex(), meta.Hash) {
				return nil
			}

			// A pre-EIP-155 legacy tx (v=27/28) carries no chain id: geth reports
			// ChainId()=0, and its chain-id signers panic on a non-positive id.
			signer := gethtypes.Signer(gethtypes.HomesteadSigner{})
			if tx.Protected() {
				chainID := tx.ChainId()
				if chainID == nil || chainID.Sign() <= 0 {
					return Skipped
				}
				signer = gethtypes.LatestSignerForChainID(chainID)
			}
			recovered, err := gethtypes.Sender(signer, &tx)
			if err != nil {
				return nil
			}
			if !eqHex(recovered.Hex(), meta.From) {
				return failf("transaction %s sender %s does not match recovered signer %s", meta.Hash, meta.From, recovered.Hex())
			}
			return nil
		},
	})
}
