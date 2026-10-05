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

// The scripted upstream finalizes tip-64, so with tip 120 heights <= 56 are
// finalized and heights above 104 are inside a depth-16 live window.
const upstreamCostTip = 120

type upstreamCostFixture struct {
	up      *scriptedEvmUpstream
	send    func(string, map[string]string, map[string]string) (int, map[string]string, string)
	network *Network
}

// newUpstreamCostFixture runs the production-shaped configuration: live
// window, historical cache and logs fill all on (or the block store fully off
// when hc is nil).
func newUpstreamCostFixture(t *testing.T, hc *common.EvmBlockStoreConfig) *upstreamCostFixture {
	t.Helper()
	up := newScriptedEvmUpstream(123, upstreamCostTip)
	t.Cleanup(up.Close)
	cfg := blockStoreTestConfig(up.URL(), hc)
	if hc != nil && !hc.Enabled && hc.Historical.Enabled {
		if hc.ConnectorId == "" {
			hc.ConnectorId = "blockstore-redis"
		}
		cfg.Database = &common.DatabaseConfig{EvmJsonRpcCache: &common.CacheConfig{Connectors: []*common.ConnectorConfig{{
			Id: hc.ConnectorId, Driver: common.DriverRedis,
			Redis: &common.RedisConnectorConfig{URI: "redis://" + blockStoreTestRedis()},
		}}}}
	}
	send, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
	t.Cleanup(shutdown)
	nw := instanceNetwork(t, instance)
	require.Eventually(t, func() bool {
		return nw.EvmHighestLatestBlockNumber(t.Context()) >= upstreamCostTip &&
			nw.EvmHighestFinalizedBlockNumber(t.Context()) >= upstreamCostTip-64
	}, 10*time.Second, 20*time.Millisecond, "the network must know its head and finalized height")
	return &upstreamCostFixture{up: up, send: send, network: nw}
}

func fullBlockStoreConfig() *common.EvmBlockStoreConfig {
	return &common.EvmBlockStoreConfig{
		Enabled:      true,
		Namespace:    fmt.Sprintf("cost-%d", time.Now().UnixNano()),
		Depth:        16,
		PollInterval: common.Duration(100 * time.Millisecond),
		MaxStaleness: common.Duration(5 * time.Second),
		Historical:   common.EvmBlockStoreHistoricalConfig{Enabled: true},
		LogsFill:     common.EvmBlockStoreLogsFillConfig{Enabled: true, UnfinalizedTTL: common.Duration(time.Minute)},
	}
}

// upstreamWork counts every upstream request a client or the block store can
// cause (state-poller tag reads excluded: they run on a timer either way).
func (f *upstreamCostFixture) upstreamWork() int64 {
	return f.up.Calls("eth_getLogs") + f.up.HeaderCalls() + f.up.FullBlockCalls() + f.up.Calls("eth_getBlockByHash")
}

// settle lets asynchronous adoption (and, before this fix, warming) finish.
func settle() { time.Sleep(700 * time.Millisecond) }

func logsParams(from, to int64, extra string) string {
	return fmt.Sprintf(`[{"fromBlock":"0x%x","toBlock":"0x%x"%s}]`, from, to, extra)
}

func decodeLogs(t *testing.T, r rpcResp) []map[string]interface{} {
	t.Helper()
	var logs []map[string]interface{}
	require.NoError(t, json.Unmarshal(r.Result, &logs))
	return logs
}

// A filtered client eth_getLogs over [n..n+5] costs exactly one upstream
// eth_getLogs (the logs fill's unfiltered range call), no blockHash logs and
// no headers. Another filter over the same range costs nothing upstream.
// Holds at finalized heights (historical) and near the tip (live window).
func TestHttp_BlockStoreCost_FilteredLogsRangeIsOneUnfilteredCall(t *testing.T) {
	for _, from := range []int64{30, 108} {
		t.Run(fmt.Sprintf("from=%d", from), func(t *testing.T) {
			f := newUpstreamCostFixture(t, fullBlockStoreConfig())
			to := from + 5
			first := doRpc(t, f.send, "eth_getLogs", logsParams(from, to, fmt.Sprintf(`,"address":%q`, scriptedEmitter)))
			require.Len(t, decodeLogs(t, first), 6)
			settle()
			require.Equal(t, int64(1), f.up.Calls("eth_getLogs"), "one upstream eth_getLogs in total")
			require.Equal(t, int64(1), f.up.UnfilteredLogCalls(), "and it is the unfiltered fill")
			require.Zero(t, f.up.BlockHashLogCalls(), "no per-block logs")
			require.Zero(t, f.up.HeaderCalls(), "no header fetches")
			require.Zero(t, f.up.FullBlockCalls())

			before := f.upstreamWork()
			other := doRpc(t, f.send, "eth_getLogs", logsParams(from, to, fmt.Sprintf(`,"address":%q`, "0x0000000000000000000000000000000000000001")))
			require.Empty(t, decodeLogs(t, other))
			even := doRpc(t, f.send, "eth_getLogs", logsParams(from, to, fmt.Sprintf(`,"topics":[%q]`, scriptedTopicEven)))
			require.Len(t, decodeLogs(t, even), 3)
			settle()
			require.Equal(t, before, f.upstreamWork(), "other filters over the same range are served locally")
		})
	}
}

// A range wider than the logs fill but inside the live window passes through
// as the client's own single request: no per-block logs, no headers.
func TestHttp_BlockStoreCost_WideLogsRangePassesThrough(t *testing.T) {
	f := newUpstreamCostFixture(t, fullBlockStoreConfig())
	r := doRpc(t, f.send, "eth_getLogs", logsParams(105, 118, fmt.Sprintf(`,"topics":[%q]`, scriptedTopicEven)))
	require.Len(t, decodeLogs(t, r), 7)
	settle()
	require.Equal(t, int64(1), f.up.Calls("eth_getLogs"))
	require.Zero(t, f.up.BlockHashLogCalls())
	require.Zero(t, f.up.HeaderCalls())
}

// Without the logs fill, a filtered historical range is the client's own
// call only: the historical cache never warms it per block.
func TestHttp_BlockStoreCost_HistoricalNeverWarmsLogs(t *testing.T) {
	hc := fullBlockStoreConfig()
	hc.LogsFill.Enabled = false
	f := newUpstreamCostFixture(t, hc)
	doRpc(t, f.send, "eth_getLogs", logsParams(30, 35, fmt.Sprintf(`,"address":%q`, scriptedEmitter)))
	settle()
	require.Equal(t, int64(1), f.up.Calls("eth_getLogs"))
	require.Zero(t, f.up.BlockHashLogCalls())
	require.Zero(t, f.up.HeaderCalls())
}

// eth_getBlockByNumber(n,false) at a finalized height costs the client's one
// call and nothing more (no full-block warm, no header recheck).
func TestHttp_BlockStoreCost_HeaderAtFinalizedHeightNoWarm(t *testing.T) {
	f := newUpstreamCostFixture(t, fullBlockStoreConfig())
	r := doRpc(t, f.send, "eth_getBlockByNumber", `["0x28",false]`)
	require.Contains(t, string(r.Result), f.up.HashAt(40))
	settle()
	require.Equal(t, int64(1), f.upstreamWork(), "only the client's own request")
	require.Equal(t, int64(1), f.up.HeaderCalls())
	require.Zero(t, f.up.FullBlockCalls())
}

// Full blocks at gapped heights (finalized and near the tip) are adopted with
// no "link" header fetch, and a repeat of each is served from the store.
func TestHttp_BlockStoreCost_GappedFullBlocksServedWithoutLinkFetches(t *testing.T) {
	f := newUpstreamCostFixture(t, fullBlockStoreConfig())
	heights := []int64{30, 33, 37, 110, 113, 117}
	first := map[int64]string{}
	for _, n := range heights {
		first[n] = string(doRpc(t, f.send, "eth_getBlockByNumber", fmt.Sprintf(`["0x%x",true]`, n)).Result)
	}
	settle()
	require.Equal(t, int64(len(heights)), f.up.FullBlockCalls())
	require.Zero(t, f.up.HeaderCalls(), "no link or recheck header fetches")
	for _, n := range heights {
		again := doRpc(t, f.send, "eth_getBlockByNumber", fmt.Sprintf(`["0x%x",true]`, n))
		require.JSONEq(t, first[n], string(again.Result))
	}
	require.Equal(t, int64(len(heights)), f.up.FullBlockCalls(), "repeats are served from the store")
	require.Zero(t, f.up.HeaderCalls())
	require.Zero(t, f.up.Calls("eth_getBlockByHash"))
}

// An unfiltered eth_getLogs for heights whose headers are unknown fetches no
// header; later filtered reads of those heights are served locally.
func TestHttp_BlockStoreCost_UnfilteredLogsUnknownHeadersNoFetch(t *testing.T) {
	f := newUpstreamCostFixture(t, fullBlockStoreConfig())
	all := doRpc(t, f.send, "eth_getLogs", logsParams(110, 115, ""))
	require.Len(t, decodeLogs(t, all), 6)
	settle()
	require.Zero(t, f.up.HeaderCalls(), "no header fetched to adopt the logs")
	require.Equal(t, int64(1), f.up.Calls("eth_getLogs"))
	before := f.upstreamWork()
	odd := doRpc(t, f.send, "eth_getLogs", logsParams(110, 115, fmt.Sprintf(`,"topics":[%q]`, scriptedTopicOdd)))
	require.Len(t, decodeLogs(t, odd), 3)
	settle()
	require.Equal(t, before, f.upstreamWork())
}

// A realistic mixed client workload never costs more upstream requests with
// the block store on than with it off.
func TestHttp_BlockStoreCost_MixedWorkloadNeverExceedsStoreOff(t *testing.T) {
	workload := func(t *testing.T, f *upstreamCostFixture) {
		for round := 0; round < 2; round++ {
			doRpc(t, f.send, "eth_blockNumber", `[]`)
			doRpc(t, f.send, "eth_getBlockByNumber", `["latest",false]`)
			for _, n := range []int64{30, 33, 40, 110, 117} {
				doRpc(t, f.send, "eth_getBlockByNumber", fmt.Sprintf(`["0x%x",true]`, n))
				doRpc(t, f.send, "eth_getBlockByNumber", fmt.Sprintf(`["0x%x",false]`, n+1))
			}
			doRpc(t, f.send, "eth_getLogs", logsParams(30, 35, fmt.Sprintf(`,"address":%q`, scriptedEmitter)))
			doRpc(t, f.send, "eth_getLogs", logsParams(30, 35, fmt.Sprintf(`,"topics":[%q]`, scriptedTopicEven)))
			doRpc(t, f.send, "eth_getLogs", logsParams(108, 112, ""))
			doRpc(t, f.send, "eth_getLogs", logsParams(108, 112, fmt.Sprintf(`,"topics":[%q]`, scriptedTopicOdd)))
			doRpc(t, f.send, "eth_getLogs", logsParams(105, 118, fmt.Sprintf(`,"address":%q`, scriptedEmitter)))
			doRpc(t, f.send, "eth_getLogs", logsParams(40, 70, ""))
			settle()
		}
	}
	off := newUpstreamCostFixture(t, nil)
	workload(t, off)
	on := newUpstreamCostFixture(t, fullBlockStoreConfig())
	workload(t, on)
	t.Logf("upstream work: store off=%d on=%d (getLogs %d/%d, headers %d/%d, full blocks %d/%d)",
		off.upstreamWork(), on.upstreamWork(), off.up.Calls("eth_getLogs"), on.up.Calls("eth_getLogs"),
		off.up.HeaderCalls(), on.up.HeaderCalls(), off.up.FullBlockCalls(), on.up.FullBlockCalls())
	require.LessOrEqual(t, on.upstreamWork(), off.upstreamWork())
	// Per kind too: savings on one kind must not hide waste on another.
	require.LessOrEqual(t, on.up.Calls("eth_getLogs"), off.up.Calls("eth_getLogs"))
	require.LessOrEqual(t, on.up.HeaderCalls(), off.up.HeaderCalls(), "no extra header fetches")
	require.LessOrEqual(t, on.up.FullBlockCalls(), off.up.FullBlockCalls(), "no extra full-block fetches")
	require.Zero(t, on.up.BlockHashLogCalls())
}
