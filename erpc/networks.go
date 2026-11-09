package erpc

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"maps"
	"math"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/evm/logfilter"
	"github.com/erpc/erpc/architecture/svm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/mempool"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type Network struct {
	networkId         string
	networkLabel      string
	projectId         string
	logger            *zerolog.Logger
	headLoopOnce      sync.Once
	appCtx            context.Context
	cfg               *common.NetworkConfig
	inFlightRequests  *sync.Map
	failsafeExecutors []*networkExecutor // main: failsafe rename FailsafeExecutor → networkExecutor
	// bareExecutor applies no network policy: timeout-only requests (state
	// probes) take it, so they get no network retry or hedge (spec §6.11).
	bareExecutor         *networkExecutor
	rateLimitersRegistry *upstream.RateLimitersRegistry
	cacheDal             common.CacheDAL
	// cacheHead is the cache's fresh head (evm.CacheHead), nil without a
	// head-aware realtime policy: its poll is its own voter's poll of
	// latest, else sights blocks for the block clock (cacheHeadPolled), and
	// with evm.servedTip.followCacheHead it lowers network-wide reads'
	// Latest (anchorHead). servedLatest is the Latest those reads took
	// last, its Clamp's floor.
	cacheHead         *evm.CacheHead
	servedLatest      atomic.Int64
	metricsTracker    *health.Tracker
	upstreamsRegistry *upstream.UpstreamsRegistry
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
	// holds the advance-age watchdog, the regression guard's memory and the
	// finalized floor; the pick itself is stateless (common.PickServedTip).
	servedTipAll servedTipLane
	// servedFinalizedShared is the highest network-wide Finalized any
	// instance served (shareFinalized). Nil without shared state.
	servedFinalizedShared data.CounterInt64SharedVariable
	// finalizedStepTo is the highest finalized any voter reported, and
	// finalizedStepAt (unix ns) when it last rose (finalizedStep).
	finalizedStepTo, finalizedStepAt atomic.Int64
	// finalizedRTT is, per upstream id, its finalized polls' round trip
	// average in ns (*atomic.Int64): a due step polls the fastest first.
	finalizedRTT sync.Map

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
	// tipMethods holds each method's served-tip voter set (methodVotersFor);
	// tipMethodsMu serializes its builds.
	tipMethods   atomic.Pointer[tipMethods]
	tipMethodsMu sync.Mutex

	// headWake wakes the head loop to compute the dirty lanes
	// (refreshDirtyHeads); headPass is closed and replaced after each
	// pass, for readers whose selector forms no lane.
	headWake chan struct{}
	headPass atomic.Pointer[chan struct{}]
	// headRefresh is the head loop's timer, set before Bootstrap; 0 is
	// headRefreshInterval.
	headRefresh time.Duration
	// heldAt is, per upstream id, the highest block this instance's
	// by-number client answers from it held (*atomic.Int64): holder
	// evidence for Firm and routing only, never a head report (spec §6.5).
	heldAt sync.Map
	// rangeAsks is, per upstream id, its newest range-check Ask (spec
	// §6.4 range check): every range check of that block or a lower one
	// joins it.
	rangeAskMu sync.Mutex
	rangeAsks  map[string]*rangeAsk

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
// intake, reconciler, taps, trackers or tracker fetches runs, nor any of
// its upstreams registry's (UpstreamsRegistry.Wait).
func (n *Network) wait() {
	n.bg.Wait()
	if n.chainTracker != nil {
		n.chainTracker.Wait()
	}
	if n.upstreamsRegistry != nil {
		n.upstreamsRegistry.Wait()
	}
}

// maxServedTipLanes caps the materialized group lanes per network, a backstop
// on top of the matched-set dedup. Realistic configs have 1–3 groups; beyond
// the cap, a group's request gets the stateless subset pick with no lane.
const maxServedTipLanes = 16

// servedTipLane is one Head scope: the network-wide lane (label "all", key
// "") or a use-upstream group's (label common.LaneName of the matched
// upstream set, key partitionKeyFor's). It publishes the lane's number
// facts and holds the advance-age watchdogs, the finalized guard's memory
// and the finalized floor; mu orders its computations so a slower one
// never publishes over a newer one. The lane's view is its tracker's.
type servedTipLane struct {
	key       string
	selector  string
	label     string
	latest    servedTipAnchor
	finalized servedTipAnchor

	mu   sync.Mutex
	head atomic.Pointer[common.EvmHead]
	// dirty: a change since the lane's last computation began
	// (markLanes).
	dirty atomic.Bool
	// gauges are bound at the first computation. Guarded by mu.
	gauges *laneGauges
	// finalizedFloor is the highest Finalized the lane served since an
	// evaluation last found no voter backing it (holdFinalized). Guarded
	// by mu.
	finalizedFloor int64
	// finalizedRefreshing: a background finalized refresh runs
	// (refreshFinalized).
	finalizedRefreshing atomic.Bool
	// finalizedReadAt (unix ns) is the lane's last finalized-tag request:
	// within 2 W of it the lane has finality demand (refreshFinalized).
	finalizedReadAt atomic.Int64
	// finalizedStepSize and finalizedStepDepth are the lane's finality
	// step and its depth below the voters' sight of the head, the smallest
	// of the last advances (finalizedSteps); 0 while none was seen.
	// finalizedDueAt is the sight a due step's refresh last started at,
	// finalizedDueStartedAt (unix ns) when (dueFinalizedStep).
	finalizedStepSize, finalizedStepDepth, finalizedDueAt, finalizedDueStartedAt atomic.Int64
	// finalizedSteps are the last advances of the lane's Finalized, size
	// and depth below Latest, newest at finalizedStepsAt-1 mod len.
	// Guarded by mu.
	finalizedSteps   [8][2]int64
	finalizedStepsAt int
}

// learnFinalizedStep records an advance of the lane's Finalized by size
// blocks to depth below its Latest; callers hold mu.
func (l *servedTipLane) learnFinalizedStep(size, depth int64) {
	l.finalizedSteps[l.finalizedStepsAt%len(l.finalizedSteps)] = [2]int64{size, depth}
	l.finalizedStepsAt++
	minSize, minDepth := int64(0), int64(0)
	for i, st := range l.finalizedSteps[:min(l.finalizedStepsAt, len(l.finalizedSteps))] {
		if i == 0 || st[0] < minSize {
			minSize = st[0]
		}
		if i == 0 || st[1] < minDepth {
			minDepth = st[1]
		}
	}
	l.finalizedStepSize.Store(minSize)
	l.finalizedStepDepth.Store(minDepth)
}

// holdFinalized returns the Finalized to serve for the computed tip and
// keeps the lane's floor, the highest Finalized it served (spec §6.4,
// #1353). Finality never reverts, so a lower tip while backing (the lowest
// highest finalized vote of the lane's ballot and each constraining
// guarantee subset) still reaches the floor means the voters changed — a
// leader left the ballot, or a lagger rejoined a lone leader — not the
// chain. The floor holds only a drop of at most tolerance blocks: voter
// changes move Finalized by far less, and a deeper drop is the regression
// guard's to bound, so a far-ahead head once served alone cannot back the
// floor on its own forever. Outside those bounds the floor drops to the
// tip, and a voter that returns later does not restore it. Callers hold
// mu: the backing is from the same evaluation as the floor it is compared
// to.
func (l *servedTipLane) holdFinalized(tip, backing, tolerance int64) int64 {
	if tip < l.finalizedFloor && l.finalizedFloor-tip <= tolerance && l.finalizedFloor <= backing {
		return l.finalizedFloor
	}
	l.finalizedFloor = tip
	return tip
}

// finalizedFloorTolerance is the deepest drop a finalized floor holds: the
// regression guard's tolerance (servedTip.maxRegressionBlocks), the default
// one when the guard is off, so the two split the drops between them.
func (n *Network) finalizedFloorTolerance() int64 {
	if t := n.servedTipMaxRegressionBlocks(); t >= 0 {
		return t
	}
	return common.DefaultToleratedBlockHeadRollback
}

// shareFinalized lifts the network-wide lane's Finalized `served` to the
// highest one any instance served (servedFinalizedShared), so instances
// behind one load balancer agree (spec §6.4, §14), under holdFinalized's
// rule: at most tolerance above the `computed` tip, and only while backing
// still reaches it. Any other shared value is ignored, and ages out as the
// chain passes it.
func (n *Network) shareFinalized(served, computed, backing, tolerance int64) int64 {
	if n.servedFinalizedShared == nil || served <= 0 {
		return served
	}
	if shared := n.servedFinalizedShared.GetValue(); served < shared && shared-computed <= tolerance && shared <= backing {
		return shared
	}
	return served
}

// publishFinalized raises the shared network-wide Finalized to `computed`,
// this evaluation's computed tip, as main does (#1364): a held value never
// publishes. Only raises are published: a value the chain left behind stays
// in the counter until the chain passes it, unserved by shareFinalized's
// tolerance bound, while publishing it down would make instances whose views
// differ by more than the counter's rollback tolerance overwrite each other on
// every evaluation. A raise updates the local copy at once and the store in
// the background (TryUpdate), so the head computation never waits on it.
func (n *Network) publishFinalized(computed int64) {
	if n.servedFinalizedShared != nil && computed > n.servedFinalizedShared.GetValue() {
		n.servedFinalizedShared.TryUpdate(context.Background(), computed, time.Now())
	}
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
	// A copy: defaults and the engine's default-policy upgrade write it, and
	// a retry runs this again while the network serves from n.cfg.
	cfg := &common.SelectionPolicyConfig{}
	if n.cfg.SelectionPolicy != nil {
		*cfg = *n.cfg.SelectionPolicy
	}
	// Callers may have set Eval but skipped SetDefaults (common in tests that
	// build Config as Go struct literals). Compile here so the engine never
	// sees a nil program.
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
	if n.cfg.Architecture == "" && n.cfg.Evm != nil {
		return common.ArchitectureEvm
	}
	return n.cfg.Architecture
}

func (n *Network) ShadowUpstreams() []*upstream.Upstream {
	return n.upstreamsRegistry.GetNetworkShadowUpstreams(n.networkId)
}

func (n *Network) Logger() *zerolog.Logger {
	return n.logger
}

// evmTipBallot builds the served-tip pick for an axis under quorum q from the
// number votes of scope s (voterSet) that satisfy member (nil = all of
// them). live reports whether live, non-capped heads decided it (benched or
// not): a cap-only ballot gives the regression guard no live reference.
//
// MIXED POOL vs ALL-CAPPED POOL. The rule applies per candidate set, which is
// what lets the same function build the finalized ballot AND each
// guaranteed-method's floor ballot: caps are dropped only when the SAME set
// contains at least one live head. A set of only historical/frozen upstreams
// legitimately serves its cap — that cap is the freshest block anyone in it can
// serve — so it keeps voting. (No live head means no live reference for the
// regression guard either; it falls back to its own last corroborated pick,
// see guardServedTipRegression.)
func (n *Network) evmTipBallot(s tipScope, useFinalized bool, q common.ServedTipQuorum, member func(common.Upstream) bool) (pick common.ServedTipPick, live bool) {
	// Typical pools fit the stack buffer, so the ballot stays off the heap;
	// append moves a larger one there.
	var buf [16]int64
	heads := buf[:0]
	vs := n.voters(s, voterQuery{kind: numberVote, finalized: useFinalized, member: member})
	vs.each(func(_ common.Upstream, head int64) bool {
		heads = append(heads, head)
		return true
	})
	return common.PickServedTip(heads, q), vs.live()
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
	return n.tipCandidates(tipScope{sel: requestSelector(ctx)})
}

// tipScope is whose heads a Head counts: the upstreams use-upstream
// selector sel matches ("" = every upstream), less excluded, those that do
// not serve the request's method (nonServers). The zero scope is every
// upstream.
type tipScope struct {
	sel      string
	excluded []common.Upstream
}

// tipCandidates is EvmRoutableUpstreams for scope s.
func (n *Network) tipCandidates(s tipScope) iter.Seq[common.Upstream] {
	// Kept this small so it inlines: the closure then stays on the caller's
	// stack instead of costing a heap allocation per walk.
	return func(yield func(common.Upstream) bool) { n.walkTipCandidates(s, yield) }
}

func (n *Network) walkTipCandidates(s tipScope, yield func(common.Upstream) bool) {
	visit := func(u common.Upstream) bool {
		if s.sel != "" {
			if m, _ := common.UpstreamMatchesSelector(s.sel, u); !m {
				return true
			}
		}
		if slices.Contains(s.excluded, u) {
			return true
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

// tipMethods is a network's nonServers per method, for one
// upstream.MethodFilterEpoch. Immutable: a change publishes a copy.
//
// methods holds only names the methods config defines exactly
// (common.IsKnownMethodName), so client input cannot grow it: a casing
// variant or an unknown name is resolved per request instead. Variants are
// not folded to the config's name because upstream ignoreMethods/allowMethods
// match case-sensitively, so a variant may have other voters.
type tipMethods struct {
	epoch uint64
	// uniform: every upstream filters methods alike, so every method is
	// served by all of them or by none, and every method has every voter.
	uniform bool
	methods map[string][]common.Upstream
}

// nonServers is the upstreams excluded from method's voters (tipScope):
// those that do not serve it (servesMethod, the filter Forward applies),
// none when no upstream serves it, as Forward then keeps them all, and
// none for eth_blockNumber, answered from the view whatever its voters. A
// request pays one atomic load, one epoch compare and, on a network whose
// upstreams filter methods differently, one map lookup; a known method's
// set is built under tipMethodsMu on its first request, and every set is
// rebuilt after the epoch moves (an upstream registered or reordered, a
// method auto-ignored or its auto-ignore expired).
func (n *Network) nonServers(ctx context.Context, method string) []common.Upstream {
	if method == "" || method == "eth_blockNumber" {
		return nil
	}
	m := n.tipMethods.Load()
	if m == nil || m.epoch != upstream.MethodFilterEpoch() {
		m = n.rebuildTipMethods(ctx)
	}
	if m.uniform {
		return nil
	}
	if ex, ok := m.methods[method]; ok {
		return ex
	}
	var methods *common.MethodsConfig
	if n.cfg != nil {
		methods = n.cfg.Methods
	}
	if !common.IsKnownMethodName(methods, method) {
		return n.computeNonServers(ctx, method)
	}
	return n.addTipMethod(ctx, method)
}

// rebuildTipMethods publishes the current epoch's tipMethods, recomputing
// every method the previous one held.
func (n *Network) rebuildTipMethods(ctx context.Context) *tipMethods {
	n.tipMethodsMu.Lock()
	defer n.tipMethodsMu.Unlock()
	// The epoch is read before the snapshot, so a change racing the build
	// moves it and the next request rebuilds.
	epoch := upstream.MethodFilterEpoch()
	prev := n.tipMethods.Load()
	if prev != nil && prev.epoch == epoch {
		return prev
	}
	next := &tipMethods{epoch: epoch, uniform: n.uniformMethodFilters(ctx), methods: map[string][]common.Upstream{}}
	if !next.uniform && prev != nil {
		for name := range prev.methods {
			next.methods[name] = n.computeNonServers(ctx, name)
		}
	}
	n.tipMethods.Store(next)
	return next
}

// addTipMethod publishes the current tipMethods with method added.
func (n *Network) addTipMethod(ctx context.Context, method string) []common.Upstream {
	n.tipMethodsMu.Lock()
	defer n.tipMethodsMu.Unlock()
	m := n.tipMethods.Load()
	if ex, ok := m.methods[method]; ok {
		return ex
	}
	next := &tipMethods{epoch: m.epoch, uniform: m.uniform, methods: maps.Clone(m.methods)}
	ex := n.computeNonServers(ctx, method)
	next.methods[method] = ex
	n.tipMethods.Store(next)
	return ex
}

// uniformMethodFilters reports whether every registered upstream has the
// same method filter (Upstream.MethodFilter).
func (n *Network) uniformMethodFilters(ctx context.Context) bool {
	if n.upstreamsRegistry == nil {
		return true
	}
	all := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
	if len(all) == 0 {
		return true
	}
	ignore0, allow0, every0 := all[0].MethodFilter()
	for _, u := range all[1:] {
		ignore, allow, every := u.MethodFilter()
		if !slices.Equal(ignore, ignore0) || !slices.Equal(allow, allow0) || every != every0 {
			return false
		}
	}
	return true
}

// computeNonServers is the registered upstreams that do not serve method
// (servesMethod), nil when every one or none serves it.
func (n *Network) computeNonServers(ctx context.Context, method string) []common.Upstream {
	if n.upstreamsRegistry == nil {
		return nil
	}
	var excluded []common.Upstream
	serving := 0
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId) {
		if servesMethod(u, method) {
			serving++
		} else {
			excluded = append(excluded, u)
		}
	}
	if serving == 0 {
		return nil
	}
	return excluded
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
// partitionKeyFor, count capped by maxServedTipLanes), else nil: the
// subset pick then runs per request with no guard, no gauges and no
// anchor, so a subset value never overwrites any gauge. A new lane
// publishes its first Head before it is returned.
func (n *Network) laneFor(sel string) *servedTipLane {
	if sel == "" {
		return &n.servedTipAll
	}
	key, ids := n.partitionKeyFor(context.Background(), sel)
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
	l := &servedTipLane{key: key, selector: sel, label: common.LaneName(ids)}
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

// EvmUpstreamGroupForSelector is the single source of node-GROUP identity:
// it maps a use-upstream selector to its lane's stable key (for keying
// per-group state) and human-readable name, or ("","") when the selector
// has no lane of its own: it carves out no real sub-group, or its group
// came past maxServedTipLanes. The lanes' Heads and the integrity module
// (group trackers, spec §13) both read it, so a network holds at most
// maxServedTipLanes group trackers, and a selector past them tracks and
// corroborates within the network-wide lane — one grouping, not two.
func (n *Network) EvmUpstreamGroupForSelector(_ context.Context, selector string) (key string, lane string) {
	if l := n.laneFor(selector); l != nil && l.key != "" {
		return l.key, l.label
	}
	return "", ""
}

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

// refreshFinalized refreshes the finalized numbers of selector sel's
// voters for a finalized-tag request (spec §6.4 Finality, §6.8). The
// request never waits for it: it takes Finalized at once, and the refresh
// runs in the background, one at a time per lane. While the lane's head
// makes its finality steps due (dueFinalizedStep started one within W),
// its requests poll nothing. Otherwise, while q voters reported finalized
// within W the refresh polls one voter; at the idle edge, fewer than q
// such reports, as many as Finalized lacks, so the next request takes
// their answers.
func (n *Network) refreshFinalized(ctx context.Context, sel string) {
	l := n.laneFor(sel)
	if l == nil {
		l = &n.servedTipAll
	}
	w := health.FreshnessWindow(n.EvmBlockTime())
	l.finalizedReadAt.Store(time.Now().UnixNano())
	if time.Since(time.Unix(0, l.finalizedDueStartedAt.Load())) <= w {
		return
	}
	s := n.voters(tipScope{sel: sel}, voterQuery{kind: numberVote})
	if s.count() == 0 {
		return
	}
	var q common.ServedTipQuorum
	if n.cfg != nil {
		q = n.cfg.Evm.ServedTipQuorum()
	}
	fresh := 0
	s.each(func(u common.Upstream, _ int64) bool {
		sp := u.(common.EvmUpstream).EvmStatePoller()
		if sp.FinalizedBlock() > 0 && !finalizedTagless(sp) && time.Since(sp.FinalizedReportAt()) <= w {
			fresh++
		}
		return true
	})
	n.goPollFinalized(context.WithoutCancel(ctx), l, sel, s, q, w, max(1, q.Of(s.attesting())-fresh), 0)
}

// dueFinalizedStep starts lane l's refresh for its next finality step
// once latest, the voters' sight of the head (refreshLane), makes the step due: at least
// Finalized + the learned step + the learned depth, as the voters' own
// finality moved before (spec §6.8). It runs while the lane has finality
// demand, once per latest; callers hold l.mu.
func (n *Network) dueFinalizedStep(l *servedTipLane, finalized, latest int64) {
	size, depth := l.finalizedStepSize.Load(), l.finalizedStepDepth.Load()
	if size <= 0 || finalized <= 0 || latest < finalized+size+depth || latest <= l.finalizedDueAt.Load() || n.appCtx == nil {
		return
	}
	w := health.FreshnessWindow(n.EvmBlockTime())
	if time.Since(time.Unix(0, l.finalizedReadAt.Load())) > 2*w {
		return
	}
	s := n.voters(tipScope{sel: l.selector}, voterQuery{kind: numberVote})
	if s.count() == 0 {
		return
	}
	var q common.ServedTipQuorum
	if n.cfg != nil {
		q = n.cfg.Evm.ServedTipQuorum()
	}
	if n.goPollFinalized(n.appCtx, l, l.selector, s, q, w, 0, finalized+size) {
		l.finalizedDueAt.Store(latest)
		l.finalizedDueStartedAt.Store(time.Now().UnixNano())
	}
}

// goPollFinalized runs pollFinalized in the background unless lane l
// already runs a finalized refresh; started reports whether it did.
func (n *Network) goPollFinalized(ctx context.Context, l *servedTipLane, sel string, s voterSet, q common.ServedTipQuorum, w time.Duration, slots int, step int64) (started bool) {
	if !l.finalizedRefreshing.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer l.finalizedRefreshing.Store(false)
		defer func() {
			if rec := recover(); rec != nil {
				telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
					"finalized-refresh",
					fmt.Sprintf("project:%s network:%s lane:%s", n.projectId, n.networkId, l.label),
					common.ErrorFingerprint(rec),
				).Inc()
				n.logger.Error().Interface("panic", rec).Str("lane", l.label).Str("stack", string(debug.Stack())).
					Msg("unexpected panic while refreshing a lane's finalized")
			}
		}()
		n.pollFinalized(ctx, sel, s, q, w, slots, step)
	}()
	return true
}

// pollFinalized polls the finalized numbers of voters s of selector sel
// and waits for them, at most W, then publishes sel's lane Head. Finality
// moves in steps, and any voter may see a step first: a voter's finality
// depth is its own, and a replica may trail the nodes. A due step (step >
// 0, dueFinalizedStep) polls as many of the voters below it as Finalized,
// the q-th highest vote, lacks to reach it, fastest first, at once: one
// round trip. Otherwise each refresh polls slots voters, those whose
// finalized reports are oldest, at their poll cadence: successive
// refreshes walk the voters. A voter finalized above every one reported
// before, by any poll, starts a step: for W after, as many of the voters
// still below it as Finalized lacks to reach it poll once since the step
// began, fastest first, at once on the refresh that saw it too, so
// Finalized follows within a round trip; a voter that has not reached the
// step leaves the next one to the next refresh. Every voter with no
// finalized number yet polls at its cadence, so a cold instance gets its
// first ballot, or gives a tagless voter's tag up, as soon as it would
// with every voter polled. Each poller joins a poll in flight, and has
// none once its finalized tag is given up.
func (n *Network) pollFinalized(ctx context.Context, sel string, s voterSet, q common.ServedTipQuorum, w time.Duration, slots int, step int64) {
	ctx, cancel := context.WithTimeout(ctx, w)
	defer cancel()
	var moved atomic.Bool
	refresh := func(u common.Upstream, sp common.EvmStatePoller, maxAge time.Duration) (fetched bool) {
		before, sent := sp.FinalizedBlock(), time.Now()
		fetched = sp.RefreshFinalized(ctx, maxAge)
		if fetched {
			n.finalizedRoundTrip(u.Id(), time.Since(sent))
		}
		if sp.FinalizedBlock() != before {
			moved.Store(true)
		}
		return fetched
	}
	type voter struct {
		u  common.Upstream
		sp common.EvmStatePoller
	}
	stepRound := func(wg *sync.WaitGroup, top int64, at time.Time) {
		lacks := q.Of(s.attesting())
		var below []voter
		s.each(func(u common.Upstream, _ int64) bool {
			switch sp := u.(common.EvmUpstream).EvmStatePoller(); {
			case finalizedTagless(sp):
			case sp.FinalizedBlock() >= top:
				lacks--
			case sp.FinalizedReportAt().Before(at):
				below = append(below, voter{u, sp})
			}
			return true
		})
		slices.SortFunc(below, func(a, b voter) int {
			return cmp.Compare(n.finalizedRoundTripOf(a.u.Id()), n.finalizedRoundTripOf(b.u.Id()))
		})
		// Each slot polls the fastest voter left until one polls: a voter
		// with no finalized poll (its tag given up) takes no slot.
		var next atomic.Int32
		for range max(0, min(lacks, len(below))) {
			wg.Go(func() {
				for i := int(next.Add(1)) - 1; i < len(below); i = int(next.Add(1)) - 1 {
					if refresh(below[i].u, below[i].sp, time.Since(at)) {
						return
					}
				}
			})
		}
	}
	var wg sync.WaitGroup
	if step > 0 {
		stepRound(&wg, step, time.Now())
		wg.Wait()
		if l := n.laneFor(sel); moved.Load() && l != nil {
			n.refreshLaneRecovered(l)
		}
		return
	}
	top, at := n.finalizedStep(s, w)
	if !at.IsZero() {
		stepRound(&wg, top, at)
	}
	var buf [16]voter
	byAge := buf[:0]
	s.each(func(u common.Upstream, _ int64) bool {
		if sp := u.(common.EvmUpstream).EvmStatePoller(); sp.FinalizedBlock() > 0 {
			byAge = append(byAge, voter{u, sp})
		} else {
			wg.Go(func() { refresh(u, sp, 0) })
		}
		return true
	})
	slices.SortFunc(byAge, func(a, b voter) int { return a.sp.FinalizedReportAt().Compare(b.sp.FinalizedReportAt()) })
	var next atomic.Int32
	var polls sync.WaitGroup
	for range min(slots, len(byAge)) {
		polls.Go(func() {
			for i := int(next.Add(1)) - 1; i < len(byAge); i = int(next.Add(1)) - 1 {
				if refresh(byAge[i].u, byAge[i].sp, 0) {
					return
				}
			}
		})
	}
	polls.Wait()
	// The poll may have seen a new step, also while an earlier one is
	// under way: else Finalized waits for the next request or poll.
	if next, nextAt := n.finalizedStep(s, w); !nextAt.IsZero() && (at.IsZero() || next > top) {
		stepRound(&wg, next, nextAt)
	}
	wg.Wait()
	if l := n.laneFor(sel); moved.Load() && l != nil {
		n.refreshLaneRecovered(l)
	}
}

// finalizedRoundTrip records d, a finalized poll of upstream id, in its
// round trip average (finalizedRoundTripOf).
func (n *Network) finalizedRoundTrip(id string, d time.Duration) {
	v, _ := n.finalizedRTT.LoadOrStore(id, new(atomic.Int64))
	rtt := v.(*atomic.Int64)
	if old := rtt.Load(); old > 0 {
		d = (3*time.Duration(old) + d) / 4
	}
	rtt.Store(int64(d))
}

// finalizedRoundTripOf is upstream id's finalized poll round trip, a
// moving average; 0 before its first, so an unmeasured voter goes first.
func (n *Network) finalizedRoundTripOf(id string) time.Duration {
	if v, ok := n.finalizedRTT.Load(id); ok {
		return time.Duration(v.(*atomic.Int64).Load())
	}
	return 0
}

// finalizedStep is the finality step under way: the highest finalized a
// voter of s reported, and when it rose above the highest any voter
// reported before; a zero time when that was W or more ago. Steps are the
// chain's, so the record is network-wide.
func (n *Network) finalizedStep(s voterSet, w time.Duration) (top int64, at time.Time) {
	s.each(func(u common.Upstream, _ int64) bool {
		top = max(top, u.(common.EvmUpstream).EvmStatePoller().FinalizedBlock())
		return true
	})
	for prev := n.finalizedStepTo.Load(); top > prev; prev = n.finalizedStepTo.Load() {
		if n.finalizedStepTo.CompareAndSwap(prev, top) {
			n.finalizedStepAt.Store(time.Now().UnixNano())
			break
		}
	}
	if at = time.Unix(0, n.finalizedStepAt.Load()); time.Since(at) >= w {
		return 0, time.Time{}
	}
	return n.finalizedStepTo.Load(), at
}

// firstHolder is the id of the first voter of selector's lane, in
// selection order, whose head counter holds block and that has a poller:
// a hash fetch of it brings the vote that moves the view's Head there
// (spec §6.4 by-hash wait); "" while none holds it.
func (n *Network) firstHolder(selector string, block int64) (id string) {
	n.voters(tipScope{sel: selector}, voterQuery{kind: numberVote}).each(func(u common.Upstream, head int64) bool {
		if head < block {
			return true
		}
		if sp := u.(common.EvmUpstream).EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
			id = u.Id()
			return false
		}
		return true
	})
	return id
}

// EvmPollForks Asks, with a fork need at pace, each voter in ids whose
// vote the pins moved off (spec §6.3). Each answer is its voter's hash
// vote, whatever it voted before. next is the soonest of their Asks' Next.
func (n *Network) EvmPollForks(ids []string, pace time.Duration) (next time.Time) {
	return n.pollHeads(ids, common.HeadNeedFork, pace, common.AskSettle)
}

// EvmHeadRewound is the network-wide tracker's note that its pins moved
// down to head (spec §14): the hash ages restart from the first votes at
// or below it.
func (n *Network) EvmHeadRewound(head int64) {
	if n.metricsTracker != nil {
		n.metricsTracker.RewindFirstVotes(n.networkId, head)
	}
}

// EvmRewindHeads is the network-wide tracker's rollback write (spec §14):
// votes holds, per upstream id, the height of its own hash vote on the
// tracker's new branch; each counter above it is rewound to it.
func (n *Network) EvmRewindHeads(votes map[string]int64) {
	if n.upstreamsRegistry == nil {
		return
	}
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId) {
		bn, ok := votes[u.Id()]
		if !ok {
			continue
		}
		if p := u.EvmStatePoller(); p != nil && !p.IsObjectNull() {
			p.RewindLatestBlock(bn)
		}
	}
}

// pollHeads Asks each upstream in ids in the background; next is the
// soonest of their Asks' Next.
func (n *Network) pollHeads(ids []string, need common.HeadNeed, pace time.Duration, reason common.AskReason) (next time.Time) {
	if n.upstreamsRegistry == nil {
		return time.Time{}
	}
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId) {
		if !slices.Contains(ids, u.Id()) {
			continue
		}
		p := u.EvmStatePoller()
		if p == nil || p.IsObjectNull() {
			continue
		}
		if at := p.AskBackground(need, pace, time.Time{}, reason).Next; !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	return next
}

// EvmLatestAskAt is, for each upstream in ids, the send time of this
// instance's last Ask fetch of its latest: the Ask half of a chain
// tracker's evidence times (common.EvidenceAt). An upstream never Asked
// is absent.
func (n *Network) EvmLatestAskAt(ids []string) map[string]time.Time {
	out := make(map[string]time.Time, len(ids))
	if n.upstreamsRegistry == nil {
		return out
	}
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId) {
		if !slices.Contains(ids, u.Id()) {
			continue
		}
		if p := u.EvmStatePoller(); p != nil && !p.IsObjectNull() {
			if at := p.LatestAskAt(); !at.IsZero() {
				out[u.Id()] = at
			}
		}
	}
	return out
}

// EvmAskHead is the Ask of upstreamId's head at pace, after after, for a
// chain tracker's settle or block clock (spec §6.5 rule 5, §6.8);
// AskBackoff when the network has no such upstream with a poller.
func (n *Network) EvmAskHead(ctx context.Context, upstreamId string, need common.HeadNeed, pace time.Duration, after time.Time, reason common.AskReason) common.HeadAnswer {
	if n.upstreamsRegistry != nil {
		for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId) {
			if u.Id() != upstreamId {
				continue
			}
			if p := u.EvmStatePoller(); p != nil && !p.IsObjectNull() {
				return p.Ask(ctx, need, pace, after, reason)
			}
			break
		}
	}
	return common.HeadAnswer{Outcome: common.AskBackoff}
}

// EvmFetchHeld fetches block number from upstreamId for a chain tracker's
// block clock (spec §6.4 by-hash wait, §6.8 shared early fetch,
// evm.EvmStatePoller.FetchHeld); AskBackoff when the network has no such
// upstream with a poller.
func (n *Network) EvmFetchHeld(ctx context.Context, upstreamId string, number uint64) common.HeadAnswer {
	if n.upstreamsRegistry != nil {
		for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId) {
			if u.Id() != upstreamId {
				continue
			}
			if p, ok := u.EvmStatePoller().(*evm.EvmStatePoller); ok && !p.IsObjectNull() {
				return p.FetchHeld(ctx, number)
			}
			break
		}
	}
	return common.HeadAnswer{Outcome: common.AskBackoff}
}

// tryShortCircuitFutureBlock returns a truthful null response (ok=true) for
// a numbered eth_getBlockByNumber the refusal rule calls future (spec §6.4,
// #1238): above the ceiling plus its age in blocks, on a ceiling younger
// than 1 bt or Asked again. No upstream can serve it yet, so dispatching
// and hedging across all of them only burns latency and load. It runs only
// when the network sets evm.shortCircuitFutureBlocks, and the synthesized
// response is returned directly from Forward, so it is never cached.
func (n *Network) tryShortCircuitFutureBlock(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, bool) {
	if !evm.ShortCircuitsFuture(req) {
		return nil, false
	}
	v := evm.HeadRefusalOf(ctx, req)
	if v == nil || v.Rule != common.HeadFuture {
		return nil, false
	}
	v.Refuse()
	return evm.NullAnswer(req, v.Block)
}

// refuseFutureRange refuses a range method (eth_getLogs, trace_filter)
// whose toBlock the refusal rule calls future, before any dispatch (spec
// §6.4): a node answers a range above its head with the part it has, so an
// empty or short answer would pass for the whole range. The error names
// the ceiling the verdict was decided against.
func (n *Network) refuseFutureRange(req *common.NormalizedRequest, method string) error {
	if n.cfg.Architecture != common.ArchitectureEvm || !methodHasDedicatedRangeAvailabilityHook(method) {
		return nil
	}
	v := req.HeadRefusal()
	if v == nil || v.Rule != common.HeadFuture {
		return nil
	}
	n.countHeadRefusal(v.FutureRule())
	return &common.ErrUpstreamBlockUnavailable{
		BaseError: common.BaseError{
			Code:    common.ErrCodeUpstreamBlockUnavailable,
			Message: fmt.Sprintf("block not found for %s, requested toBlock %d is above the highest known block %d", method, v.Block, v.Ceiling),
			Details: map[string]interface{}{
				"blockNumber":  v.Block,
				"highestBlock": v.Ceiling,
			},
		},
	}
}

// countRefusal counts the refusal rule's outcome for req (spec §17): data
// is served, or thin_found or retried_found after a miss; an empty or
// missing answer is thin_missing in the thin region, retried_missing in
// the band, future above it. eth_getBlockByNumber of a block in the head
// region, Latest − reorgWindow < N ≤ Latest, answered null or missing is
// also served_missing, a broken head promise. Other methods' null or
// missing-data answers at or below Firm can be legit (an index past a
// block's count, a state read on a pruned node), and blocks below the
// head region are availability, not the promise: no refusal, as for
// requests naming no block and empty answers.
func (n *Network) countRefusal(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error) {
	v := req.HeadRefusal()
	if v == nil || v.Block <= 0 {
		return
	}
	method, _ := req.Method()
	switch {
	case err == nil && resp != nil && !resp.IsObjectNull(ctx) && !refusalMiss(ctx, method, resp):
		switch {
		case !v.Retried():
			n.countHeadRefusal("served")
		case v.Rule == common.HeadThin:
			n.countHeadRefusal("thin_found")
		default:
			n.countHeadRefusal("retried_found")
		}
		return
	case err != nil && !missingDataErr(err):
		return
	}
	if method == "eth_getBlockByNumber" && n.inHeadRegion(v) && (err != nil || isNullResult(ctx, resp)) {
		n.countHeadRefusal("served_missing")
	}
	switch v.Rule {
	case common.HeadThin:
		n.countHeadRefusal("thin_missing")
	case common.HeadBand:
		n.countHeadRefusal("retried_missing")
	case common.HeadFuture:
		n.countHeadRefusal(v.FutureRule())
	}
}

// inHeadRegion reports whether v's block is in the head region of the
// Latest it was decided against: Latest − reorgWindow < N ≤ Latest, the
// chain tracker's window. With the head unknown no block was promised.
func (n *Network) inHeadRegion(v *common.HeadRefusal) bool {
	return v.Latest > 0 && n.chainTracker != nil && v.Block <= v.Latest && v.Block > v.Latest-int64(n.chainTracker.ReorgWindow())
}

// isNullResult reports whether resp's result is JSON null.
func isNullResult(ctx context.Context, resp *common.NormalizedResponse) bool {
	if resp == nil {
		return false
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	return err == nil && jrr != nil && isJSONNull(jrr.GetResultBytes())
}

// refusalMiss reports whether resp, an emptyish answer of method for a
// block above Firm, is a node's miss of it (evm.RefusalMiss), not the
// block's empty answer.
func refusalMiss(ctx context.Context, method string, resp *common.NormalizedResponse) bool {
	if !isEmptyNetworkResult(method, resp) {
		return false
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	return err != nil || jrr == nil || evm.RefusalMiss(method, jrr.GetResultBytes())
}

// rangeHeld reports whether u holds the whole range of method, a range
// query, by its evidence reaching toBlock, the refusal verdict's block
// (holdsBlock): its empty answer is then no miss of the range's blocks
// (spec §6.4), and an eth_getLogs one goes to the logs gate. Retried as a
// miss, an empty block's logs waited W for an untried holder (§19 P23:
// eth_getLogs p99 2.07 s).
func (n *Network) rangeHeld(method string, u common.Upstream, refusal *common.HeadRefusal) bool {
	if refusal == nil || refusal.Block <= 0 || !evm.IsRangeQuery(method) {
		return false
	}
	return n.holdsBlock(u, refusal.Block)
}

// holdsBlock reports whether u's evidence (reachedHead, uncredited)
// reaches block.
func (n *Network) holdsBlock(u common.Upstream, block int64) bool {
	head := n.reachedHead(u, 0)
	return head > 0 && common.EvmHeadReaches(u, head, block)
}

// logsGate is one leg's logs gate on the empty eth_getLogs answers it
// would serve (spec §6.4 logs gate): its last verdict, and the first
// holder whose empty answer of a range that may hold a matching log was
// a miss.
type logsGate struct {
	verdict logsVerdict
	empty   string
}

// logsVerdict is the logs gate's last verdict on a leg's empty answers.
type logsVerdict uint8

const (
	logsUnjudged logsVerdict = iota // no empty answer reached the gate
	logsNo                          // the pinned headers prove no matching log: served
	logsSweeping                    // a miss: the leg sweeps
	logsAgreed                      // a second holder answered empty: served
)

// logsGateMiss is the logs gate on u's empty eth_getLogs answer of req,
// one the refusal rule would serve (spec §6.4 logs gate): it is the
// range's answer when every pinned block of it proves no matching log
// (ChainTracker.LogsMayExist), or when u is the second upstream whose
// evidence holds the block that may (holdsBlock) to answer it empty;
// else a miss of that block, which the leg sweeps. A filter with no
// concrete range, one below the pins, and a network without a tracker
// pass as before, unjudged.
func (n *Network) logsGateMiss(req *common.NormalizedRequest, u common.Upstream, g *logsGate) (int64, bool) {
	t := n.laneTracker(selectorOf(req))
	if t == nil {
		return 0, false
	}
	jrq, err := req.JsonRpcRequest()
	if err != nil {
		return 0, false
	}
	jrq.RLock()
	f := logfilter.Parse(jrq.Params)
	jrq.RUnlock()
	if f == nil {
		return 0, false
	}
	from, to, ok := f.ConcreteRange()
	if !ok {
		return 0, false
	}
	block, verdict := t.LogsMayExist(f, from, to)
	switch {
	case verdict == evm.LogsUnheld:
		return 0, false
	case verdict == evm.LogsNone:
		g.verdict = logsNo
		return 0, false
	case !n.holdsBlock(u, block):
	case g.empty == "":
		g.empty = u.Id()
	case g.empty != u.Id():
		g.verdict = logsAgreed
		return 0, false
	}
	g.verdict = logsSweeping
	return block, true
}

// countLogsGate counts a leg's logs gate verdict as the leg ends with out
// or outErr (MetricLogsEmptyGateTotal): a sweep that served method's logs
// found them, one that served none left the range unavailable.
func (n *Network) countLogsGate(g *logsGate, method string, out *common.NormalizedResponse, outErr error) {
	var v string
	switch g.verdict {
	case logsUnjudged:
		return
	case logsNo:
		v = "no"
	case logsAgreed:
		v = "maybe_agreed"
	case logsSweeping:
		v = "maybe_unavailable"
		if outErr == nil && out != nil && !out.IsObjectNull() && !isEmptyNetworkResult(method, out) {
			v = "maybe_found"
		}
	}
	telemetry.CounterHandle(telemetry.MetricLogsEmptyGateTotal, n.projectId, n.Label(), v).Inc()
}

func (n *Network) countHeadRefusal(rule string) {
	telemetry.CounterHandle(telemetry.MetricHeadRefusalTotal, n.projectId, n.Label(), rule).Inc()
}

// missingDataErr reports whether err says an upstream lacks the block
// (spec §6.4): a missing-data error, or a range refused per upstream.
func missingDataErr(err error) bool {
	return common.HasErrorCode(err, common.ErrCodeEndpointMissingData, common.ErrCodeUpstreamBlockUnavailable)
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
// Same rationale as SvmHighestIndexedSlot and the EVM Head's ceiling: never
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
// the tip, and backing: the lowest of those constraining subsets' highest
// heads, which bounds the finalized floor (holdFinalized). Two kinds of
// subset, one rule (guaranteedSubsetPick):
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
func (n *Network) guaranteedFloor(sel string, useFinalized bool) (floor, backing int64) {
	if n.cfg == nil || n.cfg.Evm == nil || n.cfg.Evm.ServedTip == nil {
		return 0, 0
	}
	st := n.cfg.Evm.ServedTip
	fold := func(p common.ServedTipPick) {
		if p.Tip <= 0 {
			return
		}
		if floor == 0 || p.Tip < floor {
			floor = p.Tip
		}
		if backing == 0 || p.Max < backing {
			backing = p.Max
		}
	}
	for _, g := range st.GuaranteedFor {
		fold(n.guaranteedSubsetPick(sel, useFinalized, func(u common.Upstream) bool {
			m, _ := common.UpstreamMatchesSelector(g, u)
			return m
		}))
	}
	for _, m := range st.GuaranteedMethods {
		fold(n.guaranteedSubsetPick(sel, useFinalized, func(u common.Upstream) bool {
			handle, _ := u.ShouldHandleMethod(m)
			return handle
		}))
	}
	return floor, backing
}

// guaranteedSubsetPick is the majority pick of the guarantee subset `member`
// selects, or the zero pick when that subset sets no floor this evaluation.
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
func (n *Network) guaranteedSubsetPick(sel string, useFinalized bool, member func(common.Upstream) bool) common.ServedTipPick {
	pick, live := n.evmTipBallot(tipScope{sel: sel}, useFinalized, common.ServedTipQuorumMajority, member)
	if pick.Inputs == 0 || (!live && n.anyRegisteredLiveHead(member, useFinalized)) {
		return common.ServedTipPick{}
	}
	return pick
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

// laneGauges are one lane's served-tip gauges, bound once to their labels,
// so a computation sets them with no label lookup. axis ("latest" |
// "finalized") is a label rather than part of the metric name to keep the
// gauge set small. lane="all" is the network-wide lane; a named lane is a
// use-upstream group's own. The latest axis is the view's Head, its lag
// behind the ballot's Freshest while the lane has demand, else 0.
type laneGauges struct {
	latest, finalized servedTipGauges
}

type servedTipGauges struct {
	block, lag, advanceAge prometheus.Gauge
}

func (n *Network) bindLaneGauges(lane string) *laneGauges {
	axis := func(axis string) servedTipGauges {
		return servedTipGauges{
			block:      telemetry.MetricNetworkServedTipBlockNumber.WithLabelValues(n.projectId, n.Label(), lane, axis),
			lag:        telemetry.MetricNetworkServedTipLagBlocks.WithLabelValues(n.projectId, n.Label(), lane, axis),
			advanceAge: telemetry.MetricNetworkServedTipAdvanceAgeSeconds.WithLabelValues(n.projectId, n.Label(), lane, axis),
		}
	}
	return &laneGauges{latest: axis("latest"), finalized: axis("finalized")}
}

// observe exports one axis of a lane's pick.
func (g *servedTipGauges) observe(tip, freshest int64, advanceAge time.Duration) {
	if tip > 0 {
		g.block.Set(float64(tip))
		// Deliberate lag: how far the served tip sits behind the corroborated
		// freshest upstream view in the same set. Clamped at 0 because the tip
		// can legitimately sit ABOVE that view while the regression guard
		// holds its last corroborated pick, and a lag gauge that reads
		// negative is a broken dashboard, not a signal; the hold has its own
		// counter (…_regression_total).
		g.lag.Set(float64(max(freshest-tip, 0)))
	}
	// Universal stuck-tip signal: seconds since this process last saw the
	// served value change. Skipped until a first change is observed.
	if advanceAge >= 0 {
		g.advanceAge.Set(advanceAge.Seconds())
	}
}

// consensusPolicySelectorEnabled reports whether ANY failsafe executor on this
// network runs a pre-round customPolicy selector. When none do, the request
// path must not build selector-only inputs. Scans the pre-built executors
// (no per-request finality resolution, so no I/O).
func (n *Network) consensusPolicySelectorEnabled() bool {
	for _, fe := range n.failsafeExecutors {
		if fe != nil && fe.consensus != nil && fe.consensus.HasSelector() {
			return true
		}
	}
	return false
}
func (n *Network) getFailsafeExecutor(ctx context.Context, req *common.NormalizedRequest) *networkExecutor {
	if req.UpstreamPolicies() == common.UpstreamPoliciesTimeoutOnly {
		return n.bareExecutor
	}
	method, _ := req.Method()
	finality := req.Finality(ctx)

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

		if methodMatches && finalityMatches && commitmentMatches {
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
			if common.IsTracingDetailed() && len(laneIDs) > 0 {
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
	if common.IsTracingDetailed() {
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
// eligible: for bn, the block the client named (routingBlockNumber, read
// before a tag is pinned to the Head, or the Head itself in the thin
// region), upstreams that may hold it (mayHold: known to, or in the thin
// region most likely to, thinHolds) first. A read of the Head's own block,
// numbered or by tag (a miss of the cache seed at a fresh Head), takes
// those by their measured round trip (byRoundTrip): any holder serves it,
// so the fastest serves it soonest. With tag, a client read of latest sent
// as the tag (bn its Head's Latest), only an upstream whose evidence shows
// it behind bn goes last (tagHolds): its answer is its own head, served
// once the view takes it. Then failover tiers, then voters with a stale
// hash vote, and voters on a minority fork last.
func (n *Network) preferUpstreams(ctx context.Context, req *common.NormalizedRequest, method string, upsList []common.Upstream, bn int64, tag bool) []common.Upstream {
	if n.Architecture() == common.ArchitectureEvm && len(upsList) > 1 && (bn > 0 || evm.ReadsBlock(method)) {
		head := n.requestHead(ctx, req)
		headBlock := head.Latest > 0 && (bn == head.Latest || bn == 0 && evm.ReadsBlock(method) && evm.TagBlockNumber(ctx, req, "latest") == head.Latest)
		var holds func(common.Upstream) bool
		front, back := upsList, []common.Upstream(nil)
		if bn > 0 {
			if tag {
				holds = n.tagHolds(req, bn)
			} else {
				holds = n.thinHolds(req, head, bn)
			}
			front, back = n.splitHolders(upsList, bn, creditCap(head), holds)
		}
		if headBlock {
			front = slices.Clone(front)
			n.byRoundTrip(front)
		}
		upsList = append(front, back...)
		if bn > 0 {
			upsList = n.preferTipLeaderForNearTipGetBlock(upsList, method, bn, head.Leader, head.Latest, holds)
		}
	}

	// Failover tiering: try default-tier upstreams before fallback-tier ones,
	// keeping score order within each tier.
	if n.cfg.Failover.Enabled() {
		upsList = tierUpstreamsByGroup(upsList)
	}
	return n.lateLast(upsList)
}

// headFamilyMethod is the method whose answers time an upstream's round
// trip for byRoundTrip: the clock's fetches and the client reads of a
// block at or near the head both send it, so every upstream that serves
// either has a sample, and its small fixed answer times the link, not
// the work.
const headFamilyMethod = "eth_getBlockByNumber"

// byRoundTrip sorts ups in place by the median latency of their
// headFamilyMethod answers the metrics tracker keeps, fastest first;
// upstreams with no sample go after the others, and equals keep their
// order. Only answers with a block count (the tracker leaves out errors
// and empty answers), so a fast refusal never makes an upstream fastest.
func (n *Network) byRoundTrip(ups []common.Upstream) {
	if n.metricsTracker == nil {
		return
	}
	type timed struct {
		u common.Upstream
		d time.Duration
	}
	by := make([]timed, len(ups))
	for i, u := range ups {
		d := n.metricsTracker.GetUpstreamMethodMetrics(u, headFamilyMethod, common.DataFinalityStateAll).ResponseQuantiles.GetQuantile(0.5)
		if d <= 0 {
			d = time.Duration(math.MaxInt64)
		}
		by[i] = timed{u, d}
	}
	slices.SortStableFunc(by, func(a, b timed) int { return cmp.Compare(a.d, b.d) })
	for i, t := range by {
		ups[i] = t.u
	}
}

// thinHolds is, for bn in head's thin region (Firm < bn ≤ Latest), which
// upstreams most likely hold bn although none of their evidence shows it
// yet (likelyHolds). It orders a request's first attempt only: Firm, and
// the refusal rule's retry (headFirst), keep to evidence, so a rare miss
// is that retry's. nil outside the thin region.
func (n *Network) thinHolds(req *common.NormalizedRequest, head *common.EvmHead, bn int64) func(common.Upstream) bool {
	if head.Firm <= 0 || bn <= head.Firm || bn > head.Latest {
		return nil
	}
	return n.likelyHolds(req, bn)
}

// likelyHolds is which upstreams most likely hold bn: those whose thin hold
// wait passed since bn's first sight (ChainTracker.HoldsLikely), measured
// on every answer of theirs, client reads sent as the tag included; nil
// without a lane tracker.
func (n *Network) likelyHolds(req *common.NormalizedRequest, bn int64) func(common.Upstream) bool {
	t := n.laneTracker(selectorOf(req))
	if t == nil {
		return nil
	}
	now := time.Now()
	return func(u common.Upstream) bool { return t.HoldsLikely(u.Id(), uint64(bn), now) }
}

// tagHolds is, for a client read of latest sent as the tag at bn, its
// Head's Latest, which upstreams count as reaching bn beyond their
// credited head: those most likely holding it (likelyHolds), and those
// whose newest report found them at the network head (Tracker.HeadLag 0),
// whose counter is only stale: a credit's time bound left a vendor read
// moments ago a block short, and its 80-110 ms peers took at least 10% of
// reads (P20-zora-lag, p90 99 ms against 7 ms). A report that found it behind
// (unichain-testnet's fastest vendor, 5 blocks) keeps it last.
func (n *Network) tagHolds(req *common.NormalizedRequest, bn int64) func(common.Upstream) bool {
	likely := n.likelyHolds(req, bn)
	return func(u common.Upstream) bool {
		if n.metricsTracker != nil {
			if lag, ok := n.metricsTracker.HeadLag(u); ok && lag == 0 {
				return true
			}
		}
		return likely != nil && likely(u)
	}
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

	if common.IsTracingDetailed() {
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

	// Route safe-tagged requests before multiplexing and cache lookup; the
	// project already did for its requests, this covers direct callers.
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
	// and hedge, and the response all see the same latest. The block the
	// client named is read before that: a tag pinned to the Head names no
	// block, so it orders no upstream by head (§6.1).
	var namedBlock int64
	if n.cfg.Architecture == common.ArchitectureEvm {
		n.requestHead(ctx, req)
		namedBlock = n.routingBlockNumber(ctx, req)
	}
	if err := n.prepareRequest(ctx, req); err != nil {
		common.SetTraceSpanError(forwardSpan, err)
		return nil, err
	}
	forwardSpan.SetAttributes(attribute.String("request.finality", req.Finality(ctx).String()))

	// Selector-enabled requests must not share in-flight results across
	// callers: the multiplex hash omits the consensus policy and the caller's
	// auth, so a strict caller could receive a concurrent fallback /
	// waive-missing result. Keep multiplexing disabled under selector.
	//
	// Cache Get/Set reopen under selector: write-side admission
	// (cachingStrategy × response mark from Consensus.Run) keeps weaker
	// grades out of the store. Keys still omit policy+auth. Under selector,
	// Forward also skips Set when the mark is nil so unmarked no-consensus
	// routes cannot poison the shared store for later selector reads
	// (defaultAllow would otherwise admit nil). Pre-feature warm entries
	// still Get until TTL — flush/rotate the connector when first enabling
	// customPolicy on a previously caching network.
	selectorOn := n.consensusPolicySelectorEnabled()

	var mlx *Multiplexer
	var resp *common.NormalizedResponse
	var err error
	if !selectorOn {
		mlx, resp, err = n.handleMultiplexing(ctx, &lg, req, startTime)
	}
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
		} else if _, off := evm.OffViewBlock(ctx, req, resp, req.EvmHead()); resp != nil && n.cfg.Architecture == common.ArchitectureEvm && off {
			// A cached block at the view's number from another branch is a
			// miss, as an upstream's is (spec §7 family 2).
			resp.Release()
		} else if resp != nil && !resp.IsObjectNull(ctx) {
			if lg.GetLevel() <= zerolog.DebugLevel {
				lg.Debug().Object("response", resp).Msgf("response served from cache")
			} else {
				lg.Info().Msgf("response served from cache")
			}
			if n.cfg.Architecture == common.ArchitectureEvm {
				resp, _ = n.holdByHash(ctx, req, resp)
			}
			if mlx != nil {
				mlx.Close(ctx, resp, err)
			}
			forwardSpan.SetAttributes(attribute.Bool("cache.hit", true))
			return resp, err
		}
		forwardSpan.SetAttributes(attribute.Bool("cache.hit", false))
	}

	failsafeExecutor := n.getFailsafeExecutor(ctx, req)
	if failsafeExecutor == nil {
		return nil, errors.New("no failsafe executor found for this request")
	}
	// A client read of latest goes upstream as the tag, each attempt its
	// upstream's poll of latest (spec §6.4 client read).
	tagReads := n.cfg.Architecture == common.ArchitectureEvm && n.tagsLatestRead(ctx, req, failsafeExecutor.HasConsensus())

	upsList := n.selectionOrder(ctx, req, method)

	if len(upsList) == 0 {
		err := common.NewErrNoUpstreamsFound(n.projectId, n.networkId)
		common.SetTraceSpanError(forwardSpan, err)
		if mlx != nil {
			mlx.Close(ctx, nil, err)
		}
		return nil, err
	}

	// A read of latest sent as the tag reaches the Head's block when its
	// upstream's head does: upstreams whose evidence shows them behind go
	// last, the rest by round trip (spec §6.4 client read).
	if tagReads {
		namedBlock = n.requestHead(ctx, req).Latest
	}
	upsList = n.preferUpstreams(ctx, req, method, upsList, namedBlock, tagReads)

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

	// Set upstreams on the request. Candidates are the post-selection list;
	// the registry snapshot is kept separately so the consensus-policy
	// selector can still see punishment/operator cordons that removeCordoned
	// already dropped from upsList. Method-ineligible upstreams are filtered
	// out of that snapshot so a healthy ignoreMethods match cannot keep
	// allUnavailable()/canServeBlock() from selecting fallback.
	req.SetUpstreams(upsList)
	// Only the consensus-policy selector reads this snapshot; building it for
	// every request contradicts the "zero added per-request cost" contract
	// when customPolicy is unset (review finding). Skip the copy + method
	// filter entirely unless any network executor has a customPolicy selector.
	if n.upstreamsRegistry != nil && n.consensusPolicySelectorEnabled() {
		reg := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
		all := make([]common.Upstream, 0, len(reg))
		for _, u := range reg {
			all = append(all, u)
		}
		if eligible, dropped := filterMethodEligible(all, method); dropped > 0 {
			all = eligible
		}
		req.SetNetworkUpstreams(all)
	}

	// The refusal rule decides the request's block once, before dispatch
	// (spec §6.4): a stale ceiling is Asked here, a near-future block waits
	// for the ceiling, a band block refreshes the voters below it. A
	// future block, with shortCircuitFutureBlocks, gets the truthful null
	// with no dispatch and no wait; it runs before rate limiting so it
	// consumes no permit, and before the probe-bus publish so it is never
	// mirrored to excluded upstreams. A request that ends during the wait
	// ends as any request whose context ends.
	if n.cfg.Architecture == common.ArchitectureEvm {
		// A block the view pins waits for its likeliest holder instead
		// (spec §6.4 held): its first attempt goes there. A read of
		// latest sent as the tag waits for none: any upstream answers its
		// own head, the fastest first (spec §6.4 client read).
		if v := evm.HeadRefusalOf(ctx, req); v != nil && v.Held && !tagReads {
			req.SetUpstreams(n.awaitHolder(ctx, req, req.Upstreams(), v.Block, false))
		}
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				err = common.NewErrNetworkRequestTimeout(time.Since(startTime), err)
			}
			common.SetTraceSpanError(forwardSpan, err)
			if mlx != nil {
				mlx.Close(ctx, nil, err)
			}
			return nil, err
		}
	}
	if resp, ok := n.tryShortCircuitFutureBlock(ctx, req); ok {
		forwardSpan.SetAttributes(attribute.Bool("future_block.short_circuit", true))
		n.countRefusal(ctx, req, resp, nil)
		if mlx != nil {
			mlx.Close(ctx, resp, nil)
		}
		return resp, nil
	}
	if err := n.refuseFutureRange(req, method); err != nil {
		common.SetTraceSpanError(forwardSpan, err)
		if mlx != nil {
			mlx.Close(ctx, nil, err)
		}
		return nil, err
	}
	// The tag-state decision (spec §6.11) is made once, from the routed
	// upstreams, before the first attempt: every retry and hedge reads it.
	if n.cfg.Architecture == common.ArchitectureEvm {
		if err := evm.DecideTagState(ctx, n, req); err != nil {
			common.SetTraceSpanError(forwardSpan, err)
			if mlx != nil {
				mlx.Close(ctx, nil, err)
			}
			return nil, err
		}
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

	// Add tracing for which failsafe policy was selected
	forwardSpan.SetAttributes(
		attribute.String("failsafe.matched_method", failsafeExecutor.MatchMethod()),
		attribute.String("failsafe.matched_finalities", fmt.Sprintf("%v", failsafeExecutor.MatchFinality())),
	)

	// Build the per-execution upstream-loop closure. This is what the new
	// network executor invokes (potentially multiple times for retry/hedge,
	// and per-slot for consensus).
	sweepFn := func(execSpanCtx context.Context, effectiveReq *common.NormalizedRequest, oneUpstreamOnly bool) (out *common.NormalizedResponse, outErr error) {
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

		if common.IsTracingDetailed() {
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
		// leg is this execution to the refusal rule: it gets its own one
		// retryable miss, whatever its sibling slots and hedges got (spec
		// §6.4).
		leg := &common.RefusalLeg{}
		// sweep is this leg's band or thin retries of its block (spec
		// §6.4 band sweep), the logs gate's misses included.
		var sweep bandSweep
		// gate is this leg's logs gate on its empty eth_getLogs answers
		// (spec §6.4 logs gate), counted once as the leg ends.
		var gate logsGate
		defer func() { n.countLogsGate(&gate, method, out, outErr) }()
		// tag: this leg's attempts send the "latest" tag, until one
		// answer the view did not take; then they read the Head's block
		// by number (spec §6.4 client read).
		tag := tagReads
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

				sent := time.Now()
				var read *latestRead
				fwdCtx := loopCtx
				if tag {
					fwdCtx, read = n.startLatestRead(loopCtx, effectiveReq, u, sent)
				}
				r, err := tryForward(u, effectiveReq, fwdCtx, &ulg, hedges, attempts, snap.Retries)
				if e := n.normalizeResponse(loopCtx, effectiveReq, r); e != nil {
					ulg.Error().Err(e).Msgf("failed to normalize response")
					err = e
				}
				effectiveReq.MarkUpstreamCompleted(loopCtx, u, r, err)
				readServed := false
				readMissed := false
				if read != nil {
					readServed = n.finishLatestRead(loopCtx, effectiveReq, read, r, err)
					readMissed = err == nil && !readServed
				}

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

				// A read of latest whose answer the view did not take
				// serves the Head's block: the cache's seed of it, else a
				// read by number of a holder (spec §6.4 client read). The
				// answer is never served nor kept as a fallback answer. It
				// is no result either: u stays selectable, a holder by its
				// evidence like any other for the read by number, and for
				// the network's retry (celo: its one holder stayed consumed
				// and the read failed, shadow trace f32c77ce).
				if readMissed {
					if r != nil {
						effectiveReq.ClearLastValidResponseIf(r)
					}
					if cr := n.cachedHeadBlock(loopCtx, effectiveReq); cr != nil {
						loopSpan.End()
						return cr, nil
					}
					tag = false
					lastErr = common.NewErrEndpointMissingData(fmt.Errorf("upstream's latest block is not in the view"), u)
					effectiveReq.MarkUpstreamCompleted(loopCtx, u, nil, lastErr)
					delete(attempted, u.Id())
					nextUpstream = n.headFirst(loopCtx, effectiveReq, attempted, effectiveReq.EvmHead().Latest)
					maxLoopIterations = max(maxLoopIterations, loopIteration+1+effectiveReq.UpstreamsCount())
					loopSpan.End()
					continue
				}

				// One view (spec §6.5, §7 family 2): a block at the
				// request's Head on another branch than the Head's hash is
				// never served under the Head's number; it is a miss, unless
				// the view moves onto it. A read of latest the view took is
				// on the view already.
				if err == nil && !readServed {
					if c, off := evm.OffViewBlock(loopCtx, effectiveReq, r, effectiveReq.EvmHead()); off && !n.awaitViewOnto(loopCtx, effectiveReq, c) {
						r, err = nil, common.NewErrEndpointMissingData(fmt.Errorf("upstream answered another branch's block at the head"), u)
					}
				}

				// The refusal rule (spec §6.4): an empty or missing answer for a
				// block above Firm is the leg's one retryable miss, in a
				// consensus slot or a hedge pinned to one upstream too; the
				// retry asks the upstreams whose head reaches the block first.
				// The answer after it goes to the client as the upstream gave it.
				// An empty range from an upstream whose evidence reaches its
				// toBlock is the range's answer (rangeHeld). From one without
				// that evidence it is no answer, never served nor kept: a
				// node lacking the block answers it empty too, and an indexer
				// served that loses the block's logs.
				refusal := effectiveReq.HeadRefusal()
				emptyAnswer := err == nil && r != nil && !r.IsObjectNull() && refusalMiss(loopCtx, method, r) && !n.rangeHeld(method, u, refusal)
				if emptyAnswer && refusal.AboveFirm() && evm.IsRangeQuery(method) {
					effectiveReq.ClearLastValidResponseIf(r)
					r, err, emptyAnswer = nil, common.NewErrUpstreamBlockUnavailable(u.Id(), refusal.Block, upstreamLatestBlock(u), 0), false
				}
				missing := emptyAnswer || missingDataErr(err)
				// A held block's answers, served or missing, measure how
				// long after its first sight u holds it (spec §6.4 held).
				if refusal != nil && refusal.Held && (err == nil || missing) {
					if t := n.laneTracker(selectorOf(effectiveReq)); t != nil {
						t.ObserveHold(u.Id(), uint64(refusal.Block), sent, !missing)
					}
				}
				finalEmpty := false
				if refusal.AboveFirm() && missing {
					verdict := refusal.Miss(leg)
					if !refusal.Held && (refusal.Rule == common.HeadBand || refusal.Rule == common.HeadThin) {
						// A band or thin miss sweeps the routed upstreams
						// at once, then waits for one that shows the
						// block, at most W (spec §6.4 band sweep).
						verdict = common.MissFinal
						if next := n.bandNext(loopCtx, effectiveReq, attempted, &sweep, u, refusal.Block); next != nil {
							verdict, nextUpstream = common.MissRetry, next
						}
					} else if verdict == common.MissRetry {
						nextUpstream = n.headFirst(loopCtx, effectiveReq, attempted, refusal.Block)
					}
					switch verdict {
					case common.MissRetry:
						// One more pick for the retry, a slot's included.
						maxLoopIterations = max(maxLoopIterations, loopIteration+2)
						// With no upstream left to retry, the client gets
						// this answer as the upstream gave it: an empty
						// answer stays the result, never a missing-data
						// error made up from it.
						if r != nil && !r.IsObjectNull() {
							bestResp = r
						} else {
							lastErr = err
							common.SetTraceSpanError(loopSpan, err)
						}
						loopSpan.End()
						continue
					case common.MissFinal:
						if !emptyAnswer {
							common.SetTraceSpanError(loopSpan, err)
							loopSpan.End()
							return nil, err
						}
						finalEmpty = true
					}
				}

				// Return immediately when the result is usable:
				//  - Non-empty success always qualifies.
				//  - Emptyish success qualifies when the refusal rule calls it
				//    final; above Firm, when it is no miss of the block
				//    (refusalMiss: eth_call's 0x is its answer); below, when
				//    the method is in emptyResultAccept. Neither when
				//    consensus is required. Failsafe would accept the empty
				//    result anyway, so trying more upstreams just wastes time
				//    on slow ones.
				//  - An empty eth_getLogs qualifies only past the logs gate
				//    (spec §6.4 logs gate); else it is a miss, and the leg
				//    sweeps as for a band miss.
				//  - Otherwise emptyish results continue to the next upstream.
				if err == nil && r != nil && !r.IsObjectNull() {
					emptyish := isEmptyNetworkResult(method, r)
					acceptEmpty := !emptyish || finalEmpty ||
						(!failsafeExecutor.HasConsensus() && (refusal.AboveFirm() && !emptyAnswer ||
							!refusal.AboveFirm() && failsafeExecutor.acceptsEmptyResult(method, r)))
					if acceptEmpty && emptyish && method == "eth_getLogs" {
						if block, miss := n.logsGateMiss(effectiveReq, u, &gate); miss {
							effectiveReq.ClearLastValidResponseIf(r)
							lastErr = common.NewErrUpstreamBlockUnavailable(u.Id(), block, upstreamLatestBlock(u), 0)
							common.SetTraceSpanError(loopSpan, lastErr)
							loopSpan.End()
							if refusal.AboveFirm() {
								refusal.Miss(leg)
							}
							next := n.bandNext(loopCtx, effectiveReq, attempted, &sweep, u, block)
							if next == nil {
								return nil, lastErr
							}
							nextUpstream = next
							maxLoopIterations = max(maxLoopIterations, loopIteration+2)
							continue
						}
					}
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

	held := false
	if n.cfg.Architecture == common.ArchitectureEvm {
		resp, held = n.holdByHash(ctx, req, resp)
	}
	if resp != nil {
		// Under a customPolicy selector the store is shared across grades and
		// with no-consensus routes on the same network (keys omit policy/auth).
		// Consensus.Run always stamps an explicit mark; unmarked (nil) means no
		// module voted — typically a sibling failsafe entry without consensus.
		// Skip Set so defaultAllow cannot admit those into the selector-shared
		// store. Cache.Set still applies strategy × mark for stamped responses.
		skipUnmarkedSelectorSet := selectorOn && resp.Cacheable() == nil
		if n.cacheDal != nil && !held && !skipUnmarkedSelectorSet && !(tagReads && servesAnotherBlock(ctx, req, resp)) {
			n.storeInCache(ctx, forwardSpan.SpanContext(), &lg, method, req, resp)
		} else if skipUnmarkedSelectorSet {
			lg.Debug().Msg("skipping cache write: unmarked response on selector-enabled network")
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

		// The block the request targets, resolved once and only if an upstream missed.
		var targetBlock int64
		targetResolved := false

		// If response is not empty, but at least one upstream responded empty we track in a metric.
		// Derived from ErrorsByUpstream entries with ErrEndpointMissingData code.
		winner := resp.Upstream()
		req.ErrorsByUpstream.Range(func(key, value any) bool {
			// The winner's own earlier empty answer was a retry that later succeeded.
			if key == winner {
				return true
			}
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
			if !targetResolved {
				targetBlock, targetResolved = n.evmTargetBlock(ctx, req, resp), true
			}
			// An upstream whose head was below the target block when it answered
			// was behind, or the data did not exist yet (e.g. a tx not yet mined).
			// An unknown head (0) proves neither, so it still counts.
			if head := req.HeadAtError(upstream); targetBlock > 0 && head > 0 && head < targetBlock {
				return true
			}
			// A miss outside the range an operator declared for this upstream
			// (blockAvailability) is expected, never misbehavior. A request by
			// block hash names no number, so a bounded upstream (a pruned full
			// node) cannot be shown to have missed data it should hold.
			minBound, maxBound := upstream.EvmBlockAvailabilityBounds()
			if (minBound != math.MinInt64 || maxBound != math.MaxInt64) &&
				(targetBlock <= 0 || targetBlock < minBound || targetBlock > maxBound) {
				return true
			}
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
	// An interpolated finalized tag runs at the request Head's Finalized
	// (spec §7), the number its cache key holds (ResolveCacheBlockRef): its
	// answer is final as that number's is, not realtime as a tag that
	// reaches upstreams. "latest" stays realtime (#560).
	if blockRef == "finalized" {
		if num := evm.TagBlockNumber(ctx, req, blockRef); num > 0 {
			blockRef, blockNumber = strconv.FormatInt(num, 10), num
		}
	}

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

	if blockNumber <= 0 {
		return finality
	}
	// Reads decide on the request's Head.Finalized (spec §6.4). An answer
	// from an upstream is final only when that upstream has finalized the
	// block too (#90) and is not on a minority fork (§6.3), so neither
	// makes a permanent cache entry. A read above Finalized polls nothing:
	// only the full poll and the finalized-tag refresh move it.
	fin := n.EvmHead(ctx, req).Finalized
	span.SetAttributes(attribute.Int64("head.finalized", fin))
	if fin <= 0 {
		return finality
	}
	if blockNumber > fin {
		return common.DataFinalityStateUnfinalized
	}
	if resp != nil {
		if u := resp.Upstream(); u != nil {
			if n.isForkDemoted(u) {
				return common.DataFinalityStateUnfinalized
			}
			if ups, ok := u.(common.EvmUpstream); ok {
				if final, err := ups.EvmIsBlockFinalized(ctx, blockNumber, false); err != nil || !final {
					return common.DataFinalityStateUnfinalized
				}
			}
		}
	}
	return common.DataFinalityStateFinalized
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

	// When the block is slightly ahead (retryable), the skip is the lane's
	// read ahead of it (spec §6.4, §6.8): the clock's shared early fetch
	// brings its holders, the request never waits on it, and the retry
	// after blockUnavailableDelay sees their heads. A block at or below the
	// request Head's Latest, or one the view pins (Held), asks nothing more:
	// the clock already fetched it for the network. An Ask per skipped
	// upstream cost a block up to one fetch per voter.
	if isRetryable {
		if r := req.HeadRefusal(); r != nil && r.Held {
			return
		}
		if bn := requestBlockNumber(ctx, req); bn > n.requestHead(ctx, req).Latest {
			n.readAhead(selectorOf(req), bn)
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
//   - interpolation sends "latest"/"finalized" tags as the request's Head number.
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
	// A tag request whose tag-state decision says u's "latest" state is
	// stale (spec §6.11): its own error, so it never reads as -32014.
	if evm.TagStateSkip(n, u, req) {
		return common.NewErrUpstreamTagStateStale(u.Id()), false
	}
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

	// Above the configured upper bound. Normalization pins "latest"/"finalized"
	// to a number, so the target never moves; waiting helps only when the bound
	// itself moves and the gap is within MaxRetryableBlockDistance. A bound
	// that follows the upstream's head reads the head routing reads
	// (mayHold): a counter polled every 10s is blocks stale, so ordering
	// put a credited upstream first that a gate on the counter alone then
	// skipped. One that still lacks the block answers missing, and the
	// refusal rule's retry takes the next.
	var upper *common.EvmAvailabilityBoundConfig
	if cfg := eu.Config(); cfg != nil && cfg.Evm != nil && cfg.Evm.BlockAvailability != nil {
		upper = cfg.Evm.BlockAvailability.Upper
	}
	if maxBound != math.MaxInt64 && bn > maxBound && upper != nil && upper.LatestBlockMinus != nil {
		head := n.requestHead(ctx, req)
		reached := n.reachedHead(u, creditCap(head))
		if holds := n.thinHolds(req, head, bn); holds != nil && holds(u) {
			reached = max(reached, bn)
		}
		if lift := reached - upstreamLatestBlock(u); lift > 0 {
			maxBound += lift
		}
	}
	if maxBound != math.MaxInt64 && bn > maxBound {
		telemetry.MetricUpstreamStaleUpperBound.WithLabelValues(
			n.projectId, eu.VendorName(), n.Label(), eu.Id(), method, common.AvailbilityConfidenceBlockHead.String(),
		).Inc()
		latestBlock, finalizedBlock := n.upstreamHeads(eu)
		blockErr := common.NewErrUpstreamBlockUnavailable(eu.Id(), bn, latestBlock, finalizedBlock)
		return blockErr, common.UpperBoundSkipRetryable(bn, maxBound, n.maxRetryableBlockDistance(), upper.Moves(), false)
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
//
// The Use-Upstream directive is part of the key, so identical requests share
// only when they route alike: a state probe pinned to one upstream never
// shares with a client request routed elsewhere.
func (n *Network) multiplexKey(ctx context.Context, req *common.NormalizedRequest) (string, error) {
	var key string
	var err error
	if n.cfg != nil && n.cfg.Architecture == common.ArchitectureSvm {
		key, err = svm.RequestKey(ctx, req)
	} else {
		key, err = req.CacheHash()
	}
	if err != nil || key == "" {
		return key, err
	}
	if d := req.Directives(); d != nil && d.UseUpstream != "" {
		key += "|use-upstream:" + d.UseUpstream
	}
	// A client read of latest may serve a block above its Head's (spec
	// §6.4 client read): it shares only with reads of latest, never with
	// a read of its Head's number.
	if read, _ := evm.LatestBlockRead(ctx, req); read {
		key += "|latest"
	}
	return key, nil
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
		targeted := upsList
		if dr := req.Directives(); dr != nil && dr.UseUpstream != "" {
			targeted = nil
			for _, u := range upsList {
				if match, _ := common.UpstreamMatchesSelector(dr.UseUpstream, u); match {
					targeted = append(targeted, u)
				}
			}
		}
		// Forward keeps the full list when no upstream serves the method, so
		// this is "unsupported", not a multi-upstream problem.
		if len(targeted) > 0 {
			if eligible, _ := filterMethodEligible(targeted, method); len(eligible) == 0 {
				return common.NewErrNotImplemented(fmt.Sprintf("method %s is not supported by any targeted upstream", method))
			}
		}
		if len(targeted) > 1 {
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

// filterMethodEligible returns the upstreams expected to serve `method`
// (servesMethod), plus the number dropped. The input slice is never mutated —
// selection-policy slots hand out their cached backing array as read-only —
// and is returned as-is when nothing is dropped.
func filterMethodEligible(ups []common.Upstream, method string) ([]common.Upstream, int) {
	eligible := ups
	dropped := 0
	for i, u := range ups {
		if !servesMethod(u, method) {
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

// servesMethod reports whether u is expected to serve method ("" = any) per
// its ignoreMethods/allowMethods config (Upstream.ShouldHandleMethod, which is
// memoized per method). Matcher errors fail open (the upstream stays in) so a
// malformed pattern degrades to the forward-time skip rather than silently
// shrinking the pool.
func servesMethod(u common.Upstream, method string) bool {
	if method == "" {
		return true
	}
	ok, err := u.ShouldHandleMethod(method)
	return ok || err != nil
}

// evmTargetBlock is the block a request targets: the response's, or the
// request's own when the response names none (eth_call). 0 when unknown, as
// for a request by block hash.
func (n *Network) evmTargetBlock(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) int64 {
	if n.cfg.Architecture != common.ArchitectureEvm {
		return 0
	}
	if _, bn, err := evm.ExtractBlockReferenceFromResponse(ctx, resp); err == nil && bn > 0 {
		return bn
	}
	if _, bn, err := evm.ExtractBlockReferenceFromRequest(ctx, req); err == nil && bn > 0 {
		return bn
	}
	return 0
}

func (n *Network) enrichStatePoller(ctx context.Context, method string, req *common.NormalizedRequest, resp *common.NormalizedResponse) {
	ctx, span := common.StartDetailSpan(ctx, "Network.EnrichStatePoller")
	defer span.End()
	// A client answer is dated by its request's send time (spec §6.4); an
	// answer no upstream sent now carries no evidence of a head.
	sentAt := resp.SentAt()
	if sentAt.IsZero() {
		return
	}

	switch n.Architecture() {
	case common.ArchitectureEvm:
		// A by-number answer with data, or an empty one that is no miss
		// (refusalMiss: eth_call's 0x), shows its upstream holds the
		// block: holder evidence for Firm and routing (spec §6.5).
		if ups, ok := resp.Upstream().(common.EvmUpstream); ok && ups != nil {
			if bn := requestBlockNumber(ctx, req); bn > 0 && !resp.IsObjectNull(ctx) && !refusalMiss(ctx, method, resp) {
				n.noteHeld(ups, bn)
			}
		}
		// TODO Move the logic to evm package as a post-forward hook?
		if method == "eth_getBlockByNumber" {
			// sentTag is the tag the upstream resolved itself; blkTag the
			// request's own, which normalization keeps when it interpolates
			// the params to Head's number.
			jrq, err := req.JsonRpcRequest(ctx)
			if err != nil {
				return
			}
			var sentTag string
			jrq.RLock()
			if len(jrq.Params) > 0 {
				if s, ok := jrq.Params[0].(string); ok && (s == "latest" || s == "finalized") {
					sentTag = s
				}
			}
			jrq.RUnlock()
			blkTag := sentTag
			if s, ok := req.EvmBlockRef().(string); ok && (s == "latest" || s == "finalized") {
				blkTag = s
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
						// Only the upstream's own answer to the tag is its
						// finalized: an interpolated request asked for
						// Head.Finalized, eRPC's number, and serving that block
						// says nothing of what this upstream finalized (spec
						// §6.4, #90).
						if sentTag == "finalized" {
							ups.EvmStatePoller().SuggestFinalizedBlock(blockNumber, sentAt)
						}
					case "latest":
						// As finalized: an interpolated answer shows the
						// upstream has block N, not that its head is N.
						if sentTag == "latest" {
							ups.EvmStatePoller().SuggestLatestBlock(blockNumber, sentAt)
						}
						// An answer, never a vote: the tracker of the
						// request's group holds its header, in bytes of its
						// own, once per hash.
						selector := ""
						if d := req.Directives(); d != nil {
							selector = d.UseUpstream
						}
						if t := evm.ChainTrackerOf(ctx, n, selector); t != nil {
							t.Observe(ups.Id(), jrs.GetResultBytes(), false)
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
							ups.EvmStatePoller().SuggestLatestBlock(blockNumber, sentAt)
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

// headFirst picks req's retry above Firm (spec §6.4): its routed upstreams
// not yet attempted that NextUpstream may still select, ordered by their
// heads as they are now, those holding bn first, with no I/O; for a Held
// block, by when each most likely holds it, after waiting for the first
// (awaitHolder). Upstreams the policy did not route stay the fallback
// escape's.
func (n *Network) headFirst(ctx context.Context, req *common.NormalizedRequest, attempted map[string]struct{}, bn int64) func() (common.Upstream, error) {
	var rest []common.Upstream
	for _, u := range req.Upstreams() {
		if _, seen := attempted[u.Id()]; !seen {
			rest = append(rest, u)
		}
	}
	if r := req.HeadRefusal(); r != nil && r.Held {
		rest = n.awaitHolder(ctx, req, rest, bn, true)
	} else {
		rest = n.partitionUpstreamsByLatestBlock(rest, bn, creditCap(n.requestHead(ctx, req)), nil)
	}
	return func() (common.Upstream, error) {
		for len(rest) > 0 {
			u := rest[0]
			rest = rest[1:]
			if req.ReserveUpstream(u) {
				return u, nil
			}
		}
		return nil, common.NewErrNoUpstreamsLeftToSelect(req, "no upstream left for the refusal rule's retry")
	}
}

// creditCap is the highest head a credit may grant (creditedHead): Firm,
// the block q voters hold, else Latest. A credit says only that a head
// probably moved on, so it never makes a holder of a thin block (§6.5).
func creditCap(h *common.EvmHead) int64 {
	if h.Firm > 0 {
		return min(h.Firm, h.Latest)
	}
	return h.Latest
}

// partitionUpstreamsByLatestBlock moves the upstreams that may not hold
// block bn (mayHold) behind the rest, preserving order within each group
// (spec §6.1, §6.4, §8).
func (n *Network) partitionUpstreamsByLatestBlock(ups []common.Upstream, bn, credit int64, holds func(common.Upstream) bool) []common.Upstream {
	if bn <= 0 || len(ups) < 2 {
		return ups
	}
	front, back := n.splitHolders(ups, bn, credit, holds)
	return append(front, back...)
}

// splitHolders splits ups, in order, into the upstreams that may hold
// block bn (mayHold) and the rest.
func (n *Network) splitHolders(ups []common.Upstream, bn, credit int64, holds func(common.Upstream) bool) (front, back []common.Upstream) {
	front = make([]common.Upstream, 0, len(ups))
	for _, u := range ups {
		if n.mayHold(u, bn, credit, holds) {
			front = append(front, u)
		} else {
			back = append(back, u)
		}
	}
	return front, back
}

// mayHold reports whether u may hold block bn, the one predicate routing
// and the availability gate read (spec §6.4): its reachedHead reaches bn,
// or, when holds is set, holds says so (thinHolds). An upstream with no
// known head may hold any block.
func (n *Network) mayHold(u common.Upstream, bn, credit int64, holds func(common.Upstream) bool) bool {
	head := n.reachedHead(u, credit)
	return head <= 0 || common.EvmHeadReaches(u, head, bn) || holds != nil && holds(u)
}

// reachedHead is the head u reached as routing reads it (spec §6.1,
// §6.4): a push source's hash vote on this instance, as its requests ride
// this instance's socket, which may be half-open while the shared head
// counter moves; any other upstream's head counter credited up to credit
// (creditedHead); at least a by-number answer this instance took from it
// (heldAtLeast). 0 when unknown.
func (n *Network) reachedHead(u common.Upstream, credit int64) int64 {
	head := upstreamLatestBlock(u)
	if n.metricsTracker != nil && n.sources.pushSource(u.Id()) {
		head = n.metricsTracker.HashVoteNumber(u)
	} else {
		head = n.creditedHead(u, head, credit)
	}
	return max(head, n.heldAtLeast(u.Id()))
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
// have imported that block yet. Heads are read as the Leader was chosen
// (evmTipHead), the others credited up to latest (creditedHead) or, when
// holds is set, as holding bn when holds says so (thinHolds), so a fresher
// poll alone makes no leader.
func (n *Network) preferTipLeaderForNearTipGetBlock(ups []common.Upstream, method string, bn int64, leader common.Upstream, latest int64, holds func(common.Upstream) bool) []common.Upstream {
	if method != "eth_getBlockByNumber" || len(ups) < 2 || leader == nil {
		return ups
	}
	tip := evmTipHead(leader)
	if bn < tip || bn > tip+1 {
		return ups
	}
	at := -1
	for i, u := range ups {
		switch {
		case u == leader:
			at = i
		case n.creditedHead(u, evmTipHead(u), latest) >= tip, bn == tip && holds != nil && holds(u):
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

// creditedHead is head, u's head as read for ordering, plus the blocks the
// chain made since u's newest report beyond its lag then, never above
// latest (spec §6.4, #1352): a polled head ages by up to a poll interval,
// and read raw it put a fast upstream polled every 10s on a 2s chain behind
// a slow one polled more often. The report is u's head counter and its
// report time. A head that is unknown, at or above latest, capped by a
// configured exactBlock, or a push source's gets none, and so does a
// benched one (Tracker.HeadCredit). It orders only.
func (n *Network) creditedHead(u common.Upstream, head, latest int64) int64 {
	if head <= 0 || head >= latest || n.metricsTracker == nil || n.sources.pushSource(u.Id()) {
		return head
	}
	eu, ok := u.(common.EvmUpstream)
	if !ok {
		return head
	}
	sp := eu.EvmStatePoller()
	if sp == nil || sp.IsObjectNull() {
		return head
	}
	if _, capped := evmTipObservation(eu, false); capped {
		return head
	}
	return min(latest, head+n.metricsTracker.HeadCredit(u, sp.LatestBlock(), sp.LatestReportAt(), latest))
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

// evmTipHead is u's latest head as the Leader is chosen on it (voterSet,
// evmTipObservation), or 0 when unknown.
func evmTipHead(u common.Upstream) int64 {
	if eu, ok := u.(common.EvmUpstream); ok {
		head, _ := evmTipObservation(eu, false)
		return head
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

// routingBlockNumber is the block req names, for routing: requestBlockNumber,
// or for a read by block hash the number of the header the network-wide
// tracker holds; for an interpolated latest, the Head while it is thin
// (Firm < Head, spec §6.5), so the upstreams holding it go first; 0
// otherwise. Read it before prepareRequest pins a tag to the Head: a tag
// pinned to a Head q voters hold names no block (spec §6.1), and keeps the
// policy's order (P15).
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
	case ref == "latest":
		if h := n.requestHead(ctx, req); h.Firm > 0 && h.Latest > h.Firm && evm.TagBlockNumber(ctx, req, "latest") == h.Latest {
			return h.Latest
		}
	}
	return 0
}

// heldAtLeast is the highest block a by-number client answer from
// upstream id held on this instance (noteHeld); 0 before any.
func (n *Network) heldAtLeast(id string) int64 {
	if v, ok := n.heldAt.Load(id); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

// noteHeld records that u answered a by-number request at block bn with
// data: holder evidence for Firm and routing (spec §6.5), never a report
// of u's head. A rise above Firm wakes u's lanes' trackers.
func (n *Network) noteHeld(u common.Upstream, bn int64) {
	v, ok := n.heldAt.Load(u.Id())
	if !ok {
		v, _ = n.heldAt.LoadOrStore(u.Id(), new(atomic.Int64))
	}
	held := v.(*atomic.Int64)
	for cur := held.Load(); bn > cur; cur = held.Load() {
		if held.CompareAndSwap(cur, bn) {
			if h := n.headFor(tipScope{}); bn > h.Firm && bn <= h.Latest {
				evm.HoldersMoved(n, u)
			}
			return
		}
	}
}
