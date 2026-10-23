package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func init() {
	util.ConfigureTestLogger()
}

// setupWsTipNetwork bootstraps a single-upstream EVM network backed by an
// in-memory shared state registry.
func setupWsTipNetwork(t *testing.T, ctx context.Context, id, endpoint string) (*Network, *upstream.Upstream) {
	t.Helper()
	util.SetupMocksForEvmStatePoller()
	gock.New(endpoint).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), `eth_chainId`)
		}).
		Reply(200).
		JSON([]byte(`{"result":"0x7b"}`))

	rateLimitersRegistry, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)

	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "test",
		[]*common.UpstreamConfig{{
			Type:     common.UpstreamTypeEvm,
			Id:       id,
			Endpoint: endpoint,
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		}}, ssr, rateLimitersRegistry, vr, pr, nil,
		metricsTracker, nil,
	)
	network, err := NewNetwork(ctx, &log.Logger, "test", &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}, rateLimitersRegistry, upstreamsRegistry, metricsTracker, nil)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.GetInitializer().WaitForTasks(ctx))
	require.NoError(t, network.Bootstrap(ctx))
	time.Sleep(250 * time.Millisecond)

	upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.Len(t, upsList, 1)
	return network, upsList[0]
}

func tipHash(tag string, n uint64) string {
	return fmt.Sprintf("0x%s%0*x", tag, 64-len(tag), n)
}

// tipHead is block n of the linear chain tagged tag.
func tipHead(tag string, n uint64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"number":"0x%x","hash":"%s","parentHash":"%s","timestamp":"0x%x"}`,
		n, tipHash(tag, n), tipHash(tag, n-1), 1_700_000_000+n))
}

func eventuallyLatest(t *testing.T, ctx context.Context, network *Network, want int64, msg string) {
	t.Helper()
	require.Eventually(t, func() bool { return network.EvmHighestLatestBlockNumber(ctx) == want },
		2*time.Second, 10*time.Millisecond, msg)
}

// pinned is the network-wide tracker's pinned head, which only counted
// votes move; 0 while none.
func pinned(network *Network) int64 {
	if c, ok := network.chainTracker.Tip(); ok {
		return int64(c.Number)
	}
	return 0
}

func eventuallyPinned(t *testing.T, network *Network, want int64, msg string) {
	t.Helper()
	require.Eventually(t, func() bool { return pinned(network) == want }, 2*time.Second, 10*time.Millisecond, msg)
}

// Each source header moves its upstream's poller at once; the network tip,
// and every transport, move only to a block q sources have. Three wss
// sources at 1005, 1001 and 1000 with q = 2: HTTP, ws newHeads and gRPC
// StreamBlocks stop at 1001, and reach 1005 once a second source has it.
func TestNetworkIntake_HeaderAdvancesNetworkTip(t *testing.T) {
	setupGock()
	defer util.ResetGock()
	ctx := context.Background()

	ids := []string{"rpc-a", "rpc-b", "rpc-c"}
	chains := make(map[string]*mockChain, len(ids))
	vendors := make(map[string]*mockWsVendor, len(ids))
	upCfgs := make([]*common.UpstreamConfig, 0, len(ids))
	for _, id := range ids {
		// Latest polls answer block 999, so only pushes move the heads.
		chains[id] = newMockChain(999, false)
		v, url := newMockWsVendor(t, chains[id], func(v *mockWsVendor) { v.staleRPC = true })
		vendors[id] = v
		upCfgs = append(upCfgs, sourceUpstream(id, url, nil))
	}
	cfg := sourcesConfig(func(nw *common.NetworkConfig) {
		nw.SelectionPolicy = &common.SelectionPolicyConfig{EvalFunc: `(upstreams, ctx) => upstreams.filter(u => true)`}
		nw.Evm.ServedTip = &common.EvmServedTipConfig{Quorum: 2}
	}, upCfgs...)
	erpcSrv, addr, cancel := startTestERPCServer(t, cfg)
	defer func() { _ = erpcSrv.Shutdown(&log.Logger); cancel() }()
	prj, err := erpcSrv.erpc.GetProject("test_ws")
	require.NoError(t, err)
	network, err := prj.GetNetwork(ctx, "evm:123")
	require.NoError(t, err)
	byId := make(map[string]*upstream.Upstream, len(ids))
	for _, u := range network.upstreamsRegistry.GetNetworkUpstreams(ctx, network.networkId) {
		byId[u.Id()] = u
	}
	require.Len(t, byId, len(ids))

	call := func(method, params string) interface{} {
		resp, err := http.Post(fmt.Sprintf("http://%s/test_ws/evm/123", addr), "application/json",
			strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)))
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		var out map[string]interface{}
		if json.NewDecoder(resp.Body).Decode(&out) != nil {
			return nil
		}
		return out["result"]
	}
	blockNumber := func() string {
		s, _ := call("eth_blockNumber", "[]").(string)
		return s
	}
	latestBlock := func() string {
		b, _ := call("eth_getBlockByNumber", `["latest",false]`).(map[string]interface{})
		s, _ := b["number"].(string)
		return s
	}

	tapLabels := func(transport string) []string {
		return []string{"test_ws", "evm:123", transport, indexer.SubTypeNewHeads}
	}
	openTaps := func(transport string) float64 {
		return testutil.ToFloat64(telemetry.MetricTaps.WithLabelValues(tapLabels(transport)...))
	}
	wsTaps, grpcTaps := openTaps("ws"), openTaps("grpc")

	conn := dialWs(t, addr)
	defer conn.Close()
	subscribeNewHeads(t, conn)
	wsHeads := make(chan int64, 64)
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var notif struct {
				Params struct {
					Result struct {
						Number string `json:"number"`
					} `json:"result"`
				} `json:"params"`
			}
			if json.Unmarshal(msg, &notif) != nil {
				continue
			}
			if n, err := common.HexToInt64(notif.Params.Result.Number); err == nil {
				wsHeads <- n
			}
		}
	}()

	grpcAddr, stopGrpc := serveGrpc(t, ctx, erpcSrv.erpc)
	defer stopGrpc()
	gconn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer gconn.Close()
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	stream, err := bdsevm.NewStreamServiceClient(gconn).StreamBlocks(streamMD(streamCtx), &bdsevm.StreamBlocksRequest{})
	require.NoError(t, err)
	grpcHeads, grpcEnded := make(chan int64, 64), make(chan error, 1)
	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				grpcEnded <- err
				return
			}
			grpcHeads <- int64(resp.GetHeader().GetNumber())
		}
	}()

	require.Eventually(t, func() bool {
		for _, v := range vendors {
			if v.subscribes.Load() == 0 {
				return false
			}
		}
		return openTaps("ws") == wsTaps+1 && openTaps("grpc") == grpcTaps+1
	}, 10*time.Second, 10*time.Millisecond, "every source subscribes newHeads and both client taps open")

	// reach reads heads until want, none above it, then requires silence;
	// ended fails it once the client's stream ended.
	reach := func(heads <-chan int64, ended <-chan error, want int64, transport string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case n := <-heads:
				require.LessOrEqual(t, n, want, "%s sent block %d above latest %d", transport, n, want)
				if n < want {
					continue
				}
				select {
				case n := <-heads:
					t.Fatalf("%s sent block %d after latest %d", transport, n, want)
				case <-time.After(300 * time.Millisecond):
				}
				return
			case err := <-ended:
				t.Fatalf("%s ended before block %d: %v", transport, want, err)
			case <-deadline:
				t.Fatalf("%s never sent block %d", transport, want)
			}
		}
	}
	heads := func(want map[string]int64) {
		t.Helper()
		require.Eventually(t, func() bool {
			for id, n := range want {
				if byId[id].EvmStatePoller().LatestBlock() != n {
					return false
				}
			}
			return true
		}, 10*time.Second, 10*time.Millisecond, "each source header moves its own poller: %v", want)
	}

	chains["rpc-c"].advance()
	for range 2 {
		chains["rpc-b"].advance()
	}
	for range 6 {
		chains["rpc-a"].advance()
	}
	heads(map[string]int64{"rpc-a": 1005, "rpc-b": 1001, "rpc-c": 1000})
	require.Eventually(t, func() bool {
		tip, ok := network.chainTracker.Tip()
		return ok && tip.Number == 1005
	}, 10*time.Second, 10*time.Millisecond, "the tracker pins rpc-a's head")

	assert.Equal(t, "0x3e9", blockNumber(), "one source alone must not raise eth_blockNumber")
	assert.Equal(t, "0x3e9", latestBlock(), "one source alone must not raise eth_getBlockByNumber(latest)")
	reach(wsHeads, nil, 1001, "ws newHeads")
	reach(grpcHeads, grpcEnded, 1001, "gRPC StreamBlocks")

	for range 4 {
		chains["rpc-b"].advance()
	}
	heads(map[string]int64{"rpc-a": 1005, "rpc-b": 1005})
	require.Eventually(t, func() bool { return blockNumber() == "0x3ed" },
		10*time.Second, 10*time.Millisecond, "a second source at 1005 moves eth_blockNumber")
	reach(wsHeads, nil, 1005, "ws newHeads")
	reach(grpcHeads, grpcEnded, 1005, "gRPC StreamBlocks")
}

// One wss source and one HTTP upstream that is no source, polled every 30s:
// latest is the lower of the two heads, and taps stop at latest, so a
// waiting tap refreshes the HTTP upstream's head instead of holding the
// client until its next poll.
func TestNetworkIntake_NonSourceVoterNeverHoldsTaps(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	chain := newMockChain(1000, false)
	httpVendor, srv := newMockHttpVendor(t, chain)
	_, wsURL := newMockWsVendor(t, chain, nil)
	httpUp := sourceUpstream("rpc-http", srv.URL, nil)
	httpUp.Evm.StatePollerInterval = common.Duration(30 * time.Second)
	addr, cleanup := setupTestERPCServer(t, sourcesConfig(nil, sourceUpstream("rpc-ws", wsURL, nil), httpUp))
	defer cleanup()
	conn := dialWs(t, addr)
	defer conn.Close()
	subscribeNewHeads(t, conn)
	tapLabels := []string{"test_ws", "evm:123", "ws", indexer.SubTypeNewHeads}
	require.Eventually(t, func() bool { return testutil.ToFloat64(telemetry.MetricTaps.WithLabelValues(tapLabels...)) == 1 },
		5*time.Second, 10*time.Millisecond, "the client's tap opens")
	require.Eventually(t, func() bool { return httpVendor.latestPolls.Load() > 0 }, 5*time.Second, 10*time.Millisecond,
		"the HTTP upstream's poller reads its first head")

	polled := httpVendor.latestPolls.Load()
	for want := int64(1001); want <= 1005; want++ {
		chain.advance()
		n, _ := nextNewHead(t, conn, 3*time.Second)
		for n < want && n <= 1000 {
			// The head the tap found when it opened.
			n, _ = nextNewHead(t, conn, 3*time.Second)
		}
		require.Equal(t, want, n, "each pushed head reaches the client well before the HTTP upstream's next 30s poll")
	}
	assert.Greater(t, httpVendor.latestPolls.Load(), polled, "the waiting tap refreshed the HTTP upstream's head")
}

// An unknown source lifts nothing, nor does one eligible source alone; a
// quorum of eligible ones does.
func TestNetworkIntake_EligibleSourceLiftsLatest(t *testing.T) {
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{primaryLatest: "0x3e8", fallbackLatest: "0x3e8"})
	require.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx))

	network.intake.Header("unknown", tipHead("a", 1020))
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx), "an unknown source must not lift latest")

	network.intake.Header("primary-1", tipHead("a", 1001))
	eventuallyPinned(t, network, 1001, "an eligible source's head votes")
	assert.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx), "one eligible source alone must not lift latest")
	network.intake.Header("primary-2", tipHead("a", 1001))
	eventuallyLatest(t, ctx, network, 1001, "a quorum of eligible sources lifts latest")
}

// An upstream the selection policy excludes never reaches clients: its own
// poller sees its heads, but they never vote, so no tap receives them.
func TestNetworkIntake_PolicyExcludedUpstreamNeverReachesClients(t *testing.T) {
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The default policy cordons the fallback tier while the primaries are healthy.
	network, ups, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{primaryLatest: "0x3e8", fallbackLatest: "0x3e8"})
	byId := map[string]*upstream.Upstream{}
	for _, u := range ups {
		byId[u.Id()] = u
	}

	// The client last applied block 1000, so its tap does not race the
	// headers below for its start, as one from f.Head in its goroutine would.
	start, err := evm.ParseHeader(tipHead("a", 1000))
	require.NoError(t, err)
	got := make(chan uint64, 16)
	go func() {
		_ = indexer.NewTaps("test", nil).RunChain(ctx, network.networkId, "test", indexer.SubTypeNewHeads, network.chainTracker, &start, indexer.ReadOpts{},
			func(b indexer.Batch) error {
				for _, blk := range b.Applied {
					got <- blk.Number
				}
				return nil
			})
	}()

	network.intake.Header("fallback-1", tipHead("b", 1010))
	assert.Equal(t, int64(1010), byId["fallback-1"].EvmStatePoller().LatestBlock(), "its own poller still sees the head")
	for _, id := range []string{"primary-1", "primary-2"} {
		network.intake.Header(id, tipHead("a", 1001))
		network.intake.Header(id, tipHead("a", 1002))
	}

	for _, want := range []uint64{1001, 1002} {
		select {
		case n := <-got:
			require.Equal(t, want, n)
		case <-time.After(2 * time.Second):
			t.Fatalf("the client never received block %d", want)
		}
	}
	select {
	case n := <-got:
		t.Fatalf("the client received block %d, which only the excluded upstream has", n)
	case <-time.After(300 * time.Millisecond):
	}
	assert.Equal(t, int64(1002), network.EvmHighestLatestBlockNumber(ctx))
}

// With failover on, a fallback's head votes only while no eligible primary
// leads as a live source. Its own poller sees every head regardless. A
// primary leads at once here; the 60s lead runs in TestWebSocket_FallbackPollsUntilPrimaryLeads.
func TestNetworkIntake_FallbackHeadsHeldWhilePrimariesStream(t *testing.T) {
	prevLead := primaryLeadAfter.Load()
	primaryLeadAfter.Store(0)
	t.Cleanup(func() { primaryLeadAfter.Store(prevLead) })
	setup := func(t *testing.T, ctx context.Context, noPolicy bool) (*Network, map[string]*upstream.Upstream) {
		network, ups, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8", // 1000
			fallbackLatest: "0x3e8",
			enableFailover: true,
			noPolicy:       noPolicy,
		})
		require.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx))
		byId := make(map[string]*upstream.Upstream, len(ups))
		for _, u := range ups {
			byId[u.Id()] = u
		}
		return network, byId
	}

	t.Run("HeldWhileAPrimaryStreams", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, ups := setup(t, ctx, true)

		network.intake.Header("primary-2", tipHead("a", 1000))
		network.intake.Header("fallback-1", tipHead("b", 1010))
		assert.Equal(t, int64(1010), ups["fallback-1"].EvmStatePoller().LatestBlock(),
			"the fallback's poller still sees its head")
		time.Sleep(100 * time.Millisecond)
		assert.Less(t, pinned(network), int64(1010), "a held head must not vote")

		network.intake.Header("primary-2", tipHead("a", 1001))
		eventuallyLatest(t, ctx, network, 1001, "the primary's head lifts latest")
	})

	t.Run("DeliveredWhenNoPrimaryStreams", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, _ := setup(t, ctx, true)

		network.intake.Header("fallback-1", tipHead("b", 1010))
		eventuallyPinned(t, network, 1010, "with no primary source, the fallback votes")
	})

	t.Run("DeliveredWhenPolicyExcludesPrimaries", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, ups := setup(t, ctx, false)

		network.intake.Header("primary-1", tipHead("a", 1000))
		network.intake.Header("primary-2", tipHead("a", 1000))
		network.intake.Header("fallback-1", tipHead("b", 1010))
		time.Sleep(100 * time.Millisecond)
		require.Less(t, pinned(network), int64(1010))

		ups["primary-1"].Cordon("*", "test")
		ups["primary-2"].Cordon("*", "test")
		policy.TickForTest(network.policyEngine, network.networkId, "*")

		network.intake.Header("fallback-1", tipHead("b", 1011))
		eventuallyPinned(t, network, 1011, "excluded primaries no longer hold the fallback back")
	})

	t.Run("DeliveredWithFailoverOff", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8",
			fallbackLatest: "0x3e8",
			noPolicy:       true,
		})

		network.intake.Header("primary-1", tipHead("a", 1000))
		network.intake.Header("fallback-1", tipHead("a", 1001))
		eventuallyPinned(t, network, 1001, "without failover every eligible source votes")
	})
}

// A head far past the poller's (a major jump) is verified asynchronously
// before the poller takes it. Until then it must not vote: a head from an
// endpoint now answering for another chain must never reach clients.
func TestNetworkIntake_WithholdsUnacceptedMajorJump(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, u := setupWsTipNetwork(t, ctx, "rpc2", "http://rpc2.localhost")
	u.EvmStatePoller().SuggestLatestBlock(1000)
	require.Equal(t, int64(1000), u.EvmStatePoller().LatestBlock())

	// From here the endpoint answers for another chain.
	util.ResetGock()
	gock.New("http://rpc2.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), `eth_chainId`) }).
		Reply(200).JSON([]byte(`{"result":"0x1"}`))

	network.intake.Header("rpc2", tipHead("e", 5_000_000))
	time.Sleep(300 * time.Millisecond) // let the async verification finish (and fail)
	assert.Equal(t, int64(1000), u.EvmStatePoller().LatestBlock())
	assert.Less(t, pinned(network), int64(5_000_000))
	network.intake.Header("rpc2", tipHead("a", 1001))
	network.intake.Header("rpc2", tipHead("a", 1002))
	eventuallyPinned(t, network, 1002, "the next heads of the right chain vote")
}

// A head from a source the network does not know never votes and reaches
// no client.
func TestNetworkIntake_WithholdsUnverifiableSource(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupWsTipNetwork(t, ctx, "rpc3", "http://rpc3.localhost")
	before := pinned(network)

	network.intake.Header("not-in-this-network", tipHead("a", 5_000_000))
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, before, pinned(network))
}
