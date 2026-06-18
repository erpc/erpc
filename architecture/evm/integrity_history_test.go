package evm

import (
	"context"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlockHistory(t *testing.T) {
	t.Run("observe and look up", func(t *testing.T) {
		h := newBlockHistory(8)
		_, ok := h.HashAt(1)
		assert.False(t, ok)
		h.Observe(1, "0xa")
		v, ok := h.HashAt(1)
		require.True(t, ok)
		assert.Equal(t, "0xa", v)
	})

	t.Run("last write wins (reorg replaces hash)", func(t *testing.T) {
		h := newBlockHistory(8)
		h.Observe(5, "0xold")
		h.Observe(5, "0xnew")
		v, _ := h.HashAt(5)
		assert.Equal(t, "0xnew", v)
	})

	t.Run("ignores empty hash and negative number", func(t *testing.T) {
		h := newBlockHistory(8)
		h.Observe(3, "")
		h.Observe(-1, "0xa")
		_, ok := h.HashAt(3)
		assert.False(t, ok)
	})

	t.Run("evicts oldest beyond capacity", func(t *testing.T) {
		h := newBlockHistory(3)
		for i := int64(1); i <= 5; i++ {
			h.Observe(i, fmt.Sprintf("0x%d", i))
		}
		for _, gone := range []int64{1, 2} {
			_, ok := h.HashAt(gone)
			assert.False(t, ok, "block %d should be evicted", gone)
		}
		v, ok := h.HashAt(5)
		require.True(t, ok)
		assert.Equal(t, "0x5", v)
	})
}

func TestObserveBlock(t *testing.T) {
	h := newBlockHistory(8)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(`{"number":"0x10","hash":"0xabc","parentHash":"0xdef"}`), nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	observeBlock(context.Background(), h, rs)
	v, ok := h.HashAt(0x10)
	require.True(t, ok)
	assert.Equal(t, "0xabc", v)
}
