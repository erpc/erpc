package integrity

import (
	"context"
	"encoding/json"
	"math/big"

	gethtypes "github.com/ethereum/go-ethereum/core/types"
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
				return nil
			}

			var fields map[string]json.RawMessage
			if err := json.Unmarshal(d.raw, &fields); err != nil {
				return nil // not an object → leave it to schemaConformance
			}
			for k := range fields {
				if _, ok := knownBlockFields[k]; !ok {
					return nil // custom/unknown field → header not fully understood; skip
				}
			}

			var gh gethtypes.Header
			if err := gh.UnmarshalJSON(d.raw); err != nil {
				return nil // missing a required header field → skip rather than false-flag
			}
			if got := gh.Hash().Hex(); !eqHex(got, h.Hash) {
				return failf("block hash %s does not match recomputed %s", h.Hash, got)
			}
			return nil
		},
	})
}
