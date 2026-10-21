package integrity

import (
	"bytes"
	"strings"

	"github.com/erpc/erpc/architecture/evm/logfilter"
	"github.com/erpc/erpc/common"
)

// EVM byte lengths shared by the shape checks. One home, so the per-field
// length validation that was copy-pasted across the receipt and block
// validators lives in exactly one place.
const (
	addressLen = 20  // 20-byte address
	hashLen    = 32  // 32-byte hash / topic
	bloomLen   = logfilter.BloomLen
	maxTopics  = 4   // LOG0..LOG4
)

// checkByteLen returns a Violation if a present hex field does not decode to
// exactly want bytes. An empty value is treated as absent (not a violation).
func checkByteLen(field, value string, want int) *Violation {
	if value == "" {
		return nil
	}
	b, err := common.HexToBytes(value)
	if err != nil {
		return failf("%s: invalid hex: %v", field, err)
	}
	if len(b) != want {
		return failf("%s: length invalid: %d bytes (want %d)", field, len(b), want)
	}
	return nil
}

// normHex canonicalizes a hex string for set-membership comparison ("0X1A" ==
// "0x1a").
func normHex(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// eqHex compares two hex strings case-insensitively and 0x-prefix tolerantly.
// Empty operands compare equal-to-anything (absent field is not a mismatch).
func eqHex(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	return strings.EqualFold(trimHexPrefix(a), trimHexPrefix(b))
}

// trimHexPrefix removes a leading "0x"/"0X". The prefix must be trimmed
// case-insensitively: JSON-RPC callers may send "0XABC…" (the identity checks
// accept that shape from request params) while nodes answer "0xabc…", and
// comparing one against the other with a case-sensitive trim would reject a
// perfectly valid response.
func trimHexPrefix(s string) string {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		return s[2:]
	}
	return s
}

// isZeroBloomHex reports whether a hex-encoded bloom is all zeros (any width).
func isZeroBloomHex(bloomHex string) bool {
	return strings.TrimLeft(strings.TrimPrefix(bloomHex, "0x"), "0") == ""
}

// bloomFromLogs recomputes the 2048-bit logs bloom from a slice of logs by
// folding each log's address and topics into the filter, mirroring the EVM
// bloom construction.
func bloomFromLogs(logs []Log) ([]byte, error) {
	bloom := make([]byte, bloomLen)
	for _, l := range logs {
		if l.Address != "" {
			ab, err := common.HexToBytes(l.Address)
			if err != nil {
				return nil, err
			}
			logfilter.BloomAdd(bloom, ab)
		}
		for _, t := range l.Topics {
			tb, err := common.HexToBytes(t)
			if err != nil {
				return nil, err
			}
			logfilter.BloomAdd(bloom, tb)
		}
	}
	return bloom, nil
}

// bloomsEqual reports whether a hex bloom equals a recomputed bloom byte slice.
func bloomsEqual(provided string, computed []byte) (bool, error) {
	pb, err := common.HexToBytes(provided)
	if err != nil {
		return false, err
	}
	return bytes.Equal(pb, computed), nil
}

// maxPlausibleEvmIndex bounds logIndex / transactionIndex to a value far above
// any real block (gas limits cap logs/txs per block to the tens of thousands)
// yet far below the ~4.29e9 values produced by a signed-32-bit integer
// underflow. A value at or above this is provably impossible on any real chain.
const maxPlausibleEvmIndex = uint64(1) << 24 // 16,777,216

// parseIndex parses a hex-quantity index ("0x…"). ok is false for an empty
// string (absent field — caller treats as not-present, not invalid).
func parseIndex(hexValue string) (n uint64, ok bool, err error) {
	if hexValue == "" {
		return 0, false, nil
	}
	v, e := common.HexToUint64(hexValue)
	if e != nil {
		return 0, true, e
	}
	return v, true, nil
}

// isImplausibleIndex reports whether a hex index is at or beyond the maximum any
// real block could produce. Unparseable values are left to other checks.
func isImplausibleIndex(hexValue string) bool {
	n, ok, err := parseIndex(hexValue)
	if !ok || err != nil {
		return false
	}
	return n >= maxPlausibleEvmIndex
}
