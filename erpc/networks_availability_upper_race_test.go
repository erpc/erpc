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
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// Reproduction for https://github.com/erpc/erpc/issues/934
//
// Block availability gating used to apply an implicit upper-bound check
// (blockNumber > latestBlock) for ANY upstream that had block availability
// bounds configured — so configuring only `blockAvailability.lower` (for
// pruned-node protection) silently also enabled an upper/head check.
//
// On fast block-time chains a client that learned about block N via a direct
// `eth_subscribe newHeads` subscription requests block N through eRPC before
// the per-upstream state poller (HTTP `eth_getBlockByNumber("latest")`) has
// caught up. The poller still reports latestBlock = N-1, so the implicit
// upper-bound check rejected the request with ErrUpstreamBlockUnavailable even
// though the upstream actually has block N — and even though the user only
// configured a lower bound, never an upper bound.
//
// The fix makes block availability gating enforce ONLY the upstream's
// explicitly-configured bounds (lower/upper). Tip-awareness ("is this block
// past the chain head?") is owned by served-tip and integrity.enforceHighestBlock,
// not by this path. Operators who want head gating can opt in explicitly with
// `blockAvailability.upper.latestBlockMinus: 0`.
//
// These tests use real Network / Upstream / EvmStatePoller objects; only the
// upstream HTTP endpoint is mocked (via gock). The state poller settles on the
// stale tip through real HTTP polling — SuggestLatestBlock is deliberately NOT
// used so the race is reproduced faithfully.
// ----------------------------------------------------------------------------

// lowerOnlyRaceFixture configures one upstream for the reproduction.
type lowerOnlyRaceFixture struct {
	id       string // also used as the host: http://<id>.localhost
	staleTip int64  // what the poller's eth_getBlockByNumber("latest") returns
}

// setupLowerOnlyRaceNetwork builds a Network whose upstreams:
//   - are configured with ONLY blockAvailability.lower (latestBlockMinus), like
//     the issue's config — no upper bound;
//   - have their state poller settle on `staleTip` via real HTTP polling;
//   - will happily serve eth_getBlockByNumber(freshBlock) if the request ever
//     reaches them (freshBlock = staleTip+1, the block the client already saw
//     via newHeads).
//
// freshBlockHex is the lowercase hex of the block the client requests.
func setupLowerOnlyRaceNetwork(t *testing.T, ctx context.Context, fixtures []lowerOnlyRaceFixture, freshBlockHex string) (*Network, []*upstream.Upstream) {
	t.Helper()

	const latestMinus = int64(237600) // mirrors the issue config

	upCfgs := make([]*common.UpstreamConfig, 0, len(fixtures))
	for _, f := range fixtures {
		endpoint := "http://" + f.id + ".localhost"
		staleTipHex := fmt.Sprintf("0x%x", f.staleTip)
		finalizedHex := fmt.Sprintf("0x%x", f.staleTip-16)

		// eth_chainId
		gock.New(endpoint).Post("").Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_chainId")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x7b"}`))

		// eth_syncing
		gock.New(endpoint).Post("").Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_syncing")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":false}`))

		// eth_getBlockByNumber("latest") -> STALE tip (the poller is behind the
		// real chain head, exactly like a node that just produced N but whose
		// HTTP "latest" hasn't reflected it within the debounce window yet).
		gock.New(endpoint).Post("").Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "latest")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"` + staleTipHex + `","hash":"0xaaa","timestamp":"0x6702a8f0"}}`))

		// eth_getBlockByNumber("finalized")
		gock.New(endpoint).Post("").Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "finalized")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"` + finalizedHex + `","hash":"0xbbb","timestamp":"0x6702a8e0"}}`))

		// eth_getBlockByNumber(freshBlock) -> the upstream DOES have it. If the
		// implicit upper-bound check wrongly rejects the request, this mock is
		// never consumed.
		gock.New(endpoint).Post("").Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, freshBlockHex)
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"` + freshBlockHex + `","hash":"0xfresh","timestamp":"0x6702a900"}}`))

		upCfgs = append(upCfgs, &common.UpstreamConfig{
			Id:       f.id,
			Type:     common.UpstreamTypeEvm,
			Endpoint: endpoint,
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(200 * time.Millisecond),
				StatePollerDebounce: common.Duration(250 * time.Millisecond), // mirrors the issue config
				BlockAvailability: &common.EvmBlockAvailabilityConfig{
					// ONLY a lower bound — no upper bound, like the issue config.
					Lower: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(latestMinus)},
				},
			},
		})
	}

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)

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
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", upCfgs, ssr, rlr, vr, pr, nil, mt, nil)
	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	// Network has NO enforceBlockAvailability override and NO served-tip config:
	// enforcement is opted into solely by the per-upstream lower bound, exactly
	// like the issue's config.
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, err := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt, nil)
	require.NoError(t, err)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))
	network.PinUpstreamOrderForTest()

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.Len(t, ups, len(fixtures))

	// Wait for each poller to settle on the stale tip via REAL HTTP polling.
	for _, f := range fixtures {
		var u *upstream.Upstream
		for _, candidate := range ups {
			if candidate.Id() == f.id {
				u = candidate
				break
			}
		}
		require.NotNil(t, u, "fixture %q not found", f.id)
		require.Eventually(t, func() bool {
			return u.EvmStatePoller().LatestBlock() == f.staleTip
		}, 3*time.Second, 25*time.Millisecond,
			"poller for %q should settle on stale tip %d (got %d)", f.id, f.staleTip, u.EvmStatePoller().LatestBlock())
	}

	return network, ups
}

// TestNetworkAvailability_Issue934_LowerOnly_FreshBlockAboveStaleTip_Forward is
// the end-to-end reproduction: a request for the block the client just saw via
// newHeads (one block above the poller's stale tip) must be served, because the
// upstream has it and the user configured ONLY a lower bound.
//
// On current (buggy) code the implicit upper-bound check rejects the request on
// every upstream and Forward returns ErrUpstreamsExhausted (wrapping
// ErrUpstreamBlockUnavailable) without ever querying the upstream for the block.
func TestNetworkAvailability_Issue934_LowerOnly_FreshBlockAboveStaleTip_Forward(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const staleTip = int64(0x1e8480) // 2,000,000
	const freshBlockHex = "0x1e8481" // 2,000,001 == staleTip + 1
	network, _ := setupLowerOnlyRaceNetwork(t, ctx, []lowerOnlyRaceFixture{
		{id: "rpc1", staleTip: staleTip},
		{id: "rpc2", staleTip: staleTip},
	}, freshBlockHex)

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["` + freshBlockHex + `",false]}`))
	req.SetNetwork(network)

	resp, err := network.Forward(ctx, req)

	require.NoError(t, err,
		"issue #934: a lower-only blockAvailability config must not enable the implicit "+
			"upper-bound check; block %s is within the lower bound and the co-located upstream "+
			"already has it (client saw it via newHeads), so it must be served instead of rejected", freshBlockHex)
	require.NotNil(t, resp)
	jrr, jerr := resp.JsonRpcResponse(ctx)
	require.NoError(t, jerr)
	require.Contains(t, jrr.GetResultString(), "0xfresh",
		"expected the upstream to actually serve the fresh block")
	resp.Release()
}

// TestNetworkAvailability_Issue934_LowerOnly_DoesNotTriggerImplicitUpperBound_Direct
// pins the exact gating decision in isolation (no retries/hedging), mirroring the
// style of the existing direct checkUpstreamBlockAvailability tests.
func TestNetworkAvailability_Issue934_LowerOnly_DoesNotTriggerImplicitUpperBound_Direct(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const staleTip = int64(0x1e8480)
	const freshBlock = int64(0x1e8481)
	const freshBlockHex = "0x1e8481"
	network, ups := setupLowerOnlyRaceNetwork(t, ctx, []lowerOnlyRaceFixture{
		{id: "rpc1", staleTip: staleTip},
	}, freshBlockHex)

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["` + freshBlockHex + `",false]}`))
	req.SetNetwork(network)
	req.SetEvmBlockNumber(freshBlock)

	skipErr, _ := network.checkUpstreamBlockAvailability(ctx, ups[0], req, "eth_getBlockByNumber")
	require.NoError(t, skipErr,
		"issue #934: lower-only config must not reject block %d (one above the stale poller tip %d) "+
			"via the implicit upper-bound check", freshBlock, staleTip)
}

// TestNetworkAvailability_Issue934_LowerBound_StillEnforced is the guardrail:
// the fix must NOT throw away pruned-node protection. A request for a block
// below the configured lower bound must still be skipped (non-retryable).
func TestNetworkAvailability_Issue934_LowerBound_StillEnforced(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const staleTip = int64(0x1e8480) // 2,000,000
	const freshBlockHex = "0x1e8481"
	network, ups := setupLowerOnlyRaceNetwork(t, ctx, []lowerOnlyRaceFixture{
		{id: "rpc1", staleTip: staleTip},
	}, freshBlockHex)

	// lower bound = staleTip - 237600 = 1,762,400. Request a much older block.
	const oldBlock = int64(1000)
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x3e8",false]}`))
	req.SetNetwork(network)
	req.SetEvmBlockNumber(oldBlock)

	skipErr, isRetryable := network.checkUpstreamBlockAvailability(ctx, ups[0], req, "eth_getBlockByNumber")
	require.Error(t, skipErr, "block below the configured lower bound must still be skipped (pruned-node protection)")
	require.True(t, common.HasErrorCode(skipErr, common.ErrCodeUpstreamBlockUnavailable),
		"expected ErrUpstreamBlockUnavailable, got: %v", skipErr)
	require.False(t, isRetryable, "below lower bound is not retryable")
}

// ----------------------------------------------------------------------------
// Direct coverage of the bounds-only gating semantics introduced for issue #934.
//
// These call checkUpstreamBlockAvailability directly (no Forward/retry/hedging) and
// rely on SetupMocksForEvmStatePoller, which makes rpc1 settle on latest tip
// 0x11118888. Block numbers below are expressed relative to that tip.
// ----------------------------------------------------------------------------

const issue934MockTip = int64(0x11118888) // latest tip from SetupMocksForEvmStatePoller (rpc1)

// setupDirectAvailabilityNetwork builds a single "rpc1" EVM upstream with the given
// per-upstream EVM config and network config, then waits for its state poller to
// settle on the SetupMocksForEvmStatePoller tip. The caller must have already called
// util.SetupMocksForEvmStatePoller(). chainId and sane poller intervals are filled in
// when unset.
func setupDirectAvailabilityNetwork(t *testing.T, ctx context.Context, upEvm *common.EvmUpstreamConfig, ntwCfg *common.NetworkConfig) (*Network, *upstream.Upstream) {
	t.Helper()

	upEvm.ChainId = 123
	if upEvm.StatePollerInterval == 0 {
		upEvm.StatePollerInterval = common.Duration(200 * time.Millisecond)
	}
	if upEvm.StatePollerDebounce == 0 {
		upEvm.StatePollerDebounce = common.Duration(50 * time.Millisecond)
	}
	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm:      upEvm,
	}

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
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
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upCfg}, ssr, rlr, vr, pr, nil, mt, nil)
	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	if ntwCfg == nil {
		ntwCfg = &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	}
	network, err := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt, nil)
	require.NoError(t, err)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))
	network.PinUpstreamOrderForTest()

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))[0]
	require.Eventually(t, func() bool {
		return ups.EvmStatePoller().LatestBlock() == issue934MockTip
	}, 3*time.Second, 25*time.Millisecond, "poller should settle on the mock tip")
	return network, ups
}

// balanceReqAt builds an eth_getBalance request pinned to a concrete block number.
func balanceReqAt(network *Network, blockHex string, bn int64) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000","` + blockHex + `"]}`))
	req.SetNetwork(network)
	req.SetEvmBlockNumber(bn)
	return req
}

// (1) The core behavior change: an upstream with NO configured bounds must NOT be
// gated by an implicit head check, even when enforceBlockAvailability is true at the
// network level. The block being ahead of the poller tip is irrelevant — head/tip
// awareness is owned by served-tip + enforceHighestBlock, not this path.
func TestNetworkAvailability_Issue934_NoBounds_EnforceTrue_AboveTip_Allowed(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupDirectAvailabilityNetwork(t, ctx,
		&common.EvmUpstreamConfig{}, // no blockAvailability bounds at all
		&common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123, EnforceBlockAvailability: b(true)}},
	)

	req := balanceReqAt(network, "0x111188ba", issue934MockTip+50) // 50 blocks ahead of tip
	skipErr, _ := network.checkUpstreamBlockAvailability(ctx, ups, req, "eth_getBalance")
	require.NoError(t, skipErr,
		"an upstream with no configured bounds must not be head-gated, even with enforceBlockAvailability:true (issue #934)")
}

// (2) An explicit upper cap (latestBlockMinus:5) is a real structural bound: a block
// above the cap but still at/below the live head must be rejected (non-retryable,
// since waiting will not help), while a block at/below the cap is served.
func TestNetworkAvailability_Issue934_ExplicitUpperCap_BelowHead_Enforced(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupDirectAvailabilityNetwork(t, ctx,
		&common.EvmUpstreamConfig{
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Upper: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(5)}, // cap = tip-5
			},
		}, nil,
	)

	// tip-2 (0x11118886) is above the cap (tip-5) but at/below the head → reject, not retryable.
	above := balanceReqAt(network, "0x11118886", issue934MockTip-2)
	skipErr, retryable := network.checkUpstreamBlockAvailability(ctx, ups, above, "eth_getBalance")
	require.Error(t, skipErr, "block above the configured upper cap must be skipped")
	require.True(t, common.HasErrorCode(skipErr, common.ErrCodeUpstreamBlockUnavailable))
	require.False(t, retryable, "above the cap but not ahead of the live head → not retryable")

	// tip-8 (0x11118880) is at/below the cap → served.
	below := balanceReqAt(network, "0x11118880", issue934MockTip-8)
	skipErr2, _ := network.checkUpstreamBlockAvailability(ctx, ups, below, "eth_getBalance")
	require.NoError(t, skipErr2, "block within the configured upper cap must be served")
}

// (3) The migration path / opt-in head gating: blockAvailability.upper.latestBlockMinus:0
// reproduces the old "serve only blocks the node has actually synced" behavior. At the
// head is allowed; one block ahead is a retryable skip; far ahead is non-retryable.
func TestNetworkAvailability_Issue934_UpperLatestMinusZero_OptInHeadGating(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupDirectAvailabilityNetwork(t, ctx,
		&common.EvmUpstreamConfig{
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Upper: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(0)}, // cap = tip
			},
		}, nil,
	)

	// at the head → allowed
	atHead := balanceReqAt(network, "0x11118888", issue934MockTip)
	skipErr, _ := network.checkUpstreamBlockAvailability(ctx, ups, atHead, "eth_getBalance")
	require.NoError(t, skipErr, "a block at the head is within upper=latestBlockMinus:0 and must be served")

	// one block ahead → retryable skip
	ahead := balanceReqAt(network, "0x11118889", issue934MockTip+1)
	skipErr2, retryable2 := network.checkUpstreamBlockAvailability(ctx, ups, ahead, "eth_getBalance")
	require.Error(t, skipErr2)
	require.True(t, common.HasErrorCode(skipErr2, common.ErrCodeUpstreamBlockUnavailable))
	require.True(t, retryable2, "one block ahead of the head is retryable (node may catch up)")

	// far ahead (> MaxRetryableBlockDistance) → non-retryable
	farAhead := balanceReqAt(network, "0x11119999", issue934MockTip+0x1111)
	skipErr3, retryable3 := network.checkUpstreamBlockAvailability(ctx, ups, farAhead, "eth_getBalance")
	require.Error(t, skipErr3)
	require.False(t, retryable3, "far ahead of the head (> MaxRetryableBlockDistance) is not retryable")
}

// (4) An explicit method-level enforceBlockAvailability:false is an operator opt-out
// that disables bound enforcement even on an upstream that has bounds configured.
func TestNetworkAvailability_Issue934_MethodLevelEnforceFalse_DisablesBounds(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// eth_getBalance has no system default for EnforceBlockAvailability, so a
	// method-level false is a genuine user override (disables enforcement).
	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Methods: &common.MethodsConfig{Definitions: map[string]*common.CacheMethodConfig{
			"eth_getBalance": {EnforceBlockAvailability: b(false)},
		}},
	}
	network, ups := setupDirectAvailabilityNetwork(t, ctx,
		&common.EvmUpstreamConfig{
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: i64(100)},
			},
		}, ntwCfg,
	)

	// Block 50 is below the configured lower bound, but the method-level opt-out
	// disables enforcement → allowed.
	req := balanceReqAt(network, "0x32", 50)
	skipErr, _ := network.checkUpstreamBlockAvailability(ctx, ups, req, "eth_getBalance")
	require.NoError(t, skipErr,
		"method-level enforceBlockAvailability:false must disable bound enforcement even with bounds configured")
}

// (5) A configured upper bound that sits ABOVE the head must not reactivate the head
// race: a block just ahead of the stale poller tip but inside the [lower, upper]
// window is served. This is the issue #934 scenario with a window rather than
// lower-only.
func TestNetworkAvailability_Issue934_Window_AboveTipWithinWindow_Allowed(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupDirectAvailabilityNetwork(t, ctx,
		&common.EvmUpstreamConfig{
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(237600)},
				Upper: &common.EvmAvailabilityBoundConfig{ExactBlock: i64(issue934MockTip + 1000)}, // upper well above the head
			},
		}, nil,
	)

	// tip+1 is above the stale poller tip but well within [tip-237600, tip+1000].
	req := balanceReqAt(network, "0x11118889", issue934MockTip+1)
	skipErr, _ := network.checkUpstreamBlockAvailability(ctx, ups, req, "eth_getBalance")
	require.NoError(t, skipErr,
		"a configured upper bound above the head must not reintroduce the implicit head race (issue #934)")
}

// (6) Range methods (eth_getLogs, trace_filter, arbtrace_filter) are gated by their
// dedicated range-aware pre-forward hooks, not by checkUpstreamBlockAvailability's
// single-block path. Even with bounds configured and a clearly out-of-range block,
// this path must defer to the hook (return no skip).
func TestNetworkAvailability_Issue934_RangeMethod_DeferredToHook(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupDirectAvailabilityNetwork(t, ctx,
		&common.EvmUpstreamConfig{
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: i64(1_000_000)},
			},
		}, nil,
	)

	// fromBlock 0x1 is far below the lower bound; a single-block gate would skip it,
	// but eth_getLogs must be deferred to the block-range hook here.
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x2","address":"0x0000000000000000000000000000000000000000"}]}`))
	req.SetNetwork(network)

	skipErr, _ := network.checkUpstreamBlockAvailability(ctx, ups, req, "eth_getLogs")
	require.NoError(t, skipErr,
		"eth_getLogs is gated by its dedicated range hook, not by checkUpstreamBlockAvailability")
}

// mockIssue934Node mocks the EVM state-poller endpoints for one node plus the
// concrete fresh block. The poller's "latest" reflects a STALE tip, while the node
// would happily serve the fresh block if the request reaches it.
func mockIssue934Node(endpoint, chainIDHex, staleTipHex, finalizedHex, freshBlockHex string) {
	gock.New(endpoint).Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_chainId") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + chainIDHex + `"}`))
	gock.New(endpoint).Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_syncing") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":false}`))
	gock.New(endpoint).Post("").Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "latest")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"` + staleTipHex + `","hash":"0xaaa","timestamp":"0x6702a8f0"}}`))
	gock.New(endpoint).Post("").Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "finalized")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"` + finalizedHex + `","hash":"0xbbb","timestamp":"0x6702a8e0"}}`))
	gock.New(endpoint).Post("").Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, freshBlockHex)
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"` + freshBlockHex + `","hash":"0xfresh","timestamp":"0x6702a900"}}`))
}

// TestNetworkAvailability_Issue934_ExactConfigFromYAML_ServesFreshBlock is the
// definitive end-to-end check: it loads the issue's LITERAL erpc.yaml through the
// real config pipeline (LoadConfig → SetDefaults, which merges upstreamDefaults),
// asserts the resolved upstream config is lower-only (no synthesized upper), then
// builds a real Network from the resolved configs and verifies that a block one
// ahead of the stale poller tip — the exact scenario in the report — is SERVED
// rather than rejected with ErrUpstreamBlockUnavailable.
func TestNetworkAvailability_Issue934_ExactConfigFromYAML_ServesFreshBlock(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The exact config from https://github.com/erpc/erpc/issues/934
	const yamlCfg = `
logLevel: debug
projects:
  - id: my-chain
    upstreamDefaults:
      evm:
        chainId: 5042002
        statePollerDebounce: 250ms
    upstreams:
      - endpoint: http://node-1:8545
        id: node-1
        evm:
          blockAvailability:
            lower:
              latestBlockMinus: 237600
      - endpoint: http://node-2:8545
        id: node-2
        evm:
          blockAvailability:
            lower:
              latestBlockMinus: 237600
`
	fs := afero.NewMemMapFs()
	require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte(yamlCfg), 0644))
	cfg, err := common.LoadConfig(fs, "erpc.yaml", &common.DefaultOptions{})
	require.NoError(t, err, "issue #934 config must load and validate")

	// --- Config-side: the literal YAML must resolve to a lower-only bound ---
	require.Len(t, cfg.Projects, 1)
	upCfgs := cfg.Projects[0].Upstreams
	require.Len(t, upCfgs, 2)
	for _, u := range upCfgs {
		require.NotNil(t, u.Evm.BlockAvailability, "blockAvailability must be parsed for %s", u.Id)
		require.NotNil(t, u.Evm.BlockAvailability.Lower, "lower bound must be present for %s", u.Id)
		require.NotNil(t, u.Evm.BlockAvailability.Lower.LatestBlockMinus)
		require.Equal(t, int64(237600), *u.Evm.BlockAvailability.Lower.LatestBlockMinus)
		require.Nil(t, u.Evm.BlockAvailability.Upper,
			"a lower-only config must NOT synthesize an upper bound (issue #934)")
	}

	// --- Behavioral: build a real network from the resolved configs and serve ---
	const chainID = int64(5042002)
	const chainIDHex = "0x4cef52" // 5042002
	const staleTip = int64(0x4c4b40) // 5,000,000
	const finalizedHex = "0x4c4b30"  // staleTip - 16
	const freshBlockHex = "0x4c4b41" // staleTip + 1 (the block the client saw via newHeads)
	staleTipHex := fmt.Sprintf("0x%x", staleTip)
	for _, u := range upCfgs {
		mockIssue934Node(u.Endpoint, chainIDHex, staleTipHex, finalizedHex, freshBlockHex)
	}

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "my-chain", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
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
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "my-chain", upCfgs, ssr, rlr, vr, pr, nil, mt, nil)
	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: chainID}}
	network, err := NewNetwork(ctx, &log.Logger, "my-chain", ntwCfg, rlr, upr, mt, nil)
	require.NoError(t, err)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(chainID)))
	require.NoError(t, network.Bootstrap(ctx))
	network.PinUpstreamOrderForTest()

	// Wait for both pollers to settle on the stale tip via real HTTP polling.
	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(chainID))
	require.Len(t, ups, 2)
	for _, u := range ups {
		uu := u
		require.Eventually(t, func() bool { return uu.EvmStatePoller().LatestBlock() == staleTip }, 3*time.Second, 25*time.Millisecond,
			"poller for %s should settle on stale tip", u.Id())
	}

	// The client already saw freshBlock via newHeads; request it through eRPC while the
	// poller is still a block behind. It must be served, not rejected.
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["` + freshBlockHex + `",false]}`))
	req.SetNetwork(network)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err,
		"issue #934: with the literal config, a block one ahead of the stale poller tip must be served (the upstream has it)")
	require.NotNil(t, resp)
	jrr, jerr := resp.JsonRpcResponse(ctx)
	require.NoError(t, jerr)
	require.Contains(t, jrr.GetResultString(), "0xfresh", "the fresh block must be served from the upstream")
	resp.Release()
}

// TestNetworkAvailability_Issue934_ArcSyncFlow_CustomMethodAndFreshBlock mirrors the
// real Arc rpc-sync use case (from the issue discussion): on each new head the client
// immediately calls eth_getBlockByNumber(N) AND a custom consensus method,
// arc_getCertificate, for the just-produced block — through eRPC, against upstreams
// configured lower-only while the state poller is still a block behind.
//
// eth_getBlockByNumber(N) must be served (issue #934). arc_getCertificate is a custom
// method: eRPC cannot extract a block reference for it (it is in none of the cache-method
// maps), so block-availability gating fails open and the request is forwarded — i.e. it
// is never constrained by the state poller. This test grounds both in reality.
//
// Note: eRPC does not provide the newHeads websocket subscription itself (the ws upstream
// client is unimplemented and the server is request/response only), so — as in the issue
// — the client subscribes to newHeads directly on a node and uses eRPC for the follow-up
// JSON-RPC calls.
func TestNetworkAvailability_Issue934_ArcSyncFlow_CustomMethodAndFreshBlock(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const staleTip = int64(0x1e8480) // 2,000,000
	const freshBlockHex = "0x1e8481" // staleTip + 1 (the head the client just saw)
	network, _ := setupLowerOnlyRaceNetwork(t, ctx, []lowerOnlyRaceFixture{
		{id: "rpc1", staleTip: staleTip},
		{id: "rpc2", staleTip: staleTip},
	}, freshBlockHex)

	// The Arc consensus custom method, served by the node for the fresh block.
	for _, id := range []string{"rpc1", "rpc2"} {
		gock.New("http://" + id + ".localhost").Post("").Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "arc_getCertificate")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"certificate":"0xcert"}}`))
	}

	// 1) eth_getBlockByNumber(N) for the fresh head → served (issue #934 fix).
	reqBlock := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["` + freshBlockHex + `",false]}`))
	reqBlock.SetNetwork(network)
	respBlock, err := network.Forward(ctx, reqBlock)
	require.NoError(t, err, "fresh head block must be served, not rejected by the stale poller")
	jrrBlock, jerr := respBlock.JsonRpcResponse(ctx)
	require.NoError(t, jerr)
	require.Contains(t, jrrBlock.GetResultString(), "0xfresh")
	respBlock.Release()

	// 2) arc_getCertificate(N) — custom method for the same fresh block. eRPC cannot
	// extract a block reference, so gating fails open and it is forwarded regardless of
	// the lower-only bounds or the lagging state poller.
	reqCert := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":2,"method":"arc_getCertificate","params":["` + freshBlockHex + `"]}`))
	reqCert.SetNetwork(network)
	respCert, err := network.Forward(ctx, reqCert)
	require.NoError(t, err,
		"a custom method (arc_getCertificate) must be forwarded, never constrained by the state poller")
	jrrCert, jerr := respCert.JsonRpcResponse(ctx)
	require.NoError(t, jerr)
	require.Contains(t, jrrCert.GetResultString(), "0xcert", "the custom method response must be served from the upstream")
	respCert.Release()
}
