package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestCovMiscData_FinalityStateStringAndMarshal(t *testing.T) {
	cases := []struct {
		f    DataFinalityState
		want string
	}{
		{DataFinalityStateFinalized, "finalized"},
		{DataFinalityStateUnfinalized, "unfinalized"},
		{DataFinalityStateRealtime, "realtime"},
		{DataFinalityStateUnknown, "unknown"},
		{DataFinalityStateAll, "invalid(-1)"},
		{DataFinalityState(9), "invalid(9)"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tc.f.String())
		y, err := yaml.Marshal(tc.f)
		require.NoError(t, err)
		assert.Equal(t, tc.want+"\n", string(y))
		j, err := tc.f.MarshalJSON()
		require.NoError(t, err)
		assert.Equal(t, `"`+tc.want+`"`, string(j))
	}
}

func TestCovMiscData_FinalityStateUnmarshalYAML(t *testing.T) {
	cases := []struct {
		in      string
		want    DataFinalityState
		wantErr string
	}{
		{"finalized", DataFinalityStateFinalized, ""},
		{"0", DataFinalityStateFinalized, ""},
		{"Unfinalized", DataFinalityStateUnfinalized, ""},
		{"1", DataFinalityStateUnfinalized, ""},
		{"REALTIME", DataFinalityStateRealtime, ""},
		{"2", DataFinalityStateRealtime, ""},
		{"unknown", DataFinalityStateUnknown, ""},
		{"3", DataFinalityStateUnknown, ""},
		{"pending", 0, "invalid data finality state: pending"},
		{"[a]", 0, "cannot unmarshal"},
	}
	for _, tc := range cases {
		var got DataFinalityState
		err := yaml.Unmarshal([]byte(tc.in), &got)
		if tc.wantErr != "" {
			require.ErrorContains(t, err, tc.wantErr, tc.in)
			continue
		}
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}

func TestCovMiscData_CacheEmptyBehavior(t *testing.T) {
	assert.Equal(t, "ignore", CacheEmptyBehaviorIgnore.String())
	assert.Equal(t, "allow", CacheEmptyBehaviorAllow.String())
	assert.Equal(t, "only", CacheEmptyBehaviorOnly.String())
	assert.Equal(t, "invalid(7)", CacheEmptyBehavior(7).String())
	assert.Equal(t, "invalid(-1)", CacheEmptyBehavior(-1).String())

	y, err := yaml.Marshal(CacheEmptyBehaviorAllow)
	require.NoError(t, err)
	assert.Equal(t, "allow\n", string(y))

	cases := []struct {
		in      string
		want    CacheEmptyBehavior
		wantErr string
	}{
		{"ignore", CacheEmptyBehaviorIgnore, ""},
		{"0", CacheEmptyBehaviorIgnore, ""},
		{"ALLOW", CacheEmptyBehaviorAllow, ""},
		{"1", CacheEmptyBehaviorAllow, ""},
		{"only", CacheEmptyBehaviorOnly, ""},
		{"2", CacheEmptyBehaviorOnly, ""},
		{"sometimes", 0, "invalid cache empty behavior: sometimes"},
		{"{a: 1}", 0, "cannot unmarshal"},
	}
	for _, tc := range cases {
		var got CacheEmptyBehavior
		err := yaml.Unmarshal([]byte(tc.in), &got)
		if tc.wantErr != "" {
			require.ErrorContains(t, err, tc.wantErr, tc.in)
			continue
		}
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}

func TestCovMiscData_CachePolicyAppliesTo(t *testing.T) {
	assert.Equal(t, "both", CachePolicyAppliesTo("").String(), "empty defaults to both")
	assert.Equal(t, "get", CachePolicyAppliesToGet.String())
	y, err := yaml.Marshal(CachePolicyAppliesTo(""))
	require.NoError(t, err)
	assert.Equal(t, "both\n", string(y))
	j, err := CachePolicyAppliesToSet.MarshalJSON()
	require.NoError(t, err)
	assert.Equal(t, `"set"`, string(j))

	cases := []struct {
		in      string
		want    CachePolicyAppliesTo
		wantErr bool
	}{
		{`""`, CachePolicyAppliesToBoth, false},
		{`" Both "`, CachePolicyAppliesToBoth, false},
		{`"GET"`, CachePolicyAppliesToGet, false},
		{`"set"`, CachePolicyAppliesToSet, false},
		{`"delete"`, "", true},
	}
	for _, tc := range cases {
		var gotJ, gotY CachePolicyAppliesTo
		errJ := gotJ.UnmarshalJSON([]byte(tc.in))
		errY := yaml.Unmarshal([]byte(tc.in), &gotY)
		if tc.wantErr {
			require.ErrorContains(t, errJ, "invalid cache policy appliesTo")
			require.ErrorContains(t, errY, "invalid cache policy appliesTo")
			continue
		}
		require.NoError(t, errJ, tc.in)
		require.NoError(t, errY, tc.in)
		assert.Equal(t, tc.want, gotJ, tc.in)
		assert.Equal(t, tc.want, gotY, tc.in)
	}
	var a CachePolicyAppliesTo
	require.Error(t, a.UnmarshalJSON([]byte(`123`)))
	require.Error(t, yaml.Unmarshal([]byte(`[x]`), &a))
}

func TestCovMiscDuration_YAMLAndJSON(t *testing.T) {
	var d Duration
	require.NoError(t, yaml.Unmarshal([]byte(`"1m30s"`), &d))
	assert.Equal(t, 90*time.Second, d.Duration())
	assert.Equal(t, "1m30s", d.String())

	err := yaml.Unmarshal([]byte(`"forever"`), &d)
	require.ErrorContains(t, err, "invalid interval/duration format")

	err = yaml.Unmarshal([]byte(`[1]`), &d)
	require.EqualError(t, err, "cannot unmarshal duration value")

	y, err := yaml.Marshal(Duration(1500 * time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, "1.5s\n", string(y))
	j, err := Duration(2 * time.Second).MarshalJSON()
	require.NoError(t, err)
	assert.Equal(t, `"2s"`, string(j))

	p := Duration(3 * time.Second).Ptr()
	*p = Duration(time.Second)
	assert.Equal(t, time.Second, p.Duration())
	assert.Equal(t, 4*time.Second, *Duration(4 * time.Second).DurationPtr())
}

// Numeric scalars decode as strings first under yaml.v3, so the int/float
// millisecond branches are reachable only via a custom unmarshal func.
func TestCovMiscDuration_NumericFallbacks(t *testing.T) {
	asInt := func(v interface{}) error {
		switch p := v.(type) {
		case *int64:
			*p = 250
			return nil
		}
		return assert.AnError
	}
	var d Duration
	require.NoError(t, d.UnmarshalYAML(asInt))
	assert.Equal(t, 250*time.Millisecond, d.Duration())

	asFloat := func(v interface{}) error {
		if p, ok := v.(*float64); ok {
			*p = 12.9
			return nil
		}
		return assert.AnError
	}
	require.NoError(t, d.UnmarshalYAML(asFloat))
	assert.Equal(t, 12*time.Millisecond, d.Duration())
}

func TestCovMiscAdaptiveDuration_ResolveClampsAndQuantile(t *testing.T) {
	cases := []struct {
		name string
		d    *AdaptiveDuration
		qt   QuantileTracker
		want time.Duration
	}{
		{"nil", nil, nil, 0},
		{"static base ignores min/max", &AdaptiveDuration{Base: Duration(5 * time.Millisecond), Min: Duration(time.Second)}, nil, 5 * time.Millisecond},
		{"quantile cold start uses min", &AdaptiveDuration{Quantile: 0.9, Min: Duration(100 * time.Millisecond), Max: Duration(time.Second)}, nil, 100 * time.Millisecond},
		{"base plus quantile", &AdaptiveDuration{Base: Duration(10 * time.Millisecond), Quantile: 0.5}, &fakeQuantile{val: 20 * time.Millisecond}, 30 * time.Millisecond},
		{"clamped to max", &AdaptiveDuration{Quantile: 0.5, Max: Duration(50 * time.Millisecond)}, &fakeQuantile{val: time.Second}, 50 * time.Millisecond},
		{"raised to min", &AdaptiveDuration{Quantile: 0.5, Min: Duration(40 * time.Millisecond), Max: Duration(time.Second)}, &fakeQuantile{val: time.Millisecond}, 40 * time.Millisecond},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tc.d.Resolve(tc.qt), tc.name)
	}
}

func TestCovMiscAdaptiveDuration_Validate(t *testing.T) {
	ms := func(n int) Duration { return Duration(time.Duration(n) * time.Millisecond) }
	cases := []struct {
		d       *AdaptiveDuration
		wantErr string
	}{
		{nil, ""},
		{&AdaptiveDuration{Base: ms(1)}, ""},
		{&AdaptiveDuration{Quantile: 1.5, Base: ms(1)}, "x.quantile must be between 0 and 1"},
		{&AdaptiveDuration{Quantile: -0.1, Base: ms(1)}, "x.quantile must be between 0 and 1"},
		{&AdaptiveDuration{Quantile: 0.5}, "x requires base or max when quantile is set"},
		{&AdaptiveDuration{}, "x must specify at least one of base/quantile/min/max"},
		{&AdaptiveDuration{Base: ms(1), Min: ms(10), Max: ms(5)}, "x.min must be <= x.max"},
		{&AdaptiveDuration{Quantile: 0.5, Max: ms(5)}, ""},
	}
	for _, tc := range cases {
		err := tc.d.validate("x")
		if tc.wantErr == "" {
			assert.NoError(t, err)
		} else {
			assert.EqualError(t, err, tc.wantErr)
		}
	}
}

func TestCovMiscAdaptiveDuration_InheritFrom(t *testing.T) {
	src := &AdaptiveDuration{Base: Duration(1), Quantile: 0.5, Min: Duration(2), Max: Duration(3)}
	d := &AdaptiveDuration{Base: Duration(9)}
	d.inheritFrom(src)
	assert.Equal(t, &AdaptiveDuration{Base: Duration(9), Quantile: 0.5, Min: Duration(2), Max: Duration(3)}, d, "set fields win, zero fields inherit")

	full := &AdaptiveDuration{Base: Duration(7), Quantile: 0.1, Min: Duration(8), Max: Duration(9)}
	before := *full
	full.inheritFrom(src)
	assert.Equal(t, before, *full)

	full.inheritFrom(nil)
	(*AdaptiveDuration)(nil).inheritFrom(src)
	assert.Equal(t, before, *full)
	assert.Nil(t, (*AdaptiveDuration)(nil).Copy())
}

func TestCovMiscAdaptiveDuration_UnmarshalForms(t *testing.T) {
	var d AdaptiveDuration
	require.NoError(t, yaml.Unmarshal([]byte(`{base: 100ms, quantile: 0.9, min: 50ms, max: 2s}`), &d))
	assert.Equal(t, AdaptiveDuration{Base: Duration(100 * time.Millisecond), Quantile: 0.9, Min: Duration(50 * time.Millisecond), Max: Duration(2 * time.Second)}, d)

	err := yaml.Unmarshal([]byte(`[1, 2]`), &d)
	require.ErrorContains(t, err, "AdaptiveDuration must be a duration scalar or {base, quantile, min, max} object")

	// int/float branches via a custom unmarshal func (yaml.v3 decodes ints into strings first)
	d = AdaptiveDuration{}
	require.NoError(t, d.UnmarshalYAML(func(v interface{}) error {
		if p, ok := v.(*int64); ok {
			*p = 300
			return nil
		}
		return assert.AnError
	}))
	assert.Equal(t, Duration(300*time.Millisecond), d.Base)
	require.NoError(t, d.UnmarshalYAML(func(v interface{}) error {
		if p, ok := v.(*float64); ok {
			*p = 7.5
			return nil
		}
		return assert.AnError
	}))
	assert.Equal(t, Duration(7*time.Millisecond), d.Base)

	jsonCases := []struct {
		in      string
		want    AdaptiveDuration
		wantErr string
	}{
		{`null`, AdaptiveDuration{}, ""},
		{`"250ms"`, AdaptiveDuration{Base: Duration(250 * time.Millisecond)}, ""},
		{`"abc"`, AdaptiveDuration{}, `invalid duration scalar "abc"`},
		{`"\x"`, AdaptiveDuration{}, "invalid"},
		{`125`, AdaptiveDuration{Base: Duration(125 * time.Millisecond)}, ""},
		{`true`, AdaptiveDuration{}, "AdaptiveDuration must be a duration scalar"},
		{`{"base": 10, "quantile": 0.5, "min": "1ms", "max": null}`, AdaptiveDuration{Base: Duration(10 * time.Millisecond), Quantile: 0.5, Min: Duration(time.Millisecond)}, ""},
		{`{"base": "bad"}`, AdaptiveDuration{}, "AdaptiveDuration.base: invalid duration"},
		{`{"min": "bad"}`, AdaptiveDuration{}, "AdaptiveDuration.min: invalid duration"},
		{`{"max": true}`, AdaptiveDuration{}, "AdaptiveDuration.max"},
		{`{"base": "\x"}`, AdaptiveDuration{}, "AdaptiveDuration"},
		{`{"quantile": "x"}`, AdaptiveDuration{}, "AdaptiveDuration object"},
	}
	for _, tc := range jsonCases {
		var got AdaptiveDuration
		err := got.UnmarshalJSON([]byte(tc.in))
		if tc.wantErr != "" {
			require.ErrorContains(t, err, tc.wantErr, tc.in)
			continue
		}
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}

func TestCovMiscAdaptiveDuration_MarshalJSONAndCtor(t *testing.T) {
	var nilD *AdaptiveDuration
	b, err := nilD.MarshalJSON()
	require.NoError(t, err)
	assert.Equal(t, "null", string(b))

	d := NewStaticDuration(3 * time.Second)
	assert.Equal(t, Duration(3*time.Second), d.Base)
	b, err = d.MarshalJSON()
	require.NoError(t, err)
	assert.JSONEq(t, `{"base":"3s"}`, string(b))

	var back AdaptiveDuration
	require.NoError(t, back.UnmarshalJSON(b))
	assert.Equal(t, *d, back, "object form round-trips")
}

func TestCovMiscAdaptiveDuration_ResolveForRequest(t *testing.T) {
	qd := &AdaptiveDuration{Base: Duration(10 * time.Millisecond), Quantile: 0.9, Min: Duration(5 * time.Millisecond), Max: Duration(time.Second)}
	ntw := &covMiscNetwork{id: "evm:1", metrics: map[string]TrackedMetrics{
		"eth_call": &covMiscMetrics{q: &fakeQuantile{val: 90 * time.Millisecond}},
	}}
	mkReq := func(method string, withNet bool) *NormalizedRequest {
		r := NewNormalizedRequestFromJsonRpcRequest(NewJsonRpcRequest(method, nil))
		if withNet {
			r.SetNetwork(ntw)
		}
		return r
	}

	var zero *AdaptiveDuration
	assert.Equal(t, time.Duration(0), zero.ResolveForRequest(mkReq("eth_call", true)))
	assert.Equal(t, time.Duration(0), qd.ResolveForRequest(nil))
	assert.Equal(t, 7*time.Millisecond, NewStaticDuration(7*time.Millisecond).ResolveForRequest(mkReq("eth_call", true)))
	assert.Equal(t, 15*time.Millisecond, qd.ResolveForRequest(mkReq("eth_call", false)), "no network: cold start base+min")
	assert.Equal(t, 15*time.Millisecond, qd.ResolveForRequest(mkReq("", true)), "empty method: cold start")
	assert.Equal(t, 15*time.Millisecond, qd.ResolveForRequest(mkReq("eth_getLogs", true)), "no tracker: cold start")
	assert.Equal(t, 100*time.Millisecond, qd.ResolveForRequest(mkReq("eth_call", true)))
}
