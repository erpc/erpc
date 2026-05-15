package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestDurationSpec_UnmarshalYAML(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		want    DurationSpec
		wantErr bool
	}{
		{
			name:  "scalar string sets Base only",
			input: `caps: 500ms`,
			want:  DurationSpec{Base: Duration(500 * time.Millisecond)},
		},
		{
			name:  "scalar number sets Base in milliseconds",
			input: `caps: 250`,
			want:  DurationSpec{Base: Duration(250 * time.Millisecond)},
		},
		{
			name: "object with quantile + bounds",
			input: `caps:
  quantile: 0.5
  min: 5ms
  max: 1s`,
			want: DurationSpec{
				Quantile: 0.5,
				Min:      Duration(5 * time.Millisecond),
				Max:      Duration(1 * time.Second),
			},
		},
		{
			name: "object with all four fields",
			input: `caps:
  base: 100ms
  quantile: 0.9
  min: 50ms
  max: 2s`,
			want: DurationSpec{
				Base:     Duration(100 * time.Millisecond),
				Quantile: 0.9,
				Min:      Duration(50 * time.Millisecond),
				Max:      Duration(2 * time.Second),
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var wrapper struct {
				Caps DurationSpec `yaml:"caps"`
			}
			err := yaml.Unmarshal([]byte(tc.input), &wrapper)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, wrapper.Caps)
		})
	}
}

func TestDurationSpec_UnmarshalJSON(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		want    DurationSpec
		wantErr bool
	}{
		{
			name:  "string scalar",
			input: `"750ms"`,
			want:  DurationSpec{Base: Duration(750 * time.Millisecond)},
		},
		{
			name:  "number scalar (ms)",
			input: `1000`,
			want:  DurationSpec{Base: Duration(1 * time.Second)},
		},
		{
			name:  "object",
			input: `{"base": "100ms", "quantile": 0.5, "min": "5ms", "max": "1s"}`,
			want: DurationSpec{
				Base:     Duration(100 * time.Millisecond),
				Quantile: 0.5,
				Min:      Duration(5 * time.Millisecond),
				Max:      Duration(1 * time.Second),
			},
		},
		{
			name:  "null is no-op",
			input: `null`,
			want:  DurationSpec{},
		},
		{
			name:    "invalid string fails",
			input:   `"not-a-duration"`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got DurationSpec
			err := got.UnmarshalJSON([]byte(tc.input))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// fakeQuantile is a test stub for QuantileTracker.
type fakeQuantile struct{ val time.Duration }

func (f *fakeQuantile) Add(_ float64)                        {}
func (f *fakeQuantile) GetQuantile(_ float64) time.Duration  { return f.val }
func (f *fakeQuantile) Reset()                               {}

func TestDurationSpec_Resolve(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		spec *DurationSpec
		qt   QuantileTracker
		want time.Duration
	}{
		{
			name: "nil spec returns 0",
			spec: nil,
			want: 0,
		},
		{
			name: "zero spec returns 0",
			spec: &DurationSpec{},
			want: 0,
		},
		{
			name: "static base only",
			spec: &DurationSpec{Base: Duration(200 * time.Millisecond)},
			want: 200 * time.Millisecond,
		},
		{
			name: "quantile with data uses quantile value",
			spec: &DurationSpec{Quantile: 0.5, Min: Duration(5 * time.Millisecond), Max: Duration(1 * time.Second)},
			qt:   &fakeQuantile{val: 100 * time.Millisecond},
			want: 100 * time.Millisecond,
		},
		{
			name: "quantile cold start falls back to min",
			spec: &DurationSpec{Quantile: 0.5, Min: Duration(5 * time.Millisecond), Max: Duration(1 * time.Second)},
			qt:   &fakeQuantile{val: 0},
			want: 5 * time.Millisecond,
		},
		{
			name: "quantile nil tracker falls back to min",
			spec: &DurationSpec{Quantile: 0.5, Min: Duration(5 * time.Millisecond), Max: Duration(1 * time.Second)},
			qt:   nil,
			want: 5 * time.Millisecond,
		},
		{
			name: "base + quantile additive",
			spec: &DurationSpec{Base: Duration(100 * time.Millisecond), Quantile: 0.5, Max: Duration(1 * time.Second)},
			qt:   &fakeQuantile{val: 200 * time.Millisecond},
			want: 300 * time.Millisecond,
		},
		{
			name: "max clamps high values",
			spec: &DurationSpec{Quantile: 0.99, Min: Duration(5 * time.Millisecond), Max: Duration(1 * time.Second)},
			qt:   &fakeQuantile{val: 5 * time.Second},
			want: 1 * time.Second,
		},
		{
			// Static specs (Quantile == 0) return Base unchanged — Min/Max
			// don't apply. This preserves legacy hedge/timeout semantics:
			// `delay: 10ms` means exactly 10ms even if a Min default exists.
			name: "static base ignores min clamp",
			spec: &DurationSpec{Base: Duration(2 * time.Millisecond), Min: Duration(10 * time.Millisecond)},
			want: 2 * time.Millisecond,
		},
		{
			// When Quantile > 0, the Min floors the (base + adaptive) sum.
			name: "min clamps adaptive value",
			spec: &DurationSpec{Quantile: 0.5, Min: Duration(10 * time.Millisecond)},
			qt:   &fakeQuantile{val: 2 * time.Millisecond},
			want: 10 * time.Millisecond,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.spec.Resolve(tc.qt)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDurationSpec_IsZero(t *testing.T) {
	t.Parallel()
	assert.True(t, (*DurationSpec)(nil).IsZero())
	assert.True(t, (&DurationSpec{}).IsZero())
	assert.False(t, (&DurationSpec{Base: Duration(1)}).IsZero())
	assert.False(t, (&DurationSpec{Quantile: 0.5}).IsZero())
	assert.False(t, (&DurationSpec{Min: Duration(1)}).IsZero())
	assert.False(t, (&DurationSpec{Max: Duration(1)}).IsZero())
}

func TestDurationSpec_Copy(t *testing.T) {
	t.Parallel()
	orig := &DurationSpec{Base: Duration(100 * time.Millisecond), Quantile: 0.5}
	copied := orig.Copy()
	assert.Equal(t, orig, copied)
	copied.Base = Duration(999 * time.Millisecond)
	assert.NotEqual(t, orig.Base, copied.Base)
	assert.Nil(t, (*DurationSpec)(nil).Copy())
}
