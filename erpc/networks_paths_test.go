package erpc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/svm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pathsBareNetwork builds a Network with no registry, engine or tracker, the
// shape cache-only paths and partially initialized networks run with.
func pathsBareNetwork(cfg *common.NetworkConfig) *Network {
	if cfg == nil {
		cfg = &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
		}
	}
	return &Network{
		networkId:        cfg.NetworkId(),
		projectId:        "paths",
		cfg:              cfg,
		logger:           &log.Logger,
		inFlightRequests: &sync.Map{},
	}
}

// pathsFakeUpstream is a FakeUpstream with a fixed polled head.
func pathsFakeUpstream(id string, latest, finalized int64, opts ...func(*common.FakeUpstream)) common.Upstream {
	all := append([]func(*common.FakeUpstream){common.WithEvmStatePoller(common.NewFakeEvmStatePoller(latest, finalized))}, opts...)
	return common.NewFakeUpstream(id, all...)
}

func pathsIds(ups []common.Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Id()
	}
	return out
}

func pathsResult(t *testing.T, resp *common.NormalizedResponse) string {
	t.Helper()
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	return jrr.GetResultString()
}

// pathsCache is a CacheDAL whose Get answer is switched atomically: a nil
// mode is a miss, otherwise a fixed result or error. It is installed once,
// before any Forward, because Forward's async cache writes read the field.
type pathsCache struct {
	mode atomic.Pointer[pathsCacheMode]
}

type pathsCacheMode struct {
	result string
	err    error
}

func (c *pathsCache) Get(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	m := c.mode.Load()
	if m == nil {
		return nil, nil
	}
	if m.err != nil {
		return nil, m.err
	}
	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(m.result), nil)), nil
}

func (*pathsCache) Set(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) error {
	return nil
}

func (*pathsCache) IsObjectNull() bool { return false }

func TestNetworksPaths_IsSimpleGroupSelector(t *testing.T) {
	cases := []struct {
		sel  string
		want bool
	}{
		{"flashblocks*", true},
		{"!flashblocks*", true},
		{"  primary-*  ", true},
		{"", false},
		{"   ", false},
		{strings.Repeat("a", 129), false},
		{"a|b", false},
		{"a b", false},
		{"(a)", false},
		{"a&b", false},
		{"a,b", false},
		{"a!b", false},
		{"!!a", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, isSimpleGroupSelector(tc.sel), "selector %q", tc.sel)
	}
}

func TestNetworksPaths_EligibleLane(t *testing.T) {
	unb := func(id string) upstreamBlockBounds {
		return upstreamBlockBounds{id: id, min: math.MinInt64, max: math.MaxInt64}
	}
	cases := []struct {
		name   string
		bounds []upstreamBlockBounds
		bn     int64
		want   []string
	}{
		{"no bounds anywhere", []upstreamBlockBounds{unb("a"), unb("b")}, 100, nil},
		{"all eligible", []upstreamBlockBounds{unb("a"), {id: "b", min: 10, max: 200}}, 100, nil},
		{"below lower excluded", []upstreamBlockBounds{unb("a"), {id: "b", min: 150, max: math.MaxInt64}}, 100, []string{"a"}},
		{"above upper excluded", []upstreamBlockBounds{{id: "a", min: math.MinInt64, max: 50}, unb("b")}, 100, []string{"b"}},
		{"none eligible", []upstreamBlockBounds{{id: "a", min: 500, max: 600}}, 100, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, eligibleLane(tc.bounds, tc.bn))
		})
	}
}

func TestNetworksPaths_UpstreamOrderingHelpers(t *testing.T) {
	t.Run("TierUpstreamsByGroupMovesFallbacksLast", func(t *testing.T) {
		fb := common.WithTags(common.TagTierFallback)
		ups := []common.Upstream{
			pathsFakeUpstream("fb1", 1, 1, fb),
			pathsFakeUpstream("p1", 1, 1),
			pathsFakeUpstream("fb2", 1, 1, fb),
			pathsFakeUpstream("p2", 1, 1),
		}
		assert.Equal(t, []string{"p1", "p2", "fb1", "fb2"}, pathsIds(tierUpstreamsByGroup(ups)))

		plain := []common.Upstream{pathsFakeUpstream("p1", 1, 1), pathsFakeUpstream("p2", 1, 1)}
		assert.Equal(t, []string{"p1", "p2"}, pathsIds(tierUpstreamsByGroup(plain)))
	})

	t.Run("PartitionByLatestBlockMovesBehindHeadsLast", func(t *testing.T) {
		ups := []common.Upstream{
			pathsFakeUpstream("behind", 50, 1),
			pathsFakeUpstream("ahead", 200, 1),
			pathsFakeUpstream("unknown", 0, 0),
			pathsFakeUpstream("at", 150, 1),
		}
		nw := &Network{}
		assert.Equal(t, []string{"ahead", "unknown", "at", "behind"}, pathsIds(nw.partitionUpstreamsByLatestBlock(ups, 100)))
		assert.Equal(t, pathsIds(ups), pathsIds(nw.partitionUpstreamsByLatestBlock(ups, 0)), "no block keeps order")
		assert.Equal(t, []string{"behind"}, pathsIds(nw.partitionUpstreamsByLatestBlock(ups[:1], 100)))
		noPoller := []common.Upstream{common.NewFakeUpstream("np1"), common.NewFakeUpstream("np2")}
		assert.Equal(t, []string{"np1", "np2"}, pathsIds(nw.partitionUpstreamsByLatestBlock(noPoller, 100)))
	})

	t.Run("PreferTipLeaderForNearTipGetBlock", func(t *testing.T) {
		ups := []common.Upstream{
			pathsFakeUpstream("a", 100, 1),
			pathsFakeUpstream("leader", 105, 1),
			pathsFakeUpstream("c", 101, 1),
		}
		cases := []struct {
			name   string
			ups    []common.Upstream
			method string
			bn     int64
			want   []string
		}{
			{"head block", ups, "eth_getBlockByNumber", 105, []string{"leader", "a", "c"}},
			{"next block", ups, "eth_getBlockByNumber", 106, []string{"leader", "a", "c"}},
			{"beyond next", ups, "eth_getBlockByNumber", 107, []string{"a", "leader", "c"}},
			{"below tip", ups, "eth_getBlockByNumber", 104, []string{"a", "leader", "c"}},
			{"other method", ups, "eth_call", 105, []string{"a", "leader", "c"}},
			{"single upstream", ups[:1], "eth_getBlockByNumber", 100, []string{"a"}},
			{"leader already first", []common.Upstream{ups[1], ups[0]}, "eth_getBlockByNumber", 105, []string{"leader", "a"}},
			{"tie at tip", []common.Upstream{pathsFakeUpstream("x", 105, 1), pathsFakeUpstream("y", 105, 1)}, "eth_getBlockByNumber", 105, []string{"x", "y"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				assert.Equal(t, tc.want, pathsIds(preferTipLeaderForNearTipGetBlock(tc.ups, tc.method, tc.bn, highestOf(tc.ups))))
			})
		}
	})
}

func TestNetworksPaths_RequestBlockNumber(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, int64(0), requestBlockNumber(ctx, nil))

	cached := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	cached.SetEvmBlockNumber(int64(42))
	assert.Equal(t, int64(42), requestBlockNumber(ctx, cached))

	byParam := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x10",false]}`))
	assert.Equal(t, int64(16), requestBlockNumber(ctx, byParam))
}

func TestNetworksPaths_TipVoterRank(t *testing.T) {
	exactUpper := func(b int64) func(*common.FakeUpstream) {
		return func(u *common.FakeUpstream) {
			u.Config().Evm = &common.EvmUpstreamConfig{
				BlockAvailability: &common.EvmBlockAvailabilityConfig{
					Upper: &common.EvmAvailabilityBoundConfig{ExactBlock: &b},
				},
			}
		}
	}
	cases := []struct {
		name      string
		u         common.Upstream
		finalized bool
		head      int64
		rank      int
		ok        bool
	}{
		{"live head votes at rank 0", pathsFakeUpstream("a", 100, 90), false, 100, 0, true},
		{"finalized axis", pathsFakeUpstream("a", 100, 90), true, 90, 0, true},
		{"no poller never votes", common.NewFakeUpstream("np"), false, 0, 0, false},
		{"unknown head never votes", pathsFakeUpstream("z", 0, 0), false, 0, 0, false},
		{"exact cap below head ranks 2", pathsFakeUpstream("cap", 100, 90, exactUpper(50)), false, 100, 2, true},
		{"cap above finalized head does not cap", pathsFakeUpstream("cap", 100, 90, exactUpper(95)), true, 90, 0, true},
		{"cap without exact block", pathsFakeUpstream("cap", 100, 90, func(u *common.FakeUpstream) {
			u.Config().Evm = &common.EvmUpstreamConfig{BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Upper: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(0)},
			}}
		}), false, 100, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head, rank, ok := tipVoterRank(tc.u, tc.finalized, nil)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.head, head)
			assert.Equal(t, tc.rank, rank)
		})
	}
}

func TestNetworksPaths_BareNetworkAccessors(t *testing.T) {
	ctx := context.Background()

	t.Run("LabelPrefersAliasThenId", func(t *testing.T) {
		var nilNet *Network
		assert.Equal(t, "", nilNet.Label())
		n := pathsBareNetwork(nil)
		assert.Equal(t, "evm:123", n.Label())
		n.networkLabel = "mainnet"
		assert.Equal(t, "mainnet", n.Label())
		assert.Equal(t, "paths", n.ProjectId())
		assert.Same(t, n.cfg, n.Config())
		assert.Same(t, n.logger, n.Logger())
	})

	t.Run("ArchitectureDefaultsToEvmWhenEvmConfigured", func(t *testing.T) {
		n := pathsBareNetwork(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{ChainId: 1}})
		assert.Equal(t, common.ArchitectureEvm, n.Architecture())
		assert.Empty(t, n.cfg.Architecture, "a read never writes the live config, which trackers read concurrently")
		empty := pathsBareNetwork(&common.NetworkConfig{})
		assert.Equal(t, common.NetworkArchitecture(""), empty.Architecture())
	})

	t.Run("PolicyAccessorsWithoutEngine", func(t *testing.T) {
		n := pathsBareNetwork(nil)
		assert.Nil(t, n.PolicyScores(""))
		assert.Nil(t, n.RecentPolicyDecisions("", 10))
		assert.True(t, n.PolicyLastSwitchAt("").IsZero())
		assert.Nil(t, n.PolicyOrderedUpstreams(""))
		n.SetPolicyEnginePaused(true)
		n.SetPolicyStepLogEnabled(true)
		assert.Nil(t, n.policyEngine)
		require.NoError(t, n.Bootstrap(ctx), "bootstrap without an engine is a no-op")
	})

	t.Run("RegistryLessReadsFailOpen", func(t *testing.T) {
		n := pathsBareNetwork(nil)
		assert.Nil(t, n.MetricsTracker())
		assert.Equal(t, time.Duration(0), n.EvmBlockTime())
		assert.Nil(t, n.AllUpstreams())
		assert.Empty(t, n.EvmAllUpstreams(ctx))
		assert.Nil(t, n.GetMethodMetrics(""))
		head, known := laneCeilingOf(n)
		assert.False(t, known)
		assert.Equal(t, int64(0), head)
		var visited int
		n.walkTipCandidates("", func(common.Upstream) bool { visited++; return true })
		assert.Equal(t, 0, visited)
		key, ids := n.partitionKeyFor(ctx, "primary-*")
		assert.Equal(t, "", key)
		assert.Nil(t, ids)
		key, lane := n.EvmUpstreamGroupForSelector(ctx, "a|b")
		assert.Equal(t, "", key)
		assert.Equal(t, "", lane)
	})

	t.Run("ServedTipLaneForWithoutSelectorIsNetworkWide", func(t *testing.T) {
		n := pathsBareNetwork(nil)
		assert.Same(t, &n.servedTipAll, n.laneFor(requestSelector(ctx)))

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		req.SetDirectives(&common.RequestDirectives{UseUpstream: "primary-*"})
		rctx := context.WithValue(ctx, common.RequestContextKey, req)
		assert.Equal(t, "primary-*", requestSelector(rctx))
		assert.Nil(t, n.laneFor(requestSelector(rctx)), "a selector that forms no group gets no lane")
	})

	t.Run("RetryDistanceAndRegressionDefaults", func(t *testing.T) {
		n := pathsBareNetwork(nil)
		assert.Equal(t, defaultMaxRetryableBlockDistance, n.maxRetryableBlockDistance())
		n.cfg.Evm.MaxRetryableBlockDistance = i64(7)
		assert.Equal(t, int64(7), n.maxRetryableBlockDistance())

		assert.Equal(t, int64(common.DefaultToleratedBlockHeadRollback), n.servedTipMaxRegressionBlocks())
		n.cfg.Evm.ServedTip = &common.EvmServedTipConfig{MaxRegressionBlocks: -1}
		assert.Equal(t, int64(-1), n.servedTipMaxRegressionBlocks())
		n.cfg.Evm.ServedTip.MaxRegressionBlocks = 0
		assert.Equal(t, int64(common.DefaultToleratedBlockHeadRollback), n.servedTipMaxRegressionBlocks())
	})

	t.Run("SvmEnforceBlockAvailabilityDefaultsOn", func(t *testing.T) {
		n := pathsBareNetwork(nil)
		assert.True(t, n.SvmEnforceBlockAvailability())
		n.cfg.Svm = &common.SvmNetworkConfig{}
		assert.True(t, n.SvmEnforceBlockAvailability())
		n.cfg.Svm.EnforceBlockAvailability = util.BoolPtr(false)
		assert.False(t, n.SvmEnforceBlockAvailability())
		n.cfg.Svm.EnforceBlockAvailability = util.BoolPtr(true)
		assert.True(t, n.SvmEnforceBlockAvailability())
	})
}

func TestNetworksPaths_ShouldHandleMethod(t *testing.T) {
	n := pathsBareNetwork(&common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Methods: &common.MethodsConfig{Definitions: map[string]*common.CacheMethodConfig{
			"eth_newFilter": {Stateful: true},
		}},
	})
	two := []common.Upstream{common.NewFakeUpstream("a"), common.NewFakeUpstream("b")}
	newReq := func(method, useUpstream string) *common.NormalizedRequest {
		r := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, method)))
		if useUpstream != "" {
			r.SetDirectives(&common.RequestDirectives{UseUpstream: useUpstream})
		}
		return r
	}
	cases := []struct {
		name    string
		method  string
		use     string
		ups     []common.Upstream
		wantErr bool
		msg     string
	}{
		{"stateful across two upstreams", "eth_newFilter", "", two, true, "stateful method requires a single targeted upstream"},
		{"stateful pinned to one", "eth_newFilter", "a", two, false, ""},
		{"stateful selector matching both", "eth_newFilter", "*", two, true, "stateful method"},
		{"stateful with one upstream", "eth_newFilter", "", two[:1], false, ""},
		{"eth_accounts rejected", "eth_accounts", "", two[:1], true, "eth_accounts and eth_sign are not supported"},
		{"eth_sign any casing rejected", "ETH_SIGN", "", two[:1], true, "eth_accounts and eth_sign are not supported"},
		{"plain method passes", "eth_call", "", two, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := n.shouldHandleMethod(newReq(tc.method, tc.use), tc.method, tc.ups)
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeNotImplemented), "got %v", err)
			assert.Contains(t, err.Error(), tc.msg)
		})
	}
}

func TestNetworksPaths_GetFailsafeExecutor(t *testing.T) {
	mk := func(cfg *common.FailsafeConfig) *networkExecutor {
		ex, err := NewNetworkExecutor(cfg, &log.Logger, nil, nil)
		require.NoError(t, err)
		return ex
	}
	logs := mk(&common.FailsafeConfig{MatchMethod: "eth_getLogs"})
	finalizedOnly := mk(&common.FailsafeConfig{MatchMethod: "eth_call", MatchFinality: []common.DataFinalityState{common.DataFinalityStateFinalized}})
	svmFinalized := mk(&common.FailsafeConfig{MatchMethod: "eth_call", MatchCommitment: []string{"finalized"}})
	catchAll := mk(nil)

	n := pathsBareNetwork(nil)
	n.failsafeExecutors = []*networkExecutor{logs, finalizedOnly, svmFinalized, catchAll}
	ctx := context.Background()
	newReq := func(method string) *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, method)))
	}

	assert.Same(t, logs, n.getFailsafeExecutor(ctx, newReq("eth_getLogs")))
	assert.Same(t, catchAll, n.getFailsafeExecutor(ctx, newReq("eth_call")), "finality and commitment filters skip")

	n.failsafeExecutors = []*networkExecutor{logs}
	assert.Nil(t, n.getFailsafeExecutor(ctx, newReq("eth_call")), "no matching executor")
}

func TestNetworksPaths_PrepareRequest(t *testing.T) {
	ctx := context.Background()
	bad := func() *common.NormalizedRequest { return common.NewNormalizedRequest([]byte(`not-json`)) }
	good := func() *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[]}`))
	}

	evmNet := pathsBareNetwork(nil)
	err := evmNet.prepareRequest(ctx, bad())
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcExceptionInternal), "got %v", err)
	assert.Contains(t, err.Error(), "failed to unmarshal json-rpc request")

	svmNet := pathsBareNetwork(&common.NetworkConfig{Architecture: common.ArchitectureSvm, Svm: &common.SvmNetworkConfig{Cluster: "mainnet-beta"}})
	err = svmNet.prepareRequest(ctx, bad())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to unmarshal json-rpc request")
	require.NoError(t, svmNet.prepareRequest(ctx, good()))

	other := pathsBareNetwork(&common.NetworkConfig{Architecture: "cosmos"})
	err = other.prepareRequest(ctx, good())
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcExceptionInternal), "got %v", err)
	assert.Contains(t, err.Error(), "unsupported architecture: cosmos")
}

func TestNetworksPaths_GetFinalityWithoutRegistry(t *testing.T) {
	ctx := context.Background()
	n := pathsBareNetwork(&common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Methods: &common.MethodsConfig{Definitions: map[string]*common.CacheMethodConfig{
			"custom_finalized": {Finalized: true},
			"custom_realtime":  {Realtime: true},
		}},
	})
	newReq := func(body string) *common.NormalizedRequest { return common.NewNormalizedRequest([]byte(body)) }

	assert.Equal(t, common.DataFinalityStateUnknown, n.GetFinality(ctx, nil, nil))
	assert.Equal(t, common.DataFinalityStateFinalized,
		n.GetFinality(ctx, newReq(`{"jsonrpc":"2.0","id":1,"method":"custom_finalized","params":[]}`), nil))
	assert.Equal(t, common.DataFinalityStateRealtime,
		n.GetFinality(ctx, newReq(`{"jsonrpc":"2.0","id":1,"method":"custom_realtime","params":[]}`), nil))
	assert.Equal(t, common.DataFinalityStateRealtime,
		n.GetFinality(ctx, newReq(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`), nil),
		"a tag reference is realtime")

	numbered := `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x10",false]}`
	assert.Equal(t, common.DataFinalityStateUnknown, n.GetFinality(ctx, newReq(numbered), nil),
		"no registry: the Head is unknown, so is finality")

	viaLast := newReq(numbered)
	viaLast.SetLastUpstream(pathsFakeUpstream("last", 100, 90))
	assert.Equal(t, common.DataFinalityStateUnknown, n.GetFinality(ctx, viaLast, nil),
		"an upstream's own finalized never stands in for an unknown Head.Finalized")

	// The voters give Head.Finalized 90 (spec §6.4).
	n.servedTipAll.head.Store(&common.EvmHead{Latest: 100, Finalized: 90})
	assert.Equal(t, common.DataFinalityStateFinalized, n.GetFinality(ctx, newReq(numbered), nil),
		"a read at or below Head.Finalized is finalized")

	viaResp := newReq(numbered)
	resp := common.NewNormalizedResponse().WithRequest(viaResp)
	resp.SetUpstream(pathsFakeUpstream("resp", 100, 90))
	assert.Equal(t, common.DataFinalityStateUnfinalized, n.GetFinality(ctx, viaResp, resp),
		"an answer is final only when its upstream has finalized the block too")

	svmNet := pathsBareNetwork(&common.NetworkConfig{Architecture: common.ArchitectureSvm, Svm: &common.SvmNetworkConfig{Cluster: "mainnet-beta"}})
	assert.Equal(t, common.DataFinalityStateRealtime,
		svmNet.GetFinality(ctx, newReq(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["pk"]}`), nil))
	assert.Equal(t, common.DataFinalityStateFinalized,
		svmNet.GetFinality(ctx, newReq(`{"jsonrpc":"2.0","id":1,"method":"getBlockTime","params":[5]}`), nil))
}

func TestNetworksPaths_EnrichStatePoller(t *testing.T) {
	ctx := context.Background()
	n := pathsBareNetwork(nil)

	run := func(t *testing.T, body string, blockRef interface{}, result string) common.EvmStatePoller {
		t.Helper()
		req := common.NewNormalizedRequest([]byte(body))
		if blockRef != nil {
			req.SetEvmBlockRef(blockRef)
		}
		u := pathsFakeUpstream("u", 100, 90)
		resp := common.NewNormalizedResponse().WithRequest(req).
			WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(result), nil))
		resp.SetUpstream(u)
		resp.SetSentAt(time.Now())
		method, _ := req.Method()
		n.enrichStatePoller(ctx, method, req, resp)
		return u.(common.EvmUpstream).EvmStatePoller()
	}

	t.Run("LatestTagFromParamsSuggestsLatest", func(t *testing.T) {
		sp := run(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`, nil, `{"number":"0xc8"}`)
		assert.Equal(t, int64(200), sp.LatestBlock())
		assert.Equal(t, int64(90), sp.FinalizedBlock())
	})
	t.Run("InterpolatedFinalizedTagDoesNotSuggest", func(t *testing.T) {
		// The request asked for Head.Finalized, eRPC's number: serving it
		// says nothing of what this upstream finalized.
		sp := run(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x5f",false]}`, "finalized", `{"number":"0x5f"}`)
		assert.Equal(t, int64(90), sp.FinalizedBlock())
		assert.Equal(t, int64(100), sp.LatestBlock())
	})
	t.Run("LiteralFinalizedTagSuggestsFinalized", func(t *testing.T) {
		sp := run(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["finalized",false]}`, "finalized", `{"number":"0x5f"}`)
		assert.Equal(t, int64(95), sp.FinalizedBlock())
		assert.Equal(t, int64(100), sp.LatestBlock())
	})
	t.Run("NumberedBlockIsIgnored", func(t *testing.T) {
		sp := run(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x500",false]}`, nil, `{"number":"0x500"}`)
		assert.Equal(t, int64(100), sp.LatestBlock())
	})
	t.Run("MissingNumberIsIgnored", func(t *testing.T) {
		sp := run(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`, nil, `{"hash":"0x1"}`)
		assert.Equal(t, int64(100), sp.LatestBlock())
	})
	t.Run("BadNumberIsIgnored", func(t *testing.T) {
		sp := run(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`, nil, `{"number":"zz"}`)
		assert.Equal(t, int64(100), sp.LatestBlock())
	})
	t.Run("BlockNumberSuggestsLatest", func(t *testing.T) {
		sp := run(t, `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`, nil, `"0x12c"`)
		assert.Equal(t, int64(300), sp.LatestBlock())
	})
}

func TestNetworksPaths_StampClientResponseID(t *testing.T) {
	ctx := context.Background()
	n := pathsBareNetwork(nil)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":"abc","method":"eth_chainId","params":[]}`))
	require.NoError(t, n.normalizeResponse(ctx, req, nil))

	resp := common.NewNormalizedResponse().WithRequest(req).
		WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte(`999`), []byte(`"0x1"`), nil))
	require.NoError(t, n.normalizeResponse(ctx, req, resp))
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, "abc", jrr.ID(), "the client's id replaces the upstream's")
}

func TestNetworksPaths_AcquireRateLimitPermit(t *testing.T) {
	ctx := context.Background()
	budgetId := fmt.Sprintf("paths-net-budget-%d", time.Now().UnixNano())
	rlr, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{{
			Id: budgetId,
			Rules: []*common.RateLimitRuleConfig{{
				Method:   "eth_getBalance",
				MaxCount: 1,
				Period:   common.RateLimitPeriodMinute,
			}},
		}},
	}, &log.Logger)
	require.NoError(t, err)
	n := pathsBareNetwork(nil)
	n.rateLimitersRegistry = rlr
	newReq := func(method string) *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, method)))
	}

	require.NoError(t, n.acquireRateLimitPermit(ctx, newReq("eth_getBalance")), "no budget configured")

	n.cfg.RateLimitBudget = "paths-missing-budget"
	err = n.acquireRateLimitPermit(ctx, newReq("eth_getBalance"))
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, "ErrRateLimitBudgetNotFound"), "got %v", err)

	n.cfg.RateLimitBudget = budgetId
	require.NoError(t, n.acquireRateLimitPermit(ctx, newReq("eth_chainId")), "method without rules passes")
}

func TestNetworksPaths_MultiplexKey(t *testing.T) {
	ctx := context.Background()
	lower := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["dRiFTyPePEHfLqZBUyHFLGVW3d5Fk1AmmZoRbCxDMDy"]}`))
	upper := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"getBalance","params":["DRiFTyPePEHfLqZBUyHFLGVW3d5Fk1AmmZoRbCxDMDy"]}`))

	svmNet := pathsBareNetwork(&common.NetworkConfig{Architecture: common.ArchitectureSvm, Svm: &common.SvmNetworkConfig{Cluster: "mainnet-beta"}})
	lk, err := svmNet.multiplexKey(ctx, lower)
	require.NoError(t, err)
	uk, err := svmNet.multiplexKey(ctx, upper)
	require.NoError(t, err)
	assert.NotEqual(t, lk, uk, "case-distinct base58 keys never share a multiplexer on SVM")

	evmNet := pathsBareNetwork(nil)
	ek1, err := evmNet.multiplexKey(ctx, lower)
	require.NoError(t, err)
	ek2, err := evmNet.multiplexKey(ctx, upper)
	require.NoError(t, err)
	assert.Equal(t, ek1, ek2, "EVM keeps the case-insensitive cache hash")

	call := func(use string) *common.NormalizedRequest {
		r := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xca11bde05977b3631167028862be2a173976ca11","data":"0x42cbb15c"},"latest"]}`))
		if use != "" {
			r.SetDirectives(&common.RequestDirectives{UseUpstream: use})
		}
		return r
	}
	keyOf := func(r *common.NormalizedRequest) string {
		k, err := evmNet.multiplexKey(ctx, r)
		require.NoError(t, err)
		return k
	}
	routed, pinnedA, pinnedA2, pinnedB := keyOf(call("")), keyOf(call("rpc-a")), keyOf(call("rpc-a")), keyOf(call("rpc-b"))
	assert.NotEqual(t, routed, pinnedA, "a request pinned to one upstream never shares with one routed by policy")
	assert.NotEqual(t, pinnedA, pinnedB, "requests pinned to different upstreams never share")
	assert.Equal(t, pinnedA, pinnedA2, "requests pinned alike still share")
}

func TestNetworksPaths_Multiplexing(t *testing.T) {
	lg := log.Logger
	body := func(id int, addr string) []byte {
		return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBalance","params":[%q,"latest"]}`, id, addr))
	}

	t.Run("DisabledAndWriteMethodsNeverMultiplex", func(t *testing.T) {
		ctx := context.Background()
		n := pathsBareNetwork(nil)
		n.cfg.Multiplexing = util.BoolPtr(false)
		mlx, resp, err := n.handleMultiplexing(ctx, &lg, common.NewNormalizedRequest(body(1, "0xa")), time.Now())
		assert.Nil(t, mlx)
		assert.Nil(t, resp)
		assert.NoError(t, err)

		n.cfg.Multiplexing = nil
		write := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_newFilter","params":[{}]}`))
		mlx, resp, err = n.handleMultiplexing(ctx, &lg, write, time.Now())
		assert.Nil(t, mlx)
		assert.Nil(t, resp)
		assert.NoError(t, err)

		mlx, resp, err = n.handleMultiplexing(ctx, &lg, common.NewNormalizedRequest([]byte(`not-json`)), time.Now())
		assert.Nil(t, mlx, "no hash means no multiplexer")
		assert.Nil(t, resp)
		assert.NoError(t, err)
	})

	t.Run("FollowerCopiesLeaderResultThenCleanupReleases", func(t *testing.T) {
		ctx := context.Background()
		n := pathsBareNetwork(nil)
		leaderReq := common.NewNormalizedRequest(body(1, "0xb"))
		mlx, resp, err := n.handleMultiplexing(ctx, &lg, leaderReq, time.Now())
		require.NoError(t, err)
		require.Nil(t, resp)
		require.NotNil(t, mlx, "first request leads")

		leaderResp := common.NewNormalizedResponse().WithRequest(leaderReq).
			WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0xbeef"`), nil))
		mlx.Close(ctx, leaderResp, nil)

		followerReq := common.NewNormalizedRequest(body(2, "0xb"))
		fmlx, fresp, ferr := n.handleMultiplexing(ctx, &lg, followerReq, time.Now())
		require.NoError(t, ferr)
		assert.Nil(t, fmlx, "a follower does not lead")
		assert.Equal(t, `"0xbeef"`, pathsResult(t, fresp))

		n.cleanupMultiplexer(mlx)
		_, stillThere := n.inFlightRequests.Load(mlx.hash)
		assert.False(t, stillThere, "cleanup removes the in-flight entry")
		mlx.mu.RLock()
		assert.Nil(t, mlx.resp, "cleanup releases the leader response")
		mlx.mu.RUnlock()

		again, _, err := n.handleMultiplexing(ctx, &lg, common.NewNormalizedRequest(body(3, "0xb")), time.Now())
		require.NoError(t, err)
		require.NotNil(t, again, "after cleanup the next identical request leads")
		again.Close(ctx, nil, errors.New("done"))
		n.cleanupMultiplexer(again)
	})

	t.Run("FollowerSharesLeaderError", func(t *testing.T) {
		ctx := context.Background()
		n := pathsBareNetwork(nil)
		mlx, _, err := n.handleMultiplexing(ctx, &lg, common.NewNormalizedRequest(body(1, "0xc")), time.Now())
		require.NoError(t, err)
		require.NotNil(t, mlx)
		leaderErr := common.NewErrNoUpstreamsFound("paths", "evm:123")
		mlx.Close(ctx, nil, leaderErr)

		_, resp, ferr := n.handleMultiplexing(ctx, &lg, common.NewNormalizedRequest(body(2, "0xc")), time.Now())
		assert.Nil(t, resp)
		require.Error(t, ferr)
		assert.True(t, common.HasErrorCode(ferr, "ErrNoUpstreamsFound"), "got %v", ferr)
		n.cleanupMultiplexer(mlx)
	})

	t.Run("FollowerDeadlineAndCancel", func(t *testing.T) {
		n := pathsBareNetwork(nil)
		mlx, _, err := n.handleMultiplexing(context.Background(), &lg, common.NewNormalizedRequest(body(1, "0xd")), time.Now())
		require.NoError(t, err)
		require.NotNil(t, mlx)

		tctx, tcancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer tcancel()
		_, resp, ferr := n.handleMultiplexing(tctx, &lg, common.NewNormalizedRequest(body(2, "0xd")), time.Now())
		assert.Nil(t, resp)
		assert.True(t, common.HasErrorCode(ferr, common.ErrCodeNetworkRequestTimeout), "got %v", ferr)

		cctx, ccancel := context.WithCancel(context.Background())
		ccancel()
		_, resp, ferr = n.handleMultiplexing(cctx, &lg, common.NewNormalizedRequest(body(3, "0xd")), time.Now())
		assert.Nil(t, resp)
		assert.ErrorIs(t, ferr, context.Canceled)

		mlx.Close(context.Background(), nil, errors.New("done"))
		n.cleanupMultiplexer(mlx)
	})
}

func TestNetworksPaths_SvmDeclaredSlotAvailability(t *testing.T) {
	ctx := context.Background()
	n := pathsBareNetwork(&common.NetworkConfig{Architecture: common.ArchitectureSvm, Svm: &common.SvmNetworkConfig{Cluster: "mainnet-beta"}})
	withBounds := func(lower, upper *int64) common.Upstream {
		return common.NewFakeUpstream("svm-u", func(u *common.FakeUpstream) {
			ba := &common.SvmBlockAvailabilityConfig{}
			if lower != nil {
				ba.Lower = &common.SvmAvailabilityBoundConfig{ExactSlot: lower}
			}
			if upper != nil {
				ba.Upper = &common.SvmAvailabilityBoundConfig{ExactSlot: upper}
			}
			u.Config().Svm = &common.SvmUpstreamConfig{Cluster: "mainnet-beta", BlockAvailability: ba}
		})
	}
	getBlock := func(slot int64) *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[%d]}`, slot)))
	}

	err, retryable := n.checkUpstreamBlockAvailability(ctx, withBounds(i64(100), nil), getBlock(50), "getBlock")
	require.Error(t, err, "below the declared lower bound skips")
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData), "got %v", err)
	assert.False(t, retryable)
	assert.False(t, svm.IsDeclaredUpperBoundSkip(err))

	err, retryable = n.checkUpstreamBlockAvailability(ctx, withBounds(nil, i64(10)), getBlock(50), "getBlock")
	require.Error(t, err, "above a fixed upper bound skips")
	assert.True(t, svm.IsDeclaredUpperBoundSkip(err))
	assert.False(t, retryable, "a fixed exactSlot upper never becomes retryable")

	err, retryable = n.checkUpstreamBlockAvailability(ctx, withBounds(i64(10), i64(100)), getBlock(50), "getBlock")
	assert.NoError(t, err, "inside the window serves")
	assert.False(t, retryable)

	n.cfg.Svm.EnforceBlockAvailability = util.BoolPtr(false)
	err, _ = n.checkUpstreamBlockAvailability(ctx, withBounds(i64(100), nil), getBlock(50), "getBlock")
	assert.NoError(t, err, "the network switch disables declared bounds")

	other := pathsBareNetwork(&common.NetworkConfig{Architecture: "cosmos"})
	err, retryable = other.checkUpstreamBlockAvailability(ctx, withBounds(i64(100), nil), getBlock(50), "getBlock")
	assert.NoError(t, err)
	assert.False(t, retryable)
}

func TestNetworksPaths_BoundaryLaneAndFutureBlockGuards(t *testing.T) {
	ctx := context.Background()
	newReq := func(body string) *common.NormalizedRequest { return common.NewNormalizedRequest([]byte(body)) }

	svmNet := pathsBareNetwork(&common.NetworkConfig{Architecture: common.ArchitectureSvm, Svm: &common.SvmNetworkConfig{Cluster: "mainnet-beta"}})
	assert.Nil(t, svmNet.eligibleUpstreamIDsForBoundary(ctx, "getBlock", newReq(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[5]}`)))

	n := pathsBareNetwork(nil)
	assert.Nil(t, n.eligibleUpstreamIDsForBoundary(ctx, "eth_getLogs", newReq(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{}]}`)),
		"range methods own their availability")

	future := newReq(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0xffffff",false]}`)
	resp, ok := n.tryShortCircuitFutureBlock(ctx, future, "eth_getBlockByNumber")
	assert.False(t, ok, "disabled by default")
	assert.Nil(t, resp)

	n.cfg.Evm.ShortCircuitFutureBlocks = util.BoolPtr(true)
	_, ok = n.tryShortCircuitFutureBlock(ctx, future, "eth_call")
	assert.False(t, ok, "only eth_getBlockByNumber")
	_, ok = n.tryShortCircuitFutureBlock(ctx, newReq(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`), "eth_getBlockByNumber")
	assert.False(t, ok, "tags never short-circuit")
	_, ok = n.tryShortCircuitFutureBlock(ctx, future, "eth_getBlockByNumber")
	assert.False(t, ok, "unknown heads fail open")
}

func TestNetworksPaths_ForwardOnLiveNetwork(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xupstream"}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetworkSimple(t, ctx, nil, &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123, ShortCircuitFutureBlocks: util.BoolPtr(true)},
	})

	cache := &pathsCache{}
	network.cacheDal = cache

	head, known := laneCeilingOf(network)
	require.True(t, known, "the single upstream's head is known")
	require.Greater(t, head, int64(0))

	forward := func(body string) (*common.NormalizedResponse, error) {
		req := common.NewNormalizedRequest([]byte(body))
		req.SetNetwork(network)
		return network.Forward(ctx, req)
	}

	t.Run("RegistryAccessors", func(t *testing.T) {
		all := network.AllUpstreams()
		require.Len(t, all, 1)
		assert.Equal(t, "test", all[0].Id())
		assert.Equal(t, []string{"test"}, pathsIds(network.EvmAllUpstreams(ctx)))
		assert.Empty(t, network.ShadowUpstreams())
		leader := servedHead(network, ctx).Leader
		require.NotNil(t, leader)
		assert.Equal(t, "test", leader.Id())
		assert.NotNil(t, network.MetricsTracker())
		network.EvmRefreshHead(ctx, false)
		assert.Equal(t, head, servedHead(network, ctx).Latest, "one voter: the served tip is its head")
	})

	t.Run("FinalityFromHeadFinalized", func(t *testing.T) {
		fin := servedHead(network, ctx).Finalized
		require.Greater(t, fin, int64(0))
		at := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x%x",false]}`, fin)))
		assert.Equal(t, common.DataFinalityStateFinalized, network.GetFinality(ctx, at, nil))
		above := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x%x",false]}`, fin+1)))
		assert.Equal(t, common.DataFinalityStateUnfinalized, network.GetFinality(ctx, above, nil))
	})

	t.Run("FutureBlockShortCircuitsToNull", func(t *testing.T) {
		sampleBlockTime(t, network)
		resp, err := forward(fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"eth_getBlockByNumber","params":["0x%x",false]}`, head+100000))
		require.NoError(t, err)
		assert.Equal(t, "null", pathsResult(t, resp), "a block beyond every head is null without dispatch")
		assert.Equal(t, head+100000, resp.EvmBlockNumber())

		within := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x%x",false]}`, head)))
		_, ok := network.tryShortCircuitFutureBlock(ctx, within, "eth_getBlockByNumber")
		assert.False(t, ok, "a block at the head is dispatched")
	})

	t.Run("UnsupportedMethodRejectedBeforeDispatch", func(t *testing.T) {
		_, err := forward(`{"jsonrpc":"2.0","id":1,"method":"eth_accounts","params":[]}`)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeNotImplemented), "got %v", err)
	})

	t.Run("MissingRateLimitBudgetFailsForward", func(t *testing.T) {
		network.cfg.RateLimitBudget = "paths-no-such-budget"
		defer func() { network.cfg.RateLimitBudget = "" }()
		_, err := forward(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x1","0x1"]}`)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, "ErrRateLimitBudgetNotFound"), "got %v", err)
	})

	t.Run("CacheHitServedWithoutUpstream", func(t *testing.T) {
		cache.mode.Store(&pathsCacheMode{result: `"0xcached"`})
		defer cache.mode.Store(nil)
		resp, err := forward(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x2","0x1"]}`)
		require.NoError(t, err)
		assert.Equal(t, `"0xcached"`, pathsResult(t, resp))
	})

	t.Run("CacheErrorFallsThroughToUpstream", func(t *testing.T) {
		cache.mode.Store(&pathsCacheMode{err: errors.New("cache down")})
		defer cache.mode.Store(nil)
		resp, err := forward(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x3","0x1"]}`)
		require.NoError(t, err)
		assert.Equal(t, `"0xupstream"`, pathsResult(t, resp))
	})

	t.Run("StaticResponseServedWithoutUpstream", func(t *testing.T) {
		network.cfg.StaticResponses = []*common.StaticResponseConfig{{
			Method:   "eth_getBalance",
			Params:   []interface{}{"0x4", "0x1"},
			Response: &common.StaticResponseBodyConfig{Result: "0xstatic"},
		}}
		defer func() { network.cfg.StaticResponses = nil }()
		resp, err := forward(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x4","0x1"]}`)
		require.NoError(t, err)
		assert.Equal(t, `"0xstatic"`, pathsResult(t, resp))
	})
}

func TestNetworksPaths_PolicyAccessorsAndGroupLanes(t *testing.T) {
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{primaryLatest: "0x3e8", fallbackLatest: "0x3e8"})
	require.NotNil(t, network.policyEngine)
	ids := []string{"primary-1", "primary-2", "fallback-1", "fallback-2"}

	t.Run("PolicyReadouts", func(t *testing.T) {
		ordered := network.PolicyOrderedUpstreams("")
		require.NotEmpty(t, ordered)
		assert.Subset(t, ids, ordered)
		assert.Contains(t, ordered, "primary-1")
		assert.Equal(t, ordered, network.PolicyOrderedUpstreams("*"))
		assert.ElementsMatch(t, ordered, network.PolicyOrderedUpstreams("eth_neverTicked"), "an un-ticked method falls back to the wildcard slot")

		decisions := network.RecentPolicyDecisions("", 10)
		assert.NotEmpty(t, decisions, "the bootstrap tick records a decision")
		assert.Len(t, network.RecentPolicyDecisions("", 1), 1, "the limit caps the history")

		for id := range network.PolicyScores("") {
			assert.Contains(t, ids, id)
		}
		assert.False(t, network.PolicyLastSwitchAt("").After(time.Now()))
	})

	t.Run("PauseAndStepLogToggleEngine", func(t *testing.T) {
		network.SetPolicyEnginePaused(true)
		assert.True(t, network.policyEngine.IsPaused())
		network.SetPolicyEnginePaused(false)
		assert.False(t, network.policyEngine.IsPaused())
		network.SetPolicyStepLogEnabled(true)
		assert.True(t, network.policyEngine.IsStepLogEnabled())
		network.SetPolicyStepLogEnabled(false)
		assert.False(t, network.policyEngine.IsStepLogEnabled())
	})

	t.Run("GroupForSelector", func(t *testing.T) {
		key, lane := network.EvmUpstreamGroupForSelector(ctx, "primary-*")
		assert.True(t, strings.HasPrefix(key, "grp:"), "got %q", key)
		assert.Equal(t, "primary", lane)
		key2, _ := network.EvmUpstreamGroupForSelector(ctx, "primary-*")
		assert.Equal(t, key, key2, "the key is stable")
		fkey, flane := network.EvmUpstreamGroupForSelector(ctx, "fallback-*")
		assert.NotEqual(t, key, fkey)
		assert.Equal(t, "fallback", flane)

		for _, sel := range []string{"*", "primary-1", "nothing-*", "a|b"} {
			k, l := network.EvmUpstreamGroupForSelector(ctx, sel)
			assert.Equal(t, "", k, "selector %q forms no group", sel)
			assert.Equal(t, "", l)
		}
		k, l := network.EvmUpstreamGroupForSelector(ctx, "*-1")
		assert.True(t, strings.HasPrefix(k, "grp:"))
		assert.Equal(t, "1", l, "the shared id token names the lane")
	})

	t.Run("ServedTipLanesMaterializeOncePerGroupUpToCap", func(t *testing.T) {
		laneCtx := func(sel string) context.Context {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
			req.SetDirectives(&common.RequestDirectives{UseUpstream: sel})
			return context.WithValue(ctx, common.RequestContextKey, req)
		}
		first := network.laneFor(requestSelector(laneCtx("primary-*")))
		require.NotNil(t, first)
		assert.Equal(t, "primary", first.label)
		assert.Same(t, first, network.laneFor(requestSelector(laneCtx("primary-*"))), "a group reuses its lane")
		assert.Nil(t, network.laneFor(requestSelector(laneCtx("primary-1"))), "a single upstream is no group")

		count := network.servedTipLaneCount.Load()
		network.servedTipLaneCount.Store(maxServedTipLanes)
		assert.Nil(t, network.laneFor(requestSelector(laneCtx("fallback-*"))), "beyond the cap a new group gets no lane")
		assert.Same(t, first, network.laneFor(requestSelector(laneCtx("primary-*"))), "existing lanes still resolve at the cap")
		network.servedTipLaneCount.Store(count)
	})
}

func TestNetworksPaths_SvmSlotReadouts(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	const host = "svm-paths-rpc1.localhost"
	util.SetupMocksForSvmStatePollerWithShred(host, 1000, 990, 1005)
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return r.URL.Host == host && strings.Contains(util.SafeReadBody(r), `"method":"getAccountInfo"`)
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1000},"value":{"lamports":7}}}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{svmUpstreamConfig("rpc1", host)})

	require.Eventually(t, func() bool {
		return net.SvmHighestLatestSlot(ctx) == 1000 && net.SvmHighestIndexedSlot(ctx) == 1005
	}, 5*time.Second, 20*time.Millisecond)
	assert.Equal(t, int64(990), net.SvmHighestFinalizedSlot(ctx))
	assert.Equal(t, int64(990), net.SvmHighestFinalizedSlotMax(ctx))

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey",{"minContextSlot":500}]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err)
	assert.Contains(t, pathsResult(t, resp), `"lamports":7`, "an upstream past minContextSlot serves")
}
