package data

import (
	"fmt"
	"time"

	"github.com/erpc/erpc/common"
)

type CachePolicy struct {
	config    *common.CachePolicyConfig
	connector Connector
	ttl       *time.Duration
}

func NewCachePolicy(cfg *common.CachePolicyConfig, connector Connector) (*CachePolicy, error) {
	var ttl *time.Duration
	if cfg.TTL != "" {
		d, err := time.ParseDuration(cfg.TTL)
		if err != nil {
			return nil, fmt.Errorf("invalid TTL duration: %w", err)
		}
		ttl = &d
	}

	return &CachePolicy{
		config:    cfg,
		connector: connector,
		ttl:       ttl,
	}, nil
}

func (p *CachePolicy) Matches(networkId, method string, isFinalized bool) bool {
	if !common.WildcardMatch(p.config.Network, networkId) {
		return false
	}

	if !common.WildcardMatch(p.config.Method, method) {
		return false
	}

	// Match finality
	switch p.config.RequiredFinality {
	case common.DataFinalityStateUnfinalized:
		return !isFinalized
	case common.DataFinalityStateFinalized:
		return isFinalized
	case common.DataFinalityStateUnknown:
		return true
	default:
		return false
	}
}

func (p *CachePolicy) GetConnector() Connector {
	return p.connector
}

func (p *CachePolicy) GetTTL() *time.Duration {
	return p.ttl
}
