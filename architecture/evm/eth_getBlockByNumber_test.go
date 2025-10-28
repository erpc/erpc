package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
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

func TestEnforceNonNullTaggedBlocks(t *testing.T) {
	t.Run("TaggedBlockWithEnforcementDisabled_ReturnsNull", func(t *testing.T) {
		// Create a network with enforceNonNullTaggedBlocks disabled
		network := &testNetwork{
			cfg: &common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 1,
					Integrity: &common.EvmIntegrityConfig{
						EnforceNonNullTaggedBlocks: util.BoolPtr(false),
					},
				},
			},
		}

		// Create a request with a block tag ("pending")
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"pending", true}),
		)

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(network, response)

		// Assert: Should return null without error for tagged blocks when enforcement is disabled
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.True(t, result.IsResultEmptyish())
	})

	t.Run("NumericBlockWithEnforcementDisabled_StillReturnsError", func(t *testing.T) {
		// Create a network with enforceNonNullTaggedBlocks disabled
		network := &testNetwork{
			cfg: &common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 1,
					Integrity: &common.EvmIntegrityConfig{
						EnforceNonNullTaggedBlocks: util.BoolPtr(false),
					},
				},
			},
		}

		// Create a request with a numeric block (hex number)
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"0x1234", true}),
		)

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(network, response)

		// Assert: Numeric blocks ALWAYS return error when null, regardless of config
		// This is the key behavior: numeric null blocks indicate real data problems (pruned/missing)
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "block not found")
	})

	t.Run("TaggedBlockWithEnforcementEnabled_ReturnsError", func(t *testing.T) {
		// Create a network with enforceNonNullTaggedBlocks enabled (default)
		network := &testNetwork{
			cfg: &common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 1,
					Integrity: &common.EvmIntegrityConfig{
						EnforceNonNullTaggedBlocks: util.BoolPtr(true),
					},
				},
			},
		}

		// Create a request with a block tag ("pending")
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"pending", true}),
		)

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(network, response)

		// Assert: Should return error for tagged blocks when enforcement is enabled
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "block not found")
	})

	t.Run("LatestTagWithEnforcementDisabled_ReturnsNull", func(t *testing.T) {
		// Create a network with enforceNonNullTaggedBlocks disabled
		network := &testNetwork{
			cfg: &common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId: 1,
					Integrity: &common.EvmIntegrityConfig{
						EnforceNonNullTaggedBlocks: util.BoolPtr(false),
					},
				},
			},
		}

		// Create a request with "latest" tag
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"latest", true}),
		)

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(network, response)

		// Assert: Should return null without error for "latest" tag when enforcement is disabled
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.True(t, result.IsResultEmptyish())
	})

	t.Run("TaggedBlockWithFieldNotSet_DefaultsToEnforce", func(t *testing.T) {
		// Create a network WITHOUT setting enforceNonNullTaggedBlocks (nil)
		// This tests the CRITICAL default behavior
		network := &testNetwork{
			cfg: &common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId:   1,
					Integrity: &common.EvmIntegrityConfig{
						// EnforceNonNullTaggedBlocks is nil (not set)
					},
				},
			},
		}

		// Create a request with a tagged block
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"pending", true}),
		)

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(network, response)

		// Assert: Should enforce by default when field is nil (not explicitly disabled)
		assert.Error(t, err, "When field is nil, should default to enforce (return error)")
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "block not found")
	})
}
