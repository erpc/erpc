package data

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type SharedVariable interface {
	IsStale(staleness time.Duration) bool
}

type CounterInt64SharedVariable interface {
	SharedVariable
	GetValue() int64
	TryUpdateIfStale(ctx context.Context, staleness time.Duration, getNewValue func(ctx context.Context) (int64, error)) (int64, error)
	TryUpdate(ctx context.Context, newValue int64) int64
	// SetLocalValue updates the local in-memory value synchronously without any network calls.
	// Returns true if the value was actually updated (newValue > current).
	// Use this for fast path updates where remote sync can happen asynchronously.
	SetLocalValue(newValue int64) bool
	OnValue(callback func(int64))
	OnLargeRollback(callback func(currentVal, newVal int64))
}

type baseSharedVariable struct {
	lastProcessed   time.Time
	lastProcessedMu sync.RWMutex
}

func (v *baseSharedVariable) IsStale(staleness time.Duration) bool {
	v.lastProcessedMu.RLock()
	defer v.lastProcessedMu.RUnlock()
	if v.lastProcessed.IsZero() {
		return true
	}
	return time.Since(v.lastProcessed) > staleness
}

type counterInt64 struct {
	baseSharedVariable
	registry               *sharedStateRegistry
	key                    string
	value                  atomic.Int64
	updateMu               sync.Mutex
	valueCallbacks         []func(int64)
	ignoreRollbackOf       int64
	largeRollbackCallbacks []func(localVal, newVal int64)
	callbackMu             sync.RWMutex
	// bgPushInProgress dedupes best-effort remote reconciliation/push operations.
	// It MUST NOT be held while waiting on updateMu; remote sync should never block request flow.
	bgPushInProgress sync.Mutex
	// bgPushRequested indicates a newer value should be pushed after the current push attempt.
	// This avoids dropping updates when scheduleBackgroundPushCurrent() is called while a push is in progress.
	bgPushRequested atomic.Bool
}

func (c *counterInt64) GetValue() int64 {
	return c.value.Load()
}

// SetLocalValue updates only the local in-memory atomic value without any network/distributed operations.
// This is a fast, synchronous update for cases where immediate local visibility is needed
// and remote sync can happen asynchronously later.
// Returns true if the value was actually updated (newValue > currentValue).
func (c *counterInt64) SetLocalValue(newValue int64) bool {
	currentValue := c.value.Load()
	if newValue <= currentValue {
		return false
	}
	// Use CompareAndSwap for thread-safety without holding the mutex long
	// If another goroutine updated it in the meantime, we retry
	for {
		if c.value.CompareAndSwap(currentValue, newValue) {
			// Mark as fresh so TryUpdateIfStale won't trigger unnecessary polls
			c.lastProcessedMu.Lock()
			c.lastProcessed = time.Now()
			c.lastProcessedMu.Unlock()

			c.registry.logger.Trace().
				Str("key", c.key).
				Int64("from", currentValue).
				Int64("to", newValue).
				Msg("local counter value set synchronously")
			c.triggerValueCallback(newValue)
			return true
		}
		// Someone else updated it, re-check
		currentValue = c.value.Load()
		if newValue <= currentValue {
			return false // Our value is no longer an improvement
		}
	}
}

func (c *counterInt64) processNewValue(source string, newVal int64) bool {
	// processNewValue is safe for concurrent use:
	// - Local value update is done via CAS (coordinates with SetLocalValue).
	// - lastProcessed is protected by lastProcessedMu.
	// - callbacks are copied under callbackMu.
	//
	// NOTE: updateMu is NOT required for correctness of this function; updateMu exists
	// to coordinate expensive refresh execution in TryUpdateIfStale (thundering herd control).
	currentValue := c.value.Load()
	updated := false
	if newVal > currentValue {
		// Use CAS loop to handle race with SetLocalValue
		for {
			if c.value.CompareAndSwap(currentValue, newVal) {
				updated = true
				c.registry.logger.Trace().Str("source", source).Str("key", c.key).Int64("from", currentValue).Int64("to", newVal).Msg("counter value increased")
				c.triggerValueCallback(newVal)
				break
			}
			// CAS failed - someone else updated (likely SetLocalValue)
			currentValue = c.value.Load()
			if newVal <= currentValue {
				// Our value is no longer an improvement, abort
				c.registry.logger.Trace().Str("source", source).Str("key", c.key).Int64("newVal", newVal).Int64("currentValue", currentValue).Msg("CAS failed, value no longer an improvement")
				break
			}
			// Retry with updated currentValue
		}
	} else if currentValue > newVal && (currentValue-newVal > c.ignoreRollbackOf) {
		// Rollback detection: The new value is significantly lower than current.
		// This could indicate a blockchain reorg OR stale data racing with SetLocalValue.
		//
		// We do NOT apply the rollback here because:
		// 1. SetLocalValue may have set a higher value from fresh RPC data
		// 2. Even remote-sync updates might carry stale data from lagging upstreams
		// 3. For block numbers (the primary use case), values should only increase
		//
		// If a real reorg happened, subsequent polls will naturally pick up the
		// correct lower value through the normal forward-progress path.
		// We still trigger the rollback callbacks to notify listeners of the detection.
		c.registry.logger.Trace().
			Str("source", source).
			Str("key", c.key).
			Int64("currentValue", currentValue).
			Int64("newVal", newVal).
			Int64("gap", currentValue-newVal).
			Msg("rollback detected but not applied to preserve SetLocalValue invariant")

		c.callbackMu.RLock()
		callbacks := make([]func(localVal, newVal int64), len(c.largeRollbackCallbacks))
		copy(callbacks, c.largeRollbackCallbacks)
		c.callbackMu.RUnlock()
		for _, cb := range callbacks {
			if cb != nil {
				cb(currentValue, newVal)
			}
		}
	}

	// We have just consulted a fresh source; refresh the timestamp
	// so the value is considered fresh for the debounce window even
	// when it did not change.
	c.lastProcessedMu.Lock()
	c.lastProcessed = time.Now()
	c.lastProcessedMu.Unlock()

	c.registry.logger.Trace().Str("source", source).Str("key", c.key).Str("ptr", fmt.Sprintf("%p", c)).Int64("currentValue", currentValue).Int64("newVal", newVal).Bool("updated", updated).Msg("processed new value")
	return updated
}

func (c *counterInt64) triggerValueCallback(val int64) {
	c.callbackMu.RLock()
	callbacks := make([]func(int64), len(c.valueCallbacks))
	copy(callbacks, c.valueCallbacks)
	c.callbackMu.RUnlock()

	for _, cb := range callbacks {
		if cb != nil {
			cb(val)
		}
	}
}

func (c *counterInt64) TryUpdate(ctx context.Context, newValue int64) int64 {
	ctx, span := common.StartSpan(ctx, "CounterInt64.TryUpdate",
		trace.WithAttributes(
			attribute.String("key", c.key),
		),
	)
	defer span.End()
	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.Int64("new_value", newValue),
		)
	}
	// Local-only fast path. Remote reconciliation/push is always best-effort and async.
	// Capture pre-update value to decide whether it's worth scheduling a push.
	prev := c.value.Load()

	c.updateMu.Lock()
	_ = c.processNewValue(UpdateSourceTryUpdate, newValue)
	c.updateMu.Unlock()

	// Schedule a background push only when the caller is attempting to advance the value
	// (newValue >= prev). This avoids unnecessary remote traffic for obviously stale inputs
	// (e.g. newValue << prev), while still supporting the SetLocalValue+TryUpdate pattern
	// where local was already updated and TryUpdate is used purely to propagate to remote.
	if newValue > 0 && newValue >= prev {
		c.scheduleBackgroundPushCurrent()
	}
	return c.value.Load()
}

func (c *counterInt64) TryUpdateIfStale(ctx context.Context, staleness time.Duration, executeNewValueFn func(ctx context.Context) (int64, error)) (int64, error) {
	ctx, span := common.StartSpan(ctx, "CounterInt64.TryUpdateIfStale",
		trace.WithAttributes(
			attribute.String("key", c.key),
			attribute.Int64("staleness_ms", staleness.Milliseconds()),
		),
	)
	defer span.End()

	if !c.IsStale(staleness) {
		span.SetAttributes(attribute.Bool("skipped_not_stale", true))
		return c.value.Load(), nil
	}

	// Track time waiting for mutex
	_, mutexSpan := common.StartSpan(ctx, "CounterInt64.TryUpdateIfStale.AcquireMutex")
	c.updateMu.Lock()
	mutexSpan.End()
	defer c.updateMu.Unlock()

	// Double-check staleness after acquiring mutex
	if !c.IsStale(staleness) {
		span.SetAttributes(attribute.Bool("skipped_not_stale_after_mutex", true))
		return c.value.Load(), nil
	}

	initialVal := c.value.Load()
	span.SetAttributes(attribute.Int64("initial_value", initialVal))
	// IMPORTANT: Foreground path MUST be local-only and bounded by updateMaxWait.
	// Do NOT acquire distributed locks or do any remote I/O here, otherwise degraded shared state
	// (e.g. Redis lock acquisition) can block normal request flow.
	span.SetAttributes(attribute.Bool("foreground_remote_io_disabled", true))

	// Execute the refresh function (e.g., RPC call to get latest block) in background
	resultCh := make(chan refreshResult, 1)
	go func() {
		fnCtx, fnCancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
		defer fnCancel()

		// Create a span for the actual RPC/refresh call - this is usually what takes time
		_, fnSpan := common.StartSpan(ctx, "CounterInt64.TryUpdateIfStale.ExecuteRefresh",
			trace.WithAttributes(
				attribute.String("key", c.key),
				attribute.Int64("timeout_ms", c.registry.fallbackTimeout.Milliseconds()),
			),
		)
		value, err := executeNewValueFn(fnCtx)
		if err != nil {
			fnSpan.SetAttributes(attribute.String("error", err.Error()))
		} else {
			fnSpan.SetAttributes(attribute.Int64("result_value", value))
		}
		fnSpan.End()

		resultCh <- refreshResult{val: value, err: err}
	}()

	timer := time.NewTimer(c.registry.updateMaxWait)
	defer timer.Stop()
	span.SetAttributes(attribute.String("role", "local"))

	select {
	case r := <-resultCh:
		span.SetAttributes(
			attribute.String("result_path", "got_result"),
			attribute.Bool("async_refresh", false),
		)
		// Always behave like "follower" in the synchronous path: local update only,
		// remote reconciliation/push happens asynchronously via scheduleBackgroundPushCurrent().
		return c.applyRefreshResult(initialVal, r)
	case <-timer.C:
		span.SetAttributes(
			attribute.String("result_path", "timeout"),
			attribute.Bool("async_refresh", true),
		)
		c.markFreshAttempt()
		c.continueAsyncRefresh(resultCh)
		return initialVal, nil
	}
}

func (c *counterInt64) OnValue(cb func(int64)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.valueCallbacks = append(c.valueCallbacks, cb)
}

func (c *counterInt64) OnLargeRollback(cb func(currentVal, newVal int64)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.largeRollbackCallbacks = append(c.largeRollbackCallbacks, cb)
}

func (c *counterInt64) markFreshAttempt() {
	c.lastProcessedMu.Lock()
	c.lastProcessed = time.Now()
	c.lastProcessedMu.Unlock()
}

type refreshResult struct {
	val int64
	err error
}

func (c *counterInt64) applyRefreshResult(initialVal int64, r refreshResult) (int64, error) {
	if r.err != nil {
		c.markFreshAttempt()
		return initialVal, r.err
	}
	updated := c.processNewValue(UpdateSourceTryUpdateIfStale, r.val)
	if updated {
		c.scheduleBackgroundPushCurrent()
	}
	return c.value.Load(), nil
}

func (c *counterInt64) continueAsyncRefresh(resultCh <-chan refreshResult) {
	go func() {
		r, ok := <-resultCh
		if !ok {
			return
		}
		// Apply result locally; remote push is scheduled async and MUST NOT block request flow.
		c.updateMu.Lock()
		_, _ = c.applyRefreshResult(c.value.Load(), r)
		c.updateMu.Unlock()
	}()
}

func (c *counterInt64) tryAcquireLock(ctx context.Context) func() {
	lock, err := c.registry.connector.Lock(ctx, c.key, c.registry.lockTtl)
	if err != nil {
		// Log differently based on error type
		if strings.Contains(err.Error(), "lock already taken") {
			c.registry.logger.Debug().Err(err).Str("key", c.key).Msg("lock held by another instance, proceeding with local lock")
		} else if errors.Is(err, context.DeadlineExceeded) {
			c.registry.logger.Warn().Err(err).Str("key", c.key).Msg("lock acquisition timed out waiting for other instance")
		} else {
			c.registry.logger.Warn().Err(err).Str("key", c.key).Msg("failed to remotely lock counter will only use local lock")
		}
	}
	if lock != nil && !lock.IsNil() {
		c.registry.logger.Debug().Str("key", c.key).Msg("acquired remote lock for counter")
		return func() {
			unlockCtx, cancel := context.WithTimeout(c.registry.appCtx, c.registry.lockTtl)
			defer cancel()
			if err := lock.Unlock(unlockCtx); err != nil {
				// "lock was already expired" is expected when operations take longer than anticipated
				if strings.Contains(err.Error(), "lock was already expired") {
					c.registry.logger.Debug().Err(err).Str("key", c.key).Int64("lock_ttl_ms", c.registry.lockTtl.Milliseconds()).Msg("lock expired during operations (expected behavior)")
				} else {
					c.registry.logger.Debug().Err(err).Str("key", c.key).Int64("lock_ttl_ms", c.registry.lockTtl.Milliseconds()).Msg("failed to unlock counter, so it will be expired after ttl")
				}
			} else {
				c.registry.logger.Debug().Str("key", c.key).Msg("released remote lock for counter")
			}
		}
	}
	return nil
}

func (c *counterInt64) tryGetRemoteValue(ctx context.Context) int64 {
	remoteVal, err := c.registry.connector.Get(ctx, ConnectorMainIndex, c.key, "value", nil)
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			c.registry.logger.Debug().Err(err).Str("key", c.key).Msg("remote counter value not found, will initialize it")
		} else {
			c.registry.logger.Warn().Err(err).Str("key", c.key).Msg("failed to get remote counter value")
		}
	} else {
		remoteValueInt, err := strconv.ParseInt(string(remoteVal), 0, 0)
		if err != nil {
			c.registry.logger.Warn().Err(err).Str("key", c.key).Msg("failed to parse remote counter value")
		} else {
			return remoteValueInt
		}
	}
	return 0
}

func (c *counterInt64) updateRemoteUpdate(ctx context.Context, newValue int64) {
	err := c.registry.connector.Set(ctx, c.key, "value", []byte(fmt.Sprintf("%d", newValue)), nil)
	if err == nil {
		err = c.registry.connector.PublishCounterInt64(ctx, c.key, newValue)
	}
	if err != nil {
		c.registry.logger.Debug().Err(err).
			Str("key", c.key).
			Int64("value", newValue).
			Msg("failed to update remote counter value")
	} else {
		c.registry.logger.Debug().Str("key", c.key).Int64("value", newValue).Msg("published counter value to remote")
	}
}

// scheduleBackgroundPushCurrent dedupes and pushes the current local value to the
// remote store under a lock, without blocking the caller.
func (c *counterInt64) scheduleBackgroundPushCurrent() {
	// Always mark that a push is needed.
	c.bgPushRequested.Store(true)

	// Only one goroutine at a time runs the push loop.
	if !c.bgPushInProgress.TryLock() {
		return
	}

	go func() {
		defer c.bgPushInProgress.Unlock()

		for {
			// Coalesce: if no push is requested at loop start, we're done.
			if !c.bgPushRequested.Swap(false) {
				return
			}

			// Snapshot local value (atomic read; never block request flow here)
			localVal := c.value.Load()
			if localVal <= 0 {
				continue
			}

			// Best-effort fast propagation (no distributed lock): publish the latest value so
			// other instances can update their local counters quickly via WatchCounterInt64.
			// This is safe because counterInt64 never applies rollbacks (monotonic), so out-of-order
			// publishes won't regress local values.
			pubCtx, pubCancel := context.WithTimeout(c.registry.appCtx, c.registry.lockMaxWait)
			_ = c.registry.connector.PublishCounterInt64(pubCtx, c.key, localVal)
			pubCancel()

			// Acquire distributed lock with a bounded wait budget.
			// NOTE: This is a background operation and MUST NOT block request flow.
			lockCtx, lockCancel := context.WithTimeout(c.registry.appCtx, c.registry.lockMaxWait)
			unlock := c.tryAcquireLock(lockCtx)
			lockCancel()
			if unlock == nil {
				// Could not acquire lock quickly; rely on publish-only propagation for now.
				// If newer updates arrive, scheduleBackgroundPushCurrent() will set bgPushRequested
				// and this loop will run again.
				continue
			}

			func() {
				defer unlock()

				// Reconcile with remote under the distributed lock.
				getCtx, getCancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
				remoteVal := c.tryGetRemoteValue(getCtx)
				getCancel()

				// If remote is ahead, adopt it locally (briefly take updateMu for thundering herd coordination).
				if remoteVal > localVal {
					c.updateMu.Lock()
					_ = c.processNewValue(UpdateSourceRemoteCheck, remoteVal)
					c.updateMu.Unlock()
				}

				// Push max(local, remote) to remote.
				finalVal := c.value.Load()
				if remoteVal > finalVal {
					finalVal = remoteVal
				}
				if finalVal <= 0 {
					return
				}

				pctx, cancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
				c.updateRemoteUpdate(pctx, finalVal)
				cancel()
			}()
		}
	}()
}
