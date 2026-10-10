package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// hl-node's reply when a HyperEVM read precompile (0x…0800 onwards) runs
// against a block for which the node holds no HyperCore state. Many nodes hold
// that state only for their live tip, so any older block gets this reply,
// while a node that holds the state answers the same call.
const precompileStateGapMessage = "out of gas: gas exhausted during precompiled contract execution: 600000000"

const oraclePxCall = `{"to":"0x0000000000000000000000000000000000000807","data":"0x0000000000000000000000000000000000000000000000000000000000000000"}`

func precompileStateGapReply() map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"error":   map[string]interface{}{"code": -32003, "message": precompileStateGapMessage},
	}
}

func TestNetworkForward_PrecompileStateGap_FailsOverToUpstreamWithState(t *testing.T) {
	cases := []struct {
		method string
		params string
		result string
	}{
		{"eth_call", `[` + oraclePxCall + `,"0x11118800"]`,
			`"0x0000000000000000000000000000000000000000000000000000000000012345"`},
		{"eth_estimateGas", `[` + oraclePxCall + `,"0x11118800"]`,
			`"0x5a3c"`},
		{"eth_createAccessList", `[` + oraclePxCall + `,"0x11118800"]`,
			`{"accessList":[],"gasUsed":"0x5a3c"}`},
		{"eth_simulateV1", `[{"blockStateCalls":[{"calls":[` + oraclePxCall + `]}]},"0x11118800"]`,
			`[{"number":"0x11118800","calls":[{"returnData":"0x12345","status":"0x1","gasUsed":"0x5a3c","logs":[]}]}]`},
		{"debug_traceCall", `[` + oraclePxCall + `,"0x11118800",{}]`,
			`{"gas":23100,"failed":false,"returnValue":"12345","structLogs":[]}`},
	}

	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()
			defer util.AssertNoPendingMocks(t, 0)

			gock.New("http://rpc1.localhost").
				Post("").
				Filter(func(r *http.Request) bool {
					return strings.Contains(util.SafeReadBody(r), tc.method)
				}).
				Times(1).
				Reply(200).
				JSON(precompileStateGapReply())
			gock.New("http://rpc2.localhost").
				Post("").
				Filter(func(r *http.Request) bool {
					return strings.Contains(util.SafeReadBody(r), tc.method)
				}).
				Times(1).
				Reply(200).
				BodyString(`{"jsonrpc":"2.0","id":1,"result":` + tc.result + `}`)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// No network retries: the failover must happen inside a single
			// sweep, as it does for any other upstream that lacks the data.
			network := setupTestNetworkForMissingDataRetry(t, ctx, nil,
				&common.RetryPolicyConfig{MaxAttempts: 1},
			)
			network.PinUpstreamOrderForTest("rpc1", "rpc2")

			req := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":1,"method":"` + tc.method + `","params":` + tc.params + `}`))

			resp, err := network.Forward(ctx, req)
			require.NoError(t, err, "the upstream that holds HyperCore state must answer")
			require.NotNil(t, resp)

			jrr, err := resp.JsonRpcResponse()
			require.NoError(t, err)
			require.Nil(t, jrr.Error)
			assert.JSONEq(t, tc.result, string(jrr.GetResultBytes()))
			assert.Equal(t, "rpc2", resp.UpstreamId())
		})
	}
}

func TestHttpServer_PrecompileStateGap_AllUpstreamsLackStateSurfacesNodeMessage(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	for _, host := range []string{"http://rpc1.localhost", "http://rpc2.localhost"} {
		gock.New(host).
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Persist().
			Reply(200).
			JSON(precompileStateGapReply())
	}

	cfg := &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(10 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{{
			Id: "test_project",
			Networks: []*common.NetworkConfig{{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123},
			}},
			Upstreams: []*common.UpstreamConfig{
				{
					Id:       "rpc1",
					Endpoint: "http://rpc1.localhost",
					Type:     common.UpstreamTypeEvm,
					Evm:      &common.EvmUpstreamConfig{ChainId: 123},
				},
				{
					Id:       "rpc2",
					Endpoint: "http://rpc2.localhost",
					Type:     common.UpstreamTypeEvm,
					Evm:      &common.EvmUpstreamConfig{ChainId: 123},
				},
			},
		}},
	}

	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	statusCode, _, body := sendRequest(
		`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[`+oraclePxCall+`,"0x11118800"]}`, nil, nil)
	assert.Equal(t, http.StatusOK, statusCode)

	var respObject map[string]interface{}
	require.NoError(t, sonic.UnmarshalString(body, &respObject))
	errObj, ok := respObject["error"].(map[string]interface{})
	require.True(t, ok, "response must carry a JSON-RPC error: %s", body)
	// -32014 tells the client the data is missing on the node, not that its
	// call ran out of gas, and the node's own wording is kept verbatim.
	assert.Equal(t, float64(common.JsonRpcErrorMissingData), errObj["code"])
	assert.Equal(t, precompileStateGapMessage, errObj["message"])
}
