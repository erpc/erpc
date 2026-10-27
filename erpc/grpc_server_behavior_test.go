package erpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// grpcBehaviorHead is the latest (and finalized) block the mocked upstream
// reports until a test moves its head.
const grpcBehaviorHead = 0x100

// grpcBehaviorUpstream is a JSON-RPC upstream whose answer per method can be
// replaced while eRPC runs; it records the params of the last call per method.
type grpcBehaviorUpstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	replies map[string]string // method -> `"result":...` or `"error":{...}`
	params  map[string]string
	head    atomic.Uint64 // latest and finalized
}

func newGrpcBehaviorUpstream(t *testing.T) *grpcBehaviorUpstream {
	t.Helper()
	u := &grpcBehaviorUpstream{replies: map[string]string{}, params: map[string]string{}}
	u.head.Store(grpcBehaviorHead)
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Id     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := sonic.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id := string(req.Id)
		if id == "" {
			id = "1"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + id + `,` + u.reply(req.Method, req.Params) + `}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *grpcBehaviorUpstream) reply(method string, params json.RawMessage) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	raw := string(params)
	isHead := method == "eth_getBlockByNumber" && (strings.Contains(raw, "latest") || strings.Contains(raw, "finalized"))
	if !isHead {
		u.params[method] = raw
		if frag, ok := u.replies[method]; ok {
			return frag
		}
	}
	switch method {
	case "eth_chainId":
		return `"result":"0x7b"`
	case "eth_syncing":
		return `"result":false`
	case "eth_getBlockByNumber":
		n := u.head.Load()
		var p []interface{}
		if sonic.Unmarshal(params, &p) == nil && len(p) > 0 {
			if s, ok := p[0].(string); ok && strings.HasPrefix(s, "0x") {
				n, _ = strconv.ParseUint(s[2:], 16, 64)
			}
		}
		blk := qtBlock(n)
		// A full-width bloom, so the chain tracker takes the header as a vote.
		blk["logsBloom"] = "0x" + strings.Repeat("00", 256)
		b, _ := sonic.Marshal(blk)
		return `"result":` + string(b)
	}
	return `"error":{"code":-32601,"message":"the method ` + method + ` does not exist/is not available"}`
}

func (u *grpcBehaviorUpstream) set(method, frag string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.replies[method] = frag
}

func (u *grpcBehaviorUpstream) setResult(method string, result interface{}) {
	b, err := sonic.Marshal(result)
	if err != nil {
		panic(err)
	}
	u.set(method, `"result":`+string(b))
}

func (u *grpcBehaviorUpstream) lastParams(method string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.params[method]
}

type grpcBehaviorFixture struct {
	up   *grpcBehaviorUpstream
	erpc *ERPC
	gs   *GrpcServer
	conn *grpc.ClientConn
	rpc  evm.RPCQueryServiceClient
}

// newGrpcBehaviorFixture serves project "main" (no auth) and project
// "guarded" (network auth allowing only 203.0.113.9) on one eRPC gRPC server
// that trusts 127.0.0.1 as a forwarder of x-forwarded-for.
func newGrpcBehaviorFixture(t *testing.T) *grpcBehaviorFixture {
	t.Helper()
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	up := newGrpcBehaviorUpstream(t)
	upstreamCfg := func() *common.UpstreamConfig {
		return &common.UpstreamConfig{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: up.srv.URL,
			JsonRpc:  &common.JsonRpcUpstreamConfig{SupportsBatch: util.BoolPtr(false)},
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(time.Hour),
				StatePollerDebounce: common.Duration(100 * time.Millisecond),
				SkipSyncingCheck:    util.BoolPtr(true),
				QueryShim:           &common.EvmQueryShimConfig{Enabled: util.BoolPtr(true), AllowedMethods: []string{"eth_queryBlocks"}},
			},
		}
	}
	networkCfg := func() *common.NetworkConfig {
		return &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
			Failsafe:     []*common.FailsafeConfig{{Retry: &common.RetryPolicyConfig{MaxAttempts: 1}}},
		}
	}
	cfg := &common.Config{
		Server: &common.ServerConfig{MaxTimeout: common.Duration(5 * time.Second).Ptr()},
		Projects: []*common.ProjectConfig{
			{Id: "main", Networks: []*common.NetworkConfig{networkCfg()}, Upstreams: []*common.UpstreamConfig{upstreamCfg()}},
			{
				Id: "guarded",
				Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{{
					Type:    common.AuthTypeNetwork,
					Network: &common.NetworkStrategyConfig{AllowedIPs: []string{"203.0.113.9"}},
				}}},
				Networks:  []*common.NetworkConfig{networkCfg()},
				Upstreams: []*common.UpstreamConfig{upstreamCfg()},
			},
		},
	}
	require.NoError(t, cfg.SetDefaults(nil))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	erpcInstance, err := NewERPC(ctx, &log.Logger, nil, nil, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	gs, err := NewGrpcServer(ctx, &log.Logger, &common.ServerConfig{
		GrpcMaxRecvMsgSize:  util.IntPtr(1 << 20),
		GrpcMaxSendMsgSize:  util.IntPtr(1 << 20),
		GrpcReflection:      util.BoolPtr(false),
		TrustedIPForwarders: []string{"127.0.0.1"},
		TrustedIPHeaders:    []string{"X-Forwarded-For"},
	}, erpcInstance)
	require.NoError(t, err)
	_, hasReflection := gs.server.GetServiceInfo()["grpc.reflection.v1.ServerReflection"]
	require.False(t, hasReflection, "grpcReflection=false must not register reflection")

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.server.Serve(lis) }()
	t.Cleanup(gs.server.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return &grpcBehaviorFixture{up: up, erpc: erpcInstance, gs: gs, conn: conn, rpc: evm.NewRPCQueryServiceClient(conn)}
}

// grpcBehaviorCtx is an outgoing context carrying project, chain and any extra metadata pairs.
func grpcBehaviorCtx(t *testing.T, project string, kv ...string) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	md := metadata.MD{}
	if project != "" {
		md.Set("x-erpc-project", project)
		md.Set("x-erpc-chain-id", "123")
	}
	for i := 0; i+1 < len(kv); i += 2 {
		md.Set(kv[i], kv[i+1])
	}
	return metadata.NewOutgoingContext(ctx, md)
}

func grpcBehaviorBareServer(t *testing.T, forwarders, headers []string) *GrpcServer {
	t.Helper()
	gs, err := NewGrpcServer(context.Background(), &log.Logger, &common.ServerConfig{
		GrpcMaxRecvMsgSize:  util.IntPtr(1 << 20),
		GrpcMaxSendMsgSize:  util.IntPtr(1 << 20),
		TrustedIPForwarders: forwarders,
		TrustedIPHeaders:    headers,
	}, nil)
	require.NoError(t, err)
	return gs
}

func grpcBehaviorReceipt(n, i uint64) map[string]interface{} {
	r := qtReceipts(n)[i].(map[string]interface{})
	r["to"] = qtAddrB
	r["logs"] = []interface{}{grpcBehaviorLog(n, 0)}
	return r
}

func grpcBehaviorLog(n uint64, idx int) map[string]interface{} {
	return map[string]interface{}{
		"address": qtToken, "topics": []interface{}{qtTopic}, "data": "0x01", "blockNumber": fmt.Sprintf("0x%x", n),
		"blockHash": qtHash(n), "transactionHash": qtTxHash(n, 0), "transactionIndex": "0x0", "logIndex": fmt.Sprintf("0x%x", idx),
	}
}

func TestGrpcServerBehavior_MissingMetadataRejectedByEveryMethod(t *testing.T) {
	fx := newGrpcBehaviorFixture(t)
	query := evm.NewQueryServiceClient(fx.conn)
	stream := evm.NewStreamServiceClient(fx.conn)

	calls := map[string]func(ctx context.Context) error{
		"ChainId": func(ctx context.Context) error { _, err := fx.rpc.ChainId(ctx, &evm.ChainIdRequest{}); return err },
		"GetBlockByNumber": func(ctx context.Context) error {
			_, err := fx.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x1"})
			return err
		},
		"GetBlockByHash": func(ctx context.Context) error {
			_, err := fx.rpc.GetBlockByHash(ctx, &evm.GetBlockByHashRequest{BlockHash: []byte{1}})
			return err
		},
		"GetLogs": func(ctx context.Context) error { _, err := fx.rpc.GetLogs(ctx, &evm.GetLogsRequest{}); return err },
		"GetTransactionByHash": func(ctx context.Context) error {
			_, err := fx.rpc.GetTransactionByHash(ctx, &evm.GetTransactionByHashRequest{TransactionHash: []byte{1}})
			return err
		},
		"GetTransactionReceipt": func(ctx context.Context) error {
			_, err := fx.rpc.GetTransactionReceipt(ctx, &evm.GetTransactionReceiptRequest{TransactionHash: []byte{1}})
			return err
		},
		"GetBlockReceipts": func(ctx context.Context) error {
			_, err := fx.rpc.GetBlockReceipts(ctx, &evm.GetBlockReceiptsRequest{BlockHash: []byte{1}})
			return err
		},
		"QueryBlocks": func(ctx context.Context) error {
			s, err := query.QueryBlocks(ctx, &evm.QueryBlocksRequest{})
			_, code := grpcPages(t, s, err)
			return status.Error(code, "")
		},
		"StreamBlocks": func(ctx context.Context) error {
			s, err := stream.StreamBlocks(ctx, &evm.StreamBlocksRequest{})
			require.NoError(t, err)
			_, err = s.Recv()
			return err
		},
	}

	for name, call := range calls {
		t.Run(name+"/no project", func(t *testing.T) {
			ctx := grpcBehaviorCtx(t, "")
			assert.Equal(t, codes.InvalidArgument, status.Code(call(ctx)))
		})
		t.Run(name+"/no chain", func(t *testing.T) {
			ctx := grpcBehaviorCtx(t, "", "x-erpc-project", "main")
			assert.Equal(t, codes.InvalidArgument, status.Code(call(ctx)))
		})
		t.Run(name+"/malformed basic auth", func(t *testing.T) {
			ctx := grpcBehaviorCtx(t, "main", "authorization", "Basic !!not-base64!!")
			assert.Equal(t, codes.Unauthenticated, status.Code(call(ctx)))
		})
	}
}

func TestGrpcServerBehavior_ExtractRequestInput(t *testing.T) {
	gs := grpcBehaviorBareServer(t, nil, nil)
	peerCtx := func(md metadata.MD) context.Context {
		ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("198.51.100.7"), Port: 4000}})
		return metadata.NewIncomingContext(ctx, md)
	}

	t.Run("no incoming metadata", func(t *testing.T) {
		_, err := gs.extractRequestInput(context.Background(), "eth_chainId")
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "missing metadata")
	})
	t.Run("project required", func(t *testing.T) {
		_, err := gs.extractRequestInput(peerCtx(metadata.Pairs("x-erpc-chain-id", "1")), "eth_chainId")
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "x-erpc-project")
	})
	t.Run("chain required", func(t *testing.T) {
		_, err := gs.extractRequestInput(peerCtx(metadata.Pairs("x-erpc-project", "p")), "eth_chainId")
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "x-erpc-chain-id")
	})
	t.Run("basic auth without colon", func(t *testing.T) {
		md := metadata.Pairs("x-erpc-project", "p", "x-erpc-chain-id", "1",
			"authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("nocolon")))
		_, err := gs.extractRequestInput(peerCtx(md), "eth_chainId")
		assert.Equal(t, codes.Unauthenticated, status.Code(err))
	})
	t.Run("defaults and passthrough fields", func(t *testing.T) {
		md := metadata.Pairs("x-erpc-project", "p", "x-erpc-chain-id", "42",
			"user-agent", "bds-client/1.0", "x-erpc-user-id", "alice", "x-erpc-secret-token", "s3cret")
		in, err := gs.extractRequestInput(peerCtx(md), "eth_getLogs")
		require.NoError(t, err)
		assert.Equal(t, "p", in.ProjectId)
		assert.Equal(t, "42", in.ChainId)
		assert.Equal(t, "evm", in.Architecture, "architecture defaults to evm")
		assert.Equal(t, "198.51.100.7", in.ClientIP)
		assert.Contains(t, in.UserAgent, "bds-client/1.0")
		assert.Equal(t, "alice", in.TrustedUserId)
		require.NotNil(t, in.AuthPayload)
		assert.Equal(t, common.AuthTypeSecret, in.AuthPayload.Type)
		assert.Equal(t, "eth_getLogs", in.AuthPayload.Method)
	})
	t.Run("explicit architecture and bearer", func(t *testing.T) {
		md := metadata.Pairs("x-erpc-project", "p", "x-erpc-chain-id", "1",
			"x-erpc-architecture", "svm", "authorization", "Bearer tok")
		in, err := gs.extractRequestInput(peerCtx(md), "eth_chainId")
		require.NoError(t, err)
		assert.Equal(t, "svm", in.Architecture)
		assert.Equal(t, common.AuthTypeJwt, in.AuthPayload.Type)
		assert.Equal(t, "tok", in.AuthPayload.Jwt.Token)
	})
}

func TestGrpcServerBehavior_ChainId(t *testing.T) {
	fx := newGrpcBehaviorFixture(t)

	resp, err := fx.rpc.ChainId(grpcBehaviorCtx(t, "main"), &evm.ChainIdRequest{})
	require.NoError(t, err)
	assert.Equal(t, uint64(123), resp.ChainId)

	_, err = fx.rpc.ChainId(grpcBehaviorCtx(t, "no-such-project"), &evm.ChainIdRequest{})
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "no-such-project")
}

func TestGrpcServerBehavior_ClientIPAuthThroughTrustedForwarder(t *testing.T) {
	fx := newGrpcBehaviorFixture(t)

	cases := []struct {
		name string
		kv   []string
		want codes.Code
	}{
		// The test client connects from 127.0.0.1, a trusted forwarder.
		{"forwarded allowed client", []string{"x-forwarded-for", "203.0.113.9"}, codes.OK},
		{"forwarded chain picks nearest untrusted hop", []string{"x-forwarded-for", "203.0.113.9, 127.0.0.1"}, codes.OK},
		{"forwarded disallowed client", []string{"x-forwarded-for", "198.51.100.1"}, codes.Unauthenticated},
		{"no forwarded header uses the peer", nil, codes.Unauthenticated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := fx.rpc.ChainId(grpcBehaviorCtx(t, "guarded", tc.kv...), &evm.ChainIdRequest{})
			require.Equal(t, tc.want, status.Code(err), "err=%v", err)
			if tc.want == codes.OK {
				assert.Equal(t, uint64(123), resp.ChainId)
			}
		})
	}
}

func TestGrpcServerBehavior_GetBlockByNumber(t *testing.T) {
	fx := newGrpcBehaviorFixture(t)
	ctx := grpcBehaviorCtx(t, "main")

	t.Run("full transactions decoded", func(t *testing.T) {
		resp, err := fx.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x64", IncludeTransactions: true})
		require.NoError(t, err)
		require.NotNil(t, resp.Block)
		assert.Equal(t, uint64(0x64), resp.Block.Number)
		assert.Equal(t, qtHash(0x64), evm.BytesToHex(resp.Block.Hash))
		assert.Equal(t, qtHash(0x63), evm.BytesToHex(resp.Block.ParentHash))
		assert.Equal(t, uint64(1000+0x64), resp.Block.Timestamp)
		require.Len(t, resp.FullTransactions, 2)
		assert.Equal(t, qtTxHash(0x64, 0), evm.BytesToHex(resp.FullTransactions[0].Hash))
		assert.Equal(t, [][]byte{resp.FullTransactions[0].Hash, resp.FullTransactions[1].Hash}, resp.Transactions)
		assert.Contains(t, fx.up.lastParams("eth_getBlockByNumber"), `"0x64",true`)
	})
	t.Run("null block under the head is not found", func(t *testing.T) {
		fx.up.set("eth_getBlockByNumber", `"result":null`)
		_, err := fx.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x65"})
		assert.Equal(t, codes.NotFound, status.Code(err), "err=%v", err)
	})
	t.Run("null block past the head is an empty response", func(t *testing.T) {
		fx.up.set("eth_getBlockByNumber", `"result":null`)
		resp, err := fx.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x100000"})
		require.NoError(t, err)
		assert.Nil(t, resp.Block)
		assert.Empty(t, resp.Transactions)
	})
	t.Run("non-object result is internal", func(t *testing.T) {
		fx.up.set("eth_getBlockByNumber", `"result":"0xnotablock"`)
		_, err := fx.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x66"})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("undecodable block field is internal", func(t *testing.T) {
		fx.up.set("eth_getBlockByNumber", `"result":{"number":"0x67","hash":"0xzz"}`)
		_, err := fx.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x67"})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("upstream invalid params is invalid argument", func(t *testing.T) {
		fx.up.set("eth_getBlockByNumber", `"error":{"code":-32602,"message":"invalid argument 0: hex string without 0x prefix"}`)
		_, err := fx.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x68"})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "err=%v", err)
	})
}

func TestGrpcServerBehavior_GetBlockByHash(t *testing.T) {
	fx := newGrpcBehaviorFixture(t)
	ctx := grpcBehaviorCtx(t, "main")
	hash := evm.MustHexToBytes(qtHash(0x64))

	t.Run("hashes only", func(t *testing.T) {
		block := qtBlock(0x64)
		block["transactions"] = []interface{}{qtTxHash(0x64, 0), qtTxHash(0x64, 1)}
		fx.up.setResult("eth_getBlockByHash", block)
		resp, err := fx.rpc.GetBlockByHash(ctx, &evm.GetBlockByHashRequest{BlockHash: hash})
		require.NoError(t, err)
		assert.Equal(t, uint64(0x64), resp.Block.Number)
		assert.Empty(t, resp.FullTransactions)
		require.Len(t, resp.Transactions, 2)
		assert.Equal(t, qtTxHash(0x64, 1), evm.BytesToHex(resp.Transactions[1]))
		assert.Contains(t, fx.up.lastParams("eth_getBlockByHash"), `"`+qtHash(0x64)+`",false`)
	})
	t.Run("null block", func(t *testing.T) {
		fx.up.set("eth_getBlockByHash", `"result":null`)
		resp, err := fx.rpc.GetBlockByHash(ctx, &evm.GetBlockByHashRequest{BlockHash: evm.MustHexToBytes(qtHash(0x65))})
		require.NoError(t, err)
		assert.Nil(t, resp.Block)
	})
	t.Run("non-object result", func(t *testing.T) {
		fx.up.set("eth_getBlockByHash", `"result":[1,2]`)
		_, err := fx.rpc.GetBlockByHash(ctx, &evm.GetBlockByHashRequest{BlockHash: evm.MustHexToBytes(qtHash(0x66))})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("undecodable field", func(t *testing.T) {
		fx.up.set("eth_getBlockByHash", `"result":{"number":"zz"}`)
		_, err := fx.rpc.GetBlockByHash(ctx, &evm.GetBlockByHashRequest{BlockHash: evm.MustHexToBytes(qtHash(0x67))})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("upstream revert is failed precondition", func(t *testing.T) {
		fx.up.set("eth_getBlockByHash", `"error":{"code":3,"message":"execution reverted","data":"0x"}`)
		_, err := fx.rpc.GetBlockByHash(ctx, &evm.GetBlockByHashRequest{BlockHash: evm.MustHexToBytes(qtHash(0x68))})
		assert.Equal(t, codes.FailedPrecondition, status.Code(err), "err=%v", err)
	})
}

func TestGrpcServerBehavior_GetLogs(t *testing.T) {
	fx := newGrpcBehaviorFixture(t)
	ctx := grpcBehaviorCtx(t, "main")
	addrA, addrB := evm.MustHexToBytes(qtAddrA), evm.MustHexToBytes(qtAddrB)
	topic := evm.MustHexToBytes(qtTopic)
	other := evm.MustHexToBytes(qtHash(9))

	t.Run("filter translated and logs decoded", func(t *testing.T) {
		fx.up.setResult("eth_getLogs", []interface{}{grpcBehaviorLog(0x64, 0), grpcBehaviorLog(0x64, 1)})
		resp, err := fx.rpc.GetLogs(ctx, &evm.GetLogsRequest{
			FromBlock: evm.Uint64Ptr(0x64), ToBlock: evm.Uint64Ptr(0x65),
			Addresses: [][]byte{addrA, addrB},
			Topics:    []*evm.TopicFilter{{Values: [][]byte{topic}}, nil, {Values: [][]byte{topic, other}}},
		})
		require.NoError(t, err)
		require.Len(t, resp.Logs, 2)
		assert.Equal(t, qtToken, evm.BytesToHex(resp.Logs[0].Address))
		assert.Equal(t, uint32(1), resp.Logs[1].LogIndex)
		assert.Equal(t, uint64(0x64), resp.Logs[0].BlockNumber)
		assert.Equal(t, [][]byte{topic}, resp.Logs[0].Topics)

		var sent []map[string]interface{}
		require.NoError(t, sonic.UnmarshalString(fx.up.lastParams("eth_getLogs"), &sent))
		require.Len(t, sent, 1)
		assert.Equal(t, "0x64", sent[0]["fromBlock"])
		assert.Equal(t, "0x65", sent[0]["toBlock"])
		assert.Equal(t, []interface{}{qtAddrA, qtAddrB}, sent[0]["address"])
		assert.Equal(t, []interface{}{qtTopic, nil, []interface{}{qtTopic, qtHash(9)}}, sent[0]["topics"])
	})
	t.Run("single address sent as a string", func(t *testing.T) {
		fx.up.setResult("eth_getLogs", []interface{}{})
		resp, err := fx.rpc.GetLogs(ctx, &evm.GetLogsRequest{FromBlock: evm.Uint64Ptr(0x66), ToBlock: evm.Uint64Ptr(0x66), Addresses: [][]byte{addrA}})
		require.NoError(t, err)
		assert.Empty(t, resp.Logs)
		var sent []map[string]interface{}
		require.NoError(t, sonic.UnmarshalString(fx.up.lastParams("eth_getLogs"), &sent))
		assert.Equal(t, qtAddrA, sent[0]["address"])
		assert.NotContains(t, sent[0], "topics")
	})
	t.Run("non-array result", func(t *testing.T) {
		fx.up.set("eth_getLogs", `"result":{"logs":[]}`)
		_, err := fx.rpc.GetLogs(ctx, &evm.GetLogsRequest{FromBlock: evm.Uint64Ptr(0x67), ToBlock: evm.Uint64Ptr(0x67)})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("undecodable log", func(t *testing.T) {
		bad := grpcBehaviorLog(0x68, 0)
		bad["address"] = "0xzz"
		fx.up.setResult("eth_getLogs", []interface{}{bad})
		_, err := fx.rpc.GetLogs(ctx, &evm.GetLogsRequest{FromBlock: evm.Uint64Ptr(0x68), ToBlock: evm.Uint64Ptr(0x68)})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("upstream invalid params", func(t *testing.T) {
		fx.up.set("eth_getLogs", `"error":{"code":-32602,"message":"invalid params"}`)
		_, err := fx.rpc.GetLogs(ctx, &evm.GetLogsRequest{FromBlock: evm.Uint64Ptr(0x69), ToBlock: evm.Uint64Ptr(0x69)})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "err=%v", err)
	})
}

func TestGrpcServerBehavior_GetTransactionByHash(t *testing.T) {
	fx := newGrpcBehaviorFixture(t)
	ctx := grpcBehaviorCtx(t, "main")
	txHash := func(i uint64) []byte { return evm.MustHexToBytes(qtTxHash(0x64, i)) }

	t.Run("decoded", func(t *testing.T) {
		tx := qtBlock(0x64)["transactions"].([]interface{})[0]
		fx.up.setResult("eth_getTransactionByHash", tx)
		resp, err := fx.rpc.GetTransactionByHash(ctx, &evm.GetTransactionByHashRequest{TransactionHash: txHash(0)})
		require.NoError(t, err)
		require.NotNil(t, resp.Transaction)
		assert.Equal(t, qtTxHash(0x64, 0), evm.BytesToHex(resp.Transaction.Hash))
		assert.Equal(t, qtAddrA, evm.BytesToHex(resp.Transaction.From))
		assert.Equal(t, uint64(0x5208), resp.Transaction.GasLimit)
		assert.Equal(t, uint64(1), resp.Transaction.Nonce)
		assert.Contains(t, fx.up.lastParams("eth_getTransactionByHash"), qtTxHash(0x64, 0))
	})
	t.Run("null", func(t *testing.T) {
		fx.up.set("eth_getTransactionByHash", `"result":null`)
		resp, err := fx.rpc.GetTransactionByHash(ctx, &evm.GetTransactionByHashRequest{TransactionHash: txHash(1)})
		require.NoError(t, err)
		assert.Nil(t, resp.Transaction)
	})
	t.Run("non-object result", func(t *testing.T) {
		fx.up.set("eth_getTransactionByHash", `"result":[1]`)
		_, err := fx.rpc.GetTransactionByHash(ctx, &evm.GetTransactionByHashRequest{TransactionHash: txHash(2)})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("undecodable", func(t *testing.T) {
		fx.up.set("eth_getTransactionByHash", `"result":{"hash":"0xzz"}`)
		_, err := fx.rpc.GetTransactionByHash(ctx, &evm.GetTransactionByHashRequest{TransactionHash: txHash(3)})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("method unsupported upstream", func(t *testing.T) {
		fx.up.set("eth_getTransactionByHash", `"error":{"code":-32601,"message":"the method eth_getTransactionByHash does not exist/is not available"}`)
		_, err := fx.rpc.GetTransactionByHash(ctx, &evm.GetTransactionByHashRequest{TransactionHash: txHash(4)})
		assert.Equal(t, codes.Unimplemented, status.Code(err), "err=%v", err)
	})
}

func TestGrpcServerBehavior_GetTransactionReceipt(t *testing.T) {
	fx := newGrpcBehaviorFixture(t)
	ctx := grpcBehaviorCtx(t, "main")
	txHash := func(i uint64) []byte { return evm.MustHexToBytes(qtTxHash(0x65, i)) }

	t.Run("decoded", func(t *testing.T) {
		fx.up.setResult("eth_getTransactionReceipt", grpcBehaviorReceipt(0x65, 0))
		resp, err := fx.rpc.GetTransactionReceipt(ctx, &evm.GetTransactionReceiptRequest{TransactionHash: txHash(0)})
		require.NoError(t, err)
		require.NotNil(t, resp.Receipt)
		assert.Equal(t, uint64(0x65), resp.Receipt.BlockNumber)
		assert.Equal(t, uint64(0x5208), resp.Receipt.GasUsed)
		require.NotNil(t, resp.Receipt.Status)
		assert.Equal(t, uint32(1), *resp.Receipt.Status)
		require.Len(t, resp.Receipt.Logs, 1)
		assert.Equal(t, qtToken, evm.BytesToHex(resp.Receipt.Logs[0].Address))
	})
	t.Run("null", func(t *testing.T) {
		fx.up.set("eth_getTransactionReceipt", `"result":null`)
		resp, err := fx.rpc.GetTransactionReceipt(ctx, &evm.GetTransactionReceiptRequest{TransactionHash: txHash(1)})
		require.NoError(t, err, "err=%v", err)
		assert.Nil(t, resp.Receipt)
	})
	t.Run("non-object result", func(t *testing.T) {
		fx.up.set("eth_getTransactionReceipt", `"result":"0x1"`)
		_, err := fx.rpc.GetTransactionReceipt(ctx, &evm.GetTransactionReceiptRequest{TransactionHash: txHash(2)})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("undecodable", func(t *testing.T) {
		fx.up.set("eth_getTransactionReceipt", `"result":{"blockNumber":"zz"}`)
		_, err := fx.rpc.GetTransactionReceipt(ctx, &evm.GetTransactionReceiptRequest{TransactionHash: txHash(3)})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("upstream server error is unavailable", func(t *testing.T) {
		fx.up.set("eth_getTransactionReceipt", `"error":{"code":-32603,"message":"internal error"}`)
		_, err := fx.rpc.GetTransactionReceipt(ctx, &evm.GetTransactionReceiptRequest{TransactionHash: txHash(4)})
		assert.Equal(t, codes.Unavailable, status.Code(err), "err=%v", err)
	})
}

func TestGrpcServerBehavior_GetBlockReceipts(t *testing.T) {
	fx := newGrpcBehaviorFixture(t)
	ctx := grpcBehaviorCtx(t, "main")

	t.Run("by hash", func(t *testing.T) {
		fx.up.setResult("eth_getBlockReceipts", []interface{}{grpcBehaviorReceipt(0x64, 0), grpcBehaviorReceipt(0x64, 1)})
		resp, err := fx.rpc.GetBlockReceipts(ctx, &evm.GetBlockReceiptsRequest{BlockHash: evm.MustHexToBytes(qtHash(0x64))})
		require.NoError(t, err)
		require.Len(t, resp.Receipts, 2)
		assert.Equal(t, uint32(1), resp.Receipts[1].TransactionIndex)
		assert.Equal(t, qtTxHash(0x64, 1), evm.BytesToHex(resp.Receipts[1].TransactionHash))
		assert.Contains(t, fx.up.lastParams("eth_getBlockReceipts"), qtHash(0x64))
	})
	t.Run("by number", func(t *testing.T) {
		fx.up.setResult("eth_getBlockReceipts", []interface{}{grpcBehaviorReceipt(0x65, 0)})
		resp, err := fx.rpc.GetBlockReceipts(ctx, &evm.GetBlockReceiptsRequest{BlockNumber: util.StringPtr("0x65")})
		require.NoError(t, err)
		require.Len(t, resp.Receipts, 1)
		assert.Equal(t, uint64(0x65), resp.Receipts[0].BlockNumber)
		assert.Contains(t, fx.up.lastParams("eth_getBlockReceipts"), `"0x65"`)
	})
	t.Run("neither number nor hash", func(t *testing.T) {
		_, err := fx.rpc.GetBlockReceipts(ctx, &evm.GetBlockReceiptsRequest{})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "blockNumber or blockHash required")
	})
	t.Run("non-array result", func(t *testing.T) {
		fx.up.set("eth_getBlockReceipts", `"result":{"x":1}`)
		_, err := fx.rpc.GetBlockReceipts(ctx, &evm.GetBlockReceiptsRequest{BlockNumber: util.StringPtr("0x66")})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("undecodable receipt", func(t *testing.T) {
		bad := grpcBehaviorReceipt(0x67, 0)
		bad["gasUsed"] = "zz"
		fx.up.setResult("eth_getBlockReceipts", []interface{}{bad})
		_, err := fx.rpc.GetBlockReceipts(ctx, &evm.GetBlockReceiptsRequest{BlockNumber: util.StringPtr("0x67")})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("unknown project", func(t *testing.T) {
		_, err := fx.rpc.GetBlockReceipts(grpcBehaviorCtx(t, "no-such-project"), &evm.GetBlockReceiptsRequest{BlockNumber: util.StringPtr("0x68")})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
}

func TestGrpcServerBehavior_QueryService(t *testing.T) {
	fx := newGrpcBehaviorFixture(t)
	ctx := grpcBehaviorCtx(t, "main")
	client := evm.NewQueryServiceClient(fx.conn)
	from, to := util.StringPtr("0x64"), util.StringPtr("0x64")

	t.Run("blocks served by the shim", func(t *testing.T) {
		stream, err := client.QueryBlocks(ctx, &evm.QueryBlocksRequest{FromBlock: from, ToBlock: to, BlockFields: &evm.BlockFieldSelection{Number: true, Hash: true}})
		pages, code := grpcPages(t, stream, err)
		require.Equal(t, codes.OK, code)
		require.Len(t, pages, 1)
		require.Len(t, pages[0].Blocks, 1)
		assert.Equal(t, uint64(0x64), pages[0].Blocks[0].Number)
		assert.Equal(t, qtHash(0x64), evm.BytesToHex(pages[0].Blocks[0].Hash))
	})

	// The shim allows only eth_queryBlocks and no upstream is a native
	// QueryService, so every other query method is unserved.
	t.Run("transactions", func(t *testing.T) {
		s, err := client.QueryTransactions(ctx, &evm.QueryTransactionsRequest{FromBlock: from, ToBlock: to})
		_, code := grpcPages(t, s, err)
		assert.Equal(t, codes.Unimplemented, code)
	})
	t.Run("logs", func(t *testing.T) {
		s, err := client.QueryLogs(ctx, &evm.QueryLogsRequest{FromBlock: from, ToBlock: to})
		_, code := grpcPages(t, s, err)
		assert.Equal(t, codes.Unimplemented, code)
	})
	t.Run("traces", func(t *testing.T) {
		s, err := client.QueryTraces(ctx, &evm.QueryTracesRequest{FromBlock: from, ToBlock: to})
		_, code := grpcPages(t, s, err)
		assert.Equal(t, codes.Unimplemented, code)
	})
	t.Run("transfers", func(t *testing.T) {
		s, err := client.QueryTransfers(ctx, &evm.QueryTransfersRequest{FromBlock: from, ToBlock: to})
		_, code := grpcPages(t, s, err)
		assert.Equal(t, codes.Unimplemented, code)
	})
}

func TestGrpcServerBehavior_StreamBlocks(t *testing.T) {
	fx := newGrpcBehaviorFixture(t)
	client := evm.NewStreamServiceClient(fx.conn)

	t.Run("unknown project ends the stream", func(t *testing.T) {
		s, err := client.StreamBlocks(grpcBehaviorCtx(t, "no-such-project"), &evm.StreamBlocksRequest{})
		require.NoError(t, err)
		_, err = s.Recv()
		assert.Equal(t, codes.Internal, status.Code(err))
	})

	t.Run("pushes each new head in order", func(t *testing.T) {
		bctx := grpcBehaviorCtx(t, "main")
		head := int64(grpcBehaviorHead)
		// With no live source the tap's demand makes the tracker ask the
		// upstream for latest; the stream starts with that first fresh head
		// (spec §6 rule 6, §9.1).
		sctx, scancel := context.WithCancel(bctx)
		defer scancel()
		s, err := client.StreamBlocks(sctx, &evm.StreamBlocksRequest{})
		require.NoError(t, err)
		recv := func(want int64) {
			t.Helper()
			msg, err := s.Recv()
			require.NoError(t, err)
			assert.Equal(t, uint64(want), msg.Header.Number)
			assert.Equal(t, qtHash(uint64(want)), evm.BytesToHex(msg.Header.Hash))
		}

		recv(head)

		fx.up.head.Store(uint64(head + 2))
		for _, want := range []int64{head + 1, head + 2} {
			recv(want)
		}

		// A poll of the upstream outside rule 6, such as a catch-up poll,
		// votes too; the upstream is still no source, so rule 6 goes on.
		nw, err := fx.erpc.GetNetwork(bctx, "main", "evm:123")
		require.NoError(t, err)
		poller := nw.upstreamsRegistry.GetNetworkUpstreams(bctx, nw.networkId)[0].EvmStatePoller()
		fx.up.head.Store(uint64(head + 3))
		require.NoError(t, poller.Ask(bctx, common.HeadNeedFork, 0, common.AskCatchUp).Err)
		recv(head + 3)
		fx.up.head.Store(uint64(head + 5))
		for _, want := range []int64{head + 4, head + 5} {
			recv(want)
		}

		scancel()
		_, err = s.Recv()
		assert.Equal(t, codes.Canceled, status.Code(err))
	})
}

func TestGrpcServerBehavior_PanicRecovery(t *testing.T) {
	gs := grpcBehaviorBareServer(t, nil, nil)

	t.Run("unary panic is internal", func(t *testing.T) {
		resp, err := gs.panicRecoveryUnary()(context.Background(), "req", &grpc.UnaryServerInfo{FullMethod: "/x/Y"},
			func(ctx context.Context, req interface{}) (interface{}, error) { panic("boom") })
		assert.Nil(t, resp)
		assert.Equal(t, codes.Internal, status.Code(err))
		assert.Equal(t, "internal server error", status.Convert(err).Message())
	})
	t.Run("unary passthrough", func(t *testing.T) {
		resp, err := gs.panicRecoveryUnary()(context.Background(), "req", &grpc.UnaryServerInfo{},
			func(ctx context.Context, req interface{}) (interface{}, error) { return req.(string) + "-ok", nil })
		require.NoError(t, err)
		assert.Equal(t, "req-ok", resp)
	})
	t.Run("stream panic is internal", func(t *testing.T) {
		err := gs.panicRecoveryStream()(nil, nil, &grpc.StreamServerInfo{FullMethod: "/x/Z"},
			func(srv interface{}, ss grpc.ServerStream) error { panic(errors.New("boom")) })
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("stream passthrough keeps the handler error", func(t *testing.T) {
		want := status.Error(codes.Aborted, "stop")
		err := gs.panicRecoveryStream()(nil, nil, &grpc.StreamServerInfo{},
			func(srv interface{}, ss grpc.ServerStream) error { return want })
		assert.Equal(t, want, err)
	})
}

func TestGrpcServerBehavior_GrpcStatusOf(t *testing.T) {
	retryableExhausted := common.NewErrUpstreamsExhaustedWithCause(errors.Join(common.NewErrEndpointServerSideException(errors.New("502"), nil, 502)))
	revertExhausted := common.NewErrUpstreamsExhaustedWithCause(errors.Join(common.NewErrEndpointExecutionException(errors.New("reverted"))))
	noUpstreams := common.NewErrNoUpstreamsLeftToSelect(common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`)), "all cordoned")

	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"unsupported", common.NewErrEndpointUnsupported(nil), codes.Unimplemented},
		{"auth unauthorized", common.NewErrAuthUnauthorized("secret", "invalid_secret", "bad secret"), codes.Unauthenticated},
		{"endpoint unauthorized", common.NewErrEndpointUnauthorized(nil), codes.Unauthenticated},
		{"endpoint timeout", common.NewErrEndpointRequestTimeout(time.Second, nil), codes.DeadlineExceeded},
		{"network timeout", common.NewErrNetworkRequestTimeout(time.Second, nil), codes.DeadlineExceeded},
		{"failsafe timeout", common.NewErrFailsafeTimeoutExceeded(common.ScopeNetwork, nil, nil), codes.DeadlineExceeded},
		{"context deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), codes.DeadlineExceeded},
		{"capacity", common.NewErrEndpointCapacityExceeded(nil), codes.ResourceExhausted},
		{"too large", common.NewErrEndpointRequestTooLarge(nil, common.EvmBlockRangeTooLarge), codes.ResourceExhausted},
		{"billing", common.NewErrEndpointBillingIssue(nil), codes.ResourceExhausted},
		{"auth rate limit", common.NewErrAuthRateLimitRuleExceeded("p", "secret", "b", "r", "u", "1.2.3.4"), codes.ResourceExhausted},
		{"project rate limit", common.NewErrProjectRateLimitRuleExceeded("p", "b", "r"), codes.ResourceExhausted},
		{"network rate limit", common.NewErrNetworkRateLimitRuleExceeded("p", "n", "b", "r"), codes.ResourceExhausted},
		{"upstream rate limit", common.NewErrUpstreamRateLimitRuleExceeded("u", "b", "r"), codes.ResourceExhausted},
		{"missing data", common.NewErrEndpointMissingData(nil, nil), codes.NotFound},
		{"client side", common.NewErrEndpointClientSideException(nil), codes.InvalidArgument},
		{"invalid request", common.NewErrInvalidRequest(nil), codes.InvalidArgument},
		{"getLogs range", common.NewErrGetLogsExceededMaxAllowedRange(100, 10), codes.InvalidArgument},
		{"execution exception", common.NewErrEndpointExecutionException(nil), codes.FailedPrecondition},
		{"exhausted on a revert", revertExhausted, codes.FailedPrecondition},
		{"no upstreams left", noUpstreams, codes.Unavailable},
		{"exhausted, retryable", retryableExhausted, codes.Unavailable},
		{"exhausted, nothing tried", common.NewErrUpstreamsExhaustedWithCause(nil), codes.Internal},
		{"plain error", errors.New("boom"), codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := grpcStatusOf(tc.err)
			st, ok := status.FromError(got)
			require.True(t, ok)
			assert.Equal(t, tc.want, st.Code())
			// Details are a map, so compare only the message head.
			assert.Equal(t, strings.SplitN(tc.err.Error(), " -> ", 2)[0], strings.SplitN(st.Message(), " -> ", 2)[0])
		})
	}
	assert.NoError(t, grpcStatusOf(nil))
}

func TestGrpcServerBehavior_ClientIP(t *testing.T) {
	gs := grpcBehaviorBareServer(t,
		[]string{"10.0.0.0/8", " 192.0.2.1 ", "", "10.0.0.0/33", "not-an-ip"},
		[]string{" X-Real-IP ", "", "x-forwarded-for"})
	peerAt := func(ip string) context.Context {
		return peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 1}})
	}

	cases := []struct {
		name string
		ctx  context.Context
		md   metadata.MD
		want string
	}{
		{"no peer", context.Background(), metadata.Pairs("x-forwarded-for", "203.0.113.9"), ""},
		{"untrusted peer ignores headers", peerAt("198.51.100.25"), metadata.Pairs("x-forwarded-for", "203.0.113.9"), "198.51.100.25"},
		{"trusted IP peer uses xff", peerAt("192.0.2.1"), metadata.Pairs("x-forwarded-for", "203.0.113.9"), "203.0.113.9"},
		{"trusted CIDR peer trims trusted hops", peerAt("10.1.2.3"), metadata.Pairs("x-forwarded-for", "203.0.113.9, 10.9.9.9, 192.0.2.1"), "203.0.113.9"},
		{"headers tried in configured order", peerAt("10.1.2.3"), metadata.Pairs("x-real-ip", "198.51.100.4", "x-forwarded-for", "203.0.113.9"), "198.51.100.4"},
		{"all-trusted chain falls back to next header", peerAt("10.1.2.3"), metadata.Pairs("x-real-ip", "10.0.0.1", "x-forwarded-for", "203.0.113.9"), "203.0.113.9"},
		{"all-trusted chain falls back to peer", peerAt("10.1.2.3"), metadata.Pairs("x-forwarded-for", "10.0.0.7, 192.0.2.1"), "10.1.2.3"},
		{"trusted peer without headers", peerAt("192.0.2.1"), metadata.MD{}, "192.0.2.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, gs.grpcClientIP(tc.ctx, tc.md))
		})
	}

	assert.False(t, gs.isTrustedForwarder(nil))
	assert.False(t, gs.isTrustedForwarder(net.ParseIP("11.0.0.1")), "an invalid /33 CIDR is ignored, not widened")
	assert.Nil(t, grpcPeerIP(context.Background()))
	assert.Nil(t, grpcPeerIP(peer.NewContext(context.Background(), &peer.Peer{})), "peer without address")
}

func TestGrpcServerBehavior_SharesHttpV4(t *testing.T) {
	host, other := "0.0.0.0", "127.0.0.1"
	port, otherPort := 4000, 4001
	cfg := func(mut func(c *common.ServerConfig)) *common.ServerConfig {
		c := &common.ServerConfig{
			GrpcEnabled: util.BoolPtr(true), ListenV4: util.BoolPtr(true),
			HttpHostV4: &host, HttpPortV4: &port, GrpcHostV4: &host, GrpcPortV4: &port,
		}
		if mut != nil {
			mut(c)
		}
		return c
	}
	cases := []struct {
		name string
		cfg  *common.ServerConfig
		want bool
	}{
		{"nil", nil, false},
		{"grpc unset", cfg(func(c *common.ServerConfig) { c.GrpcEnabled = nil }), false},
		{"grpc disabled", cfg(func(c *common.ServerConfig) { c.GrpcEnabled = util.BoolPtr(false) }), false},
		{"v4 not listening", cfg(func(c *common.ServerConfig) { c.ListenV4 = util.BoolPtr(false) }), false},
		{"v4 unset", cfg(func(c *common.ServerConfig) { c.ListenV4 = nil }), false},
		{"grpc port unset", cfg(func(c *common.ServerConfig) { c.GrpcPortV4 = nil }), false},
		{"same host and port", cfg(nil), true},
		{"different port", cfg(func(c *common.ServerConfig) { c.GrpcPortV4 = &otherPort }), false},
		{"different host", cfg(func(c *common.ServerConfig) { c.GrpcHostV4 = &other }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, grpcSharesHttpV4(tc.cfg))
		})
	}
}

func TestGrpcServerBehavior_NewAndStart(t *testing.T) {
	t.Run("reflection on by default", func(t *testing.T) {
		gs := grpcBehaviorBareServer(t, nil, nil)
		info := gs.server.GetServiceInfo()
		assert.Contains(t, info, "grpc.reflection.v1.ServerReflection")
		assert.Contains(t, info, "bds.evm.RPCQueryService")
		assert.Contains(t, info, "bds.evm.QueryService")
		assert.Contains(t, info, "bds.evm.StreamService")
	})

	t.Run("unreadable TLS files fail construction", func(t *testing.T) {
		_, err := NewGrpcServer(context.Background(), &log.Logger, &common.ServerConfig{
			GrpcMaxRecvMsgSize: util.IntPtr(1 << 20),
			GrpcMaxSendMsgSize: util.IntPtr(1 << 20),
			TLS:                &common.TLSConfig{Enabled: true, CertFile: t.TempDir() + "/missing.crt", KeyFile: t.TempDir() + "/missing.key"},
		}, nil)
		assert.Error(t, err)
	})

	t.Run("start fails on an occupied port", func(t *testing.T) {
		busy, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer busy.Close()
		host, port := "127.0.0.1", busy.Addr().(*net.TCPAddr).Port
		gs, err := NewGrpcServer(context.Background(), &log.Logger, &common.ServerConfig{
			GrpcMaxRecvMsgSize: util.IntPtr(1 << 20), GrpcMaxSendMsgSize: util.IntPtr(1 << 20),
			GrpcHostV4: &host, GrpcPortV4: &port,
		}, nil)
		require.NoError(t, err)
		err = gs.Start(&log.Logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), fmt.Sprintf("failed to listen on 127.0.0.1:%d", port))
	})

	t.Run("serves until the app context ends", func(t *testing.T) {
		probe, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		host, port := "127.0.0.1", probe.Addr().(*net.TCPAddr).Port
		require.NoError(t, probe.Close())

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		gs, err := NewGrpcServer(ctx, &log.Logger, &common.ServerConfig{
			GrpcMaxRecvMsgSize: util.IntPtr(1 << 20), GrpcMaxSendMsgSize: util.IntPtr(1 << 20),
			GrpcHostV4: &host, GrpcPortV4: &port,
		}, nil)
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { done <- gs.Start(&log.Logger) }()

		addr := fmt.Sprintf("127.0.0.1:%d", port)
		require.Eventually(t, func() bool {
			c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
			if err != nil {
				return false
			}
			_ = c.Close()
			return true
		}, 5*time.Second, 10*time.Millisecond)

		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err, "graceful stop ends Serve cleanly")
		case <-time.After(5 * time.Second):
			t.Fatal("Start did not return after the app context ended")
		}
	})
}
