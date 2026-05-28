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

// HealthTracker is an interface for tracking upstream health metrics.
//
// `finality` is the DataFinalityState the request resolved to —
// `DataFinalityStateAll` when the caller can't determine finality
// (legacy call sites, internal probes, batch retries). Cordon/Uncordon
// stay finality-agnostic because cordoning is an admin-level decision
// about an entire (upstream, method) pair, not a per-finality bucket.
type HealthTracker interface {
	RecordUpstreamMisbehavior(up Upstream, method string, finality DataFinalityState)
	RecordUpstreamRequest(up Upstream, method string, finality DataFinalityState)
	RecordUpstreamFailure(up Upstream, method string, finality DataFinalityState, err error)
	Cordon(upstream Upstream, method string, reason string)
	Uncordon(upstream Upstream, method string, reason string)
}

type Upstream interface {
	Id() string
	VendorName() string
	NetworkId() string
	NetworkLabel() string
	Config() *UpstreamConfig
	Logger() *zerolog.Logger
	Vendor() Vendor
	Tracker() HealthTracker
	// Forward executes one attempt against this upstream. isHedgeAttempt
	// flags whether this call is a hedged speculative attempt (set by the
	// network layer where the hedge policy lives) — used to gate per-upstream
	// rate counters so hedges don't inflate them.
	Forward(ctx context.Context, nq *NormalizedRequest, byPassMethodExclusion, isHedgeAttempt bool) (*NormalizedResponse, error)
	Cordon(method string, reason string)
	Uncordon(method string, reason string)
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
	if cfg.JsonRpc != nil && cfg.JsonRpc.Headers != nil {
		for k, v := range cfg.JsonRpc.Headers {
			sha.Write([]byte(k))
			sha.Write([]byte(v))
		}
	}

	return cfg.Id + "/" + hex.EncodeToString(sha.Sum(nil))
}
