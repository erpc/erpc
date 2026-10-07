package blockstore

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A body whose transactions array repeats an entry must not validate against
// a verified header that lists each transaction once (set comparison would
// accept [A,B,A] for [A,B]), and a header that itself repeats a hash must be
// rejected at parse time so it can never become a verified header.
func TestValidatePayload_RejectsDuplicatedTransactions(t *testing.T) {
	const (
		blockHash  = "0x00000000000000000000000000000000000000000000000000000000000000b1"
		parentHash = "0x00000000000000000000000000000000000000000000000000000000000000b0"
		txA        = "0x000000000000000000000000000000000000000000000000000000000000000a"
		txB        = "0x000000000000000000000000000000000000000000000000000000000000000b"
	)
	block := func(txs ...interface{}) json.RawMessage {
		raw, err := json.Marshal(map[string]interface{}{
			"number": "0x10", "hash": blockHash, "parentHash": parentHash,
			"timestamp": "0x1", "transactions": txs,
		})
		require.NoError(t, err)
		return raw
	}
	full := func(h string) map[string]interface{} { return map[string]interface{}{"hash": h} }

	c := New(testOpts(), newMapStore(), nil, nil, nil)

	hdr, err := parseHeader(block(txA, txB), 0x10)
	require.NoError(t, err)
	require.NoError(t, c.validatePayload(PayloadBlock, hdr, block(full(txA), full(txB))), "exact body validates")
	require.Error(t, c.validatePayload(PayloadBlock, hdr, block(full(txA), full(txB), full(txA))),
		"a body repeating a transaction must not match a header listing it once")

	_, err = parseHeader(block(txA, txB, txA), 0x10)
	require.Error(t, err, "a header repeating a transaction hash must be rejected, so it can never verify a shorter body")
}
