package solana

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// buildReq is a helper to construct a NormalizedRequest from a JSON-RPC body.
func buildReq(body string) *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(body))
}

// ── neverCacheMethods ────────────────────────────────────────────────────────

func TestGetFinality_NeverCacheMethods(t *testing.T) {
	ctx := context.Background()
	methods := []string{
		"getLatestBlockhash",
		"getRecentBlockhash",
		"getFeeForMessage",
		"sendTransaction",
		"sendRawTransaction",
		"simulateTransaction",
		"getSignatureStatuses",
		"getSignatureStatus",
		"getVoteAccounts",
		"getLeaderSchedule",
		"getEpochInfo",
		"getEpochSchedule",
		"getSlotLeaders",
		"getRecentPerformanceSamples",
		"getRecentPrioritizationFees",
		"requestAirdrop",
		// Real-time cluster / node state
		"getSlot",
		"getBlockHeight",
		"getHealth",
		"getFirstAvailableBlock",
		"getHighestSnapshotSlot",
		"getMaxRetransmitSlot",
		"getMaxShredInsertSlot",
	}
	for _, method := range methods {
		req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`)
		got := GetFinality(ctx, nil, req, nil)
		assert.Equal(t, common.DataFinalityStateRealtime, got,
			"method %q should map to Realtime (never cache)", method)
	}
}

// ── historical methods default to Finalized when no commitment is given ──────
// (Solana's own default commitment is "finalized".)

func TestGetFinality_HistoricalMethodsDefaultFinalized(t *testing.T) {
	ctx := context.Background()
	methods := []string{
		"getBlock",
		"getTransaction",
		"getConfirmedBlock",
		"getConfirmedTransaction",
		"getInflationReward",
		"getBlocks",
		"getBlockTime",
		"getSignaturesForAddress",
	}
	for _, method := range methods {
		req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`)
		got := GetFinality(ctx, nil, req, nil)
		assert.Equal(t, common.DataFinalityStateFinalized, got,
			"method %q with no commitment should map to Finalized", method)
	}
}

// ── commitment governs even historical methods (regression: confirmed/processed
//    getBlock/getTransaction must NOT be cached as finalized — can roll back) ──

func TestGetFinality_HistoricalMethodWeakerCommitment(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		method     string
		commitment string
		want       common.DataFinalityState
	}{
		{"getBlock", "confirmed", common.DataFinalityStateUnfinalized},
		{"getBlock", "processed", common.DataFinalityStateRealtime},
		{"getTransaction", "confirmed", common.DataFinalityStateUnfinalized},
		{"getTransaction", "processed", common.DataFinalityStateRealtime},
		{"getBlock", "finalized", common.DataFinalityStateFinalized},
	}
	for _, c := range cases {
		req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"` + c.method +
			`","params":[123,{"commitment":"` + c.commitment + `"}]}`)
		got := GetFinality(ctx, nil, req, nil)
		assert.Equal(t, c.want, got,
			"%q at commitment %q", c.method, c.commitment)
	}
}

// ── commitment mapping ────────────────────────────────────────────────────────

func TestGetFinality_CommitmentFinalized(t *testing.T) {
	ctx := context.Background()
	req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["addr",{"commitment":"finalized"}]}`)
	assert.Equal(t, common.DataFinalityStateFinalized, GetFinality(ctx, nil, req, nil))
}

func TestGetFinality_CommitmentConfirmed(t *testing.T) {
	ctx := context.Background()
	req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["addr",{"commitment":"confirmed"}]}`)
	assert.Equal(t, common.DataFinalityStateUnfinalized, GetFinality(ctx, nil, req, nil))
}

func TestGetFinality_CommitmentProcessed(t *testing.T) {
	ctx := context.Background()
	req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["addr",{"commitment":"processed"}]}`)
	assert.Equal(t, common.DataFinalityStateRealtime, GetFinality(ctx, nil, req, nil))
}

func TestGetFinality_MissingCommitmentDefaultsToFinalized(t *testing.T) {
	ctx := context.Background()
	// No commitment field in params object
	req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["addr",{}]}`)
	assert.Equal(t, common.DataFinalityStateFinalized, GetFinality(ctx, nil, req, nil))
}

func TestGetFinality_NoParamsDefaultsToFinalized(t *testing.T) {
	ctx := context.Background()
	req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":[]}`)
	assert.Equal(t, common.DataFinalityStateFinalized, GetFinality(ctx, nil, req, nil))
}

func TestGetFinality_CommitmentCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["addr",{"commitment":"CONFIRMED"}]}`)
	assert.Equal(t, common.DataFinalityStateUnfinalized, GetFinality(ctx, nil, req, nil))
}

// ── NeverCache takes priority over alwaysFinalized ───────────────────────────

func TestGetFinality_NeverCacheBeatsAlwaysFinalized(t *testing.T) {
	// sendTransaction is in neverCache; it should never be Finalized regardless
	ctx := context.Background()
	req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["base64tx",{"commitment":"finalized"}]}`)
	assert.Equal(t, common.DataFinalityStateRealtime, GetFinality(ctx, nil, req, nil))
}

// ── extractCommitment unit tests ─────────────────────────────────────────────

func TestExtractCommitment_LastParamObject(t *testing.T) {
	req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["addr",{"commitment":"confirmed","encoding":"base58"}]}`)
	assert.Equal(t, "confirmed", extractCommitment(req))
}

func TestExtractCommitment_NoConfigObject(t *testing.T) {
	req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["addr"]}`)
	assert.Equal(t, "", extractCommitment(req))
}

func TestExtractCommitment_EmptyParams(t *testing.T) {
	req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":[]}`)
	assert.Equal(t, "", extractCommitment(req))
}

func TestExtractCommitment_LastParamNotObject(t *testing.T) {
	// Last param is a number, not an object
	req := buildReq(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[299000000]}`)
	assert.Equal(t, "", extractCommitment(req))
}
