package util

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/rs/zerolog"
)

type InitializationState int

const (
	StateUninitialized InitializationState = iota
	StateInitializing
	StatePartial
	StateRetrying
	StateReady
	StateFailed
)

func (s InitializationState) String() string {
	return []string{"uninitialized", "initializing", "partial", "retrying", "ready", "failed"}[s]
}

type TaskState int

const (
	TaskPending TaskState = iota
	TaskRunning
	TaskSucceeded
	TaskTimedOut
	TaskFailed
)

func (s TaskState) String() string {
	return []string{"pending", "running", "succeeded", "timedOut", "failed"}[s]
}

type BootstrapTask struct {
	Name        string
	Fn          func(ctx context.Context) error // Must respect ctx.Done()
	state       atomic.Int32                    // TaskState
	lastErr     atomic.Value                    // error
	lastAttempt atomic.Value                    // time.Time
	ctxCancel   atomic.Value                    // context.CancelFunc
	doneVal     atomic.Value                    // chan struct{}
	attempts    atomic.Int32
}

func NewBootstrapTask(name string, fn func(ctx context.Context) error) *BootstrapTask {
	t := &BootstrapTask{
		Name: name,
		Fn:   fn,
	}
	return t
}

type TaskError struct {
	TaskName  string
	Err       error
	Timestamp time.Time
	Attempt   int
}

type wrappedError struct {
	err error
}

func (t *BootstrapTask) Error() *TaskError {
	wr, _ := t.lastErr.Load().(wrappedError)
	if wr.err == nil {
		return nil
	}
	return &TaskError{
		TaskName:  t.Name,
		Err:       wr.err,
		Timestamp: t.lastAttempt.Load().(time.Time),
		Attempt:   int(t.attempts.Load()),
	}
}

// createNewDoneChannel re-creates the done channel for a fresh attempt.
// Must be called only after a successful CompareAndSwap to TaskRunning.
func (t *BootstrapTask) createNewDoneChannel() chan struct{} {
	newCh := make(chan struct{})
	t.doneVal.Store(newCh)
	return newCh
}

// Wait waits until the most recent attempt finishes (i.e., "done" is closed)
// or until the context is canceled.
// If the task has never begun (still pending), Wait will block until it eventually starts or ctx is canceled.
func (t *BootstrapTask) Wait(ctx context.Context) error {
	for {
		state := TaskState(t.state.Load())
		if state == TaskSucceeded || state == TaskFailed || state == TaskTimedOut {
			lastErr, ok := t.lastErr.Load().(wrappedError)
			if ok && lastErr.err != nil {
				return lastErr.err
			}
			return nil
		}
		ch := t.doneVal.Load()
		if ch == nil {
			// The task hasn't started an attempt yet. If the state is no longer pending, break out.
			if state != TaskPending {
				// It's either running, failed, or succeeded => loop again so we re-fetch the channel.
				continue
			}
			// We'll just do a short sleep or yield.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
				// keep looping until the task actually starts
				continue
			}
		} else {
			// We have a valid channel. Wait on it or until context is canceled.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ch.(chan struct{}):
				// The attempt ended. Check if we failed.
				if TaskState(t.state.Load()) == TaskFailed {
					wr, _ := t.lastErr.Load().(wrappedError)
					if wr.err == nil {
						t.lastErr.Store(wrappedError{err: errors.New("task failed without specific error")})
					}
					return wr.err
				}
				return nil // Succeeded or otherwise finished
			}
		}
	}
}

// attempt is called just before a new attempt to run t.Fn.
func (t *BootstrapTask) beginAttempt() {
	t.attempts.Add(1)
	t.lastAttempt.Store(time.Now())
}

type InitializerConfig struct {
	TaskTimeout   time.Duration
	AutoRetry     bool
	RetryFactor   float64
	RetryMinDelay time.Duration
	RetryMaxDelay time.Duration
}

type Initializer struct {
	appCtx   context.Context
	logger   *zerolog.Logger
	attempts atomic.Int32
	tasks    sync.Map
	tasksMu  sync.Mutex

	autoRetryActive atomic.Bool
	cancelAutoRetry atomic.Value // context.CancelFunc
	autoRetryWg     sync.WaitGroup

	conf *InitializerConfig
}

func NewInitializer(appCtx context.Context, logger *zerolog.Logger, conf *InitializerConfig) *Initializer {
	if conf == nil {
		conf = &InitializerConfig{
			TaskTimeout:   120 * time.Second,
			AutoRetry:     true,
			RetryFactor:   1.5,
			RetryMinDelay: 3 * time.Second,
			RetryMaxDelay: 130 * time.Second,
		}
	}
	return &Initializer{
		appCtx:          appCtx,
		logger:          logger,
		attempts:        atomic.Int32{},
		autoRetryActive: atomic.Bool{},
		conf:            conf,
	}
}

// Schedules tasks for execution (does not block).
// The caller is typically responsible for calling WaitForTasks after this returns.
func (i *Initializer) ExecuteTasks(ctx context.Context, tasks ...*BootstrapTask) error {
	if len(tasks) == 0 {
		return nil
	}

	i.tasksMu.Lock()
	tasksToWait := make([]*BootstrapTask, 0, len(tasks))
	for _, task := range tasks {
		actual, existed := i.tasks.LoadOrStore(task.Name, task)
		bts := actual.(*BootstrapTask)
		i.logger.Debug().Bool("existed", existed).Int32("state", bts.state.Load()).Str("task", task.Name).Msg("executing task")
		tasksToWait = append(tasksToWait, bts)
	}
	i.tasksMu.Unlock()

	i.ensureAutoRetryIfEnabled()
	i.attemptRemainingTasks()

	return i.waitForTasks(ctx, tasksToWait...)
}

func (i *Initializer) WaitForTasks(ctx context.Context) error {
	allTasks := []*BootstrapTask{}
	i.tasks.Range(func(key, value interface{}) bool {
		allTasks = append(allTasks, value.(*BootstrapTask))
		return true
	})
	return i.waitForTasks(ctx, allTasks...)
}

// Wait for a set of tasks to complete or ctx to expire.
func (i *Initializer) waitForTasks(ctx context.Context, tasks ...*BootstrapTask) error {
	var errs []error
	for _, task := range tasks {
		if err := task.Wait(ctx); err != nil {
			// If context was canceled, likely best to just return.
			return err
		}
		// If task is failed, record that error
		state := TaskState(task.state.Load())
		if state == TaskFailed && task.Error() != nil {
			errs = append(errs, task.Error().Err)
		}
	}
	if len(errs) > 0 {
		total := len(tasks)
		i.logger.Warn().Errs("tasks", errs).Msgf("initialization failed: %d/%d tasks failed", len(errs), total)
		return fmt.Errorf("initialization failed: %d/%d tasks failed: %v", len(errs), total, errs)
	}
	return nil
}

// attemptRemainingTasks tries to run any tasks in Pending, Failed or TimedOut states again.
// This function must use appContext to avoid premature cancellation of tasks when caller context is cancelled.
// The correct way to enforce timeout is to pass appropriate context to "waitForTasks()" function.
// To enforce timeout of task execution set proper TaskTimeout in InitializerConfig.
// To cancel a running task, use MarkTaskAsFailed() function instead.
func (i *Initializer) attemptRemainingTasks() {
	i.tasksMu.Lock()
	defer i.tasksMu.Unlock()

	var tasksToRun []*BootstrapTask

	wg := sync.WaitGroup{}
	i.tasks.Range(func(key, value interface{}) bool {
		wg.Add(1)
		t := value.(*BootstrapTask)
		state := TaskState(t.state.Load())
		if state == TaskPending || state == TaskFailed || state == TaskTimedOut {
			// Attempt to swap from [Pending|Failed|Timeout] -> Running
			// #nosec G115 - We know TaskState is small enough that int->int32 won't overflow
			if t.state.CompareAndSwap(int32(state), int32(TaskRunning)) {
				t.beginAttempt()
				t.lastErr.Store(wrappedError{err: nil})

				// Create a fresh done channel to signal this attempt's completion
				doneCh := t.createNewDoneChannel()
				tasksToRun = append(tasksToRun, t)

				go func(bt *BootstrapTask, doneCh chan struct{}) {
					// Close the channel when the function finishes
					// The CompareAndSwap will ensure we always and only close the channel once for each attempt
					defer close(doneCh)

					if i.appCtx.Err() != nil {
						bt.lastErr.Store(wrappedError{err: i.appCtx.Err()})
						bt.state.Store(int32(TaskFailed))
						i.logger.Warn().Str("task", bt.Name).Err(i.appCtx.Err()).Msg("initialization task context error")
						return
					}

					tctx, cancel := context.WithTimeout(i.appCtx, i.conf.TaskTimeout)
					bt.ctxCancel.Store(cancel)
					wg.Done()
					err := bt.Fn(tctx)
					if err == nil {
						// If the function returns nil but context says we're canceled, treat it as an error
						err = tctx.Err()
					}

					if err != nil {
						// If context is cancelled there will be a reason already set for it on lastErr
						if !errors.Is(err, context.Canceled) {
							if cause := context.Cause(tctx); cause != nil {
								err = cause
							}
							bt.lastErr.Store(wrappedError{err: err})
						} else {
							bt.lastErr.CompareAndSwap(nil, wrappedError{err: err})
						}
						bt.state.Store(int32(TaskFailed))
						i.logger.Warn().Str("task", bt.Name).Err(err).Msg("initialization task failed")
					} else {
						bt.lastErr.Store(wrappedError{err: nil})
						bt.state.Store(int32(TaskSucceeded))
						lastAttempt, _ := bt.lastAttempt.Load().(time.Time)
						i.logger.Info().Str("task", bt.Name).Dur("durationMs", time.Since(lastAttempt)).Msg("initialization task succeeded")
					}
				}(t, doneCh)
			} else {
				wg.Done()
			}
		} else {
			wg.Done()
		}
		return true
	})

	// Wait for tasks to "start" running. To wait for them to finish, use WaitForTasks()
	wg.Wait()
}

func (i *Initializer) State() InitializationState {
	var total, pending, running, succeeded, failed int
	i.tasks.Range(func(key, value interface{}) bool {
		t := value.(*BootstrapTask)
		state := TaskState(t.state.Load())
		switch state {
		case TaskPending:
			pending++
		case TaskRunning:
			running++
		case TaskSucceeded:
			succeeded++
		case TaskFailed:
			failed++
		}
		total++
		return true
	})
	i.logger.Trace().
		Int32("attempts", i.attempts.Load()).
		Int("total", total).
		Int("pending", pending).
		Int("running", running).
		Int("succeeded", succeeded).
		Int("failed", failed).
		Msg("calculating initialization state")

	if total == succeeded {
		return StateReady
	}
	// If all tasks are done (some are failed, none running or pending), it's a "Failed" state
	if failed > 0 && (pending+running+succeeded == 0) {
		return StateFailed
	}
	if failed > 0 && (pending+running == 0) {
		return StatePartial
	}
	// If we've tried multiple times but still have tasks not succeeded
	atp := i.attempts.Load()
	if atp > 1 && (pending > 0 || running > 0) {
		return StateRetrying
	}
	return StateInitializing
}

func (i *Initializer) Status() *InitializerStatus {
	state := i.State()
	return &InitializerStatus{
		State: state,
		Tasks: i.tasksStatus(),
	}
}

func (i *Initializer) Errors() error {
	var errs []error
	i.tasks.Range(func(key, value interface{}) bool {
		t := value.(*BootstrapTask)
		if t.Error() != nil {
			errs = append(errs, t.Error().Err)
		}
		return true
	})
	return errors.Join(errs...)
}

func (i *Initializer) MarkTaskAsFailed(name string, err error) {
	i.logger.Error().Str("task", name).Err(err).Msg("marking task as failed")
	i.tasks.Range(func(key, value interface{}) bool {
		t := value.(*BootstrapTask)
		if t.Name == name {
			previousState := TaskState(t.state.Swap(int32(TaskFailed)))
			if previousState == TaskRunning {
				if ctxCancel, ok := t.ctxCancel.Load().(context.CancelFunc); ok && ctxCancel != nil {
					ctxCancel()
				}
			}
			t.lastErr.Store(wrappedError{err: err})
			return false
		}
		return true
	})

	i.ensureAutoRetryIfEnabled()
}

func (i *Initializer) Stop(destroyFn func() error) error {
	i.logger.Debug().Msg("stopping initializer")

	i.tasksMu.Lock()
	defer i.tasksMu.Unlock()

	if cancel := i.cancelAutoRetry.Load(); cancel != nil {
		cancel.(context.CancelFunc)()
	}

	// Wait for auto-retry goroutine to finish
	i.autoRetryWg.Wait()

	// Now, wait for any tasks that might still be running to finish or fail.
	waitCtx, waitCancel := context.WithTimeout(i.appCtx, i.conf.TaskTimeout+100*time.Millisecond)
	defer waitCancel()

	// WaitForTasks will block until all tasks have ended (either succeeded or failed).
	if err := i.WaitForTasks(waitCtx); err != nil {
		i.logger.Warn().Err(err).Msg("failed waiting for tasks to finish within the stop sequence")
	}

	var err error
	if destroyFn != nil {
		err = destroyFn()
	}
	return err
}

type TaskStatus struct {
	Name        string
	State       TaskState
	Err         error
	LastAttempt time.Time
	Attempts    int
}

func (s *TaskStatus) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]interface{}{
		"name":        s.Name,
		"state":       s.State.String(),
		"err":         s.Err,
		"lastAttempt": s.LastAttempt,
		"attempts":    s.Attempts,
	})
}
func (i *Initializer) tasksStatus() []TaskStatus {
	var statuses []TaskStatus
	i.tasks.Range(func(key, value interface{}) bool {
		t := value.(*BootstrapTask)
		lastAttempt, _ := t.lastAttempt.Load().(time.Time)
		var errVal error
		if ev := t.lastErr.Load(); ev != nil {
			wr, _ := ev.(wrappedError)
			errVal = wr.err
		}
		statuses = append(statuses, TaskStatus{
			Name:        t.Name,
			State:       TaskState(t.state.Load()),
			Err:         errVal,
			LastAttempt: lastAttempt,
			Attempts:    int(t.attempts.Load()),
		})
		return true
	})
	return statuses
}

type InitializerStatus struct {
	State     InitializationState
	LastError error
	Tasks     []TaskStatus
}

func (s *InitializerStatus) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]interface{}{
		"state":     s.State.String(),
		"lastError": s.LastError,
		"tasks":     s.Tasks,
	})
}

// Start background auto-retry, if configured
func (i *Initializer) ensureAutoRetryIfEnabled() {
	if !i.conf.AutoRetry {
		return
	}
	if i.autoRetryActive.Load() {
		return
	}
	i.autoRetryActive.Store(true)

	rctx, cancel := context.WithCancel(i.appCtx)
	i.cancelAutoRetry.Store(cancel)

	// Add to wait group
	i.autoRetryWg.Add(1)
	go func() {
		defer i.autoRetryWg.Done()
		i.logger.Debug().Msg("initializer auto-retry loop started")
		i.autoRetryLoop(rctx)
		i.logger.Debug().Msg("initializer auto-retry loop finished")
	}()
}

// Continually attempt tasks until all succeed or context is canceled
func (i *Initializer) autoRetryLoop(ctx context.Context) {
	if cancel := i.cancelAutoRetry.Load(); cancel != nil {
		defer cancel.(context.CancelFunc)()
	}
	if i.State() == StateReady {
		i.autoRetryActive.Store(false)
		return
	}

	delay := i.conf.RetryMinDelay
	// Wait for the first delay before doing the first retry
	<-time.After(delay)
	for {
		if ctx.Err() != nil {
			i.logger.Debug().Err(ctx.Err()).Msg("initialization auto-retry interrupted")
			i.autoRetryActive.Store(false)
			return
		}
		i.attempts.Add(1)
		i.attemptRemainingTasks()
		err := i.WaitForTasks(ctx)
		state := i.State()
		if err == nil && state == StateReady {
			i.autoRetryActive.Store(false)
			return
		}
		if err != nil {
			i.logger.Warn().Err(err).Str("state", state.String()).Msgf("initialization auto-retry failed, will retry in %v", delay)
		}

		select {
		case <-ctx.Done():
			i.logger.Debug().Err(ctx.Err()).Msg("initialization auto-retry cancelled")
			i.autoRetryActive.Store(false)
			return
		case <-time.After(delay):
		}

		delay = time.Duration(float64(delay) * i.conf.RetryFactor)
		if delay > i.conf.RetryMaxDelay {
			delay = i.conf.RetryMaxDelay
		} else if delay < i.conf.RetryMinDelay {
			delay = i.conf.RetryMinDelay
		}
	}
}
