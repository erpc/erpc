package common

import (
	bdscommon "github.com/blockchain-data-standards/manifesto/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExtractGrpcErrorFromGrpcStatus normalizes gRPC status errors returned by BDS servers
// into eRPC standard errors. This is kept in common to avoid import cycles between
// clients, data, and architecture packages.
func ExtractGrpcErrorFromGrpcStatus(st *status.Status, upstream Upstream) error {
	if st == nil || st.Code() == codes.OK {
		return nil
	}

	details := make(map[string]interface{})
	details["grpcCode"] = st.Code().String()
	details["grpcMessage"] = st.Message()
	upsId := "n/a"
	if upstream != nil {
		upsId = upstream.Id()
	}
	details["upstreamId"] = upsId

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
			return NewErrEndpointUnsupported(
				NewErrJsonRpcExceptionInternal(
					int(JsonRpcErrorUnsupportedException),
					JsonRpcErrorUnsupportedException,
					msg,
					nil,
					details,
				),
			)
		case bdscommon.ErrorCode_RANGE_OUTSIDE_AVAILABLE:
			return NewErrEndpointMissingData(
				NewErrJsonRpcExceptionInternal(
					int(JsonRpcErrorMissingData),
					JsonRpcErrorMissingData,
					msg,
					nil,
					details,
				),
				upstream,
			)
		case bdscommon.ErrorCode_INVALID_PARAMETER, bdscommon.ErrorCode_INVALID_REQUEST:
			return NewErrEndpointClientSideException(
				NewErrJsonRpcExceptionInternal(
					int(JsonRpcErrorInvalidArgument),
					JsonRpcErrorInvalidArgument,
					msg,
					nil,
					details,
				),
			).WithRetryableTowardNetwork(false)
		case bdscommon.ErrorCode_RATE_LIMITED:
			return NewErrEndpointCapacityExceeded(
				NewErrJsonRpcExceptionInternal(
					int(JsonRpcErrorCapacityExceeded),
					JsonRpcErrorCapacityExceeded,
					msg,
					nil,
					details,
				),
			)
		case bdscommon.ErrorCode_TIMEOUT_ERROR:
			return NewErrEndpointServerSideException(
				NewErrJsonRpcExceptionInternal(
					int(JsonRpcErrorNodeTimeout),
					JsonRpcErrorNodeTimeout,
					msg,
					nil,
					details,
				),
				nil,
				0,
			)
		case bdscommon.ErrorCode_RANGE_TOO_LARGE:
			return NewErrEndpointRequestTooLarge(
				NewErrJsonRpcExceptionInternal(
					int(JsonRpcErrorEvmLargeRange),
					JsonRpcErrorEvmLargeRange,
					msg,
					nil,
					details,
				),
				EvmBlockRangeTooLarge,
			)
		case bdscommon.ErrorCode_INTERNAL_ERROR:
			return NewErrEndpointServerSideException(
				NewErrJsonRpcExceptionInternal(
					int(JsonRpcErrorServerSideException),
					JsonRpcErrorServerSideException,
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
		return NewErrEndpointUnsupported(
			NewErrJsonRpcExceptionInternal(
				int(JsonRpcErrorUnsupportedException),
				JsonRpcErrorUnsupportedException,
				msg,
				nil,
				details,
			),
		)
	case codes.InvalidArgument:
		return NewErrEndpointClientSideException(
			NewErrJsonRpcExceptionInternal(
				int(JsonRpcErrorInvalidArgument),
				JsonRpcErrorInvalidArgument,
				msg,
				nil,
				details,
			),
		).WithRetryableTowardNetwork(false)
	case codes.ResourceExhausted:
		return NewErrEndpointCapacityExceeded(
			NewErrJsonRpcExceptionInternal(
				int(JsonRpcErrorCapacityExceeded),
				JsonRpcErrorCapacityExceeded,
				msg,
				nil,
				details,
			),
		)
	case codes.DeadlineExceeded:
		return NewErrEndpointServerSideException(
			NewErrJsonRpcExceptionInternal(
				int(JsonRpcErrorNodeTimeout),
				JsonRpcErrorNodeTimeout,
				msg,
				nil,
				details,
			),
			nil,
			0,
		)
	case codes.Unauthenticated, codes.PermissionDenied:
		return NewErrEndpointUnauthorized(
			NewErrJsonRpcExceptionInternal(
				int(JsonRpcErrorUnauthorized),
				JsonRpcErrorUnauthorized,
				msg,
				nil,
				details,
			),
		)
	case codes.NotFound, codes.OutOfRange:
		return NewErrEndpointMissingData(
			NewErrJsonRpcExceptionInternal(
				int(JsonRpcErrorMissingData),
				JsonRpcErrorMissingData,
				msg,
				nil,
				details,
			),
			upstream,
		)
	case codes.Internal, codes.Unknown, codes.Unavailable:
		return NewErrEndpointServerSideException(
			NewErrJsonRpcExceptionInternal(
				int(JsonRpcErrorServerSideException),
				JsonRpcErrorServerSideException,
				msg,
				nil,
				details,
			),
			nil,
			0,
		)
	default:
		return NewErrEndpointServerSideException(
			NewErrJsonRpcExceptionInternal(
				int(JsonRpcErrorServerSideException),
				JsonRpcErrorServerSideException,
				msg,
				nil,
				details,
			),
			nil,
			0,
		)
	}
}
