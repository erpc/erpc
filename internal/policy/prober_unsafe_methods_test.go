package policy

import "testing"

// probeExcluded shadow-mirrors sampled real traffic to excluded
// upstreams. Write methods must never be mirrored — for SVM that means
// sendTransaction/sendRawTransaction (duplicate wire broadcast) and
// requestAirdrop (double-airdrop). Reads stay mirrorable.
func TestIsProbeUnsafeMethod(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method string
		unsafe bool
	}{
		// SVM writes — gated.
		{"sendTransaction", true},
		{"sendRawTransaction", true},
		{"requestAirdrop", true},
		// SVM reads — mirrorable.
		{"simulateTransaction", false},
		{"getSlot", false},
		{"getAccountInfo", false},
		{"getBlock", false},
		// EVM writes — pre-existing behavior.
		{"eth_sendRawTransaction", true},
		{"eth_sendTransaction", true},
		{"eth_signTypedData_v4", true},
		{"personal_sign", true},
		// EVM reads.
		{"eth_getBalance", false},
		{"eth_call", false},
		// Unknown/empty → skip.
		{"", true},
	}
	for _, tc := range cases {
		if got := isProbeUnsafeMethod(tc.method); got != tc.unsafe {
			t.Errorf("isProbeUnsafeMethod(%q) = %v, want %v", tc.method, got, tc.unsafe)
		}
	}
}
