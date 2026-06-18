package consensus

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- helpers -----------------------------------------------------------------

// receiptResultBytes builds an eth_getTransactionReceipt result with one log per
// supplied logIndex. Corrupt (underflowed) logIndex values produce a longer
// byte string than canonical ones, which is exactly what makes the corrupt
// response "larger" in the real-world incident.
func receiptResultBytes(logIndexes []string) []byte {
	logs := make([]string, len(logIndexes))
	for i, li := range logIndexes {
		logs[i] = fmt.Sprintf(`{"address":"0x27b26e88f007ec9109648c6da522fcaba06c74d7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"],"data":"0x","logIndex":"%s","removed":false}`, li)
	}
	return []byte(fmt.Sprintf(`{"blockHash":"0x5a28cc00c288af5a055bba9ea5b202b8406e86138ec94ddfc8e96978c752c28a","blockNumber":"0xe57e13","status":"0x1","transactionIndex":"0x0","logs":[%s]}`, strings.Join(logs, ",")))
}

func receiptResp(t *testing.T, logIndexes ...string) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte("1"), receiptResultBytes(logIndexes), nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrr)
}

// logsResultBytes builds an eth_getLogs result (a top-level array of log objects).
func logsResultBytes(logIndexes []string) []byte {
	logs := make([]string, len(logIndexes))
	for i, li := range logIndexes {
		logs[i] = fmt.Sprintf(`{"address":"0x27b26e88f007ec9109648c6da522fcaba06c74d7","data":"0x","logIndex":"%s"}`, li)
	}
	return []byte("[" + strings.Join(logs, ",") + "]")
}

func logsResp(t *testing.T, logIndexes ...string) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte("1"), logsResultBytes(logIndexes), nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrr)
}

func firstLogIndex(t *testing.T, ctx context.Context, resp *common.NormalizedResponse) string {
	t.Helper()
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	li, err := jrr.PeekStringByPath(ctx, "logs", 0, "logIndex")
	require.NoError(t, err)
	return li
}

var canonicalLogIndexes = []string{"0x0", "0x1", "0x2", "0x3", "0x4", "0x5", "0x6", "0x7", "0x8"}

// 0xfffffff7..0xffffffff == -9..-1 read as int32; the signature of the real bug.
var underflowedLogIndexes = []string{"0xfffffff7", "0xfffffff8", "0xfffffff9", "0xfffffffa", "0xfffffffb", "0xfffffffc", "0xfffffffd", "0xfffffffe", "0xffffffff"}

func receiptConsensusConfig() *config {
	return &config{
		maxParticipants:         3,
		agreementThreshold:      2,
		disputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		preferNonEmpty:          true,
		preferLargerResponses:   true,
	}
}

// --- integrity validator unit tests ------------------------------------------

func TestHasInvalidIntegrity(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name   string
		method string
		resp   *common.NormalizedResponse
		want   bool
	}{
		{
			name:   "canonical receipt logIndexes are valid",
			method: "eth_getTransactionReceipt",
			resp:   receiptResp(t, canonicalLogIndexes...),
			want:   false,
		},
		{
			name:   "underflowed receipt logIndex is invalid",
			method: "eth_getTransactionReceipt",
			resp:   receiptResp(t, underflowedLogIndexes...),
			want:   true,
		},
		{
			name:   "single underflowed logIndex among valid ones is invalid",
			method: "eth_getTransactionReceipt",
			resp:   receiptResp(t, "0x0", "0x1", "0xfffffff7"),
			want:   true,
		},
		{
			name:   "eth_getLogs with valid logIndexes is valid",
			method: "eth_getLogs",
			resp:   logsResp(t, "0x0", "0x1", "0x2"),
			want:   false,
		},
		{
			name:   "eth_getLogs with underflowed logIndex is invalid",
			method: "eth_getLogs",
			resp:   logsResp(t, "0x0", "0xfffffffe"),
			want:   true,
		},
		{
			name:   "non-family method is never flagged (no-op)",
			method: "eth_call",
			resp:   receiptResp(t, underflowedLogIndexes...),
			want:   false,
		},
		{
			name:   "nil response is valid",
			method: "eth_getTransactionReceipt",
			resp:   nil,
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hasInvalidIntegrity(ctx, tc.resp, tc.method))
		})
	}
}

func TestIsImplausibleIndex_Boundary(t *testing.T) {
	// Just below the ceiling is plausible; at/above is not.
	assert.False(t, isImplausibleIndex(fmt.Sprintf("0x%x", maxPlausibleEvmLogIndex-1)))
	assert.True(t, isImplausibleIndex(fmt.Sprintf("0x%x", maxPlausibleEvmLogIndex)))
	assert.True(t, isImplausibleIndex("0xfffffff7")) // the real-incident value
	assert.False(t, isImplausibleIndex("0x0"))
	assert.False(t, isImplausibleIndex("not-hex")) // unparseable -> not flagged
}

// --- consensus scenario tests ------------------------------------------------

// TestConsensus_CorruptLargerLogIndex_ServesHonestMajority recreates the
// Polygon Amoy incident: two upstreams return the canonical receipt (logIndex
// 0..8) and a third returns a byte-larger receipt whose logIndex values are
// underflowed garbage (0xfffffff7..). With disputeBehavior=returnError and
// preferLargerResponses=true (a default), the corrupt-but-larger minority used
// to convert the honest 2/3 majority into an ErrConsensusDispute. After the fix
// the corrupt group is excluded from the prefer-larger candidate set and the
// canonical receipt is served.
func TestConsensus_CorruptLargerLogIndex_ServesHonestMajority(t *testing.T) {
	lg := zerolog.Nop()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"]}`))
	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)

	responses := []*execResult{
		{Result: receiptResp(t, canonicalLogIndexes...), Index: 0},
		{Result: receiptResp(t, canonicalLogIndexes...), Index: 1},
		{Result: receiptResp(t, underflowedLogIndexes...), Index: 2},
	}

	cfg := receiptConsensusConfig()
	cfg.logger = &lg

	analysis := newConsensusAnalysis(&lg, ctx, cfg, responses)

	// Sanity: there are two groups; the corrupt one is genuinely larger AND flagged invalid.
	require.Len(t, analysis.groups, 2, "canonical (x2) and corrupt (x1) must form distinct groups")
	var correct, corrupt *responseGroup
	for _, g := range analysis.groups {
		if g.IntegrityInvalid {
			corrupt = g
		} else {
			correct = g
		}
	}
	require.NotNil(t, correct, "expected an integrity-valid (canonical) group")
	require.NotNil(t, corrupt, "expected the underflowed group to be flagged IntegrityInvalid")
	assert.Equal(t, 2, correct.Count)
	assert.Equal(t, 1, corrupt.Count)
	assert.Greater(t, corrupt.ResponseSize, correct.ResponseSize, "corrupt response must be the larger one")

	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(&lg, analysis)

	require.NotNil(t, winner)
	require.Nil(t, winner.Error, "honest 2/3 majority must win, not ErrConsensusDispute")
	require.NotNil(t, winner.Result, "a result must be served")
	assert.Equal(t, "0x0", firstLogIndex(t, ctx, winner.Result),
		"served receipt must be the canonical one (logIndex 0..8), never the corrupt larger response")
}

// TestConsensus_GenuineLargerValidResponse_StillDisputes guards against
// over-correction: when the larger minority response is itself VALID (e.g. one
// node returned more logs because the majority missed some), the strict
// returnError + preferLargerResponses behavior must be preserved and still
// dispute. The integrity gate must only suppress provably-corrupt larger
// responses, not legitimate ones.
func TestConsensus_GenuineLargerValidResponse_StillDisputes(t *testing.T) {
	lg := zerolog.Nop()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"]}`))
	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)

	smaller := []string{"0x0", "0x1", "0x2", "0x3", "0x4"}
	larger := canonicalLogIndexes // 0..8, more logs, all valid

	responses := []*execResult{
		{Result: receiptResp(t, smaller...), Index: 0},
		{Result: receiptResp(t, smaller...), Index: 1},
		{Result: receiptResp(t, larger...), Index: 2},
	}

	cfg := receiptConsensusConfig()
	cfg.logger = &lg

	analysis := newConsensusAnalysis(&lg, ctx, cfg, responses)

	// Neither group is integrity-invalid here (all logIndexes are plausible).
	for _, g := range analysis.groups {
		assert.False(t, g.IntegrityInvalid, "valid responses must not be flagged")
	}

	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(&lg, analysis)

	require.NotNil(t, winner)
	require.NotNil(t, winner.Error, "a genuine larger valid response must still dispute under returnError+preferLargerResponses")
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
		"expected ErrConsensusDispute, preserving the strict prefer-larger behavior")
}
