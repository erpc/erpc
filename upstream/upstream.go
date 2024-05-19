package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/common"
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
}

type UpstreamMetrics struct {
	P90Latency     float64   `json:"p90Latency"`
	ErrorsTotal    float64   `json:"errorsTotal"`
	ThrottledTotal float64   `json:"throttledTotal"`
	RequestsTotal  float64   `json:"requestsTotal"`
	BlocksLag      float64   `json:"blocksLag"`
	LastCollect    time.Time `json:"lastCollect"`
}

func (c *UpstreamMetrics) MarshalZerologObject(e *zerolog.Event) {
	e.Float64("p90Latency", c.P90Latency).
		Float64("errorsTotal", c.ErrorsTotal).
		Float64("requestsTotal", c.RequestsTotal).
		Float64("throttledTotal", c.ThrottledTotal).
		Float64("blocksLag", c.BlocksLag).
		Time("lastCollect", c.LastCollect)
}

func (u *PreparedUpstream) PrepareRequest(normalizedReq *NormalizedRequest) (interface{}, error) {
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

func (u *PreparedUpstream) Forward(ctx context.Context, networkId string, req interface{}, w http.ResponseWriter, wmu *sync.Mutex) error {
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

		limitersBucket, errLimiters := u.rateLimitersRegistry.GetBucket(u.RateLimitBucket)
		if errLimiters != nil {
			return errLimiters
		}

		lg := u.Logger.With().Str("method", req.(*JsonRpcRequest).Method).Logger()

		lg.Trace().Msgf("checking upstream-level rate limiters bucket: %s", u.RateLimitBucket)
		category := req.(*JsonRpcRequest).Method

		if limitersBucket != nil {
			rules := limitersBucket.GetRulesByMethod(req.(*JsonRpcRequest).Method)
			if len(rules) > 0 {
				for _, rule := range rules {
					if !(*rule.Limiter).TryAcquirePermit() {
						lg.Warn().Msgf("upstream-level rate limit '%v' exceeded", rule.Config)
						health.MetricUpstreamRequestLocalRateLimited.WithLabelValues(
							u.ProjectId,
							networkId,
							u.Id,
							category,
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

		resp, errCall := jsonRpcClient.SendRequest(ctx, req.(*JsonRpcRequest))

		lg.Debug().Err(errCall).Msgf("upstream call result received: %v", resp)

		if errCall != nil {
			if !errors.Is(errCall, context.DeadlineExceeded) {
				health.MetricUpstreamRequestErrors.WithLabelValues(
					u.ProjectId,
					networkId,
					u.Id,
					category,
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
				category,
			).Inc()
			return common.NewErrUpstreamMalformedResponse(
				errMarshal,
				u.Id,
			)
		}

		if wmu.TryLock() {
			w.Header().Add("Content-Type", "application/json")
			w.Header().Add("X-ERPC-Upstream", u.Id)
			w.Header().Add("X-ERPC-Network", networkId)
			w.Write(respBytes)
		} else {
			return common.NewErrUpstreamResponseWriteLock(u.Id)
		}
		return nil
	default:
		return common.NewErrUpstreamClientInitialization(
			fmt.Errorf("unsupported client type: %s", clientType),
			u.Id,
		)
	}
}
