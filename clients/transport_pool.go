package clients

import (
	"net/http"
	"sync"
	"time"

	"github.com/erpc/erpc/util"
)

// sharedTransportPool is the process-wide pool of *http.Transport instances,
// keyed by common.UniqueUpstreamKey. Multiple eRPC projects that configure the
// SAME upstream endpoint (identical id + endpoint + headers) reuse ONE
// connection pool instead of each allocating a full http.Transport — with its
// idle-connection pool (MaxIdleConns=1024, MaxIdleConnsPerHost=256), TLS
// session cache, and per-host buffers — per project.
//
// This is the co-resident multi-project deduplication: without it, N projects
// pointing at the same endpoint hold N independent connection pools to it.
// http.Transport is safe for concurrent use by many goroutines, so the sharing
// is transparent to every caller.
var sharedTransportPool = NewTransportPool()

// TransportPool hands out shared *http.Transport instances keyed by an opaque
// string (the upstream's UniqueUpstreamKey). Distinct keys get distinct
// transports, so per-upstream connection-pool isolation WITHIN a project is
// unchanged; the pool only deduplicates transports ACROSS projects (or any
// other callers) that resolve to the identical upstream key.
type TransportPool struct {
	mu         sync.Mutex
	transports map[string]*http.Transport
}

// NewTransportPool returns an empty pool. The package uses a single global
// instance (sharedTransportPool); this constructor is exported so tests can
// exercise the pool in isolation.
func NewTransportPool() *TransportPool {
	return &TransportPool{transports: make(map[string]*http.Transport)}
}

// GetOrCreate returns the shared transport for key, lazily creating the standard
// high-RPS transport on first use. The transport tuning is identical for every
// upstream, so callers only ever vary the key (the sharing granularity), never
// the shape of the transport.
func (p *TransportPool) GetOrCreate(key string) *http.Transport {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.transports[key]; ok {
		return t
	}
	t := newDefaultTransport()
	p.transports[key] = t
	return t
}

// size reports how many distinct transports the pool currently holds. Used by
// tests to assert deduplication.
func (p *TransportPool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.transports)
}

// newDefaultTransport builds the standard non-proxy outbound transport. It is
// the single source of truth for the tuning; the pool caches one per upstream
// key.
//
// Optimized for high-latency, high-RPS scenarios to prevent connection churn.
// DialContext via util.DefaultOutboundDialer enables kernel-level TCP keepalive
// so wedged outbound flows are detected within ~45s (3 missed probes) instead of
// the OS default tcp_keepalive_time of 2h on Linux.
func newDefaultTransport() *http.Transport {
	return &http.Transport{
		DialContext:           util.DefaultOutboundDialer().DialContext,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   256,
		MaxConnsPerHost:       0, // Unlimited active connections (prevents bottleneck)
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}
