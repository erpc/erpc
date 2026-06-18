package integrity

import "github.com/erpc/erpc/common"

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
