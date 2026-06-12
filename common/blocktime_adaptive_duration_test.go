package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestBlockTimeAdaptiveDuration_Unmarshal(t *testing.T) {
	t.Run("JSONScalarString", func(t *testing.T) {
		var d BlockTimeAdaptiveDuration
		require.NoError(t, SonicCfg.Unmarshal([]byte(`"2s"`), &d))
		assert.Equal(t, 2*time.Second, d.Fallback.Duration())
		assert.Zero(t, d.BlockTimeMultiplier)
	})

	t.Run("JSONScalarNumberMillis", func(t *testing.T) {
		var d BlockTimeAdaptiveDuration
		require.NoError(t, SonicCfg.Unmarshal([]byte(`1500`), &d))
		assert.Equal(t, 1500*time.Millisecond, d.Fallback.Duration())
	})

	t.Run("JSONObject", func(t *testing.T) {
		var d BlockTimeAdaptiveDuration
		require.NoError(t, SonicCfg.Unmarshal([]byte(`{"blockTimeMultiplier":1,"fallback":"2s"}`), &d))
		assert.Equal(t, 1.0, d.BlockTimeMultiplier)
		assert.Equal(t, 2*time.Second, d.Fallback.Duration())
	})

	t.Run("YAMLScalar", func(t *testing.T) {
		var d BlockTimeAdaptiveDuration
		require.NoError(t, yaml.Unmarshal([]byte(`3s`), &d))
		assert.Equal(t, 3*time.Second, d.Fallback.Duration())
		assert.Zero(t, d.BlockTimeMultiplier)
	})

	t.Run("YAMLObject", func(t *testing.T) {
		var d BlockTimeAdaptiveDuration
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

	t.Run("RejectsUnknownKeysYAML", func(t *testing.T) {
		// A quantile-style (AdaptiveDuration) spec in a block-time context must
		// fail loudly, not be silently ignored.
		var d BlockTimeAdaptiveDuration
		err := yaml.Unmarshal([]byte("quantile: 0.95\nmax: 1s\n"), &d)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown field")

		var p CachePolicyConfig
		err = yaml.Unmarshal([]byte("connector: c\nttl:\n  quantile: 0.95\n"), &p)
		require.Error(t, err)
	})

	t.Run("RejectsUnknownKeysJSON", func(t *testing.T) {
		var d BlockTimeAdaptiveDuration
		err := SonicCfg.Unmarshal([]byte(`{"quantile":0.95}`), &d)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown field")
	})
}

// TestBlockTimeAdaptiveDuration_PolicyValidation pins the context restriction:
// a block-time multiplier only makes sense for head freshness, so policies for
// immutable finality states must reject it at validation time.
func TestBlockTimeAdaptiveDuration_PolicyValidation(t *testing.T) {
	cacheCfg := &CacheConfig{
		Connectors: []*ConnectorConfig{{Id: "c", Driver: DriverMemory, Memory: &MemoryConnectorConfig{MaxItems: 1}}},
	}
	base := CachePolicyConfig{
		Connector: "c",
		Network:   "*",
		Method:    "*",
		TTL:       &BlockTimeAdaptiveDuration{BlockTimeMultiplier: 1, Fallback: Duration(2 * time.Second)},
	}

	t.Run("MultiplierAllowedOnRealtime", func(t *testing.T) {
		p := base
		p.Finality = DataFinalityStateRealtime
		require.NoError(t, p.Validate(cacheCfg))
	})

	t.Run("MultiplierRejectedOnNonRealtime", func(t *testing.T) {
		for _, fin := range []DataFinalityState{DataFinalityStateFinalized, DataFinalityStateUnfinalized, DataFinalityStateUnknown} {
			p := base
			p.Finality = fin
			err := p.Validate(cacheCfg)
			require.Error(t, err, fin.String())
			assert.Contains(t, err.Error(), "blockTimeMultiplier")
		}
	})

	t.Run("FixedTTLAllowedOnAnyFinality", func(t *testing.T) {
		p := base
		p.TTL = &BlockTimeAdaptiveDuration{Fallback: Duration(time.Minute)}
		p.Finality = DataFinalityStateFinalized
		require.NoError(t, p.Validate(cacheCfg))
	})

	t.Run("NegativeMultiplierRejected", func(t *testing.T) {
		p := base
		p.Finality = DataFinalityStateRealtime
		p.TTL = &BlockTimeAdaptiveDuration{BlockTimeMultiplier: -1}
		require.Error(t, p.Validate(cacheCfg))
	})
}

func TestBlockTimeAdaptiveDuration_Resolve(t *testing.T) {
	const coldDefault = 2 * time.Second

	t.Run("FixedIgnoresBlockTime", func(t *testing.T) {
		d := FixedDuration(5 * time.Second)
		assert.Equal(t, 5*time.Second, d.Resolve(12*time.Second, coldDefault))
		assert.Equal(t, 5*time.Second, d.Resolve(0, coldDefault))
	})

	t.Run("MultiplierUsesBlockTime", func(t *testing.T) {
		d := &BlockTimeAdaptiveDuration{BlockTimeMultiplier: 1}
		assert.Equal(t, 12*time.Second, d.Resolve(12*time.Second, coldDefault))
		assert.Equal(t, 4*time.Second, (&BlockTimeAdaptiveDuration{BlockTimeMultiplier: 2}).Resolve(2*time.Second, coldDefault))
	})

	t.Run("MultiplierUnknownBlockTimeUsesFallback", func(t *testing.T) {
		d := &BlockTimeAdaptiveDuration{BlockTimeMultiplier: 1, Fallback: Duration(3 * time.Second)}
		assert.Equal(t, 3*time.Second, d.Resolve(0, coldDefault))
	})

	t.Run("MultiplierUnknownNoFallbackUsesColdDefault", func(t *testing.T) {
		d := &BlockTimeAdaptiveDuration{BlockTimeMultiplier: 1}
		assert.Equal(t, coldDefault, d.Resolve(0, coldDefault))
	})

	t.Run("NilAndZero", func(t *testing.T) {
		var d *BlockTimeAdaptiveDuration
		assert.Zero(t, d.Resolve(12*time.Second, coldDefault))
		assert.Zero(t, d.FixedDuration())
		assert.Zero(t, (&BlockTimeAdaptiveDuration{}).Resolve(12*time.Second, coldDefault))
	})
}
