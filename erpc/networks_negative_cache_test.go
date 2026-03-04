package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNetwork_DeterministicNegativeCache(t *testing.T) {
	t.Run("FinalizedExecutionError_ReusedWithinTTL", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var upstreamCalls atomic.Int32

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if strings.Contains(body, "eth_call") {
					upstreamCalls.Add(1)
					return true
				}
				return false
			}).
			Persist().
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    3,
					"message": "execution reverted",
					"data":    "0x08c379a0",
				},
			})

		network := setupTestNetworkForMultiplexer(t, ctx)
		requestBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"0x10"]}`)

		req1 := common.NewNormalizedRequest(requestBody)
		_, err := network.Forward(ctx, req1)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointExecutionException))

		req2 := common.NewNormalizedRequest(requestBody)
		_, err = network.Forward(ctx, req2)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointExecutionException))

		assert.Equal(t, int32(1), upstreamCalls.Load(), "second request should hit deterministic negative cache")
	})

	t.Run("PendingNonceSensitiveMethod_BypassesNegativeCache", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var upstreamCalls atomic.Int32

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if strings.Contains(body, "eth_getTransactionCount") {
					upstreamCalls.Add(1)
					return true
				}
				return false
			}).
			Persist().
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32602,
					"message": "invalid argument 1: hex string has length 3, want 40 for common.Address",
				},
			})

		network := setupTestNetworkForMultiplexer(t, ctx)
		requestBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionCount","params":["0xabc","pending"]}`)

		req1 := common.NewNormalizedRequest(requestBody)
		_, err := network.Forward(ctx, req1)
		require.Error(t, err)
		assert.True(t, common.IsClientError(err))

		req2 := common.NewNormalizedRequest(requestBody)
		_, err = network.Forward(ctx, req2)
		require.Error(t, err)
		assert.True(t, common.IsClientError(err))

		assert.Equal(t, int32(2), upstreamCalls.Load(), "pending/nonce-sensitive methods must bypass negative cache")
	})
}
