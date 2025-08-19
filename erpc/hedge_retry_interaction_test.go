package erpc

// import (
// 	"fmt"
// 	"net/http"
// 	"strings"
// 	"sync"
// 	"sync/atomic"
// 	"testing"
// 	"time"

// 	"github.com/erpc/erpc/common"
// 	"github.com/erpc/erpc/util"
// 	"github.com/h2non/gock"
// 	"github.com/rs/zerolog/log"
// 	"github.com/stretchr/testify/assert"
// )

// func init() {
// 	util.ConfigureTestLogger()
// }

// // TestProductionIssueReproduction demonstrates the actual production issue where
// // hedge and retry policies don't work together correctly, causing timeouts
// // even when a good upstream is available.
// func TestProductionIssueReproduction(t *testing.T) {
// 	t.Run("ActualProductionBugWithTraceFilter", func(t *testing.T) {
// 		// This test reproduces the exact production scenario:
// 		// - trace_filter requests
// 		// - quicknode fails initially, then works
// 		// - nirvana is always extremely slow
// 		// - With retry (no delay) + hedge, we expect to get response from quicknode via hedge
// 		// - BUT the system waits for nirvana instead

// 		util.ResetGock()
// 		defer util.ResetGock()
// 		util.SetupMocksForEvmStatePoller()
// 		defer util.AssertNoPendingMocks(t, 0)

// 		cfg := &common.Config{
// 			Server: &common.ServerConfig{
// 				MaxTimeout: common.Duration(15 * time.Second).Ptr(),
// 			},
// 			Projects: []*common.ProjectConfig{
// 				{
// 					Id: "test_project",
// 					Networks: []*common.NetworkConfig{
// 						{
// 							Architecture: common.ArchitectureEvm,
// 							Evm: &common.EvmNetworkConfig{
// 								ChainId: 1,
// 							},
// 							Failsafe: []*common.FailsafeConfig{
// 								{
// 									Timeout: &common.TimeoutPolicyConfig{
// 										Duration: common.Duration(15 * time.Second),
// 									},
// 									Retry: &common.RetryPolicyConfig{
// 										MaxAttempts: 3,
// 										Delay:       common.Duration(0), // No delay like production
// 									},
// 									Hedge: &common.HedgePolicyConfig{
// 										MaxCount: 2,
// 										Delay:    common.Duration(800 * time.Millisecond),
// 									},
// 								},
// 							},
// 						},
// 					},
// 					Upstreams: []*common.UpstreamConfig{
// 						{
// 							Id:       "quicknode-upstream",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://quicknode-test.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 						{
// 							Id:       "nirvana-upstream",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://nirvana-test.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 					},
// 				},
// 			},
// 			RateLimiters: &common.RateLimiterConfig{},
// 		}

// 		var quicknodeAttempts, nirvanaAttempts int32
// 		startTime := time.Now()

// 		// Quicknode: First attempt fails with rate limit
// 		gock.New("http://quicknode-test.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "trace_filter") {
// 					attempt := atomic.AddInt32(&quicknodeAttempts, 1)
// 					log.Info().
// 						Dur("elapsed", time.Since(startTime)).
// 						Int32("attempt", attempt).
// 						Str("upstream", "quicknode").
// 						Msg("Quicknode request")
// 					return attempt == 1
// 				}
// 				return false
// 			}).
// 			Reply(200).
// 			Delay(300 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"error": map[string]interface{}{
// 					"code":    -32005,
// 					"message": "rate limit exceeded",
// 				},
// 			})

// 		// Quicknode: Subsequent attempts work
// 		gock.New("http://quicknode-test.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(200 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result": []interface{}{
// 					map[string]interface{}{
// 						"action": map[string]interface{}{
// 							"from": "0xQUICKNODE_SUCCESS",
// 							"to":   "0x742d35Cc6634C0532925a3b844Bc9e7595f0bEb",
// 						},
// 						"blockHash": "0xabc",
// 					},
// 				},
// 			})

// 		// Nirvana: Always extremely slow (like in production)
// 		gock.New("http://nirvana-test.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "trace_filter") {
// 					attempt := atomic.AddInt32(&nirvanaAttempts, 1)
// 					log.Info().
// 						Dur("elapsed", time.Since(startTime)).
// 						Int32("attempt", attempt).
// 						Str("upstream", "nirvana").
// 						Msg("Nirvana request - will be slow")
// 					return true
// 				}
// 				return false
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(14 * time.Second). // Takes almost the full timeout!
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result": []interface{}{
// 					map[string]interface{}{
// 						"action": map[string]interface{}{
// 							"from": "0xNIRVANA_SLOW",
// 						},
// 					},
// 				},
// 			})

// 		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
// 		defer shutdown()

// 		startTime = time.Now()
// 		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"trace_filter","params":[{"fromBlock":"0x1","toBlock":"0x2"}],"id":1}`, nil, nil)
// 		elapsed := time.Since(startTime)

// 		log.Info().
// 			Dur("totalElapsed", elapsed).
// 			Int("statusCode", statusCode).
// 			Str("body", body).
// 			Int32("quicknodeAttempts", atomic.LoadInt32(&quicknodeAttempts)).
// 			Int32("nirvanaAttempts", atomic.LoadInt32(&nirvanaAttempts)).
// 			Msg("Test result")

// 		// EXPECTED TIMELINE:
// 		// T+0ms: First request to quicknode
// 		// T+300ms: Quicknode fails with rate limit
// 		// T+300ms: Immediate retry (delay=0) goes to nirvana
// 		// T+1100ms: Hedge kicks in (300ms + 800ms) and goes back to quicknode
// 		// T+1300ms: Quicknode responds successfully (1100ms + 200ms)
// 		// TOTAL: ~1.3 seconds

// 		// BUT IF THE BUG EXISTS:
// 		// System waits for nirvana to complete (14 seconds) or timeout

// 		if statusCode == http.StatusOK && elapsed < 3*time.Second {
// 			// EXPECTED: Hedge worked correctly
// 			assert.Contains(t, body, "QUICKNODE_SUCCESS", "Should get result from quicknode via hedge")
// 			t.Logf("✓ EXPECTED BEHAVIOR: Hedge worked! Got response in %v", elapsed)
// 		} else if elapsed > 10*time.Second {
// 			// BUG: System waited for slow nirvana
// 			t.Errorf("✗ BUG REPRODUCED: System waited %v instead of using hedge (~1.3s expected)", elapsed)
// 			t.Errorf("This demonstrates the production issue where hedge doesn't save us from slow upstreams")
// 			if strings.Contains(body, "NIRVANA_SLOW") {
// 				t.Errorf("Got response from NIRVANA (slow) instead of QUICKNODE (fast via hedge)")
// 			}
// 		} else {
// 			// Some other issue
// 			t.Errorf("Unexpected result - Status: %d, Elapsed: %v, Body: %s", statusCode, elapsed, body)
// 		}
// 	})

// 	t.Run("SimpleScenarioShowingBug", func(t *testing.T) {
// 		// Simplest possible test showing the issue:
// 		// 1. Two upstreams: one that fails first then works, one that is always slow
// 		// 2. With retry (no delay) + hedge (with delay), we expect hedge to save us
// 		// 3. But it doesn't work as expected

// 		util.ResetGock()
// 		defer util.ResetGock()
// 		util.SetupMocksForEvmStatePoller()
// 		defer util.AssertNoPendingMocks(t, 0)

// 		cfg := &common.Config{
// 			Server: &common.ServerConfig{
// 				MaxTimeout: common.Duration(10 * time.Second).Ptr(),
// 			},
// 			Projects: []*common.ProjectConfig{
// 				{
// 					Id: "test_project",
// 					Networks: []*common.NetworkConfig{
// 						{
// 							Architecture: common.ArchitectureEvm,
// 							Evm: &common.EvmNetworkConfig{
// 								ChainId: 1,
// 							},
// 							Failsafe: []*common.FailsafeConfig{
// 								{
// 									Timeout: &common.TimeoutPolicyConfig{
// 										Duration: common.Duration(10 * time.Second),
// 									},
// 									Retry: &common.RetryPolicyConfig{
// 										MaxAttempts: 2,
// 										Delay:       common.Duration(0), // No delay
// 									},
// 									Hedge: &common.HedgePolicyConfig{
// 										MaxCount: 1,
// 										Delay:    common.Duration(500 * time.Millisecond),
// 									},
// 								},
// 							},
// 						},
// 					},
// 					Upstreams: []*common.UpstreamConfig{
// 						{
// 							Id:       "good",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://good.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 						{
// 							Id:       "slow",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://slow.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 					},
// 				},
// 			},
// 			RateLimiters: &common.RateLimiterConfig{},
// 		}

// 		// Good upstream: Fails once, then works
// 		gock.New("http://good.localhost").
// 			Post("/").
// 			Times(1).
// 			Reply(500).
// 			Delay(100 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"error": map[string]interface{}{
// 					"code":    -32000,
// 					"message": "Temporary error",
// 				},
// 			})

// 		gock.New("http://good.localhost").
// 			Post("/").
// 			Persist().
// 			Reply(200).
// 			Delay(100 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  "0x123456", // Valid hex for block number
// 			})

// 		// Slow upstream: Always very slow
// 		gock.New("http://slow.localhost").
// 			Post("/").
// 			Persist().
// 			Reply(200).
// 			Delay(9 * time.Second).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  "0x654321", // Different block number
// 			})

// 		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
// 		defer shutdown()

// 		start := time.Now()
// 		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`, nil, nil)
// 		elapsed := time.Since(start)

// 		log.Info().
// 			Dur("elapsed", elapsed).
// 			Int("statusCode", statusCode).
// 			Str("body", body).
// 			Msg("Simple test result")

// 		// EXPECTED:
// 		// - Request to good upstream fails at 100ms
// 		// - Immediate retry goes to slow upstream
// 		// - Hedge after 500ms goes back to good upstream
// 		// - Good upstream responds at 600ms (500ms + 100ms)
// 		// - Total time: ~600ms
// 		//
// 		// ACTUAL (the bug):
// 		// - System waits for slow upstream (9 seconds)

// 		assert.Equal(t, http.StatusOK, statusCode)
// 		if elapsed < 2*time.Second {
// 			// This is what we want - hedge worked!
// 			assert.Contains(t, body, "0x123456", "Should get result from good upstream via hedge")
// 			t.Logf("SUCCESS: Hedge worked correctly, got response in %v", elapsed)
// 		} else {
// 			// This is the bug - we waited for slow upstream
// 			t.Errorf("BUG DEMONSTRATED: Waited %v for response (expected ~600ms)", elapsed)
// 			t.Errorf("Response body: %s", body)
// 			if strings.Contains(body, "0x654321") {
// 				t.Errorf("Got response from SLOW upstream instead of using hedge to get GOOD upstream")
// 			}
// 		}
// 	})

// 	t.Run("FAILING_ExpectedBehaviorNotWorking", func(t *testing.T) {
// 		// This test SHOULD pass but WILL fail, demonstrating the production issue.
// 		// Expected behavior:
// 		// 1. First attempt -> fast upstream (fails quickly or takes a bit)
// 		// 2. Second attempt (retry OR hedge) -> slow upstream (takes forever)
// 		// 3. Third attempt (hedge) -> back to fast upstream (works and wins)
// 		// Total time should be < 10s, definitely not 90s timeout!

// 		util.ResetGock()
// 		defer util.ResetGock()
// 		util.SetupMocksForEvmStatePoller()
// 		defer util.AssertNoPendingMocks(t, 0)

// 		// Configuration similar to production
// 		cfg := &common.Config{
// 			Server: &common.ServerConfig{
// 				MaxTimeout: common.Duration(90 * time.Second).Ptr(), // Production timeout
// 			},
// 			Projects: []*common.ProjectConfig{
// 				{
// 					Id: "test_project",
// 					Networks: []*common.NetworkConfig{
// 						{
// 							Architecture: common.ArchitectureEvm,
// 							Evm: &common.EvmNetworkConfig{
// 								ChainId: 1,
// 							},
// 							Failsafe: []*common.FailsafeConfig{
// 								{
// 									// Production-like configuration
// 									Timeout: &common.TimeoutPolicyConfig{
// 										Duration: common.Duration(90 * time.Second),
// 									},
// 									Retry: &common.RetryPolicyConfig{
// 										MaxAttempts: 4,
// 										Delay:       common.Duration(0), // No delay like in production
// 									},
// 									Hedge: &common.HedgePolicyConfig{
// 										MaxCount: 3,
// 										Delay:    common.Duration(500 * time.Millisecond), // Hedge after 500ms
// 									},
// 								},
// 							},
// 						},
// 					},
// 					Upstreams: []*common.UpstreamConfig{
// 						{
// 							Id:       "fast-upstream",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://fast.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 						{
// 							Id:       "slow-upstream",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://slow.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 					},
// 				},
// 			},
// 			RateLimiters: &common.RateLimiterConfig{},
// 		}

// 		var attemptCounter int32

// 		// Fast upstream: First attempt fails, third attempt succeeds
// 		gock.New("http://fast.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "trace_filter") {
// 					current := atomic.AddInt32(&attemptCounter, 1)
// 					log.Info().Int32("attempt", current).Str("upstream", "fast").Msg("Request to fast upstream")
// 					return current == 1 // Only match first attempt
// 				}
// 				return false
// 			}).
// 			Reply(500).
// 			Delay(200 * time.Millisecond). // Fails quickly
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"error": map[string]interface{}{
// 					"code":    -32000,
// 					"message": "Internal error - will recover",
// 				},
// 			})

// 		// Fast upstream: Third attempt succeeds (hedge should bring us back here)
// 		gock.New("http://fast.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(300 * time.Millisecond). // Fast response
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result": []interface{}{
// 					map[string]interface{}{
// 						"action": map[string]interface{}{
// 							"from": "0xFAST_UPSTREAM_SUCCESS",
// 						},
// 						"blockHash": "0xabc",
// 					},
// 				},
// 			})

// 		// Slow upstream: Takes forever (simulating the nirvana-like behavior)
// 		gock.New("http://slow.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "trace_filter") {
// 					log.Info().Str("upstream", "slow").Msg("Request to slow upstream - will take forever")
// 					return true
// 				}
// 				return false
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(85 * time.Second). // Takes almost the full timeout!
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  []interface{}{},
// 			})

// 		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
// 		defer shutdown()

// 		// Log upstream registry state
// 		if erpcInstance != nil && erpcInstance.projectsRegistry != nil {
// 			if proj, ok := erpcInstance.projectsRegistry.preparedProjects["test_project"]; ok {
// 				log.Info().
// 					Int("upstreamCount", len(proj.upstreamsRegistry.GetAllUpstreams())).
// 					Msg("Upstream registry state before request")
// 			}
// 		}

// 		start := time.Now()
// 		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"trace_filter","params":[{"fromBlock":"0x1","toBlock":"0x2"}],"id":1}`, nil, nil)
// 		elapsed := time.Since(start)

// 		log.Info().
// 			Dur("elapsed", elapsed).
// 			Int("statusCode", statusCode).
// 			Str("body", body).
// 			Int32("totalAttempts", atomic.LoadInt32(&attemptCounter)).
// 			Msg("Test completed")

// 		// EXPECTED BEHAVIOR (but this will FAIL showing the bug):
// 		// 1. First attempt to fast upstream fails at ~200ms
// 		// 2. Retry immediately goes to slow upstream (at ~200ms)
// 		// 3. Hedge kicks in at ~700ms (200ms + 500ms hedge delay) and goes back to fast upstream
// 		// 4. Fast upstream responds at ~1000ms (700ms + 300ms) and wins
// 		// Total time: ~1 second

// 		// What we expect:
// 		assert.Equal(t, http.StatusOK, statusCode, "Should get successful response")
// 		assert.Contains(t, body, "0xFAST_UPSTREAM_SUCCESS", "Should get response from fast upstream via hedge")
// 		assert.Less(t, elapsed, 5*time.Second, "Should complete in ~1 second, not timeout at 90s!")

// 		// This test FAILING proves the production issue:
// 		// The hedge policy is not working correctly with retry policy,
// 		// causing the system to wait for the slow upstream instead of
// 		// using hedge to go back to the recovered fast upstream.
// 	})

// 	t.Run("DetailedTimingScenario", func(t *testing.T) {
// 		// This test shows exactly what's happening with detailed timing logs
// 		util.ResetGock()
// 		defer util.ResetGock()
// 		util.SetupMocksForEvmStatePoller()
// 		defer util.AssertNoPendingMocks(t, 0)

// 		cfg := &common.Config{
// 			Server: &common.ServerConfig{
// 				MaxTimeout: common.Duration(30 * time.Second).Ptr(), // Shorter for testing
// 			},
// 			Projects: []*common.ProjectConfig{
// 				{
// 					Id: "test_project",
// 					Networks: []*common.NetworkConfig{
// 						{
// 							Architecture: common.ArchitectureEvm,
// 							Evm: &common.EvmNetworkConfig{
// 								ChainId: 1,
// 							},
// 							Failsafe: []*common.FailsafeConfig{
// 								{
// 									Timeout: &common.TimeoutPolicyConfig{
// 										Duration: common.Duration(30 * time.Second),
// 									},
// 									Retry: &common.RetryPolicyConfig{
// 										MaxAttempts: 3,
// 										Delay:       common.Duration(0), // Immediate retry like production
// 									},
// 									Hedge: &common.HedgePolicyConfig{
// 										MaxCount: 2,
// 										Delay:    common.Duration(1 * time.Second), // Hedge after 1 second
// 									},
// 								},
// 							},
// 						},
// 					},
// 					Upstreams: []*common.UpstreamConfig{
// 						{
// 							Id:       "quicknode-like",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://quicknode.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 						{
// 							Id:       "nirvana-like",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://nirvana.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 					},
// 				},
// 			},
// 			RateLimiters: &common.RateLimiterConfig{},
// 		}

// 		var quicknodeAttempts, nirvanaAttempts int32
// 		var requestTimes []string
// 		var mu sync.Mutex

// 		// Quicknode: First attempt has a temporary issue
// 		gock.New("http://quicknode.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "trace_filter") {
// 					attempt := atomic.AddInt32(&quicknodeAttempts, 1)
// 					mu.Lock()
// 					requestTimes = append(requestTimes, fmt.Sprintf("quicknode-attempt-%d at %v", attempt, time.Since(time.Now())))
// 					mu.Unlock()
// 					log.Info().
// 						Int32("attempt", attempt).
// 						Str("upstream", "quicknode").
// 						Msg("Quicknode request")
// 					return attempt == 1
// 				}
// 				return false
// 			}).
// 			Reply(200).
// 			Delay(500 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"error": map[string]interface{}{
// 					"code":    -32005,
// 					"message": "Rate limit exceeded temporarily",
// 				},
// 			})

// 		// Quicknode: Subsequent attempts work fine
// 		gock.New("http://quicknode.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(200 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result": []interface{}{
// 					map[string]interface{}{
// 						"action": map[string]interface{}{
// 							"from": "0xQUICKNODE_WORKS",
// 						},
// 					},
// 				},
// 			})

// 		// Nirvana: Always extremely slow
// 		gock.New("http://nirvana.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "trace_filter") {
// 					attempt := atomic.AddInt32(&nirvanaAttempts, 1)
// 					mu.Lock()
// 					requestTimes = append(requestTimes, fmt.Sprintf("nirvana-attempt-%d at %v", attempt, time.Since(time.Now())))
// 					mu.Unlock()
// 					log.Info().
// 						Int32("attempt", attempt).
// 						Str("upstream", "nirvana").
// 						Msg("Nirvana request - will be very slow")
// 					return true
// 				}
// 				return false
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(25 * time.Second). // Very slow!
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  []interface{}{},
// 			})

// 		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
// 		defer shutdown()

// 		start := time.Now()
// 		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"trace_filter","params":[{"fromBlock":"0x1","toBlock":"0x2"}],"id":1}`, nil, nil)
// 		elapsed := time.Since(start)

// 		mu.Lock()
// 		log.Info().
// 			Dur("totalElapsed", elapsed).
// 			Int("statusCode", statusCode).
// 			Str("body", body).
// 			Int32("quicknodeAttempts", atomic.LoadInt32(&quicknodeAttempts)).
// 			Int32("nirvanaAttempts", atomic.LoadInt32(&nirvanaAttempts)).
// 			Strs("requestSequence", requestTimes).
// 			Msg("Detailed timing analysis")
// 		mu.Unlock()

// 		// WHAT WE EXPECT TO HAPPEN:
// 		// T+0ms: Request starts
// 		// T+0ms: First attempt to quicknode
// 		// T+500ms: Quicknode fails with rate limit
// 		// T+500ms: Immediate retry (delay=0) goes to nirvana
// 		// T+1500ms: Hedge kicks in (1s delay) and goes back to quicknode
// 		// T+1700ms: Quicknode responds successfully (1500ms + 200ms)
// 		// Total: ~1.7 seconds

// 		// BUT WHAT ACTUALLY HAPPENS (the bug):
// 		// The system waits for nirvana to timeout instead of using the hedge response

// 		if statusCode == http.StatusOK {
// 			assert.Contains(t, body, "0xQUICKNODE_WORKS", "Should get response from quicknode via hedge")
// 			assert.Less(t, elapsed, 3*time.Second, "Should complete quickly via hedge, not wait for slow nirvana")
// 		} else {
// 			// This is likely what will happen - timeout or error
// 			t.Logf("FAILED as expected - system is waiting for slow upstream instead of using hedge")
// 			t.Logf("Status: %d, Body: %s", statusCode, body)
// 			t.Logf("Elapsed: %v (should have been ~1.7s)", elapsed)
// 		}
// 	})}

// func TestHedgeRetryInteraction(t *testing.T) {
// 	t.Run("SlowUpstreamTimeoutThenHedgeToGoodUpstream", func(t *testing.T) {
// 		// Scenario: First request goes to slow upstream that times out
// 		// Hedge should kick in and go to good upstream
// 		util.ResetGock()
// 		defer util.ResetGock()
// 		util.SetupMocksForEvmStatePoller()
// 		defer util.AssertNoPendingMocks(t, 0)

// 		cfg := &common.Config{
// 			Server: &common.ServerConfig{
// 				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
// 			},
// 			Projects: []*common.ProjectConfig{
// 				{
// 					Id: "test_project",
// 					Networks: []*common.NetworkConfig{
// 						{
// 							Architecture: common.ArchitectureEvm,
// 							Evm: &common.EvmNetworkConfig{
// 								ChainId: 1,
// 							},
// 							Failsafe: []*common.FailsafeConfig{
// 								{
// 									Timeout: &common.TimeoutPolicyConfig{
// 										Duration: common.Duration(2 * time.Second),
// 									},
// 									Retry: &common.RetryPolicyConfig{
// 										MaxAttempts: 2,
// 										Delay:       common.Duration(100 * time.Millisecond),
// 									},
// 									Hedge: &common.HedgePolicyConfig{
// 										MaxCount: 2,
// 										Delay:    common.Duration(500 * time.Millisecond),
// 									},
// 								},
// 							},
// 						},
// 					},
// 					Upstreams: []*common.UpstreamConfig{
// 						{
// 							Id:       "rpc1-slow",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://rpc1-slow.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							Failsafe: []*common.FailsafeConfig{
// 								{
// 									Timeout: &common.TimeoutPolicyConfig{
// 										Duration: common.Duration(1 * time.Second),
// 									},
// 								},
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 						{
// 							Id:       "rpc2-good",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://rpc2-good.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 					},
// 				},
// 			},
// 			RateLimiters: &common.RateLimiterConfig{},
// 		}

// 		// Slow rpc1 upstream - will timeout
// 		gock.New("http://rpc1-slow.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Reply(200).
// 			Delay(10 * time.Second). // Very slow, will timeout
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  []interface{}{},
// 			})

// 		// Good rpc2 upstream - fast response
// 		gock.New("http://rpc2-good.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Reply(200).
// 			Delay(100 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  []interface{}{
// 					map[string]interface{}{
// 						"action": map[string]interface{}{
// 							"from": "0x1234",
// 							"to":   "0x5678",
// 						},
// 						"blockHash": "0xabc",
// 						"result":    map[string]interface{}{"gasUsed": "0x5208"},
// 					},
// 				},
// 			})

// 		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
// 		defer shutdown()

// 		start := time.Now()
// 		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"trace_filter","params":[{"fromBlock":"0x1","toBlock":"0x2"}],"id":1}`, nil, nil)
// 		elapsed := time.Since(start)

// 		assert.Equal(t, http.StatusOK, statusCode)
// 		assert.Contains(t, body, "0x5208") // Result from good upstream
// 		assert.Less(t, elapsed, 2*time.Second, "Should get response from hedge before timeout")

// 		log.Info().Dur("elapsed", elapsed).Msg("Request completed")
// 	})

// 	t.Run("FastUpstreamFailsRetryToSlowHedgeBackToFastNowWorking", func(t *testing.T) {
// 		// Scenario: First request to fast upstream fails with server error
// 		// Retry goes to slow upstream which times out
// 		// BUT hedge should go back to first upstream which now works
// 		util.ResetGock()
// 		defer util.ResetGock()
// 		util.SetupMocksForEvmStatePoller()
// 		defer util.AssertNoPendingMocks(t, 0)

// 		cfg := &common.Config{
// 			Server: &common.ServerConfig{
// 				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
// 			},
// 			Projects: []*common.ProjectConfig{
// 				{
// 					Id: "test_project",
// 					Networks: []*common.NetworkConfig{
// 						{
// 							Architecture: common.ArchitectureEvm,
// 							Evm: &common.EvmNetworkConfig{
// 								ChainId: 1,
// 							},
// 							Failsafe: []*common.FailsafeConfig{
// 								{
// 									Timeout: &common.TimeoutPolicyConfig{
// 										Duration: common.Duration(3 * time.Second),
// 									},
// 									Retry: &common.RetryPolicyConfig{
// 										MaxAttempts: 3,
// 										Delay:       common.Duration(50 * time.Millisecond),
// 									},
// 									Hedge: &common.HedgePolicyConfig{
// 										MaxCount: 2,
// 										Delay:    common.Duration(300 * time.Millisecond),
// 									},
// 								},
// 							},
// 						},
// 					},
// 					Upstreams: []*common.UpstreamConfig{
// 						{
// 							Id:       "rpc1",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://rpc1.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 						{
// 							Id:       "rpc2",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://rpc2.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							Failsafe: []*common.FailsafeConfig{
// 								{
// 									Timeout: &common.TimeoutPolicyConfig{
// 										Duration: common.Duration(1 * time.Second),
// 									},
// 								},
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 					},
// 				},
// 			},
// 			RateLimiters: &common.RateLimiterConfig{},
// 		}

// 		// rpc1: First attempt fails with 500
// 		gock.New("http://rpc1.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Times(1).
// 			Reply(500).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"error": map[string]interface{}{
// 					"code":    -32000,
// 					"message": "Internal server error",
// 				},
// 			})

// 		// rpc1: Subsequent attempts succeed
// 		gock.New("http://rpc1.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Times(2).
// 			Reply(200).
// 			Delay(50 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  []interface{}{
// 					map[string]interface{}{
// 						"action": map[string]interface{}{
// 							"from": "0xrpc1_success",
// 						},
// 						"blockHash": "0xabc",
// 					},
// 				},
// 			})

// 		// rpc2: Always slow/timeout
// 		gock.New("http://rpc2.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Reply(200).
// 			Delay(10 * time.Second). // Will timeout
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  []interface{}{},
// 			})

// 		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
// 		defer shutdown()

// 		start := time.Now()
// 		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"trace_filter","params":[{"fromBlock":"0x1","toBlock":"0x2"}],"id":1}`, nil, nil)
// 		elapsed := time.Since(start)

// 		assert.Equal(t, http.StatusOK, statusCode)
// 		assert.Contains(t, body, "0xrpc1_success") // Should get result from rpc1
// 		assert.Less(t, elapsed, 2*time.Second, "Should complete before timeout")

// 		log.Info().
// 			Dur("elapsed", elapsed).
// 			Msg("Request completed")
// 	})

// 	t.Run("UpstreamExhaustionWithRecovery", func(t *testing.T) {
// 		// Scenario: All upstreams fail initially, but system should recover
// 		// This simulates the ErrUpstreamsExhausted issue from production
// 		util.ResetGock()
// 		defer util.ResetGock()
// 		util.SetupMocksForEvmStatePoller()
// 		defer util.AssertNoPendingMocks(t, 0)

// 		cfg := &common.Config{
// 			Server: &common.ServerConfig{
// 				MaxTimeout: common.Duration(10 * time.Second).Ptr(),
// 			},
// 			Projects: []*common.ProjectConfig{
// 				{
// 					Id: "test_project",
// 					Networks: []*common.NetworkConfig{
// 						{
// 							Architecture: common.ArchitectureEvm,
// 							Evm: &common.EvmNetworkConfig{
// 								ChainId: 1,
// 							},
// 							Failsafe: []*common.FailsafeConfig{
// 								{
// 									Timeout: &common.TimeoutPolicyConfig{
// 										Duration: common.Duration(2 * time.Second),
// 									},
// 									Retry: &common.RetryPolicyConfig{
// 										MaxAttempts: 4,
// 										Delay:       common.Duration(100 * time.Millisecond),
// 										Jitter:      common.Duration(50 * time.Millisecond),
// 									},
// 									Hedge: &common.HedgePolicyConfig{
// 										MaxCount: 3,
// 										Delay:    common.Duration(400 * time.Millisecond),
// 									},
// 								},
// 							},
// 						},
// 					},
// 					Upstreams: []*common.UpstreamConfig{
// 						{
// 							Id:       "upstream1",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://upstream1.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 						{
// 							Id:       "upstream2",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://upstream2.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 						{
// 							Id:       "upstream3",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://upstream3.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 					},
// 				},
// 			},
// 			RateLimiters: &common.RateLimiterConfig{},
// 		}

// 		// Upstream1: Fails first 2 times
// 		gock.New("http://upstream1.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Times(2).
// 			Reply(503).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"error": map[string]interface{}{
// 					"code":    -32000,
// 					"message": "Service unavailable",
// 				},
// 			})

// 		// Upstream1: Third+ attempts work
// 		gock.New("http://upstream1.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(100 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  []interface{}{
// 					map[string]interface{}{
// 						"from": "0xupstream1_recovered",
// 					},
// 				},
// 			})

// 		// Upstream2: First attempt times out
// 		gock.New("http://upstream2.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Times(1).
// 			Reply(200).
// 			Delay(3 * time.Second). // Force timeout
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  []interface{}{},
// 			})

// 		// Upstream2: Second+ attempts work
// 		gock.New("http://upstream2.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(150 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  []interface{}{
// 					map[string]interface{}{
// 						"from": "0xupstream2_recovered",
// 					},
// 				},
// 			})

// 		// Upstream3: Always slow/times out
// 		gock.New("http://upstream3.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(5 * time.Second).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  []interface{}{},
// 			})

// 		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
// 		defer shutdown()

// 		start := time.Now()
// 		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"trace_filter","params":[{"fromBlock":"0x1","toBlock":"0x2"}],"id":1}`, nil, nil)
// 		elapsed := time.Since(start)

// 		// Should eventually succeed through retry/hedge mechanisms
// 		assert.Equal(t, http.StatusOK, statusCode)
// 		assert.True(t,
// 			strings.Contains(body, "0xupstream1_recovered") || strings.Contains(body, "0xupstream2_recovered"),
// 			"Should get result from recovered upstream")

// 		log.Info().
// 			Dur("elapsed", elapsed).
// 			Msg("Request completed with recovery")
// 	})

// 	t.Run("ComplexRetryHedgeInteractionWithPartialFailures", func(t *testing.T) {
// 		// Complex scenario mixing timeouts, errors, and successes
// 		// This mimics production where some methods work but trace_filter specifically has issues
// 		util.ResetGock()
// 		defer util.ResetGock()
// 		util.SetupMocksForEvmStatePoller()
// 		defer util.AssertNoPendingMocks(t, 0)

// 		cfg := &common.Config{
// 			Server: &common.ServerConfig{
// 				MaxTimeout: common.Duration(8 * time.Second).Ptr(),
// 			},
// 			Projects: []*common.ProjectConfig{
// 				{
// 					Id: "test_project",
// 					Networks: []*common.NetworkConfig{
// 						{
// 							Architecture: common.ArchitectureEvm,
// 							Evm: &common.EvmNetworkConfig{
// 								ChainId: 1,
// 							},
// 							Failsafe: []*common.FailsafeConfig{
// 								{
// 									Timeout: &common.TimeoutPolicyConfig{
// 										Duration: common.Duration(3 * time.Second),
// 									},
// 									Retry: &common.RetryPolicyConfig{
// 										MaxAttempts: 3,
// 										Delay:       common.Duration(200 * time.Millisecond),
// 										BackoffMaxDelay: common.Duration(1 * time.Second),
// 										BackoffFactor:   2.0,
// 									},
// 									Hedge: &common.HedgePolicyConfig{
// 										MaxCount: 2,
// 										Delay:    common.Duration(500 * time.Millisecond),
// 									},
// 								},
// 							},
// 						},
// 					},
// 					Upstreams: []*common.UpstreamConfig{
// 						{
// 							Id:       "rpc1",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://rpc1.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 						{
// 							Id:       "rpc2",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://rpc2.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 					},
// 				},
// 			},
// 			RateLimiters: &common.RateLimiterConfig{},
// 		}

// 		// Track attempts
// 		var rpc1TraceAttempts, rpc2TraceAttempts int32
// 		var mu sync.Mutex
// 		var requestLog []string

// 		// rpc1: First attempt - Method not found
// 		gock.New("http://rpc1.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "trace_filter") {
// 					attempt := atomic.AddInt32(&rpc1TraceAttempts, 1)
// 					mu.Lock()
// 					requestLog = append(requestLog, fmt.Sprintf("rpc1-trace-%d", attempt))
// 					mu.Unlock()
// 					return attempt == 1
// 				}
// 				return false
// 			}).
// 			Reply(200).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"error": map[string]interface{}{
// 					"code":    -32601,
// 					"message": "Method not found",
// 				},
// 			})

// 		// rpc1: Second attempt - Rate limit
// 		gock.New("http://rpc1.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "trace_filter") {
// 					attempt := atomic.LoadInt32(&rpc1TraceAttempts)
// 					return attempt == 1 // This will be the second call (after first incremented)
// 				}
// 				return false
// 			}).
// 			Reply(429).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"error": map[string]interface{}{
// 					"code":    -32005,
// 					"message": "Rate limit exceeded",
// 				},
// 			})

// 		// rpc1: Third+ attempts - Success
// 		gock.New("http://rpc1.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(200 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  []interface{}{
// 					map[string]interface{}{
// 						"from": "0xrpc1_success",
// 					},
// 				},
// 			})

// 		// rpc2: Always very slow for trace_filter
// 		gock.New("http://rpc2.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "trace_filter") {
// 					attempt := atomic.AddInt32(&rpc2TraceAttempts, 1)
// 					mu.Lock()
// 					requestLog = append(requestLog, fmt.Sprintf("rpc2-trace-%d", attempt))
// 					mu.Unlock()
// 					return true
// 				}
// 				return false
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(4 * time.Second). // Always timeout-level slow
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  []interface{}{},
// 			})

// 		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
// 		defer shutdown()

// 		start := time.Now()
// 		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"trace_filter","params":[{"fromBlock":"0x1","toBlock":"0x2"}],"id":1}`, nil, nil)
// 		elapsed := time.Since(start)

// 		// Should eventually succeed
// 		assert.Equal(t, http.StatusOK, statusCode)
// 		assert.Contains(t, body, "0xrpc1_success", "Should eventually get success from rpc1")

// 		mu.Lock()
// 		log.Info().
// 			Dur("elapsed", elapsed).
// 			Int32("rpc1Attempts", atomic.LoadInt32(&rpc1TraceAttempts)).
// 			Int32("rpc2Attempts", atomic.LoadInt32(&rpc2TraceAttempts)).
// 			Strs("requestSequence", requestLog).
// 			Msg("Complex interaction completed")
// 		mu.Unlock()

// 		// Verify that we tried multiple times and used hedge
// 		assert.GreaterOrEqual(t, atomic.LoadInt32(&rpc1TraceAttempts), int32(2), "Should retry rpc1 after initial failures")
// 	})

// 	t.Run("ReproduceProductionScenarioExactly", func(t *testing.T) {
// 		// This test attempts to reproduce the exact production scenario:
// 		// - Config similar to production (from attached config.yml)
// 		// - rpc1 works but may have intermittent issues
// 		// - rpc2 is consistently slow for trace_filter
// 		// - Other methods work fine
// 		util.ResetGock()
// 		defer util.ResetGock()
// 		util.SetupMocksForEvmStatePoller()
// 		defer util.AssertNoPendingMocks(t, 0)

// 		// Using configuration values similar to production
// 		cfg := &common.Config{
// 			Server: &common.ServerConfig{
// 				MaxTimeout: common.Duration(300 * time.Second).Ptr(), // 300s like in production
// 			},
// 			Projects: []*common.ProjectConfig{
// 				{
// 					Id: "test_project",
// 					Networks: []*common.NetworkConfig{
// 						{
// 							Architecture: common.ArchitectureEvm,
// 							Evm: &common.EvmNetworkConfig{
// 								ChainId: 42161, // Arbitrum like in screenshots
// 							},
// 							Failsafe: []*common.FailsafeConfig{
// 								{
// 									Timeout: &common.TimeoutPolicyConfig{
// 										Duration: common.Duration(300 * time.Second), // From production config
// 									},
// 									Hedge: &common.HedgePolicyConfig{
// 										Quantile: 0.95,
// 										MaxCount: 3,
// 										MinDelay: common.Duration(100 * time.Millisecond),
// 										MaxDelay: common.Duration(5 * time.Second),
// 									},
// 									Retry: &common.RetryPolicyConfig{
// 										MaxAttempts: 4, // From production config
// 										Delay:       common.Duration(0),
// 									},
// 								},
// 							},
// 						},
// 					},
// 					Upstreams: []*common.UpstreamConfig{
// 						{
// 							Id:       "rpc1-arbitrum",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://rpc1-arbitrum.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 42161,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 							RateLimitBudget: "rpc1-global",
// 						},
// 						{
// 							Id:       "rpc2-arbitrum",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://rpc2-arbitrum.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 42161,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 							RateLimitBudget: "rpc2-global",
// 						},
// 					},
// 				},
// 			},
// 			RateLimiters: &common.RateLimiterConfig{
// 				Budgets: []*common.RateLimitBudgetConfig{
// 											{
// 							Id: "rpc1-global",
// 						Rules: []*common.RateLimitRuleConfig{
// 							{
// 								Method:   "*",
// 								MaxCount: 10000,
// 								Period:   common.Duration(1 * time.Second),
// 							},
// 						},
// 					},
// 											{
// 							Id: "rpc2-global",
// 						Rules: []*common.RateLimitRuleConfig{
// 							{
// 								Method:   "*",
// 								MaxCount: 10000,
// 								Period:   common.Duration(1 * time.Second),
// 							},
// 						},
// 					},
// 				},
// 			},
// 		}

// 		// rpc1: First attempt might hit rate limit
// 		gock.New("http://rpc1-arbitrum.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Times(1).
// 			Reply(200).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"error": map[string]interface{}{
// 					"code":    -32005,
// 					"message": "Request exceeded the allowed RPS",
// 				},
// 			})

// 		// rpc1: Subsequent attempts work fine
// 		gock.New("http://rpc1-arbitrum.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(500 * time.Millisecond). // Reasonable response time
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result": []interface{}{
// 					map[string]interface{}{
// 						"action": map[string]interface{}{
// 							"callType": "call",
// 							"from":     "0x742d35Cc6634C0532925a3b844Bc9e7595f0bEb",
// 							"gas":      "0x76c0",
// 							"input":    "0x",
// 							"to":       "0x742d35Cc6634C0532925a3b844Bc9e7595f0bEb",
// 							"value":    "0x0",
// 						},
// 						"blockHash":   "0x123",
// 						"blockNumber": "0x5BAD55",
// 						"result": map[string]interface{}{
// 							"gasUsed": "0x5208",
// 							"output":  "0x",
// 						},
// 						"subtraces":           0,
// 						"traceAddress":        []interface{}{},
// 						"transactionHash":     "0xabc",
// 						"transactionPosition": 0,
// 						"type":                "call",
// 					},
// 				},
// 			})

// 		// rpc2: Consistently very slow for trace_filter (mimicking production issue)
// 		gock.New("http://rpc2-arbitrum.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				return strings.Contains(string(body), "trace_filter")
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(90 * time.Second). // Simulate the extreme slowness seen in production
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  []interface{}{}, // Eventually returns but too late
// 			})

// 		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
// 		defer shutdown()

// 		// Simulate a trace_filter request like in production
// 		requestBody := `{
// 			"jsonrpc":"2.0",
// 			"method":"trace_filter",
// 			"params":[{
// 				"fromBlock":"0x1234567",
// 				"toBlock":"0x1234568",
// 				"fromAddress":["0x742d35Cc6634C0532925a3b844Bc9e7595f0bEb"],
// 				"after":0,
// 				"count":1000
// 			}],
// 			"id":1
// 		}`

// 		start := time.Now()
// 		statusCode, body := sendRequest(requestBody, nil, map[string]string{"chainId": "42161"})
// 		elapsed := time.Since(start)

// 		// Key assertions:
// 		// 1. Should NOT timeout at network level (should use hedge to get faster response)
// 		// 2. Should get response from rpc1 (the working upstream)
// 		// 3. Should complete in reasonable time (not 90+ seconds)
// 		assert.Equal(t, http.StatusOK, statusCode, "Should return OK, not timeout")
// 		assert.Contains(t, body, "0x5208", "Should contain result from rpc1")
// 		assert.Less(t, elapsed, 10*time.Second, "Should complete much faster than rpc2's 90s timeout")

// 		log.Info().
// 			Dur("elapsed", elapsed).
// 			Str("response", body).
// 			Msg("Production scenario test completed")
// 	})
// }
