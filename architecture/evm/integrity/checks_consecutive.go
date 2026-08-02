package integrity

import (
	"context"
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
			want, ok := NextBaseFee(parent, feeModelFrom(cfg))
			if !ok {
				return Skipped
			}
			got, ok := hexToBig(h.BaseFeePerGas)
			if !ok {
				return Skipped
			}
			if want.Cmp(got) != 0 {
				fm := feeModelFrom(cfg)
				return failf("block %d baseFeePerGas %s does not follow from parent (gasUsed %s, gasLimit %s, baseFee %s) with elasticity %d / denominator %d — expected %s",
					n, got.String(), parent.GasUsed, parent.GasLimit, parent.BaseFeePerGas,
					fm.Elasticity, fm.Denominator, want.String())
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

// feeModelFrom builds the protocol model from the constants the chain profile
// injected, falling back to mainnet's when a caller supplies none (library use
// with no chain context).
func feeModelFrom(cfg CheckConfig) EIP1559Model {
	m := EIP1559Model{Derivable: true, Elasticity: defaultElasticity, Denominator: defaultDenominator}
	if v, err := strconv.ParseInt(cfg.param("elasticity", ""), 10, 64); err == nil && v > 0 {
		m.Elasticity = v
	}
	if v, err := strconv.ParseInt(cfg.param("denominator", ""), 10, 64); err == nil && v > 0 {
		m.Denominator = v
	}
	if v, err := strconv.ParseInt(cfg.param("minBaseFee", ""), 10, 64); err == nil && v > 0 {
		m.MinBaseFee = v
	}
	return m
}
