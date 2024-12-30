package util

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type InitializationState int

const (
	StateUninitialized InitializationState = iota
	StateRegistered
	StateInitializing
	StatePartial
	StateReady
	StateFailed
	StateDestroyed
)

type TaskState int

const (
	TaskPending TaskState = iota
	TaskRunning
	TaskSucceeded
	TaskFailed
)

type BootstrapTask struct {
	Name        string
	Fn          func(ctx context.Context) error
	state       TaskState
	lastErr     error
	lastAttempt time.Time
	attempts    int
}

type TaskError struct {
	TaskName  string
	Err       error
	Timestamp time.Time
	Attempt   int
}

func (t *BootstrapTask) Error() *TaskError {
	if t.lastErr == nil {
		return nil
	}
	return &TaskError{
		TaskName:  t.Name,
		Err:       t.lastErr,
		Timestamp: t.lastAttempt,
		Attempt:   t.attempts,
	}
}

type InitializerConfig struct {
	RetryFactor      float64
	RetryMinDelay    time.Duration
	RetryMaxDelay    time.Duration
	BootstrapTimeout time.Duration
	AutoRetry        bool
}

type Initializer struct {
	name            string
	state           InitializationState
	lastErr         error
	mu              sync.Mutex
	destroyOnce     sync.Once
	bootstrapOnce   sync.Once
	attempts        int
	bootstrapTasks  []*BootstrapTask
	autoRetryActive bool
	autoRetryWg     sync.WaitGroup
	cancelAutoRetry context.CancelFunc
	conf            *InitializerConfig
}

func NewInitializer(name string, conf *InitializerConfig) *Initializer {
	if conf == nil {
		conf = &InitializerConfig{
			RetryMinDelay:    3 * time.Second,
			RetryFactor:      1.5,
			RetryMaxDelay:    30 * time.Second,
			BootstrapTimeout: 0,
			AutoRetry:        false,
		}
	}
	return &Initializer{
		name:  name,
		state: StateUninitialized,
		conf:  conf,
	}
}

func (i *Initializer) Register(registerTasksFn func() []*BootstrapTask) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.state == StateDestroyed {
		return fmt.Errorf("cannot register %s, destroyed", i.name)
	}
	switch i.state {
	case StateRegistered, StateInitializing, StateReady, StatePartial, StateFailed:
		return nil
	}
	if i.state == StateUninitialized {
		tasks := registerTasksFn()
		for _, t := range tasks {
			t.state = TaskPending
		}
		i.bootstrapTasks = tasks
		i.state = StateRegistered
		i.lastErr = nil
	}
	return nil
}

// Bootstrap will attempt to run all tasks. If any tasks fail, it sets partial or failed.
// It waits up to InitializerConfig.BootstrapTimeout. If it times out, it returns immediately.
// After that, if AutoRetry is true and tasks are not fully ready, it starts auto-retries in the background.
func (i *Initializer) Bootstrap(ctx context.Context) error {
	var err error

	i.bootstrapOnce.Do(func() {
		i.mu.Lock()
		if i.state == StateDestroyed {
			i.mu.Unlock()
			err = fmt.Errorf("cannot bootstrap %s, destroyed", i.name)
			return
		}
		if i.state == StateReady {
			i.mu.Unlock()
			return
		}
		if i.state == StateUninitialized {
			i.state = StateRegistered
		}
		i.state = StateInitializing
		i.attempts++
		i.mu.Unlock()

		var bctx context.Context
		var cancel context.CancelFunc
		if i.conf.BootstrapTimeout > 0 {
			bctx, cancel = context.WithTimeout(ctx, i.conf.BootstrapTimeout)
		} else {
			bctx, cancel = context.WithCancel(ctx)
		}
		defer cancel()

		i.runBootstrapTasks(bctx)

		i.mu.Lock()
		if i.state == StateDestroyed {
			i.mu.Unlock()
			err = fmt.Errorf("bootstrap canceled, %s destroyed", i.name)
			return
		}

		failedCount := 0
		pendingCount := 0
		for _, task := range i.bootstrapTasks {
			if task.state == TaskFailed {
				failedCount++
			} else if task.state == TaskPending {
				pendingCount++
			}
		}

		if failedCount > 0 {
			successCount := len(i.bootstrapTasks) - failedCount - pendingCount
			if successCount > 0 {
				i.state = StatePartial
			} else {
				i.state = StateFailed
			}
			i.lastErr = fmt.Errorf("%d task(s) failed", failedCount)
			err = i.lastErr
			if i.conf.AutoRetry {
				i.startAutoRetryLocked(ctx)
			}
			i.mu.Unlock()
			return
		}

		if pendingCount > 0 {
			i.state = StatePartial
			i.lastErr = fmt.Errorf("%d task(s) still pending or timed out", pendingCount)
			err = i.lastErr
			if i.conf.AutoRetry {
				i.startAutoRetryLocked(ctx)
			}
			i.mu.Unlock()
			return
		}

		i.state = StateReady
		i.lastErr = nil
		i.mu.Unlock()
	})

	return err
}

// runBootstrapTasks runs tasks sequentially. If the context times out mid-task, subsequent tasks remain pending.
func (i *Initializer) runBootstrapTasks(ctx context.Context) {
	i.mu.Lock()
	defer i.mu.Unlock()

	for _, task := range i.bootstrapTasks {
		if task.state == TaskSucceeded || task.state == TaskFailed {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		task.state = TaskRunning
		task.attempts++
		task.lastAttempt = time.Now()
		i.mu.Unlock()
		err := task.Fn(ctx)
		i.mu.Lock()
		if ctx.Err() != nil {
			// If context timed out or canceled after the call started,
			// we just stop running further tasks. This task might have partial side-effects.
			if err == nil {
				// If the function didn't error, but context ended, we can mark it partial success or just fail
				// for simplicity let's fail it.
				err = ctx.Err()
			}
		}
		if err != nil {
			task.state = TaskFailed
			task.lastErr = err
		} else {
			task.state = TaskSucceeded
		}
	}
}

func (i *Initializer) RetryFailedTasks(ctx context.Context) error {
	i.mu.Lock()
	switch i.state {
	case StateReady:
		i.mu.Unlock()
		return nil
	case StateFailed, StatePartial:
		i.state = StateInitializing
	default:
		if i.state == StateDestroyed {
			i.mu.Unlock()
			return fmt.Errorf("cannot retry %s, destroyed", i.name)
		}
		if i.state == StateInitializing {
			i.mu.Unlock()
			return errors.New("already initializing, cannot retry")
		}
		if i.state == StateUninitialized || i.state == StateRegistered {
			i.mu.Unlock()
			return fmt.Errorf("cannot retry tasks, must bootstrap first")
		}
	}
	i.attempts++
	i.mu.Unlock()

	i.runBootstrapTasks(ctx)

	i.mu.Lock()
	defer i.mu.Unlock()

	failedCount := 0
	pendingCount := 0
	for _, t := range i.bootstrapTasks {
		if t.state == TaskFailed {
			failedCount++
		} else if t.state == TaskPending {
			pendingCount++
		}
	}
	if failedCount == 0 && pendingCount == 0 {
		i.state = StateReady
		i.lastErr = nil
		return nil
	}
	if failedCount == 0 && pendingCount > 0 {
		i.state = StatePartial
		i.lastErr = fmt.Errorf("%d task(s) still pending", pendingCount)
		return i.lastErr
	}
	successCount := len(i.bootstrapTasks) - failedCount - pendingCount
	if successCount > 0 {
		i.state = StatePartial
		i.lastErr = fmt.Errorf("%d task(s) still failed", failedCount)
	} else {
		i.state = StateFailed
		i.lastErr = fmt.Errorf("all tasks failed")
	}
	return i.lastErr
}

// StartAutoRetry can be called manually, but we also call it automatically
// if AutoRetry is true and the first bootstrap ends in partial/failure.
func (i *Initializer) StartAutoRetry(ctx context.Context) {
	i.mu.Lock()
	i.startAutoRetryLocked(ctx)
	i.mu.Unlock()
}

func (i *Initializer) startAutoRetryLocked(ctx context.Context) {
	if i.autoRetryActive {
		return
	}
	if i.state == StateDestroyed {
		return
	}
	i.autoRetryActive = true

	rctx, cancel := context.WithCancel(ctx)
	i.cancelAutoRetry = cancel
	i.autoRetryWg.Add(1)

	go i.autoRetryLoop(rctx)
}

func (i *Initializer) autoRetryLoop(ctx context.Context) {
	defer i.autoRetryWg.Done()
	defer i.cancelAutoRetry()

	delay := i.conf.RetryMinDelay
	for {
		i.mu.Lock()
		s := i.state
		i.mu.Unlock()

		if s == StateDestroyed || s == StateReady {
			return
		}

		err := i.RetryFailedTasks(ctx)
		if err == nil {
			i.mu.Lock()
			if i.state == StateReady || i.state == StateDestroyed {
				i.mu.Unlock()
				return
			}
			i.mu.Unlock()
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		delay = time.Duration(float64(delay) * i.conf.RetryFactor)
		if delay > i.conf.RetryMaxDelay {
			delay = i.conf.RetryMaxDelay
		}
	}
}

func (i *Initializer) Destroy(destroyFn func() error) error {
	var err error
	i.destroyOnce.Do(func() {
		i.mu.Lock()
		if i.state == StateDestroyed {
			i.mu.Unlock()
			return
		}
		if i.cancelAutoRetry != nil {
			i.cancelAutoRetry()
		}
		i.state = StateDestroyed
		i.mu.Unlock()

		i.autoRetryWg.Wait()

		if destroyFn != nil {
			err = destroyFn()
		}
	})
	return err
}

func (i *Initializer) MarkPartial(err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.state == StateDestroyed {
		return
	}
	i.state = StatePartial
	i.lastErr = err
}

func (i *Initializer) State() InitializationState {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.state
}

func (i *Initializer) LastError() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.lastErr
}

func (i *Initializer) Name() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.name
}

func (i *Initializer) Attempts() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.attempts
}

type TaskStatus struct {
	Name        string
	State       TaskState
	Err         error
	LastAttempt time.Time
	Attempts    int
}

func (i *Initializer) TasksStatus() []TaskStatus {
	i.mu.Lock()
	defer i.mu.Unlock()

	res := make([]TaskStatus, 0, len(i.bootstrapTasks))
	for _, t := range i.bootstrapTasks {
		res = append(res, TaskStatus{
			Name:        t.Name,
			State:       t.state,
			Err:         t.lastErr,
			LastAttempt: t.lastAttempt,
			Attempts:    t.attempts,
		})
	}
	return res
}

type InitializerStatus struct {
	State     InitializationState
	LastError error
	Tasks     []TaskStatus
}

func (i *Initializer) Status() *InitializerStatus {
	return &InitializerStatus{
		State:     i.State(),
		LastError: i.LastError(),
		Tasks:     i.TasksStatus(),
	}
}
