package clients

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

func TestGrpcBdsClientRejectsQueryMethodsInSendRequest(t *testing.T) {
	client := &GenericGrpcBdsClient{}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks","params":[{}]}`))

	_, err := client.SendRequest(context.Background(), req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnsupported))
}
