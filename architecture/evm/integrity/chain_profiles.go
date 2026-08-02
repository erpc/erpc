package integrity

import "strconv"

// Chain profiles describe how a chain deviates from the "standard EVM" the
// checks assume, shipped as defaults so an operator doesn't rediscover each
// deviation as a reject flood. A profile is applied AFTER the level preset and
// BEFORE the operator's per-check overrides — an explicit
// `checks.<id>.enabled: true` still wins over it.
//
// Two kinds of deviation are modelled today, and the struct is the place to add
// more as chains need them (zkSync-style architectures, other L2 fee markets):
//
//   - Disable — the check is protocol-INVALID here. Usual cause: synthetic or
//     system transactions that the protocol commits in the header roots but
//     omits from the RPC response, so recomputing a root from the response can
//     never reproduce it.
//   - Fee — how the chain sets baseFeePerGas, which decides whether, and with
//     which constants, the base fee can be derived from the parent block.
//
// Diagnostic rule for finding new deviations: a check rejecting across ALL
// upstreams of one chain is a chain quirk; scattered per-upstream rejects are
// real catches.
type ChainProfile struct {
	// Family is the architecture this chain belongs to. Informational, and a
	// handle for grouping chains that share deviations.
	Family string
	// Disable lists check ids that are protocol-invalid on this chain.
	Disable []string
	// Fee describes the base-fee mechanism. Nil means "not characterised", and
	// the base-fee derivation does not run — see ApplyChainProfile.
	Fee *FeeModel
}

// FeeModel says whether baseFeePerGas is a pure function of the parent block,
// and with which constants.
//
// These constants are NOT universal, and assuming they are is the most
// dangerous mistake available in this package: a wrong constant rejects EVERY
// block on the chain, rather than a rare bad one. So they are established
// empirically — replay consecutive live blocks and search for the
// (elasticity, denominator, floor) that reproduces the observed sequence.
//
// A chain earns Derivable only when that search returns an answer over a window
// where the fee actually MOVES. A window where the fee sits pinned at its floor
// is not evidence: a constant series is reproduced by almost any parameters, so
// it identifies nothing. Both Base and HyperEVM sampled that way, which is
// exactly why neither is marked derivable here.
type FeeModel struct {
	// Derivable reports that the EIP-1559 formula with the parameters below
	// reproduces this chain's base fee. False means the fee is set somewhere
	// the formula cannot see (a sequencer, an L2 fee market, a fixed value),
	// so the derivation check must not run.
	Derivable bool
	// Elasticity divides the parent gas limit to get the gas target.
	Elasticity int64
	// Denominator damps how far the fee may move in one block.
	Denominator int64
	// MinBaseFee, when > 0, is a floor the chain clamps the computed fee to.
	MinBaseFee int64
}

// mainnetFees is the canonical EIP-1559 configuration, verified against live
// blocks: over 39 consecutive pairs with a moving fee, (2, 8) was the only
// candidate in a wide search that reproduced every observed base fee.
var mainnetFees = &FeeModel{Derivable: true, Elasticity: 2, Denominator: 8}

// notDerivable marks a chain whose base fee provably does NOT follow from its
// parent: a wide search over elasticity, denominator and floor reproduced none
// of the observed sequence.
var notDerivable = &FeeModel{Derivable: false}

var chainProfiles = map[int64]ChainProfile{
	// Ethereum mainnet — the reference EVM.
	1: {Family: "ethereum", Fee: mainnetFees},

	// HyperEVM: HyperCore system transactions are committed in the header roots
	// but omitted from eth_getBlock*'s transactions list, so the whole
	// tx/receipt-root family is unreproducible — including the structural
	// has-txs ⟺ non-empty-root consistency check.
	//
	// Fee: uncharacterised. Sampled windows show the fee pinned at a constant
	// 100000000, consistent with a floor but identifying nothing.
	999: {
		Family:  "hyperevm",
		Disable: []string{"transactionsRootRecompute", "receiptsRootRecompute", "transactionsRootConsistency"},
	},

	// Polygon PoS: bor's state-sync transactions are committed but not listed.
	//
	// Fee: NOT derivable. A search over elasticity 1..16, denominators to 2048
	// and a floor reproduced none of 39 consecutive observed fees — and the
	// live shadow confirmed it the hard way, rejecting across three independent
	// vendors within minutes when mainnet's constants were assumed.
	137: {
		Family:  "polygon-pos",
		Disable: []string{"transactionsRootRecompute", "receiptsRootRecompute"},
		Fee:     notDerivable,
	},

	// Arbitrum One: ArbOS internal transactions are committed but not listed,
	// and ArbOS sets the base fee itself — no (elasticity, denominator, floor)
	// reproduces the observed sequence.
	42161: {
		Family:  "arbitrum-nitro",
		Disable: []string{"transactionsRootRecompute", "receiptsRootRecompute"},
		Fee:     notDerivable,
	},

	// Base (OP Stack): the OP Stack runs EIP-1559 with its OWN elasticity and
	// denominator. Sampled windows had the fee pinned at its 5000000 floor, so
	// they could not identify the constants — and guessing them is precisely
	// the mistake that rejects every block. Left uncharacterised until a
	// moving-fee window pins them down.
	8453: {Family: "op-stack"},

	// BNB Smart Chain: the base fee is a constant 0, so there is no fee market
	// to derive. (Strict EIP-1559 would even predict 1 rather than 0 for an
	// above-target parent, since the formula forces a minimum 1 wei rise.)
	56: {Family: "bsc", Fee: notDerivable},
}

// ApplyChainProfile adapts a resolved check set to a chain: it drops checks
// that are protocol-invalid there, and supplies the chain's fee parameters to
// the checks that need them.
func ApplyChainProfile(cs CheckSet, chainId int64) {
	p := chainProfiles[chainId]
	for _, id := range p.Disable {
		delete(cs, id)
	}

	// The base-fee derivation runs ONLY where the fee model is characterised
	// and derivable. Defaulting an unmodelled chain to mainnet's constants
	// would invert the risk: of the chains measured so far most deviate, and a
	// wrong constant rejects every block instead of a rare bad one. For a chain
	// nobody has characterised, silence is the safe answer.
	if fee := p.Fee; fee != nil && fee.Derivable {
		applyFeeParams(cs, fee)
		return
	}
	delete(cs, "baseFeeDerivation")
}

// applyFeeParams hands the chain's constants to the derivation check.
func applyFeeParams(cs CheckSet, fee *FeeModel) {
	cfg, ok := cs["baseFeeDerivation"]
	if !ok {
		return
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["elasticity"] = strconv.FormatInt(fee.Elasticity, 10)
	cfg.Params["denominator"] = strconv.FormatInt(fee.Denominator, 10)
	if fee.MinBaseFee > 0 {
		cfg.Params["minBaseFee"] = strconv.FormatInt(fee.MinBaseFee, 10)
	}
	cs["baseFeeDerivation"] = cfg
}

// ChainProfileDisables returns the check ids a chain's profile disables (for
// introspection/docs/tests). nil for chains without a profile.
func ChainProfileDisables(chainId int64) []string {
	return chainProfiles[chainId].Disable
}

// ChainFeeModel returns a chain's characterised fee model, or nil when the
// chain has not been characterised.
func ChainFeeModel(chainId int64) *FeeModel {
	return chainProfiles[chainId].Fee
}
