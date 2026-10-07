package erpc

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/blockstore"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

type logsFillFixture struct {
	up      *scriptedEvmUpstream
	send    func(string, map[string]string, map[string]string) (int, map[string]string, string)
	network *Network
}

// newLogsFillFixture runs a server whose blockStore has ONLY logsFill on:
// live window and historical cache disabled, no websocket. withRedis selects
// the shared connector store; otherwise the in-memory store is used.
func newLogsFillFixture(t *testing.T, tip int64, withRedis bool) *logsFillFixture {
	t.Helper()
	up := newScriptedEvmUpstream(123, tip)
	t.Cleanup(up.Close)
	hc := &common.EvmBlockStoreConfig{
		Namespace: fmt.Sprintf("logsfill-%d", time.Now().UnixNano()),
		LogsFill:  common.EvmBlockStoreLogsFillConfig{Enabled: true, UnfinalizedTTL: common.Duration(time.Minute)},
	}
	cfg := blockStoreTestConfig(up.URL(), hc)
	cfg.Server.WebSocket = nil
	if withRedis {
		hc.ConnectorId = "logsfill-redis"
		cfg.Database = &common.DatabaseConfig{EvmJsonRpcCache: &common.CacheConfig{Connectors: []*common.ConnectorConfig{{
			Id: hc.ConnectorId, Driver: common.DriverRedis,
			Redis: &common.RedisConnectorConfig{URI: "redis://" + blockStoreTestRedis()},
		}}}}
	}
	send, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
	t.Cleanup(shutdown)
	project, err := instance.GetProject("test_project")
	require.NoError(t, err)
	network, err := project.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	require.Nil(t, network.BlockStore(), "live window stays disabled")
	require.Nil(t, network.historicalBlockStore, "historical cache stays disabled")
	require.NotNil(t, network.logsFiller)
	require.Eventually(t, func() bool {
		return network.EvmHighestLatestBlockNumber(t.Context()) >= tip
	}, 10*time.Second, 20*time.Millisecond, "the network must know its head")
	return &logsFillFixture{up: up, send: send, network: network}
}

func (f *logsFillFixture) getLogs(t *testing.T, from, to int64, extra string) []map[string]interface{} {
	t.Helper()
	params := fmt.Sprintf(`[{"fromBlock":"0x%x","toBlock":"0x%x"%s}]`, from, to, extra)
	r := doRpc(t, f.send, "eth_getLogs", params)
	require.NotEqual(t, "null", strings.TrimSpace(string(r.Result)), "empty results must be [] not null")
	var logs []map[string]interface{}
	require.NoError(t, json.Unmarshal(r.Result, &logs))
	return logs
}

func logsFillCounter(outcome, reason string) float64 {
	return testutil.ToFloat64(telemetry.MetricBlockStoreLogsFillTotal.WithLabelValues("test_project", "evm:123", outcome, reason))
}

func TestHttp_LogsFill_DifferentFiltersShareOneUpstreamCall(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withRedis bool
	}{{"memory store", false}, {"redis store", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLogsFillFixture(t, 120, tc.withRedis)
			f.up.SetEmptyLogs(101)
			hitsBefore := logsFillCounter("hit", "ok")
			fillsBefore := logsFillCounter("fill", "ok")
			rangeBefore := f.up.RangeLogCalls()

			all := f.getLogs(t, 100, 104, "")
			require.Len(t, all, 4, "101 has no logs")
			for i, want := range []string{"0x64", "0x66", "0x67", "0x68"} {
				require.Equal(t, want, all[i]["blockNumber"], "ordered by block")
			}
			require.EqualValues(t, 1, f.up.UnfilteredLogCalls())

			even := f.getLogs(t, 100, 104, fmt.Sprintf(`,"topics":[%q]`, scriptedTopicEven))
			require.Len(t, even, 3)
			byAddr := f.getLogs(t, 102, 103, fmt.Sprintf(`,"address":[%q]`, scriptedEmitter))
			require.Len(t, byAddr, 2, "a sub-range is served from the same per-block entries")
			none := f.getLogs(t, 101, 101, fmt.Sprintf(`,"address":%q`, "0x0000000000000000000000000000000000000001"))
			require.Empty(t, none)
			odd := f.getLogs(t, 100, 104, fmt.Sprintf(`,"topics":[[%q,%q]]`, scriptedTopicOdd, "0x"+strings.Repeat("9", 64)))
			require.Len(t, odd, 1)

			require.Equal(t, rangeBefore+1, f.up.RangeLogCalls(), "only the one unfiltered fill reached upstream")
			require.Equal(t, fillsBefore+1, logsFillCounter("fill", "ok"))
			require.Equal(t, hitsBefore+4, logsFillCounter("hit", "ok"))
		})
	}
}

// The fill's own upstream fetch is an internal hydration request, like the head
// fetcher's: `matchRequestKind: internal` failsafe policies must apply to it.
func TestHttp_LogsFill_UnfilteredFetchIsInternal(t *testing.T) {
	up := newScriptedEvmUpstream(123, 120)
	t.Cleanup(up.Close)
	hc := &common.EvmBlockStoreConfig{
		Namespace: fmt.Sprintf("logsfill-%d", time.Now().UnixNano()),
		LogsFill:  common.EvmBlockStoreLogsFillConfig{Enabled: true, UnfinalizedTTL: common.Duration(time.Minute)},
	}
	cfg := blockStoreTestConfig(up.URL(), hc)
	cfg.Server.WebSocket = nil
	// Only internal requests retry; user requests get a single attempt.
	cfg.Projects[0].Networks[0].Failsafe = []*common.FailsafeConfig{
		{MatchMethod: "*", MatchRequestKind: "internal", Retry: &common.RetryPolicyConfig{MaxAttempts: 3}},
		{MatchMethod: "*", MatchRequestKind: "user", Retry: &common.RetryPolicyConfig{MaxAttempts: 1}},
	}
	send, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
	t.Cleanup(shutdown)
	project, err := instance.GetProject("test_project")
	require.NoError(t, err)
	network, err := project.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return network.EvmHighestLatestBlockNumber(t.Context()) >= 120 }, 10*time.Second, 20*time.Millisecond)

	up.failUnfilteredLogs.Store(true)
	before := up.UnfilteredLogCalls()
	_, _, _ = send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x46","toBlock":"0x48","address":%q}]}`, scriptedEmitter), nil, nil)
	require.Equal(t, before+3, up.UnfilteredLogCalls(),
		"the unfiltered fill fetch must be retried by the internal-kind policy (3 attempts), not treated as user traffic")
}

// The fill's unfiltered fetch and a byte-identical client request must not share
// one in-flight upstream response through the multiplexer: the internal fetch
// runs under internal failsafe/integrity rules the client did not ask for.
func TestHttp_LogsFill_InternalFetchNotMultiplexedWithClients(t *testing.T) {
	f := newLogsFillFixture(t, 120, false)
	f.up.unfilteredLogDelay.Store(int64(400 * time.Millisecond))
	defer f.up.unfilteredLogDelay.Store(0)
	before := f.up.UnfilteredLogCalls()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Triggers the fill: one internal unfiltered call for 0x50..0x52.
		f.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x50","toBlock":"0x52","address":%q}]}`, scriptedEmitter), nil, nil)
	}()
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond) // while the internal fetch is in flight
		// Same body as the internal fetch (unfiltered, same range). A client
		// request skips the fill (no filter is fine), so it goes upstream itself.
		hdr := map[string]string{"X-ERPC-Skip-Cache-Read": "true"}
		f.send(`{"jsonrpc":"2.0","id":2,"method":"eth_getLogs","params":[{"fromBlock":"0x50","toBlock":"0x52"}]}`, hdr, nil)
	}()
	wg.Wait()
	require.Equal(t, before+2, f.up.UnfilteredLogCalls(),
		"internal fetch and client request each reach upstream instead of sharing one response")
}

func TestHttp_LogsFill_SkipsAndFallbacks(t *testing.T) {
	f := newLogsFillFixture(t, 120, false)

	t.Run("any skipCacheRead value bypasses the fill, including connector patterns", func(t *testing.T) {
		addr := fmt.Sprintf(`,"address":%q`, scriptedEmitter)
		f.getLogs(t, 60, 62, addr) // warm the fill for this range
		for _, skip := range []string{"true", "*", "logsfill-*"} {
			req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x3c","toBlock":"0x3e"%s}]}`, addr)))
			req.SetDirectives(&common.RequestDirectives{SkipCacheRead: skip})
			skipped := logsFillCounter("skipped", "directive")
			resp, ok := f.network.tryServeLogsFill(t.Context(), req)
			require.False(t, ok, "skipCacheRead=%q must not be answered from the fill", skip)
			require.Nil(t, resp)
			require.Equal(t, skipped+1, logsFillCounter("skipped", "directive"), skip)
		}
		// An explicit "false" means do not skip, as in ShouldSkipCacheRead: still served.
		for _, noSkip := range []string{"false", "FALSE"} {
			req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x3c","toBlock":"0x3e"%s}]}`, addr)))
			req.SetDirectives(&common.RequestDirectives{SkipCacheRead: noSkip})
			skipped := logsFillCounter("skipped", "directive")
			resp, ok := f.network.tryServeLogsFill(t.Context(), req)
			require.True(t, ok, "skipCacheRead=%q must still be answered from the fill", noSkip)
			require.NotNil(t, resp)
			resp.Release()
			require.Equal(t, skipped, logsFillCounter("skipped", "directive"), noSkip)
		}
	})

	t.Run("above head goes upstream unchanged", func(t *testing.T) {
		unfiltered := f.up.UnfilteredLogCalls()
		skipped := logsFillCounter("skipped", "above_head")
		// The normal path may answer or reject a range past the head (integrity
		// range enforcement); either way the fill must not touch it.
		f.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x76","toBlock":"0x7d","address":%q}]}`, scriptedEmitter), nil, nil)
		require.Equal(t, unfiltered, f.up.UnfilteredLogCalls(), "no fill above head")
		require.Equal(t, skipped+1, logsFillCounter("skipped", "above_head"))
	})

	t.Run("range above maxRange goes upstream unchanged", func(t *testing.T) {
		unfiltered := f.up.UnfilteredLogCalls()
		skipped := logsFillCounter("skipped", "range_too_large")
		logs := f.getLogs(t, 10, 20, fmt.Sprintf(`,"address":%q`, scriptedEmitter))
		require.Len(t, logs, 11)
		require.Equal(t, unfiltered, f.up.UnfilteredLogCalls())
		require.Equal(t, skipped+1, logsFillCounter("skipped", "range_too_large"))
	})

	t.Run("tags and blockHash go upstream unchanged", func(t *testing.T) {
		unfiltered := f.up.UnfilteredLogCalls()
		doRpc(t, f.send, "eth_getLogs", fmt.Sprintf(`[{"fromBlock":"latest","toBlock":"latest","address":%q}]`, scriptedEmitter))
		doRpc(t, f.send, "eth_getLogs", fmt.Sprintf(`[{"blockHash":%q}]`, f.up.HashAt(50)))
		require.Equal(t, unfiltered, f.up.UnfilteredLogCalls())
	})

	t.Run("unfiltered fetch error falls back to the original request", func(t *testing.T) {
		f.up.failUnfilteredLogs.Store(true)
		defer f.up.failUnfilteredLogs.Store(false)
		fallback := logsFillCounter("fallback", "fetch_error")
		logs := f.getLogs(t, 30, 32, fmt.Sprintf(`,"topics":[%q]`, scriptedTopicEven))
		require.Len(t, logs, 2, "the client's filtered request still succeeds")
		require.Equal(t, fallback+1, logsFillCounter("fallback", "fetch_error"))
	})

	t.Run("removed logs are never cached", func(t *testing.T) {
		f.up.removedUnfiltered.Store(true)
		fallback := logsFillCounter("fallback", "removed_logs")
		addr := fmt.Sprintf(`,"address":%q`, scriptedEmitter)
		logs := f.getLogs(t, 40, 41, addr)
		require.Len(t, logs, 2)
		for _, l := range logs {
			require.Equal(t, false, l["removed"], "the client sees the upstream's answer to its own request")
		}
		require.Equal(t, fallback+1, logsFillCounter("fallback", "removed_logs"))
		f.up.removedUnfiltered.Store(false)
		calls := f.up.UnfilteredLogCalls()
		f.getLogs(t, 40, 41, addr)
		require.Equal(t, calls+1, f.up.UnfilteredLogCalls(), "nothing was stored for the removed range")
	})
}

func TestHttp_LogsFill_ConcurrentRequestsCoalesce(t *testing.T) {
	f := newLogsFillFixture(t, 120, false)
	f.up.unfilteredLogDelay.Store(int64(300 * time.Millisecond))
	before := f.up.UnfilteredLogCalls()
	filters := []string{"", fmt.Sprintf(`,"address":%q`, scriptedEmitter), fmt.Sprintf(`,"topics":[%q]`, scriptedTopicOdd)}
	const n = 12
	var wg sync.WaitGroup
	counts := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			counts[i] = len(f.getLogs(t, 60, 65, filters[i%len(filters)]))
		}(i)
	}
	wg.Wait()
	require.Equal(t, before+1, f.up.UnfilteredLogCalls(), "N concurrent requests over one range make one upstream call")
	for i, c := range counts {
		want := 6
		if i%len(filters) == 2 {
			want = 3
		}
		require.Equal(t, want, c, "request %d", i)
	}
}

// Two eRPC instances ("replicas") with the same config share one Redis and one
// upstream. Concurrent misses for one range on both make exactly one
// unfiltered upstream call: the fill lock holder fetches, the other replica
// waits for it and serves the stored entries.
func TestHttp_LogsFill_ReplicasShareOneUpstreamCallViaRedis(t *testing.T) {
	up := newScriptedEvmUpstream(123, 120)
	t.Cleanup(up.Close)
	up.unfilteredLogDelay.Store(int64(300 * time.Millisecond))
	ns := fmt.Sprintf("logsfill-peer-%d", time.Now().UnixNano())
	newReplica := func() *logsFillFixture {
		hc := &common.EvmBlockStoreConfig{
			Namespace:   ns,
			ConnectorId: "logsfill-redis",
			LogsFill:    common.EvmBlockStoreLogsFillConfig{Enabled: true, UnfinalizedTTL: common.Duration(time.Minute)},
		}
		cfg := blockStoreTestConfig(up.URL(), hc)
		cfg.Server.WebSocket = nil
		cfg.Database = &common.DatabaseConfig{EvmJsonRpcCache: &common.CacheConfig{Connectors: []*common.ConnectorConfig{{
			Id: hc.ConnectorId, Driver: common.DriverRedis,
			Redis: &common.RedisConnectorConfig{URI: "redis://" + blockStoreTestRedis()},
		}}}}
		send, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
		t.Cleanup(shutdown)
		project, err := instance.GetProject("test_project")
		require.NoError(t, err)
		network, err := project.GetNetwork(t.Context(), "evm:123")
		require.NoError(t, err)
		require.NotNil(t, network.logsFiller)
		require.Eventually(t, func() bool {
			return network.EvmHighestLatestBlockNumber(t.Context()) >= 120
		}, 10*time.Second, 20*time.Millisecond)
		return &logsFillFixture{up: up, send: send, network: network}
	}
	replicas := []*logsFillFixture{newReplica(), newReplica()}

	before := up.UnfilteredLogCalls()
	peerHits := logsFillCounter("hit", blockstore.LogsFillReasonPeerFill)
	filters := []string{"", fmt.Sprintf(`,"address":%q`, scriptedEmitter), fmt.Sprintf(`,"topics":[%q]`, scriptedTopicOdd)}
	const n = 12
	var wg sync.WaitGroup
	counts := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			counts[i] = len(replicas[i%2].getLogs(t, 70, 75, filters[i%len(filters)]))
		}(i)
	}
	wg.Wait()
	require.Equal(t, before+1, up.UnfilteredLogCalls(), "one upstream call across both replicas")
	for i, c := range counts {
		want := 6
		if i%len(filters) == 2 {
			want = 3
		}
		require.Equal(t, want, c, "request %d", i)
	}
	require.Greater(t, logsFillCounter("hit", blockstore.LogsFillReasonPeerFill), peerHits, "the waiting replica served the peer's fill")
}

// With both the live window and logsFill on, they share one per-block logs
// store: a small range that misses the window is filled by logsFill's one
// unfiltered range call, whose per-height lists the window adopts as its
// logs, so the window, later ranges, blockHash reads and logs subscriptions
// make no further logs calls for those blocks.
func TestHttp_LogsFill_LiveWindowAdoptsFill(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{
		Enabled: true, Depth: 16, PollInterval: common.Duration(100 * time.Millisecond),
		LogsFill: common.EvmBlockStoreLogsFillConfig{Enabled: true, UnfinalizedTTL: common.Duration(time.Minute)},
	})
	send, _, _, shutdown, instance := createServerTestFixtures(cfg, t)
	defer shutdown()
	hc := blockStoreOf(t, instance)
	// No subscriber: the window is built from what the fill returned (pull).
	require.Eventually(t, func() bool { return instanceNetwork(t, instance).EvmHighestLatestBlockNumber(t.Context()) == 20 }, 10*time.Second, 20*time.Millisecond)

	// The window validates a fill's lists against headers it already holds
	// (here, from the client's own block reads); it never fetches one.
	for n := 16; n <= 19; n++ {
		doRpc(t, send, "eth_getBlockByNumber", fmt.Sprintf(`["0x%x",false]`, n))
	}
	require.Eventually(t, func() bool { return hc.CanonicalHash(16) != "" && hc.CanonicalHash(19) != "" },
		5*time.Second, 20*time.Millisecond)
	headers := up.HeaderCalls()
	first := doRpc(t, send, "eth_getLogs", `[{"fromBlock":"0x10","toBlock":"0x13"}]`)
	require.Equal(t, int64(1), up.UnfilteredLogCalls(), "the miss is filled by one unfiltered range call")
	require.Zero(t, up.BlockHashLogCalls(), "the window does not fetch per-block logs for the same range")
	// Adoption runs asynchronously after the response.
	require.Eventually(t, func() bool {
		_, ok := hc.LogsRangeCached(t.Context(), 16, 19, nil)
		return ok
	}, 5*time.Second, 20*time.Millisecond, "the fill's lists are adopted with their headers")
	require.Equal(t, headers, up.HeaderCalls(), "adoption fetches no header")

	again := doRpc(t, send, "eth_getLogs", fmt.Sprintf(`[{"fromBlock":"0x10","toBlock":"0x13","topics":[%q]}]`, scriptedTopicEven))
	var even []map[string]interface{}
	require.NoError(t, json.Unmarshal(again.Result, &even))
	require.Len(t, even, 2)
	byHash := doRpc(t, send, "eth_getLogs", fmt.Sprintf(`[{"blockHash":%q}]`, up.HashAt(17)))
	require.Contains(t, string(first.Result), strings.TrimSuffix(strings.TrimPrefix(string(byHash.Result), "["), "]"))
	logs, ok := hc.LogsRange(t.Context(), 16, 19, nil)
	require.True(t, ok)
	require.Len(t, logs, 4)
	require.Equal(t, int64(1), up.UnfilteredLogCalls())
	require.Zero(t, up.BlockHashLogCalls(), "adopted lists serve window reads with no upstream call")
}

// A panic while adopting a fill into the live window is recovered and counted
// instead of crashing the process (the adopt goroutine is detached from any
// request). A zero-value Cache panics on AdoptLogs (nil clock).
func TestHttp_LogsFill_AdoptPanicIsRecovered(t *testing.T) {
	f := newLogsFillFixture(t, 120, false)
	f.network.blockStore = &blockstore.Cache{}
	f.network.blockStoreAdoptSem = make(chan struct{}, blockStoreAdoptLimit)
	panics := func() float64 {
		ch := make(chan prometheus.Metric, 1024)
		go func() { telemetry.MetricUnexpectedPanicTotal.Collect(ch); close(ch) }()
		total := 0.0
		for m := range ch {
			var pb dto.Metric
			require.NoError(t, m.Write(&pb))
			for _, l := range pb.GetLabel() {
				if l.GetName() == "scope" && l.GetValue() == "blockstore-adopt" {
					total += pb.GetCounter().GetValue()
				}
			}
		}
		return total
	}
	panicsBefore := panics()
	before := f.up.UnfilteredLogCalls()
	logs := f.getLogs(t, 100, 104, "")
	require.NotEmpty(t, logs)
	require.Equal(t, before+1, f.up.UnfilteredLogCalls())
	require.Eventually(t, func() bool { return len(f.network.blockStoreAdoptSem) == 0 }, 5*time.Second, 10*time.Millisecond,
		"the adopt goroutine must finish (recovered) and release its slot")
	require.Eventually(t, func() bool { return panics() == panicsBefore+1 }, 5*time.Second, 10*time.Millisecond,
		"the recovered panic is counted under scope blockstore-adopt")
}
