package evm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

const FullySyncedThreshold = 4

type EvmStatePoller struct {
	Enabled bool

	appCtx   context.Context
	logger   *zerolog.Logger
	upstream common.Upstream
	cfg      *common.EvmNetworkConfig
	tracker  *health.Tracker

	// When node is fully synced we don't need to query syncing state anymore.
	// A number is used so that at least X times the upstream tells us it's synced.
	// During checks, if a syncing=true is returned we must reset this counter.
	//
	// This approach slightly helps in two scenarios:
	// - When self-hosted nodes have issues flipping back and forth between syncing state.
	// - When a third-party provider is using a degraded network of upstream nodes that are in different states.
	//
	// To save memory, 0 means we haven't checked, 1+ means we have checked but it's still syncing OR
	// we haven't received enough "synced" responses to assume it's fully synced.
	synced       int8
	syncingState common.EvmSyncingState

	// Certain upstreams do not support calling such method,
	// therefore we must avoid sending redundant requests.
	// We will return "nil" as status for syncing which means we don't know for certain.
	skipSyncingCheck bool

	// Certain networks and nodes do not support "finalized" tag,
	// therefore we must avoid sending redundant requests.
	skipFinalizedCheck   bool
	latestBlockNumber    int64
	finalizedBlockNumber int64

	mu sync.RWMutex
}

func NewEvmStatePoller(
	appCtx context.Context,
	logger *zerolog.Logger,
	up common.Upstream,
	tracker *health.Tracker,
) *EvmStatePoller {
	lg := logger.With().Str("upstreamId", up.Config().Id).Logger()
	e := &EvmStatePoller{
		appCtx:   appCtx,
		logger:   &lg,
		upstream: up,
		tracker:  tracker,
	}

	return e
}

func (e *EvmStatePoller) Bootstrap(ctx context.Context) error {
	cfg := e.upstream.Config()
	var intvl string
	if cfg.Evm != nil && cfg.Evm.StatePollerInterval != "" {
		intvl = cfg.Evm.StatePollerInterval
	}
	if intvl == "" {
		e.logger.Debug().Msgf("skipping evm state poller for upstream as no interval is provided")
		return nil
	}
	interval, err := time.ParseDuration(intvl)
	if err != nil {
		return fmt.Errorf("invalid state poller interval: %v", err)
	}

	if interval == 0 {
		e.logger.Debug().Msg("skipping evm state poller for upstream as interval is 0")
		return nil
	} else {
		e.logger.Info().Msgf("bootstraped evm state poller to track upstream latest, finalized blocks and syncing states")
	}

	e.Enabled = true

	go (func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-e.appCtx.Done():
				e.logger.Debug().Msg("shutting down evm state poller due to app context interruption")
				return
			case <-ticker.C:
				nctx, cancel := context.WithTimeout(e.appCtx, 10*time.Second)
				defer cancel()
				err := e.Poll(nctx)
				if err != nil {
					e.logger.Error().Err(err).Msgf("failed to poll evm state for upstream %s", e.upstream.Config().Id)
				}
			}
		}
	})()

	return e.Poll(ctx)
}

func (e *EvmStatePoller) SetNetworkConfig(cfg *common.EvmNetworkConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg = cfg
}

func (e *EvmStatePoller) Poll(ctx context.Context) error {
	var wg sync.WaitGroup
	var errs []error
	ermu := &sync.Mutex{}

	// Fetch latest block
	wg.Add(1)
	go func() {
		defer wg.Done()
		lb, err := e.fetchLatestBlockNumber(ctx)
		if err != nil {
			e.logger.Debug().Err(err).Msg("failed to get latest block number in evm state poller")
			ermu.Lock()
			errs = append(errs, err)
			ermu.Unlock()
			return
		}
		e.logger.Debug().Int64("blockNumber", lb).Msg("fetched latest block")
		if lb > 0 {
			e.setLatestBlockNumber(lb)
		}
	}()

	// Fetch finalized block (if upstream supports)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if e.shouldSkipFinalizedCheck() {
			return
		}

		fb, err := e.fetchFinalizedBlockNumber(ctx)
		if err != nil {
			if common.IsClientError(err) || common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
				e.setSkipFinalizedCheck(true)
				e.logger.Warn().Err(err).Msg("upstream does not support fetching finalized block number in evm state poller")
			} else {
				e.logger.Debug().Err(err).Msg("failed to get finalized block number in evm state poller")
			}
			ermu.Lock()
			errs = append(errs, err)
			ermu.Unlock()
			return
		}
		e.logger.Debug().Int64("blockNumber", fb).Msg("fetched finalized block")
		if fb > 0 {
			e.setFinalizedBlockNumber(fb)
		}
	}()

	// Fetch "syncing" state
	wg.Add(1)
	go func() {
		defer wg.Done()
		if e.synced > FullySyncedThreshold || e.skipSyncingCheck {
			return
		}

		syncing, err := e.fetchSyncingState(ctx)
		if err != nil {
			e.logger.Debug().Bool("syncingResult", syncing).Err(err).Msg("failed to get syncing state in evm state poller")
			ermu.Lock()
			errs = append(errs, err)
			ermu.Unlock()
			if common.IsClientError(err) {
				e.skipSyncingCheck = true
			}
			return
		}

		e.logger.Debug().Bool("syncingResult", syncing).Msg("fetched syncing state")

		e.mu.Lock()
		defer e.mu.Unlock()
		if syncing {
			e.synced = 1
		} else {
			e.synced++
		}

		upsCfg := e.upstream.Config()
		if upsCfg.Evm == nil {
			upsCfg.Evm = &common.EvmUpstreamConfig{}
		}

		// By default, we don't know if the node is syncing or not.
		e.syncingState = common.EvmSyncingStateUnknown

		// if we have received enough consecutive "synced" responses, we can assume it's fully synced.
		if e.synced >= FullySyncedThreshold {
			e.syncingState = common.EvmSyncingStateNotSyncing
			e.logger.Info().Bool("syncingResult", syncing).Msg("node is marked as fully synced")
		} else if e.synced >= 0 && syncing {
			e.syncingState = common.EvmSyncingStateSyncing
			// If we have received at least one response (syncing or not-syncing) we explicitly assume it's syncing.
			e.logger.Debug().Bool("syncingResult", syncing).Msgf("node is marked as still syncing %d out of %d confirmations done so far", e.synced, FullySyncedThreshold)
		}
	}()

	wg.Wait()

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

func (e *EvmStatePoller) setLatestBlockNumber(blockNumber int64) {
	e.mu.Lock()
	e.latestBlockNumber = blockNumber
	e.mu.Unlock()
	e.tracker.SetLatestBlockNumber(e.upstream.Config().Id, e.upstream.NetworkId(), blockNumber)
}

func (e *EvmStatePoller) SyncingState() common.EvmSyncingState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.syncingState
}

func (e *EvmStatePoller) SetSyncingState(state common.EvmSyncingState) {
	e.mu.Lock()
	e.syncingState = state
	e.mu.Unlock()
}

func (e *EvmStatePoller) LatestBlock() int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return int64(e.latestBlockNumber)
}

func (e *EvmStatePoller) setFinalizedBlockNumber(blockNumber int64) {
	e.mu.Lock()
	e.finalizedBlockNumber = blockNumber
	e.mu.Unlock()
	e.tracker.SetFinalizedBlockNumber(e.upstream.Config().Id, e.upstream.NetworkId(), blockNumber)
}

func (e *EvmStatePoller) shouldSkipFinalizedCheck() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.skipFinalizedCheck
}

func (e *EvmStatePoller) setSkipFinalizedCheck(skip bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.skipFinalizedCheck = skip
}

func (e *EvmStatePoller) FinalizedBlock() int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.finalizedBlockNumber
}

func (e *EvmStatePoller) IsBlockFinalized(blockNumber int64) (bool, error) {
	finalizedBlock := e.finalizedBlockNumber
	latestBlock := e.latestBlockNumber
	if latestBlock == 0 && finalizedBlock == 0 {
		e.logger.Debug().
			Int64("finalizedBlock", finalizedBlock).
			Int64("latestBlock", latestBlock).
			Int64("blockNumber", blockNumber).
			Msgf("finalized/latest blocks are not available yet when checking block finality")
		return false, common.NewErrFinalizedBlockUnavailable(blockNumber)
	}

	e.logger.Debug().
		Int64("finalizedBlock", finalizedBlock).
		Int64("latestBlock", latestBlock).
		Int64("blockNumber", blockNumber).
		Msgf("calculating block finality")

	if finalizedBlock > 0 {
		return blockNumber <= finalizedBlock, nil
	}

	if latestBlock == 0 {
		return false, nil
	}

	var fb int64
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.cfg != nil && e.cfg.FallbackFinalityDepth > 0 {
		if latestBlock > e.cfg.FallbackFinalityDepth {
			fb = latestBlock - e.cfg.FallbackFinalityDepth
		} else {
			fb = 0
		}
	} else {
		if latestBlock > common.DefaultEvmFinalityDepth {
			fb = latestBlock - common.DefaultEvmFinalityDepth
		} else {
			fb = 0
		}
	}

	e.logger.Debug().
		Int64("inferredFinalizedBlock", fb).
		Int64("latestBlock", latestBlock).
		Int64("blockNumber", blockNumber).
		Msgf("calculating block finality using inferred finalized block")

	return blockNumber <= fb, nil
}

func (e *EvmStatePoller) SuggestFinalizedBlock(blockNumber int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if blockNumber > e.finalizedBlockNumber {
		e.finalizedBlockNumber = blockNumber
	}
}

func (e *EvmStatePoller) SuggestLatestBlock(blockNumber int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if blockNumber > e.latestBlockNumber {
		e.latestBlockNumber = blockNumber
	}
}

func (e *EvmStatePoller) IsObjectNull() bool {
	return e == nil || e.upstream == nil
}

func (e *EvmStatePoller) fetchLatestBlockNumber(ctx context.Context) (int64, error) {
	return e.fetchBlock(ctx, "latest")
}

func (e *EvmStatePoller) fetchFinalizedBlockNumber(ctx context.Context) (int64, error) {
	return e.fetchBlock(ctx, "finalized")
}

func (e *EvmStatePoller) fetchBlock(ctx context.Context, blockTag string) (int64, error) {
	pr := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByNumber","params":["%s",false]}`, util.RandomID(), blockTag),
	))

	resp, err := e.upstream.Forward(ctx, pr, true)
	if err != nil {
		return 0, err
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return 0, err
	}

	if jrr == nil || jrr.Error != nil {
		return 0, jrr.Error
	}

	numberStr, err := jrr.PeekStringByPath("number")
	if err != nil {
		return 0, &common.BaseError{
			Code:    "ErrEvmStatePoller",
			Message: "cannot get block number from block data",
			Details: map[string]interface{}{
				"blockTag": blockTag,
				"result":   jrr.Result,
			},
		}
	}

	return common.HexToInt64(numberStr)
}

func (e *EvmStatePoller) fetchSyncingState(ctx context.Context) (bool, error) {
	pr := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_syncing","params":[]}`, util.RandomID())))
	// pr.SetNetwork(e.network)

	resp, err := e.upstream.Forward(ctx, pr, true)
	if err != nil {
		if common.HasErrorCode(err,
			common.ErrCodeEndpointClientSideException,
			common.ErrCodeUpstreamRequestSkipped,
			common.ErrCodeUpstreamMethodIgnored,
			common.ErrCodeEndpointUnsupported,
		) {
			e.skipSyncingCheck = true
		}
		return false, err
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return false, err
	}

	if jrr == nil || jrr.Error != nil {
		return false, jrr.Error
	}

	var syncing interface{}
	err = common.SonicCfg.Unmarshal(jrr.Result, &syncing)
	if err != nil {
		return false, &common.BaseError{
			Code:    "ErrEvmStatePoller",
			Message: "cannot parse syncing state result (must be boolean or object)",
			Details: map[string]interface{}{
				"result": util.Mem2Str(jrr.Result),
			},
		}
	}

	if s, ok := syncing.(bool); ok {
		return s, nil
	}

	if s, ok := syncing.(map[string]interface{}); ok {
		// For chains such as Arbitrum L2, the syncing state is returned as an object with a "msgCount" field
		// And any value for "msgCount" means the node is syncing.
		// Ref. https://docs.arbitrum.io/build-decentralized-apps/arbitrum-vs-ethereum/rpc-methods#eth_syncing
		if _, ok := s["msgCount"]; ok {
			return true, nil
		}
	}

	return false, &common.BaseError{
		Code:    "ErrEvmStatePoller",
		Message: "cannot parse syncing state result (must be boolean or object)",
		Details: map[string]interface{}{
			"result": util.Mem2Str(jrr.Result),
		},
	}
}
