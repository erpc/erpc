package erpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// directiveMatcherProjects registers, through the real ProjectsRegistry, two
// projects whose auth strategies carry `allowClientDirectives` patterns, so
// PreparedProject.strategyDirectiveMatchers is populated by production code
// rather than by the test.
//
//   - "deny_project": project denies everything; strategies grant "*" and "retry-*".
//   - "allow_project": project allows everything; strategies grant "" and "retry-*".
func directiveMatcherProjects(t *testing.T) *ProjectsRegistry {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "1MB"},
		},
	})
	require.NoError(t, err)

	project := func(id string, projectPattern string, strategyPatterns ...string) *common.ProjectConfig {
		strategies := make([]*common.AuthStrategyConfig, 0, len(strategyPatterns))
		for i, pattern := range strategyPatterns {
			strategies = append(strategies, &common.AuthStrategyConfig{
				Type:                  common.AuthTypeSecret,
				AllowClientDirectives: util.StringPtr(pattern),
				Secret: &common.SecretStrategyConfig{
					Id:    "ops",
					Value: fmt.Sprintf("token-%d", i),
				},
			})
		}
		return &common.ProjectConfig{
			Id:                    id,
			AllowClientDirectives: util.StringPtr(projectPattern),
			Auth:                  &common.AuthConfig{Strategies: strategies},
			Networks: []*common.NetworkConfig{
				{
					Architecture: common.ArchitectureEvm,
					Evm:          &common.EvmNetworkConfig{ChainId: 123},
				},
			},
			Upstreams: []*common.UpstreamConfig{
				{
					Id:       "rpc1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1.localhost",
					Evm:      &common.EvmUpstreamConfig{ChainId: 123},
				},
			},
		}
	}

	reg, err := NewProjectsRegistry(
		ctx,
		&log.Logger,
		[]*common.ProjectConfig{
			project("deny_project", "", "*", "retry-*"),
			project("allow_project", "*", "", "retry-*"),
		},
		ssr,
		nil, // evmJsonRpcCache
		nil, // svmJsonRpcCache — added on this branch
		nil, // rateLimitersRegistry
		thirdparty.NewVendorsRegistry(),
		nil,
		nil,
	)
	require.NoError(t, err)
	return reg
}

// TestPreparedProject_ClientDirectiveMatcherFor covers the resolution matrix
// between the project-level `allowClientDirectives` pattern and the per-auth-
// strategy override carried on the authenticated user.
func TestPreparedProject_ClientDirectiveMatcherFor(t *testing.T) {
	reg := directiveMatcherProjects(t)

	tests := []struct {
		name      string
		projectId string
		user      *common.User
		directive string
		want      bool
	}{
		{
			name:      "strategy grants all over a project that denies all",
			projectId: "deny_project",
			user:      &common.User{Id: "ops", AllowClientDirectives: util.StringPtr("*")},
			directive: "use-upstream",
			want:      true,
		},
		{
			name:      "strategy denies all over a project that allows all",
			projectId: "allow_project",
			user:      &common.User{Id: "customer", AllowClientDirectives: util.StringPtr("")},
			directive: "use-upstream",
			want:      false,
		},
		{
			name:      "strategy narrows a project that allows all",
			projectId: "allow_project",
			user:      &common.User{Id: "partner", AllowClientDirectives: util.StringPtr("retry-*")},
			directive: "retry-empty",
			want:      true,
		},
		{
			name:      "narrowing strategy denies directives outside its pattern",
			projectId: "allow_project",
			user:      &common.User{Id: "partner", AllowClientDirectives: util.StringPtr("retry-*")},
			directive: "use-upstream",
			want:      false,
		},
		{
			name:      "strategy partially widens a project that denies all",
			projectId: "deny_project",
			user:      &common.User{Id: "partner", AllowClientDirectives: util.StringPtr("retry-*")},
			directive: "retry-empty",
			want:      true,
		},
		{
			name:      "strategy widens a project that denies all, but only within its pattern",
			projectId: "deny_project",
			user:      &common.User{Id: "partner", AllowClientDirectives: util.StringPtr("retry-*")},
			directive: "use-upstream",
			want:      false,
		},
		{
			name:      "user without an override inherits the project deny-all",
			projectId: "deny_project",
			user:      &common.User{Id: "customer"},
			directive: "use-upstream",
			want:      false,
		},
		{
			name:      "user without an override inherits the project allow-all",
			projectId: "allow_project",
			user:      &common.User{Id: "customer"},
			directive: "use-upstream",
			want:      true,
		},
		{
			name:      "nil user falls back to the project deny-all",
			projectId: "deny_project",
			user:      nil,
			directive: "use-upstream",
			want:      false,
		},
		{
			name:      "nil user falls back to the project allow-all",
			projectId: "allow_project",
			user:      nil,
			directive: "use-upstream",
			want:      true,
		},
		{
			name:      "pattern that was never configured on a strategy falls back to the project",
			projectId: "deny_project",
			user:      &common.User{Id: "forged", AllowClientDirectives: util.StringPtr("use-upstream")},
			directive: "use-upstream",
			want:      false,
		},
		{
			name:      "unconfigured pattern cannot narrow a permissive project either",
			projectId: "allow_project",
			user:      &common.User{Id: "forged", AllowClientDirectives: util.StringPtr("!use-upstream")},
			directive: "use-upstream",
			want:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			project, err := reg.GetProject(tc.projectId)
			require.NoError(t, err)

			matcher := project.clientDirectiveMatcherFor(tc.user)
			require.NotNil(t, matcher)
			assert.Equal(t, tc.want, matcher(tc.directive))
		})
	}
}

// TestPreparedProject_TrustedHeaderUserNeverGainsStrategyClientDirectives is
// the security invariant: X-ERPC-User-Id is an unvalidated header, so identity
// taken from it must never carry an auth strategy's directive capability — not
// even when the spoofed id is byte-identical to the id of a strategy that
// grants every directive in the very same project.
func TestPreparedProject_TrustedHeaderUserNeverGainsStrategyClientDirectives(t *testing.T) {
	reg := directiveMatcherProjects(t)
	project, err := reg.GetProject("deny_project")
	require.NoError(t, err)

	// Contrast case: the same project genuinely grants "use-upstream" to a
	// caller an auth strategy authenticated, so the deny below is a real
	// decision rather than a project with nothing to give.
	authenticated := project.clientDirectiveMatcherFor(&common.User{
		Id:                    "ops",
		AllowClientDirectives: util.StringPtr("*"),
	})
	require.NotNil(t, authenticated)
	require.True(t, authenticated("use-upstream"),
		"precondition: the ops strategy's capability grants use-upstream in this project")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
	req.SetUserFromTrustedHeader("ops")
	spoofed := req.User()
	require.NotNil(t, spoofed)
	require.Equal(t, "ops", spoofed.Id, "precondition: the spoofed id matches the ops strategy's user id")

	matcher := project.clientDirectiveMatcherFor(spoofed)
	require.NotNil(t, matcher)
	assert.False(t, matcher("use-upstream"),
		"a user resolved from the unvalidated X-ERPC-User-Id header must fall through to the project pattern")
	assert.False(t, matcher("skip-cache-read"),
		"a user resolved from the unvalidated X-ERPC-User-Id header must fall through to the project pattern")
}
