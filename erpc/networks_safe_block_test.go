package erpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── fixtures ────────────────────────────────────────────────────────────────

const (
	// safeSourceTag is the tag an operator designates as authoritative for the
	// `safe` tag; the network selects on it via evm.safeBlock.source.
	safeSourceTag = "tier:operator"
	// publicProviderTag marks a provider that is explicitly NOT authoritative.
	publicProviderTag = "tier:public"

	// trustedSafeHead is what an authoritative source reports as its `safe`
	// head; trustedSafeHex is its canonical hex form, spelled out literally so
	// a broken encoder cannot agree with itself.
	trustedSafeHead = int64(4096)
	trustedSafeHex  = "0x1000"

	// stalledLatestHead is where the sequencer kept going while batch
	// publication (and with it the safe head) sat frozen at trustedSafeHead.
	// The gap is ~286M blocks: no fixed `latest - N` estimate survives it.
	stalledLatestHead = int64(286397576)
)

// safeHeadPoller reports a `safe` head. common.FakeEvmStatePoller deliberately
// lacks SafeBlock() — the resolver reaches for it through a narrow capability
// interface — so tests that need an authoritative source add the method here
// instead of widening the shared fake for a feature most pollers don't have.
type safeHeadPoller struct {
	common.EvmStatePoller
	safe int64
}

func (p *safeHeadPoller) SafeBlock() int64 { return p.safe }

func newSafeHeadPoller(latest, safe int64) common.EvmStatePoller {
	return &safeHeadPoller{
		EvmStatePoller: common.NewFakeEvmStatePoller(latest, latest),
		safe:           safe,
	}
}

// syncingUpstream reports EvmSyncingStateSyncing. The shared FakeUpstream is
// hardwired to "unknown", so the one method is overridden over the embedded
// interface value rather than reimplementing ~25 forwarding methods.
type syncingUpstream struct{ common.EvmUpstream }

func (u *syncingUpstream) EvmSyncingState() common.EvmSyncingState {
	return common.EvmSyncingStateSyncing
}

// fakeUpstream builds an upstream with the given id and tags whose poller
// reports `safe` as its safe head. Ids must be unique: the policy engine
// dedupes candidates by id.
func fakeUpstream(id string, latest, safe int64, tags ...string) common.Upstream {
	return common.NewFakeUpstream(
		id,
		common.WithTags(tags...),
		common.WithEvmStatePoller(newSafeHeadPoller(latest, safe)),
	)
}

// safeBlockNetwork builds a Network whose tip-candidate set is exactly `ups`,
// fed by a real policy engine running the identity policy. That keeps the safe
// resolver observable with no HTTP, no project bootstrap and no timing.
//
// `source` == "" leaves evm.safeBlock unset (feature off).
func safeBlockNetwork(t *testing.T, ctx context.Context, source string, ups ...common.Upstream) *Network {
	t.Helper()

	polCfg := &common.SelectionPolicyConfig{
		EvalInterval: common.Duration(time.Second),
		EvalTimeout:  common.Duration(100 * time.Millisecond),
		// Distinct from common.DefaultSelectionPolicySource so the engine does
		// NOT upgrade to the rich default policy (whose health filters would
		// make the candidate set depend on tracker state).
		EvalFunc: "(ups, _ctx) => ups",
	}
	require.NoError(t, polCfg.SetDefaults())
	require.NoError(t, polCfg.Validate())

	tracker := health.NewTracker(&log.Logger, "prjSafe", time.Minute)
	engine := policy.NewEngine(ctx, &log.Logger, "prjSafe", tracker, nil, nil)
	t.Cleanup(engine.Stop)

	networkId := util.EvmNetworkId(123)
	require.NoError(t, engine.RegisterNetwork(networkId, "", func() []common.Upstream { return ups }, polCfg))
	require.Len(t, engine.GetOrdered(networkId, "*", "*"), len(ups),
		"every injected upstream must be a tip candidate, otherwise the assertions below prove nothing")

	evmCfg := &common.EvmNetworkConfig{ChainId: 123}
	if source != "" {
		evmCfg.SafeBlock = &common.EvmSafeBlockConfig{Source: source}
	}

	return &Network{
		networkId:      networkId,
		projectId:      "prjSafe",
		logger:         &log.Logger,
		cfg:            &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: evmCfg},
		policyEngine:   engine,
		metricsTracker: tracker,
	}
}

// normalizeSafeRequest runs the exact pair Network.prepareRequest runs back to
// back: tag resolution, then the fail-closed gate. Returns the resolved params
// and the gate's verdict.
func normalizeSafeRequest(t *testing.T, ctx context.Context, ntw *Network, body string) (*common.NormalizedRequest, []interface{}, error) {
	t.Helper()
	return normalizeSafeRequestWithDirectives(t, ctx, ntw, body, nil)
}

// normalizeSafeRequestWithDirectives is normalizeSafeRequest with client-supplied
// directives applied before normalization.
func normalizeSafeRequestWithDirectives(
	t *testing.T,
	ctx context.Context,
	ntw *Network,
	body string,
	dirs *common.RequestDirectives,
) (*common.NormalizedRequest, []interface{}, error) {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(body))
	req.SetNetwork(ntw)
	if dirs != nil {
		req.SetDirectives(dirs)
	}
	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)
	return req, jrq.Params, evm.EnforceSafeBlockResolved(ctx, ntw, req)
}

// requireSafeBlockUnavailable asserts the error is ErrEvmSafeBlockUnavailable
// carrying `wantReason`. The reason is operator-facing and the two values demand
// different responses (chase a source that stopped reporting vs. tell the caller
// to drop a directive), so a swapped reason is a real defect.
func requireSafeBlockUnavailable(t *testing.T, err error, wantReason string) {
	t.Helper()
	require.Error(t, err)
	var sbErr *common.ErrEvmSafeBlockUnavailable
	require.ErrorAs(t, err, &sbErr, "expected ErrEvmSafeBlockUnavailable, got %v", err)
	assert.Equal(t, common.ErrCodeEvmSafeBlockUnavailable, sbErr.Code)
	assert.Equal(t, wantReason, sbErr.Details["reason"])
}

// ─── the resolver: which upstreams get to define `safe` ──────────────────────

// TestNetwork_EvmHighestSafeBlockNumber pins who is allowed to answer the
// `safe` question for a network. Each row is a topology the operator can
// actually end up with.
func TestNetwork_EvmHighestSafeBlockNumber(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// An upstream whose poller predates the feature: it satisfies
	// common.EvmStatePoller but not the SafeBlock() capability.
	capabilityLessSource := common.NewFakeUpstream(
		"ups-legacy",
		common.WithTags(safeSourceTag),
		common.WithEvmStatePoller(common.NewFakeEvmStatePoller(stalledLatestHead, stalledLatestHead)),
	)

	for _, tc := range []struct {
		name   string
		source string
		ups    []common.Upstream
		want   int64
	}{
		{
			// Opt-in gate: without evm.safeBlock the resolver must stay silent
			// so `safe` keeps being forwarded verbatim (pre-existing behavior),
			// even when an upstream happens to know its safe head.
			name:   "feature off yields nothing even with a reporting upstream",
			source: "",
			ups:    []common.Upstream{fakeUpstream("op-node-a", stalledLatestHead, trustedSafeHead, safeSourceTag)},
			want:   0,
		},
		{
			name:   "single authoritative source defines the head",
			source: safeSourceTag,
			ups:    []common.Upstream{fakeUpstream("op-node-a", stalledLatestHead, trustedSafeHead, safeSourceTag)},
			want:   trustedSafeHead,
		},
		{
			// The trust boundary. A permissive provider running zero L1
			// confirmations reports a much higher `safe`; it must not move the
			// network's answer, or the selector stops being a boundary at all.
			name:   "untrusted upstream reporting a higher safe never raises it",
			source: safeSourceTag,
			ups: []common.Upstream{
				fakeUpstream("op-node-a", stalledLatestHead, trustedSafeHead, safeSourceTag),
				fakeUpstream("provider-x", stalledLatestHead, stalledLatestHead, publicProviderTag),
			},
			want: trustedSafeHead,
		},
		{
			// Sources enforcing the same policy converge; taking the MAX keeps
			// one lagging peer from dragging the network backwards.
			name:   "two sources at different heights resolve to the max",
			source: safeSourceTag,
			ups: []common.Upstream{
				fakeUpstream("op-node-a", stalledLatestHead, trustedSafeHead-1024, safeSourceTag),
				fakeUpstream("op-node-b", stalledLatestHead, trustedSafeHead, safeSourceTag),
			},
			want: trustedSafeHead,
		},
		{
			// A syncing node's derived state is mid-flight; its (higher) safe
			// head must be excluded even though it matches the selector.
			name:   "syncing source is excluded even when it reports higher",
			source: safeSourceTag,
			ups: []common.Upstream{
				&syncingUpstream{fakeUpstream("op-node-syncing", stalledLatestHead, stalledLatestHead, safeSourceTag).(common.EvmUpstream)},
				fakeUpstream("op-node-b", stalledLatestHead, trustedSafeHead, safeSourceTag),
			},
			want: trustedSafeHead,
		},
		{
			// Fail-closed default of the capability interface: a poller that
			// cannot report a safe head contributes nothing rather than being
			// counted as authoritative-at-zero or crashing the resolver.
			name:   "source whose poller lacks the capability contributes nothing",
			source: safeSourceTag,
			ups:    []common.Upstream{capabilityLessSource},
			want:   0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ntw := safeBlockNetwork(t, ctx, tc.source, tc.ups...)
			assert.Equal(t, tc.want, ntw.EvmHighestSafeBlockNumber(ctx))
		})
	}
}

// TestNetwork_SafeBlock_StalledHeadDoesNotDriftWithLatest is the safety
// property the whole feature exists for.
//
// The safe head advances only as batch data lands on L1, so its distance behind
// `latest` is unbounded during a batcher or derivation stall. Any `latest - N`
// estimate would keep marching forward past a frozen safe head and start
// reporting unsafe blocks as safe. With latest ~286M blocks ahead of a frozen
// safe head, resolution must stay exactly on the frozen head.
func TestNetwork_SafeBlock_StalledHeadDoesNotDriftWithLatest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := fakeUpstream("op-node-a", stalledLatestHead, trustedSafeHead, safeSourceTag)
	ntw := safeBlockNetwork(t, ctx, safeSourceTag, src)

	// Precondition: the stall is real. Without a huge latest-vs-safe gap a
	// drifting estimator could coincidentally agree and this test would be
	// vacuous.
	require.Equal(t, stalledLatestHead, src.(common.EvmUpstream).EvmStatePoller().LatestBlock())
	require.Equal(t, stalledLatestHead, ntw.EvmHighestLatestBlockNumber(ctx))
	require.Greater(t, stalledLatestHead-trustedSafeHead, int64(1_000_000))

	assert.Equal(t, trustedSafeHead, ntw.EvmHighestSafeBlockNumber(ctx),
		"a stalled safe head must not follow latest")

	// ...and the tag itself resolves to the frozen head, not to anything near latest.
	_, params, gateErr := normalizeSafeRequest(t, ctx, ntw,
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["safe",false]}`)
	require.NoError(t, gateErr)
	require.Len(t, params, 2)
	assert.Equal(t, trustedSafeHex, params[0])
}

// ─── tag resolution + the fail-closed gate ───────────────────────────────────

func TestNetwork_SafeBlock_TagResolution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reportingSource := func() common.Upstream {
		return fakeUpstream("op-node-a", stalledLatestHead, trustedSafeHead, safeSourceTag)
	}
	// A source that matches the selector but has not observed a safe head yet
	// (cold start, or every source unsupported/syncing).
	coldSource := func() common.Upstream {
		return fakeUpstream("op-node-a", stalledLatestHead, 0, safeSourceTag)
	}

	// eth_getBlockByNumber is the interesting method: its config disables
	// latest/finalized interpolation because it is the source of truth for
	// those tags. `safe` has no such escape hatch — it is precisely the
	// question eRPC must answer itself — so it must be rewritten here too.
	t.Run("eth_getBlockByNumber safe is rewritten despite latest/finalized interpolation being off", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, safeSourceTag, reportingSource())

		req, params, gateErr := normalizeSafeRequest(t, ctx, ntw,
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["safe",false]}`)
		require.NoError(t, gateErr)

		require.Len(t, params, 2)
		assert.Equal(t, trustedSafeHex, params[0])
		assert.Equal(t, int64(trustedSafeHead), req.EvmBlockNumber())
		// The ref stays tag-shaped so cache identity and finality
		// classification match other tag requests.
		assert.Equal(t, "safe", req.EvmBlockRef())
	})

	t.Run("ordinary methods resolve safe the same way", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, safeSourceTag, reportingSource())

		_, params, gateErr := normalizeSafeRequest(t, ctx, ntw,
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","safe"]}`)
		require.NoError(t, gateErr)
		require.Len(t, params, 2)
		assert.Equal(t, trustedSafeHex, params[1])
	})

	// Zero regression for every network that did not opt in.
	t.Run("unconfigured network forwards safe verbatim and does not fail closed", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, "", reportingSource())

		req, params, gateErr := normalizeSafeRequest(t, ctx, ntw,
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["safe",false]}`)
		require.NoError(t, gateErr, "a network without evm.safeBlock must never be rejected for a safe request")
		require.Len(t, params, 2)
		assert.Equal(t, "safe", params[0])
		assert.Equal(t, "safe", req.EvmBlockRef())
	})

	// Fail closed: forwarding the literal tag would let whichever provider
	// answers define it, which is the failure the feature removes.
	t.Run("configured but nothing observed is rejected rather than forwarded", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, safeSourceTag, coldSource())

		_, params, gateErr := normalizeSafeRequest(t, ctx, ntw,
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["safe",false]}`)

		requireSafeBlockUnavailable(t, gateErr, common.SafeBlockUnresolvedNoSource)
		// The tag was NOT rewritten — which is exactly why the gate must stop it.
		require.Len(t, params, 2)
		assert.Equal(t, "safe", params[0])
	})

	t.Run("no authoritative source in the pool is rejected too", func(t *testing.T) {
		// Selector matches nobody: an operator typo or a source that got
		// removed from the pool must not silently fall back to any provider.
		ntw := safeBlockNetwork(t, ctx, safeSourceTag,
			fakeUpstream("provider-x", stalledLatestHead, stalledLatestHead, publicProviderTag))

		_, _, gateErr := normalizeSafeRequest(t, ctx, ntw,
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","safe"]}`)

		requireSafeBlockUnavailable(t, gateErr, common.SafeBlockUnresolvedNoSource)
	})

	// The trust boundary is not client-negotiable. `skipInterpolation` is a
	// client-supplied directive while `evm.safeBlock.source` is an operator
	// guarantee; honoring the directive here would let any caller opt itself
	// back into "answer my safe request from whichever upstream replies",
	// re-opening the exact hole the feature closes. Same reasoning as
	// ErrConsensusCompositionDispute being non-bypassable by `disputeBehavior`.
	//
	// The source here is HEALTHY and reporting, so this cannot pass by accident
	// as an availability failure: the refusal is about the directive, which is
	// why the distinct reason is asserted.
	t.Run("SkipInterpolationCannotBypassTrustBoundary", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, safeSourceTag, reportingSource())
		require.Equal(t, trustedSafeHead, ntw.EvmHighestSafeBlockNumber(ctx),
			"precondition: a source IS reporting, so a rejection here can only be the directive")

		_, params, gateErr := normalizeSafeRequestWithDirectives(t, ctx, ntw,
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","safe"]}`,
			&common.RequestDirectives{SkipInterpolation: true})

		requireSafeBlockUnavailable(t, gateErr, common.SafeBlockUnresolvedSkipInterpolation)
		// The directive did keep the tag verbatim — so the gate is the only
		// thing standing between the caller and a non-authoritative answer.
		require.Len(t, params, 2)
		assert.Equal(t, "safe", params[1])
	})

	// The directive keeps working for every tag that is not a trust boundary.
	t.Run("skip interpolation still suppresses latest interpolation", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, safeSourceTag, reportingSource())

		_, params, gateErr := normalizeSafeRequestWithDirectives(t, ctx, ntw,
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`,
			&common.RequestDirectives{SkipInterpolation: true})

		require.NoError(t, gateErr, "only safe-tagged requests are gated")
		require.Len(t, params, 2)
		assert.Equal(t, "latest", params[1])
	})

	// Prior collapse behavior must survive the new tag: a mixed range keeps the
	// ref it had before, so the gate (which only fires on a "safe" ref) does
	// not start rejecting perfectly serviceable range requests.
	t.Run("mixed latest and safe range keeps the latest ref and is not rejected", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, safeSourceTag, reportingSource())

		req, params, gateErr := normalizeSafeRequest(t, ctx, ntw,
			`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"safe","toBlock":"latest"}]}`)
		require.NoError(t, gateErr)
		assert.Equal(t, "latest", req.EvmBlockRef())

		require.Len(t, params, 1)
		obj, ok := params[0].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, trustedSafeHex, obj["fromBlock"], "the safe bound still resolves inside a mixed range")
	})

	// `pending` includes mempool state nobody can vouch for; it must keep
	// flowing through untouched even on a safe-configured network.
	t.Run("pending is still never translated", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, safeSourceTag, reportingSource())

		req, params, gateErr := normalizeSafeRequest(t, ctx, ntw,
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","pending"]}`)
		require.NoError(t, gateErr)
		require.Len(t, params, 2)
		assert.Equal(t, "pending", params[1])
		assert.Nil(t, req.EvmBlockRef(), "pending must not be recorded as a resolvable ref")
	})
}
