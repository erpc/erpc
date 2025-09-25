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
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/upstream"
)

type HealthCheckResponse struct {
	Status  string         `json:"status"`
	Message string         `json:"message,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// Unified health data structures for all modes
type ProjectHealthData struct {
	ProjectId         string                        `json:"projectId"`
	Healthy           bool                          `json:"healthy"`
	Status            string                        `json:"status"`
	Message           string                        `json:"message"`
	StaticCounts      *StaticCounts                 `json:"staticCounts,omitempty"`
	Networks          map[string]*NetworkHealthData `json:"networks"`
	InitializerStatus any                           `json:"initializerStatus,omitempty"`
}

type StaticCounts struct {
	Networks  int `json:"networks"`
	Upstreams int `json:"upstreams"`
	Providers int `json:"providers"`
}

type NetworkHealthData struct {
	NetworkId string                         `json:"networkId"`
	Alias     string                         `json:"alias,omitempty"`
	Healthy   bool                           `json:"healthy"`
	Status    string                         `json:"status"`
	Message   string                         `json:"message,omitempty"`
	Upstreams map[string]*UpstreamHealthData `json:"upstreams"`
}

type UpstreamHealthData struct {
	UpstreamId      string `json:"upstreamId"`
	NetworkId       string `json:"networkId"`
	Healthy         bool   `json:"healthy"`
	Metrics         any    `json:"metrics,omitempty"`
	LastEvaluation  string `json:"lastEvaluation,omitempty"`
	ExpectedChainId int64  `json:"expectedChainId,omitempty"`
	ActualChainId   int64  `json:"actualChainId,omitempty"`
	ChainIdStatus   string `json:"chainIdStatus,omitempty"`
	ChainIdMessage  string `json:"chainIdMessage,omitempty"`
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
			_, err := s.healthCheckAuthRegistry.Authenticate(ctx, "healthcheck", ap)
			if err != nil {
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

	// Collect all health data in a unified structure
	projectsHealth := make([]*ProjectHealthData, 0, len(projects))
	allHealthy := true

	for _, project := range projects {
		// Initialize project health data
		projectHealth := &ProjectHealthData{
			ProjectId: project.Config.Id,
			Healthy:   true,
			Status:    "OK",
			Networks:  make(map[string]*NetworkHealthData),
		}

		project.cfgMu.RLock()
		staticUpsCount := len(project.Config.Upstreams)
		staticNetsCount := len(project.Config.Networks)
		staticProvidersCount := len(project.Config.Providers)
		projectHealth.StaticCounts = &StaticCounts{
			Networks:  staticNetsCount,
			Upstreams: staticUpsCount,
			Providers: staticProvidersCount,
		}
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

		// Store initializer status for verbose mode
		projectHealth.InitializerStatus = projHealthInfo.Initialization

		// Special case when no upstreams are statically configured and only one or more providers are configured
		if staticProvidersCount > 0 && len(projHealthInfo.Upstreams) == 0 && staticUpsCount == 0 {
			projectHealth.Status = "OK"
			projectHealth.Message = "no upstreams initialized yet, and no networks configured, but there are providers configured, send first actual request to initialize the upstreams"
			projectHealth.Healthy = true
			projectsHealth = append(projectsHealth, projectHealth)
			continue
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

		// Group upstreams by network and collect health data
		for _, ups := range filteredUpstreams {
			networkId := ups.NetworkId()

			// Get or create network health data
			networkHealth, exists := projectHealth.Networks[networkId]
			if !exists {
				network, _ := project.GetNetwork(ctx, networkId)
				alias := ""
				if network != nil && network.Config() != nil {
					alias = network.Config().Alias
				}
				networkHealth = &NetworkHealthData{
					NetworkId: networkId,
					Alias:     alias,
					Healthy:   true,
					Status:    "OK",
					Upstreams: make(map[string]*UpstreamHealthData),
				}
				projectHealth.Networks[networkId] = networkHealth
			}

			// Create upstream health data
			upstreamHealth := &UpstreamHealthData{
				UpstreamId: ups.Id(),
				NetworkId:  networkId,
				Healthy:    true,
			}

			// Collect metrics for non-simple modes
			if !s.isSimpleMode() {
				mts := metricsTracker.GetUpstreamMethodMetrics(ups, "*")
				upstreamHealth.Metrics = mts

				// Add last evaluation time from selection policy evaluator
				if network, err := project.GetNetwork(ctx, networkId); err == nil && network != nil {
					if network.selectionPolicyEvaluator != nil {
						lastEvalTime := network.selectionPolicyEvaluator.GetLastEvalTime(ups.Id(), "*")
						if !lastEvalTime.IsZero() {
							upstreamHealth.LastEvaluation = lastEvalTime.Format(time.RFC3339)
						}
					}
				}
			}

			networkHealth.Upstreams[ups.Id()] = upstreamHealth
		}

		// First, evaluate each network individually for granular visibility
		for networkId, networkHealth := range projectHealth.Networks {
			networkUpstreams := make([]*upstream.Upstream, 0)
			for _, ups := range filteredUpstreams {
				if ups.NetworkId() == networkId {
					networkUpstreams = append(networkUpstreams, ups)
				}
			}

			// Evaluate this specific network
			networkHealthy, networkStatus, networkMessage := s.evaluateNetworkHealth(
				ctx, networkUpstreams, networkHealth.Upstreams, metricsTracker,
				project, networkId, evalStrategy,
			)

			networkHealth.Healthy = networkHealthy
			networkHealth.Status = networkStatus
			networkHealth.Message = networkMessage
		}

		// Now determine the overall project health based on the scope
		if architecture != "" && chainId != "" {
			// Specific network requested - use that network's health as project health
			targetNetworkId := fmt.Sprintf("%s:%s", architecture, chainId)
			if networkHealth, exists := projectHealth.Networks[targetNetworkId]; exists {
				projectHealth.Healthy = networkHealth.Healthy
				projectHealth.Status = networkHealth.Status
				projectHealth.Message = networkHealth.Message
			} else {
				// Network not found - mark as error
				projectHealth.Healthy = false
				projectHealth.Status = "ERROR"
				projectHealth.Message = fmt.Sprintf("network %s not found", targetNetworkId)
			}
		} else {
			// No specific network - evaluate across ALL upstreams
			projectHealthy, projectStatus, projectMessage := s.evaluateProjectHealth(
				ctx, filteredUpstreams, projectHealth.Networks, metricsTracker,
				project, evalStrategy,
			)

			projectHealth.Healthy = projectHealthy
			projectHealth.Status = projectStatus
			projectHealth.Message = projectMessage
		}

		// Update overall project status
		if !projectHealth.Healthy {
			projectHealth.Status = "ERROR"
			allHealthy = false
		}

		projectsHealth = append(projectsHealth, projectHealth)
	}

	// Format and return response based on mode
	statusCode := http.StatusOK
	if !allHealthy {
		statusCode = http.StatusBadGateway
	}

	if s.isSimpleMode() {
		// Simple mode: just return OK or error
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
					Details: s.formatHealthDataForMode(projectsHealth, "simple").(map[string]any),
				},
				w,
				encoder,
				writeFatalError,
				&common.TRUE,
			)
		}
		return
	}

	// Networks or Verbose mode: return JSON response
	w.Header().Set("Content-Type", "application/json")
	common.EnrichHTTPServerSpan(ctx, statusCode, nil)
	w.WriteHeader(statusCode)

	var response interface{}
	if s.isNetworksMode() {
		response = s.formatHealthDataForMode(projectsHealth, "networks")
	} else {
		// Verbose mode
		response = HealthCheckResponse{
			Status: func() string {
				if allHealthy {
					return "OK"
				} else {
					return "ERROR"
				}
			}(),
			Message: func() string {
				if allHealthy {
					return "all systems operational"
				} else {
					return "one or more health checks failed"
				}
			}(),
			Details: s.formatHealthDataForMode(projectsHealth, "verbose").(map[string]any),
		}
	}

	if err := encoder.Encode(response); err != nil {
		logger.Error().Err(err).Msg("failed to encode health check response")
	}
}

func (s *HttpServer) isSimpleMode() bool {
	return s.healthCheckCfg == nil ||
		s.healthCheckCfg.Mode == "" ||
		s.healthCheckCfg.Mode == common.HealthCheckModeSimple
}

func (s *HttpServer) isNetworksMode() bool {
	return s.healthCheckCfg != nil &&
		s.healthCheckCfg.Mode == common.HealthCheckModeNetworks
}

// evaluateNetworkHealth evaluates a single network's health based on the eval strategy
func (s *HttpServer) evaluateNetworkHealth(
	ctx context.Context,
	networkUpstreams []*upstream.Upstream,
	upstreamsHealth map[string]*UpstreamHealthData,
	metricsTracker *health.Tracker,
	project *PreparedProject,
	networkId string,
	evalStrategy string,
) (healthy bool, status string, message string) {
	// Get network config if available
	network, _ := project.GetNetwork(ctx, networkId)
	var nwCfg *common.NetworkConfig
	if network != nil {
		nwCfg = network.Config()
	}

	project.cfgMu.RLock()
	staticUps := project.Config.Upstreams
	staticProvidersCount := len(project.Config.Providers)
	project.cfgMu.RUnlock()

	switch evalStrategy {
	case common.EvalAnyInitializedUpstreams:
		if len(networkUpstreams) > 0 {
			return true, "OK", fmt.Sprintf("%d upstreams initialized", len(networkUpstreams))
		}
		return false, "ERROR", "no upstreams initialized"

	case common.EvalAnyErrorRateBelow90, common.EvalAnyErrorRateBelow100,
		common.EvalAllErrorRateBelow90, common.EvalAllErrorRateBelow100:
		threshold := 0.90
		inclusionStrategy := "any"
		if evalStrategy == common.EvalAnyErrorRateBelow100 || evalStrategy == common.EvalAllErrorRateBelow100 {
			threshold = 1.00
		}
		if evalStrategy == common.EvalAllErrorRateBelow90 || evalStrategy == common.EvalAllErrorRateBelow100 {
			inclusionStrategy = "all"
		}

		allErrorRates := []float64{}
		belowThresholdErrorRates := []float64{}

		for _, ups := range networkUpstreams {
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
				return false, "ERROR", "no error rate data available yet"
			} else if len(belowThresholdErrorRates) == 0 && len(allErrorRates) > 0 {
				return false, "ERROR", fmt.Sprintf("all %d upstreams have high error rates", len(allErrorRates))
			}
			return true, "OK", fmt.Sprintf("%d / %d upstreams have low error rates", len(belowThresholdErrorRates), len(allErrorRates))
		case "all":
			if len(belowThresholdErrorRates) == len(allErrorRates) && len(allErrorRates) > 0 {
				return true, "OK", fmt.Sprintf("%d / %d upstreams have low error rates", len(belowThresholdErrorRates), len(allErrorRates))
			}
			return false, "ERROR", fmt.Sprintf("%d / %d upstreams have high error rates", len(allErrorRates)-len(belowThresholdErrorRates), len(allErrorRates))
		}

	case common.EvalEvmAllChainId, common.EvalEvmAnyChainId:
		// Prepare upstream details for chain ID check
		upstreamsDetails := make(map[string]map[string]any)
		for _, ups := range networkUpstreams {
			upstreamsDetails[ups.Id()] = map[string]any{"network": ups.NetworkId()}
		}
		results := checkEvmChainId(ctx, networkUpstreams, upstreamsDetails, evalStrategy)

		// Update upstream health data with chain ID results
		for upsId, details := range upstreamsDetails {
			if upsHealth, ok := upstreamsHealth[upsId]; ok {
				if expectedChainId, ok := details["expectedChainId"].(int64); ok {
					upsHealth.ExpectedChainId = expectedChainId
				}
				if actualChainId, ok := details["actualChainId"].(int64); ok {
					upsHealth.ActualChainId = actualChainId
				}
				if status, ok := details["status"].(string); ok {
					upsHealth.ChainIdStatus = status
				}
				if message, ok := details["message"].(string); ok {
					upsHealth.ChainIdMessage = message
				}
			}
		}

		return results.healthy, results.status, results.message

	case common.EvalAllActiveUpstreams:
		// Count static upstreams that belong to this network
		networkStaticUpsCount := 0
		if nwCfg != nil {
			for _, upsCfg := range staticUps {
				if upsCfg == nil {
					continue
				}
				switch nwCfg.Architecture {
				case common.ArchitectureEvm:
					if upsCfg.Evm != nil && nwCfg.Evm != nil && upsCfg.Evm.ChainId == nwCfg.Evm.ChainId {
						networkStaticUpsCount++
					}
				}
			}
		}

		totalInitializedUpstreams := len(networkUpstreams)
		activeUpstreams := 0
		cordonedUpstreams := 0

		for _, ups := range networkUpstreams {
			if metricsTracker.IsCordoned(ups, "*") {
				cordonedUpstreams++
			} else {
				activeUpstreams++
			}
		}

		var hasUninitializedUpstreams bool
		if networkStaticUpsCount > 0 {
			hasUninitializedUpstreams = networkStaticUpsCount > totalInitializedUpstreams
		}

		if networkStaticUpsCount == 0 && staticProvidersCount == 0 {
			return false, "ERROR", "no upstreams configured"
		} else if hasUninitializedUpstreams {
			return false, "ERROR", fmt.Sprintf("only %d / %d upstreams are initialized (%d active, %d cordoned)", totalInitializedUpstreams, networkStaticUpsCount, activeUpstreams, cordonedUpstreams)
		} else if activeUpstreams >= totalInitializedUpstreams {
			return true, "OK", fmt.Sprintf("all %d upstreams are active (initialized and not cordoned)", totalInitializedUpstreams)
		}
		return false, "ERROR", fmt.Sprintf("%d / %d upstreams are active (%d cordoned)", activeUpstreams, totalInitializedUpstreams, cordonedUpstreams)

	default:
		return false, "ERROR", fmt.Sprintf("unknown evaluation strategy: %s", evalStrategy)
	}

	return false, "ERROR", "evaluation failed"
}

// evaluateProjectHealth evaluates project-wide health based on the eval strategy
func (s *HttpServer) evaluateProjectHealth(
	ctx context.Context,
	filteredUpstreams []*upstream.Upstream,
	networksHealth map[string]*NetworkHealthData,
	metricsTracker *health.Tracker,
	project *PreparedProject,
	evalStrategy string,
) (healthy bool, status string, message string) {
	project.cfgMu.RLock()
	staticUpsCount := len(project.Config.Upstreams)
	staticProvidersCount := len(project.Config.Providers)
	project.cfgMu.RUnlock()

	switch evalStrategy {
	case common.EvalAnyInitializedUpstreams:
		if len(filteredUpstreams) > 0 {
			return true, "OK", fmt.Sprintf("%d upstreams are initialized", len(filteredUpstreams))
		}
		return false, "ERROR", "no upstreams initialized"

	case common.EvalAnyErrorRateBelow90, common.EvalAnyErrorRateBelow100,
		common.EvalAllErrorRateBelow90, common.EvalAllErrorRateBelow100:
		threshold := 0.90
		inclusionStrategy := "any"
		if evalStrategy == common.EvalAnyErrorRateBelow100 || evalStrategy == common.EvalAllErrorRateBelow100 {
			threshold = 1.00
		}
		if evalStrategy == common.EvalAllErrorRateBelow90 || evalStrategy == common.EvalAllErrorRateBelow100 {
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
				return false, "ERROR", "no error rate data available yet"
			} else if len(belowThresholdErrorRates) == 0 && len(allErrorRates) > 0 {
				return false, "ERROR", fmt.Sprintf("all %d upstreams have high error rates", len(allErrorRates))
			}
			return true, "OK", fmt.Sprintf("%d / %d upstreams have low error rates", len(belowThresholdErrorRates), len(allErrorRates))
		case "all":
			if len(belowThresholdErrorRates) == len(allErrorRates) && len(allErrorRates) > 0 {
				return true, "OK", fmt.Sprintf("%d / %d upstreams have low error rates", len(belowThresholdErrorRates), len(allErrorRates))
			}
			return false, "ERROR", fmt.Sprintf("%d / %d upstreams have high error rates", len(allErrorRates)-len(belowThresholdErrorRates), len(allErrorRates))
		}

	case common.EvalEvmAllChainId, common.EvalEvmAnyChainId:
		// Prepare upstream details for chain ID check
		upstreamsDetails := make(map[string]map[string]any)
		for _, ups := range filteredUpstreams {
			upstreamsDetails[ups.Id()] = map[string]any{"network": ups.NetworkId()}
		}
		results := checkEvmChainId(ctx, filteredUpstreams, upstreamsDetails, evalStrategy)

		// Update upstream health data with chain ID results in all networks
		for upsId, details := range upstreamsDetails {
			for _, networkHealth := range networksHealth {
				if upsHealth, ok := networkHealth.Upstreams[upsId]; ok {
					if expectedChainId, ok := details["expectedChainId"].(int64); ok {
						upsHealth.ExpectedChainId = expectedChainId
					}
					if actualChainId, ok := details["actualChainId"].(int64); ok {
						upsHealth.ActualChainId = actualChainId
					}
					if status, ok := details["status"].(string); ok {
						upsHealth.ChainIdStatus = status
					}
					if message, ok := details["message"].(string); ok {
						upsHealth.ChainIdMessage = message
					}
				}
			}
		}

		return results.healthy, results.status, results.message

	case common.EvalAllActiveUpstreams:
		// When checking project-wide, use total static upstream count
		networkStaticUpsCount := staticUpsCount

		totalInitializedUpstreams := len(filteredUpstreams)
		activeUpstreams := 0
		cordonedUpstreams := 0

		for _, ups := range filteredUpstreams {
			if metricsTracker.IsCordoned(ups, "*") {
				cordonedUpstreams++
			} else {
				activeUpstreams++
			}
		}

		var hasUninitializedUpstreams bool
		if networkStaticUpsCount > 0 {
			hasUninitializedUpstreams = networkStaticUpsCount > totalInitializedUpstreams
		} else {
			// For provider-based setups, ignore uninitialized upstream check
			hasUninitializedUpstreams = false
		}

		if networkStaticUpsCount == 0 && staticProvidersCount == 0 {
			return false, "ERROR", "no upstreams configured"
		} else if hasUninitializedUpstreams {
			return false, "ERROR", fmt.Sprintf("only %d / %d upstreams are initialized (%d active, %d cordoned)", totalInitializedUpstreams, networkStaticUpsCount, activeUpstreams, cordonedUpstreams)
		} else if activeUpstreams >= totalInitializedUpstreams {
			return true, "OK", fmt.Sprintf("all %d upstreams are active (initialized and not cordoned)", totalInitializedUpstreams)
		}
		return false, "ERROR", fmt.Sprintf("%d / %d upstreams are active (%d cordoned)", activeUpstreams, totalInitializedUpstreams, cordonedUpstreams)

	default:
		return false, "ERROR", fmt.Sprintf("unknown evaluation strategy: %s", evalStrategy)
	}

	return false, "ERROR", "evaluation failed"
}

// formatHealthDataForMode formats the unified health data for the specified mode
func (s *HttpServer) formatHealthDataForMode(projectsHealth []*ProjectHealthData, mode string) interface{} {
	switch mode {
	case "simple":
		// Simple mode just needs basic project status
		result := make(map[string]any)
		for _, ph := range projectsHealth {
			result[ph.ProjectId] = map[string]any{
				"status":  ph.Status,
				"message": ph.Message,
			}
		}
		return result

	case "networks":
		// Networks mode: return per-project list of networks
		type NetworkHealthItem struct {
			Id    string `json:"id"`
			Alias string `json:"alias,omitempty"`
			State string `json:"state"`
		}

		result := make(map[string][]NetworkHealthItem)
		for _, ph := range projectsHealth {
			items := make([]NetworkHealthItem, 0)
			for _, nh := range ph.Networks {
				items = append(items, NetworkHealthItem{
					Id:    nh.NetworkId,
					Alias: nh.Alias,
					State: nh.Status,
				})
			}
			result[ph.ProjectId] = items
		}
		return result

	case "verbose":
		// Verbose mode: return full details in backward-compatible format
		result := make(map[string]any)
		for _, ph := range projectsHealth {
			projectDetails := map[string]any{
				"status":  ph.Status,
				"message": ph.Message,
			}

			if ph.StaticCounts != nil {
				projectDetails["config"] = map[string]any{
					"networks":  ph.StaticCounts.Networks,
					"upstreams": ph.StaticCounts.Upstreams,
					"providers": ph.StaticCounts.Providers,
				}
			}

			if ph.InitializerStatus != nil {
				projectDetails["initializer"] = ph.InitializerStatus
			}

			// Convert network/upstream data to flat upstream details for backward compat
			upstreamsDetails := make(map[string]map[string]any)
			for _, nh := range ph.Networks {
				for upsId, uh := range nh.Upstreams {
					details := map[string]any{
						"network": uh.NetworkId,
					}
					if uh.Metrics != nil {
						details["metrics"] = uh.Metrics
					}
					if uh.LastEvaluation != "" {
						details["lastEvaluation"] = uh.LastEvaluation
					}
					if uh.ExpectedChainId != 0 {
						details["expectedChainId"] = uh.ExpectedChainId
					}
					if uh.ActualChainId != 0 {
						details["actualChainId"] = uh.ActualChainId
					}
					if uh.ChainIdStatus != "" {
						details["status"] = uh.ChainIdStatus
					}
					if uh.ChainIdMessage != "" {
						details["message"] = uh.ChainIdMessage
					}
					upstreamsDetails[upsId] = details
				}
			}

			if len(upstreamsDetails) > 0 {
				projectDetails["upstreams"] = upstreamsDetails
			}

			result[ph.ProjectId] = projectDetails
		}
		return result

	default:
		return projectsHealth
	}
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
