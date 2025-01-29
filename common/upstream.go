package common

import (
	"context"

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

type Upstream interface {
	Config() *UpstreamConfig
	Logger() *zerolog.Logger
	Vendor() Vendor
	NetworkId() string
	Forward(ctx context.Context, nq *NormalizedRequest, byPassMethodExclusion bool) (*NormalizedResponse, error)
}
