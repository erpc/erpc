package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// Two full eRPC instances share one Redis. An operator cordon issued on one
// instance's admin endpoint takes the upstream out of routing on the other
// after its sync tick, a freshly started third instance restores it before
// serving its first request, and an uncordon on any instance clears it
// everywhere. Routing is observed through real JSON-RPC responses: each mock
// upstream answers eth_getBalance with a distinct value.
func TestAdminCordon_TwoInstancesConvergeThroughSharedState(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	for host, balance := range map[string]string{"rpc3": "0x3", "rpc4": "0x4"} {
		gock.New("http://" + host + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBalance") }).
			Reply(200).
			JSON([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%q}`, balance)))
	}

	redis, err := miniredis.Run()
	require.NoError(t, err)
	defer redis.Close()

	const adminSecret = "shared-secret"
	newInstance := func(t *testing.T) (*ERPC, string, func()) {
		t.Helper()
		logger := log.Logger
		ctx, cancel := context.WithCancel(context.Background())
		cfg := &common.Config{
			Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
			Admin: &common.AdminConfig{Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
				{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Value: adminSecret}},
			}}},
			Projects: []*common.ProjectConfig{{
				Id: "test_project",
				Networks: []*common.NetworkConfig{{
					Architecture: common.ArchitectureEvm,
					Evm:          &common.EvmNetworkConfig{ChainId: 123},
					SelectionPolicy: &common.SelectionPolicyConfig{
						EvalInterval: common.Duration(50 * time.Millisecond),
					},
				}},
				Upstreams: []*common.UpstreamConfig{
					{Id: "rpc3", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc3.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
					{Id: "rpc4", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc4.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
				},
			}},
			RateLimiters: &common.RateLimiterConfig{},
		}
		require.NoError(t, cfg.SetDefaults(nil))

		ssrCfg := &common.SharedStateConfig{
			ClusterKey: "e2e-cordons",
			Connector: &common.ConnectorConfig{
				Driver: common.DriverRedis,
				Redis: &common.RedisConnectorConfig{
					Addr:         redis.Addr(),
					ConnPoolSize: 5,
					InitTimeout:  common.Duration(2 * time.Second),
					GetTimeout:   common.Duration(2 * time.Second),
					SetTimeout:   common.Duration(2 * time.Second),
				},
			},
		}
		require.NoError(t, ssrCfg.SetDefaults("e2e-cordons"))
		ssr, err := data.NewSharedStateRegistry(ctx, &logger, ssrCfg)
		require.NoError(t, err)

		instance, err := NewERPC(ctx, &logger, ssr, nil, nil, cfg)
		require.NoError(t, err)
		instance.Bootstrap(ctx)

		httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, instance)
		require.NoError(t, err)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		go func() { _ = httpServer.serverV4.Serve(listener) }()
		baseURL := fmt.Sprintf("http://%s", listener.Addr().String())

		// Wait until both upstreams are registered and the policy has ticked.
		require.Eventually(t, func() bool {
			prj, err := instance.GetProject("test_project")
			return err == nil && len(prj.upstreamsRegistry.GetAllUpstreams()) == 2
		}, 10*time.Second, 50*time.Millisecond)
		return instance, baseURL, func() {
			_ = httpServer.serverV4.Shutdown(context.Background())
			cancel()
		}
	}

	post := func(t *testing.T, url, secret, body string) map[string]any {
		t.Helper()
		req, err := http.NewRequest("POST", url, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if secret != "" {
			req.Header.Set("x-erpc-secret-token", secret)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		var out map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return out
	}
	admin := func(t *testing.T, baseURL, method, params string) map[string]any {
		t.Helper()
		out := post(t, baseURL+"/admin", adminSecret,
			fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[%s]}`, method, params))
		require.NotContains(t, out, "error", "admin %s: %v", method, out)
		return out
	}
	balance := func(t *testing.T, baseURL string) string {
		t.Helper()
		out := post(t, baseURL+"/test_project/evm/123", "",
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000001","latest"]}`)
		require.NotContains(t, out, "error", "request failed: %v", out)
		return out["result"].(string)
	}
	listed := func(t *testing.T, baseURL string) []any {
		t.Helper()
		return admin(t, baseURL, "erpc_listCordoned", `{"projectId":"test_project"}`)["result"].(map[string]any)["cordoned"].([]any)
	}
	// servedOnlyBy sends requests until the policy has converged and asserts
	// every subsequent answer comes from the one expected upstream.
	servedOnlyBy := func(t *testing.T, baseURL, want string) {
		t.Helper()
		require.Eventually(t, func() bool { return balance(t, baseURL) == want }, 5*time.Second, 50*time.Millisecond,
			"%s never converged on %s; cordoned=%v", baseURL, want, listed(t, baseURL))
		for range 20 {
			require.Equal(t, want, balance(t, baseURL))
		}
	}
	sync := func(t *testing.T, instance *ERPC) {
		t.Helper()
		prj, err := instance.GetProject("test_project")
		require.NoError(t, err)
		prj.upstreamsRegistry.SyncOperatorCordons()
	}

	a, urlA, stopA := newInstance(t)
	defer stopA()
	b, urlB, stopB := newInstance(t)
	defer stopB()

	// Cordon rpc3 on A: A stops using it immediately, B after its sync tick.
	res := admin(t, urlA, "erpc_cordonUpstream", `{"projectId":"test_project","upstream":"rpc3","reason":"vendor incident #1"}`)
	require.Equal(t, true, res["result"].(map[string]any)["cordoned"])
	servedOnlyBy(t, urlA, "0x4")
	require.Len(t, listed(t, urlA), 1)
	sync(t, b)
	servedOnlyBy(t, urlB, "0x4")
	require.Equal(t, listed(t, urlA), listed(t, urlB), "both instances list the same cordon")
	require.Equal(t, "vendor incident #1", listed(t, urlB)[0].(map[string]any)["reason"])

	// A "restarted pod" restores the cordon before it serves anything.
	c, urlC, stopC := newInstance(t)
	defer stopC()
	require.Len(t, listed(t, urlC), 1, "restored from shared state at boot, no admin call needed")
	servedOnlyBy(t, urlC, "0x4")

	// Uncordon from C; A and B pick it up on sync. Then cordon rpc4 from B
	// and every instance ends up on rpc3 only.
	admin(t, urlC, "erpc_uncordonUpstream", `{"projectId":"test_project","upstream":"rpc3"}`)
	sync(t, a)
	sync(t, b)
	for _, url := range []string{urlA, urlB, urlC} {
		require.Empty(t, listed(t, url))
	}
	admin(t, urlB, "erpc_cordonUpstream", `{"projectId":"test_project","upstream":"rpc4","reason":"vendor incident #2"}`)
	sync(t, a)
	sync(t, c)
	for _, url := range []string{urlA, urlB, urlC} {
		servedOnlyBy(t, url, "0x3")
	}

	// A method-scoped cordon is listed with its method and does not touch
	// other methods; the record for an unknown id can still be removed.
	admin(t, urlA, "erpc_cordonUpstream", `{"projectId":"test_project","upstream":"rpc3","method":"eth_getLogs","reason":"slow logs"}`)
	rows := listed(t, urlA)
	require.Len(t, rows, 2)
	servedOnlyBy(t, urlA, "0x3")
	admin(t, urlA, "erpc_uncordonUpstream", `{"projectId":"test_project","upstream":"rpc3","method":"eth_getLogs"}`)
	admin(t, urlA, "erpc_uncordonUpstream", `{"projectId":"test_project","upstream":"removed-long-ago"}`)
	require.Len(t, listed(t, urlA), 1)

	// Redis gone: the admin write fails closed and nothing changes locally.
	redis.Close()
	out := post(t, urlA+"/admin", adminSecret,
		`{"jsonrpc":"2.0","id":1,"method":"erpc_uncordonUpstream","params":[{"projectId":"test_project","upstream":"rpc4"}]}`)
	require.Contains(t, out, "error", "persistence failure must surface to the operator")
	servedOnlyBy(t, urlA, "0x3")
}
