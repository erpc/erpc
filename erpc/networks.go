package erpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	bootstrapOnce        sync.Once
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

	// servedLatest / servedFinalized are STRICT-MONOTONIC at the network level:
	// once we serve a tip of N to clients, EvmHighestLatest/FinalizedBlockNumber
	// servedTipAnchor watchdogs track when this process last SAW the served
	// value change — purely for the advance-age stuck-tip gauge. The pick
	// itself is stateless (evm.PickServedTip); nothing here feeds back into
	// what clients receive.
	servedLatestAnchor    servedTipAnchor
	servedFinalizedAnchor servedTipAnchor

	// servedTipPartitions holds lazily-materialized per-group LANES for
	// use-upstream selectors that carve out a real sub-group (e.g.
	// `flashblocks*` / `family:systx`): a stable gauge label (LaneName) plus
	// watchdog anchors. TELEMETRY ONLY — the pick for any selector is the
	// same stateless majority over the matched subset. Bounded by
	// construction: keyed by the hash of the MATCHED UPSTREAM SET (equivalent
	// selectors dedup; garbage selectors can't inflate state beyond the real
	// topology) and capped by maxServedTipPartitions.
	servedTipPartitions     sync.Map // map[string]*servedTipPartition (key = "grp:<hash>")
	servedTipPartitionCount atomic.Int32

	// servedTipBlockTimeOverride, when > 0, replaces the tracker's EMA block
	// time in buildServedTipConfig. Set ONLY from package-internal tests: the
	// EMA needs live timestamped blocks that test fixtures don't produce, and
	// the prod-incident invariant tests must arm the velocity gate exactly the
	// way prod had it armed.
	servedTipBlockTimeOverride float64
}

// maxServedTipPartitions caps the number of materialized per-tag served-tip
// partitions per network (a backstop on top of the "must be a configured tag"
// gate). Realistic configs have 1–3 groups; beyond the cap, extra tags fall
// back to the stateless subset candidate.
const maxServedTipPartitions = 16

// servedTipPartition is one node group's telemetry lane: the stable gauge
// label (common.LaneName of the matched upstream set) and the group's
// watchdog anchors. It holds NO pick state.
type servedTipPartition struct {
	lane            string
	latestAnchor    servedTipAnchor
	finalizedAnchor servedTipAnchor
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
// pods. A persisted anchor plus a strict clamp is exactly what wedged the
// served tip fleet-wide in 2026-06 (see the picker's doc comment and
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

	// trajectory is this lane+axis's long-term head track — the referee that
	// lets a corroborated FRESH MINORITY outvote a stalled majority
	// (refereeServedTip). It holds no pick either: its samples are the plain
	// majority over live heads, its output is always one of the head values the
	// live upstreams offer in the same evaluation, and it can only ever raise
	// the pick. See evm.TipTrajectory.
	trajectory evm.TipTrajectory

	// refereeState is one of the servedTipReferee* constants: like guardState,
	// both the referee's counter and its log fire on state ENTRY only.
	refereeState atomic.Int32
}

const (
	// servedTipRefereeIdle: the referee is not intervening.
	servedTipRefereeIdle int32 = iota
	// servedTipRefereeOverriding: a corroborated group is being served in place
	// of the majority pick.
	servedTipRefereeOverriding
	// servedTipRefereeDeclining: a fresher corroborated group sat above the
	// majority pick and was refused — too far off the trajectory, or it has not
	// held its place long enough to have earned it.
	servedTipRefereeDeclining
)

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

// servedTipClock is the regression guard's clock, swapped in tests.
var servedTipClock = time.Now

// observe records v as the latest served value, stamping the anchor when the
// value changed since the last observation.
//
// LOAD FIRST: the served tip is read several times per request and is the same
// value on almost every one of them, so the steady state must be a shared READ
// of this cache line, not an exclusive read-modify-write from every serving
// core. The Swap survives only on the (rare) path where the value really moved,
// where it is what makes exactly one caller see the transition.
func (a *servedTipAnchor) observe(v int64) {
	if a.seenValue.Load() == v {
		return
	}
	if old := a.seenValue.Swap(v); old != v && old != 0 {
		a.changedAtMs.Store(time.Now().UnixMilli())
	}
}

// age returns time since the last observed value change, or -1 when no change
// has been observed yet (the advance-age gauge is skipped on -1).
func (a *servedTipAnchor) age() time.Duration {
	if ms := a.changedAtMs.Load(); ms > 0 {
		return time.Since(time.UnixMilli(ms))
	}
	return -1
}

// servedTipLaneAll is the lane label for the network-wide served tip.
const servedTipLaneAll = "all"

// servedTipLaneNone is the sentinel passed to clusteredServedTip for a
// stateless selector-scoped tip (glob/single-node/match-all that did not form a
// group): observeServedTipMetrics emits nothing for it, so such a subset value
// never overwrites any gauge.
const servedTipLaneNone = "\x00scoped"

// servedTipLaneLabel maps an internal lane to its metric label: the
// network-wide pick ("") is published as lane="all".
func servedTipLaneLabel(lane string) string {
	if lane == "" {
		return servedTipLaneAll
	}
	return lane
}

// Bootstrap registers this network with the policy engine. The engine kicks
// off the slot's ticker and runs an initial synchronous eval so request-path
// reads through `policyEngine.GetOrdered` always see a populated cache.
//
// The upstream list is supplied as a closure so newly-bootstrapped upstreams
// become visible to the engine each tick without a re-register.
func (n *Network) Bootstrap(ctx context.Context) error {
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

// gatherEvmTipInputsForMethod builds the picker inputs from the upstreams that
// the selection policy considers ELIGIBLE for `method` — so an upstream the
// user (or an integrity guard) excluded/cordoned drops out of head tracking in
// the same place it drops out of routing. `method == "*"` yields the
// network-wide eligible set (the global served tip); a concrete method yields
// the per-method eligible set (a capability lane). Falls back to the full
// registered set on cold start / when the policy has produced no decision yet.
//
// The ballot itself (who votes, and the guard's live reference) is
// evmTipBallot's rule.
func (n *Network) gatherEvmTipInputsForMethod(
	ctx context.Context,
	useFinalized bool,
	method string,
) ([]evm.ServedTipInput, servedTipReference) {
	return evmTipBallot(n.tipCandidateUpstreams(ctx, method), useFinalized)
}

// evmTipBallot turns one candidate set into the served-tip ballot for an axis,
// plus the regression guard's view of its live heads (servedTipReference).
//
// STATIC-CAP UPSTREAMS DO NOT VOTE while any live head is present. An upstream
// configured with `blockAvailability.upper.exactBlock` below the chain's head is
// declaring a SERVING RANGE ("I answer up to block X"), not observing where the
// head is — see evmTipObservation. Letting such a declaration onto the ballot is
// what broke celo (evm:42220) on 2026-08-11: two legacy upstreams pinned at
// `upper.exactBlock: 21615999` outvoted the live heads (74.5M) the moment a
// third live upstream was excluded for lag, "latest" interpolated to a
// September-2023 block, the availability gate then admitted ONLY those two
// upstreams (the live ones start at 21616000), and funded accounts read 0x0.
//
// MIXED POOL vs ALL-CAPPED POOL. The rule applies per candidate set, which is
// what lets the same function build the network-wide ballot AND each
// guaranteed-method's floor ballot: caps are dropped only when the SAME set
// contains at least one live head. A set of only historical/frozen upstreams
// legitimately serves its cap — that cap is the freshest block anyone in it can
// serve — so it keeps voting.
func evmTipBallot(upstreams []common.Upstream, useFinalized bool) ([]evm.ServedTipInput, servedTipReference) {
	out := make([]evm.ServedTipInput, 0, len(upstreams))
	var capped []evm.ServedTipInput
	var liveTop, liveSecond int64
	for _, cu := range upstreams {
		u, ok := cu.(common.EvmUpstream)
		if !ok || u.EvmStatePoller() == nil {
			continue
		}
		if u.EvmSyncingState() == common.EvmSyncingStateSyncing {
			continue
		}
		blk, boundCapped := evmTipObservation(u, useFinalized)
		if blk <= 0 {
			continue
		}
		in := evm.ServedTipInput{
			UpstreamID:  u.Id(),
			BlockNumber: blk,
		}
		if boundCapped {
			capped = append(capped, in)
			continue
		}
		switch {
		case blk > liveTop:
			liveTop, liveSecond = blk, liveTop
		case blk > liveSecond:
			liveSecond = blk
		}
		out = append(out, in)
	}
	if len(out) == 0 {
		// All-capped set: there is no live head to prefer, so the caps remain
		// the only observations available. (No live head means no live reference
		// for the regression guard either; it falls back to its own last
		// corroborated pick, see guardServedTipRegression.)
		return capped, servedTipReference{}
	}
	if liveSecond <= 0 {
		liveSecond = liveTop // a single live input corroborates only itself
	}
	return out, servedTipReference{Corroborated: liveSecond, Max: liveTop}
}

// servedTipReference is what the LIVE (non-capped) heads of one ballot say,
// in the two distinct roles the regression guard needs. Both are 0 when every
// candidate is capped.
type servedTipReference struct {
	// Corroborated is the SECOND-highest live head — the freshest view that two
	// live upstreams both reach — or the only live head when there is exactly
	// one. It is what a regression is MEASURED against, and it is second-highest
	// for the same reason ServedTipPick.Freshest is: measured against the raw
	// max, one far-future/wrong-chain upstream made every honest pick look like
	// a regression, so the guard never took its healthy branch, never recorded a
	// corroborated value, and sat permanently disarmed — emitting no metric and
	// no log at all.
	//
	// BOUNDARY, stated because it is a real one: this tolerates ONE Byzantine
	// head, not two. Two upstreams reporting the same far-future block are
	// indistinguishable from two upstreams that are simply ahead, so they can
	// still raise the reference and leave the guard unarmed (silently — the
	// guard only records a corroborated value on its healthy branch). Nothing
	// available in a single evaluation separates those cases; what limits the
	// damage is elsewhere: the majority pick itself needs a MAJORITY to move,
	// rememberServedTipPick refuses to commit an uncorroborated leap into the
	// memory, and the trajectory referee's dwell refuses to elect a group that
	// has not tracked the chain for half a minute.
	Corroborated int64

	// Max is the highest live head: the ceiling a HELD value may not exceed, so
	// the guard can never advertise a block no live upstream has (the invariant
	// the old monotonic clamp broke). It is deliberately the raw max and not the
	// corroborated view — a ceiling that a single rogue can only RAISE cannot be
	// used to make the guard serve anything the rogue chose, while a ceiling of
	// the second-highest head would collapse the hold back onto the poisoned
	// pick in the very shape the guard exists for (one live witness left, two
	// stuck upstreams outvoting it).
	Max int64
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

// tipCandidateUpstreams returns the eligible upstreams that feed the served-tip
// picker for `method`, sourced from the selection policy so head tracking and
// routing share one notion of "which upstreams count". Falls back to the full
// registered set when the policy is absent or has not yet produced a decision.
func (n *Network) tipCandidateUpstreams(ctx context.Context, method string) []common.Upstream {
	var ups []common.Upstream
	if n.policyEngine != nil {
		if eligible := n.policyEngine.GetOrdered(n.networkId, method, "*"); len(eligible) > 0 {
			ups = eligible
		}
	}
	if ups == nil {
		if n.upstreamsRegistry == nil {
			// Defensive: a partially-initialized network (e.g. cache-only paths or tests
			// that don't wire a registry) has none — report no candidates so the head
			// accessors fail open (return 0) instead of dereferencing a nil registry.
			return nil
		}
		raw := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
		ups = make([]common.Upstream, 0, len(raw))
		for _, u := range raw {
			ups = append(ups, u)
		}
	}

	// Selector-scoped served tip: when the request targets a subset of
	// upstreams (the use-upstream id/tag selector), the network's
	// `latest`/`finalized` must be decided AMONG that subset — otherwise a
	// more-ahead group (e.g. base flashblocks vs normal) would define the tip
	// for a request pinned to the other group, causing "block not found"
	// churn. This is stateless (no shared counter / no materialized state),
	// so an arbitrary selector value can never grow per-pod memory. No-op when
	// there is no request or no selector in ctx (background/admin callers).
	if sel := requestSelector(ctx); sel != "" {
		filtered := make([]common.Upstream, 0, len(ups))
		for _, u := range ups {
			if m, _ := common.UpstreamMatchesSelector(sel, u); m {
				filtered = append(filtered, u)
			}
		}
		return filtered
	}
	return ups
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

// servedTipPartitionFor returns the lazily-materialized telemetry lane for a
// GROUP-scoped request, or nil to signal the unlabeled stateless path. The group
// identity (key + lane, and its bounded/DDoS-safe gating) comes from
// EvmUpstreamGroupForSelector; the count is capped by maxServedTipPartitions.
func (n *Network) servedTipPartitionFor(ctx context.Context, selector string) *servedTipPartition {
	key, lane := n.EvmUpstreamGroupForSelector(ctx, selector)
	if key == "" {
		return nil
	}
	// Fast path: already materialized.
	if p, ok := n.servedTipPartitions.Load(key); ok {
		return p.(*servedTipPartition)
	}
	// Cap backstop (also bounds a race racing past the cap).
	if n.servedTipPartitionCount.Load() >= maxServedTipPartitions {
		return nil
	}
	p := &servedTipPartition{lane: lane}
	actual, loaded := n.servedTipPartitions.LoadOrStore(key, p)
	if !loaded {
		n.servedTipPartitionCount.Add(1)
	}
	return actual.(*servedTipPartition)
}

// partitionKeyFor maps a use-upstream selector to a STABLE served-tip partition
// key, or "" when the selector must use the stateless path.
//
// A per-group monotonic tracker is materialized only for a "simple" selector (a
// single glob token, optionally negated — e.g. `flashblocks*`, `!flashblocks*`,
// or an exact tag like `family:systx`) that carves out a real SUB-group of the
// network's upstreams. The key is a hash of the MATCHED UPSTREAM SET, not the
// selector text, which makes the whole thing bounded and cross-pod safe:
//
//   - `flashblocks*`, `fl*`, `flash*`, and a `family:flashblocks` tag that all
//     resolve to the same upstreams collapse to ONE partition (dedup);
//   - prefix-enumeration / garbage selectors cannot inflate state beyond the
//     (small, topology-bounded) number of real groupings — an attacker can't
//     manufacture more distinct sets than the upstream layout allows;
//   - every pod derives the same key from the same config, so the shared
//     counter is cross-pod consistent.
//
// A group must match >=2 upstreams (single-node targeting needs no group
// counter — the per-upstream poller is already monotonic) and fewer than ALL
// (matching everything is just the network-wide tip). A per-network cap
// (maxServedTipPartitions) backstops pathological topologies. Anything else
// returns "" (stateless cluster-min over the subset).
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
// the lane. Used by served-tip latest-block tracking (servedTipPartitionFor) AND by
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

// EvmHighestLatestBlockNumber returns the served latest block for this network.
//
// In the default max mode it is the MAX effective latest block across eligible
// non-syncing upstreams. When the served tip is enabled (EvmServedTipConfig),
// it is instead the freshest block a strict MAJORITY of the eligible upstreams
// already have — so interpolated requests land on upstreams that can serve the
// advertised block. Advertising a block visible on only the single most-ahead
// upstream is what causes the "block not found" churn the majority mode avoids.
func (n *Network) EvmHighestLatestBlockNumber(ctx context.Context) int64 {
	ctx, span := common.StartDetailSpan(ctx, "Network.EvmHighestLatestBlockNumber")
	defer span.End()

	if !n.servedTipEnabledFor("latest") {
		// Max mode: evmHighestBlockMax → tipCandidateUpstreams already scopes
		// to the request's selector (if any), so the MAX is within-subset.
		return n.evmHighestBlockMax(ctx, false)
	}
	if sel := requestSelector(ctx); sel != "" {
		// Targeted request: the gather is already scoped to the selector's
		// subset, so the majority is within-group. A selector that names a
		// real configured group additionally gets its own lane (gauge labels
		// + watchdog anchor); arbitrary selectors emit no gauges.
		if p := n.servedTipPartitionFor(ctx, sel); p != nil {
			return n.servedTip(ctx, span, false, "latest", &p.latestAnchor, p.lane)
		}
		return n.servedTip(ctx, span, false, "latest", nil, servedTipLaneNone)
	}
	return n.servedTip(ctx, span, false, "latest", &n.servedLatestAnchor, "")
}

// servedTipEnabledFor reports whether the majority served tip is enabled for
// the given block tag ("latest"/"finalized") on this (EVM) network. Default is
// the max mode for every tag — see EvmServedTipConfig.
func (n *Network) servedTipEnabledFor(tag string) bool {
	return n.cfg != nil && n.cfg.Evm != nil && n.cfg.Evm.ServedTipEnabledFor(tag)
}

// evmHighestBlockMax returns the MAX effective latest/finalized block across the
// eligible, non-syncing upstreams — the default max-mode served tip used when
// the majority served tip is not enabled for this network.
func (n *Network) evmHighestBlockMax(ctx context.Context, useFinalized bool) int64 {
	var maxBlock int64
	for _, cu := range n.tipCandidateUpstreams(ctx, "*") {
		u, ok := cu.(common.EvmUpstream)
		if !ok || u.EvmStatePoller() == nil || u.EvmSyncingState() == common.EvmSyncingStateSyncing {
			continue
		}
		b := u.EvmEffectiveLatestBlock()
		if useFinalized {
			b = u.EvmEffectiveFinalizedBlock()
		}
		if b > maxBlock {
			maxBlock = b
		}
	}
	return maxBlock
}

// tryShortCircuitFutureBlock returns a truthful null response (ok=true) when
// `req` is a concrete-numbered eth_getBlockByNumber lookup whose target block is
// beyond every eligible upstream's head (at the network's emptyResultConfidence level).
// No upstream can serve such a block yet, so dispatching + hedging across all of
// them only burns latency and load before they each return empty — returning the
// null here skips that fan-out entirely.
//
// Safety: it compares against the MAX observed head across eligible upstreams
// (not the majority served tip), so it never nulls out a block the most-ahead
// upstream actually has. It is gated on served-tip being enabled for the latest
// axis — the same opt-in that makes the head trustworthy — and the synthesized
// response is returned directly from Forward, so it is never written to cache
// (the block will exist later). The post-forward empty guard remains as
// defense-in-depth for the in-flight case where an upstream advances mid-request.
func (n *Network) tryShortCircuitFutureBlock(ctx context.Context, req *common.NormalizedRequest, method string) (*common.NormalizedResponse, bool) {
	if n.cfg == nil || n.cfg.Evm == nil || !n.servedTipEnabledFor("latest") {
		return nil, false
	}
	if !strings.EqualFold(method, "eth_getBlockByNumber") {
		return nil, false
	}
	_, bn, err := evm.ExtractBlockReferenceFromRequest(ctx, req)
	if err != nil || bn <= 0 {
		// Tags ("latest"/"pending"/...), block-hash lookups, or unparseable
		// params carry no concrete future number — never short-circuit.
		return nil, false
	}
	useFinalized := n.cfg.Evm.EmptyResultConfidence == common.AvailbilityConfidenceFinalized
	maxHead := n.evmHighestBlockMax(ctx, useFinalized)
	if maxHead <= 0 || bn <= maxHead {
		// Unknown head (fail open) or block within reach of some upstream.
		return nil, false
	}
	jrr, err := common.NewJsonRpcResponse(req.ID(), nil, nil)
	if err != nil {
		return nil, false
	}
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	resp.SetEvmBlockNumber(bn)
	return resp, true
}

// servedTip computes the majority served tip for one axis over the
// selection-policy-eligible upstreams — the freshest block a strict majority of
// them already has (see evm.PickServedTip) — lets the trajectory referee prefer
// a corroborated on-trajectory group when that majority has stalled, guards the
// result against a regression far below the live heads, applies the
// guaranteed-method floor, and exports the gauges.
//
// Both history-aware layers are in-process, bounded and one-directional (the
// referee can only raise the pick; the guard can only hold it, and only
// briefly), and NEITHER can produce a block that no live upstream reports in
// the very same evaluation. Nothing is persisted and nothing is predicted, so
// no inherited or rogue value can pin, freeze or poison the result
// (networks_served_tip_invariants_test.go pins those outcomes against the
// 2026-06 production incident).
func (n *Network) servedTip(
	ctx context.Context,
	span trace.Span,
	useFinalized bool,
	axis string,
	anchor *servedTipAnchor,
	lane string,
) int64 {
	tips, ref := n.gatherEvmTipInputsForMethod(ctx, useFinalized, "*")
	pick := evm.PickServedTip(tips)

	// Trajectory referee: when the live heads split and the majority is the
	// STALLED group, serve the corroborated group that matches where this
	// network's head has been going for the last ten minutes. Advisory and
	// upward-only — it can only replace the majority pick with a higher block
	// that at least two live upstreams already have.
	majority := pick.Tip
	decision := n.refereeServedTip(anchor, pick.Sorted, pick.Tip, ref)
	pick.Tip = decision.Pick

	// Regression guard: "latest" cannot drop far below the corroborated live
	// head in one evaluation. Runs BEFORE the guaranteed-method floor, which is
	// a deliberate operator-configured clamp the guard must not fight.
	pick.Tip = n.guardServedTipRegression(axis, lane, anchor, pick.Tip, ref)

	// Capability guarantee (#855): the served tip must never exceed what any
	// configured guaranteed-method's supporting upstreams can serve, so a
	// request on such a method (e.g. trace_*) never resolves "latest" to a
	// block only non-supporting upstreams have.
	if pick.Tip > 0 {
		if floor := n.guaranteedMethodFloor(ctx, useFinalized); floor > 0 && floor < pick.Tip {
			pick.Tip = floor
		}
	}

	// The referee's outcome is reported only now, against the value the network
	// actually serves: an override the guaranteed-method floor pulled back to
	// (or below) the majority pick changed nothing, and counting or logging it
	// would page someone about an intervention that did not happen.
	n.observeServedTipRefereeTransition(axis, lane, anchor, majority, pick.Tip, decision)

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("served_tip.axis", axis),
			attribute.Int64("served_tip.tip", pick.Tip),
			attribute.Int64("served_tip.freshest", pick.Freshest),
			attribute.Int("served_tip.inputs", pick.Inputs),
		)
	}

	// Watchdog anchor: when did this process last see the served value
	// change. Telemetry only — nothing feeds back into the pick.
	advanceAge := time.Duration(-1)
	if anchor != nil {
		if pick.Tip > 0 {
			anchor.observe(pick.Tip)
		}
		advanceAge = anchor.age()
	}
	n.observeServedTipMetrics(axis, lane, pick, advanceAge)
	return pick.Tip
}

// refereeServedTip returns the tip to serve for `median` (the plain majority
// pick), letting the network's long-term head trajectory choose between the
// groups the LIVE heads of this same evaluation form.
//
// Every mechanism before this one is a snapshot of one instant, which is what a
// majority stall defeats: if N upstreams freeze while staying inside the
// lag-exclusion threshold — or a fleet-wide shared-counter freeze stalls the
// lag metric itself — the majority IS the stale group, and the two upstreams
// that genuinely hold the chain's head are outvoted. The trajectory is the one
// piece of evidence that separates "the chain stopped" from "those upstreams
// stopped": where this network's head has actually been going for the last ten
// minutes (evm.TipTrajectory).
//
// It is deliberately the weakest intervention that answers that:
//
//   - it never runs without an anchor (arbitrary use-upstream selectors
//     materialize no state), without a pick, or without a live head. That last
//     condition is stricter than the guard's, which measures against its own
//     last corroborated pick when the live heads are gone: the referee's
//     evidence is a HEAD TRACK, and a ballot of serving ranges must never enter
//     one;
//   - it never runs when the config disables it (trajectoryWindow: 0), and it
//     is a no-op until every confidence condition in evm.TipTrajectory holds,
//     so a cold or stale process behaves byte-identically to the plain
//     majority;
//   - it can only RAISE the pick, and only to the minimum of a group of ≥2
//     live upstreams — a value they can all serve, and one at most as high as
//     the freshest live head. It can neither hold the tip back nor invent one;
//   - and that group must have EARNED it: half a minute of continuously
//     matching the trajectory while advancing its own head (evm.TipTrajectory's
//     dwell and velocity-agreement conditions), so neither a static fork swept
//     over by a halting chain's expected head nor a routine 15-second
//     divergence can trigger an intervention.
//
// Serving a winning group's MINIMUM also keeps the pick ≤
// evm.ServedTipPick.Freshest (the 2nd-highest head): a group has at least two
// members, so at least two heads sit at or above what it serves.
func (n *Network) refereeServedTip(anchor *servedTipAnchor, sorted []evm.ServedTipInput, median int64, ref servedTipReference) evm.TipTrajectoryDecision {
	stood := evm.TipTrajectoryDecision{Pick: median}
	if anchor == nil || median <= 0 || ref.Max <= 0 {
		// A live head is also what makes `sorted` safe to sample and cluster:
		// gatherEvmTipInputsForMethod only returns bound-capped observations
		// when there is no live head at all, and a serving range must never
		// enter a head trajectory.
		return stood
	}
	window := n.servedTipTrajectoryWindow()
	if window <= 0 {
		return stood
	}

	return anchor.trajectory.Observe(servedTipClock(), sorted, median, evm.TipTrajectoryParams{
		Window: window,
		// maxRegressionBlocks: -1 turns the GUARD off; for the referee it only
		// removes the floor under the tolerance, which the window's own residual
		// spread then sets on its own.
		ToleranceFloor: max(0, n.servedTipMaxRegressionBlocks()),
	})
}

// observeServedTipRefereeTransition counts and logs the referee's outcome ONCE
// PER STATE CHANGE, against `final` — the value the network actually serves
// after the guaranteed-method floor.
//
// Transitions, not evaluations: EvmHighestLatestBlockNumber runs several times
// per request, so a per-evaluation counter scales with RPS and reads as a storm
// of interventions when there is exactly one. Counting entries is what makes
// "any non-zero rate is alert-worthy" true.
func (n *Network) observeServedTipRefereeTransition(axis string, lane string, anchor *servedTipAnchor, majority int64, final int64, d evm.TipTrajectoryDecision) {
	if anchor == nil {
		return
	}
	state := servedTipRefereeIdle
	switch {
	case d.Overrode && final > majority:
		state = servedTipRefereeOverriding
	case d.Declined:
		state = servedTipRefereeDeclining
	}

	// Load first: "still idle" is the answer on essentially every evaluation
	// this process ever makes.
	if anchor.refereeState.Load() == state || anchor.refereeState.Swap(state) == state {
		return
	}
	switch state {
	case servedTipRefereeOverriding:
		telemetry.MetricNetworkServedTipTrajectoryTotal.
			WithLabelValues(n.projectId, n.Label(), servedTipLaneLabel(lane), axis, "override").
			Inc()
		n.logger.Error().
			Str("axis", axis).
			Int64("pick", final).
			Int64("majorityPick", majority).
			Int64("expectedBlock", d.Expected).
			Int64("tolerance", d.Tolerance).
			Float64("blocksPerSecond", d.VelocityPerSec).
			Msg("majority of the eligible upstreams is off the network's long-term head trajectory; serving the corroborated group that is on it")
	case servedTipRefereeDeclining:
		telemetry.MetricNetworkServedTipTrajectoryTotal.
			WithLabelValues(n.projectId, n.Label(), servedTipLaneLabel(lane), axis, "fallback").
			Inc()
	default:
		n.logger.Warn().
			Str("axis", axis).
			Int64("pick", final).
			Msg("served tip is back on the majority pick; the trajectory referee is no longer overriding")
	}
}

// servedTipTrajectoryWindow is how much recorded head history the trajectory
// referee needs before it may participate. Unset uses
// DefaultServedTipTrajectoryWindow; an explicit 0 disables the referee.
func (n *Network) servedTipTrajectoryWindow() time.Duration {
	if n.cfg != nil && n.cfg.Evm != nil && n.cfg.Evm.ServedTip != nil && n.cfg.Evm.ServedTip.TrajectoryWindow != nil {
		return n.cfg.Evm.ServedTip.TrajectoryWindow.Duration()
	}
	return common.DefaultServedTipTrajectoryWindow
}

// guardServedTipRegression returns the tip to serve for `pick`, substituting the
// last corroborated pick when `pick` sits further than the configured tolerance
// below the best CORROBORATED evidence of where the chain is.
//
// This is a physics check, independent of which voters are honest: a chain's
// head does not fall a thousand blocks between two evaluations while an upstream
// still reports the old one, so a pick that far below corroborated evidence is a
// poisoned ballot — an availability bound voting as a head (the 2026-08 celo
// incident), a bogus vendor head, a rolled-back shared counter. It is
// deliberately independent of the bound-capped filter in
// gatherEvmTipInputsForMethod: that removes ONE known source of low votes, this
// bounds the damage of any other.
//
// TWO SOURCES OF EVIDENCE, ONE CEILING. A regression is measured against the
// higher of (a) `ref.Corroborated`, the second-highest LIVE head of this
// evaluation, and (b) the last pick this process saw live-corroborated. Neither
// alone is enough: (a) alone is defeated by the guard's own headline shape —
// one live upstream left, two stuck ones outvoting it, where the second-highest
// live head IS the poison — and (b) alone would be memory with no present-tense
// check. A HELD value is then capped at `ref.Max`, the highest live head, so the
// guard can never advertise a block no live upstream has.
//
// It never blocks serving and never persists anything:
//   - no pick, or no anchor for this lane (arbitrary use-upstream selectors
//     deliberately materialize no state), or maxRegressionBlocks: -1 → returns
//     the pick untouched;
//   - within tolerance → a LIVE-corroborated pick becomes the new remembered
//     value (a capped one never does);
//   - below tolerance, for up to servedTipRegressionTTL since that value was
//     recorded → serves it instead, never above the highest live head;
//   - past the TTL → fails open, serves the pick as computed, and DISARMS: a
//     value that has outlived the window is no longer evidence, so the guard
//     re-arms from whatever the live heads corroborate next instead of measuring
//     forever against a number the chain has left behind.
//
// NO LIVE HEAD IS NOT A REASON TO STAND DOWN. When every live upstream leaves the
// eligible set while bound-capped ones remain, the ballot legitimately falls back
// to the caps — and a guard that requires a live reference would be exactly as
// absent as it was during the celo incident, on the one ballot shape that caused
// it. With no live head the guard measures against its own last corroborated pick
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
func (n *Network) guardServedTipRegression(axis string, lane string, anchor *servedTipAnchor, pick int64, ref servedTipReference) int64 {
	if anchor == nil || pick <= 0 {
		return pick
	}
	tolerance := n.servedTipMaxRegressionBlocks()
	if tolerance < 0 {
		// maxRegressionBlocks: -1 — the operator turned the guard off, the
		// symmetric opposite of trajectoryWindow: 0.
		return pick
	}

	lastGood, lastGoodAt := anchor.lastGoodValue.Load(), anchor.lastGoodAt.Load()
	armed := lastGood > 0 && lastGoodAt != nil
	reference := ref.Corroborated
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
		if n.rememberServedTipPick(anchor, pick, lastGood, lastGoodAt, ref, tolerance, now) {
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
		if n.noteServedTipGuardTransition(anchor, servedTipGuardFailedOpen, axis, pick, reference, held) {
			telemetry.MetricNetworkServedTipRegressionTotal.
				WithLabelValues(n.projectId, n.Label(), servedTipLaneLabel(lane), axis, "failed_open").
				Inc()
		}
		anchor.lastGoodValue.Store(0)
		anchor.lastGoodAt.Store(nil)
		return pick
	}

	// Serve the remembered pick — but never above the highest live head, so the
	// guard cannot advertise a block no live upstream has (the invariant the old
	// monotonic clamp broke). With no live head there is nothing to clamp to and
	// the remembered value stands as recorded.
	served := lastGood
	if ref.Max > 0 && ref.Max < served {
		served = ref.Max
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
			WithLabelValues(n.projectId, n.Label(), servedTipLaneLabel(lane), axis, "held").
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
func (n *Network) rememberServedTipPick(anchor *servedTipAnchor, pick int64, lastGood int64, lastGoodAt *time.Time, ref servedTipReference, tolerance int64, now time.Time) bool {
	if ref.Max <= 0 || pick > ref.Corroborated {
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
// once when it gives up — never once per evaluation. EvmHighestLatestBlockNumber
// runs several times per request, so a per-evaluation counter would scale with
// RPS and read as thousands of suppressed picks where there is one sustained
// regression.
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
// -1 is returned as-is: it disables the guard, the symmetric opposite of
// trajectoryWindow: 0.
func (n *Network) servedTipMaxRegressionBlocks() int64 {
	if n.cfg != nil && n.cfg.Evm != nil && n.cfg.Evm.ServedTip != nil && n.cfg.Evm.ServedTip.MaxRegressionBlocks != 0 {
		return n.cfg.Evm.ServedTip.MaxRegressionBlocks
	}
	return common.DefaultToleratedBlockHeadRollback
}

// SvmHighestLatestSlot returns the majority-vote latest slot across SVM
// upstreams. Uses evm.PickServedTip (floor(N/2)-th highest) so a single
// fast/rogue upstream cannot inflate the served tip.
func (n *Network) SvmHighestLatestSlot(ctx context.Context) int64 {
	_, span := common.StartDetailSpan(ctx, "Network.SvmHighestLatestSlot")
	defer span.End()
	pick := evm.PickServedTip(n.gatherSvmTipInputs(ctx, false))
	span.SetAttributes(attribute.Int64("highest_latest_slot", pick.Tip))
	return pick.Tip
}

// SvmHighestFinalizedSlot is the finalized-slot counterpart. Used by the
// slot-lag consensus filter as the reference "pool leader" value.
func (n *Network) SvmHighestFinalizedSlot(ctx context.Context) int64 {
	_, span := common.StartDetailSpan(ctx, "Network.SvmHighestFinalizedSlot")
	defer span.End()
	pick := evm.PickServedTip(n.gatherSvmTipInputs(ctx, true))
	span.SetAttributes(attribute.Int64("highest_finalized_slot", pick.Tip))
	return pick.Tip
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
// tryShortCircuitFutureBlock uses evmHighestBlockMax (not PickServedTip.Tip):
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
// majority-tip computation via evm.PickServedTip.
func (n *Network) gatherSvmTipInputs(ctx context.Context, useFinalized bool) []evm.ServedTipInput {
	upstreams := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
	out := make([]evm.ServedTipInput, 0, len(upstreams))
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
		out = append(out, evm.ServedTipInput{UpstreamID: u.Id(), BlockNumber: slot})
	}
	return out
}

// EvmHighestFinalizedBlockNumber is the finalized-axis sibling of
// EvmHighestLatestBlockNumber. Same majority semantics, applied to each
// upstream's EvmEffectiveFinalizedBlock.
func (n *Network) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 {
	ctx, span := common.StartDetailSpan(ctx, "Network.EvmHighestFinalizedBlockNumber", trace.WithAttributes(
		attribute.String("network.id", n.networkId),
	))
	defer span.End()

	if !n.servedTipEnabledFor("finalized") {
		return n.evmHighestBlockMax(ctx, true)
	}
	if sel := requestSelector(ctx); sel != "" {
		// See EvmHighestLatestBlockNumber: a configured-tag selector gets its
		// own lane (telemetry labels + watchdog anchor); any other selector
		// computes the same stateless majority without emitting gauges.
		if p := n.servedTipPartitionFor(ctx, sel); p != nil {
			return n.servedTip(ctx, span, true, "finalized", &p.finalizedAnchor, p.lane)
		}
		return n.servedTip(ctx, span, true, "finalized", nil, servedTipLaneNone)
	}
	return n.servedTip(ctx, span, true, "finalized", &n.servedFinalizedAnchor, "")
}

// guaranteedMethodFloor returns the lowest majority served tip across the
// configured GuaranteedMethods' supporting (eligible) upstream sets, or 0 when
// no guaranteed methods are configured or none constrain the tip. Each method's
// supporting set is the selection-policy-eligible set for that method (which,
// via autoIgnoreUnsupportedMethods, excludes upstreams that don't support it).
//
// Each method's ballot is built by evmTipBallot, i.e. by exactly the rule the
// network-wide ballot uses — including the static-cap filter, applied to THAT
// method's supporting set. Without it the floor was a second door onto the
// incident value: with `guaranteedMethods` configured, the floor ballot took raw
// effective heads and ran AFTER the guard, so the celo pair's 21,615,999 came
// back as a "floor" and was served, on a branch whose whole premise is that a
// serving range never defines the head.
//
// The per-set mixed-pool rule is what keeps that from breaking the floor's
// purpose: capped upstreams drop out of a method's ballot only when some LIVE
// upstream also supports that method. If only capped upstreams support it, they
// set the floor — which is the entire point of the floor, since they are the
// only upstreams that can serve the method at all.
//
// STATIC CONFIG DECIDES THAT, NOT TRANSIENT ELIGIBILITY. A cap-only ballot has
// two very different causes, and only one of them is a floor:
//
//   - PERMANENT: no live upstream is configured to serve the method at all
//     (the archive-only trace_* shape). The cap is genuinely the freshest block
//     anyone can serve, so it floors the tip.
//   - TRANSIENT: live upstreams DO serve the method, they are just absent from
//     this instant's eligible set — lag-excluded, cordoned, mid-restart. Their
//     absence is exactly the condition the regression guard is holding through,
//     and flooring to the cap would hand the incident value back the moment the
//     eligible set collapsed. That composition (the guard correctly holding
//     74.5M while the floor dragged the served value to 21,615,999) was live at
//     the previous head of this branch.
//
// So a cap-only ballot floors only when the REGISTERED set — the operator's
// configuration, which eligibility churn cannot change — has no live upstream
// serving that method. Otherwise the method sets no floor for this evaluation
// and the guard's hold (and its bounded fail-open) governs, exactly as it does
// when the whole ballot goes cap-only.
func (n *Network) guaranteedMethodFloor(ctx context.Context, useFinalized bool) int64 {
	if n.cfg == nil || n.cfg.Evm == nil || n.cfg.Evm.ServedTip == nil {
		return 0
	}
	methods := n.cfg.Evm.ServedTip.GuaranteedMethods
	if len(methods) == 0 {
		return 0
	}
	eligible := n.tipCandidateUpstreams(ctx, "*")
	supporting := make([]common.Upstream, 0, len(eligible))
	var floor int64
	for _, m := range methods {
		supporting = supporting[:0]
		for _, cu := range eligible {
			// Supporting set = eligible upstreams that handle this method.
			if handle, _ := cu.ShouldHandleMethod(m); !handle {
				continue
			}
			supporting = append(supporting, cu)
		}
		tips, ref := evmTipBallot(supporting, useFinalized)
		if len(tips) == 0 {
			// No supporting upstream for this method → no constraint (fall through
			// rather than pinning the tip to 0).
			continue
		}
		if ref.Max <= 0 && n.liveUpstreamServesMethod(ctx, m, useFinalized) {
			// Cap-only ballot, but the configuration says live upstreams serve
			// this method — they are merely absent right now. See the comment
			// above: that floor is a transient lie, and serving it is the celo
			// value.
			continue
		}
		if t := evm.PickServedTip(tips).Tip; t > 0 && (floor == 0 || t < floor) {
			floor = t
		}
	}
	return floor
}

// liveUpstreamServesMethod reports whether any REGISTERED upstream that is not a
// static serving-range cap is configured to serve `method` — the static-config
// question guaranteedMethodFloor asks before letting a cap-only ballot set a
// floor. It reads the registered set, not the eligible one, precisely because
// eligibility is the thing that churns.
//
// An upstream with no head yet counts as live: "we cannot tell" must fall on the
// side of skipping the floor, since serving a stale cap is the failure this
// whole change exists to prevent. Only reached when a method's ballot came back
// cap-only, i.e. approximately never on the hot path.
func (n *Network) liveUpstreamServesMethod(ctx context.Context, method string, useFinalized bool) bool {
	if n.upstreamsRegistry == nil {
		return false
	}
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId) {
		if handle, _ := u.ShouldHandleMethod(method); !handle {
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

// observeServedTipMetrics exports the served-tip gauges. axis
// ("latest"|"finalized") is a label rather than part of the metric name to
// keep the gauge set small. lane="all" is the network-wide pick; a named lane
// is a use-upstream group's own pick; the stateless lane-none sentinel emits
// nothing (a subset value must never overwrite a gauge).
func (n *Network) observeServedTipMetrics(axis string, lane string, pick evm.ServedTipPick, advanceAge time.Duration) {
	if lane == servedTipLaneNone {
		return
	}
	laneLabel := servedTipLaneLabel(lane)
	if pick.Tip > 0 {
		telemetry.MetricNetworkServedTipBlockNumber.
			WithLabelValues(n.projectId, n.Label(), laneLabel, axis).
			Set(float64(pick.Tip))
		// Deliberate lag: how far the majority tip sits behind the corroborated
		// freshest upstream view in the same set. Clamped at 0 because the tip
		// can legitimately sit ABOVE that view — while the regression guard
		// holds its last corroborated pick, and on the evaluation a referee
		// override raises the tip to a fresh group — and a lag gauge that reads
		// negative is a broken dashboard, not a signal. Both of those states
		// have their own counters (…_regression_total / …_trajectory_total).
		lag := pick.Freshest - pick.Tip
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

func (n *Network) EvmLeaderUpstream(ctx context.Context) common.Upstream {
	var leader common.Upstream
	var leaderLastBlock int64 = 0
	upsList := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
	for _, u := range upsList {
		if statePoller := u.EvmStatePoller(); statePoller != nil {
			lastBlock := statePoller.LatestBlock()
			if lastBlock > leaderLastBlock {
				leader = u
				leaderLastBlock = lastBlock
			}
		}
	}
	return leader
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

		mk := fe.MatchRequestKind()
		kindMatches := mk == "*" || mk == "" || mk == kind

		if methodMatches && finalityMatches && kindMatches {
			return fe
		}
	}

	return nil
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
			attribute.String("request.finality", req.Finality(ctx).String()),
		),
	)

	// Bind the request to ctx so downstream served-tip resolution can decide
	// `latest`/`finalized` AMONG the use-upstream-selected subset (see
	// tipCandidateUpstreams / requestSelector). The failsafe executor re-sets
	// the same value below for its own readers; setting it here makes it
	// available to the earlier pre-forward / short-circuit paths too.
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
	}

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

	if len(upsList) == 0 {
		err := common.NewErrNoUpstreamsFound(n.projectId, n.networkId)
		common.SetTraceSpanError(forwardSpan, err)
		if mlx != nil {
			mlx.Close(ctx, nil, err)
		}
		return nil, err
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

	// Feed the per-network probe-bus AFTER we know the request is
	// actually going to dispatch to an upstream (i.e. not a
	// cache-hit / static-response / follower-multiplexer
	// short-circuit, all of which returned earlier). The publish is
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

	// Future-block short-circuit: a concrete block number beyond every eligible
	// upstream's head cannot be served yet — return the truthful null instead of
	// dispatching + hedging across upstreams that will all return empty. Runs
	// before rate limiting so a non-dispatched request consumes no permit.
	if resp, ok := n.tryShortCircuitFutureBlock(ctx, req, method); ok {
		forwardSpan.SetAttributes(attribute.Bool("future_block.short_circuit", true))
		if mlx != nil {
			mlx.Close(ctx, resp, nil)
		}
		return resp, nil
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

	// 4) Prepare the request
	if err := n.prepareRequest(ctx, req); err != nil {
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

			u, selErr := effectiveReq.NextUpstream()
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

			// Return immediately when the result is usable:
			//  - Non-empty success always qualifies.
			//  - Emptyish success qualifies when the method is in
			//    emptyResultAccept and consensus is not required,
			//    because failsafe would accept the empty result anyway
			//    so trying more upstreams just wastes time on slow ones.
			//  - Otherwise emptyish results continue to the next upstream.
			if err == nil && r != nil && !r.IsObjectNull() {
				emptyish := r.IsResultEmptyish()
				acceptEmpty := !emptyish ||
					(!failsafeExecutor.HasConsensus() &&
						slices.Contains(failsafeExecutor.EmptyResultAccept(), method))
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
			return nil, translatedErr
		}
	}

	if resp != nil {
		if n.cacheDal != nil {
			// Force-materialize jrr so the goroutine reads only via atomic pointer (no locks needed).
			// TODO For other architectures we might need a different approach
			_, _ = resp.JsonRpcResponse(ctx)
			resp.AddRef()

			go (func(resp *common.NormalizedResponse, forwardSpan trace.Span) {
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
				tracedCtx := trace.ContextWithSpanContext(timeoutCtx, forwardSpan.SpanContext())
				err := n.cacheDal.Set(tracedCtx, req, resp)
				if err != nil {
					lg.Warn().Err(err).Msgf("could not store response in cache")
				}
			})(resp, forwardSpan)
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

	return resp, nil
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
func (n *Network) checkUpstreamBlockAvailability(ctx context.Context, u common.Upstream, req *common.NormalizedRequest, method string) (error, bool) {
	if n.cfg.Architecture != common.ArchitectureEvm {
		return nil, false
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

	// Prefer the cached block number from normalization. Fall back to extracting
	// from the request (defensive: handles paths that bypass json_rpc.go's
	// normalization, and methods whose params haven't been pre-cached yet).
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

		// Shouldn't reach here normally, but if we do, retry
		lg.Warn().Str("hash", mlxHash).Msg("multiplexer follower got nil response and no error, retrying")
	}
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

	// For any JSON-RPC architecture: ensure the response ID always reflects the
	// client's original request ID, regardless of what the upstream echoed back.
	// This is especially important for proxies that normalize or multiplex IDs
	// toward upstreams, and must apply to every JSON-RPC architecture — not just
	// EVM — so non-EVM clients (Solana and future architectures) aren't left with
	// mismatched response IDs.
	if resp != nil {
		if jrr, err := resp.JsonRpcResponse(ctx); err == nil && jrr != nil {
			jrq, err := req.JsonRpcRequest(ctx)
			if err != nil {
				return err
			}
			// Prefer the verbatim request id bytes when available so that
			// large integers (>2^53), fractional ids, and other exotic
			// numeric formats round-trip without precision loss. Falls
			// back to the typed id for programmatically-constructed
			// requests where idRaw is unset.
			if rawID := jrq.IDRawBytes(); len(rawID) > 0 {
				if err := jrr.SetIDBytes(rawID); err != nil {
					return err
				}
			} else if err := jrr.SetID(jrq.ID); err != nil {
				return err
			}
		}
	}

	return nil
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
