package erpc

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	authDirectivesOpsToken      = "ops-token"
	authDirectivesCustomerToken = "customer-token"
	authDirectivesOpsUserId     = "ops"
)

// authStrategyDirectivesCfg builds a project that denies every client directive
// project-wide and re-grants all of them only to callers presenting the "ops"
// secret. The "customer" strategy leaves allowClientDirectives unset, so those
// callers inherit the project-level deny-all.
//
// trustUserIdHeader is enabled so the spoofing case below is realistic: a
// caller may set X-ERPC-User-Id freely, and that must never buy the ops
// strategy's capability.
func authStrategyDirectivesCfg() *common.Config {
	return &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(10 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{
			{
				Id:                    "test_project",
				AllowClientDirectives: util.StringPtr(""),
				TrustUserIdHeader:     true,
				Auth: &common.AuthConfig{
					Strategies: []*common.AuthStrategyConfig{
						{
							Type:                  common.AuthTypeSecret,
							AllowClientDirectives: util.StringPtr("*"),
							Secret: &common.SecretStrategyConfig{
								Id:    authDirectivesOpsUserId,
								Value: authDirectivesOpsToken,
							},
						},
						{
							Type: common.AuthTypeSecret,
							Secret: &common.SecretStrategyConfig{
								Id:    "customer",
								Value: authDirectivesCustomerToken,
							},
						},
					},
				},
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm:          &common.EvmNetworkConfig{ChainId: 123},
						Failsafe:     []*common.FailsafeConfig{{}},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "rpc1",
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc1.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
						Failsafe: []*common.FailsafeConfig{{}},
					},
					{
						Id:       "rpc2",
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc2.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
						Failsafe: []*common.FailsafeConfig{{}},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}
}

// TestHttpServer_AuthStrategyClientDirectives drives the whole HTTP path —
// auth, capability attachment, matcher resolution, directive parsing, upstream
// selection — and asserts the observable effect of the `use-upstream`
// directive: which upstream actually served the request.
//
// The project denies all client directives; only the ops strategy re-grants
// them. So `X-ERPC-Use-Upstream: rpc2` must pin the response to rpc2 for the
// ops caller and be silently dropped for everyone else.
func TestHttpServer_AuthStrategyClientDirectives(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	// Both result mocks are persisted so every subtest (plus the selection
	// policy's background probe of the non-pinned upstream) is served
	// deterministically instead of racing a single-use mock. They remain
	// pending by design, hence expecting 2.
	defer util.AssertNoPendingMocks(t, 2)

	gock.New("http://rpc1.localhost").
		Post("/").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(util.SafeReadBody(request), "eth_getBlockNumber")
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 111, "result": "0x1111111"})
	gock.New("http://rpc2.localhost").
		Post("/").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(util.SafeReadBody(request), "eth_getBlockNumber")
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 222, "result": "0x2222222"})

	sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(authStrategyDirectivesCfg(), t)
	defer shutdown()

	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	policy.OverrideAllForTest(prj.policyEngine)

	const body = `{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`

	// Deny cases run first so rpc2 has served no traffic when the unpinned
	// selection happens — the pinned case cannot influence them.
	t.Run("StrategyWithoutOverrideInheritsProjectDenyAll", func(t *testing.T) {
		status, headers, respBody := sendRequest(body, map[string]string{
			"X-ERPC-Secret-Token": authDirectivesCustomerToken,
			"X-ERPC-Use-Upstream": "rpc2",
		}, nil)

		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, "rpc1", headers["X-Erpc-Upstream"],
			"customer strategy leaves allowClientDirectives unset, so the project deny-all applies and the pin must be dropped")
		assert.Contains(t, respBody, "0x1111111")
	})

	// The single most important behavior: identity asserted through the
	// unvalidated trusted-user-id header must never carry a strategy
	// capability, even when the spoofed id is byte-identical to the ops
	// strategy's user id.
	t.Run("SpoofedTrustedUserIdHeaderCannotWidenDirectives", func(t *testing.T) {
		status, headers, respBody := sendRequest(body, map[string]string{
			"X-ERPC-Secret-Token": authDirectivesCustomerToken,
			"X-ERPC-User-Id":      authDirectivesOpsUserId,
			"X-ERPC-Use-Upstream": "rpc2",
		}, nil)

		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, "rpc1", headers["X-Erpc-Upstream"],
			"claiming the ops user id via X-ERPC-User-Id must not grant the ops strategy's directive capability")
		assert.Contains(t, respBody, "0x1111111")
	})

	t.Run("StrategyOverrideGrantsDirectiveDeniedProjectWide", func(t *testing.T) {
		status, headers, respBody := sendRequest(body, map[string]string{
			"X-ERPC-Secret-Token": authDirectivesOpsToken,
			"X-ERPC-Use-Upstream": "rpc2",
		}, nil)

		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, "rpc2", headers["X-Erpc-Upstream"],
			"ops strategy sets allowClientDirectives to \"*\", overriding the project deny-all, so the pin must take effect")
		assert.Contains(t, respBody, "0x2222222")
	})
}
