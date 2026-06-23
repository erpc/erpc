package evm

import (
	"context"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChainView(t *testing.T) {
	t.Run("observe and look up the pin", func(t *testing.T) {
		c := newChainView(nil, 8)
		_, ok := c.HashAt(1)
		assert.False(t, ok)
		c.observe(1, "0xa", nil)
		v, ok := c.HashAt(1)
		require.True(t, ok)
		assert.Equal(t, "0xa", v)
	})

	t.Run("reorg adopts the new hash and rolls back descendants", func(t *testing.T) {
		c := newChainView(nil, 32)
		c.observe(5, "0xa", nil)
		c.observe(6, "0xb", nil)
		c.observe(7, "0xc", nil)
		// A different hash for 5 is a reorg: 6 and 7 were built on the old fork, so
		// their pins are invalidated (they re-populate as the new fork extends).
		c.observe(5, "0xa2", nil)
		v, ok := c.HashAt(5)
		require.True(t, ok)
		assert.Equal(t, "0xa2", v)
		_, ok = c.HashAt(6)
		assert.False(t, ok, "descendant 6 should be rolled back")
		_, ok = c.HashAt(7)
		assert.False(t, ok, "descendant 7 should be rolled back")
	})

	t.Run("re-observing the same hash is a no-op (no rollback)", func(t *testing.T) {
		c := newChainView(nil, 32)
		c.observe(5, "0xa", nil)
		c.observe(6, "0xb", nil)
		c.observe(5, "0xa", nil) // same hash — must NOT roll back 6
		v, ok := c.HashAt(6)
		require.True(t, ok)
		assert.Equal(t, "0xb", v)
	})

	t.Run("ignores empty hash and negative number", func(t *testing.T) {
		c := newChainView(nil, 8)
		c.observe(3, "", nil)
		c.observe(-1, "0xa", nil)
		_, ok := c.HashAt(3)
		assert.False(t, ok)
	})

	t.Run("evicts below tip minus window", func(t *testing.T) {
		c := newChainView(nil, 2)
		for i := int64(1); i <= 5; i++ {
			c.observe(i, fmt.Sprintf("0x%d", i), nil)
		}
		// tip=5, window=2 → keep numbers >= 3.
		for _, gone := range []int64{1, 2} {
			_, ok := c.HashAt(gone)
			assert.False(t, ok, "block %d should be evicted", gone)
		}
		v, ok := c.HashAt(5)
		require.True(t, ok)
		assert.Equal(t, "0x5", v)
	})

	t.Run("zero window falls back to default", func(t *testing.T) {
		c := newChainView(nil, 0)
		assert.Equal(t, defaultReorgWindow, c.window)
	})
}

func TestObserveBlockView(t *testing.T) {
	c := newChainView(nil, 8)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(`{"number":"0x10","hash":"0xabc","parentHash":"0xdef"}`), nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	observeBlockView(context.Background(), c, rs)
	v, ok := c.HashAt(0x10)
	require.True(t, ok)
	assert.Equal(t, "0xabc", v)
}
