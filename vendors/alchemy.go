package vendors

import (
	"net/http"
	"strings"

	"github.com/flair-sdk/erpc/common"
)

type AlchemyVendor struct {
	common.Vendor
}

func CreateAlchemyVendor() common.Vendor {
	return &AlchemyVendor{}
}

func (v *AlchemyVendor) Name() string {
	return "alchemy"
}

func (v *AlchemyVendor) GetVendorSpecificErrorIfAny(resp *http.Response, bodyObject interface{}) error {
	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *AlchemyVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.Contains(ups.Endpoint, ".alchemy.com") || strings.Contains(ups.Endpoint, ".alchemyapi.io")
}
