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
	qe.logger.Debug().Str("upstreamId", ups.Id()).Msgf("opening QueryBlocks stream to upstream")
	stream, err := client.QueryClient().QueryBlocks(ctx, req)
	if err != nil {
		qe.logger.Debug().Err(err).Str("upstreamId", ups.Id()).Msgf("failed to open QueryBlocks stream")
		return err
	}
	return qe.recvProtoStream(func() (proto.Message, error) { return stream.Recv() }, onPage, "eth_queryBlocks", ups.Id())
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
	qe.logger.Debug().Str("upstreamId", ups.Id()).Msgf("opening QueryTransactions stream to upstream")
	stream, err := client.QueryClient().QueryTransactions(ctx, req)
	if err != nil {
		return err
	}
	return qe.recvProtoStream(func() (proto.Message, error) { return stream.Recv() }, onPage, "eth_queryTransactions", ups.Id())
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
	qe.logger.Debug().Str("upstreamId", ups.Id()).Msgf("opening QueryLogs stream to upstream")
	stream, err := client.QueryClient().QueryLogs(ctx, req)
	if err != nil {
		return err
	}
	return qe.recvProtoStream(func() (proto.Message, error) { return stream.Recv() }, onPage, "eth_queryLogs", ups.Id())
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
	qe.logger.Debug().Str("upstreamId", ups.Id()).Msgf("opening QueryTraces stream to upstream")
	stream, err := client.QueryClient().QueryTraces(ctx, req)
	if err != nil {
		return err
	}
	return qe.recvProtoStream(func() (proto.Message, error) { return stream.Recv() }, onPage, "eth_queryTraces", ups.Id())
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
	qe.logger.Debug().Str("upstreamId", ups.Id()).Msgf("opening QueryTransfers stream to upstream")
	stream, err := client.QueryClient().QueryTransfers(ctx, req)
	if err != nil {
		return err
	}
	return qe.recvProtoStream(func() (proto.Message, error) { return stream.Recv() }, onPage, "eth_queryTransfers", ups.Id())
}

func (qe *EvmQueryExecutor) recvProtoStream(recv func() (proto.Message, error), onPage func(proto.Message) error, method string, upstreamId string) error {
	type cursorPage interface {
		proto.Message
		GetCursorBlock() *evm.CursorBlock
	}

	var lastCursor *evm.CursorBlock
	pageEmitted := false
	pageCount := 0

	for {
		page, err := recv()
		if err == io.EOF {
			qe.logger.Debug().Str("upstreamId", upstreamId).Str("method", method).Int("pagesReceived", pageCount).Msgf("upstream query stream completed (EOF)")
			return nil
		}
		if err != nil {
			qe.logger.Debug().Err(err).Str("upstreamId", upstreamId).Str("method", method).Int("pagesReceived", pageCount).Bool("pageEmitted", pageEmitted).Msgf("upstream query stream error")
			return &StreamError{Err: err, LastCursor: lastCursor, PageEmitted: pageEmitted}
		}

		pageCount++
		cursorBlock := (*evm.CursorBlock)(nil)
		if cursorAware, ok := any(page).(cursorPage); ok {
			cursorBlock = cursorAware.GetCursorBlock()
			if cursorBlock != nil {
				lastCursor = cursorBlock
			}
		}

		qe.logger.Trace().Str("upstreamId", upstreamId).Str("method", method).Int("page", pageCount).Interface("cursor", cursorBlock).Msgf("received page from upstream query stream")

		if err := onPage(page); err != nil {
			return &StreamError{Err: err, LastCursor: lastCursor, PageEmitted: true}
		}
		pageEmitted = true

		if cursorBlock == nil {
			qe.logger.Debug().Str("upstreamId", upstreamId).Str("method", method).Int("pagesReceived", pageCount).Msgf("upstream query stream completed (no cursor)")
			return nil
		}
	}
}
