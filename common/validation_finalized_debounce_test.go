package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvmNetworkConfig_Validate_FinalizedDebounceDepthMultiplier(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }

	t.Run("nil is valid", func(t *testing.T) {
		require.NoError(t, baseValidEvmNetworkConfig().Validate())
	})

	t.Run("default 0.05 is valid", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.FinalizedDebounceDepthMultiplier = ptr(0.05)
		require.NoError(t, e.Validate())
	})

	t.Run("0 disables and is valid", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.FinalizedDebounceDepthMultiplier = ptr(0)
		require.NoError(t, e.Validate())
	})

	t.Run("just below 1 is valid", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.FinalizedDebounceDepthMultiplier = ptr(0.999)
		require.NoError(t, e.Validate())
	})

	t.Run("1 is rejected", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.FinalizedDebounceDepthMultiplier = ptr(1)
		err := e.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "finalizedDebounceDepthMultiplier")
	})

	t.Run("above 1 is rejected", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.FinalizedDebounceDepthMultiplier = ptr(2)
		err := e.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "finalizedDebounceDepthMultiplier")
	})
}
