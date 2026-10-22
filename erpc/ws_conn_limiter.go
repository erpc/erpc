package erpc

import "sync"

// wsConnLimiter bounds concurrent client WebSocket connections on this
// instance, in total and per client IP; a zero limit is unlimited. A nil
// limiter admits everything.
type wsConnLimiter struct {
	mu       sync.Mutex
	total    int
	perIP    map[string]int
	maxTotal int
	maxPerIP int
}

const (
	wsRejectConnGlobal = "conn_global"
	wsRejectConnPerIP  = "conn_per_ip"
	wsRejectSubGlobal  = "sub_global"
)

func newWsConnLimiter(maxTotal, maxPerIP int) *wsConnLimiter {
	if maxTotal <= 0 && maxPerIP <= 0 {
		return nil
	}
	return &wsConnLimiter{perIP: make(map[string]int), maxTotal: maxTotal, maxPerIP: maxPerIP}
}

// acquire reserves a slot for ip, or returns the rejection reason. Every
// successful acquire must be paired with exactly one release(ip). An
// unresolved IP ("" or "n/a") is only subject to the total cap.
func (l *wsConnLimiter) acquire(ip string) (reason string, ok bool) {
	if l == nil {
		return "", true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.maxTotal > 0 && l.total >= l.maxTotal {
		return wsRejectConnGlobal, false
	}
	tracked := wsIPTracked(ip)
	if tracked && l.maxPerIP > 0 && l.perIP[ip] >= l.maxPerIP {
		return wsRejectConnPerIP, false
	}
	l.total++
	if tracked {
		l.perIP[ip]++
	}
	return "", true
}

func (l *wsConnLimiter) release(ip string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total > 0 {
		l.total--
	}
	if !wsIPTracked(ip) {
		return
	}
	// Delete at zero so a stream of distinct IPs can't grow the map.
	if n := l.perIP[ip]; n <= 1 {
		delete(l.perIP, ip)
	} else {
		l.perIP[ip] = n - 1
	}
}

func wsIPTracked(ip string) bool { return ip != "" && ip != "n/a" }
