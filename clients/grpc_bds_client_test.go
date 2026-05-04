package clients

import (
	"context"
	"net/url"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// TestGrpcBdsClientQueryMethodsDoNotShortCircuit verifies that query methods
// are routed to the streaming QueryService handlers rather than being
// rejected outright by SendRequest. With no live queryClient wired in, the
// handler surfaces a clear error — but critically NOT ErrEndpointUnsupported
// which would disqualify the upstream from carrying eth_query* traffic.
func TestGrpcBdsClientQueryMethodsDoNotShortCircuit(t *testing.T) {
	parsedURL, err := url.Parse("grpc://localhost:0")
	require.NoError(t, err)

	client := &GenericGrpcBdsClient{Url: parsedURL}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks","params":[{"fromBlock":"0x1","toBlock":"0x2","limit":1}]}`))

	_, err = client.SendRequest(context.Background(), req)
	require.Error(t, err)
	require.False(
		t,
		common.HasErrorCode(err, common.ErrCodeEndpointUnsupported),
		"query methods must not be short-circuited as unsupported at SendRequest level; error was: %v",
		err,
	)
}
