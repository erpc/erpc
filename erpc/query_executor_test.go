package erpc

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	bdscommon "github.com/blockchain-data-standards/manifesto/common"
	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	upstreampkg "github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Native QueryService upstreams through the executor, for both transports:
// a JSON-RPC eth_query* request through Network.Forward gets one page of the
// first native upstream that answers, and a gRPC stream gets every page and
// never switches upstream once a page went out.
func TestQueryExecutor_Native(t *testing.T) {
	page := func(n uint64, cursor uint64) *evm.QueryLogsResponse {
		ref := func(b uint64) *evm.CursorBlock {
			return &evm.CursorBlock{Number: b, Hash: make([]byte, 32), ParentHash: make([]byte, 32)}
		}
		return &evm.QueryLogsResponse{
			Logs:        []*evm.Log{{BlockNumber: n, LogIndex: 0, Address: make([]byte, 20), BlockHash: make([]byte, 32), TransactionHash: make([]byte, 32)}},
			FromBlock:   ref(1),
			ToBlock:     ref(9),
			CursorBlock: ref(cursor),
		}
	}
	var seen []*evm.QueryLogsRequest
	unimplemented := newTestQueryUpstream(t, "native-old", &fakeGrpcBdsClient{queryClient: &fakeQueryServiceClient{
		queryLogsFn: func(ctx context.Context, in *evm.QueryLogsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryLogsResponse], error) {
			return &fakeServerStreamingClient[evm.QueryLogsResponse]{finalErr: status.Error(codes.Unimplemented, "QueryLogs")}, nil
		},
	}})
	serving := newTestQueryUpstream(t, "native", &fakeGrpcBdsClient{queryClient: &fakeQueryServiceClient{
		queryLogsFn: func(ctx context.Context, in *evm.QueryLogsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryLogsResponse], error) {
			seen = append(seen, in)
			return &fakeServerStreamingClient[evm.QueryLogsResponse]{
				responses: []*evm.QueryLogsResponse{page(1, 4), page(5, 9)},
				finalErr:  errors.New("stream broke after two pages"),
			}, nil
		},
	}})
	qe := newTestQueryExecutor(t, "eth_queryLogs", unimplemented, serving)

	t.Run("JSON-RPC gets one page, parsed and rendered by the MIP-16 codec", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":7,"method":"eth_queryLogs","params":[{"fromBlock":"0x1","toBlock":"0x9","target":"0x1","fields":{"logs":["blockNumber"]}}]}`))
		resp, err := qe.network.forwardQuery(context.Background(), req, "eth_queryLogs")
		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		var result map[string]interface{}
		require.NoError(t, sonic.Unmarshal(jrr.GetResultBytes(), &result))
		assert.Equal(t, map[string]interface{}{"logs": []interface{}{map[string]interface{}{"blockNumber": "0x1"}}}, result["data"])
		assert.Equal(t, "0x4", result["cursorBlock"].(map[string]interface{})["number"])

		require.Len(t, seen, 1)
		assert.Equal(t, uint32(1), seen[0].GetTarget(), "the native upstream gets the MIP-16 request unchanged")
		assert.Equal(t, uint64(1), seen[0].GetChainId())
	})

	t.Run("gRPC gets every page and no other upstream after a page", func(t *testing.T) {
		var cursors []uint64
		err := qe.Execute(context.Background(), &evm.QueryLogsRequest{FromBlock: util.StringPtr("0x1"), ToBlock: util.StringPtr("0x9")}, func(p proto.Message) error {
			cursors = append(cursors, p.(*evm.QueryLogsResponse).GetCursorBlock().GetNumber())
			return nil
		})
		require.ErrorContains(t, err, "stream broke after two pages")
		assert.Equal(t, []uint64{4, 9}, cursors)
	})

	t.Run("errors carry the MIP-16 codes on both transports", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":8,"method":"eth_queryBlocks","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`))
		_, err := qe.network.forwardQuery(context.Background(), req, "eth_queryBlocks")
		var jre *common.ErrJsonRpcExceptionInternal
		require.ErrorAs(t, err, &jre)
		assert.Equal(t, common.JsonRpcErrorNumber(-32004), jre.NormalizedCode(), "no native QueryBlocks and no shim")

		err = qe.Execute(context.Background(), &evm.QueryBlocksRequest{FromBlock: util.StringPtr("0x1"), ToBlock: util.StringPtr("0x2")}, func(proto.Message) error { return nil })
		assert.Equal(t, codes.Unimplemented, status.Code(queryGrpcError(err)))

		order := evm.SortOrder_DESC
		err = qe.Execute(context.Background(), &evm.QueryLogsRequest{FromBlock: util.StringPtr("0x1"), ToBlock: util.StringPtr("0x9"), Order: &order}, func(proto.Message) error { return nil })
		assert.Equal(t, codes.InvalidArgument, status.Code(queryGrpcError(err)), "inverted desc range")

		// A native upstream's MIP-16 failure (pipeThrough wraps it in
		// StreamError) keeps its code on both transports when no shim can
		// take over.
		outOfRange := newTestQueryUpstream(t, "native-window", &fakeGrpcBdsClient{queryClient: &fakeQueryServiceClient{
			queryTracesFn: func(ctx context.Context, in *evm.QueryTracesRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTracesResponse], error) {
				return &fakeServerStreamingClient[evm.QueryTracesResponse]{finalErr: status.Error(codes.OutOfRange, "traces pruned")}, nil
			},
		}})
		windowed := newTestQueryExecutor(t, "eth_queryTraces", outOfRange)
		err = windowed.Execute(context.Background(), &evm.QueryTracesRequest{FromBlock: util.StringPtr("0x1"), ToBlock: util.StringPtr("0x2")}, func(proto.Message) error { return nil })
		assert.Equal(t, codes.OutOfRange, status.Code(queryGrpcError(err)))
		req = common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":9,"method":"eth_queryTraces","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`))
		_, err = windowed.network.forwardQuery(context.Background(), req, "eth_queryTraces")
		require.ErrorAs(t, err, &jre)
		assert.Equal(t, common.JsonRpcErrorNumber(-32001), jre.NormalizedCode())
	})

	t.Run("only a budget fromBlock alone exceeds is MIP-16 -32005", func(t *testing.T) {
		// -32005 makes a client stop paging. A slow or rate-limited native
		// upstream is transient: a retry reads the same range.
		for _, tc := range []struct {
			name     string
			failure  error
			jsonCode int
			grpcCode codes.Code
		}{
			{"deadline", status.Error(codes.DeadlineExceeded, "slow read"), -32603, codes.DeadlineExceeded},
			{"BDS timeout", bdscommon.NewError(bdscommon.ErrorCode_TIMEOUT_ERROR, "slow read").ToGRPCStatus().Err(), -32603, codes.DeadlineExceeded},
			{"BDS rate limit", bdscommon.NewError(bdscommon.ErrorCode_RATE_LIMITED, "slow down").ToGRPCStatus().Err(), -32603, codes.Unavailable},
			{"budget", status.Error(codes.ResourceExhausted, "block 0x1 alone exceeds the page budget"), -32005, codes.ResourceExhausted},
			{"BDS budget", bdscommon.NewError(bdscommon.ErrorCode_RANGE_TOO_LARGE, "block 0x1 alone exceeds the page budget").ToGRPCStatus().Err(), -32005, codes.ResourceExhausted},
		} {
			failing := newTestQueryUpstream(t, "native-failing", &fakeGrpcBdsClient{queryClient: &fakeQueryServiceClient{
				queryLogsFn: func(ctx context.Context, in *evm.QueryLogsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryLogsResponse], error) {
					return &fakeServerStreamingClient[evm.QueryLogsResponse]{finalErr: tc.failure}, nil
				},
			}})
			qe := newTestQueryExecutor(t, "eth_queryLogs", failing)
			err := qe.Execute(context.Background(), &evm.QueryLogsRequest{FromBlock: util.StringPtr("0x1"), ToBlock: util.StringPtr("0x9")}, func(proto.Message) error { return nil })
			assert.Equal(t, tc.grpcCode, status.Code(queryGrpcError(err)), tc.name)
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":10,"method":"eth_queryLogs","params":[{"fromBlock":"0x1","toBlock":"0x9"}]}`))
			_, err = qe.network.forwardQuery(context.Background(), req, "eth_queryLogs")
			var jre *common.ErrJsonRpcExceptionInternal
			require.ErrorAs(t, err, &jre, tc.name)
			assert.Equal(t, common.JsonRpcErrorNumber(tc.jsonCode), jre.NormalizedCode(), tc.name)
		}
	})

	t.Run("a real BDS client sends the upstream's grpc.headers on the stream", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		server := grpc.NewServer()
		qs := &headerRecordingQueryServer{}
		evm.RegisterQueryServiceServer(server, qs)
		go func() { _ = server.Serve(lis) }()
		defer server.Stop()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		logger := zerolog.Nop()
		endpoint, err := url.Parse("grpc://" + lis.Addr().String())
		require.NoError(t, err)
		client, err := clients.NewGrpcBdsClient(ctx, &logger, "prjA", nil, endpoint, 0, "")
		require.NoError(t, err)
		defer client.(clients.ShutdownableClient).Shutdown()
		client.SetHeaders(map[string]string{"x-api-key": "demo"})

		native := newTestQueryExecutor(t, "eth_queryLogs", newTestQueryUpstream(t, "native-real", client))
		var pages int
		err = native.Execute(ctx, &evm.QueryLogsRequest{FromBlock: util.StringPtr("0x1"), ToBlock: util.StringPtr("0x9")}, func(proto.Message) error {
			pages++
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 1, pages)
		require.NotNil(t, qs.apiKey.Load(), "the QueryService server got no call")
		assert.Equal(t, []string{"demo"}, *qs.apiKey.Load(), "x-api-key must reach the QueryService server")
	})

	t.Run("a down or silent native upstream yields before its first page, and selection learns it", func(t *testing.T) {
		logger := zerolog.Nop()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		realUpstream := func(id string, qs evm.QueryServiceServer) (*upstreampkg.Upstream, *grpc.Server) {
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			server := grpc.NewServer()
			evm.RegisterQueryServiceServer(server, qs)
			go func() { _ = server.Serve(lis) }()
			t.Cleanup(server.Stop)
			endpoint, err := url.Parse("grpc://" + lis.Addr().String())
			require.NoError(t, err)
			client, err := clients.NewGrpcBdsClient(ctx, &logger, "prjA", nil, endpoint, 0, "")
			require.NoError(t, err)
			t.Cleanup(client.(clients.ShutdownableClient).Shutdown)
			return newTestQueryUpstream(t, id, client), server
		}
		// down: its server stopped after the client connected. With
		// waitForReady the stream would queue for a reconnect that never comes.
		down, downServer := realUpstream("native-down", &headerRecordingQueryServer{})
		downServer.Stop()
		// No first-page deadline here: only fail-fast ends the wait early.
		waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
		defer waitCancel()
		start := time.Now()
		err := newTestQueryExecutor(t, "eth_queryLogs", down).Execute(waitCtx, &evm.QueryLogsRequest{FromBlock: util.StringPtr("0x1"), ToBlock: util.StringPtr("0x9")}, func(proto.Message) error { return nil })
		assert.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
		assert.Less(t, time.Since(start), 2*time.Second, "a down upstream fails at once, not at the caller's deadline")
		// silent: accepts the stream and never sends a page.
		silent, _ := realUpstream("native-silent", &silentQueryServer{})
		serving := newTestQueryUpstream(t, "native-serving", &fakeGrpcBdsClient{queryClient: &fakeQueryServiceClient{
			queryLogsFn: func(ctx context.Context, in *evm.QueryLogsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryLogsResponse], error) {
				return &fakeServerStreamingClient[evm.QueryLogsResponse]{responses: []*evm.QueryLogsResponse{page(1, 9)}}, nil
			},
		}})
		qe := newTestQueryExecutor(t, "eth_queryLogs", down, silent, serving)
		fe, err := NewNetworkExecutor(&common.FailsafeConfig{
			MatchMethod: "eth_query*",
			Timeout:     &common.TimeoutPolicyConfig{Duration: common.NewStaticDuration(300 * time.Millisecond)},
		}, &logger, nil, nil)
		require.NoError(t, err)
		qe.network.failsafeExecutors = []*networkExecutor{fe}

		reqCtx, reqCancel := context.WithTimeout(ctx, 10*time.Second)
		defer reqCancel()
		start = time.Now()
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":10,"method":"eth_queryLogs","params":[{"fromBlock":"0x1","toBlock":"0x9"}]}`))
		resp, err := qe.network.forwardQuery(reqCtx, req, "eth_queryLogs")
		require.NoError(t, err)
		assert.Less(t, time.Since(start), 3*time.Second, "the down upstream fails at once and the silent one at the 300ms first-page deadline")
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		var result map[string]interface{}
		require.NoError(t, sonic.Unmarshal(jrr.GetResultBytes(), &result))
		assert.Equal(t, "0x9", result["cursorBlock"].(map[string]interface{})["number"], "the serving upstream's page")

		metrics := func(u *upstreampkg.Upstream) *health.TrackedMetrics {
			return u.MetricsTracker().GetUpstreamMethodMetrics(u, "eth_queryLogs", common.DataFinalityStateAll)
		}
		for _, u := range []*upstreampkg.Upstream{down, silent} {
			assert.EqualValues(t, 1, metrics(u).RequestsTotal.Load(), u.Id())
			assert.EqualValues(t, 1, metrics(u).ErrorsTotal.Load(), "%s failed before its first page", u.Id())
		}
		assert.EqualValues(t, 1, metrics(serving).RequestsTotal.Load())
		assert.EqualValues(t, 0, metrics(serving).ErrorsTotal.Load())
		assert.Positive(t, metrics(serving).ResponseQuantiles.GetQuantile(0.5), "time to the first page feeds the latency quantile")
	})
}

// headerRecordingQueryServer is a QueryService server that records the
// x-api-key metadata of a QueryLogs call and answers one page.
type headerRecordingQueryServer struct {
	evm.UnimplementedQueryServiceServer
	apiKey atomic.Pointer[[]string]
}

func (s *headerRecordingQueryServer) QueryLogs(req *evm.QueryLogsRequest, stream grpc.ServerStreamingServer[evm.QueryLogsResponse]) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	key := md.Get("x-api-key")
	s.apiKey.Store(&key)
	ref := &evm.CursorBlock{Number: 9, Hash: make([]byte, 32), ParentHash: make([]byte, 32)}
	return stream.Send(&evm.QueryLogsResponse{FromBlock: ref, ToBlock: ref, CursorBlock: ref})
}

// silentQueryServer accepts a QueryLogs stream and sends nothing until the
// client leaves.
type silentQueryServer struct {
	evm.UnimplementedQueryServiceServer
}

func (s *silentQueryServer) QueryLogs(req *evm.QueryLogsRequest, stream grpc.ServerStreamingServer[evm.QueryLogsResponse]) error {
	<-stream.Context().Done()
	return stream.Context().Err()
}

type fakeGrpcBdsClient struct {
	queryClient evm.QueryServiceClient
}

func (c *fakeGrpcBdsClient) GetType() clients.ClientType { return clients.ClientTypeGrpcBds }

func (c *fakeGrpcBdsClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, errors.New("unexpected SendRequest")
}

func (c *fakeGrpcBdsClient) SetHeaders(h map[string]string) {}

func (c *fakeGrpcBdsClient) QueryClient() evm.QueryServiceClient { return c.queryClient }

func (c *fakeGrpcBdsClient) WithHeaders(ctx context.Context) context.Context { return ctx }

type fakeQueryServiceClient struct {
	queryBlocksFn       func(ctx context.Context, in *evm.QueryBlocksRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryBlocksResponse], error)
	queryTransactionsFn func(ctx context.Context, in *evm.QueryTransactionsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransactionsResponse], error)
	queryLogsFn         func(ctx context.Context, in *evm.QueryLogsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryLogsResponse], error)
	queryTracesFn       func(ctx context.Context, in *evm.QueryTracesRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTracesResponse], error)
	queryTransfersFn    func(ctx context.Context, in *evm.QueryTransfersRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransfersResponse], error)
}

func (c *fakeQueryServiceClient) QueryBlocks(ctx context.Context, in *evm.QueryBlocksRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryBlocksResponse], error) {
	if c.queryBlocksFn == nil {
		return nil, status.Error(codes.Unimplemented, "QueryBlocks")
	}
	return c.queryBlocksFn(ctx, in, opts...)
}

func (c *fakeQueryServiceClient) QueryTransactions(ctx context.Context, in *evm.QueryTransactionsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransactionsResponse], error) {
	if c.queryTransactionsFn == nil {
		return nil, status.Error(codes.Unimplemented, "QueryTransactions")
	}
	return c.queryTransactionsFn(ctx, in, opts...)
}

func (c *fakeQueryServiceClient) QueryLogs(ctx context.Context, in *evm.QueryLogsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryLogsResponse], error) {
	if c.queryLogsFn == nil {
		return nil, status.Error(codes.Unimplemented, "QueryLogs")
	}
	return c.queryLogsFn(ctx, in, opts...)
}

func (c *fakeQueryServiceClient) QueryTraces(ctx context.Context, in *evm.QueryTracesRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTracesResponse], error) {
	if c.queryTracesFn == nil {
		return nil, status.Error(codes.Unimplemented, "QueryTraces")
	}
	return c.queryTracesFn(ctx, in, opts...)
}

func (c *fakeQueryServiceClient) QueryTransfers(ctx context.Context, in *evm.QueryTransfersRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransfersResponse], error) {
	if c.queryTransfersFn == nil {
		return nil, status.Error(codes.Unimplemented, "QueryTransfers")
	}
	return c.queryTransfersFn(ctx, in, opts...)
}

type fakeServerStreamingClient[Res any] struct {
	responses []*Res
	finalErr  error
	index     int
	ctx       context.Context
}

func (s *fakeServerStreamingClient[Res]) Recv() (*Res, error) {
	if s.index < len(s.responses) {
		resp := s.responses[s.index]
		s.index++
		return resp, nil
	}
	if s.finalErr != nil {
		err := s.finalErr
		s.finalErr = nil
		return nil, err
	}
	return nil, io.EOF
}

func (s *fakeServerStreamingClient[Res]) Header() (metadata.MD, error) { return metadata.MD{}, nil }

func (s *fakeServerStreamingClient[Res]) Trailer() metadata.MD { return metadata.MD{} }

func (s *fakeServerStreamingClient[Res]) CloseSend() error { return nil }

func (s *fakeServerStreamingClient[Res]) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func (s *fakeServerStreamingClient[Res]) SendMsg(m any) error { return nil }

func (s *fakeServerStreamingClient[Res]) RecvMsg(m any) error { return nil }

func newTestQueryExecutor(t *testing.T, method string, upstreams ...*upstreampkg.Upstream) *EvmQueryExecutor {
	t.Helper()

	logger := zerolog.Nop()
	return &EvmQueryExecutor{
		network: &Network{
			networkId:         "evm:1",
			logger:            &logger,
			cfg:               &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}},
			upstreamsRegistry: newTestUpstreamsRegistry(t, "evm:1", method, upstreams...),
		},
		logger: &logger,
	}
}

func newTestUpstreamsRegistry(t *testing.T, networkID, method string, upstreams ...*upstreampkg.Upstream) *upstreampkg.UpstreamsRegistry {
	t.Helper()
	_ = method // method parameter retained for callsite parity; new registry is method-agnostic.

	registry := &upstreampkg.UpstreamsRegistry{}
	setUnexportedField(t, registry, "upstreamsMu", &sync.RWMutex{})
	setUnexportedField(t, registry, "networkUpstreams", map[string][]*upstreampkg.Upstream{
		networkID: upstreams,
	})
	return registry
}

func newTestQueryUpstream(t *testing.T, id string, client clients.ClientInterface) *upstreampkg.Upstream {
	t.Helper()

	ups := &upstreampkg.Upstream{Client: client}
	logger := zerolog.Nop()

	setUnexportedField(t, ups, "config", &common.UpstreamConfig{
		Id:       id,
		Type:     common.UpstreamTypeEvm,
		Endpoint: "grpc://bds.example:443",
		Evm:      &common.EvmUpstreamConfig{ChainId: 1},
	})
	setUnexportedField(t, ups, "logger", &logger)
	setUnexportedField(t, ups, "metricsTracker", health.NewTracker(&logger, "prjA", time.Minute))

	return ups
}

func setUnexportedField(t *testing.T, target any, fieldName string, value any) {
	t.Helper()

	field := reflect.ValueOf(target).Elem().FieldByName(fieldName)
	require.True(t, field.IsValid(), "field %s must exist", fieldName)

	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(reflect.ValueOf(value))
}
