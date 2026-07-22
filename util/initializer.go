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
	StateFatal
)

func (s InitializationState) String() string {
	return []string{"uninitialized", "initializing", "partial", "retrying", "ready", "failed", "fatal"}[s]
}

type TaskState int

const (
	TaskPending TaskState = iota
	TaskRunning
	TaskSucceeded
	TaskTimedOut
	TaskFailed
	TaskFatal
)

func (s TaskState) String() string {
	return []string{"pending", "running", "succeeded", "timedOut", "failed", "fatal"}[s]
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
				// Attempt ended — loop back to the terminal-state check so
				// TimedOut/Fatal/Failed all surface lastErr consistently
				// (previously only TaskFailed was handled here, so a deadline
				// TimedOut incorrectly returned nil).
				continue
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

	StateUpdates chan InitializationState
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
//
// Waits run in parallel so one slow/hung task cannot serialize the wait
// budget across siblings (previously a single hung task at the front of
// sync.Map iteration burned the whole timeout before others were observed).
func (i *Initializer) waitForTasks(ctx context.Context, tasks ...*BootstrapTask) error {
	if len(tasks) == 0 {
		return nil
	}

	type waitResult struct {
		task *BootstrapTask
		err  error
	}
	errCh := make(chan waitResult, len(tasks))
	for _, task := range tasks {
		go func(task *BootstrapTask) {
			errCh <- waitResult{task: task, err: task.Wait(ctx)}
		}(task)
	}

	var errs []error
	var ctxErr error
	for range tasks {
		res := <-errCh
		if res.err == nil {
			continue
		}
		st := TaskState(res.task.state.Load())
		// Wait-context abort: task still in-flight when Wait returned a
		// context error. A task that already finished as TimedOut also
		// surfaces DeadlineExceeded via lastErr — that is a task failure.
		if (errors.Is(res.err, context.Canceled) || errors.Is(res.err, context.DeadlineExceeded)) &&
			(st == TaskPending || st == TaskRunning) {
			if ctxErr == nil {
				ctxErr = res.err
			}
			continue
		}
		errs = append(errs, res.err)
	}
	if ctxErr != nil {
		return ctxErr
	}
	if len(errs) > 0 {
		total := len(tasks)
		i.logger.Warn().Errs("tasks", errs).Msgf("initialization failed: %d/%d tasks failed", len(errs), total)
		return fmt.Errorf("initialization failed: %d/%d tasks failed: %w", len(errs), total, errors.Join(errs...))
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
				attemptID := t.attempts.Load()
				t.lastErr.Store(wrappedError{err: nil})

				// Create a fresh done channel to signal this attempt's completion
				doneCh := t.createNewDoneChannel()
				tasksToRun = append(tasksToRun, t)

				go func(bt *BootstrapTask, doneCh chan struct{}, attemptID int32) {
					// Close the channel when the function finishes.
					defer close(doneCh)

					// finishAttempt applies terminalState only if this goroutine
					// still owns the attempt (same attemptID and still Running).
					// Prevents a reaped/superseded hung Fn from clobbering a
					// later retry's state.
					finishAttempt := func(terminal TaskState, err error) bool {
						if bt.attempts.Load() != attemptID {
							return false
						}
						if !bt.state.CompareAndSwap(int32(TaskRunning), int32(terminal)) {
							return false
						}
						if err != nil {
							bt.lastErr.Store(wrappedError{err: err})
						} else {
							bt.lastErr.Store(wrappedError{err: nil})
						}
						return true
					}

					if i.appCtx.Err() != nil {
						if finishAttempt(TaskFailed, i.appCtx.Err()) {
							i.logger.Warn().Str("task", bt.Name).Err(i.appCtx.Err()).Msg("initialization task context error")
						}
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
						// Detect fatal control errors without importing the common package to avoid cycles
						var fatal interface{ IsTaskFatal() bool }
						if errors.As(err, &fatal) {
							underlying := err
							if uw, ok := err.(interface{ Unwrap() error }); ok && uw.Unwrap() != nil {
								underlying = uw.Unwrap()
							}
							if finishAttempt(TaskFatal, underlying) {
								i.logger.Error().Str("task", bt.Name).Err(underlying).Msg("initialization task fatal error")
							}
							return
						}
						if !errors.Is(err, context.Canceled) {
							if cause := context.Cause(tctx); cause != nil {
								err = cause
							}
						} else {
							// Preserve a reason already set (e.g. by reap) when canceled.
							if wr, ok := bt.lastErr.Load().(wrappedError); ok && wr.err != nil {
								err = wr.err
							}
						}
						terminal := TaskFailed
						if errors.Is(err, context.DeadlineExceeded) {
							terminal = TaskTimedOut
						}
						if finishAttempt(terminal, err) {
							i.logger.Warn().Str("task", bt.Name).Err(err).Str("state", terminal.String()).Msg("initialization task failed")
						}
					} else {
						if finishAttempt(TaskSucceeded, nil) {
							lastAttempt, _ := bt.lastAttempt.Load().(time.Time)
							i.logger.Info().Str("task", bt.Name).Dur("durationMs", time.Since(lastAttempt)).Msg("initialization task succeeded")
						}
					}
				}(t, doneCh, attemptID)
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
	var total, pending, running, succeeded, failed, timedOut, fatal int
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
		case TaskTimedOut:
			timedOut++
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
		Int("timedOut", timedOut).
		Int("fatal", fatal).
		Msg("calculating initialization state")

	if total == 0 {
		return StateUninitialized
	}
	if total == succeeded {
		return StateReady
	}

	// failed + timedOut are retryable; pending/running are in-flight.
	// Do NOT map "any fatal" → StateFatal while siblings can still recover —
	// one permanently-misconfigured upstream must not mark a shared
	// Initializer (dozens of networks) as wholly fatal.
	retryable := failed + timedOut
	inFlight := pending + running
	nonTerminal := inFlight + retryable

	if nonTerminal > 0 {
		atp := i.attempts.Load()
		if atp > 1 {
			return StateRetrying
		}
		if inFlight > 0 {
			return StateInitializing
		}
		// Only retryable tasks left (awaiting auto-retry), nothing in-flight.
		if succeeded > 0 || fatal > 0 {
			return StatePartial
		}
		return StateFailed
	}

	// All terminal: succeeded and/or fatal only.
	if fatal == total {
		return StateFatal
	}
	if fatal > 0 {
		return StatePartial
	}
	return StateInitializing
}

// reapOverdueRunningTasks force-transitions Running tasks whose attempt has
// exceeded TaskTimeout to TaskTimedOut. Used after a bounded WaitForTasks so a
// Fn that ignores ctx cannot keep the auto-retry loop's hasPendingWork true
// forever as TaskRunning (and cannot block Stop on that goroutine forever —
// Stop still may time out waiting for the leaked Fn, but the task is
// retryable again).
func (i *Initializer) reapOverdueRunningTasks() {
	now := time.Now()
	i.tasks.Range(func(_, value interface{}) bool {
		t := value.(*BootstrapTask)
		if TaskState(t.state.Load()) != TaskRunning {
			return true
		}
		lastAttempt, _ := t.lastAttempt.Load().(time.Time)
		if lastAttempt.IsZero() || now.Sub(lastAttempt) < i.conf.TaskTimeout {
			return true
		}
		if cancel, ok := t.ctxCancel.Load().(context.CancelFunc); ok && cancel != nil {
			cancel()
		}
		if t.state.CompareAndSwap(int32(TaskRunning), int32(TaskTimedOut)) {
			t.lastErr.Store(wrappedError{err: context.DeadlineExceeded})
			i.logger.Warn().
				Str("task", t.Name).
				Dur("runningFor", now.Sub(lastAttempt)).
				Msg("initialization task timed out while still running; reaped for retry")
		}
		return true
	})
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

	// Cancel the auto-retry loop and wait for it to exit BEFORE taking
	// tasksMu: the loop acquires tasksMu inside attemptRemainingTasks, so
	// holding the mutex while waiting for the goroutine can deadlock if the
	// loop is blocked on the mutex when the cancel lands.
	if cancel := i.cancelAutoRetry.Load(); cancel != nil {
		cancel.(context.CancelFunc)()
	}
	i.autoRetryWg.Wait()

	i.tasksMu.Lock()
	defer i.tasksMu.Unlock()

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

// RangeTaskStates calls fn(name, state) for each registered task. Return
// false from fn to stop iteration early.
//
// Allocation-free streaming alternative to `Status().Tasks` for callers
// that only need (name, state) and don't want to materialize the full
// `[]TaskStatus`. Pprof on prod showed `tasksStatus`'s growslice +
// per-task TaskStatus allocs at ~10% CPU during the bootstrap-wait
// window, where `summarizeNetworkTasks` was calling Status() every
// 200ms and immediately throwing away the Err / LastAttempt / Attempts
// fields it didn't need.
func (i *Initializer) RangeTaskStates(fn func(name string, state TaskState) bool) {
	i.tasks.Range(func(_, value any) bool {
		t := value.(*BootstrapTask)
		return fn(t.Name, TaskState(t.state.Load()))
	})
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

// hasPendingWork reports whether any registered task is still in a non-terminal
// state (pending, running, failed, or timed-out) and could therefore benefit
// from another attempt. Only succeeded and fatal tasks are terminal.
//
// This is the auto-retry loop's stop condition. It deliberately does NOT key off
// State() alone: a single fatal sibling must not end retries for recoverable
// tasks in the same shared Initializer.
func (i *Initializer) hasPendingWork() bool {
	pending := false
	i.tasks.Range(func(_, value interface{}) bool {
		switch TaskState(value.(*BootstrapTask).state.Load()) {
		case TaskPending, TaskRunning, TaskFailed, TaskTimedOut:
			pending = true
			return false // found one; stop iterating
		}
		return true
	})
	return pending
}

// Continually attempt tasks until every task is terminal (succeeded or fatal)
// or the context is canceled.
func (i *Initializer) autoRetryLoop(ctx context.Context) {
	if cancel := i.cancelAutoRetry.Load(); cancel != nil {
		defer cancel.(context.CancelFunc)()
	}
	// Nothing to retry once every task is terminal. A fatal task must not end
	// the loop on its own — recoverable siblings must keep retrying.
	if !i.hasPendingWork() {
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
		// Bounded wait: a task hung inside its Fn (e.g. a client dial that
		// ignores ctx and never returns) stays Running forever; an unbounded
		// WaitForTasks would then block this loop and stop retries of every
		// other task.
		waitCtx, waitCancel := context.WithTimeout(ctx, i.conf.TaskTimeout)
		err := i.WaitForTasks(waitCtx)
		waitCancel()
		// Reap Fns that ignored their deadline so they become TaskTimedOut
		// (retryable) instead of wedging hasPendingWork as TaskRunning forever.
		i.reapOverdueRunningTasks()
		state := i.State()
		// Stop only once no task can benefit from another attempt (every task
		// succeeded or is fatal). Fatal tasks are skipped by
		// attemptRemainingTasks, so a permanently-failing task cannot wedge the
		// retries of its still-recoverable siblings.
		if !i.hasPendingWork() {
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
