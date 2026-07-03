package svm

import "testing"

func TestIsNonRetryableWriteMethod(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method string
		want   bool
	}{
		{"sendTransaction", true},
		{"sendRawTransaction", true},
		{"requestAirdrop", true},
		// Read-only / safe to fan out:
		{"simulateTransaction", false},
		{"getSlot", false},
		{"getBlock", false},
		{"getSignatureStatuses", false},
		// EVM names are handled by the evm package, not here:
		{"eth_sendRawTransaction", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsNonRetryableWriteMethod(tc.method); got != tc.want {
			t.Errorf("IsNonRetryableWriteMethod(%q) = %v, want %v", tc.method, got, tc.want)
		}
	}
}
