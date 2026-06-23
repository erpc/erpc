package clients

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ───────────────────────────── Happy-path server ─────────────────────────────

// happyRPCServer implements bds.evm.RPCQueryService with deterministic
// responses for the unary methods. Used to verify the refactored
// SendRequest still happily routes valid requests end-to-end.
type happyRPCServer struct {
	evm.UnimplementedRPCQueryServiceServer

	calls       atomic.Int64
	chainID     uint64
	blockNumber uint64

	// captureMetadata stores the incoming gRPC metadata seen by the most
	// recent call. Used by header-propagation tests.
	mu           sync.Mutex
	lastMetadata metadata.MD
}

func (s *happyRPCServer) recordMetadata(ctx context.Context) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		s.mu.Lock()
		s.lastMetadata = md.Copy()
		s.mu.Unlock()
	}
}

func (s *happyRPCServer) snapshotMetadata() metadata.MD {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastMetadata == nil {
		return metadata.MD{}
	}
	return s.lastMetadata.Copy()
}

func (s *happyRPCServer) ChainId(ctx context.Context, _ *evm.ChainIdRequest) (*evm.ChainIdResponse, error) {
	s.calls.Add(1)
	s.recordMetadata(ctx)
	return &evm.ChainIdResponse{ChainId: s.chainID}, nil
}

func (s *happyRPCServer) GetBlockByNumber(ctx context.Context, req *evm.GetBlockByNumberRequest) (*evm.GetBlockResponse, error) {
	s.calls.Add(1)
	s.recordMetadata(ctx)
	return &evm.GetBlockResponse{
		Block: &evm.BlockHeader{
			Number: s.blockNumber,
			Hash:   []byte{0xde, 0xad, 0xbe, 0xef},
		},
	}, nil
}

func startHappyServer(t *testing.T, chainID, blockNumber uint64) (string, *happyRPCServer, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	happy := &happyRPCServer{chainID: chainID, blockNumber: blockNumber}
	evm.RegisterRPCQueryServiceServer(srv, happy)
	go func() {
		_ = srv.Serve(lis)
	}()
	return lis.Addr().String(), happy, srv.Stop
}

// errorRPCServer returns a configurable gRPC status code on every call.
// Used to verify error propagation through the refactored client.
type errorRPCServer struct {
	evm.UnimplementedRPCQueryServiceServer
	code codes.Code
	msg  string
}

func (s *errorRPCServer) ChainId(_ context.Context, _ *evm.ChainIdRequest) (*evm.ChainIdResponse, error) {
	return nil, status.Error(s.code, s.msg)
}

func startErrorServer(t *testing.T, code codes.Code, msg string) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	evm.RegisterRPCQueryServiceServer(srv, &errorRPCServer{code: code, msg: msg})
	go func() {
		_ = srv.Serve(lis)
	}()
	return lis.Addr().String(), srv.Stop
}

// newTestClient creates a GenericGrpcBdsClient against addr and returns
// it as the concrete type so tests can poke at internals.
func newTestClient(t *testing.T, addr string) *GenericGrpcBdsClient {
	t.Helper()
	parsedURL, err := url.Parse(fmt.Sprintf("grpc://%s", addr))
	require.NoError(t, err)
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client, err := NewGrpcBdsClient(ctx, &logger, "test-project", nil, parsedURL, 0)
	require.NoError(t, err)
	return client.(*GenericGrpcBdsClient)
}

// ───────────────────────────── Happy paths ─────────────────────────────

// TestSendRequest_HappyPath_ChainId verifies eth_chainId routes to the
// gRPC ChainId handler and the response payload is shaped per JSON-RPC.
// Guards against the refactor accidentally rerouting / dropping
// successful responses.
func TestSendRequest_HappyPath_ChainId(t *testing.T) {
	const chainID uint64 = 137 // polygon
	addr, server, stop := startHappyServer(t, chainID, 0)
	defer stop()

	client := newTestClient(t, addr)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))

	resp, err := client.SendRequest(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.GreaterOrEqual(t, server.calls.Load(), int64(1))

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.NotNil(t, jrr)
	// Result is hex-encoded chain id per JSON-RPC convention. 137 → 0x89.
	require.Equal(t, `"0x89"`, jrr.GetResultString())
}

// TestSendRequest_HappyPath_GetBlockByNumber exercises eth_getBlockByNumber
// against a successful server and asserts the block payload is propagated.
func TestSendRequest_HappyPath_GetBlockByNumber(t *testing.T) {
	addr, server, stop := startHappyServer(t, 1, 0x100)
	defer stop()

	client := newTestClient(t, addr)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x100",false]}`))

	resp, err := client.SendRequest(context.Background(), req)
	require.NoError(t, err)
	require.GreaterOrEqual(t, server.calls.Load(), int64(1))

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.NotNil(t, jrr)
	require.NotEqual(t, "null", jrr.GetResultString(),
		"block must be non-null when server returns one")
}

// TestSendRequest_HeadersPassedAsMetadata verifies SetHeaders entries
// become outgoing gRPC metadata. Authentication / routing logic depends
// on this — silently dropping headers would be a P0 production bug.
func TestSendRequest_HeadersPassedAsMetadata(t *testing.T) {
	addr, server, stop := startHappyServer(t, 1, 0)
	defer stop()

	client := newTestClient(t, addr)
	client.SetHeaders(map[string]string{"x-test-header": "deadbeef"})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	_, err := client.SendRequest(context.Background(), req)
	require.NoError(t, err)

	md := server.snapshotMetadata()
	vals := md.Get("x-test-header")
	require.Equal(t, []string{"deadbeef"}, vals,
		"client-set header must reach the server as gRPC metadata")
}

// TestSendRequest_ConfigHeadersReachWireAsMetadata closes the config→wire
// seam: headers configured via grpc.headers on the upstream (the path
// NewGrpcBdsClient wires) must reach the server as gRPC metadata. The other
// tests cover config→headers map and SetHeaders→wire separately; this proves
// the full chain end-to-end, mirroring the HTTP client's header test.
func TestSendRequest_ConfigHeadersReachWireAsMetadata(t *testing.T) {
	addr, server, stop := startHappyServer(t, 1, 0)
	defer stop()

	parsedURL, err := url.Parse(fmt.Sprintf("grpc://%s", addr))
	require.NoError(t, err)
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ups := common.NewFakeUpstream("test-ups", common.WithGrpcConfig(&common.GrpcUpstreamConfig{
		Headers: map[string]string{"authorization": "Bearer secret-token"},
	}))
	client, err := NewGrpcBdsClient(ctx, &logger, "test-project", ups, parsedURL, 0)
	require.NoError(t, err)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	_, err = client.SendRequest(ctx, req)
	require.NoError(t, err)

	md := server.snapshotMetadata()
	require.Equal(t, []string{"Bearer secret-token"}, md.Get("authorization"),
		"grpc.headers configured on the upstream must reach the server as gRPC metadata")
}

// TestSendRequest_ConcurrentRequests_AllSucceed fires many simultaneous
// requests against a healthy server. Asserts no races, no goroutine
// leaks, and every caller gets a response. The pool has shared mutable
// state (cursor, per-conn stuckTimes); a race here would manifest as a
// flaky or failing test under -race.
func TestSendRequest_ConcurrentRequests_AllSucceed(t *testing.T) {
	addr, server, stop := startHappyServer(t, 1, 0)
	defer stop()

	client := newTestClient(t, addr)
	const N = 50
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
			_, errs[i] = client.SendRequest(context.Background(), req)
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		require.NoError(t, e, "request %d failed", i)
	}
	require.GreaterOrEqual(t, server.calls.Load(), int64(N))
}

// ───────────────────────────── Unhappy paths ─────────────────────────────

// TestSendRequest_ServerReturnsGrpcError verifies that gRPC error
// statuses returned by the server are propagated to the caller —
// these are application-level errors (the upstream policy layer
// decides what to do with them).
func TestSendRequest_ServerReturnsGrpcError(t *testing.T) {
	addr, stop := startErrorServer(t, codes.Internal, "synthetic upstream failure")
	defer stop()

	client := newTestClient(t, addr)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))

	_, err := client.SendRequest(context.Background(), req)
	require.Error(t, err)
}

// TestSendRequest_UnsupportedMethod verifies unsupported methods are
// rejected with the right error code BEFORE dialing into the pool.
func TestSendRequest_UnsupportedMethod(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 0)
	defer stop()

	client := newTestClient(t, addr)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_someMadeUpMethod","params":[]}`))
	_, err := client.SendRequest(context.Background(), req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnsupported))
}

// TestSendRequest_AlreadyCancelledCtx_FailsFast verifies the early
// ctx-check returns immediately when the caller's context is already
// done — defends against piling work on a context that's gone.
func TestSendRequest_AlreadyCancelledCtx_FailsFast(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 0)
	defer stop()

	client := newTestClient(t, addr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before SendRequest sees it

	start := time.Now()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	_, err := client.SendRequest(ctx, req)
	elapsed := time.Since(start)
	require.Error(t, err)
	require.Less(t, elapsed, 100*time.Millisecond,
		"already-cancelled ctx must fail-fast; got %v", elapsed)
}

// ───────────────────────────── Pool internals ─────────────────────────────

// TestPool_RoundRobin verifies Pick() rotates deterministically through
// all pool slots. Critical for the "blast radius" guarantee — if Pick
// always returned the same slot, the pool would offer no isolation.
func TestPool_RoundRobin(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 0)
	defer stop()

	client := newTestClient(t, addr)
	p := client.pool
	require.Equal(t, bdsPoolSize, len(p.conns))

	seen := make(map[*bdsConn]int)
	for i := 0; i < len(p.conns)*3; i++ {
		seen[p.Pick()]++
	}
	require.Len(t, seen, len(p.conns), "Pick must rotate through every slot")
	for c, n := range seen {
		require.Equal(t, 3, n, "every slot should be picked exactly 3 times; %p got %d", c, n)
	}
}

// TestPool_RecordStuck_BelowThreshold verifies the rolling-window
// counter doesn't trip until enough events accumulate.
func TestPool_RecordStuck_BelowThreshold(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 0)
	defer stop()

	client := newTestClient(t, addr)
	p := client.pool
	c := p.Pick()

	require.Equal(t, 3, bdsStuckCallThreshold, "test assumes default threshold of 3")
	require.False(t, p.recordStuck(c), "1st stuck call should NOT trip")
	require.False(t, p.recordStuck(c), "2nd stuck call should NOT trip")
	require.True(t, p.recordStuck(c), "3rd stuck call should trip")
}

// TestPool_RecordStuck_WindowEvictsOldEvents verifies events outside the
// rolling window are dropped — preventing slow burns over hours from
// tripping the threshold falsely.
func TestPool_RecordStuck_WindowEvictsOldEvents(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 0)
	defer stop()

	client := newTestClient(t, addr)
	p := client.pool
	c := p.Pick()

	// Two stuck events deep in the past (way outside the 60s window).
	pastEvents := []time.Time{
		time.Now().Add(-10 * time.Minute),
		time.Now().Add(-5 * time.Minute),
	}
	c.stuckMu.Lock()
	c.stuckTimes = append(c.stuckTimes, pastEvents...)
	c.stuckMu.Unlock()

	// First "current" stuck call: window evicts the two old events.
	// Counter should reflect 1, not 3 → should NOT trip.
	require.False(t, p.recordStuck(c),
		"stale stuck events outside the window must be evicted before counting")
}

// TestPool_ReplaceConn_DedupWithin5s verifies replaceConn skips a second
// replacement within bdsReplacementDedupWindow. Prevents thrashing when
// many in-flight callers all hit the threshold simultaneously.
func TestPool_ReplaceConn_DedupWithin5s(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 0)
	defer stop()

	client := newTestClient(t, addr)
	p := client.pool
	c := p.Pick()
	original := c.conn

	p.replaceConn(c)
	first := c.closedAt.Load()
	require.NotZero(t, first, "first replaceConn must record closedAt")

	// Second call to replaceConn(c) within the dedup window: c.closedAt
	// stays unchanged.
	p.replaceConn(c)
	require.Equal(t, first, c.closedAt.Load(),
		"second replaceConn within dedup window must be deduped (no closedAt update)")

	// New conn pointer must differ from original (first replace actually swapped).
	require.NotSame(t, original, p.conns[0].conn,
		"first replaceConn must have actually swapped the conn")
}

// ───────────────────────────── callBounded edge cases ─────────────────────────────

// TestCallBounded_HappyPath verifies the obvious: when fn returns
// normally before ctx fires, callBounded returns fn's result with no
// timeout error.
func TestCallBounded_HappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := callBounded(ctx, func(_ context.Context) error {
		return nil
	})
	require.NoError(t, err)
}

// TestCallBounded_FnError verifies fn errors are propagated unchanged.
func TestCallBounded_FnError(t *testing.T) {
	sentinel := errors.New("fn-failed")
	err := callBounded(context.Background(), func(_ context.Context) error {
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
}

// TestCallBounded_AlreadyExpiredCtx returns immediately without
// burning a goroutine on a dead ctx.
func TestCallBounded_AlreadyExpiredCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := callBounded(ctx, func(c context.Context) error {
		// honor cancellation — return promptly when ctx is done
		select {
		case <-c.Done():
			return c.Err()
		case <-time.After(time.Second):
			return errors.New("fn did not see cancellation")
		}
	})
	elapsed := time.Since(start)
	require.Error(t, err)
	require.Less(t, elapsed, 100*time.Millisecond,
		"callBounded with pre-cancelled ctx must return promptly")
}

// TestCallBounded_PanicInFn verifies the deferred recover() catches a
// panic in fn and surfaces it as an error to the caller — important
// because a panic in the abandoned goroutine would otherwise kill the
// whole process.
func TestCallBounded_PanicInFn(t *testing.T) {
	err := callBounded(context.Background(), func(_ context.Context) error {
		panic("synthetic panic in fn")
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "synthetic panic")
}

// TestCallBoundedT_HappyPath verifies the typed variant carries the
// concrete return value back to the caller.
func TestCallBoundedT_HappyPath(t *testing.T) {
	result, err := callBoundedT(context.Background(), func(_ context.Context) (int, error) {
		return 42, nil
	})
	require.NoError(t, err)
	require.Equal(t, 42, result)
}

// TestCallBounded_NoGoroutineLeakOnSuccess asserts the helper doesn't
// leak when fn returns cleanly. Important because every SendRequest
// uses it.
func TestCallBounded_NoGoroutineLeakOnSuccess(t *testing.T) {
	pre := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		err := callBounded(context.Background(), func(_ context.Context) error {
			return nil
		})
		require.NoError(t, err)
	}
	time.Sleep(20 * time.Millisecond)
	post := runtime.NumGoroutine()
	require.LessOrEqual(t, post, pre+2,
		"100 successful callBounded should leak no goroutines; pre=%d post=%d", pre, post)
}

// ───────────────────────────── Nil-safe helpers ─────────────────────────────

// TestNilClient_QueryClient_DoesNotPanic guards against the
// accessor hitting a nil client (e.g. constructor failed but the
// client got registered somewhere).
func TestNilClient_QueryClient_DoesNotPanic(t *testing.T) {
	var c *GenericGrpcBdsClient
	require.Nil(t, c.QueryClient())
}
