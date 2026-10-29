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
// and the eth_calls it served.
type minBlockNode struct {
	head  atomic.Int64
	polls atomic.Int64
	calls atomic.Int64
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
		Reply(200).Map(func(res *http.Response) *http.Response { return reply(res, header(n.head.Load())) })
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
		start, timeouts := time.Now(), outcome("timeout")
		status, _, a := ask(minBlock("104"), nil)
		assert.Equal(t, http.StatusServiceUnavailable, status)
		require.NotNil(t, a.Error, "result %q", a.Result)
		assert.Equal(t, -32015, a.Error.Code)
		assert.Equal(t, "block 104 not reached", a.Error.Message)
		assert.JSONEq(t, `{"minBlock":"0x68","latest":"0x66"}`, string(a.Error.Data))
		assert.GreaterOrEqual(t, time.Since(start), 2500*time.Millisecond, "refused once W ended")
		assert.Equal(t, timeouts+1, outcome("timeout"))
	})

	t.Run("FarFutureRefusedWithoutDispatch", func(t *testing.T) {
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
		later(400*time.Millisecond, 103, "slow-1", "slow-2")
		status, hdrs, a := ask(lane("103"), nil)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, int64(103), a.block(t), "the slow lane's Latest waited for 103")
		assert.Contains(t, hdrs["X-Erpc-Upstream"], "slow-")

		// A selector forming no lane waits on its stateless pick.
		later(400*time.Millisecond, 104, "slow-1", "slow-2")
		status, hdrs, a = ask(map[string]string{"X-ERPC-Min-Block": "104", "X-ERPC-Use-Upstream": "slow-1|slow-2"}, nil)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, int64(104), a.block(t))
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
}
