package erpc

import (
	"context"
	"encoding/json"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
	"testing"
)

func init() { util.ConfigureTestLogger() }

func TestNetworkExecutor_RetryPendingStopsAtConfirmedTransaction(t *testing.T) {
	for _, method := range []string{"eth_getTransactionReceipt", "eth_getTransactionByHash", "eth_getTransactionByBlockHashAndIndex", "eth_getTransactionByBlockNumberAndIndex"} {
		for _, tc := range []struct {
			name   string
			result string
			want   string
		}{
			{"confirmed", `{"blockNumber":"0x123","blockHash":"0xabc"}`, ""},
			{"zero height", `{"blockNumber":"0x0"}`, ""},
			{"pending", `{"blockNumber":null}`, "pending_tx"},
			{"no inclusion", `{"hash":"0xabc"}`, "pending_tx"},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`))
				req.SetDirectives(&common.RequestDirectives{RetryPending: true})
				var result interface{}
				require.NoError(t, json.Unmarshal([]byte(tc.result), &result))
				jrr, err := common.NewJsonRpcResponse(1, result, nil)
				require.NoError(t, err)
				resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
				defer resp.Release()
				e := &networkExecutor{cfg: &common.NetworkFailsafeConfig{Retry: &common.RetryPolicyConfig{MaxAttempts: 4, EmptyResultMaxAttempts: 2}}}
				require.Equal(t, tc.want, e.shouldRetryWithReason(req, resp, nil, 0))
				require.Empty(t, e.shouldRetryWithReason(req, resp, nil, 1), "pending recovery remains capped")
				req.SetDirectives(&common.RequestDirectives{RetryPending: false})
				require.Empty(t, e.shouldRetryWithReason(req, resp, nil, 0), "explicit disable remains effective")
			})
		}
	}
}

func TestNetworkExecutor_ConfirmedLookupFinishesWithoutExtraBackendCalls(t *testing.T) {
	for _, pendingFirst := range []bool{false, true} {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":[]}`))
		req.SetDirectives(&common.RequestDirectives{RetryPending: true})
		e := &networkExecutor{cfg: &common.NetworkFailsafeConfig{Retry: &common.RetryPolicyConfig{MaxAttempts: 4, EmptyResultMaxAttempts: 4}}}
		calls := 0
		got, err := e.runRetry(context.Background(), req, func(context.Context) (*common.NormalizedResponse, error) {
			calls++
			result := map[string]interface{}{"blockNumber": interface{}("0x123")}
			if pendingFirst && calls == 1 {
				result["blockNumber"] = nil
			}
			jrr, err := common.NewJsonRpcResponse(1, result, nil)
			require.NoError(t, err)
			return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
		})
		require.NoError(t, err)
		require.NotNil(t, got)
		defer got.Release()
		want := 1
		if pendingFirst {
			want = 2
		}
		require.Equal(t, want, calls)
	}
}
