package consensus

import (
	"net/http"
	"testing"

	"github.com/erpc/erpc/architecture/svm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// svmStubUpstream is an SVM-typed upstream; the SVM extractor no-ops unless
// Config().Type says SVM, and reads nothing else.
type svmStubUpstream struct {
	common.Upstream
	id string
}

func (u *svmStubUpstream) Id() string { return u.id }
func (u *svmStubUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: u.id, Type: common.UpstreamTypeSvm}
}

// svmError drives the real SVM normalizer with an agave-shaped error body, so
// these tests exercise the same path a live getBlock failure takes.
func svmError(t *testing.T, upstreamID string, code int, msg string) error {
	t.Helper()
	e := svm.NewJsonRpcErrorExtractor()
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	body := common.NewErrJsonRpcExceptionExternal(code, msg, "")
	err := e.Extract(resp, nil, common.MustNewJsonRpcResponse(1, nil, body), &svmStubUpstream{id: upstreamID})
	require.NotNil(t, err, "extractor must classify code %d", code)
	return err
}

// decide runs two participant errors through classification, grouping and the
// winner rules under 2-of-2 returnError — the configuration Solana networks use.
func decide(t *testing.T, errs ...error) *slotResult {
	t.Helper()
	cfg := &config{
		maxParticipants:    len(errs),
		agreementThreshold: len(errs),
		disputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
	}
	analysis := &consensusAnalysis{
		config:            cfg,
		groups:            make(map[string]*responseGroup),
		totalParticipants: len(errs),
		method:            "getBlock",
	}
	for i, err := range errs {
		r := &execResult{Err: err, Index: i}
		classifyAndHashResponse(r, nil, cfg)
		if r.CachedResponseType != ResponseTypeInfrastructureError {
			analysis.validParticipants++
		}
		group, ok := analysis.groups[r.CachedHash]
		if !ok {
			group = &responseGroup{Hash: r.CachedHash, ResponseType: r.CachedResponseType, ResponseSize: r.CachedResponseSize}
			analysis.groups[r.CachedHash] = group
		}
		group.Count++
		group.Results = append(group.Results, r)
		if group.FirstError == nil {
			group.FirstError = err
		}
	}
	lg := zerolog.Nop()
	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(&lg, analysis)
	require.NotNil(t, winner)
	return winner
}

// Agave numbers a missing block by which storage tier answered, not by what it
// found, so two honest providers describing the same absent data reported
// different JSON-RPC codes and disputed. The verdict is what consensus compares.
func TestMissingData_SameVerdictDifferentCodes_Agree(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codeA int
		msgA  string
		codeB int
		msgB  string
	}{
		{
			// check_blockstore_root settles it locally; check_bigtable_result
			// had to ask long-term storage. Same skipped slot either way.
			name:  "skipped slot: local blockstore vs long-term storage",
			codeA: -32007, msgA: "Slot 500281501 was skipped, or missing due to ledger jump to recent snapshot",
			codeB: -32009, msgB: "Slot 500281501 was skipped, or missing in long-term storage",
		},
		{
			// One node pruned its ledger below the slot, the other has not
			// rooted it yet. Both mean "not from me, try someone else".
			name:  "unservable block: pruned ledger vs unrooted slot",
			codeA: -32001, msgA: "Block cleaned up, does not exist on node. First available block: 500000000",
			codeB: -32004, msgB: "Block not available for slot 500281501",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			winner := decide(t,
				svmError(t, "upstream-a", tc.codeA, tc.msgA),
				svmError(t, "upstream-b", tc.codeB, tc.msgB),
			)
			require.NotNil(t, winner.Error)
			assert.False(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
				"two upstreams reporting the same verdict must agree, not dispute")
			assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeEndpointMissingData),
				"the agreed-upon missing-data verdict must reach the caller")
		})
	}
}

// Permanence is a real disagreement about the chain, not a naming difference:
// "skipped for good" against "not indexed yet" must stay a dispute, because the
// dispute is what routes the request into the wait-and-retry that settles it.
func TestMissingData_PermanentVsTransient_StillDispute(t *testing.T) {
	winner := decide(t,
		svmError(t, "upstream-a", -32007, "Slot 500281501 was skipped, or missing due to ledger jump to recent snapshot"),
		svmError(t, "upstream-b", -32004, "Block not available for slot 500281501"),
	)
	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
		"a permanent skip and a transient gap are different claims and must dispute")
}

// The client-facing number is untouched by this grouping: consensus compares a
// verdict, but the caller still receives agave's own code, which @solana/kit
// maps 1:1 to its SolanaError classes.
func TestMissingData_WireCodesStayNative(t *testing.T) {
	for _, code := range []int{-32007, -32009, -32001, -32004} {
		err := svmError(t, "upstream-a", code, "Slot 500281501 unavailable")
		var jre *common.ErrJsonRpcExceptionInternal
		require.ErrorAs(t, err, &jre)
		assert.Equal(t, common.JsonRpcErrorNumber(code), jre.NormalizedCode(),
			"code %d must still reach the client unchanged", code)
	}
}
