package erpc

import (
	"context"
	"net/http"
	"strconv"
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

// These tests pin the semantics of Network.EvmHighestLatestBlockNumber in
// served-tip mode: the MAJORITY order statistic — the freshest block a strict
// majority of eligible upstreams already have (evm.PickServedTip). Each case
// is designed so MAX(tips) != majority to make regressions visible.

// servedTipFixture is a compact description of one upstream's poller state
// at test time. Used to drive the lighter setupServedTipNetwork helper.
type servedTipFixture struct {
	id          string
	chainID     int64
	latestBlock int64
	// upperExactBlock, when > 0, configures blockAvailability.upper.exactBlock —
	// the upstream still polls latestBlock as its head, but only SERVES up to
	// this block, so its effective latest is the bound rather than a head
	// observation (the celo "legacy" upstream shape).
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

// ─── Availability bounds are not head observations (2026-08 celo) ────────────

// A blockAvailability.upper bound is a SERVING-RANGE declaration, not a view of
// the chain head, so it must not appear on the ballot while any live head does.
// Two upstreams pinned at `upper.exactBlock` (their pollers sitting at the live
// tip) used to outvote the live heads as soon as the eligible set shrank —
// exactly how celo served September-2023 state on 2026-08-11.
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

// The guaranteed-method floor was the second door onto the incident value: its
// ballot took raw effective heads and ran AFTER the guard, so a static cap that
// the network ballot had just refused came back as a "floor" and was served.
// The floor now builds its per-method ballot with the same rule.
func TestServedTip_GuaranteedMethodFloorCannotReadmitTheCap(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const legacyCap = int64(21_615_999)
	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "prism-celo", chainID: 123, latestBlock: 74_550_978},
		{id: "celo-mainnet-nirvana", chainID: 123, latestBlock: 74_550_974},
		{id: "quicknode-celo", chainID: 123, latestBlock: 74_550_946},
		{id: "celo-mainnet-nirvana-legacy", chainID: 123, latestBlock: 74_550_978, upperExactBlock: legacyCap},
		{id: "quicknode-celo-legacy", chainID: 123, latestBlock: 74_550_978, upperExactBlock: legacyCap},
	}, &common.EvmServedTipConfig{
		EnabledFor:        []string{"latest"},
		GuaranteedMethods: []string{"eth_getLogs"},
	})

	require.Greater(t, network.EvmHighestLatestBlockNumber(ctx), legacyCap,
		"precondition: with the full fleet eligible the tip is a live head")

	// The incident ballot: one live upstream lag-excluded.
	network.PinUpstreamOrderForTest("celo-mainnet-nirvana", "quicknode-celo", "celo-mainnet-nirvana-legacy", "quicknode-celo-legacy")

	assert.Greater(t, network.EvmHighestLatestBlockNumber(ctx), legacyCap,
		"the guaranteed-method floor must not re-admit the serving range the ballot filter removed")
}

// THE COMPOSITION of the two doors: the guard correctly refuses a cap-only
// ballot (no live head → it holds its last corroborated pick), and the
// guaranteed-method floor — which runs AFTER the guard — then recomputes that
// same collapsed instant and drags the served value back to the cap. Each fix
// was correct alone; together they still served 21,615,999.
//
// The live upstreams here go SYNCING rather than being un-pinned: that is the
// production shape (lag-excluded, cordoned, mid-restart upstreams stay
// REGISTERED), and registration is what tells the floor that a cap-only ballot
// is transient rather than the operator's configuration.
func TestServedTip_GuaranteedMethodFloor_DoesNotUndoTheGuardsHold(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	advance := pinServedTipClock(t)

	const legacyCap = int64(21_615_999)
	network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "prism-celo", chainID: 123, latestBlock: 74_550_978},
		{id: "celo-mainnet-nirvana", chainID: 123, latestBlock: 74_550_974},
		{id: "quicknode-celo", chainID: 123, latestBlock: 74_550_946},
		{id: "celo-mainnet-nirvana-legacy", chainID: 123, latestBlock: 74_550_978, upperExactBlock: legacyCap},
		{id: "quicknode-celo-legacy", chainID: 123, latestBlock: 74_550_978, upperExactBlock: legacyCap},
	}, &common.EvmServedTipConfig{
		EnabledFor:        []string{"latest"},
		GuaranteedMethods: []string{"eth_getLogs"},
	})

	healthy := network.EvmHighestLatestBlockNumber(ctx)
	require.Greater(t, healthy, legacyCap, "precondition: the live heads carry the ballot")

	// Every live upstream drops out of the ballot at once; only the two
	// 2023-capped ones remain servable, and they support eth_getLogs.
	for _, u := range ups[:3] {
		u.EvmStatePoller().SetSyncingState(common.EvmSyncingStateSyncing)
	}

	assert.Equal(t, healthy, network.EvmHighestLatestBlockNumber(ctx),
		"the floor must not undo the guard's hold: live upstreams serve eth_getLogs, "+
			"they are merely absent, so the cap-only floor is a transient lie")
	assert.Zero(t, network.guaranteedMethodFloor(ctx, false),
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
		EnabledFor:        []string{"latest"},
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
// moment the healthy pair leaves the eligible set — the same shape as celo's
// bound-capped pair, reached without any bounds at all (which is why the guard
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
// served as computed. This is the property whose absence (a clamp that could
// hold forever) froze the fleet in 2026-06.
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

// THE remaining door onto the celo incident value, and the reason the guard no
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

	const legacyCap = int64(21_615_999)
	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "prism-celo", chainID: 123, latestBlock: 74_550_978},
		{id: "celo-mainnet-nirvana", chainID: 123, latestBlock: 74_550_974},
		{id: "quicknode-celo", chainID: 123, latestBlock: 74_550_946},
		{id: "celo-mainnet-nirvana-legacy", chainID: 123, latestBlock: 74_550_978, upperExactBlock: legacyCap},
		{id: "quicknode-celo-legacy", chainID: 123, latestBlock: 74_550_978, upperExactBlock: legacyCap},
	}, &common.EvmServedTipConfig{EnabledFor: []string{"latest"}})

	healthy := network.EvmHighestLatestBlockNumber(ctx)
	require.Greater(t, healthy, legacyCap, "precondition: the live heads carry the ballot")
	beforeHeld := servedTipRegressionCount(network, "latest", "held")
	beforeOpen := servedTipRegressionCount(network, "latest", "failed_open")

	// All three live upstreams drop out at once (a vendor incident, a policy
	// exclusion sweep): only the two 2023-capped ones remain eligible.
	network.PinUpstreamOrderForTest("celo-mainnet-nirvana-legacy", "quicknode-celo-legacy")

	assert.Equal(t, healthy, network.EvmHighestLatestBlockNumber(ctx),
		"with no live head left the guard must hold its last corroborated pick, "+
			"not hand `latest` to a September-2023 serving range")
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

// The off switch, symmetric with trajectoryWindow: 0.
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
	}, &common.EvmServedTipConfig{EnabledFor: []string{"latest"}, MaxRegressionBlocks: -1})

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

// ─── Long-term trajectory referee ────────────────────────────────────────────
//
// Every mechanism above is a snapshot of one instant, and a majority stall
// defeats all of them at once: when N upstreams freeze while staying eligible,
// the majority IS the stale group and the 2 upstreams holding the real head are
// outvoted. These tests pin the referee that breaks that tie from the network's
// own 10-minute head trajectory — and, just as importantly, pin how narrowly it
// is allowed to act.

const (
	// servedTipSampleInterval mirrors the tracker's unexported sample throttle
	// (architecture/evm: tipSampleInterval) — the cadence a warm-up must tick
	// at for the buffer to fill.
	servedTipSampleInterval = 5 * time.Second

	// servedTipMaxSampleGap mirrors architecture/evm's tipMaxSampleGap.
	servedTipMaxSampleGap = time.Minute

	// The warm-up track: 100 blocks per 5s sample = 20 blocks/s, a fast-L2
	// cadence that makes one stalled sample worth 100 blocks and keeps the
	// scenarios legible. 130 samples ≈ 10m45s, just past the default window.
	trajectoryStartHead       = int64(1_000_000)
	trajectoryBlocksPerSample = int64(100)
	trajectoryWarmSamples     = 130
)

// servedTipTrajectoryCount reads the referee's counter for a network/axis/outcome.
func servedTipTrajectoryCount(n *Network, axis, outcome string) float64 {
	return promUtil.ToFloat64(telemetry.MetricNetworkServedTipTrajectoryTotal.
		WithLabelValues(n.projectId, n.Label(), servedTipLaneAll, axis, outcome))
}

// servedTipIsOverriding reports whether the network-wide latest lane is
// CURRENTLY being overridden by the referee — the state the transition log and
// counter are driven from.
func servedTipIsOverriding(n *Network) bool {
	return n.servedLatestAnchor.refereeState.Load() == servedTipRefereeOverriding
}

// setupTrajectoryNetwork builds a `count`-upstream network with the pinned
// served-tip clock, and returns the clock's advance function.
func setupTrajectoryNetwork(t *testing.T, ctx context.Context, count int, stCfg *common.EvmServedTipConfig) (*Network, []*upstream.Upstream, func(time.Duration)) {
	t.Helper()
	advance := pinServedTipClock(t)
	fixtures := make([]servedTipFixture, count)
	for i := range fixtures {
		fixtures[i] = servedTipFixture{id: "u" + strconv.Itoa(i+1), chainID: 123, latestBlock: trajectoryStartHead}
	}
	network, ups := setupServedTipNetworkWith(t, ctx, fixtures, stCfg)
	return network, ups, advance
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

// warmServedTipTrajectory drives `samples` real evaluations one sample interval
// apart with every upstream advancing together, so the network's tracker sees a
// clean linear head track through the ordinary request path. Returns the head
// every upstream ends on (the clock sits on the last sample).
func warmServedTipTrajectory(t *testing.T, ctx context.Context, network *Network, ups []*upstream.Upstream, advance func(time.Duration), samples int) int64 {
	t.Helper()
	head := trajectoryStartHead
	for i := 0; i < samples; i++ {
		for _, u := range ups {
			u.EvmStatePoller().SuggestLatestBlock(head)
		}
		require.Equal(t, head, network.EvmHighestLatestBlockNumber(ctx),
			"warm-up sample %d: an agreeing fleet must serve its agreed head", i)
		if i < samples-1 {
			advance(servedTipSampleInterval)
			head += trajectoryBlocksPerSample
		}
	}
	return head
}

// stallMajority freezes every upstream except the first `fresh` of them, which
// keep the warm-up track for `samples` more evaluations. Returns the head the
// fresh group ends on and the last served value.
func stallMajority(t *testing.T, ctx context.Context, network *Network, ups []*upstream.Upstream, advance func(time.Duration), head int64, fresh int, samples int) (int64, int64) {
	t.Helper()
	served := int64(0)
	for i := 0; i < samples; i++ {
		advance(servedTipSampleInterval)
		head += trajectoryBlocksPerSample
		for _, u := range ups[:fresh] {
			u.EvmStatePoller().SuggestLatestBlock(head)
		}
		served = network.EvmHighestLatestBlockNumber(ctx)
	}
	return head, served
}

// THE headline case, and the requirement the referee exists for: in an N-vs-2
// split where the 2 sit at the higher block, use the 2. Three upstreams freeze
// inside the lag-exclusion threshold (a shared-counter freeze, a stuck vendor
// region) while two keep the chain's head; the strict majority is now the stale
// group, so the plain majority pick — and every instantaneous rule — serves a
// block the chain left minutes ago.
func TestServedTip_TrajectoryReferee_StalledMajorityLosesToFreshMinority(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups, advance := setupTrajectoryNetwork(t, ctx, 5, &common.EvmServedTipConfig{EnabledFor: []string{"latest"}})
	frozen := warmServedTipTrajectory(t, ctx, network, ups, advance, trajectoryWarmSamples)

	beforeOverride := servedTipTrajectoryCount(network, "latest", "override")
	beforeHeld := servedTipRegressionCount(network, "latest", "held")

	fresh, served := stallMajority(t, ctx, network, ups, advance, frozen, 2, 24)

	// What the plain majority pick WOULD be over the very same ballot: the
	// frozen head, held by 3 of the 5 upstreams.
	majority := evm.PickServedTip([]evm.ServedTipInput{
		{UpstreamID: "u1", BlockNumber: fresh},
		{UpstreamID: "u2", BlockNumber: fresh},
		{UpstreamID: "u3", BlockNumber: frozen},
		{UpstreamID: "u4", BlockNumber: frozen},
		{UpstreamID: "u5", BlockNumber: frozen},
	}).Tip
	require.Equal(t, frozen, majority,
		"precondition: the stalled group holds the majority, so the plain pick is the stale head")

	assert.Equal(t, fresh, served,
		"the corroborated pair that is where the chain should be by now must define the tip; "+
			"the plain majority would have served %d, %d blocks behind", majority, fresh-majority)
	assert.Greater(t, servedTipTrajectoryCount(network, "latest", "override"), beforeOverride,
		"an override is an intervention and must be counted")
	assert.Equal(t, beforeHeld, servedTipRegressionCount(network, "latest", "held"),
		"raising the tip to the live pair is not a regression; the guard must stay out of it")
}

// A routine transient: two of four upstreams fall behind for 15 seconds (a
// poller hiccup, a vendor blip) and catch up. Nothing is wrong with the fleet,
// and the value every upstream can serve is the lagged head — an intervention
// here would advertise a block only half the fleet has, for minutes. This shape
// is why corroboration has to be earned over time rather than in one instant.
func TestServedTip_TrajectoryReferee_TransientHalfFleetLagIsNotAnOverride(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups, advance := setupTrajectoryNetwork(t, ctx, 4, &common.EvmServedTipConfig{EnabledFor: []string{"latest"}})
	head := warmServedTipTrajectory(t, ctx, network, ups, advance, trajectoryWarmSamples)
	lagged := head

	before := servedTipTrajectoryCount(network, "latest", "override")

	// Three ticks in which only u1/u2 advance: u3/u4 end 300 blocks (15s of this
	// 20 blocks/s chain) behind — past the 200-block cluster width, so the fleet
	// reads as two groups of two.
	fresh, served := stallMajority(t, ctx, network, ups, advance, head, 2, 3)

	assert.Equal(t, lagged, served,
		"a 15-second divergence must keep serving the value all four upstreams have (fresh pair was at %d)", fresh)
	assert.Equal(t, before, servedTipTrajectoryCount(network, "latest", "override"),
		"and must not be counted as an intervention")
}

// A PERMANENTLY split fleet (the flashblocks shape: two upstreams always ~300
// blocks ahead, both halves advancing at the same velocity) is a normal
// topology, not a stall. The trajectory is fitted to the MEDIAN of the live
// heads, so no permanently-offset group is ever closer to it than the median's
// own — the referee has nothing to correct and stays silent forever.
func TestServedTip_TrajectoryReferee_PermanentlySplitFleetIsSilent(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups, advance := setupTrajectoryNetwork(t, ctx, 4, &common.EvmServedTipConfig{EnabledFor: []string{"latest"}})
	before := servedTipTrajectoryCount(network, "latest", "override")

	head := trajectoryStartHead
	for i := 0; i < trajectoryWarmSamples+40; i++ {
		for j, u := range ups {
			target := head
			if j < 2 {
				target = head + 300 // the always-ahead pair
			}
			suggestServedTipHead(u, target)
		}
		network.EvmHighestLatestBlockNumber(ctx)
		advance(servedTipSampleInterval)
		head += trajectoryBlocksPerSample
	}

	assert.Equal(t, before, servedTipTrajectoryCount(network, "latest", "override"),
		"a permanently split fleet must never trigger an override")
}

// The referee's intervention EXPIRES on its own. Once the group it elected stops
// advancing, that group can no longer have produced the chain progress the fit
// describes, and the override ends without any operator action — the property
// that makes an upward-only referee unable to pin a tip the way the 2026-06
// clamp did.
func TestServedTip_TrajectoryReferee_OverrideSelfExpires(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups, advance := setupTrajectoryNetwork(t, ctx, 5, &common.EvmServedTipConfig{EnabledFor: []string{"latest"}})
	frozen := warmServedTipTrajectory(t, ctx, network, ups, advance, trajectoryWarmSamples)
	fresh, served := stallMajority(t, ctx, network, ups, advance, frozen, 2, 24)
	require.Equal(t, fresh, served, "precondition: the referee is holding the tip on the corroborated pair")

	require.True(t, servedTipIsOverriding(network), "precondition: the referee reports itself as overriding")

	// Now the whole chain stops: nobody advances, including the elected pair.
	for i := 0; i < 240; i++ {
		advance(servedTipSampleInterval)
		network.EvmHighestLatestBlockNumber(ctx)
		if !servedTipIsOverriding(network) {
			t.Logf("override expired after %v of a fully halted chain", time.Duration(i+1)*servedTipSampleInterval)
			return
		}
	}
	t.Fatalf("the override never expired over %v of a halted chain", 240*servedTipSampleInterval)
}

// The inverse split, and the reason a group is elected by its DISTANCE to the
// trajectory rather than by its freshness: two upstreams running far past where
// the chain can possibly be (wrong chain, garbage endpoint) must lose to the
// healthy group, however corroborated they are with each other.
func TestServedTip_TrajectoryReferee_RunawayMinorityLoses(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// maxRegressionBlocks is widened so the branch's regression guard (which
	// measures against the RAW max live head, and would hold the tip the moment
	// any upstream runs 1024 blocks ahead) stays out of the way: what is under
	// test here is the referee's own election.
	network, ups, advance := setupTrajectoryNetwork(t, ctx, 5, &common.EvmServedTipConfig{
		EnabledFor:          []string{"latest"},
		MaxRegressionBlocks: 100_000,
	})
	head := warmServedTipTrajectory(t, ctx, network, ups, advance, trajectoryWarmSamples)

	beforeOverride := servedTipTrajectoryCount(network, "latest", "override")
	beforeFallback := servedTipTrajectoryCount(network, "latest", "fallback")

	// Two upstreams run 20k blocks (≈16 minutes of this chain) ahead; the other
	// three stay exactly on the track.
	advance(servedTipSampleInterval)
	head += trajectoryBlocksPerSample
	for _, u := range ups {
		suggestServedTipHead(u, head)
	}
	for _, u := range ups[:2] {
		suggestServedTipHead(u, head+20_000)
	}

	assert.Equal(t, head, network.EvmHighestLatestBlockNumber(ctx),
		"the runaway pair is nowhere near the trajectory; the healthy majority keeps the tip")
	assert.Equal(t, beforeOverride, servedTipTrajectoryCount(network, "latest", "override"),
		"no override happened")
	assert.Equal(t, beforeFallback, servedTipTrajectoryCount(network, "latest", "fallback"),
		"the healthy group is elected outright, so this is not even a near miss")
}

// Warm-up inertness: until the tracker has a window's worth of history the
// referee is a no-op and the network behaves exactly as it does without it.
// This is also the boot behaviour of every pod after every deploy.
func TestServedTip_TrajectoryReferee_ColdProcessIsInert(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups, advance := setupTrajectoryNetwork(t, ctx, 5, &common.EvmServedTipConfig{EnabledFor: []string{"latest"}})
	// Half a window: enough samples for a fit, nowhere near enough span.
	frozen := warmServedTipTrajectory(t, ctx, network, ups, advance, trajectoryWarmSamples/2)

	before := servedTipTrajectoryCount(network, "latest", "override")
	_, served := stallMajority(t, ctx, network, ups, advance, frozen, 2, 24)

	assert.Equal(t, frozen, served,
		"a cold tracker must leave the plain majority pick untouched")
	assert.Equal(t, before, servedTipTrajectoryCount(network, "latest", "override"))
}

// Staleness: the fit describes the present only if the track is continuous. A
// gap larger than the tracker's tolerance is exactly where a halt or a burst
// hides, so extrapolating across one is guessing — and the referee stands down.
// The paired sub-tests differ ONLY in the size of that gap.
func TestServedTip_TrajectoryReferee_StaleTrackFailsOpen(t *testing.T) {
	// After `gap` of silence the fresh pair returns exactly on the trajectory
	// (20 blocks/s) and then HOLDS it for longer than the dwell — i.e. the
	// referee's evidence is otherwise perfect, and the only difference between
	// the sub-tests is whether the hole in the track is inside the tolerance.
	run := func(t *testing.T, gap time.Duration) (served, frozen, fresh int64, overrides float64) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network, ups, advance := setupTrajectoryNetwork(t, ctx, 5, &common.EvmServedTipConfig{EnabledFor: []string{"latest"}})
		frozen = warmServedTipTrajectory(t, ctx, network, ups, advance, trajectoryWarmSamples)
		before := servedTipTrajectoryCount(network, "latest", "override")

		advance(gap)
		fresh = frozen + trajectoryBlocksPerSample*int64(gap/servedTipSampleInterval)
		for _, u := range ups[:2] {
			suggestServedTipHead(u, fresh)
		}
		served = network.EvmHighestLatestBlockNumber(ctx)

		// Eight more samples (40s) of the pair tracking the chain: enough dwell
		// for a group that is genuinely where the head should be.
		for i := 0; i < 8; i++ {
			advance(servedTipSampleInterval)
			fresh += trajectoryBlocksPerSample
			for _, u := range ups[:2] {
				suggestServedTipHead(u, fresh)
			}
			served = network.EvmHighestLatestBlockNumber(ctx)
		}
		return served, frozen, fresh, servedTipTrajectoryCount(network, "latest", "override") - before
	}

	t.Run("ContinuousTrackOverrides", func(t *testing.T) {
		served, _, fresh, overrides := run(t, servedTipMaxSampleGap-5*time.Second)
		assert.Equal(t, fresh, served, "an unbroken track lets the referee act")
		assert.Positive(t, overrides)
	})

	t.Run("GappedTrackFailsOpen", func(t *testing.T) {
		served, frozen, _, overrides := run(t, servedTipMaxSampleGap+time.Second)
		assert.Equal(t, frozen, served,
			"one sample past the gap tolerance the same evidence must be refused")
		assert.Zero(t, overrides)
	})
}

// A chain that pauses and bursts (an L2 landing batches) has a large residual
// spread of its own, which widens the referee's tolerance automatically — and a
// legitimate fleet-wide jump keeps every head in ONE cluster, where the referee
// has nothing to choose between. Neither may produce a false override.
func TestServedTip_TrajectoryReferee_PauseThenBurstDoesNotFalsePositive(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups, advance := setupTrajectoryNetwork(t, ctx, 5, &common.EvmServedTipConfig{EnabledFor: []string{"latest"}})

	// Same average velocity as the linear warm-up, delivered in batches: nine
	// samples of nothing, then 1000 blocks at once.
	head := trajectoryStartHead
	for i := 0; i < trajectoryWarmSamples; i++ {
		if i%10 == 9 {
			head += trajectoryBlocksPerSample * 10
		}
		for _, u := range ups {
			u.EvmStatePoller().SuggestLatestBlock(head)
		}
		require.Equal(t, head, network.EvmHighestLatestBlockNumber(ctx))
		advance(servedTipSampleInterval)
	}

	beforeOverride := servedTipTrajectoryCount(network, "latest", "override")
	beforeFallback := servedTipTrajectoryCount(network, "latest", "fallback")

	// The whole fleet lands the next batch, with ordinary propagation skew.
	head += 5_000
	for i, u := range ups {
		suggestServedTipHead(u, head-int64(i%2)*50)
	}
	served := network.EvmHighestLatestBlockNumber(ctx)

	assert.Equal(t, head, served,
		"a legitimate fleet-wide burst is one cluster; the majority pick stands")
	assert.Equal(t, beforeOverride, servedTipTrajectoryCount(network, "latest", "override"))
	assert.Equal(t, beforeFallback, servedTipTrajectoryCount(network, "latest", "fallback"),
		"a fleet in one cluster does not even register a disagreement")
}

// Corroboration, not agreement with the model: a single upstream sitting
// exactly where the trajectory says the head is proves nothing — it is the one
// endpoint that could be wrong in the same direction as everyone else being
// stuck. A group of one is never electable.
func TestServedTip_TrajectoryReferee_SingletonIsNotCorroboration(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups, advance := setupTrajectoryNetwork(t, ctx, 5, &common.EvmServedTipConfig{EnabledFor: []string{"latest"}})
	frozen := warmServedTipTrajectory(t, ctx, network, ups, advance, trajectoryWarmSamples)

	before := servedTipTrajectoryCount(network, "latest", "override")
	_, served := stallMajority(t, ctx, network, ups, advance, frozen, 1, 24)

	assert.Equal(t, frozen, served,
		"one on-trajectory witness cannot move the tip, however well it fits")
	assert.Equal(t, before, servedTipTrajectoryCount(network, "latest", "override"))
}

// Composition with the regression guard: the referee runs first and the guard
// judges whatever comes out of it. Once a stall has handed the tip to the fresh
// pair, losing that pair from the eligible set drops the majority pick far below
// the best live head — and the guard holds the last corroborated value exactly
// as it does for any other poisoned ballot.
func TestServedTip_TrajectoryReferee_ComposesWithRegressionGuard(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups, advance := setupTrajectoryNetwork(t, ctx, 5, &common.EvmServedTipConfig{EnabledFor: []string{"latest"}})
	frozen := warmServedTipTrajectory(t, ctx, network, ups, advance, trajectoryWarmSamples)
	fresh, served := stallMajority(t, ctx, network, ups, advance, frozen, 2, 24)
	require.Equal(t, fresh, served, "precondition: the referee is holding the tip on the fresh pair")
	require.Greater(t, fresh-frozen, int64(common.DefaultToleratedBlockHeadRollback),
		"precondition: the stall is deeper than the regression tolerance")

	before := servedTipRegressionCount(network, "latest", "held")

	// One fresh upstream leaves the eligible set: the remaining ballot is
	// [fresh, frozen, frozen] — a lone fresh head is no group, so the referee
	// stands down and the majority pick collapses onto the stalled pair.
	network.PinUpstreamOrderForTest("u1", "u3", "u4")

	assert.Equal(t, fresh, network.EvmHighestLatestBlockNumber(ctx),
		"the guard must hold the last corroborated pick, as it does for any poisoned ballot")
	assert.Equal(t, before+1, servedTipRegressionCount(network, "latest", "held"))
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

// The regression guard and the trajectory referee, both on the finalized axis,
// in one run: warm a finalized head track, stall the majority (the referee must
// serve the corroborated fresh pair), then lose one of that pair from the
// eligible set (the guard must hold what it last saw corroborated).
func TestServedTip_Finalized_RefereeAndGuardEndToEnd(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups, advance := setupTrajectoryNetwork(t, ctx, 5, &common.EvmServedTipConfig{EnabledFor: []string{"finalized"}})

	head := trajectoryStartHead
	for i := 0; i < trajectoryWarmSamples; i++ {
		for _, u := range ups {
			setServedTipFinalized(t, u, head)
		}
		require.Equal(t, head, network.EvmHighestFinalizedBlockNumber(ctx),
			"warm-up sample %d: an agreeing fleet serves its agreed finalized head", i)
		advance(servedTipSampleInterval)
		head += trajectoryBlocksPerSample
	}
	frozen := head - trajectoryBlocksPerSample

	// The stall: three finalized heads freeze, two keep tracking the chain.
	var served int64
	for i := 0; i < 24; i++ {
		for _, u := range ups[:2] {
			setServedTipFinalized(t, u, head)
		}
		served = network.EvmHighestFinalizedBlockNumber(ctx)
		advance(servedTipSampleInterval)
		head += trajectoryBlocksPerSample
	}
	fresh := head - trajectoryBlocksPerSample
	require.Equal(t, fresh, served,
		"the corroborated fresh pair must define the finalized tip; the plain majority would serve %d", frozen)
	assert.Positive(t, servedTipTrajectoryCount(network, "finalized", "override"),
		"the finalized axis gets its own counter series")

	// One of the fresh pair leaves: the ballot collapses onto the stalled trio
	// and the guard holds the last corroborated finalized tip.
	before := servedTipRegressionCount(network, "finalized", "held")
	network.PinUpstreamOrderForTest("u1", "u3", "u4")
	assert.Equal(t, fresh, network.EvmHighestFinalizedBlockNumber(ctx),
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

// An override the guaranteed-method floor immediately nullifies did not happen:
// the referee raised the pick, the floor put it back, the served value is the
// majority pick — counting or logging that would page someone about nothing.
func TestServedTip_OverrideNullifiedByTheFloorIsNotCounted(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// u5 is the only trace_block-capable upstream and it is one of the stalled
	// trio, so its supporting set floors the tip back at the frozen head.
	advance := pinServedTipClock(t)
	fixtures := make([]servedTipFixture, 5)
	for i := range fixtures {
		fixtures[i] = servedTipFixture{id: "u" + strconv.Itoa(i+1), chainID: 123, latestBlock: trajectoryStartHead}
		if i < 4 {
			fixtures[i].ignoreMethods = []string{"trace_block"}
		}
	}
	network, ups := setupServedTipNetworkWith(t, ctx, fixtures, &common.EvmServedTipConfig{
		EnabledFor:        []string{"latest"},
		GuaranteedMethods: []string{"trace_block"},
	})

	frozen := warmServedTipTrajectory(t, ctx, network, ups, advance, trajectoryWarmSamples)
	before := servedTipTrajectoryCount(network, "latest", "override")

	_, served := stallMajority(t, ctx, network, ups, advance, frozen, 2, 24)

	assert.Equal(t, frozen, served,
		"the floor holds the tip at what the trace_block upstream can serve")
	assert.Equal(t, before, servedTipTrajectoryCount(network, "latest", "override"),
		"an override the floor nullified must not be counted as an intervention")
}

// The off switch is a real off switch: trajectoryWindow: 0 records nothing and
// decides nothing, so the network serves the plain majority pick — the stale
// head — through the exact scenario the referee exists to fix.
func TestServedTip_TrajectoryReferee_DisabledByZeroWindow(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups, advance := setupTrajectoryNetwork(t, ctx, 5, &common.EvmServedTipConfig{
		EnabledFor:       []string{"latest"},
		TrajectoryWindow: common.Duration(0).Ptr(),
	})
	frozen := warmServedTipTrajectory(t, ctx, network, ups, advance, trajectoryWarmSamples)

	before := servedTipTrajectoryCount(network, "latest", "override")
	_, served := stallMajority(t, ctx, network, ups, advance, frozen, 2, 24)

	assert.Equal(t, frozen, served,
		"with the referee off the stalled majority defines the tip again")
	assert.Equal(t, before, servedTipTrajectoryCount(network, "latest", "override"))
	assert.Zero(t, network.servedLatestAnchor.trajectory.SampleCount(),
		"a disabled referee must not even record a head track")
}
