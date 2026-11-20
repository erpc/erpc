package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// When every attempt failed there is no response to name an upstream, so
// HandleNetworkPostForward reads the last upstream tried: a client method it
// renamed to eth_sendRawTransaction still gets the last-line idempotency
// check (the tx is looked up on the network and found).
func TestHandleNetworkPostForward_RenamedSendUsesLastUpstream(t *testing.T) {
	newReq := func() *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"custom_sendMevProtected","params":["` + sendRawTxFixture + `"]}`))
	}
	newNetwork := func() *mockNetwork {
		n := new(mockNetwork)
		n.On("Id").Return("evm:8453").Maybe()
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()
		txObject := []byte(`{"hash":"` + sendRawTxFixtureHash + `","blockNumber":"0x123","from":"0x0","to":"0x0"}`)
		n.On("Forward", mock.Anything, mock.MatchedBy(func(r *common.NormalizedRequest) bool {
			m, _ := r.Method()
			return m == "eth_getTransactionByHash"
		})).Return(
			common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`1`), txObject, nil),
			),
			nil,
		).Maybe()
		return n
	}

	t.Run("last upstream renames to eth_sendRawTransaction", func(t *testing.T) {
		relay := common.NewFakeUpstream("relay")
		relay.Config().MethodRewrite = map[string]string{"custom_sendMevProtected": "eth_sendRawTransaction"}
		req := newReq()
		req.SetLastUpstream(relay)
		n := newNetwork()

		resp, err := HandleNetworkPostForward(context.Background(), n, req, nil, makeExhaustedError())
		require.NoError(t, err, "the tx is on the network, the broadcast succeeded")
		require.NotNil(t, resp)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), sendRawTxFixtureHash)
		n.AssertCalled(t, "Forward", mock.Anything, mock.Anything)
	})

	t.Run("no rename keeps the client method", func(t *testing.T) {
		req := newReq()
		req.SetLastUpstream(common.NewFakeUpstream("plain"))
		n := newNetwork()

		_, err := HandleNetworkPostForward(context.Background(), n, req, nil, makeExhaustedError())
		require.Error(t, err, "custom_sendMevProtected has no network hook")
		n.AssertNotCalled(t, "Forward", mock.Anything, mock.Anything)
	})
}
