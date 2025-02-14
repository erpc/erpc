package common

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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
	StateFatal
)

type TaskState int

const (
	TaskPending TaskState = iota
	TaskRunning
	TaskSucceeded
	TaskTimedOut
	TaskFailed
	TaskFatal
)

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
		if state == TaskSucceeded || state == TaskFailed || state == TaskTimedOut || state == TaskFatal {
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
			// If it's still Pending, just wait a bit, check for cancellation.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
				// keep looping until the task actually starts
				continue
			}
		} else {
			// We have a valid channel => wait on it or until context is canceled.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ch.(chan struct{}):
				// The attempt ended => see if it's failed or fatal. If so, return the stored error.
				currState := TaskState(t.state.Load())
				if currState == TaskFailed || currState == TaskFatal {
					wr, _ := t.lastErr.Load().(wrappedError)
					if wr.err == nil {
						t.lastErr.Store(wrappedError{err: errors.New("task ended with failure/fatal but no specific error")})
					}
					return wr.err
				}
				// Otherwise, it must be succeeded or timed out but subsequently retried => no error.
				return nil
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
			RetryMaxDelay: 30 * time.Second,
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
		i.logger.Debug().
			Bool("existed", existed).
			Int32("state", bts.state.Load()).
			Str("task", task.Name).
			Msg("executing task")
		tasksToWait = append(tasksToWait, bts)
	}
	i.tasksMu.Unlock()

	// Kick off auto-retry if needed
	i.ensureAutoRetryIfEnabled()
	i.attemptRemainingTasks(ctx)

	// Wait for these newly scheduled tasks to complete or fail
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

// waitForTasks blocks until the given tasks all reach a terminal state
// or until the context expires/cancels. Returns an error if any tasks are
// in TaskFailed or TaskFatal at the end.
func (i *Initializer) waitForTasks(ctx context.Context, tasks ...*BootstrapTask) error {
	var errs []error
	for _, task := range tasks {
		if err := task.Wait(ctx); err != nil {
			// If context is canceled, bubble that up right away
			return err
		}
		state := TaskState(task.state.Load())
		if (state == TaskFailed || state == TaskFatal) && task.Error() != nil {
			errs = append(errs, task.Error().Err)
		}
	}
	if len(errs) > 0 {
		total := len(tasks)
		i.logger.Warn().
			Errs("tasks", errs).
			Msgf("initialization failed: %d/%d tasks ended in failed or fatal state", len(errs), total)
		return fmt.Errorf("initialization failed: %d/%d tasks failed", len(errs), total)
	}
	return nil
}

// attemptRemainingTasks tries to run any tasks in Pending, Failed or TimedOut states again.
func (i *Initializer) attemptRemainingTasks(ctx context.Context) {
	i.tasksMu.Lock()
	defer i.tasksMu.Unlock()

	wg := sync.WaitGroup{}

	i.tasks.Range(func(key, value interface{}) bool {
		t := value.(*BootstrapTask)
		state := TaskState(t.state.Load())
		if state == TaskPending || state == TaskFailed || state == TaskTimedOut {
			// Attempt CAS => Running
			if t.state.CompareAndSwap(int32(state), int32(TaskRunning)) {
				t.beginAttempt()
				t.lastErr.Store(wrappedError{err: nil}) // reset error

				doneCh := t.createNewDoneChannel()
				wg.Add(1)
				go func(bt *BootstrapTask, done chan struct{}) {
					defer close(done)
					defer wg.Done()

					if ctx.Err() != nil {
						// The global context is canceled => mark as failed
						bt.lastErr.Store(wrappedError{err: ctx.Err()})
						bt.state.Store(int32(TaskFailed))
						i.logger.Warn().Str("task", bt.Name).Err(ctx.Err()).
							Msg("initialization task context error")
						return
					}

					tctx, cancel := context.WithTimeout(ctx, i.conf.TaskTimeout)
					defer cancel()

					bt.ctxCancel.Store(cancel)
					err := bt.Fn(tctx)
					if err == nil {
						// If the function returned nil but the context is canceled, treat as error
						err = tctx.Err()
					}
					if err != nil {
						// record the error
						if !errors.Is(err, context.Canceled) {
							// If the context has a cause, store that
							if cause := context.Cause(tctx); cause != nil {
								err = cause
							}
							bt.lastErr.Store(wrappedError{err: err})
						} else {
							bt.lastErr.CompareAndSwap(nil, wrappedError{err: err})
						}
						// Check if it's fatal or normal
						if HasErrorCode(err, ErrCodeTaskFatal) {
							bt.state.Store(int32(TaskFatal))
							i.logger.Error().
								Str("task", bt.Name).
								Err(err).
								Msg("initialization task encountered fatal error, no more retries")
						} else {
							bt.state.Store(int32(TaskFailed))
							i.logger.Warn().
								Str("task", bt.Name).
								Err(err).
								Msg("initialization task failed")
						}
					} else {
						// success => store no error
						bt.lastErr.Store(wrappedError{err: nil})
						bt.state.Store(int32(TaskSucceeded))
						lastAttempt, _ := bt.lastAttempt.Load().(time.Time)
						i.logger.Info().
							Str("task", bt.Name).
							Dur("durationMs", time.Since(lastAttempt)).
							Msg("initialization task succeeded")
					}
				}(t, doneCh)
			}
		}
		return true
	})

	// Wait for tasks to at least *start* running. The final results come from waitForTasks or task.Wait.
	wg.Wait()
}

// State calculates the overall state of the initializer based on all tasks.
func (i *Initializer) State() InitializationState {
	var total, pending, running, succeeded, failed, fatal int
	i.tasks.Range(func(key, value interface{}) bool {
		t := value.(*BootstrapTask)
		switch TaskState(t.state.Load()) {
		case TaskPending:
			pending++
		case TaskRunning:
			running++
		case TaskSucceeded:
			succeeded++
		case TaskFailed:
			failed++
		case TaskFatal:
			fatal++
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
		Int("fatal", fatal).
		Msg("calculating initialization state")

	// If *any* tasks are fatal => entire initializer is fatal
	if fatal > 0 {
		return StateFatal
	}
	// If all tasks ended in success => ready
	if total == succeeded {
		return StateReady
	}

	// If no tasks are pending/running, but at least one is failed => overall state is failed
	if failed > 0 && (pending+running+succeeded == 0) {
		return StateFailed
	}

	// If we have made multiple attempts but still have tasks not succeeded => StateRetrying
	atp := i.attempts.Load()
	if atp > 1 && (pending > 0 || running > 0) {
		return StateRetrying
	}
	// If there's at least one fail but some tasks are still going => partial
	if failed > 0 {
		return StatePartial
	}
	// Otherwise, tasks still in progress => StateInitializing
	return StateInitializing
}

func (i *Initializer) Status() *InitializerStatus {
	return &InitializerStatus{
		State: i.State(),
		Tasks: i.tasksStatus(),
	}
}

func (i *Initializer) MarkTaskAsFailed(name string, err error) {
	i.logger.Warn().Str("task", name).Err(err).Msg("marking task as failed")
	i.tasks.Range(func(key, value interface{}) bool {
		t := value.(*BootstrapTask)
		if t.Name == name {
			currentState := TaskState(t.state.Load())
			if currentState == TaskRunning {
				// Cancel the task's context if we can
				if ctxCancel, ok := t.ctxCancel.Load().(context.CancelFunc); ok && ctxCancel != nil {
					ctxCancel()
				}
			}
			t.lastErr.Store(wrappedError{err: err})
			t.state.Store(int32(TaskFailed))
			return false
		}
		return true
	})

	i.ensureAutoRetryIfEnabled()
}

func (i *Initializer) Stop(destroyFn func() error) error {
	i.logger.Debug().Msg("stopping initializer")

	// Cancel auto-retry loop
	i.tasksMu.Lock()
	if cancel := i.cancelAutoRetry.Load(); cancel != nil {
		cancel.(context.CancelFunc)()
	}
	i.tasksMu.Unlock()

	// Wait for auto-retry goroutine to finish
	i.autoRetryWg.Wait()

	// Now wait for tasks
	i.tasksMu.Lock()
	defer i.tasksMu.Unlock()

	waitCtx, waitCancel := context.WithTimeout(i.appCtx, i.conf.TaskTimeout+100*time.Millisecond)
	defer waitCancel()

	if err := i.WaitForTasks(waitCtx); err != nil {
		i.logger.Warn().Err(err).Msg("failed waiting for tasks to finish within the stop sequence")
	}

	var finalErr error
	if destroyFn != nil {
		finalErr = destroyFn()
	}
	return finalErr
}

type TaskStatus struct {
	Name        string
	State       TaskState
	Err         error
	LastAttempt time.Time
	Attempts    int
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

// InitializerStatus is a snapshot of the overall state and tasks
type InitializerStatus struct {
	State     InitializationState
	LastError error
	Tasks     []TaskStatus
}

// ensureAutoRetryIfEnabled checks whether autoRetry is configured and starts
// the auto-retry goroutine if not already active.
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

	i.autoRetryWg.Add(1)
	go func() {
		defer i.autoRetryWg.Done()
		i.logger.Debug().Msg("initializer auto-retry loop started")
		i.autoRetryLoop(rctx)
		i.logger.Debug().Msg("initializer auto-retry loop finished")
	}()
}

// autoRetryLoop repeatedly attempts tasks until success, fatal, or cancellation.
func (i *Initializer) autoRetryLoop(ctx context.Context) {
	defer func() {
		if c := i.cancelAutoRetry.Load(); c != nil {
			c.(context.CancelFunc)()
		}
	}()

	// If already fatal or ready, no reason to continue
	state := i.State()
	if state == StateReady || state == StateFatal {
		i.logger.Debug().Msgf("initial state is %v => no auto-retry needed", state)
		i.autoRetryActive.Store(false)
		return
	}

	delay := i.conf.RetryMinDelay
	<-time.After(delay) // initial wait before first attempt
	for {
		if ctx.Err() != nil {
			i.logger.Debug().Err(ctx.Err()).Msg("auto-retry interrupted by context")
			i.autoRetryActive.Store(false)
			return
		}

		i.attempts.Add(1)
		i.attemptRemainingTasks(ctx)

		if err := i.WaitForTasks(ctx); err != nil {
			i.logger.Warn().Err(err).Msgf("auto-retry iteration ended with error")
		}
		state := i.State()
		if state == StateReady || state == StateFatal {
			i.autoRetryActive.Store(false)
			return
		}

		select {
		case <-ctx.Done():
			i.logger.Debug().Err(ctx.Err()).Msg("auto-retry cancelled")
			i.autoRetryActive.Store(false)
			return
		case <-time.After(delay):
		}

		// Exponential-ish backoff
		delay = time.Duration(float64(delay) * i.conf.RetryFactor)
		if delay > i.conf.RetryMaxDelay {
			delay = i.conf.RetryMaxDelay
		} else if delay < i.conf.RetryMinDelay {
			delay = i.conf.RetryMinDelay
		}
	}
}
