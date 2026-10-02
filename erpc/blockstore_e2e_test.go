package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/blockstore"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

var (
	blockStoreTestRedisOnce sync.Once
	blockStoreTestRedisAddr string
)

// blockStoreTestRedis returns a process-wide Redis address for head cache
// tests (BLOCKSTORE_TEST_REDIS_ADDR or an in-process miniredis). Scopes are
// isolated by the per-test upstream fingerprint and namespace.
func blockStoreTestRedis() string {
	blockStoreTestRedisOnce.Do(func() {
		blockStoreTestRedisAddr = os.Getenv("BLOCKSTORE_TEST_REDIS_ADDR")
		if blockStoreTestRedisAddr == "" {
			mr, err := miniredis.Run()
			if err != nil {
				panic(err)
			}
			blockStoreTestRedisAddr = mr.Addr()
		}
	})
	return blockStoreTestRedisAddr
}

// blockStoreTestConfig wires an enabled head cache to a redis connector under
// database.evmJsonRpcCache (the only supported store).
func blockStoreTestConfig(upstreamURL string, hc *common.EvmBlockStoreConfig) *common.Config {
	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true), WebSocket: &common.WebSocketServerConfig{Enabled: true}},
		Projects: []*common.ProjectConfig{{
			Id: "test_project",
			Networks: []*common.NetworkConfig{{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123, BlockStore: hc},
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
			hc.ConnectorId = "blockstore-redis"
		}
		if hc.Namespace == "" {
			hc.Namespace = fmt.Sprintf("t-%d", time.Now().UnixNano())
		}
		cfg.Database = &common.DatabaseConfig{EvmJsonRpcCache: &common.CacheConfig{
			Connectors: []*common.ConnectorConfig{{
				Id: hc.ConnectorId, Driver: common.DriverRedis,
				Redis: &common.RedisConnectorConfig{URI: "redis://" + blockStoreTestRedis()},
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
	require.Empty(t, r.Error, body)
	return r
}

func TestHttp_BlockStore_ServesReusesAndHandlesReorg(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{
		Enabled: true, Depth: 16, PollInterval: common.Duration(100 * time.Millisecond),
	})
	send, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()

	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	hc := nw.BlockStore()
	require.NotNil(t, hc)
	require.Eventually(t, func() bool {
		_, blockReady := hc.BlockByNumber(19, true)
		_, logsReady := hc.LogsRange(8, 19, nil)
		return hc.Head() == 20 && blockReady && logsReady
	}, 10*time.Second, 50*time.Millisecond, "cache must hydrate the block and log range before HTTP cache-hit assertions")

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

	// Same-height reorg: numeric mapping follows the new chain, and orphan
	// hashes stop being served from the cache.
	orphan := up.HashAt(19)
	up.Reorg(19, "b")
	var reorgVisible bool
	require.Eventually(t, func() bool {
		code, _, body := send(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x13",false]}`, nil, nil)
		if code != 200 {
			return false
		}
		var response rpcResp
		if err := json.Unmarshal([]byte(body), &response); err != nil || response.Error != nil {
			return false
		}
		reorgVisible = strings.Contains(string(response.Result), up.HashAt(19)) && hc.Head() == 20
		return reorgVisible
	}, 10*time.Second, 50*time.Millisecond)
	require.True(t, reorgVisible, "reorg should become visible through HTTP")
	r := doRpc(t, send, "eth_getBlockByNumber", `["0x13",false]`)
	require.Contains(t, string(r.Result), up.HashAt(19))
	_, ok := hc.BlockByHash(orphan, false)
	require.False(t, ok)
	logs := doRpc(t, send, "eth_getLogs", `[{"fromBlock":"0x12","toBlock":"0x14"}]`)
	require.NotContains(t, strings.ToLower(string(logs.Result)), strings.ToLower(orphan))
	require.Contains(t, strings.ToLower(string(logs.Result)), strings.ToLower(up.HashAt(19)))
}

func TestHttp_BlockStore_DisabledPreservesBehavior(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := blockStoreTestConfig(up.URL(), nil)
	send, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()
	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	require.Nil(t, nw.BlockStore())
	before := up.BlockCalls(19)
	r := doRpc(t, send, "eth_getBlockByNumber", `["0x13",false]`)
	require.Contains(t, string(r.Result), up.HashAt(19))
	require.Greater(t, up.BlockCalls(19), before)
}

// The lease holder verifies headers and shares its snapshot and immutable
// block/log payloads. The follower reads them without hydrating upstream.
func TestHttp_BlockStore_SharedRedisTwoReplicas(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	mk := func() *common.Config {
		cfg := blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{
			Enabled: true, Depth: 16,
			Namespace:    fmt.Sprintf("e2e-%d", time.Now().UnixNano()),
			PollInterval: common.Duration(100 * time.Millisecond),
			MaxStaleness: common.Duration(5 * time.Second),
		})
		return cfg
	}
	cfgA, cfgB := mk(), mk()
	cfgB.Projects[0].Networks[0].Evm.BlockStore.Namespace = cfgA.Projects[0].Networks[0].Evm.BlockStore.Namespace
	sendA, _, _, shutdownA, eA := createServerTestFixtures(cfgA, t)
	aStopped := false
	defer func() {
		if !aStopped {
			shutdownA()
		}
	}()

	get := func(e *ERPC) *Network {
		p, err := e.GetProject("test_project")
		require.NoError(t, err)
		n, err := p.GetNetwork(t.Context(), "evm:123")
		require.NoError(t, err)
		return n
	}
	ca := get(eA).BlockStore()
	require.Eventually(t, func() bool {
		_, logsReady := ca.LogsRange(5, 20, nil)
		return ca.Head() == 20 && logsReady
	}, 10*time.Second, 50*time.Millisecond, "replica A must hydrate its complete window before replica B starts")
	require.Positive(t, ca.Stats.Hydrated.Load())

	fullBeforeB, logsByHashBeforeB, rangeLogsBeforeB := up.FullBlockCalls(), up.BlockHashLogCalls(), up.RangeLogCalls()
	headersBeforeB := up.HeaderCalls()
	blockNumbersBeforeB := up.Calls("eth_blockNumber")
	latestBlocksBeforeB := up.LatestBlockCalls()
	sendB, _, baseB, shutdownB, eB := createServerTestFixtures(cfgB, t)
	defer shutdownB()
	cb := get(eB).BlockStore()
	require.Eventually(t, func() bool {
		_, logsReady := cb.LogsRange(5, 20, nil)
		return cb.Head() == 20 && logsReady
	}, 10*time.Second, 50*time.Millisecond, "replica B must reuse the complete shared payload window")
	require.Zero(t, cb.Stats.Hydrated.Load(), "replica B should reuse Redis payloads")
	require.Equal(t, fullBeforeB, up.FullBlockCalls(), "replica B must not re-fetch full blocks")
	require.Equal(t, logsByHashBeforeB, up.BlockHashLogCalls(), "replica B must not re-fetch block logs")
	require.Equal(t, rangeLogsBeforeB, up.RangeLogCalls(), "replica B must not re-fetch range logs")
	t.Logf("follower bootstrap RPC counts: eth_blockNumber=%d, latest-block queries=%d (independent EVM state-poller probes), explicit headers=%d (leader may poll), full blocks=%d, block-hash logs=%d, range logs=%d",
		up.Calls("eth_blockNumber")-blockNumbersBeforeB, up.LatestBlockCalls()-latestBlocksBeforeB,
		up.HeaderCalls()-headersBeforeB,
		up.FullBlockCalls()-fullBeforeB, up.BlockHashLogCalls()-logsByHashBeforeB,
		up.RangeLogCalls()-rangeLogsBeforeB)
	require.Equal(t, ca.Head(), cb.Head())

	// HTTP on the follower is served from its local view and shared payloads.
	before := up.FullBlockCalls() + up.BlockHashLogCalls() + up.RangeLogCalls()
	header := doRpc(t, sendB, "eth_getBlockByNumber", `["0x13",false]`)
	require.Contains(t, string(header.Result), up.HashAt(19))
	ra := doRpc(t, sendA, "eth_getLogs", `[{"fromBlock":"0xa","toBlock":"0x12"}]`)
	rb := doRpc(t, sendB, "eth_getLogs", `[{"fromBlock":"0xa","toBlock":"0x12"}]`)
	require.JSONEq(t, string(ra.Result), string(rb.Result))
	require.Equal(t, before, up.FullBlockCalls()+up.BlockHashLogCalls()+up.RangeLogCalls(), "both replicas serve payloads from cache")

	// A WebSocket connected to the follower receives events from its local view.
	w, _, err := dialWs(t, wsURL(baseB, ""), nil)
	require.NoError(t, err)
	headSub := w.call("eth_subscribe", `["newHeads"]`)
	require.Nil(t, headSub.Error)
	var headSubID string
	require.NoError(t, json.Unmarshal(headSub.Result, &headSubID))
	up.Mine(1)
	require.Eventually(t, func() bool { return ca.Head() == 21 }, 10*time.Second, 20*time.Millisecond,
		"leader should publish the new head after its refresh")
	require.Eventually(t, func() bool { return cb.Head() == 21 }, 10*time.Second, 20*time.Millisecond,
		"follower should install the leader's published snapshot")
	require.Contains(t, string(w.next(headSubID)), `"number":"0x15"`)

	// The follower takes leadership after A stops, then detects and serves a
	// same-height reorg from its new leader refresh.
	shutdownA()
	aStopped = true
	orphan := up.HashAt(19)
	up.Reorg(19, "b")
	newHash := up.HashAt(19)
	require.Eventually(t, func() bool {
		block, ok := cb.BlockByNumber(19, false)
		return ok && strings.Contains(string(block), newHash)
	}, 10*time.Second, 20*time.Millisecond, "the takeover leader should refresh the same-height reorg")
	reorgHTTP := doRpc(t, sendB, "eth_getBlockByNumber", `["0x13",false]`)
	require.Contains(t, string(reorgHTTP.Result), newHash)
	_, orphanCanonical := cb.BlockByHash(orphan, false)
	require.False(t, orphanCanonical, "the orphan hash must not remain in the takeover view")
}

func TestHttp_BlockStore_ConnectorTTLScopeAndCorruption(t *testing.T) {
	mr := miniredis.RunT(t)
	up := newScriptedEvmUpstream(123, 2)
	defer up.Close()
	hcCfg := &common.EvmBlockStoreConfig{Enabled: true, Depth: 2}
	cfg := blockStoreTestConfig(up.URL(), hcCfg)
	cfg.Database.EvmJsonRpcCache.Connectors[0].Redis.URI = "redis://" + mr.Addr()
	_, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
	defer shutdown()

	project, err := instance.GetProject("test_project")
	require.NoError(t, err)
	registry := project.networksRegistry
	store, err := registry.blockStoreStore(hcCfg)
	require.NoError(t, err)
	connector := registry.evmJsonRpcCache.Connector(hcCfg.ConnectorId)
	require.NotNil(t, connector)
	adapter, ok := store.(*blockStoreConnectorStore)
	require.True(t, ok)

	scope := blockstore.Scope{Namespace: "connector-test", ProjectId: "test_project", NetworkId: "evm:123"}
	record := &blockstore.BlockRecord{
		Number: 1, Hash: "0x" + strings.Repeat("a", 64),
		ParentHash: "0x" + strings.Repeat("b", 64),
		Block:      json.RawMessage(`{"number":"0x1"}`), Logs: json.RawMessage(`[]`),
	}
	ttl := 150 * time.Millisecond
	ctx := t.Context()
	require.NoError(t, store.PutBlock(ctx, scope, record, ttl))
	got, err := store.GetBlock(ctx, scope, record.Hash)
	require.NoError(t, err)
	require.Equal(t, record, got)

	otherScope := scope
	otherScope.Namespace = "different-scope"
	got, err = store.GetBlock(ctx, otherScope, record.Hash)
	require.Nil(t, got)
	require.ErrorIs(t, err, blockstore.ErrNotFound)

	partition, err := adapter.partition(scope)
	require.NoError(t, err)
	require.NoError(t, connector.Set(ctx, partition, strings.ToLower(record.Hash), []byte("{"), &ttl))
	_, err = store.GetBlock(ctx, scope, record.Hash)
	require.ErrorContains(t, err, "decode head cache record")
	require.NotErrorIs(t, err, blockstore.ErrStoreUnavailable, "corrupt payloads must not be treated as transient read failures")
	key := partition + ":" + strings.ToLower(record.Hash)
	require.NoError(t, adapter.redis.Client().Del(ctx, key).Err())
	require.NoError(t, adapter.redis.Client().LPush(ctx, key, "wrong type").Err())
	_, err = store.GetBlock(ctx, scope, record.Hash)
	require.Error(t, err)
	require.NotErrorIs(t, err, blockstore.ErrStoreUnavailable, "Redis server errors must stay fail-closed")
	require.NoError(t, connector.Set(ctx, partition, strings.ToLower(record.Hash), []byte("{"), &ttl))

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = store.GetBlock(canceled, scope, record.Hash)
	require.ErrorIs(t, err, blockstore.ErrStoreUnavailable)
	require.ErrorIs(t, err, context.Canceled, "the connector error must remain available to callers")

	mr.FastForward(ttl + 50*time.Millisecond)
	got, err = store.GetBlock(ctx, scope, record.Hash)
	require.Nil(t, got)
	require.ErrorIs(t, err, blockstore.ErrNotFound)
}

func TestHttp_BlockStore_BlockNumberFailureDoesNotSeedOrRefresh(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	up.FailBlockNumber(true)
	cfg := blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{
		Enabled: true, Depth: 16, PollInterval: common.Duration(100 * time.Millisecond),
		MaxStaleness: common.Duration(500 * time.Millisecond),
	})
	_, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
	defer shutdown()
	project, err := instance.GetProject("test_project")
	require.NoError(t, err)
	network, err := project.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	hc := network.BlockStore()
	require.NotNil(t, hc)
	require.Eventually(t, func() bool { return up.Calls("eth_blockNumber") > 0 }, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, int64(-1), hc.Head(), "failed discovery must not seed block zero")
	require.Zero(t, hc.Stats.Hydrated.Load())

	up.FailBlockNumber(false)
	require.Eventually(t, func() bool { return hc.Head() == 20 }, 5*time.Second, 20*time.Millisecond)
	up.FailBlockNumber(true)
	hc.Kick()
	require.Eventually(t, func() bool { return !hc.Fresh() }, 3*time.Second, 20*time.Millisecond,
		"failed discovery must not refresh the previous view's freshness")
	require.Equal(t, int64(-1), hc.Head(), "stale canonical view must stop being served")
}

func TestHttp_BlockStore_ColdFillCanExceedPollInterval(t *testing.T) {
	up := newScriptedEvmUpstream(123, 2)
	defer up.Close()
	const blockDelay = 80 * time.Millisecond
	up.SetFullBlockDelay(blockDelay)
	cfg := blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{
		Enabled: true, Depth: 2, MaxPerTick: 2, Concurrency: 1,
		PollInterval: common.Duration(100 * time.Millisecond),
		FetchTimeout: common.Duration(2 * time.Second),
	})
	started := time.Now()
	send, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
	defer shutdown()
	project, err := instance.GetProject("test_project")
	require.NoError(t, err)
	network, err := project.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	hc := network.BlockStore()
	require.NotNil(t, hc)
	require.Eventually(t, func() bool {
		_, ok := hc.BlockByNumber(1, true)
		return hc.Head() == 2 && ok
	}, 5*time.Second, 20*time.Millisecond, "cold fill must publish the complete configured window")
	require.GreaterOrEqual(t, time.Since(started), 2*blockDelay, "cold fill must complete despite taking longer than one poll interval")
	require.Equal(t, int64(2), hc.Stats.Hydrated.Load())
	require.Equal(t, int64(2), up.FullBlockCalls())
	require.Equal(t, int64(2), up.BlockHashLogCalls())

	hitsBefore := hc.Stats.Hits.Load()
	blocksBefore, logsBefore := up.FullBlockCalls(), up.BlockHashLogCalls()
	block := doRpc(t, send, "eth_getBlockByNumber", `["0x1",true]`)
	require.Contains(t, string(block.Result), `"transactions"`)
	logs := doRpc(t, send, "eth_getLogs", `[{"fromBlock":"0x1","toBlock":"0x2"}]`)
	require.JSONEq(t, `[{"address":"`+scriptedEmitter+`","topics":["`+scriptedTopicOdd+`"],"data":"0x","blockNumber":"0x1","blockHash":"`+up.HashAt(1)+`","transactionHash":"`+scriptedTx(1, "a")+`","transactionIndex":"0x0","logIndex":"0x0","removed":false},{"address":"`+scriptedEmitter+`","topics":["`+scriptedTopicEven+`"],"data":"0x","blockNumber":"0x2","blockHash":"`+up.HashAt(2)+`","transactionHash":"`+scriptedTx(2, "a")+`","transactionIndex":"0x0","logIndex":"0x0","removed":false}]`, string(logs.Result))
	require.Equal(t, blocksBefore, up.FullBlockCalls(), "cached block response must not hit upstream")
	require.Equal(t, logsBefore, up.BlockHashLogCalls(), "cached log range must not hit upstream")
	require.Equal(t, hitsBefore+2, hc.Stats.Hits.Load(), "complete block and log reads must be cache hits")
}
