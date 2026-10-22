package util

import "testing"

func TestSvmNetworkId_BackwardCompat(t *testing.T) {
	cases := []struct {
		name    string
		chain   string
		cluster string
		want    string
	}{
		{"empty chain → implicit solana", "", "mainnet-beta", "svm:mainnet-beta"},
		{"explicit solana collapses to short form", "solana", "devnet", "svm:devnet"},
		{"non-solana chain uses explicit prefix", "fogo", "mainnet", "svm:fogo:mainnet"},
		{"eclipse chain", "eclipse", "mainnet-beta", "svm:eclipse:mainnet-beta"},
		{"private localnet", "mychain", "localnet", "svm:mychain:localnet"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SvmNetworkId(tc.chain, tc.cluster); got != tc.want {
				t.Fatalf("SvmNetworkId(%q, %q) = %q, want %q", tc.chain, tc.cluster, got, tc.want)
			}
		})
	}
}

func TestIsValidNetworkId_SvmWithChain(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		// Back-compat shape.
		{"svm:mainnet-beta", true},
		{"svm:devnet", true},
		// New three-segment shape.
		{"svm:fogo:mainnet", true},
		{"svm:eclipse:mainnet-beta", true},
		{"svm:my_chain:my_cluster", true},

		// Rejections.
		{"svm:", false},             // empty rest
		{"svm:a:", false},            // trailing colon → empty cluster
		{"svm::mainnet", false},      // empty chain segment
		{"svm:a:b:c", false},         // three colons not supported
		{"svm:main net-beta", false}, // space not allowed
		{"svm:main/net", false},      // slash not allowed
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			if got := IsValidNetworkId(tc.id); got != tc.want {
				t.Fatalf("IsValidNetworkId(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}
