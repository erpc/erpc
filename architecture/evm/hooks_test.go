package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// Verifies that empty point-lookups are classified as missing-data at upstream post-forward,
// enabling network-level retries and proper final error shaping.
func TestUpstreamPostForward_PointLookupMissingData_BlockByNumber(t *testing.T) {
	// Build request: eth_getBlockByNumber("0x123", false)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x123",false]}`))

	// Build empty response
	resp := common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(&common.JsonRpcResponse{Result: []byte("null")})

	// Call upstream post-forward hook; network and upstream can be nil for this classification
	outResp, err := HandleUpstreamPostForward(
		context.Background(),
		nil, // network not used by this hook
		nil, // upstream is optional for classification
		req,
		resp,
		nil,
		false,
	)

	assert.Equal(t, resp, outResp, "response pointer should be unchanged")
	assert.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData), "expected ErrEndpointMissingData")
}

func TestUpstreamPostForward_PointLookupMissingData_BlockByHash(t *testing.T) {
	// Build request: eth_getBlockByHash("0xabc...", false)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByHash","params":["0xabcdef",false]}`))

	// Build empty response
	resp := common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(&common.JsonRpcResponse{Result: []byte("null")})

	outResp, err := HandleUpstreamPostForward(
		context.Background(),
		nil,
		nil,
		req,
		resp,
		nil,
		false,
	)

	assert.Equal(t, resp, outResp, "response pointer should be unchanged")
	assert.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData), "expected ErrEndpointMissingData")
}
