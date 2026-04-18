package clients

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "github.com/blockchain-data-standards/manifesto/common"
	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	// Import gzip to register the compressor - enables automatic gzip compression
	// when clients send "grpc-accept-encoding: gzip" header
	_ "google.golang.org/grpc/encoding/gzip"
)

type GrpcBdsClient interface {
	GetType() ClientType
	SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
	SetHeaders(h map[string]string)
	QueryClient() evm.QueryServiceClient
}

type GenericGrpcBdsClient struct {
	Url         *url.URL
	headers     map[string]string
	conn        *grpc.ClientConn
	rpcClient   evm.RPCQueryServiceClient
	queryClient evm.QueryServiceClient

	projectId       string
	upstream        common.Upstream
	upstreamId      string
	appCtx          context.Context
	logger          *zerolog.Logger
	isLogLevelTrace bool
}

func NewGrpcBdsClient(
	appCtx context.Context,
	logger *zerolog.Logger,
	projectId string,
	upstream common.Upstream,
	parsedUrl *url.URL,
) (GrpcBdsClient, error) {
	upsId := "n/a"
	if upstream != nil {
		upsId = upstream.Id()
	}
	client := &GenericGrpcBdsClient{
		Url:             parsedUrl,
		appCtx:          appCtx,
		logger:          logger,
		projectId:       projectId,
		upstream:        upstream,
		upstreamId:      upsId,
		isLogLevelTrace: logger.GetLevel() == zerolog.TraceLevel,
		headers:         make(map[string]string),
	}

	// Extract host and port from URL
	target := parsedUrl.Host
	if parsedUrl.Port() == "" {
		target = fmt.Sprintf("%s:50051", parsedUrl.Hostname())
	}

	// Use dns:/// prefix so gRPC resolves all A records (e.g. Kubernetes headless services)
	// and round_robin distributes RPCs across them. For single-target hosts this is a no-op.
	target = fmt.Sprintf("dns:///%s", target)

	// Determine whether to use TLS based on port or URL scheme
	var transportCredentials credentials.TransportCredentials
	port := parsedUrl.Port()
	portNum, portErr := strconv.Atoi(port)

	// Use TLS if:
	// 1. Port is 443 (standard HTTPS port)
	// 2. URL scheme suggests TLS (grpcs://, grpc+tls://, etc.)
	// 3. URL scheme is grpc:// but uses port 443
	useTLS := false
	if portErr == nil && portNum == 443 {
		useTLS = true
	} else if strings.HasPrefix(parsedUrl.Scheme, "grpcs") || strings.Contains(parsedUrl.Scheme, "tls") {
		useTLS = true
	}

	if useTLS {
		// Use TLS credentials with system's trusted CA certificates
		transportCredentials = credentials.NewTLS(&tls.Config{
			ServerName: parsedUrl.Hostname(),
			MinVersion: tls.VersionTLS12,
		})
		logger.Debug().Str("target", target).Msg("using TLS credentials for gRPC connection")
	} else {
		// Use insecure credentials
		transportCredentials = insecure.NewCredentials()
		logger.Debug().Str("target", target).Msg("using insecure credentials for gRPC connection")
	}

	// gRPC service config: round_robin distributes RPCs across all resolved addresses
	// (no-op for single-target hosts). Transparent retries handle transient failures
	// (UNAVAILABLE from connection resets, TCP retransmits) without surfacing errors
	// to callers. WaitForReady queues RPCs during brief reconnects instead of failing
	// immediately with UNAVAILABLE.
	serviceConfig := `{
		"loadBalancingConfig": [{"round_robin":{}}],
		"methodConfig": [{
			"name": [{"service": ""}],
			"waitForReady": true,
			"retryPolicy": {
				"maxAttempts": 2,
				"initialBackoff": "1s",
				"maxBackoff": "5s",
				"backoffMultiplier": 2,
				"retryableStatusCodes": ["UNAVAILABLE"]
			}
		}]
	}`

	// Create gRPC connection with aggressive timeouts suitable for cache services
	// These should fail fast to allow failover to other upstreams
	conn, err := grpc.NewClient(target,
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithTransportCredentials(transportCredentials),
		grpc.WithChainUnaryInterceptor(grpcResponseMetadataInterceptor()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(100*1024*1024),
			grpc.MaxCallSendMsgSize(100*1024*1024),
		),
		grpc.WithDefaultServiceConfig(serviceConfig),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second, // Detect dead connections faster (was 2min)
			Timeout:             5 * time.Second,  // Fail fast on dead connections
			PermitWithoutStream: true,             // Keep connection warm even during idle periods
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			MinConnectTimeout: 3 * time.Second, // Cross-region TLS through Fly Anycast can take 400-600ms; 500ms caused mid-handshake aborts
			Backoff: backoff.Config{
				BaseDelay:  100 * time.Millisecond, // Give proxy breathing room between reconnect attempts
				Multiplier: 1.5,
				Jitter:     0.2,
				MaxDelay:   1 * time.Second, // Allow longer backoff to reduce connection churn
			},
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to gRPC server at %s: %w", target, err)
	}

	client.conn = conn
	client.rpcClient = evm.NewRPCQueryServiceClient(conn)
	client.queryClient = evm.NewQueryServiceClient(conn)

	// Setup graceful shutdown
	go func() {
		<-appCtx.Done()
		client.shutdown()
	}()

	logger.Debug().Str("target", target).Msg("created gRPC BDS client")

	return client, nil
}

// grpcResponseMetadataInterceptor captures all response metadata (headers)
// from gRPC calls and records them as span attributes on detailed traces.
func grpcResponseMetadataInterceptor() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		if !common.IsTracingDetailed {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		span := trace.SpanFromContext(ctx)
		if !span.IsRecording() {
			return invoker(ctx, method, req, reply, cc, opts...)
		}

		var respMD metadata.MD
		opts = append(opts, grpc.Header(&respMD))

		err := invoker(ctx, method, req, reply, cc, opts...)

		for key, vals := range respMD {
			if len(vals) == 1 {
				span.SetAttributes(attribute.String("grpc.response.metadata."+key, vals[0]))
			} else if len(vals) > 1 {
				span.SetAttributes(attribute.StringSlice("grpc.response.metadata."+key, vals))
			}
		}

		return err
	}
}

func (c *GenericGrpcBdsClient) GetType() ClientType {
	return ClientTypeGrpcBds
}

func (c *GenericGrpcBdsClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	ctx, span := common.StartSpan(ctx, "GrpcBdsClient.SendRequest",
		trace.WithAttributes(
			attribute.String("network.id", req.NetworkId()),
			attribute.String("upstream.id", c.upstreamId),
		),
	)
	defer span.End()

	// Fail fast if context is already canceled or expired
	if err := ctx.Err(); err != nil {
		common.SetTraceSpanError(span, err)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, common.NewErrEndpointRequestTimeout(0, err)
		}
		return nil, common.NewErrEndpointRequestCanceled(err)
	}

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("request.id", fmt.Sprintf("%v", req.ID())),
		)
	}

	jrReq, err := req.JsonRpcRequest()
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, common.NewErrUpstreamRequest(
			err,
			c.upstream,
			req.NetworkId(),
			"",
			0, 0, 0, 0,
		)
	}

	span.SetAttributes(attribute.String("request.method", jrReq.Method))

	// Add headers to context if any
	if len(c.headers) > 0 {
		md := metadata.New(c.headers)
		ctx = metadata.NewOutgoingContext(ctx, md)
	}

	// Route to appropriate handler based on method
	var resp *common.NormalizedResponse
	switch jrReq.Method {
	case "eth_getBlockByNumber":
		resp, err = c.handleGetBlockByNumber(ctx, req, jrReq)
	case "eth_getBlockByHash":
		resp, err = c.handleGetBlockByHash(ctx, req, jrReq)
	case "eth_getLogs":
		resp, err = c.handleGetLogs(ctx, req, jrReq)
	case "eth_getTransactionByHash":
		resp, err = c.handleGetTransactionByHash(ctx, req, jrReq)
	case "eth_getTransactionReceipt":
		resp, err = c.handleGetTransactionReceipt(ctx, req, jrReq)
	case "eth_getBlockReceipts":
		resp, err = c.handleGetBlockReceipts(ctx, req, jrReq)
	case "eth_chainId":
		resp, err = c.handleChainId(ctx, req, jrReq)
	case "eth_queryBlocks":
		resp, err = c.handleQueryBlocks(ctx, req, jrReq)
	case "eth_queryTransactions":
		resp, err = c.handleQueryTransactions(ctx, req, jrReq)
	case "eth_queryLogs":
		resp, err = c.handleQueryLogs(ctx, req, jrReq)
	case "eth_queryTraces":
		resp, err = c.handleQueryTraces(ctx, req, jrReq)
	case "eth_queryTransfers":
		resp, err = c.handleQueryTransfers(ctx, req, jrReq)
	default:
		err := common.NewErrEndpointUnsupported(
			fmt.Errorf("unsupported method for gRPC BDS client: %s", jrReq.Method),
		)
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	// TODO Distinguish between different architectures as a property on GenericGrpcBdsClient during initialization
	// TODO Move the logic to evm package as a post-response hook?
	if err != nil {
		return nil, c.normalizeGrpcError(err)
	}

	return resp, nil
}

func (c *GenericGrpcBdsClient) handleGetBlockByNumber(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	var params []interface{}
	jrReq.RLock()
	paramsBytes, err := sonic.Marshal(jrReq.Params)
	jrReq.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}
	if err := sonic.Unmarshal(paramsBytes, &params); err != nil {
		return nil, fmt.Errorf("failed to parse params: %w", err)
	}

	if len(params) < 2 {
		return nil, fmt.Errorf("insufficient params for eth_getBlockByNumber")
	}

	// Parse the block parameter (could be number, hash, or object with blockHash)
	blockNumber, blockHash, err := util.ParseBlockParameter(params[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse block parameter: %w", err)
	}

	includeTransactions, ok := params[1].(bool)
	if !ok {
		return nil, fmt.Errorf("invalid includeTransactions parameter")
	}

	// If we have a blockHash, use GetBlockByHash instead
	if blockHash != nil {
		grpcReq := &evm.GetBlockByHashRequest{
			BlockHash:           blockHash,
			IncludeTransactions: includeTransactions,
		}

		hashHex := hex.EncodeToString(blockHash)
		c.logger.Debug().
			Str("blockHash", hashHex).
			Interface("originalParam", params[0]).
			Bool("includeTransactions", includeTransactions).
			Msg("calling gRPC GetBlockByHash (from eth_getBlockByNumber with blockHash)")

		ctx, grpcHashSpan := common.StartDetailSpan(ctx, "GrpcBdsClient.GetBlockByHash",
			trace.WithAttributes(
				attribute.String("block_hash", hashHex),
				attribute.String("original_param", fmt.Sprintf("%v", params[0])),
			),
		)
		grpcResp, err := c.rpcClient.GetBlockByHash(ctx, grpcReq)
		if err != nil {
			grpcHashSpan.SetAttributes(attribute.String("grpc_error", err.Error()))
			common.SetTraceSpanError(grpcHashSpan, err)
			grpcHashSpan.End()
			return nil, fmt.Errorf("gRPC call failed: %w", err)
		}
		grpcHashSpan.SetAttributes(attribute.Bool("response_has_block", grpcResp.Block != nil))
		grpcHashSpan.End()

		var result interface{}
		if grpcResp.Block != nil {
			result = evm.BlockToJsonRpc(grpcResp.Block, grpcResp.Transactions, grpcResp.FullTransactions, grpcResp.Withdrawals)
		}

		jsonRpcResp := &common.JsonRpcResponse{}
		err = jsonRpcResp.SetID(jrReq.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to set ID: %w", err)
		}

		if result != nil {
			resultBytes, err := sonic.Marshal(result)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal result: %w", err)
			}
			jsonRpcResp.SetResult(resultBytes)
		} else {
			jsonRpcResp.SetResult([]byte("null"))
		}

		return common.NewNormalizedResponse().
			WithRequest(req).
			WithJsonRpcResponse(jsonRpcResp), nil
	}

	// Otherwise, use GetBlockByNumber
	grpcReq := &evm.GetBlockByNumberRequest{
		BlockNumber:         blockNumber,
		IncludeTransactions: includeTransactions,
	}

	c.logger.Debug().
		Str("blockNumber", blockNumber).
		Interface("originalParam", params[0]).
		Bool("includeTransactions", includeTransactions).
		Msg("calling gRPC GetBlockByNumber")

	isTag := blockNumber == "latest" || blockNumber == "earliest" || blockNumber == "finalized" || blockNumber == "safe" || blockNumber == "pending"
	ctx, grpcSpan := common.StartDetailSpan(ctx, "GrpcBdsClient.GetBlockByNumber",
		trace.WithAttributes(
			attribute.String("block_number", blockNumber),
			attribute.Bool("is_block_tag", isTag),
			attribute.String("original_param", fmt.Sprintf("%v", params[0])),
		),
	)
	grpcResp, err := c.rpcClient.GetBlockByNumber(ctx, grpcReq)
	if err != nil {
		grpcSpan.SetAttributes(attribute.String("grpc_error", err.Error()))
		common.SetTraceSpanError(grpcSpan, err)
		grpcSpan.End()
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}
	hasBlock := grpcResp.Block != nil
	grpcSpan.SetAttributes(attribute.Bool("response_has_block", hasBlock))
	if hasBlock {
		grpcSpan.SetAttributes(
			attribute.Int64("response_block_number", int64(grpcResp.Block.Number)),
			attribute.String("response_block_hash", fmt.Sprintf("%x", grpcResp.Block.Hash)),
		)
	}
	grpcSpan.End()

	var result interface{}
	if hasBlock {
		result = evm.BlockToJsonRpc(grpcResp.Block, grpcResp.Transactions, grpcResp.FullTransactions, grpcResp.Withdrawals)
	}

	jsonRpcResp := &common.JsonRpcResponse{}
	err = jsonRpcResp.SetID(jrReq.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to set ID: %w", err)
	}

	if result != nil {
		resultBytes, err := sonic.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal result: %w", err)
		}
		jsonRpcResp.SetResult(resultBytes)
	} else {
		jsonRpcResp.SetResult([]byte("null"))
	}

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jsonRpcResp), nil
}

func (c *GenericGrpcBdsClient) handleGetBlockByHash(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	var params []interface{}
	jrReq.RLock()
	paramsBytes, err := sonic.Marshal(jrReq.Params)
	jrReq.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}
	if err := sonic.Unmarshal(paramsBytes, &params); err != nil {
		return nil, fmt.Errorf("failed to parse params: %w", err)
	}

	if len(params) < 2 {
		return nil, fmt.Errorf("insufficient params for eth_getBlockByHash")
	}

	blockHashStr, ok := params[0].(string)
	if !ok {
		return nil, fmt.Errorf("invalid block hash parameter")
	}

	blockHash, err := util.ParseBlockHashHexToBytes(blockHashStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse block hash: %w", err)
	}

	includeTransactions, ok := params[1].(bool)
	if !ok {
		return nil, fmt.Errorf("invalid includeTransactions parameter")
	}

	grpcReq := &evm.GetBlockByHashRequest{
		BlockHash:           blockHash,
		IncludeTransactions: includeTransactions,
	}

	c.logger.Debug().
		Str("blockHash", blockHashStr).
		Bool("includeTransactions", includeTransactions).
		Msg("calling gRPC GetBlockByHash")

	grpcResp, err := c.rpcClient.GetBlockByHash(ctx, grpcReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	var result interface{}
	if grpcResp.Block != nil {
		result = evm.BlockToJsonRpc(grpcResp.Block, grpcResp.Transactions, grpcResp.FullTransactions, grpcResp.Withdrawals)
	}

	jsonRpcResp := &common.JsonRpcResponse{}
	err = jsonRpcResp.SetID(jrReq.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to set ID: %w", err)
	}

	if result != nil {
		resultBytes, err := sonic.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal result: %w", err)
		}
		jsonRpcResp.SetResult(resultBytes)
	} else {
		jsonRpcResp.SetResult([]byte("null"))
	}

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jsonRpcResp), nil
}

func (c *GenericGrpcBdsClient) handleGetLogs(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	var params []map[string]interface{}
	jrReq.RLock()
	paramsBytes, err := sonic.Marshal(jrReq.Params)
	jrReq.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}
	if err := sonic.Unmarshal(paramsBytes, &params); err != nil {
		return nil, fmt.Errorf("failed to parse params: %w", err)
	}

	if len(params) < 1 {
		return nil, fmt.Errorf("insufficient params for eth_getLogs")
	}

	filterParams := params[0]

	var fromBlock, toBlock *uint64
	if fromStr, ok := filterParams["fromBlock"].(string); ok {
		if fromStr != "latest" && fromStr != "pending" && fromStr != "earliest" {
			fromParsed, err := evm.HexToUint64(fromStr)
			if err != nil {
				return nil, fmt.Errorf("failed to parse fromBlock: %w", err)
			}
			fromBlock = &fromParsed
		}
	}

	if toStr, ok := filterParams["toBlock"].(string); ok {
		if toStr != "latest" && toStr != "pending" && toStr != "earliest" {
			toParsed, err := evm.HexToUint64(toStr)
			if err != nil {
				return nil, fmt.Errorf("failed to parse toBlock: %w", err)
			}
			toBlock = &toParsed
		}
	}

	if fromBlock == nil || toBlock == nil {
		return nil, fmt.Errorf("special block numbers not yet supported via gRPC for eth_getLogs")
	}

	var addresses [][]byte
	if addrParam, ok := filterParams["address"]; ok {
		switch v := addrParam.(type) {
		case string:
			addr, err := parseHexBytes(v)
			if err != nil {
				return nil, fmt.Errorf("failed to parse address: %w", err)
			}
			addresses = append(addresses, addr)
		case []interface{}:
			for _, a := range v {
				if addrStr, ok := a.(string); ok {
					addr, err := parseHexBytes(addrStr)
					if err != nil {
						return nil, fmt.Errorf("failed to parse address: %w", err)
					}
					addresses = append(addresses, addr)
				}
			}
		}
	}

	var topics []*evm.TopicFilter
	if topicsParam, ok := filterParams["topics"].([]interface{}); ok {
		for _, topicParam := range topicsParam {
			topicFilter := &evm.TopicFilter{}

			switch v := topicParam.(type) {
			case string:
				// Single topic value
				topic, err := parseHexBytes(v)
				if err != nil {
					return nil, fmt.Errorf("failed to parse topic: %w", err)
				}
				topicFilter.Values = append(topicFilter.Values, topic)
			case []interface{}:
				// Multiple possible values for this topic position
				for _, t := range v {
					if topicStr, ok := t.(string); ok {
						topic, err := parseHexBytes(topicStr)
						if err != nil {
							return nil, fmt.Errorf("failed to parse topic: %w", err)
						}
						topicFilter.Values = append(topicFilter.Values, topic)
					}
				}
			case nil:
				// null topic means any value at this position
				continue
			}

			topics = append(topics, topicFilter)
		}
	}

	grpcReq := &evm.GetLogsRequest{
		FromBlock: fromBlock,
		ToBlock:   toBlock,
		Addresses: addresses,
		Topics:    topics,
	}

	c.logger.Debug().
		Uint64("fromBlock", *fromBlock).
		Uint64("toBlock", *toBlock).
		Int("addressCount", len(addresses)).
		Int("topicCount", len(topics)).
		Msg("calling gRPC GetLogs")

	ctx, grpcSpan := common.StartDetailSpan(ctx, "GrpcBdsClient.GetLogs",
		trace.WithAttributes(
			attribute.Int64("from_block", int64(*fromBlock)),
			attribute.Int64("to_block", int64(*toBlock)),
		),
	)
	grpcResp, err := c.rpcClient.GetLogs(ctx, grpcReq)
	grpcSpan.End()
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	var result []interface{}
	for _, log := range grpcResp.Logs {
		result = append(result, evm.LogToJsonRpc(log))
	}

	jsonRpcResp := &common.JsonRpcResponse{}
	jrReq.RLock()
	err = jsonRpcResp.SetID(jrReq.ID)
	jrReq.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("failed to set ID: %w", err)
	}

	if len(result) == 0 {
		jsonRpcResp.SetResult([]byte("[]"))
	} else {
		resultBytes, err := sonic.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal result: %w", err)
		}
		jsonRpcResp.SetResult(resultBytes)
	}

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jsonRpcResp), nil
}

func (c *GenericGrpcBdsClient) handleGetTransactionByHash(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	var params []interface{}
	jrReq.RLock()
	paramsBytes, err := sonic.Marshal(jrReq.Params)
	jrReq.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}
	if err := sonic.Unmarshal(paramsBytes, &params); err != nil {
		return nil, fmt.Errorf("failed to parse params: %w", err)
	}

	if len(params) < 1 {
		return nil, fmt.Errorf("insufficient params for eth_getTransactionByHash")
	}

	txHashStr, ok := params[0].(string)
	if !ok {
		return nil, fmt.Errorf("invalid transaction hash parameter")
	}

	txHash, err := parseHexBytes(txHashStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transaction hash: %w", err)
	}

	grpcReq := &evm.GetTransactionByHashRequest{
		TransactionHash: txHash,
	}

	c.logger.Debug().
		Str("transactionHash", txHashStr).
		Msg("calling gRPC GetTransactionByHash")

	grpcResp, err := c.rpcClient.GetTransactionByHash(ctx, grpcReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	var result interface{}
	if grpcResp.Transaction != nil {
		result = evm.TransactionToJsonRpc(grpcResp.Transaction)
	}

	jsonRpcResp := &common.JsonRpcResponse{}
	err = jsonRpcResp.SetID(jrReq.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to set ID: %w", err)
	}

	if result != nil {
		resultBytes, err := sonic.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal result: %w", err)
		}
		jsonRpcResp.SetResult(resultBytes)
	} else {
		jsonRpcResp.SetResult([]byte("null"))
	}

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jsonRpcResp), nil
}

func (c *GenericGrpcBdsClient) handleGetTransactionReceipt(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	var params []interface{}
	jrReq.RLock()
	paramsBytes, err := sonic.Marshal(jrReq.Params)
	jrReq.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}
	if err := sonic.Unmarshal(paramsBytes, &params); err != nil {
		return nil, fmt.Errorf("failed to parse params: %w", err)
	}

	if len(params) < 1 {
		return nil, fmt.Errorf("insufficient params for eth_getTransactionReceipt")
	}

	txHashStr, ok := params[0].(string)
	if !ok {
		return nil, fmt.Errorf("invalid transaction hash parameter")
	}

	txHash, err := parseHexBytes(txHashStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transaction hash: %w", err)
	}

	grpcReq := &evm.GetTransactionReceiptRequest{
		TransactionHash: txHash,
	}

	c.logger.Debug().
		Str("transactionHash", txHashStr).
		Msg("calling gRPC GetTransactionReceipt")

	grpcResp, err := c.rpcClient.GetTransactionReceipt(ctx, grpcReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	var result interface{}
	if grpcResp.Receipt != nil {
		result = evm.ReceiptToJsonRpc(grpcResp.Receipt)
	}

	jsonRpcResp := &common.JsonRpcResponse{}
	err = jsonRpcResp.SetID(jrReq.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to set ID: %w", err)
	}

	if result != nil {
		resultBytes, err := sonic.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal result: %w", err)
		}
		jsonRpcResp.SetResult(resultBytes)
	} else {
		jsonRpcResp.SetResult([]byte("null"))
	}

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jsonRpcResp), nil
}

func (c *GenericGrpcBdsClient) handleChainId(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	grpcReq := &evm.ChainIdRequest{}

	c.logger.Debug().
		Msg("calling gRPC ChainId")

	grpcResp, err := c.rpcClient.ChainId(ctx, grpcReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	// Convert chain ID to hex string per JSON-RPC standard
	result := fmt.Sprintf("0x%x", grpcResp.ChainId)

	jsonRpcResp := &common.JsonRpcResponse{}
	err = jsonRpcResp.SetID(jrReq.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to set ID: %w", err)
	}

	resultBytes, err := sonic.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal result: %w", err)
	}
	jsonRpcResp.SetResult(resultBytes)

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jsonRpcResp), nil
}

func (c *GenericGrpcBdsClient) handleGetBlockReceipts(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	var params []interface{}
	jrReq.RLock()
	paramsBytes, err := sonic.Marshal(jrReq.Params)
	jrReq.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}
	if err := sonic.Unmarshal(paramsBytes, &params); err != nil {
		return nil, fmt.Errorf("failed to parse params: %w", err)
	}

	if len(params) < 1 {
		return nil, fmt.Errorf("insufficient params for eth_getBlockReceipts")
	}

	// Parse the block parameter (could be number, hash, or object with blockHash)
	blockNumber, blockHash, err := util.ParseBlockParameter(params[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse block parameter: %w", err)
	}

	grpcReq := &evm.GetBlockReceiptsRequest{}

	// If we got a block hash, use it directly
	if blockHash != nil {
		grpcReq.BlockHash = blockHash
	} else {
		grpcReq.BlockNumber = &blockNumber
	}

	c.logger.Debug().
		Str("blockNumber", blockNumber).
		Interface("originalParam", params[0]).
		Msg("calling gRPC GetBlockReceipts")

	grpcResp, err := c.rpcClient.GetBlockReceipts(ctx, grpcReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	// Convert receipts to JSON-RPC format
	var result []interface{}
	if grpcResp.Receipts != nil {
		result = make([]interface{}, len(grpcResp.Receipts))
		for i, receipt := range grpcResp.Receipts {
			result[i] = evm.ReceiptToJsonRpc(receipt)
		}
	}

	jsonRpcResp := &common.JsonRpcResponse{}
	err = jsonRpcResp.SetID(jrReq.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to set ID: %w", err)
	}

	if len(result) > 0 {
		resultBytes, err := sonic.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal result: %w", err)
		}
		jsonRpcResp.SetResult(resultBytes)
	} else {
		jsonRpcResp.SetResult([]byte("[]"))
	}

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jsonRpcResp), nil
}

func (c *GenericGrpcBdsClient) normalizeGrpcError(err error) error {
	if err == nil {
		return nil
	}

	// status.FromError only checks the top-level error for GRPCStatus().
	// Handler methods wrap gRPC errors with fmt.Errorf, so we walk the
	// Unwrap chain to find the original gRPC status.
	current := err
	for current != nil {
		if st, ok := status.FromError(current); ok {
			return common.ExtractGrpcErrorFromGrpcStatus(st, c.upstream)
		}
		current = errors.Unwrap(current)
	}

	return common.NewErrEndpointTransportFailure(c.Url, err)
}

func (c *GenericGrpcBdsClient) shutdown() {
	if c.conn != nil {
		err := c.conn.Close()
		if err != nil {
			c.logger.Error().Err(err).Msg("failed to close gRPC connection")
		}
	}
}

func (c *GenericGrpcBdsClient) SetHeaders(h map[string]string) {
	if c == nil || h == nil {
		return
	}
	for k, v := range h {
		c.headers[k] = v
	}
}

func (c *GenericGrpcBdsClient) QueryClient() evm.QueryServiceClient {
	if c == nil {
		return nil
	}
	return c.queryClient
}

// Helper functions for conversion

func parseHexBytes(hexStr string) ([]byte, error) {
	return evm.HexToBytes(hexStr)
}

// ensureQueryClient returns an error if the gRPC QueryService client has not
// been wired (e.g. when constructing the client without a live connection).
func (c *GenericGrpcBdsClient) ensureQueryClient(method string) error {
	if c == nil || c.queryClient == nil {
		return fmt.Errorf("%s: gRPC QueryService client not initialized", method)
	}
	return nil
}

// jsonRpcParamsFor extracts params[0] from a JSON-RPC request as a raw JSON
// object, suitable for passing to manifesto's Query*RequestFromJsonRpc helpers.
func jsonRpcParamsFor(jrReq *common.JsonRpcRequest) (json.RawMessage, error) {
	jrReq.RLock()
	defer jrReq.RUnlock()
	if len(jrReq.Params) == 0 {
		return json.RawMessage("{}"), nil
	}
	raw, err := sonic.Marshal(jrReq.Params[0])
	if err != nil {
		return nil, fmt.Errorf("failed to marshal query params: %w", err)
	}
	return raw, nil
}

// buildQueryJsonRpcResponse finalizes a NormalizedResponse from a marshaled
// JSON-RPC result payload for query methods.
func (c *GenericGrpcBdsClient) buildQueryJsonRpcResponse(req *common.NormalizedRequest, jrReq *common.JsonRpcRequest, payload interface{}) (*common.NormalizedResponse, error) {
	resultBytes, err := sonic.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal query result: %w", err)
	}
	jsonRpcResp := &common.JsonRpcResponse{}
	jrReq.RLock()
	if err := jsonRpcResp.SetID(jrReq.ID); err != nil {
		jrReq.RUnlock()
		return nil, fmt.Errorf("failed to set ID: %w", err)
	}
	jrReq.RUnlock()
	jsonRpcResp.SetResult(resultBytes)
	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jsonRpcResp), nil
}

// recvQueryStream drains an upstream query stream and invokes onPage for each
// received response. It returns once the stream is closed (EOF) or on error.
func recvQueryStream[T proto.Message](recv func() (T, error), onPage func(T)) error {
	for {
		page, err := recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		onPage(page)
	}
}

func (c *GenericGrpcBdsClient) handleQueryBlocks(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	if err := c.ensureQueryClient("eth_queryBlocks"); err != nil {
		return nil, err
	}
	rawParams, err := jsonRpcParamsFor(jrReq)
	if err != nil {
		return nil, err
	}
	grpcReq, err := evm.QueryBlocksRequestFromJsonRpc(rawParams)
	if err != nil {
		return nil, fmt.Errorf("invalid eth_queryBlocks params: %w", err)
	}

	ctx, span := common.StartDetailSpan(ctx, "GrpcBdsClient.QueryBlocks")
	defer span.End()
	stream, err := c.queryClient.QueryBlocks(ctx, grpcReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	aggregated := &evm.QueryBlocksResponse{}
	if err := recvQueryStream(stream.Recv, func(page *evm.QueryBlocksResponse) {
		aggregated.Blocks = append(aggregated.Blocks, page.GetBlocks()...)
		if aggregated.FromBlock == nil && page.GetFromBlock() != nil {
			aggregated.FromBlock = page.GetFromBlock()
		}
		if aggregated.ToBlock == nil && page.GetToBlock() != nil {
			aggregated.ToBlock = page.GetToBlock()
		}
		if page.GetCursorBlock() != nil {
			aggregated.CursorBlock = page.GetCursorBlock()
		}
	}); err != nil {
		return nil, fmt.Errorf("gRPC stream error: %w", err)
	}

	return c.buildQueryJsonRpcResponse(req, jrReq, evm.QueryBlocksResponseToJsonRpc(aggregated))
}

func (c *GenericGrpcBdsClient) handleQueryTransactions(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	if err := c.ensureQueryClient("eth_queryTransactions"); err != nil {
		return nil, err
	}
	rawParams, err := jsonRpcParamsFor(jrReq)
	if err != nil {
		return nil, err
	}
	grpcReq, err := evm.QueryTransactionsRequestFromJsonRpc(rawParams)
	if err != nil {
		return nil, fmt.Errorf("invalid eth_queryTransactions params: %w", err)
	}

	ctx, span := common.StartDetailSpan(ctx, "GrpcBdsClient.QueryTransactions")
	defer span.End()
	stream, err := c.queryClient.QueryTransactions(ctx, grpcReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	aggregated := &evm.QueryTransactionsResponse{}
	if err := recvQueryStream(stream.Recv, func(page *evm.QueryTransactionsResponse) {
		aggregated.Transactions = append(aggregated.Transactions, page.GetTransactions()...)
		aggregated.Blocks = append(aggregated.Blocks, page.GetBlocks()...)
		if aggregated.FromBlock == nil && page.GetFromBlock() != nil {
			aggregated.FromBlock = page.GetFromBlock()
		}
		if aggregated.ToBlock == nil && page.GetToBlock() != nil {
			aggregated.ToBlock = page.GetToBlock()
		}
		if page.GetCursorBlock() != nil {
			aggregated.CursorBlock = page.GetCursorBlock()
		}
	}); err != nil {
		return nil, fmt.Errorf("gRPC stream error: %w", err)
	}

	return c.buildQueryJsonRpcResponse(req, jrReq, evm.QueryTransactionsResponseToJsonRpc(aggregated))
}

func (c *GenericGrpcBdsClient) handleQueryLogs(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	if err := c.ensureQueryClient("eth_queryLogs"); err != nil {
		return nil, err
	}
	rawParams, err := jsonRpcParamsFor(jrReq)
	if err != nil {
		return nil, err
	}
	grpcReq, err := evm.QueryLogsRequestFromJsonRpc(rawParams)
	if err != nil {
		return nil, fmt.Errorf("invalid eth_queryLogs params: %w", err)
	}

	ctx, span := common.StartDetailSpan(ctx, "GrpcBdsClient.QueryLogs")
	defer span.End()
	stream, err := c.queryClient.QueryLogs(ctx, grpcReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	aggregated := &evm.QueryLogsResponse{}
	if err := recvQueryStream(stream.Recv, func(page *evm.QueryLogsResponse) {
		aggregated.Logs = append(aggregated.Logs, page.GetLogs()...)
		aggregated.Transactions = append(aggregated.Transactions, page.GetTransactions()...)
		aggregated.Blocks = append(aggregated.Blocks, page.GetBlocks()...)
		if aggregated.FromBlock == nil && page.GetFromBlock() != nil {
			aggregated.FromBlock = page.GetFromBlock()
		}
		if aggregated.ToBlock == nil && page.GetToBlock() != nil {
			aggregated.ToBlock = page.GetToBlock()
		}
		if page.GetCursorBlock() != nil {
			aggregated.CursorBlock = page.GetCursorBlock()
		}
	}); err != nil {
		return nil, fmt.Errorf("gRPC stream error: %w", err)
	}

	return c.buildQueryJsonRpcResponse(req, jrReq, evm.QueryLogsResponseToJsonRpc(aggregated))
}

func (c *GenericGrpcBdsClient) handleQueryTraces(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	if err := c.ensureQueryClient("eth_queryTraces"); err != nil {
		return nil, err
	}
	rawParams, err := jsonRpcParamsFor(jrReq)
	if err != nil {
		return nil, err
	}
	grpcReq, err := evm.QueryTracesRequestFromJsonRpc(rawParams)
	if err != nil {
		return nil, fmt.Errorf("invalid eth_queryTraces params: %w", err)
	}

	ctx, span := common.StartDetailSpan(ctx, "GrpcBdsClient.QueryTraces")
	defer span.End()
	stream, err := c.queryClient.QueryTraces(ctx, grpcReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	aggregated := &evm.QueryTracesResponse{}
	if err := recvQueryStream(stream.Recv, func(page *evm.QueryTracesResponse) {
		aggregated.Traces = append(aggregated.Traces, page.GetTraces()...)
		aggregated.Transactions = append(aggregated.Transactions, page.GetTransactions()...)
		aggregated.Blocks = append(aggregated.Blocks, page.GetBlocks()...)
		if aggregated.FromBlock == nil && page.GetFromBlock() != nil {
			aggregated.FromBlock = page.GetFromBlock()
		}
		if aggregated.ToBlock == nil && page.GetToBlock() != nil {
			aggregated.ToBlock = page.GetToBlock()
		}
		if page.GetCursorBlock() != nil {
			aggregated.CursorBlock = page.GetCursorBlock()
		}
	}); err != nil {
		return nil, fmt.Errorf("gRPC stream error: %w", err)
	}

	return c.buildQueryJsonRpcResponse(req, jrReq, evm.QueryTracesResponseToJsonRpc(aggregated))
}

func (c *GenericGrpcBdsClient) handleQueryTransfers(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	if err := c.ensureQueryClient("eth_queryTransfers"); err != nil {
		return nil, err
	}
	rawParams, err := jsonRpcParamsFor(jrReq)
	if err != nil {
		return nil, err
	}
	grpcReq, err := evm.QueryTransfersRequestFromJsonRpc(rawParams)
	if err != nil {
		return nil, fmt.Errorf("invalid eth_queryTransfers params: %w", err)
	}

	ctx, span := common.StartDetailSpan(ctx, "GrpcBdsClient.QueryTransfers")
	defer span.End()
	stream, err := c.queryClient.QueryTransfers(ctx, grpcReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	aggregated := &evm.QueryTransfersResponse{}
	if err := recvQueryStream(stream.Recv, func(page *evm.QueryTransfersResponse) {
		aggregated.Transfers = append(aggregated.Transfers, page.GetTransfers()...)
		aggregated.Transactions = append(aggregated.Transactions, page.GetTransactions()...)
		aggregated.Blocks = append(aggregated.Blocks, page.GetBlocks()...)
		if aggregated.FromBlock == nil && page.GetFromBlock() != nil {
			aggregated.FromBlock = page.GetFromBlock()
		}
		if aggregated.ToBlock == nil && page.GetToBlock() != nil {
			aggregated.ToBlock = page.GetToBlock()
		}
		if page.GetCursorBlock() != nil {
			aggregated.CursorBlock = page.GetCursorBlock()
		}
	}); err != nil {
		return nil, fmt.Errorf("gRPC stream error: %w", err)
	}

	return c.buildQueryJsonRpcResponse(req, jrReq, evm.QueryTransfersResponseToJsonRpc(aggregated))
}
