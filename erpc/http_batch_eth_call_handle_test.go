package erpc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestHandleEthCallBatchAggregation_EarlyReturn(t *testing.T) {
	server := &HttpServer{serverCfg: &common.ServerConfig{IncludeErrorDetails: &common.TRUE}}
	startedAt := time.Now()
	req := httptest.NewRequest("POST", "http://localhost", nil)
	req.RemoteAddr = "127.0.0.1:1234"

	handled := server.handleEthCallBatchAggregation(
		context.Background(),
		&startedAt,
		req,
		nil,
		log.Logger,
		nil,
		nil,
		req.Header,
		req.URL.Query(),
		nil,
	)

	assert.False(t, handled)
}

func TestHandleEthCallBatchAggregation_RequestAndAuthErrors(t *testing.T) {
	t.Run("validation error", func(t *testing.T) {
		cfg := baseBatchConfig()
		server, project, ctx, cleanup := setupBatchHandler(t, cfg)
		defer cleanup()

		requests := []json.RawMessage{
			json.RawMessage(`{"jsonrpc":"2.0","id":1}`),
			json.RawMessage(`{"jsonrpc":"2.0","id":2}`),
		}

		handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), requests, nil)
		require.True(t, handled)
		require.Len(t, responses, len(requests))
		for _, resp := range responses {
			require.NotNil(t, resp)
		}
	})

	t.Run("auth payload error", func(t *testing.T) {
		cfg := baseBatchConfig()
		server, project, ctx, cleanup := setupBatchHandler(t, cfg)
		defer cleanup()

		headers := http.Header{}
		headers.Set("Authorization", "Basic !!!")

		handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), validBatchRequests(t), headers)
		require.True(t, handled)
		for _, resp := range responses {
			require.NotNil(t, resp)
		}
	})

	t.Run("auth unauthorized", func(t *testing.T) {
		cfg := baseBatchConfig()
		cfg.Projects[0].Auth = &common.AuthConfig{
			Strategies: []*common.AuthStrategyConfig{
				{
					Type: common.AuthTypeSecret,
					Secret: &common.SecretStrategyConfig{
						Id:    "secret",
						Value: "s3cr3t",
					},
				},
			},
		}

		server, project, ctx, cleanup := setupBatchHandler(t, cfg)
		defer cleanup()

		handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), validBatchRequests(t), nil)
		require.True(t, handled)
		for _, resp := range responses {
			require.NotNil(t, resp)
		}
	})
}

func TestHandleEthCallBatchAggregation_NetworkAndRateLimitErrors(t *testing.T) {
	t.Run("network not found", func(t *testing.T) {
		cfg := baseBatchConfig()
		server, project, ctx, cleanup := setupBatchHandler(t, cfg)
		defer cleanup()

		batchInfo := &ethCallBatchInfo{networkId: "evm:999", blockRef: "latest", blockParam: "latest"}
		handled, responses := runHandle(t, ctx, server, project, batchInfo, validBatchRequests(t), nil)
		require.True(t, handled)
		for _, resp := range responses {
			require.NotNil(t, resp)
		}
	})

	t.Run("project rate limit", func(t *testing.T) {
		cfg := baseBatchConfig()
		cfg.RateLimiters = rateLimitConfig("project-budget")
		cfg.Projects[0].RateLimitBudget = "project-budget"

		server, project, ctx, cleanup := setupBatchHandler(t, cfg)
		defer cleanup()

		handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), validBatchRequests(t), nil)
		require.True(t, handled)
		for _, resp := range responses {
			require.NotNil(t, resp)
		}
	})

	t.Run("network rate limit", func(t *testing.T) {
		cfg := baseBatchConfig()
		cfg.RateLimiters = rateLimitConfig("network-budget")
		cfg.Projects[0].Networks[0].RateLimitBudget = "network-budget"

		server, project, ctx, cleanup := setupBatchHandler(t, cfg)
		defer cleanup()

		handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), validBatchRequests(t), nil)
		require.True(t, handled)
		for _, resp := range responses {
			require.NotNil(t, resp)
		}
	})
}

func TestHandleEthCallBatchAggregation_FallbackPaths(t *testing.T) {
	cfg := baseBatchConfig()
	server, project, ctx, cleanup := setupBatchHandler(t, cfg)
	defer cleanup()

	validRequests := validBatchRequests(t)
	invalidRequests := invalidBatchRequests(t)
	singleRequest := validRequests[:1]

	cases := []struct {
		name              string
		requests          []json.RawMessage
		networkResponse   func() (*common.NormalizedResponse, error)
		expectedProjCalls int
		expectedNetCalls  int
	}{
		{
			name:              "single candidate",
			requests:          singleRequest,
			networkResponse:   func() (*common.NormalizedResponse, error) { return nil, nil },
			expectedProjCalls: 1,
		},
		{
			name:              "build error",
			requests:          invalidRequests,
			networkResponse:   func() (*common.NormalizedResponse, error) { return nil, nil },
			expectedProjCalls: 2,
		},
		{
			name:     "forward error fallback",
			requests: validRequests,
			networkResponse: func() (*common.NormalizedResponse, error) {
				// Use "contract not found" to trigger ShouldFallbackMulticall3
				// Note: "execution reverted" does NOT trigger fallback (would also fail individually)
				return nil, common.NewErrEndpointExecutionException(errors.New("contract not found"))
			},
			expectedProjCalls: 2,
			expectedNetCalls:  1,
		},
		{
			name:              "mc response nil",
			requests:          validRequests,
			networkResponse:   func() (*common.NormalizedResponse, error) { return nil, nil },
			expectedProjCalls: 2,
			expectedNetCalls:  1,
		},
		{
			name:     "mc response error",
			requests: validRequests,
			networkResponse: func() (*common.NormalizedResponse, error) {
				jrr := mustJsonRpcResponse(t, 1, nil, common.NewErrJsonRpcExceptionExternal(-32000, "boom", ""))
				return common.NewNormalizedResponse().WithJsonRpcResponse(jrr), nil
			},
			expectedProjCalls: 2,
			expectedNetCalls:  1,
		},
		{
			name:     "result unmarshal error",
			requests: validRequests,
			networkResponse: func() (*common.NormalizedResponse, error) {
				jrr := mustJsonRpcResponse(t, 1, map[string]interface{}{"oops": "nope"}, nil)
				return common.NewNormalizedResponse().WithJsonRpcResponse(jrr), nil
			},
			expectedProjCalls: 2,
			expectedNetCalls:  1,
		},
		{
			name:     "result hex error",
			requests: validRequests,
			networkResponse: func() (*common.NormalizedResponse, error) {
				jrr := mustJsonRpcResponse(t, 1, "0xzz", nil)
				return common.NewNormalizedResponse().WithJsonRpcResponse(jrr), nil
			},
			expectedProjCalls: 2,
			expectedNetCalls:  1,
		},
		{
			name:     "decode error",
			requests: validRequests,
			networkResponse: func() (*common.NormalizedResponse, error) {
				jrr := mustJsonRpcResponse(t, 1, "0x01", nil)
				return common.NewNormalizedResponse().WithJsonRpcResponse(jrr), nil
			},
			expectedProjCalls: 2,
			expectedNetCalls:  1,
		},
		{
			name:     "length mismatch",
			requests: validRequests,
			networkResponse: func() (*common.NormalizedResponse, error) {
				resultHex := encodeAggregate3Results([]evm.Multicall3Result{{Success: true, ReturnData: []byte{0x01}}})
				jrr := mustJsonRpcResponse(t, 1, resultHex, nil)
				return common.NewNormalizedResponse().WithJsonRpcResponse(jrr), nil
			},
			expectedProjCalls: 2,
			expectedNetCalls:  1,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			projCalls := 0
			netCalls := 0

			withBatchStubs(t,
				func(ctx context.Context, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
					mu.Lock()
					netCalls++
					mu.Unlock()
					return tt.networkResponse()
				},
				func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
					mu.Lock()
					projCalls++
					mu.Unlock()
					return fallbackResponse(t, req), nil
				},
				nil,
			)

			handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), tt.requests, nil)
			require.True(t, handled)
			mu.Lock()
			assert.Equal(t, tt.expectedProjCalls, projCalls)
			assert.Equal(t, tt.expectedNetCalls, netCalls)
			mu.Unlock()
			if len(responses) > 0 {
				_, ok := responses[0].(*common.NormalizedResponse)
				assert.True(t, ok)
			}
		})
	}
}

func TestHandleEthCallBatchAggregation_NonFallbackError(t *testing.T) {
	cfg := baseBatchConfig()
	server, project, ctx, cleanup := setupBatchHandler(t, cfg)
	defer cleanup()

	projCalls := 0
	withBatchStubs(t,
		func(ctx context.Context, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, errors.New("boom")
		},
		func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			projCalls++
			return fallbackResponse(t, req), nil
		},
		nil,
	)

	handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), validBatchRequests(t), nil)
	require.True(t, handled)
	assert.Equal(t, 0, projCalls)
	for _, resp := range responses {
		_, ok := resp.(*common.NormalizedResponse)
		assert.False(t, ok)
	}
}

func TestHandleEthCallBatchAggregation_NewJsonRpcResponseError(t *testing.T) {
	cfg := baseBatchConfig()
	server, project, ctx, cleanup := setupBatchHandler(t, cfg)
	defer cleanup()

	results := []evm.Multicall3Result{{Success: true, ReturnData: []byte{0xaa}}, {Success: true, ReturnData: []byte{0xbb}}}
	resultHex := encodeAggregate3Results(results)

	withBatchStubs(t,
		func(ctx context.Context, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrr := mustJsonRpcResponse(t, 1, resultHex, nil)
			return common.NewNormalizedResponse().WithJsonRpcResponse(jrr), nil
		},
		func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return fallbackResponse(t, req), nil
		},
		func(id interface{}, result interface{}, rpcError *common.ErrJsonRpcExceptionExternal) (*common.JsonRpcResponse, error) {
			return nil, errors.New("boom")
		},
	)

	handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), validBatchRequests(t), nil)
	require.True(t, handled)
	for _, resp := range responses {
		_, ok := resp.(*common.NormalizedResponse)
		assert.False(t, ok)
	}
}

func TestHandleEthCallBatchAggregation_SuccessAndFailureResults(t *testing.T) {
	cfg := baseBatchConfig()
	server, project, ctx, cleanup := setupBatchHandler(t, cfg)
	defer cleanup()

	results := []evm.Multicall3Result{
		{Success: true, ReturnData: []byte{0xaa}},
		{Success: false, ReturnData: []byte{0xbb}},
	}
	resultHex := encodeAggregate3Results(results)

	withBatchStubs(t,
		func(ctx context.Context, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrr := mustJsonRpcResponse(t, 1, resultHex, nil)
			return common.NewNormalizedResponse().WithJsonRpcResponse(jrr), nil
		},
		nil,
		nil,
	)

	handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), validBatchRequests(t), nil)
	require.True(t, handled)
	require.Len(t, responses, 2)

	resp0, ok := responses[0].(*common.NormalizedResponse)
	require.True(t, ok)
	jrr, err := resp0.JsonRpcResponse(ctx)
	require.NoError(t, err)
	var decodedHex string
	require.NoError(t, common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &decodedHex))
	assert.Equal(t, "0x"+hex.EncodeToString(results[0].ReturnData), decodedHex)

	errResp, ok := responses[1].(*HttpJsonRpcErrorResponse)
	require.True(t, ok)
	errMap, ok := errResp.Error.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "0x"+hex.EncodeToString(results[1].ReturnData), errMap["data"])
}

func TestHandleEthCallBatchAggregation_PanicRecovery(t *testing.T) {
	t.Run("panic in fallback forward is recovered", func(t *testing.T) {
		cfg := baseBatchConfig()
		server, project, ctx, cleanup := setupBatchHandler(t, cfg)
		defer cleanup()

		// Panic recovery exists in forwardEthCallBatchCandidates (fallback path)
		// The multicall3 network forward doesn't have panic recovery, but the
		// fallback path does. So we make network forward fail to trigger fallback,
		// then have the project forward (fallback) panic.
		withBatchStubs(t,
			func(ctx context.Context, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				// Return "contract not found" error to trigger fallback via ShouldFallbackMulticall3
				return nil, common.NewErrEndpointExecutionException(errors.New("contract not found"))
			},
			func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				// Panic in the fallback forward - this should be recovered
				panic("test panic in fallback forward")
			},
			nil,
		)

		// The function should not crash - panic in fallback path should be recovered
		handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), validBatchRequests(t), nil)
		require.True(t, handled)
		// We expect error responses due to the panic
		require.Len(t, responses, 2)
		for _, resp := range responses {
			require.NotNil(t, resp, "response should not be nil after panic recovery")
		}
	})
}

func TestHandleEthCallBatchAggregation_DetectEthCallBatchInfo_EmptyParams(t *testing.T) {
	// Empty params in eth_call is valid - the block param defaults to "latest".
	// This test verifies that empty params requests are handled correctly.
	requests := []json.RawMessage{
		json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`),
		json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"eth_call","params":[]}`),
	}

	// detectEthCallBatchInfo should return valid info with "latest" block ref
	info, err := detectEthCallBatchInfo(requests, "evm", "123")
	require.NoError(t, err)
	require.NotNil(t, info, "empty params defaults to latest - should be valid for batching")
	assert.Equal(t, "evm:123", info.networkId)
	assert.Equal(t, "latest", info.blockRef)
	assert.Equal(t, interface{}("latest"), info.blockParam)
}

func TestHandleEthCallBatchAggregation_CacheHits(t *testing.T) {
	t.Run("all cached - no multicall3 call", func(t *testing.T) {
		cfg := baseBatchConfig()
		server, project, network, ctx, cleanup := setupBatchHandlerWithCache(t, cfg)
		defer cleanup()

		// Set up mock cache
		mockCache := &common.MockCacheDal{}
		network.cacheDal = mockCache

		// Create cached responses
		cachedResp1 := createCachedResponse(t, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x"},"latest"]}`)), "0xcached1")
		cachedResp2 := createCachedResponse(t, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x"},"latest"]}`)), "0xcached2")

		// Mock cache hits for both requests
		mockCache.On("Get", mock.Anything, mock.Anything).Return(cachedResp1, nil).Once()
		mockCache.On("Get", mock.Anything, mock.Anything).Return(cachedResp2, nil).Once()

		// Track if multicall3 was called (it shouldn't be)
		multicall3Called := false
		withBatchStubs(t,
			func(ctx context.Context, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				multicall3Called = true
				return nil, errors.New("should not be called")
			},
			nil,
			nil,
		)

		handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), validBatchRequests(t), nil)
		require.True(t, handled)
		require.False(t, multicall3Called, "multicall3 should not be called when all requests are cached")
		require.Len(t, responses, 2)

		// Verify both responses are from cache
		for i, resp := range responses {
			nr, ok := resp.(*common.NormalizedResponse)
			require.True(t, ok, "response %d should be NormalizedResponse", i)
			assert.True(t, nr.FromCache(), "response %d should be from cache", i)
		}

		mockCache.AssertExpectations(t)
	})

	t.Run("mixed cached and uncached", func(t *testing.T) {
		cfg := baseBatchConfig()
		server, project, network, ctx, cleanup := setupBatchHandlerWithCache(t, cfg)
		defer cleanup()

		// Set up mock cache - first request cached, second not
		mockCache := &common.MockCacheDal{}
		network.cacheDal = mockCache

		// Create cached response for first request
		cachedResp := createCachedResponse(t, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x"},"latest"]}`)), "0xcached_first")

		// First request - cache hit, second request - cache miss
		mockCache.On("Get", mock.Anything, mock.Anything).Return(cachedResp, nil).Once()
		mockCache.On("Get", mock.Anything, mock.Anything).Return(nil, nil).Once()

		// Cache set should be called for the uncached request
		mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

		// Only the second (uncached) request should go through multicall3
		// But since we only have 1 uncached request, it falls back to individual forwarding
		withBatchStubs(t,
			nil,
			func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				// This is the fallback for single uncached request
				return fallbackResponse(t, req), nil
			},
			nil,
		)

		handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), validBatchRequests(t), nil)
		require.True(t, handled)
		require.Len(t, responses, 2)

		// First response should be from cache
		resp0, ok := responses[0].(*common.NormalizedResponse)
		require.True(t, ok)
		assert.True(t, resp0.FromCache())

		// Second response should be from fallback (not cache)
		_, ok = responses[1].(*common.NormalizedResponse)
		require.True(t, ok)

		mockCache.AssertExpectations(t)
	})

	t.Run("cache write after successful multicall3", func(t *testing.T) {
		cfg := baseBatchConfig()
		server, project, network, ctx, cleanup := setupBatchHandlerWithCache(t, cfg)
		defer cleanup()

		// Set up mock cache - all cache misses
		mockCache := &common.MockCacheDal{}
		network.cacheDal = mockCache

		mockCache.On("Get", mock.Anything, mock.Anything).Return(nil, nil).Times(2)

		// Expect cache Set to be called for each successful response
		setCalled := make(chan struct{}, 2)
		mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
			setCalled <- struct{}{}
		}).Times(2)

		results := []evm.Multicall3Result{
			{Success: true, ReturnData: []byte{0xaa}},
			{Success: true, ReturnData: []byte{0xbb}},
		}
		resultHex := encodeAggregate3Results(results)

		withBatchStubs(t,
			func(ctx context.Context, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				jrr := mustJsonRpcResponse(t, 1, resultHex, nil)
				return common.NewNormalizedResponse().WithJsonRpcResponse(jrr), nil
			},
			nil,
			nil,
		)

		handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), validBatchRequests(t), nil)
		require.True(t, handled)
		require.Len(t, responses, 2)

		// Wait for async cache writes (with timeout)
		for i := 0; i < 2; i++ {
			select {
			case <-setCalled:
				// Good, cache set was called
			case <-time.After(2 * time.Second):
				t.Fatal("timeout waiting for cache Set to be called")
			}
		}

		mockCache.AssertExpectations(t)
	})
}
