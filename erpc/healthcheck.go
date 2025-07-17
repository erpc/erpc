package erpc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
)

type HealthCheckResponse struct {
	Status  string         `json:"status"`
	Message string         `json:"message,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

func (s *HttpServer) handleHealthCheck(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	startedAt *time.Time,
	projectId string,
	architecture string,
	chainId string,
	encoder sonic.Encoder,
	writeFatalError func(ctx context.Context, statusCode int, body error),
) {
	logger := s.logger.With().Str("handler", "healthcheck").Str("projectId", projectId).Logger()
	if s.draining.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}

	if s.healthCheckAuthRegistry != nil {
		headers := r.Header
		queryArgs := r.URL.Query()

		ap, err := auth.NewPayloadFromHttp("healthcheck", r.RemoteAddr, headers, queryArgs)
		if err != nil {
			handleErrorResponse(ctx, &logger, startedAt, nil, err, w, encoder, writeFatalError, &common.TRUE)
			return
		}
		if s.healthCheckAuthRegistry != nil {
			if err := s.healthCheckAuthRegistry.Authenticate(ctx, "healthcheck", ap); err != nil {
				handleErrorResponse(ctx, &logger, startedAt, nil, err, w, encoder, writeFatalError, &common.TRUE)
				return
			}
		}
	}
	evalStrategy := r.URL.Query().Get("eval")
	if evalStrategy == "" && s.healthCheckCfg != nil && s.healthCheckCfg.DefaultEval != "" {
		evalStrategy = s.healthCheckCfg.DefaultEval
	}
	if evalStrategy == "" {
		evalStrategy = common.EvalAnyInitializedUpstreams
	}
	logger = logger.With().Str("evalStrategy", evalStrategy).Logger()

	if s.erpc == nil {
		handleErrorResponse(ctx, &logger, startedAt, nil, errors.New("eRPC is not initialized"), w, encoder, writeFatalError, s.serverCfg.IncludeErrorDetails)
		return
	}

	var projects []*PreparedProject
	if projectId == "" {
		projects = s.erpc.GetProjects()
		if len(projects) == 0 {
			handleErrorResponse(ctx, &logger, startedAt, nil, errors.New("no projects found"), w, encoder, writeFatalError, s.serverCfg.IncludeErrorDetails)
			return
		}
	} else {
		project, err := s.erpc.GetProject(projectId)
		if err != nil {
			handleErrorResponse(ctx, &logger, startedAt, nil, err, w, encoder, writeFatalError, &common.TRUE)
			return
		}
		projects = []*PreparedProject{project}
	}

	// Track health status across all projects and networks
	allHealthy := true
	details := make(map[string]any)

	for _, project := range projects {
		project.cfgMu.RLock()
		staticUpsCount := len(project.Config.Upstreams)
		staticNetsCount := len(project.Config.Networks)
		staticProvidersCount := len(project.Config.Providers)
		project.cfgMu.RUnlock()

		// If no upstreams are statically configured but there are some networks statically defined,
		// let's lazy load the upstreams for those networks so that we have enough metrics to evaluate the health.
		registeredUpsList, _ := project.upstreamsRegistry.GetSortedUpstreams(ctx, "*", "*")
		if len(registeredUpsList) == 0 && staticUpsCount == 0 {
			project.cfgMu.RLock()
			if staticNetsCount > 0 {
				for _, network := range project.Config.Networks {
					// Ignore the error because healthcheck eval might only care about 1 upstream
					_ = project.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, network.NetworkId())
				}
			}
			project.cfgMu.RUnlock()
		}

		// When healthcheck for a specific network is requested, we need to ensure that the upstreams are initialized for that network.
		if architecture != "" && chainId != "" {
			networkId := fmt.Sprintf("%s:%s", architecture, chainId)
			if len(project.upstreamsRegistry.GetNetworkUpstreams(ctx, networkId)) == 0 {
				// Ignore the error because healthcheck eval might only care about 1 upstream
				_ = project.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, networkId)
			}
		}

		// Attempt to gather health info for all initialized upstreams
		projHealthInfo, err := project.GatherHealthInfo()
		if err != nil {
			handleErrorResponse(ctx, &logger, startedAt, nil, err, w, encoder, writeFatalError, &common.TRUE)
			return
		}

		// Special case when no upstreams are statically configured and only one or more providers are configured
		// we should return OK so that container is considered healthy to receive the first request for lazy-loading of the upstreams.
		// To avoid this scenario it is recommended to define the networks and/or upstreams statically OR call healthcheck endpoint
		// for a specific network and chainId for example: "http://localhost:4000/evm/1/healthcheck"
		// so that upstreams are lazy-loaded for the given network and chainId.
		if staticProvidersCount > 0 && len(projHealthInfo.Upstreams) == 0 && staticUpsCount == 0 {
			allHealthy = true
			details["status"] = "OK"
			details["message"] = "no upstreams initialized yet, and no networks configured, but there are providers configured, send first actual request to initialize the upstreams"
			details["totalProviders"] = staticProvidersCount
			break
		}

		// Filter upstreams by network if architecture and chainId are specified
		var filteredUpstreams []*upstream.Upstream
		if architecture != "" && chainId != "" {
			for _, ups := range projHealthInfo.Upstreams {
				upsConfig := ups.Config()
				switch common.NetworkArchitecture(architecture) {
				case common.ArchitectureEvm:
					cid, err := strconv.ParseInt(chainId, 10, 64)
					if err != nil {
						logger.Error().Err(err).Msg("failed to parse chainId")
						continue
					}
					if upsConfig.Evm != nil && upsConfig.Evm.ChainId == cid {
						filteredUpstreams = append(filteredUpstreams, ups)
					}
				}
			}
		} else {
			filteredUpstreams = projHealthInfo.Upstreams
		}

		metricsTracker := project.upstreamsRegistry.GetMetricsTracker()

		projectDetails := map[string]any{
			"config": map[string]any{
				"networks":  staticNetsCount,
				"upstreams": staticUpsCount,
				"providers": staticProvidersCount,
			},
		}
		projectHealthy := true
		upstreamsDetails := make(map[string]map[string]any)

		for _, ups := range filteredUpstreams {
			upstreamsDetails[ups.Id()] = map[string]any{
				"network": ups.NetworkId(),
			}
			if !s.isSimpleMode() {
				mts := metricsTracker.GetUpstreamMethodMetrics(ups, "*")
				upstreamsDetails[ups.Id()]["metrics"] = mts

				// Add last evaluation time from selection policy evaluator
				if network, err := project.GetNetwork(ups.NetworkId()); err == nil && network != nil {
					if network.selectionPolicyEvaluator != nil {
						lastEvalTime := network.selectionPolicyEvaluator.GetLastEvalTime(ups.Id(), "*")
						if !lastEvalTime.IsZero() {
							upstreamsDetails[ups.Id()]["lastEvaluation"] = lastEvalTime.Format(time.RFC3339)
						}
					}
				}
			}
		}

		// Apply the evaluation strategy
		switch evalStrategy {
		case common.EvalAnyInitializedUpstreams:
			if len(filteredUpstreams) > 0 {
				projectDetails["status"] = "OK"
				projectDetails["message"] = fmt.Sprintf("%d upstreams are initialized", len(filteredUpstreams))
			} else {
				projectHealthy = false
				projectDetails["status"] = "ERROR"
				projectDetails["message"] = "no upstreams initialized"
			}

		case common.EvalAnyErrorRateBelow90,
			common.EvalAnyErrorRateBelow100,
			common.EvalAllErrorRateBelow90,
			common.EvalAllErrorRateBelow100:
			// Check error rates
			threshold := 0.90
			inclusionStrategy := "any"
			if evalStrategy == common.EvalAnyErrorRateBelow100 ||
				evalStrategy == common.EvalAllErrorRateBelow100 {
				threshold = 1.00
			}
			if evalStrategy == common.EvalAllErrorRateBelow90 ||
				evalStrategy == common.EvalAllErrorRateBelow100 {
				inclusionStrategy = "all"
			}

			allErrorRates := []float64{}
			belowThresholdErrorRates := []float64{}

			for _, ups := range filteredUpstreams {
				mts := metricsTracker.GetUpstreamMethodMetrics(ups, "*")
				if mts != nil && mts.RequestsTotal.Load() > 0 {
					errorRate := float64(mts.ErrorsTotal.Load()) / float64(mts.RequestsTotal.Load())
					allErrorRates = append(allErrorRates, errorRate)
					if errorRate < threshold {
						belowThresholdErrorRates = append(belowThresholdErrorRates, errorRate)
					}
				}
			}

			switch inclusionStrategy {
			case "any":
				if len(belowThresholdErrorRates) == 0 && len(allErrorRates) == 0 {
					projectHealthy = false
					projectDetails["status"] = "ERROR"
					projectDetails["message"] = "no error rate data available yet"
				} else if len(belowThresholdErrorRates) == 0 && len(allErrorRates) > 0 {
					projectHealthy = false
					projectDetails["status"] = "ERROR"
					projectDetails["message"] = fmt.Sprintf("all %d upstreams have high error rates", len(allErrorRates))
				} else {
					projectDetails["status"] = "OK"
					projectDetails["message"] = fmt.Sprintf("%d / %d upstreams have low error rates", len(belowThresholdErrorRates), len(allErrorRates))
				}
			case "all":
				if len(belowThresholdErrorRates) == len(allErrorRates) {
					projectDetails["status"] = "OK"
					projectDetails["message"] = fmt.Sprintf("%d / %d upstreams have low error rates", len(belowThresholdErrorRates), len(allErrorRates))
				} else {
					projectHealthy = false
					projectDetails["status"] = "ERROR"
					projectDetails["message"] = fmt.Sprintf("%d / %d upstreams have high error rates", len(allErrorRates)-len(belowThresholdErrorRates), len(allErrorRates))
				}
			}

		case common.EvalEvmAllChainId,
			common.EvalEvmAnyChainId:
			results := checkEvmChainId(ctx, filteredUpstreams, upstreamsDetails, evalStrategy)
			projectHealthy = results.healthy
			projectDetails["status"] = results.status
			projectDetails["message"] = results.message

		case common.EvalAllActiveUpstreams:
			// Check if all configured upstreams are initialized and not cordoned
			totalInitializedUpstreams := len(filteredUpstreams)
			activeUpstreams := 0
			cordonedUpstreams := 0

			// Count static upstreams for this specific network
			networkStaticUpsCount := 0
			if architecture != "" && chainId != "" {
				// Count only static upstreams that match the specific network
				for _, upsConfig := range project.Config.Upstreams {
					switch common.NetworkArchitecture(architecture) {
					case common.ArchitectureEvm:
						if cid, err := strconv.ParseInt(chainId, 10, 64); err == nil {
							if upsConfig.Evm != nil && upsConfig.Evm.ChainId == cid {
								networkStaticUpsCount++
							}
						}
					}
				}
			} else {
				// When checking all networks, use total static upstream count
				networkStaticUpsCount = staticUpsCount
			}

			var hasUninitializedUpstreams bool
			if networkStaticUpsCount > 0 {
				hasUninitializedUpstreams = networkStaticUpsCount > totalInitializedUpstreams
			} else {
				// For provider-based setups, ignore uninitialized upstream check
				hasUninitializedUpstreams = false
			}

			for _, ups := range filteredUpstreams {
				if metricsTracker.IsCordoned(ups, "*") {
					cordonedUpstreams++
				} else {
					activeUpstreams++
				}
			}

			if networkStaticUpsCount == 0 && staticProvidersCount == 0 {
				projectHealthy = false
				projectDetails["status"] = "ERROR"
				projectDetails["message"] = "no upstreams configured"
			} else if hasUninitializedUpstreams {
				projectHealthy = false
				projectDetails["status"] = "ERROR"
				projectDetails["message"] = fmt.Sprintf("only %d / %d upstreams are initialized (%d active, %d cordoned)", totalInitializedUpstreams, networkStaticUpsCount, activeUpstreams, cordonedUpstreams)
			} else if activeUpstreams >= totalInitializedUpstreams {
				projectDetails["status"] = "OK"
				projectDetails["message"] = fmt.Sprintf("all %d upstreams are active (initialized and not cordoned)", totalInitializedUpstreams)
			} else {
				projectHealthy = false
				projectDetails["status"] = "ERROR"
				projectDetails["message"] = fmt.Sprintf("%d / %d upstreams are active (%d cordoned)", activeUpstreams, totalInitializedUpstreams, cordonedUpstreams)
			}

		default:
			// Unknown evaluation strategy
			projectHealthy = false
			projectDetails["status"] = "ERROR"
			projectDetails["message"] = fmt.Sprintf("unknown evaluation strategy: %s", evalStrategy)
		}

		if !projectHealthy {
			allHealthy = false
		}
		if !s.isSimpleMode() {
			projectDetails["upstreams"] = upstreamsDetails
			projectDetails["initializer"] = project.upstreamsRegistry.GetInitializer().Status()
		}
		details[project.Config.Id] = projectDetails
	}

	if s.isSimpleMode() {
		if allHealthy {
			common.EnrichHTTPServerSpan(ctx, http.StatusOK, nil)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		} else {
			w.WriteHeader(http.StatusBadGateway)
			handleErrorResponse(
				ctx,
				&logger,
				startedAt,
				nil,
				&common.BaseError{
					Code:    "ErrHealthCheckFailed",
					Message: "one or more health checks failed",
					Details: details,
				},
				w,
				encoder,
				writeFatalError,
				&common.TRUE,
			)
		}
	} else {
		statusCode := http.StatusOK
		if !allHealthy {
			statusCode = http.StatusBadGateway
		}
		response := HealthCheckResponse{
			Details: details,
		}
		if allHealthy {
			response.Status = "OK"
			response.Message = "all systems operational"
		} else {
			response.Status = "ERROR"
			response.Message = "one or more health checks failed"
		}

		w.Header().Set("Content-Type", "application/json")
		common.EnrichHTTPServerSpan(ctx, statusCode, nil)
		w.WriteHeader(statusCode)

		if err := encoder.Encode(response); err != nil {
			logger.Error().Err(err).Msg("failed to encode health check response")
		}
	}
}

func (s *HttpServer) isSimpleMode() bool {
	return s.healthCheckCfg == nil ||
		s.healthCheckCfg.Mode == "" ||
		s.healthCheckCfg.Mode == common.HealthCheckModeSimple
}

type chainIdCheckResults struct {
	healthy bool
	status  string
	message string
	details map[string]map[string]any
}

func checkEvmChainId(ctx context.Context, upstreams []*upstream.Upstream, upstreamsDetails map[string]map[string]any, evalStrategy string) chainIdCheckResults {
	results := chainIdCheckResults{
		healthy: true,
		status:  "OK",
		details: make(map[string]map[string]any),
	}

	var successCount atomic.Int64
	var failureCount atomic.Int64

	wg := sync.WaitGroup{}
	mu := sync.Mutex{}
	semaphore := make(chan struct{}, 10)

	for _, ups := range upstreams {
		upsConfig := ups.Config()

		if upsConfig.Type != common.UpstreamTypeEvm || upsConfig.Evm == nil {
			continue
		}

		wg.Add(1)
		go func(ups *upstream.Upstream) {
			defer wg.Done()

			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			expectedChainId := ups.Config().Evm.ChainId
			mu.Lock()
			upstreamResult := upstreamsDetails[ups.Id()]
			upstreamResult["expectedChainId"] = expectedChainId
			mu.Unlock()

			reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			upChainId, err := ups.EvmGetChainId(reqCtx)

			if err != nil {
				mu.Lock()
				upstreamResult["status"] = "ERROR"
				upstreamResult["message"] = "eth_chainId request failed"
				mu.Unlock()
				failureCount.Add(1)
				return
			}

			actualChainId, err := strconv.ParseInt(upChainId, 0, 64)
			if err != nil {
				mu.Lock()
				upstreamResult["status"] = "ERROR"
				upstreamResult["message"] = "invalid chain id format"
				mu.Unlock()
				failureCount.Add(1)
				return
			}

			upstreamResult["actualChainId"] = actualChainId

			if actualChainId != expectedChainId {
				mu.Lock()
				upstreamResult["status"] = "ERROR"
				upstreamResult["message"] = fmt.Sprintf("chain id mismatch: expected %d, got %d", expectedChainId, actualChainId)
				mu.Unlock()
				failureCount.Add(1)
				return
			}

			mu.Lock()
			upstreamResult["status"] = "OK"
			mu.Unlock()
			successCount.Add(1)
		}(ups)
	}

	wg.Wait()

	successTotal := successCount.Load()
	failureTotal := failureCount.Load()

	if failureTotal > 0 {
		if evalStrategy == common.EvalEvmAnyChainId && successTotal > 0 {
			results.message = fmt.Sprintf("%d / %d upstreams passed (%d failed)", successTotal, len(upstreams), failureTotal)
		} else {
			results.healthy = false
			results.status = "ERROR"
			results.message = fmt.Sprintf("chain id verification failed for %d / %d upstreams", failureTotal, len(upstreams))
		}
	} else if successTotal == 0 {
		results.healthy = false
		results.status = "ERROR"
		results.message = "no upstreams available for chain ID verification"
	} else {
		results.message = fmt.Sprintf("all %d / %d upstreams verified", successTotal, len(upstreams))
	}

	return results
}
