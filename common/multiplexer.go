package common

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/trace"
)

// Multiplexer provides a way to deduplicate concurrent operations with the same key.
// It ensures that only one operation is executed while others wait for the result.
// Type parameter T represents the result type of the operation.
type Multiplexer[T any] struct {
	Key    string
	result T
	err    error
	done   chan struct{}
	mu     sync.RWMutex
	once   sync.Once
}

// NewMultiplexer creates a new multiplexer instance with the given key.
func NewMultiplexer[T any](key string) *Multiplexer[T] {
	return &Multiplexer[T]{
		Key:  key,
		done: make(chan struct{}),
	}
}

// Close signals that the operation is complete and provides the result and error.
// It ensures that Close is only called once, even if called from multiple goroutines.
func (m *Multiplexer[T]) Close(ctx context.Context, result T, err error) {
	_, span := StartDetailSpan(ctx, "Multiplexer.Close",
		trace.WithAttributes(),
	)
	defer span.End()

	m.once.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.result = result
		m.err = err
		close(m.done)
	})
}

// Done returns a channel that is closed when the operation completes.
func (m *Multiplexer[T]) Done() <-chan struct{} {
	return m.done
}

// Result returns the result and error from the completed operation.
// It should only be called after Done() channel is closed.
func (m *Multiplexer[T]) Result() (T, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.result, m.err
}

// ExecuteMultiplexed provides a generic way to multiplex operations with the same key.
// It ensures that only one operation with the same key is executed at a time,
// while other callers wait for the result.
func ExecuteMultiplexed[T any](
	ctx context.Context,
	registry *sync.Map,
	key string,
	operation func(context.Context) (T, error),
) (T, error) {
	_, span := StartDetailSpan(ctx, "Multiplexer.ExecuteMultiplexed",
		trace.WithAttributes(),
	)
	defer span.End()

	// Try to load an existing multiplexer or create a new one
	existingMux, loaded := registry.LoadOrStore(key, NewMultiplexer[T](key))
	mux := existingMux.(*Multiplexer[T])

	if loaded {
		// Another goroutine is already executing the operation, wait for its result
		select {
		case <-mux.Done():
			// Result is ready
			return mux.Result()
		case <-ctx.Done():
			// Context cancelled while waiting
			return *new(T), ctx.Err()
		}
	} else {
		// This goroutine is the leader, execute the operation
		defer registry.Delete(key)

		// Execute the actual operation
		result, err := operation(ctx)

		// Close the multiplexer to signal completion to waiters
		mux.Close(ctx, result, err)

		return result, err
	}
}
