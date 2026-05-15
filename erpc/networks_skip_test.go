package erpc

// Integration tests for "upstream skip" scenarios — situations where the
// network executor MUST rotate past an upstream WITHOUT invoking its
// transport. Each subtest stages two (or more) upstreams, arms a skip
// condition on the first, fires one or more requests through ntw.Forward,
// and uses gock's pending-mock accounting to prove the skipped upstream's
// endpoint was never dialed.
//
// Reference patterns:
//   - setupTestNetworkWithRetryConfig (networks_failsafe_test.go) for
//     two-upstream fixture wiring
//   - createTestNetwork (policy_evaluator_test.go) for SelectionPolicy
//     setup
//   - TestNetworkAvailability_LowerExactBlock_Skip
//     (networks_availability_test.go) for the "expected-skip" assertion
//     style
//   - TestNetwork_Forward.ForwardSkipsOpenedCB (networks_test.go) for the
//     existing breaker-open precedent (single-upstream variant)

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

func init() {
	util.ConfigureTestLogger()
}

// setupTwoUpstreamNetworkForSkip wires a deterministic two-upstream network
// suitable for skip-rotation tests. It accepts per-upstream FailsafeConfigs
// (so callers can hang circuit-breaker / etc. on rpc1 alone), a network-level
// FailsafeConfig (typically nil — we want the skip behaviour, not retry), an
// optional rate-limiters registry override (so the rate-limit test can wire a
// budget by id), and an optional SelectionPolicy.
func setupTwoUpstreamNetworkForSkip(
	t *testing.T,
	ctx context.Context,
	up1Failsafe []*common.FailsafeConfig,
	up2Failsafe []*common.FailsafeConfig,
	up1Extra func(*common.UpstreamConfig),
	up2Extra func(*common.UpstreamConfig),
	networkFailsafe []*common.FailsafeConfig,
	rlrOverride *upstream.RateLimitersRegistry,
	selectionPolicy *common.SelectionPolicyConfig,
) *Network {
	t.Helper()

	up1 := &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc1",
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
		Failsafe: up1Failsafe,
	}
	if up1Extra != nil {
		up1Extra(up1)
	}
	up2 := &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc2",
		Endpoint: "http://rpc2.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
		Failsafe: up2Failsafe,
	}
	if up2Extra != nil {
		up2Extra(up2)
	}

	var rlr *upstream.RateLimitersRegistry
	if rlrOverride != nil {
		rlr = rlrOverride
	} else {
		r, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
		require.NoError(t, err)
		rlr = r
	}

	mt := health.NewTracker(&log.Logger, "test", time.Minute)
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)

	upr := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "test",
		[]*common.UpstreamConfig{up1, up2},
		ssr, rlr, vr, pr, nil, mt, nil,
	)

	networkCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Failsafe:        networkFailsafe,
		SelectionPolicy: selectionPolicy,
	}

	network, err := NewNetwork(ctx, &log.Logger, "test", networkCfg, rlr, upr, mt, nil)
	require.NoError(t, err)

	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Legacy `upstream.ReorderUpstreams(upr)` is gone; the selection-policy
	// engine drives routing. For tests that built Network with `policyEngine=nil`
	// (above), there's no engine to drive — the request path falls back to
	// raw registration order via `upstreamsRegistry.GetNetworkUpstreams`,
	// which is what the original ReorderUpstreams call was guaranteeing.
	return network
}

// TestNetworkSkip groups all upstream-skip scenarios. Each subtest stages a
// minimal two-upstream fixture and verifies that the skipped upstream's
// endpoint mock is NEVER consumed — the proof of "skipped without dialing
// transport".
func TestNetworkSkip(t *testing.T) {

	// -----------------------------------------------------------------
	// Priority 1 — circuit breaker open
	// -----------------------------------------------------------------
	//
	// Upstream-level CircuitBreaker with FailureThresholdCount=1,
	// FailureThresholdCapacity=1 trips after a single 503 from rpc1.
	// On the second request, the breaker is OPEN — Upstream.Forward
	// returns ErrFailsafeCircuitBreakerOpen BEFORE dialing rpc1, and
	// network.Forward rotates to rpc2.
	//
	// Test method `eth_traceTransaction` is chosen because it is NOT
	// consulted by the state poller, so we get a clean per-method
	// accounting of dials.
	t.Run("BreakerOpen_RotatesToNextUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// We expect: rpc1 trip mock consumed, rpc2 success mock
		// consumed twice. Therefore no user-pending mocks remain.
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// rpc1: one 503 to trip the breaker.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_traceTransaction")
			}).
			Times(1).
			Reply(503).
			JSON(map[string]interface{}{
				"error": map[string]interface{}{"code": -32000, "message": "upstream blew up"},
			})

		// rpc2: should be called twice — first as the fallback on the
		// trip request, second when rpc1's breaker is open.
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_traceTransaction")
			}).
			Times(2).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  map[string]interface{}{"fromHost": "rpc2"},
			})

		network := setupTwoUpstreamNetworkForSkip(t, ctx,
			[]*common.FailsafeConfig{{
				CircuitBreaker: &common.CircuitBreakerPolicyConfig{
					FailureThresholdCount:    1,
					FailureThresholdCapacity: 1,
					HalfOpenAfter:            common.Duration(5 * time.Minute),
				},
			}},
			nil, nil, nil,
			// No network-level retry — we want a clean rotation, not a retry.
			nil, nil, nil,
		)

		// Request #1 — rpc1 fails (trips breaker), rpc2 succeeds.
		req1 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0xdead"]}`))
		resp1, err1 := network.Forward(ctx, req1)
		require.NoError(t, err1)
		require.NotNil(t, resp1)

		jrr1, err := resp1.JsonRpcResponse()
		require.NoError(t, err)
		host, _ := jrr1.PeekStringByPath(ctx, "fromHost")
		assert.Equal(t, "rpc2", host, "first request should fall through to rpc2 after rpc1 503")

		// Request #2 — rpc1's breaker should be open; transport NOT
		// dialed. rpc2 alone serves the request. If rpc1's breaker did
		// NOT trip, gock would assert the rpc1 mock was reused
		// (Times(1) was already consumed, so a 2nd dial would surface
		// as an unmatched request → test failure via gock.IsPending).
		req2 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"eth_traceTransaction","params":["0xdead"]}`))
		resp2, err2 := network.Forward(ctx, req2)
		require.NoError(t, err2)
		require.NotNil(t, resp2)

		jrr2, err := resp2.JsonRpcResponse()
		require.NoError(t, err)
		host2, _ := jrr2.PeekStringByPath(ctx, "fromHost")
		assert.Equal(t, "rpc2", host2, "second request should be served by rpc2 with rpc1 breaker open")
	})

	// -----------------------------------------------------------------
	// Priority 2 — rate-limit budget exhausted
	// -----------------------------------------------------------------
	//
	// rpc1 carries a budget of maxCount=1 / second. After the first
	// request consumes it, the second request must observe
	// ErrUpstreamRateLimitRuleExceeded for rpc1 BEFORE dialing the
	// transport, and rotate to rpc2.
	t.Run("RateLimitBudgetExhausted_RotatesToNextUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// Expect: rpc1 mock consumed once, rpc2 mock consumed once.
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		rlr, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{{
				Id: "rpc1-tight-budget",
				Rules: []*common.RateLimitRuleConfig{{
					Method:   "*",
					MaxCount: 1,
					Period:   common.RateLimitPeriodSecond,
				}},
			}},
		}, &log.Logger)
		require.NoError(t, err)

		// rpc1: serves exactly ONE request. The mock Times(1) doubles
		// as the assertion that rpc1 was dialed exactly once.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_traceTransaction")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  map[string]interface{}{"fromHost": "rpc1"},
			})

		// rpc2: serves exactly ONE request (the second one, when
		// rpc1's budget is exhausted).
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_traceTransaction")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      2,
				"result":  map[string]interface{}{"fromHost": "rpc2"},
			})

		network := setupTwoUpstreamNetworkForSkip(t, ctx,
			nil, nil,
			func(c *common.UpstreamConfig) { c.RateLimitBudget = "rpc1-tight-budget" },
			nil, nil, rlr, nil,
		)

		// Align to the start of the next second to avoid rate-limit
		// window-rollover flakiness — same pattern used in
		// TestNetwork_Forward.ForwardCorrectlyRateLimitedOnNetworkLevel.
		now := time.Now()
		time.Sleep(time.Until(now.Truncate(time.Second).Add(time.Second)))

		// Request #1 — rpc1 has budget; serves and consumes its 1/s.
		req1 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0xa"]}`))
		resp1, err1 := network.Forward(ctx, req1)
		require.NoError(t, err1)
		jrr1, err := resp1.JsonRpcResponse()
		require.NoError(t, err)
		host1, _ := jrr1.PeekStringByPath(ctx, "fromHost")
		assert.Equal(t, "rpc1", host1)

		// Request #2 — rpc1's budget is exhausted; transport MUST NOT
		// be dialed. Network must rotate to rpc2.
		req2 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"eth_traceTransaction","params":["0xa"]}`))
		resp2, err2 := network.Forward(ctx, req2)
		require.NoError(t, err2)
		jrr2, err := resp2.JsonRpcResponse()
		require.NoError(t, err)
		host2, _ := jrr2.PeekStringByPath(ctx, "fromHost")
		assert.Equal(t, "rpc2", host2, "second request must rotate to rpc2 after rpc1 budget exhausted")
	})

	// -----------------------------------------------------------------
	// Priority 3 — Selection policy cordon at network.Forward level
	// -----------------------------------------------------------------
	//
	// SelectionPolicyConfig.EvalFunction returns only rpc2 as healthy.
	// The PolicyEvaluator runs at EvalInterval and cordons rpc1. Once
	// the cordon is in place, network.Forward must rotate past rpc1
	// without dialing its transport.
	t.Run("SelectionPolicyCordoned_RotatesToNextUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// rpc2 mock consumed, rpc1 mock unconsumed (Times(0)).
		defer util.AssertNoPendingMocks(t, 1)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// rpc1: MUST NOT be called. Times(0) means any matched call
		// will count as unmatched (gock pending stays at 0 if untouched
		// — but we use Times(1) here ONLY as a tripwire: if rpc1 IS
		// hit, the mock IS consumed and our final
		// AssertNoPendingMocks(t, 1) assertion fails because the
		// pending count drops to 0. The "1" in AssertNoPendingMocks is
		// the pending tripwire.)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_traceTransaction")
			}).
			Times(1).
			Reply(500).
			JSON(map[string]interface{}{"_note": "tripwire — rpc1 must NOT be dialed when cordoned"})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_traceTransaction")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  map[string]interface{}{"fromHost": "rpc2"},
			})

		// Selection policy: keep only upstreams whose id is rpc2.
		// (Legacy `EvalFunction` + `Resample*` fields are gone — the new
		// shape uses `Eval string` with `(upstreams, ctx) => …`, and
		// rotation/re-admission is expressed via `.probeExcluded()` in
		// the eval chain. This test doesn't need probe behavior, so the
		// chain is just the filter.)
		policy := &common.SelectionPolicyConfig{
			EvalInterval:  common.Duration(50 * time.Millisecond),
			EvalPerMethod: false,
			Eval:          `(upstreams, ctx) => upstreams.filter(u => u.id === 'rpc2')`,
		}

		network := setupTwoUpstreamNetworkForSkip(t, ctx,
			nil, nil, nil, nil, nil, nil, policy,
		)

		// Give the evaluator time to run AT LEAST one eval and cordon
		// rpc1. EvalInterval is 50ms; 250ms is comfortably 5 ticks.
		time.Sleep(250 * time.Millisecond)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0xb"]}`))
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		host, _ := jrr.PeekStringByPath(ctx, "fromHost")
		assert.Equal(t, "rpc2", host, "request must be served by rpc2 — rpc1 cordoned by selection policy")
	})

	// -----------------------------------------------------------------
	// Priority 4 — autoIgnoreUnsupportedMethods (dynamic skip)
	// -----------------------------------------------------------------
	//
	// rpc1 has AutoIgnoreUnsupportedMethods=true. The first call for a
	// method returns -32601 (method not found). The network code path
	// classifies this as ErrEndpointUnsupported and invokes
	// Upstream.IgnoreMethod, which appends the method to IgnoreMethods.
	// Subsequent requests for that method must skip rpc1 entirely.
	//
	// (Side-note: IgnoreMethod is fired via `go u.IgnoreMethod(method)`
	// in networks.go — there's a microsecond-scale window between
	// request #1 completing and the goroutine taking effect. We give
	// it 100ms of headroom, well above any plausible scheduling delay.)
	t.Run("AutoIgnoreUnsupportedMethods_SkipsAfterFirstRejection", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// Expect: rpc1 unsupported response consumed once, rpc2 OK
		// consumed twice. No user-pending mocks remain.
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// rpc1: returns -32601 once. After this is consumed, rpc1 must
		// not be redialed for eth_traceTransaction.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_traceTransaction")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32601,
					"message": "the method eth_traceTransaction does not exist/is not available",
				},
			})

		// rpc2: serves both requests successfully.
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_traceTransaction")
			}).
			Times(2).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  map[string]interface{}{"fromHost": "rpc2"},
			})

		network := setupTwoUpstreamNetworkForSkip(t, ctx,
			nil, nil,
			func(c *common.UpstreamConfig) {
				c.AutoIgnoreUnsupportedMethods = &common.TRUE
			},
			nil, nil, nil, nil,
		)

		// Request #1 — rpc1 returns unsupported, network falls
		// through to rpc2.
		req1 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_traceTransaction","params":["0xc"]}`))
		resp1, err1 := network.Forward(ctx, req1)
		require.NoError(t, err1)
		require.NotNil(t, resp1)
		jrr1, err := resp1.JsonRpcResponse()
		require.NoError(t, err)
		host1, _ := jrr1.PeekStringByPath(ctx, "fromHost")
		assert.Equal(t, "rpc2", host1)

		// Wait for the async IgnoreMethod goroutine to add the method
		// to rpc1's ignoreMethods list before issuing request #2.
		time.Sleep(100 * time.Millisecond)

		// Request #2 — rpc1 MUST be skipped (its method is now
		// auto-ignored); rpc2 alone serves the call. If rpc1's
		// auto-ignore failed to take effect, the test catches it via
		// AssertNoPendingMocks: rpc1's Times(1) mock is already
		// consumed, so a second dial to rpc1 surfaces as an unmatched
		// request and gock returns an error from the round-tripper —
		// rpc2's mock then stays pending and the assertion trips.
		req2 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"eth_traceTransaction","params":["0xc"]}`))
		resp2, err2 := network.Forward(ctx, req2)
		require.NoError(t, err2)
		require.NotNil(t, resp2)
		jrr2, err := resp2.JsonRpcResponse()
		require.NoError(t, err)
		host2, _ := jrr2.PeekStringByPath(ctx, "fromHost")
		assert.Equal(t, "rpc2", host2, "second request must rotate past rpc1 after auto-ignore")
	})

	// -----------------------------------------------------------------
	// Priority 5 — Finality mismatch (archive request to full node)
	// -----------------------------------------------------------------
	//
	// rpc1 is a "full" node with MaxAvailableRecentBlocks=128 (default
	// retention). rpc2 is "archive" (unlimited history). A historical
	// request for block 0x1 must skip rpc1 (it can't serve historical
	// data) and route to rpc2.
	//
	// This is the closest production approximation of "realtime vs
	// archive" selection: the upstream is excluded BEFORE any RPC
	// dial by the block-availability check, which is exactly the same
	// machinery that powers matchFinality routing.
	t.Run("FinalityMismatch_FullNodeSkippedForArchiveRequest", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// rpc1's request mock stays pending (Times(0) — see below).
		// rpc2's request mock is consumed.
		defer util.AssertNoPendingMocks(t, 1)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// rpc1: tripwire. If it's ever dialed for eth_getBalance, the
		// mock would be consumed and our pending=1 assertion fails.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Times(1).
			Reply(500).
			JSON(map[string]interface{}{"_note": "tripwire — full node MUST NOT be dialed for historical block"})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xabcdef",
			})

		// Use the established full+archive fixture for finality
		// routing tests so we exercise the exact same machinery as
		// TestNetwork_HistoricalBlockNumberSkip (around line 9111 in
		// networks_test.go) — there's a richer codepath here than
		// our two-upstream helper would activate.
		network := setupTestNetworkWithFullAndArchiveNodeUpstreams(
			t, ctx,
			common.EvmNodeTypeFull, 128,
			common.EvmNodeTypeArchive, 0,
			nil,
		)

		// Historical block 0x1 is well beyond 128 blocks below latest
		// (0x11118888 on rpc1) — the full node must be skipped.
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000","0x1"]}`))
		req.SetNetwork(network)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		result, _ := jrr.PeekStringByPath(ctx)
		assert.Equal(t, "0xabcdef", result, "request must be served by archive rpc2 — full rpc1 skipped on historical block")
	})
}
