package common

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"

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

// UniqueUpstreamKey returns a stable hash for an upstream, derived only from
// config fields that don't change after the upstream is constructed.
//
// Why: this key is the dedup key for the per-upstream client cache
// (clients/registry.go) and for shared-state counters (latestBlock,
// finalizedBlock, earliestBlock by probe). If the key changes during the
// upstream's lifetime, those caches are bypassed — every key flip leaks a
// client (with its goroutines) and a counter-sync goroutine, and a fresh
// pod's view of the latest/finalized block diverges from the cluster's.
//
// Two prior bugs we fix here:
//  1. up.NetworkId() returns "n/a" before registration and the real id
//     after. The endpoint already disambiguates which network the upstream
//     points at, so NetworkId is redundant — and including it changed the
//     key mid-lifetime.
//  2. Iterating cfg.JsonRpc.Headers in map order is non-deterministic, so
//     two calls in the same process could hash to different values for the
//     same headers. Sort by key before hashing.
func UniqueUpstreamKey(up Upstream) string {
	sha := sha256.New()
	cfg := up.Config()

	sha.Write([]byte(cfg.Id))
	sha.Write([]byte(cfg.Endpoint))
	if cfg.JsonRpc != nil && len(cfg.JsonRpc.Headers) > 0 {
		keys := make([]string, 0, len(cfg.JsonRpc.Headers))
		for k := range cfg.JsonRpc.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sha.Write([]byte(k))
			sha.Write([]byte(cfg.JsonRpc.Headers[k]))
		}
	}

	return cfg.Id + "/" + hex.EncodeToString(sha.Sum(nil))
}
