package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	require.Eventually(t, func() bool { return servedHead(network, ctx).Latest == want },
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

// An HTTP upstream that is no source, polled every 30s, can hold latest:
// as the lower of two heads, or as the one member of a guarantee subset.
// Taps stop at latest, so a waiting tap refreshes every voter below the
// held block instead of holding the client until that upstream's next poll.
func TestNetworkIntake_NonSourceVoterNeverHoldsTaps(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wsSources []string
		network   func(nw *common.NetworkConfig)
	}{
		{name: "lower of two heads", wsSources: []string{"a-ws"}},
		{
			// Routing order puts z-http last, so the ballot's first voters,
			// all at the pushed head, are not the ones holding latest.
			name:      "guarantee subset",
			wsSources: []string{"a-ws", "b-ws", "c-ws"},
			network: func(nw *common.NetworkConfig) {
				nw.SelectionPolicy = &common.SelectionPolicyConfig{
					EvalFunc: `(upstreams, ctx) => upstreams.filter(u => true).sort((a, b) => (a.id < b.id ? -1 : 1))`,
				}
				nw.Evm.ServedTip = &common.EvmServedTipConfig{GuaranteedFor: []string{"z-http"}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupGock()
			defer util.ResetGock()

			chain := newMockChain(1000, false)
			httpVendor, srv := newMockHttpVendor(t, chain)
			ups := make([]*common.UpstreamConfig, 0, len(tc.wsSources)+1)
			for _, id := range tc.wsSources {
				_, wsURL := newMockWsVendor(t, chain, nil)
				ups = append(ups, sourceUpstream(id, wsURL, nil))
			}
			httpUp := sourceUpstream("z-http", srv.URL, nil)
			httpUp.Evm.StatePollerInterval = common.Duration(30 * time.Second)
			addr, cleanup := setupTestERPCServer(t, sourcesConfig(tc.network, append(ups, httpUp)...))
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
		})
	}
}

// An unknown source lifts nothing, nor does one eligible source alone; a
// quorum of eligible ones does.
func TestNetworkIntake_EligibleSourceLiftsLatest(t *testing.T) {
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{primaryLatest: "0x3e8", fallbackLatest: "0x3e8"})
	require.Equal(t, int64(1000), servedHead(network, ctx).Latest)

	network.intake.Polled("unknown", tipHead("a", 1020), evm.HeaderStreamed)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int64(1000), servedHead(network, ctx).Latest, "an unknown source must not lift latest")

	network.intake.Polled("primary-1", tipHead("a", 1001), evm.HeaderStreamed)
	eventuallyPinned(t, network, 1001, "an eligible source's head votes")
	assert.Equal(t, int64(1000), servedHead(network, ctx).Latest, "one eligible source alone must not lift latest")
	network.intake.Polled("primary-2", tipHead("a", 1001), evm.HeaderStreamed)
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

	network.intake.Polled("fallback-1", tipHead("b", 1010), evm.HeaderStreamed)
	assert.Equal(t, int64(1010), byId["fallback-1"].EvmStatePoller().LatestBlock(), "its own poller still sees the head")
	for _, id := range []string{"primary-1", "primary-2"} {
		network.intake.Polled(id, tipHead("a", 1001), evm.HeaderStreamed)
		network.intake.Polled(id, tipHead("a", 1002), evm.HeaderStreamed)
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
	assert.Equal(t, int64(1002), servedHead(network, ctx).Latest)
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
		require.Equal(t, int64(1000), servedHead(network, ctx).Latest)
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

		network.intake.Polled("primary-2", tipHead("a", 1000), evm.HeaderStreamed)
		network.intake.Polled("fallback-1", tipHead("b", 1010), evm.HeaderStreamed)
		assert.Equal(t, int64(1010), ups["fallback-1"].EvmStatePoller().LatestBlock(),
			"the fallback's poller still sees its head")
		time.Sleep(100 * time.Millisecond)
		assert.Less(t, pinned(network), int64(1010), "a held head must not vote")

		network.intake.Polled("primary-2", tipHead("a", 1001), evm.HeaderStreamed)
		eventuallyLatest(t, ctx, network, 1001, "the primary's head lifts latest")
	})

	t.Run("DeliveredWhenNoPrimaryStreams", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, _ := setup(t, ctx, true)

		network.intake.Polled("fallback-1", tipHead("b", 1010), evm.HeaderStreamed)
		eventuallyPinned(t, network, 1010, "with no primary source, the fallback votes")
	})

	t.Run("DeliveredWhenPolicyExcludesPrimaries", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, ups := setup(t, ctx, false)

		network.intake.Polled("primary-1", tipHead("a", 1000), evm.HeaderStreamed)
		network.intake.Polled("primary-2", tipHead("a", 1000), evm.HeaderStreamed)
		network.intake.Polled("fallback-1", tipHead("b", 1010), evm.HeaderStreamed)
		time.Sleep(100 * time.Millisecond)
		require.Less(t, pinned(network), int64(1010))

		ups["primary-1"].Cordon("*", "test")
		ups["primary-2"].Cordon("*", "test")
		policy.TickForTest(network.policyEngine, network.networkId, "*")

		network.intake.Polled("fallback-1", tipHead("b", 1011), evm.HeaderStreamed)
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

		network.intake.Polled("primary-1", tipHead("a", 1000), evm.HeaderStreamed)
		network.intake.Polled("fallback-1", tipHead("a", 1001), evm.HeaderStreamed)
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

	network.intake.Polled("rpc2", tipHead("e", 5_000_000), evm.HeaderStreamed)
	time.Sleep(300 * time.Millisecond) // let the async verification finish (and fail)
	assert.Equal(t, int64(1000), u.EvmStatePoller().LatestBlock())
	assert.Less(t, pinned(network), int64(5_000_000))
	network.intake.Polled("rpc2", tipHead("a", 1001), evm.HeaderStreamed)
	network.intake.Polled("rpc2", tipHead("a", 1002), evm.HeaderStreamed)
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

	network.intake.Polled("not-in-this-network", tipHead("a", 5_000_000), evm.HeaderStreamed)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, before, pinned(network))
}

// sourcesNetwork starts an eRPC server for cfg and returns its network,
// its upstreams by id and its address.
func sourcesNetwork(t *testing.T, cfg *common.Config) (*Network, map[string]*upstream.Upstream, string) {
	t.Helper()
	srv, addr, cancel := startTestERPCServer(t, cfg)
	t.Cleanup(func() { _ = srv.Shutdown(&log.Logger); cancel() })
	prj, err := srv.erpc.GetProject("test_ws")
	require.NoError(t, err)
	network, err := prj.GetNetwork(context.Background(), "evm:123")
	require.NoError(t, err)
	byId := map[string]*upstream.Upstream{}
	for _, u := range network.upstreamsRegistry.GetNetworkUpstreams(context.Background(), network.networkId) {
		byId[u.Id()] = u
	}
	require.Len(t, byId, len(cfg.Projects[0].Upstreams))
	return network, byId, addr
}

// pollerUpstream is an HTTP upstream that is no source, polled every 30s.
func pollerUpstream(id, endpoint string) *common.UpstreamConfig {
	u := sourceUpstream(id, endpoint, nil)
	u.Evm.StatePollerInterval = common.Duration(30 * time.Second)
	return u
}

// A group tracker's pins go out as far as its lane's Latest. When that
// Latest moves through a counter alone (another instance, a client
// response), the group tracker confirms the pin at once, not at its next
// header.
func TestNetworkIntake_GroupLaneConfirmsWhenLatestMovesWithoutHeader(t *testing.T) {
	setupGock()
	defer util.ResetGock()
	ctx := context.Background()

	chain := newMockChain(1000, false)
	aVendor, aURL := newMockWsVendor(t, chain, nil)
	xVendor, xURL := newMockWsVendor(t, chain, nil)
	httpVendor, srv := newMockHttpVendor(t, chain)
	network, byId, _ := sourcesNetwork(t, sourcesConfig(func(nw *common.NetworkConfig) {
		nw.Evm.ServedTip = &common.EvmServedTipConfig{Quorum: 2}
	}, sourceUpstream("g-a", aURL, nil), pollerUpstream("g-b", srv.URL), sourceUpstream("x-c", xURL, nil)))
	require.Eventually(t, func() bool {
		return httpVendor.latestPolls.Load() > 0 && aVendor.subscribes.Load() > 0 && xVendor.subscribes.Load() > 0
	}, 5*time.Second, 10*time.Millisecond, "the poller reads its first head and both sources subscribe")
	group := evm.ChainTrackerOf(ctx, network, "g-*")
	require.NotSame(t, network.chainTracker, group, "g-* is a lane of its own")
	laneReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	laneReq.SetDirectives(&common.RequestDirectives{UseUpstream: "g-*"})
	laneCtx := context.WithValue(ctx, common.RequestContextKey, laneReq)

	chain.advance()
	require.Eventually(t, func() bool {
		tip, ok := group.Tip()
		hv := network.EvmHashVoters("g-*", group.W(), false)
		return ok && tip.Number == 1001 && slices.Equal(hv.Hash, []string{"g-a"})
	}, 10*time.Second, 10*time.Millisecond, "the lane pins 1001, and only g-a's vote is fresh once g-b's ages past W")
	require.Equal(t, int64(1000), servedHead(network, laneCtx).Latest, "g-b's counter holds the lane's Latest")
	time.Sleep(200 * time.Millisecond)
	if c, ok := group.Confirmed(); ok {
		require.Less(t, c.Number, uint64(1001), "the lane's Latest holds 1001 back")
	}

	byId["g-b"].EvmStatePoller().SuggestLatestBlock(1001)
	require.Eventually(t, func() bool {
		c, ok := group.Confirmed()
		return ok && c.Number == 1001
	}, 2*time.Second, 10*time.Millisecond, "the lane's Latest reaching 1001 confirms it without another header")
}

// One wss source and two HTTP pollers, quorum 2: the pollers' counters
// already show the pushed head (another instance or a client response
// moved them), but their hash votes do not. The waiting tap polls them for
// their heads instead of waiting for their 30s polls or for W.
func TestNetworkIntake_HeldHashVoteNeedPollsVotersBelowThePin(t *testing.T) {
	setupGock()
	defer util.ResetGock()
	ctx := context.Background()

	chain := newMockChain(1000, false)
	aVendor, aURL := newMockWsVendor(t, chain, nil)
	bVendor, bSrv := newMockHttpVendor(t, chain)
	cVendor, cSrv := newMockHttpVendor(t, chain)
	network, byId, addr := sourcesNetwork(t, sourcesConfig(func(nw *common.NetworkConfig) {
		nw.Evm.ServedTip = &common.EvmServedTipConfig{Quorum: 2}
	}, sourceUpstream("a-ws", aURL, nil), pollerUpstream("b-http", bSrv.URL), pollerUpstream("c-http", cSrv.URL)))
	conn := dialWs(t, addr)
	defer conn.Close()
	subscribeNewHeads(t, conn)
	require.Eventually(t, func() bool {
		for _, id := range []string{"b-http", "c-http"} {
			if v, ok := network.intake.hashVote(byId[id]); !ok || v.Number != 1000 {
				byId[id].EvmStatePoller().RefreshLatestBlock()
				return false
			}
		}
		return aVendor.subscribes.Load() > 0
	}, 5*time.Second, 10*time.Millisecond, "both pollers vote 1000 and the source subscribes")
	n, _ := nextNewHead(t, conn, 3*time.Second)
	require.Equal(t, int64(1000), n)

	polled := bVendor.latestPolls.Load() + cVendor.latestPolls.Load()
	for _, id := range []string{"b-http", "c-http"} {
		byId[id].EvmStatePoller().SuggestLatestBlock(1001)
	}
	eventuallyLatest(t, ctx, network, 1001, "the pollers' counters move Latest to 1001")
	chain.advance()
	n, _ = nextNewHead(t, conn, 2500*time.Millisecond)
	require.Equal(t, int64(1001), n, "the pushed head reaches the client before the pollers' votes age past W")
	assert.Greater(t, bVendor.latestPolls.Load()+cVendor.latestPolls.Load(), polled, "the held tap polled the voters below the pin")
}

// A 1-block reorg leaves a healthy HTTP voter's last vote, polled up to 30s
// ago, on the old branch. The tracker polls it at once; its answer is on
// the new branch, so it is never demoted. Then a reorg to a shorter branch:
// the voter's vote, polled across a gap, has no header at the new head's
// height, so only the fork poll's answer shows that its node moved.
func TestNetworkIntake_ReorgNeverDemotesHealthyPoller(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	chain := newMockChain(1000, false)
	aVendor, aURL := newMockWsVendor(t, chain, nil)
	bVendor, bURL := newMockWsVendor(t, chain, nil)
	httpVendor, srv := newMockHttpVendor(t, chain)
	network, byId, _ := sourcesNetwork(t, sourcesConfig(nil,
		sourceUpstream("a-ws", aURL, nil), sourceUpstream("b-ws", bURL, nil), pollerUpstream("z-http", srv.URL)))
	require.Eventually(t, func() bool {
		return httpVendor.latestPolls.Load() > 0 && aVendor.subscribes.Load() > 0 && bVendor.subscribes.Load() > 0
	}, 5*time.Second, 10*time.Millisecond, "the poller reads its first head and both sources subscribe")
	for range 3 {
		chain.advance()
	}
	eventuallyPinned(t, network, 1003, "the sources' pushes pin 1003")
	require.Eventually(t, func() bool {
		v, ok := network.intake.hashVote(byId["z-http"])
		if !ok || v.Number != 1003 {
			byId["z-http"].EvmStatePoller().RefreshLatestBlock()
			return false
		}
		return true
	}, 5*time.Second, 10*time.Millisecond, "the poller's vote is 1003")

	polled := httpVendor.latestPolls.Load()
	chain.reorg(1)
	eventuallyPinned(t, network, 1004, "the sources' pushes pin the new branch")
	deadline := time.Now().Add(network.chainTracker.W() + 2*time.Second)
	for time.Now().Before(deadline) {
		require.False(t, network.isForkDemoted(byId["z-http"]), "a healthy poller whose last vote a reorg left behind is never demoted")
		time.Sleep(20 * time.Millisecond)
	}
	assert.Greater(t, httpVendor.latestPolls.Load(), polled, "the tracker polled the voter whose vote left the pins")

	for range 3 {
		chain.advance()
	}
	eventuallyPinned(t, network, 1007, "the sources' pushes pin 1007")
	require.Eventually(t, func() bool {
		v, ok := network.intake.hashVote(byId["z-http"])
		if !ok || v.Number != 1007 {
			byId["z-http"].EvmStatePoller().RefreshLatestBlock()
			return false
		}
		return true
	}, 5*time.Second, 10*time.Millisecond, "the poller's vote is 1007, polled across 1005 and 1006")
	chain.rewind(1005)
	eventuallyPinned(t, network, 1005, "the sources' pushes pin the shorter branch")
	deadline = time.Now().Add(network.chainTracker.W() + 2*time.Second)
	for time.Now().Before(deadline) {
		require.False(t, network.isForkDemoted(byId["z-http"]), "a healthy poller on the shorter branch is never demoted")
		time.Sleep(20 * time.Millisecond)
	}
	v, _ := network.intake.hashVote(byId["z-http"])
	assert.Equal(t, int64(1005), v.Number, "the fork poll's answer, lower than the vote, moves it")
}

// A push source's socket closes on a halted chain and comes back: its first
// header after the reconnect is its current vote. That vote is fresh again,
// so the source neither stays last in routing nor stops being a hash voter.
func TestNetworkIntake_ReconnectOnHaltedChainRenewsVote(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	chain := newMockChain(1000, false)
	aVendor, aURL := newMockWsVendor(t, chain, nil)
	bVendor, bURL := newMockWsVendor(t, chain, nil)
	network, byId, _ := sourcesNetwork(t, sourcesConfig(nil, sourceUpstream("a-ws", aURL, nil), sourceUpstream("b-ws", bURL, nil)))
	require.Eventually(t, func() bool { return aVendor.subscribes.Load() > 0 && bVendor.subscribes.Load() > 0 },
		5*time.Second, 10*time.Millisecond, "both sources subscribe")
	chain.advance()
	eventuallyPinned(t, network, 1001, "the sources' pushes pin 1001")
	head, err := json.Marshal(chain.block(1001))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		v, ok := network.intake.hashVote(byId["a-ws"])
		return ok && v.Number == 1001
	}, 5*time.Second, 10*time.Millisecond, "a-ws votes 1001")

	aVendor.dropConns()
	require.Eventually(t, func() bool {
		s := network.hashStale.Load()
		return s != nil && slices.Contains(*s, "a-ws")
	}, 5*time.Second, 10*time.Millisecond, "a closed socket goes last in routing")

	pushIntake{network.intake}.Header("a-ws", head)
	require.Eventually(t, func() bool {
		v, _ := network.intake.hashVote(byId["a-ws"])
		return !v.Expired && network.hashStale.Load() == nil
	}, 2*time.Second, 10*time.Millisecond, "the reconnect's first header, the current vote, renews it on a halted chain")
}

// A polled voter is a hash voter until its node's failed polls of latest
// span W, first to last, with no success since (spec §6.1):
//  1. Two 429s W/2 apart, read W after the first, keep the vote; a third
//     past W ends it; a success brings it back.
//  2. A slow poll the node fails after a later poll succeeded started before
//     that success, so it does not count: a 429 W after it keeps the vote.
//  3. Client traffic uses up the upstream's daily rate-limit budget: eRPC's
//     own refusals for 2·W are no poll, so the vote holds.
func TestNetworkIntake_PolledVoterEndsOnlyOnNodeFailuresSpanningW(t *testing.T) {
	setupGock()
	defer util.ResetGock()
	ctx := context.Background()

	chain := newMockChain(1000, false)
	vendor, srv := newMockHttpVendor(t, chain)
	up := pollerUpstream("p-http", srv.URL)
	up.RateLimitBudget = "p-budget"
	up.RateLimitAutoTune = &common.RateLimitAutoTuneConfig{Enabled: util.BoolPtr(false)}
	up.Failsafe = []*common.FailsafeConfig{{MatchMethod: "*"}}
	up.JsonRpc = &common.JsonRpcUpstreamConfig{SupportsBatch: util.BoolPtr(false)}
	cfg := sourcesConfig(nil, up)
	cfg.RateLimiters.Budgets = []*common.RateLimitBudgetConfig{{Id: "p-budget",
		Rules: []*common.RateLimitRuleConfig{{Method: "eth_getBlockByNumber", MaxCount: 100, Period: common.RateLimitPeriodDay}}}}
	network, byId, addr := sourcesNetwork(t, cfg)
	poller := byId["p-http"].EvmStatePoller()
	hashVoter := func() bool {
		return slices.Contains(network.EvmHashVoters("", network.chainTracker.W(), false).Hash, "p-http")
	}
	restored := func(msg string) {
		require.Eventually(t, func() bool {
			if !hashVoter() {
				poller.RefreshLatestBlock()
				return false
			}
			return true
		}, 5*time.Second, 10*time.Millisecond, msg)
	}
	// pollFails polls once the poller's 100ms debounce has passed.
	pollFails := func(msg string) {
		time.Sleep(150 * time.Millisecond)
		_, err := poller.PollLatestBlockNumber(ctx)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded), "%s: got %v", msg, err)
	}
	restored("the poller's head makes it a hash voter")

	w := network.chainTracker.W()
	polled := vendor.latestPolls.Load()
	vendor.failLatest.Store(http.StatusTooManyRequests)
	pollFails("the first 429")
	first := time.Now()
	time.Sleep(w / 2)
	pollFails("the second 429")
	time.Sleep(time.Until(first.Add(w + 300*time.Millisecond)))
	require.Equal(t, polled+2, vendor.latestPolls.Load(), "only the test polls the node")
	require.True(t, hashVoter(), "two 429s W/2 apart: the polls failed for less than W")
	pollFails("the third 429")
	require.False(t, hashVoter(), "failed polls spanning W end the vote")
	vendor.failLatest.Store(0)
	restored("a success ends the failures")

	polled = vendor.latestPolls.Load()
	vendor.slowFail.Store(int64(time.Second))
	poller.PollHead(false)
	require.Eventually(t, func() bool { return vendor.latestPolls.Load() > polled }, 2*time.Second, 5*time.Millisecond, "the slow poll reaches the node")
	time.Sleep(200 * time.Millisecond)
	_, err := poller.PollLatestBlockNumber(ctx)
	require.NoError(t, err)
	require.Equal(t, polled+2, vendor.latestPolls.Load(), "a poll after the slow one succeeds while it is in flight")
	require.Eventually(t, func() bool { return vendor.slowFailed.Load() == 1 }, 3*time.Second, 5*time.Millisecond, "the node fails the slow poll")
	time.Sleep(100 * time.Millisecond)
	slowFailedAt := time.Now()
	vendor.failLatest.Store(http.StatusTooManyRequests)
	time.Sleep(time.Until(slowFailedAt.Add(w + 300*time.Millisecond)))
	pollFails("a 429 W after the slow poll failed")
	require.True(t, hashVoter(), "a failure whose request started before the last success does not count")
	vendor.failLatest.Store(0)
	restored("a success ends the failures")

	flood, stopFlood := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { stopFlood(); wg.Wait() }()
	var answer atomic.Value
	for range 4 {
		wg.Go(func() {
			body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`)
			for flood.Err() == nil {
				if resp, err := http.Post(fmt.Sprintf("http://%s/test_ws/evm/123", addr), "application/json", bytes.NewReader(body)); err == nil {
					b, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					answer.Store(string(b))
				}
			}
		})
	}
	var lastErr error
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && !common.HasErrorCode(lastErr, common.ErrCodeUpstreamRateLimitRuleExceeded); {
		time.Sleep(150 * time.Millisecond)
		_, lastErr = poller.PollLatestBlockNumber(ctx)
	}
	require.True(t, common.HasErrorCode(lastErr, common.ErrCodeUpstreamRateLimitRuleExceeded),
		"client traffic uses up the upstream's budget: last poll %v, last client answer %.600v", lastErr, answer.Load())
	polled = vendor.latestPolls.Load()
	var refused []time.Time
	for end := time.Now().Add(2 * network.chainTracker.W()); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if _, err := poller.PollLatestBlockNumber(ctx); common.HasErrorCode(err, common.ErrCodeUpstreamRateLimitRuleExceeded) {
			refused = append(refused, time.Now())
		}
	}
	keptVote := hashVoter()
	stopFlood()
	wg.Wait()
	require.Equal(t, polled, vendor.latestPolls.Load(), "client traffic uses up the budget: no poll reaches the node")
	require.NotEmpty(t, refused)
	require.GreaterOrEqual(t, refused[len(refused)-1].Sub(refused[0]), network.chainTracker.W(), "eRPC refused the polls for W")
	require.True(t, keptVote, "polls eRPC refused itself never end a hash vote")
}
