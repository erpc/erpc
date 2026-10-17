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
// of rpc2, and returns the network and rpc2.
func setupTipLeaderServer(t *testing.T, evmCfg *common.EvmNetworkConfig, maxAttempts int) (
	sendRequest func(body string, headers map[string]string, queryParams map[string]string) (int, map[string]string, string),
	shutdown func(),
	nw *Network,
	leader *upstream.Upstream,
) {
	t.Helper()
	upstreams := make([]*common.UpstreamConfig, 0, 2)
	for _, id := range []string{"rpc1", "rpc2"} {
		upstreams = append(upstreams, &common.UpstreamConfig{
			Id:       id,
			Endpoint: "http://" + id + ".localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(10 * time.Second),
			},
		})
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

// EnforceHighestBlock's tip re-fetch must reach the upstream whose poller has
// the tip, not settle for the lagging sibling that answered "latest" first.
func TestHttpServer_GetBlockByNumberLatest_RefetchReachesTipUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	const tip = int64(0x22228889)
	tipHex := "0x22228889"
	var leaderHits atomic.Int64

	gock.New("http://rpc2.localhost").
		Post("").
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

	// rpc1 lags, so the initial "latest" comes from it and triggers the
	// EnforceHighestBlock re-fetch.
	sendRequest, shutdown, nw, leader := setupTipLeaderServer(t, &common.EvmNetworkConfig{
		ChainId: 123,
		Integrity: &common.EvmIntegrityConfig{
			EnforceHighestBlock: util.BoolPtr(true),
		},
	}, 3)
	defer shutdown()

	// Mirror WS ingest: the tip-source poller and the delivered-head floor
	// advance before any client sees the head.
	leader.EvmStatePoller().SuggestLatestBlock(tip)
	nw.NoteObservedLatestBlock(context.Background(), tip)

	require.Equal(t, tip, leader.EvmStatePoller().LatestBlock())
	require.Equal(t, "rpc2", nw.EvmLeaderUpstream(context.Background()).Id())
	require.Equal(t, tip, nw.EvmHighestLatestBlockNumber(context.Background()))

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
	assert.GreaterOrEqual(t, leaderHits.Load(), int64(1),
		"the tip re-fetch must reach the upstream that has the tip")
}

// Direct eth_getBlockByNumber(tip) goes first to the upstream that has the
// tip, not to a lagging sibling preferred by selection order.
func TestHttpServer_GetBlockByNumber_NearTipPrefersTipUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
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

	sendRequest, shutdown, nw, leader := setupTipLeaderServer(t, &common.EvmNetworkConfig{ChainId: 123}, 2)
	defer shutdown()
	leader.EvmStatePoller().SuggestLatestBlock(tip)
	require.Equal(t, "rpc2", nw.EvmLeaderUpstream(context.Background()).Id())

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
