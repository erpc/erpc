package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestAdminPurgeCache_DryRunAndSuffixIsolation(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	mr := miniredis.RunT(t)
	const adminSecret = "purge-secret"

	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Admin: &common.AdminConfig{Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
			{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Value: adminSecret}},
		}}},
		Database: &common.DatabaseConfig{
			EvmJsonRpcCache: &common.CacheConfig{
				Connectors: []*common.ConnectorConfig{{
					Id: "redis-cache", Driver: common.DriverRedis,
					Redis: &common.RedisConnectorConfig{
						Addr: mr.Addr(), ConnPoolSize: 5,
						InitTimeout: common.Duration(2 * time.Second),
						GetTimeout:  common.Duration(2 * time.Second),
						SetTimeout:  common.Duration(2 * time.Second),
					},
				}},
				Policies: []*common.CachePolicyConfig{{
					Network: "*", Method: "*", Finality: common.DataFinalityStateFinalized,
					Connector: "redis-cache", TTL: common.FixedDuration(5 * time.Minute),
				}},
			},
		},
		Projects: []*common.ProjectConfig{{
			Id: "test_project",
			Networks: []*common.NetworkConfig{
				{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}},
			},
			Upstreams: []*common.UpstreamConfig{
				{Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
			},
		}},
		RateLimiters: &common.RateLimiterConfig{},
	}

	_, _, baseURL, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	// Seed Redis directly so purge does not depend on request caching paths.
	logger := log.Logger
	seedCtx := context.Background()
	rcfg := cfg.Database.EvmJsonRpcCache.Connectors[0].Redis
	conn, err := data.NewRedisConnector(seedCtx, &logger, "seed", rcfg)
	require.NoError(t, err)

	rk := "eth_blockNumber:abcdef0123456789"
	require.Eventually(t, func() bool {
		return conn.Set(data.WithReverseIndex(seedCtx, "evm:123", ""), "evm:123:100", rk, []byte("main"), nil) == nil
	}, 5*time.Second, 50*time.Millisecond)
	require.NoError(t, conn.Set(data.WithReverseIndex(seedCtx, "evm:123", "systx"), "evm:123:systx:100", rk, []byte("systx"), nil))

	post := func(method, params string) map[string]any {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, baseURL+"/admin", strings.NewReader(
			fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[%s]}`, method, params)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-erpc-secret-token", adminSecret)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		var out map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return out
	}

	t.Run("star without WIPE rejected", func(t *testing.T) {
		out := post("erpc_purgeCache", `{"projectId":"test_project","networkId":"*"}`)
		require.Contains(t, out, "error")
		msg, _ := json.Marshal(out["error"])
		require.Contains(t, string(msg), "WIPE")
		require.True(t, mr.Exists("evm:123:100:"+rk), "rejected wipe must not delete")
	})

	t.Run("dryRun counts and deletes nothing", func(t *testing.T) {
		out := post("erpc_purgeCache", `{"projectId":"test_project","networkId":"evm:123","dryRun":true}`)
		require.NotContains(t, out, "error", "%v", out)
		res := out["result"].(map[string]any)
		require.Equal(t, true, res["dryRun"])
		require.Equal(t, float64(0), res["totalDeleted"])
		conns := res["connectors"].([]any)
		require.NotEmpty(t, conns)
		matched := conns[0].(map[string]any)["matched"].(float64)
		require.GreaterOrEqual(t, matched, float64(2))
		require.True(t, mr.Exists("evm:123:100:"+rk))
		require.True(t, mr.Exists("evm:123:systx:100:"+rk))
	})

	t.Run("unsuffixed purge keeps systx sibling", func(t *testing.T) {
		out := post("erpc_purgeCache", `{"projectId":"test_project","networkId":"evm:123"}`)
		require.NotContains(t, out, "error", "%v", out)
		res := out["result"].(map[string]any)
		require.GreaterOrEqual(t, res["totalDeleted"].(float64), float64(2))
		require.False(t, mr.Exists("evm:123:100:"+rk))
		require.True(t, mr.Exists("evm:123:systx:100:"+rk), "suffixed sibling must survive unsuffixed purge")
		scope := res["scope"].(map[string]any)
		require.Equal(t, "evm:123", scope["networkId"])
	})
}

func TestAdminPurgeCache_Validation(t *testing.T) {
	f := newAdminApiFixture(t, adminSecretConfig())
	cases := []struct {
		name, params, msg string
	}{
		{"missing params", `[]`, "params is required"},
		{"missing networkId", `[{"projectId":"test_project"}]`, "projectId and networkId are required"},
		{"star without confirm", `[{"projectId":"test_project","networkId":"*"}]`, "WIPE"},
		{"method glob rejected", `[{"projectId":"test_project","networkId":"evm:123","method":"*"}]`, "glob metacharacters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out := f.call(t, adminApiSecret, "erpc_purgeCache", tc.params)
			require.Equal(t, http.StatusBadRequest, status)
			msg, _ := json.Marshal(out["error"])
			require.Contains(t, string(msg), tc.msg)
		})
	}
}
