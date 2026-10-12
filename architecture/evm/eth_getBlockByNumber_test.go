package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testNetwork is a simple test implementation of common.Network interface for this file
type testNetwork struct {
	cfg           *common.NetworkConfig
	finalityState common.DataFinalityState
	highestLatest int64
}

func (t *testNetwork) Architecture() common.NetworkArchitecture {
	if t.cfg != nil {
		return t.cfg.Architecture
	}
	return common.ArchitectureEvm
}

func (t *testNetwork) Config() *common.NetworkConfig {
	return t.cfg
}

func (t *testNetwork) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}

func (t *testNetwork) Bootstrap(ctx context.Context) error {
	return nil
}

func (t *testNetwork) Id() string {
	return "test-network"
}

func (t *testNetwork) Label() string {
	return "test"
}

func (t *testNetwork) ProjectId() string {
	return "test-project"
}

func (t *testNetwork) Logger() *zerolog.Logger {
	logger := zerolog.Nop()
	return &logger
}

func (t *testNetwork) EvmHighestLatestBlockNumber(ctx context.Context) int64 {
	return t.highestLatest
}

func (t *testNetwork) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 {
	return 0
}

func (t *testNetwork) EvmLeaderUpstream(ctx context.Context) common.Upstream {
	return nil
}

func (t *testNetwork) GetMethodMetrics(method string) common.TrackedMetrics {
	return nil
}

func (t *testNetwork) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	return t.finalityState
}

func TestEnforceNonNullTaggedBlocks(t *testing.T) {
	t.Run("TaggedBlockWithEnforcementDisabled_ReturnsNull", func(t *testing.T) {
		// Create a request with a block tag ("pending") and directive disabled
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"pending", true}),
		)
		request.SetDirectives(&common.RequestDirectives{
			EnforceNonNullTaggedBlocks: false,
		})

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(context.Background(), request, response)

		// Assert: Should return null without error for tagged blocks when enforcement is disabled
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.True(t, result.IsResultEmptyish())
	})

	t.Run("NumericBlockWithEnforcementDisabled_StillReturnsError", func(t *testing.T) {
		// Create a request with a numeric block (hex number) and directive disabled
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"0x1234", true}),
		)
		request.SetDirectives(&common.RequestDirectives{
			EnforceNonNullTaggedBlocks: false,
		})

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(context.Background(), request, response)

		// Assert: Numeric blocks ALWAYS return error when null, regardless of directive
		// This is the key behavior: numeric null blocks indicate real data problems (pruned/missing)
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "block not found")
	})

	t.Run("TaggedBlockWithEnforcementEnabled_ReturnsError", func(t *testing.T) {
		// Create a request with a block tag ("pending") and directive enabled
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"pending", true}),
		)
		request.SetDirectives(&common.RequestDirectives{
			EnforceNonNullTaggedBlocks: true,
		})

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(context.Background(), request, response)

		// Assert: Should return error for tagged blocks when enforcement is enabled
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "block not found")
	})

	t.Run("LatestTagWithEnforcementDisabled_ReturnsNull", func(t *testing.T) {
		// Create a request with "latest" tag and directive disabled
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"latest", true}),
		)
		request.SetDirectives(&common.RequestDirectives{
			EnforceNonNullTaggedBlocks: false,
		})

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(context.Background(), request, response)

		// Assert: Should return null without error for "latest" tag when enforcement is disabled
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.True(t, result.IsResultEmptyish())
	})

	t.Run("TaggedBlockWithNilDirectives_DefaultsToNoEnforce", func(t *testing.T) {
		// Create a request WITHOUT setting directives (nil)
		// This tests the behavior when directives are not set
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"pending", true}),
		)
		// Note: directives are nil by default

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(context.Background(), request, response)

		// Assert: When directives are nil, should NOT enforce (allow null)
		// The defaults are applied at network level via DirectiveDefaults.SetDefaults()
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.True(t, result.IsResultEmptyish())
	})
}

// tipFetchNetwork serves any concrete block number from Forward so the tip
// re-fetch in enforceHighestBlock can succeed.
type tipFetchNetwork struct {
	testNetwork
	finalizedTip int64
}

func (n *tipFetchNetwork) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 {
	return n.finalizedTip
}

func (n *tipFetchNetwork) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	rqj, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return nil, err
	}
	jrr, err := common.NewJsonRpcResponse(rqj.ID, map[string]interface{}{"number": rqj.Params[0]}, nil)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}

// A cached response carries no upstream. When it lags the tip, enforcement
// must re-fetch the tip instead of dereferencing the missing upstream.
func TestEnforceHighestBlock_CachedResponseWithoutUpstream(t *testing.T) {
	cases := []struct {
		tag           string
		latestTip     int64
		finalizedTip  int64
		cachedNumber  string
		expectedBlock int64
	}{
		{tag: "latest", latestTip: 0x101, cachedNumber: "0x100", expectedBlock: 0x101},
		{tag: "finalized", latestTip: 0x200, finalizedTip: 0x101, cachedNumber: "0x100", expectedBlock: 0x101},
	}

	for _, tc := range cases {
		t.Run(tc.tag, func(t *testing.T) {
			network := &tipFetchNetwork{
				testNetwork:  testNetwork{highestLatest: tc.latestTip},
				finalizedTip: tc.finalizedTip,
			}
			req := common.NewNormalizedRequestFromJsonRpcRequest(
				common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{tc.tag, false}),
			)
			req.SetDirectives(&common.RequestDirectives{EnforceHighestBlock: true})
			jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{"number": tc.cachedNumber}, nil)
			assert.NoError(t, err)
			cached := common.NewNormalizedResponse().WithRequest(req).WithFromCache(true).WithJsonRpcResponse(jrr)
			assert.Nil(t, cached.Upstream())

			var out *common.NormalizedResponse
			assert.NotPanics(t, func() {
				out, err = enforceHighestBlock(context.Background(), network, req, cached, nil)
			})
			assert.NoError(t, err)
			assert.NotNil(t, out)
			_, bn, err := ExtractBlockReferenceFromResponse(context.Background(), out)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedBlock, bn)
		})
	}
}

// refetchNetwork scopes the latest tip to the request's use-upstream selector
// (as Network does), answers every tip re-fetch with null and records the
// selector each re-fetch was forwarded with.
type refetchNetwork struct {
	testNetwork
	scopedTip int64
	forwarded []string
}

func (n *refetchNetwork) EvmHighestLatestBlockNumber(ctx context.Context) int64 {
	if req, ok := ctx.Value(common.RequestContextKey).(*common.NormalizedRequest); ok && req.Directives().UseUpstream != "" {
		return n.scopedTip
	}
	return n.highestLatest
}

func (n *refetchNetwork) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	n.forwarded = append(n.forwarded, req.Directives().UseUpstream)
	jrr, err := common.NewJsonRpcResponse(1, nil, nil)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}

func latestBlockResponse(t *testing.T, useUpstream string, number string, fromCache bool) (*common.NormalizedRequest, *common.NormalizedResponse) {
	req := common.NewNormalizedRequestFromJsonRpcRequest(
		common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"latest", false}),
	)
	req.SetDirectives(&common.RequestDirectives{EnforceHighestBlock: true, UseUpstream: useUpstream})
	jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{"number": number, "hash": "0x01"}, nil)
	require.NoError(t, err)
	resp := common.NewNormalizedResponse().WithRequest(req).WithFromCache(fromCache).WithJsonRpcResponse(jrr)
	if !fromCache {
		resp.SetUpstream(common.NewFakeUpstream("rpc1"))
	}
	return req, resp
}

// When the tip re-fetch misses, the stale-but-valid block is served rather
// than an error, after a single re-fetch.
func TestEnforceHighestBlock_LatestRefetchMissFailsOpen(t *testing.T) {
	for _, fromCache := range []bool{false, true} {
		network := &refetchNetwork{testNetwork: testNetwork{highestLatest: 100}}
		req, resp := latestBlockResponse(t, "", "0x63", fromCache)

		out, err := enforceHighestBlock(context.Background(), network, req, resp, nil)
		require.NoError(t, err)
		_, bn, err := ExtractBlockReferenceFromResponse(context.Background(), out)
		require.NoError(t, err)
		assert.Equal(t, int64(99), bn)

		// A single re-fetch that keeps the request's (empty) selector: the
		// stale responder may be the only upstream that has the tip block.
		assert.Equal(t, []string{""}, network.forwarded, "fromCache=%v", fromCache)
	}
}

// A use-upstream-scoped request is enforced against its group's tip, and the
// re-fetch keeps the caller's selector.
func TestEnforceHighestBlock_KeepsUseUpstreamScope(t *testing.T) {
	for _, fromCache := range []bool{false, true} {
		network := &refetchNetwork{testNetwork: testNetwork{highestLatest: 2000}, scopedTip: 1000}

		req, resp := latestBlockResponse(t, "slow*", "0x3e8", fromCache)
		out, err := enforceHighestBlock(context.Background(), network, req, resp, nil)
		require.NoError(t, err)
		assert.Same(t, resp, out)
		assert.Empty(t, network.forwarded, "fromCache=%v: at the group tip", fromCache)

		req, resp = latestBlockResponse(t, "slow*", "0x3e7", fromCache)
		_, err = enforceHighestBlock(context.Background(), network, req, resp, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"slow*"}, network.forwarded, "fromCache=%v", fromCache)
	}
}
