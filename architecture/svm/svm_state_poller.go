package svm

import (
	"context"
	"errors"
	"fmt"
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
	pollMu           sync.Mutex
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

func (e *SvmStatePoller) Bootstrap(ctx context.Context) error {
	// The debounce gate defaults to one slot when not configured. It may have
	// already been set via SetDebounceInterval (config can arrive before
	// Bootstrap), so only fill the default when still unset.
	if e.debounceInterval.Load() <= 0 {
		e.debounceInterval.Store(int64(DefaultPollInterval))
	}
	e.Enabled = true
	e.logger.Debug().
		Dur("tickInterval", DefaultPollInterval).
		Dur("debounce", time.Duration(e.debounceInterval.Load())).
		Msg("bootstrapping svm state poller")

	// The ticker stays at the fixed one-slot cadence (cheap, no I/O); the
	// debounce gate in Poll throttles the actual network fan-out to the
	// configured rate.
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
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.appCtx.Done():
			e.logger.Debug().Msg("shutting down svm state poller due to app context interruption")
			return
		case <-ticker.C:
			nctx, cancel := context.WithTimeout(e.appCtx, 15*time.Second)
			if err := e.Poll(nctx); err != nil {
				if !errors.Is(nctx.Err(), context.Canceled) {
					e.logger.Warn().Err(err).Msg("svm state poll failed")
				}
			}
			cancel()
		}
	}
}

// Poll fans out four RPC calls in parallel. Each result updates its own field so a
// single failure doesn't blank the others. Shred-insert lag is computed after
// the fan-out joins — otherwise the getMaxShredInsertSlot goroutine races
// against the getSlot goroutine and reads a stale (or zero) latest slot.
func (e *SvmStatePoller) Poll(ctx context.Context) error {
	e.pollMu.Lock()
	defer e.pollMu.Unlock()

	if d := time.Duration(e.debounceInterval.Load()); d > 0 {
		last := e.lastPollAt.Load()
		if last > 0 && time.Since(time.UnixMilli(last)) < d {
			return nil
		}
	}

	var wg sync.WaitGroup
	wg.Add(4)

	// Shared variable for shred → filled by the shred goroutine, read after Wait.
	// Only the shred goroutine writes, so no atomics needed.
	var shredSlot int64

	go func() {
		defer wg.Done()
		healthy := e.fetchHealth(ctx)
		e.healthy.Store(healthy)
	}()

	go func() {
		defer wg.Done()
		if slot, err := e.fetchSlot(ctx, reqGetSlotProcessed); err == nil && slot > 0 {
			e.SuggestLatestSlot(slot)
		}
	}()

	go func() {
		defer wg.Done()
		if slot, err := e.fetchSlot(ctx, reqGetSlotFinalized); err == nil && slot > 0 {
			e.SuggestFinalizedSlot(slot)
		}
	}()

	go func() {
		defer wg.Done()
		if shred, err := e.fetchSlot(ctx, reqGetMaxShredInsertSlot); err == nil && shred > 0 {
			shredSlot = shred
		}
	}()

	wg.Wait()

	// Safe to read LatestSlot() now — the processed-slot goroutine has joined.
	if shredSlot > 0 {
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

func (e *SvmStatePoller) SuggestLatestSlot(slot int64) {
	if slot <= 0 {
		return
	}
	e.latestSlotShared.TryUpdate(e.appCtx, slot)
}

func (e *SvmStatePoller) SuggestFinalizedSlot(slot int64) {
	if slot <= 0 {
		return
	}
	e.finalizedSlotShared.TryUpdate(e.appCtx, slot)
}
