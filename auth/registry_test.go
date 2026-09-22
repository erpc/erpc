package auth

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthRegistry_Authenticate_AttachesAllowClientDirectives pins the
// capability-attachment contract: the caller is stamped with the
// `allowClientDirectives` pattern of the strategy that actually authenticated
// them, and only that one.
//
// The nil / non-nil-empty distinction is load-bearing downstream: nil means
// "inherit the project pattern" while a pointer to "" means "this strategy
// denies every directive", so a strategy configured with "" must NOT come back
// as nil.
func TestAuthRegistry_Authenticate_AttachesAllowClientDirectives(t *testing.T) {
	registry := newSecretAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{
			{
				Type:                  common.AuthTypeSecret,
				AllowClientDirectives: util.StringPtr("*"),
				Secret:                &common.SecretStrategyConfig{Id: "ops", Value: "ops-token"},
			},
			{
				Type:                  common.AuthTypeSecret,
				AllowClientDirectives: util.StringPtr(""),
				Secret:                &common.SecretStrategyConfig{Id: "partner", Value: "partner-token"},
			},
			{
				Type:   common.AuthTypeSecret,
				Secret: &common.SecretStrategyConfig{Id: "customer", Value: "customer-token"},
			},
			{
				Type:                  common.AuthTypeSecret,
				AllowClientDirectives: util.StringPtr("retry-*"),
				Secret:                &common.SecretStrategyConfig{Id: "audit", Value: "audit-token"},
			},
		},
	})

	tests := []struct {
		name           string
		token          string
		wantUserId     string
		wantDirectives *string
	}{
		{
			name:           "granting strategy stamps its pattern",
			token:          "ops-token",
			wantUserId:     "ops",
			wantDirectives: util.StringPtr("*"),
		},
		{
			name:           "empty pattern is attached as a set-but-empty override, not nil",
			token:          "partner-token",
			wantUserId:     "partner",
			wantDirectives: util.StringPtr(""),
		},
		{
			name:           "strategy without the field leaves the capability unset",
			token:          "customer-token",
			wantUserId:     "customer",
			wantDirectives: nil,
		},
		{
			name:           "the matching strategy wins, not the first configured one",
			token:          "audit-token",
			wantUserId:     "audit",
			wantDirectives: util.StringPtr("retry-*"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))

			user, err := registry.Authenticate(context.Background(), req, "eth_chainId", &AuthPayload{
				Method: "eth_chainId",
				Type:   common.AuthTypeSecret,
				Secret: &SecretPayload{Value: tc.token},
			})
			require.NoError(t, err)
			require.NotNil(t, user)
			require.Equal(t, tc.wantUserId, user.Id)

			if tc.wantDirectives == nil {
				assert.Nil(t, user.AllowClientDirectives)
			} else {
				require.NotNil(t, user.AllowClientDirectives)
				assert.Equal(t, *tc.wantDirectives, *user.AllowClientDirectives)
			}

			// The HTTP path reads the capability off the request, not off the
			// return value, so the user stored on the request must carry it too.
			require.NotNil(t, req.User())
			assert.Equal(t, user, req.User())
		})
	}
}

// TestAuthRegistry_Authenticate_FailedStrategyDoesNotLeakCapability guards the
// ordering inside the strategy loop: a strategy whose Authenticate rejects the
// caller must not stamp its capability onto whoever succeeds afterwards.
func TestAuthRegistry_Authenticate_FailedStrategyDoesNotLeakCapability(t *testing.T) {
	registry := newSecretAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{
			{
				Type:                  common.AuthTypeSecret,
				AllowClientDirectives: util.StringPtr("*"),
				Secret:                &common.SecretStrategyConfig{Id: "ops", Value: "ops-token"},
			},
			{
				Type:   common.AuthTypeSecret,
				Secret: &common.SecretStrategyConfig{Id: "customer", Value: "customer-token"},
			},
		},
	})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
	user, err := registry.Authenticate(context.Background(), req, "eth_chainId", &AuthPayload{
		Method: "eth_chainId",
		Type:   common.AuthTypeSecret,
		Secret: &SecretPayload{Value: "customer-token"},
	})

	require.NoError(t, err)
	require.NotNil(t, user)
	require.Equal(t, "customer", user.Id)
	assert.Nil(t, user.AllowClientDirectives,
		"the rejected ops strategy must not grant its directive capability to the customer it failed to authenticate")
}

func newSecretAuthRegistry(t *testing.T, cfg *common.AuthConfig) *AuthRegistry {
	t.Helper()
	logger := zerolog.Nop()
	registry, err := NewAuthRegistry(context.Background(), &logger, "test_project", cfg, nil)
	require.NoError(t, err)
	return registry
}
