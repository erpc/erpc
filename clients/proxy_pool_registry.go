package clients

import (
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"

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
func (p *ProxyPool) GetClient() (*http.Client, error) {
	if len(p.clients) == 0 {
		return nil, fmt.Errorf("ProxyPool '%s' has no clients registered", p.ID)
	}
	// Round-robin client selection
	idx := atomic.AddUint64(&p.counter, 1) % uint64(len(p.clients))
	return p.clients[idx], nil
}

// holds all pools
type ProxyPoolRegistry struct {
	logger *zerolog.Logger
	pools  map[string]*ProxyPool
}

// creates a new registry and initializes each pool.
func NewProxyPoolRegistry(
	cfg []*common.ProxyPoolConfig,
	logger *zerolog.Logger,
) (*ProxyPoolRegistry, error) {
	r := &ProxyPoolRegistry{
		logger: logger,
		pools:  make(map[string]*ProxyPool),
	}
	if len(cfg) == 0 {
		r.logger.Debug().Msg("no proxy pools defined, all outgoing requests will go direct to upstreams")
		return r, nil
	}

	// Initialize each proxy pool
	for _, poolCfg := range cfg {
		pool, err := createProxyPool(*poolCfg)
		if err != nil {
			return nil, err
		}
		r.pools[poolCfg.ID] = pool

		resolved := poolCfg.HTTPClientTimeouts.Resolve()
		logger.Debug().
			Str("poolId", poolCfg.ID).
			Int("clientCount", len(pool.clients)).
			Dur("timeout", resolved.Timeout).
			Dur("responseHeaderTimeout", resolved.ResponseHeaderTimeout).
			Dur("tlsHandshakeTimeout", resolved.TLSHandshakeTimeout).
			Dur("idleConnTimeout", resolved.IdleConnTimeout).
			Dur("expectContinueTimeout", resolved.ExpectContinueTimeout).
			Msg("proxy pool created with timeout configuration")
	}

	return r, nil
}

// creates a ProxyPool from a given config, building an http.Client for each URL
func createProxyPool(poolCfg common.ProxyPoolConfig) (*ProxyPool, error) {
	if len(poolCfg.Urls) == 0 {
		return &ProxyPool{ID: poolCfg.ID}, fmt.Errorf("no proxy URLs defined for pool '%s'. at least one proxy URL is required", poolCfg.ID)
	}

	resolved := poolCfg.HTTPClientTimeouts.Resolve()
	clients := make([]*http.Client, 0, len(poolCfg.Urls))

	for _, proxyStr := range poolCfg.Urls {
		proxyURL, err := url.Parse(proxyStr)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL '%s' in pool '%s': %w", proxyStr, poolCfg.ID, err)
		}

		transport := &http.Transport{
			MaxIdleConns:          1024,
			MaxIdleConnsPerHost:   256,
			MaxConnsPerHost:       0, // Unlimited active connections (prevents bottleneck)
			IdleConnTimeout:       resolved.IdleConnTimeout,
			ResponseHeaderTimeout: resolved.ResponseHeaderTimeout,
			TLSHandshakeTimeout:   resolved.TLSHandshakeTimeout,
			ExpectContinueTimeout: resolved.ExpectContinueTimeout,
			Proxy:                 http.ProxyURL(proxyURL),
		}
		client := &http.Client{
			Timeout:   resolved.Timeout,
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

	if pool, exists := r.pools[poolID]; exists {
		return pool, nil
	}
	return nil, fmt.Errorf("no proxy pool found with ID '%s'", poolID)
}
