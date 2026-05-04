package auth

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

func TestNewPayloadFromGrpcBearer(t *testing.T) {
	md := metadata.New(map[string]string{
		"authorization": "Bearer test-jwt",
	})

	ap, err := NewPayloadFromGrpc("eth_queryBlocks", md)
	require.NoError(t, err)
	require.Equal(t, common.AuthTypeJwt, ap.Type)
	require.NotNil(t, ap.Jwt)
	require.Equal(t, "test-jwt", ap.Jwt.Token)
	require.Equal(t, "eth_queryBlocks", ap.Method)
}

func TestNewPayloadFromGrpcBasic(t *testing.T) {
	md := metadata.New(map[string]string{
		"authorization": "Basic dXNlcjpzZWNyZXQ=",
	})

	ap, err := NewPayloadFromGrpc("eth_getBlockByNumber", md)
	require.NoError(t, err)
	require.Equal(t, common.AuthTypeSecret, ap.Type)
	require.NotNil(t, ap.Secret)
	require.Equal(t, "secret", ap.Secret.Value)
}
