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

// TipHW advanced via WS must not fail-open to a stale HTTP "latest" when tip
// re-fetch cannot reach TipHW. Prefer an error over demoting MultiNode FOOS.
func TestHttpServer_GetBlockByNumberLatest_RefusesStaleWhenTipRefetchMisses(t *testing.T) {
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

	const tip = int64(0x11118889)
	nw.NoteObservedLatestBlock(context.Background(), tip)
	require.Equal(t, tip, nw.EvmHighestLatestBlockNumber(context.Background()))

	statusCode, _, body := sendRequest(`{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_getBlockByNumber",
		"params": ["latest", false]
	}`, nil, nil)

	var respObject map[string]interface{}
	require.NoError(t, sonic.UnmarshalString(body, &respObject))

	// Upstream only has 0x11118888; TipHW is one ahead. Must not return the
	// stale header as success.
	if statusCode == http.StatusOK {
		if result, ok := respObject["result"].(map[string]interface{}); ok {
			assert.NotEqual(t, "0x11118888", result["number"],
				"must not fail-open to stale latest below TipHW; body=%s", body)
		}
	} else {
		_, hasErr := respObject["error"]
		assert.True(t, hasErr, "non-OK response should carry a JSON-RPC error; body=%s", body)
	}
}
