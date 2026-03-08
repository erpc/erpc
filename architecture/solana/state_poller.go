package solana

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
)

const defaultSlotDebounce = 400 * time.Millisecond // ~1 Solana slot

// Pre-computed static request bodies — avoids fmt.Sprintf allocations on every poll tick.
var (
	reqGetHealth              = []byte(`{"jsonrpc":"2.0","id":1,"method":"getHealth","params":[]}`)
	reqGetSlotProcessed       = []byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"processed"}]}`)
	reqGetSlotFinalized       = []byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"finalized"}]}`)
	reqGetMaxShredInsertSlot  = []byte(`{"jsonrpc":"2.0","id":1,"method":"getMaxShredInsertSlot","params":[]}`)
)

// MaxShredInsertSlotLagThreshold is the number of slots by which a node's
// max-shred-insert slot may lead its processed slot before it is marked
// degraded. A large delta means the node receives block shreds from the
// network but fails to process them — it can still pass getHealth.
const MaxShredInsertSlotLagThreshold int64 = 100

// DefaultToleratedSlotRollback is the maximum slot decrease considered normal
// churn (e.g. processed → confirmed reorg). Larger rollbacks trigger alarms.
// Mirrors evm.DefaultToleratedBlockHeadRollback.
const DefaultToleratedSlotRollback int64 = 512

var _ common.SolanaStatePoller = &SolanaStatePoller{}

// SolanaStatePoller tracks the latest and finalized slot for a Solana upstream,
// and polls the node's health via getHealth. Mirrors EvmStatePoller.
//
// Slot state is stored via common.SlotSharedVariable — a narrow interface
// satisfied by data.CounterInt64SharedVariable. This allows the upstream
// package (which imports data) to wire shared-state backends without creating
// an import cycle through clients → architecture/solana → data.
type SolanaStatePoller struct {
	Enabled bool

	appCtx   context.Context
	logger   *zerolog.Logger
	upstream common.Upstream
	tracker  *health.Tracker

	// latestSlotShared and finalizedSlotShared feed into health.Tracker via
	// OnValue callbacks, which drives BlockHeadLag scoring for upstream routing.
	// They also propagate across erpc instances via the shared-state backend
	// (Redis/DynamoDB/in-memory) when a data.SharedStateRegistry is wired.
	latestSlotShared    common.SlotSharedVariable
	finalizedSlotShared common.SlotSharedVariable

	healthy            atomic.Bool
	shredInsertLag     atomic.Int64 // maxShredInsertSlot − processedSlot; 0 = unknown
	debounceInterval   time.Duration
	lastPollTime       time.Time
	pollMu             sync.Mutex

	skipFinalizedCheck bool
	finalizedFailCount int
	latestFailCount    int
	healthFailCount    int
}

// NewSolanaStatePoller creates a new poller. latestSlotVar and finalizedSlotVar
// are common.SlotSharedVariable values created by the caller (upstream.go) using
// the data.SharedStateRegistry, breaking the import cycle.
//
// Both variables MUST be non-nil. The caller registers OnValue / OnLargeRollback
// callbacks here so the tracker is notified when slots advance.
func NewSolanaStatePoller(
	appCtx context.Context,
	logger *zerolog.Logger,
	upstream common.Upstream,
	tracker *health.Tracker,
	latestSlotVar common.SlotSharedVariable,
	finalizedSlotVar common.SlotSharedVariable,
) *SolanaStatePoller {
	lg := logger.With().
		Str("component", "solanaStatePoller").
		Str("upstreamId", upstream.Id()).
		Logger()

	p := &SolanaStatePoller{
		Enabled:             true,
		appCtx:              appCtx,
		logger:              &lg,
		upstream:            upstream,
		tracker:             tracker,
		latestSlotShared:    latestSlotVar,
		finalizedSlotShared: finalizedSlotVar,
		debounceInterval:    defaultSlotDebounce,
	}

	// Wire tracker callbacks — these drive BlockHeadLag scoring used in
	// upstream selection. Called whenever the shared variable advances.
	latestSlotVar.OnValue(func(slot int64) {
		// Pass 0 for timestamp — Solana blocks don't carry Unix timestamps.
		p.tracker.SetLatestBlockNumber(p.upstream, slot, 0)
	})
	finalizedSlotVar.OnValue(func(slot int64) {
		p.tracker.SetFinalizedBlockNumber(p.upstream, slot)
	})

	// Large-rollback alarms.
	latestSlotVar.OnLargeRollback(func(currentVal, newVal int64) {
		p.tracker.RecordBlockHeadLargeRollback(p.upstream, "latest", currentVal, newVal)
	})
	finalizedSlotVar.OnLargeRollback(func(currentVal, newVal int64) {
		p.tracker.RecordBlockHeadLargeRollback(p.upstream, "finalized", currentVal, newVal)
	})

	// Optimistically assume healthy until the first health poll completes.
	p.healthy.Store(true)
	return p
}

func (p *SolanaStatePoller) Bootstrap(ctx context.Context) error {
	if !p.Enabled {
		return nil
	}
	// Initial synchronous poll (non-fatal — node may not be ready yet).
	if err := p.Poll(ctx); err != nil {
		p.logger.Warn().Err(err).Msg("initial solana slot poll failed — will retry in background")
	}
	// Background polling goroutine — mirrors EvmStatePoller.Bootstrap.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				p.logger.Error().
					Interface("panic", r).
					Str("stack", string(debug.Stack())).
					Msg("solana state poller goroutine panicked")
			}
		}()
		ticker := time.NewTicker(p.debounceInterval)
		defer ticker.Stop()
		for {
			select {
			case <-p.appCtx.Done():
				return
			case <-ticker.C:
				if err := p.Poll(p.appCtx); err != nil {
					p.logger.Debug().Err(err).Msg("solana slot poll failed")
				}
			}
		}
	}()
	return nil
}

func (p *SolanaStatePoller) Poll(ctx context.Context) error {
	p.pollMu.Lock()
	if time.Since(p.lastPollTime) < p.debounceInterval {
		p.pollMu.Unlock()
		return nil
	}
	p.lastPollTime = time.Now()
	p.pollMu.Unlock()

	// Fan out four independent RPC calls concurrently so a slow node
	// doesn't serialise round-trips per tick.
	type slotResult struct {
		slot int64
		err  error
	}
	healthErrCh := make(chan error, 1)
	latestCh    := make(chan slotResult, 1)
	finalizedCh := make(chan slotResult, 1)
	shredCh     := make(chan slotResult, 1)

	go func() { healthErrCh <- p.PollHealth(ctx) }()
	go func() {
		s, e := p.PollProcessedSlot(ctx)
		latestCh <- slotResult{s, e}
	}()
	if !p.skipFinalizedCheck {
		go func() {
			s, e := p.PollFinalizedSlot(ctx)
			finalizedCh <- slotResult{s, e}
		}()
	} else {
		finalizedCh <- slotResult{} // unblock channel read
	}
	go func() {
		s, e := p.PollMaxShredInsertSlot(ctx)
		shredCh <- slotResult{s, e}
	}()

	healthErr := <-healthErrCh
	if healthErr != nil {
		p.healthFailCount++
		p.logger.Debug().Err(healthErr).Msg("solana getHealth poll failed")
	} else {
		p.healthFailCount = 0
	}

	latestRes := <-latestCh
	if latestRes.err != nil {
		p.latestFailCount++
		p.logger.Debug().Err(latestRes.err).Msg("failed to poll processed slot")
	} else {
		p.latestFailCount = 0
		p.SuggestLatestSlot(latestRes.slot)
	}

	if !p.skipFinalizedCheck {
		fRes := <-finalizedCh
		if fRes.err != nil {
			p.finalizedFailCount++
			if p.finalizedFailCount >= 5 {
				p.logger.Warn().
					Err(fRes.err).
					Msg("disabling finalized slot polling after 5 consecutive failures")
				p.skipFinalizedCheck = true
			}
		} else {
			p.finalizedFailCount = 0
			p.SuggestFinalizedSlot(fRes.slot)
		}
	}

	// getMaxShredInsertSlot: if it leads the processed slot by more than
	// MaxShredInsertSlotLagThreshold, the node is receiving shreds but failing
	// to process them. Mark it degraded even when getHealth returns "ok".
	shredRes := <-shredCh
	if shredRes.err == nil && latestRes.err == nil && latestRes.slot > 0 {
		lag := shredRes.slot - latestRes.slot
		if lag < 0 {
			lag = 0
		}
		p.shredInsertLag.Store(lag)
		if lag > MaxShredInsertSlotLagThreshold {
			p.logger.Warn().
				Int64("maxShredInsertSlot", shredRes.slot).
				Int64("processedSlot", latestRes.slot).
				Int64("lag", lag).
				Msg("shred-insert lag exceeds threshold — marking upstream degraded")
			p.healthy.Store(false)
		}
	}

	return latestRes.err
}

// PollHealth calls getHealth and updates the healthy flag.
// A node reports healthy when result == "ok"; any RPC error means unhealthy.
func (p *SolanaStatePoller) PollHealth(ctx context.Context) error {
	pr := common.NewNormalizedRequest(reqGetHealth)

	resp, err := p.upstream.Forward(ctx, pr, true)
	if err != nil {
		p.healthy.Store(false)
		return fmt.Errorf("getHealth: %w", err)
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		p.healthy.Store(false)
		return fmt.Errorf("getHealth parse: %w", err)
	}
	if jrr.Error != nil {
		// -32005 = node is unhealthy / behind; any error means not serving well.
		p.healthy.Store(false)
		return fmt.Errorf("getHealth rpc error %d: %s", jrr.Error.Code, jrr.Error.Message)
	}

	// result should be the string "ok"
	var status string
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &status); err != nil {
		// Non-fatal: if we can't parse but there's no error, treat as healthy.
		p.healthy.Store(true)
		return nil
	}
	p.healthy.Store(status == "ok")
	return nil
}

func (p *SolanaStatePoller) PollProcessedSlot(ctx context.Context) (int64, error) {
	return p.pollSlot(ctx, reqGetSlotProcessed, string(common.SolanaCommitmentProcessed))
}

func (p *SolanaStatePoller) PollFinalizedSlot(ctx context.Context) (int64, error) {
	return p.pollSlot(ctx, reqGetSlotFinalized, string(common.SolanaCommitmentFinalized))
}

// PollMaxShredInsertSlot calls getMaxShredInsertSlot and returns the slot number.
// This is the highest slot for which the node has inserted shreds — it leads
// the processed slot when the node's processing pipeline is backlogged.
func (p *SolanaStatePoller) PollMaxShredInsertSlot(ctx context.Context) (int64, error) {
	pr := common.NewNormalizedRequest(reqGetMaxShredInsertSlot)
	resp, err := p.upstream.Forward(ctx, pr, true)
	if err != nil {
		return 0, fmt.Errorf("getMaxShredInsertSlot: %w", err)
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return 0, fmt.Errorf("getMaxShredInsertSlot parse: %w", err)
	}
	if jrr.Error != nil {
		return 0, fmt.Errorf("getMaxShredInsertSlot rpc error: %s", jrr.Error.Message)
	}
	var slot int64
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &slot); err != nil {
		return 0, fmt.Errorf("getMaxShredInsertSlot unmarshal: %w", err)
	}
	return slot, nil
}

// MaxShredInsertSlotLag returns the last measured difference between the node's
// maxShredInsertSlot and its processedSlot. Returns 0 when unknown (first poll
// not yet complete or either call failed).
func (p *SolanaStatePoller) MaxShredInsertSlotLag() int64 {
	return p.shredInsertLag.Load()
}

func (p *SolanaStatePoller) pollSlot(ctx context.Context, reqBody []byte, commitment string) (int64, error) {
	pr := common.NewNormalizedRequest(reqBody)

	resp, err := p.upstream.Forward(ctx, pr, true)
	if err != nil {
		return 0, fmt.Errorf("getSlot(%s): %w", commitment, err)
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return 0, fmt.Errorf("getSlot(%s) parse: %w", commitment, err)
	}
	if jrr.Error != nil {
		return 0, fmt.Errorf("getSlot(%s) rpc error: %s", commitment, jrr.Error.Message)
	}

	var slot int64
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &slot); err != nil {
		return 0, fmt.Errorf("getSlot(%s) unmarshal: %w", commitment, err)
	}
	return slot, nil
}

func (p *SolanaStatePoller) LatestSlot() int64 {
	if p.latestSlotShared == nil {
		return 0
	}
	return p.latestSlotShared.GetValue()
}

func (p *SolanaStatePoller) FinalizedSlot() int64 {
	if p.finalizedSlotShared == nil {
		return 0
	}
	return p.finalizedSlotShared.GetValue()
}

// IsHealthy returns true if the last getHealth call succeeded with "ok".
// Defaults to true until the first health poll completes.
func (p *SolanaStatePoller) IsHealthy() bool {
	return p.healthy.Load()
}

// SuggestLatestSlot forwards the slot value into the shared variable, triggering
// OnValue → tracker.SetLatestBlockNumber → BlockHeadLag scoring.
func (p *SolanaStatePoller) SuggestLatestSlot(slot int64) {
	if p.latestSlotShared == nil {
		return
	}
	p.latestSlotShared.TryUpdate(p.appCtx, slot)
}

// SuggestFinalizedSlot forwards the slot value into the shared variable.
func (p *SolanaStatePoller) SuggestFinalizedSlot(slot int64) {
	if p.finalizedSlotShared == nil {
		return
	}
	p.finalizedSlotShared.TryUpdate(p.appCtx, slot)
}

func (p *SolanaStatePoller) IsObjectNull() bool {
	return p == nil || !p.Enabled
}
