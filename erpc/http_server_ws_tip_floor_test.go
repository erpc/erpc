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
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// Reproduces the tip race that trips consumers when HTTP "latest" lags a
// WS newHeads tip already delivered on the same pod: the only HTTP upstream
// still serves N, TipHW/cache is N+1 from WS, and re-fetch of N+1 would fail.
// eth_getBlockByNumber("latest", false) must return the cached WS header.
func TestHttpServer_GetBlockByNumberLatest_UsesCachedWsTipWhenHttpLags(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

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
				},
			},
		},
	}

	sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()

	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	policy.OverrideAllForTest(prj.policyEngine)

	// Let state poller settle at 0x11118888 (SetupMocksForEvmStatePoller).
	time.Sleep(500 * time.Millisecond)

	nw, err := prj.GetNetwork(context.Background(), "evm:123")
	require.NoError(t, err)

	// WS path noted tip N+1 before fan-out; HTTP upstream still only has N.
	const tip = int64(0x11118889)
	wsHeader := []byte(`{"number":"0x11118889","hash":"0xwshead","parentHash":"0xwsparent","timestamp":"0x6702a8f1"}`)
	nw.NoteObservedLatestHead(context.Background(), tip, wsHeader, "rpc1")
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
	assert.Equal(t, "0x11118889", result["number"],
		"HTTP latest must not regress below the WS tip already noted on this pod")
	assert.Equal(t, "0xwshead", result["hash"])
}

// Tip-aware routing: after WS ingest advances TipHW + the tip-source poller,
// eth_getBlockByNumber("latest") must prefer that tip-source upstream over a
// lagging HTTP sibling (partition), and EnforceHighestBlock must pin the
// concrete tip re-fetch to it when the first response is still stale.
func TestHttpServer_GetBlockByNumberLatest_PinsReFetchToTipSourceUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	const tip = int64(0x22228889)
	tipHex := "0x22228889"
	var tipSourceHits atomic.Int64

	gock.New("http://rpc2.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			if !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, tipHex) {
				return false
			}
			tipSourceHits.Add(1)
			return true
		}).
		Reply(200).
		JSON([]byte(`{"result":{"number":"0x22228889","hash":"0xtipsrc","parentHash":"0xparent","timestamp":"0x6702a8f1"}}`))

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
	// Prefer lagging HTTP first so EnforceHighestBlock re-fetch pin is exercised
	// even when partition would otherwise put the tip-source first.
	policy.OverrideOrderForTest(prj.policyEngine, "evm:123", "rpc1", "rpc2")

	time.Sleep(500 * time.Millisecond)

	nw, err := prj.GetNetwork(context.Background(), "evm:123")
	require.NoError(t, err)

	// Advance TipHW + tip-source id WITHOUT a cached header and WITHOUT
	// bumping rpc2's poller. That forces EnforceHighestBlock to pin the
	// concrete tip re-fetch to rpc2 (cache cannot short-circuit; partition
	// cannot reorder rpc2 ahead).
	nw.NoteObservedLatestHead(context.Background(), tip, nil, "rpc2")

	require.Equal(t, tip, nw.EvmHighestLatestBlockNumber(context.Background()))
	cachedNum, cachedPayload, tipSrc := nw.LastObservedLatestHead()
	require.Equal(t, tip, cachedNum)
	require.Empty(t, cachedPayload)
	require.Equal(t, "rpc2", tipSrc)

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
	assert.Equal(t, "0xtipsrc", result["hash"])
	assert.GreaterOrEqual(t, tipSourceHits.Load(), int64(1),
		"EnforceHighestBlock must pin the concrete tip re-fetch to the tip-source upstream")
}
