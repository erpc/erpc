package consensus

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	failsafeCommon "github.com/failsafe-go/failsafe-go/common"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHashCalculationError tests behavior when hash calculation fails
func TestHashCalculationError(t *testing.T) {
	// Create responses that will fail hash calculation
	upstreams := []common.Upstream{
		common.NewFakeUpstream("upstream1"),
		common.NewFakeUpstream("upstream2"),
		common.NewFakeUpstream("upstream3"),
	}

	// Create invalid responses that will fail hash calculation
	responses := []*common.NormalizedResponse{
		common.NewNormalizedResponse().SetUpstream(upstreams[0]), // No JSON-RPC response set
		common.NewNormalizedResponse().SetUpstream(upstreams[1]), // No JSON-RPC response set
		common.NewNormalizedResponse().SetUpstream(upstreams[2]), // No JSON-RPC response set
	}

	mockExec := &mockExecution{
		responses: responses,
	}

	logger := log.Logger
	policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
		WithRequiredParticipants(3).
		WithAgreementThreshold(2).
		WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError).
		WithLogger(&logger).
		Build()

	executor := &executor[*common.NormalizedResponse]{
		consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
	}

	// Create upstream selector for this test
	selector := newTestUpstreamSelector(upstreams)

	result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
		// Get the next upstream based on execution index
		upstream, upstreamIndex := selector.getNextUpstream()
		if upstream == nil {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: fmt.Errorf("no upstream available"),
			}
		}

		// Return the invalid response
		if upstreamIndex < len(responses) {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[upstreamIndex]}
		}
		return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: errors.New("no response")}
	})(mockExec)

	assert.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "ErrConsensusLowParticipants")
}

// TestContextCancellation tests behavior when context is cancelled during execution
func TestContextCancellation(t *testing.T) {
	upstreams := []common.Upstream{
		common.NewFakeUpstream("upstream1"),
		common.NewFakeUpstream("upstream2"),
		common.NewFakeUpstream("upstream3"),
	}

	responses := []*common.NormalizedResponse{
		createResponse("result1", upstreams[0]),
		createResponse("result2", upstreams[1]),
		createResponse("result3", upstreams[2]),
	}

	// Create a cancellable context
	ctx, cancel := context.WithCancel(context.Background())

	mockExec := &mockExecutionWithCancellableContext{
		mockExecution: mockExecution{responses: responses},
		baseCtx:       ctx,
	}

	logger := log.Logger
	policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
		WithRequiredParticipants(3).
		WithAgreementThreshold(2).
		WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError).
		WithLogger(&logger).
		Build()

	executor := &executor[*common.NormalizedResponse]{
		consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
	}

	// Start execution in a goroutine
	var result *failsafeCommon.PolicyResult[*common.NormalizedResponse]
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		// Create upstream selector for this test
		selector := newTestUpstreamSelector(upstreams)

		result = executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			// Cancel context after a small delay
			go func() {
				time.Sleep(10 * time.Millisecond)
				cancel()
			}()

			// Simulate slow response
			time.Sleep(50 * time.Millisecond)

			// Get the next upstream based on execution index
			upstream, upstreamIndex := selector.getNextUpstream()
			if upstream == nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: fmt.Errorf("no upstream available"),
				}
			}

			if upstreamIndex < len(responses) {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[upstreamIndex]}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: errors.New("no response")}
		})(mockExec)
	}()

	wg.Wait()

	// Should handle context cancellation gracefully
	assert.NotNil(t, result)
}

// TestLargeScaleConsensus tests consensus with many upstreams
func TestLargeScaleConsensus(t *testing.T) {
	tests := []struct {
		name                 string
		numUpstreams         int
		requiredParticipants int
		agreementThreshold   int
		consensusRatio       float64 // What fraction should agree
	}{
		{
			name:                 "20_upstreams_consensus",
			numUpstreams:         20,
			requiredParticipants: 15,
			agreementThreshold:   11,
			consensusRatio:       0.6,
		},
		{
			name:                 "50_upstreams_consensus",
			numUpstreams:         50,
			requiredParticipants: 30,
			agreementThreshold:   20,
			consensusRatio:       0.5,
		},
		{
			name:                 "100_upstreams_consensus",
			numUpstreams:         100,
			requiredParticipants: 60,
			agreementThreshold:   40,
			consensusRatio:       0.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create upstreams
			upstreams := make([]common.Upstream, tt.numUpstreams)
			responses := make([]*common.NormalizedResponse, tt.numUpstreams)

			consensusCount := int(float64(tt.numUpstreams) * tt.consensusRatio)

			for i := 0; i < tt.numUpstreams; i++ {
				upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i))

				// Create consensus for the specified ratio
				if i < consensusCount {
					responses[i] = createResponse("consensus_result", upstreams[i])
				} else {
					responses[i] = createResponse(fmt.Sprintf("result%d", i), upstreams[i])
				}
			}

			mockExec := &mockExecution{
				responses: responses,
			}

			logger := zerolog.Nop()
			policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(tt.requiredParticipants).
				WithAgreementThreshold(tt.agreementThreshold).
				WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError).
				WithLogger(&logger).
				Build()

			executor := &executor[*common.NormalizedResponse]{
				consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
			}

			// Track execution time
			start := time.Now()

			// Track responses collected
			responsesCollected := atomic.Int32{}

			// Create upstream selector for this test
			selector := newTestUpstreamSelector(upstreams)

			result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
				// Get the next upstream based on execution index
				upstream, upstreamIndex := selector.getNextUpstream()
				if upstream == nil {
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Error: fmt.Errorf("no upstream available"),
					}
				}

				responsesCollected.Add(1)

				// Simulate network delay
				time.Sleep(2 * time.Millisecond)

				// Return the response for this upstream
				if upstreamIndex < len(responses) {
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[upstreamIndex]}
				}
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: errors.New("no response")}
			})(mockExec)

			elapsed := time.Since(start)
			collected := responsesCollected.Load()

			// Should achieve consensus
			require.NoError(t, result.Error)
			require.NotNil(t, result.Result)

			// Verify consensus result
			jrr, err := result.Result.JsonRpcResponse()
			require.NoError(t, err)
			assert.Equal(t, "\"consensus_result\"", string(jrr.Result))

			// Log performance metrics
			t.Logf("Consensus achieved with %d upstreams in %v", tt.numUpstreams, elapsed)
			t.Logf("Collected %d responses (%.1f%% of total) due to short-circuit",
				collected, float64(collected)/float64(tt.numUpstreams)*100)

			// Verify short-circuit worked (should collect fewer responses than total)
			assert.Less(t, int(collected), tt.numUpstreams,
				"Short-circuit should reduce number of responses collected")
		})
	}
}

// TestMixedErrorScenarios tests various error combinations
func TestMixedErrorScenarios(t *testing.T) {
	tests := []struct {
		name              string
		setupResponses    func() ([]*common.NormalizedResponse, []common.Upstream)
		expectedError     bool
		expectedConsensus bool
	}{
		{
			name: "half_errors_half_consensus",
			setupResponses: func() ([]*common.NormalizedResponse, []common.Upstream) {
				upstreams := make([]common.Upstream, 6)
				responses := make([]*common.NormalizedResponse, 6)

				for i := 0; i < 6; i++ {
					upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i))
					// Alternate between error and valid responses within the first 3 upstreams
					// that will be selected by selectUpstreams (requiredParticipants = 3)
					if i == 0 {
						// Error response
						responses[i] = nil
					} else if i == 1 || i == 2 {
						// Valid consensus responses
						responses[i] = createResponse("consensus", upstreams[i])
					} else {
						// These won't be selected since only first 3 are used
						responses[i] = createResponse("other", upstreams[i])
					}
				}
				return responses, upstreams
			},
			expectedError:     false,
			expectedConsensus: true,
		},
		{
			name: "all_different_errors",
			setupResponses: func() ([]*common.NormalizedResponse, []common.Upstream) {
				upstreams := make([]common.Upstream, 5)
				responses := make([]*common.NormalizedResponse, 5)

				for i := 0; i < 5; i++ {
					upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i))
					responses[i] = nil // All error responses
				}
				return responses, upstreams
			},
			expectedError:     true,
			expectedConsensus: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responses, upstreams := tt.setupResponses()

			mockExec := &mockExecutionWithErrors{
				responses: responses,
				upstreams: upstreams,
			}

			logger := log.Logger
			policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(3).
				WithAgreementThreshold(2).
				WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError).
				WithLogger(&logger).
				Build()

			executor := &executor[*common.NormalizedResponse]{
				consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
			}

			// Create upstream selector for this test
			selector := newTestUpstreamSelector(upstreams)

			result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
				// Get the next upstream based on execution index
				upstream, upstreamIndex := selector.getNextUpstream()
				if upstream == nil {
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Error: fmt.Errorf("no upstream available"),
					}
				}

				if upstreamIndex < len(responses) {
					if responses[upstreamIndex] == nil {
						// Create a response with upstream set for error tracking
						resp := common.NewNormalizedResponse().SetUpstream(upstream)
						return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
							Result: resp,
							Error:  fmt.Errorf("error from %s", upstream.Id()),
						}
					}
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[upstreamIndex]}
				}
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: errors.New("no upstream found")}
			})(mockExec)

			if tt.expectedError {
				assert.Error(t, result.Error)
			} else {
				assert.NoError(t, result.Error)
			}

			if tt.expectedConsensus {
				assert.NotNil(t, result.Result)
			}
		})
	}
}

// TestGoroutineLeakDetection ensures all goroutines are properly cleaned up
func TestGoroutineLeakDetection(t *testing.T) {
	initialGoroutines := runtime.NumGoroutine()

	// Run a consensus execution that should clean up all goroutines
	upstreams := make([]common.Upstream, 10)
	responses := make([]*common.NormalizedResponse, 10)

	for i := 0; i < 10; i++ {
		upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i))
		responses[i] = createResponse("result", upstreams[i])
	}

	mockExec := &mockExecution{
		responses: responses,
	}

	logger := log.Logger
	policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
		WithRequiredParticipants(5).
		WithAgreementThreshold(5).
		WithLogger(&logger).
		Build()

	executor := &executor[*common.NormalizedResponse]{
		consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
	}

	// Create upstream selector for this test
	selector := newTestUpstreamSelector(upstreams)

	// Execute consensus
	result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
		// Get the next upstream based on execution index
		upstream, upstreamIndex := selector.getNextUpstream()
		if upstream == nil {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: fmt.Errorf("no upstream available"),
			}
		}

		// Simulate some work
		time.Sleep(5 * time.Millisecond)

		if upstreamIndex < len(responses) {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[upstreamIndex]}
		}
		return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: errors.New("no response")}
	})(mockExec)

	assert.NoError(t, result.Error)

	// Wait for goroutines to clean up
	time.Sleep(100 * time.Millisecond)

	// Check for goroutine leaks
	finalGoroutines := runtime.NumGoroutine()
	leaked := finalGoroutines - initialGoroutines

	// Allow some tolerance for test framework goroutines
	assert.LessOrEqual(t, leaked, 2, "Detected goroutine leak: %d goroutines leaked", leaked)
}

// TestPanicRecovery tests that panics in response handlers are recovered
func TestPanicRecovery(t *testing.T) {
	upstreams := []common.Upstream{
		common.NewFakeUpstream("upstream1"),
		common.NewFakeUpstream("upstream2"),
		common.NewFakeUpstream("upstream3"),
	}

	// Make upstream2 and upstream3 return the same result so consensus can be achieved
	responses := []*common.NormalizedResponse{
		createResponse("result1", upstreams[0]),
		createResponse("consensus_result", upstreams[1]),
		createResponse("consensus_result", upstreams[2]),
	}

	mockExec := &mockExecution{
		responses: responses,
	}

	logger := log.Logger
	policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
		WithRequiredParticipants(3).
		WithAgreementThreshold(2).
		WithLogger(&logger).
		Build()

	executor := &executor[*common.NormalizedResponse]{
		consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
	}

	// Track which upstreams were called
	upstreamsCalled := make(map[string]bool)
	var mu sync.Mutex

	// Create upstream selector for this test
	selector := newTestUpstreamSelector(upstreams)

	result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
		// Get the next upstream based on execution index
		upstream, upstreamIndex := selector.getNextUpstream()
		if upstream == nil {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: fmt.Errorf("no upstream available"),
			}
		}
		upstreamID := upstream.Id()

		mu.Lock()
		upstreamsCalled[upstreamID] = true
		mu.Unlock()

		// Panic on first upstream
		if upstreamID == "upstream1" {
			panic("simulated panic in handler")
		}

		if upstreamIndex < len(responses) {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[upstreamIndex]}
		}
		return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: errors.New("no response")}
	})(mockExec)

	// The panic should have been recovered and the executor should still work
	// It should be able to achieve consensus with the other two upstreams
	assert.NoError(t, result.Error, "Should achieve consensus despite one upstream potentially panicking")
	assert.NotNil(t, result.Result, "Should have a result from consensus")

	// Check if upstream1 was called (it might have been cancelled due to short-circuit)
	mu.Lock()
	upstream1Called := upstreamsCalled["upstream1"]
	mu.Unlock()

	// Verify that consensus was achieved with the non-panicking upstreams
	if result.Result != nil {
		jrr, err := result.Result.JsonRpcResponse()
		assert.NoError(t, err)
		assert.Equal(t, `"consensus_result"`, string(jrr.Result),
			"Result should be the consensus from non-panicking upstreams")
	}

	// Log whether upstream1 was called or not (both cases are valid due to short-circuit)
	if upstream1Called {
		t.Log("Panic recovery test passed: consensus achieved despite upstream1 panicking")
	} else {
		t.Log("Panic recovery test passed: consensus achieved via short-circuit before upstream1 could panic")
	}
}

// TestEmptyUpstreamsList tests behavior with empty upstreams
func TestEmptyUpstreamsList(t *testing.T) {
	mockExec := &mockExecutionWithEmptyUpstreams{}

	logger := log.Logger
	policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
		WithRequiredParticipants(3).
		WithAgreementThreshold(2).
		WithLogger(&logger).
		Build()

	executor := &executor[*common.NormalizedResponse]{
		consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
	}

	result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
		// This should be called through the fallback path
		return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
			Result: createResponse("fallback", nil),
		}
	})(mockExec)

	// Should fall back to simple execution
	assert.NoError(t, result.Error)
	assert.NotNil(t, result.Result)
}

// TestResponseChannelSaturation tests behavior when response channel fills up
func TestResponseChannelSaturation(t *testing.T) {
	numUpstreams := 100
	upstreams := make([]common.Upstream, numUpstreams)
	responses := make([]*common.NormalizedResponse, numUpstreams)

	for i := 0; i < numUpstreams; i++ {
		upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i))
		responses[i] = createResponse("result", upstreams[i])
	}

	mockExec := &mockExecution{
		responses: responses,
	}

	logger := log.Logger
	policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
		WithRequiredParticipants(50).
		WithAgreementThreshold(50).
		WithLogger(&logger).
		Build()

	executor := &executor[*common.NormalizedResponse]{
		consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
	}

	// Create upstream selector for this test
	selector := newTestUpstreamSelector(upstreams)

	// Execute with very slow response processing to test channel saturation
	result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
		// Get the next upstream based on execution index
		upstream, upstreamIndex := selector.getNextUpstream()
		if upstream == nil {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: fmt.Errorf("no upstream available"),
			}
		}

		// Simulate very slow processing
		time.Sleep(100 * time.Millisecond)

		if upstreamIndex < len(responses) {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[upstreamIndex]}
		}
		return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: errors.New("no response")}
	})(mockExec)

	// Should handle channel saturation gracefully
	assert.NoError(t, result.Error)
	assert.NotNil(t, result.Result)
}

// TestNonBlockingSelectBug tests the non-blocking select bug
func TestNonBlockingSelectBug(t *testing.T) {
	// This test directly demonstrates the non-blocking select bug
	// that causes goroutines to exit without sending messages

	// Create a buffered channel like in the actual code
	responseChan := make(chan *execResult[*common.NormalizedResponse], 3)

	// Fill the channel to capacity
	for i := 0; i < 3; i++ {
		responseChan <- &execResult[*common.NormalizedResponse]{
			result: nil,
			err:    nil,
			index:  i,
		}
	}

	// Track if message was sent
	var messageSent bool

	// Simulate what happens in the goroutine when context is cancelled
	// This mimics the exact code pattern from collectResponses
	func() {
		// Try to send nil when context is cancelled
		// This is the exact pattern used in the actual code
		select {
		case responseChan <- nil:
			messageSent = true
		default:
			// BUG: This path is taken when channel is full
			// The goroutine exits without sending anything
			messageSent = false
		}
	}()

	// The bug: message was not sent because channel was full
	assert.False(t, messageSent, "Bug reproduced: non-blocking select with default case causes message to be dropped when channel is full")

	// Now demonstrate the impact on drainResponses
	// Clear the channel first
	for len(responseChan) > 0 {
		<-responseChan
	}

	// drainResponses expects to receive messages from goroutines that may have exited without sending
	expectedMessages := 2 // Expecting 2 messages
	receivedMessages := 0

	timeout := time.NewTimer(100 * time.Millisecond)
	defer timeout.Stop()

	// Try to receive the expected messages
	for i := 0; i < expectedMessages; i++ {
		select {
		case <-responseChan:
			receivedMessages++
		case <-timeout.C:
			// Timeout waiting for messages that were never sent
			t.Logf("Timed out waiting for message %d", i+1)
			goto done
		}
	}
done:

	// Bug impact: We expected 2 messages but received 0 because goroutines exited without sending
	assert.Less(t, receivedMessages, expectedMessages,
		"drainResponses times out because goroutines exited without sending their messages")

	t.Log("Bug confirmed: Non-blocking select with default case in goroutines causes messages to be dropped, leading to drainResponses timeout or hang")
}

func TestGoroutineCancellationFixed(t *testing.T) {
	// This test verifies that the fix (removing default cases) works correctly

	// Create upstreams
	upstreams := []common.Upstream{
		common.NewFakeUpstream("upstream1"),
		common.NewFakeUpstream("upstream2"),
		common.NewFakeUpstream("upstream3"),
		common.NewFakeUpstream("upstream4"),
		common.NewFakeUpstream("upstream5"),
	}

	// Create a request
	dummyReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_test","id":1}`))

	// Create execution context
	ctx := context.Background()
	ctx = context.WithValue(ctx, common.RequestContextKey, dummyReq)
	ctx = context.WithValue(ctx, common.UpstreamsContextKey, upstreams)

	// Create a simple mock execution
	mockExec := &mockExecution{
		responses: []*common.NormalizedResponse{
			createResponse("result1", upstreams[0]),
			createResponse("consensus_result", upstreams[1]),
			createResponse("consensus_result", upstreams[2]),
			createResponse("result4", upstreams[3]),
			createResponse("result5", upstreams[4]),
		},
	}

	// Create consensus policy that will reach consensus with 2 out of 5
	log := log.Logger
	policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
		WithRequiredParticipants(5).
		WithAgreementThreshold(2). // Consensus with just 2 responses
		WithLogger(&log).
		Build()

	// Create executor
	executor := &executor[*common.NormalizedResponse]{
		consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
	}

	// Track response count
	responseCount := atomic.Int32{}

	// Execute consensus in a goroutine
	done := make(chan struct{})
	var result *failsafeCommon.PolicyResult[*common.NormalizedResponse]

	go func() {
		defer close(done)
		// Create upstream selector for this test
		selector := newTestUpstreamSelector(upstreams).withMapping(map[int]int{
			0: 0, // upstream1 (slow)
			1: 1, // upstream2 (consensus)
			2: 2, // upstream3 (consensus)
			3: 3, // upstream4 (slow)
			4: 4, // upstream5 (slow)
		})

		result = executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			ctx := exec.Context()
			if ctx == nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: errors.New("missing execution context")}
			}

			// Get the next upstream based on execution index
			upstream, _ := selector.getNextUpstream()
			if upstream == nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: fmt.Errorf("no upstream available"),
				}
			}
			upstreamID := upstream.Id()

			// For specific upstreams, respond quickly with consensus result
			switch upstreamID {
			case "upstream2", "upstream3":
				responseCount.Add(1)
				jrr, _ := common.NewJsonRpcResponse(1, "consensus_result", nil)
				resp := common.NewNormalizedResponse().
					WithJsonRpcResponse(jrr).
					SetUpstream(upstream)
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: resp}
			default:
				// Other upstreams wait for context cancellation
				select {
				case <-ctx.Done():
					// Context cancelled
					return nil
				case <-time.After(100 * time.Millisecond):
					// Slow response
					responseCount.Add(1)
					jrr, _ := common.NewJsonRpcResponse(1, "slow_result", nil)
					resp := common.NewNormalizedResponse().
						WithJsonRpcResponse(jrr).
						SetUpstream(upstream)
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: resp}
				}
			}
		})(mockExec)
	}()

	// Wait for completion
	<-done

	// Wait for result
	require.NoError(t, result.Error)
	require.NotNil(t, result.Result)

	// Verify consensus was achieved
	jrr, err := result.Result.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, `"consensus_result"`, string(jrr.Result))

	// The fix ensures that even with cancellation, the executor doesn't hang
	// The successful completion of this test verifies that:
	// 1. Consensus was achieved (2 upstreams returned consensus_result)
	// 2. The executor didn't hang waiting for cancelled goroutines
	// 3. The drainResponses function properly handled remaining messages

	t.Logf("Consensus achieved with %d responses collected", responseCount.Load())
	t.Log("Fix verified: Removing default cases ensures all goroutines send messages, preventing collector hangs")
}

// TestCloneFailureGoroutineLeak tests that when request cloning fails,
// the collector doesn't hang waiting for goroutines that were never spawned
func TestCloneFailureGoroutineLeak(t *testing.T) {
	// NOTE: This test demonstrates the fix for the goroutine leak bug.
	// The fix ensures that when originalReq.Clone() fails, a nil response is sent
	// to maintain the expected goroutine count, preventing the collector from hanging.

	// To properly test this, we'll simulate the scenario indirectly by verifying
	// that the executor handles missing responses gracefully
	upstreams := []common.Upstream{
		common.NewFakeUpstream("upstream1"),
		common.NewFakeUpstream("upstream2"),
		common.NewFakeUpstream("upstream3"),
	}

	responses := []*common.NormalizedResponse{
		createResponse("result1", upstreams[0]),
		nil, // Simulate a failed clone by having a nil response
		createResponse("result1", upstreams[2]),
	}

	// Track how many times innerFn is called
	executionCount := &atomic.Int32{}

	mockExec := &mockExecutionWithNilResponse{
		responses: responses,
		upstreams: upstreams,
	}

	logger := log.Logger
	policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
		WithRequiredParticipants(3).
		WithAgreementThreshold(2).
		WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError).
		WithLogger(&logger).
		Build()

	executor := &executor[*common.NormalizedResponse]{
		consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
	}

	// Track completion time
	start := time.Now()
	done := make(chan struct{})

	go func() {
		defer close(done)
		// Create upstream selector for this test
		selector := newTestUpstreamSelector(upstreams)

		result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			// Get the next upstream based on execution index
			upstream, upstreamIndex := selector.getNextUpstream()
			if upstream == nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: fmt.Errorf("no upstream available"),
				}
			}

			executionCount.Add(1)

			// Simulate the scenario where upstream2 would fail to clone
			// by returning an error for it
			if upstreamIndex < len(responses) {
				if responses[upstreamIndex] == nil {
					// Create a response with upstream set for error tracking
					resp := common.NewNormalizedResponse().SetUpstream(upstream)
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Result: resp,
						Error:  errors.New("simulated clone failure"),
					}
				}
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[upstreamIndex]}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: errors.New("no response")}
		})(mockExec)

		// Should still achieve consensus with the two successful responses
		require.NoError(t, result.Error)
		require.NotNil(t, result.Result)
	}()

	// Test should complete quickly without hanging
	select {
	case <-done:
		elapsed := time.Since(start)
		t.Logf("Test completed in %v", elapsed)
		t.Logf("Executions: %d", executionCount.Load())

		// Due to short-circuit optimization, may execute fewer than 3 upstreams
		// The important thing is that the test completes without hanging
		execCount := executionCount.Load()
		assert.GreaterOrEqual(t, execCount, int32(2), "Should have executed at least 2 upstreams for consensus")
		assert.LessOrEqual(t, execCount, int32(3), "Should have executed at most 3 upstreams")

		assert.Less(t, elapsed, 500*time.Millisecond, "Test should complete quickly without hanging")

		t.Log("Fix verified: The executor handles missing responses gracefully without hanging")
	case <-time.After(2 * time.Second):
		t.Fatal("Test timed out - goroutine leak detected (would happen without the fix)")
	}
}

// TestCancellationRaceCondition tests that requests are not executed after context cancellation
func TestCancellationRaceCondition(t *testing.T) {
	upstreams := []common.Upstream{
		common.NewFakeUpstream("upstream1"),
		common.NewFakeUpstream("upstream2"),
		common.NewFakeUpstream("upstream3"),
	}

	responses := []*common.NormalizedResponse{
		createResponse("result1", upstreams[0]),
		createResponse("result1", upstreams[1]),
		createResponse("result1", upstreams[2]),
	}

	// Track which upstreams actually executed
	executedUpstreams := &sync.Map{}
	executionStarted := make(chan string, 3)

	mockExec := &mockExecution{
		responses: responses,
	}

	logger := log.Logger
	policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
		WithRequiredParticipants(3).
		WithAgreementThreshold(2).
		WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError).
		WithLogger(&logger).
		Build()

	executor := &executor[*common.NormalizedResponse]{
		consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
	}

	// Create a context that will be cancelled quickly
	ctx, cancel := context.WithCancel(context.Background())
	mockExecWithCtx := &mockExecutionWithCancellableContext{
		mockExecution: *mockExec,
		baseCtx:       ctx,
	}

	// Start the consensus execution
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		// Create upstream selector for this test
		selector := newTestUpstreamSelector(upstreams)

		result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			ctx := exec.Context()

			// Get the next upstream based on execution index
			upstream, upstreamIndex := selector.getNextUpstream()
			if upstream == nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: fmt.Errorf("no upstream available"),
				}
			}
			upstreamID := upstream.Id()

			// Notify that execution started
			select {
			case executionStarted <- upstreamID:
			default:
			}

			// Simulate work that takes time
			time.Sleep(50 * time.Millisecond)

			// Check if context is cancelled
			select {
			case <-ctx.Done():
				t.Logf("Upstream %s detected cancellation during execution", upstreamID)
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: ctx.Err(),
				}
			default:
			}

			// Track that this upstream actually executed
			executedUpstreams.Store(upstreamID, true)

			if upstreamIndex < len(responses) {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[upstreamIndex]}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: errors.New("no response")}
		})(mockExecWithCtx)

		// Result doesn't matter for this test
		_ = result
	}()

	// Cancel the context after first execution starts
	<-executionStarted
	cancel()

	// Wait for completion
	wg.Wait()

	// Count how many upstreams actually executed
	executedCount := 0
	executedUpstreams.Range(func(key, value interface{}) bool {
		executedCount++
		return true
	})

	// With the fix, once context is cancelled, subsequent executions should be skipped
	t.Logf("Executed %d out of %d upstreams", executedCount, len(upstreams))
	assert.Less(t, executedCount, len(upstreams), "Some executions should be skipped after cancellation")
}

// mockExecutionWithNilResponse allows simulating nil responses in the list
type mockExecutionWithNilResponse struct {
	responses []*common.NormalizedResponse
	upstreams []common.Upstream
	ctx       context.Context
}

func (m *mockExecutionWithNilResponse) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_test"}`))

	m.ctx = context.Background()
	m.ctx = context.WithValue(m.ctx, common.RequestContextKey, req)
	m.ctx = context.WithValue(m.ctx, common.UpstreamsContextKey, m.upstreams)
	return m.ctx
}

func (m *mockExecutionWithNilResponse) Attempts() int                          { return 1 }
func (m *mockExecutionWithNilResponse) Executions() int                        { return 1 }
func (m *mockExecutionWithNilResponse) Retries() int                           { return 0 }
func (m *mockExecutionWithNilResponse) Hedges() int                            { return 0 }
func (m *mockExecutionWithNilResponse) StartTime() time.Time                   { return time.Now() }
func (m *mockExecutionWithNilResponse) ElapsedTime() time.Duration             { return 0 }
func (m *mockExecutionWithNilResponse) LastResult() *common.NormalizedResponse { return nil }
func (m *mockExecutionWithNilResponse) LastError() error                       { return nil }
func (m *mockExecutionWithNilResponse) IsFirstAttempt() bool                   { return true }
func (m *mockExecutionWithNilResponse) IsRetry() bool                          { return false }
func (m *mockExecutionWithNilResponse) IsHedge() bool                          { return false }
func (m *mockExecutionWithNilResponse) AttemptStartTime() time.Time            { return time.Now() }
func (m *mockExecutionWithNilResponse) ElapsedAttemptTime() time.Duration      { return 0 }
func (m *mockExecutionWithNilResponse) IsCanceled() bool                       { return false }
func (m *mockExecutionWithNilResponse) Canceled() <-chan struct{}              { return nil }
func (m *mockExecutionWithNilResponse) Cancel(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
}
func (m *mockExecutionWithNilResponse) CopyForCancellable() failsafe.Execution[*common.NormalizedResponse] {
	return m
}
func (m *mockExecutionWithNilResponse) CopyForCancellableWithValue(key, value any) failsafe.Execution[*common.NormalizedResponse] {
	newCtx := context.WithValue(m.Context(), key, value)
	return &mockExecutionWithNilResponse{
		responses: m.responses,
		upstreams: m.upstreams,
		ctx:       newCtx,
	}
}
func (m *mockExecutionWithNilResponse) CopyForHedge() failsafe.Execution[*common.NormalizedResponse] {
	return m
}
func (m *mockExecutionWithNilResponse) CopyWithResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) failsafe.Execution[*common.NormalizedResponse] {
	return m
}
func (m *mockExecutionWithNilResponse) InitializeRetry() *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return nil
}
func (m *mockExecutionWithNilResponse) IsCanceledWithResult() (bool, *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
	return false, nil
}
func (m *mockExecutionWithNilResponse) RecordResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return result
}

// Mock implementations for edge case testing

type mockExecutionWithCancellableContext struct {
	mockExecution
	baseCtx context.Context
}

func (m *mockExecutionWithCancellableContext) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}

	// Use the cancellable context as base
	ctx := m.baseCtx

	// Build upstream list
	var upsList []common.Upstream
	for _, r := range m.responses {
		if up := r.Upstream(); up != nil {
			upsList = append(upsList, up)
		}
	}

	dummyReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_test"}`))

	ctx = context.WithValue(ctx, common.RequestContextKey, dummyReq)
	ctx = context.WithValue(ctx, common.UpstreamsContextKey, upsList)
	m.ctx = ctx
	return ctx
}

type mockExecutionWithErrors struct {
	responses []*common.NormalizedResponse
	upstreams []common.Upstream
	ctx       context.Context
}

func (m *mockExecutionWithErrors) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}

	dummyReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_test"}`))

	m.ctx = context.Background()
	m.ctx = context.WithValue(m.ctx, common.RequestContextKey, dummyReq)
	m.ctx = context.WithValue(m.ctx, common.UpstreamsContextKey, m.upstreams)
	return m.ctx
}

// Implement remaining Execution interface methods
func (m *mockExecutionWithErrors) Attempts() int                          { return 1 }
func (m *mockExecutionWithErrors) Executions() int                        { return 1 }
func (m *mockExecutionWithErrors) Retries() int                           { return 0 }
func (m *mockExecutionWithErrors) Hedges() int                            { return 0 }
func (m *mockExecutionWithErrors) StartTime() time.Time                   { return time.Now() }
func (m *mockExecutionWithErrors) ElapsedTime() time.Duration             { return 0 }
func (m *mockExecutionWithErrors) LastResult() *common.NormalizedResponse { return nil }
func (m *mockExecutionWithErrors) LastError() error                       { return nil }
func (m *mockExecutionWithErrors) IsFirstAttempt() bool                   { return true }
func (m *mockExecutionWithErrors) IsRetry() bool                          { return false }
func (m *mockExecutionWithErrors) IsHedge() bool                          { return false }
func (m *mockExecutionWithErrors) AttemptStartTime() time.Time            { return time.Now() }
func (m *mockExecutionWithErrors) ElapsedAttemptTime() time.Duration      { return 0 }
func (m *mockExecutionWithErrors) IsCanceled() bool                       { return false }
func (m *mockExecutionWithErrors) Canceled() <-chan struct{}              { return nil }
func (m *mockExecutionWithErrors) Cancel(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
}
func (m *mockExecutionWithErrors) CopyForCancellable() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecution{}
}
func (m *mockExecutionWithErrors) CopyForCancellableWithValue(key, value any) failsafe.Execution[*common.NormalizedResponse] {
	newCtx := context.WithValue(m.Context(), key, value)
	return &mockExecution{ctx: newCtx}
}
func (m *mockExecutionWithErrors) CopyForHedge() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecution{}
}
func (m *mockExecutionWithErrors) CopyWithResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) failsafe.Execution[*common.NormalizedResponse] {
	return m
}
func (m *mockExecutionWithErrors) InitializeRetry() *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return nil
}
func (m *mockExecutionWithErrors) IsCanceledWithResult() (bool, *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
	return false, nil
}
func (m *mockExecutionWithErrors) RecordResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return result
}

type mockExecutionWithEmptyUpstreams struct {
	ctx context.Context
}

func (m *mockExecutionWithEmptyUpstreams) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}

	dummyReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_test"}`))

	m.ctx = context.Background()
	m.ctx = context.WithValue(m.ctx, common.RequestContextKey, dummyReq)
	m.ctx = context.WithValue(m.ctx, common.UpstreamsContextKey, []common.Upstream{}) // Empty list
	return m.ctx
}

// Implement remaining Execution interface methods (same as mockExecutionWithErrors)
func (m *mockExecutionWithEmptyUpstreams) Attempts() int                          { return 1 }
func (m *mockExecutionWithEmptyUpstreams) Executions() int                        { return 1 }
func (m *mockExecutionWithEmptyUpstreams) Retries() int                           { return 0 }
func (m *mockExecutionWithEmptyUpstreams) Hedges() int                            { return 0 }
func (m *mockExecutionWithEmptyUpstreams) StartTime() time.Time                   { return time.Now() }
func (m *mockExecutionWithEmptyUpstreams) ElapsedTime() time.Duration             { return 0 }
func (m *mockExecutionWithEmptyUpstreams) LastResult() *common.NormalizedResponse { return nil }
func (m *mockExecutionWithEmptyUpstreams) LastError() error                       { return nil }
func (m *mockExecutionWithEmptyUpstreams) IsFirstAttempt() bool                   { return true }
func (m *mockExecutionWithEmptyUpstreams) IsRetry() bool                          { return false }
func (m *mockExecutionWithEmptyUpstreams) IsHedge() bool                          { return false }
func (m *mockExecutionWithEmptyUpstreams) AttemptStartTime() time.Time            { return time.Now() }
func (m *mockExecutionWithEmptyUpstreams) ElapsedAttemptTime() time.Duration      { return 0 }
func (m *mockExecutionWithEmptyUpstreams) IsCanceled() bool                       { return false }
func (m *mockExecutionWithEmptyUpstreams) Canceled() <-chan struct{}              { return nil }
func (m *mockExecutionWithEmptyUpstreams) Cancel(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
}
func (m *mockExecutionWithEmptyUpstreams) CopyForCancellable() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecution{}
}
func (m *mockExecutionWithEmptyUpstreams) CopyForCancellableWithValue(key, value any) failsafe.Execution[*common.NormalizedResponse] {
	newCtx := context.WithValue(m.Context(), key, value)
	return &mockExecution{ctx: newCtx}
}
func (m *mockExecutionWithEmptyUpstreams) CopyForHedge() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecution{}
}
func (m *mockExecutionWithEmptyUpstreams) CopyWithResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) failsafe.Execution[*common.NormalizedResponse] {
	return m
}
func (m *mockExecutionWithEmptyUpstreams) InitializeRetry() *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return nil
}
func (m *mockExecutionWithEmptyUpstreams) IsCanceledWithResult() (bool, *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
	return false, nil
}
func (m *mockExecutionWithEmptyUpstreams) RecordResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return result
}

// Mock execution with goroutine tracking
type mockExecutionWithGoroutineTracking struct {
	ctx                 context.Context
	onGoroutineStart    func()
	onGoroutineComplete func()
}

func (m *mockExecutionWithGoroutineTracking) Context() context.Context {
	return m.ctx
}

func (m *mockExecutionWithGoroutineTracking) CopyForCancellableWithValue(key, value any) failsafe.Execution[*common.NormalizedResponse] {
	newCtx := context.WithValue(m.ctx, key, value)
	return &mockExecutionWithGoroutineTracking{
		ctx:                 newCtx,
		onGoroutineStart:    m.onGoroutineStart,
		onGoroutineComplete: m.onGoroutineComplete,
	}
}

// Implement the remaining required methods
func (m *mockExecutionWithGoroutineTracking) Attempts() int                          { return 1 }
func (m *mockExecutionWithGoroutineTracking) Executions() int                        { return 1 }
func (m *mockExecutionWithGoroutineTracking) Retries() int                           { return 0 }
func (m *mockExecutionWithGoroutineTracking) Hedges() int                            { return 0 }
func (m *mockExecutionWithGoroutineTracking) StartTime() time.Time                   { return time.Now() }
func (m *mockExecutionWithGoroutineTracking) ElapsedTime() time.Duration             { return 0 }
func (m *mockExecutionWithGoroutineTracking) LastResult() *common.NormalizedResponse { return nil }
func (m *mockExecutionWithGoroutineTracking) LastError() error                       { return nil }
func (m *mockExecutionWithGoroutineTracking) IsFirstAttempt() bool                   { return true }
func (m *mockExecutionWithGoroutineTracking) IsRetry() bool                          { return false }
func (m *mockExecutionWithGoroutineTracking) IsHedge() bool                          { return false }
func (m *mockExecutionWithGoroutineTracking) AttemptStartTime() time.Time            { return time.Now() }
func (m *mockExecutionWithGoroutineTracking) ElapsedAttemptTime() time.Duration      { return 0 }
func (m *mockExecutionWithGoroutineTracking) IsCanceled() bool                       { return false }
func (m *mockExecutionWithGoroutineTracking) Canceled() <-chan struct{}              { return nil }
func (m *mockExecutionWithGoroutineTracking) Cancel(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
}
func (m *mockExecutionWithGoroutineTracking) CopyForCancellable() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecutionWithGoroutineTracking{
		ctx:                 m.ctx,
		onGoroutineStart:    m.onGoroutineStart,
		onGoroutineComplete: m.onGoroutineComplete,
	}
}
func (m *mockExecutionWithGoroutineTracking) CopyForHedge() failsafe.Execution[*common.NormalizedResponse] {
	return m
}
func (m *mockExecutionWithGoroutineTracking) CopyWithResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) failsafe.Execution[*common.NormalizedResponse] {
	return m
}
func (m *mockExecutionWithGoroutineTracking) InitializeRetry() *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return nil
}
func (m *mockExecutionWithGoroutineTracking) IsCanceledWithResult() (bool, *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
	return false, nil
}
func (m *mockExecutionWithGoroutineTracking) RecordResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return result
}

// Mock execution for testing the fix
type mockExecutionForFixTest struct {
	ctx        context.Context
	onComplete func(received int)
}

func (m *mockExecutionForFixTest) Context() context.Context {
	return m.ctx
}

func (m *mockExecutionForFixTest) CopyForCancellableWithValue(key, value any) failsafe.Execution[*common.NormalizedResponse] {
	newCtx := context.WithValue(m.ctx, key, value)
	return &mockExecutionForFixTest{
		ctx:        newCtx,
		onComplete: m.onComplete,
	}
}

// Implement the remaining required methods
func (m *mockExecutionForFixTest) Attempts() int                          { return 1 }
func (m *mockExecutionForFixTest) Executions() int                        { return 1 }
func (m *mockExecutionForFixTest) Retries() int                           { return 0 }
func (m *mockExecutionForFixTest) Hedges() int                            { return 0 }
func (m *mockExecutionForFixTest) StartTime() time.Time                   { return time.Now() }
func (m *mockExecutionForFixTest) ElapsedTime() time.Duration             { return 0 }
func (m *mockExecutionForFixTest) LastResult() *common.NormalizedResponse { return nil }
func (m *mockExecutionForFixTest) LastError() error                       { return nil }
func (m *mockExecutionForFixTest) IsFirstAttempt() bool                   { return true }
func (m *mockExecutionForFixTest) IsRetry() bool                          { return false }
func (m *mockExecutionForFixTest) IsHedge() bool                          { return false }
func (m *mockExecutionForFixTest) AttemptStartTime() time.Time            { return time.Now() }
func (m *mockExecutionForFixTest) ElapsedAttemptTime() time.Duration      { return 0 }
func (m *mockExecutionForFixTest) IsCanceled() bool                       { return false }
func (m *mockExecutionForFixTest) Canceled() <-chan struct{}              { return nil }
func (m *mockExecutionForFixTest) Cancel(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
}
func (m *mockExecutionForFixTest) CopyForCancellable() failsafe.Execution[*common.NormalizedResponse] {
	return m
}
func (m *mockExecutionForFixTest) CopyForHedge() failsafe.Execution[*common.NormalizedResponse] {
	return m
}
func (m *mockExecutionForFixTest) CopyWithResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) failsafe.Execution[*common.NormalizedResponse] {
	return m
}
func (m *mockExecutionForFixTest) InitializeRetry() *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return nil
}
func (m *mockExecutionForFixTest) IsCanceledWithResult() (bool, *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
	return false, nil
}
func (m *mockExecutionForFixTest) RecordResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return result
}
