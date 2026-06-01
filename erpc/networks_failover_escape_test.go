package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	policystdlib "github.com/erpc/erpc/internal/policy/stdlib"
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

// NOTE (post-refactor adaptation):
//
// This file originally drove a hand-written `selectionPolicy` JS function
// that keyed off `u.config.group === 'fallback'`. After the policy refactor
// the per-request `selectionPolicyEvaluator` / `AcquirePermit` API is gone;
// selection is pre-computed per (network, method) tick by `internal/policy`'s
// Engine, and fallback-tier upstreams are declared via the
// `tier:fallback` tag (constant `common.TagTierFallback`).
//
// The production default policy (internal/policy/default_policy.js) already
// expresses exactly the behaviour these tests need via
//   .preferTag('!tier:fallback', { minHealthy: 1, fallback: 'tier:fallback' })
// which cordons fallback-tagged upstreams out of the ordered list while at
// least one primary survives the health excludes, and promotes the
// fallbacks once every primary is excluded (e.g. by error-rate/lag).
//
// So instead of a custom JS function + `AcquirePermit` introspection, these
// tests now build a real `*policy.Engine` running the default policy and
// assert OBSERVABLE behaviour:
//   - tracker metrics moved (errorRate) via GetUpstreamMethodMetrics(..., finality)
//   - the policy's ordered list (PolicyOrderedUpstreams / the engine's
//     LatestDecisionOutputForTest) cordons the primary / promotes the fallback
//   - which upstream actually served the response
//   - the erpc_network_fallback_escape_total metric count
//
// The ticker is frozen (EvalInterval=0); tests drive ticks via
// `policy.TickForTest`.

// mockJsonRpcUpstream wires the standard state-poller mocks for one upstream
// at a fixed block height. Used to build the multi-primary + multi-fallback
// fixture the failover tests need.
func mockJsonRpcUpstream(host string, chainIdHex, latestHex, finalizedHex string) {
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_chainId")
		}).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"result":"%s"}`, chainIdHex)))

	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, `"latest"`)
		}).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"result":{"number":"%s","timestamp":"0x6702a8f0"}}`, latestHex)))

	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, `"finalized"`)
		}).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"result":{"number":"%s","timestamp":"0x6702a8e0"}}`, finalizedHex)))

	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_syncing")
		}).
		Reply(200).
		JSON([]byte(`{"result":false}`))
}

// mockEthCallReturning wires an eth_call mock that echoes which upstream
// served the request via a unique result hex. Lets the test assert
// failover by which upstream actually answered.
func mockEthCallReturning(host string, resultHex string) {
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_call")
		}).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":"%s"}`, resultHex)))
}

// failoverUpstreamConfigs builds the standard 4-upstream layout used by the
// failover tests: 2 primaries + 2 fallbacks (the latter tagged
// `common.TagTierFallback`). Hosts are rpc1..rpc4.localhost.
func failoverUpstreamConfigs() []*common.UpstreamConfig {
	return []*common.UpstreamConfig{
		{
			Type: common.UpstreamTypeEvm, Id: "primary-1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId:             999,
				StatePollerInterval: common.Duration(100 * time.Millisecond),
				StatePollerDebounce: common.Duration(20 * time.Millisecond),
			},
		},
		{
			Type: common.UpstreamTypeEvm, Id: "primary-2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId:             999,
				StatePollerInterval: common.Duration(100 * time.Millisecond),
				StatePollerDebounce: common.Duration(20 * time.Millisecond),
			},
		},
		{
			Type: common.UpstreamTypeEvm, Id: "fallback-1",
			Endpoint: "http://rpc3.localhost",
			Tags:     []string{common.TagTierFallback},
			Evm: &common.EvmUpstreamConfig{
				ChainId:             999,
				StatePollerInterval: common.Duration(100 * time.Millisecond),
				StatePollerDebounce: common.Duration(20 * time.Millisecond),
			},
		},
		{
			Type: common.UpstreamTypeEvm, Id: "fallback-2",
			Endpoint: "http://rpc4.localhost",
			Tags:     []string{common.TagTierFallback},
			Evm: &common.EvmUpstreamConfig{
				ChainId:             999,
				StatePollerInterval: common.Duration(100 * time.Millisecond),
				StatePollerDebounce: common.Duration(20 * time.Millisecond),
			},
		},
	}
}

// buildFailoverNetwork wires a real Network + UpstreamsRegistry +
// policy.Engine running the production default selection policy (which
// cordons `tier:fallback` upstreams while a primary survives). The engine
// ticker is frozen; the caller drives ticks via policy.TickForTest.
func buildFailoverNetwork(
	t *testing.T, ctx context.Context,
	upstreamConfigs []*common.UpstreamConfig,
	enableFailover bool,
) (*Network, *upstream.UpstreamsRegistry, *health.Tracker) {
	t.Helper()

	rlr, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)
	// 5s metrics window so accumulated request/failure samples don't get
	// reset mid-test.
	mt := health.NewTracker(&log.Logger, "main", 5*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	require.NoError(t, err)

	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
		LockMaxWait:     common.Duration(200 * time.Millisecond),
		UpdateMaxWait:   common.Duration(200 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second),
		LockTtl:         common.Duration(4 * time.Second),
	}
	require.NoError(t, sharedStateCfg.SetDefaults("test"))
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	require.NoError(t, err)

	upr := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "main", upstreamConfigs,
		ssr, rlr, vr, pr, nil, mt, nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:                  999,
			EnforceBlockAvailability: util.BoolPtr(true),
		},
		// Default selection policy, frozen ticker; tests drive ticks.
		SelectionPolicy: &common.SelectionPolicyConfig{
			EvalInterval: 0,
		},
	}
	if enableFailover {
		networkConfig.Failover = &common.FailoverConfig{OnDefaultsExhausted: util.BoolPtr(true)}
	}

	policyEngine := policy.NewEngine(ctx, &log.Logger, "main", mt, policystdlib.Install, nil)

	network, err := NewNetwork(ctx, &log.Logger, "main", networkConfig, rlr, upr, mt, policyEngine)
	require.NoError(t, err)

	upr.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, upr.GetInitializer().WaitForTasks(ctx))
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(999)))
	require.NoError(t, network.Bootstrap(ctx))

	// Bootstrap each upstream's state poller; they fetch latest/finalized
	// from the gock mocks and populate the per-upstream shared counters.
	upsList := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(999))
	require.Len(t, upsList, 4)
	for _, ups := range upsList {
		require.NoError(t, ups.Bootstrap(ctx))
	}

	// Let pollers run a couple of cycles so per-upstream LatestBlock counters
	// are populated.
	time.Sleep(500 * time.Millisecond)

	return network, upr, mt
}

// finalityForRequest computes the finality the gate-skip recording path uses
// for a request, so assertions read the same tracker bucket. We read the
// all-finalities aggregate via DataFinalityStateAll which is fed by every
// Record* regardless of the request's specific finality, so this is mostly
// documentation — DataFinalityStateAll is the safe key.

func mustGetUpstream(ups []*upstream.Upstream, id string) *upstream.Upstream {
	for _, u := range ups {
		if u.Id() == id {
			return u
		}
	}
	panic("upstream not found: " + id)
}

// TestFailover_GateSkipsAccumulateErrorRate verifies the recording fix:
// block-availability gate-skips (handleBlockSkip) now record (Request,
// Failure) on the health tracker so the upstream's errorRate moves. With the
// tracker reflecting the skips, the default selection policy's
// `errorRateAbove(0.7)` exclude eventually cordons the primary on the next
// eval tick, promoting the fallbacks via `preferTag`, and a client request
// then succeeds via a fallback.
//
// Pre-fix behaviour: gate rejections in checkUpstreamBlockAvailability did
// not call RecordUpstreamFailure, so the primary's errorRate stayed at 0
// indefinitely, the policy kept the primary, and the fallbacks remained
// cordoned. Post-fix: handleBlockSkip records on every retryable gate-reject.
func TestFailover_GateSkipsAccumulateErrorRate(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Two HTTP primaries stuck at block 1000; two fallbacks at 1002.
	const chainIdHex = "0x3e7" // 999
	const primaryLatest = "0x3e8"
	const fallbackLatest = "0x3ea"
	const finalizedHex = "0x3e0"
	const requestBlock = "0x3ea" // 1002 — beyond primary cache, gate-rejects on primaries

	mockJsonRpcUpstream("rpc1.localhost", chainIdHex, primaryLatest, finalizedHex)
	mockJsonRpcUpstream("rpc2.localhost", chainIdHex, primaryLatest, finalizedHex)
	mockJsonRpcUpstream("rpc3.localhost", chainIdHex, fallbackLatest, finalizedHex)
	mockJsonRpcUpstream("rpc4.localhost", chainIdHex, fallbackLatest, finalizedHex)

	mockEthCallReturning("rpc1.localhost", "0x1111")
	mockEthCallReturning("rpc2.localhost", "0x2222")
	mockEthCallReturning("rpc3.localhost", "0x3333")
	mockEthCallReturning("rpc4.localhost", "0x4444")

	// Failover enabled so once fallbacks are promoted (or via the escape
	// hatch) the final request is served by a fallback.
	network, upr, mt := buildFailoverNetwork(t, ctx, failoverUpstreamConfigs(), true)

	upsList := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(999))
	require.Len(t, upsList, 4)
	primaryUp := mustGetUpstream(upsList, "primary-1")
	fallbackUp := mustGetUpstream(upsList, "fallback-1")
	require.Equal(t, int64(1000), primaryUp.EvmStatePoller().LatestBlock(), "primary should be at block 1000")
	require.Equal(t, int64(1002), fallbackUp.EvmStatePoller().LatestBlock(), "fallback should be at block 1002")

	// Initial tick: primaries are healthy → default policy's preferTag keeps
	// only primaries in the ordered list and cordons the fallbacks out.
	policy.ResetSlotStateForTest(network.policyEngine, network.networkId, "*")
	policy.TickForTest(network.policyEngine, network.networkId, "*")

	order := network.PolicyOrderedUpstreams("eth_call")
	require.NotEmpty(t, order, "policy must have produced an ordered list")
	assert.Contains(t, order, "primary-1", "primary should be active initially")
	assert.NotContains(t, order, "fallback-1",
		"fallback should be cordoned initially (primaries are healthy)")

	// --- Phase 2: trigger the gate-rejection burst ---
	//
	// Each request targets block 1002. The gate compares against the
	// per-upstream cache (1000 on primaries) and rejects. With the
	// handleBlockSkip recording fix, each rejection records (request,
	// failure) on the metrics tracker for the primary.
	const burstSize = 80
	for i := 0; i < burstSize; i++ {
		req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"eth_call","params":[{"to":"0xdead","data":"0x"},"%s"]}`,
			i, requestBlock,
		)))
		req.SetNetwork(network)
		_, _ = network.Forward(ctx, req)
	}

	// --- Assertions ---

	// 1) The handleBlockSkip recording fix plumbed gate-skips into the
	//    tracker: the primary's errorRate on the all-finalities "*" aggregate
	//    (the slot the policy reads) must reflect the gate-rejection burst.
	//    Without the fix this stays at 0 indefinitely.
	primaryMetrics := mt.GetUpstreamMethodMetrics(primaryUp, "*", common.DataFinalityStateAll)
	require.NotNil(t, primaryMetrics)
	t.Logf("primary primary-1 metrics: requests=%d errors=%d errorRate=%.3f",
		primaryMetrics.RequestsTotal.Load(), primaryMetrics.ErrorsTotal.Load(), primaryMetrics.ErrorRate())
	assert.Greater(t, primaryMetrics.ErrorRate(), 0.7,
		"primary errorRate should cross 0.7 from gate-skip recording; "+
			"if 0 the handleBlockSkip recording fix didn't take effect")

	// 2) After the burst, the next policy eval tick observes the elevated
	//    errorRate and excludes the primary (errorRateAbove(0.7) gated on
	//    samplesAbove(10) — the burst supplies >>10 samples). With every
	//    primary excluded, preferTag promotes the fallbacks.
	policy.TickForTest(network.policyEngine, network.networkId, "*")
	order = network.PolicyOrderedUpstreams("eth_call")
	require.NotEmpty(t, order)
	t.Logf("post-burst policy order: %v", order)
	assert.NotContains(t, order, "primary-1",
		"primary should be cordoned after the gate-skip burst moved its errorRate past 0.7")
	assert.Contains(t, order, "fallback-1",
		"fallback should be promoted after every primary is excluded")

	// 3) A fresh request for block 1002 must now succeed via a fallback
	//    (which have block 1002). Refresh the registry's sorted list so the
	//    request path sees the post-tick ordering.
	require.NoError(t, upr.RefreshUpstreamNetworkMethodScores())
	time.Sleep(50 * time.Millisecond)

	finalReq := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":9999,"method":"eth_call","params":[{"to":"0xdead","data":"0x"},"%s"]}`,
		requestBlock,
	)))
	finalReq.SetNetwork(network)
	resp, err := network.Forward(ctx, finalReq)
	require.NoError(t, err, "client request must succeed via fallback after failover")
	require.NotNil(t, resp)
	defer resp.Release()

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.NotNil(t, jrr)
	result := strings.Trim(jrr.GetResultString(), `"`)
	// Fallbacks return 0x3333 or 0x4444; primaries return 0x1111 or 0x2222.
	assert.Contains(t, []string{"0x3333", "0x4444"}, result,
		"final eth_call must be served by a fallback (0x3333 or 0x4444), got %q", result)
}

// failoverFixtureOpts configures the standard 4-upstream test layout
// (2 primaries + 2 fallbacks) used by the escape-hatch sub-tests.
type failoverFixtureOpts struct {
	primaryLatest  string
	fallbackLatest string
	enableFailover bool
}

func setupFailoverFixture(
	t *testing.T, ctx context.Context, opts failoverFixtureOpts,
) (*Network, []*upstream.Upstream, *health.Tracker) {
	t.Helper()
	util.ResetGock()

	const chainIdHex = "0x3e7" // 999
	const finalizedHex = "0x3e0"

	mockJsonRpcUpstream("rpc1.localhost", chainIdHex, opts.primaryLatest, finalizedHex)
	mockJsonRpcUpstream("rpc2.localhost", chainIdHex, opts.primaryLatest, finalizedHex)
	mockJsonRpcUpstream("rpc3.localhost", chainIdHex, opts.fallbackLatest, finalizedHex)
	mockJsonRpcUpstream("rpc4.localhost", chainIdHex, opts.fallbackLatest, finalizedHex)

	mockEthCallReturning("rpc1.localhost", "0x1111")
	mockEthCallReturning("rpc2.localhost", "0x2222")
	mockEthCallReturning("rpc3.localhost", "0x3333")
	mockEthCallReturning("rpc4.localhost", "0x4444")

	network, upr, mt := buildFailoverNetwork(t, ctx, failoverUpstreamConfigs(), opts.enableFailover)

	upsList := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(999))
	require.Len(t, upsList, 4)

	// Drive an initial tick so the default policy computes the ordered list.
	// With healthy primaries, preferTag cordons the fallbacks out of the
	// ordered list — so the request path sees only primaries, exhausts them
	// on a gate-skip, and the per-request escape hatch (not the policy) is
	// what brings the fallbacks in. This is exactly the path under test.
	policy.ResetSlotStateForTest(network.policyEngine, network.networkId, "*")
	policy.TickForTest(network.policyEngine, network.networkId, "*")
	require.NoError(t, upr.RefreshUpstreamNetworkMethodScores())
	time.Sleep(50 * time.Millisecond)

	return network, upsList, mt
}

// ethCallRequest constructs an eth_call request targeting a specific block.
func ethCallRequest(id int, blockHex string) *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"eth_call","params":[{"to":"0xdead","data":"0x"},"%s"]}`,
		id, blockHex,
	)))
}

// TestFailover_EscapeHatch verifies the per-request escape hatch:
// when the primary set is exhausted with retryable errors within a single
// request, fallbacks are appended and re-iterated so the client receives a
// fallback response on the same call.
func TestFailover_EscapeHatch(t *testing.T) {
	t.Run("EscapesToFallbackOnFirstFailingRequest", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Primaries stuck at 1000, fallbacks at 1002. Request block 1002 →
		// gate-rejects on primaries → escape hatch should fire and route to
		// fallback.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8", // 1000
			fallbackLatest: "0x3ea", // 1002
			enableFailover: true,
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)

		// Single request — no warm-up burst, no policy-tick wait.
		req := ethCallRequest(1, "0x3ea")
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err, "client request must succeed on first try via escape hatch")
		require.NotNil(t, resp)
		defer resp.Release()

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		result := strings.Trim(jrr.GetResultString(), `"`)
		assert.Contains(t, []string{"0x3333", "0x4444"}, result,
			"escape hatch must route to a fallback; got %q", result)

		after := promUtil.ToFloat64(counter)
		assert.Equal(t, before+1, after,
			"MetricNetworkFallbackEscapeTotal must increment by exactly 1 for the escape firing")
	})

	t.Run("NoEscapeWhenPrimariesHealthy", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// All upstreams at block 1002. Request block 1002 → primary's gate
		// passes → returns on first iteration → escape hatch never fires.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3ea", // 1002
			fallbackLatest: "0x3ea", // 1002
			enableFailover: true,
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)

		// 30 requests over the healthy path. None should hit fallback.
		for i := 0; i < 30; i++ {
			req := ethCallRequest(i, "0x3ea")
			req.SetNetwork(network)
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err, "healthy primary request must succeed (iter %d)", i)
			require.NotNil(t, resp)

			jrr, _ := resp.JsonRpcResponse()
			result := strings.Trim(jrr.GetResultString(), `"`)
			assert.Contains(t, []string{"0x1111", "0x2222"}, result,
				"healthy request must be served by a primary (0x1111/0x2222), got %q (iter %d)", result, i)
			resp.Release()
		}

		after := promUtil.ToFloat64(counter)
		assert.Equal(t, before, after,
			"escape hatch must NOT fire when primaries are healthy; counter must be unchanged")
	})

	t.Run("NoEscapeWhenFailoverDisabled", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Same primary-stuck-fallback-ahead setup as Sub-test A, but with
		// failover.onDefaultsExhausted unset. The per-request escape hatch must
		// respect the operator's opt-out and never fire.
		//
		// NOTE (post-#888): the request may still be served by the fallback
		// tier — upstream's default selection policy natively falls through to
		// `tier:fallback` via preferTag('!tier:fallback', {fallback}). That
		// native routing is independent of our Failover feature. What this test
		// guards is precisely OUR contribution: with Failover disabled the
		// `network_fallback_escape_total` counter must stay flat.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8", // 1000
			fallbackLatest: "0x3ea", // 1002
			enableFailover: false,   // <-- disabled
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)
		req := ethCallRequest(1, "0x3ea")
		req.SetNetwork(network)
		resp, _ := network.Forward(ctx, req)
		if resp != nil {
			resp.Release()
		}

		after := promUtil.ToFloat64(counter)
		assert.Equal(t, before, after,
			"with failover disabled the per-request escape hatch must NOT fire; counter must be unchanged")
	})

	t.Run("OnlyEscalatesOncePerRequest", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Primaries stuck at 1000, fallbacks ALSO stuck at 1000. Request
		// block 1002. The gap (2 blocks) is within MaxRetryableBlockDistance,
		// so the gate-skip is RETRYABLE and the primary set exhausts → the
		// escape hatch fires and appends the fallback tier. Under upstream's
		// new model the fallback (within retryable tolerance) then serves the
		// request. The invariant this test guards is escalate-AT-MOST-ONCE: the
		// escape must fire exactly once and never re-enter escalationLoop a
		// second time (which the single MarkEscalatedToFallbacks gate ensures).
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8", // 1000
			fallbackLatest: "0x3e8", // also 1000
			enableFailover: true,
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)

		req := ethCallRequest(1, "0x3ea")
		req.SetNetwork(network)
		resp, _ := network.Forward(ctx, req)
		if resp != nil {
			resp.Release()
		}

		after := promUtil.ToFloat64(counter)
		assert.Equal(t, before+1, after,
			"escape must fire EXACTLY once per request (no re-escalation loop); got %v→%v",
			before, after)
	})

	t.Run("EscapesOnNonRetryableGateSkip", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Primaries stuck at block 1000, fallbacks far ahead at block 10000.
		// Request block 9000 (0x2328). The gap from primary (1000) to 9000 is
		// 8000 blocks — well beyond the default MaxRetryableBlockDistance —
		// so checkUpstreamBlockAvailability classifies each primary's skip as
		// NON-retryable (ErrUpstreamRequestSkipped wrapping
		// ErrUpstreamBlockUnavailable).
		//
		// This mirrors the live B2 incident pattern: primaries fronting a
		// stalled L2 reth pod return latestBlock far behind the chain head,
		// while a third-party fallback (Ankr) is at the real head and can
		// serve. Before the fix, lastErr stayed nil for non-retryable skips,
		// the escape gate's `lastErr != nil` check failed, and clients got
		// ErrUpstreamsExhausted. After the fix, lastErr is set unconditionally
		// in the gate-skip branch and the escape fires.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8",  // 1000
			fallbackLatest: "0x2710", // 10000
			enableFailover: true,
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)

		req := ethCallRequest(1, "0x2328") // 9000 — 8000 blocks ahead of primary
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err,
			"request must succeed via fallback even when primary gate-skip is non-retryable "+
				"(distance > MaxRetryableBlockDistance)")
		require.NotNil(t, resp)
		defer resp.Release()

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		result := strings.Trim(jrr.GetResultString(), `"`)
		assert.Contains(t, []string{"0x3333", "0x4444"}, result,
			"non-retryable-gate-skip escape must route to a fallback; got %q", result)

		after := promUtil.ToFloat64(counter)
		assert.Equal(t, before+1, after,
			"escape hatch must fire exactly once for the non-retryable gate-skip case")
	})
}
