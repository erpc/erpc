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

func blockStoreOf(t *testing.T, e *ERPC) *blockstore.Cache {
	t.Helper()
	hc := instanceNetwork(t, e).BlockStore()
	require.NotNil(t, hc)
	return hc
}

func instanceNetwork(t *testing.T, e *ERPC) *Network {
	t.Helper()
	prj, err := e.GetProject("test_project")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	return nw
}

// holdSubscriber keeps one drained newHeads subscription open on hc, which is
// what turns header following on (the store does not follow the chain without
// subscribers). Tests asserting follower behaviour use it.
func holdSubscriber(t *testing.T, hc *blockstore.Cache) {
	t.Helper()
	sub := hc.Subscribe(1024)
	go func() {
		for range sub.C {
		}
	}()
	t.Cleanup(sub.Close)
}

// The background refresh fetches headers only; the first client read of a
// block body or logs fetches it once, and every later read (any filter, full
// or hash-only) is served from the cache.
func TestHttp_BlockStore_ServesReusesAndHandlesReorg(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{
		Enabled: true, Depth: 16, PollInterval: common.Duration(100 * time.Millisecond),
	})
	send, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()
	hc := blockStoreOf(t, erpcInstance)
	holdSubscriber(t, hc)
	require.Eventually(t, func() bool { return hc.Head() == 20 && hc.CanonicalHash(5) != "" },
		10*time.Second, 50*time.Millisecond, "the header window must cover the full depth")
	require.Zero(t, up.FullBlockCalls(), "the background never fetches block bodies")
	require.Zero(t, up.BlockHashLogCalls(), "the background never fetches logs")

	// Hash-only reads are served from the verified header with no upstream call.
	headerCalls := up.HeaderCalls()
	lite := doRpc(t, send, "eth_getBlockByNumber", `["0x13",false]`)
	require.Contains(t, string(lite.Result), scriptedTx(19, "a"))
	require.NotContains(t, string(lite.Result), `"from"`)
	byHash := doRpc(t, send, "eth_getBlockByHash", fmt.Sprintf(`[%q,false]`, up.HashAt(19)))
	require.JSONEq(t, string(lite.Result), string(byHash.Result))
	require.Zero(t, up.FullBlockCalls())
	require.LessOrEqual(t, up.HeaderCalls()-headerCalls, int64(2), "only background header polling, not the reads")

	// A full block is fetched once, then served from the cache.
	full := doRpc(t, send, "eth_getBlockByNumber", `["0x13",true]`)
	require.Contains(t, string(full.Result), `"from"`)
	require.Equal(t, int64(1), up.FullBlockCalls())
	again := doRpc(t, send, "eth_getBlockByHash", fmt.Sprintf(`[%q,true]`, up.HashAt(19)))
	require.JSONEq(t, string(full.Result), string(again.Result))
	require.Equal(t, int64(1), up.FullBlockCalls(), "the second read is a cache hit")
	require.Zero(t, up.Calls("eth_getBlockByHash"))

	// Logs over window heights: one blockHash fetch per height, then any
	// filter is served locally.
	even := doRpc(t, send, "eth_getLogs", fmt.Sprintf(`[{"fromBlock":"0x8","toBlock":"0x13","topics":[%q]}]`, scriptedTopicEven))
	var evenLogs []map[string]interface{}
	require.NoError(t, json.Unmarshal(even.Result, &evenLogs))
	require.Len(t, evenLogs, 6)
	require.Equal(t, int64(12), up.BlockHashLogCalls(), "one logs fetch per block hash")
	all := doRpc(t, send, "eth_getLogs", fmt.Sprintf(`[{"fromBlock":"0x8","toBlock":"0x13","address":%q}]`, scriptedEmitter))
	var allLogs []map[string]interface{}
	require.NoError(t, json.Unmarshal(all.Result, &allLogs))
	require.Len(t, allLogs, 12)
	none := doRpc(t, send, "eth_getLogs", `[{"fromBlock":"0x8","toBlock":"0x13","address":"0x0000000000000000000000000000000000000001"}]`)
	require.JSONEq(t, `[]`, string(none.Result))
	require.Equal(t, int64(12), up.BlockHashLogCalls(), "later filters reuse cached logs")
	require.Zero(t, up.RangeLogCalls())

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

	// Reorg at 19 (seen once 21 does not link): numeric reads follow the new
	// chain and payloads cached for the orphan are never served.
	orphan := up.HashAt(19)
	up.Reorg(19, "b")
	up.Mine(1)
	require.Eventually(t, func() bool { return hc.CanonicalHash(19) == strings.ToLower(up.HashAt(19)) },
		10*time.Second, 20*time.Millisecond, "header walk-back must replace the orphan hash")
	r := doRpc(t, send, "eth_getBlockByNumber", `["0x13",true]`)
	require.Contains(t, string(r.Result), up.HashAt(19))
	require.NotContains(t, string(r.Result), orphan)
	_, ok := hc.BlockByHash(t.Context(), orphan, false)
	require.False(t, ok)
	logs := doRpc(t, send, "eth_getLogs", `[{"fromBlock":"0x12","toBlock":"0x14"}]`)
	require.NotContains(t, strings.ToLower(string(logs.Result)), strings.ToLower(orphan))
	require.Contains(t, strings.ToLower(string(logs.Result)), strings.ToLower(up.HashAt(19)))
	require.Zero(t, up.Calls("eth_getBlockByHash"))
}

// The getLogs serve branch runs before the network pre-forward hook, so it must
// enforce the same hard limits itself rather than answer what the network rejects.
func TestHttp_BlockStore_GetLogsHonorsNetworkHardLimits(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{
		Enabled: true, Depth: 16, PollInterval: common.Duration(100 * time.Millisecond),
	})
	cfg.Projects[0].Networks[0].Evm.GetLogsMaxAllowedRange = 4
	cfg.Projects[0].Networks[0].Evm.GetLogsMaxAllowedAddresses = 1
	cfg.Projects[0].Networks[0].Evm.GetLogsMaxAllowedTopics = 1
	send, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()
	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	hc := nw.BlockStore()
	require.NotNil(t, hc)
	holdSubscriber(t, hc)
	require.Eventually(t, func() bool {
		_, ok := hc.LogsRange(t.Context(), 8, 19, nil)
		return hc.Head() == 20 && ok
	}, 10*time.Second, 50*time.Millisecond)

	rejectCode := func(params string) string {
		_, _, body := send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":%s}`, params), nil, nil)
		var r struct {
			Error *struct {
				Data map[string]interface{} `json:"data"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &r), body)
		require.NotNil(t, r.Error, "must be rejected, not served from the head cache: %s", body)
		return body
	}
	other := "0x0000000000000000000000000000000000000002"
	require.Contains(t, rejectCode(`[{"fromBlock":"0x8","toBlock":"0x13"}]`), "ErrGetLogsExceededMaxAllowedRange")
	require.Contains(t, rejectCode(fmt.Sprintf(`[{"fromBlock":"0x8","toBlock":"0x9","address":[%q,%q]}]`, scriptedEmitter, other)), "ErrGetLogsExceededMaxAllowedAddresses")
	require.Contains(t, rejectCode(fmt.Sprintf(`[{"fromBlock":"0x8","toBlock":"0x9","topics":[[%q,%q]]}]`, scriptedTopicEven, scriptedTopicOdd)), "ErrGetLogsExceededMaxAllowedTopics")

	// Within every limit, the head cache still answers without an upstream call.
	before := up.RangeLogCalls()
	ok := doRpc(t, send, "eth_getLogs", fmt.Sprintf(`[{"fromBlock":"0x8","toBlock":"0xb","address":[%q],"topics":[%q]}]`, scriptedEmitter, scriptedTopicEven))
	require.NotEqual(t, "null", string(ok.Result))
	require.Equal(t, before, up.RangeLogCalls(), "an in-limit range is still served from the head cache")
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

// The lease holder verifies headers and shares its snapshot and header
// payloads. The follower reads them without upstream calls; a body or logs
// payload fetched by one replica on a miss is reused by the other.
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
	ca := blockStoreOf(t, eA)
	// A subscriber on A turns following on fleet-wide (shared presence mark).
	holdSubscriber(t, ca)
	require.Eventually(t, func() bool { return ca.Head() == 20 && ca.CanonicalHash(5) != "" },
		10*time.Second, 50*time.Millisecond, "replica A must verify its complete header window before replica B starts")

	sendB, _, baseB, shutdownB, eB := createServerTestFixtures(cfgB, t)
	defer shutdownB()
	cb := blockStoreOf(t, eB)
	require.Eventually(t, func() bool { return cb.Head() == 20 && cb.CanonicalHash(5) != "" },
		10*time.Second, 50*time.Millisecond, "replica B must install the shared header window")
	require.Equal(t, ca.Head(), cb.Head())
	require.Zero(t, up.FullBlockCalls()+up.BlockHashLogCalls()+up.RangeLogCalls(), "no bodies or logs without client reads")

	// A miss on A fetches once; B serves the same payloads from Redis.
	ra := doRpc(t, sendA, "eth_getLogs", `[{"fromBlock":"0xa","toBlock":"0x12"}]`)
	fa := doRpc(t, sendA, "eth_getBlockByNumber", `["0x13",true]`)
	require.Equal(t, int64(9), up.BlockHashLogCalls())
	require.Equal(t, int64(1), up.FullBlockCalls())
	rb := doRpc(t, sendB, "eth_getLogs", `[{"fromBlock":"0xa","toBlock":"0x12"}]`)
	fb := doRpc(t, sendB, "eth_getBlockByNumber", `["0x13",true]`)
	header := doRpc(t, sendB, "eth_getBlockByNumber", `["0x13",false]`)
	require.JSONEq(t, string(ra.Result), string(rb.Result))
	require.JSONEq(t, string(fa.Result), string(fb.Result))
	require.Contains(t, string(header.Result), up.HashAt(19))
	require.Equal(t, int64(9), up.BlockHashLogCalls(), "replica B reuses shared logs")
	require.Equal(t, int64(1), up.FullBlockCalls(), "replica B reuses the shared block")
	require.Zero(t, cb.Stats.Hydrated.Load(), "replica B fetched nothing from upstream")

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
	// reorg from its own header refresh.
	shutdownA()
	aStopped = true
	orphan := up.HashAt(19)
	up.Reorg(19, "b")
	up.Mine(1)
	newHash := up.HashAt(19)
	require.Eventually(t, func() bool {
		block, ok := cb.BlockByNumber(t.Context(), 19, false)
		return ok && strings.Contains(string(block), newHash)
	}, 10*time.Second, 20*time.Millisecond, "the takeover leader should walk back the reorg")
	reorgHTTP := doRpc(t, sendB, "eth_getBlockByNumber", `["0x13",true]`)
	require.Contains(t, string(reorgHTTP.Result), newHash)
	_, orphanCanonical := cb.BlockByHash(t.Context(), orphan, false)
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
	hash := "0x" + strings.Repeat("A", 64)
	payload := json.RawMessage(`{"number":"0x1"}`)
	ttl := 150 * time.Millisecond
	ctx := t.Context()
	require.NoError(t, store.PutPayload(ctx, scope, blockstore.PayloadBlock, hash, payload, ttl))
	got, err := store.GetPayload(ctx, scope, blockstore.PayloadBlock, strings.ToLower(hash))
	require.NoError(t, err)
	require.JSONEq(t, string(payload), string(got))
	_, err = store.GetPayload(ctx, scope, blockstore.PayloadLogs, hash)
	require.ErrorIs(t, err, blockstore.ErrNotFound, "payload kinds are keyed separately")

	otherScope := scope
	otherScope.Namespace = "different-scope"
	got, err = store.GetPayload(ctx, otherScope, blockstore.PayloadBlock, hash)
	require.Nil(t, got)
	require.ErrorIs(t, err, blockstore.ErrNotFound)

	partition, err := adapter.partition(scope)
	require.NoError(t, err)
	key := partition + ":" + payloadKey(blockstore.PayloadBlock, hash)
	require.NoError(t, adapter.redis.Client().Del(ctx, key).Err())
	require.NoError(t, adapter.redis.Client().LPush(ctx, key, "wrong type").Err())
	_, err = store.GetPayload(ctx, scope, blockstore.PayloadBlock, hash)
	require.Error(t, err)
	require.NotErrorIs(t, err, blockstore.ErrStoreUnavailable, "Redis server errors must stay fail-closed")
	require.NoError(t, connector.Set(ctx, partition, payloadKey(blockstore.PayloadBlock, hash), payload, &ttl))

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = store.GetPayload(canceled, scope, blockstore.PayloadBlock, hash)
	require.ErrorIs(t, err, blockstore.ErrStoreUnavailable)
	require.ErrorIs(t, err, context.Canceled, "the connector error must remain available to callers")

	mr.FastForward(ttl + 50*time.Millisecond)
	got, err = store.GetPayload(ctx, scope, blockstore.PayloadBlock, hash)
	require.Nil(t, got)
	require.ErrorIs(t, err, blockstore.ErrNotFound)

	// The payload fill lock is token-fenced and exclusive per key.
	release, ok, err := adapter.TryLock(ctx, scope, "block/"+hash, time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = adapter.TryLock(ctx, scope, "block/"+hash, time.Second)
	require.NoError(t, err)
	require.False(t, ok)
	held, err := adapter.Locked(ctx, scope, "block/"+hash)
	require.NoError(t, err)
	require.True(t, held)
	release(ctx)
	held, err = adapter.Locked(ctx, scope, "block/"+hash)
	require.NoError(t, err)
	require.False(t, held)
}

// The refresh takes its tip from the network's in-memory latest block (state
// pollers), so it does not depend on eth_blockNumber: with eth_blockNumber
// failing upstream the window still tracks the chain. eth_blockNumber is only
// a fallback while that value is unknown or has not moved for a while.
func TestHttp_BlockStore_HeadComesFromStatePoller(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	up.FailBlockNumber(true)
	cfg := blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{
		Enabled: true, Depth: 16, PollInterval: common.Duration(100 * time.Millisecond),
		MaxStaleness: common.Duration(500 * time.Millisecond),
	})
	_, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
	defer shutdown()
	hc := blockStoreOf(t, instance)
	holdSubscriber(t, hc)
	require.Eventually(t, func() bool { return hc.Head() == 20 }, 5*time.Second, 20*time.Millisecond)
	up.Mine(2)
	require.Eventually(t, func() bool { return hc.Head() == 22 }, 5*time.Second, 20*time.Millisecond,
		"with eth_blockNumber failing upstream, the head still advances from the state pollers")
	require.Zero(t, hc.Stats.Hydrated.Load())
	require.Zero(t, up.FullBlockCalls()+up.BlockHashLogCalls())
}

func TestHttp_BlockStore_SlowBodyFetchIsOnDemand(t *testing.T) {
	up := newScriptedEvmUpstream(123, 2)
	defer up.Close()
	const blockDelay = 80 * time.Millisecond
	up.SetFullBlockDelay(blockDelay)
	cfg := blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{
		Enabled: true, Depth: 2, MaxPerTick: 2, Concurrency: 1,
		PollInterval: common.Duration(100 * time.Millisecond),
		FetchTimeout: common.Duration(2 * time.Second),
	})
	send, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
	defer shutdown()
	hc := blockStoreOf(t, instance)
	holdSubscriber(t, hc)
	require.Eventually(t, func() bool { return hc.Head() == 2 && hc.CanonicalHash(1) != "" },
		5*time.Second, 20*time.Millisecond, "the window must cover the configured depth")
	require.Zero(t, hc.Stats.Hydrated.Load())
	require.Zero(t, up.FullBlockCalls())
	require.Zero(t, up.BlockHashLogCalls())

	started := time.Now()
	block := doRpc(t, send, "eth_getBlockByNumber", `["0x1",true]`)
	require.Contains(t, string(block.Result), `"transactions"`)
	require.GreaterOrEqual(t, time.Since(started), blockDelay, "a body miss waits for its one upstream fetch")
	logs := doRpc(t, send, "eth_getLogs", `[{"fromBlock":"0x1","toBlock":"0x2"}]`)
	require.JSONEq(t, `[{"address":"`+scriptedEmitter+`","topics":["`+scriptedTopicOdd+`"],"data":"0x","blockNumber":"0x1","blockHash":"`+up.HashAt(1)+`","transactionHash":"`+scriptedTx(1, "a")+`","transactionIndex":"0x0","logIndex":"0x0","removed":false},{"address":"`+scriptedEmitter+`","topics":["`+scriptedTopicEven+`"],"data":"0x","blockNumber":"0x2","blockHash":"`+up.HashAt(2)+`","transactionHash":"`+scriptedTx(2, "a")+`","transactionIndex":"0x0","logIndex":"0x0","removed":false}]`, string(logs.Result))
	require.Equal(t, int64(1), up.FullBlockCalls())
	require.Equal(t, int64(2), up.BlockHashLogCalls())
	require.Equal(t, int64(3), hc.Stats.Hydrated.Load())

	hitsBefore := hc.Stats.Hits.Load()
	doRpc(t, send, "eth_getBlockByNumber", `["0x1",true]`)
	doRpc(t, send, "eth_getLogs", `[{"fromBlock":"0x1","toBlock":"0x2"}]`)
	require.Equal(t, int64(1), up.FullBlockCalls(), "cached block response must not hit upstream")
	require.Equal(t, int64(2), up.BlockHashLogCalls(), "cached log range must not hit upstream")
	require.Equal(t, hitsBefore+2, hc.Stats.Hits.Load(), "complete block and log reads must be cache hits")
}
