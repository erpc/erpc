package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJsonRpcUpstreamMaxResponseBytes(t *testing.T) {
	limit := int64(1024)
	zero := int64(0)
	defaults := &UpstreamConfig{JsonRpc: &JsonRpcUpstreamConfig{MaxResponseBytes: &limit}}

	t.Run("inherits with no jsonRpc block", func(t *testing.T) {
		upstream := &UpstreamConfig{}
		require.NoError(t, upstream.ApplyDefaults(defaults))
		require.Equal(t, limit, *upstream.JsonRpc.MaxResponseBytes)
	})
	t.Run("inherits with another jsonRpc setting", func(t *testing.T) {
		upstream := &UpstreamConfig{JsonRpc: &JsonRpcUpstreamConfig{EnableGzip: &TRUE}}
		require.NoError(t, upstream.ApplyDefaults(defaults))
		require.Equal(t, limit, *upstream.JsonRpc.MaxResponseBytes)
	})
	t.Run("explicit zero disables inherited limit", func(t *testing.T) {
		upstream := &UpstreamConfig{JsonRpc: &JsonRpcUpstreamConfig{MaxResponseBytes: &zero}}
		require.NoError(t, upstream.ApplyDefaults(defaults))
		require.Zero(t, *upstream.JsonRpc.MaxResponseBytes)
	})
	t.Run("rejects negative limit", func(t *testing.T) {
		negative := int64(-1)
		cfg := &JsonRpcUpstreamConfig{MaxResponseBytes: &negative}
		require.ErrorContains(t, cfg.Validate(&Config{}), "non-negative")
	})
}

func TestUpstreamResponseTooLargeRetryClassification(t *testing.T) {
	err := NewErrUpstreamResponseTooLarge(1025, 1024)
	require.True(t, HasErrorCode(err, ErrCodeUpstreamResponseTooLarge))
	require.False(t, IsRetryableTowardsUpstream(err))
	require.True(t, IsRetryableTowardNetwork(err))
	require.False(t, HasErrorCode(err, ErrCodeEndpointRequestTooLarge))
}
