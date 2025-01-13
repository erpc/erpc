package thirdparty

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

type QuicknodeVendor struct {
	common.Vendor
}

func CreateQuicknodeVendor() common.Vendor {
	return &QuicknodeVendor{}
}

type QuicknodeSettings struct{}

func (s *QuicknodeSettings) IsObjectNull() bool {
	return s == nil
}

func (s *QuicknodeSettings) Validate() error {
	return nil
}

func (v *QuicknodeVendor) Name() string {
	return "quicknode"
}

func (v *QuicknodeVendor) OverrideConfig(upstream *common.UpstreamConfig, settings common.VendorSettings) error {
	if upstream.Endpoint == "" && settings != nil && !settings.IsObjectNull() {
		return fmt.Errorf("quicknode vendor requires upstream.endpoint to be defined")
	}

	return nil
}

func (v *QuicknodeVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if code := err.Code; code != 0 {
		msg := err.Message
		var details map[string]interface{} = make(map[string]interface{})
		if err.Data != "" {
			details["data"] = err.Data
		}

		if code == -32602 && strings.Contains(msg, "eth_getLogs") && strings.Contains(msg, "limited") {
			return common.NewErrEndpointRequestTooLarge(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorEvmLogsLargeRange, msg, nil, details),
				common.EvmBlockRangeTooLarge,
			)
		} else if code == -32009 || code == -32007 {
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorCapacityExceeded, msg, nil, details),
			)
		} else if code == -32612 || code == -32613 {
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorCapacityExceeded, msg, nil, details),
			)
		} else if strings.Contains(msg, "failed to parse") {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorParseException, msg, nil, details),
			)
		} else if code == -32010 {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorClientSideException, msg, nil, details),
			)
		} else if code == -32602 {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorInvalidArgument, msg, nil, details),
			)
		} else if strings.Contains(msg, "UNAUTHORIZED") {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorUnauthorized, msg, nil, details),
			)
		} else if code == -32011 || code == -32603 {
			return common.NewErrEndpointServerSideException(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorServerSideException, msg, nil, details),
				nil,
			)
		} else if code == 3 {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorEvmReverted,
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

func (v *QuicknodeVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.Contains(ups.Endpoint, ".quiknode.pro")
}
