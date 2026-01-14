package erpc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
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
				return nil, common.NewErrEndpointExecutionException(errors.New("boom"))
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
			projCalls := 0
			netCalls := 0

			withBatchStubs(t,
				func(ctx context.Context, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
					netCalls++
					return tt.networkResponse()
				},
				func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
					projCalls++
					return fallbackResponse(t, req), nil
				},
				nil,
			)

			handled, responses := runHandle(t, ctx, server, project, defaultBatchInfo(), tt.requests, nil)
			require.True(t, handled)
			assert.Equal(t, tt.expectedProjCalls, projCalls)
			assert.Equal(t, tt.expectedNetCalls, netCalls)
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
