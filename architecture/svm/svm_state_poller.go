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

// Static request payloads — avoid allocating on every tick.
var (
	reqGetHealth             = []byte(`{"jsonrpc":"2.0","id":1,"method":"getHealth","params":[]}`)
	reqGetSlotProcessed      = []byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"processed"}]}`)
	reqGetSlotFinalized      = []byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"finalized"}]}`)
	reqGetMaxShredInsertSlot = []byte(`{"jsonrpc":"2.0","id":1,"method":"getMaxShredInsertSlot","params":[]}`)
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
	pollMu        sync.Mutex

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

	latestShared := sharedState.GetCounterInt64(latestKey, DefaultToleratedSlotRollback)
	finalizedShared := sharedState.GetCounterInt64(finalizedKey, DefaultToleratedSlotRollback)

	e := &SvmStatePoller{
		projectId:           projectId,
		appCtx:              appCtx,
		logger:              &lg,
		upstream:            up,
		tracker:             tracker,
		latestSlotShared:    latestShared,
		finalizedSlotShared: finalizedShared,
	}

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
// getHealth and getMaxShredInsertSlot always run: traffic carries neither
// signal. Bounded by maxConsecutiveSlotPollSkips so suggestions can never
// fully replace the poller's own observations.
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

	var wg sync.WaitGroup

	// Shared variable for shred → filled by the shred goroutine, read after Wait.
	// Only the shred goroutine writes, so no atomics needed.
	var shredSlot int64

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer e.recoverPanic("fetchHealth")
		healthy := e.fetchHealth(ctx)
		e.healthy.Store(healthy)
	}()

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
