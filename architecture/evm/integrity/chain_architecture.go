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
	// Header lists the header constants this family's consensus fixes. Nil =
	// uncharacterised, and the header-invariant check does not judge the chain.
	Header *HeaderInvariants
	// StateContext is how to ask this family's EVM "which block are you
	// executing in?" for the state probe. Nil = the standard EVM probe
	// (Multicall3 getBlockNumber). Families where block.number does NOT mean
	// the chain's own height must override it — measured, not assumed.
	StateContext *StateContextProbe
}

// StateContextProbe is an eth_call whose return value, executed pinned at
// block N, must equal N.
type StateContextProbe struct {
	To   string // contract address
	Data string // calldata (4-byte selector)
}

// multicall3Context is the standard-EVM context probe: Multicall3 is deployed
// at one address on 200+ chains and getBlockNumber() returns block.number.
var multicall3Context = &StateContextProbe{
	To:   "0xcA11bde05977b3631167028862bE2a173976CA11",
	Data: "0x42cbb15c", // getBlockNumber()
}

// arbSysContext is the Nitro override. On Arbitrum, block.number returns the
// L1 (Ethereum) height, NOT the L2 height — measured live: Multicall3
// getBlockNumber pinned at L2 490382800 answered 25668365 (mainnet's height),
// so the standard probe mismatches on every honest Nitro node (observed as
// 6-of-6 upstreams "failing" on the shadow, the all-upstream signature that
// means the probe is wrong, not the nodes). The ArbSys precompile's
// arbBlockNumber() answers the L2 height: pinned at 490382800 it returned
// exactly 490382800.
var arbSysContext = &StateContextProbe{
	To:   "0x0000000000000000000000000000000000000064",
	Data: "0xa3b1b31d", // arbBlockNumber()
}

// HeaderInvariants are header fields a consensus regime pins to a constant.
//
// Measured, not inferred — the differences between regimes are not guessable.
// Across six chains sha3Uncles is the empty-ommers hash on every one, while
// difficulty is zero only on mainnet, OP Stack and hyperevm (polygon reports
// 0x1, arbitrum 0x1, bsc 0x2), and the nonce is zero everywhere except
// arbitrum. Declaring false simply means "not guaranteed here", never "known to
// be violated".
type HeaderInvariants struct {
	// EmptyUncles — sha3Uncles is always the empty-ommers hash (no proof-of-work
	// ommers are produced).
	EmptyUncles bool
	// ZeroDifficulty — difficulty is fixed at zero.
	ZeroDifficulty bool
	// ZeroNonce — the header nonce is fixed at zero.
	ZeroNonce bool
	// BlobGasMultiple — blobGasUsed is always an exact multiple of the
	// per-blob gas unit (EIP-4844 sells blob space in whole blobs).
	//
	// Chain-specific like the rest: measured true on mainnet over 60
	// consecutive blocks and FALSE on Base, whose blobGasUsed is not a multiple
	// of that unit at all. Granularity only — the excess-blob-gas UPDATE rule
	// is NOT modelled anywhere, because it could not be identified: on mainnet
	// in 2026 no constant target reproduces it (the best of 48 candidates fits
	// 12.8% of 179 consecutive pairs) and a used/3 rule cannot produce the
	// observed decreases. Two independent vendors agree on the values, so the
	// data is sound and the rule is simply not something we can state — and a
	// derivation check built on a guess would reject ~87% of mainnet blocks.
	BlobGasMultiple bool
}

// postMergeHeader is the full set a post-merge / PoS-style regime guarantees.
var postMergeHeader = &HeaderInvariants{EmptyUncles: true, ZeroDifficulty: true, ZeroNonce: true}

// ethereumHeader adds the blob-granularity invariant, measured on mainnet and
// NOT true of the OP Stack, so it cannot live in postMergeHeader.
var ethereumHeader = &HeaderInvariants{EmptyUncles: true, ZeroDifficulty: true, ZeroNonce: true, BlobGasMultiple: true}

// recomputeFamily is the check set that synthetic/system transactions break:
// the protocol commits them in the header roots but omits them from the RPC
// response, so recomputing a root from the response can never reproduce it.
var recomputeFamily = []string{"transactionsRootRecompute", "receiptsRootRecompute"}

var architectures = map[string]Architecture{
	// The reference EVM.
	"ethereum": {
		Name:   "ethereum",
		Fee:    EIP1559Mainnet,
		Header: ethereumHeader,
	},

	// OP Stack (Base, Optimism, ...): runs EIP-1559 with its OWN elasticity and
	// denominator, which differ per deployment. Deliberately left
	// uncharacterised: sampled windows had the fee pinned at its floor, which
	// identifies no constants, and guessing them would reject every block.
	// A chain earns constants in the chain layer once measured.
	"op-stack": {
		Name: "op-stack",
		// Measured on Base: empty ommers, zero difficulty, zero nonce. Its
		// blobGasUsed is NOT blob-granular, so that invariant is not declared.
		//
		// Fee: still uncharacterised, and now known to be UNIDENTIFIABLE from
		// Base rather than merely unsampled — the base fee sits at exactly
		// 5000000 across three windows spanning a week (recent, ~1d, ~1w). A
		// series that never moves is reproduced by any parameters, so nothing
		// can be inferred from it. Enabling the derivation on a guess would be
		// a latent landmine: it would pass while the fee is floored and start
		// rejecting every block the moment the chain gets busy. To identify the
		// constants, sample a congested period where the fee actually moves and
		// run scripts/solve-eip1559-params.py against that range.
		Header: postMergeHeader,
	},

	// Arbitrum Nitro: ArbOS internal transactions are committed but not listed,
	// and ArbOS sets the base fee itself — no (elasticity, denominator, floor)
	// reproduces the observed sequence.
	"arbitrum-nitro": {
		Name:         "arbitrum-nitro",
		Disable:      recomputeFamily,
		Fee:          EIP1559NotDerivable,
		StateContext: arbSysContext,
		// Nitro reports difficulty 0x1 and uses a non-zero header nonce, so only
		// the ommers invariant holds.
		Header: &HeaderInvariants{EmptyUncles: true},
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
		// bor reports difficulty 0x1 (validator span weight), nonce zero.
		Header: &HeaderInvariants{EmptyUncles: true, ZeroNonce: true},
	},

	// BNB Smart Chain: base fee is a constant 0, so there is no fee market to
	// derive. (Strict EIP-1559 would even predict 1 for an above-target parent,
	// since the rule forces a minimum 1 wei rise.)
	"bsc": {
		Name: "bsc",
		Fee:  EIP1559NotDerivable,
		// Parlia reports difficulty 0x2 (in-turn/out-of-turn), nonce zero.
		Header: &HeaderInvariants{EmptyUncles: true, ZeroNonce: true},
	},

	// HyperEVM: HyperCore system transactions are committed in the header roots
	// but omitted from eth_getBlock*'s transactions list, which breaks the
	// recompute family AND the structural has-txs ⟺ non-empty-root check.
	// Fee uncharacterised: sampled windows sat at a constant 100000000.
	"hyperevm": {
		Name: "hyperevm",
		// transactionsRootConsistency is NOT disabled any more: its phantom
		// predicate now recognises HyperEVM's native/L1 system transactions
		// (synthetic signature r=0x1 with zero gasPrice), so the check works
		// here instead of being switched off. The cryptographic recompute
		// family still cannot work — those txs are committed in the roots but
		// absent from the response, so the root can never be reproduced.
		Disable: recomputeFamily,
		Header:  postMergeHeader,
	},
}

// ArchitectureByName exposes a family for introspection/tests.
func ArchitectureByName(name string) (Architecture, bool) {
	a, ok := architectures[name]
	return a, ok
}
