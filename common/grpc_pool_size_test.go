package common

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestGrpcConnectorConfig_Validate_PoolSize(t *testing.T) {
	t.Run("zero (unset) is valid", func(t *testing.T) {
		require.NoError(t, (&GrpcConnectorConfig{PoolSize: 0}).Validate())
	})
	t.Run("positive is valid", func(t *testing.T) {
		require.NoError(t, (&GrpcConnectorConfig{PoolSize: 8}).Validate())
	})
	t.Run("max boundary is valid", func(t *testing.T) {
		require.NoError(t, (&GrpcConnectorConfig{PoolSize: MaxGrpcConnPoolSize}).Validate())
	})
	t.Run("negative is rejected", func(t *testing.T) {
		err := (&GrpcConnectorConfig{PoolSize: -1}).Validate()
		require.ErrorContains(t, err, "must not be negative")
		require.ErrorContains(t, err, "database.*.connector.grpc.poolSize")
	})
	t.Run("above max is rejected", func(t *testing.T) {
		err := (&GrpcConnectorConfig{PoolSize: MaxGrpcConnPoolSize + 1}).Validate()
		require.ErrorContains(t, err, "must be <=")
	})
}

func TestGrpcUpstreamConfig_Validate_PoolSize(t *testing.T) {
	t.Run("zero (unset) is valid", func(t *testing.T) {
		require.NoError(t, (&GrpcUpstreamConfig{PoolSize: 0}).Validate())
	})
	t.Run("positive is valid", func(t *testing.T) {
		require.NoError(t, (&GrpcUpstreamConfig{PoolSize: 16}).Validate())
	})
	t.Run("negative is rejected with upstream scope", func(t *testing.T) {
		err := (&GrpcUpstreamConfig{PoolSize: -5}).Validate()
		require.ErrorContains(t, err, "must not be negative")
		require.ErrorContains(t, err, "upstream.*.grpc.poolSize")
	})
	t.Run("above max is rejected", func(t *testing.T) {
		require.ErrorContains(t,
			(&GrpcUpstreamConfig{PoolSize: MaxGrpcConnPoolSize + 1}).Validate(), "must be <=")
	})
}

// TestConnectorConfig_Validate_PropagatesGrpcPoolSize proves the connector-level
// Validate() actually dispatches into the gRPC sub-config (i.e. the hook is
// wired), not just that GrpcConnectorConfig.Validate() exists.
func TestConnectorConfig_Validate_PropagatesGrpcPoolSize(t *testing.T) {
	cfg := &ConnectorConfig{
		Id:     "edge-cache",
		Driver: DriverGrpc,
		Grpc:   &GrpcConnectorConfig{PoolSize: -1},
	}
	require.ErrorContains(t, cfg.Validate(), "must not be negative")

	cfg.Grpc.PoolSize = 8
	require.NoError(t, cfg.Validate())
}

// TestUpstreamConfig_Validate_PropagatesGrpcPoolSize proves the upstream-level
// Validate() dispatches into the gRPC sub-config.
func TestUpstreamConfig_Validate_PropagatesGrpcPoolSize(t *testing.T) {
	cfg := &UpstreamConfig{
		Id:       "gcs-edge",
		Endpoint: "grpc://example.internal:50051",
		Grpc:     &GrpcUpstreamConfig{PoolSize: -1},
	}
	require.ErrorContains(t, cfg.Validate(&Config{}, false), "must not be negative")

	cfg.Grpc.PoolSize = 16
	require.NoError(t, cfg.Validate(&Config{}, false))
}

// TestGrpcUpstreamConfig_Copy_PreservesPoolSize guards the upstream-config copy
// path (used when materializing per-upstream configs) against dropping the knob.
func TestGrpcUpstreamConfig_Copy_PreservesPoolSize(t *testing.T) {
	orig := &GrpcUpstreamConfig{
		Headers:  map[string]string{"authorization": "Bearer x"},
		PoolSize: 9,
	}
	cp := orig.Copy()
	require.Equal(t, 9, cp.PoolSize)

	// Mutating the copy must not affect the original (deep copy of headers,
	// independent PoolSize value).
	cp.PoolSize = 1
	cp.Headers["authorization"] = "Bearer y"
	require.Equal(t, 9, orig.PoolSize)
	require.Equal(t, "Bearer x", orig.Headers["authorization"])
}

func TestGrpcConnectorConfig_PoolSize_YAMLRoundTrip(t *testing.T) {
	t.Run("parses poolSize from yaml", func(t *testing.T) {
		var cfg GrpcConnectorConfig
		require.NoError(t, yaml.Unmarshal([]byte("poolSize: 12\n"), &cfg))
		require.Equal(t, 12, cfg.PoolSize)
	})
	t.Run("omitempty drops zero poolSize", func(t *testing.T) {
		out, err := yaml.Marshal(GrpcConnectorConfig{})
		require.NoError(t, err)
		require.NotContains(t, string(out), "poolSize")
	})
	t.Run("marshals non-zero poolSize", func(t *testing.T) {
		out, err := yaml.Marshal(GrpcConnectorConfig{PoolSize: 12})
		require.NoError(t, err)
		require.Contains(t, string(out), "poolSize: 12")
	})
}

func TestGrpcUpstreamConfig_PoolSize_YAMLRoundTrip(t *testing.T) {
	var cfg GrpcUpstreamConfig
	require.NoError(t, yaml.Unmarshal([]byte("poolSize: 9\n"), &cfg))
	require.Equal(t, 9, cfg.PoolSize)

	out, err := yaml.Marshal(GrpcUpstreamConfig{})
	require.NoError(t, err)
	require.False(t, strings.Contains(string(out), "poolSize"),
		"zero poolSize must be omitted, got: %q", string(out))
}
