package util

import (
	"context"
	"fmt"
	"runtime/debug"
)

// BoundedCallT runs fn in a goroutine and waits up to ctx.Done() for it to
// complete. If ctx fires first, returns context.Cause(ctx) — which surfaces
// the explicit cancellation cause (e.g. common.ErrDynamicTimeoutExceeded
// when ctx was wrapped via context.WithTimeoutCause) rather than the
// generic context.DeadlineExceeded — and lets the goroutine "leak" so the
// underlying stdlib (grpc-go, net/http, etc.) can clean it up when it
// eventually honors the context.
//
// This is the foundation defense against a class of outbound-conn wedge
// where the stdlib transport doesn't wake on ctx cancellation (observed
// in production for both gRPC H2 flow-control deadlocks and net/http's
// client.Do refusing to honor ctx when the connection is in a bad state).
// The select guarantees the CALLER returns within ctx's deadline
// regardless of the stdlib's internal state. The abandoned goroutine is
// freed when the stdlib's own cleanup eventually fires.
//
// If ctx fires simultaneously with fn returning, prefer ctx's cause over
// the returned error — the proximate cause is still the deadline.
func BoundedCallT[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	// Fast path: ctx is already done. Avoid spawning a goroutine that
	// would just immediately observe its own ctx is dead.
	if ctx.Err() != nil {
		var zero T
		return zero, context.Cause(ctx)
	}

	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- result{err: fmt.Errorf("bounded-call goroutine panic: %v\n%s", rec, debug.Stack())}
			}
		}()
		v, err := fn(ctx)
		done <- result{v: v, err: err}
	}()
	select {
	case r := <-done:
		if ctx.Err() != nil {
			var zero T
			return zero, context.Cause(ctx)
		}
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, context.Cause(ctx)
	}
}

// BoundedCall is the untyped variant of BoundedCallT — useful when fn
// has no meaningful return value (e.g. a stream-drain loop).
func BoundedCall(ctx context.Context, fn func(context.Context) error) error {
	_, err := BoundedCallT(ctx, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	})
	return err
}
