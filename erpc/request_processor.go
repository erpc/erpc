package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
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
	start := time.Now()
	ctx, span := common.StartSpan(ctx, "QueryStream.Handle")
	defer span.End()

	project, err := rp.erpc.GetProject(input.ProjectId)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	method := queryMethodFromProto(queryReq)
	networkID := fmt.Sprintf("%s:%s", input.Architecture, input.ChainId)

	span.SetAttributes(
		attribute.String("query.method", method),
		attribute.String("project.id", input.ProjectId),
		attribute.String("network.id", networkID),
	)

	lg := rp.logger.With().
		Str("component", "queryStream").
		Str("projectId", input.ProjectId).
		Str("networkId", networkID).
		Str("method", method).
		Str("clientIP", input.ClientIP).
		Logger()

	lg.Info().Msgf("processing query stream request")

	nq := common.NewNormalizedRequestFromJsonRpcRequest(
		common.NewJsonRpcRequest(method, []interface{}{}),
	)
	nq.SetClientIP(input.ClientIP)
	if input.UserAgent != "" {
		nq.SetAgentName(input.UserAgent)
	}

	user, err := project.AuthenticateConsumer(ctx, nq, method, input.AuthPayload)
	if err != nil {
		lg.Debug().Err(err).Msgf("query stream authentication failed")
		common.SetTraceSpanError(span, err)
		return err
	}
	nq.SetUser(user)

	network, err := project.GetNetwork(ctx, networkID)
	if err != nil {
		lg.Debug().Err(err).Msgf("failed to resolve network for query stream")
		common.SetTraceSpanError(span, err)
		return err
	}
	nq.SetNetwork(network)

	if err := project.AcquireRateLimitPermit(ctx, nq); err != nil {
		lg.Debug().Err(err).Msgf("query stream rate limited")
		common.SetTraceSpanError(span, err)
		return err
	}

	executor := NewEvmQueryExecutor(network, &lg)
	executor.parentRequestId = nq.ID()
	err = executor.Execute(ctx, queryReq, onPage)

	dur := time.Since(start)
	if err != nil {
		lg.Info().Err(err).Dur("durationMs", dur).Msgf("query stream completed with error")
		common.SetTraceSpanError(span, err)
	} else {
		lg.Info().Dur("durationMs", dur).Msgf("query stream completed successfully")
	}

	return err
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
