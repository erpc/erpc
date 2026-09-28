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

// The WebSocket dialer does not use a proxy pool, so a WebSocket upstream
// that names one must fail instead of silently dialling direct.
func TestClientRegistryRejectsProxyPoolForWebSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	proxies, err := NewProxyPoolRegistry([]*common.ProxyPoolConfig{
		{ID: "pool1", Urls: []string{"http://myproxy1:8080"}},
	}, &logger)
	require.NoError(t, err)
	registry := NewClientRegistry(&logger, "test-project", proxies, nil)

	up := common.NewFakeUpstream("ws-upstream")
	up.Config().Type = common.UpstreamTypeEvm
	up.Config().Endpoint = "wss://rpc1.localhost"
	up.Config().JsonRpc = &common.JsonRpcUpstreamConfig{ProxyPool: "pool1"}

	_, err = registry.GetOrCreateClient(ctx, up)
	require.ErrorContains(t, err, "proxyPool is not supported for WebSocket")
}
