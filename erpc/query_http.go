package erpc

import (
	"context"
	"fmt"
	"strings"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"google.golang.org/protobuf/proto"
)

func handleQueryHTTPForward(ctx context.Context, network *Network, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrReq, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}
	paramsBytes, err := sonic.Marshal(jrReq.Params)
	if err != nil {
		return nil, common.NewErrEndpointClientSideException(
			common.NewErrJsonRpcExceptionInternal(int(common.JsonRpcErrorInvalidArgument), common.JsonRpcErrorInvalidArgument, err.Error(), err, nil),
		)
	}

	var queryReq proto.Message
	method, _ := nq.Method()
	switch strings.ToLower(method) {
	case "eth_queryblocks":
		queryReq, err = bdsevm.QueryBlocksRequestFromJsonRpc(paramsBytes)
	case "eth_querytransactions":
		queryReq, err = bdsevm.QueryTransactionsRequestFromJsonRpc(paramsBytes)
	case "eth_querylogs":
		queryReq, err = bdsevm.QueryLogsRequestFromJsonRpc(paramsBytes)
	case "eth_querytraces":
		queryReq, err = bdsevm.QueryTracesRequestFromJsonRpc(paramsBytes)
	case "eth_querytransfers":
		queryReq, err = bdsevm.QueryTransfersRequestFromJsonRpc(paramsBytes)
	}
	if err != nil {
		return nil, common.NewErrEndpointClientSideException(
			common.NewErrJsonRpcExceptionInternal(int(common.JsonRpcErrorInvalidArgument), common.JsonRpcErrorInvalidArgument, err.Error(), err, nil),
		)
	}

	executor := NewQueryExecutor(network, network.Logger())
	var firstPage proto.Message
	if err := executor.Execute(ctx, queryReq, func(page proto.Message) error {
		firstPage = page
		return nil
	}); err != nil {
		return nil, err
	}

	var result interface{}
	switch strings.ToLower(method) {
	case "eth_queryblocks":
		result = bdsevm.QueryBlocksResponseToJsonRpc(firstPage.(*bdsevm.QueryBlocksResponse))
	case "eth_querytransactions":
		result = bdsevm.QueryTransactionsResponseToJsonRpc(firstPage.(*bdsevm.QueryTransactionsResponse))
	case "eth_querylogs":
		result = bdsevm.QueryLogsResponseToJsonRpc(firstPage.(*bdsevm.QueryLogsResponse))
	case "eth_querytraces":
		result = bdsevm.QueryTracesResponseToJsonRpc(firstPage.(*bdsevm.QueryTracesResponse))
	case "eth_querytransfers":
		result = bdsevm.QueryTransfersResponseToJsonRpc(firstPage.(*bdsevm.QueryTransfersResponse))
	default:
		return nil, fmt.Errorf("unsupported query method: %s", method)
	}

	jrr, err := common.NewJsonRpcResponse(jrReq.ID, result, nil)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithRequest(nq).WithJsonRpcResponse(jrr), nil
}
