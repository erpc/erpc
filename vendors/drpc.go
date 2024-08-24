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
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.JsonRpc.SupportsBatch == nil {
		upstream.JsonRpc.SupportsBatch = &TRUE
		upstream.JsonRpc.BatchMaxWait = "100ms"
		upstream.JsonRpc.BatchMaxSize = 3
	}

	// By default disable auto-ignore because free-tier plans of dRPC
	// might give wrong responses when using public nodes.
	if upstream.AutoIgnoreUnsupportedMethods == nil {
		upstream.AutoIgnoreUnsupportedMethods = &FALSE
	}

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

		if code == 4 && strings.Contains(msg, "token is invalid") {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnauthorized,
					msg,
					nil,
					details,
				),
			)
		} else if code == 32601 && strings.Contains(msg, "does not exist/is not available") {
			// Intentionally consider missing methods as server-side exceptions
			// because dRPC might give a false error when their underlying nodes
			// have issues e.g. you might falsly get "eth_blockNumber not supported" errors.
			return common.NewErrEndpointServerSideException(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorServerSideException,
					msg,
					nil,
					details,
				),
				details,
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
