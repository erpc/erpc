package integrity

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// autoCorrectWhenPossible splits "seek a valid replacement" from "what to do
// when none exists". The engine's share of the contract: a recordOnly verdict
// under AutoCorrect must REJECT (so the failsafe hunts a replacement) while
// marking the rejection FALLBACK-ELIGIBLE (so exhaustion serves the flagged
// original instead of an error). Without AutoCorrect the verdict serves
// immediately, corrected by nothing.

func autocorrectValidate(t *testing.T, autoCorrect, observeOnly bool, override *Behavior) Result {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[` + observeFilter + `]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(observeBadLogs), nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	var params []any
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(`[`+observeFilter+`]`), &params))
	cs := CheckSet{"getLogsFilterSanity": CheckConfig{Enabled: true, FailOverride: override}}
	return Validate(context.Background(), Input{
		Method:      "eth_getLogs",
		Upstream:    common.NewFakeUpstream("u"),
		Response:    rs,
		Checks:      cs,
		Params:      params,
		Reorg:       rejectAll,
		ObserveOnly: observeOnly,
		AutoCorrect: autoCorrect,
	})
}

func TestAutoCorrect_EscalatesRecordOnlyToRecoverableReject(t *testing.T) {
	record := BehaviorRecord
	res := autocorrectValidate(t, true, false, &record)
	require.Error(t, res.Err, "AutoCorrect must reject so the failsafe hunts a replacement")
	assert.True(t, common.HasErrorCode(res.Err, common.ErrCodeEndpointContentValidation))
	assert.True(t, res.FallbackEligible, "the rejection must be fallback-eligible: exhaustion serves the original")
	assert.Equal(t, "reject_recoverable", outcomeOf(res, "getLogsFilterSanity"))
	assert.Equal(t, "getLogsFilterSanity", res.RejectedCheckID)
	assert.NotEmpty(t, res.RejectedReason, "the fallback serve is recorded with the original violation reason")
}

func TestAutoCorrect_OffServesAndRecordsImmediately(t *testing.T) {
	record := BehaviorRecord
	res := autocorrectValidate(t, false, false, &record)
	require.NoError(t, res.Err, "without AutoCorrect a recordOnly verdict serves immediately")
	assert.False(t, res.FallbackEligible)
	assert.Equal(t, "record_only", outcomeOf(res, "getLogsFilterSanity"))
	require.Len(t, res.Recorded, 1)
	assert.Equal(t, "record_only", res.Recorded[0].Verdict)
}

func TestAutoCorrect_HardRejectIsNeverFallbackEligible(t *testing.T) {
	res := autocorrectValidate(t, true, false, nil) // deterministic default = hardReject
	require.Error(t, res.Err)
	assert.False(t, res.FallbackEligible, "a hardReject verdict must stay correct-or-die")
	assert.Equal(t, "reject", outcomeOf(res, "getLogsFilterSanity"))
}

func TestAutoCorrect_ObserveOnlyStillOutranksEverything(t *testing.T) {
	record := BehaviorRecord
	res := autocorrectValidate(t, true, true, &record)
	require.NoError(t, res.Err, "observe-only must never fail a request, AutoCorrect included")
	assert.False(t, res.FallbackEligible)
	assert.Equal(t, "record_only", outcomeOf(res, "getLogsFilterSanity"),
		"a recordOnly verdict under observe records as record_only: even under enforcement it would have ended served (via fallback), so would_reject would overstate the enforcement cost")
}
