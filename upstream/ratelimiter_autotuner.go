package upstream

import (
	"sync"
	"time"

	"github.com/rs/zerolog"
)

type RateLimitAutoTuner struct {
	logger             *zerolog.Logger
	budget             *RateLimiterBudget
	errorCounts        map[string]*ErrorCounter
	lastAdjustments    map[string]time.Time
	adjustmentPeriod   time.Duration
	errorRateThreshold float64
	increaseFactor     float64
	decreaseFactor     float64
	minBudget          int
	maxBudget          int
	mu                 sync.Mutex
}

type ErrorCounter struct {
	totalCount int
	errorCount int
}

func NewRateLimitAutoTuner(
	logger *zerolog.Logger,
	budget *RateLimiterBudget,
	adjustmentPeriod time.Duration,
	errorRateThreshold,
	increaseFactor,
	decreaseFactor float64,
	minBudget,
	maxBudget int,
) *RateLimitAutoTuner {
	return &RateLimitAutoTuner{
		logger:             logger,
		budget:             budget,
		errorCounts:        make(map[string]*ErrorCounter),
		lastAdjustments:    make(map[string]time.Time),
		adjustmentPeriod:   adjustmentPeriod,
		errorRateThreshold: errorRateThreshold,
		increaseFactor:     increaseFactor,
		decreaseFactor:     decreaseFactor,
		minBudget:          minBudget,
		maxBudget:          maxBudget,
	}
}

func (arl *RateLimitAutoTuner) RecordSuccess(method string) {
	arl.mu.Lock()
	defer arl.mu.Unlock()

	arl.getOrCreateCounter(method).totalCount++
	arl.maybeAdjust(method)
}

func (arl *RateLimitAutoTuner) RecordError(method string) {
	arl.mu.Lock()
	defer arl.mu.Unlock()

	c := arl.getOrCreateCounter(method)
	c.totalCount++
	c.errorCount++
	arl.maybeAdjust(method)
}

func (arl *RateLimitAutoTuner) getOrCreateCounter(method string) *ErrorCounter {
	if c, exists := arl.errorCounts[method]; exists {
		return c
	}
	c := &ErrorCounter{}
	arl.errorCounts[method] = c
	return c
}

// maybeAdjust checks whether enough time has elapsed since the last adjustment
// for the given method and, if so, evaluates the error rate to scale the budget
// up or down. Must be called while arl.mu is held.
func (arl *RateLimitAutoTuner) maybeAdjust(method string) {
	lastAdj, exists := arl.lastAdjustments[method]
	if exists && time.Since(lastAdj) < arl.adjustmentPeriod {
		return
	}

	c := arl.errorCounts[method]
	ttc := c.totalCount
	erc := c.errorCount

	// Reset counters for next window regardless of whether we adjust.
	arl.lastAdjustments[method] = time.Now()
	c.totalCount = 0
	c.errorCount = 0

	if ttc < 10 {
		return
	}

	errorRate := float64(erc) / float64(ttc)

	var direction string
	if errorRate > arl.errorRateThreshold {
		direction = "decrease"
	} else if errorRate == 0 {
		direction = "increase"
	} else {
		return
	}

	rules, err := arl.budget.GetRulesByMethod(method)
	if err != nil {
		arl.logger.Warn().Err(err).Str("method", method).Msg("auto-tuner: failed to get rules")
		return
	}

	for _, rule := range rules {
		var factor float64
		if direction == "decrease" {
			factor = arl.decreaseFactor
		} else {
			factor = arl.increaseFactor
		}

		prev, next, changed := arl.budget.AdjustBudgetByFactor(rule, factor, arl.minBudget, arl.maxBudget)
		if !changed {
			continue
		}

		arl.logger.Info().
			Str("method", rule.Config.Method).
			Str("triggeredBy", method).
			Uint32("from", prev).
			Uint32("to", next).
			Float64("errorRate", errorRate).
			Int("samples", ttc).
			Str("direction", direction).
			Msg("auto-tuner: adjusting rate limit budget")
	}
}
