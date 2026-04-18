package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

func TestBuildJSONRPCRequest(t *testing.T) {
	body := buildJSONRPCRequest("eth_chainId", []interface{}{})
	req := common.NewNormalizedRequest(body)

	require.NoError(t, req.Validate())
	method, err := req.Method()
	require.NoError(t, err)
	require.Equal(t, "eth_chainId", method)
}

func TestParseJSONRPCResult(t *testing.T) {
	resp := common.NewNormalizedResponse().
		WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0x1"`), nil))

	result, err := parseJSONRPCResult(context.Background(), resp)
	require.NoError(t, err)
	require.Equal(t, []byte(`"0x1"`), []byte(result))
}
