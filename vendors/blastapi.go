package vendors

import (
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

type BlastApiVendor struct {
	common.Vendor
}

func CreateBlastApiVendor() common.Vendor {
	return &BlastApiVendor{}
}

func (v *BlastApiVendor) Name() string {
	return "blastapi"
}

func (v *BlastApiVendor) OverrideConfig(upstream *common.UpstreamConfig) error {
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

func (v *BlastApiVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if err.Data != "" {
		details["data"] = err.Data
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *BlastApiVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.Contains(ups.Endpoint, ".blastapi.io")
}
