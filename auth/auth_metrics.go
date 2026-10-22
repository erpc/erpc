package auth

import (
	"errors"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// RecordAuthFailed increments erpc_auth_failed_total for ErrAuthUnauthorized
// about to be returned to the client; no-ops for other errors.
func RecordAuthFailed(req *common.NormalizedRequest, err error) {
	if !common.HasErrorCode(err, common.ErrCodeAuthUnauthorized) {
		return
	}
	strategy, reason := authFailureLabels(err)
	project := "n/a"
	network := "n/a"
	if req != nil {
		if n := req.Network(); n != nil {
			project = n.ProjectId()
			if lbl := n.Label(); lbl != "" {
				network = lbl
			} else {
				network = n.Id()
			}
		}
	}
	telemetry.MetricAuthFailedTotal.WithLabelValues(
		project,
		network,
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
