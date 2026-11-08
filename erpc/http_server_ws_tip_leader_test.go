package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// setupTipLeaderServer starts a server with two upstreams, rpc1 ordered ahead
// of rpc2, each first given to with when set, and returns the network and
// rpc2.
func setupTipLeaderServer(t *testing.T, evmCfg *common.EvmNetworkConfig, maxAttempts int, with func(*common.UpstreamConfig)) (
	sendRequest func(body string, headers map[string]string, queryParams map[string]string) (int, map[string]string, string),
	shutdown func(),
	nw *Network,
	leader *upstream.Upstream,
) {
	t.Helper()
	upstreams := make([]*common.UpstreamConfig, 0, 2)
	for _, id := range []string{"rpc1", "rpc2"} {
		u := &common.UpstreamConfig{
			Id:       id,
			Endpoint: "http://" + id + ".localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(10 * time.Second),
			},
		}
		if with != nil {
			with(u)
		}
		upstreams = append(upstreams, u)
	}
	cfg := &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(100 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{{
			Id: "test_project",
			Networks: []*common.NetworkConfig{{
				Architecture: "evm",
				Evm:          evmCfg,
				Failsafe: []*common.FailsafeConfig{{
					Retry: &common.RetryPolicyConfig{MaxAttempts: maxAttempts},
				}},
			}},
			Upstreams: upstreams,
		}},
	}

	sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	policy.OverrideAllForTest(prj.policyEngine)
	policy.OverrideOrderForTest(prj.policyEngine, "evm:123", "rpc1", "rpc2")

	time.Sleep(500 * time.Millisecond)

	nw, err = prj.GetNetwork(context.Background(), "evm:123")
	require.NoError(t, err)
	for _, u := range nw.upstreamsRegistry.GetNetworkUpstreams(context.Background(), "evm:123") {
		if u.Id() == "rpc2" {
			leader = u
		}
	}
	require.NotNil(t, leader)
	return sendRequest, shutdown, nw, leader
}

// mockHashedLatest makes id's latest answer the hashed block body, so it
// forms the view (spec §6.5: the view is a pin hash votes back; a number
// alone never moves it), persistent: the block clock fetches latest
// again. The poller mock answering id's latest without a hash matches
// first, so it goes.
func mockHashedLatest(id, body string) {
	for _, m := range gock.GetAll() {
		probe, _ := http.NewRequest(http.MethodPost, "http://"+id+".localhost", strings.NewReader(`{"method":"eth_getBlockByNumber","params":["latest",false]}`))
		if ok, _ := m.Match(probe); ok {
			gock.Remove(m)
		}
	}
	gock.New("http://" + id + ".localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, `"latest"`)
		}).
		Reply(200).
		JSON([]byte(body))
}

// "latest" goes to the upstreams as the tag, by round trip (spec §6.4
// client read): rpc2 answers its tip at once, and rpc1's lagging answer,
// which the view does not take, makes the leg read the Head's number from
// rpc2, the upstream whose poller has the tip. Either way the client gets
// rpc2's tip block, never the lagging sibling's.
func TestHttpServer_GetBlockByNumberLatest_InterpolatedReachesTipUpstream(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForEvmStatePoller()
	mockHashedLatest("rpc2", `{"result":{"number":"0x22228889","hash":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","parentHash":"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","timestamp":"0x6702a8f1"}}`)
	defer util.AssertNoPendingMocks(t, 1) // the by-number mock persists: the tag read may serve without it

	const tip = int64(0x22228889)
	tipHex := "0x22228889"
	var leaderHits atomic.Int64
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			if !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, tipHex) {
				return false
			}
			leaderHits.Add(1)
			return true
		}).
		Reply(200).
		JSON([]byte(`{"result":{"number":"0x22228889","hash":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","parentHash":"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","timestamp":"0x6702a8f1"}}`))
	// rpc1 lags; "latest" becomes the tip's number. Quorum 1: rpc2's head
	// alone is the tip.
	sendRequest, shutdown, nw, leader := setupTipLeaderServer(t, &common.EvmNetworkConfig{
		ChainId: 123,
		Integrity: &common.EvmIntegrityConfig{
			EnforceHighestBlock: util.BoolPtr(true),
		},
		ServedTip: &common.EvmServedTipConfig{Quorum: 1},
	}, 3, nil)
	defer shutdown()

	require.Eventually(t, func() bool { return servedHead(nw, context.Background()).Latest == tip }, 5*time.Second, 50*time.Millisecond)

	require.Equal(t, tip, leader.EvmStatePoller().LatestBlock())
	require.Equal(t, "rpc2", servedHead(nw, context.Background()).Leader.Id())
	require.Equal(t, tip, servedHead(nw, context.Background()).Latest)

	statusCode, _, body := sendRequest(`{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_getBlockByNumber",
		"params": ["latest", false]
	}`, nil, nil)

	require.Equal(t, http.StatusOK, statusCode)

	var respObject map[string]interface{}
	require.NoError(t, sonic.UnmarshalString(body, &respObject))
	result, ok := respObject["result"].(map[string]interface{})
	require.True(t, ok, "response should have a result object, got: %s", body)
	assert.Equal(t, tipHex, result["number"])
	assert.Equal(t, "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", result["hash"])
	assert.LessOrEqual(t, leaderHits.Load(), int64(1), "at most the one by-number read of the Head's block")
}

// celo's shape (shadow trace f32c77ce, a read of the Head block): rpc2 is
// a legacy upstream whose counter holds the tip but whose configured
// exactBlock upper bound makes the gate skip it; rpc1, the voter that
// made the tip the Head, answers the tag with the tip's parent, which
// the view does not take, yet holds the tip by number. The leg reads the
// Head's block by number from rpc1 itself, its last holder left, and the
// client gets it: the tag answer is no result, so it neither consumes
// rpc1 for the leg nor for the network's retry.
func TestHttpServer_GetBlockByNumberLatest_TagMissReadsItsUpstreamByNumber(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForEvmStatePoller()
	const tip = int64(0x22228889)
	tipHex := "0x22228889"
	tipBody := `{"result":{"number":"0x22228889","hash":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","parentHash":"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","timestamp":"0x6702a8f1"}}`
	mockHashedLatest("rpc1", tipBody)
	mockHashedLatest("rpc2", tipBody)
	var byNumber atomic.Int64
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			if !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, tipHex) {
				return false
			}
			byNumber.Add(1)
			return true
		}).
		Reply(200).
		JSON([]byte(tipBody))
	exact := int64(0x100)
	sendRequest, shutdown, nw, gated := setupTipLeaderServer(t, &common.EvmNetworkConfig{
		ChainId:   123,
		ServedTip: &common.EvmServedTipConfig{Quorum: 1},
	}, 2, func(u *common.UpstreamConfig) {
		if u.Id == "rpc2" {
			u.Evm.BlockAvailability = &common.EvmBlockAvailabilityConfig{Upper: &common.EvmAvailabilityBoundConfig{ExactBlock: &exact}}
		}
	})
	defer shutdown()

	require.Eventually(t, func() bool { return servedHead(nw, context.Background()).Latest == tip }, 5*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool { return gated.EvmStatePoller().LatestBlock() == tip }, 5*time.Second, 50*time.Millisecond)
	// rpc1's node now answers latest with the tip's parent.
	mockHashedLatest("rpc1", `{"result":{"number":"0x22228888","hash":"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","parentHash":"0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","timestamp":"0x6702a8f0"}}`)
	before := byNumber.Load()

	statusCode, _, body := sendRequest(`{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_getBlockByNumber",
		"params": ["latest", false]
	}`, nil, nil)

	require.Equal(t, http.StatusOK, statusCode, "body=%s", body)
	var respObject map[string]interface{}
	require.NoError(t, sonic.UnmarshalString(body, &respObject))
	result, ok := respObject["result"].(map[string]interface{})
	require.True(t, ok, "response should have a result object, got: %s", body)
	assert.Equal(t, tipHex, result["number"])
	assert.Equal(t, "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", result["hash"])
	assert.GreaterOrEqual(t, byNumber.Load()-before, int64(1), "the Head's block read by number from rpc1")
}

// Direct eth_getBlockByNumber(tip) goes first to the upstream that has the
// tip, not to a lagging sibling preferred by selection order.
func TestHttpServer_GetBlockByNumber_NearTipPrefersTipUpstream(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForEvmStatePoller()
	mockHashedLatest("rpc2", `{"result":{"number":"0x33338889","hash":"0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","parentHash":"0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","timestamp":"0x6702a8f1"}}`)
	// rpc1 tip-null Persist mock is intentionally unused.
	defer util.AssertNoPendingMocks(t, 1)

	const tip = int64(0x33338889)
	tipHex := "0x33338889"
	var leaderHits atomic.Int64
	var laggingHits atomic.Int64

	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host != "rpc1.localhost" {
				return false
			}
			body := util.SafeReadBody(r)
			if strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, tipHex) {
				laggingHits.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(`{"result":null}`))

	gock.New("http://rpc2.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			if r.URL.Host != "rpc2.localhost" {
				return false
			}
			body := util.SafeReadBody(r)
			if strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, tipHex) {
				leaderHits.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(`{"result":{"number":"0x33338889","hash":"0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","parentHash":"0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","timestamp":"0x6702a8f1"}}`))

	// Quorum 1: rpc2's head alone is Latest, so rpc2 leads (the Leader is
	// the highest voter within Latest + maxRetryableBlockDistance; with the
	// default quorum one far-ahead upstream never leads, spec §6.4, U12).
	sendRequest, shutdown, nw, leader := setupTipLeaderServer(t, &common.EvmNetworkConfig{
		ChainId:   123,
		ServedTip: &common.EvmServedTipConfig{Quorum: 1},
	}, 2, nil)
	defer shutdown()
	require.Eventually(t, func() bool { return servedHead(nw, context.Background()).Latest == tip }, 5*time.Second, 50*time.Millisecond)
	require.Equal(t, tip, leader.EvmStatePoller().LatestBlock())
	require.Equal(t, "rpc2", servedHead(nw, context.Background()).Leader.Id())

	statusCode, _, body := sendRequest(`{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_getBlockByNumber",
		"params": ["`+tipHex+`", false]
	}`, nil, nil)

	require.Equal(t, http.StatusOK, statusCode, "body=%s", body)
	var respObject map[string]interface{}
	require.NoError(t, sonic.UnmarshalString(body, &respObject))
	result, ok := respObject["result"].(map[string]interface{})
	require.True(t, ok, "got: %s", body)
	assert.Equal(t, tipHex, result["number"])
	assert.GreaterOrEqual(t, leaderHits.Load(), int64(1),
		"near-tip getBlock must go to the upstream that has the tip")
	assert.Equal(t, int64(0), laggingHits.Load(),
		"lagging sibling must not receive the near-tip getBlock")
}
