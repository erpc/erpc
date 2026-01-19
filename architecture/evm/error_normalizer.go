package evm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	bdscommon "github.com/blockchain-data-standards/manifesto/common"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func init() {
	_ = (&bdscommon.ErrorDetails{}).ProtoReflect().Descriptor()
}

func ExtractJsonRpcError(r *http.Response, nr *common.NormalizedResponse, jr *common.JsonRpcResponse, upstream common.Upstream) error {
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

		//----------------------------------------------------------------
		// "Request-too-large / range-too-large" errors
		//----------------------------------------------------------------

		if strings.Contains(msg, "Try with this block range") ||
			strings.Contains(msg, "block range is too wide") ||
			strings.Contains(msg, "this block range should work") ||
			strings.Contains(msg, "range too large") ||
			strings.Contains(msg, "exceeds the range") ||
			strings.Contains(msg, "max block range") ||
			strings.Contains(msg, "Max range") ||
			strings.Contains(msg, "logs over more") ||
			strings.Contains(msg, "response size should not") ||
			strings.Contains(msg, "returned more than") ||
			strings.Contains(msg, "exceeds max results") ||
			strings.Contains(msg, "range is too large") ||
			strings.Contains(msg, "too large, max is") ||
			strings.Contains(msg, "response too large") ||
			strings.Contains(msg, "query exceeds limit") ||
			strings.Contains(msg, "limit the query to") ||
			strings.Contains(msg, "maximum block range") ||
			strings.Contains(msg, "range limit exceeded") ||
			(strings.Contains(msg, "maximum") && strings.Contains(msg, "blocks distance")) ||
			strings.Contains(msg, "eth_getLogs is limited") {
			return common.NewErrEndpointRequestTooLarge(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorEvmLargeRange,
					fmt.Sprintf("getLogs request exceeded max allowed range: %s", err.Message),
					nil,
					details,
				),
				common.EvmBlockRangeTooLarge,
			)
		} else if strings.Contains(msg, "specify less number of address") {
			return common.NewErrEndpointRequestTooLarge(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorEvmLargeRange,
					fmt.Sprintf("getLogs request exceeded max allowed addresses: %s", err.Message),
					nil,
					details,
				),
				common.EvmAddressesTooLarge,
			)
		}

		//----------------------------------------------------------------
		// "Capacity-exceeded / rate-limiting / billing" errors
		//----------------------------------------------------------------

		if r.StatusCode == 402 ||
			strings.Contains(msg, "reached the free tier") ||
			strings.Contains(msg, "Monthly capacity limit") ||
			strings.Contains(msg, "limit for your current plan") ||
			strings.Contains(msg, "/billing") {
			// Specific billing-tier exhaustion or subscription limit
			return common.NewErrEndpointBillingIssue(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCapacityExceeded,
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
			strings.Contains(msg, "under too much load") ||
			strings.Contains(msg, "request limit reached") ||
			strings.Contains(msg, "No server available") ||
			strings.Contains(msg, "reached the quota") ||
			strings.Contains(msg, "upgrade your tier") ||
			strings.Contains(msg, "rate limit") ||
			strings.Contains(msg, "too many requests") ||
			strings.Contains(msg, "limit exceeded") {
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCapacityExceeded,
					err.Message,
					nil,
					details,
				),
			)
		}

		//----------------------------------------------------------------
		// "Block tag" errors (pending/finalized/safe not supported)
		//----------------------------------------------------------------

		if strings.Contains(msg, "pending block is not available") ||
			strings.Contains(msg, "pending block not found") ||
			strings.Contains(msg, "Pending block not found") ||
			strings.Contains(msg, "safe block not found") ||
			strings.Contains(msg, "Safe block not found") ||
			strings.Contains(msg, "finalized block not found") ||
			strings.Contains(msg, "Finalized block not found") ||
			strings.Contains(msg, "finalized is not a supported") ||
			strings.Contains(msg, "pending is not a supported") ||
			strings.Contains(msg, "safe is not a supported") ||
			strings.Contains(msg, "not a supported commitment") ||
			strings.Contains(msg, "malformed blocknumber") {

			// by default, we retry this type of client-side exception as other upstreams might
			// have/support this specific block tag data.
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

		//----------------------------------------------------------------
		// "Known missing data" errors
		//----------------------------------------------------------------

		if IsMissingDataError(err) {
			return common.NewErrEndpointMissingData(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorMissingData,
					err.Message,
					nil,
					details,
				),
				upstream,
			)
		}

		//----------------------------------------------------------------
		// "Timeouts / node-level" errors
		//----------------------------------------------------------------

		if strings.Contains(msg, "execution timeout") {
			return common.NewErrEndpointServerSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorNodeTimeout,
					err.Message,
					nil,
					details,
				),
				nil,
				r.StatusCode,
			)
		}

		//----------------------------------------------------------------
		// "EVM reverts and execution" errors
		//----------------------------------------------------------------
		if strings.Contains(msg, "reverted") ||
			strings.Contains(msg, "VM execution error") ||
			strings.Contains(msg, "transaction: revert") ||
			strings.Contains(msg, "VM Exception") ||
			strings.Contains(strings.ToLower(msg), "intrinsic gas too high") {
			execErr := common.NewErrEndpointExecutionException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorEvmReverted,
					err.Message,
					nil,
					details,
				),
			)
			// For eth_sendRawTransaction, execution reverted is retryable toward other upstreams
			// because different providers may have different simulation/pre-check behavior
			if nr != nil && nr.Request() != nil {
				if m, _ := nr.Request().Method(); strings.ToLower(m) == "eth_sendrawtransaction" {
					if re, ok := execErr.(common.RetryableError); ok {
						return re.WithRetryableTowardNetwork(true)
					}
				}
			}
			return execErr
		}
		// Hack for some chains (Berachain) to make the message compatible with Subgraph and other tools.
		if strings.Contains(msg, "EVM error: InvalidJump") {
			execErr := common.NewErrEndpointExecutionException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorEvmReverted,
					"revert: invalid jump destination",
					nil,
					details,
				),
			)
			// For eth_sendRawTransaction, execution reverted is retryable toward other upstreams
			if nr != nil && nr.Request() != nil {
				if m, _ := nr.Request().Method(); strings.ToLower(m) == "eth_sendrawtransaction" {
					if re, ok := execErr.(common.RetryableError); ok {
						return re.WithRetryableTowardNetwork(true)
					}
				}
			}
			return execErr
		}

		//----------------------------------------------------------------
		// "Duplicate transaction / nonce" errors for eth_sendRawTransaction idempotency
		// IMPORTANT: This must come BEFORE the generic -32003 handling below, because
		// some upstreams use -32003 for "already known" or "nonce too low" messages.
		// Note: "replacement transaction underpriced" is NOT handled here - it should remain an error
		//----------------------------------------------------------------

		ml := strings.ToLower(msg)
		if strings.Contains(ml, "already known") ||
			strings.Contains(ml, "known transaction") ||
			strings.Contains(ml, "already imported") ||
			strings.Contains(ml, "transaction already in mempool") ||
			strings.Contains(ml, "tx already in mempool") ||
			strings.Contains(ml, "already in the mempool") ||
			strings.Contains(ml, "transaction already exists") ||
			strings.Contains(ml, "already have transaction") ||
			strings.Contains(ml, "already exists in mempool") {
			// These indicate the exact same transaction is already known - idempotent success case
			return common.NewErrEndpointNonceException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorTransactionRejected,
					err.Message,
					nil,
					details,
				),
				common.NonceExceptionReasonAlreadyKnown,
			)
		}

		if strings.Contains(ml, "nonce too low") ||
			strings.Contains(ml, "nonce is too low") ||
			strings.Contains(ml, "nonce has already been used") {
			// These indicate a nonce conflict - requires verification to distinguish
			// between duplicate tx (idempotent) vs different tx with same nonce (error)
			return common.NewErrEndpointNonceException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorTransactionRejected,
					err.Message,
					nil,
					details,
				),
				common.NonceExceptionReasonNonceTooLow,
			)
		}

		//----------------------------------------------------------------
		// "Transaction rejected" or "Insufficient funds" or "out of gas" errors
		// Note: This comes AFTER nonce/duplicate detection to avoid masking those errors
		//----------------------------------------------------------------

		if code == common.JsonRpcErrorTransactionRejected ||
			strings.Contains(msg, "insufficient funds") ||
			strings.Contains(msg, "insufficient balance") ||
			strings.Contains(msg, "out of gas") ||
			strings.Contains(msg, "gas too low") ||
			strings.Contains(msg, "IntrinsicGas") {

			execErr := common.NewErrEndpointExecutionException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorTransactionRejected,
					err.Message,
					nil,
					details,
				),
			)
			// For eth_sendRawTransaction, these errors are retryable toward other upstreams
			// because different providers may have different balance-checking or gas-estimation logic
			if nr != nil && nr.Request() != nil {
				if m, _ := nr.Request().Method(); strings.ToLower(m) == "eth_sendrawtransaction" {
					if re, ok := execErr.(common.RetryableError); ok {
						return re.WithRetryableTowardNetwork(true)
					}
				}
			}
			return execErr
		}

		//----------------------------------------------------------------
		// "Not found" or "disabled" errors (missing data or unsupported)
		//----------------------------------------------------------------

		if strings.Contains(msg, "not found") ||
			strings.Contains(msg, "does not exist") ||
			strings.Contains(msg, "not available") ||
			strings.Contains(msg, "is disabled") ||
			strings.Contains(msg, "is not available") {

			if strings.Contains(msg, "Method") || strings.Contains(msg, "method") ||
				strings.Contains(msg, "Module") || strings.Contains(msg, "module") {
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
					upstream,
				)
			} else {
				// by default, we retry this type of client-side exception, as the root cause
				// might be an unsupported method or missing data, that another upstream might support.
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
		}

		//----------------------------------------------------------------
		// "Unsupported" errors
		//----------------------------------------------------------------

		// Note: do not move this check above "Not found" errors, as we want to
		// avoid premature detection when message is only "not found" (e.g. from Tenderly)

		if r.StatusCode == 415 || r.StatusCode == 405 ||
			code == common.JsonRpcErrorUnsupportedException || // By HTTP status code or explicit JSON-RPC error code
			code == -32004 || code == -32001 || // direct codes from upstream
			strings.Contains(msg, "Unsupported method") ||
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
		}

		//----------------------------------------------------------------
		// "Malformed transaction" errors (invalid RLP encoding, truncated data, etc.)
		// These indicate the raw transaction bytes are invalid - not retryable
		//----------------------------------------------------------------

		if strings.Contains(ml, "typed transaction too short") ||
			strings.Contains(ml, "transaction too short") ||
			strings.Contains(ml, "rlp: expected input list") ||
			strings.Contains(ml, "rlp: input string too short") ||
			strings.Contains(ml, "rlp: value size exceeds") ||
			strings.Contains(ml, "invalid transaction") ||
			strings.Contains(ml, "transaction type not supported") {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorInvalidArgument,
					err.Message,
					nil,
					details,
				),
			).WithRetryableTowardNetwork(false)
		}

		//----------------------------------------------------------------
		// "Invalid Argument / Params / Request" errors
		//----------------------------------------------------------------

		// Even though these errors have invalid argument or params, they are more about a lack of standard
		// for certain args/params for certain methods, among different providers or clients. We will retry them.
		//
		// Examples:
		if strings.Contains(msg, "tx of type") || // Should be retried toward upstreams that support this tx type
			strings.Contains(msg, "invalid type: map, expected BlockNumber, 'latest', or 'earliest'") { // Envio not supporting { blockNumber: XXX, blockHash: YYY } for eth_getBlockReceipts
			return common.NewErrEndpointClientSideException(
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
				if innerMsg, ok := dt["message"]; ok {
					if strings.Contains(innerMsg.(string), "validation errors in batch") {
						// Return a retryable client-side error so the caller might retry or split the batch.
						return common.NewErrEndpointClientSideException(
							common.NewErrJsonRpcExceptionInternal(
								int(code),
								common.JsonRpcErrorCallException,
								err.Message,
								nil,
								details,
							),
						)
					}
				}
			}
		} else if code == -32602 || code == -32600 {
			// For generic invalid args/params errors, we retry.
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorInvalidArgument,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(msg, "param is required") ||
			strings.Contains(msg, "Invalid Request") ||
			strings.Contains(msg, "validation errors") ||
			strings.Contains(msg, "invalid argument") ||
			strings.Contains(msg, "invalid params") ||
			strings.Contains(msg, "Bad request input parameters") {

			// For specific invalid args/params errors, there is a high chance that the error is due to a mistake that the user
			// has done, and retrying another upstream would not help.
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorInvalidArgument,
					err.Message,
					nil,
					details,
				),
			).WithRetryableTowardNetwork(false)
		}

		//----------------------------------------------------------------
		// "Unauthorized" errors
		//----------------------------------------------------------------

		if r.StatusCode == 401 || r.StatusCode == 403 ||
			strings.Contains(msg, "not allowed to access") ||
			strings.Contains(msg, "invalid api key") ||
			strings.Contains(msg, "key is inactive") ||
			strings.Contains(msg, "unauthorized") {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorUnauthorized,
					err.Message,
					nil,
					details,
				),
			)
		}

		//----------------------------------------------------------------
		// Fallback -> we consider it a server-side problem (failover / retry).
		//----------------------------------------------------------------
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(
				int(code),
				code,
				err.Message,
				nil,
				details,
			),
			nil,
			r.StatusCode,
		)
	}

	// -----------------------------------------------------------------------
	// Special-case check for reverts: Some clients return a normal 200 status,
	// but an EVM revert payload in jr.Result.
	// -----------------------------------------------------------------------
	if jr != nil && jr.ResultLength() > 0 {
		result := jr.GetResultString()
		dt := result
		// keccak256("Error(string)")
		if len(dt) > 11 && dt[1:11] == "0x08c379a0" {
			return common.NewErrEndpointExecutionException(
				common.NewErrJsonRpcExceptionInternal(
					0,
					common.JsonRpcErrorEvmReverted,
					"transaction reverted",
					nil,
					map[string]interface{}{
						"data": json.RawMessage(result),
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
									"data": json.RawMessage(result),
								},
							),
							nil,
							r.StatusCode,
						)
					}
				}
			}
		}
	}

	// No error detected.
	return nil
}

func ExtractGrpcError(st *status.Status, upstream common.Upstream) error {
	if st == nil || st.Code() == codes.OK {
		return nil
	}

	// Extract error details from gRPC status
	details := make(map[string]interface{})
	details["grpcCode"] = st.Code().String()
	details["grpcMessage"] = st.Message()
	upsId := "n/a"
	if upstream != nil {
		upsId = upstream.Id()
	}
	details["upstreamId"] = upsId

	// Try to extract BDS error details using FromGRPCStatus
	bdsErr, hasBdsError := bdscommon.FromGRPCStatus(st)
	if hasBdsError {
		details["bdsErrorCode"] = bdsErr.Code
		if bdsErr.Cause != nil {
			details["cause"] = bdsErr.Cause.Error()
		}
		if bdsErr.Details != nil {
			for k, v := range bdsErr.Details {
				details[k] = v
			}
		}
	}

	msg := st.Message()
	code := st.Code()

	if hasBdsError {
		switch bdsErr.Code {
		case bdscommon.ErrorCode_UNSUPPORTED_BLOCK_TAG,
			bdscommon.ErrorCode_UNSUPPORTED_METHOD:
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcExceptionInternal(
					int(common.JsonRpcErrorUnsupportedException),
					common.JsonRpcErrorUnsupportedException,
					msg,
					nil,
					details,
				),
			)

		case bdscommon.ErrorCode_RANGE_OUTSIDE_AVAILABLE:
			return common.NewErrEndpointMissingData(
				common.NewErrJsonRpcExceptionInternal(
					int(common.JsonRpcErrorMissingData),
					common.JsonRpcErrorMissingData,
					msg,
					nil,
					details,
				),
				upstream,
			)

		case bdscommon.ErrorCode_INVALID_PARAMETER, bdscommon.ErrorCode_INVALID_REQUEST:
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(common.JsonRpcErrorInvalidArgument),
					common.JsonRpcErrorInvalidArgument,
					msg,
					nil,
					details,
				),
			).WithRetryableTowardNetwork(false)

		case bdscommon.ErrorCode_RATE_LIMITED:
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(
					int(common.JsonRpcErrorCapacityExceeded),
					common.JsonRpcErrorCapacityExceeded,
					msg,
					nil,
					details,
				),
			)

		case bdscommon.ErrorCode_TIMEOUT_ERROR:
			return common.NewErrEndpointServerSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(common.JsonRpcErrorNodeTimeout),
					common.JsonRpcErrorNodeTimeout,
					msg,
					nil,
					details,
				),
				nil,
				0,
			)

		case bdscommon.ErrorCode_RANGE_TOO_LARGE:
			return common.NewErrEndpointRequestTooLarge(
				common.NewErrJsonRpcExceptionInternal(
					int(common.JsonRpcErrorEvmLargeRange),
					common.JsonRpcErrorEvmLargeRange,
					msg,
					nil,
					details,
				),
				common.EvmBlockRangeTooLarge,
			)

		case bdscommon.ErrorCode_INTERNAL_ERROR:
			return common.NewErrEndpointServerSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(common.JsonRpcErrorServerSideException),
					common.JsonRpcErrorServerSideException,
					msg,
					nil,
					details,
				),
				nil,
				0,
			)
		}
	}

	switch code {
	case codes.Unimplemented:
		return common.NewErrEndpointUnsupported(
			common.NewErrJsonRpcExceptionInternal(
				int(common.JsonRpcErrorUnsupportedException),
				common.JsonRpcErrorUnsupportedException,
				msg,
				nil,
				details,
			),
		)

	case codes.InvalidArgument:
		return common.NewErrEndpointClientSideException(
			common.NewErrJsonRpcExceptionInternal(
				int(common.JsonRpcErrorInvalidArgument),
				common.JsonRpcErrorInvalidArgument,
				msg,
				nil,
				details,
			),
		).WithRetryableTowardNetwork(false)

	case codes.ResourceExhausted:
		return common.NewErrEndpointCapacityExceeded(
			common.NewErrJsonRpcExceptionInternal(
				int(common.JsonRpcErrorCapacityExceeded),
				common.JsonRpcErrorCapacityExceeded,
				msg,
				nil,
				details,
			),
		)

	case codes.DeadlineExceeded:
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(
				int(common.JsonRpcErrorNodeTimeout),
				common.JsonRpcErrorNodeTimeout,
				msg,
				nil,
				details,
			),
			nil,
			0,
		)

	case codes.Unauthenticated, codes.PermissionDenied:
		return common.NewErrEndpointUnauthorized(
			common.NewErrJsonRpcExceptionInternal(
				int(common.JsonRpcErrorUnauthorized),
				common.JsonRpcErrorUnauthorized,
				msg,
				nil,
				details,
			),
		)

	case codes.NotFound, codes.OutOfRange:
		return common.NewErrEndpointMissingData(
			common.NewErrJsonRpcExceptionInternal(
				int(common.JsonRpcErrorMissingData),
				common.JsonRpcErrorMissingData,
				msg,
				nil,
				details,
			),
			upstream,
		)

	case codes.Internal, codes.Unknown, codes.Unavailable:
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(
				int(common.JsonRpcErrorServerSideException),
				common.JsonRpcErrorServerSideException,
				msg,
				nil,
				details,
			),
			nil,
			0,
		)

	default:
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(
				int(common.JsonRpcErrorServerSideException),
				common.JsonRpcErrorServerSideException,
				msg,
				nil,
				details,
			),
			nil,
			0,
		)
	}
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

	return vn.GetVendorSpecificErrorIfAny(req, rp, jr, details)
}
