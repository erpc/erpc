package wsupstream

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsZeroBlockRef(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"0", true},
		{"0x0", true},
		{"0X0", true},
		{"0x00", true},
		{"0x0000", true},
		{"  0x0 ", true},
		{"0x1", false},
		{"0x100", false},
		{"latest", false},
		{"finalized", false},
		{"", false},
		{"0x", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			assert.Equal(t, c.want, isZeroBlockRef(c.in))
		})
	}
}

func TestStripFromBlockZero(t *testing.T) {
	t.Run("strips fromBlock:0x0 and keeps toBlock", func(t *testing.T) {
		params := []interface{}{
			map[string]interface{}{
				"address":   "0xabc",
				"topics":    []string{"0xdef"},
				"fromBlock": "0x0",
				"toBlock":   "latest",
			},
		}
		out, changed := stripFromBlockZero(params)
		assert.True(t, changed)
		m := out[0].(map[string]interface{})
		_, hasFrom := m["fromBlock"]
		assert.False(t, hasFrom)
		assert.Equal(t, "latest", m["toBlock"])
		assert.Equal(t, "0xabc", m["address"])
	})

	t.Run("non-zero fromBlock passes through untouched", func(t *testing.T) {
		params := []interface{}{
			map[string]interface{}{"fromBlock": "0x100", "toBlock": "latest"},
		}
		out, changed := stripFromBlockZero(params)
		assert.False(t, changed)
		m := out[0].(map[string]interface{})
		assert.Equal(t, "0x100", m["fromBlock"])
	})

	t.Run("no fromBlock returns changed=false", func(t *testing.T) {
		params := []interface{}{
			map[string]interface{}{"address": "0xabc", "topics": []string{"0xdef"}},
		}
		out, changed := stripFromBlockZero(params)
		assert.False(t, changed)
		assert.Equal(t, params, out)
	})

	t.Run("non-map params are untouched", func(t *testing.T) {
		params := []interface{}{"logs", "notAMap", 42}
		out, changed := stripFromBlockZero(params)
		assert.False(t, changed)
		assert.Equal(t, params, out)
	})

	t.Run("input is not mutated", func(t *testing.T) {
		original := []interface{}{
			map[string]interface{}{"fromBlock": "0x0", "address": "0xabc"},
		}
		_, changed := stripFromBlockZero(original)
		assert.True(t, changed)
		// Original still has fromBlock.
		orig := original[0].(map[string]interface{})
		_, hasFrom := orig["fromBlock"]
		assert.True(t, hasFrom, "stripFromBlockZero must not mutate its input")
	})
}
