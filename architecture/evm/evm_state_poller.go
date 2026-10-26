package evm

import (
	"bytes"
	"context"
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
	"go.opentelemetry.io/otel/codes"
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

// askTimeout bounds one head Ask fetch.
var askTimeout = 10 * time.Second

// The Ask's backoff cap and expiries; exported so end-to-end tests can
// shorten them.
var (
	AskMaxBackoff        = util.NewDuration(60 * time.Second)
	PollerGiveUpExpiry   = util.NewDuration(10 * time.Minute)
	ChainIdCordonRecheck = util.NewDuration(5 * time.Minute)
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

	// syncingGiveUp, finalizedGiveUp and latestGiveUp are the give-ups of
	// the three poller methods (giveUp); stateMu guards them.
	syncingGiveUp, finalizedGiveUp, latestGiveUp giveUp

	// Avoid making redundant calls based on a networks block time
	// i.e. latest/finalized block is not going to change within the debounce interval
	debounceInterval time.Duration

	finalizedBlockShared data.CounterInt64SharedVariable

	// The reason to not use latestBlockTimestamp is to avoid thundering herd,
	// when a node is actively syncing and timestamp is always way in the past.
	latestBlockShared data.CounterInt64SharedVariable

	// pollSrc is set once the network binds this poller as a poll source;
	// while pollHeads is on, the poll loop Asks once per backed-off
	// cadence (spec §6.8). pollWake starts a pass at once.
	pollSrc   atomic.Pointer[pollSource]
	pollHeads atomic.Bool
	pollWake  chan struct{}

	// headAsk is the upstream's one head fetch schedule (Ask).
	headAsk headAsk

	// chainIdRecheck is the pending eth_chainId check of a chain-identity
	// cordon, nil while none; chainIdRecheckUnwatch stops watching appCtx
	// for it; chainIdMatches counts its matching checks in a row.
	// chainIdCordonReason is the cordon's reason.
	chainIdRecheckMu      sync.Mutex
	chainIdRecheck        *time.Timer
	chainIdRecheckUnwatch func() bool
	chainIdMatches        int
	chainIdCordonReason   atomic.Value // string
	cordonHooksMu         sync.Mutex
	cordonHooks           []func()

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

	// Single in-flight flag for RefreshFinalizedBlock's background poll.
	finalizedRefreshInFlight atomic.Bool

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

	lbs.OnRewind(func(value int64) {
		e.tracker.RewindLatestBlockNumber(e.upstream, value)
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
// while a poll source, its Ask with a hash need once per backed-off
// cadence in between (spec §6.8).
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
		if full {
			lastFull = time.Now()
		}
		e.pollPass(full)
	}
}

// pollPass is one pass of the poll loop: the poll source's Ask, then the
// full state poll when full. A panic ends the pass, never the loop.
func (e *EvmStatePoller) pollPass(full bool) {
	defer e.recoverHeadPanic("poll-loop")
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
	defer cancel()
	var err error
	if e.activePollSource() != nil {
		// The poll source's own channel (spec §6.2); a full poll's
		// hash need right after it is met by the fetch it sent.
		err = e.Ask(nctx, common.HeadNeedHash, common.AskPoll).Err
	}
	if full {
		err = errors.Join(err, e.Poll(nctx))
	}
	if err != nil {
		if errors.Is(nctx.Err(), context.Canceled) {
			e.logger.Info().Err(err).
				Msgf("shutting down evm state poller due to context cancellation (e.g. app exiting)")
		} else {
			e.logger.Warn().Err(err).Msgf("failed to poll evm state")
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

	// Latest: an Ask with a hash need, a local fetch (spec §6.3, §6.8):
	// a hash vote, and the block time sample of a poll-only network. The
	// poll waits for it as long as for the finalized counter's refresh, and
	// the fetch goes on without it: a node that holds its answer must not
	// hold the upstream's registration past the tracker's cold window.
	wg.Add(1)
	go func() {
		defer wg.Done()
		actx, cancel := context.WithTimeout(ctx, e.sharedStateRegistry.GetUpdateMaxWait())
		defer cancel()
		err := e.Ask(actx, common.HeadNeedHash, common.AskPoll).Err
		if err != nil && actx.Err() != nil && ctx.Err() == nil {
			return
		}
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

		upsCfg := e.upstream.Config()
		if upsCfg.Evm != nil && upsCfg.Evm.SkipSyncingCheck != nil && *upsCfg.Evm.SkipSyncingCheck {
			e.stateMu.Lock()
			e.syncingState = common.EvmSyncingStateNotSyncing
			e.stateMu.Unlock()
			return
		}

		if e.synced >= FullySyncedThreshold || e.givenUp(&e.syncingGiveUp) {
			return
		}

		syncCtx, syncCancel := context.WithTimeout(ctx, 5*time.Second)
		defer syncCancel()
		syncing, err := e.fetchSyncingState(syncCtx)
		if err != nil {
			// Any eth_syncing error counts as a refusal: a node that cannot
			// answer it gives no syncing state to wait for.
			if e.refused(&e.syncingGiveUp, "eth_syncing", err) {
				e.stateMu.Lock()
				e.syncingState = common.EvmSyncingStateUnknown
				e.stateMu.Unlock()
			}
			e.logger.Warn().Bool("syncingResult", syncing).Err(err).Msg("failed to get syncing state in evm state poller")
			ermu.Lock()
			errs = append(errs, err)
			ermu.Unlock()
			return
		}

		e.logger.Debug().Bool("syncingResult", syncing).Msg("fetched syncing state")

		e.answered(&e.syncingGiveUp)
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
	// The multiplier (default 0.7) makes the debounce expire before the next
	// block is expected: freshness over saving RPC calls.
	return cfg.PollCadence(blockTime)
}

// resolveFinalizedDebounce widens the debounce for finalized polls to a fraction of the
// chain's finality depth in time. Finalized moves once per finality window, so a poll at
// the latest-block cadence mostly returns the number it already has; this bounds how far
// the finalized block can trail to that fraction of the window instead.
//
// An upstream behind the network's corroborated finalized block keeps the regular
// debounce: peers already saw the next finalized block, and a vendor whose load balancer
// mixes stale and fresh backends needs every sample to catch up. Finality reads the
// voters' finalized ballot, Head.Finalized, and an answer its upstream's own too (spec §6.4).
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

// PollLatestBlockNumber is the upstream's latest block number after an Ask
// with a number need (spec §6.8); the error is the Ask's.
func (e *EvmStatePoller) PollLatestBlockNumber(ctx context.Context) (int64, error) {
	a := e.Ask(ctx, common.HeadNeedNumber, common.AskRefresh)
	return a.Block, a.Err
}

// headAsk is the upstream's one head fetch schedule (spec §6.8): at most
// one fetch of latest in flight, and one backoff.
type headAsk struct {
	mu sync.Mutex
	// last is the newest fetch, in flight or answered.
	last *headFetch
	// failures counts the node failures in a row (nodeFailedPoll).
	failures int
}

// headFetch is one fetch of latest. answered, next and the answer are
// guarded by headAsk.mu; header and err are final once done is closed.
type headFetch struct {
	sent time.Time
	done chan struct{}
	// fork: a fork Ask started it, so its header goes to intake as
	// HeaderForkPolled; set before it starts, never after.
	fork     bool
	answered bool
	// next is the fork fetch queued while this one was in flight: it
	// starts once this one ends.
	next   *headFetch
	header common.EvmHeader
	err    error
	// parent is the span of the Ask that started the fetch: the fetch's
	// span shows under it without taking its cancellation.
	parent trace.SpanContext
}

// Ask is the upstream's head Ask (spec §6.8). A number need is met by a
// shared counter whose report is younger than the cadence; a number or
// hash need by this instance's fetch in flight, or one sent within the
// cadence whatever its outcome; a fork need only by a fetch sent after it,
// since a header from before the pins moved is no evidence of the fork
// (spec §6.3). Else it fetches latest, cache reads skipped and no rate
// permit taken, its header to intake as a hash vote. It waits for that
// fetch's answer, ctx's end or W, whichever comes first. While the
// upstream backs off it fetches nothing and returns AskBackoff at once.
func (e *EvmStatePoller) Ask(ctx context.Context, need common.HeadNeed, reason common.AskReason) common.HeadAnswer {
	if e.IsObjectNull() {
		return common.HeadAnswer{Outcome: common.AskBackoff}
	}
	ctx, span := common.StartDetailSpan(ctx, "EvmStatePoller.Ask",
		trace.WithAttributes(
			attribute.String("upstream.id", e.upstream.Id()),
			attribute.String("reason", string(reason)),
		),
	)
	defer span.End()
	f, outcome := e.ask(ctx, need, reason)
	span.SetAttributes(attribute.String("outcome", string(outcome)))
	a := common.HeadAnswer{Outcome: outcome}
	if !e.askable() {
		return a
	}
	if f != nil {
		w := time.NewTimer(e.freshnessWindow())
		defer w.Stop()
		select {
		case <-f.done:
			a.Header, a.Err = f.header, f.err
			if f.header != nil {
				a.Sent = f.sent
			}
		case <-ctx.Done():
			a.Err = ctx.Err()
		case <-w.C:
			a.Err = context.DeadlineExceeded
		}
	}
	if due := e.askDue(); due > 0 {
		a.Next = time.Now().Add(due)
	}
	a.Block = e.LatestBlock()
	return a
}

// AskReaching is an Ask for a caller that needs block (spec §6.4 band):
// a number need first; when that answer is below block and came from no
// fetch of the caller's own (the shared counter, or a fetch sent before
// the caller came), it Asks once more with a fork need, a fetch sent after
// the caller came, fresh evidence of the node's head: a counter a push
// keeps fresh over a slow link is no evidence the node lacks the block
// (spec §21). Both waits end at most W after it starts.
func (e *EvmStatePoller) AskReaching(ctx context.Context, block int64, reason common.AskReason) common.HeadAnswer {
	if e.IsObjectNull() {
		return common.HeadAnswer{Outcome: common.AskBackoff}
	}
	ctx, cancel := context.WithTimeout(ctx, e.freshnessWindow())
	defer cancel()
	a := e.Ask(ctx, common.HeadNeedNumber, reason)
	if a.Err == nil && a.Block < block && (a.Outcome == common.AskInflight || a.Outcome == common.AskSharedFresh) {
		a = e.Ask(ctx, common.HeadNeedFork, reason)
	}
	return a
}

// freshnessWindow is W of the upstream's network (health.FreshnessWindow).
func (e *EvmStatePoller) freshnessWindow() time.Duration {
	var bt time.Duration
	if e.tracker != nil {
		bt = e.tracker.GetNetworkBlockTime(e.upstream.NetworkId())
	}
	return health.FreshnessWindow(bt)
}

// AskBackground is Ask without waiting for the answer.
func (e *EvmStatePoller) AskBackground(need common.HeadNeed, reason common.AskReason) {
	if !e.IsObjectNull() {
		e.ask(context.Background(), need, reason)
	}
}

// askable: the poller has a latest counter to Ask for.
func (e *EvmStatePoller) askable() bool {
	return e.latestBlockShared != nil
}

// ask counts an Ask and returns the fetch that meets need, started when
// none does, under ctx's span; nil when the shared counter meets it or the
// upstream backs off, as it does while it has no latest counter to Ask for.
func (e *EvmStatePoller) ask(ctx context.Context, need common.HeadNeed, reason common.AskReason) (*headFetch, common.AskOutcome) {
	f, outcome := (*headFetch)(nil), common.AskBackoff
	if e.askable() {
		f, outcome = e.askFetch(trace.SpanContextFromContext(ctx), need)
	}
	telemetry.CounterHandle(telemetry.MetricHeadAskTotal, e.projectId, e.netLabel(), string(reason), string(outcome)).Inc()
	return f, outcome
}

func (e *EvmStatePoller) askFetch(parent trace.SpanContext, need common.HeadNeed) (*headFetch, common.AskOutcome) {
	if e.givenUp(&e.latestGiveUp) {
		// The give-up after latest was refused as unsupported backs off
		// until its re-check.
		return nil, common.AskBackoff
	}
	cadence, now := e.pollCadence(), time.Now()
	if need == common.HeadNeedNumber {
		if at := e.latestBlockShared.ReportAt(); !at.IsZero() && now.Sub(at) < cadence {
			return nil, common.AskSharedFresh
		}
	}
	a := &e.headAsk
	a.mu.Lock()
	f := a.last
	switch {
	case f != nil && !f.answered && need == common.HeadNeedFork:
		if f.next == nil {
			f.next = &headFetch{fork: true, done: make(chan struct{}), parent: parent}
		}
		next := f.next
		a.mu.Unlock()
		return next, common.AskFetched
	case f != nil && !f.answered:
		a.mu.Unlock()
		return f, common.AskInflight
	// Any outcome debounces for one cadence; only node failures back off
	// further.
	case f != nil && need != common.HeadNeedFork && now.Sub(f.sent) < cadence:
		a.mu.Unlock()
		return f, common.AskInflight
	case f != nil && a.failures > 0 && now.Sub(f.sent) < backoffDelay(cadence, a.failures):
		a.mu.Unlock()
		return nil, common.AskBackoff
	}
	f = &headFetch{sent: now, fork: need == common.HeadNeedFork, done: make(chan struct{}), parent: parent}
	a.last = f
	a.mu.Unlock()
	go e.runFetch(f)
	return f, common.AskFetched
}

// runFetch runs f and gives its outcome to every waiter; a panic fails
// them with an error.
func (e *EvmStatePoller) runFetch(f *headFetch) {
	var header common.EvmHeader
	var answered bool
	var err error
	defer func() {
		if rec := recover(); rec != nil {
			e.countHeadPanic("fetch", rec)
			header, answered, err = nil, false, fmt.Errorf("head ask of upstream %s panicked: %v", e.upstream.Id(), rec)
		}
		a := &e.headAsk
		a.mu.Lock()
		f.answered, f.header, f.err = true, header, err
		switch {
		case nodeFailedPoll(err):
			a.failures++
		case answered:
			a.failures = 0
		}
		next := f.next
		if next != nil {
			next.sent, a.last = time.Now(), next
		}
		a.mu.Unlock()
		close(f.done)
		if next != nil {
			go e.runFetch(next)
		}
	}()
	ctx := e.appCtx
	// A background Ask has no caller span: its fetch starts no root trace.
	if f.parent.IsValid() {
		var span trace.Span
		ctx, span = common.StartDetailSpan(trace.ContextWithSpanContext(ctx, f.parent), "EvmStatePoller.AskFetch",
			trace.WithAttributes(attribute.String("upstream.id", e.upstream.Id()), attribute.Bool("fork", f.fork)))
		defer func() {
			if err != nil {
				span.SetStatus(codes.Error, err.Error())
			}
			span.SetAttributes(attribute.Bool("header", header != nil))
			span.End()
		}()
	}
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	header, answered, err = e.fetchLatest(ctx, f)
}

// fetchLatest fetches latest for f. With an intake bound, the header goes
// to it, as a hash vote of this upstream (spec §6.3), and the counter
// moves only there, after the header passed its checks. answered: the
// node answered with a block; header: that block past intake check 0. No
// header and no error: latest was refused as unsupported, or the answer
// did not pass; after 10 refusals before any success the Ask gives up
// for PollerGiveUpExpiry (latestGiveUp), and the reconciler polls other
// sources (LatestUnsupported).
func (e *EvmStatePoller) fetchLatest(ctx context.Context, f *headFetch) (header common.EvmHeader, answered bool, err error) {
	if e.logger.GetLevel() <= zerolog.TraceLevel {
		e.logger.Trace().Str("ptr", fmt.Sprintf("%p", e)).Str("stack", string(debug.Stack())).Msg("fetching latest block number for evm state poller")
	}
	telemetry.MetricUpstreamLatestBlockPolled.WithLabelValues(
		e.projectId,
		e.upstream.VendorName(),
		e.netLabel(),
		e.upstream.Id(),
	).Inc()
	bound := e.pollSrc.Load()
	b, err := e.fetchBlock(ctx, "latest", common.UpstreamPoliciesHeadAsk)
	if err != nil || b.number == 0 {
		if refusedPoll(err) {
			e.refused(&e.latestGiveUp, "latest", err)
			return nil, false, nil
		}
		if nodeFailedPoll(err) && bound != nil {
			bound.intake.PollFailed(e.upstream.Id(), f.sent)
		}
		// An open circuit breaker short-circuits before tryForward
		// records the failure, which would freeze the upstream's error
		// rate; record it here. Other failures are already recorded.
		if e.tracker != nil && common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen) {
			e.tracker.RecordUpstreamRequest(e.upstream, "eth_getBlockByNumber", common.DataFinalityStateRealtime)
			e.tracker.RecordUpstreamFailure(e.upstream, "eth_getBlockByNumber", common.DataFinalityStateRealtime, err)
		}
		e.logger.Warn().Err(err).Msg("failed to get latest block number in evm state poller")
		return nil, false, err
	}

	e.answered(&e.latestGiveUp)

	if bound != nil {
		// Intake checks the header once (spec §13), verifies a major jump
		// and moves the counter through ObserveHead; the fetch itself must
		// not move it. The Ask answer carries the header only once it
		// passed, so no tracker holds a header intake dropped.
		if e.deliverFetched(bound, f, b) {
			header = b.header
		}
		counter := e.latestBlockShared.GetValue()
		if counter >= b.number {
			e.tracker.SetLatestBlockNumber(e.upstream, b.number, b.timestamp)
		}
		e.logger.Debug().
			Int64("blockNumber", b.number).
			Int64("blockTimestamp", b.timestamp).
			Int64("counter", counter).
			Msg("fetched latest block from upstream for its intake")
		return header, true, nil
	}

	// A major move must pass a fresh chain-identity check before entering
	// the shared counter / tracker (see verifyChainIdOnMajorHeadMove).
	if !e.verifyChainIdOnMajorHeadMove(ctx, "latest", e.latestBlockShared.GetValue(), b.number) {
		return nil, false, nil
	}
	if b.malformed == nil {
		header = b.header
	}

	// Directly update tracker with the correct timestamp for this locally-fetched block
	// This happens BEFORE the OnValue callback is triggered, ensuring only the fetching node emits the metric
	e.tracker.SetLatestBlockNumber(e.upstream, b.number, b.timestamp)
	e.latestBlockShared.TryUpdate(ctx, b.number, f.sent)

	e.logger.Debug().
		Int64("blockNumber", b.number).
		Int64("blockTimestamp", b.timestamp).
		Msg("fetched latest block from upstream")
	return header, true, nil
}

// deliverFetched sends f's header to the bound intake: a fork poll's
// answer when a fork Ask waits on f, the poll source's stream while it is
// one, else a poll's. passed: the header passed intake's checks.
func (e *EvmStatePoller) deliverFetched(bound *pollSource, f *headFetch, b polledBlock) (passed bool) {
	kind := HeaderPolled
	if e.activePollSource() != nil {
		kind = HeaderStreamed
	}
	e.headAsk.mu.Lock()
	if f.fork {
		kind = HeaderForkPolled
	}
	e.headAsk.mu.Unlock()
	return bound.intake.Polled(e.upstream.Id(), b.header, b.malformed, kind, f.sent)
}

// backoffDelay is how long after a fetch's send the next one may start:
// the cadence, doubled per node failure in a row, at most AskMaxBackoff.
func backoffDelay(cadence time.Duration, failures int) time.Duration {
	if failures == 0 {
		return cadence
	}
	return max(cadence, min(cadence<<min(failures, 16), AskMaxBackoff.Load()))
}

// backoff is the upstream's backed-off cadence now (backoffDelay).
func (e *EvmStatePoller) backoff() time.Duration {
	cadence := e.pollCadence()
	e.headAsk.mu.Lock()
	defer e.headAsk.mu.Unlock()
	return backoffDelay(cadence, e.headAsk.failures)
}

// askDue is how long until the Ask may fetch again: its last fetch's send
// plus the backed-off cadence; 0 before any.
func (e *EvmStatePoller) askDue() time.Duration {
	cadence := e.pollCadence()
	a := &e.headAsk
	a.mu.Lock()
	f, n := a.last, a.failures
	a.mu.Unlock()
	if f == nil {
		return 0
	}
	return max(0, time.Until(f.sent.Add(backoffDelay(cadence, n))))
}

// recoverHeadPanic, deferred by a head fetch's goroutine, recovers a
// panic there (spec §6.8, #1060).
func (e *EvmStatePoller) recoverHeadPanic(where string) {
	if rec := recover(); rec != nil {
		e.countHeadPanic(where, rec)
	}
}

// countHeadPanic counts a recovered head fetch panic in
// erpc_unexpected_panic_total, scope head-ask.
func (e *EvmStatePoller) countHeadPanic(where string, rec any) {
	telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
		"head-ask",
		fmt.Sprintf("project:%s network:%s upstream:%s where:%s", e.projectId, e.netLabel(), e.upstream.Id(), where),
		common.ErrorFingerprint(rec),
	).Inc()
	e.logger.Error().Interface("panic", rec).Str("where", where).Str("stack", string(debug.Stack())).
		Msg("unexpected panic in a head fetch")
}

// netLabel is the network's metric label.
func (e *EvmStatePoller) netLabel() string {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.networkLabel
}

// nodeFailedPoll reports whether err is the node failing a poll of latest
// (spec §6.1): a transport error, a timeout, a 5xx or a 429. eRPC's own
// refusals (local rate limit, open breaker, skip) are no poll.
func nodeFailedPoll(err error) bool {
	return common.HasErrorCode(err,
		common.ErrCodeEndpointTransportFailure,
		common.ErrCodeEndpointRequestTimeout,
		common.ErrCodeFailsafeTimeoutExceeded,
		common.ErrCodeEndpointServerSideException,
		common.ErrCodeEndpointCapacityExceeded,
	)
}

// RefreshFinalizedBlock starts a background finalized-block poll when the
// stored one is older than the finalized poll's debounce and no refresh is
// already in flight; it never blocks.
func (e *EvmStatePoller) RefreshFinalizedBlock() {
	if e.IsObjectNull() || e.finalizedBlockShared == nil || e.givenUp(&e.finalizedGiveUp) {
		return
	}
	e.stateMu.RLock()
	cfg := e.cfg
	e.stateMu.RUnlock()
	if !e.finalizedBlockShared.IsStale(e.resolveFinalizedDebounce(cfg, e.resolveDebounce(cfg))) {
		return
	}
	if !e.finalizedRefreshInFlight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer e.finalizedRefreshInFlight.Store(false)
		defer e.recoverHeadPanic("finalized-refresh")
		ctx, cancel := context.WithTimeout(e.appCtx, 10*time.Second)
		defer cancel()
		if _, err := e.PollFinalizedBlockNumber(ctx); err != nil {
			e.logger.Debug().Err(err).Msg("on-demand finalized block refresh failed in evm state poller")
		}
	}()
}

// SuggestLatestBlock reports blockNumber as the upstream's latest, its
// evidence taken at at (spec §6.4): an answer's request send time, a
// push's receive time. A report of the counter's own value dates it anew.
func (e *EvmStatePoller) SuggestLatestBlock(blockNumber int64, at time.Time) {
	// Best-effort, non-blocking update.
	//
	// IMPORTANT:
	// - This must be fast and MUST NOT be blocked by any refresh/thundering-herd coordination.
	// - TryUpdate is local-only and schedules remote propagation asynchronously via the shared variable's
	//   deduped publish-first background push.
	currentValue := e.latestBlockShared.GetValue()
	if blockNumber < currentValue {
		e.logger.Trace().
			Int64("blockNumber", blockNumber).
			Int64("currentValue", currentValue).
			Msg("skipping latest block suggestion as it's older")
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
		e.verifyThenSuggestLatestBlock(blockNumber, at)
		return
	}

	newValue := e.latestBlockShared.TryUpdate(e.appCtx, blockNumber, at)
	e.logger.Trace().
		Int64("blockNumber", blockNumber).
		Int64("previousValue", currentValue).
		Int64("newValue", newValue).
		Msg("latest block suggestion applied")
}

// RewindLatestBlock is the rollback write (spec §14): the counter takes
// blockNumber, published with a rewind mark, and no misbehaviour is
// counted against the upstream.
func (e *EvmStatePoller) RewindLatestBlock(blockNumber int64) {
	if e.IsObjectNull() || e.latestBlockShared == nil {
		return
	}
	from := e.latestBlockShared.GetValue()
	if e.latestBlockShared.Rewind(blockNumber) {
		e.logger.Info().Int64("from", from).Int64("to", blockNumber).
			Msg("rewound the latest block counter to the upstream's hash vote on the tracker's new branch")
	}
}

// ObserveHead is the per-upstream part of intake for a source header's
// height n, its evidence taken at at: n goes through SuggestLatestBlock,
// so a major jump is verified before it moves the counter. accepted: the
// counter holds n now.
func (e *EvmStatePoller) ObserveHead(n int64, at time.Time) (accepted bool) {
	if n < 0 {
		return false
	}
	e.SuggestLatestBlock(n, at)
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

// PollIntake takes a bound poller's latest and finalized headers.
type PollIntake interface {
	// Polled takes the header a poll of latest, sent at sent, fetched;
	// malformed: why it failed intake check 0, h then unset. passed: h
	// passed intake's header checks (spec §5.3, §13).
	Polled(upstreamId string, h SourceHeader, malformed error, kind HeaderKind, sent time.Time) (passed bool)
	// PollFailed notes a poll of latest, sent at started, that the node
	// failed (spec §6.1).
	PollFailed(upstreamId string, started time.Time)
	// PolledFinalized takes the header a poll of finalized fetched.
	PolledFinalized(upstreamId string, h SourceHeader)
}

// BindIntake sends this poller's latest headers to intake; Set makes it a
// poll source, polled at its cadence.
func (e *EvmStatePoller) BindIntake(intake PollIntake) {
	e.pollSrc.Store(&pollSource{intake: intake})
}

// LatestUnsupported reports the give-up after latest was refused as
// unsupported; a retry follows PollerGiveUpExpiry after it began.
func (e *EvmStatePoller) LatestUnsupported() bool {
	return e.givenUp(&e.latestGiveUp)
}

// Set Asks with a hash need once per backed-off cadence while topics
// include newHeads, feeding each header to the bound intake; empty returns
// to statePollerInterval. It starts the poll loop when Bootstrap did not,
// as with statePollerInterval 0.
func (e *EvmStatePoller) Set(topics []indexer.Topic) {
	on := slices.Contains(topics, indexer.TopicNewHeads) && e.pollSrc.Load() != nil
	if e.pollHeads.Swap(on) == on || !on {
		return
	}
	e.headAsk.mu.Lock()
	e.headAsk.failures = 0
	e.headAsk.mu.Unlock()
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

// pollCadence is the Ask's cadence (spec §6.8): the poller's debounce
// (resolveDebounce: the learned block time ×
// dynamicBlockTimeDebounceMultiplier), at most pollSourceMaxCadence;
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
// source, until its Ask is due (askDue). false: nothing is due until a
// wake.
func (e *EvmStatePoller) nextPollDelay(interval time.Duration, lastFull time.Time) (d time.Duration, ok bool) {
	if interval > 0 {
		d, ok = max(interval-time.Since(lastFull), 0), true
	}
	if e.activePollSource() != nil {
		if p := e.askDue(); !ok || p < d {
			d, ok = p, true
		}
	}
	return d, ok
}

// BackedOffCadence is how often this upstream's latest head reaches this
// instance now, the head bench's cadence (spec §6.1, §6.8): while a poll
// source, the Ask's backoff; a wss subscription source pushes every head,
// so its cadence; else the full poll interval or the backoff, the longer.
func (e *EvmStatePoller) BackedOffCadence() time.Duration {
	backoff := e.backoff()
	if e.activePollSource() != nil {
		return backoff
	}
	c := e.upstream.Config()
	if c != nil && c.IsSubscriptionSource() && util.IsWsEndpoint(c.Endpoint) {
		return e.pollCadence()
	}
	interval := time.Duration(0)
	if c != nil && c.Evm != nil {
		interval = c.Evm.StatePollerInterval.Duration()
	}
	return max(interval, backoff)
}

// verifyThenSuggestLatestBlock validates a MAJOR suggested forward jump with a
// fresh chain-identity check and applies it to the shared counter only if it
// passes. A proven cross-wired endpoint is cordoned by the check itself; an
// unverifiable one (a transient eth_chainId failure) drops the suggestion for
// now and it is re-observed on the next suggestion or verified poll. Runs in
// its own goroutine so the caller (response enrichment) never blocks, and at
// most one verification is in flight per poller. at is the suggestion's
// evidence time.
func (e *EvmStatePoller) verifyThenSuggestLatestBlock(blockNumber int64, at time.Time) {
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
		newValue := e.latestBlockShared.TryUpdate(e.appCtx, blockNumber, at)
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

// LatestReportAt is when the upstream's latest block report's evidence was
// taken by this instance's clock (spec §6.4, §14): a poll's send time, else
// the receive time.
func (e *EvmStatePoller) LatestReportAt() time.Time {
	if e.latestBlockShared == nil {
		return time.Time{}
	}
	return e.latestBlockShared.ReportAt()
}

// OnFinalizedBlock is OnLatestBlock for the finalized block number.
func (e *EvmStatePoller) OnFinalizedBlock(cb func(int64)) {
	if e.finalizedBlockShared != nil {
		e.finalizedBlockShared.OnValue(cb)
	}
}

// OnChainIdCordonChanged registers cb to fire each time this poller sets or
// lifts its chain-identity cordon, which moves the upstream in or out of
// its lanes' ceiling. Like OnLatestBlock, cb MUST NOT block, and it cannot
// be unregistered.
func (e *EvmStatePoller) OnChainIdCordonChanged(cb func()) {
	e.cordonHooksMu.Lock()
	defer e.cordonHooksMu.Unlock()
	e.cordonHooks = append(e.cordonHooks, cb)
}

func (e *EvmStatePoller) chainIdCordonChanged() {
	e.cordonHooksMu.Lock()
	hooks := e.cordonHooks
	e.cordonHooksMu.Unlock()
	for _, cb := range hooks {
		cb()
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
	e.chainIdCordonChanged()
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
			e.chainIdCordonChanged()
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
		e.chainIdCordonChanged()
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
	if e.givenUp(&e.finalizedGiveUp) {
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
		b, err := e.fetchBlock(ctx, "finalized", common.UpstreamPoliciesTimeoutOnly)
		if err != nil || b.number == 0 {
			if refusedPoll(err) {
				e.refused(&e.finalizedGiveUp, "finalized", err)
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

		e.answered(&e.finalizedGiveUp)

		// Same chain-identity gate as the latest ratchet (see there).
		if !e.verifyChainIdOnMajorHeadMove(ctx, "finalized", e.finalizedBlockShared.GetValue(), b.number) {
			return 0, nil
		}

		e.logger.Debug().
			Int64("blockNumber", b.number).
			Msg("fetched finalized block")

		e.tracker.SetFinalizedBlockNumber(e.upstream, b.number)
		// The header is this instance's own evidence of what is final
		// (spec §6.5); another instance's poll of the shared counter is none.
		if bound := e.pollSrc.Load(); bound != nil && b.malformed == nil {
			bound.intake.PolledFinalized(e.upstream.Id(), b.header)
		}

		return b.number, nil
	})
}

// SuggestFinalizedBlock reports blockNumber as the upstream's finalized
// block, its evidence taken at at (a request's send time).
func (e *EvmStatePoller) SuggestFinalizedBlock(blockNumber int64, at time.Time) {
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

		e.finalizedBlockShared.TryUpdate(ctx, blockNumber, at)
		e.logger.Trace().
			Int64("blockNumber", blockNumber).
			Msg("finalized block suggestion applied")
	}()
}

// FinalizedBlock is the upstream's finalized tag. Only an upstream known to
// have no finalized tag (finalizedGiveUp given) reports latest −
// fallbackFinalityDepth instead: a tagged upstream reports 0 until its tag
// first answers, so a derived guess never enters the Head.Finalized ballot
// on a chain whose real finality is slower (#1079).
func (e *EvmStatePoller) FinalizedBlock() int64 {
	if fin := e.finalizedBlockShared.GetValue(); fin > 0 {
		return fin
	}
	if !e.finalizedTagless() {
		return 0
	}
	latest := e.latestBlockShared.GetValue()
	if latest <= 0 {
		return 0
	}
	if depth := e.fallbackFinalityDepth(); latest > depth {
		return latest - depth
	}
	return 0
}

// DerivedFinalized is FinalizedBlock while the upstream has no finalized
// tag, latest − the derived finality depth (spec §6.4); ok false while it
// has the tag.
func (e *EvmStatePoller) DerivedFinalized() (number int64, ok bool) {
	if !e.finalizedTagless() {
		return 0, false
	}
	return e.FinalizedBlock(), true
}

// finalizedTagless: finalized was given up and has not answered since,
// through its retries, so derived finality never flips off on a retry.
func (e *EvmStatePoller) finalizedTagless() bool {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.finalizedGiveUp.given()
}

// refusedPoll reports whether a poll's outcome is the method refused, not
// failed: an empty answer (no error), unsupported, skipped, ignored,
// missing data or a client error. It counts towards the give-up.
func refusedPoll(err error) bool {
	return err == nil ||
		common.HasErrorCode(err,
			common.ErrCodeUpstreamRequestSkipped,
			common.ErrCodeUpstreamMethodIgnored,
			common.ErrCodeEndpointUnsupported,
			common.ErrCodeEndpointMissingData,
		) ||
		common.IsClientError(err)
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

// fallbackFinalityMinAge is the minimum age, in chain time, a block must reach
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
// today. The age is counted at the network's block time floor, never above
// the EMA, so an estimate running long, as one does at a cold start, cannot
// make the depth shorter than fallbackFinalityMinAge (spec §6.4, §6.7). While
// the block time is unknown the cold block time stands in, as for every
// reader.
func (e *EvmStatePoller) fallbackFinalityDepth() int64 {
	e.stateMu.RLock()
	depth := int64(common.DefaultEvmFinalityDepth)
	if e.cfg != nil && e.cfg.FallbackFinalityDepth > 0 {
		depth = e.cfg.FallbackFinalityDepth
	}
	e.stateMu.RUnlock()

	blockTime := e.tracker.GetNetworkBlockTimeFloor(e.upstream.NetworkId())
	if blockTime <= 0 {
		blockTime = health.ColdBlockTime
	}
	if byAge := int64((fallbackFinalityMinAge + blockTime - 1) / blockTime); byAge > depth {
		depth = byAge
	}
	return depth
}

func (e *EvmStatePoller) IsBlockFinalized(blockNumber int64) (bool, error) {
	if e.latestBlockShared.GetValue() == 0 && e.finalizedBlockShared.GetValue() == 0 {
		return false, common.NewErrFinalizedBlockUnavailable(blockNumber)
	}
	return blockNumber <= e.FinalizedBlock(), nil
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
		SkipSyncingCheck: e.syncingGiveUp.given(),

		// Latest block detection status
		SkipLatestBlockCheck:      e.latestGiveUp.given(),
		LatestBlockFailureCount:   e.latestGiveUp.refusals,
		LatestBlockSuccessfulOnce: e.latestGiveUp.succeeded,

		// Finalized block detection status
		SkipFinalizedCheck:           e.finalizedGiveUp.given(),
		FinalizedBlockFailureCount:   e.finalizedGiveUp.refusals,
		FinalizedBlockSuccessfulOnce: e.finalizedGiveUp.succeeded,
	}
	e.stateMu.RUnlock()

	if diag.SkipSyncingCheck {
		diag.SyncingCheckError = "syncing check given up after consecutive refusals (method may not be supported); retried every " + PollerGiveUpExpiry.Load().String()
	}
	if diag.SkipLatestBlockCheck {
		diag.LatestBlockDetectionIssue = "latest block check given up after consecutive refusals (method may not be supported); retried every " + PollerGiveUpExpiry.Load().String()
	}
	if diag.SkipFinalizedCheck {
		diag.FinalizedBlockDetectionIssue = "finalized block check given up after consecutive refusals (method may not be supported); retried every " + PollerGiveUpExpiry.Load().String()
	}
	// Also reflect operator-configured skip in diagnostics.
	if upsCfg := e.upstream.Config(); upsCfg.Evm != nil && upsCfg.Evm.SkipSyncingCheck != nil && *upsCfg.Evm.SkipSyncingCheck {
		diag.SkipSyncingCheck = true
		diag.SyncingCheckError = "syncing check disabled by upstream config (evm.skipSyncingCheck)"
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

// giveUpRefusals is how many refusals before any success give a poller
// method up (spec §6.8).
const giveUpRefusals = 10

// giveUp is one poller method's give-up (latest, finalized, the syncing
// check; spec §6.8, §15.1): refused giveUpRefusals times before any
// success, the method is given up for PollerGiveUpExpiry, then retried;
// a refused retry gives it up again at once, an answer ends it for good.
// The poller's stateMu guards it.
type giveUp struct {
	refusals  int
	succeeded bool
	// at is when the method was last given up; zero while it never was
	// or once it answered.
	at time.Time
}

// holds reports whether the method is given up at now: no poll is sent.
func (g *giveUp) holds(now time.Time) bool {
	return !g.at.IsZero() && now.Sub(g.at) < PollerGiveUpExpiry.Load()
}

// given reports whether the method was given up and has not answered
// since, its retries included: what stands in for it (finality derived
// from latest) stays on.
func (g *giveUp) given() bool { return !g.at.IsZero() }

// givenUp reports whether g holds now.
func (e *EvmStatePoller) givenUp(g *giveUp) bool {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return g.holds(time.Now())
}

// refused counts a refusal of method against g; true when it gives the
// method up now.
func (e *EvmStatePoller) refused(g *giveUp, method string, err error) bool {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if g.succeeded {
		return false
	}
	g.refusals++
	if g.refusals < giveUpRefusals {
		e.logger.Debug().Err(err).Msgf("upstream refused %s in evm state poller %d times in a row, will retry", method, g.refusals)
		return false
	}
	g.at = time.Now()
	e.logger.Warn().Err(err).Dur("retryIn", PollerGiveUpExpiry.Load()).
		Msgf("upstream refused %s in evm state poller %d times with no success, giving it up", method, g.refusals)
	return true
}

// answered ends g for good: the method answered.
func (e *EvmStatePoller) answered(g *giveUp) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	g.refusals, g.succeeded, g.at = 0, true, time.Time{}
}

// polledBlock is a polled latest or finalized block, decoded once: its
// number and timestamp (0 when it carries none or an unparseable one) for
// the counters, and its header past intake check 0, or why it failed it.
type polledBlock struct {
	number, timestamp int64
	header            SourceHeader
	malformed         error
}

// fetchBlock fetches the block at blockTag, latest or finalized; policies
// are the probe's upstream policies. A zero number: the answer was empty.
func (e *EvmStatePoller) fetchBlock(ctx context.Context, blockTag string, policies common.UpstreamPolicies) (polledBlock, error) {
	pr := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByNumber","params":["%s",false]}`, util.RandomID(), blockTag),
	))
	pr.SetDirectives(&common.RequestDirectives{UpstreamPolicies: policies})
	resp, err := e.upstream.Forward(ctx, pr, true, false)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return polledBlock{}, err
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return polledBlock{}, err
	}
	if jrr == nil || jrr.Error != nil {
		return polledBlock{}, jrr.Error
	}

	if jrr.IsResultEmptyish(ctx) {
		return polledBlock{}, nil
	}

	// resp is released on return.
	raw := bytes.Clone(jrr.GetResultBytes())
	var h integrity.Header
	if err := common.SonicCfg.Unmarshal(raw, &h); err != nil || h.Number == "" {
		return polledBlock{}, &common.BaseError{
			Code:    "ErrEvmStatePoller",
			Message: "cannot get block number from block data",
			Details: map[string]interface{}{
				"blockTag": blockTag,
				"result":   string(raw),
			},
		}
	}
	b := polledBlock{}
	if b.number, err = common.HexToInt64(h.Number); err != nil {
		return polledBlock{}, err
	}
	if s := h.Timestamp; s != "" {
		if b.timestamp, err = parseBlockTimestamp(s); err != nil {
			b.timestamp = 0
			e.logger.Debug().Err(err).Msg("failed to parse block timestamp, continuing without it")
		}
	}
	if ch, err := checkChainHeader(raw, &h); err != nil {
		b.malformed = err
	} else {
		b.header = SourceHeader{ch}
	}
	return b, nil
}

func (e *EvmStatePoller) fetchSyncingState(ctx context.Context) (bool, error) {
	pr := common.NewProbeRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_syncing","params":[]}`, util.RandomID())))

	resp, err := e.upstream.Forward(ctx, pr, true, false)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
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
