package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the semantics of Network.EvmHighestLatestBlockNumber in
// served-tip mode: the MAJORITY order statistic — the freshest block a strict
// majority of eligible upstreams already have (evm.PickServedTip). Each case
// is designed so MAX(tips) != majority to make regressions visible.

// servedTipFixture is a compact description of one upstream's poller state
// at test time. Used to drive the lighter setupServedTipNetwork helper.
type servedTipFixture struct {
	id            string
	chainID       int64
	latestBlock   int64
	syncing       bool
	ignoreMethods []string
	tags          []string
}

// setupServedTipNetwork bootstraps a Network with the supplied upstream
// fixtures and pushes each upstream's poller to the requested state.
//
// Each fixture maps to one upstream with a unique localhost endpoint mocked
// via gock to answer eth_chainId so initial validation succeeds. After
// bootstrap, SuggestLatestBlock and SetSyncingState are applied per fixture.
//
// Returns the network and the upstream slice in input order.
func setupServedTipNetwork(t *testing.T, ctx context.Context, fixtures []servedTipFixture) (*Network, []*upstream.Upstream) {
	return setupServedTipNetworkWith(t, ctx, fixtures, &common.EvmServedTipConfig{EnabledFor: []string{"latest", "finalized"}})
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
		upstreamConfigs = append(upstreamConfigs, &common.UpstreamConfig{
			Type:          common.UpstreamTypeEvm,
			Id:            f.id,
			Endpoint:      endpoint,
			IgnoreMethods: f.ignoreMethods,
			Tags:          f.tags,
			Evm: &common.EvmUpstreamConfig{
				ChainId: cid,
			},
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

// With the served-tip feature DISABLED (the default, nil config), the network
// must preserve the legacy MAX-across-upstreams behavior.
func TestServedTip_DisabledByDefault_ReturnsMax(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: 100},
		{id: "u2", chainID: 123, latestBlock: 99},
		{id: "u3", chainID: 123, latestBlock: 98},
	}, nil) // nil ServedTip config => feature disabled => legacy MAX

	served := network.EvmHighestLatestBlockNumber(ctx)
	assert.Equal(t, int64(100), served,
		"served-tip clustering disabled (default) must return MAX(tips)=100, not cluster-min")
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
		EnabledFor:        []string{"latest"},
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
// mis-tuned gate freezing on the jump is what wedged prod; statelessness has
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
