package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestTimeoutPolicyConfig_LegacyYAML verifies that pre-DurationSpec configs
// still parse correctly. Old configs declared Duration/Quantile/Min/Max as
// siblings; the new UnmarshalYAML folds those siblings into the unified
// Duration *DurationSpec field.
func TestTimeoutPolicyConfig_LegacyYAML(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		yaml string
		want *TimeoutPolicyConfig
	}{
		{
			name: "legacy scalar duration only",
			yaml: `duration: 5s`,
			want: &TimeoutPolicyConfig{Duration: NewStaticDurationSpec(5 * time.Second)},
		},
		{
			name: "legacy flat with quantile + bounds",
			yaml: `duration: 5s
quantile: 0.99
minDuration: 200ms
maxDuration: 10s`,
			want: &TimeoutPolicyConfig{
				Duration: &DurationSpec{
					Base:     Duration(5 * time.Second),
					Quantile: 0.99,
					Min:      Duration(200 * time.Millisecond),
					Max:      Duration(10 * time.Second),
				},
			},
		},
		{
			name: "new object form",
			yaml: `duration:
  base: 5s
  quantile: 0.99
  min: 200ms
  max: 10s`,
			want: &TimeoutPolicyConfig{
				Duration: &DurationSpec{
					Base:     Duration(5 * time.Second),
					Quantile: 0.99,
					Min:      Duration(200 * time.Millisecond),
					Max:      Duration(10 * time.Second),
				},
			},
		},
		{
			name: "new object form without base",
			yaml: `duration:
  quantile: 0.5
  min: 5ms
  max: 1s`,
			want: &TimeoutPolicyConfig{
				Duration: &DurationSpec{
					Quantile: 0.5,
					Min:      Duration(5 * time.Millisecond),
					Max:      Duration(1 * time.Second),
				},
			},
		},
		{
			name: "object form takes precedence; legacy siblings ignored when set",
			yaml: `duration:
  base: 10s
  quantile: 0.95
quantile: 0.50
minDuration: 1s`,
			want: &TimeoutPolicyConfig{
				Duration: &DurationSpec{
					Base:     Duration(10 * time.Second),
					Quantile: 0.95,
					Min:      Duration(1 * time.Second), // legacy filled because Min was unset
				},
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got TimeoutPolicyConfig
			require.NoError(t, yaml.Unmarshal([]byte(tc.yaml), &got))
			assert.Equal(t, tc.want, &got)
		})
	}
}

// TestHedgePolicyConfig_LegacyYAML verifies the same backward-compat
// behaviour for hedge.
func TestHedgePolicyConfig_LegacyYAML(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		yaml string
		want *HedgePolicyConfig
	}{
		{
			name: "legacy scalar delay only",
			yaml: `delay: 100ms
maxCount: 1`,
			want: &HedgePolicyConfig{
				Delay:    NewStaticDurationSpec(100 * time.Millisecond),
				MaxCount: 1,
			},
		},
		{
			name: "legacy flat with quantile + bounds",
			yaml: `delay: 100ms
maxCount: 2
quantile: 0.95
minDelay: 50ms
maxDelay: 2s`,
			want: &HedgePolicyConfig{
				Delay: &DurationSpec{
					Base:     Duration(100 * time.Millisecond),
					Quantile: 0.95,
					Min:      Duration(50 * time.Millisecond),
					Max:      Duration(2 * time.Second),
				},
				MaxCount: 2,
			},
		},
		{
			name: "new object form",
			yaml: `delay:
  base: 100ms
  quantile: 0.95
  min: 50ms
  max: 2s
maxCount: 2`,
			want: &HedgePolicyConfig{
				Delay: &DurationSpec{
					Base:     Duration(100 * time.Millisecond),
					Quantile: 0.95,
					Min:      Duration(50 * time.Millisecond),
					Max:      Duration(2 * time.Second),
				},
				MaxCount: 2,
			},
		},
		{
			name: "quantile-only (no base) via legacy form",
			yaml: `quantile: 0.7
maxCount: 1`,
			want: &HedgePolicyConfig{
				Delay:    &DurationSpec{Quantile: 0.7},
				MaxCount: 1,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got HedgePolicyConfig
			require.NoError(t, yaml.Unmarshal([]byte(tc.yaml), &got))
			assert.Equal(t, tc.want, &got)
		})
	}
}

// TestConsensusWaitCaps_YAML verifies the wait caps accept both scalar
// shorthand and the object form via the DurationSpec unmarshaler.
func TestConsensusWaitCaps_YAML(t *testing.T) {
	t.Parallel()

	t.Run("scalar shorthand", func(t *testing.T) {
		t.Parallel()
		input := `maxParticipants: 3
agreementThreshold: 2
maxWaitOnResult: 200ms
maxWaitOnEmpty: 800ms`
		var got ConsensusPolicyConfig
		require.NoError(t, yaml.Unmarshal([]byte(input), &got))
		assert.Equal(t, Duration(200*time.Millisecond), got.MaxWaitOnResult.Base)
		assert.Equal(t, Duration(800*time.Millisecond), got.MaxWaitOnEmpty.Base)
	})

	t.Run("object form with quantile", func(t *testing.T) {
		t.Parallel()
		input := `maxParticipants: 3
agreementThreshold: 2
maxWaitOnResult:
  quantile: 0.5
  min: 5ms
  max: 1s
maxWaitOnEmpty:
  quantile: 0.9
  min: 50ms
  max: 2s`
		var got ConsensusPolicyConfig
		require.NoError(t, yaml.Unmarshal([]byte(input), &got))
		assert.Equal(t, 0.5, got.MaxWaitOnResult.Quantile)
		assert.Equal(t, Duration(5*time.Millisecond), got.MaxWaitOnResult.Min)
		assert.Equal(t, Duration(1*time.Second), got.MaxWaitOnResult.Max)
		assert.Equal(t, 0.9, got.MaxWaitOnEmpty.Quantile)
	})
}
