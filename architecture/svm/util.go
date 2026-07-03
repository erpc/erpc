package svm

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
func IsNonRetryableWriteMethod(method string) bool {
	switch method {
	case "sendTransaction", "sendRawTransaction", "requestAirdrop":
		return true
	}
	return false
}
