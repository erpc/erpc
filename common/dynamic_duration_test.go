package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestDynamicDuration_Unmarshal(t *testing.T) {
	t.Run("JSONScalarString", func(t *testing.T) {
		var d DynamicDuration
		require.NoError(t, SonicCfg.Unmarshal([]byte(`"2s"`), &d))
		assert.Equal(t, 2*time.Second, d.Fallback.Duration())
		assert.Zero(t, d.BlockTimeMultiplier)
	})

	t.Run("JSONScalarNumberMillis", func(t *testing.T) {
		var d DynamicDuration
		require.NoError(t, SonicCfg.Unmarshal([]byte(`1500`), &d))
		assert.Equal(t, 1500*time.Millisecond, d.Fallback.Duration())
	})

	t.Run("JSONObject", func(t *testing.T) {
		var d DynamicDuration
		require.NoError(t, SonicCfg.Unmarshal([]byte(`{"blockTimeMultiplier":1,"fallback":"2s"}`), &d))
		assert.Equal(t, 1.0, d.BlockTimeMultiplier)
		assert.Equal(t, 2*time.Second, d.Fallback.Duration())
	})

	t.Run("YAMLScalar", func(t *testing.T) {
		var d DynamicDuration
		require.NoError(t, yaml.Unmarshal([]byte(`3s`), &d))
		assert.Equal(t, 3*time.Second, d.Fallback.Duration())
		assert.Zero(t, d.BlockTimeMultiplier)
	})

	t.Run("YAMLObject", func(t *testing.T) {
		var d DynamicDuration
		require.NoError(t, yaml.Unmarshal([]byte("blockTimeMultiplier: 1.5\nfallback: 2s\n"), &d))
		assert.Equal(t, 1.5, d.BlockTimeMultiplier)
		assert.Equal(t, 2*time.Second, d.Fallback.Duration())
	})

	t.Run("AsCachePolicyField", func(t *testing.T) {
		// Both forms parse where the field is used (CachePolicyConfig.TTL).
		var fixed CachePolicyConfig
		require.NoError(t, yaml.Unmarshal([]byte("connector: c\nttl: 5s\n"), &fixed))
		assert.Equal(t, 5*time.Second, fixed.TTL.FixedDuration())

		var dyn CachePolicyConfig
		require.NoError(t, yaml.Unmarshal([]byte("connector: c\nttl:\n  blockTimeMultiplier: 1\n  fallback: 2s\n"), &dyn))
		assert.Equal(t, 1.0, dyn.TTL.BlockTimeMultiplier)
		assert.Equal(t, 2*time.Second, dyn.TTL.Fallback.Duration())
	})
}

func TestDynamicDuration_Resolve(t *testing.T) {
	const coldDefault = 2 * time.Second

	t.Run("FixedIgnoresBlockTime", func(t *testing.T) {
		d := FixedDuration(5 * time.Second)
		assert.Equal(t, 5*time.Second, d.Resolve(12*time.Second, coldDefault))
		assert.Equal(t, 5*time.Second, d.Resolve(0, coldDefault))
	})

	t.Run("MultiplierUsesBlockTime", func(t *testing.T) {
		d := &DynamicDuration{BlockTimeMultiplier: 1}
		assert.Equal(t, 12*time.Second, d.Resolve(12*time.Second, coldDefault))
		assert.Equal(t, 4*time.Second, (&DynamicDuration{BlockTimeMultiplier: 2}).Resolve(2*time.Second, coldDefault))
	})

	t.Run("MultiplierUnknownBlockTimeUsesFallback", func(t *testing.T) {
		d := &DynamicDuration{BlockTimeMultiplier: 1, Fallback: Duration(3 * time.Second)}
		assert.Equal(t, 3*time.Second, d.Resolve(0, coldDefault))
	})

	t.Run("MultiplierUnknownNoFallbackUsesColdDefault", func(t *testing.T) {
		d := &DynamicDuration{BlockTimeMultiplier: 1}
		assert.Equal(t, coldDefault, d.Resolve(0, coldDefault))
	})

	t.Run("NilAndZero", func(t *testing.T) {
		var d *DynamicDuration
		assert.Zero(t, d.Resolve(12*time.Second, coldDefault))
		assert.Zero(t, d.FixedDuration())
		assert.Zero(t, (&DynamicDuration{}).Resolve(12*time.Second, coldDefault))
	})
}
