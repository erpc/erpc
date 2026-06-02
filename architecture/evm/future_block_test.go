package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fbI64(v int64) *int64 { return &v }

// fbNetwork builds a testNetwork with the future-block distance bound configured
// and a fixed highest-latest head, using the default mark-empty method list.
func fbNetwork(distance *int64, head int64) *testNetwork {
	return &testNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				MarkEmptyAsErrorMethods:     common.DefaultMarkEmptyAsErrorMethods(),
				MaxFutureBlockRetryDistance: distance,
			},
		},
		highestLatestBlock: head,
	}
}

// fbRequest builds an eth_getBlockByNumber request and, when blockNumber > 0,
// pins the resolved numeric block so the predicate is exercised deterministically
// without depending on param normalization.
func fbRequest(method string, blockNumber int64) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":["0x1"]}`))
	if blockNumber > 0 {
		req.SetEvmBlockNumber(blockNumber)
	}
	return req
}

func TestEmptyResultTruthfulForFutureBlock(t *testing.T) {
	const head = int64(1000)

	cases := []struct {
		name     string
		distance *int64
		block    int64 // 0 => leave the request without a concrete numeric block
		want     bool
	}{
		{"bound nil disables the check", nil, 5000, false},
		{"below head is not future", fbI64(1), 999, false},
		{"exactly at head is not future", fbI64(1), 1000, false},
		{"inside the slack window is not future", fbI64(5), 1003, false},
		{"exactly head+slack is not future", fbI64(1), 1001, false},
		{"one past head+slack is future", fbI64(1), 1002, true},
		{"far ahead of head is future", fbI64(1), 5000, true},
		{"zero slack: head+1 is future", fbI64(0), 1001, true},
		{"zero slack: head is not future", fbI64(0), 1000, false},
		{"negative bound disables the check", fbI64(-1), 5000, false},
		{"no concrete block (tag) is not future", fbI64(1), 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := fbNetwork(tc.distance, head)
			req := fbRequest("eth_getBlockByNumber", tc.block)
			got := EmptyResultTruthfulForFutureBlock(context.Background(), n, req)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestEmptyResultTruthfulForFutureBlock_ColdHeadFailsOpen(t *testing.T) {
	// head == 0 (e.g. a cold state poller): even a far-future block must not be
	// classified as future, so existing retry-on-empty behavior is preserved and a
	// real block is never reported as empty due to a temporarily-unknown head.
	n := fbNetwork(fbI64(1), 0)
	req := fbRequest("eth_getBlockByNumber", 5000)
	assert.False(t, EmptyResultTruthfulForFutureBlock(context.Background(), n, req))
}

func TestEmptyResultTruthfulForFutureBlock_NilSafety(t *testing.T) {
	assert.False(t, EmptyResultTruthfulForFutureBlock(context.Background(), nil, nil))

	n := fbNetwork(fbI64(1), 1000)
	assert.False(t, EmptyResultTruthfulForFutureBlock(context.Background(), n, nil))
}

func TestEmptyResultTruthfulForFutureBlock_NonEvmConfig(t *testing.T) {
	// A network without an Evm config block never opts in.
	n := &testNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm}, highestLatestBlock: 1000}
	req := fbRequest("eth_getBlockByNumber", 5000)
	assert.False(t, EmptyResultTruthfulForFutureBlock(context.Background(), n, req))
}

func TestEmptyResultTruthfulForFutureBlock_TagNotFuture(t *testing.T) {
	// A "latest"/"pending"/etc. tag has no concrete number and must never be
	// treated as a future block, regardless of how far the head is.
	n := fbNetwork(fbI64(1), 1000)
	for _, tag := range []string{"latest", "pending", "finalized", "safe", "earliest"} {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["` + tag + `"]}`))
		assert.False(t, EmptyResultTruthfulForFutureBlock(context.Background(), n, req), tag)
	}
}

// --- Hook-level behavior: the actual fix routed through HandleUpstreamPostForward ---

func fbEmptyResponse(t *testing.T, req *common.NormalizedRequest) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`"1"`), []byte("null"), nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func fbBlockRequest(t *testing.T, blockNumber int64) *common.NormalizedRequest {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1"]}`))
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})
	req.SetEvmBlockNumber(blockNumber)
	return req
}

func TestHandleUpstreamPostForward_FutureBlock_KeepsEmptyResult(t *testing.T) {
	// Block well beyond head+slack: the empty result is truthful and must NOT be
	// converted into a retryable missing-data error.
	n := fbNetwork(fbI64(1), 1000)
	req := fbBlockRequest(t, 2000)
	resp := fbEmptyResponse(t, req)

	out, err := HandleUpstreamPostForward(context.Background(), n, nil, req, resp, nil, false)

	assert.NoError(t, err, "future-block empty must be kept, not marked missing-data")
	assert.Equal(t, resp, out, "response should pass through unchanged")
}

func TestHandleUpstreamPostForward_AtOrBelowHead_MarksMissingData(t *testing.T) {
	// Block at/below head: empty is unexpected (the data should exist), so the
	// existing rotate-to-another-upstream behavior must be preserved.
	n := fbNetwork(fbI64(1), 1000)
	for _, bn := range []int64{1, 999, 1000, 1001} { // 1001 == head+slack, still not "future"
		req := fbBlockRequest(t, bn)
		resp := fbEmptyResponse(t, req)

		_, err := HandleUpstreamPostForward(context.Background(), n, nil, req, resp, nil, false)

		assert.Error(t, err, "block %d should still be marked missing-data", bn)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData), "block %d", bn)
	}
}

func TestHandleUpstreamPostForward_FutureBlock_BoundDisabled_MarksMissingData(t *testing.T) {
	// With the bound unset (default), behavior is unchanged: even a far-future
	// block's empty result is marked missing-data.
	n := fbNetwork(nil, 1000)
	req := fbBlockRequest(t, 5000)
	resp := fbEmptyResponse(t, req)

	_, err := HandleUpstreamPostForward(context.Background(), n, nil, req, resp, nil, false)

	assert.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
}

func TestHandleUpstreamPostForward_FutureBlock_ColdHead_MarksMissingData(t *testing.T) {
	// Head unknown (cold poller): fail open to the existing behavior rather than
	// nulling a block we cannot place relative to the head.
	n := fbNetwork(fbI64(1), 0)
	req := fbBlockRequest(t, 5000)
	resp := fbEmptyResponse(t, req)

	_, err := HandleUpstreamPostForward(context.Background(), n, nil, req, resp, nil, false)

	assert.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
}

func TestHandleUpstreamPostForward_FutureBlock_RetryEmptyFalse_NoError(t *testing.T) {
	// An explicit retryEmpty=false already short-circuits marking; the future-block
	// gate must not change that (still no error).
	n := fbNetwork(fbI64(1), 1000)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1"]}`))
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: false})
	req.SetEvmBlockNumber(int64(2000))
	resp := fbEmptyResponse(t, req)

	_, err := HandleUpstreamPostForward(context.Background(), n, nil, req, resp, nil, false)
	assert.NoError(t, err)
}
