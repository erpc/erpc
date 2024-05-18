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
	n.Logger.Debug().Object("req", req).Msgf("forwarding request")

	// TODO check if request exists in the hot, warm, or cold cache

	if err := n.acquireRateLimitPermit(req); err != nil {
		return err
	}

	var errorsByUpstream = []error{}

	// Function to prepare and forward the request to an upstream
	tryForward := func(u *upstream.PreparedUpstream) (skipped bool, err error) {
		lg := u.Logger.With().Str("network", n.NetworkId).Logger()
		if u.Score < 0 {
			lg.Debug().Msgf("skipping upstream with negative score %f", u.Score)
			return true, nil
		}

		pr, err := u.PrepareRequest(req)
		lg.Debug().Err(err).Msgf("prepared request: %v", pr)
		if pr == nil && err == nil {
			return true, nil
		}
		if err != nil {
			return false, err
		}

		err = n.forwardToUpstream(u, context.Background(), pr, w)
		if !common.IsNull(err) {
			return false, err
		}

		lg.Info().Msgf("successfully forward request")
		return false, nil
	}

	if n.FailsafePolicies == nil || len(n.FailsafePolicies) == 0 {
		// Handling via simple loop over upstreams until one responds
		for _, u := range n.Upstreams {
			if _, err := tryForward(u); err != nil {
				errorsByUpstream = append(errorsByUpstream, err)
				continue
			}
			return nil
		}

		return common.NewErrUpstreamsExhausted(errorsByUpstream)
	}

	// Handling when FailsafePolicies are defined
	i := 0
	_, execErr := n.Executor().GetWithExecution(func(exec failsafe.Execution[interface{}]) (interface{}, error) {
		n.Logger.Debug().Msgf("executing forward current index: %d", i)

		// We should try all upstreams at least once, but using "i" we make sure
		// across different executions we pick up next upstream vs retrying the same upstream.
		// This mimicks a round-robin behavior.
		// Upstream-level retry is handled by the upstream itself (and its own failsafe policies).
		ln := len(n.Upstreams)
		for count := 0; count < ln; count++ {
			u := n.Upstreams[i]
			i++
			if i >= ln {
				i = 0
			}
			n.Logger.Debug().Msgf("executing forward to upstream: %s next index: %d", u.Id, i)

			skipped, err := tryForward(u)
			n.Logger.Debug().Err(err).Msgf("forwarded request to upstream %s skipped: %v err: %v", u.Id, skipped, err)
			if !skipped {
				return nil, err
			} else if err != nil {
				errorsByUpstream = append(errorsByUpstream, err)
				continue
			}
		}

		return nil, common.NewErrUpstreamsExhausted(errorsByUpstream)
	})

	if execErr != nil {
		return resiliency.TranslateFailsafeError(execErr)
	}

	return nil
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
			permit := (*rule.Limiter).TryAcquirePermit()
			if !permit {
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
				n.Logger.Debug().Object("rateLimitRule", rule.Config).Msgf("network-level rate limit passed")
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
