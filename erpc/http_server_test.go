package erpc

// import (
// 	"context"
// 	"encoding/json"
// 	"net/http"
// 	"net/http/httptest"
// 	"strings"
// 	"sync"
// 	"testing"
// 	"time"

// 	"github.com/erpc/erpc/common"
// 	"github.com/h2non/gock"
// 	"github.com/rs/zerolog"
// 	"github.com/stretchr/testify/assert"
// 	"github.com/stretchr/testify/require"
// )

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	// "github.com/erpc/erpc/health"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHttpServer_RaceTimeouts(t *testing.T) {
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})
	defer gock.Off()

	// Setup
	logger := zerolog.New(zerolog.NewConsoleWriter())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: "200ms", // Set a very short timeout for testing
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm: &common.EvmNetworkConfig{
							ChainId: 1,
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc1.localhost",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 1,
						},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	erpcInstance, err := NewERPC(ctx, &logger, nil, cfg)
	require.NoError(t, err)

	httpServer := NewHttpServer(ctx, &logger, cfg.Server, erpcInstance)

	// Start the server on a random port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port

	go func() {
		err := httpServer.server.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("Server error: %v", err)
		}
	}()
	defer httpServer.server.Shutdown()

	// Wait for the server to start
	time.Sleep(1000 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost:%d", port)

	sendRequest := func() (int, string) {
		body := strings.NewReader(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`)
		req, err := http.NewRequest("POST", baseURL+"/test_project/evm/1", body)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{
			Timeout: 1000 * time.Millisecond, // Set a client timeout longer than the server timeout
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp.StatusCode, err.Error()
		}
		return resp.StatusCode, string(respBody)
	}

	t.Run("ConcurrentRequestsWithTimeouts", func(t *testing.T) {
		gock.New("http://rpc1.localhost").
			Post("/").
			Times(50).
			Reply(200).
			Delay(2000 * time.Millisecond). // Delay longer than the server timeout
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1234",
			})

		const concurrentRequests = 5
		var wg sync.WaitGroup
		results := make([]struct {
			statusCode int
			body       string
		}, concurrentRequests)

		for i := 0; i < concurrentRequests; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				results[index].statusCode, results[index].body = sendRequest()
			}(i)
		}

		wg.Wait()

		for _, result := range results {
			if result.statusCode != http.StatusGatewayTimeout && result.statusCode != http.StatusRequestTimeout {
				t.Errorf("unexpected status code: %d", result.statusCode)
			}
			assert.Contains(t, result.body, "Timeout")
		}
	})

	t.Run("RapidSuccessiveRequests", func(t *testing.T) {
		gock.New("http://rpc1.localhost").
			Post("/").
			Times(10).
			Reply(200).
			Delay(300 * time.Millisecond). // Delay longer than the server timeout
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1234",
			})

		for i := 0; i < 10; i++ {
			statusCode, body := sendRequest()
			assert.Equal(t, http.StatusGatewayTimeout, statusCode)
			assert.Contains(t, body, "Timeout")
		}
	})

	t.Run("MixedTimeoutAndNonTimeoutRequests", func(t *testing.T) {
		totalReqs := 20

		for i := 0; i < totalReqs; i++ {
			var delay time.Duration
			if i % 2 == 0 {
				delay = 1 * time.Millisecond // shorter than the server timeout
			} else {
				delay = 400 * time.Millisecond // longer than the server timeout
			}
			gock.New("http://rpc1.localhost").
				Post("/").
				Times(1).
				ReplyFunc(func(r *gock.Response) {
					r.Status(200)
					r.JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      rand.Intn(100000000),
						"result":  "0x1234",
					})
				}).
				Delay(delay)
		}

		var wg sync.WaitGroup
		results := make([]struct {
			statusCode int
			body       string
		}, totalReqs)

		for i := 0; i < totalReqs; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				results[index].statusCode, results[index].body = sendRequest()
			}(i)
			time.Sleep(50 * time.Millisecond)
		}
		wg.Wait()

		timeouts := 0
		successes := 0
		for _, result := range results {
			if result.statusCode == http.StatusGatewayTimeout || result.statusCode == http.StatusRequestTimeout {
				timeouts++
				assert.Contains(t, result.body, "Timeout")
			} else {
				successes++
				assert.Contains(t, result.body, "0x1234")
			}
		}

		fmt.Printf("Timeouts: %d, Successes: %d\n", timeouts, successes)
		assert.True(t, timeouts > 0, "Expected some timeouts")
		assert.True(t, successes > 0, "Expected some successes")
	})
}

// func TestHttpServer_HandleRequest_EthGetBlockNumber(t *testing.T) {
// 	defer gock.Off()

// 	// Setup
// 	logger := zerolog.New(zerolog.NewConsoleWriter())
// 	ctx := context.Background()

// 	cfg := &common.Config{
// 		Server: &common.ServerConfig{
// 			MaxTimeout: "5s",
// 		},
// 		Projects: []*common.ProjectConfig{
// 			{
// 				Id: "test_project",
// 				Networks: []*common.NetworkConfig{
// 					{
// 						Architecture: common.ArchitectureEvm,
// 						Evm: &common.EvmNetworkConfig{
// 							ChainId: 1,
// 						},
// 					},
// 				},
// 			},
// 		},
// 		RateLimiters: &common.RateLimiterConfig{},
// 	}

// 	erpcInstance, err := NewERPC(ctx, &logger, nil, cfg)
// 	require.NoError(t, err)

// 	httpServer := NewHttpServer(ctx, &logger, cfg.Server, erpcInstance)

// 	// Test case: Multiple concurrent eth_getBlockNumber requests
// 	t.Run("ConcurrentEthGetBlockNumber", func(t *testing.T) {
// 		gock.New("http://rpc1.localhost").
// 			Post("/").
// 			Times(10).
// 			Reply(200).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  "0x1234",
// 			})

// 		const concurrentRequests = 10
// 		var wg sync.WaitGroup
// 		responses := make([]*http.Response, concurrentRequests)
// 		errors := make([]error, concurrentRequests)

// 		for i := 0; i < concurrentRequests; i++ {
// 			wg.Add(1)
// 			go func(index int) {
// 				defer wg.Done()
// 				body := strings.NewReader(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`)
// 				req := httptest.NewRequest("POST", "/test_project/evm:1", body)
// 				req.Header.Set("Content-Type", "application/json")
// 				w := httptest.NewRecorder()
// 				httpServer.server.Handler.ServeHTTP(w, req)
// 				responses[index] = w.Result()
// 			}(i)
// 		}

// 		wg.Wait()

// 		for i, resp := range responses {
// 			assert.NotNil(t, resp, "Response should not be nil for request %d", i)
// 			assert.Equal(t, http.StatusOK, resp.StatusCode, "Status code should be 200 for request %d", i)

// 			var result map[string]interface{}
// 			err := json.NewDecoder(resp.Body).Decode(&result)
// 			assert.NoError(t, err, "Should be able to decode response for request %d", i)
// 			assert.Equal(t, "0x1234", result["result"], "Unexpected result for request %d", i)
// 			resp.Body.Close()
// 		}

// 		assert.True(t, gock.IsDone(), "All mocks should have been called")
// 	})

// 	// Test case: Request with invalid JSON
// 	t.Run("InvalidJSON", func(t *testing.T) {
// 		body := strings.NewReader(`{"invalid json`)
// 		req := httptest.NewRequest("POST", "/test_project/evm:1", body)
// 		req.Header.Set("Content-Type", "application/json")
// 		w := httptest.NewRecorder()
// 		httpServer.server.Handler.ServeHTTP(w, req)

// 		assert.Equal(t, http.StatusInternalServerError, w.Code)

// 		var errorResponse map[string]interface{}
// 		err := json.NewDecoder(w.Body).Decode(&errorResponse)
// 		require.NoError(t, err)

// 		assert.Contains(t, errorResponse, "error")
// 		errorObj := errorResponse["error"].(map[string]interface{})
// 		assert.Contains(t, errorObj["message"], "unexpected end of JSON input")
// 	})

// 	// Test case: Request with unsupported method
// 	t.Run("UnsupportedMethod", func(t *testing.T) {
// 		gock.New("http://rpc1.localhost").
// 			Post("/").
// 			Reply(200).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"error": map[string]interface{}{
// 					"code":    -32601,
// 					"message": "Method not found",
// 				},
// 			})

// 		body := strings.NewReader(`{"jsonrpc":"2.0","method":"unsupported_method","params":[],"id":1}`)
// 		req := httptest.NewRequest("POST", "/test_project/evm:1", body)
// 		req.Header.Set("Content-Type", "application/json")
// 		w := httptest.NewRecorder()
// 		httpServer.server.Handler.ServeHTTP(w, req)

// 		assert.Equal(t, http.StatusOK, w.Code) // JSON-RPC errors are returned with 200 OK

// 		var errorResponse map[string]interface{}
// 		err := json.NewDecoder(w.Body).Decode(&errorResponse)
// 		require.NoError(t, err)

// 		assert.Contains(t, errorResponse, "error")
// 		errorObj := errorResponse["error"].(map[string]interface{})
// 		assert.Equal(t, float64(-32601), errorObj["code"])
// 		assert.Contains(t, errorObj["message"], "Method not found")

// 		assert.True(t, gock.IsDone(), "All mocks should have been called")
// 	})

// 	// Test case: Request with invalid project ID
// 	t.Run("InvalidProjectID", func(t *testing.T) {
// 		body := strings.NewReader(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`)
// 		req := httptest.NewRequest("POST", "/invalid_project/evm:1", body)
// 		req.Header.Set("Content-Type", "application/json")
// 		w := httptest.NewRecorder()
// 		httpServer.server.Handler.ServeHTTP(w, req)

// 		assert.Equal(t, http.StatusInternalServerError, w.Code)

// 		var errorResponse map[string]interface{}
// 		err := json.NewDecoder(w.Body).Decode(&errorResponse)
// 		require.NoError(t, err)

// 		assert.Contains(t, errorResponse, "error")
// 		errorObj := errorResponse["error"].(map[string]interface{})
// 		assert.Contains(t, errorObj["message"], "project not found")
// 	})

// 	// Test case: Simulating upstream latency and timeout
// 	t.Run("UpstreamLatencyAndTimeout", func(t *testing.T) {
// 		gock.New("http://rpc1.localhost").
// 			Post("/").
// 			Reply(200).
// 			Delay(6 * time.Second). // Delay longer than the server timeout
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  "0x1234",
// 			})

// 		body := strings.NewReader(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`)
// 		req := httptest.NewRequest("POST", "/test_project/evm:1", body)
// 		req.Header.Set("Content-Type", "application/json")
// 		w := httptest.NewRecorder()
// 		httpServer.server.Handler.ServeHTTP(w, req)

// 		assert.Equal(t, http.StatusInternalServerError, w.Code)

// 		var errorResponse map[string]interface{}
// 		err := json.NewDecoder(w.Body).Decode(&errorResponse)
// 		require.NoError(t, err)

// 		assert.Contains(t, errorResponse, "error")
// 		errorObj := errorResponse["error"].(map[string]interface{})
// 		assert.Contains(t, errorObj["message"], "timeout")

// 		assert.True(t, gock.IsDone(), "All mocks should have been called")
// 	})

// 	// Test case: Multiple different concurrent requests
// 	t.Run("MultipleDifferentConcurrentRequests", func(t *testing.T) {
// 		gock.New("http://rpc1.localhost").
// 			Post("/").
// 			Times(5).
// 			Reply(200).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  "0x1234",
// 			})

// 		gock.New("http://rpc1.localhost").
// 			Post("/").
// 			Times(5).
// 			Reply(200).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  "0x5678",
// 			})

// 		const concurrentRequests = 10
// 		var wg sync.WaitGroup
// 		responses := make([]*http.Response, concurrentRequests)
// 		errors := make([]error, concurrentRequests)

// 		for i := 0; i < concurrentRequests; i++ {
// 			wg.Add(1)
// 			go func(index int) {
// 				defer wg.Done()
// 				var body string
// 				if index%2 == 0 {
// 					body = `{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`
// 				} else {
// 					body = `{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x742d35Cc6634C0532925a3b844Bc454e4438f44e"],"id":1}`
// 				}
// 				req := httptest.NewRequest("POST", "/test_project/evm:1", strings.NewReader(body))
// 				req.Header.Set("Content-Type", "application/json")
// 				w := httptest.NewRecorder()
// 				httpServer.server.Handler.ServeHTTP(w, req)
// 				responses[index] = w.Result()
// 			}(i)
// 		}

// 		wg.Wait()

// 		for i, resp := range responses {
// 			assert.NotNil(t, resp, "Response should not be nil for request %d", i)
// 			assert.Equal(t, http.StatusOK, resp.StatusCode, "Status code should be 200 for request %d", i)

// 			var result map[string]interface{}
// 			err := json.NewDecoder(resp.Body).Decode(&result)
// 			assert.NoError(t, err, "Should be able to decode response for request %d", i)
// 			assert.Contains(t, []string{"0x1234", "0x5678"}, result["result"], "Unexpected result for request %d", i)
// 			resp.Body.Close()
// 		}

// 		assert.True(t, gock.IsDone(), "All mocks should have been called")
// 	})
// }
