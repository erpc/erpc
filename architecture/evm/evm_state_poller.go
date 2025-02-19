package evm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

const FullySyncedThreshold = 4

var _ common.EvmStatePoller = &EvmStatePoller{}

type EvmStatePoller struct {
	Enabled bool

	projectId string
	appCtx    context.Context
	logger    *zerolog.Logger
	upstream  common.Upstream
	cfg       *common.EvmNetworkConfig
	tracker   *health.Tracker

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

	// Certain upstreams do not support calling such method, therefore we must avoid sending redundant requests.
	// We will return "nil" as status for syncing which means we don't know for certain.
	skipSyncingCheck bool

	// Avoid making redundant calls based on a networks block time
	// i.e. latest/finalized block is not going to change within the debounce interval
	debounceInterval time.Duration

	// Certain networks and nodes do not support "finalized" tag,
	// therefore we must avoid sending redundant requests.
	skipFinalizedCheck   bool
	finalizedBlockShared data.CounterInt64SharedVariable

	// The reason to not use latestBlockTimestamp is to avoid thundering herd,
	// when a node is actively syncing and timestamp is always way in the past.
	skipLatestBlockCheck bool
	latestBlockShared    data.CounterInt64SharedVariable

	stateMu sync.RWMutex
}

func NewEvmStatePoller(
	projectId string,
	appCtx context.Context,
	logger *zerolog.Logger,
	up common.Upstream,
	tracker *health.Tracker,
	sharedState data.SharedStateRegistry,
) *EvmStatePoller {
	lg := logger.With().Str("upstreamId", up.Config().Id).Str("component", "evmStatePoller").Logger()

	lbs := sharedState.GetCounterInt64(fmt.Sprintf("latestBlock/%s", common.UniqueUpstreamKey(up)))
	fbs := sharedState.GetCounterInt64(fmt.Sprintf("finalizedBlock/%s", common.UniqueUpstreamKey(up)))

	e := &EvmStatePoller{
		projectId:            projectId,
		appCtx:               appCtx,
		logger:               &lg,
		upstream:             up,
		tracker:              tracker,
		latestBlockShared:    lbs,
		finalizedBlockShared: fbs,
	}

	lbs.OnValue(func(value int64) {
		e.tracker.SetLatestBlockNumber(e.upstream.Config().Id, e.upstream.NetworkId(), value)
	})
	fbs.OnValue(func(value int64) {
		e.tracker.SetFinalizedBlockNumber(e.upstream.Config().Id, e.upstream.NetworkId(), value)
	})

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
	}

	if cfg.Evm != nil {
		if cfg.Evm.StatePollerDebounce != "" {
			if d, derr := time.ParseDuration(cfg.Evm.StatePollerDebounce); derr == nil {
				e.debounceInterval = d
			}
		}
		if e.debounceInterval == 0 && cfg.Evm.ChainId > 0 {
			e.inferDebounceIntervalFromBlockTime(cfg.Evm.ChainId)
		}
	}

	e.logger.Debug().Msgf("bootstrapping evm state poller to track upstream latest/finalized blocks and syncing states")
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
				err := e.Poll(nctx)
				cancel()
				if err != nil {
					e.logger.Error().Err(err).Msgf("failed to poll evm state for upstream %s", e.upstream.Config().Id)
				}
			}
		}
	})()

	err = e.Poll(ctx)
	if err == nil {
		e.logger.Info().Msgf("bootstrapped evm state poller to track upstream latest/finalized blocks and syncing states")
	}

	return err
}

func (e *EvmStatePoller) SetNetworkConfig(cfg *common.EvmNetworkConfig) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.cfg = cfg

	if e.debounceInterval == 0 {
		if cfg.FallbackStatePollerDebounce != "" {
			if d, err := time.ParseDuration(cfg.FallbackStatePollerDebounce); err == nil {
				e.debounceInterval = d
			}
		}
	}
	if e.debounceInterval == 0 {
		if cfg.ChainId > 0 {
			e.inferDebounceIntervalFromBlockTime(cfg.ChainId)
		}
	}
}

func (e *EvmStatePoller) Poll(ctx context.Context) error {
	var wg sync.WaitGroup
	var errs []error
	ermu := &sync.Mutex{}

	// Fetch latest block number
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := e.PollLatestBlockNumber(ctx)
		if err != nil {
			e.logger.Debug().Err(err).Msg("failed to get latest block number in evm state poller")
			ermu.Lock()
			errs = append(errs, err)
			ermu.Unlock()
			return
		}
	}()

	// Fetch finalized block (if upstream supports)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := e.PollFinalizedBlockNumber(ctx)
		if err != nil {
			e.logger.Debug().Err(err).Msg("failed to get finalized block number in evm state poller")
			ermu.Lock()
			errs = append(errs, err)
			ermu.Unlock()
		}
	}()

	// Fetch "syncing" state
	wg.Add(1)
	go func() {
		defer wg.Done()
		if e.synced >= FullySyncedThreshold || e.skipSyncingCheck {
			return
		}

		syncing, err := e.fetchSyncingState(ctx)
		if err != nil {
			if !e.skipSyncingCheck {
				e.logger.Warn().Bool("syncingResult", syncing).Err(err).Msg("failed to get syncing state in evm state poller")
				ermu.Lock()
				errs = append(errs, err)
				ermu.Unlock()
			} else {
				e.logger.Info().Bool("syncingResult", syncing).Err(err).Msg("upstream does not support eth_syncing method for evm state poller, will skip")
			}
			return
		}

		e.logger.Debug().Bool("syncingResult", syncing).Msg("fetched syncing state")

		e.stateMu.Lock()
		defer e.stateMu.Unlock()
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

// PollLatestBlockNumber fetches the latest block number in a blocking manner.
// Respects the debounce interval if configured (if the last poll happened too recently, it reuses the cached value).
func (e *EvmStatePoller) PollLatestBlockNumber(ctx context.Context) (int64, error) {
	if e.shouldSkipLatestBlockCheck() {
		return 0, nil
	}
	dbi := e.debounceInterval
	if dbi == 0 {
		// We must have some debounce interval to avoid thundering herd
		dbi = 1 * time.Second
	}
	return e.latestBlockShared.TryUpdateIfStale(ctx, dbi, func() (int64, error) {
		e.logger.Trace().Msg("fetching latest block number for evm state poller")
		health.MetricUpstreamLatestBlockPolled.WithLabelValues(
			e.projectId,
			e.upstream.NetworkId(),
			e.upstream.Config().Id,
		).Inc()
		blockNum, err := e.fetchBlock(ctx, "latest")
		if err != nil {
			if common.HasErrorCode(err,
				common.ErrCodeUpstreamRequestSkipped,
				common.ErrCodeUpstreamMethodIgnored,
				common.ErrCodeEndpointUnsupported,
				common.ErrCodeEndpointMissingData,
			) || common.IsClientError(err) {
				e.setSkipLatestBlockCheck(true)
				e.logger.Info().Err(err).Msg("upstream does not support fetching latest block number in evm state poller, will skip")
				return 0, nil
			} else {
				e.logger.Warn().Err(err).Msg("failed to get latest block number in evm state poller")
				return 0, err
			}
		}
		e.logger.Debug().
			Int64("blockNumber", blockNum).
			Msg("fetched latest block from upstream")
		return blockNum, nil
	})
}

func (e *EvmStatePoller) SuggestLatestBlock(blockNumber int64) {
	// TODO after subscription epic this method should be called for every new block received from this upstream
	e.latestBlockShared.TryUpdate(e.appCtx, blockNumber)
}

func (e *EvmStatePoller) LatestBlock() int64 {
	return e.latestBlockShared.GetValue()
}

func (e *EvmStatePoller) PollFinalizedBlockNumber(ctx context.Context) (int64, error) {
	if e.shouldSkipFinalizedCheck() {
		return 0, nil
	}
	dbi := e.debounceInterval
	if dbi == 0 {
		// We must have some debounce interval to avoid thundering herd
		dbi = 1 * time.Second
	}
	return e.finalizedBlockShared.TryUpdateIfStale(ctx, dbi, func() (int64, error) {
		e.logger.Trace().Msg("fetching finalized block number for evm state poller")
		health.MetricUpstreamFinalizedBlockPolled.WithLabelValues(
			e.projectId,
			e.upstream.NetworkId(),
			e.upstream.Config().Id,
		).Inc()

		// Actually fetch from upstream
		blockNum, err := e.fetchBlock(ctx, "finalized")
		if err != nil {
			if common.HasErrorCode(err,
				common.ErrCodeUpstreamRequestSkipped,
				common.ErrCodeUpstreamMethodIgnored,
				common.ErrCodeEndpointUnsupported,
				common.ErrCodeEndpointMissingData,
			) || common.IsClientError(err) {
				e.setSkipFinalizedCheck(true)
				e.logger.Info().Err(err).Msg("upstream does not support fetching finalized block number in evm state poller, will skip")
				return 0, nil
			} else {
				e.logger.Warn().Err(err).Msg("failed to get finalized block number in evm state poller")
				return 0, err
			}
		}

		e.logger.Debug().
			Int64("blockNumber", blockNum).
			Msg("fetched finalized block")

		e.tracker.SetFinalizedBlockNumber(e.upstream.Config().Id, e.upstream.NetworkId(), blockNum)

		return blockNum, nil
	})
}

func (e *EvmStatePoller) SuggestFinalizedBlock(blockNumber int64) {
	e.finalizedBlockShared.TryUpdate(e.appCtx, blockNumber)
}

func (e *EvmStatePoller) FinalizedBlock() int64 {
	return e.finalizedBlockShared.GetValue()
}

func (e *EvmStatePoller) IsBlockFinalized(blockNumber int64) (bool, error) {
	finalizedBlock := e.finalizedBlockShared.GetValue()
	latestBlock := e.latestBlockShared.GetValue()
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
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
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

func (e *EvmStatePoller) SyncingState() common.EvmSyncingState {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.syncingState
}

func (e *EvmStatePoller) SetSyncingState(state common.EvmSyncingState) {
	e.stateMu.Lock()
	e.syncingState = state
	e.stateMu.Unlock()
}

func (e *EvmStatePoller) IsObjectNull() bool {
	return e == nil || e.upstream == nil
}

func (e *EvmStatePoller) shouldSkipLatestBlockCheck() bool {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.skipLatestBlockCheck
}

func (e *EvmStatePoller) setSkipLatestBlockCheck(skip bool) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.skipLatestBlockCheck = skip
}

func (e *EvmStatePoller) shouldSkipFinalizedCheck() bool {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()

	return e.skipFinalizedCheck
}

func (e *EvmStatePoller) setSkipFinalizedCheck(skip bool) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	e.skipFinalizedCheck = skip
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
	blockNum, err := common.HexToInt64(numberStr)
	if err != nil {
		return 0, err
	}

	return blockNum, nil
}

func (e *EvmStatePoller) fetchSyncingState(ctx context.Context) (bool, error) {
	pr := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_syncing","params":[]}`, util.RandomID())))
	// pr.SetNetwork(e.network)

	resp, err := e.upstream.Forward(ctx, pr, true)
	if err != nil {
		if common.HasErrorCode(err,
			common.ErrCodeUpstreamRequestSkipped,
			common.ErrCodeUpstreamMethodIgnored,
			common.ErrCodeEndpointUnsupported,
		) || common.IsClientError(err) {
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

func (e *EvmStatePoller) inferDebounceIntervalFromBlockTime(chainId int64) {
	if d, ok := KnownBlockTimes[chainId]; ok {
		// Anything lower than 1 second has a chance of causing a thundering herd (e.g. a high RPS for a method like getLogs).
		// If users truly want to have a smaller value they can directly set the debounce interval
		// either on network config or upstream config.
		if d < 1*time.Second {
			e.debounceInterval = 1 * time.Second
		} else {
			e.debounceInterval = d
		}
	}
}
