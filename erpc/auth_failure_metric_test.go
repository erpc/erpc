package erpc

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func init() { util.ConfigureTestLogger() }

func authFailureCount(strategy, reason string) float64 {
	return testutil.ToFloat64(telemetry.MetricAuthFailedTotal.WithLabelValues(
		"n/a", "n/a", strategy, reason, "unknown",
	))
}

func TestProcessErrorBody_RecordsClientFacingUnauthorized(t *testing.T) {
	err := common.NewErrAuthUnauthorizedReason(
		"jwt",
		common.AuthUnauthorizedReasonTokenExpired,
	)
	before := authFailureCount("jwt", "token_expired")
	startedAt := time.Now()
	logger := zerolog.Nop()

	processErrorBody(&logger, &startedAt, nil, err, &common.FALSE)

	assert.Equal(t, before+1, authFailureCount("jwt", "token_expired"))
}

func TestGrpcStatusOf_RecordsClientFacingUnauthorized(t *testing.T) {
	err := common.NewErrAuthUnauthorizedReason(
		"jwt",
		common.AuthUnauthorizedReasonInvalidSignature,
	)
	before := authFailureCount("jwt", "invalid_signature")

	got := grpcStatusOf(err)

	assert.Equal(t, codes.Unauthenticated, status.Code(got))
	assert.Equal(t, before+1, authFailureCount("jwt", "invalid_signature"))
}
