package clients

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWsClientMethodRewrite: a WebSocket upstream receives the methodRewrite
// target, and the subscription-ownership guard judges that target, so a
// method renamed to eth_subscribe cannot open a subscription nobody owns.
func TestWsClientMethodRewrite(t *testing.T) {
	requests := make(chan wsTestRequest, 16)
	client := newTestWsClient(t, newSubscribeServer(t, 0, requests))
	client.upstream.Config().MethodRewrite = map[string]string{
		"custom_sendMevProtected": "eth_sendRawTransaction",
		"custom_sub":              "eth_subscribe",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"custom_sendMevProtected","params":["0x01"]}`))
	_, err := client.SendRequest(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, "eth_sendRawTransaction", (<-requests).Method, "upstream must receive the rewritten method")
	method, _ := req.Method()
	assert.Equal(t, "custom_sendMevProtected", method, "the request keeps the client's method")

	err = sendSubscriptionRPC(ctx, client, nil, "custom_sub", `["newHeads"]`)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamRequestSkipped), "renamed eth_subscribe without an owner must be skipped, got %v", err)

	require.NoError(t, sendSubscriptionRPC(ctx, client, nil, "eth_chainId", `[]`))
	assert.Equal(t, "eth_chainId", (<-requests).Method, "the renamed subscribe must not have been sent")
}
