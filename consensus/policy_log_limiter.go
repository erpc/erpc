package consensus

import (
	"sync"
	"time"
)

// policyLogLimiter rate-limits repeated Warn logs from the consensus-policy
// selector so a failing eval under load emits once per (project, network,
// reason) per window instead of once per request (review finding). The
// `consensus_policy_eval_failed_total` metric still counts every occurrence.
type policyLogLimiter struct {
	mu     sync.Mutex
	window time.Duration
	last   map[string]time.Time
}

func newPolicyLogLimiter(window time.Duration) *policyLogLimiter {
	if window <= 0 {
		window = 5 * time.Second
	}
	return &policyLogLimiter{window: window, last: make(map[string]time.Time)}
}

// allow reports whether this (project, network, reason) may log now.
func (l *policyLogLimiter) allow(projectId, networkId, reason string) bool {
	if l == nil {
		return true
	}
	key := projectId + "|" + networkId + "|" + reason
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[key]; ok && now.Sub(t) < l.window {
		return false
	}
	l.last[key] = now
	return true
}
