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

	projectId    string
	appCtx       context.Context
	logger       *zerolog.Logger
	upstream     common.Upstream
	cfg          *common.EvmNetworkConfig
	tracker      *health.Tracker
	networkLabel string

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

	sharedStateRegistry data.SharedStateRegistry

	stateMu sync.RWMutex

	// Track if updates are in progress to avoid goroutine pile-up
	latestUpdateInProgress    sync.Mutex
	finalizedUpdateInProgress sync.Mutex

	// Earliest per probe tracking
	earliestByProbe          map[common.EvmAvailabilityProbeType]data.CounterInt64SharedVariable
	earliestSchedulerStarted map[common.EvmAvailabilityProbeType]bool
	earliestMu               sync.RWMutex
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
		projectId:                projectId,
		appCtx:                   appCtx,
		logger:                   &lg,
		upstream:                 up,
		tracker:                  tracker,
		latestBlockShared:        lbs,
		finalizedBlockShared:     fbs,
		sharedStateRegistry:      sharedState,
		networkLabel:             "n/a",
		earliestByProbe:          make(map[common.EvmAvailabilityProbeType]data.CounterInt64SharedVariable),
		earliestSchedulerStarted: make(map[common.EvmAvailabilityProbeType]bool),
	}

	lbs.OnValue(func(value int64) {
		// Always pass 0 timestamp to avoid using stale/incorrect timestamps from remote updates
		// Only nodes that fetch blocks directly will emit timestamp metrics (via direct tracker update)
		e.tracker.SetLatestBlockNumber(e.upstream, value, 0)
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
				// Calculate timeout based on shared state config:
				// 1. Wait for distributed lock (up to lockTtl)
				// 2. Buffer for operations (fetch block, update remote)
				const operationBuffer = 15 * time.Second
				lockTtl := e.sharedStateRegistry.GetLockTtl()
				timeout := lockTtl + operationBuffer

				// Ensure minimum timeout for basic operations
				if timeout < 30*time.Second {
					timeout = 30 * time.Second
				}

				e.logger.Debug().
					Dur("lockTtl", lockTtl).
					Dur("timeout", timeout).
					Msg("calculated poll timeout based on shared state config")

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

func (e *EvmStatePoller) SetNetworkConfig(cfg *common.NetworkConfig) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if cfg == nil || cfg.Evm == nil {
		e.logger.Warn().Msg("skipping evm state poller network config as it is nil")
		return
	}

	e.cfg = cfg.Evm
	if cfg.Alias != "" {
		e.networkLabel = cfg.Alias
	} else {
		e.networkLabel = cfg.NetworkId()
	}

	if e.debounceInterval == 0 {
		if cfg.Evm.FallbackStatePollerDebounce != 0 {
			e.debounceInterval = cfg.Evm.FallbackStatePollerDebounce.Duration()
		}
	}
	if e.debounceInterval == 0 {
		if cfg.Evm.ChainId > 0 {
			e.inferDebounceIntervalFromBlockTime(cfg.Evm.ChainId)
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

	// Earliest per-probe updates (only if needed by upstream bound config)
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.pollEarliest(ctx)
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

	// Read networkLabel with lock to avoid race condition
	e.stateMu.RLock()
	networkLabel := e.networkLabel
	e.stateMu.RUnlock()

	return e.latestBlockShared.TryUpdateIfStale(ctx, dbi, func(ctx context.Context) (int64, error) {
		if e.logger.GetLevel() <= zerolog.TraceLevel {
			e.logger.Trace().Str("ptr", fmt.Sprintf("%p", e)).Str("stack", string(debug.Stack())).Msg("fetching latest block number for evm state poller")
		}
		telemetry.MetricUpstreamLatestBlockPolled.WithLabelValues(
			e.projectId,
			e.upstream.VendorName(),
			networkLabel,
			e.upstream.Id(),
		).Inc()
		blockNum, blockTimestamp, err := e.fetchBlock(ctx, "latest")
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

		// Directly update tracker with the correct timestamp for this locally-fetched block
		// This happens BEFORE the OnValue callback is triggered, ensuring only the fetching node emits the metric
		e.tracker.SetLatestBlockNumber(e.upstream, blockNum, blockTimestamp)

		e.logger.Debug().
			Int64("blockNumber", blockNum).
			Int64("blockTimestamp", blockTimestamp).
			Msg("fetched latest block from upstream")
		return blockNum, nil
	})
}

func (e *EvmStatePoller) SuggestLatestBlock(blockNumber int64) {
	// Best-effort, non-blocking update to avoid goroutine pile-up
	// If an update is already in progress, skip this one
	if !e.latestUpdateInProgress.TryLock() {
		// Another update is already in progress, skip this one
		e.logger.Trace().
			Int64("blockNumber", blockNumber).
			Msg("skipping latest block suggestion as another update is in progress")
		return
	}

	// We acquired the lock, perform the update and release when done
	go func() {
		defer e.latestUpdateInProgress.Unlock()

		// Check if this update is still relevant (not older than current value)
		currentValue := e.latestBlockShared.GetValue()
		if blockNumber <= currentValue {
			e.logger.Trace().
				Int64("blockNumber", blockNumber).
				Int64("currentValue", currentValue).
				Msg("skipping latest block update as it's not newer")
			return
		}

		// Create a timeout context to avoid blocking forever on Redis operations
		ctx, cancel := context.WithTimeout(e.appCtx, 5*time.Second)
		defer cancel()

		e.latestBlockShared.TryUpdate(ctx, blockNumber)
	}()
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

	// Read networkLabel with lock to avoid race condition
	e.stateMu.RLock()
	networkLabel := e.networkLabel
	e.stateMu.RUnlock()

	return e.finalizedBlockShared.TryUpdateIfStale(ctx, dbi, func(ctx context.Context) (int64, error) {
		e.logger.Trace().Msg("fetching finalized block number for evm state poller")
		telemetry.MetricUpstreamFinalizedBlockPolled.WithLabelValues(
			e.projectId,
			e.upstream.VendorName(),
			networkLabel,
			e.upstream.Id(),
		).Inc()

		// Actually fetch from upstream
		blockNum, _, err := e.fetchBlock(ctx, "finalized")
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
	// Best-effort, non-blocking update to avoid goroutine pile-up
	// If an update is already in progress, skip this one
	if !e.finalizedUpdateInProgress.TryLock() {
		// Another update is already in progress, skip this one
		e.logger.Trace().
			Int64("blockNumber", blockNumber).
			Msg("skipping finalized block suggestion as another update is in progress")
		return
	}

	// We acquired the lock, perform the update and release when done
	go func() {
		defer e.finalizedUpdateInProgress.Unlock()

		// Check if this update is still relevant (not older than current value)
		currentValue := e.finalizedBlockShared.GetValue()
		if blockNumber <= currentValue {
			e.logger.Trace().
				Int64("blockNumber", blockNumber).
				Int64("currentValue", currentValue).
				Msg("skipping finalized block update as it's not newer")
			return
		}

		// Create a timeout context to avoid blocking forever on Redis operations
		ctx, cancel := context.WithTimeout(e.appCtx, 5*time.Second)
		defer cancel()

		e.finalizedBlockShared.TryUpdate(ctx, blockNumber)
	}()
}

func (e *EvmStatePoller) FinalizedBlock() int64 {
	return e.finalizedBlockShared.GetValue()
}

func (e *EvmStatePoller) PollEarliestBlockNumber(ctx context.Context, probe common.EvmAvailabilityProbeType) (int64, error) {
	// Initialize storage for probe
	e.earliestMu.Lock()
	if _, ok := e.earliestByProbe[probe]; !ok {
		key := fmt.Sprintf("earliestBlock/%s/%s", common.UniqueUpstreamKey(e.upstream), string(probe))
		e.earliestByProbe[probe] = e.sharedStateRegistry.GetCounterInt64(key, 0)
	}
	v := e.earliestByProbe[probe]
	e.earliestMu.Unlock()
	latest := e.latestBlockShared.GetValue()
	if latest == 0 {
		var err error
		latest, err = e.PollLatestBlockNumber(ctx)
		if err != nil || latest == 0 {
			return 0, err
		}
	}
	val, err := e.binarySearchEarliest(ctx, probe, 0, latest)
	if err != nil {
		return 0, err
	}
	cctx, cancel := context.WithTimeout(e.appCtx, 5*time.Second)
	defer cancel()
	v.TryUpdate(cctx, val)
	e.logger.Debug().
		Int64("earliestBlock", val).
		Int64("latestBlock", latest).
		Str("probe", string(probe)).
		Str("upstreamId", e.upstream.Config().Id).
		Msg("binary search completed for earliest block availability bound")
	return val, nil
}

func (e *EvmStatePoller) EarliestBlock(probe common.EvmAvailabilityProbeType) int64 {
	e.earliestMu.RLock()
	v, ok := e.earliestByProbe[probe]
	e.earliestMu.RUnlock()
	if ok {
		return v.GetValue()
	}
	return 0
}

// pollEarliest performs earliest per-probe initialization and periodic updates
// based on upstream's block availability configuration.
func (e *EvmStatePoller) pollEarliest(ctx context.Context) {
	e.stateMu.RLock()
	upCfg := e.upstream.Config()
	e.stateMu.RUnlock()
	if upCfg == nil || upCfg.Evm == nil || upCfg.Evm.BlockAvailability == nil {
		return
	}

	// Determine probes that require earliest updates and minimal non-zero updateRate per probe
	type probeSchedule struct {
		rate   time.Duration
		needed bool
	}
	schedules := map[common.EvmAvailabilityProbeType]*probeSchedule{}
	consider := func(b *common.EvmAvailabilityBoundConfig) {
		// Only schedule earliest work when the bound actually uses earliest.
		if b == nil || b.EarliestBlockPlus == nil {
			return
		}
		probe := b.Probe
		if probe == "" {
			probe = common.EvmProbeBlockHeader
		}
		ps, ok := schedules[probe]
		if !ok {
			ps = &probeSchedule{rate: 0, needed: true}
			schedules[probe] = ps
		}
		if b.UpdateRate > 0 {
			r := b.UpdateRate.Duration()
			if ps.rate == 0 || r < ps.rate {
				ps.rate = r
			}
		}
	}
	consider(upCfg.Evm.BlockAvailability.Lower)
	consider(upCfg.Evm.BlockAvailability.Upper)

	if len(schedules) == 0 {
		return
	}

	// Ensure earliest initialized if needed
	// If latest block is not available, try to fetch it first
	latest := e.latestBlockShared.GetValue()
	if latest == 0 {
		e.logger.Debug().Msg("latest block not available, attempting fresh poll before earliest detection")
		var err error
		latest, err = e.PollLatestBlockNumber(ctx)
		if err != nil || latest == 0 {
			// Still unavailable - log and skip for this cycle
			// The next poll cycle will retry, and the scheduler will also keep retrying
			if err != nil {
				e.logger.Debug().Err(err).Msg("failed to fetch latest block for earliest bound detection, will retry on next poll")
			} else {
				e.logger.Debug().Msg("latest block still not available after fresh poll, skipping earliest detection for this cycle")
			}
			return
		}
		e.logger.Debug().Int64("latestBlock", latest).Msg("successfully fetched latest block, proceeding with earliest detection")
	}

	for probe, ps := range schedules {
		// Initialize shared var for this probe if missing
		e.earliestMu.Lock()
		if _, ok := e.earliestByProbe[probe]; !ok {
			key := fmt.Sprintf("earliestBlock/%s/%s", common.UniqueUpstreamKey(e.upstream), string(probe))
			e.earliestByProbe[probe] = e.sharedStateRegistry.GetCounterInt64(key, 0)
		}
		e.earliestMu.Unlock()
		// If not initialized yet, run initial search once using PollEarliestBlockNumber
		if e.earliestByProbe[probe].GetValue() == 0 {
			earliest, err := e.PollEarliestBlockNumber(ctx, probe)
			if err != nil {
				// fail-open: skip if cannot initialize now
				e.logger.Warn().
					Err(err).
					Str("probe", string(probe)).
					Str("upstreamId", e.upstream.Config().Id).
					Msg("failed to initialize earliest block bound for probe; availability checks will be less accurate")
			} else if earliest >= 0 {
				e.logger.Info().
					Int64("earliestBlock", earliest).
					Str("probe", string(probe)).
					Str("upstreamId", e.upstream.Config().Id).
					Msg("initialized earliest block availability bound")
			}
		}
		// Start scheduler per probe if updateRate>0 and not started yet
		if ps.rate > 0 {
			e.earliestMu.Lock()
			started := e.earliestSchedulerStarted[probe]
			if !started {
				e.earliestSchedulerStarted[probe] = true
			}
			e.earliestMu.Unlock()
			if !started {
				e.logger.Info().
					Str("probe", string(probe)).
					Str("updateRate", ps.rate.String()).
					Str("upstreamId", e.upstream.Config().Id).
					Msg("started periodic scheduler for earliest block availability bound")
				go e.runEarliestScheduler(probe, ps.rate)
			}
		}
	}
}

// runEarliestScheduler periodically attempts to advance earliest for a probe at the given rate.
// It is liberal: if latest or probe checks are unavailable, it skips and retries on next tick.
func (e *EvmStatePoller) runEarliestScheduler(probe common.EvmAvailabilityProbeType, rate time.Duration) {
	ticker := time.NewTicker(rate)
	defer ticker.Stop()
	for {
		select {
		case <-e.appCtx.Done():
			return
		case <-ticker.C:
			latest := e.latestBlockShared.GetValue()
			if latest == 0 {
				// Latest block not available - try to fetch it before giving up
				// This makes the scheduler more robust to transient failures
				var err error
				latest, err = e.PollLatestBlockNumber(e.appCtx)
				if err != nil || latest == 0 {
					// Still unavailable - skip this cycle, will retry on next tick
					continue
				}
			}
			e.earliestMu.RLock()
			varCounter, ok := e.earliestByProbe[probe]
			e.earliestMu.RUnlock()
			if !ok {
				continue
			}
			curr := varCounter.GetValue()
			if curr <= 0 {
				// If still uninitialized (race), try initializing quickly using binary search
				if val, err := e.binarySearchEarliest(e.appCtx, probe, 0, latest); err == nil && val >= 0 {
					cctx, cancel := context.WithTimeout(e.appCtx, 5*time.Second)
					e.earliestByProbe[probe].TryUpdate(cctx, val)
					cancel()
				}
				continue
			}
			// Quick current-bound check; if still valid, skip heavy work
			ok, unsupported, err := e.checkProbe(e.appCtx, probe, curr)
			if err != nil {
				e.logger.Debug().
					Err(err).
					Int64("block", curr).
					Str("probe", string(probe)).
					Str("upstreamId", e.upstream.Config().Id).
					Msg("probe check failed during earliest bound update cycle")
				continue
			}
			if unsupported {
				e.logger.Warn().
					Int64("block", curr).
					Str("probe", string(probe)).
					Str("upstreamId", e.upstream.Config().Id).
					Msg("probe is unsupported on upstream; earliest bound detection will be less accurate")
				continue
			}
			if ok {
				e.logger.Debug().
					Int64("block", curr).
					Str("probe", string(probe)).
					Str("upstreamId", e.upstream.Config().Id).
					Msg("earliest bound still valid, skipping update cycle")
				continue
			}
			// Attempt incremental advance
			e.logger.Debug().
				Int64("currentEarliest", curr).
				Int64("latest", latest).
				Str("probe", string(probe)).
				Str("upstreamId", e.upstream.Config().Id).
				Msg("current earliest bound no longer valid, attempting incremental advance")
			if newVal, changed, err := e.incrementalAdvanceEarliest(e.appCtx, probe, curr, latest); err == nil && changed {
				cctx, cancel := context.WithTimeout(e.appCtx, 5*time.Second)
				e.earliestMu.RLock()
				varCounter2, ok2 := e.earliestByProbe[probe]
				e.earliestMu.RUnlock()
				if ok2 {
					varCounter2.TryUpdate(cctx, newVal)
					e.logger.Info().
						Int64("oldEarliest", curr).
						Int64("newEarliest", newVal).
						Str("probe", string(probe)).
						Str("upstreamId", e.upstream.Config().Id).
						Msg("earliest block availability bound advanced (pruning detected)")
				}
				cancel()
			} else if err != nil {
				e.logger.Debug().
					Err(err).
					Int64("currentEarliest", curr).
					Str("probe", string(probe)).
					Str("upstreamId", e.upstream.Config().Id).
					Msg("failed to advance earliest bound during update cycle")
			}
		}
	}
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

func (e *EvmStatePoller) fetchBlock(ctx context.Context, blockTag string) (int64, int64, error) {
	pr := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByNumber","params":["%s",false]}`, util.RandomID(), blockTag),
	))
	resp, err := e.upstream.Forward(ctx, pr, true)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return 0, 0, err
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return 0, 0, err
	}
	if jrr == nil || jrr.Error != nil {
		return 0, 0, jrr.Error
	}

	if jrr.IsResultEmptyish(ctx) {
		return 0, 0, nil
	}

	numberStr, err := jrr.PeekStringByPath(ctx, "number")
	if err != nil {
		return 0, 0, &common.BaseError{
			Code:    "ErrEvmStatePoller",
			Message: "cannot get block number from block data",
			Details: map[string]interface{}{
				"blockTag": blockTag,
				"result":   jrr.GetResultString(),
			},
		}
	}
	// Force-copy the small string to avoid any potential reference to backing buffers
	numberStr = string(append([]byte(nil), numberStr...))
	blockNum, err := common.HexToInt64(numberStr)
	if err != nil {
		return 0, 0, err
	}

	// Extract timestamp using shared function
	blockTimestamp, err := ExtractBlockTimestampFromResponse(ctx, resp)
	if err != nil {
		// If timestamp parsing fails, log debug and continue without timestamp
		// This gracefully handles invalid timestamps without breaking the flow
		e.logger.Debug().
			Err(err).
			Msg("failed to parse block timestamp, continuing without it")
		blockTimestamp = 0
	}

	return blockNum, blockTimestamp, nil
}

func (e *EvmStatePoller) fetchSyncingState(ctx context.Context) (bool, error) {
	pr := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_syncing","params":[]}`, util.RandomID())))

	resp, err := e.upstream.Forward(ctx, pr, true)
	if resp != nil {
		defer resp.Release()
	}
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

	result := jrr.GetResultBytes()

	var syncing interface{}
	err = common.SonicCfg.Unmarshal(result, &syncing)
	if err != nil {
		return false, &common.BaseError{
			Code:    "ErrEvmStatePoller",
			Message: "cannot parse syncing state result (must be boolean or object)",
			Details: map[string]interface{}{
				"result": result,
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
			"result": result,
		},
	}
}

// --- Probes & Earliest search helpers ---

// checkProbe determines whether data for a given block is available for the given probe type.
// Returns (available, unsupported, err).
func (e *EvmStatePoller) checkProbe(ctx context.Context, probe common.EvmAvailabilityProbeType, block int64) (bool, bool, error) {
	eff := probe
	if eff == "" {
		eff = common.EvmProbeBlockHeader
	}
	e.logger.Debug().
		Int64("block", block).
		Str("probe", string(eff)).
		Str("upstreamId", e.upstream.Config().Id).
		Msg("checking probe for block availability")
	var ok bool
	var unsupported bool
	var err error
	switch eff {
	case common.EvmProbeBlockHeader:
		ok, err = e.checkBlockHeaderProbe(ctx, block)
		unsupported = false
	case common.EvmProbeEventLogs:
		ok, unsupported, err = e.checkEventLogsProbe(ctx, block)
	case common.EvmProbeCallState:
		ok, unsupported, err = e.checkCallStateProbe(ctx, block)
	case common.EvmProbeTraceData:
		ok, unsupported, err = e.checkTraceDataProbe(ctx, block)
	default:
		e.logger.Warn().
			Str("probe", string(eff)).
			Str("upstreamId", e.upstream.Config().Id).
			Msg("unknown probe type; availability detection will be less accurate")
		return false, true, nil
	}
	if unsupported {
		e.logger.Debug().
			Int64("block", block).
			Str("probe", string(eff)).
			Str("upstreamId", e.upstream.Config().Id).
			Msg("probe unsupported by upstream")
	} else {
		e.logger.Debug().
			Int64("block", block).
			Str("probe", string(eff)).
			Bool("available", ok).
			Str("upstreamId", e.upstream.Config().Id).
			Msg("probe check completed")
	}
	return ok, unsupported, err
}

func (e *EvmStatePoller) checkBlockHeaderProbe(ctx context.Context, block int64) (bool, error) {
	if block < 0 {
		return false, nil
	}
	// Build request eth_getBlockByNumber with hex number
	hex := fmt.Sprintf("0x%x", uint64(block))
	pr := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByNumber","params":["%s",false]}`,
			util.RandomID(), hex,
		),
	))
	resp, err := e.upstream.Forward(ctx, pr, true)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		// Treat transport/server errors as not available for probe decisions
		return false, nil
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil || jrr == nil || jrr.Error != nil {
		return false, nil
	}
	if jrr.IsResultEmptyish(ctx) {
		return false, nil
	}
	return true, nil
}

// fetchBlockHashByNumber returns the block hash for the given block number.
// Returns (hash, ok, err) where ok=false means the block data is not available.
func (e *EvmStatePoller) fetchBlockHashByNumber(ctx context.Context, block int64) (string, bool, error) {
	if block < 0 {
		return "", false, nil
	}
	hex := fmt.Sprintf("0x%x", uint64(block))
	pr := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByNumber","params":["%s",false]}`,
			util.RandomID(), hex,
		),
	))
	resp, err := e.upstream.Forward(ctx, pr, true)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		// Treat transport/server errors as not available for probe decisions
		return "", false, nil
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil || jrr == nil || jrr.Error != nil {
		return "", false, nil
	}
	if jrr.IsResultEmptyish(ctx) {
		return "", false, nil
	}
	hash, err := jrr.PeekStringByPath(ctx, "hash")
	if err != nil || hash == "" {
		return "", false, nil
	}
	return hash, true, nil
}

// checkEventLogsProbe verifies whether the upstream can return at least one log for the given block.
// An empty logs array is NOT considered available.
func (e *EvmStatePoller) checkEventLogsProbe(ctx context.Context, block int64) (bool, bool, error) {
	if block < 0 {
		return false, false, nil
	}
	hash, ok, _ := e.fetchBlockHashByNumber(ctx, block)
	if !ok {
		return false, false, nil
	}
	pr := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getLogs","params":[{"blockHash":"%s"}]}`,
			util.RandomID(), hash,
		),
	))
	resp, err := e.upstream.Forward(ctx, pr, true)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		if common.HasErrorCode(err,
			common.ErrCodeUpstreamRequestSkipped,
			common.ErrCodeUpstreamMethodIgnored,
			common.ErrCodeEndpointUnsupported,
		) {
			return false, true, nil
		}
		// Treat other errors as not available
		return false, false, nil
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil || jrr == nil {
		return false, false, nil
	}
	if jrr.Error != nil {
		// Method not found -> unsupported; otherwise treat as not-available
		if jrr.Error.Code == -32601 {
			return false, true, nil
		}
		return false, false, nil
	}
	// Expect an array of logs; require at least one entry
	var logs []interface{}
	if er := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &logs); er == nil {
		if len(logs) >= 1 {
			return true, false, nil
		}
		return false, false, nil
	}
	return false, false, nil
}

// checkCallStateProbe verifies whether historical state (e.g., balance) is available at the given block.
// Any non-null result (including "0x0") is considered available.
func (e *EvmStatePoller) checkCallStateProbe(ctx context.Context, block int64) (bool, bool, error) {
	if block < 0 {
		return false, false, nil
	}
	hex := fmt.Sprintf("0x%x", uint64(block))
	pr := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000","%s"]}`,
			util.RandomID(), hex,
		),
	))
	resp, err := e.upstream.Forward(ctx, pr, true)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		if common.HasErrorCode(err,
			common.ErrCodeUpstreamRequestSkipped,
			common.ErrCodeUpstreamMethodIgnored,
			common.ErrCodeEndpointUnsupported,
		) {
			return false, true, nil
		}
		return false, false, nil
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil || jrr == nil {
		return false, false, nil
	}
	if jrr.Error != nil {
		return false, false, nil
	}
	// Consider available if result is present and not null
	res := jrr.GetResultBytes()
	if len(res) == 0 || string(res) == "null" {
		return false, false, nil
	}
	return true, false, nil
}

// checkTraceDataProbe verifies tracing availability by attempting multiple engine-specific methods.
// It tries in order: trace_block, debug_traceBlockByHash, trace_replayBlockTransactions.
// Availability is true if any method returns a non-empty result.
func (e *EvmStatePoller) checkTraceDataProbe(ctx context.Context, block int64) (bool, bool, error) {
	if block < 0 {
		return false, false, nil
	}
	hash, ok, _ := e.fetchBlockHashByNumber(ctx, block)
	if !ok {
		return false, false, nil
	}

	attempts := 0
	unsupported := 0

	tryCall := func(methodPayload string) (bool, bool) {
		attempts++
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()

		pr := common.NewNormalizedRequest([]byte(methodPayload))
		resp, err := e.upstream.Forward(cctx, pr, true)
		if resp != nil {
			defer resp.Release()
		}
		if err != nil {
			if common.HasErrorCode(err,
				common.ErrCodeUpstreamRequestSkipped,
				common.ErrCodeUpstreamMethodIgnored,
				common.ErrCodeEndpointUnsupported,
			) {
				unsupported++
			}
			return false, false
		}
		jrr, jerr := resp.JsonRpcResponse()
		if jerr != nil || jrr == nil {
			return false, false
		}
		if jrr.Error != nil {
			if jrr.Error.Code == -32601 {
				unsupported++
			}
			// Otherwise, treat as not available for this attempt
			return false, false
		}
		if jrr.IsResultEmptyish(ctx) {
			return false, false
		}
		return true, false
	}

	// 1) trace_block(hash)
	if ok, _ := tryCall(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"trace_block","params":["%s"]}`,
		util.RandomID(), hash)); ok {
		return true, false, nil
	}

	// 2) debug_traceBlockByHash(hash, {})
	if ok, _ := tryCall(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"debug_traceBlockByHash","params":["%s",{}]}`,
		util.RandomID(), hash)); ok {
		return true, false, nil
	}

	// 3) trace_replayBlockTransactions(hash, ["trace"]) (Parity-style)
	if ok, _ := tryCall(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"trace_replayBlockTransactions","params":["%s",["trace"]]}`,
		util.RandomID(), hash)); ok {
		return true, false, nil
	}

	if attempts > 0 && unsupported == attempts {
		return false, true, nil
	}
	return false, false, nil
}

// binarySearchEarliest finds the first available block for the given probe in [low, high].
func (e *EvmStatePoller) binarySearchEarliest(ctx context.Context, probe common.EvmAvailabilityProbeType, low, high int64) (int64, error) {
	eff := probe
	if eff == "" {
		eff = common.EvmProbeBlockHeader
	}
	if high < 0 {
		return 0, nil
	}
	if low < 0 {
		low = 0
	}
	// Fast-path: block 0
	if ok, _, err := e.checkProbe(ctx, eff, 0); err == nil && ok {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	// Fast-path: block 1
	if ok, _, err := e.checkProbe(ctx, eff, 1); err == nil && ok {
		return 1, nil
	} else if err != nil {
		return 0, err
	}

	l, r := low, high
	for l < r {
		mid := l + (r-l)/2
		ok, _, err := e.checkProbe(ctx, eff, mid)
		if err != nil {
			return 0, err
		}
		e.logger.Debug().
			Int64("mid", mid).
			Int64("low", l).
			Int64("high", r).
			Bool("available", ok).
			Str("probe", string(eff)).
			Str("upstreamId", e.upstream.Config().Id).
			Msg("binary search iteration")
		if ok {
			r = mid
		} else {
			l = mid + 1
		}
	}
	return l, nil
}

// incrementalAdvanceEarliest advances earliest if pruning moved it forward; returns (newEarliest, changed, err).
func (e *EvmStatePoller) incrementalAdvanceEarliest(ctx context.Context, probe common.EvmAvailabilityProbeType, start, latest int64) (int64, bool, error) {
	eff := probe
	if eff == "" {
		eff = common.EvmProbeBlockHeader
	}
	if start < 0 {
		start = 0
	}
	if latest <= 0 || start > latest {
		return start, false, nil
	}
	// Quick current-bound check: if still valid, skip
	ok, _, err := e.checkProbe(ctx, eff, start)
	if err != nil {
		return start, false, err
	}
	if ok {
		return start, false, nil
	}
	// Exponential forward search to find first existing
	step := int64(1)
	probeBn := start + step
	for probeBn <= latest {
		ok, _, err = e.checkProbe(ctx, eff, probeBn)
		if err != nil {
			return start, false, err
		}
		if ok {
			break
		}
		step *= 2
		probeBn = start + step
	}
	// Bound the search interval
	low := start
	high := probeBn
	if high > latest {
		high = latest
	}
	// Binary search between low..high to find new earliest
	e.logger.Debug().
		Int64("searchLow", low).
		Int64("searchHigh", high).
		Int64("startBlock", start).
		Str("probe", string(eff)).
		Str("upstreamId", e.upstream.Config().Id).
		Msg("starting binary search to find new earliest block bound")
	val, err := e.binarySearchEarliest(ctx, eff, low, high)
	if err != nil {
		return start, false, err
	}
	if val != start {
		e.logger.Debug().
			Int64("foundEarliest", val).
			Int64("previousEarliest", start).
			Str("probe", string(eff)).
			Str("upstreamId", e.upstream.Config().Id).
			Msg("binary search found new earliest block")
		return val, true, nil
	}
	return start, false, nil
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
