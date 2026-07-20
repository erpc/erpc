package upstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

// UpstreamsRegistry owns the per-network upstream lists plus the per-upstream
// bootstrap pipeline. It is INPUT to the selection-policy engine; ordering
// decisions and scoring are owned by `internal/policy/` and are not stored
// here. Until the engine is wired in Phase 7, `GetSortedUpstreams` returns the
// raw registration order.
type UpstreamsRegistry struct {
	appCtx               context.Context
	prjId                string
	logger               *zerolog.Logger
	metricsTracker       *health.Tracker
	sharedStateRegistry  data.SharedStateRegistry
	clientRegistry       *clients.ClientRegistry
	vendorsRegistry      *thirdparty.VendorsRegistry
	providersRegistry    *thirdparty.ProvidersRegistry
	rateLimitersRegistry *RateLimitersRegistry
	upsCfg               []*common.UpstreamConfig
	initializer          *util.Initializer

	allUpstreams []*Upstream
	upstreamsMu  *sync.RWMutex
	networkMu    *sync.Map
	// map of network => upstreams
	networkUpstreams       map[string][]*Upstream
	networkShadowUpstreams map[string][]*Upstream
	networkUpstreamsAtomic sync.Map

	providerOnce sync.Map // networkId -> *sync.Once

	// pendingUpstreams holds Upstream instances created by bootstrap task
	// attempts that have not completed successfully yet, keyed by upstream id.
	// Retried or concurrent attempts MUST reuse the same instance: creating a
	// fresh one per attempt duplicates HTTP clients and — once an attempt
	// reaches Upstream.Bootstrap — state pollers, whose ticker goroutines can
	// only be stopped by the app context.
	pendingUpstreams sync.Map // upstreamId -> *Upstream

	onUpstreamRegistered func(ups *Upstream) error
}

type UpstreamsHealth struct {
	Upstreams []*Upstream `json:"upstreams"`
}

func NewUpstreamsRegistry(
	appCtx context.Context,
	logger *zerolog.Logger,
	prjId string,
	upsCfg []*common.UpstreamConfig,
	ssr data.SharedStateRegistry,
	rr *RateLimitersRegistry,
	vr *thirdparty.VendorsRegistry,
	pr *thirdparty.ProvidersRegistry,
	ppr *clients.ProxyPoolRegistry,
	mt *health.Tracker,
	onUpstreamRegistered func(*Upstream) error,
) *UpstreamsRegistry {
	lg := logger.With().Str("component", "upstreams").Logger()
	return &UpstreamsRegistry{
		appCtx:              appCtx,
		prjId:               prjId,
		logger:              logger,
		sharedStateRegistry: ssr,
		clientRegistry: clients.NewClientRegistry(
			logger,
			prjId,
			ppr,
			evm.NewJsonRpcErrorExtractor(),
		),
		rateLimitersRegistry:   rr,
		vendorsRegistry:        vr,
		providersRegistry:      pr,
		metricsTracker:         mt,
		upsCfg:                 upsCfg,
		networkUpstreams:       make(map[string][]*Upstream),
		networkShadowUpstreams: make(map[string][]*Upstream),
		upstreamsMu:            &sync.RWMutex{},
		networkMu:              &sync.Map{},
		initializer:            util.NewInitializer(appCtx, &lg, nil),
		onUpstreamRegistered:   onUpstreamRegistered,
	}
}

func (u *UpstreamsRegistry) Bootstrap(ctx context.Context) {
	// Fire-and-forget: register upstreams in background to avoid blocking service startup
	go func() {
		if err := u.registerUpstreams(u.appCtx, u.upsCfg...); err != nil {
			u.logger.Error().Err(err).Msg("failed to register upstreams in background")
		} else {
			u.logger.Info().Msg("upstreams registration completed")
		}
	}()
}

func (u *UpstreamsRegistry) NewUpstream(cfg *common.UpstreamConfig) (*Upstream, error) {
	// Warn about deprecated upstream-level eth_getLogs hard limits that are ignored now
	if cfg != nil && cfg.Evm != nil {
		if cfg.Evm.DeprecatedGetLogsMaxAllowedRange > 0 || cfg.Evm.DeprecatedGetLogsMaxAllowedAddresses > 0 || cfg.Evm.DeprecatedGetLogsMaxAllowedTopics > 0 {
			u.logger.Warn().
				Str("upstreamId", cfg.Id).
				Msg("deprecated upstream-level getLogs maxAllowed* configs detected; they are ignored. Configure limits at network-level evm.* instead")
		}
		if cfg.Evm.DeprecatedGetLogsSplitOnError != nil {
			u.logger.Warn().
				Str("upstreamId", cfg.Id).
				Msg("deprecated upstream-level getLogsSplitOnError detected; it is ignored. Configure at network-level evm.getLogsSplitOnError instead")
		}
	}
	return NewUpstream(
		u.appCtx,
		u.prjId,
		cfg,
		u.clientRegistry,
		u.rateLimitersRegistry,
		u.vendorsRegistry,
		u.logger,
		u.metricsTracker,
		u.sharedStateRegistry,
	)
}

func (u *UpstreamsRegistry) GetInitializer() *util.Initializer {
	return u.initializer
}

func (u *UpstreamsRegistry) getNetworkMutex(networkId string) *sync.RWMutex {
	mutex, _ := u.networkMu.LoadOrStore(networkId, &sync.RWMutex{})
	return mutex.(*sync.RWMutex)
}

func (u *UpstreamsRegistry) GetProvidersRegistry() *thirdparty.ProvidersRegistry {
	return u.providersRegistry
}

// SharedStateRegistry exposes the registry's shared-state backing store so
// that consumers (e.g., Network) can register their own counters/values for
// strict-monotonic coordination across pods. The registry is owned here
// because UpstreamsRegistry is constructed with it; surfacing it via an
// accessor is cheaper than threading it through Network's constructor.
func (u *UpstreamsRegistry) SharedStateRegistry() data.SharedStateRegistry {
	return u.sharedStateRegistry
}

func (u *UpstreamsRegistry) PrepareUpstreamsForNetwork(ctx context.Context, networkId string) error {
	networkMu := u.getNetworkMutex(networkId)
	networkMu.Lock()
	defer networkMu.Unlock()

	// 1) Static upstreams are expected to be registered via Bootstrap() already.

	// 2) Schedule provider-based upstream tasks once per network; the initializer's
	// auto-retry loop handles failures on appCtx so we don't leak goroutines on retries.
	onceVal, _ := u.providerOnce.LoadOrStore(networkId, &sync.Once{})
	onceVal.(*sync.Once).Do(func() {
		allProviders := u.providersRegistry.GetAllProviders()
		var tasks []*util.BootstrapTask
		for _, p := range allProviders {
			t := u.buildProviderBootstrapTask(p, networkId)
			tasks = append(tasks, t)
		}
		go func() {
			if err := u.initializer.ExecuteTasks(u.appCtx, tasks...); err != nil {
				u.logger.Error().
					Err(err).
					Str("networkId", networkId).
					Interface("status", u.initializer.Status()).
					Msg("failed to execute provider bootstrap tasks")
			} else {
				u.logger.Info().
					Str("networkId", networkId).
					Interface("status", u.initializer.Status()).
					Msg("provider bootstrap tasks executed successfully")
			}
		}()
	})

	// Require only one ready upstream to start handling requests.
	// Use adaptive timeout: default to 30s, but cap to the caller's context deadline if sooner.
	minReady := 1
	waitTimeout := 30 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		until := time.Until(deadline)
		if until < waitTimeout {
			if until <= 0 {
				// Ensure a small positive wait in case the deadline is already due
				until = 50 * time.Millisecond
			}
			waitTimeout = until
		}
	}

	// 3) Wait until either static upstreams or provider-based upstreams are ready, or timeout/context ends
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timeoutCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	for {
		select {
		case <-timeoutCtx.Done():
			err := timeoutCtx.Err()
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				// Timeout reached, check if we have any upstreams ready
				u.upstreamsMu.RLock()
				upstreamsCount := len(u.networkUpstreams[networkId])
				u.upstreamsMu.RUnlock()

				if upstreamsCount > 0 {
					u.logger.Info().
						Str("networkId", networkId).
						Int("upstreamsCount", upstreamsCount).
						Int("minReady", minReady).
						Dur("waitTimeout", waitTimeout).
						Msg("timeout reached but some upstreams are ready for network initialization")
					return nil
				}
				// Consider per-network and unknown upstream/provider activity
				summary := u.summarizeNetworkTasks(networkId)
				if summary.hasOngoing {
					return common.NewErrNetworkInitializing(u.prjId, networkId)
				}
				if summary.providersAllTerminal {
					// Grace period to allow in-flight upstream registrations to complete
					time.Sleep(100 * time.Millisecond)
					u.upstreamsMu.RLock()
					latest := len(u.networkUpstreams[networkId])
					u.upstreamsMu.RUnlock()
					if latest >= minReady {
						u.logger.Info().
							Str("networkId", networkId).
							Int("upstreamsCount", latest).
							Msg("upstreams became ready during grace period after providers terminal state")
						return nil
					}
					// Return a retryable error so the auto-retry loop can
					// re-attempt when upstreams may have recovered.
					return common.NewErrNetworkNotSupported(u.prjId, networkId)
				}
				// Default: initializing
				return common.NewErrNetworkInitializing(u.prjId, networkId)
			}
			return timeoutCtx.Err()
		case <-ticker.C:
			// Check how many upstreams are ready for this network
			u.upstreamsMu.RLock()
			upstreamsCount := len(u.networkUpstreams[networkId])
			u.upstreamsMu.RUnlock()

			if upstreamsCount < minReady {
				// Keep waiting if there is any ongoing per-network or unknown upstream work
				summary := u.summarizeNetworkTasks(networkId)
				if summary.hasOngoing {
					continue
				}
				if summary.providersAllTerminal {
					// Grace period to allow in-flight upstream registrations to complete
					time.Sleep(100 * time.Millisecond)
					u.upstreamsMu.RLock()
					latest := len(u.networkUpstreams[networkId])
					u.upstreamsMu.RUnlock()
					if latest >= minReady {
						u.logger.Info().
							Str("networkId", networkId).
							Int("upstreamsCount", latest).
							Msg("upstreams became ready during grace period after providers terminal state")
						return nil
					}
					// Return a retryable error so the auto-retry loop can
					// re-attempt when upstreams may have recovered.
					return common.NewErrNetworkNotSupported(u.prjId, networkId)
				}
				continue
			}

			u.logger.Info().
				Str("networkId", networkId).
				Int("upstreamsCount", upstreamsCount).
				Int("minReady", minReady).
				Msg("upstreams ready for network initialization")
			return nil
		}
	}
}

func (u *UpstreamsRegistry) GetNetworkShadowUpstreams(networkId string) []*Upstream {
	u.upstreamsMu.RLock()
	defer u.upstreamsMu.RUnlock()
	return u.networkShadowUpstreams[networkId]
}

// OverrideOrderForTest pins the registration order of `networkUpstreams[networkId]`.
// Test-only helper for fixtures that don't wire a policy.Engine; production
// code uses `policy.OverrideAllForTest` against the engine. If `ids` is
// empty, sorts the existing list by upstream id (matches legacy
// ReorderUpstreams convenience).
//
// Lives here (not in test files) because in-package test helpers can't be
// referenced from *_test.go in other packages.
func (u *UpstreamsRegistry) OverrideOrderForTest(networkId string, ids ...string) {
	u.upstreamsMu.Lock()
	defer u.upstreamsMu.Unlock()

	src, ok := u.networkUpstreams[networkId]
	if !ok || len(src) == 0 {
		return
	}
	byID := make(map[string]*Upstream, len(src))
	for _, up := range src {
		byID[up.Id()] = up
	}
	if len(ids) == 0 {
		ids = make([]string, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		// Deterministic ascending-id order.
		for i := 1; i < len(ids); i++ {
			for j := i; j > 0 && ids[j-1] > ids[j]; j-- {
				ids[j-1], ids[j] = ids[j], ids[j-1]
			}
		}
	}
	reordered := make([]*Upstream, 0, len(ids))
	for _, id := range ids {
		if up, ok := byID[id]; ok {
			reordered = append(reordered, up)
		}
	}
	u.networkUpstreams[networkId] = reordered
	cp := make([]*Upstream, len(reordered))
	copy(cp, reordered)
	u.networkUpstreamsAtomic.Store(networkId, cp)
}

func (u *UpstreamsRegistry) GetNetworkUpstreams(ctx context.Context, networkId string) []*Upstream {
	if ctx == nil {
		ctx = u.appCtx
	}
	_, span := common.StartDetailSpan(ctx, "UpstreamsRegistry.GetNetworkUpstreams")
	defer span.End()

	// Fast path: lock-free atomic snapshot
	if v, ok := u.networkUpstreamsAtomic.Load(networkId); ok {
		if arr, ok2 := v.([]*Upstream); ok2 {
			return arr
		}
	}

	// Fallback: read under lock and populate snapshot for future reads
	u.upstreamsMu.RLock()
	ups := u.networkUpstreams[networkId]
	if ups == nil {
		u.upstreamsMu.RUnlock()
		return nil
	}
	cp := make([]*Upstream, len(ups))
	copy(cp, ups)
	// Populate snapshot while still holding RLock to avoid stale overwrite after a writer runs
	u.networkUpstreamsAtomic.Store(networkId, cp)
	u.upstreamsMu.RUnlock()
	return cp
}

func (u *UpstreamsRegistry) GetAllUpstreams() []*Upstream {
	u.upstreamsMu.RLock()
	defer u.upstreamsMu.RUnlock()
	return u.allUpstreams
}

// GetSortedUpstreams returns the registered upstreams for `networkId`.
//
// Today this is a thin pass-through over `GetNetworkUpstreams` (raw
// registration order). Phase 7 replaces it with a call into the policy
// engine's per-(network, method) ordered list. The `method` argument is
// currently ignored.
func (u *UpstreamsRegistry) GetSortedUpstreams(ctx context.Context, networkId, method string) ([]common.Upstream, error) {
	_, span := common.StartDetailSpan(ctx, "UpstreamsRegistry.GetSortedUpstreams")
	defer span.End()

	upsList := u.GetNetworkUpstreams(ctx, networkId)
	if len(upsList) == 0 {
		return nil, common.NewErrNoUpstreamsFound(u.prjId, networkId)
	}
	return castToCommonUpstreams(upsList), nil
}

func (u *UpstreamsRegistry) RLockUpstreams() {
	u.upstreamsMu.RLock()
}

func (u *UpstreamsRegistry) RUnlockUpstreams() {
	u.upstreamsMu.RUnlock()
}

// RefreshUpstreamNetworkMethodScores is retained as a no-op until Phase 7
// retires all callers. The selection-policy engine owns scoring now.
func (u *UpstreamsRegistry) RefreshUpstreamNetworkMethodScores() error {
	return nil
}

func (u *UpstreamsRegistry) registerUpstreams(ctx context.Context, upsCfgs ...*common.UpstreamConfig) error {
	tasks := make([]*util.BootstrapTask, 0)
	for _, c := range upsCfgs {
		upsCfg := c
		tasks = append(tasks, u.buildUpstreamBootstrapTask(upsCfg))
	}
	return u.initializer.ExecuteTasks(ctx, tasks...)
}

func (u *UpstreamsRegistry) buildUpstreamBootstrapTask(upsCfg *common.UpstreamConfig) *util.BootstrapTask {
	// Deep copy to avoid race conditions when detectFeatures modifies the config
	cfg := upsCfg.Copy()
	// Name: network/<networkId>/upstream/<id> if chainId configured; else upstream/<id>
	taskName := fmt.Sprintf("upstream/%s", cfg.Id)
	if cfg.Evm != nil && cfg.Evm.ChainId > 0 {
		taskName = fmt.Sprintf("network/%s/upstream/%s", util.EvmNetworkId(cfg.Evm.ChainId), cfg.Id)
	}
	return util.NewBootstrapTask(
		taskName,
		func(ctx context.Context) error {
			_, span := common.StartDetailSpan(ctx, "UpstreamsRegistry.buildUpstreamBootstrapTask")
			defer span.End()

			u.logger.Debug().Str("upstreamId", cfg.Id).Msg("attempt to bootstrap upstream")

			u.upstreamsMu.RLock()
			var ups *Upstream
			for _, up := range u.allUpstreams {
				if up.Id() == cfg.Id {
					ups = up
					break
				}
			}
			u.upstreamsMu.RUnlock()

			if ups == nil {
				// Reuse the instance created by a previous failed attempt, if any,
				// instead of building a new one per retry.
				if v, ok := u.pendingUpstreams.Load(cfg.Id); ok {
					ups = v.(*Upstream)
				}
			}

			var err error
			if ups == nil {
				var created *Upstream
				// Copy the config per attempt: NewUpstream mutates it (vendor
				// detection), and concurrent attempts of this task would
				// otherwise race on the shared task-level copy.
				created, err = u.NewUpstream(cfg.Copy())
				if err != nil {
					return err
				}
				// LoadOrStore so concurrent attempts of this task converge on a
				// single instance; the loser is discarded before it starts any
				// background work.
				actual, _ := u.pendingUpstreams.LoadOrStore(cfg.Id, created)
				ups = actual.(*Upstream)
			}

			err = ups.Bootstrap(ctx)
			if err != nil {
				return err
			}
			u.doRegisterBootstrappedUpstream(ups)
			u.pendingUpstreams.Delete(cfg.Id)

			if u.onUpstreamRegistered != nil {
				// TODO Refactor the upstream<->network relationship to avoid circular dependency. Then we can remove this goroutine.
				// We need this now at the moment so that lazy-loaded networks and lazy-loaded upstreams (from Providers) can work together.
				go func() {
					err = u.onUpstreamRegistered(ups)
					if err != nil {
						u.logger.Error().Err(err).Str("upstreamId", cfg.Id).Msg("failed to call onUpstreamRegistered")
					}
				}()
			}

			u.logger.Debug().Str("upstreamId", cfg.Id).Msg("upstream bootstrap completed")
			return nil
		},
	)
}

func (u *UpstreamsRegistry) buildProviderBootstrapTask(
	provider *thirdparty.Provider,
	networkId string,
) *util.BootstrapTask {
	taskName := fmt.Sprintf("network/%s/provider/%s", networkId, provider.Id())
	return util.NewBootstrapTask(
		taskName,
		func(ctx context.Context) error {
			_, span := common.StartDetailSpan(ctx, "UpstreamsRegistry.buildProviderBootstrapTask")
			defer span.End()

			lg := u.logger.With().Str("provider", provider.Id()).Str("networkId", networkId).Logger()
			lg.Debug().Msg("attempting to create upstream(s) from provider")

			if ok, err := provider.SupportsNetwork(ctx, networkId); err == nil && !ok {
				lg.Debug().Msg("provider does not support network; skipping upstream creation")
				return nil
			} else if err != nil {
				return err
			}

			upsCfgs, err := provider.GenerateUpstreamConfigs(ctx, &lg, networkId)
			if err != nil {
				return err
			}
			if lg.GetLevel() <= zerolog.DebugLevel {
				lg.Debug().Interface("upstreams", upsCfgs).Msgf("created %d upstream(s) from provider", len(upsCfgs))
			} else {
				lg.Info().Msgf("registering %d upstream(s) from provider", len(upsCfgs))
			}
			return u.registerUpstreams(ctx, upsCfgs...)
		},
	)
}

// providerTasksCompletionAndFatal inspects tasks in the initializer and returns
// (allDone, anyFatal) for provider tasks associated with the given networkId.
// We infer membership by task name prefix: "provider/<id>/network/<networkId>".
type networkTaskSummary struct {
	providersAllTerminal bool
	hasOngoing           bool
}

// summarizeNetworkTasks computes provider completion/fatality and presence of any ongoing
// per-network or unknown upstream tasks in a single pass.
//
// Called from `PrepareUpstreamsForNetwork`'s 200ms bootstrap-wait ticker.
// Previously walked `Initializer.Status()` which materializes a
// `[]TaskStatus` (growslice + per-task struct alloc) every call —
// pprof showed this at ~10% CPU during the bootstrap-wait window.
// Now uses the allocation-free `RangeTaskStates` streaming API since
// we only need name + state.
func (u *UpstreamsRegistry) summarizeNetworkTasks(networkId string) networkTaskSummary {
	provPrefix := "network/" + networkId + "/provider/"
	upsPrefix := "network/" + networkId + "/upstream/"
	unknownUpsPrefix := "upstream/"

	providersAllTerminal := true
	hasOngoing := false

	u.initializer.RangeTaskStates(func(name string, state util.TaskState) bool {
		if strings.HasPrefix(name, provPrefix) {
			switch state {
			case util.TaskSucceeded, util.TaskFatal:
				// terminal
			case util.TaskPending, util.TaskRunning, util.TaskFailed, util.TaskTimedOut:
				providersAllTerminal = false
				hasOngoing = true
			default:
				providersAllTerminal = false
			}
			return true
		}
		if strings.HasPrefix(name, upsPrefix) || strings.HasPrefix(name, unknownUpsPrefix) {
			switch state {
			case util.TaskPending, util.TaskRunning, util.TaskFailed, util.TaskTimedOut:
				hasOngoing = true
			}
		}
		return true
	})
	return networkTaskSummary{
		providersAllTerminal: providersAllTerminal,
		hasOngoing:           hasOngoing,
	}
}

func (u *UpstreamsRegistry) doRegisterBootstrappedUpstream(ups *Upstream) {
	networkId := ups.NetworkId()
	cfg := ups.Config()

	u.upstreamsMu.Lock()
	defer u.upstreamsMu.Unlock()

	// A bootstrap task may be re-executed against an already-registered
	// upstream; never register the same instance twice.
	alreadyInAll := false
	for _, existing := range u.allUpstreams {
		if existing == ups {
			alreadyInAll = true
			break
		}
	}
	if !alreadyInAll {
		u.allUpstreams = append(u.allUpstreams, ups)
	}

	// Add to network upstreams map
	isShadow := ups.Config() != nil && ups.Config().Shadow != nil && ups.Config().Shadow.Enabled
	exists := false
	if isShadow {
		for _, existingUps := range u.networkShadowUpstreams[networkId] {
			if existingUps.Id() == cfg.Id {
				exists = true
				break
			}
		}
		if !exists {
			u.networkShadowUpstreams[networkId] = append(u.networkShadowUpstreams[networkId], ups)
		}
	} else {
		for _, existingUps := range u.networkUpstreams[networkId] {
			if existingUps.Id() == cfg.Id {
				exists = true
				break
			}
		}
		if !exists {
			u.networkUpstreams[networkId] = append(u.networkUpstreams[networkId], ups)
		}
	}

	// Refresh atomic snapshot for this network's upstreams
	if !isShadow {
		cp := make([]*Upstream, len(u.networkUpstreams[networkId]))
		copy(cp, u.networkUpstreams[networkId])
		u.networkUpstreamsAtomic.Store(networkId, cp)
	}

	u.logger.Debug().
		Str("upstreamId", cfg.Id).
		Str("networkId", networkId).
		Msg("upstream registered and initialized in registry")
}

func (u *UpstreamsRegistry) GetUpstreamsHealth() (*UpstreamsHealth, error) {
	u.upstreamsMu.RLock()
	defer u.upstreamsMu.RUnlock()
	return &UpstreamsHealth{Upstreams: u.allUpstreams}, nil
}

func (u *UpstreamsRegistry) GetMetricsTracker() *health.Tracker {
	return u.metricsTracker
}

func castToCommonUpstreams(upstreams []*Upstream) []common.Upstream {
	commonUpstreams := make([]common.Upstream, len(upstreams))
	for i, ups := range upstreams {
		commonUpstreams[i] = ups
	}
	return commonUpstreams
}
