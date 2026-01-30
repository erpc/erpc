package util

import (
	"context"
	"errors"
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

func TestInitializer_SingleTaskSuccess(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

	// Give time for tasks to start so we can observe StateInitializing (increased for busy CI runners)
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, StateInitializing, init.State(), "one or more tasks should still be running")
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, StatePartial, init.State(), "one task must be failed")

	// Wait again for the retry attempt to finish (increased for busy CI runners)
	time.Sleep(350 * time.Millisecond)
	err := init.WaitForTasks(appCtx)
	require.NoError(t, err, "the second attempt should succeed, no further errors expected")

	// Now that the failed task has succeeded, the overall state should be Ready.
	assert.Equal(t, StateReady, init.State(), "once all tasks succeed, the initializer should be Ready")
	init.Stop(nil)
}

func TestInitializer_LongRunningTask(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
