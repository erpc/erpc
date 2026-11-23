package evm

import (
	"context"
	"errors"
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

func (t *testNetwork) EvmHead(ctx context.Context, req *common.NormalizedRequest) common.EvmHead {
	return common.EvmHead{Latest: t.highestLatest}
}
func (t *testNetwork) EvmLaneHead(string, string) common.EvmHead {
	return t.EvmHead(context.Background(), nil)
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

	// #477: "latest" is interpolated to the Head's number, and the null-for-tag
	// opt-out still reads the tag the client sent.
	t.Run("InterpolatedLatestTagWithEnforcementDisabled_ReturnsNull", func(t *testing.T) {
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"latest", true}),
		)
		request.SetNetwork(&testNetwork{highestLatest: 0x20})
		request.SetDirectives(&common.RequestDirectives{
			EnforceNonNullTaggedBlocks: false,
		})
		jrq, err := request.JsonRpcRequest()
		require.NoError(t, err)
		NormalizeHttpJsonRpc(context.Background(), request, jrq)
		require.Equal(t, "0x20", jrq.Params[0], "latest must be interpolated to the Head")

		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		result, err := enforceNonNullBlock(context.Background(), request, response)
		assert.NoError(t, err)
		require.NotNil(t, result)
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

func TestUnservedNull_PreservesNullRoundError(t *testing.T) {
	t.Parallel()
	newReq := func() *common.NormalizedRequest {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x65",false]}`))
		req.TakeHeadRefusal(&common.HeadRefusal{Block: 101, Latest: 100, Rule: common.HeadBand})
		return req
	}

	t.Run("direct null-round missing data stays an error", func(t *testing.T) {
		out, ok := unservedNull(context.Background(), newReq(), lotusNull(t, 101))
		assert.False(t, ok, "a null round must not be rewritten to synthetic JSON null")
		assert.Nil(t, out)
	})

	t.Run("exhausted bundle containing null round stays an error", func(t *testing.T) {
		exhausted := common.NewErrUpstreamsExhaustedWithCause(errors.Join(lotusNull(t, 101), common.NewErrUpstreamBlockUnavailable("rpc2", 101, 100, 0)))
		out, ok := unservedNull(context.Background(), newReq(), exhausted)
		assert.False(t, ok, "an exhausted bundle with a null round must not be rewritten to synthetic JSON null")
		assert.Nil(t, out)
	})

	t.Run("transient missing data above Latest still synthesizes null", func(t *testing.T) {
		transient := common.NewErrEndpointMissingData(errors.New("block not found"), nil)
		out, ok := unservedNull(context.Background(), newReq(), transient)
		require.True(t, ok)
		require.NotNil(t, out)
		assert.True(t, out.IsResultEmptyish())
	})
}
