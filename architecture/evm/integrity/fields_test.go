package integrity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// eqHex compares hex values that may differ in case AND in prefix case: a
// caller may send "0XABC…" in request params (the identity checks accept that
// shape) while the node answers "0xabc…". Rejecting that pair would be the
// module rejecting valid data — the one thing it must never do.
func TestEqHex(t *testing.T) {
	const (
		lower = "0x59d203e3c683df400be7440166d2939a887d54982fcede662861d3dfd7fe5910"
		upper = "0X59D203E3C683DF400BE7440166D2939A887D54982FCEDE662861D3DFD7FE5910"
		other = "0x7c9f61a71bf3541ff02f19af20dc3763158936770b7d9be1eb5e0bb3ecee913a"
	)

	t.Run("equal regardless of body case", func(t *testing.T) {
		assert.True(t, eqHex(lower, "0x59D203E3C683DF400BE7440166D2939A887D54982FCEDE662861D3DFD7FE5910"))
	})
	t.Run("equal regardless of PREFIX case", func(t *testing.T) {
		assert.True(t, eqHex(lower, upper))
		assert.True(t, eqHex(upper, lower))
	})
	t.Run("equal with prefix present on only one side", func(t *testing.T) {
		assert.True(t, eqHex(lower, lower[2:]))
	})
	t.Run("genuinely different values are not equal", func(t *testing.T) {
		assert.False(t, eqHex(lower, other))
		assert.False(t, eqHex(upper, other))
	})
	t.Run("an absent value never fails a check", func(t *testing.T) {
		assert.True(t, eqHex("", lower))
		assert.True(t, eqHex(lower, ""))
	})
}
