package util

import (
	"context"
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
)

type TaskState int

const (
	TaskPending TaskState = iota
	TaskRunning
	TaskSucceeded
	TaskTimedOut
	TaskFailed
)

type BootstrapTask struct {
	Name        string
	Fn          func(ctx context.Context) error
	state       atomic.Int32 // TaskState
	lastErr     atomic.Value // error
	lastAttempt atomic.Value // time.Time
	attempts    atomic.Int32
	done        chan struct{} // Signals when task reaches terminal state
}

func NewBootstrapTask(name string, fn func(ctx context.Context) error) *BootstrapTask {
	return &BootstrapTask{
		Name: name,
		Fn:   fn,
		done: make(chan struct{}),
	}
}

type TaskError struct {
	TaskName  string
	Err       error
	Timestamp time.Time
	Attempt   int
}

func (t *BootstrapTask) Error() *TaskError {
	if t.lastErr.Load() == nil {
		return nil
	}
	return &TaskError{
		TaskName:  t.Name,
		Err:       t.lastErr.Load().(error),
		Timestamp: t.lastAttempt.Load().(time.Time),
		Attempt:   int(t.attempts.Load()),
	}
}

type InitializerConfig struct {
	// TaskTimeout is the maximum time to wait for a task to complete before
	// marking it as failed.
	TaskTimeout time.Duration

	// AutoRetry is whether to retry failed tasks automatically.
	// If true, it will retry failed tasks in a loop with exponential backoff
	// indefinitely in the background.
	AutoRetry bool
	// RetryFactor is the factor to multiply the delay by after each retry.
	RetryFactor float64
	// RetryMinDelay is the minimum delay between retries.
	RetryMinDelay time.Duration
	// RetryMaxDelay is the maximum delay between retries.
	RetryMaxDelay time.Duration
}

// Initializer manages lifecycle of initialization for various components.
// It handles parallel subtasks, retrying failures, and tracking overall state.
type Initializer struct {
	logger   *zerolog.Logger
	attempts atomic.Int32
	tasks    sync.Map
	tasksMu  sync.Mutex

	autoRetryActive atomic.Bool
	cancelAutoRetry atomic.Value // context.CancelFunc

	conf *InitializerConfig
}

func NewInitializer(logger *zerolog.Logger, conf *InitializerConfig) *Initializer {
	if conf == nil {
		conf = &InitializerConfig{
			TaskTimeout:   60 * time.Second,
			AutoRetry:     true,
			RetryMinDelay: 3 * time.Second,
			RetryFactor:   1.5,
			RetryMaxDelay: 30 * time.Second,
		}
	}
	return &Initializer{
		logger:          logger,
		attempts:        atomic.Int32{},
		autoRetryActive: atomic.Bool{},
		conf:            conf,
	}
}

func (i *Initializer) ExecuteTasks(ctx context.Context, tasks ...*BootstrapTask) error {
	var tasksToWait []*BootstrapTask

	i.tasksMu.Lock()
	for _, task := range tasks {
		// Load existing task or store new one
		actual, existed := i.tasks.LoadOrStore(task.Name, task)
		bts := actual.(*BootstrapTask)
		i.logger.Debug().Bool("existed", existed).Int32("state", bts.state.Load()).Str("task", task.Name).Msg("executing task")
		tasksToWait = append(tasksToWait, bts)
	}
	i.tasksMu.Unlock()

	i.ensureAutoRetryIfEnabled(ctx)
	i.attemptRemainingTasks(ctx)

	return i.waitForTasks(ctx, tasksToWait...)
}

func (i *Initializer) WaitForTasks(ctx context.Context) error {
	allTasks := []*BootstrapTask{}
	i.tasks.Range(func(key, value interface{}) bool {
		allTasks = append(allTasks, value.(*BootstrapTask))
		return true
	})
	return i.waitForTasks(ctx)
}

func (i *Initializer) waitForTasks(ctx context.Context, tasks ...*BootstrapTask) error {
	if len(tasks) == 0 {
		tasks = []*BootstrapTask{}
		i.tasks.Range(func(key, value interface{}) bool {
			tasks = append(tasks, value.(*BootstrapTask))
			return true
		})
	}

	errs := []error{}
	for _, task := range tasks {
		select {
		case <-task.done:
			if TaskState(task.state.Load()) == TaskFailed {
				errs = append(errs, task.lastErr.Load().(error))
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if len(errs) > 0 {
		totalTasks := len(tasks)
		totalErrors := len(errs)
		i.logger.Warn().Errs("tasks", errs).Msgf("initialization failed: %d/%d tasks failed", totalErrors, totalTasks)
		return fmt.Errorf("initialization failed: %d/%d tasks failed", totalErrors, totalTasks)
	}

	return nil
}

func (i *Initializer) attemptRemainingTasks(ctx context.Context) {
	i.tasksMu.Lock()

	var tasksToRun []*BootstrapTask
	var total int

	i.tasks.Range(func(key, value interface{}) bool {
		total++
		t := value.(*BootstrapTask)
		state := TaskState(t.state.Load())
		if state == TaskPending || state == TaskFailed || state == TaskTimedOut {
			t.state.Store(int32(TaskRunning))
			t.done = make(chan struct{})
			t.attempts.Add(1)
			t.lastAttempt.Store(time.Now())
			tasksToRun = append(tasksToRun, t)
		}
		return true
	})

	if len(tasksToRun) == 0 {
		i.tasksMu.Unlock()
		return
	}

	// Run tasks in parallel
	for _, task := range tasksToRun {
		go func(t *BootstrapTask) {
			i.logger.Debug().Str("timeout", i.conf.TaskTimeout.String()).Str("task", t.Name).Msg("running task")

			tctx, cancel := context.WithTimeout(ctx, i.conf.TaskTimeout)
			defer cancel()

			err := t.Fn(tctx)
			if err == nil && tctx.Err() != nil {
				err = tctx.Err()
			}

			if err != nil {
				t.lastErr.Store(err)
				t.state.Store(int32(TaskFailed))
				i.logger.Warn().Str("task", t.Name).Err(err).Msg("initialization task failed")
			} else {
				t.state.Store(int32(TaskSucceeded))
				i.logger.Info().Str("task", t.Name).Msg("initialization task succeeded")
			}
			close(t.done)
		}(task)
	}

	i.tasksMu.Unlock()
}

// ensureAutoRetryIfEnabled spins up a goroutine for auto-retry if not already active.
func (i *Initializer) ensureAutoRetryIfEnabled(ctx context.Context) {
	if !i.conf.AutoRetry {
		return
	}

	if i.autoRetryActive.Load() {
		return
	}

	i.autoRetryActive.Store(true)
	rctx, cancel := context.WithCancel(ctx)
	i.cancelAutoRetry.Store(cancel)

	go i.autoRetryLoop(rctx)
}

// autoRetryLoop continuously attempts tasks, with exponential backoff, until they're all successful
// or the initializer is destroyed.
func (i *Initializer) autoRetryLoop(ctx context.Context) {
	if cancel := i.cancelAutoRetry.Load(); cancel != nil {
		defer cancel.(context.CancelFunc)()
	}

	if i.State() == StateReady {
		i.autoRetryActive.Store(false)
		return
	}

	delay := i.conf.RetryMinDelay
	for {
		i.attempts.Add(1)
		i.attemptRemainingTasks(ctx)
		err := i.waitForTasks(ctx)
		if err == nil {
			// If there's no error, it means all tasks have succeeded => state=StateReady
			if i.State() == StateReady {
				i.autoRetryActive.Store(false)
				return
			}
		}

		i.logger.Warn().Err(err).Msgf("initialization auto-retry failed, will retry in %v", delay)
		select {
		case <-ctx.Done():
			i.logger.Debug().Err(ctx.Err()).Msg("initialization auto-retry cancelled")
			i.autoRetryActive.Store(false)
			return
		case <-time.After(delay):
		}

		// Increase the delay up to the max.
		delay = time.Duration(float64(delay) * i.conf.RetryFactor)
		if delay > i.conf.RetryMaxDelay {
			delay = i.conf.RetryMaxDelay
		}
	}
}

func (i *Initializer) State() InitializationState {
	var total, pending, running, succeeded, failed int
	i.tasks.Range(func(key, value interface{}) bool {
		total++
		t := value.(*BootstrapTask)
		state := TaskState(t.state.Load())
		if state == TaskPending {
			pending++
		} else if state == TaskRunning {
			running++
		} else if state == TaskSucceeded {
			succeeded++
		} else if state == TaskFailed {
			failed++
		}
		return true
	})
	if total == succeeded {
		return StateReady
	}
	if failed > 0 && pending == 0 && running == 0 && succeeded == 0 {
		return StateFailed
	}
	if i.attempts.Load() > 1 && (pending > 0 || running > 0) {
		return StateRetrying
	}
	return StatePartial
}

// Status returns the overall state and scenario of the initializer and its tasks.
func (i *Initializer) Status() *InitializerStatus {
	state := i.State()
	return &InitializerStatus{
		State: state,
		Tasks: i.tasksStatus(),
	}
}

// Stop halts any background retries, marks the initializer destroyed, and executes destroyFn if provided.
func (i *Initializer) Stop(destroyFn func() error) error {
	i.tasksMu.Lock()
	defer i.tasksMu.Unlock()

	var err error
	if cancel := i.cancelAutoRetry.Load(); cancel != nil {
		cancel.(context.CancelFunc)()
	}
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

// tasksStatus generates a slice of TaskStatus for all known tasks.
func (i *Initializer) tasksStatus() []TaskStatus {
	var statuses []TaskStatus
	i.tasks.Range(func(key, value interface{}) bool {
		t := value.(*BootstrapTask)
		statuses = append(statuses, TaskStatus{
			Name:        t.Name,
			State:       TaskState(t.state.Load()),
			Err:         t.lastErr.Load().(error),
			LastAttempt: t.lastAttempt.Load().(time.Time),
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
