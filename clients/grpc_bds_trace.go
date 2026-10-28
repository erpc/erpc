package clients

import (
	"context"
	"fmt"
	"math/big"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
)

// Trace and balance calls on bds.evm.RPCQueryService. The JSON-RPC mapping
// matches the manifesto Rust mapping (rust/src/evm/json_rpc.rs) that
// edge-boost uses for the same server. Params this mapping cannot express
// are ErrEndpointUnsupported, so the network tries the next upstream instead
// of returning a client error a node would not have returned.

func (c *GenericGrpcBdsClient) handleTraceBlock(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	params, err := snapshotParams(jrReq)
	if err != nil {
		return nil, err
	}
	if len(params) < 1 {
		return nil, unmappable("%s: missing block parameter", jrReq.Method)
	}
	first, ok := params[0].(string)
	if !ok {
		return nil, unmappable("%s: params[0] must be a string", jrReq.Method)
	}
	opts, err := traceOptions(params, 1)
	if err != nil {
		return nil, err
	}
	grpcReq := &evm.TraceBlockRequest{Trace: opts, ChainId: c.chainIdParam()}
	if jrReq.Method == "debug_traceBlockByHash" {
		hash, err := parseFixedHex(first, 32)
		if err != nil {
			return nil, unmappable("debug_traceBlockByHash: %v", err)
		}
		grpcReq.BlockHash = hash
	} else {
		grpcReq.BlockNumber = &first
	}
	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*evm.TraceResponse, error) {
		return conn.rpcClient.TraceBlock(ctx, grpcReq)
	})
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}
	return rawResultResponse(req, jrReq, grpcResp.Result)
}

func (c *GenericGrpcBdsClient) handleTraceTransaction(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	params, err := snapshotParams(jrReq)
	if err != nil {
		return nil, err
	}
	if len(params) < 1 {
		return nil, unmappable("debug_traceTransaction: missing transaction hash")
	}
	hashStr, ok := params[0].(string)
	if !ok {
		return nil, unmappable("debug_traceTransaction: params[0] must be a string")
	}
	hash, err := parseFixedHex(hashStr, 32)
	if err != nil {
		return nil, unmappable("debug_traceTransaction: %v", err)
	}
	opts, err := traceOptions(params, 1)
	if err != nil {
		return nil, err
	}
	grpcReq := &evm.TraceTransactionRequest{TransactionHash: hash, Trace: opts, ChainId: c.chainIdParam()}
	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*evm.TraceResponse, error) {
		return conn.rpcClient.TraceTransaction(ctx, grpcReq)
	})
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}
	return rawResultResponse(req, jrReq, grpcResp.Result)
}

func (c *GenericGrpcBdsClient) handleGetBalance(ctx context.Context, conn *bdsConn, req *common.NormalizedRequest, jrReq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	params, err := snapshotParams(jrReq)
	if err != nil {
		return nil, err
	}
	// A node refuses a missing block parameter, so it is not guessed here.
	if len(params) < 2 {
		return nil, unmappable("eth_getBalance: needs an address and a block parameter")
	}
	addrStr, ok := params[0].(string)
	if !ok {
		return nil, unmappable("eth_getBalance: params[0] must be a string")
	}
	addr, err := parseFixedHex(addrStr, 20)
	if err != nil {
		return nil, unmappable("eth_getBalance: %v", err)
	}
	grpcReq := &evm.GetBalanceRequest{Address: addr, ChainId: c.chainIdParam()}
	switch b := params[1].(type) {
	case string:
		grpcReq.BlockNumber = &b
	case map[string]interface{}:
		if h, ok := b["blockHash"].(string); ok {
			hash, err := parseFixedHex(h, 32)
			if err != nil {
				return nil, unmappable("eth_getBalance: %v", err)
			}
			grpcReq.BlockHash = hash
		} else if n, ok := b["blockNumber"].(string); ok {
			grpcReq.BlockNumber = &n
		} else {
			return nil, unmappable("eth_getBalance: object param needs a string blockHash or blockNumber")
		}
	default:
		return nil, unmappable("eth_getBalance: params[1] must be a string or an object")
	}
	grpcResp, err := callBoundedT(ctx, func(ctx context.Context) (*evm.GetBalanceResponse, error) {
		return conn.rpcClient.GetBalance(ctx, grpcReq)
	})
	if err != nil {
		return nil, fmt.Errorf("gRPC call failed: %w", err)
	}
	result, err := sonic.Marshal(fmt.Sprintf("0x%x", new(big.Int).SetBytes(grpcResp.Balance)))
	if err != nil {
		return nil, fmt.Errorf("failed to marshal result: %w", err)
	}
	return rawResultResponse(req, jrReq, result)
}

// traceOptions splits params[i] into TraceOptions: `tracer` as its string,
// `tracerConfig` as the raw JSON of its value, every other key as one JSON
// object. Absent or null leaves the field empty.
func traceOptions(params []interface{}, i int) (*evm.TraceOptions, error) {
	opts := &evm.TraceOptions{}
	if len(params) <= i || params[i] == nil {
		return opts, nil
	}
	obj, ok := params[i].(map[string]interface{})
	if !ok {
		return nil, unmappable("debug_trace*: trace options must be an object")
	}
	rest := make(map[string]interface{}, len(obj))
	for key, value := range obj {
		switch key {
		case "tracer":
			if value == nil {
				continue
			}
			s, ok := value.(string)
			if !ok {
				return nil, unmappable("debug_trace*: tracer must be a string")
			}
			opts.Tracer = s
		case "tracerConfig":
			if value == nil {
				continue
			}
			b, err := sonic.Marshal(value)
			if err != nil {
				return nil, unmappable("debug_trace*: tracerConfig: %v", err)
			}
			opts.TracerConfig = b
		default:
			rest[key] = value
		}
	}
	if len(rest) > 0 {
		b, err := sonic.Marshal(rest)
		if err != nil {
			return nil, unmappable("debug_trace*: options: %v", err)
		}
		opts.Options = b
	}
	return opts, nil
}

// snapshotParams deep-copies the params, as the other handlers do, so a
// concurrent rewrite of the shared request cannot race the mapping.
func snapshotParams(jrReq *common.JsonRpcRequest) ([]interface{}, error) {
	jrReq.RLock()
	b, err := sonic.Marshal(jrReq.Params)
	jrReq.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}
	var params []interface{}
	if err := sonic.Unmarshal(b, &params); err != nil {
		return nil, fmt.Errorf("failed to parse params: %w", err)
	}
	return params, nil
}

func parseFixedHex(s string, n int) ([]byte, error) {
	b, err := parseHexBytes(s)
	if err != nil {
		return nil, err
	}
	if len(b) != n {
		return nil, fmt.Errorf("%q is %d bytes, want %d", s, len(b), n)
	}
	return b, nil
}

func unmappable(format string, args ...interface{}) error {
	return common.NewErrEndpointUnsupported(fmt.Errorf(format, args...))
}

// rawResultResponse wraps already-encoded JSON as the JSON-RPC result.
func rawResultResponse(req *common.NormalizedRequest, jrReq *common.JsonRpcRequest, result []byte) (*common.NormalizedResponse, error) {
	if len(result) == 0 {
		result = []byte("null")
	}
	jsonRpcResp := &common.JsonRpcResponse{}
	if err := jsonRpcResp.SetID(jrReq.ID); err != nil {
		return nil, fmt.Errorf("failed to set ID: %w", err)
	}
	jsonRpcResp.SetResult(result)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jsonRpcResp), nil
}
