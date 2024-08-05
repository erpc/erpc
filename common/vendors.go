package common

import (
	"net/http"
)

type Vendor interface {
	Name() string
	OwnsUpstream(upstream *UpstreamConfig) bool
	OverrideConfig(upstream *UpstreamConfig) error
	GetVendorSpecificErrorIfAny(resp *http.Response, bodyObject interface{}) error
}
