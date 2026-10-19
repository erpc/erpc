package common

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"

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

// CordonClass is the typed reason-category a cordon carries. The consensus
// selector matches on this class, never on the free-text cordon reason (which
// is diagnostic only), so the eval never couples to the message text of a
// cordon-producing call site. An upstream can hold several classes at once —
// the health tracker stores one flag per class (see health.Tracker), so
// classes do not overwrite each other and Uncordon clears only its own class.
//
// The class is a required parameter at every call site, so there is no runtime
// "unknown" class: a class added in the future must be mapped explicitly by the
// selector before it can ever permit a downgrade — fail closed by construction.
type CordonClass string

const (
	// CordonClassPunishment is set by the consensus executor's misbehavior
	// sitout. It BLOCKS role-gated fallback: a punished upstream anywhere pins
	// the request to the strictest policy.
	CordonClassPunishment CordonClass = "punishment"
	// CordonClassAvailability is set by the EVM state poller (chain-identity
	// mismatch) and the SVM state poller (lag / unhealthy). It ALLOWS role-gated
	// fallback — a genuine availability failure is the case fallback exists for.
	CordonClassAvailability CordonClass = "availability"
	// CordonClassOperator is set by the admin API. It BLOCKS fallback (fail
	// closed): an admin cordon is undifferentiated between maintenance and
	// distrust, so it is read as distrust.
	CordonClassOperator CordonClass = "operator"
)

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
	Cordon(upstream Upstream, method string, reason string, class CordonClass)
	Uncordon(upstream Upstream, method string, reason string, class CordonClass)
	// CordonClasses returns the set of cordon classes currently held for
	// (upstream, method), as the union of the exact-method scope and the
	// wildcard "*" scope. Empty means not cordoned. Read by the consensus
	// selector to build the eval context.
	CordonClasses(upstream Upstream, method string) []CordonClass
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
	Cordon(method string, reason string, class CordonClass)
	Uncordon(method string, reason string, class CordonClass)
	IgnoreMethod(method string)
	// ShouldHandleMethod reports whether this upstream is expected to serve the
	// given method (false when ignored/unsupported, e.g. via autoIgnoreUnsupportedMethods).
	ShouldHandleMethod(method string) (bool, error)
}

// UniqueUpstreamKey returns a unique hash for an upstream.
// It is used to identify the upstream uniquely in shared-state storage and the
// client cache, so it is derived from static config only (the network id is
// unknown until registration) with headers in sorted order.
func UniqueUpstreamKey(up Upstream) string {
	sha := sha256.New()
	cfg := up.Config()

	sha.Write([]byte(cfg.Id))
	sha.Write([]byte(cfg.Endpoint))
	if cfg.JsonRpc != nil {
		for _, k := range slices.Sorted(maps.Keys(cfg.JsonRpc.Headers)) {
			sha.Write([]byte(k))
			sha.Write([]byte(cfg.JsonRpc.Headers[k]))
		}
	}

	return cfg.Id + "/" + hex.EncodeToString(sha.Sum(nil))
}
