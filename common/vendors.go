package common

import (
	"context"
	"net/http"

	"github.com/rs/zerolog"
)

type Vendor interface {
	Name() string
	OwnsUpstream(upstream *UpstreamConfig) bool
	GenerateConfigs(ctx context.Context, logger *zerolog.Logger, baseConfig *UpstreamConfig, settings VendorSettings) ([]*UpstreamConfig, error)
	SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings VendorSettings, networkId string) (bool, error)
	GetVendorSpecificErrorIfAny(req *NormalizedRequest, resp *http.Response, bodyObject interface{}, details map[string]interface{}) error
}

// CreditUnitsProvider is an OPTIONAL capability a Vendor may implement to
// price upstream calls in its own credit units (Alchemy compute units,
// QuickNode API credits, …). It backs the opt-in cost accounting behind the
// X-ERPC-Credits response header: the upstream Forward path asks the vendor
// to price every physical attempt, so operators see the true upstream cost
// of a request (retries, hedges and consensus fan-out included; cache hits
// cost zero by construction).
//
// The VENDOR owns the pricing logic — nothing is hard-coded in the erpc
// layer. Most vendors resolve their publicly documented per-method table
// merged with the operator's per-method override (`upstream.CreditUnits`,
// populated from `providers[].settings.creditUnits`) via
// ResolveCreditUnits, but a vendor is free to price on anything it knows —
// request params, response classes, plan tiers, extra keys it reads from
// its settings at config-generation time. Values are the vendor's OWN
// units: deliberately not normalized, not comparable across vendors, not
// money. Vendors that do NOT implement this interface are costed at a flat
// 1 credit per request (opt out with `creditUnits: {"*": 0}`).
type CreditUnitsProvider interface {
	// CreditUnits prices ONE physical attempt of req against the given
	// upstream, in the vendor's own units. Called once per attempt by the
	// upstream Forward path when cost accounting is active.
	CreditUnits(req *NormalizedRequest, upstream *UpstreamConfig) int64
}

// ResolveCreditUnits is the shared table-resolution convention most
// CreditUnitsProvider implementations delegate to: the operator override
// wins per method over the vendor defaults, "*" is the per-table fallback
// for unlisted methods, and an entirely unpriced method costs a flat
// 1 credit per request (explicit "*": 0 opts out). Vendors remain free to
// price without it.
func ResolveCreditUnits(defaults, override map[string]int64, method string) int64 {
	if units, ok := override[method]; ok {
		return units
	}
	if units, ok := defaults[method]; ok {
		return units
	}
	if units, ok := override["*"]; ok {
		return units
	}
	if units, ok := defaults["*"]; ok {
		return units
	}
	return 1
}
