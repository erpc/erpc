package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minBlockNode is one vendor node: its head, the head polls it answered
// and the eth_calls it served. A head poll waits while stall holds a
// channel, until it is closed: a node whose link hangs.
type minBlockNode struct {
	head  atomic.Int64
	polls atomic.Int64
	calls atomic.Int64
	stall atomic.Pointer[chan struct{}]
}

var minBlockCallBlock = regexp.MustCompile(`"(0x[0-9a-fA-F]+|latest)"\s*\]`)

// mockMinBlockNode answers as a node at n.head: block headers by tag,
// number or hash (hash = the number, padded), and every eth_call with the
// block it executes at, as Multicall3 getBlockNumber() does.
func mockMinBlockNode(id string, n *minBlockNode) {
	host := "http://" + id + ".localhost"
	header := func(num int64) string {
		return fmt.Sprintf(`{"number":"0x%x","hash":"0x%064x","parentHash":"0x%064x","timestamp":"0x%x"}`, num, num, num-1, 0x6702a8f0+num)
	}
	reply := func(res *http.Response, result string) *http.Response {
		body := `{"jsonrpc":"2.0","id":1,"result":` + result + `}`
		res.Body = io.NopCloser(strings.NewReader(body))
		res.ContentLength = int64(len(body))
		return res
	}
	has := func(r *http.Request, parts ...string) bool {
		b := util.SafeReadBody(r)
		for _, p := range parts {
			if !strings.Contains(b, p) {
				return false
			}
		}
		return r.URL.Host == id+".localhost"
	}
	gock.New(host).Post("").Persist().Filter(func(r *http.Request) bool { return has(r, "eth_chainId") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x7b"}`))
	gock.New(host).Post("").Persist().Filter(func(r *http.Request) bool { return has(r, "eth_syncing") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":false}`))
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool {
			if !has(r, "eth_getBlockByNumber", `"latest"`) {
				return false
			}
			n.polls.Add(1)
			return true
		}).
		Reply(200).Map(func(res *http.Response) *http.Response {
		if c := n.stall.Load(); c != nil {
			<-*c
		}
		return reply(res, header(n.head.Load()))
	})
	gock.New(host).Post("").Persist().Filter(func(r *http.Request) bool { return has(r, "eth_getBlockByNumber", `"finalized"`) }).
		Reply(200).Map(func(res *http.Response) *http.Response { return reply(res, header(n.head.Load()-10)) })
	gock.New(host).Post("").Persist().Filter(func(r *http.Request) bool { return has(r, "eth_getBlockByNumber", `"safe"`) }).
		Reply(200).Map(func(res *http.Response) *http.Response { return reply(res, header(n.head.Load()-5)) })
	byNumber := regexp.MustCompile(`"(0x[0-9a-fA-F]{1,16})"`)
	byHash := regexp.MustCompile(`"0x([0-9a-fA-F]{64})"`)
	var asked sync.Map
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			if r.URL.Host != id+".localhost" || !strings.Contains(b, "eth_getBlockBy") {
				return false
			}
			var num int64 = -1
			if m := byHash.FindStringSubmatch(b); m != nil && strings.Contains(b, "eth_getBlockByHash") {
				num, _ = strconv.ParseInt(m[1], 16, 64)
			} else if m := byNumber.FindStringSubmatch(b); m != nil {
				num, _ = strconv.ParseInt(m[1][2:], 16, 64)
			}
			asked.Store(r, num)
			return true
		}).
		Reply(200).Map(func(res *http.Response) *http.Response {
		v, _ := asked.LoadAndDelete(res.Request)
		num, _ := v.(int64)
		if num < 0 || num > n.head.Load() {
			return reply(res, "null")
		}
		return reply(res, header(num))
	})
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			if r.URL.Host != id+".localhost" || !strings.Contains(b, "eth_call") {
				return false
			}
			n.calls.Add(1)
			at := n.head.Load()
			if m := minBlockCallBlock.FindStringSubmatch(b); m != nil && m[1] != "latest" {
				at, _ = strconv.ParseInt(m[1][2:], 16, 64)
			}
			asked.Store(r, at)
			return true
		}).
		Reply(200).Map(func(res *http.Response) *http.Response {
		v, _ := asked.LoadAndDelete(res.Request)
		at, _ := v.(int64)
		return reply(res, fmt.Sprintf(`"0x%064x"`, at))
	})
}

// minBlockAnswer is one JSON-RPC answer as a client decodes it.
type minBlockAnswer struct {
	Result string `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

func (a minBlockAnswer) block(t *testing.T) int64 {
	t.Helper()
	require.Nil(t, a.Error, "answer: %+v", a.Error)
	v, err := strconv.ParseInt(strings.TrimPrefix(a.Result, "0x"), 16, 64)
	require.NoError(t, err, "result %q", a.Result)
	return v
}

// TestHttpServer_MinBlock drives X-ERPC-Min-Block (streams spec §8) through
// the HTTP server over four vendor nodes whose heads the test moves: fast-1,
// fast-2 and the lane slow-*. Their state polls run every 30s, so eRPC
// learns a head only by an Ask. Every eth_call answers the block it
// executes at, so the client sees the block that served it.
func TestHttpServer_MinBlock(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ids := []string{"fast-1", "fast-2", "slow-1", "slow-2"}
	nodes := map[string]*minBlockNode{}
	for _, id := range ids {
		n := &minBlockNode{}
		n.head.Store(100)
		nodes[id] = n
		mockMinBlockNode(id, n)
	}
	setHeads := func(head int64, ids ...string) {
		for _, id := range ids {
			nodes[id].head.Store(head)
		}
	}
	calls := func() (sum int64) {
		for _, n := range nodes {
			sum += n.calls.Load()
		}
		return sum
	}

	upstreams := make([]*common.UpstreamConfig, 0, len(ids))
	for _, id := range ids {
		upstreams = append(upstreams, &common.UpstreamConfig{
			Id: id, Endpoint: "http://" + id + ".localhost", Type: common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(30 * time.Second)},
		})
	}
	cfg := &common.Config{
		Server: &common.ServerConfig{MaxTimeout: common.Duration(10 * time.Second).Ptr()},
		Projects: []*common.ProjectConfig{{
			Id:        "test_project",
			Networks:  []*common.NetworkConfig{{Architecture: "evm", Evm: &common.EvmNetworkConfig{ChainId: 123}}},
			Upstreams: upstreams,
		}},
		Database: &common.DatabaseConfig{EvmJsonRpcCache: &common.CacheConfig{
			Connectors: []*common.ConnectorConfig{{
				Id: "mem", Driver: common.DriverMemory,
				Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
			}},
			Policies: []*common.CachePolicyConfig{{
				Network: "*", Method: "eth_call", Finality: common.DataFinalityStateUnfinalized,
				Connector: "mem", TTL: common.FixedDuration(time.Minute),
			}},
		}},
	}
	send, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()
	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	ntw, err := prj.GetNetwork(context.Background(), util.EvmNetworkId(123))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return servedHead(ntw, context.Background()).Latest == 100 },
		5*time.Second, 50*time.Millisecond, "eRPC learns the nodes' head 100")
	// bt 1s, W 3s: before the first sample no block is future (CT §6.4).
	sampleBlockTime(t, ntw)

	const call = `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xca11bde05977b3631167028862be2a173976ca11","data":"0x42cbb15c"},"latest"]}`
	ask := func(headers, query map[string]string) (int, map[string]string, minBlockAnswer) {
		t.Helper()
		status, hdrs, body := send(call, headers, query)
		var a minBlockAnswer
		require.NoError(t, json.Unmarshal([]byte(body), &a), body)
		return status, hdrs, a
	}
	minBlock := func(n string) map[string]string { return map[string]string{"X-ERPC-Min-Block": n} }
	later := func(d time.Duration, head int64, ids ...string) { time.AfterFunc(d, func() { setHeads(head, ids...) }) }
	outcome := func(o string) float64 {
		return promUtil.ToFloat64(telemetry.MetricMinBlockTotal.WithLabelValues("test_project", ntw.Label(), o))
	}
	critical := func() float64 {
		return metricSum(telemetry.MetricNetworkFailedRequests, map[string]string{"network": ntw.Label(), "severity": "critical"})
	}
	stall := func(ids ...string) (release func()) {
		c := make(chan struct{})
		for _, id := range ids {
			nodes[id].stall.Store(&c)
		}
		return func() {
			for _, id := range ids {
				nodes[id].stall.Store(nil)
			}
			close(c)
		}
	}

	t.Run("ServedAtOrAfterN_CacheKeyedByWaitedBlock", func(t *testing.T) {
		_, _, a := ask(nil, nil)
		require.Equal(t, int64(100), a.block(t), "latest at 100, now cached")
		setHeads(101, ids...)
		ready := outcome("waited") + outcome("ready")
		status, _, a := ask(minBlock("101"), nil)
		assert.Equal(t, ready+1, outcome("waited")+outcome("ready"))
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, int64(101), a.block(t), "a block streamed at 101 is read at 101, never the cached 100")
	})

	t.Run("WaitsUpToWForLatest", func(t *testing.T) {
		later(400*time.Millisecond, 102, ids...)
		start, waited := time.Now(), outcome("waited")
		status, _, a := ask(nil, map[string]string{"min-block": "0x66"})
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, int64(102), a.block(t), "served once Latest reaches 102")
		assert.GreaterOrEqual(t, time.Since(start), 350*time.Millisecond, "it waited for the chain")
		assert.Less(t, time.Since(start), 3*time.Second, "within W")
		assert.Equal(t, waited+1, outcome("waited"))
	})

	t.Run("RefusedPastW", func(t *testing.T) {
		start, timeouts, alerts := time.Now(), outcome("timeout"), critical()
		status, _, a := ask(minBlock("104"), nil)
		assert.Equal(t, http.StatusServiceUnavailable, status)
		require.NotNil(t, a.Error, "result %q", a.Result)
		assert.Equal(t, -32015, a.Error.Code)
		assert.Equal(t, "block 104 not reached", a.Error.Message)
		assert.JSONEq(t, `{"minBlock":"0x68","latest":"0x66"}`, string(a.Error.Data))
		assert.GreaterOrEqual(t, time.Since(start), 2500*time.Millisecond, "refused once W ended")
		assert.Equal(t, timeouts+1, outcome("timeout"))
		assert.Equal(t, alerts, critical(), "a block the client asked too early for is no critical failure")
	})

	t.Run("FarFutureRefusedWithoutDispatch", func(t *testing.T) {
		// The last Ask is a block time old: the Ask of the stale reports
		// dates them fresh, whether or not the chain made a block (on a
		// halted chain too), and the block is future: refused at once.
		setHeads(103, ids...)
		time.Sleep(1100 * time.Millisecond)
		before, future := calls(), outcome("future")
		start := time.Now()
		status, _, a := ask(minBlock("1000000"), nil)
		assert.Equal(t, http.StatusServiceUnavailable, status)
		require.NotNil(t, a.Error, "result %q", a.Result)
		assert.Equal(t, -32015, a.Error.Code)
		assert.Less(t, time.Since(start), time.Second, "no wait for a block far above every head")
		assert.Equal(t, before, calls(), "no dispatch")
		assert.Equal(t, future+1, outcome("future"))
	})

	t.Run("WaitsOnTheRequestsLane", func(t *testing.T) {
		setHeads(104, "fast-1", "fast-2")
		_, _, a := ask(minBlock("104"), nil)
		require.Equal(t, int64(104), a.block(t), "network-wide Latest moves with the fast nodes")

		lane := func(n string) map[string]string {
			return map[string]string{"X-ERPC-Min-Block": n, "X-ERPC-Use-Upstream": "slow-*"}
		}
		// Each step waits for a head above the slow nodes' own, so its
		// timer has fired before the next step sets its own: two timers
		// due together fire in either order.
		later(400*time.Millisecond, 104, "slow-1", "slow-2")
		status, hdrs, a := ask(lane("104"), nil)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, int64(104), a.block(t), "the slow lane's Latest waited for 104")
		assert.Contains(t, hdrs["X-Erpc-Upstream"], "slow-")

		// A selector forming no lane waits on its stateless pick.
		later(400*time.Millisecond, 105, "slow-1", "slow-2")
		status, hdrs, a = ask(map[string]string{"X-ERPC-Min-Block": "105", "X-ERPC-Use-Upstream": "slow-1|slow-2"}, nil)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, int64(105), a.block(t))
		assert.Contains(t, hdrs["X-Erpc-Upstream"], "slow-")

		// A batch: each call at or after n on the lane, one Ask round for all.
		var batch []string
		for i := range 20 {
			batch = append(batch, strings.Replace(call, `"id":1`, fmt.Sprintf(`"id":%d`, i+1), 1))
		}
		polls0 := nodes["slow-1"].polls.Load() + nodes["slow-2"].polls.Load()
		later(400*time.Millisecond, 106, "slow-1", "slow-2")
		status, _, body := send("["+strings.Join(batch, ",")+"]", lane("106"), nil)
		require.Equal(t, http.StatusOK, status, body)
		var answers []minBlockAnswer
		require.NoError(t, json.Unmarshal([]byte(body), &answers), body)
		require.Len(t, answers, 20)
		for _, a := range answers {
			assert.Equal(t, int64(106), a.block(t))
		}
		polls := nodes["slow-1"].polls.Load() + nodes["slow-2"].polls.Load() - polls0
		assert.LessOrEqual(t, polls, int64(10), "20 waiting calls share each Ask round of the lane's 2 voters")
	})

	t.Run("StaleReportsAskedBeforeFuture", func(t *testing.T) {
		// The nodes moved 20 blocks since eRPC last heard them: far above
		// the ceiling it holds, yet its reports are older than 1 bt, so they
		// are Asked before the block is called future (CT §6.4).
		setHeads(130, ids...)
		status, _, a := ask(minBlock("130"), nil)
		require.Equal(t, http.StatusOK, status, "%+v", a.Error)
		assert.Equal(t, int64(130), a.block(t))
	})

	t.Run("FinalizedAndSafeUnaffected", func(t *testing.T) {
		// Neither tag reads Latest: the header neither holds nor refuses
		// it (streams spec §8), though 131 is above every head.
		for _, tag := range []string{"finalized", "safe"} {
			start := time.Now()
			status, _, body := send(strings.Replace(call, `"latest"`, `"`+tag+`"`, 1), minBlock("131"), nil)
			require.Equal(t, http.StatusOK, status, "%s: %s", tag, body)
			var a minBlockAnswer
			require.NoError(t, json.Unmarshal([]byte(body), &a), body)
			a.block(t)
			assert.Less(t, time.Since(start), time.Second, "%s: no wait for block 131", tag)
		}
	})

	t.Run("MalformedValueRefused", func(t *testing.T) {
		// Served without its block, the answer would look like one at or
		// after it: a value that is no block number is refused.
		before := calls()
		for _, v := range []string{"abc", "0x", "-5", "0x-1"} {
			status, _, a := ask(minBlock(v), nil)
			assert.Equal(t, http.StatusBadRequest, status, v)
			require.NotNil(t, a.Error, "%s: result %q", v, a.Result)
			assert.Equal(t, -32602, a.Error.Code, v)
		}
		status, _, a := ask(nil, map[string]string{"min-block": "1e3"})
		assert.Equal(t, http.StatusBadRequest, status)
		require.NotNil(t, a.Error, "result %q", a.Result)
		assert.Equal(t, -32602, a.Error.Code)
		assert.Equal(t, before, calls(), "no dispatch")
	})

	t.Run("OrderedByMinBlockOnlyForLatestSentAsIs", func(t *testing.T) {
		// 132 is within the near-future bound of 130 (3 blocks at 1s, W 3s).
		setHeads(132, "fast-1", "fast-2")
		_, _, a := ask(minBlock("132"), nil)
		require.Equal(t, int64(132), a.block(t), "eRPC hears fast-* at 132, slow-* at 130")
		ups := map[string]common.Upstream{}
		for _, u := range ntw.AllUpstreams() {
			ups[u.Id()] = u
		}
		order := func(body string, d *common.RequestDirectives) []string {
			req := common.NewNormalizedRequest([]byte(body))
			req.SetNetwork(ntw)
			req.SetDirectives(d)
			method, err := req.Method()
			require.NoError(t, err)
			var out []string
			for _, u := range ntw.preferUpstreams(context.Background(), req, method, []common.Upstream{ups["slow-1"], ups["fast-1"]}) {
				out = append(out, u.Id())
			}
			return out
		}
		old := `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x64",false]}`
		assert.Equal(t, []string{"slow-1", "fast-1"}, order(old, &common.RequestDirectives{MinBlock: 132}),
			"block 100 is on both: the policy's order, never the tip's")
		assert.Equal(t, []string{"fast-1", "slow-1"}, order(call, &common.RequestDirectives{MinBlock: 132, SkipInterpolation: true}),
			"latest sent as is: upstreams at or past 132 first")
	})

	t.Run("OneSlowVoterHoldsNoRequest", func(t *testing.T) {
		// fast-1 and fast-2 are the quorum at 133; slow-1's head poll
		// hangs. The request goes on once Latest has 133 (spec §8).
		release := stall("slow-1")
		defer release()
		time.Sleep(1100 * time.Millisecond)
		setHeads(133, "fast-1", "fast-2")
		start, waited := time.Now(), outcome("waited")
		status, _, a := ask(minBlock("133"), nil)
		require.Equal(t, http.StatusOK, status, "%+v", a.Error)
		assert.Equal(t, int64(133), a.block(t))
		assert.Less(t, time.Since(start), 2*time.Second, "never waits for every Ask, at most W (3s)")
		assert.Equal(t, waited+1, outcome("waited"))
	})

	t.Run("OneWForWaitAndRefresh", func(t *testing.T) {
		// slow-1|slow-2 forms no lane and both hang: the min-block wait
		// ends at W, and the latest refresh of a read that answers Latest
		// (§6.8), on reports older than W, gets none of its own.
		release := stall("slow-1", "slow-2")
		defer release()
		start := time.Now()
		status, _, body := send(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`,
			map[string]string{"X-ERPC-Min-Block": "131", "X-ERPC-Use-Upstream": "slow-1|slow-2"}, nil)
		took := time.Since(start)
		var a minBlockAnswer
		require.NoError(t, json.Unmarshal([]byte(body), &a), body)
		assert.Equal(t, http.StatusServiceUnavailable, status)
		require.NotNil(t, a.Error, "result %q", a.Result)
		assert.Equal(t, -32015, a.Error.Code)
		assert.Less(t, took, 3*time.Second+700*time.Millisecond, "one W (3s), never two")
	})
}
