package erpc

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

// PolicyEvaluator is responsible for evaluating which upstreams should be active
// based on their performance metrics and configured policy rules
type PolicyEvaluator struct {
	networkId         string
	logger            *zerolog.Logger
	config            *common.SelectionPolicyConfig
	runtime           *common.Runtime
	upstreamsMu       sync.RWMutex
	metricsTracker    *health.Tracker
	upstreamsRegistry *upstream.UpstreamsRegistry

	// methodName -> upstreamId -> state
	methodStates map[string]map[string]*upstreamState
	// Handle global state when evalPerMethod is false
	globalState map[string]*upstreamState

	evalMutex sync.Mutex
	appCtx    context.Context
}

type upstreamState struct {
	mu               *sync.RWMutex
	isActive         bool
	resampleInterval time.Time
	sampleCounter    int
	lastEvalTime     time.Time
}

type metricData map[string]interface{}

func NewPolicyEvaluator(
	networkId string,
	logger *zerolog.Logger,
	config *common.SelectionPolicyConfig,
	upstreamsRegistry *upstream.UpstreamsRegistry,
	metricsTracker *health.Tracker,
) (*PolicyEvaluator, error) {
	runtime, err := common.NewRuntime()
	if err != nil {
		return nil, fmt.Errorf("failed to create JavaScript runtime: %w", err)
	}

	return &PolicyEvaluator{
		networkId:         networkId,
		logger:            logger,
		config:            config,
		runtime:           runtime,
		methodStates:      make(map[string]map[string]*upstreamState),
		globalState:       make(map[string]*upstreamState),
		upstreamsRegistry: upstreamsRegistry,
		metricsTracker:    metricsTracker,
	}, nil
}

func (p *PolicyEvaluator) Start(ctx context.Context) error {
	p.appCtx = ctx

	go func() {
		ticker := time.NewTicker(p.config.EvalInterval.Duration())
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.evaluateUpstreams(); err != nil {
					p.logger.Error().Err(err).Msg("failed to evaluate upstreams")
				}
			}
		}
	}()

	err := p.evaluateUpstreams()
	if err != nil {
		p.logger.Error().Err(err).Msg("failed to evaluate upstreams")
	}

	return nil
}

func (p *PolicyEvaluator) evaluateUpstreams() error {
	p.upstreamsMu.Lock()
	defer p.upstreamsMu.Unlock()

	// Get all upstreams for this network
	upsList := p.upstreamsRegistry.GetNetworkUpstreams(p.appCtx, p.networkId)
	if len(upsList) == 0 {
		return fmt.Errorf("no upstreams found for network: %s", p.networkId)
	}

	if p.config.EvalPerMethod {
		// Handle method-specific evaluations
		// Get all metrics to find unique methods
		allMetrics := make(map[string]bool)
		for _, ups := range upsList {
			metrics := p.metricsTracker.GetUpstreamMetrics(ups)
			for method := range metrics {
				allMetrics[method] = true
			}
		}

		// Evaluate each method separately
		for method := range allMetrics {
			if err := p.evaluateMethod(method, upsList); err != nil {
				p.logger.Error().Err(err).Str("method", method).Msg("failed to evaluate user-defined selectionPolicy for method")
			}
		}
	} else {
		// Handle network-level evaluation
		if err := p.evaluateMethod("*", upsList); err != nil {
			p.logger.Error().Err(err).Msg("failed to evaluate user-defined selectionPolicy for network")
		}
	}

	return nil
}

func (p *PolicyEvaluator) evaluateMethod(method string, upsList []*upstream.Upstream) error {
	metricsData := make([]metricData, len(upsList))
	for i, ups := range upsList {
		metrics := p.metricsTracker.GetUpstreamMethodMetrics(ups, method)
		metricsData[i] = metricData{
			"id":     ups.Id(),
			"config": ups.Config(),
			"metrics": map[string]interface{}{
				"errorRate":          metrics.ErrorRate(),
				"errorsTotal":        metrics.ErrorsTotal.Load(),
				"requestsTotal":      metrics.RequestsTotal.Load(),
				"throttledRate":      metrics.ThrottledRate(),
				"p90ResponseSeconds": metrics.ResponseQuantiles.GetQuantile(0.90).Seconds(),
				"p95ResponseSeconds": metrics.ResponseQuantiles.GetQuantile(0.95).Seconds(),
				"p99ResponseSeconds": metrics.ResponseQuantiles.GetQuantile(0.99).Seconds(),
				"blockHeadLag":       metrics.BlockHeadLag.Load(),
				"finalizationLag":    metrics.FinalizationLag.Load(),

				// @deprecated
				"p90LatencySecs": metrics.ResponseQuantiles.GetQuantile(0.90).Seconds(),
				"p95LatencySecs": metrics.ResponseQuantiles.GetQuantile(0.95).Seconds(),
				"p99LatencySecs": metrics.ResponseQuantiles.GetQuantile(0.99).Seconds(),
			},
		}
	}

	if p.logger.GetLevel() == zerolog.TraceLevel {
		p.logger.Debug().Str("method", method).Interface("upstreams", metricsData).Msg("evaluating selection policy function")
	}

	// Call user-defined evaluation function
	p.evalMutex.Lock()
	defer p.evalMutex.Unlock()

	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"selection-policy-eval",
				fmt.Sprintf("network:%s method:%s", p.networkId, method),
				common.ErrorFingerprint(rec),
			).Inc()
			p.logger.Error().
				Str("method", method).
				Interface("upstreams", metricsData).
				Interface("panic", rec).
				Str("stack", string(debug.Stack())).
				Msg("unexpected panic in user-defined selection policy function")
		}
	}()

	result, err := p.config.EvalFunction(nil, p.runtime.ToValue(metricsData), p.runtime.ToValue(method))
	if err != nil {
		return fmt.Errorf("failed to evaluate selection policy: %w", err)
	}

	// Process results and update states
	selectedUpstreams := make(map[string]bool)
	exp := result.Export()

	if p.logger.GetLevel() <= zerolog.TraceLevel {
		p.logger.Trace().Str("method", method).Interface("result", exp).Msg("received evalFunction result for selection policy")
	}

	var arr []interface{}

	if a, ok := exp.([]metricData); ok {
		for _, v := range a {
			arr = append(arr, v)
		}
	} else if !ok {
		if a, ok := exp.([]interface{}); ok {
			arr = a
		} else {
			return fmt.Errorf("unexpected return value from evalFunction, expected an array of upstreams: %v", result)
		}
	}

	for _, v := range arr {
		ups, ok := v.(metricData)
		if !ok {
			ups, ok = v.(map[string]interface{})
			if !ok {
				return fmt.Errorf("unexpected return value from evalFunction, expected objects inside the returned array: %+v raw value: %+v full result: %+v", ups, v, result)
			}
		}
		if upstreamId, ok := ups["id"].(string); ok {
			selectedUpstreams[upstreamId] = true
		} else {
			return fmt.Errorf("unexpected return value from evalFunction, expected a string 'id' key in each object of returned array: %+v raw value: %+v full result: %+v", ups, v, result)
		}
	}

	if p.logger.GetLevel() <= zerolog.TraceLevel {
		p.logger.Trace().Str("method", method).Interface("selectedUpstreams", selectedUpstreams).Msg("finished evaluating selection policy")
	}

	// Update states based on evaluation
	now := time.Now()
	stateMap := p.getStateMap(method)

	for _, ups := range upsList {
		id := ups.Id()
		state, exists := stateMap[id]
		if !exists {
			state = &upstreamState{
				mu:       &sync.RWMutex{},
				isActive: true,
			}
			stateMap[id] = state
		}

		state.mu.Lock()

		if selectedUpstreams[id] {
			state.isActive = true
			state.sampleCounter = 0
			state.resampleInterval = time.Time{}
		} else {
			if state.isActive {
				// Newly deactivated
				state.isActive = false
				state.resampleInterval = now.Add(p.config.ResampleInterval.Duration())
				state.sampleCounter = p.config.ResampleCount
			}
		}

		state.lastEvalTime = now

		// Update tracker state
		if !state.isActive {
			p.metricsTracker.Cordon(ups, method, "excluded by selection policy")
		} else {
			p.metricsTracker.Uncordon(ups, method)
		}

		state.mu.Unlock()
	}

	return nil
}

func (p *PolicyEvaluator) getStateMap(method string) map[string]*upstreamState {
	if p.config.EvalPerMethod {
		if _, exists := p.methodStates[method]; !exists {
			p.methodStates[method] = make(map[string]*upstreamState)
		}
		return p.methodStates[method]
	}
	return p.globalState
}

func (p *PolicyEvaluator) AcquirePermit(logger *zerolog.Logger, ups common.Upstream, method string) error {
	// First check method-specific state if enabled
	if p.config.EvalPerMethod {
		if permit := p.checkPermitForMethod(ups.Id(), method); permit {
			return nil
		}
		// If method-specific check failed, fall back to checking global (*) method state
		if permit := p.checkPermitForMethod(ups.Id(), "*"); permit {
			return nil
		}
	} else {
		// Only check global state
		if permit := p.checkPermitForMethod(ups.Id(), "*"); permit {
			return nil
		}
	}

	logger.Debug().
		Str("upstreamId", ups.Id()).
		Str("method", method).
		Msg("upstream excluded by selection policy")

	return common.NewErrUpstreamExcludedByPolicy(ups.Id())
}

func (p *PolicyEvaluator) checkPermitForMethod(upstreamId string, method string) bool {
	p.upstreamsMu.RLock()
	var state *upstreamState

	if p.config.EvalPerMethod {
		if methodStates, exists := p.methodStates[method]; exists {
			state = methodStates[upstreamId]
		}
	} else {
		state = p.globalState[upstreamId]
	}
	p.upstreamsMu.RUnlock()

	if state == nil {
		// If we haven't evaluated this upstream yet, consider it active
		return true
	}

	state.mu.RLock()
	if state.isActive {
		state.mu.RUnlock()
		return true
	}

	if !p.config.ResampleExcluded {
		state.mu.RUnlock()
		return false
	}

	// Check if we should allow sampling
	now := time.Now()
	if now.After(state.resampleInterval) && state.sampleCounter > 0 {
		// Switch to write lock to update counter
		state.mu.RUnlock()
		state.mu.Lock()
		// Double-check conditions after acquiring write lock
		if now.After(state.resampleInterval) && state.sampleCounter > 0 {
			state.sampleCounter--
			if state.sampleCounter < 1 {
				state.sampleCounter = p.config.ResampleCount
				state.resampleInterval = now.Add(p.config.ResampleInterval.Duration())
			}
			state.mu.Unlock()
			return true
		}
		state.mu.Unlock()
	} else {
		state.mu.RUnlock()
	}

	return false
}

func (p *PolicyEvaluator) GetLastEvalTime(upstreamId string, method string) time.Time {
	p.upstreamsMu.RLock()
	defer p.upstreamsMu.RUnlock()

	var state *upstreamState

	if p.config.EvalPerMethod {
		if methodStates, exists := p.methodStates[method]; exists {
			state = methodStates[upstreamId]
		}
		// If no method-specific state, try global state
		if state == nil {
			if methodStates, exists := p.methodStates["*"]; exists {
				state = methodStates[upstreamId]
			}
		}
	} else {
		state = p.globalState[upstreamId]
	}

	if state == nil {
		return time.Time{}
	}

	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.lastEvalTime
}
