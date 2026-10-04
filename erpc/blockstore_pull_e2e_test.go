package erpc

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

// withResponseCache adds an in-memory evmJsonRpcCache connector and policies
// (finalized and unfinalized) next to the block store's Redis connector.
func withResponseCache(cfg *common.Config) *common.Config {
	if cfg.Database == nil {
		cfg.Database = &common.DatabaseConfig{}
	}
	if cfg.Database.EvmJsonRpcCache == nil {
		cfg.Database.EvmJsonRpcCache = &common.CacheConfig{}
	}
	cc := cfg.Database.EvmJsonRpcCache
	cc.Connectors = append(cc.Connectors, &common.ConnectorConfig{
		Id: "resp-mem", Driver: common.DriverMemory,
		Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
	})
	for _, fin := range []common.DataFinalityState{common.DataFinalityStateFinalized, common.DataFinalityStateUnfinalized} {
		cc.Policies = append(cc.Policies, &common.CachePolicyConfig{
			Network: "*", Method: "*", Finality: fin, Connector: "resp-mem",
			TTL: common.FixedDuration(time.Minute),
		})
	}
	return cfg
}

// Block-store fetches read through (and write) eRPC's response cache: once a
// header is in the cache, the store's own fetch of it makes no upstream call.
func TestHttp_BlockStore_FetchReadsThroughResponseCache(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := withResponseCache(blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{
		Enabled: true, Depth: 4, PollInterval: common.Duration(100 * time.Millisecond),
	}))
	send, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
	defer shutdown()
	prj, err := instance.GetProject("test_project")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	f := &networkHeadFetcher{n: nw}

	// A client request puts eth_getBlockByNumber(10,false) in the response cache.
	client := doRpc(t, send, "eth_getBlockByNumber", `["0xa",false]`)
	require.Contains(t, string(client.Result), up.HashAt(10))
	require.Equal(t, int64(1), up.BlockCalls(10))

	// The cache write is asynchronous; let it land, then the store's fetch is free.
	time.Sleep(500 * time.Millisecond)
	raw, err := f.HeaderByNumber(t.Context(), 10)
	require.NoError(t, err)
	require.JSONEq(t, string(client.Result), string(raw))
	require.Equal(t, int64(1), up.BlockCalls(10), "the store's header fetch must be served from the response cache")
}

func pullTestConfig(up *scriptedEvmUpstream, ns string) *common.Config {
	return blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{
		Enabled: true, Depth: 16, Namespace: ns,
		PollInterval: common.Duration(100 * time.Millisecond),
		MaxStaleness: common.Duration(5 * time.Second),
	})
}

// No WebSocket subscriber and no client traffic: as blocks are produced the
// block store makes no header, body or logs call at all.
func TestHttp_BlockStore_IdleChainNoUpstreamFetches(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	_, _, _, shutdown, instance := createServerTestFixtures(pullTestConfig(up, fmt.Sprintf("idle-%d", time.Now().UnixNano())), t)
	defer shutdown()
	nw := instanceNetwork(t, instance)
	require.Eventually(t, func() bool { return nw.EvmHighestLatestBlockNumber(t.Context()) == 20 }, 10*time.Second, 20*time.Millisecond)
	before := up.HeaderCalls() + up.FullBlockCalls() + up.BlockHashLogCalls() + up.RangeLogCalls()
	for i := 0; i < 5; i++ {
		up.Mine(1)
		time.Sleep(300 * time.Millisecond)
	}
	require.Eventually(t, func() bool { return nw.EvmHighestLatestBlockNumber(t.Context()) == 25 }, 5*time.Second, 20*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, before, up.HeaderCalls()+up.FullBlockCalls()+up.BlockHashLogCalls()+up.RangeLogCalls(),
		"no explicit-number block or logs fetch without subscribers or clients")
	require.False(t, blockStoreOf(t, instance).Fresh())
}

// A client's full block is adopted; the same block is then served from the
// store on this replica and on another replica sharing Redis, with no
// upstream call. Hash-only reads are rendered from the adopted header.
func TestHttp_BlockStore_ClientBlockAdoptedAndSharedAcrossReplicas(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	ns := fmt.Sprintf("adopt-%d", time.Now().UnixNano())
	sendA, _, _, shutdownA, eA := createServerTestFixtures(pullTestConfig(up, ns), t)
	defer shutdownA()
	sendB, _, _, shutdownB, eB := createServerTestFixtures(pullTestConfig(up, ns), t)
	defer shutdownB()
	for _, e := range []*ERPC{eA, eB} {
		nw := instanceNetwork(t, e)
		require.Eventually(t, func() bool { return nw.EvmHighestLatestBlockNumber(t.Context()) == 20 }, 10*time.Second, 20*time.Millisecond)
	}
	ca, cb := blockStoreOf(t, eA), blockStoreOf(t, eB)

	first := doRpc(t, sendA, "eth_getBlockByNumber", `["0x14",true]`)
	require.Contains(t, string(first.Result), up.HashAt(20))
	require.Equal(t, int64(1), up.FullBlockCalls())
	headers := up.HeaderCalls()
	require.Eventually(t, func() bool {
		_, ok := ca.BlockByNumber(t.Context(), 20, true)
		return ok
	}, 5*time.Second, 20*time.Millisecond, "the response is adopted (header and body)")

	hitsA, hitsB := ca.Stats.Hits.Load(), cb.Stats.Hits.Load()
	again := doRpc(t, sendA, "eth_getBlockByNumber", `["0x14",true]`)
	require.JSONEq(t, string(first.Result), string(again.Result))
	other := doRpc(t, sendB, "eth_getBlockByNumber", `["0x14",true]`)
	require.JSONEq(t, string(first.Result), string(other.Result))
	byHash := doRpc(t, sendB, "eth_getBlockByHash", fmt.Sprintf(`[%q,false]`, up.HashAt(20)))
	require.Contains(t, string(byHash.Result), up.HashAt(20))
	require.NotContains(t, string(byHash.Result), `"from"`)
	require.Equal(t, int64(1), up.FullBlockCalls(), "same and other replica are served from the store")
	require.Equal(t, headers, up.HeaderCalls(), "no header fetch either")
	require.Zero(t, up.Calls("eth_getBlockByHash"))
	require.Greater(t, ca.Stats.Hits.Load(), hitsA)
	require.GreaterOrEqual(t, cb.Stats.Hits.Load(), hitsB+2)
	require.Zero(t, cb.Stats.Hydrated.Load(), "replica B fetched nothing")
}

// A client's unfiltered getLogs for a block is adopted (with its header,
// fetched once on demand); filtered reads of that block are then local.
func TestHttp_BlockStore_UnfilteredLogsAdoptedFilteredServedLocally(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	send, _, _, shutdown, instance := createServerTestFixtures(pullTestConfig(up, fmt.Sprintf("logs-%d", time.Now().UnixNano())), t)
	defer shutdown()
	nw := instanceNetwork(t, instance)
	require.Eventually(t, func() bool { return nw.EvmHighestLatestBlockNumber(t.Context()) == 20 }, 10*time.Second, 20*time.Millisecond)
	hc := blockStoreOf(t, instance)

	all := doRpc(t, send, "eth_getLogs", `[{"fromBlock":"0x13","toBlock":"0x14"}]`)
	require.Equal(t, int64(1), up.RangeLogCalls())
	require.Eventually(t, func() bool {
		_, ok := hc.LogsRangeCached(t.Context(), 19, 20, nil)
		return ok
	}, 5*time.Second, 20*time.Millisecond, "the unfiltered result is adopted per block")

	upstream := func() int64 {
		return up.RangeLogCalls() + up.BlockHashLogCalls() + up.HeaderCalls() + up.FullBlockCalls()
	}
	before := upstream()
	even := doRpc(t, send, "eth_getLogs", fmt.Sprintf(`[{"fromBlock":"0x13","toBlock":"0x14","topics":[%q]}]`, scriptedTopicEven))
	var evenLogs []map[string]interface{}
	require.NoError(t, json.Unmarshal(even.Result, &evenLogs))
	require.Len(t, evenLogs, 1)
	require.Contains(t, string(all.Result), up.HashAt(20))
	byHash := doRpc(t, send, "eth_getLogs", fmt.Sprintf(`[{"blockHash":%q,"address":%q}]`, up.HashAt(19), scriptedEmitter))
	require.Contains(t, string(byHash.Result), up.HashAt(19))
	require.Equal(t, before, upstream(), "filtered reads of adopted logs make no upstream call")
}

// Following runs only while a WebSocket subscriber exists anywhere in the
// fleet: a subscriber on one replica starts it (one header per new block,
// even if the other replica leads), and once the last one leaves and the
// shared presence mark expires, following stops.
func TestHttp_BlockStore_FollowingOnlyWhileSubscribed(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	ns := fmt.Sprintf("follow-%d", time.Now().UnixNano())
	_, _, _, shutdownA, eA := createServerTestFixtures(pullTestConfig(up, ns), t)
	defer shutdownA()
	_, _, baseB, shutdownB, eB := createServerTestFixtures(pullTestConfig(up, ns), t)
	defer shutdownB()
	for _, e := range []*ERPC{eA, eB} {
		nw := instanceNetwork(t, e)
		require.Eventually(t, func() bool { return nw.EvmHighestLatestBlockNumber(t.Context()) == 20 }, 10*time.Second, 20*time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	require.Zero(t, up.HeaderCalls(), "no following before any subscriber")

	w, _, err := dialWs(t, wsURL(baseB, ""), nil)
	require.NoError(t, err)
	r := w.call("eth_subscribe", `["newHeads"]`)
	require.Nil(t, r.Error)
	var id string
	require.NoError(t, json.Unmarshal(r.Result, &id))
	cb := blockStoreOf(t, eB)
	require.Eventually(t, func() bool { return cb.Head() == 20 }, 10*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool { return up.HeaderCalls() >= 16 }, 10*time.Second, 20*time.Millisecond)
	cold := up.HeaderCalls()
	for n := 21; n <= 23; n++ {
		up.Mine(1)
		require.Contains(t, string(w.next(id)), fmt.Sprintf(`"number":"0x%x"`, n))
	}
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, cold+3, up.HeaderCalls(), "one header per new block while subscribed")

	require.Equal(t, "true", string(w.call("eth_unsubscribe", fmt.Sprintf(`[%q]`, id)).Result))
	// The presence mark (lease TTL) expires; then following stops fleet-wide.
	time.Sleep(3 * time.Second)
	stopped := up.HeaderCalls()
	for i := 0; i < 3; i++ {
		up.Mine(1)
		time.Sleep(300 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, stopped, up.HeaderCalls(), "no header fetches after the last subscriber left")
}
