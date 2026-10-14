package svm

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

const DefaultToleratedSlotRollback = 1024

// DefaultPollInterval matches the Solana block time (~400ms). Ticks any faster burn
// upstream RPC quota without materially improving freshness.
const DefaultPollInterval = 400 * time.Millisecond

// maxConsecutiveSlotPollSkips bounds how many consecutive ticks the traffic
// gate may skip the two getSlot calls. Live traffic proves freshness, but the
// poller must periodically observe on its own — a stream of suggestions from a
// single busy method must never fully starve independent verification.
// ponytail: fixed bound mirrors the relay-skip cap proven in Lava-derived
// routers; make it configurable only if a real workload needs it.
const maxConsecutiveSlotPollSkips = 4

// healthPollEveryNTicks throttles getHealth to every Nth eligible poll. On paid
// vendor RPCs the poller is the dominant background cost — measured on
// solana-devnet it ran ~3.7 getHealth calls/sec, comparable to ALL
// client-driven getBlock traffic after the finalized-tip guard landed.
//
// getHealth is the one poller signal whose VALUE is near-static: a boolean that
// flips only when an upstream actually degrades, and it feeds only the
// cordon/uncordon edge in applyHealthToRouting. Worst-case detection latency
// becomes N * debounce (~2s at defaults), against a circuit breaker that
// already reacts to real request failures far sooner.
//
// The other three calls are deliberately NOT throttled, because their VALUES
// move at chain rate (~2.5-3.7 slots/sec, measured) and each is a live bound:
//
//   - getSlot(finalized) IS the getBlock guard's bound at finalized commitment
//     — which includes every request that pins no commitment — so a staler tip
//     directly widens the window of requests that escape to upstreams.
//   - getSlot(processed) is the bound for non-finalized routing.
//   - getMaxShredInsertSlot bounds every commitment level, and
//     tipStalenessMargin is derived from the assumption that this snapshot is
//     at most ONE debounce old. Throttling it without widening that margin
//     would silently start rejecting slots the upstreams actually hold.
//
// A FAILED probe is never throttled: an unhealthy verdict cordons the upstream
// out of rotation (applyHealthToRouting), so letting one transient probe error
// — a vendor 429 against the poller is the common case — ride out the whole
// window would multiply a 400ms outage into N * debounce. The next poll
// re-probes instead, which costs extra calls only while something is actually
// wrong.
//
// ponytail: fixed multiplier, promote to SvmNetworkConfig only if a deployment
// needs a different health-detection latency than ~2s.
const healthPollEveryNTicks = 5

// DefaultFirstAvailableSlotStaleness is the minimum interval between floor
// probes (getBlocksWithLimit(0, 1)) per upstream.
//
// Deliberately ~75x the slot time. The floor sits O(100k) slots below the head
// and rises at most one slot per slot, so freshness buys nothing measurable.
// It also needs NO staleness margin on the comparison: a stale floor is always
// LOWER than the true floor, so it can only under-exclude — one wasted round
// trip the sweep already absorbs — and never wrongly exclude. That is the
// opposite of indexedTipStalenessMargin, whose bound moves TOWARD the
// requested slot and therefore does need slack.
//
// The only thing this window bounds is recovery after a floor DROP (a node
// re-provisioned with longer history behind a stable endpoint).
const DefaultFirstAvailableSlotStaleness = 30 * time.Second

// Static request payloads — avoid allocating on every tick.
var (
	reqGetHealth              = []byte(`{"jsonrpc":"2.0","id":1,"method":"getHealth","params":[]}`)
	reqGetSlotProcessed       = []byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"processed"}]}`)
	reqGetSlotFinalized       = []byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"finalized"}]}`)
	reqGetMaxShredInsertSlot  = []byte(`{"jsonrpc":"2.0","id":1,"method":"getMaxShredInsertSlot","params":[]}`)
	reqGetOldestStoredBlock   = []byte(`{"jsonrpc":"2.0","id":1,"method":"getBlocksWithLimit","params":[0,1]}`)
)

var _ common.SvmStatePoller = (*SvmStatePoller)(nil)

type SvmStatePoller struct {
	projectId string
	appCtx    context.Context
	logger    *zerolog.Logger
	upstream  common.Upstream
	tracker   *health.Tracker

	latestSlotShared    data.CounterInt64SharedVariable
	finalizedSlotShared data.CounterInt64SharedVariable

	// firstAvailableSlotShared is this upstream's LEDGER FLOOR — the lowest
	// slot it can still serve, the oldest block getBlocksWithLimit returns.
	// Created with ignoreRollbackOf = 0 so a DECREASE is accepted: a node
	// re-provisioned from a longer snapshot, or a vendor enabling long-term
	// storage, legitimately drops its floor. The error asymmetry forces that
	// choice — a floor stuck too HIGH excludes an upstream that can serve,
	// while a floor stuck too LOW costs one wasted round trip.
	firstAvailableSlotShared data.CounterInt64SharedVariable

	// firstAvailableSlotKnown separates "observed floor of 0" from "never
	// observed". The counter cannot: GetValue() returns 0 for both, and 0 is a
	// real answer — a genesis-backed archive reports it. Conflating them would
	// make a full-history upstream read as "history starts at MIN(others)",
	// i.e. unknown reading as unavailable.
	firstAvailableSlotKnown atomic.Bool

	// firstAvailableSlotPending is set when a stale refresh timed out (or
	// otherwise returned without establishing known). TryUpdateIfStale's
	// UpdateMaxWait path marks the counter fresh with a nil error while the
	// fetch continues asynchronously — that freshness must NOT be read as a
	// peer-seeded observation on the next tick's fast path.
	firstAvailableSlotPending atomic.Bool

	shredInsertSlot       atomic.Int64
	maxShredInsertSlotLag atomic.Int64
	healthy               atomic.Bool

	// debounceInterval is the GATE: the minimum wall-clock interval between
	// network polls (see SvmNetworkConfig.StatePollerDebounce). The ticker fires
	// at the fixed, cheap DefaultPollInterval; this gate throttles the actual
	// fan-out. Stored as nanoseconds so SetDebounceInterval can update it
	// race-free while Poll reads it. NB: it must NOT also drive the ticker period
	// — a ticker firing at exactly the gate value skips every other tick (the
	// gate compares against lastPollAt recorded at poll completion, always a hair
	// under the interval), halving the effective cadence.
	debounceInterval atomic.Int64
	lastPollAt       atomic.Int64

	// lastExternalLatestAt / lastExternalFinalizedAt stamp (UnixMilli) the most
	// recent EXTERNAL slot observation (live-traffic context.slot harvest or
	// shared-state suggestion) per commitment view. The poll loop uses them as
	// the traffic gate: when both views are fresher than the debounce window,
	// the two getSlot calls are skipped. Only the public Suggest* entry points
	// stamp these — the poller's own fetches go through the private variants,
	// so a poll can never satisfy its own gate.
	lastExternalLatestAt    atomic.Int64
	lastExternalFinalizedAt atomic.Int64
	// slotPollSkips counts consecutive traffic-gated skips; guarded by pollMu.
	slotPollSkips int
	// pollCount counts polls that passed the debounce gate. Guarded by pollMu.
	pollCount int
	// nextHealthPoll is the pollCount at which getHealth runs again. Starts at 0
	// so the first poll always probes (cold start must not route on a
	// zero-valued health verdict), then advances by healthPollEveryNTicks — or
	// back to the immediate next poll after a failed probe. Guarded by pollMu.
	nextHealthPoll int
	pollMu         sync.Mutex

	// loopStarted guards the polling goroutine. Bootstrap is retried on the
	// SAME poller instance (the upstream initializer reuses a pending Upstream
	// and re-runs the whole bootstrap task after a failed genesis validation),
	// and every unguarded `go e.loop(...)` leaks a ticker goroutine that only
	// appCtx cancellation can stop. pollMu and the debounce gate suppress the
	// duplicate I/O but not the goroutine growth. Mirrors EvmStatePoller.started.
	loopStarted atomic.Bool
	// loopsRunning counts LIVE polling goroutines. Invariant: never above 1;
	// back to 0 once appCtx cancellation tears the loop down.
	loopsRunning atomic.Int32

	// cordonedByHealth records whether THIS poller took the upstream out of
	// rotation, making cordon/uncordon edge-triggered and keeping a recovery
	// from lifting an operator's manual cordon. See applyHealthToRouting.
	cordonedByHealth atomic.Bool
}

func NewSvmStatePoller(
	projectId string,
	appCtx context.Context,
	logger *zerolog.Logger,
	up common.Upstream,
	tracker *health.Tracker,
	sharedState data.SharedStateRegistry,
) *SvmStatePoller {
	networkId := up.NetworkId()
	lg := logger.With().Str("component", "svmStatePoller").Str("networkId", networkId).Logger()

	latestKey := fmt.Sprintf("svm/latestSlot/%s", common.UniqueUpstreamKey(up))
	finalizedKey := fmt.Sprintf("svm/finalizedSlot/%s", common.UniqueUpstreamKey(up))

	firstAvailableKey := fmt.Sprintf("svm/firstAvailableSlot/%s", common.UniqueUpstreamKey(up))

	latestShared := sharedState.GetCounterInt64(latestKey, DefaultToleratedSlotRollback)
	finalizedShared := sharedState.GetCounterInt64(finalizedKey, DefaultToleratedSlotRollback)
	// ignoreRollbackOf = 0 accepts every change, including decreases, and also
	// disables the non-positive guard — required here, because 0 is a legal
	// floor. DefaultToleratedSlotRollback is for fork detection on a HEAD; a
	// floor has no forks. No OnLargeRollback callback either: a floor decrease
	// is normal operation, not the wrong-node signal that callback reports.
	firstAvailableShared := sharedState.GetCounterInt64(firstAvailableKey, 0)

	e := &SvmStatePoller{
		projectId:           projectId,
		appCtx:              appCtx,
		logger:              &lg,
		upstream:            up,
		tracker:             tracker,
		latestSlotShared:    latestShared,
		finalizedSlotShared: finalizedShared,

		firstAvailableSlotShared: firstAvailableShared,
	}

	// Any accepted floor CHANGE marks the floor known and refreshes the gauge,
	// whatever produced it (this instance's fetch, or a peer's push). OnValue
	// is edge-triggered: it cannot cover an initial load that raced this
	// registration, nor a propagated value equal to the counter's current one
	// (a peer's floor of 0 into a zero-valued counter). Those paths are closed
	// by refreshFirstAvailableSlot's not-stale fast path — but ONLY when no
	// local refresh is still pending a real observation (see
	// firstAvailableSlotPending). Registered unconditionally: unlike the slot
	// counters below it feeds no tracker, so it has no dependency on `tracker`
	// being non-nil.
	firstAvailableShared.OnValue(func(v int64) {
		e.firstAvailableSlotPending.Store(false)
		e.firstAvailableSlotKnown.Store(true)
		e.gauge(telemetry.MetricUpstreamSvmFirstAvailableSlot).Set(float64(v))
	})

	// Counter callbacks, mirroring EvmStatePoller (evm_state_poller.go:157-171).
	// OnValue is the ONLY tracker feed for slots: it fires once per ACCEPTED
	// value whatever the source — this poller's fetch, a live-traffic
	// context.slot suggestion, or cross-instance propagation of the shared
	// counter — and stays silent when the counter rejects a lower slot, so the
	// tracker can never be fed a slot the counter itself refused.
	if tracker != nil {
		latestShared.OnValue(func(value int64) {
			// Processed-slot lag drives score-based upstream selection on every
			// path, not just the consensus slot-lag pre-filter. Solana slots
			// carry no block timestamp, so pass 0 (EVM passes 0 here too, to
			// avoid attributing a remote update's timestamp locally).
			e.tracker.SetLatestBlockNumber(e.upstream, value, 0)
		})
		finalizedShared.OnValue(func(value int64) {
			e.tracker.SetFinalizedBlockNumber(e.upstream, value)
		})

		// A slot jump backwards past DefaultToleratedSlotRollback is the
		// non-canonical / wrong-node signal: a load balancer swapping in a node
		// on another fork, a snapshot restore, or a cross-wired endpoint.
		latestShared.OnLargeRollback(func(currentVal, newVal int64) {
			e.tracker.RecordBlockHeadLargeRollback(e.upstream, "latest", currentVal, newVal)
		})
		finalizedShared.OnLargeRollback(func(currentVal, newVal int64) {
			e.tracker.RecordBlockHeadLargeRollback(e.upstream, "finalized", currentVal, newVal)
		})
	}

	// Start healthy — flipped to false on first failing getHealth. "Not yet
	// observed" must never read as unhealthy (see applyHealthToRouting).
	e.healthy.Store(true)
	return e
}

func (e *SvmStatePoller) IsObjectNull() bool {
	return e == nil
}

// recoverPanic keeps a panic in a poll tick or fan-out goroutine from taking
// down the whole erpc process (which serves every other network too).
func (e *SvmStatePoller) recoverPanic(where string) {
	if r := recover(); r != nil {
		e.logger.Error().
			Interface("panic", r).
			Str("where", where).
			Bytes("stack", debug.Stack()).
			Msg("recovered from panic in svm state poller")
	}
}

func (e *SvmStatePoller) Bootstrap(ctx context.Context) error {
	// The debounce gate defaults to one slot when not configured. It may have
	// already been set via SetDebounceInterval (config can arrive before
	// Bootstrap), so only fill the default when still unset.
	if e.debounceInterval.Load() <= 0 {
		e.debounceInterval.Store(int64(DefaultPollInterval))
	}
	e.logger.Debug().
		Dur("tickInterval", DefaultPollInterval).
		Dur("debounce", time.Duration(e.debounceInterval.Load())).
		Msg("bootstrapping svm state poller")

	// The ticker stays at the fixed one-slot cadence (cheap, no I/O); the
	// debounce gate in Poll throttles the actual network fan-out to the
	// configured rate.
	//
	// Idempotent w.r.t. the loop: at most one goroutine per poller instance no
	// matter how many times Bootstrap runs. Genesis validation (the caller's
	// fail-closed gate) may still be retried freely.
	if !e.loopStarted.CompareAndSwap(false, true) {
		e.logger.Debug().Msg("svm state poller loop already running; skipping duplicate start")
		return nil
	}

	go e.loop(DefaultPollInterval)
	return nil
}

// SetDebounceInterval wires the configured poll-throttle gate from
// SvmNetworkConfig.StatePollerDebounce (see upstream.SetNetworkConfig). Callable
// at any time; takes effect on the next ticker fire.
func (e *SvmStatePoller) SetDebounceInterval(d time.Duration) {
	if d > 0 {
		e.debounceInterval.Store(int64(d))
	}
}

func (e *SvmStatePoller) loop(interval time.Duration) {
	e.loopsRunning.Add(1)
	defer e.loopsRunning.Add(-1)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.appCtx.Done():
			e.logger.Debug().Msg("shutting down svm state poller due to app context interruption")
			return
		case <-ticker.C:
			func() {
				defer e.recoverPanic("loop")
				nctx, cancel := context.WithTimeout(e.appCtx, 15*time.Second)
				defer cancel()
				if err := e.Poll(nctx); err != nil {
					if !errors.Is(nctx.Err(), context.Canceled) {
						e.logger.Warn().Err(err).Msg("svm state poll failed")
					}
				}
			}()
		}
	}
}

// Poll fans out up to four RPC calls in parallel. Each result updates its own
// field so a single failure doesn't blank the others. Shred-insert lag is
// computed after the fan-out joins — otherwise the getMaxShredInsertSlot
// goroutine races against the getSlot goroutine and reads a stale (or zero)
// latest slot.
//
// Traffic gate: when live traffic has refreshed BOTH the latest and finalized
// slot views within the debounce window (via SuggestLatestSlot /
// SuggestFinalizedSlot, fed by upstreamPostForward_trackContextSlot), the two
// getSlot calls are skipped this tick — traffic already proved slot freshness,
// and on paid vendor RPCs the poller is the dominant background cost.
// Bounded by maxConsecutiveSlotPollSkips so suggestions can never fully
// replace the poller's own observations.
//
// getMaxShredInsertSlot always runs: traffic carries no such signal, and the
// watermark advances at chain rate, so a throttled snapshot would go stale
// against indexedTipStalenessMargin and start rejecting servable slots.
//
// getHealth cannot use the traffic gate either, but its value is a near-static
// boolean, so it is throttled by count instead (healthPollEveryNTicks). A
// skipped tick leaves the previously stored verdict in place;
// applyHealthToRouting still runs every poll off that last-known value.
func (e *SvmStatePoller) Poll(ctx context.Context) error {
	e.pollMu.Lock()
	defer e.pollMu.Unlock()

	d := time.Duration(e.debounceInterval.Load())
	if d > 0 {
		last := e.lastPollAt.Load()
		if last > 0 && time.Since(time.UnixMilli(last)) < d {
			return nil
		}
	}

	skipSlots := false
	if d > 0 && e.slotPollSkips < maxConsecutiveSlotPollSkips {
		nowMs := time.Now().UnixMilli()
		window := d.Milliseconds()
		latestAt := e.lastExternalLatestAt.Load()
		finalizedAt := e.lastExternalFinalizedAt.Load()
		if window > 0 &&
			latestAt > 0 && nowMs-latestAt < window &&
			finalizedAt > 0 && nowMs-finalizedAt < window {
			skipSlots = true
		}
	}
	if skipSlots {
		e.slotPollSkips++
	} else {
		e.slotPollSkips = 0
	}

	// getHealth carries a near-static boolean and feeds only the cordon edge, so
	// run it every Nth poll instead of every one. See healthPollEveryNTicks for
	// why a FAILED probe re-schedules itself onto the next poll.
	runHealth := e.pollCount >= e.nextHealthPoll
	if runHealth {
		e.nextHealthPoll = e.pollCount + healthPollEveryNTicks
	}
	e.pollCount++

	var wg sync.WaitGroup

	// Shared variable for shred → filled by the shred goroutine, read after Wait.
	// Only the shred goroutine writes, so no atomics needed.
	var shredSlot int64

	if runHealth {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer e.recoverPanic("fetchHealth")
			healthy := e.fetchHealth(ctx)
			e.healthy.Store(healthy)
		}()
	}

	if !skipSlots {
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer e.recoverPanic("fetchSlot.processed")
			if slot, err := e.fetchSlot(ctx, reqGetSlotProcessed); err == nil && slot > 0 {
				e.suggestLatestSlot(slot)
			}
		}()

		go func() {
			defer wg.Done()
			defer e.recoverPanic("fetchSlot.finalized")
			if slot, err := e.fetchSlot(ctx, reqGetSlotFinalized); err == nil && slot > 0 {
				e.suggestFinalizedSlot(slot)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer e.recoverPanic("fetchSlot.maxShredInsert")
		if shred, err := e.fetchSlot(ctx, reqGetMaxShredInsertSlot); err == nil && shred > 0 {
			shredSlot = shred
		}
	}()

	wg.Wait()

	// Safe to read LatestSlot() now — the processed-slot goroutine has joined
	// (and on a skipped tick the shared value is traffic-fed and current).
	if shredSlot > 0 {
		e.shredInsertSlot.Store(shredSlot)
		if latest := e.LatestSlot(); latest > 0 {
			// getMaxShredInsertSlot is the BLOCKSTORE-INGESTION watermark and is
			// structurally >= the replayed (processed) slot, so ingestion lag is
			// `maxShredInsertSlot - processedSlot` (DESIGN-MULTI-CHAIN-SOLANA.md §9.2).
			// That subtraction IS the silent-stale detector: shreds keep arriving
			// while replay stalls, so the watermark runs away from the processed
			// slot while the node still answers getHealth "ok". Subtracting the
			// other way inverts it — a degraded node then yields a negative
			// number that clamps to zero and the detector can never fire.
			lag := shredSlot - latest
			if lag < 0 {
				// A watermark BEHIND the processed slot is structurally
				// impossible on one node, so this is a skewed pair of samples:
				// the processed slot came from a later traffic suggestion, or a
				// shared-state peer wrote a higher slot for this upstream. Skew
				// is not evidence of ingestion lag — report none rather than
				// invent a positive lag out of sampling noise.
				lag = 0
			}
			e.maxShredInsertSlotLag.Store(lag)
		}
	}

	// A probe that came back unhealthy cordons this upstream out of rotation
	// below, so it must not ride out the throttle window on the strength of one
	// sample: re-probe on the very next poll. Extra calls happen only while
	// something is actually wrong, and recovery is detected in one debounce
	// instead of N.
	if runHealth && !e.healthy.Load() {
		e.nextHealthPoll = e.pollCount
	}

	// The floor is probed ONLY for an upstream whose operator declared a
	// firstAvailableSlotPlus bound (see declaresFirstAvailableSlotPlus), the
	// same opt-in as EVM's earliestBlockPlus. An upstream with no such bound
	// costs no extra call and carries no observed floor at all.
	//
	// It runs AFTER the fan-out joins, not inside it, because its
	// edge validation compares the sampled floor against this upstream's own
	// head — and inside the fan-out that read races the getSlot goroutine, so
	// LatestSlot() would still be 0 and the check would silently never fire.
	// The cost of serialising it is bounded: TryUpdateIfStale returns on a fast
	// path while the value is fresh, and when it does refresh, the foreground
	// wait is capped by SharedStateConfig.UpdateMaxWait with the fetch
	// continuing asynchronously.
	if e.declaresFirstAvailableSlotPlus() {
		func() {
			defer e.recoverPanic("fetchFirstAvailableSlot")
			e.refreshFirstAvailableSlot(ctx)
		}()
	}

	// A health verdict nobody routes on is not a defense; publish it.
	e.applyHealthToRouting()

	e.lastPollAt.Store(time.Now().UnixMilli())
	return nil
}

func (e *SvmStatePoller) fetchHealth(ctx context.Context) bool {
	resp, err := e.call(ctx, reqGetHealth)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return false
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil || jrr == nil {
		return false
	}
	return jrr.Error == nil
}

func (e *SvmStatePoller) fetchSlot(ctx context.Context, payload []byte) (int64, error) {
	resp, err := e.call(ctx, payload)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return 0, err
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return 0, err
	}
	if jrr.Error != nil {
		return 0, jrr.Error
	}
	raw := string(jrr.GetResultBytes())
	if raw == "" || raw == "null" {
		return 0, nil
	}
	// getSlot / getMaxShredInsertSlot return a bare integer.
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse slot %q: %w", raw, err)
	}
	return v, nil
}

func (e *SvmStatePoller) call(ctx context.Context, payload []byte) (*common.NormalizedResponse, error) {
	req := common.NewNormalizedRequest(payload)
	return e.upstream.Forward(ctx, req, true, false)
}

func (e *SvmStatePoller) LatestSlot() int64 {
	return e.latestSlotShared.GetValue()
}

func (e *SvmStatePoller) FinalizedSlot() int64 {
	return e.finalizedSlotShared.GetValue()
}

func (e *SvmStatePoller) ShredInsertSlot() int64 {
	return e.shredInsertSlot.Load()
}

func (e *SvmStatePoller) MaxShredInsertSlotLag() int64 {
	return e.maxShredInsertSlotLag.Load()
}

// IsHealthy reports both getHealth status and shred-insert-lag health. Nodes that
// receive shreds but don't process them can respond to getHealth while serving
// stale reads, so both signals are required.
//
// Consumed by applyHealthToRouting on every poll tick: a false verdict cordons
// the upstream out of selection. Do not weaken it into a diagnostic-only flag.
func (e *SvmStatePoller) IsHealthy() bool {
	if !e.healthy.Load() {
		return false
	}
	if e.maxShredInsertSlotLag.Load() > common.MaxShredInsertSlotLagThreshold {
		return false
	}
	return true
}

// declaresFirstAvailableSlotPlus reports whether this upstream's operator-
// declared svm.blockAvailability uses a firstAvailableSlotPlus bound on either
// side — the only way an observed floor is consulted. Same spirit as EVM's
// earliestBlockPlus: the floor is polled because a declared bound needs it as
// its anchor, never by default.
func (e *SvmStatePoller) declaresFirstAvailableSlotPlus() bool {
	if e.upstream == nil {
		return false
	}
	cfg := e.upstream.Config()
	if cfg == nil || cfg.Svm == nil || cfg.Svm.BlockAvailability == nil {
		return false
	}
	ba := cfg.Svm.BlockAvailability
	return (ba.Lower != nil && ba.Lower.FirstAvailableSlotPlus != nil) ||
		(ba.Upper != nil && ba.Upper.FirstAvailableSlotPlus != nil)
}

// refreshFirstAvailableSlot re-reads this upstream's ledger floor, debounced by
// the counter itself rather than by the poll gate. TryUpdateIfStale returns on
// a fast path when the value is fresh, so calling it every tick costs one
// atomic load, and it coordinates the actual fetch across erpc instances
// through the counter's updatedAt.
//
// It sits OUTSIDE the skipSlots traffic gate for the same reason getHealth and
// getMaxShredInsertSlot do: live traffic carries no floor signal, so a busy
// method must never suppress this observation.
//
// known is set only from a successful fetch, OnValue, or a not-stale fast path
// that is NOT still waiting on a timed-out refresh. TryUpdateIfStale's
// UpdateMaxWait timeout returns (initialValue, nil) while the RPC continues
// asynchronously — treating that nil as an observation would mark slot 0 known
// on a cold poller and leave firstAvailableSlotPlus enforcing from genesis if
// the async fetch later fails.
func (e *SvmStatePoller) refreshFirstAvailableSlot(ctx context.Context) {
	wasStale := e.firstAvailableSlotShared.IsStale(DefaultFirstAvailableSlotStaleness)
	slot, err := e.firstAvailableSlotShared.TryUpdateIfStale(ctx, DefaultFirstAvailableSlotStaleness,
		func(ctx context.Context) (int64, error) {
			slot, err := e.fetchFirstAvailableSlot(ctx)
			if err != nil {
				// Includes -32601 from an upstream that does not implement the
				// method, and null/empty results. Returning the error leaves
				// the previous value and the known flag untouched, so that
				// upstream is never excluded.
				return 0, err
			}
			if slot < 0 {
				return 0, fmt.Errorf("negative first available slot %d", slot)
			}
			// A node's floor cannot exceed its own head. Validating that here
			// is what stops one buggy or hostile upstream from advertising a
			// floor near MaxInt64 and self-evicting from every slot-named
			// read. When LatestSlot is still 0 the check is skipped, so a cold
			// poller cannot reject a legitimate sample.
			if latest := e.LatestSlot(); latest > 0 && slot > latest {
				return 0, fmt.Errorf("first available slot %d is above own latest slot %d", slot, latest)
			}
			e.firstAvailableSlotPending.Store(false)
			e.firstAvailableSlotKnown.Store(true)
			e.gauge(telemetry.MetricUpstreamSvmFirstAvailableSlot).Set(float64(slot))
			return slot, nil
		})
	if err != nil {
		return
	}
	if e.firstAvailableSlotKnown.Load() {
		e.firstAvailableSlotPending.Store(false)
		e.gauge(telemetry.MetricUpstreamSvmFirstAvailableSlot).Set(float64(slot))
		return
	}
	if !wasStale {
		// Fast path: counter was already fresh before this call. Timeout
		// cannot produce this branch on the tick that timed out. A prior
		// timeout's markFreshAttempt CAN make a later tick look fresh with
		// no observation — firstAvailableSlotPending refuses to promote that
		// zero. Peer-seeded freshness (including a propagated 0 that fired
		// no OnValue) has pending=false and is trusted here.
		if e.firstAvailableSlotPending.Load() {
			return
		}
		e.firstAvailableSlotKnown.Store(true)
		e.gauge(telemetry.MetricUpstreamSvmFirstAvailableSlot).Set(float64(slot))
		return
	}
	// wasStale and still unknown: the foreground wait timed out (or the
	// closure never established known). Remember so a subsequent fast path
	// does not treat markFreshAttempt as a peer observation. Async success
	// clears pending via the closure / OnValue.
	e.firstAvailableSlotPending.Store(true)
}

// fetchFirstAvailableSlot reads the oldest block this upstream can actually
// serve: the first element of getBlocksWithLimit(0, 1). Agave answers it from
// the same storage getBlock reads — long-term storage (BigTable) when the start
// is below the local ledger, else the local ledger's first rooted slot — so the
// slot it names is servable and the one before it is not. getFirstAvailableBlock
// is NOT used: it reports only the local ledger, so a BigTable-backed node
// (Alchemy devnet: 509M) reports a floor far above slots it serves (116M).
// An empty list (no stored block, or a swallowed long-term-storage error) is no
// observation, not a floor.
func (e *SvmStatePoller) fetchFirstAvailableSlot(ctx context.Context) (int64, error) {
	resp, err := e.call(ctx, reqGetOldestStoredBlock)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return 0, err
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return 0, err
	}
	if jrr.Error != nil {
		return 0, jrr.Error
	}
	var slots []int64
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &slots); err != nil {
		return 0, fmt.Errorf("parse getBlocksWithLimit result: %w", err)
	}
	if len(slots) == 0 {
		return 0, errors.New("getBlocksWithLimit(0, 1) returned no block")
	}
	return slots[0], nil
}

// gauge resolves a per-upstream gauge for this poller's upstream. Labels match
// the tracker's convention so SVM series join with the generic upstream ones.
func (e *SvmStatePoller) gauge(vec *prometheus.GaugeVec) prometheus.Gauge {
	if e.upstream == nil {
		// Same guard applyHealthToRouting applies: a poller without an
		// upstream has nothing to label a series with.
		return vec.WithLabelValues(e.projectId, "", "", "")
	}
	return vec.WithLabelValues(
		e.projectId,
		e.upstream.VendorName(),
		e.upstream.NetworkLabel(),
		e.upstream.Id(),
	)
}

// FirstAvailableSlot reports this upstream's ledger floor.
//
// known=false means never observed — a cold poller, a failed fetch, or an
// upstream without the method — and callers MUST read that as "no constraint",
// never as "nothing available".
func (e *SvmStatePoller) FirstAvailableSlot() (int64, bool) {
	if !e.firstAvailableSlotKnown.Load() {
		return 0, false
	}
	return e.firstAvailableSlotShared.GetValue(), true
}

// applyHealthToRouting publishes the poller's verdict to upstream selection.
// Cordon is the established mechanism — the default selection policy runs
// `.removeCordoned()`, so a cordoned upstream is dropped from routing for every
// request until it recovers; EvmStatePoller.cordonForChainIdMismatch is the
// precedent. Without this, IsHealthy() has no production consumer and neither a
// failing getHealth nor a runaway shred-insert lag affects where traffic goes.
//
// EDGE-TRIGGERED via cordonedByHealth: Cordon/Uncordon fire only on a
// transition, never once per tick. Re-cordoning every 400ms would restamp the
// reason (spawning a fresh `erpc_upstream_cordoned` gauge series per tick and
// resetting the cordon-duration observation), and the flag also means a
// recovering poller lifts only ITS OWN cordon, never an operator's manual one.
//
// A cold poller cannot cordon: healthy starts true and maxShredInsertSlotLag
// stays 0 until a real getMaxShredInsertSlot sample lands, so "not yet
// observed" reads as healthy. Unknown != unhealthy.
//
// Recovery is observable because Upstream.Forward does not consult cordon state
// (only the selection policy does), so the poller keeps polling while cordoned.
func (e *SvmStatePoller) applyHealthToRouting() {
	if e.upstream == nil {
		return
	}
	if e.IsHealthy() {
		if e.cordonedByHealth.CompareAndSwap(true, false) {
			e.logger.Info().
				Int64("shredInsertSlotLag", e.maxShredInsertSlotLag.Load()).
				Msg("svm upstream recovered; uncordoning")
			e.upstream.Uncordon("*", "svm state poller: getHealth ok and shred-insert lag back within threshold")
		}
		return
	}
	if !e.cordonedByHealth.CompareAndSwap(false, true) {
		return
	}
	reason := "svm state poller: getHealth reported unhealthy"
	if e.healthy.Load() {
		reason = fmt.Sprintf(
			"svm state poller: shred-insert lag %d slots exceeds threshold %d (node ingests shreds but does not replay them)",
			e.maxShredInsertSlotLag.Load(), common.MaxShredInsertSlotLagThreshold,
		)
	}
	e.logger.Warn().
		Int64("shredInsertSlot", e.shredInsertSlot.Load()).
		Int64("shredInsertSlotLag", e.maxShredInsertSlotLag.Load()).
		Int64("latestSlot", e.LatestSlot()).
		Str("reason", reason).
		Msg("svm upstream unhealthy; cordoning out of rotation")
	e.upstream.Cordon("*", reason)
}

// SuggestLatestSlot ingests an externally-observed processed/latest slot —
// live-traffic context.slot harvesting (upstreamPostForward_trackContextSlot)
// or a shared-state neighbor. External observations double as freshness
// evidence for the poll traffic gate; the poller's own fetches use the private
// variant so a poll never satisfies its own gate.
func (e *SvmStatePoller) SuggestLatestSlot(slot int64) {
	if slot <= 0 {
		return
	}
	e.lastExternalLatestAt.Store(time.Now().UnixMilli())
	e.suggestLatestSlot(slot)
}

func (e *SvmStatePoller) suggestLatestSlot(slot int64) {
	if slot <= 0 {
		return
	}
	e.latestSlotShared.TryUpdate(e.appCtx, slot)
	// Tracker feed lives in the counter's OnValue callback (NewSvmStatePoller):
	// it also covers traffic-fed and cross-instance updates, and skips values
	// the counter rejected.
}

// SuggestFinalizedSlot is the finalized-commitment sibling of
// SuggestLatestSlot; same external-vs-internal split.
func (e *SvmStatePoller) SuggestFinalizedSlot(slot int64) {
	if slot <= 0 {
		return
	}
	e.lastExternalFinalizedAt.Store(time.Now().UnixMilli())
	e.suggestFinalizedSlot(slot)
}

func (e *SvmStatePoller) suggestFinalizedSlot(slot int64) {
	if slot <= 0 {
		return
	}
	e.finalizedSlotShared.TryUpdate(e.appCtx, slot)
	// Tracker feed: see suggestLatestSlot.
}
