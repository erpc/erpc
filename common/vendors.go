package common

import (
	"context"
	"net/http"
)

type Vendor interface {
	Name() string
	OwnsUpstream(upstream *UpstreamConfig) bool
	OverrideConfig(upstream *UpstreamConfig, settings VendorSettings) error
	SupportsNetwork(ctx context.Context, networkId string) (bool, error)
	GetVendorSpecificErrorIfAny(resp *http.Response, bodyObject interface{}, details map[string]interface{}) error
}
