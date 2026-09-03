package common

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lvrResp(t *testing.T, result string) *NormalizedResponse {
	t.Helper()
	jrr := MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(result), nil)
	return NewNormalizedResponse().WithJsonRpcResponse(jrr)
}

// The LVR reject race: attempts store their response as LVR BEFORE post-forward
// validation, so a rejected body must (a) never re-enter the slot and (b) its
// clear must not drop a concurrent VALID response from another hedged attempt.
func TestLastValidResponse_IntegrityRejected(t *testing.T) {
	ctx := context.Background()

	t.Run("marked response refused by SetLastValidResponse", func(t *testing.T) {
		rq := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
		bad := lvrResp(t, `{"hash":"0xbad"}`)
		bad.MarkIntegrityRejected()
		assert.False(t, rq.SetLastValidResponse(ctx, bad))
		assert.Nil(t, rq.LastValidResponse())
	})

	t.Run("identity-checked clear removes the rejected response", func(t *testing.T) {
		rq := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
		bad := lvrResp(t, `{"hash":"0xbad"}`)
		require.True(t, rq.SetLastValidResponse(ctx, bad))
		bad.MarkIntegrityRejected()
		rq.ClearLastValidResponseIf(bad)
		assert.Nil(t, rq.LastValidResponse())
	})

	t.Run("identity-checked clear does NOT drop a concurrent valid response", func(t *testing.T) {
		rq := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
		bad := lvrResp(t, `{"hash":"0xbad"}`)
		require.True(t, rq.SetLastValidResponse(ctx, bad))
		good := lvrResp(t, `{"hash":"0xgood"}`)
		require.True(t, rq.SetLastValidResponse(ctx, good), "another attempt stores a valid response")
		bad.MarkIntegrityRejected()
		rq.ClearLastValidResponseIf(bad) // rejects only ITS OWN response
		assert.Equal(t, good, rq.LastValidResponse(), "the valid response must survive")
	})
}
