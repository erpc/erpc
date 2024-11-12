package erpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

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
		w:   w,
		h:   make(http.Header),
		req: r,
	}
	panicChan := make(chan any, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
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
		dst := w.Header()
		for k, vv := range tw.h {
			dst[k] = vv
		}
		if !tw.wroteHeader {
			tw.code = http.StatusOK
		}
		w.WriteHeader(tw.code)
		w.Write(tw.wbuf.Bytes())
	case <-ctx.Done():
		tw.mu.Lock()
		defer tw.mu.Unlock()
		err := context.Cause(ctx)
		if err == nil {
			err = ctx.Err()
		}
		switch err {
		case context.DeadlineExceeded, ErrHandlerTimeout:
			w.WriteHeader(http.StatusGatewayTimeout)
			// TODO When other architectures are implemented we should return appropriate structure (currently only evm json-rpc)
			io.WriteString(w, `{"jsonrpc":"2.0","error":{"code":-32603,"message":"http request handling timeout"}}`)
			tw.err = ErrHandlerTimeout
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			tw.err = err
		}
	}
}

type timeoutWriter struct {
	w    http.ResponseWriter
	h    http.Header
	wbuf bytes.Buffer
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
			caller := relevantCaller()
			log.Trace().Msgf("http: superfluous response.WriteHeader call from %s (%s:%d)", caller.Function, path.Base(caller.File), caller.Line)
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

func relevantCaller() runtime.Frame {
	pc := make([]uintptr, 16)
	n := runtime.Callers(1, pc)
	frames := runtime.CallersFrames(pc[:n])
	var frame runtime.Frame
	for {
		frame, more := frames.Next()
		if !strings.HasPrefix(frame.Function, "net/http.") {
			return frame
		}
		if !more {
			break
		}
	}
	return frame
}
