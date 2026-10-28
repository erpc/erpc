package util

import (
	"math"
	"strings"
	"testing"
)

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
		{"svm:", false},              // empty rest
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

// Chain ids reach the network registry straight off the URL path. Every
// accepted spelling becomes its own permanent BootstrapTask, its own lazily
// created NetworkConfig and its own `network` metric label — so the accepted
// set must be exactly one id per chain, not one per spelling.
func TestIsValidNetworkId_EvmCanonicalOnly(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"evm:1", true},
		{"evm:137", true},
		{"evm:42161", true},
		{"evm:11155111", true},
		{"evm:9223372036854775807", true}, // max int64, what EvmNetworkConfig.ChainId holds

		// Aliases of a chain that already has a canonical spelling.
		{"evm:01", false},
		{"evm:007", false},
		{"evm:0000000001", false},
		{"evm:+1", false},
		{"evm:-1", false},
		{"evm:0", false},

		// Not a decimal chain id at all.
		{"evm:", false},
		{"evm: 1", false},
		{"evm:1 ", false},
		{"evm:0x1", false},
		{"evm:1_0", false},
		{"evm:1.0", false},
		{"evm:1e3", false},
		{"evm:one", false},
		{"evm:9223372036854775808", false},  // int64 overflow
		{"evm:99999999999999999999", false}, // far past int64

		// Unknown architecture prefixes stay rejected.
		{"1", false},
		{"", false},
		{"btc:1", false},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			if got := IsValidNetworkId(tc.id); got != tc.want {
				t.Fatalf("IsValidNetworkId(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

// A network id becomes a Prometheus label value; an unbounded path segment
// must not become an unbounded one.
func TestIsValidNetworkId_LengthBounded(t *testing.T) {
	if IsValidNetworkId("svm:" + strings.Repeat("a", MaxNetworkIdLength)) {
		t.Fatal("expected an over-long svm cluster to be rejected")
	}
	if !IsValidNetworkId("svm:" + strings.Repeat("a", MaxNetworkIdLength-4)) {
		t.Fatal("expected a cluster name at the ceiling to be accepted")
	}
}

// EvmNetworkId is the canonical constructor; whatever it emits must round-trip
// through the validator, or config-built networks would stop resolving.
func TestEvmNetworkId_RoundTripsThroughValidator(t *testing.T) {
	for _, chainId := range []int64{1, 10, 137, 8453, 42161, 11155111, math.MaxInt64} {
		id := EvmNetworkId(chainId)
		if !IsValidNetworkId(id) {
			t.Fatalf("EvmNetworkId(%d) = %q which the validator rejects", chainId, id)
		}
	}
}
