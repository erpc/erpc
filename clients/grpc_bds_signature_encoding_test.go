package clients

import (
	"context"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// TestSendRequest_SignatureEncodingFollowsChain pins that the client renders
// transaction r/s per its armed chain: QUANTITY (minimal hex) everywhere,
// 32-byte DATA on Tron. go-ethereum rejects the padded form on non-Tron
// chains, so a regression here breaks every eth_getTransactionByHash consumer.
func TestSendRequest_SignatureEncodingFollowsChain(t *testing.T) {
	const tronMainnet uint64 = 728126428
	cases := []struct {
		name    string
		chainId uint64
		r, s    string
	}{
		{"quantity on generic chain", 999, "0x1", "0x2"},
		{"32-byte data on tron",
			tronMainnet,
			"0x0000000000000000000000000000000000000000000000000000000000000001",
			"0x0000000000000000000000000000000000000000000000000000000000000002"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, _, stop := startHappyServer(t, tc.chainId, 0)
			defer stop()

			client := newTestClient(t, addr)
			client.SetExpectedChainId(tc.chainId)
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionByHash","params":["0x000000000000000000000000000000000000000000000000000000000000abcd"]}`))

			resp, err := client.SendRequest(context.Background(), req)
			require.NoError(t, err)
			jrr, err := resp.JsonRpcResponse()
			require.NoError(t, err)

			var tx map[string]interface{}
			require.NoError(t, sonic.Unmarshal([]byte(jrr.GetResultString()), &tx))
			require.Equal(t, tc.r, tx["r"])
			require.Equal(t, tc.s, tx["s"])
		})
	}
}
