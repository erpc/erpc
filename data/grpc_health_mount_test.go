package data

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Bootstrap replaces the static servers list before clients are built, so a
// path-mounted URL returned by discovery must hit the same mount guard that
// GrpcConnectorConfig.Validate applies to YAML servers — otherwise
// healthCheckService would enable a root-only Watch on a mount.
func TestNewGrpcConnector_BootstrapMountedHealthCheckRejected(t *testing.T) {
	tests := []struct {
		name       string
		discovered []string
	}{
		{name: "boost mount", discovered: []string{"grpc://host/boost"}},
		{name: "generic mount", discovered: []string{"grpc://host/mount"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(tt.discovered)
			require.NoError(t, err)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(body)
			}))
			t.Cleanup(srv.Close)

			lg := zerolog.Nop()
			cfg := &common.GrpcConnectorConfig{
				// Static root servers would pass config-time Validate; bootstrap
				// replaces them with the discovered list — the regression path.
				Servers:            []string{"grpc://static.example:50051"},
				Bootstrap:          srv.URL,
				HealthCheckService: "bds",
			}

			conn, err := NewGrpcConnector(context.Background(), &lg, "test-grpc", cfg)
			require.ErrorContains(t, err, "database.*.connector.grpc.healthCheckService")
			require.Nil(t, conn)
		})
	}
}
