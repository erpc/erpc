package consensus

import (
	"context"
	"errors"
	"fmt"
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

// participant is one upstream's answer, attributed, as the analyzer receives it.
func participant(t *testing.T, upstreamID string, code int, msg string) *execResult {
	t.Helper()
	return &execResult{
		Err:      svmError(t, upstreamID, code, msg),
		Upstream: &svmStubUpstream{id: upstreamID},
	}
}

// decide runs participants through the real analysis and winner rules under
// 2-of-2 returnError — the configuration Solana networks use. Index mirrors
// arrival position, which is what the analyzer assigns.
func decide(t *testing.T, parts ...*execResult) *slotResult {
	t.Helper()
	return decideWithMethod(t, "", parts...)
}

// decideWithMethod is decide with an optional JSON-RPC method on the request
// context (used when classification is method-sensitive).
func decideWithMethod(t *testing.T, method string, parts ...*execResult) *slotResult {
	t.Helper()
	cfg := &config{
		maxParticipants:    len(parts),
		agreementThreshold: len(parts),
		disputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
	}
	for i, p := range parts {
		p.Index = i
	}
	ctx := context.Background()
	if method != "" {
		req := common.NewNormalizedRequest([]byte(
			fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[1]}`, method)))
		ctx = context.WithValue(ctx, common.RequestContextKey, req)
	}
	lg := zerolog.Nop()
	analysis := newConsensusAnalysis(&lg, ctx, cfg, parts)
	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(ctx, &lg, analysis)
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
}

// slotSkippedInputs are agave skipped-slot answers: ExecutionException (not
// MissingData), native wire codes, shared consensus hash svm:slot_skipped.
var slotSkippedInputs = []missingDataInput{
	{"-32007 SlotSkipped", -32007, "Slot 500281501 was skipped, or missing due to ledger jump to recent snapshot"},
	{"-32009 LongTermStorageSlotSkipped", -32009, "Slot 500281501 was skipped, or missing in long-term storage"},
	{"codeless -32000 long-term storage", -32000, "Slot 500281501 was skipped, or missing in long-term storage"},
	{"codeless -32000 ledger jump", -32000, "Slot 500281501 was skipped, or missing due to ledger jump to recent snapshot"},
}

// Two honest providers describing the same absent data must agree, for every
// pair of inputs in a family, not only the pairs seen in production so far.
func TestMissingData_SameVerdictDifferentCodes_Agree(t *testing.T) {
	for _, fam := range missingDataFamilies {
		t.Run(fam.name, func(t *testing.T) {
			parts := make([]*execResult, len(fam.inputs))
			for i, in := range fam.inputs {
				parts[i] = participant(t, "upstream-"+in.label, in.code, in.msg)
				require.True(t, common.HasErrorCode(parts[i].Err, common.ErrCodeEndpointMissingData),
					"%s must classify as missing data", in.label)
				require.Equal(t, fam.permanent, common.IsPermanentlyMissingData(parts[i].Err),
					"%s must be in the %s family", in.label, fam.name)
			}
			for i, a := range fam.inputs {
				for j := i + 1; j < len(fam.inputs); j++ {
					b := fam.inputs[j]
					winner := decide(t, parts[i], parts[j])
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

// Skipped-slot vs transient coverage are different claims: svm:slot_skipped
// must dispute missingdata, not collapse into whichever arrived first.
func TestSlotSkipped_VsTransientMissingData_StillDispute(t *testing.T) {
	winner := decide(t,
		participant(t, "upstream-a", -32007, "Slot 500281501 was skipped, or missing due to ledger jump to recent snapshot"),
		participant(t, "upstream-b", -32004, "Block not available for slot 500281501"),
	)
	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
		"a skipped slot and a transient gap are different claims and must dispute")
}

// The client-facing number is untouched by grouping: consensus compares a
// verdict, but the caller still receives agave's own code.
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

// -32007 / -32009 (and codeless equivalents) must classify as ExecutionException
// — not MissingData — agree under consensus, and return a native Solana code.
func TestSlotSkipped_AgreeAsExecutionExceptionWithNativeCodes(t *testing.T) {
	parts := make([]*execResult, len(slotSkippedInputs))
	for i, in := range slotSkippedInputs {
		parts[i] = participant(t, "upstream-"+in.label, in.code, in.msg)
		require.True(t, common.HasErrorCode(parts[i].Err, common.ErrCodeEndpointExecutionException),
			"%s must classify as ExecutionException, got %v", in.label, parts[i].Err)
		require.False(t, common.HasErrorCode(parts[i].Err, common.ErrCodeEndpointMissingData),
			"%s must not be MissingData", in.label)
		require.True(t, common.IsPermanentlyMissingData(parts[i].Err),
			"%s must be permanent (no delayed re-sweep)", in.label)
		assert.Equal(t, common.SeverityInfo, common.ClassifySeverity(parts[i].Err),
			"%s must be severity info (expected chain outcome)", in.label)
	}
	for i, a := range slotSkippedInputs {
		for j := i + 1; j < len(slotSkippedInputs); j++ {
			b := slotSkippedInputs[j]
			winner := decide(t, parts[i], parts[j])
			require.NotNil(t, winner.Error, "%s vs %s", a.label, b.label)
			assert.False(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
				"%s vs %s: skipped-slot answers must agree", a.label, b.label)
			assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeEndpointExecutionException),
				"%s vs %s: winner must stay ExecutionException, got %v", a.label, b.label, winner.Error)
			var jre *common.ErrJsonRpcExceptionInternal
			require.ErrorAs(t, winner.Error, &jre)
			assert.Contains(t, []common.JsonRpcErrorNumber{-32007, -32009, -32000}, jre.NormalizedCode(),
				"%s vs %s: client must see a native skip code, got %v", a.label, b.label, jre.NormalizedCode())
		}
	}
}

// A skip verdict group can hold -32007 and -32009; the code the caller sees
// must not depend on arrival order (lowest upstream id wins).
func TestSlotSkipped_ReturnedErrorIsOrderIndependent(t *testing.T) {
	const slot = "Slot 500281501 was skipped, or missing"
	codeOf := func(t *testing.T, parts ...*execResult) common.JsonRpcErrorNumber {
		t.Helper()
		winner := decide(t, parts...)
		require.NotNil(t, winner.Error)
		var jre *common.ErrJsonRpcExceptionInternal
		require.ErrorAs(t, winner.Error, &jre)
		return jre.NormalizedCode()
	}
	alchemy := func() *execResult {
		return participant(t, "alchemy-svm", -32007, slot+" due to ledger jump to recent snapshot")
	}
	quicknode := func() *execResult {
		return participant(t, "quicknode-svm", -32009, slot+" in long-term storage")
	}

	first := codeOf(t, alchemy(), quicknode())
	second := codeOf(t, quicknode(), alchemy())
	assert.Equal(t, first, second, "arrival order must not change the code the client sees")
	assert.Equal(t, common.JsonRpcErrorNumber(-32007), first,
		"the lowest upstream id (alchemy-svm) must supply the group's error")
}

// Transient MissingData on getBlock stays a coverage claim.
func TestTransientMissingData_GetBlockStaysMissingData(t *testing.T) {
	winner := decideWithMethod(t, "getBlock",
		participant(t, "upstream-a", -32004, "Block not available for slot 500281501"),
		participant(t, "upstream-b", -32001, "Block 500281501 cleaned up, does not exist on node. First available block: 500300000"),
	)
	require.NotNil(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeEndpointMissingData),
		"transient coverage gaps must still win as MissingData, got: %v", winner.Error)
	assert.False(t, common.IsPermanentlyMissingData(winner.Error))
}

// evmStubUpstream is an EVM-typed upstream for Lotus null-round consensus tests.
type evmStubUpstream struct {
	common.Upstream
	id string
}

func (u *evmStubUpstream) Id() string { return u.id }
func (u *evmStubUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: u.id, Type: common.UpstreamTypeEvm}
}

func lotusNullParticipant(t *testing.T, upstreamID string, epoch uint64) *execResult {
	t.Helper()
	up := &evmStubUpstream{id: upstreamID}
	msg := fmt.Sprintf("requested epoch was a null round (%d)", epoch)
	md := common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(12, 12, msg, nil, map[string]interface{}{"data": float64(epoch)}),
		up,
	)
	md.(*common.ErrEndpointMissingData).WithPermanentMissingData(true)
	return &execResult{Err: md, Upstream: up}
}

func evmBlockParticipant(t *testing.T, upstreamID string, blockJSON string) *execResult {
	t.Helper()
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), []byte(blockJSON), nil)
	require.NoError(t, err)
	up := &evmStubUpstream{id: upstreamID}
	nr := common.NewNormalizedResponse().WithJsonRpcResponse(jrr).SetUpstream(up)
	return &execResult{Result: nr, Upstream: up}
}

func TestFilecoinNullRound_Consensus(t *testing.T) {
	const epoch = 6568208
	const blockJSON = `{"number":"0x643970","hash":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`

	t.Run("two null rounds agree, short-circuit, and preserve native code 12", func(t *testing.T) {
		cfg := &config{
			maxParticipants:    3,
			agreementThreshold: 2,
			disputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
		}
		parts := []*execResult{
			lotusNullParticipant(t, "lotus-a", epoch),
			lotusNullParticipant(t, "lotus-b", epoch),
		}
		for i, p := range parts {
			p.Index = i
		}
		lg := zerolog.Nop()
		analysis := newConsensusAnalysis(&lg, context.Background(), cfg, parts)
		e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
		winner := e.determineWinner(context.Background(), &lg, analysis)

		require.NotNil(t, winner.Error)
		assert.Equal(t, 2, analysis.validParticipants, "null rounds must count as valid consensus participants")
		assert.True(t, common.IsPermanentlyMissingData(winner.Error))
		var jre *common.ErrJsonRpcExceptionInternal
		require.ErrorAs(t, winner.Error, &jre)
		assert.Equal(t, common.JsonRpcErrorNumber(12), jre.NormalizedCode())
		assert.Equal(t, fmt.Sprintf("requested epoch was a null round (%d)", epoch), jre.Message)

		reason, ok := e.shouldShortCircuit(winner, analysis)
		assert.True(t, ok, "agreed null round meeting threshold must short-circuit")
		assert.Equal(t, "consensus_error_threshold", reason)
	})

	t.Run("null round and infra 500 do not merge into false agreement", func(t *testing.T) {
		cfg := &config{
			maxParticipants:         2,
			agreementThreshold:      2,
			disputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		}
		infra := &execResult{
			Err:      common.NewErrEndpointServerSideException(errors.New("500 internal error"), nil, 500),
			Upstream: &evmStubUpstream{id: "lotus-b"},
			Index:    1,
		}
		nullPart := lotusNullParticipant(t, "lotus-a", epoch)
		nullPart.Index = 0

		lg := zerolog.Nop()
		analysis := newConsensusAnalysis(&lg, context.Background(), cfg, []*execResult{nullPart, infra})
		e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
		winner := e.determineWinner(context.Background(), &lg, analysis)

		require.NotNil(t, winner.Error)
		assert.Equal(t, 1, analysis.validParticipants, "only the null round is a valid participant; 500 stays infra")
		assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusLowParticipants),
			"1 null round + 1 HTTP 500 must be low participants, not false agreement: got %v", winner.Error)
	})

	t.Run("null round disputes with transient missing data", func(t *testing.T) {
		upB := &evmStubUpstream{id: "lotus-b"}
		transientErr := common.NewErrEndpointMissingData(
			common.NewErrJsonRpcExceptionInternal(-32000, common.JsonRpcErrorMissingData, "header not found", nil, nil),
			upB,
		)
		require.True(t, common.HasErrorCode(transientErr, common.ErrCodeEndpointMissingData))
		require.False(t, common.IsPermanentlyMissingData(transientErr))

		winner := decide(t,
			lotusNullParticipant(t, "lotus-a", epoch),
			&execResult{Err: transientErr, Upstream: upB},
		)
		require.NotNil(t, winner.Error)
		assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
			"permanent null round and transient missing data must dispute")
	})

	t.Run("minority fork null round loses to two agreeing blocks", func(t *testing.T) {
		cfg := &config{
			maxParticipants:    3,
			agreementThreshold: 2,
			disputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
		}
		parts := []*execResult{
			lotusNullParticipant(t, "lotus-fork", epoch),
			evmBlockParticipant(t, "lotus-a", blockJSON),
			evmBlockParticipant(t, "lotus-b", blockJSON),
		}
		for i, p := range parts {
			p.Index = i
		}
		lg := zerolog.Nop()
		analysis := newConsensusAnalysis(&lg, context.Background(), cfg, parts)
		e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
		winner := e.determineWinner(context.Background(), &lg, analysis)

		require.NoError(t, winner.Error)
		require.NotNil(t, winner.Result)
	})

	t.Run("acceptMostCommon with preferNonEmpty prefers a real block over null rounds", func(t *testing.T) {
		cfg := &config{
			maxParticipants:         3,
			agreementThreshold:      2,
			disputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			preferNonEmpty:          true,
		}
		parts := []*execResult{
			lotusNullParticipant(t, "lotus-a", epoch),
			lotusNullParticipant(t, "lotus-b", epoch),
			evmBlockParticipant(t, "lotus-c", blockJSON),
		}
		for i, p := range parts {
			p.Index = i
		}
		lg := zerolog.Nop()
		analysis := newConsensusAnalysis(&lg, context.Background(), cfg, parts)
		e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
		winner := e.determineWinner(context.Background(), &lg, analysis)

		require.NoError(t, winner.Error)
		require.NotNil(t, winner.Result)
	})
}
