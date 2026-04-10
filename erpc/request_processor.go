package erpc

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

type RequestProcessor struct {
	erpc   *ERPC
	logger *zerolog.Logger
}

type RequestInput struct {
	ProjectId    string
	Architecture string
	ChainId      string
	AuthPayload  *auth.AuthPayload
	ClientIP     string
	UserAgent    string
}

func NewRequestProcessor(erpc *ERPC, logger *zerolog.Logger) *RequestProcessor {
	return &RequestProcessor{erpc: erpc, logger: logger}
}

func (rp *RequestProcessor) ProcessUnary(
	ctx context.Context,
	input *RequestInput,
	rawJSONRPC json.RawMessage,
) (*common.NormalizedResponse, error) {
	project, err := rp.erpc.GetProject(input.ProjectId)
	if err != nil {
		return nil, err
	}

	nq := common.NewNormalizedRequest(rawJSONRPC)
	if err := nq.Validate(); err != nil {
		return nil, err
	}

	nq.SetClientIP(input.ClientIP)
	method, _ := nq.Method()
	user, err := project.AuthenticateConsumer(ctx, nq, method, input.AuthPayload)
	if err != nil {
		return nil, err
	}
	nq.SetUser(user)

	networkID := fmt.Sprintf("%s:%s", input.Architecture, input.ChainId)
	network, err := project.GetNetwork(ctx, networkID)
	if err != nil {
		return nil, err
	}
	nq.SetNetwork(network)
	nq.ApplyDirectiveDefaults(network.Config().DirectiveDefaults)

	return project.Forward(ctx, networkID, nq)
}

func (rp *RequestProcessor) ProcessQueryStream(
	ctx context.Context,
	input *RequestInput,
	queryReq proto.Message,
	onPage func(proto.Message) error,
) error {
	project, err := rp.erpc.GetProject(input.ProjectId)
	if err != nil {
		return err
	}

	method := queryMethodFromProto(queryReq)
	authReq := common.NewNormalizedRequest(buildJSONRPCRequest(method, []interface{}{}))
	authReq.SetClientIP(input.ClientIP)
	if _, err := project.AuthenticateConsumer(ctx, authReq, method, input.AuthPayload); err != nil {
		return err
	}

	networkID := fmt.Sprintf("%s:%s", input.Architecture, input.ChainId)
	network, err := project.GetNetwork(ctx, networkID)
	if err != nil {
		return err
	}

	executor := NewQueryExecutor(network, rp.logger)
	return executor.Execute(ctx, queryReq, onPage)
}

func queryMethodFromProto(req proto.Message) string {
	switch req.(type) {
	case *evm.QueryBlocksRequest:
		return "eth_queryBlocks"
	case *evm.QueryTransactionsRequest:
		return "eth_queryTransactions"
	case *evm.QueryLogsRequest:
		return "eth_queryLogs"
	case *evm.QueryTracesRequest:
		return "eth_queryTraces"
	case *evm.QueryTransfersRequest:
		return "eth_queryTransfers"
	default:
		return "unknown"
	}
}
