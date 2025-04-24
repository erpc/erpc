package common

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var key = "test-key"

func TestMultiplexer_NewMultiplexer(t *testing.T) {
	m := NewMultiplexer[string](key)

	if m.Key() != key {
		t.Errorf("Expected key %s, got %s", key, m.Key())
	}

	// Verify done channel is initialized but not closed
	select {
	case <-m.Done():
		t.Error("Done channel should not be closed on initialization")
	default:
	}
}

func TestMultiplexer_Close(t *testing.T) {
	ctx := context.Background()
	m := NewMultiplexer[string](key)

	expectedResult := "test-result"
	expectedErr := errors.New("test-error")

	m.Close(ctx, expectedResult, expectedErr)

	// Verify done channel is closed
	select {
	case <-m.Done():
		// This is the expected behavior
	case <-time.After(100 * time.Millisecond):
		t.Error("Done channel should be closed after Close is called")
	}

	// Verify result and error are set correctly
	result, err := m.Result()
	if result != expectedResult {
		t.Errorf("Expected result %s, got %s", expectedResult, result)
	}
	if err != expectedErr {
		t.Errorf("Expected error %v, got %v", expectedErr, err)
	}
}

func TestMultiplexer_CloseOnce(t *testing.T) {
	ctx := context.Background()
	m := NewMultiplexer[string](key)

	// First close
	m.Close(ctx, "first-result", errors.New("first-error"))

	// Second close with different values
	m.Close(ctx, "second-result", errors.New("second-error"))

	// Verify only the first close took effect
	result, err := m.Result()
	if result != "first-result" {
		t.Errorf("Expected result %s, got %s", "first-result", result)
	}
	if err == nil || err.Error() != "first-error" {
		t.Errorf("Expected error %v, got %v", errors.New("first-error"), err)
	}
}

func TestMultiplexer_ConcurrentClose(t *testing.T) {
	ctx := context.Background()
	m := NewMultiplexer[string](key)

	var wg sync.WaitGroup
	closeCalls := 100
	wg.Add(closeCalls)

	for i := 0; i < closeCalls; i++ {
		go func(i int) {
			defer wg.Done()
			m.Close(ctx, "result", nil)
		}(i)
	}

	wg.Wait()

	// Verify done channel is closed exactly once
	select {
	case <-m.Done():
		// This is the expected behavior
	default:
		t.Error("Done channel should be closed after concurrent Close calls")
	}
}

func TestMultiplexer_WaitForResult(t *testing.T) {
	ctx := context.Background()
	m := NewMultiplexer[string](key)

	expectedResult := "test-result"

	// Start a goroutine that will close the multiplexer after a short delay
	go func() {
		time.Sleep(100 * time.Millisecond)
		m.Close(ctx, expectedResult, nil)
	}()

	// Wait for the result
	select {
	case <-m.Done():
		result, err := m.Result()
		if result != expectedResult {
			t.Errorf("Expected result %s, got %s", expectedResult, result)
		}
		if err != nil {
			t.Errorf("Expected nil error, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("Timed out waiting for result")
	}
}

func TestMultiplexer_ResultAlreadyAvailable(t *testing.T) {
	ctx := context.Background()
	m := NewMultiplexer[string](key)

	expectedResult := "test-result"
	m.Close(ctx, expectedResult, nil)

	// Result should be immediately available
	select {
	case <-m.Done():
		result, err := m.Result()
		if result != expectedResult {
			t.Errorf("Expected result %s, got %s", expectedResult, result)
		}
		if err != nil {
			t.Errorf("Expected nil error, got %v", err)
		}
	default:
		t.Error("Done channel should be closed and result should be immediately available")
	}
}

func TestMultiplexer_ContextCancellation(t *testing.T) {
	m := NewMultiplexer[string](key)

	// Create a context that will be canceled
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel the context
	cancel()

	// Wait for either the result or context cancellation
	select {
	case <-m.Done():
		t.Error("Should not receive result before Close is called")
	case <-ctx.Done():
		// This is the expected behavior
	case <-time.After(100 * time.Millisecond):
		t.Error("Context cancellation should be detected")
	}
}
