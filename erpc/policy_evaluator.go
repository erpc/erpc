package erpc

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
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

type stateEntry struct {
	upstream common.Upstream
	state    *upstreamState
}

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
		evalErrs := make([]error, 0)
		methodCount := 0
		for method := range allMetrics {
			methodCount++
			if err := p.evaluateMethod(method, upsList); err != nil {
				p.logger.Error().Err(err).Str("method", method).Msg("failed to evaluate user-defined selectionPolicy for method")
				evalErrs = append(evalErrs, fmt.Errorf("%s: %w", method, err))
			}
		}
		if len(evalErrs) > 0 {
			joined := errors.Join(evalErrs...)
			if len(evalErrs) == methodCount {
				return fmt.Errorf("failed to evaluate user-defined selectionPolicy for all methods (%d): %w", methodCount, joined)
			}
			return fmt.Errorf("failed to evaluate user-defined selectionPolicy for %d/%d methods: %w", len(evalErrs), methodCount, joined)
		}
	} else {
		// Handle network-level evaluation
		if err := p.evaluateMethod("*", upsList); err != nil {
			p.logger.Error().Err(err).Msg("failed to evaluate user-defined selectionPolicy for network")
			return fmt.Errorf("failed to evaluate user-defined selectionPolicy for network: %w", err)
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

	var (
		selectedUpstreams map[string]bool
		err               error
	)
	if p.config.UsesEvalFunction() {
		selectedUpstreams, err = p.evaluateWithFunction(method, metricsData)
	} else {
		selectedUpstreams, err = p.evaluateWithRules(method, metricsData)
	}
	if err != nil {
		return err
	}

	if p.logger.GetLevel() <= zerolog.TraceLevel {
		p.logger.Trace().Str("method", method).Interface("selectedUpstreams", selectedUpstreams).Msg("finished evaluating selection policy")
	}

	// Update states based on evaluation
	now := time.Now()
	stateEntries := p.getOrCreateStateEntries(method, upsList)

	for _, entry := range stateEntries {
		isSelected := selectedUpstreams[entry.upstream.Id()]
		isActive := p.updateState(entry.state, isSelected, now)

		// Tracker updates can be slow; keep them outside shared evaluator locks.
		if !isActive {
			p.metricsTracker.Cordon(entry.upstream, method, "excluded by selection policy")
		} else {
			p.metricsTracker.Uncordon(entry.upstream, method, "included by selection policy")
		}
	}

	return nil
}

func (p *PolicyEvaluator) evaluateWithFunction(method string, metricsData []metricData) (selectedUpstreams map[string]bool, err error) {
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
			err = fmt.Errorf("panic while evaluating selection policy: %v", rec)
		}
	}()

	result, evalErr := p.config.EvalFunction(nil, p.runtime.ToValue(metricsData), p.runtime.ToValue(method))
	if evalErr != nil {
		return nil, fmt.Errorf("failed to evaluate selection policy: %w", evalErr)
	}

	exp := result.Export()
	if p.logger.GetLevel() <= zerolog.TraceLevel {
		p.logger.Trace().Str("method", method).Interface("result", exp).Msg("received evalFunction result for selection policy")
	}

	selectedUpstreams = make(map[string]bool)
	var arr []interface{}

	if a, ok := exp.([]metricData); ok {
		for _, v := range a {
			arr = append(arr, v)
		}
	} else if !ok {
		if a, ok := exp.([]interface{}); ok {
			arr = a
		} else {
			return nil, fmt.Errorf("unexpected return value from evalFunction, expected an array of upstreams: %v", result)
		}
	}

	for _, v := range arr {
		ups, ok := v.(metricData)
		if !ok {
			ups, ok = v.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("unexpected return value from evalFunction, expected objects inside the returned array: %+v raw value: %+v full result: %+v", ups, v, result)
			}
		}
		if upstreamId, ok := ups["id"].(string); ok {
			selectedUpstreams[upstreamId] = true
		} else {
			return nil, fmt.Errorf("unexpected return value from evalFunction, expected a string 'id' key in each object of returned array: %+v raw value: %+v full result: %+v", ups, v, result)
		}
	}

	return selectedUpstreams, err
}

func (p *PolicyEvaluator) evaluateWithRules(method string, metricsData []metricData) (map[string]bool, error) {
	selectedUpstreams := make(map[string]bool, len(metricsData))
	for _, md := range metricsData {
		if upstreamID, ok := md["id"].(string); ok && upstreamID != "" {
			selectedUpstreams[upstreamID] = true
		}
	}

	for _, rule := range p.config.Rules {
		if rule == nil {
			continue
		}
		matchMethod := strings.TrimSpace(rule.MatchMethod)
		if matchMethod == "" {
			matchMethod = "*"
		}
		methodMatched, err := common.WildcardMatch(matchMethod, method)
		if err != nil {
			return nil, fmt.Errorf("selection policy rule has invalid matchMethod pattern %q: %w", matchMethod, err)
		}
		if !methodMatched {
			continue
		}

		action := strings.ToLower(strings.TrimSpace(string(rule.Action)))
		if action == "" {
			action = string(common.SelectionPolicyRuleActionInclude)
		}
		exclude := action == string(common.SelectionPolicyRuleActionExclude)

		for _, md := range metricsData {
			upstreamID, _ := md["id"].(string)
			if upstreamID == "" {
				continue
			}
			matched, err := matchesDeclarativeRule(md, rule)
			if err != nil {
				return nil, err
			}
			if !matched {
				continue
			}
			selectedUpstreams[upstreamID] = !exclude
		}
	}

	return selectedUpstreams, nil
}

func matchesDeclarativeRule(md metricData, rule *common.SelectionPolicyRuleConfig) (bool, error) {
	if md == nil || rule == nil {
		return false, nil
	}

	if rule.MatchUpstreamID != "" {
		id, _ := md["id"].(string)
		if id == "" {
			return false, nil
		}
		matched, err := common.WildcardMatch(rule.MatchUpstreamID, id)
		if err != nil {
			return false, fmt.Errorf("selection policy rule has invalid matchUpstreamId pattern %q: %w", rule.MatchUpstreamID, err)
		}
		if !matched {
			return false, nil
		}
	}
	if rule.MatchUpstreamGroup != "" {
		upstreamCfg, _ := md["config"].(*common.UpstreamConfig)
		if upstreamCfg == nil {
			return false, nil
		}
		matched, err := common.WildcardMatch(rule.MatchUpstreamGroup, upstreamCfg.Group)
		if err != nil {
			return false, fmt.Errorf("selection policy rule has invalid matchUpstreamGroup pattern %q: %w", rule.MatchUpstreamGroup, err)
		}
		if !matched {
			return false, nil
		}
	}

	metrics, _ := md["metrics"].(map[string]interface{})
	if !withinMax(metrics, "errorRate", rule.MaxErrorRate) {
		return false, nil
	}
	if !withinMax(metrics, "blockHeadLag", rule.MaxBlockHeadLag) {
		return false, nil
	}
	if !withinMax(metrics, "finalizationLag", rule.MaxFinalizationLag) {
		return false, nil
	}
	if !withinMax(metrics, "p90ResponseSeconds", rule.MaxP90ResponseSeconds) {
		return false, nil
	}
	if !withinMax(metrics, "p95ResponseSeconds", rule.MaxP95ResponseSeconds) {
		return false, nil
	}
	if !withinMax(metrics, "p99ResponseSeconds", rule.MaxP99ResponseSeconds) {
		return false, nil
	}
	if !withinMax(metrics, "throttledRate", rule.MaxThrottledRate) {
		return false, nil
	}

	return true, nil
}

func withinMax(metrics map[string]interface{}, key string, threshold *float64) bool {
	if threshold == nil {
		return true
	}
	if metrics == nil {
		return false
	}

	raw, ok := metrics[key]
	if !ok {
		return false
	}

	switch v := raw.(type) {
	case float64:
		return v <= *threshold
	case float32:
		return float64(v) <= *threshold
	case int64:
		return float64(v) <= *threshold
	case int32:
		return float64(v) <= *threshold
	case int:
		return float64(v) <= *threshold
	case uint64:
		return float64(v) <= *threshold
	case uint32:
		return float64(v) <= *threshold
	case uint:
		return float64(v) <= *threshold
	default:
		return false
	}
}

func (p *PolicyEvaluator) getOrCreateStateEntries(method string, upsList []*upstream.Upstream) []stateEntry {
	p.upstreamsMu.Lock()
	defer p.upstreamsMu.Unlock()

	stateMap := p.getStateMap(method)
	stateEntries := make([]stateEntry, 0, len(upsList))

	for _, ups := range upsList {
		state, exists := stateMap[ups.Id()]
		if !exists {
			state = &upstreamState{
				mu:       &sync.RWMutex{},
				isActive: true,
			}
			stateMap[ups.Id()] = state
		}

		stateEntries = append(stateEntries, stateEntry{
			upstream: ups,
			state:    state,
		})
	}

	return stateEntries
}

func (p *PolicyEvaluator) updateState(state *upstreamState, isSelected bool, now time.Time) bool {
	state.mu.Lock()
	defer state.mu.Unlock()

	if isSelected {
		state.isActive = true
		state.sampleCounter = 0
		state.resampleInterval = time.Time{}
	} else if state.isActive {
		// Newly deactivated
		state.isActive = false
		// Only initialize resampling state when resampling is enabled
		if p.config.ResampleExcluded {
			state.resampleInterval = now.Add(p.config.ResampleInterval.Duration())
			state.sampleCounter = p.config.ResampleCount
		}
	}

	state.lastEvalTime = now

	return state.isActive
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
