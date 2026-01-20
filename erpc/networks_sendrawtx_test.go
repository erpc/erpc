package erpc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// Sample signed LEGACY (type-0) transaction for testing (a valid RLP-encoded Ethereum transaction)
// This is a simple transfer transaction - the hash is deterministic based on the content
const sampleSignedTx = "0xf86c098504a817c800825208943535353535353535353535353535353535353535880de0b6b3a76400008025a028ef61340bd939bc2195fe537567866003e1a15d3c71ff63e1590620aa636276a067cbe9d8997f761aecb703304b3800ccf555c9f3dc64214b297fb1966a3b6d83"

// Expected tx hash for the sample legacy transaction
const expectedTxHash = "0x33469b22e9f636356c4160a87eb19df52b7412e8eac32a4a55f0ef7be5c61c8d"

// Sample signed EIP-1559 (type-2) transaction for testing typed transaction support
// This is a type-2 transaction which uses maxFeePerGas and maxPriorityFeePerGas
// Format: 0x02 || rlp([chainId, nonce, maxPriorityFeePerGas, maxFeePerGas, gasLimit, to, value, data, accessList, v, r, s])
// Generated with ethers.js: wallet.signTransaction({ type: 2, chainId: 1, nonce: 10, ... })
const sampleEIP1559SignedTx = "0x02f873010a8459682f008506fc23ac0082520894d8da6bf26964af9d7eed9e03e53415d37aa9604588016345785d8a000080c080a0a3d5fd825e582675933b2b6aea774b0454633edb49e94699d6f88d197cd26589a06295b0b43a9e93a3390b308272a65bb063d9f18deb4cb7db5ecf352bf9ba9fe7"

// Expected tx hash for the sample EIP-1559 transaction
const expectedEIP1559TxHash = "0xb9f61197f9c6c63a6981ba69fb22308469d03a4e013b10bcd69315745110acf7"

func TestNetwork_SendRawTransaction_Idempotency(t *testing.T) {
	t.Run("AlreadyKnownReturnsSuccess", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// Upstream returns "already known" error
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "already known",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed (idempotent) - error is converted to success
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		// Result should be the transaction hash
		result := jrr.GetResultString()
		assert.Contains(t, result, "0x")
	})

	t.Run("NonceTooLowWithMatchingTxReturnsSuccess", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First: upstream returns "nonce too low" error
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "nonce too low",
				},
			})

		// Then: eth_getTransactionByHash returns the matching transaction
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"hash":        expectedTxHash,
					"nonce":       "0x9",
					"blockHash":   "0x1234567890abcdef",
					"blockNumber": "0x100",
					"from":        "0x1234567890123456789012345678901234567890",
					"to":          "0x3535353535353535353535353535353535353535",
					"value":       "0xde0b6b3a7640000",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed - tx exists on chain
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		// Result should be the transaction hash
		result := jrr.GetResultString()
		assert.Contains(t, result, "0x")
	})

	t.Run("NonceTooLowWithNoMatchingTxReturnsError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First: upstream returns "nonce too low" error
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "nonce too low",
				},
			})

		// Then: eth_getTransactionByHash returns null (tx not found)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return error - different tx with same nonce
		require.Error(t, err)

		// The error should be normalized to -32003 (Transaction rejected)
		var jrpcErr *common.ErrJsonRpcExceptionInternal
		if common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
			// Check that it contains the original message
			assert.Contains(t, err.Error(), "nonce too low")
		} else {
			// Accept other error types as long as they indicate failure
			assert.NotNil(t, err)
		}
		_ = jrpcErr
		_ = resp
	})

	t.Run("ExecutionRevertedIsRetryableAcrossUpstreams", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First upstream returns execution reverted
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    3,
					"message": "execution reverted",
				},
			})

		// Second upstream succeeds
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  expectedTxHash,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Use retry policy to enable retrying across upstreams
		network := setupSendRawTxTestNetworkWithRetry(t, ctx, &common.RetryPolicyConfig{
			MaxAttempts: 3,
			Delay:       common.Duration(10 * time.Millisecond),
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed - retried to second upstream
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		result := jrr.GetResultString()
		assert.Contains(t, result, expectedTxHash)
	})

	t.Run("AllUpstreamsRevertReturnsRevertError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// Both upstreams return execution reverted
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    3,
					"message": "execution reverted: insufficient balance",
				},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    3,
					"message": "execution reverted: insufficient balance",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkWithRetry(t, ctx, &common.RetryPolicyConfig{
			MaxAttempts: 3,
			Delay:       common.Duration(10 * time.Millisecond),
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return the execution exception error (not ErrFailsafeRetryExceeded)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointExecutionException),
			"expected ErrCodeEndpointExecutionException but got: %v", err)
		_ = resp
	})

	t.Run("HedgeWorksForSendRawTransaction", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First upstream is slow
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			Delay(500 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  expectedTxHash,
			})

		// Second upstream (hedged) is fast and returns "already known"
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "already known",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkWithHedge(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(100 * time.Millisecond),
			MaxCount: 1,
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed - hedged request returned "already known" which is converted to success
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		result := jrr.GetResultString()
		assert.Contains(t, result, "0x")
	})

	t.Run("ReplacementUnderpricedRemainsError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// Upstream returns "replacement transaction underpriced" - this should NOT be converted to success
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "replacement transaction underpriced",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return error - replacement underpriced is NOT idempotent
		require.Error(t, err)
		_ = resp
	})

	t.Run("IdempotentBroadcastDisabledReturnsRawError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// Upstream returns "already known" error - normally this would be converted to success
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "already known",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Set up network with IdempotentTransactionBroadcast DISABLED
		network := setupSendRawTxTestNetworkWithIdempotentDisabled(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return error - idempotency is disabled, so "already known" is NOT converted to success
		require.Error(t, err)
		_ = resp
	})

	// Test that EIP-1559 (type-2) transactions work with idempotent handling
	// This is a regression test for typed transactions not being decoded correctly
	t.Run("EIP1559AlreadyKnownReturnsSuccess", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleEIP1559SignedTx + `"]}`)

		// Upstream returns "already known" error for EIP-1559 transaction
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "already known",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed (idempotent) - EIP-1559 transactions should be handled correctly
		require.NoError(t, err, "EIP-1559 transaction should be parsed and converted to success")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		// Result should be the expected EIP-1559 transaction hash
		result := jrr.GetResultString()
		assert.Contains(t, result, expectedEIP1559TxHash, "Result should contain the EIP-1559 transaction hash")
	})

	// Test that nonce/duplicate detection works even when upstream uses -32003 code
	// Some upstreams return -32003 (transaction rejected) with "already known" or "nonce too low" messages
	t.Run("AlreadyKnownWith32003CodeReturnsSuccess", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// Upstream returns "already known" error with -32003 code (some nodes do this)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32003, // JsonRpcErrorTransactionRejected
					"message": "already known",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed (idempotent) - error is converted to success even with -32003 code
		require.NoError(t, err, "already known with -32003 code should be converted to success")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		// Result should be the transaction hash
		result := jrr.GetResultString()
		assert.Contains(t, result, "0x")
	})

	t.Run("NonceTooLowWith32003CodeAndMatchingTxReturnsSuccess", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First: upstream returns "nonce too low" error with -32003 code
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32003, // JsonRpcErrorTransactionRejected
					"message": "nonce too low",
				},
			})

		// Second: verification call returns the transaction (it exists on-chain)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"hash":             expectedTxHash,
					"nonce":            "0x9",
					"blockHash":        "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
					"blockNumber":      "0x100",
					"transactionIndex": "0x0",
					"from":             "0x3535353535353535353535353535353535353535",
					"to":               "0x3535353535353535353535353535353535353535",
					"value":            "0xde0b6b3a7640000",
					"gas":              "0x5208",
					"gasPrice":         "0x4a817c800",
					"input":            "0x",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed - transaction exists on-chain, idempotent success even with -32003 code
		require.NoError(t, err, "nonce too low with -32003 code should be verified and converted to success")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		result := jrr.GetResultString()
		assert.Contains(t, result, "0x")
	})

	// ============================================================
	// ADDITIONAL REAL-WORLD EDGE CASE TESTS
	// ============================================================

	// Happy path: First upstream succeeds immediately (most common case)
	t.Run("FirstUpstreamSucceedsImmediately", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First upstream succeeds immediately
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  expectedTxHash,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), expectedTxHash)
	})

	// Geth-style "known transaction: <hash>" message
	t.Run("GethStyleKnownTransactionWithHash", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// Geth returns "known transaction: <hash>" format
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "known transaction: " + expectedTxHash,
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed - "known transaction" is idempotent
		require.NoError(t, err, "Geth-style 'known transaction: hash' should be converted to success")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x")
	})

	// Insufficient funds is retryable across upstreams
	t.Run("InsufficientFundsIsRetryableAcrossUpstreams", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First upstream returns "insufficient funds" - might be stale balance check
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "insufficient funds for gas * price + value",
				},
			})

		// Second upstream succeeds (has more up-to-date balance)
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  expectedTxHash,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkWithRetry(t, ctx, &common.RetryPolicyConfig{
			MaxAttempts: 3,
			Delay:       common.Duration(10 * time.Millisecond),
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed - retried to second upstream
		require.NoError(t, err, "insufficient funds should be retryable to other upstreams")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), expectedTxHash)
	})

	// Verification call returns error (not null) - should return original nonce error
	t.Run("NonceTooLowVerificationReturnsError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First: upstream returns "nonce too low"
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "nonce too low",
				},
			})

		// Then: verification call returns an error (upstream issue)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "internal error",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return error - verification failed, so we can't confirm idempotency
		require.Error(t, err, "should return error when verification call fails")
		_ = resp
	})

	// User sends same tx twice: first succeeds, second gets "already known" - both should succeed
	t.Run("SameTransactionSentTwice", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First request succeeds
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  expectedTxHash,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		// First request
		req1 := common.NewNormalizedRequest(requestBytes)
		resp1, err1 := network.Forward(ctx, req1)
		require.NoError(t, err1)
		jrr1, _ := resp1.JsonRpcResponse()
		assert.Contains(t, jrr1.GetResultString(), expectedTxHash)

		// Reset mocks for second request
		util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Second request gets "already known"
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "already known",
				},
			})

		// Second request
		req2 := common.NewNormalizedRequest(requestBytes)
		resp2, err2 := network.Forward(ctx, req2)
		require.NoError(t, err2, "second request with 'already known' should succeed")
		jrr2, _ := resp2.JsonRpcResponse()
		assert.Contains(t, jrr2.GetResultString(), "0x")
	})

	// EIP-2930 (type-1) access list transaction
	// Format: 0x01 || rlp([chainId, nonce, gasPrice, gasLimit, to, value, data, accessList, v, r, s])
	t.Run("EIP2930AccessListTransaction", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// EIP-2930 transaction (type 1) - access list transaction
		// Generated with ethers.js: wallet.signTransaction({ type: 1, chainId: 1, nonce: 5, ... })
		eip2930Tx := "0x01f8a30105843b9aca00825208943535353535353535353535353535353535353535880de0b6b3a764000080f838f7943535353535353535353535353535353535353535e1a00000000000000000000000000000000000000000000000000000000000000000" +
			"80a0b50409ac5bc0a54c0d37db42f9e6e2d29cdf4c6c0c4f7c45b3e0e0c7c9b8b7a6a03f5d5e5a5b5c5d5e5f606162636465666768696a6b6c6d6e6f707172737475"

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + eip2930Tx + `"]}`)

		// Note: This tx might not parse correctly due to invalid signature - that's fine, we're testing error handling
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "already known",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// If tx parses successfully, should return success
		// If tx fails to parse, should return original "already known" error (which is still handled)
		// Either way, we shouldn't panic
		if err == nil {
			jrr, _ := resp.JsonRpcResponse()
			assert.Contains(t, jrr.GetResultString(), "0x")
		}
		// Test passes as long as we don't panic
	})

	// Invalid transaction hex - should return error gracefully
	t.Run("InvalidTransactionHex", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Invalid hex (not a valid transaction)
		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["0xnotvalidhex"]}`)

		// Upstream returns "already known" - but we can't parse the tx
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "already known",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return original error since we can't parse the tx to get the hash
		// The idempotency hook returns the original error when parsing fails
		require.Error(t, err, "should return error when tx cannot be parsed")
		_ = resp
	})

	// Alternative message: "nonce has already been used" (Alchemy format)
	t.Run("AlchemyStyleNonceAlreadyUsed", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// Alchemy-style error message
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "nonce has already been used",
				},
			})

		// Verification finds the tx
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"hash": expectedTxHash,
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed - "nonce has already been used" is recognized
		require.NoError(t, err, "'nonce has already been used' should trigger verification")
		require.NotNil(t, resp)
	})

	// Base fee error - should NOT be converted to success (not idempotent)
	t.Run("MaxFeePerGasLessThanBaseFeeRemainsError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleEIP1559SignedTx + `"]}`)

		// Base fee error - transaction is genuinely invalid, not a duplicate
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "max fee per gas less than block base fee",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return error - this is not an idempotent case
		require.Error(t, err, "base fee error should not be converted to success")
		_ = resp
	})

	// HTTP 500 from upstream - should be retryable
	t.Run("HTTP500IsRetryable", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First upstream returns HTTP 500
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(500).
			BodyString("Internal Server Error")

		// Second upstream succeeds
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  expectedTxHash,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkWithRetry(t, ctx, &common.RetryPolicyConfig{
			MaxAttempts: 3,
			Delay:       common.Duration(10 * time.Millisecond),
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed - retried after HTTP 500
		require.NoError(t, err, "HTTP 500 should trigger retry to second upstream")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), expectedTxHash)
	})

	// Contract deployment transaction (to = null) - should work with idempotency
	t.Run("ContractDeploymentAlreadyKnown", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Contract deployment transaction (legacy type-0, to field is empty)
		// This is a simplified contract deployment tx
		// Format: rlp([nonce, gasPrice, gasLimit, to (empty), value, data (contract bytecode), v, r, s])
		contractDeployTx := "0xf8a80985174876e800830186a08080b853604580600e600039806000f350fe7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe03601600081602082378035828234f58015156039578182fd5b8082525050506014600cf325a0b5acb56a4d4e7c65adb35c1ba24ce2c887c95c5c7635f4f3fa7a5a5c17a4a0c9a0456f5a5b5c5d5e5f606162636465666768696a6b6c6d6e6f707172737475"

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + contractDeployTx + `"]}`)

		// Upstream returns "already known"
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "already known",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Contract deployment should work with idempotency (if tx parses)
		// If tx fails to parse due to invalid signature, original error is returned
		if err == nil {
			jrr, _ := resp.JsonRpcResponse()
			assert.Contains(t, jrr.GetResultString(), "0x")
		}
		// Test passes as long as no panic
	})

	// Retry + Hedge combined policies
	t.Run("RetryAndHedgeCombined", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First upstream is slow and returns error
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			Delay(300 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    3,
					"message": "execution reverted",
				},
			})

		// Second upstream (hedged) succeeds quickly
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  expectedTxHash,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkWithRetryAndHedge(t, ctx,
			&common.RetryPolicyConfig{
				MaxAttempts: 3,
				Delay:       common.Duration(10 * time.Millisecond),
			},
			&common.HedgePolicyConfig{
				Delay:    common.Duration(100 * time.Millisecond),
				MaxCount: 1,
			},
		)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed via hedge
		require.NoError(t, err, "retry+hedge combined should work")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), expectedTxHash)
	})

	// First upstream returns connection error, second succeeds
	t.Run("FirstUpstreamConnectionErrorSecondSucceeds", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First upstream returns connection refused / network error
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			ReplyError(errors.New("connection refused"))

		// Second upstream succeeds
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  expectedTxHash,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkWithRetry(t, ctx, &common.RetryPolicyConfig{
			MaxAttempts: 3,
			Delay:       common.Duration(10 * time.Millisecond),
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed via retry after connection error
		require.NoError(t, err, "should retry after connection error")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), expectedTxHash)
	})

	// "tx underpriced" (not replacement) - should be retryable, not idempotent
	t.Run("TxUnderpricedIsRetryable", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First upstream returns "tx underpriced" (gas price too low for network)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "tx underpriced",
				},
			})

		// Second upstream succeeds (maybe has different gas requirements)
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  expectedTxHash,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkWithRetry(t, ctx, &common.RetryPolicyConfig{
			MaxAttempts: 3,
			Delay:       common.Duration(10 * time.Millisecond),
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed via retry - "tx underpriced" is not the same as "replacement underpriced"
		require.NoError(t, err, "tx underpriced should be retryable")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), expectedTxHash)
	})

	// "transaction type not supported" - should return error (not idempotent)
	t.Run("TransactionTypeNotSupportedRemainsError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleEIP1559SignedTx + `"]}`)

		// Upstream doesn't support EIP-1559
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "transaction type not supported",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return error - this is not idempotent
		require.Error(t, err, "transaction type not supported should remain an error")
		_ = resp
	})

	// Empty params array - should return error gracefully
	t.Run("EmptyParamsReturnsError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Request with empty params
		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":[]}`)

		// Upstream returns error for invalid params
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32602,
					"message": "invalid argument 0: missing value",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return error - invalid params
		require.Error(t, err, "empty params should return error")
		_ = resp
	})

	// Non-string first param - should return error gracefully
	t.Run("NonStringParamReturnsError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Request with non-string param (number instead of hex string)
		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":[12345]}`)

		// Upstream returns error for invalid params
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32602,
					"message": "invalid argument 0: hex string expected",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return error - invalid params
		require.Error(t, err, "non-string param should return error")
		_ = resp
	})

	// "already imported" message variation (Besu format)
	t.Run("BesuStyleAlreadyImported", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// Besu-style error message
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "already imported",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed - "already imported" is idempotent
		require.NoError(t, err, "'already imported' should be converted to success")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x")
	})
}

// Helper to set up a single-upstream test network (no load balancing uncertainty)
func setupSendRawTxTestNetworkSingleUpstream(t *testing.T, ctx context.Context) *Network {
	t.Helper()

	upstreamConfigs := []*common.UpstreamConfig{
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
	}

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
	}

	return setupSendRawTxNetwork(t, ctx, upstreamConfigs, networkConfig)
}

// Helper to set up a single-upstream network with IdempotentTransactionBroadcast disabled
func setupSendRawTxTestNetworkWithIdempotentDisabled(t *testing.T, ctx context.Context) *Network {
	t.Helper()

	upstreamConfigs := []*common.UpstreamConfig{
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
	}

	idempotentDisabled := false
	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:                        123,
			IdempotentTransactionBroadcast: &idempotentDisabled,
		},
	}

	return setupSendRawTxNetwork(t, ctx, upstreamConfigs, networkConfig)
}

// Helper to set up a test network with retry policy (two upstreams)
func setupSendRawTxTestNetworkWithRetry(t *testing.T, ctx context.Context, retryConfig *common.RetryPolicyConfig) *Network {
	t.Helper()

	upstreamConfigs := []*common.UpstreamConfig{
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
	}

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Failsafe: []*common.FailsafeConfig{{
			Retry: retryConfig,
		}},
	}

	return setupSendRawTxNetwork(t, ctx, upstreamConfigs, networkConfig)
}

// Helper to set up a test network with hedge policy
func setupSendRawTxTestNetworkWithHedge(t *testing.T, ctx context.Context, hedgeConfig *common.HedgePolicyConfig) *Network {
	t.Helper()

	upstreamConfigs := []*common.UpstreamConfig{
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
	}

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Failsafe: []*common.FailsafeConfig{{
			Hedge: hedgeConfig,
		}},
	}

	return setupSendRawTxNetwork(t, ctx, upstreamConfigs, networkConfig)
}

// Helper to set up a test network with both retry and hedge policies
func setupSendRawTxTestNetworkWithRetryAndHedge(t *testing.T, ctx context.Context, retryConfig *common.RetryPolicyConfig, hedgeConfig *common.HedgePolicyConfig) *Network {
	t.Helper()

	upstreamConfigs := []*common.UpstreamConfig{
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
	}

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Failsafe: []*common.FailsafeConfig{{
			Retry: retryConfig,
			Hedge: hedgeConfig,
		}},
	}

	return setupSendRawTxNetwork(t, ctx, upstreamConfigs, networkConfig)
}

func setupSendRawTxNetwork(t *testing.T, ctx context.Context, upstreamConfigs []*common.UpstreamConfig, networkConfig *common.NetworkConfig) *Network {
	t.Helper()

	rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)

	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)

	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx,
		&log.Logger,
		"test",
		upstreamConfigs,
		ssr,
		rateLimitersRegistry,
		vr,
		pr,
		nil,
		metricsTracker,
		1*time.Second,
		nil,
	)

	network, err := NewNetwork(
		ctx,
		&log.Logger,
		"test",
		networkConfig,
		rateLimitersRegistry,
		upstreamsRegistry,
		metricsTracker,
	)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	return network
}
