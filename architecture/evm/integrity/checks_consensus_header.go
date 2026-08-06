package integrity

import (
	"context"
	"math/big"
	"strings"
)

// emptyUnclesHash is keccak256(rlp([])) — the sha3Uncles of a block with no
// ommers. Every chain measured carries it (mainnet, base, polygon, arbitrum,
// bsc, hyperevm), because ommers are a proof-of-work artefact that no modern
// EVM chain produces. It is still declared per-architecture rather than assumed
// universal: a proof-of-work chain (Ethereum Classic and friends) legitimately
// has real ommers, and enforcing this there would reject every block.
// gasPerBlob is the EIP-4844 blob gas unit; blob space is sold in whole blobs.
const gasPerBlob = 131072

const emptyUnclesHash = "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347"

func init() {
	// headerConsensusInvariants — fields a chain's consensus pins to a constant.
	// A fabricated or mangled header has no reason to reproduce them, and
	// nothing else in the catalog looks at these fields at all, so this closes
	// a real gap for zero cost: no fetch, no parent, pure comparison.
	//
	// WHICH constants hold is a property of the consensus regime, not of the
	// EVM, and the differences are not guessable — measured across six chains,
	// sha3Uncles is empty on all of them, while difficulty is 0 only on
	// mainnet/OP-Stack/hyperevm (polygon 0x1, arbitrum 0x1, bsc 0x2) and the
	// nonce is zero everywhere except arbitrum. So each architecture declares
	// what it guarantees, and a chain with no declaration is not judged.
	register(&Check{
		ID: "headerConsensusInvariants", Family: FamilyShape, Class: Deterministic,
		Methods: []string{MethodGetBlockByNumber, MethodGetBlockByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			h := d.Header()
			if h == nil {
				return Skipped
			}
			checked := false

			if cfg.boolParam("emptyUncles", false) && h.Sha3Uncles != "" {
				checked = true
				if !strings.EqualFold(h.Sha3Uncles, emptyUnclesHash) {
					return failf("sha3Uncles %s is not the empty-ommers hash, which this chain's consensus fixes", h.Sha3Uncles)
				}
			}
			if cfg.boolParam("zeroDifficulty", false) && h.Difficulty != "" {
				checked = true
				if !isZeroHex(h.Difficulty) {
					return failf("difficulty %s is non-zero on a chain whose consensus fixes it at zero", h.Difficulty)
				}
			}
			if cfg.boolParam("zeroNonce", false) && h.Nonce != "" {
				checked = true
				if !isZeroHex(h.Nonce) {
					return failf("nonce %s is non-zero on a chain whose consensus fixes it at zero", h.Nonce)
				}
			}

			if cfg.boolParam("blobGasMultiple", false) && h.BlobGasUsed != "" {
				checked = true
				if v, ok := hexToBig(h.BlobGasUsed); ok {
					if new(big.Int).Mod(v, big.NewInt(gasPerBlob)).Sign() != 0 {
						return failf("blobGasUsed %s is not a whole number of blobs (%d gas each)", v.String(), gasPerBlob)
					}
				}
			}

			if !checked {
				// No invariant declared for this chain, or the fields are absent
				// from the response — nothing was verified, so say so rather
				// than reporting a pass that proved nothing.
				return Skipped
			}
			return nil
		},
	})
}

// isZeroHex reports whether a 0x quantity is zero, tolerating any width
// ("0x0", "0x0000000000000000").
func isZeroHex(s string) bool {
	t := trimHexPrefix(s)
	if t == "" {
		return false
	}
	return strings.Trim(t, "0") == ""
}
