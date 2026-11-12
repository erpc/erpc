package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/test/chainsim"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// cacheHeadBds is an in-process BDS reader of a simulated chain whose newest
// block, and that block's age against the wall clock, the test moves. With
// numbers it serves the chain's canonical blocks by number up to that head;
// without, NotFound, as a reader that ingested none of them. A head the chain
// does not hold (another chain's, say) carries no hash.
type cacheHeadBds struct {
	evm.UnimplementedRPCQueryServiceServer
	chain   *chainsim.Chain
	numbers bool
	mu      sync.Mutex
	head    uint64
	age     time.Duration
}

// startCacheHeadBds serves a cacheHeadBds of chain until the test ends.
func startCacheHeadBds(t *testing.T, chain *chainsim.Chain, numbers bool) (*cacheHeadBds, string) {
	t.Helper()
	s := &cacheHeadBds{chain: chain, numbers: numbers}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	evm.RegisterRPCQueryServiceServer(srv, s)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return s, "grpc://" + lis.Addr().String()
}

func (s *cacheHeadBds) set(head uint64, age time.Duration) {
	s.mu.Lock()
	s.head, s.age = head, age
	s.mu.Unlock()
}

func (s *cacheHeadBds) ChainId(context.Context, *evm.ChainIdRequest) (*evm.ChainIdResponse, error) {
	return &evm.ChainIdResponse{ChainId: uint64(s.chain.ID())}, nil
}

func (s *cacheHeadBds) GetBlockByNumber(_ context.Context, req *evm.GetBlockByNumberRequest) (*evm.GetBlockResponse, error) {
	s.mu.Lock()
	head, age := s.head, s.age
	s.mu.Unlock()
	num := head
	switch tag := req.GetBlockNumber(); tag {
	case "latest":
	case "earliest", "finalized", "safe", "pending":
		return &evm.GetBlockResponse{}, nil
	default:
		n, err := strconv.ParseUint(strings.TrimPrefix(tag, "0x"), 16, 64)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, tag)
		}
		if !s.numbers || n > head {
			return nil, status.Error(codes.NotFound, "block not ingested")
		}
		num = n
	}
	h := &evm.BlockHeader{Number: num, Timestamp: uint64(time.Now().Add(-age).Unix())}
	if b := s.chain.CanonAt(num); b != nil {
		parent := b.ParentHash()
		h.Hash, h.ParentHash = b.Hash.Bytes(), parent.Bytes()
	} else if num != head {
		return nil, status.Error(codes.NotFound, "block not ingested")
	}
	return &evm.GetBlockResponse{Block: h}, nil
}

// cacheHeadCache is a cache whose realtime eth_getBlockByNumber policy, with
// ttl, reads from the BDS reader at addr under connector id; with numbers,
// unfinalized and finalized blocks are read from it too.
func cacheHeadCache(id, addr string, ttl time.Duration, numbers bool) *common.CacheConfig {
	policies := []*common.CachePolicyConfig{{
		Network: "*", Method: "eth_getBlockByNumber", Finality: common.DataFinalityStateRealtime,
		Connector: id, TTL: common.FixedDuration(ttl),
	}}
	if numbers {
		for _, f := range []common.DataFinalityState{common.DataFinalityStateUnfinalized, common.DataFinalityStateFinalized} {
			policies = append(policies, &common.CachePolicyConfig{
				Network: "*", Method: "eth_getBlockByNumber", Finality: f,
				Connector: id, TTL: common.FixedDuration(time.Minute),
			})
		}
	}
	return &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{{
			Id: id, Driver: common.DriverGrpc,
			Grpc: &common.GrpcConnectorConfig{Servers: []string{addr}},
		}},
		Policies: policies,
	}
}

func cacheConnectorHead(id string) float64 {
	return testutil.ToFloat64(telemetry.MetricCacheConnectorLatestBlockNumber.WithLabelValues(id, common.NetworkAlias("evm:123")))
}

// cacheHeadNode is one eRPC instance on a halted simulated chain, its two
// vendors push and poll sources, its cache a cacheHeadBds.
type cacheHeadNode struct {
	t     *testing.T
	chain *chainsim.Chain
	bds   *cacheHeadBds
	addr  string
	nw    *Network
}

func newCacheHeadNode(t *testing.T, connector string, ttl time.Duration, follow bool) *cacheHeadNode {
	chain := chainsim.NewChain(chainsim.Config{ChainID: 123, BlockTime: time.Second, Seed: 7, History: 20, Halted: true})
	t.Cleanup(chain.Close)
	ws, err := chain.AddVendor("ws-a", "vendor-a", true)
	require.NoError(t, err)
	http1, err := chain.AddVendor("http-b", "vendor-b", false)
	require.NoError(t, err)
	bds, bdsAddr := startCacheHeadBds(t, chain, true)
	cfg := sourcesConfig(func(nw *common.NetworkConfig) {
		nw.Evm.ServedTip = &common.EvmServedTipConfig{FollowCacheHead: follow}
	}, sourceUpstream(ws.ID, ws.URL(), nil), sourceUpstream(http1.ID, http1.URL(), util.BoolPtr(true)))
	cfg.Server.WaitBeforeShutdown = common.Duration(0).Ptr()
	cfg.Database = &common.DatabaseConfig{EvmJsonRpcCache: cacheHeadCache(connector, bdsAddr, ttl, true)}
	srv, addr, cancel := startTestERPCServer(t, cfg)
	n := &cacheHeadNode{t: t, chain: chain, bds: bds, addr: addr}
	t.Cleanup(func() {
		_ = srv.Shutdown(&zerolog.Logger{})
		cancel()
		if n.nw != nil {
			n.nw.wait()
		}
	})
	require.Eventually(t, func() bool {
		nw, err := srv.erpc.GetNetwork(context.Background(), "test_ws", "evm:123")
		if err != nil || nw == nil || nw.chainTracker == nil {
			return false
		}
		n.nw = nw
		return true
	}, 15*time.Second, 20*time.Millisecond, "network initializes")
	n.awaitView(chain.Tip().Number)
	return n
}

// awaitView waits until the tracker's view stands at block number.
func (n *cacheHeadNode) awaitView(number uint64) {
	require.Eventually(n.t, func() bool {
		h, ok := n.nw.chainTracker.View().Head()
		return ok && h.Number == number
	}, 15*time.Second, 20*time.Millisecond, "the view reaches %d", number)
}

func (n *cacheHeadNode) post(body string) (*http.Response, []byte) {
	resp, err := http.Post(fmt.Sprintf("http://%s/test_ws/evm/123", n.addr), "application/json", bytes.NewReader([]byte(body)))
	require.NoError(n.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(n.t, err)
	return resp, raw
}

func (n *cacheHeadNode) blockNumber() uint64 {
	resp, raw := n.post(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)
	require.Equal(n.t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	var r struct {
		Result string `json:"result"`
	}
	require.NoError(n.t, json.Unmarshal(raw, &r), "body: %s", raw)
	v, err := strconv.ParseUint(strings.TrimPrefix(r.Result, "0x"), 16, 64)
	require.NoError(n.t, err, "body: %s", raw)
	return v
}

// latestBlock reads eth_getBlockByNumber("latest"): its number, its hash and
// the X-ERPC-Cache header.
func (n *cacheHeadNode) latestBlock() (uint64, string, string) {
	resp, raw := n.post(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`)
	require.Equal(n.t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	var r struct {
		Result struct {
			Number string `json:"number"`
			Hash   string `json:"hash"`
		} `json:"result"`
	}
	require.NoError(n.t, json.Unmarshal(raw, &r), "body: %s", raw)
	v, err := strconv.ParseUint(strings.TrimPrefix(r.Result.Number, "0x"), 16, 64)
	require.NoError(n.t, err, "body: %s", raw)
	return v, r.Result.Hash, resp.Header.Get("X-ERPC-Cache")
}

// The Latest a network-wide read takes follows a fresh cache head down, and
// the block it names is read from the cache; a stale head falls back to the
// view; a fresh head far below the Latest served holds it there for one TTL,
// then lets go; a head ahead never raises it (evm.servedTip.followCacheHead).
// Full HTTP server, the real chain tracker on a simulated chain's vendors,
// the real grpc cache connector against an in-process BDS reader.
func TestNetworkCacheHead_AnchorsReadHeadToFreshCacheHead(t *testing.T) {
	const ttl = 3 * time.Second
	n := newCacheHeadNode(t, "bds-anchor", ttl, true)
	require.NotNil(t, n.nw.cacheHead, "a realtime grpc policy with a TTL makes the anchor")
	tip := n.chain.Tip().Number

	// 1. Cache two blocks behind the view, stamped now, before any read
	// served the view's head: reads take the cache's head, and the block it
	// names comes from the cache with its pinned hash.
	n.bds.set(tip-2, 0)
	// The cache head is polled only while the lane has a reader.
	n.nw.chainTracker.MarkRead()
	require.Eventually(t, func() bool { return cacheConnectorHead("bds-anchor") == float64(tip-2) }, 10*time.Second, 20*time.Millisecond,
		"the anchor's poll observes the fresh head")
	assert.Equal(t, tip-2, n.blockNumber(), "eth_blockNumber serves the fresh cache head")
	number, hash, cache := n.latestBlock()
	assert.Equal(t, tip-2, number)
	assert.Equal(t, n.chain.CanonAt(tip-2).Hash.Hex(), hash, "the anchored block is the canonical one")
	assert.Equal(t, "HIT", cache, "the anchored block is read from the cache")
	view, _ := n.nw.chainTracker.View().Head()
	assert.Equal(t, tip, view.Number, "the view stays at the tracker's head")

	// 2. The cache head is 10s old, past the 3s TTL: reads take the view's
	// head, read from the vendors.
	n.bds.set(tip-2, 10*time.Second)
	require.Eventually(t, func() bool { return n.blockNumber() == tip }, 10*time.Second, 50*time.Millisecond,
		"a stale cache head does not anchor")
	number, hash, cache = n.latestBlock()
	assert.Equal(t, tip, number)
	assert.Equal(t, n.chain.CanonAt(tip).Hash.Hex(), hash)
	assert.NotEqual(t, "HIT", cache, "the cache lacks the view's head")

	// 3. A fresh head far below the Latest served (another chain's, say):
	// Latest neither drops nor follows the chain for one TTL, then lets go.
	n.bds.set(tip-15, 0)
	require.Eventually(t, func() bool { return cacheConnectorHead("bds-anchor") == float64(tip-15) }, 10*time.Second, 20*time.Millisecond,
		"the anchor's poll observes the fresh head")
	held := time.Now()
	assert.Equal(t, tip, n.blockNumber(), "a fresh head below the served Latest does not drag it down")
	n.chain.Append(chainsim.KindEmpty, func(map[string]json.RawMessage) {})
	n.chain.Append(chainsim.KindEmpty, func(map[string]json.RawMessage) {})
	n.awaitView(tip + 2)
	if got := n.blockNumber(); time.Since(held) < ttl {
		assert.Equal(t, tip, got, "Latest holds at the value served while the hold lasts")
	}
	require.Eventually(t, func() bool { return n.blockNumber() == tip+2 }, ttl+5*time.Second, 20*time.Millisecond,
		"the hold lets go once the cache does not catch up within its TTL")
	assert.GreaterOrEqual(t, time.Since(held), ttl, "the hold lasts one TTL")

	// 4. A fresh cache head ahead of the view never raises Latest.
	n.bds.set(tip+7, 0)
	require.Eventually(t, func() bool { return cacheConnectorHead("bds-anchor") == float64(tip+7) }, 10*time.Second, 20*time.Millisecond,
		"the anchor's poll observes the head ahead")
	assert.Equal(t, tip+2, n.blockNumber())
	number, hash, _ = n.latestBlock()
	assert.Equal(t, tip+2, number)
	assert.Equal(t, n.chain.CanonAt(tip+2).Hash.Hex(), hash)
}

// Without evm.servedTip.followCacheHead the same cache anchors nothing: its
// head is polled, for the block clock's sightings, but reads take the
// view's head.
func TestNetworkCacheHead_OffServesTheView(t *testing.T) {
	n := newCacheHeadNode(t, "bds-off", 3*time.Second, false)
	require.NotNil(t, n.nw.cacheHead, "a realtime grpc policy with a TTL makes the cache head")
	tip := n.chain.Tip().Number
	n.bds.set(tip-2, 0)
	n.nw.chainTracker.MarkRead()
	require.Eventually(t, func() bool { return cacheConnectorHead("bds-off") == float64(tip-2) }, 10*time.Second, 20*time.Millisecond,
		"the cache head's poll observes the fresh head")
	for range 10 {
		require.Equal(t, tip, n.blockNumber())
		time.Sleep(100 * time.Millisecond)
	}
	number, hash, _ := n.latestBlock()
	assert.Equal(t, tip, number)
	assert.Equal(t, n.chain.CanonAt(tip).Hash.Hex(), hash)
}
