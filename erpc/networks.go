package erpc

import (
	"context"
	"net/http"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/resiliency"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

type PreparedNetwork struct {
	NetworkId        string
	ProjectId        string
	FailsafePolicies []failsafe.Policy[any]
	Config           *config.NetworkConfig
	Logger           zerolog.Logger
	Upstreams        []*upstream.PreparedUpstream

	rateLimitersRegistry *resiliency.RateLimitersRegistry
	failsafeExecutor     failsafe.Executor[interface{}]
}

var preparedNetworks map[string]*PreparedNetwork = make(map[string]*PreparedNetwork)

func (r *ProjectsRegistry) NewNetwork(logger zerolog.Logger, prjCfg *config.ProjectConfig, nwCfg *config.NetworkConfig) (*PreparedNetwork, error) {
	var key = prjCfg.Id + ":" + nwCfg.NetworkId

	if pn, ok := preparedNetworks[key]; ok {
		return pn, nil
	}

	var policies []failsafe.Policy[any]

	if (nwCfg != nil) && (nwCfg.Failsafe != nil) {
		pls, err := resiliency.CreateFailSafePolicies(key, nwCfg.Failsafe)
		if err != nil {
			return nil, err
		}
		policies = pls
	}

	preparedNetworks[key] = &PreparedNetwork{
		NetworkId:        nwCfg.NetworkId,
		ProjectId:        prjCfg.Id,
		FailsafePolicies: policies,
		Config:           nwCfg,
		Logger:           logger.With().Str("network", nwCfg.NetworkId).Logger(),

		rateLimitersRegistry: r.rateLimitersRegistry,
		failsafeExecutor:     failsafe.NewExecutor[interface{}](policies...),
	}

	return preparedNetworks[key], nil
}

func (n *PreparedNetwork) Forward(ctx context.Context, req *upstream.NormalizedRequest, w http.ResponseWriter) error {
	// TODO check if request exists in the hot, warm, or cold cache

	if err := n.acquireRateLimitPermit(req); err != nil {
		return err
	}

	var errorsByUpstream = make(map[string]error)
	for _, u := range n.Upstreams {
		lg := u.Logger.With().Str("network", n.NetworkId).Logger()
		if u.Score < 0 {
			lg.Debug().Msgf("skipping upstream with negative score %f", u.Score)
			continue
		}

		pr, errPrep := u.PrepareRequest(req)
		lg.Debug().Err(errPrep).Msgf("prepared request prepared: %v", pr)
		if pr == nil && errPrep == nil {
			continue
		}
		if errPrep != nil {
			errorsByUpstream[u.Id] = errPrep
			continue
		}

		var forwardErr error

		if u.FailsafePolicies == nil || len(u.FailsafePolicies) == 0 {
			lg.Debug().Msgf("forwarding request to upstream without any failsafe policy")
			forwardErr = n.forwardToUpstream(u, context.Background(), pr, w)
			if !common.IsNull(forwardErr) {
				lg.Debug().Err(forwardErr).Msgf("forward errored for upstream without any failsafe policy")
			}
		} else {
			_, execErr := n.Executor().GetWithExecution(func(exec failsafe.Execution[interface{}]) (interface{}, error) {
				lg.Debug().Int("attempts", exec.Attempts()).Msgf("forwarding request to upstream")
				err := n.forwardToUpstream(u, exec.Context(), pr, w)
				if !common.IsNull(err) {
					lg.Debug().Err(err).Msgf("forward errored for upstream")
				}
				return nil, err
			})
			if execErr != nil {
				forwardErr = resiliency.TranslateFailsafeError(execErr)
			}
		}

		if forwardErr == nil {
			lg.Info().Msgf("successfully forward request")
			return nil
		} else {
			lg.Debug().Err(forwardErr).Msgf("failed to forward request")
		}

		errorsByUpstream[u.Id] = forwardErr
	}

	return common.NewErrUpstreamsExhausted(errorsByUpstream)
}

func (n *PreparedNetwork) Executor() failsafe.Executor[interface{}] {
	return n.failsafeExecutor
}

func (n *PreparedNetwork) acquireRateLimitPermit(req *upstream.NormalizedRequest) error {
	if n.Config.RateLimitBucket == "" {
		return nil
	}

	rlb, errNetLimit := n.rateLimitersRegistry.GetBucket(n.Config.RateLimitBucket)
	if errNetLimit != nil {
		return errNetLimit
	}
	if rlb == nil {
		return nil
	}

	method, errMethod := req.Method()
	if errMethod != nil {
		return errMethod
	}

	rules := rlb.GetRulesByMethod(method)
	n.Logger.Debug().Msgf("found %d network-level rate limiters for network: %s method: %s", len(rules), n.NetworkId, method)

	if len(rules) > 0 {
		for _, rule := range rules {
			if !(*rule.Limiter).TryAcquirePermit() {
				health.MetricNetworkRequestLocalRateLimited.WithLabelValues(
					n.ProjectId,
					n.NetworkId,
					method,
				).Inc()
				return common.NewErrNetworkRateLimitRuleExceeded(
					n.ProjectId,
					n.NetworkId,
					n.Config.RateLimitBucket,
					rule.Config,
				)
			} else {
				n.Logger.Debug().Msgf("network-level rate limit '%v' passed for network: %s", rule.Config, n.NetworkId)
			}
		}
	}

	return nil
}

func (n *PreparedNetwork) forwardToUpstream(
	thisUpstream *upstream.PreparedUpstream,
	ctx context.Context,
	r interface{},
	w http.ResponseWriter,
) error {
	var category string = ""
	if jrr, ok := r.(*upstream.JsonRpcRequest); ok {
		category = jrr.Method
	}
	health.MetricUpstreamRequestTotal.WithLabelValues(
		n.ProjectId,
		n.NetworkId,
		thisUpstream.Id,
		category,
	).Inc()
	timer := prometheus.NewTimer(health.MetricUpstreamRequestDuration.WithLabelValues(
		n.ProjectId,
		n.NetworkId,
		thisUpstream.Id,
		category,
	))
	defer timer.ObserveDuration()

	return thisUpstream.Forward(ctx, n.NetworkId, r, w)
}
