package upstream

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/util"
	"github.com/erpc/erpc/vendors"
	"github.com/failsafe-go/failsafe-go"
	"github.com/rs/zerolog"
)

type Upstream struct {
	ProjectId string
	Client    ClientInterface
	Logger    zerolog.Logger

	appCtx context.Context
	config *common.UpstreamConfig
	vendor common.Vendor

	metricsTracker       *health.Tracker
	failsafePolicies     []failsafe.Policy[*common.NormalizedResponse]
	failsafeExecutor     failsafe.Executor[*common.NormalizedResponse]
	rateLimitersRegistry *RateLimitersRegistry
	rateLimiterAutoTuner *RateLimitAutoTuner

	methodCheckResults    map[string]bool
	methodCheckResultsMu  sync.RWMutex
	supportedNetworkIds   map[string]bool
	supportedNetworkIdsMu sync.RWMutex
}

func NewUpstream(
	appCtx context.Context,
	projectId string,
	cfg *common.UpstreamConfig,
	cr *ClientRegistry,
	rlr *RateLimitersRegistry,
	vr *vendors.VendorsRegistry,
	logger *zerolog.Logger,
	mt *health.Tracker,
) (*Upstream, error) {
	lg := logger.With().Str("upstreamId", cfg.Id).Logger()

	policies, err := CreateFailSafePolicies(&lg, common.ScopeUpstream, cfg.Id, cfg.Failsafe)
	if err != nil {
		return nil, err
	}

	vn := vr.LookupByUpstream(cfg)

	pup := &Upstream{
		ProjectId: projectId,
		Logger:    lg,

		appCtx:               appCtx,
		config:               cfg,
		vendor:               vn,
		metricsTracker:       mt,
		failsafePolicies:     policies,
		failsafeExecutor:     failsafe.NewExecutor[*common.NormalizedResponse](policies...),
		rateLimitersRegistry: rlr,
		methodCheckResults:   map[string]bool{},
		supportedNetworkIds:  map[string]bool{},
	}

	pup.initRateLimitAutoTuner()

	if vn != nil {
		err = vn.OverrideConfig(cfg)
		if err != nil {
			return nil, err
		}
	}

	err = pup.guessUpstreamType()
	if err != nil {
		return nil, err
	}
	if client, err := cr.GetOrCreateClient(appCtx, pup); err != nil {
		return nil, err
	} else {
		pup.Client = client
	}

	go func() {
		err = pup.detectFeatures()
		if err != nil {
			lg.Error().Err(err).Msgf("could not fully detect features for upstream")
		}
	}()

	lg.Debug().Msgf("prepared upstream")

	return pup, nil
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
	case common.UpstreamTypeEvm,
		common.UpstreamTypeEvmAlchemy,
		common.UpstreamTypeEvmDrpc,
		common.UpstreamTypeEvmBlastapi,
		common.UpstreamTypeEvmThirdweb,
		common.UpstreamTypeEvmEnvio,
		common.UpstreamTypeEvmEtherspot,
		common.UpstreamTypeEvmInfura,
		common.UpstreamTypeEvmPimlico:
		if u.Client == nil {
			return common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorServerSideException,
				fmt.Sprintf("client not initialized for evm upstream: %s", cfg.Id),
				nil,
				nil,
			)
		}

		if u.Client.GetType() == ClientTypeHttpJsonRpc ||
			u.Client.GetType() == ClientTypeAlchemyHttpJsonRpc ||
			u.Client.GetType() == ClientTypeDrpcHttpJsonRpc ||
			u.Client.GetType() == ClientTypeBlastapiHttpJsonRpc ||
			u.Client.GetType() == ClientTypeThirdwebHttpJsonRpc ||
			u.Client.GetType() == ClientTypeEnvioHttpJsonRpc ||
			u.Client.GetType() == ClientTypePimlicoHttpJsonRpc ||
			u.Client.GetType() == ClientTypeEtherspotHttpJsonRpc ||
			u.Client.GetType() == ClientTypeInfuraHttpJsonRpc {
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
			common.NormalizeEvmHttpJsonRpc(nr, jsonRpcReq)
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

// Forward is used during lifecycle of a proxied request, it uses writers and readers for better performance
func (u *Upstream) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	startTime := time.Now()
	cfg := u.Config()

	if reason, skip := u.shouldSkip(req); skip {
		return nil, common.NewErrUpstreamRequestSkipped(reason, cfg.Id)
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

	netId := req.NetworkId()
	method, err := req.Method()
	if err != nil {
		return nil, common.NewErrUpstreamRequest(
			err,
			cfg.Id,
			netId,
			method,
			time.Since(startTime),
			0,
			0,
			0,
		)
	}

	lg := u.Logger.With().Str("method", method).Str("networkId", netId).Interface("id", req.ID()).Logger()

	if limitersBudget != nil {
		lg.Trace().Str("budget", cfg.RateLimitBudget).Msgf("checking upstream-level rate limiters budget")
		rules := limitersBudget.GetRulesByMethod(method)
		if len(rules) > 0 {
			for _, rule := range rules {
				if !rule.Limiter.TryAcquirePermit() {
					lg.Warn().Str("budget", cfg.RateLimitBudget).Msgf("upstream-level rate limit '%v' exceeded", rule.Config)
					u.metricsTracker.RecordUpstreamSelfRateLimited(
						netId,
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
	case ClientTypeHttpJsonRpc,
		ClientTypeAlchemyHttpJsonRpc,
		ClientTypeDrpcHttpJsonRpc,
		ClientTypeBlastapiHttpJsonRpc,
		ClientTypeThirdwebHttpJsonRpc,
		ClientTypeEnvioHttpJsonRpc,
		ClientTypeEtherspotHttpJsonRpc,
		ClientTypeInfuraHttpJsonRpc,
		ClientTypePimlicoHttpJsonRpc:
		jsonRpcClient, okClient := u.Client.(HttpJsonRpcClient)
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
				netId,
				method,
			)
			timer := u.metricsTracker.RecordUpstreamDurationStart(cfg.Id, netId, method)
			defer timer.ObserveDuration()
			resp, errCall := jsonRpcClient.SendRequest(ctx, req)
			if resp != nil {
				jrr, _ := resp.JsonRpcResponse()
				if jrr != nil && jrr.Error == nil {
					req.SetLastValidResponse(resp)
				}
				if lg.GetLevel() == zerolog.TraceLevel {
					lg.Debug().Err(errCall).Object("response", resp).Msgf("upstream result received (traced)")
				} else {
					lg.Debug().Err(errCall).Msgf("upstream result received (non-nil response)")
				}
			} else {
				if errCall != nil {
					lg.Debug().Err(errCall).Msgf("upstream result received (errored)")
				} else {
					lg.Debug().Msgf("upstream result received (nil response)")
				}
			}
			if errCall != nil {
				if !errors.Is(errCall, context.Canceled) {
					if common.HasErrorCode(errCall, common.ErrCodeEndpointCapacityExceeded) {
						u.recordRemoteRateLimit(netId, method)
					} else if common.HasErrorCode(errCall, common.ErrCodeUpstreamRequestSkipped) {
						health.MetricUpstreamSkippedTotal.WithLabelValues(u.ProjectId, cfg.Id, netId, method).Inc()
					} else if common.HasErrorCode(errCall, common.ErrCodeEndpointMissingData) {
						health.MetricUpstreamMissingDataErrorTotal.WithLabelValues(u.ProjectId, cfg.Id, netId, method).Inc()
					} else if !common.HasErrorCode(errCall, common.ErrCodeEndpointClientSideException) {
						u.metricsTracker.RecordUpstreamFailure(
							cfg.Id,
							netId,
							method,
							common.ErrorSummary(errCall),
						)
					}
				}

				if exec != nil {
					return nil, common.NewErrUpstreamRequest(
						errCall,
						cfg.Id,
						netId,
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
						netId,
						method,
						time.Since(startTime),
						1,
						0,
						0,
					)
				}
			} else {
				if resp.IsResultEmptyish() {
					health.MetricUpstreamEmptyResponseTotal.WithLabelValues(u.ProjectId, netId, cfg.Id, method).Inc()
				}
			}

			u.recordRequestSuccess(method)

			return resp, nil
		}

		if u.failsafePolicies != nil && len(u.failsafePolicies) > 0 {
			executor := u.failsafeExecutor
			resp, execErr := executor.
				WithContext(ctx).
				GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
					if err := exec.Context().Err(); err != nil {
						return nil, err
					}
					return tryForward(exec.Context(), exec)
				})

			if execErr != nil {
				return nil, TranslateFailsafeError(common.ScopeUpstream, u.config.Id, method, execErr, &startTime)
			}

			return resp, nil
		} else {
			return tryForward(ctx, nil)
		}
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

func (u *Upstream) EvmGetChainId(ctx context.Context) (string, error) {
	pr := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":75412,"method":"eth_chainId","params":[]}`))

	resp, err := u.Forward(ctx, pr)
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

func (u *Upstream) SupportsNetwork(networkId string) (bool, error) {
	u.supportedNetworkIdsMu.RLock()
	supports, exists := u.supportedNetworkIds[networkId]
	u.supportedNetworkIdsMu.RUnlock()
	if exists && supports {
		return true, nil
	}

	supports, err := u.Client.SupportsNetwork(networkId)
	if err != nil {
		return false, err
	}

	u.supportedNetworkIdsMu.Lock()
	u.supportedNetworkIds[networkId] = supports
	u.supportedNetworkIdsMu.Unlock()

	return supports, nil
}

func (u *Upstream) IgnoreMethod(method string) {
	ai := u.config.AutoIgnoreUnsupportedMethods
	if ai == nil || !*ai {
		return
	}

	u.methodCheckResultsMu.Lock()
	// TODO make this per-network vs global because some upstreams support multiple networks (e.g. alchemy://)
	u.config.IgnoreMethods = append(u.config.IgnoreMethods, method)
	if u.methodCheckResults == nil {
		u.methodCheckResults = map[string]bool{}
	}
	delete(u.methodCheckResults, method)
	u.methodCheckResultsMu.Unlock()
}

// Add this method to the Upstream struct
func (u *Upstream) initRateLimitAutoTuner() {
	if u.config.RateLimitBudget != "" && u.config.RateLimitAutoTune != nil {
		cfg := u.config.RateLimitAutoTune
		if cfg.Enabled {
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

func (u *Upstream) shouldHandleMethod(method string) (v bool) {
	cfg := u.Config()
	u.methodCheckResultsMu.RLock()
	if s, ok := u.methodCheckResults[method]; ok {
		u.methodCheckResultsMu.RUnlock()
		return s
	}
	u.methodCheckResultsMu.RUnlock()

	v = true

	// First check if method is ignored, and then check if it is explicitly mentioned to be allowed.
	// This order allows an upstream for example to define "ignore all except eth_getLogs".

	if cfg.IgnoreMethods != nil {
		for _, m := range cfg.IgnoreMethods {
			if common.WildcardMatch(m, method) {
				v = false
				break
			}
		}
	}

	if cfg.AllowMethods != nil {
		for _, m := range cfg.AllowMethods {
			if common.WildcardMatch(m, method) {
				v = true
				break
			}
		}
	}

	// TODO if method is one of exclusiveMethods by another upstream (e.g. alchemy_*) skip

	// Cache the result
	u.methodCheckResultsMu.Lock()
	if u.methodCheckResults == nil {
		u.methodCheckResults = map[string]bool{}
	}
	u.methodCheckResults[method] = v
	u.methodCheckResultsMu.Unlock()

	u.Logger.Debug().Bool("allowed", v).Str("method", method).Msg("method support result")

	return v
}

func (u *Upstream) guessUpstreamType() error {
	cfg := u.Config()

	if cfg.Type != "" {
		return nil
	}

	if strings.HasPrefix(cfg.Endpoint, "alchemy://") || strings.HasPrefix(cfg.Endpoint, "evm+alchemy://") {
		cfg.Type = common.UpstreamTypeEvmAlchemy
		return nil
	}
	if strings.HasPrefix(cfg.Endpoint, "drpc://") || strings.HasPrefix(cfg.Endpoint, "evm+drpc://") {
		cfg.Type = common.UpstreamTypeEvmDrpc
		return nil
	}
	if strings.HasPrefix(cfg.Endpoint, "blastapi://") || strings.HasPrefix(cfg.Endpoint, "evm+blastapi://") {
		cfg.Type = common.UpstreamTypeEvmBlastapi
		return nil
	}
	if strings.HasPrefix(cfg.Endpoint, "thirdweb://") || strings.HasPrefix(cfg.Endpoint, "evm+thirdweb://") {
		cfg.Type = common.UpstreamTypeEvmThirdweb
		return nil
	}
	if strings.HasPrefix(cfg.Endpoint, "envio://") || strings.HasPrefix(cfg.Endpoint, "evm+envio://") {
		cfg.Type = common.UpstreamTypeEvmEnvio
		return nil
	}
	if strings.HasPrefix(cfg.Endpoint, "pimlico://") || strings.HasPrefix(cfg.Endpoint, "evm+pimlico://") {
		cfg.Type = common.UpstreamTypeEvmPimlico
		return nil
	}
	if strings.HasPrefix(cfg.Endpoint, "etherspot://") || strings.HasPrefix(cfg.Endpoint, "evm+etherspot://") {
		cfg.Type = common.UpstreamTypeEvmEtherspot
		return nil
	}
	if strings.HasPrefix(cfg.Endpoint, "infura://") || strings.HasPrefix(cfg.Endpoint, "evm+infura://") {
		cfg.Type = common.UpstreamTypeEvmInfura
		return nil
	}

	// TODO make actual calls to detect other types (solana, btc, etc)
	cfg.Type = common.UpstreamTypeEvm
	return nil
}

func (u *Upstream) detectFeatures() error {
	cfg := u.Config()

	if cfg.Type == "" {
		return fmt.Errorf("upstream type not set yet")
	}

	if cfg.Type == common.UpstreamTypeEvm {
		if cfg.Evm == nil {
			cfg.Evm = &common.EvmUpstreamConfig{}
		}
		if cfg.Evm.ChainId == 0 {
			nid, err := u.EvmGetChainId(u.appCtx)
			if err != nil {
				return common.NewErrUpstreamClientInitialization(
					fmt.Errorf("failed to get chain id: %w", err),
					cfg.Id,
				)
			}
			cfg.Evm.ChainId, err = strconv.Atoi(nid)
			if err != nil {
				return common.NewErrUpstreamClientInitialization(
					fmt.Errorf("failed to parse chain id: %w", err),
					cfg.Id,
				)
			}
			u.supportedNetworkIdsMu.Lock()
			u.supportedNetworkIds[util.EvmNetworkId(cfg.Evm.ChainId)] = true
			u.supportedNetworkIdsMu.Unlock()
		}

		if cfg.Evm.NodeType == "" {
			nodeType, err := u.detectNodeType()
			if err != nil {
				return err
			}
			cfg.Evm.NodeType = common.EvmNodeType(nodeType)
		}

		if cfg.Evm.MaxAvailableRecentBlocks == 0 && cfg.Evm.NodeType == "full" {
			cfg.Evm.MaxAvailableRecentBlocks = 128
		}

		// TODO evm: check trace methods availability (by engine? erigon/geth/etc)
		// TODO evm: detect max eth_getLogs max block range
	}

	return nil
}

func (u *Upstream) shouldSkip(req *common.NormalizedRequest) (reason error, skip bool) {
	method, _ := req.Method()

	if u.config.Evm != nil {
		if u.config.Evm.Syncing != nil && *u.config.Evm.Syncing {
			return common.NewErrUpstreamSyncing(u.config.Id), true
		}
	}

	if !u.shouldHandleMethod(method) {
		u.Logger.Debug().Str("method", method).Msg("method not allowed or ignored by upstread")
		return common.NewErrUpstreamMethodIgnored(method, u.config.Id), true
	}

	dirs := req.Directives()
	if dirs.UseUpstream != "" {
		if !common.WildcardMatch(dirs.UseUpstream, u.config.Id) {
			return common.NewErrUpstreamNotAllowed(u.config.Id), true
		}
	}

	// TODO evm: if block can be determined from request and upstream is only full-node and block is historical skip
	jrReq, err := req.JsonRpcRequest()
	if err != nil {
		return fmt.Errorf("failed to get json-rpc request: %w", err), false
	}

	// sample of methods with block param
	methodsWithBlockParam := map[string]int{
		"eth_getBalance":          1,
		"eth_getCode":             1,
		"eth_getTransactionCount": 1,
		"eth_call":                1,
	}

	if index, exists := methodsWithBlockParam[jrReq.Method]; exists {
		if len(jrReq.Params) > index {
			// TODO: do we need to check if blockNumber is a valid hex?
			_, ok := jrReq.Params[index].(string)

			if ok {
				nodeType := u.config.Evm.NodeType
				if nodeType == "" {
					nodeType, err = u.detectNodeType()
					if err != nil {
						return fmt.Errorf("failed to detect node type: %w", err), false
					}
				}

				if nodeType == common.EvmNodeTypeFull {
					lb := req.Network().EvmStatePollerOf(u.Config().Id).LatestBlock()

					bn, ebn := req.EvmBlockNumber()
					if ebn != nil {
						return fmt.Errorf("failed to get request block number: %w", ebn), false
					}

					if bn < lb-int64(u.config.Evm.MaxAvailableRecentBlocks) {
						return fmt.Errorf("block number %d is out of range (must be >= %d)", bn, lb-int64(u.config.Evm.MaxAvailableRecentBlocks)), true
					}
				}
			}
		}
	}

	return nil, false
}

func isMissingHistoricalStateError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := err.Error()
	// Check for common error messages indicating missing historical state
	return strings.Contains(errMsg, "missing trie node") ||
		strings.Contains(errMsg, "required historical state unavailable") ||
		strings.Contains(errMsg, "historical state unavailable") ||
		strings.Contains(errMsg, "state unavailable") ||
		strings.Contains(errMsg, "not found")
}

func (u *Upstream) detectNodeType() (common.EvmNodeType, error) {
	// Step 1: Attempt to get balance at block 0x1
	balanceReqBlock1 := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x1", false]}`,
	)))

	_, errBlock1 := u.Forward(context.Background(), balanceReqBlock1)

	if errBlock1 == nil {
		// No error, node is an archive node
		return "archive", nil
	} else if common.EvmIsMissingDataError(errBlock1) {
		// Error due to missing historical state, proceed to check latest block
	} else {
		return "", fmt.Errorf("error getting balance at block 0x1: %w", errBlock1)
	}

	// Step 2: Attempt to get balance at the latest block
	balanceReqLatest := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNUmber","params":["latest", false]}`,
	)))

	_, errLatest := u.Forward(context.Background(), balanceReqLatest)

	if errLatest == nil {
		// No error, node is a full node
		return "full", nil
	} else {
		return "", fmt.Errorf("error getting balance at latest block: %w", errLatest)
	}
}

func (u *Upstream) getScoreMultipliers(networkId, method string) *common.ScoreMultiplierConfig {
	if u.config.Routing != nil {
		for _, mul := range u.config.Routing.ScoreMultipliers {
			if common.WildcardMatch(mul.Network, networkId) && common.WildcardMatch(mul.Method, method) {
				return mul
			}
		}
	}

	return common.DefaultScoreMultiplier
}

func (u *Upstream) MarshalJSON() ([]byte, error) {
	type upstreamPublic struct {
		Id             string                            `json:"id"`
		Metrics        map[string]*health.TrackedMetrics `json:"metrics"`
		ActiveNetworks []string                          `json:"activeNetworks"`
	}

	var activeNetworks []string
	u.supportedNetworkIdsMu.RLock()
	for netId := range u.supportedNetworkIds {
		activeNetworks = append(activeNetworks, netId)
	}
	u.supportedNetworkIdsMu.RUnlock()

	metrics := u.metricsTracker.GetUpstreamMetrics(u.config.Id)

	uppub := upstreamPublic{
		Id:             u.config.Id,
		Metrics:        metrics,
		ActiveNetworks: activeNetworks,
	}

	return sonic.Marshal(uppub)
}
