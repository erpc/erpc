package common

import (
	"errors"
	"testing"

	bdscommon "github.com/blockchain-data-standards/manifesto/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covErrorsInnerJsonRpc returns the json-rpc exception the gRPC mapper nests as cause.
func covErrorsInnerJsonRpc(t *testing.T, err error) *ErrJsonRpcExceptionInternal {
	t.Helper()
	var inner *ErrJsonRpcExceptionInternal
	require.True(t, errors.As(err, &inner), "expected nested ErrJsonRpcExceptionInternal in %v", err)
	return inner
}

func TestCovErrors_Grpc_NilAndOK(t *testing.T) {
	assert.Nil(t, ExtractGrpcErrorFromGrpcStatus(nil, nil))
	assert.Nil(t, ExtractGrpcErrorFromGrpcStatus(status.New(codes.OK, "fine"), NewFakeUpstream("u")))
}

func TestCovErrors_Grpc_PlainStatusCodes(t *testing.T) {
	cases := []struct {
		code       codes.Code
		errCode    ErrorCode
		normalized JsonRpcErrorNumber
	}{
		{codes.Canceled, ErrCodeEndpointRequestCanceled, 0},
		{codes.Unimplemented, ErrCodeEndpointUnsupported, JsonRpcErrorUnsupportedException},
		{codes.InvalidArgument, ErrCodeEndpointClientSideException, JsonRpcErrorInvalidArgument},
		{codes.ResourceExhausted, ErrCodeEndpointCapacityExceeded, JsonRpcErrorCapacityExceeded},
		{codes.DeadlineExceeded, ErrCodeEndpointRequestTimeout, JsonRpcErrorNodeTimeout},
		{codes.Unauthenticated, ErrCodeEndpointUnauthorized, JsonRpcErrorUnauthorized},
		{codes.PermissionDenied, ErrCodeEndpointUnauthorized, JsonRpcErrorUnauthorized},
		{codes.NotFound, ErrCodeEndpointMissingData, JsonRpcErrorMissingData},
		{codes.OutOfRange, ErrCodeEndpointMissingData, JsonRpcErrorMissingData},
		{codes.Internal, ErrCodeEndpointServerSideException, JsonRpcErrorServerSideException},
		{codes.Unknown, ErrCodeEndpointServerSideException, JsonRpcErrorServerSideException},
		{codes.Unavailable, ErrCodeEndpointServerSideException, JsonRpcErrorServerSideException},
		{codes.AlreadyExists, ErrCodeEndpointServerSideException, JsonRpcErrorServerSideException},
		{codes.Aborted, ErrCodeEndpointServerSideException, JsonRpcErrorServerSideException},
	}
	for _, tc := range cases {
		for _, withUpstream := range []bool{false, true} {
			name := tc.code.String()
			var up Upstream
			wantUpsId := "n/a"
			if withUpstream {
				name += "/withUpstream"
				up = NewFakeUpstream("bds-up")
				wantUpsId = "bds-up"
			}
			t.Run(name, func(t *testing.T) {
				err := ExtractGrpcErrorFromGrpcStatus(status.New(tc.code, "boom from server"), up)
				require.Error(t, err)
				assert.True(t, HasErrorCode(err, tc.errCode), "got %v", err)

				inner := covErrorsInnerJsonRpc(t, err)
				assert.Equal(t, tc.normalized, inner.NormalizedCode())
				assert.Equal(t, int(tc.normalized), inner.OriginalCode())
				assert.Equal(t, "boom from server", inner.Message)
				assert.Equal(t, tc.code.String(), inner.Details["grpcCode"])
				assert.Equal(t, "boom from server", inner.Details["grpcMessage"])
				assert.Equal(t, wantUpsId, inner.Details["upstreamId"])
				assert.NotContains(t, inner.Details, "bdsErrorCode")

				if tc.code == codes.InvalidArgument {
					assert.False(t, IsRetryableTowardNetwork(err), "invalid argument must not fail over")
				}
				if tc.errCode == ErrCodeEndpointMissingData {
					md := err.(*ErrEndpointMissingData)
					if withUpstream {
						assert.Equal(t, "bds-up", md.Details["upstreamId"])
						assert.Same(t, up, md.Upstream())
					} else {
						assert.NotContains(t, md.Details, "upstreamId")
					}
				}
			})
		}
	}
}

func TestCovErrors_Grpc_BdsErrorCodes(t *testing.T) {
	cases := []struct {
		bds        bdscommon.ErrorCode
		errCode    ErrorCode
		normalized JsonRpcErrorNumber
	}{
		{bdscommon.ErrorCode_UNSUPPORTED_BLOCK_TAG, ErrCodeEndpointUnsupported, JsonRpcErrorUnsupportedException},
		{bdscommon.ErrorCode_UNSUPPORTED_METHOD, ErrCodeEndpointUnsupported, JsonRpcErrorUnsupportedException},
		{bdscommon.ErrorCode_RANGE_OUTSIDE_AVAILABLE, ErrCodeEndpointMissingData, JsonRpcErrorMissingData},
		{bdscommon.ErrorCode_INVALID_PARAMETER, ErrCodeEndpointClientSideException, JsonRpcErrorInvalidArgument},
		{bdscommon.ErrorCode_INVALID_REQUEST, ErrCodeEndpointClientSideException, JsonRpcErrorInvalidArgument},
		{bdscommon.ErrorCode_RATE_LIMITED, ErrCodeEndpointCapacityExceeded, JsonRpcErrorCapacityExceeded},
		{bdscommon.ErrorCode_TIMEOUT_ERROR, ErrCodeEndpointRequestTimeout, JsonRpcErrorNodeTimeout},
		{bdscommon.ErrorCode_RANGE_TOO_LARGE, ErrCodeEndpointRequestTooLarge, JsonRpcErrorEvmLargeRange},
		{bdscommon.ErrorCode_INTERNAL_ERROR, ErrCodeEndpointServerSideException, JsonRpcErrorServerSideException},
		// Unmapped BDS codes fall through to the gRPC code switch (Unknown -> server-side).
		{bdscommon.ErrorCode_DATA_NOT_FOUND, ErrCodeEndpointServerSideException, JsonRpcErrorServerSideException},
	}
	for _, tc := range cases {
		t.Run(tc.bds.String(), func(t *testing.T) {
			up := NewFakeUpstream("bds-up")
			st := bdscommon.NewError(tc.bds, "bds says no").
				WithCause(errors.New("root cause")).
				WithDetail("fromBlock", 5).
				WithDetail("hint", "narrow it").
				ToGRPCStatus()

			err := ExtractGrpcErrorFromGrpcStatus(st, up)
			require.Error(t, err)
			assert.True(t, HasErrorCode(err, tc.errCode), "got %v", err)

			inner := covErrorsInnerJsonRpc(t, err)
			assert.Equal(t, tc.normalized, inner.NormalizedCode())
			assert.Equal(t, "bds says no", inner.Message)
			assert.Equal(t, tc.bds, inner.Details["bdsErrorCode"])
			assert.Equal(t, "root cause", inner.Details["cause"])
			assert.Equal(t, float64(5), inner.Details["fromBlock"], "JSON-encoded BDS details are decoded")
			assert.Equal(t, "narrow it", inner.Details["hint"])
			assert.Equal(t, "bds-up", inner.Details["upstreamId"])
			assert.Equal(t, st.Code().String(), inner.Details["grpcCode"])

			if tc.errCode == ErrCodeEndpointRequestTooLarge {
				assert.Equal(t, EvmBlockRangeTooLarge, err.(*ErrEndpointRequestTooLarge).Details["complaint"])
			}
			if tc.errCode == ErrCodeEndpointClientSideException {
				assert.False(t, IsRetryableTowardNetwork(err))
			}
		})
	}
}

func TestCovErrors_Grpc_BdsWithoutCause(t *testing.T) {
	st := bdscommon.NewError(bdscommon.ErrorCode_RATE_LIMITED, "slow down").ToGRPCStatus()
	err := ExtractGrpcErrorFromGrpcStatus(st, nil)
	assert.True(t, HasErrorCode(err, ErrCodeEndpointCapacityExceeded))
	inner := covErrorsInnerJsonRpc(t, err)
	assert.NotContains(t, inner.Details, "cause")
	assert.Equal(t, bdscommon.ErrorCode_RATE_LIMITED, inner.Details["bdsErrorCode"])
	assert.Equal(t, "n/a", inner.Details["upstreamId"])
	assert.Equal(t, codes.ResourceExhausted.String(), inner.Details["grpcCode"])
}
