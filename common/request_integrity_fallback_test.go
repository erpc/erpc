package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrityFallbackStash(t *testing.T) {
	rq := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[]}`))

	assert.Nil(t, rq.TakeIntegrityFallbackResponse(), "empty stash yields nil")

	r1 := NewNormalizedResponse().WithRequest(rq)
	r2 := NewNormalizedResponse().WithRequest(rq)
	rq.SetIntegrityFallbackResponse(r1, "checkA", "unfinalized", "first")
	rq.SetIntegrityFallbackResponse(r2, "checkB", "unfinalized", "second")

	fb := rq.TakeIntegrityFallbackResponse()
	require.NotNil(t, fb)
	assert.Same(t, r2, fb.Response, "the NEWEST eligible original wins")
	assert.Equal(t, "checkB", fb.CheckID)
	assert.Equal(t, "second", fb.Reason)

	assert.Nil(t, rq.TakeIntegrityFallbackResponse(), "take clears the slot: a fallback serves at most once")

	rq.SetIntegrityFallbackResponse(nil, "checkC", "finalized", "nil response ignored")
	assert.Nil(t, rq.TakeIntegrityFallbackResponse(), "a nil response never stashes")
}
