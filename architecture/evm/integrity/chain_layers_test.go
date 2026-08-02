package integrity

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The layering is the point of this split, so it is asserted rather than left
// to convention:
//
//	protocol_*.go          an EIP's rule, parameterised. Knows no chains.
//	chain_architecture.go  what a family does differently. Knows no chain ids.
//	chain_profiles.go      which chain runs which family. The only chain ids.
//
// When these blur, chain-specific knowledge leaks into protocol code and the
// next chain becomes a special case instead of a line of data.
func TestChainKnowledgeStaysInTheChainLayer(t *testing.T) {
	chainIDLiteral := regexp.MustCompile(`(?m)^\s*(1|56|137|999|8453|42161)\s*:`)

	for _, f := range []string{"protocol_eip1559.go", "chain_architecture.go", "checks_consecutive.go"} {
		body, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.False(t, chainIDLiteral.Match(body),
			"%s must not name individual chains — that belongs in chain_profiles.go", f)
	}

	protocol, err := os.ReadFile("protocol_eip1559.go")
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(protocol), "big.Int"),
		"the protocol layer owns the arithmetic")

	check, err := os.ReadFile("checks_consecutive.go")
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(check), "big.Int"),
		"the check layer should delegate the arithmetic to the protocol layer, not reimplement it")
}

// A chain inherits its family's deviations; overrides are for what is true of
// that chain alone.
func TestChainProfileResolvesArchitectureThenOverrides(t *testing.T) {
	t.Run("a chain inherits its architecture's exclusions", func(t *testing.T) {
		p := ProfileFor(42161) // arbitrum-nitro
		assert.Equal(t, "arbitrum-nitro", p.Architecture)
		assert.Contains(t, p.Disable, "transactionsRootRecompute")
		require.NotNil(t, p.Fee)
		assert.False(t, p.Fee.Derivable, "ArbOS sets the fee; it does not follow from the parent")
	})

	t.Run("a chain inherits its architecture's fee model", func(t *testing.T) {
		p := ProfileFor(1)
		require.NotNil(t, p.Fee)
		assert.True(t, p.Fee.Derivable)
		assert.EqualValues(t, 2, p.Fee.Elasticity)
		assert.EqualValues(t, 8, p.Fee.Denominator)
	})

	t.Run("an uncharacterised family yields no fee model", func(t *testing.T) {
		p := ProfileFor(8453) // op-stack: constants not measured yet
		assert.Equal(t, "op-stack", p.Architecture)
		assert.Nil(t, p.Fee, "guessing a family's constants is what rejects every block")
	})

	t.Run("an unknown chain gets an empty profile", func(t *testing.T) {
		p := ProfileFor(987654321)
		assert.Empty(t, p.Architecture)
		assert.Nil(t, p.Fee)
		assert.Empty(t, p.Disable)
	})

	// The mechanism a new chain uses: point it at a family and, if it has been
	// measured while the family has not, give it constants of its own.
	t.Run("a per-chain fee override wins over the family", func(t *testing.T) {
		chains[424242] = ChainSpec{
			Architecture: "op-stack",
			Fee:          &EIP1559Model{Derivable: true, Elasticity: 6, Denominator: 250, MinBaseFee: 5000000},
		}
		defer delete(chains, 424242)

		p := ProfileFor(424242)
		require.NotNil(t, p.Fee)
		assert.True(t, p.Fee.Derivable)
		assert.EqualValues(t, 6, p.Fee.Elasticity)

		cs := CheckSet{"baseFeeDerivation": CheckConfig{Enabled: true}}
		ApplyChainProfile(cs, 424242)
		cfg, ok := cs["baseFeeDerivation"]
		require.True(t, ok, "a measured chain runs the derivation even when its family is uncharacterised")
		assert.Equal(t, "6", cfg.Params["elasticity"])
		assert.Equal(t, "250", cfg.Params["denominator"])
		assert.Equal(t, "5000000", cfg.Params["minBaseFee"])
	})
}

// The execution-context probe is per-architecture because block.number does NOT
// mean "this chain's height" everywhere. Measured on Arbitrum: Multicall3
// getBlockNumber pinned at L2 490382800 answered 25668365 — the L1 height — so
// the standard probe mislabels every honest Nitro node as stale; the ArbSys
// precompile's arbBlockNumber() answered the pinned height exactly.
func TestStateContextProbePerArchitecture(t *testing.T) {
	std := ChainStateContextProbe(1)
	require.NotNil(t, std)
	assert.Equal(t, "0x42cbb15c", std.Data, "standard EVMs probe via Multicall3 getBlockNumber")

	arb := ChainStateContextProbe(42161)
	require.NotNil(t, arb)
	assert.Equal(t, "0x0000000000000000000000000000000000000064", arb.To, "Nitro probes the ArbSys precompile")
	assert.Equal(t, "0xa3b1b31d", arb.Data, "arbBlockNumber() — block.number would answer the L1 height")

	unknown := ChainStateContextProbe(987654321)
	require.NotNil(t, unknown)
	assert.Equal(t, std.To, unknown.To, "an unknown chain gets the standard probe — worst case it reads unsupported and never advances, it cannot mislabel")
}
