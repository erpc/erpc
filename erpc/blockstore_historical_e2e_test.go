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

func TestHttp_BlockStoreHistorical_IndependentPayloadsAndRangeReuse(t *testing.T) {
	up := newScriptedEvmUpstream(123, 120)
	defer up.Close()
	from := int64(24 + time.Now().UnixNano()%24)
	if from%2 != 0 {
		from++
	}
	to := from + 2
	hc := &common.EvmBlockStoreConfig{
		ConnectorId:  "blockstore-redis",
		Namespace:    fmt.Sprintf("historical-%d", time.Now().UnixNano()),
		Depth:        8,
		MaxLogsRange: 8,
		MaxPerTick:   4,
		PollInterval: common.Duration(100 * time.Millisecond),
		MaxStaleness: common.Duration(5 * time.Second),
		Historical:   common.EvmBlockStoreHistoricalConfig{Enabled: true},
	}
	cfg := blockStoreTestConfig(up.URL(), hc)
	cfg.Database = &common.DatabaseConfig{EvmJsonRpcCache: &common.CacheConfig{Connectors: []*common.ConnectorConfig{{
		Id: hc.ConnectorId, Driver: common.DriverRedis,
		Redis: &common.RedisConnectorConfig{URI: "redis://" + blockStoreTestRedis()},
	}}}}
	send, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
	defer shutdown()

	project, err := instance.GetProject("test_project")
	require.NoError(t, err)
	network, err := project.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	require.Nil(t, network.BlockStore())
	require.NotNil(t, network.historicalBlockStore)
	require.Eventually(t, func() bool {
		return network.EvmHighestFinalizedBlockNumber(t.Context()) >= to
	}, 10*time.Second, 50*time.Millisecond, "the network must have a corroborated finalized height")

	// A successful full block response warms only the block payload. A later
	// by-hash request reaches the historical path, not the ordinary request cache.
	fullBefore := up.FullBlockCalls()
	full := doRpc(t, send, "eth_getBlockByNumber", fmt.Sprintf(`[%q,true]`, fmt.Sprintf("0x%x", from)))
	require.Contains(t, string(full.Result), `"from"`)
	require.Eventually(t, func() bool {
		_, hit := network.historicalBlockStore.ReadBlockByNumber(t.Context(), from)
		return hit
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, fullBefore+1, up.FullBlockCalls())
	byHash := doRpc(t, send, "eth_getBlockByHash", fmt.Sprintf(`[%q,true]`, up.HashAt(from)))
	require.JSONEq(t, string(full.Result), string(byHash.Result))
	require.Equal(t, fullBefore+1, up.FullBlockCalls(), "historical by-hash block read must not refetch a full block")
	byHashCalls := up.Calls("eth_getBlockByHash")
	header := doRpc(t, send, "eth_getBlockByHash", fmt.Sprintf(`[%q,false]`, up.HashAt(from)))
	var headerBlock struct {
		Uncles       []string          `json:"uncles"`
		Transactions []json.RawMessage `json:"transactions"`
	}
	require.NoError(t, json.Unmarshal(header.Result, &headerBlock))
	require.NotNil(t, headerBlock.Uncles, "eth_getBlockByHash(false) retains standard block fields")
	require.Len(t, headerBlock.Transactions, 1)
	var txHash string
	require.NoError(t, json.Unmarshal(headerBlock.Transactions[0], &txHash))
	require.Equal(t, scriptedTx(from, "a"), txHash)
	require.Equal(t, byHashCalls, up.Calls("eth_getBlockByHash"), "historical hash-only block read must not reach upstream")
	require.Zero(t, up.BlockHashLogCalls(), "block warming must not fetch logs")

	// A filtered range warms logs by block hash using header+unfiltered-log
	// requests, never full blocks. Another filter over the same range reuses all
	// per-block historical log payloads without an upstream range call.
	rangeCalls := up.RangeLogCalls()
	fullBeforeLogs := up.FullBlockCalls()
	blockHashLogsBefore := up.BlockHashLogCalls()
	fromHex, toHex := fmt.Sprintf("0x%x", from), fmt.Sprintf("0x%x", to)
	rangeResponse := doRpc(t, send, "eth_getLogs", fmt.Sprintf(`[{"fromBlock":%q,"toBlock":%q,"address":%q}]`, fromHex, toHex, scriptedEmitter))
	var rangeResponseLogs []map[string]interface{}
	require.NoError(t, json.Unmarshal(rangeResponse.Result, &rangeResponseLogs))
	require.Len(t, rangeResponseLogs, 3, fmt.Sprintf("%s, range calls: %d -> %d", rangeResponse.Result, rangeCalls, up.RangeLogCalls()))
	require.Eventually(t, func() bool {
		_, hit := network.historicalBlockStore.ReadLogsRange(t.Context(), from, to)
		return hit
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, fullBeforeLogs, up.FullBlockCalls(), "logs warming must not request full blocks")
	require.Equal(t, blockHashLogsBefore+3, up.BlockHashLogCalls(), "logs must be warmed independently for each block")

	even := doRpc(t, send, "eth_getLogs", fmt.Sprintf(`[{"fromBlock":%q,"toBlock":%q,"topics":[%q]}]`, fromHex, toHex, scriptedTopicEven))
	var evenLogs []map[string]interface{}
	require.NoError(t, json.Unmarshal(even.Result, &evenLogs))
	require.Len(t, evenLogs, 2)
	require.Equal(t, rangeCalls+1, up.RangeLogCalls(), "a different filter over the same range must reuse historical logs")

	// A range with an unavailable edge is a full miss. It falls through to the
	// original range query rather than returning the cached middle blocks.
	missBefore := up.RangeLogCalls()
	wide := doRpc(t, send, "eth_getLogs", fmt.Sprintf(`[{"fromBlock":%q,"toBlock":%q}]`, fmt.Sprintf("0x%x", from-1), fmt.Sprintf("0x%x", to+1)))
	var wideLogs []map[string]interface{}
	require.NoError(t, json.Unmarshal(wide.Result, &wideLogs))
	require.Len(t, wideLogs, 5)
	require.Equal(t, missBefore+1, up.RangeLogCalls())
}
