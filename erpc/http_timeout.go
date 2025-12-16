package erpc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"bytes"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
)

func TimeoutHandler(h http.Handler, dt time.Duration) http.Handler {
	return &timeoutHandler{
		handler: h,
		dt:      dt,
	}
}

var ErrHandlerTimeout = errors.New("http request handling timeout")

type timeoutHandler struct {
	handler http.Handler
	dt      time.Duration
}

func (h *timeoutHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancelCtx := context.WithTimeoutCause(r.Context(), h.dt, ErrHandlerTimeout)
	defer func() {
		cancelCtx()
	}()
	r = r.WithContext(ctx)
	done := make(chan struct{})
	tw := &timeoutWriter{
		w:    w,
		h:    make(http.Header),
		req:  r,
		wbuf: util.BorrowBuf(),
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
				log.Error().
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
			log.Debug().Err(err).Msg("context canceled before writing response")
			util.ReturnBuf(tw.wbuf)
			tw.wbuf = nil
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
		_, err := w.Write(tw.wbuf.Bytes())
		util.ReturnBuf(tw.wbuf)
		tw.wbuf = nil
		if err != nil {
			if common.IsClientDisconnect(err) {
				log.Debug().Err(err).Msg("client disconnected while writing response")
			} else {
				log.Warn().Err(err).Msg("failed to write response")
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
			code := http.StatusGatewayTimeout
			// JSON-RPC (POST) should keep transport 200 and return error in body
			if r.Method == http.MethodPost {
				code = http.StatusOK
			}
			w.WriteHeader(code)
			// TODO When other architectures are implemented we should return appropriate structure (currently only evm json-rpc)
			_, err := io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"http request handling timeout"}}`)
			if err != nil {
				log.Error().Err(err).Msg("failed to write error response")
			}
			tw.err = ErrHandlerTimeout
			// Drop any buffered response data to avoid retaining large allocations
			util.ReturnBuf(tw.wbuf)
			tw.wbuf = nil
		default:
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
					log.Error().Err(writeErr).Msg("failed to write error response")
				}
			}
			tw.err = err
			// Drop any buffered response data to avoid retaining large allocations
			util.ReturnBuf(tw.wbuf)
			tw.wbuf = nil
		}
	}
}

type timeoutWriter struct {
	w    http.ResponseWriter
	h    http.Header
	wbuf *bytes.Buffer
	req  *http.Request

	mu          sync.Mutex
	err         error
	wroteHeader bool
	code        int
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

func (tw *timeoutWriter) Write(p []byte) (int, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.err != nil {
		return 0, tw.err
	}
	if !tw.wroteHeader {
		tw.writeHeaderLocked(http.StatusOK)
	}
	return tw.wbuf.Write(p)
}

func (tw *timeoutWriter) writeHeaderLocked(code int) {
	switch {
	case tw.err != nil:
		return
	case tw.wroteHeader:
		if tw.req != nil {
			log.Trace().Msgf("http: superfluous response.WriteHeader call from: %s", string(debug.Stack()))
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
