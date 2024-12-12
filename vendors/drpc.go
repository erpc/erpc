package vendors

import (
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

var FALSE = false

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
		} else if strings.Contains(msg, "does not exist/is not available") {
			// Intentionally consider missing methods as client-side exceptions
			// because dRPC might give a false error when their underlying nodes
			// have issues e.g. you might falsely get "eth_blockNumber not supported" errors.
			// The reason we don't consider this as server-side exceptions is because
			// we don't want to trigger the circuit breaker in such cases.
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnsupportedException,
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
