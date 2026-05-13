package evm

import (
	"github.com/erpc/erpc/util"
)

// classifyChangingStorageResult applies the two assertions of the
// "changingStorage" probe strategy:
//
//  1. non-empty result (catches "0x0 at head" — the literal failure mode from
//     the captured trace a96e11c29cdf5be9e26b05c7c15773b8 on Polygon).
//  2. result differs from the previous successful probe (catches the silent-
//     stale-prev-block failure mode where a node serves the value from block
//     N-1 while claiming to be at block N).
//
// First-cycle semantics: when prevResult is empty, assertion (2) is bypassed
// so a healthy upstream can advance stateReadyBlock on cold start without
// waiting for a second cycle.
//
// Returns (resultToStoreAsPrev, ready). When ready=false the caller should
// leave stateReadyBlock unchanged.
func classifyChangingStorageResult(resultBytes []byte, prevResult string) (string, bool) {
	if util.IsBytesEmptyish(resultBytes) {
		return "", false
	}
	result := string(resultBytes)
	if prevResult != "" && result == prevResult {
		return "", false
	}
	return result, true
}
