package util

import (
	"context"
	"fmt"
	"runtime/debug"
)

// BoundedCall runs fn in a goroutine and waits up to ctx.Done() for it
// to complete. If ctx fires first, returns ctx.Err() and lets the
// goroutine "leak" — the underlying stdlib (grpc-go, net/http, etc.)
// cleans it up when it eventually honors the context.
//
// This is the foundation defense against a class of outbound-conn
// wedge where the stdlib transport doesn't wake on ctx cancellation.
// Observed in production for both gRPC streams (H2 flow-control
// deadlocks on Fly's egress) and HTTP requests (net/http's client.Do
// not honoring ctx when the connection is in a bad state). The select
// below guarantees the CALLER returns within ctx's deadline regardless
// of the stdlib's internal state. The abandoned goroutine is freed
// when the stdlib's own cleanup eventually fires.
//
// If ctx fires simultaneously with fn returning, prefer ctx.Err() over
// the returned error — the proximate cause is still the deadline.
func BoundedCall(ctx context.Context, fn func(context.Context) error) error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- fmt.Errorf("bounded-call goroutine panic: %v\n%s", rec, debug.Stack())
			}
		}()
		done <- fn(ctx)
	}()
	select {
	case err := <-done:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// BoundedCallT is the typed wrapper for functions that return a value.
// Sends the concrete return through a channel so the caller and the
// (possibly abandoned) inner goroutine never race on a shared variable.
func BoundedCallT[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
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
			return zero, ctx.Err()
		}
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}
