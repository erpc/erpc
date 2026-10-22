package data

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// bdsTestServer answers ChainId and GetBlockByNumber the way a BDS reader
// does: each tag maps to a fixed block (number, timestamp).
type bdsTestServer struct {
	evm.UnimplementedRPCQueryServiceServer
	chainID  uint64
	chainErr error
	blocks   map[string][2]uint64
	calls    atomic.Int64
}

func (s *bdsTestServer) ChainId(context.Context, *evm.ChainIdRequest) (*evm.ChainIdResponse, error) {
	s.calls.Add(1)
	if s.chainErr != nil {
		return nil, s.chainErr
	}
	return &evm.ChainIdResponse{ChainId: s.chainID}, nil
}

func (s *bdsTestServer) GetBlockByNumber(_ context.Context, req *evm.GetBlockByNumberRequest) (*evm.GetBlockResponse, error) {
	s.calls.Add(1)
	b, ok := s.blocks[req.GetBlockNumber()]
	if !ok {
		return &evm.GetBlockResponse{}, nil
	}
	return &evm.GetBlockResponse{Block: &evm.BlockHeader{Number: b[0], Timestamp: b[1], Hash: []byte{0xaa}}}, nil
}

func startBdsTestServer(t *testing.T, s *bdsTestServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	evm.RegisterRPCQueryServiceServer(srv, s)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return "grpc://" + lis.Addr().String()
}

func newTestGrpcConnector(t *testing.T, cfg *common.GrpcConnectorConfig) *GrpcConnector {
	t.Helper()
	lg := zerolog.Nop()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gc, err := NewGrpcConnector(ctx, &lg, "grpc-test", cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		gc.mu.RLock()
		defer gc.mu.RUnlock()
		for _, cli := range gc.clientByNetwork {
			if sd, ok := cli.(clients.ShutdownableClient); ok {
				sd.Shutdown()
			}
		}
	})
	return gc
}

func TestGrpcConnector_ProbesChainIdAndPollsHeads(t *testing.T) {
	srv := &bdsTestServer{chainID: 1, blocks: map[string][2]uint64{
		"earliest":  {100, 1_000},
		"latest":    {500, 5_000},
		"finalized": {400, 0},
	}}
	addr := startBdsTestServer(t, srv)
	gc := newTestGrpcConnector(t, &common.GrpcConnectorConfig{Servers: []string{addr}, Headers: map[string]string{"x-key": "v"}})

	require.Equal(t, util.StateReady, gc.initializer.State())
	require.NoError(t, gc.checkReady())
	gc.mu.RLock()
	_, ok := gc.clientByNetwork["evm:1"]
	gc.mu.RUnlock()
	require.True(t, ok, "probed chainId 1 registers the server as evm:1")

	_, known := gc.CacheLatestBlockTimestamp("evm:1")
	assert.False(t, known, "head unknown before the first poll")

	gc.pollBlockHeadsOnce(context.Background())
	ts, known := gc.CacheLatestBlockTimestamp("evm:1")
	assert.True(t, known)
	assert.Equal(t, int64(5_000), ts)
	gc.mu.RLock()
	assert.Equal(t, uint64(100), gc.earliestByNetwork["evm:1"])
	assert.Equal(t, uint64(500), gc.latestByNetwork["evm:1"])
	assert.Equal(t, uint64(400), gc.finalizedByNetwork["evm:1"])
	gc.mu.RUnlock()

	ctx := context.Background()
	t.Run("block before earliest is a fast miss without a round trip", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x5",false]}`))
		req.SetNetwork(fakeBlockNumberNetwork{})
		req.SetEvmBlockNumber(uint64(5))
		before := srv.calls.Load()
		_, err := gc.Get(ctx, ConnectorMainIndex, "pk", "rk", req)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "got %v", err)
		assert.Equal(t, before, srv.calls.Load())
	})

	t.Run("block at or after earliest is read through", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
		req.SetNetwork(fakeBlockNumberNetwork{})
		req.SetEvmBlockNumber(uint64(500))
		got, err := gc.Get(ctx, ConnectorMainIndex, "pk", "rk", req)
		require.NoError(t, err)
		assert.Contains(t, string(got), `"number":"0x1f4"`)
	})

	t.Run("eth_blockNumber reflects the server's latest", func(t *testing.T) {
		got, err := gc.Get(ctx, ConnectorMainIndex, "pk", "rk", blockNumberRequest(t))
		require.NoError(t, err)
		assert.Equal(t, `"0x1f4"`, string(got))
	})

	t.Run("a second server for the same chain is ignored", func(t *testing.T) {
		dup := newTestGrpcConnector(t, &common.GrpcConnectorConfig{Servers: []string{addr, addr}})
		require.Equal(t, util.StateReady, dup.initializer.State())
		dup.mu.RLock()
		assert.Len(t, dup.clientByNetwork, 1)
		dup.mu.RUnlock()
	})
}

func TestGrpcConnector_PollKeepsPriorHeadsWhenTagsAreMissing(t *testing.T) {
	srv := &bdsTestServer{chainID: 1, blocks: map[string][2]uint64{"latest": {7, 70}}}
	gc := newTestGrpcConnector(t, &common.GrpcConnectorConfig{Servers: []string{startBdsTestServer(t, srv)}})
	gc.mu.Lock()
	gc.earliestByNetwork["evm:1"] = 3
	gc.finalizedByNetwork["evm:1"] = 6
	gc.mu.Unlock()

	gc.pollBlockHeadsOnce(context.Background())

	gc.mu.RLock()
	defer gc.mu.RUnlock()
	assert.Equal(t, uint64(3), gc.earliestByNetwork["evm:1"], "a null earliest block leaves the prior value")
	assert.Equal(t, uint64(6), gc.finalizedByNetwork["evm:1"], "a null finalized block leaves the prior value")
	assert.Equal(t, uint64(7), gc.latestByNetwork["evm:1"])
}

func TestGrpcConnector_ConfiguredNetworkIdSkipsProbe(t *testing.T) {
	srv := &bdsTestServer{chainErr: status.Error(codes.Unimplemented, "no chain id")}
	gc := newTestGrpcConnector(t, &common.GrpcConnectorConfig{Servers: []string{startBdsTestServer(t, srv)}, NetworkId: "svm:solana-mainnet"})
	require.Equal(t, util.StateReady, gc.initializer.State())
	assert.Zero(t, srv.calls.Load(), "no eth_chainId probe when the network is configured")
	gc.mu.RLock()
	_, ok := gc.clientByNetwork["svm:solana-mainnet"]
	gc.mu.RUnlock()
	assert.True(t, ok)
}

func TestGrpcConnector_InitFailures(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		lg := zerolog.Nop()
		_, err := NewGrpcConnector(context.Background(), &lg, "x", nil)
		require.ErrorContains(t, err, "requires config")
	})

	t.Run("failed chainId probe leaves the connector not ready", func(t *testing.T) {
		srv := &bdsTestServer{chainErr: status.Error(codes.Unavailable, "down")}
		gc := newTestGrpcConnector(t, &common.GrpcConnectorConfig{Servers: []string{startBdsTestServer(t, srv)}})
		assert.NotEqual(t, util.StateReady, gc.initializer.State())
		assert.ErrorContains(t, gc.checkReady(), "no gRPC clients initialized")

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
		req.SetNetwork(fakeBlockNumberNetwork{})
		_, err := gc.Get(context.Background(), ConnectorMainIndex, "pk", "rk", req)
		assert.ErrorContains(t, err, "no gRPC clients initialized")
	})

	t.Run("unparsable server url is fatal", func(t *testing.T) {
		gc := newTestGrpcConnector(t, &common.GrpcConnectorConfig{Servers: []string{"grpc://%zz"}})
		assert.Equal(t, util.StateFatal, gc.initializer.State())
	})

	t.Run("no servers yields an empty connector", func(t *testing.T) {
		gc := newTestGrpcConnector(t, &common.GrpcConnectorConfig{})
		assert.ErrorContains(t, gc.checkReady(), "no gRPC clients initialized")
		assert.ErrorContains(t, (&GrpcConnector{}).checkReady(), "initializer not set")
	})

	t.Run("unreachable bootstrap endpoint fails construction", func(t *testing.T) {
		lg := zerolog.Nop()
		_, err := NewGrpcConnector(context.Background(), &lg, "x", &common.GrpcConnectorConfig{Bootstrap: "http://127.0.0.1:1/servers"})
		require.ErrorContains(t, err, "bootstrap failed")
		_, err = NewGrpcConnector(context.Background(), &lg, "x", &common.GrpcConnectorConfig{Bootstrap: "://bad"})
		require.ErrorContains(t, err, "bootstrap failed")
	})
}

func TestGrpcConnector_BootstrapDiscoversServers(t *testing.T) {
	addr := startBdsTestServer(t, &bdsTestServer{chainID: 10})
	cases := map[string]string{
		"json array":        fmt.Sprintf(`[%q]`, addr),
		"newline separated": fmt.Sprintf("\n  %s  \n\n", addr),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(hs.Close)
			gc := newTestGrpcConnector(t, &common.GrpcConnectorConfig{Bootstrap: hs.URL})
			require.Equal(t, util.StateReady, gc.initializer.State())
			gc.mu.RLock()
			_, ok := gc.clientByNetwork["evm:10"]
			gc.mu.RUnlock()
			assert.True(t, ok)
		})
	}
}

// scriptedClient returns a fixed response/error and records the deadline it
// was called with.
type scriptedClient struct {
	clients.GrpcBdsClient
	resp     *common.NormalizedResponse
	err      error
	deadline atomic.Int64
}

func (s *scriptedClient) SendRequest(ctx context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	if dl, ok := ctx.Deadline(); ok {
		s.deadline.Store(int64(time.Until(dl)))
	}
	return s.resp, s.err
}

func TestGrpcConnector_GetOutcomes(t *testing.T) {
	getReq := func() *common.NormalizedRequest {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
		req.SetNetwork(fakeBlockNumberNetwork{})
		return req
	}
	sendErr := errors.New("transport down")

	t.Run("transport error is returned as is", func(t *testing.T) {
		gc := newBlockNumberConnector(t, &scriptedClient{err: sendErr})
		_, err := gc.Get(context.Background(), ConnectorMainIndex, "pk", "rk", getReq())
		assert.ErrorIs(t, err, sendErr)
	})

	t.Run("nil response without error is an empty miss", func(t *testing.T) {
		gc := newBlockNumberConnector(t, &scriptedClient{})
		got, err := gc.Get(context.Background(), ConnectorMainIndex, "pk", "rk", getReq())
		assert.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("missing request metadata or method is a miss", func(t *testing.T) {
		gc := newBlockNumberConnector(t, &scriptedClient{})
		_, err := gc.Get(context.Background(), ConnectorMainIndex, "pk", "rk", nil)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
		_, err = gc.Get(context.Background(), ConnectorMainIndex, "pk", "rk", common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"params":[]}`)))
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
	})

	t.Run("getTimeout tightens a longer caller deadline", func(t *testing.T) {
		cli := &scriptedClient{resp: blockResponse(t, `{"number":"0x1"}`)}
		gc := newBlockNumberConnector(t, cli)
		gc.getTimeout = 200 * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		got, err := gc.Get(ctx, ConnectorMainIndex, "pk", "rk", getReq())
		require.NoError(t, err)
		assert.Equal(t, `{"number":"0x1"}`, string(got))
		assert.LessOrEqual(t, time.Duration(cli.deadline.Load()), 200*time.Millisecond)
	})
}

func TestGrpcConnector_WriteSideIsUnsupported(t *testing.T) {
	g := &GrpcConnector{id: "g"}
	ctx := context.Background()
	assert.True(t, g.ReadOnly())
	assert.Equal(t, "g", g.Id())
	assert.ErrorContains(t, g.Set(ctx, "pk", "rk", nil, nil), "read-only")
	assert.ErrorContains(t, g.Delete(ctx, "pk", "rk"), "read-only")
	_, err := g.Lock(ctx, "k", time.Second)
	assert.ErrorContains(t, err, "read-only")
	_, _, err = g.List(ctx, ConnectorMainIndex, 1, "")
	assert.ErrorContains(t, err, "List")
	_, cleanup, err := g.WatchCounterInt64(ctx, "k")
	assert.ErrorContains(t, err, "WatchCounterInt64")
	cleanup() // the returned cleanup is safe to call even on error
	assert.ErrorContains(t, g.PublishCounterInt64(ctx, "k", CounterInt64State{}), "PublishCounterInt64")
}
