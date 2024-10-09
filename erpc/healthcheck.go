package erpc

import (
	"errors"
	"fmt"
	"sort"

	"github.com/bytedance/sonic"
	"github.com/valyala/fasthttp"
)

func (s *HttpServer) handleHealthCheck(ctx *fasthttp.RequestCtx, encoder sonic.Encoder) {
	logger := s.logger.With().Str("handler", "healthcheck").Logger()

	if s.erpc == nil {
		handleErrorResponse(&logger, nil, errors.New("eRPC is not initialized"), ctx, encoder)
		return
	}

	projects := s.erpc.GetProjects()

	for _, project := range projects {
		h, err := project.gatherHealthInfo()
		if err != nil {
			handleErrorResponse(&logger, nil, err, ctx, encoder)
			return
		}

		if h.Upstreams != nil && len(h.Upstreams) > 0 {
			metricsTracker := project.upstreamsRegistry.GetMetricsTracker()
			allErrorRates := []float64{}
			for _, ups := range h.Upstreams {
				cfg := ups.Config()
				mts := metricsTracker.GetUpstreamMethodMetrics(cfg.Id, "*", "*")
				if mts != nil && mts.RequestsTotal > 0 {
					allErrorRates = append(allErrorRates, float64(mts.ErrorsTotal)/float64(mts.RequestsTotal))
				}
			}

			if len(allErrorRates) > 0 {
				sort.Float64s(allErrorRates)
				if allErrorRates[0] > 0.99 {
					handleErrorResponse(&logger, nil, fmt.Errorf("all upstreams are down: %+v", allErrorRates), ctx, encoder)
					return
				}
			}
		}
	}

	logger.Info().Msg("Healthcheck passed")
	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.SetBodyString("OK")
}
