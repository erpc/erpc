package erpc

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/headcache"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

var (
	headCacheTestRedisOnce sync.Once
	headCacheTestRedisAddr string
)

// headCacheTestRedis returns a process-wide Redis address for head cache
// tests (HEADCACHE_TEST_REDIS_ADDR or an in-process miniredis). Scopes are
// isolated by the per-test upstream fingerprint and namespace.
func headCacheTestRedis() string {
	headCacheTestRedisOnce.Do(func() {
		headCacheTestRedisAddr = os.Getenv("HEADCACHE_TEST_REDIS_ADDR")
		if headCacheTestRedisAddr == "" {
			mr, err := miniredis.Run()
			if err != nil {
				panic(err)
			}
			headCacheTestRedisAddr = mr.Addr()
		}
	})
	return headCacheTestRedisAddr
}

// headCacheTestConfig wires an enabled head cache to a redis connector under
// database.evmJsonRpcCache (the only supported store).
func headCacheTestConfig(upstreamURL string, hc *common.EvmHeadCacheConfig) *common.Config {
	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true), WebSocket: &common.WebSocketServerConfig{Enabled: true}},
		Projects: []*common.ProjectConfig{{
			Id: "test_project",
			Networks: []*common.NetworkConfig{{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123, HeadCache: hc},
			}},
			Upstreams: []*common.UpstreamConfig{{
				Id:       "scripted",
				Type:     common.UpstreamTypeEvm,
				Endpoint: upstreamURL,
				Evm:      &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(100 * time.Millisecond)},
			}},
		}},
		RateLimiters: &common.RateLimiterConfig{},
	}
	if hc != nil && hc.Enabled {
		if hc.ConnectorId == "" {
			hc.ConnectorId = "headcache-redis"
		}
		if hc.Namespace == "" {
			hc.Namespace = fmt.Sprintf("t-%d", time.Now().UnixNano())
		}
		cfg.Database = &common.DatabaseConfig{EvmJsonRpcCache: &common.CacheConfig{
			Connectors: []*common.ConnectorConfig{{
				Id: hc.ConnectorId, Driver: common.DriverRedis,
				Redis: &common.RedisConnectorConfig{URI: "redis://" + headCacheTestRedis()},
			}},
		}}
	}
	return cfg
}

func doRpc(t *testing.T, send func(string, map[string]string, map[string]string) (int, map[string]string, string), method string, params string) rpcResp {
	t.Helper()
	code, _, body := send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params), nil, nil)
	require.Equal(t, 200, code, body)
	var r rpcResp
	require.NoError(t, json.Unmarshal([]byte(body), &r), body)
	return r
}

func TestHttp_HeadCache_ServesReusesAndHandlesReorg(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := headCacheTestConfig(up.URL(), &common.EvmHeadCacheConfig{
		Enabled: true, Depth: 16, PollInterval: common.Duration(100 * time.Millisecond),
	})
	send, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()

	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	hc := nw.HeadCache()
	require.NotNil(t, hc)
	require.Eventually(t, func() bool { return hc.Head() == 20 }, 10*time.Second, 50*time.Millisecond)

	// One hydrated block answers full, hash-only and by-hash queries without upstream calls.
	before := up.BlockCalls(19) + up.Calls("eth_getBlockByHash") + up.RangeLogCalls()
	full := doRpc(t, send, "eth_getBlockByNumber", `["0x13",true]`)
	require.Contains(t, string(full.Result), `"from"`)
	lite := doRpc(t, send, "eth_getBlockByNumber", `["0x13",false]`)
	require.Contains(t, string(lite.Result), scriptedTx(19, "a"))
	require.NotContains(t, string(lite.Result), `"from"`)
	byHash := doRpc(t, send, "eth_getBlockByHash", fmt.Sprintf(`[%q,false]`, up.HashAt(19)))
	require.JSONEq(t, string(lite.Result), string(byHash.Result))

	// Different filters over the same range reuse hydrated logs.
	even := doRpc(t, send, "eth_getLogs", fmt.Sprintf(`[{"fromBlock":"0x8","toBlock":"0x13","topics":[%q]}]`, scriptedTopicEven))
	var evenLogs []map[string]interface{}
	require.NoError(t, json.Unmarshal(even.Result, &evenLogs))
	require.Len(t, evenLogs, 6)
	all := doRpc(t, send, "eth_getLogs", fmt.Sprintf(`[{"fromBlock":"0x8","toBlock":"0x13","address":%q}]`, scriptedEmitter))
	var allLogs []map[string]interface{}
	require.NoError(t, json.Unmarshal(all.Result, &allLogs))
	require.Len(t, allLogs, 12)
	none := doRpc(t, send, "eth_getLogs", `[{"fromBlock":"0x8","toBlock":"0x13","address":"0x0000000000000000000000000000000000000001"}]`)
	require.JSONEq(t, `[]`, string(none.Result))
	after := up.BlockCalls(19) + up.Calls("eth_getBlockByHash") + up.RangeLogCalls()
	require.Equal(t, before, after, "client reads must be served from the head cache")

	// Range reaching below the window falls through to upstream (never partial).
	logsBefore := up.RangeLogCalls()
	wide := doRpc(t, send, "eth_getLogs", `[{"fromBlock":"0x1","toBlock":"0x13"}]`)
	var wideLogs []map[string]interface{}
	require.NoError(t, json.Unmarshal(wide.Result, &wideLogs))
	require.Len(t, wideLogs, 19)
	require.Greater(t, up.RangeLogCalls(), logsBefore)

	// Directed request bypasses the cache.
	dirBefore := up.BlockCalls(19)
	code, _, _ := send(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x13",false]}`, map[string]string{"X-ERPC-Use-Upstream": "scripted"}, nil)
	require.Equal(t, 200, code)
	require.Greater(t, up.BlockCalls(19), dirBefore)

	// Same-height and multi-block reorg: numeric mapping follows the new chain,
	// orphan hashes stop being served from the cache.
	orphan := up.HashAt(19)
	up.Reorg(18, "b")
	up.Mine(1)
	require.Eventually(t, func() bool {
		r := doRpc(t, send, "eth_getBlockByNumber", `["0x13",false]`)
		return strings.Contains(string(r.Result), up.HashAt(19)) && hc.Head() == 21
	}, 10*time.Second, 50*time.Millisecond)
	_, ok := hc.BlockByHash(orphan, false)
	require.False(t, ok)
	logs := doRpc(t, send, "eth_getLogs", `[{"fromBlock":"0x12","toBlock":"0x15"}]`)
	require.NotContains(t, strings.ToLower(string(logs.Result)), strings.ToLower(orphan))
	require.Contains(t, strings.ToLower(string(logs.Result)), strings.ToLower(up.HashAt(19)))
}

func TestHttp_HeadCache_DisabledPreservesBehavior(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := headCacheTestConfig(up.URL(), nil)
	send, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()
	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	require.Nil(t, nw.HeadCache())
	before := up.BlockCalls(19)
	r := doRpc(t, send, "eth_getBlockByNumber", `["0x13",false]`)
	require.Contains(t, string(r.Result), up.HashAt(19))
	require.Greater(t, up.BlockCalls(19), before)
}

// Two eRPC processes (separate ERPC instances and HTTP servers) share one
// Redis: exactly one hydrates, both serve the committed window, and the
// follower takes over hydration when the leader stops.
func TestHttp_HeadCache_SharedRedisTwoReplicas(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	mk := func() *common.Config {
		return headCacheTestConfig(up.URL(), &common.EvmHeadCacheConfig{
			Enabled: true, Depth: 16,
			Namespace:    fmt.Sprintf("e2e-%d", time.Now().UnixNano()),
			PollInterval: common.Duration(100 * time.Millisecond),
		})
	}
	cfgA, cfgB := mk(), mk()
	cfgB.Projects[0].Networks[0].Evm.HeadCache.Namespace = cfgA.Projects[0].Networks[0].Evm.HeadCache.Namespace
	sendA, _, _, shutdownA, eA := createServerTestFixtures(cfgA, t)
	sendB, _, _, shutdownB, eB := createServerTestFixtures(cfgB, t)
	defer shutdownB()

	get := func(e *ERPC) *Network {
		p, err := e.GetProject("test_project")
		require.NoError(t, err)
		n, err := p.GetNetwork(t.Context(), "evm:123")
		require.NoError(t, err)
		return n
	}
	ca, cb := get(eA).HeadCache(), get(eB).HeadCache()
	require.Eventually(t, func() bool { return ca.Head() == 20 && cb.Head() == 20 }, 10*time.Second, 50*time.Millisecond)
	leaders := 0
	for _, c := range []*headcache.Cache{ca, cb} {
		if c.Stats.Hydrated.Load() > 0 {
			leaders++
		}
	}
	require.Equal(t, 1, leaders, "exactly one replica hydrates")

	before := up.BlockCalls(15) + up.RangeLogCalls()
	ra := doRpc(t, sendA, "eth_getLogs", `[{"fromBlock":"0xa","toBlock":"0x12"}]`)
	rb := doRpc(t, sendB, "eth_getLogs", `[{"fromBlock":"0xa","toBlock":"0x12"}]`)
	require.JSONEq(t, string(ra.Result), string(rb.Result))
	require.Equal(t, before, up.BlockCalls(15)+up.RangeLogCalls())

	// Stop the leader; the other replica must take over and keep advancing.
	leader, follower := ca, cb
	if cb.Stats.Hydrated.Load() > 0 {
		leader, follower = cb, ca
	}
	leader.Stop()
	if leader == ca {
		shutdownA()
	} else {
		defer shutdownA()
	}
	up.Mine(3)
	require.Eventually(t, func() bool { return follower.Head() == 23 }, 15*time.Second, 50*time.Millisecond)
	require.Positive(t, follower.Stats.LeaderEpochs.Load())
}
