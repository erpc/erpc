package integrity

import "math/big"

// EIP-1559 — the protocol rule, parameterised.
//
// This layer knows the EIP and nothing else: not which chain is being served,
// not which check is asking. Chains supply the constants (see the architecture
// and chain layers); checks supply the blocks. Keeping the rule here means the
// arithmetic is written and tested once, and a chain that implements the EIP
// with its own constants needs data, not code.

// EIP1559Model is a chain's parameterisation of the fee rule.
//
// The constants are NOT universal, and assuming they are is the most dangerous
// mistake available in this package: a wrong constant rejects EVERY block on
// the chain rather than a rare bad one. They are established empirically —
// replay consecutive live blocks and search for the (elasticity, denominator,
// floor) that reproduces the observed sequence (scripts/solve-eip1559-params.py).
//
// A chain earns Derivable only when that search answers over a window where the
// fee actually MOVES. A window with the fee pinned at its floor identifies
// nothing: a constant series is reproduced by almost any parameters.
type EIP1559Model struct {
	// Derivable reports that this rule, with the constants below, reproduces
	// the chain's base fee. False means the fee is set somewhere the rule
	// cannot see (a sequencer, an L2 fee market, a fixed value).
	Derivable bool
	// Elasticity divides the parent gas limit to get the gas target.
	Elasticity int64
	// Denominator damps how far the fee may move in one block.
	Denominator int64
	// MinBaseFee, when > 0, is a floor the chain clamps the computed fee to.
	MinBaseFee int64
}

// EIP1559Mainnet is the canonical configuration, verified against live blocks:
// over 39 consecutive pairs with a moving fee, (2, 8) was the only candidate in
// a wide search that reproduced every observed base fee.
var EIP1559Mainnet = &EIP1559Model{Derivable: true, Elasticity: 2, Denominator: 8}

// EIP1559NotDerivable marks a chain whose base fee provably does not follow
// from its parent — a wide search over elasticity, denominator and floor
// reproduced none of the observed sequence.
var EIP1559NotDerivable = &EIP1559Model{Derivable: false}

// mainnet constants, used only as the fallback when a caller supplies no params
// (library use with no chain context).
const (
	defaultElasticity  = 2
	defaultDenominator = 8
)

// NextBaseFee computes the base fee a child of `parent` must carry under
// EIP-1559 with the given constants. ok=false when the parent's fields cannot
// be parsed.
func NextBaseFee(parent *Header, m EIP1559Model) (*big.Int, bool) {
	parentBaseFee, ok1 := hexToBig(parent.BaseFeePerGas)
	parentGasLimit, ok2 := hexToBig(parent.GasLimit)
	parentGasUsed, ok3 := hexToBig(parent.GasUsed)
	if !ok1 || !ok2 || !ok3 || parentGasLimit.Sign() <= 0 || m.Elasticity <= 0 || m.Denominator <= 0 {
		return nil, false
	}

	gasTarget := new(big.Int).Div(parentGasLimit, big.NewInt(m.Elasticity))
	if gasTarget.Sign() == 0 {
		return nil, false
	}

	switch parentGasUsed.Cmp(gasTarget) {
	case 0:
		// Exactly on target: the fee is unchanged.
		return clampBaseFee(new(big.Int).Set(parentBaseFee), m), true

	case 1:
		// Above target: the fee rises by at least 1 wei.
		delta := new(big.Int).Sub(parentGasUsed, gasTarget)
		delta.Mul(delta, parentBaseFee)
		delta.Div(delta, gasTarget)
		delta.Div(delta, big.NewInt(m.Denominator))
		if delta.Sign() == 0 {
			delta = big.NewInt(1)
		}
		return clampBaseFee(new(big.Int).Add(parentBaseFee, delta), m), true

	default:
		// Below target: the fee falls, floored at zero (and at MinBaseFee).
		delta := new(big.Int).Sub(gasTarget, parentGasUsed)
		delta.Mul(delta, parentBaseFee)
		delta.Div(delta, gasTarget)
		delta.Div(delta, big.NewInt(m.Denominator))
		next := new(big.Int).Sub(parentBaseFee, delta)
		if next.Sign() < 0 {
			next = big.NewInt(0)
		}
		return clampBaseFee(next, m), true
	}
}

// clampBaseFee applies the chain's floor, where it has one.
func clampBaseFee(v *big.Int, m EIP1559Model) *big.Int {
	if m.MinBaseFee > 0 && v.Cmp(big.NewInt(m.MinBaseFee)) < 0 {
		return big.NewInt(m.MinBaseFee)
	}
	return v
}

// hexToBig parses a 0x-prefixed quantity into a big.Int.
func hexToBig(s string) (*big.Int, bool) {
	t := trimHexPrefix(s)
	if t == "" {
		return nil, false
	}
	v, ok := new(big.Int).SetString(t, 16)
	if !ok {
		return nil, false
	}
	return v, true
}
