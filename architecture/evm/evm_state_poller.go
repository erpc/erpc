package evm

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const FullySyncedThreshold = 4

// TODO: find a clean way to pass integrity config to evm pollers for lazy-loaded
// networks (not statically configured in erpc.yaml). at the moment an "evm state poller"
// might be initiated "before" a network is physically created and configured
// (e.g. when a new network is lazy-loaded from a Repository Provider)
const DefaultToleratedBlockHeadRollback = 1024

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
	syncMu       sync.Mutex

	// Certain upstreams do not support calling such method, therefore we must avoid sending redundant requests.
	// We will return "nil" as status for syncing which means we don't know for certain.
	skipSyncingCheck bool

	// Track consecutive failures when querying eth_syncing so we can automatically
	// stop querying after a threshold (similar to latest/finalized block logic).
	syncingFailureCount   int
	syncingSuccessfulOnce bool

	// Avoid making redundant calls based on a networks block time
	// i.e. latest/finalized block is not going to change within the debounce interval
	debounceInterval time.Duration

	// Certain networks and nodes do not support "finalized" tag,
	// therefore we must avoid sending redundant requests.
	skipFinalizedCheck           bool
	finalizedBlockFailureCount   int
	finalizedBlockSuccessfulOnce bool
	finalizedBlockShared         data.CounterInt64SharedVariable

	// The reason to not use latestBlockTimestamp is to avoid thundering herd,
	// when a node is actively syncing and timestamp is always way in the past.
	skipLatestBlockCheck      bool
	latestBlockFailureCount   int
	latestBlockSuccessfulOnce bool
	latestBlockShared         data.CounterInt64SharedVariable

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
	networkId := up.NetworkId()
	lg := logger.With().Str("component", "evmStatePoller").Str("networkId", networkId).Logger()

	lbs := sharedState.GetCounterInt64(fmt.Sprintf("latestBlock/%s", common.UniqueUpstreamKey(up)), DefaultToleratedBlockHeadRollback)
	fbs := sharedState.GetCounterInt64(fmt.Sprintf("finalizedBlock/%s", common.UniqueUpstreamKey(up)), DefaultToleratedBlockHeadRollback)

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
		e.tracker.SetLatestBlockNumber(e.upstream, value)
	})
	fbs.OnValue(func(value int64) {
		e.tracker.SetFinalizedBlockNumber(e.upstream, value)
	})

	lbs.OnLargeRollback(func(currentVal, newVal int64) {
		e.tracker.RecordBlockHeadLargeRollback(e.upstream, "latest", currentVal, newVal)
	})
	fbs.OnLargeRollback(func(currentVal, newVal int64) {
		e.tracker.RecordBlockHeadLargeRollback(e.upstream, "finalized", currentVal, newVal)
	})

	return e
}

func (e *EvmStatePoller) Bootstrap(ctx context.Context) error {
	cfg := e.upstream.Config()
	interval := cfg.Evm.StatePollerInterval
	if interval == 0 {
		e.logger.Debug().Msg("skipping evm state poller for upstream as interval is 0")
		return nil
	}

	if cfg.Evm != nil {
		if cfg.Evm.StatePollerDebounce != 0 {
			e.debounceInterval = cfg.Evm.StatePollerDebounce.Duration()
		}
		if e.debounceInterval == 0 && cfg.Evm.ChainId > 0 {
			e.inferDebounceIntervalFromBlockTime(cfg.Evm.ChainId)
		}
	}

	e.logger.Debug().Msgf("bootstrapping evm state poller to track upstream latest/finalized blocks and syncing states")
	e.Enabled = true

	go (func() {
		ticker := time.NewTicker(interval.Duration())
		defer ticker.Stop()
		for {
			select {
			case <-e.appCtx.Done():
				e.logger.Debug().Msg("shutting down evm state poller due to app context interruption")
				return
			case <-ticker.C:
				timeout := 10 * time.Second
				nctx, cancel := context.WithTimeout(e.appCtx, timeout)
				err := e.Poll(nctx)
				subCtxErr := nctx.Err()
				cancel()
				if err != nil {
					if errors.Is(subCtxErr, context.Canceled) {
						e.logger.Info().Err(err).
							Msgf("shutting down evm state poller due to context cancellation (e.g. app exiting)")
					} else {
						e.logger.Warn().Err(err).Msgf("failed to poll evm state")
					}
				}
			}
		}
	})()

	err := e.Poll(ctx)
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
		if cfg.FallbackStatePollerDebounce != 0 {
			e.debounceInterval = cfg.FallbackStatePollerDebounce.Duration()
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
		e.syncMu.Lock()
		defer e.syncMu.Unlock()

		e.stateMu.RLock()
		skip := e.skipSyncingCheck
		e.stateMu.RUnlock()
		if e.synced >= FullySyncedThreshold || skip {
			return
		}

		syncing, err := e.fetchSyncingState(ctx)
		if err != nil {
			// Handle consecutive failures to determine if we should stop querying eth_syncing
			e.stateMu.Lock()
			// Only apply the failure threshold logic if we have never had a successful response.
			if !e.syncingSuccessfulOnce {
				e.syncingFailureCount++
				if e.syncingFailureCount >= 10 {
					e.skipSyncingCheck = true
					e.syncingState = common.EvmSyncingStateUnknown
					e.logger.Warn().Err(err).
						Msgf("upstream does not support fetching syncing state in evm state poller after %d consecutive failures, will give up", e.syncingFailureCount)
				} else {
					e.logger.Debug().Err(err).
						Msgf("upstream does not seem to support fetching syncing state in evm state poller after %d consecutive failures, will retry again", e.syncingFailureCount)
				}
			}
			e.stateMu.Unlock()

			if !skip {
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

		// Mark as successful and reset failure counter
		e.syncingSuccessfulOnce = true
		e.syncingFailureCount = 0
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
		e.logger.Trace().Msg("skipping latest block number poll as it is not supported by the upstream")
		return 0, nil
	}
	dbi := e.debounceInterval
	if dbi == 0 {
		// We must have some debounce interval to avoid thundering herd
		dbi = 1 * time.Second
	}
	e.logger.Trace().Int64("debounceMs", dbi.Milliseconds()).Msg("attempt to poll latest block number")
	ctx, span := common.StartDetailSpan(ctx, "EvmStatePoller.PollLatestBlockNumber",
		trace.WithAttributes(
			attribute.String("upstream.id", e.upstream.Id()),
			attribute.String("network.id", e.upstream.NetworkId()),
		),
	)
	defer span.End()
	return e.latestBlockShared.TryUpdateIfStale(ctx, dbi, func(ctx context.Context) (int64, error) {
		e.logger.Trace().Str("ptr", fmt.Sprintf("%p", e)).Str("stack", string(debug.Stack())).Msg("fetching latest block number for evm state poller")
		telemetry.MetricUpstreamLatestBlockPolled.WithLabelValues(
			e.projectId,
			e.upstream.VendorName(),
			e.upstream.NetworkId(),
			e.upstream.Id(),
		).Inc()
		blockNum, err := e.fetchBlock(ctx, "latest")
		if err != nil || blockNum == 0 {
			if err == nil ||
				common.HasErrorCode(err,
					common.ErrCodeUpstreamRequestSkipped,
					common.ErrCodeUpstreamMethodIgnored,
					common.ErrCodeEndpointUnsupported,
					common.ErrCodeEndpointMissingData,
				) ||
				common.IsClientError(err) {
				e.stateMu.Lock()
				// Only skip after multiple consecutive failures if we've never had a success
				if !e.latestBlockSuccessfulOnce {
					e.latestBlockFailureCount++
					if e.latestBlockFailureCount >= 10 {
						e.skipLatestBlockCheck = true
						e.logger.Warn().Err(err).Msgf("upstream does not support fetching latest block number in evm state poller after %d consecutive failures, will give up", e.latestBlockFailureCount)
					} else {
						e.logger.Debug().Err(err).Msgf("upstream does not seem to support fetching latest block number in evm state poller after %d consecutive failures, will retry again", e.latestBlockFailureCount)
					}
				}
				e.stateMu.Unlock()
				return 0, nil
			} else {
				e.logger.Warn().Err(err).Msg("failed to get latest block number in evm state poller")
				return 0, err
			}
		}

		// Mark as successful and reset failure counter
		e.stateMu.Lock()
		e.latestBlockSuccessfulOnce = true
		e.latestBlockFailureCount = 0
		e.stateMu.Unlock()

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
	ctx, span := common.StartDetailSpan(ctx, "EvmStatePoller.PollFinalizedBlockNumber",
		trace.WithAttributes(
			attribute.String("upstream.id", e.upstream.Id()),
			attribute.String("network.id", e.upstream.NetworkId()),
		),
	)
	defer span.End()
	dbi := e.debounceInterval
	if dbi == 0 {
		// We must have some debounce interval to avoid thundering herd
		dbi = 1 * time.Second
	}
	return e.finalizedBlockShared.TryUpdateIfStale(ctx, dbi, func(ctx context.Context) (int64, error) {
		e.logger.Trace().Msg("fetching finalized block number for evm state poller")
		telemetry.MetricUpstreamFinalizedBlockPolled.WithLabelValues(
			e.projectId,
			e.upstream.VendorName(),
			e.upstream.NetworkId(),
			e.upstream.Id(),
		).Inc()

		// Actually fetch from upstream
		blockNum, err := e.fetchBlock(ctx, "finalized")
		if err != nil || blockNum == 0 {
			if err == nil ||
				common.HasErrorCode(err,
					common.ErrCodeUpstreamRequestSkipped,
					common.ErrCodeUpstreamMethodIgnored,
					common.ErrCodeEndpointUnsupported,
					common.ErrCodeEndpointMissingData,
				) || common.IsClientError(err) {
				e.stateMu.Lock()
				// Only skip after multiple consecutive failures if we've never had a success
				if !e.finalizedBlockSuccessfulOnce {
					e.finalizedBlockFailureCount++
					if e.finalizedBlockFailureCount >= 10 {
						e.skipFinalizedCheck = true
						e.logger.Warn().Err(err).Msgf("upstream does not support fetching finalized block number in evm state poller after %d consecutive failures, will give up", e.finalizedBlockFailureCount)
					} else {
						e.logger.Debug().Err(err).Msgf("upstream does not seem to support fetching finalized block number in evm state poller after %d consecutive failures, will retry again", e.finalizedBlockFailureCount)
					}
				}
				e.stateMu.Unlock()
				return 0, nil
			} else {
				e.logger.Warn().Err(err).Msg("failed to get finalized block number in evm state poller")
				return 0, err
			}
		}

		// Mark as successful and reset failure counter
		e.stateMu.Lock()
		e.finalizedBlockSuccessfulOnce = true
		e.finalizedBlockFailureCount = 0
		e.stateMu.Unlock()

		e.logger.Debug().
			Int64("blockNumber", blockNum).
			Msg("fetched finalized block")

		e.tracker.SetFinalizedBlockNumber(e.upstream, blockNum)

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

func (e *EvmStatePoller) shouldSkipFinalizedCheck() bool {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()

	return e.skipFinalizedCheck
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

	if util.IsBytesEmptyish(jrr.Result) {
		return 0, nil
	}

	numberStr, err := jrr.PeekStringByPath(ctx, "number")
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
			e.stateMu.Lock()
			e.skipSyncingCheck = true
			e.stateMu.Unlock()
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
				"result": util.B2Str(jrr.Result),
			},
		}
	}

	if s, ok := syncing.(bool); ok {
		return s, nil
	}

	if objectSync, ok := syncing.(map[string]interface{}); ok {
		// For some chains (e.g. Arbitrum), the syncing state is returned as an object with a "msgCount" field
		// And any value for "msgCount" means the node is syncing.
		// Ref. https://docs.arbitrum.io/build-decentralized-apps/arbitrum-vs-ethereum/rpc-methods#eth_syncing
		//
		// For other EVM chains, returning an object containing "currentBlock" means the node is syncing.
		// Ref. https://ethereum.org/en/developers/docs/apis/json-rpc/#eth_syncing
		if objectSync["currentBlock"] != nil || objectSync["msgCount"] != nil {
			return true, nil
		}
		// Handle non-standard structure {Ok: bool}
		if okVal, exists := objectSync["Ok"]; exists {
			if b, ok := okVal.(bool); ok {
				// Interpret Ok=true as not syncing, Ok=false as syncing (best-effort).
				return !b, nil
			}
		}
		if okVal, exists := objectSync["ok"]; exists { // lowercase variant just in case
			if b, ok := okVal.(bool); ok {
				return !b, nil
			}
		}
	}

	return false, &common.BaseError{
		Code:    "ErrEvmStatePoller",
		Message: "cannot parse syncing state result (must be boolean or object)",
		Details: map[string]interface{}{
			"result": util.B2Str(jrr.Result),
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
