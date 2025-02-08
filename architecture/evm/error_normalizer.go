package evm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

func ExtractJsonRpcError(r *http.Response, nr *common.NormalizedResponse, jr *common.JsonRpcResponse) error {
	if (jr != nil && jr.Error != nil) || r.StatusCode > 299 {
		var details map[string]interface{} = make(map[string]interface{})
		details["statusCode"] = r.StatusCode
		details["headers"] = util.ExtractUsefulHeaders(r)

		var err *common.ErrJsonRpcExceptionExternal
		if jr != nil && jr.Error != nil {
			err = jr.Error
			if ver := getVendorSpecificErrorIfAny(r, nr, jr, details); ver != nil {
				return ver
			}
		} else {
			err = common.NewErrJsonRpcExceptionExternal(
				int(common.JsonRpcErrorServerSideException),
				fmt.Sprintf("unexpected http failure with status code %d", r.StatusCode),
				"",
			)
		}

		code := common.JsonRpcErrorNumber(err.Code)
		msg := err.Message

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

				// Add the data to the message for text-based checks below
				msg += " Data: " + s
			}
		default:
			// passthrough error data as is
			details["data"] = err.Data

			// Add the data as string to the message for text-based checks below
			msg += " Data: " + fmt.Sprintf("%v", err.Data)
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
			strings.Contains(msg, "requests limited to") ||
			strings.Contains(msg, "has exceeded") ||
			strings.Contains(msg, "Exceeded the quota") ||
			strings.Contains(msg, "Too many requests") ||
			strings.Contains(msg, "Too Many Requests") ||
			strings.Contains(msg, "under too much load") {
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCapacityExceeded,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(msg, "block range") ||
			strings.Contains(msg, "exceeds the range") ||
			strings.Contains(msg, "Max range") ||
			strings.Contains(msg, "limited to") ||
			strings.Contains(msg, "response size should not") ||
			strings.Contains(msg, "returned more than") ||
			strings.Contains(msg, "exceeds max results") ||
			strings.Contains(msg, "range is too large") ||
			strings.Contains(msg, "too large, max is") ||
			strings.Contains(msg, "response too large") ||
			strings.Contains(msg, "query exceeds limit") ||
			strings.Contains(msg, "exceeds the range") ||
			strings.Contains(msg, "range limit exceeded") {
			return common.NewErrEndpointRequestTooLarge(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorEvmLargeRange,
					err.Message,
					nil,
					details,
				),
				common.EvmBlockRangeTooLarge,
			)
		} else if strings.Contains(msg, "specify less number of address") {
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
		} else if strings.Contains(msg, "reached the free tier") ||
			strings.Contains(msg, "Monthly capacity limit") {
			return common.NewErrEndpointBillingIssue(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCapacityExceeded,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.HasPrefix(msg, "pending block is not available") ||
			strings.HasPrefix(msg, "pending block not found") ||
			strings.HasPrefix(msg, "Pending block not found") ||
			strings.HasPrefix(msg, "safe block not found") ||
			strings.HasPrefix(msg, "Safe block not found") ||
			strings.HasPrefix(msg, "finalized block not found") ||
			strings.HasPrefix(msg, "Finalized block not found") {
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
		} else if strings.Contains(msg, "execution timeout") {
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
		} else if strings.Contains(msg, "reverted") ||
			strings.Contains(msg, "VM execution error") ||
			strings.Contains(msg, "transaction: revert") ||
			strings.Contains(msg, "VM Exception") {
			return common.NewErrEndpointExecutionException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorEvmReverted,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(msg, "insufficient funds") ||
			strings.Contains(msg, "insufficient balance") ||
			strings.Contains(msg, "out of gas") ||
			strings.Contains(msg, "gas too low") ||
			strings.Contains(msg, "IntrinsicGas") {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCallException,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(msg, "not found") ||
			strings.Contains(msg, "does not exist") ||
			strings.Contains(msg, "not available") ||
			strings.Contains(msg, "is disabled") {
			if strings.Contains(msg, "Method") ||
				strings.Contains(msg, "method") ||
				strings.Contains(msg, "Module") ||
				strings.Contains(msg, "module") {
				return common.NewErrEndpointUnsupported(
					common.NewErrJsonRpcExceptionInternal(
						int(code),
						common.JsonRpcErrorUnsupportedException,
						err.Message,
						nil,
						details,
					),
				)
			} else if strings.Contains(msg, "header") ||
				strings.Contains(msg, "block") ||
				strings.Contains(msg, "Header") ||
				strings.Contains(msg, "Block") ||
				strings.Contains(msg, "transaction") ||
				strings.Contains(msg, "Transaction") {
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
		} else if strings.Contains(msg, "Unsupported method") ||
			strings.Contains(msg, "not supported") ||
			strings.Contains(msg, "method is not whitelisted") ||
			strings.Contains(msg, "is not included in your current plan") {
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
		} else if r.StatusCode == 401 || r.StatusCode == 403 || strings.Contains(msg, "not allowed to access") {
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
			strings.Contains(msg, "param is required") ||
			strings.Contains(msg, "Invalid Request") ||
			strings.Contains(msg, "validation errors") ||
			strings.Contains(msg, "invalid argument") ||
			strings.Contains(msg, "invalid params") {
			if strings.Contains(msg, "tx of type") {
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
			return common.NewErrEndpointExecutionException(
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
