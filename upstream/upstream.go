package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

	ProjectId        string                 `json:"projectId"`
	NetworkIds       []string               `json:"networkIds"`
	Logger           zerolog.Logger         `json:"-"`
	FailsafePolicies []failsafe.Policy[any] `json:"-"`

	Client  ClientInterface  `json:"-"`
	Metrics *UpstreamMetrics `json:"metrics"`
	Score   float64          `json:"score"`

	rateLimitersRegistry *resiliency.RateLimitersRegistry `json:"-"`
	failsafeExecutor     failsafe.Executor[interface{}]   `json:"-"`
}

type UpstreamMetrics struct {
	P90Latency     float64   `json:"p90Latency"`
	ErrorsTotal    float64   `json:"errorsTotal"`
	ThrottledTotal float64   `json:"throttledTotal"`
	RequestsTotal  float64   `json:"requestsTotal"`
	BlocksLag      float64   `json:"blocksLag"`
	LastCollect    time.Time `json:"lastCollect"`
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
			networkIds = append(networkIds, val)
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

		ProjectId:  projectId,
		NetworkIds: networkIds,
		Logger:     lg,

		rateLimitersRegistry: rlr,
		failsafeExecutor:     failsafe.NewExecutor[interface{}](policies...),
	}

	lg.Debug().Msgf("prepared upstream")

	if client, err := cr.GetOrCreateClient(preparedUpstream); err != nil {
		return nil, err
	} else {
		preparedUpstream.Client = client
	}

	return preparedUpstream, nil
}

func (c *UpstreamMetrics) MarshalZerologObject(e *zerolog.Event) {
	e.Float64("p90Latency", c.P90Latency).
		Float64("errorsTotal", c.ErrorsTotal).
		Float64("requestsTotal", c.RequestsTotal).
		Float64("throttledTotal", c.ThrottledTotal).
		Float64("blocksLag", c.BlocksLag).
		Time("lastCollect", c.LastCollect)
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
			// TODO check supported/unsupported methods for this upstream
			return normalizedReq.JsonRpcRequest()
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

func (u *PreparedUpstream) Forward(ctx context.Context, networkId string, req interface{}, w common.ResponseWriter) error {
	clientType := u.Client.GetType()

	switch clientType {
	case "HttpJsonRpcClient":
		jsonRpcClient, okClient := u.Client.(*HttpJsonRpcClient)
		if !okClient {
			return common.NewErrUpstreamClientInitialization(
				fmt.Errorf("failed to cast client to HttpJsonRpcClient"),
				u.Id,
			)
		}

		var limitersBucket *resiliency.RateLimiterBucket
		if u.RateLimitBucket != "" {
			var errLimiters error
			limitersBucket, errLimiters = u.rateLimitersRegistry.GetBucket(u.RateLimitBucket)
			if errLimiters != nil {
				return errLimiters
			}
		}

		method := req.(*common.JsonRpcRequest).Method
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
						return common.NewErrUpstreamRateLimitRuleExceeded(
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

		tryForward := func(
			ctx context.Context,
			req interface{},
		) error {
			resp, errCall := jsonRpcClient.SendRequest(ctx, req.(*common.JsonRpcRequest))

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

				return common.NewErrUpstreamRequest(
					errCall,
					u.Id,
				)
			}

			respBytes, errMarshal := json.Marshal(resp)
			if errMarshal != nil {
				health.MetricUpstreamRequestErrors.WithLabelValues(
					u.ProjectId,
					networkId,
					u.Id,
					method,
				).Inc()
				return common.NewErrUpstreamMalformedResponse(
					errMarshal,
					u.Id,
				)
			}

			if w.TryLock() {
				w.AddHeader("Content-Type", "application/json")
				w.AddHeader("X-ERPC-Upstream", u.Id)
				w.AddHeader("X-ERPC-Network", networkId)
				w.AddHeader("X-ERPC-Cache", "Miss")
				w.Write(respBytes)
			} else {
				return common.NewErrResponseWriteLock(u.Id)
			}

			return nil
		}

		if u.FailsafePolicies != nil && len(u.FailsafePolicies) > 0 {
			_, execErr := u.failsafeExecutor.
				WithContext(ctx).
				GetWithExecution(func(exec failsafe.Execution[interface{}]) (interface{}, error) {
					return nil, tryForward(ctx, req)
				})

			if execErr != nil {
				return resiliency.TranslateFailsafeError(execErr)
			}

			return execErr
		} else {
			return tryForward(ctx, req)
		}
	default:
		return common.NewErrUpstreamClientInitialization(
			fmt.Errorf("unsupported client type: %s", clientType),
			u.Id,
		)
	}
}

func (n *PreparedUpstream) Executor() failsafe.Executor[interface{}] {
	return n.failsafeExecutor
}
