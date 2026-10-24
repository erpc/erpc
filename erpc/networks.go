package erpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"math"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/svm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/mempool"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type Network struct {
	networkId            string
	networkLabel         string
	projectId            string
	logger               *zerolog.Logger
	headLoopOnce         sync.Once
	appCtx               context.Context
	cfg                  *common.NetworkConfig
	inFlightRequests     *sync.Map
	failsafeExecutors    []*networkExecutor // main: failsafe rename FailsafeExecutor → networkExecutor
	rateLimitersRegistry *upstream.RateLimitersRegistry
	cacheDal             common.CacheDAL
	metricsTracker       *health.Tracker
	upstreamsRegistry    *upstream.UpstreamsRegistry
	// policyEngine replaces the legacy `selectionPolicyEvaluator *PolicyEvaluator` field
	// (and the `erpc/policy_evaluator.go` per-request `AcquirePermit` gating it owned).
	// The engine pre-computes the ordered upstream list per (network, method) tick;
	// the request path consumes the head via `policyEngine.GetOrdered`, so per-attempt
	// permit-acquisition is no longer needed.
	policyEngine        *policy.Engine
	initializer         *util.Initializer
	architectureHandler common.ArchitectureHandler

	// forkDemoted are the voters on a minority fork (EvmForkDemoted): no
	// number vote, last in routing. nil while none is.
	forkDemoted atomic.Pointer[[]string]
	// hashStale are the sources whose hash age exceeds W, or whose socket
	// closed (setHashStale): tried after the rest, before forkDemoted. nil
	// while none is.
	hashStale atomic.Pointer[[]string]

	// servedTipAll is the network-wide lane. A lane publishes its Head and
	// holds the advance-age watchdog and the regression guard's memory; the
	// pick itself is stateless (common.PickServedTip).
	servedTipAll servedTipLane

	// servedTipLanes holds lazily-materialized per-group lanes for
	// use-upstream selectors that carve out a real sub-group (e.g.
	// `flashblocks*` / `family:systx`). The pick for any selector is the same
	// stateless quorum over the matched subset; a lane adds its published
	// Head, gauge label (LaneName), watchdog and guard memory. Bounded by
	// construction: keyed by the hash of the MATCHED UPSTREAM SET (equivalent
	// selectors dedup; garbage selectors can't inflate state beyond the real
	// topology) and capped by maxServedTipLanes.
	servedTipLanes     sync.Map // map[string]*servedTipLane (key = "grp:<hash>")
	servedTipLaneCount atomic.Int32

	// headWake asks the head loop to compute every lane's Head again
	// (headsChanged); headPass is closed and replaced after each pass, for
	// taps on a selector with no lane.
	headWake chan struct{}
	headPass atomic.Pointer[chan struct{}]

	// queryTraceSource remembers which trace method answered the eth_query*
	// shim on this network (queryTraceSource* values), so a network whose
	// upstreams lack trace_block asks debug_traceBlockByNumber directly.
	queryTraceSource atomic.Int32

	// chainTracker is the network-wide tracker; intake feeds it from the
	// sources, which the reconciler sets; taps read it and are the demand.
	// mempool holds the pending transactions intake admits. All nil on
	// non-EVM networks. bg runs the intake, reconciler and tap loops.
	chainTracker *evm.ChainTracker
	intake       *networkIntake
	sources      *sourceReconciler
	taps         *indexer.Taps
	mempool      *mempool.Feed
	bg           util.Goroutines
}

// wait waits, once the app ctx ended, until no goroutine of the network's
// intake, reconciler, taps, trackers or tracker fetches runs.
func (n *Network) wait() {
	n.bg.Wait()
	if n.chainTracker != nil {
		n.chainTracker.Wait()
	}
}

// maxServedTipLanes caps the materialized group lanes per network, a backstop
// on top of the matched-set dedup. Realistic configs have 1–3 groups; beyond
// the cap, a group's request gets the stateless subset pick with no lane.
const maxServedTipLanes = 16

// servedTipLane is one Head scope: the network-wide lane (label "all", key
// "") or a use-upstream group's (label common.LaneName of the matched
// upstream set, key partitionKeyFor's). It publishes the lane's Head and
// holds the guard memory; mu orders its computations so a slower one never
// publishes over a newer one.
type servedTipLane struct {
	key       string
	selector  string
	label     string
	latest    servedTipAnchor
	finalized servedTipAnchor

	mu   sync.Mutex
	head atomic.Pointer[common.EvmHead]
	// moved is closed and replaced when Latest changes.
	moved atomic.Pointer[chan struct{}]
}

func (l *servedTipLane) anchor(finalized bool) *servedTipAnchor {
	if finalized {
		return &l.finalized
	}
	return &l.latest
}

// servedTipAnchor tracks, per process, when the served-tip VALUE was last
// seen to change — the advance-age watchdog's clock — and holds the regression
// guard's last corroborated pick (see guardServedTipRegression).
//
// The first observed value does NOT count as a change (there is nothing to
// compare against): the anchor stays unset until the process witnesses the
// value actually move.
//
// IN-PROCESS ONLY, by design: nothing here is persisted or shared between
// pods. A persisted anchor plus a strict clamp is exactly what produces the
// frozen-tip regression (see the picker's doc comment and
// networks_served_tip_invariants_test.go); every field below is rebuilt from
// live observations after a restart, and the guard always fails open on its
// own after a bounded window.
type servedTipAnchor struct {
	seenValue   atomic.Int64
	changedAtMs atomic.Int64

	// lastGoodValue / lastGoodAt is the most recent pick the guard saw
	// corroborated by a live head (within tolerance), and when it recorded it.
	// Only a LIVE-corroborated pick is stored, so the guard can never hold a
	// value that no live upstream ever backed.
	//
	// The timestamp is a monotonic time.Time, not a wall-clock UnixMilli: the
	// guard's TTL is a duration measured across an interval, and an NTP step
	// (or a container clock correction) must not be able to expire — or extend
	// — a hold that is bounded precisely so it cannot become a wedge.
	lastGoodValue atomic.Int64
	lastGoodAt    atomic.Pointer[time.Time]

	// guardState is one of the servedTipGuard* constants. The guard logs only
	// on transitions, so a sustained regression reports twice (once when it
	// starts holding, once if it gives up) instead of once per request.
	guardState atomic.Int32
}

const (
	// servedTipGuardHealthy: the pick tracks the live heads.
	servedTipGuardHealthy int32 = iota
	// servedTipGuardHolding: the pick regressed and the last corroborated pick
	// is being served in its place.
	servedTipGuardHolding
	// servedTipGuardFailedOpen: the regression outlasted
	// servedTipRegressionTTL, so the pick is served as computed.
	servedTipGuardFailedOpen
)

// servedTipRegressionTTL bounds how long the guard may serve its last
// corroborated pick instead of the current one. Past it the guard fails OPEN:
// a minute of disagreement is long enough that reality, not a transient
// poisoned ballot, is the better explanation — and a guard that can hold
// forever is the absorbing frozen-tip state this design exists to make
// impossible.
const servedTipRegressionTTL = time.Minute

// servedTipNow is the regression guard's clock while set; tests swap it
// while network goroutines read it.
var servedTipNow atomic.Pointer[func() time.Time]

func servedTipClock() time.Time {
	if now := servedTipNow.Load(); now != nil {
		return (*now)()
	}
	return time.Now()
}

// observe records v as the latest served value, stamping the anchor when the
// value changed since the last observation. moved: this caller saw v replace
// another value, the first one included.
//
// LOAD FIRST: the served tip is read several times per request and is the same
// value on almost every one of them, so the steady state must be a shared READ
// of this cache line, not an exclusive read-modify-write from every serving
// core. The Swap survives only on the (rare) path where the value really moved,
// where it is what makes exactly one caller see the transition.
func (a *servedTipAnchor) observe(v int64) (moved bool) {
	if a.seenValue.Load() == v {
		return false
	}
	old := a.seenValue.Swap(v)
	if old != v && old != 0 {
		a.changedAtMs.Store(time.Now().UnixMilli())
	}
	return old != v
}

// age returns time since the last observed value change, or -1 when no change
// has been observed yet (the advance-age gauge is skipped on -1).
func (a *servedTipAnchor) age() time.Duration {
	if ms := a.changedAtMs.Load(); ms > 0 {
		return time.Since(time.UnixMilli(ms))
	}
	return -1
}

// Bootstrap registers this network with the policy engine. The engine kicks
// off the slot's ticker and runs an initial synchronous eval so request-path
// reads through `policyEngine.GetOrdered` always see a populated cache.
//
// The upstream list is supplied as a closure so newly-bootstrapped upstreams
// become visible to the engine each tick without a re-register.
func (n *Network) Bootstrap(ctx context.Context) error {
	n.startHeads()
	if n.policyEngine == nil {
		return nil
	}
	cfg := n.cfg.SelectionPolicy
	if cfg == nil {
		cfg = &common.SelectionPolicyConfig{}
		n.cfg.SelectionPolicy = cfg
	}
	// Defensive: callers may have set Eval but skipped SetDefaults (common
	// in tests that build Config as Go struct literals). Compile here so
	// the engine never sees a nil program.
	if cfg.CompiledProgram == nil {
		if err := cfg.SetDefaults(); err != nil {
			return fmt.Errorf("selectionPolicy SetDefaults: %w", err)
		}
	}
	upstreamsFn := func() []common.Upstream {
		ptrs := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
		out := make([]common.Upstream, len(ptrs))
		for i, u := range ptrs {
			out[i] = u
		}
		return out
	}
	return n.policyEngine.RegisterNetwork(n.networkId, n.Label(), upstreamsFn, cfg)
}

// PinUpstreamOrderForTest pins the upstream ordering for this network to
// the given IDs (or alphabetical-by-id if `ids` is empty). Affects both
// the underlying registry (so cold-start reads see the pinned order) and
// the policy engine's cache when one is wired up. Test-only.
func (n *Network) PinUpstreamOrderForTest(ids ...string) {
	if n.upstreamsRegistry != nil {
		n.upstreamsRegistry.OverrideOrderForTest(n.networkId, ids...)
	}
	if n.policyEngine != nil {
		policy.OverrideOrderForTest(n.policyEngine, n.networkId, ids...)
	}
}

func (n *Network) Id() string {
	return n.networkId
}

// MetricsTracker returns the network's shared health tracker. Exposed
// so diagnostic tooling (the erpc-simulator, admin readouts) can read
// per-upstream observed metrics — the same numbers the selection
// policy's `keepHealthy` / `sortByScore` filters consume.
func (n *Network) MetricsTracker() *health.Tracker {
	return n.metricsTracker
}

// EvmBlockTime returns the network's estimated (EMA) block time, or 0 if not
// yet known. Used by the cache layer to derive realtime TTLs from block cadence.
func (n *Network) EvmBlockTime() time.Duration {
	if n.metricsTracker == nil {
		return 0
	}
	return n.metricsTracker.GetNetworkBlockTime(n.networkId)
}

// AllUpstreams returns every upstream configured on the network, in
// no particular order. Diagnostic tooling uses this to walk upstreams
// for tracker lookups without needing to know the routing order.
func (n *Network) AllUpstreams() []*upstream.Upstream {
	if n.upstreamsRegistry == nil {
		return nil
	}
	return n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId)
}

// PolicyScores returns the per-upstream `score` map produced by the
// selection-policy engine's most recent tick for `(networkID, method)`,
// or nil if the engine isn't wired up. Source of truth for "what does
// the policy rank this upstream at?" — never re-implement the PREFER_FASTEST
// weight formula client-side; read from here.
func (n *Network) PolicyScores(method string) map[string]float64 {
	if n.policyEngine == nil {
		return nil
	}
	if method == "" {
		method = "*"
	}
	return n.policyEngine.GetScores(n.networkId, method, "*")
}

// RecentPolicyDecisions returns up to `limit` most-recent policy
// engine Decisions for `(networkID, method)`, OLDEST-first. Diagnostic
// tooling uses this to render a tick-by-tick replay panel. Returns nil
// if no engine is wired up.
func (n *Network) RecentPolicyDecisions(method string, limit int) []*policy.Decision {
	if n.policyEngine == nil {
		return nil
	}
	if method == "" {
		method = "*"
	}
	return n.policyEngine.RecentDecisions(n.networkId, method, "*", limit)
}

// PolicyLastSwitchAt returns when the primary upstream last changed
// for `(networkID, method)`. Used by diagnostics to render the
// `stickyPrimary` cooldown countdown ("primary held for Xs").
func (n *Network) PolicyLastSwitchAt(method string) time.Time {
	if n.policyEngine == nil {
		return time.Time{}
	}
	if method == "" {
		method = "*"
	}
	return n.policyEngine.LastSwitchAt(n.networkId, method, "*")
}

// PolicyOrderedUpstreams returns the IDs of upstreams in the order the
// selection-policy engine currently has them ordered for the given
// method. The slot's cache is read lock-free — this is the same source
// of truth `Forward` uses to pick attempts. Returns nil if no policy
// engine is wired up or the slot hasn't ticked yet.
//
// Diagnostics tooling (the simulator, admin endpoints) uses this to
// render "position pills" that reflect the policy's real verdict —
// NOT a guess derived from per-second selection counts.
func (n *Network) PolicyOrderedUpstreams(method string) []string {
	if n.policyEngine == nil {
		return nil
	}
	if method == "" {
		method = "*"
	}
	ups := n.policyEngine.GetOrdered(n.networkId, method, "*")
	if len(ups) == 0 {
		return nil
	}
	out := make([]string, 0, len(ups))
	for _, u := range ups {
		out = append(out, u.Id())
	}
	return out
}

// SetPolicyEnginePaused gates the per-slot ticker on this network's
// selection-policy engine. While paused every slot still wakes on each
// tick but skips `tickOnce`, so the cached ordering — what `Forward`
// reads via `policyEngine.GetOrdered` — stays frozen at the last
// verdict. No-op if no policy engine is wired up (test-only networks,
// or YAML without a `selectionPolicy` block).
//
// Wired up so the eRPC simulator's pause button can stop the policy
// engine churning while traffic generation is halted. Production
// callers shouldn't need this — leave the engine running.
func (n *Network) SetPolicyEnginePaused(paused bool) {
	if n.policyEngine == nil {
		return
	}
	n.policyEngine.SetPaused(paused)
}

// SetPolicyStepLogEnabled toggles per-tick capture of the selection
// policy's chain trail. While enabled the engine's `Decision` carries
// `Output.StepLog` (the chain timeline) and DEBUG-level logs print one
// line per step + one per excluded upstream.
//
// Off by default in production (zero overhead beyond a function-call
// indirection per stdlib step). Flipped on by the simulator at boot so
// the policy-history drawer has data; production callers running with
// DEBUG-level logs may also want to enable it for incident triage.
func (n *Network) SetPolicyStepLogEnabled(enabled bool) {
	if n.policyEngine == nil {
		return
	}
	n.policyEngine.SetStepLogEnabled(enabled)
}

func (n *Network) Label() string {
	if n == nil {
		return ""
	}
	if n.networkLabel != "" {
		return n.networkLabel
	}
	return n.networkId
}

func (n *Network) ProjectId() string {
	return n.projectId
}

func (n *Network) Architecture() common.NetworkArchitecture {
	if n.cfg.Architecture == "" {
		if n.cfg.Evm != nil {
			n.cfg.Architecture = common.ArchitectureEvm
		}
	}

	return n.cfg.Architecture
}

func (n *Network) ShadowUpstreams() []*upstream.Upstream {
	return n.upstreamsRegistry.GetNetworkShadowUpstreams(n.networkId)
}

func (n *Network) Logger() *zerolog.Logger {
	return n.logger
}

// tipVoterRank is the one rule for who votes on a served-tip ballot. ok is
// false for an upstream with no head on the axis: non-EVM, no state poller,
// syncing, or no head yet. Otherwise rank orders the voters, and a ballot
// counts only the lowest rank present: 0 live, 1 benched, 2 static cap,
// 3 benched static cap.
//
// STATIC-CAP UPSTREAMS DO NOT VOTE while any live head is present. An upstream
// configured with `blockAvailability.upper.exactBlock` below the chain's head is
// declaring a SERVING RANGE ("I answer up to block X"), not observing where the
// head is — see evmTipObservation. Letting such a declaration onto the ballot
// lets two capped legacy upstreams pull the tip down: pinned at an
// `upper.exactBlock` far below the live heads, they outvote them the moment a
// third live upstream is excluded for lag, "latest" interpolates to a
// historical block, the availability gate then admits ONLY those two
// upstreams (the live ones start above the cap), and funded accounts read 0x0.
//
// BENCHED UPSTREAMS DO NOT VOTE while any non-benched one does: the health
// tracker benches a head that stayed behind peers that kept advancing
// (health.Tracker.IsHeadBenched), so one stalled node cannot hold
// "latest" at its height. A live head always outranks a cap, benched or not:
// a stalled live node is still closer to the chain than a serving range.
func tipVoterRank(u common.Upstream, finalized bool, tracker *health.Tracker) (head int64, rank int, ok bool) {
	eu, isEvm := u.(common.EvmUpstream)
	if !isEvm {
		return 0, 0, false
	}
	if sp := eu.EvmStatePoller(); sp == nil || sp.IsObjectNull() || eu.EvmSyncingState() == common.EvmSyncingStateSyncing {
		return 0, 0, false
	}
	head, capped := evmTipObservation(eu, finalized)
	if head <= 0 {
		return 0, 0, false
	}
	if capped {
		rank = 2
	}
	if tracker != nil && tracker.IsHeadBenched(u) {
		rank++
	}
	return head, rank, true
}

// evmTipBallot builds the served-tip pick for an axis under quorum q from the
// request's tip candidates (EvmRoutableUpstreams) that satisfy member (nil =
// all of them): the heads of the best-ranked voters (tipVoterRank), minus
// the voters on a minority fork, which have no number vote (spec §6.3). live
// reports whether live, non-capped heads decided it (benched or not): a
// cap-only ballot gives the regression guard no live reference.
//
// MIXED POOL vs ALL-CAPPED POOL. The rule applies per candidate set, which is
// what lets the same function build the network-wide ballot AND each
// guaranteed-method's floor ballot: caps are dropped only when the SAME set
// contains at least one live head. A set of only historical/frozen upstreams
// legitimately serves its cap — that cap is the freshest block anyone in it can
// serve — so it keeps voting. (No live head means no live reference for the
// regression guard either; it falls back to its own last corroborated pick,
// see guardServedTipRegression.)
func (n *Network) evmTipBallot(sel string, useFinalized bool, q common.ServedTipQuorum, member func(common.Upstream) bool) (common.ServedTipPick, bool) {
	// Typical pools fit the stack buffer, so the ballot stays off the heap;
	// append moves a larger one there.
	var buf [16]int64
	heads := buf[:0]
	best := -1
	for u := range n.tipCandidates(sel) {
		if member != nil && !member(u) || n.isForkDemoted(u) {
			continue
		}
		head, rank, ok := tipVoterRank(u, useFinalized, n.metricsTracker)
		if !ok || (best >= 0 && rank > best) {
			continue
		}
		if best < 0 || rank < best {
			best, heads = rank, heads[:0]
		}
		heads = append(heads, head)
	}
	return common.PickServedTip(heads, q), best >= 0 && best <= 1
}

// evmTipObservation returns an upstream's effective head for the axis, plus
// whether that value is a STATIC SERVING-RANGE DECLARATION rather than an
// observation of the chain head.
//
// The classification is a property of the CONFIG, not of a resolved bound:
// `blockAvailability.upper.exactBlock` is a constant an operator wrote down
// ("this upstream answers up to block X"), so once the chain passes X the
// upstream's effective latest stops moving and stops being a head observation.
// Every OTHER upper form is derived from the upstream's own head — most of all
// `latestBlockMinus: N`, which tracks the chain at a fixed lag — and those are
// live observations: an upstream that serves up to head-5 still knows exactly
// where the head is. The earlier dynamic test ("the resolved upper bound is
// below the head") could not tell the two apart and disenfranchised every
// latestBlockMinus upstream, turning a tip four of four upstreams could serve
// into one only one of them could.
//
// Reading the configured constant also drops the SECOND resolveAvailabilityBounds
// call per upstream per evaluation that the dynamic test required — the largest
// per-request cost this feature added — and with it a torn two-load snapshot
// (bound and head resolved at different instants, off the same moving head).
func evmTipObservation(u common.EvmUpstream, useFinalized bool) (blk int64, boundCapped bool) {
	if useFinalized {
		blk = u.EvmEffectiveFinalizedBlock()
	} else {
		blk = u.EvmEffectiveLatestBlock()
	}
	if blk <= 0 {
		return blk, false
	}
	exact := configuredUpperExactBlock(u)
	if exact == nil {
		return blk, false
	}
	sp := u.EvmStatePoller()
	if sp == nil {
		return blk, false
	}
	// Per axis: a cap the FINALIZED head has not reached yet still caps nothing
	// on the finalized ballot, even while it caps the latest one.
	head := sp.LatestBlock()
	if useFinalized {
		head = sp.FinalizedBlock()
	}
	return blk, head > 0 && *exact < head
}

// configuredUpperExactBlock returns the upstream's configured
// blockAvailability.upper.exactBlock, or nil when it declares no such constant.
func configuredUpperExactBlock(u common.Upstream) *int64 {
	cfg := u.Config()
	if cfg == nil || cfg.Evm == nil || cfg.Evm.BlockAvailability == nil || cfg.Evm.BlockAvailability.Upper == nil {
		return nil
	}
	return cfg.Evm.BlockAvailability.Upper.ExactBlock
}

// EvmRoutableUpstreams yields, in routing order, the upstreams eRPC can route
// the request to, which are also those that may vote on its served tip (and
// eRPC's peers for net_peerCount): the selection policy's eligible set, so an
// upstream the user (or an integrity guard) excluded or cordoned drops out of
// head tracking where it drops out of routing, else the full registered set
// while the policy is absent or has made no decision yet. It walks the
// policy's or registry's own slice in place, so callers on the hot path pay no
// copy.
//
// Selector-scoped served tip: when the request targets a subset of upstreams
// (the use-upstream id/tag selector), the network's `latest`/`finalized` must
// be decided AMONG that subset — otherwise a more-ahead group (e.g. base
// flashblocks vs normal) would define the tip for a request pinned to the
// other group, causing "block not found" churn. The filter is stateless, so an
// arbitrary selector value can never grow per-pod memory. No-op when there is
// no request or no selector in ctx (background/admin callers).
func (n *Network) EvmRoutableUpstreams(ctx context.Context) iter.Seq[common.Upstream] {
	return n.tipCandidates(requestSelector(ctx))
}

// tipCandidates is EvmRoutableUpstreams for the use-upstream selector sel
// ("" = every upstream).
func (n *Network) tipCandidates(sel string) iter.Seq[common.Upstream] {
	// Kept this small so it inlines: the closure then stays on the caller's
	// stack instead of costing a heap allocation per walk.
	return func(yield func(common.Upstream) bool) { n.walkTipCandidates(sel, yield) }
}

func (n *Network) walkTipCandidates(sel string, yield func(common.Upstream) bool) {
	visit := func(u common.Upstream) bool {
		if sel != "" {
			if m, _ := common.UpstreamMatchesSelector(sel, u); !m {
				return true
			}
		}
		return yield(u)
	}
	policy, all := n.eligibleSet(context.Background())
	for _, u := range policy {
		if !visit(u) {
			return
		}
	}
	for _, u := range all {
		if !visit(u) {
			return
		}
	}
}

// eligibleSet is the policy's eligible set, or, while it has none, every
// upstream of the network; the other is nil. A partially-initialized
// network (cache-only paths, tests without a registry) has neither, so the
// head accessors fail open (0).
func (n *Network) eligibleSet(ctx context.Context) (policy []common.Upstream, all []*upstream.Upstream) {
	if n.policyEngine != nil {
		if eligible := n.policyEngine.GetOrdered(n.networkId, "*", "*"); len(eligible) > 0 {
			return eligible, nil
		}
	}
	if n.upstreamsRegistry == nil {
		return nil, nil
	}
	return nil, n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
}

// requestSelector returns the use-upstream selector for the request bound to
// ctx (set at the top of Forward), or "" when there is no request/selector.
func requestSelector(ctx context.Context) string {
	if req, ok := ctx.Value(common.RequestContextKey).(*common.NormalizedRequest); ok && req != nil {
		if d := req.Directives(); d != nil {
			return d.UseUpstream
		}
	}
	return ""
}

// laneFor returns the lane of the use-upstream selector sel: the
// network-wide lane for "", the group's lazily-materialized lane when sel
// carves out a real sub-group (identity and DDoS-safe gating from
// EvmUpstreamGroupForSelector, count capped by maxServedTipLanes), else nil:
// the subset pick then runs per request with no guard, no gauges and no
// anchor, so a subset value never overwrites any gauge. A new lane publishes
// its first Head before it is returned.
func (n *Network) laneFor(sel string) *servedTipLane {
	if sel == "" {
		return &n.servedTipAll
	}
	key, label := n.EvmUpstreamGroupForSelector(context.Background(), sel)
	if key == "" {
		return nil
	}
	if l, ok := n.servedTipLanes.Load(key); ok {
		return l.(*servedTipLane)
	}
	// Cap backstop (also bounds a race racing past the cap).
	if n.servedTipLaneCount.Load() >= maxServedTipLanes {
		return nil
	}
	l := &servedTipLane{key: key, selector: sel, label: label}
	n.refreshLane(l)
	actual, loaded := n.servedTipLanes.LoadOrStore(key, l)
	if !loaded {
		n.servedTipLaneCount.Add(1)
	}
	return actual.(*servedTipLane)
}

// partitionKeyFor maps a use-upstream selector to a STABLE group key, or ""
// when the selector forms no group.
//
// A group forms only for a "simple" selector (a single glob token, optionally
// negated — e.g. `flashblocks*`, `!flashblocks*`, or an exact tag like
// `family:systx`) that carves out a real SUB-group of the network's upstreams.
// The key is a hash of the MATCHED UPSTREAM SET, not the selector text, which
// makes the whole thing bounded and cross-pod safe:
//
//   - `flashblocks*`, `fl*`, `flash*`, and a `family:flashblocks` tag that all
//     resolve to the same upstreams collapse to ONE group (dedup);
//   - prefix-enumeration / garbage selectors cannot inflate state beyond the
//     (small, topology-bounded) number of real groupings — an attacker can't
//     manufacture more distinct sets than the upstream layout allows;
//   - every pod derives the same key from the same config, so the group's
//     identity (and lane label) is consistent across pods.
//
// A group must match >=2 upstreams (single-node targeting has no peers to
// group) and fewer than ALL (matching everything is just the network-wide
// tip). A per-network cap (maxServedTipLanes) backstops pathological
// topologies. Anything else returns "": the request gets the stateless subset
// pick with no lane.
func (n *Network) partitionKeyFor(ctx context.Context, selector string) (string, []string) {
	if !isSimpleGroupSelector(selector) || n.upstreamsRegistry == nil {
		return "", nil
	}
	all := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
	if len(all) < 2 {
		return "", nil
	}
	matched := make([]string, 0, len(all))
	for _, u := range all {
		if m, _ := common.UpstreamMatchesSelector(selector, u); m {
			matched = append(matched, u.Id())
		}
	}
	if len(matched) < 2 || len(matched) >= len(all) {
		return "", nil
	}
	slices.Sort(matched)
	sum := sha256.Sum256([]byte(strings.Join(matched, "\x00")))
	return "grp:" + hex.EncodeToString(sum[:8]), matched
}

// EvmUpstreamGroupForSelector is the single source of node-GROUP identity: it maps a
// use-upstream selector to a stable group key (for keying per-group state) and a
// human-readable lane name, or ("","") when the selector doesn't carve out a real
// sub-group. partitionKeyFor provides the dedup/bounds/cross-pod-stable key; this adds
// the lane. Used by the lanes' Heads (laneFor) AND by
// the integrity module, so both corroborate/track within the SAME group a request was
// pinned to (whatever node groups the use-upstream selector carves out) — one grouping, not two.
// EvmAllUpstreams exposes the network's upstreams to the architecture layer
// (interface-asserted from architecture/evm, which cannot import this package).
// Used by the integrity state prober to enumerate probe targets.
func (n *Network) EvmAllUpstreams(ctx context.Context) []common.Upstream {
	ups := n.AllUpstreams()
	out := make([]common.Upstream, 0, len(ups))
	for _, u := range ups {
		out = append(out, u)
	}
	return out
}

func (n *Network) EvmUpstreamGroupForSelector(ctx context.Context, selector string) (key string, lane string) {
	k, ids := n.partitionKeyFor(ctx, selector)
	if k == "" {
		return "", ""
	}
	return k, common.LaneName(ids)
}

// isSimpleGroupSelector reports whether a selector is a single glob token
// (optionally negated) rather than a boolean expression — keeping automatic
// group materialization to simple patterns like `flashblocks*` / `!flashblocks*`
// and bounding the cost of resolving the matched set. Boolean combinators,
// grouping, whitespace, and embedded negation are rejected.
func isSimpleGroupSelector(selector string) bool {
	s := strings.TrimSpace(selector)
	if s == "" || len(s) > 128 {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n()|&,") {
		return false
	}
	// Allow at most one negation, only at the very start (`!prefix*`).
	if i := strings.IndexByte(s, '!'); i > 0 || strings.Count(s, "!") > 1 {
		return false
	}
	return true
}

// EvmRefreshLatestBlock asks the pollers that decide the request's latest head
// for a background refresh: in routing order, only as many of the ballot's
// voters (its best tipVoterRank) as the quorum needs, since a fresher head
// anywhere else cannot move the served value. Two passes over the candidates,
// counting then refreshing, keep a hot path such as eth_blockNumber free of
// allocation.
func (n *Network) EvmRefreshLatestBlock(ctx context.Context) {
	best, voters := -1, 0
	for u := range n.EvmRoutableUpstreams(ctx) {
		if _, rank, ok := tipVoterRank(u, false, n.metricsTracker); ok {
			switch {
			case best < 0 || rank < best:
				best, voters = rank, 1
			case rank == best:
				voters++
			}
		}
	}
	if voters == 0 {
		return
	}
	var q common.ServedTipQuorum
	if n.cfg != nil {
		q = n.cfg.Evm.ServedTipQuorum()
	}
	need := q.Of(voters)
	for u := range n.EvmRoutableUpstreams(ctx) {
		if _, rank, ok := tipVoterRank(u, false, n.metricsTracker); ok && rank == best {
			u.(common.EvmUpstream).EvmStatePoller().RefreshLatestBlock()
			if need--; need == 0 {
				return
			}
		}
	}
}

// EvmRefreshHeadsBelow asks every voter whose head is below block n for a
// background refresh (spec §6.5 catch-up): the network's latest block holds
// n back from subscribers until enough of them report it, guarantee subsets
// included, so a voter already at n is never polled for it. Each poller
// skips a refresh inside its debounce and keeps one in flight.
func (n *Network) EvmRefreshHeadsBelow(selector string, block int64) {
	for u := range n.tipCandidates(selector) {
		if head, _, ok := tipVoterRank(u, false, n.metricsTracker); ok && head < block {
			u.(common.EvmUpstream).EvmStatePoller().RefreshLatestBlock()
		}
	}
}

// EvmPollHeads polls the latest header of each upstream in ids for a hash
// vote: voters that a held vote need waits on (spec §6.5). Every lane's
// tracker asks at each publish; the network polls each upstream at most
// once per block time, and each poller keeps one poll in flight.
func (n *Network) EvmPollHeads(ids []string) {
	n.pollHeads(ids, evm.HeaderPolled)
}

// EvmPollForks polls the latest header of each voter in ids, whose vote the
// pins moved off (spec §6.3), at most once per block time per upstream as
// EvmPollHeads. Each answer is its voter's hash vote, whatever it voted
// before.
func (n *Network) EvmPollForks(ids []string) {
	n.pollHeads(ids, evm.HeaderForkPolled)
}

func (n *Network) pollHeads(ids []string, kind evm.HeaderKind) {
	if n.upstreamsRegistry == nil {
		return
	}
	every := n.EvmBlockTime()
	if every <= 0 {
		every = health.ColdBlockTime
	}
	now := time.Now()
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId) {
		if !slices.Contains(ids, u.Id()) {
			continue
		}
		p := u.EvmStatePoller()
		if p == nil || p.IsObjectNull() || n.intake != nil && !n.intake.pollDue(u.Id(), kind, now, every) {
			continue
		}
		p.PollHead(kind == evm.HeaderForkPolled)
	}
}

// tryShortCircuitFutureBlock returns a truthful null response (ok=true) for
// a numbered eth_getBlockByNumber the refusal rule calls future (spec §6.4,
// #1238): above the ceiling plus its age in blocks, on a ceiling younger
// than 1 bt or Asked again. No upstream can serve it yet, so dispatching
// and hedging across all of them only burns latency and load. It runs only
// when the network sets evm.shortCircuitFutureBlocks, and the synthesized
// response is returned directly from Forward, so it is never cached.
func (n *Network) tryShortCircuitFutureBlock(ctx context.Context, req *common.NormalizedRequest, method string) (*common.NormalizedResponse, bool) {
	if n.cfg == nil || !n.cfg.Evm.ShortCircuitFutureBlocksEnabled() {
		return nil, false
	}
	if !strings.EqualFold(method, "eth_getBlockByNumber") {
		return nil, false
	}
	v := evm.HeadRefusalOf(ctx, req)
	if v == nil || v.Rule != common.HeadFuture {
		return nil, false
	}
	jrr, err := common.NewJsonRpcResponse(req.ID(), nil, nil)
	if err != nil {
		return nil, false
	}
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	resp.SetEvmBlockNumber(v.Block)
	return resp, true
}

// countRefusal counts the refusal rule's outcome for req (spec §17): data
// is served, or retried_found after a miss; an empty or missing answer is
// retried_missing in the band, future (future_after_refresh after an Ask)
// above it. Requests naming no block, and empty answers at or below
// Latest, are no refusal.
func (n *Network) countRefusal(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error) {
	v := req.HeadRefusal()
	if v == nil || v.Block <= 0 {
		return
	}
	method, _ := req.Method()
	switch {
	case err == nil && resp != nil && !resp.IsObjectNull(ctx) && !isEmptyNetworkResult(method, resp):
		if v.Retried() {
			n.countHeadRefusal("retried_found")
		} else {
			n.countHeadRefusal("served")
		}
	case err != nil && !common.HasErrorCode(err, common.ErrCodeEndpointMissingData):
	case v.Rule == common.HeadBand:
		n.countHeadRefusal("retried_missing")
	case v.Rule == common.HeadFuture:
		n.countHeadRefusal(futureRule(v))
	}
}

// futureRule is the refusal counter's rule for a future block: Asked
// first when its ceiling was stale.
func futureRule(v *common.HeadRefusal) string {
	if v.Asked {
		return "future_after_refresh"
	}
	return "future"
}

func (n *Network) countHeadRefusal(rule string) {
	telemetry.CounterHandle(telemetry.MetricHeadRefusalTotal, n.projectId, n.Label(), rule).Inc()
}

// guardServedTipRegression returns the tip to serve for `pick`, substituting the
// last corroborated pick when `pick` sits further than the configured tolerance
// below the best CORROBORATED evidence of where the chain is.
//
// This is a physics check, independent of which voters are honest: a chain's
// head does not fall a thousand blocks between two evaluations while an upstream
// still reports the old one, so a pick that far below corroborated evidence is a
// poisoned ballot — an availability bound voting as a head (two capped
// legacy upstreams), a bogus vendor head, a rolled-back shared counter. It is
// deliberately independent of the static-cap rule in tipVoterRank: that
// removes ONE known source of low votes, this bounds the damage of any other.
//
// TWO SOURCES OF EVIDENCE, ONE CEILING. A regression is measured against the
// higher of (a) the corroborated live head, `ballot.Freshest` of a live ballot
// (the second-highest live head, or the only one), and (b) the last pick this
// process saw live-corroborated. Neither alone is enough: (a) alone is defeated
// by the guard's own headline shape — one live upstream left, two stuck ones
// outvoting it, where the second-highest live head IS the poison — and (b)
// alone would be memory with no present-tense check. (a) is second-highest
// because, measured against the raw max, one far-future/wrong-chain upstream
// made every honest pick look like a regression, so the guard never took its
// healthy branch, never recorded a value, and sat permanently disarmed with no
// metric and no log. That tolerates ONE Byzantine head, not two: two upstreams
// reporting the same far-future block are indistinguishable from two that are
// simply ahead, so they can still raise the reference and leave the guard
// silently unarmed. Nothing in a single evaluation separates those cases; the
// pick's own quorum and rememberServedTipPick's refusal to commit an
// uncorroborated leap limit the damage.
//
// A HELD value is capped at `ballot.Max` of a live ballot, the highest live
// head, so the guard can never advertise a block no live upstream has. It is
// deliberately the raw max: a ceiling a single rogue can only RAISE cannot make
// the guard serve anything the rogue chose, while a second-highest ceiling
// would collapse the hold onto the poisoned pick in the very shape the guard
// exists for (one live witness left, two stuck upstreams outvoting it).
//
// It never blocks serving and never persists anything:
//   - no pick, or maxRegressionBlocks: -1 → returns the pick untouched (a
//     request with no lane — an arbitrary use-upstream selector — never reaches
//     the guard: it deliberately materializes no state);
//   - within tolerance → a LIVE-corroborated pick becomes the new remembered
//     value (a capped one never does);
//   - below tolerance, for up to servedTipRegressionTTL since that value was
//     recorded → serves it instead, never above the highest live head, unless
//     every live voter is within tolerance of the pick (then only the memory
//     disagrees, and the pick is served);
//   - past the TTL → fails open, serves the pick as computed, and DISARMS: a
//     value that has outlived the window is no longer evidence, so the guard
//     re-arms from whatever the live heads corroborate next instead of measuring
//     forever against a number the chain has left behind.
//
// NO LIVE HEAD IS NOT A REASON TO STAND DOWN. When every live upstream leaves the
// eligible set while bound-capped ones remain, the ballot legitimately falls back
// to the caps — and a guard that requires a live reference would be absent on
// exactly the ballot shape that pulls the tip down to a cap. With no live head
// the guard measures against its own last corroborated pick
// and holds it UNCLAMPED (there is no live head to clamp to) for the same bounded
// TTL, then fails open to the capped pick. Networks that are genuinely all-capped
// never armed the guard in the first place — nothing live ever corroborated a
// pick — so they keep passing straight through.
//
// COVERAGE BOUNDARY: the remembered value's freshness comes from REQUEST TRAFFIC —
// the guard only runs when someone asks for the tip. A network that goes idle for
// longer than servedTipRegressionTTL therefore fails open on its first poisoned
// evaluation after the quiet period, exactly as a cold process does. That is the
// deliberate direction: this guard bounds the damage of a poisoned ballot on a
// busy network; it is not a memory that survives silence, because a memory that
// survives silence is the frozen-tip absorbing state this design forbids.
//
// While it holds, the served-tip lag gauge (Freshest - Tip) would read negative —
// the served value is above the corroborated freshest view — so it is clamped at
// 0; erpc_network_served_tip_regression_total is what says a hold is in progress.
func (n *Network) guardServedTipRegression(axis string, label string, anchor *servedTipAnchor, ballot common.ServedTipPick, live bool) int64 {
	pick := ballot.Tip
	if pick <= 0 {
		return pick
	}
	var corroborated, liveMax int64
	if live {
		corroborated, liveMax = ballot.Freshest, ballot.Max
	}
	tolerance := n.servedTipMaxRegressionBlocks()
	if tolerance < 0 {
		// maxRegressionBlocks: -1 — the operator turned the guard off.
		return pick
	}

	lastGood, lastGoodAt := anchor.lastGoodValue.Load(), anchor.lastGoodAt.Load()
	armed := lastGood > 0 && lastGoodAt != nil
	reference := corroborated
	if armed && lastGood > reference {
		reference = lastGood
	}
	if reference <= 0 {
		// Nothing live and nothing remembered (cold start, or a genuinely
		// all-capped network): there is no evidence to measure against, and a cap
		// must never become the reference.
		return pick
	}

	// Regressions are measured against the reference, floored at 0 so an absurd
	// tolerance disables the guard rather than underflowing.
	var minAcceptable int64
	if tolerance < reference {
		minAcceptable = reference - tolerance
	}
	if pick >= minAcceptable {
		now := servedTipClock()
		if n.rememberServedTipPick(anchor, pick, lastGood, lastGoodAt, corroborated, tolerance, now) {
			// The timestamp is a TTL clock, not an audit log: refreshing it at
			// most once a second keeps a 60s window exact to within its own
			// granularity, and keeps the ONLY heap allocation on this path — the
			// time.Time the atomic pointer must own — off all but one evaluation
			// per second. (Measured: 0 allocs/op on the throttled path.)
			if lastGoodAt == nil || now.Sub(*lastGoodAt) >= time.Second {
				at := now
				anchor.lastGoodAt.Store(&at)
			}
		}
		n.noteServedTipGuardTransition(anchor, servedTipGuardHealthy, axis, pick, reference, 0)
		return pick
	}

	if !armed {
		return pick
	}
	held := servedTipClock().Sub(*lastGoodAt)
	if held >= servedTipRegressionTTL {
		// Failing open is an event only for a guard that was holding; a memory
		// that merely aged out while the guard stood aside is dropped quietly.
		if anchor.guardState.Load() == servedTipGuardHolding && n.noteServedTipGuardTransition(anchor, servedTipGuardFailedOpen, axis, pick, reference, held) {
			telemetry.MetricNetworkServedTipRegressionTotal.
				WithLabelValues(n.projectId, n.Label(), label, axis, "failed_open").
				Inc()
		}
		anchor.lastGoodValue.Store(0)
		anchor.lastGoodAt.Store(nil)
		return pick
	}

	// Only the live voters can show that a pick is poisoned. When every one of
	// them sits within tolerance of it, the regression is against memory
	// alone, left by heads that no longer vote (benched for a dead fork, or
	// excluded), and a hold would serve a head fewer than quorum voters have.
	if live && liveMax-pick <= tolerance {
		return pick
	}

	// Serve the remembered pick — but never above the highest live head, so the
	// guard cannot advertise a block no live upstream has (the invariant the old
	// monotonic clamp broke). With no live head there is nothing to clamp to and
	// the remembered value stands as recorded.
	served := lastGood
	if liveMax > 0 && liveMax < served {
		served = liveMax
	}
	if served <= pick {
		// A HOLD MAY ONLY EVER RAISE. A memory at or below the current pick has
		// nothing to offer, and serving it would drag the tip DOWN — the exact
		// harm the guard exists to prevent, inflicted by the guard. Reachable
		// whenever the memory is stale-low and it is the live corroborated head,
		// not the memory, that tripped the tolerance.
		return pick
	}
	if n.noteServedTipGuardTransition(anchor, servedTipGuardHolding, axis, pick, reference, held) {
		telemetry.MetricNetworkServedTipRegressionTotal.
			WithLabelValues(n.projectId, n.Label(), label, axis, "held").
			Inc()
	}
	return served
}

// rememberServedTipPick decides whether `pick` may become the value the guard
// holds the network to, and stores it if so. It reports whether the memory is
// now current (so the caller may refresh its TTL clock).
//
// Three conditions, each closing a way the memory could be poisoned:
//
//   - A LIVE ballot. A capped pick must never become the remembered value, or a
//     serving range would become the block the network is later held to.
//
//   - CORROBORATED BY TWO. The pick must sit at or below the second-highest live
//     head, i.e. at least two live upstreams already have it. (With a single
//     live input that head IS the reference, so a one-upstream network behaves
//     as before.)
//
//   - CONTINUITY. A value may enter the memory only if it is within the
//     regression tolerance ABOVE the last value this process already trusted:
//     the memory itself while that is fresh, and otherwise the tip this process
//     most recently SERVED. Without it, a moment in which the eligible set is
//     majority far-future — two rogues outvoting one honest upstream while the
//     rest are lag-excluded — commits the fantasy, and when the honest fleet
//     returns the guard holds that fantasy against them for a full TTL. main
//     recovers on the next evaluation because it remembers nothing, and a
//     memory that makes us worse than no memory is not worth having.
//
//     Gating only a FRESH memory was not enough: every fail-open disarms
//     (deliberately — a value that outlived its window is not evidence), and an
//     empty memory then armed at whatever pick appeared. The window between a
//     fail-open and the next honest evaluation is exactly when a rogue majority
//     is most likely, so an unarmed guard is precisely where the continuity
//     check is needed. Falling back to the last served value closes it without
//     any new state: that value is this process's own recent history.
//
//     A TRUE first evaluation — nothing remembered and nothing ever served —
//     arms unconditionally. There is no history to be continuous with, and one
//     evaluation of exposure is exactly what a process without a guard has at
//     all times.
//
// None of this constrains what is SERVED. The healthy branch returns the pick
// either way, so no arming rule can hold, freeze or lower a tip; only what
// enters the memory is gated. A legitimate leap (a halt resuming, an L2 landing
// a batch) simply leaves the memory where it was until it ages out of its TTL,
// after which the next healthy evaluation adopts the new height — and the
// detection reference already takes the HIGHER of the memory and the live
// corroborated head, so a memory left behind is ignored rather than harmful.
func (n *Network) rememberServedTipPick(anchor *servedTipAnchor, pick int64, lastGood int64, lastGoodAt *time.Time, corroborated int64, tolerance int64, now time.Time) bool {
	if corroborated <= 0 || pick > corroborated {
		return false
	}
	// The last value this process trusted: the memory while it is still fresh,
	// otherwise the tip it most recently served (0 = nothing yet, i.e. a true
	// first evaluation).
	trusted := anchor.seenValue.Load()
	if lastGood > 0 && lastGoodAt != nil && now.Sub(*lastGoodAt) < servedTipRegressionTTL {
		trusted = lastGood
	}
	if trusted > 0 && pick > trusted+tolerance {
		return false
	}
	if lastGood != pick {
		anchor.lastGoodValue.Store(pick)
	}
	return true
}

// noteServedTipGuardTransition records the guard's state and reports whether it
// CHANGED, so both the log line and the counter fire once when a hold starts and
// once when it gives up — never once per evaluation. Lanes compute their Head
// on every head move and every second, so a per-evaluation counter would read
// as thousands of suppressed picks where there is one sustained regression.
func (n *Network) noteServedTipGuardTransition(anchor *servedTipAnchor, state int32, axis string, pick int64, reference int64, held time.Duration) bool {
	// Load first: the steady state is "still healthy", read by every serving
	// core on every evaluation, and it must not cost an exclusive line.
	if anchor.guardState.Load() == state || anchor.guardState.Swap(state) == state {
		return false
	}
	switch state {
	case servedTipGuardHolding:
		n.logger.Error().
			Str("axis", axis).
			Int64("pick", pick).
			Int64("reference", reference).
			Int64("lastGoodPick", anchor.lastGoodValue.Load()).
			Msg("served tip regressed far below the corroborated live head; serving the last corroborated tip")
	case servedTipGuardFailedOpen:
		n.logger.Error().
			Str("axis", axis).
			Int64("pick", pick).
			Int64("reference", reference).
			Dur("heldFor", held).
			Msg("served tip regression outlasted the guard window; failing open and serving the current pick")
	default:
		n.logger.Warn().
			Str("axis", axis).
			Int64("pick", pick).
			Int64("reference", reference).
			Msg("served tip recovered; the regression guard is no longer holding")
	}
	return true
}

// defaultMaxRetryableBlockDistance is the block-count fallback used when the
// network does not configure MaxRetryableBlockDistance and the block time is
// not known yet.
const defaultMaxRetryableBlockDistance = int64(128)

// retryableBlockHorizon is how far ahead of an upstream's head a requested
// block may sit and still be worth retrying, expressed as chain progress.
//
// The question — "is this block close enough that the upstream may catch up?" —
// is about how soon it arrives, so a block count answers it differently on
// every chain: 128 blocks is ~25 minutes of a 12s chain but ~30 seconds of a
// 4 blocks/s one. A minute of chain progress means the same thing everywhere.
const retryableBlockHorizon = 60 * time.Second

// maxRetryableBlockDistance is the larger of the block-count default and
// retryableBlockHorizon of chain progress.
//
// Larger is the safe direction here: the value only decides whether to keep
// trying, so widening it can cost a few extra retries but can never turn a
// serveable request into a failure. An explicit operator setting is returned
// untouched — the derived floor exists to give the DEFAULT a sane meaning
// per chain, not to override a deliberate choice.
func (n *Network) maxRetryableBlockDistance() int64 {
	if n.cfg != nil && n.cfg.Evm != nil && n.cfg.Evm.MaxRetryableBlockDistance != nil {
		return *n.cfg.Evm.MaxRetryableBlockDistance
	}
	distance := defaultMaxRetryableBlockDistance
	if blockTime := n.EvmBlockTime(); blockTime > 0 {
		if byTime := int64(retryableBlockHorizon / blockTime); byTime > distance {
			distance = byTime
		}
	}
	return distance
}

// servedTipMaxRegressionBlocks is how far below the corroborated live head a
// pick may fall before the regression guard treats it as poisoned. Defaults to
// the rollback tolerance the state poller and health tracker already use, so
// every layer agrees on what a genuine deep correction looks like. A configured
// -1 is returned as-is: it disables the guard.
func (n *Network) servedTipMaxRegressionBlocks() int64 {
	if n.cfg != nil && n.cfg.Evm != nil && n.cfg.Evm.ServedTip != nil && n.cfg.Evm.ServedTip.MaxRegressionBlocks != 0 {
		return n.cfg.Evm.ServedTip.MaxRegressionBlocks
	}
	return common.DefaultToleratedBlockHeadRollback
}

// SvmHighestLatestSlot returns the majority-vote latest slot across SVM
// upstreams. Uses common.PickServedTip with a strict-majority quorum so a single
// fast/rogue upstream cannot inflate the served tip.
func (n *Network) SvmHighestLatestSlot(ctx context.Context) int64 {
	_, span := common.StartDetailSpan(ctx, "Network.SvmHighestLatestSlot")
	defer span.End()
	pick := common.PickServedTip(n.gatherSvmTipInputs(ctx, false), common.ServedTipQuorumMajority)
	span.SetAttributes(attribute.Int64("highest_latest_slot", pick.Tip))
	return pick.Tip
}

// SvmHighestFinalizedSlot is the finalized-slot counterpart: the MAJORITY tip.
// Used by the slot-lag consensus filter as the reference "pool leader" value
// and by networkPostForward_getSlot to advertise a head to clients. Rejection
// gates want SvmHighestFinalizedSlotMax instead.
func (n *Network) SvmHighestFinalizedSlot(ctx context.Context) int64 {
	_, span := common.StartDetailSpan(ctx, "Network.SvmHighestFinalizedSlot")
	defer span.End()
	pick := common.PickServedTip(n.gatherSvmTipInputs(ctx, true), common.ServedTipQuorumMajority)
	span.SetAttributes(attribute.Int64("highest_finalized_slot", pick.Tip))
	return pick.Tip
}

// SvmHighestFinalizedSlotMax returns the MAX finalized root across SVM
// upstreams. Used only by the networkPreForward_getBlock guard: at finalized
// commitment an upstream answers -32004 for any slot above its own root, so
// the pool can serve a slot iff the LEADING upstream has rooted it. The
// majority tip is the wrong aggregate for that decision — with three upstreams
// whose roots are 1000/1000/1100, it reports 1000 and the guard would null out
// ~100 slots the third upstream is holding, converting a served request into a
// client-visible -32014.
//
// Same rationale as SvmHighestIndexedSlot and EVM's futureBlockCeiling: never
// reject a block the most-ahead upstream actually has. Zero means UNKNOWN (no
// poller has reported a root yet), and every caller must treat it as "no bound"
// rather than as slot 0.
//
// Reads the pollers directly rather than through gatherSvmTipInputs: a MAX
// needs no sorting, and this runs on the pre-forward path of every getBlock, so
// it must not allocate a slice per request.
func (n *Network) SvmHighestFinalizedSlotMax(ctx context.Context) int64 {
	_, span := common.StartDetailSpan(ctx, "Network.SvmHighestFinalizedSlotMax")
	defer span.End()
	var maxSlot int64
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId) {
		sp := u.SvmStatePoller()
		if sp == nil || sp.IsObjectNull() {
			continue
		}
		if slot := sp.FinalizedSlot(); slot > maxSlot {
			maxSlot = slot
		}
	}
	span.SetAttributes(attribute.Int64("highest_finalized_slot_max", maxSlot))
	return maxSlot
}

// SvmEnforceBlockAvailability reports whether the networkPreForward_getBlock
// guard is enabled. Defaults to true when unconfigured.
func (n *Network) SvmEnforceBlockAvailability() bool {
	if n.cfg == nil || n.cfg.Svm == nil || n.cfg.Svm.EnforceBlockAvailability == nil {
		return true
	}
	return *n.cfg.Svm.EnforceBlockAvailability
}

// SvmHighestIndexedSlot returns the MAX maxShredInsertSlot across SVM upstreams.
// This is used only by the networkPreForward_getBlock guard — its job is to
// avoid short-circuiting requests that ANY upstream can serve. It therefore
// uses MAX, not the majority (median) tip. This mirrors how EVM's
// tryShortCircuitFutureBlock uses the max head across upstreams (not PickServedTip.Tip):
// "never null out a block the most-ahead upstream actually has."
// SvmHighestLatestSlot / SvmHighestFinalizedSlot remain median-based because
// they advertise the chain head to clients — a different, conservative goal.
func (n *Network) SvmHighestIndexedSlot(ctx context.Context) int64 {
	_, span := common.StartDetailSpan(ctx, "Network.SvmHighestIndexedSlot")
	defer span.End()
	upstreams := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
	var maxSlot int64
	for _, u := range upstreams {
		sp := u.SvmStatePoller()
		if sp == nil || sp.IsObjectNull() {
			continue
		}
		slot := sp.ShredInsertSlot()
		if slot > maxSlot {
			maxSlot = slot
		}
	}
	span.SetAttributes(attribute.Int64("highest_indexed_slot", maxSlot))
	return maxSlot
}

// gatherSvmTipInputs collects slot values from SVM state pollers for
// majority-tip computation via common.PickServedTip.
func (n *Network) gatherSvmTipInputs(ctx context.Context, useFinalized bool) []int64 {
	upstreams := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
	out := make([]int64, 0, len(upstreams))
	for _, u := range upstreams {
		sp := u.SvmStatePoller()
		if sp == nil || sp.IsObjectNull() {
			continue
		}
		var slot int64
		if useFinalized {
			slot = sp.FinalizedSlot()
		} else {
			slot = sp.LatestSlot()
		}
		if slot <= 0 {
			continue
		}
		out = append(out, slot)
	}
	return out
}

// guaranteedFloor returns the lowest majority served tip across every
// configured guarantee SUBSET, or 0 when none is configured or none constrains
// the tip. Two kinds of subset, one rule (guaranteedSubsetTip):
//
//   - GuaranteedMethods — the eligible upstreams that handle the method
//     (autoIgnoreUnsupportedMethods drops the rest). "latest" never resolves
//     to a block only non-supporting upstreams have.
//   - GuaranteedFor — the eligible upstreams matching an id/tag selector. The
//     advertised tip stays servable by an operator-named GROUP, which is what a
//     mixed internal/external pool needs when consensus requires the internals
//     (consensus.requiredParticipants names them with the same vocabulary).
//
// Each subset is clamped to its OWN MAJORITY, never its minimum, so a single
// frozen member is outvoted inside its group and cannot pin the network. An
// empty subset constrains nothing.
func (n *Network) guaranteedFloor(sel string, useFinalized bool) int64 {
	if n.cfg == nil || n.cfg.Evm == nil || n.cfg.Evm.ServedTip == nil {
		return 0
	}
	st := n.cfg.Evm.ServedTip
	var floor int64
	fold := func(tip int64) {
		if tip > 0 && (floor == 0 || tip < floor) {
			floor = tip
		}
	}
	for _, g := range st.GuaranteedFor {
		fold(n.guaranteedSubsetTip(sel, useFinalized, func(u common.Upstream) bool {
			m, _ := common.UpstreamMatchesSelector(g, u)
			return m
		}))
	}
	for _, m := range st.GuaranteedMethods {
		fold(n.guaranteedSubsetTip(sel, useFinalized, func(u common.Upstream) bool {
			handle, _ := u.ShouldHandleMethod(m)
			return handle
		}))
	}
	return floor
}

// guaranteedSubsetTip is the majority tip of the guarantee subset `member`
// selects, or 0 when that subset sets no floor this evaluation.
//
// The ballot is evmTipBallot's, so a subset's static caps drop out whenever a
// live member is in it; without that the floor was a second door onto the
// capped value that the network-wide ballot keeps out. A CAP-ONLY ballot has
// two causes, and only one is a floor:
//
//   - PERMANENT: no live upstream is configured into the subset at all (the
//     archive-only trace_* shape). The cap is the freshest block anyone in it
//     can serve, so it floors the tip.
//   - TRANSIENT: live members exist but are absent from this instant's eligible
//     set — lag-excluded, cordoned, mid-restart. That is exactly the shape the
//     regression guard holds through, and flooring to the cap would hand the
//     capped value back the moment the eligible set collapsed.
//
// So a cap-only ballot floors only when the REGISTERED set, which eligibility
// churn cannot change, has no live member (anyRegisteredLiveHead).
func (n *Network) guaranteedSubsetTip(sel string, useFinalized bool, member func(common.Upstream) bool) int64 {
	pick, live := n.evmTipBallot(sel, useFinalized, common.ServedTipQuorumMajority, member)
	if pick.Inputs == 0 || (!live && n.anyRegisteredLiveHead(member, useFinalized)) {
		return 0
	}
	return pick.Tip
}

// anyRegisteredLiveHead reports whether any REGISTERED upstream matching member
// is not a static serving-range cap. An upstream with no head yet counts as
// live: "we cannot tell" must fall on the side of skipping the floor, since
// serving a stale cap is the failure the floor rule exists to prevent. Only
// reached for a cap-only subset ballot, i.e. approximately never on the hot
// path.
func (n *Network) anyRegisteredLiveHead(member func(common.Upstream) bool, useFinalized bool) bool {
	if n.upstreamsRegistry == nil {
		return false
	}
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId) {
		if !member(u) {
			continue
		}
		if eu, ok := common.Upstream(u).(common.EvmUpstream); ok {
			if _, capped := evmTipObservation(eu, useFinalized); capped {
				continue
			}
		}
		return true
	}
	return false
}

// observeServedTipMetrics exports one lane's served-tip gauges. axis
// ("latest"|"finalized") is a label rather than part of the metric name to
// keep the gauge set small. lane="all" is the network-wide pick; a named lane
// is a use-upstream group's own pick.
func (n *Network) observeServedTipMetrics(axis string, laneLabel string, tip, freshest int64, advanceAge time.Duration) {
	if tip > 0 {
		telemetry.MetricNetworkServedTipBlockNumber.
			WithLabelValues(n.projectId, n.Label(), laneLabel, axis).
			Set(float64(tip))
		// Deliberate lag: how far the served tip sits behind the corroborated
		// freshest upstream view in the same set. Clamped at 0 because the tip
		// can legitimately sit ABOVE that view while the regression guard
		// holds its last corroborated pick, and a lag gauge that reads
		// negative is a broken dashboard, not a signal; the hold has its own
		// counter (…_regression_total).
		lag := freshest - tip
		if lag < 0 {
			lag = 0
		}
		telemetry.MetricNetworkServedTipLagBlocks.
			WithLabelValues(n.projectId, n.Label(), laneLabel, axis).
			Set(float64(lag))
	}
	// Universal stuck-tip signal: seconds since this process last saw the
	// served value change. Skipped until a first change is observed.
	if advanceAge >= 0 {
		telemetry.MetricNetworkServedTipAdvanceAgeSeconds.
			WithLabelValues(n.projectId, n.Label(), laneLabel, axis).
			Set(advanceAge.Seconds())
	}
}

func (n *Network) EvmLowestFinalizedBlockNumber(ctx context.Context) int64 {
	ctx, span := common.StartDetailSpan(ctx, "Network.EvmLowestFinalizedBlockNumber", trace.WithAttributes(
		attribute.String("network.id", n.networkId),
	))
	defer span.End()

	upstreams := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
	var minBlock int64 = 0
	var initialized bool = false

	for _, u := range upstreams {
		statePoller := u.EvmStatePoller()
		if statePoller == nil {
			continue
		}

		// Check if the node is syncing - skip syncing nodes as their block numbers may be unreliable
		if u.EvmSyncingState() == common.EvmSyncingStateSyncing {
			n.logger.Debug().Str("upstreamId", u.Id()).Msg("skipping syncing upstream for lowest finalized block calculation")
			continue
		}

		// Use effective finalized block which considers blockAvailability.upper config
		upBlock := u.EvmEffectiveFinalizedBlock()
		// Skip upstreams that haven't determined finalized block yet (returning 0)
		if upBlock > 0 {
			if !initialized || upBlock < minBlock {
				minBlock = upBlock
				initialized = true
			}
		}
	}

	span.SetAttributes(attribute.Int64("lowest_finalized_block", minBlock))
	return minBlock
}

func (n *Network) getFailsafeExecutor(ctx context.Context, req *common.NormalizedRequest) *networkExecutor {
	method, _ := req.Method()
	finality := req.Finality(ctx)
	// Request kind: "internal" = erpc's own auxiliary fetches (e.g. integrity's
	// canonical corroboration), "user" = client traffic. Lets a policy give the
	// deduplicated internal fetches consensus while user methods rely on
	// integrity validation (matchRequestKind).
	kind := "user"
	if dirs := req.Directives(); dirs != nil && dirs.IsInternal {
		kind = "internal"
	}

	// matchCommitment compares the commitment erpc pins on the wire, so a rule
	// is a function of the bytes the multiplexer and cache key on. Non-SVM
	// requests carry no commitment.
	commitment := "none"
	if n.cfg.Architecture == common.ArchitectureSvm {
		commitment = svm.FailsafeCommitment(ctx, n, req)
	}

	// Iterate through executors in config order and return the first match.
	// This respects the user-defined priority order in the config file.
	for _, fe := range n.failsafeExecutors {
		mp := fe.MatchMethod()
		methodMatches := mp == "*"
		if !methodMatches {
			methodMatches, _ = common.WildcardMatch(mp, method)
		}

		fl := fe.MatchFinality()
		finalityMatches := len(fl) == 0 || slices.Contains(fl, finality)

		mc := fe.MatchCommitment()
		commitmentMatches := len(mc) == 0 || slices.Contains(mc, commitment)

		mk := fe.MatchRequestKind()
		kindMatches := mk == "*" || mk == "" || mk == kind

		if methodMatches && finalityMatches && commitmentMatches && kindMatches {
			return fe
		}
	}

	return nil
}

// selectionOrder is the selection policy's order of the upstreams for req:
// the engine's order, or registration order until its first tick, without
// upstreams whose method config excludes the method.
func (n *Network) selectionOrder(ctx context.Context, req *common.NormalizedRequest, method string) []common.Upstream {
	_, upstreamSpan := common.StartDetailSpan(ctx, "PolicyEngine.GetOrdered")
	var upsList []common.Upstream
	if n.policyEngine != nil {
		// Pass the request's actual finality so per-finality slots
		// (when EvalPerFinality is on) resolve to the bucket-specific
		// ordering. Networks not configured per-finality see "*" and
		// resolve to the wildcard slot regardless.
		finality := req.Finality(ctx).String()
		// When the boundary axis (EvalPerBoundary) is on, scope selection to
		// the request's block-availability lane — the set of upstreams whose
		// configured bounds can serve this block. laneIDs stays nil when the
		// axis is off or the request has no resolvable block number, in which
		// case GetOrderedInLane behaves exactly like GetOrdered.
		var laneIDs []string
		if n.policyEngine.PerBoundaryEnabled(n.networkId) {
			laneIDs = n.eligibleUpstreamIDsForBoundary(ctx, method, req)
			if common.IsTracingDetailed && len(laneIDs) > 0 {
				upstreamSpan.SetAttributes(attribute.Int("upstreams.lane_size", len(laneIDs)))
			}
		}
		upsList = n.policyEngine.GetOrderedInLane(n.networkId, method, finality, laneIDs)
	}
	if len(upsList) == 0 {
		// Cold-start fallback: serve the raw registration order until the
		// engine's first tick completes for this slot.
		for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId) {
			upsList = append(upsList, u)
		}
	}
	// Enforce method eligibility at selection time: an upstream whose
	// ignoreMethods/allowMethods config (including entries added by
	// autoIgnoreUnsupportedMethods) excludes this method would only be
	// skipped at forward time anyway — but letting it into the request's
	// upstream list makes it consume retry/hedge/consensus attempts and
	// emit instant "method ignored" skips; under consensus fan-out those
	// skips become phantom participants. When EVERY upstream ignores the
	// method, keep the full list so the request fails with the descriptive
	// per-upstream "method ignored" error instead of a generic
	// "no upstreams found".
	if eligible, dropped := filterMethodEligible(upsList, method); dropped > 0 && len(eligible) > 0 {
		upstreamSpan.SetAttributes(attribute.Int("upstreams.method_ineligible", dropped))
		upsList = eligible
	}
	upstreamSpan.SetAttributes(attribute.Int("upstreams.count", len(upsList)))
	if common.IsTracingDetailed {
		ids := make([]string, len(upsList))
		for i, u := range upsList {
			ids[i] = u.Id()
		}
		upstreamSpan.SetAttributes(
			attribute.String("upstreams.sorted", strings.Join(ids, ", ")),
		)
	}
	upstreamSpan.End()
	return upsList
}

// preferUpstreams reorders ups by hints only, every upstream staying
// eligible: upstreams whose poller has the block first, then failover tiers,
// then voters with a stale hash vote, and voters on a minority fork last.
func (n *Network) preferUpstreams(ctx context.Context, req *common.NormalizedRequest, method string, upsList []common.Upstream) []common.Upstream {
	// For a specific block, try upstreams whose poller already has it first.
	// Ordering hints only: every upstream stays eligible.
	if n.Architecture() == common.ArchitectureEvm {
		if bn := n.routingBlockNumber(ctx, req); bn > 0 {
			upsList = n.partitionUpstreamsByLatestBlock(upsList, bn)
			upsList = preferTipLeaderForNearTipGetBlock(upsList, method, bn, n.requestHead(req).Leader)
		}
	}

	// Failover tiering: try default-tier upstreams before fallback-tier ones,
	// keeping score order within each tier.
	if n.cfg.Failover.Enabled() {
		upsList = tierUpstreamsByGroup(upsList)
	}
	return n.lateLast(upsList)
}

// lateLast moves sources with a stale hash vote (spec §6.1), then voters on
// a minority fork (§6.3), to the end of upsList, keeping each part's order:
// a half-open socket keeps its upstream's rank in the policy for a while,
// and a request it holds waits for its timeout. upsList is the request's
// own copy.
func (n *Network) lateLast(upsList []common.Upstream) []common.Upstream {
	stale, demoted := n.hashStale.Load(), n.forkDemoted.Load()
	if stale == nil && demoted == nil {
		return upsList
	}
	rank := func(u common.Upstream) int {
		switch {
		case demoted != nil && slices.Contains(*demoted, u.Id()):
			return 2
		case stale != nil && slices.Contains(*stale, u.Id()):
			return 1
		}
		return 0
	}
	if !slices.ContainsFunc(upsList, func(u common.Upstream) bool { return rank(u) > 0 }) {
		return upsList
	}
	out := make([]common.Upstream, 0, len(upsList))
	for r := 0; r <= 2; r++ {
		for _, u := range upsList {
			if rank(u) == r {
				out = append(out, u)
			}
		}
	}
	return out
}

func (n *Network) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	startTime := time.Now()
	req.SetNetwork(n)
	req.SetCacheDal(n.cacheDal)

	// Apply default directives from config
	req.ApplyDirectiveDefaults(n.cfg.DirectiveDefaults)

	method, _ := req.Method()
	lg := n.logger.With().Str("method", method).Interface("id", req.ID()).Str("ptr", fmt.Sprintf("%p", req)).Logger()

	// Start a span for network forwarding
	ctx, forwardSpan := common.StartSpan(ctx, "Network.Forward",
		trace.WithAttributes(
			attribute.String("network.id", n.networkId),
			attribute.String("request.method", method),
		),
	)

	// Bind the request to ctx so the pre-forward and short-circuit paths read
	// its Head and its use-upstream subset (see EvmRoutableUpstreams /
	// requestSelector). The failsafe executor re-sets the same value below
	// for its own readers.
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	if common.IsTracingDetailed {
		forwardSpan.SetAttributes(
			attribute.String("request.id", fmt.Sprintf("%v", req.ID())),
			attribute.String("user.id", req.UserId()),
			attribute.String("agent.name", req.AgentName()),
		)
	}
	defer forwardSpan.End()

	if lg.GetLevel() == zerolog.TraceLevel {
		lg.Debug().Object("request", req).Msgf("forwarding request for network")
	} else {
		lg.Debug().Msgf("forwarding request for network")
	}

	// Static response short-circuit. Checked after method extraction and before
	// the multiplexer/cache/upstream-selection path so matching requests never
	// touch any upstream. See StaticResponseConfig for match semantics.
	if len(n.cfg.StaticResponses) > 0 {
		if resp, ok := n.tryServeStaticResponse(ctx, &lg, req, method); ok {
			forwardSpan.SetAttributes(attribute.Bool("static_response.hit", true))
			return resp, nil
		}
	}

	// Route safe-tagged requests before multiplexing and cache lookup.
	if n.cfg.Architecture == common.ArchitectureEvm {
		if err := evm.ApplySafeBlockSource(ctx, n, req); err != nil {
			common.SetTraceSpanError(forwardSpan, err)
			return nil, err
		}

		// MIP-16 eth_query*: the query executor answers it for both transports
		// (see forwardQuery). Shim sub-requests carry a parent id and are
		// ordinary methods, so they never re-enter here.
		if queryMethod := canonicalQueryMethod(method); queryMethod != "" {
			resp, err := n.forwardQuery(ctx, req, queryMethod)
			if err != nil {
				common.SetTraceSpanError(forwardSpan, err)
			}
			return resp, err
		}
	}
	// One Head per request (spec §7), taken before the multiplexer and the
	// cache, and its tags interpolated to it: the cache key, every attempt
	// and hedge, and the response all see the same latest.
	if n.cfg.Architecture == common.ArchitectureEvm {
		n.requestHead(req)
	}
	if err := n.prepareRequest(ctx, req); err != nil {
		common.SetTraceSpanError(forwardSpan, err)
		return nil, err
	}
	forwardSpan.SetAttributes(attribute.String("request.finality", req.Finality(ctx).String()))

	mlx, resp, err := n.handleMultiplexing(ctx, &lg, req, startTime)
	if err != nil || resp != nil {
		// When the original request is already fulfilled by multiplexer (follower path)
		forwardSpan.SetAttributes(
			attribute.Bool("multiplexed", true),
			attribute.String("multiplexer.role", "follower"),
		)
		if err != nil {
			common.SetTraceSpanError(forwardSpan, err)
		}
		return resp, err
	}
	if mlx != nil {
		forwardSpan.SetAttributes(
			attribute.String("multiplexer.hash", mlx.hash),
			attribute.String("multiplexer.role", "leader"),
		)
		defer n.cleanupMultiplexer(mlx)
	}

	if n.cacheDal != nil && !req.ShouldSkipCacheRead("") {
		lg.Debug().Msgf("checking cache for request")
		resp, err := n.cacheDal.Get(ctx, req)
		if err != nil {
			lg.Debug().Err(err).Msgf("could not find response in cache")
		} else if resp != nil && !resp.IsObjectNull(ctx) {
			if lg.GetLevel() <= zerolog.DebugLevel {
				lg.Debug().Object("response", resp).Msgf("response served from cache")
			} else {
				lg.Info().Msgf("response served from cache")
			}
			if mlx != nil {
				mlx.Close(ctx, resp, err)
			}
			forwardSpan.SetAttributes(attribute.Bool("cache.hit", true))
			return resp, err
		}
		forwardSpan.SetAttributes(attribute.Bool("cache.hit", false))
	}

	upsList := n.selectionOrder(ctx, req, method)

	if len(upsList) == 0 {
		err := common.NewErrNoUpstreamsFound(n.projectId, n.networkId)
		common.SetTraceSpanError(forwardSpan, err)
		if mlx != nil {
			mlx.Close(ctx, nil, err)
		}
		return nil, err
	}

	upsList = n.preferUpstreams(ctx, req, method, upsList)

	// SVM history reads go deepest known floor first within each tier, so the
	// first answer is the most complete one the pool holds.
	if n.Architecture() == common.ArchitectureSvm && svm.IsHistoryMethod(method) {
		upsList = svm.OrderByHistoryDepth(upsList, func(a, b common.Upstream) bool {
			return isFallbackTier(a) == isFallbackTier(b)
		})
	}

	// Architecture-specific pruning of the upstream list. Currently only SVM
	// uses this hook; both filters are gated on ArchitectureSvm so EVM networks
	// never enter this block.
	if n.cfg.Architecture == common.ArchitectureSvm && n.cfg.Svm != nil {
		// (1) Consensus slot-lag prefilter — excludes upstreams whose
		// FinalizedSlot trails the pool by more than MaxFinalizedSlotLag, but
		// only when a consensus policy is active AND the request actually reads
		// at commitment=finalized. For non-consensus paths the existing
		// score-based retry already handles stale upstreams. The reference slot
		// is the single-liar-safe clamped leader (see ReferenceFinalizedSlot),
		// not the raw pool max — a lone upstream reporting an inflated finalized
		// slot must not shrink the pool to itself.
		//
		// The gate is IsFinalizedCommitment, NOT req.Finality()==Finalized:
		// a moving-head read at commitment=finalized (getBalance and friends)
		// is classified Realtime because the rooted head advances every ~400ms,
		// yet it still evaluates against the upstream's ROOTED slot — which is
		// exactly what this filter compares. The two propositions used to
		// coincide; they no longer do.
		//
		// An explicit 0 disables the filter; nil never reaches here (SetDefaults
		// is the only place that fills it in).
		if lag := n.cfg.Svm.MaxFinalizedSlotLag; lag != nil && *lag > 0 {
			fe := n.getFailsafeExecutor(ctx, req)
			if fe != nil && fe.consensus != nil && svm.IsFinalizedCommitment(ctx, n, req) {
				before := len(upsList)
				reference := svm.ReferenceFinalizedSlot(upsList, *lag)
				upsList = svm.FilterByFinalizedSlotLag(upsList, *lag, reference)
				if len(upsList) != before {
					lg.Debug().
						Int("before", before).
						Int("after", len(upsList)).
						Int64("maxFinalizedSlotLag", *lag).
						Int64("referenceSlot", reference).
						Msg("svm: filtered stale upstreams for consensus")
				}
			}
		}
		// (2) minContextSlot prefilter — an upstream whose tracked slot (at the
		// request's commitment) is behind the caller's minContextSlot is a
		// guaranteed -32016 round-trip; skip it before selection. Applies to any
		// SVM request carrying the param, with or without consensus. Defensive
		// fallback inside the filter keeps the pool non-empty so the -32016
		// failover path still reports the truth when every upstream trails.
		if mcs := svm.MinContextSlotOf(ctx, req); mcs > 0 {
			before := len(upsList)
			// Same commitment-vs-finality distinction as (1): pick the slot
			// counter the upstream will actually evaluate minContextSlot
			// against.
			finalized := svm.IsFinalizedCommitment(ctx, n, req)
			upsList = svm.FilterByMinContextSlot(upsList, mcs, finalized)
			if len(upsList) != before {
				lg.Debug().
					Int("before", before).
					Int("after", len(upsList)).
					Int64("minContextSlot", mcs).
					Msg("svm: filtered upstreams behind minContextSlot")
			}
		}
	}

	// Set upstreams on the request
	req.SetUpstreams(upsList)

	// The refusal rule decides the request's block once, before dispatch
	// (spec §6.4): a stale ceiling is Asked here, a band block refreshes
	// the voters below it. A future block, with shortCircuitFutureBlocks,
	// gets the truthful null with no dispatch; it runs before rate limiting
	// so it consumes no permit, and before the probe-bus publish so it is
	// never mirrored to excluded upstreams.
	if n.cfg.Architecture == common.ArchitectureEvm {
		evm.HeadRefusalOf(ctx, req)
	}
	if resp, ok := n.tryShortCircuitFutureBlock(ctx, req, method); ok {
		forwardSpan.SetAttributes(attribute.Bool("future_block.short_circuit", true))
		n.countRefusal(ctx, req, resp, nil)
		if mlx != nil {
			mlx.Close(ctx, resp, nil)
		}
		return resp, nil
	}

	// Feed the per-network probe-bus AFTER we know the request is
	// actually going to dispatch to an upstream (i.e. not a
	// cache-hit / static-response / follower-multiplexer /
	// future-block short-circuit, all of which returned earlier). The publish is
	// non-blocking and drops on overflow — request latency is never
	// affected. The Prober (if any) samples from this feed to mirror
	// the request against currently-excluded upstreams so their
	// tracker counters get refreshed without touching real traffic.
	// No-op for networks whose policy chain doesn't include
	// `probeExcluded`.
	if n.policyEngine != nil {
		n.policyEngine.PublishRequest(n.networkId, req)
	}

	// Network-level pre-forward (executed after upstream selection) for upstream-aware logic
	if n.architectureHandler != nil {
		if handled, resp, err := n.architectureHandler.HandleNetworkPreForward(ctx, n, upsList, req); handled {
			if err != nil {
				if mlx != nil {
					mlx.Close(ctx, nil, err)
				}
				return nil, err
			}
			if mlx != nil {
				mlx.Close(ctx, resp, nil)
			}
			return resp, nil
		}
	}

	// 3) Check if we should handle this method on this network
	if err := n.shouldHandleMethod(req, method, upsList); err != nil {
		if mlx != nil {
			mlx.Close(ctx, nil, err)
		}
		return nil, err
	}

	// 3) Apply rate limits
	if err := n.acquireRateLimitPermit(ctx, req); err != nil {
		if mlx != nil {
			mlx.Close(ctx, nil, err)
		}
		return nil, err
	}

	// 5) Iterate over upstreams and forward the request until success or fatal failure
	tryForward := func(
		u common.Upstream,
		req *common.NormalizedRequest,
		execSpanCtx context.Context,
		lg *zerolog.Logger,
		hedge int,
		attempt int,
		retry int,
	) (resp *common.NormalizedResponse, err error) {
		ctx, span := common.StartDetailSpan(execSpanCtx, "Network.TryForward")
		defer span.End()

		// hedge > 0 means failsafe spawned this attempt as a hedge (not the
		// primary). Threaded down explicitly into doForward → Upstream.Forward
		// so the per-upstream rate counters stay clean. The hedge policy lives
		// at this network layer, so this is where the signal originates.
		isHedgeAttempt := hedge > 0

		lg.Debug().Int("hedge", hedge).Int("attempt", attempt).Int("retry", retry).Msgf("trying to forward request to upstream")

		// Selection-policy permit acquisition (legacy `acquireSelectionPolicyPermit`)
		// is gone — the new policy engine pre-computes the ordered upstream list
		// per (network, method) and the request path just consumes it. We still
		// thread isHedgeAttempt down so per-upstream rate counters aren't
		// inflated by hedge fan-out.
		resp, err = n.doForward(ctx, u, req, false, isHedgeAttempt)

		if err != nil && !common.IsNull(err) {
			// If upstream complains that the method is not supported let's dynamically add it ignoreMethods config
			if common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) {
				go u.IgnoreMethod(method)
			}

			lg.Debug().Object("response", resp).Err(err).Msgf("finished forwarding request to upstream with error")
			return nil, err
		}

		lg.Debug().Object("response", resp).Msgf("finished forwarding request to upstream with success")

		return resp, err
	}
	// This is the only way to pass additional values to failsafe policy executors context
	ectx := context.WithValue(ctx, common.RequestContextKey, req)

	failsafeExecutor := n.getFailsafeExecutor(ctx, req)
	if failsafeExecutor == nil {
		return nil, errors.New("no failsafe executor found for this request")
	}

	// Add tracing for which failsafe policy was selected
	forwardSpan.SetAttributes(
		attribute.String("failsafe.matched_method", failsafeExecutor.MatchMethod()),
		attribute.String("failsafe.matched_finalities", fmt.Sprintf("%v", failsafeExecutor.MatchFinality())),
	)

	// Build the per-execution upstream-loop closure. This is what the new
	// network executor invokes (potentially multiple times for retry/hedge,
	// and per-slot for consensus).
	sweepFn := func(execSpanCtx context.Context, effectiveReq *common.NormalizedRequest, oneUpstreamOnly bool) (*common.NormalizedResponse, error) {
		snap := effectiveReq.ExecState().Snapshot()
		_, execSpan := common.StartSpan(execSpanCtx, "Network.forwardAttempt",
			trace.WithAttributes(
				attribute.String("network.id", n.networkId),
				attribute.String("request.method", method),
				attribute.Int("execution.attempt", snap.Attempts),
				attribute.Int("execution.retry", snap.Retries),
				attribute.Int("execution.hedge", snap.Hedges),
			),
		)
		defer execSpan.End()

		if common.IsTracingDetailed {
			execSpan.SetAttributes(
				attribute.String("request.id", fmt.Sprintf("%v", effectiveReq.ID())),
			)
		}

		if ctxErr := execSpanCtx.Err(); ctxErr != nil {
			cause := context.Cause(execSpanCtx)
			if cause != nil {
				common.SetTraceSpanError(execSpan, cause)
				return nil, cause
			} else {
				common.SetTraceSpanError(execSpan, ctxErr)
				return nil, ctxErr
			}
		}
		// Network-scope timeout is applied inside networkExecutor.Run.
		// Per-attempt enforcement here would double-apply and break retry budgets.

		var bestResp *common.NormalizedResponse
		var lastErr error
		maxLoopIterations := effectiveReq.UpstreamsCount()
		if oneUpstreamOnly {
			maxLoopIterations = 1
		}
		attempted := make(map[string]struct{}, maxLoopIterations)
		// The fallback escape below swaps in its own picker, leaving the
		// request's routed list untouched for later retries.
		nextUpstream := effectiveReq.NextUpstream
		// An empty result is a miss (rather than a legitimate answer, like a
		// pending tx's null receipt) only for a concrete block whose empty
		// answer the refusal rule does not call final.
		emptyIsMiss := func(ctx context.Context) bool {
			return requestBlockNumber(ctx, effectiveReq) > 0 && !evm.EmptyResultBeyondConfidence(ctx, effectiveReq)
		}

	escalationLoop:
		for {
			for loopIteration := 0; loopIteration < maxLoopIterations; loopIteration++ {
				loopCtx, loopSpan := common.StartDetailSpan(execSpanCtx, "Network.UpstreamLoop")
				if loopIteration > 0 {
					// Non-first picks of this execution are sweep iterations —
					// tag the context so the attempt record reads sweep instead
					// of primary/retry (see common.WithSweepIteration).
					loopCtx = common.WithSweepIteration(loopCtx)
				}
				if ctxErr := loopCtx.Err(); ctxErr != nil {
					cause := context.Cause(loopCtx)
					if cause == nil {
						cause = ctxErr
					}
					common.SetTraceSpanError(loopSpan, cause)
					loopSpan.End()
					return nil, cause
				}

				u, selErr := nextUpstream()
				if selErr != nil {
					loopSpan.SetAttributes(
						attribute.Bool("upstreams_exhausted", true),
						attribute.String("error", selErr.Error()),
					)
					loopSpan.End()
					break
				}

				if _, seen := attempted[u.Id()]; seen {
					// Already tried in this execution — MarkUpstreamCompleted freed
					// it from ConsumedUpstreams (retryable error or empty result) and
					// UpstreamIdx wrapped around. Release the reservation so the
					// upstream is available for the next failsafe retry round.
					effectiveReq.ConsumedUpstreams.Delete(u)
					loopSpan.SetAttributes(attribute.Bool("duplicate_selection", true))
					loopSpan.End()
					break
				}
				attempted[u.Id()] = struct{}{}

				loopSpan.SetAttributes(attribute.String("upstream.id", u.Id()))
				if eu, ok := u.(common.EvmUpstream); ok {
					if sp := eu.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
						loopSpan.SetAttributes(
							attribute.Int64("upstream.latest_block", sp.LatestBlock()),
							attribute.Int64("upstream.finalized_block", sp.FinalizedBlock()),
						)
					}
				}

				ulg := lg.With().Str("upstreamId", u.Id()).Logger()
				ulg.Debug().
					Interface("id", effectiveReq.ID()).
					Str("ptr", fmt.Sprintf("%p", effectiveReq)).
					Str("selectedUpstream", u.Id()).
					Msg("selected upstream from list")

				// Pre-forward: block availability gating → skip to next upstream
				if skipErr, isRetryable := n.checkUpstreamBlockAvailability(loopCtx, u, effectiveReq, method); skipErr != nil {
					n.handleBlockSkip(loopCtx, loopSpan, &ulg, u, effectiveReq, method, skipErr, isRetryable)
					// Lets the fallback escape below see the skip; a consensus
					// slot keeps reporting it as no attempt.
					if !oneUpstreamOnly {
						lastErr = skipErr
					}
					loopSpan.End()
					continue
				}

				hedges := snap.Hedges
				attempts := snap.Attempts
				if hedges > 0 {
					finality := effectiveReq.Finality(loopCtx)
					telemetry.CounterHandle(telemetry.MetricNetworkHedgedRequestTotal,
						n.projectId, n.Label(), u.Id(), method, fmt.Sprintf("%d", hedges),
						finality.String(), effectiveReq.UserId(), effectiveReq.AgentName(),
					).Inc()
				}

				r, err := tryForward(u, effectiveReq, loopCtx, &ulg, hedges, attempts, snap.Retries)
				if e := n.normalizeResponse(loopCtx, effectiveReq, r); e != nil {
					ulg.Error().Err(e).Msgf("failed to normalize response")
					err = e
				}
				effectiveReq.MarkUpstreamCompleted(loopCtx, u, r, err)

				// Hedge cancelled → this execution lost the race, bail out
				if hedges > 0 && common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled) {
					n.recordHedgeDiscard(loopCtx, loopSpan, &ulg, u, effectiveReq, method, err, attempts, hedges)
					loopSpan.End()
					return nil, common.NewErrUpstreamHedgeCancelled(u.Id(), err)
				}
				_ = attempts // keep symbol live for future telemetry callsites

				if r != nil {
					r.SetUpstream(u)
					r.WithRequest(effectiveReq)
				}

				// The refusal rule (spec §6.4): an empty or missing answer for a
				// block above Latest is a retryable miss once, in the band, and
				// the retry asks the upstreams whose head reaches the block
				// first. After it, and above the band, an empty answer is the
				// answer and a missing one ends the request.
				refusal := effectiveReq.HeadRefusal()
				emptyAnswer := err == nil && r != nil && !r.IsObjectNull() && isEmptyNetworkResult(method, r)
				finalEmpty := false
				if refusal.AboveLatest() && (emptyAnswer || common.HasErrorCode(err, common.ErrCodeEndpointMissingData)) {
					final := refusal.Final()
					switch {
					case refusal.Miss():
						nextUpstream = n.headFirst(effectiveReq, attempted, refusal.Block)
					case final && !emptyAnswer:
						common.SetTraceSpanError(loopSpan, err)
						loopSpan.End()
						return nil, err
					}
					finalEmpty = final && emptyAnswer
				}

				// Return immediately when the result is usable:
				//  - Non-empty success always qualifies.
				//  - Emptyish success qualifies when the refusal rule calls it
				//    final, or when the method is in emptyResultAccept and
				//    consensus is not required, because failsafe would accept
				//    the empty result anyway so trying more upstreams just
				//    wastes time on slow ones.
				//  - Otherwise emptyish results continue to the next upstream.
				if err == nil && r != nil && !r.IsObjectNull() {
					emptyish := isEmptyNetworkResult(method, r)
					acceptEmpty := !emptyish || finalEmpty ||
						(!failsafeExecutor.HasConsensus() &&
							failsafeExecutor.acceptsEmptyResult(method, r))
					if acceptEmpty {
						st := effectiveReq.ExecState()
						st.MarkUpstreamAttemptWon(r.UpstreamId())
						s := st.Snapshot()
						r.SetAttempts(s.Attempts)
						r.SetRetries(s.Retries)
						r.SetHedges(s.Hedges)
						loopSpan.SetStatus(codes.Ok, "")
						if emptyish {
							loopSpan.SetAttributes(attribute.Bool("emptyish_accepted", true))
						}
						loopSpan.End()
						return r, nil
					}
				}

				// Deterministic errors: client faults and execution reverts are the
				// same on every upstream — no point trying others.
				if common.IsClientError(err) || common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
					common.SetTraceSpanError(loopSpan, err)
					loopSpan.End()
					return nil, err
				}

				// Track best result and continue to next upstream.
				if err != nil {
					lastErr = err
					common.SetTraceSpanError(loopSpan, err)
				} else if r != nil {
					bestResp = r
					loopSpan.SetStatus(codes.Ok, "")
					if isEmptyNetworkResult(method, r) && n.cfg.Failover.Enabled() && emptyIsMiss(loopCtx) {
						lastErr = common.NewErrEndpointMissingData(
							fmt.Errorf("upstream responded emptyish"), u,
						)
					}
				}
				loopSpan.End()

				// An error explicitly marked non-retryable toward the network is
				// an authoritative answer — e.g. an OP-Stack sequencer rate limit
				// every provider shares. Every remaining upstream returns the same
				// verdict, so sweeping them burns latency and quota for nothing.
				// Same stop condition shouldRetryWithReason already applies on the
				// retry path.
				//
				// A skipped slot is deliberately NOT an example here. Solana's
				// -32009 reads "skipped, OR missing in long-term storage", and the
				// second half is per-provider archive policy, not chain truth — so
				// it sweeps (architecture/svm/error_normalizer.go). Only mark a
				// verdict non-retryable when EVERY provider must return it by
				// construction; "this node does not have it" never qualifies.
				//
				// Stop by breaking, not returning: the caller then sees exactly
				// the result a full sweep would have produced (bestResp if one
				// was collected, otherwise the aggregated ErrUpstreamsExhausted),
				// just without the wasted round-trips.
				if err != nil && !common.IsRetryableTowardNetwork(err) {
					break
				}
			}

			// Check context after the loop — handles single-upstream case where
			// the loop cap is reached before a new iteration can check ctx.
			if ctxErr := execSpanCtx.Err(); ctxErr != nil {
				cause := context.Cause(execSpanCtx)
				if cause == nil {
					cause = ctxErr
				}
				return nil, cause
			}

			// Fallback escape: once the routed upstreams are exhausted without
			// a usable result, sweep the untried fallback tier once per
			// request. Errors non-retryable toward the network do not
			// escalate, and consensus keeps its fixed participant set.
			if (bestResp == nil || (isEmptyNetworkResult(method, bestResp) && emptyIsMiss(execSpanCtx))) &&
				lastErr != nil && common.IsRetryableTowardNetwork(lastErr) &&
				n.cfg.Failover.Enabled() && !failsafeExecutor.HasConsensus() {
				// A use-upstream pin narrows the escape exactly as it narrows
				// NextUpstream.
				var selector string
				if d := effectiveReq.Directives(); d != nil {
					selector = d.UseUpstream
				}
				var fallbacks []common.Upstream
				for _, fb := range n.upstreamsRegistry.GetFallbackEscapeUpstreams(execSpanCtx, n.networkId, method, effectiveReq.Finality(execSpanCtx)) {
					if selector != "" {
						if matches, _ := common.UpstreamMatchesSelector(selector, fb); !matches {
							continue
						}
					}
					if _, seen := attempted[fb.Id()]; !seen {
						fallbacks = append(fallbacks, fb)
					}
				}
				if len(fallbacks) > 0 && effectiveReq.MarkEscalatedToFallbacks() {
					// Keep bestResp: if every fallback fails, the routed
					// upstreams' empty answer is still the result.
					lastErr = nil
					maxLoopIterations = len(fallbacks)
					nextUpstream = func() (common.Upstream, error) {
						for len(fallbacks) > 0 {
							fb := fallbacks[0]
							fallbacks = fallbacks[1:]
							// Skip one a sibling hedge is already running.
							if _, loaded := effectiveReq.ConsumedUpstreams.LoadOrStore(fb, true); !loaded {
								return fb, nil
							}
						}
						return nil, common.NewErrNoUpstreamsLeftToSelect(effectiveReq, "no more fallback upstreams left")
					}

					telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues(
						n.projectId, n.Label(), method,
					).Inc()
					lg.Debug().Int("fallbacks", maxLoopIterations).Msg("escalating to fallback upstreams")

					continue escalationLoop
				}
			}

			break escalationLoop
		}

		// All upstreams tried. Return the best result for the retry/hedge
		// wrapper to evaluate. Prefer a valid response over an error so
		// the delay function can detect empty results and apply
		// emptyResultDelay.
		if bestResp != nil {
			st := effectiveReq.ExecState()
			st.MarkUpstreamAttemptWon(bestResp.UpstreamId())
			s := st.Snapshot()
			bestResp.SetAttempts(s.Attempts)
			bestResp.SetRetries(s.Retries)
			bestResp.SetHedges(s.Hedges)
			return bestResp, nil
		}

		// For consensus, return the raw upstream error so the consensus
		// policy receives the actual error type (e.g. server error, missing
		// data) rather than a wrapped ErrUpstreamsExhausted.
		if oneUpstreamOnly && lastErr != nil {
			return nil, lastErr
		}

		s := effectiveReq.ExecState().Snapshot()
		exhaustedErr := common.NewErrUpstreamsExhausted(
			effectiveReq,
			&effectiveReq.ErrorsByUpstream,
			n.projectId,
			n.networkId,
			method,
			time.Since(startTime),
			s.Attempts,
			s.Retries,
			s.Hedges,
			len(upsList),
		)
		common.SetTraceSpanError(execSpan, exhaustedErr)
		return nil, exhaustedErr
	}

	// Two entry points into sweepFn:
	//   tryOneUpstream — single-upstream variant for consensus slots
	//   runUpstreamSweep — multi-upstream variant for non-consensus path
	tryOneUpstream := func(c context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return sweepFn(c, r, true)
	}
	runUpstreamSweep := func(c context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return sweepFn(c, r, false)
	}

	resp, execErr := failsafeExecutor.Run(ectx, req, tryOneUpstream, runUpstreamSweep)

	req.RLockWithTrace(ctx)
	defer req.RUnlock()

	if execErr != nil {
		// When the lifecycle ctx fires, the network executor may return plain
		// context.DeadlineExceeded with no sentinel in the Unwrap chain.
		// Substitute only when the ctx cause is OUR sentinel.
		if _, ok := execErr.(common.StandardError); !ok && errors.Is(execErr, context.DeadlineExceeded) {
			if cause := context.Cause(ectx); errors.Is(cause, common.ErrDynamicTimeoutExceeded) {
				execErr = cause
			}
		}
		// Timeout-attribution metric: emit only when this scope's policy
		// fired the timeout and the error is not retry-exhausted.
		if failsafeExecutor.HasTimeout() &&
			!common.HasErrorCode(execErr, common.ErrCodeFailsafeRetryExceeded) &&
			errors.Is(execErr, common.ErrDynamicTimeoutExceeded) &&
			!common.HasErrorCode(execErr, common.ErrCodeFailsafeTimeoutExceeded) {
			finality := req.Finality(ctx)
			telemetry.MetricNetworkTimeoutFiredTotal.WithLabelValues(
				n.projectId,
				req.NetworkLabel(),
				method,
				finality.String(),
				string(common.ScopeNetwork),
			).Inc()
		}
		// Wrap bare timeout sentinel as a typed error for downstream callers.
		translatedErr := execErr
		if _, ok := translatedErr.(common.StandardError); !ok && errors.Is(translatedErr, common.ErrDynamicTimeoutExceeded) {
			translatedErr = common.NewErrFailsafeTimeoutExceeded(common.ScopeNetwork, translatedErr, &startTime)
		}
		// Don't override consensus results with last valid response from individual upstreams
		// For example if 1 upstream gives empty response another 3 give "reverted" error,
		// we should still return reverted error, even though there was an empty response before.
		if failsafeExecutor.HasConsensus() {
			if mlx != nil {
				mlx.Close(ctx, nil, translatedErr)
			}
			// LVR is a borrowed pointer — consensus executor owns releasing all participant
			// responses (winners and losers). Just drop our reference to avoid dangling pointer.
			req.ClearLastValidResponse()
			n.countRefusal(ctx, req, nil, translatedErr)
			return nil, translatedErr
		}

		lvr := req.LastValidResponse()
		if lvr != nil && !lvr.IsObjectNull() {
			// A valid response is a json-rpc response without "error" object.
			// This mechanism is needed in these two scenarios:
			//
			// 1) If error is due to empty response be generous and accept it,
			// because this means after many retries or exhausting all upstreams still no data is available.
			// We don't need to worry about wrongly replying empty responses for unfinalized data
			// because cache layer has mechanism to deal with empty and/or unfinalized data.
			//
			// 2) For pending txs we can accept the response, if after retries it is still pending.
			// This avoids failing with "retry" error, when we actually do have a response but blockNumber is null since tx is pending.
			resp = lvr
			req.SetLastUpstream(resp.Upstream())
		} else {
			if mlx != nil {
				mlx.Close(ctx, nil, translatedErr)
			}
			n.countRefusal(ctx, req, nil, translatedErr)
			return nil, translatedErr
		}
	}

	if resp != nil {
		if n.cacheDal != nil {
			n.storeInCache(ctx, forwardSpan.SpanContext(), &lg, method, req, resp)
		}

		// Per-request execution counters + full upstream-attempt trace.
		// req.ExecState().Apply emits the standard execution.* attrs
		// AND the upstreams.* slices (tried, outcomes, reasons,
		// durations) so traces answer "who, what, why" without
		// enumerating child spans.
		req.ExecState().Apply(forwardSpan)
	}

	isEmpty := resp == nil || resp.IsObjectNull(ctx) || resp.IsResultEmptyish(ctx)
	forwardSpan.SetAttributes(attribute.Bool("response.emptyish", isEmpty))
	if isEmpty {
		lg.Trace().Msgf("response is empty")
	}

	if execErr == nil && !isEmpty {
		n.enrichStatePoller(ctx, method, req, resp)

		// Extract block number from successful response for block availability bounds check below.
		var respBlockNumber int64
		if n.cfg.Architecture == common.ArchitectureEvm {
			if _, bn, err := evm.ExtractBlockReferenceFromResponse(ctx, resp); err == nil && bn > 0 {
				respBlockNumber = bn
			}
		}

		// If response is not empty, but at least one upstream responded empty we track in a metric.
		// Derived from ErrorsByUpstream entries with ErrEndpointMissingData code.
		req.ErrorsByUpstream.Range(func(key, value any) bool {
			upstreamErr, ok := value.(error)
			if !ok || !common.HasErrorCode(upstreamErr, common.ErrCodeEndpointMissingData) {
				return true
			}
			// SVM history sweep uses MissingData to continue a sweep after an
			// incomplete 200 — not evidence this upstream disagreed with the winner.
			if common.IsHistorySweepContinuation(upstreamErr) {
				return true
			}
			upstream := key.(*upstream.Upstream)
			finality := req.Finality(ctx)
			telemetry.CounterHandle(telemetry.MetricUpstreamWrongEmptyResponseTotal,
				n.projectId,
				upstream.VendorName(),
				n.Label(),
				upstream.Id(),
				method,
				finality.String(),
				req.UserId(),
				req.AgentName()).Inc()

			// If the response block number is known, check if it falls outside
			// this upstream's configured block availability range.
			// If so, the empty response was expected — skip misbehavior recording.
			if respBlockNumber > 0 {
				minBound, maxBound := upstream.EvmBlockAvailabilityBounds()
				if (minBound != math.MinInt64 && respBlockNumber < minBound) ||
					(maxBound != math.MaxInt64 && respBlockNumber > maxBound) {
					return true
				}
			}

			// Wrong-empty is a misbehavior (data disagreement with other upstreams),
			// not an error. The upstream responded correctly, it just lacked data
			// that others had. Only record misbehavior, not failure. `finality` was
			// resolved a few lines up for the wrong-empty telemetry metric — reuse it
			// here so per-(method, finality) misbehavior counters stratify
			// consistently with the rest of the tracker writes.
			if upstream != nil {
				if mt := upstream.MetricsTracker(); mt != nil {
					mt.RecordUpstreamMisbehavior(upstream, method, finality)
				}
			}
			return true
		})
	}

	if mlx != nil {
		mlx.Close(ctx, resp, nil)
	}

	// LVR is a borrowed pointer — consensus executor owns releasing non-winner responses.
	// Just drop our reference so it doesn't outlive the response lifecycle.
	if failsafeExecutor.HasConsensus() {
		req.ClearLastValidResponse()
	}

	n.countRefusal(ctx, req, resp, nil)
	return resp, nil
}

// storeInCache writes resp to the cache in the background; the cache
// policies decide by the request's finality what is kept and for how long.
func (n *Network) storeInCache(ctx context.Context, spanCtx trace.SpanContext, lg *zerolog.Logger, method string, req *common.NormalizedRequest, resp *common.NormalizedResponse) {
	// Force-materialize jrr so the goroutine reads only via atomic pointer (no locks needed).
	// TODO For other architectures we might need a different approach
	_, _ = resp.JsonRpcResponse(ctx)
	resp.AddRef()

	go func() {
		defer (func() {
			if rec := recover(); rec != nil {
				telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
					"cache-set",
					fmt.Sprintf("network:%s method:%s", n.networkId, method),
					common.ErrorFingerprint(rec),
				).Inc()
				lg.Error().
					Interface("panic", rec).
					Str("stack", string(debug.Stack())).
					Msgf("unexpected panic on cache-set")
			}
		})()
		defer resp.DoneRef()

		timeoutCtx, timeoutCtxCancel := context.WithTimeoutCause(n.appCtx, 10*time.Second, errors.New("cache driver timeout during set"))
		defer timeoutCtxCancel()
		tracedCtx := trace.ContextWithSpanContext(timeoutCtx, spanCtx)
		if err := n.cacheDal.Set(tracedCtx, req, resp); err != nil {
			lg.Warn().Err(err).Msgf("could not store response in cache")
		}
	}()
}

func (n *Network) prepareRequest(ctx context.Context, nr *common.NormalizedRequest) error {
	switch n.Architecture() {
	case common.ArchitectureEvm:
		jsonRpcReq, err := nr.JsonRpcRequest(ctx)
		if err != nil {
			return common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorParseException,
				"failed to unmarshal json-rpc request",
				err,
				nil,
			)
		}
		evm.NormalizeHttpJsonRpc(ctx, nr, jsonRpcReq)
	case common.ArchitectureSvm:
		// SVM doesn't need any EVM-style normalization (hex padding, block tag expansion, etc.).
		// Validate that the request parses as JSON-RPC and move on.
		if _, err := nr.JsonRpcRequest(ctx); err != nil {
			return common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorParseException,
				"failed to unmarshal json-rpc request",
				err,
				nil,
			)
		}
	default:
		return common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorServerSideException,
			fmt.Sprintf("unsupported architecture: %s for network: %s", n.Architecture(), n.Id()),
			nil,
			nil,
		)
	}

	return nil
}

func (n *Network) GetMethodMetrics(method string) common.TrackedMetrics {
	if method == "" {
		return nil
	}

	mt := n.metricsTracker.GetNetworkMethodMetrics(n.networkId, method)
	return mt
}

func (n *Network) Config() *common.NetworkConfig {
	return n.cfg
}

func (n *Network) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	ctx, span := common.StartDetailSpan(ctx, "Network.GetFinality")
	defer span.End()

	finality := common.DataFinalityStateUnknown

	if req == nil && resp == nil {
		return finality
	}

	method, _ := req.Method()
	if cfg := n.cfg.Methods.FindMethodConfig(method); cfg != nil {
		if cfg.Finalized {
			finality = common.DataFinalityStateFinalized
			return finality
		} else if cfg.Realtime {
			finality = common.DataFinalityStateRealtime
			return finality
		}
	}

	// Architecture-specific finality resolution. SVM uses commitment + slot comparisons,
	// EVM uses block-number comparisons against the state poller's finalized block.
	// Keeping this switch here (rather than inside the ArchitectureHandler) avoids
	// plumbing Network's internal upstreams registry through the generic interface.
	if n.cfg.Architecture == common.ArchitectureSvm {
		return svm.GetFinality(ctx, n, req, resp)
	}

	blockRef, blockNumber, _ := evm.ExtractBlockReferenceFromRequest(ctx, req)

	// When the request alone doesn't carry a block number (e.g. tx-hash lookups
	// like eth_getTransactionReceipt, or block-hash lookups like eth_getBlockByHash),
	// try to extract it from the response body. This is critical for cache hits where
	// the response contains blockNumber but the request only has a hash.
	if blockNumber == 0 && resp != nil {
		if _, respBlockNumber, err := evm.ExtractBlockReferenceFromResponse(ctx, resp); err == nil && respBlockNumber > 0 {
			blockNumber = respBlockNumber
		}
	}

	// If we still have no block number and the request is a wildcard/hash-only ref
	// with no way to determine finality, return unknown early.
	if blockNumber == 0 && (blockRef == "" || blockRef == "*") {
		return finality // unknown
	}

	if blockRef != "" && blockRef != "*" && (blockRef[0] < '0' || blockRef[0] > '9') {
		finality = common.DataFinalityStateRealtime
		return finality
	}

	if blockNumber > 0 {
		// Try response's upstream first (direct upstream responses)
		if resp != nil {
			upstream := resp.Upstream()
			if upstream != nil {
				if ups, ok := upstream.(common.EvmUpstream); ok {
					if isFinalized, err := ups.EvmIsBlockFinalized(ctx, blockNumber, false); err == nil {
						if isFinalized {
							finality = common.DataFinalityStateFinalized
						} else {
							finality = common.DataFinalityStateUnfinalized
						}
						return finality
					}
				}
			}
		}

		// Try LastUpstream from the request (retries or subsequent attempts)
		if upstream := req.LastUpstream(); upstream != nil {
			if ups, ok := upstream.(common.EvmUpstream); ok {
				if isFinalized, err := ups.EvmIsBlockFinalized(ctx, blockNumber, false); err == nil {
					if isFinalized {
						finality = common.DataFinalityStateFinalized
					} else {
						finality = common.DataFinalityStateUnfinalized
					}
					return finality
				}
			}
		}

		// Fallback: use the network's lowest finalized block as a heuristic.
		// This is the primary path for cache hits where no upstream is available.
		if n.upstreamsRegistry != nil {
			lowestFinalized := n.EvmLowestFinalizedBlockNumber(ctx)
			if lowestFinalized > 0 {
				if blockNumber <= lowestFinalized {
					finality = common.DataFinalityStateFinalized
				} else {
					finality = common.DataFinalityStateUnfinalized
				}
				span.SetAttributes(
					attribute.Bool("used_network_finalized_heuristic", true),
					attribute.Int64("lowest_finalized", lowestFinalized),
				)
			}
		}
		// If we still can't determine, it remains unknown
	}

	return finality
}

func (n *Network) doForward(execSpanCtx context.Context, u common.Upstream, req *common.NormalizedRequest, skipCacheRead, isHedgeAttempt bool) (*common.NormalizedResponse, error) {
	// evm.pinLatestTag: this upstream is sent the network head in place of
	// "latest"; the request keeps "latest" for the cache, the response and every
	// other upstream. With the head unknown it is never sent "latest" at all.
	if params, height, pinned := evm.PinLatestTag(execSpanCtx, n, u, req); pinned {
		if height <= 0 {
			var latest, finalized int64
			if eu, ok := u.(common.EvmUpstream); ok {
				latest, finalized = n.upstreamHeads(eu)
			}
			return nil, common.NewErrUpstreamBlockUnavailable(u.Id(), 0, latest, finalized)
		}
		execSpanCtx = common.WithWireParams(execSpanCtx, req, params)
	}

	h := n.architectureHandler
	if h != nil {
		if handled, resp, err := h.HandleUpstreamPreForward(execSpanCtx, n, u, req, skipCacheRead); handled {
			return h.HandleUpstreamPostForward(execSpanCtx, n, u, req, resp, err, skipCacheRead)
		}
	}

	// If not handled, then fallback to the normal forward
	resp, err := u.Forward(execSpanCtx, req, false, isHedgeAttempt)
	if h != nil {
		return h.HandleUpstreamPostForward(execSpanCtx, n, u, req, resp, err, skipCacheRead)
	}
	return resp, err
}

// methodHasDedicatedRangeAvailabilityHook reports whether a method enforces its own
// block availability via a dedicated pre-forward hook (CheckBlockRangeAvailability),
// rather than the single-block path in checkUpstreamBlockAvailability. These are
// range methods (fromBlock/toBlock) where gating on a single extracted block number
// would be wrong and would conflict with the range-aware hook.
func methodHasDedicatedRangeAvailabilityHook(method string) bool {
	switch strings.ToLower(method) {
	case "eth_getlogs", "trace_filter", "arbtrace_filter":
		return true
	default:
		return false
	}
}

// systemDefaultEnforceBlockAvailability returns the global system default for
// EnforceBlockAvailability for a given method, or nil if no default is set.
func systemDefaultEnforceBlockAvailability(method string) *bool {
	if dmc := common.DefaultWithBlockMethodConfig(method); dmc != nil {
		return dmc.EnforceBlockAvailability
	}
	return nil
}

// blockAvailabilityExplicitlyDisabled reports whether the user explicitly turned
// OFF block availability enforcement for this method, via a method-level or
// network-level enforceBlockAvailability:false. It is the escape hatch that lets an
// operator disable bound enforcement even on an upstream that has bounds configured.
//
// The method-level value is only treated as an override when it differs from the
// system default, because MethodsConfig.SetDefaults() merges DefaultWithBlockCacheMethods
// into Methods.Definitions at config load — so a value that matches the merged-in
// default is not a real user override.
func (n *Network) blockAvailabilityExplicitlyDisabled(method string) bool {
	sysDefault := systemDefaultEnforceBlockAvailability(method)
	if n.cfg != nil {
		if mc := n.cfg.Methods.FindMethodConfig(method); mc != nil && mc.EnforceBlockAvailability != nil {
			if sysDefault == nil || *mc.EnforceBlockAvailability != *sysDefault {
				return !*mc.EnforceBlockAvailability
			}
		}
	}
	if n.cfg != nil && n.cfg.Evm != nil && n.cfg.Evm.EnforceBlockAvailability != nil {
		return !*n.cfg.Evm.EnforceBlockAvailability
	}
	return false
}

// upstreamHeads returns the upstream's last-known latest and finalized block numbers,
// or (0, 0) when the state poller is unavailable.
func (n *Network) upstreamHeads(eu common.EvmUpstream) (latest int64, finalized int64) {
	if sp := eu.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
		return sp.LatestBlock(), sp.FinalizedBlock()
	}
	return 0, 0
}

// handleBlockSkip records telemetry when a block availability check causes an upstream to be skipped,
// and triggers an async state poller refresh for retryable cases so subsequent retries see fresh block numbers.
func (n *Network) handleBlockSkip(
	ctx context.Context,
	span trace.Span,
	ulg *zerolog.Logger,
	u common.Upstream,
	req *common.NormalizedRequest,
	method string,
	skipErr error,
	isRetryable bool,
) {
	ulg.Debug().Err(skipErr).Bool("retryable", isRetryable).Msg("skipping upstream due to block availability gating")
	span.SetAttributes(
		attribute.Bool("skipped", true),
		attribute.String("skip_reason", skipErr.Error()),
		attribute.Bool("skip_retryable", isRetryable),
	)
	finality := req.Finality(ctx)
	telemetry.CounterHandle(telemetry.MetricUpstreamErrorTotal,
		n.projectId, u.VendorName(), n.Label(), u.Id(), method,
		common.ErrorFingerprint(skipErr), string(common.SeverityInfo),
		req.CompositeType(), finality.String(),
		req.UserId(), req.AgentName()).Inc()
	errToStore := skipErr
	if !isRetryable {
		errToStore = common.NewErrUpstreamRequestSkipped(skipErr, u.Id())
	}
	req.MarkUpstreamCompleted(ctx, u, nil, errToStore)

	// When the block is slightly ahead (retryable), trigger an async poll so the
	// state poller fetches the latest block number before the next retry fires.
	// Without this the retry loop sleeps for blockUnavailableDelay but the cached
	// latestBlock stays stale until the background ticker fires (often 10s+).
	// PollLatestBlockNumber respects its own debounce interval so concurrent
	// triggers from multiple upstreams/requests are coalesced safely.
	if isRetryable {
		if eu, ok := u.(common.EvmUpstream); ok {
			if sp := eu.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
				go func() { // #nosec G118 -- fire-and-forget poll; must not share request lifetime
					pollCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_, _ = sp.PollLatestBlockNumber(pollCtx)
				}()
			}
		}
	}
}

// recordHedgeDiscard handles telemetry when a hedged request is discarded because another hedge won.
func (n *Network) recordHedgeDiscard(
	ctx context.Context,
	span trace.Span,
	ulg *zerolog.Logger,
	u common.Upstream,
	req *common.NormalizedRequest,
	method string,
	err error,
	attempts int,
	hedges int,
) {
	ulg.Debug().Err(err).Msgf("discarding hedged request to upstream")
	finality := req.Finality(ctx)
	telemetry.CounterHandle(telemetry.MetricNetworkHedgeDiscardsTotal,
		n.projectId, n.Label(), u.Id(), method, fmt.Sprintf("%d", attempts),
		fmt.Sprintf("%d", hedges), finality.String(),
		req.UserId(), req.AgentName(),
	).Inc()
	common.SetTraceSpanError(span, common.NewErrUpstreamHedgeCancelled(u.Id(), err))
}

func (n *Network) checkUpstreamBlockAvailability(ctx context.Context, u common.Upstream, req *common.NormalizedRequest, method string) (error, bool) {
	switch n.cfg.Architecture {
	case common.ArchitectureEvm:
		return n.checkEvmUpstreamBlockAvailability(ctx, u, req, method)
	case common.ArchitectureSvm:
		return n.checkSvmUpstreamSlotAvailability(ctx, u, req, method)
	}
	return nil, false
}

// checkSvmUpstreamSlotAvailability skips an upstream whose OPERATOR-DECLARED
// svm.blockAvailability window excludes the slot the request literally names
// (the END of the range for getBlocks/getBlocksWithLimit on the upper side).
//
// Mirrors the EVM flow: availability is what the operator declares. An observed
// floor takes part only when the operator opts in through
// firstAvailableSlotPlus, resolved inside CheckUpstreamDeclaredSlotBounds.
// Without a declared window every upstream is tried.
//
// Gated on SvmEnforceBlockAvailability so the lower and upper SVM bounds share
// ONE operator switch.
//
// Declared-upper skips pass their retryable flag through to handleBlockSkip
// (whose async poller-kick is already EVM-gated, so SVM retryable only
// affects skip bookkeeping).
func (n *Network) checkSvmUpstreamSlotAvailability(ctx context.Context, u common.Upstream, req *common.NormalizedRequest, method string) (error, bool) {
	if !n.SvmEnforceBlockAvailability() {
		return nil, false
	}
	skipErr, retryable := svm.CheckUpstreamDeclaredSlotBounds(ctx, u, req)
	if skipErr == nil {
		return nil, false
	}
	if svm.IsDeclaredUpperBoundSkip(skipErr) {
		telemetry.MetricUpstreamStaleUpperBound.WithLabelValues(
			n.projectId, u.VendorName(), n.Label(), u.Id(), method, svm.DeclaredConfidenceLabel,
		).Inc()
	} else {
		telemetry.MetricUpstreamStaleLowerBound.WithLabelValues(
			n.projectId, u.VendorName(), n.Label(), u.Id(), method, svm.DeclaredConfidenceLabel,
		).Inc()
	}
	return skipErr, retryable
}

// checkUpstreamBlockAvailability performs per-upstream gating for the request based on
// the upstream's configured block availability bounds. It is invoked just before
// forwarding to an upstream to avoid copying/filtering the list.
//
// Returns (nil, false) if the upstream is allowed to serve the block.
// Returns (error, isRetryable) if the block is outside the configured range:
//   - below the lower bound (pruned/historical) → not retryable
//   - above the upper bound AND ahead of the live head within MaxRetryableBlockDistance → retryable
//   - above the upper bound otherwise → not retryable
//
// SCOPE: this enforces ONLY the upstream's structural serving range — the configured
// blockAvailability lower/upper bounds (which also cover the legacy
// maxAvailableRecentBlocks via EvmBlockAvailabilityBounds). It deliberately does NOT
// apply any implicit "blockNumber > latestBlock" head check. Tip-awareness — i.e.
// "is this block past the chain head?" — is owned elsewhere:
//   - served-tip short-circuits concrete future blocks beyond the network tip, and
//   - integrity.enforceHighestBlock keeps "latest"/"finalized" tag responses honest.
//
// Coupling an implicit head check to this path raced with the state poller on fast
// block-time chains and falsely rejected fresh blocks an upstream already had
// (https://github.com/erpc/erpc/issues/934). Operators who want a node to serve only
// blocks it has actually synced can express that explicitly as
// blockAvailability.upper.latestBlockMinus: 0.
//
// FAIL-OPEN BEHAVIOR: If we cannot determine the bounds or block number (e.g. state
// poller not ready), we allow the request to proceed rather than blocking traffic.
func (n *Network) checkEvmUpstreamBlockAvailability(ctx context.Context, u common.Upstream, req *common.NormalizedRequest, method string) (error, bool) {
	// Range methods (eth_getLogs, trace_filter, arbtrace_filter) enforce block
	// availability via their own range-aware pre-forward hooks; gating them here on a
	// single extracted block number would conflict with that.
	if methodHasDedicatedRangeAvailabilityHook(method) {
		return nil, false
	}
	// Explicit method/network enforceBlockAvailability:false is an operator opt-out.
	if n.blockAvailabilityExplicitlyDisabled(method) {
		return nil, false
	}
	eu, ok := u.(common.EvmUpstream)
	if !ok {
		return nil, false
	}
	// Resolve the upstream's configured serving range (explicit blockAvailability
	// lower/upper, or the legacy maxAvailableRecentBlocks lower bound). Unbounded on
	// both sides means the upstream advertises no restriction → nothing to enforce.
	minBound, maxBound := eu.EvmBlockAvailabilityBounds()
	if minBound == math.MinInt64 && maxBound == math.MaxInt64 {
		return nil, false
	}

	// An upstream that pins "latest" (evm.pinLatestTag) is sent the network head,
	// so that head is the block it must hold. Otherwise prefer the cached block
	// number from normalization, falling back to extracting from the request
	// (defensive: handles paths that bypass json_rpc.go's normalization, and
	// methods whose params haven't been pre-cached yet).
	var bn int64
	if _, height, pinned := evm.PinLatestTag(ctx, n, u, req); pinned {
		bn = height
	} else if v := req.EvmBlockNumber(); v != nil {
		if n64, ok := v.(int64); ok {
			bn = n64
		}
	}
	if bn <= 0 {
		if _, x, ebn := evm.ExtractBlockReferenceFromRequest(ctx, req); ebn == nil && x > 0 {
			bn = x
		}
	}
	if bn <= 0 {
		// If still unknown, skip gating (fail-open)
		return nil, false
	}

	// Below the configured lower bound: the upstream genuinely cannot serve this
	// historical block (e.g. pruned). Not retryable on this upstream.
	if minBound != math.MinInt64 && bn < minBound {
		telemetry.MetricUpstreamStaleLowerBound.WithLabelValues(
			n.projectId, eu.VendorName(), n.Label(), eu.Id(), method, common.AvailbilityConfidenceBlockHead.String(),
		).Inc()
		latestBlock, finalizedBlock := n.upstreamHeads(eu)
		return common.NewErrUpstreamBlockUnavailable(eu.Id(), bn, latestBlock, finalizedBlock), false
	}

	// Above the configured upper bound. Classify retryability by distance to the live
	// head: a block just ahead of the head (e.g. upper=latestBlockMinus:0) may become
	// serveable shortly, so it is retryable within MaxRetryableBlockDistance.
	if maxBound != math.MaxInt64 && bn > maxBound {
		telemetry.MetricUpstreamStaleUpperBound.WithLabelValues(
			n.projectId, eu.VendorName(), n.Label(), eu.Id(), method, common.AvailbilityConfidenceBlockHead.String(),
		).Inc()
		latestBlock, finalizedBlock := n.upstreamHeads(eu)
		blockErr := common.NewErrUpstreamBlockUnavailable(eu.Id(), bn, latestBlock, finalizedBlock)
		if bn > latestBlock && latestBlock > 0 {
			return blockErr, bn-latestBlock <= n.maxRetryableBlockDistance()
		}
		return blockErr, false
	}

	return nil, false
}

// eligibleUpstreamIDsForBoundary derives the block-availability "lane" for a
// single-block request: the IDs of upstreams whose configured availability
// bounds can serve the request's block. It feeds the selection policy's
// per-boundary axis (EvalPerBoundary) so the policy evaluates against — and
// keeps health/exclusion state for — only the upstreams that can actually
// answer this block.
//
// It returns nil ("no lane scoping → use the full-pool wildcard slot") when:
//   - the network isn't EVM, or the method has a dedicated range-availability
//     hook (eth_getLogs / trace_filter, gated by CheckBlockRangeAvailability),
//   - the block number can't be resolved (e.g. eth_getBlockByHash carries a
//     hash, not a number) — same fail-open as checkUpstreamBlockAvailability,
//   - no upstream advertises any bound (a single implicit lane = everything), or
//   - every upstream is eligible (the lane equals the full pool, so the caller
//     reuses the wildcard slot instead of spawning an identical duplicate).
//
// The eligibility predicate mirrors checkUpstreamBlockAvailability's use of
// EvmBlockAvailabilityBounds so the lane membership and the per-upstream
// availability gate never disagree about which upstreams can serve a block.
func (n *Network) eligibleUpstreamIDsForBoundary(ctx context.Context, method string, req *common.NormalizedRequest) []string {
	if n.cfg == nil || n.cfg.Architecture != common.ArchitectureEvm {
		return nil
	}
	if methodHasDedicatedRangeAvailabilityHook(method) {
		return nil
	}
	// Resolve the request's block number (cached at normalization, with a
	// defensive extraction fallback) — mirrors checkUpstreamBlockAvailability.
	var bn int64
	if v := req.EvmBlockNumber(); v != nil {
		if n64, ok := v.(int64); ok {
			bn = n64
		}
	}
	if bn <= 0 {
		if _, x, ebn := evm.ExtractBlockReferenceFromRequest(ctx, req); ebn == nil && x > 0 {
			bn = x
		}
	}
	if bn <= 0 {
		return nil
	}

	ups := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
	if len(ups) == 0 {
		return nil
	}
	bounds := make([]upstreamBlockBounds, len(ups))
	for i, u := range ups {
		// EvmBlockAvailabilityBounds returns (MinInt64, MaxInt64) — i.e.
		// unbounded, hence always-eligible — for non-EVM or boundless
		// upstreams, so no special-casing is needed here.
		minBound, maxBound := u.EvmBlockAvailabilityBounds()
		bounds[i] = upstreamBlockBounds{id: u.Id(), min: minBound, max: maxBound}
	}
	return eligibleLane(bounds, bn)
}

// upstreamBlockBounds is an upstream's resolved [min,max] availability range.
type upstreamBlockBounds struct {
	id       string
	min, max int64
}

// eligibleLane returns the IDs of upstreams whose [min,max] range covers block
// bn — the block-availability "lane" for that block. It returns nil ("no lane
// scoping") when no upstream advertises any bound (a single implicit lane) or
// when every upstream is eligible (the lane equals the full pool, so callers
// reuse the wildcard slot rather than spawning a redundant duplicate). Split
// out from eligibleUpstreamIDsForBoundary so the lane decision is unit-testable
// without a live upstream registry.
func eligibleLane(bounds []upstreamBlockBounds, bn int64) []string {
	eligible := make([]string, 0, len(bounds))
	anyBounded := false
	for _, b := range bounds {
		if b.min != math.MinInt64 || b.max != math.MaxInt64 {
			anyBounded = true
		}
		if (b.min == math.MinInt64 || bn >= b.min) && (b.max == math.MaxInt64 || bn <= b.max) {
			eligible = append(eligible, b.id)
		}
	}
	// No bounds anywhere, or every upstream eligible → a single lane that
	// equals the full pool. Returning nil routes to the wildcard slot and
	// avoids spawning a redundant lane identical to it.
	if !anyBounded || len(eligible) == len(bounds) {
		return nil
	}
	return eligible
}

// multiplexKey derives the in-flight dedup identity for a request.
//
// EVM keeps req.CacheHash() byte-for-byte. SVM must not: CacheHash lowercases
// every string param — correct normalization for EVM hex, but it collapses
// case-sensitive base58, so two DISTINCT Solana accounts or signatures
// differing only by letter case would share one multiplexer and the follower
// would be served the leader's data. This is the same hazard that made the SVM
// cache derive its own key (architecture/svm.RequestKey); multiplexing needs
// the identical guarantee because it hands the leader's response to followers
// verbatim.
func (n *Network) multiplexKey(ctx context.Context, req *common.NormalizedRequest) (string, error) {
	if n.cfg != nil && n.cfg.Architecture == common.ArchitectureSvm {
		return svm.RequestKey(ctx, req)
	}
	return req.CacheHash()
}

func (n *Network) handleMultiplexing(ctx context.Context, lg *zerolog.Logger, req *common.NormalizedRequest, startTime time.Time) (*Multiplexer, *common.NormalizedResponse, error) {
	if !n.cfg.MultiplexingEnabled() {
		return nil, nil, nil
	}

	// A non-retryable write (e.g. creating a filter) must reach an upstream
	// once per call, so it is never shared with a concurrent identical call.
	if method, _ := req.Method(); evm.IsNonRetryableWriteMethod(method) || svm.IsNonRetryableWriteMethod(method) {
		return nil, nil, nil
	}

	mlxHash, err := n.multiplexKey(ctx, req)
	lg.Trace().Str("hash", mlxHash).Object("request", req).Msgf("checking if multiplexing is possible")
	if err != nil || mlxHash == "" {
		lg.Debug().Str("hash", mlxHash).Err(err).Object("request", req).Msgf("could not get multiplexing hash for request")
		return nil, nil, nil
	}

	// Try to atomically register as the leader or become a follower.
	// Loop handles the edge case where we find a closed multiplexer during cleanup.
	for {
		mlx := NewMultiplexer(mlxHash)
		vinf, loaded := n.inFlightRequests.LoadOrStore(mlxHash, mlx)

		if !loaded {
			// Our multiplexer was stored, we're the leader
			return mlx, nil, nil
		}

		// Another request is already in flight - try to be a follower
		inf := vinf.(*Multiplexer)

		// Try to register as follower under lock to prevent race with cleanup's Wait()
		inf.mu.Lock()
		if inf.closed {
			// Leader already finished and cleanup started, can't be a follower.
			// Retry LoadOrStore - the old entry should be deleted soon, allowing
			// us to properly become the new leader (with our mlx stored in the map).
			inf.mu.Unlock()
			continue
		}

		inf.copyWg.Add(1)
		inf.mu.Unlock()

		method, _ := req.Method()
		finality := req.Finality(ctx)
		telemetry.CounterHandle(telemetry.MetricNetworkMultiplexedRequests,
			n.projectId, n.Label(), method, finality.String(), req.UserId(), req.AgentName(),
		).Inc()

		lg.Debug().Str("hash", mlxHash).Msgf("found identical request initiating multiplexer")

		if span := trace.SpanFromContext(ctx); span.IsRecording() {
			span.SetAttributes(attribute.String("multiplexer.hash", mlxHash))
		}

		resp, err := n.waitForMultiplexResult(ctx, inf, req, startTime)

		// Done() is called in waitForMultiplexResult via defer
		lg.Trace().Str("hash", mlxHash).Object("response", resp).Err(err).Msgf("multiplexed request result")

		if err != nil {
			return nil, nil, err
		}
		if resp != nil {
			return nil, resp, nil
		}

		// A leader that shares only a usable answer (a chain tracker fetch)
		// closed without one: lead or join anew, with this request's own
		// failsafe policies.
		lg.Debug().Str("hash", mlxHash).Msg("multiplexer leader shared no response, forwarding anew")
	}
}

// leadMultiplexer registers req as the leader of its in-flight key so
// concurrent identical requests join it. It returns nil when multiplexing is
// off or another request already leads the key; it never follows one.
func (n *Network) leadMultiplexer(ctx context.Context, req *common.NormalizedRequest) *Multiplexer {
	if !n.cfg.MultiplexingEnabled() {
		return nil
	}
	hash, err := n.multiplexKey(ctx, req)
	if err != nil || hash == "" {
		return nil
	}
	mlx := NewMultiplexer(hash)
	if _, loaded := n.inFlightRequests.LoadOrStore(hash, mlx); loaded {
		return nil
	}
	return mlx
}

func (n *Network) waitForMultiplexResult(ctx context.Context, mlx *Multiplexer, req *common.NormalizedRequest, startTime time.Time) (*common.NormalizedResponse, error) {
	ctx, span := common.StartSpan(ctx, "Network.WaitForMultiplexResult")
	defer span.End()
	span.SetAttributes(attribute.String("multiplexer.hash", mlx.hash))

	// Caller already registered with copyWg.Add(1), ensure we signal completion
	defer mlx.copyWg.Done()

	// Wait for leader to complete (or check if already done)
	select {
	case <-mlx.done:
		// Leader finished - copy the response
		// Lock protects against concurrent reads, cleanup waits for copyWg so response is valid
		mlx.mu.RLock()
		out, err := common.CopyResponseForRequest(ctx, mlx.resp, req)
		resultErr := mlx.err
		mlx.mu.RUnlock()

		if err != nil {
			return nil, err
		}
		return out, resultErr
	case <-ctx.Done():
		err := ctx.Err()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, common.NewErrNetworkRequestTimeout(time.Since(startTime), err)
		}
		return nil, err
	}
}

func (n *Network) cleanupMultiplexer(mlx *Multiplexer) {
	// Mark as closed under lock to prevent new followers from registering
	mlx.mu.Lock()
	mlx.closed = true
	mlx.mu.Unlock()

	// Remove from map so new requests create their own multiplexer
	n.inFlightRequests.Delete(mlx.hash)

	// Wait for all followers to finish copying before releasing.
	// Wait() is safe because closed=true prevents any new Add() calls.
	// This blocks the leader briefly, but the leader has already returned
	// its response to the caller, so this only delays cleanup.
	mlx.copyWg.Wait()

	// After Wait(), no followers are accessing the response.
	mlx.mu.Lock()
	if mlx.resp != nil {
		mlx.resp.Release()
		mlx.resp = nil
	}
	mlx.mu.Unlock()
}

func (n *Network) shouldHandleMethod(req *common.NormalizedRequest, method string, upsList []common.Upstream) error {
	// Check stateful methods policy
	// Methods.Definitions is guaranteed to be populated by SetDefaults() with all necessary stateful methods
	isStateful := false
	if n.cfg != nil {
		if mc := n.cfg.Methods.FindMethodConfig(method); mc != nil {
			isStateful = mc.Stateful
		}
	}
	if isStateful {
		// Determine targeted upstream count
		targeted := 0
		if dr := req.Directives(); dr != nil && dr.UseUpstream != "" {
			for _, u := range upsList {
				if match, _ := common.UpstreamMatchesSelector(dr.UseUpstream, u); match {
					targeted++
				}
			}
		} else {
			targeted = len(upsList)
		}
		if targeted > 1 {
			return common.NewErrNotImplemented("stateful method requires a single targeted upstream; either configure only 1 upstream or set Use-Upstream to a single upstream id")
		}
	}

	// Case-insensitive for the same reason the method-config lookup above is:
	// dispatch treats method names case-insensitively, so a non-canonical casing
	// must not slip past a guard that canonical casing is rejected by.
	if strings.EqualFold(method, "eth_accounts") || strings.EqualFold(method, "eth_sign") {
		return common.NewErrNotImplemented("eth_accounts and eth_sign are not supported")
	}

	return nil
}

// filterMethodEligible returns the upstreams expected to serve `method` per
// their ignoreMethods/allowMethods config (Upstream.ShouldHandleMethod, which
// is memoized per method), plus the number dropped. The input slice is never
// mutated — selection-policy slots hand out their cached backing array as
// read-only — and is returned as-is when nothing is dropped. Matcher errors
// fail open (the upstream stays in) so a malformed pattern degrades to the
// forward-time skip rather than silently shrinking the pool.
func filterMethodEligible(ups []common.Upstream, method string) ([]common.Upstream, int) {
	eligible := ups
	dropped := 0
	for i, u := range ups {
		ok, err := u.ShouldHandleMethod(method)
		if err == nil && !ok {
			if dropped == 0 {
				// First drop: materialize a fresh slice with the survivors so far.
				eligible = make([]common.Upstream, i, len(ups))
				copy(eligible, ups[:i])
			}
			dropped++
			continue
		}
		if dropped > 0 {
			eligible = append(eligible, u)
		}
	}
	return eligible, dropped
}

func (n *Network) enrichStatePoller(ctx context.Context, method string, req *common.NormalizedRequest, resp *common.NormalizedResponse) {
	ctx, span := common.StartDetailSpan(ctx, "Network.EnrichStatePoller")
	defer span.End()

	switch n.Architecture() {
	case common.ArchitectureEvm:
		// TODO Move the logic to evm package as a post-forward hook?
		if method == "eth_getBlockByNumber" {
			// Prefer the original block reference preserved by normalization.
			// This stays "latest"/"finalized" even if params were interpolated to hex.
			var blkTag string
			if ref := req.EvmBlockRef(); ref != nil {
				if s, ok := ref.(string); ok && (s == "latest" || s == "finalized") {
					blkTag = s
				}
			}
			if lg := n.logger; lg != nil && lg.GetLevel() <= zerolog.TraceLevel {
				lg.Trace().
					Str("blkTagFromEvmBlockRef", blkTag).
					Interface("evmBlockRefRaw", req.EvmBlockRef()).
					Msg("enrichStatePoller: resolved block tag from request EvmBlockRef")
			}
			// Fallback to inspecting the request param only if we couldn't resolve from EvmBlockRef
			if blkTag == "" {
				jrq, err := req.JsonRpcRequest(ctx)
				if err != nil {
					return
				}
				jrq.RLock()
				if len(jrq.Params) > 0 {
					if s, ok := jrq.Params[0].(string); ok && (s == "latest" || s == "finalized") {
						blkTag = s
					}
				}
				jrq.RUnlock()
				if lg := n.logger; lg != nil {
					lg.Trace().
						Str("blkTagFromParams", blkTag).
						Msg("enrichStatePoller: resolved block tag from request params")
				}
			}
			if blkTag == "" {
				return
			}
			jrs, _ := resp.JsonRpcResponse(ctx)
			bnh, err := jrs.PeekStringByPath(ctx, "number")
			if err != nil {
				return
			}
			blockNumber, err := common.HexToInt64(bnh)
			if err != nil {
				return
			}
			if lg := n.logger; lg != nil {
				lg.Trace().
					Str("blkTag", blkTag).
					Int64("blockNumber", blockNumber).
					Str("method", method).
					Msg("enrichStatePoller: suggesting block number to state poller")
			}
			if ups := resp.Upstream(); ups != nil {
				if ups, ok := ups.(common.EvmUpstream); ok {
					// These methods are non-blocking and handle async updates internally
					switch blkTag {
					case "finalized":
						ups.EvmStatePoller().SuggestFinalizedBlock(blockNumber)
					case "latest":
						ups.EvmStatePoller().SuggestLatestBlock(blockNumber)
						// An answer, never a vote: held by the tracker of
						// the request's group.
						selector := ""
						if d := req.Directives(); d != nil {
							selector = d.UseUpstream
						}
						if t := evm.ChainTrackerOf(ctx, n, selector); t != nil {
							t.Observe(ups.Id(), bytes.Clone(jrs.GetResultBytes()), false)
						}
					}
				}
			}
		} else if method == "eth_blockNumber" {
			jrs, _ := resp.JsonRpcResponse(ctx)
			bnh, err := jrs.PeekStringByPath(ctx)
			if err == nil {
				blockNumber, err := common.HexToInt64(bnh)
				if err == nil {
					if ups := resp.Upstream(); ups != nil {
						if ups, ok := ups.(common.EvmUpstream); ok {
							// This method is non-blocking and handles async updates internally
							ups.EvmStatePoller().SuggestLatestBlock(blockNumber)
						}
					}
				}
			}
		}
	}
}

func (n *Network) normalizeResponse(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	ctx, span := common.StartDetailSpan(ctx, "Network.NormalizeResponse")
	defer span.End()
	return stampClientResponseID(ctx, req, resp)
}

// stampClientResponseID makes the response id the client's original request
// id, regardless of what the upstream echoed back or which typed id a hook or
// cache built the response with. It applies to every JSON-RPC architecture, so
// proxies that multiplex ids toward upstreams never leak them to the client.
//
// The verbatim request id bytes win when present, so large integers (>2^53),
// fractional ids, and string ids round-trip exactly. A `null` id is echoed as
// `null` only on Solana (Agave does); EVM keeps the random id the request
// carried upstream. Programmatically built requests fall back to the typed id.
func stampClientResponseID(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	if resp == nil {
		return nil
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return nil
	}
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return err
	}
	if rawID := jrq.IDRawBytes(); len(rawID) > 0 && (string(rawID) != "null" || isSvmRequest(req)) {
		return jrr.SetIDBytes(rawID)
	}
	return jrr.SetID(jrq.ID)
}

func (n *Network) acquireRateLimitPermit(ctx context.Context, req *common.NormalizedRequest) error {
	if n.cfg.RateLimitBudget == "" {
		return nil
	}

	rlb, errNetLimit := n.rateLimitersRegistry.GetBudget(n.cfg.RateLimitBudget)
	if errNetLimit != nil {
		return errNetLimit
	}
	if rlb == nil {
		return nil
	}

	method, errMethod := req.Method()
	if errMethod != nil {
		return errMethod
	}
	lg := n.logger.With().Str("method", method).Logger()

	rules, errRules := rlb.GetRulesByMethod(method)
	if errRules != nil {
		return errRules
	}
	lg.Debug().Msgf("found %d network-level rate limiters", len(rules))

	if len(rules) > 0 {
		allowed, err := rlb.TryAcquirePermit(ctx, n.projectId, req, method, "", "", "", "network")
		if err != nil {
			return err
		}
		if !allowed {
			// Blocked event already recorded in budget.TryAcquirePermit; avoid double recording here
			return common.NewErrNetworkRateLimitRuleExceeded(
				n.projectId,
				n.networkId,
				n.cfg.RateLimitBudget,
				fmt.Sprintf("method:%s", method),
			)
		}
	}

	return nil
}

// tierUpstreamsByGroup moves fallback-tier upstreams behind the rest,
// preserving order within each tier.
func tierUpstreamsByGroup(ups []common.Upstream) []common.Upstream {
	return stablePartition(ups, isFallbackTier)
}

// isFallbackTier reports whether u is tagged tier:fallback.
func isFallbackTier(u common.Upstream) bool {
	return u.Config() != nil && u.Config().HasTag(common.TagTierFallback)
}

// headFirst picks req's band retry (spec §6.4): the upstreams of its lane
// not yet attempted, routed ones in routing order, then every other
// registered one within its use-upstream selector, since the ceiling that
// put the block in the band counts them all (syncing, excluded or fallback
// upstreams included); ordered by their heads as they are now, those
// reaching bn first, with no I/O.
func (n *Network) headFirst(req *common.NormalizedRequest, attempted map[string]struct{}, bn int64) func() (common.Upstream, error) {
	sel := ""
	if d := req.Directives(); d != nil {
		sel = d.UseUpstream
	}
	var rest []common.Upstream
	add := func(u common.Upstream) {
		if _, seen := attempted[u.Id()]; seen || slices.Contains(rest, u) {
			return
		}
		if sel != "" {
			if m, _ := common.UpstreamMatchesSelector(sel, u); !m {
				return
			}
		}
		rest = append(rest, u)
	}
	for _, u := range req.Upstreams() {
		add(u)
	}
	if n.upstreamsRegistry != nil {
		for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId) {
			add(u)
		}
	}
	rest = n.partitionUpstreamsByLatestBlock(rest, bn)
	return func() (common.Upstream, error) {
		for len(rest) > 0 {
			u := rest[0]
			rest = rest[1:]
			if _, loaded := req.ConsumedUpstreams.LoadOrStore(u, true); !loaded {
				return u, nil
			}
		}
		return nil, common.NewErrNoUpstreamsLeftToSelect(req, "no upstream left for the refusal band's retry")
	}
}

// partitionUpstreamsByLatestBlock moves the upstreams known not to reach
// block bn (common.EvmHeadReaches) behind the rest, preserving order within
// each group (spec §6.1, §6.4, §8). A push source is judged by its hash
// vote on this instance, as its requests ride this instance's socket,
// which may be half-open while the shared head counter moves; any other
// upstream by its head counter. An upstream with no known head keeps its
// place.
func (n *Network) partitionUpstreamsByLatestBlock(ups []common.Upstream, bn int64) []common.Upstream {
	if bn <= 0 || len(ups) < 2 {
		return ups
	}
	return stablePartition(ups, func(u common.Upstream) bool {
		head := upstreamLatestBlock(u)
		if n.metricsTracker != nil && n.sources.pushSource(u.Id()) {
			head = n.metricsTracker.HashVoteNumber(u)
		}
		return head > 0 && !common.EvmHeadReaches(u, head, bn)
	})
}

// stablePartition returns ups with the elements matching last moved to the
// end, preserving relative order; ups itself when none match.
func stablePartition(ups []common.Upstream, last func(common.Upstream) bool) []common.Upstream {
	if !slices.ContainsFunc(ups, last) {
		return ups
	}
	out := make([]common.Upstream, 0, len(ups))
	for _, u := range ups {
		if !last(u) {
			out = append(out, u)
		}
	}
	for _, u := range ups {
		if last(u) {
			out = append(out, u)
		}
	}
	return out
}

// preferTipLeaderForNearTipGetBlock moves leader, the request Head's Leader,
// to the front for eth_getBlockByNumber of its head or the next block while
// its head is strictly ahead of every other upstream in ups, which may not
// have imported that block yet.
func preferTipLeaderForNearTipGetBlock(ups []common.Upstream, method string, bn int64, leader common.Upstream) []common.Upstream {
	if method != "eth_getBlockByNumber" || len(ups) < 2 || leader == nil {
		return ups
	}
	tip := upstreamLatestBlock(leader)
	if bn < tip || bn > tip+1 {
		return ups
	}
	at := -1
	for i, u := range ups {
		switch {
		case u == leader:
			at = i
		case upstreamLatestBlock(u) >= tip:
			return ups
		}
	}
	if at <= 0 {
		return ups
	}
	out := make([]common.Upstream, 0, len(ups))
	out = append(out, ups[at])
	out = append(out, ups[:at]...)
	out = append(out, ups[at+1:]...)
	return out
}

// upstreamLatestBlock is u's polled head, or 0 when unknown.
func upstreamLatestBlock(u common.Upstream) int64 {
	if eu, ok := u.(common.EvmUpstream); ok {
		if sp := eu.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
			return sp.LatestBlock()
		}
	}
	return 0
}

// requestBlockNumber returns the block number a request targets, or 0 when
// it has none, extracted the same way as checkUpstreamBlockAvailability.
func requestBlockNumber(ctx context.Context, req *common.NormalizedRequest) int64 {
	if req == nil {
		return 0
	}
	if v := req.EvmBlockNumber(); v != nil {
		if n64, ok := v.(int64); ok && n64 > 0 {
			return n64
		}
	}
	if _, x, ebn := evm.ExtractBlockReferenceFromRequest(ctx, req); ebn == nil && x > 0 {
		return x
	}
	return 0
}

// routingBlockNumber is the block req reads, for routing: requestBlockNumber,
// or for a read by block hash the number of the header the network-wide
// tracker holds; 0 otherwise.
func (n *Network) routingBlockNumber(ctx context.Context, req *common.NormalizedRequest) int64 {
	if req == nil {
		return 0
	}
	if v, ok := req.EvmBlockNumber().(int64); ok && v > 0 {
		return v
	}
	ref, x, err := evm.ExtractBlockReferenceFromRequest(ctx, req)
	switch {
	case err != nil:
		return 0
	case x > 0:
		return x
	case len(ref) == 66:
		bn, _ := n.chainTracker.HeldNumber(ref)
		return bn
	}
	return 0
}
