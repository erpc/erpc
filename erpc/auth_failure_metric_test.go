package erpc

import (
	"testing"

	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func init() { util.ConfigureTestLogger() }

func authFailureCount(project, strategy, reason string) float64 {
	return testutil.ToFloat64(telemetry.MetricAuthFailedTotal.WithLabelValues(
		project, "n/a", strategy, reason, "unknown",
	))
}

func TestRecordAuthFailed_RecordsClientFacingUnauthorized(t *testing.T) {
	err := common.NewErrAuthUnauthorizedReason(
		"jwt",
		common.AuthUnauthorizedReasonTokenExpired,
	)
	before := authFailureCount("proj-x", "jwt", "token_expired")

	auth.RecordAuthFailed("proj-x", err)

	assert.Equal(t, before+1, authFailureCount("proj-x", "jwt", "token_expired"))
}

func TestGrpcStatusOf_RecordsClientFacingUnauthorized(t *testing.T) {
	err := common.NewErrAuthUnauthorizedReason(
		"jwt",
		common.AuthUnauthorizedReasonInvalidSignature,
	)
	before := authFailureCount("n/a", "jwt", "invalid_signature")

	got := grpcStatusOf(err)

	assert.Equal(t, codes.Unauthenticated, status.Code(got))
	assert.Equal(t, before+1, authFailureCount("n/a", "jwt", "invalid_signature"))
}
