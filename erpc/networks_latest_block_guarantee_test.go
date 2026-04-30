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

// guaranteeFixture wires up a Network plus N Upstreams, mocks chainId, and exposes
// the bootstrapped objects so individual tests can manipulate state pollers and
// then call EvmHighestLatestBlockNumber/EvmHighestFinalizedBlockNumber.
//
// Why a helper: every existing TestNetwork_HighestLatestBlockNumber subtest
// repeats ~70 lines of boilerplate. Extracting the boilerplate keeps each new
// subtest focused on the property under test.
type guaranteeFixture struct {
	t       *testing.T
	ctx     context.Context
	cancel  context.CancelFunc
	network *Network
	// upstreams is keyed by upstream Id for easy lookup.
	upstreams map[string]*upstream.Upstream
}

func newGuaranteeFixture(
	t *testing.T,
	upstreamCfgs []*common.UpstreamConfig,
	guaranteedMethods []string,
) *guaranteeFixture {
	t.Helper()

	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Mock eth_chainId for every upstream so detectFeatures succeeds. Persist so
	// any in-flight bootstrap retries don't run out of mocks.
	for _, ucfg := range upstreamCfgs {
		gock.New(ucfg.Endpoint).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), `eth_chainId`)
			}).
			Reply(200).
			JSON([]byte(`{"result":"0x7b"}`)) // 123
	}

	rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)
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
		ctx,
		&log.Logger,
		"test",
		upstreamCfgs,
		ssr,
		rateLimitersRegistry,
		vr,
		pr,
		nil,
		metricsTracker,
		1*time.Second,
		nil,
		nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:                      123,
			LatestBlockGuaranteedMethods: guaranteedMethods,
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
	)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(150 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.GetInitializer().WaitForTasks(ctx))
	require.NoError(t, network.Bootstrap(ctx))
	time.Sleep(150 * time.Millisecond)

	upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.Len(t, upsList, len(upstreamCfgs))

	upstreams := make(map[string]*upstream.Upstream, len(upsList))
	for _, u := range upsList {
		upstreams[u.Id()] = u
	}
	for _, cfg := range upstreamCfgs {
		require.Contains(t, upstreams, cfg.Id, "missing upstream %q after bootstrap", cfg.Id)
	}

	return &guaranteeFixture{
		t:         t,
		ctx:       ctx,
		cancel:    cancel,
		network:   network,
		upstreams: upstreams,
	}
}

// withSyncedLatest sets a not-syncing state and a latest block on the upstream.
func (f *guaranteeFixture) withSyncedLatest(id string, block int64) {
	u := f.upstreams[id]
	u.EvmStatePoller().SetSyncingState(common.EvmSyncingStateNotSyncing)
	u.EvmStatePoller().SuggestLatestBlock(block)
}

// withSyncedFinalized sets a not-syncing state and a finalized block on the
// upstream. SuggestFinalizedBlock applies asynchronously, so this helper polls
// until the value is observable (or fails the test).
func (f *guaranteeFixture) withSyncedFinalized(id string, block int64) {
	u := f.upstreams[id]
	u.EvmStatePoller().SetSyncingState(common.EvmSyncingStateNotSyncing)
	u.EvmStatePoller().SuggestFinalizedBlock(block)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if u.EvmStatePoller().FinalizedBlock() >= block {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.GreaterOrEqualf(f.t, u.EvmStatePoller().FinalizedBlock(), block,
		"finalized block did not settle for upstream %q", id)
}

func (f *guaranteeFixture) withSyncing(id string) {
	f.upstreams[id].EvmStatePoller().SetSyncingState(common.EvmSyncingStateSyncing)
}

// makeEvmUpstream returns an UpstreamConfig with the given Id, allow/ignore method
// rules, and autoIgnoreUnsupportedMethods enabled by default (so dynamic
// IgnoreMethod() calls actually take effect).
func makeEvmUpstream(id string, allow, ignore []string) *common.UpstreamConfig {
	return &common.UpstreamConfig{
		Type:                         common.UpstreamTypeEvm,
		Id:                           id,
		Endpoint:                     "http://" + id + ".localhost",
		AllowMethods:                 allow,
		IgnoreMethods:                ignore,
		AutoIgnoreUnsupportedMethods: util.BoolPtr(true),
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}
}

// TestLatestBlockGuarantee_NoMethodsConfigured verifies the historical
// (unconstrained) behavior is preserved when LatestBlockGuaranteedMethods is empty.
func TestLatestBlockGuarantee_NoMethodsConfigured(t *testing.T) {
	f := newGuaranteeFixture(t,
		[]*common.UpstreamConfig{
			makeEvmUpstream("alice", nil, nil),
			makeEvmUpstream("bob", nil, nil),
		},
		nil, // no guarantee
	)
	f.withSyncedLatest("alice", 100)
	f.withSyncedLatest("bob", 95)

	assert.Equal(t, int64(100), f.network.EvmHighestLatestBlockNumber(f.ctx))
}

// TestLatestBlockGuarantee_AllSupportSingleMethod is the trivial guarantee case:
// every upstream supports the single guaranteed method, so the result equals the
// raw max.
func TestLatestBlockGuarantee_AllSupportSingleMethod(t *testing.T) {
	f := newGuaranteeFixture(t,
		[]*common.UpstreamConfig{
			makeEvmUpstream("alice", nil, nil),
			makeEvmUpstream("bob", nil, nil),
		},
		[]string{"eth_getBlockByNumber"},
	)
	f.withSyncedLatest("alice", 100)
	f.withSyncedLatest("bob", 95)

	assert.Equal(t, int64(100), f.network.EvmHighestLatestBlockNumber(f.ctx))
}

// TestLatestBlockGuarantee_CapabilityGapCapsLatest is the headline scenario:
// the upstream with the highest latest block does NOT support a guaranteed method,
// while a lower-block upstream does. The guarantee floor is the lower block.
func TestLatestBlockGuarantee_CapabilityGapCapsLatest(t *testing.T) {
	// alice is ahead but ignores trace_block. bob is behind but supports it.
	// Expected: latest = min(maxOver(eth_getBlockByNumber)=100, maxOver(trace_block)=95) = 95.
	f := newGuaranteeFixture(t,
		[]*common.UpstreamConfig{
			makeEvmUpstream("alice", nil, []string{"trace_block"}),
			makeEvmUpstream("bob", nil, nil),
		},
		[]string{"eth_getBlockByNumber", "trace_block"},
	)
	f.withSyncedLatest("alice", 100)
	f.withSyncedLatest("bob", 95)

	assert.Equal(t, int64(95), f.network.EvmHighestLatestBlockNumber(f.ctx))
}

// TestLatestBlockGuarantee_NoUpstreamSupportsMethod returns 0 when any guaranteed
// method has no supporting upstream. 0 is the sentinel "unknown" used elsewhere
// in the codebase so callers fall through to existing fallbacks.
func TestLatestBlockGuarantee_NoUpstreamSupportsMethod(t *testing.T) {
	f := newGuaranteeFixture(t,
		[]*common.UpstreamConfig{
			makeEvmUpstream("alice", nil, []string{"trace_block"}),
			makeEvmUpstream("bob", nil, []string{"trace_block"}),
		},
		[]string{"trace_block"},
	)
	f.withSyncedLatest("alice", 100)
	f.withSyncedLatest("bob", 95)

	assert.Equal(t, int64(0), f.network.EvmHighestLatestBlockNumber(f.ctx))
}

// TestLatestBlockGuarantee_SyncingUpstreamIsExcludedEvenWhenItSupports verifies
// that the existing "ignore syncing nodes" filter applies before method-support
// is considered. A syncing node with the highest block must not contribute.
func TestLatestBlockGuarantee_SyncingUpstreamIsExcludedEvenWhenItSupports(t *testing.T) {
	f := newGuaranteeFixture(t,
		[]*common.UpstreamConfig{
			makeEvmUpstream("alice", nil, nil), // would be the leader but is syncing
			makeEvmUpstream("bob", nil, nil),
		},
		[]string{"eth_getBlockByNumber"},
	)
	f.withSyncedLatest("alice", 999)
	f.withSyncedLatest("bob", 95)
	f.withSyncing("alice")

	assert.Equal(t, int64(95), f.network.EvmHighestLatestBlockNumber(f.ctx))
}

// TestLatestBlockGuarantee_IgnoreAllExceptIdiom verifies the canonical
// "this upstream only serves a narrow set of methods" idiom (ignoreMethods="*"
// rescued by allowMethods) is honored by the guarantee algorithm.
//
// Note: AllowMethods on its own does NOT act as a strict whitelist in this
// codebase — by design, it only "rescues" methods from IgnoreMethods. To
// express "only serve eth_getBlockByNumber" you must combine the two.
func TestLatestBlockGuarantee_IgnoreAllExceptIdiom(t *testing.T) {
	// alice declares she only supports eth_getBlockByNumber. bob supports all.
	// Even though alice has the higher block, the trace_block maximum across
	// supporting upstreams is bound by bob's 95.
	f := newGuaranteeFixture(t,
		[]*common.UpstreamConfig{
			makeEvmUpstream("alice", []string{"eth_getBlockByNumber"}, []string{"*"}),
			makeEvmUpstream("bob", nil, nil),
		},
		[]string{"eth_getBlockByNumber", "trace_block"},
	)
	f.withSyncedLatest("alice", 100)
	f.withSyncedLatest("bob", 95)

	assert.Equal(t, int64(95), f.network.EvmHighestLatestBlockNumber(f.ctx))
}

// TestLatestBlockGuarantee_DynamicIgnoreSelfHealing exercises the
// autoIgnoreUnsupportedMethods feedback loop: once an upstream is taught
// dynamically that it doesn't support a method, the guarantee algorithm must
// stop counting its blocks toward that method's max — without operator
// intervention.
func TestLatestBlockGuarantee_DynamicIgnoreSelfHealing(t *testing.T) {
	f := newGuaranteeFixture(t,
		[]*common.UpstreamConfig{
			// Both initially declare full support; alice will be taught dynamically.
			makeEvmUpstream("alice", nil, nil),
			makeEvmUpstream("bob", nil, nil),
		},
		[]string{"trace_block"},
	)
	f.withSyncedLatest("alice", 100)
	f.withSyncedLatest("bob", 95)

	// Before any feedback, alice (the leader) supports everything -> max = 100.
	assert.Equal(t, int64(100), f.network.EvmHighestLatestBlockNumber(f.ctx))

	// Simulate alice returning "method not supported" -> autoIgnore registers it.
	f.upstreams["alice"].IgnoreMethod("trace_block")

	// Now alice should be excluded from the trace_block calculation, so max = 95.
	assert.Equal(t, int64(95), f.network.EvmHighestLatestBlockNumber(f.ctx))
}

// TestLatestBlockGuarantee_ExtraMethodsUnionWithConfigured verifies the variadic
// extras compose with the configured set: the per-call `eth_getBlockByNumber`
// auto-injection in the eth_getBlockByNumber post-forward should compose with,
// not replace, any configured guaranteed methods.
func TestLatestBlockGuarantee_ExtraMethodsUnionWithConfigured(t *testing.T) {
	// Configured: trace_block. Extra (passed at call site): eth_getBlockByNumber.
	// alice supports eth_getBlockByNumber but not trace_block; carol supports both
	// but is behind. The min-of-max should be carol's block.
	f := newGuaranteeFixture(t,
		[]*common.UpstreamConfig{
			makeEvmUpstream("alice", nil, []string{"trace_block"}),
			makeEvmUpstream("carol", nil, nil),
		},
		[]string{"trace_block"},
	)
	f.withSyncedLatest("alice", 100)
	f.withSyncedLatest("carol", 90)

	// Without the extra, the min-of-max is bound by carol's trace_block contribution
	// (alice ignores trace_block) -> 90.
	assert.Equal(t, int64(90), f.network.EvmHighestLatestBlockNumber(f.ctx))

	// Extras don't lower the floor below what configured methods already give.
	assert.Equal(t, int64(90), f.network.EvmHighestLatestBlockNumber(f.ctx, "eth_getBlockByNumber"))
}

// TestLatestBlockGuarantee_ExtraMethodOnlyConstrainsWithoutConfig verifies the
// "self-reference" piece: even with no configured methods, passing
// "eth_getBlockByNumber" as an extra correctly excludes upstreams that ignore it.
func TestLatestBlockGuarantee_ExtraMethodOnlyConstrainsWithoutConfig(t *testing.T) {
	f := newGuaranteeFixture(t,
		[]*common.UpstreamConfig{
			makeEvmUpstream("alice", nil, []string{"eth_getBlockByNumber"}),
			makeEvmUpstream("bob", nil, nil),
		},
		nil, // no configured methods
	)
	f.withSyncedLatest("alice", 100)
	f.withSyncedLatest("bob", 95)

	// Unconstrained: alice still wins (her ignore of eth_getBlockByNumber doesn't
	// matter when no methods are required).
	assert.Equal(t, int64(100), f.network.EvmHighestLatestBlockNumber(f.ctx))

	// With the self-reference extra, alice must be excluded for that method.
	assert.Equal(t, int64(95), f.network.EvmHighestLatestBlockNumber(f.ctx, "eth_getBlockByNumber"))
}

// TestLatestBlockGuarantee_DuplicateExtraMethodIsDeduplicated guards against the
// trivial bug where passing the same method as an extra inflates the required-set.
func TestLatestBlockGuarantee_DuplicateExtraMethodIsDeduplicated(t *testing.T) {
	f := newGuaranteeFixture(t,
		[]*common.UpstreamConfig{
			makeEvmUpstream("alice", nil, []string{"eth_getBlockByNumber"}),
			makeEvmUpstream("bob", nil, nil),
		},
		[]string{"eth_getBlockByNumber"},
	)
	f.withSyncedLatest("alice", 100)
	f.withSyncedLatest("bob", 95)

	// Result should be the same whether we pass "eth_getBlockByNumber" as extra
	// or not, because it's already configured.
	withoutExtra := f.network.EvmHighestLatestBlockNumber(f.ctx)
	withExtra := f.network.EvmHighestLatestBlockNumber(f.ctx, "eth_getBlockByNumber")
	assert.Equal(t, withoutExtra, withExtra)
	assert.Equal(t, int64(95), withExtra)
}

// TestLatestBlockGuarantee_FinalizedBlock mirrors the latest-block tests for the
// finalized path so we know both code paths share the algorithm correctly.
func TestLatestBlockGuarantee_FinalizedBlock(t *testing.T) {
	f := newGuaranteeFixture(t,
		[]*common.UpstreamConfig{
			makeEvmUpstream("alice", nil, []string{"trace_block"}),
			makeEvmUpstream("bob", nil, nil),
		},
		[]string{"trace_block"},
	)
	f.withSyncedFinalized("alice", 90)
	f.withSyncedFinalized("bob", 80)

	// alice is excluded for trace_block, bob is the only contributor.
	assert.Equal(t, int64(80), f.network.EvmHighestFinalizedBlockNumber(f.ctx))
}

// TestLatestBlockGuarantee_ZeroBlocksAreIgnored guards a small bug class: an
// upstream that hasn't reported a block yet (effective block == 0) must not be
// counted as "supporting" the method at block 0 (which would then make any
// guarantee effectively zero).
func TestLatestBlockGuarantee_ZeroBlocksAreIgnored(t *testing.T) {
	f := newGuaranteeFixture(t,
		[]*common.UpstreamConfig{
			makeEvmUpstream("alice", nil, nil),
			makeEvmUpstream("bob", nil, nil),
		},
		[]string{"eth_getBlockByNumber"},
	)
	f.withSyncedLatest("alice", 100)
	// bob deliberately not given a latest block (stays at 0)
	f.upstreams["bob"].EvmStatePoller().SetSyncingState(common.EvmSyncingStateNotSyncing)

	// bob's 0 must not pull max down; alice carries the method.
	assert.Equal(t, int64(100), f.network.EvmHighestLatestBlockNumber(f.ctx))
}

// TestLatestBlockGuarantee_DefaultsPropagation verifies that
// networkDefaults.evm.latestBlockGuaranteedMethods flows into per-network
// EvmNetworkConfig when the network does not set its own.
func TestLatestBlockGuarantee_DefaultsPropagation(t *testing.T) {
	t.Run("default applies when network has no own list", func(t *testing.T) {
		network := &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
		}
		defaults := &common.NetworkDefaults{
			Evm: &common.EvmNetworkConfig{
				LatestBlockGuaranteedMethods: []string{"eth_getBlockReceipts", "trace_block"},
			},
		}
		require.NoError(t, network.SetDefaults(nil, defaults))
		assert.Equal(t, []string{"eth_getBlockReceipts", "trace_block"}, network.Evm.LatestBlockGuaranteedMethods)
	})

	t.Run("network-level setting wins over defaults", func(t *testing.T) {
		network := &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId:                      123,
				LatestBlockGuaranteedMethods: []string{"eth_getLogs"},
			},
		}
		defaults := &common.NetworkDefaults{
			Evm: &common.EvmNetworkConfig{
				LatestBlockGuaranteedMethods: []string{"eth_getBlockReceipts"},
			},
		}
		require.NoError(t, network.SetDefaults(nil, defaults))
		assert.Equal(t, []string{"eth_getLogs"}, network.Evm.LatestBlockGuaranteedMethods)
	})

	t.Run("defaults copy is independent from source", func(t *testing.T) {
		// Mutating the propagated list must not bleed back into the defaults
		// (which are shared across networks).
		defaultsList := []string{"eth_getBlockReceipts"}
		network := &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
		}
		defaults := &common.NetworkDefaults{
			Evm: &common.EvmNetworkConfig{LatestBlockGuaranteedMethods: defaultsList},
		}
		require.NoError(t, network.SetDefaults(nil, defaults))
		network.Evm.LatestBlockGuaranteedMethods[0] = "MUTATED"
		assert.Equal(t, "eth_getBlockReceipts", defaultsList[0], "default list must not be mutated through network ref")
	})
}
