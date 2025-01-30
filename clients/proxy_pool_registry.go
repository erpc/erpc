package clients

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// contains a set of http.Clients (each configured with a different proxy).
type ProxyPool struct {
	ID      string
	clients []*http.Client
	counter uint64
}

// returns a round-robin client from the pool.
func (p *ProxyPool) GetClient() *http.Client {
	if len(p.clients) == 0 {
		// Fallback: if for some reason the pool has zero clients
		return &http.Client{Timeout: 60 * time.Second}
	}
	// Round-robin client selection
	idx := atomic.AddUint64(&p.counter, 1) % uint64(len(p.clients))
	return p.clients[idx]
}

// holds all pools
type ProxyPoolRegistry struct {
	logger *zerolog.Logger
	mu     sync.RWMutex
	pools  map[string]*ProxyPool
}

// NewProxyPoolRegistry creates a new registry and initializes each pool.
func NewProxyPoolRegistry(
	cfg []common.ProxyPoolConfig,
	logger *zerolog.Logger,
) (*ProxyPoolRegistry, error) {
	r := &ProxyPoolRegistry{
		logger: logger,
		pools:  make(map[string]*ProxyPool),
	}
	if len(cfg) == 0 {
		r.logger.Warn().Msg("no proxy pools defined; all requests will go direct")
		return r, nil
	}

	// Initialize each proxy pool
	for _, poolCfg := range cfg {
		pool, err := createProxyPool(poolCfg)
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.pools[poolCfg.ID] = pool
		r.mu.Unlock()
		logger.Debug().
			Str("poolId", poolCfg.ID).
			Int("clientCount", len(pool.clients)).
			Msg("proxy pool created")
	}

	return r, nil
}

// creates a ProxyPool from a given config, building an http.Client for each URL
func createProxyPool(poolCfg common.ProxyPoolConfig) (*ProxyPool, error) {
	if len(poolCfg.Urls) == 0 {
		return &ProxyPool{ID: poolCfg.ID}, nil
	}

	clients := make([]*http.Client, 0, len(poolCfg.Urls))

	for _, proxyStr := range poolCfg.Urls {
		proxyURL, err := url.Parse(proxyStr)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL '%s' in pool '%s': %w", proxyStr, poolCfg.ID, err)
		}

		transport := &http.Transport{
			MaxIdleConns:        1024,
			MaxIdleConnsPerHost: 256,
			IdleConnTimeout:     90 * time.Second,
			Proxy:               http.ProxyURL(proxyURL),
		}
		client := &http.Client{
			Timeout:   60 * time.Second,
			Transport: transport,
		}
		clients = append(clients, client)
	}

	return &ProxyPool{
		ID:      poolCfg.ID,
		clients: clients,
	}, nil
}

// returns the ProxyPool for the given pool ID, or an error if not found.
func (r *ProxyPoolRegistry) GetPool(poolID string) (*ProxyPool, error) {
	if poolID == "" {
		// If no proxy is configured, return nil to indicate direct requests.
		return nil, nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if pool, exists := r.pools[poolID]; exists {
		return pool, nil
	}
	return nil, fmt.Errorf("no proxy pool found with ID '%s'", poolID)
}
