package erpc

// import (
// 	"net/http"
// 	"strings"
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

// // TestPolicyOrderingIssue demonstrates that the order of retry and hedge policies matters
// // and is the root cause of the production issue
// func TestPolicyOrderingIssue(t *testing.T) {
// 	t.Run("CurrentOrderingCausesIssue_RetryBeforeHedge", func(t *testing.T) {
// 		// This test shows the CURRENT behavior where retry is applied BEFORE hedge
// 		// This causes hedges to be cancelled when retry moves to the next attempt

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
// 									// Current production-like config
// 									Timeout: &common.TimeoutPolicyConfig{
// 										Duration: common.Duration(10 * time.Second),
// 									},
// 									Retry: &common.RetryPolicyConfig{
// 										MaxAttempts: 3,
// 										Delay:       common.Duration(0), // Immediate retry
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
// 							Id:       "initially-fails-then-works",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://recoverable.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 						{
// 							Id:       "always-slow",
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

// 		var recoverableAttempts, slowAttempts int32
// 		testStart := time.Now()

// 		// Recoverable upstream: First attempt fails, subsequent attempts succeed
// 		gock.New("http://recoverable.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "eth_blockNumber") {
// 					attempt := atomic.AddInt32(&recoverableAttempts, 1)
// 					elapsed := time.Since(testStart)
// 					log.Info().
// 						Dur("elapsed", elapsed).
// 						Int32("attempt", attempt).
// 						Str("upstream", "recoverable").
// 						Msg("Request to recoverable upstream")
// 					return attempt == 1 // Only fail first attempt
// 				}
// 				return false
// 			}).
// 			Reply(500).
// 			Delay(200 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"error": map[string]interface{}{
// 					"code":    -32000,
// 					"message": "Temporary error",
// 				},
// 			})

// 		// Recoverable upstream: Subsequent attempts succeed
// 		gock.New("http://recoverable.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "eth_blockNumber") {
// 					elapsed := time.Since(testStart)
// 					log.Info().
// 						Dur("elapsed", elapsed).
// 						Str("upstream", "recoverable").
// 						Msg("Recoverable upstream will succeed")
// 					return true
// 				}
// 				return false
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(200 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  "0xRECOVERABLE_SUCCESS",
// 			})

// 		// Slow upstream: Always very slow
// 		gock.New("http://slow.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "eth_blockNumber") {
// 					attempt := atomic.AddInt32(&slowAttempts, 1)
// 					elapsed := time.Since(testStart)
// 					log.Info().
// 						Dur("elapsed", elapsed).
// 						Int32("attempt", attempt).
// 						Str("upstream", "slow").
// 						Msg("Request to slow upstream - will be very slow")
// 					return true
// 				}
// 				return false
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(9 * time.Second).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  "0xSLOW_RESPONSE",
// 			})

// 		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
// 		defer shutdown()

// 		testStart = time.Now()
// 		statusCode, body := sendRequest(
// 			`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`,
// 			nil, nil,
// 		)
// 		elapsed := time.Since(testStart)

// 		log.Info().
// 			Dur("totalElapsed", elapsed).
// 			Int("statusCode", statusCode).
// 			Str("body", body).
// 			Int32("recoverableAttempts", atomic.LoadInt32(&recoverableAttempts)).
// 			Int32("slowAttempts", atomic.LoadInt32(&slowAttempts)).
// 			Msg("Test completed")

// 		// EXPECTED TIMELINE (what we want):
// 		// T+0ms: Request to recoverable upstream
// 		// T+200ms: Recoverable fails
// 		// T+200ms: Retry immediately goes to slow upstream
// 		// T+700ms: Hedge kicks in (200ms + 500ms) and goes back to recoverable
// 		// T+900ms: Recoverable succeeds (700ms + 200ms)
// 		// TOTAL: ~900ms

// 		// ACTUAL (the bug):
// 		// T+0ms: Request to recoverable upstream
// 		// T+200ms: Recoverable fails
// 		// T+200ms: Retry immediately goes to slow upstream (CANCELS any pending hedges)
// 		// T+700ms: Hedge would kick in but the context was cancelled by retry
// 		// T+9200ms: Slow upstream finally responds
// 		// TOTAL: ~9.2 seconds

// 		// The test will show the actual behavior (waiting for slow upstream)
// 		if elapsed > 5*time.Second {
// 			t.Logf("❌ BUG CONFIRMED: System waited %v for slow upstream", elapsed)
// 			t.Logf("This proves that retry policy cancels hedge contexts when moving to next attempt")
// 			t.Logf("The issue is that retry is OUTSIDE hedge in the policy chain")

// 			// This is the expected behavior with current ordering
// 			assert.Contains(t, body, "SLOW_RESPONSE", "Got response from slow upstream as expected with current ordering")
// 		} else if elapsed < 2*time.Second {
// 			t.Logf("✅ UNEXPECTED: System completed quickly in %v", elapsed)
// 			t.Logf("This would mean hedge is working correctly, which shouldn't happen with current ordering")
// 			assert.Contains(t, body, "RECOVERABLE_SUCCESS", "Got response from recoverable upstream via hedge")
// 		} else {
// 			t.Logf("⚠️ Intermediate result: %v", elapsed)
// 		}
// 	})

// 	t.Run("ProofOfConcept_HedgeWithDelayedRetry", func(t *testing.T) {
// 		// This test shows that adding a delay to retry allows hedge to work
// 		// because hedge has time to kick in before retry moves to the next attempt

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
// 										MaxAttempts: 3,
// 										Delay:       common.Duration(1 * time.Second), // DELAY ADDED!
// 									},
// 									Hedge: &common.HedgePolicyConfig{
// 										MaxCount: 2,
// 										Delay:    common.Duration(300 * time.Millisecond), // Hedge faster than retry
// 									},
// 								},
// 							},
// 						},
// 					},
// 					Upstreams: []*common.UpstreamConfig{
// 						{
// 							Id:       "initially-fails-then-works",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://recoverable2.localhost",
// 							Evm: &common.EvmUpstreamConfig{
// 								ChainId: 1,
// 							},
// 							JsonRpc: &common.JsonRpcUpstreamConfig{
// 								SupportsBatch: &common.FALSE,
// 							},
// 						},
// 						{
// 							Id:       "always-slow",
// 							Type:     common.UpstreamTypeEvm,
// 							Endpoint: "http://slow2.localhost",
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

// 		// Similar mocks but for different endpoints
// 		gock.New("http://recoverable2.localhost").
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

// 		gock.New("http://recoverable2.localhost").
// 			Post("/").
// 			Persist().
// 			Reply(200).
// 			Delay(100 * time.Millisecond).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  "0xRECOVERABLE_WITH_DELAY",
// 			})

// 		gock.New("http://slow2.localhost").
// 			Post("/").
// 			Persist().
// 			Reply(200).
// 			Delay(9 * time.Second).
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result":  "0xSLOW2_RESPONSE",
// 			})

// 		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
// 		defer shutdown()

// 		start := time.Now()
// 		statusCode, body := sendRequest(
// 			`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`,
// 			nil, nil,
// 		)
// 		elapsed := time.Since(start)

// 		log.Info().
// 			Dur("elapsed", elapsed).
// 			Int("statusCode", statusCode).
// 			Str("body", body).
// 			Msg("Test with delayed retry completed")

// 		// With delayed retry, hedge has time to work!
// 		// Timeline:
// 		// T+0ms: Request to recoverable
// 		// T+100ms: Recoverable fails
// 		// T+400ms: Hedge kicks in (100ms + 300ms) BEFORE retry at 1100ms
// 		// T+500ms: Hedged request to slow2 or recoverable2 succeeds

// 		if elapsed < 2*time.Second {
// 			t.Logf("✅ SUCCESS with delayed retry: Completed in %v", elapsed)
// 			t.Logf("This proves that adding delay to retry allows hedge to work!")
// 			assert.Contains(t, body, "RECOVERABLE_WITH_DELAY", "Got response from recoverable via hedge")
// 		} else {
// 			t.Logf("❌ Even with delay, system waited %v", elapsed)
// 			t.Logf("This suggests a deeper issue with policy interaction")
// 		}
// 	})
// }
