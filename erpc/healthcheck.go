package erpc

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/bytedance/sonic"
)

func (s *HttpServer) handleHealthCheck(w http.ResponseWriter, startedAt *time.Time, projectId string, encoder sonic.Encoder, writeFatalError func(statusCode int, body error)) {
	logger := s.logger.With().Str("handler", "healthcheck").Str("projectId", projectId).Logger()

	if s.erpc == nil {
		handleErrorResponse(&logger, startedAt, nil, errors.New("eRPC is not initialized"), w, encoder, writeFatalError)
		return
	}

	var projects []*PreparedProject
	if projectId == "" {
		projects = s.erpc.GetProjects()
	} else {
		project, err := s.erpc.GetProject(projectId)
		if err != nil {
			handleErrorResponse(&logger, startedAt, nil, err, w, encoder, writeFatalError)
			return
		}
		projects = []*PreparedProject{project}
	}

	for _, project := range projects {
		h, err := project.GatherHealthInfo()
		if err != nil {
			handleErrorResponse(&logger, startedAt, nil, err, w, encoder, writeFatalError)
			return
		}

		if h.Upstreams != nil && len(h.Upstreams) > 0 {
			metricsTracker := project.upstreamsRegistry.GetMetricsTracker()
			allErrorRates := []float64{}
			for _, ups := range h.Upstreams {
				cfg := ups.Config()
				mts := metricsTracker.GetUpstreamMethodMetrics(cfg.Id, "*", "*")
				if mts != nil && mts.RequestsTotal.Load() > 0 {
					errorRate := float64(mts.ErrorsTotal.Load()) / float64(mts.RequestsTotal.Load())
					allErrorRates = append(allErrorRates, errorRate)
				}
			}

			if len(allErrorRates) > 0 {
				sort.Float64s(allErrorRates)
				if allErrorRates[0] > 0.99 {
					handleErrorResponse(
						&logger,
						startedAt,
						nil,
						fmt.Errorf("all upstreams are down: %+v", allErrorRates),
						w,
						encoder,
						writeFatalError,
					)
					return
				}
			}
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}
