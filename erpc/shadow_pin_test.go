package erpc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPinBlockTagInBody(t *testing.T) {
	t.Run("PinsLatestForEthCall", func(t *testing.T) {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xabc","data":"0x1"},"latest"]}`)
		out, changed := pinBlockTagInBody(body, 1, 0x189cbe5)
		assert.True(t, changed)
		assert.Contains(t, string(out), `"0x189cbe5"`)
		assert.NotContains(t, string(out), `"latest"`)
	})

	t.Run("PinsStorageSlotIndexTwo", func(t *testing.T) {
		body := []byte(`{"jsonrpc":"2.0","id":7,"method":"eth_getStorageAt","params":["0xabc","0x0","latest"]}`)
		out, changed := pinBlockTagInBody(body, 2, 100)
		assert.True(t, changed)
		assert.Contains(t, string(out), `"0x64"`)
	})

	t.Run("ConcreteHeightUntouched", func(t *testing.T) {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xabc"},"0x1234"]}`)
		out, changed := pinBlockTagInBody(body, 1, 100)
		assert.False(t, changed)
		assert.Equal(t, body, out)
	})

	t.Run("PendingUntouched", func(t *testing.T) {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xabc"},"pending"]}`)
		_, changed := pinBlockTagInBody(body, 1, 100)
		assert.False(t, changed)
	})

	t.Run("MissingParamUntouched", func(t *testing.T) {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xabc"}]}`)
		_, changed := pinBlockTagInBody(body, 1, 100)
		assert.False(t, changed)
	})

	t.Run("GarbageUntouched", func(t *testing.T) {
		body := []byte(`{not json`)
		out, changed := pinBlockTagInBody(body, 1, 100)
		assert.False(t, changed)
		assert.Equal(t, []byte(`{not json`), out)
	})
}
