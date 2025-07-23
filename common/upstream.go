package common

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/rs/zerolog"
)

type Scope string

const (
	// Policies must be created with a "network" in mind,
	// assuming there will be many upstreams e.g. Retry might endup using a different upstream
	ScopeNetwork Scope = "network"

	// Policies must be created with one only "upstream" in mind
	// e.g. Retry with be towards the same upstream
	ScopeUpstream Scope = "upstream"
)

type UpstreamType string

type LoadBalancerType string

const (
	LoadBalancerTypeHighestScore       LoadBalancerType = "highestScore" // Original behavior - always use highest scored upstream
	LoadBalancerTypeWeightedRoundRobin LoadBalancerType = "weightedRoundRobin"
	LoadBalancerTypeRoundRobin         LoadBalancerType = "roundRobin"
	LoadBalancerTypeLeastConnection    LoadBalancerType = "leastConnection"
)

type Upstream interface {
	Id() string
	VendorName() string
	NetworkId() string
	Config() *UpstreamConfig
	Logger() *zerolog.Logger
	Vendor() Vendor
	Forward(ctx context.Context, nq *NormalizedRequest, byPassMethodExclusion bool) (*NormalizedResponse, error)
	Cordon(method string, reason string)
	Uncordon(method string)
	IgnoreMethod(method string)
}

// UniqueUpstreamKey returns a unique hash for an upstream.
// It is used to identify the upstream uniquely in shared-state storage.
// Sometimes ID might not be enough for example if user changes the endpoint to a completely different network.
func UniqueUpstreamKey(up Upstream) string {
	sha := sha256.New()
	cfg := up.Config()

	sha.Write([]byte(cfg.Id))
	sha.Write([]byte(cfg.Endpoint))
	sha.Write([]byte(up.NetworkId()))
	if cfg.JsonRpc != nil {
		for k, v := range cfg.JsonRpc.Headers {
			sha.Write([]byte(k))
			sha.Write([]byte(v))
		}
	}

	return cfg.Id + "/" + hex.EncodeToString(sha.Sum(nil))
}
