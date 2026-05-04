package erpc

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"unsafe"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	upstreampkg "github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

func TestQueryBlocks_DoesNotFallbackAfterPartialPipeThroughFailure(t *testing.T) {
	t.Helper()

	firstCalls := 0
	secondCalls := 0

	firstUpstream := newTestQueryUpstream(
		t,
		"upstream-1",
		&fakeGrpcBdsClient{
			queryClient: &fakeQueryServiceClient{
				queryBlocksFn: func(ctx context.Context, in *evm.QueryBlocksRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryBlocksResponse], error) {
					firstCalls++
					return &fakeServerStreamingClient[evm.QueryBlocksResponse]{
						responses: []*evm.QueryBlocksResponse{
							{
								Blocks:      []*evm.BlockHeader{{Number: 1}},
								CursorBlock: &evm.CursorBlock{Number: 1},
							},
						},
						finalErr: errors.New("upstream stream failed"),
					}, nil
				},
			},
		},
	)
	secondUpstream := newTestQueryUpstream(
		t,
		"upstream-2",
		&fakeGrpcBdsClient{
			queryClient: &fakeQueryServiceClient{
				queryBlocksFn: func(ctx context.Context, in *evm.QueryBlocksRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryBlocksResponse], error) {
					secondCalls++
					return &fakeServerStreamingClient[evm.QueryBlocksResponse]{
						responses: []*evm.QueryBlocksResponse{
							{
								Blocks: []*evm.BlockHeader{{Number: 1}},
							},
						},
					}, nil
				},
			},
		},
	)

	qe := newTestQueryExecutor(t, "eth_queryBlocks", firstUpstream, secondUpstream)

	pageCount := 0
	err := qe.queryBlocks(context.Background(), &evm.QueryBlocksRequest{
		FromBlock: util.StringPtr("0x1"),
		ToBlock:   util.StringPtr("0x2"),
	}, func(page proto.Message) error {
		pageCount++
		return nil
	})

	require.Error(t, err)
	assert.Equal(t, 1, firstCalls)
	assert.Equal(t, 0, secondCalls)
	assert.Equal(t, 1, pageCount)

	var streamErr *StreamError
	require.ErrorAs(t, err, &streamErr)
	assert.True(t, streamErr.PageEmitted)
	require.NotNil(t, streamErr.LastCursor)
	assert.Equal(t, uint64(1), streamErr.LastCursor.Number)
}

func TestQueryBlocks_FallsBackWhenPipeThroughFailsBeforeFirstPage(t *testing.T) {
	t.Helper()

	firstCalls := 0
	secondCalls := 0

	firstUpstream := newTestQueryUpstream(
		t,
		"upstream-1",
		&fakeGrpcBdsClient{
			queryClient: &fakeQueryServiceClient{
				queryBlocksFn: func(ctx context.Context, in *evm.QueryBlocksRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryBlocksResponse], error) {
					firstCalls++
					return &fakeServerStreamingClient[evm.QueryBlocksResponse]{
						finalErr: errors.New("upstream failed before first page"),
					}, nil
				},
			},
		},
	)
	secondUpstream := newTestQueryUpstream(
		t,
		"upstream-2",
		&fakeGrpcBdsClient{
			queryClient: &fakeQueryServiceClient{
				queryBlocksFn: func(ctx context.Context, in *evm.QueryBlocksRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryBlocksResponse], error) {
					secondCalls++
					return &fakeServerStreamingClient[evm.QueryBlocksResponse]{
						responses: []*evm.QueryBlocksResponse{
							{
								Blocks: []*evm.BlockHeader{{Number: 2}},
							},
						},
					}, nil
				},
			},
		},
	)

	qe := newTestQueryExecutor(t, "eth_queryBlocks", firstUpstream, secondUpstream)

	var pages []*evm.QueryBlocksResponse
	err := qe.queryBlocks(context.Background(), &evm.QueryBlocksRequest{
		FromBlock: util.StringPtr("0x1"),
		ToBlock:   util.StringPtr("0x2"),
	}, func(page proto.Message) error {
		pages = append(pages, page.(*evm.QueryBlocksResponse))
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 1, firstCalls)
	assert.Equal(t, 1, secondCalls)
	require.Len(t, pages, 1)
	require.Len(t, pages[0].Blocks, 1)
	assert.Equal(t, uint64(2), pages[0].Blocks[0].Number)
}

func TestQueryLogs_DoesNotFallbackAfterPartialPipeThroughFailure(t *testing.T) {
	t.Helper()

	firstCalls := 0
	secondCalls := 0

	firstUpstream := newTestQueryUpstream(
		t,
		"upstream-1",
		&fakeGrpcBdsClient{
			queryClient: &fakeQueryServiceClient{
				queryLogsFn: func(ctx context.Context, in *evm.QueryLogsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryLogsResponse], error) {
					firstCalls++
					return &fakeServerStreamingClient[evm.QueryLogsResponse]{
						responses: []*evm.QueryLogsResponse{
							{
								Logs:        []*evm.Log{{BlockNumber: 1, LogIndex: 0}},
								CursorBlock: &evm.CursorBlock{Number: 1},
							},
						},
						finalErr: errors.New("upstream log stream failed"),
					}, nil
				},
			},
		},
	)
	secondUpstream := newTestQueryUpstream(
		t,
		"upstream-2",
		&fakeGrpcBdsClient{
			queryClient: &fakeQueryServiceClient{
				queryLogsFn: func(ctx context.Context, in *evm.QueryLogsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryLogsResponse], error) {
					secondCalls++
					return &fakeServerStreamingClient[evm.QueryLogsResponse]{
						responses: []*evm.QueryLogsResponse{
							{
								Logs: []*evm.Log{{BlockNumber: 1, LogIndex: 0}},
							},
						},
					}, nil
				},
			},
		},
	)

	qe := newTestQueryExecutor(t, "eth_queryLogs", firstUpstream, secondUpstream)

	pageCount := 0
	err := qe.queryLogs(context.Background(), &evm.QueryLogsRequest{
		FromBlock: util.StringPtr("0x1"),
		ToBlock:   util.StringPtr("0x2"),
	}, func(page proto.Message) error {
		pageCount++
		return nil
	})

	require.Error(t, err)
	assert.Equal(t, 1, firstCalls)
	assert.Equal(t, 0, secondCalls)
	assert.Equal(t, 1, pageCount)

	var streamErr *StreamError
	require.ErrorAs(t, err, &streamErr)
	assert.True(t, streamErr.PageEmitted)
	require.NotNil(t, streamErr.LastCursor)
	assert.Equal(t, uint64(1), streamErr.LastCursor.Number)
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

type fakeQueryServiceClient struct {
	queryBlocksFn       func(ctx context.Context, in *evm.QueryBlocksRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryBlocksResponse], error)
	queryTransactionsFn func(ctx context.Context, in *evm.QueryTransactionsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransactionsResponse], error)
	queryLogsFn         func(ctx context.Context, in *evm.QueryLogsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryLogsResponse], error)
	queryTracesFn       func(ctx context.Context, in *evm.QueryTracesRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTracesResponse], error)
	queryTransfersFn    func(ctx context.Context, in *evm.QueryTransfersRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransfersResponse], error)
}

func (c *fakeQueryServiceClient) QueryBlocks(ctx context.Context, in *evm.QueryBlocksRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryBlocksResponse], error) {
	if c.queryBlocksFn == nil {
		return nil, errors.New("unexpected QueryBlocks")
	}
	return c.queryBlocksFn(ctx, in, opts...)
}

func (c *fakeQueryServiceClient) QueryTransactions(ctx context.Context, in *evm.QueryTransactionsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransactionsResponse], error) {
	if c.queryTransactionsFn == nil {
		return nil, errors.New("unexpected QueryTransactions")
	}
	return c.queryTransactionsFn(ctx, in, opts...)
}

func (c *fakeQueryServiceClient) QueryLogs(ctx context.Context, in *evm.QueryLogsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryLogsResponse], error) {
	if c.queryLogsFn == nil {
		return nil, errors.New("unexpected QueryLogs")
	}
	return c.queryLogsFn(ctx, in, opts...)
}

func (c *fakeQueryServiceClient) QueryTraces(ctx context.Context, in *evm.QueryTracesRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTracesResponse], error) {
	if c.queryTracesFn == nil {
		return nil, errors.New("unexpected QueryTraces")
	}
	return c.queryTracesFn(ctx, in, opts...)
}

func (c *fakeQueryServiceClient) QueryTransfers(ctx context.Context, in *evm.QueryTransfersRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransfersResponse], error) {
	if c.queryTransfersFn == nil {
		return nil, errors.New("unexpected QueryTransfers")
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

	registry := &upstreampkg.UpstreamsRegistry{}
	setUnexportedField(t, registry, "upstreamsMu", &sync.RWMutex{})
	setUnexportedField(t, registry, "sortedUpstreams", map[string]map[string][]*upstreampkg.Upstream{
		networkID: {
			method: upstreams,
		},
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

	return ups
}

func setUnexportedField(t *testing.T, target any, fieldName string, value any) {
	t.Helper()

	field := reflect.ValueOf(target).Elem().FieldByName(fieldName)
	require.True(t, field.IsValid(), "field %s must exist", fieldName)

	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(reflect.ValueOf(value))
}
