package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the semantics of Network.EvmHighestLatestBlockNumber: the
// quorum order statistic — the freshest block at least servedTip.quorum
// eligible, non-benched upstreams already have (common.PickServedTip). Most
// cases run quorum "majority" and are designed so MAX(tips) != the pick to
// make regressions visible.

// servedTipFixture is a compact description of one upstream's poller state
// at test time. Used to drive the lighter setupServedTipNetwork helper.
type servedTipFixture struct {
	id          string
	chainID     int64
	latestBlock int64
	// upperExactBlock, when > 0, configures blockAvailability.upper.exactBlock —
	// the upstream still polls latestBlock as its head, but only SERVES up to
	// this block, so its effective latest is the bound rather than a head
	// observation (a "legacy" upstream that serves only a frozen historical range).
	upperExactBlock int64
	// upperLatestMinus, when set, configures blockAvailability.upper.latestBlockMinus
	// — a bound DERIVED from the upstream's own head, so it tracks the chain at
	// a fixed lag and remains a head observation (the "serve up to head-N"
	// shape).
	upperLatestMinus *int64
	syncing          bool
	ignoreMethods    []string
	tags             []string
}

// setupServedTipNetwork bootstraps a Network with the supplied upstream
// fixtures and pushes each upstream's poller to the requested state.
//
// Each fixture maps to one upstream with a unique localhost endpoint mocked
// via gock to answer eth_chainId so initial validation succeeds. After
// bootstrap, SuggestLatestBlock and SetSyncingState are applied per fixture.
//
// Returns the network and the upstream slice in input order. The fixture runs
// quorum "majority" on both axes.
func setupServedTipNetwork(t *testing.T, ctx context.Context, fixtures []servedTipFixture) (*Network, []*upstream.Upstream) {
	return setupServedTipNetworkWith(t, ctx, fixtures, &common.EvmServedTipConfig{Quorum: common.ServedTipQuorumMajority})
}

func setupServedTipNetworkWith(t *testing.T, ctx context.Context, fixtures []servedTipFixture, stCfg *common.EvmServedTipConfig) (*Network, []*upstream.Upstream) {
	t.Helper()

	if len(fixtures) == 0 {
		t.Fatal("setupServedTipNetwork requires at least one fixture")
	}

	chainID := fixtures[0].chainID
	if chainID == 0 {
		chainID = 123
	}

	upstreamConfigs := make([]*common.UpstreamConfig, 0, len(fixtures))
	for _, f := range fixtures {
		cid := f.chainID
		if cid == 0 {
			cid = chainID
		}
		endpoint := "http://" + f.id + ".localhost"
		evmCfg := &common.EvmUpstreamConfig{ChainId: cid}
		if f.upperExactBlock > 0 {
			upper := f.upperExactBlock
			evmCfg.BlockAvailability = &common.EvmBlockAvailabilityConfig{
				Upper: &common.EvmAvailabilityBoundConfig{ExactBlock: &upper},
			}
		}
		if f.upperLatestMinus != nil {
			evmCfg.BlockAvailability = &common.EvmBlockAvailabilityConfig{
				Upper: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: f.upperLatestMinus},
			}
		}
		upstreamConfigs = append(upstreamConfigs, &common.UpstreamConfig{
			Type:          common.UpstreamTypeEvm,
			Id:            f.id,
			Endpoint:      endpoint,
			IgnoreMethods: f.ignoreMethods,
			Tags:          f.tags,
			Evm:           evmCfg,
		})

		gock.New(endpoint).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, `eth_chainId`)
			}).
			Reply(200).
			JSON([]byte(`{"result":"0x7b"}`))
	}

	rateLimitersRegistry, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)
	// The head bench state machine shares the guard's clock, so
	// pinServedTipClock drives both.
	metricsTracker.SetClockForTest(func() time.Time { return servedTipClock() })

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(
		&log.Logger,
		vr,
		[]*common.ProviderConfig{},
		nil,
	)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000, MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)

	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx,
		&log.Logger,
		"test",
		upstreamConfigs,
		ssr,
		rateLimitersRegistry,
		vr,
		pr,
		nil,
		metricsTracker,
		nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:   chainID,
			ServedTip: stCfg,
		},
	}

	network, err := NewNetwork(
		ctx,
		&log.Logger,
		"test",
		networkConfig,
		rateLimitersRegistry,
		upstreamsRegistry,
		metricsTracker,
		nil,
	)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)

	initErr := upstreamsRegistry.GetInitializer().WaitForTasks(ctx)
	require.NoError(t, initErr, "Upstream initializer failed to complete tasks")

	err = network.Bootstrap(ctx)
	require.NoError(t, err)
	network.PinUpstreamOrderForTest()
	time.Sleep(250 * time.Millisecond)

	upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(chainID))
	require.Len(t, upsList, len(fixtures))

	// Re-order returned upstreams to match fixtures' input order.
	ordered := make([]*upstream.Upstream, len(fixtures))
	for i, f := range fixtures {
		for _, u := range upsList {
			if u.Id() == f.id {
				ordered[i] = u
				break
			}
		}
		require.NotNil(t, ordered[i], "fixture %q not found in upstreams list", f.id)
	}

	// Push each upstream's poller to the requested state.
	for i, f := range fixtures {
		if f.latestBlock > 0 {
			ordered[i].EvmStatePoller().SuggestLatestBlock(f.latestBlock)
		}
		if f.syncing {
			ordered[i].EvmStatePoller().SetSyncingState(common.EvmSyncingStateSyncing)
		} else {
			ordered[i].EvmStatePoller().SetSyncingState(common.EvmSyncingStateNotSyncing)
		}
	}
	time.Sleep(50 * time.Millisecond)

	return network, ordered
}

// ----- new-semantics scenarios ----------------------------------------------

// Three close upstreams: the majority head (2nd of 3), not MAX, is served.
func TestServedTip_ThreeUpstreams_AllClose_ReturnsMajority(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100},
		{id: "u2", chainID: 123, latestBlock: 99},
		{id: "u3", chainID: 123, latestBlock: 98},
	})

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(99), served,
		"served tip must be the majority head (2 of 3 have 99), not MAX(100)")
}

// One leader + two laggers: the majority view (the middle head) wins — the
// single leader cannot define "latest" for the whole network.
func TestServedTip_OneLeader_TwoLaggers_ReturnsMajority(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "leader", chainID: 123, latestBlock: 100},
		{id: "lagger1", chainID: 123, latestBlock: 50},
		{id: "lagger2", chainID: 123, latestBlock: 49},
	})

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(50), served,
		"with 1 leader vs 2 laggers the majority head (50) is served; "+
			"MAX semantics would return the leader's 100")
}

// Three close + two way-behind: the majority head is the 3rd of 5 (98) —
// fresh enough to be useful, held by a strict majority. MAX would return 100.
func TestServedTip_FiveUpstreams_ThreeClose_TwoBehind(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100},
		{id: "u2", chainID: 123, latestBlock: 99},
		{id: "u3", chainID: 123, latestBlock: 98},
		{id: "u4", chainID: 123, latestBlock: 50},
		{id: "u5", chainID: 123, latestBlock: 49},
	})

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(98), served,
		"majority head (3 of 5 have 98) is served; MAX semantics would return 100")
}

// Monotonicity under upstream retreat: the served tip must not regress when
// upstreams transiently report lower heads. There is no network-level counter
// anymore — the guarantee comes from the per-upstream poller counters being
// monotonic (small retreats are rejected at the poller layer), and the
// majority of monotonic inputs is monotonic while the set is stable.
func TestServedTip_NoRegressionOnUpstreamRetreat(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100},
		{id: "u2", chainID: 123, latestBlock: 100},
		{id: "u3", chainID: 123, latestBlock: 99},
	})

	served1 := network.EvmHighestLatestBlockNumber(ctx)
	require.Equal(t, int64(100), served1,
		"first call: majority of [100,100,99] = 100")

	// All upstreams retreat (e.g., transient outage causing pollers to lose
	// state, or a partial chain reorg perceived by some upstreams).
	ups[0].EvmStatePoller().SuggestLatestBlock(80)
	ups[1].EvmStatePoller().SuggestLatestBlock(79)
	ups[2].EvmStatePoller().SuggestLatestBlock(78)
	time.Sleep(50 * time.Millisecond)

	served2 := network.EvmHighestLatestBlockNumber(ctx)
	// The per-upstream poller is monotonic via CounterInt64: its TryUpdate
	// rejects the small retreat, so the majority over the (clamped) poller
	// state cannot regress either.
	assert.GreaterOrEqual(t, served2, served1,
		"served tip must never regress; expected >= %d, got %d", served1, served2)
}

// Cold start: no upstreams populated yet. Returns 0.
func TestServedTip_NoUpstreamsHaveBlock_ReturnsZero(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 0},
		{id: "u2", chainID: 123, latestBlock: 0},
	})

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(0), served, "no upstream observations yet → 0")
}

// All upstreams syncing → filtered out → 0.
func TestServedTip_AllSyncing_ReturnsZero(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100, syncing: true},
		{id: "u2", chainID: 123, latestBlock: 200, syncing: true},
	})

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(0), served, "all upstreams syncing → no candidates → 0")
}

// ----- opt-in gating + eligible-set sourcing -------------------------------

// The default config (nil servedTip = quorum 2) serves the second-highest head:
// not MAX and not the majority pick.
func TestServedTip_DefaultQuorum_ServesSecondHighest(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100},
		{id: "u2", chainID: 123, latestBlock: 99},
		{id: "u3", chainID: 123, latestBlock: 98},
	}, nil)

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(99), served,
		"the default quorum serves the second-highest head, not MAX(tips)=100")
}

// A lone upstream reporting another chain's height must not define the network
// head on the default config: "latest" interpolation, eth_blockNumber and the
// cache's known-highest-block all read this accessor.
func TestServedTip_DefaultQuorum_LoneFarAheadUpstreamDoesNotDefineHead(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 32_610_710},
		{id: "u2", chainID: 123, latestBlock: 32_610_709},
		{id: "rogue", chainID: 123, latestBlock: 62_381_379},
	}, nil)

	assert.Equal(t, int64(32_610_710), network.EvmHighestLatestBlockNumber(ctx),
		"the rogue's head is not corroborated")
	ceiling, known := network.futureBlockCeiling(ctx)
	assert.True(t, known)
	assert.Equal(t, int64(62_381_379), ceiling,
		"the raw max stays visible for the future-block short-circuit")
}

// A selection-policy-EXCLUDED upstream must drop out of the served tip — head
// tracking and routing share one eligible set. Here u3 reports the cluster min
// (98); once the policy keeps only {u1,u2} eligible, the served tip must move to
// the eligible cluster min (99) and never reflect u3's block.
func TestServedTip_ExcludedUpstreamDropsOutOfTip(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100},
		{id: "u2", chainID: 123, latestBlock: 99},
		{id: "u3", chainID: 123, latestBlock: 98},
	})

	// Warm the policy slot (lazy-created on first GetOrdered), then restrict the
	// eligible set so u3 is excluded.
	_ = network.EvmHighestLatestBlockNumber(ctx)
	network.PinUpstreamOrderForTest("u1", "u2")

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(99), served,
		"served tip must ignore policy-excluded u3 (block 98); eligible {100,99} → min 99")
}

// A configured guaranteed method clamps the global served tip down to what that
// method's SUPPORTING upstreams can serve. u3 is the only trace_block-capable
// upstream and it lags (90); u1/u2 (99/100) don't support trace_block. Without
// the guarantee the served tip is the dominant cluster min (99, u3 an outlier);
// with the guarantee it must drop to 90 so a trace_block("latest") request hits
// a block u3 actually has.
func TestServedTip_GuaranteedMethodClampsTip(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fixtures := []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100, ignoreMethods: []string{"trace_block"}},
		{id: "u2", chainID: 123, latestBlock: 99, ignoreMethods: []string{"trace_block"}},
		{id: "u3", chainID: 123, latestBlock: 90},
	}

	nwA, _ := setupServedTipNetwork(t, ctx, fixtures)
	assert.Equal(t, int64(99), nwA.EvmHighestLatestBlockNumber(ctx),
		"without a guaranteed method, served = dominant cluster min 99 (u3=90 is an outlier)")

	nwB, _ := setupServedTipNetworkWith(t, ctx, fixtures, &common.EvmServedTipConfig{
		Quorum:            common.ServedTipQuorumMajority,
		GuaranteedMethods: []string{"trace_block"},
	})
	assert.Equal(t, int64(90), nwB.EvmHighestLatestBlockNumber(ctx),
		"trace_block supported only by u3 (90) clamps served down to 90")
}

// TestServedTip_SelectorScoped pins the use-upstream selector affecting the
// served `latest`/`finalized` decision: when a request targets a subset of
// upstreams (by tag here), the tip is computed AMONG that subset so a
// more-ahead group (e.g. base flashblocks) never defines `latest` for a
// request pinned to the other group. Selector scoping is stateless — it must
// not advance/read the network-wide monotonic counter.
func TestServedTip_SelectorScoped(t *testing.T) {
	withSel := func(ctx context.Context, sel string) context.Context {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		req.SetDirectives(&common.RequestDirectives{UseUpstream: sel})
		return context.WithValue(ctx, common.RequestContextKey, req)
	}

	fixtures := []servedTipFixture{
		{id: "fast-1", chainID: 123, latestBlock: 2000, tags: []string{"family:fast"}},
		{id: "fast-2", chainID: 123, latestBlock: 2000, tags: []string{"family:fast"}},
		{id: "slow-1", chainID: 123, latestBlock: 1000, tags: []string{"family:slow"}},
		{id: "slow-2", chainID: 123, latestBlock: 1000, tags: []string{"family:slow"}},
	}

	t.Run("MaxMode_Default", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		util.SetupMocksForEvmStatePoller()
		defer util.ResetGock()
		// Empty ServedTip config => default MAX mode.
		nw, _ := setupServedTipNetworkWith(t, ctx, fixtures, &common.EvmServedTipConfig{})

		assert.Equal(t, int64(2000), nw.EvmHighestLatestBlockNumber(ctx),
			"no selector: network-wide MAX across both families")
		assert.Equal(t, int64(1000), nw.EvmHighestLatestBlockNumber(withSel(ctx, "family:slow")),
			"family:slow: MAX within the slow family only, NOT the fast family's 2000")
		assert.Equal(t, int64(2000), nw.EvmHighestLatestBlockNumber(withSel(ctx, "family:fast")),
			"family:fast: MAX within the fast family")
		// An id-based selector must work too (use-upstream was always id-aware).
		assert.Equal(t, int64(1000), nw.EvmHighestLatestBlockNumber(withSel(ctx, "slow-*")),
			"id glob selector also scopes the tip")
	})

	t.Run("MajorityMode", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		util.SetupMocksForEvmStatePoller()
		defer util.ResetGock()
		// Majority served tip enabled for latest+finalized.
		nw, _ := setupServedTipNetwork(t, ctx, fixtures)

		// No selector: 2 fast(2000) vs 2 slow(1000) — only half the upstreams
		// have 2000, so the strict majority (3 of 4) head is 1000. Deliberate
		// semantics: never advertise a block the routed-to upstream may lack.
		// (The old cluster tie-break advertised 2000 here.)
		assert.Equal(t, int64(1000), nw.EvmHighestLatestBlockNumber(ctx),
			"no selector: strict-majority head across both families")
		// family:slow: majority over the slow subset only.
		assert.Equal(t, int64(1000), nw.EvmHighestLatestBlockNumber(withSel(ctx, "family:slow")),
			"family:slow: majority within the slow family")
		assert.Equal(t, int64(2000), nw.EvmHighestLatestBlockNumber(withSel(ctx, "family:fast")),
			"family:fast: majority within the fast family — the fast pair both have 2000")
	})

	// Group selectors (id PATTERNS or tags) that carve out a real sub-group
	// (>=2 upstreams and < all) each materialize ONE telemetry lane (gauge
	// label + watchdog anchor), keyed by the matched upstream SET — equivalent
	// selectors dedup, and garbage selectors can never inflate state (DDoS-safe).
	t.Run("GroupSelectors_MaterializeDedupAndBound", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		util.SetupMocksForEvmStatePoller()
		defer util.ResetGock()
		nw, _ := setupServedTipNetwork(t, ctx, fixtures) // cluster mode; 2 fast(2000) + 2 slow(1000)

		// A pattern and its negation form the two groups — each tracked among
		// its own upstreams (this is the flashblocks* / !flashblocks* shape).
		assert.Equal(t, int64(2000), nw.EvmHighestLatestBlockNumber(withSel(ctx, "fast*")),
			"fast* -> the fast group's own tip")
		assert.Equal(t, int64(1000), nw.EvmHighestLatestBlockNumber(withSel(ctx, "!fast*")),
			"!fast* -> the complement (slow) group's own tip")
		assert.Equal(t, int32(2), nw.servedTipPartitionCount.Load(),
			"a pattern and its negation create exactly two lanes")

		// Each partition is named by common.LaneName of its matched id set:
		// {fast-1,fast-2} -> "fast", {slow-1,slow-2} -> "slow". A single vendor
		// prefix is never a lane (the partition needs >=2 upstreams).
		lanes := map[string]bool{}
		nw.servedTipPartitions.Range(func(_, v any) bool {
			lanes[v.(*servedTipPartition).lane] = true
			return true
		})
		assert.True(t, lanes["fast"] && lanes["slow"],
			"lanes named via LaneName (token shared by all ids); got %v", lanes)

		// Selectors that resolve to the SAME upstream set dedup into the SAME
		// partition (keyed by matched set, not text): fast*/fast-*/family:fast
		// all = {fast-1,fast-2}; slow*/!fast*/family:slow all = {slow-1,slow-2}.
		for _, sel := range []string{"fast-*", "family:fast", "slow*", "family:slow", "!fast*"} {
			nw.EvmHighestLatestBlockNumber(withSel(ctx, sel))
		}
		assert.Equal(t, int32(2), nw.servedTipPartitionCount.Load(),
			"equivalent selectors (same matched set) must dedup, not create new lanes")

		// Non-group selectors NEVER materialize: match-all, single-node,
		// no-match, or a boolean expression.
		for _, sel := range []string{"*", "fast-1", "slow-1", "ghost*", "does-not-exist", "(fast*|slow*)"} {
			nw.EvmHighestLatestBlockNumber(withSel(ctx, sel))
		}
		assert.Equal(t, int32(2), nw.servedTipPartitionCount.Load(),
			"match-all / single-node / no-match / boolean selectors must not create state (DDoS-safe)")
	})
}

// The stuck-tip watchdog: the anchor stamps when the SERVED VALUE is seen to
// change. The first observed value at boot deliberately does not stamp (there
// is nothing to compare against); once the tip moves, the advance-age gauge
// becomes live.
func TestServedTip_AnchorStampsOnServedValueChange(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100},
		{id: "u2", chainID: 123, latestBlock: 100},
		{id: "u3", chainID: 123, latestBlock: 99},
	})

	require.Equal(t, int64(100), network.EvmHighestLatestBlockNumber(ctx))
	require.Negative(t, network.servedLatestAnchor.age(),
		"first observation alone must not stamp the watchdog")

	for _, u := range ups {
		u.EvmStatePoller().SuggestLatestBlock(105)
	}
	require.Equal(t, int64(105), network.EvmHighestLatestBlockNumber(ctx))
	age := network.servedLatestAnchor.age()
	require.GreaterOrEqual(t, age, time.Duration(0),
		"watchdog must stamp once the served value moves")
	require.Less(t, age, 5*time.Second)
}

// ─── Network-level scenarios the retired machinery existed for ───────────────

// Eligible-set churn (policy exclusion / cordon) may dip the advertised tip,
// but only within the live head spread. The old persistent counter held the
// value through churn; the bounded dip is the documented trade for
// wedge-immunity (an order statistic over a changed set can step down by at
// most the spread, and the per-upstream poller counters are monotonic).
func TestServedTip_EligibleSetChurn_DipBoundedBySpread(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 103},
		{id: "u2", chainID: 123, latestBlock: 102},
		{id: "u3", chainID: 123, latestBlock: 101},
		{id: "u4", chainID: 123, latestBlock: 100},
	})

	served1 := network.EvmHighestLatestBlockNumber(ctx)
	require.Equal(t, int64(101), served1, "majority of [103,102,101,100] = 3rd highest")

	// The two freshest upstreams get excluded (cordon / policy decision) —
	// the worst-case churn direction.
	network.PinUpstreamOrderForTest("u3", "u4")

	served2 := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(100), served2, "majority of the remaining [101,100]")
	assert.GreaterOrEqual(t, served2, served1-3,
		"a set-churn dip must stay within the pre-churn head spread")
}

// The network layer must add ZERO stickiness of its own: every pick reflects
// the heads as they are NOW. This is what makes deep-reorg recovery
// automatic — the per-upstream poller counters accept large rollbacks (their
// own, unchanged layer), and the network tip simply follows. The OLD design
// failed this half: its persistent counter pinned the tip AHEAD of the chain
// for any reorg smaller than its 1024-block rollback tolerance. (The poller
// suggestion path deliberately ignores lower values, so this test drives the
// "heads got lower" condition via eligibility instead.)
func TestServedTip_NoNetworkLevelStickiness(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 10_000},
		{id: "u2", chainID: 123, latestBlock: 10_000},
		{id: "u3", chainID: 123, latestBlock: 9_000},
	})
	require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx))

	// The fresh pair drops out (cordon/policy); only the 9_000 view remains.
	network.PinUpstreamOrderForTest("u3")
	assert.Equal(t, int64(9_000), network.EvmHighestLatestBlockNumber(ctx),
		"the pick must reflect current heads with no memory of the prior 10_000")

	// And no downward stickiness either: the moment the remaining head
	// advances, the pick follows it up.
	// (9_000 → 11_000 is a MAJOR jump, so it passes the poller's off-hot-path
	// chain-identity check — mocked eth_chainId matches — before it applies.)
	ups[2].EvmStatePoller().SuggestLatestBlock(11_000)
	require.Eventually(t, func() bool {
		return network.EvmHighestLatestBlockNumber(ctx) == 11_000
	}, 2*time.Second, 10*time.Millisecond,
		"the pick must follow the verified major forward jump with no stickiness")
}

// A halted chain that resumes with a burst (sequencer outage ending, L2 batch
// landing) must be served the moment a majority reports the jump. The old
// velocity gate had to be tuned (slack/buffer) to permit exactly this, and a
// mis-tuned gate freezing on the jump is the frozen-tip regression; statelessness has
// no window to outrun.
func TestServedTip_HaltedChainResume_CatchesUpImmediately(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100},
		{id: "u2", chainID: 123, latestBlock: 100},
		{id: "u3", chainID: 123, latestBlock: 99},
	})

	// Halted: repeated picks over frozen heads hold the same honest value.
	require.Equal(t, int64(100), network.EvmHighestLatestBlockNumber(ctx))
	require.Equal(t, int64(100), network.EvmHighestLatestBlockNumber(ctx))

	// Resume with a 500-block burst.
	for _, u := range ups {
		u.EvmStatePoller().SuggestLatestBlock(600)
	}
	assert.Equal(t, int64(600), network.EvmHighestLatestBlockNumber(ctx),
		"the post-halt burst must be served on the first pick that sees it")
}

// ─── Availability bounds are not head observations ──────────────────────────

// A blockAvailability.upper bound is a SERVING-RANGE declaration, not a view of
// the chain head, so it must not appear on the ballot while any live head does.
// Two upstreams pinned at `upper.exactBlock` (their pollers sitting at the live
// tip) used to outvote the live heads as soon as the eligible set shrank —
// pulling "latest" down to a block years in the past.
func TestServedTip_BoundCappedUpstreamsDoNotVote(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "live-1", chainID: 123, latestBlock: 10_000},
		{id: "live-2", chainID: 123, latestBlock: 9_998},
		{id: "capped-1", chainID: 123, latestBlock: 10_000, upperExactBlock: 5_000},
		{id: "capped-2", chainID: 123, latestBlock: 10_000, upperExactBlock: 5_000},
	})

	// Ballot without the caps: [10000, 9998] → the lower live head.
	// With them it would be [10000, 9998, 5000, 5000] → 3rd highest = 5000.
	assert.Equal(t, int64(9_998), network.EvmHighestLatestBlockNumber(ctx),
		"a bound-capped upstream declares what it SERVES, not where the head is; "+
			"it must not vote a cap onto the ballot while live heads exist")
}

// A DERIVED upper bound is still a head observation. `upper.latestBlockMinus: N`
// tracks the chain at a fixed lag — such an upstream knows exactly where the head
// is, it just declines to serve the last N blocks — so it must keep voting. The
// earlier dynamic classification ("the resolved upper bound is below the head")
// could not tell it apart from a frozen serving range and dropped all three of
// them here, handing the tip to the single unbounded upstream: a tip four of four
// upstreams could serve became one only one of them could.
func TestServedTip_LatestBlockMinusUpstreamsStillVote(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	minus := int64(5)
	network, ups := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 10_000},
		{id: "u2", chainID: 123, latestBlock: 10_000, upperLatestMinus: &minus},
		{id: "u3", chainID: 123, latestBlock: 10_000, upperLatestMinus: &minus},
		{id: "u4", chainID: 123, latestBlock: 10_000, upperLatestMinus: &minus},
	})

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(9_995), served,
		"the ballot is [10000, 9995, 9995, 9995]: a lagging-but-live bound is a head observation")

	// And the point of the majority pick holds: the advertised tip is servable
	// by a majority of the upstreams that produced it.
	canServe := 0
	for _, u := range ups {
		ok, err := u.EvmAssertBlockAvailability(ctx, "eth_call", common.AvailbilityConfidenceBlockHead, false, served)
		require.NoError(t, err)
		if ok {
			canServe++
		}
	}
	assert.Greater(t, canServe, len(ups)/2,
		"served tip %d must be servable by a majority; only %d of %d upstreams can serve it",
		served, canServe, len(ups))
}

// The guaranteed-method floor was the second door onto the capped value: its
// ballot took raw effective heads and ran AFTER the guard, so a static cap that
// the network ballot had just refused came back as a "floor" and was served.
// The floor now builds its per-method ballot with the same rule.
func TestServedTip_GuaranteedMethodFloorCannotReadmitTheCap(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const legacyCap = int64(50_000)
	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "leader", chainID: 123, latestBlock: 100_000},
		{id: "peer-a", chainID: 123, latestBlock: 99_996},
		{id: "peer-b", chainID: 123, latestBlock: 99_968},
		{id: "legacy-1", chainID: 123, latestBlock: 100_000, upperExactBlock: legacyCap},
		{id: "legacy-2", chainID: 123, latestBlock: 100_000, upperExactBlock: legacyCap},
	}, &common.EvmServedTipConfig{
		Quorum:            common.ServedTipQuorumMajority,
		GuaranteedMethods: []string{"eth_getLogs"},
	})

	require.Greater(t, network.EvmHighestLatestBlockNumber(ctx), legacyCap,
		"precondition: with the full fleet eligible the tip is a live head")

	// The failing ballot: one live upstream lag-excluded.
	network.PinUpstreamOrderForTest("peer-a", "peer-b", "legacy-1", "legacy-2")

	assert.Greater(t, network.EvmHighestLatestBlockNumber(ctx), legacyCap,
		"the guaranteed-method floor must not re-admit the serving range the ballot filter removed")
}

// THE COMPOSITION of the two doors: the guard correctly refuses a cap-only
// ballot (no live head → it holds its last corroborated pick), and the
// guaranteed-method floor — which runs AFTER the guard — then recomputes that
// same collapsed instant and drags the served value back to the cap. Each fix
// was correct alone; together they still served the cap.
//
// The live upstreams here go SYNCING rather than being un-pinned: that is the
// realistic shape (lag-excluded, cordoned, mid-restart upstreams stay
// REGISTERED), and registration is what tells the floor that a cap-only ballot
// is transient rather than the operator's configuration.
func TestServedTip_GuaranteedMethodFloor_DoesNotUndoTheGuardsHold(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	advance := pinServedTipClock(t)

	const legacyCap = int64(50_000)
	network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "leader", chainID: 123, latestBlock: 100_000},
		{id: "peer-a", chainID: 123, latestBlock: 99_996},
		{id: "peer-b", chainID: 123, latestBlock: 99_968},
		{id: "legacy-1", chainID: 123, latestBlock: 100_000, upperExactBlock: legacyCap},
		{id: "legacy-2", chainID: 123, latestBlock: 100_000, upperExactBlock: legacyCap},
	}, &common.EvmServedTipConfig{
		Quorum:            common.ServedTipQuorumMajority,
		GuaranteedMethods: []string{"eth_getLogs"},
	})

	healthy := network.EvmHighestLatestBlockNumber(ctx)
	require.Greater(t, healthy, legacyCap, "precondition: the live heads carry the ballot")

	// Every live upstream drops out of the ballot at once; only the two
	// capped ones remain servable, and they support eth_getLogs.
	for _, u := range ups[:3] {
		u.EvmStatePoller().SetSyncingState(common.EvmSyncingStateSyncing)
	}

	assert.Equal(t, healthy, network.EvmHighestLatestBlockNumber(ctx),
		"the floor must not undo the guard's hold: live upstreams serve eth_getLogs, "+
			"they are merely absent, so the cap-only floor is a transient lie")
	assert.Zero(t, network.guaranteedFloor(ctx, false),
		"and the method must contribute no floor at all while that is true")

	advance(servedTipRegressionTTL + time.Second)
	assert.Equal(t, legacyCap, network.EvmHighestLatestBlockNumber(ctx),
		"past the guard's TTL the cap is served — bounded fail-open, not a wedge")
}

// The floor's PURPOSE is unchanged by that filter: when only capped upstreams
// support a guaranteed method, they are the only upstreams that can serve it at
// all, so their cap is exactly the floor the tip must respect.
func TestServedTip_GuaranteedMethodFloor_CapsStillSetTheFloorWhenAloneOnAMethod(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "live-1", chainID: 123, latestBlock: 10_000, ignoreMethods: []string{"trace_block"}},
		{id: "live-2", chainID: 123, latestBlock: 10_000, ignoreMethods: []string{"trace_block"}},
		{id: "archive-1", chainID: 123, latestBlock: 10_000, upperExactBlock: 5_000},
		{id: "archive-2", chainID: 123, latestBlock: 10_000, upperExactBlock: 5_000},
	}, &common.EvmServedTipConfig{
		Quorum:            common.ServedTipQuorumMajority,
		GuaranteedMethods: []string{"trace_block"},
	})

	assert.Equal(t, int64(5_000), network.EvmHighestLatestBlockNumber(ctx),
		"only the capped pair serves trace_block, so their range IS the guarantee")

	// And it stays the guarantee when the live upstreams leave the ballot: no
	// live upstream is CONFIGURED to serve trace_block, so this cap-only ballot
	// is the permanent case, not a transient one.
	for _, u := range ups[:2] {
		u.EvmStatePoller().SetSyncingState(common.EvmSyncingStateSyncing)
	}
	assert.Equal(t, int64(5_000), network.EvmHighestLatestBlockNumber(ctx),
		"a permanently cap-only method must keep flooring the tip")
}

// A ballot the filter shrinks to a SINGLE live upstream is served as that
// upstream's head — deliberately. One live witness is thin evidence, but the
// alternative is advertising a serving range that stopped tracking the chain
// years ago, and the regression guard bounds how far the tip may move from what
// this process last saw corroborated.
func TestServedTip_CapFilterMayShrinkTheBallotToOneLiveUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pinServedTipClock(t)

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "live-1", chainID: 123, latestBlock: 10_000},
		{id: "capped-1", chainID: 123, latestBlock: 10_000, upperExactBlock: 5_000},
		{id: "capped-2", chainID: 123, latestBlock: 10_000, upperExactBlock: 5_000},
	})

	assert.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx),
		"one live head beats two serving ranges; the ballot is [10000], not [10000,5000,5000]")
}

// The all-capped pool is the legitimate case for a cap to be served: a network
// of only historical/frozen upstreams has no live head to prefer, and its cap
// IS the freshest block anyone can serve. Unchanged by the filter.
func TestServedTip_AllCappedPool_ServesTheCap(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "capped-1", chainID: 123, latestBlock: 10_000, upperExactBlock: 5_000},
		{id: "capped-2", chainID: 123, latestBlock: 10_000, upperExactBlock: 5_000},
		{id: "capped-3", chainID: 123, latestBlock: 10_000, upperExactBlock: 4_000},
	})

	assert.Equal(t, int64(5_000), network.EvmHighestLatestBlockNumber(ctx),
		"with no live head on the ballot the caps are the only observations there are; "+
			"the majority of [5000,5000,4000] must still be served")
}

// Every candidate filtered (all syncing here) → the tip is UNKNOWN, and unknown
// must stay unknown: eth_blockNumber falls through untouched and "latest" is
// left uninterpolated (architecture/evm: resolveBlockTagToHex and the
// eth_blockNumber pin both fail open on <= 0 — "never synthesize a block number
// from nothing"). The regression guard must not resurrect a remembered tip
// here either: with nothing to corroborate it, a held value would be exactly
// the frozen tip served from memory that this design forbids.
func TestServedTip_AllInputsFiltered_FailsOpenToZero(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 10_000},
		{id: "u2", chainID: 123, latestBlock: 10_000},
		{id: "u3", chainID: 123, latestBlock: 10_000},
	})
	require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx),
		"healthy pick first, so the guard has a corroborated value to (not) serve")

	for _, u := range ups {
		u.EvmStatePoller().SetSyncingState(common.EvmSyncingStateSyncing)
	}

	assert.Equal(t, int64(0), network.EvmHighestLatestBlockNumber(ctx),
		"no eligible observations → unknown tip (0), never a remembered one")
}

// ─── Regression guard: the tip cannot fall far below the best live head ──────

// servedTipRegressionCount reads the guard's counter for a network/axis/outcome.
func servedTipRegressionCount(n *Network, axis, outcome string) float64 {
	return promUtil.ToFloat64(telemetry.MetricNetworkServedTipRegressionTotal.
		WithLabelValues(n.projectId, n.Label(), servedTipLaneAll, axis, outcome))
}

// pinServedTipClock freezes the guard's clock for the duration of a test and
// returns a function that advances it (mirrors integrity's exhaustionClock).
func pinServedTipClock(t *testing.T) func(time.Duration) {
	t.Helper()
	var mu sync.Mutex
	now := time.Now()
	prev := servedTipClock
	servedTipClock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	t.Cleanup(func() { servedTipClock = prev })
	return func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
}

// A pick that drops more than the tolerance below the freshest live head is a
// poisoned ballot, not a chain that moved: the last corroborated pick is served
// instead. Here the two upstreams stuck at block 5 become the majority the
// moment the healthy pair leaves the eligible set — the same shape as two
// bound-capped upstreams, reached without any bounds at all (which is why the guard
// exists independently of the bounds filter).
func TestServedTip_RegressionGuard_HoldsLastCorroboratedPick(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pinServedTipClock(t)

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "live-1", chainID: 123, latestBlock: 10_000},
		{id: "live-2", chainID: 123, latestBlock: 10_000},
		{id: "live-3", chainID: 123, latestBlock: 10_000},
		{id: "stuck-1", chainID: 123, latestBlock: 5},
		{id: "stuck-2", chainID: 123, latestBlock: 5},
	})

	require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx),
		"majority of [10000,10000,10000,5,5] = 10000, corroborated by the live heads")
	before := servedTipRegressionCount(network, "latest", "held")

	// The healthy pair drops out (policy exclusion / cordon): the ballot
	// becomes [10000, 5, 5] and the majority pick collapses to 5.
	network.PinUpstreamOrderForTest("live-1", "stuck-1", "stuck-2")

	assert.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx),
		"a pick 9995 blocks below the live head is a poisoned ballot; "+
			"the last corroborated pick must be served instead")
	assert.Equal(t, before+1, servedTipRegressionCount(network, "latest", "held"),
		"the guard must count what it suppressed")
}

// The guard is a bounded pause, never a wedge: once the regression outlasts
// servedTipRegressionTTL, reality is the better explanation and the pick is
// served as computed. Without this property a clamp could hold forever and
// freeze the tip.
func TestServedTip_RegressionGuard_FailsOpenAfterTTL(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	advance := pinServedTipClock(t)

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "live-1", chainID: 123, latestBlock: 10_000},
		{id: "live-2", chainID: 123, latestBlock: 10_000},
		{id: "live-3", chainID: 123, latestBlock: 10_000},
		{id: "stuck-1", chainID: 123, latestBlock: 5},
		{id: "stuck-2", chainID: 123, latestBlock: 5},
	})
	require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx))
	network.PinUpstreamOrderForTest("live-1", "stuck-1", "stuck-2")
	require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx),
		"inside the window the guard holds")

	before := servedTipRegressionCount(network, "latest", "failed_open")
	advance(servedTipRegressionTTL + time.Second)

	assert.Equal(t, int64(5), network.EvmHighestLatestBlockNumber(ctx),
		"past the guard window the pick must be served as computed (fail open)")
	assert.Equal(t, before+1, servedTipRegressionCount(network, "latest", "failed_open"),
		"failing open is still worth counting")
}

// A genuine reorg/lag inside the tolerance is reality, not poison: it passes
// through untouched and becomes the new corroborated value (so the guard can
// never ratchet the tip upward across small legitimate steps back).
func TestServedTip_RegressionGuard_SmallReorgPassesThrough(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pinServedTipClock(t)

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "live-1", chainID: 123, latestBlock: 10_000},
		{id: "live-2", chainID: 123, latestBlock: 10_000},
		{id: "live-3", chainID: 123, latestBlock: 10_000},
		{id: "behind-1", chainID: 123, latestBlock: 9_500},
		{id: "behind-2", chainID: 123, latestBlock: 9_500},
	})
	require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx))
	before := servedTipRegressionCount(network, "latest", "held")

	// [10000, 9500, 9500] → 9500: 500 blocks back, inside the 1024 tolerance.
	network.PinUpstreamOrderForTest("live-1", "behind-1", "behind-2")

	assert.Equal(t, int64(9_500), network.EvmHighestLatestBlockNumber(ctx),
		"a regression inside the tolerance is reality and must pass through")
	assert.Equal(t, before, servedTipRegressionCount(network, "latest", "held"),
		"a tolerated regression is not a guard trip")
	assert.Equal(t, int64(9_500), network.servedLatestAnchor.lastGoodValue.Load(),
		"the tolerated pick becomes the new corroborated value")
}

// With no live head on the ballot there is nothing to measure a regression
// against, so the guard steps aside entirely — and records nothing, because a
// cap must never become the value it later holds the network to.
func TestServedTip_RegressionGuard_SkippedWithoutLiveHead(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pinServedTipClock(t)

	// Every upstream's poller sits at 10_000 while serving only up to 5_000:
	// were the raw heads a live reference, 5_000 would trip the guard.
	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "capped-1", chainID: 123, latestBlock: 10_000, upperExactBlock: 5_000},
		{id: "capped-2", chainID: 123, latestBlock: 10_000, upperExactBlock: 5_000},
		{id: "capped-3", chainID: 123, latestBlock: 10_000, upperExactBlock: 5_000},
	})
	before := servedTipRegressionCount(network, "latest", "held")

	assert.Equal(t, int64(5_000), network.EvmHighestLatestBlockNumber(ctx),
		"no live head → no reference → the guard must not interfere")
	assert.Equal(t, before, servedTipRegressionCount(network, "latest", "held"),
		"the guard cannot trip without a live head to compare against")
	assert.Zero(t, network.servedLatestAnchor.lastGoodValue.Load(),
		"an uncorroborated cap must never be remembered as a good pick")
}

// THE remaining door onto the capped value, and the reason the guard no
// longer stands down without a live head: when EVERY live upstream leaves the
// eligible set while the bound-capped ones stay, the ballot legitimately falls
// back to the caps — and a guard that requires a live reference is absent on the
// exact ballot shape that caused the outage. With no live head it measures
// against its own last corroborated pick instead, holds it UNCLAMPED for the
// TTL, and only then fails open to the cap.
func TestServedTip_RegressionGuard_HoldsWhenEveryLiveUpstreamLeaves(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	advance := pinServedTipClock(t)

	const legacyCap = int64(50_000)
	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "leader", chainID: 123, latestBlock: 100_000},
		{id: "peer-a", chainID: 123, latestBlock: 99_996},
		{id: "peer-b", chainID: 123, latestBlock: 99_968},
		{id: "legacy-1", chainID: 123, latestBlock: 100_000, upperExactBlock: legacyCap},
		{id: "legacy-2", chainID: 123, latestBlock: 100_000, upperExactBlock: legacyCap},
	}, &common.EvmServedTipConfig{Quorum: common.ServedTipQuorumMajority})

	healthy := network.EvmHighestLatestBlockNumber(ctx)
	require.Greater(t, healthy, legacyCap, "precondition: the live heads carry the ballot")
	beforeHeld := servedTipRegressionCount(network, "latest", "held")
	beforeOpen := servedTipRegressionCount(network, "latest", "failed_open")

	// All three live upstreams drop out at once (a provider outage, a policy
	// exclusion sweep): only the two capped ones remain eligible.
	network.PinUpstreamOrderForTest("legacy-1", "legacy-2")

	assert.Equal(t, healthy, network.EvmHighestLatestBlockNumber(ctx),
		"with no live head left the guard must hold its last corroborated pick, "+
			"not hand `latest` to a historical serving range")
	assert.Equal(t, beforeHeld+1, servedTipRegressionCount(network, "latest", "held"))

	advance(servedTipRegressionTTL + time.Second)
	assert.Equal(t, legacyCap, network.EvmHighestLatestBlockNumber(ctx),
		"and past the TTL it must still fail open: a guard that can hold forever is the wedge")
	assert.Equal(t, beforeOpen+1, servedTipRegressionCount(network, "latest", "failed_open"))
}

// The guard's reference is the SECOND-highest live head, so a single far-future
// upstream cannot make every honest pick look like a regression. Measured
// against the raw max it did exactly that — and because a corroborated value is
// only recorded on the healthy branch, the guard then sat permanently disarmed
// while emitting no metric and no log at all.
func TestServedTip_RegressionGuard_SingleRogueUpstreamDoesNotDisarmIt(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pinServedTipClock(t)

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "rogue", chainID: 123, latestBlock: 100_000_000},
		{id: "live-1", chainID: 123, latestBlock: 10_000},
		{id: "live-2", chainID: 123, latestBlock: 10_000},
		{id: "live-3", chainID: 123, latestBlock: 10_000},
		{id: "stuck-1", chainID: 123, latestBlock: 5},
		{id: "stuck-2", chainID: 123, latestBlock: 5},
	})

	require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx),
		"the majority pick ignores the rogue, as it always did")
	require.Equal(t, int64(10_000), network.servedLatestAnchor.lastGoodValue.Load(),
		"and the guard must be ARMED by that honest pick, not silently disabled by the rogue")

	// Armed means it still protects: lose the honest majority to a stuck pair.
	network.PinUpstreamOrderForTest("rogue", "live-1", "stuck-1", "stuck-2")
	assert.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx),
		"the armed guard holds the corroborated pick when the ballot collapses")
}

// The guard's memory must not be able to commit a FANTASY. If the eligible set
// is momentarily majority far-future — two wrong-chain upstreams outvoting one
// honest head while the rest are lag-excluded — the pick IS that fantasy, and a
// guard that remembers it holds it against the honest fleet for the whole TTL
// once they return. main, which remembers nothing, recovers on the next
// evaluation; the memory must not make us worse than that.
func TestServedTip_RegressionGuard_DoesNotCommitAFantasyPick(t *testing.T) {
	// fantasyFleet returns a network whose eligible set can be reshaped at will:
	// the syncing state is how an upstream leaves and RE-ENTERS the ballot (the
	// order-pinning helper only ever removes, and removes from the registry).
	fantasyFleet := func(t *testing.T, ctx context.Context) (*Network, func(id string, in bool), func(time.Duration)) {
		util.SetupMocksForEvmStatePoller()
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetwork(t, ctx, []servedTipFixture{
			{id: "rogue-1", chainID: 123, latestBlock: 100_000_000},
			{id: "rogue-2", chainID: 123, latestBlock: 100_000_000},
			{id: "h1", chainID: 123, latestBlock: 10_000},
			{id: "h2", chainID: 123, latestBlock: 10_000},
			{id: "h3", chainID: 123, latestBlock: 10_000},
			{id: "stuck-1", chainID: 123, latestBlock: 5},
			{id: "stuck-2", chainID: 123, latestBlock: 5},
		})
		byID := map[string]*upstream.Upstream{}
		for _, u := range ups {
			byID[u.Id()] = u
		}
		eligible := func(id string, in bool) {
			st := common.EvmSyncingStateSyncing
			if in {
				st = common.EvmSyncingStateNotSyncing
			}
			byID[id].EvmStatePoller().SetSyncingState(st)
		}
		for _, id := range []string{"rogue-1", "rogue-2", "h1", "h2", "h3", "stuck-1", "stuck-2"} {
			eligible(id, false)
		}
		return network, eligible, advance
	}

	// The armed case: a fresh memory refuses an uncorroborated leap.
	t.Run("WhileArmed", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, eligible, advance := fantasyFleet(t, ctx)

		for _, id := range []string{"h1", "h2", "h3"} {
			eligible(id, true)
		}
		require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx))
		require.Equal(t, int64(10_000), network.servedLatestAnchor.lastGoodValue.Load())

		// Both rogues return, two honest upstreams drop out: the ballot is
		// [100M, 100M, 10000] and the majority pick IS the fantasy.
		eligible("rogue-1", true)
		eligible("rogue-2", true)
		eligible("h2", false)
		eligible("h3", false)
		advance(time.Second)
		require.Equal(t, int64(100_000_000), network.EvmHighestLatestBlockNumber(ctx),
			"precondition: the majority of this eligible set really is far-future")
		assert.Equal(t, int64(10_000), network.servedLatestAnchor.lastGoodValue.Load(),
			"an uncorroborated leap must never become the value the guard holds the network to")

		eligible("h2", true)
		eligible("h3", true)
		eligible("rogue-2", false)
		advance(time.Second)
		assert.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx),
			"recovery must be immediate, not after the guard's TTL expires")
	})

	// THE UNARMED CASE, and the one that mattered: every fail-open DISARMS, and
	// an empty memory used to arm at whatever pick appeared next. The window
	// right after a fail-open is exactly when a rogue majority is most likely,
	// so that is precisely where the continuity check is needed.
	t.Run("AfterFailOpenDisarm", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, eligible, advance := fantasyFleet(t, ctx)
		anchor := &network.servedLatestAnchor

		// 1. Armed at the honest head.
		for _, id := range []string{"h1", "h2", "h3"} {
			eligible(id, true)
		}
		require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx))
		require.Equal(t, int64(10_000), anchor.lastGoodValue.Load())

		// 2. A poisoned ballot is held.
		eligible("h2", false)
		eligible("h3", false)
		eligible("stuck-1", true)
		eligible("stuck-2", true)
		require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx),
			"precondition: the guard is holding the poisoned ballot")

		// 3. The hold outlasts its window: fail open, and disarm.
		advance(servedTipRegressionTTL + time.Second)
		require.Equal(t, int64(5), network.EvmHighestLatestBlockNumber(ctx))
		require.Zero(t, anchor.lastGoodValue.Load(), "precondition: failing open disarms the memory")

		// 4. One majority-rogue evaluation. It must SERVE the rogue value — no
		//    arming rule may ever constrain what is served — and must commit
		//    nothing, because 100M has no continuity with anything this process
		//    has served.
		eligible("stuck-1", false)
		eligible("stuck-2", false)
		eligible("rogue-1", true)
		eligible("rogue-2", true)
		advance(time.Second)
		assert.Equal(t, int64(100_000_000), network.EvmHighestLatestBlockNumber(ctx),
			"serving is never constrained by an arming rule: the pick is the pick")
		assert.Zero(t, anchor.lastGoodValue.Load(),
			"but an unarmed guard must not commit a value discontinuous with what it just served")

		// 5. The honest fleet returns: immediate recovery, and the memory arms
		//    at the honest head.
		eligible("h2", true)
		eligible("h3", true)
		eligible("rogue-1", false)
		eligible("rogue-2", false)
		advance(time.Second)
		assert.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx),
			"recovery on the very next evaluation, exactly as a guardless process behaves")
		assert.Equal(t, int64(10_000), anchor.lastGoodValue.Load(),
			"and the memory re-arms at the honest head")
	})

	// A true first evaluation has no history to be continuous with, so it arms
	// unconditionally: one evaluation of exposure, which is what a process with
	// no guard at all has permanently. The value must still not survive contact
	// with the honest fleet.
	t.Run("AtColdBoot", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network, eligible, advance := fantasyFleet(t, ctx)
		anchor := &network.servedLatestAnchor

		// The very first evaluation this process ever makes is majority-rogue.
		eligible("rogue-1", true)
		eligible("rogue-2", true)
		eligible("h1", true)
		require.Zero(t, anchor.seenValue.Load(), "precondition: nothing has ever been served")
		require.Equal(t, int64(100_000_000), network.EvmHighestLatestBlockNumber(ctx))
		require.Equal(t, int64(100_000_000), anchor.lastGoodValue.Load(),
			"with no history at all there is nothing to be continuous with")

		// The honest fleet arrives.
		eligible("h2", true)
		eligible("h3", true)
		eligible("rogue-1", false)
		eligible("rogue-2", false)
		advance(time.Second)
		assert.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx),
			"a boot-time fantasy must not survive the first honest evaluation")
	})
}

// A hold may only ever RAISE the served value toward the corroborated past. A
// memory that is stale-LOW (the live corroborated head is what tripped the
// tolerance, not the memory) must never drag the tip below the pick the ballot
// produced — that is the harm the guard exists to prevent, inflicted by the
// guard itself. Driven through guardServedTipRegression directly, because the
// shape needs a memory the fleet has long since left behind.
func TestServedTip_RegressionGuard_AHoldNeverLowersTheTip(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pinServedTipClock(t)

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 10_000},
		{id: "u2", chainID: 123, latestBlock: 10_000},
	})

	var anchor servedTipAnchor
	anchor.lastGoodValue.Store(100)
	at := servedTipClock()
	anchor.lastGoodAt.Store(&at)

	// The ballot's own corroborated head is 10_000, so a pick of 5_000 trips the
	// guard — but the memory (100) is far below the pick.
	served := network.guardServedTipRegression("latest", "", &anchor, 5_000,
		servedTipReference{Corroborated: 10_000, Max: 10_000})

	assert.GreaterOrEqual(t, served, int64(5_000),
		"a hold must never serve a block LOWER than the pick it was suppressing")
	assert.Equal(t, int64(5_000), served, "with nothing better to offer, the guard is transparent")
}

// The guard runs several times per request, so its steady state must not
// allocate. The only heap allocation on the healthy path is the time.Time the
// atomic pointer owns, and the once-a-second throttle keeps it off every other
// evaluation.
func TestServedTip_RegressionGuard_HealthyPathDoesNotAllocate(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	advance := pinServedTipClock(t)

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 10_000},
		{id: "u2", chainID: 123, latestBlock: 10_000},
	})

	var anchor servedTipAnchor
	ref := servedTipReference{Corroborated: 10_000, Max: 10_000}
	require.Equal(t, int64(10_000), network.guardServedTipRegression("latest", "", &anchor, 10_000, ref),
		"arm the guard first; that evaluation is the one allowed to allocate")

	allocs := testing.AllocsPerRun(200, func() {
		network.guardServedTipRegression("latest", "", &anchor, 10_000, ref)
	})
	assert.Zero(t, allocs, "the throttled healthy path must not allocate at all")

	// And the throttle is a throttle, not a disable: past a second the clock is
	// refreshed again.
	advance(2 * time.Second)
	before := anchor.lastGoodAt.Load()
	network.guardServedTipRegression("latest", "", &anchor, 10_000, ref)
	assert.NotEqual(t, before, anchor.lastGoodAt.Load(), "the TTL clock still advances")
}

// The off switch.
func TestServedTip_RegressionGuard_DisabledByNegativeOne(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pinServedTipClock(t)

	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "live-1", chainID: 123, latestBlock: 10_000},
		{id: "live-2", chainID: 123, latestBlock: 10_000},
		{id: "live-3", chainID: 123, latestBlock: 10_000},
		{id: "stuck-1", chainID: 123, latestBlock: 5},
		{id: "stuck-2", chainID: 123, latestBlock: 5},
	}, &common.EvmServedTipConfig{Quorum: common.ServedTipQuorumMajority, MaxRegressionBlocks: -1})

	require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx))
	before := servedTipRegressionCount(network, "latest", "held")

	network.PinUpstreamOrderForTest("live-1", "stuck-1", "stuck-2")
	assert.Equal(t, int64(5), network.EvmHighestLatestBlockNumber(ctx),
		"maxRegressionBlocks: -1 means the operator gets the raw pick, poisoned or not")
	assert.Equal(t, before, servedTipRegressionCount(network, "latest", "held"))
	assert.Zero(t, network.servedLatestAnchor.lastGoodValue.Load(),
		"a disabled guard records nothing either")
}

// The lag gauge is a lag gauge: while the guard serves ABOVE the corroborated
// freshest view it must read 0, not a negative number no dashboard can plot.
func TestServedTip_LagGaugeNeverReadsNegative(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pinServedTipClock(t)

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "live-1", chainID: 123, latestBlock: 10_000},
		{id: "live-2", chainID: 123, latestBlock: 10_000},
		{id: "live-3", chainID: 123, latestBlock: 10_000},
		{id: "stuck-1", chainID: 123, latestBlock: 5},
		{id: "stuck-2", chainID: 123, latestBlock: 5},
	})
	require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx))

	network.PinUpstreamOrderForTest("live-1", "stuck-1", "stuck-2")
	require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx),
		"precondition: the guard is holding above the ballot's freshest corroborated view")

	lag := promUtil.ToFloat64(telemetry.MetricNetworkServedTipLagBlocks.
		WithLabelValues(network.projectId, network.Label(), servedTipLaneAll, "latest"))
	assert.GreaterOrEqual(t, lag, float64(0), "the deliberate-lag gauge must never read negative")
}

// suggestServedTipHead moves an upstream's poller head up to `head` in steps no
// larger than the poller's major-jump threshold. A single larger suggestion is
// deliberately deferred to an asynchronous chain-identity check
// (verifyThenSuggestLatestBlock), which these deterministic scenarios must not
// race against.
func suggestServedTipHead(u *upstream.Upstream, head int64) {
	for {
		current := u.EvmStatePoller().LatestBlock()
		if current >= head {
			return
		}
		next := head
		if head-current > common.DefaultToleratedBlockHeadRollback {
			next = current + common.DefaultToleratedBlockHeadRollback
		}
		u.EvmStatePoller().SuggestLatestBlock(next)
		if u.EvmStatePoller().LatestBlock() < next {
			return // the poller refused it; let the assertion report the state
		}
	}
}

// ─── Stalled heads are benched ───────────────────────────────────────────────
//
// The erpc#1253 shape end to end, on the default config, through
// real pollers, the real health tracker and the real Network: two upstreams,
// one stops advancing. With quorum 2 the stalled one would hold
// "latest" (and eth_call interpolation and eth_blockNumber) at its height for
// the whole outage, and as the lag reference it would read lag 0 so routing
// never drops it. The tracker benches it once it has sat below a peer that kept
// moving for its stall window.
func TestServedTip_StalledHeadIsBenched(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// stallAfter mirrors the tracker's stall window: three poll periods, never
	// under 30s. A poll period is the longer of the poll interval and the
	// debounce; an unset debounce follows the network block time, which stays 0
	// in subtests that report no block timestamps.
	stallAfter := func(u *upstream.Upstream, blockTime time.Duration) time.Duration {
		cfg := u.Config().Evm
		debounce := blockTime
		if d := cfg.StatePollerDebounce.Duration(); d > 0 {
			debounce = d
		}
		return max(30*time.Second, 3*max(cfg.StatePollerInterval.Duration(), debounce))
	}
	// report mirrors one production poll: the tracker gets the block and its
	// timestamp first, then the poller's counter (whose OnValue reports ts 0
	// and changes nothing). The counter moves one block at a time so no step
	// is a major head move deferred to an asynchronous chain-identity check.
	report := func(tr *health.Tracker, u *upstream.Upstream, head, ts int64) {
		tr.SetLatestBlockNumber(u, head, ts)
		for b := u.EvmStatePoller().LatestBlock() + 1; b <= head; b++ {
			u.EvmStatePoller().SuggestLatestBlock(b)
		}
	}
	lagOf := func(n *Network, u *upstream.Upstream) int64 {
		return n.metricsTracker.GetUpstreamMethodMetrics(u, "*", common.DataFinalityStateAll).BlockHeadLag.Load()
	}
	interpolatedLatest := func(n *Network) int64 {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x"},"latest"]}`))
		req.SetNetwork(n)
		jrq, err := req.JsonRpcRequest()
		require.NoError(t, err)
		evm.NormalizeHttpJsonRpc(ctx, req, jrq)
		blk, ok := jrq.Params[1].(string)
		require.True(t, ok, "latest must be interpolated to a hex block, got %v", jrq.Params[1])
		v, err := common.HexToInt64(blk)
		require.NoError(t, err)
		return v
	}
	blockNumber := func(n *Network) int64 {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		req.SetNetwork(n)
		req.SetDirectives(&common.RequestDirectives{EnforceHighestBlock: true})
		resp, err := n.Forward(ctx, req)
		require.NoError(t, err)
		require.Nil(t, resp.Upstream(), "eth_blockNumber must be answered from the served head without dispatching")
		_, bn, err := evm.ExtractBlockReferenceFromResponse(ctx, resp)
		require.NoError(t, err)
		return bn
	}
	// served asserts every reader of "latest" agrees on `want`.
	served := func(t *testing.T, n *Network, want int64, msg string) {
		t.Helper()
		require.Equal(t, want, n.EvmHighestLatestBlockNumber(ctx), "latest: %s", msg)
		require.Equal(t, want, interpolatedLatest(n), "eth_call latest interpolation: %s", msg)
		require.Equal(t, want, blockNumber(n), "eth_blockNumber: %s", msg)
	}

	t.Run("stall, bench, replay, rejoin", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "live", chainID: 123, latestBlock: 1_000},
			{id: "stalled", chainID: 123, latestBlock: 1_000},
		}, nil)
		live, stalled := ups[0], ups[1]
		tr := network.metricsTracker
		finalizedOf := func() int64 { return network.EvmHighestFinalizedBlockNumber(ctx) }

		// Both report finalized heads; stalled's stays frozen with its head.
		const stalledFinalized = int64(990)
		setServedTipFinalized(t, live, stalledFinalized)
		setServedTipFinalized(t, stalled, stalledFinalized)

		// Healthy: both move together.
		head := int64(1_000)
		for range 5 {
			advance(2 * time.Second)
			head += 2
			live.EvmStatePoller().SuggestLatestBlock(head)
			stalled.EvmStatePoller().SuggestLatestBlock(head)
		}
		served(t, network, head, "a healthy pair")
		stalledAt := head

		// stalled stops advancing; live keeps moving, finalized included.
		// Inside the stall window nothing changes: quorum 2 still serves the
		// lower heads on both axes.
		window := stallAfter(stalled, 0)
		for elapsed := time.Duration(0); elapsed+2*time.Second < window; elapsed += 2 * time.Second {
			advance(2 * time.Second)
			head += 2
			live.EvmStatePoller().SuggestLatestBlock(head)
			setServedTipFinalized(t, live, head-10)
		}
		require.False(t, tr.IsHeadBenched(stalled), "a peer below for less than its stall window is not benched")
		served(t, network, stalledAt, "inside the stall window")
		require.Equal(t, stalledFinalized, finalizedOf(), "finalized: quorum 2 holds the stalled one's finalized inside the stall window")
		require.Zero(t, lagOf(network, stalled), "inside the stall window the lower head is still the lag reference")

		// Past the stall window, the next head live reports benches stalled.
		advance(4 * time.Second)
		head += 2
		live.EvmStatePoller().SuggestLatestBlock(head)
		setServedTipFinalized(t, live, head-10)
		require.True(t, tr.IsHeadBenched(stalled), "a head below a moving peer for its stall window is benched")
		require.False(t, tr.IsHeadBenched(live))
		served(t, network, head, "a benched head leaves the ballot")
		require.Equal(t, head-10, finalizedOf(), "finalized: a benched head leaves the finalized ballot too")
		require.Equal(t, head-stalledAt, lagOf(network, stalled),
			"the benched head is measured against the live one, so blockNumberLagAbove can route around it")
		require.Zero(t, lagOf(network, live))

		// stalled comes back and replays: moving, but behind. It stays benched
		// and "latest" never drops back to it.
		last := head
		for replay := stalledAt; replay < head-10; replay += 20 {
			advance(time.Second)
			head++
			live.EvmStatePoller().SuggestLatestBlock(head)
			stalled.EvmStatePoller().SuggestLatestBlock(replay)
			require.True(t, tr.IsHeadBenched(stalled), "a replaying head stays benched until it catches up (at %d, live %d)", replay, head)
			got := network.EvmHighestLatestBlockNumber(ctx)
			require.GreaterOrEqual(t, got, last, "latest must never regress while a benched head replays")
			last = got
		}
		served(t, network, head, "while the benched head replays")

		// stalled catches up on both axes: it rejoins and its lag is 0 again.
		advance(time.Second)
		stalled.EvmStatePoller().SuggestLatestBlock(head)
		require.False(t, tr.IsHeadBenched(stalled), "a benched head that caught up rejoins")
		require.Zero(t, lagOf(network, stalled))
		served(t, network, head, "after the rejoin")
		setServedTipFinalized(t, live, head-10)
		setServedTipFinalized(t, stalled, head-10)
		require.Equal(t, head-10, finalizedOf(), "finalized after the rejoin")
	})

	t.Run("chain halt benches nobody", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "a", chainID: 123, latestBlock: 2_000},
			{id: "b", chainID: 123, latestBlock: 2_000},
		}, nil)
		tr := network.metricsTracker
		// Every poll returns the same head, with the same old block timestamp,
		// for ten minutes.
		haltedTs := servedTipClock().Add(-10 * time.Minute).Unix()
		for range 60 {
			advance(10 * time.Second)
			for _, u := range ups {
				report(tr, u, 2_000, haltedTs)
			}
		}
		for _, u := range ups {
			require.False(t, tr.IsHeadBenched(u), "%s: nobody moved, so nobody is below a moving peer", u.Id())
			require.Zero(t, lagOf(network, u))
		}
		served(t, network, 2_000, "a halted chain")
	})

	t.Run("static far-future upstream is never served, benches nobody, and never blocks a rejoin", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "honest-a", chainID: 123, latestBlock: 3_000},
			{id: "honest-b", chainID: 123, latestBlock: 3_000},
			{id: "rogue", chainID: 123, latestBlock: 3_000},
		}, nil)
		a, b, rogue := ups[0], ups[1], ups[2]
		tr := network.metricsTracker
		suggestServedTipHead(rogue, 103_000)
		require.Equal(t, int64(103_000), rogue.EvmStatePoller().LatestBlock())

		head := int64(3_000)
		for range 60 {
			advance(10 * time.Second)
			head++
			a.EvmStatePoller().SuggestLatestBlock(head)
			b.EvmStatePoller().SuggestLatestBlock(head)
			tr.SetLatestBlockNumber(rogue, 103_000, 0)
			require.False(t, tr.IsHeadBenched(a), "a static head above never benches the moving ones")
			require.False(t, tr.IsHeadBenched(b), "a static head above never benches the moving ones")
		}
		require.False(t, tr.IsHeadBenched(rogue))
		require.Zero(t, lagOf(network, a), "the rogue is not the lag reference")
		served(t, network, head, "a lone far-future head is never served")

		// b stalls past its window and is benched; the rogue stays a voter.
		for elapsed := time.Duration(0); elapsed <= stallAfter(b, 0); elapsed += 10 * time.Second {
			advance(10 * time.Second)
			head++
			a.EvmStatePoller().SuggestLatestBlock(head)
		}
		require.True(t, tr.IsHeadBenched(b))
		served(t, network, head, "quorum 2 over {a, rogue} still serves the honest head")

		// b catches up with a. The rogue's head is out of anyone's reach, so it
		// must not be the rejoin bar.
		advance(time.Second)
		b.EvmStatePoller().SuggestLatestBlock(head)
		require.False(t, tr.IsHeadBenched(b), "a caught-up head rejoins even with a far-future voter above it")
		require.Zero(t, lagOf(network, b))
		served(t, network, head, "after the rejoin")
	})

	t.Run("moving far-future upstream never benches a stalled honest one", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "honest", chainID: 123, latestBlock: 4_000},
			{id: "rogue", chainID: 123, latestBlock: 4_000},
		}, nil)
		honest, rogue := ups[0], ups[1]
		tr := network.metricsTracker
		rogueHead := int64(104_000)
		suggestServedTipHead(rogue, rogueHead)

		// honest's poller dies for well past its stall window while the
		// wrong-chain head keeps advancing: a lead no chain produced in that
		// time is another chain, not a stall.
		for range 30 {
			advance(10 * time.Second)
			rogueHead += 5
			tr.SetLatestBlockNumber(rogue, rogueHead, 0)
		}
		require.False(t, tr.IsHeadBenched(honest), "an implausible lead must not bench the honest head")
		require.Zero(t, lagOf(network, honest), "the rogue is not the lag reference")
		served(t, network, 4_000, "a lone moving far-future head is never served")
	})

	t.Run("a process that starts mid-stall benches the stalled head", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "live", chainID: 123, latestBlock: 47_880_000},
			{id: "stalled-a", chainID: 123, latestBlock: 1_800_000},
			{id: "stalled-b", chainID: 123, latestBlock: 1_800_000},
		}, nil)
		live, stalls := ups[0], ups[1:]
		tr := network.metricsTracker
		const stalledAt = int64(1_800_000)
		// The stalled heads' block was produced two years before this process.
		// Each has two non-benched peers and only live is younger, so the time
		// rule never fires: only the height rule can bench them, and with the
		// network block time unknown (the corroborated head is the stalled
		// one) that rule needs live's own block time.
		stalledTs := servedTipClock().Add(-2 * 365 * 24 * time.Hour).Unix()
		head := int64(47_880_000)
		poll := func(d time.Duration) {
			advance(d)
			for _, u := range stalls {
				report(tr, u, stalledAt, stalledTs)
			}
			head += int64(d / (2 * time.Second))
			report(tr, live, head, servedTipClock().Unix())
		}
		for _, u := range stalls {
			report(tr, u, stalledAt, stalledTs)
		}
		served(t, network, stalledAt, "at boot quorum 2 serves the stalled head")

		// Three timestamped head changes: live's block time is not known yet.
		for range 3 {
			poll(2 * time.Second)
		}
		for _, u := range ups {
			require.False(t, tr.IsHeadBenched(u), "%s: nobody is benched before live's block time is known", u.Id())
		}
		served(t, network, stalledAt, "before live's block time is known")

		// The fourth settles it; the stall window runs from there.
		poll(2 * time.Second)
		window := stallAfter(stalls[0], 0)
		for elapsed := time.Duration(0); elapsed+2*time.Second < window; elapsed += 2 * time.Second {
			poll(2 * time.Second)
		}
		for _, u := range stalls {
			require.False(t, tr.IsHeadBenched(u), "%s: not benched inside its stall window", u.Id())
		}
		served(t, network, stalledAt, "inside the stall window")

		poll(4 * time.Second)
		for _, u := range stalls {
			require.True(t, tr.IsHeadBenched(u), "%s: a head frozen since before the process started is benched after its stall window", u.Id())
		}
		require.False(t, tr.IsHeadBenched(live))
		served(t, network, head, "the stalled heads leave the ballot")
		require.Equal(t, head-stalledAt, lagOf(network, stalls[0]), "the benched head is measured against the live one")
		require.Zero(t, lagOf(network, live))
	})

	t.Run("a node replaying far behind the tip is benched while it moves", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "live", chainID: 123, latestBlock: 100_000},
			{id: "replaying", chainID: 123, latestBlock: 99_400},
		}, nil)
		live, replaying := ups[0], ups[1]
		tr := network.metricsTracker
		// A 2s chain. replaying restarted from a snapshot 600 blocks (20 min)
		// old and replays 4 blocks per 2s, so it moves on every poll. Once the
		// block time is known, every other move of each upstream arrives
		// without its block timestamp, as a peer pod's poll does through
		// shared state.
		start := servedTipClock().Unix()
		tsOf := func(n int64) int64 { return start + (n-100_000)*2 }
		head, behind := int64(100_000), int64(99_400)
		polls := 0
		poll := func(d time.Duration) {
			advance(d)
			polls++
			ts := func(n int64) int64 {
				if polls > 4 && polls%2 == 0 {
					return 0
				}
				return tsOf(n)
			}
			behind = min(behind+int64(d/(500*time.Millisecond)), head)
			report(tr, replaying, behind, ts(behind))
			head += int64(d / (2 * time.Second))
			report(tr, live, head, ts(head))
		}
		report(tr, replaying, behind, tsOf(behind))
		report(tr, live, head, tsOf(head))
		served(t, network, behind, "at boot quorum 2 serves the replaying head")

		window := stallAfter(replaying, 2*time.Second)
		for elapsed := time.Duration(0); elapsed+2*time.Second < window; elapsed += 2 * time.Second {
			poll(2 * time.Second)
		}
		require.False(t, tr.IsHeadBenched(replaying), "not benched inside its stall window")
		served(t, network, behind, "inside the stall window quorum 2 serves the replaying head")

		// Its own moves never reset the window: its head block stays a stall
		// window older than live's.
		poll(4 * time.Second)
		require.True(t, tr.IsHeadBenched(replaying), "a head replaying far behind a live peer for its stall window is benched")
		require.False(t, tr.IsHeadBenched(live))
		served(t, network, head, "the replaying head leaves the ballot")
		require.Equal(t, head-behind, lagOf(network, replaying), "a benched head that moves lags by its real distance")
		require.Greater(t, lagOf(network, replaying), int64(16), "past the default blockNumberLagAbove, so routing leaves it")

		// It gains 3 blocks a poll. It rejoins on the report that reaches live's
		// head, not before, and "latest" never drops back to it meanwhile.
		last := head
		for polls := 0; tr.IsHeadBenched(replaying); polls++ {
			require.Less(t, polls, 400, "a replaying head rejoins once it catches up")
			poll(2 * time.Second)
			got := network.EvmHighestLatestBlockNumber(ctx)
			require.GreaterOrEqual(t, got, last, "latest never regresses while it replays")
			last = got
		}
		require.Equal(t, head-1, behind, "it rejoins on reaching live's head; live then moved one on")

		// Back at the tip it trails live by one block, far less than its stall
		// window of chain time: it votes again and is never benched.
		for range 3 * int(window/(2*time.Second)) {
			poll(2 * time.Second)
			require.False(t, tr.IsHeadBenched(replaying), "a node one block behind is never benched (at %d, live %d)", behind, head)
		}
		require.Equal(t, head-1, behind)
		require.Zero(t, lagOf(network, replaying), "the lower voter is the lag reference again")
		served(t, network, behind, "quorum 2 serves the lower of two heads at the tip")
	})

	t.Run("a staleness verdict ends when the node catches up, so a plausible rogue cannot keep it", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "live", chainID: 123, latestBlock: 50_000},
			{id: "trailing", chainID: 123, latestBlock: 49_940},
			{id: "rogue", chainID: 123, latestBlock: 50_000},
		}, nil)
		live, trailing, rogue := ups[0], ups[1], ups[2]
		tr := network.metricsTracker
		// A 2s chain. trailing keeps pace 60 blocks (120s) back, more than its
		// 90s stall window of chain time, so its own timestamps prove it stale.
		start := servedTipClock().Unix()
		tsOf := func(n int64) int64 { return start + (n-50_000)*2 }
		head := int64(50_000)
		for range 5 {
			advance(2 * time.Second)
			head++
			report(tr, trailing, head-60, tsOf(head-60))
			report(tr, live, head, tsOf(head))
			report(tr, rogue, head, tsOf(head))
		}
		require.False(t, tr.IsHeadBenched(trailing), "not benched inside its stall window")

		// It catches up through a peer pod's poll (no timestamp). Then the
		// rogue claims a head 60 blocks above the tip and keeps moving: a lead
		// a stall window of blocks ahead, but no proof trailing is stale.
		advance(2 * time.Second)
		head++
		report(tr, trailing, head, 0)
		report(tr, live, head, tsOf(head))
		report(tr, rogue, head, tsOf(head))
		for range 3 * int(stallAfter(trailing, 2*time.Second)/(2*time.Second)) {
			advance(2 * time.Second)
			head++
			report(tr, trailing, head, 0)
			report(tr, live, head, tsOf(head))
			report(tr, rogue, head+60, tsOf(head+60))
			require.False(t, tr.IsHeadBenched(trailing), "a node at the tip is never benched by a verdict it disproved (head %d)", head)
		}
		require.Zero(t, lagOf(network, trailing))
		served(t, network, head, "the rogue's lone head above the tip is never served")
	})

	t.Run("a benched replay that reaches a voter as stale as itself stays benched", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "live", chainID: 123, latestBlock: 100_000},
			{id: "restored", chainID: 123, latestBlock: 99_200},
			{id: "peer-replay", chainID: 123, latestBlock: 99_300},
		}, nil)
		live, restored, peerReplay := ups[0], ups[1], ups[2]
		tr := network.metricsTracker
		// A 2s chain. Two nodes replay from old snapshots. restored gains 4
		// blocks per 2s with its own block timestamps, so it is proven stale
		// and benched. peer-replay gains 3, and each of its moves reaches this
		// process without a timestamp (a peer pod's poll), so nothing proves
		// it stale: it stays a voter and sets the bar restored must reach.
		start := servedTipClock().Unix()
		tsOf := func(n int64) int64 { return start + (n-100_000)*2 }
		head, r, p := int64(100_000), int64(99_200), int64(99_300)
		window := stallAfter(restored, 2*time.Second)
		last, wasBenched := int64(0), false
		for polls := 0; !wasBenched || tr.IsHeadBenched(restored); polls++ {
			require.Less(t, polls, 1000, "restored rejoins once it replays to the tip")
			advance(2 * time.Second)
			r = min(r+4, head)
			report(tr, restored, r, tsOf(r))
			p = min(p+3, head)
			report(tr, peerReplay, p, 0)
			head++
			report(tr, live, head, tsOf(head))
			wasBenched = wasBenched || tr.IsHeadBenched(restored)
			if wasBenched && (head-r)*2 >= 2*int64(window/time.Second) {
				require.True(t, tr.IsHeadBenched(restored), "a benched head two stall windows behind the tip stays benched, even once it reaches a stale voter (restored %d, peer-replay %d, live %d)", r, p, head)
			}
			got := network.EvmHighestLatestBlockNumber(ctx)
			require.GreaterOrEqual(t, got, last, "latest never regresses (restored %d, peer-replay %d, live %d)", r, p, head)
			last = got
		}
		require.Less(t, head-r, int64(window/(2*time.Second))+2, "it rejoins once within a stall window of chain time of the tip")
		served(t, network, r, "after the rejoin quorum 2 serves the restored head")
	})

	t.Run("heads frozen above a chain that restarted lower are benched and routed around", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "fork-a", chainID: 123, latestBlock: 9_060},
			{id: "fork-b", chainID: 123, latestBlock: 9_060},
			{id: "canon-a", chainID: 123, latestBlock: 9_000},
			{id: "canon-b", chainID: 123, latestBlock: 9_000},
		}, nil)
		forks, canons := ups[:2], ups[2:]
		tr := network.metricsTracker
		const forkAt = int64(9_060)
		forkTs := servedTipClock().Add(-10 * time.Minute).Unix()
		canon := int64(9_000)
		poll := func(d time.Duration) {
			advance(d)
			for _, u := range forks {
				report(tr, u, forkAt, forkTs)
			}
			canon += int64(d / (2 * time.Second))
			for _, u := range canons {
				report(tr, u, canon, servedTipClock().Unix())
			}
		}

		// The corroborated head is a frozen fork's, so the network block time
		// stays unknown.
		window := stallAfter(forks[0], 0)
		for elapsed := time.Duration(0); elapsed+2*time.Second < window; elapsed += 2 * time.Second {
			poll(2 * time.Second)
		}
		for _, u := range forks {
			require.False(t, tr.IsHeadBenched(u), "%s: not benched inside its stall window", u.Id())
		}
		served(t, network, forkAt, "inside the stall window quorum 2 still serves the frozen pair")

		// The forks re-report first in each poll, so the bench lands before the
		// canonical pair moves: the canonical head at bench time is `canon` now.
		benchedAt := canon
		poll(4 * time.Second)
		for _, u := range forks {
			require.True(t, tr.IsHeadBenched(u), "%s: a head whose block is far older than its moving peers' is benched", u.Id())
		}
		for _, u := range canons {
			require.False(t, tr.IsHeadBenched(u), "%s", u.Id())
			require.Zero(t, lagOf(network, u), "%s: the canonical pair is the lag reference", u.Id())
		}
		served(t, network, canon, "both forks benched: the canonical pair defines latest")

		// The canonical chain passes the frozen height. The frozen head still
		// lags by what the chain made since its bench, not by the one block it
		// now trails: its blocks up to forkAt are the dead fork's.
		for canon <= forkAt {
			poll(2 * time.Second)
		}
		for _, u := range forks {
			require.True(t, tr.IsHeadBenched(u), "%s: a frozen head never rejoins", u.Id())
		}
		lag := lagOf(network, forks[0])
		require.Equal(t, canon-benchedAt, lag, "a frozen benched head lags by what the chain made since its bench")
		require.Greater(t, lag, int64(16), "past the default blockNumberLagAbove, so routing leaves the frozen head")
		served(t, network, canon, "as the canonical chain moves on")
	})

	t.Run("one moving wrong-chain peer cannot bench a halted fleet by block age", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "a", chainID: 123, latestBlock: 5_000},
			{id: "b", chainID: 123, latestBlock: 5_000},
			{id: "rogue", chainID: 123, latestBlock: 1_000},
		}, nil)
		a, b, rogue := ups[0], ups[1], ups[2]
		tr := network.metricsTracker
		haltedTs := servedTipClock().Add(-10 * time.Minute).Unix()
		rogueHead := int64(1_000)
		for range 300 {
			advance(2 * time.Second)
			report(tr, a, 5_000, haltedTs)
			report(tr, b, 5_000, haltedTs)
			rogueHead++
			report(tr, rogue, rogueHead, servedTipClock().Unix())
			require.False(t, tr.IsHeadBenched(a), "one younger peer is not enough while two peers exist")
			require.False(t, tr.IsHeadBenched(b), "one younger peer is not enough while two peers exist")
		}
		require.Zero(t, lagOf(network, a))
		require.Zero(t, lagOf(network, b))
		served(t, network, 5_000, "a halted fleet beside one moving wrong-chain peer")
	})

	t.Run("an endpoint serving another chain with the same chain id is benched by block age", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "chain", chainID: 123, latestBlock: 47_900_000},
			{id: "other", chainID: 123, latestBlock: 1_823_000},
		}, nil)
		chain, other := ups[0], ups[1]
		tr := network.metricsTracker
		// Two chains share the chain id: ours makes a block every 2s, the other
		// (an on-demand rollup testnet) one every 5 minutes, both with fresh
		// timestamps. 46M blocks apart, no lead is plausible, so only block
		// age can tell them apart.
		const gap = 5 * time.Minute
		window := stallAfter(other, 0)
		require.Less(t, 2*window, gap, "the other chain's gap must outlast its bench delay")
		head, otherHead := int64(47_900_000), int64(1_823_000)
		report(tr, chain, head, servedTipClock().Unix())
		report(tr, other, otherHead, servedTipClock().Unix())
		served(t, network, otherHead, "until block age tells the chains apart, quorum 2 serves the lower head")

		for elapsed := time.Duration(0); elapsed < gap; elapsed += 2 * time.Second {
			advance(2 * time.Second)
			head++
			report(tr, chain, head, servedTipClock().Unix())
		}
		require.True(t, tr.IsHeadBenched(other), "a head whose block stays a stall window older than its peer's is benched")
		require.False(t, tr.IsHeadBenched(chain))
		served(t, network, head, "the chain that keeps producing blocks defines latest")
		require.Greater(t, lagOf(network, other), int64(16), "past the default blockNumberLagAbove, so routing leaves it")

		// Its next block never brings it back: it cannot reach our head.
		advance(2 * time.Second)
		head++
		report(tr, other, otherHead+1, servedTipClock().Unix())
		report(tr, chain, head, servedTipClock().Unix())
		require.True(t, tr.IsHeadBenched(other))
		served(t, network, head, "after the other chain's next block")
	})

	t.Run("a static head at a slow chain's tip is not benched while peers catch up", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "tip", chainID: 123, latestBlock: 6_000},
			{id: "c1", chainID: 123, latestBlock: 5_000},
			{id: "c2", chainID: 123, latestBlock: 5_000},
		}, nil)
		tip, catchers := ups[0], ups[1:]
		tr := network.metricsTracker
		start := servedTipClock()
		tipTs := start.Add(-50 * time.Second).Unix()
		// A 60s-block chain: the catchers replay blocks produced ~20h ago.
		tsOf := func(n int64) int64 { return start.Add(-20*time.Hour).Unix() + (n-5_000)*60 }
		c := int64(5_000)
		for range 30 {
			advance(10 * time.Second)
			report(tr, tip, 6_000, tipTs)
			c += 5
			for _, u := range catchers {
				report(tr, u, c, tsOf(c))
			}
			require.False(t, tr.IsHeadBenched(tip), "peers below with older blocks never prove the tip stalled (at %d)", c)
		}
		served(t, network, c, "quorum 2 serves the catching-up pair")
	})

	t.Run("quorum majority: three of five stall", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "live-a", chainID: 123, latestBlock: 7_000},
			{id: "live-b", chainID: 123, latestBlock: 7_000},
			{id: "stalled-a", chainID: 123, latestBlock: 7_000},
			{id: "stalled-b", chainID: 123, latestBlock: 7_000},
			{id: "stalled-c", chainID: 123, latestBlock: 7_000},
		}, &common.EvmServedTipConfig{Quorum: common.ServedTipQuorumMajority})
		lives, stalls := ups[:2], ups[2:]
		tr := network.metricsTracker

		head := int64(7_000)
		for range 5 {
			advance(2 * time.Second)
			head += 2
			for _, u := range ups {
				u.EvmStatePoller().SuggestLatestBlock(head)
			}
		}
		served(t, network, head, "a healthy fleet")
		stalledAt := head

		window := stallAfter(stalls[0], 0)
		for elapsed := time.Duration(0); elapsed+2*time.Second < window; elapsed += 2 * time.Second {
			advance(2 * time.Second)
			head += 2
			for _, u := range lives {
				u.EvmStatePoller().SuggestLatestBlock(head)
			}
		}
		for _, u := range stalls {
			require.False(t, tr.IsHeadBenched(u), "%s: inside its stall window", u.Id())
		}
		served(t, network, stalledAt, "inside the stall window the stalled majority (3 of 5) holds latest")

		advance(4 * time.Second)
		head += 2
		for _, u := range lives {
			u.EvmStatePoller().SuggestLatestBlock(head)
		}
		for _, u := range stalls {
			require.True(t, tr.IsHeadBenched(u), "%s: benched after its stall window", u.Id())
			require.Equal(t, head-stalledAt, lagOf(network, u), "%s", u.Id())
		}
		for _, u := range lives {
			require.False(t, tr.IsHeadBenched(u), "%s", u.Id())
		}
		served(t, network, head, "the live pair is the majority of the non-benched voters")

		advance(time.Second)
		stalls[0].EvmStatePoller().SuggestLatestBlock(head)
		require.False(t, tr.IsHeadBenched(stalls[0]), "a stalled head that caught up rejoins")
		require.Zero(t, lagOf(network, stalls[0]))
		for _, u := range stalls[1:] {
			require.True(t, tr.IsHeadBenched(u), "%s", u.Id())
		}
		served(t, network, head, "after one rejoins")
	})

	t.Run("a benched head still votes when every other voter leaves", func(t *testing.T) {
		advance := pinServedTipClock(t)
		network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
			{id: "live", chainID: 123, latestBlock: 8_000},
			{id: "stalled", chainID: 123, latestBlock: 8_000},
		}, nil)
		live, stalled := ups[0], ups[1]
		tr := network.metricsTracker
		const stalledAt = int64(8_000)
		head := stalledAt
		for elapsed := time.Duration(0); elapsed <= stallAfter(stalled, 0); elapsed += 2 * time.Second {
			advance(2 * time.Second)
			head += 2
			live.EvmStatePoller().SuggestLatestBlock(head)
		}
		require.True(t, tr.IsHeadBenched(stalled), "precondition: stalled is benched")
		served(t, network, head, "precondition: live defines latest")

		live.EvmStatePoller().SetSyncingState(common.EvmSyncingStateSyncing)
		require.Equal(t, stalledAt, network.EvmHighestLatestBlockNumber(ctx),
			"with no non-benched voter left, latest fails open to the benched head instead of 0")
	})
}

// ─── The finalized axis is not a second-class citizen ────────────────────────
//
// Every mechanism runs per axis, and the finalized tip drives cache finality and
// the availability gate just as the latest tip drives interpolation. These pin
// all three of them end to end on `finalized`.

// setServedTipFinalized pushes an upstream's finalized head and waits for the
// poller's asynchronous suggestion path to apply it.
func setServedTipFinalized(t *testing.T, u *upstream.Upstream, head int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		if u.EvmStatePoller().FinalizedBlock() >= head {
			return true
		}
		u.EvmStatePoller().SuggestFinalizedBlock(head)
		return u.EvmStatePoller().FinalizedBlock() >= head
	}, 5*time.Second, time.Millisecond, "finalized head %d never applied on %s", head, u.Id())
}

// A serving-range cap must not define the FINALIZED tip either — same filter,
// same per-axis head comparison.
func TestServedTip_Finalized_BoundCappedUpstreamsDoNotVote(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "live-1", chainID: 123, latestBlock: 10_000},
		{id: "live-2", chainID: 123, latestBlock: 10_000},
		{id: "capped-1", chainID: 123, latestBlock: 10_000, upperExactBlock: 5_000},
		{id: "capped-2", chainID: 123, latestBlock: 10_000, upperExactBlock: 5_000},
	})
	for i, u := range ups {
		fin := int64(9_900)
		if i == 1 {
			fin = 9_898
		}
		setServedTipFinalized(t, u, fin)
	}

	assert.Equal(t, int64(9_898), network.EvmHighestFinalizedBlockNumber(ctx),
		"the capped pair's effective finalized is their 5000 bound, which is a serving range, not a finalized head")
}

// The regression guard on the finalized axis: one of the fresh pair leaves the
// eligible set, the ballot collapses onto heads far below, and the guard holds
// the last corroborated finalized tip.
func TestServedTip_Finalized_RegressionGuardHolds(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pinServedTipClock(t)

	network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 1_010_000},
		{id: "u2", chainID: 123, latestBlock: 1_010_000},
		{id: "u3", chainID: 123, latestBlock: 1_010_000},
		{id: "u4", chainID: 123, latestBlock: 1_010_000},
	}, nil)
	for i, u := range ups {
		fin := int64(1_000_000)
		if i < 2 {
			fin = 1_002_400
		}
		setServedTipFinalized(t, u, fin)
	}
	require.Equal(t, int64(1_002_400), network.EvmHighestFinalizedBlockNumber(ctx),
		"quorum 2: the fresh pair defines the finalized tip")

	before := servedTipRegressionCount(network, "finalized", "held")
	network.PinUpstreamOrderForTest("u1", "u3", "u4")
	assert.Equal(t, int64(1_002_400), network.EvmHighestFinalizedBlockNumber(ctx),
		"the guard protects the finalized axis exactly as it protects latest")
	assert.Equal(t, before+1, servedTipRegressionCount(network, "finalized", "held"))
}

// The guard's clamp branch: the upstream that backed the remembered pick leaves
// the eligible set and the best remaining LIVE head is BELOW it, so the held
// value must be capped to what a live upstream actually has.
func TestServedTip_RegressionGuard_HeldValueIsClampedToTheLiveHead(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pinServedTipClock(t)

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "live-1", chainID: 123, latestBlock: 10_000},
		{id: "live-2", chainID: 123, latestBlock: 10_000},
		{id: "live-3", chainID: 123, latestBlock: 10_000},
		{id: "live-4", chainID: 123, latestBlock: 10_000},
		{id: "low", chainID: 123, latestBlock: 9_000},
		{id: "stuck-1", chainID: 123, latestBlock: 5},
		{id: "stuck-2", chainID: 123, latestBlock: 5},
	})
	require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx))
	require.Equal(t, int64(10_000), network.servedLatestAnchor.lastGoodValue.Load())

	// Every upstream that had 10_000 drops out. The ballot is [9000, 5, 5] → 5,
	// which the guard suppresses — but it may not serve its remembered 10_000
	// either, because no live upstream has it any more.
	network.PinUpstreamOrderForTest("low", "stuck-1", "stuck-2")

	assert.Equal(t, int64(9_000), network.EvmHighestLatestBlockNumber(ctx),
		"the held value must be clamped to the best live head, and must still suppress the poisoned pick")
}

// Metrics count STATE TRANSITIONS, not evaluations. The served tip is computed
// several times per request (block-tag interpolation, the availability gate, the
// empty-result guard), so a per-evaluation counter scales with RPS and reports
// one sustained regression as thousands of them.
func TestServedTip_MetricsCountTransitionsNotEvaluations(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	advance := pinServedTipClock(t)

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "live-1", chainID: 123, latestBlock: 10_000},
		{id: "live-2", chainID: 123, latestBlock: 10_000},
		{id: "live-3", chainID: 123, latestBlock: 10_000},
		{id: "stuck-1", chainID: 123, latestBlock: 5},
		{id: "stuck-2", chainID: 123, latestBlock: 5},
	})
	require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx))

	before := servedTipRegressionCount(network, "latest", "held")
	network.PinUpstreamOrderForTest("live-1", "stuck-1", "stuck-2")

	// One sustained regression, twenty evaluations of it — the shape of a few
	// requests, not of twenty incidents.
	for i := 0; i < 20; i++ {
		require.Equal(t, int64(10_000), network.EvmHighestLatestBlockNumber(ctx))
	}
	assert.Equal(t, before+1, servedTipRegressionCount(network, "latest", "held"),
		"entering the hold is one event, however many times the tip is computed while it lasts")

	// Failing open is likewise one event, not one per evaluation.
	beforeOpen := servedTipRegressionCount(network, "latest", "failed_open")
	advance(servedTipRegressionTTL + time.Second)
	for i := 0; i < 20; i++ {
		network.EvmHighestLatestBlockNumber(ctx)
	}
	assert.Equal(t, beforeOpen+1, servedTipRegressionCount(network, "latest", "failed_open"))
}
