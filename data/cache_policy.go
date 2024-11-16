package data

import (
	"time"

	"github.com/erpc/erpc/common"
)

type CachePolicy struct {
	config    *common.CachePolicyConfig
	connector Connector
}

func NewCachePolicy(cfg *common.CachePolicyConfig, connector Connector) (*CachePolicy, error) {
	return &CachePolicy{
		config:    cfg,
		connector: connector,
	}, nil
}

func (p *CachePolicy) MatchesForSet(networkId, method string, finality common.DataFinalityState) bool {
	if !common.WildcardMatch(p.config.Network, networkId) {
		return false
	}

	if !common.WildcardMatch(p.config.Method, method) {
		return false
	}

	// TODO do we need to make unknown superset of finalized/unfinalized?
	return p.config.RequiredFinality == finality
}

func (p *CachePolicy) MatchesForGet(networkId, method string) bool {
	if !common.WildcardMatch(p.config.Network, networkId) {
		return false
	}

	if !common.WildcardMatch(p.config.Method, method) {
		return false
	}

	// When fetching data we need to iterate over all policies as we don't know finality of the data when originally written
	// We will iterate from first to last policy (matched on network/method) to see which one has the data
	// Therefore it is recommended to put the fastest most up-to-date policy first
	return true
}

func (p *CachePolicy) GetConnector() Connector {
	return p.connector
}

func (p *CachePolicy) GetTTL() *time.Duration {
	return &p.config.TTL
}
