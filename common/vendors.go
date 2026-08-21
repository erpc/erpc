package common

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/rs/zerolog"
)

type Vendor interface {
	Name() string
	OwnsUpstream(upstream *UpstreamConfig) bool
	GenerateConfigs(ctx context.Context, logger *zerolog.Logger, baseConfig *UpstreamConfig, settings VendorSettings) ([]*UpstreamConfig, error)
	SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings VendorSettings, networkId string) (bool, error)
	GetVendorSpecificErrorIfAny(req *NormalizedRequest, resp *http.Response, bodyObject interface{}, details map[string]interface{}) error
}

// VendorSettingsBuilder translates an upstream endpoint written in a vendor's
// `<vendor>://…` shorthand into the VendorSettings that same vendor reads back
// at config-generation time. Both ends of that translation belong to the
// vendor, so the parser lives next to the vendor implementation
// (thirdparty/<vendor>.go) rather than in a table here — config loading only
// needs to know THAT a vendor claims a scheme, never which vendors exist.
type VendorSettingsBuilder func(endpoint *url.URL) (VendorSettings, error)

// vendorSettingsBuilders is the catalog of shorthand parsers keyed by vendor
// name, registered by the thirdparty package at init time (common cannot
// import it — that would be a cycle). Same shape as the integrity check id
// catalog in validation.go. Registration happens during package init, before
// any config is parsed, so no locking is needed.
var vendorSettingsBuilders = map[string]VendorSettingsBuilder{}

// RegisterVendorSettingsBuilder registers a vendor's endpoint shorthand parser
// under its Name(). Call it from the vendor file's init() — a build that links
// the vendor can then resolve its shorthand without any VendorsRegistry having
// been constructed yet (config defaults run long before that).
func RegisterVendorSettingsBuilder(vendorName string, build VendorSettingsBuilder) {
	vendorSettingsBuilders[vendorName] = build
}

// buildProviderSettings resolves the `<vendor>://…` upstream endpoint shorthand
// into provider settings, by handing the parsed URL to whichever vendor
// registered that name. `evm+<vendor>` is an alias of the bare vendor name.
func buildProviderSettings(vendorName string, endpoint *url.URL) (VendorSettings, error) {
	build, ok := vendorSettingsBuilders[vendorName]
	if !ok {
		build, ok = vendorSettingsBuilders[strings.TrimPrefix(vendorName, "evm+")]
	}
	if !ok {
		return nil, fmt.Errorf("unsupported vendor name in vendor.settings: %s", vendorName)
	}
	return build(endpoint)
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
