package erpc

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/util"
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
	nw.NoteObservedLatestHead(context.Background(), tip, wsHeader)
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
