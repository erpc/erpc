package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/indexer"
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
// Alias of the shared default so the poller counters and the health tracker
// apply the same rollback tolerance to block heads.
const DefaultToleratedBlockHeadRollback = common.DefaultToleratedBlockHeadRollback

const (
	// chainIdVerifyChainProgress is how much CHAIN PROGRESS a head move must
	// represent before it is treated as major and re-verified against a live
	// eth_chainId (see majorHeadMoveThreshold). One minute is comfortably more
	// chain than a healthy poller can miss between samples — the default state
	// poller interval is 30s — while staying far below the height difference any
	// two unrelated chains exhibit.
	chainIdVerifyChainProgress = 60 * time.Second

	// chainIdVerifyMinBlocks keeps the derived threshold off zero on a chain
	// whose measured block time is longer than the window itself.
	chainIdVerifyMinBlocks = 2

	// chainIdCordonMatches is how many matching eth_chainId checks in a row,
	// ChainIdCordonRecheck apart, lift a chain-identity cordon: one match
	// can be a load balancer's right backend among wrong ones.
	chainIdCordonMatches = 2
)

// pollSourceMaxCadence caps a poll source's cadence.
var pollSourceMaxCadence = 12 * time.Second

// Poll source backoff and expiries; exported so end-to-end tests can
// shorten them.
var (
	PollSourceMaxBackoff  = util.NewDuration(60 * time.Second)
	LatestBlockSkipExpiry = util.NewDuration(10 * time.Minute)
	ChainIdCordonRecheck  = util.NewDuration(5 * time.Minute)
)

var (
	_ common.EvmStatePoller = &EvmStatePoller{}
	_ indexer.Source        = &EvmStatePoller{}
)

type EvmStatePoller struct {
	Enabled bool

	// started guards the background ticker goroutine: Bootstrap may be called
	// more than once on the same poller (upstream bootstrap tasks are retried
	// and may be re-executed against an already-registered upstream). The
	// ticker goroutine is bound to appCtx and has no other stop mechanism, so
	// spawning a duplicate would poll the upstream forever.
	started atomic.Bool

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
	// skipLatestBlockCheck expires LatestBlockSkipExpiry after skipLatestSince.
	skipLatestBlockCheck      bool
	skipLatestSince           time.Time
	latestBlockFailureCount   int
	latestBlockSuccessfulOnce bool
	latestBlockShared         data.CounterInt64SharedVariable

	// pollSrc is set once the network binds this poller as a poll source;
	// while pollHeads is on, each latest poll feeds its intake, at the poll
	// source's cadence. pollFailures counts failed polls in a row; pollWake
	// starts a poll at once.
	pollSrc      atomic.Pointer[pollSource]
	pollHeads    atomic.Bool
	pollFailures atomic.Int32
	pollWake     chan struct{}

	// chainIdRecheck is the pending eth_chainId check of a chain-identity
	// cordon, nil while none; chainIdRecheckUnwatch stops watching appCtx
	// for it; chainIdMatches counts its matching checks in a row.
	// chainIdCordonReason is the cordon's reason.
	chainIdRecheckMu      sync.Mutex
	chainIdRecheck        *time.Timer
	chainIdRecheckUnwatch func() bool
	chainIdMatches        int
	chainIdCordonReason   atomic.Value // string

	sharedStateRegistry data.SharedStateRegistry

	stateMu sync.RWMutex

	// Track if updates are in progress to avoid goroutine pile-up
	finalizedUpdateInProgress sync.Mutex

	// Serializes the off-hot-path chain-identity verification for a MAJOR
	// forward jump suggested out-of-band via SuggestLatestBlock. At most one
	// verification is in flight per poller; a concurrent major suggestion is
	// dropped and re-observed on the next suggestion (or verified poll). Small
	// keep-fresh advances never touch this.
	latestMajorVerifyInProgress sync.Mutex

	// Single in-flight flag for RefreshLatestBlock's background poll.
	latestRefreshInFlight atomic.Bool

	// Earliest per probe tracking
	earliestByProbe              map[common.EvmAvailabilityProbeType]data.CounterInt64SharedVariable
	earliestSchedulerStarted     map[common.EvmAvailabilityProbeType]bool
	earliestInitialDetectionDone map[common.EvmAvailabilityProbeType]bool // tracks if THIS instance did initial detection
	earliestMu                   sync.RWMutex
}

// sharedCounterKey builds a shared-state counter key namespaced by the counter
// value's wire-format version (data.CounterValueSchemaVersion). Namespacing by
// version keeps erpc instances running incompatible counter formats from
// reading or writing the same key — mixing the pre-0.0.63 bare-integer format
// with the current JSON CounterInt64State on one key surfaces as an
// "expected integer" parse error and breaks block-tip coordination. Crossing a
// version boundary cold-starts the counter once; it re-seeds within a poll
// interval, so requests are unaffected.
func sharedCounterKey(parts ...string) string {
	return data.CounterValueSchemaVersion + "/" + strings.Join(parts, "/")
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

	lbs := sharedState.GetCounterInt64(sharedCounterKey("latestBlock", common.UniqueUpstreamKey(up)), DefaultToleratedBlockHeadRollback)
	fbs := sharedState.GetCounterInt64(sharedCounterKey("finalizedBlock", common.UniqueUpstreamKey(up)), DefaultToleratedBlockHeadRollback)

	e := &EvmStatePoller{
		projectId:                    projectId,
		appCtx:                       appCtx,
		logger:                       &lg,
		upstream:                     up,
		tracker:                      tracker,
		latestBlockShared:            lbs,
		finalizedBlockShared:         fbs,
		sharedStateRegistry:          sharedState,
		networkLabel:                 "n/a",
		earliestByProbe:              make(map[common.EvmAvailabilityProbeType]data.CounterInt64SharedVariable),
		earliestSchedulerStarted:     make(map[common.EvmAvailabilityProbeType]bool),
		earliestInitialDetectionDone: make(map[common.EvmAvailabilityProbeType]bool),
		pollWake:                     make(chan struct{}, 1),
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
	if tracker != nil {
		tracker.SetFetchCadence(up, e.BackedOffCadence)
	}

	return e
}

func (e *EvmStatePoller) Bootstrap(ctx context.Context) error {
	cfg := e.upstream.Config()
	if cfg.Evm != nil && cfg.Evm.StatePollerDebounce != 0 {
		// Guarded by stateMu: live poll goroutines read this via
		// resolveDebounce while Bootstrap may run again concurrently.
		e.stateMu.Lock()
		e.debounceInterval = cfg.Evm.StatePollerDebounce.Duration()
		e.stateMu.Unlock()
	}
	interval := cfg.Evm.StatePollerInterval.Duration()
	if interval == 0 {
		// Only the full state poll is off: Set still starts the loop for a
		// poll source.
		e.logger.Debug().Msg("skipping evm state poller for upstream as interval is 0")
		return nil
	}

	e.logger.Debug().Msgf("bootstrapping evm state poller to track upstream latest/finalized blocks and syncing states")

	if !e.startLoop(interval) {
		// A ticker goroutine is already running for this poller. Do not spawn
		// another one — just refresh the state once so the caller still gets
		// an up-to-date view.
		return e.Poll(ctx)
	}
	e.Enabled = true

	err := e.Poll(ctx)
	if err == nil {
		e.logger.Info().Msgf("bootstrapped evm state poller to track upstream latest/finalized blocks and syncing states")
	}

	return err
}

// startLoop starts the poller's one poll loop unless it runs already; it
// runs until appCtx ends. Bootstrap and Set may both start it, since the
// started flag guards a loop with no other stop.
func (e *EvmStatePoller) startLoop(interval time.Duration) bool {
	if !e.started.CompareAndSwap(false, true) {
		return false
	}
	go e.pollLoop(interval)
	return true
}

// pollLoop runs a full state poll every interval (none while it is 0), and
// while a poll source, a latest poll at its cadence in between.
func (e *EvmStatePoller) pollLoop(interval time.Duration) {
	lastFull := time.Now()
	for {
		var timer *time.Timer
		var fire <-chan time.Time
		if d, ok := e.nextPollDelay(interval, lastFull); ok {
			timer = time.NewTimer(d)
			fire = timer.C
		}
		select {
		case <-e.appCtx.Done():
			if timer != nil {
				timer.Stop()
			}
			e.logger.Debug().Msg("shutting down evm state poller due to app context interruption")
			return
		case <-e.pollWake:
		case <-fire:
		}
		if timer != nil {
			timer.Stop()
		}
		full := interval > 0 && time.Since(lastFull) >= interval
		if !full && e.activePollSource() == nil {
			continue
		}
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
		var err error
		if e.activePollSource() != nil {
			// The poll source's own channel (spec §6.2); a full poll's latest
			// fetch right after it is inside the debounce, so it is skipped.
			_, err = e.pollLatest(nctx, true, HeaderStreamed)
		}
		if full {
			lastFull = time.Now()
			err = errors.Join(err, e.Poll(nctx))
		}
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

		upsCfg := e.upstream.Config()
		if upsCfg.Evm != nil && upsCfg.Evm.SkipSyncingCheck != nil && *upsCfg.Evm.SkipSyncingCheck {
			e.stateMu.Lock()
			e.syncingState = common.EvmSyncingStateNotSyncing
			e.stateMu.Unlock()
			return
		}

		if e.synced >= FullySyncedThreshold || skip {
			return
		}

		syncCtx, syncCancel := context.WithTimeout(ctx, 5*time.Second)
		defer syncCancel()
		syncing, err := e.fetchSyncingState(syncCtx)
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

		upsCfg = e.upstream.Config()
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
		e.initializeEarliestBlockDetectionAndStartScheduler(ctx)
	}()

	wg.Wait()

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

// resolveDebounce returns the debounce interval for poll methods.
// The debounce prevents redundant fetches when request traffic or other
// paths (e.g. SuggestLatestBlock) already keep the block number fresh.
//
//	user config → block time × multiplier → while the block time is unknown,
//	network FallbackStatePollerDebounce, else health.ColdBlockTime × multiplier
//
// so a cold instance polls at the cold block time's cadence (spec §6.7).
func (e *EvmStatePoller) resolveDebounce(cfg *common.EvmNetworkConfig) time.Duration {
	e.stateMu.RLock()
	dbi := e.debounceInterval
	e.stateMu.RUnlock()
	if dbi != 0 {
		return dbi
	}
	blockTime := e.tracker.GetNetworkBlockTime(e.upstream.NetworkId())
	if blockTime == 0 {
		if cfg != nil && cfg.FallbackStatePollerDebounce != 0 {
			return cfg.FallbackStatePollerDebounce.Duration()
		}
		blockTime = health.ColdBlockTime
	}
	// Scale block time down by the configured multiplier (default 0.7) so the
	// debounce expires before the next block is expected. This prefers freshness
	// over saving RPC calls — EMA smooths long-term, multiplier covers the tail.
	mult := common.DefaultDynamicBlockTimeDebounceMultiplier
	if cfg != nil && cfg.DynamicBlockTimeDebounceMultiplier != nil && *cfg.DynamicBlockTimeDebounceMultiplier > 0 {
		mult = *cfg.DynamicBlockTimeDebounceMultiplier
	}
	return time.Duration(float64(blockTime) * mult)
}

// resolveFinalizedDebounce widens the debounce for finalized polls to a fraction of the
// chain's finality depth in time. Finalized moves once per finality window, so a poll at
// the latest-block cadence mostly returns the number it already has; this bounds how far
// the finalized block can trail to that fraction of the window instead.
//
// An upstream behind the network's corroborated finalized block keeps the regular
// debounce: peers already saw the next finalized block, and a vendor whose load balancer
// mixes stale and fresh backends needs every sample to catch up. Response finality uses
// the serving upstream's own finalized block, and cache hits the lowest across upstreams.
func (e *EvmStatePoller) resolveFinalizedDebounce(cfg *common.EvmNetworkConfig, debounce time.Duration) time.Duration {
	if cfg == nil || cfg.FinalizedDebounceDepthMultiplier == nil {
		return debounce
	}
	mult := *cfg.FinalizedDebounceDepthMultiplier
	networkId := e.upstream.NetworkId()
	finalized := e.finalizedBlockShared.GetValue()
	depth := e.latestBlockShared.GetValue() - finalized
	blockTime := e.tracker.GetNetworkBlockTime(networkId)
	if mult <= 0 || finalized <= 0 || depth <= 0 || blockTime <= 0 || finalized < e.tracker.GetNetworkFinalizedBlock(networkId) {
		return debounce
	}
	return max(debounce, time.Duration(float64(depth)*float64(blockTime)*mult))
}

// PollLatestBlockNumber fetches the latest block number in a blocking manner.
// Respects the debounce interval if configured (if the last poll happened too recently, it reuses the cached value).
// With an intake bound, each fetched header goes to it, as a hash vote of
// this upstream (spec §6.3: every latest poll skips cache reads), and the
// counter moves only there, after the header passed its checks. The
// give-up after latest was refused as unsupported applies to a poll source
// too: while it lasts, the reconciler polls other sources (LatestUnsupported).
func (e *EvmStatePoller) PollLatestBlockNumber(ctx context.Context) (int64, error) {
	return e.pollLatest(ctx, true, HeaderPolled)
}

// pollLatest is PollLatestBlockNumber; debounced: skip while this
// instance's last fetch is within the debounce. kind is what the fetched
// header is to the bound intake.
func (e *EvmStatePoller) pollLatest(ctx context.Context, debounced bool, kind HeaderKind) (int64, error) {
	if e.shouldSkipLatestBlockCheck() {
		e.logger.Trace().Msg("skipping latest block number poll as it is not supported by the upstream")
		return 0, nil
	}
	src, bound := e.activePollSource(), e.pollSrc.Load()
	e.stateMu.RLock()
	cfg := e.cfg
	networkLabel := e.networkLabel
	e.stateMu.RUnlock()

	var dbi time.Duration
	if debounced {
		dbi = e.latestDebounce(cfg)
	}
	ctx, span := common.StartDetailSpan(ctx, "EvmStatePoller.PollLatestBlockNumber",
		trace.WithAttributes(
			attribute.String("upstream.id", e.upstream.Id()),
			attribute.String("network.id", e.upstream.NetworkId()),
		),
	)
	defer span.End()

	return e.latestBlockShared.TryUpdateIfLocallyStale(ctx, dbi, func(ctx context.Context) (int64, error) {
		if e.logger.GetLevel() <= zerolog.TraceLevel {
			e.logger.Trace().Str("ptr", fmt.Sprintf("%p", e)).Str("stack", string(debug.Stack())).Msg("fetching latest block number for evm state poller")
		}
		telemetry.MetricUpstreamLatestBlockPolled.WithLabelValues(
			e.projectId,
			e.upstream.VendorName(),
			networkLabel,
			e.upstream.Id(),
		).Inc()
		blockNum, blockTimestamp, header, err := e.fetchBlock(ctx, "latest")
		if err != nil || blockNum == 0 {
			if bound != nil {
				bound.intake.PollFailed(e.upstream.Id())
			}
			if src != nil {
				e.pollFailures.Add(1)
			}
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
						e.skipLatestSince = time.Now()
						e.logger.Warn().Err(err).Dur("retryIn", LatestBlockSkipExpiry.Load()).Msgf("upstream does not support fetching latest block number in evm state poller after %d consecutive failures, pausing latest polls", e.latestBlockFailureCount)
					} else {
						e.logger.Debug().Err(err).Msgf("upstream does not seem to support fetching latest block number in evm state poller after %d consecutive failures, will retry again", e.latestBlockFailureCount)
					}
				}
				e.stateMu.Unlock()
				return 0, nil
			} else {
				// An open circuit breaker short-circuits before tryForward
				// records the failure, which would freeze the upstream's error
				// rate; record it here. Other failures are already recorded.
				if e.tracker != nil && common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen) {
					e.tracker.RecordUpstreamRequest(e.upstream, "eth_getBlockByNumber", common.DataFinalityStateRealtime)
					e.tracker.RecordUpstreamFailure(e.upstream, "eth_getBlockByNumber", common.DataFinalityStateRealtime, err)
				}
				e.logger.Warn().Err(err).Msg("failed to get latest block number in evm state poller")
				return 0, err
			}
		}

		// Mark as successful and reset failure counter
		e.stateMu.Lock()
		e.latestBlockSuccessfulOnce = true
		e.latestBlockFailureCount = 0
		e.stateMu.Unlock()

		if src != nil {
			e.pollFailures.Store(0)
		}
		if bound != nil {
			// Intake parses, verifies a major jump and moves the counter
			// through ObserveHead; the fetch itself must not move it.
			bound.intake.Polled(e.upstream.Id(), header, kind)
			counter := e.latestBlockShared.GetValue()
			if counter >= blockNum {
				e.tracker.SetLatestBlockNumber(e.upstream, blockNum, blockTimestamp)
			}
			e.logger.Debug().
				Int64("blockNumber", blockNum).
				Int64("blockTimestamp", blockTimestamp).
				Int64("counter", counter).
				Msg("fetched latest block from upstream for its intake")
			return counter, nil
		}

		// A major move must pass a fresh chain-identity check before entering
		// the shared counter / tracker (see verifyChainIdOnMajorHeadMove).
		if !e.verifyChainIdOnMajorHeadMove(ctx, "latest", e.latestBlockShared.GetValue(), blockNum) {
			return 0, nil
		}

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

// RefreshLatestBlock starts a background latest-block poll when the stored
// head is older than the poll debounce and no refresh is already in flight,
// so a head read on the request path does not wait for the next ticker poll.
// It never blocks and spawns at most one goroutine per poller at a time.
func (e *EvmStatePoller) RefreshLatestBlock() {
	e.refreshLatest(true, HeaderPolled)
}

// PollHead is RefreshLatestBlock for a hash vote (spec §6.3): it polls now,
// however fresh the shared counter or this instance's last fetch is, since
// another instance's fetch or a client response votes no hash here and a
// vote from before a reorg is no evidence. Its callers pace their asks.
// fork: the poll asks a voter whose vote the pins moved off.
func (e *EvmStatePoller) PollHead(fork bool) {
	kind := HeaderPolled
	if fork {
		kind = HeaderForkPolled
	}
	e.refreshLatest(false, kind)
}

// refreshLatest: shared, poll only once the shared counter is older than
// the debounce, and then only once this instance's last fetch is too;
// otherwise poll now, as the caller paces its own asks.
func (e *EvmStatePoller) refreshLatest(shared bool, kind HeaderKind) {
	if e.IsObjectNull() || e.latestBlockShared == nil || e.shouldSkipLatestBlockCheck() {
		return
	}
	if shared {
		e.stateMu.RLock()
		cfg := e.cfg
		e.stateMu.RUnlock()
		if !e.latestBlockShared.IsStale(e.latestDebounce(cfg)) {
			return
		}
	}
	if !e.latestRefreshInFlight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer e.latestRefreshInFlight.Store(false)
		ctx, cancel := context.WithTimeout(e.appCtx, 10*time.Second)
		defer cancel()
		if _, err := e.pollLatest(ctx, shared, kind); err != nil {
			e.logger.Debug().Err(err).Msg("on-demand latest block refresh failed in evm state poller")
		}
	}()
}

func (e *EvmStatePoller) SuggestLatestBlock(blockNumber int64) {
	// Best-effort, non-blocking update.
	//
	// IMPORTANT:
	// - This must be fast and MUST NOT be blocked by any refresh/thundering-herd coordination.
	// - TryUpdate is local-only and schedules remote propagation asynchronously via the shared variable's
	//   deduped publish-first background push.
	currentValue := e.latestBlockShared.GetValue()
	if blockNumber <= currentValue {
		e.logger.Trace().
			Int64("blockNumber", blockNumber).
			Int64("currentValue", currentValue).
			Msg("skipping latest block suggestion as it's not newer")
		return
	}

	// A major forward jump from an out-of-band suggestion has the exact shape of
	// a cross-wired / poisoned upstream: a 200-OK response carrying another
	// chain's (higher) height. The verified poll path already gates such moves
	// behind a fresh chain-identity check (verifyChainIdOnMajorHeadMove); route
	// suggestions through the same gate before the sample can enter the shared
	// counter and skew every lag-based routing decision. That check makes a live
	// eth_chainId call, so it runs OFF the hot path and this function stays
	// non-blocking. Small advances (the common keep-fresh case) still apply
	// inline with zero added latency, exactly as before.
	if currentValue > 0 && blockNumber-currentValue > e.majorHeadMoveThreshold() {
		e.verifyThenSuggestLatestBlock(blockNumber)
		return
	}

	newValue := e.latestBlockShared.TryUpdate(e.appCtx, blockNumber)
	e.logger.Trace().
		Int64("blockNumber", blockNumber).
		Int64("previousValue", currentValue).
		Int64("newValue", newValue).
		Msg("latest block suggestion applied")
}

// ObserveHead is the per-upstream part of intake for a source header: the
// height goes through SuggestLatestBlock, so a major jump is verified before
// it moves the counter. accepted: the counter holds the header's height now.
func (e *EvmStatePoller) ObserveHead(raw json.RawMessage) (accepted bool) {
	var h struct {
		Number string `json:"number"`
	}
	if common.SonicCfg.Unmarshal(raw, &h) != nil {
		return false
	}
	n, err := common.HexToInt64(h.Number)
	if err != nil || n < 0 {
		return false
	}
	e.SuggestLatestBlock(n)
	return e.LatestBlock() >= n
}

// pollSource is where a poller's latest headers go.
type pollSource struct {
	intake PollIntake
}

// HeaderKind is what a polled header is to intake: which call polled it.
type HeaderKind uint8

const (
	// HeaderPolled: a poll of latest that is not its source's own channel
	// (a full poll, a refresh, a catch-up poll); it votes (spec §6.3).
	HeaderPolled HeaderKind = iota
	// HeaderStreamed: a header on its source's own channel (spec §6.2): a
	// push, or a poll source's poll at its cadence.
	HeaderStreamed
	// HeaderForkPolled: the answer to a fork poll (spec §6.3), fresh
	// evidence of the node's head whatever its earlier votes say.
	HeaderForkPolled
)

// PollIntake takes a bound poller's latest headers.
type PollIntake interface {
	Polled(upstreamId string, raw json.RawMessage, kind HeaderKind)
	// PollFailed notes a poll of latest that fetched no header (spec §6.1).
	PollFailed(upstreamId string)
}

// BindIntake sends this poller's latest headers to intake; Set makes it a
// poll source, polled at its cadence.
func (e *EvmStatePoller) BindIntake(intake PollIntake) {
	e.pollSrc.Store(&pollSource{intake: intake})
}

// LatestUnsupported reports the give-up after latest was refused as
// unsupported; it lapses LatestBlockSkipExpiry after it began.
func (e *EvmStatePoller) LatestUnsupported() bool {
	return e.shouldSkipLatestBlockCheck()
}

// Set polls latest at the poll source's cadence while topics include
// newHeads, feeding each header to the bound intake; empty returns to
// statePollerInterval. It starts the poll loop when Bootstrap did not, as
// with statePollerInterval 0.
func (e *EvmStatePoller) Set(topics []indexer.Topic) {
	on := slices.Contains(topics, indexer.TopicNewHeads) && e.pollSrc.Load() != nil
	if e.pollHeads.Swap(on) == on || !on {
		return
	}
	e.pollFailures.Store(0)
	if cfg := e.upstream.Config(); cfg != nil && cfg.Evm != nil {
		e.startLoop(cfg.Evm.StatePollerInterval.Duration())
	}
	select {
	case e.pollWake <- struct{}{}:
	default:
	}
}

// activePollSource is the bound source while it feeds newHeads, else nil.
func (e *EvmStatePoller) activePollSource() *pollSource {
	if !e.pollHeads.Load() {
		return nil
	}
	return e.pollSrc.Load()
}

// pollCadence is the poller's debounce (resolveDebounce: the learned block
// time × dynamicBlockTimeDebounceMultiplier), at most pollSourceMaxCadence;
// statePollerDebounce overrides it, cap included.
func (e *EvmStatePoller) pollCadence() time.Duration {
	e.stateMu.RLock()
	cfg, override := e.cfg, e.debounceInterval != 0
	e.stateMu.RUnlock()
	d := e.resolveDebounce(cfg)
	if override {
		return d
	}
	return min(d, pollSourceMaxCadence)
}

// nextPollDelay is the poll loop's wait: until the next full state poll,
// interval after lastFull (none while interval is 0), or sooner while a poll
// source, its pollSourceDelay. false: nothing is due until a wake.
func (e *EvmStatePoller) nextPollDelay(interval time.Duration, lastFull time.Time) (d time.Duration, ok bool) {
	if interval > 0 {
		d, ok = max(interval-time.Since(lastFull), 0), true
	}
	if e.activePollSource() != nil {
		if p := e.pollSourceDelay(); !ok || p < d {
			d, ok = p, true
		}
	}
	return d, ok
}

// pollSourceDelay is a poll source's wait between latest polls: its
// cadence, doubled per failed poll in a row up to PollSourceMaxBackoff.
func (e *EvmStatePoller) pollSourceDelay() time.Duration {
	d := e.pollCadence()
	if n := e.pollFailures.Load(); n > 0 {
		d = max(d, min(d<<min(n, 16), PollSourceMaxBackoff.Load()))
	}
	return d
}

// BackedOffCadence is how often this upstream's latest head reaches this
// instance now, the head bench's cadence (spec §6.1, §6.8): while a poll
// source, its backed-off pollSourceDelay; a wss subscription source pushes
// every head, so its poll cadence; else the full poll interval or the
// debounce, the longer, since a poll inside the debounce reuses the stored
// head.
func (e *EvmStatePoller) BackedOffCadence() time.Duration {
	if e.activePollSource() != nil {
		return e.pollSourceDelay()
	}
	c := e.upstream.Config()
	if c != nil && c.IsSubscriptionSource() && util.IsWsEndpoint(c.Endpoint) {
		return e.pollCadence()
	}
	e.stateMu.RLock()
	cfg := e.cfg
	e.stateMu.RUnlock()
	interval := time.Duration(0)
	if c != nil && c.Evm != nil {
		interval = c.Evm.StatePollerInterval.Duration()
	}
	return max(interval, e.resolveDebounce(cfg))
}

// latestDebounce is how fresh a latest poll keeps the counter: while a poll
// source, its backed-off pollSourceDelay, so every scheduled poll fetches
// and client traffic cannot poll faster.
func (e *EvmStatePoller) latestDebounce(cfg *common.EvmNetworkConfig) time.Duration {
	if e.activePollSource() != nil {
		return e.pollSourceDelay()
	}
	return e.resolveDebounce(cfg)
}

// verifyThenSuggestLatestBlock validates a MAJOR suggested forward jump with a
// fresh chain-identity check and applies it to the shared counter only if it
// passes. A proven cross-wired endpoint is cordoned by the check itself; an
// unverifiable one (a transient eth_chainId failure) drops the suggestion for
// now and it is re-observed on the next suggestion or verified poll. Runs in
// its own goroutine so the caller (response enrichment) never blocks, and at
// most one verification is in flight per poller.
func (e *EvmStatePoller) verifyThenSuggestLatestBlock(blockNumber int64) {
	if !e.latestMajorVerifyInProgress.TryLock() {
		return
	}
	go func() {
		defer e.latestMajorVerifyInProgress.Unlock()

		ctx, cancel := context.WithTimeout(e.appCtx, 5*time.Second)
		defer cancel()

		// Re-read: another path may have advanced the head while this was queued,
		// turning the jump into a small (or already-applied) one.
		currentValue := e.latestBlockShared.GetValue()
		if blockNumber <= currentValue {
			return
		}
		if blockNumber-currentValue > e.majorHeadMoveThreshold() &&
			!e.verifyChainIdOnMajorHeadMove(ctx, "latest", currentValue, blockNumber) {
			e.logger.Warn().
				Int64("blockNumber", blockNumber).
				Int64("currentValue", currentValue).
				Msg("dropping major latest block suggestion: chain-identity check did not pass")
			return
		}
		newValue := e.latestBlockShared.TryUpdate(e.appCtx, blockNumber)
		e.logger.Debug().
			Int64("blockNumber", blockNumber).
			Int64("previousValue", currentValue).
			Int64("newValue", newValue).
			Msg("verified major latest block suggestion applied")
	}()
}

func (e *EvmStatePoller) LatestBlock() int64 {
	return e.latestBlockShared.GetValue()
}

// OnLatestBlock registers cb to fire on every forward advance of the latest block
// number, regardless of source — a proactive poll, a SuggestLatestBlock
// write-through from request traffic, or cross-node propagation of the shared
// counter all flow through the same callback.
//
// IMPORTANT: cb runs synchronously inside the shared-variable update path, so it
// MUST NOT block (do a non-blocking hand-off and return). Callbacks cannot be
// unregistered, so register once per long-lived consumer, never per request.
func (e *EvmStatePoller) OnLatestBlock(cb func(int64)) {
	if e.latestBlockShared != nil {
		e.latestBlockShared.OnValue(cb)
	}
}

// OnFinalizedBlock is OnLatestBlock for the finalized block number.
func (e *EvmStatePoller) OnFinalizedBlock(cb func(int64)) {
	if e.finalizedBlockShared != nil {
		e.finalizedBlockShared.OnValue(cb)
	}
}

// verifyChainIdOnMajorHeadMove gates a polled head sample that moved beyond
// the shared rollback tolerance (in either direction) behind a fresh chain
// identity check. Such moves are exactly what a cross-wired endpoint — a DNS
// record or load balancer briefly answering for another chain — produces, and
// once a bogus sample enters the shared counters and the tracker, every
// lag-based routing decision is skewed until corrected. Built from existing
// primitives only: the shared tolerance constant, EvmGetChainId (always a
// real upstream call), and Cordon on proven mismatch (the selection policy
// already excludes cordoned upstreams). Legitimate deep reorgs and
// post-downtime catch-ups pass the probe and are accepted unchanged; a failed
// probe drops the sample for this cycle only — the next poll re-observes the
// same height seconds later.
//
// The out-of-band Suggest* paths gate only MAJOR (> tolerance) forward jumps,
// and do so OFF the hot path: SuggestLatestBlock hands a major jump to a single
// background verification and SuggestFinalizedBlock already runs in its own
// goroutine, so response enrichment never blocks on the live eth_chainId call.
// Small keep-fresh advances stay ungated and inline. The gate is required
// because a poisoned major suggestion does NOT reliably self-heal: a
// cross-wired upstream that never returns a correct low sample (it errors out
// or keeps serving the wrong chain) would otherwise leave the bogus head pinned
// in the shared counter and skew lag-based routing until manually corrected.
// majorHeadMoveThreshold is how far a head may move, in blocks, before the move
// counts as major and is re-verified against eth_chainId.
//
// The question it answers — "does this move represent more chain than we could
// plausibly have missed?" — is about TIME, so the threshold is
// chainIdVerifyChainProgress of chain progress converted through the network's
// measured block time. A fixed block count cannot ask it across a multi-chain
// fleet: DefaultToleratedBlockHeadRollback's 1024 blocks is ~3.4 hours of a 12s
// chain and ~4 minutes of a 4 blocks/s one, so the same constant gates almost
// nothing on the slow chains and a fair amount on the fast ones. Two chains
// cross-wired at similar heights therefore slip through on exactly the chains
// where the check is loosest.
//
// Clamped to DefaultToleratedBlockHeadRollback at the top so this can only ever
// ADD verification relative to the previous behaviour, never remove it —
// including on a sub-60ms-block chain, where a raw window would derive a LOOSER
// threshold than the old constant. While the block time is still unknown (cold
// start) it falls back to that constant, so nothing changes until there is a
// measurement to act on.
func (e *EvmStatePoller) majorHeadMoveThreshold() int64 {
	blockTime := e.tracker.GetNetworkBlockTime(e.upstream.NetworkId())
	if blockTime <= 0 {
		return common.DefaultToleratedBlockHeadRollback
	}
	blocks := int64(chainIdVerifyChainProgress / blockTime)
	if blocks < chainIdVerifyMinBlocks {
		return chainIdVerifyMinBlocks
	}
	if blocks > common.DefaultToleratedBlockHeadRollback {
		return common.DefaultToleratedBlockHeadRollback
	}
	return blocks
}

func (e *EvmStatePoller) verifyChainIdOnMajorHeadMove(ctx context.Context, tag string, current, polled int64) bool {
	if current <= 0 || absInt64(polled-current) <= e.majorHeadMoveThreshold() {
		return true
	}
	mismatch, err := e.checkChainId(ctx)
	if mismatch != nil {
		e.cordonForChainIdMismatch(tag, current, polled, mismatch)
		return false
	}
	if err != nil {
		e.logger.Warn().Err(err).
			Str("tag", tag).
			Int64("currentValue", current).
			Int64("polledValue", polled).
			Msg("major head move: chainId re-validation failed, dropping sample until next poll")
		return false
	}
	return true
}

// checkChainId asks the upstream for eth_chainId. mismatch is set when the
// answer proves another chain; err when the answer proves nothing. Both nil
// when it matches, or when the chain identity is not pinned yet.
func (e *EvmStatePoller) checkChainId(ctx context.Context) (mismatch, err error) {
	cfgChainId := int64(0)
	if cfg := e.upstream.Config(); cfg != nil && cfg.Evm != nil {
		cfgChainId = cfg.Evm.ChainId
	}
	if cfgChainId <= 0 {
		// Chain identity not pinned yet (auto-detection still in flight) —
		// nothing trustworthy to verify against.
		return nil, nil
	}
	eu, ok := e.upstream.(common.EvmUpstream)
	if !ok {
		return nil, nil
	}
	detected, err := eu.EvmGetChainId(ctx)
	if err != nil {
		// The gRPC BDS client surfaces a cross-wired server as a typed
		// mismatch (it compares the ChainId response itself) — treat that as
		// proof, same as a differing answer below.
		if common.HasErrorCode(err, common.ErrCodeEndpointChainIdMismatch) {
			return err, nil
		}
		return nil, err
	}
	if detected != strconv.FormatInt(cfgChainId, 10) {
		return fmt.Errorf("eth_chainId returned %s but upstream is configured for chainId %d", detected, cfgChainId), nil
	}
	return nil, nil
}

// cordonForChainIdMismatch fails loud on a proven cross-wired endpoint: the
// sample is rejected and the upstream is cordoned until eth_chainId matches
// chainIdCordonMatches times in a row, checked every ChainIdCordonRecheck.
func (e *EvmStatePoller) cordonForChainIdMismatch(tag string, current, polled int64, cause error) {
	e.logger.Error().Err(cause).
		Str("tag", tag).
		Int64("currentValue", current).
		Int64("polledValue", polled).
		Msg("major head move REJECTED: upstream answers for a different chain — cordoning upstream")
	reason := fmt.Sprintf("chain identity mismatch on major %s head move: %s", tag, cause.Error())
	e.chainIdRecheckMu.Lock()
	defer e.chainIdRecheckMu.Unlock()
	e.chainIdCordonReason.Store(reason)
	e.chainIdMatches = 0
	e.upstream.Cordon("*", reason)
	if e.chainIdRecheck != nil || e.appCtx.Err() != nil {
		return
	}
	e.chainIdRecheck = time.AfterFunc(ChainIdCordonRecheck.Load(), e.recheckChainIdCordon)
	e.chainIdRecheckUnwatch = context.AfterFunc(e.appCtx, func() {
		e.chainIdRecheckMu.Lock()
		defer e.chainIdRecheckMu.Unlock()
		if e.chainIdRecheck != nil {
			e.chainIdRecheck.Stop()
			e.chainIdRecheck = nil
		}
	})
}

// ChainIdCordoned reports whether the upstream is cordoned because it
// answers for another chain: the cordon this poller set is still in place.
func (e *EvmStatePoller) ChainIdCordoned() bool {
	reason, _ := e.chainIdCordonReason.Load().(string)
	if reason == "" || e.tracker == nil {
		return false
	}
	current, cordoned := e.tracker.CordonedReason(e.upstream, "*")
	return cordoned && current == reason
}

// recheckChainIdCordon lifts the chain-identity cordon once eth_chainId
// matched chainIdCordonMatches checks in a row, as the consensus sit-out
// lifts its own, unless a cordon with another reason replaced it. A
// mismatch cordons again if something lifted the cordon meanwhile. Until
// then it checks every ChainIdCordonRecheck. A major head move after the
// lift still passes verifyChainIdOnMajorHeadMove first.
func (e *EvmStatePoller) recheckChainIdCordon() {
	if e.appCtx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(e.appCtx, 10*time.Second)
	mismatch, err := e.checkChainId(ctx)
	cancel()

	e.chainIdRecheckMu.Lock()
	defer e.chainIdRecheckMu.Unlock()
	if e.chainIdRecheck == nil {
		return
	}
	reason, _ := e.chainIdCordonReason.Load().(string)
	current, cordoned := e.tracker.CordonedReason(e.upstream, "*")
	if mismatch != nil || err != nil {
		e.chainIdMatches = 0
		if mismatch != nil && !cordoned {
			e.upstream.Cordon("*", reason)
			e.logger.Warn().Err(mismatch).Msg("upstream still answers for another chain after its cordon was lifted, cordoning it again")
		}
		e.logger.Warn().Err(errors.Join(mismatch, err)).Dur("retryIn", ChainIdCordonRecheck.Load()).
			Msg("upstream still fails the chain-identity check, keeping it cordoned")
		e.chainIdRecheck.Reset(ChainIdCordonRecheck.Load())
		return
	}
	if e.chainIdMatches++; e.chainIdMatches < chainIdCordonMatches {
		e.logger.Info().Int("matches", e.chainIdMatches).Dur("recheckIn", ChainIdCordonRecheck.Load()).
			Msg("upstream answers for its configured chain again, keeping it cordoned until the next check matches too")
		e.chainIdRecheck.Reset(ChainIdCordonRecheck.Load())
		return
	}
	e.chainIdMatches = 0
	e.chainIdRecheck = nil
	e.chainIdRecheckUnwatch()
	if cordoned && current == reason {
		e.upstream.Uncordon("*", reason)
		e.logger.Info().Msg("upstream answers for its configured chain again, uncordoned")
	}
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
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

	e.stateMu.RLock()
	cfg := e.cfg
	networkLabel := e.networkLabel
	e.stateMu.RUnlock()

	dbi := e.resolveFinalizedDebounce(cfg, e.resolveDebounce(cfg))

	return e.finalizedBlockShared.TryUpdateIfStale(ctx, dbi, func(ctx context.Context) (int64, error) {
		e.logger.Trace().Msg("fetching finalized block number for evm state poller")
		telemetry.MetricUpstreamFinalizedBlockPolled.WithLabelValues(
			e.projectId,
			e.upstream.VendorName(),
			networkLabel,
			e.upstream.Id(),
		).Inc()

		// Actually fetch from upstream
		blockNum, _, _, err := e.fetchBlock(ctx, "finalized")
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
				// See PollLatestBlockNumber.
				if e.tracker != nil && common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen) {
					e.tracker.RecordUpstreamRequest(e.upstream, "eth_getBlockByNumber", common.DataFinalityStateFinalized)
					e.tracker.RecordUpstreamFailure(e.upstream, "eth_getBlockByNumber", common.DataFinalityStateFinalized, err)
				}
				e.logger.Warn().Err(err).Msg("failed to get finalized block number in evm state poller")
				return 0, err
			}
		}

		// Mark as successful and reset failure counter
		e.stateMu.Lock()
		e.finalizedBlockSuccessfulOnce = true
		e.finalizedBlockFailureCount = 0
		e.stateMu.Unlock()

		// Same chain-identity gate as the latest ratchet (see there).
		if !e.verifyChainIdOnMajorHeadMove(ctx, "finalized", e.finalizedBlockShared.GetValue(), blockNum) {
			return 0, nil
		}

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
		e.logger.Trace().
			Int64("blockNumber", blockNumber).
			Int64("currentValue", currentValue).
			Msg("accepting finalized block suggestion")

		// Create a timeout context to avoid blocking forever on Redis operations
		ctx, cancel := context.WithTimeout(e.appCtx, 5*time.Second)
		defer cancel()

		// Gate a major forward jump behind a fresh chain-identity check, the same
		// protection the verified poll path and SuggestLatestBlock apply: a
		// cross-wired endpoint reporting another chain's height must not enter the
		// shared finalized counter. Already off the hot path here (own goroutine),
		// so the live eth_chainId call is safe to make.
		if blockNumber-currentValue > e.majorHeadMoveThreshold() &&
			!e.verifyChainIdOnMajorHeadMove(ctx, "finalized", currentValue, blockNumber) {
			e.logger.Warn().
				Int64("blockNumber", blockNumber).
				Int64("currentValue", currentValue).
				Msg("dropping major finalized block suggestion: chain-identity check did not pass")
			return
		}

		e.finalizedBlockShared.TryUpdate(ctx, blockNumber)
		e.logger.Trace().
			Int64("blockNumber", blockNumber).
			Msg("finalized block suggestion applied")
	}()
}

func (e *EvmStatePoller) FinalizedBlock() int64 {
	return e.finalizedBlockShared.GetValue()
}

func (e *EvmStatePoller) PollEarliestBlockNumber(ctx context.Context, probe common.EvmAvailabilityProbeType, staleness time.Duration) (int64, error) {
	// Initialize storage for probe
	e.earliestMu.Lock()
	if _, ok := e.earliestByProbe[probe]; !ok {
		key := fmt.Sprintf("earliestBlock/%s/%s", common.UniqueUpstreamKey(e.upstream), string(probe))
		e.earliestByProbe[probe] = e.sharedStateRegistry.GetCounterInt64(key, 0)
	}
	v := e.earliestByProbe[probe]
	e.earliestMu.Unlock()

	// Use TryUpdateIfStale for cross-instance coordination
	// Only one instance will do the binary search at a time
	return v.TryUpdateIfStale(ctx, staleness, func(ctx context.Context) (int64, error) {
		latest := e.latestBlockShared.GetValue()
		if latest == 0 {
			var err error
			latest, err = e.PollLatestBlockNumber(ctx)
			if err != nil || latest == 0 {
				e.logger.Error().
					Err(err).
					Str("probe", string(probe)).
					Str("upstreamId", e.upstream.Config().Id).
					Msg("failed to get latest block for earliest detection; block availability checks will fail-open")
				return 0, err
			}
		}

		val, err := e.binarySearchEarliest(ctx, probe, 0, latest)
		if err != nil {
			e.logger.Error().
				Err(err).
				Int64("latestBlock", latest).
				Str("probe", string(probe)).
				Str("upstreamId", e.upstream.Config().Id).
				Msg("binary search failed for earliest block detection; block availability checks will fail-open")
			return 0, err
		}

		e.logger.Debug().
			Int64("earliestBlock", val).
			Int64("latestBlock", latest).
			Str("probe", string(probe)).
			Str("upstreamId", e.upstream.Config().Id).
			Msg("binary search completed for earliest block availability bound")

		return val, nil
	})
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

// initializeEarliestBlockDetectionAndStartScheduler performs one-time earliest block detection
// for each configured probe, and starts a periodic scheduler if updateRate is configured.
// Called from Poll() but only does initial detection once per probe.
func (e *EvmStatePoller) initializeEarliestBlockDetectionAndStartScheduler(ctx context.Context) {
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
			// Still unavailable - log error and skip for this cycle
			// The next poll cycle will retry, and the scheduler will also keep retrying
			if err != nil {
				e.logger.Error().
					Err(err).
					Str("upstreamId", e.upstream.Config().Id).
					Msg("failed to fetch latest block for earliest bound detection; block availability checks will fail-open")
			} else {
				e.logger.Warn().
					Str("upstreamId", e.upstream.Config().Id).
					Msg("latest block still not available after fresh poll; skipping earliest detection for this cycle (will retry)")
			}
			return
		}
		e.logger.Debug().Int64("latestBlock", latest).Msg("successfully fetched latest block, proceeding with earliest detection")
	}

	for probe, ps := range schedules {
		// Read flags, make decisions, and claim work atomically under a single lock hold.
		// This prevents TOCTOU race where flags could change between reading and using them.
		e.earliestMu.Lock()

		// Initialize shared var if missing
		if _, ok := e.earliestByProbe[probe]; !ok {
			key := sharedCounterKey("earliestBlock", common.UniqueUpstreamKey(e.upstream), string(probe))
			e.earliestByProbe[probe] = e.sharedStateRegistry.GetCounterInt64(key, 0)
		}

		initialDetectionDone := e.earliestInitialDetectionDone[probe]
		schedulerStarted := e.earliestSchedulerStarted[probe]

		// Skip if this instance already did initial detection AND scheduler is running
		if initialDetectionDone && schedulerStarted {
			e.earliestMu.Unlock()
			continue
		}

		// Claim initial detection work if needed (set flag optimistically before releasing lock)
		shouldDoDetection := false
		if !initialDetectionDone {
			e.earliestInitialDetectionDone[probe] = true
			shouldDoDetection = true
		}

		// Claim scheduler start if needed (set flag before releasing lock)
		shouldStartScheduler := false
		if ps.rate > 0 && !schedulerStarted {
			e.earliestSchedulerStarted[probe] = true
			shouldStartScheduler = true
		}

		e.earliestMu.Unlock()

		// Do detection outside lock (to avoid blocking other probes/goroutines)
		if shouldDoDetection {
			earliest, err := e.PollEarliestBlockNumber(ctx, probe, 1*time.Millisecond)
			if err != nil {
				e.logger.Error().
					Err(err).
					Str("probe", string(probe)).
					Str("upstreamId", e.upstream.Config().Id).
					Msg("failed to initialize earliest block bound for probe; block availability checks will fail-open (will retry)")
				// Reset to allow retry on next Poll() cycle
				e.earliestMu.Lock()
				e.earliestInitialDetectionDone[probe] = false
				e.earliestMu.Unlock()
			} else {
				e.logger.Info().
					Int64("earliestBlock", earliest).
					Str("probe", string(probe)).
					Str("upstreamId", e.upstream.Config().Id).
					Msg("initial earliest block detection completed for this instance")
			}
		}

		// Start scheduler outside lock
		if shouldStartScheduler {
			e.logger.Info().
				Str("probe", string(probe)).
				Str("updateRate", ps.rate.String()).
				Str("upstreamId", e.upstream.Config().Id).
				Msg("started periodic scheduler for earliest block availability bound")
			go e.runPeriodicEarliestBlockBoundUpdateLoop(probe, ps.rate)
		}
	}
}

// runPeriodicEarliestBlockBoundUpdateLoop runs a goroutine that periodically re-detects
// the earliest available block for a given probe. Uses TryUpdateIfStale for cross-instance
// coordination so only one container instance performs the binary search at a time.
func (e *EvmStatePoller) runPeriodicEarliestBlockBoundUpdateLoop(probe common.EvmAvailabilityProbeType, rate time.Duration) {
	ticker := time.NewTicker(rate)
	defer ticker.Stop()
	for {
		select {
		case <-e.appCtx.Done():
			return
		case <-ticker.C:
			// Use PollEarliestBlockNumber with rate as staleness
			// TryUpdateIfStale handles cross-instance coordination
			_, err := e.PollEarliestBlockNumber(e.appCtx, probe, rate)
			if err != nil {
				e.logger.Debug().
					Err(err).
					Str("probe", string(probe)).
					Str("upstreamId", e.upstream.Config().Id).
					Msg("earliest block update cycle failed; will retry")
			}
		}
	}
}

// fallbackFinalityMinAge is the minimum WALL-CLOCK age a block must reach
// before the SYNTHETIC finalized head can cover it. It applies only on the
// fallback path — when an upstream reports no finalized block at all — and only
// ever DEEPENS the configured block depth, never shortens it.
//
// A block count alone cannot express "old enough that a reorg is implausible".
// DefaultEvmFinalityDepth's 1024 blocks is ~3.4 hours of a 12s chain but only a
// couple of minutes of a sub-second one, so the same constant is far more
// conservative than needed on slow chains and potentially SHALLOWER than the
// chain's real reorg risk on fast ones — and this value decides what erpc
// treats as immutable and therefore permanently cacheable. Getting it wrong in
// the shallow direction caches data that can still be reorged away.
//
// 30 minutes sits comfortably beyond Ethereum's ~13-minute finality and beyond
// the unsafe-head reorg windows typical L2s expose. On any chain where the
// configured block depth already represents more than this, nothing changes.
const fallbackFinalityMinAge = 30 * time.Minute

// fallbackFinalityDepth is how far below the head the synthetic finalized block
// sits: the DEEPER of the configured block count and fallbackFinalityMinAge of
// chain progress. Taking the deeper of the two is what makes this safe to ship
// unconditionally — the synthetic finalized head can only ever move further
// from the tip, never closer, so no block becomes "final" earlier than it does
// today. While the block time is unknown the configured count stands alone.
func (e *EvmStatePoller) fallbackFinalityDepth() int64 {
	e.stateMu.RLock()
	depth := int64(common.DefaultEvmFinalityDepth)
	if e.cfg != nil && e.cfg.FallbackFinalityDepth > 0 {
		depth = e.cfg.FallbackFinalityDepth
	}
	e.stateMu.RUnlock()

	if blockTime := e.tracker.GetNetworkBlockTime(e.upstream.NetworkId()); blockTime > 0 {
		if byAge := int64(fallbackFinalityMinAge / blockTime); byAge > depth {
			depth = byAge
		}
	}
	return depth
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
	depth := e.fallbackFinalityDepth()
	if latestBlock > depth {
		fb = latestBlock - depth
	}

	e.logger.Debug().
		Int64("inferredFinalizedBlock", fb).
		Int64("fallbackFinalityDepth", depth).
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

// GetDiagnostics returns diagnostic information about the state poller including
// block bounds, probe status, and any detection issues for health check visibility.
func (e *EvmStatePoller) GetDiagnostics() *common.EvmStatePollerDiagnostics {
	if e == nil {
		return nil
	}

	// Collect state under stateMu first, then release before acquiring earliestMu
	// to avoid nested lock ordering issues (always acquire locks in consistent order).
	e.stateMu.RLock()
	diag := &common.EvmStatePollerDiagnostics{
		Enabled:        e.Enabled,
		LatestBlock:    e.latestBlockShared.GetValue(),
		FinalizedBlock: e.finalizedBlockShared.GetValue(),
		SyncingState:   e.syncingState.String(),

		// Syncing check status
		SkipSyncingCheck: e.skipSyncingCheck,

		// Latest block detection status
		SkipLatestBlockCheck:      e.skipLatestBlockCheck,
		LatestBlockFailureCount:   e.latestBlockFailureCount,
		LatestBlockSuccessfulOnce: e.latestBlockSuccessfulOnce,

		// Finalized block detection status
		SkipFinalizedCheck:           e.skipFinalizedCheck,
		FinalizedBlockFailureCount:   e.finalizedBlockFailureCount,
		FinalizedBlockSuccessfulOnce: e.finalizedBlockSuccessfulOnce,
	}

	// Also reflect operator-configured skip in diagnostics.
	upsCfg := e.upstream.Config()
	if upsCfg.Evm != nil && upsCfg.Evm.SkipSyncingCheck != nil && *upsCfg.Evm.SkipSyncingCheck {
		diag.SkipSyncingCheck = true
	}

	// Build detection issue messages
	skipSyncingCheck := e.skipSyncingCheck || diag.SkipSyncingCheck
	syncingSuccessfulOnce := e.syncingSuccessfulOnce
	skipLatestBlockCheck := e.skipLatestBlockCheck
	latestBlockSuccessfulOnce := e.latestBlockSuccessfulOnce
	skipFinalizedCheck := e.skipFinalizedCheck
	finalizedBlockSuccessfulOnce := e.finalizedBlockSuccessfulOnce
	e.stateMu.RUnlock()

	if skipSyncingCheck && !syncingSuccessfulOnce {
		diag.SyncingCheckError = "syncing check disabled after consecutive failures (method may not be supported)"
	}
	if skipLatestBlockCheck && !latestBlockSuccessfulOnce {
		diag.LatestBlockDetectionIssue = "latest block check disabled after consecutive failures (method may not be supported)"
	}
	if skipFinalizedCheck && !finalizedBlockSuccessfulOnce {
		diag.FinalizedBlockDetectionIssue = "finalized block check disabled after consecutive failures (method may not be supported)"
	}

	// Collect earliest block bounds per probe type (separate lock)
	e.earliestMu.RLock()
	if len(e.earliestByProbe) > 0 {
		diag.EarliestByProbe = make(map[common.EvmAvailabilityProbeType]*common.EvmProbeEarliestInfo)
		for probeType, sharedVar := range e.earliestByProbe {
			info := &common.EvmProbeEarliestInfo{
				ProbeType:        probeType,
				EarliestBlock:    sharedVar.GetValue(),
				SchedulerRunning: e.earliestSchedulerStarted[probeType],
			}
			diag.EarliestByProbe[probeType] = info
		}
	}
	e.earliestMu.RUnlock()

	return diag
}

// shouldSkipLatestBlockCheck reports the give-up after failed latest polls;
// it lapses LatestBlockSkipExpiry after it was set, with a fresh count.
func (e *EvmStatePoller) shouldSkipLatestBlockCheck() bool {
	e.stateMu.RLock()
	skip, expired := e.skipLatestBlockCheck, e.skipLatestBlockCheck && time.Since(e.skipLatestSince) >= LatestBlockSkipExpiry.Load()
	e.stateMu.RUnlock()
	if !expired {
		return skip
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	// Another caller may have lifted it, or a new give-up started, meanwhile.
	if e.skipLatestBlockCheck && time.Since(e.skipLatestSince) >= LatestBlockSkipExpiry.Load() {
		e.skipLatestBlockCheck = false
		e.latestBlockFailureCount = 0
		e.logger.Info().Msg("retrying latest block polls after the give-up expired")
	}
	return e.skipLatestBlockCheck
}

func (e *EvmStatePoller) shouldSkipFinalizedCheck() bool {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()

	return e.skipFinalizedCheck
}

// fetchBlock returns the number and timestamp of the block at blockTag, and,
// for latest, the header as the upstream sent it.
func (e *EvmStatePoller) fetchBlock(ctx context.Context, blockTag string) (int64, int64, json.RawMessage, error) {
	pr := common.NewProbeRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByNumber","params":["%s",false]}`, util.RandomID(), blockTag),
	))
	resp, err := e.upstream.Forward(ctx, pr, true, false)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return 0, 0, nil, err
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return 0, 0, nil, err
	}
	if jrr == nil || jrr.Error != nil {
		return 0, 0, nil, jrr.Error
	}

	if jrr.IsResultEmptyish(ctx) {
		return 0, 0, nil, nil
	}

	numberStr, err := jrr.PeekStringByPath(ctx, "number")
	if err != nil {
		return 0, 0, nil, &common.BaseError{
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
		return 0, 0, nil, err
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

	var header json.RawMessage
	if blockTag == "latest" {
		// resp is released on return.
		header = bytes.Clone(jrr.GetResultBytes())
	}
	return blockNum, blockTimestamp, header, nil
}

func (e *EvmStatePoller) fetchSyncingState(ctx context.Context) (bool, error) {
	pr := common.NewProbeRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_syncing","params":[]}`, util.RandomID())))

	resp, err := e.upstream.Forward(ctx, pr, true, false)
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
	pr := common.NewProbeRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByNumber","params":["%s",false]}`,
			util.RandomID(), hex,
		),
	))
	resp, err := e.upstream.Forward(ctx, pr, true, false)
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
	pr := common.NewProbeRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByNumber","params":["%s",false]}`,
			util.RandomID(), hex,
		),
	))
	resp, err := e.upstream.Forward(ctx, pr, true, false)
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
	pr := common.NewProbeRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getLogs","params":[{"blockHash":"%s"}]}`,
			util.RandomID(), hash,
		),
	))
	resp, err := e.upstream.Forward(ctx, pr, true, false)
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

// checkCallStateProbe verifies whether historical STATE is available at the
// given block.
//
// Strong form first: the per-architecture execution canary (the same
// integrity.ChainStateContextProbe the state prober uses — Multicall3
// getBlockNumber() on standard EVMs, ArbSys arbBlockNumber() on Nitro, where
// block.number is the L1 height) is eth_call'ed pinned at the block, and
// availability means the node EXECUTED at exactly that height. A node serving
// stale state answers eth_getBalance with a well-formed 0x0, but it cannot
// answer the canary with the pinned height — the returned number comes from
// the execution context the node actually used. A wrong canary for a chain
// (the Multicall3-on-Nitro precedent) mismatches on every upstream alike, so
// it degrades bounds symmetrically and leaves relative ranking unchanged.
//
// Where the canary yields no evidence — not deployed at that height ("0x"),
// erroring/reverting, or answering something unparseable — this DISCOVERS the
// gap and falls back to the balance heuristic, so chains and heights without
// the canary keep exactly the previous behavior.
func (e *EvmStatePoller) checkCallStateProbe(ctx context.Context, block int64) (bool, bool, error) {
	if block < 0 {
		return false, false, nil
	}
	hex := fmt.Sprintf("0x%x", uint64(block))
	var chainId int64
	if upCfg := e.upstream.Config(); upCfg != nil && upCfg.Evm != nil {
		chainId = upCfg.Evm.ChainId
	}
	canary := integrity.ChainStateContextProbe(chainId)
	pr := common.NewProbeRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_call","params":[{"to":"%s","data":"%s"},"%s"]}`,
			util.RandomID(), canary.To, canary.Data, hex,
		),
	))
	resp, err := e.upstream.Forward(ctx, pr, true, false)
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
	} else if jrr, jerr := resp.JsonRpcResponse(); jerr == nil && jrr != nil && jrr.Error == nil {
		if got, ok := parseHexQuantity(string(jrr.GetResultBytes())); ok {
			return got == block, false, nil
		}
	}
	return e.checkBalanceStateProbe(ctx, block)
}

// checkBalanceStateProbe is the weak fallback behind checkCallStateProbe:
// eth_getBalance(0x0, block) accepting any non-null result. It cannot tell
// state AT the block from state anywhere (a stale node happily answers 0x0),
// which is why it is only consulted where the execution canary yields no
// evidence.
func (e *EvmStatePoller) checkBalanceStateProbe(ctx context.Context, block int64) (bool, bool, error) {
	hex := fmt.Sprintf("0x%x", uint64(block))
	pr := common.NewProbeRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000","%s"]}`,
			util.RandomID(), hex,
		),
	))
	resp, err := e.upstream.Forward(ctx, pr, true, false)
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

		pr := common.NewProbeRequest([]byte(methodPayload))
		resp, err := e.upstream.Forward(cctx, pr, true, false)
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
