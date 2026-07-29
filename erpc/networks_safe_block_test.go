package erpc

import (
	"context"
	"net/http"
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

	// pinnedLatestHead is a NON-authoritative provider's own latest head. It
	// sits below stalledLatestHead so selector scoping of the served tip stays
	// observable: a request pinned to that provider must be told its head, not
	// the network-wide one.
	pinnedLatestHead = stalledLatestHead - 1024
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

	// A mixed range whose `safe` bound RESOLVES is none of the gate's business:
	// after normalization no verbatim `safe` is left anywhere in the params, so
	// there is nothing to fail closed on. The ref collapsing to "latest" here is
	// incidental — the gate no longer consults the ref at all, and
	// TestNetwork_SafeBlock_SiblingBoundCannotBypassTheGate covers the
	// UNRESOLVABLE half of this same shape, which is now rejected.
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

// ─── a sibling bound is not proof that `safe` was resolved ───────────────────

// TestNetwork_SafeBlock_SiblingBoundCannotBypassTheGate is the regression for a
// bypass of the fail-closed gate that a range request could trigger by
// accident, or reach for on purpose.
//
// The gate used to decide "was the tag rewritten?" from two pieces of
// normalization METADATA rather than from the params themselves, and a range
// request carrying a second bound could forge both:
//
//   - EvmBlockNumber is cached for ANY numeric block param the walk sees, so on
//     eth_getLogs(fromBlock: 0x100, toBlock: "safe") the concrete `fromBlock`
//     alone set it to 256, and the gate read that as "the tag resolved";
//   - EvmBlockRef collapses to "latest" whenever `latest` and `safe` appear
//     together, and the gate only fired on a "safe" ref — so a latest/safe
//     range skipped it outright.
//
// Either way an unresolved, verbatim `safe` went to the wire, and whichever
// provider answered got to define it — including one running zero L1
// confirmations. That is the entire hole evm.safeBlock.source exists to close.
//
// The gate now asks the question literally: is a raw `safe` still sitting in one
// of this method's block params? A sibling bound cannot answer that for it.
func TestNetwork_SafeBlock_SiblingBoundCannotBypassTheGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reportingSource := func() common.Upstream {
		return fakeUpstream("op-node-a", stalledLatestHead, trustedSafeHead, safeSourceTag)
	}
	// Configured and selected, but no safe head observed yet.
	coldSource := func() common.Upstream {
		return fakeUpstream("op-node-a", stalledLatestHead, 0, safeSourceTag)
	}

	// A concrete lower bound plus a `safe` upper bound: the shape that forged
	// EvmBlockNumber. 0x100 is already canonical hex, so normalization cannot
	// rewrite it out from under the assertions, and it differs from
	// trustedSafeHex so the two bounds can never be confused.
	const (
		concreteFrom       = "0x100"
		concreteFromNumber = int64(256)
		numericSiblingBody = `{"jsonrpc":"2.0","id":1,"method":"eth_getLogs",` +
			`"params":[{"fromBlock":"0x100","toBlock":"safe"}]}`
	)

	logsFilter := func(t *testing.T, params []interface{}) map[string]interface{} {
		t.Helper()
		require.Len(t, params, 1)
		obj, ok := params[0].(map[string]interface{})
		require.True(t, ok, "eth_getLogs carries a single filter object")
		return obj
	}

	t.Run("concrete sibling bound is not proof the safe bound resolved", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, safeSourceTag, coldSource())

		req, params, gateErr := normalizeSafeRequest(t, ctx, ntw, numericSiblingBody)

		// Preconditions: both signals the old gate trusted are present and
		// pointing the wrong way. Without them this fixture would reject for
		// ordinary reasons and prove nothing about the bypass.
		require.Equal(t, "safe", req.EvmBlockRef(),
			"precondition: the ref must reach the gate as safe, or the old code never got as far as the second check")
		require.Equal(t, concreteFromNumber, req.EvmBlockNumber(),
			"precondition: the concrete fromBlock must have populated the number the old gate read as proof of resolution")

		requireSafeBlockUnavailable(t, gateErr, common.SafeBlockUnresolvedNoSource)
		// And the leak the gate is holding back: the tag really is still raw.
		assert.Equal(t, "safe", logsFilter(t, params)["toBlock"],
			"nothing rewrote the tag, so forwarding this would hand `safe` to whichever provider answered")
	})

	// Same forged proof, different protection: skipInterpolation keeps the tag
	// verbatim by design, and the numeric sibling used to buy a pass out of the
	// gate that catches it. The source here is HEALTHY and reporting, so the
	// refusal can only be about the directive — hence the distinct reason.
	t.Run("concrete sibling bound does not rescue skipInterpolation either", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, safeSourceTag, reportingSource())
		require.Equal(t, trustedSafeHead, ntw.EvmHighestSafeBlockNumber(ctx),
			"precondition: a source IS reporting, so a rejection here can only be the directive")

		req, params, gateErr := normalizeSafeRequestWithDirectives(t, ctx, ntw, numericSiblingBody,
			&common.RequestDirectives{SkipInterpolation: true})

		require.Equal(t, concreteFromNumber, req.EvmBlockNumber(),
			"precondition: skipInterpolation suppresses tag translation but not number caching, which is what made this shape a bypass")

		requireSafeBlockUnavailable(t, gateErr, common.SafeBlockUnresolvedSkipInterpolation)
		assert.Equal(t, "safe", logsFilter(t, params)["toBlock"])
	})

	// The other forged signal: `latest` alongside `safe` collapses the ref to
	// "latest", which used to skip the gate entirely. Rejecting this is an
	// intended behavior change — a mixed range whose safe bound cannot be
	// resolved now fails closed instead of leaking the tag.
	t.Run("unresolvable safe bound is rejected even when the ref collapses to latest", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, safeSourceTag, coldSource())

		req, params, gateErr := normalizeSafeRequest(t, ctx, ntw,
			`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"safe","toBlock":"latest"}]}`)

		require.Equal(t, "latest", req.EvmBlockRef(),
			"precondition: the safe bound must be masked by the latest collapse, or this is not the shape that used to skip the gate")

		requireSafeBlockUnavailable(t, gateErr, common.SafeBlockUnresolvedNoSource)
		assert.Equal(t, "safe", logsFilter(t, params)["fromBlock"])
	})

	// The counterweight: the gate must not have become "reject any range that
	// mentions safe". With a source reporting, the exact same request shape
	// sails through and ONLY the safe bound is rewritten — the concrete sibling
	// keeps its own value rather than being overwritten by the trusted head.
	t.Run("a resolvable safe bound passes and only that bound is rewritten", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, safeSourceTag, reportingSource())

		_, params, gateErr := normalizeSafeRequest(t, ctx, ntw, numericSiblingBody)
		require.NoError(t, gateErr, "a safe bound that resolved is not a trust-boundary failure")

		obj := logsFilter(t, params)
		assert.Equal(t, trustedSafeHex, obj["toBlock"], "the safe bound must carry the trusted head")
		assert.Equal(t, concreteFrom, obj["fromBlock"], "the caller's own lower bound must survive untouched")
	})
}

// TestNetwork_SafeBlock_GateWalksNestedObjectParams pins that the gate descends
// into object params instead of only glancing at positional ones.
//
// eth_getBlockReceipts is the discriminating method: its ReqRefs are {0},
// {0,"blockHash"} and {0,"blockNumber"}, so for the object form the POSITIONAL
// ref peeks a map — never the string "safe" — and only the nested ref sees the
// tag. A gate that inspected params[i] and stopped would find nothing here and
// wave an unresolved `safe` straight through to an upstream.
//
// Normalization walks these same paths, which is the invariant the pair below
// locks together: every position that can be REWRITTEN is a position that is
// CHECKED. Give a method another nested ref and both halves move at once.
func TestNetwork_SafeBlock_GateWalksNestedObjectParams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const nestedBody = `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts",` +
		`"params":[{"blockNumber":"safe"}]}`

	nestedBlockNumber := func(t *testing.T, params []interface{}) interface{} {
		t.Helper()
		require.Len(t, params, 1)
		obj, ok := params[0].(map[string]interface{})
		require.True(t, ok, "the block param must stay an object, not be flattened by normalization")
		return obj["blockNumber"]
	}

	t.Run("unresolvable nested safe tag is rejected", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, safeSourceTag,
			fakeUpstream("op-node-a", stalledLatestHead, 0, safeSourceTag))

		_, params, gateErr := normalizeSafeRequest(t, ctx, ntw, nestedBody)

		requireSafeBlockUnavailable(t, gateErr, common.SafeBlockUnresolvedNoSource)
		assert.Equal(t, "safe", nestedBlockNumber(t, params),
			"the tag is still raw inside the object, which is exactly what must not be forwarded")
	})

	t.Run("resolvable nested safe tag is rewritten in place", func(t *testing.T) {
		ntw := safeBlockNetwork(t, ctx, safeSourceTag,
			fakeUpstream("op-node-a", stalledLatestHead, trustedSafeHead, safeSourceTag))

		_, params, gateErr := normalizeSafeRequest(t, ctx, ntw, nestedBody)
		require.NoError(t, gateErr)
		assert.Equal(t, trustedSafeHex, nestedBlockNumber(t, params))
	})
}

// ─── the trust boundary vs. client-supplied routing ──────────────────────────

// safeRequestPinnedTo builds the request a client sends when it carries a
// `use-upstream` selector, and binds it to ctx the way Network.Forward does
// (common.RequestContextKey). That binding is the only channel through which
// requestSelector — and therefore the tip-candidate narrowing — can see the
// selector, so a test that only calls SetDirectives would exercise nothing.
func safeRequestPinnedTo(
	t *testing.T,
	ctx context.Context,
	ntw *Network,
	body string,
	selector string,
) (context.Context, *common.NormalizedRequest, *common.JsonRpcRequest) {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(body))
	req.SetNetwork(ntw)
	req.SetDirectives(&common.RequestDirectives{UseUpstream: selector})
	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	return context.WithValue(ctx, common.RequestContextKey, req), req, jrq
}

func upstreamIds(ups []common.Upstream) []string {
	ids := make([]string, 0, len(ups))
	for _, u := range ups {
		ids = append(ids, u.Id())
	}
	return ids
}

// TestNetwork_SafeBlock_UseUpstreamCannotUnresolveTrustedSafe pins the trust
// boundary as an OPERATOR decision that a client cannot narrow.
//
// `use-upstream` is a client-supplied routing preference. Resolving `safe` from
// the selector-narrowed candidate set made that preference decide whether the
// operator's boundary could be satisfied at all: a caller pinned away from the
// authoritative sources got 0 from the resolver and its request was failed
// CLOSED, even though those sources were healthy and reporting. Which upstreams
// DEFINE `safe` is the operator's call (evm.safeBlock.source plus operator-level
// selection-policy eligibility); which upstreams SERVE the resulting concrete
// block still honors the pin.
//
// Each row is a real header a client can send. The fixture is doubly
// discriminating: the pinned-to provider reports a far HIGHER safe head
// (stalledLatestHead, the shape of a provider running zero L1 confirmations),
// so a resolver that lost the trust boundary would answer with that value,
// while a resolver that keeps the selector narrowing answers 0. Only correct
// behavior yields trustedSafeHead.
func TestNetwork_SafeBlock_UseUpstreamCannotUnresolveTrustedSafe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const body = `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["safe",false]}`

	for _, tc := range []struct {
		name     string
		selector string
	}{
		// X-ERPC-Use-Upstream: provider-x — the single most common shape.
		{"pinned by id to a non-authoritative upstream", "provider-x"},
		// A whole tier pinned by tag.
		{"pinned by tag to a non-authoritative group", publicProviderTag},
		// Negation matches on the id only, so this excludes every source.
		{"negated selector excluding every authoritative source", "!op-node-*"},
		// A typo or a decommissioned id: the candidate set is empty. The
		// request cannot be served at all, but that must surface as a routing
		// failure downstream, never as "eRPC forgot its safe head".
		{"selector matching no upstream at all", "ghost-node-7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ntw := safeBlockNetwork(t, ctx, safeSourceTag,
				fakeUpstream("op-node-a", stalledLatestHead, trustedSafeHead, safeSourceTag),
				fakeUpstream("provider-x", stalledLatestHead, stalledLatestHead, publicProviderTag),
			)
			reqCtx, req, jrq := safeRequestPinnedTo(t, ctx, ntw, body, tc.selector)

			// Precondition: the pin really does exclude the only authoritative
			// source from the ROUTING set. Without this the row could pass
			// while never reproducing the condition it exists to cover.
			require.NotContains(t, upstreamIds(ntw.tipCandidateUpstreams(reqCtx, "*")), "op-node-a",
				"selector %q must pin AWAY from the source, otherwise this row proves nothing", tc.selector)

			assert.Equal(t, trustedSafeHead, ntw.EvmHighestSafeBlockNumber(reqCtx),
				"a client's routing preference must not narrow the operator's trust boundary")

			// ...and the request that carried the pin is resolved and served,
			// not rejected. This is the user-visible half of the regression.
			evm.NormalizeHttpJsonRpc(reqCtx, req, jrq)
			require.NoError(t, evm.EnforceSafeBlockResolved(reqCtx, ntw, req),
				"healthy reporting sources must not fail a safe request closed because the caller pinned elsewhere")
			require.Len(t, jrq.Params, 2)
			assert.Equal(t, trustedSafeHex, jrq.Params[0])
			assert.Equal(t, int64(trustedSafeHead), req.EvmBlockNumber())
		})
	}
}

// TestNetwork_SafeBlock_SelectorScopesServedTipNotTrustBoundary guards the split
// between eligibleTipUpstreams (no selector narrowing) and
// tipCandidateUpstreams (selector narrowing), from BOTH directions, in one
// fixture and under one selector:
//
//   - collapse tipCandidateUpstreams into eligibleTipUpstreams and the `latest`
//     assertion reddens — a request pinned to a lagging provider would be
//     promised the network-wide head, which is the "block not found" churn
//     selector-scoped served tip exists to prevent;
//   - point EvmHighestSafeBlockNumber back at tipCandidateUpstreams and the
//     `safe` assertion reddens.
//
// Broader selector-scoped served-tip coverage lives in
// TestServedTip_SelectorScoped; this one pins the two helpers' DIVERGENCE so it
// cannot be silently refactored away.
func TestNetwork_SafeBlock_SelectorScopesServedTipNotTrustBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const body = `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["safe",false]}`

	ntw := safeBlockNetwork(t, ctx, safeSourceTag,
		fakeUpstream("op-node-a", stalledLatestHead, trustedSafeHead, safeSourceTag),
		fakeUpstream("provider-x", pinnedLatestHead, stalledLatestHead, publicProviderTag),
	)
	pinned, _, _ := safeRequestPinnedTo(t, ctx, ntw, body, "provider-x")

	// Unpinned baseline: both heads are network-wide.
	require.Equal(t, stalledLatestHead, ntw.EvmHighestLatestBlockNumber(ctx))
	require.Equal(t, trustedSafeHead, ntw.EvmHighestSafeBlockNumber(ctx))

	assert.Equal(t, pinnedLatestHead, ntw.EvmHighestLatestBlockNumber(pinned),
		"`latest` MUST stay selector-scoped: advertising a head the pinned upstream lacks causes block-not-found churn")
	assert.Equal(t, trustedSafeHead, ntw.EvmHighestSafeBlockNumber(pinned),
		"`safe` MUST NOT be selector-scoped: it is an operator trust decision, not a routing one")
}

// ─── the wire status of a fail-closed safe request ───────────────────────────

// TestSafeBlockUnavailable_WireStatusIsDeliberately200 pins a DIVERGENCE, and
// the pair of assertions is the point: ErrEvmSafeBlockUnavailable reports 503
// from ErrorStatusCode(), but the HTTP layer never consults that method for this
// code — determineResponseStatusCode has no 503 branch — so a client actually
// receives HTTP 200 with a JSON-RPC error body.
//
// That is deliberate: a JSON-RPC batch carrying one failed `safe` item must not
// fail the whole HTTP response and take unrelated sub-requests down with it.
// Callers alert on the error code, not on an HTTP status.
//
// If someone later adds a 503 branch, this test reddens — which is the signal to
// update the documented status table (and to think about batch semantics) rather
// than to relax the assertion.
func TestSafeBlockUnavailable_WireStatusIsDeliberately200(t *testing.T) {
	err := common.NewErrEvmSafeBlockUnavailable(
		util.EvmNetworkId(123), safeSourceTag, common.SafeBlockUnresolvedNoSource)

	var sbErr *common.ErrEvmSafeBlockUnavailable
	require.ErrorAs(t, err, &sbErr)
	assert.Equal(t, http.StatusServiceUnavailable, sbErr.ErrorStatusCode(),
		"the error's own severity: eRPC cannot answer safely right now")

	assert.Equal(t, http.StatusOK, determineResponseStatusCode(err),
		"the WIRE status: a failed safe item must not fail the whole HTTP response")
	assert.Equal(t, http.StatusOK, determineResponseStatusCode(&HttpJsonRpcErrorResponse{Cause: err}),
		"the error-body path the server actually writes must agree with the bare-error path")

	// Control: the mapper IS live in this test. Without it, an assertion of 200
	// would also hold if determineResponseStatusCode had stopped inspecting the
	// error at all.
	require.Equal(t, http.StatusUnauthorized,
		determineResponseStatusCode(common.NewErrAuthUnauthorized("secret", "bad key")),
		"precondition: transport-level codes still map, so the 200 above is a decision and not a no-op")
}
