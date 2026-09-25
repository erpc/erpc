package erpc

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// fillUpstream is a minimal EVM JSON-RPC upstream that counts eth_call and
// can delay it or answer null.
type fillUpstream struct {
	srv       *httptest.Server
	ethCalls  atomic.Int64
	delay     atomic.Int64 // ms
	nullReply atomic.Bool
}

func newFillUpstream() *fillUpstream {
	u := &fillUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Id     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		var result interface{}
		switch req.Method {
		case "eth_chainId":
			result = "0x7b"
		case "net_version":
			result = "123"
		case "eth_syncing":
			result = false
		case "eth_blockNumber":
			result = "0x64"
		case "eth_getBlockByNumber":
			var ref string
			if len(req.Params) > 0 {
				_ = json.Unmarshal(req.Params[0], &ref)
			}
			n := int64(100)
			if ref == "finalized" || ref == "safe" {
				n = 90
			} else if strings.HasPrefix(ref, "0x") {
				fmt.Sscanf(ref, "0x%x", &n)
			}
			result = map[string]interface{}{
				"number": fmt.Sprintf("0x%x", n), "hash": fmt.Sprintf("0x%064x", n),
				"parentHash": fmt.Sprintf("0x%064x", n-1), "timestamp": fmt.Sprintf("0x%x", time.Now().Unix()),
				"transactions": []interface{}{},
			}
		case "eth_call":
			u.ethCalls.Add(1)
			if d := u.delay.Load(); d > 0 {
				time.Sleep(time.Duration(d) * time.Millisecond)
			}
			if u.nullReply.Load() {
				result = nil
			} else {
				result = "0x00000000000000000000000000000000000000000000000000000000000000ff"
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, req.Id)
			return
		}
		out, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": req.Id, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	return u
}

func cacheFillTestConfig(upstreamURL, redisAddr string, fill *common.CacheFillConfig) *common.Config {
	redisCfg := func() *common.RedisConnectorConfig {
		return &common.RedisConnectorConfig{
			URI:               "redis://" + redisAddr,
			InitTimeout:       common.Duration(500 * time.Millisecond),
			GetTimeout:        common.Duration(500 * time.Millisecond),
			SetTimeout:        common.Duration(500 * time.Millisecond),
			LockRetryInterval: common.Duration(20 * time.Millisecond),
		}
	}
	return &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Database: &common.DatabaseConfig{
			SharedState: &common.SharedStateConfig{
				ClusterKey: "cachefill-test",
				Connector:  &common.ConnectorConfig{Id: "ss", Driver: common.DriverRedis, Redis: redisCfg()},
			},
			EvmJsonRpcCache: &common.CacheConfig{
				Connectors: []*common.ConnectorConfig{{Id: "shared", Driver: common.DriverRedis, Redis: redisCfg()}},
				Policies: []*common.CachePolicyConfig{{
					Network: "*", Method: "*", Finality: common.DataFinalityStateFinalized,
					Connector: "shared", TTL: common.FixedDuration(time.Minute),
				}},
			},
		},
		Projects: []*common.ProjectConfig{{
			Id: "test_project",
			Networks: []*common.NetworkConfig{{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123},
				// Single attempt, no hedge: one healthy upstream read per fill.
				Failsafe:  []*common.FailsafeConfig{{MatchMethod: "*", Retry: &common.RetryPolicyConfig{MaxAttempts: 1}}},
				CacheFill: fill,
			}},
			Upstreams: []*common.UpstreamConfig{{
				Id: "fill-up", Type: common.UpstreamTypeEvm, Endpoint: upstreamURL,
				Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(time.Second)},
			}},
		}},
		RateLimiters: &common.RateLimiterConfig{},
	}
}

type fillReplica struct {
	send func(string, map[string]string, map[string]string) (int, map[string]string, string)
}

func startFillReplicas(t *testing.T, upURL, redisAddr string, fill func() *common.CacheFillConfig) (fillReplica, fillReplica) {
	sa, _, _, shutA, _ := createServerTestFixtures(cacheFillTestConfig(upURL, redisAddr, fill()), t)
	sb, _, _, shutB, _ := createServerTestFixtures(cacheFillTestConfig(upURL, redisAddr, fill()), t)
	t.Cleanup(func() { shutA(); shutB() })
	return fillReplica{sa}, fillReplica{sb}
}

const fillCallBody = `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x5fbdb2315678afecb367f032d93f642f64180aa3","data":"0x%s"},"%s"]}`

type timedResult struct {
	code    int
	body    string
	elapsed time.Duration
}

func concurrentCalls(reps []fillReplica, body string) []timedResult {
	out := make([]timedResult, len(reps))
	var wg sync.WaitGroup
	for i, r := range reps {
		wg.Add(1)
		go func(i int, r fillReplica) {
			defer wg.Done()
			st := time.Now()
			code, _, b := r.send(body, nil, nil)
			out[i] = timedResult{code, b, time.Since(st)}
		}(i, r)
	}
	wg.Wait()
	return out
}

func fillCounter(outcome string) float64 {
	return promUtil.ToFloat64(telemetry.MetricCacheFillTotal.WithLabelValues("test_project", "evm:123", outcome))
}

func defaultFill() *common.CacheFillConfig {
	return &common.CacheFillConfig{
		Enabled: true, LockTtl: common.Duration(5 * time.Second), MaxWait: common.Duration(3 * time.Second),
		PollInterval: common.Duration(20 * time.Millisecond), LockAcquireTimeout: common.Duration(200 * time.Millisecond),
	}
}

// Two replicas, simultaneous cold miss for a pinned, cacheable eth_call:
// exactly one upstream read; the other replica serves it from its cache.
func TestCacheFill_TwoReplicasOneUpstreamRead(t *testing.T) {
	mr := miniredis.RunT(t)
	up := newFillUpstream()
	defer up.srv.Close()
	up.delay.Store(300)
	a, b := startFillReplicas(t, up.srv.URL, mr.Addr(), defaultFill)
	hitBefore := fillCounter("follower_hit")

	res := concurrentCalls([]fillReplica{a, b}, fmt.Sprintf(fillCallBody, "01", "0x5"))
	for _, r := range res {
		require.Equal(t, 200, r.code, r.body)
		require.Contains(t, r.body, "ff\"")
	}
	require.Equal(t, int64(1), up.ethCalls.Load(), "exactly one upstream eth_call across replicas")
	require.Equal(t, hitBefore+1, fillCounter("follower_hit"))
}

// Leader gets a null (not cached under empty=ignore): the waiter must not sit
// out maxWait, it forwards as soon as the "uncached" marker appears.
func TestCacheFill_UncachedResponseReleasesWaiterEarly(t *testing.T) {
	mr := miniredis.RunT(t)
	up := newFillUpstream()
	defer up.srv.Close()
	up.delay.Store(200)
	up.nullReply.Store(true)
	a, b := startFillReplicas(t, up.srv.URL, mr.Addr(), defaultFill)

	res := concurrentCalls([]fillReplica{a, b}, fmt.Sprintf(fillCallBody, "02", "0x5"))
	for _, r := range res {
		require.Equal(t, 200, r.code, r.body)
		require.Less(t, r.elapsed, 1500*time.Millisecond, "waiter must not wait maxWait (3s)")
	}
	require.Equal(t, int64(2), up.ethCalls.Load())
}

// Slow leader: the waiter gives up at maxWait and calls upstream itself.
func TestCacheFill_MaxWaitBoundsWaiter(t *testing.T) {
	mr := miniredis.RunT(t)
	up := newFillUpstream()
	defer up.srv.Close()
	up.delay.Store(1500)
	a, b := startFillReplicas(t, up.srv.URL, mr.Addr(), func() *common.CacheFillConfig {
		c := defaultFill()
		c.MaxWait = common.Duration(300 * time.Millisecond)
		return c
	})
	res := concurrentCalls([]fillReplica{a, b}, fmt.Sprintf(fillCallBody, "03", "0x5"))
	for _, r := range res {
		require.Equal(t, 200, r.code, r.body)
		require.Less(t, r.elapsed, 2500*time.Millisecond)
	}
	require.Equal(t, int64(2), up.ethCalls.Load())
}

// Redis unavailable: fill fails open immediately (no lock wait beyond the
// acquire timeout), both requests succeed from upstream.
func TestCacheFill_RedisDownFailsOpen(t *testing.T) {
	mr := miniredis.RunT(t)
	up := newFillUpstream()
	defer up.srv.Close()
	a, b := startFillReplicas(t, up.srv.URL, mr.Addr(), defaultFill)
	mr.Close()
	errBefore := fillCounter("error")
	res := concurrentCalls([]fillReplica{a, b}, fmt.Sprintf(fillCallBody, "04", "0x5"))
	for _, r := range res {
		require.Equal(t, 200, r.code, r.body)
		require.Contains(t, r.body, "ff\"")
		require.Less(t, r.elapsed, 2*time.Second)
	}
	require.Equal(t, int64(2), up.ethCalls.Load())
	require.GreaterOrEqual(t, fillCounter("error"), errBefore)
}

// Tag-based reads have an unstable key (latest moves) and never coordinate.
func TestCacheFill_TagRequestsBypass(t *testing.T) {
	mr := miniredis.RunT(t)
	up := newFillUpstream()
	defer up.srv.Close()
	up.delay.Store(200)
	a, b := startFillReplicas(t, up.srv.URL, mr.Addr(), defaultFill)
	leaderBefore := fillCounter("leader")
	res := concurrentCalls([]fillReplica{a, b}, fmt.Sprintf(fillCallBody, "05", "latest"))
	for _, r := range res {
		require.Equal(t, 200, r.code, r.body)
		require.Less(t, r.elapsed, 1500*time.Millisecond)
	}
	require.Equal(t, int64(2), up.ethCalls.Load())
	require.Equal(t, leaderBefore, fillCounter("leader"), "tag reads must not take the fill lock")
}

// Head cache hydration reads unfinalized head data that a later reorg may
// orphan. Those reads must not be written into the ordinary JSON-RPC cache
// (where they would only expire by TTL); the head cache owns that data.
func TestHeadCache_HydrationDoesNotWriteOrdinaryCache(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := headCacheTestConfig(up.URL(), &common.EvmHeadCacheConfig{
		Enabled: true, Depth: 16, PollInterval: common.Duration(100 * time.Millisecond),
	})
	cfg.Database = &common.DatabaseConfig{EvmJsonRpcCache: &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{{Id: "mem", Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 10_000, MaxTotalSize: "64MB"}}},
		Policies: []*common.CachePolicyConfig{{Network: "*", Method: "*", Finality: common.DataFinalityStateUnfinalized,
			Connector: "mem", TTL: common.FixedDuration(time.Minute)}},
	}}
	_, _, _, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	p, err := e.GetProject("test_project")
	require.NoError(t, err)
	n, err := p.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return n.HeadCache().Head() == 20 }, 10*time.Second, 50*time.Millisecond)
	require.NotNil(t, n.cacheDal)
	time.Sleep(200 * time.Millisecond) // async writes, if any, would have landed

	for _, params := range []string{`["0x14",true]`, `["0x13",true]`} {
		rq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":` + params + `}`))
		rq.SetNetwork(n)
		resp, _ := n.cacheDal.Get(t.Context(), rq)
		require.True(t, resp == nil || resp.IsObjectNull(t.Context()), "hydrated block %s leaked into ordinary cache", params)
	}
}
