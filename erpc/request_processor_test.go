package erpc

import (
	"context"
	"testing"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessQueryStream_UsesClientIPForNetworkAuth(t *testing.T) {
	logger := zerolog.Nop()
	authRegistry, err := auth.NewAuthRegistry(
		context.Background(),
		&logger,
		"test-project",
		&common.AuthConfig{
			Strategies: []*common.AuthStrategyConfig{
				{
					Type: common.AuthTypeNetwork,
					Network: &common.NetworkStrategyConfig{
						AllowedCIDRs: []string{"203.0.113.0/24"},
					},
				},
			},
		},
		nil,
	)
	require.NoError(t, err)

	rp := NewRequestProcessor(&ERPC{
		projectsRegistry: &ProjectsRegistry{
			preparedProjects: map[string]*PreparedProject{
				"test-project": {
					consumerAuthRegistry: authRegistry,
					networksRegistry:     &NetworksRegistry{},
				},
			},
		},
	}, &logger)

	err = rp.ProcessQueryStream(context.Background(), &RequestInput{
		ProjectId:    "test-project",
		Architecture: "",
		ChainId:      "",
		AuthPayload:  &auth.AuthPayload{Type: common.AuthTypeNetwork, Method: "eth_queryBlocks"},
		ClientIP:     "203.0.113.10",
	}, &bdsevm.QueryBlocksRequest{}, func(proto.Message) error {
		return nil
	})

	require.Error(t, err)
	assert.False(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))
	assert.True(t, common.HasErrorCode(err, common.ErrCodeInvalidRequest))
}
