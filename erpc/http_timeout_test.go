package erpc

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func init() {
	util.ConfigureTestLogger()
}

func TestTimeoutHandler_TimeoutReturnsJsonRpcError(t *testing.T) {
	// Handler that takes longer than the timeout
	slowHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	})

	handler := TimeoutHandler(&log.Logger, slowHandler, 10*time.Millisecond)

	t.Run("POST request timeout returns 200 with JSON-RPC error body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		body := rec.Body.String()
		assert.NotEmpty(t, body, "response body should not be empty")
		assert.Contains(t, body, `"jsonrpc":"2.0"`)
		assert.Contains(t, body, `"error"`)
		assert.Contains(t, body, `"code":-32603`)
		assert.Contains(t, body, "timeout")
	})

	t.Run("GET request timeout returns 504 Gateway Timeout", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusGatewayTimeout, rec.Code)
	})
}

func TestTimeoutHandler_NonTimeoutContextCancellation(t *testing.T) {
	// This test verifies the bug fix: non-timeout context cancellation should
	// still return a proper JSON-RPC error body for POST requests, not an empty body.

	// Create a handler that waits for context cancellation
	handlerStarted := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		<-r.Context().Done() // Wait for context cancellation
	})

	// Use a long timeout so we can cancel the context before timeout
	wrappedHandler := TimeoutHandler(&log.Logger, handler, 10*time.Second)

	t.Run("POST request with context.Canceled returns 200 with JSON-RPC error body", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			wrappedHandler.ServeHTTP(rec, req)
			close(done)
		}()

		// Wait for handler to start, then cancel
		<-handlerStarted
		cancel()
		<-done

		assert.Equal(t, http.StatusOK, rec.Code, "POST request should return 200 OK for JSON-RPC")
		body := rec.Body.String()
		assert.NotEmpty(t, body, "response body should not be empty - this is the bug!")
		assert.Contains(t, body, `"jsonrpc":"2.0"`, "should return valid JSON-RPC error")
		assert.Contains(t, body, `"error"`, "should contain error field")
		assert.Contains(t, body, "cancelled by client", "should indicate client cancellation")
	})

	t.Run("GET request with context.Canceled returns 503 Service Unavailable", func(t *testing.T) {
		handlerStarted2 := make(chan struct{})
		handler2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(handlerStarted2)
			<-r.Context().Done()
		})
		wrappedHandler2 := TimeoutHandler(&log.Logger, handler2, 10*time.Second)

		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			wrappedHandler2.ServeHTTP(rec, req)
			close(done)
		}()

		<-handlerStarted2
		cancel()
		<-done

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "GET request should return 503")
	})
}

func TestTimeoutHandler_SuccessfulResponse(t *testing.T) {
	// Handler that responds quickly
	fastHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	})

	handler := TimeoutHandler(&log.Logger, fastHandler, 1*time.Second)

	t.Run("successful POST request returns response body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		body := rec.Body.String()
		assert.Equal(t, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`, body)
	})
}

func TestTimeoutHandler_LargeResponseSwitchesToPassthrough(t *testing.T) {
	prev := maxBufferedResponseBytes
	maxBufferedResponseBytes = 8 << 10 // 8KiB
	t.Cleanup(func() { maxBufferedResponseBytes = prev })

	largeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Write far beyond the buffer limit.
		_, _ = w.Write([]byte(strings.Repeat("x", 64<<10)))
	})

	handler := TimeoutHandler(&log.Logger, largeHandler, 10*time.Second)

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(`{"jsonrpc":"2.0","method":"eth_getLogs","id":1}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Should preserve feature behavior: serve the large response without returning
	// a synthetic "response too large" JSON-RPC error.
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, strings.Repeat("x", 64<<10), rec.Body.String())
}

func TestTimeoutHandler_PanicRecovery(t *testing.T) {
	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	handler := TimeoutHandler(&log.Logger, panicHandler, 1*time.Second)

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	rec := httptest.NewRecorder()

	assert.Panics(t, func() {
		handler.ServeHTTP(rec, req)
	})
}

func TestTimeoutWriter_Header(t *testing.T) {
	fastHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom-Header", "test-value")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("response"))
	})

	handler := TimeoutHandler(&log.Logger, fastHandler, 1*time.Second)

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "test-value", rec.Header().Get("X-Custom-Header"))
	assert.Equal(t, "response", rec.Body.String())
}

func TestTimeoutWriter_Push(t *testing.T) {
	// Test that Push returns ErrNotSupported for non-pusher response writers
	fastHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pusher, ok := w.(http.Pusher); ok {
			err := pusher.Push("/resource", nil)
			assert.Equal(t, http.ErrNotSupported, err)
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := TimeoutHandler(&log.Logger, fastHandler, 1*time.Second)

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// BenchmarkTimeoutHandler measures overhead of the timeout wrapper
func BenchmarkTimeoutHandler(b *testing.B) {
	fastHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	})

	handler := TimeoutHandler(&log.Logger, fastHandler, 1*time.Second)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/test", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}
