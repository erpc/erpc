package erpc

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/common/script"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyEvaluator(t *testing.T) {
	logger := log.Logger

	t.Run("BasicEvaluation", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, ups2, _ := createTestNetwork(t, ctx)

		// Create eval function that selects upstreams with error rate < 0.5
		evalFn, err := script.CompileFunction(`
			(upstreams) => {
				return upstreams.filter(u => u.metrics.errorRate < 0.5);
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			EvalFunction:     evalFn,
			ResampleInterval: 200 * time.Millisecond,
			ResampleCount:    1,
		}

		mt := ntw.metricsTracker

		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")

		mt.RecordUpstreamRequest("rpc2", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc2", "evm:123", "method1", 10*time.Millisecond)

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Allow time for evaluation
		time.Sleep(100 * time.Millisecond)

		// ups1 should be inactive due to high error rate
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err)

		// ups2 should be active due to low error rate
		err = evaluator.AcquirePermit(&logger, ups2, "method1")
		assert.NoError(t, err)
	})

	t.Run("InvalidEvalFunction_NonArrayReturn", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, ups2, _ := createTestNetwork(t, ctx)

		evalFn, err := script.CompileFunction(`
			(upstreams) => {
				return "not an array";
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			EvalFunction:     evalFn,
			ResampleInterval: 200 * time.Millisecond,
			ResampleCount:    1,
		}

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, ntw.metricsTracker)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Allow time for evaluation
		time.Sleep(100 * time.Millisecond)

		// Both upstreams should be inactive due to invalid evaluation
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err)
		err = evaluator.AcquirePermit(&logger, ups2, "method1")
		assert.NoError(t, err)
	})

	t.Run("InvalidEvalFunction_InvalidObjectStructure", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, ups2, _ := createTestNetwork(t, ctx)

		evalFn, err := script.CompileFunction(`
			(upstreams) => {
				return [{ invalid: "structure" }];
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			EvalFunction:     evalFn,
			ResampleInterval: 200 * time.Millisecond,
			ResampleCount:    1,
		}

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, ntw.metricsTracker)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Allow time for evaluation
		time.Sleep(100 * time.Millisecond)

		// Both upstreams should be inactive due to invalid evaluation
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err)
		err = evaluator.AcquirePermit(&logger, ups2, "method1")
		assert.NoError(t, err)
	})

	t.Run("InvalidEvalFunction_MissingIdField", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, ups2, _ := createTestNetwork(t, ctx)

		evalFn, err := script.CompileFunction(`
			(upstreams) => {
				return upstreams.map(u => ({
					metrics: u.metrics,
					// id field intentionally omitted
				}));
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			EvalFunction:     evalFn,
			ResampleInterval: 200 * time.Millisecond,
			ResampleCount:    1,
		}

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, ntw.metricsTracker)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Allow time for evaluation
		time.Sleep(100 * time.Millisecond)

		// Both upstreams should be inactive due to invalid evaluation
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err)
		err = evaluator.AcquirePermit(&logger, ups2, "method1")
		assert.NoError(t, err)
	})

	t.Run("SamplingBehavior", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, _, _ := createTestNetwork(t, ctx)

		// Create eval function that marks all upstreams as inactive
		evalFn, err := script.CompileFunction(`
			(upstreams) => {
				return []; // Return empty array to make all upstreams inactive
			}
		`)
		require.NoError(t, err)

		resampleCount := 3
		resampleInterval := 100 * time.Millisecond
		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			EvalFunction:     evalFn,
			ResampleExcluded: true,
			ResampleInterval: resampleInterval,
			ResampleCount:    resampleCount,
		}

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, ntw.metricsTracker)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Allow time for initial evaluation
		time.Sleep(75 * time.Millisecond)

		// Initially, upstream should be inactive
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err)

		// Wait for sampling period to begin
		time.Sleep(resampleInterval)

		// During sampling period, we should get resampleCount successful permits
		for i := 0; i < resampleCount; i++ {
			err = evaluator.AcquirePermit(&logger, ups1, "method1")
			assert.NoError(t, err, "Sample permit %d should be granted", i+1)
		}

		// Next permit should be denied (sample count exhausted)
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Permit should be denied after sample count exhausted")

		// Wait for next evaluation cycle
		time.Sleep(75 * time.Millisecond)

		// Verify upstream is still inactive after sampling
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Upstream should be inactive after sampling period")

		// Wait for another sampling period
		time.Sleep(resampleInterval)

		// Verify sample counter was reset and we can sample again
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "New sampling period should allow permits again")
	})

	t.Run("UpstreamRecovery", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, _, _ := createTestNetwork(t, ctx)

		// Create eval function that selects upstreams with error rate < 0.3
		evalFn, err := script.CompileFunction(`
			(upstreams) => {
				return upstreams.filter(u => u.metrics.errorRate < 0.3);
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			EvalFunction:     evalFn,
			ResampleExcluded: true,
			ResampleInterval: 100 * time.Millisecond,
			ResampleCount:    2,
		}

		mt := ntw.metricsTracker

		// Initially set high error rate
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Allow time for initial evaluation
		time.Sleep(75 * time.Millisecond)

		// Verify upstream is initially inactive due to high error rate
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Upstream should be inactive due to high error rate")

		// Record successful requests to improve error rate
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 10*time.Millisecond)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 15*time.Millisecond)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 12*time.Millisecond)

		// Wait for next evaluation cycle plus a small buffer
		time.Sleep(75 * time.Millisecond)

		// Verify upstream is now active due to improved error rate
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Upstream should be active after error rate improves")

		// Verify it stays active
		time.Sleep(75 * time.Millisecond)
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Upstream should remain active")
	})

	t.Run("ConcurrentEvaluation", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, ups2, _ := createTestNetwork(t, ctx)

		// Create eval function that alternates between accepting all and no upstreams
		evalFn, err := script.CompileFunction(`
        let counter = 0;
        (upstreams) => {
            counter++;
            // Alternate between returning all upstreams and none
            return counter % 2 === 0 ? upstreams : [];
        }
    `)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     1 * time.Millisecond, // Fast evaluation for testing
			EvalPerMethod:    false,
			EvalFunction:     evalFn,
			ResampleInterval: 50 * time.Millisecond,
			ResampleCount:    2,
		}

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, ntw.metricsTracker)
		require.NoError(t, err)

		ctxLimited, cancelLimited := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancelLimited()

		err = evaluator.Start(ctxLimited)
		require.NoError(t, err)

		// Run multiple goroutines that continuously try to acquire permits
		const numGoroutines = 10
		const iterationsPerGoroutine = 50

		var wg sync.WaitGroup
		errorsChan := make(chan error, numGoroutines*iterationsPerGoroutine)

		// Launch goroutines for ups1
		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func(routineID int) {
				defer wg.Done()
				for j := 0; j < iterationsPerGoroutine; j++ {
					// Randomly sleep to increase chance of race conditions
					time.Sleep(time.Duration(rand.Intn(5)) * time.Millisecond)

					err := evaluator.AcquirePermit(&logger, ups1, "method1")
					if err != nil && !common.HasErrorCode(err, common.ErrCodeUpstreamExcludedByPolicy) {
						errorsChan <- fmt.Errorf("unexpected error in routine %d, iteration %d: %v", routineID, j, err)
					}
				}
			}(i)
		}

		// Launch goroutines for ups2
		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func(routineID int) {
				defer wg.Done()
				for j := 0; j < iterationsPerGoroutine; j++ {
					time.Sleep(time.Duration(rand.Intn(5)) * time.Millisecond)

					err := evaluator.AcquirePermit(&logger, ups2, "method2")
					if err != nil && !common.HasErrorCode(err, common.ErrCodeUpstreamExcludedByPolicy) {
						errorsChan <- fmt.Errorf("unexpected error in routine %d, iteration %d: %v", routineID, j, err)
					}
				}
			}(i)
		}

		// Wait for all goroutines to complete
		wg.Wait()
		close(errorsChan)

		// Check for any errors
		var errList []error
		for err := range errorsChan {
			errList = append(errList, err)
		}
		assert.Empty(t, errList, "Unexpected errors during concurrent execution: %v", errList)

		// Verify evaluator is still functioning after concurrent operations
		_ = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NotPanics(t, func() {
			_ = evaluator.AcquirePermit(&logger, ups1, "method1")
		}, "Evaluator should still function after concurrent operations")
	})

	t.Run("MetricsUpdate", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, _, _ := createTestNetwork(t, ctx)

		// Create eval function that selects upstreams with error rate < 0.4
		evalFn, err := script.CompileFunction(`
			(upstreams) => {
				return upstreams.filter(u => {
					// Handle case where metrics might be undefined
					if (!u.metrics) return false;
					return u.metrics.errorRate < 0.4;
				});
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			EvalFunction:     evalFn,
			ResampleInterval: 100 * time.Millisecond,
			ResampleCount:    2,
		}

		mt := ntw.metricsTracker

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Initially no metrics, upstream should still be permitted
		time.Sleep(75 * time.Millisecond)
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Upstream should be active with no metrics")

		// Add good metrics
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 10*time.Millisecond)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 15*time.Millisecond)

		// Wait for evaluation
		time.Sleep(75 * time.Millisecond)
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Upstream should be active with good metrics")

		// Degrade metrics
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")

		// Wait for evaluation
		time.Sleep(75 * time.Millisecond)
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Upstream should be inactive with degraded metrics")

		// Improve metrics again
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 10*time.Millisecond)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 15*time.Millisecond)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 20*time.Millisecond)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 25*time.Millisecond)

		// Wait for evaluation and sampling period
		time.Sleep(200 * time.Millisecond)
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Upstream should be active again after metrics improve")
	})

	t.Run("StateTransitions", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, _, _ := createTestNetwork(t, ctx)

		// Create eval function that uses a threshold variable to control upstream selection
		evalFn, err := script.CompileFunction(`
        let errorThreshold = 0.3;
        (upstreams) => {
            return upstreams.filter(u => {
                if (!u.metrics || !u.metrics.errorRate) return true;
                return u.metrics.errorRate < errorThreshold;
            });
        }
    `)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			EvalFunction:     evalFn,
			ResampleExcluded: true,
			ResampleInterval: 100 * time.Millisecond,
			ResampleCount:    2,
		}

		mt := ntw.metricsTracker
		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Initially upstream should be active (no metrics)
		time.Sleep(75 * time.Millisecond)
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Upstream should start in active state")

		// Transition: Active -> Inactive (add bad metrics)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")

		// Wait for evaluation
		time.Sleep(75 * time.Millisecond)
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Upstream should transition to inactive state")

		// Transition: Inactive -> Sampling (wait for sampling period)
		time.Sleep(config.ResampleInterval)

		// Should get exactly ResampleCount permits during sampling
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "First sample permit should be granted")
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Second sample permit should be granted")
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Third permit should be denied (sample count exceeded)")

		// Transition: Sampling -> Active (improve metrics during sampling)
		for i := 0; i < 10; i++ {
			mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
			mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 10*time.Millisecond)
		}

		// Wait for next evaluation after sampling
		time.Sleep(75 * time.Millisecond)
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Upstream should transition to active state")

		// Verify stable active state
		time.Sleep(75 * time.Millisecond)
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Upstream should remain in active state")

		// Transition: Active -> Inactive -> Sampling -> Inactive
		// (degrade metrics, wait for sampling, fail to improve)
		for i := 0; i < 30; i++ {
			mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
			mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
		}

		// Wait for evaluation
		time.Sleep(75 * time.Millisecond)

		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Upstream should return to inactive state after sampling")
	})

	t.Run("CordonUncordonBehavior", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, _, _, _ := createTestNetwork(t, ctx)

		// Create eval function that selects upstreams based on error rate threshold
		evalFn, err := script.CompileFunction(`
			(upstreams) => {
				return upstreams.filter(u => {
					if (!u.metrics || !u.metrics.errorRate) return true;
					return u.metrics.errorRate < 0.5;
				});
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    true, // Enable per-method evaluation
			EvalFunction:     evalFn,
			ResampleInterval: 100 * time.Millisecond,
			ResampleCount:    2,
		}

		mt := ntw.metricsTracker
		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Initially should be active (no metrics)
		time.Sleep(75 * time.Millisecond)
		metrics := mt.GetUpstreamMethodMetrics("rpc1", "evm:123", "method1")
		assert.False(t, metrics.Cordoned.Load(), "Upstream should start uncordoned")

		// Add bad metrics to trigger cordoning
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")

		// Wait for evaluation
		time.Sleep(75 * time.Millisecond)

		// Verify upstream is cordoned for method1
		metrics = mt.GetUpstreamMethodMetrics("rpc1", "evm:123", "method1")
		assert.True(t, metrics.Cordoned.Load(), "Upstream should be cordoned for method1")
		reason, ok := metrics.CordonedReason.Load().(string)
		assert.True(t, ok, "Cordon reason should be a string")
		assert.Contains(t, reason, "excluded by selection policy", "Cordon reason should indicate policy exclusion")

		// Verify different method (method2) is not cordoned
		metrics = mt.GetUpstreamMethodMetrics("rpc1", "evm:123", "method2")
		assert.False(t, metrics.Cordoned.Load(), "Different method should not be cordoned")

		// Improve metrics to trigger uncordoning
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 10*time.Millisecond)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 15*time.Millisecond)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 20*time.Millisecond)

		// Wait for evaluation and sampling period
		time.Sleep(200 * time.Millisecond)

		// Verify upstream is uncordoned
		assert.False(t, mt.IsCordoned("rpc1", "evm:123", "method1"), "Upstream should be uncordoned after metrics improve")

		// Verify cordon state persists across evaluations
		time.Sleep(100 * time.Millisecond)
		assert.False(t, mt.IsCordoned("rpc1", "evm:123", "method1"), "Upstream should remain uncordoned")
	})

	t.Run("NetworkWideCordoning", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, _, _, _ := createTestNetwork(t, ctx)

		// Create eval function that cordons all upstreams
		evalFn, err := script.CompileFunction(`
			(upstreams) => {
				return []; // Return empty array to cordon all upstreams
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false, // Test network-wide evaluation
			EvalFunction:     evalFn,
			ResampleInterval: 100 * time.Millisecond,
			ResampleCount:    2,
		}

		mt := ntw.metricsTracker
		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Create failures
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")

		// Wait for evaluation
		time.Sleep(75 * time.Millisecond)

		// Verify all methods are cordoned
		assert.True(t, mt.IsCordoned("rpc1", "evm:123", "method1"), "All methods should be cordoned")
		assert.True(t, mt.IsCordoned("rpc1", "evm:123", "method2"), "All methods should be cordoned")
	})

	t.Run("EvaluationInterval", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, _, _ := createTestNetwork(t, ctx)

		// Create eval function that counts evaluations
		evalFn, err := script.CompileFunction(`
			let evaluationCount = 0;
			(upstreams) => {
				evaluationCount++;
				// Return all upstreams when count is even, none when odd
				return evaluationCount % 2 === 0 ? upstreams : [];
			}
		`)
		require.NoError(t, err)

		evalInterval := 100 * time.Millisecond
		config := &common.SelectionPolicyConfig{
			EvalInterval:     evalInterval,
			EvalPerMethod:    false,
			EvalFunction:     evalFn,
			ResampleInterval: 200 * time.Millisecond,
			ResampleCount:    1,
		}

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, ntw.metricsTracker)
		require.NoError(t, err)

		// Test initial evaluation happens immediately
		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Check initial evaluation (should happen almost immediately)
		time.Sleep(10 * time.Millisecond)
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Initial evaluation should have occurred immediately")

		// Wait for next evaluation cycle
		time.Sleep(evalInterval)
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Second evaluation should have different result")

		// Wait for third evaluation cycle
		time.Sleep(evalInterval)
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Third evaluation should have different result")

		// Test evaluation stops when context is cancelled
		cancel()
		time.Sleep(evalInterval * 2)

		// Record current permit state
		initialPermitState := evaluator.AcquirePermit(&logger, ups1, "method1")

		// Wait for what would have been multiple evaluation cycles
		time.Sleep(evalInterval * 3)

		// Verify permit state hasn't changed after context cancellation
		laterPermitState := evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Equal(t,
			initialPermitState != nil,
			laterPermitState != nil,
			"Permit state should not change after context cancellation")

		// Test starting a new evaluator after cancellation
		ctx2, cancel2 := context.WithTimeout(context.Background(), evalInterval*5)
		defer cancel2()

		evaluator2, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, ntw.metricsTracker)
		require.NoError(t, err)

		err = evaluator2.Start(ctx2)
		require.NoError(t, err)

		// Verify the new evaluator is working
		var lastResult error
		var changes int
		checkInterval := evalInterval / 4
		deadline := time.Now().Add(evalInterval * 3)

		// Monitor for at least 2 state changes to confirm periodic evaluation
		for time.Now().Before(deadline) && changes < 2 {
			currentResult := evaluator2.AcquirePermit(&logger, ups1, "method1")
			if lastResult == nil && currentResult != nil || lastResult != nil && currentResult == nil {
				changes++
				lastResult = currentResult
			}
			time.Sleep(checkInterval)
		}

		assert.GreaterOrEqual(t, changes, 2, "Should observe at least 2 state changes during periodic evaluation")
	})

	t.Run("ZeroSampleCount", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, _, _ := createTestNetwork(t, ctx)

		// Create eval function that excludes all upstreams
		evalFn, err := script.CompileFunction(`
			(upstreams) => {
				return []; // Return empty array to exclude all upstreams
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			EvalFunction:     evalFn,
			ResampleInterval: 100 * time.Millisecond,
			ResampleCount:    0, // Zero sample count
		}

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, ntw.metricsTracker)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Wait for initial evaluation and sampling period
		time.Sleep(150 * time.Millisecond)

		// Should remain inactive even during sampling period due to zero sample count
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Upstream should remain inactive with zero sample count")
	})

	t.Run("HighSampleCount", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, _, _ := createTestNetwork(t, ctx)

		// Create eval function that excludes all upstreams
		evalFn, err := script.CompileFunction(`
			(upstreams) => {
				return []; // Return empty array to exclude all upstreams
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			EvalFunction:     evalFn,
			ResampleExcluded: true,
			ResampleInterval: 100 * time.Millisecond,
			ResampleCount:    1000, // Very high sample count
		}

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, ntw.metricsTracker)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Wait for initial evaluation and sampling period
		time.Sleep(150 * time.Millisecond)

		// Should be able to get many permits during sampling
		successCount := 0
		for i := 0; i < 100; i++ {
			err = evaluator.AcquirePermit(&logger, ups1, "method1")
			if err == nil {
				successCount++
			}
		}
		assert.Equal(t, 100, successCount, "Should grant all permits during sampling with high sample count")
	})

	t.Run("ExtremeSampleInterval", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, _, _ := createTestNetwork(t, ctx)

		// Create eval function that excludes all upstreams
		evalFn1, err := script.CompileFunction(`
			(upstreams) => {
				return []; // Return empty array to exclude all upstreams
			}
		`)
		require.NoError(t, err)

		// Test very short sample after
		shortConfig := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			EvalFunction:     evalFn1,
			ResampleExcluded: true,
			ResampleInterval: 1 * time.Millisecond, // Very short
			ResampleCount:    2,
		}

		evaluator, err := NewPolicyEvaluator("evm:123", &logger, shortConfig, ntw.upstreamsRegistry, ntw.metricsTracker)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Wait for initial evaluation
		time.Sleep(75 * time.Millisecond)

		// Should still get correct number of samples even with very short sample after
		samplesGranted := 0
		for i := 0; i < 3; i++ {
			err = evaluator.AcquirePermit(&logger, ups1, "method1")
			if err == nil {
				samplesGranted++
			}
		}
		assert.Equal(t, 2, samplesGranted, "Should grant exactly ResampleCount permits with very short ResampleInterval")

		evalFn2, err := script.CompileFunction(`
			(upstreams) => {
				return []; // Return empty array to exclude all upstreams
			}
		`)
		require.NoError(t, err)

		// Test very long sample after
		longConfig := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			EvalFunction:     evalFn2,
			ResampleInterval: 24 * time.Hour, // Very long
			ResampleCount:    2,
		}

		evaluator, err = NewPolicyEvaluator("evm:123", &logger, longConfig, ntw.upstreamsRegistry, ntw.metricsTracker)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Wait for initial evaluation
		time.Sleep(75 * time.Millisecond)

		// Should remain inactive due to long sample after period
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Upstream should remain inactive with very long ResampleInterval")
	})

	t.Run("MetricsRaceCondition", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctxRoot, cancelRoot := context.WithCancel(context.Background())
		defer cancelRoot()
		ntw, ups1, _, _ := createTestNetwork(t, ctxRoot)

		// Create eval function that introduces artificial delay to increase chance of race conditions
		evalFn, err := script.CompileFunction(`
			(upstreams) => {
				// Artificial delay during evaluation
				const start = Date.now();
				while (Date.now() - start < 50) {} // 50ms delay
				return upstreams.filter(u => {
					if (!u.metrics) return true;
					return u.metrics.errorRate < 0.4;
				});
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     100 * time.Millisecond,
			EvalPerMethod:    false,
			EvalFunction:     evalFn,
			ResampleExcluded: true,
			ResampleInterval: 200 * time.Millisecond,
			ResampleCount:    2,
		}

		mt := ntw.metricsTracker
		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		ctxLimited, cancelLimited := context.WithTimeout(ctxRoot, 2*time.Second)
		defer cancelLimited()

		err = evaluator.Start(ctxLimited)
		require.NoError(t, err)

		// Wait for initial evaluation
		time.Sleep(150 * time.Millisecond)

		// Launch goroutine that continuously updates metrics
		updateDone := make(chan struct{})
		go func() {
			defer close(updateDone)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()

			errorRate := 0.0
			increasing := true

			for {
				select {
				case <-ctxLimited.Done():
					return
				case <-ticker.C:
					// Update error rate
					if increasing {
						errorRate += 0.1
						if errorRate >= 0.9 {
							increasing = false
						}
					} else {
						errorRate -= 0.1
						if errorRate <= 0.1 {
							increasing = true
						}
					}

					// Record new metrics
					mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
					if rand.Float64() < errorRate {
						mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
						mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
						mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
						mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
					} else {
						mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", time.Duration(rand.Intn(100))*time.Millisecond)
					}
				}
			}
		}()

		// Launch goroutine that periodically deletes metrics
		deleteDone := make(chan struct{})
		go func() {
			defer close(deleteDone)
			ticker := time.NewTicker(300 * time.Millisecond)
			defer ticker.Stop()

			for {
				select {
				case <-ctxLimited.Done():
					return
				case <-ticker.C:
					// Delete metrics for method1
					mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
					metrics := mt.GetUpstreamMethodMetrics("rpc1", "evm:123", "method1")
					metrics.Reset()
					// Small sleep to allow evaluation to potentially happen with no metrics
					time.Sleep(20 * time.Millisecond)
				}
			}
		}()

		// Launch goroutine that adds new method metrics
		addDone := make(chan struct{})
		go func() {
			defer close(addDone)
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()

			methodCounter := 2
			for {
				select {
				case <-ctxLimited.Done():
					return
				case <-ticker.C:
					methodName := fmt.Sprintf("method%d", methodCounter)
					mt.RecordUpstreamRequest("rpc1", "evm:123", methodName)
					mt.RecordUpstreamDuration("rpc1", "evm:123", methodName, 50*time.Millisecond)
					methodCounter++
				}
			}
		}()

		// Monitor permit acquisition during concurrent operations
		var permitResults []error
		resultMutex := sync.Mutex{}

		// Continuously try to acquire permits
		monitorDone := make(chan struct{})
		go func() {
			defer close(monitorDone)
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()

			for {
				select {
				case <-ctxLimited.Done():
					return
				case <-ticker.C:
					err := evaluator.AcquirePermit(&logger, ups1, "method1")
					resultMutex.Lock()
					permitResults = append(permitResults, err)
					resultMutex.Unlock()
				}
			}
		}()

		// Let the test run for a while
		time.Sleep(2000 * time.Millisecond)
		cancelRoot()

		// Wait for all goroutines to finish
		<-updateDone
		<-deleteDone
		<-addDone
		<-monitorDone

		// Analyze results
		resultMutex.Lock()
		defer resultMutex.Unlock()

		// Verify we got a mix of successful and failed permits
		successCount := 0
		failureCount := 0
		for _, err := range permitResults {
			if err == nil {
				successCount++
			} else {
				failureCount++
			}
		}

		// We should see both successes and failures due to changing metrics
		assert.Greater(t, successCount, 0, "Should have some successful permits")
		assert.Greater(t, failureCount, 0, "Should have some failed permits")

		// Verify evaluator is still functional after all concurrent operations
		assert.NotPanics(t, func() {
			_ = evaluator.AcquirePermit(&logger, ups1, "method1")
		}, "Evaluator should still be functional after concurrent operations")

		// Final evaluation should complete without panic
		assert.NotPanics(t, func() {
			time.Sleep(config.EvalInterval)
			_ = evaluator.AcquirePermit(&logger, ups1, "method1")
		}, "Final evaluation should complete without panic")
	})

	t.Run("MethodSpecificDifferentMethodStates", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, _, _ := createTestNetwork(t, ctx)

		// Create eval function that uses different error thresholds per method
		evalFn, err := script.CompileFunction(`
			(upstreams, method) => {
				const thresholds = {
					"method1": 0.3,
					"method2": 0.6,
					"method3": 0.1
				};
				const threshold = thresholds[method] || 0.5; // Default threshold
				return upstreams.filter(u => {
					if (!u.metrics || !u.metrics.errorRate) return true;
					return u.metrics.errorRate < threshold;
				});
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    true, // Enable per-method evaluation
			EvalFunction:     evalFn,
			ResampleInterval: 100 * time.Millisecond,
			ResampleCount:    2,
		}

		mt := ntw.metricsTracker
		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Set error rate of 0.4 (should be active for method2, inactive for method3)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method2")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method2")
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method2")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method2", 10*time.Millisecond)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method3")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method3")
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method3")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method3", 10*time.Millisecond)

		// Wait for evaluation
		time.Sleep(75 * time.Millisecond)

		// Verify different states for different methods
		err = evaluator.AcquirePermit(&logger, ups1, "method2")
		assert.NoError(t, err, "Should be active for method2 (threshold 0.6)")

		err = evaluator.AcquirePermit(&logger, ups1, "method3")
		assert.Error(t, err, "Should be inactive for method3 (threshold 0.1)")
	})

	t.Run("MethodSpecificMetricsIsolation", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, _, _ := createTestNetwork(t, ctx)

		evalFn, err := script.CompileFunction(`
			(upstreams, method) => {
				return upstreams.filter(u => {
					if (!u.metrics || !u.metrics.errorRate) return true;
					return u.metrics.errorRate < 0.5;
				});
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    true,
			EvalFunction:     evalFn,
			ResampleInterval: 100 * time.Millisecond,
			ResampleCount:    2,
		}

		mt := ntw.metricsTracker
		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Set high error rate for method1
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")

		// Set low error rate for method2
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method2")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method2", 10*time.Millisecond)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method2")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method2", 15*time.Millisecond)

		// Wait for evaluation
		time.Sleep(75 * time.Millisecond)

		// Verify metrics isolation
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Should be inactive for method1")

		err = evaluator.AcquirePermit(&logger, ups1, "method2")
		assert.NoError(t, err, "Should be active for method2")
	})

	t.Run("MethodSpecificGlobalFallback", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, _, _ := createTestNetwork(t, ctx)

		evalFn, err := script.CompileFunction(`
			(upstreams, method) => {
				if (method === "method1") {
					throw new Error("Intentional evaluation failure");
				}
				return upstreams.filter(u => {
					if (!u.metrics || !u.metrics.errorRate) return true;
					return u.metrics.errorRate < 0.5;
				});
			}
		`)
		require.NoError(t, err)

		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    true,
			EvalFunction:     evalFn,
			ResampleInterval: 100 * time.Millisecond,
			ResampleCount:    2,
		}

		mt := ntw.metricsTracker
		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Set good metrics for global (*) method
		mt.RecordUpstreamRequest("rpc1", "evm:123", "*")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "*", 10*time.Millisecond)

		// Wait for evaluation
		time.Sleep(75 * time.Millisecond)

		// Verify fallback to global evaluation when method-specific fails
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Should fallback to global evaluation")
	})

	t.Run("DefaultPolicyActivateFallbackWhenDefaultsUnhealthy", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, ups2, _ := createTestNetwork(t, ctx)

		// Create config with default policy
		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			ResampleInterval: 100 * time.Millisecond,
			ResampleCount:    2,
		}
		config.SetDefaults() // This will set the default policy function

		mt := ntw.metricsTracker
		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Test Case 1: All default upstreams healthy
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 10*time.Millisecond)
		mt.SetLatestBlockNumber("rpc1", "evm:123", 100)
		mt.SetLatestBlockNumber("*", "evm:123", 105) // Network head

		mt.RecordUpstreamRequest("rpc2", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc2", "evm:123", "method1", 15*time.Millisecond)
		mt.SetLatestBlockNumber("rpc2", "evm:123", 102)

		time.Sleep(75 * time.Millisecond)

		// Should use default upstreams
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Healthy default upstream should be active")
		err = evaluator.AcquirePermit(&logger, ups2, "method1")
		assert.NoError(t, err, "Healthy default upstream should be active")

		// Test Case 2: Default upstreams unhealthy (high error rate)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")

		mt.RecordUpstreamRequest("rpc2", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc2", "evm:123", "method1")
		mt.RecordUpstreamRequest("rpc2", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc2", "evm:123", "method1")

		time.Sleep(75 * time.Millisecond)

		// Should include fallback upstream
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Fallback should be active when defaults are unhealthy")
	})

	t.Run("DefaultPolicyDisableFallbacksWhenDefaultsHealthy", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, ups2, ups3 := createTestNetwork(t, ctx)

		// Create config with default policy
		config := &common.SelectionPolicyConfig{
			EvalInterval:     50 * time.Millisecond,
			EvalPerMethod:    false,
			ResampleExcluded: false,
		}
		config.SetDefaults() // This will set the default policy function

		mt := ntw.metricsTracker
		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Set healthy metrics for default upstreams
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 10*time.Millisecond)
		mt.SetLatestBlockNumber("rpc1", "evm:123", 100)
		mt.SetLatestBlockNumber("*", "evm:123", 105) // Network head

		mt.RecordUpstreamRequest("rpc2", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc2", "evm:123", "method1", 15*time.Millisecond)
		mt.SetLatestBlockNumber("rpc2", "evm:123", 102)

		// Set healthy metrics for fallback upstream
		mt.RecordUpstreamRequest("rpc3", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc3", "evm:123", "method1", 5*time.Millisecond)
		mt.SetLatestBlockNumber("rpc3", "evm:123", 103)

		time.Sleep(75 * time.Millisecond)

		// Verify default upstreams are active and fallback is disabled
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Healthy default upstream should be active")
		err = evaluator.AcquirePermit(&logger, ups2, "method1")
		assert.NoError(t, err, "Healthy default upstream should be active")
		err = evaluator.AcquirePermit(&logger, ups3, "method1")
		assert.Error(t, err, "Fallback should be disabled when defaults are healthy")

		// Now degrade one default upstream
		for i := 0; i < 10; i++ {
			mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
			mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
		}

		time.Sleep(75 * time.Millisecond)

		// Verify healthy default is still active
		err = evaluator.AcquirePermit(&logger, ups2, "method1")
		assert.NoError(t, err, "Healthy default should remain active")

		// Verify unhealthy default is inactive
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Unhealthy default should become inactive")

		// Verify fallback is still disabled since we have one healthy default
		err = evaluator.AcquirePermit(&logger, ups3, "method1")
		assert.Error(t, err, "Fallback should remain disabled when at least one default is healthy")
	})

	t.Run("DefaultPolicyMinHealthyThreshold", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, ups2, ups3 := createTestNetwork(t, ctx)

		// Set environment variable for minimum healthy threshold
		t.Setenv("ROUTING_POLICY_MIN_HEALTHY_THRESHOLD", "2")
		t.Setenv("ROUTING_POLICY_MAX_ERROR_RATE", "0.3")
		t.Setenv("ROUTING_POLICY_MAX_BLOCK_HEAD_LAG", "5")

		config := &common.SelectionPolicyConfig{
			EvalInterval:  50 * time.Millisecond,
			EvalPerMethod: false,
		}
		config.SetDefaults() // This will set the default policy function

		mt := ntw.metricsTracker
		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Test Case 1: Only one default healthy (should activate fallbacks)
		mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 10*time.Millisecond)
		mt.SetLatestBlockNumber("rpc1", "evm:123", 100)
		mt.SetLatestBlockNumber("*", "evm:123", 102) // Network head

		mt.RecordUpstreamRequest("rpc2", "evm:123", "method1")
		mt.RecordUpstreamFailure("rpc2", "evm:123", "method1")
		mt.SetLatestBlockNumber("rpc2", "evm:123", 95) // Lagging

		// Set good metrics for fallback
		mt.RecordUpstreamRequest("rpc3", "evm:123", "method1")
		mt.RecordUpstreamDuration("rpc3", "evm:123", "method1", 15*time.Millisecond)
		mt.SetLatestBlockNumber("rpc3", "evm:123", 101)

		time.Sleep(75 * time.Millisecond)

		// Should include fallback since we don't meet min healthy threshold
		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Healthy default should not be active")
		err = evaluator.AcquirePermit(&logger, ups2, "method1")
		assert.Error(t, err, "Unhealthy default should not be active")
		err = evaluator.AcquirePermit(&logger, ups3, "method1")
		assert.NoError(t, err, "Fallback should be active when below min healthy threshold")

		// Test Case 2: Both defaults become healthy (should disable fallbacks)
		for i := 0; i < 10; i++ {
			mt.RecordUpstreamRequest("rpc2", "evm:123", "method1")
			mt.RecordUpstreamDuration("rpc2", "evm:123", "method1", 20*time.Millisecond)
		}
		mt.SetLatestBlockNumber("rpc2", "evm:123", 101)

		time.Sleep(75 * time.Millisecond)

		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "First healthy default should be active")
		err = evaluator.AcquirePermit(&logger, ups2, "method1")
		assert.NoError(t, err, "Second healthy default should be active")
		err = evaluator.AcquirePermit(&logger, ups3, "method1")
		assert.Error(t, err, "Fallback should be inactive when enough healthy defaults")
	})

	t.Run("DefaultPolicyBlockLagThreshold", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, ups2, ups3 := createTestNetwork(t, ctx)

		t.Setenv("ROUTING_POLICY_MAX_BLOCK_HEAD_LAG", "3")
		t.Setenv("ROUTING_POLICY_MIN_HEALTHY_THRESHOLD", "1")

		config := &common.SelectionPolicyConfig{
			EvalInterval:  50 * time.Millisecond,
			EvalPerMethod: false,
		}
		config.SetDefaults()

		mt := ntw.metricsTracker
		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Test Case 1: Default upstream within lag threshold
		mt.SetLatestBlockNumber("rpc1", "evm:123", 100) // Leader
		mt.SetLatestBlockNumber("rpc2", "evm:123", 95)  // Lag of 5 blocks (exceeds threshold)
		mt.SetLatestBlockNumber("rpc3", "evm:123", 99)  // Small lag for fallback (does not exceed threshold)

		time.Sleep(75 * time.Millisecond)

		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Default ups1 with leader block should be active")
		err = evaluator.AcquirePermit(&logger, ups2, "method1")
		assert.Error(t, err, "Default ups2 with excessive lag should be inactive")
		err = evaluator.AcquirePermit(&logger, ups3, "method1")
		assert.Error(t, err, "Fallback ups3 with should be inactive because at least one default is healthy")

		// // Test Case 2: All defaults exceed lag threshold
		mt.SetLatestBlockNumber("rpc3", "evm:123", 200) // Fallback becomes leader
		mt.SetLatestBlockNumber("rpc1", "evm:123", 195) // Now lagging by 5 blocks (exceeds threshold)
		mt.SetLatestBlockNumber("rpc2", "evm:123", 190) // Now lagging by 10 blocks (exceeds threshold)

		time.Sleep(75 * time.Millisecond)

		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Lagging default ups1 should not be active")
		err = evaluator.AcquirePermit(&logger, ups2, "method1")
		assert.Error(t, err, "Lagging default ups2 should not be active")
		err = evaluator.AcquirePermit(&logger, ups3, "method1")
		assert.NoError(t, err, "Fallback ups3 should be active when all defaults are lagging")
	})

	t.Run("DefaultPolicyErrorRateThreshold", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw, ups1, ups2, ups3 := createTestNetwork(t, ctx)

		t.Setenv("ROUTING_POLICY_MAX_ERROR_RATE", "0.25")
		t.Setenv("ROUTING_POLICY_MIN_HEALTHY_THRESHOLD", "1")

		config := &common.SelectionPolicyConfig{
			EvalInterval:  50 * time.Millisecond,
			EvalPerMethod: false,
		}
		config.SetDefaults()

		mt := ntw.metricsTracker
		evaluator, err := NewPolicyEvaluator("evm:123", &logger, config, ntw.upstreamsRegistry, mt)
		require.NoError(t, err)

		err = evaluator.Start(ctx)
		require.NoError(t, err)

		// Set initial block numbers
		mt.SetLatestBlockNumber("*", "evm:123", 100)
		mt.SetLatestBlockNumber("rpc1", "evm:123", 99)
		mt.SetLatestBlockNumber("rpc2", "evm:123", 99)
		mt.SetLatestBlockNumber("rpc3", "evm:123", 99)

		// Test Case 1: Error rates around threshold
		// rpc1: 20% error rate (below threshold)
		for i := 0; i < 8; i++ {
			mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
			mt.RecordUpstreamDuration("rpc1", "evm:123", "method1", 10*time.Millisecond)
		}
		for i := 0; i < 2; i++ {
			mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
			mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
		}

		// rpc2: 30% error rate (above threshold)
		for i := 0; i < 7; i++ {
			mt.RecordUpstreamRequest("rpc2", "evm:123", "method1")
			mt.RecordUpstreamDuration("rpc2", "evm:123", "method1", 15*time.Millisecond)
		}
		for i := 0; i < 3; i++ {
			mt.RecordUpstreamRequest("rpc2", "evm:123", "method1")
			mt.RecordUpstreamFailure("rpc2", "evm:123", "method1")
		}

		time.Sleep(75 * time.Millisecond)

		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.NoError(t, err, "Default with error rate below threshold should be active")
		err = evaluator.AcquirePermit(&logger, ups2, "method1")
		assert.Error(t, err, "Default with error rate above threshold should be inactive")

		// Test Case 2: All defaults exceed error rate threshold
		for i := 0; i < 5; i++ {
			mt.RecordUpstreamRequest("rpc1", "evm:123", "method1")
			mt.RecordUpstreamFailure("rpc1", "evm:123", "method1")
		}

		// Set good metrics for fallback
		for i := 0; i < 10; i++ {
			mt.RecordUpstreamRequest("rpc3", "evm:123", "method1")
			mt.RecordUpstreamDuration("rpc3", "evm:123", "method1", 20*time.Millisecond)
		}

		time.Sleep(75 * time.Millisecond)

		err = evaluator.AcquirePermit(&logger, ups1, "method1")
		assert.Error(t, err, "Default with high error rate should not be active")
		err = evaluator.AcquirePermit(&logger, ups3, "method1")
		assert.NoError(t, err, "Fallback should be active when defaults have high error rates")
	})
}

func createTestNetwork(t *testing.T, ctx context.Context) (*Network, *upstream.Upstream, *upstream.Upstream, *upstream.Upstream) {
	clr := clients.NewClientRegistry(&log.Logger, "prjA")
	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	}, &log.Logger)
	if err != nil {
		t.Fatal(err)
	}
	vndr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(
		&log.Logger,
		vndr,
		[]*common.ProviderConfig{},
	)
	if err != nil {
		t.Fatal(err)
	}
	mt := health.NewTracker("prjA", time.Minute)

	upstreamConfigs := []*common.UpstreamConfig{
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc3",
			Group:    "fallback",
			Endpoint: "http://rpc3.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
	}

	upr := upstream.NewUpstreamsRegistry(
		ctx,
		&log.Logger,
		"prjA",
		upstreamConfigs,
		rlr,
		vndr,
		pr,
		mt,
		1*time.Second,
	)
	err = upr.Bootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
	if err != nil {
		t.Fatal(err)
	}

	var pup1, pup2, pup3 *upstream.Upstream
	if len(upstreamConfigs) > 0 {
		pup1, err = upr.NewUpstream(
			"prjA",
			upstreamConfigs[0],
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl1, err := clr.GetOrCreateClient(ctx, pup1)
		if err != nil {
			t.Fatal(err)
		}
		pup1.Client = cl1
	}

	if len(upstreamConfigs) > 1 {
		pup2, err = upr.NewUpstream(
			"prjA",
			upstreamConfigs[1],
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl2, err := clr.GetOrCreateClient(ctx, pup2)
		if err != nil {
			t.Fatal(err)
		}
		pup2.Client = cl2
	}

	if len(upstreamConfigs) > 2 {
		pup3, err = upr.NewUpstream(
			"prjA",
			upstreamConfigs[2],
			&log.Logger,
			mt,
		)
		if err != nil {
			t.Fatal(err)
		}
		cl3, err := clr.GetOrCreateClient(ctx, pup3)
		if err != nil {
			t.Fatal(err)
		}
		pup3.Client = cl3
	}
	ntw, err := NewNetwork(
		&log.Logger,
		"prjA",
		&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
			Failsafe: nil,
		},
		rlr,
		upr,
		mt,
	)
	if err != nil {
		t.Fatal(err)
	}

	return ntw, pup1, pup2, pup3
}
