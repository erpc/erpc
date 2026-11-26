package common

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/rs/zerolog"
)

// covJsonrpcEnableLogs lifts the package-wide disabled log level (set by the
// test init) for the duration of one test so log output can be asserted.
func covJsonrpcEnableLogs(t interface{ Cleanup(func()) }) {
	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })
}

// covJsonrpcNetwork is a minimal Network for request/response tests: it only
// answers identity, config, logger and finality.
type covJsonrpcNetwork struct {
	Network
	id            string
	label         string
	cfg           *NetworkConfig
	finality      DataFinalityState
	finalityCalls int
	logger        zerolog.Logger
}

func (n *covJsonrpcNetwork) Id() string              { return n.id }
func (n *covJsonrpcNetwork) Label() string           { return n.label }
func (n *covJsonrpcNetwork) Config() *NetworkConfig  { return n.cfg }
func (n *covJsonrpcNetwork) Logger() *zerolog.Logger { return &n.logger }
func (n *covJsonrpcNetwork) GetFinality(_ context.Context, _ *NormalizedRequest, _ *NormalizedResponse) DataFinalityState {
	n.finalityCalls++
	return n.finality
}

// covJsonrpcUpstream is an Upstream whose Config may be nil, to exercise the
// "upstream without config" branches.
type covJsonrpcUpstream struct {
	Upstream
	id  string
	cfg *UpstreamConfig
}

func (u *covJsonrpcUpstream) Id() string              { return u.id }
func (u *covJsonrpcUpstream) Config() *UpstreamConfig { return u.cfg }

// covJsonrpcByteWriter is a util.ByteWriter (and ReleasableByteWriter) backed by
// a fixed payload, with optional injected errors.
type covJsonrpcByteWriter struct {
	data      []byte
	writeErr  error
	sizeErr   error
	released  bool
	emptyish  bool
	writeCall int
}

func (w *covJsonrpcByteWriter) WriteTo(out io.Writer, trimSides bool) (int64, error) {
	w.writeCall++
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	b := w.data
	if trimSides && len(b) >= 2 {
		b = b[1 : len(b)-1]
	}
	n, err := out.Write(b)
	return int64(n), err
}
func (w *covJsonrpcByteWriter) IsResultEmptyish() bool { return w.emptyish }
func (w *covJsonrpcByteWriter) Size(_ ...context.Context) (int, error) {
	if w.sizeErr != nil {
		return 0, w.sizeErr
	}
	return len(w.data), nil
}
func (w *covJsonrpcByteWriter) Release() { w.released = true }

// covJsonrpcFailWriter fails the Nth Write call (1-based) and accepts the rest.
type covJsonrpcFailWriter struct {
	failAt int
	calls  int
	buf    bytes.Buffer
}

var errCovJsonrpcWrite = errors.New("cov-jsonrpc write failed")

func (w *covJsonrpcFailWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.failAt {
		return 0, errCovJsonrpcWrite
	}
	return w.buf.Write(p)
}

// covJsonrpcBody is a ReadCloser with a controllable Close error.
type covJsonrpcBody struct {
	io.Reader
	closeErr error
	closed   int
}

func (b *covJsonrpcBody) Close() error {
	b.closed++
	return b.closeErr
}

// covJsonrpcErrReader always fails to read.
type covJsonrpcErrReader struct{}

func (covJsonrpcErrReader) Read([]byte) (int, error) { return 0, errors.New("cov-jsonrpc read failed") }
