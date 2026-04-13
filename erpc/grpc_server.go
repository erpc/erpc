package erpc

import (
	"context"
	"fmt"
	"net"
	"runtime/debug"
	"strings"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type GrpcServer struct {
	appCtx    context.Context
	serverCfg *common.ServerConfig
	erpc      *ERPC
	processor *RequestProcessor
	logger    *zerolog.Logger
	server    *grpc.Server

	trustedForwarderNets []net.IPNet
	trustedForwarderIPs  map[string]struct{}
	trustedIPHeaders     []string

	evm.UnimplementedRPCQueryServiceServer
	evm.UnimplementedQueryServiceServer
}

func grpcSharesHttpV4(cfg *common.ServerConfig) bool {
	if cfg == nil || cfg.GrpcEnabled == nil || !*cfg.GrpcEnabled {
		return false
	}
	if cfg.ListenV4 == nil || !*cfg.ListenV4 {
		return false
	}
	if cfg.HttpHostV4 == nil || cfg.HttpPortV4 == nil || cfg.GrpcHostV4 == nil || cfg.GrpcPortV4 == nil {
		return false
	}
	return *cfg.HttpHostV4 == *cfg.GrpcHostV4 && *cfg.HttpPortV4 == *cfg.GrpcPortV4
}

func NewGrpcServer(
	ctx context.Context,
	logger *zerolog.Logger,
	cfg *common.ServerConfig,
	erpcInstance *ERPC,
) (*GrpcServer, error) {
	gs := &GrpcServer{
		appCtx:    ctx,
		serverCfg: cfg,
		erpc:      erpcInstance,
		processor: NewRequestProcessor(erpcInstance, logger),
		logger:    logger,
	}
	if cfg != nil {
		gs.trustedForwarderIPs = make(map[string]struct{}, len(cfg.TrustedIPForwarders))
		for _, entry := range cfg.TrustedIPForwarders {
			val := strings.TrimSpace(entry)
			if val == "" {
				continue
			}
			if strings.Contains(val, "/") {
				if _, ipnet, err := net.ParseCIDR(val); err == nil && ipnet != nil {
					gs.trustedForwarderNets = append(gs.trustedForwarderNets, *ipnet)
				} else {
					logger.Warn().Str("trustedForwarder", val).Msg("invalid CIDR in trusted forwarders; ignoring")
				}
				continue
			}
			if ip := net.ParseIP(val); ip != nil {
				gs.trustedForwarderIPs[ip.String()] = struct{}{}
			} else {
				logger.Warn().Str("trustedForwarder", val).Msg("invalid IP in trusted forwarders; ignoring")
			}
		}
		for _, h := range cfg.TrustedIPHeaders {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" {
				continue
			}
			gs.trustedIPHeaders = append(gs.trustedIPHeaders, h)
		}
	}

	opts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(*cfg.GrpcMaxRecvMsgSize),
		grpc.MaxSendMsgSize(*cfg.GrpcMaxSendMsgSize),
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(gs.panicRecoveryUnary()),
		grpc.ChainStreamInterceptor(gs.panicRecoveryStream()),
	}
	if cfg.TLS != nil && cfg.TLS.Enabled {
		creds, err := credentials.NewServerTLSFromFile(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.Creds(creds))
	}

	gs.server = grpc.NewServer(opts...)
	evm.RegisterRPCQueryServiceServer(gs.server, gs)
	evm.RegisterQueryServiceServer(gs.server, gs)
	return gs, nil
}

func (gs *GrpcServer) Start(logger *zerolog.Logger) error {
	addr := fmt.Sprintf("%s:%d", *gs.serverCfg.GrpcHostV4, *gs.serverCfg.GrpcPortV4)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gRPC: failed to listen on %s: %w", addr, err)
	}
	logger.Info().Str("addr", addr).Msg("starting gRPC server")
	go func() {
		<-gs.appCtx.Done()
		gs.server.GracefulStop()
	}()
	return gs.server.Serve(lis)
}

func (gs *GrpcServer) extractRequestInput(ctx context.Context, method string) (*RequestInput, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "missing metadata")
	}
	project := firstMD(md, "x-erpc-project")
	if project == "" {
		return nil, status.Error(codes.InvalidArgument, "x-erpc-project metadata required")
	}
	chainID := firstMD(md, "x-erpc-chain-id")
	if chainID == "" {
		return nil, status.Error(codes.InvalidArgument, "x-erpc-chain-id metadata required")
	}
	ap, err := auth.NewPayloadFromGrpc(method, md)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	return &RequestInput{
		ProjectId:    project,
		Architecture: fallback(firstMD(md, "x-erpc-architecture"), "evm"),
		ChainId:      chainID,
		AuthPayload:  ap,
		ClientIP:     gs.grpcClientIP(ctx, md),
		UserAgent:    firstMD(md, "user-agent"),
	}, nil
}

func (gs *GrpcServer) ChainId(ctx context.Context, req *evm.ChainIdRequest) (*evm.ChainIdResponse, error) {
	input, err := gs.extractRequestInput(ctx, "eth_chainId")
	if err != nil {
		return nil, err
	}
	resp, err := gs.processor.ProcessUnary(ctx, input, buildJSONRPCRequest("eth_chainId", []interface{}{}))
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	result, err := parseJSONRPCResult(ctx, resp)
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	var chainIDHex string
	if err := sonic.Unmarshal(result, &chainIDHex); err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	chainID, err := evm.HexToUint64(chainIDHex)
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	return &evm.ChainIdResponse{ChainId: chainID}, nil
}

func (gs *GrpcServer) GetBlockByNumber(ctx context.Context, req *evm.GetBlockByNumberRequest) (*evm.GetBlockResponse, error) {
	input, err := gs.extractRequestInput(ctx, "eth_getBlockByNumber")
	if err != nil {
		return nil, err
	}
	resp, err := gs.processor.ProcessUnary(ctx, input, buildJSONRPCRequest("eth_getBlockByNumber", []interface{}{req.BlockNumber, req.IncludeTransactions}))
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	result, err := parseJSONRPCResult(ctx, resp)
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	if string(result) == "null" {
		return &evm.GetBlockResponse{}, nil
	}
	var block evm.JsonRpcBlock
	if err := sonic.Unmarshal(result, &block); err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	protoBlock, err := block.ToProto()
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	return &evm.GetBlockResponse{
		Block:            protoBlock.Header,
		Transactions:     protoBlock.TransactionHashes,
		FullTransactions: protoBlock.FullTransactions,
		Withdrawals:      protoBlock.Withdrawals,
	}, nil
}

func (gs *GrpcServer) GetBlockByHash(ctx context.Context, req *evm.GetBlockByHashRequest) (*evm.GetBlockResponse, error) {
	input, err := gs.extractRequestInput(ctx, "eth_getBlockByHash")
	if err != nil {
		return nil, err
	}
	resp, err := gs.processor.ProcessUnary(ctx, input, buildJSONRPCRequest("eth_getBlockByHash", []interface{}{evm.BytesToHex(req.BlockHash), req.IncludeTransactions}))
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	result, err := parseJSONRPCResult(ctx, resp)
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	if string(result) == "null" {
		return &evm.GetBlockResponse{}, nil
	}
	var block evm.JsonRpcBlock
	if err := sonic.Unmarshal(result, &block); err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	protoBlock, err := block.ToProto()
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	return &evm.GetBlockResponse{
		Block:            protoBlock.Header,
		Transactions:     protoBlock.TransactionHashes,
		FullTransactions: protoBlock.FullTransactions,
		Withdrawals:      protoBlock.Withdrawals,
	}, nil
}

func (gs *GrpcServer) GetLogs(ctx context.Context, req *evm.GetLogsRequest) (*evm.GetLogsResponse, error) {
	input, err := gs.extractRequestInput(ctx, "eth_getLogs")
	if err != nil {
		return nil, err
	}
	payload := map[string]interface{}{}
	if req.FromBlock != nil {
		payload["fromBlock"] = fmt.Sprintf("0x%x", *req.FromBlock)
	}
	if req.ToBlock != nil {
		payload["toBlock"] = fmt.Sprintf("0x%x", *req.ToBlock)
	}
	if len(req.Addresses) == 1 {
		payload["address"] = evm.BytesToHex(req.Addresses[0])
	} else if len(req.Addresses) > 1 {
		addrs := make([]string, 0, len(req.Addresses))
		for _, addr := range req.Addresses {
			addrs = append(addrs, evm.BytesToHex(addr))
		}
		payload["address"] = addrs
	}
	if len(req.Topics) > 0 {
		topics := make([]interface{}, 0, len(req.Topics))
		for _, topic := range req.Topics {
			if topic == nil || len(topic.Values) == 0 {
				topics = append(topics, nil)
				continue
			}
			if len(topic.Values) == 1 {
				topics = append(topics, evm.BytesToHex(topic.Values[0]))
				continue
			}
			values := make([]string, 0, len(topic.Values))
			for _, value := range topic.Values {
				values = append(values, evm.BytesToHex(value))
			}
			topics = append(topics, values)
		}
		payload["topics"] = topics
	}
	resp, err := gs.processor.ProcessUnary(ctx, input, buildJSONRPCRequest("eth_getLogs", []interface{}{payload}))
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	result, err := parseJSONRPCResult(ctx, resp)
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	var rawLogs []*evm.JsonRpcLog
	if err := sonic.Unmarshal(result, &rawLogs); err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	logs := make([]*evm.Log, 0, len(rawLogs))
	for _, rawLog := range rawLogs {
		log, err := rawLog.ToProto()
		if err != nil {
			return nil, gs.mapToGRPCStatus(err)
		}
		logs = append(logs, log)
	}
	return &evm.GetLogsResponse{Logs: logs}, nil
}

func (gs *GrpcServer) GetTransactionByHash(ctx context.Context, req *evm.GetTransactionByHashRequest) (*evm.GetTransactionByHashResponse, error) {
	input, err := gs.extractRequestInput(ctx, "eth_getTransactionByHash")
	if err != nil {
		return nil, err
	}
	resp, err := gs.processor.ProcessUnary(ctx, input, buildJSONRPCRequest("eth_getTransactionByHash", []interface{}{evm.BytesToHex(req.TransactionHash)}))
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	result, err := parseJSONRPCResult(ctx, resp)
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	if string(result) == "null" {
		return &evm.GetTransactionByHashResponse{}, nil
	}
	var txMap map[string]interface{}
	if err := sonic.Unmarshal(result, &txMap); err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	tx, err := evm.ParseJsonRpcTransaction(txMap, nil)
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	return &evm.GetTransactionByHashResponse{Transaction: tx}, nil
}

func (gs *GrpcServer) GetTransactionReceipt(ctx context.Context, req *evm.GetTransactionReceiptRequest) (*evm.GetTransactionReceiptResponse, error) {
	input, err := gs.extractRequestInput(ctx, "eth_getTransactionReceipt")
	if err != nil {
		return nil, err
	}
	resp, err := gs.processor.ProcessUnary(ctx, input, buildJSONRPCRequest("eth_getTransactionReceipt", []interface{}{evm.BytesToHex(req.TransactionHash)}))
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	result, err := parseJSONRPCResult(ctx, resp)
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	if string(result) == "null" {
		return &evm.GetTransactionReceiptResponse{}, nil
	}
	var receipt evm.JsonRpcReceipt
	if err := sonic.Unmarshal(result, &receipt); err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	protoReceipt, err := receipt.ToProto()
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	return &evm.GetTransactionReceiptResponse{Receipt: protoReceipt}, nil
}

func (gs *GrpcServer) GetBlockReceipts(ctx context.Context, req *evm.GetBlockReceiptsRequest) (*evm.GetBlockReceiptsResponse, error) {
	input, err := gs.extractRequestInput(ctx, "eth_getBlockReceipts")
	if err != nil {
		return nil, err
	}
	var blockParam interface{}
	if len(req.BlockHash) > 0 {
		blockParam = evm.BytesToHex(req.BlockHash)
	} else if req.BlockNumber != nil {
		blockParam = *req.BlockNumber
	} else {
		return nil, status.Error(codes.InvalidArgument, "blockNumber or blockHash required")
	}
	resp, err := gs.processor.ProcessUnary(ctx, input, buildJSONRPCRequest("eth_getBlockReceipts", []interface{}{blockParam}))
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	result, err := parseJSONRPCResult(ctx, resp)
	if err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	var rawReceipts []*evm.JsonRpcReceipt
	if err := sonic.Unmarshal(result, &rawReceipts); err != nil {
		return nil, gs.mapToGRPCStatus(err)
	}
	receipts := make([]*evm.Receipt, 0, len(rawReceipts))
	for _, rawReceipt := range rawReceipts {
		receipt, err := rawReceipt.ToProto()
		if err != nil {
			return nil, gs.mapToGRPCStatus(err)
		}
		receipts = append(receipts, receipt)
	}
	return &evm.GetBlockReceiptsResponse{Receipts: receipts}, nil
}

func (gs *GrpcServer) QueryBlocks(req *evm.QueryBlocksRequest, stream evm.QueryService_QueryBlocksServer) error {
	input, err := gs.extractRequestInput(stream.Context(), "eth_queryBlocks")
	if err != nil {
		return err
	}
	return gs.processor.ProcessQueryStream(stream.Context(), input, req, func(page proto.Message) error {
		return stream.Send(page.(*evm.QueryBlocksResponse))
	})
}

func (gs *GrpcServer) QueryTransactions(req *evm.QueryTransactionsRequest, stream evm.QueryService_QueryTransactionsServer) error {
	input, err := gs.extractRequestInput(stream.Context(), "eth_queryTransactions")
	if err != nil {
		return err
	}
	return gs.processor.ProcessQueryStream(stream.Context(), input, req, func(page proto.Message) error {
		return stream.Send(page.(*evm.QueryTransactionsResponse))
	})
}

func (gs *GrpcServer) QueryLogs(req *evm.QueryLogsRequest, stream evm.QueryService_QueryLogsServer) error {
	input, err := gs.extractRequestInput(stream.Context(), "eth_queryLogs")
	if err != nil {
		return err
	}
	return gs.processor.ProcessQueryStream(stream.Context(), input, req, func(page proto.Message) error {
		return stream.Send(page.(*evm.QueryLogsResponse))
	})
}

func (gs *GrpcServer) QueryTraces(req *evm.QueryTracesRequest, stream evm.QueryService_QueryTracesServer) error {
	input, err := gs.extractRequestInput(stream.Context(), "eth_queryTraces")
	if err != nil {
		return err
	}
	return gs.processor.ProcessQueryStream(stream.Context(), input, req, func(page proto.Message) error {
		return stream.Send(page.(*evm.QueryTracesResponse))
	})
}

func (gs *GrpcServer) QueryTransfers(req *evm.QueryTransfersRequest, stream evm.QueryService_QueryTransfersServer) error {
	input, err := gs.extractRequestInput(stream.Context(), "eth_queryTransfers")
	if err != nil {
		return err
	}
	return gs.processor.ProcessQueryStream(stream.Context(), input, req, func(page proto.Message) error {
		return stream.Send(page.(*evm.QueryTransfersResponse))
	})
}

func (gs *GrpcServer) panicRecoveryUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				gs.logger.Error().Interface("panic", r).Str("stack", string(debug.Stack())).Msg("gRPC unary panic")
				err = status.Error(codes.Internal, "internal server error")
			}
		}()
		return handler(ctx, req)
	}
}

func (gs *GrpcServer) panicRecoveryStream() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				gs.logger.Error().Interface("panic", r).Str("stack", string(debug.Stack())).Msg("gRPC stream panic")
				err = status.Error(codes.Internal, "internal server error")
			}
		}()
		return handler(srv, ss)
	}
}

func (gs *GrpcServer) mapToGRPCStatus(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case common.HasErrorCode(err, common.ErrCodeEndpointUnsupported):
		return status.Error(codes.Unimplemented, err.Error())
	case common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized):
		return status.Error(codes.Unauthenticated, err.Error())
	case common.HasErrorCode(err, common.ErrCodeEndpointRequestTimeout):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded, common.ErrCodeEndpointRequestTooLarge):
		return status.Error(codes.ResourceExhausted, err.Error())
	case common.HasErrorCode(err, common.ErrCodeEndpointMissingData):
		return status.Error(codes.NotFound, err.Error())
	case common.HasErrorCode(err, common.ErrCodeEndpointClientSideException):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func firstMD(md metadata.MD, key string) string {
	values := md.Get(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func fallback(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (gs *GrpcServer) grpcClientIP(ctx context.Context, md metadata.MD) string {
	remoteIP := grpcPeerIP(ctx)
	if remoteIP == nil {
		return ""
	}
	if !gs.isTrustedForwarder(remoteIP) {
		return remoteIP.String()
	}
	for _, hdr := range gs.trustedIPHeaders {
		if hdr == "" {
			continue
		}
		if v := firstMD(md, hdr); v != "" {
			ips := parseXForwardedFor(v)
			if ip := trimRightTrustedAndPick(ips, gs.isTrustedForwarder); ip != nil {
				return ip.String()
			}
		}
	}
	return remoteIP.String()
}

func (gs *GrpcServer) isTrustedForwarder(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if gs.trustedForwarderIPs != nil {
		if _, ok := gs.trustedForwarderIPs[ip.String()]; ok {
			return true
		}
	}
	for i := range gs.trustedForwarderNets {
		if gs.trustedForwarderNets[i].Contains(ip) {
			return true
		}
	}
	return false
}

func grpcPeerIP(ctx context.Context) net.IP {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return parseRemoteIP(p.Addr.String())
	}
	return nil
}
