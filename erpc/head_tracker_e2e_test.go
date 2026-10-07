package erpc

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/blockstore"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// timedChain is a JSON-RPC EVM node whose head advances on the wall clock
// (one block per blockTime) with real unix timestamps, so the head tracker's
// block-time alignment is exercised as on a live chain. It counts calls per
// method and per "latest" tag, and can serve a frozen head for eth_blockNumber
// (a stale poller view) while getBlockByNumber stays live.
type timedChain struct {
	start     time.Time
	blockTime time.Duration
	base      int64
	srv       *httptest.Server

	mu    sync.Mutex
	calls map[string]int
	// latestCalls counts eth_getBlockByNumber("latest", *) calls.
	latestCalls atomic.Int64
	// byNumberCalls / byHashCalls count explicit-number and by-hash reads.
	byNumberCalls atomic.Int64
	byHashCalls   atomic.Int64
	// frozenBlockNumber, when > 0, is what eth_blockNumber returns.
	frozenBlockNumber atomic.Int64
	// lagBlocks makes this node see the chain that many blocks late: a
	// lagging upstream that returns null (or an error) for the head block.
	lagBlocks atomic.Int64
	// failData makes eth_call / eth_getLogs fail with a retryable server
	// error, so data traffic fails over to the other upstreams.
	failData atomic.Bool
	// pinned, when > 0, freezes the chain at that height (deterministic
	// assertions on one head block).
	pinned atomic.Int64
	// errorBeyondHead makes eth_getLogs past this node's head an error
	// instead of a silently short result.
	errorBeyondHead atomic.Bool
	// unsupportedLogs answers eth_getLogs with "method not found".
	unsupportedLogs atomic.Bool
}

func (c *timedChain) resolveTag(ref string) int64 {
	switch ref {
	case "latest", "", "<nil>":
		return c.head()
	}
	n, _ := strconv.ParseInt(strings.TrimPrefix(ref, "0x"), 16, 64)
	return n
}

func newTimedChain(blockTime time.Duration, base int64) *timedChain {
	c := &timedChain{start: time.Now(), blockTime: blockTime, base: base, calls: map[string]int{}}
	c.srv = httptest.NewServer(http.HandlerFunc(c.serve))
	return c
}

func (c *timedChain) Close()      { c.srv.Close() }
func (c *timedChain) URL() string { return c.srv.URL }

func (c *timedChain) head() int64 {
	if p := c.pinned.Load(); p > 0 {
		return p - c.lagBlocks.Load()
	}
	return c.base + int64(time.Since(c.start)/c.blockTime) - c.lagBlocks.Load()
}

// view returns a node sharing the same chain (start, base, block time) but
// with its own server and counters, so several upstreams of one network can
// be observed separately.
func (c *timedChain) view() *timedChain {
	v := &timedChain{start: c.start, blockTime: c.blockTime, base: c.base, calls: map[string]int{}}
	v.srv = httptest.NewServer(http.HandlerFunc(v.serve))
	return v
}

func (c *timedChain) Calls(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[method]
}

func timedHash(n int64) string { return fmt.Sprintf("0x%064x", n) }

func (c *timedChain) block(n int64, full bool) interface{} {
	if n > c.head() || n < 0 {
		return nil
	}
	ts := c.start.Add(time.Duration(n-c.base) * c.blockTime).Unix()
	tx := fmt.Sprintf("0x%064x", n+1<<40)
	var txs interface{} = []string{tx}
	if full {
		txs = []map[string]interface{}{{
			"hash": tx, "blockHash": timedHash(n), "blockNumber": fmt.Sprintf("0x%x", n),
			"transactionIndex": "0x0", "from": "0x0000000000000000000000000000000000000001",
			"to": "0x0000000000000000000000000000000000000002", "value": "0x0", "input": "0x",
			"nonce": "0x0", "gas": "0x5208", "gasPrice": "0x1", "type": "0x0",
			"v": "0x1b", "r": "0x1", "s": "0x1",
		}}
	}
	return map[string]interface{}{
		"number": fmt.Sprintf("0x%x", n), "hash": timedHash(n), "parentHash": timedHash(n - 1),
		"timestamp": fmt.Sprintf("0x%x", ts), "gasLimit": "0x1c9c380", "gasUsed": "0x5208",
		"miner": "0x0000000000000000000000000000000000000000", "extraData": "0x",
		"logsBloom": "0x" + strings.Repeat("0", 512), "transactions": txs, "uncles": []string{},
	}
}

func (c *timedChain) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Id     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &req)
	c.mu.Lock()
	c.calls[req.Method]++
	c.mu.Unlock()
	var result interface{}
	var rpcErr interface{}
	if c.failData.Load() && (req.Method == "eth_call" || req.Method == "eth_getLogs") {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"temporarily unavailable"}}`))
		return
	}
	switch req.Method {
	case "eth_getLogs":
		if c.unsupportedLogs.Load() {
			rpcErr = map[string]interface{}{"code": -32601, "message": "the method eth_getLogs does not exist/is not available"}
		}
	}
	if rpcErr != nil {
		resp := map[string]interface{}{"jsonrpc": "2.0", "id": req.Id, "error": rpcErr}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	switch req.Method {
	case "eth_chainId":
		result = "0x7b"
	case "net_version":
		result = "123"
	case "eth_syncing":
		result = false
	case "eth_blockNumber":
		h := c.head()
		if f := c.frozenBlockNumber.Load(); f > 0 {
			h = f
		}
		result = fmt.Sprintf("0x%x", h)
	case "eth_call":
		var tag string
		if len(req.Params) > 1 {
			_ = json.Unmarshal(req.Params[1], &tag)
		}
		if strings.HasPrefix(tag, "0x") {
			if n, err := strconv.ParseInt(tag[2:], 16, 64); err == nil && n > c.head() {
				// Some providers answer a future block with a RESULT computed
				// at their own head: wrong data, marked so tests can see it.
				result = "0xdead"
				break
			}
		}
		result = "0x" + strings.Repeat("0", 63) + "1"
		if tag != "latest" {
			// Echo a different value for interpolated calls so a test can
			// tell whether "latest" reached the upstream untouched.
			result = "0x" + strings.Repeat("0", 63) + "2"
		}
	case "eth_getLogs":
		var flt map[string]interface{}
		_ = json.Unmarshal(req.Params[0], &flt)
		from := c.resolveTag(fmt.Sprint(flt["fromBlock"]))
		to := c.resolveTag(fmt.Sprint(flt["toBlock"]))
		if c.errorBeyondHead.Load() && to > c.head() {
			rpcErr = map[string]interface{}{"code": -32000, "message": "block range extends beyond current head block"}
			break
		}
		// Like most nodes, a block this node has not seen yet silently
		// contributes no logs: every block has exactly one log, so an empty
		// or short result for a range at the head is WRONG data.
		logs := []interface{}{}
		for n := from; n <= to && n <= c.head(); n++ {
			logs = append(logs, map[string]interface{}{
				"address": "0x0000000000000000000000000000000000000002", "topics": []string{},
				"data": "0x", "blockNumber": fmt.Sprintf("0x%x", n), "blockHash": timedHash(n),
				"transactionHash": fmt.Sprintf("0x%064x", n+1<<40), "transactionIndex": "0x0",
				"logIndex": "0x0", "removed": false,
			})
		}
		result = logs
	case "eth_getBalance", "eth_gasPrice":
		// Head-relative state: the answer is the height this node computed
		// it at, so a test can tell a lagging upstream's stale answer.
		at := c.head()
		if len(req.Params) > 1 {
			var tag string
			_ = json.Unmarshal(req.Params[1], &tag)
			if strings.HasPrefix(tag, "0x") {
				at = min(c.resolveTag(tag), c.head())
			}
		}
		result = fmt.Sprintf("0x%x", at)
	case "eth_getBlockReceipts":
		var ref string
		_ = json.Unmarshal(req.Params[0], &ref)
		n := c.resolveTag(ref)
		// A block this node has not seen yet: [] (as several providers do).
		receipts := []interface{}{}
		if n <= c.head() {
			receipts = append(receipts, map[string]interface{}{
				"blockNumber": fmt.Sprintf("0x%x", n), "blockHash": timedHash(n),
				"transactionHash": fmt.Sprintf("0x%064x", n+1<<40), "transactionIndex": "0x0",
				"status": "0x1", "logs": []interface{}{}, "gasUsed": "0x5208", "cumulativeGasUsed": "0x5208",
			})
		}
		result = receipts
	case "eth_getBlockByNumber", "eth_getBlockByHash":
		var ref string
		var full bool
		_ = json.Unmarshal(req.Params[0], &ref)
		if len(req.Params) > 1 {
			_ = json.Unmarshal(req.Params[1], &full)
		}
		var n int64
		switch {
		case req.Method == "eth_getBlockByHash":
			c.byHashCalls.Add(1)
			n, _ = strconv.ParseInt(strings.TrimPrefix(ref, "0x"), 16, 64)
		case ref == "latest":
			c.latestCalls.Add(1)
			n = c.head()
		case ref == "finalized" || ref == "safe":
			n = c.head() - 64
		default:
			c.byNumberCalls.Add(1)
			n, _ = strconv.ParseInt(strings.TrimPrefix(ref, "0x"), 16, 64)
		}
		result = c.block(n, full)
	default:
		rpcErr = map[string]interface{}{"code": -32601, "message": "the method " + req.Method + " does not exist/is not available"}
	}
	resp := map[string]interface{}{"jsonrpc": "2.0", "id": req.Id}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// headTrackerTestConfig builds one replica's config: shared state and the
// JSON-RPC cache in the same Redis, a slow (60s) state poller, and the head
// tracker on or off.
func headTrackerTestConfig(redisAddr, cluster, upstreamURL string, ht *common.EvmHeadTrackerConfig, cache bool) *common.Config {
	rc := func() *common.RedisConnectorConfig {
		c := &common.RedisConnectorConfig{Addr: redisAddr, ConnPoolSize: 8}
		// The test fixture builds the sharedState registry before
		// cfg.SetDefaults runs, so defaults are applied here.
		_ = c.SetDefaults()
		return c
	}
	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Database: &common.DatabaseConfig{
			SharedState: &common.SharedStateConfig{
				ClusterKey: cluster,
				Connector:  &common.ConnectorConfig{Id: "ss", Driver: common.DriverRedis, Redis: rc()},
			},
		},
		Projects: []*common.ProjectConfig{{
			Id: "test_project",
			Networks: []*common.NetworkConfig{{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123, HeadTracker: ht},
			}},
			Upstreams: []*common.UpstreamConfig{{
				Id:       "timed",
				Type:     common.UpstreamTypeEvm,
				Endpoint: upstreamURL,
				Evm:      &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(60 * time.Second)},
			}},
		}},
		RateLimiters: &common.RateLimiterConfig{},
	}
	if cache {
		cfg.Database.EvmJsonRpcCache = &common.CacheConfig{
			Connectors: []*common.ConnectorConfig{{Id: "cache", Driver: common.DriverRedis, Redis: rc()}},
			Policies: []*common.CachePolicyConfig{
				{Network: "*", Method: "*", Finality: common.DataFinalityStateUnfinalized, Connector: "cache", TTL: common.FixedDuration(time.Minute)},
				{Network: "*", Method: "*", Finality: common.DataFinalityStateFinalized, Connector: "cache", TTL: common.FixedDuration(time.Minute)},
				{Network: "*", Method: "*", Finality: common.DataFinalityStateUnknown, Connector: "cache", TTL: common.FixedDuration(time.Minute)},
			},
		}
	}
	return cfg
}

type htReplica struct {
	send     func(string, map[string]string, map[string]string) (int, map[string]string, string)
	shutdown func()
	erpc     *ERPC
}

func (r htReplica) network(t *testing.T) *Network { return instanceNetwork(t, r.erpc) }

func (r htReplica) blockNumber(t *testing.T) int64 {
	res := doRpc(t, r.send, "eth_blockNumber", "[]")
	var s string
	require.NoError(t, json.Unmarshal(res.Result, &s))
	n, err := common.HexToInt64(s)
	require.NoError(t, err)
	return n
}

func startHTReplicas(t *testing.T, n int, mk func() *common.Config) []htReplica {
	t.Helper()
	out := make([]htReplica, n)
	for i := range out {
		send, _, _, shutdown, e := createServerTestFixtures(mk(), t)
		out[i] = htReplica{send: send, shutdown: shutdown, erpc: e}
		t.Cleanup(shutdown)
		// Materialize the network (and its tracker) on every replica.
		_ = out[i].network(t)
	}
	return out
}

func trackerOf(t *testing.T, r htReplica) *headTracker {
	ht := r.network(t).headTracker
	require.NotNil(t, ht)
	return ht
}

// N replicas, one fast chain, 60s state pollers: eth_blockNumber on every
// replica tracks the chain within about one block while the upstream sees
// about one "latest" poll per block in total.
func TestHeadTracker_E2E_MultiPodTracksFastChain(t *testing.T) {
	mr := miniredis.RunT(t)
	chain := newTimedChain(time.Second, 5_000)
	defer chain.Close()
	cluster := fmt.Sprintf("ht-e2e-%d", time.Now().UnixNano())
	ht := func() *common.EvmHeadTrackerConfig {
		return &common.EvmHeadTrackerConfig{Enabled: true, LeaseTtl: common.Duration(2 * time.Second)}
	}
	reps := startHTReplicas(t, 3, func() *common.Config { return headTrackerTestConfig(mr.Addr(), cluster, chain.URL(), ht(), false) })

	require.Eventually(t, func() bool {
		for _, r := range reps {
			if trackerOf(t, r).FreshHead() < chain.head()-1 {
				return false
			}
		}
		return true
	}, 10*time.Second, 50*time.Millisecond, "every replica follows the tracker head")

	// Count concurrent leaders on every tick of the run, not one snapshot.
	stopSampling := make(chan struct{})
	var maxLeaders, minLeaders atomic.Int32
	minLeaders.Store(99)
	trackers := make([]*headTracker, len(reps))
	for i, r := range reps {
		trackers[i] = trackerOf(t, r)
	}
	go func() {
		tk := time.NewTicker(5 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-tk.C:
				var c int32
				for _, tr := range trackers {
					if tr.IsLeader() {
						c++
					}
				}
				maxLeaders.Store(max(maxLeaders.Load(), c))
				minLeaders.Store(min(minLeaders.Load(), c))
			}
		}
	}()

	window := 6 * time.Second
	startLatest := chain.latestCalls.Load()
	startBN := chain.Calls("eth_blockNumber")
	deadline := time.Now().Add(window)
	maxLag := int64(0)
	for time.Now().Before(deadline) {
		for _, r := range reps {
			lag := chain.head() - r.blockNumber(t)
			maxLag = max(maxLag, lag)
		}
		time.Sleep(100 * time.Millisecond)
	}
	close(stopSampling)
	require.Equal(t, int32(1), maxLeaders.Load(), "never more than one polling replica")
	require.Equal(t, int32(1), minLeaders.Load(), "always exactly one polling replica")
	latestPerBlock := float64(chain.latestCalls.Load()-startLatest) / window.Seconds()
	t.Logf("3 replicas: %.2f latest polls per block, %d eth_blockNumber upstream calls, max lag %d blocks",
		latestPerBlock, chain.Calls("eth_blockNumber")-startBN, maxLag)
	require.LessOrEqual(t, maxLag, int64(1), "eth_blockNumber within ~1 block of the chain on every replica")
	require.LessOrEqual(t, latestPerBlock, 1.5, "about one poll per block across the fleet")
	require.Zero(t, chain.Calls("eth_blockNumber")-startBN, "client eth_blockNumber is answered locally")
}

// Kill the leader: a follower takes over within about TTL + 1 block and the
// head keeps advancing.
func TestHeadTracker_E2E_LeaderFailover(t *testing.T) {
	mr := miniredis.RunT(t)
	// miniredis expires keys only when time is advanced.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				mr.FastForward(50 * time.Millisecond)
			}
		}
	}()
	chain := newTimedChain(time.Second, 9_000)
	defer chain.Close()
	cluster := fmt.Sprintf("ht-fo-%d", time.Now().UnixNano())
	ttl := 2 * time.Second
	reps := startHTReplicas(t, 2, func() *common.Config {
		return headTrackerTestConfig(mr.Addr(), cluster, chain.URL(), &common.EvmHeadTrackerConfig{Enabled: true, LeaseTtl: common.Duration(ttl)}, false)
	})
	var leader, follower htReplica
	require.Eventually(t, func() bool {
		for i, r := range reps {
			if trackerOf(t, r).IsLeader() {
				leader, follower = r, reps[1-i]
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool { return follower.blockNumber(t) >= chain.head()-1 }, 10*time.Second, 50*time.Millisecond)

	// Crash: the leader stops without releasing its lease.
	lt := trackerOf(t, leader)
	lt.abandonLease.Store(true)
	lt.Stop()
	killed := time.Now()
	ft := trackerOf(t, follower)
	require.Eventually(t, ft.IsLeader, 3*ttl, 20*time.Millisecond)
	took := time.Since(killed)
	t.Logf("takeover after %s (ttl %s)", took, ttl)
	require.LessOrEqual(t, took, ttl+time.Second+500*time.Millisecond)
	require.Eventually(t, func() bool { return follower.blockNumber(t) >= chain.head()-1 }, ttl+3*time.Second, 50*time.Millisecond,
		"the new leader keeps the head advancing")
}

// After one leader poll, another replica sharing the cache serves
// getBlockByNumber(latest), (<hex>) and getBlockByHash with zero upstream
// calls; a fullBlocks poll also fills the hashes-only variant.
func TestHeadTracker_E2E_FollowerServesPolledBlockFromCache(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("fullBlocks=%v", full), func(t *testing.T) {
			mr := miniredis.RunT(t)
			chain := newTimedChain(2*time.Second, 7_000)
			defer chain.Close()
			cluster := fmt.Sprintf("ht-cache-%d", time.Now().UnixNano())
			reps := startHTReplicas(t, 2, func() *common.Config {
				return headTrackerTestConfig(mr.Addr(), cluster, chain.URL(),
					&common.EvmHeadTrackerConfig{Enabled: true, FullBlocks: full, LeaseTtl: common.Duration(2 * time.Second)}, true)
			})
			var follower htReplica
			require.Eventually(t, func() bool {
				for i, r := range reps {
					if trackerOf(t, r).IsLeader() {
						follower = reps[1-i]
						return true
					}
				}
				return false
			}, 10*time.Second, 20*time.Millisecond)

			// Freeze the chain, then wait until the follower serves that head
			// as fresh. The leader writes the block BEFORE publishing (S1), so
			// no sleep is needed: seeing the head implies the block is cached.
			require.Eventually(t, func() bool { return trackerOf(t, follower).FreshHead() > 0 }, 10*time.Second, 20*time.Millisecond)
			head := chain.head()
			chain.pinned.Store(head)
			require.Eventually(t, func() bool { return trackerOf(t, follower).FreshHead() == head }, 5*time.Second, 5*time.Millisecond)
			hexHead := fmt.Sprintf("0x%x", head)

			before := chain.byNumberCalls.Load() + chain.byHashCalls.Load()
			beforeLatest := chain.latestCalls.Load()
			for _, variant := range []bool{false, true} {
				if variant && !full {
					continue // a hashes-only poll never fabricates a full block
				}
				p := fmt.Sprintf(`["latest",%v]`, variant)
				r := doRpc(t, follower.send, "eth_getBlockByNumber", p)
				require.Contains(t, string(r.Result), timedHash(head), "latest resolves to the tracker head")
				r = doRpc(t, follower.send, "eth_getBlockByNumber", fmt.Sprintf(`["%s",%v]`, hexHead, variant))
				require.Contains(t, string(r.Result), timedHash(head))
				r = doRpc(t, follower.send, "eth_getBlockByHash", fmt.Sprintf(`["%s",%v]`, timedHash(head), variant))
				require.Contains(t, string(r.Result), hexHead)
			}
			require.Equal(t, before, chain.byNumberCalls.Load()+chain.byHashCalls.Load(), "follower reads made no upstream call")
			// The leader may poll latest once more meanwhile; the follower never does.
			require.LessOrEqual(t, chain.latestCalls.Load()-beforeLatest, int64(2))
		})
	}
}

// With the tracker OFF (premium-style config: no servedTip, skipInterpolation,
// enforceHighestBlock=false), eth_blockNumber, "latest" eth_call and
// getBlockByNumber("latest") reach the upstream untouched even while the
// state poller's view is stale, and no tracker runs.
func TestHeadTracker_E2E_PremiumPassthroughWhileDisabled(t *testing.T) {
	mr := miniredis.RunT(t)
	chain := newTimedChain(500*time.Millisecond, 3_000)
	defer chain.Close()
	cfg := headTrackerTestConfig(mr.Addr(), fmt.Sprintf("ht-prem-%d", time.Now().UnixNano()), chain.URL(), nil, false)
	cfg.Projects[0].Networks[0].DirectiveDefaults = &common.DirectiveDefaultsConfig{
		SkipInterpolation:   util.BoolPtr(true),
		EnforceHighestBlock: util.BoolPtr(false),
	}
	reps := startHTReplicas(t, 1, func() *common.Config { return cfg })
	r := reps[0]
	require.Nil(t, r.network(t).headTracker, "tracker is inert when not enabled")

	// Let the poller seed, then let the chain run ahead of it (60s interval).
	require.Eventually(t, func() bool { return r.network(t).EvmHighestLatestBlockNumber(t.Context()) > 0 }, 5*time.Second, 20*time.Millisecond)
	time.Sleep(2 * time.Second)
	stale := r.network(t).EvmHighestLatestBlockNumber(t.Context())
	require.Less(t, stale, chain.head()-1, "poller view is stale")

	before := chain.Calls("eth_blockNumber")
	got := r.blockNumber(t)
	require.GreaterOrEqual(t, got, chain.head()-1, "eth_blockNumber is the live upstream answer, not the stale poller value")
	require.Greater(t, chain.Calls("eth_blockNumber"), before, "eth_blockNumber went upstream")

	res := doRpc(t, r.send, "eth_call", `[{"to":"0x0000000000000000000000000000000000000002","data":"0x"},"latest"]`)
	require.Equal(t, `"0x`+strings.Repeat("0", 63)+`1"`, string(res.Result), `"latest" reached the upstream as the tag`)

	beforeLatest := chain.latestCalls.Load()
	res = doRpc(t, r.send, "eth_getBlockByNumber", `["latest",false]`)
	require.Greater(t, chain.latestCalls.Load(), beforeLatest, "getBlockByNumber(latest) went upstream as the tag")
	var blk struct{ Number string }
	require.NoError(t, json.Unmarshal(res.Result, &blk))
	n, _ := common.HexToInt64(blk.Number)
	require.Greater(t, n, stale, "latest block is live, not the stale poller head")
}

// latestTrafficScenario runs 3 replicas x 3 upstreams on a 1s chain and a
// steady stream of eth_getLogs(latest), eth_call(latest) and
// getBlockByNumber(latest) for `blocks` blocks. setup configures the three
// upstream nodes and their configs. It returns per-upstream head polls
// (getBlockByNumber(latest) + eth_blockNumber), failures, and wrong getLogs
// results (a range at the head must contain one log per block).
type trafficResult struct {
	reqs, fails, wrongLogs int
	polls                  [3]int64
}

func runLatestTraffic(t *testing.T, blocks int, setup func(ups []*timedChain, cfgs []*common.UpstreamConfig)) trafficResult {
	mr := miniredis.RunT(t)
	chain := newTimedChain(time.Second, 20_000)
	defer chain.Close()
	ups := []*timedChain{chain, chain.view(), chain.view()}
	defer ups[1].Close()
	defer ups[2].Close()

	cluster := fmt.Sprintf("ht-traffic-%d", time.Now().UnixNano())
	mk := func() *common.Config {
		cfg := headTrackerTestConfig(mr.Addr(), cluster, chain.URL(),
			&common.EvmHeadTrackerConfig{Enabled: true, LeaseTtl: common.Duration(2 * time.Second)}, false)
		prj := cfg.Projects[0]
		base := prj.Upstreams[0]
		prj.Upstreams = nil
		for i, u := range ups {
			c := *base
			evmCfg := *base.Evm
			c.Evm = &evmCfg
			c.Id = fmt.Sprintf("u%d", i)
			c.Endpoint = u.URL()
			prj.Upstreams = append(prj.Upstreams, &c)
		}
		setup(ups, prj.Upstreams)
		return cfg
	}
	reps := startHTReplicas(t, 3, mk)
	for _, r := range reps {
		r.network(t).PinUpstreamOrderForTest("u0", "u1", "u2")
	}
	require.Eventually(t, func() bool {
		for _, r := range reps {
			if trackerOf(t, r).FreshHead() < chain.head()-1 {
				return false
			}
		}
		return true
	}, 15*time.Second, 50*time.Millisecond)

	var res trafficResult
	var start [3]int64
	for i, u := range ups {
		start[i] = u.latestCalls.Load() + int64(u.Calls("eth_blockNumber"))
	}
	deadline := time.Now().Add(time.Duration(blocks) * chain.blockTime)
	for time.Now().Before(deadline) {
		for _, r := range reps {
			head := trackerOf(t, r).FreshHead()
			for _, q := range []struct{ m, p string }{
				{"eth_getLogs", fmt.Sprintf(`[{"fromBlock":"0x%x","toBlock":"latest"}]`, head-2)},
				{"eth_call", `[{"to":"0x0000000000000000000000000000000000000002","data":"0x"},"latest"]`},
				{"eth_getBlockByNumber", `["latest",false]`},
				{"eth_getBlockReceipts", fmt.Sprintf(`["0x%x"]`, head)},
			} {
				res.reqs++
				code, _, body := r.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, q.m, q.p), nil, nil)
				var resp rpcResp
				_ = json.Unmarshal([]byte(body), &resp)
				if code != 200 || len(resp.Error) > 0 || string(resp.Result) == "null" {
					res.fails++
					if res.fails <= 5 {
						t.Logf("%s failed: %d %s", q.m, code, body)
					}
					continue
				}
				if q.m == "eth_call" && strings.Contains(string(resp.Result), "dead") {
					res.wrongLogs++
					t.Logf("wrong eth_call result served: %s", resp.Result)
				}
				if q.m == "eth_getBlockReceipts" && string(resp.Result) == "[]" {
					res.wrongLogs++
					t.Logf("wrong (empty) eth_getBlockReceipts served for 0x%x", head)
				}
				if q.m == "eth_getLogs" {
					var logs []struct{ BlockNumber string }
					_ = json.Unmarshal(resp.Result, &logs)
					// fromBlock..latest where latest >= head: at least 3 logs.
					if len(logs) < 3 {
						res.wrongLogs++
						t.Logf("wrong getLogs result (%d logs) for range from 0x%x", len(logs), head-2)
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i, u := range ups {
		res.polls[i] = u.latestCalls.Load() + int64(u.Calls("eth_blockNumber")) - start[i]
	}
	t.Logf("%d requests over %d blocks: head polls per upstream %v (total %.2f per block), %d failures, %d wrong getLogs",
		res.reqs, blocks, res.polls, float64(res.polls[0]+res.polls[1]+res.polls[2])/float64(blocks), res.fails, res.wrongLogs)
	return res
}

// Regression (B1 + rc.2 tip check): with the tracker on, "latest" resolves
// to the tracker head, ahead of every upstream's 60s poller view.
//
// Steady state: u0 serves the leader's polls, so its head is the tracker
// head and it takes tip traffic with NO check. u1 has a head-relative serving
// range (latestBlockMinus: 0). u2 lags 3 blocks and fails every data method,
// so no response ever advances its head. Head polls stay ~1 per block in
// total (the leader), with zero checks of u2.
func TestHeadTracker_E2E_NoPerUpstreamPollingUnderLatestTraffic(t *testing.T) {
	res := runLatestTraffic(t, 30, func(ups []*timedChain, cfgs []*common.UpstreamConfig) {
		zero := int64(0)
		cfgs[1].Evm.BlockAvailability = &common.EvmBlockAvailabilityConfig{
			Upper: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: &zero},
		}
		ups[2].lagBlocks.Store(3)
		ups[2].failData.Store(true)
	})
	require.Zero(t, res.fails, "requests succeed")
	require.Zero(t, res.wrongLogs, "no short/empty getLogs result is served for a range at the head")
	require.LessOrEqual(t, res.polls[2], int64(3), "the lagging upstream is not checked (at most its own 60s ticks)")
	total := res.polls[0] + res.polls[1] + res.polls[2]
	require.LessOrEqual(t, float64(total), 30*1.1+9, "~1 head poll per block in total (leader) plus 60s ticks")
}

// Failover: the leader-served upstream u0 fails every data method, so tip
// traffic fails over to u1 (current) and u2 (1 block behind, inside the old
// proximity window, returns short/[] logs, null blocks, results at its own
// head). Each replica checks a non-leader-served upstream's head at most
// about once per block (debounced), u2 is skipped after its check, and its
// wrong results are never served.
func TestHeadTracker_E2E_FailoverChecksHeadOncePerBlock(t *testing.T) {
	const blocks = 20
	res := runLatestTraffic(t, blocks, func(ups []*timedChain, cfgs []*common.UpstreamConfig) {
		ups[0].failData.Store(true)
		ups[2].lagBlocks.Store(1)
	})
	require.Zero(t, res.wrongLogs, "u2's clamped logs are never served")
	require.Zero(t, res.fails, "requests succeed on u1")
	// Per non-leader upstream: at most ~1 check per block per replica.
	for i := 1; i <= 2; i++ {
		require.LessOrEqual(t, float64(res.polls[i]), blocks*3*1.2+3, "u%d: checks debounced to ~1 per block per replica", i)
	}
}

// NEW-1 (strict, lag inside the old proximity window): every data upstream
// lags 1-2 blocks and returns short/[] logs for the head. Nothing wrong is
// ever served; the requests are retried/failed instead.
func TestHeadTracker_E2E_LaggingUpstreamEmptyLogsNotServed(t *testing.T) {
	for _, lag := range []int64{1, 2} {
		t.Run(fmt.Sprintf("lag=%d", lag), func(t *testing.T) {
			res := runLatestTraffic(t, 8, func(ups []*timedChain, cfgs []*common.UpstreamConfig) {
				ups[0].failData.Store(true)
				ups[1].lagBlocks.Store(lag)
				ups[2].lagBlocks.Store(lag)
			})
			require.Zero(t, res.wrongLogs, "a lagging upstream's short getLogs result is never served")
		})
	}
}

// The hashes-only block derived from a fullBlocks poll must equal what an
// upstream returns for eth_getBlockByNumber(n, false): same fields and values,
// transactions as hashes in order (key order is irrelevant on the wire).
func TestHeadTracker_DerivedHashesOnlyBlockMatchesUpstream(t *testing.T) {
	type tx = map[string]interface{}
	hashes := []string{timedHash(1 << 41), timedHash(1<<41 + 1), timedHash(1<<41 + 2)}
	base := map[string]interface{}{
		"number": "0x10", "hash": timedHash(16), "parentHash": timedHash(15), "timestamp": "0x65",
		"gasLimit": "0x1c9c380", "gasUsed": "0x5208", "miner": "0x0000000000000000000000000000000000000000",
		"extraData": "0x", "logsBloom": "0x" + strings.Repeat("0", 512), "uncles": []string{},
		"baseFeePerGas": "0x7", "withdrawals": []interface{}{}, "size": "0x220",
	}
	full := map[string]interface{}{}
	hashOnly := map[string]interface{}{}
	for k, v := range base {
		full[k], hashOnly[k] = v, v
	}
	var txs []tx
	for i, h := range hashes {
		txs = append(txs, tx{"hash": h, "blockHash": timedHash(16), "blockNumber": "0x10",
			"transactionIndex": fmt.Sprintf("0x%x", i), "from": "0x0000000000000000000000000000000000000001",
			"input": "0x", "nonce": fmt.Sprintf("0x%x", i), "type": "0x2", "value": "0x0"})
	}
	full["transactions"] = txs
	hashOnly["transactions"] = hashes
	for name, pair := range map[string][2]interface{}{
		"three txs":  {full, hashOnly},
		"timedChain": {newTimedChain(time.Second, 1).block(1, true), newTimedChain(time.Second, 1).block(1, false)},
	} {
		fullRaw, _ := json.Marshal(pair[0])
		wantRaw, _ := json.Marshal(pair[1])
		got, err := (&blockstore.BlockRecord{Block: fullRaw}).BlockJSON(false)
		require.NoError(t, err, name)
		require.JSONEq(t, string(wantRaw), string(got), name)
	}
}

// Shared state (Redis) goes away: no replica can hold or renew the lease, so
// none polls; all fall back to the poller heads with the floor keeping
// eth_blockNumber monotonic; no polling storm.
func TestHeadTracker_E2E_RedisOutageFallsBackWithoutPolling(t *testing.T) {
	mr := miniredis.RunT(t)
	chain := newTimedChain(time.Second, 40_000)
	defer chain.Close()
	cluster := fmt.Sprintf("ht-outage-%d", time.Now().UnixNano())
	reps := startHTReplicas(t, 3, func() *common.Config {
		return headTrackerTestConfig(mr.Addr(), cluster, chain.URL(),
			&common.EvmHeadTrackerConfig{Enabled: true, LeaseTtl: common.Duration(time.Second)}, false)
	})
	require.Eventually(t, func() bool {
		for _, r := range reps {
			if trackerOf(t, r).FreshHead() < chain.head()-1 {
				return false
			}
		}
		return true
	}, 15*time.Second, 50*time.Millisecond)
	last := map[int]int64{}
	for i, r := range reps {
		last[i] = r.blockNumber(t)
	}

	mr.Close()
	// Every replica steps down within ~2/3 TTL and enters fallback after
	// the staleness window.
	require.Eventually(t, func() bool {
		for _, r := range reps {
			if trackerOf(t, r).IsLeader() || trackerOf(t, r).FreshHead() != 0 {
				return false
			}
		}
		return true
	}, 90*time.Second, 100*time.Millisecond, "no leader and every replica in fallback (within the published window)")

	// Client eth_blockNumber requests go upstream in fallback (as without the
	// tracker), so count only background head polls: getBlockByNumber(latest).
	start := chain.latestCalls.Load()
	window := 5 * time.Second
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		for i, r := range reps {
			bn := r.blockNumber(t)
			require.GreaterOrEqual(t, bn, last[i], "eth_blockNumber never goes backwards in fallback")
			last[i] = bn
		}
		time.Sleep(100 * time.Millisecond)
	}
	polls := chain.latestCalls.Load() - start
	t.Logf("redis outage: %d upstream head polls in %s across 3 replicas", polls, window)
	require.LessOrEqual(t, polls, int64(3), "no polling storm: only the 60s pollers may tick")
}

// Head contamination (homura): an upstream that answers a request for a
// future block (an eth_call RESULT computed at its own head, or clamped
// logs) must never be credited with that block's height. Only its own head
// check / poll or the leader's poll response advance its known head.
func TestHeadTracker_E2E_ResponsesDoNotAdvanceUpstreamHead(t *testing.T) {
	mr := miniredis.RunT(t)
	chain := newTimedChain(time.Second, 60_000)
	defer chain.Close()
	lagging := chain.view()
	defer lagging.Close()
	lagging.lagBlocks.Store(50)
	cfg := headTrackerTestConfig(mr.Addr(), fmt.Sprintf("ht-contam-%d", time.Now().UnixNano()), lagging.URL(), nil, false)
	// Tracker off: requests at a future block are forwarded as-is, so the
	// only way the lagging node's head could move is response crediting.
	reps := startHTReplicas(t, 1, func() *common.Config { return cfg })
	nw := reps[0].network(t)
	require.Eventually(t, func() bool { return nw.EvmHighestLatestBlockNumber(t.Context()) > 0 }, 5*time.Second, 20*time.Millisecond)
	sp := nw.AllUpstreams()[0].EvmStatePoller()
	before := sp.LatestBlock()
	future := before + 40
	res := doRpc(t, reps[0].send, "eth_call", fmt.Sprintf(`[{"to":"0x0000000000000000000000000000000000000002","data":"0x"},"0x%x"]`, future))
	require.Contains(t, string(res.Result), "dead", "the provider returns a (wrong) result for the future block")
	_, _, _ = reps[0].send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x%x","toBlock":"0x%x"}]}`, before-2, future), nil, nil)
	time.Sleep(200 * time.Millisecond)
	require.Less(t, sp.LatestBlock(), future, "no response credited the upstream with the requested future height")
}

// fluxPriorityEval is the flux-latitude-oku PRIORITY_EVAL_FUNC (build.ts), with
// PRIORITY_TAG="priority:" and UNTAGGED_PRIORITY=1000000 substituted.
const fluxPriorityEval = `(upstreams, ctx) =>
  upstreams
    .removeCordoned()
    .excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
    .excludeIf(all(samplesAbove(10), throttleRateAbove(0.4)))
    .excludeIf(any(all(samplesAbove(20), latencyAbove(3000), latencyDeviationAbove(3, { mode: 'majority' })), latencyAbove(10000)))
    .excludeIf(any(blockNumberLagAbove(16), blockSecondsLagAbove(30)))
    .whenEmpty(() => upstreams)
    .sortByScore(PREFER_FASTEST)
    .sortBy((u) => {
      const tag = (u.tags || []).find((t) => t.indexOf('priority:') === 0);
      const n = tag ? Number(tag.slice(9)) : NaN;
      return isFinite(n) ? n : 1000000;
    })
    .probeExcluded({ sampleRate: 0.1, minSamples: 10, minSamplesWindow: '60s', maxConcurrent: 4, timeout: '10s' })
`

// priorityNet is one replica of a head-tracked network with 4 upstreams
// (u0..u3, priority 1 / 1.2 / 1.25 / 1.7) on one timed chain, routed by the
// flux priority policy, 60s state pollers and no response cache, so every
// client request reaches an upstream.
type priorityNet struct {
	chain *timedChain
	ups   []*timedChain
	rep   htReplica
	nw    *Network
}

func startPriorityNet(t *testing.T, blockTime time.Duration, evalScope common.EvalScope, scoreWindow time.Duration, setup func(ups []*timedChain)) *priorityNet {
	t.Helper()
	mr := miniredis.RunT(t)
	chain := newTimedChain(blockTime, 81_850_000)
	t.Cleanup(chain.Close)
	ups := []*timedChain{chain, chain.view(), chain.view(), chain.view()}
	for _, u := range ups[1:] {
		t.Cleanup(u.Close)
	}
	if setup != nil {
		setup(ups)
	}
	prios := []string{"1", "1.2", "1.25", "1.7"}
	cfg := headTrackerTestConfig(mr.Addr(), fmt.Sprintf("ht-prio-%d", time.Now().UnixNano()), chain.URL(),
		&common.EvmHeadTrackerConfig{Enabled: true, LeaseTtl: common.Duration(2 * time.Second), FullBlocks: true}, false)
	prj := cfg.Projects[0]
	if scoreWindow > 0 {
		prj.ScoreMetricsWindowSize = common.Duration(scoreWindow)
	}
	prj.Networks[0].SelectionPolicy = &common.SelectionPolicyConfig{
		EvalFunc:     fluxPriorityEval,
		EvalInterval: common.Duration(500 * time.Millisecond),
		EvalScope:    evalScope,
	}
	base := prj.Upstreams[0]
	prj.Upstreams = nil
	for i, u := range ups {
		c := *base
		evmCfg := *base.Evm
		c.Evm = &evmCfg
		c.Id = fmt.Sprintf("u%d", i)
		c.Endpoint = u.URL()
		c.Tags = []string{"priority:" + prios[i]}
		prj.Upstreams = append(prj.Upstreams, &c)
	}
	rep := startHTReplicas(t, 1, func() *common.Config { return cfg })[0]
	require.Eventually(t, func() bool {
		return trackerOf(t, rep).FreshHead() >= chain.head()-3
	}, 15*time.Second, 20*time.Millisecond, "tracker follows the chain")
	return &priorityNet{chain: chain, ups: ups, rep: rep, nw: rep.network(t)}
}

func (p *priorityNet) upstream(t *testing.T, i int) common.EvmUpstream {
	for _, u := range p.nw.upstreamsRegistry.GetNetworkUpstreams(t.Context(), p.nw.networkId) {
		if u.Id() == fmt.Sprintf("u%d", i) {
			return u
		}
	}
	t.Fatalf("u%d not found", i)
	return nil
}

func probeCount(nw *Network, id string, methods ...string) float64 {
	var s float64
	for _, m := range methods {
		s += testutil.ToFloat64(telemetry.MetricSelectionProbeRequests.WithLabelValues(nw.networkId, id, m))
	}
	return s
}

// upstreamCalls is every JSON-RPC call a node received, by method.
func (c *timedChain) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.calls))
	for k, v := range c.calls {
		out[k] = v
	}
	return out
}

func callsDelta(before, after map[string]int) (map[string]int, int) {
	d := map[string]int{}
	total := 0
	for k, v := range after {
		if n := v - before[k]; n > 0 {
			d[k] = n
			total += n
		}
	}
	return d, total
}

type fastChainResult struct {
	reqs, fails, notLeader int
	leaderPolls            int64
	window                 time.Duration
	blocks                 int64
	perUpstream            [4]map[string]int
	extra                  [4]int // calls other than u0's leader polls / client traffic
	probes                 [4]float64
}

// runFastChainTraffic drives steady client eth_getBlockByNumber(n, true) and
// eth_getLogs at the tracker head for `window`. u1's shared head is kept
// current from the side (as other replicas' tip checks / its own
// fresh-enough poller do in production), so the corroborated network head is
// live while u2/u3's 60s poller views go stale: the dev-robinhood state that
// put hundreds of blocks of apparent lag on the non-leader upstreams.
func runFastChainTraffic(t *testing.T, legacy bool, window time.Duration) fastChainResult {
	p := startPriorityNet(t, 100*time.Millisecond, common.EvalScopeNetwork, 0, nil)
	if legacy {
		// Pre-fix behavior: the selection policy reads the poller lag view.
		p.nw.policyEngine.SetNetworkHooks(p.nw.networkId, nil)
	}
	u1 := p.upstream(t, 1)
	stopSide := make(chan struct{})
	sideDone := make(chan struct{})
	go func() {
		defer close(sideDone)
		tk := time.NewTicker(200 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stopSide:
				return
			case <-tk.C:
				u1.EvmStatePoller().SuggestLatestBlock(p.chain.head())
			}
		}
	}()
	defer func() { close(stopSide); <-sideDone }()
	// Let u2/u3's poller views go stale (> 16 blocks / a few seconds) and a
	// few policy ticks run.
	time.Sleep(3 * time.Second)

	methods := []string{"eth_getBlockByNumber", "eth_getLogs"}
	var before [4]map[string]int
	var probesBefore [4]float64
	for i, u := range p.ups {
		before[i] = u.snapshot()
		probesBefore[i] = probeCount(p.nw, fmt.Sprintf("u%d", i), methods...)
	}
	startHead := p.chain.head()
	startPolls := p.ups[0].latestCalls.Load()
	var res fastChainResult
	start := time.Now()
	for time.Since(start) < window {
		head := trackerOf(t, p.rep).FreshHead()
		for _, q := range []struct{ m, params string }{
			{"eth_getBlockByNumber", fmt.Sprintf(`["0x%x",true]`, head-2)},
			{"eth_getLogs", fmt.Sprintf(`[{"fromBlock":"0x%x","toBlock":"0x%x"}]`, head-2, head)},
		} {
			res.reqs++
			code, hdr, body := p.rep.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, q.m, q.params), nil, nil)
			var r rpcResp
			_ = json.Unmarshal([]byte(body), &r)
			if code != 200 || len(r.Error) > 0 || string(r.Result) == "null" {
				res.fails++
				if res.fails <= 3 {
					t.Logf("%s failed: %d %s", q.m, code, body)
				}
				continue
			}
			if hdr["X-Erpc-Upstream"] != "u0" && hdr["X-ERPC-Upstream"] != "u0" {
				res.notLeader++
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	res.window = time.Since(start)
	res.blocks = p.chain.head() - startHead
	res.leaderPolls = p.ups[0].latestCalls.Load() - startPolls
	for i, u := range p.ups {
		res.perUpstream[i], _ = callsDelta(before[i], u.snapshot())
		res.probes[i] = probeCount(p.nw, fmt.Sprintf("u%d", i), methods...) - probesBefore[i]
		if i > 0 {
			for _, n := range res.perUpstream[i] {
				res.extra[i] += n
			}
		}
	}
	mode := "fixed"
	if legacy {
		mode = "legacy"
	}
	totalExtra := res.extra[1] + res.extra[2] + res.extra[3]
	t.Logf("[%s] %d requests over %s (%d blocks), %d failed, %d not served by u0; leader polls on u0: %d (%.2f/s); non-leader upstream calls: u1=%v u2=%v u3=%v (%.2f calls/s); probes u1..u3: %v",
		mode, res.reqs, res.window.Round(time.Millisecond), res.blocks, res.fails, res.notLeader,
		res.leaderPolls, float64(res.leaderPolls)/res.window.Seconds(),
		res.perUpstream[1], res.perUpstream[2], res.perUpstream[3], float64(totalExtra)/res.window.Seconds(), res.probes[1:])
	return res
}

func fastChainWindow() time.Duration {
	if v := os.Getenv("ERPC_TEST_FASTCHAIN_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	if testing.Short() {
		return 5 * time.Second
	}
	return 15 * time.Second
}

// Spec (head lag on demand): on a fast (0.1s) tracked chain whose non-leader
// upstreams have ~stale poller views, steady tip traffic is served by the
// leader-served upstream with NO extra upstream calls: no lag exclusion, no
// shadow probes, no tip checks (nothing is routed past u0). The legacy run
// (pre-fix policy view) is measured for comparison and must show the probe
// fan-out the fix removes.
func TestHeadTracker_E2E_FastChainNoProbeFanOut(t *testing.T) {
	window := fastChainWindow()
	var legacy fastChainResult
	t.Run("legacy", func(t *testing.T) { legacy = runFastChainTraffic(t, true, window) })
	var fixed fastChainResult
	t.Run("fixed", func(t *testing.T) { fixed = runFastChainTraffic(t, false, window) })

	legacyExtra := legacy.extra[1] + legacy.extra[2] + legacy.extra[3]
	fixedExtra := fixed.extra[1] + fixed.extra[2] + fixed.extra[3]
	t.Logf("extra non-leader upstream calls/s: before %.2f, after %.2f; shadow probes: before %.0f, after %.0f",
		float64(legacyExtra)/legacy.window.Seconds(), float64(fixedExtra)/fixed.window.Seconds(),
		legacy.probes[1]+legacy.probes[2]+legacy.probes[3], fixed.probes[1]+fixed.probes[2]+fixed.probes[3])

	require.Positive(t, legacy.probes[2]+legacy.probes[3], "legacy: stale-lag exclusion mirrors client requests to u2/u3 (the regression this guards)")

	require.Zero(t, fixed.fails, "requests succeed")
	require.Zero(t, fixed.notLeader, "every request is served by the leader-served upstream u0")
	for i := 1; i < 4; i++ {
		require.Zero(t, fixed.probes[i], "u%d: zero shadow probes to a healthy upstream", i)
		require.Zero(t, fixed.perUpstream[i]["eth_getBlockByNumber"]+fixed.perUpstream[i]["eth_getLogs"],
			"u%d: no data calls (got %v)", i, fixed.perUpstream[i])
		// Only possible background calls: one 60s poller tick (eth_getBlockByNumber
		// latest/finalized + eth_syncing) and no tip checks (no traffic reaches it).
		require.LessOrEqual(t, fixed.perUpstream[i]["eth_blockNumber"], 0, "u%d: no tip checks without traffic", i)
	}
	// Leader polls: the head tracker on u0, about one per 500ms floor.
	require.LessOrEqual(t, float64(fixed.leaderPolls)/fixed.window.Seconds(), 3.0, "u0: leader polls stay ~2/s (500ms floor)")
}

// Spec: when the leader-served upstream cannot serve a tip request, exactly
// the next upstream in priority order gets ONE eth_blockNumber tip check and
// then the data request; no other upstream is contacted.
func TestHeadTracker_E2E_FailoverTipChecksOnlyNextInLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(u0 *timedChain)
	}{
		{"unsupported", func(u0 *timedChain) { u0.unsupportedLogs.Store(true) }},
		{"server-error", func(u0 *timedChain) { u0.failData.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startPriorityNet(t, 100*time.Millisecond, common.EvalScopeNetwork, 0, func(ups []*timedChain) { tc.setup(ups[0]) })
			// u1..u3's 60s poller views are now well behind the chain; freeze
			// the chain so the target block is deterministic.
			time.Sleep(2 * time.Second)
			p.chain.pinned.Store(p.chain.head())
			pinned := p.chain.pinned.Load()
			require.Eventually(t, func() bool { return trackerOf(t, p.rep).FreshHead() == pinned }, 5*time.Second, 10*time.Millisecond)
			for i := 1; i < 4; i++ {
				require.Less(t, p.upstream(t, i).EvmStatePoller().LatestBlock(), pinned-10, "u%d's known head is stale", i)
			}
			var before [4]map[string]int
			for i, u := range p.ups {
				before[i] = u.snapshot()
			}
			code, hdr, body := p.rep.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x%x","toBlock":"0x%x"}]}`, pinned-2, pinned), nil, nil)
			var r rpcResp
			require.NoError(t, json.Unmarshal([]byte(body), &r), body)
			require.Equal(t, 200, code, body)
			require.Empty(t, r.Error, body)
			var logs []json.RawMessage
			require.NoError(t, json.Unmarshal(r.Result, &logs))
			require.Len(t, logs, 3, "full range served")
			served := hdr["X-Erpc-Upstream"] + hdr["X-ERPC-Upstream"]
			require.Equal(t, "u1", served, "served by the next upstream in priority order")

			d0, _ := callsDelta(before[0], p.ups[0].snapshot())
			d1, _ := callsDelta(before[1], p.ups[1].snapshot())
			t.Logf("u0 %v, u1 %v", d0, d1)
			require.Equal(t, 1, d0["eth_getLogs"], "u0 tried once")
			require.Equal(t, 1, d1["eth_blockNumber"], "u1: exactly one tip check")
			require.Equal(t, 1, d1["eth_getLogs"], "u1: then the data request")
			for i := 2; i < 4; i++ {
				d, n := callsDelta(before[i], p.ups[i].snapshot())
				require.Zero(t, n, "u%d is not contacted (got %v)", i, d)
			}
		})
	}
}

// Probing for re-admission still works on a tracked network: upstreams
// excluded for errors get (sampled) probes, and a healed one is re-admitted
// and serves again. Upstreams that were never excluded get no probes.
func TestHeadTracker_E2E_ErrorExcludedUpstreamProbedAndReadmitted(t *testing.T) {
	p := startPriorityNet(t, 100*time.Millisecond, common.EvalScopeNetworkMethod, 3*time.Second, func(ups []*timedChain) {
		ups[0].failData.Store(true)
		ups[1].failData.Store(true)
	})
	call := func() string {
		code, hdr, body := p.rep.send(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000002","data":"0x"},"latest"]}`, nil, nil)
		require.Equal(t, 200, code, body)
		return hdr["X-Erpc-Upstream"] + hdr["X-ERPC-Upstream"]
	}
	excluded := func() []string {
		var out []string
		for _, u := range p.nw.policyEngine.GetExcluded(p.nw.networkId, "eth_call", "*") {
			out = append(out, u.Id())
		}
		return out
	}
	require.Eventually(t, func() bool {
		_ = call()
		ex := excluded()
		return len(ex) == 2
	}, 20*time.Second, 20*time.Millisecond, "u0 and u1 are excluded for errors")
	require.ElementsMatch(t, []string{"u0", "u1"}, excluded())
	require.Equal(t, "u2", call())

	probes1 := probeCount(p.nw, "u1", "eth_call")
	probes3 := probeCount(p.nw, "u3", "eth_call")
	p.ups[1].failData.Store(false)
	require.Eventually(t, func() bool {
		return call() == "u1"
	}, 20*time.Second, 20*time.Millisecond, "healed u1 is re-admitted and serves eth_call again")
	require.Greater(t, probeCount(p.nw, "u1", "eth_call"), probes1, "u1 was re-admitted via probes")
	require.Zero(t, probeCount(p.nw, "u3", "eth_call")-probes3, "the never-excluded u3 is not probed")
	require.Contains(t, excluded(), "u0", "u0 still failing, still excluded")
}
