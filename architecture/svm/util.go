package svm

import "strings"

// IsNonRetryableWriteMethod returns true for SVM methods that must never
// be dispatched to more than one upstream by automatic machinery (hedge,
// retry, probe mirroring). Solana tx broadcasts are same-signature
// idempotent on-chain, so the risk is not double-spend — it is duplicate
// broadcasts burning vendor quota and violating the documented
// single-broadcast guarantee; requestAirdrop mints per call and is
// genuinely non-idempotent. simulateTransaction is intentionally absent:
// it is read-only and safe to retry/hedge.
//
// The EVM twin is architecture/evm.IsNonRetryableWriteMethod; call sites
// that gate on method names check both (names cannot collide — EVM
// methods are eth_*-prefixed, SVM methods are bare).
// Matching is case-insensitive so a mis-cased method name cannot slip past the
// guard on any call site. An upstream would reject the mis-cased name anyway,
// but the guard must not be the thing that depends on that.
func IsNonRetryableWriteMethod(method string) bool {
	switch {
	case strings.EqualFold(method, "sendTransaction"),
		strings.EqualFold(method, "sendRawTransaction"),
		strings.EqualFold(method, "requestAirdrop"):
		return true
	}
	return false
}

// IsSingleDispatchWriteMethod reports whether a write method must reach at most
// ONE upstream per client call, so it can never be fanned out in parallel.
//
// This is the strict subset of IsNonRetryableWriteMethod that is NOT a
// transaction broadcast. The distinction matters because consensus treats the
// two oppositely: sending the SAME signed transaction to several nodes is still
// one transaction (consensus short-circuits to the first valid signature — see
// consensus.isTxBroadcastMethod), whereas requestAirdrop MINTS per call, so an
// N-participant fan-out mints N times and then disputes the N distinct
// signatures it produced.
//
// Kept as a literal method switch rather than an import from consensus/:
// erpc/ deliberately does not import consensus/ (dependency cycle), so the
// predicate has to live where the pipeline can reach it. util_test.go pins it
// against IsNonRetryableWriteMethod and the broadcast set so the two cannot
// drift apart silently.
func IsSingleDispatchWriteMethod(method string) bool {
	return strings.EqualFold(method, "requestAirdrop")
}
