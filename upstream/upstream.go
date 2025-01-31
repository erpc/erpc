package upstream

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/failsafe-go/failsafe-go"
	"github.com/rs/zerolog"
)

type Upstream struct {
	ProjectId string
	Client    clients.ClientInterface
	Logger    zerolog.Logger

	appCtx context.Context
	config *common.UpstreamConfig
	cfgMu  sync.RWMutex
	vendor common.Vendor

	networkId            string
	supportedMethods     sync.Map
	metricsTracker       *health.Tracker
	timeoutDuration      *time.Duration
	failsafeExecutor     failsafe.Executor[*common.NormalizedResponse]
	rateLimitersRegistry *RateLimitersRegistry
	rateLimiterAutoTuner *RateLimitAutoTuner
	evmStatePoller       common.EvmStatePoller
}

func NewUpstream(
	appCtx context.Context,
	projectId string,
	cfg *common.UpstreamConfig,
	cr *clients.ClientRegistry,
	rlr *RateLimitersRegistry,
	vr *thirdparty.VendorsRegistry,
	logger *zerolog.Logger,
	mt *health.Tracker,
) (*Upstream, error) {
	lg := logger.With().Str("upstreamId", cfg.Id).Logger()

	policiesMap, err := CreateFailSafePolicies(&lg, common.ScopeUpstream, cfg.Id, cfg.Failsafe)
	if err != nil {
		return nil, err
	}
	policiesArray := ToPolicyArray(policiesMap, "retry", "circuitBreaker", "hedge", "timeout")

	var timeoutDuration *time.Duration
	if cfg.Failsafe != nil && cfg.Failsafe.Timeout != nil {
		d, err := time.ParseDuration(cfg.Failsafe.Timeout.Duration)
		timeoutDuration = &d
		if err != nil {
			return nil, err
		}
	}

	vn := vr.LookupByUpstream(cfg)

	pup := &Upstream{
		ProjectId: projectId,
		Logger:    lg,

		appCtx:               appCtx,
		config:               cfg,
		vendor:               vn,
		metricsTracker:       mt,
		timeoutDuration:      timeoutDuration,
		failsafeExecutor:     failsafe.NewExecutor(policiesArray...),
		rateLimitersRegistry: rlr,
		supportedMethods:     sync.Map{},
	}

	pup.initRateLimitAutoTuner()

	if vn != nil {
		err = vn.PrepareConfig(cfg, nil)
		if err != nil {
			return nil, err
		}
	}

	if client, err := cr.GetOrCreateClient(appCtx, pup); err != nil {
		return nil, err
	} else {
		pup.Client = client
	}

	if pup.config.Type == common.UpstreamTypeEvm {
		pup.evmStatePoller = evm.NewEvmStatePoller(appCtx, &lg, pup, mt)
	}

	lg.Debug().Msgf("prepared upstream")

	return pup, nil
}

func (u *Upstream) Bootstrap(ctx context.Context) error {
	err := u.detectFeatures(ctx)
	if err != nil {
		return err
	}

	if u.evmStatePoller != nil {
		err = u.evmStatePoller.Bootstrap(ctx)
		if err != nil {
			// The reason we're not returning error is to allow upstream to still be registered
			// even if background block polling fails initially.
			u.Logger.Error().Err(err).Msg("failed on initial bootstrap of evm state poller (will retry in background)")
		}
	}

	return nil
}

func (u *Upstream) Config() *common.UpstreamConfig {
	return u.config
}

func (u *Upstream) Vendor() common.Vendor {
	return u.vendor
}

func (u *Upstream) prepareRequest(nr *common.NormalizedRequest) error {
	cfg := u.Config()
	switch cfg.Type {
	case common.UpstreamTypeEvm:
		// TODO Move the logic to evm package as a pre-request hook?
		if u.Client == nil {
			return common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorServerSideException,
				fmt.Sprintf("client not initialized for evm upstream: %s", cfg.Id),
				nil,
				nil,
			)
		}

		if u.Client.GetType() == clients.ClientTypeHttpJsonRpc {
			jsonRpcReq, err := nr.JsonRpcRequest()
			if err != nil {
				return common.NewErrJsonRpcExceptionInternal(
					0,
					common.JsonRpcErrorParseException,
					"failed to unmarshal json-rpc request",
					err,
					nil,
				)
			}
			evm.NormalizeHttpJsonRpc(nr, jsonRpcReq)
		} else {
			return common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorServerSideException,
				fmt.Sprintf("unsupported evm client type: %s upstream: %s", u.Client.GetType(), cfg.Id),
				nil,
				nil,
			)
		}
	default:
		return common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorServerSideException,
			fmt.Sprintf("unsupported architecture: %s for upstream: %s", cfg.Type, cfg.Id),
			nil,
			nil,
		)
	}

	return nil
}

func (u *Upstream) NetworkId() string {
	return u.networkId
}

func (u *Upstream) SetNetworkConfig(cfg *common.NetworkConfig) {
	// TODO Can we eliminate this circular dependency of upstream state poller <> network? e.g. by requiring network ID everywhere?
	if cfg != nil && cfg.Evm != nil && u.evmStatePoller != nil {
		u.evmStatePoller.SetNetworkConfig(cfg.Evm)
	}
}

func (u *Upstream) Forward(ctx context.Context, req *common.NormalizedRequest, byPassMethodExclusion bool) (*common.NormalizedResponse, error) {
	// TODO Should we move byPassMethodExclusion to directives? How do we prevent clients from setting it?
	startTime := time.Now()
	cfg := u.Config()

	if !byPassMethodExclusion {
		if reason, skip := u.shouldSkip(req); skip {
			return nil, common.NewErrUpstreamRequestSkipped(reason, cfg.Id)
		}
	}

	clientType := u.Client.GetType()

	//
	// Apply rate limits
	//
	var limitersBudget *RateLimiterBudget
	if cfg.RateLimitBudget != "" {
		var errLimiters error
		limitersBudget, errLimiters = u.rateLimitersRegistry.GetBudget(cfg.RateLimitBudget)
		if errLimiters != nil {
			return nil, errLimiters
		}
	}

	method, err := req.Method()
	if err != nil {
		return nil, common.NewErrUpstreamRequest(
			err,
			cfg.Id,
			u.networkId,
			method,
			time.Since(startTime),
			0,
			0,
			0,
		)
	}

	lg := u.Logger.With().Str("method", method).Str("networkId", u.networkId).Interface("id", req.ID()).Logger()

	if limitersBudget != nil {
		lg.Trace().Str("budget", cfg.RateLimitBudget).Msgf("checking upstream-level rate limiters budget")
		rules, err := limitersBudget.GetRulesByMethod(method)
		if err != nil {
			return nil, err
		}
		if len(rules) > 0 {
			for _, rule := range rules {
				if !rule.Limiter.TryAcquirePermit() {
					lg.Debug().Str("budget", cfg.RateLimitBudget).Msgf("upstream-level rate limit '%v' exceeded", rule.Config)
					u.metricsTracker.RecordUpstreamSelfRateLimited(
						u.networkId,
						cfg.Id,
						method,
					)
					return nil, common.NewErrUpstreamRateLimitRuleExceeded(
						cfg.Id,
						cfg.RateLimitBudget,
						fmt.Sprintf("%+v", rule.Config),
					)
				} else {
					lg.Trace().Str("budget", cfg.RateLimitBudget).Object("rule", rule.Config).Msgf("upstream-level rate limit passed")
				}
			}
		}
	}

	//
	// Prepare and normalize the request object
	//
	req.SetLastUpstream(u)
	err = u.prepareRequest(req)
	if err != nil {
		return nil, err
	}

	//
	// Send the request based on client type
	//
	switch clientType {
	case clients.ClientTypeHttpJsonRpc:
		jsonRpcClient, okClient := u.Client.(clients.HttpJsonRpcClient)
		if !okClient {
			return nil, common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorServerSideException,
				fmt.Sprintf("failed to initialize client for upstream %s", cfg.Id),
				common.NewErrUpstreamClientInitialization(
					fmt.Errorf("failed to cast client to HttpJsonRpcClient"),
					cfg.Id,
				),
				nil,
			)
		}

		tryForward := func(
			ctx context.Context,
			exec failsafe.Execution[*common.NormalizedResponse],
		) (*common.NormalizedResponse, error) {
			u.metricsTracker.RecordUpstreamRequest(
				cfg.Id,
				u.networkId,
				method,
			)
			health.MetricUpstreamRequestTotal.WithLabelValues(u.ProjectId, u.networkId, cfg.Id, method, strconv.Itoa(exec.Attempts())).Inc()
			timer := u.metricsTracker.RecordUpstreamDurationStart(cfg.Id, u.networkId, method)
			defer timer.ObserveDuration()

			resp, errCall := jsonRpcClient.SendRequest(ctx, req)
			if resp != nil {
				jrr, _ := resp.JsonRpcResponse()
				if jrr != nil && jrr.Error == nil {
					req.SetLastValidResponse(resp)
				}
				if lg.GetLevel() == zerolog.TraceLevel {
					lg.Debug().Err(errCall).Object("response", resp).Msgf("upstream request ended with response")
				} else {
					lg.Debug().Err(errCall).Msgf("upstream request ended with non-nil response")
				}
			} else {
				if errCall != nil {
					if lg.GetLevel() == zerolog.TraceLevel && errors.Is(errCall, context.Canceled) {
						lg.Trace().Err(errCall).Msgf("upstream request ended due to context cancellation")
					} else {
						lg.Debug().Err(errCall).Msgf("upstream request ended with error")
					}
				} else {
					lg.Warn().Msgf("upstream request ended with nil response and nil error")
				}
			}
			if errCall != nil {
				if common.HasErrorCode(errCall, common.ErrCodeUpstreamRequestSkipped) {
					health.MetricUpstreamSkippedTotal.WithLabelValues(u.ProjectId, cfg.Id, u.networkId, method).Inc()
				} else if common.HasErrorCode(errCall, common.ErrCodeEndpointMissingData) {
					health.MetricUpstreamMissingDataErrorTotal.WithLabelValues(u.ProjectId, cfg.Id, u.networkId, method).Inc()
				} else {
					if common.HasErrorCode(errCall, common.ErrCodeEndpointCapacityExceeded) {
						u.recordRemoteRateLimit(u.networkId, method)
					}
					severity := common.ClassifySeverity(errCall)
					if severity == common.SeverityCritical {
						// We only consider a subset of errors in metrics tracker (which is used for score calculation)
						// so that we only penalize upstreams for internal issues (not rate limits, or client-side, or method support issues, etc.)
						u.metricsTracker.RecordUpstreamFailure(
							cfg.Id,
							u.networkId,
							method,
						)
					}
					health.MetricUpstreamErrorTotal.WithLabelValues(u.ProjectId, u.networkId, cfg.Id, method, common.ErrorSummary(errCall), string(severity)).Inc()
				}

				if exec != nil {
					return nil, common.NewErrUpstreamRequest(
						errCall,
						cfg.Id,
						u.networkId,
						method,
						time.Since(startTime),
						exec.Attempts(),
						exec.Retries(),
						exec.Hedges(),
					)
				} else {
					return nil, common.NewErrUpstreamRequest(
						errCall,
						cfg.Id,
						u.networkId,
						method,
						time.Since(startTime),
						1,
						0,
						0,
					)
				}
			} else {
				if resp.IsResultEmptyish() {
					health.MetricUpstreamEmptyResponseTotal.WithLabelValues(u.ProjectId, u.networkId, cfg.Id, method).Inc()
				}
			}

			u.recordRequestSuccess(method)

			return resp, nil
		}

		executor := u.failsafeExecutor
		resp, execErr := executor.
			WithContext(ctx).
			GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
				ectx := exec.Context()
				if ctxErr := ectx.Err(); ctxErr != nil {
					cause := context.Cause(ectx)
					if cause != nil {
						return nil, cause
					} else {
						return nil, ctxErr
					}
				}
				if u.timeoutDuration != nil {
					var cancelFn context.CancelFunc
					ectx, cancelFn = context.WithTimeout(
						ectx,
						// TODO Carrying the timeout helps setting correct timeout on actual http request to upstream (during batch mode).
						//      Is there a way to do this cleanly? e.g. if failsafe lib works via context rather than Ticker?
						//      5ms is a workaround to ensure context carries the timeout deadline (used when calling upstreams),
						//      but allow the failsafe execution to fail with timeout first for proper error handling.
						*u.timeoutDuration+5*time.Millisecond,
					)
					defer cancelFn()
				}

				return tryForward(ectx, exec)
			})

		if _, ok := execErr.(common.StandardError); !ok {
			if ctxErr := ctx.Err(); ctxErr != nil {
				cause := context.Cause(ctx)
				if cause != nil {
					execErr = cause
				} else {
					execErr = ctxErr
				}
			}
		}

		if execErr != nil {
			return nil, TranslateFailsafeError(common.ScopeUpstream, u.config.Id, method, execErr, &startTime)
		}

		return resp, nil
	default:
		return nil, common.NewErrUpstreamClientInitialization(
			fmt.Errorf("unsupported client type during forward: %s", clientType),
			cfg.Id,
		)
	}
}

func (u *Upstream) Executor() failsafe.Executor[*common.NormalizedResponse] {
	// TODO extend this to per-network and/or per-method because of either upstream performance diff
	// or if user wants diff policies (retry/cb/integrity) per network/method.
	return u.failsafeExecutor
}

// TODO move to evm package
func (u *Upstream) EvmGetChainId(ctx context.Context) (string, error) {
	pr := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":75412,"method":"eth_chainId","params":[]}`))

	resp, err := u.Forward(ctx, pr, true)
	if err != nil {
		return "", err
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return "", err
	}
	if jrr.Error != nil {
		return "", jrr.Error
	}
	var chainId string
	err = common.SonicCfg.Unmarshal(jrr.Result, &chainId)
	if err != nil {
		return "", err
	}
	hex, err := common.NormalizeHex(chainId)
	if err != nil {
		return "", err
	}
	dec, err := common.HexToUint64(hex)
	if err != nil {
		return "", err
	}

	return strconv.FormatUint(dec, 10), nil
}

// TODO move to evm package
func (u *Upstream) EvmIsBlockFinalized(blockNumber int64) (bool, error) {
	if u.evmStatePoller == nil {
		return false, fmt.Errorf("evm state poller not initialized yet")
	}
	return u.evmStatePoller.IsBlockFinalized(blockNumber)
}

// TODO move to evm package?
func (u *Upstream) EvmSyncingState() common.EvmSyncingState {
	if u.evmStatePoller == nil {
		return common.EvmSyncingStateUnknown
	}
	return u.evmStatePoller.SyncingState()
}

// TODO move to evm package?
func (u *Upstream) EvmLatestBlock() (int64, error) {
	if u.evmStatePoller == nil {
		return 0, fmt.Errorf("evm state poller not initialized yet")
	}
	return u.evmStatePoller.LatestBlock(), nil
}

// TODO move to evm package?
func (u *Upstream) EvmFinalizedBlock() (int64, error) {
	if u.evmStatePoller == nil {
		return 0, fmt.Errorf("evm state poller not initialized yet")
	}
	return u.evmStatePoller.FinalizedBlock(), nil
}

// TODO move to evm package?
func (u *Upstream) EvmStatePoller() common.EvmStatePoller {
	return u.evmStatePoller
}

func (u *Upstream) IgnoreMethod(method string) {
	ai := u.config.AutoIgnoreUnsupportedMethods
	if ai == nil || !*ai {
		return
	}

	u.cfgMu.Lock()
	u.config.IgnoreMethods = append(u.config.IgnoreMethods, method)
	u.cfgMu.Unlock()
	u.supportedMethods.Store(method, false)
}

// Add this method to the Upstream struct
func (u *Upstream) initRateLimitAutoTuner() {
	if u.config.RateLimitBudget != "" && u.config.RateLimitAutoTune != nil {
		cfg := u.config.RateLimitAutoTune
		if cfg.Enabled != nil && *cfg.Enabled {
			budget, err := u.rateLimitersRegistry.GetBudget(u.config.RateLimitBudget)
			if err == nil {
				dur, err := time.ParseDuration(cfg.AdjustmentPeriod)
				if err != nil {
					u.Logger.Error().Err(err).Msgf("failed to parse rate limit auto-tune adjustment period: %s", cfg.AdjustmentPeriod)
					return
				}
				u.rateLimiterAutoTuner = NewRateLimitAutoTuner(
					&u.Logger,
					budget,
					dur,
					cfg.ErrorRateThreshold,
					cfg.IncreaseFactor,
					cfg.DecreaseFactor,
					cfg.MinBudget,
					cfg.MaxBudget,
				)
			}
		}
	}
}

func (u *Upstream) recordRequestSuccess(method string) {
	if u.rateLimiterAutoTuner != nil {
		u.rateLimiterAutoTuner.RecordSuccess(method)
	}
}

func (u *Upstream) recordRemoteRateLimit(netId, method string) {
	u.metricsTracker.RecordUpstreamRemoteRateLimited(
		u.config.Id,
		netId,
		method,
	)

	if u.rateLimiterAutoTuner != nil {
		u.rateLimiterAutoTuner.RecordError(method)
	}
}

func (u *Upstream) shouldHandleMethod(method string) (v bool, err error) {
	cfg := u.Config()
	if s, ok := u.supportedMethods.Load(method); ok {
		return s.(bool), nil
	}

	v = true

	// First check if method is ignored, and then check if it is explicitly mentioned to be allowed.
	// This order allows an upstream for example to define "ignore all except eth_getLogs".

	u.cfgMu.RLock()
	defer u.cfgMu.RUnlock()

	if cfg.IgnoreMethods != nil {
		for _, m := range cfg.IgnoreMethods {
			match, err := common.WildcardMatch(m, method)
			if err != nil {
				return false, err
			}
			if match {
				v = false
				break
			}
		}
	}

	if cfg.AllowMethods != nil {
		for _, m := range cfg.AllowMethods {
			match, err := common.WildcardMatch(m, method)
			if err != nil {
				return false, err
			}
			if match {
				v = true
				break
			}
		}
	}

	u.supportedMethods.Store(method, v)
	u.Logger.Debug().Bool("allowed", v).Str("method", method).Msg("method support result")

	return v, nil
}

func (u *Upstream) detectFeatures(ctx context.Context) error {
	cfg := u.Config()

	if cfg.Type == "" {
		return fmt.Errorf("upstream type not set yet")
	}

	if cfg.Type == common.UpstreamTypeEvm {
		// TODO Move the logic to evm package as a upstream-init hook?
		if cfg.Evm == nil {
			cfg.Evm = &common.EvmUpstreamConfig{}
		}
		if cfg.Evm.ChainId == 0 {
			nid, err := u.EvmGetChainId(ctx)
			if err != nil {
				return common.NewErrUpstreamClientInitialization(
					fmt.Errorf("failed to get chain id: %w", err),
					cfg.Id,
				)
			}
			cfg.Evm.ChainId, err = strconv.ParseInt(nid, 10, 64)
			if err != nil {
				return common.NewErrUpstreamClientInitialization(
					fmt.Errorf("failed to parse chain id: %w", err),
					cfg.Id,
				)
			}
		}
		u.networkId = util.EvmNetworkId(cfg.Evm.ChainId)

		if cfg.Evm.MaxAvailableRecentBlocks == 0 && cfg.Evm.NodeType == common.EvmNodeTypeFull {
			cfg.Evm.MaxAvailableRecentBlocks = 128
		}

		// TODO evm: check trace methods availability (by engine? erigon/geth/etc)
		// TODO evm: detect max eth_getLogs max block range
	} else {
		return fmt.Errorf("upstream type not supported: %s", cfg.Type)
	}

	return nil
}

func (u *Upstream) shouldSkip(req *common.NormalizedRequest) (reason error, skip bool) {
	method, _ := req.Method()

	if u.config.Evm != nil {
		if u.EvmSyncingState() == common.EvmSyncingStateSyncing {
			return common.NewErrUpstreamSyncing(u.config.Id), true
		}
	}

	allowed, err := u.shouldHandleMethod(method)
	if err != nil {
		return err, true
	}
	if !allowed {
		u.Logger.Debug().Str("method", method).Msg("method not allowed or ignored by upstread")
		return common.NewErrUpstreamMethodIgnored(method, u.config.Id), true
	}

	dirs := req.Directives()
	if dirs.UseUpstream != "" {
		match, err := common.WildcardMatch(dirs.UseUpstream, u.config.Id)
		if err != nil {
			return err, true
		}
		if !match {
			return common.NewErrUpstreamNotAllowed(u.config.Id), true
		}
	}

	// if block can be determined from request and upstream is only full-node and block is historical skip
	if u.config.Evm != nil && u.config.Evm.MaxAvailableRecentBlocks > 0 {
		if u.config.Evm.NodeType == common.EvmNodeTypeFull {
			_, bn, ebn := evm.ExtractBlockReferenceFromRequest(req)
			if ebn != nil || bn <= 0 {
				return nil, false
			}

			if lb := u.evmStatePoller.LatestBlock(); lb > 0 && bn < lb-u.config.Evm.MaxAvailableRecentBlocks {
				// Allow requests for block numbers greater than the latest known block
				if bn > lb {
					return nil, false
				}
				return common.NewErrUpstreamNodeTypeMismatch(fmt.Errorf("block number (%d) in request will not yield result for a fullNodeType upstream since it is not recent enough (must be >= %d", bn, lb-u.config.Evm.MaxAvailableRecentBlocks), common.EvmNodeTypeArchive, common.EvmNodeTypeFull), true
			}
		}
	}

	return nil, false
}

func (u *Upstream) getScoreMultipliers(networkId, method string) *common.ScoreMultiplierConfig {
	if u.config.Routing != nil {
		for _, mul := range u.config.Routing.ScoreMultipliers {
			matchNet, err := common.WildcardMatch(mul.Network, networkId)
			if err != nil {
				continue
			}
			matchMeth, err := common.WildcardMatch(mul.Method, method)
			if err != nil {
				continue
			}
			if matchNet && matchMeth {
				return mul
			}
		}
	}

	return common.DefaultScoreMultiplier
}

func (u *Upstream) MarshalJSON() ([]byte, error) {
	type upstreamPublic struct {
		Id        string                            `json:"id"`
		Metrics   map[string]*health.TrackedMetrics `json:"metrics"`
		NetworkId string                            `json:"networkId"`
		// ActiveNetworks []string                          `json:"activeNetworks"`
	}

	// var activeNetworks []string
	// u.supportedNetworkIdsMu.RLock()
	// for netId := range u.supportedNetworkIds {
	// 	activeNetworks = append(activeNetworks, netId)
	// }
	// u.supportedNetworkIdsMu.RUnlock()

	metrics := u.metricsTracker.GetUpstreamMetrics(u.config.Id)

	uppub := upstreamPublic{
		Id:        u.config.Id,
		Metrics:   metrics,
		NetworkId: u.NetworkId(),
		// ActiveNetworks: activeNetworks,
	}

	return sonic.Marshal(uppub)
}
