package erpc

import (
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/bytedance/sonic"
)

func (s *HttpServer) handleHealthCheck(w http.ResponseWriter, encoder sonic.Encoder) {
	logger := s.logger.With().Str("handler", "healthcheck").Logger()

	if s.erpc == nil {
		handleErrorResponse(&logger, nil, errors.New("eRPC is not initialized"), w, encoder)
		return
	}

	projects := s.erpc.GetProjects()

	for _, project := range projects {
		h, err := project.GatherHealthInfo()
		if err != nil {
			handleErrorResponse(&logger, nil, err, w, encoder)
			return
		}

		if h.Upstreams != nil && len(h.Upstreams) > 0 {
			metricsTracker := project.upstreamsRegistry.GetMetricsTracker()
			allErrorRates := []float64{}
			for _, ups := range h.Upstreams {
				cfg := ups.Config()
				mts := metricsTracker.GetUpstreamMethodMetrics(cfg.Id, "*", "*")
				if mts != nil && mts.RequestsTotal > 0 {
					errorRate := float64(mts.ErrorsTotal) / float64(mts.RequestsTotal)
					allErrorRates = append(allErrorRates, errorRate)
				}
			}

			if len(allErrorRates) > 0 {
				sort.Float64s(allErrorRates)
				if allErrorRates[0] > 0.99 {
					handleErrorResponse(&logger, nil, fmt.Errorf("all upstreams are down: %+v", allErrorRates), w, encoder)
					return
				}
			}
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}
