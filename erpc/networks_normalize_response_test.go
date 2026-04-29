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

	// Regression: clients that explicitly send "id":null (a valid JSON-RPC 2.0
	// request) used to receive a leaked internal random ID instead. erpc
	// internally assigns a random ID for multiplexing correlation; the
	// response-side rewrite must restore null. Caught by the dogfood run
	// against public Solana devnet.
	t.Run("ExplicitNullIDIsPreservedInResponse", func(t *testing.T) {
		network := &Network{cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm}}
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":null,"method":"eth_chainId","params":[]}`))
		// Upstream saw whatever internal ID erpc assigned and echoed it back.
		jrr := common.MustNewJsonRpcResponseFromBytes(
			[]byte(`123456789`),
			[]byte(`"0x1"`),
			nil,
		)
		resp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)
		require.NoError(t, network.normalizeResponse(ctx, req, resp))

		out, err := resp.JsonRpcResponse(ctx)
		require.NoError(t, err)
		require.NotNil(t, out)
		assert.Nil(t, out.ID(),
			"client sent id:null and the response id must be null (got %v)", out.ID())

		// Sanity: the request's parsed ID should still be the random internal
		// ID (used for upstream multiplexing). The response-rewrite handles
		// the special null case via the ResponseIDIsNull flag.
		jrq, _ := req.JsonRpcRequest(ctx)
		assert.NotNil(t, jrq.ID,
			"internal ID must still be assigned for upstream multiplexing correlation")
		assert.True(t, jrq.ResponseIDIsNull(),
			"ResponseIDIsNull must be set when client sent id:null")
	})

	// Sanity: a request with NO id field (a notification) should NOT trigger
	// the null-preservation path. erpc historically treats it as a regular
	// request with a random ID; we keep that behaviour.
	t.Run("MissingIDFieldIsNotTreatedAsExplicitNull", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[]}`))
		jrq, _ := req.JsonRpcRequest(ctx)
		assert.False(t, jrq.ResponseIDIsNull(),
			"missing id field must NOT be treated as explicit null")
		assert.NotNil(t, jrq.ID,
			"missing id field must still get an internal random ID")
	})
}
