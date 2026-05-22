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

// failoverSelectionPolicy is a representative production selectionPolicy that
// cordons fallback-group upstreams while any primary is healthy. Without the
// gate-skip/CB-open tracker fixes plus the per-request fallback escape hatch,
// a primary collapse leaves the policy unable to detect the failure and
// fallbacks remain cordoned out of upsList, producing client-facing
// ErrUpstreamsExhausted until the next eval tick (1m worst case).
const failoverSelectionPolicy = `
(upstreams, method) => {
  const isFallback = u => u && u.config && u.config.group === 'fallback'
  const healthOK = u => {
    const m = (u && u.metrics) || {}
    const err = m.errorRate
    const lag = m.blockHeadLag
    return (err == null || err < 0.5) && (lag == null || lag < 5)
  }
  const primary  = upstreams.filter(u => !isFallback(u))
  const fallback = upstreams.filter(isFallback)
  const healthy  = primary.filter(healthOK)
  if (healthy.length  > 0) return healthy
  if (fallback.length > 0) return fallback
  return upstreams
}
`

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

// TestFailover_GateSkipsAccumulateErrorRate verifies the recording fixes
// (handleBlockSkip → tracker, state-poller CB-open → tracker). With the
// fixes in place, a gate-rejection burst against a primary stuck behind
// the network aggregator must move errorRate past 0.5, the selectionPolicy
// must cordon the primary on its next eval tick, and the fallbacks must
// be promoted into upsList so subsequent client requests succeed via them.
//
// Pre-fix behaviour (verified via git stash on this same test): gate
// rejections in checkUpstreamBlockAvailability do not call
// RecordUpstreamFailure, so the primary's errorRate stays at 0
// indefinitely, the policy keeps the primary in `healthy`, and the
// fallbacks remain cordoned out of upsList. The final assertion fails
// with the primary still routable and the fallbacks not.
//
// Post-fix behaviour: handleBlockSkip records (Request, Failure) on every
// retryable gate-reject, errorRate crosses 0.5 within a single eval window
// once the burst starts, the next policy tick excludes the primary, and
// the fallbacks take over.
func TestFailover_GateSkipsAccumulateErrorRate(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Two HTTP primaries stuck at block 1000 (analogous to a node that
	// can't advance — pollers freeze, but the network aggregator
	// continues advancing via the fallbacks' independent pollers).
	const chainIdHex = "0x3e7" // 999
	const primaryLatest = "0x3e8"
	const fallbackLatest = "0x3ea"
	const finalizedHex = "0x3e0"
	const requestBlock = "0x3ea" // beyond primary cache, gate-rejects on primaries

	mockJsonRpcUpstream("rpc1.localhost", chainIdHex, primaryLatest, finalizedHex)
	mockJsonRpcUpstream("rpc2.localhost", chainIdHex, primaryLatest, finalizedHex)
	mockJsonRpcUpstream("rpc3.localhost", chainIdHex, fallbackLatest, finalizedHex)
	mockJsonRpcUpstream("rpc4.localhost", chainIdHex, fallbackLatest, finalizedHex)

	mockEthCallReturning("rpc1.localhost", "0x1111")
	mockEthCallReturning("rpc2.localhost", "0x2222")
	mockEthCallReturning("rpc3.localhost", "0x3333")
	mockEthCallReturning("rpc4.localhost", "0x4444")

	upstreamConfigs := []*common.UpstreamConfig{
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
			Group:    common.UpstreamGroupFallback,
			Evm: &common.EvmUpstreamConfig{
				ChainId:             999,
				StatePollerInterval: common.Duration(100 * time.Millisecond),
				StatePollerDebounce: common.Duration(20 * time.Millisecond),
			},
		},
		{
			Type: common.UpstreamTypeEvm, Id: "fallback-2",
			Endpoint: "http://rpc4.localhost",
			Group:    common.UpstreamGroupFallback,
			Evm: &common.EvmUpstreamConfig{
				ChainId:             999,
				StatePollerInterval: common.Duration(100 * time.Millisecond),
				StatePollerDebounce: common.Duration(20 * time.Millisecond),
			},
		},
	}

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	// 5s metrics window so accumulated request/failure samples don't get
	// reset mid-test. Pre-failure successes accumulate during initial
	// warmup but are dominated quickly once the gate-rejection burst starts.
	mt := health.NewTracker(&log.Logger, "main", 5*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)

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
		ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil, nil,
	)

	// The production-style selectionPolicy compressed to fire every 150ms
	// so the test doesn't have to wait the production 1m interval.
	evalFn, err := common.CompileFunction(failoverSelectionPolicy)
	require.NoError(t, err)
	selectionPolicy := &common.SelectionPolicyConfig{
		EvalInterval:       common.Duration(150 * time.Millisecond),
		EvalFunctionSource: failoverSelectionPolicy,
		EvalFunction:       evalFn,
		EvalPerMethod:      false,
	}
	require.NoError(t, selectionPolicy.SetDefaults())

	maxRetryable := int64(128)
	enforce := true
	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:                   999,
			MaxRetryableBlockDistance: &maxRetryable,
			EnforceBlockAvailability:  &enforce,
		},
		SelectionPolicy: selectionPolicy,
		// Failover.onDefaultsExhausted = true so once fallbacks are in upsList,
		// they're tried intra-request after primaries fail.
		Failover: &common.FailoverConfig{
			OnDefaultsExhausted: &enforce,
		},
	}

	network, err := NewNetwork(ctx, &log.Logger, "main", networkConfig, rlr, upr, mt)
	require.NoError(t, err)

	upr.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, upr.GetInitializer().WaitForTasks(ctx))
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(999)))
	require.NoError(t, network.Bootstrap(ctx))

	// Bootstrap each upstream's state poller; they'll fetch latest/finalized
	// from the gock mocks and populate the per-upstream shared counters.
	upsList := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(999))
	require.Len(t, upsList, 4)
	for _, ups := range upsList {
		require.NoError(t, ups.Bootstrap(ctx))
	}

	// Let pollers run a couple of cycles so per-upstream LatestBlock counters
	// are populated and the selection policy has evaluated at least once.
	time.Sleep(500 * time.Millisecond)

	// Sanity-check the initial state:
	// - Primaries should have LatestBlock = 1000
	// - Fallbacks should have LatestBlock = 1002
	// - Policy should keep primaries active and cordon fallbacks
	primaryUp := mustGetUpstream(upsList, "primary-1")
	fallbackUp := mustGetUpstream(upsList, "fallback-1")
	require.Equal(t, int64(1000), primaryUp.EvmStatePoller().LatestBlock(), "primary should be at block 1000")
	require.Equal(t, int64(1002), fallbackUp.EvmStatePoller().LatestBlock(), "fallback should be at block 1002")

	// Verify policy initially returns primaries only — fallback should be cordoned.
	require.NoError(t, network.selectionPolicyEvaluator.AcquirePermit(&log.Logger, primaryUp, "eth_call"),
		"primary should be active initially")
	require.Error(t, network.selectionPolicyEvaluator.AcquirePermit(&log.Logger, fallbackUp, "eth_call"),
		"fallback should be cordoned initially (primaries are healthy)")

	// --- Phase 2: trigger the gate-rejection burst ---
	//
	// Each request targets block 1002. The gate compares against the
	// per-upstream cache (1000 on primaries) and rejects. With the
	// handleBlockSkip recording fix, each rejection records (request,
	// failure) on the metrics tracker for the primary.
	//
	// Send enough requests to dominate the pre-burst success ratio. With
	// the 5s window and ~10 prior bootstrap-related successes, ~50 failures
	// is comfortably > 0.5 errorRate.
	const burstSize = 80
	for i := 0; i < burstSize; i++ {
		req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"eth_call","params":[{"to":"0xdead","data":"0x"},"%s"]}`,
			i, requestBlock,
		)))
		req.SetNetwork(network)
		_, _ = network.Forward(ctx, req)
	}

	// Wait for at least one full selectionPolicy eval tick (150ms) so the
	// policy observes the elevated errorRate and applies the cordon, plus
	// one score-refresh tick (1s) so the registry rebuilds sortedUpstreams
	// with fallbacks included. Then force an extra refresh to remove any
	// remaining timing slack.
	time.Sleep(400 * time.Millisecond)
	require.NoError(t, upr.RefreshUpstreamNetworkMethodScores())
	time.Sleep(100 * time.Millisecond)

	// --- Assertions ---

	// 1) The handleBlockSkip recording fix plumbed gate-skips into the
	//    tracker: the primary's errorRate on the "*" aggregate (the slot
	//    the policy reads with evalPerMethod=false) must reflect the
	//    gate-rejection burst. Without the fix this stays at 0
	//    indefinitely and the policy never reacts.
	primaryMetrics := mt.GetUpstreamMethodMetrics(primaryUp, "*")
	t.Logf("primary primary-1 metrics: requests=%d errors=%d errorRate=%.3f",
		primaryMetrics.RequestsTotal.Load(), primaryMetrics.ErrorsTotal.Load(), primaryMetrics.ErrorRate())
	assert.Greater(t, primaryMetrics.ErrorRate(), 0.5,
		"primary errorRate should cross 0.5 from gate-skip recording; "+
			"if 0 the handleBlockSkip recording fix didn't take effect")

	// 2) After the burst + at least one policy eval tick, the primary must
	//    be cordoned. AcquirePermit returns ErrCodeUpstreamExcludedByPolicy
	//    for cordoned upstreams.
	err = network.selectionPolicyEvaluator.AcquirePermit(&log.Logger, primaryUp, "eth_call")
	require.Error(t, err, "primary should be cordoned after the gate-skip burst")
	assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamExcludedByPolicy),
		"expected primary to be excluded by policy, got: %v", err)

	// 3) The fallback must be promoted to active by the same eval tick.
	require.NoError(t, network.selectionPolicyEvaluator.AcquirePermit(&log.Logger, fallbackUp, "eth_call"),
		"fallback should be promoted after primary errorRate exceeds 0.5")

	// 4) A fresh request for block 1002 must now succeed. With fallbacks in
	//    upsList and primaries either cordoned or still gate-rejected, the
	//    request must route to one of the fallbacks (which have block 1002).
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

func mustGetUpstream(ups []*upstream.Upstream, id string) *upstream.Upstream {
	for _, u := range ups {
		if u.Id() == id {
			return u
		}
	}
	panic("upstream not found: " + id)
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

	upstreamConfigs := []*common.UpstreamConfig{
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
			Group:    common.UpstreamGroupFallback,
			Evm: &common.EvmUpstreamConfig{
				ChainId:             999,
				StatePollerInterval: common.Duration(100 * time.Millisecond),
				StatePollerDebounce: common.Duration(20 * time.Millisecond),
			},
		},
		{
			Type: common.UpstreamTypeEvm, Id: "fallback-2",
			Endpoint: "http://rpc4.localhost",
			Group:    common.UpstreamGroupFallback,
			Evm: &common.EvmUpstreamConfig{
				ChainId:             999,
				StatePollerInterval: common.Duration(100 * time.Millisecond),
				StatePollerDebounce: common.Duration(20 * time.Millisecond),
			},
		},
	}

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "main", 5*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)

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
		ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil, nil,
	)

	evalFn, err := common.CompileFunction(failoverSelectionPolicy)
	require.NoError(t, err)
	selectionPolicy := &common.SelectionPolicyConfig{
		EvalInterval:       common.Duration(150 * time.Millisecond),
		EvalFunctionSource: failoverSelectionPolicy,
		EvalFunction:       evalFn,
		EvalPerMethod:      false,
	}
	require.NoError(t, selectionPolicy.SetDefaults())

	maxRetryable := int64(128)
	enforce := true
	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:                   999,
			MaxRetryableBlockDistance: &maxRetryable,
			EnforceBlockAvailability:  &enforce,
		},
		SelectionPolicy: selectionPolicy,
	}
	if opts.enableFailover {
		on := true
		networkConfig.Failover = &common.FailoverConfig{OnDefaultsExhausted: &on}
	}

	network, err := NewNetwork(ctx, &log.Logger, "main", networkConfig, rlr, upr, mt)
	require.NoError(t, err)

	upr.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, upr.GetInitializer().WaitForTasks(ctx))
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(999)))
	require.NoError(t, network.Bootstrap(ctx))

	upsList := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(999))
	require.Len(t, upsList, 4)
	for _, ups := range upsList {
		require.NoError(t, ups.Bootstrap(ctx))
	}

	// Let pollers run and policy evaluate at least once.
	time.Sleep(500 * time.Millisecond)

	// Prime the registry's sortedUpstreams[networkId]["eth_call"] cache by
	// asking for it once. GetSortedUpstreams populates the slot from "*" or
	// raw networkUpstreams on first call but does NOT apply filterCordoned —
	// only RefreshUpstreamNetworkMethodScores does. So we prime FIRST, then
	// refresh, to get the cordon decisions applied to the eth_call slot
	// before the test request fires. Without this two-step, the first
	// network.Forward sees all 4 upstreams (cordoned-but-not-yet-filtered),
	// which masks the cordon-then-escape behaviour under test.
	_, err = upr.GetSortedUpstreams(ctx, util.EvmNetworkId(999), "eth_call")
	require.NoError(t, err)
	require.NoError(t, upr.RefreshUpstreamNetworkMethodScores())
	time.Sleep(100 * time.Millisecond)

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
		// failover.onDefaultsExhausted unset. Escape hatch must respect the
		// operator's opt-out.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8", // 1000
			fallbackLatest: "0x3ea", // 1002
			enableFailover: false,   // <-- disabled
		})

		req := ethCallRequest(1, "0x3ea")
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		require.Error(t, err,
			"with failover disabled the escape hatch must NOT fire; request must surface ErrUpstreamsExhausted")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
			"expected ErrUpstreamsExhausted, got %v", err)
		if resp != nil {
			resp.Release()
		}
	})

	t.Run("OnlyEscalatesOncePerRequest", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Primaries stuck at 1000, fallbacks ALSO stuck at 1000. Request
		// block 1002 → gate-rejects everywhere. Escape fires once, finds
		// fallbacks still can't serve, returns exhausted — does NOT loop.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8", // 1000
			fallbackLatest: "0x3e8", // also 1000
			enableFailover: true,
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)

		req := ethCallRequest(1, "0x3ea")
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		require.Error(t, err,
			"all upstreams (primary + fallback) can't serve the block; must return ErrUpstreamsExhausted")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
			"expected ErrUpstreamsExhausted, got %v", err)
		if resp != nil {
			resp.Release()
		}

		after := promUtil.ToFloat64(counter)
		assert.Equal(t, before+1, after,
			"escape must fire exactly once per request even when fallbacks also fail; got %v→%v",
			before, after)
	})

	t.Run("EscapesOnNonRetryableGateSkip", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Primaries stuck at block 1000, fallbacks far ahead at block 10000.
		// Request block 9000 (0x2328). The gap from primary (1000) to 9000 is
		// 8000 blocks — well beyond the default MaxRetryableBlockDistance of
		// 128 — so checkUpstreamBlockAvailability classifies each primary's
		// skip as NON-retryable (ErrUpstreamRequestSkipped wrapping
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
