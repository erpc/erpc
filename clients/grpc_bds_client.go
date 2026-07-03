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
	"sync/atomic"

	_ "github.com/blockchain-data-standards/manifesto/common"
	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
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
	Url     *url.URL
	headers map[string]string

	// pool is a small round-robin pool of independent gRPC connections
	// plus a stuck-call watchdog. See grpc_bds_resilience.go.
	pool *bdsPool

	projectId       string
	upstream        common.Upstream
	upstreamId      string
	appCtx          context.Context
	logger          *zerolog.Logger
	isLogLevelTrace bool

	// expectedChainId is the chainId this client is configured to serve
	// (0 = unknown). When set, it is (a) stamped into every BDS request that
	// carries an optional chainId field — a chain-aware server then REJECTS
	// requests reaching the wrong chain's backend, so a cross-wired endpoint
	// (a stale DNS record or reused address answering for another chain)
	// fails loudly per request instead of silently answering with another
	// chain's data — and (b) verified by the pool's connection maintainer via
	// the ChainId RPC (see grpc_bds_resilience.go). Atomic: the cache
	// connector arms it via SetExpectedChainId after probing.
	expectedChainId atomic.Uint64
}

// SetExpectedChainId arms chain-identity enforcement after construction —
// for callers that only learn the server's chain by probing it (the gRPC
// cache connector). From then on every request carries the chainId assertion
// and the pool maintainer keeps re-verifying the connections. Deliberately
// NOT part of the GrpcBdsClient interface (callers type-assert) so existing
// interface implementations stay compatible. Safe to call concurrently with
// in-flight requests.
func (c *GenericGrpcBdsClient) SetExpectedChainId(chainId uint64) {
	c.expectedChainId.Store(chainId)
	if c.pool != nil {
		c.pool.expectedChainId.Store(chainId)
	}
}

// NewGrpcBdsClient builds a BDS gRPC client backed by a round-robin connection
// pool. poolSize sets the number of connections; <= 0 uses the built-in default
// (bdsPoolSize).
func NewGrpcBdsClient(
	appCtx context.Context,
	logger *zerolog.Logger,
	projectId string,
	upstream common.Upstream,
	parsedUrl *url.URL,
	poolSize int,
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
	if upstream != nil {
		if cfg := upstream.Config(); cfg != nil && cfg.Evm != nil && cfg.Evm.ChainId > 0 {
			client.expectedChainId.Store(uint64(cfg.Evm.ChainId))
		}
	}

	// Apply any grpc.headers configured on the upstream as gRPC metadata on
	// every outbound request, mirroring how the HTTP client applies
	// jsonRpc.headers (http_json_rpc_client.go) and the gRPC cache connector
	// applies its headers (data/grpc.go). This is how an edge-api auth key
	// (authorization: Bearer <token>) reaches the wire for a grpc:// upstream.
	if upstream != nil {
		if cfg := upstream.Config(); cfg != nil && cfg.Grpc != nil {
			client.SetHeaders(cfg.Grpc.Headers)
		}
	}

	target, useTLS := pickTargetForBDS(parsedUrl)

	// Determine whether to use TLS based on port or URL scheme.
	var transportCredentials credentials.TransportCredentials
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

	pool, err := newBdsPool(appCtx, logger, projectId, upsId, target, transportCredentials, serviceConfig, poolSize, client.expectedChainId.Load())
	if err != nil {
		return nil, err
	}
	client.pool = pool

	// Setup graceful shutdown
	go func() {
		<-appCtx.Done()
		client.shutdown()
	}()

	logger.Debug().
		Str("target", target).
		Int("pool_size", pool.Size()).
		Dur("hard_call_timeout", bdsHardCallTimeout).
		Msg("created gRPC BDS client")

	return client, nil
}

// chainIdParam returns the configured chainId as the optional per-request
// chain assertion, or nil when unknown. Chain-aware BDS servers validate
// this field on every RPC and reject mismatches, so a request that lands on
// the wrong chain's backend errors loudly instead of being answered with
// another chain's data.
func (c *GenericGrpcBdsClient) chainIdParam() *uint64 {
	v := c.expectedChainId.Load()
	if v == 0 {
		return nil
	}
	return &v
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
	// Hard per-call ceiling. FIRST line of defense — bounds the worst case
	// for wedged H2 streams independent of caller-supplied deadlines.
	// The cause is OUR private sentinel (not the shared failsafe one) so
	// the watchdog classification below can tell this cap apart from a
	// caller-side dynamic-timeout expiry.
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeoutCause(ctx, bdsHardCallTimeout, errBdsHardCapExceeded)
	defer cancel()

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

	if len(c.headers) > 0 {
		md := metadata.New(c.headers)
		ctx = metadata.NewOutgoingContext(ctx, md)
	}

	conn := c.pool.Pick()
	if conn == nil || conn.rpcClient == nil {
		err := fmt.Errorf("BDS client has no available connections")
		common.SetTraceSpanError(span, err)
		return nil, common.NewErrEndpointTransportFailure(c.Url, err)
	}

	var resp *common.NormalizedResponse
	switch jrReq.Method {
	case "eth_getBlockByNumber":
		resp, err = c.handleGetBlockByNumber(ctx, conn, req, jrReq)
	case "eth_getBlockByHash":
		resp, err = c.handleGetBlockByHash(ctx, conn, req, jrReq)
	case "eth_getLogs":
		resp, err = c.handleGetLogs(ctx, conn, req, jrReq)
	case "eth_getTransactionByHash":
		resp, err = c.handleGetTransactionByHash(ctx, conn, req, jrReq)
	case "eth_getTransactionReceipt":
		resp, err = c.handleGetTransactionReceipt(ctx, conn, req, jrReq)
	case "eth_getBlockReceipts":
		resp, err = c.handleGetBlockReceipts(ctx, conn, req, jrReq)
	case "eth_chainId":
		resp, err = c.handleChainId(ctx, conn, req, jrReq)
	case "eth_queryBlocks":
		resp, err = c.handleQueryBlocks(ctx, conn, req, jrReq)
	case "eth_queryTransactions":
		resp, err = c.handleQueryTransactions(ctx, conn, req, jrReq)
	case "eth_queryLogs":
		resp, err = c.handleQueryLogs(ctx, conn, req, jrReq)
	case "eth_queryTraces":
		resp, err = c.handleQueryTraces(ctx, conn, req, jrReq)
	case "eth_queryTransfers":
		resp, err = c.handleQueryTransfers(ctx, conn, req, jrReq)
	default:
		err := common.NewErrEndpointUnsupported(
			fmt.Errorf("unsupported method for gRPC BDS client: %s", jrReq.Method),
		)
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	// Classify any timeout-class error and decide whether to trigger
	// the watchdog. We distinguish three cases via context.Cause():
	//
	//   - OUR bdsHardCallTimeout fired:
	//       cause is errBdsHardCapExceeded (the cause we set on the
	//       inner WithTimeoutCause). This is the wedged-stream signal —
	//       feed it to the pool watchdog so a consistently wedged conn
	//       is force-closed and replaced. Logged at Warn: it should be
	//       rare and always deserves attention.
	//
	//   - A failsafe timeout policy fired on the caller's ctx:
	//       cause is common.ErrDynamicTimeoutExceeded (set by the
	//       upstream/network executors). Under quantile-driven policies
	//       this fires routinely — it is a normal caller-side budget
	//       expiry, NOT a wedge. No watchdog, and only Debug logging:
	//       the failsafe layer already surfaces these via error returns
	//       and the timeout-fired metric.
	//
	//   - The caller's ctx fired with a generic DeadlineExceeded:
	//       same as above — caller-side timeout, not a wedge.
	if err != nil {
		ourHardCap := errors.Is(err, errBdsHardCapExceeded)
		anyTimeout := ourHardCap ||
			errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, common.ErrDynamicTimeoutExceeded)
		if anyTimeout {
			evt := c.logger.Debug()
			if ourHardCap {
				evt = c.logger.Warn()
			}
			evt.
				Err(err).
				Str("network.id", req.NetworkId()).
				Str("upstream.id", c.upstreamId).
				Str("method", jrReq.Method).
				Interface("request.id", req.ID()).
				Bool("our_hardcap", ourHardCap).
				Msg("BDS bounded-wait timeout fired")
			if ourHardCap {
				c.pool.OnBoundedTimeout(conn, jrReq.Method)
			}
			// Classify the error so callers see it as a request-timeout
			// rather than a generic transport failure (normalizeGrpcError
			// would otherwise wrap it as ErrEndpointTransportFailure).
			return nil, common.NewErrEndpointRequestTimeout(0, err)
		}
		// Caller-side cancellation is not a transport failure either.
		// BoundedCall surfaces context.Canceled (via context.Cause) when
		// the caller aborts while the underlying gRPC call is wedged;
		// classify it accordingly so this doesn't show up as an upstream
		// failure in metrics.
		if errors.Is(err, context.Canceled) {
			return nil, common.NewErrEndpointRequestCanceled(err)
		}
		return nil, c.normalizeGrpcError(err)
	}
	return resp, nil
}

func (c *GenericGrpcBdsClient) handleGetBlockByNumber(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
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
			ChainId:             c.chainIdParam(),
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
		grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*evm.GetBlockResponse, error) {
			return conn.rpcClient.GetBlockByHash(ctx, grpcReq)
		})
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
		ChainId:             c.chainIdParam(),
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
	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*evm.GetBlockResponse, error) {
		return conn.rpcClient.GetBlockByNumber(ctx, grpcReq)
	})
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

func (c *GenericGrpcBdsClient) handleGetBlockByHash(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
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
		ChainId:             c.chainIdParam(),
	}

	c.logger.Debug().
		Str("blockHash", blockHashStr).
		Bool("includeTransactions", includeTransactions).
		Msg("calling gRPC GetBlockByHash")

	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*evm.GetBlockResponse, error) {
		return conn.rpcClient.GetBlockByHash(ctx, grpcReq)
	})
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

func (c *GenericGrpcBdsClient) handleGetLogs(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
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

	topics, err := buildTopicFilters(filterParams["topics"])
	if err != nil {
		return nil, err
	}

	grpcReq := &evm.GetLogsRequest{
		FromBlock: fromBlock,
		ToBlock:   toBlock,
		Addresses: addresses,
		Topics:    topics,
		ChainId:   c.chainIdParam(),
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
	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*evm.GetLogsResponse, error) {
		return conn.rpcClient.GetLogs(ctx, grpcReq)
	})
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

func (c *GenericGrpcBdsClient) handleGetTransactionByHash(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
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
		ChainId:         c.chainIdParam(),
	}

	c.logger.Debug().
		Str("transactionHash", txHashStr).
		Msg("calling gRPC GetTransactionByHash")

	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*evm.GetTransactionByHashResponse, error) {
		return conn.rpcClient.GetTransactionByHash(ctx, grpcReq)
	})
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

func (c *GenericGrpcBdsClient) handleGetTransactionReceipt(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
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
		ChainId:         c.chainIdParam(),
	}

	c.logger.Debug().
		Str("transactionHash", txHashStr).
		Msg("calling gRPC GetTransactionReceipt")

	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*evm.GetTransactionReceiptResponse, error) {
		return conn.rpcClient.GetTransactionReceipt(ctx, grpcReq)
	})
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

func (c *GenericGrpcBdsClient) handleChainId(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	grpcReq := &evm.ChainIdRequest{}

	c.logger.Debug().
		Msg("calling gRPC ChainId")

	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*evm.ChainIdResponse, error) {
		return conn.rpcClient.ChainId(ctx, grpcReq)
	})
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}

	// A server answering for a different chain than this client is configured
	// for is a cross-wired endpoint (stale DNS record / reused address) — fail
	// loudly instead of propagating another chain's identity (and, through the
	// state poller, its heads).
	if expected := c.expectedChainId.Load(); expected > 0 && grpcResp.ChainId != expected {
		mismatchErr := common.NewErrEndpointChainIdMismatch(grpcResp.ChainId, expected)
		c.logger.Error().Err(mismatchErr).Str("target", c.Url.String()).Msg("chainId mismatch from BDS server")
		return nil, common.NewErrEndpointTransportFailure(c.Url, mismatchErr)
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

func (c *GenericGrpcBdsClient) handleGetBlockReceipts(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
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

	grpcReq := &evm.GetBlockReceiptsRequest{ChainId: c.chainIdParam()}

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

	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*evm.GetBlockReceiptsResponse, error) {
		return conn.rpcClient.GetBlockReceipts(ctx, grpcReq)
	})
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
	if c.pool != nil {
		c.pool.Shutdown()
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
	if c == nil || c.pool == nil {
		return nil
	}
	conn := c.pool.Pick()
	if conn == nil {
		return nil
	}
	return conn.queryClient
}

// Helper functions for conversion

func parseHexBytes(hexStr string) ([]byte, error) {
	return evm.HexToBytes(hexStr)
}

// buildTopicFilters converts the JSON-RPC topics array (where each entry may be
// a string, an array of strings, or null) into the proto TopicFilter slice.
//
// A null entry is a wildcard at that position and MUST emit an empty
// TopicFilter so positional alignment with subsequent filters is preserved:
// dropping the entry would shift later filters left, e.g. [selector, null, to]
// would be sent as [selector, to] and match logs where topic[1]=to instead of
// topic[2]=to — silently returning zero results.
func buildTopicFilters(topicsParam interface{}) ([]*evm.TopicFilter, error) {
	raw, ok := topicsParam.([]interface{})
	if !ok {
		return nil, nil
	}
	topics := make([]*evm.TopicFilter, 0, len(raw))
	for _, topicParam := range raw {
		topicFilter := &evm.TopicFilter{}
		switch v := topicParam.(type) {
		case string:
			topic, err := parseHexBytes(v)
			if err != nil {
				return nil, fmt.Errorf("failed to parse topic: %w", err)
			}
			topicFilter.Values = append(topicFilter.Values, topic)
		case []interface{}:
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
			// wildcard: leave Values empty, fall through to append below
		}
		topics = append(topics, topicFilter)
	}
	return topics, nil
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

// queryPageRange is the structural interface every Query*Response type
// satisfies — they all expose GetFromBlock/GetToBlock/GetCursorBlock
// returning *evm.CursorBlock.
type queryPageRange interface {
	GetFromBlock() *evm.CursorBlock
	GetToBlock() *evm.CursorBlock
	GetCursorBlock() *evm.CursorBlock
}

// applyQueryRangeBounds propagates the From/To/CursorBlock fields from
// a streaming page into the per-field aggregate slots. From/To use
// "first wins" semantics (the upstream sets them on the opening page
// and never changes them); CursorBlock uses "last wins" so each page
// advances the cursor. Centralizing this avoids the 9-line repeat
// across the five eth_query* handlers.
func applyQueryRangeBounds(aggFrom, aggTo, aggCursor **evm.CursorBlock, page queryPageRange) {
	if *aggFrom == nil {
		if v := page.GetFromBlock(); v != nil {
			*aggFrom = v
		}
	}
	if *aggTo == nil {
		if v := page.GetToBlock(); v != nil {
			*aggTo = v
		}
	}
	if v := page.GetCursorBlock(); v != nil {
		*aggCursor = v
	}
}

func (c *GenericGrpcBdsClient) handleQueryBlocks(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	rawParams, err := jsonRpcParamsFor(jrReq)
	if err != nil {
		return nil, err
	}
	grpcReq, err := evm.QueryBlocksRequestFromJsonRpc(rawParams)
	if err != nil {
		return nil, fmt.Errorf("invalid eth_queryBlocks params: %w", err)
	}
	if grpcReq.ChainId == nil {
		grpcReq.ChainId = c.chainIdParam()
	}

	ctx, span := common.StartDetailSpan(ctx, "GrpcBdsClient.QueryBlocks")
	defer span.End()

	// Bound the whole stream lifecycle: open AND recv loop. Stream-open
	// itself can wedge under H2 flow-control deadlock, so wrapping only
	// the recv loop (the previous shape) didn't actually cap worst-case
	// latency. Using BoundedCallT also avoids a leaked-goroutine race
	// on the aggregated buffer: the leaked inner goroutine never shares
	// state with the outer caller — the result is communicated only via
	// the helper's channel.
	aggregated, err := callBoundedT(ctx, func(ctx context.Context) (*evm.QueryBlocksResponse, error) {
		stream, err := conn.queryClient.QueryBlocks(ctx, grpcReq)
		if err != nil {
			return nil, err
		}
		agg := &evm.QueryBlocksResponse{}
		if err := recvQueryStream(stream.Recv, func(page *evm.QueryBlocksResponse) {
			agg.Blocks = append(agg.Blocks, page.GetBlocks()...)
			applyQueryRangeBounds(&agg.FromBlock, &agg.ToBlock, &agg.CursorBlock, page)
		}); err != nil {
			return nil, err
		}
		return agg, nil
	})
	if err != nil {
		return nil, fmt.Errorf("gRPC stream error: %w", err)
	}

	return c.buildQueryJsonRpcResponse(req, jrReq, evm.QueryBlocksResponseToJsonRpc(aggregated))
}

func (c *GenericGrpcBdsClient) handleQueryTransactions(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	rawParams, err := jsonRpcParamsFor(jrReq)
	if err != nil {
		return nil, err
	}
	grpcReq, err := evm.QueryTransactionsRequestFromJsonRpc(rawParams)
	if err != nil {
		return nil, fmt.Errorf("invalid eth_queryTransactions params: %w", err)
	}
	if grpcReq.ChainId == nil {
		grpcReq.ChainId = c.chainIdParam()
	}

	ctx, span := common.StartDetailSpan(ctx, "GrpcBdsClient.QueryTransactions")
	defer span.End()

	aggregated, err := callBoundedT(ctx, func(ctx context.Context) (*evm.QueryTransactionsResponse, error) {
		stream, err := conn.queryClient.QueryTransactions(ctx, grpcReq)
		if err != nil {
			return nil, err
		}
		agg := &evm.QueryTransactionsResponse{}
		if err := recvQueryStream(stream.Recv, func(page *evm.QueryTransactionsResponse) {
			agg.Transactions = append(agg.Transactions, page.GetTransactions()...)
			agg.Blocks = append(agg.Blocks, page.GetBlocks()...)
			applyQueryRangeBounds(&agg.FromBlock, &agg.ToBlock, &agg.CursorBlock, page)
		}); err != nil {
			return nil, err
		}
		return agg, nil
	})
	if err != nil {
		return nil, fmt.Errorf("gRPC stream error: %w", err)
	}

	return c.buildQueryJsonRpcResponse(req, jrReq, evm.QueryTransactionsResponseToJsonRpc(aggregated))
}

func (c *GenericGrpcBdsClient) handleQueryLogs(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	rawParams, err := jsonRpcParamsFor(jrReq)
	if err != nil {
		return nil, err
	}
	grpcReq, err := evm.QueryLogsRequestFromJsonRpc(rawParams)
	if err != nil {
		return nil, fmt.Errorf("invalid eth_queryLogs params: %w", err)
	}
	if grpcReq.ChainId == nil {
		grpcReq.ChainId = c.chainIdParam()
	}

	ctx, span := common.StartDetailSpan(ctx, "GrpcBdsClient.QueryLogs")
	defer span.End()

	aggregated, err := callBoundedT(ctx, func(ctx context.Context) (*evm.QueryLogsResponse, error) {
		stream, err := conn.queryClient.QueryLogs(ctx, grpcReq)
		if err != nil {
			return nil, err
		}
		agg := &evm.QueryLogsResponse{}
		if err := recvQueryStream(stream.Recv, func(page *evm.QueryLogsResponse) {
			agg.Logs = append(agg.Logs, page.GetLogs()...)
			agg.Transactions = append(agg.Transactions, page.GetTransactions()...)
			agg.Blocks = append(agg.Blocks, page.GetBlocks()...)
			applyQueryRangeBounds(&agg.FromBlock, &agg.ToBlock, &agg.CursorBlock, page)
		}); err != nil {
			return nil, err
		}
		return agg, nil
	})
	if err != nil {
		return nil, fmt.Errorf("gRPC stream error: %w", err)
	}

	return c.buildQueryJsonRpcResponse(req, jrReq, evm.QueryLogsResponseToJsonRpc(aggregated))
}

func (c *GenericGrpcBdsClient) handleQueryTraces(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	rawParams, err := jsonRpcParamsFor(jrReq)
	if err != nil {
		return nil, err
	}
	grpcReq, err := evm.QueryTracesRequestFromJsonRpc(rawParams)
	if err != nil {
		return nil, fmt.Errorf("invalid eth_queryTraces params: %w", err)
	}
	if grpcReq.ChainId == nil {
		grpcReq.ChainId = c.chainIdParam()
	}

	ctx, span := common.StartDetailSpan(ctx, "GrpcBdsClient.QueryTraces")
	defer span.End()

	aggregated, err := callBoundedT(ctx, func(ctx context.Context) (*evm.QueryTracesResponse, error) {
		stream, err := conn.queryClient.QueryTraces(ctx, grpcReq)
		if err != nil {
			return nil, err
		}
		agg := &evm.QueryTracesResponse{}
		if err := recvQueryStream(stream.Recv, func(page *evm.QueryTracesResponse) {
			agg.Traces = append(agg.Traces, page.GetTraces()...)
			agg.Transactions = append(agg.Transactions, page.GetTransactions()...)
			agg.Blocks = append(agg.Blocks, page.GetBlocks()...)
			applyQueryRangeBounds(&agg.FromBlock, &agg.ToBlock, &agg.CursorBlock, page)
		}); err != nil {
			return nil, err
		}
		return agg, nil
	})
	if err != nil {
		return nil, fmt.Errorf("gRPC stream error: %w", err)
	}

	return c.buildQueryJsonRpcResponse(req, jrReq, evm.QueryTracesResponseToJsonRpc(aggregated))
}

func (c *GenericGrpcBdsClient) handleQueryTransfers(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	rawParams, err := jsonRpcParamsFor(jrReq)
	if err != nil {
		return nil, err
	}
	grpcReq, err := evm.QueryTransfersRequestFromJsonRpc(rawParams)
	if err != nil {
		return nil, fmt.Errorf("invalid eth_queryTransfers params: %w", err)
	}
	if grpcReq.ChainId == nil {
		grpcReq.ChainId = c.chainIdParam()
	}

	ctx, span := common.StartDetailSpan(ctx, "GrpcBdsClient.QueryTransfers")
	defer span.End()

	aggregated, err := callBoundedT(ctx, func(ctx context.Context) (*evm.QueryTransfersResponse, error) {
		stream, err := conn.queryClient.QueryTransfers(ctx, grpcReq)
		if err != nil {
			return nil, err
		}
		agg := &evm.QueryTransfersResponse{}
		if err := recvQueryStream(stream.Recv, func(page *evm.QueryTransfersResponse) {
			agg.Transfers = append(agg.Transfers, page.GetTransfers()...)
			agg.Transactions = append(agg.Transactions, page.GetTransactions()...)
			agg.Blocks = append(agg.Blocks, page.GetBlocks()...)
			applyQueryRangeBounds(&agg.FromBlock, &agg.ToBlock, &agg.CursorBlock, page)
		}); err != nil {
			return nil, err
		}
		return agg, nil
	})
	if err != nil {
		return nil, fmt.Errorf("gRPC stream error: %w", err)
	}

	return c.buildQueryJsonRpcResponse(req, jrReq, evm.QueryTransfersResponseToJsonRpc(aggregated))
}
