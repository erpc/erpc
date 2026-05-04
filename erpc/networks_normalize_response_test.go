package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNormalizeResponse_IDRewrite_AppliesToAllArchitectures pins the contract
// that the response ID always reflects the client's original request ID, for
// every JSON-RPC architecture. Prior to the fix, the rewrite was guarded by
// `switch n.Architecture() { case common.ArchitectureEvm: ... }`, so any
// non-EVM JSON-RPC architecture would leak whatever ID the upstream echoed.
func TestNormalizeResponse_IDRewrite_AppliesToAllArchitectures(t *testing.T) {
	ctx := context.Background()

	// Upstream responded with ID 99 (what an upstream might echo after its
	// own multiplexing/renumbering); client sent ID 1 and expects 1 back.
	buildResp := func(t *testing.T) *common.NormalizedResponse {
		t.Helper()
		jrr := common.MustNewJsonRpcResponseFromBytes(
			[]byte(`99`),      // id bytes the upstream returned
			[]byte(`"0xabc"`), // some result
			nil,
		)
		return common.NewNormalizedResponse().WithJsonRpcResponse(jrr)
	}

	buildReq := func() *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(`{"method":"eth_chainId","params":[],"id":1,"jsonrpc":"2.0"}`))
	}

	// Asserts the response's ID matches the request's parsed ID (both should
	// end up as float64(1) after JSON decode).
	assertResponseMatchesRequest := func(t *testing.T, req *common.NormalizedRequest, resp *common.NormalizedResponse) {
		t.Helper()
		jrr, err := resp.JsonRpcResponse(ctx)
		require.NoError(t, err)
		require.NotNil(t, jrr)
		jrq, err := req.JsonRpcRequest(ctx)
		require.NoError(t, err)
		require.NotNil(t, jrq)
		assert.Equal(t, jrq.ID, jrr.ID(),
			"response ID should be rewritten to match the client's original request ID")
	}

	t.Run("EVM", func(t *testing.T) {
		network := &Network{cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm}}
		req := buildReq()
		resp := buildResp(t)
		require.NoError(t, network.normalizeResponse(ctx, req, resp))
		assertResponseMatchesRequest(t, req, resp)
	})

	t.Run("NonEvmJsonRpcArchitecture", func(t *testing.T) {
		// Regression: pre-fix this returned the upstream's echoed ID (99)
		// because the switch only handled EVM. Post-fix: rewrite applies to
		// any JSON-RPC architecture. We use a placeholder architecture name
		// so the test exercises the non-EVM branch without depending on a
		// specific architecture being merged to main.
		network := &Network{cfg: &common.NetworkConfig{Architecture: common.NetworkArchitecture("jsonrpc-test")}}
		req := buildReq()
		resp := buildResp(t)
		require.NoError(t, network.normalizeResponse(ctx, req, resp))
		assertResponseMatchesRequest(t, req, resp)
	})

	t.Run("NilResponse_NoError", func(t *testing.T) {
		network := &Network{cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm}}
		assert.NoError(t, network.normalizeResponse(ctx, buildReq(), nil))
	})
}
