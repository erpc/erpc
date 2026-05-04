package common

import (
	"reflect"
)

// FindStaticResponseMatch returns the first StaticResponseConfig whose method
// and params match the given request, or nil if none match.
//
// Match semantics:
//   - method: exact string equality
//   - params: recursive deep equality treating numeric types equivalently and
//     comparing maps by key regardless of declared order
func FindStaticResponseMatch(entries []*StaticResponseConfig, method string, params []interface{}) *StaticResponseConfig {
	for _, e := range entries {
		if e == nil || e.Method != method {
			continue
		}
		if paramsEqual(e.Params, params) {
			return e
		}
	}
	return nil
}

// paramsEqual compares two param slices from different deserialization paths
// (YAML config vs JSON request body). It tolerates numeric-type divergence
// (int vs float64) and compares maps by key regardless of iteration order.
func paramsEqual(a, b []interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !valueEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func valueEqual(a, b interface{}) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	// Numbers: JSON decodes integers as float64 while YAML often decodes as int.
	// Compare as float64 when both are numeric, regardless of concrete type.
	if fa, ok := toFloat64(a); ok {
		if fb, ok := toFloat64(b); ok {
			return fa == fb
		}
		return false
	}

	switch va := a.(type) {
	case string:
		vb, ok := b.(string)
		return ok && va == vb
	case bool:
		vb, ok := b.(bool)
		return ok && va == vb
	case []interface{}:
		vb, ok := b.([]interface{})
		return ok && sliceEqual(va, vb)
	case map[string]interface{}:
		vb, ok := b.(map[string]interface{})
		return ok && mapEqual(va, vb)
	}

	return reflect.DeepEqual(a, b)
}

func sliceEqual(a, b []interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !valueEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func mapEqual(a, b map[string]interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if !valueEqual(va, vb) {
			return false
		}
	}
	return true
}

func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}
