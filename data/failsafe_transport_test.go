package data

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIsTransportError(t *testing.T) {
	u, err := url.Parse("https://example.com/rpc")
	require.NoError(t, err)

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain stringy error", errors.New("something went wrong"), false},
		{"app exception", fmt.Errorf("invalid argument: bad request"), false},
		{"connection refused string", errors.New("dial tcp: connection refused"), true},
		{"connection reset string", errors.New("read: connection reset by peer"), true},
		{"broken pipe string", errors.New("write: broken pipe"), true},
		{"no such host string", errors.New("dial tcp: lookup foo.invalid: no such host"), true},
		{"i/o timeout string", errors.New("read tcp 1.2.3.4:443: i/o timeout"), true},
		{"tls handshake string", errors.New("net/http: TLS handshake timeout"), true},
		{"network unreachable string", errors.New("dial: network is unreachable"), true},
		{"syscall ECONNREFUSED", syscall.ECONNREFUSED, true},
		{"syscall ECONNRESET", syscall.ECONNRESET, true},
		{"syscall EPIPE", syscall.EPIPE, true},
		{"syscall ETIMEDOUT", syscall.ETIMEDOUT, true},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"net.OpError timeout", &net.OpError{Op: "read", Err: timeoutErr{}}, true},
		{"grpc Unavailable", status.Error(codes.Unavailable, "service down"), true},
		{"grpc DeadlineExceeded", status.Error(codes.DeadlineExceeded, "took too long"), true},
		{"grpc Aborted", status.Error(codes.Aborted, "aborted by transport"), true},
		{"grpc InvalidArgument", status.Error(codes.InvalidArgument, "bad params"), false},
		{"grpc PermissionDenied", status.Error(codes.PermissionDenied, "no"), false},
		{"common transport failure", common.NewErrEndpointTransportFailure(u, errors.New("dial fail")), true},
		{"wrapped grpc unavailable", fmt.Errorf("rpc: %w", status.Error(codes.Unavailable, "x")), true},
		{"wrapped io.EOF", fmt.Errorf("read: %w", io.EOF), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isTransportError(tc.err))
		})
	}
}

// timeoutErr satisfies net.Error with Timeout() == true so we can construct
// a synthetic net.OpError without doing real I/O.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "synthetic timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestCacheFailsafe_RetryPolicy_RetriesTypedTransportFailure(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	u, _ := url.Parse("https://example.com/rpc")
	transportErr := common.NewErrEndpointTransportFailure(u, errors.New("dial fail"))
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, transportErr).Times(2)
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte("data"), nil).Once()

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 5,
				Delay:       common.Duration(5 * time.Millisecond),
			},
		},
	}, nil)
	require.NoError(t, err)

	result, err := fc.Get(context.Background(), "", "pk", "rk", nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("data"), result)
	mc.AssertNumberOfCalls(t, "Get", 3)
}

func TestCacheFailsafe_RetryPolicy_RetriesGrpcUnavailable(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, status.Error(codes.Unavailable, "no service")).Times(2)
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte("data"), nil).Once()

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 5,
				Delay:       common.Duration(5 * time.Millisecond),
			},
		},
	}, nil)
	require.NoError(t, err)

	result, err := fc.Get(context.Background(), "", "pk", "rk", nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("data"), result)
	mc.AssertNumberOfCalls(t, "Get", 3)
}

func TestCacheFailsafe_RetryPolicy_RetriesNetTimeout(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	netErr := &net.OpError{Op: "read", Err: timeoutErr{}}
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, netErr).Times(2)
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte("data"), nil).Once()

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 5,
				Delay:       common.Duration(5 * time.Millisecond),
			},
		},
	}, nil)
	require.NoError(t, err)

	result, err := fc.Get(context.Background(), "", "pk", "rk", nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("data"), result)
	mc.AssertNumberOfCalls(t, "Get", 3)
}

func TestCacheFailsafe_RetryPolicy_DoesNotRetryApplicationError(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	appErr := errors.New("malformed response payload")
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, appErr)

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 5,
				Delay:       common.Duration(5 * time.Millisecond),
			},
		},
	}, nil)
	require.NoError(t, err)

	_, err = fc.Get(context.Background(), "", "pk", "rk", nil)
	require.Error(t, err)
	mc.AssertNumberOfCalls(t, "Get", 1)
}

func TestCacheFailsafe_RetryPolicy_DoesNotRetryGrpcInvalidArgument(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, status.Error(codes.InvalidArgument, "bad params"))

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 5,
				Delay:       common.Duration(5 * time.Millisecond),
			},
		},
	}, nil)
	require.NoError(t, err)

	_, err = fc.Get(context.Background(), "", "pk", "rk", nil)
	require.Error(t, err)
	mc.AssertNumberOfCalls(t, "Get", 1)
}

// Sanity check: the existing well-known transient errors (record_not_found,
// record_expired, context cancellation) still don't retry under the narrowed
// predicate.
func TestCacheFailsafe_RetryPolicy_StillExcludesKnownNonRetriable(t *testing.T) {
	logger := zerolog.New(io.Discard)

	cases := []struct {
		name string
		err  error
	}{
		{"record not found", common.NewErrRecordNotFound("pk", "rk", "memory")},
		{"context canceled", context.Canceled},
		{"context deadline exceeded", context.DeadlineExceeded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := NewMockConnector("test")
			mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
				Return(nil, tc.err)

			fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
				{
					Retry: &common.RetryPolicyConfig{
						MaxAttempts: 3,
						Delay:       common.Duration(5 * time.Millisecond),
					},
				},
			}, nil)
			require.NoError(t, err)

			_, _ = fc.Get(context.Background(), "", "pk", "rk", nil)
			mc.AssertNumberOfCalls(t, "Get", 1)
		})
	}
}
