package erpc

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	upstreampkg "github.com/erpc/erpc/upstream"
	"google.golang.org/protobuf/proto"
)

// StreamError is a native query stream failure. PageEmitted tells whether the
// caller already received a page, after which no other upstream may continue
// the stream.
type StreamError struct {
	Err         error
	PageEmitted bool
}

func (e *StreamError) Error() string { return e.Err.Error() }
func (e *StreamError) Unwrap() error { return e.Err }

func getGrpcBdsClient(ups common.Upstream) (clients.GrpcBdsClient, bool) {
	concrete, ok := ups.(*upstreampkg.Upstream)
	if !ok || concrete == nil || concrete.Client == nil {
		return nil, false
	}
	client, ok := concrete.Client.(clients.GrpcBdsClient)
	return client, ok
}

// servesNativeQuery reports whether ups answers bds.evm.QueryService itself
// (a gRPC BDS client), rather than through the query shim.
func servesNativeQuery(ups common.Upstream) bool {
	client, ok := getGrpcBdsClient(ups)
	return ok && client.QueryClient() != nil
}

// pipeThrough sends the MIP-16 request unchanged to a native QueryService
// upstream and hands each page to onPage. When onPage stops the stream
// (errQueryPageDone), the stream is cancelled.
func (qe *EvmQueryExecutor) pipeThrough(ctx context.Context, ups common.Upstream, req proto.Message, onPage func(proto.Message) error) error {
	client, ok := getGrpcBdsClient(ups)
	if !ok || client.QueryClient() == nil {
		return fmt.Errorf("upstream %s does not serve QueryService", ups.Id())
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx, span := common.StartDetailSpan(ctx, "Query.PipeThrough")
	defer span.End()
	// The upstream's grpc.headers (auth keys) go on every call, as SendRequest sends them.
	ctx = client.WithHeaders(ctx)

	qc := client.QueryClient()
	var recv func() (proto.Message, error)
	var err error
	switch r := req.(type) {
	case *evm.QueryBlocksRequest:
		var s interface {
			Recv() (*evm.QueryBlocksResponse, error)
		}
		if s, err = qc.QueryBlocks(ctx, r); err == nil {
			recv = func() (proto.Message, error) { return s.Recv() }
		}
	case *evm.QueryTransactionsRequest:
		var s interface {
			Recv() (*evm.QueryTransactionsResponse, error)
		}
		if s, err = qc.QueryTransactions(ctx, r); err == nil {
			recv = func() (proto.Message, error) { return s.Recv() }
		}
	case *evm.QueryLogsRequest:
		var s interface {
			Recv() (*evm.QueryLogsResponse, error)
		}
		if s, err = qc.QueryLogs(ctx, r); err == nil {
			recv = func() (proto.Message, error) { return s.Recv() }
		}
	case *evm.QueryTracesRequest:
		var s interface {
			Recv() (*evm.QueryTracesResponse, error)
		}
		if s, err = qc.QueryTraces(ctx, r); err == nil {
			recv = func() (proto.Message, error) { return s.Recv() }
		}
	case *evm.QueryTransfersRequest:
		var s interface {
			Recv() (*evm.QueryTransfersResponse, error)
		}
		if s, err = qc.QueryTransfers(ctx, r); err == nil {
			recv = func() (proto.Message, error) { return s.Recv() }
		}
	default:
		return fmt.Errorf("unknown query request type %T", req)
	}
	if err != nil {
		common.SetTraceSpanError(span, err)
		return &StreamError{Err: err}
	}

	emitted := false
	for {
		page, err := recv()
		if err == io.EOF {
			if !emitted {
				return &StreamError{Err: fmt.Errorf("upstream %s closed the query stream without a page", ups.Id())}
			}
			return nil
		}
		if err != nil {
			common.SetTraceSpanError(span, err)
			return &StreamError{Err: err, PageEmitted: emitted}
		}
		if err := onPage(page); err != nil {
			if errors.Is(err, errQueryPageDone) {
				return nil
			}
			return &StreamError{Err: err, PageEmitted: true}
		}
		emitted = true
	}
}
