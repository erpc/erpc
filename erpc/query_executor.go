package erpc

import (
	"context"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type EvmQueryExecutor struct {
	network *Network
	logger  *zerolog.Logger
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
	fromBlock, toBlock, err := qe.resolveQueryBounds(ctx, req.GetFromBlock(), req.GetToBlock(), req.GetOrder(), req.GetCursor())
	if err != nil {
		return err
	}
	if upstreams, err := qe.network.upstreamsRegistry.GetSortedUpstreams(ctx, qe.network.Id(), "eth_queryBlocks"); err == nil {
		for _, ups := range upstreams {
			if qe.supportsQueryMethods(ups) {
				if err := qe.pipeThroughQueryBlocks(ctx, ups, req, onPage); err == nil {
					return nil
				}
			}
		}
	}
	return qe.shimQueryBlocks(ctx, req, fromBlock, toBlock, onPage)
}

func (qe *EvmQueryExecutor) queryTransactions(ctx context.Context, req *evm.QueryTransactionsRequest, onPage func(proto.Message) error) error {
	fromBlock, toBlock, err := qe.resolveQueryBounds(ctx, req.GetFromBlock(), req.GetToBlock(), req.GetOrder(), req.GetCursor())
	if err != nil {
		return err
	}
	if upstreams, err := qe.network.upstreamsRegistry.GetSortedUpstreams(ctx, qe.network.Id(), "eth_queryTransactions"); err == nil {
		for _, ups := range upstreams {
			if qe.supportsQueryMethods(ups) {
				if err := qe.pipeThroughQueryTransactions(ctx, ups, req, onPage); err == nil {
					return nil
				}
			}
		}
	}
	return qe.shimQueryTransactions(ctx, req, fromBlock, toBlock, onPage)
}

func (qe *EvmQueryExecutor) queryLogs(ctx context.Context, req *evm.QueryLogsRequest, onPage func(proto.Message) error) error {
	fromBlock, toBlock, err := qe.resolveQueryBounds(ctx, req.GetFromBlock(), req.GetToBlock(), req.GetOrder(), req.GetCursor())
	if err != nil {
		return err
	}
	if upstreams, err := qe.network.upstreamsRegistry.GetSortedUpstreams(ctx, qe.network.Id(), "eth_queryLogs"); err == nil {
		for _, ups := range upstreams {
			if qe.supportsQueryMethods(ups) {
				if err := qe.pipeThroughQueryLogs(ctx, ups, req, onPage); err == nil {
					return nil
				}
			}
		}
	}
	return qe.shimQueryLogs(ctx, req, fromBlock, toBlock, onPage)
}

func (qe *EvmQueryExecutor) queryTraces(ctx context.Context, req *evm.QueryTracesRequest, onPage func(proto.Message) error) error {
	fromBlock, toBlock, err := qe.resolveQueryBounds(ctx, req.GetFromBlock(), req.GetToBlock(), req.GetOrder(), req.GetCursor())
	if err != nil {
		return err
	}
	if upstreams, err := qe.network.upstreamsRegistry.GetSortedUpstreams(ctx, qe.network.Id(), "eth_queryTraces"); err == nil {
		for _, ups := range upstreams {
			if qe.supportsQueryMethods(ups) {
				if err := qe.pipeThroughQueryTraces(ctx, ups, req, onPage); err == nil {
					return nil
				}
			}
		}
	}
	return qe.shimQueryTraces(ctx, req, fromBlock, toBlock, onPage)
}

func (qe *EvmQueryExecutor) queryTransfers(ctx context.Context, req *evm.QueryTransfersRequest, onPage func(proto.Message) error) error {
	fromBlock, toBlock, err := qe.resolveQueryBounds(ctx, req.GetFromBlock(), req.GetToBlock(), req.GetOrder(), req.GetCursor())
	if err != nil {
		return err
	}
	if upstreams, err := qe.network.upstreamsRegistry.GetSortedUpstreams(ctx, qe.network.Id(), "eth_queryTransfers"); err == nil {
		for _, ups := range upstreams {
			if qe.supportsQueryMethods(ups) {
				if err := qe.pipeThroughQueryTransfers(ctx, ups, req, onPage); err == nil {
					return nil
				}
			}
		}
	}
	return qe.shimQueryTransfers(ctx, req, fromBlock, toBlock, onPage)
}

func (qe *EvmQueryExecutor) supportsQueryMethods(ups common.Upstream) bool {
	client, ok := getGrpcBdsClient(ups)
	return ok && client.QueryClient() != nil
}

func (qe *EvmQueryExecutor) resolveQueryBounds(ctx context.Context, from, to string, order evm.SortOrder, cursor *evm.CursorBlock) (uint64, uint64, error) {
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
	}
	return fromBlock, toBlock, nil
}

func (qe *EvmQueryExecutor) resolveBlockTag(ctx context.Context, block string, upper bool) (uint64, error) {
	switch block {
	case "", "latest":
		return uint64(qe.network.EvmHighestLatestBlockNumber(ctx)), nil
	case "finalized", "safe":
		return uint64(qe.network.EvmHighestFinalizedBlockNumber(ctx)), nil
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
