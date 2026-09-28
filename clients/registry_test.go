package clients

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestClientRegistryRetriesFailedCreation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	registry := NewClientRegistry(&logger, "test-project", nil, nil)

	up := common.NewFakeUpstream("test-upstream")
	up.Config().Endpoint = "http://rpc1.localhost"

	_, err := registry.GetOrCreateClient(ctx, up)
	require.Error(t, err, "creation should fail while the upstream type is unset")

	up.Config().Type = common.UpstreamTypeEvm
	client, err := registry.GetOrCreateClient(ctx, up)
	require.NoError(t, err, "a failed creation must not be memoised")
	require.NotNil(t, client)
}
