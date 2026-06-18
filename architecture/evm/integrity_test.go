package evm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func receiptResultBytes(logIndexes []string) []byte {
	logs := make([]string, len(logIndexes))
	for i, li := range logIndexes {
		logs[i] = fmt.Sprintf(`{"address":"0x27b26e88f007ec9109648c6da522fcaba06c74d7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"],"data":"0x","logIndex":"%s","removed":false}`, li)
	}
	return []byte(fmt.Sprintf(`{"blockHash":"0x5a28cc00c288af5a055bba9ea5b202b8406e86138ec94ddfc8e96978c752c28a","blockNumber":"0xe57e13","status":"0x1","transactionIndex":"0x0","logs":[%s]}`, strings.Join(logs, ",")))
}

func logsResultBytes(logIndexes []string) []byte {
	logs := make([]string, len(logIndexes))
	for i, li := range logIndexes {
		logs[i] = fmt.Sprintf(`{"address":"0x27b26e88f007ec9109648c6da522fcaba06c74d7","data":"0x","logIndex":"%s"}`, li)
	}
	return []byte("[" + strings.Join(logs, ",") + "]")
}

func respFromResult(method string, result []byte) *common.NormalizedResponse {
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"%s","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"]}`, method)))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), result, nil)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

var canonicalLogIndexes = []string{"0x0", "0x1", "0x2", "0x3", "0x4", "0x5", "0x6", "0x7", "0x8"}

// 0xfffffff7..0xffffffff == -9..-1 read as int32; the signature of the real bug.
var underflowedLogIndexes = []string{"0xfffffff7", "0xfffffff8", "0xfffffff9", "0xfffffffa", "0xfffffffb", "0xfffffffc", "0xfffffffd", "0xfffffffe", "0xffffffff"}

func TestValidateIndexIntegrity(t *testing.T) {
	ctx := context.Background()
	u := common.NewFakeUpstream("test-upstream")

	tests := []struct {
		name       string
		resp       *common.NormalizedResponse
		wantErr    bool
		wantInBody string
	}{
		{
			name:    "canonical receipt is valid",
			resp:    respFromResult("eth_getTransactionReceipt", receiptResultBytes(canonicalLogIndexes)),
			wantErr: false,
		},
		{
			name:       "underflowed receipt logIndex is a content-validation error",
			resp:       respFromResult("eth_getTransactionReceipt", receiptResultBytes(underflowedLogIndexes)),
			wantErr:    true,
			wantInBody: "logIndex",
		},
		{
			name:       "single underflowed logIndex among valid ones is rejected",
			resp:       respFromResult("eth_getTransactionReceipt", receiptResultBytes([]string{"0x0", "0x1", "0xfffffff7"})),
			wantErr:    true,
			wantInBody: "logIndex",
		},
		{
			name:       "underflowed transactionIndex is rejected",
			resp:       respFromResult("eth_getTransactionReceipt", []byte(`{"blockNumber":"0xe57e13","transactionIndex":"0xfffffffe","logs":[]}`)),
			wantErr:    true,
			wantInBody: "transactionIndex",
		},
		{
			name:    "eth_getLogs array with valid logIndexes is valid",
			resp:    respFromResult("eth_getLogs", logsResultBytes([]string{"0x0", "0x1", "0x2"})),
			wantErr: false,
		},
		{
			name:       "eth_getLogs array with underflowed logIndex is rejected",
			resp:       respFromResult("eth_getLogs", logsResultBytes([]string{"0x0", "0xfffffffe"})),
			wantErr:    true,
			wantInBody: "logIndex",
		},
		{
			name:    "null result is valid (nothing to check)",
			resp:    respFromResult("eth_getTransactionReceipt", []byte("null")),
			wantErr: false,
		},
		{
			name:    "empty logs array is valid",
			resp:    respFromResult("eth_getLogs", []byte("[]")),
			wantErr: false,
		},
		{
			name:    "nil response is valid",
			resp:    nil,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIndexIntegrity(ctx, u, tc.resp)
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointContentValidation),
				"expected ErrEndpointContentValidation, got %v", err)
			if tc.wantInBody != "" {
				assert.Contains(t, err.Error(), tc.wantInBody)
			}
		})
	}
}

func TestIsImplausibleIndex(t *testing.T) {
	assert.False(t, isImplausibleIndex(fmt.Sprintf("0x%x", maxPlausibleEvmIndex-1)))
	assert.True(t, isImplausibleIndex(fmt.Sprintf("0x%x", maxPlausibleEvmIndex)))
	assert.True(t, isImplausibleIndex("0xfffffff7")) // the real-incident value
	assert.False(t, isImplausibleIndex("0x0"))
	assert.False(t, isImplausibleIndex("0x8"))
	assert.False(t, isImplausibleIndex("not-hex")) // unparseable -> not flagged
}

// TestHandleUpstreamPostForward_LogIndexIntegrity verifies the wiring: a corrupt
// receipt is converted to a content-validation error by the post-forward hook
// (so retry/consensus route around it), while a canonical receipt and a
// non-family method pass through untouched.
func TestHandleUpstreamPostForward_LogIndexIntegrity(t *testing.T) {
	ctx := context.Background()
	u := common.NewFakeUpstream("test-upstream")

	t.Run("corrupt receipt becomes a content-validation error", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"]}`))
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), receiptResultBytes(underflowedLogIndexes), nil)
		rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

		_, err := HandleUpstreamPostForward(ctx, nil, u, req, rs, nil, false)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointContentValidation))
	})

	t.Run("canonical receipt passes through", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"]}`))
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), receiptResultBytes(canonicalLogIndexes), nil)
		rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

		gotRs, err := HandleUpstreamPostForward(ctx, nil, u, req, rs, nil, false)
		require.NoError(t, err)
		assert.Equal(t, rs, gotRs)
	})

	t.Run("non-family method is not integrity-checked", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{},"latest"]}`))
		// A value that would be flagged as an implausible index if this method were checked.
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(`{"logIndex":"0xfffffff7"}`), nil)
		rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

		_, err := HandleUpstreamPostForward(ctx, nil, u, req, rs, nil, false)
		assert.NoError(t, err)
	})
}
