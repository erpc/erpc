package vendors

import (
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

type DrpcVendor struct {
	common.Vendor
}

func CreateDrpcVendor() common.Vendor {
	return &DrpcVendor{}
}

func (v *DrpcVendor) Name() string {
	return "drpc"
}

func (v *DrpcVendor) OverrideConfig(upstream *common.UpstreamConfig) error {
	// Intentionally not ignore missing method exceptions because dRPC sometimes routes to nodes that don't support the method
	// but it doesn't mean that method is actually not supported, i.e. on next retry to dRPC it might work.
	upstream.AutoIgnoreUnsupportedMethods = &common.FALSE
	return nil
}

func (v *DrpcVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if code := err.Code; code != 0 {
		msg := err.Message
		if err.Data != "" {
			details["data"] = err.Data
		}

		if strings.Contains(msg, "token is invalid") {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnauthorized,
					msg,
					nil,
					details,
				),
			)
		}
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *DrpcVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "drpc://") || strings.HasPrefix(ups.Endpoint, "evm+drpc://") {
		return true
	}

	return strings.Contains(ups.Endpoint, ".drpc.org")
}
