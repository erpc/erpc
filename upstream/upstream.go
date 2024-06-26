package upstream

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/evm"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/vendors"
	"github.com/rs/zerolog"
)

type Upstream struct {
	Client  ClientInterface
	Metrics *UpstreamMetrics
	Logger  zerolog.Logger

	ProjectId  string
	NetworkIds []string
	Score      int

	config *common.UpstreamConfig
	vendor common.Vendor

	failsafePolicies     []failsafe.Policy[common.NormalizedResponse]
	failsafeExecutor     failsafe.Executor[common.NormalizedResponse]
	rateLimitersRegistry *RateLimitersRegistry
	methodCheckResults   map[string]bool
	methodCheckResultsMu sync.RWMutex
}

func NewUpstream(
	projectId string,
	cfg *common.UpstreamConfig,
	cr *ClientRegistry,
	rlr *RateLimitersRegistry,
	vr *vendors.VendorsRegistry,
	logger *zerolog.Logger,
) (*Upstream, error) {
	lg := logger.With().Str("upstream", cfg.Id).Logger()

	if cfg.Architecture == "" {
		cfg.Architecture = common.ArchitectureEvm
	}

	policies, err := CreateFailSafePolicies(ScopeUpstream, cfg.Id, cfg.Failsafe)
	if err != nil {
		return nil, err
	}

	vn := vr.LookupByUpstream(cfg)

	preparedUpstream := &Upstream{
		ProjectId: projectId,
		Logger:    lg,

		config:               cfg,
		vendor:               vn,
		failsafePolicies:     policies,
		failsafeExecutor:     failsafe.NewExecutor[common.NormalizedResponse](policies...),
		rateLimitersRegistry: rlr,
		methodCheckResults:   map[string]bool{},
	}

	if client, err := cr.GetOrCreateClient(preparedUpstream); err != nil {
		return nil, err
	} else {
		preparedUpstream.Client = client
	}

	err = preparedUpstream.resolveNetworkIds(context.Background())
	if err != nil {
		return nil, err
	}

	if len(preparedUpstream.NetworkIds) == 0 {
		return nil, common.NewErrUpstreamNetworkNotDetected(projectId, cfg.Id)
	}

	lg.Debug().Msgf("prepared upstream")

	return preparedUpstream, nil
}

func (u *Upstream) Config() *common.UpstreamConfig {
	return u.config
}

func (u *Upstream) Vendor() common.Vendor {
	return u.vendor
}

func (u *Upstream) prepareRequest(normalizedReq *NormalizedRequest) error {
	cfg := u.Config()
	switch cfg.Architecture {
	case common.ArchitectureEvm:
		if u.Client == nil {
			return common.NewErrJsonRpcException(
				0,
				common.JsonRpcErrorServerSideException,
				fmt.Sprintf("client not initialized for evm upstream: %s", cfg.Id),
				nil,
			)
		}

		if u.Client.GetType() == "HttpJsonRpcClient" {
			jsonRpcReq, err := normalizedReq.JsonRpcRequest()
			if err != nil {
				return common.NewErrJsonRpcException(
					0,
					common.JsonRpcErrorClientSideException,
					"failed to unmarshal jsonrpc request",
					err,
				)
			}
			err = evm.NormalizeHttpJsonRpc(jsonRpcReq)
			if err != nil {
				return common.NewErrJsonRpcException(
					0,
					common.JsonRpcErrorServerSideException,
					"failed to normalize jsonrpc request",
					err,
				)
			}

			if !u.shouldHandleMethod(jsonRpcReq.Method) {
				u.Logger.Debug().Str("method", jsonRpcReq.Method).Msg("method not allowed or ignored by upstread")
				return nil
			}

			return nil
		} else {
			return common.NewErrJsonRpcException(
				0,
				common.JsonRpcErrorServerSideException,
				fmt.Sprintf("unsupported evm client type: %s upstream: %s", u.Client.GetType(), cfg.Id),
				nil,
			)
		}
	default:
		return common.NewErrJsonRpcException(
			0,
			common.JsonRpcErrorServerSideException,
			fmt.Sprintf("unsupported architecture: %s for upstream: %s", cfg.Architecture, cfg.Id),
			nil,
		)
	}
}

// Forward is used during lifecycle of a proxied request, it uses writers and readers for better performance
func (u *Upstream) Forward(ctx context.Context, req *NormalizedRequest) (common.NormalizedResponse, error) {
	cfg := u.Config()
	clientType := u.Client.GetType()

	//
	// Apply rate limits
	//
	var limitersBucket *RateLimiterBucket
	if cfg.RateLimitBucket != "" {
		var errLimiters error
		limitersBucket, errLimiters = u.rateLimitersRegistry.GetBucket(cfg.RateLimitBucket)
		if errLimiters != nil {
			return nil, errLimiters
		}
	}

	method, err := req.Method()
	if err != nil {
		return nil, common.NewErrUpstreamRequest(err, cfg.Id, req)
	}

	lg := u.Logger.With().Str("method", method).Logger()

	if limitersBucket != nil {
		lg.Trace().Msgf("checking upstream-level rate limiters bucket: %s", cfg.RateLimitBucket)
		rules := limitersBucket.GetRulesByMethod(method)
		if len(rules) > 0 {
			for _, rule := range rules {
				if !(*rule.Limiter).TryAcquirePermit() {
					lg.Warn().Msgf("upstream-level rate limit '%v' exceeded", rule.Config)
					netId := "n/a"
					if req.Network() != nil {
						netId = req.Network().Id()
					}
					health.MetricUpstreamRequestLocalRateLimited.WithLabelValues(
						u.ProjectId,
						netId,
						cfg.Id,
						method,
					).Inc()
					return nil, common.NewErrUpstreamRateLimitRuleExceeded(
						cfg.Id,
						cfg.RateLimitBucket,
						fmt.Sprintf("%+v", rule.Config),
					)
				} else {
					lg.Debug().Object("rule", rule.Config).Msgf("upstream-level rate limit passed")
				}
			}
		}
	}

	//
	// Prepare and normalize the request object
	//
	req = req.Clone().(*NormalizedRequest)
	req.WithUpstream(u)
	err = u.prepareRequest(req)
	if err != nil {
		return nil, err
	}

	//
	// Send the request based on client type
	//
	switch clientType {
	case "HttpJsonRpcClient":
		jsonRpcClient, okClient := u.Client.(*HttpJsonRpcClient)
		if !okClient {
			return nil, common.NewErrUpstreamClientInitialization(
				fmt.Errorf("failed to cast client to HttpJsonRpcClient"),
				cfg.Id,
			)
		}

		tryForward := func(
			ctx context.Context,
		) (*NormalizedResponse, error) {
			resp, errCall := jsonRpcClient.SendRequest(ctx, req)
			lg.Debug().Err(errCall).Msgf("upstream call result received: %v", &resp)

			if errCall != nil {
				if !errors.Is(errCall, context.DeadlineExceeded) && !errors.Is(errCall, context.Canceled) {
					netId := "n/a"
					if req.Network() != nil {
						netId = req.Network().Id()
					}
					health.MetricUpstreamRequestErrors.WithLabelValues(
						u.ProjectId,
						netId,
						cfg.Id,
						method,
					).Inc()
				}
				return nil, common.NewErrUpstreamRequest(
					errCall,
					cfg.Id,
					req,
				)
			}

			return resp, nil
		}

		if u.failsafePolicies != nil && len(u.failsafePolicies) > 0 {
			resp, execErr := u.failsafeExecutor.
				WithContext(ctx).
				GetWithExecution(func(exec failsafe.Execution[common.NormalizedResponse]) (common.NormalizedResponse, error) {
					return tryForward(ctx)
				})

			if execErr != nil {
				return nil, TranslateFailsafeError(execErr)
			}

			return resp, execErr
		} else {
			return tryForward(ctx)
		}
	default:
		return nil, common.NewErrUpstreamClientInitialization(
			fmt.Errorf("unsupported client type: %s", clientType),
			cfg.Id,
		)
	}
}

func (u *Upstream) Executor() failsafe.Executor[common.NormalizedResponse] {
	return u.failsafeExecutor
}

func (u *Upstream) EvmGetChainId(ctx context.Context) (string, error) {
	pr := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[]}`))
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

	u.Logger.Debug().Msgf("eth_chainId response: %+v", jrr)

	hex, err := common.NormalizeHex(jrr.Result)
	if err != nil {
		return "", err
	}

	dec, err := common.HexToUint64(hex)
	if err != nil {
		return "", err
	}

	return strconv.FormatUint(dec, 10), nil
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

	if cfg.AllowMethods != nil {
		v = false
		for _, m := range cfg.AllowMethods {
			if common.WildcardMatch(m, method) {
				v = true
				break
			}
		}
	}

	if cfg.IgnoreMethods != nil {
		for _, m := range cfg.IgnoreMethods {
			if common.WildcardMatch(m, method) {
				v = false
				break
			}
		}
	}

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

func (u *Upstream) resolveNetworkIds(ctx context.Context) error {
	if u.Client == nil {
		return common.NewErrUpstreamClientInitialization(fmt.Errorf("client not initialized for upstream to resolve networkId"), u.Config().Id)
	}

	if u.NetworkIds == nil || len(u.NetworkIds) == 0 {
		u.NetworkIds = []string{}
	}

	cfg := u.Config()

	switch cfg.Architecture {
	case common.ArchitectureEvm:
		if cfg.Evm != nil && cfg.Evm.ChainId > 0 {
			u.NetworkIds = append(u.NetworkIds, fmt.Sprintf("eip155:%d", cfg.Evm.ChainId))
		} else {
			nid, err := u.EvmGetChainId(ctx)
			if err != nil {
				return err
			}
			u.NetworkIds = append(u.NetworkIds, fmt.Sprintf("eip155:%s", nid))
		}
	}

	// remove duplicates
	u.NetworkIds = common.RemoveDuplicates(u.NetworkIds)

	return nil
}
