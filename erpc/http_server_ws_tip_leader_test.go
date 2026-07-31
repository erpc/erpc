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

// EnforceHighestBlock must re-fetch the concrete tip via EvmLeaderUpstream
// (the poller advanced by SuggestLatestBlock), not fail-open to a lagging
// sibling that answered "latest" first.
func TestHttpServer_GetBlockByNumberLatest_RefetchPinsEvmLeaderUpstream(t *testing.T) {
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

	cfg := &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(100 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: "evm",
						Evm: &common.EvmNetworkConfig{
							ChainId: 123,
							Integrity: &common.EvmIntegrityConfig{
								EnforceHighestBlock: util.BoolPtr(true),
							},
						},
						Failsafe: []*common.FailsafeConfig{
							{
								Retry: &common.RetryPolicyConfig{MaxAttempts: 3},
							},
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "rpc1",
						Endpoint: "http://rpc1.localhost",
						Type:     common.UpstreamTypeEvm,
						Evm: &common.EvmUpstreamConfig{
							ChainId:             123,
							StatePollerInterval: common.Duration(10 * time.Second),
						},
					},
					{
						Id:       "rpc2",
						Endpoint: "http://rpc2.localhost",
						Type:     common.UpstreamTypeEvm,
						Evm: &common.EvmUpstreamConfig{
							ChainId:             123,
							StatePollerInterval: common.Duration(10 * time.Second),
						},
					},
				},
			},
		},
	}

	sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()

	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	policy.OverrideAllForTest(prj.policyEngine)
	// Prefer lagging rpc1 for the initial "latest" so EnforceHighestBlock re-fetch runs.
	policy.OverrideOrderForTest(prj.policyEngine, "evm:123", "rpc1", "rpc2")

	time.Sleep(500 * time.Millisecond)

	nw, err := prj.GetNetwork(context.Background(), "evm:123")
	require.NoError(t, err)

	var leader *upstream.Upstream
	for _, u := range nw.upstreamsRegistry.GetNetworkUpstreams(context.Background(), "evm:123") {
		if u.Id() == "rpc2" {
			leader = u
			break
		}
	}
	require.NotNil(t, leader)

	// Mirror WS ingest: tip-source poller + TipHW before any client sees the head.
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
		"EnforceHighestBlock must pin the tip re-fetch to EvmLeaderUpstream")
}

// Direct eth_getBlockByNumber(tip) must pin to EvmLeaderUpstream on first
// forward (same idea as EnforceHighestBlock tip re-fetch), not hit a lagging
// sibling that is preferred by selection order.
func TestHttpServer_GetBlockByNumber_NearTipPinsEvmLeaderUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	// rpc1 tip-null Persist mock is intentionally unused when the pin works.
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

	cfg := &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(100 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: "evm",
						Evm: &common.EvmNetworkConfig{
							ChainId: 123,
						},
						Failsafe: []*common.FailsafeConfig{
							{
								Retry: &common.RetryPolicyConfig{MaxAttempts: 2},
							},
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "rpc1",
						Endpoint: "http://rpc1.localhost",
						Type:     common.UpstreamTypeEvm,
						Evm: &common.EvmUpstreamConfig{
							ChainId:             123,
							StatePollerInterval: common.Duration(10 * time.Second),
						},
					},
					{
						Id:       "rpc2",
						Endpoint: "http://rpc2.localhost",
						Type:     common.UpstreamTypeEvm,
						Evm: &common.EvmUpstreamConfig{
							ChainId:             123,
							StatePollerInterval: common.Duration(10 * time.Second),
						},
					},
				},
			},
		},
	}

	sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()

	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	policy.OverrideAllForTest(prj.policyEngine)
	policy.OverrideOrderForTest(prj.policyEngine, "evm:123", "rpc1", "rpc2")

	time.Sleep(500 * time.Millisecond)

	nw, err := prj.GetNetwork(context.Background(), "evm:123")
	require.NoError(t, err)

	var leader *upstream.Upstream
	for _, u := range nw.upstreamsRegistry.GetNetworkUpstreams(context.Background(), "evm:123") {
		if u.Id() == "rpc2" {
			leader = u
			break
		}
	}
	require.NotNil(t, leader)
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
		"near-tip getBlock must pin to EvmLeaderUpstream")
	assert.Equal(t, int64(0), laggingHits.Load(),
		"lagging sibling must not receive the pinned near-tip getBlock")
}

// When TipHW is ahead of every upstream's concrete block response,
// EnforceHighestBlock must NOT fail-open to the stale "latest" — that is
// the MultiNode FOOS trigger once WS has already delivered the higher head.
func TestHttpServer_GetBlockByNumberLatest_RefusesStaleFailOpen(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	// Two Persist tip-null mocks remain pending by design.
	defer util.AssertNoPendingMocks(t, 2)

	const tip = int64(0x22228889)
	tipHex := "0x22228889"
	staleHex := "0x22228888"

	// Tip re-fetch always misses (null) — pinned and unconstrained paths.
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, tipHex)
		}).
		Reply(200).
		JSON([]byte(`{"result":null}`))
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, tipHex)
		}).
		Reply(200).
		JSON([]byte(`{"result":null}`))

	cfg := &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(100 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: "evm",
						Evm: &common.EvmNetworkConfig{
							ChainId: 123,
							Integrity: &common.EvmIntegrityConfig{
								EnforceHighestBlock: util.BoolPtr(true),
							},
						},
						Failsafe: []*common.FailsafeConfig{
							{
								Retry: &common.RetryPolicyConfig{MaxAttempts: 2},
							},
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "rpc1",
						Endpoint: "http://rpc1.localhost",
						Type:     common.UpstreamTypeEvm,
						Evm: &common.EvmUpstreamConfig{
							ChainId:             123,
							StatePollerInterval: common.Duration(10 * time.Second),
						},
					},
					{
						Id:       "rpc2",
						Endpoint: "http://rpc2.localhost",
						Type:     common.UpstreamTypeEvm,
						Evm: &common.EvmUpstreamConfig{
							ChainId:             123,
							StatePollerInterval: common.Duration(10 * time.Second),
						},
					},
				},
			},
		},
	}

	sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()

	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	policy.OverrideAllForTest(prj.policyEngine)
	policy.OverrideOrderForTest(prj.policyEngine, "evm:123", "rpc1", "rpc2")

	time.Sleep(500 * time.Millisecond)

	nw, err := prj.GetNetwork(context.Background(), "evm:123")
	require.NoError(t, err)

	var leader *upstream.Upstream
	for _, u := range nw.upstreamsRegistry.GetNetworkUpstreams(context.Background(), "evm:123") {
		if u.Id() == "rpc2" {
			leader = u
			break
		}
	}
	require.NotNil(t, leader)
	leader.EvmStatePoller().SuggestLatestBlock(tip)
	nw.NoteObservedLatestBlock(context.Background(), tip)

	statusCode, _, body := sendRequest(`{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_getBlockByNumber",
		"params": ["latest", false]
	}`, nil, nil)

	var respObject map[string]interface{}
	require.NoError(t, sonic.UnmarshalString(body, &respObject))
	if result, ok := respObject["result"].(map[string]interface{}); ok {
		require.NotEqual(t, staleHex, result["number"],
			"must not fail-open to stale tip below TipHW; status=%d body=%s", statusCode, body)
		require.NotEqual(t, tipHex, result["number"],
			"tip was mocked as null; unexpected tip success: %s", body)
	}
	_, hasErr := respObject["error"]
	require.True(t, hasErr || statusCode >= 400,
		"expected error when tip re-fetch cannot reach TipHW, got status=%d body=%s", statusCode, body)
}
