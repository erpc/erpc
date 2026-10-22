package auth

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestRecordAuthFailed_ProjectAndDefaults(t *testing.T) {
	count := func(project, network, strategy, reason string) float64 {
		return testutil.ToFloat64(telemetry.MetricAuthFailedTotal.WithLabelValues(project, network, strategy, reason, "unknown"))
	}

	t.Run("project id is recorded; network is always n/a", func(t *testing.T) {
		before := count("proj-labels", "n/a", "network", "ip_not_allowed")
		RecordAuthFailed("proj-labels", common.NewErrAuthUnauthorized("network", "ip_not_allowed", "nope"))
		assert.Equal(t, before+1, count("proj-labels", "n/a", "network", "ip_not_allowed"))
	})

	t.Run("empty project id falls back to n/a", func(t *testing.T) {
		before := count("n/a", "n/a", "siwe", "parse_failed")
		RecordAuthFailed("", common.NewErrAuthUnauthorized("siwe", "parse_failed", "bad"))
		assert.Equal(t, before+1, count("n/a", "n/a", "siwe", "parse_failed"))
	})

	t.Run("missing strategy and reason fall back to defaults", func(t *testing.T) {
		before := count("n/a", "n/a", "n/a", "unauthorized")
		RecordAuthFailed("", common.NewErrAuthUnauthorized("", "", "anon"))
		assert.Equal(t, before+1, count("n/a", "n/a", "n/a", "unauthorized"))
	})
}
