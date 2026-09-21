package erpc

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func envelopeTestConfig() *common.Config {
	return &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm:          &common.EvmNetworkConfig{ChainId: 123},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "rpc1",
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc1.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}
}

// TestHttpServer_RequestEnvelope covers the request envelope end to end: an
// object that states "method" more than once is reported as malformed and
// reaches no upstream, and the "networkId" member still routes a request that
// carries no architecture or chain in its path.
func TestHttpServer_RequestEnvelope(t *testing.T) {
	t.Run("RepeatedMethodMemberReachesNoUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Neither spelling may be forwarded, so stand up a mock for each and
		// require both to go unused.
		for _, method := range []string{"eth_getBalance", "eth_sendRawTransaction"} {
			gock.New("http://rpc1.localhost").
				Post("/").
				Filter(func(request *http.Request) bool {
					return strings.Contains(util.SafeReadBody(request), method)
				}).
				Reply(200).
				JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0xdeadbeef"})
		}
		defer util.AssertNoPendingMocks(t, 2)

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(envelopeTestConfig(), t)
		defer shutdown()
		time.Sleep(1500 * time.Millisecond)

		for name, body := range map[string]string{
			"repeated key":   `{"jsonrpc":"2.0","method":"eth_getBalance","method":"eth_sendRawTransaction","params":[],"id":1}`,
			"escaped repeat": `{"jsonrpc":"2.0","\u006dethod":"eth_getBalance","method":"eth_sendRawTransaction","params":[],"id":1}`,
			"case variant":   `{"jsonrpc":"2.0","method":"eth_getBalance","Method":"eth_sendRawTransaction","params":[],"id":1}`,
		} {
			t.Run(name, func(t *testing.T) {
				statusCode, _, respBody := sendRequest(body, nil, nil)
				assert.Equal(t, http.StatusBadRequest, statusCode, "body: %s", respBody)
				assert.Contains(t, respBody, "method")
				assert.NotContains(t, respBody, "0xdeadbeef")
			})
		}
	})

	t.Run("NetworkIdMemberRoutesPathlessRequest", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		gock.New("http://rpc1.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				// The routing hint is server-side only and must not be relayed.
				return strings.Contains(body, "trace_transaction") && !strings.Contains(body, "networkId")
			}).
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1a2b3c"})
		defer util.AssertNoPendingMocks(t, 0)

		_, _, baseURL, shutdown, _ := createServerTestFixtures(envelopeTestConfig(), t)
		defer shutdown()
		time.Sleep(1500 * time.Millisecond)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/test_project", strings.NewReader(
			`{"jsonrpc":"2.0","method":"trace_transaction","params":["0xabc"],"id":1,"networkId":"evm:123"}`,
		))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		respBody, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", string(respBody))
		assert.Contains(t, string(respBody), "0x1a2b3c")
	})
}
