package erpc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	policystdlib "github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilterMethodEligible(t *testing.T) {
	full1 := common.NewFakeUpstream("full-1")
	full2 := common.NewFakeUpstream("full-2")
	archive1 := common.NewFakeUpstream("archive-1")
	archive1.Config().IgnoreMethods = []string{"eth_send*"}
	archive2 := common.NewFakeUpstream("archive-2")
	archive2.Config().IgnoreMethods = []string{"*"}

	t.Run("drops ineligible upstreams preserving order", func(t *testing.T) {
		ups := []common.Upstream{archive1, full1, archive2, full2}
		eligible, dropped := filterMethodEligible(ups, "eth_sendRawTransaction")
		require.Equal(t, 2, dropped)
		require.Len(t, eligible, 2)
		assert.Equal(t, "full-1", eligible[0].Id())
		assert.Equal(t, "full-2", eligible[1].Id())
	})

	t.Run("returns input slice untouched when all are eligible", func(t *testing.T) {
		ups := []common.Upstream{full1, archive1, full2}
		eligible, dropped := filterMethodEligible(ups, "eth_getLogs")
		assert.Equal(t, 0, dropped)
		// Same backing array, no copy: the selection-policy cache slice is
		// passed through as-is on the hot path.
		assert.Equal(t, &ups[0], &eligible[0])
		assert.Len(t, eligible, 3)
	})

	t.Run("all ineligible reports full drop", func(t *testing.T) {
		ups := []common.Upstream{archive1, archive2}
		eligible, dropped := filterMethodEligible(ups, "eth_sendRawTransaction")
		assert.Equal(t, 2, dropped)
		assert.Empty(t, eligible)
		// The Forward call-site keeps the original list in this case so the
		// request still fails with the descriptive per-upstream
		// "method ignored" error rather than "no upstreams found".
	})

	t.Run("does not mutate the input slice", func(t *testing.T) {
		ups := []common.Upstream{full1, archive1, full2}
		eligible, dropped := filterMethodEligible(ups, "eth_sendRawTransaction")
		require.Equal(t, 1, dropped)
		require.Len(t, eligible, 2)
		assert.Equal(t, "full-1", ups[0].Id())
		assert.Equal(t, "archive-1", ups[1].Id())
		assert.Equal(t, "full-2", ups[2].Id())
	})
}

// A stateful method that no targeted upstream serves (e.g. filter methods left
// out of every allowMethods) is unsupported, not a multi-upstream problem.
func TestShouldHandleMethod_StatefulEligibility(t *testing.T) {
	statefulNet := func() *Network {
		return &Network{
			cfg: &common.NetworkConfig{
				Methods: &common.MethodsConfig{
					Definitions: map[string]*common.CacheMethodConfig{
						"eth_getFilterChanges": {Stateful: true},
					},
				},
			},
		}
	}
	newReq := func() *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getFilterChanges","params":[]}`))
	}

	full1 := common.NewFakeUpstream("full-1")
	full2 := common.NewFakeUpstream("full-2")
	noFilter1 := common.NewFakeUpstream("no-filter-1")
	noFilter1.Config().IgnoreMethods = []string{"eth_getFilterChanges"}
	noFilter2 := common.NewFakeUpstream("no-filter-2")
	noFilter2.Config().IgnoreMethods = []string{"eth_getFilterChanges"}

	t.Run("no eligible upstream reports unsupported, not single-upstream", func(t *testing.T) {
		err := statefulNet().shouldHandleMethod(newReq(), "eth_getFilterChanges", []common.Upstream{noFilter1, noFilter2})
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeNotImplemented))
		assert.Contains(t, err.Error(), "not supported by any targeted upstream")
	})

	t.Run("multiple eligible upstreams report single-upstream requirement", func(t *testing.T) {
		err := statefulNet().shouldHandleMethod(newReq(), "eth_getFilterChanges", []common.Upstream{full1, full2})
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeNotImplemented))
		assert.Contains(t, err.Error(), "single targeted upstream")
	})
}

// sumCounterFamily returns the sum of every series in a counter family from
// the default prometheus registry. Reading the family sum (rather than exact
// label values) keeps the assertion robust to label changes; callers compare
// before/after deltas around the code under test.
func sumCounterFamily(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	var sum float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			sum += m.GetCounter().GetValue()
		}
	}
	return sum
}

// TestNetworkConsensus_MethodIgnoredUpstreams_EndToEnd exercises the full
// network path (real Network, real upstream registry, per-upstream HTTP
// servers) for consensus over a mixed pool where some upstreams statically
// ignore the requested method. End goals verified:
//
//  1. method-ignored upstreams receive ZERO traffic and never become
//     participants (strict per-upstream call counts);
//  2. their instant exclusions do NOT arm maxWaitOnResult/maxWaitOnEmpty —
//     the round waits for the real participants even when they are far
//     slower than both caps (duration check + wait-capped metric delta);
//  3. surplus participant slots (maxParticipants > eligible upstreams)
//     resolve as no-attempt and do not arm the caps either;
//  4. when EVERY upstream ignores the method, the request still fails fast
//     with the descriptive per-upstream "method ignored" error and no
//     upstream traffic.
func TestNetworkConsensus_MethodIgnoredUpstreams_EndToEnd(t *testing.T) {
	t.Run("ignored upstreams excluded and wait caps not armed by exclusions", func(t *testing.T) {
		upstreams := createTestUpstreams(4)
		// Archive-style nodes: statically ignore broadcast methods.
		upstreams[0].IgnoreMethods = []string{"eth_send*"}
		upstreams[1].IgnoreMethods = []string{"eth_send*"}

		waitCappedBefore := sumCounterFamily(t, "erpc_consensus_wait_capped_total")

		runConsensusTest(t, consensusTestCase{
			name:          "ignored_upstreams_excluded_wait_caps_not_armed",
			upstreams:     upstreams,
			requestMethod: "eth_sendRawTransaction",
			requestParams: []interface{}{"0x02f870018203e8843b9aca00847735940082520894aabbccddeeff00112233445566778899aabbccdd8080c0"},
			consensusConfig: &common.ConsensusPolicyConfig{
				// More participants than eligible upstreams: the surplus
				// slots resolve instantly with no-upstreams-left and must
				// not arm the wait caps.
				MaxParticipants: 4,
				// Unreachable threshold: the round must rely on response
				// collection (and the method's first-success rule), not on
				// an agreement short-circuit, so premature wait-cap firing
				// would be visible as a failure.
				AgreementThreshold:      4,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				// Far smaller than the real upstreams' response time: if
				// the instant exclusions armed these, the round would
				// resolve before any tx hash arrives.
				MaxWaitOnResult: common.NewStaticDuration(30 * time.Millisecond),
				MaxWaitOnEmpty:  common.NewStaticDuration(30 * time.Millisecond),
			},
			mockResponses: []mockResponse{
				{}, // ignored upstream: must never be called (placeholder keeps indexes aligned)
				{}, // ignored upstream: must never be called
				{status: 200, body: jsonRpcSuccess("0x1111111111111111111111111111111111111111111111111111111111111111"), delay: 150 * time.Millisecond},
				{status: 200, body: jsonRpcSuccess("0x1111111111111111111111111111111111111111111111111111111111111111"), delay: 150 * time.Millisecond},
			},
			expectedCalls: []int{0, 0, 1, 1},
			expectedResult: &expectedResult{
				contains: "0x1111111111111111111111111111111111111111111111111111111111111111",
				check: func(t *testing.T, resp *common.NormalizedResponse, duration time.Duration) {
					assert.GreaterOrEqual(t, duration, 140*time.Millisecond,
						"round must wait for the real (slow) participants; instant exclusions must not start the wait-cap countdown")
				},
			},
		})

		assert.Equal(t, waitCappedBefore, sumCounterFamily(t, "erpc_consensus_wait_capped_total"),
			"wait caps must not fire when the only instant responses are method-ignored exclusions")
	})

	t.Run("all upstreams ignore the method: descriptive error, zero traffic", func(t *testing.T) {
		upstreams := createTestUpstreams(3)
		for _, u := range upstreams {
			u.IgnoreMethods = []string{"eth_send*"}
		}

		runConsensusTest(t, consensusTestCase{
			name:          "all_upstreams_ignore_method",
			upstreams:     upstreams,
			requestMethod: "eth_sendRawTransaction",
			requestParams: []interface{}{"0x02f870018203e8843b9aca00847735940082520894aabbccddeeff00112233445566778899aabbccdd8080c0"},
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 3,
				MaxWaitOnResult:    common.NewStaticDuration(30 * time.Millisecond),
				MaxWaitOnEmpty:     common.NewStaticDuration(30 * time.Millisecond),
			},
			mockResponses: []mockResponse{},
			expectedCalls: []int{0, 0, 0},
			expectedError: &expectedError{
				code: common.ErrCodeUpstreamMethodIgnored,
			},
		})
	})
}

// TestFilterMethodEligible_StickySurvivorFallsBackToRegistry reproduces
// erpc#1125: after a sticky selection policy pins a single survivor, the
// method-eligibility filter must NOT keep that ineligible survivor when the
// full registry still holds method-eligible upstreams. The pre-fix call site
// is:
//
//	upsList = n.policyEngine.GetOrderedInLane(...)   // sticky: ONE pinned node
//	if eligible, dropped := filterMethodEligible(upsList, method); dropped > 0 && len(eligible) > 0 {
//	    upsList = eligible
//	}
//
// When the pinned survivor ignores the requested method, `len(eligible) == 0`
// so the ineligible survivor is kept and the request never reaches the
// method-eligible upstreams that are still registered. This test pins the
// policy survivor set to a single write-ineligible upstream while the
// registry holds a write-eligible one, and asserts the eligible upstream is
// the one that serves the request.
func TestFilterMethodEligible_StickySurvivorFallsBackToRegistry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	setupGockObserver(t)

	upstreamConfigs := []*common.UpstreamConfig{
		{
			Id:       "public-read",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://public-read.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
			Routing:  &common.UpstreamRoutingConfig{Probe: common.ProbeModeOff},
			// The sticky-pinned public node cannot broadcast.
			IgnoreMethods: []string{"eth_sendRawTransaction", "eth_sendTransaction"},
		},
		{
			Id:       "paid-write",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://paid-write.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
			Routing:  &common.UpstreamRoutingConfig{Probe: common.ProbeModeOff},
		},
	}
	// State-poller mocks: without these the upstreams never finish
	// bootstrapping and Forward fails with ErrNetworkInitializing.
	for _, c := range upstreamConfigs {
		mockStatePollerForHost(c.Endpoint)
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
		ctx, &log.Logger, "test", upstreamConfigs, ssr,
		rateLimitersRegistry, vr, pr, nil, metricsTracker, nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		SelectionPolicy: &common.SelectionPolicyConfig{
			EvalInterval: 0, // frozen — the test pins the order itself
		},
		Failsafe: []*common.FailsafeConfig{
			{
				MatchMethod: "eth_sendRawTransaction",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 2,
					Delay:       common.Duration(10 * time.Millisecond),
				},
			},
		},
	}

	policyEngine := policy.NewEngine(ctx, &log.Logger, "test", metricsTracker, policystdlib.Install, nil)
	network, err := NewNetwork(
		ctx, &log.Logger, "test", networkConfig,
		rateLimitersRegistry, upstreamsRegistry, metricsTracker, policyEngine,
	)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	require.Len(t, upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123)), 2,
		"registry must hold both upstreams")

	// Pin the POLICY SURVIVOR SET to the single write-ineligible node — the
	// in-test analogue of stickyPrimary pinning `public-read` for the network.
	// Only the policy engine is pinned here: the upstream registry must keep
	// BOTH upstreams, because that is precisely the precondition the issue
	// describes (a narrowed survivor set over an intact full registry).
	// (`network.PinUpstreamOrderForTest` would reorder the registry itself,
	// which is not what sticky selection does.)
	policy.OverrideOrderForTest(policyEngine, network.networkId, "public-read")

	survivors := policyEngine.GetOrderedInLane(network.networkId, "eth_sendRawTransaction", "*", nil)
	require.Len(t, survivors, 1, "precondition: sticky survivor set is a single node")
	require.Equal(t, "public-read", survivors[0].Id())

	// Mock only the paid writer: it is the only upstream that can serve the
	// method, so a correct fallback must reach it. Reject (no mock) any
	// traffic to the write-ineligible node so a buggy fallback surfaces.
	gock.New("http://paid-write.localhost").
		Post("/").
		Persist().
		Reply(200).
		JSON(jsonRpcSuccess("0xdeadbeef"))
	resetGockHits()

	reqBytes, err := json.Marshal(map[string]interface{}{
		"method": "eth_sendRawTransaction",
		"params": []interface{}{"0x02f870018203e8843b9aca0084"},
	})
	require.NoError(t, err)
	req := common.NewNormalizedRequest(reqBytes)
	req.SetNetwork(network)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "request must be served by the method-eligible upstream from the full registry")
	require.NotNil(t, resp)
	jrr, jrrErr := resp.JsonRpcResponse()
	require.NoError(t, jrrErr)
	assert.Contains(t, jrr.GetResultString(), "0xdeadbeef")
	assert.Greater(t, gockHits("http://paid-write.localhost"), 0,
		"paid-write (the only method-eligible upstream) must have been called")
	assert.Equal(t, 0, gockHits("http://public-read.localhost"),
		"the write-ineligible sticky survivor must never receive the send")
}
