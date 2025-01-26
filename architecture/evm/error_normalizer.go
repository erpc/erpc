package evm

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

func ExtractJsonRpcError(r *http.Response, nr *common.NormalizedResponse, jr *common.JsonRpcResponse) error {
	if jr != nil && jr.Error != nil {
		err := jr.Error

		var details map[string]interface{} = make(map[string]interface{})
		details["statusCode"] = r.StatusCode
		details["headers"] = util.ExtractUsefulHeaders(r)

		if ver := getVendorSpecificErrorIfAny(r, nr, jr, details); ver != nil {
			return ver
		}

		code := common.JsonRpcErrorNumber(err.Code)

		switch err.Data.(type) {
		case string:
			s := err.Data.(string)
			if s != "" {
				// Some providers such as Alchemy prefix the data with this string
				// we omit this prefix for standardization.
				if strings.HasPrefix(s, "Reverted ") {
					details["data"] = s[9:]
				} else {
					details["data"] = s
				}
			}
		default:
			// passthrough error data as is
			details["data"] = err.Data
		}

		// Infer from known status codes
		if r.StatusCode == 415 || code == common.JsonRpcErrorUnsupportedException {
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorUnsupportedException,
					err.Message,
					nil,
					details,
				),
			)
		} else if r.StatusCode == 429 ||
			strings.Contains(err.Message, "requests limited to") ||
			strings.Contains(err.Message, "has exceeded") ||
			strings.Contains(err.Message, "Exceeded the quota") ||
			strings.Contains(err.Message, "Too many requests") ||
			strings.Contains(err.Message, "Too Many Requests") ||
			strings.Contains(err.Message, "under too much load") {
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCapacityExceeded,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "block range") ||
			strings.Contains(err.Message, "exceeds the range") ||
			strings.Contains(err.Message, "Max range") ||
			strings.Contains(err.Message, "limited to") ||
			strings.Contains(err.Message, "response size should not") ||
			strings.Contains(err.Message, "returned more than") ||
			strings.Contains(err.Message, "exceeds max results") ||
			strings.Contains(err.Message, "response too large") ||
			strings.Contains(err.Message, "query exceeds limit") ||
			strings.Contains(err.Message, "exceeds the range") ||
			strings.Contains(err.Message, "range limit exceeded") {
			return common.NewErrEndpointRequestTooLarge(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCapacityExceeded,
					err.Message,
					nil,
					details,
				),
				common.EvmBlockRangeTooLarge,
			)
		} else if strings.Contains(err.Message, "specify less number of address") {
			return common.NewErrEndpointRequestTooLarge(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCapacityExceeded,
					err.Message,
					nil,
					details,
				),
				common.EvmAddressesTooLarge,
			)
		} else if strings.Contains(err.Message, "reached the free tier") ||
			strings.Contains(err.Message, "Monthly capacity limit") {
			return common.NewErrEndpointBillingIssue(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCapacityExceeded,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.HasPrefix(err.Message, "pending block is not available") ||
			strings.HasPrefix(err.Message, "pending block not found") ||
			strings.HasPrefix(err.Message, "Pending block not found") ||
			strings.HasPrefix(err.Message, "safe block not found") ||
			strings.HasPrefix(err.Message, "Safe block not found") ||
			strings.HasPrefix(err.Message, "finalized block not found") ||
			strings.HasPrefix(err.Message, "Finalized block not found") {
			// This error means node does not support "finalized/safe/pending" blocks.
			// ref https://github.com/ethereum/go-ethereum/blob/368e16f39d6c7e5cce72a92ec289adbfbaed4854/eth/api_backend.go#L67-L95
			details["blockTag"] = strings.ToLower(strings.SplitN(err.Message, " ", 2)[0])
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorClientSideException,
					err.Message,
					nil,
					details,
				),
			)
		} else if IsMissingDataError(err) {
			return common.NewErrEndpointMissingData(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorMissingData,
					err.Message,
					nil,
					details,
				),
			)
		} else if code == -32004 || code == -32001 {
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorUnsupportedException,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "execution timeout") {
			return common.NewErrEndpointServerSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorNodeTimeout,
					err.Message,
					nil,
					details,
				),
				nil,
			)
		} else if strings.Contains(err.Message, "reverted") ||
			strings.Contains(err.Message, "VM execution error") ||
			strings.Contains(err.Message, "transaction: revert") ||
			strings.Contains(err.Message, "VM Exception") {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorEvmReverted,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "insufficient funds") ||
			strings.Contains(err.Message, "insufficient balance") ||
			strings.Contains(err.Message, "out of gas") ||
			strings.Contains(err.Message, "gas too low") ||
			strings.Contains(err.Message, "IntrinsicGas") {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCallException,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "not found") ||
			strings.Contains(err.Message, "does not exist") ||
			strings.Contains(err.Message, "is not available") ||
			strings.Contains(err.Message, "is disabled") {
			if strings.Contains(err.Message, "Method") ||
				strings.Contains(err.Message, "method") ||
				strings.Contains(err.Message, "Module") ||
				strings.Contains(err.Message, "module") {
				return common.NewErrEndpointUnsupported(
					common.NewErrJsonRpcExceptionInternal(
						int(code),
						common.JsonRpcErrorUnsupportedException,
						err.Message,
						nil,
						details,
					),
				)
			} else if strings.Contains(err.Message, "header") ||
				strings.Contains(err.Message, "block") ||
				strings.Contains(err.Message, "Header") ||
				strings.Contains(err.Message, "Block") ||
				strings.Contains(err.Message, "transaction") ||
				strings.Contains(err.Message, "Transaction") {
				return common.NewErrEndpointMissingData(
					common.NewErrJsonRpcExceptionInternal(
						int(code),
						common.JsonRpcErrorMissingData,
						err.Message,
						nil,
						details,
					),
				)
			} else {
				return common.NewErrEndpointClientSideException(
					common.NewErrJsonRpcExceptionInternal(
						int(code),
						common.JsonRpcErrorClientSideException,
						err.Message,
						nil,
						details,
					),
				)
			}
		} else if strings.Contains(err.Message, "Unsupported method") ||
			strings.Contains(err.Message, "not supported") ||
			strings.Contains(err.Message, "method is not whitelisted") ||
			strings.Contains(err.Message, "is not included in your current plan") {
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorUnsupportedException,
					err.Message,
					nil,
					details,
				),
			)
		} else if code == -32600 {
			if dt, ok := err.Data.(map[string]interface{}); ok {
				if msg, ok := dt["message"]; ok {
					if strings.Contains(msg.(string), "validation errors in batch") {
						// Intentionally return a server-side error for failed requests in a batch
						// so they are retried in a different batch.
						// TODO Should we split a batch instead on json-rpc client level?
						return common.NewErrEndpointServerSideException(
							common.NewErrJsonRpcExceptionInternal(
								int(code),
								common.JsonRpcErrorServerSideException,
								err.Message,
								nil,
								details,
							),
							nil,
						)
					}
				}
			}
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorInvalidArgument,
					err.Message,
					nil,
					details,
				),
			)
		} else if r.StatusCode == 401 || r.StatusCode == 403 || strings.Contains(err.Message, "not allowed to access") {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorUnauthorized,
					err.Message,
					nil,
					details,
				),
			)
		} else if code == -32602 ||
			strings.Contains(err.Message, "param is required") ||
			strings.Contains(err.Message, "Invalid Request") ||
			strings.Contains(err.Message, "validation errors") ||
			strings.Contains(err.Message, "invalid argument") {
			if strings.Contains(err.Message, "tx of type") {
				// TODO For now we're returning a server-side exception for this error
				// so that the request is retried on a different upstream which supports the requested TX type.
				// This must be properly handled when "Upstream Features" is implemented which allows for feature-based routing.
				return common.NewErrEndpointServerSideException(
					common.NewErrJsonRpcExceptionInternal(
						int(code),
						common.JsonRpcErrorCallException,
						err.Message,
						nil,
						details,
					),
					nil,
				)
			}
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorInvalidArgument,
					err.Message,
					nil,
					details,
				),
			)
		}

		// By default we consider a problem on the server so that retry/failover mechanisms try other upstreams
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(
				int(code),
				common.JsonRpcErrorServerSideException,
				err.Message,
				nil,
				details,
			),
			nil,
		)
	}

	// There's a special case for certain clients that return a normal response for reverts:
	if jr != nil && jr.Result != nil && len(jr.Result) > 0 {
		dt := util.Mem2Str(jr.Result)
		// keccak256("Error(string)")
		if len(dt) > 11 && dt[1:11] == "0x08c379a0" {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					0,
					common.JsonRpcErrorEvmReverted,
					"transaction reverted",
					nil,
					map[string]interface{}{
						"data": json.RawMessage(jr.Result),
					},
				),
			)
		} else {
			// Trace and debug requests might fail due to operation timeout.
			// The response structure is not a standard json-rpc error response,
			// so we need to check the response body for a timeout message.
			// We avoid using JSON parsing to keep it fast on large (50MB) trace data.
			if rq := nr.Request(); rq != nil {
				m, _ := rq.Method()
				if strings.HasPrefix(m, "trace_") ||
					strings.HasPrefix(m, "debug_") ||
					strings.HasPrefix(m, "eth_trace") {
					if strings.Contains(dt, "execution timeout") {
						// Returning a server-side exception so that retry/failover mechanisms retry same and/or other upstreams.
						return common.NewErrEndpointServerSideException(
							common.NewErrJsonRpcExceptionInternal(
								0,
								common.JsonRpcErrorNodeTimeout,
								"execution timeout",
								nil,
								map[string]interface{}{
									"data": json.RawMessage(jr.Result),
								},
							),
							nil,
						)
					}
				}
			}
		}
	}

	return nil
}

func getVendorSpecificErrorIfAny(
	rp *http.Response,
	nr *common.NormalizedResponse,
	jr *common.JsonRpcResponse,
	details map[string]interface{},
) error {
	req := nr.Request()
	if req == nil {
		return nil
	}

	ups := req.LastUpstream()
	if ups == nil {
		return nil
	}

	vn := ups.Vendor()
	if vn == nil {
		return nil
	}

	return vn.GetVendorSpecificErrorIfAny(rp, jr, details)
}
