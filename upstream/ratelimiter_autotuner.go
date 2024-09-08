package upstream

import (
	"math"
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
	mu                 sync.RWMutex
}

type ErrorCounter struct {
	totalCount int
	errorCount int
	lastSeen   time.Time
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

	if _, exists := arl.errorCounts[method]; !exists {
		arl.errorCounts[method] = &ErrorCounter{}
	}

	arl.errorCounts[method].totalCount++
}

func (arl *RateLimitAutoTuner) RecordError(method string) {
	arl.mu.Lock()
	defer arl.mu.Unlock()

	if _, exists := arl.errorCounts[method]; !exists {
		arl.errorCounts[method] = &ErrorCounter{}
	}

	arl.errorCounts[method].totalCount++
	arl.errorCounts[method].errorCount++
	arl.errorCounts[method].lastSeen = time.Now()

	arl.adjustBudget(method)
}

func (arl *RateLimitAutoTuner) adjustBudget(method string) {
	lastAdjustment, exists := arl.lastAdjustments[method]
	if !exists || time.Since(lastAdjustment) >= arl.adjustmentPeriod {
		rules := arl.budget.GetRulesByMethod(method)
		for _, rule := range rules {
			currentMax := rule.Config.MaxCount
			erc := arl.errorCounts[method].errorCount
			ttc := arl.errorCounts[method].totalCount

			if ttc < 10 {
				continue
			}

			errorRate := float64(erc) / float64(ttc)

			var newMaxCount uint
			if errorRate > arl.errorRateThreshold {
				newMaxCount = uint(math.Ceil(float64(currentMax) * arl.decreaseFactor))
			} else if errorRate == 0 {
				newMaxCount = uint(math.Ceil(float64(currentMax) * arl.increaseFactor))
			} else {
				continue
			}

			err := arl.budget.AdjustBudget(rule, newMaxCount)
			if err != nil {
				arl.logger.Warn().Err(err).Msgf("failed to adjust budget for method %s", method)
			}
		}

		arl.lastAdjustments[method] = time.Now()
		arl.errorCounts[method].errorCount = 0
		arl.errorCounts[method].totalCount = 0
	}
}
