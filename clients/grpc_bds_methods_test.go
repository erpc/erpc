package clients

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// scriptedBdsServer answers every bds.evm.RPCQueryService method with a
// configured response (or a configured error) and records the last request
// each method received, so tests can assert both directions of the mapping
// between JSON-RPC and the BDS protobuf API.
type scriptedBdsServer struct {
	evm.UnimplementedRPCQueryServiceServer

	calls atomic.Int64

	mu       sync.Mutex
	err      error
	chainID  uint64
	block    *evm.GetBlockResponse
	tx       *evm.GetTransactionByHashResponse
	receipt  *evm.GetTransactionReceiptResponse
	receipts *evm.GetBlockReceiptsResponse
	logs     *evm.GetLogsResponse
	trace    *evm.TraceResponse
	balance  *evm.GetBalanceResponse
	headerMD metadata.MD

	byNumber   *evm.GetBlockByNumberRequest
	byHash     *evm.GetBlockByHashRequest
	txReq      *evm.GetTransactionByHashRequest
	receiptReq *evm.GetTransactionReceiptRequest
	blockRcpts *evm.GetBlockReceiptsRequest
	logsReq    *evm.GetLogsRequest
	traceBlock *evm.TraceBlockRequest
	traceTx    *evm.TraceTransactionRequest
	balanceReq *evm.GetBalanceRequest
}

func (s *scriptedBdsServer) enter(ctx context.Context, record func()) error {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	record()
	if s.headerMD != nil {
		_ = grpc.SetHeader(ctx, s.headerMD)
	}
	return s.err
}

func (s *scriptedBdsServer) ChainId(ctx context.Context, _ *evm.ChainIdRequest) (*evm.ChainIdResponse, error) {
	if err := s.enter(ctx, func() {}); err != nil {
		return nil, err
	}
	return &evm.ChainIdResponse{ChainId: s.chainID}, nil
}

func (s *scriptedBdsServer) GetBlockByNumber(ctx context.Context, req *evm.GetBlockByNumberRequest) (*evm.GetBlockResponse, error) {
	if err := s.enter(ctx, func() { s.byNumber = req }); err != nil {
		return nil, err
	}
	return orEmpty(s.block, &evm.GetBlockResponse{}), nil
}

func (s *scriptedBdsServer) GetBlockByHash(ctx context.Context, req *evm.GetBlockByHashRequest) (*evm.GetBlockResponse, error) {
	if err := s.enter(ctx, func() { s.byHash = req }); err != nil {
		return nil, err
	}
	return orEmpty(s.block, &evm.GetBlockResponse{}), nil
}

func (s *scriptedBdsServer) GetTransactionByHash(ctx context.Context, req *evm.GetTransactionByHashRequest) (*evm.GetTransactionByHashResponse, error) {
	if err := s.enter(ctx, func() { s.txReq = req }); err != nil {
		return nil, err
	}
	return orEmpty(s.tx, &evm.GetTransactionByHashResponse{}), nil
}

func (s *scriptedBdsServer) GetTransactionReceipt(ctx context.Context, req *evm.GetTransactionReceiptRequest) (*evm.GetTransactionReceiptResponse, error) {
	if err := s.enter(ctx, func() { s.receiptReq = req }); err != nil {
		return nil, err
	}
	return orEmpty(s.receipt, &evm.GetTransactionReceiptResponse{}), nil
}

func (s *scriptedBdsServer) GetBlockReceipts(ctx context.Context, req *evm.GetBlockReceiptsRequest) (*evm.GetBlockReceiptsResponse, error) {
	if err := s.enter(ctx, func() { s.blockRcpts = req }); err != nil {
		return nil, err
	}
	return orEmpty(s.receipts, &evm.GetBlockReceiptsResponse{}), nil
}

func (s *scriptedBdsServer) GetLogs(ctx context.Context, req *evm.GetLogsRequest) (*evm.GetLogsResponse, error) {
	if err := s.enter(ctx, func() { s.logsReq = req }); err != nil {
		return nil, err
	}
	return orEmpty(s.logs, &evm.GetLogsResponse{}), nil
}

func (s *scriptedBdsServer) TraceBlock(ctx context.Context, req *evm.TraceBlockRequest) (*evm.TraceResponse, error) {
	if err := s.enter(ctx, func() { s.traceBlock = req }); err != nil {
		return nil, err
	}
	return orEmpty(s.trace, &evm.TraceResponse{}), nil
}

func (s *scriptedBdsServer) TraceTransaction(ctx context.Context, req *evm.TraceTransactionRequest) (*evm.TraceResponse, error) {
	if err := s.enter(ctx, func() { s.traceTx = req }); err != nil {
		return nil, err
	}
	return orEmpty(s.trace, &evm.TraceResponse{}), nil
}

func (s *scriptedBdsServer) GetBalance(ctx context.Context, req *evm.GetBalanceRequest) (*evm.GetBalanceResponse, error) {
	if err := s.enter(ctx, func() { s.balanceReq = req }); err != nil {
		return nil, err
	}
	return orEmpty(s.balance, &evm.GetBalanceResponse{}), nil
}

func orEmpty[T any](v *T, empty *T) *T {
	if v == nil {
		return empty
	}
	return v
}

func startScriptedBdsServer(t *testing.T, s *scriptedBdsServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	evm.RegisterRPCQueryServiceServer(srv, s)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func bdsCall(t *testing.T, client *GenericGrpcBdsClient, method, params string) ([]byte, error) {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":%q,"params":%s}`, method, params)))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.SendRequest(ctx, req)
	if err != nil {
		require.Nil(t, resp)
		return nil, err
	}
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Equal(t, int64(7), jrr.ID(), "response must carry the request id")
	return jrr.GetResultBytes(), nil
}

func hash32(b byte) ([]byte, string) {
	h := make([]byte, 32)
	for i := range h {
		h[i] = b
	}
	return h, fmt.Sprintf("0x%x", h)
}

// Each supported JSON-RPC method must reach the matching BDS RPC with the
// parameters translated field by field, and the BDS reply must come back as
// the JSON-RPC result a node would send (null for a miss, [] for no rows).
// The client is armed with chainId 10, so every request that has the optional
// chainId field must carry it.
func TestGrpcBdsClient_RequestResponseMapping(t *testing.T) {
	const chainID = uint64(10)
	hashBytes, hashHex := hash32(0xab)
	addrA := "0x00000000000000000000000000000000000000aa"
	addrB := "0x00000000000000000000000000000000000000bb"
	_, topicHex := hash32(0x11)

	cases := []struct {
		name   string
		method string
		params string
		setup  func(s *scriptedBdsServer)
		check  func(t *testing.T, s *scriptedBdsServer, result []byte)
	}{
		{
			name:   "getBlockByHash with full transactions",
			method: "eth_getBlockByHash",
			params: fmt.Sprintf(`[%q,true]`, hashHex),
			setup: func(s *scriptedBdsServer) {
				s.block = &evm.GetBlockResponse{Block: &evm.BlockHeader{Number: 0x10, Hash: hashBytes}}
			},
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.byHash)
				assert.Equal(t, hashBytes, s.byHash.BlockHash)
				assert.True(t, s.byHash.IncludeTransactions)
				assert.Equal(t, chainID, s.byHash.GetChainId())
				var blk map[string]interface{}
				require.NoError(t, sonic.Unmarshal(result, &blk))
				assert.Equal(t, "0x10", blk["number"])
				assert.Equal(t, hashHex, blk["hash"])
			},
		},
		{
			name:   "getBlockByHash for an unknown block is null",
			method: "eth_getBlockByHash",
			params: fmt.Sprintf(`[%q,false]`, hashHex),
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.byHash)
				assert.False(t, s.byHash.IncludeTransactions)
				assert.Equal(t, "null", string(result))
			},
		},
		{
			name:   "getBlockByNumber given a block hash is served by GetBlockByHash",
			method: "eth_getBlockByNumber",
			params: fmt.Sprintf(`[%q,false]`, hashHex),
			setup: func(s *scriptedBdsServer) {
				s.block = &evm.GetBlockResponse{Block: &evm.BlockHeader{Number: 0x22, Hash: hashBytes}}
			},
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				assert.Nil(t, s.byNumber, "a hash must not be sent as a block number")
				require.NotNil(t, s.byHash)
				assert.Equal(t, hashBytes, s.byHash.BlockHash)
				assert.Equal(t, chainID, s.byHash.GetChainId())
				var blk map[string]interface{}
				require.NoError(t, sonic.Unmarshal(result, &blk))
				assert.Equal(t, "0x22", blk["number"])
			},
		},
		{
			name:   "getBlockByNumber given an EIP-1898 blockHash object misses as null",
			method: "eth_getBlockByNumber",
			params: fmt.Sprintf(`[{"blockHash":%q},true]`, hashHex),
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				assert.Nil(t, s.byNumber)
				require.NotNil(t, s.byHash)
				assert.True(t, s.byHash.IncludeTransactions)
				assert.Equal(t, "null", string(result))
			},
		},
		{
			name:   "getBlockByNumber passes a block tag through and misses as null",
			method: "eth_getBlockByNumber",
			params: `["finalized",false]`,
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.byNumber)
				assert.Equal(t, "finalized", s.byNumber.BlockNumber)
				assert.Equal(t, chainID, s.byNumber.GetChainId())
				assert.Equal(t, "null", string(result))
			},
		},
		{
			name:   "getTransactionByHash for an unknown tx is null",
			method: "eth_getTransactionByHash",
			params: fmt.Sprintf(`[%q]`, hashHex),
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.txReq)
				assert.Equal(t, hashBytes, s.txReq.TransactionHash)
				assert.Equal(t, chainID, s.txReq.GetChainId())
				assert.Equal(t, "null", string(result))
			},
		},
		{
			name:   "getTransactionReceipt for an unknown tx is null",
			method: "eth_getTransactionReceipt",
			params: fmt.Sprintf(`[%q]`, hashHex),
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.receiptReq)
				assert.Equal(t, hashBytes, s.receiptReq.TransactionHash)
				assert.Equal(t, chainID, s.receiptReq.GetChainId())
				assert.Equal(t, "null", string(result))
			},
		},
		{
			name:   "getBlockReceipts by number returns every receipt in order",
			method: "eth_getBlockReceipts",
			params: `["0x10"]`,
			setup: func(s *scriptedBdsServer) {
				s.receipts = &evm.GetBlockReceiptsResponse{Receipts: []*evm.Receipt{
					{TransactionIndex: 0, BlockNumber: 0x10},
					{TransactionIndex: 1, BlockNumber: 0x10},
				}}
			},
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.blockRcpts)
				assert.Equal(t, "0x10", s.blockRcpts.GetBlockNumber())
				assert.Nil(t, s.blockRcpts.BlockHash)
				assert.Equal(t, chainID, s.blockRcpts.GetChainId())
				var rcpts []map[string]interface{}
				require.NoError(t, sonic.Unmarshal(result, &rcpts))
				require.Len(t, rcpts, 2)
				assert.Equal(t, "0x0", rcpts[0]["transactionIndex"])
				assert.Equal(t, "0x1", rcpts[1]["transactionIndex"])
			},
		},
		{
			name:   "getBlockReceipts by hash with no receipts is an empty array",
			method: "eth_getBlockReceipts",
			params: fmt.Sprintf(`[%q]`, hashHex),
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.blockRcpts)
				assert.Equal(t, hashBytes, s.blockRcpts.BlockHash)
				assert.Nil(t, s.blockRcpts.BlockNumber)
				assert.Equal(t, "[]", string(result))
			},
		},
		{
			name:   "getLogs maps an address list, topics and range; no logs is []",
			method: "eth_getLogs",
			params: fmt.Sprintf(`[{"fromBlock":"0x5","toBlock":"0x9","address":[%q,%q],"topics":[%q,null,[%q,%q]]}]`, addrA, addrB, topicHex, topicHex, topicHex),
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.logsReq)
				assert.Equal(t, uint64(5), s.logsReq.GetFromBlock())
				assert.Equal(t, uint64(9), s.logsReq.GetToBlock())
				assert.Nil(t, s.logsReq.BlockHash)
				require.Len(t, s.logsReq.Addresses, 2)
				assert.Equal(t, byte(0xaa), s.logsReq.Addresses[0][19])
				assert.Equal(t, byte(0xbb), s.logsReq.Addresses[1][19])
				require.Len(t, s.logsReq.Topics, 3)
				assert.Len(t, s.logsReq.Topics[0].Values, 1)
				assert.Empty(t, s.logsReq.Topics[1].Values, "null topic is a positional wildcard")
				assert.Len(t, s.logsReq.Topics[2].Values, 2)
				assert.Equal(t, chainID, s.logsReq.GetChainId())
				assert.Equal(t, "[]", string(result))
			},
		},
		{
			name:   "getLogs maps a single address and renders each log",
			method: "eth_getLogs",
			params: fmt.Sprintf(`[{"fromBlock":"0x1","toBlock":"0x1","address":%q}]`, addrA),
			setup: func(s *scriptedBdsServer) {
				s.logs = &evm.GetLogsResponse{Logs: []*evm.Log{{BlockNumber: 1, LogIndex: 3, Address: make([]byte, 20)}}}
			},
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.logsReq)
				require.Len(t, s.logsReq.Addresses, 1)
				assert.Empty(t, s.logsReq.Topics)
				var logs []map[string]interface{}
				require.NoError(t, sonic.Unmarshal(result, &logs))
				require.Len(t, logs, 1)
				assert.Equal(t, "0x1", logs[0]["blockNumber"])
				assert.Equal(t, "0x3", logs[0]["logIndex"])
			},
		},
		{
			name:   "traceBlockByNumber splits tracer, tracerConfig and other options",
			method: "debug_traceBlockByNumber",
			params: `["0x10",{"tracer":"callTracer","tracerConfig":{"onlyTopCall":true},"timeout":"5s"}]`,
			setup: func(s *scriptedBdsServer) {
				s.trace = &evm.TraceResponse{Result: []byte(`[{"txHash":"0x01","result":{"type":"CALL"}}]`)}
			},
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.traceBlock)
				assert.Equal(t, "0x10", s.traceBlock.GetBlockNumber())
				assert.Nil(t, s.traceBlock.BlockHash)
				assert.Equal(t, chainID, s.traceBlock.GetChainId())
				assert.Equal(t, "callTracer", s.traceBlock.Trace.Tracer)
				assert.JSONEq(t, `{"onlyTopCall":true}`, string(s.traceBlock.Trace.TracerConfig))
				assert.JSONEq(t, `{"timeout":"5s"}`, string(s.traceBlock.Trace.Options))
				assert.Equal(t, `[{"txHash":"0x01","result":{"type":"CALL"}}]`, string(result), "result is passed through verbatim")
			},
		},
		{
			name:   "traceBlockByHash without options sends empty options",
			method: "debug_traceBlockByHash",
			params: fmt.Sprintf(`[%q]`, hashHex),
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.traceBlock)
				assert.Equal(t, hashBytes, s.traceBlock.BlockHash)
				assert.Nil(t, s.traceBlock.BlockNumber)
				assert.Empty(t, s.traceBlock.Trace.Tracer)
				assert.Empty(t, s.traceBlock.Trace.TracerConfig)
				assert.Empty(t, s.traceBlock.Trace.Options)
				assert.Equal(t, "null", string(result))
			},
		},
		{
			name:   "traceTransaction maps hash and tracer",
			method: "debug_traceTransaction",
			params: fmt.Sprintf(`[%q,{"tracer":"callTracer"}]`, hashHex),
			setup: func(s *scriptedBdsServer) {
				s.trace = &evm.TraceResponse{Result: []byte(`{"type":"CALL","gasUsed":"0x5208"}`)}
			},
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.traceTx)
				assert.Equal(t, hashBytes, s.traceTx.TransactionHash)
				assert.Equal(t, chainID, s.traceTx.GetChainId())
				assert.Equal(t, "callTracer", s.traceTx.Trace.Tracer)
				assert.Equal(t, `{"type":"CALL","gasUsed":"0x5208"}`, string(result))
			},
		},
		{
			name:   "getBalance renders big-endian wei as a hex quantity",
			method: "eth_getBalance",
			params: fmt.Sprintf(`[%q,"latest"]`, addrA),
			setup: func(s *scriptedBdsServer) {
				s.balance = &evm.GetBalanceResponse{Balance: []byte{0x01, 0x00}}
			},
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.balanceReq)
				assert.Len(t, s.balanceReq.Address, 20)
				assert.Equal(t, "latest", s.balanceReq.GetBlockNumber())
				assert.Equal(t, chainID, s.balanceReq.GetChainId())
				assert.Equal(t, `"0x100"`, string(result))
			},
		},
		{
			name:   "getBalance zero balance and EIP-1898 blockHash",
			method: "eth_getBalance",
			params: fmt.Sprintf(`[%q,{"blockHash":%q}]`, addrB, hashHex),
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				require.NotNil(t, s.balanceReq)
				assert.Equal(t, hashBytes, s.balanceReq.BlockHash)
				assert.Nil(t, s.balanceReq.BlockNumber)
				assert.Equal(t, `"0x0"`, string(result))
			},
		},
		{
			name:   "chainId renders as a hex quantity",
			method: "eth_chainId",
			params: `[]`,
			check: func(t *testing.T, s *scriptedBdsServer, result []byte) {
				assert.Equal(t, `"0xa"`, string(result))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptedBdsServer{chainID: chainID}
			if tc.setup != nil {
				tc.setup(s)
			}
			client := newTestClient(t, startScriptedBdsServer(t, s))
			client.SetExpectedChainId(chainID)

			result, err := bdsCall(t, client, tc.method, tc.params)
			require.NoError(t, err)
			s.mu.Lock()
			defer s.mu.Unlock()
			tc.check(t, s, result)
		})
	}
}

// Malformed parameters must be refused locally — the server never sees the
// call — and surface as an error the caller can see, never as a result.
func TestGrpcBdsClient_InvalidParamsNeverReachServer(t *testing.T) {
	_, hashHex := hash32(0xcd)
	cases := []struct {
		name   string
		method string
		params string
		code   common.ErrorCode
		msg    string
	}{
		{"getBlockByNumber no includeTx", "eth_getBlockByNumber", `["0x1"]`, common.ErrCodeEndpointTransportFailure, "insufficient params"},
		{"getBlockByNumber bool block", "eth_getBlockByNumber", `[true,false]`, common.ErrCodeEndpointTransportFailure, "failed to parse block parameter"},
		{"getBlockByNumber empty object", "eth_getBlockByNumber", `[{"foo":1},false]`, common.ErrCodeEndpointTransportFailure, "failed to parse block parameter"},
		{"getBlockByNumber string includeTx", "eth_getBlockByNumber", `["0x1","yes"]`, common.ErrCodeEndpointTransportFailure, "invalid includeTransactions"},
		{"getBlockByHash one param", "eth_getBlockByHash", fmt.Sprintf(`[%q]`, hashHex), common.ErrCodeEndpointTransportFailure, "insufficient params"},
		{"getBlockByHash numeric hash", "eth_getBlockByHash", `[1,false]`, common.ErrCodeEndpointTransportFailure, "invalid block hash parameter"},
		{"getBlockByHash non-hex hash", "eth_getBlockByHash", `["0xzz",false]`, common.ErrCodeEndpointTransportFailure, "failed to parse block hash"},
		{"getBlockByHash string includeTx", "eth_getBlockByHash", fmt.Sprintf(`[%q,"no"]`, hashHex), common.ErrCodeEndpointTransportFailure, "invalid includeTransactions"},
		{"getLogs no filter", "eth_getLogs", `[]`, common.ErrCodeEndpointTransportFailure, "insufficient params"},
		{"getLogs filter not object", "eth_getLogs", `["x"]`, common.ErrCodeEndpointTransportFailure, "failed to parse params"},
		{"getLogs numeric blockHash", "eth_getLogs", `[{"blockHash":5}]`, common.ErrCodeEndpointTransportFailure, "failed to parse blockHash"},
		{"getLogs blockHash with range", "eth_getLogs", fmt.Sprintf(`[{"blockHash":%q,"fromBlock":"0x1"}]`, hashHex), common.ErrCodeEndpointTransportFailure, "cannot be combined"},
		{"getLogs non-hex blockHash", "eth_getLogs", `[{"blockHash":"0xzz"}]`, common.ErrCodeEndpointTransportFailure, "failed to parse blockHash"},
		{"getLogs bad fromBlock", "eth_getLogs", `[{"fromBlock":"12","toBlock":"0x1"}]`, common.ErrCodeEndpointTransportFailure, "failed to parse fromBlock"},
		{"getLogs bad toBlock", "eth_getLogs", `[{"fromBlock":"0x1","toBlock":"12"}]`, common.ErrCodeEndpointTransportFailure, "failed to parse toBlock"},
		{"getLogs tag range", "eth_getLogs", `[{"fromBlock":"0x1","toBlock":"latest"}]`, common.ErrCodeEndpointTransportFailure, "special block numbers not yet supported"},
		{"getLogs bad address", "eth_getLogs", `[{"fromBlock":"0x1","toBlock":"0x1","address":"0xzz"}]`, common.ErrCodeEndpointTransportFailure, "failed to parse address"},
		{"getLogs bad address in list", "eth_getLogs", `[{"fromBlock":"0x1","toBlock":"0x1","address":["0xzz"]}]`, common.ErrCodeEndpointTransportFailure, "failed to parse address"},
		{"getLogs bad topic", "eth_getLogs", `[{"fromBlock":"0x1","toBlock":"0x1","topics":["0xzz"]}]`, common.ErrCodeEndpointTransportFailure, "failed to parse topic"},
		{"getTransactionByHash no params", "eth_getTransactionByHash", `[]`, common.ErrCodeEndpointTransportFailure, "insufficient params"},
		{"getTransactionByHash numeric", "eth_getTransactionByHash", `[1]`, common.ErrCodeEndpointTransportFailure, "invalid transaction hash"},
		{"getTransactionByHash bad hex", "eth_getTransactionByHash", `["0xzz"]`, common.ErrCodeEndpointTransportFailure, "failed to parse transaction hash"},
		{"getTransactionReceipt no params", "eth_getTransactionReceipt", `[]`, common.ErrCodeEndpointTransportFailure, "insufficient params"},
		{"getTransactionReceipt numeric", "eth_getTransactionReceipt", `[1]`, common.ErrCodeEndpointTransportFailure, "invalid transaction hash"},
		{"getTransactionReceipt bad hex", "eth_getTransactionReceipt", `["0xzz"]`, common.ErrCodeEndpointTransportFailure, "failed to parse transaction hash"},
		{"getBlockReceipts no params", "eth_getBlockReceipts", `[]`, common.ErrCodeEndpointTransportFailure, "insufficient params"},
		{"getBlockReceipts bool block", "eth_getBlockReceipts", `[true]`, common.ErrCodeEndpointTransportFailure, "failed to parse block parameter"},
		{"svm getBlock no slot", "getBlock", `[]`, common.ErrCodeEndpointTransportFailure, "insufficient params"},
		{"svm getBlock hex slot", "getBlock", `["0x10"]`, common.ErrCodeEndpointTransportFailure, "slot"},
		{"svm getBlock config not object", "getBlock", `[1,"json"]`, common.ErrCodeEndpointTransportFailure, "failed to parse getBlock config"},
		{"svm getBlock raw encoding", "getBlock", `[1,{"encoding":"base64"}]`, common.ErrCodeEndpointUnsupported, "base64"},
		{"traceBlockByNumber no params", "debug_traceBlockByNumber", `[]`, common.ErrCodeEndpointUnsupported, "missing block parameter"},
		{"traceBlockByNumber numeric block", "debug_traceBlockByNumber", `[16]`, common.ErrCodeEndpointUnsupported, "must be a string"},
		{"traceBlockByNumber options not object", "debug_traceBlockByNumber", `["0x1","callTracer"]`, common.ErrCodeEndpointUnsupported, "must be an object"},
		{"traceBlockByNumber numeric tracer", "debug_traceBlockByNumber", `["0x1",{"tracer":1}]`, common.ErrCodeEndpointUnsupported, "tracer must be a string"},
		{"traceBlockByHash short hash", "debug_traceBlockByHash", `["0xabcd"]`, common.ErrCodeEndpointUnsupported, "want 32"},
		{"traceTransaction no params", "debug_traceTransaction", `[]`, common.ErrCodeEndpointUnsupported, "missing transaction hash"},
		{"traceTransaction bad hex", "debug_traceTransaction", `["0xzz"]`, common.ErrCodeEndpointUnsupported, "debug_traceTransaction"},
		{"getBalance no block", "eth_getBalance", `["0x00000000000000000000000000000000000000aa"]`, common.ErrCodeEndpointUnsupported, "needs an address and a block"},
		{"getBalance short address", "eth_getBalance", `["0xaa","latest"]`, common.ErrCodeEndpointUnsupported, "want 20"},
		{"getBalance bool block", "eth_getBalance", `["0x00000000000000000000000000000000000000aa",true]`, common.ErrCodeEndpointUnsupported, "must be a string or an object"},
		{"getBalance empty object", "eth_getBalance", `["0x00000000000000000000000000000000000000aa",{}]`, common.ErrCodeEndpointUnsupported, "needs a string blockHash or blockNumber"},
	}

	s := &scriptedBdsServer{chainID: 1}
	client := newTestClient(t, startScriptedBdsServer(t, s))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := s.calls.Load()
			_, err := bdsCall(t, client, tc.method, tc.params)
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, tc.code), "want %s, got %v", tc.code, err)
			assert.Contains(t, err.Error(), tc.msg)
			assert.Equal(t, before, s.calls.Load(), "invalid params must not reach the server")
		})
	}
}

// Every gRPC status a BDS server can answer with must land on the eRPC error
// type the upstream policies key on (retry, failover, missing-data handling),
// keeping the server's message.
func TestGrpcBdsClient_GrpcStatusToErpcError(t *testing.T) {
	cases := []struct {
		code codes.Code
		want common.ErrorCode
	}{
		{codes.Unavailable, common.ErrCodeEndpointServerSideException},
		{codes.Internal, common.ErrCodeEndpointServerSideException},
		{codes.Unknown, common.ErrCodeEndpointServerSideException},
		{codes.FailedPrecondition, common.ErrCodeEndpointServerSideException},
		{codes.NotFound, common.ErrCodeEndpointMissingData},
		{codes.OutOfRange, common.ErrCodeEndpointMissingData},
		{codes.InvalidArgument, common.ErrCodeEndpointClientSideException},
		{codes.ResourceExhausted, common.ErrCodeEndpointCapacityExceeded},
		{codes.Unauthenticated, common.ErrCodeEndpointUnauthorized},
		{codes.PermissionDenied, common.ErrCodeEndpointUnauthorized},
		{codes.Unimplemented, common.ErrCodeEndpointUnsupported},
		{codes.Canceled, common.ErrCodeEndpointRequestCanceled},
		{codes.DeadlineExceeded, common.ErrCodeEndpointRequestTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.code.String(), func(t *testing.T) {
			msg := "bds says " + strings.ToLower(tc.code.String())
			s := &scriptedBdsServer{err: status.Error(tc.code, msg)}
			client := newTestClient(t, startScriptedBdsServer(t, s))
			_, err := bdsCall(t, client, "eth_chainId", `[]`)
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, tc.want), "want %s, got %v", tc.want, err)
			assert.Contains(t, err.Error(), msg)
		})
	}
}

// A gRPC failure on any method's call path is classified the same way: the
// handler's wrapping must not hide the status from the classifier.
func TestGrpcBdsClient_EveryMethodPropagatesGrpcStatus(t *testing.T) {
	_, hashHex := hash32(0x01)
	calls := []struct{ method, params string }{
		{"eth_getBlockByNumber", `["0x1",false]`},
		{"eth_getBlockByNumber", fmt.Sprintf(`[%q,false]`, hashHex)},
		{"eth_getBlockByHash", fmt.Sprintf(`[%q,false]`, hashHex)},
		{"eth_getLogs", `[{"fromBlock":"0x1","toBlock":"0x2"}]`},
		{"eth_getTransactionByHash", fmt.Sprintf(`[%q]`, hashHex)},
		{"eth_getTransactionReceipt", fmt.Sprintf(`[%q]`, hashHex)},
		{"eth_getBlockReceipts", `["0x1"]`},
		{"eth_chainId", `[]`},
		{"debug_traceBlockByNumber", `["0x1",{"tracer":"callTracer"}]`},
		{"debug_traceBlockByHash", fmt.Sprintf(`[%q]`, hashHex)},
		{"debug_traceTransaction", fmt.Sprintf(`[%q]`, hashHex)},
		{"eth_getBalance", `["0x00000000000000000000000000000000000000aa","0x1"]`},
	}
	s := &scriptedBdsServer{err: status.Error(codes.NotFound, "block not indexed")}
	client := newTestClient(t, startScriptedBdsServer(t, s))
	for _, c := range calls {
		t.Run(c.method+c.params[:2], func(t *testing.T) {
			_, err := bdsCall(t, client, c.method, c.params)
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData), "got %v", err)
			assert.Contains(t, err.Error(), "block not indexed")
		})
	}
}

// A request body that is not JSON-RPC fails before any connection is used.
func TestGrpcBdsClient_UnparsableRequestIsUpstreamRequestError(t *testing.T) {
	s := &scriptedBdsServer{}
	client := newTestClient(t, startScriptedBdsServer(t, s))
	_, err := client.SendRequest(context.Background(), common.NewNormalizedRequest([]byte(`not json`)))
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamRequest), "got %v", err)
	assert.Zero(t, s.calls.Load())
}

// A server answering eth_chainId for another chain than the one this client
// is armed for is a cross-wired endpoint: the caller must get an error, never
// the foreign chain id.
func TestGrpcBdsClient_ChainIdMismatchIsError(t *testing.T) {
	s := &scriptedBdsServer{chainID: 56}
	client := newTestClient(t, startScriptedBdsServer(t, s))
	client.SetExpectedChainId(1)

	result, err := bdsCall(t, client, "eth_chainId", `[]`)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure), "got %v", err)
	assert.Contains(t, err.Error(), common.ErrCodeEndpointChainIdMismatch)
}

// An upstream configured for one chain must refuse to build a client against
// an endpoint that proves to serve another; the matching chain builds fine and
// stamps its chainId on requests.
func TestNewGrpcBdsClient_VerifiesConfiguredChainId(t *testing.T) {
	s := &scriptedBdsServer{chainID: 1}
	addr := startScriptedBdsServer(t, s)
	parsedURL, err := url.Parse("grpc://" + addr)
	require.NoError(t, err)
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	wrong := common.NewFakeUpstream("bds-wrong")
	wrong.Config().Evm = &common.EvmUpstreamConfig{ChainId: 5}
	_, err = NewGrpcBdsClient(ctx, &logger, "prj", wrong, parsedURL, 1, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cross-wired")

	right := common.NewFakeUpstream("bds-right")
	right.Config().Evm = &common.EvmUpstreamConfig{ChainId: 1}
	c, err := NewGrpcBdsClient(ctx, &logger, "prj", right, parsedURL, 1, "")
	require.NoError(t, err)
	t.Cleanup(c.(*GenericGrpcBdsClient).Shutdown)
	assert.Equal(t, ClientTypeGrpcBds, c.GetType())

	_, err = bdsCall(t, c.(*GenericGrpcBdsClient), "eth_getBlockByNumber", `["0x1",false]`)
	require.NoError(t, err)
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotNil(t, s.byNumber)
	assert.Equal(t, uint64(1), s.byNumber.GetChainId())
}

// Caller-side ends of a request are not upstream failures: an expired caller
// deadline is a request timeout, a caller cancel mid-call is a cancellation.
func TestGrpcBdsClient_CallerContextEndings(t *testing.T) {
	addr, wedged, stop := startWedgedServer(t)
	defer stop()
	client := newTestClient(t, addr)
	req := func() *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
	}

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	_, err := client.SendRequest(expired, req())
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestTimeout), "got %v", err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.SendRequest(ctx, req())
		done <- err
	}()
	require.Eventually(t, func() bool { return wedged.started.Load() >= 1 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled), "got %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("SendRequest did not return after the caller canceled")
	}
}

// A client whose pool has no connection left must fail as a transport
// failure, and QueryClient must report no client rather than panic.
func TestGrpcBdsClient_EmptyPool(t *testing.T) {
	s := &scriptedBdsServer{chainID: 1}
	client := newTestClient(t, startScriptedBdsServer(t, s))
	require.NotNil(t, client.QueryClient(), "a live pool hands out a query client")

	client.pool.poolMu.Lock()
	saved := client.pool.conns
	client.pool.conns = nil
	client.pool.poolMu.Unlock()
	t.Cleanup(func() {
		client.pool.poolMu.Lock()
		client.pool.conns = saved
		client.pool.poolMu.Unlock()
	})

	assert.Nil(t, client.QueryClient())
	_, err := bdsCall(t, client, "eth_chainId", `[]`)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure), "got %v", err)
	assert.Contains(t, err.Error(), "no available connections")
	assert.Zero(t, s.calls.Load())
}

// Errors that carry no gRPC status are transport failures; nil stays nil.
func TestGrpcBdsClient_NormalizeNonGrpcError(t *testing.T) {
	client := newClientWithPoolSize(t, 1)
	assert.NoError(t, client.normalizeGrpcError(nil))
	err := client.normalizeGrpcError(fmt.Errorf("wrapped: %w", errors.New("connection reset")))
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure), "got %v", err)
	assert.Contains(t, err.Error(), "connection reset")
}

// With detailed tracing on, response metadata the server sends back is
// recorded on the client span: single values as strings, repeated keys as
// lists.
func TestGrpcBdsClient_DetailedTracingRecordsResponseMetadata(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	common.SetTracerProviderForTest(tp)
	common.IsTracingDetailed = true
	t.Cleanup(func() {
		common.IsTracingDetailed = false
		common.IsTracingEnabled = false
	})

	s := &scriptedBdsServer{
		chainID:  1,
		headerMD: metadata.Pairs("x-served-by", "reader-3", "x-shard", "a", "x-shard", "b"),
		block:    &evm.GetBlockResponse{Block: &evm.BlockHeader{Number: 9, Hash: []byte{1}}},
	}
	client := newTestClient(t, startScriptedBdsServer(t, s))

	_, err := bdsCall(t, client, "eth_chainId", `[]`)
	require.NoError(t, err)
	_, err = bdsCall(t, client, "eth_getBlockByNumber", `["0x9",false]`)
	require.NoError(t, err)
	_, hashHex := hash32(0x02)
	_, err = bdsCall(t, client, "eth_getBlockByNumber", fmt.Sprintf(`[%q,false]`, hashHex))
	require.NoError(t, err)
	require.NoError(t, tp.ForceFlush(context.Background()))

	attrs := map[string]map[string]interface{}{}
	for _, sp := range exp.GetSpans() {
		if attrs[sp.Name] == nil {
			attrs[sp.Name] = map[string]interface{}{}
		}
		for _, a := range sp.Attributes {
			attrs[sp.Name][string(a.Key)] = a.Value.AsInterface()
		}
	}
	send := attrs["GrpcBdsClient.SendRequest"]
	require.NotNil(t, send, "SendRequest span missing")
	assert.Equal(t, "reader-3", send["grpc.response.metadata.x-served-by"])
	assert.Equal(t, []string{"a", "b"}, send["grpc.response.metadata.x-shard"])
	assert.Equal(t, "7", send["request.id"])

	byNum := attrs["GrpcBdsClient.GetBlockByNumber"]
	require.NotNil(t, byNum, "GetBlockByNumber detail span missing")
	assert.Equal(t, true, byNum["response_has_block"])
	assert.Equal(t, int64(9), byNum["response_block_number"])
	assert.Equal(t, "reader-3", byNum["grpc.response.metadata.x-served-by"])

	byHash := attrs["GrpcBdsClient.GetBlockByHash"]
	require.NotNil(t, byHash, "GetBlockByHash detail span missing")
	assert.Equal(t, hashHex[2:], byHash["block_hash"])
}

// The maintainer's recycle swaps a fresh verified conn into the slot; a
// replacement that proves to serve another chain is discarded and the slot
// keeps its conn.
func TestBdsPool_RecycleConn(t *testing.T) {
	s := &scriptedBdsServer{chainID: 1}
	client := newTestClient(t, startScriptedBdsServer(t, s))
	p := client.pool
	slot0 := func() *bdsConn {
		p.poolMu.RLock()
		defer p.poolMu.RUnlock()
		return p.conns[0]
	}

	old := slot0()
	p.expectedChainId.Store(2)
	p.recycleConn(old, "chainid_mismatch")
	assert.Same(t, old, slot0(), "a wrong-chain replacement must not be installed")

	p.expectedChainId.Store(1)
	p.recycleConn(old, "closed")
	replaced := slot0()
	assert.NotSame(t, old, replaced, "a verified replacement takes the slot")
	assert.Positive(t, old.closedAt.Load())

	p.recycleConn(old, "age")
	assert.Same(t, replaced, slot0(), "a conn no longer pooled is not recycled again")

	_, err := bdsCall(t, client, "eth_chainId", `[]`)
	require.NoError(t, err, "pool keeps serving after a recycle")
}
