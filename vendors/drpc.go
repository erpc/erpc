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

func (v *DrpcVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if code := err.Code; code != 0 {
		msg := err.Message

		if code == 4 && strings.Contains(msg, "token is invalid") {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnauthorized,
					msg,
					nil,
				),
			)
		}
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *DrpcVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.Contains(ups.Endpoint, ".drpc.org")
}
