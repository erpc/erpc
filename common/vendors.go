package common

import (
	"context"
	"net/http"

	"github.com/rs/zerolog"
)

type Vendor interface {
	Name() string
	OwnsUpstream(upstream *UpstreamConfig) bool
	PrepareConfig(upstream *UpstreamConfig, settings VendorSettings) error
	SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings VendorSettings, networkId string) (bool, error)
	GetVendorSpecificErrorIfAny(resp *http.Response, bodyObject interface{}, details map[string]interface{}) error
}
