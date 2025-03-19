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

func (v *QuicknodeVendor) Name() string {
	return "quicknode"
}

func (v *QuicknodeVendor) GenerateConfigs(upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		return nil, fmt.Errorf("quicknode vendor requires upstream.endpoint to be defined")
	}

	return []*common.UpstreamConfig{upstream}, nil
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

		if (code == -32614 && strings.Contains(msg, "eth_getLogs") && strings.Contains(msg, "limited")) ||
			strings.Contains(msg, "eth_getLogs and eth_newFilter are limited") {
			return common.NewErrEndpointRequestTooLarge(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorEvmLargeRange, msg, nil, details),
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
			// We do not retry on parse errors, as retrying another upstream would not help.
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorParseException, msg, nil, details),
			).WithRetryableTowardNetwork(false)
		} else if code == -32010 { // Transaction cost exceeds current gas limit
			// retrying on gas limit exceeded errors toward other upstreams would be helpful, as max gas limit
			// can be defined per client (reth, geth, parity, etc.) (still needs to be lower than overall block gas limit)
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorClientSideException, msg, nil, details),
			)
		} else if code == -32602 && strings.Contains(msg, "cannot unmarshal hex string") {
			// we do not retry on invalid argument errors, as retrying another upstream would not help.
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorInvalidArgument, msg, nil, details),
			).WithRetryableTowardNetwork(false)
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
			return common.NewErrEndpointExecutionException(
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
