package vendors

import (
	"net/http"
	"strings"

	"github.com/flair-sdk/erpc/common"
)

type InfuraVendor struct {
	common.Vendor
}

func CreateInfuraVendor() common.Vendor {
	return &InfuraVendor{}
}

func (v *InfuraVendor) Name() string {
	return "alchemy"
}

func (v *InfuraVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if code := err.Code; code != 0 {
		msg := err.Message

		if code == -32600 && (strings.Contains(msg, "be authenticated") || strings.Contains(msg, "access key")) {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnauthorized,
					msg,
					nil,
				),
			)
		} else if code == -32001 || code == -32004 {
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnsupportedException,
					msg,
					nil,
				),
			)
		} else if code == -32005 {
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorCapacityExceeded,
					msg,
					nil,
				),
			)
		}
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *InfuraVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.Contains(ups.Endpoint, ".alchemy.com") || strings.Contains(ups.Endpoint, ".alchemyapi.io")
}
