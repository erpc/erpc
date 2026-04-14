package erpc

import (
	"context"
	"errors"
	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type EvmQueryExecutor struct {
	network             *Network
	logger              *zerolog.Logger
	parentRequestId     interface{}
	forwardSubrequestFn func(context.Context, string, []interface{}) ([]byte, error)
}

func NewEvmQueryExecutor(network *Network, logger *zerolog.Logger) *EvmQueryExecutor {
	return &EvmQueryExecutor{network: network, logger: logger}
}

func (qe *EvmQueryExecutor) Execute(ctx context.Context, req proto.Message, onPage func(proto.Message) error) error {
	switch r := req.(type) {
	case *evm.QueryBlocksRequest:
		return qe.queryBlocks(ctx, r, onPage)
	case *evm.QueryTransactionsRequest:
		return qe.queryTransactions(ctx, r, onPage)
	case *evm.QueryLogsRequest:
		return qe.queryLogs(ctx, r, onPage)
	case *evm.QueryTracesRequest:
		return qe.queryTraces(ctx, r, onPage)
	case *evm.QueryTransfersRequest:
		return qe.queryTransfers(ctx, r, onPage)
	default:
		return status.Error(codes.InvalidArgument, "unknown query request type")
	}
}

func (qe *EvmQueryExecutor) queryBlocks(ctx context.Context, req *evm.QueryBlocksRequest, onPage func(proto.Message) error) error {
	ctx, span := common.StartDetailSpan(ctx, "Query.Execute",
		trace.WithAttributes(attribute.String("query.method", "eth_queryBlocks")),
	)
	defer span.End()

	fromBlock, toBlock, err := qe.resolveQueryBounds(ctx, req.GetFromBlock(), req.GetToBlock(), req.GetOrder(), req.GetCursor())
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	span.SetAttributes(
		attribute.Int64("query.fromBlock", int64(fromBlock)),
		attribute.Int64("query.toBlock", int64(toBlock)),
	)
	qe.logger.Debug().Uint64("fromBlock", fromBlock).Uint64("toBlock", toBlock).Msgf("resolved query bounds for eth_queryBlocks")

	handled, err := qe.tryQueryUpstreams(ctx, "eth_queryBlocks", func(ups common.Upstream) error {
		return qe.pipeThroughQueryBlocks(ctx, ups, req, onPage)
	})
	if handled {
		if err != nil {
			common.SetTraceSpanError(span, err)
		}
		return err
	}

	qe.logger.Debug().Msgf("no native upstream available, using shim for eth_queryBlocks")
	span.SetAttributes(attribute.String("query.path", "shim"))
	return qe.shimQueryBlocks(ctx, req, fromBlock, toBlock, onPage)
}

func (qe *EvmQueryExecutor) queryTransactions(ctx context.Context, req *evm.QueryTransactionsRequest, onPage func(proto.Message) error) error {
	ctx, span := common.StartDetailSpan(ctx, "Query.Execute",
		trace.WithAttributes(attribute.String("query.method", "eth_queryTransactions")),
	)
	defer span.End()

	fromBlock, toBlock, err := qe.resolveQueryBounds(ctx, req.GetFromBlock(), req.GetToBlock(), req.GetOrder(), req.GetCursor())
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	span.SetAttributes(
		attribute.Int64("query.fromBlock", int64(fromBlock)),
		attribute.Int64("query.toBlock", int64(toBlock)),
	)
	qe.logger.Debug().Uint64("fromBlock", fromBlock).Uint64("toBlock", toBlock).Msgf("resolved query bounds for eth_queryTransactions")

	handled, err := qe.tryQueryUpstreams(ctx, "eth_queryTransactions", func(ups common.Upstream) error {
		return qe.pipeThroughQueryTransactions(ctx, ups, req, onPage)
	})
	if handled {
		if err != nil {
			common.SetTraceSpanError(span, err)
		}
		return err
	}

	qe.logger.Debug().Msgf("no native upstream available, using shim for eth_queryTransactions")
	span.SetAttributes(attribute.String("query.path", "shim"))
	return qe.shimQueryTransactions(ctx, req, fromBlock, toBlock, onPage)
}

func (qe *EvmQueryExecutor) queryLogs(ctx context.Context, req *evm.QueryLogsRequest, onPage func(proto.Message) error) error {
	ctx, span := common.StartDetailSpan(ctx, "Query.Execute",
		trace.WithAttributes(attribute.String("query.method", "eth_queryLogs")),
	)
	defer span.End()

	fromBlock, toBlock, err := qe.resolveQueryBounds(ctx, req.GetFromBlock(), req.GetToBlock(), req.GetOrder(), req.GetCursor())
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	span.SetAttributes(
		attribute.Int64("query.fromBlock", int64(fromBlock)),
		attribute.Int64("query.toBlock", int64(toBlock)),
	)
	qe.logger.Debug().Uint64("fromBlock", fromBlock).Uint64("toBlock", toBlock).Msgf("resolved query bounds for eth_queryLogs")

	handled, err := qe.tryQueryUpstreams(ctx, "eth_queryLogs", func(ups common.Upstream) error {
		return qe.pipeThroughQueryLogs(ctx, ups, req, onPage)
	})
	if handled {
		if err != nil {
			common.SetTraceSpanError(span, err)
		}
		return err
	}

	qe.logger.Debug().Msgf("no native upstream available, using shim for eth_queryLogs")
	span.SetAttributes(attribute.String("query.path", "shim"))
	return qe.shimQueryLogs(ctx, req, fromBlock, toBlock, onPage)
}

func (qe *EvmQueryExecutor) queryTraces(ctx context.Context, req *evm.QueryTracesRequest, onPage func(proto.Message) error) error {
	ctx, span := common.StartDetailSpan(ctx, "Query.Execute",
		trace.WithAttributes(attribute.String("query.method", "eth_queryTraces")),
	)
	defer span.End()

	fromBlock, toBlock, err := qe.resolveQueryBounds(ctx, req.GetFromBlock(), req.GetToBlock(), req.GetOrder(), req.GetCursor())
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	span.SetAttributes(
		attribute.Int64("query.fromBlock", int64(fromBlock)),
		attribute.Int64("query.toBlock", int64(toBlock)),
	)
	qe.logger.Debug().Uint64("fromBlock", fromBlock).Uint64("toBlock", toBlock).Msgf("resolved query bounds for eth_queryTraces")

	handled, err := qe.tryQueryUpstreams(ctx, "eth_queryTraces", func(ups common.Upstream) error {
		return qe.pipeThroughQueryTraces(ctx, ups, req, onPage)
	})
	if handled {
		if err != nil {
			common.SetTraceSpanError(span, err)
		}
		return err
	}

	qe.logger.Debug().Msgf("no native upstream available, using shim for eth_queryTraces")
	span.SetAttributes(attribute.String("query.path", "shim"))
	return qe.shimQueryTraces(ctx, req, fromBlock, toBlock, onPage)
}

func (qe *EvmQueryExecutor) queryTransfers(ctx context.Context, req *evm.QueryTransfersRequest, onPage func(proto.Message) error) error {
	ctx, span := common.StartDetailSpan(ctx, "Query.Execute",
		trace.WithAttributes(attribute.String("query.method", "eth_queryTransfers")),
	)
	defer span.End()

	fromBlock, toBlock, err := qe.resolveQueryBounds(ctx, req.GetFromBlock(), req.GetToBlock(), req.GetOrder(), req.GetCursor())
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	span.SetAttributes(
		attribute.Int64("query.fromBlock", int64(fromBlock)),
		attribute.Int64("query.toBlock", int64(toBlock)),
	)
	qe.logger.Debug().Uint64("fromBlock", fromBlock).Uint64("toBlock", toBlock).Msgf("resolved query bounds for eth_queryTransfers")

	handled, err := qe.tryQueryUpstreams(ctx, "eth_queryTransfers", func(ups common.Upstream) error {
		return qe.pipeThroughQueryTransfers(ctx, ups, req, onPage)
	})
	if handled {
		if err != nil {
			common.SetTraceSpanError(span, err)
		}
		return err
	}

	qe.logger.Debug().Msgf("no native upstream available, using shim for eth_queryTransfers")
	span.SetAttributes(attribute.String("query.path", "shim"))
	return qe.shimQueryTransfers(ctx, req, fromBlock, toBlock, onPage)
}

func (qe *EvmQueryExecutor) tryQueryUpstreams(
	ctx context.Context,
	method string,
	attempt func(common.Upstream) error,
) (handled bool, err error) {
	upstreams, err := qe.network.upstreamsRegistry.GetSortedUpstreams(ctx, qe.network.Id(), method)
	if err != nil {
		qe.logger.Debug().Err(err).Str("method", method).Msgf("failed to get sorted upstreams for query method")
		return false, nil
	}

	for _, ups := range upstreams {
		if !qe.supportsQueryMethods(ups) {
			qe.logger.Trace().Str("upstreamId", ups.Id()).Str("method", method).Msgf("upstream does not support query streaming, skipping")
			continue
		}

		qe.logger.Debug().Str("upstreamId", ups.Id()).Str("method", method).Msgf("attempting native query pipe-through to upstream")

		err := attempt(ups)
		if err == nil {
			qe.logger.Debug().Str("upstreamId", ups.Id()).Str("method", method).Msgf("native query pipe-through succeeded")
			return true, nil
		}
		if qe.canRetryQueryStream(err) {
			qe.logger.Debug().Err(err).Str("upstreamId", ups.Id()).Str("method", method).Msgf("query pipe-through failed before page emission, trying next upstream")
			continue
		}
		qe.logger.Debug().Err(err).Str("upstreamId", ups.Id()).Str("method", method).Msgf("query pipe-through failed after page emission, cannot retry")
		return true, err
	}

	return false, nil
}

func (qe *EvmQueryExecutor) canRetryQueryStream(err error) bool {
	if err == nil {
		return false
	}

	var streamErr *StreamError
	if errors.As(err, &streamErr) {
		return !streamErr.PageEmitted
	}

	return true
}

func (qe *EvmQueryExecutor) supportsQueryMethods(ups common.Upstream) bool {
	client, ok := getGrpcBdsClient(ups)
	return ok && client.QueryClient() != nil
}

func (qe *EvmQueryExecutor) resolveQueryBounds(ctx context.Context, from, to string, order evm.SortOrder, cursor *evm.CursorBlock) (uint64, uint64, error) {
	_, span := common.StartDetailSpan(ctx, "Query.ResolveQueryBounds")
	defer span.End()

	fromBlock, err := qe.resolveBlockTag(ctx, from, false)
	if err != nil {
		return 0, 0, err
	}
	toBlock, err := qe.resolveBlockTag(ctx, to, true)
	if err != nil {
		return 0, 0, err
	}
	if fromBlock > toBlock {
		return 0, 0, status.Error(codes.InvalidArgument, "fromBlock must be less than or equal to toBlock")
	}
	if cursor != nil {
		if order == evm.SortOrder_DESC {
			if cursor.Number == 0 {
				return 0, 0, status.Error(codes.InvalidArgument, "invalid DESC cursor")
			}
			toBlock = cursor.Number - 1
		} else {
			fromBlock = cursor.Number + 1
		}
		qe.logger.Trace().
			Uint64("cursorNumber", cursor.Number).
			Str("order", order.String()).
			Uint64("adjustedFrom", fromBlock).
			Uint64("adjustedTo", toBlock).
			Msgf("adjusted query bounds from cursor")
	}
	return fromBlock, toBlock, nil
}

func (qe *EvmQueryExecutor) resolveBlockTag(ctx context.Context, block string, upper bool) (uint64, error) {
	switch block {
	case "", "latest":
		return uint64(qe.network.EvmHighestLatestBlockNumber(ctx)), nil
	case "finalized":
		return uint64(qe.network.EvmHighestFinalizedBlockNumber(ctx)), nil
	case "safe":
		if finalized := qe.network.EvmHighestFinalizedBlockNumber(ctx); finalized > 0 {
			return uint64(finalized), nil
		}
		return uint64(qe.network.EvmHighestLatestBlockNumber(ctx)), nil
	case "earliest":
		return 0, nil
	case "pending":
		return 0, status.Error(codes.InvalidArgument, "pending is not supported for query methods")
	default:
		n, err := evm.HexToUint64(block)
		if err == nil {
			return n, nil
		}
		return 0, status.Errorf(codes.InvalidArgument, "invalid block reference: %s", block)
	}
}
