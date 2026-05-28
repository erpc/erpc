package policy

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/grafana/sobek"
	"github.com/stretchr/testify/require"
)

// TestInstallSharedHelpers_LatencyP — the VM-wide `__sharedLatencyP`
// must reproduce the legacy per-object closure's behavior using
// `this`-binding instead of captured locals. Mirrors every branch
// of the snap-to-bucket logic so a future refactor of the percentile
// floor → bucket mapping won't silently drift.
func TestInstallSharedHelpers_LatencyP(t *testing.T) {
	rt, err := common.NewRuntime()
	require.NoError(t, err)
	require.NoError(t, installSharedHelpers(rt))
	vm := rt.VM()

	mObj := vm.NewObject()
	_ = mObj.Set("p50ResponseSeconds", 0.010)
	_ = mObj.Set("p70ResponseSeconds", 0.020)
	_ = mObj.Set("p90ResponseSeconds", 0.040)
	_ = mObj.Set("p95ResponseSeconds", 0.080)
	_ = mObj.Set("p99ResponseSeconds", 0.250)
	// Attach the shared function so we can invoke it through `this`.
	_ = mObj.Set("latencyP", vm.GlobalObject().Get(sharedHelperLatencyP))

	cases := []struct {
		name string
		arg  float64
		want float64 // ms
	}{
		// Fraction form
		{"p50 frac", 0.50, 10},
		{"p70 frac", 0.70, 20},
		{"p90 frac", 0.90, 40},
		{"p95 frac", 0.95, 80},
		{"p99 frac", 0.99, 250},
		// Percentile form (>1)
		{"p50 percentile", 50, 10},
		{"p99 percentile", 99, 250},
		// Below p50 → snaps up to p50
		{"below p50 frac", 0.20, 10},
		{"zero frac", 0, 10},
		// Between p70 and p90 → snaps up to p90
		{"between p70/p90", 0.80, 40},
		// Between p95 and p99 → snaps to p99
		{"between p95/p99", 0.97, 250},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = vm.GlobalObject().Set("_m", mObj)
			val, err := vm.RunString("_m.latencyP(" + sobekNum(tc.arg) + ")")
			require.NoError(t, err)
			require.InDelta(t, tc.want, val.ToFloat(), 0.001,
				"latencyP(%v) — should snap to expected bucket × 1000", tc.arg)
		})
	}
}

// TestInstallSharedHelpers_LatencyP_NoArg — invoking with no args
// must return 0 (matches legacy closure behavior).
func TestInstallSharedHelpers_LatencyP_NoArg(t *testing.T) {
	rt, err := common.NewRuntime()
	require.NoError(t, err)
	require.NoError(t, installSharedHelpers(rt))
	vm := rt.VM()
	mObj := vm.NewObject()
	_ = mObj.Set("p50ResponseSeconds", 0.123)
	_ = mObj.Set("latencyP", vm.GlobalObject().Get(sharedHelperLatencyP))
	_ = vm.GlobalObject().Set("_m", mObj)
	val, err := vm.RunString("_m.latencyP()")
	require.NoError(t, err)
	require.Equal(t, 0.0, val.ToFloat())
}

// TestInstallSharedHelpers_HasTag — `__sharedHasTag` must read
// `this.tags` correctly. Empty tags → always false. Matching tag →
// true. Non-matching → false.
func TestInstallSharedHelpers_HasTag(t *testing.T) {
	rt, err := common.NewRuntime()
	require.NoError(t, err)
	require.NoError(t, installSharedHelpers(rt))
	vm := rt.VM()

	hasTagFn := vm.GlobalObject().Get(sharedHelperHasTag)

	t.Run("matching tag returns true", func(t *testing.T) {
		obj := vm.NewObject()
		_ = obj.Set("tags", vm.ToValue([]string{"tier:hot", "region:us-east"}))
		_ = obj.Set("hasTag", hasTagFn)
		_ = vm.GlobalObject().Set("_u", obj)
		val, err := vm.RunString(`_u.hasTag("tier:hot")`)
		require.NoError(t, err)
		require.True(t, val.ToBoolean())
	})

	t.Run("non-matching tag returns false", func(t *testing.T) {
		obj := vm.NewObject()
		_ = obj.Set("tags", vm.ToValue([]string{"tier:hot"}))
		_ = obj.Set("hasTag", hasTagFn)
		_ = vm.GlobalObject().Set("_u", obj)
		val, err := vm.RunString(`_u.hasTag("tier:cold")`)
		require.NoError(t, err)
		require.False(t, val.ToBoolean())
	})

	t.Run("empty tags returns false", func(t *testing.T) {
		obj := vm.NewObject()
		_ = obj.Set("tags", vm.ToValue([]string{}))
		_ = obj.Set("hasTag", hasTagFn)
		_ = vm.GlobalObject().Set("_u", obj)
		val, err := vm.RunString(`_u.hasTag("anything")`)
		require.NoError(t, err)
		require.False(t, val.ToBoolean())
	})

	t.Run("no-arg returns false", func(t *testing.T) {
		obj := vm.NewObject()
		_ = obj.Set("tags", vm.ToValue([]string{"tier:hot"}))
		_ = obj.Set("hasTag", hasTagFn)
		_ = vm.GlobalObject().Set("_u", obj)
		val, err := vm.RunString(`_u.hasTag()`)
		require.NoError(t, err)
		require.False(t, val.ToBoolean())
	})
}

// TestInstallSharedHelpers_SingletonValueReused — the SAME sobek
// function value must be reused across many objects without
// re-wrapping. This is the entire point of the singleton: a fresh
// `vm.ToValue(goFn)` per attach would defeat the optimization.
func TestInstallSharedHelpers_SingletonValueReused(t *testing.T) {
	rt, err := common.NewRuntime()
	require.NoError(t, err)
	require.NoError(t, installSharedHelpers(rt))
	vm := rt.VM()

	v1 := vm.GlobalObject().Get(sharedHelperLatencyP)
	v2 := vm.GlobalObject().Get(sharedHelperLatencyP)
	require.NotNil(t, v1)
	require.NotNil(t, v2)
	// Same underlying wrapper — `Get` returns the stored Value verbatim.
	require.True(t, v1.Equals(v2),
		"sharedHelperLatencyP must be a stable singleton across Get calls")

	v3 := vm.GlobalObject().Get(sharedHelperHasTag)
	v4 := vm.GlobalObject().Get(sharedHelperHasTag)
	require.True(t, v3.Equals(v4),
		"sharedHelperHasTag must be a stable singleton across Get calls")
}

// sobekNum stringifies a float for sobek's RunString.
func sobekNum(f float64) string {
	// Sobek parses standard JS numeric literals. Use %g for compact
	// representation; integers stay integer-looking.
	if f == float64(int64(f)) {
		return sobekIntegerString(int64(f))
	}
	return sobekFloatString(f)
}

func sobekIntegerString(i int64) string {
	// Avoid strconv import in the test for a single use.
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = digits[i%10]
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}

func sobekFloatString(f float64) string {
	// Minimal %g-style formatter using sobek's existing ToValue path
	// would be cleaner — but for the few literal cases the tests pass
	// in, a fixed-precision render is sufficient.
	return formatFloat(f)
}

func formatFloat(f float64) string {
	// Use sobek itself as the source of truth: a fresh runtime would
	// be wasteful per-call here, so just hand-roll a small printer
	// good enough for our test inputs (all are small positive
	// decimals like 0.5, 0.95, 0.97).
	const precision = 6
	if f == 0 {
		return "0"
	}
	neg := f < 0
	if neg {
		f = -f
	}
	whole := int64(f)
	frac := f - float64(whole)
	out := sobekIntegerString(whole)
	if frac > 0 {
		out += "."
		for i := 0; i < precision; i++ {
			frac *= 10
			d := int(frac)
			out += string(rune('0' + d))
			frac -= float64(d)
		}
	}
	if neg {
		return "-" + out
	}
	return out
}

// _ = sobek import guard
var _ = sobek.Undefined
