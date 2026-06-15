package common

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildUpstreamInitErr constructs a bare ErrUpstreamClientInitialization with
// the given cause — the shape produced by upstream.go for the RPC-failure path.
func buildUpstreamInitErr(cause error) error {
	return &ErrUpstreamClientInitialization{
		BaseError: BaseError{
			Code:    "ErrUpstreamClientInitialization",
			Message: "could not initialize upstream client",
			Cause:   cause,
		},
	}
}

// TestErrUpstreamClientInitialization_NotFatalByDefault pins the contract that
// the base error type does NOT implement IsTaskFatal. Call sites that know the
// cause is permanent (chainId mismatch, parse error, unsupported type) wrap
// with common.NewTaskFatal(). Call sites that carry a potentially transient
// cause (RPC failure during chainId detection) leave the error unwrapped so
// the Initializer's auto-retry loop keeps trying.
func TestErrUpstreamClientInitialization_NotFatalByDefault(t *testing.T) {
	err := buildUpstreamInitErr(errors.New("connection refused during startup"))

	var fatal interface{ IsTaskFatal() bool }
	assert.False(t, errors.As(err, &fatal),
		"bare ErrUpstreamClientInitialization must NOT satisfy IsTaskFatal so transient failures stay retryable")
}

// TestErrUpstreamClientInitialization_WrappedWithTaskFatal_IsFatal verifies
// that call sites with permanently-broken causes (chainId mismatch, parse
// error) produce an error that the Initializer treats as fatal.
func TestErrUpstreamClientInitialization_WrappedWithTaskFatal_IsFatal(t *testing.T) {
	inner := buildUpstreamInitErr(errors.New("chainId mismatch: configured 1, detected 137"))
	wrapped := NewTaskFatal(inner)

	var fatal interface{ IsTaskFatal() bool }
	require.True(t, errors.As(wrapped, &fatal),
		"NewTaskFatal-wrapped ErrUpstreamClientInitialization must satisfy IsTaskFatal")
	assert.True(t, fatal.IsTaskFatal())

	// Sanity: the underlying error type is still reachable via errors.As.
	var uErr *ErrUpstreamClientInitialization
	assert.True(t, errors.As(wrapped, &uErr),
		"the underlying ErrUpstreamClientInitialization must still be retrievable via errors.As")
}

// TestErrUpstreamClientInitialization_WrappedStopsInitializerRetryLoop
// exercises the real util.Initializer: a task that returns a
// NewTaskFatal-wrapped ErrUpstreamClientInitialization transitions to
// TaskFatal after a single attempt. This covers the "permanent failure"
// call sites (chainId mismatch, parse error, unsupported client type).
func TestErrUpstreamClientInitialization_WrappedStopsInitializerRetryLoop(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.New(zerolog.NewTestWriter(t))
	init := util.NewInitializer(appCtx, &logger, &util.InitializerConfig{
		TaskTimeout:   time.Second,
		AutoRetry:     true,
		RetryMinDelay: time.Millisecond,
		RetryMaxDelay: time.Millisecond * 10,
		RetryFactor:   1.1,
	})
	require.NotNil(t, init)

	var attempts int32
	task := util.NewBootstrapTask("chainid-mismatch", func(ctx context.Context) error {
		atomic.AddInt32(&attempts, 1)
		return NewTaskFatal(buildUpstreamInitErr(
			errors.New("chainId mismatch: configured 1, detected 137"),
		))
	})

	runCtx, runCancel := context.WithTimeout(appCtx, 200*time.Millisecond)
	defer runCancel()
	_ = init.ExecuteTasks(runCtx, task)

	time.Sleep(150 * time.Millisecond)
	init.Stop(nil)

	got := atomic.LoadInt32(&attempts)
	assert.Equal(t, int32(1), got,
		"NewTaskFatal-wrapped ErrUpstreamClientInitialization must stop retries (attempted %d times)", got)
	assert.Equal(t, util.StateFatal, init.State(),
		"initializer must be in StateFatal after a permanent init failure")
}

// TestErrUpstreamClientInitialization_TransientCauseIsRetried verifies the
// other direction: an RPC-failure-path error (unwrapped) keeps the task
// retryable, so the upstream can self-heal when the provider recovers.
func TestErrUpstreamClientInitialization_TransientCauseIsRetried(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.New(zerolog.NewTestWriter(t))
	init := util.NewInitializer(appCtx, &logger, &util.InitializerConfig{
		TaskTimeout:   time.Second,
		AutoRetry:     true,
		RetryMinDelay: time.Millisecond,
		RetryMaxDelay: time.Millisecond * 10,
		RetryFactor:   1.1,
	})
	require.NotNil(t, init)

	var attempts int32
	task := util.NewBootstrapTask("transient-network", func(ctx context.Context) error {
		atomic.AddInt32(&attempts, 1)
		return buildUpstreamInitErr(errors.New("connection refused during startup"))
	})

	runCtx, runCancel := context.WithTimeout(appCtx, 200*time.Millisecond)
	defer runCancel()
	_ = init.ExecuteTasks(runCtx, task)

	time.Sleep(150 * time.Millisecond)
	init.Stop(nil)

	got := atomic.LoadInt32(&attempts)
	assert.Greater(t, got, int32(5),
		"bare ErrUpstreamClientInitialization (transient cause) must keep retrying; observed only %d attempts", got)
	assert.NotEqual(t, util.StateFatal, init.State(),
		"initializer must NOT enter StateFatal for transient init failures")
}
