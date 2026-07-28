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

// SolanaSlotDuration is the nominal Solana slot time. Used as the default
// traffic-gate / coalesce window and as the unit for tip-staleness margins.
const SolanaSlotDuration = 400 * time.Millisecond

// DefaultStatePollerInterval is the background ticker cadence for health/shred
// (and getSlot when not traffic-gated). Kept well above one slot to limit
// quota; live traffic still refreshes slot views via context.slot.
const DefaultStatePollerInterval = 5 * time.Second

// DefaultStatePollerDebounce is the coalesce / traffic-gate window (one slot).
const DefaultStatePollerDebounce = SolanaSlotDuration

// DefaultPollInterval is kept as an alias of SolanaSlotDuration for older
// call sites/tests that treated "one slot" as the poller default.
const DefaultPollInterval = SolanaSlotDuration

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
	Enabled bool

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

	// pollInterval is the TICKER cadence (see SvmNetworkConfig.StatePollerInterval).
	// getHealth / getMaxShredInsertSlot run at most once per this period; getSlot
	// may still be skipped by the traffic gate. Stored as nanoseconds so
	// SetPollInterval can update it race-free while loop sleeps between ticks.
	pollInterval atomic.Int64

	// debounceInterval is the GATE: coalesce whole Poll() calls that land
	// within this window, and skip the two getSlot calls when live traffic
	// refreshed both views within it (see SvmNetworkConfig.StatePollerDebounce).
	// Must stay ≤ pollInterval so ticker fires are not skipped. Stored as
	// nanoseconds for race-free SetDebounceInterval updates.
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

	e := &SvmStatePoller{
		projectId:           projectId,
		appCtx:              appCtx,
		logger:              &lg,
		upstream:            up,
		tracker:             tracker,
		latestSlotShared:    sharedState.GetCounterInt64(latestKey, DefaultToleratedSlotRollback),
		finalizedSlotShared: sharedState.GetCounterInt64(finalizedKey, DefaultToleratedSlotRollback),
	}

	// Start healthy — flipped to false on first failing getHealth.
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
	// Interval/debounce may already have been set via SetPollInterval /
	// SetDebounceInterval (config can arrive before Bootstrap). Only fill
	// defaults when still unset.
	if e.pollInterval.Load() <= 0 {
		e.pollInterval.Store(int64(DefaultStatePollerInterval))
	}
	if e.debounceInterval.Load() <= 0 {
		e.debounceInterval.Store(int64(DefaultStatePollerDebounce))
	}
	e.Enabled = true
	e.logger.Debug().
		Dur("tickInterval", time.Duration(e.pollInterval.Load())).
		Dur("debounce", time.Duration(e.debounceInterval.Load())).
		Msg("bootstrapping svm state poller")

	// Loop reads pollInterval each sleep so SetPollInterval after Bootstrap
	// (SetNetworkConfig often lands later) takes effect on the next tick.
	go e.loop()
	return nil
}

// SetPollInterval wires the configured ticker cadence from
// SvmNetworkConfig.StatePollerInterval (see upstream.SetNetworkConfig).
// Callable at any time; takes effect on the next loop sleep.
func (e *SvmStatePoller) SetPollInterval(d time.Duration) {
	if d > 0 {
		e.pollInterval.Store(int64(d))
	}
}

// SetDebounceInterval wires the configured coalesce / traffic-gate window from
// SvmNetworkConfig.StatePollerDebounce (see upstream.SetNetworkConfig). Callable
// at any time; takes effect on the next Poll.
func (e *SvmStatePoller) SetDebounceInterval(d time.Duration) {
	if d > 0 {
		e.debounceInterval.Store(int64(d))
	}
}

func (e *SvmStatePoller) loop() {
	for {
		interval := time.Duration(e.pollInterval.Load())
		if interval <= 0 {
			// Not yet configured / disabled — wait and re-check so a late
			// SetPollInterval can still start polling.
			select {
			case <-e.appCtx.Done():
				e.logger.Debug().Msg("shutting down svm state poller due to app context interruption")
				return
			case <-time.After(time.Second):
				continue
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-e.appCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			e.logger.Debug().Msg("shutting down svm state poller due to app context interruption")
			return
		case <-timer.C:
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
			lag := latest - shredSlot
			if lag < 0 {
				lag = 0
			}
			e.maxShredInsertSlotLag.Store(lag)
		}
	}

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
func (e *SvmStatePoller) IsHealthy() bool {
	if !e.healthy.Load() {
		return false
	}
	if e.maxShredInsertSlotLag.Load() > common.MaxShredInsertSlotLagThreshold {
		return false
	}
	return true
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
	// Feed the shared health tracker so processed-slot lag drives score-based
	// upstream selection on every path — not just the consensus slot-lag
	// pre-filter (design §8). Solana slots carry no block timestamp, so pass 0.
	if e.tracker != nil {
		e.tracker.SetLatestBlockNumber(e.upstream, slot, 0)
	}
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
	// Feed finalized-slot lag into the tracker (FinalizationLag) for scoring.
	if e.tracker != nil {
		e.tracker.SetFinalizedBlockNumber(e.upstream, slot)
	}
}
