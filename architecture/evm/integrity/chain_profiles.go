package integrity

import "strconv"

// The chain layer — which architecture a chainId runs, plus anything true of
// that chain alone. This is the ONLY place a chain id appears; the protocol and
// architecture layers stay chain-agnostic (see chain_architecture.go for the
// layering).
//
// Most chains need one line here. Reach for an override only when the chain
// genuinely differs from its family — a measured fee floor, a deviation its
// stack does not share. Anything true of the whole family belongs upstairs in
// the architecture.
//
// Diagnostic rule for finding new deviations: a check rejecting across ALL
// upstreams of one chain is a chain quirk; scattered per-upstream rejects are
// real catches.
type ChainSpec struct {
	// Architecture names the family (see architectures). Empty = uncharacterised.
	Architecture string
	// Disable adds chain-specific check exclusions on top of the family's.
	Disable []string
	// Fee overrides the family's fee model — for a chain that has been measured
	// while its family has not, or that clamps at its own floor.
	Fee *EIP1559Model
	// Header overrides the family's declared header invariants, for a chain
	// whose consensus fixes a different set than its stack normally does.
	Header *HeaderInvariants
}

var chains = map[int64]ChainSpec{
	1:     {Architecture: "ethereum"},
	56:    {Architecture: "bsc"},
	137:   {Architecture: "polygon-pos"},
	999:   {Architecture: "hyperevm"},
	8453:  {Architecture: "op-stack"},
	42161: {Architecture: "arbitrum-nitro"},
}

// ChainProfile is the resolved view for one chain: the family's deviations with
// the chain's own layered on top.
type ChainProfile struct {
	Architecture string
	Disable      []string
	Fee          *EIP1559Model
	Header       *HeaderInvariants
	StateContext *StateContextProbe
}

// ProfileFor resolves a chain's profile. An unknown chain gets an empty profile
// — no exclusions, and no fee model, which means no fee derivation.
func ProfileFor(chainId int64) ChainProfile {
	spec, known := chains[chainId]
	if !known {
		return ChainProfile{}
	}
	p := ChainProfile{Architecture: spec.Architecture}
	if arch, ok := architectures[spec.Architecture]; ok {
		p.Disable = append(p.Disable, arch.Disable...)
		p.Fee = arch.Fee
		p.Header = arch.Header
		p.StateContext = arch.StateContext
	}
	// Chain-specific additions win over the family's.
	p.Disable = append(p.Disable, spec.Disable...)
	if spec.Fee != nil {
		p.Fee = spec.Fee
	}
	if spec.Header != nil {
		p.Header = spec.Header
	}
	return p
}

// ApplyChainProfile adapts a resolved check set to a chain: it drops checks the
// chain's protocol makes invalid, and supplies the fee constants to the checks
// that need them. Applied AFTER the level preset and BEFORE the operator's
// per-check overrides, so an explicit `checks.<id>.enabled: true` still wins.
func ApplyChainProfile(cs CheckSet, chainId int64) {
	p := ProfileFor(chainId)
	for _, id := range p.Disable {
		delete(cs, id)
	}

	// Fee derivation runs ONLY where the model is characterised and derivable.
	// Defaulting an unmodelled chain to mainnet's constants would invert the
	// risk: of the chains measured so far most deviate, and a wrong constant
	// rejects every block instead of a rare bad one. For a chain nobody has
	// characterised, silence is the safe answer.
	applyHeaderInvariants(cs, p.Header)

	if p.Fee != nil && p.Fee.Derivable {
		applyFeeParams(cs, p.Fee)
		return
	}
	delete(cs, "baseFeeDerivation")
}

// applyHeaderInvariants tells the header check which constants this chain's
// consensus fixes. A chain with no declaration keeps the check enabled but it
// verifies nothing and reports skip, which is honest — the alternative would be
// asserting invariants nobody has measured for that chain.
func applyHeaderInvariants(cs CheckSet, inv *HeaderInvariants) {
	if inv == nil {
		return
	}
	cfg, ok := cs["headerConsensusInvariants"]
	if !ok {
		return
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	if inv.EmptyUncles {
		cfg.Params["emptyUncles"] = "true"
	}
	if inv.ZeroDifficulty {
		cfg.Params["zeroDifficulty"] = "true"
	}
	if inv.ZeroNonce {
		cfg.Params["zeroNonce"] = "true"
	}
	if inv.BlobGasMultiple {
		cfg.Params["blobGasMultiple"] = "true"
	}
	cs["headerConsensusInvariants"] = cfg
}

// applyFeeParams hands the chain's constants to the derivation check.
func applyFeeParams(cs CheckSet, m *EIP1559Model) {
	cfg, ok := cs["baseFeeDerivation"]
	if !ok {
		return
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["elasticity"] = strconv.FormatInt(m.Elasticity, 10)
	cfg.Params["denominator"] = strconv.FormatInt(m.Denominator, 10)
	if m.MinBaseFee > 0 {
		cfg.Params["minBaseFee"] = strconv.FormatInt(m.MinBaseFee, 10)
	}
	cs["baseFeeDerivation"] = cfg
}

// ChainProfileDisables returns the check ids a chain excludes (for
// introspection/docs/tests). nil for chains without a profile.
func ChainProfileDisables(chainId int64) []string {
	return ProfileFor(chainId).Disable
}

// ChainStateContextProbe returns the execution-context probe for a chain: the
// family's override where one exists, the standard Multicall3 probe otherwise.
func ChainStateContextProbe(chainId int64) *StateContextProbe {
	if p := ProfileFor(chainId).StateContext; p != nil {
		return p
	}
	return multicall3Context
}

// ChainFeeModel returns a chain's characterised fee model, or nil when neither
// the chain nor its architecture has been characterised.
func ChainFeeModel(chainId int64) *EIP1559Model {
	return ProfileFor(chainId).Fee
}
