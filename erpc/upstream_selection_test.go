package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// TestUpstreamSelectionWithHedgeAndRetry tests that upstream selection works correctly
// when both hedge and retry policies are configured at the network level
func TestUpstreamSelectionWithHedgeAndRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Test scenarios with different failsafe configurations
	testCases := []struct {
		name               string
		failsafeConfig     *common.FailsafeConfig
		expectedBehavior   string
		maxAcceptableDelay time.Duration
		minExpectedCalls   int
		expectedUpstreams  []string // Expected order of upstream attempts
		setupMocks         func()
	}{
		{
			name: "retry_only_no_delays",
			failsafeConfig: &common.FailsafeConfig{
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 3,
					Delay:       0, // No delay between retries
				},
			},
			expectedBehavior:   "immediate retry with no gaps",
			maxAcceptableDelay: 100 * time.Millisecond, // Should be very fast
			minExpectedCalls:   3,                      // Should try all 3 upstreams
			expectedUpstreams:  []string{"rpc1", "rpc2", "rpc3"},
			setupMocks: func() {
				// Upstream 1: Fails immediately (connection refused)
				gock.New("http://rpc1.localhost").
					Post("").
					Times(1).
					Reply(0).
					SetError(errors.New("connection refused"))

				// Upstream 2: Also fails immediately
				gock.New("http://rpc2.localhost").
					Post("").
					Times(1).
					Reply(0).
					SetError(errors.New("connection refused"))

				// Upstream 3: Succeeds
				gock.New("http://rpc3.localhost").
					Post("").
					Times(1).
					Reply(200).
					JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
						"result":  "0x1234",
					})
			},
		},
		{
			name: "hedge_and_retry_reordered",
			failsafeConfig: &common.FailsafeConfig{
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 3,
					Delay:       0, // No delay for retry
				},
				Hedge: &common.HedgePolicyConfig{
					MaxCount: 2,
					Delay:    common.Duration(200 * time.Millisecond), // Hedge after 200ms
				},
			},
			expectedBehavior:   "retry should not wait for hedge delay when failure is fast",
			maxAcceptableDelay: 100 * time.Millisecond, // Retry should be immediate, not wait for hedge
			minExpectedCalls:   3,                      // Should try all 3 upstreams
			expectedUpstreams:  []string{"rpc1", "rpc2", "rpc3"},
			setupMocks: func() {
				// Upstream 1: Fails immediately (connection refused)
				gock.New("http://rpc1.localhost").
					Post("").
					Times(1).
					Reply(0).
					SetError(errors.New("connection refused"))

				// Upstream 2: Also fails immediately
				gock.New("http://rpc2.localhost").
					Post("").
					Times(1).
					Reply(0).
					SetError(errors.New("connection refused"))

				// Upstream 3: Succeeds
				gock.New("http://rpc3.localhost").
					Post("").
					Times(1).
					Reply(200).
					JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
						"result":  "0x1234",
					})
			},
		},
		{
			name: "hedge_only_slow_upstreams",
			failsafeConfig: &common.FailsafeConfig{
				Hedge: &common.HedgePolicyConfig{
					MaxCount: 2,
					Delay:    common.Duration(100 * time.Millisecond),
				},
				Timeout: &common.TimeoutPolicyConfig{
					Duration: common.Duration(1 * time.Second),
				},
			},
			expectedBehavior:   "should hedge to additional upstreams",
			maxAcceptableDelay: 250 * time.Millisecond,   // Hedge delay + some buffer
			minExpectedCalls:   2,                        // At least 2 upstreams tried
			expectedUpstreams:  []string{"rpc1", "rpc2"}, // May include rpc3 if both fail
			setupMocks: func() {
				// All upstreams fail immediately to test hedging
				gock.New("http://rpc1.localhost").
					Post("").
					Times(1).
					Reply(0).
					SetError(errors.New("connection refused"))

				gock.New("http://rpc2.localhost").
					Post("").
					Times(1).
					Reply(0).
					SetError(errors.New("connection refused"))

				// Upstream 3: Succeeds
				gock.New("http://rpc3.localhost").
					Post("").
					Times(1).
					Reply(200).
					JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
						"result":  "0x1234",
					})
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Configure responses for each upstream
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()

			// Setup test-specific mocks
			tc.setupMocks()

			// Setup network with the failsafe config
			network := setupTestNetworkForTiming(t, ctx, tc.failsafeConfig)

			// Create request
			req := common.NewNormalizedRequest([]byte(`{
				"jsonrpc": "2.0",
				"method": "eth_getBalance",
				"params": ["0x742d35Cc6634C0532925a3b844Bc9e7595f8fA49", "latest"],
				"id": 1
			}`))

			startTime := time.Now()
			resp, err := network.Forward(ctx, req)
			totalTime := time.Since(startTime)

			// Should succeed eventually
			require.NoError(t, err)
			require.NotNil(t, resp)

			// Extract attempted upstreams from request state
			attemptedUpstreams := []string{}
			req.ErrorsByUpstream.Range(func(key, value interface{}) bool {
				upstream := key.(common.Upstream)
				attemptedUpstreams = append(attemptedUpstreams, upstream.Id())
				return true
			})

			// Add the successful upstream
			if resp.Upstream() != nil {
				attemptedUpstreams = append(attemptedUpstreams, resp.Upstream().Id())
			}

			t.Logf("Test case: %s", tc.name)
			t.Logf("Total time: %v", totalTime)
			t.Logf("Attempted upstreams: %v", attemptedUpstreams)
			t.Logf("Expected behavior: %s", tc.expectedBehavior)
			t.Logf("Response metadata - Attempts: %d, Retries: %d, Hedges: %d",
				resp.Attempts(), resp.Retries(), resp.Hedges())

			// Should have made expected number of attempts
			assert.GreaterOrEqual(t, len(attemptedUpstreams), tc.minExpectedCalls,
				"Should have made at least %d attempts", tc.minExpectedCalls)

			// Check timing for fast retry scenarios
			if tc.name == "retry_only_no_delays" || tc.name == "hedge_and_retry_reordered" {
				assert.LessOrEqual(t, totalTime, tc.maxAcceptableDelay,
					"Total time should be minimal for %s, but was %v", tc.expectedBehavior, totalTime)
			}
		})
	}
}

// TestCentralizedUpstreamRotation verifies that upstream selection is truly centralized
// and maintains order across hedge and retry attempts
func TestCentralizedUpstreamRotation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Mock 4 upstreams to test rotation
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	// All upstreams are slow (to trigger hedging)
	for i := 1; i <= 4; i++ {
		url := fmt.Sprintf("http://rpc%d.localhost", i)

		gock.New(url).
			Post("").
			Times(1). // Each should only be called once due to centralized selection
			Reply(200).
			Delay(400 * time.Millisecond). // Increased delay to ensure hedging triggers properly
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  fmt.Sprintf("0x%d", i),
			})
	}

	// Configure aggressive hedging
	failsafeConfig := &common.FailsafeConfig{
		Hedge: &common.HedgePolicyConfig{
			MaxCount: 2,                                       // Allow up to 2 hedges (3 total requests)
			Delay:    common.Duration(100 * time.Millisecond), // Hedge quickly
		},
		Timeout: &common.TimeoutPolicyConfig{
			Duration: common.Duration(2 * time.Second),
		},
	}

	network := setupTestNetworkWithFourUpstreams(t, ctx, failsafeConfig)

	req := common.NewNormalizedRequest([]byte(`{
		"jsonrpc": "2.0",
		"method": "eth_getBalance",
		"params": ["0x742d35Cc6634C0532925a3b844Bc9e7595f8fA49", "latest"],
		"id": 1
	}`))

	// Make request
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Wait a bit for any cancelled requests to be processed
	time.Sleep(50 * time.Millisecond)

	// Track all attempted upstreams from different sources
	attemptedUpstreams := make(map[string]bool)
	var erroredUpstreams []string
	var cancelledUpstreams []string

	// 1. Check ErrorsByUpstream (contains all upstreams that had errors)
	req.ErrorsByUpstream.Range(func(key, value interface{}) bool {
		upstream := key.(common.Upstream)
		err := value.(error)
		attemptedUpstreams[upstream.Id()] = true

		// All errors should be cancelled hedged requests
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamHedgeCancelled, common.ErrCodeEndpointRequestCanceled),
			"Upstream %s should have cancellation error, but got: %v", upstream.Id(), err)

		if common.HasErrorCode(err, common.ErrCodeUpstreamHedgeCancelled, common.ErrCodeEndpointRequestCanceled) {
			cancelledUpstreams = append(cancelledUpstreams, upstream.Id())
			t.Logf("Cancelled upstream %s with error: %v", upstream.Id(), err)
		} else {
			erroredUpstreams = append(erroredUpstreams, upstream.Id())
			t.Logf("Errored upstream %s with error: %v", upstream.Id(), err)
		}
		return true
	})

	// 2. Check ConsumedUpstreams (with new logic, should only contain upstreams with final responses)
	var consumedUpstreams []string
	req.ConsumedUpstreams.Range(func(key, value interface{}) bool {
		upstream := key.(common.Upstream)
		consumedUpstreams = append(consumedUpstreams, upstream.Id())
		attemptedUpstreams[upstream.Id()] = true
		return true
	})

	// 3. Add winning upstream
	winningUpstream := resp.Upstream().Id()
	attemptedUpstreams[winningUpstream] = true

	// Log what happened
	t.Logf("Winning upstream: %s", winningUpstream)
	t.Logf("Consumed upstreams (final responses): %v", consumedUpstreams)
	t.Logf("Cancelled upstreams (hedged): %v", cancelledUpstreams)
	t.Logf("Errored upstreams (other errors): %v", erroredUpstreams)
	t.Logf("Total attempted upstreams: %d", len(attemptedUpstreams))
	t.Logf("Response metadata - Attempts: %d, Retries: %d, Hedges: %d",
		resp.Attempts(), resp.Retries(), resp.Hedges())

	// With hedge delay of 100ms and response time of 400ms:
	// - Primary request starts at 0ms
	// - First hedge at 100ms
	// - Second hedge at 200ms
	// - Primary completes at 400ms, cancelling others
	// So we expect 3 attempts total (1 primary + 2 hedges)
	assert.Equal(t, 3, resp.Attempts(), "Should have made 3 attempts")
	assert.Equal(t, 2, resp.Hedges(), "Should have made 2 hedges")

	// Since all upstreams are slow (400ms) and hedge delay is 100ms,
	// we should see multiple upstreams being attempted
	assert.GreaterOrEqual(t, len(attemptedUpstreams), 3,
		"Should have attempted at least 3 upstreams due to hedging")

	// With the new logic, cancelled requests might still appear in ConsumedUpstreams
	// if they were selected but then cancelled. The key is that they were given a chance.

	// The winning upstream should definitely be in attemptedUpstreams
	assert.Contains(t, attemptedUpstreams, winningUpstream,
		"Winning upstream should be in attempted upstreams")

	// Since all upstreams have the same 400ms delay and we have 100ms hedge delay:
	// - Primary (winning) completes at 400ms
	// - First hedge starts at 100ms, gets cancelled
	// - Second hedge starts at 200ms, gets cancelled
	// So we expect 2 cancelled upstreams
	assert.Equal(t, 2, len(cancelledUpstreams),
		"Should have exactly 2 cancelled hedged requests")

	// The winning upstream should NOT be in the cancelled list
	assert.NotContains(t, cancelledUpstreams, winningUpstream,
		"Winning upstream should not be cancelled")

	// All errors should be cancellation errors (already asserted in the Range loop)
	assert.Empty(t, erroredUpstreams,
		"Should have no non-cancellation errors")

	// Verify pending mocks - with 3 attempts out of 4 upstreams, 1 should remain
	util.AssertNoPendingMocks(t, 1) // Expect 1 pending since only 3 of 4 upstreams should be called
}

// TestMixedResponseTypes tests upstream selection with various response types
func TestMixedResponseTypes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testCases := []struct {
		name             string
		setupMocks       func()
		expectedSuccess  bool
		expectedUpstream string // Which upstream should provide the final response
	}{
		{
			name: "empty_then_valid_response",
			setupMocks: func() {
				// Upstream 1: Returns empty array
				gock.New("http://rpc1.localhost").
					Post("").
					Times(1).
					Reply(200).
					JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
						"result":  []interface{}{}, // Empty array
					})

				// Upstream 2: Returns non-empty result
				gock.New("http://rpc2.localhost").
					Post("").
					Times(1).
					Reply(200).
					JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
						"result":  []interface{}{"0x1234"}, // Non-empty result
					})
				// Don't set up mock for rpc3 since retry succeeds on rpc2
			},
			expectedSuccess:  true,
			expectedUpstream: "rpc2", // Should get non-empty response from rpc2
		},
		{
			name: "execution_error_is_final",
			setupMocks: func() {
				// Only set up mock for rpc1 since execution errors don't retry
				gock.New("http://rpc1.localhost").
					Post("").
					Times(1).
					Reply(200).
					JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
						"error": map[string]interface{}{
							"code":    3,
							"message": "execution reverted",
						},
					})
				// Don't set up mocks for rpc2 and rpc3 since they won't be called
			},
			expectedSuccess:  false, // Execution error is final, no retry
			expectedUpstream: "rpc1",
		},
		{
			name: "method_not_found_then_success",
			setupMocks: func() {
				// Upstream 1: Method not found
				gock.New("http://rpc1.localhost").
					Post("").
					Times(1).
					Reply(200).
					JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
						"error": map[string]interface{}{
							"code":    -32601,
							"message": "method not found",
						},
					})

				// Upstream 2: Success
				gock.New("http://rpc2.localhost").
					Post("").
					Times(1).
					Reply(200).
					JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
						"result":  "0x1234",
					})
				// Don't set up mock for rpc3 since retry succeeds on rpc2
			},
			expectedSuccess:  true,   // Method not found now retries to next upstream (per your comment)
			expectedUpstream: "rpc2", // Should get success from rpc2
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()

			// Setup test-specific mocks
			tc.setupMocks()

			// Setup network with retry policy
			failsafeConfig := &common.FailsafeConfig{
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 3,
					Delay:       0,
				},
			}
			network := setupTestNetworkForTiming(t, ctx, failsafeConfig)

			// Create request with retryEmpty directive
			req := common.NewNormalizedRequest([]byte(`{
				"jsonrpc": "2.0",
				"method": "eth_getTransactionReceipt",
				"params": ["0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"],
				"id": 1
			}`))
			req.SetDirectives(&common.RequestDirectives{
				RetryEmpty: true,
			})

			// Make request
			resp, err := network.Forward(ctx, req)

			if tc.expectedSuccess {
				require.NoError(t, err)
				require.NotNil(t, resp)
				assert.Equal(t, tc.expectedUpstream, resp.Upstream().Id())
			} else {
				// For execution exceptions, we expect a Go error (not a response with JSON-RPC error)
				require.Error(t, err, "Should have a Go error for execution exceptions")
				require.Nil(t, resp, "Should not have a response when execution exception occurs")

				// Verify it's an execution exception
				assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointExecutionException),
					"Error should be an execution exception")

				// The error should contain information about which upstream returned it
				// Check that only the first upstream was tried (no retry)
				var upstreamsAttempted []string
				req.ErrorsByUpstream.Range(func(key, value interface{}) bool {
					upstream := key.(common.Upstream)
					upstreamsAttempted = append(upstreamsAttempted, upstream.Id())
					return true
				})
				assert.Equal(t, 1, len(upstreamsAttempted), "Should only have tried one upstream for execution exception")
				assert.Equal(t, tc.expectedUpstream, upstreamsAttempted[0], "Should have tried the expected upstream")
			}

			// Verify no pending mocks (all expected calls were made)
			util.AssertNoPendingMocks(t, 0)
		})
	}
}

// TestFourAttemptScenario tests the specific scenario with 4 attempts:
// - attempt 1 -> upstream1 fails with error fast before hedge
// - attempt 2 -> upstream2 gets a correct response but very slow which makes hedge kick in
// - attempt 3 -> upstream3 fails with error fast before next hedge
// - attempt 4 -> upstream4 responds super fast and ends up being the winning response
func TestFourAttemptScenario(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Track request timing
	var upstream2ResponseTime time.Duration = 300 * time.Millisecond // Slow enough to ensure hedge and retries complete
	var hedgeDelay time.Duration = 50 * time.Millisecond             // Quick hedge

	// Configure responses for each upstream
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	// Upstream 1: Fails immediately (connection refused) - triggers retry
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Reply(0).
		SetError(errors.New("connection refused"))

	// Upstream 2: Succeeds but very slow (300ms) - triggers hedge after 50ms
	gock.New("http://rpc2.localhost").
		Post("").
		Times(1).
		Reply(200).
		Delay(upstream2ResponseTime).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x2222",
		})

	// Upstream 3: Fails immediately when hedge triggers - triggers another retry
	gock.New("http://rpc3.localhost").
		Post("").
		Times(1).
		Reply(0).
		SetError(errors.New("connection refused"))

	// Upstream 4: Succeeds immediately - should always win
	gock.New("http://rpc4.localhost").
		Post("").
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x4444",
		})

	// Configure with hedge and retry policies
	failsafeConfig := &common.FailsafeConfig{
		Retry: &common.RetryPolicyConfig{
			MaxAttempts: 5, // Enough to reach rpc4
			Delay:       0, // No delay for retry on failure
		},
		Hedge: &common.HedgePolicyConfig{
			MaxCount: 3, // Allow multiple hedges
			Delay:    common.Duration(hedgeDelay),
		},
		Timeout: &common.TimeoutPolicyConfig{
			Duration: common.Duration(1 * time.Second),
		},
	}

	network := setupTestNetworkWithFourUpstreams(t, ctx, failsafeConfig)

	// Create request
	req := common.NewNormalizedRequest([]byte(`{
		"jsonrpc": "2.0",
		"method": "eth_getBalance",
		"params": ["0x742d35Cc6634C0532925a3b844Bc9e7595f8fA49", "latest"],
		"id": 1
	}`))

	// Execute request and measure timing
	startTime := time.Now()
	resp, err := network.Forward(ctx, req)
	totalDuration := time.Since(startTime)

	// Verify success
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Extract attempted upstreams from request state
	attemptedUpstreams := make(map[string]bool)
	var erroredUpstreams []string
	var cancelledUpstreams []string

	// Track upstreams that errored
	req.ErrorsByUpstream.Range(func(key, value interface{}) bool {
		upstream := key.(common.Upstream)
		err := value.(error)
		attemptedUpstreams[upstream.Id()] = true

		if common.HasErrorCode(err, common.ErrCodeUpstreamHedgeCancelled, common.ErrCodeEndpointRequestCanceled) {
			cancelledUpstreams = append(cancelledUpstreams, upstream.Id())
			t.Logf("Upstream %s was cancelled", upstream.Id())
		} else if common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure) {
			erroredUpstreams = append(erroredUpstreams, upstream.Id())
			t.Logf("Upstream %s had transport failure: %v", upstream.Id(), err)
		} else {
			erroredUpstreams = append(erroredUpstreams, upstream.Id())
			t.Logf("Upstream %s had other error: %v", upstream.Id(), err)
		}
		return true
	})

	// Track upstreams that were consumed (selected for attempts)
	var consumedUpstreams []string
	req.ConsumedUpstreams.Range(func(key, value interface{}) bool {
		upstream := key.(common.Upstream)
		attemptedUpstreams[upstream.Id()] = true
		consumedUpstreams = append(consumedUpstreams, upstream.Id())
		return true
	})

	// Add the successful upstream
	winningUpstream := resp.Upstream().Id()
	attemptedUpstreams[winningUpstream] = true

	// Log what happened
	t.Logf("Response metadata - Attempts: %d, Retries: %d, Hedges: %d",
		resp.Attempts(), resp.Retries(), resp.Hedges())
	t.Logf("Winning upstream: %s", winningUpstream)
	t.Logf("Errored upstreams (transport failures): %v", erroredUpstreams)
	t.Logf("Cancelled upstreams: %v", cancelledUpstreams)
	t.Logf("Consumed upstreams (still in map): %v", consumedUpstreams)
	t.Logf("Total attempted upstreams: %d", len(attemptedUpstreams))
	t.Logf("Total duration: %v", totalDuration)

	// Verify timing and result
	jrpcResp, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)

	// Extract the result as string
	var resultStr string
	if len(jrpcResp.Result) > 0 {
		err = json.Unmarshal(jrpcResp.Result, &resultStr)
		require.NoError(t, err)
	}

	// rpc4 should ALWAYS win with this timing setup
	assert.Equal(t, "rpc4", winningUpstream, "rpc4 should always win with this timing")
	assert.Equal(t, "0x4444", resultStr, "Result should be from rpc4")

	// Verify specific error expectations:
	// - rpc1 and rpc3 should fail with transport errors (connection refused)
	assert.Contains(t, erroredUpstreams, "rpc1", "rpc1 should have transport failure")
	assert.Contains(t, erroredUpstreams, "rpc3", "rpc3 should have transport failure")

	// - rpc2 might be cancelled if it's still running when rpc4 completes
	// - rpc4 succeeds, so should not be in any error list
	assert.NotContains(t, erroredUpstreams, "rpc4", "rpc4 should not have errors")
	assert.NotContains(t, cancelledUpstreams, "rpc4", "rpc4 should not be cancelled")

	// Note: rpc2 might not appear in our tracking if it was cancelled before MarkUpstreamCompleted was called
	// This is expected behavior - the upstream was selected but the request was cancelled mid-flight
	// We should have attempted at least 3 upstreams (rpc1, rpc3, rpc4)
	assert.GreaterOrEqual(t, len(attemptedUpstreams), 3, "Should have attempted at least 3 upstreams")

	// Based on the response metadata, we should see the expected number of attempts
	assert.Equal(t, 4, resp.Attempts(), "Should have made 4 attempts total")
	assert.Equal(t, 2, resp.Retries(), "Should have made 2 retries")
	assert.Equal(t, 1, resp.Hedges(), "Should have made 1 hedge")

	// Timing should show hedge worked and we finished before rpc2
	assert.Greater(t, totalDuration, hedgeDelay, "Should take at least as long as hedge delay")
	assert.Less(t, totalDuration, upstream2ResponseTime, "Should complete before rpc2 finishes (300ms)")

	// The expected flow:
	// 1. Primary: rpc1 fails immediately -> retry
	// 2. Retry: rpc2 starts (slow) -> hedge after 50ms
	// 3. Hedge: rpc3 fails immediately -> retry
	// 4. Retry: rpc4 succeeds immediately -> wins
	t.Logf("Expected flow executed correctly with rpc4 winning")
}

// Helper to setup network with timing test configuration
func setupTestNetworkForTiming(t *testing.T, ctx context.Context, failsafeConfig *common.FailsafeConfig) *Network {
	upstreamConfigs := []*common.UpstreamConfig{
		{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "rpc3",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc3.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
	}

	return setupTestNetworkWithConfig(t, ctx, upstreamConfigs, failsafeConfig)
}

// Helper to setup network with 4 upstreams
func setupTestNetworkWithFourUpstreams(t *testing.T, ctx context.Context, failsafeConfig *common.FailsafeConfig) *Network {
	upstreamConfigs := []*common.UpstreamConfig{
		{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "rpc3",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc3.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "rpc4",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc4.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
	}

	return setupTestNetworkWithConfig(t, ctx, upstreamConfigs, failsafeConfig)
}

// Helper to setup network with specific config
func setupTestNetworkWithConfig(t *testing.T, ctx context.Context, upstreamConfigs []*common.UpstreamConfig, failsafeConfig *common.FailsafeConfig) *Network {
	t.Helper()

	rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)

	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)

	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx,
		&log.Logger,
		"test",
		upstreamConfigs,
		ssr,
		rateLimitersRegistry,
		vr,
		pr,
		nil,
		metricsTracker,
		1*time.Second,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Failsafe: []*common.FailsafeConfig{failsafeConfig},
	}

	network, err := NewNetwork(
		ctx,
		&log.Logger,
		"test",
		networkConfig,
		rateLimitersRegistry,
		upstreamsRegistry,
		metricsTracker,
	)
	require.NoError(t, err)

	err = upstreamsRegistry.Bootstrap(ctx)
	require.NoError(t, err)

	err = upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
	require.NoError(t, err)

	err = network.Bootstrap(ctx)
	require.NoError(t, err)

	// Set up state pollers
	upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	for _, ups := range upsList {
		err = ups.Bootstrap(ctx)
		require.NoError(t, err)
		ups.EvmStatePoller().SuggestLatestBlock(1000)
		ups.EvmStatePoller().SuggestFinalizedBlock(900)
	}

	// Force score refresh to ensure upstreams are properly sorted
	err = upstreamsRegistry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	return network
}
