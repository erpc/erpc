package erpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"bytes"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

func TimeoutHandler(logger *zerolog.Logger, h http.Handler, dt time.Duration) http.Handler {
	return &timeoutHandler{
		logger:  logger,
		handler: h,
		dt:      dt,
	}
}

var ErrHandlerTimeout = errors.New("http request handling timeout")

// maxBufferedResponseBytes bounds per-request buffering in the timeout wrapper.
// This handler intentionally buffers to provide deterministic timeout/cancel JSON bodies.
// Without a hard cap, large JSON-RPC results (e.g. eth_getLogs) can OOM the pod.
//
// var (not const) so tests can lower it.
var maxBufferedResponseBytes = 32 << 20 // 32MiB

type timeoutHandler struct {
	logger  *zerolog.Logger
	handler http.Handler
	dt      time.Duration
}

func writeJsonRpcErrorBody(w http.ResponseWriter, msg string, code int) {
	// Minimal JSON-RPC error response; id is unknown here.
	_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":`)
	_, _ = io.WriteString(w, fmt.Sprint(code))
	_, _ = io.WriteString(w, `,"message":"`)
	_, _ = io.WriteString(w, msg)
	_, _ = io.WriteString(w, `"}}`)
}

func (h *timeoutHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctxTimeout, cancelTimeout := context.WithTimeoutCause(r.Context(), h.dt, ErrHandlerTimeout)
	ctx, cancelCause := context.WithCancelCause(ctxTimeout)
	defer func() {
		cancelTimeout()
	}()
	r = r.WithContext(ctx)
	done := make(chan struct{})
	tw := &timeoutWriter{
		logger: h.logger,
		w:      w,
		h:      make(http.Header),
		req:    r,
		wbuf:   util.BorrowBuf(),
		cancel: cancelCause,
	}
	panicChan := make(chan any, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
					"timeout-handler",
					"",
					common.ErrorFingerprint(p),
				).Inc()
				h.logger.Error().
					Interface("panic", p).
					Str("stack", string(debug.Stack())).
					Msgf("unexpected panic on timeout handler")
				panicChan <- p
			}
		}()
		h.handler.ServeHTTP(tw, r)
		close(done)
	}()
	select {
	case p := <-panicChan:
		panic(p)
	case <-done:
		tw.mu.Lock()
		defer tw.mu.Unlock()
		if err := ctx.Err(); err != nil {
			h.logger.Debug().Err(err).Msg("context canceled before writing response")
			if tw.wbuf != nil {
				util.ReturnBuf(tw.wbuf)
				tw.wbuf = nil
			}
			return
		}
		// If we've already switched to streaming mode, the handler has written
		// directly to the underlying writer; don't write again.
		if tw.passthrough {
			if tw.wbuf != nil {
				util.ReturnBuf(tw.wbuf)
				tw.wbuf = nil
			}
			return
		}
		dst := w.Header()
		for k, vv := range tw.h {
			dst[k] = vv
		}
		if !tw.wroteHeader {
			tw.code = http.StatusOK
		}
		w.WriteHeader(tw.code)
		var err error
		if tw.wbuf != nil {
			_, err = w.Write(tw.wbuf.Bytes())
			util.ReturnBuf(tw.wbuf)
			tw.wbuf = nil
		}
		if err != nil {
			if common.IsClientDisconnect(err) {
				h.logger.Debug().Err(err).Msg("client disconnected while writing response")
			} else {
				h.logger.Warn().Err(err).Msg("failed to write response")
			}
		}
	case <-ctx.Done():
		tw.mu.Lock()
		defer tw.mu.Unlock()
		err := context.Cause(ctx)
		if err == nil {
			err = ctx.Err()
		}
		switch err {
		case context.DeadlineExceeded, ErrHandlerTimeout:
			if tw.passthrough {
				tw.err = ErrHandlerTimeout
				if tw.wbuf != nil {
					util.ReturnBuf(tw.wbuf)
					tw.wbuf = nil
				}
				return
			}
			code := http.StatusGatewayTimeout
			// JSON-RPC (POST) should keep transport 200 and return error in body
			if r.Method == http.MethodPost {
				code = http.StatusOK
			}
			w.WriteHeader(code)
			// TODO When other architectures are implemented we should return appropriate structure (currently only evm json-rpc)
			_, err := io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"http request handling timeout"}}`)
			if err != nil {
				h.logger.Error().Err(err).Msg("failed to write error response")
			}
			tw.err = ErrHandlerTimeout
			// Drop any buffered response data to avoid retaining large allocations
			if tw.wbuf != nil {
				util.ReturnBuf(tw.wbuf)
				tw.wbuf = nil
			}
		default:
			if tw.passthrough {
				tw.err = err
				if tw.wbuf != nil {
					util.ReturnBuf(tw.wbuf)
					tw.wbuf = nil
				}
				return
			}
			code := http.StatusServiceUnavailable
			// JSON-RPC (POST) should keep transport 200 and return error in body
			if r.Method == http.MethodPost {
				code = http.StatusOK
			}
			w.WriteHeader(code)
			// Write JSON-RPC error body for POST requests (same as timeout case)
			if r.Method == http.MethodPost {
				_, writeErr := io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"request cancelled by client"}}`)
				if writeErr != nil {
					h.logger.Error().Err(writeErr).Msg("failed to write error response")
				}
			}
			tw.err = err
			// Drop any buffered response data to avoid retaining large allocations
			if tw.wbuf != nil {
				util.ReturnBuf(tw.wbuf)
				tw.wbuf = nil
			}
		}
	}
}

type timeoutWriter struct {
	logger *zerolog.Logger
	w      http.ResponseWriter
	h      http.Header
	wbuf   *bytes.Buffer
	req    *http.Request
	cancel context.CancelCauseFunc

	mu          sync.Mutex
	err         error
	wroteHeader bool
	code        int
	passthrough bool

	// headerFlushed indicates we've copied headers and status to the underlying writer.
	// Needed when switching to passthrough to avoid double WriteHeader/body writes.
	headerFlushed bool
}

var _ http.Pusher = (*timeoutWriter)(nil)

// Push implements the [Pusher] interface.
func (tw *timeoutWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := tw.w.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (tw *timeoutWriter) Header() http.Header { return tw.h }

func (tw *timeoutWriter) flushHeaderLocked() {
	if tw.headerFlushed {
		return
	}
	dst := tw.w.Header()
	for k, vv := range tw.h {
		dst[k] = vv
	}
	if !tw.wroteHeader {
		tw.code = http.StatusOK
	}
	tw.w.WriteHeader(tw.code)
	tw.headerFlushed = true
}

func (tw *timeoutWriter) Write(p []byte) (int, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.err != nil {
		return 0, tw.err
	}
	if !tw.wroteHeader {
		tw.writeHeaderLocked(http.StatusOK)
	}

	if tw.passthrough {
		tw.flushHeaderLocked()
		n, err := tw.w.Write(p)
		if err != nil {
			tw.err = err
		}
		return n, err
	}

	if tw.wbuf == nil {
		// Defensive: no buffer available; stream directly.
		tw.passthrough = true
		tw.flushHeaderLocked()
		n, err := tw.w.Write(p)
		if err != nil {
			tw.err = err
		}
		return n, err
	}

	if maxBufferedResponseBytes > 0 && tw.wbuf.Len()+len(p) > maxBufferedResponseBytes {
		// Switch to passthrough to avoid unbounded buffering (OOM risk), while
		// preserving the ability to serve large responses.
		tw.passthrough = true
		tw.flushHeaderLocked()

		if tw.wbuf.Len() > 0 {
			if _, err := tw.w.Write(tw.wbuf.Bytes()); err != nil {
				tw.err = err
				util.ReturnBuf(tw.wbuf)
				tw.wbuf = nil
				return 0, err
			}
		}
		util.ReturnBuf(tw.wbuf)
		tw.wbuf = nil

		n, err := tw.w.Write(p)
		if err != nil {
			tw.err = err
		}
		return n, err
	}
	return tw.wbuf.Write(p)
}

func (tw *timeoutWriter) writeHeaderLocked(code int) {
	switch {
	case tw.err != nil:
		return
	case tw.wroteHeader:
		if tw.req != nil {
			tw.logger.Trace().Msgf("http: superfluous response.WriteHeader call from: %s", string(debug.Stack()))
		}
	default:
		tw.wroteHeader = true
		tw.code = code
	}
}

func (tw *timeoutWriter) WriteHeader(code int) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	tw.writeHeaderLocked(code)
}
