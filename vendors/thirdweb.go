package vendors

import (
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

type ThirdwebVendor struct {
	common.Vendor
}

func CreateThirdwebVendor() common.Vendor {
	return &ThirdwebVendor{}
}

func (v *ThirdwebVendor) Name() string {
	return "thirdweb"
}

func (v *ThirdwebVendor) OverrideConfig(upstream *common.UpstreamConfig) error {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.JsonRpc.SupportsBatch == nil {
		upstream.JsonRpc.SupportsBatch = &TRUE
		upstream.JsonRpc.BatchMaxWait = "100ms"
		upstream.JsonRpc.BatchMaxSize = 100
	}

	return nil
}

func (v *ThirdwebVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *ThirdwebVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "thirdweb://") || strings.HasPrefix(ups.Endpoint, "evm+thirdweb://") {
		return true
	}

	return strings.Contains(ups.Endpoint, ".thirdweb.com")
}
