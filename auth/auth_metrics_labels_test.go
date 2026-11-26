package auth

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

// labelNetwork satisfies common.Network for the label lookups RecordAuthFailed
// performs; any other method call panics via the nil embedded interface.
type labelNetwork struct {
	common.Network
	id, label, project string
}

func (n *labelNetwork) Id() string        { return n.id }
func (n *labelNetwork) Label() string     { return n.label }
func (n *labelNetwork) ProjectId() string { return n.project }

func TestRecordAuthFailed_NetworkLabels(t *testing.T) {
	count := func(project, network, strategy, reason string) float64 {
		return testutil.ToFloat64(telemetry.MetricAuthFailedTotal.WithLabelValues(project, network, strategy, reason, "unknown"))
	}

	t.Run("network label preferred over id", func(t *testing.T) {
		req := newRequest("eth_call")
		req.SetNetwork(&labelNetwork{id: "evm:1", label: "mainnet", project: "proj-labels"})
		before := count("proj-labels", "mainnet", "network", "ip_not_allowed")
		RecordAuthFailed(req, common.NewErrAuthUnauthorized("network", "ip_not_allowed", "nope"))
		assert.Equal(t, before+1, count("proj-labels", "mainnet", "network", "ip_not_allowed"))
	})

	t.Run("network id used when label empty", func(t *testing.T) {
		req := newRequest("eth_call")
		req.SetNetwork(&labelNetwork{id: "evm:137", project: "proj-labels"})
		before := count("proj-labels", "evm:137", "siwe", "parse_failed")
		RecordAuthFailed(req, common.NewErrAuthUnauthorized("siwe", "parse_failed", "bad"))
		assert.Equal(t, before+1, count("proj-labels", "evm:137", "siwe", "parse_failed"))
	})

	t.Run("missing strategy and reason fall back to defaults", func(t *testing.T) {
		before := count("n/a", "n/a", "n/a", "unauthorized")
		RecordAuthFailed(nil, common.NewErrAuthUnauthorized("", "", "anon"))
		assert.Equal(t, before+1, count("n/a", "n/a", "n/a", "unauthorized"))
	})
}
