package erpc

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// cacheHeadBdsServer is an in-process BDS reader for chain 123 whose newest
// block (number, and age against the wall clock) the test moves mid-run.
// Blocks above it are NotFound, as a reader that has not ingested them yet.
type cacheHeadBdsServer struct {
	evm.UnimplementedRPCQueryServiceServer
	mu     sync.Mutex
	latest uint64
	age    time.Duration
}

func (s *cacheHeadBdsServer) set(latest uint64, age time.Duration) {
	s.mu.Lock()
	s.latest, s.age = latest, age
	s.mu.Unlock()
}

func (s *cacheHeadBdsServer) ChainId(context.Context, *evm.ChainIdRequest) (*evm.ChainIdResponse, error) {
	return &evm.ChainIdResponse{ChainId: 123}, nil
}

func (s *cacheHeadBdsServer) GetBlockByNumber(_ context.Context, req *evm.GetBlockByNumberRequest) (*evm.GetBlockResponse, error) {
	s.mu.Lock()
	latest, age := s.latest, s.age
	s.mu.Unlock()
	num := latest
	switch tag := req.GetBlockNumber(); tag {
	case "latest":
	case "earliest", "finalized":
		return &evm.GetBlockResponse{}, nil
	default:
		n, err := strconv.ParseUint(strings.TrimPrefix(tag, "0x"), 16, 64)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, tag)
		}
		if n > latest {
			return nil, status.Error(codes.NotFound, "block not ingested")
		}
		num = n
	}
	return &evm.GetBlockResponse{Block: &evm.BlockHeader{
		Number:    num,
		Timestamp: uint64(time.Now().Add(-age).Unix()),
		Hash:      bytes.Repeat([]byte{0xcc}, 32),
	}}, nil
}

// The served latest follows a fresh cache head down, never up, and falls back
// to the upstreams' head once the cache head is older than cacheHeadMaxAge:
// full HTTP server, real pollers, the real grpc cache connector against an
// in-process BDS reader; only the upstreams' HTTP answers are mocked.
func TestNetworkCacheHead_AnchorsServedLatestToFreshCacheHead(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	const (
		upstreamHead = int64(0x1000)
		cacheHead    = upstreamHead - 2
		cacheAhead   = upstreamHead + 5
	)
	upstreamHash := "0x" + strings.Repeat("aa", 32)
	cacheHash := "0x" + strings.Repeat("cc", 32)

	// Numbered eth_getBlockByNumber reads reaching an upstream; the pollers
	// only ever ask for tags.
	var upstreamByNumber atomic.Int64
	for _, host := range []string{"http://rpc1.localhost", "http://rpc2.localhost"} {
		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_chainId")
			}).
			Reply(200).JSON([]byte(`{"result":"0x7b"}`))
		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_syncing")
			}).
			Reply(200).JSON([]byte(`{"result":false}`))
		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				b := util.SafeReadBody(r)
				return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "latest")
			}).
			Reply(200).JSON([]byte(`{"result":{"number":"0x1000","timestamp":"0x6702a8f0"}}`))
		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				b := util.SafeReadBody(r)
				return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "finalized")
			}).
			Reply(200).JSON([]byte(`{"result":{"number":"0x900","timestamp":"0x6702a8e0"}}`))
		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				b := util.SafeReadBody(r)
				if !strings.Contains(b, "eth_getBlockByNumber") || strings.Contains(b, "latest") || strings.Contains(b, "finalized") {
					return false
				}
				upstreamByNumber.Add(1)
				return true
			}).
			Reply(200).JSON([]byte(fmt.Sprintf(`{"result":{"number":"0x1000","hash":"%s","timestamp":"0x6702a8f0"}}`, upstreamHash)))
	}

	bds := &cacheHeadBdsServer{}
	bds.set(uint64(cacheHead), 0)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcSrv := grpc.NewServer()
	evm.RegisterRPCQueryServiceServer(grpcSrv, bds)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	upstreams := make([]*common.UpstreamConfig, 0, 2)
	for _, id := range []string{"rpc1", "rpc2"} {
		upstreams = append(upstreams, &common.UpstreamConfig{
			Id:       id,
			Endpoint: "http://" + id + ".localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(10 * time.Second),
			},
		})
	}
	cfg := &common.Config{
		Server: &common.ServerConfig{MaxTimeout: common.Duration(10 * time.Second).Ptr()},
		Database: &common.DatabaseConfig{
			EvmJsonRpcCache: &common.CacheConfig{
				Connectors: []*common.ConnectorConfig{{
					Id:     "bds",
					Driver: common.DriverGrpc,
					Grpc:   &common.GrpcConnectorConfig{Servers: []string{"grpc://" + lis.Addr().String()}},
				}},
				Policies: []*common.CachePolicyConfig{
					{Network: "evm:123", Method: "*", Finality: common.DataFinalityStateRealtime, Connector: "bds", TTL: common.FixedDuration(10 * time.Second)},
					{Network: "evm:123", Method: "*", Finality: common.DataFinalityStateUnfinalized, Connector: "bds", TTL: common.FixedDuration(10 * time.Second)},
				},
			},
		},
		Projects: []*common.ProjectConfig{{
			Id: "test_project",
			Networks: []*common.NetworkConfig{{
				Architecture: "evm",
				Evm: &common.EvmNetworkConfig{
					ChainId:   123,
					ServedTip: &common.EvmServedTipConfig{CacheHeadMaxAge: common.Duration(3 * time.Second)},
				},
			}},
			Upstreams: upstreams,
		}},
	}

	send, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()
	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	policy.OverrideAllForTest(prj.policyEngine, "rpc1", "rpc2")
	nw, err := prj.GetNetwork(context.Background(), util.EvmNetworkId(123))
	require.NoError(t, err)

	anchorTotal := func(outcome string) float64 {
		return testutil.ToFloat64(telemetry.MetricNetworkCacheHeadAnchorTotal.WithLabelValues("test_project", nw.Label(), outcome))
	}
	blockNumber := func() int64 {
		code, _, body := send(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`, nil, nil)
		if code != http.StatusOK {
			return -1
		}
		var r struct {
			Result string `json:"result"`
		}
		if sonic.UnmarshalString(body, &r) != nil {
			return -1
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(r.Result, "0x"), 16, 64)
		if err != nil {
			return -1
		}
		return n
	}
	latestBlock := func() (number, hash, cacheHeader string) {
		code, headers, body := send(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`, nil, nil)
		require.Equal(t, http.StatusOK, code, "body: %s", body)
		var r struct {
			Result map[string]interface{} `json:"result"`
		}
		require.NoError(t, sonic.UnmarshalString(body, &r), "body: %s", body)
		require.NotNil(t, r.Result, "body: %s", body)
		number, _ = r.Result["number"].(string)
		hash, _ = r.Result["hash"].(string)
		return number, hash, headers["X-Erpc-Cache"]
	}

	// 1. Cache two blocks behind the upstreams, stamped now: latest is the
	// cache's head, and the block it names comes from the cache.
	anchoredBefore := anchorTotal("anchored")
	require.Eventually(t, func() bool { return blockNumber() == cacheHead }, 5*time.Second, 50*time.Millisecond,
		"eth_blockNumber must serve the fresh cache head")
	byNumberBefore := upstreamByNumber.Load()
	number, hash, cacheHeader := latestBlock()
	assert.Equal(t, fmt.Sprintf("0x%x", cacheHead), number)
	assert.Equal(t, cacheHash, hash, "the anchored latest block must be read from the cache")
	assert.Equal(t, "HIT", cacheHeader)
	assert.Equal(t, byNumberBefore, upstreamByNumber.Load(), "no upstream eth_getBlockByNumber for the anchored block")
	assert.Greater(t, anchorTotal("anchored"), anchoredBefore)

	// 2. The cache head stops being fresh (its block is now 10s old, past the
	// 3s bound): latest returns to the upstreams' head, read from them.
	staleBefore := anchorTotal("stale")
	bds.set(uint64(cacheHead), 10*time.Second)
	require.Eventually(t, func() bool { return blockNumber() == upstreamHead }, 10*time.Second, 50*time.Millisecond,
		"a stale cache head must not anchor latest")
	assert.Greater(t, anchorTotal("stale"), staleBefore)
	byNumberBefore = upstreamByNumber.Load()
	number, hash, _ = latestBlock()
	assert.Equal(t, fmt.Sprintf("0x%x", upstreamHead), number)
	assert.Equal(t, upstreamHash, hash, "a block the cache lacks comes from an upstream")
	assert.Greater(t, upstreamByNumber.Load(), byNumberBefore)

	// 3. The cache head ahead of the upstreams, fresh: the anchor never
	// raises latest.
	levelBefore := anchorTotal("level")
	bds.set(uint64(cacheAhead), 0)
	require.Eventually(t, func() bool { return anchorTotal("level") > levelBefore }, 5*time.Second, 50*time.Millisecond,
		"the anchor must see the fresh cache head ahead of latest")
	assert.Equal(t, upstreamHead, blockNumber())
}
