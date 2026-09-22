// Network-level regression guards for the SVM correctness-hardening round.
//
// Each test here pins a behavior that is only observable once the whole
// pipeline is wired together — the hook-level unit tests in architecture/svm
// cannot see the upstream sweep, the cache DAL, upstream selection, or the HTTP
// status code, because those live outside the hook.
//
// Conventions follow svm_e2e_test.go: real Network + real UpstreamsRegistry +
// real health tracker, only the upstream HTTP endpoints mocked with gock. gock
// is process-global state, so nothing in this file may run t.Parallel(), and
// every test uses its own set of hostnames.
package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/svm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	policystdlib "github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// svmCountedMock registers a persistent gock mock for (host, method) and hands
// back a counter of how many requests for that method actually reached that
// host.
//
// Counting inside the Filter is exact rather than approximate: gock invokes a
// mock's filter only while matching a real outbound request and stops at the
// first mock that matches, so one increment means this host really did receive
// this method once. That is the assertion the non-retryable-write guard needs —
// "the second upstream was never contacted" is a statement about wire traffic,
// not about which value the caller happened to get back.
func svmCountedMock(host, method string, times int, status int, body string) *atomic.Int64 {
	n := &atomic.Int64{}
	m := gock.New("http://" + host).Post("")
	if times > 0 {
		m = m.Times(times)
	} else {
		m = m.Persist()
	}
	m.Filter(func(r *http.Request) bool {
		if r.URL.Host != host {
			return false
		}
		if !strings.Contains(util.SafeReadBody(r), `"method":"`+method+`"`) {
			return false
		}
		n.Add(1)
		return true
	}).
		Reply(status).
		BodyString(body)
	return n
}

// ──────────────────────────────────────────────────────────────────────
// Non-idempotent write guard (network scope)
// ──────────────────────────────────────────────────────────────────────

// TestSvm_NonRetryableWrite_NeverDispatchedToSecondUpstream is the duplicate-mint
// / duplicate-broadcast guard, asserted where it actually matters: the network
// sweep, not the hook in isolation.
//
// HandleUpstreamPostForward gates on svm.IsNonRetryableWriteMethod, which wraps
// the upstream error as a ClientSideException with retryableTowardNetwork=false
// so the sweep stops instead of trying the next upstream. A server-side 5xx is
// the dangerous case: the request may well have taken effect before the error
// was produced, so "the server failed" is NOT permission to run it again.
//
//   - requestAirdrop MINTS lamports per call. A second dispatch is a real
//     duplicate mint. This is the method the fix generalized TO, and it shipped
//     with no network-level test.
//   - sendTransaction is the method the fix generalized FROM — kept here so the
//     generalization cannot silently drop it.
//   - The match is case-insensitive, so a mis-cased method name cannot walk past
//     the guard.
func TestSvm_NonRetryableWrite_NeverDispatchedToSecondUpstream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		host   string
		method string
		params string
	}{
		{"requestAirdrop", "airdrop", "requestAirdrop", `["pubkey", 1000000000]`},
		{"sendTransaction", "sendtx", "sendTransaction", `["base64tx"]`},
		{"mis-cased requestairdrop", "aircase", "requestairdrop", `["pubkey", 1000000000]`},
		{"sendRawTransaction", "sendraw", "sendRawTransaction", `["base64tx"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No t.Parallel: gock's mock registry is process-global.
			util.ResetGock()
			defer util.ResetGock()

			h1 := "svm-nrw-" + tc.host + "-rpc1.localhost"
			h2 := "svm-nrw-" + tc.host + "-rpc2.localhost"
			util.SetupMocksForSvmStatePoller(h1, 1000, 990)
			util.SetupMocksForSvmStatePoller(h2, 1000, 990)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Primary fails with a SERVER-side error — the one class that would
			// otherwise be retried across upstreams.
			primary := svmCountedMock(h1, tc.method, 0, 500,
				`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Internal error"}}`)
			// Secondary would happily succeed. If the guard regresses, this mock
			// gets consumed — that IS the duplicate effect on a real cluster.
			secondary := svmCountedMock(h2, tc.method, 0, 200,
				`{"jsonrpc":"2.0","id":1,"result":"DuplicateEffectSignature"}`)

			net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
				svmUpstreamConfig("rpc1", h1),
				svmUpstreamConfig("rpc2", h2),
			})
			net.PinUpstreamOrderForTest("rpc1", "rpc2")

			req := common.NewNormalizedRequest(fmt.Appendf(nil,
				`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, tc.method, tc.params))
			resp, err := svmProjectForward(ctx, net, req)

			require.EqualValues(t, 1, primary.Load(),
				"the primary must receive the write exactly once")
			require.Zero(t, secondary.Load(),
				"%s was re-dispatched to a second upstream after an effective first attempt", tc.method)

			// The failure must surface, not be papered over by a silent failover.
			require.Error(t, err, "the primary's error must reach the caller")
			if resp != nil {
				if jrr, jerr := resp.JsonRpcResponse(); jerr == nil && jrr != nil {
					assert.NotContains(t, string(jrr.GetResultBytes()), "DuplicateEffectSignature")
				}
			}
		})
	}
}

// TestSvm_ReadMethod_StillFailsOverInTheSameSetup is the control for the test
// above: identical topology and identical primary failure, but a READ method.
// Without this, a guard that accidentally marked every SVM error non-retryable
// would pass the write tests while silently disabling failover for the entire
// architecture.
func TestSvm_ReadMethod_StillFailsOverInTheSameSetup(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	h1 := "svm-nrw-read-rpc1.localhost"
	h2 := "svm-nrw-read-rpc2.localhost"
	util.SetupMocksForSvmStatePoller(h1, 1000, 990)
	util.SetupMocksForSvmStatePoller(h2, 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	primary := svmCountedMock(h1, "getBalance", 0, 500,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Internal error"}}`)
	secondary := svmCountedMock(h2, "getBalance", 0, 200,
		`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1000},"value":777}}`)

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", h1),
		svmUpstreamConfig("rpc2", h2),
	})
	net.PinUpstreamOrderForTest("rpc1", "rpc2")

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["pubkey"]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, string(jrr.GetResultBytes()), "777",
		"a read method must still fail over to the healthy upstream")
	require.EqualValues(t, 1, primary.Load())
	require.EqualValues(t, 1, secondary.Load(),
		"the guard must be method-scoped; reads still sweep to the next upstream")
}

// ──────────────────────────────────────────────────────────────────────
// Stale-forever cache regression (the full chain, not one classification)
// ──────────────────────────────────────────────────────────────────────

// svmMinimalCachePolicyNetwork attaches a cache built from the MINIMAL policy an
// operator can write: a connector and nothing else.
//
// That minimality is the whole point. CachePolicyConfig.SetDefaults fills
// network and method with "*", the finality field keeps its zero value — which
// is DataFinalityStateFinalized, not "any" — and the TTL pointer stays nil,
// which every connector reads as "no expiry". So this one-line policy means
// "cache anything classified Finalized, forever". The stale-forever P1 was that
// chain, not any single link: only the finality classification changed, so a
// test that stubs the policy out cannot see the regression.
func svmMinimalCachePolicyNetwork(t *testing.T, ctx context.Context, net *Network) {
	t.Helper()
	minimal := &common.CachePolicyConfig{Connector: "mem"}
	require.NoError(t, minimal.SetDefaults())
	require.Equal(t, "*", minimal.Network)
	require.Equal(t, "*", minimal.Method)
	require.Nil(t, minimal.TTL, "an unset TTL is what makes a matched entry permanent")

	cache, err := svm.NewSvmJsonRpcCache(ctx, &log.Logger, &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{
			{Id: "mem", Driver: common.DriverMemory,
				Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "1MB"}},
		},
		Policies: []*common.CachePolicyConfig{minimal},
	})
	require.NoError(t, err)
	net.cacheDal = cache
}

// TestSvm_MinimalCachePolicy_MovingHeadReadNeverGoesStaleForever reproduces the
// stale-forever P1 end to end, and its mirror image in the same breath.
//
// Under one minimal opt-in cache policy (see svmMinimalCachePolicyNetwork):
//
//   - getBalance at commitment:finalized is a MOVING-HEAD read. Solana's
//     `finalized` is the state at the latest ROOTED slot and that head advances
//     roughly every 400ms, so the answer changes with every transfer. It must be
//     classified Realtime, never match this Finalized-only policy, and therefore
//     never be served from a permanent entry. Before the fix the client received
//     the first observed balance forever.
//
//   - getBlock(<slot>) at the same commitment is SLOT-PINNED: once that slot is
//     rooted the answer can never change. It must still be served from cache.
//     Over-correcting into "cache nothing at finalized" would destroy the cache's
//     entire value for the ETL workload, so both directions are asserted here
//     against the same policy — a fix in either direction alone fails this test.
func TestSvm_MinimalCachePolicy_MovingHeadReadNeverGoesStaleForever(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	host := "svm-stale-rpc1.localhost"
	util.SetupMocksForSvmStatePoller(host, 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Balance answer A, then the on-chain balance changes and the upstream
	// answers B. Registration order is the serving order in gock, so the
	// Times(1) mock covers the first call and the persistent one every call
	// after it.
	firstBalance := svmCountedMock(host, "getBalance", 1, 200,
		`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1000},"value":111}}`)
	laterBalance := svmCountedMock(host, "getBalance", 0, 200,
		`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1001},"value":222}}`)

	// The same shape for a slot-pinned read: if the second answer is ever
	// observed by the caller, the cache failed to serve an immutable response.
	firstBlock := svmCountedMock(host, "getBlock", 1, 200,
		`{"jsonrpc":"2.0","id":1,"result":{"blockhash":"immutable-block-42"}}`)
	laterBlock := svmCountedMock(host, "getBlock", 0, 200,
		`{"jsonrpc":"2.0","id":1,"result":{"blockhash":"SHOULD-NEVER-BE-SERVED"}}`)

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{svmUpstreamConfig("rpc1", host)})
	svmMinimalCachePolicyNetwork(t, ctx, net)

	balanceReq := []byte(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["SoLAddr111",{"commitment":"finalized"}]}`)
	blockReq := []byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[42,{"commitment":"finalized"}]}`)

	resultOf := func(body []byte) string {
		t.Helper()
		resp, err := svmProjectForward(ctx, net, common.NewNormalizedRequest(body))
		require.NoError(t, err)
		require.NotNil(t, resp)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		return string(jrr.GetResultBytes())
	}

	// --- moving-head read: must NOT be pinned to its first observed value ---
	require.Contains(t, resultOf(balanceReq), "111")
	// Let ristretto flush its admission buffer, so a wrongly-written entry has
	// every chance to become visible before the second read.
	time.Sleep(100 * time.Millisecond)
	require.Contains(t, resultOf(balanceReq), "222",
		"getBalance at commitment:finalized was served from a permanent cache entry; "+
			"the balance is pinned to its first observed value forever")
	require.EqualValues(t, 1, firstBalance.Load())
	require.EqualValues(t, 1, laterBalance.Load(),
		"the second getBalance must reach the upstream, not the cache")

	// --- slot-pinned read: must STILL be served from cache -----------------
	require.Contains(t, resultOf(blockReq), "immutable-block-42")
	time.Sleep(100 * time.Millisecond)
	require.Contains(t, resultOf(blockReq), "immutable-block-42",
		"getBlock(slot) at commitment:finalized must still be served from cache — "+
			"over-correcting the moving-head fix destroys the cache for the ETL workload")
	require.EqualValues(t, 1, firstBlock.Load())
	require.Zero(t, laterBlock.Load(),
		"a second getBlock reached the upstream; the immutable response was not cached")
}

// ──────────────────────────────────────────────────────────────────────
// Health verdict → upstream selection
// ──────────────────────────────────────────────────────────────────────

// setupSvmSelectionPolicyNetwork is setupTestSvmNetwork plus a real policy
// engine running the production default policy (whose first chain step is
// `.removeCordoned()`), with the eval ticker frozen so tests drive ticks. This
// is what makes the poller's cordon verdict observable as a ROUTING decision
// rather than just a tracker flag.
func setupSvmSelectionPolicyNetwork(t *testing.T, ctx context.Context, upstreamConfigs []*common.UpstreamConfig) (*Network, *upstream.UpstreamsRegistry) {
	t.Helper()

	for _, c := range upstreamConfigs {
		// Opt out of probe traffic: the default policy mirrors shadow requests
		// to excluded upstreams, which would falsify "excluded ⇒ zero requests".
		if c.Routing == nil {
			c.Routing = &common.UpstreamRoutingConfig{}
		}
		if c.Routing.Probe == "" {
			c.Routing.Probe = common.ProbeModeOff
		}
	}

	rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)
	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
		LockMaxWait:     common.Duration(200 * time.Millisecond),
		UpdateMaxWait:   common.Duration(200 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second),
		LockTtl:         common.Duration(4 * time.Second),
	}
	sharedStateCfg.SetDefaults("test")
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	require.NoError(t, err)

	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "test",
		upstreamConfigs, ssr, rateLimitersRegistry, vr, pr,
		nil, metricsTracker, nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm: &common.SvmNetworkConfig{
			Cluster:    "mainnet-beta",
			Commitment: "confirmed",
		},
		SelectionPolicy: &common.SelectionPolicyConfig{EvalInterval: 0}, // frozen; tests tick
	}
	policyEngine := policy.NewEngine(ctx, &log.Logger, "test", metricsTracker, policystdlib.Install, nil)
	network, err := NewNetwork(
		ctx, &log.Logger, "test", networkConfig,
		rateLimitersRegistry, upstreamsRegistry, metricsTracker, policyEngine,
	)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.SvmNetworkId("", "mainnet-beta")))
	require.NoError(t, network.Bootstrap(ctx))

	require.Eventually(t, func() bool {
		return len(upstreamsRegistry.GetNetworkUpstreams(ctx, network.networkId)) == len(upstreamConfigs)
	}, 5*time.Second, 20*time.Millisecond, "registry never converged on the full upstream set")

	return network, upstreamsRegistry
}

// svmPollerFor returns the SVM state poller of the named upstream.
func svmPollerFor(t *testing.T, reg *upstream.UpstreamsRegistry, ctx context.Context, networkId, id string) common.SvmStatePoller {
	t.Helper()
	for _, u := range reg.GetNetworkUpstreams(ctx, networkId) {
		if u.Id() == id {
			p := u.SvmStatePoller()
			require.NotNil(t, p, "upstream %q has no svm state poller", id)
			return p
		}
	}
	t.Fatalf("upstream %q not registered", id)
	return nil
}

// svmAwaitHealthVerdict drives the poller until IsHealthy() reads `want`, then
// returns. Bounded — it fails the test rather than hanging.
//
// A single Poll() call is NOT enough to force a fresh sample: Bootstrap seeds
// the debounce gate with DefaultPollInterval, so Poll returns nil without doing
// any I/O when the background ticker already sampled inside the last 400ms.
// Asserting on IsHealthy() straight after one Poll() therefore reads whichever
// verdict the last background tick happened to leave behind — which is exactly
// how the recovery phase used to observe the pre-recovery lag. Re-driving Poll
// on a short interval means the first non-debounced tick lands the new sample,
// and Solana's own 400ms cadence is what makes that bounded rather than flaky.
func svmAwaitHealthVerdict(t *testing.T, ctx context.Context, p common.SvmStatePoller, want bool, msg string) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		_ = p.Poll(ctx)
		return p.IsHealthy() == want
	}, 5*time.Second, 50*time.Millisecond,
		"%s (healthy=%v shredLag=%d)", msg, p.IsHealthy(), p.MaxShredInsertSlotLag())
}

// svmRepolicy wipes the traffic metrics the bootstrap/state-poller phase left
// behind and re-ticks the policy, so each phase's routing decision is made
// against the health verdict under test rather than against uneven request
// counts accumulated during setup.
//
// It deliberately does NOT call TrackedMetrics.Reset(): that helper also does
// `Cordoned.Store(false)` + `LastCordonedReason.Store("")`, which would erase
// the very cordon this test exists to observe, leaving `removeCordoned()`
// nothing to act on and turning the routing assertion into a tautology. Only
// the cumulative TRAFFIC components are wiped here; cordon state and the
// state-poller gauges (BlockHeadLag / FinalizationLag) are verdicts, not
// counts, and must survive into the eval under test.
func svmRepolicy(t *testing.T, net *Network, reg *upstream.UpstreamsRegistry, ctx context.Context) {
	t.Helper()
	for _, ups := range reg.GetNetworkUpstreams(ctx, net.networkId) {
		if m := net.metricsTracker.GetUpstreamMethodMetrics(ups, "*", common.DataFinalityStateAll); m != nil {
			m.ErrorsTotal.Wipe()
			m.RequestsTotal.Wipe()
			m.RemoteRateLimitedTotal.Wipe()
			m.MisbehaviorsTotal.Wipe()
			m.ResponseQuantiles.Reset()
		}
	}
	policy.ResetSlotStateForTest(net.policyEngine, net.networkId, "*")
	policy.TickForTest(net.policyEngine, net.networkId, "*")
}

// TestSvm_UnhealthyUpstream_ExcludedFromRouting closes the loop the poller unit
// tests cannot: they prove Cordon/Uncordon fire on the right edges, this proves
// SELECTION honors them. Without it the whole silent-stale defense is
// decorative — a verdict nobody routes on.
//
// The degradation is a runaway shred-insert lag: the node keeps ingesting shreds
// (maxShredInsertSlot races ahead) while replay stalls at the processed slot, so
// getHealth still answers "ok" while every read it serves is stale. That is
// exactly the failure mode getHealth alone cannot catch.
//
// Recovery is driven by SuggestLatestSlot — the production signal that live
// traffic observed the node's replay catching up to the shred watermark —
// rather than by re-writing mocks mid-flight, which keeps the test deterministic
// against the 400ms background ticker.
func TestSvm_UnhealthyUpstream_ExcludedFromRouting(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	h1 := "svm-cordon-rpc1.localhost"
	h2 := "svm-cordon-rpc2.localhost"
	// rpc1: shred watermark 600 slots AHEAD of the replayed slot — ingestion is
	// fine, replay is stuck. Lag 600 > common.MaxShredInsertSlotLagThreshold.
	util.SetupMocksForSvmStatePollerWithShred(h1, 1000, 990, 1600)
	// rpc2: watermark level with the processed slot — healthy.
	util.SetupMocksForSvmStatePollerWithShred(h2, 1000, 990, 1000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// rpc1 answers forever; rpc2 answers exactly once. The single-use mock is
	// what makes the recovery phase a real assertion: once it is spent, only a
	// re-admitted rpc1 can serve the request at all.
	rpc1Hits := svmCountedMock(h1, "getBalance", 0, 200,
		`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1000},"value":1}}`)
	rpc2Hits := svmCountedMock(h2, "getBalance", 1, 200,
		`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1000},"value":2}}`)

	net, reg := setupSvmSelectionPolicyNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", h1),
		svmUpstreamConfig("rpc2", h2),
	})
	poller1 := svmPollerFor(t, reg, ctx, net.networkId, "rpc1")
	poller2 := svmPollerFor(t, reg, ctx, net.networkId, "rpc2")

	req := func() *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["pubkey"]}`))
	}

	// --- phase 1: rpc1 degraded, must be routed around ---------------------
	svmAwaitHealthVerdict(t, ctx, poller1, false, "shred lag 600 must read as unhealthy")
	svmAwaitHealthVerdict(t, ctx, poller2, true, "a level watermark must read as healthy")
	svmRepolicy(t, net, reg, ctx)

	require.NotContains(t, net.PolicyOrderedUpstreams("*"), "rpc1",
		"a cordoned upstream must be dropped from the selection order")

	rpc1Before := rpc1Hits.Load()
	resp, err := svmProjectForward(ctx, net, req())
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, string(jrr.GetResultBytes()), `"value":2`,
		"the request must be served by the healthy upstream")
	require.Equal(t, rpc1Before, rpc1Hits.Load(),
		"the unhealthy upstream received user traffic despite being cordoned")
	require.EqualValues(t, 1, rpc2Hits.Load())

	// --- phase 2: replay catches up, rpc1 must be selectable again ---------
	// Live traffic observes rpc1 at slot 1600, level with its shred watermark,
	// so the ingestion lag is gone. (The poller's own getSlot still answers
	// 1000, but that is a rollback within the tolerated window and the shared
	// counter ignores it, so the recovered view survives background ticks.)
	poller1.SuggestLatestSlot(1600)
	svmAwaitHealthVerdict(t, ctx, poller1, true, "lag back to zero must read as healthy")
	svmRepolicy(t, net, reg, ctx)

	require.Contains(t, net.PolicyOrderedUpstreams("*"), "rpc1",
		"a recovered upstream must be re-admitted to the selection order")

	// rpc2's mock is spent, so this can only succeed via rpc1.
	resp, err = svmProjectForward(ctx, net, req())
	require.NoError(t, err, "no upstream served the request after recovery")
	jrr, err = resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, string(jrr.GetResultBytes()), `"value":1`,
		"the recovered upstream must serve traffic again")
	require.Positive(t, rpc1Hits.Load(),
		"the recovered upstream never received traffic; uncordon had no routing effect")
}

// ──────────────────────────────────────────────────────────────────────
// HTTP-level auth / rate-limit classification
// ──────────────────────────────────────────────────────────────────────

// svmClientErrorCode runs the error through the production response builder and
// returns the JSON-RPC code the client actually receives. Going through
// buildErrorResponseBody rather than reimplementing its unwrap chain is the
// point: the assertion is about the bytes on the wire, not about an internal
// error type.
func svmClientErrorCode(t *testing.T, req *common.NormalizedRequest, err error) common.JsonRpcErrorNumber {
	t.Helper()
	body := buildErrorResponseBody(req, err, err, nil)
	jrErr, ok := body.(*HttpJsonRpcErrorResponse)
	require.Truef(t, ok, "expected a json-rpc error response, got %T: %v", body, body)
	obj, ok := jrErr.Error.(map[string]interface{})
	require.Truef(t, ok, "expected an error object, got %T", jrErr.Error)
	code, ok := obj["code"].(common.JsonRpcErrorNumber)
	require.Truef(t, ok, "expected a numeric error code, got %T (%v)", obj["code"], obj["code"])
	return code
}

// TestSvm_HttpStatusClassification_WithJsonRpcErrorBody pins auth and quota
// classification for the shape vendors actually send: a non-2xx status AND a
// JSON-RPC error object in the same response. Reading the body first — which is
// what the old code did — sent an expired API key down whichever generic path
// its code happened to map to, so it was retried across every upstream and
// never reached eRPC's unauthorized handling.
//
// The wire-code assertions are the other half. common.JsonRpcErrorNumber reuses
// -32005 for CapacityExceeded and -32016 for Unauthorized, while agave assigns
// those to NodeUnhealthy and MinContextSlotNotReached. On an SVM path eRPC must
// therefore never SYNTHESIZE either number: a client that reads -32005 backs off
// waiting for a validator to catch up, and one that reads -32016 retries against
// a fresher node forever — neither ever fixes its API key or slows down.
func TestSvm_HttpStatusClassification_WithJsonRpcErrorBody(t *testing.T) {
	for _, tc := range []struct {
		name      string
		host      string
		status    int
		body      string
		wantCode  common.ErrorCode
		wantWire  common.JsonRpcErrorNumber
		forbidden common.JsonRpcErrorNumber
	}{
		{
			name:      "401 with json-rpc body",
			host:      "auth401",
			status:    http.StatusUnauthorized,
			body:      `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"invalid api key provided"}}`,
			wantCode:  common.ErrCodeEndpointUnauthorized,
			wantWire:  -32603, // the upstream's own code, passed through verbatim
			forbidden: -32016, // agave's MinContextSlotNotReached
		},
		{
			name:      "403 with json-rpc body",
			host:      "auth403",
			status:    http.StatusForbidden,
			body:      `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"account suspended"}}`,
			wantCode:  common.ErrCodeEndpointUnauthorized,
			wantWire:  -32603,
			forbidden: -32016,
		},
		{
			name:      "429 with json-rpc body",
			host:      "rate429",
			status:    http.StatusTooManyRequests,
			body:      `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"429 Too Many Requests"}}`,
			wantCode:  common.ErrCodeEndpointCapacityExceeded,
			wantWire:  -32000, // agave's generic server-error bucket
			forbidden: -32005, // agave's NodeUnhealthy
		},
		{
			// A 429 whose body parses as JSON-RPC but carries no error member —
			// the shape an edge proxy produces when it rate-limits a request the
			// origin already answered. Here the status is the only signal, so
			// this is the row that proves the status is consulted at all.
			name:      "429 with a bodied json-rpc response carrying no error",
			host:      "rate429noerr",
			status:    http.StatusTooManyRequests,
			body:      `{"jsonrpc":"2.0","id":1,"result":null}`,
			wantCode:  common.ErrCodeEndpointCapacityExceeded,
			wantWire:  -32000,
			forbidden: -32005,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No t.Parallel: gock's mock registry is process-global.
			util.ResetGock()
			defer util.ResetGock()

			host := "svm-http-" + tc.host + ".localhost"
			util.SetupMocksForSvmStatePoller(host, 1000, 990)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			svmCountedMock(host, "getBalance", 0, tc.status, tc.body)

			net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
				svmUpstreamConfig("rpc1", host),
			})

			req := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["pubkey"]}`))
			_, err := svmProjectForward(ctx, net, req)
			require.Error(t, err)
			require.Truef(t, common.HasErrorCode(err, tc.wantCode),
				"HTTP %d with body %q must classify as %s, got: %v", tc.status, tc.body, tc.wantCode, err)

			got := svmClientErrorCode(t, req, err)
			require.EqualValues(t, tc.wantWire, got,
				"client-visible json-rpc code changed for HTTP %d", tc.status)
			require.NotEqualValues(t, tc.forbidden, got,
				"eRPC synthesized %d on an SVM path; a Solana client reads that as an agave condition, not an eRPC verdict",
				tc.forbidden)
		})
	}
}

// ──────────────────────────────────────────────────────────────────────
// Bare (non-JSON) HTTP failure classification
// ──────────────────────────────────────────────────────────────────────

// TestSvm_BareHttpFailure_ClassifiedFromStatusAndFailsOver covers the response
// shape the extractor's `jr == nil || jr.Error == nil` branch was WRITTEN for
// but could never actually reach.
//
// NormalizedResponse.JsonRpcResponse() never returns a nil jr for an
// unparseable body: it SYNTHESIZES a -32700 error object (common/response.go).
// So a 429 whose body is Cloudflare's HTML page, nginx's plaintext line, or
// nothing at all arrived at the extractor looking exactly like a JSON-RPC
// -32700 — and the old -32700 case wrapped it as a ClientSideException with
// retryableTowardNetwork=false. A CDN rate-limiting one provider therefore
// failed the whole request with a parse error: no failover to a healthy
// upstream, no capacity signal, and a caller told its own JSON was malformed.
//
// Every row is measured end to end over the real HTTP path. Hand-constructing
// `jr = nil` is precisely what let this hide — a unit test on the extractor
// passes a nil jr the production pipeline can never produce, so it exercised a
// branch no request reaches.
//
// Each row asserts both halves, because either alone is satisfiable by a wrong
// fix: the CLASSIFICATION (single upstream — which StandardError class, and
// which code the client actually reads) and the ROUTING CONSEQUENCE (two
// upstreams — the request must reach the healthy one).
func TestSvm_BareHttpFailure_ClassifiedFromStatusAndFailsOver(t *testing.T) {
	for _, tc := range []struct {
		name     string
		host     string
		status   int
		body     string
		wantCode common.ErrorCode
		wantWire common.JsonRpcErrorNumber
	}{
		{
			// nginx / HAProxy in front of a provider.
			name:     "429 plaintext",
			host:     "bare429text",
			status:   http.StatusTooManyRequests,
			body:     "Too Many Requests\n",
			wantCode: common.ErrCodeEndpointCapacityExceeded,
			wantWire: -32000,
		},
		{
			// Cloudflare's interstitial — valid HTML, no JSON anywhere.
			name:     "429 html",
			host:     "bare429html",
			status:   http.StatusTooManyRequests,
			body:     "<html><head><title>429 Too Many Requests</title></head><body><h1>Rate limited</h1></body></html>",
			wantCode: common.ErrCodeEndpointCapacityExceeded,
			wantWire: -32000,
		},
		{
			// A bodiless 429: the status is the ONLY signal in the response.
			name:     "429 empty body",
			host:     "bare429empty",
			status:   http.StatusTooManyRequests,
			body:     "",
			wantCode: common.ErrCodeEndpointCapacityExceeded,
			wantWire: -32000,
		},
		{
			// Load balancer with no healthy backend.
			name:     "503 plaintext",
			host:     "bare503text",
			status:   http.StatusServiceUnavailable,
			body:     "no healthy upstream",
			wantCode: common.ErrCodeEndpointServerSideException,
			wantWire: -32603,
		},
		{
			// HTTP 200 with a truncated / garbage body. No failing status to
			// classify from, so the -32700 case itself must hold the line: the
			// upstream produced bytes eRPC cannot parse, which is that
			// UPSTREAM's fault, not the caller's — retryable, and the raw
			// -32700 still reaches the client.
			name:     "200 unparseable body",
			host:     "bare200garbage",
			status:   http.StatusOK,
			body:     `{"jsonrpc":"2.0","id":1,"result":{"conte`,
			wantCode: common.ErrCodeEndpointServerSideException,
			wantWire: -32700,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// --- classification: one upstream, so the verdict reaches the caller ---
			func() {
				// No t.Parallel: gock's mock registry is process-global.
				util.ResetGock()
				defer util.ResetGock()

				host := "svm-bare-" + tc.host + ".localhost"
				util.SetupMocksForSvmStatePoller(host, 1000, 990)

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				svmCountedMock(host, "getBalance", 0, tc.status, tc.body)
				net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
					svmUpstreamConfig("rpc1", host),
				})

				req := common.NewNormalizedRequest([]byte(
					`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["pubkey"]}`))
				_, err := svmProjectForward(ctx, net, req)
				require.Error(t, err)
				require.Truef(t, common.HasErrorCode(err, tc.wantCode),
					"HTTP %d with a non-JSON body must classify as %s, got: %v", tc.status, tc.wantCode, err)

				// The regression itself: a bare HTTP failure must never land in
				// the caller-side bucket. That class is what stops the sweep.
				require.Falsef(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException),
					"HTTP %d with a non-JSON body was blamed on the caller; the sweep stops and no upstream failover happens", tc.status)

				got := svmClientErrorCode(t, req, err)
				require.EqualValues(t, tc.wantWire, got,
					"client-visible json-rpc code changed for HTTP %d with a non-JSON body", tc.status)
				// -32005 is eRPC's CapacityExceeded AND agave's NodeUnhealthy.
				// A Solana client that reads it on a rate-limited response backs
				// off waiting for a validator to catch up instead of slowing its
				// own request rate, and eRPC's own quota verdict is already
				// carried by the outer class plus the 429 status.
				require.NotEqualValues(t, common.JsonRpcErrorCapacityExceeded, got,
					"eRPC synthesized -32005 for a rate limit; a Solana client decodes that as agave NodeUnhealthy")
			}()

			// --- routing consequence: the request must reach a healthy node ---
			util.ResetGock()
			defer util.ResetGock()

			h1 := "svm-bare-" + tc.host + "-p.localhost"
			h2 := "svm-bare-" + tc.host + "-s.localhost"
			util.SetupMocksForSvmStatePoller(h1, 1000, 990)
			util.SetupMocksForSvmStatePoller(h2, 1000, 990)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			primary := svmCountedMock(h1, "getBalance", 0, tc.status, tc.body)
			secondary := svmCountedMock(h2, "getBalance", 0, 200,
				`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1000},"value":777}}`)

			net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
				svmUpstreamConfig("rpc1", h1),
				svmUpstreamConfig("rpc2", h2),
			})
			net.PinUpstreamOrderForTest("rpc1", "rpc2")

			resp, err := svmProjectForward(ctx, net, common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["pubkey"]}`)))
			require.NoErrorf(t, err,
				"HTTP %d with a non-JSON body did not fail over to the healthy upstream", tc.status)
			jrr, jerr := resp.JsonRpcResponse()
			require.NoError(t, jerr)
			require.Contains(t, string(jrr.GetResultBytes()), "777")
			require.EqualValues(t, 1, primary.Load(), "the degraded upstream must be tried exactly once")
			require.EqualValues(t, 1, secondary.Load(), "the healthy upstream must serve the request")
		})
	}
}
