package erpc

import (
	"context"
	"fmt"
	"io"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	upstreampkg "github.com/erpc/erpc/upstream"
	"google.golang.org/protobuf/proto"
)

type StreamError struct {
	Err         error
	LastCursor  *evm.CursorBlock
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

func (qe *EvmQueryExecutor) pipeThroughQueryBlocks(
	ctx context.Context,
	ups common.Upstream,
	req *evm.QueryBlocksRequest,
	onPage func(proto.Message) error,
) error {
	client, ok := getGrpcBdsClient(ups)
	if !ok || client.QueryClient() == nil {
		return fmt.Errorf("upstream %s does not support query streaming", ups.Id())
	}
	stream, err := client.QueryClient().QueryBlocks(ctx, req)
	if err != nil {
		return err
	}
	return recvProtoStream(stream.Recv, onPage)
}

func (qe *EvmQueryExecutor) pipeThroughQueryTransactions(
	ctx context.Context,
	ups common.Upstream,
	req *evm.QueryTransactionsRequest,
	onPage func(proto.Message) error,
) error {
	client, ok := getGrpcBdsClient(ups)
	if !ok || client.QueryClient() == nil {
		return fmt.Errorf("upstream %s does not support query streaming", ups.Id())
	}
	stream, err := client.QueryClient().QueryTransactions(ctx, req)
	if err != nil {
		return err
	}
	return recvProtoStream(stream.Recv, onPage)
}

func (qe *EvmQueryExecutor) pipeThroughQueryLogs(
	ctx context.Context,
	ups common.Upstream,
	req *evm.QueryLogsRequest,
	onPage func(proto.Message) error,
) error {
	client, ok := getGrpcBdsClient(ups)
	if !ok || client.QueryClient() == nil {
		return fmt.Errorf("upstream %s does not support query streaming", ups.Id())
	}
	stream, err := client.QueryClient().QueryLogs(ctx, req)
	if err != nil {
		return err
	}
	return recvProtoStream(stream.Recv, onPage)
}

func (qe *EvmQueryExecutor) pipeThroughQueryTraces(
	ctx context.Context,
	ups common.Upstream,
	req *evm.QueryTracesRequest,
	onPage func(proto.Message) error,
) error {
	client, ok := getGrpcBdsClient(ups)
	if !ok || client.QueryClient() == nil {
		return fmt.Errorf("upstream %s does not support query streaming", ups.Id())
	}
	stream, err := client.QueryClient().QueryTraces(ctx, req)
	if err != nil {
		return err
	}
	return recvProtoStream(stream.Recv, onPage)
}

func (qe *EvmQueryExecutor) pipeThroughQueryTransfers(
	ctx context.Context,
	ups common.Upstream,
	req *evm.QueryTransfersRequest,
	onPage func(proto.Message) error,
) error {
	client, ok := getGrpcBdsClient(ups)
	if !ok || client.QueryClient() == nil {
		return fmt.Errorf("upstream %s does not support query streaming", ups.Id())
	}
	stream, err := client.QueryClient().QueryTransfers(ctx, req)
	if err != nil {
		return err
	}
	return recvProtoStream(stream.Recv, onPage)
}

func recvProtoStream[T proto.Message](recv func() (T, error), onPage func(proto.Message) error) error {
	type cursorPage interface {
		proto.Message
		GetCursorBlock() *evm.CursorBlock
	}

	var lastCursor *evm.CursorBlock
	pageEmitted := false

	for {
		page, err := recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return &StreamError{Err: err, LastCursor: lastCursor, PageEmitted: pageEmitted}
		}

		cursorBlock := (*evm.CursorBlock)(nil)
		if cursorAware, ok := any(page).(cursorPage); ok {
			cursorBlock = cursorAware.GetCursorBlock()
			if cursorBlock != nil {
				lastCursor = cursorBlock
			}
		}

		if err := onPage(page); err != nil {
			return &StreamError{Err: err, LastCursor: lastCursor, PageEmitted: true}
		}
		pageEmitted = true

		if cursorBlock == nil {
			return nil
		}
	}
}
