package erpc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
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

func svmEnvelopeTestConfig() *common.Config {
	upstream := func(id string) *common.UpstreamConfig {
		return &common.UpstreamConfig{
			Id:       id,
			Type:     common.UpstreamTypeSvm,
			Endpoint: "http://" + id + ".localhost",
			Svm:      &common.SvmUpstreamConfig{Cluster: "mainnet-beta"},
		}
	}
	return &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureSvm,
						Svm:          &common.SvmNetworkConfig{Cluster: "mainnet-beta"},
						Failsafe: []*common.FailsafeConfig{{
							MatchMethod: "*",
							Retry:       &common.RetryPolicyConfig{MaxAttempts: 3},
							Hedge:       &common.HedgePolicyConfig{Delay: common.NewStaticDuration(10 * time.Millisecond), MaxCount: 1},
						}},
					},
				},
				Upstreams: []*common.UpstreamConfig{upstream("svm-env-rpc1"), upstream("svm-env-rpc2")},
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
		t.Cleanup(util.ResetGock)
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

	t.Run("BatchItemsAreParsedIndependently", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()

		// Exactly the two well-formed items reach the upstream; the item in
		// between is answered locally.
		gock.New("http://rpc1.localhost").
			Post("/").
			Times(2).
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "trace_transaction") && !strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0xfeed"})
		defer util.AssertNoPendingMocks(t, 0)

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(envelopeTestConfig(), t)
		defer shutdown()
		time.Sleep(1500 * time.Millisecond)

		statusCode, _, respBody := sendRequest(`[`+
			`{"jsonrpc":"2.0","method":"trace_transaction","params":["0x01"],"id":1},`+
			`{"jsonrpc":"2.0","method":"trace_transaction","method":"eth_sendRawTransaction","params":["0x02"],"id":2},`+
			`{"jsonrpc":"2.0","method":"trace_transaction","params":["0x03"],"id":3}`+
			`]`, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode, "body: %s", respBody)

		var items []map[string]interface{}
		require.NoError(t, common.SonicCfg.Unmarshal([]byte(respBody), &items), "body: %s", respBody)
		require.Len(t, items, 3)
		assert.Equal(t, "0xfeed", items[0]["result"])
		assert.Equal(t, "0xfeed", items[2]["result"])
		assert.Nil(t, items[1]["result"])
		errObj, ok := items[1]["error"].(map[string]interface{})
		require.True(t, ok, "item 2 should carry an error object: %v", items[1])
		t.Logf("batch item error: %v", errObj)
		assert.EqualValues(t, common.JsonRpcErrorParseException, errObj["code"])
	})

	t.Run("NetworkIdMemberRoutesPathlessRequest", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
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

	// JSON-RPC 2.0 §4: an id is a string, a number, or null. Any other id is
	// an Invalid Request (-32600, id null) answered before any upstream is
	// asked, alone or as one item of a batch, on every architecture. The
	// Solana network carries a retry and a hedge over two upstreams, so a
	// bad id that leaks into forwarding is seen as many upstream calls.
	t.Run("NonScalarIdIsInvalidRequestAndReachesNoUpstream", func(t *testing.T) {
		for _, tc := range []struct {
			name, path, method string
			cfg                func() *common.Config
			hosts              []string
		}{
			{name: "evm", path: "/test_project/evm/123", method: "eth_getBalance", cfg: envelopeTestConfig, hosts: []string{"rpc1.localhost"}},
			{name: "svm", path: "/test_project/svm/mainnet-beta", method: "getBalance", cfg: svmEnvelopeTestConfig, hosts: []string{"svm-env-rpc1.localhost", "svm-env-rpc2.localhost"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				util.ResetGock()
				t.Cleanup(util.ResetGock)
				util.SetupMocksForEvmStatePoller()
				var calls atomic.Int32
				for _, host := range tc.hosts {
					util.SetupMocksForSvmStatePoller(host, 1000, 990)
					gock.New("http://" + host).
						Post("").
						Persist().
						Filter(func(request *http.Request) bool {
							if request.URL.Host != host || !strings.Contains(util.SafeReadBody(request), `"method":"`+tc.method+`"`) {
								return false
							}
							calls.Add(1)
							return true
						}).
						Reply(200).
						JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0xfeed"})
				}

				_, _, baseURL, shutdown, _ := createServerTestFixtures(tc.cfg(), t)
				defer shutdown()
				time.Sleep(1500 * time.Millisecond)

				post := func(body string) []map[string]interface{} {
					t.Helper()
					resp, err := http.Post(baseURL+tc.path, "application/json", strings.NewReader(body))
					require.NoError(t, err)
					defer resp.Body.Close()
					raw, err := io.ReadAll(resp.Body)
					require.NoError(t, err)
					var items []map[string]interface{}
					if strings.HasPrefix(body, "[") {
						require.NoError(t, common.SonicCfg.Unmarshal(raw, &items), "body: %s", raw)
					} else {
						var item map[string]interface{}
						require.NoError(t, common.SonicCfg.Unmarshal(raw, &item), "body: %s", raw)
						items = append(items, item)
					}
					return items
				}
				assertInvalidRequest := func(item map[string]interface{}) {
					t.Helper()
					id, hasID := item["id"]
					assert.True(t, hasID, "the reply must carry an id member: %v", item)
					assert.Nil(t, id, "a bad id is answered with id null: %v", item)
					assert.Nil(t, item["result"])
					errObj, ok := item["error"].(map[string]interface{})
					require.True(t, ok, "the reply must carry an error object: %v", item)
					assert.EqualValues(t, common.JsonRpcErrorClientSideException, errObj["code"], "error: %v", errObj)
				}

				for _, badID := range []string{`true`, `{"a":1}`, `[1]`} {
					item := func(id string) string {
						return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":["0x0000000000000000000000000000000000000001"]}`, id, tc.method)
					}

					calls.Store(0)
					single := post(item(badID))
					require.Len(t, single, 1)
					assertInvalidRequest(single[0])
					assert.Zero(t, calls.Load(), "id %s: no upstream may be asked", badID)

					calls.Store(0)
					batch := post("[" + item(badID) + "," + item(`7`) + "]")
					require.Len(t, batch, 2)
					assertInvalidRequest(batch[0])
					assert.EqualValues(t, 7, batch[1]["id"])
					assert.Equal(t, "0xfeed", batch[1]["result"], "the valid item is served: %v", batch[1])
					assert.EqualValues(t, 1, calls.Load(), "id %s: only the valid item reaches an upstream", badID)
				}
			})
		}
	})

	// Empty batch body must stay "[]". The Solana notification path writes an
	// empty body when every reply was filtered out; that branch must not also
	// catch len(requests)==0 from "[]", or EVM clients that JSON.parse the
	// response will throw on a 200 with no body.
	t.Run("EmptyBatchBodyIsEmptyArray", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(envelopeTestConfig(), t)
		defer shutdown()
		time.Sleep(1500 * time.Millisecond)

		statusCode, _, respBody := sendRequest(`[]`, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode, "body: %s", respBody)
		assert.Equal(t, `[]`, strings.TrimSpace(respBody))
	})
}
