package clients

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

type GrpcBdsClient interface {
	GetType() ClientType
	SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

type GenericGrpcBdsClient struct {
	Url       *url.URL
	headers   map[string]string
	conn      *grpc.ClientConn
	rpcClient evm.RPCQueryServiceClient

	projectId       string
	upstreamId      string
	appCtx          context.Context
	logger          *zerolog.Logger
	isLogLevelTrace bool
}

func NewGrpcBdsClient(
	appCtx context.Context,
	logger *zerolog.Logger,
	projectId string,
	upstreamId string,
	parsedUrl *url.URL,
) (GrpcBdsClient, error) {
	client := &GenericGrpcBdsClient{
		Url:             parsedUrl,
		appCtx:          appCtx,
		logger:          logger,
		projectId:       projectId,
		upstreamId:      upstreamId,
		isLogLevelTrace: logger.GetLevel() == zerolog.TraceLevel,
		headers:         make(map[string]string),
	}

	// Extract host and port from URL
	// For grpc:// or grpc+bds:// schemes, use the host:port directly
	target := parsedUrl.Host
	if parsedUrl.Port() == "" {
		// Default gRPC port if not specified
		target = fmt.Sprintf("%s:50051", parsedUrl.Hostname())
	}

	// Create gRPC connection
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(100*1024*1024)), // 100MB max message size
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to gRPC server at %s: %w", target, err)
	}

	client.conn = conn
	client.rpcClient = evm.NewRPCQueryServiceClient(conn)

	// Setup graceful shutdown
	go func() {
		<-appCtx.Done()
		client.shutdown()
	}()

	logger.Debug().Str("target", target).Msg("created gRPC BDS client")

	return client, nil
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
			c.upstreamId,
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
	switch jrReq.Method {
	case "eth_getBlockByNumber":
		return c.handleGetBlockByNumber(ctx, req, jrReq)
	case "eth_getBlockByHash":
		return c.handleGetBlockByHash(ctx, req, jrReq)
	case "eth_getLogs":
		return c.handleGetLogs(ctx, req, jrReq)
	default:
		err := fmt.Errorf("unsupported method for gRPC BDS client: %s", jrReq.Method)
		common.SetTraceSpanError(span, err)
		return nil, err
	}
}

func (c *GenericGrpcBdsClient) handleGetBlockByNumber(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	// Parse params: [blockNumber, includeTransactions]
	var params []interface{}
	// Marshal and unmarshal to handle the conversion properly
	paramsBytes, err := sonic.Marshal(jrReq.Params)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}
	if err := sonic.Unmarshal(paramsBytes, &params); err != nil {
		return nil, fmt.Errorf("failed to parse params: %w", err)
	}

	if len(params) < 2 {
		return nil, fmt.Errorf("insufficient params for eth_getBlockByNumber")
	}

	// Parse block number
	blockNumber, ok := params[0].(string)
	if !ok {
		return nil, fmt.Errorf("invalid block number parameter")
	}

	// Parse includeTransactions
	includeTransactions, ok := params[1].(bool)
	if !ok {
		return nil, fmt.Errorf("invalid includeTransactions parameter")
	}

	// Make gRPC call
	grpcReq := &evm.GetBlockByNumberRequest{
		BlockNumber:         blockNumber,
		IncludeTransactions: includeTransactions,
	}

	c.logger.Debug().
		Str("blockNumber", blockNumber).
		Bool("includeTransactions", includeTransactions).
		Msg("calling gRPC GetBlockByNumber")

	grpcResp, err := c.rpcClient.GetBlockByNumber(ctx, grpcReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	// Convert gRPC response to JSON-RPC response
	var result interface{}
	if grpcResp.Block != nil {
		result = convertBlockToJsonRpc(grpcResp.Block, grpcResp.Transactions)
	}

	// Create JSON-RPC response
	jsonRpcResp := &common.JsonRpcResponse{}
	jsonRpcResp.SetID(jrReq.ID)

	if result != nil {
		resultBytes, err := sonic.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal result: %w", err)
		}
		jsonRpcResp.Result = resultBytes
	} else {
		// null result for not found
		jsonRpcResp.Result = []byte("null")
	}

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jsonRpcResp), nil
}

func (c *GenericGrpcBdsClient) handleGetBlockByHash(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	// Parse params: [blockHash, includeTransactions]
	var params []interface{}
	// Marshal and unmarshal to handle the conversion properly
	paramsBytes, err := sonic.Marshal(jrReq.Params)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}
	if err := sonic.Unmarshal(paramsBytes, &params); err != nil {
		return nil, fmt.Errorf("failed to parse params: %w", err)
	}

	if len(params) < 2 {
		return nil, fmt.Errorf("insufficient params for eth_getBlockByHash")
	}

	// Parse block hash
	blockHashStr, ok := params[0].(string)
	if !ok {
		return nil, fmt.Errorf("invalid block hash parameter")
	}

	blockHash, err := parseHexBytes(blockHashStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse block hash: %w", err)
	}

	// Parse includeTransactions
	includeTransactions, ok := params[1].(bool)
	if !ok {
		return nil, fmt.Errorf("invalid includeTransactions parameter")
	}

	// Make gRPC call
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

	// Convert gRPC response to JSON-RPC response
	var result interface{}
	if grpcResp.Block != nil {
		result = convertBlockToJsonRpc(grpcResp.Block, grpcResp.Transactions)
	}

	// Create JSON-RPC response
	jsonRpcResp := &common.JsonRpcResponse{}
	jsonRpcResp.SetID(jrReq.ID)

	if result != nil {
		resultBytes, err := sonic.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal result: %w", err)
		}
		jsonRpcResp.Result = resultBytes
	} else {
		// null result for not found
		jsonRpcResp.Result = []byte("null")
	}

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jsonRpcResp), nil
}

func (c *GenericGrpcBdsClient) handleGetLogs(ctx context.Context, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	// Parse params: [{fromBlock, toBlock, address/addresses, topics}]
	var params []map[string]interface{}
	// Marshal and unmarshal to handle the conversion properly
	paramsBytes, err := sonic.Marshal(jrReq.Params)
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

	// Parse from/to blocks
	var fromBlock, toBlock *uint64
	if fromStr, ok := filterParams["fromBlock"].(string); ok {
		if fromStr != "latest" && fromStr != "pending" && fromStr != "earliest" {
			from, err := parseHexUint64(strings.TrimPrefix(fromStr, "0x"))
			if err != nil {
				return nil, fmt.Errorf("failed to parse fromBlock: %w", err)
			}
			fromBlock = &from
		}
	}

	if toStr, ok := filterParams["toBlock"].(string); ok {
		if toStr != "latest" && toStr != "pending" && toStr != "earliest" {
			to, err := parseHexUint64(strings.TrimPrefix(toStr, "0x"))
			if err != nil {
				return nil, fmt.Errorf("failed to parse toBlock: %w", err)
			}
			toBlock = &to
		}
	}

	// For now, require explicit block numbers
	if fromBlock == nil || toBlock == nil {
		return nil, fmt.Errorf("special block numbers not yet supported via gRPC for eth_getLogs")
	}

	// Parse addresses
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

	// Parse topics
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

	// Make gRPC call
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

	grpcResp, err := c.rpcClient.GetLogs(ctx, grpcReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	// Convert logs to JSON-RPC format
	var result []interface{}
	for _, log := range grpcResp.Logs {
		result = append(result, convertLogToJsonRpc(log))
	}

	// Create JSON-RPC response
	jsonRpcResp := &common.JsonRpcResponse{}
	jsonRpcResp.SetID(jrReq.ID)

	resultBytes, err := sonic.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal result: %w", err)
	}
	jsonRpcResp.Result = resultBytes

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jsonRpcResp), nil
}

func (c *GenericGrpcBdsClient) shutdown() {
	if c.conn != nil {
		c.conn.Close()
	}
}

// Helper functions for conversion

func parseHexUint64(hexStr string) (uint64, error) {
	hexStr = strings.TrimPrefix(hexStr, "0x")
	var value uint64
	_, err := fmt.Sscanf(hexStr, "%x", &value)
	return value, err
}

func parseHexBytes(hexStr string) ([]byte, error) {
	hexStr = strings.TrimPrefix(hexStr, "0x")
	return hex.DecodeString(hexStr)
}

func convertBlockToJsonRpc(block *evm.BlockHeader, transactions [][]byte) map[string]interface{} {
	result := map[string]interface{}{
		"number":           fmt.Sprintf("0x%x", block.Number),
		"hash":             fmt.Sprintf("0x%x", block.Hash),
		"parentHash":       fmt.Sprintf("0x%x", block.ParentHash),
		"nonce":            fmt.Sprintf("0x%016x", block.Nonce),
		"sha3Uncles":       fmt.Sprintf("0x%x", block.Sha3Uncles),
		"logsBloom":        fmt.Sprintf("0x%x", block.LogsBloom),
		"transactionsRoot": fmt.Sprintf("0x%x", block.TransactionsRoot),
		"stateRoot":        fmt.Sprintf("0x%x", block.StateRoot),
		"receiptsRoot":     fmt.Sprintf("0x%x", block.ReceiptsRoot),
		"miner":            fmt.Sprintf("0x%x", block.Miner),
		"difficulty":       block.Difficulty,
		"totalDifficulty":  block.TotalDifficulty,
		"extraData":        fmt.Sprintf("0x%x", block.ExtraData),
		"size":             fmt.Sprintf("0x%x", block.Size),
		"gasLimit":         fmt.Sprintf("0x%x", block.GasLimit),
		"gasUsed":          fmt.Sprintf("0x%x", block.GasUsed),
		"timestamp":        fmt.Sprintf("0x%x", block.Timestamp),
	}

	// Add optional fields if present
	if block.BaseFeePerGas != nil {
		result["baseFeePerGas"] = block.BaseFeePerGas
	}
	if block.MixHash != nil {
		result["mixHash"] = fmt.Sprintf("0x%x", block.MixHash)
	}
	if block.WithdrawalsRoot != nil {
		result["withdrawalsRoot"] = fmt.Sprintf("0x%x", block.WithdrawalsRoot)
	}
	if block.BlobGasUsed != nil {
		result["blobGasUsed"] = fmt.Sprintf("0x%x", *block.BlobGasUsed)
	}
	if block.ExcessBlobGas != nil {
		result["excessBlobGas"] = fmt.Sprintf("0x%x", *block.ExcessBlobGas)
	}
	if block.ParentBeaconBlockRoot != nil {
		result["parentBeaconBlockRoot"] = fmt.Sprintf("0x%x", block.ParentBeaconBlockRoot)
	}

	// Handle transactions
	if transactions != nil {
		// For now, just include transaction hashes
		// Full transaction objects would require more complex conversion
		txList := make([]string, len(transactions))
		for i, tx := range transactions {
			// Assuming transactions are hashes when includeTransactions is false
			txList[i] = fmt.Sprintf("0x%x", tx)
		}
		result["transactions"] = txList
	} else {
		result["transactions"] = []interface{}{}
	}

	// Add uncles
	if block.Uncles != nil {
		uncleList := make([]string, len(block.Uncles))
		for i, uncle := range block.Uncles {
			uncleList[i] = fmt.Sprintf("0x%x", uncle)
		}
		result["uncles"] = uncleList
	} else {
		result["uncles"] = []interface{}{}
	}

	return result
}

func convertLogToJsonRpc(log *evm.Log) map[string]interface{} {
	topics := make([]string, len(log.Topics))
	for i, topic := range log.Topics {
		topics[i] = fmt.Sprintf("0x%x", topic)
	}

	result := map[string]interface{}{
		"address":          fmt.Sprintf("0x%x", log.Address),
		"topics":           topics,
		"data":             fmt.Sprintf("0x%x", log.Data),
		"blockNumber":      fmt.Sprintf("0x%x", log.BlockNumber),
		"transactionHash":  fmt.Sprintf("0x%x", log.TransactionHash),
		"transactionIndex": fmt.Sprintf("0x%x", log.TransactionIndex),
		"blockHash":        fmt.Sprintf("0x%x", log.BlockHash),
		"logIndex":         fmt.Sprintf("0x%x", log.LogIndex),
		"removed":          false, // Always false for confirmed logs
	}

	return result
}
