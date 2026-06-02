package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fbValidEvmNetworkConfig returns an EvmNetworkConfig that satisfies the other
// required fields, so each sub-test isolates MaxFutureBlockRetryDistance.
func fbValidEvmNetworkConfig() *EvmNetworkConfig {
	return &EvmNetworkConfig{
		FallbackFinalityDepth:       8,
		FallbackStatePollerDebounce: Duration(time.Second),
		GetLogsMaxAllowedRange:      1000,
	}
}

func TestEvmNetworkConfig_Validate_MaxFutureBlockRetryDistance(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }

	t.Run("nil is valid (feature disabled)", func(t *testing.T) {
		cfg := fbValidEvmNetworkConfig()
		cfg.MaxFutureBlockRetryDistance = nil
		assert.NoError(t, cfg.Validate())
	})

	t.Run("zero is valid", func(t *testing.T) {
		cfg := fbValidEvmNetworkConfig()
		cfg.MaxFutureBlockRetryDistance = i64(0)
		assert.NoError(t, cfg.Validate())
	})

	t.Run("positive is valid", func(t *testing.T) {
		cfg := fbValidEvmNetworkConfig()
		cfg.MaxFutureBlockRetryDistance = i64(16)
		assert.NoError(t, cfg.Validate())
	})

	t.Run("negative is rejected", func(t *testing.T) {
		cfg := fbValidEvmNetworkConfig()
		cfg.MaxFutureBlockRetryDistance = i64(-1)
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "maxFutureBlockRetryDistance")
	})
}
