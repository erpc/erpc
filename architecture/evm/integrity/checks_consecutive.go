package integrity

import (
	"context"
	"math/big"
	"strconv"

	"github.com/erpc/erpc/common"
)

// Consecutive-header checks.
//
// Every other check in the catalog judges a response against itself, against a
// commitment inside it, or against another source. These judge it against its
// PARENT — which is only possible because the ChainView follower walks the
// chain block by block and can hand back the block that genuinely precedes this
// one on the same fork. A sparse, traffic-learned pin cannot support them: two
// pins at adjacent heights may sit on different forks, and comparing across
// that boundary would reject honest data.
//
// So they all skip unless the height AND its parent are inside the verified
// segment. Skipping is the correct answer when the follower is off — these
// checks buy their strength from the follower and must not pretend to it
// otherwise.

// Mainnet's EIP-1559 constants, used only as the fallback when a caller
// supplies none. Which constants apply is a per-chain fact (see ChainProfile /
// FeeModel), established by replaying live blocks rather than assumed — the
// chain profile normally injects them as check params.
const (
	defaultElasticity  = 2
	defaultDenominator = 8
)

func init() {
	// baseFeeDerivation — EIP-1559 fixes baseFeePerGas[n] as an exact function
	// of the PARENT's gasUsed, gasLimit and baseFeePerGas. There is no
	// discretion in it, so a header that does not satisfy the formula was not
	// produced by a compliant chain: this catches a fabricated or mangled
	// header that every self-consistency check would wave through, because
	// nothing inside a single header contradicts a wrong base fee.
	//
	// Chains that do not implement EIP-1559 exactly (different elasticity, a
	// sequencer-set fee, no 1559 at all) are excluded per-chain — see
	// chainProfiles. On such a chain this check is protocol-invalid, not a
	// catch, and would reject every block.
	register(&Check{
		ID: "baseFeeDerivation", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetBlockByNumber},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			h := d.Header()
			if h == nil || h.Number == "" || h.BaseFeePerGas == "" {
				return Skipped // pre-1559 block, or a chain that omits the field
			}
			n, err := common.HexToInt64(h.Number)
			if err != nil || n <= 0 {
				return Skipped
			}
			parent, ok := parentInSegment(ctx, n)
			if !ok || parent == nil {
				return Skipped // no verified parent — the follower owns this
			}
			if parent.BaseFeePerGas == "" || parent.GasLimit == "" || parent.GasUsed == "" {
				return Skipped
			}
			want, ok := nextBaseFee(parent, feeParamsFrom(cfg))
			if !ok {
				return Skipped
			}
			got, ok := hexToBig(h.BaseFeePerGas)
			if !ok {
				return Skipped
			}
			if want.Cmp(got) != 0 {
				fp := feeParamsFrom(cfg)
				return failf("block %d baseFeePerGas %s does not follow from parent (gasUsed %s, gasLimit %s, baseFee %s) with elasticity %d / denominator %d — expected %s",
					n, got.String(), parent.GasUsed, parent.GasLimit, parent.BaseFeePerGas,
					fp.elasticity, fp.denominator, want.String())
			}
			return nil
		},
	})

	// timestampMonotonicity — a block must not be older than its parent. The
	// comparison is non-strict on purpose: several chains legitimately produce
	// consecutive blocks sharing a timestamp (batched L2 blocks), so requiring
	// a strict increase would reject honest data there, while a timestamp that
	// moves BACKWARDS is invalid everywhere.
	register(&Check{
		ID: "timestampMonotonicity", Family: FamilyStructural, Class: Deterministic,
		Methods: []string{MethodGetBlockByNumber},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			h := d.Header()
			if h == nil || h.Number == "" || h.Timestamp == "" {
				return Skipped
			}
			n, err := common.HexToInt64(h.Number)
			if err != nil || n <= 0 {
				return Skipped
			}
			parent, ok := parentInSegment(ctx, n)
			if !ok || parent == nil || parent.Timestamp == "" {
				return Skipped
			}
			ts, ok1 := hexToBig(h.Timestamp)
			pts, ok2 := hexToBig(parent.Timestamp)
			if !ok1 || !ok2 {
				return Skipped
			}
			if ts.Cmp(pts) < 0 {
				return failf("block %d timestamp %s is older than its parent's %s", n, ts.String(), pts.String())
			}
			return nil
		},
	})
}

// feeParams are the chain's EIP-1559 constants for this evaluation.
type feeParams struct {
	elasticity  int64
	denominator int64
	minBaseFee  int64
}

// feeParamsFrom reads the constants the chain profile injected, falling back to
// mainnet's when a caller supplies none (library use).
func feeParamsFrom(cfg CheckConfig) feeParams {
	p := feeParams{elasticity: defaultElasticity, denominator: defaultDenominator}
	if v, err := strconv.ParseInt(cfg.param("elasticity", ""), 10, 64); err == nil && v > 0 {
		p.elasticity = v
	}
	if v, err := strconv.ParseInt(cfg.param("denominator", ""), 10, 64); err == nil && v > 0 {
		p.denominator = v
	}
	if v, err := strconv.ParseInt(cfg.param("minBaseFee", ""), 10, 64); err == nil && v > 0 {
		p.minBaseFee = v
	}
	return p
}

// nextBaseFee computes the base fee a child of `parent` must carry under the
// EIP-1559 formula with the given chain constants. Returns ok=false when the
// parent's fields cannot be parsed.
func nextBaseFee(parent *Header, fp feeParams) (*big.Int, bool) {
	parentBaseFee, ok1 := hexToBig(parent.BaseFeePerGas)
	parentGasLimit, ok2 := hexToBig(parent.GasLimit)
	parentGasUsed, ok3 := hexToBig(parent.GasUsed)
	if !ok1 || !ok2 || !ok3 || parentGasLimit.Sign() <= 0 {
		return nil, false
	}

	gasTarget := new(big.Int).Div(parentGasLimit, big.NewInt(fp.elasticity))
	if gasTarget.Sign() == 0 {
		return nil, false
	}

	switch parentGasUsed.Cmp(gasTarget) {
	case 0:
		// Exactly on target: the fee is unchanged.
		return clampFee(new(big.Int).Set(parentBaseFee), fp), true

	case 1:
		// Above target: the fee rises by at least 1 wei.
		delta := new(big.Int).Sub(parentGasUsed, gasTarget)
		delta.Mul(delta, parentBaseFee)
		delta.Div(delta, gasTarget)
		delta.Div(delta, big.NewInt(fp.denominator))
		if delta.Sign() == 0 {
			delta = big.NewInt(1)
		}
		return clampFee(new(big.Int).Add(parentBaseFee, delta), fp), true

	default:
		// Below target: the fee falls, floored at zero.
		delta := new(big.Int).Sub(gasTarget, parentGasUsed)
		delta.Mul(delta, parentBaseFee)
		delta.Div(delta, gasTarget)
		delta.Div(delta, big.NewInt(fp.denominator))
		next := new(big.Int).Sub(parentBaseFee, delta)
		if next.Sign() < 0 {
			next = big.NewInt(0)
		}
		return clampFee(next, fp), true
	}
}

// clampFee applies the chain's floor, where it has one.
func clampFee(v *big.Int, fp feeParams) *big.Int {
	if fp.minBaseFee > 0 && v.Cmp(big.NewInt(fp.minBaseFee)) < 0 {
		return big.NewInt(fp.minBaseFee)
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
