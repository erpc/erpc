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

type missingDataInput struct {
	label string
	code  int
	msg   string
}

// missingDataFamilies is every agave-shaped body the SVM normalizer classifies
// as missing data, split by permanence. Agave numbers a missing block by which
// storage tier answered, not by what it found, and vendor proxies sometimes
// strip the code but keep agave's message. Consensus must group each family as
// one verdict whatever number or wording a provider uses.
var missingDataFamilies = []struct {
	name      string
	permanent bool
	inputs    []missingDataInput
}{
	{
		name:      "transient",
		permanent: false,
		inputs: []missingDataInput{
			{"-32001 BlockCleanedUp", -32001, "Block 500281501 cleaned up, does not exist on node. First available block: 500300000"},
			{"-32004 BlockNotAvailable", -32004, "Block not available for slot 500281501"},
			{"-32008 NoSnapshot", -32008, "No snapshot"},
			{"-32010 KeyExcludedFromSecondaryIndex", -32010, "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA excluded from account secondary indexes; this RPC method unavailable for key"},
			{"-32011 TransactionHistoryNotAvailable", -32011, "Transaction history is not available from this node"},
			{"-32014 BlockStatusNotAvailableYet", -32014, "Block status not yet available for slot 500281501"},
		},
	},
	{
		name:      "permanent",
		permanent: true,
		inputs: []missingDataInput{
			{"-32007 SlotSkipped", -32007, "Slot 500281501 was skipped, or missing due to ledger jump to recent snapshot"},
			{"-32009 LongTermStorageSlotSkipped", -32009, "Slot 500281501 was skipped, or missing in long-term storage"},
			{"codeless -32000 long-term storage", -32000, "Slot 500281501 was skipped, or missing in long-term storage"},
			{"codeless -32000 ledger jump", -32000, "Slot 500281501 was skipped, or missing due to ledger jump to recent snapshot"},
		},
	},
}

// Two honest providers describing the same absent data must agree, for every
// pair of inputs in a family, not only the pairs seen in production so far.
func TestMissingData_SameVerdictDifferentCodes_Agree(t *testing.T) {
	for _, fam := range missingDataFamilies {
		t.Run(fam.name, func(t *testing.T) {
			errs := make([]error, len(fam.inputs))
			for i, in := range fam.inputs {
				errs[i] = svmError(t, "upstream-"+in.label, in.code, in.msg)
				require.True(t, common.HasErrorCode(errs[i], common.ErrCodeEndpointMissingData),
					"%s must classify as missing data", in.label)
				require.Equal(t, fam.permanent, common.IsPermanentlyMissingData(errs[i]),
					"%s must be in the %s family", in.label, fam.name)
			}
			for i, a := range fam.inputs {
				for j := i + 1; j < len(fam.inputs); j++ {
					b := fam.inputs[j]
					winner := decide(t, errs[i], errs[j])
					require.NotNil(t, winner.Error, "%s vs %s", a.label, b.label)
					assert.False(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
						"%s vs %s: the same verdict must agree, not dispute", a.label, b.label)
					assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeEndpointMissingData),
						"%s vs %s: the agreed missing-data verdict must reach the caller", a.label, b.label)
				}
			}
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
	for _, fam := range missingDataFamilies {
		for _, in := range fam.inputs {
			err := svmError(t, "upstream-a", in.code, in.msg)
			var jre *common.ErrJsonRpcExceptionInternal
			require.ErrorAs(t, err, &jre)
			assert.Equal(t, common.JsonRpcErrorNumber(in.code), jre.NormalizedCode(),
				"%s must reach the client with its own code", in.label)
		}
	}
}
