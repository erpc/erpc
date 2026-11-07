package evm

import (
	"context"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestTxHashParam(t *testing.T) {
	const hash = "0xe39263b2b8ccf8f18f344e860bb8eb62c68f6ee0de8c877131bc469e31af837e"
	ctx := context.Background()
	req := func(params string) *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":` + params + `}`))
	}
	assert.Equal(t, hash, txHashParam(ctx, req(`["`+hash+`"]`)))
	assert.Equal(t, "", txHashParam(ctx, req(`[]`)))
	assert.Equal(t, "", txHashParam(ctx, req(`["0x1234"]`)))
	assert.Equal(t, "", txHashParam(ctx, req(`[42]`)))
	assert.Equal(t, "", txHashParam(ctx, req(`["`+hash[:65]+`g"]`)))
	assert.Equal(t, "", txHashParam(ctx, req(`["\"],\"x\":[\"`+strings.Repeat("a", 54)+`"]`)), "not a hash")
	assert.Equal(t, "", txHashParam(ctx, nil))
}
