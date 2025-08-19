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
// 	"github.com/stretchr/testify/require"
// )

// func init() {
// 	util.ConfigureTestLogger()
// }

// // TestProductionIssueWithHedgeRetry demonstrates the issue reported in production
// // where trace_filter calls timeout even though good upstreams are available
// func TestProductionIssueWithHedgeRetry(t *testing.T) {
// 	t.Run("DemonstrateExpectedVsActualBehavior", func(t *testing.T) {
// 		// This test shows what SHOULD happen vs what ACTUALLY happens
// 		// when we have:
// 		// 1. A fast upstream that fails initially then recovers
// 		// 2. A slow upstream that takes forever
// 		// 3. Retry with no delay + Hedge with delay

// 		util.ResetGock()
// 		defer util.ResetGock()
// 		util.SetupMocksForEvmStatePoller()
// 		defer util.AssertNoPendingMocks(t, 0)

// 		cfg := &common.Config{
// 			Server: &common.ServerConfig{
// 				MaxTimeout: common.Duration(20 * time.Second).Ptr(),
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
// 										Duration: common.Duration(20 * time.Second),
// 									},
// 									Retry: &common.RetryPolicyConfig{
// 										MaxAttempts: 3,
// 										Delay:       common.Duration(0), // No delay - immediate retry
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
// 							Id:       "fast",
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

// 		testStartTime := time.Now()
// 		var fastAttempts, slowAttempts int32

// 		// Fast upstream: First attempt fails
// 		gock.New("http://fast.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "eth_getBlockByNumber") {
// 					attempt := atomic.AddInt32(&fastAttempts, 1)
// 					elapsed := time.Since(testStartTime)
// 					log.Info().
// 						Dur("elapsed", elapsed).
// 						Int32("attempt", attempt).
// 						Str("upstream", "fast").
// 						Msg("Request to fast upstream")
// 					return attempt == 1
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
// 					"message": "Internal error",
// 				},
// 			})

// 		// Fast upstream: Subsequent attempts succeed
// 		gock.New("http://fast.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "eth_getBlockByNumber") {
// 					elapsed := time.Since(testStartTime)
// 					log.Info().
// 						Dur("elapsed", elapsed).
// 						Str("upstream", "fast").
// 						Msg("Fast upstream will respond successfully")
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
// 				"result": map[string]interface{}{
// 					"number": "0x1234567",
// 					"hash":   "0xFAST_SUCCESS",
// 				},
// 			})

// 		// Slow upstream: Always very slow
// 		gock.New("http://slow.localhost").
// 			Post("/").
// 			Filter(func(request *http.Request) bool {
// 				body := util.SafeReadBody(request)
// 				if strings.Contains(string(body), "eth_getBlockByNumber") {
// 					attempt := atomic.AddInt32(&slowAttempts, 1)
// 					elapsed := time.Since(testStartTime)
// 					log.Info().
// 						Dur("elapsed", elapsed).
// 						Int32("attempt", attempt).
// 						Str("upstream", "slow").
// 						Msg("Request to slow upstream - will take forever")
// 					return true
// 				}
// 				return false
// 			}).
// 			Persist().
// 			Reply(200).
// 			Delay(19 * time.Second). // Takes almost the full timeout
// 			JSON(map[string]interface{}{
// 				"jsonrpc": "2.0",
// 				"id":      1,
// 				"result": map[string]interface{}{
// 					"number": "0x1234567",
// 					"hash":   "0xSLOW_RESPONSE",
// 				},
// 			})

// 		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
// 		defer shutdown()

// 		// Make sure upstreams are registered
// 		require.NotNil(t, erpcInstance)
// 		proj := erpcInstance.projectsRegistry.preparedProjects["test_project"]
// 		require.NotNil(t, proj)
// 		require.NotEmpty(t, proj.upstreamsRegistry.GetAllUpstreams())

// 		testStartTime = time.Now()
// 		statusCode, body := sendRequest(
// 			`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest",false],"id":1}`,
// 			nil, nil,
// 		)
// 		elapsed := time.Since(testStartTime)

// 		log.Info().
// 			Dur("totalElapsed", elapsed).
// 			Int("statusCode", statusCode).
// 			Str("body", body).
// 			Int32("fastAttempts", atomic.LoadInt32(&fastAttempts)).
// 			Int32("slowAttempts", atomic.LoadInt32(&slowAttempts)).
// 			Msg("Test completed")

// 		// EXPECTED TIMELINE:
// 		// T+0ms: First request to fast upstream
// 		// T+200ms: Fast upstream fails
// 		// T+200ms: Immediate retry (delay=0) goes to slow upstream
// 		// T+1200ms: Hedge kicks in (200ms + 1000ms) and goes back to fast upstream
// 		// T+1400ms: Fast upstream responds successfully (1200ms + 200ms)
// 		// TOTAL: ~1.4 seconds

// 		// ACTUAL (if bug exists):
// 		// System waits for slow upstream (19 seconds) or times out

// 		assert.Equal(t, http.StatusOK, statusCode, "Should get successful response")

// 		if elapsed < 3*time.Second {
// 			// SUCCESS: Hedge worked as expected
// 			assert.Contains(t, body, "FAST_SUCCESS", "Should get response from fast upstream via hedge")
// 			t.Logf("✅ SUCCESS: Hedge worked correctly! Response in %v (expected ~1.4s)", elapsed)
// 			t.Logf("Fast attempts: %d, Slow attempts: %d",
// 				atomic.LoadInt32(&fastAttempts),
// 				atomic.LoadInt32(&slowAttempts))
// 		} else if elapsed > 15*time.Second {
// 			// BUG: System waited for slow upstream
// 			t.Errorf("❌ BUG CONFIRMED: System waited %v for response (expected ~1.4s)", elapsed)
// 			t.Errorf("This proves the production issue: hedge is not saving us from slow upstreams")
// 			if strings.Contains(body, "SLOW_RESPONSE") {
// 				t.Errorf("Got response from SLOW upstream instead of using hedge to get FAST upstream")
// 			}
// 			t.Errorf("Fast attempts: %d, Slow attempts: %d",
// 				atomic.LoadInt32(&fastAttempts),
// 				atomic.LoadInt32(&slowAttempts))
// 		} else {
// 			// Something in between
// 			t.Logf("⚠️  Response took %v - not fast enough for hedge, not slow enough for timeout", elapsed)
// 			t.Logf("Body: %s", body)
// 		}

// 		// Additional assertion to ensure we at least tried both upstreams
// 		if atomic.LoadInt32(&fastAttempts) == 0 && atomic.LoadInt32(&slowAttempts) == 0 {
// 			t.Errorf("No upstream attempts were made - check test configuration")
// 		}
// 	})
// }
