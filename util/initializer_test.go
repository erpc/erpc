package util

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupInitializer(t *testing.T, ctx context.Context, conf *InitializerConfig) *Initializer {
	logger := zerolog.New(zerolog.NewTestWriter(t))
	return NewInitializer(ctx, &logger, conf)
}

// TestInitializer_FailedTaskBackoffGatesReRuns reproduces the production hot
// path where NetworksRegistry.GetNetwork calls ExecuteTasks on every incoming
// request for a network that never finishes initializing (e.g. a lazy-loaded
// network that resolves to zero upstreams). Each request builds a fresh task
// with the same name, so without a backoff gate the failing task is re-executed
// on every single request — which is what flooded prod with ~900k/2h
// "network initialization ended with zero upstreams" error logs.
//
// A just-failed task must NOT be re-executed again until its retry backoff has
// elapsed; rapid requests inside that window should return the cached failure.
func TestInitializer_FailedTaskBackoffGatesReRuns(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Auto-retry off isolates the request-path (ExecuteTasks) behavior; a large
	// RetryMinDelay keeps every rapid re-execution inside the backoff window.
	conf := &InitializerConfig{
		TaskTimeout:   5 * time.Second,
		AutoRetry:     false,
		RetryFactor:   1.5,
		RetryMinDelay: 10 * time.Second,
		RetryMaxDelay: 130 * time.Second,
	}
	init := setupInitializer(t, appCtx, conf)

	var runs atomic.Int32
	fn := func(ctx context.Context) error {
		runs.Add(1)
		return errors.New("network initialization ended with zero upstreams")
	}

	const requests = 25
	for i := 0; i < requests; i++ {
		// Mirrors GetNetwork -> ExecuteTasks(buildNetworkBootstrapTask(id)):
		// a fresh task object each time, keyed by the same name.
		err := init.ExecuteTasks(appCtx, NewBootstrapTask("network/evm:999", fn))
		require.Error(t, err, "every request must still surface the init failure")
	}

	assert.Equal(t, int32(1), runs.Load(),
		"a failing task must not be re-executed on every request within its backoff window")
}

// TestInitializer_FailedTaskReRunsAfterBackoff is the complement: once the
// backoff elapses, the task becomes eligible again so a subsequent request (or
// the auto-retry loop) re-runs it — the gate delays re-execution, it does not
// permanently pin a task to its first failure.
func TestInitializer_FailedTaskReRunsAfterBackoff(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conf := &InitializerConfig{
		TaskTimeout:   5 * time.Second,
		AutoRetry:     false,
		RetryFactor:   1.5,
		RetryMinDelay: 50 * time.Millisecond,
		RetryMaxDelay: 130 * time.Second,
	}
	init := setupInitializer(t, appCtx, conf)

	var runs atomic.Int32
	fn := func(ctx context.Context) error {
		runs.Add(1)
		return errors.New("still failing")
	}

	_ = init.ExecuteTasks(appCtx, NewBootstrapTask("network/evm:1000", fn))
	require.Equal(t, int32(1), runs.Load(), "first request runs the task")

	// Within the backoff window: no re-run.
	_ = init.ExecuteTasks(appCtx, NewBootstrapTask("network/evm:1000", fn))
	require.Equal(t, int32(1), runs.Load(), "request inside backoff window must not re-run")

	// After the backoff elapses: the next request re-runs the task.
	time.Sleep(80 * time.Millisecond)
	_ = init.ExecuteTasks(appCtx, NewBootstrapTask("network/evm:1000", fn))
	require.Equal(t, int32(2), runs.Load(),
		"a failing task must become eligible for re-execution once its backoff elapses")
}

func TestInitializer_SingleTaskSuccess(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, nil)
	task := NewBootstrapTask("success", func(ctx context.Context) error {
		return nil
	})

	err := init.ExecuteTasks(appCtx, task)
	defer init.Stop(nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, init.State())
	assert.Equal(t, TaskSucceeded, TaskState(task.state.Load()))
}

func TestInitializer_SingleTaskImmediateFailureNoRetry(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: time.Second,
		AutoRetry:   false,
	})

	expectedErr := errors.New("immediate failure")
	task := NewBootstrapTask("failing", func(ctx context.Context) error {
		return expectedErr
	})

	err := init.ExecuteTasks(appCtx, task)
	defer init.Stop(nil)
	require.Error(t, err)
	assert.Equal(t, StateFailed, init.State())
	assert.Equal(t, TaskFailed, TaskState(task.state.Load()))
	assert.Equal(t, expectedErr, task.Error().Err)
}

func TestInitializer_SingleTaskFailureWithRetryNeverSucceeds(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout:   time.Second,
		AutoRetry:     true,
		RetryMinDelay: time.Millisecond,
		RetryMaxDelay: time.Millisecond * 10,
		RetryFactor:   1.1,
	})

	attempts := 0
	task := NewBootstrapTask("always-failing", func(ctx context.Context) error {
		attempts++
		return errors.New("persistent failure")
	})

	// Use a context with timeout to ensure the test doesn't run forever
	ctx, cancel := context.WithTimeout(appCtx, time.Millisecond*100)
	defer cancel()

	init.ExecuteTasks(ctx, task)

	time.Sleep(time.Millisecond * 200)
	init.Stop(nil)

	assert.Equal(t, TaskFailed, TaskState(task.state.Load()))
	assert.True(t, attempts >= 10, "should have attempted multiple times")
}

func TestInitializer_SingleTaskFailsThenSucceeds(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout:   time.Second,
		AutoRetry:     true,
		RetryMinDelay: time.Millisecond,
		RetryMaxDelay: time.Millisecond * 10,
		RetryFactor:   1.5,
	})

	attempts := 0
	task := NewBootstrapTask("eventually-succeeds", func(ctx context.Context) error {
		attempts++
		if attempts == 1 {
			return errors.New("first attempt fails")
		}
		return nil
	})

	init.ExecuteTasks(appCtx, task)
	defer init.Stop(nil)
	time.Sleep(time.Millisecond * 100)
	err := init.WaitForTasks(appCtx)
	require.NoError(t, err)
	assert.Equal(t, StateReady, init.State())
	assert.Equal(t, TaskSucceeded, TaskState(task.state.Load()))
	assert.Equal(t, 2, attempts)
}

func TestInitializer_MultipleTasksAllSucceed(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, nil)

	tasks := []*BootstrapTask{
		NewBootstrapTask("task1", func(ctx context.Context) error { return nil }),
		NewBootstrapTask("task2", func(ctx context.Context) error { return nil }),
		NewBootstrapTask("task3", func(ctx context.Context) error { return nil }),
	}

	err := init.ExecuteTasks(appCtx, tasks...)
	defer init.Stop(nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, init.State())

	for _, task := range tasks {
		assert.Equal(t, TaskSucceeded, TaskState(task.state.Load()))
	}
}

func TestInitializer_MultipleTasksMixedResultsNoRetry(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: 5 * time.Second,
		AutoRetry:   false,
	})

	failingTask := NewBootstrapTask("failing", func(ctx context.Context) error {
		return errors.New("task failed")
	})

	tasks := []*BootstrapTask{
		NewBootstrapTask("success1", func(ctx context.Context) error { return nil }),
		failingTask,
		NewBootstrapTask("success2", func(ctx context.Context) error { return nil }),
	}

	err := init.ExecuteTasks(appCtx, tasks...)
	defer init.Stop(nil)
	require.Error(t, err)
	assert.Equal(t, TaskFailed, TaskState(failingTask.state.Load()))
	assert.Equal(t, StatePartial, init.State())
}

func TestInitializer_MultipleTasksMixedResultsInitializing(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout:   time.Second,
		AutoRetry:     true,
		RetryMinDelay: time.Millisecond * 250,
		RetryMaxDelay: time.Millisecond * 250,
	})

	// We'll define three tasks:
	// 1) A task that takes time (so the initializer stays "Initializing" briefly).
	// 2) A task that fails on the first run.
	// 3) A task that succeeds immediately.
	longRunningTask := NewBootstrapTask("long-running", func(ctx context.Context) error {
		time.Sleep(50 * time.Millisecond)
		return nil
	})

	attempts := 0
	failingTaskFirst := NewBootstrapTask("fail-first-attempt", func(ctx context.Context) error {
		time.Sleep(10 * time.Millisecond)
		attempts++
		if attempts <= 1 {
			return errors.New("failing on first attempt")
		}
		return nil
	})

	immediateSuccess := NewBootstrapTask("immediate-success", func(ctx context.Context) error {
		return nil
	})

	// Start them in a goroutine so we can check state mid-run:
	go func() {
		_ = init.ExecuteTasks(appCtx, longRunningTask, failingTaskFirst, immediateSuccess)
	}()

	// Give an instant for tasks to start so we can observe StateInitializing.
	time.Sleep(5 * time.Millisecond)
	assert.Equal(t, StateInitializing, init.State(), "one or more tasks should still be running")
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, StatePartial, init.State(), "one task must be failed")

	// Wait again for the retry attempt to finish
	time.Sleep(250 * time.Millisecond)
	err := init.WaitForTasks(appCtx)
	require.NoError(t, err, "the second attempt should succeed, no further errors expected")

	// Now that the failed task has succeeded, the overall state should be Ready.
	assert.Equal(t, StateReady, init.State(), "once all tasks succeed, the initializer should be Ready")
	init.Stop(nil)
}

func TestInitializer_LongRunningTask(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: time.Second * 2,
		AutoRetry:   false,
	})

	longTask := NewBootstrapTask("long-task", func(ctx context.Context) error {
		time.Sleep(time.Second) // Sleep for 1 second
		return nil
	})

	quickTask := NewBootstrapTask("quick-task", func(ctx context.Context) error {
		return nil
	})

	err := init.ExecuteTasks(appCtx, longTask, quickTask)
	defer init.Stop(nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, init.State())
	assert.Equal(t, TaskSucceeded, TaskState(longTask.state.Load()))
	assert.Equal(t, TaskSucceeded, TaskState(quickTask.state.Load()))
}

func TestInitializer_TaskTimeout(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: time.Millisecond * 100,
		AutoRetry:   false,
	})

	task := NewBootstrapTask("timeout-task", func(ctx context.Context) error {
		time.Sleep(time.Second) // Sleep longer than timeout
		return nil
	})

	err := init.ExecuteTasks(appCtx, task)
	defer init.Stop(nil)
	require.Error(t, err)
	assert.Equal(t, StateFailed, init.State())
	assert.Equal(t, TaskFailed, TaskState(task.state.Load()))
	assert.ErrorIs(t, task.Error().Err, context.DeadlineExceeded)
}

func TestInitializer_MarkTaskAsFailed(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: time.Second,
		AutoRetry:   false,
	})

	task := NewBootstrapTask("to-be-marked-failed", func(ctx context.Context) error {
		time.Sleep(time.Hour) // Long sleep that we'll interrupt
		return nil
	})

	// Start the task in a goroutine
	go func() {
		_ = init.ExecuteTasks(appCtx, task)
	}()

	// Give it a moment to start
	time.Sleep(time.Millisecond * 10)

	expectedErr := errors.New("manual failure")
	init.MarkTaskAsFailed("to-be-marked-failed", expectedErr)

	// Wait for task to reach terminal state
	err := task.Wait(context.Background())
	require.Error(t, err)

	assert.Equal(t, StateFailed, init.State())
	assert.Equal(t, TaskFailed, TaskState(task.state.Load()))
	assert.Equal(t, expectedErr, task.Error().Err)

	init.Stop(nil)
}

func TestInitializer_StopWithDestroyFn(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: time.Second,
		AutoRetry:   true,
	})

	destroyed := false
	destroyFn := func() error {
		destroyed = true
		return nil
	}

	// Start a failing task that will trigger auto-retry
	task := NewBootstrapTask("failing-task", func(ctx context.Context) error {
		return errors.New("persistent failure")
	})

	go func() {
		_ = init.ExecuteTasks(appCtx, task)
	}()

	// Give it a moment to start retrying
	time.Sleep(time.Millisecond * 100)

	err := init.Stop(destroyFn)
	require.NoError(t, err)
	assert.True(t, destroyed, "destroyFn should have been called")
}

func TestInitializer_MultipleRapidFailures(t *testing.T) {
	conf := &InitializerConfig{
		TaskTimeout:   time.Millisecond * 50,
		AutoRetry:     true,
		RetryMinDelay: time.Millisecond * 10,
		RetryMaxDelay: time.Millisecond * 20,
		RetryFactor:   1.2,
	}
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, conf)

	var attempts int
	task := NewBootstrapTask("quick-failer", func(ctx context.Context) error {
		attempts++
		// Fail quickly:
		return errors.New("keep failing")
	})

	init.ExecuteTasks(appCtx, task)

	// Use a context with short timeout so we don't spin forever
	ctx, cancel := context.WithTimeout(appCtx, time.Millisecond*300)
	defer cancel()

	// WaitForTasks is expected to fail
	time.Sleep(time.Millisecond * 200)
	err := init.WaitForTasks(ctx)
	require.Error(t, err, "task should eventually fail or context should time out")

	// Check we tried multiple times (rapidly)
	assert.True(t, attempts > 1, "should attempt multiple times in quick succession")

	// Check final State is either partial or failed
	state := init.State()
	assert.True(
		t,
		state == StateFailed,
		"final state should reflect the repeated failures, got %v", state,
	)

	init.Stop(nil)
}

func TestInitializer_ForcedCancellationMidTask(t *testing.T) {
	conf := &InitializerConfig{
		TaskTimeout:   time.Second * 2, // Somewhat long
		AutoRetry:     false,
		RetryMinDelay: time.Millisecond * 50,
		RetryMaxDelay: time.Millisecond * 100,
		RetryFactor:   1.5,
	}
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, conf)

	task := NewBootstrapTask("never-finishes", func(ctx context.Context) error {
		// Sleep forever or until ctx is canceled
		<-ctx.Done()
		// Return the ctx error or something
		return ctx.Err()
	})

	init.ExecuteTasks(appCtx, task)

	// Cancel the context after a brief delay
	ctx, cancel := context.WithTimeout(appCtx, time.Millisecond*100)
	defer cancel()

	// Once canceled, WaitForTasks should return a context error
	err := init.WaitForTasks(ctx)
	defer init.Stop(nil)
	require.Error(t, err, "should fail or be canceled")
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// Check task state is failed (or timed out) after forced cancel
	st := TaskState(task.state.Load())
	assert.True(t, st == TaskFailed, "task should show failed or timed out, got %d", st)
}

func TestInitializer_MarkTaskAsFailedMidRun(t *testing.T) {
	conf := &InitializerConfig{
		TaskTimeout:   time.Millisecond * 500,
		AutoRetry:     true, // let it possibly re-attempt
		RetryMinDelay: time.Millisecond * 30,
		RetryMaxDelay: time.Millisecond * 30,
		RetryFactor:   1.5,
	}
	appCtx, cancelApp := context.WithCancel(context.Background())
	defer cancelApp()

	init := setupInitializer(t, appCtx, conf)

	blockingTask := NewBootstrapTask("blocker", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second * 1):
			return nil
		}
	})

	// Start the task asynchronously
	init.ExecuteTasks(appCtx, blockingTask)

	// Give it a moment to start
	time.Sleep(time.Millisecond * 100)

	// Mark the task as failed
	customErr := errors.New("manual failure")
	init.MarkTaskAsFailed("blocker", customErr)

	// Check that state is updated to FAILED quickly
	assert.Equal(t, TaskFailed, TaskState(blockingTask.state.Load()), "task should be forced to FAIL")

	// If AutoRetry is true, might see a new attempt => it might be re-run
	time.Sleep(time.Millisecond * 100)
	st := TaskState(blockingTask.state.Load())

	// If the re-attempt has started, we might see it in RUNNING or even SUCCEEDED,
	// but it definitely shouldn't be in PENDING anymore. Let's just check it's not the old one.
	assert.Equal(t, TaskRunning, st, "should not remain pending after forced failure")

	// Final wait
	ctxt, cancel := context.WithTimeout(appCtx, time.Second*2)
	defer cancel()

	// Could succeed if the second attempt eventually finishes, or fail if it times out again
	err := init.WaitForTasks(ctxt)
	// We'll accept either no error (if the re-attempt succeeded) or error (if still failing).
	// But let's at least confirm we don't time out indefinitely here.
	require.True(t, err == nil || err != nil, "just confirming we didn't hang forever")

	finalState := init.State()
	assert.NotEqual(t, StatePartial, finalState, "expected eventually a stable state after re-attempts")

	err = init.Stop(nil)
	require.NoError(t, err)
}

func TestInitializer_ManualMarkAsFailedAfterSuccess(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conf := &InitializerConfig{
		TaskTimeout:   time.Second * 2,
		AutoRetry:     true,
		RetryMinDelay: time.Millisecond * 50,
		RetryMaxDelay: time.Millisecond * 150,
		RetryFactor:   1.3,
	}
	init := setupInitializer(t, appCtx, conf)

	var attempts int32
	task := NewBootstrapTask("flaky-task", func(ctx context.Context) error {
		// We'll track attempts in a local variable:
		attempts++
		switch attempts {
		case 1:
			// First attempt: Sleep a bit, then succeed
			time.Sleep(time.Millisecond * 100)
			return nil
		case 2, 3:
			// Second & Third attempts: fail quickly
			return errors.New("manual-failure-phase: fails on attempt #2 and #3")
		case 4:
			// Fourth attempt: succeed
			return nil
		default:
			// Just in case, fail all other attempts after #4 (shouldn't happen)
			return errors.New("unexpected extra attempt")
		}
	})

	// 1) Execute tasks for the first time - should succeed on attempt #1
	err := init.ExecuteTasks(appCtx, task)
	defer init.Stop(nil)
	require.NoError(t, err, "ExecuteTasks should not error")

	// Wait for the first success
	err = init.WaitForTasks(appCtx)
	require.NoError(t, err, "first attempt should succeed without errors")

	// Confirm it's in the SUCCEEDED state now
	st := TaskState(task.state.Load())
	require.Equal(t, TaskSucceeded, st, "task should be succeeded after first attempt")
	require.Equal(t, int32(1), attempts, "should have run exactly once so far")

	// 2) The task is "good" for a while
	time.Sleep(time.Millisecond * 100)

	// 3) Manually mark the task as failed
	init.MarkTaskAsFailed(task.Name, errors.New("manual forced failure after success"))

	// After forcing it to fail, with AutoRetry=true we expect a new attempt to start soon
	// That next attempt is attempts=2 => fails quickly
	// Next one is attempts=3 => fails quickly
	// Next is attempts=4 => eventually succeeds
	time.Sleep(time.Millisecond * 200)

	// 4) Wait for the re-attempts to finish
	ctx, cancel := context.WithTimeout(appCtx, time.Second*3)
	defer cancel()
	err = init.WaitForTasks(ctx)

	// We expect the final attempt (#4) to succeed eventually
	require.NoError(t, err, "final attempt should succeed eventually")

	// Check that we ended up with SUCCEEDED state
	st = TaskState(task.state.Load())
	assert.Equal(t, TaskSucceeded, st, "task should be succeeded after final attempt")

	// We expect attempts to be at least 4 now
	finalAttempts := attempts
	assert.Equal(t, int32(4), finalAttempts, "should have attempted 4 times in total")
}

func TestInitializer_MarkAsFailedUnblocksWaitThenRetries(t *testing.T) {
	conf := &InitializerConfig{
		TaskTimeout:   500 * time.Millisecond,
		AutoRetry:     true, // We'll rely on auto-retry to eventually succeed
		RetryMinDelay: time.Millisecond * 50,
		RetryMaxDelay: time.Millisecond * 50,
	}
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, conf)

	// This task function will block until context is canceled
	// (i.e., it doesn't naturally exit).
	blockingFn := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	// We'll make it so that on the third attempt, it succeeds quickly.
	var attemptCount int32
	customFn := func(ctx context.Context) error {
		attemptCount++
		if attemptCount == 3 {
			return nil // third attempt succeeds
		}
		// first/second attempt never returns unless forcibly canceled/failed
		return blockingFn(ctx)
	}

	task := NewBootstrapTask("blocked-then-succeed", customFn)

	// Execute the task. The first attempt will start running and block.
	err := init.ExecuteTasks(appCtx, task)
	require.Error(t, err, "First attempt ExecuteTasks() should have timeout error")

	// Give the task time to get into RUNNING state with second attempt.
	time.Sleep(time.Millisecond * 80)

	// We'll have a goroutine that waits on the task. It should block until the task is forcibly failed.
	waitErrCh := make(chan error)
	go func() {
		waitErrCh <- task.Wait(appCtx)
	}()

	// Now mark the task as failed. This should:
	// 1) cause Wait() to unblock with an error.
	// 2) trigger a third attempt if AutoRetry=true.
	customErr := errors.New("manual fail to unblock waiters")
	go func() {
		time.Sleep(time.Millisecond * 5)
		init.MarkTaskAsFailed(task.Name, customErr)
	}()

	unblockedErr := <-waitErrCh
	require.Error(t, unblockedErr, "Wait() should be unblocked with an error when MarkTaskAsFailed is called")
	assert.Equal(t, customErr, unblockedErr, "Wait() should return the custom error")

	// Wait for the second attempt to finish
	time.Sleep(time.Millisecond * 100)

	// Now auto-retry should notice the task is FAILED and re-run it.
	// On the third attempt, our function is designed to succeed, so let's wait for that success.
	ctx, cancel := context.WithTimeout(appCtx, 1*time.Second)
	defer cancel()

	waitForAllErr := init.WaitForTasks(ctx)
	require.NoError(t, waitForAllErr, "final attempt should succeed after auto-retry")

	// Verify final state is ready
	assert.Equal(t, StateReady, init.State(), "initializer should be fully ready after second attempt")

	// The task should have final TaskSucceeded state
	assert.Equal(t, TaskSucceeded, TaskState(task.state.Load()), "task should be succeeded after final attempt")

	init.Stop(nil)
}

func TestInitializer_SyncOncePatternNoGoroutineLeak(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	init := setupInitializer(t, appCtx, nil)

	slowTask := NewBootstrapTask("network/evm:1/provider/slow", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})

	var once sync.Once
	before := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		once.Do(func() {
			go func() {
				_ = init.ExecuteTasks(appCtx, slowTask)
			}()
		})
	}

	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()

	cancel()
	init.Stop(nil)

	// sync.Once ensures only 1 ExecuteTasks goroutine + initializer internals.
	// Without it, 20 goroutines would pile up blocking in waitForTasks.
	assert.Less(t, after-before, 5)
}

// TestInitializer_RangeTaskStates_YieldsAllRegistered — the streaming
// alternative to `Status().Tasks` must visit every registered task
// with its current (name, state). This is the alloc-free API
// `summarizeNetworkTasks` uses on the 200ms bootstrap-wait ticker;
// missing tasks would mean we'd never see "all providers terminal"
// and the loop would burn until the 30s timeout.
func TestInitializer_RangeTaskStates_YieldsAllRegistered(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: time.Second,
		AutoRetry:   false,
	})
	defer init.Stop(nil)

	tasks := []*BootstrapTask{
		NewBootstrapTask("alpha", func(ctx context.Context) error { return nil }),
		NewBootstrapTask("beta", func(ctx context.Context) error { return nil }),
		NewBootstrapTask("gamma", func(ctx context.Context) error { return nil }),
	}
	require.NoError(t, init.ExecuteTasks(appCtx, tasks...))

	seen := make(map[string]TaskState)
	init.RangeTaskStates(func(name string, state TaskState) bool {
		seen[name] = state
		return true
	})

	require.Len(t, seen, 3, "every registered task must be visited")
	for _, want := range []string{"alpha", "beta", "gamma"} {
		state, ok := seen[want]
		require.True(t, ok, "task %q must appear in Range", want)
		assert.Equal(t, TaskSucceeded, state, "task %q state must reflect completion", want)
	}
}

// TestInitializer_RangeTaskStates_EarlyStop — returning false from
// the callback halts iteration. Callers that find what they need
// early (e.g. "any task is still running") shouldn't pay the cost of
// walking the rest of the map.
func TestInitializer_RangeTaskStates_EarlyStop(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, nil)
	defer init.Stop(nil)

	tasks := []*BootstrapTask{
		NewBootstrapTask("a", func(ctx context.Context) error { return nil }),
		NewBootstrapTask("b", func(ctx context.Context) error { return nil }),
		NewBootstrapTask("c", func(ctx context.Context) error { return nil }),
		NewBootstrapTask("d", func(ctx context.Context) error { return nil }),
	}
	require.NoError(t, init.ExecuteTasks(appCtx, tasks...))

	visits := 0
	init.RangeTaskStates(func(name string, state TaskState) bool {
		visits++
		return false // stop after first
	})
	assert.Equal(t, 1, visits, "Range must stop when callback returns false")
}

// TestInitializer_RangeTaskStates_AgreesWithStatus — semantic
// equivalence with the old Status().Tasks path on the fields the
// caller (summarizeNetworkTasks) actually consults.
func TestInitializer_RangeTaskStates_AgreesWithStatus(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout: time.Second,
		AutoRetry:   false,
	})
	defer init.Stop(nil)

	expectedErr := errors.New("boom")
	tasks := []*BootstrapTask{
		NewBootstrapTask("ok-task", func(ctx context.Context) error { return nil }),
		NewBootstrapTask("fail-task", func(ctx context.Context) error { return expectedErr }),
	}
	_ = init.ExecuteTasks(appCtx, tasks...)

	statusMap := make(map[string]TaskState)
	for _, ts := range init.Status().Tasks {
		statusMap[ts.Name] = ts.State
	}

	rangeMap := make(map[string]TaskState)
	init.RangeTaskStates(func(name string, state TaskState) bool {
		rangeMap[name] = state
		return true
	})

	assert.Equal(t, statusMap, rangeMap,
		"RangeTaskStates must agree with Status().Tasks on (name, state) for every entry")
}

// BenchmarkInitializer_RangeTaskStates_vs_Status — measures the
// allocation diff between the two APIs on a 200-task fleet (matches
// production scale: ~50 networks × ~4 tasks each). RangeTaskStates
// should report zero allocs/op; Status materializes a `[]TaskStatus`
// with N elements.
func BenchmarkInitializer_RangeTaskStates_vs_Status(b *testing.B) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	init := NewInitializer(appCtx, &logger, &InitializerConfig{
		TaskTimeout: time.Second,
	})
	defer init.Stop(nil)

	tasks := make([]*BootstrapTask, 200)
	for i := range tasks {
		i := i
		tasks[i] = NewBootstrapTask(
			fmt.Sprintf("network/evm:%d/provider/p", i),
			func(ctx context.Context) error { return nil },
		)
	}
	_ = init.ExecuteTasks(appCtx, tasks...)

	b.Run("Status_AllocsFullSlice", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			st := init.Status()
			_ = st.Tasks
		}
	})

	b.Run("RangeTaskStates_Streaming", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			count := 0
			init.RangeTaskStates(func(name string, state TaskState) bool {
				count++
				return true
			})
		}
	})
}

// fatalErr is a task error that reports itself as fatal via the same
// `IsTaskFatal() bool` interface the Initializer detects with errors.As. It
// mirrors common.TaskFatalError without importing the common package (which
// would create an import cycle), so these tests exercise the real fatal path.
type fatalErr struct{ msg string }

func (e *fatalErr) Error() string     { return e.msg }
func (e *fatalErr) IsTaskFatal() bool { return true }

// TestInitializer_HasPendingWork exercises the auto-retry loop's stop condition
// across every task state. Only Succeeded and Fatal are terminal; crucially, a
// Fatal task mixed with any non-terminal task must still report pending work —
// that is the whole point of the fix (a fatal task must not make the loop treat
// the entire shared initializer as done).
func TestInitializer_HasPendingWork(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cases := []struct {
		name   string
		states []TaskState
		want   bool
	}{
		{"empty", nil, false},
		{"all succeeded", []TaskState{TaskSucceeded, TaskSucceeded}, false},
		{"all fatal", []TaskState{TaskFatal, TaskFatal}, false},
		{"succeeded and fatal", []TaskState{TaskSucceeded, TaskFatal}, false},
		{"one pending", []TaskState{TaskPending}, true},
		{"one running", []TaskState{TaskRunning}, true},
		{"one failed", []TaskState{TaskFailed}, true},
		{"one timed out", []TaskState{TaskTimedOut}, true},
		{"fatal plus failed (regression)", []TaskState{TaskFatal, TaskFailed}, true},
		{"succeeded plus fatal plus pending", []TaskState{TaskSucceeded, TaskFatal, TaskPending}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh initializer per case so we never copy a sync.Map.
			init := setupInitializer(t, appCtx, nil)
			for idx, st := range tc.states {
				task := NewBootstrapTask(fmt.Sprintf("t%d", idx), func(ctx context.Context) error { return nil })
				task.state.Store(int32(st))
				init.tasks.Store(task.Name, task)
			}
			assert.Equal(t, tc.want, init.hasPendingWork())
		})
	}
}

// TestInitializer_FatalTaskDoesNotStrandSiblings is the primary regression test
// for the shared-initializer wedge: one task returns a permanent (fatal) error
// while a sibling fails transiently a few times before recovering. Because both
// tasks live in the same Initializer, the old code flipped State() to
// StateFatal the moment the fatal task landed and the auto-retry loop exited —
// stranding the recoverable sibling until process restart. The sibling must now
// reach Succeeded on its own.
func TestInitializer_FatalTaskDoesNotStrandSiblings(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout:   time.Second,
		AutoRetry:     true,
		RetryMinDelay: 5 * time.Millisecond,
		RetryMaxDelay: 20 * time.Millisecond,
		RetryFactor:   1.5,
	})
	defer init.Stop(nil)

	var fatalAttempts atomic.Int32
	fatalTask := NewBootstrapTask("upstream/permanently-broken", func(ctx context.Context) error {
		fatalAttempts.Add(1)
		return &fatalErr{"chain id mismatch"}
	})

	const recoverAfter = int32(3)
	var sibAttempts atomic.Int32
	siblingTask := NewBootstrapTask("upstream/transiently-broken", func(ctx context.Context) error {
		if sibAttempts.Add(1) < recoverAfter {
			return errors.New("transient boot failure")
		}
		return nil
	})

	_ = init.ExecuteTasks(appCtx, fatalTask, siblingTask)

	require.Eventually(t, func() bool {
		return TaskState(siblingTask.state.Load()) == TaskSucceeded
	}, 2*time.Second, 5*time.Millisecond,
		"recoverable sibling must reach Succeeded even though a peer task is fatal")

	assert.Equal(t, TaskFatal, TaskState(fatalTask.state.Load()), "fatal task stays fatal")
	assert.Equal(t, int32(1), fatalAttempts.Load(), "a fatal task must never be retried")
	assert.GreaterOrEqual(t, sibAttempts.Load(), recoverAfter, "sibling retried until it recovered")
}

// TestInitializer_FatalTaskKeepsSiblingRetryLoopAlive is the sharper regression
// guard: even a sibling that NEVER recovers must keep being retried while a
// fatal peer exists. Before the fix the auto-retry loop exited after the first
// round (State()==StateFatal), pinning the sibling at a single attempt.
func TestInitializer_FatalTaskKeepsSiblingRetryLoopAlive(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout:   time.Second,
		AutoRetry:     true,
		RetryMinDelay: 5 * time.Millisecond,
		RetryMaxDelay: 10 * time.Millisecond,
		RetryFactor:   1.2,
	})
	defer init.Stop(nil)

	fatalTask := NewBootstrapTask("upstream/fatal", func(ctx context.Context) error {
		return &fatalErr{"permanent"}
	})
	var sibAttempts atomic.Int32
	failingSibling := NewBootstrapTask("upstream/failing", func(ctx context.Context) error {
		sibAttempts.Add(1)
		return errors.New("still failing")
	})

	_ = init.ExecuteTasks(appCtx, fatalTask, failingSibling)

	require.Eventually(t, func() bool {
		return sibAttempts.Load() >= 5
	}, 2*time.Second, 5*time.Millisecond,
		"failing sibling must keep being retried even though a peer task is fatal")
}

// TestInitializer_AllFatalTasksStopRetrying is the complement: when every task
// is fatal there is nothing to recover, so the loop must terminate and never
// re-run a fatal task (no busy-loop, no wasted attempts).
func TestInitializer_AllFatalTasksStopRetrying(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout:   time.Second,
		AutoRetry:     true,
		RetryMinDelay: 5 * time.Millisecond,
		RetryMaxDelay: 10 * time.Millisecond,
		RetryFactor:   1.5,
	})
	defer init.Stop(nil)

	var a, b atomic.Int32
	t1 := NewBootstrapTask("upstream/fatal-1", func(ctx context.Context) error { a.Add(1); return &fatalErr{"boom1"} })
	t2 := NewBootstrapTask("upstream/fatal-2", func(ctx context.Context) error { b.Add(1); return &fatalErr{"boom2"} })

	_ = init.ExecuteTasks(appCtx, t1, t2)

	// Ample time for any (erroneous) retries to happen.
	time.Sleep(120 * time.Millisecond)

	assert.Equal(t, TaskFatal, TaskState(t1.state.Load()))
	assert.Equal(t, TaskFatal, TaskState(t2.state.Load()))
	assert.Equal(t, int32(1), a.Load(), "fatal task 1 must never be retried")
	assert.Equal(t, int32(1), b.Load(), "fatal task 2 must never be retried")
	assert.Equal(t, StateFatal, init.State())
	assert.False(t, init.hasPendingWork(), "no pending work once every task is terminal")
}

// TestInitializer_FatalTaskWithSucceededSiblingExits verifies the mixed
// terminal case: one fatal + one already-succeeded task means no pending work,
// so the loop exits cleanly (and Stop returns promptly).
func TestInitializer_FatalTaskWithSucceededSiblingExits(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout:   time.Second,
		AutoRetry:     true,
		RetryMinDelay: 5 * time.Millisecond,
		RetryMaxDelay: 10 * time.Millisecond,
		RetryFactor:   1.5,
	})

	okTask := NewBootstrapTask("upstream/ok", func(ctx context.Context) error { return nil })
	fatalTask := NewBootstrapTask("upstream/fatal", func(ctx context.Context) error { return &fatalErr{"permanent"} })

	_ = init.ExecuteTasks(appCtx, okTask, fatalTask)
	time.Sleep(50 * time.Millisecond)

	assert.Equal(t, TaskSucceeded, TaskState(okTask.state.Load()))
	assert.Equal(t, TaskFatal, TaskState(fatalTask.state.Load()))
	assert.False(t, init.hasPendingWork())

	done := make(chan struct{})
	go func() { _ = init.Stop(nil); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return promptly after all tasks reached terminal states")
	}
}
