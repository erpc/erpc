package clients

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// wedgedRPCServer implements the bds.evm.RPCQueryService but every method
// blocks until ctx is cancelled — simulating the failure mode where a
// gRPC stream sits at the H2 layer with no response forthcoming.
//
// `started` and `finished` counters let the test assert that callers
// actually entered the server-side handler and that they don't block
// it indefinitely.
type wedgedRPCServer struct {
	evm.UnimplementedRPCQueryServiceServer
	started  atomic.Int64
	finished atomic.Int64
}

func (s *wedgedRPCServer) blockForever(ctx context.Context) error {
	s.started.Add(1)
	defer s.finished.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

func (s *wedgedRPCServer) GetBlockByNumber(ctx context.Context, _ *evm.GetBlockByNumberRequest) (*evm.GetBlockResponse, error) {
	return nil, s.blockForever(ctx)
}

func (s *wedgedRPCServer) ChainId(ctx context.Context, _ *evm.ChainIdRequest) (*evm.ChainIdResponse, error) {
	return nil, s.blockForever(ctx)
}

// startWedgedServer spins up a real gRPC server on a random port that
// implements the BDS RPC service but never returns. Returns the address
// and a cleanup func.
func startWedgedServer(t *testing.T) (string, *wedgedRPCServer, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := grpc.NewServer()
	wedged := &wedgedRPCServer{}
	evm.RegisterRPCQueryServiceServer(srv, wedged)
	go func() {
		_ = srv.Serve(lis)
	}()
	cleanup := func() {
		srv.Stop()
	}
	return lis.Addr().String(), wedged, cleanup
}

// TestGrpcBdsClient_HardTimeoutFreesGoroutineWithinCap is the
// regression test for the wedged-H2-stream failure mode. Without the
// bounded-wait defense, callers that hit a stuck stream sit for the
// full caller-supplied deadline (or longer if grpc-go's cancellation
// doesn't reach the stream). After the fix, the SendRequest caller
// returns within bdsHardCallTimeout regardless of what's happening at
// the H2 layer.
//
// Asserts:
//   - SendRequest returns within bdsHardCallTimeout + 2s
//   - The error is a request-timeout (DeadlineExceeded-equivalent)
//   - The server-side handler eventually unblocks (proves cancellation
//     does propagate when grpc-go is behaving normally)
func TestGrpcBdsClient_HardTimeoutFreesGoroutineWithinCap(t *testing.T) {
	addr, wedged, stop := startWedgedServer(t)
	defer stop()

	parsedURL, err := url.Parse(fmt.Sprintf("grpc://%s", addr))
	require.NoError(t, err)

	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := NewGrpcBdsClient(ctx, &logger, "test-project", nil, parsedURL)
	require.NoError(t, err)

	// Use a tight caller-supplied deadline so the test runs fast — the
	// bounded-wait kicks in at min(caller-deadline, bdsHardCallTimeout),
	// so the caller's 500ms is what fires here.
	callerDeadline := 500 * time.Millisecond
	callCtx, cancelCall := context.WithTimeout(context.Background(), callerDeadline)
	defer cancelCall()

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))

	start := time.Now()
	_, err = client.SendRequest(callCtx, req)
	elapsed := time.Since(start)

	require.Error(t, err, "expected timeout error when server wedges")
	require.Less(t, elapsed, callerDeadline+2*time.Second,
		"SendRequest must return within deadline+grace; got %v (deadline=%v)", elapsed, callerDeadline)

	// Sanity-check we actually reached the server (i.e. the test exercised
	// the wedged-handler path, not a connect-time failure).
	require.GreaterOrEqual(t, wedged.started.Load(), int64(1),
		"server handler should have been entered")

	// The grpc-go cancel-path should eventually wake the handler when
	// the underlying stream is closed (here by callBounded abandoning
	// the goroutine + ctx fire). Wait up to 2s for the handler to exit.
	require.Eventually(t, func() bool {
		return wedged.finished.Load() >= 1
	}, 2*time.Second, 20*time.Millisecond,
		"server handler should be unblocked after the client bounded-wait fires")
}

// TestGrpcBdsClient_WatchdogReplacesWedgedConn verifies that after
// enough stuck calls accumulate on a single connection, the pool
// force-closes it and dials a replacement.
//
// Drives our OWN hardCap (bdsHardCallTimeout) rather than a caller-
// supplied deadline — the watchdog now only fires when WE choose to
// abandon (cause == ErrDynamicTimeoutExceeded), not when a parent
// ctx fires (which is a normal caller-side timeout, not a wedge).
func TestGrpcBdsClient_WatchdogReplacesWedgedConn(t *testing.T) {
	// Temporarily shrink the hard cap for this test — otherwise we'd
	// wait 20s per stuck call. Restored on cleanup so it doesn't bleed
	// into sibling tests.
	origHardCap := bdsHardCallTimeout
	bdsHardCallTimeout = 250 * time.Millisecond
	t.Cleanup(func() { bdsHardCallTimeout = origHardCap })

	addr, _, stop := startWedgedServer(t)
	defer stop()

	parsedURL, err := url.Parse(fmt.Sprintf("grpc://%s", addr))
	require.NoError(t, err)

	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := NewGrpcBdsClient(ctx, &logger, "test-project", nil, parsedURL)
	require.NoError(t, err)
	gen := client.(*GenericGrpcBdsClient)

	// Pin to a single pool slot for determinism (single-threaded mutation).
	gen.pool.conns = gen.pool.conns[:1]

	originalConn := gen.pool.Pick().conn
	require.NotNil(t, originalConn)

	// Use context.Background() so OUR hardCap (not a caller deadline)
	// is the proximate cause of cancellation. Each call wedges for
	// 250ms (our cap), then OnBoundedTimeout records a stuck call.
	for i := 0; i < bdsStuckCallThreshold+1; i++ {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
		_, _ = client.SendRequest(context.Background(), req)
	}

	require.Eventually(t, func() bool {
		current := gen.pool.Pick().conn
		return current != originalConn
	}, 2*time.Second, 20*time.Millisecond,
		"pool slot should have been replaced after threshold stuck calls")
}

// TestGrpcBdsClient_WatchdogIgnoresCallerDeadline verifies the second
// half of the cause-distinguishing fix: when the CALLER's parent
// context fires (not our hardCap), the watchdog must NOT trigger.
// Otherwise routine slow-path caller timeouts would churn the pool.
func TestGrpcBdsClient_WatchdogIgnoresCallerDeadline(t *testing.T) {
	addr, _, stop := startWedgedServer(t)
	defer stop()

	parsedURL, err := url.Parse(fmt.Sprintf("grpc://%s", addr))
	require.NoError(t, err)

	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := NewGrpcBdsClient(ctx, &logger, "test-project", nil, parsedURL)
	require.NoError(t, err)
	gen := client.(*GenericGrpcBdsClient)
	gen.pool.conns = gen.pool.conns[:1]
	originalConn := gen.pool.Pick().conn

	// Caller-supplied 250ms deadline. bdsHardCallTimeout is 20s here,
	// so the caller's deadline fires first. The watchdog must NOT
	// trigger because the cause is the caller's, not ours.
	for i := 0; i < bdsStuckCallThreshold+2; i++ {
		callCtx, cancelCall := context.WithTimeout(context.Background(), 250*time.Millisecond)
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
		_, _ = client.SendRequest(callCtx, req)
		cancelCall()
	}

	// Give the watchdog a chance to (wrongly) fire if it's going to.
	time.Sleep(200 * time.Millisecond)
	current := gen.pool.Pick().conn
	require.Same(t, originalConn, current,
		"caller-deadline timeouts must NOT trigger conn replacement")
}

// TestCallBoundedReturnsOnCtxCancel proves the bounded-wait primitive
// returns within the caller-supplied deadline even when the underlying
// function never returns. This is the foundation under
// HardTimeoutFreesGoroutineWithinCap.
func TestCallBoundedReturnsOnCtxCancel(t *testing.T) {
	preCount := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := callBounded(ctx, func(ctx context.Context) error {
		<-ctx.Done() // honor cancellation cleanly
		return ctx.Err()
	})
	elapsed := time.Since(start)

	require.True(t, errors.Is(err, context.DeadlineExceeded))
	require.Less(t, elapsed, 200*time.Millisecond)

	// Give the goroutine a moment to clean up.
	time.Sleep(50 * time.Millisecond)
	require.LessOrEqual(t, runtime.NumGoroutine(), preCount+1,
		"callBounded must not leak goroutines on clean ctx-cancel")
}

// TestCallBoundedAbandonsStuckFunc proves the bounded-wait pattern
// returns even when the wrapped function REFUSES to honor cancellation
// (the wedged-stream case). The goroutine "leaks" — that's the trade —
// but the caller is freed within ctx's deadline.
func TestCallBoundedAbandonsStuckFunc(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // cleanup the parked goroutine when test ends

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := callBounded(ctx, func(_ context.Context) error {
		<-release // ignores ctx entirely — simulates wedged stream
		return nil
	})
	elapsed := time.Since(start)

	require.True(t, errors.Is(err, context.DeadlineExceeded),
		"caller must see DeadlineExceeded even when fn ignores ctx; got %v", err)
	require.Less(t, elapsed, 200*time.Millisecond,
		"caller must return within ctx's deadline regardless of fn; got %v", elapsed)
}
