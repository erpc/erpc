package auth

import (
	"errors"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// RecordAuthFailed increments erpc_auth_failed_total for ErrAuthUnauthorized
// about to be returned to the client; no-ops for other errors.
// Auth is project-scoped, so network is always "n/a".
func RecordAuthFailed(projectId string, err error) {
	if !common.HasErrorCode(err, common.ErrCodeAuthUnauthorized) {
		return
	}
	if projectId == "" {
		projectId = "n/a"
	}
	strategy, reason := authFailureLabels(err)
	telemetry.MetricAuthFailedTotal.WithLabelValues(
		projectId,
		"n/a",
		strategy,
		reason,
		"unknown",
	).Inc()
}

func authFailureLabels(err error) (strategy, reason string) {
	var ae *common.ErrAuthUnauthorized
	if !errors.As(err, &ae) || ae == nil {
		return "n/a", "unauthorized"
	}
	strategy = "n/a"
	if ae.Details != nil {
		if s, ok := ae.Details["strategy"].(string); ok && s != "" {
			strategy = s
		}
	}
	reason = string(ae.Reason())
	if reason == "" {
		reason = "unauthorized"
	}
	return strategy, reason
}
