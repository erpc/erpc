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

// TestIsSingleDispatchWriteMethod_ParityWithBroadcastSet pins the invariant the
// consensus gate depends on: single-dispatch is exactly the non-broadcast
// subset of the non-retryable write set. If someone adds a method to one table
// and not the other, either a mint gets fanned out across consensus
// participants or a tx broadcast loses its first-valid short-circuit.
//
// The broadcast names are duplicated here rather than imported because
// consensus.isTxBroadcastMethod is unexported and erpc/ cannot import
// consensus/ (dependency cycle). This test IS the coupling.
func TestIsSingleDispatchWriteMethod_ParityWithBroadcastSet(t *testing.T) {
	t.Parallel()
	// Mirror of consensus.isTxBroadcastMethod's SVM entries.
	broadcast := map[string]bool{"sendTransaction": true, "sendRawTransaction": true}

	for _, m := range []string{"sendTransaction", "sendRawTransaction", "requestAirdrop"} {
		if !IsNonRetryableWriteMethod(m) {
			t.Fatalf("test premise broken: %q must be a non-retryable write method", m)
		}
		want := !broadcast[m]
		if got := IsSingleDispatchWriteMethod(m); got != want {
			t.Errorf("IsSingleDispatchWriteMethod(%q) = %v, want %v "+
				"(single-dispatch must be exactly the non-broadcast writes)", m, got, want)
		}
	}

	// Reads and EVM names must never be single-dispatch — that would drop them
	// out of consensus entirely.
	for _, m := range []string{"getBalance", "getSlot", "simulateTransaction", "eth_sendRawTransaction", ""} {
		if IsSingleDispatchWriteMethod(m) {
			t.Errorf("IsSingleDispatchWriteMethod(%q) = true, want false", m)
		}
	}

	// Case-insensitive, matching IsNonRetryableWriteMethod: the guard must not
	// be the thing that depends on the caller casing the method correctly.
	if !IsSingleDispatchWriteMethod("REQUESTAIRDROP") {
		t.Error("IsSingleDispatchWriteMethod must be case-insensitive")
	}
}
