package upstream

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/resiliency"
	"github.com/rs/zerolog"
)

type PreparedUpstream struct {
	Id               string            `json:"id"`
	Architecture     string            `json:"architecture"`
	Endpoint         string            `json:"endpoint"`
	RateLimitBucket  string            `json:"rateLimitBucket"`
	HealthCheckGroup string            `json:"healthCheckGroup"`
	Metadata         map[string]string `json:"metadata"`
	AllowMethods     []string          `json:"allowMethods"`
	IgnoreMethods    []string          `json:"ignoreMethods"`

	ProjectId        string                                        `json:"projectId"`
	NetworkIds       []string                                      `json:"networkIds"`
	Logger           zerolog.Logger                                `json:"-"`
	FailsafePolicies []failsafe.Policy[*common.NormalizedResponse] `json:"-"`

	Client  ClientInterface  `json:"-"`
	Metrics *UpstreamMetrics `json:"metrics"`
	Score   int              `json:"score"`

	methodCheckResults   map[string]bool                               `json:"-"`
	methodCheckResultsMu sync.RWMutex                                  `json:"-"`
	rateLimitersRegistry *resiliency.RateLimitersRegistry              `json:"-"`
	failsafeExecutor     failsafe.Executor[*common.NormalizedResponse] `json:"-"`
}

func NewUpstream(
	projectId string,
	cfg *config.UpstreamConfig,
	cr *ClientRegistry,
	rlr *resiliency.RateLimitersRegistry,
	logger *zerolog.Logger,
) (*PreparedUpstream, error) {
	var networkIds []string = []string{}

	lg := logger.With().Str("upstream", cfg.Id).Logger()

	if cfg.Metadata != nil {
		if val, ok := cfg.Metadata["evmChainId"]; ok {
			lg.Debug().Str("network", val).Msgf("network ID set to %s via evmChainId", val)
			networkIds = append(networkIds, fmt.Sprintf("eip155:%s", val))
		} else {
			lg.Debug().Msgf("network ID not set via metadata.evmChainId: %v", cfg.Metadata["evmChainId"])
		}
	}

	if cfg.Architecture == "" {
		cfg.Architecture = ArchitectureEvm
	}

	// TODO create a Client for upstream and try to "detect" the network ID(s)
	// if networkIds == nil || len(networkIds) == 0 {
	// }

	if len(networkIds) == 0 {
		return nil, common.NewErrUpstreamNetworkNotDetected(projectId, cfg.Id)
	}

	policies, err := resiliency.CreateFailSafePolicies(cfg.Id, cfg.Failsafe)
	if err != nil {
		return nil, err
	}

	preparedUpstream := &PreparedUpstream{
		Id:               cfg.Id,
		Architecture:     cfg.Architecture,
		Endpoint:         cfg.Endpoint,
		Metadata:         cfg.Metadata,
		RateLimitBucket:  cfg.RateLimitBucket,
		HealthCheckGroup: cfg.HealthCheckGroup,
		FailsafePolicies: policies,
		AllowMethods:     cfg.AllowMethods,
		IgnoreMethods:    cfg.IgnoreMethods,

		ProjectId:  projectId,
		NetworkIds: networkIds,
		Logger:     lg,

		rateLimitersRegistry: rlr,
		failsafeExecutor:     failsafe.NewExecutor[*common.NormalizedResponse](policies...),
		methodCheckResults:   map[string]bool{},
	}

	lg.Debug().Msgf("prepared upstream")

	if client, err := cr.GetOrCreateClient(preparedUpstream); err != nil {
		return nil, err
	} else {
		preparedUpstream.Client = client
	}

	return preparedUpstream, nil
}

func (u *PreparedUpstream) PrepareRequest(normalizedReq *common.NormalizedRequest) (interface{}, error) {
	switch u.Architecture {
	case ArchitectureEvm:
		if u.Client == nil {
			return nil, common.NewErrJsonRpcRequestPreparation(fmt.Errorf("client not initialized for evm upstream"), map[string]interface{}{
				"upstreamId": u.Id,
			})
		}

		if u.Client.GetType() == "HttpJsonRpcClient" {
			jsonRpcReq, err := normalizedReq.JsonRpcRequest()
			normalizeEvmHttpJsonRpc(jsonRpcReq)
			if err != nil {
				return nil, err
			}

			if !u.shouldHandleMethod(jsonRpcReq.Method) {
				u.Logger.Debug().Str("method", jsonRpcReq.Method).Msg("method not allowed or ignored by upstread")
				return nil, nil
			}

			return jsonRpcReq, nil
		} else {
			return nil, common.NewErrJsonRpcRequestPreparation(fmt.Errorf("unsupported evm client type for upstream"), map[string]interface{}{
				"upstreamId": u.Id,
				"clientType": u.Client.GetType(),
			})
		}
	default:
		return nil, common.NewErrJsonRpcRequestPreparation(fmt.Errorf("unsupported architecture for upstream"), map[string]interface{}{
			"upstreamId":   u.Id,
			"architecture": u.Architecture,
		})
	}
}

// Forward is used during lifecycle of a proxied request, it uses writers and readers for better performance
func (u *PreparedUpstream) Forward(ctx context.Context, networkId string, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	clientType := u.Client.GetType()

	var limitersBucket *resiliency.RateLimiterBucket
	if u.RateLimitBucket != "" {
		var errLimiters error
		limitersBucket, errLimiters = u.rateLimitersRegistry.GetBucket(u.RateLimitBucket)
		if errLimiters != nil {
			return nil, errLimiters
		}
	}

	method, err := req.Method()
	if err != nil {
		return nil, common.NewErrUpstreamRequest(err, u.Id)
	}

	lg := u.Logger.With().Str("method", method).Logger()

	if limitersBucket != nil {
		lg.Trace().Msgf("checking upstream-level rate limiters bucket: %s", u.RateLimitBucket)
		rules := limitersBucket.GetRulesByMethod(method)
		if len(rules) > 0 {
			for _, rule := range rules {
				if !(*rule.Limiter).TryAcquirePermit() {
					lg.Warn().Msgf("upstream-level rate limit '%v' exceeded", rule.Config)
					health.MetricUpstreamRequestLocalRateLimited.WithLabelValues(
						u.ProjectId,
						networkId,
						u.Id,
						method,
					).Inc()
					return nil, common.NewErrUpstreamRateLimitRuleExceeded(
						u.Id,
						u.RateLimitBucket,
						rule.Config,
					)
				} else {
					lg.Debug().Object("rule", rule.Config).Msgf("upstream-level rate limit passed")
				}
			}
		}
	}

	switch clientType {
	case "HttpJsonRpcClient":
		jsonRpcClient, okClient := u.Client.(*HttpJsonRpcClient)
		jrReq, err := req.JsonRpcRequest()
		if err != nil {
			return nil, common.NewErrUpstreamRequest(err, u.Id)
		}
		if !okClient {
			return nil, common.NewErrUpstreamClientInitialization(
				fmt.Errorf("failed to cast client to HttpJsonRpcClient"),
				u.Id,
			)
		}

		tryForward := func(
			ctx context.Context,
		) (*common.NormalizedResponse, error) {
			resp, errCall := jsonRpcClient.SendRequest(ctx, jrReq)
			lg.Debug().Err(errCall).Msgf("upstream call result received: %v", &resp)

			if errCall != nil {
				if !errors.Is(errCall, context.DeadlineExceeded) && !errors.Is(errCall, context.Canceled) {
					health.MetricUpstreamRequestErrors.WithLabelValues(
						u.ProjectId,
						networkId,
						u.Id,
						method,
					).Inc()
				}
				return nil, common.NewErrUpstreamRequest(
					errCall,
					u.Id,
				)
			}

			return resp, nil
		}

		if u.FailsafePolicies != nil && len(u.FailsafePolicies) > 0 {
			resp, execErr := u.failsafeExecutor.
				WithContext(ctx).
				GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
					return tryForward(ctx)
				})

			if execErr != nil {
				return nil, resiliency.TranslateFailsafeError(execErr)
			}

			return resp, execErr
		} else {
			return tryForward(ctx)
		}
	default:
		return nil, common.NewErrUpstreamClientInitialization(
			fmt.Errorf("unsupported client type: %s", clientType),
			u.Id,
		)
	}
}

// Send will call the upstream and parses the response, used in internal non-proxy flows such as block tracker
// func (u *PreparedUpstream) Send(ctx context.Context, networkId string, req interface{}) (interface{}, error) {
// 	clientType := u.Client.GetType()

// 	switch clientType {
// 	case "HttpJsonRpcClient":
// 		rw := common.NewMemoryResponseWriter()
// 		crw := common.NewHttpCompositeResponseWriter(rw)

// 		err := u.Forward(ctx, networkId, req, crw)
// 		if err != nil {
// 			return nil, err
// 		}

// 		respBody, err := io.ReadAll(rw)
// 		if err != nil {
// 			return nil, common.NewErrUpstreamRequest(err, u.Id)
// 		}

// 		result := &common.JsonRpcResponse{}
// 		err = json.Unmarshal(respBody, result)
// 		if err != nil {
// 			return nil, err
// 		}

// 		return result, nil
// 	default:
// 		return nil, common.NewErrUpstreamClientInitialization(
// 			fmt.Errorf("unsupported client type: %s", clientType),
// 			u.Id,
// 		)
// 	}
// }

func (n *PreparedUpstream) Executor() failsafe.Executor[*common.NormalizedResponse] {
	return n.failsafeExecutor
}

func (u *PreparedUpstream) shouldHandleMethod(method string) (v bool) {
	u.methodCheckResultsMu.RLock()
	if s, ok := u.methodCheckResults[method]; ok {
		u.methodCheckResultsMu.RUnlock()
		return s
	}
	u.methodCheckResultsMu.RUnlock()

	v = true

	if u.AllowMethods != nil {
		v = false
		for _, m := range u.AllowMethods {
			if common.WildcardMatch(m, method) {
				v = true
				break
			}
		}
	}

	if u.IgnoreMethods != nil {
		for _, m := range u.IgnoreMethods {
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

func normalizeEvmHttpJsonRpc(r *common.JsonRpcRequest) error {
	switch r.Method {
	case "eth_getBlockByNumber",
		"eth_getUncleByBlockNumberAndIndex",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getUncleCountByBlockNumber",
		"eth_getBlockTransactionCountByNumber":
		if len(r.Params) > 0 {
			b, err := common.NormalizeHex(r.Params[0])
			if err != nil {
				return err
			}
			r.Params[0] = b
		}
	case "eth_getBalance",
		"eth_getStorageAt",
		"eth_getCode",
		"eth_getTransactionCount",
		"eth_call",
		"eth_estimateGas":
		if len(r.Params) > 1 {
			b, err := common.NormalizeHex(r.Params[1])
			if err != nil {
				return err
			}
			r.Params[1] = b
		}
	case "eth_getLogs":
		if len(r.Params) > 0 {
			if paramsMap, ok := r.Params[0].(map[string]interface{}); ok {
				if fromBlock, ok := paramsMap["fromBlock"]; ok {
					b, err := common.NormalizeHex(fromBlock)
					if err != nil {
						return err
					}
					paramsMap["fromBlock"] = b
				}
				if toBlock, ok := paramsMap["toBlock"]; ok {
					b, err := common.NormalizeHex(toBlock)
					if err != nil {
						return err
					}
					paramsMap["toBlock"] = b
				}
			}
		}
	}

	return nil
}
