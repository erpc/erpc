package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFindStaticResponseMatch(t *testing.T) {
	entries := []*StaticResponseConfig{
		{
			Method: "eth_getBlockByNumber",
			Params: []interface{}{"0x0", false},
			Response: &StaticResponseBodyConfig{
				Result: map[string]interface{}{"number": "0x0"},
			},
		},
		{
			Method: "eth_chainId",
			Params: []interface{}{},
			Response: &StaticResponseBodyConfig{
				Result: "0x539",
			},
		},
	}

	t.Run("hit on exact params", func(t *testing.T) {
		m := FindStaticResponseMatch(entries, "eth_getBlockByNumber", []interface{}{"0x0", false})
		if assert.NotNil(t, m) {
			assert.Equal(t, "eth_getBlockByNumber", m.Method)
		}
	})

	t.Run("miss on different params", func(t *testing.T) {
		m := FindStaticResponseMatch(entries, "eth_getBlockByNumber", []interface{}{"0x1", false})
		assert.Nil(t, m)
	})

	t.Run("miss on different method", func(t *testing.T) {
		m := FindStaticResponseMatch(entries, "eth_getBlockByHash", []interface{}{"0x0", false})
		assert.Nil(t, m)
	})

	t.Run("hit on empty params", func(t *testing.T) {
		m := FindStaticResponseMatch(entries, "eth_chainId", []interface{}{})
		assert.NotNil(t, m)
	})

	t.Run("nil slice vs empty slice are equivalent", func(t *testing.T) {
		m := FindStaticResponseMatch(entries, "eth_chainId", nil)
		assert.NotNil(t, m)
	})

	t.Run("nil entries returns nil", func(t *testing.T) {
		assert.Nil(t, FindStaticResponseMatch(nil, "eth_chainId", []interface{}{}))
	})
}

func TestParamsEqual_NumericEquivalence(t *testing.T) {
	// YAML decodes ints as int; JSON decodes numbers as float64. Same logical
	// value from both sources must compare equal.
	assert.True(t, paramsEqual([]interface{}{1}, []interface{}{float64(1)}))
	assert.True(t, paramsEqual([]interface{}{int64(100)}, []interface{}{float64(100)}))
	assert.False(t, paramsEqual([]interface{}{1}, []interface{}{float64(1.5)}))
}

func TestParamsEqual_NestedObject(t *testing.T) {
	// eth_call-style object params with keys in different orders still match.
	a := []interface{}{
		map[string]interface{}{"to": "0xabc", "data": "0x01"},
		"latest",
	}
	b := []interface{}{
		map[string]interface{}{"data": "0x01", "to": "0xabc"},
		"latest",
	}
	assert.True(t, paramsEqual(a, b))
}

func TestParamsEqual_MismatchedSlice(t *testing.T) {
	assert.False(t, paramsEqual([]interface{}{"0x0"}, []interface{}{"0x0", false}))
}

func TestParamsEqual_HexStringCaseSensitive(t *testing.T) {
	// We do not normalize hex. "0x0" and "0x00" are distinct keys; callers
	// are responsible for matching the exact form their clients send.
	assert.False(t, paramsEqual([]interface{}{"0x0"}, []interface{}{"0x00"}))
}

func TestStaticResponseConfig_Validate(t *testing.T) {
	t.Run("valid result", func(t *testing.T) {
		s := &StaticResponseConfig{
			Method:   "eth_chainId",
			Response: &StaticResponseBodyConfig{Result: "0x1"},
		}
		assert.NoError(t, s.Validate())
	})

	t.Run("valid error", func(t *testing.T) {
		s := &StaticResponseConfig{
			Method: "some_method",
			Response: &StaticResponseBodyConfig{
				Error: &StaticResponseErrorConfig{Code: -32601, Message: "Method not found"},
			},
		}
		assert.NoError(t, s.Validate())
	})

	t.Run("missing method", func(t *testing.T) {
		s := &StaticResponseConfig{
			Response: &StaticResponseBodyConfig{Result: "0x1"},
		}
		assert.Error(t, s.Validate())
	})

	t.Run("missing response", func(t *testing.T) {
		s := &StaticResponseConfig{Method: "eth_chainId"}
		assert.Error(t, s.Validate())
	})

	t.Run("both result and error", func(t *testing.T) {
		s := &StaticResponseConfig{
			Method: "eth_chainId",
			Response: &StaticResponseBodyConfig{
				Result: "0x1",
				Error:  &StaticResponseErrorConfig{Code: -1, Message: "x"},
			},
		}
		assert.Error(t, s.Validate())
	})

	t.Run("neither result nor error", func(t *testing.T) {
		s := &StaticResponseConfig{
			Method:   "eth_chainId",
			Response: &StaticResponseBodyConfig{},
		}
		assert.Error(t, s.Validate())
	})

	t.Run("error missing message", func(t *testing.T) {
		s := &StaticResponseConfig{
			Method: "eth_chainId",
			Response: &StaticResponseBodyConfig{
				Error: &StaticResponseErrorConfig{Code: -32601},
			},
		}
		assert.Error(t, s.Validate())
	})
}
