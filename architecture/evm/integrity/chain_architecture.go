package integrity

// The architecture layer — how a FAMILY of chains deviates from the reference
// EVM. It sits between the protocol layer (an EIP's rule, parameterised) and
// the chain layer (one chainId's specifics):
//
//	protocol_*.go   the EIP's rule and its parameter type. Knows no chains.
//	chain_architecture.go   what a family (op-stack, arbitrum-nitro, ...) does
//	                        differently. Knows no individual chains.
//	chain_profiles.go       which chain runs which architecture, plus per-chain
//	                        overrides. The only place a chainId appears.
//
// Most deviations are properties of the ARCHITECTURE, not the chain: every OP
// Stack chain shares a fee mechanism, every Nitro chain shares ArbOS system
// transactions. Recording them here means a new chain on a known stack is one
// line in the chain layer rather than a rediscovery.
//
// Adding an architecture: give it a name, list the checks its protocol makes
// invalid, and state its fee model — Derivable with measured constants, or
// EIP1559NotDerivable when the fee does not follow from the parent. Leave Fee
// nil when nobody has characterised it; the chain layer then runs no
// fee-derivation, which is the safe answer.
type Architecture struct {
	// Name identifies the family (also surfaced for introspection/docs).
	Name string
	// Disable lists check ids the family's protocol makes invalid.
	Disable []string
	// Fee is the family's base-fee mechanism. Nil = uncharacterised.
	Fee *EIP1559Model
}

// recomputeFamily is the check set that synthetic/system transactions break:
// the protocol commits them in the header roots but omits them from the RPC
// response, so recomputing a root from the response can never reproduce it.
var recomputeFamily = []string{"transactionsRootRecompute", "receiptsRootRecompute"}

var architectures = map[string]Architecture{
	// The reference EVM.
	"ethereum": {
		Name: "ethereum",
		Fee:  EIP1559Mainnet,
	},

	// OP Stack (Base, Optimism, ...): runs EIP-1559 with its OWN elasticity and
	// denominator, which differ per deployment. Deliberately left
	// uncharacterised: sampled windows had the fee pinned at its floor, which
	// identifies no constants, and guessing them would reject every block.
	// A chain earns constants in the chain layer once measured.
	"op-stack": {
		Name: "op-stack",
	},

	// Arbitrum Nitro: ArbOS internal transactions are committed but not listed,
	// and ArbOS sets the base fee itself — no (elasticity, denominator, floor)
	// reproduces the observed sequence.
	"arbitrum-nitro": {
		Name:    "arbitrum-nitro",
		Disable: recomputeFamily,
		Fee:     EIP1559NotDerivable,
	},

	// Polygon PoS (bor): state-sync transactions are committed but not listed.
	// The fee is not parent-derivable either — a search over elasticity 1..16
	// and denominators to 2048 reproduced none of 39 observed fees, which the
	// live shadow confirmed by rejecting across three independent vendors when
	// mainnet's constants were assumed.
	"polygon-pos": {
		Name:    "polygon-pos",
		Disable: recomputeFamily,
		Fee:     EIP1559NotDerivable,
	},

	// BNB Smart Chain: base fee is a constant 0, so there is no fee market to
	// derive. (Strict EIP-1559 would even predict 1 for an above-target parent,
	// since the rule forces a minimum 1 wei rise.)
	"bsc": {
		Name: "bsc",
		Fee:  EIP1559NotDerivable,
	},

	// HyperEVM: HyperCore system transactions are committed in the header roots
	// but omitted from eth_getBlock*'s transactions list, which breaks the
	// recompute family AND the structural has-txs ⟺ non-empty-root check.
	// Fee uncharacterised: sampled windows sat at a constant 100000000.
	"hyperevm": {
		Name:    "hyperevm",
		Disable: append(append([]string{}, recomputeFamily...), "transactionsRootConsistency"),
	},
}

// ArchitectureByName exposes a family for introspection/tests.
func ArchitectureByName(name string) (Architecture, bool) {
	a, ok := architectures[name]
	return a, ok
}
