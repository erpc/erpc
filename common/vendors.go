package common

import (
	"net/http"
)

type Vendor interface {
	Name() string
	OwnsUpstream(upstream *UpstreamConfig) bool
	GetVendorSpecificErrorIfAny(resp *http.Response, bodyObject interface{}) error
}
