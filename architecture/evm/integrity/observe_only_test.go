package integrity

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Observe-only mode is how a network gets integrity enabled for the FIRST time:
// every check runs and reports, but no verdict may touch the response. The
// promise has to hold unconditionally — including for Deterministic checks
// (which ignore invalidBehavior by design), for a per-check onFailure that asks
// for a rejection, and for checks that do not exist yet.

// a corrupt getLogs response: the returned log does not match the request
// filter, which getLogsFilterSanity (Deterministic) rejects.
const observeBadLogs = `[{"address":"0xEVIL","topics":["0xa"],"data":"0xd1","blockNumber":"0x10","blockHash":"0xbb","transactionHash":"0xt1","logIndex":"0x0"}]`

const observeFilter = `{"address":"0xaddr","fromBlock":"0x10","toBlock":"0x10"}`

func observeValidate(t *testing.T, cs CheckSet, observeOnly bool) Result {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[` + observeFilter + `]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(observeBadLogs), nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	var params []any
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(`[`+observeFilter+`]`), &params))
	return Validate(context.Background(), Input{
		Method:      "eth_getLogs",
		Upstream:    common.NewFakeUpstream("u"),
		Response:    rs,
		Checks:      cs,
		Params:      params,
		Reorg:       rejectAll,
		ObserveOnly: observeOnly,
	})
}

func TestObserveOnly(t *testing.T) {
	sanity := only("getLogsFilterSanity", nil)

	t.Run("enforcing (control): a deterministic violation rejects", func(t *testing.T) {
		res := observeValidate(t, sanity, false)
		require.Error(t, res.Err)
		assert.True(t, common.HasErrorCode(res.Err, common.ErrCodeEndpointContentValidation))
		assert.Equal(t, "reject", outcomeOf(res, "getLogsFilterSanity"))
	})

	t.Run("observe-only: same violation is served, reported as would_reject", func(t *testing.T) {
		res := observeValidate(t, sanity, true)
		require.NoError(t, res.Err, "observe-only must never fail a request")
		assert.Empty(t, res.RejectedCheckID)
		assert.Equal(t, "would_reject", outcomeOf(res, "getLogsFilterSanity"))
		require.Len(t, res.Recorded, 1, "the violation must still be recorded for forensics")
		assert.Equal(t, "would_reject", res.Recorded[0].Verdict)
		assert.Equal(t, Deterministic, res.Recorded[0].Class)
		assert.NotEmpty(t, res.Recorded[0].Reason, "the reason must survive for adjudication")
	})

	// The guarantee is absolute: an operator (or a stale config) asking a
	// specific check to reject must not punch through observe mode.
	t.Run("observe-only outranks a per-check onFailure: reject", func(t *testing.T) {
		reject := BehaviorError
		cs := CheckSet{}
		cs.Enable("getLogsFilterSanity", nil)
		cfg := cs["getLogsFilterSanity"]
		cfg.FailOverride = &reject
		cs["getLogsFilterSanity"] = cfg

		res := observeValidate(t, cs, true)
		require.NoError(t, res.Err, "a per-check onFailure must not escape observe-only")
		assert.Equal(t, "would_reject", outcomeOf(res, "getLogsFilterSanity"))
	})

	// invalidBehavior cannot express this: Deterministic checks bypass it. Prove
	// the strictest reorg policy still cannot reject under observe mode.
	t.Run("observe-only overrides invalidBehavior reject/reject", func(t *testing.T) {
		res := observeValidate(t, sanity, true)
		require.NoError(t, res.Err)
		assert.Equal(t, "would_reject", outcomeOf(res, "getLogsFilterSanity"))
	})

	// A clean response must look identical in both modes — observe mode changes
	// only what happens to violations, never what counts as one.
	t.Run("observe-only does not alter passing verdicts", func(t *testing.T) {
		good := `[{"address":"0xaddr","topics":["0xa"],"data":"0xd1","blockNumber":"0x10","blockHash":"0xbb","transactionHash":"0xt1","logIndex":"0x0"}]`
		for _, observe := range []bool{false, true} {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[` + observeFilter + `]}`))
			jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(good), nil)
			rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
			var params []any
			require.NoError(t, common.SonicCfg.Unmarshal([]byte(`[`+observeFilter+`]`), &params))
			res := Validate(context.Background(), Input{
				Method: "eth_getLogs", Upstream: common.NewFakeUpstream("u"), Response: rs,
				Checks: sanity, Params: params, Reorg: rejectAll, ObserveOnly: observe,
			})
			assert.NoError(t, res.Err)
			assert.Equal(t, "pass", outcomeOf(res, "getLogsFilterSanity"))
			assert.Empty(t, res.Recorded)
		}
	})

	// A reorg-sensitive soft-flag keeps its own label — the two must stay
	// distinguishable, since one is routine and the other is an enforcement cost.
	t.Run("soft_flag keeps its verdict label, not would_reject", func(t *testing.T) {
		res := validateBlockPolicy(t, blockResult("0x10", "0xnew", "0xparent"),
			only("hashStability", nil), mockHistory{0x10: "0xold"}, DefaultReorgPolicy())
		require.NoError(t, res.Err)
		assert.Equal(t, "soft_flag", outcomeOf(res, "hashStability"))
		require.Len(t, res.Recorded, 1)
		assert.Equal(t, "soft_flag", res.Recorded[0].Verdict)
	})

	// Off must stay off: observe mode reports what enforcement WOULD do, so it
	// must not resurrect checks the operator disabled (nor their aux fetches).
	t.Run("observe-only does not re-enable a disabled check", func(t *testing.T) {
		off := BehaviorIgnore
		cs := CheckSet{}
		cs.Enable("getLogsFilterSanity", nil)
		cfg := cs["getLogsFilterSanity"]
		cfg.FailOverride = &off
		cs["getLogsFilterSanity"] = cfg

		res := observeValidate(t, cs, true)
		assert.NoError(t, res.Err)
		assert.Equal(t, "off", outcomeOf(res, "getLogsFilterSanity"))
		assert.Empty(t, res.Recorded)
	})
}

// would_reject must be the MARGINAL cost of enforcement, not the total: a
// violation that soft-flags under the configured invalidBehavior soft-flags in
// observe mode too. Only verdicts that would have REJECTED get relabelled.
// This is what makes the two knobs orthogonal — invalidBehavior sets the
// steady-state policy, observeOnly measures the delta to enforcing it.
func TestObserveOnly_IsMarginalOverInvalidBehavior(t *testing.T) {
	// hashStability on an unfinalized block, default policy → soft-flag either way.
	softFlagPolicy := DefaultReorgPolicy() // finalized: reject, unfinalized: soft-flag
	run := func(observe bool) Result {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), blockResult("0x10", "0xnew", "0xparent"), nil)
		rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
		return Validate(context.Background(), Input{
			Method: "eth_getBlockByNumber", Upstream: common.NewFakeUpstream("u"), Response: rs,
			Checks: only("hashStability", nil), History: mockHistory{0x10: "0xold"},
			Reorg: softFlagPolicy, ObserveOnly: observe,
		})
	}
	enforce, observe := run(false), run(true)
	assert.Equal(t, "soft_flag", outcomeOf(enforce, "hashStability"))
	assert.Equal(t, "soft_flag", outcomeOf(observe, "hashStability"),
		"a soft-flag must NOT be relabelled would_reject — otherwise would_reject overstates the enforcement cost")
	require.Len(t, observe.Recorded, 1)
	assert.Equal(t, "soft_flag", observe.Recorded[0].Verdict)
	assert.NoError(t, enforce.Err, "already served under the policy, nothing for observe mode to suppress")
}
