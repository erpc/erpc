package erpc

import (
	"bytes"
	"compress/gzip"
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

	// maxClientBatchSize refuses a whole batch before any of it runs: one
	// -32600 error with id null, and no upstream call.
	t.Run("ClientBatchCap", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			configure func(cfg *common.Config)
		}{
			{"NetworkLimit", func(cfg *common.Config) {
				cfg.Projects[0].Networks[0].MaxClientBatchSize = util.IntPtr(3)
			}},
			{"InheritedFromNetworkDefaults", func(cfg *common.Config) {
				cfg.Projects[0].NetworkDefaults = &common.NetworkDefaults{MaxClientBatchSize: util.IntPtr(3)}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				calls := countEvmGetBalanceCalls(t)
				cfg := clientBatchCapTestConfig()
				tc.configure(cfg)
				_, _, baseURL, shutdown, _ := createServerTestFixtures(cfg, t)
				defer shutdown()
				time.Sleep(1500 * time.Millisecond)

				statusCode, respBody := postBody(t, baseURL+"/test_project/evm/123", getBalanceBatch(3, ""), nil)
				assert.Equal(t, http.StatusOK, statusCode, "body: %s", respBody)
				var items []map[string]interface{}
				require.NoError(t, common.SonicCfg.Unmarshal([]byte(respBody), &items), "body: %s", respBody)
				assert.Len(t, items, 3)
				assert.EqualValues(t, 3, calls.Load(), "a batch at the limit is served")

				statusCode, respBody = postBody(t, baseURL+"/test_project/evm/123", getBalanceBatch(4, ""), nil)
				assert.Equal(t, http.StatusBadRequest, statusCode, "body: %s", respBody)
				assertSingleBatchCapError(t, respBody, 4, 3)
				assert.EqualValues(t, 3, calls.Load(), "an over-limit batch must reach no upstream")
			})
		}

		t.Run("UnsetServesAnySize", func(t *testing.T) {
			calls := countEvmGetBalanceCalls(t)
			_, _, baseURL, shutdown, _ := createServerTestFixtures(clientBatchCapTestConfig(), t)
			defer shutdown()
			time.Sleep(1500 * time.Millisecond)

			statusCode, respBody := postBody(t, baseURL+"/test_project/evm/123", getBalanceBatch(20, ""), nil)
			assert.Equal(t, http.StatusOK, statusCode, "body: %s", respBody)
			assert.EqualValues(t, 20, calls.Load())
		})

		// Without a network in the path each request names its own, so the
		// limit counts only the requests addressed to the limited network.
		t.Run("PathlessBatchCountsPerNetwork", func(t *testing.T) {
			calls := countEvmGetBalanceCalls(t)
			cfg := clientBatchCapTestConfig()
			cfg.Projects[0].Networks[0].MaxClientBatchSize = util.IntPtr(2)
			_, _, baseURL, shutdown, _ := createServerTestFixtures(cfg, t)
			defer shutdown()
			time.Sleep(1500 * time.Millisecond)

			mixed := `[` +
				`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x01","latest"],"networkId":"evm:123"},` +
				`{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":["0x02","latest"],"networkId":"evm:123"},` +
				`{"jsonrpc":"2.0","id":3,"method":"eth_getBalance","params":["0x03","latest"],"networkId":"evm:999"}` +
				`]`
			statusCode, respBody := postBody(t, baseURL+"/test_project", mixed, nil)
			assert.Equal(t, http.StatusOK, statusCode, "body: %s", respBody)
			assert.EqualValues(t, 2, calls.Load(), "two requests to the limited network are within its limit; body: %s", respBody)

			statusCode, respBody = postBody(t, baseURL+"/test_project", getBalanceBatch(3, "evm:123"), nil)
			assert.Equal(t, http.StatusBadRequest, statusCode, "body: %s", respBody)
			assertSingleBatchCapError(t, respBody, 3, 2)
			assert.EqualValues(t, 2, calls.Load(), "an over-limit batch must reach no upstream")
		})

		// Solana clients read errors from a 200 body, as for any invalid
		// request on a Solana network.
		t.Run("SolanaAnswersWith200", func(t *testing.T) {
			util.ResetGock()
			t.Cleanup(util.ResetGock)
			util.SetupMocksForSvmStatePoller("svm-cap-rpc1.localhost", 1000, 990)
			var calls atomic.Int64
			gock.New("http://svm-cap-rpc1.localhost").
				Post("").
				Persist().
				Filter(func(r *http.Request) bool {
					return strings.Contains(util.SafeReadBody(r), `"getBalance"`)
				}).
				Reply(200).
				Map(func(res *http.Response) *http.Response { calls.Add(1); return res }).
				BodyString(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1000},"value":5}}`)

			cfg := &common.Config{
				Server: &common.ServerConfig{MaxTimeout: common.Duration(5 * time.Second).Ptr()},
				Projects: []*common.ProjectConfig{{
					Id: "test_project",
					Networks: []*common.NetworkConfig{{
						Architecture:       common.ArchitectureSvm,
						Svm:                &common.SvmNetworkConfig{Cluster: "mainnet-beta"},
						MaxClientBatchSize: util.IntPtr(1),
					}},
					Upstreams: []*common.UpstreamConfig{svmUpstreamConfig("rpc1", "svm-cap-rpc1.localhost")},
				}},
				RateLimiters: &common.RateLimiterConfig{},
			}
			_, _, baseURL, shutdown, _ := createServerTestFixtures(cfg, t)
			defer shutdown()
			time.Sleep(1500 * time.Millisecond)

			one := `[{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["11111111111111111111111111111111"]}]`
			statusCode, respBody := postBody(t, baseURL+"/test_project/svm/mainnet-beta", one, nil)
			assert.Equal(t, http.StatusOK, statusCode, "body: %s", respBody)
			assert.Contains(t, respBody, `"value":5`)
			assert.EqualValues(t, 1, calls.Load())

			two := `[` +
				`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["11111111111111111111111111111111"]},` +
				`{"jsonrpc":"2.0","id":2,"method":"getBalance","params":["11111111111111111111111111111111"]}` +
				`]`
			statusCode, respBody = postBody(t, baseURL+"/test_project/svm/mainnet-beta", two, nil)
			assert.Equal(t, http.StatusOK, statusCode, "body: %s", respBody)
			assertSingleBatchCapError(t, respBody, 2, 1)
			assert.EqualValues(t, 1, calls.Load(), "an over-limit batch must reach no upstream")
		})
	})

	// maxRequestBodySize bounds the body eRPC reads, after gzip
	// decompression: past it the request gets 413 and runs nothing.
	t.Run("RequestBodyCap", func(t *testing.T) {
		calls := countEvmGetBalanceCalls(t)
		atLimit := getBalanceBatch(3, "")
		limit := int64(len(atLimit))
		overLimit := atLimit + " "
		cfg := clientBatchCapTestConfig()
		cfg.Server.MaxRequestBodySize = &limit
		_, _, baseURL, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()
		time.Sleep(1500 * time.Millisecond)
		url := baseURL + "/test_project/evm/123"
		gz := map[string]string{"Content-Encoding": "gzip"}

		statusCode, respBody := postBody(t, url, atLimit, nil)
		assert.Equal(t, http.StatusOK, statusCode, "body: %s", respBody)
		assert.EqualValues(t, 3, calls.Load(), "a body at the limit is served")
		statusCode, respBody = postRawBody(t, url, bytes.NewReader(gzipBytes(t, atLimit)), gz)
		assert.Equal(t, http.StatusOK, statusCode, "body: %s", respBody)
		assert.EqualValues(t, 6, calls.Load(), "a gzip body at the limit is served")

		for _, tc := range []struct {
			name    string
			body    []byte
			headers map[string]string
		}{
			{"Plain", []byte(overLimit), nil},
			// Compressed, the body is under the limit; decompressed it is over.
			{"Gzip", gzipBytes(t, overLimit), gz},
		} {
			t.Run(tc.name, func(t *testing.T) {
				statusCode, respBody := postRawBody(t, url, bytes.NewReader(tc.body), tc.headers)
				assert.Equal(t, http.StatusRequestEntityTooLarge, statusCode, "body: %s", respBody)
				var obj map[string]interface{}
				require.NoError(t, common.SonicCfg.Unmarshal([]byte(respBody), &obj), "body: %s", respBody)
				assert.Contains(t, obj, "id")
				assert.Nil(t, obj["id"])
				errObj, ok := obj["error"].(map[string]interface{})
				require.True(t, ok, "body: %s", respBody)
				assert.EqualValues(t, common.JsonRpcErrorClientSideException, errObj["code"])
				assert.Contains(t, errObj["message"], fmt.Sprintf("limit of %d bytes", limit))
				assert.EqualValues(t, 6, calls.Load(), "an over-limit body must reach no upstream")
			})
		}
	})
}

// clientBatchCapTestConfig is envelopeTestConfig with upstream batching off,
// so each client request is one upstream call.
func clientBatchCapTestConfig() *common.Config {
	cfg := envelopeTestConfig()
	cfg.Projects[0].Upstreams[0].JsonRpc = &common.JsonRpcUpstreamConfig{SupportsBatch: &common.FALSE}
	return cfg
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write([]byte(s))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// countEvmGetBalanceCalls answers every eth_getBalance sent to rpc1 and
// counts them.
func countEvmGetBalanceCalls(t *testing.T) *atomic.Int64 {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForEvmStatePoller()
	calls := &atomic.Int64{}
	gock.New("http://rpc1.localhost").
		Post("/").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), `"eth_getBalance"`)
		}).
		Reply(200).
		Map(func(res *http.Response) *http.Response { calls.Add(1); return res }).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})
	return calls
}

// getBalanceBatch builds a batch of n eth_getBalance requests, each naming
// networkId when it is set.
func getBalanceBatch(n int, networkId string) string {
	hint := ""
	if networkId != "" {
		hint = `,"networkId":"` + networkId + `"`
	}
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBalance","params":["0x%02x","latest"]%s}`, i+1, i+1, hint)
	}
	return "[" + strings.Join(items, ",") + "]"
}

func postBody(t *testing.T, url, body string, headers map[string]string) (int, string) {
	return postRawBody(t, url, strings.NewReader(body), headers)
}

func postRawBody(t *testing.T, url string, body io.Reader, headers map[string]string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(respBody)
}

func assertSingleBatchCapError(t *testing.T, respBody string, batchSize, limit int) {
	t.Helper()
	var obj map[string]interface{}
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(respBody), &obj), "want one error object, got: %s", respBody)
	assert.Contains(t, obj, "id")
	assert.Nil(t, obj["id"])
	errObj, ok := obj["error"].(map[string]interface{})
	require.True(t, ok, "body: %s", respBody)
	assert.EqualValues(t, common.JsonRpcErrorClientSideException, errObj["code"])
	assert.Contains(t, errObj["message"], fmt.Sprintf("batch of %d requests", batchSize))
	assert.Contains(t, errObj["message"], fmt.Sprintf("limit of %d", limit))
}
