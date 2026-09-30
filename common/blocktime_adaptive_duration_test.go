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

func TestBlockTimeAdaptiveDuration_MinMax(t *testing.T) {
	t.Run("ParseYAML", func(t *testing.T) {
		var d BlockTimeAdaptiveDuration
		require.NoError(t, yaml.Unmarshal([]byte("blockTimeMultiplier: 2\nmin: 5s\nmax: 20s\nfallback: 10s\n"), &d))
		assert.Equal(t, BlockTimeAdaptiveDuration{
			BlockTimeMultiplier: 2,
			Min:                 Duration(5 * time.Second),
			Max:                 Duration(20 * time.Second),
			Fallback:            Duration(10 * time.Second),
		}, d)
	})

	t.Run("ParseJSON", func(t *testing.T) {
		var d BlockTimeAdaptiveDuration
		require.NoError(t, SonicCfg.Unmarshal([]byte(`{"blockTimeMultiplier":2,"min":"5s","max":20000,"fallback":"10s"}`), &d))
		assert.Equal(t, 5*time.Second, d.Min.Duration())
		assert.Equal(t, 20*time.Second, d.Max.Duration())
		assert.Equal(t, 10*time.Second, d.Fallback.Duration())
	})

	t.Run("ResolveClamps", func(t *testing.T) {
		d := &BlockTimeAdaptiveDuration{
			BlockTimeMultiplier: 2,
			Min:                 Duration(5 * time.Second),
			Max:                 Duration(20 * time.Second),
			Fallback:            Duration(10 * time.Second),
		}
		assert.Equal(t, 5*time.Second, d.Resolve(250*time.Millisecond, 0), "below min is raised to min")
		assert.Equal(t, 8*time.Second, d.Resolve(4*time.Second, 0), "within bounds is unchanged")
		assert.Equal(t, 20*time.Second, d.Resolve(12*time.Second, 0), "above max is capped at max")
		assert.Equal(t, 10*time.Second, d.Resolve(0, 0), "fallback until block time is known")
	})

	t.Run("OneSidedBound", func(t *testing.T) {
		d := &BlockTimeAdaptiveDuration{BlockTimeMultiplier: 2, Max: Duration(20 * time.Second)}
		assert.Equal(t, 500*time.Millisecond, d.Resolve(250*time.Millisecond, 0))
		assert.Equal(t, 20*time.Second, d.Resolve(time.Minute, 0))
	})

	t.Run("MarshalRoundTrip", func(t *testing.T) {
		fixed, err := SonicCfg.Marshal(FixedDuration(10 * time.Second))
		require.NoError(t, err)
		assert.JSONEq(t, `"10s"`, string(fixed))

		obj := &BlockTimeAdaptiveDuration{BlockTimeMultiplier: 2, Min: Duration(5 * time.Second), Max: Duration(20 * time.Second)}
		raw, err := SonicCfg.Marshal(obj)
		require.NoError(t, err)
		var back BlockTimeAdaptiveDuration
		require.NoError(t, SonicCfg.Unmarshal(raw, &back))
		assert.Equal(t, *obj, back)

		y, err := yaml.Marshal(obj)
		require.NoError(t, err)
		var backY BlockTimeAdaptiveDuration
		require.NoError(t, yaml.Unmarshal(y, &backY))
		assert.Equal(t, *obj, backY)
	})
}

func TestEvmUpstreamConfig_StatePollerInterval(t *testing.T) {
	t.Run("ScalarParsesAsFixed", func(t *testing.T) {
		var e EvmUpstreamConfig
		require.NoError(t, yaml.Unmarshal([]byte("statePollerInterval: 10s\n"), &e))
		require.NoError(t, e.SetDefaults(nil))
		assert.Equal(t, FixedDuration(10*time.Second), e.StatePollerInterval)
	})

	t.Run("ObjectParses", func(t *testing.T) {
		var e EvmUpstreamConfig
		require.NoError(t, yaml.Unmarshal([]byte("statePollerInterval:\n  blockTimeMultiplier: 2\n  min: 5s\n  max: 20s\n  fallback: 10s\n"), &e))
		require.NoError(t, e.SetDefaults(nil))
		assert.Equal(t, 2.0, e.StatePollerInterval.BlockTimeMultiplier)
		assert.Equal(t, 20*time.Second, e.StatePollerInterval.Max.Duration())
	})

	t.Run("UnsetOrZeroGetsDefault", func(t *testing.T) {
		for _, src := range []string{"chainId: 1\n", "statePollerInterval: 0\n"} {
			var e EvmUpstreamConfig
			require.NoError(t, yaml.Unmarshal([]byte(src), &e))
			require.NoError(t, e.SetDefaults(nil))
			assert.Equal(t, FixedDuration(DefaultEvmStatePollerInterval), e.StatePollerInterval, src)
		}
	})

	t.Run("InheritsFromUpstreamDefaultsWithoutSharing", func(t *testing.T) {
		defaults := &UpstreamConfig{Evm: &EvmUpstreamConfig{
			StatePollerInterval: &BlockTimeAdaptiveDuration{BlockTimeMultiplier: 2, Max: Duration(20 * time.Second)},
		}}
		withEvm := &UpstreamConfig{Evm: &EvmUpstreamConfig{ChainId: 1}}
		withoutEvm := &UpstreamConfig{}
		for _, u := range []*UpstreamConfig{withEvm, withoutEvm} {
			require.NoError(t, u.ApplyDefaults(defaults))
			require.NotNil(t, u.Evm)
			assert.Equal(t, *defaults.Evm.StatePollerInterval, *u.Evm.StatePollerInterval)
			assert.NotSame(t, defaults.Evm.StatePollerInterval, u.Evm.StatePollerInterval)
		}

		explicit := &UpstreamConfig{Evm: &EvmUpstreamConfig{StatePollerInterval: FixedDuration(5 * time.Second)}}
		require.NoError(t, explicit.ApplyDefaults(defaults))
		assert.Equal(t, FixedDuration(5*time.Second), explicit.Evm.StatePollerInterval)
	})

	t.Run("Validation", func(t *testing.T) {
		up := &UpstreamConfig{Endpoint: "http://localhost:8545"}
		cases := []struct {
			name    string
			v       *BlockTimeAdaptiveDuration
			wantErr string
		}{
			{"Scalar", FixedDuration(10 * time.Second), ""},
			{"Object", &BlockTimeAdaptiveDuration{BlockTimeMultiplier: 2, Min: Duration(5 * time.Second), Max: Duration(20 * time.Second), Fallback: Duration(10 * time.Second)}, ""},
			{"MultiplierOnly", &BlockTimeAdaptiveDuration{BlockTimeMultiplier: 2}, ""},
			{"Missing", nil, "is required"},
			{"NegativeMultiplier", &BlockTimeAdaptiveDuration{BlockTimeMultiplier: -1, Fallback: Duration(time.Second)}, "blockTimeMultiplier must be >= 0"},
			{"MinAboveMax", &BlockTimeAdaptiveDuration{BlockTimeMultiplier: 2, Min: Duration(30 * time.Second), Max: Duration(20 * time.Second)}, "must be <= max"},
			{"BoundsWithoutMultiplier", &BlockTimeAdaptiveDuration{Fallback: Duration(10 * time.Second), Max: Duration(20 * time.Second)}, "only apply with blockTimeMultiplier"},
			{"NegativeScalar", FixedDuration(-time.Second), "must be >= 0"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := (&EvmUpstreamConfig{StatePollerInterval: tc.v}).Validate(up)
				if tc.wantErr == "" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, tc.wantErr)
				}
			})
		}
	})
}
