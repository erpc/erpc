package common

import (
	"net/http"
)

type Vendor interface {
	Name() string
	OwnsUpstream(upstream *UpstreamConfig) bool
	OverrideConfig(upstream *UpstreamConfig) error
	SupportsNetwork(networkId string) (bool, error)
	GetVendorSpecificErrorIfAny(resp *http.Response, bodyObject interface{}, details map[string]interface{}) error
}
