package common

import "testing"

func TestResolveSvmChain_DefaultsToSolana(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", SvmChainSolana},
		{"solana", "solana"},
		{"fogo", "fogo"},
		{"Eclipse", "Eclipse"}, // case-preserving — resolution does not lowercase
	}
	for _, tc := range cases {
		if got := ResolveSvmChain(tc.in); got != tc.want {
			t.Errorf("ResolveSvmChain(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestKnownGenesisHash_ChainAware(t *testing.T) {
	// Empty chain must resolve to solana — existing configs without the new
	// `chain` field continue to see mainnet-beta / devnet / testnet.
	if h, ok := KnownGenesisHash("", "mainnet-beta"); !ok || h == "" {
		t.Errorf("empty-chain lookup for mainnet-beta should resolve to solana; got ok=%v h=%q", ok, h)
	}
	if _, ok := KnownGenesisHash("solana", "devnet"); !ok {
		t.Error("explicit solana + devnet should be known")
	}

	// Unknown chains return !ok even for a cluster name that exists elsewhere.
	if _, ok := KnownGenesisHash("fogo", "mainnet-beta"); ok {
		t.Error("fogo:mainnet-beta should not be known until the chain is onboarded")
	}

	// Unknown cluster on a known chain returns !ok without panicking.
	if _, ok := KnownGenesisHash("solana", "nonesuch"); ok {
		t.Error("solana:nonesuch should not be known")
	}
}

func TestIsValidSvmCluster_ChainAware(t *testing.T) {
	cases := []struct {
		chain, cluster string
		want           bool
	}{
		{"", "mainnet-beta", true},    // back-compat: no chain → solana
		{"solana", "devnet", true},    // explicit solana
		{"solana", "testnet", true},   // solana testnet
		{"solana", "localnet", false}, // unknown solana cluster
		{"fogo", "mainnet", false},    // chain not onboarded yet
		{"", "localnet", false},       // unknown solana cluster via back-compat
	}
	for _, tc := range cases {
		tc := tc
		if got := IsValidSvmCluster(tc.chain, tc.cluster); got != tc.want {
			t.Errorf("IsValidSvmCluster(%q, %q) = %v, want %v", tc.chain, tc.cluster, got, tc.want)
		}
	}
}
