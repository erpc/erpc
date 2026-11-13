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
					if tc.name == "svm" {
						// A Solana node answers a negative id with a parse error.
						gock.New("http://" + host).
							Post("").
							Persist().
							Filter(func(request *http.Request) bool {
								body := util.SafeReadBody(request)
								if request.URL.Host != host || !strings.Contains(body, `"method":"`+tc.method+`"`) || !strings.Contains(body, `"id":-`) {
									return false
								}
								calls.Add(1)
								return true
							}).
							Reply(200).
							JSON(map[string]interface{}{"jsonrpc": "2.0", "id": nil, "error": map[string]interface{}{"code": -32700, "message": "Parse error"}})
					}
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

				postRaw := func(body string) (int, []byte, []map[string]interface{}) {
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
					return resp.StatusCode, raw, items
				}
				post := func(body string) []map[string]interface{} {
					t.Helper()
					_, _, items := postRaw(body)
					return items
				}
				item := func(id string) string {
					return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":["0x0000000000000000000000000000000000000001"]}`, id, tc.method)
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

				// A Solana node reads an id as a u64 or a string. It answers a
				// negative id with -32700 "Parse error", id null, HTTP 200, so
				// on Solana eRPC gives that reply itself instead of asking
				// every upstream for it. EVM nodes take any number as an id,
				// so there the request is forwarded.
				t.Run("NegativeId", func(t *testing.T) {
					assertAgaveParseError := func(item map[string]interface{}) {
						t.Helper()
						id, hasID := item["id"]
						assert.True(t, hasID, "the reply must carry an id member: %v", item)
						assert.Nil(t, id, "the reply carries id null: %v", item)
						errObj, ok := item["error"].(map[string]interface{})
						require.True(t, ok, "the reply must carry an error object: %v", item)
						assert.EqualValues(t, common.JsonRpcErrorParseException, errObj["code"], "error: %v", errObj)
						assert.Equal(t, "Parse error", errObj["message"], "error: %v", errObj)
					}

					calls.Store(0)
					status, raw, single := postRaw(item(`-7`))
					require.Len(t, single, 1)
					if tc.name == "svm" {
						assert.Equal(t, http.StatusOK, status, "body: %s", raw)
						assertAgaveParseError(single[0])
						assert.Zero(t, calls.Load(), "no upstream may be asked")

						calls.Store(0)
						batch := post("[" + item(`-7`) + "," + item(`7`) + "]")
						require.Len(t, batch, 2)
						assertAgaveParseError(batch[0])
						assert.EqualValues(t, 7, batch[1]["id"])
						assert.Equal(t, "0xfeed", batch[1]["result"], "the valid item is served: %v", batch[1])
						assert.EqualValues(t, 1, calls.Load(), "only the valid item reaches an upstream")
					} else {
						assert.EqualValues(t, -7, single[0]["id"], "body: %s", raw)
						assert.Equal(t, "0xfeed", single[0]["result"], "body: %s", raw)
						assert.EqualValues(t, 1, calls.Load(), "the request is forwarded")
					}
				})

				// An integer id above the int64 range (up to the u64 maximum a
				// Solana node accepts) is served and echoed byte for byte.
				t.Run("Uint64RangeId", func(t *testing.T) {
					for _, bigID := range []string{`9223372036854775808`, `18446744073709551615`} {
						calls.Store(0)
						status, raw, single := postRaw(item(bigID))
						require.Len(t, single, 1)
						assert.Equal(t, http.StatusOK, status, "body: %s", raw)
						assert.Contains(t, string(raw), `"id":`+bigID, "the id is echoed verbatim")
						assert.Equal(t, "0xfeed", single[0]["result"], "body: %s", raw)
						assert.EqualValues(t, 1, calls.Load(), "id %s", bigID)
					}
				})
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
