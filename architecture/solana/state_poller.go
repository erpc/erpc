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

var _ common.SolanaStatePoller = &SolanaStatePoller{}

// SolanaStatePoller tracks the latest and finalized slot for a Solana upstream.
// It mirrors the pattern of EvmStatePoller (polls on a ticker, shares state).
type SolanaStatePoller struct {
	Enabled bool

	projectId string
	appCtx    context.Context
	logger    *zerolog.Logger
	upstream  common.Upstream
	tracker   *health.Tracker

	latestSlot    atomic.Int64
	finalizedSlot atomic.Int64

	debounceInterval time.Duration
	lastPollTime     time.Time
	pollMu           sync.Mutex

	skipFinalizedCheck bool
	finalizedFailCount int
	latestFailCount    int
}

func NewSolanaStatePoller(
	projectId string,
	appCtx context.Context,
	logger *zerolog.Logger,
	upstream common.Upstream,
	tracker *health.Tracker,
) *SolanaStatePoller {
	debounce := defaultSlotDebounce
	if cfg := upstream.Config(); cfg.Solana != nil && cfg.Solana.Cluster != "" {
		// Could read network config debounce here if available
	}
	lg := logger.With().
		Str("component", "solanaStatePoller").
		Str("upstreamId", upstream.Id()).
		Logger()
	return &SolanaStatePoller{
		Enabled:          true,
		projectId:        projectId,
		appCtx:           appCtx,
		logger:           &lg,
		upstream:         upstream,
		tracker:          tracker,
		debounceInterval: debounce,
	}
}

func (p *SolanaStatePoller) Bootstrap(ctx context.Context) error {
	if !p.Enabled {
		return nil
	}
	// Initial synchronous poll (non-fatal on error)
	if err := p.Poll(ctx); err != nil {
		p.logger.Warn().Err(err).Msg("initial solana slot poll failed — will retry in background")
	}
	// Background polling goroutine
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

	slot, err := p.PollProcessedSlot(ctx)
	if err != nil {
		p.latestFailCount++
		p.logger.Debug().Err(err).Msg("failed to poll processed slot")
	} else {
		p.latestFailCount = 0
		p.SuggestLatestSlot(slot)
	}

	if !p.skipFinalizedCheck {
		fSlot, fErr := p.PollFinalizedSlot(ctx)
		if fErr != nil {
			p.finalizedFailCount++
			if p.finalizedFailCount >= 5 {
				p.logger.Warn().
					Err(fErr).
					Msg("disabling finalized slot polling after 5 consecutive failures")
				p.skipFinalizedCheck = true
			}
		} else {
			p.finalizedFailCount = 0
			p.SuggestFinalizedSlot(fSlot)
		}
	}
	return err
}

func (p *SolanaStatePoller) PollProcessedSlot(ctx context.Context) (int64, error) {
	return p.pollSlot(ctx, string(common.SolanaCommitmentProcessed))
}

func (p *SolanaStatePoller) PollFinalizedSlot(ctx context.Context) (int64, error) {
	return p.pollSlot(ctx, string(common.SolanaCommitmentFinalized))
}

func (p *SolanaStatePoller) pollSlot(ctx context.Context, commitment string) (int64, error) {
	pr := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":%q}]}`, commitment),
	))

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
	return p.latestSlot.Load()
}

func (p *SolanaStatePoller) FinalizedSlot() int64 {
	return p.finalizedSlot.Load()
}

func (p *SolanaStatePoller) SuggestLatestSlot(slot int64) {
	for {
		current := p.latestSlot.Load()
		if slot > current {
			if p.latestSlot.CompareAndSwap(current, slot) {
				break
			}
		} else {
			break
		}
	}
}

func (p *SolanaStatePoller) SuggestFinalizedSlot(slot int64) {
	for {
		current := p.finalizedSlot.Load()
		if slot > current {
			if p.finalizedSlot.CompareAndSwap(current, slot) {
				break
			}
		} else {
			break
		}
	}
}

func (p *SolanaStatePoller) IsObjectNull() bool {
	return p == nil || !p.Enabled
}
