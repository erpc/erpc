package common

import (
	"reflect"
	"testing"
	"time"

	"github.com/grafana/sobek"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type covMiscJsInner struct {
	Name string `json:"name"`
}

type covMiscJsTarget struct {
	Flag      bool                      `json:"flag"`
	Count     int                       `json:"count"`
	Size      uint32                    `json:"size"`
	Ratio     float64                   `json:"ratio"`
	Label     string                    `json:"label,omitempty"`
	Untagged  string                    // falls back to the Go field name
	Timeout   Duration                  `json:"timeout"`
	Period    RateLimitPeriod           `json:"period"`
	Inner     covMiscJsInner            `json:"inner"`
	Ptr       *int                      `json:"ptr"`
	PtrNull   *int                      `json:"ptrNull"`
	Tags      []string                  `json:"tags"`
	Nested    []covMiscJsInner          `json:"nested"`
	Fn        sobek.Callable            `json:"fn"`
	Labels    map[string]string         `json:"labels"`
	NullMap   map[string]int            `json:"nullMap"`
	Any       interface{}               `json:"any"`
	Skipped   string                    `json:"skipped"`
	unexposed string                    //nolint:unused
	Objects   map[string]covMiscJsInner `json:"objects"`
}

func covMiscEval(t *testing.T, src string) sobek.Value {
	t.Helper()
	rt, err := NewRuntime()
	require.NoError(t, err)
	v, err := rt.Evaluate(src)
	require.NoError(t, err)
	return v
}

func TestCovMiscObject_MapsEveryKind(t *testing.T) {
	v := covMiscEval(t, `({
		flag: true, count: 42, size: 7, ratio: 0.25, label: "lbl", Untagged: "u",
		timeout: "1500ms", period: "hour",
		inner: {name: "in"},
		ptr: 9, ptrNull: null,
		tags: ["a", "b"],
		nested: [{name: "n0"}, {name: "n1"}],
		fn: function(x) { return x * 2 },
		labels: {k1: "v1", k2: "v2"},
		nullMap: null,
		any: {deep: [1, "two"]},
		objects: {o: {name: "obj"}},
	})`)

	var got covMiscJsTarget
	got.Skipped = "keep"
	require.NoError(t, MapJavascriptObjectToGo(v, &got))

	assert.True(t, got.Flag)
	assert.Equal(t, 42, got.Count)
	assert.Equal(t, uint32(7), got.Size)
	assert.Equal(t, 0.25, got.Ratio)
	assert.Equal(t, "lbl", got.Label)
	assert.Equal(t, "u", got.Untagged)
	assert.Equal(t, Duration(1500*time.Millisecond), got.Timeout)
	assert.Equal(t, RateLimitPeriodHour, got.Period)
	assert.Equal(t, "in", got.Inner.Name)
	require.NotNil(t, got.Ptr)
	assert.Equal(t, 9, *got.Ptr)
	assert.Nil(t, got.PtrNull)
	assert.Equal(t, []string{"a", "b"}, got.Tags)
	assert.Equal(t, []covMiscJsInner{{"n0"}, {"n1"}}, got.Nested)
	require.NotNil(t, got.Fn)
	res, err := got.Fn(sobek.Undefined(), covMiscEval(t, "21"))
	require.NoError(t, err)
	assert.Equal(t, int64(42), res.ToInteger())
	assert.Equal(t, map[string]string{"k1": "v1", "k2": "v2"}, got.Labels)
	assert.Nil(t, got.NullMap)
	assert.Equal(t, map[string]interface{}{"deep": []interface{}{int64(1), "two"}}, got.Any)
	assert.Equal(t, "keep", got.Skipped, "absent JS keys leave fields untouched")
	assert.Equal(t, map[string]covMiscJsInner{"o": {"obj"}}, got.Objects)
}

func TestCovMiscObject_NumericDurationIsMilliseconds(t *testing.T) {
	var got struct {
		Timeout Duration `json:"timeout"`
	}
	require.NoError(t, MapJavascriptObjectToGo(covMiscEval(t, `({timeout: 250})`), &got))
	assert.Equal(t, Duration(250*time.Millisecond), got.Timeout)
}

func TestCovMiscObject_RateLimitPeriodForms(t *testing.T) {
	type target struct {
		P RateLimitPeriod `json:"p"`
	}
	cases := []struct {
		js      string
		want    RateLimitPeriod
		wantErr string
	}{
		{`"second"`, RateLimitPeriodSecond, ""},
		{`" 1M "`, RateLimitPeriodMinute, ""},
		{`"60s"`, RateLimitPeriodMinute, ""},
		{`"3600s"`, RateLimitPeriodHour, ""},
		{`"1d"`, RateLimitPeriodDay, ""},
		{`"7d"`, RateLimitPeriodWeek, ""},
		{`"720h"`, RateLimitPeriodMonth, ""},
		{`"year"`, RateLimitPeriodYear, ""},
		{`"fortnight"`, 0, "invalid rate limit period: fortnight"},
		{`4`, RateLimitPeriodWeek, ""},
		{`99`, 0, "invalid rate limit period: 99"},
		{`true`, 0, "invalid rate limit period type: bool"},
	}
	for _, tc := range cases {
		t.Run(tc.js, func(t *testing.T) {
			var got target
			err := MapJavascriptObjectToGo(covMiscEval(t, "({p: "+tc.js+"})"), &got)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Contains(t, err.Error(), "failed to set field p")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.P)
		})
	}
}

func TestCovMiscObject_Errors(t *testing.T) {
	obj := covMiscEval(t, `({})`)

	var notStruct int
	require.EqualError(t, MapJavascriptObjectToGo(obj, notStruct), "dest must be a non-nil pointer")
	var nilPtr *covMiscJsInner
	require.EqualError(t, MapJavascriptObjectToGo(obj, nilPtr), "dest must be a non-nil pointer")
	require.EqualError(t, MapJavascriptObjectToGo(obj, &notStruct), "dest must point to a struct")

	var inner covMiscJsInner
	require.NoError(t, MapJavascriptObjectToGo(nil, &inner))
	require.NoError(t, MapJavascriptObjectToGo(sobek.Undefined(), &inner))
	require.NoError(t, MapJavascriptObjectToGo(sobek.Null(), &inner))
	assert.Empty(t, inner.Name)

	cases := []struct {
		name    string
		dest    interface{}
		js      string
		wantErr string
	}{
		{"bad duration", &struct {
			D Duration `json:"d"`
		}{}, `({d: "soon"})`, "invalid duration format"},
		{"array expected", &struct {
			S []string `json:"s"`
		}{}, `({s: "nope"})`, "expected array but got nope"},
		{"array undefined element", &struct {
			S []string `json:"s"`
		}{}, `({s: [undefined, "x"]})`, "expected array element 0"},
		{"array element error", &struct {
			S []Duration `json:"s"`
		}{}, `({s: ["bad"]})`, "invalid duration format"},
		{"pointer element error", &struct {
			P *Duration `json:"p"`
		}{}, `({p: "bad"})`, "invalid duration format"},
		{"func from object", &struct {
			F sobek.Callable `json:"f"`
		}{}, `({f: {}})`, "field is not a function"},
		{"func from primitive", &struct {
			F sobek.Callable `json:"f"`
		}{}, `({f: 1})`, "field is not a function"},
		{"map from primitive", &struct {
			M map[string]string `json:"m"`
		}{}, `({m: 5})`, "expected object but got 5"},
		{"map non-string key", &struct {
			M map[int]string `json:"m"`
		}{}, `({m: {a: "b"}})`, "unsupported map key type: int"},
		{"map value error", &struct {
			M map[string]Duration `json:"m"`
		}{}, `({m: {a: "bad"}})`, "failed to set map value for key a"},
		{"nested struct error", &struct {
			In struct {
				D Duration `json:"d"`
			} `json:"in"`
		}{}, `({in: {d: "bad"}})`, "failed to set field d"},
		{"unsupported kind", &struct {
			C chan int `json:"c"`
		}{}, `({c: 1})`, "unsupported kind: chan"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, MapJavascriptObjectToGo(covMiscEval(t, tc.js), tc.dest), tc.wantErr)
		})
	}
}

func TestCovMiscObject_MapSkipsUndefinedValues(t *testing.T) {
	var got struct {
		M map[string]string `json:"m"`
	}
	require.NoError(t, MapJavascriptObjectToGo(covMiscEval(t, `({m: {a: "x", b: undefined}})`), &got))
	assert.Equal(t, map[string]string{"a": "x"}, got.M)
}

func TestCovMiscObject_UnsettableFieldIsNoop(t *testing.T) {
	var s struct{ hidden string }
	fv := reflect.ValueOf(&s).Elem().Field(0)
	require.NoError(t, setJsValueToGoField(covMiscEval(t, `"x"`), fv))
	assert.Empty(t, s.hidden)
}
