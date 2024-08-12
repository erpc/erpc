package upstream

import (
	"math"
	"sync"
	"time"
)

type RateLimitAutoTuner struct {
	budget           *RateLimiterBudget
	errorCounts      map[string]*ErrorCounter
	lastAdjustments  map[string]time.Time
	adjustmentPeriod time.Duration
	minBudget        int
	maxBudget        int
	mu               sync.RWMutex
}

type ErrorCounter struct {
	totalCount int
	errorCount int
	lastSeen   time.Time
}

func NewRateLimitAutoTuner(budget *RateLimiterBudget, adjustmentPeriod time.Duration, minBudget, maxBudget int) *RateLimitAutoTuner {
	return &RateLimitAutoTuner{
		budget:           budget,
		errorCounts:      make(map[string]*ErrorCounter),
		lastAdjustments:  make(map[string]time.Time),
		adjustmentPeriod: adjustmentPeriod,
		minBudget:        minBudget,
		maxBudget:        maxBudget,
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

			var newMaxCount int
			if errorRate > 0.1 {
				newMaxCount = int(math.Ceil(float64(currentMax) * 0.9))
			} else if errorRate == 0 {
				newMaxCount = int(math.Ceil(float64(currentMax) * 1.05))
			} else {
				continue
			}

			arl.budget.AdjustBudget(rule, newMaxCount)
		}

		arl.lastAdjustments[method] = time.Now()
		arl.errorCounts[method].errorCount = 0
		arl.errorCounts[method].totalCount = 0
	}
}
