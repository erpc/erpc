package integrity

import (
	"context"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// segmentHistory is a History that also exposes a verified contiguous segment.
type segmentHistory struct {
	from, to int64
	headers  map[int64]*Header
}

func (s *segmentHistory) HashAt(n int64) (string, bool) {
	h, ok := s.headers[n]
	if !ok {
		return "", false
	}
	return h.Hash, true
}
func (s *segmentHistory) FollowedRange() (int64, int64, bool) {
	if s.to == 0 {
		return 0, 0, false
	}
	return s.from, s.to, true
}
func (s *segmentHistory) HeaderAt(n int64) (*Header, bool) {
	h, ok := s.headers[n]
	return h, ok
}

// plainHistory has NO segment — the shape when the follower is disabled.
type plainHistory struct{ headers map[int64]*Header }

func (p plainHistory) HashAt(n int64) (string, bool) {
	h, ok := p.headers[n]
	if !ok {
		return "", false
	}
	return h.Hash, true
}

func consecutiveBody(number int64, baseFee, gasLimit, gasUsed, ts string) []byte {
	return []byte(fmt.Sprintf(
		`{"number":"0x%x","hash":"0xchild","parentHash":"0xparent","baseFeePerGas":"%s","gasLimit":"%s","gasUsed":"%s","timestamp":"%s"}`,
		number, baseFee, gasLimit, gasUsed, ts))
}

func runConsecutive(t *testing.T, checkID string, body []byte, hist History) Result {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x65",false]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), body, nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	return Validate(context.Background(), Input{
		Method:   "eth_getBlockByNumber",
		Upstream: common.NewFakeUpstream("u"),
		Response: rs,
		Checks:   only(checkID, nil),
		Params:   []any{"0x65", false},
		History:  hist,
	})
}

// EIP-1559 makes the child's base fee a pure function of the parent, so a
// header whose base fee does not follow was not produced by a compliant chain —
// something no single-header check can notice.
func TestBaseFeeDerivation(t *testing.T) {
	// parent: gasLimit 30M => target 15M. gasUsed 15M == target => fee unchanged.
	parentOnTarget := &Header{
		Number: "0x64", Hash: "0xparent",
		BaseFeePerGas: "0x3b9aca00", // 1 gwei
		GasLimit:      "0x1c9c380",  // 30,000,000
		GasUsed:       "0xe4e1c0",   // 15,000,000
	}
	seg := &segmentHistory{from: 100, to: 101, headers: map[int64]*Header{100: parentOnTarget}}

	t.Run("a correctly derived base fee passes", func(t *testing.T) {
		res := runConsecutive(t, "baseFeeDerivation",
			consecutiveBody(101, "0x3b9aca00", "0x1c9c380", "0x0", "0x100"), seg)
		require.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "baseFeeDerivation"))
	})

	t.Run("a base fee that does not follow from the parent is rejected", func(t *testing.T) {
		res := runConsecutive(t, "baseFeeDerivation",
			consecutiveBody(101, "0x4a817c800", "0x1c9c380", "0x0", "0x100"), seg)
		require.Error(t, res.Err, "an impossible base fee must not be served")
		assert.Equal(t, "reject", outcomeOf(res, "baseFeeDerivation"))
	})

	t.Run("a full parent block raises the fee by exactly the formula", func(t *testing.T) {
		// gasUsed 30M, target 15M => delta = 1gwei * 15M/15M / 8 = 0.125 gwei
		full := &Header{
			Number: "0x64", Hash: "0xparent",
			BaseFeePerGas: "0x3b9aca00", GasLimit: "0x1c9c380", GasUsed: "0x1c9c380",
		}
		want, ok := NextBaseFee(full, EIP1559Model{Derivable: true, Elasticity: 2, Denominator: 8})
		require.True(t, ok)
		assert.Equal(t, "1125000000", want.String(), "1 gwei + 1/8 of 1 gwei")

		s := &segmentHistory{from: 100, to: 101, headers: map[int64]*Header{100: full}}
		res := runConsecutive(t, "baseFeeDerivation",
			consecutiveBody(101, "0x430e2340", "0x1c9c380", "0x0", "0x100"), s)
		require.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "baseFeeDerivation"))
	})

	// The check's strength comes entirely from the follower's verified chain.
	// Without it, the "parent" is whatever pin traffic happened to leave behind,
	// which may be on another fork — comparing against that would reject honest
	// data, so the only correct answer is to skip.
	t.Run("skips when there is no verified chain segment", func(t *testing.T) {
		res := runConsecutive(t, "baseFeeDerivation",
			consecutiveBody(101, "0x4a817c800", "0x1c9c380", "0x0", "0x100"),
			plainHistory{headers: map[int64]*Header{100: parentOnTarget}})
		require.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "baseFeeDerivation"))
	})

	t.Run("skips when the block sits outside the verified segment", func(t *testing.T) {
		outside := &segmentHistory{from: 500, to: 600, headers: map[int64]*Header{100: parentOnTarget}}
		res := runConsecutive(t, "baseFeeDerivation",
			consecutiveBody(101, "0x4a817c800", "0x1c9c380", "0x0", "0x100"), outside)
		require.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "baseFeeDerivation"))
	})

	t.Run("skips a pre-1559 block that carries no base fee", func(t *testing.T) {
		body := []byte(`{"number":"0x65","hash":"0xchild","parentHash":"0xparent","gasLimit":"0x1c9c380","gasUsed":"0x0","timestamp":"0x100"}`)
		res := runConsecutive(t, "baseFeeDerivation", body, seg)
		require.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "baseFeeDerivation"))
	})
}

func TestTimestampMonotonicity(t *testing.T) {
	parent := &Header{Number: "0x64", Hash: "0xparent", Timestamp: "0x1000"}
	seg := &segmentHistory{from: 100, to: 101, headers: map[int64]*Header{100: parent}}

	t.Run("a later timestamp passes", func(t *testing.T) {
		res := runConsecutive(t, "timestampMonotonicity",
			consecutiveBody(101, "0x0", "0x1c9c380", "0x0", "0x1001"), seg)
		require.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "timestampMonotonicity"))
	})

	// Non-strict on purpose: chains that batch blocks legitimately repeat a
	// timestamp, and rejecting those would be a false positive on honest data.
	t.Run("an equal timestamp passes, since batched chains repeat them", func(t *testing.T) {
		res := runConsecutive(t, "timestampMonotonicity",
			consecutiveBody(101, "0x0", "0x1c9c380", "0x0", "0x1000"), seg)
		require.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "timestampMonotonicity"))
	})

	t.Run("a timestamp going backwards is rejected", func(t *testing.T) {
		res := runConsecutive(t, "timestampMonotonicity",
			consecutiveBody(101, "0x0", "0x1c9c380", "0x0", "0xfff"), seg)
		require.Error(t, res.Err)
		assert.Equal(t, "reject", outcomeOf(res, "timestampMonotonicity"))
	})

	t.Run("skips without a verified segment", func(t *testing.T) {
		res := runConsecutive(t, "timestampMonotonicity",
			consecutiveBody(101, "0x0", "0x1c9c380", "0x0", "0xfff"),
			plainHistory{headers: map[int64]*Header{100: parent}})
		require.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "timestampMonotonicity"))
	})
}

// The derivation runs ONLY on chains whose fee model has been characterised.
// A wrong constant rejects EVERY block, so an uncharacterised chain must get
// silence rather than mainnet's constants by default.
func TestBaseFeeDerivationRunsOnlyWhereTheFeeModelIsCharacterised(t *testing.T) {
	withCheck := func(chainId int64) (CheckConfig, bool) {
		cs := CheckSet{"baseFeeDerivation": CheckConfig{Enabled: true}}
		ApplyChainProfile(cs, chainId)
		cfg, ok := cs["baseFeeDerivation"]
		return cfg, ok
	}

	t.Run("mainnet runs it with its verified constants", func(t *testing.T) {
		cfg, ok := withCheck(1)
		require.True(t, ok, "mainnet is characterised and must run the derivation")
		assert.Equal(t, "2", cfg.Params["elasticity"])
		assert.Equal(t, "8", cfg.Params["denominator"])
	})

	// Measured, not assumed: no (elasticity, denominator, floor) reproduces
	// these chains' observed base-fee sequences.
	for _, chainId := range []int64{137, 42161, 56} {
		t.Run(fmt.Sprintf("chain %d is not derivable", chainId), func(t *testing.T) {
			_, ok := withCheck(chainId)
			assert.False(t, ok, "a chain whose fee provably does not follow from its parent must not run the derivation")
		})
	}

	// Base and HyperEVM sampled with the fee pinned at a floor, which cannot
	// identify constants — so they stay uncharacterised rather than guessed.
	for _, chainId := range []int64{8453, 999} {
		t.Run(fmt.Sprintf("chain %d is uncharacterised", chainId), func(t *testing.T) {
			_, ok := withCheck(chainId)
			assert.False(t, ok, "an uncharacterised chain must not inherit mainnet's constants")
		})
	}

	t.Run("an unknown chain gets silence, not mainnet's constants", func(t *testing.T) {
		_, ok := withCheck(1337424242)
		assert.False(t, ok)
	})
}

// The constants actually drive the arithmetic, so a chain with a different
// model computes a different expected fee.
func TestFeeParamsDriveTheDerivation(t *testing.T) {
	parent := &Header{
		Number: "0x64", Hash: "0xparent",
		BaseFeePerGas: "0x3b9aca00", GasLimit: "0x1c9c380", GasUsed: "0x1c9c380", // full block
	}
	mainnet, ok := NextBaseFee(parent, EIP1559Model{Derivable: true, Elasticity: 2, Denominator: 8})
	require.True(t, ok)
	opStack, ok := NextBaseFee(parent, EIP1559Model{Derivable: true, Elasticity: 6, Denominator: 250})
	require.True(t, ok)
	assert.NotEqual(t, mainnet.String(), opStack.String(),
		"different chain constants must produce different expected fees — that is why they cannot be assumed")

	t.Run("a floor clamps the computed fee", func(t *testing.T) {
		draining := &Header{
			Number: "0x64", Hash: "0xparent",
			BaseFeePerGas: "0x3b9aca00", GasLimit: "0x1c9c380", GasUsed: "0x0", // empty → fee falls
		}
		unclamped, _ := NextBaseFee(draining, EIP1559Model{Derivable: true, Elasticity: 2, Denominator: 8})
		clamped, _ := NextBaseFee(draining, EIP1559Model{Derivable: true, Elasticity: 2, Denominator: 8, MinBaseFee: 999999999})
		assert.Equal(t, "999999999", clamped.String())
		assert.NotEqual(t, unclamped.String(), clamped.String())
	})
}
