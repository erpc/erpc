package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

// testNetwork is a simple test implementation of common.Network interface for this file
type testNetwork struct {
	cfg *common.NetworkConfig
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
	return 0
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
	return common.DataFinalityStateFinalized
}

func (t *testNetwork) Cache() common.CacheDAL {
	return nil
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
		result, err := enforceNonNullBlock(request, response)

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
		result, err := enforceNonNullBlock(request, response)

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
		result, err := enforceNonNullBlock(request, response)

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
		result, err := enforceNonNullBlock(request, response)

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
		result, err := enforceNonNullBlock(request, response)

		// Assert: When directives are nil, should NOT enforce (allow null)
		// The defaults are applied at network level via DirectiveDefaults.SetDefaults()
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.True(t, result.IsResultEmptyish())
	})
}
